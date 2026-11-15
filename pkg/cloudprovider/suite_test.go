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

package cloudprovider

// TODO v1beta1 extra refactor into suite_test.go / cloudprovider_test.go
import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v7"
	. "github.com/Azure/karpenter-provider-azure/pkg/test/expectations"
	"github.com/Azure/skewer"
	"github.com/awslabs/operatorpkg/object"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubernetesfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/record"
	clock "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	autoscalingv1beta1 "sigs.k8s.io/karpenter/pkg/apis/autoscaling/v1beta1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	karpv1alpha1 "sigs.k8s.io/karpenter/pkg/apis/v1alpha1"
	corecloudprovider "sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/controllers/capacitybuffer"
	"sigs.k8s.io/karpenter/pkg/controllers/dynamicresources/deviceallocation"
	"sigs.k8s.io/karpenter/pkg/controllers/node/health"
	"sigs.k8s.io/karpenter/pkg/controllers/nodeoverlay"
	"sigs.k8s.io/karpenter/pkg/controllers/provisioning"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/events"
	"sigs.k8s.io/karpenter/pkg/metrics"
	coreoptions "sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/scheduling"
	"sigs.k8s.io/karpenter/pkg/state/virtualpods"
	coretest "sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
	"sigs.k8s.io/karpenter/pkg/test/v1alpha1"
	. "sigs.k8s.io/karpenter/pkg/utils/testing"

	"github.com/Azure/karpenter-provider-azure/pkg/apis"
	"github.com/Azure/karpenter-provider-azure/pkg/apis/v1beta1"
	"github.com/Azure/karpenter-provider-azure/pkg/auth"
	"github.com/Azure/karpenter-provider-azure/pkg/consts"
	"github.com/Azure/karpenter-provider-azure/pkg/controllers/nodeclaim/garbagecollection"
	"github.com/Azure/karpenter-provider-azure/pkg/controllers/nodeclass/status"
	"github.com/Azure/karpenter-provider-azure/pkg/fake"
	"github.com/Azure/karpenter-provider-azure/pkg/operator/options"
	"github.com/Azure/karpenter-provider-azure/pkg/providers/imagefamily"
	"github.com/Azure/karpenter-provider-azure/pkg/providers/instance"
	"github.com/Azure/karpenter-provider-azure/pkg/providers/regional"
	"github.com/Azure/karpenter-provider-azure/pkg/test"
	"github.com/Azure/karpenter-provider-azure/pkg/utils/zones"
	corestatus "github.com/awslabs/operatorpkg/status"
)

var ctx context.Context
var testOptions *options.Options
var stop context.CancelFunc
var env *coretest.Environment
var azureEnv *test.Environment
var azureEnvNonZonal *test.Environment
var coreProvisioner *provisioning.Provisioner
var coreProvisionerNonZonal *provisioning.Provisioner
var cloudProvider *CloudProvider
var cloudProviderNonZonal *CloudProvider
var cluster *state.Cluster
var clusterNonZonal *state.Cluster
var virtualPodCache *virtualpods.Cache
var fakeClock *clock.FakeClock
var recorder events.Recorder
var statusController *status.Controller

var nodePool *karpv1.NodePool
var nodeClass *v1beta1.AKSNodeClass
var nodeClaim *karpv1.NodeClaim

var fakeZone1 = zones.MakeAKSLabelZoneFromARMZone(fake.Region, "1")
var defaultTestSKU = fake.MakeSKU("Standard_D2_v3")

func TestCloudProvider(t *testing.T) {
	ctx = TestContextWithLogger(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "cloudProvider/Azure")
}

var _ = BeforeSuite(func() {
	env = coretest.NewEnvironment(
		coretest.WithCRDs(apis.CRDs...),
		coretest.WithCRDs(v1alpha1.CRDs...),
		coretest.WithFieldIndexers(coretest.NodeProviderIDFieldIndexer(ctx)))

	ctx = coreoptions.ToContext(ctx, coretest.Options())
	ctx, stop = context.WithCancel(ctx)
	fakeClock = clock.NewFakeClock(time.Now())
	recorder = events.NewRecorder(&record.FakeRecorder{})
})

var _ = AfterSuite(func() {
	stop()
	Expect(env.Stop()).To(Succeed(), "Failed to stop environment")
})

var _ = BeforeEach(func() {
	nodeClass = test.AKSNodeClass(test.WithVersionBasedDefaults(env.Version))
	nodePool = coretest.NodePool(karpv1.NodePool{
		Spec: karpv1.NodePoolSpec{
			Template: karpv1.NodeClaimTemplate{
				Spec: karpv1.NodeClaimTemplateSpec{
					NodeClassRef: &karpv1.NodeClassReference{
						Group: object.GVK(nodeClass).Group,
						Kind:  object.GVK(nodeClass).Kind,
						Name:  nodeClass.Name,
					},
				},
			},
		},
	})

	nodeClaim = coretest.NodeClaim(karpv1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{karpv1.NodePoolLabelKey: nodePool.Name},
		},
		Spec: karpv1.NodeClaimSpec{
			NodeClassRef: &karpv1.NodeClassReference{
				Group: object.GVK(nodeClass).Group,
				Kind:  object.GVK(nodeClass).Kind,
				Name:  nodeClass.Name,
			},
		},
	})
})

var _ = AfterEach(func() {
	ExpectCleanedUp(ctx, env.Client)
})

// Helper functions for NodeClaim validation
func validateNodeClaimCommon(nodeClaim *karpv1.NodeClaim, nodePool *karpv1.NodePool) {
	// Basic validation
	Expect(nodeClaim).ToNot(BeNil())
	Expect(nodeClaim.Status.Capacity).ToNot(BeEmpty())

	// Status fields
	Expect(nodeClaim.Status.ProviderID).ToNot(BeEmpty())
	Expect(nodeClaim.Status.ImageID).ToNot(BeEmpty())

	// Common labels validation
	Expect(nodeClaim.Labels).To(HaveKey(karpv1.CapacityTypeLabelKey))
	Expect(nodeClaim.Labels).To(HaveKey(karpv1.NodePoolLabelKey))
	if nodePool != nil {
		Expect(nodeClaim.Labels[karpv1.NodePoolLabelKey]).To(Equal(nodePool.Name))
	}
	Expect(nodeClaim.Labels).To(HaveKey(v1.LabelInstanceTypeStable))
	Expect(nodeClaim.Labels).To(HaveKey(v1.LabelArchStable))
	Expect(nodeClaim.Labels).To(HaveKey(v1beta1.LabelSKUName))
	Expect(nodeClaim.Labels).To(HaveKey(v1beta1.LabelSKUFamily))
	Expect(nodeClaim.Labels).To(HaveKey(v1beta1.LabelSKUCPU))
	Expect(nodeClaim.Labels).To(HaveKey(v1beta1.LabelSKUMemory))
	Expect(nodeClaim.Annotations).To(HaveKey(v1beta1.AnnotationCapacityReservationGroupID))

	// Zone validation (conditional)
	zone := nodeClaim.Labels[v1.LabelTopologyZone]
	if zone != "" && zone != zones.Regional {
		Expect(zone).To(MatchRegexp(`^[a-z0-9-]+-[0-9]+$`))
	}

	// Capacity and Allocatable resources
	Expect(nodeClaim.Status.Capacity).To(HaveKey(v1.ResourceCPU))
	Expect(nodeClaim.Status.Capacity).To(HaveKey(v1.ResourceMemory))
	Expect(nodeClaim.Status.Allocatable).To(HaveKey(v1.ResourceCPU))
	Expect(nodeClaim.Status.Allocatable).To(HaveKey(v1.ResourceMemory))

	// Lifecycle validation
	Expect(nodeClaim.DeletionTimestamp).To(BeNil())
}

func validateVMNodeClaim(nodeClaim *karpv1.NodeClaim, nodePool *karpv1.NodePool) {
	// Common validations
	validateNodeClaimCommon(nodeClaim, nodePool)

	// VM-specific validation (should NOT have AKS machine annotation)
	Expect(nodeClaim.Annotations).ToNot(HaveKey(v1beta1.AnnotationAKSMachineResourceID))
}

func reconcileCapacityOverlay(customResource v1.ResourceName, overlayCapacity resource.Quantity) {
	GinkgoHelper()
	nodeOverlay := coretest.NodeOverlay(karpv1alpha1.NodeOverlay{
		Spec: karpv1alpha1.NodeOverlaySpec{
			Requirements: []karpv1alpha1.NodeSelectorRequirement{{
				Key:      karpv1.NodePoolLabelKey,
				Operator: v1.NodeSelectorOpIn,
				Values:   []string{nodePool.Name},
			}},
			Capacity: v1.ResourceList{customResource: overlayCapacity},
		},
	})
	ExpectApplied(ctx, env.Client, nodeOverlay)
	nodeOverlayController := nodeoverlay.NewController(env.Clock, env.Client, cloudProvider, azureEnv.InstanceTypeStore, cluster)
	ExpectReconcileSucceeded(ctx, nodeOverlayController, client.ObjectKeyFromObject(nodeOverlay))
}

type nodeOverlayCapacityTestOptions struct {
	validateNodeClaim func(*karpv1.NodeClaim)
	resetCreateCalls  func()
	expectCreateCalls func()
}

func runNodeOverlayCapacityTests(testOptions nodeOverlayCapacityTestOptions) {
	Context("NodeOverlay", func() {
		It("should launch a NodeClaim that requests capacity added by a NodeOverlay", func() {
			ctx = coreoptions.ToContext(ctx, coretest.Options(coretest.OptionsFields{
				FeatureGates: coretest.FeatureGates{NodeOverlay: lo.ToPtr(true)},
			}))
			customResource := v1.ResourceName("example.com/dongle")
			overlayCapacity := resource.MustParse("100")
			nodeClaim.Spec.Resources.Requests = v1.ResourceList{customResource: resource.MustParse("1")}

			ExpectApplied(ctx, env.Client, nodeClass, nodePool, nodeClaim)
			reconcileCapacityOverlay(customResource, overlayCapacity)

			if testOptions.resetCreateCalls != nil {
				testOptions.resetCreateCalls()
			}
			cloudProviderMachine, err := CreateAndWaitForPromises(ctx, cloudProvider, azureEnv, nodeClaim)
			Expect(err).ToNot(HaveOccurred())
			Expect(cloudProviderMachine).ToNot(BeNil())
			if testOptions.validateNodeClaim != nil {
				testOptions.validateNodeClaim(cloudProviderMachine)
			}
			if testOptions.expectCreateCalls != nil {
				testOptions.expectCreateCalls()
			}
			capacity, ok := cloudProviderMachine.Status.Capacity[customResource]
			Expect(ok).To(BeTrue())
			Expect(capacity.Cmp(overlayCapacity)).To(Equal(0))
			allocatable, ok := cloudProviderMachine.Status.Allocatable[customResource]
			Expect(ok).To(BeTrue())
			Expect(allocatable.Cmp(overlayCapacity)).To(Equal(0))
		})

		It("should not use overlaid capacity when NodeOverlay is disabled", func() {
			// Explicitly disable the NodeOverlay feature gate so this test does not
			// depend on ordering with the previous It block that enables it.
			ctx = coreoptions.ToContext(ctx, coretest.Options(coretest.OptionsFields{
				FeatureGates: coretest.FeatureGates{NodeOverlay: lo.ToPtr(false)},
			}))
			customResource := v1.ResourceName("example.com/dongle")
			overlayCapacity := resource.MustParse("100")
			nodeClaim.Spec.Resources.Requests = v1.ResourceList{customResource: resource.MustParse("1")}

			ExpectApplied(ctx, env.Client, nodeClass, nodePool, nodeClaim)
			reconcileCapacityOverlay(customResource, overlayCapacity)

			if testOptions.resetCreateCalls != nil {
				testOptions.resetCreateCalls()
			}
			cloudProviderMachine, err := CreateAndWaitForPromises(ctx, cloudProvider, azureEnv, nodeClaim)
			Expect(corecloudprovider.IsInsufficientCapacityError(err)).To(BeTrue())
			Expect(cloudProviderMachine).To(BeNil())
		})
	})
}

func vmNodeOverlayCapacityTestOptions() nodeOverlayCapacityTestOptions {
	return nodeOverlayCapacityTestOptions{
		validateNodeClaim: func(nodeClaim *karpv1.NodeClaim) {
			validateVMNodeClaim(nodeClaim, nodePool)
		},
		resetCreateCalls: func() {
			azureEnv.VirtualMachinesAPI.VirtualMachineCreateOrUpdateBehavior.CalledWithInput.Reset()
			azureEnv.AKSMachinesAPI.AKSMachineCreateOrUpdateBehavior.CalledWithInput.Reset()
		},
		expectCreateCalls: func() {
			Expect(azureEnv.VirtualMachinesAPI.VirtualMachineCreateOrUpdateBehavior.CalledWithInput.Len()).To(Equal(1))
			Expect(azureEnv.AKSMachinesAPI.AKSMachineCreateOrUpdateBehavior.CalledWithInput.Len()).To(Equal(0))
		},
	}
}

