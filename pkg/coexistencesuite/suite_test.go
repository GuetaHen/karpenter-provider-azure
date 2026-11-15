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

package coexistencesuite_test

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v7"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork"
	"github.com/awslabs/operatorpkg/object"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/record"
	clock "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	corecloudprovider "sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/events"
	coreoptions "sigs.k8s.io/karpenter/pkg/operator/options"
	coretest "sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/test/expectations"
	"sigs.k8s.io/karpenter/pkg/test/v1alpha1"
	nodeutils "sigs.k8s.io/karpenter/pkg/utils/node"
	nodeclaimutils "sigs.k8s.io/karpenter/pkg/utils/nodeclaim"
	nodepoolutils "sigs.k8s.io/karpenter/pkg/utils/nodepool"
	. "sigs.k8s.io/karpenter/pkg/utils/testing"

	"github.com/Azure/karpenter-provider-azure/pkg/apis"
	"github.com/Azure/karpenter-provider-azure/pkg/apis/coexistence"
	"github.com/Azure/karpenter-provider-azure/pkg/apis/v1beta1"
	"github.com/Azure/karpenter-provider-azure/pkg/cloudprovider"
	cloudproviderevents "github.com/Azure/karpenter-provider-azure/pkg/cloudprovider/events"
	"github.com/Azure/karpenter-provider-azure/pkg/consts"
	"github.com/Azure/karpenter-provider-azure/pkg/controllers/nodeclaim/garbagecollection"
	"github.com/Azure/karpenter-provider-azure/pkg/controllers/nodeclaim/inplaceupdate"
	nodeclasshash "github.com/Azure/karpenter-provider-azure/pkg/controllers/nodeclass/hash"
	"github.com/Azure/karpenter-provider-azure/pkg/controllers/nodeclass/status"
	"github.com/Azure/karpenter-provider-azure/pkg/controllers/nodeclass/termination"
	"github.com/Azure/karpenter-provider-azure/pkg/fake"
	"github.com/Azure/karpenter-provider-azure/pkg/operator/options"
	"github.com/Azure/karpenter-provider-azure/pkg/providers/instance"
	"github.com/Azure/karpenter-provider-azure/pkg/providers/launchtemplate"
	"github.com/Azure/karpenter-provider-azure/pkg/providers/loadbalancer"
	"github.com/Azure/karpenter-provider-azure/pkg/providers/regional"
	"github.com/Azure/karpenter-provider-azure/pkg/test"
	azureexpectations "github.com/Azure/karpenter-provider-azure/pkg/test/expectations"
	"github.com/Azure/karpenter-provider-azure/pkg/utils"
)

var (
	ctx           context.Context
	env           *coretest.Environment
	azureEnv      *test.Environment
	testOptions   *options.Options
	fakeClock     *clock.FakeClock
	recorder      *record.FakeRecorder
	cloudProvider *cloudprovider.CloudProvider
)

const (
	ownPool   = "own-pool"
	stockPool = "stock-pool"
)

