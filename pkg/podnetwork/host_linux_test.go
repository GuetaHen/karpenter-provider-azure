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
	"errors"
	"net"
	"net/netip"
	"slices"
	"testing"

	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gstruct"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

var (
	regionalOwn  = Node{Name: "regional-1", Role: Regional, IP: netip.MustParseAddr("10.1.0.4"), PodRange: netip.MustParsePrefix("10.244.7.0/24"), Index: 7}
	homePeer     = Node{Name: "home-1", Role: Home, IP: netip.MustParseAddr("10.0.0.4"), PodRange: netip.MustParsePrefix("10.244.1.0/24"), Index: 1}
	regionalPeer = Node{Name: "regional-2", Role: Regional, IP: netip.MustParseAddr("10.2.0.4"), PodRange: netip.MustParsePrefix("10.244.255.0/24"), Index: 255}
)

// fakeKernel keeps devices, addresses, neighbor entries and routes in memory, and records changes.
type fakeKernel struct {
	links    []netlink.Link
	addrs    []netlink.Addr
	neighs   []netlink.Neigh
	routes   []netlink.Route
	learning map[string]bool
	changes  []string
	// fail names, for an operation, the device on which it fails.
	fail map[string]string
}

func (k *fakeKernel) LinkByName(name string) (netlink.Link, error) {
	if k.fail["LinkByName"] == name {
		return nil, errors.New("reading failed")
	}
	for _, link := range k.links {
		if link.Attrs().Name == name {
			return link, nil
		}
	}
	return nil, errors.New("link not found")
}

func (k *fakeKernel) LinkList() ([]netlink.Link, error) { return k.links, nil }

// LinkAdd adds a device without the alias, which create sets apart.
func (k *fakeKernel) LinkAdd(link netlink.Link) error {
	link.Attrs().Index = 100 + len(k.changes)
	link.Attrs().Alias = ""
	k.links = append(k.links, link)
	k.changes = append(k.changes, "add link "+link.Attrs().Name)
	return nil
}

// LinkDel deletes a device with its addresses, routes and entries, as the kernel does.
func (k *fakeKernel) LinkDel(link netlink.Link) error {
	if k.fail["LinkDel"] == link.Attrs().Name {
		return errors.New("deleting failed")
	}
	index := link.Attrs().Index
	k.links = slices.DeleteFunc(k.links, func(l netlink.Link) bool { return l.Attrs().Index == index })
	k.addrs = slices.DeleteFunc(k.addrs, func(a netlink.Addr) bool { return a.LinkIndex == index })
	k.routes = slices.DeleteFunc(k.routes, func(r netlink.Route) bool { return r.LinkIndex == index })
	k.neighs = slices.DeleteFunc(k.neighs, func(n netlink.Neigh) bool { return n.LinkIndex == index })
	k.changes = append(k.changes, "delete link "+link.Attrs().Name)
	return nil
}

func (k *fakeKernel) LinkSetUp(link netlink.Link) error {
	if k.fail["LinkSetUp"] == link.Attrs().Name {
		return errors.New("setting up failed")
	}
	link.Attrs().Flags |= net.FlagUp
	return nil
}

func (k *fakeKernel) LinkSetAlias(link netlink.Link, alias string) error {
	if k.fail["LinkSetAlias"] == link.Attrs().Name {
		return errors.New("marking failed")
	}
	link.Attrs().Alias = alias
	return nil
}

func (k *fakeKernel) LinkSetMaster(link, master netlink.Link) error {
	link.Attrs().MasterIndex = master.Attrs().Index
	return nil
}

func (k *fakeKernel) LinkSetLearning(link netlink.Link, mode bool) error {
	if k.learning == nil {
		k.learning = map[string]bool{}
	}
	k.learning[link.Attrs().Name] = mode
	return nil
}

func (k *fakeKernel) AddrList(link netlink.Link, _ int) ([]netlink.Addr, error) {
	if link == nil {
		return k.addrs, nil
	}
	return slices.DeleteFunc(slices.Clone(k.addrs), func(a netlink.Addr) bool { return a.LinkIndex != link.Attrs().Index }), nil
}

