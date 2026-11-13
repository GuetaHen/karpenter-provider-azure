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
	"errors"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork"
	"github.com/Azure/karpenter-provider-azure/pkg/providers/azclient/azapi"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
)

type testNetworks struct {
	location string
	err      error
}

func (p *testNetworks) Get(context.Context, string, string, *armnetwork.VirtualNetworksClientGetOptions) (armnetwork.VirtualNetworksClientGetResponse, error) {
	return armnetwork.VirtualNetworksClientGetResponse{VirtualNetwork: armnetwork.VirtualNetwork{Location: lo.ToPtr(p.location)}}, p.err
}

type testSubnets struct {
	azapi.SubnetsAPI
	properties *armnetwork.SubnetPropertiesFormat
	err        error
}

func (p *testSubnets) Get(context.Context, string, string, string, *armnetwork.SubnetsClientGetOptions) (armnetwork.SubnetsClientGetResponse, error) {
	return armnetwork.SubnetsClientGetResponse{Subnet: armnetwork.Subnet{Properties: p.properties}}, p.err
}

func TestRegionalNetworkValidation(t *testing.T) {
	for _, tc := range []struct {
		name, subscription, location string
		properties                   *armnetwork.SubnetPropertiesFormat
		err                          error
		want                         string
	}{
		{name: "correct region and subnet NSG", subscription: "00000000-0000-0000-0000-000000000001", location: "eastus2",
			properties: &armnetwork.SubnetPropertiesFormat{NetworkSecurityGroup: &armnetwork.SecurityGroup{ID: lo.ToPtr("/synthetic/nsg")}}},
		{name: "other subscription", subscription: "other", location: "eastus2", want: "cluster subscription"},
		{name: "other physical region", subscription: "00000000-0000-0000-0000-000000000001", location: "westus2", want: "does not match"},
		{name: "nil subnet properties", subscription: "00000000-0000-0000-0000-000000000001", location: "eastus2", want: "network security group"},
		{name: "missing subnet NSG", subscription: "00000000-0000-0000-0000-000000000001", location: "eastus2", properties: &armnetwork.SubnetPropertiesFormat{}, want: "network security group"},
		{name: "API failure", subscription: "00000000-0000-0000-0000-000000000001", location: "eastus2", err: errors.New("forbidden"), want: "forbidden"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			v := NetworkValidator{SubscriptionID: tc.subscription, VirtualNetworks: &testNetworks{location: tc.location}, Subnets: &testSubnets{properties: tc.properties, err: tc.err}}
			err := v.Validate(t.Context(), testClass().Spec.Regions[0])
			if tc.want == "" {
				g.Expect(err).ToNot(HaveOccurred())
			} else {
				g.Expect(err).To(MatchError(ContainSubstring(tc.want)))
			}
		})
	}
}
