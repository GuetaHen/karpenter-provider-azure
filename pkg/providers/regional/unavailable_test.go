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
	"context"
	"encoding/json"
	"errors"
	"testing"

	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/Azure/karpenter-provider-azure/pkg/apis/v1beta1"
	"github.com/Azure/karpenter-provider-azure/pkg/providers/imagefamily"
)

type failingImages struct {
	imagefamily.NodeImageProvider
	err error
}

func (p *failingImages) List(context.Context, *v1beta1.AKSNodeClass) ([]imagefamily.NodeImage, error) {
	return nil, p.err
}

func regionImage(id, region string) v1beta1.NodeImage {
	return v1beta1.NodeImage{ID: id, Requirements: []corev1.NodeSelectorRequirement{{Key: corev1.LabelTopologyRegion, Operator: corev1.NodeSelectorOpIn, Values: []string{region}}}}
}

func TestAFailingRegionGetsNoNewNodesWithoutBlockingTheOthers(t *testing.T) {
	g := NewWithT(t)
	ctx := testContext(t)
	home, remote := testProviders("eastus2", .4), testProviders("westus2", .2)
	subnetErr := errors.New("subnet not found")
	failing := subnetErr
	registry := New("eastus2", home, func(string) *Providers { return remote }, func(_ context.Context, region v1beta1.Region) error {
		return lo.Ternary(region.Name == "westus2", failing, nil)
	}).WithKubeClient(fake.NewClientBuilder().WithScheme(scheme.Scheme).Build())
	provider := &InstanceTypes{Provider: home.Types, Registry: registry}
	nodeClass := testClass()

	err := registry.ValidateSubnets(ctx, nodeClass)
	g.Expect(err).To(MatchError(subnetErr))
	var partial interface {
		Reason() string
		UnavailableRegions() []string
	}
	g.Expect(errors.As(err, &partial)).To(BeTrue())
	g.Expect(partial.Reason()).To(Equal(RegionsUnavailableReason))
	g.Expect(partial.UnavailableRegions()).To(ConsistOf("westus2"))

	types, err := provider.List(ctx, nodeClass)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(types[0].Offerings).To(HaveLen(2), "the failing region keeps its instance type for core drift")
	for _, offering := range types[0].Offerings {
		region := offering.Requirements.Get(corev1.LabelTopologyRegion).Any()
		g.Expect(offering.Available).To(Equal(region == "eastus2"), region)
	}
	again, err := provider.List(ctx, nodeClass)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(again[0]).To(BeIdenticalTo(types[0]), "the merged catalog stays cached")
	g.Expect(remote.Types.(*testTypes).types[0].Offerings[0].Available).To(BeTrue(), "the region's own catalog is unchanged")

	failing = nil
	g.Expect(registry.ValidateSubnets(ctx, nodeClass)).To(Succeed())
	types, err = provider.List(ctx, nodeClass)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(types[0].Offerings.Available()).To(HaveLen(2), "the region serves again once it passes")
}

func TestANeverLoadedRegionFailsClosedOnlyWhileItHasNodeClaims(t *testing.T) {
	g := NewWithT(t)
	ctx := testContext(t)
	home, remote := testProviders("eastus2", .4), testProviders("westus2", .2)
	remote.Types.(*testTypes).updateErr = errors.New("SKU API unavailable")
	nodeClass := testClass()
	nodeClass.Name = "regional"
	placed := &karpv1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "placed", Annotations: map[string]string{
			PlacementAnnotation: string(lo.Must(json.Marshal(nodeClass.Spec.Regions[1]))),
		}},
		Spec: karpv1.NodeClaimSpec{NodeClassRef: &karpv1.NodeClassReference{Name: nodeClass.Name}},
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(placed).Build()
	registry := New("eastus2", home, func(string) *Providers { return remote }, func(context.Context, v1beta1.Region) error { return nil }).WithKubeClient(kubeClient)
	provider := &InstanceTypes{Provider: home.Types, Registry: registry}
	var reasoned interface{ Reason() string }

	err := registry.ValidateSubnets(ctx, nodeClass)
	g.Expect(errors.As(err, &reasoned)).To(BeTrue())
	g.Expect(reasoned.Reason()).To(Equal(RegionNotLoadedReason), "core drift would not find the placed NodeClaim's instance type")
	_, err = provider.List(ctx, nodeClass)
	g.Expect(err).To(MatchError(ContainSubstring("region westus2 has no SKU catalog yet")))

	g.Expect(kubeClient.Delete(ctx, placed)).To(Succeed())
	err = registry.ValidateSubnets(ctx, nodeClass)
	g.Expect(errors.As(err, &reasoned)).To(BeTrue())
	g.Expect(reasoned.Reason()).To(Equal(RegionsUnavailableReason))
	types, err := provider.List(ctx, nodeClass)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(types[0].Requirements.Get(corev1.LabelTopologyRegion).Values()).To(ConsistOf("eastus2"), "a region without NodeClaims is left out")
}

func TestImagesLeaveOutAFailingRegionThatHasNoImagesYet(t *testing.T) {
	g := NewWithT(t)
	ctx := testContext(t)
	home, remote := testProviders("eastus2", .4), testProviders("westus2", .2)
	remote.Images = &failingImages{err: errors.New("gallery unavailable")}
	registry := New("eastus2", home, func(string) *Providers { return remote }, func(context.Context, v1beta1.Region) error { return nil })
	images := &Images{NodeImageProvider: home.Images, Registry: registry}
	nodeClass := testClass()

	listed, err := images.List(ctx, nodeClass)
	g.Expect(err).ToNot(HaveOccurred(), "the other regions' images still refresh")
	g.Expect(listed).To(HaveLen(1))
	g.Expect(listed[0].Requirements.Get(corev1.LabelTopologyRegion).Values()).To(ConsistOf("eastus2"))
	nodeClass.Status.Images = []v1beta1.NodeImage{regionImage(listed[0].ID, "eastus2")}
	err = registry.ValidateSubnets(ctx, nodeClass)
	g.Expect(err).To(MatchError(ContainSubstring("region westus2 has no node images")))
	var partial interface{ UnavailableRegions() []string }
	g.Expect(errors.As(err, &partial)).To(BeTrue())
	g.Expect(partial.UnavailableRegions()).To(ConsistOf("westus2"))

	nodeClass.Status.Images = append(nodeClass.Status.Images, regionImage("gallery/image/versions/0", "westus2"))
	_, err = images.List(ctx, nodeClass)
	g.Expect(err).To(MatchError(ContainSubstring("region westus2 images")), "a region with images in use keeps them through a failure")

	home.Images = &failingImages{err: errors.New("gallery unavailable")}
	nodeClass.Status.Images = nil
	_, err = images.List(ctx, nodeClass)
	g.Expect(err).To(MatchError(ContainSubstring("gallery unavailable")), "a NodeClass with no region left fails")
}
