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

package podnetwork

import (
	"encoding/json"
	"net/netip"
	"strings"
	"testing"

	"github.com/Azure/karpenter-provider-azure/pkg/apis/v1beta1"
	"github.com/Azure/karpenter-provider-azure/pkg/providers/regional"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

const (
	testGroup           = "multiregion.io"
	testResourceGroupID = "/subscriptions/00000000-0000-0000-0000-000000000001/resourceGroups/cluster-nodes"
	testSubnetID        = "/subscriptions/00000000-0000-0000-0000-000000000001/resourceGroups/network/providers/Microsoft.Network/virtualNetworks/nodes/subnets/nodes"
)

func testConfig(t *testing.T) *Config {
	t.Helper()
	c := &Config{PodCIDR: netip.MustParsePrefix("10.244.0.0/16"), NodeResourceGroupID: testResourceGroupID, NodeClassGroup: testGroup,
		TransitPrefix: netip.MustParsePrefix(DefaultTransitPrefix)}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	return c
}

// cluster holds the objects that Evaluate reads.
type cluster struct {
	nodes   []corev1.Node
	configs []unstructured.Unstructured
	claims  []karpv1.NodeClaim
}

// add adds a Node with its NodeNetworkConfig. A node with a region is a registered regional node
// with its NodeClaim, placed the way the controller places it; a node without one is a home node.
func (cl *cluster) add(name, ip, podRange, region string) {
	node := corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID(name + "-uid"), Labels: map[string]string{
			corev1.LabelOSStable: "linux", corev1.LabelTopologyRegion: "eastus2"}},
		Spec: corev1.NodeSpec{ProviderID: "azure:///subscriptions/00000000-0000-0000-0000-000000000001/resourceGroups/CLUSTER-NODES/providers/Microsoft.Compute/virtualMachines/" + name},
		Status: corev1.NodeStatus{
			Addresses: []corev1.NodeAddress{{Type: corev1.NodeHostName, Address: name}, {Type: corev1.NodeInternalIP, Address: ip}},
			NodeInfo:  corev1.NodeSystemInfo{SystemUUID: "VM-" + strings.ToUpper(name)},
		},
	}
	if region != "" {
		raw, _ := json.Marshal(v1beta1.Region{Name: region, VNETSubnetID: testSubnetID})
		node.Labels[corev1.LabelTopologyRegion] = region
		node.Labels[karpv1.NodeRegisteredLabelKey] = "true"
		node.Annotations = map[string]string{regional.PlacementAnnotation: string(raw)}
		cl.claims = append(cl.claims, claimFor(&node, testGroup, string(raw)))
	}
	cl.nodes = append(cl.nodes, node)
	config := unstructured.Unstructured{Object: map[string]any{"apiVersion": "acn.azure.com/v1alpha", "kind": "NodeNetworkConfig"}}
	config.SetNamespace("kube-system")
	config.SetName(name)
	config.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "v1", Kind: "Node", Name: name, UID: node.UID}})
	setContainers(&config, container(podRange, ip, "overlay"))
	cl.configs = append(cl.configs, config)
}

// claimFor returns a NodeClaim with the provider ID of node and a node class of group, which carries
// the placement annotation unless placement is empty.
func claimFor(node *corev1.Node, group, placement string) karpv1.NodeClaim {
	claim := karpv1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: node.Name},
		Spec:       karpv1.NodeClaimSpec{NodeClassRef: &karpv1.NodeClassReference{Group: group, Kind: "AKSNodeClass", Name: "default"}},
		Status:     karpv1.NodeClaimStatus{ProviderID: node.Spec.ProviderID},
	}
	if placement != "" {
		claim.Annotations = map[string]string{regional.PlacementAnnotation: placement}
	}
	return claim
}

func container(podRange, nodeIP, kind string) map[string]any {
	return map[string]any{"id": "nc-" + podRange, "type": kind, "primaryIP": podRange, "nodeIP": nodeIP, "subnetAddressSpace": "10.244.0.0/16", "version": int64(1)}
}

func setContainers(config *unstructured.Unstructured, containers ...map[string]any) {
	list := make([]any, 0, len(containers))
	for _, c := range containers {
		list = append(list, c)
	}
	config.Object["status"] = map[string]any{"networkContainers": list}
}