// TestMain runs this package in coexistence mode. The mode is read once at process start, so a
// plain test run starts this test binary again with COEXISTENCE_MODE=true.
func TestMain(m *testing.M) {
	if coexistence.Enabled() {
		os.Exit(m.Run())
	}
	cmd := exec.Command(os.Args[0], os.Args[1:]...) //nolint:gosec // re-runs this test binary
	cmd.Env = append(os.Environ(), coexistence.EnvVar+"=true")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	err := cmd.Run()
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		os.Exit(0)
	case errors.As(err, &exitErr):
		os.Exit(exitErr.ExitCode())
	default:
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func TestCoexistence(t *testing.T) {
	ctx = TestContextWithLogger(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "Coexistence")
}

var _ = BeforeSuite(func() {
	ctx = coreoptions.ToContext(ctx, coretest.Options())
	testOptions = test.Options()
	ctx = options.ToContext(ctx, testOptions)
	env = coretest.NewEnvironment(
		coretest.WithCRDs(apis.CRDs...),
		// The default API group's AKSNodeClass, which a controller outside coexistence mode serves.
		coretest.WithCRDs(object.Unmarshal[apiextensionsv1.CustomResourceDefinition](apis.AKSNodeClassCRD)),
		coretest.WithCRDs(v1alpha1.CRDs...),
		coretest.WithFieldIndexers(test.AKSNodeClassFieldIndexer(ctx)),
	)
	azureEnv = test.NewEnvironment(ctx, env)
	fakeClock = clock.NewFakeClock(time.Now())
	recorder = record.NewFakeRecorder(100)
	cloudProvider = cloudprovider.New(azureEnv.InstanceTypesProvider, azureEnv.VMInstanceProvider, azureEnv.AKSMachineProvider,
		events.NewRecorder(recorder), env.Client, azureEnv.ImageProvider, azureEnv.InstanceTypeStore, coreoptions.FromContext(ctx).LegacyNodeRepair)
})

var _ = AfterSuite(func() {
	Expect(env.Stop()).To(Succeed(), "Failed to stop environment")
})

var _ = BeforeEach(func() {
	azureEnv.Reset(ctx)
	for len(recorder.Events) > 0 {
		<-recorder.Events
	}
})

var _ = AfterEach(func() {
	ExpectCleanedUp(ctx, env.Client)
})

func ownRef(nodeClass *v1beta1.AKSNodeClass) *karpv1.NodeClassReference {
	return &karpv1.NodeClassReference{Group: object.GVK(nodeClass).Group, Kind: object.GVK(nodeClass).Kind, Name: nodeClass.Name}
}

// foreignRef points at an AKSNodeClass that another controller serves under the default API group.
func foreignRef(name string) *karpv1.NodeClassReference {
	return &karpv1.NodeClassReference{Group: apis.Group, Kind: v1beta1.AKSNodeClassKind, Name: name}
}

func nodePoolFor(ref *karpv1.NodeClassReference) *karpv1.NodePool {
	return coretest.NodePool(karpv1.NodePool{Spec: karpv1.NodePoolSpec{Template: karpv1.NodeClaimTemplate{Spec: karpv1.NodeClaimTemplateSpec{NodeClassRef: ref}}}})
}

func registeredClaim(ref *karpv1.NodeClassReference, providerID string) *karpv1.NodeClaim {
	claim := coretest.NodeClaim(karpv1.NodeClaim{Spec: karpv1.NodeClaimSpec{NodeClassRef: ref}, Status: karpv1.NodeClaimStatus{ProviderID: providerID}})
	claim.StatusConditions().SetTrue(karpv1.ConditionTypeLaunched)
	claim.StatusConditions().SetTrue(karpv1.ConditionTypeRegistered)
	return claim
}

func staleHashAnnotations() map[string]string {
	return map[string]string{v1beta1.AnnotationAKSNodeClassHashVersion: "stale", v1beta1.AnnotationAKSNodeClassHash: "stale"}
}

func ownTags() map[string]*string {
	return map[string]*string{
		launchtemplate.KarpenterManagedTagKey: lo.ToPtr("test-cluster"),
		launchtemplate.NodePoolTagKey:         lo.ToPtr(ownPool),
		launchtemplate.OwnerTagKey:            lo.ToPtr(coexistence.Group),
	}
}

// oldVM stores a VM that is older than the garbage collection grace period.
func oldVM(name string, tags map[string]*string) *armcompute.VirtualMachine {
	vm := test.VirtualMachine(test.VirtualMachineOptions{Name: name, Tags: tags, Properties: &armcompute.VirtualMachineProperties{TimeCreated: lo.ToPtr(time.Now().Add(-10 * time.Minute))}})
	azureEnv.VirtualMachinesAPI.Instances.Store(lo.FromPtr(vm.ID), *vm)
	return vm
}

func storedNIC(name string, tags map[string]*string) *armnetwork.Interface {
	nic := test.Interface(test.InterfaceOptions{Name: name, Tags: tags})
	azureEnv.NetworkInterfacesAPI.NetworkInterfaces.Store(lo.FromPtr(nic.ID), *nic)
	return nic
}

func vmExists(name string) bool {
	_, err := azureEnv.VMInstanceProvider.Get(ctx, name)
	if corecloudprovider.IsNodeClaimNotFoundError(err) {
		return false
	}
	Expect(err).ToNot(HaveOccurred())
	return true
}

// failingReader returns its error from every read.
type failingReader struct{ err error }

func (r failingReader) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return r.err
}

