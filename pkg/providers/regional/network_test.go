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
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork"
	"github.com/Azure/karpenter-provider-azure/pkg/providers/azclient/azapi"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	clocktesting "k8s.io/utils/clock/testing"
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
	calls      int
}

func (p *testSubnets) Get(context.Context, string, string, string, *armnetwork.SubnetsClientGetOptions) (armnetwork.SubnetsClientGetResponse, error) {
	p.calls++
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

func TestRegionalNetworkValidationIsReusedBriefly(t *testing.T) {
	g := NewWithT(t)
	clk := clocktesting.NewFakeClock(time.Now())
	subnets := &testSubnets{properties: &armnetwork.SubnetPropertiesFormat{NetworkSecurityGroup: &armnetwork.SecurityGroup{ID: lo.ToPtr("/synthetic/nsg")}}}
	v := &NetworkValidator{SubscriptionID: "00000000-0000-0000-0000-000000000001", VirtualNetworks: &testNetworks{location: "eastus2"}, Subnets: subnets, Clock: clk}
	region := testClass().Spec.Regions[0]
	for range 3 {
		g.Expect(v.Validate(t.Context(), region)).To(Succeed())
	}
	g.Expect(subnets.calls).To(Equal(1), "launch bursts share one successful check")
	clk.Step(validationTTL)
	subnets.properties = &armnetwork.SubnetPropertiesFormat{}
	for range 2 {
		g.Expect(v.Validate(t.Context(), region)).To(MatchError(ContainSubstring("network security group")))
	}
	g.Expect(subnets.calls).To(Equal(3), "expired successes are re-checked and failures are never cached")
}

func TestRegionalNetworkValidationAcceptsARecreatedNetwork(t *testing.T) {
	g := NewWithT(t)
	clk := clocktesting.NewFakeClock(time.Now())
	networks := &testNetworks{location: "eastus2"}
	subnets := &testSubnets{properties: &armnetwork.SubnetPropertiesFormat{NetworkSecurityGroup: &armnetwork.SecurityGroup{ID: lo.ToPtr("/synthetic/nsg")}}}
	v := &NetworkValidator{SubscriptionID: "00000000-0000-0000-0000-000000000001", VirtualNetworks: networks, Subnets: subnets, Clock: clk}
	region := testClass().Spec.Regions[0]
	g.Expect(v.Validate(t.Context(), region)).To(Succeed())
	// The region's network is deleted, and the cached success expires.
	clk.Step(validationTTL)
	networks.err = errors.New("synthetic ResourceGroupNotFound")
	g.Expect(v.Validate(t.Context(), region)).To(MatchError(ContainSubstring("getting regional VNet")))
	// The network is created again with the same IDs. The next check passes, with no restart.
	networks.err = nil
	g.Expect(v.Validate(t.Context(), region)).To(Succeed())
	g.Expect(subnets.calls).To(Equal(2), "the failed check was not cached")
}