func TestConfigValidate(t *testing.T) {
	g := NewWithT(t)
	g.Expect(testConfig(t).nodeResourceGroup.ResourceGroupName).To(Equal("cluster-nodes"))
	for name, change := range map[string]func(*Config){
		"no pod CIDR":                           func(c *Config) { c.PodCIDR = netip.Prefix{} },
		"an IPv6 pod CIDR":                      func(c *Config) { c.PodCIDR = netip.MustParsePrefix("fd00::/48") },
		"a pod CIDR longer than /24":            func(c *Config) { c.PodCIDR = netip.MustParsePrefix("10.244.0.0/25") },
		"a pod CIDR with host bits":             func(c *Config) { c.PodCIDR = netip.MustParsePrefix("10.244.1.0/16") },
		"no resource group":                     func(c *Config) { c.NodeResourceGroupID = "" },
		"a VM instead of a group":               func(c *Config) { c.NodeResourceGroupID += "/providers/Microsoft.Compute/virtualMachines/vm" },
		"no node class group":                   func(c *Config) { c.NodeClassGroup = "" },
		"a pod CIDR in link-local space":        func(c *Config) { c.PodCIDR = netip.MustParsePrefix("169.254.0.0/16") },
		"a resource group without subscription": func(c *Config) { c.NodeResourceGroupID = "/providers/Microsoft.Resources/resourceGroups/cluster-nodes" },
	} {
		c := &Config{PodCIDR: netip.MustParsePrefix("10.244.0.0/16"), NodeResourceGroupID: testResourceGroupID, NodeClassGroup: testGroup,
			TransitPrefix: netip.MustParsePrefix(DefaultTransitPrefix)}
		change(c)
		g.Expect(c.Validate()).ToNot(Succeed(), name)
	}
}

func TestEvaluateAcceptsHomeAndRegionalNodes(t *testing.T) {
	g := NewWithT(t)
	g.Expect(PlacementAnnotation).To(Equal(regional.PlacementAnnotation), "the agent reads the annotation that the controller writes")
	var cl cluster
	cl.add("home-1", "10.0.0.4", "10.244.1.0/24", "")
	cl.add("home-2", "10.0.0.5", "10.244.2.0/24", "")
	cl.add("regional-1", "10.1.0.4", "10.244.7.0/24", "westus2")
	cl.add("regional-2", "10.2.0.4", "10.244.255.0/24", "centralus")
	// A home node may run on a scale set, an ExternalIP does not count, and provider IDs compare
	// ignoring case.
	cl.nodes[1].Spec.ProviderID = "azure:///subscriptions/00000000-0000-0000-0000-000000000001/resourceGroups/cluster-nodes/providers/Microsoft.Compute/virtualMachineScaleSets/home-vmss/virtualMachines/2"
	cl.nodes[0].Status.Addresses = append(cl.nodes[0].Status.Addresses, corev1.NodeAddress{Type: corev1.NodeExternalIP, Address: "20.0.0.4"})
	cl.claims[0].Status.ProviderID = strings.ToUpper(cl.claims[0].Status.ProviderID)
	// A NodeClaim without a placement, of either installation, leaves its node a home node.
	cl.claims = append(cl.claims, claimFor(&cl.nodes[0], testGroup, ""), claimFor(&cl.nodes[1], "karpenter.azure.com", ""))
	eval := testConfig(t).Evaluate(cl.nodes, cl.configs, cl.claims)
	g.Expect(eval.Rejected).To(BeEmpty())
	node := func(name string, role Role, region, ip, podRange string, index uint32) Node {
		return Node{Name: name, UID: types.UID(name + "-uid"), Role: role, Region: region, VMID: "vm-" + name,
			IP: netip.MustParseAddr(ip), PodRange: netip.MustParsePrefix(podRange), Index: index}
	}
	g.Expect(eval.Accepted).To(Equal(map[string]Node{
		"home-1":     node("home-1", Home, "eastus2", "10.0.0.4", "10.244.1.0/24", 1),
		"home-2":     node("home-2", Home, "eastus2", "10.0.0.5", "10.244.2.0/24", 2),
		"regional-1": node("regional-1", Regional, "westus2", "10.1.0.4", "10.244.7.0/24", 7),
		"regional-2": node("regional-2", Regional, "centralus", "10.2.0.4", "10.244.255.0/24", 255),
	}))
	g.Expect(eval.Peers(eval.Accepted["home-1"])).To(Equal([]Node{eval.Accepted["regional-1"], eval.Accepted["regional-2"]}))
	g.Expect(eval.Peers(eval.Accepted["regional-1"])).To(Equal([]Node{eval.Accepted["home-1"], eval.Accepted["home-2"], eval.Accepted["regional-2"]}))
}

