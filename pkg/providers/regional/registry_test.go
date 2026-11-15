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
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v7"
	"github.com/Azure/karpenter-provider-azure/pkg/apis/v1beta1"
	"github.com/Azure/karpenter-provider-azure/pkg/consts"
	"github.com/Azure/karpenter-provider-azure/pkg/operator/options"
	"github.com/Azure/karpenter-provider-azure/pkg/providers/imagefamily"
	"github.com/Azure/karpenter-provider-azure/pkg/providers/instance"
	"github.com/Azure/karpenter-provider-azure/pkg/providers/instancetype"
	"github.com/Azure/karpenter-provider-azure/pkg/providers/quota"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	clocktesting "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"
)

type testTypes struct {
	instancetype.Provider
	types     []*cloudprovider.InstanceType
	err       error
	updateErr error
	updates   int
}

func (p *testTypes) List(context.Context, *v1beta1.AKSNodeClass) ([]*cloudprovider.InstanceType, error) {
	return p.types, p.err
}
func (p *testTypes) UpdateInstanceTypes(context.Context) error { p.updates++; return p.updateErr }

type testQuota struct {
	quota.Provider
	err           error
	calls, resets int
}

func (p *testQuota) Update(context.Context) error { p.calls++; return p.err }
func (p *testQuota) Reset()                       { p.resets++ }

type testImages struct{ images []imagefamily.NodeImage }

func (p *testImages) List(context.Context, *v1beta1.AKSNodeClass) ([]imagefamily.NodeImage, error) {
	return p.images, nil
}

type testVMs struct {
	instance.VMProvider
	vm             *armcompute.VirtualMachine
	calls, deletes int
	create         func(context.Context, *v1beta1.AKSNodeClass, *karpv1.NodeClaim, []*cloudprovider.InstanceType) error
}

func (p *testVMs) Get(context.Context, string) (*armcompute.VirtualMachine, error) {
	if p.vm == nil {
		return nil, cloudprovider.NewNodeClaimNotFoundError(errors.New("not found"))
	}
	return p.vm, nil
}

func (p *testVMs) BeginCreate(ctx context.Context, nc *v1beta1.AKSNodeClass, claim *karpv1.NodeClaim, types []*cloudprovider.InstanceType) (*instance.VirtualMachinePromise, error) {
	p.calls++
	if p.create != nil {
		if err := p.create(ctx, nc, claim, types); err != nil {
			return nil, err
		}
	}
	return &instance.VirtualMachinePromise{VM: p.vm, WaitFunc: func() error { return nil }}, nil
}

func (p *testVMs) Delete(context.Context, *karpv1.NodeClaim, string) error { p.deletes++; return nil }

func testClass() *v1beta1.AKSNodeClass {
	return &v1beta1.AKSNodeClass{Spec: v1beta1.AKSNodeClassSpec{
		ImageFamily: lo.ToPtr("Ubuntu"),
		Regions: []v1beta1.Region{
			{Name: "eastus2", VNETSubnetID: "/subscriptions/00000000-0000-0000-0000-000000000001/resourceGroups/east/providers/Microsoft.Network/virtualNetworks/nodes/subnets/nodes"},
			{Name: "westus2", VNETSubnetID: "/subscriptions/00000000-0000-0000-0000-000000000001/resourceGroups/west/providers/Microsoft.Network/virtualNetworks/nodes/subnets/nodes"},
		},
	}}
}

func testContext(t *testing.T) context.Context {
	t.Helper()
	return options.ToContext(t.Context(), &options.Options{ProvisionMode: consts.ProvisionModeAKSScriptless, NetworkPlugin: consts.NetworkPluginNone, EnableMultiRegion: true})
}

func testType(region string, price float64) *cloudprovider.InstanceType {
	return &cloudprovider.InstanceType{
		Name:     "Standard_D2s_v5",
		Capacity: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")},
		Overhead: &cloudprovider.InstanceTypeOverhead{},
		Requirements: scheduling.NewRequirements(
			scheduling.NewRequirement(corev1.LabelInstanceTypeStable, corev1.NodeSelectorOpIn, "Standard_D2s_v5"),
			scheduling.NewRequirement(corev1.LabelTopologyRegion, corev1.NodeSelectorOpIn, region),
			scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "0"),
			scheduling.NewRequirement(karpv1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, karpv1.CapacityTypeOnDemand),
			scheduling.NewRequirement(v1beta1.LabelPlacementScope, corev1.NodeSelectorOpIn, v1beta1.PlacementScopeRegional),
			scheduling.NewRequirement(v1beta1.LabelUltraSSD, corev1.NodeSelectorOpIn, "false"),
		),
		Offerings: cloudprovider.Offerings{{
			Available: true, Price: price,
			Requirements: scheduling.NewRequirements(
				scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "0"),
				scheduling.NewRequirement(karpv1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, karpv1.CapacityTypeOnDemand),
				scheduling.NewRequirement(v1beta1.LabelPlacementScope, corev1.NodeSelectorOpIn, v1beta1.PlacementScopeRegional),
				scheduling.NewRequirement(v1beta1.LabelUltraSSD, corev1.NodeSelectorOpIn, "false"),
			),
		}},
	}
}