func (r failingReader) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return r.err
}

func nicExists(nic *armnetwork.Interface) bool {
	_, ok := azureEnv.NetworkInterfacesAPI.NetworkInterfaces.Load(lo.FromPtr(nic.ID))
	return ok
}

var _ = Describe("Coexistence mode", func() {
	var nodeClass *v1beta1.AKSNodeClass

	BeforeEach(func() {
		nodeClass = test.AKSNodeClass()
	})

	It("serves AKSNodeClass under its own API group and labels nodes with it", func() {
		Expect(object.GVK(nodeClass).Group).To(Equal(coexistence.Group))
		Expect(karpv1.NodeClassLabelKey(object.GVK(nodeClass).GroupKind())).To(Equal(coexistence.Group + "/aksnodeclass"))
	})

	It("treats only NodePools, NodeClaims and Nodes of its own API group as managed", func() {
		Expect(nodepoolutils.IsManaged(nodePoolFor(ownRef(nodeClass)), cloudProvider)).To(BeTrue())
		Expect(nodepoolutils.IsManaged(nodePoolFor(foreignRef(nodeClass.Name)), cloudProvider)).To(BeFalse())
		Expect(nodeclaimutils.IsManaged(registeredClaim(ownRef(nodeClass), ""), cloudProvider)).To(BeTrue())
		Expect(nodeclaimutils.IsManaged(registeredClaim(foreignRef(nodeClass.Name), ""), cloudProvider)).To(BeFalse())
		ownNode := coretest.Node(coretest.NodeOptions{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{karpv1.NodeClassLabelKey(object.GVK(nodeClass).GroupKind()): nodeClass.Name}}})
		foreignNode := coretest.Node(coretest.NodeOptions{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{apis.Group + "/aksnodeclass": nodeClass.Name}}})
		Expect(nodeutils.IsManaged(ownNode, cloudProvider)).To(BeTrue())
		Expect(nodeutils.IsManaged(foreignNode, cloudProvider)).To(BeFalse())
	})

	It("gives another controller's NodePool no instance types and records no resolution event", func() {
		foreignPool := nodePoolFor(foreignRef("managed-default"))
		instanceTypes, err := cloudProvider.GetInstanceTypes(ctx, foreignPool)
		Expect(err).ToNot(HaveOccurred())
		Expect(instanceTypes).To(BeEmpty())
		Expect(cloudProvider.IsDrifted(ctx, registeredClaim(foreignRef("managed-default"), ""))).To(BeEmpty())
		Expect(recorder.Events).To(BeEmpty())
	})

	It("garbage collects only its own orphaned VMs", func() {
		oldVM("aks-stock-vm", test.ManagedTags(stockPool))
		oldVM("aks-stock-machine-vm", test.ManagedTagsAKSMachine(stockPool, "stock-claim"))
		oldVM("aks-own-vm", ownTags())
		ExpectSingletonReconciled(ctx, garbagecollection.NewInstance(env.Client, cloudProvider, fakeClock))
		Expect(vmExists("aks-stock-vm")).To(BeTrue())
		Expect(vmExists("aks-stock-machine-vm")).To(BeTrue())
		Expect(vmExists("aks-own-vm")).To(BeFalse())
	})

	It("garbage collects only its own orphaned network interfaces", func() {
		stockNIC := storedNIC("aks-stock-nic", test.ManagedTags(stockPool))
		stockMachineNIC := storedNIC("aks-stock-machine-nic", test.ManagedTagsAKSMachine(stockPool, "missing-claim"))
		ownNIC := storedNIC("aks-own-nic", ownTags())
		ExpectSingletonReconciled(ctx, garbagecollection.NewNetworkInterface(env.Client, azureEnv.VMInstanceProvider))
		Expect(nicExists(stockNIC)).To(BeTrue())
		Expect(nicExists(stockMachineNIC)).To(BeTrue())
		Expect(nicExists(ownNIC)).To(BeFalse())
	})

	It("never adopts or deletes a VM or NIC that another controller created", func() {
		claim := registeredClaim(ownRef(nodeClass), "")
		name := instance.GenerateResourceName(claim.Name)
		oldVM(name, test.ManagedTags(stockPool))
		stockNIC := storedNIC(name, test.ManagedTags(stockPool))
		_, err := azureEnv.VMInstanceProvider.BeginCreate(ctx, nodeClass, claim, nil)
		Expect(err).To(MatchError(ContainSubstring("not created by this controller")))
		Expect(azureEnv.VMInstanceProvider.Delete(ctx, claim, name)).To(MatchError(ContainSubstring("not created by this controller")))
		Expect(azureEnv.VMInstanceProvider.DeleteNic(ctx, name)).To(MatchError(ContainSubstring("not created by this controller")))
		Expect(vmExists(name)).To(BeTrue())
		Expect(nicExists(stockNIC)).To(BeTrue())
	})

	It("never updates another controller's NodeClaims or VMs in place", func() {
		ExpectApplied(ctx, env.Client, nodeClass)
		vm := oldVM("aks-stock-claim", test.ManagedTags(stockPool))
		providerID := utils.VMResourceIDToProviderID(ctx, lo.FromPtr(vm.ID))
		foreignClaim := registeredClaim(foreignRef(nodeClass.Name), providerID)
		ownClaimOnForeignVM := registeredClaim(ownRef(nodeClass), providerID)
		ExpectApplied(ctx, env.Client, foreignClaim, ownClaimOnForeignVM)
		controller := inplaceupdate.NewController(env.Client, azureEnv.VMInstanceProvider, azureEnv.AKSMachineProvider)
		ExpectObjectReconciled(ctx, env.Client, controller, foreignClaim)
		ExpectObjectReconciled(ctx, env.Client, controller, ownClaimOnForeignVM)
		Expect(azureEnv.VirtualMachinesAPI.VirtualMachineUpdateBehavior.Calls()).To(BeZero())
		Expect(ExpectExists(ctx, env.Client, foreignClaim).Annotations).ToNot(HaveKey(v1beta1.AnnotationInPlaceUpdateHash))
		Expect(ExpectExists(ctx, env.Client, ownClaimOnForeignVM).Annotations).ToNot(HaveKey(v1beta1.AnnotationInPlaceUpdateHash))
	})

	It("rewrites node class hashes only on its own NodeClaims", func() {
		nodeClass.Annotations = lo.Assign(nodeClass.Annotations, map[string]string{v1beta1.AnnotationAKSNodeClassHashVersion: "stale"})
		ExpectApplied(ctx, env.Client, nodeClass)
		foreignClaim := coretest.NodeClaim(karpv1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Annotations: staleHashAnnotations()}, Spec: karpv1.NodeClaimSpec{NodeClassRef: foreignRef(nodeClass.Name)}})
		ownClaim := coretest.NodeClaim(karpv1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Annotations: staleHashAnnotations()}, Spec: karpv1.NodeClaimSpec{NodeClassRef: ownRef(nodeClass)}})
		ExpectApplied(ctx, env.Client, foreignClaim, ownClaim)
		ExpectObjectReconciled(ctx, env.Client, nodeclasshash.NewController(env.Client), nodeClass)
		Expect(ExpectExists(ctx, env.Client, foreignClaim).Annotations).To(HaveKeyWithValue(v1beta1.AnnotationAKSNodeClassHashVersion, "stale"))
		Expect(ExpectExists(ctx, env.Client, ownClaim).Annotations).To(HaveKeyWithValue(v1beta1.AnnotationAKSNodeClassHashVersion, v1beta1.AKSNodeClassHashVersion))
	})

	It("removes its finalizer although another controller's NodeClaim uses the same node class name", func() {
		nodeClass.Finalizers = []string{v1beta1.TerminationFinalizer}
		foreignClaim := coretest.NodeClaim(karpv1.NodeClaim{Spec: karpv1.NodeClaimSpec{NodeClassRef: foreignRef(nodeClass.Name)}})
		ExpectApplied(ctx, env.Client, nodeClass, foreignClaim)
		Expect(env.Client.Delete(ctx, nodeClass)).To(Succeed())
		ExpectObjectReconciled(ctx, env.Client, termination.NewController(env.Client, events.NewRecorder(recorder)), nodeClass)
		ExpectNotFound(ctx, env.Client, nodeClass)
		ExpectExists(ctx, env.Client, foreignClaim)
	})

	It("marks its node class not ready while an AKSNodeClass of the default API group has the same name", func() {
		test.ApplyDefaultStatus(nodeClass, env, testOptions.UseSIG)
		ExpectApplied(ctx, env.Client, nodeClass)
		// No NodePool or NodeClaim references the other class: its existence alone lets a controller
		// outside coexistence mode rewrite the tags of this controller's VMs.
		foreignClass := &unstructured.Unstructured{Object: map[string]interface{}{"spec": map[string]interface{}{}}}
		foreignClass.SetGroupVersionKind(schema.GroupVersionKind{Group: apis.Group, Version: "v1beta1", Kind: v1beta1.AKSNodeClassKind})
		foreignClass.SetName(nodeClass.Name)
		Expect(env.Client.Create(ctx, foreignClass)).To(Succeed())
		DeferCleanup(func() { Expect(client.IgnoreNotFound(env.Client.Delete(ctx, foreignClass))).To(Succeed()) })
		// Production passes the manager's uncached API reader.
		apiReader := lo.Must(client.New(env.Config, client.Options{}))
		reconciler := status.NewStaticValidationReconciler(azureEnv.DiskEncryptionSetsAPI, nil).WithNodeClassReader(apiReader)
		result, err := reconciler.Reconcile(ctx, nodeClass)
		Expect(err).ToNot(HaveOccurred())
		condition := nodeClass.StatusConditions().Get(v1beta1.ConditionTypeValidationSucceeded)
		Expect(condition.IsFalse()).To(BeTrue())
		Expect(condition.Reason).To(Equal(status.ForeignNodeClassNameConflict))
		Expect(condition.Message).To(ContainSubstring(apis.Group))
		Expect(result.RequeueAfter).To(Equal(status.ValidationFailureRequeueInterval))

		Expect(env.Client.Delete(ctx, foreignClass)).To(Succeed())
		Eventually(func(g Gomega) {
			recheck, recheckErr := reconciler.Reconcile(ctx, nodeClass)
			g.Expect(recheckErr).ToNot(HaveOccurred())
			g.Expect(nodeClass.StatusConditions().Get(v1beta1.ConditionTypeValidationSucceeded).IsTrue()).To(BeTrue())
			g.Expect(recheck.RequeueAfter).To(Equal(status.ForeignNodeClassRecheckInterval))
		}).Should(Succeed())
	})

	It("passes the name check when the default API group is not served, and surfaces other read errors", func() {
		test.ApplyDefaultStatus(nodeClass, env, testOptions.UseSIG)
		notServed := &meta.NoKindMatchError{GroupKind: schema.GroupKind{Group: apis.Group, Kind: v1beta1.AKSNodeClassKind}}
		_, err := status.NewStaticValidationReconciler(azureEnv.DiskEncryptionSetsAPI, nil).WithNodeClassReader(failingReader{err: notServed}).Reconcile(ctx, nodeClass)
		Expect(err).ToNot(HaveOccurred())
		Expect(nodeClass.StatusConditions().Get(v1beta1.ConditionTypeValidationSucceeded).IsTrue()).To(BeTrue())
		forbidden := apierrors.NewForbidden(schema.GroupResource{Group: apis.Group, Resource: "aksnodeclasses"}, nodeClass.Name, errors.New("synthetic denial"))
		_, err = status.NewStaticValidationReconciler(azureEnv.DiskEncryptionSetsAPI, nil).WithNodeClassReader(failingReader{err: forbidden}).Reconcile(ctx, nodeClass)
		Expect(err).ToNot(HaveOccurred())
		Expect(nodeClass.StatusConditions().Get(v1beta1.ConditionTypeValidationSucceeded).Reason).To(Equal(status.ForeignNodeClassCheckForbidden), "a missing get permission must not pass the check")
		_, err = status.NewStaticValidationReconciler(azureEnv.DiskEncryptionSetsAPI, nil).WithNodeClassReader(failingReader{err: errors.New("synthetic outage")}).Reconcile(ctx, nodeClass)
		Expect(err).To(MatchError(ContainSubstring("synthetic outage")))
	})

	It("records an event when it refuses to delete a VM that lost its owner tag", func() {
		claim := registeredClaim(ownRef(nodeClass), "")
		name := instance.GenerateResourceName(claim.Name)
		// What an in-place update outside coexistence mode leaves behind: this controller's VM without the owner tag.
		vm := oldVM(name, lo.OmitByKeys(ownTags(), []string{launchtemplate.OwnerTagKey}))
		claim.Status.ProviderID = utils.VMResourceIDToProviderID(ctx, lo.FromPtr(vm.ID))
		err := cloudProvider.Delete(ctx, claim)
		Expect(instance.IsNotOwnedError(err)).To(BeTrue())
		Expect(err).To(MatchError(ContainSubstring(launchtemplate.OwnerTagKey + "=" + coexistence.Group)))
		Expect(vmExists(name)).To(BeTrue())
		Expect(recorder.Events).To(Receive(ContainSubstring(cloudproviderevents.VirtualMachineNotOwnedReason)))
	})

	It("stamps its ownership and drops tags that would expose its resources to another installation", func() {
		nodeClass.Spec.Tags = map[string]string{
			"karpenter.sh/nodepool":        "forged",
			"team":                         "karpenter.sh_nodepool",
			launchtemplate.OwnerTagKey:     "forged",
			"KARPENTER.AZURE.COM_NODEPOOL": "forged",
			"cost-center":                  "kept",
		}
		claim := coretest.NodeClaim(karpv1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{karpv1.NodePoolLabelKey: ownPool}}})
		tags := lo.MapValues(launchtemplate.Tags(testOptions, nodeClass, claim), func(value *string, _ string) string { return lo.FromPtr(value) })
		Expect(tags).To(HaveKeyWithValue(launchtemplate.OwnerTagKey, coexistence.Group))
		Expect(tags).To(HaveKeyWithValue(launchtemplate.CoexistenceNodePoolTagKey, ownPool))
		Expect(tags).To(HaveKeyWithValue("cost-center", "kept"))
		Expect(tags).ToNot(HaveKey(launchtemplate.DefaultNodePoolTagKey))
		Expect(tags).ToNot(HaveKey("team"))
		Expect(tags).ToNot(HaveKey("KARPENTER.AZURE.COM_NODEPOOL"))
	})

	It("lists only resources that carry its owner tag", func() {
		resourceGroup := strings.ToLower(testOptions.NodeResourceGroup)
		owned := ` | where tags has_cs "karpenter.azure.com_nodepool" | where tostring(tags["karpenter.azure.com_owner"]) == "multiregion.io"`
		vmPrefix := `Resources | where type == "microsoft.compute/virtualmachines" | where resourceGroup == "` + resourceGroup + `"`
		nicPrefix := `Resources | where type == "microsoft.network/networkinterfaces" | where resourceGroup == "` + resourceGroup + `"`
		noMachines := ` | where not(tags has_cs "karpenter.azure.com_aksmachine_nodeclaim")`
		Expect(instance.GetVMListQueryBuilder(testOptions.NodeResourceGroup).String()).To(Equal(vmPrefix + owned + noMachines))
		Expect(instance.GetNICListQueryBuilder(testOptions.NodeResourceGroup, false).String()).To(Equal(nicPrefix + owned + noMachines))
		Expect(instance.GetNICListQueryBuilder(testOptions.NodeResourceGroup, true).String()).To(Equal(nicPrefix + owned))
	})

	It("gives a NodeClass without regions ordinary home nodes while regional pools use an external pod network", func() {
		multiRegion, externalPodNetwork := testOptions.EnableMultiRegion, testOptions.MultiRegionExternalPodNetwork
		plugin, pluginMode, dataplane := testOptions.NetworkPlugin, testOptions.NetworkPluginMode, testOptions.NetworkDataplane
		DeferCleanup(func() {
			testOptions.EnableMultiRegion, testOptions.MultiRegionExternalPodNetwork = multiRegion, externalPodNetwork
			testOptions.NetworkPlugin, testOptions.NetworkPluginMode, testOptions.NetworkDataplane = plugin, pluginMode, dataplane
		})
		testOptions.EnableMultiRegion, testOptions.MultiRegionExternalPodNetwork = true, true
		testOptions.NetworkPlugin, testOptions.NetworkPluginMode = consts.NetworkPluginAzure, consts.NetworkPluginModeOverlay
		testOptions.NetworkDataplane = consts.NetworkDataplaneCilium
		// The operator wraps the providers like this whenever multi-region provisioning is enabled.
		var regionCalls atomic.Int32
		registry := regional.New(fake.Region, &regional.Providers{
			Types: azureEnv.InstanceTypesProvider, Images: azureEnv.ImageProvider, Instances: azureEnv.VMInstanceProvider, Quota: azureEnv.QuotaProvider,
		}, func(string) *regional.Providers {
			regionCalls.Add(1)
			return nil
		}, func(context.Context, v1beta1.Region) error {
			regionCalls.Add(1)
			return nil
		})
		homeProvider := cloudprovider.New(&regional.InstanceTypes{Provider: azureEnv.InstanceTypesProvider, Registry: registry},
			&regional.Instances{VMProvider: azureEnv.VMInstanceProvider, Registry: registry, KubeClient: env.Client},
			azureEnv.AKSMachineProvider, events.NewRecorder(recorder), env.Client,
			&regional.Images{NodeImageProvider: azureEnv.ImageProvider, Registry: registry}, azureEnv.InstanceTypeStore, false)
		DeferCleanup(homeProvider.WaitForInstancePromises)
		for _, lb := range []armnetwork.LoadBalancer{
			test.MakeStandardLoadBalancer(testOptions.NodeResourceGroup, loadbalancer.SLBName, true),
			test.MakeStandardLoadBalancer(testOptions.NodeResourceGroup, loadbalancer.InternalSLBName, false),
		} {
			azureEnv.LoadBalancersAPI.LoadBalancers.Store(lo.FromPtr(lb.ID), lb)
		}
		nodeClass.Spec.VNETSubnetID = nil
		test.ApplyDefaultStatus(nodeClass, env, testOptions.UseSIG)
		nodePool := nodePoolFor(ownRef(nodeClass))
		claim := coretest.NodeClaim(karpv1.NodeClaim{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{karpv1.NodePoolLabelKey: nodePool.Name}},
			Spec:       karpv1.NodeClaimSpec{NodeClassRef: ownRef(nodeClass)},
		})
		ExpectApplied(ctx, env.Client, nodeClass, nodePool, claim)

		created, err := azureexpectations.CreateAndWaitForPromises(ctx, homeProvider, azureEnv, claim)
		Expect(err).ToNot(HaveOccurred())
		Expect(regionCalls.Load()).To(BeZero(), "a NodeClass without regions never loads or validates a region")
		Expect(created.Labels).ToNot(HaveKey(corev1.LabelNodeExcludeBalancers))
		Expect(ExpectExists(ctx, env.Client, claim).Annotations).ToNot(HaveKey(regional.PlacementAnnotation))
		input := azureEnv.VirtualMachinesAPI.VirtualMachineCreateOrUpdateBehavior.CalledWithInput.Pop()
		Expect(lo.FromPtr(input.VM.Location)).To(Equal(fake.Region))
		customData := string(lo.Must(base64.StdEncoding.DecodeString(lo.FromPtr(input.VM.Properties.OSProfile.CustomData))))
		for _, label := range []string{
			"kubernetes.azure.com/network-subnet=aks-subnet",
			"kubernetes.azure.com/nodenetwork-vnetguid=" + testOptions.VnetGUID,
			"kubernetes.azure.com/azure-cni-overlay=true",
			"kubernetes.azure.com/podnetwork-type=overlay",
		} {
			Expect(customData).To(ContainSubstring(label))
		}
		Expect(customData).ToNot(ContainSubstring(corev1.LabelNodeExcludeBalancers))
		nic, err := azureEnv.NetworkInterfacesAPI.Get(ctx, testOptions.NodeResourceGroup, input.VMName, nil)
		Expect(err).ToNot(HaveOccurred())
		for _, config := range nic.Properties.IPConfigurations {
			Expect(lo.FromPtr(config.Properties.Subnet.ID)).To(Equal(testOptions.SubnetID))
			Expect(config.Properties.LoadBalancerBackendAddressPools).ToNot(BeEmpty(), "home nodes join the cluster load balancers")
		}
	})
})