func TestEvaluateRejectsNodesThatFailACheck(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(*cluster)
		reason string
	}{
		"a Windows node":     {func(cl *cluster) { cl.nodes[1].Labels[corev1.LabelOSStable] = "windows" }, "not a Linux node"},
		"no region label":    {func(cl *cluster) { delete(cl.nodes[1].Labels, corev1.LabelTopologyRegion) }, "no topology.kubernetes.io/region label"},
		"no systemUUID":      {func(cl *cluster) { cl.nodes[1].Status.NodeInfo.SystemUUID = "" }, "no systemUUID"},
		"no provider ID yet": {func(cl *cluster) { cl.nodes[1].Spec.ProviderID = "" }, "no Azure provider ID"},
		"a provider ID in another resource group": {func(cl *cluster) {
			cl.nodes[1].Spec.ProviderID = strings.Replace(cl.nodes[1].Spec.ProviderID, "CLUSTER-NODES", "other", 1)
		}, "not in the node resource group"},
		"a provider ID in another subscription": {func(cl *cluster) {
			cl.nodes[1].Spec.ProviderID = strings.Replace(cl.nodes[1].Spec.ProviderID, "-000000000001/", "-000000000002/", 1)
		}, "not in the node resource group"},
		"a provider ID of the resource group": {func(cl *cluster) {
			cl.nodes[1].Spec.ProviderID = "azure:///subscriptions/00000000-0000-0000-0000-000000000001/resourceGroups/CLUSTER-NODES"
		}, "not a VM"},
		"a provider ID of a network interface": {func(cl *cluster) {
			cl.nodes[1].Spec.ProviderID = strings.Replace(cl.nodes[1].Spec.ProviderID, "Microsoft.Compute/virtualMachines", "Microsoft.Network/networkInterfaces", 1)
		}, "not a VM"},
		"a provider ID of a scale set that names no instance": {func(cl *cluster) {
			cl.nodes[1].Spec.ProviderID = "azure:///subscriptions/00000000-0000-0000-0000-000000000001/resourceGroups/CLUSTER-NODES/providers/Microsoft.Compute/virtualMachineScaleSets/regional-vmss/virtualMachines"
		}, "names no VM"},
		"a provider ID that ends in virtualMachines": {func(cl *cluster) {
			cl.nodes[1].Spec.ProviderID = "azure:///subscriptions/00000000-0000-0000-0000-000000000001/resourceGroups/CLUSTER-NODES/providers/Microsoft.Compute/virtualMachines"
		}, "no valid Azure resource ID"},
		"two IPv4 InternalIPs": {func(cl *cluster) {
			cl.nodes[1].Status.Addresses = append(cl.nodes[1].Status.Addresses, corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: "10.1.0.5"})
		}, "2 IPv4 InternalIPs instead of one"},
		"no NodeNetworkConfig":                     {func(cl *cluster) { cl.configs = cl.configs[:1] }, "no NodeNetworkConfig"},
		"a NodeNetworkConfig in another namespace": {func(cl *cluster) { cl.configs[1].SetNamespace("default") }, "no NodeNetworkConfig"},
		"the NodeNetworkConfig of a former Node": {func(cl *cluster) {
			cl.configs[1].SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "v1", Kind: "Node", Name: "regional-1", UID: "former-uid"}})
		}, "belongs to Node regional-1 with UID former-uid"},
		"the NodeNetworkConfig of another Node with this UID": {func(cl *cluster) {
			cl.configs[1].SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "v1", Kind: "Node", Name: "regional-2", UID: "regional-1-uid"}})
		}, "belongs to Node regional-2 with UID regional-1-uid"},
		"a NodeNetworkConfig without owner": {func(cl *cluster) { cl.configs[1].SetOwnerReferences(nil) }, "has no Node owner"},
		"another node IP": {func(cl *cluster) {
			setContainers(&cl.configs[1], container("10.244.7.0/24", "10.1.0.9", "overlay"))
		}, `names node IP "10.1.0.9"`},
		"two network containers": {func(cl *cluster) {
			setContainers(&cl.configs[1], container("10.244.7.0/24", "10.1.0.4", "overlay"), container("10.244.8.0/24", "10.1.0.4", "overlay"))
		}, "exactly one network container"},
		"a network container that is not overlay": {func(cl *cluster) {
			setContainers(&cl.configs[1], container("10.244.7.0/24", "10.1.0.4", "vnet"))
		}, "exactly one network container"},
		"a range of length 25": {func(cl *cluster) {
			setContainers(&cl.configs[1], container("10.244.7.0/25", "10.1.0.4", "overlay"))
		}, "is not an IPv4 /24 inside the pod CIDR"},
		"a range with host bits": {func(cl *cluster) {
			setContainers(&cl.configs[1], container("10.244.7.1/24", "10.1.0.4", "overlay"))
		}, "is not an IPv4 /24 inside the pod CIDR"},
		"a range outside the pod CIDR": {func(cl *cluster) {
			setContainers(&cl.configs[1], container("10.245.7.0/24", "10.1.0.4", "overlay"))
		}, "is not an IPv4 /24 inside the pod CIDR"},
		"no NodeClaim":   {func(cl *cluster) { cl.claims = nil }, "0 NodeClaims instead of one"},
		"two NodeClaims": {func(cl *cluster) { cl.claims = append(cl.claims, cl.claims[0]) }, "2 NodeClaims instead of one"},
		"a NodeClaim of the other installation": {func(cl *cluster) {
			cl.claims[0].Spec.NodeClassRef.Group = "karpenter.azure.com"
		}, "does not reference a node class of group multiregion.io"},
		"a node not registered yet": {func(cl *cluster) { delete(cl.nodes[1].Labels, karpv1.NodeRegisteredLabelKey) }, "not registered yet"},
		"a placed NodeClaim whose node has no annotation yet": {func(cl *cluster) {
			cl.nodes[1].Annotations = nil
			delete(cl.nodes[1].Labels, karpv1.NodeRegisteredLabelKey)
		}, "not registered yet"},
		"an incomplete placement": {func(cl *cluster) {
			cl.claims[0].Annotations[regional.PlacementAnnotation] = `{"name": "westus2"}`
		}, "no valid placement annotation"},
		"a placement in another region": {func(cl *cluster) {
			cl.nodes[1].Labels[corev1.LabelTopologyRegion] = "eastus"
		}, "places the node in westus2, but its region label is eastus"},
	} {
		t.Run(name, func(t *testing.T) {
			g := NewWithT(t)
			var cl cluster
			cl.add("home-1", "10.0.0.4", "10.244.1.0/24", "")
			cl.add("regional-1", "10.1.0.4", "10.244.7.0/24", "westus2")
			tc.change(&cl)
			eval := testConfig(t).Evaluate(cl.nodes, cl.configs, cl.claims)
			g.Expect(eval.Rejected).To(HaveKeyWithValue("regional-1", ContainSubstring(tc.reason)))
			g.Expect(eval.Accepted).To(HaveKey("home-1"))
		})
	}
}

func TestEvaluateRejectsEveryNodeOfASharedPodRange(t *testing.T) {
	g := NewWithT(t)
	var cl cluster
	cl.add("home-1", "10.0.0.4", "10.244.1.0/24", "")
	cl.add("home-2", "10.0.0.5", "10.244.1.0/24", "")
	cl.add("home-3", "10.0.0.6", "10.244.2.0/24", "")
	cl.add("home-4", "10.0.0.7", "10.244.4.0/24", "")
	// The NodeNetworkConfig of a Node that is gone still names its range.
	cl.add("gone-1", "10.0.0.8", "10.244.2.0/24", "")
	cl.nodes = cl.nodes[:4]
	eval := testConfig(t).Evaluate(cl.nodes, cl.configs, cl.claims)
	g.Expect(eval.Accepted).To(ConsistOf(HaveField("Name", "home-4")))
	g.Expect(eval.Rejected).To(Equal(map[string]string{
		"home-1": "the NodeNetworkConfigs of home-2 name the same pod range 10.244.1.0/24",
		"home-2": "the NodeNetworkConfigs of home-1 name the same pod range 10.244.1.0/24",
		"home-3": "the NodeNetworkConfigs of gone-1 name the same pod range 10.244.2.0/24",
	}))
}
