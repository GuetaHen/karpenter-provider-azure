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

package inplaceupdate_test

import (
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v7"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	coretest "sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"

	"github.com/Azure/karpenter-provider-azure/pkg/apis/coexistence"
	"github.com/Azure/karpenter-provider-azure/pkg/apis/v1beta1"
	"github.com/Azure/karpenter-provider-azure/pkg/controllers/nodeclaim/inplaceupdate"
	"github.com/Azure/karpenter-provider-azure/pkg/fake"
	"github.com/Azure/karpenter-provider-azure/pkg/operator/options"
	"github.com/Azure/karpenter-provider-azure/pkg/providers/launchtemplate"
	"github.com/Azure/karpenter-provider-azure/pkg/test"
	"github.com/Azure/karpenter-provider-azure/pkg/utils"
)

const (
	globalIdentity = "/subscriptions/00000000-0000-0000-0000-000000000001/resourceGroups/identities/providers/Microsoft.ManagedIdentity/userAssignedIdentities/global"
	regionIdentity = "/subscriptions/00000000-0000-0000-0000-000000000001/resourceGroups/identities/providers/Microsoft.ManagedIdentity/userAssignedIdentities/westus2"
)

func regionalIdentityOptions() *options.Options {
	o := test.Options()
	o.EnableMultiRegion = true
	o.NodeIdentities = []string{globalIdentity}
	o.MultiRegionNodeIdentities = map[string]options.MultiRegionNodeIdentity{
		"westus2": {NodeIdentities: []string{regionIdentity}, KubeletIdentityClientID: "11111111-2222-3333-4444-555555555555"},
	}
	return o
}

var _ = Describe("Regional node identities", func() {
	var nodeClass *v1beta1.AKSNodeClass
	var nodeClaim *karpv1.NodeClaim

	BeforeEach(func() {
		nodeClass = test.AKSNodeClass()
		nodeClaim = coretest.NodeClaim()
	})

	It("hashes a regional NodeClaim with its region's identities", func() {
		regionalOpts := regionalIdentityOptions()
		home, err := inplaceupdate.HashFromNodeClaim(regionalOpts, nodeClaim, nodeClass)
		Expect(err).ToNot(HaveOccurred())
		nodeClaim.Labels = lo.Assign(nodeClaim.Labels, map[string]string{corev1.LabelTopologyRegion: "westus2"})
		regional, err := inplaceupdate.HashFromNodeClaim(regionalOpts, nodeClaim, nodeClass)
		Expect(err).ToNot(HaveOccurred())
		Expect(regional).ToNot(Equal(home))
		regionalOpts.MultiRegionNodeIdentities = nil
		Expect(inplaceupdate.HashFromNodeClaim(regionalOpts, nodeClaim, nodeClass)).To(Equal(home), "without an entry every region uses the global identities")
	})

	It("never adds the global identities to a regional VM", func() {
		vm := &armcompute.VirtualMachine{Location: lo.ToPtr("westus2"), Tags: launchtemplate.Tags(regionalIdentityOptions(), nodeClass, nodeClaim)}
		update := inplaceupdate.CalculateVMPatch(regionalIdentityOptions(), nodeClaim, nodeClass, vm)
		Expect(update).ToNot(BeNil())
		Expect(lo.Keys(update.Identity.UserAssignedIdentities)).To(ConsistOf(regionIdentity))
		vm.Identity = &armcompute.VirtualMachineIdentity{UserAssignedIdentities: map[string]*armcompute.UserAssignedIdentitiesValue{regionIdentity: {}}}
		Expect(inplaceupdate.CalculateVMPatch(regionalIdentityOptions(), nodeClaim, nodeClass, vm)).To(BeNil())
		vm.Location = lo.ToPtr(fake.Region)
		homeUpdate := inplaceupdate.CalculateVMPatch(regionalIdentityOptions(), nodeClaim, nodeClass, vm)
		Expect(homeUpdate).ToNot(BeNil())
		Expect(lo.Keys(homeUpdate.Identity.UserAssignedIdentities)).To(ConsistOf(globalIdentity), "home VMs keep the global identities")
	})
})

