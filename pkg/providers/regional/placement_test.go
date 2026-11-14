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

package regional

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Azure/karpenter-provider-azure/pkg/apis/v1beta1"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
)

func TestPlacementDriftOnlyForRemovedPlacements(t *testing.T) {
	east, west := testClass().Spec.Regions[0], testClass().Spec.Regions[1]
	encoded, err := json.Marshal(west)
	NewWithT(t).Expect(err).ToNot(HaveOccurred())
	raw := string(encoded)
	recased := v1beta1.Region{Name: west.Name, VNETSubnetID: strings.ToUpper(west.VNETSubnetID)}
	moved := v1beta1.Region{Name: west.Name, VNETSubnetID: west.VNETSubnetID + "-new"}
	central := v1beta1.Region{Name: "centralus", VNETSubnetID: east.VNETSubnetID + "-central"}
	for _, tc := range []struct {
		name    string
		regions []v1beta1.Region
		raw     string
		drifted bool
	}{
		{name: "single-region NodeClass and NodeClaim"},
		{name: "placement still listed", regions: []v1beta1.Region{east, west}, raw: raw},
		{name: "another region added", regions: []v1beta1.Region{east, west, central}, raw: raw},
		{name: "regions reordered", regions: []v1beta1.Region{west, east}, raw: raw},
		{name: "another region removed", regions: []v1beta1.Region{west}, raw: raw},
		{name: "placement subnet recased", regions: []v1beta1.Region{east, recased}, raw: raw},
		{name: "placement region removed", regions: []v1beta1.Region{east}, raw: raw, drifted: true},
		{name: "placement subnet changed", regions: []v1beta1.Region{east, moved}, raw: raw, drifted: true},
		{name: "regions removed from the NodeClass", raw: raw, drifted: true},
		{name: "regions added for a single-region NodeClaim", regions: []v1beta1.Region{east, west}, drifted: true},
		{name: "unreadable placement", regions: []v1beta1.Region{east, west}, raw: "{", drifted: true},
		{name: "incomplete placement", regions: []v1beta1.Region{east, west}, raw: `{"name":"westus2"}`, drifted: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			NewWithT(t).Expect(PlacementDrift(tc.regions, tc.raw) != "").To(Equal(tc.drifted), PlacementDrift(tc.regions, tc.raw))
		})
	}
}

func TestImageMatchesRegionDoesNotAllocate(t *testing.T) {
	g := NewWithT(t)
	arch := corev1.NodeSelectorRequirement{Key: corev1.LabelArchStable, Operator: corev1.NodeSelectorOpIn, Values: []string{"amd64"}}
	region := corev1.NodeSelectorRequirement{Key: corev1.LabelTopologyRegion, Operator: corev1.NodeSelectorOpIn, Values: []string{"eastus2"}}
	unscoped := []corev1.NodeSelectorRequirement{arch}
	scoped := []corev1.NodeSelectorRequirement{arch, region}
	g.Expect(ImageMatchesRegion(unscoped, "westus2")).To(BeTrue())
	g.Expect(ImageMatchesRegion(scoped, "eastus2")).To(BeTrue())
	g.Expect(ImageMatchesRegion(scoped, "westus2")).To(BeFalse())
	g.Expect(testing.AllocsPerRun(100, func() { ImageMatchesRegion(unscoped, "eastus2") })).To(BeZero())
	g.Expect(testing.AllocsPerRun(100, func() { ImageMatchesRegion(scoped, "westus2") })).To(BeZero())
}
