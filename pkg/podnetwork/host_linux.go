//go:build linux

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
	"bytes"
	"fmt"
	"net"
	"net/netip"
	"strings"

	"github.com/google/nftables"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

const (
	// BridgeName is the bridge that holds the transit address of the node.
	BridgeName = "kpnbr0"
	// VXLANName is the VXLAN device of the tunnel, a port of the bridge.
	VXLANName = "kpnvx0"
	// deviceAlias marks the devices that the agent created.
	deviceAlias = "karpenter.azure.com/pod-network"
	// routeProtocol marks the routes that the agent created.
	routeProtocol netlink.RouteProtocol = 157
)

// kernel is the part of the netlink API that the agent uses.
type kernel interface {
	LinkByName(name string) (netlink.Link, error)
	LinkList() ([]netlink.Link, error)
	LinkAdd(link netlink.Link) error
	LinkDel(link netlink.Link) error
	LinkSetUp(link netlink.Link) error
	LinkSetAlias(link netlink.Link, name string) error
	LinkSetMaster(link, master netlink.Link) error
	LinkSetLearning(link netlink.Link, mode bool) error
	AddrList(link netlink.Link, family int) ([]netlink.Addr, error)
	AddrReplace(link netlink.Link, addr *netlink.Addr) error
	AddrDel(link netlink.Link, addr *netlink.Addr) error
	NeighList(linkIndex, family int) ([]netlink.Neigh, error)
	NeighSet(neigh *netlink.Neigh) error
	NeighDel(neigh *netlink.Neigh) error
	RouteListFiltered(family int, filter *netlink.Route, filterMask uint64) ([]netlink.Route, error)
	RouteReplace(route *netlink.Route) error
	RouteDel(route *netlink.Route) error
}

var _ kernel = (*netlink.Handle)(nil)

// firewall programs the firewall of the agent.
type firewall interface {
	Apply(plan Plan) error
}

// Host programs the tunnel of a plan on the node that the agent runs on. It is not safe for
// concurrent use.
type Host struct {
	kernel   kernel
	firewall firewall
}

// NewHost returns a Host for the network namespace of the agent, which must be the host's.
func NewHost() (*Host, error) {
	handle, err := netlink.NewHandle()
	if err != nil {
		return nil, fmt.Errorf("opening netlink: %w", err)
	}
	conn, err := nftables.New()
	if err != nil {
		return nil, fmt.Errorf("opening nftables: %w", err)
	}
	return &Host{kernel: handle, firewall: &nftablesFirewall{conn: conn}}, nil
}

// Apply makes the tunnel of the node match plan: its devices, the transit address, the neighbor
// and forwarding entries, the routes and the firewall. It removes what it programmed for former
// peers. It changes nothing when another VXLAN device uses the tunnel port, when a device with a
// tunnel name was not created by the agent, or when a host address that the agent did not assign
// lies in the transit prefix. A peer whose destinations another program routes is left out and
// reported in the error. The firewall comes first, with every peer of the plan, so that the tunnel
// port never accepts other sources, and once more at the end without the peers left out, if any.
func (h *Host) Apply(plan Plan) error {
	if err := h.check(plan); err != nil {
		return err
	}
	if err := h.firewall.Apply(plan); err != nil {
		return fmt.Errorf("programming the firewall: %w", err)
	}
	bridge, err := h.ensureBridge(plan)
	if err != nil {
		return err
	}
	vxlan, err := h.ensureVXLAN(plan, bridge)
	if err != nil {
		return err
	}
	if err := h.ensureAddress(plan, bridge); err != nil {
		return err
	}
	programmed, skipped, err := h.syncRoutes(plan, bridge.Attrs().Index)
	if err != nil {
		return err
	}
	if err := h.syncNeighbors(programmed, bridge.Attrs().Index, vxlan.Attrs().Index); err != nil {
		return err
	}
	if len(skipped) == 0 {
		return nil
	}
	plan.Peers = programmed
	if err := h.firewall.Apply(plan); err != nil {
		return fmt.Errorf("programming the firewall: %w", err)
	}
	return fmt.Errorf("left out peers whose destinations another program routes: %s", strings.Join(skipped, ", "))
}

