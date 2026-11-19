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
	"testing"

	"github.com/google/nftables"
	"github.com/google/nftables/expr"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gstruct"
)

func TestFirewallRules(t *testing.T) {
	g := NewWithT(t)
	chainNames := func(chains []*nftables.Chain) []string {
		names := make([]string, 0, len(chains))
		for _, chain := range chains {
			names = append(names, chain.Name)
		}
		return names
	}

	// A home node drops tunnel traffic from outside its peers and clamps the MSS into the tunnel.
	table, chains, set, elements, rules := firewallRules(testConfig(t).TunnelPlan(homePeer, []Node{regionalPeer}))
	g.Expect(table).To(Equal(&nftables.Table{Family: nftables.TableFamilyIPv4, Name: TableName}))
	g.Expect(chainNames(chains)).To(Equal([]string{"input", "forward"}))
	g.Expect(chains[0]).To(gstruct.PointTo(And(HaveField("Type", nftables.ChainTypeFilter), HaveField("Hooknum", nftables.ChainHookInput))))
	g.Expect(chains[1]).To(gstruct.PointTo(And(HaveField("Type", nftables.ChainTypeFilter), HaveField("Hooknum", nftables.ChainHookForward))))
	g.Expect(set).To(gstruct.PointTo(And(HaveField("Name", "peers"), HaveField("KeyType", nftables.TypeIPAddr))))
	g.Expect(elements).To(Equal([]nftables.SetElement{{Key: []byte{10, 2, 0, 4}}}))
	g.Expect(rules).To(HaveLen(2))
	g.Expect(rules[0].Chain).To(Equal(chains[0]))
	g.Expect(rules[0].Exprs).To(Equal([]expr.Any{
		&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{17}},
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseTransportHeader, Offset: 2, Len: 2},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{0x12, 0xb5}},
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: 12, Len: 4},
		&expr.Lookup{SourceRegister: 1, SetName: "peers", Invert: true},
		&expr.Verdict{Kind: expr.VerdictDrop},
	}), "UDP to port 4789 from outside the peers")
	g.Expect(rules[1].Chain).To(Equal(chains[1]))
	g.Expect(rules[1].Exprs).To(Equal([]expr.Any{
		&expr.Meta{Key: expr.MetaKeyOIFNAME, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: bridgeIfname()},
		&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{6}},
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseTransportHeader, Offset: 13, Len: 1},
		&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 1, Mask: []byte{0x02}, Xor: []byte{0x00}},
		&expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: []byte{0x00}},
		&expr.Immediate{Register: 1, Data: []byte{0x05, 0x50}},
		&expr.Exthdr{SourceRegister: 1, Type: 2, Offset: 2, Len: 2, Op: expr.ExthdrOpTcpopt},
	}), "TCP MSS 1360 on SYNs into the tunnel")

	// A regional node also keeps pods from instance metadata and masquerades their other traffic.
	_, chains, _, elements, rules = firewallRules(testConfig(t).TunnelPlan(regionalOwn, []Node{homePeer}))
	g.Expect(chainNames(chains)).To(Equal([]string{"input", "forward", "postrouting"}))
	g.Expect(chains[2]).To(gstruct.PointTo(And(HaveField("Type", nftables.ChainTypeNAT), HaveField("Hooknum", nftables.ChainHookPostrouting),
		HaveField("Priority", nftables.ChainPriorityNATSource))))
	g.Expect(elements).To(Equal([]nftables.SetElement{{Key: []byte{10, 0, 0, 4}}}))
	g.Expect(rules).To(HaveLen(4))
	g.Expect(rules[2].Chain).To(Equal(chains[1]))
	g.Expect(rules[2].Exprs).To(Equal([]expr.Any{
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: 16, Len: 4},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{169, 254, 169, 254}},
		&expr.Verdict{Kind: expr.VerdictDrop},
	}), "forwarded traffic to instance metadata")
	g.Expect(rules[3].Chain).To(Equal(chains[2]))
	g.Expect(rules[3].Exprs).To(Equal([]expr.Any{
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: 12, Len: 4},
		&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 4, Mask: []byte{255, 255, 255, 0}, Xor: []byte{0, 0, 0, 0}},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{10, 244, 7, 0}},
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: 16, Len: 4},
		&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 4, Mask: []byte{255, 255, 0, 0}, Xor: []byte{0, 0, 0, 0}},
		&expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: []byte{10, 244, 0, 0}},
		&expr.Meta{Key: expr.MetaKeyOIFNAME, Register: 1},
		&expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: bridgeIfname()},
		&expr.Masq{},
	}), "pod traffic from the own range to neither the pod CIDR nor the tunnel")
}
