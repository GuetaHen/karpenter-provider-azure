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

package multiregion_test

import (
	"fmt"
	"strings"
	"time"

	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/test"

	"github.com/Azure/karpenter-provider-azure/pkg/apis/v1beta1"
	"github.com/Azure/karpenter-provider-azure/pkg/consts"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("MultiRegion", func() {
	BeforeEach(func() {
		Expect(env.ProvisionMode).To(Equal(consts.ProvisionModeAKSScriptless), "spec.regions requires aksscriptless provisioning")
		subnets := lo.Map(regions, func(region v1beta1.Region, _ int) string { return region.VNETSubnetID })
		env.ExpectVNETsPeered(append(subnets, lo.FromPtr(env.GetClusterSubnet().ID))...)
		nodeClass.Spec.VNETSubnetID = nil
		nodeClass.Spec.Regions = regions
		nodePool.Spec.Disruption.ConsolidateAfter = karpv1.MustParseNillableDuration("Never")
	})

	It("should launch one NodePool in every listed region, connect the pods of every two regions, and delete the nodes", func() {
		env.ExpectCreated(nodeClass, nodePool)
		servers := make([]*corev1.Pod, 0, len(regions))
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
		nodes := env.EventuallyExpectInitializedNodeCount("==", len(regions))
		claims := env.EventuallyExpectRegisteredNodeClaimCount("==", len(regions))
		for _, node := range nodes {
			vm := env.GetVM(node.Name)
			location := strings.ToLower(lo.FromPtr(vm.Location))
			Expect(location).To(Equal(node.Labels[corev1.LabelTopologyRegion]), "the region label must match the VM's ARM location")
			Expect(node.Labels[corev1.LabelNodeExcludeBalancers]).To(Equal("true"))
			target, ok := lo.Find(regions, func(r v1beta1.Region) bool { return strings.EqualFold(r.Name, location) })
			Expect(ok).To(BeTrue(), "node %s is in %s, which the AKSNodeClass does not list", node.Name, location)
			nic := env.GetNodeNetworkInterface(node.Name)
			Expect(strings.ToLower(lo.FromPtr(nic.Location))).To(Equal(target.Name))
			Expect(lo.FromPtr(nic.Properties.EnableIPForwarding)).To(BeFalse(), "NICs in other regions keep IP forwarding off")
			Expect(nic.Properties.NetworkSecurityGroup).To(BeNil())
			for _, ip := range nic.Properties.IPConfigurations {
				Expect(strings.ToLower(lo.FromPtr(ip.Properties.Subnet.ID))).To(Equal(strings.ToLower(target.VNETSubnetID)))
				Expect(ip.Properties.LoadBalancerBackendAddressPools).To(BeEmpty())
			}
		}
		// Probe every ordered pair of regions from the server's node, with cluster DNS.
		// A Ready node alone cannot prove the pod network.
		for i, source := range servers {
			Expect(env.Client.Get(env, client.ObjectKeyFromObject(source), source)).To(Succeed())
			for j, target := range servers {
				if i == j {
					continue
				}
				Expect(env.Client.Get(env, client.ObjectKeyFromObject(target), target)).To(Succeed())
				probe := test.Pod(test.PodOptions{Image: "mcr.microsoft.com/azurelinux/busybox:1.36"})
				probe.Spec.NodeName = source.Spec.NodeName
				probe.Spec.RestartPolicy = corev1.RestartPolicyNever
				probe.Spec.Containers[0].Command = []string{"sh", "-ec",
					fmt.Sprintf("nslookup kubernetes.default.svc.cluster.local; wget -T 20 -qO- http://%s:8080 | grep cross-region-ok", target.Status.PodIP)}
				env.ExpectCreated(probe)
				Eventually(func(g Gomega) {
					g.Expect(env.Client.Get(env, client.ObjectKeyFromObject(probe), probe)).To(Succeed())
					g.Expect(probe.Status.Phase).To(Equal(corev1.PodSucceeded), "from %s to %s", regions[i].Name, regions[j].Name)
				}).WithTimeout(2 * time.Minute).Should(Succeed())
				env.ExpectDeleted(probe)
			}
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