func (k *fakeKernel) AddrReplace(link netlink.Link, addr *netlink.Addr) error {
	k.addrs = slices.DeleteFunc(k.addrs, func(a netlink.Addr) bool {
		return a.LinkIndex == link.Attrs().Index && a.IPNet.String() == addr.IPNet.String()
	})
	added := *addr
	added.LinkIndex = link.Attrs().Index
	k.addrs = append(k.addrs, added)
	k.changes = append(k.changes, "replace address "+addr.IPNet.String())
	return nil
}

func (k *fakeKernel) AddrDel(link netlink.Link, addr *netlink.Addr) error {
	k.addrs = slices.DeleteFunc(k.addrs, func(a netlink.Addr) bool {
		return a.LinkIndex == link.Attrs().Index && a.IPNet.String() == addr.IPNet.String()
	})
	k.changes = append(k.changes, "delete address "+addr.IPNet.String())
	return nil
}

func (k *fakeKernel) NeighList(index, family int) ([]netlink.Neigh, error) {
	return slices.DeleteFunc(slices.Clone(k.neighs), func(n netlink.Neigh) bool { return n.LinkIndex != index || n.Family != family }), nil
}

func (k *fakeKernel) NeighSet(n *netlink.Neigh) error {
	k.neighs = slices.DeleteFunc(k.neighs, func(e netlink.Neigh) bool { return sameEntry(&e, n) })
	k.neighs = append(k.neighs, *n)
	k.changes = append(k.changes, "set neighbor "+neighborKey(n))
	return nil
}

func (k *fakeKernel) NeighDel(n *netlink.Neigh) error {
	k.neighs = slices.DeleteFunc(k.neighs, func(e netlink.Neigh) bool { return sameEntry(&e, n) })
	k.changes = append(k.changes, "delete neighbor "+neighborKey(n))
	return nil
}

// sameEntry matches entries as the kernel does: forwarding entries by MAC address, neighbor
// entries by IP, on the same device.
func sameEntry(a, b *netlink.Neigh) bool {
	if a.LinkIndex != b.LinkIndex || a.Family != b.Family {
		return false
	}
	if a.Family == unix.AF_BRIDGE {
		return a.HardwareAddr.String() == b.HardwareAddr.String()
	}
	return a.IP.Equal(b.IP)
}

func (k *fakeKernel) RouteListFiltered(int, *netlink.Route, uint64) ([]netlink.Route, error) {
	return k.routes, nil
}

// RouteReplace replaces any route to the same destination, as the kernel does in one table.
func (k *fakeKernel) RouteReplace(r *netlink.Route) error {
	k.routes = slices.DeleteFunc(k.routes, func(e netlink.Route) bool { return e.Dst.String() == r.Dst.String() })
	k.routes = append(k.routes, *r)
	k.changes = append(k.changes, "replace route "+r.Dst.String())
	return nil
}

func (k *fakeKernel) RouteDel(r *netlink.Route) error {
	k.routes = slices.DeleteFunc(k.routes, func(e netlink.Route) bool { return e.Dst.String() == r.Dst.String() && e.LinkIndex == r.LinkIndex })
	k.changes = append(k.changes, "delete route "+r.Dst.String())
	return nil
}

// route returns the route to dst, or nil.
func (k *fakeKernel) route(dst string) *netlink.Route {
	for i := range k.routes {
		if k.routes[i].Dst.String() == dst {
			return &k.routes[i]
		}
	}
	return nil
}

// neighbor returns the entry of a device and family for an IP, or nil.
func (k *fakeKernel) neighbor(index, family int, ip string) *netlink.Neigh {
	for i := range k.neighs {
		if n := &k.neighs[i]; n.LinkIndex == index && n.Family == family && n.IP.String() == ip {
			return n
		}
	}
	return nil
}

