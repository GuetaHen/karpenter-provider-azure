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

package options

import (
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/karpenter-provider-azure/pkg/apis/coexistence"
	"github.com/Azure/karpenter-provider-azure/pkg/apis/v1beta1"
	"github.com/Azure/karpenter-provider-azure/pkg/consts"
	"github.com/google/uuid"
	"github.com/samber/lo"
)

var regionNamePattern = regexp.MustCompile(`^[a-z][a-z0-9]{1,63}$`)

// ValidateRegions is also called on the launch path, so stale Ready conditions
// cannot enable an unsupported configuration.
func (o *Options) ValidateRegions(nodeClass *v1beta1.AKSNodeClass) error {
	if len(nodeClass.Spec.Regions) == 0 {
		return nil
	}
	if !o.EnableMultiRegion {
		return fmt.Errorf("spec.regions requires the controller option --enable-multi-region (ENABLE_MULTI_REGION=true)")
	}
	if err := o.validateRegionalMode(); err != nil {
		return err
	}
	if err := validateRegionalFeatures(nodeClass); err != nil {
		return err
	}
	return validateRegionalTargets(nodeClass.Spec.Regions)
}

func (o *Options) validateRegionalMode() error {
	if o.ProvisionMode != consts.ProvisionModeAKSScriptless {
		return fmt.Errorf("spec.regions requires aksscriptless provisioning; AKS Machine API and bootstrappingclient cannot provision remote nodes")
	}
	if !o.regionalNetworkSupported() {
		return fmt.Errorf("spec.regions requires networkPlugin=none and an empty networkPluginMode with a cross-region-capable BYO CNI, or Azure CNI Overlay with --multi-region-external-pod-network; otherwise managed Azure CNI is unsupported")
	}
	if o.DiskEncryptionSetID != "" {
		return fmt.Errorf("spec.regions does not support a cluster-wide disk encryption set")
	}
	return nil
}

func validateRegionalFeatures(nodeClass *v1beta1.AKSNodeClass) error {
	if nodeClass.Spec.CapacityReservation != nil || nodeClass.IsNodePublicIPEnabled() {
		return fmt.Errorf("spec.regions does not support disk encryption sets, capacity reservations, or node public IPs")
	}
	if nodeClass.Spec.VNETSubnetID != nil {
		return fmt.Errorf("spec.regions and spec.vnetSubnetID are mutually exclusive")
	}
	if v1beta1.GetOSForImageFamily(lo.FromPtr(nodeClass.Spec.ImageFamily)) != "linux" {
		return fmt.Errorf("spec.regions supports Linux nodes only")
	}
	if nodeClass.Spec.Versions != nil && lo.FromPtr(nodeClass.Spec.Versions.NodeImageVersion) != "" {
		return fmt.Errorf("spec.regions does not support nodeImageVersion pinning; image availability must be resolved independently in each region")
	}
	return nil
}

func validateRegionalTargets(regions []v1beta1.Region) error {
	if len(regions) > 8 {
		return fmt.Errorf("spec.regions supports at most eight regions")
	}
	seen := map[string]bool{}
	for _, region := range regions {
		if !regionNamePattern.MatchString(region.Name) || seen[region.Name] {
			return fmt.Errorf("spec.regions contains an invalid or duplicate location %q", region.Name)
		}
		seen[region.Name] = true
		id, err := arm.ParseResourceID(region.VNETSubnetID)
		if err != nil || !strings.EqualFold(id.ResourceType.String(), "Microsoft.Network/virtualNetworks/subnets") {
			return fmt.Errorf("spec.regions[%s].vnetSubnetID must be a subnet ARM resource ID", region.Name)
		}
	}
	return nil
}

// regionalNetworkSupported reports whether the cluster network can host regional nodes: a BYO
// CNI with networkPlugin none, or Azure CNI Overlay whose regional pod network an external
// agent supplies.
func (o *Options) regionalNetworkSupported() bool {
	if o.IsNetworkPluginNone() && o.NetworkPluginMode == "" {
		return true
	}
	return o.MultiRegionExternalPodNetwork && o.IsAzureCNIOverlay()
}

// RegionalNodeIdentity is the identity configuration of the VMs in one region.
type RegionalNodeIdentity struct {
	NodeIdentities          []string `json:"nodeIdentities"`
	KubeletIdentityClientID string   `json:"kubeletIdentityClientID"`
}

// NodeIdentitiesFor returns the user-assigned identities for the VMs in location.
func (o *Options) NodeIdentitiesFor(location string) []string {
	if identity, ok := o.regionalIdentity(location); ok {
		return identity.NodeIdentities
	}
	return o.NodeIdentities
}

// KubeletIdentityClientIDFor returns the kubelet identity client ID for the nodes in location.
func (o *Options) KubeletIdentityClientIDFor(location string) string {
	if identity, ok := o.regionalIdentity(location); ok {
		return identity.KubeletIdentityClientID
	}
	return o.KubeletIdentityClientID
}

