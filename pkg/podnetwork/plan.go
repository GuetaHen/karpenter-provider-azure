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
	"net"
	"net/netip"
)

// Plan is the tunnel state that the agent programs on its own node.
type Plan struct {
	// Regional is set on a regional node, where the agent also masquerades pod traffic that leaves
	// through the node's own interface and drops forwarded traffic to instance metadata.
	Regional bool
	VNI      uint32
	// NodeIP is the IP of the node, the local end of the tunnel.
	NodeIP netip.Addr
	// Transit is the transit address of the node, and MAC the MAC address of its bridge.
	Transit netip.Addr
	MAC     net.HardwareAddr
	// PodRange is the pod range of the node, and PodCIDR the pod CIDR of the cluster.
	PodRange netip.Prefix
	PodCIDR  netip.Prefix
	// TransitPrefix holds the transit addresses of all nodes.
	TransitPrefix netip.Prefix
	Peers         []Peer
}

// Peer is a node that the tunnel connects with this node.
type Peer struct {
	Name     string
	NodeIP   netip.Addr
	Transit  netip.Addr
	MAC      net.HardwareAddr
	PodRange netip.Prefix
}

// TunnelPlan returns the tunnel state for own and its peers.
func (c *Config) TunnelPlan(own Node, peers []Node) Plan {
	transit := c.TransitAddress(own.Index)
	plan := Plan{
		Regional:      own.Role == Regional,
		VNI:           c.VNI(),
		NodeIP:        own.IP,
		Transit:       transit,
		MAC:           TransitMAC(transit),
		PodRange:      own.PodRange,
		PodCIDR:       c.PodCIDR,
		TransitPrefix: c.TransitPrefix,
		Peers:         make([]Peer, 0, len(peers)),
	}
	for _, n := range peers {
		peerTransit := c.TransitAddress(n.Index)
		plan.Peers = append(plan.Peers, Peer{Name: n.Name, NodeIP: n.IP, Transit: peerTransit, MAC: TransitMAC(peerTransit), PodRange: n.PodRange})
	}
	return plan
}
