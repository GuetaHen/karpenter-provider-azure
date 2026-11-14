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

package status_test

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"

	"github.com/Azure/karpenter-provider-azure/pkg/apis/v1beta1"
	"github.com/Azure/karpenter-provider-azure/pkg/controllers/nodeclass/status"
	"github.com/Azure/karpenter-provider-azure/pkg/test"
)

type unloadedRegionError struct{}

func (unloadedRegionError) Error() string {
	return "region westus2 has no SKU catalog yet: SKU API unavailable"
}

func (unloadedRegionError) Reason() string {
	return "RegionalCatalogUnavailable"
}

func TestRegionalSubnetConditionNamesAnUnloadedRegion(t *testing.T) {
	g := NewWithT(t)
	nodeClass := test.AKSNodeClass()
	nodeClass.Spec.Regions = []v1beta1.Region{{
		Name:         "westus2",
		VNETSubnetID: "/subscriptions/00000000-0000-0000-0000-000000000001/resourceGroups/network/providers/Microsoft.Network/virtualNetworks/nodes/subnets/nodes",
	}}
	reconciler := status.NewSubnetReconciler(nil, func(context.Context, *v1beta1.AKSNodeClass) error { return unloadedRegionError{} })
	_, err := reconciler.Reconcile(t.Context(), nodeClass)
	g.Expect(err).To(HaveOccurred(), "an error requeues with backoff until the region loads")
	condition := nodeClass.StatusConditions().Get(v1beta1.ConditionTypeSubnetsReady)
	g.Expect(condition.IsFalse()).To(BeTrue())
	g.Expect(condition.Reason).To(Equal("RegionalCatalogUnavailable"))
	g.Expect(condition.Message).To(ContainSubstring("westus2"))
}
