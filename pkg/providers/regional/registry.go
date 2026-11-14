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
	"fmt"
	"sync"
	"time"

	"github.com/Azure/karpenter-provider-azure/pkg/apis/v1beta1"
	"github.com/Azure/karpenter-provider-azure/pkg/operator/options"
	"github.com/Azure/karpenter-provider-azure/pkg/providers/imagefamily"
	"github.com/Azure/karpenter-provider-azure/pkg/providers/instance"
	"github.com/Azure/karpenter-provider-azure/pkg/providers/instancetype"
	"github.com/Azure/karpenter-provider-azure/pkg/providers/quota"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/log"
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

	mu      sync.Mutex
	entries map[string]*entry
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
		return nil, fmt.Errorf("region %s has no SKU catalog yet: %w", region, p.skusErr)
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