func testProviders(region string, price float64) *Providers {
	return &Providers{
		Types:     &testTypes{types: []*cloudprovider.InstanceType{testType(region, price)}},
		Quota:     &testQuota{},
		Images:    &testImages{images: []imagefamily.NodeImage{{ID: "gallery/image/versions/1", Requirements: scheduling.NewRequirements()}}},
		Instances: &testVMs{},
	}
}

func TestRegionalOfferings(t *testing.T) {
	g := NewWithT(t)
	ctx := testContext(t)
	home, remote := testProviders("eastus2", .4), testProviders("westus2", .2)
	factoryCalls := 0
	registry := New("eastus2", home, func(string) *Providers { factoryCalls++; return remote }, nil)
	provider := &InstanceTypes{Provider: home.Types, Registry: registry}
	defaultClass := testClass()
	defaultClass.Spec.Regions = nil
	unmodified, err := provider.List(ctx, defaultClass)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(unmodified[0]).To(BeIdenticalTo(home.Types.(*testTypes).types[0]))
	g.Expect(factoryCalls).To(BeZero())
	g.Expect(home.Quota.(*testQuota).calls).To(BeZero())

	types, err := provider.List(ctx, testClass())
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(types).To(HaveLen(1), "one real SKU, not two virtual instance-type names")
	g.Expect(types[0].Name).To(Equal("Standard_D2s_v5"))
	g.Expect(types[0].Requirements.Get(corev1.LabelTopologyRegion).Values()).To(ConsistOf("eastus2", "westus2"))
	g.Expect(types[0].Offerings).To(HaveLen(2))
	for _, offering := range types[0].Offerings {
		region := offering.Requirements.Get(corev1.LabelTopologyRegion).Any()
		expected := .4
		if region == "westus2" {
			expected = .2
		}
		g.Expect(offering.Price).To(Equal(expected))
	}
	g.Expect(home.Types.(*testTypes).types[0].Offerings[0].Requirements.Has(corev1.LabelTopologyRegion)).To(BeFalse())
	west := narrow(types, "westus2")
	g.Expect(west[0].Offerings).To(HaveLen(1))
	g.Expect(west[0].Requirements.Get(corev1.LabelTopologyRegion).Values()).To(Equal([]string{"westus2"}))
	g.Expect(types[0].Offerings).To(HaveLen(2), "narrowing cannot mutate cached offerings")
	g.Expect(narrow(types, "centralus")).To(BeEmpty())
	g.Expect(factoryCalls).To(Equal(1))
}

func TestOmittedRegionsDoesNotHydrateRegionalState(t *testing.T) {
	g := NewWithT(t)
	ctx := testContext(t)
	home := testProviders("eastus2", .4)
	factoryCalls, networkCalls := 0, 0
	registry := New("eastus2", home, func(string) *Providers {
		factoryCalls++
		return testProviders("westus2", .2)
	}, func(context.Context, v1beta1.Region) error {
		networkCalls++
		return nil
	})
	nodeClass := testClass()
	nodeClass.Spec.Regions = nil
	types := &InstanceTypes{Provider: home.Types, Registry: registry}
	images := &Images{NodeImageProvider: home.Images, Registry: registry}
	// No Kubernetes client is needed on this path. An accidental placement read
	// would fail instead of being hidden by another fake.
	instances := &Instances{VMProvider: home.Instances, Registry: registry}
	claim := &karpv1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{corev1.LabelTopologyRegion: "eastus2"}}}
	for range 100 {
		listed, err := types.List(ctx, nodeClass)
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(listed[0]).To(BeIdenticalTo(home.Types.(*testTypes).types[0]))
		resolved, err := images.List(ctx, nodeClass)
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(&resolved[0]).To(BeIdenticalTo(&home.Images.(*testImages).images[0]))
		_, err = instances.BeginCreate(ctx, nodeClass, claim, listed)
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(instances.Delete(ctx, claim, "aks-claim")).To(Succeed())
		g.Expect(registry.ValidateSubnets(ctx, nodeClass)).To(Succeed())
	}
	g.Expect(home.Instances.(*testVMs).calls).To(Equal(100))
	g.Expect(home.Instances.(*testVMs).deletes).To(Equal(100))
	g.Expect(home.Quota.(*testQuota).calls).To(BeZero())
	g.Expect(factoryCalls).To(BeZero(), "regional caches and pricing workers are constructed only by this factory")
	g.Expect(networkCalls).To(BeZero())
	g.Expect(registry.entries).To(HaveLen(1), "only the pre-existing home provider is referenced")
}

