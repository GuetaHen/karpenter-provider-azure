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
	"testing"

	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"
)

func renamed(it *cloudprovider.InstanceType, name string) *cloudprovider.InstanceType {
	it.Name = name
	it.Requirements[corev1.LabelInstanceTypeStable] = scheduling.NewRequirement(corev1.LabelInstanceTypeStable, corev1.NodeSelectorOpIn, name)
	return it
}

func TestInconsistentSKUExcludesOnlyThatRegionsOfferings(t *testing.T) {
	g := NewWithT(t)
	ctx := testContext(t)
	home, remote := testProviders("eastus2", .4), testProviders("westus2", .2)
	inconsistent := testType("westus2", .2)
	inconsistent.Capacity = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4")}
	remote.Types.(*testTypes).types = []*cloudprovider.InstanceType{inconsistent, renamed(testType("westus2", .3), "Standard_D4s_v5")}
	excluded := InconsistentOfferingsMetric.WithLabelValues("westus2", "Standard_D2s_v5", "capacity")
	before := testutil.ToFloat64(excluded)
	provider := &InstanceTypes{Provider: home.Types, Registry: New("eastus2", home, func(string) *Providers { return remote }, nil)}
	types, err := provider.List(ctx, testClass())
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(lo.Map(types, func(it *cloudprovider.InstanceType, _ int) string { return it.Name })).To(Equal([]string{"Standard_D2s_v5", "Standard_D4s_v5"}),
		"an inconsistent SKU removes neither other SKUs nor other regions")
	g.Expect(types[0].Offerings).To(HaveLen(1))
	g.Expect(types[0].Offerings[0].Requirements.Get(corev1.LabelTopologyRegion).Values()).To(Equal([]string{"eastus2"}))
	g.Expect(types[0].Requirements.Get(corev1.LabelTopologyRegion).Values()).To(Equal([]string{"eastus2"}))
	g.Expect(types[1].Offerings[0].Requirements.Get(corev1.LabelTopologyRegion).Values()).To(Equal([]string{"westus2"}))
	g.Expect(testutil.ToFloat64(excluded) - before).To(Equal(1.0))

	equivalent := testType("westus2", .2)
	equivalent.Capacity = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2000m")}
	remote.Types.(*testTypes).types = []*cloudprovider.InstanceType{equivalent}
	types, err = provider.List(ctx, testClass())
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(types).To(HaveLen(1))
	g.Expect(types[0].Offerings).To(HaveLen(2), "quantities compare semantically: 2000m CPUs equal 2 CPUs")
	g.Expect(testutil.ToFloat64(excluded) - before).To(Equal(1.0))
}

func TestMergedCatalogIsReusedUntilARegionChanges(t *testing.T) {
	g := NewWithT(t)
	ctx := testContext(t)
	home, remote := testProviders("eastus2", .4), testProviders("westus2", .2)
	provider := &InstanceTypes{Provider: home.Types, Registry: New("eastus2", home, func(string) *Providers { return remote }, nil)}
	first, err := provider.List(ctx, testClass())
	g.Expect(err).ToNot(HaveOccurred())
	merged := first[0]
	first[0] = nil
	second, err := provider.List(ctx, testClass())
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(second[0]).To(BeIdenticalTo(merged), "unchanged per-region results reuse the merged catalog, and each caller gets its own slice")
	remote.Types.(*testTypes).types = []*cloudprovider.InstanceType{testType("westus2", .1)}
	refreshed, err := provider.List(ctx, testClass())
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(refreshed[0]).ToNot(BeIdenticalTo(merged), "new per-region results rebuild the catalog")
	g.Expect(lo.Map(refreshed[0].Offerings, func(o *cloudprovider.Offering, _ int) float64 { return o.Price })).To(ConsistOf(.4, .1))
	reordered := testClass()
	reordered.Spec.Regions[0], reordered.Spec.Regions[1] = reordered.Spec.Regions[1], reordered.Spec.Regions[0]
	swapped, err := provider.List(ctx, reordered)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(swapped[0]).ToNot(BeIdenticalTo(refreshed[0]), "region order selects the reference region and is part of the key")
}