// fakeFirewall records the plans that it applies and, for each, how many changes the kernel made
// before.
type fakeFirewall struct {
	kernel *fakeKernel
	plans  []Plan
	at     []int
}

func (f *fakeFirewall) Apply(plan Plan) error {
	f.plans = append(f.plans, plan)
	if f.kernel != nil {
		f.at = append(f.at, len(f.kernel.changes))
	}
	return nil
}

func ipNet(cidr string) *net.IPNet {
	p := netip.MustParsePrefix(cidr)
	return &net.IPNet{IP: p.Addr().AsSlice(), Mask: net.CIDRMask(p.Bits(), 32)}
}

// testHost returns a host whose kernel holds the uplink of a node with IP 10.1.0.4, another VXLAN
// device on another port, and an address of another program outside the transit prefix.
func testHost() (*Host, *fakeKernel, *fakeFirewall) {
	k := &fakeKernel{
		links: []netlink.Link{
			&netlink.Device{LinkAttrs: netlink.LinkAttrs{Name: "eth0", Index: 2, MTU: 1500}},
			&netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "other-vxlan", Index: 3}, Port: 8472},
		},
		addrs:  []netlink.Addr{{IPNet: ipNet("10.1.0.4/24"), LinkIndex: 2}, {IPNet: ipNet("169.254.23.0/32"), LinkIndex: 3}},
		routes: []netlink.Route{{LinkIndex: 2, Dst: ipNet("10.1.0.0/24"), Protocol: unix.RTPROT_KERNEL}},
	}
	f := &fakeFirewall{kernel: k}
	return &Host{kernel: k, firewall: f}, k, f
}

// expectProgrammed checks that the kernel holds the tunnel of plan: its devices, the transit
// address, and the routes and entries of every peer.
func expectProgrammed(g *WithT, k *fakeKernel, plan Plan) {
	bridge, err := k.LinkByName(BridgeName)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(bridge.Attrs()).To(And(HaveField("HardwareAddr", plan.MAC), HaveField("MTU", TunnelMTU), HaveField("Alias", deviceAlias)))
	g.Expect(bridge.Attrs().Flags & net.FlagUp).ToNot(BeZero())
	link, err := k.LinkByName(VXLANName)
	g.Expect(err).ToNot(HaveOccurred())
	vxlan, ok := link.(*netlink.Vxlan)
	g.Expect(ok).To(BeTrue())
	g.Expect(vxlan).To(And(HaveField("VxlanId", int(plan.VNI)), HaveField("Port", TunnelPort), HaveField("Learning", false)))
	g.Expect(vxlan.SrcAddr.String()).To(Equal(plan.NodeIP.String()))
	g.Expect(vxlan.Attrs()).To(And(HaveField("MTU", TunnelMTU), HaveField("Alias", deviceAlias), HaveField("MasterIndex", bridge.Attrs().Index)))
	g.Expect(vxlan.Flags & net.FlagUp).ToNot(BeZero())
	g.Expect(k.learning).To(HaveKeyWithValue(VXLANName, false), "the bridge port learns no addresses")
	addrs, err := k.AddrList(bridge, netlink.FAMILY_V4)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(addrs).To(ConsistOf(HaveField("IPNet", ipNet(plan.Transit.String()+"/32"))))
	for _, peer := range plan.Peers {
		toTransit, toRange := k.route(peer.Transit.String()+"/32"), k.route(peer.PodRange.String())
		g.Expect(toTransit).To(gstruct.PointTo(And(HaveField("LinkIndex", bridge.Attrs().Index), HaveField("Scope", netlink.SCOPE_LINK),
			HaveField("Protocol", routeProtocol))), peer.Name)
		g.Expect(toTransit.Src.String()).To(Equal(plan.Transit.String()), peer.Name)
		g.Expect(toRange).To(gstruct.PointTo(And(HaveField("LinkIndex", bridge.Attrs().Index), HaveField("Protocol", routeProtocol))), peer.Name)
		g.Expect(toRange.Gw.String()).To(Equal(peer.Transit.String()), peer.Name)
		g.Expect(toRange.Src.String()).To(Equal(plan.Transit.String()), peer.Name)
		g.Expect(k.neighbor(bridge.Attrs().Index, netlink.FAMILY_V4, peer.Transit.String())).To(gstruct.PointTo(And(
			HaveField("State", netlink.NUD_PERMANENT), HaveField("HardwareAddr", peer.MAC))), peer.Name)
		g.Expect(k.neighbor(vxlan.Index, unix.AF_BRIDGE, peer.NodeIP.String())).To(gstruct.PointTo(And(
			HaveField("State", netlink.NUD_PERMANENT), HaveField("Flags", netlink.NTF_SELF), HaveField("HardwareAddr", peer.MAC))), peer.Name)
	}
}