// check refuses a node where another program uses the tunnel port, a tunnel device name or an
// address in the transit prefix.
func (h *Host) check(plan Plan) error {
	links, err := h.kernel.LinkList()
	if err != nil {
		return fmt.Errorf("listing devices: %w", err)
	}
	owned, err := checkDevices(links)
	if err != nil {
		return err
	}
	addrs, err := h.kernel.AddrList(nil, netlink.FAMILY_V4)
	if err != nil {
		return fmt.Errorf("listing addresses: %w", err)
	}
	for _, addr := range addrs {
		if addr.IPNet == nil || owned[addr.LinkIndex] {
			continue
		}
		if ip, ok := netip.AddrFromSlice(addr.IP); ok && plan.TransitPrefix.Contains(ip.Unmap()) {
			return fmt.Errorf("address %s of another device lies in the transit prefix %s", ip.Unmap(), plan.TransitPrefix)
		}
	}
	return nil
}

// checkDevices returns the indexes of the tunnel devices, and refuses a device with a tunnel name
// that the agent did not create and another VXLAN device on the tunnel port.
func checkDevices(links []netlink.Link) (map[int]bool, error) {
	owned := map[int]bool{}
	for _, link := range links {
		attrs := link.Attrs()
		switch vxlan, isVXLAN := link.(*netlink.Vxlan); {
		case attrs.Name == BridgeName || attrs.Name == VXLANName:
			if attrs.Alias != deviceAlias {
				return nil, fmt.Errorf("device %s exists, but the agent did not create it or its creation was interrupted: delete the device or restart the node", attrs.Name)
			}
			owned[attrs.Index] = true
		case isVXLAN && vxlan.Port == TunnelPort:
			return nil, fmt.Errorf("VXLAN device %s already uses UDP port %d", attrs.Name, TunnelPort)
		}
	}
	return owned, nil
}

// ensureBridge returns the bridge of the tunnel, set up. It creates the bridge again when its MAC
// address or MTU differs from the plan.
func (h *Host) ensureBridge(plan Plan) (netlink.Link, error) {
	if link, err := h.kernel.LinkByName(BridgeName); err == nil {
		if attrs := link.Attrs(); bytes.Equal(attrs.HardwareAddr, plan.MAC) && attrs.MTU == TunnelMTU {
			if err := h.kernel.LinkSetUp(link); err != nil {
				return nil, fmt.Errorf("setting the bridge up: %w", err)
			}
			return link, nil
		}
		if err := h.kernel.LinkDel(link); err != nil {
			return nil, fmt.Errorf("deleting the outdated bridge: %w", err)
		}
	}
	attrs := netlink.NewLinkAttrs()
	attrs.Name, attrs.MTU, attrs.HardwareAddr, attrs.Alias = BridgeName, TunnelMTU, plan.MAC, deviceAlias
	return h.create(&netlink.Bridge{LinkAttrs: attrs})
}

// ensureVXLAN returns the VXLAN device of the tunnel as a port of the bridge. It creates the device
// again when its VNI, port, local address, learning or MTU differs from the plan.
func (h *Host) ensureVXLAN(plan Plan, bridge netlink.Link) (netlink.Link, error) {
	local := net.IP(plan.NodeIP.AsSlice())
	if link, err := h.kernel.LinkByName(VXLANName); err == nil {
		if vxlan, ok := link.(*netlink.Vxlan); ok && vxlan.VxlanId == int(plan.VNI) && vxlan.Port == TunnelPort &&
			vxlan.SrcAddr.Equal(local) && !vxlan.Learning && vxlan.MTU == TunnelMTU {
			return link, h.attach(link, bridge)
		}
		if err := h.kernel.LinkDel(link); err != nil {
			return nil, fmt.Errorf("deleting the outdated VXLAN device: %w", err)
		}
	}
	attrs := netlink.NewLinkAttrs()
	attrs.Name, attrs.MTU, attrs.Alias = VXLANName, TunnelMTU, deviceAlias
	link, err := h.create(&netlink.Vxlan{LinkAttrs: attrs, VxlanId: int(plan.VNI), SrcAddr: local, Port: TunnelPort})
	if err != nil {
		return nil, err
	}
	return link, h.attach(link, bridge)
}

