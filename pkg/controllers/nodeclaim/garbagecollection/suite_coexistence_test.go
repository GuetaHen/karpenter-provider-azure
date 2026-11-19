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

package garbagecollection_test

import (
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v7"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	corecloudprovider "sigs.k8s.io/karpenter/pkg/cloudprovider"
	coretest "sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"

	"github.com/Azure/karpenter-provider-azure/pkg/apis/coexistence"
	"github.com/Azure/karpenter-provider-azure/pkg/consts"
	"github.com/Azure/karpenter-provider-azure/pkg/controllers/nodeclaim/garbagecollection"
	"github.com/Azure/karpenter-provider-azure/pkg/operator/options"
	"github.com/Azure/karpenter-provider-azure/pkg/providers/launchtemplate"
	"github.com/Azure/karpenter-provider-azure/pkg/test"
	"github.com/Azure/karpenter-provider-azure/pkg/utils"
)

// These specs run the default build, which stands in for a controller outside coexistence mode such
// as the managed one, against resources that a coexistence-mode controller creates in the same node
// resource group.
var _ = Describe("Resources of a coexistence-mode controller", func() {
	coexistenceTags := func() map[string]*string {
		return map[string]*string{
			launchtemplate.KarpenterManagedTagKey:    lo.ToPtr("test-cluster"),
			launchtemplate.CoexistenceNodePoolTagKey: lo.ToPtr("coexisting-pool"),
			launchtemplate.OwnerTagKey:               lo.ToPtr(coexistence.Group),
		}
	}
	DescribeTable("are invisible to the garbage collection of a default-mode controller",
		func(provisionMode string) {
			testCtx := options.ToContext(ctx, test.Options(test.OptionsFields{ProvisionMode: lo.ToPtr(provisionMode)}))
			opts := options.FromContext(testCtx)
			agentPool := test.AKSAgentPool(test.AKSAgentPoolOptions{Name: opts.AKSMachinesPoolName, ResourceGroup: opts.NodeResourceGroup, ClusterName: opts.ClusterName})
			azureEnv.AKSDataStorage.AgentPools.Store(lo.FromPtr(agentPool.ID), *agentPool)
			vm := test.VirtualMachine(test.VirtualMachineOptions{Name: "aks-coexisting", Tags: coexistenceTags(), Properties: &armcompute.VirtualMachineProperties{TimeCreated: lo.ToPtr(time.Now().Add(-10 * time.Minute))}})
			azureEnv.VirtualMachinesAPI.Instances.Store(lo.FromPtr(vm.ID), *vm)
			nic := test.Interface(test.InterfaceOptions{Name: "aks-coexisting-nic", Tags: coexistenceTags()})
			azureEnv.NetworkInterfacesAPI.NetworkInterfaces.Store(lo.FromPtr(nic.ID), *nic)

			ExpectSingletonReconciled(testCtx, garbagecollection.NewInstance(env.Client, cloudProvider, fakeClock))
			ExpectSingletonReconciled(testCtx, networkInterfaceGCController)

			_, err := azureEnv.VMInstanceProvider.Get(testCtx, "aks-coexisting")
			Expect(err).ToNot(HaveOccurred())
			_, ok := azureEnv.NetworkInterfacesAPI.NetworkInterfaces.Load(lo.FromPtr(nic.ID))
			Expect(ok).To(BeTrue())
		},
		Entry("VM provisioning", consts.ProvisionModeAKSScriptless),
		Entry("AKS Machine API provisioning", consts.ProvisionModeAKSMachineAPI),
	)
})

var _ = Describe("Remote-region VMs with multi-region provisioning disabled", func() {
	It("are deleted through their NodeClaims and garbage collected when orphaned", func() {
		testCtx := options.ToContext(ctx, test.Options())
		Expect(options.FromContext(testCtx).EnableMultiRegion).To(BeFalse())
		orphan := test.VirtualMachine(test.VirtualMachineOptions{Name: "aks-remote-orphan", Location: "westus2", Properties: &armcompute.VirtualMachineProperties{TimeCreated: lo.ToPtr(time.Now().Add(-10 * time.Minute))}})
		claimed := test.VirtualMachine(test.VirtualMachineOptions{Name: "aks-remote-claimed", Location: "westus2"})
		azureEnv.VirtualMachinesAPI.Instances.Store(lo.FromPtr(orphan.ID), *orphan)
		azureEnv.VirtualMachinesAPI.Instances.Store(lo.FromPtr(claimed.ID), *claimed)
		nodeClaim := coretest.NodeClaim(karpv1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{corev1.LabelTopologyRegion: "westus2"}},
			Status:     karpv1.NodeClaimStatus{ProviderID: utils.VMResourceIDToProviderID(testCtx, lo.FromPtr(claimed.ID))},
		})
		ExpectApplied(testCtx, env.Client, nodeClaim)

		ExpectSingletonReconciled(testCtx, garbagecollection.NewInstance(env.Client, cloudProvider, fakeClock))
		_, err := azureEnv.VMInstanceProvider.Get(testCtx, "aks-remote-orphan")
		Expect(corecloudprovider.IsNodeClaimNotFoundError(err)).To(BeTrue())
		_, err = azureEnv.VMInstanceProvider.Get(testCtx, "aks-remote-claimed")
		Expect(err).ToNot(HaveOccurred(), "a VM with a NodeClaim is not garbage collected")

		Expect(cloudProvider.Delete(testCtx, nodeClaim)).To(Succeed())
		_, err = azureEnv.VMInstanceProvider.Get(testCtx, "aks-remote-claimed")
		Expect(corecloudprovider.IsNodeClaimNotFoundError(err)).To(BeTrue())
	})
})