func TestHostProgramsTheTunnelAndRemovesFormerPeers(t *testing.T) {
	g := NewWithT(t)
	host, k, f := testHost()
	plan := testConfig(t).TunnelPlan(regionalOwn, []Node{homePeer, regionalPeer})
	g.Expect(host.Apply(plan)).To(Succeed())
	g.Expect(k.changes).To(ContainElements("add link kpnbr0", "add link kpnvx0", "replace address 169.254.64.7/32"))
	expectProgrammed(g, k, plan)
	g.Expect(f.plans).To(HaveLen(1))
	g.Expect(f.plans[0].Peers).To(HaveLen(2))
	g.Expect(f.at).To(Equal([]int{0}), "the firewall comes before the devices")

	// A restarted agent adopts its devices. A former peer loses its routes and entries, and the
	// bridge loses any other address. Learned entries, the entries of the bridge on its port and the
	// routes of other programs stay, even on the bridge or with the protocol of the agent.
	bridge, _ := k.LinkByName(BridgeName)
	vxlan, _ := k.LinkByName(VXLANName)
	b, v := bridge.Attrs().Index, vxlan.Attrs().Index
	portMAC := net.HardwareAddr{0x02, 0x42, 0x0a, 0x00, 0x00, 0x01}
	k.neighs = append(k.neighs,
		netlink.Neigh{LinkIndex: b, Family: netlink.FAMILY_V4, State: netlink.NUD_REACHABLE, IP: net.ParseIP("169.254.64.9")},
		netlink.Neigh{LinkIndex: v, Family: unix.AF_BRIDGE, State: netlink.NUD_PERMANENT, Flags: netlink.NTF_MASTER, HardwareAddr: portMAC})
	k.addrs = append(k.addrs, netlink.Addr{IPNet: ipNet("169.254.64.8/32"), LinkIndex: b})
	k.routes = append(k.routes,
		netlink.Route{LinkIndex: b, Dst: ipNet("10.9.1.0/24"), Protocol: unix.RTPROT_BOOT},
		netlink.Route{LinkIndex: 2, Dst: ipNet("10.9.2.0/24"), Protocol: routeProtocol})
	before := len(k.changes)
	restarted := &Host{kernel: k, firewall: f}
	reduced := testConfig(t).TunnelPlan(regionalOwn, []Node{homePeer})
	g.Expect(restarted.Apply(reduced)).To(Succeed())
	g.Expect(k.changes[before:]).ToNot(ContainElement(HavePrefix("add link")))
	expectProgrammed(g, k, reduced)
	g.Expect(k.route("10.244.255.0/24")).To(BeNil())
	g.Expect(k.route("169.254.64.255/32")).To(BeNil())
	g.Expect(k.neighs).ToNot(ContainElement(HaveField("HardwareAddr", TransitMAC(netip.MustParseAddr("169.254.64.255")))))
	g.Expect(k.neighbor(b, netlink.FAMILY_V4, "169.254.64.9")).ToNot(BeNil())
	g.Expect(k.neighs).To(ContainElement(HaveField("HardwareAddr", portMAC)))
	for _, dst := range []string{"10.1.0.0/24", "10.9.1.0/24", "10.9.2.0/24"} {
		g.Expect(k.route(dst)).ToNot(BeNil(), dst)
	}
	g.Expect(f.plans[1].Peers).To(ConsistOf(HaveField("Name", "home-1")))

	// Another VNI replaces the VXLAN device but keeps the bridge.
	before = len(k.changes)
	changed := reduced
	changed.VNI++
	g.Expect(restarted.Apply(changed)).To(Succeed())
	g.Expect(k.changes[before:]).To(ContainElements("delete link kpnvx0", "add link kpnvx0"))
	g.Expect(k.changes[before:]).ToNot(ContainElement("add link kpnbr0"))
	expectProgrammed(g, k, changed)
}

