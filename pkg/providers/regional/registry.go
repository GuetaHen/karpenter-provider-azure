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
	"strings"
	"sync"
	"time"

	"github.com/Azure/karpenter-provider-azure/pkg/apis/v1beta1"
	"github.com/Azure/karpenter-provider-azure/pkg/operator/options"
	"github.com/Azure/karpenter-provider-azure/pkg/providers/imagefamily"
	"github.com/Azure/karpenter-provider-azure/pkg/providers/instance"
	"github.com/Azure/karpenter-provider-azure/pkg/providers/instancetype"
	"github.com/Azure/karpenter-provider-azure/pkg/providers/quota"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/scheduling"
	"sigs.k8s.io/karpenter/pkg/utils/pretty"
)

const (
	// skuRefreshInterval matches the home-region instance type refresh.
	skuRefreshInterval = 12 * time.Hour
	// quotaRefreshInterval and quotaMaxStaleness match the home-region quota controller:
	// stale quota is served until it is too old, then cleared so that it fails open.
	quotaRefreshInterval = 10 * time.Minute
	quotaMaxStaleness    = 15 * time.Minute
	// retryBackoff bounds Azure calls for a region whose refresh keeps failing.
	retryBackoff = time.Minute
)

// RegionNotLoadedReason is the SubnetsReady reason while a region has never loaded its SKU catalog.
const RegionNotLoadedReason = "RegionalCatalogUnavailable"

// regionNotLoadedError names a region that has never loaded its SKU catalog. Its reason lets
// the subnet status reconciler report it without importing this package.
type regionNotLoadedError struct {
	region string
	err    error
}

func (e *regionNotLoadedError) Error() string {
	return fmt.Sprintf("region %s has no SKU catalog yet: %v", e.region, e.err)
}

func (e *regionNotLoadedError) Unwrap() error {
	return e.err
}

// Reason is the status condition reason for this error.
func (e *regionNotLoadedError) Reason() string {
	return RegionNotLoadedReason
}

// RegionsUnavailableReason is the SubnetsReady reason while some regions fail their checks and
// get no new nodes, and the other regions serve.
const RegionsUnavailableReason = "RegionsUnavailable"

// regionsUnavailableError names the regions that get no new nodes while the others serve.
type regionsUnavailableError struct {
	regions []string
	errs    []error
}

func (e *regionsUnavailableError) Error() string {
	return "regions without new nodes until their checks pass: " +
		strings.Join(lo.Map(e.errs, func(err error, _ int) string { return err.Error() }), "; ")
}

func (e *regionsUnavailableError) Unwrap() []error {
	return e.errs
}

// Reason is the status condition reason for this error.
func (e *regionsUnavailableError) Reason() string {
	return RegionsUnavailableReason
}

// UnavailableRegions lists the regions that get no new nodes.
func (e *regionsUnavailableError) UnavailableRegions() []string {
	return e.regions
}

type Providers struct {
	Types     instancetype.Provider
	Images    imagefamily.NodeImageProvider
	Instances instance.VMProvider
	Quota     quota.Provider
}

// entry tracks one region. A region is hydrated once its SKU catalog has loaded; after
// that a failed refresh keeps serving the last successful data, as the home region does.
type entry struct {
	*Providers
	// external entries are refreshed by the home-region controllers, never here.
	external bool

	mu             sync.Mutex
	hydrated       bool
	skusErr        error
	skusUpdated    time.Time
	skusAttempted  time.Time
	quotaUpdated   time.Time
	quotaAttempted time.Time
}

type Registry struct {
	home     string
	factory  func(string) *Providers
	validate func(context.Context, v1beta1.Region) error
	clock    clock.Clock
	catalogs catalogCache

	// kubeClient lists NodeClaims to find the regions in use. Without it, every listed region counts as in use.
	kubeClient client.Reader

	mu      sync.Mutex
	entries map[string]*entry

	// unavailable holds, per NodeClass, the regions that failed their last check. It is guarded by mu.
	unavailable map[types.UID]sets.Set[string]
}

