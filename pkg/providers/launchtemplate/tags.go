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

package launchtemplate

import (
	"strings"

	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/Azure/karpenter-provider-azure/pkg/apis/coexistence"
	"github.com/Azure/karpenter-provider-azure/pkg/apis/v1beta1"
	"github.com/Azure/karpenter-provider-azure/pkg/operator/options"
	"github.com/samber/lo"
)

const (
	KarpenterManagedTagKey             = "karpenter.azure.com_cluster"
	KarpenterAKSMachineNodeClaimTagKey = "karpenter.azure.com_aksmachine_nodeclaim"
	BillingTagKey                      = "compute.aks.billing"
	BillingTagValueLinux               = "linux"
	// OwnerTagKey marks the resources a coexistence-mode controller creates. Its value is the
	// API group that controller serves. Listing, adoption, update and cleanup require it.
	OwnerTagKey = "karpenter.azure.com_owner"
	// CoexistenceNodePoolTagKey carries the NodePool name in coexistence mode in place of
	// DefaultNodePoolTagKey, which other installations select their resources by.
	CoexistenceNodePoolTagKey = "karpenter.azure.com_nodepool"
)

var (
	// DefaultNodePoolTagKey carries the NodePool name outside coexistence mode.
	DefaultNodePoolTagKey = strings.ReplaceAll(karpv1.NodePoolLabelKey, "/", "_")
	// NodePoolTagKey is the NodePool tag key of this process. Like the API group, it is fixed
	// at process start.
	NodePoolTagKey = lo.Ternary(coexistence.Enabled(), CoexistenceNodePoolTagKey, DefaultNodePoolTagKey)
	// stockSelectorTerms are matched anywhere in a resource's tags by the Resource Graph
	// listings of installations that do not run in coexistence mode.
	stockSelectorTerms = []string{DefaultNodePoolTagKey, KarpenterAKSMachineNodeClaimTagKey}
)

// OwnerTagValue returns the owner tag value this process stamps and requires, or "" outside
// coexistence mode.
func OwnerTagValue() string {
	if !coexistence.Enabled() {
		return ""
	}
	return coexistence.Group
}

// IsOwned reports whether tags mark a resource as created by this process. Outside
// coexistence mode every resource that the existing listings select is owned.
func IsOwned(tags map[string]*string) bool {
	owner := OwnerTagValue()
	return owner == "" || lo.FromPtr(tags[OwnerTagKey]) == owner
}

// isCoexistenceReservedTag reports a NodeClass or additional tag that coexistence mode drops,
// because it would forge ownership or expose the resource to another installation's listings.
func isCoexistenceReservedTag(key string, value *string) bool {
	if strings.EqualFold(key, OwnerTagKey) || strings.EqualFold(key, CoexistenceNodePoolTagKey) {
		return true
	}
	return lo.ContainsBy(stockSelectorTerms, func(term string) bool {
		return strings.Contains(strings.ToLower(key), term) || strings.Contains(strings.ToLower(lo.FromPtr(value)), term)
	})
}

// TODO: Would like to refactor this out of launchtemplate at some point
// Tags returns the tags to be applied to a resource (VM, Disk, NIC, etc)
func Tags(
	options *options.Options,
	nodeClass *v1beta1.AKSNodeClass,
	nodeClaim *karpv1.NodeClaim,
) map[string]*string {
	defaultTags := map[string]string{
		KarpenterManagedTagKey: options.ClusterName,
		BillingTagKey:          BillingTagValueLinux,
	}
	// Note: Be careful depending on nodeClaim.Labels here, as we assign some additional labels during the creation
	// of the static parameters for the launch template. Those labels haven't actually been applied to the nodeClaim yet,
	// so if you try to use them here you will find they are missing.
	// For now, we only depend on labels that are added to the nodeClaim itself.
	if val, ok := nodeClaim.Labels[karpv1.NodePoolLabelKey]; ok {
		defaultTags[NodePoolTagKey] = val
	}
	if owner := OwnerTagValue(); owner != "" {
		defaultTags[OwnerTagKey] = owner
	}

	// MapEntries first so that karpenter.azure.com_cluster and karpenter.azure.com/cluster collide
	additionalTags := lo.MapEntries(options.AdditionalTags, mapTags)
	nodeClassTags := lo.MapEntries(nodeClass.Spec.Tags, mapTags)
	defaultTagsMapped := lo.MapEntries(defaultTags, mapTags)
	if coexistence.Enabled() {
		additionalTags = lo.OmitBy(additionalTags, isCoexistenceReservedTag)
		nodeClassTags = lo.OmitBy(nodeClassTags, isCoexistenceReservedTag)
	}

	return lo.Assign(additionalTags, nodeClassTags, defaultTagsMapped)
}

func mapTags(key string, value string) (string, *string) {
	return strings.ReplaceAll(key, "/", "_"), lo.ToPtr(value)
}