// create adds a device, marks it as the agent's and sets it up. When a step after the addition fails,
// it deletes the device again, because every later pass would refuse an unmarked device with a
// tunnel name. It returns the error of the step, with the error of the deletion if that fails too.
func (h *Host) create(link netlink.Link) (netlink.Link, error) {
	name := link.Attrs().Name
	if err := h.kernel.LinkAdd(link); err != nil {
		return nil, fmt.Errorf("creating device %s: %w", name, err)
	}
	created, err := h.markAndSetUp(name)
	if err != nil {
		if delErr := h.kernel.LinkDel(link); delErr != nil {
			return nil, fmt.Errorf("%w; deleting device %s again: %w", err, name, delErr)
		}
		return nil, err
	}
	return created, nil
}

// markAndSetUp marks a device that the agent added as its own and sets it up.
func (h *Host) markAndSetUp(name string) (netlink.Link, error) {
	created, err := h.kernel.LinkByName(name)
	if err != nil {
		return nil, fmt.Errorf("reading device %s: %w", name, err)
	}
	if err := h.kernel.LinkSetAlias(created, deviceAlias); err != nil {
		return nil, fmt.Errorf("marking device %s: %w", name, err)
	}
	if err := h.kernel.LinkSetUp(created); err != nil {
		return nil, fmt.Errorf("setting device %s up: %w", name, err)
	}
	return created, nil
}

// attach makes the VXLAN device a port of the bridge that learns no addresses, and sets it up.
func (h *Host) attach(vxlan, bridge netlink.Link) error {
	if vxlan.Attrs().MasterIndex != bridge.Attrs().Index {
		if err := h.kernel.LinkSetMaster(vxlan, bridge); err != nil {
			return fmt.Errorf("attaching the VXLAN device to the bridge: %w", err)
		}
	}
	if err := h.kernel.LinkSetLearning(vxlan, false); err != nil {
		return fmt.Errorf("turning off learning on the VXLAN device: %w", err)
	}
	if err := h.kernel.LinkSetUp(vxlan); err != nil {
		return fmt.Errorf("setting the VXLAN device up: %w", err)
	}
	return nil
}

// ensureAddress gives the bridge the transit address of the node as its only address, a /32.
func (h *Host) ensureAddress(plan Plan, bridge netlink.Link) error {
	want := &netlink.Addr{IPNet: &net.IPNet{IP: plan.Transit.AsSlice(), Mask: net.CIDRMask(32, 32)}}
	addrs, err := h.kernel.AddrList(bridge, netlink.FAMILY_V4)
	if err != nil {
		return fmt.Errorf("listing the addresses of the bridge: %w", err)
	}
	for i := range addrs {
		if addrs[i].IPNet != nil && addrs[i].IPNet.String() != want.IPNet.String() {
			if err := h.kernel.AddrDel(bridge, &addrs[i]); err != nil {
				return fmt.Errorf("deleting address %s from the bridge: %w", addrs[i].IPNet, err)
			}
		}
	}
	if err := h.kernel.AddrReplace(bridge, want); err != nil {
		return fmt.Errorf("adding the transit address to the bridge: %w", err)
	}
	return nil
}

// syncRoutes programs, for every peer, a link route to its transit address and a route to its pod
// range through that address, both with the node's own transit address as source. It leaves out a
// peer whose destinations another program routes, deletes the routes of the agent that the plan
// no longer holds, and returns the programmed peers and the names of the others. A route of another
// program counts only when its destination is exactly the transit address or the pod range of a
// peer: a more specific route inside a peer's pod range goes unseen, and the traffic for that part
// follows it.
func (h *Host) syncRoutes(plan Plan, bridge int) ([]Peer, []string, error) {
	routes, err := h.kernel.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{Table: unix.RT_TABLE_MAIN}, netlink.RT_FILTER_TABLE)
	if err != nil {
		return nil, nil, fmt.Errorf("listing routes: %w", err)
	}
	stale, foreign := classifyRoutes(routes, bridge)
	programmed, skipped := make([]Peer, 0, len(plan.Peers)), make([]string, 0)
	for _, peer := range plan.Peers {
		toTransit, toRange := peerRoutes(plan, peer, bridge)
		if foreign[toTransit.Dst.String()] || foreign[toRange.Dst.String()] {
			skipped = append(skipped, peer.Name)
			continue
		}
		for _, route := range []*netlink.Route{toTransit, toRange} {
			if err := h.kernel.RouteReplace(route); err != nil {
				return nil, nil, fmt.Errorf("routing to peer %s: %w", peer.Name, err)
			}
			delete(stale, route.Dst.String())
		}
		programmed = append(programmed, peer)
	}
	for dst := range stale {
		route := stale[dst]
		if err := h.kernel.RouteDel(&route); err != nil {
			return nil, nil, fmt.Errorf("deleting the route to %s: %w", dst, err)
		}
	}
	return programmed, skipped, nil
}