func New(home string, providers *Providers, factory func(string) *Providers, validate func(context.Context, v1beta1.Region) error) *Registry {
	return &Registry{
		home: home, factory: factory, validate: validate, clock: clock.RealClock{},
		catalogs: catalogCache{changes: pretty.NewChangeMonitor()},
		entries:  map[string]*entry{home: {Providers: providers, external: true, hydrated: true}},
	}
}

// WithClock sets the clock that schedules regional refreshes.
func (r *Registry) WithClock(clk clock.Clock) *Registry {
	r.clock = clk
	return r
}

// WithKubeClient lets ValidateSubnets find the regions that have NodeClaims.
func (r *Registry) WithKubeClient(kubeClient client.Reader) *Registry {
	r.kubeClient = kubeClient
	return r
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

// get returns a region's providers and refreshes them when due. Only opted-in calls
// refresh regional state, and a failing refresh is retried at most once per backoff.
// An error is returned only while a region has never loaded its SKU catalog.
func (r *Registry) get(ctx context.Context, region string) (*entry, error) {
	p := r.provider(region)
	if p.external {
		return p, nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := r.clock.Now()
	p.refreshSKUs(ctx, region, now)
	if !p.hydrated {
		return nil, &regionNotLoadedError{region: region, err: p.skusErr}
	}
	p.refreshQuota(ctx, region, now)
	return p, nil
}

func (p *entry) refreshSKUs(ctx context.Context, region string, now time.Time) {
	due := !p.hydrated || now.Sub(p.skusUpdated) >= skuRefreshInterval
	if !due || now.Sub(p.skusAttempted) < retryBackoff {
		return
	}
	p.skusAttempted = now
	if err := p.Types.UpdateInstanceTypes(ctx); err != nil {
		p.skusErr = err
		if p.hydrated {
			log.FromContext(ctx).Error(err, "failed to refresh regional SKUs, serving the last successful catalog",
				"region", region, "age", now.Sub(p.skusUpdated).String())
		}
		return
	}
	p.hydrated, p.skusErr, p.skusUpdated = true, nil, now
}

func (p *entry) refreshQuota(ctx context.Context, region string, now time.Time) {
	if now.Sub(p.quotaUpdated) < quotaRefreshInterval || now.Sub(p.quotaAttempted) < retryBackoff {
		return
	}
	p.quotaAttempted = now
	if err := p.Quota.Update(ctx); err != nil {
		// Quota that never loaded already fails open. Loaded quota is served until it is
		// older than the home-region limit, then cleared so that it fails open too.
		expired := !p.quotaUpdated.IsZero() && now.Sub(p.quotaUpdated) >= quotaMaxStaleness
		log.FromContext(ctx).Error(err, "failed to refresh regional quota", "region", region,
			"failingOpen", expired || p.quotaUpdated.IsZero())
		if expired {
			p.Quota.Reset()
		}
		return
	}
	p.quotaUpdated = now
}

// ValidateSubnets checks each listed region: its subnet, its SKU catalog and, once the status
// lists node images, an image for it. A failing region does not block the others: it gets no
// new nodes while its instance types stay in the catalog for core drift, and the returned error
// has reason RegionsUnavailable. The NodeClass stays not ready only while every region fails, or
// while a region that has never loaded its SKU catalog has NodeClaims, whose instance types core
// drift would otherwise not find. The subnet reconciler repeats the check.
func (r *Registry) ValidateSubnets(ctx context.Context, nodeClass *v1beta1.AKSNodeClass) error {
	if err := options.FromContext(ctx).ValidateRegions(nodeClass); err != nil {
		return err
	}
	var errs, blocking []error
	var inUse sets.Set[string]
	unavailable := sets.New[string]()
	for _, region := range nodeClass.Spec.Regions {
		err := r.check(ctx, nodeClass, region)
		if err == nil {
			continue
		}
		errs = append(errs, err)
		var notLoaded *regionNotLoadedError
		if errors.As(err, &notLoaded) {
			if inUse == nil {
				inUse = r.regionsInUse(ctx, nodeClass)
			}
			if inUse.Has(region.Name) {
				blocking = append(blocking, err)
				continue
			}
		}
		unavailable.Insert(region.Name)
	}
	r.setUnavailable(nodeClass.UID, unavailable)
	switch {
	case len(blocking) > 0:
		return blocking[0]
	case len(errs) == 0:
		return nil
	case len(errs) == len(nodeClass.Spec.Regions):
		return errs[0]
	}
	return &regionsUnavailableError{regions: sets.List(unavailable), errs: errs}
}

// check returns why a region cannot get new nodes, or nil.
func (r *Registry) check(ctx context.Context, nodeClass *v1beta1.AKSNodeClass, region v1beta1.Region) error {
	if err := r.validate(ctx, region); err != nil {
		return fmt.Errorf("region %s subnet: %w", region.Name, err)
	}
	if _, err := r.get(ctx, region.Name); err != nil {
		return err
	}
	if len(nodeClass.Status.Images) > 0 && !hasImageFor(nodeClass, region.Name) {
		return fmt.Errorf("region %s has no node images", region.Name)
	}
	return nil
}

func hasImageFor(nodeClass *v1beta1.AKSNodeClass, region string) bool {
	return lo.ContainsBy(nodeClass.Status.Images, func(image v1beta1.NodeImage) bool {
		return ImageMatchesRegion(image.Requirements, region)
	})
}

// regionsInUse returns the regions of the NodeClaims that reference a NodeClass by name, in any
// API group. Without a client, or when listing fails, every listed region counts as in use, which
// keeps a failing region failing closed.
func (r *Registry) regionsInUse(ctx context.Context, nodeClass *v1beta1.AKSNodeClass) sets.Set[string] {
	listed := sets.New(lo.Map(nodeClass.Spec.Regions, func(region v1beta1.Region, _ int) string { return region.Name })...)
	if r.kubeClient == nil {
		return listed
	}
	nodeClaims := &karpv1.NodeClaimList{}
	if err := r.kubeClient.List(ctx, nodeClaims); err != nil {
		log.FromContext(ctx).Error(err, "failed to list NodeClaims, treating every region as in use")
		return listed
	}
	inUse := sets.New[string]()
	for i := range nodeClaims.Items {
		nodeClaim := &nodeClaims.Items[i]
		if nodeClaim.Spec.NodeClassRef == nil || nodeClaim.Spec.NodeClassRef.Name != nodeClass.Name {
			continue
		}
		inUse.Insert(nodeClaim.Labels[corev1.LabelTopologyRegion])
		if placement, err := DecodePlacement(nodeClaim.Annotations[PlacementAnnotation]); err == nil {
			inUse.Insert(placement.Name)
		}
	}
	return inUse
}

func (r *Registry) setUnavailable(uid types.UID, regions sets.Set[string]) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if regions.Len() == 0 {
		delete(r.unavailable, uid)
		return
	}
	if r.unavailable == nil {
		r.unavailable = map[types.UID]sets.Set[string]{}
	}
	r.unavailable[uid] = regions
}

// isUnavailable reports whether a region failed the NodeClass's last check.
func (r *Registry) isUnavailable(uid types.UID, region string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.unavailable[uid].Has(region)
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
	var leftOut error
	for _, region := range nodeClass.Spec.Regions {
		images, err := p.Registry.provider(region.Name).Images.List(ctx, nodeClass)
		if err != nil {
			err = fmt.Errorf("region %s images: %w", region.Name, err)
			// Preserve the previously resolved image set on transient regional failures. A region
			// the status has no image for has no nodes to keep: it is left out so that the other
			// regions' images still refresh, and ValidateSubnets keeps new nodes out of it.
			if hasImageFor(nodeClass, region.Name) {
				return nil, err
			}
			log.FromContext(ctx).Error(err, "leaving a region without node images out of the NodeClass images", "region", region.Name)
			leftOut = err
			continue
		}
		for _, image := range images {
			image.Requirements = scheduling.NewNodeSelectorRequirementsWithMinValues(image.Requirements.NodeSelectorRequirements()...)
			image.Requirements.Add(scheduling.NewRequirement(corev1.LabelTopologyRegion, corev1.NodeSelectorOpIn, region.Name))
			result = append(result, image)
		}
	}
	if len(result) == 0 && leftOut != nil {
		return nil, leftOut
	}
	return result, nil
}
