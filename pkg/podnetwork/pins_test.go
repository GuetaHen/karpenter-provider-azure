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
	"net/netip"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestPinsWithdrawANodeWhoseValuesChangeUntilItsNodeIsGone(t *testing.T) {
	g := NewWithT(t)
	pinned := Node{Name: "regional-1", UID: "uid-1", Role: Regional, Region: "westus2", VMID: "vm-1", IP: netip.MustParseAddr("10.1.0.4"),
		PodRange: netip.MustParsePrefix("10.244.3.0/24"), Index: 3}
	live := func(uid types.UID) []corev1.Node {
		return []corev1.Node{{ObjectMeta: metav1.ObjectMeta{Name: "regional-1", UID: uid}}}
	}
	// rejection applies the pins to an evaluation that accepted n and returns why they rejected it.
	rejection := func(pins *Pins, n Node, nodes []corev1.Node) string {
		eval := Evaluation{Accepted: map[string]Node{n.Name: n}, Rejected: map[string]string{}}
		pins.Apply(&eval, nodes)
		_, accepted := eval.Accepted[n.Name]
		g.Expect(accepted).To(Equal(eval.Rejected[n.Name] == ""))
		return eval.Rejected[n.Name]
	}
	for name, change := range map[string]func(*Node){
		"pod range": func(n *Node) { n.PodRange, n.Index = netip.MustParsePrefix("10.244.4.0/24"), 4 },
		"region":    func(n *Node) { n.Region = "eastus2" },
		"VM ID":     func(n *Node) { n.VMID = "vm-2" },
		"IP":        func(n *Node) { n.IP = netip.MustParseAddr("10.1.0.5") },
	} {
		pins := NewPins()
		g.Expect(rejection(pins, pinned, live("uid-1"))).To(BeEmpty(), name)
		changed := pinned
		change(&changed)
		g.Expect(rejection(pins, changed, live("uid-1"))).To(ContainSubstring(name+" changed"), name)
		g.Expect(rejection(pins, pinned, live("uid-1"))).To(ContainSubstring("withdrawn"), "%s: a withdrawn node stays withdrawn", name)
		pins.Apply(&Evaluation{Accepted: map[string]Node{}, Rejected: map[string]string{}}, nil)
		g.Expect(rejection(pins, changed, live("uid-1"))).To(BeEmpty(), "%s: the pin of a deleted Node is forgotten", name)
	}

	pins := NewPins()
	g.Expect(rejection(pins, pinned, live("uid-1"))).To(BeEmpty())
	home := pinned
	home.Role = Home
	g.Expect(rejection(pins, home, live("uid-1"))).To(BeEmpty(), "the role is not pinned: it follows the NodeClaim")
	replaced := pinned
	replaced.UID, replaced.VMID, replaced.IP = "uid-2", "vm-2", netip.MustParseAddr("10.1.0.6")
	g.Expect(rejection(pins, replaced, live("uid-2"))).To(BeEmpty(), "a replaced Node gets a pin of its own")
}
