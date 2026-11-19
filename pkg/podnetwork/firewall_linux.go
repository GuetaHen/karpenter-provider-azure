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
	"net"

	"github.com/google/nftables"
	"github.com/google/nftables/binaryutil"
	"github.com/google/nftables/expr"
	"golang.org/x/sys/unix"
)

// TableName is the nftables table of the agent.
const TableName = "karpenter_pod_network"

// nftablesFirewall programs the firewall of the agent as one nftables table, which it replaces as a
// whole in one transaction.
type nftablesFirewall struct {
	conn *nftables.Conn
}

// Apply replaces the table of the agent with the rules for plan.
func (f *nftablesFirewall) Apply(plan Plan) error {
	table, chains, peers, elements, rules := firewallRules(plan)
	// Adding the table first lets the deletion succeed whether or not the table exists.
	f.conn.AddTable(table)
	f.conn.DelTable(table)
	f.conn.AddTable(table)
	for _, chain := range chains {
		f.conn.AddChain(chain)
	}
	if err := f.conn.AddSet(peers, elements); err != nil {
		return err
	}
	for _, rule := range rules {
		f.conn.AddRule(rule)
	}
	return f.conn.Flush()
}

// firewallRules returns the table of the agent for plan:
//   - input: UDP to the tunnel port only from the node IPs of the peers;
//   - forward: the TCP MSS of SYNs and SYN-ACKs into the tunnel and, on a regional node, a drop of
//     forwarded traffic to instance metadata;
//   - postrouting, on a regional node only: masquerade of pod traffic that leaves through neither
//     the tunnel nor to the pod CIDR.
func firewallRules(plan Plan) (*nftables.Table, []*nftables.Chain, *nftables.Set, []nftables.SetElement, []*nftables.Rule) {
	table := &nftables.Table{Family: nftables.TableFamilyIPv4, Name: TableName}
	input := &nftables.Chain{Name: "input", Table: table, Type: nftables.ChainTypeFilter, Hooknum: nftables.ChainHookInput, Priority: nftables.ChainPriorityFilter}
	forward := &nftables.Chain{Name: "forward", Table: table, Type: nftables.ChainTypeFilter, Hooknum: nftables.ChainHookForward, Priority: nftables.ChainPriorityFilter}
	chains := []*nftables.Chain{input, forward}
	peers := &nftables.Set{Table: table, Name: "peers", KeyType: nftables.TypeIPAddr}
	elements := make([]nftables.SetElement, 0, len(plan.Peers))
	for _, peer := range plan.Peers {
		elements = append(elements, nftables.SetElement{Key: peer.NodeIP.AsSlice()})
	}
	rules := []*nftables.Rule{
		{Table: table, Chain: input, Exprs: tunnelFilter(peers.Name)},
		{Table: table, Chain: forward, Exprs: mssClamp()},
	}
	if plan.Regional {
		postrouting := &nftables.Chain{Name: "postrouting", Table: table, Type: nftables.ChainTypeNAT, Hooknum: nftables.ChainHookPostrouting, Priority: nftables.ChainPriorityNATSource}
		chains = append(chains, postrouting)
		rules = append(rules,
			&nftables.Rule{Table: table, Chain: forward, Exprs: metadataDrop()},
			&nftables.Rule{Table: table, Chain: postrouting, Exprs: masquerade(plan)})
	}
	return table, chains, peers, elements, rules
}

// tunnelFilter drops UDP to the tunnel port from addresses outside the set of peers.
func tunnelFilter(set string) []expr.Any {
	return []expr.Any{
		&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{unix.IPPROTO_UDP}},
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseTransportHeader, Offset: 2, Len: 2},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: binaryutil.BigEndian.PutUint16(TunnelPort)},
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: 12, Len: 4},
		&expr.Lookup{SourceRegister: 1, SetName: set, Invert: true},
		&expr.Verdict{Kind: expr.VerdictDrop},
	}
}

// mssClamp lowers the TCP MSS option of SYNs and SYN-ACKs that leave through the bridge into the
// tunnel to TunnelMSS; the kernel never raises an MSS option. Pods advertise the MSS of the route MTU
// that cilium-cni gives them, which is more than the tunnel takes, so this keeps the TCP segments of
// both directions within TunnelMTU. Pod traffic reaches the forward hook because the agent requires
// legacy host routing, under which Cilium hands it to the routes of the kernel instead of redirecting
// it in eBPF.
func mssClamp() []expr.Any {
	return []expr.Any{
		&expr.Meta{Key: expr.MetaKeyOIFNAME, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: bridgeIfname()},
		&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{unix.IPPROTO_TCP}},
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseTransportHeader, Offset: 13, Len: 1},
		&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 1, Mask: []byte{0x02}, Xor: []byte{0x00}},
		&expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: []byte{0x00}},
		&expr.Immediate{Register: 1, Data: binaryutil.BigEndian.PutUint16(TunnelMSS)},
		&expr.Exthdr{SourceRegister: 1, Type: 2, Offset: 2, Len: 2, Op: expr.ExthdrOpTcpopt},
	}
}

// metadataDrop drops forwarded traffic to instance metadata, so that pods cannot reach it.
func metadataDrop() []expr.Any {
	return []expr.Any{
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: 16, Len: 4},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: instanceMetadataAddress.AsSlice()},
		&expr.Verdict{Kind: expr.VerdictDrop},
	}
}

// masquerade translates the source of traffic from the node's pod range to the node IP, unless it
// goes to the pod CIDR or into the tunnel.
func masquerade(plan Plan) []expr.Any {
	return []expr.Any{
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: 12, Len: 4},
		&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 4, Mask: net.CIDRMask(plan.PodRange.Bits(), 32), Xor: make([]byte, 4)},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: plan.PodRange.Addr().AsSlice()},
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: 16, Len: 4},
		&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 4, Mask: net.CIDRMask(plan.PodCIDR.Bits(), 32), Xor: make([]byte, 4)},
		&expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: plan.PodCIDR.Addr().AsSlice()},
		&expr.Meta{Key: expr.MetaKeyOIFNAME, Register: 1},
		&expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: bridgeIfname()},
		&expr.Masq{},
	}
}

// bridgeIfname returns the name of the bridge, padded to the length of an nftables interface name.
func bridgeIfname() []byte {
	b := make([]byte, unix.IFNAMSIZ)
	copy(b, BridgeName)
	return b
}
