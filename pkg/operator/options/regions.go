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
	"github.com/Azure/karpenter-provider-azure/pkg/apis/v1beta1"
	"github.com/Azure/karpenter-provider-azure/pkg/consts"
	"github.com/google/uuid"
	"github.com/samber/lo"
	"k8s.io/apimachinery/pkg/util/validation"
)

var regionNamePattern = regexp.MustCompile(`^[a-z][a-z0-9]{1,63}$`)

// Reasons that the status conditions of an AKSNodeClass with spec.regions report when it cannot be used.
const (
	ReasonMultiRegionDisabled                 = "MultiRegionDisabled"
	ReasonMultiRegionProvisionModeUnsupported = "MultiRegionProvisionModeUnsupported"
	ReasonMultiRegionUnsupportedConfiguration = "MultiRegionUnsupportedConfiguration"
)

// RegionsError is an error of ValidateRegions with the reason that status conditions report.
type RegionsError struct {
	reason string
	err    error
}

func (e *RegionsError) Error() string {
	return e.err.Error()
}

// Reason returns the reason that status conditions report.
func (e *RegionsError) Reason() string {
	return e.reason
}

// RegionsErrorReason returns the condition reason for an error of ValidateRegions.
func RegionsErrorReason(err error) string {
	if regionsErr, ok := lo.ErrorsAs[*RegionsError](err); ok {
		return regionsErr.Reason()
	}
	return ReasonMultiRegionUnsupportedConfiguration
}

// ValidateRegions is also called on the launch path, so stale Ready conditions
// cannot enable an unsupported configuration.
func (o *Options) ValidateRegions(nodeClass *v1beta1.AKSNodeClass) error {
	if len(nodeClass.Spec.Regions) == 0 {
		return nil
	}
	if !o.EnableMultiRegion {
		return &RegionsError{reason: ReasonMultiRegionDisabled, err: fmt.Errorf("spec.regions requires the controller option --enable-multi-region (ENABLE_MULTI_REGION=true)")}
	}
	if o.ProvisionMode != consts.ProvisionModeAKSScriptless {
		return &RegionsError{reason: ReasonMultiRegionProvisionModeUnsupported,
			err: fmt.Errorf("spec.regions requires aksscriptless provisioning; provision mode %s cannot launch nodes outside the cluster region", o.ProvisionMode)}
	}
	err := o.validateMultiRegionMode()
	if err == nil {
		err = validateMultiRegionFeatures(nodeClass)
	}
	if err == nil {
		err = validateMultiRegionTargets(nodeClass.Spec.Regions)
	}
	if err != nil {
		return &RegionsError{reason: ReasonMultiRegionUnsupportedConfiguration, err: err}
	}
	return nil
}

func (o *Options) validateMultiRegionMode() error {
	if !o.multiRegionNetworkSupported() {
		return fmt.Errorf("spec.regions requires networkPlugin=none and an empty networkPluginMode, with a bring-your-own CNI whose pod network reaches all listed regions; Azure CNI is not supported in any mode")
	}
	if o.DiskEncryptionSetID != "" {
		return fmt.Errorf("spec.regions does not support a cluster-wide disk encryption set")
	}
	return nil
}

