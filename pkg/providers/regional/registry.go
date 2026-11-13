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

// Package regional composes the existing single-region providers. Azure resources
// retain their existing names and node resource group, so discovery and garbage
// collection do not depend on a NodeClass or an in-memory region registry surviving.
package regional

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"sort"
	"sync"
	"time"

	"github.com/Azure/karpenter-provider-azure/pkg/apis/v1beta1"
	"github.com/Azure/karpenter-provider-azure/pkg/operator/options"
	"github.com/Azure/karpenter-provider-azure/pkg/providers/imagefamily"
	"github.com/Azure/karpenter-provider-azure/pkg/providers/instance"
	"github.com/Azure/karpenter-provider-azure/pkg/providers/instancetype"
	"github.com/Azure/karpenter-provider-azure/pkg/providers/quota"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"
)

type Providers struct {
	Types     instancetype.Provider
	Images    imagefamily.NodeImageProvider
	Instances instance.VMProvider
	Quota     quota.Provider
}

type entry struct {
	*Providers
	mu           sync.Mutex
	skusUpdated  time.Time
	quotaUpdated time.Time
	lastAttempt  time.Time
	lastError    error
}

type Registry struct {
	home     string
	factory  func(string) *Providers
	validate func(context.Context, v1beta1.Region) error
	mu       sync.Mutex
	entries  map[string]*entry
}

func New(home string, providers *Providers, factory func(string) *Providers, validate func(context.Context, v1beta1.Region) error) *Registry {
	return &Registry{
		home: home, factory: factory, validate: validate,
		entries: map[string]*entry{home: {Providers: providers, skusUpdated: time.Now()}},
	}
}

func (r *Registry) provider(region string) *entry {
	r.mu.Lock()
	defer r.mu.Unlock()
	if p, ok := r.entries[region]; ok {
		return p
	}
	p := &entry{Providers: r.factory(region)}
	r.entries[region] = p
	return p
}

// Only opted-in calls refresh regional state. A failed region is retried at most
// once per minute and never borrows another region's quota or SKU catalog.
func (r *Registry) get(ctx context.Context, region string) (*entry, error) {
	p := r.provider(region)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.lastError != nil && time.Since(p.lastAttempt) < time.Minute {
		return nil, p.lastError
	}
	p.lastAttempt = time.Now()
	if time.Since(p.skusUpdated) >= 12*time.Hour {
		if err := p.Types.UpdateInstanceTypes(ctx); err != nil {
			p.lastError = fmt.Errorf("region %s SKUs: %w", region, err)
			return nil, p.lastError
		}
		p.skusUpdated = time.Now()
	}
	if time.Since(p.quotaUpdated) >= 10*time.Minute {
		if err := p.Quota.Update(ctx); err != nil {
			p.lastError = fmt.Errorf("region %s quota: %w", region, err)
			return nil, p.lastError
		}
		p.quotaUpdated = time.Now()
	}
	p.lastError = nil
	return p, nil
}

func (r *Registry) ValidateSubnets(ctx context.Context, nodeClass *v1beta1.AKSNodeClass) error {
	if err := options.FromContext(ctx).ValidateRegions(nodeClass); err != nil {
		return err
	}
	for _, region := range nodeClass.Spec.Regions {
		if err := r.validate(ctx, region); err != nil {
			return fmt.Errorf("region %s subnet: %w", region.Name, err)
		}
	}
	return nil
}

type InstanceTypes struct {
	instancetype.Provider
	Registry *Registry
}

var regionalKeys = []string{
	corev1.LabelTopologyRegion, corev1.LabelTopologyZone, karpv1.CapacityTypeLabelKey,
	v1beta1.LabelPlacementScope, v1beta1.LabelUltraSSD,
}

