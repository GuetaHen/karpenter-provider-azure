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
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Azure/karpenter-provider-azure/pkg/apis/v1beta1"
	"github.com/Azure/karpenter-provider-azure/pkg/consts"
	"github.com/Azure/karpenter-provider-azure/pkg/operator/options"
	"github.com/Azure/karpenter-provider-azure/pkg/providers/allocationstrategy"
	"github.com/Azure/karpenter-provider-azure/pkg/providers/instance"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"
)

const PlacementAnnotation = "karpenter.azure.com/region-placement"

type Instances struct {
	instance.VMProvider
	Registry   *Registry
	KubeClient client.Client
}

func (p *Instances) BeginCreate(ctx context.Context, nodeClass *v1beta1.AKSNodeClass, nodeClaim *karpv1.NodeClaim, types []*cloudprovider.InstanceType) (*instance.VirtualMachinePromise, error) {
	if len(nodeClass.Spec.Regions) == 0 && nodeClaim.Annotations[PlacementAnnotation] == "" {
		return p.VMProvider.BeginCreate(ctx, nodeClass, nodeClaim, types)
	}
	if err := options.FromContext(ctx).ValidateRegions(nodeClass); err != nil {
		return nil, err
	}
	stored, err := p.readClaim(ctx, nodeClaim)
	if err != nil {
		return nil, err
	}
	placement, err := p.resolvePlacement(ctx, nodeClass, stored, types)
	if err != nil {
		return nil, err
	}
	if err := p.persistPlacement(ctx, nodeClass, stored, placement); err != nil {
		return nil, err
	}
	return p.launch(ctx, nodeClass, stored, types, placement)
}

func (p *Instances) readClaim(ctx context.Context, nodeClaim *karpv1.NodeClaim) (*karpv1.NodeClaim, error) {
	requirements := scheduling.NewNodeSelectorRequirementsWithMinValues(nodeClaim.Spec.Requirements...)
	if (requirements.Has(corev1.LabelNodeExcludeBalancers) && !requirements.Get(corev1.LabelNodeExcludeBalancers).Has("true")) ||
		(nodeClaim.Labels[corev1.LabelNodeExcludeBalancers] != "" && nodeClaim.Labels[corev1.LabelNodeExcludeBalancers] != "true") {
		return nil, fmt.Errorf("regional nodes must be excluded from external load balancers")
	}
	// Always read the current annotation. Optimistic locking below prevents two
	// concurrent callers from committing different regions before Azure is touched.
	stored := &karpv1.NodeClaim{}
	if err := p.KubeClient.Get(ctx, client.ObjectKeyFromObject(nodeClaim), stored); err != nil {
		return nil, fmt.Errorf("reading placement: %w", err)
	}
	if stored.UID != nodeClaim.UID || !stored.DeletionTimestamp.IsZero() {
		return nil, fmt.Errorf("nodeclaim identity changed or deletion started before regional placement")
	}
	return stored, nil
}

func (p *Instances) resolvePlacement(ctx context.Context, nodeClass *v1beta1.AKSNodeClass, stored *karpv1.NodeClaim, types []*cloudprovider.InstanceType) (v1beta1.Region, error) {
	var placement v1beta1.Region
	if raw := stored.Annotations[PlacementAnnotation]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &placement); err != nil {
			return placement, fmt.Errorf("decoding persisted regional placement: %w", err)
		}
	}
	placement, err := p.existingPlacement(ctx, nodeClass, stored, placement)
	if err != nil {
		return placement, err
	}
	if placement.Name != "" {
		return placement, nil
	}
	selection := allocationstrategy.NewProvider(nil, consts.ComputeRecommendationModeDisabled).Allocate(
		ctx, types, scheduling.NewNodeSelectorRequirementsWithMinValues(stored.Spec.Requirements...))
	if selection == nil {
		return placement, cloudprovider.NewInsufficientCapacityError(fmt.Errorf("no regional offerings available"))
	}
	region := selection.Offering.Requirements.Get(corev1.LabelTopologyRegion).Any()
	placement, _ = lo.Find(nodeClass.Spec.Regions, func(r v1beta1.Region) bool { return r.Name == region })
	return placement, nil
}

