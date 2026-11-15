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
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

// Pins remembers, for as long as the agent runs, the pod range, region, VM ID and IP of every node
// that it accepted, by Node UID. These values do not change during the life of a node, so a node
// whose values differ from its pin is withdrawn until its Node is deleted. Pins is not safe for
// concurrent use.
type Pins struct {
	pins map[types.UID]pin
}

type pin struct {
	node Node
	// withdrawn says how the values of the node changed, once they did.
	withdrawn string
}

// NewPins returns empty pins.
func NewPins() *Pins {
	return &Pins{pins: map[types.UID]pin{}}
}

// Apply forgets the pins of Nodes that no longer exist, pins every accepted node of eval that has
// no pin yet, and moves to eval.Rejected every accepted node that differs from its pin.
func (p *Pins) Apply(eval *Evaluation, nodes []corev1.Node) {
	live := make(map[types.UID]bool, len(nodes))
	for i := range nodes {
		live[nodes[i].UID] = true
	}
	for uid := range p.pins {
		if !live[uid] {
			delete(p.pins, uid)
		}
	}
	for name, n := range eval.Accepted {
		if err := p.admit(n); err != nil {
			delete(eval.Accepted, name)
			eval.Rejected[name] = err.Error()
		}
	}
}

// admit pins n when it has no pin, and returns an error when n differs from its pin, now or before.
func (p *Pins) admit(n Node) error {
	existing, ok := p.pins[n.UID]
	if !ok {
		p.pins[n.UID] = pin{node: n}
		return nil
	}
	if existing.withdrawn == "" {
		existing.withdrawn = changes(existing.node, n)
		p.pins[n.UID] = existing
	}
	if existing.withdrawn != "" {
		return fmt.Errorf("withdrawn until the Node is replaced: its %s", existing.withdrawn)
	}
	return nil
}

// changes describes how the values of a node differ from its pin, or returns "" when they do not.
func changes(pinned, current Node) string {
	var diffs []string
	if pinned.PodRange != current.PodRange {
		diffs = append(diffs, fmt.Sprintf("pod range changed from %s to %s", pinned.PodRange, current.PodRange))
	}
	if pinned.Region != current.Region {
		diffs = append(diffs, fmt.Sprintf("region changed from %s to %s", pinned.Region, current.Region))
	}
	if pinned.VMID != current.VMID {
		diffs = append(diffs, "VM ID changed")
	}
	if pinned.IP != current.IP {
		diffs = append(diffs, fmt.Sprintf("IP changed from %s to %s", pinned.IP, current.IP))
	}
	return strings.Join(diffs, ", ")
}