func TestHostRecreatesADeviceThatDiffersFromThePlan(t *testing.T) {
	for name, tc := range map[string]struct {
		device, kept string
		change       func(netlink.Link)
	}{
		"another MAC address of the bridge": {BridgeName, VXLANName, func(l netlink.Link) { l.Attrs().HardwareAddr = TransitMAC(netip.MustParseAddr("169.254.64.8")) }},
		"another MTU of the bridge":         {BridgeName, VXLANName, func(l netlink.Link) { l.Attrs().MTU = 1500 }},
		"another VXLAN port":                {VXLANName, BridgeName, func(l netlink.Link) { l.(*netlink.Vxlan).Port = 8472 }},
		"another local address":             {VXLANName, BridgeName, func(l netlink.Link) { l.(*netlink.Vxlan).SrcAddr = net.ParseIP("10.1.0.5") }},
		"learning on the VXLAN device":      {VXLANName, BridgeName, func(l netlink.Link) { l.(*netlink.Vxlan).Learning = true }},
		"another MTU of the VXLAN device":   {VXLANName, BridgeName, func(l netlink.Link) { l.Attrs().MTU = 1450 }},
	} {
		t.Run(name, func(t *testing.T) {
			g := NewWithT(t)
			host, k, _ := testHost()
			plan := testConfig(t).TunnelPlan(regionalOwn, []Node{homePeer, regionalPeer})
			g.Expect(host.Apply(plan)).To(Succeed())
			link, err := k.LinkByName(tc.device)
			g.Expect(err).ToNot(HaveOccurred())
			tc.change(link)
			before := len(k.changes)
			g.Expect(host.Apply(plan)).To(Succeed())
			g.Expect(k.changes[before:]).To(ContainElements("delete link "+tc.device, "add link "+tc.device))
			g.Expect(k.changes[before:]).ToNot(ContainElement("add link " + tc.kept))
			expectProgrammed(g, k, plan)
		})
	}
}

func TestHostRefusesANodeThatAnotherProgramUses(t *testing.T) {
	for name, tc := range map[string]struct {
		prepare func(*fakeKernel)
		reason  string
	}{
		"another VXLAN device on the tunnel port": {func(k *fakeKernel) {
			k.links = append(k.links, &netlink.Vxlan{LinkAttrs: netlink.LinkAttrs{Name: "vxlan0", Index: 4}, Port: TunnelPort})
		}, "VXLAN device vxlan0 already uses UDP port 4789"},
		"a bridge with the tunnel name": {func(k *fakeKernel) {
			k.links = append(k.links, &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: BridgeName, Index: 4}})
		}, "device kpnbr0 exists, but the agent did not create it or its creation was interrupted: delete the device or restart the node"},
		"a host address in the transit prefix": {func(k *fakeKernel) {
			k.addrs = append(k.addrs, netlink.Addr{IPNet: ipNet("169.254.64.200/32"), LinkIndex: 2})
		}, "address 169.254.64.200 of another device lies in the transit prefix"},
	} {
		t.Run(name, func(t *testing.T) {
			g := NewWithT(t)
			host, k, f := testHost()
			tc.prepare(k)
			g.Expect(host.Apply(testConfig(t).TunnelPlan(regionalOwn, []Node{homePeer}))).To(MatchError(ContainSubstring(tc.reason)))
			g.Expect(k.changes).To(BeEmpty())
			g.Expect(f.plans).To(BeEmpty())
		})
	}
}