func (p *Instances) existingPlacement(ctx context.Context, nodeClass *v1beta1.AKSNodeClass, claim *karpv1.NodeClaim, placement v1beta1.Region) (v1beta1.Region, error) {
	vm, err := p.Get(ctx, instance.GenerateResourceName(claim.Name))
	if cloudprovider.IsNodeClaimNotFoundError(err) {
		return placement, nil
	}
	if err != nil {
		return placement, err
	}
	region := strings.ToLower(lo.FromPtr(vm.Location))
	if placement.Name != "" && placement.Name != region {
		return placement, fmt.Errorf("existing VM region %q differs from persisted placement %q", region, placement.Name)
	}
	if placement.Name != "" {
		return placement, nil
	}
	placement, ok := lo.Find(nodeClass.Spec.Regions, func(r v1beta1.Region) bool { return r.Name == region })
	if !ok {
		return placement, fmt.Errorf("existing VM region %q is not configured", region)
	}
	return placement, nil
}

func (p *Instances) persistPlacement(ctx context.Context, nodeClass *v1beta1.AKSNodeClass, stored *karpv1.NodeClaim, placement v1beta1.Region) error {
	if _, ok := lo.Find(nodeClass.Spec.Regions, func(r v1beta1.Region) bool {
		return r.Name == placement.Name && strings.EqualFold(r.VNETSubnetID, placement.VNETSubnetID)
	}); !ok {
		return fmt.Errorf("persisted regional placement no longer matches spec.regions; recreate the NodeClaim to select another region")
	}
	if err := p.Registry.validate(ctx, placement); err != nil {
		return fmt.Errorf("validating regional network before launch: %w", err)
	}
	if stored.Annotations[PlacementAnnotation] == "" {
		base := stored.DeepCopy()
		raw, err := json.Marshal(placement)
		if err != nil {
			return err
		}
		stored.Annotations = lo.Assign(stored.Annotations, map[string]string{PlacementAnnotation: string(raw)})
		if err := p.KubeClient.Patch(ctx, stored, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			return fmt.Errorf("persisting regional placement before launch: %w", err)
		}
	}
	return nil
}

func (p *Instances) launch(ctx context.Context, nodeClass *v1beta1.AKSNodeClass, stored *karpv1.NodeClaim, types []*cloudprovider.InstanceType, placement v1beta1.Region) (*instance.VirtualMachinePromise, error) {
	providers, err := p.Registry.get(ctx, placement.Name)
	if err != nil {
		return nil, err
	}
	regionalTypes := narrow(types, placement.Name)
	if len(regionalTypes) == 0 {
		return nil, cloudprovider.NewInsufficientCapacityError(fmt.Errorf("no compatible instance types in pinned region %s", placement.Name))
	}
	scopedClass := nodeClass.DeepCopy()
	scopedClass.Spec.VNETSubnetID = lo.ToPtr(placement.VNETSubnetID)
	promise, err := providers.Instances.BeginCreate(ctx, scopedClass, stored, regionalTypes)
	if err != nil {
		return nil, err
	}
	promise.NodeClaimAnnotations = map[string]string{PlacementAnnotation: stored.Annotations[PlacementAnnotation]}
	return promise, nil
}

// narrow preserves NodeOverlay resource/price adjustments while removing every
// offering from other regions. Never mutate the shared instance-type cache.
func narrow(types []*cloudprovider.InstanceType, region string) []*cloudprovider.InstanceType {
	var result []*cloudprovider.InstanceType
	for _, it := range types {
		copy := it.DeepCopy()
		copy.Offerings = lo.Filter(copy.Offerings, func(o *cloudprovider.Offering, _ int) bool {
			return o.Requirements.Get(corev1.LabelTopologyRegion).Has(region)
		})
		if len(copy.Offerings) == 0 {
			continue
		}
		for _, key := range regionalKeys {
			values := []string{}
			for _, offering := range copy.Offerings {
				values = append(values, offering.Requirements.Get(key).Values()...)
			}
			copy.Requirements[key] = scheduling.NewRequirement(key, corev1.NodeSelectorOpIn, lo.Uniq(values)...)
		}
		result = append(result, copy)
	}
	return result
}

func (p *Instances) Delete(ctx context.Context, nodeClaim *karpv1.NodeClaim, name string) error {
	region := nodeClaim.Labels[corev1.LabelTopologyRegion]
	if region == "" || region == p.Registry.home {
		return p.VMProvider.Delete(ctx, nodeClaim, name)
	}
	// Deletion must not depend on quota/SKU refresh, network validation, or the
	// continued existence of spec.regions. The regional instance owns its quota.
	return p.Registry.provider(region).Instances.Delete(ctx, nodeClaim, name)
}
