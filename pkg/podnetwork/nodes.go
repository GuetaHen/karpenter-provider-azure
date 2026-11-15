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
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

// PlacementAnnotation holds the region and subnet of a regional node. The controller writes it on
// the NodeClaim before launch, and Karpenter copies it to the Node at registration.
const PlacementAnnotation = "karpenter.azure.com/region-placement"

// NetworkConfigNamespace holds the NodeNetworkConfigs of Azure CNI Overlay, named after their Nodes.
const NetworkConfigNamespace = "kube-system"

// NetworkConfigs is the resource of the NodeNetworkConfigs, which name the pod range of every node.
var NetworkConfigs = schema.GroupVersionResource{Group: "acn.azure.com", Version: "v1alpha", Resource: "nodenetworkconfigs"}

// Role tells how the agent connects the pods of a node.
type Role string

const (
	// Home nodes are the ordinary nodes of the cluster, whose pods Azure CNI Overlay connects.
	Home Role = "home"
	// Regional nodes belong to an AKSNodeClass with spec.regions. The agent connects their pods
	// with the pods of every other node.
	Regional Role = "regional"
)

// Node is a node that passed every check, with the values that identify it.
type Node struct {
	Name string
	UID  types.UID
	Role Role
	// Region is the node's region label. For a regional node it is also its placement region.
	Region string
	// VMID is the node's systemUUID in lower case.
	VMID string
	// IP is the node's IPv4 InternalIP.
	IP netip.Addr
	// PodRange is the /24 that the node's NodeNetworkConfig names.
	PodRange netip.Prefix
	// Index is the position of PodRange among the /24s of the pod CIDR, counting from 0.
	Index uint32
}

// Evaluation is the result of checking every node once.
type Evaluation struct {
	// Accepted holds by name the nodes that passed every check.
	Accepted map[string]Node
	// Rejected holds by name why each other node did not.
	Rejected map[string]string
}

// Evaluate checks every node against its NodeNetworkConfig and the NodeClaims with its provider
// ID, which it compares ignoring case. A node is also rejected while another NodeNetworkConfig
// names its pod range, if that one is well-formed: its only network container is of type overlay
// and names a /24 of the pod CIDR. A malformed NodeNetworkConfig gives its own node no pod range
// and rejects no other node.
func (c *Config) Evaluate(nodes []corev1.Node, networkConfigs []unstructured.Unstructured, nodeClaims []karpv1.NodeClaim) Evaluation {
	configs, rangeOwners := c.indexNetworkConfigs(networkConfigs)
	claims := map[string][]*karpv1.NodeClaim{}
	for i := range nodeClaims {
		if id := strings.ToLower(nodeClaims[i].Status.ProviderID); id != "" {
			claims[id] = append(claims[id], &nodeClaims[i])
		}
	}
	eval := Evaluation{Accepted: map[string]Node{}, Rejected: map[string]string{}}
	for i := range nodes {
		node := &nodes[i]
		n, err := c.check(node, configs[node.Name], claims[strings.ToLower(node.Spec.ProviderID)])
		if err == nil {
			err = sharedRange(n, rangeOwners[n.PodRange])
		}
		if err != nil {
			eval.Rejected[node.Name] = err.Error()
			continue
		}
		eval.Accepted[node.Name] = n
	}
	return eval
}

// Peers returns, sorted by name, the accepted nodes that the agent connects with own: every other
// node for a regional node, and the regional nodes for a home node.
func (e Evaluation) Peers(own Node) []Node {
	peers := make([]Node, 0, len(e.Accepted))
	for name, n := range e.Accepted {
		if name != own.Name && (own.Role == Regional || n.Role == Regional) {
			peers = append(peers, n)
		}
	}
	slices.SortFunc(peers, func(a, b Node) int { return strings.Compare(a.Name, b.Name) })
	return peers
}

// indexNetworkConfigs maps the NodeNetworkConfigs by name, and each well-formed pod range to the
// NodeNetworkConfigs that name it.
func (c *Config) indexNetworkConfigs(networkConfigs []unstructured.Unstructured) (map[string]*unstructured.Unstructured, map[netip.Prefix][]string) {
	configs := map[string]*unstructured.Unstructured{}
	owners := map[netip.Prefix][]string{}
	for i := range networkConfigs {
		config := &networkConfigs[i]
		if config.GetNamespace() != NetworkConfigNamespace {
			continue
		}
		configs[config.GetName()] = config
		if container, err := overlayContainer(config); err == nil {
			if podRange, _, err := c.podRange(container.PrimaryIP); err == nil {
				owners[podRange] = append(owners[podRange], config.GetName())
			}
		}
	}
	return configs, owners
}

