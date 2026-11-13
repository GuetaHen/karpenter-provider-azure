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

// ValidateRegions is also called on the launch path, so stale Ready conditions
// cannot enable an unsupported configuration.
func (o *Options) ValidateRegions(nodeClass *v1beta1.AKSNodeClass) error {
	if len(nodeClass.Spec.Regions) == 0 {
		return nil
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
	if !o.IsNetworkPluginNone() || o.NetworkPluginMode != "" {
		return fmt.Errorf("spec.regions requires networkPlugin=none and an empty networkPluginMode with a cross-region-capable BYO CNI; managed Azure CNI is unsupported")
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
