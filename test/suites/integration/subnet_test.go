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

package integration_test

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork"
	"github.com/Azure/karpenter-provider-azure/pkg/apis/v1beta1"
	"github.com/Azure/karpenter-provider-azure/pkg/consts"
	"github.com/Azure/karpenter-provider-azure/pkg/utils"
	"github.com/samber/lo"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/test"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Subnets", func() {
	var dep *appsv1.Deployment
	var selector labels.Selector
	var numPods int

	BeforeEach(func() {
		numPods = 1
		dep = test.Deployment(test.DeploymentOptions{
			Replicas: int32(numPods),
			PodOptions: test.PodOptions{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": "my-app"},
				},
			},
		})
		selector = labels.SelectorFromSet(dep.Spec.Selector.MatchLabels)
	})

	It("should allocate node in NodeClass subnet", func() {
		// This test requires creating a custom subnet in the cluster's VNet.
		// Only works with BYO VNets. Skips for managed VNets since we block custom subnets there.
		clusterSubnet := env.GetClusterSubnet()
		isManaged, err := utils.IsAKSManagedVNET(env.NodeResourceGroup, lo.FromPtr(clusterSubnet.ID))
		Expect(err).ToNot(HaveOccurred())

		if isManaged {
			Skip("Skipping test: cluster uses managed VNet, custom subnets in managed VNets are blocked")
		}

		subnetName := "test-subnet"
		subnet := &armnetwork.Subnet{
			Name: lo.ToPtr(subnetName),
			Properties: &armnetwork.SubnetPropertiesFormat{
				AddressPrefix: lo.ToPtr("10.225.0.0/16"),
			},
		}

		// Use existing cluster VNet (which is BYO)
		vnet := env.GetClusterVNET()
		vnetName := lo.FromPtr(vnet.Name)

		env.ExpectCreatedSubnet(vnetName, subnet)
		nodeClass.Spec.VNETSubnetID = subnet.ID // Should be populated by the Expect call above

		env.ExpectCreated(nodeClass, nodePool, dep)

		env.EventuallyExpectCreatedNodeClaimCount("==", 1)
		nodes := env.EventuallyExpectCreatedNodeCount("==", 1)
		env.EventuallyExpectHealthyPodCount(selector, numPods)

		// The NIC should have the right subnet
		nic := env.GetNodeNetworkInterface(nodes[0].Name)
		Expect(nic.Properties).ToNot(BeNil())
		Expect(nic.Properties.IPConfigurations).To(HaveLen(1))
		Expect(nic.Properties.IPConfigurations[0].Properties).ToNot(BeNil())
		Expect(nic.Properties.IPConfigurations[0].Properties.Subnet).ToNot(BeNil())
		Expect(nic.Properties.IPConfigurations[0].Properties.Subnet.ID).To(Equal(subnet.ID))

		// The NIC should have the right NSG
		Expect(nic.Properties.NetworkSecurityGroup).ToNot(BeNil())
		Expect(nic.Properties.NetworkSecurityGroup.ID).ToNot(BeNil())
	})
})