// sharedRange names the other NodeNetworkConfigs that name the pod range of n.
func sharedRange(n Node, owners []string) error {
	others := slices.DeleteFunc(slices.Clone(owners), func(name string) bool { return name == n.Name })
	if len(others) == 0 {
		return nil
	}
	slices.Sort(others)
	return fmt.Errorf("the NodeNetworkConfigs of %s name the same pod range %s", strings.Join(others, ", "), n.PodRange)
}

// check returns the node with its pod range and role, or why the agent does not accept it.
func (c *Config) check(node *corev1.Node, config *unstructured.Unstructured, claims []*karpv1.NodeClaim) (Node, error) {
	n, err := c.identity(node)
	if err != nil {
		return n, err
	}
	if n.PodRange, n.Index, err = c.networkConfigRange(config, node, n.IP); err != nil {
		return n, err
	}
	n.Role, err = c.role(node, claims, n.Region)
	return n, err
}

// identity reads the values that identify a node, apart from its pod range.
func (c *Config) identity(node *corev1.Node) (Node, error) {
	n := Node{
		Name:   node.Name,
		UID:    node.UID,
		Region: strings.ToLower(node.Labels[corev1.LabelTopologyRegion]),
		VMID:   strings.ToLower(node.Status.NodeInfo.SystemUUID),
	}
	switch {
	case node.Labels[corev1.LabelOSStable] != "linux":
		return n, errors.New("not a Linux node")
	case n.Region == "":
		return n, fmt.Errorf("no %s label", corev1.LabelTopologyRegion)
	case n.VMID == "":
		return n, errors.New("no systemUUID")
	}
	if err := c.checkProviderID(node.Spec.ProviderID); err != nil {
		return n, err
	}
	ip, err := internalIPv4(node)
	n.IP = ip
	return n, err
}

// checkProviderID requires an Azure provider ID that names a VM, standalone or of a scale set, in the
// node resource group.
func (c *Config) checkProviderID(providerID string) error {
	raw, ok := strings.CutPrefix(providerID, "azure://")
	if !ok {
		return errors.New("no Azure provider ID")
	}
	id, err := arm.ParseResourceID(raw)
	if err != nil || !strings.EqualFold(id.SubscriptionID, c.nodeResourceGroup.SubscriptionID) ||
		!strings.EqualFold(id.ResourceGroupName, c.nodeResourceGroup.ResourceGroupName) {
		return errors.New("the provider ID is not in the node resource group")
	}
	if t := id.ResourceType.String(); !strings.EqualFold(t, "Microsoft.Compute/virtualMachines") &&
		!strings.EqualFold(t, "Microsoft.Compute/virtualMachineScaleSets/virtualMachines") {
		return fmt.Errorf("the provider ID names a resource of type %s, not a VM", t)
	}
	return nil
}

// internalIPv4 returns the only IPv4 InternalIP of a node.
func internalIPv4(node *corev1.Node) (netip.Addr, error) {
	var ip netip.Addr
	found := 0
	for _, address := range node.Status.Addresses {
		if parsed, err := netip.ParseAddr(address.Address); err == nil && address.Type == corev1.NodeInternalIP && parsed.Is4() {
			ip, found = parsed, found+1
		}
	}
	if found != 1 {
		return netip.Addr{}, fmt.Errorf("%d IPv4 InternalIPs instead of one", found)
	}
	return ip, nil
}

// networkConfigRange returns the pod range that a node's NodeNetworkConfig names, with its index.
// The NodeNetworkConfig must belong to the Node's current UID and hold exactly one network
// container, of type overlay, whose nodeIP is the node's InternalIP.
func (c *Config) networkConfigRange(config *unstructured.Unstructured, node *corev1.Node, ip netip.Addr) (netip.Prefix, uint32, error) {
	if config == nil {
		return netip.Prefix{}, 0, errors.New("no NodeNetworkConfig")
	}
	if err := ownedBy(config, node); err != nil {
		return netip.Prefix{}, 0, err
	}
	container, err := overlayContainer(config)
	if err != nil {
		return netip.Prefix{}, 0, err
	}
	if nodeIP, err := netip.ParseAddr(container.NodeIP); err != nil || nodeIP != ip {
		return netip.Prefix{}, 0, fmt.Errorf("the NodeNetworkConfig names node IP %q, not the InternalIP %s", container.NodeIP, ip)
	}
	return c.podRange(container.PrimaryIP)
}