func (p *InstanceTypes) List(ctx context.Context, nodeClass *v1beta1.AKSNodeClass) ([]*cloudprovider.InstanceType, error) {
	if len(nodeClass.Spec.Regions) == 0 {
		return p.Provider.List(ctx, nodeClass)
	}
	if err := options.FromContext(ctx).ValidateRegions(nodeClass); err != nil {
		return nil, err
	}
	byName := map[string]*cloudprovider.InstanceType{}
	var errs []error
	for _, region := range nodeClass.Spec.Regions {
		types, err := p.listRegion(ctx, nodeClass, region.Name)
		if err != nil {
			log.FromContext(ctx).Error(err, "excluding unavailable region", "region", region.Name)
			errs = append(errs, err)
			continue
		}
		if err := addRegion(byName, types, region.Name); err != nil {
			return nil, err
		}
	}
	if len(byName) == 0 && len(errs) != 0 {
		return nil, errors.Join(errs...)
	}
	result := make([]*cloudprovider.InstanceType, 0, len(byName))
	for _, it := range byName {
		result = append(result, it)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result, nil
}

func (p *InstanceTypes) listRegion(ctx context.Context, nodeClass *v1beta1.AKSNodeClass, region string) ([]*cloudprovider.InstanceType, error) {
	provider, err := p.Registry.get(ctx, region)
	if err != nil {
		return nil, err
	}
	return provider.Types.List(ctx, nodeClass)
}

func addRegion(byName map[string]*cloudprovider.InstanceType, types []*cloudprovider.InstanceType, region string) error {
	for _, it := range types {
		scoped := it.DeepCopy()
		for _, offering := range scoped.Offerings {
			offering.Requirements.Add(scheduling.NewRequirement(corev1.LabelTopologyRegion, corev1.NodeSelectorOpIn, region))
		}
		if existing, ok := byName[it.Name]; ok {
			if err := merge(existing, scoped); err != nil {
				return err
			}
		} else {
			byName[it.Name] = scoped
		}
	}
	return nil
}

func merge(existing, other *cloudprovider.InstanceType) error {
	if !reflect.DeepEqual(existing.Capacity, other.Capacity) || !reflect.DeepEqual(existing.Overhead, other.Overhead) {
		return fmt.Errorf("SKU %s has inconsistent resources across regions", existing.Name)
	}
	left, right := maps.Clone(existing.Requirements), maps.Clone(other.Requirements)
	for _, key := range regionalKeys {
		delete(left, key)
		delete(right, key)
	}
	if !sameRequirements(left, right) {
		return fmt.Errorf("SKU %s has inconsistent capabilities across regions", existing.Name)
	}
	for _, key := range regionalKeys {
		existing.Requirements.Get(key).Insert(other.Requirements.Get(key).Values()...)
	}
	existing.Offerings = append(existing.Offerings, other.Offerings...)
	return nil
}

func sameRequirements(left, right scheduling.Requirements) bool {
	if len(left) != len(right) {
		return false
	}
	for key, requirement := range left {
		if !right.Has(key) {
			return false
		}
		a, b := requirement.NodeSelectorRequirement(), right.Get(key).NodeSelectorRequirement()
		sort.Strings(a.Values)
		sort.Strings(b.Values)
		if !reflect.DeepEqual(a, b) {
			return false
		}
	}
	return true
}

type Images struct {
	imagefamily.NodeImageProvider
	Registry *Registry
}

func (p *Images) List(ctx context.Context, nodeClass *v1beta1.AKSNodeClass) ([]imagefamily.NodeImage, error) {
	if len(nodeClass.Spec.Regions) == 0 {
		return p.NodeImageProvider.List(ctx, nodeClass)
	}
	if err := options.FromContext(ctx).ValidateRegions(nodeClass); err != nil {
		return nil, err
	}
	var result []imagefamily.NodeImage
	for _, region := range nodeClass.Spec.Regions {
		images, err := p.Registry.provider(region.Name).Images.List(ctx, nodeClass)
		if err != nil {
			// Preserve the previously resolved image set on transient regional failures.
			return nil, fmt.Errorf("region %s images: %w", region.Name, err)
		}
		for _, image := range images {
			image.Requirements = scheduling.NewNodeSelectorRequirementsWithMinValues(image.Requirements.NodeSelectorRequirements()...)
			image.Requirements.Add(scheduling.NewRequirement(corev1.LabelTopologyRegion, corev1.NodeSelectorOpIn, region.Name))
			result = append(result, image)
		}
	}
	return result, nil
}
