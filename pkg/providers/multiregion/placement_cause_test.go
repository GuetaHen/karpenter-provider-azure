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

package multiregion

import (
	"encoding/json"
	"testing"

	. "github.com/onsi/gomega"

	"github.com/Azure/karpenter-provider-azure/pkg/apis/v1beta1"
)

func TestPlacementDriftCauseSeparatesUnreadableFromUnlisted(t *testing.T) {
	g := NewWithT(t)
	listed := v1beta1.Region{Name: "westus2", VNETSubnetID: "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/virtualNetworks/vnet/subnets/nodes"}
	regions := []v1beta1.Region{listed}
	encode := func(region v1beta1.Region) string {
		raw, err := json.Marshal(region)
		g.Expect(err).ToNot(HaveOccurred())
		return string(raw)
	}
	for raw, expected := range map[string]string{
		encode(listed): "",
		encode(v1beta1.Region{Name: "eastus", VNETSubnetID: listed.VNETSubnetID}): PlacementCauseUnlisted,
		"{":                  PlacementCauseUnreadable,
		`{"name":"westus2"}`: PlacementCauseUnreadable,
	} {
		cause, reason := PlacementDriftCause(regions, raw)
		g.Expect(cause).To(Equal(expected), raw)
		g.Expect(reason == "").To(Equal(expected == ""), raw)
		g.Expect(PlacementDrift(regions, raw)).To(Equal(reason), raw)
	}
	cause, _ := PlacementDriftCause(regions, "")
	g.Expect(cause).To(Equal(PlacementCauseMissing))
	cause, _ = PlacementDriftCause(nil, encode(listed))
	g.Expect(cause).To(Equal(PlacementCauseRegionsRemoved))
}
