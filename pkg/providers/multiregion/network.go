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

package multiregion

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork"
	"github.com/Azure/karpenter-provider-azure/pkg/apis/v1beta1"
	"github.com/Azure/karpenter-provider-azure/pkg/providers/azclient/azapi"
	"github.com/samber/lo"
	"k8s.io/utils/clock"
)

type VirtualNetworksAPI interface {
	Get(context.Context, string, string, *armnetwork.VirtualNetworksClientGetOptions) (armnetwork.VirtualNetworksClientGetResponse, error)
}

type NetworkValidator struct {
	SubscriptionID  string
	VirtualNetworks VirtualNetworksAPI
	Subnets         azapi.SubnetsAPI
	// Clock defaults to the real clock.
	Clock clock.Clock

	mu        sync.Mutex
	validated map[string]time.Time
}

// validationTTL lets bursts of launches and status reconciles share one successful
// check per region and subnet without hiding a broken subnet for long.
const validationTTL = time.Minute

// Validate checks a subnet of a listed region, reusing a success for validationTTL. Failures are
// never cached, so a repaired subnet is accepted on the next call.
func (v *NetworkValidator) Validate(ctx context.Context, region v1beta1.Region) error {
	clk := v.Clock
	if clk == nil {
		clk = clock.RealClock{}
	}
	key := region.Name + "|" + strings.ToLower(region.VNETSubnetID)
	now := clk.Now()
	v.mu.Lock()
	validatedAt, ok := v.validated[key]
	v.mu.Unlock()
	if ok && now.Sub(validatedAt) < validationTTL {
		return nil
	}
	if err := v.validate(ctx, region); err != nil {
		return err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.validated == nil || len(v.validated) >= 64 {
		v.validated = map[string]time.Time{}
	}
	v.validated[key] = now
	return nil
}

func (v *NetworkValidator) validate(ctx context.Context, region v1beta1.Region) error {
	id, err := arm.ParseResourceID(region.VNETSubnetID)
	if err != nil {
		return err
	}
	if !strings.EqualFold(id.SubscriptionID, v.SubscriptionID) ||
		!strings.EqualFold(id.ResourceType.String(), "Microsoft.Network/virtualNetworks/subnets") {
		return fmt.Errorf("subnet of a listed region must be in the cluster subscription")
	}
	vnet, err := v.VirtualNetworks.Get(ctx, id.ResourceGroupName, id.Parent.Name, nil)
	if err != nil {
		return fmt.Errorf("getting VNet of a listed region: %w", err)
	}
	if !strings.EqualFold(lo.FromPtr(vnet.Location), region.Name) {
		return fmt.Errorf("VNet location %q does not match configured region %q", lo.FromPtr(vnet.Location), region.Name)
	}
	subnet, err := v.Subnets.Get(ctx, id.ResourceGroupName, id.Parent.Name, id.Name, nil)
	if err != nil {
		return fmt.Errorf("getting subnet of a listed region: %w", err)
	}
	if subnet.Properties == nil || subnet.Properties.NetworkSecurityGroup == nil || lo.FromPtr(subnet.Properties.NetworkSecurityGroup.ID) == "" {
		return fmt.Errorf("subnet of a listed region must have a network security group; the cluster NSG cannot be attached across regions")
	}
	return nil
}