func TestHostLeavesOutAPeerWhoseDestinationAnotherProgramRoutes(t *testing.T) {
	for name, dsts := range map[string][2]string{
		"its pod range":       {"10.244.255.0/24", "169.254.64.255/32"},
		"its transit address": {"169.254.64.255/32", "10.244.255.0/24"},
	} {
		t.Run(name, func(t *testing.T) {
			g := NewWithT(t)
			host, k, f := testHost()
			k.routes = append(k.routes, netlink.Route{LinkIndex: 2, Dst: ipNet(dsts[0]), Gw: net.ParseIP("10.1.0.1"), Protocol: unix.RTPROT_BOOT})
			err := host.Apply(testConfig(t).TunnelPlan(regionalOwn, []Node{homePeer, regionalPeer}))
			g.Expect(err).To(MatchError(ContainSubstring("left out peers whose destinations another program routes: regional-2")))
			g.Expect(k.route(dsts[0])).To(gstruct.PointTo(HaveField("LinkIndex", 2)), "the route of the other program stays")
			g.Expect(k.route(dsts[1])).To(BeNil())
			g.Expect(k.neighs).ToNot(ContainElement(HaveField("HardwareAddr", TransitMAC(netip.MustParseAddr("169.254.64.255")))))
			g.Expect(f.plans).To(HaveLen(2))
			g.Expect(f.plans[0].Peers).To(HaveLen(2))
			g.Expect(f.plans[1].Peers).To(ConsistOf(HaveField("Name", "home-1")))
			g.Expect(f.at).To(Equal([]int{0, len(k.changes)}), "the firewall comes first, and once more at the end")
			expectProgrammed(g, k, testConfig(t).TunnelPlan(regionalOwn, []Node{homePeer}))
		})
	}
}

func TestHostDeletesADeviceWhoseSetupFails(t *testing.T) {
	for _, step := range []string{"LinkByName", "LinkSetAlias", "LinkSetUp"} {
		t.Run(step, func(t *testing.T) {
			g := NewWithT(t)
			host, k, _ := testHost()
			k.fail = map[string]string{step: VXLANName}
			plan := testConfig(t).TunnelPlan(regionalOwn, []Node{homePeer})
			g.Expect(host.Apply(plan)).To(MatchError(ContainSubstring("device " + VXLANName)))
			g.Expect(k.changes).To(ContainElements("add link "+VXLANName, "delete link "+VXLANName))
			g.Expect(k.links).ToNot(ContainElement(WithTransform(func(l netlink.Link) string { return l.Attrs().Name }, Equal(VXLANName))))

			// The next pass creates the device again instead of refusing the node.
			k.fail = nil
			g.Expect(host.Apply(plan)).To(Succeed())
			expectProgrammed(g, k, plan)
		})
	}

	// When the deletion fails as well, both errors are returned and the unmarked device stays, so the
	// next pass refuses the node.
	t.Run("LinkDel", func(t *testing.T) {
		g := NewWithT(t)
		host, k, _ := testHost()
		k.fail = map[string]string{"LinkSetAlias": VXLANName, "LinkDel": VXLANName}
		plan := testConfig(t).TunnelPlan(regionalOwn, []Node{homePeer})
		err := host.Apply(plan)
		g.Expect(err).To(MatchError(ContainSubstring("marking device " + VXLANName + ": marking failed")))
		g.Expect(err).To(MatchError(ContainSubstring("deleting device " + VXLANName + " again: deleting failed")))
		g.Expect(k.links).To(ContainElement(WithTransform(func(l netlink.Link) string { return l.Attrs().Name }, Equal(VXLANName))))

		k.fail = nil
		g.Expect(host.Apply(plan)).To(MatchError(ContainSubstring("its creation was interrupted")))
	})
}
