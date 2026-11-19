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
	"fmt"
	"regexp"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/karpenter-provider-azure/pkg/apis/v1beta1"
	"github.com/Azure/karpenter-provider-azure/pkg/consts"
	"github.com/samber/lo"
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
func (o *Options) multiRegionNetworkSupported() bool {
	return o.IsNetworkPluginNone() && o.NetworkPluginMode == ""
}
