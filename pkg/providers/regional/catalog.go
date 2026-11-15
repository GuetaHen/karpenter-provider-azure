/*
Portions Copyright (c) Microsoft Corporation.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package regional

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"sync"

	"github.com/Azure/karpenter-provider-azure/pkg/apis/v1beta1"
	"github.com/Azure/karpenter-provider-azure/pkg/metrics"
	"github.com/Azure/karpenter-provider-azure/pkg/operator/options"
	"github.com/Azure/karpenter-provider-azure/pkg/providers/instancetype"
	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/log"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"
	"sigs.k8s.io/karpenter/pkg/utils/pretty"
)

// regionalKeys vary by region; they are unioned when a SKU is offered in several regions.
var regionalKeys = []string{
	corev1.LabelTopologyRegion, corev1.LabelTopologyZone, karpv1.CapacityTypeLabelKey,
	v1beta1.LabelPlacementScope, v1beta1.LabelUltraSSD,
}

// maxCachedCatalogs bounds the merged-catalog cache if NodeClasses churn.
const maxCachedCatalogs = 128

// InconsistentOfferingsMetric counts catalog builds that excluded a region's offerings
// for a SKU because its SKU data disagreed with the first listed region offering it.
//
// STABILITY: ALPHA - This metric may change or be removed without notice.
var InconsistentOfferingsMetric = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: metrics.Namespace,
		Subsystem: "regional",
		Name:      "inconsistent_instance_type_offerings_total",
		Help:      "Number of times a region's offerings for an instance type were excluded from a multi-region catalog because its SKU data disagreed with the first listed region that offers it.",
	},
	[]string{"region", "instance_type", "difference"},
)

func init() {
	crmetrics.Registry.MustRegister(InconsistentOfferingsMetric)
}

type InstanceTypes struct {
	instancetype.Provider
	Registry *Registry
}

// List merges the instance types of every listed region into one catalog. A region that
// failed the NodeClass's last check offers no capacity: its instance types stay with
// unavailable offerings, so that core drift still finds the instance types of its
// NodeClaims, or, if it has never loaded, ValidateSubnets found no NodeClaims in it and it
// is left out. Any other region that has never loaded fails the NodeClass: a partial catalog
// would make core drift treat healthy NodeClaims in that region as having an unknown
// instance type.
func (p *InstanceTypes) List(ctx context.Context, nodeClass *v1beta1.AKSNodeClass) ([]*cloudprovider.InstanceType, error) {
	if len(nodeClass.Spec.Regions) == 0 {
		return p.Provider.List(ctx, nodeClass)
	}
	if err := options.FromContext(ctx).ValidateRegions(nodeClass); err != nil {
		return nil, err
	}
	inputs := make([]regionTypes, 0, len(nodeClass.Spec.Regions))
	for _, region := range nodeClass.Spec.Regions {
		unavailable := p.Registry.isUnavailable(nodeClass.UID, region.Name)
		provider, err := p.Registry.get(ctx, region.Name)
		if err != nil {
			if unavailable {
				continue
			}
			return nil, err
		}
		listed, err := provider.Types.List(ctx, nodeClass)
		if err != nil {
			return nil, fmt.Errorf("region %s instance types: %w", region.Name, err)
		}
		if unavailable {
			listed = p.Registry.catalogs.withoutCapacity(nodeClass.UID, region.Name, listed)
		}
		inputs = append(inputs, regionTypes{region: region.Name, types: listed})
	}
	return p.Registry.catalogs.get(ctx, nodeClass.UID, inputs), nil
}

type regionTypes struct {
	region string
	types  []*cloudprovider.InstanceType
}

type catalog struct {
	inputs []regionTypes
	merged []*cloudprovider.InstanceType
}

// catalogCache reuses a merged catalog while every region returns the same instance
// types. Providers return pointer-stable results until their SKU, price, quota or
// availability inputs change, so pointer identity is an exact and cheap cache key.
type catalogCache struct {
	mu       sync.Mutex
	catalogs map[types.UID]*catalog
	changes  *pretty.ChangeMonitor

	// unavailable reuses the capacity-free copies of a failing region's instance types.
	unavailable map[string]unavailableTypes
}

type unavailableTypes struct {
	listed, copies []*cloudprovider.InstanceType
}

func (c *catalogCache) get(ctx context.Context, uid types.UID, inputs []regionTypes) []*cloudprovider.InstanceType {
	c.mu.Lock()
	cached, ok := c.catalogs[uid]
	c.mu.Unlock()
	if !ok || !slices.EqualFunc(cached.inputs, inputs, sameRegionTypes) {
		cached = &catalog{inputs: inputs, merged: c.merge(ctx, inputs)}
		c.mu.Lock()
		if c.catalogs == nil || len(c.catalogs) >= maxCachedCatalogs {
			c.catalogs = map[types.UID]*catalog{}
		}
		c.catalogs[uid] = cached
		c.mu.Unlock()
	}
	// Callers may reorder or filter the slice; the instance types are shared read-only,
	// exactly like the single-region provider's cached results.
	return slices.Clone(cached.merged)
}

func sameRegionTypes(a, b regionTypes) bool {
	return a.region == b.region && slices.Equal(a.types, b.types)
}

// withoutCapacity returns copies of a failing region's instance types whose offerings are
// unavailable: core drift still finds the instance types of the region's NodeClaims, and no new
// node is placed there. The copies are reused while the region lists the same instance types,
// so that the merged catalog stays cached.
func (c *catalogCache) withoutCapacity(uid types.UID, region string, listed []*cloudprovider.InstanceType) []*cloudprovider.InstanceType {
	key := string(uid) + "/" + region
	c.mu.Lock()
	defer c.mu.Unlock()
	if cached, ok := c.unavailable[key]; ok && slices.Equal(cached.listed, listed) {
		return cached.copies
	}
	copies := make([]*cloudprovider.InstanceType, 0, len(listed))
	for _, it := range listed {
		copied := it.DeepCopy()
		for _, offering := range copied.Offerings {
			offering.Available = false
		}
		copies = append(copies, copied)
	}
	if c.unavailable == nil || len(c.unavailable) >= maxCachedCatalogs {
		c.unavailable = map[string]unavailableTypes{}
	}
	c.unavailable[key] = unavailableTypes{listed: listed, copies: copies}
	return copies
}

// merge combines each SKU's offerings across regions. The first listed region that
// offers a SKU defines its resources and capabilities; a later region whose data
// disagrees contributes no offerings for that SKU instead of failing the catalog.
func (c *catalogCache) merge(ctx context.Context, inputs []regionTypes) []*cloudprovider.InstanceType {
	byName := map[string]*cloudprovider.InstanceType{}
	origin := map[string]string{}
	for _, input := range inputs {
		for _, it := range input.types {
			existing, ok := byName[it.Name]
			if !ok {
				byName[it.Name], origin[it.Name] = scope(it, input.region), input.region
				continue
			}
			if difference := inconsistency(existing, it); difference != "" {
				c.excluded(ctx, it.Name, input.region, origin[it.Name], difference)
				continue
			}
			absorb(existing, scope(it, input.region))
		}
	}
	result := make([]*cloudprovider.InstanceType, 0, len(byName))
	for _, it := range byName {
		result = append(result, it)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result
}

func (c *catalogCache) excluded(ctx context.Context, name, region, reference, difference string) {
	InconsistentOfferingsMetric.WithLabelValues(region, name, difference).Inc()
	if c.changes.HasChanged(region+"/"+name, reference+"/"+difference) {
		log.FromContext(ctx).Info("excluding regional offerings for an instance type whose SKU data differs from the reference region",
			"instanceType", name, "region", region, "referenceRegion", reference, "difference", difference)
	}
}

func scope(it *cloudprovider.InstanceType, region string) *cloudprovider.InstanceType {
	scoped := it.DeepCopy()
	for _, offering := range scoped.Offerings {
		offering.Requirements.Add(scheduling.NewRequirement(corev1.LabelTopologyRegion, corev1.NodeSelectorOpIn, region))
	}
	return scoped
}

func absorb(existing, other *cloudprovider.InstanceType) {
	for _, key := range regionalKeys {
		existing.Requirements.Get(key).Insert(other.Requirements.Get(key).Values()...)
	}
	existing.Offerings = append(existing.Offerings, other.Offerings...)
}

// inconsistency names the first difference between two regions' data for one SKU,
// ignoring requirement keys that legitimately vary by region. Quantities compare
// semantically, so "2" and "2000m" CPUs are equal.
func inconsistency(reference, other *cloudprovider.InstanceType) string {
	switch {
	case !equality.Semantic.DeepEqual(reference.Capacity, other.Capacity):
		return "capacity"
	case !equality.Semantic.DeepEqual(reference.Overhead, other.Overhead):
		return "overhead"
	case !sameCapabilities(reference.Requirements, other.Requirements):
		return "requirements"
	}
	return ""
}

func sameCapabilities(left, right scheduling.Requirements) bool {
	if nonRegional(left) != nonRegional(right) {
		return false
	}
	for key, requirement := range left {
		if slices.Contains(regionalKeys, key) {
			continue
		}
		if !right.Has(key) {
			return false
		}
		a, b := requirement.NodeSelectorRequirement(), right.Get(key).NodeSelectorRequirement()
		sort.Strings(a.Values)
		sort.Strings(b.Values)
		if !equality.Semantic.DeepEqual(a, b) {
			return false
		}
	}
	return true
}

func nonRegional(requirements scheduling.Requirements) int {
	count := 0
	for key := range requirements {
		if !slices.Contains(regionalKeys, key) {
			count++
		}
	}
	return count
}