func TestColdRegionFailsTheNodeClassInsteadOfServingAPartialCatalog(t *testing.T) {
	g := NewWithT(t)
	ctx := testContext(t)
	home, remote := testProviders("eastus2", .4), testProviders("westus2", .2)
	remoteTypes := remote.Types.(*testTypes)
	remoteTypes.updateErr = errors.New("SKU API unavailable")
	clk := clocktesting.NewFakeClock(time.Now())
	provider := &InstanceTypes{Provider: home.Types, Registry: New("eastus2", home, func(string) *Providers { return remote }, nil).WithClock(clk)}
	for range 2 {
		_, err := provider.List(ctx, testClass())
		g.Expect(err).To(MatchError(ContainSubstring("region westus2 has no SKU catalog yet: SKU API unavailable")))
	}
	g.Expect(remoteTypes.updates).To(Equal(1), "a cold region is retried at most once per backoff")
	remoteTypes.updateErr = nil
	clk.Step(retryBackoff)
	types, err := provider.List(ctx, testClass())
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(types[0].Requirements.Get(corev1.LabelTopologyRegion).Values()).To(ConsistOf("eastus2", "westus2"))
	g.Expect(home.Quota.(*testQuota).calls).To(BeZero(), "home-region controllers own home refreshes")
}

func TestHydratedRegionServesLastGoodDataWhenRefreshFails(t *testing.T) {
	g := NewWithT(t)
	ctx := testContext(t)
	home, remote := testProviders("eastus2", .4), testProviders("westus2", .2)
	remoteTypes, remoteQuota := remote.Types.(*testTypes), remote.Quota.(*testQuota)
	clk := clocktesting.NewFakeClock(time.Now())
	provider := &InstanceTypes{Provider: home.Types, Registry: New("eastus2", home, func(string) *Providers { return remote }, nil).WithClock(clk)}
	listedRegions := func() []string {
		types, err := provider.List(ctx, testClass())
		g.Expect(err).ToNot(HaveOccurred())
		return types[0].Requirements.Get(corev1.LabelTopologyRegion).Values()
	}
	g.Expect(listedRegions()).To(ConsistOf("eastus2", "westus2"))
	remoteTypes.updateErr = errors.New("SKU API unavailable")
	remoteQuota.err = errors.New("quota API unavailable")
	clk.Step(quotaRefreshInterval)
	g.Expect(listedRegions()).To(ConsistOf("eastus2", "westus2"))
	g.Expect(remoteQuota.calls).To(Equal(2))
	g.Expect(remoteQuota.resets).To(BeZero(), "quota younger than the staleness limit is still served")
	clk.Step(quotaMaxStaleness - quotaRefreshInterval)
	g.Expect(listedRegions()).To(ConsistOf("eastus2", "westus2"))
	g.Expect(remoteQuota.resets).To(Equal(1), "stale quota fails open like the home region")
	clk.Step(skuRefreshInterval)
	g.Expect(listedRegions()).To(ConsistOf("eastus2", "westus2"), "a failed SKU refresh keeps the last successful catalog")
	g.Expect(remoteTypes.updates).To(Equal(2))
	g.Expect(home.Quota.(*testQuota).calls).To(BeZero())
}

func TestConcurrentRegionalHydration(t *testing.T) {
	g := NewWithT(t)
	ctx := testContext(t)
	home, remote := testProviders("eastus2", .4), testProviders("westus2", .2)
	factoryCalls := 0
	registry := New("eastus2", home, func(string) *Providers {
		factoryCalls++
		return remote
	}, nil)
	results := make(chan error, 16)
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := registry.get(ctx, "westus2")
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	for err := range results {
		g.Expect(err).ToNot(HaveOccurred())
	}
	g.Expect(factoryCalls).To(Equal(1))
	g.Expect(remote.Quota.(*testQuota).calls).To(Equal(1))
	g.Expect(home.Quota.(*testQuota).calls).To(BeZero())
}

