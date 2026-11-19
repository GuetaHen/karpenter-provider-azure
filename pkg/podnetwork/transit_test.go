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
	"strings"
	"testing"

	. "github.com/onsi/gomega"
)

func TestTransitPrefixValidation(t *testing.T) {
	g := NewWithT(t)
	config := func(transit, podCIDR string) *Config {
		c := &Config{PodCIDR: netip.MustParsePrefix(podCIDR), NodeResourceGroupID: testResourceGroupID, NodeClassGroup: testGroup}
		if transit != "" {
			c.TransitPrefix = netip.MustParsePrefix(transit)
		}
		return c
	}
	for _, valid := range [][2]string{{DefaultTransitPrefix, "10.0.0.0/10"}, {DefaultTransitPrefix, "10.244.0.0/16"}, {"169.254.64.0/20", "10.240.0.0/12"}, {"169.254.200.0/24", "10.244.0.0/16"}} {
		g.Expect(config(valid[0], valid[1]).Validate()).To(Succeed(), "%s for %s", valid[0], valid[1])
	}
	for name, invalid := range map[string][2]string{
		"no transit prefix":              {"", "10.244.0.0/16"},
		"an IPv6 prefix":                 {"fd00::/64", "10.244.0.0/16"},
		"a prefix outside link-local":    {"10.255.0.0/18", "10.0.0.0/10"},
		"a prefix wider than link-local": {"169.254.0.0/15", "10.244.0.0/16"},
		"host bits":                      {"169.254.64.1/18", "10.0.0.0/10"},
		"the instance metadata address":  {"169.254.128.0/17", "10.0.0.0/9"},
		"too small for the pod CIDR":     {"169.254.64.0/19", "10.0.0.0/10"},
	} {
		g.Expect(config(invalid[0], invalid[1]).Validate()).ToNot(Succeed(), name)
	}
}

func TestTransitAddressesAndMACsAreUniqueOverEveryIndex(t *testing.T) {
	g := NewWithT(t)
	c := &Config{PodCIDR: netip.MustParsePrefix("10.0.0.0/10"), NodeResourceGroupID: testResourceGroupID, NodeClassGroup: testGroup,
		TransitPrefix: netip.MustParsePrefix(DefaultTransitPrefix)}
	g.Expect(c.Validate()).To(Succeed())
	g.Expect(c.TransitAddress(0)).To(Equal(netip.MustParseAddr("169.254.64.0")))
	g.Expect(c.TransitAddress(1<<14 - 1)).To(Equal(netip.MustParseAddr("169.254.127.255")))
	g.Expect(TransitMAC(netip.MustParseAddr("169.254.64.5"))).To(Equal(net.HardwareAddr{0x02, 0x00, 0xa9, 0xfe, 0x40, 0x05}))
	addresses, macs := map[netip.Addr]bool{}, map[string]bool{}
	for index := range uint32(1 << 14) {
		addr := c.TransitAddress(index)
		mac := TransitMAC(addr)
		g.Expect(c.TransitPrefix.Contains(addr)).To(BeTrue())
		g.Expect(mac[0]&0x03).To(Equal(byte(0x02)), "locally administered and unicast")
		addresses[addr], macs[mac.String()] = true, true
	}
	g.Expect(addresses).To(HaveLen(1 << 14))
	g.Expect(macs).To(HaveLen(1 << 14))
}

func TestVNI(t *testing.T) {
	g := NewWithT(t)
	c := testConfig(t)
	g.Expect(c.VNI()).To(And(BeNumerically(">", 0), BeNumerically("<", 1<<24)))
	same := &Config{PodCIDR: c.PodCIDR, NodeResourceGroupID: strings.ToUpper(testResourceGroupID), NodeClassGroup: testGroup, TransitPrefix: c.TransitPrefix}
	other := &Config{PodCIDR: c.PodCIDR, NodeResourceGroupID: testResourceGroupID + "-2", NodeClassGroup: testGroup, TransitPrefix: c.TransitPrefix}
	g.Expect(same.Validate()).To(Succeed())
	g.Expect(other.Validate()).To(Succeed())
	g.Expect(same.VNI()).To(Equal(c.VNI()), "ARM IDs compare ignoring case")
	g.Expect(other.VNI()).ToNot(Equal(c.VNI()))
	g.Expect(c.VNI()).To(Equal(uint32(0x22b499)), "agents of different versions must agree on the VNI")
	g.Expect(foldVNI(0x12345678)).To(Equal(uint32(0x34566a)))
	g.Expect(foldVNI(0xffffffff)).To(Equal(uint32(0xffff00)))
	g.Expect(foldVNI(0x01000001)).To(Equal(uint32(1)), "never 0")
	g.Expect(foldVNI(0)).To(Equal(uint32(1)), "never 0")
}

func TestTunnelPlan(t *testing.T) {
	g := NewWithT(t)
	c := testConfig(t)
	own := Node{Name: "regional-1", Role: Regional, IP: netip.MustParseAddr("10.1.0.4"), PodRange: netip.MustParsePrefix("10.244.7.0/24"), Index: 7}
	home := Node{Name: "home-1", Role: Home, IP: netip.MustParseAddr("10.0.0.4"), PodRange: netip.MustParsePrefix("10.244.1.0/24"), Index: 1}
	g.Expect(c.TunnelPlan(own, []Node{home})).To(Equal(Plan{
		Regional:      true,
		VNI:           c.VNI(),
		NodeIP:        own.IP,
		Transit:       netip.MustParseAddr("169.254.64.7"),
		MAC:           net.HardwareAddr{0x02, 0x00, 0xa9, 0xfe, 0x40, 0x07},
		PodRange:      own.PodRange,
		PodCIDR:       c.PodCIDR,
		TransitPrefix: c.TransitPrefix,
		Peers: []Peer{{Name: "home-1", NodeIP: home.IP, Transit: netip.MustParseAddr("169.254.64.1"),
			MAC: net.HardwareAddr{0x02, 0x00, 0xa9, 0xfe, 0x40, 0x01}, PodRange: home.PodRange}},
	}))
	g.Expect(c.TunnelPlan(home, []Node{own}).Regional).To(BeFalse())
}
