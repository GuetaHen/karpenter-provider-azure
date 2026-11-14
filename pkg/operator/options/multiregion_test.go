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

package options_test

import (
	"flag"
	"os"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	coreoptions "sigs.k8s.io/karpenter/pkg/operator/options"

	"github.com/Azure/karpenter-provider-azure/pkg/apis/v1beta1"
	"github.com/Azure/karpenter-provider-azure/pkg/consts"
	"github.com/Azure/karpenter-provider-azure/pkg/operator/options"
)

const regionalClientID = "11111111-2222-3333-4444-555555555555"

func identityID(name string) string {
	return "/subscriptions/00000000-0000-0000-0000-000000000001/resourceGroups/identities/providers/Microsoft.ManagedIdentity/userAssignedIdentities/" + name
}

func identitiesJSON(region, identity, clientID string) string {
	return `"` + region + `": {"nodeIdentities": ["` + identity + `"], "kubeletIdentityClientID": "` + clientID + `"}`
}

var _ = Describe("Community gallery FIPS images option", func() {
	parse := func(args ...string) *options.Options {
		fs := &coreoptions.FlagSet{FlagSet: flag.NewFlagSet("karpenter", flag.ContinueOnError)}
		opts := &options.Options{}
		opts.AddFlags(fs)
		Expect(fs.Parse(args)).To(Succeed())
		return opts
	}

	It("defaults to false and is set by its flag or COMMUNITY_GALLERY_FIPS_IMAGES", func() {
		Expect(parse().CommunityGalleryFIPSImages).To(BeFalse())
		Expect(parse("--community-gallery-fips-images").CommunityGalleryFIPSImages).To(BeTrue())
		Expect(os.Setenv("COMMUNITY_GALLERY_FIPS_IMAGES", "true")).To(Succeed())
		DeferCleanup(os.Unsetenv, "COMMUNITY_GALLERY_FIPS_IMAGES")
		Expect(parse().CommunityGalleryFIPSImages).To(BeTrue())
	})
})

var _ = Describe("Multi-region overlay and identity options", func() {
	It("accepts Azure CNI Overlay for spec.regions only with the external pod network opt-in", func() {
		opts := &options.Options{ProvisionMode: consts.ProvisionModeAKSScriptless, NetworkPlugin: consts.NetworkPluginAzure, NetworkPluginMode: consts.NetworkPluginModeOverlay, EnableMultiRegion: true}
		nodeClass := &v1beta1.AKSNodeClass{Spec: v1beta1.AKSNodeClassSpec{ImageFamily: lo.ToPtr("Ubuntu"), Regions: []v1beta1.Region{{
			Name:         "westus2",
			VNETSubnetID: "/subscriptions/00000000-0000-0000-0000-000000000001/resourceGroups/network/providers/Microsoft.Network/virtualNetworks/nodes/subnets/nodes",
		}}}}
		Expect(opts.ValidateRegions(nodeClass)).To(MatchError(ContainSubstring("--multi-region-external-pod-network")))
		opts.MultiRegionExternalPodNetwork = true
		Expect(opts.ValidateRegions(nodeClass)).To(Succeed())
		Expect(opts.ValidateMultiRegionOptions()).To(Succeed())
		opts.EnableMultiRegion = false
		Expect(opts.ValidateMultiRegionOptions()).To(MatchError(ContainSubstring("enable-multi-region")))
		opts.EnableMultiRegion, opts.NetworkPlugin, opts.NetworkPluginMode = true, consts.NetworkPluginNone, ""
		Expect(opts.ValidateMultiRegionOptions()).To(MatchError(ContainSubstring("network-plugin-mode overlay")))
	})

	It("replaces the global identities only for regions with an entry", func() {
		opts := &options.Options{EnableMultiRegion: true, NodeIdentities: []string{identityID("global")}, KubeletIdentityClientID: "61f71907-753f-4802-a901-47361c3664f2"}
		opts.SetMultiRegionNodeIdentitiesJSON("{" + identitiesJSON("WestUS2", identityID("west"), regionalClientID) + "}")
		Expect(opts.ValidateMultiRegionOptions()).To(Succeed())
		Expect(opts.NodeIdentitiesFor("westus2")).To(Equal([]string{identityID("west")}))
		Expect(opts.KubeletIdentityClientIDFor("WESTUS2")).To(Equal(regionalClientID))
		Expect(opts.NodeIdentitiesFor("eastus")).To(Equal([]string{identityID("global")}))
		Expect(opts.KubeletIdentityClientIDFor("eastus")).To(Equal(opts.KubeletIdentityClientID))
		Expect(opts.ValidateHomeLocation("eastus")).To(Succeed())
		Expect(opts.ValidateHomeLocation("WestUS2")).To(MatchError(ContainSubstring("cluster region")))
	})

	DescribeTable("rejects malformed regional identities",
		func(raw, message string) {
			opts := &options.Options{EnableMultiRegion: true}
			opts.SetMultiRegionNodeIdentitiesJSON(raw)
			Expect(opts.ValidateMultiRegionOptions()).To(MatchError(ContainSubstring(message)))
		},
		Entry("not JSON", "westus2", "JSON object"),
		Entry("unknown field", `{"westus2": {"identities": []}}`, "JSON object"),
		Entry("duplicate region", "{"+identitiesJSON("westus2", identityID("a"), regionalClientID)+", "+identitiesJSON("WestUS2", identityID("a"), regionalClientID)+"}", "more than once"),
		Entry("invalid region", "{"+identitiesJSON("west-us", identityID("a"), regionalClientID)+"}", "invalid region"),
		Entry("no identities", `{"westus2": {"nodeIdentities": [], "kubeletIdentityClientID": "`+regionalClientID+`"}}`, "at least one identity"),
		Entry("not an identity", "{"+identitiesJSON("westus2", "/subscriptions/s/resourceGroups/r/providers/Microsoft.Compute/virtualMachines/vm", regionalClientID)+"}", "user-assigned identity resource IDs"),
		Entry("malformed client ID", "{"+identitiesJSON("westus2", identityID("a"), "client")+"}", "client ID"),
	)

	It("requires multi-region provisioning for regional identities", func() {
		opts := &options.Options{}
		opts.SetMultiRegionNodeIdentitiesJSON("{" + identitiesJSON("westus2", identityID("a"), regionalClientID) + "}")
		Expect(opts.ValidateMultiRegionOptions()).To(MatchError(ContainSubstring("requires enable-multi-region")))
	})

	It("rejects AKS machine modes in coexistence mode", func() {
		opts := &options.Options{CoexistenceMode: true, ProvisionMode: consts.ProvisionModeAKSMachineAPI}
		Expect(opts.ValidateCoexistence()).To(MatchError(ContainSubstring("COEXISTENCE_MODE")))
		opts.ProvisionMode, opts.ManageExistingAKSMachines = consts.ProvisionModeAKSScriptless, true
		Expect(opts.ValidateCoexistence()).To(HaveOccurred())
		opts.ManageExistingAKSMachines = false
		Expect(opts.ValidateCoexistence()).To(Succeed())
		Expect(options.ValidateCoexistenceFeatureGates(true, true)).To(Succeed(), "the feature gates are restricted only in coexistence mode")
	})
})
