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

package azure

import (
	"strings"

	"github.com/samber/lo"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork"
)

// ExpectVNETsPeered expects the VNets of the given subnets to be peered in a full mesh: each VNet
// has a connected peering to every other one. VNet peering is not transitive, and the pod network
// of multi-region nodes needs a direct path between every two node subnets.
func (env *Environment) ExpectVNETsPeered(subnetIDs ...string) {
	GinkgoHelper()
	vnets := map[string]*arm.ResourceID{}
	for _, subnetID := range subnetIDs {
		subnet, err := arm.ParseResourceID(subnetID)
		Expect(err).ToNot(HaveOccurred(), "parsing subnet ID %s", subnetID)
		vnets[strings.ToLower(subnet.Parent.String())] = subnet.Parent
	}
	for id, vnet := range vnets {
		connected := map[string]bool{}
		pager := env.vnetPeeringsClient.NewListPager(vnet.ResourceGroupName, vnet.Name, nil)
		for pager.More() {
			page, err := pager.NextPage(env.Context)
			Expect(err).ToNot(HaveOccurred(), "listing the peerings of VNet %s", vnet.String())
			for _, peering := range page.Value {
				if peering.Properties == nil || peering.Properties.RemoteVirtualNetwork == nil {
					continue
				}
				if lo.FromPtr(peering.Properties.PeeringState) == armnetwork.VirtualNetworkPeeringStateConnected {
					connected[strings.ToLower(lo.FromPtr(peering.Properties.RemoteVirtualNetwork.ID))] = true
				}
			}
		}
		for other := range vnets {
			if other != id {
				Expect(connected).To(HaveKey(other), "VNet %s has no connected peering to VNet %s; the pod network of multi-region nodes needs a full mesh of peerings", vnet.String(), other)
			}
		}
	}
}