func TestRegionalImages(t *testing.T) {
	g := NewWithT(t)
	home, remote := testProviders("eastus2", .4), testProviders("westus2", .2)
	remote.Images.(*testImages).images[0].ID = "gallery/image/versions/2"
	registry := New("eastus2", home, func(string) *Providers { return remote }, nil)
	images, err := (&Images{NodeImageProvider: home.Images, Registry: registry}).List(testContext(t), testClass())
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(images).To(HaveLen(2))
	g.Expect(images[0].Requirements.Get(corev1.LabelTopologyRegion).Any()).To(Equal("eastus2"))
	g.Expect(images[1].Requirements.Get(corev1.LabelTopologyRegion).Any()).To(Equal("westus2"))
	g.Expect(images[1].ID).To(Equal("gallery/image/versions/2"))
	g.Expect(home.Images.(*testImages).images[0].Requirements).To(BeEmpty())
}

func TestPlacementPersistsBeforeLaunchAndSurvivesRetry(t *testing.T) {
	g := NewWithT(t)
	ctx := testContext(t)
	home, remote := testProviders("eastus2", .4), testProviders("westus2", .2)
	registry := New("eastus2", home, func(string) *Providers { return remote }, func(context.Context, v1beta1.Region) error { return nil })
	claim := &karpv1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Name: "regional-claim", UID: "synthetic-uid"}}
	kube := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(claim).Build()
	vm := &Instances{VMProvider: home.Instances, Registry: registry, KubeClient: kube}
	types, err := (&InstanceTypes{Provider: home.Types, Registry: registry}).List(ctx, testClass())
	g.Expect(err).ToNot(HaveOccurred())
	remote.Instances.(*testVMs).create = func(ctx context.Context, nc *v1beta1.AKSNodeClass, _ *karpv1.NodeClaim, candidates []*cloudprovider.InstanceType) error {
		current := &karpv1.NodeClaim{}
		g.Expect(kube.Get(ctx, client.ObjectKeyFromObject(claim), current)).To(Succeed())
		g.Expect(current.Annotations[PlacementAnnotation]).To(ContainSubstring("westus2"))
		g.Expect(*nc.Spec.VNETSubnetID).To(Equal(testClass().Spec.Regions[1].VNETSubnetID))
		g.Expect(candidates[0].Requirements.Get(corev1.LabelTopologyRegion).Values()).To(Equal([]string{"westus2"}))
		return errors.New("simulated failure after placement")
	}
	_, err = vm.BeginCreate(ctx, testClass(), claim, types)
	g.Expect(err).To(MatchError("simulated failure after placement"))
	for _, offering := range types[0].Offerings {
		if offering.Requirements.Get(corev1.LabelTopologyRegion).Has("eastus2") {
			offering.Price = .01
		}
	}
	_, err = vm.BeginCreate(ctx, testClass(), claim, types)
	g.Expect(err).To(MatchError("simulated failure after placement"))
	g.Expect(remote.Instances.(*testVMs).calls).To(Equal(2))
	g.Expect(home.Instances.(*testVMs).calls).To(BeZero())
	g.Expect(claim.Annotations).To(BeNil(), "caller's stale object is not mutated")
	g.Expect(testClass().Spec.VNETSubnetID).To(BeNil())

	changed := testClass()
	changed.Spec.Regions[1].VNETSubnetID += "-changed"
	_, err = vm.BeginCreate(ctx, changed, claim, types)
	g.Expect(err).To(MatchError(ContainSubstring("no longer matches")))
	g.Expect(remote.Instances.(*testVMs).calls).To(Equal(2))
}

func TestPlacementConflictStopsBeforeAzureMutation(t *testing.T) {
	g := NewWithT(t)
	ctx := testContext(t)
	home, remote := testProviders("eastus2", .4), testProviders("westus2", .2)
	registry := New("eastus2", home, func(string) *Providers { return remote }, func(context.Context, v1beta1.Region) error { return nil })
	claim := &karpv1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Name: "conflicted-claim", UID: "synthetic-uid"}}
	kube := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(claim).WithInterceptorFuncs(interceptor.Funcs{
		Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
			return errors.New("optimistic conflict")
		},
	}).Build()
	types, err := (&InstanceTypes{Provider: home.Types, Registry: registry}).List(ctx, testClass())
	g.Expect(err).ToNot(HaveOccurred())
	_, err = (&Instances{VMProvider: home.Instances, Registry: registry, KubeClient: kube}).BeginCreate(ctx, testClass(), claim, types)
	g.Expect(err).To(MatchError(ContainSubstring("persisting regional placement")))
	g.Expect(home.Instances.(*testVMs).calls + remote.Instances.(*testVMs).calls).To(BeZero())
}