// classifyRoutes splits the routes of the main table into those that the agent created on the
// bridge, by destination, and the destinations of all others.
func classifyRoutes(routes []netlink.Route, bridge int) (map[string]netlink.Route, map[string]bool) {
	ours, foreign := map[string]netlink.Route{}, map[string]bool{}
	for _, route := range routes {
		switch {
		case route.Dst == nil:
			continue
		case route.LinkIndex == bridge && route.Protocol == routeProtocol:
			ours[route.Dst.String()] = route
		default:
			foreign[route.Dst.String()] = true
		}
	}
	return ours, foreign
}

// peerRoutes returns the link route to the transit address of a peer and the route to its pod
// range through that address.
func peerRoutes(plan Plan, peer Peer, bridge int) (*netlink.Route, *netlink.Route) {
	toTransit := &netlink.Route{LinkIndex: bridge, Table: unix.RT_TABLE_MAIN, Protocol: routeProtocol, Scope: netlink.SCOPE_LINK,
		Dst: &net.IPNet{IP: peer.Transit.AsSlice(), Mask: net.CIDRMask(32, 32)}, Src: plan.Transit.AsSlice()}
	toRange := &netlink.Route{LinkIndex: bridge, Table: unix.RT_TABLE_MAIN, Protocol: routeProtocol,
		Dst: &net.IPNet{IP: peer.PodRange.Addr().AsSlice(), Mask: net.CIDRMask(peer.PodRange.Bits(), 32)},
		Gw:  peer.Transit.AsSlice(), Src: plan.Transit.AsSlice()}
	return toTransit, toRange
}

// syncNeighbors programs, for every peer, a permanent neighbor entry for its transit address on
// the bridge and a forwarding entry from its MAC address to its node IP on the VXLAN device. It
// deletes the entries of former peers and leaves entries that the kernel learned alone.
func (h *Host) syncNeighbors(peers []Peer, bridge, vxlan int) error {
	keep := map[string]bool{}
	for _, peer := range peers {
		for _, n := range []*netlink.Neigh{
			{LinkIndex: bridge, Family: netlink.FAMILY_V4, State: netlink.NUD_PERMANENT, IP: peer.Transit.AsSlice(), HardwareAddr: peer.MAC},
			{LinkIndex: vxlan, Family: unix.AF_BRIDGE, State: netlink.NUD_PERMANENT, Flags: netlink.NTF_SELF, IP: peer.NodeIP.AsSlice(), HardwareAddr: peer.MAC},
		} {
			if err := h.kernel.NeighSet(n); err != nil {
				return fmt.Errorf("programming the entries of peer %s: %w", peer.Name, err)
			}
			keep[neighborKey(n)] = true
		}
	}
	for _, device := range [][2]int{{bridge, netlink.FAMILY_V4}, {vxlan, unix.AF_BRIDGE}} {
		existing, err := h.kernel.NeighList(device[0], device[1])
		if err != nil {
			return fmt.Errorf("listing neighbor entries: %w", err)
		}
		for i := range existing {
			if ownNeighbor(&existing[i]) && !keep[neighborKey(&existing[i])] {
				if err := h.kernel.NeighDel(&existing[i]); err != nil {
					return fmt.Errorf("deleting the entry of a former peer: %w", err)
				}
			}
		}
	}
	return nil
}

// neighborKey identifies a neighbor or forwarding entry.
func neighborKey(n *netlink.Neigh) string {
	return fmt.Sprintf("%d/%d/%s/%s", n.LinkIndex, n.Family, n.IP, n.HardwareAddr)
}

// ownNeighbor reports whether the agent may have programmed an entry: a permanent neighbor entry,
// or a permanent forwarding entry of the VXLAN device itself with a destination.
func ownNeighbor(n *netlink.Neigh) bool {
	if n.State&netlink.NUD_PERMANENT == 0 {
		return false
	}
	return n.Family != unix.AF_BRIDGE || (n.Flags&netlink.NTF_SELF != 0 && n.IP != nil)
}