func runCapacityBufferTests(expectCreateCalls func(expectedCalls int)) {
	Context("CapacityBuffer", func() {
		It("should launch capacity for a PodTemplate-backed buffer", func() {
			ctx = coreoptions.ToContext(ctx, coretest.Options(coretest.OptionsFields{
				FeatureGates: coretest.FeatureGates{CapacityBuffer: lo.ToPtr(true)},
			}))
			virtualPodCache = virtualpods.NewVirtualPodCache(env.Client)
			coreProvisioner = provisioning.NewProvisioner(env.Client, recorder, cloudProvider, cluster, fakeClock, deviceallocation.NewController(env.Client), virtualPodCache)
			ExpectApplied(ctx, env.Client, nodeClass, nodePool)

			podTemplate := &v1.PodTemplate{
				ObjectMeta: metav1.ObjectMeta{Name: "capacity-buffer-template", Namespace: "default"},
				Template: v1.PodTemplateSpec{
					Spec: v1.PodSpec{
						Containers: []v1.Container{{
							Name:  "workload",
							Image: "pause:latest",
							Resources: v1.ResourceRequirements{Requests: v1.ResourceList{
								v1.ResourceCPU:    resource.MustParse("1"),
								v1.ResourceMemory: resource.MustParse("1Gi"),
							}},
						}},
						NodeSelector: map[string]string{v1.LabelOSStable: string(v1.Linux)},
					},
				},
			}
			buffer := coretest.CapacityBuffer(autoscalingv1beta1.CapacityBuffer{
				ObjectMeta: metav1.ObjectMeta{Name: "capacity-buffer", Namespace: "default"},
				Spec: autoscalingv1beta1.CapacityBufferSpec{
					ProvisioningStrategy: lo.ToPtr(autoscalingv1beta1.ActiveProvisioningStrategy),
					PodTemplateRef:       &autoscalingv1beta1.LocalObjectRef{Name: podTemplate.Name},
					Replicas:             lo.ToPtr(int32(1)),
				},
			})
			DeferCleanup(func() {
				ExpectDeleted(ctx, env.Client, buffer, podTemplate)
			})

			ExpectApplied(ctx, env.Client, podTemplate, buffer)
			ExpectReconcileSucceeded(ctx, capacitybuffer.NewController(env.Client, coreProvisioner, virtualPodCache), client.ObjectKeyFromObject(buffer))
			EventuallyExpectCapacityBufferReplicas(ctx, env.Client, buffer, 1)

			ExpectProvisionedAndWaitForPromises(ctx, env.Client, cluster, cloudProvider, coreProvisioner, azureEnv)
			nodeClaims := ExpectNodeClaims(ctx, env.Client)
			Expect(nodeClaims).To(HaveLen(1))
			Expect(nodeClaims[0].Spec.Requirements).To(ContainElement(And(
				HaveField("Key", v1.LabelOSStable),
				HaveField("Values", ContainElement(string(v1.Linux))),
			)))
			expectCreateCalls(1)
		})
	})
}

