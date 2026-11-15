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
	"fmt"
	"hash/fnv"
	"net"
	"net/netip"
	"strings"
)

const (
	// DefaultTransitPrefix holds one transit address for every /24 of a pod CIDR up to /10.
	DefaultTransitPrefix = "169.254.64.0/18"
	// TunnelPort is the UDP port of the VXLAN tunnel.
	TunnelPort = 4789
	// TunnelMTU is the MTU of the tunnel devices and of the pods on regional nodes.
	TunnelMTU = 1400
	// TunnelMSS is the TCP MSS that the agent sets on SYNs into the tunnel.
	TunnelMSS = 1360
)

var (
	linkLocal               = netip.MustParsePrefix("169.254.0.0/16")
	instanceMetadataAddress = netip.MustParseAddr("169.254.169.254")
)

// validateTransit checks the transit prefix. It must lie in IPv4 link-local space without host bits
// and without the instance metadata address, and hold one address for every /24 of the pod CIDR,
// because the bridge of each node holds its address as a /32.
func (c *Config) validateTransit() error {
	p := c.TransitPrefix
	switch {
	case !p.IsValid() || !p.Addr().Is4() || p.Masked() != p || p.Bits() < linkLocal.Bits() || !linkLocal.Contains(p.Addr()):
		return fmt.Errorf("the transit prefix %s must be an IPv4 prefix inside %s without host bits", p, linkLocal)
	case p.Contains(instanceMetadataAddress):
		return fmt.Errorf("the transit prefix %s must not contain the instance metadata address %s", p, instanceMetadataAddress)
	case p.Bits() > c.PodCIDR.Bits()+8:
		return fmt.Errorf("the transit prefix %s is too small for the pod CIDR %s: its length must be %d or less", p, c.PodCIDR, c.PodCIDR.Bits()+8)
	}
	return nil
}

// TransitAddress returns the transit address of the node whose pod range has the given index: the
// address of the transit prefix at that index, counting from 0.
func (c *Config) TransitAddress(index uint32) netip.Addr {
	base := c.TransitPrefix.Addr().As4()
	var addr [4]byte
	binary.BigEndian.PutUint32(addr[:], binary.BigEndian.Uint32(base[:])+index)
	return netip.AddrFrom4(addr)
}

// TransitMAC returns the MAC address of the bridge that holds a transit address: 02:00, which is
// locally administered and unicast, followed by the four bytes of the address.
func TransitMAC(addr netip.Addr) net.HardwareAddr {
	b := addr.As4()
	return net.HardwareAddr{0x02, 0x00, b[0], b[1], b[2], b[3]}
}

// VNI returns the VXLAN network identifier of the cluster. It derives from the node resource group
// ID, so that the tunnels of clusters on peered networks stay apart.
func (c *Config) VNI() uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(strings.ToLower(c.nodeResourceGroup.String())))
	return foldVNI(h.Sum32())
}

// foldVNI folds a 32-bit hash into the 24 bits of a VXLAN network identifier, which is never 0.
func foldVNI(sum uint32) uint32 {
	if vni := (sum >> 24) ^ (sum & 0xffffff); vni != 0 {
		return vni
	}
	return 1
}