func (o *Options) regionalIdentity(location string) (RegionalNodeIdentity, bool) {
	if len(o.MultiRegionNodeIdentities) == 0 {
		return RegionalNodeIdentity{}, false
	}
	identity, ok := o.MultiRegionNodeIdentities[strings.ToLower(location)]
	return identity, ok
}

// ValidateHomeLocation rejects a node identity override for the cluster region, whose nodes
// always use --node-identities and --kubelet-identity-client-id.
func (o *Options) ValidateHomeLocation(home string) error {
	if _, ok := o.regionalIdentity(home); ok {
		return fmt.Errorf("multi-region-node-identities must not contain the cluster region %q", strings.ToLower(home))
	}
	return nil
}

// ValidateCoexistenceFeatureGates rejects, in coexistence mode, the CapacityBuffer feature: both
// controllers would provision for the same CapacityBuffers and write their status. NodeOverlay is
// allowed because the operator keeps the core controllers from writing NodeOverlay status.
func ValidateCoexistenceFeatureGates(capacityBuffer bool) error {
	if coexistence.Enabled() && capacityBuffer {
		return fmt.Errorf("%s=true requires the CapacityBuffer feature gate to be disabled, because both controllers would reconcile CapacityBuffers", coexistence.EnvVar)
	}
	return nil
}

func coexistenceEnabled() bool {
	return coexistence.Enabled()
}

func (o *Options) validateCoexistence() error {
	if err := coexistence.Err(); err != nil {
		return err
	}
	if o.CoexistenceMode && (o.IsAKSMachineAPIMode() || o.ManageExistingAKSMachines) {
		return fmt.Errorf("%s=true does not support AKS Machine API provision modes or manage-existing-aks-machines", coexistence.EnvVar)
	}
	return nil
}

func (o *Options) validateMultiRegionOptions() error {
	if o.MultiRegionExternalPodNetwork && (!o.EnableMultiRegion || !o.IsAzureCNIOverlay()) {
		return fmt.Errorf("multi-region-external-pod-network requires enable-multi-region with network-plugin azure and network-plugin-mode overlay")
	}
	if err := o.parseRegionalNodeIdentities(); err != nil {
		return err
	}
	if len(o.MultiRegionNodeIdentities) == 0 {
		return nil
	}
	if !o.EnableMultiRegion {
		return fmt.Errorf("multi-region-node-identities requires enable-multi-region")
	}
	for _, region := range slices.Sorted(maps.Keys(o.MultiRegionNodeIdentities)) {
		if err := validateRegionalNodeIdentity(region, o.MultiRegionNodeIdentities[region]); err != nil {
			return err
		}
	}
	return nil
}

// parseRegionalNodeIdentities decodes --multi-region-node-identities with lowercase region keys.
func (o *Options) parseRegionalNodeIdentities() error {
	if o.multiRegionNodeIdentitiesJSON == "" {
		return nil
	}
	identities := map[string]RegionalNodeIdentity{}
	decoder := json.NewDecoder(strings.NewReader(o.multiRegionNodeIdentitiesJSON))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&identities); err != nil {
		return fmt.Errorf("multi-region-node-identities must be a JSON object that maps a region to nodeIdentities and kubeletIdentityClientID: %w", err)
	}
	o.MultiRegionNodeIdentities = make(map[string]RegionalNodeIdentity, len(identities))
	for region, identity := range identities {
		key := strings.ToLower(region)
		if _, ok := o.MultiRegionNodeIdentities[key]; ok {
			return fmt.Errorf("multi-region-node-identities lists region %q more than once", key)
		}
		o.MultiRegionNodeIdentities[key] = identity
	}
	return nil
}

func validateRegionalNodeIdentity(region string, identity RegionalNodeIdentity) error {
	if !regionNamePattern.MatchString(region) {
		return fmt.Errorf("multi-region-node-identities contains an invalid region %q", region)
	}
	if len(identity.NodeIdentities) == 0 {
		return fmt.Errorf("multi-region-node-identities[%s].nodeIdentities must list at least one identity", region)
	}
	for _, id := range identity.NodeIdentities {
		parsed, err := arm.ParseResourceID(id)
		if err != nil || !strings.EqualFold(parsed.ResourceType.String(), "Microsoft.ManagedIdentity/userAssignedIdentities") {
			return fmt.Errorf("multi-region-node-identities[%s].nodeIdentities must contain user-assigned identity resource IDs, got %q", region, id)
		}
	}
	if uuid.Validate(identity.KubeletIdentityClientID) != nil {
		return fmt.Errorf("multi-region-node-identities[%s].kubeletIdentityClientID must be a client ID", region)
	}
	return nil
}