var _ = Describe("Multi-region subnets", func() {
	It("should launch one NodePool in two physical regions, connect pods, and delete both nodes", func() {
		raw := os.Getenv("MULTI_REGION_TEST_REGIONS")
		if raw == "" {
			Skip("requires MULTI_REGION_TEST_REGIONS, a dedicated cross-region BYO-CNI cluster, and a controller started with ENABLE_MULTI_REGION=true")
		}
		Expect(env.ProvisionMode).To(Equal(consts.ProvisionModeAKSScriptless))
		var regions []v1beta1.Region
		Expect(json.Unmarshal([]byte(raw), &regions)).To(Succeed())
		Expect(regions).To(HaveLen(2))
		Expect(regions[0].Name).ToNot(Equal(regions[1].Name))
		nodeClass.Spec.VNETSubnetID = nil
		nodeClass.Spec.Regions = regions
		nodePool.Spec.Disruption.ConsolidateAfter = karpv1.MustParseNillableDuration("Never")
		env.ExpectCreated(nodeClass, nodePool)

		servers := make([]*corev1.Pod, 0, 2)
		for _, region := range regions {
			pod := test.Pod(test.PodOptions{
				NodeSelector: map[string]string{
					corev1.LabelTopologyRegion: region.Name,
					karpv1.NodePoolLabelKey:    nodePool.Name,
				},
				Image: "mcr.microsoft.com/azurelinux/busybox:1.36",
			})
			pod.Spec.Containers[0].Command = []string{"sh", "-c", "mkdir -p /tmp/www; echo cross-region-ok > /tmp/www/index.html; exec httpd -f -p 8080 -h /tmp/www"}
			pod.Spec.Containers[0].ReadinessProbe = &corev1.Probe{ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{Path: "/", Port: intstr.FromInt32(8080)},
			}}
			env.ExpectCreated(pod)
			servers = append(servers, pod)
		}
		env.EventuallyExpectHealthy(servers...)
		nodes := env.EventuallyExpectInitializedNodeCount("==", 2)
		claims := env.EventuallyExpectRegisteredNodeClaimCount("==", 2)
		for _, node := range nodes {
			vm := env.GetVM(node.Name)
			Expect(strings.ToLower(lo.FromPtr(vm.Location))).To(Equal(node.Labels[corev1.LabelTopologyRegion]), "verify ARM location, not just Kubernetes labels")
			Expect(node.Labels[corev1.LabelNodeExcludeBalancers]).To(Equal("true"))
			target, ok := lo.Find(regions, func(r v1beta1.Region) bool { return strings.EqualFold(r.Name, lo.FromPtr(vm.Location)) })
			Expect(ok).To(BeTrue())
			nic := env.GetNodeNetworkInterface(node.Name)
			Expect(strings.ToLower(lo.FromPtr(nic.Location))).To(Equal(target.Name))
			for _, ip := range nic.Properties.IPConfigurations {
				Expect(strings.ToLower(lo.FromPtr(ip.Properties.Subnet.ID))).To(Equal(strings.ToLower(target.VNETSubnetID)))
				Expect(ip.Properties.LoadBalancerBackendAddressPools).To(BeEmpty())
			}
		}
		// Probe both directions from the actual application nodes, including DNS
		// through the cluster Service network. A Ready node alone cannot prove CNI.
		for i, source := range servers {
			target := servers[1-i]
			Expect(env.Client.Get(env, client.ObjectKeyFromObject(source), source)).To(Succeed())
			Expect(env.Client.Get(env, client.ObjectKeyFromObject(target), target)).To(Succeed())
			probe := test.Pod(test.PodOptions{Image: "mcr.microsoft.com/azurelinux/busybox:1.36"})
			probe.Spec.NodeName = source.Spec.NodeName
			probe.Spec.RestartPolicy = corev1.RestartPolicyNever
			probe.Spec.Containers[0].Command = []string{"sh", "-ec",
				fmt.Sprintf("nslookup kubernetes.default.svc.cluster.local; wget -T 20 -qO- http://%s:8080 | grep cross-region-ok", target.Status.PodIP)}
			env.ExpectCreated(probe)
			Eventually(func(g Gomega) {
				g.Expect(env.Client.Get(env, client.ObjectKeyFromObject(probe), probe)).To(Succeed())
				g.Expect(probe.Status.Phase).To(Equal(corev1.PodSucceeded))
			}).WithTimeout(2 * time.Minute).Should(Succeed())
			env.ExpectDeleted(probe)
		}
		for _, server := range servers {
			env.ExpectDeleted(server)
		}
		for _, claim := range claims {
			env.ExpectDeleted(claim)
		}
		for _, claim := range claims {
			env.EventuallyExpectNotFound(claim)
		}
		for _, node := range nodes {
			env.EventuallyExpectNotFound(node)
		}
		env.EventuallyExpectKarpenterNicsToBeDeleted()
	})
})