func validateMultiRegionFeatures(nodeClass *v1beta1.AKSNodeClass) error {
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

func validateMultiRegionTargets(regions []v1beta1.Region) error {
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

// multiRegionNetworkSupported reports whether pod traffic can cross the global VNet peerings between the
// regions without changes to AKS-managed network components. Only a bring-your-own CNI (networkPlugin
// none) qualifies. Azure CNI Overlay routes its pod ranges only in the cluster VNet, and Azure CNI with
// pod IPs from the node subnet depends on AKS-managed IP address management and masquerade rules that
// are not verified for nodes in other regions.
// With --multi-region-external-pod-network, Azure CNI Overlay is also supported: an external agent then
// supplies the pod network of these nodes.
func (o *Options) multiRegionNetworkSupported() bool {
	if o.IsNetworkPluginNone() && o.NetworkPluginMode == "" {
		return true
	}
	return o.MultiRegionExternalPodNetwork && o.IsAzureCNIOverlay()
}

// MultiRegionNodeIdentity is the identity configuration of the VMs in one region.
type MultiRegionNodeIdentity struct {
	NodeIdentities          []string `json:"nodeIdentities"`
	KubeletIdentityClientID string   `json:"kubeletIdentityClientID"`
}

// NodeIdentitiesFor returns the user-assigned identities for the VMs in location.
func (o *Options) NodeIdentitiesFor(location string) []string {
	if identity, ok := o.regionIdentity(location); ok {
		return identity.NodeIdentities
	}
	return o.NodeIdentities
}

// KubeletIdentityClientIDFor returns the kubelet identity client ID for the nodes in location.
func (o *Options) KubeletIdentityClientIDFor(location string) string {
	if identity, ok := o.regionIdentity(location); ok {
		return identity.KubeletIdentityClientID
	}
	return o.KubeletIdentityClientID
}

func (o *Options) regionIdentity(location string) (MultiRegionNodeIdentity, bool) {
	if len(o.MultiRegionNodeIdentities) == 0 {
		return MultiRegionNodeIdentity{}, false
	}
	identity, ok := o.MultiRegionNodeIdentities[strings.ToLower(location)]
	return identity, ok
}

// ValidateHomeLocation rejects a node identity override for the cluster region, whose nodes
// always use --node-identities and --kubelet-identity-client-id.
func (o *Options) ValidateHomeLocation(home string) error {
	if _, ok := o.regionIdentity(home); ok {
		return fmt.Errorf("multi-region-node-identities must not contain the cluster region %q", strings.ToLower(home))
	}
	return nil
}

func (o *Options) validateMultiRegionOptions() error {
	if o.MultiRegionExternalPodNetwork && (!o.EnableMultiRegion || !o.IsAzureCNIOverlay()) {
		return fmt.Errorf("multi-region-external-pod-network requires enable-multi-region with network-plugin azure and network-plugin-mode overlay")
	}
	if err := o.validatePodNetworkStartupTaint(); err != nil {
		return err
	}
	if err := o.parseMultiRegionNodeIdentities(); err != nil {
		return err
	}
	if len(o.MultiRegionNodeIdentities) == 0 {
		return nil
	}
	if !o.EnableMultiRegion {
		return fmt.Errorf("multi-region-node-identities requires enable-multi-region")
	}
	for _, region := range slices.Sorted(maps.Keys(o.MultiRegionNodeIdentities)) {
		if err := validateMultiRegionNodeIdentity(region, o.MultiRegionNodeIdentities[region]); err != nil {
			return err
		}
	}
	return nil
}

// validatePodNetworkStartupTaint accepts no key, or a taint key together with the external pod network.
func (o *Options) validatePodNetworkStartupTaint() error {
	key := o.MultiRegionPodNetworkStartupTaint
	if key == "" {
		return nil
	}
	if !o.MultiRegionExternalPodNetwork {
		return fmt.Errorf("multi-region-pod-network-startup-taint requires multi-region-external-pod-network")
	}
	if errs := validation.IsQualifiedName(key); len(errs) > 0 {
		return fmt.Errorf("multi-region-pod-network-startup-taint must be a taint key: %s", strings.Join(errs, "; "))
	}
	return nil
}

// parseMultiRegionNodeIdentities decodes --multi-region-node-identities with lowercase region keys.
func (o *Options) parseMultiRegionNodeIdentities() error {
	if o.multiRegionNodeIdentitiesJSON == "" {
		return nil
	}
	identities := map[string]MultiRegionNodeIdentity{}
	decoder := json.NewDecoder(strings.NewReader(o.multiRegionNodeIdentitiesJSON))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&identities); err != nil {
		return fmt.Errorf("multi-region-node-identities must be a JSON object that maps a region to nodeIdentities and kubeletIdentityClientID: %w", err)
	}
	o.MultiRegionNodeIdentities = make(map[string]MultiRegionNodeIdentity, len(identities))
	for region, identity := range identities {
		key := strings.ToLower(region)
		if _, ok := o.MultiRegionNodeIdentities[key]; ok {
			return fmt.Errorf("multi-region-node-identities lists region %q more than once", key)
		}
		o.MultiRegionNodeIdentities[key] = identity
	}
	return nil
}

func validateMultiRegionNodeIdentity(region string, identity MultiRegionNodeIdentity) error {
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