var _ = Describe("CloudProvider", func() {
	Context("Repair", func() {
		const (
			legacyRepairMode      = true
			budgetAwareRepairMode = false
		)

		It("should explicitly reject in-place reboot", func() {
			err := (&CloudProvider{}).Reboot(ctx, nodeClaim, "operation-id")
			Expect(corecloudprovider.IsNodeRebootNotImplementedError(err)).To(BeTrue())
		})

		DescribeTable("should preserve repair tolerations and forceful drain bounds", func(conditionType v1.NodeConditionType, conditionStatus v1.ConditionStatus, toleration time.Duration) {
			repairCtx := coreoptions.ToContext(ctx, coretest.Options(coretest.OptionsFields{
				FeatureGates:     coretest.FeatureGates{NodeRepair: lo.ToPtr(true)},
				LegacyNodeRepair: lo.ToPtr(false),
			}))
			legacyProvider := New(nil, nil, nil, nil, nil, nil, nil, legacyRepairMode)
			Expect(legacyProvider.RepairPolicies()).To(HaveLen(4))
			Expect(legacyProvider.RepairPolicies()).To(ContainElement(corecloudprovider.RepairPolicy{
				ConditionType:      conditionType,
				ConditionStatus:    conditionStatus,
				TolerationDuration: toleration,
				Action:             corecloudprovider.ReplaceNode,
			}))

			provider := New(nil, nil, nil, nil, nil, nil, nil, budgetAwareRepairMode)
			matcher, err := health.NewRepairPolicyMatcher(repairCtx, provider)
			Expect(err).ToNot(HaveOccurred())
			Expect(matcher).ToNot(BeNil())
			now := fakeClock.Now()
			node := coretest.Node()
			node.CreationTimestamp = metav1.NewTime(now.Add(-time.Hour))
			node.Status.Conditions = []v1.NodeCondition{{
				Type:               conditionType,
				Status:             conditionStatus,
				Reason:             "UnrecognizedReason",
				LastTransitionTime: metav1.NewTime(now),
			}}
			matches := matcher.Match(node)
			Expect(matches).To(HaveLen(1))
			Expect(health.Resolve(matches, now.Add(toleration-time.Nanosecond), time.Time{}).Action).To(BeEmpty())
			result := health.Resolve(matches, now.Add(toleration), time.Time{})
			Expect(result.Action).To(Equal(corecloudprovider.ReplaceNode))
			Expect(result.Condition).To(Equal(conditionType))
			Expect(result.TerminationGracePeriod).To(Equal(lo.ToPtr(time.Duration(0))))

			node.Status.Conditions[0].Status = lo.Ternary(conditionStatus == v1.ConditionTrue, v1.ConditionFalse, v1.ConditionTrue)
			Expect(matcher.Match(node)).To(BeEmpty())
		},
			Entry("NodeReady=False", v1.NodeReady, v1.ConditionFalse, 10*time.Minute),
			Entry("NodeReady=Unknown", v1.NodeReady, v1.ConditionUnknown, 10*time.Minute),
			Entry("NodeHealthy=False", v1.NodeConditionType("kubernetes.azure.com/NodeHealthy"), v1.ConditionFalse, time.Duration(0)),
			Entry("PreemptionScheduled=True", v1.NodeConditionType(SpotConditionPreemptionScheduled), v1.ConditionTrue, time.Duration(0)),
		)

		It("should select the legacy policy set explicitly", func() {
			provider := New(nil, nil, nil, nil, nil, nil, nil, legacyRepairMode)
			policies := provider.RepairPolicies()
			Expect(policies).To(HaveLen(4))
			for _, policy := range policies {
				Expect(policy.Action).To(Equal(corecloudprovider.ReplaceNode))
				Expect(policy.ReasonRegex).To(BeEmpty())
				Expect(policy.TerminationGracePeriod).To(BeNil())
			}
		})

		It("should not construct the new matcher for legacy repair", func() {
			repairCtx := coreoptions.ToContext(ctx, coretest.Options(coretest.OptionsFields{
				FeatureGates:     coretest.FeatureGates{NodeRepair: lo.ToPtr(true)},
				LegacyNodeRepair: lo.ToPtr(true),
			}))
			provider := New(nil, nil, nil, nil, nil, nil, nil, legacyRepairMode)
			matcher, err := health.NewRepairPolicyMatcher(repairCtx, provider)
			Expect(err).ToNot(HaveOccurred())
			Expect(matcher).To(BeNil())
		})

		It("should keep returned repair policy profiles independent", func() {
			legacyProvider := New(nil, nil, nil, nil, nil, nil, nil, legacyRepairMode)
			provider := New(nil, nil, nil, nil, nil, nil, nil, budgetAwareRepairMode)
			legacyPolicies := legacyProvider.RepairPolicies()
			policies := provider.RepairPolicies()
			legacyPolicies[0].TolerationDuration = time.Hour
			*policies[0].TerminationGracePeriod = time.Minute
			Expect(legacyProvider.RepairPolicies()[0].TolerationDuration).To(Equal(10 * time.Minute))
			Expect(provider.RepairPolicies()[0].TerminationGracePeriod).To(Equal(lo.ToPtr(time.Duration(0))))
		})
	})

	// Attention: tests under "ProvisionMode = AKSScriptless" are not applicable to ProvisionMode = AKSMachineAPI option.
	// Due to different assumptions, not all tests can be shared. Add tests for AKS machine instances in a different Context/file.
	// If ProvisionMode = AKSScriptless is no longer supported, their code/tests will be replaced with ProvisionMode = AKSMachineAPI.
	Context("ProvisionMode = AKSScriptless, ManageExistingAKSMachines = false", func() {
		BeforeEach(func() {
			testOptions = test.Options(test.OptionsFields{
				ProvisionMode:             lo.ToPtr(consts.ProvisionModeAKSScriptless),
				ManageExistingAKSMachines: lo.ToPtr(false),
			})
			ctx = coreoptions.ToContext(ctx, coretest.Options())
			ctx = options.ToContext(ctx, testOptions)

			azureEnv = test.NewEnvironment(ctx, env)
			test.ApplyDefaultStatus(nodeClass, env, testOptions.UseSIG)
			cloudProvider = New(azureEnv.InstanceTypesProvider, azureEnv.VMInstanceProvider, azureEnv.AKSMachineProvider, recorder, env.Client, azureEnv.ImageProvider, azureEnv.InstanceTypeStore, coreoptions.FromContext(ctx).LegacyNodeRepair)

			cluster = state.NewCluster(fakeClock, env.Client, cloudProvider)
			coreProvisioner = provisioning.NewProvisioner(env.Client, recorder, cloudProvider, cluster, fakeClock, deviceallocation.NewController(env.Client), virtualpods.NewVirtualPodCache(env.Client))
		})

		AfterEach(func() {
			// Wait for any async polling goroutines to complete before resetting
			cloudProvider.WaitForInstancePromises()
			cluster.Reset()
			azureEnv.Reset(ctx)
		})

		runCapacityBufferTests(func(expectedCalls int) {
			Expect(azureEnv.VirtualMachinesAPI.VirtualMachineCreateOrUpdateBehavior.CalledWithInput.Len()).To(Equal(expectedCalls))
			Expect(azureEnv.AKSMachinesAPI.AKSMachineCreateOrUpdateBehavior.CalledWithInput.Len()).To(Equal(0))
		})

		Context("Multi-region", func() {
			var remote *test.Environment
			var regionalImages *regional.Images
			var registry *regional.Registry
			// regionErrs fails the subnet check of a region.
			var regionErrs map[string]error
			BeforeEach(func() {
				regionErrs = map[string]error{}
				testOptions.NetworkPlugin = consts.NetworkPluginNone
				testOptions.NetworkPluginMode = ""
				testOptions.ComputeRecommendationMode = consts.ComputeRecommendationModeDisabled
				testOptions.EnableMultiRegion = true
				remote = test.NewRegionalEnvironment(ctx, env, fake.RegionNonZonal, true)
				remoteVMs := instance.NewDefaultVMProvider(azureEnv.AZClient,
					remote.InstanceTypesProvider, remote.AllocationStrategyProvider, remote.LaunchTemplateProvider,
					azureEnv.LoadBalancerProvider, azureEnv.NetworkSecurityGroupProvider, remote.UnavailableOfferingsCache,
					fake.RegionNonZonal, testOptions.NodeResourceGroup, azureEnv.SubscriptionID, testOptions.ProvisionMode, "",
					lo.Must(auth.EnvironmentFromName("AzurePublicCloud")), remote.QuotaProvider)
				registry = regional.New(fake.Region, &regional.Providers{
					Types: azureEnv.InstanceTypesProvider, Images: azureEnv.ImageProvider, Instances: azureEnv.VMInstanceProvider, Quota: azureEnv.QuotaProvider,
				}, func(region string) *regional.Providers {
					Expect(region).To(Equal(fake.RegionNonZonal))
					return &regional.Providers{Types: remote.InstanceTypesProvider, Images: remote.ImageProvider, Instances: remoteVMs, Quota: remote.QuotaProvider}
				}, func(_ context.Context, region v1beta1.Region) error {
					Expect(region.Name).To(BeElementOf(fake.Region, fake.RegionNonZonal))
					return regionErrs[region.Name]
				}).WithClock(fakeClock)
				regionalTypes := &regional.InstanceTypes{Provider: azureEnv.InstanceTypesProvider, Registry: registry}
				regionalImages = &regional.Images{NodeImageProvider: azureEnv.ImageProvider, Registry: registry}
				regionalVMs := &regional.Instances{VMProvider: azureEnv.VMInstanceProvider, Registry: registry, KubeClient: env.Client}
				cloudProvider = New(regionalTypes, regionalVMs, azureEnv.AKSMachineProvider, recorder, env.Client, regionalImages, azureEnv.InstanceTypeStore, false)
				cluster = state.NewCluster(fakeClock, env.Client, cloudProvider)
				coreProvisioner = provisioning.NewProvisioner(env.Client, recorder, cloudProvider, cluster, fakeClock, deviceallocation.NewController(env.Client), virtualpods.NewVirtualPodCache(env.Client))
				nodeClass.Spec.VNETSubnetID = nil
				nodeClass.Spec.Regions = []v1beta1.Region{
					{Name: fake.Region, VNETSubnetID: fmt.Sprintf("/subscriptions/%s/resourceGroups/network-home/providers/Microsoft.Network/virtualNetworks/nodes/subnets/nodes", azureEnv.SubscriptionID)},
					{Name: fake.RegionNonZonal, VNETSubnetID: fmt.Sprintf("/subscriptions/%s/resourceGroups/network-remote/providers/Microsoft.Network/virtualNetworks/nodes/subnets/nodes", azureEnv.SubscriptionID)},
				}
				images, err := regionalImages.List(ctx, nodeClass)
				Expect(err).ToNot(HaveOccurred())
				nodeClass.Status.Images = lo.Map(images, func(image imagefamily.NodeImage, _ int) v1beta1.NodeImage {
					return v1beta1.NodeImage{ID: image.ID, Requirements: lo.Map(image.Requirements.NodeSelectorRequirements(), func(r karpv1.NodeSelectorRequirementWithMinValues, _ int) v1.NodeSelectorRequirement {
						return v1.NodeSelectorRequirement{Key: r.Key, Operator: r.Operator, Values: r.Values}
					})}
				})
			})
			AfterEach(func() {
				cloudProvider.WaitForInstancePromises()
				remote.Reset(ctx)
			})
			It("should not accept another region's image as the current image during drift checks", func() {
				nodeClass.Status.Images = []v1beta1.NodeImage{
					{ID: "gallery/image/versions/1", Requirements: []v1.NodeSelectorRequirement{{Key: v1.LabelTopologyRegion, Operator: v1.NodeSelectorOpIn, Values: []string{fake.Region}}}},
					{ID: "gallery/image/versions/2", Requirements: []v1.NodeSelectorRequirement{{Key: v1.LabelTopologyRegion, Operator: v1.NodeSelectorOpIn, Values: []string{fake.RegionNonZonal}}}},
				}
				ExpectApplied(ctx, env.Client, nodeClass)
				current := ExpectExists(ctx, env.Client, nodeClass)
				nodeClaim.Labels[v1.LabelTopologyRegion] = fake.RegionNonZonal
				nodeClaim.Status.ImageID = "gallery/image/versions/1"
				drift, err := cloudProvider.isImageVersionDrifted(ctx, nodeClaim, current)
				Expect(err).ToNot(HaveOccurred())
				Expect(drift).To(Equal(ImageDrift))
				nodeClaim.Status.ImageID = "gallery/image/versions/2"
				drift, err = cloudProvider.isImageVersionDrifted(ctx, nodeClaim, current)
				Expect(err).ToNot(HaveOccurred())
				Expect(drift).To(BeEmpty())
			})
			It("should isolate quota and capacity failures for regional zone zero", func() {
				ExpectApplied(ctx, env.Client, nodeClass, nodePool)
				sku := fake.MakeSKU("Standard_D2_v3")
				remote.UsageAPI.Usages.Append(&armcompute.Usage{
					Name:         &armcompute.UsageName{Value: lo.ToPtr(sku.GetFamilyName())},
					CurrentValue: lo.ToPtr(int32(10)), Limit: lo.ToPtr[int64](10),
				})
				Expect(remote.QuotaProvider.Update(ctx)).To(Succeed())
				remote.UnavailableOfferingsCache.MarkUnavailable(ctx, "AllocationFailed", sku, zones.Regional, karpv1.CapacityTypeSpot)
				types, err := cloudProvider.GetInstanceTypes(ctx, nodePool)
				Expect(err).ToNot(HaveOccurred())
				instanceType, ok := lo.Find(types, func(i *corecloudprovider.InstanceType) bool { return i.Name == sku.GetName() })
				Expect(ok).To(BeTrue())
				for _, offering := range instanceType.Offerings {
					if offering.Requirements.Get(v1.LabelTopologyZone).Has(zones.Regional) {
						Expect(offering.Available).To(Equal(offering.Requirements.Get(v1.LabelTopologyRegion).Has(fake.Region)))
					}
				}
				Expect(azureEnv.QuotaProvider.HasQuotaFor(ctx, sku)).To(BeTrue())
				Expect(azureEnv.UnavailableOfferingsCache.IsUnavailable(sku, zones.Regional, karpv1.CapacityTypeSpot)).To(BeFalse())
			})
			It("should schedule one NodePool in two physical regions and preserve retry, discovery and deletion", func() {
				ExpectApplied(ctx, env.Client, nodeClass, nodePool)
				pods := []*v1.Pod{
					coretest.UnschedulablePod(coretest.PodOptions{NodeSelector: map[string]string{v1.LabelTopologyRegion: fake.Region, v1.LabelInstanceTypeStable: "Standard_D2_v3"}}),
					coretest.UnschedulablePod(coretest.PodOptions{NodeSelector: map[string]string{v1.LabelTopologyRegion: fake.RegionNonZonal, v1.LabelInstanceTypeStable: "Standard_D2_v3"}}),
				}
				results := ExpectProvisionedResults(ctx, env.Client, cluster, cloudProvider, coreProvisioner, pods...)
				Expect(results.NewNodeClaims).To(HaveLen(2))
				scheduled := ExpectScheduledNodeClaimsCreated(ctx, env.Client, coreProvisioner, results.NewNodeClaims...)
				for i, claim := range scheduled {
					deployed, node, err := ExpectNodeClaimDeployed(ctx, env.Client, cloudProvider, claim)
					Expect(err).ToNot(HaveOccurred())
					cluster.UpdateNodeClaim(deployed)
					Expect(cluster.UpdateNode(ctx, node)).To(Succeed())
					cloudProvider.WaitForInstancePromises()
					BindPodsToNode(ctx, env.Client, cluster, results.NewNodeClaims[i], node)
				}
				for _, pod := range pods {
					ExpectScheduled(ctx, env.Client, pod)
				}
				Expect(azureEnv.VirtualMachinesAPI.VirtualMachineCreateOrUpdateBehavior.CalledWithInput.Len()).To(Equal(2))
				claims, err := cloudProvider.List(ctx)
				Expect(err).ToNot(HaveOccurred())
				Expect(claims).To(HaveLen(2))
				Expect(lo.Map(claims, func(c *karpv1.NodeClaim, _ int) string { return c.Labels[v1.LabelTopologyRegion] })).To(ConsistOf(fake.Region, fake.RegionNonZonal))
				for _, created := range claims {
					actual, err := cloudProvider.Get(ctx, created.Status.ProviderID)
					Expect(err).ToNot(HaveOccurred())
					Expect(actual.Labels[v1.LabelTopologyRegion]).To(Equal(created.Labels[v1.LabelTopologyRegion]))
					claim := ExpectExists(ctx, env.Client, &karpv1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Name: created.Name}})
					Expect(claim.Annotations[regional.PlacementAnnotation]).To(ContainSubstring(created.Labels[v1.LabelTopologyRegion]))
					retried, err := CreateAndWaitForPromises(ctx, cloudProvider, azureEnv, claim)
					Expect(err).ToNot(HaveOccurred())
					Expect(retried.Status.ProviderID).To(Equal(created.Status.ProviderID))
					vmName := instance.GenerateResourceName(created.Name)
					nic, err := azureEnv.NetworkInterfacesAPI.Get(ctx, testOptions.NodeResourceGroup, vmName, nil)
					Expect(err).ToNot(HaveOccurred())
					Expect(lo.FromPtr(nic.Location)).To(Equal(created.Labels[v1.LabelTopologyRegion]))
					Expect(nic.Properties.NetworkSecurityGroup).To(BeNil(), "regional NSGs belong to the subnet, not the home-region NIC")
					for _, config := range nic.Properties.IPConfigurations {
						Expect(config.Properties.LoadBalancerBackendAddressPools).To(BeEmpty())
						target, found := lo.Find(nodeClass.Spec.Regions, func(r v1beta1.Region) bool { return r.Name == created.Labels[v1.LabelTopologyRegion] })
						Expect(found).To(BeTrue())
						Expect(lo.FromPtr(config.Properties.Subnet.ID)).To(Equal(target.VNETSubnetID))
					}
					Expect(cloudProvider.Delete(ctx, created)).To(Succeed())
				}
				Expect(azureEnv.VirtualMachinesAPI.VirtualMachineCreateOrUpdateBehavior.CalledWithInput.Len()).To(Equal(2), "retries must adopt the original VM")
				Expect(azureEnv.LoadBalancersAPI.NewListPagerBehavior.CalledWithInput.Len()).To(BeZero())
			})
			It("should give regional VMs the identities and kubelet identity of their region", func() {
				globalIdentity := "/subscriptions/00000000-0000-0000-0000-000000000001/resourceGroups/identities/providers/Microsoft.ManagedIdentity/userAssignedIdentities/global"
				regionalIdentity := "/subscriptions/00000000-0000-0000-0000-000000000001/resourceGroups/identities/providers/Microsoft.ManagedIdentity/userAssignedIdentities/remote"
				// An add-on identity that every node carries is listed globally and in each regional entry.
				addonIdentity := "/subscriptions/00000000-0000-0000-0000-000000000001/resourceGroups/identities/providers/Microsoft.ManagedIdentity/userAssignedIdentities/addon"
				regionalClientID := "11111111-2222-3333-4444-555555555555"
				testOptions.NodeIdentities = []string{globalIdentity, addonIdentity}
				testOptions.MultiRegionNodeIdentities = map[string]options.RegionalNodeIdentity{
					fake.RegionNonZonal: {NodeIdentities: []string{regionalIdentity, addonIdentity}, KubeletIdentityClientID: regionalClientID},
				}
				ExpectApplied(ctx, env.Client, nodeClass, nodePool)
				pod := coretest.UnschedulablePod(coretest.PodOptions{NodeSelector: map[string]string{v1.LabelTopologyRegion: fake.RegionNonZonal, v1.LabelInstanceTypeStable: "Standard_D2_v3"}})
				ExpectProvisionedAndWaitForPromises(ctx, env.Client, cluster, cloudProvider, coreProvisioner, azureEnv, pod)
				ExpectScheduled(ctx, env.Client, pod)
				Expect(azureEnv.VirtualMachinesAPI.VirtualMachineCreateOrUpdateBehavior.CalledWithInput.Len()).To(Equal(1))
				created := azureEnv.VirtualMachinesAPI.VirtualMachineCreateOrUpdateBehavior.CalledWithInput.Pop().VM
				Expect(lo.FromPtr(created.Location)).To(Equal(fake.RegionNonZonal))
				Expect(lo.Keys(created.Identity.UserAssignedIdentities)).To(ConsistOf(regionalIdentity, addonIdentity), "the regional entry replaces the global identities")
				customData, err := base64.StdEncoding.DecodeString(lo.FromPtr(created.Properties.OSProfile.CustomData))
				Expect(err).ToNot(HaveOccurred())
				Expect(string(customData)).To(ContainSubstring(v1beta1.AKSLabelKubeletIdentityClientID + "=" + regionalClientID))
				Expect(string(customData)).ToNot(ContainSubstring(testOptions.KubeletIdentityClientID))
				Expect(string(customData)).ToNot(ContainSubstring("test-kubelet-identity-client-id"))

				// A retried launch adopts the regional VM and attaches no global identity.
				claims := &karpv1.NodeClaimList{}
				Expect(env.Client.List(ctx, claims)).To(Succeed())
				Expect(claims.Items).To(HaveLen(1))
				retried, err := CreateAndWaitForPromises(ctx, cloudProvider, azureEnv, &claims.Items[0])
				Expect(err).ToNot(HaveOccurred())
				Expect(retried.Labels).To(HaveKeyWithValue(v1.LabelTopologyRegion, fake.RegionNonZonal))
				Expect(azureEnv.VirtualMachinesAPI.VirtualMachineCreateOrUpdateBehavior.CalledWithInput.Len()).To(BeZero(), "the retry adopts the existing VM")
				Expect(azureEnv.VirtualMachinesAPI.VirtualMachineUpdateBehavior.Calls()).To(BeZero(), "adoption attaches no identity")

				// The cluster region has no entry, so its VMs get every global identity.
				homePod := coretest.UnschedulablePod(coretest.PodOptions{NodeSelector: map[string]string{v1.LabelTopologyRegion: fake.Region, v1.LabelInstanceTypeStable: "Standard_D2_v3"}})
				ExpectProvisionedAndWaitForPromises(ctx, env.Client, cluster, cloudProvider, coreProvisioner, azureEnv, homePod)
				ExpectScheduled(ctx, env.Client, homePod)
				home := azureEnv.VirtualMachinesAPI.VirtualMachineCreateOrUpdateBehavior.CalledWithInput.Pop().VM
				Expect(lo.FromPtr(home.Location)).To(Equal(fake.Region))
				Expect(lo.Keys(home.Identity.UserAssignedIdentities)).To(ConsistOf(globalIdentity, addonIdentity))
			})
			It("should label regional nodes with their placement subnet and the cluster VNet GUID under the external pod network opt-in", func() {
				testOptions.NetworkPlugin, testOptions.NetworkPluginMode = consts.NetworkPluginAzure, consts.NetworkPluginModeOverlay
				testOptions.NetworkDataplane = consts.NetworkDataplaneCilium
				testOptions.MultiRegionExternalPodNetwork = true
				// Overlay pod ranges are allocated in the cluster VNet, so a regional node pairs the cluster
				// VNet GUID with the name of the subnet its NIC is in.
				regionalSubnetID := fmt.Sprintf("/subscriptions/%s/resourceGroups/network-remote/providers/Microsoft.Network/virtualNetworks/remote-vnet/subnets/remote-nodes", azureEnv.SubscriptionID)
				nodeClass.Spec.Regions[1].VNETSubnetID = regionalSubnetID
				ExpectApplied(ctx, env.Client, nodeClass, nodePool)
				pod := coretest.UnschedulablePod(coretest.PodOptions{NodeSelector: map[string]string{v1.LabelTopologyRegion: fake.RegionNonZonal, v1.LabelInstanceTypeStable: "Standard_D2_v3"}})
				ExpectProvisionedAndWaitForPromises(ctx, env.Client, cluster, cloudProvider, coreProvisioner, azureEnv, pod)
				ExpectScheduled(ctx, env.Client, pod)
				input := azureEnv.VirtualMachinesAPI.VirtualMachineCreateOrUpdateBehavior.CalledWithInput.Pop()
				customData, err := base64.StdEncoding.DecodeString(lo.FromPtr(input.VM.Properties.OSProfile.CustomData))
				Expect(err).ToNot(HaveOccurred())
				for _, label := range []string{
					"kubernetes.azure.com/nodenetwork-vnetguid=" + testOptions.VnetGUID,
					"kubernetes.azure.com/network-subnet=remote-nodes",
					"kubernetes.azure.com/azure-cni-overlay=true",
					"kubernetes.azure.com/podnetwork-type=overlay",
					"kubernetes.azure.com/ebpf-dataplane=cilium",
					v1.LabelNodeExcludeBalancers + "=true",
				} {
					Expect(string(customData)).To(ContainSubstring(label))
				}
				nic, err := azureEnv.NetworkInterfacesAPI.Get(ctx, testOptions.NodeResourceGroup, input.VMName, nil)
				Expect(err).ToNot(HaveOccurred())
				Expect(nic.Properties.NetworkSecurityGroup).To(BeNil())
				for _, config := range nic.Properties.IPConfigurations {
					Expect(config.Properties.LoadBalancerBackendAddressPools).To(BeEmpty())
					Expect(lo.FromPtr(config.Properties.Subnet.ID)).To(Equal(regionalSubnetID))
				}
			})
			Context("FIPS images from the community gallery", func() {
				fipsDefinitions := func() []string {
					family := imagefamily.GetImageFamily(nodeClass.Spec.ImageFamily, nodeClass.Spec.FIPSMode, false, lo.Must(nodeClass.GetKubernetesVersion()), nil)
					defaultImages := family.DefaultImages(true, nodeClass.Spec.FIPSMode, false, false)
					names := make([]string, 0, len(defaultImages))
					for _, image := range defaultImages {
						names = append(names, image.ImageDefinition)
					}
					return names
				}
				fipsImageIDs := func(version string) []string {
					return lo.Map(fipsDefinitions(), func(name string, _ int) string {
						return imagefamily.BuildImageIDCIG(imagefamily.AKSAzureLinuxPublicGalleryURL, name, version)
					})
				}
				communityVersion := func(name string, published time.Time) *armcompute.CommunityGalleryImageVersion {
					return &armcompute.CommunityGalleryImageVersion{Name: lo.ToPtr(name), Properties: &armcompute.CommunityGalleryImageVersionProperties{PublishedDate: lo.ToPtr(published)}}
				}
				// reconcileImagesOutsideWindow reconciles the images of the NodeClass while the node OS maintenance window is
				// closed, listing the remote region again.
				reconcileImagesOutsideWindow := func() {
					GinkgoHelper()
					remote.NodeImagesCache.Flush()
					_, err := status.NewNodeImageReconciler(regionalImages, kubernetesfake.NewClientset(&v1.ConfigMap{
						ObjectMeta: metav1.ObjectMeta{Name: "upcoming-maintenance-window", Namespace: "kube-system"},
						Data: map[string]string{
							"aksManagedNodeOSUpgradeSchedule-start": time.Now().Add(time.Hour).Format(time.RFC3339),
							"aksManagedNodeOSUpgradeSchedule-end":   time.Now().Add(2 * time.Hour).Format(time.RFC3339),
						},
					}), "kube-system").Reconcile(ctx, nodeClass)
					Expect(err).ToNot(HaveOccurred())
				}
				BeforeEach(func() {
					testOptions.CommunityGalleryFIPSImages = true
					nodeClass.Spec.ImageFamily = lo.ToPtr(v1beta1.AzureLinuxImageFamily)
					nodeClass.Spec.FIPSMode = lo.ToPtr(v1beta1.FIPSModeFIPS)
					published := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
					azureEnv.CommunityImageVersionsAPI.ImageVersions.Append(communityVersion("202610.23.0", published.AddDate(0, 0, -7)))
					remote.CommunityImageVersionsAPI.ImageVersions.Append(communityVersion("202610.30.1", published))
				})
				It("should resolve FIPS images in each region and launch FIPS nodes there", func() {
					images, err := regionalImages.List(ctx, nodeClass)
					Expect(err).ToNot(HaveOccurred())
					homeIDs, remoteIDs := fipsImageIDs("202610.23.0"), fipsImageIDs("202610.30.1")
					Expect(images).To(HaveLen(len(homeIDs) + len(remoteIDs)))
					for _, image := range images {
						region := image.Requirements.Get(v1.LabelTopologyRegion).Any()
						Expect(lo.Ternary(region == fake.RegionNonZonal, remoteIDs, homeIDs)).To(ContainElement(image.ID), "only FIPS images, each at the newest version of its region")
					}
					nodeClass.Status.Images = lo.Map(images, func(image imagefamily.NodeImage, _ int) v1beta1.NodeImage {
						return v1beta1.NodeImage{ID: image.ID, Requirements: lo.Map(image.Requirements.NodeSelectorRequirements(), func(r karpv1.NodeSelectorRequirementWithMinValues, _ int) v1.NodeSelectorRequirement {
							return v1.NodeSelectorRequirement{Key: r.Key, Operator: r.Operator, Values: r.Values}
						})}
					})
					ExpectApplied(ctx, env.Client, nodeClass, nodePool)
					pod := coretest.UnschedulablePod(coretest.PodOptions{NodeSelector: map[string]string{v1.LabelTopologyRegion: fake.RegionNonZonal, v1.LabelInstanceTypeStable: "Standard_D2_v3"}})
					ExpectProvisionedAndWaitForPromises(ctx, env.Client, cluster, cloudProvider, coreProvisioner, azureEnv, pod)
					ExpectScheduled(ctx, env.Client, pod)
					vm := azureEnv.VirtualMachinesAPI.VirtualMachineCreateOrUpdateBehavior.CalledWithInput.Pop().VM
					Expect(lo.FromPtr(vm.Location)).To(Equal(fake.RegionNonZonal))
					Expect(remoteIDs).To(ContainElement(lo.FromPtr(vm.Properties.StorageProfile.ImageReference.CommunityGalleryImageID)))
					customData, err := base64.StdEncoding.DecodeString(lo.FromPtr(vm.Properties.OSProfile.CustomData))
					Expect(err).ToNot(HaveOccurred())
					Expect(string(customData)).To(ContainSubstring(v1beta1.AKSLabelFIPSEnabled + "=true"))

					// An arm64 size gets the arm64 FIPS image of its region.
					armPod := coretest.UnschedulablePod(coretest.PodOptions{NodeSelector: map[string]string{v1.LabelTopologyRegion: fake.RegionNonZonal, v1.LabelArchStable: karpv1.ArchitectureArm64}})
					ExpectProvisionedAndWaitForPromises(ctx, env.Client, cluster, cloudProvider, coreProvisioner, azureEnv, armPod)
					ExpectScheduled(ctx, env.Client, armPod)
					armVM := azureEnv.VirtualMachinesAPI.VirtualMachineCreateOrUpdateBehavior.CalledWithInput.Pop().VM
					Expect(lo.FromPtr(armVM.Location)).To(Equal(fake.RegionNonZonal))
					Expect(lo.FromPtr(armVM.Properties.StorageProfile.ImageReference.CommunityGalleryImageID)).To(And(BeElementOf(remoteIDs), ContainSubstring(imagefamily.AzureLinux3Gen2Arm64FIPSImageDefinition)))
				})
				It("should name a region without a FIPS image instead of falling back to non-FIPS images", func() {
					remote.CommunityImageVersionsAPI.NotFoundImageDefinitions = fipsDefinitions()
					_, err := regionalImages.List(ctx, nodeClass)
					var notFound *imagefamily.FIPSImagesNotFoundError
					Expect(errors.As(err, &notFound)).To(BeTrue())
					Expect(notFound.Location).To(Equal(fake.RegionNonZonal))
					Expect(err).To(MatchError(ContainSubstring("region " + fake.RegionNonZonal)))
				})
				It("should drift a node that carries the FIPS image version of another region", func() {
					images, err := regionalImages.List(ctx, nodeClass)
					Expect(err).ToNot(HaveOccurred())
					nodeClass.Status.Images = lo.Map(images, func(image imagefamily.NodeImage, _ int) v1beta1.NodeImage {
						return v1beta1.NodeImage{ID: image.ID, Requirements: lo.Map(image.Requirements.NodeSelectorRequirements(), func(r karpv1.NodeSelectorRequirementWithMinValues, _ int) v1.NodeSelectorRequirement {
							return v1.NodeSelectorRequirement{Key: r.Key, Operator: r.Operator, Values: r.Values}
						})}
					})
					ExpectApplied(ctx, env.Client, nodeClass)
					current := ExpectExists(ctx, env.Client, nodeClass)
					var drift corecloudprovider.DriftReason
					for _, placement := range []struct{ region, own, other string }{
						{fake.Region, "202610.23.0", "202610.30.1"},
						{fake.RegionNonZonal, "202610.30.1", "202610.23.0"},
					} {
						nodeClaim.Labels[v1.LabelTopologyRegion] = placement.region
						others := fipsImageIDs(placement.other)
						for i, own := range fipsImageIDs(placement.own) {
							nodeClaim.Status.ImageID = own
							drift, err = cloudProvider.isImageVersionDrifted(ctx, nodeClaim, current)
							Expect(err).ToNot(HaveOccurred())
							Expect(drift).To(BeEmpty(), "%s in %s", own, placement.region)
							nodeClaim.Status.ImageID = others[i]
							drift, err = cloudProvider.isImageVersionDrifted(ctx, nodeClaim, current)
							Expect(err).ToNot(HaveOccurred())
							Expect(drift).To(Equal(ImageDrift), "%s in %s", others[i], placement.region)
						}
					}
				})
				It("should keep the FIPS images in use, in their priority order and without drift, while a region's image definitions return one at a time outside the maintenance window", func() {
					reconcileImagesOutsideWindow()
					inUse := nodeClass.DeepCopy().Status.Images
					expectNoDrift := func(step string) {
						GinkgoHelper()
						Expect(nodeClass.StatusConditions().IsTrue(v1beta1.ConditionTypeImagesReady)).To(BeTrue(), step)
						Expect(nodeClass.Status.Images).To(Equal(inUse), step)
						nodeClaim.Labels[v1.LabelTopologyRegion] = fake.RegionNonZonal
						for _, id := range fipsImageIDs("202610.30.1") {
							nodeClaim.Status.ImageID = id
							drift, err := cloudProvider.isImageVersionDrifted(ctx, nodeClaim, nodeClass)
							Expect(err).ToNot(HaveOccurred())
							Expect(drift).To(BeEmpty(), "%s: %s", step, id)
						}
					}
					expectNoDrift("before")

					remote.CommunityImageVersionsAPI.ImageVersions.Append(communityVersion("202611.06.0", time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)))
					for _, reverse := range []bool{false, true} {
						definitions := fipsDefinitions()
						if reverse {
							slices.Reverse(definitions)
						}
						remote.CommunityImageVersionsAPI.NotFoundImageDefinitions = definitions
						reconcileImagesOutsideWindow()
						Expect(nodeClass.StatusConditions().Get(v1beta1.ConditionTypeImagesReady).Reason).To(Equal(imagefamily.FIPSImagesNotFoundReason))
						Expect(nodeClass.Status.Images).To(Equal(inUse))
						for i, definition := range definitions {
							// The remote region lists the first i+1 image definitions in the order of their return.
							remote.CommunityImageVersionsAPI.NotFoundImageDefinitions = definitions[i+1:]
							reconcileImagesOutsideWindow()
							expectNoDrift(fmt.Sprintf("%s returned, reverse priority order: %t", definition, reverse))
						}
					}
				})
				It("should launch a new node on the kept FIPS image of a skipped image definition ahead of lower-priority definitions", func() {
					reconcileImagesOutsideWindow()
					inUse := nodeClass.DeepCopy().Status.Images
					remote.CommunityImageVersionsAPI.ImageVersions.Append(communityVersion("202611.06.0", time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)))
					remote.CommunityImageVersionsAPI.NotFoundImageDefinitions = fipsDefinitions()[:1]
					reconcileImagesOutsideWindow()
					Expect(nodeClass.StatusConditions().IsTrue(v1beta1.ConditionTypeImagesReady)).To(BeTrue())
					// The previous spec asserts the order; this one shows its effect on a launch.
					Expect(nodeClass.Status.Images).To(ConsistOf(inUse))

					// Standard_D2s_v3 supports both Hyper-V generations, so it gets the first of the Gen2 and Gen1 images.
					ExpectApplied(ctx, env.Client, nodeClass, nodePool)
					pod := coretest.UnschedulablePod(coretest.PodOptions{NodeSelector: map[string]string{v1.LabelTopologyRegion: fake.RegionNonZonal, v1.LabelInstanceTypeStable: "Standard_D2s_v3"}})
					ExpectProvisionedAndWaitForPromises(ctx, env.Client, cluster, cloudProvider, coreProvisioner, azureEnv, pod)
					ExpectScheduled(ctx, env.Client, pod)
					vm := azureEnv.VirtualMachinesAPI.VirtualMachineCreateOrUpdateBehavior.CalledWithInput.Pop().VM
					Expect(lo.FromPtr(vm.Location)).To(Equal(fake.RegionNonZonal))
					Expect(lo.FromPtr(vm.Properties.StorageProfile.ImageReference.CommunityGalleryImageID)).To(Equal(fipsImageIDs("202610.30.1")[0]))
				})
			})
			It("should keep a hydrated region when its refresh fails and fail stale quota open", func() {
				start := fakeClock.Now()
				DeferCleanup(func() { fakeClock.SetTime(start) })
				ExpectApplied(ctx, env.Client, nodeClass, nodePool)
				sku := fake.MakeSKU("Standard_D2_v3")
				remote.UsageAPI.Usages.Append(&armcompute.Usage{
					Name:         &armcompute.UsageName{Value: lo.ToPtr(sku.GetFamilyName())},
					CurrentValue: lo.ToPtr(int32(10)), Limit: lo.ToPtr[int64](10),
				})
				// Core drift requires an offering compatible with a launched NodeClaim's labels.
				claimLabels := scheduling.NewLabelRequirements(map[string]string{
					v1.LabelTopologyRegion: fake.RegionNonZonal, karpv1.CapacityTypeLabelKey: karpv1.CapacityTypeOnDemand,
				})
				remoteOfferings := func() corecloudprovider.Offerings {
					types, err := cloudProvider.GetInstanceTypes(ctx, nodePool)
					Expect(err).ToNot(HaveOccurred())
					instanceType, ok := lo.Find(types, func(i *corecloudprovider.InstanceType) bool { return i.Name == sku.GetName() })
					Expect(ok).To(BeTrue())
					offerings := instanceType.Offerings.Compatible(claimLabels)
					Expect(offerings).ToNot(BeEmpty(), "a failed refresh must not make core drift report InstanceTypeNotFound")
					return offerings
				}
				Expect(remoteOfferings().Available()).To(BeEmpty(), "fresh quota is enforced")
				remote.SKUsAPI.Error = errors.New("synthetic SKU outage")
				remote.UsageAPI.Error = errors.New("synthetic quota outage")
				fakeClock.Step(13 * time.Hour)
				Expect(remoteOfferings().Available()).ToNot(BeEmpty(), "stale quota fails open like the home region")
			})
			It("should fail the NodeClass instead of serving a partial catalog when a region never loaded", func() {
				ExpectApplied(ctx, env.Client, nodeClass, nodePool)
				remote.SKUsAPI.Error = errors.New("synthetic SKU outage")
				_, err := cloudProvider.GetInstanceTypes(ctx, nodePool)
				Expect(err).To(MatchError(ContainSubstring("region " + fake.RegionNonZonal + " has no SKU catalog yet")))
			})
			It("should fail the NodeClass while a never-loaded region with a NodeClaim also fails its subnet check", func() {
				registry.WithKubeClient(env.Client)
				remote.SKUsAPI.Error = errors.New("synthetic SKU outage")
				regionErrs[fake.RegionNonZonal] = errors.New("synthetic subnet outage")
				placed := coretest.NodeClaim(karpv1.NodeClaim{
					ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
						regional.PlacementAnnotation: string(lo.Must(json.Marshal(nodeClass.Spec.Regions[1]))),
					}},
					Spec: karpv1.NodeClaimSpec{NodeClassRef: &karpv1.NodeClassReference{
						Group: object.GVK(nodeClass).Group, Kind: object.GVK(nodeClass).Kind, Name: nodeClass.Name,
					}},
				})
				ExpectApplied(ctx, env.Client, nodeClass, nodePool, placed)
				notLoaded := ContainSubstring("region " + fake.RegionNonZonal + " has no SKU catalog yet")
				err := registry.ValidateSubnets(ctx, nodeClass)
				Expect(err).To(MatchError(notLoaded), "the SKU catalog is checked before the subnet")
				var partial interface{ UnavailableRegions() []string }
				Expect(errors.As(err, &partial)).To(BeFalse(), "SubnetsReady turns False, so the NodeClass is not ready")
				_, err = cloudProvider.GetInstanceTypes(ctx, nodePool)
				Expect(err).To(MatchError(notLoaded), "no partial catalog in which core drift would miss the NodeClaim's instance type")
			})
			It("should keep provisioning in the other regions while a region without NodeClaims fails its checks", func() {
				registry.WithKubeClient(env.Client)
				regionErrs[fake.RegionNonZonal] = errors.New("synthetic subnet outage")
				ExpectApplied(ctx, env.Client, nodeClass, nodePool)
				err := registry.ValidateSubnets(ctx, nodeClass)
				Expect(err).To(MatchError(ContainSubstring("region " + fake.RegionNonZonal + " subnet: synthetic subnet outage")))
				var partial interface{ UnavailableRegions() []string }
				Expect(errors.As(err, &partial)).To(BeTrue())
				Expect(partial.UnavailableRegions()).To(ConsistOf(fake.RegionNonZonal))
				instanceTypes, err := cloudProvider.GetInstanceTypes(ctx, nodePool)
				Expect(err).ToNot(HaveOccurred())
				remoteOfferings := 0
				for _, instanceType := range instanceTypes {
					for _, offering := range instanceType.Offerings {
						if offering.Requirements.Get(v1.LabelTopologyRegion).Has(fake.RegionNonZonal) {
							remoteOfferings++
							Expect(offering.Available).To(BeFalse(), "%s in the failing region", instanceType.Name)
						}
					}
				}
				Expect(remoteOfferings).ToNot(BeZero(), "the failing region keeps its instance types for core drift")
				pod := coretest.UnschedulablePod()
				ExpectProvisionedAndWaitForPromises(ctx, env.Client, cluster, cloudProvider, coreProvisioner, azureEnv, pod)
				ExpectScheduled(ctx, env.Client, pod)
				Expect(lo.FromPtr(azureEnv.VirtualMachinesAPI.VirtualMachineCreateOrUpdateBehavior.CalledWithInput.Pop().VM.Location)).To(Equal(fake.Region))

				delete(regionErrs, fake.RegionNonZonal)
				Expect(registry.ValidateSubnets(ctx, nodeClass)).To(Succeed())
				instanceTypes, err = cloudProvider.GetInstanceTypes(ctx, nodePool)
				Expect(err).ToNot(HaveOccurred())
				Expect(lo.ContainsBy(instanceTypes, func(instanceType *corecloudprovider.InstanceType) bool {
					return lo.ContainsBy(instanceType.Offerings.Available(), func(offering *corecloudprovider.Offering) bool {
						return offering.Requirements.Get(v1.LabelTopologyRegion).Has(fake.RegionNonZonal)
					})
				})).To(BeTrue(), "the region serves again once it passes")
			})
			It("should garbage collect the VM of a removed region", func() {
				ExpectApplied(ctx, env.Client, nodeClass, nodePool)
				pod := coretest.UnschedulablePod(coretest.PodOptions{NodeSelector: map[string]string{v1.LabelTopologyRegion: fake.RegionNonZonal}})
				ExpectProvisionedAndWaitForPromises(ctx, env.Client, cluster, cloudProvider, coreProvisioner, azureEnv, pod)
				ExpectScheduled(ctx, env.Client, pod)
				input := azureEnv.VirtualMachinesAPI.VirtualMachineCreateOrUpdateBehavior.CalledWithInput.Pop()
				Expect(lo.FromPtr(input.VM.Location)).To(Equal(fake.RegionNonZonal))
				// The region leaves the NodeClass, and its NodeClaim is gone while its VM is past the grace period.
				nodeClass.Spec.Regions = nodeClass.Spec.Regions[:1]
				ExpectApplied(ctx, env.Client, nodeClass)
				nodeClaims := &karpv1.NodeClaimList{}
				Expect(env.Client.List(ctx, nodeClaims)).To(Succeed())
				Expect(nodeClaims.Items).To(HaveLen(1))
				ExpectFinalizersRemoved(ctx, env.Client, &nodeClaims.Items[0])
				ExpectDeleted(ctx, env.Client, &nodeClaims.Items[0])
				stored, err := azureEnv.VirtualMachinesAPI.Get(ctx, testOptions.NodeResourceGroup, input.VMName, nil)
				Expect(err).ToNot(HaveOccurred())
				vm := stored.VirtualMachine
				vm.Properties.TimeCreated = lo.ToPtr(time.Now().Add(-10 * time.Minute))
				azureEnv.VirtualMachinesAPI.Instances.Store(lo.FromPtr(vm.ID), vm)
				ExpectSingletonReconciled(ctx, garbagecollection.NewInstance(env.Client, cloudProvider, fakeClock))
				_, found := azureEnv.VirtualMachinesAPI.Instances.Load(lo.FromPtr(vm.ID))
				Expect(found).To(BeFalse(), "the VM of the removed region is deleted")
			})
			It("should serve a region again after it was removed and its network deleted and created again, without a restart", func() {
				ExpectApplied(ctx, env.Client, nodeClass, nodePool)
				pod := coretest.UnschedulablePod(coretest.PodOptions{NodeSelector: map[string]string{v1.LabelTopologyRegion: fake.RegionNonZonal}})
				ExpectProvisionedAndWaitForPromises(ctx, env.Client, cluster, cloudProvider, coreProvisioner, azureEnv, pod)
				node := ExpectScheduled(ctx, env.Client, pod)
				Expect(lo.FromPtr(azureEnv.VirtualMachinesAPI.VirtualMachineCreateOrUpdateBehavior.CalledWithInput.Pop().VM.Location)).To(Equal(fake.RegionNonZonal))
				nodeClaims := &karpv1.NodeClaimList{}
				Expect(env.Client.List(ctx, nodeClaims)).To(Succeed())
				Expect(nodeClaims.Items).To(HaveLen(1))
				claim := &nodeClaims.Items[0]
				remoteAvailable := func() bool {
					GinkgoHelper()
					instanceTypes, err := cloudProvider.GetInstanceTypes(ctx, nodePool)
					Expect(err).ToNot(HaveOccurred())
					return lo.ContainsBy(instanceTypes, func(instanceType *corecloudprovider.InstanceType) bool {
						return lo.ContainsBy(instanceType.Offerings.Available(), func(offering *corecloudprovider.Offering) bool {
							return offering.Requirements.Get(v1.LabelTopologyRegion).Has(fake.RegionNonZonal)
						})
					})
				}
				// observed applies a spec change and stands in for the status controller, which records the
				// NodeClass generation that its checks saw.
				observed := func() {
					GinkgoHelper()
					ExpectApplied(ctx, env.Client, nodeClass)
					conditions := nodeClass.GetConditions()
					for i := range conditions {
						conditions[i].ObservedGeneration = nodeClass.Generation
					}
					nodeClass.SetConditions(conditions)
					ExpectApplied(ctx, env.Client, nodeClass)
				}

				// The region leaves the NodeClass: its NodeClaim drifts, and the replacement deletes its VM and node.
				home, removed := nodeClass.Spec.Regions[0], nodeClass.Spec.Regions[1]
				nodeClass.Spec.Regions = []v1beta1.Region{home}
				observed()
				Expect(cloudProvider.IsDrifted(ctx, claim)).To(Equal(RegionalPlacementDrift))
				Expect(cloudProvider.Delete(ctx, claim)).To(Succeed())
				ExpectFinalizersRemoved(ctx, env.Client, claim, node)
				ExpectDeleted(ctx, env.Client, claim, node)
				cluster.DeleteNodeClaim(claim.Name)
				cluster.DeleteNode(node.Name)

				// The region's network is deleted. While the region is not listed, nothing checks or offers it.
				regionErrs[fake.RegionNonZonal] = errors.New("synthetic ResourceGroupNotFound")
				Expect(registry.ValidateSubnets(ctx, nodeClass)).To(Succeed())
				Expect(remoteAvailable()).To(BeFalse())

				// Listed again before its network exists, the region gets no new nodes.
				nodeClass.Spec.Regions = []v1beta1.Region{home, removed}
				observed()
				var partial interface{ UnavailableRegions() []string }
				Expect(errors.As(registry.ValidateSubnets(ctx, nodeClass), &partial)).To(BeTrue())
				Expect(partial.UnavailableRegions()).To(ConsistOf(fake.RegionNonZonal))
				Expect(remoteAvailable()).To(BeFalse())
				pod = coretest.UnschedulablePod(coretest.PodOptions{NodeSelector: map[string]string{v1.LabelTopologyRegion: fake.RegionNonZonal}})
				ExpectProvisionedAndWaitForPromises(ctx, env.Client, cluster, cloudProvider, coreProvisioner, azureEnv, pod)
				ExpectNotScheduled(ctx, env.Client, pod)
				Expect(azureEnv.VirtualMachinesAPI.VirtualMachineCreateOrUpdateBehavior.CalledWithInput.Len()).To(BeZero())

				// Once the network exists again, the same registry and providers serve the region on it.
				delete(regionErrs, fake.RegionNonZonal)
				Expect(registry.ValidateSubnets(ctx, nodeClass)).To(Succeed())
				Expect(remoteAvailable()).To(BeTrue())
				ExpectProvisionedAndWaitForPromises(ctx, env.Client, cluster, cloudProvider, coreProvisioner, azureEnv, pod)
				ExpectScheduled(ctx, env.Client, pod)
				input := azureEnv.VirtualMachinesAPI.VirtualMachineCreateOrUpdateBehavior.CalledWithInput.Pop()
				Expect(lo.FromPtr(input.VM.Location)).To(Equal(fake.RegionNonZonal))
				nic, err := azureEnv.NetworkInterfacesAPI.Get(ctx, testOptions.NodeResourceGroup, input.VMName, nil)
				Expect(err).ToNot(HaveOccurred())
				for _, config := range nic.Properties.IPConfigurations {
					Expect(lo.FromPtr(config.Properties.Subnet.ID)).To(Equal(removed.VNETSubnetID))
				}
			})
			It("should reject spec.regions before any Azure write when multi-region provisioning is disabled", func() {
				testOptions.EnableMultiRegion = false
				ExpectApplied(ctx, env.Client, nodeClass, nodePool, nodeClaim)
				_, err := cloudProvider.Create(ctx, ExpectExists(ctx, env.Client, nodeClaim))
				Expect(corecloudprovider.IsNodeClassNotReadyError(err)).To(BeTrue())
				Expect(azureEnv.VirtualMachinesAPI.VirtualMachineCreateOrUpdateBehavior.CalledWithInput.Len()).To(BeZero())
			})
			It("should report placement drift through the NodeClass drift checks", func() {
				nodeClaim.Status.ProviderID = "azure:///subscriptions/synthetic/resourceGroups/synthetic/providers/Microsoft.Compute/virtualMachines/synthetic"
				nodeClaim.Annotations = map[string]string{regional.PlacementAnnotation: string(lo.Must(json.Marshal(nodeClass.Spec.Regions[1])))}
				nodeClass.Spec.Regions = nodeClass.Spec.Regions[:1]
				drift, err := cloudProvider.isNodeClassDrifted(ctx, nodeClaim, nodeClass)
				Expect(err).ToNot(HaveOccurred())
				Expect(drift).To(Equal(RegionalPlacementDrift))
			})
			DescribeTable("should drift only NodeClaims whose own placement is no longer listed",
				func(annotated bool, edit func([]v1beta1.Region) []v1beta1.Region, expected corecloudprovider.DriftReason) {
					delete(nodeClaim.Annotations, regional.PlacementAnnotation)
					if annotated {
						nodeClaim.Annotations = lo.Assign(nodeClaim.Annotations, map[string]string{regional.PlacementAnnotation: string(lo.Must(json.Marshal(nodeClass.Spec.Regions[1])))})
					}
					nodeClass.Spec.Regions = edit(slices.Clone(nodeClass.Spec.Regions))
					drift, err := cloudProvider.isRegionalPlacementDrifted(ctx, nodeClaim, nodeClass)
					Expect(err).ToNot(HaveOccurred())
					Expect(drift).To(Equal(expected))
				},
				Entry("unchanged", true, func(r []v1beta1.Region) []v1beta1.Region { return r }, NoDrift),
				Entry("a region added", true, func(r []v1beta1.Region) []v1beta1.Region {
					return append(r, v1beta1.Region{Name: "centralus", VNETSubnetID: r[0].VNETSubnetID + "-central"})
				}, NoDrift),
				Entry("regions reordered", true, func(r []v1beta1.Region) []v1beta1.Region { return []v1beta1.Region{r[1], r[0]} }, NoDrift),
				Entry("another region removed", true, func(r []v1beta1.Region) []v1beta1.Region { return r[1:] }, NoDrift),
				Entry("placement subnet recased", true, func(r []v1beta1.Region) []v1beta1.Region {
					r[1].VNETSubnetID = strings.ToUpper(r[1].VNETSubnetID)
					return r
				}, NoDrift),
				Entry("placement region removed", true, func(r []v1beta1.Region) []v1beta1.Region { return r[:1] }, RegionalPlacementDrift),
				Entry("placement subnet changed", true, func(r []v1beta1.Region) []v1beta1.Region {
					r[1].VNETSubnetID += "-new"
					return r
				}, RegionalPlacementDrift),
				Entry("regions removed", true, func([]v1beta1.Region) []v1beta1.Region { return nil }, RegionalPlacementDrift),
				Entry("regions added for a single-region NodeClaim", false, func(r []v1beta1.Region) []v1beta1.Region { return r }, RegionalPlacementDrift),
			)
			DescribeTable("should reject unsupported configurations even with a stale Ready condition", func(mode, plugin, pluginMode string) {
				testOptions.ProvisionMode, testOptions.NetworkPlugin, testOptions.NetworkPluginMode = mode, plugin, pluginMode
				ExpectApplied(ctx, env.Client, nodeClass, nodePool, nodeClaim)
				_, err := cloudProvider.Create(ctx, ExpectExists(ctx, env.Client, nodeClaim))
				Expect(corecloudprovider.IsNodeClassNotReadyError(err)).To(BeTrue())
				Expect(azureEnv.VirtualMachinesAPI.VirtualMachineCreateOrUpdateBehavior.CalledWithInput.Len()).To(BeZero())
				Expect(azureEnv.AKSMachinesAPI.AKSMachineCreateOrUpdateBehavior.CalledWithInput.Len()).To(BeZero())
			},
				Entry("managed Azure CNI", consts.ProvisionModeAKSScriptless, consts.NetworkPluginAzure, consts.NetworkPluginModeOverlay),
				Entry("Machine API", consts.ProvisionModeAKSMachineAPI, consts.NetworkPluginNone, ""),
				Entry("Machine API header batch", consts.ProvisionModeAKSMachineAPIHeaderBatch, consts.NetworkPluginNone, ""),
			)
		})

		It("should reject managed GPU before creating a VM on an unsupported provision mode", func() {
			nodeClass.Spec.GPU = &v1beta1.GPU{
				Mode: lo.ToPtr(v1beta1.GPUModeDriver),
				Nvidia: &v1beta1.NvidiaGPU{
					ManagementMode: lo.ToPtr(v1beta1.ManagementModeManaged),
				},
			}
			statusController = status.NewController(env.Client, azureEnv.SubscriptionID, fake.Region, azureEnv.KubernetesVersionProvider, azureEnv.ImageProvider, env.KubernetesInterface, env.KubernetesInterface, azureEnv.DynamicInterface, azureEnv.SubnetsAPI, azureEnv.DiskEncryptionSetsAPI, testOptions.ParsedDiskEncryptionSetID, options.FromContext(ctx).NetworkPolicy, options.FromContext(ctx).NetworkPlugin,
				azureEnv.CapacityReservationGroupsAPI, azureEnv.CapacityReservationsAPI, azureEnv.InstanceTypesProvider, azureEnv.UnavailableOfferingsCache, testOptions.SystemNamespace)

			ExpectApplied(ctx, env.Client, nodePool, nodeClass, nodeClaim)
			ExpectObjectReconciled(ctx, env.Client, statusController, nodeClass)
			nodeClass = ExpectExists(ctx, env.Client, nodeClass)

			validationCondition := nodeClass.StatusConditions().Get(v1beta1.ConditionTypeValidationSucceeded)
			Expect(validationCondition.IsFalse()).To(BeTrue())
			Expect(validationCondition.Reason).To(Equal(status.ManagedGPUUnsupportedProvisionMode))
			Expect(nodeClass.StatusConditions().Get(corestatus.ConditionReady).IsFalse()).To(BeTrue())

			created, err := CreateAndWaitForPromises(ctx, cloudProvider, azureEnv, nodeClaim)
			Expect(corecloudprovider.IsNodeClassNotReadyError(err)).To(BeTrue())
			Expect(created).To(BeNil())
			Expect(azureEnv.VirtualMachinesAPI.VirtualMachineCreateOrUpdateBehavior.CalledWithInput.Len()).To(BeZero())
			Expect(azureEnv.AKSMachinesAPI.AKSMachineCreateOrUpdateBehavior.CalledWithInput.Len()).To(BeZero())
		})

		It("should list nodeclaim created by the CloudProvider", func() {
			ExpectApplied(ctx, env.Client, nodeClass, nodePool)
			pod := coretest.UnschedulablePod()
			ExpectProvisionedAndWaitForPromises(ctx, env.Client, cluster, cloudProvider, coreProvisioner, azureEnv, pod)
			ExpectScheduled(ctx, env.Client, pod)
			Expect(azureEnv.VirtualMachinesAPI.VirtualMachineCreateOrUpdateBehavior.CalledWithInput.Len()).To(Equal(1))

			nodeClaims, _ := cloudProvider.List(ctx)
			Expect(azureEnv.AKSMachinesAPI.AKSMachineNewListPagerBehavior.CalledWithInput.Len()).To(Equal(0))
			Expect(azureEnv.AzureResourceGraphAPI.AzureResourceGraphResourcesBehavior.CalledWithInput.Len()).To(Equal(1))
			queryRequest := azureEnv.AzureResourceGraphAPI.AzureResourceGraphResourcesBehavior.CalledWithInput.Pop().Query
			Expect(*queryRequest.Query).To(Equal(instance.GetVMListQueryBuilder(azureEnv.AzureResourceGraphAPI.ResourceGroup).String()))
			Expect(nodeClaims).To(HaveLen(1))
			validateVMNodeClaim(nodeClaims[0], nodePool)
			resp, _ := azureEnv.VirtualMachinesAPI.Get(ctx, azureEnv.AzureResourceGraphAPI.ResourceGroup, nodeClaims[0].Name, nil)
			Expect(resp.VirtualMachine).ToNot(BeNil())
		})

		It("should track quota across accepted VM create and delete", func() {
			sku := fake.MakeSKU("Standard_D2_v3")
			vCPUs := lo.Must(sku.VCPU())
			azureEnv.UsageAPI.Usages.Append(&armcompute.Usage{
				Name:         &armcompute.UsageName{Value: lo.ToPtr(sku.GetFamilyName())},
				CurrentValue: lo.ToPtr(int32(100 - vCPUs)),
				Limit:        lo.ToPtr[int64](100),
			})
			Expect(azureEnv.QuotaProvider.Update(ctx)).To(Succeed())
			nodeClaim.Spec.Requirements = []karpv1.NodeSelectorRequirementWithMinValues{
				{
					Key:      v1.LabelInstanceTypeStable,
					Operator: v1.NodeSelectorOpIn,
					Values:   []string{sku.GetName()},
				},
				{
					Key:      karpv1.CapacityTypeLabelKey,
					Operator: v1.NodeSelectorOpIn,
					Values:   []string{karpv1.CapacityTypeOnDemand},
				},
			}
			ExpectApplied(ctx, env.Client, nodeClass, nodePool, nodeClaim)
			nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)

			created, err := CreateAndWaitForPromises(ctx, cloudProvider, azureEnv, nodeClaim)
			Expect(err).ToNot(HaveOccurred())
			Expect(azureEnv.QuotaProvider.HasQuotaFor(ctx, sku)).To(BeFalse())

			retried, err := CreateAndWaitForPromises(ctx, cloudProvider, azureEnv, nodeClaim)
			Expect(err).ToNot(HaveOccurred())
			Expect(retried).ToNot(BeNil())
			Expect(azureEnv.VirtualMachinesAPI.VirtualMachineCreateOrUpdateBehavior.CalledWithInput.Len()).To(Equal(1))

			nodeClaim.Status = created.Status
			nodeClaim.Labels = created.Labels
			nodeClaim.Annotations = created.Annotations
			// The completed delete releases quota immediately.
			Expect(cloudProvider.Delete(ctx, nodeClaim)).To(Succeed())
			Expect(azureEnv.QuotaProvider.HasQuotaFor(ctx, sku)).To(BeTrue())

			// A subsequent NotFound remains idempotent.
			err = cloudProvider.Delete(ctx, nodeClaim)
			Expect(corecloudprovider.IsNodeClaimNotFoundError(err)).To(BeTrue())
			Expect(azureEnv.QuotaProvider.HasQuotaFor(ctx, sku)).To(BeTrue())
		})

		It("should not adjust family quota for Spot VM create or delete", func() {
			spotSKU := fake.MakeSKU("Standard_D2s_v3")
			largerSKU := fake.MakeSKU("Standard_D4s_v3")
			Expect(largerSKU.GetFamilyName()).To(Equal(spotSKU.GetFamilyName()))
			azureEnv.UsageAPI.Usages.Append(&armcompute.Usage{
				Name:         &armcompute.UsageName{Value: lo.ToPtr(spotSKU.GetFamilyName())},
				CurrentValue: lo.ToPtr[int32](98),
				Limit:        lo.ToPtr[int64](100),
			})
			Expect(azureEnv.QuotaProvider.Update(ctx)).To(Succeed())
			nodeClaim.Spec.Requirements = []karpv1.NodeSelectorRequirementWithMinValues{
				{Key: v1.LabelInstanceTypeStable, Operator: v1.NodeSelectorOpIn, Values: []string{spotSKU.GetName()}},
				{Key: karpv1.CapacityTypeLabelKey, Operator: v1.NodeSelectorOpIn, Values: []string{karpv1.CapacityTypeSpot}},
			}
			ExpectApplied(ctx, env.Client, nodeClass, nodePool, nodeClaim)
			nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)

			created, err := CreateAndWaitForPromises(ctx, cloudProvider, azureEnv, nodeClaim)
			Expect(err).ToNot(HaveOccurred())
			Expect(created.Labels[karpv1.CapacityTypeLabelKey]).To(Equal(karpv1.CapacityTypeSpot))
			Expect(azureEnv.QuotaProvider.HasQuotaFor(ctx, spotSKU)).To(BeTrue(), "Spot create must not consume family quota")
			Expect(azureEnv.QuotaProvider.HasQuotaFor(ctx, largerSKU)).To(BeFalse())

			// Clear any create adjustment before verifying delete independently.
			Expect(azureEnv.QuotaProvider.Update(ctx)).To(Succeed())
			nodeClaim.Status = created.Status
			nodeClaim.Labels = created.Labels
			nodeClaim.Annotations = created.Annotations
			Expect(cloudProvider.Delete(ctx, nodeClaim)).To(Succeed())
			Expect(azureEnv.QuotaProvider.HasQuotaFor(ctx, largerSKU)).To(BeFalse(), "Spot delete must not release family quota")
		})

		It("should release quota after failed VM creation cleanup", func() {
			sku := fake.MakeSKU("Standard_D2_v3")
			vCPUs := lo.Must(sku.VCPU())
			azureEnv.UsageAPI.Usages.Append(&armcompute.Usage{
				Name:         &armcompute.UsageName{Value: lo.ToPtr(sku.GetFamilyName())},
				CurrentValue: lo.ToPtr(int32(100 - vCPUs)),
				Limit:        lo.ToPtr[int64](100),
			})
			Expect(azureEnv.QuotaProvider.Update(ctx)).To(Succeed())
			delete(nodeClaim.Labels, karpv1.NodePoolLabelKey)
			nodeClaim.Spec.Requirements = []karpv1.NodeSelectorRequirementWithMinValues{
				{Key: v1.LabelInstanceTypeStable, Operator: v1.NodeSelectorOpIn, Values: []string{sku.GetName()}},
				{Key: karpv1.CapacityTypeLabelKey, Operator: v1.NodeSelectorOpIn, Values: []string{karpv1.CapacityTypeOnDemand}},
			}
			ExpectApplied(ctx, env.Client, nodeClass, nodeClaim)
			nodeClaim = ExpectExists(ctx, env.Client, nodeClaim)
			azureEnv.VirtualMachinesAPI.VirtualMachineCreateOrUpdateBehavior.Error.Set(&azcore.ResponseError{ErrorCode: "InternalOperationError"})

			created, err := cloudProvider.Create(ctx, nodeClaim)
			Expect(err).To(HaveOccurred())
			Expect(created).To(BeNil())
			Expect(azureEnv.QuotaProvider.HasQuotaFor(ctx, sku)).To(BeTrue())

			azureEnv.VirtualMachinesAPI.VirtualMachineCreateOrUpdateBehavior.Error.Set(nil)
			created, err = cloudProvider.Create(ctx, nodeClaim)
			Expect(err).ToNot(HaveOccurred())
			Expect(created).ToNot(BeNil())
			Expect(azureEnv.QuotaProvider.HasQuotaFor(ctx, sku)).To(BeFalse())
		})

		It("should increment the disruption metric after an async VM provisioning failure", func() {
			coretest.ReplaceRequirements(nodePool, karpv1.NodeSelectorRequirementWithMinValues{
				Key:      karpv1.CapacityTypeLabelKey,
				Operator: v1.NodeSelectorOpIn,
				Values:   []string{karpv1.CapacityTypeOnDemand},
			})
			azureEnv.VirtualMachinesAPI.VirtualMachineCreateOrUpdateBehavior.Error.Set(&azcore.ResponseError{ErrorCode: "InternalOperationError"})

			ExpectApplied(ctx, env.Client, nodeClass, nodePool)
			pod := coretest.UnschedulablePod()
			ExpectLaunched(ctx, env.Client, cloudProvider, coreProvisioner, pod)
			cloudProvider.WaitForInstancePromises()
			ExpectNotScheduled(ctx, env.Client, pod)
			Expect(ExpectNodeClaims(ctx, env.Client)).To(BeEmpty())

			Expect(azureEnv.VirtualMachinesAPI.VirtualMachineCreateOrUpdateBehavior.CalledWithInput.Len()).To(Equal(1))
			ExpectMetricCounterValue(metrics.NodeClaimsDisruptedTotal, 1, map[string]string{
				metrics.ReasonLabel:              "async_provisioning",
				metrics.NodePoolLabel:            nodePool.Name,
				metrics.CapacityTypeLabel:        karpv1.CapacityTypeOnDemand,
				metrics.ConsolidationPolicyLabel: "",
				metrics.TerminationModeLabel:     metrics.TerminationModeGraceful,
			})
		})

		It("should retain quota when VM promise cleanup cannot delete the instance", func() {
			sku := fake.MakeSKU("Standard_D2_v3")
			vCPUs := lo.Must(sku.VCPU())
			azureEnv.UsageAPI.Usages.Append(&armcompute.Usage{
				Name:         &armcompute.UsageName{Value: lo.ToPtr(sku.GetFamilyName())},
				CurrentValue: lo.ToPtr(int32(100 - vCPUs)),
				Limit:        lo.ToPtr[int64](100),
			})
			Expect(azureEnv.QuotaProvider.Update(ctx)).To(Succeed())
			nodeClaim.Spec.Requirements = []karpv1.NodeSelectorRequirementWithMinValues{
				{Key: v1.LabelInstanceTypeStable, Operator: v1.NodeSelectorOpIn, Values: []string{sku.GetName()}},
				{Key: karpv1.CapacityTypeLabelKey, Operator: v1.NodeSelectorOpIn, Values: []string{karpv1.CapacityTypeOnDemand}},
			}
			instanceTypes, err := azureEnv.InstanceTypesProvider.List(ctx, nodeClass)
			Expect(err).ToNot(HaveOccurred())
			instanceTypes = lo.Filter(instanceTypes, func(instanceType *corecloudprovider.InstanceType, _ int) bool {
				return instanceType.Name == sku.GetName()
			})
			Expect(instanceTypes).To(HaveLen(1))
			promise, err := azureEnv.VMInstanceProvider.BeginCreate(ctx, nodeClass, nodeClaim, instanceTypes)
			Expect(err).ToNot(HaveOccurred())
			Expect(azureEnv.QuotaProvider.HasQuotaFor(ctx, sku)).To(BeFalse())

			azureEnv.VirtualMachinesAPI.VirtualMachineDeleteBehavior.BeginError.Set(&azcore.ResponseError{ErrorCode: "DeleteFailed"})

			err = promise.Cleanup(ctx)
			Expect(err).To(HaveOccurred())
			Expect(azureEnv.QuotaProvider.HasQuotaFor(ctx, sku)).To(BeFalse(), "failed cleanup must retain the accepted-create debit")
		})

		Context("Quota deletion metadata", func() {
			initializeQuota := func() *skewer.SKU {
				sku := fake.MakeSKU("Standard_D2_v3")
				azureEnv.UsageAPI.Usages.Append(&armcompute.Usage{
					Name:         &armcompute.UsageName{Value: lo.ToPtr(sku.GetFamilyName())},
					CurrentValue: lo.ToPtr[int32](100),
					Limit:        lo.ToPtr[int64](100),
				})
				Expect(azureEnv.QuotaProvider.Update(ctx)).To(Succeed())
				nodeClaim.Labels[v1.LabelInstanceTypeStable] = sku.GetName()
				nodeClaim.Labels[karpv1.CapacityTypeLabelKey] = karpv1.CapacityTypeOnDemand
				return sku
			}

			It("should release quota for an ordinary instance after the NodeClass adds a reservation", func() {
				sku := initializeQuota()
				nodeClaim.Annotations = map[string]string{v1beta1.AnnotationCapacityReservationGroupID: ""}
				nodeClass.Spec.CapacityReservation = &v1beta1.CapacityReservationConfiguration{GroupID: lo.ToPtr("/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/capacityReservationGroups/crg")}

				err := azureEnv.VMInstanceProvider.Delete(ctx, nodeClaim, "missing")
				Expect(corecloudprovider.IsNodeClaimNotFoundError(err)).To(BeTrue())
				Expect(azureEnv.QuotaProvider.HasQuotaFor(ctx, sku)).To(BeTrue())
			})

			It("should not release quota for a reserved instance after the NodeClass removes its reservation", func() {
				sku := initializeQuota()
				nodeClaim.Annotations = map[string]string{v1beta1.AnnotationCapacityReservationGroupID: "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/capacityReservationGroups/crg"}
				nodeClass.Spec.CapacityReservation = nil

				err := azureEnv.VMInstanceProvider.Delete(ctx, nodeClaim, "missing")
				Expect(corecloudprovider.IsNodeClaimNotFoundError(err)).To(BeTrue())
				Expect(azureEnv.QuotaProvider.HasQuotaFor(ctx, sku)).To(BeFalse())
			})

			It("should release quota while the NodeClass is terminating", func() {
				sku := initializeQuota()
				nodeClaim.Annotations = map[string]string{v1beta1.AnnotationCapacityReservationGroupID: ""}
				nodeClass.Finalizers = []string{test.TestingFinalizer}
				ExpectApplied(ctx, env.Client, nodeClass)
				Expect(env.Client.Delete(ctx, nodeClass)).To(Succeed())

				err := azureEnv.VMInstanceProvider.Delete(ctx, nodeClaim, "missing")
				Expect(corecloudprovider.IsNodeClaimNotFoundError(err)).To(BeTrue())
				Expect(azureEnv.QuotaProvider.HasQuotaFor(ctx, sku)).To(BeTrue())
			})
		})

		It("should list nodeclaim with correct instance type even after capacity error marks offerings unavailable", func() {
			ExpectApplied(ctx, env.Client, nodeClass, nodePool)
			pod := coretest.UnschedulablePod()
			ExpectProvisionedAndWaitForPromises(ctx, env.Client, cluster, cloudProvider, coreProvisioner, azureEnv, pod)
			ExpectScheduled(ctx, env.Client, pod)
			Expect(azureEnv.VirtualMachinesAPI.VirtualMachineCreateOrUpdateBehavior.CalledWithInput.Len()).To(Equal(1))

			// Get the instance type from the created VM
			vm := azureEnv.VirtualMachinesAPI.VirtualMachineCreateOrUpdateBehavior.CalledWithInput.Pop().VM
			vmSize := string(lo.FromPtr(vm.Properties.HardwareProfile.VMSize))
			Expect(vmSize).ToNot(BeEmpty())

			// Simulate a capacity error by marking all offerings for this instance type as unavailable
			for _, zone := range azureEnv.Zones() {
				azureEnv.UnavailableOfferingsCache.MarkUnavailable(ctx, "ZonalAllocationFailure", fake.MakeSKU(vmSize), zone, karpv1.CapacityTypeOnDemand)
				azureEnv.UnavailableOfferingsCache.MarkUnavailable(ctx, "ZonalAllocationFailure", fake.MakeSKU(vmSize), zone, karpv1.CapacityTypeSpot)
			}

			// List should still return the nodeclaim with the correct instance type
			nodeClaims, err := cloudProvider.List(ctx)
			Expect(err).ToNot(HaveOccurred())
			Expect(nodeClaims).To(HaveLen(1))
			validateVMNodeClaim(nodeClaims[0], nodePool)
			Expect(nodeClaims[0].Labels[v1.LabelInstanceTypeStable]).To(Equal(vmSize))
		})
		It("should return an ICE error when there are no instance types to launch", func() {
			// Specify no instance types and expect to receive a capacity error
			nodeClaim.Spec.Requirements = []karpv1.NodeSelectorRequirementWithMinValues{
				{
					Key:      v1.LabelInstanceTypeStable,
					Operator: v1.NodeSelectorOpIn,
					Values:   []string{"doesnotexist"}, // will not match any instance types
				},
			}

			ExpectApplied(ctx, env.Client, nodePool, nodeClass, nodeClaim)
			cloudProviderMachine, err := CreateAndWaitForPromises(ctx, cloudProvider, azureEnv, nodeClaim)
			Expect(corecloudprovider.IsInsufficientCapacityError(err)).To(BeTrue())
			Expect(cloudProviderMachine).To(BeNil())
		})
		Context("Create - Expected Creation Failures", func() {
			It("should return a NodeClassNotReadyError before instance creation when validation marks the NodeClass incompatible with the Kubernetes version", func() {
				nodeClass.Spec.ImageFamily = lo.ToPtr(v1beta1.Ubuntu2404ImageFamily)
				nodeClass.Status.KubernetesVersion = lo.ToPtr("1.31")
				message := `requested image family Ubuntu2404 is not supported with Kubernetes version "1.31"; supported range is >= 1.32.0`
				_, validationErr := status.NewStaticValidationReconciler(azureEnv.DiskEncryptionSetsAPI, nil).Reconcile(ctx, nodeClass)
				Expect(validationErr).ToNot(HaveOccurred())
				Expect(nodeClass.StatusConditions().Get(v1beta1.ConditionTypeValidationSucceeded).Reason).To(Equal(status.ImageFamilyKubernetesVersionIncompatible))
				Expect(nodeClass.StatusConditions().Get(v1beta1.ConditionTypeValidationSucceeded).Message).To(Equal(message))
				Expect(nodeClass.StatusConditions().Get(corestatus.ConditionReady).IsFalse()).To(BeTrue())

				ExpectApplied(ctx, env.Client, nodePool, nodeClass, nodeClaim)
				cloudProviderMachine, err := CreateAndWaitForPromises(ctx, cloudProvider, azureEnv, nodeClaim)
				Expect(err).To(HaveOccurred())
				Expect(corecloudprovider.IsNodeClassNotReadyError(err)).To(BeTrue())
				Expect(err).To(BeAssignableToTypeOf(&corecloudprovider.NodeClassNotReadyError{}))
				Expect(err.Error()).To(ContainSubstring(fmt.Sprintf("%s=False", v1beta1.ConditionTypeValidationSucceeded)))
				Expect(cloudProviderMachine).To(BeNil())
				Expect(azureEnv.VirtualMachinesAPI.VirtualMachineCreateOrUpdateBehavior.CalledWithInput.Len()).To(Equal(0))
				Expect(azureEnv.AKSMachinesAPI.AKSMachineCreateOrUpdateBehavior.CalledWithInput.Len()).To(Equal(0))
			})
		})

		runNodeOverlayCapacityTests(vmNodeOverlayCapacityTestOptions())

		Context("AKS Machine API integration", func() {
			It("should not call writes to AKS Machine API", func() {
				ExpectApplied(ctx, env.Client, nodeClass, nodePool)
				pod := coretest.UnschedulablePod()
				ExpectProvisionedAndWaitForPromises(ctx, env.Client, cluster, cloudProvider, coreProvisioner, azureEnv, pod)
				ExpectScheduled(ctx, env.Client, pod)

				Expect(azureEnv.AKSMachinesAPI.AKSMachineCreateOrUpdateBehavior.CalledWithInput.Len()).To(Equal(0))
			})

			Context("AKS Machines Pool Management", func() {
				It("should handle AKS machines pool not found on each CloudProvider operation", func() {
					// First create a successful VM
					ExpectApplied(ctx, env.Client, nodeClass, nodePool)
					pod := coretest.UnschedulablePod()
					ExpectProvisionedAndWaitForPromises(ctx, env.Client, cluster, cloudProvider, coreProvisioner, azureEnv, pod)
					ExpectScheduled(ctx, env.Client, pod)

					// cloudprovider.List should return vm nodeclaim
					nodeClaims, err := cloudProvider.List(ctx)
					Expect(err).ToNot(HaveOccurred())
					Expect(nodeClaims).To(HaveLen(1))
					validateVMNodeClaim(nodeClaims[0], nodePool)

					// cloudprovider.Delete should be fine also
					err = cloudProvider.Delete(ctx, nodeClaims[0])
					Expect(err).ToNot(HaveOccurred())
				})
			})
		})
	})

	Context("ProvisionMode = AKSScriptless, ManageExistingAKSMachines = true", func() {
		BeforeEach(func() {
			testOptions = test.Options(test.OptionsFields{
				ProvisionMode:             lo.ToPtr(consts.ProvisionModeAKSScriptless),
				ManageExistingAKSMachines: lo.ToPtr(true),
			})
			ctx = coreoptions.ToContext(ctx, coretest.Options())
			ctx = options.ToContext(ctx, testOptions)

			azureEnv = test.NewEnvironment(ctx, env)
			test.ApplyDefaultStatus(nodeClass, env, testOptions.UseSIG)
			cloudProvider = New(azureEnv.InstanceTypesProvider, azureEnv.VMInstanceProvider, azureEnv.AKSMachineProvider, recorder, env.Client, azureEnv.ImageProvider, azureEnv.InstanceTypeStore, coreoptions.FromContext(ctx).LegacyNodeRepair)

			cluster = state.NewCluster(fakeClock, env.Client, cloudProvider)
			coreProvisioner = provisioning.NewProvisioner(env.Client, recorder, cloudProvider, cluster, fakeClock, deviceallocation.NewController(env.Client), virtualpods.NewVirtualPodCache(env.Client))
		})

		AfterEach(func() {
			// Wait for any async polling goroutines to complete before resetting
			cloudProvider.WaitForInstancePromises()
			cluster.Reset()
			azureEnv.Reset(ctx)
		})

		It("should list nodeclaim created by the CloudProvider", func() {
			ExpectApplied(ctx, env.Client, nodeClass, nodePool)
			pod := coretest.UnschedulablePod()
			ExpectProvisionedAndWaitForPromises(ctx, env.Client, cluster, cloudProvider, coreProvisioner, azureEnv, pod)
			ExpectScheduled(ctx, env.Client, pod)
			Expect(azureEnv.VirtualMachinesAPI.VirtualMachineCreateOrUpdateBehavior.CalledWithInput.Len()).To(Equal(1))

			nodeClaims, _ := cloudProvider.List(ctx)
			Expect(azureEnv.AKSMachinesAPI.AKSMachineNewListPagerBehavior.CalledWithInput.Len()).To(Equal(1)) // Expect to be called in case of existing AKS machines
			Expect(azureEnv.AzureResourceGraphAPI.AzureResourceGraphResourcesBehavior.CalledWithInput.Len()).To(Equal(1))
			queryRequest := azureEnv.AzureResourceGraphAPI.AzureResourceGraphResourcesBehavior.CalledWithInput.Pop().Query
			Expect(*queryRequest.Query).To(Equal(instance.GetVMListQueryBuilder(azureEnv.AzureResourceGraphAPI.ResourceGroup).String()))
			Expect(nodeClaims).To(HaveLen(1))
			validateVMNodeClaim(nodeClaims[0], nodePool)
			resp, _ := azureEnv.VirtualMachinesAPI.Get(ctx, azureEnv.AzureResourceGraphAPI.ResourceGroup, nodeClaims[0].Name, nil)
			Expect(resp.VirtualMachine).ToNot(BeNil())
		})
		It("should return an ICE error when there are no instance types to launch", func() {
			// Specify no instance types and expect to receive a capacity error
			nodeClaim.Spec.Requirements = []karpv1.NodeSelectorRequirementWithMinValues{
				{
					Key:      v1.LabelInstanceTypeStable,
					Operator: v1.NodeSelectorOpIn,
					Values:   []string{"doesnotexist"}, // will not match any instance types
				},
			}

			ExpectApplied(ctx, env.Client, nodePool, nodeClass, nodeClaim)
			cloudProviderMachine, err := CreateAndWaitForPromises(ctx, cloudProvider, azureEnv, nodeClaim)
			Expect(corecloudprovider.IsInsufficientCapacityError(err)).To(BeTrue())
			Expect(cloudProviderMachine).To(BeNil())
		})

		runNodeOverlayCapacityTests(vmNodeOverlayCapacityTestOptions())

		Context("AKS Machine API integration", func() {
			It("should not call writes to AKS Machine API", func() {
				ExpectApplied(ctx, env.Client, nodeClass, nodePool)
				pod := coretest.UnschedulablePod()
				ExpectProvisionedAndWaitForPromises(ctx, env.Client, cluster, cloudProvider, coreProvisioner, azureEnv, pod)
				ExpectScheduled(ctx, env.Client, pod)

				Expect(azureEnv.AKSMachinesAPI.AKSMachineCreateOrUpdateBehavior.CalledWithInput.Len()).To(Equal(0))
			})

			Context("AKS Machines Pool Management", func() {
				It("should handle AKS machines pool not found on each CloudProvider operation", func() {
					// First create a successful VM
					ExpectApplied(ctx, env.Client, nodeClass, nodePool)
					pod := coretest.UnschedulablePod()
					ExpectProvisionedAndWaitForPromises(ctx, env.Client, cluster, cloudProvider, coreProvisioner, azureEnv, pod)
					ExpectScheduled(ctx, env.Client, pod)

					// cloudprovider.List should return vm nodeclaim
					nodeClaims, err := cloudProvider.List(ctx)
					Expect(err).ToNot(HaveOccurred())
					Expect(nodeClaims).To(HaveLen(1))
					validateVMNodeClaim(nodeClaims[0], nodePool)

					// cloudprovider.Delete should be fine also
					err = cloudProvider.Delete(ctx, nodeClaims[0])
					Expect(err).ToNot(HaveOccurred())
				})
			})
		})
	})
})