func TestRegionalLaunchRequiresThePodNetworkStartupTaint(t *testing.T) {
	g := NewWithT(t)
	const key = "karpenter.azure.com/regional-pod-network-not-ready"
	ctx := options.ToContext(t.Context(), &options.Options{ProvisionMode: consts.ProvisionModeAKSScriptless, NetworkPlugin: consts.NetworkPluginAzure,
		NetworkPluginMode: consts.NetworkPluginModeOverlay, EnableMultiRegion: true, MultiRegionExternalPodNetwork: true, MultiRegionPodNetworkStartupTaint: key})
	home, remote := testProviders("eastus2", .4), testProviders("westus2", .2)
	registry := New("eastus2", home, func(string) *Providers { return remote }, func(context.Context, v1beta1.Region) error { return nil })
	claim := &karpv1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Name: "regional-claim", UID: "synthetic-uid"}}
	kube := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(claim).Build()
	vm := &Instances{VMProvider: home.Instances, Registry: registry, KubeClient: kube}
	types, err := (&InstanceTypes{Provider: home.Types, Registry: registry}).List(ctx, testClass())
	g.Expect(err).ToNot(HaveOccurred())
	for _, spec := range []karpv1.NodeClaimSpec{
		{},
		{StartupTaints: []corev1.Taint{{Key: key, Effect: corev1.TaintEffectPreferNoSchedule}}},
		{StartupTaints: []corev1.Taint{{Key: "example.com/other", Effect: corev1.TaintEffectNoSchedule}}},
		{Taints: []corev1.Taint{{Key: key, Effect: corev1.TaintEffectNoSchedule}}},
	} {
		claim.Spec = spec
		_, err = vm.BeginCreate(ctx, testClass(), claim, types)
		g.Expect(err).To(MatchError(ContainSubstring(key)))
	}
	stored := &karpv1.NodeClaim{}
	g.Expect(kube.Get(ctx, client.ObjectKeyFromObject(claim), stored)).To(Succeed())
	g.Expect(stored.Annotations).ToNot(HaveKey(PlacementAnnotation), "the check runs before any write")
	g.Expect(home.Instances.(*testVMs).calls + remote.Instances.(*testVMs).calls).To(BeZero())

	for i, accepted := range []*karpv1.NodeClaim{
		{ObjectMeta: metav1.ObjectMeta{Name: "no-schedule-claim", UID: "no-schedule-uid"}, Spec: karpv1.NodeClaimSpec{StartupTaints: []corev1.Taint{{Key: key, Effect: corev1.TaintEffectNoSchedule}}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "no-execute-claim", UID: "no-execute-uid"}, Spec: karpv1.NodeClaimSpec{StartupTaints: []corev1.Taint{{Key: key, Effect: corev1.TaintEffectNoExecute}}}},
	} {
		g.Expect(kube.Create(ctx, accepted)).To(Succeed())
		_, err = vm.BeginCreate(ctx, testClass(), accepted, types)
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(remote.Instances.(*testVMs).calls).To(Equal(i+1), accepted.Name)
	}

	homeClass := testClass()
	homeClass.Spec.Regions = nil
	_, err = vm.BeginCreate(ctx, homeClass, &karpv1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Name: "home-claim"}}, types)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(home.Instances.(*testVMs).calls).To(Equal(1), "home NodeClaims need no startup taint")
}

func TestDeleteDoesNotRequireRegionConfigurationOrRefresh(t *testing.T) {
	g := NewWithT(t)
	home, remote := testProviders("eastus2", .4), testProviders("westus2", .2)
	remote.Quota.(*testQuota).err = errors.New("quota unavailable")
	registry := New("eastus2", home, func(string) *Providers { return remote }, func(context.Context, v1beta1.Region) error { return errors.New("subnet deleted") })
	claim := &karpv1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{corev1.LabelTopologyRegion: "westus2"}}}
	g.Expect((&Instances{VMProvider: home.Instances, Registry: registry}).Delete(testContext(t), claim, "aks-claim")).To(Succeed())
	g.Expect(remote.Instances.(*testVMs).deletes).To(Equal(1))
	g.Expect(home.Instances.(*testVMs).deletes).To(BeZero())
	g.Expect(remote.Quota.(*testQuota).calls).To(BeZero())
}