// These specs run the default build, which stands in for a controller outside coexistence mode such
// as the managed one, against a NodeClaim and VM of a coexistence-mode controller.
var _ = Describe("NodeClaims of a coexistence-mode controller", func() {
	var vm *armcompute.VirtualMachine
	var nodeClaim *karpv1.NodeClaim

	BeforeEach(func() {
		ctx = options.ToContext(ctx, test.Options())
		azureEnv.Reset(ctx)
		tags := map[string]*string{
			launchtemplate.KarpenterManagedTagKey:    lo.ToPtr(opts.ClusterName),
			launchtemplate.CoexistenceNodePoolTagKey: lo.ToPtr("coexisting-pool"),
			launchtemplate.OwnerTagKey:               lo.ToPtr(coexistence.Group),
		}
		vm = &armcompute.VirtualMachine{
			ID:   lo.ToPtr(fake.MkVMID(azureEnv.AzureResourceGraphAPI.ResourceGroup, "aks-coexisting")),
			Name: lo.ToPtr("aks-coexisting"),
			Tags: tags,
		}
		azureEnv.VirtualMachinesAPI.Instances.Store(lo.FromPtr(vm.ID), *vm)
		nic := &armnetwork.Interface{
			ID:   lo.ToPtr(fake.MakeNetworkInterfaceID(azureEnv.AzureResourceGraphAPI.ResourceGroup, "aks-coexisting")),
			Name: lo.ToPtr("aks-coexisting"),
			Tags: tags,
		}
		azureEnv.NetworkInterfacesAPI.NetworkInterfaces.Store(lo.FromPtr(nic.ID), *nic)
		for _, name := range []string{"computeAksLinuxBilling", "cse-agent-karpenter"} {
			ext := &armcompute.VirtualMachineExtension{
				ID:   lo.ToPtr(fake.MakeVMExtensionID(azureEnv.AzureResourceGraphAPI.ResourceGroup, "aks-coexisting", name)),
				Name: lo.ToPtr(name),
				Tags: tags,
			}
			azureEnv.VirtualMachineExtensionsAPI.Extensions.Store(lo.FromPtr(ext.ID), *ext)
		}
		nodeClaim = coretest.NodeClaim(karpv1.NodeClaim{
			Spec:   karpv1.NodeClaimSpec{NodeClassRef: &karpv1.NodeClassReference{Group: coexistence.Group, Kind: v1beta1.AKSNodeClassKind, Name: "coexisting"}},
			Status: karpv1.NodeClaimStatus{ProviderID: utils.VMResourceIDToProviderID(ctx, *vm.ID)},
		})
		nodeClaim.StatusConditions().SetTrue(karpv1.ConditionTypeLaunched)
		nodeClaim.StatusConditions().SetTrue(karpv1.ConditionTypeRegistered)
	})

	AfterEach(func() {
		ExpectCleanedUp(ctx, env.Client)
	})

	It("are left alone while no default-group node class has their class name", func() {
		ExpectApplied(ctx, env.Client, nodeClaim)
		_, err := inPlaceUpdateController.Reconcile(ctx, nodeClaim)
		Expect(err).To(MatchError(ContainSubstring("resolving AKSNodeClass")))
		Expect(azureEnv.VirtualMachinesAPI.VirtualMachineUpdateBehavior.Calls()).To(BeZero())
		Expect(ExpectExists(ctx, env.Client, nodeClaim).Annotations).ToNot(HaveKey(v1beta1.AnnotationInPlaceUpdateHash))
	})

	It("are rewritten when a default-group node class shares their class name, so names must differ", func() {
		nodeClass := test.AKSNodeClass()
		nodeClass.Name = "coexisting"
		// ExpectCleanedUp keeps AKSNodeClasses, and the other spec must not resolve this one.
		DeferCleanup(func() { ExpectDeleted(ctx, env.Client, nodeClass) })
		ExpectApplied(ctx, env.Client, nodeClass, nodeClaim)
		ExpectObjectReconciled(ctx, env.Client, inPlaceUpdateController, nodeClaim)
		Expect(azureEnv.VirtualMachinesAPI.VirtualMachineUpdateBehavior.Calls()).To(Equal(1))
		updated, err := azureEnv.VMInstanceProvider.Get(ctx, "aks-coexisting")
		Expect(err).ToNot(HaveOccurred())
		Expect(updated.Tags).ToNot(HaveKey(launchtemplate.OwnerTagKey), "the update replaces the tags and drops the owner tag")
	})
})