// ownedBy requires a NodeNetworkConfig to belong to node, and to no other Node.
func ownedBy(config *unstructured.Unstructured, node *corev1.Node) error {
	owned := false
	for _, ref := range config.GetOwnerReferences() {
		if ref.Kind != "Node" {
			continue
		}
		if ref.APIVersion != "v1" || ref.Name != node.Name || ref.UID != node.UID {
			return fmt.Errorf("the NodeNetworkConfig belongs to Node %s with UID %s", ref.Name, ref.UID)
		}
		owned = true
	}
	if !owned {
		return errors.New("the NodeNetworkConfig has no Node owner")
	}
	return nil
}

// networkContainer holds the fields of a NodeNetworkConfig network container that the agent reads.
type networkContainer struct {
	Type      string `json:"type"`
	PrimaryIP string `json:"primaryIP"`
	NodeIP    string `json:"nodeIP"`
}

// overlayContainer returns the only network container of a NodeNetworkConfig, which must be of
// type overlay.
func overlayContainer(config *unstructured.Unstructured) (networkContainer, error) {
	var status struct {
		NetworkContainers []networkContainer `json:"networkContainers"`
	}
	raw, found, err := unstructured.NestedMap(config.Object, "status")
	if err == nil && found {
		err = runtime.DefaultUnstructuredConverter.FromUnstructured(raw, &status)
	}
	if err != nil {
		return networkContainer{}, fmt.Errorf("reading the NodeNetworkConfig status: %w", err)
	}
	if len(status.NetworkContainers) != 1 || status.NetworkContainers[0].Type != "overlay" {
		return networkContainer{}, errors.New("the NodeNetworkConfig does not hold exactly one network container, of type overlay")
	}
	return status.NetworkContainers[0], nil
}

// podRange parses a pod range, which must be an IPv4 /24 inside the pod CIDR, and returns its
// index among the /24s of the pod CIDR.
func (c *Config) podRange(primaryIP string) (netip.Prefix, uint32, error) {
	p, err := netip.ParsePrefix(primaryIP)
	if err != nil || !p.Addr().Is4() || p.Bits() != 24 || p.Masked() != p || !c.PodCIDR.Contains(p.Addr()) {
		return netip.Prefix{}, 0, fmt.Errorf("the pod range %q is not an IPv4 /24 inside the pod CIDR %s", primaryIP, c.PodCIDR)
	}
	addr, base := p.Addr().As4(), c.PodCIDR.Addr().As4()
	return p, (binary.BigEndian.Uint32(addr[:]) - binary.BigEndian.Uint32(base[:])) >> 8, nil
}

// role tells a home node from a regional one. A node is regional when it, or a NodeClaim with its
// provider ID, carries the placement annotation, so a node without the annotation counts as home
// until a NodeClaim carries its provider ID. The agent accepts a regional node once Karpenter has
// registered it and exactly one NodeClaim, of the configured node class group, has its provider ID
// and a placement in the node's region. The region label of a node names the location of its VM;
// the agent checks the location of its own VM against instance metadata.
func (c *Config) role(node *corev1.Node, claims []*karpv1.NodeClaim, region string) (Role, error) {
	_, annotated := node.Annotations[PlacementAnnotation]
	if !annotated && !slices.ContainsFunc(claims, placed) {
		return Home, nil
	}
	switch {
	case len(claims) != 1:
		return "", fmt.Errorf("%d NodeClaims instead of one have the provider ID of this regional node", len(claims))
	case claims[0].Spec.NodeClassRef == nil || claims[0].Spec.NodeClassRef.Group != c.NodeClassGroup:
		return "", fmt.Errorf("the NodeClaim of this regional node does not reference a node class of group %s", c.NodeClassGroup)
	case !annotated || node.Labels[karpv1.NodeRegisteredLabelKey] != "true":
		return "", errors.New("the regional node is not registered yet")
	}
	placement, err := decodePlacement(claims[0].Annotations[PlacementAnnotation])
	if err != nil {
		return "", err
	}
	if !strings.EqualFold(placement.Name, region) {
		return "", fmt.Errorf("the NodeClaim places the node in %s, but its region label is %s", placement.Name, region)
	}
	return Regional, nil
}

// placed reports whether a NodeClaim carries the placement annotation.
func placed(claim *karpv1.NodeClaim) bool {
	_, ok := claim.Annotations[PlacementAnnotation]
	return ok
}

// regionalPlacement is the part of the placement annotation that the agent reads. The controller
// writes the annotation as the JSON of an AKSNodeClass region.
type regionalPlacement struct {
	Name         string `json:"name"`
	VNETSubnetID string `json:"vnetSubnetID"`
}

func decodePlacement(raw string) (regionalPlacement, error) {
	var p regionalPlacement
	if err := json.Unmarshal([]byte(raw), &p); err != nil || p.Name == "" || p.VNETSubnetID == "" {
		return p, errors.New("the NodeClaim of this regional node has no valid placement annotation")
	}
	return p, nil
}
