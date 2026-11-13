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
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork"
	"github.com/Azure/karpenter-provider-azure/pkg/apis/v1beta1"
	"github.com/Azure/karpenter-provider-azure/pkg/providers/azclient/azapi"
	"github.com/samber/lo"
)

type VirtualNetworksAPI interface {
	Get(context.Context, string, string, *armnetwork.VirtualNetworksClientGetOptions) (armnetwork.VirtualNetworksClientGetResponse, error)
}

type NetworkValidator struct {
	SubscriptionID  string
	VirtualNetworks VirtualNetworksAPI
	Subnets         azapi.SubnetsAPI
}

func (v *NetworkValidator) Validate(ctx context.Context, region v1beta1.Region) error {
	id, err := arm.ParseResourceID(region.VNETSubnetID)
	if err != nil {
		return err
	}
	if !strings.EqualFold(id.SubscriptionID, v.SubscriptionID) ||
		!strings.EqualFold(id.ResourceType.String(), "Microsoft.Network/virtualNetworks/subnets") {
		return fmt.Errorf("regional subnet must be in the cluster subscription")
	}
	vnet, err := v.VirtualNetworks.Get(ctx, id.ResourceGroupName, id.Parent.Name, nil)
	if err != nil {
		return fmt.Errorf("getting regional VNet: %w", err)
	}
	if !strings.EqualFold(lo.FromPtr(vnet.Location), region.Name) {
		return fmt.Errorf("VNet location %q does not match configured region %q", lo.FromPtr(vnet.Location), region.Name)
	}
	subnet, err := v.Subnets.Get(ctx, id.ResourceGroupName, id.Parent.Name, id.Name, nil)
	if err != nil {
		return fmt.Errorf("getting regional subnet: %w", err)
	}
	if subnet.Properties == nil || subnet.Properties.NetworkSecurityGroup == nil || lo.FromPtr(subnet.Properties.NetworkSecurityGroup.ID) == "" {
		return fmt.Errorf("regional subnet must have a network security group; the cluster NSG cannot be attached across regions")
	}
	return nil
}
