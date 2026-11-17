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
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	toolscache "k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

var startupTaint = corev1.Taint{Key: DefaultStartupTaintKey, Effect: corev1.TaintEffectNoSchedule}

// fakeNode stands for the node that the agent runs on: its instance metadata, its tunnel, its CNI
// configuration and its Cilium agent. It records the calls of the agent in order.
type fakeNode struct {
	mu          sync.Mutex
	metadata    InstanceMetadata
	metadataErr error
	tunnelErr   error
	cniErr      error
	ciliumErr   error
	calls       []string
	plans       []Plan
	// applying, when set, runs inside Apply after the call is recorded.
	applying func()
}

func (f *fakeNode) Get(context.Context) (InstanceMetadata, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "metadata")
	return f.metadata, f.metadataErr
}

func (f *fakeNode) Apply(plan Plan) error {
	f.mu.Lock()
	f.calls = append(f.calls, "tunnel")
	f.plans = append(f.plans, plan)
	err, applying := f.tunnelErr, f.applying
	f.mu.Unlock()
	if applying != nil {
		applying()
	}
	return err
}

func (f *fakeNode) Ensure(podRange netip.Prefix) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "cni "+podRange.String())
	return f.cniErr
}

func (f *fakeNode) Check(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "cilium")
	return f.ciliumErr
}

// takeCalls returns the calls recorded since the last takeCalls.
func (f *fakeNode) takeCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	calls := f.calls
	f.calls = nil
	return calls
}

// count returns how many calls of one kind have been recorded.
func (f *fakeNode) count(call string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == call {
			n++
		}
	}
	return n
}

// twoNodes returns a cluster with the home node home-1 and the regional node regional-1.
func twoNodes() *cluster {
	cl := &cluster{}
	cl.add("home-1", "10.0.0.4", "10.244.1.0/24", "")
	cl.add("regional-1", "10.1.0.4", "10.244.2.0/24", "westus2")
	return cl
}

// newAgentTest returns the agent of the node own of cl, whose Node carries exactly taints. Its client
// is a fake that holds cl and a cilium-config that the agent accepts, and instance metadata
// describes the VM of own.
func newAgentTest(t *testing.T, cl *cluster, own string, funcs interceptor.Funcs, taints ...corev1.Taint) (*Agent, *KubeCluster, *fakeNode) {
	t.Helper()
	f := &fakeNode{}
	objs := make([]client.Object, 0, len(cl.nodes)+len(cl.configs)+len(cl.claims)+1)
	objs = append(objs, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "kube-system", Name: "cilium-config"},
		Data: map[string]string{"enable-host-legacy-routing": "true", "cni-exclusive": "false"}})
	for i := range cl.nodes {
		node := &cl.nodes[i]
		if node.Name == own {
			node.Spec.Taints = taints
			ip, err := internalIPv4(node)
			requireNoError(t, err)
			f.metadata = InstanceMetadata{VMID: node.Status.NodeInfo.SystemUUID, Location: node.Labels[corev1.LabelTopologyRegion],
				SubscriptionID: "00000000-0000-0000-0000-000000000001", ResourceGroupName: "cluster-nodes", PrivateIP: ip}
		}
		objs = append(objs, node)
	}
	for i := range cl.configs {
		objs = append(objs, &cl.configs[i])
	}
	for i := range cl.claims {
		objs = append(objs, &cl.claims[i])
	}
	kube := fakeKubeCluster(t, funcs, objs...)
	settings := &Settings{Config: *testConfig(t), NodeName: own, StartupTaintKey: DefaultStartupTaintKey, ProbeAddress: DefaultProbeAddress}
	return NewAgent(settings, kube, f, f, f, f, logr.Discard()), kube, f
}

// taintKeys returns the keys of the taints of the Node name, in order.
func taintKeys(t *testing.T, kube *KubeCluster, name string) []string {
	t.Helper()
	node, err := kube.GetNode(context.Background(), name)
	requireNoError(t, err)
	keys := make([]string, 0, len(node.Spec.Taints))
	for _, taint := range node.Spec.Taints {
		keys = append(keys, taint.Key)
	}
	return keys
}

// peerNames returns the names of the peers of plan.
func peerNames(plan Plan) []string {
	names := make([]string, 0, len(plan.Peers))
	for _, peer := range plan.Peers {
		names = append(names, peer.Name)
	}
	return names
}

// failConfigMapGets returns interceptors that answer every Get of a ConfigMap with err.
func failConfigMapGets(err error) interceptor.Funcs {
	return interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
		if _, ok := obj.(*corev1.ConfigMap); ok {
			return err
		}
		return c.Get(ctx, key, obj, opts...)
	}}
}

// changeBeforePatches returns interceptors that count the patches in patches and, before each of the
// first n, let change alter the patched Node in the store, so that the patch conflicts.
func changeBeforePatches(n int, patches *int, change func(context.Context, client.WithWatch, *corev1.Node) error) interceptor.Funcs {
	return interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
		*patches++
		if *patches <= n {
			node := &corev1.Node{}
			if err := c.Get(ctx, client.ObjectKeyFromObject(obj), node); err != nil {
				return err
			}
			if err := change(ctx, c, node); err != nil {
				return err
			}
		}
		return c.Patch(ctx, obj, patch, opts...)
	}}
}

// relabel returns a change that sets a label of a Node.
func relabel(key, value string) func(context.Context, client.WithWatch, *corev1.Node) error {
	return func(ctx context.Context, c client.WithWatch, node *corev1.Node) error {
		node.Labels[key] = value
		return c.Update(ctx, node)
	}
}

// replaceNode deletes a Node and creates it again with another UID, as for a new VM of the same name.
func replaceNode(ctx context.Context, c client.WithWatch, node *corev1.Node) error {
	if err := c.Delete(ctx, node); err != nil {
		return err
	}
	fresh := node.DeepCopy()
	fresh.UID, fresh.ResourceVersion = "replaced-uid", ""
	if err := c.Create(ctx, fresh); err != nil {
		return err
	}
	// The fake client may give a new object the resourceVersion that the deleted one started with.
	fresh.Labels["example.com/replaced"] = "true"
	return c.Update(ctx, fresh)
}

// fakeInformers hands out informers that record the event handlers added to them.
type fakeInformers struct {
	err      error
	objects  []client.Object
	handlers []toolscache.ResourceEventHandler
}

func (f *fakeInformers) GetInformer(_ context.Context, obj client.Object, _ ...cache.InformerGetOption) (cache.Informer, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.objects = append(f.objects, obj)
	return &fakeInformer{informers: f}, nil
}

// fakeInformer records the event handlers added to it. It implements no other method of an informer.
type fakeInformer struct {
	cache.Informer
	informers *fakeInformers
}

func (f *fakeInformer) AddEventHandler(handler toolscache.ResourceEventHandler) (toolscache.ResourceEventHandlerRegistration, error) {
	f.informers.handlers = append(f.informers.handlers, handler)
	return nil, nil
}

// runAgent runs a in the background, and returns a function that stops it and waits until it returns.
func runAgent(a *Agent) func() {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.Run(ctx)
	}()
	return func() {
		cancel()
		<-done
	}
}

// countPasses returns interceptors that count the passes of the agent in passes. Every pass lists
// the Nodes once.
func countPasses(passes *atomic.Int32) interceptor.Funcs {
	return interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
		if _, ok := list.(*corev1.NodeList); ok {
			passes.Add(1)
		}
		return c.List(ctx, list, opts...)
	}}
}

// testLog returns a logger that records its lines as JSON, and a function that returns the lines.
func testLog() (logr.Logger, func() []string) {
	var mu sync.Mutex
	var lines []string
	log := funcr.NewJSON(func(obj string) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, obj)
	}, funcr.Options{})
	return log, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(lines)
	}
}

// countLines counts the lines that hold all of the substrings.
func countLines(lines []string, substrings ...string) int {
	n := 0
	for _, line := range lines {
		if !slices.ContainsFunc(substrings, func(s string) bool { return !strings.Contains(line, s) }) {
			n++
		}
	}
	return n
}

func TestAgentRemovesTheStartupTaintFromAHomeNode(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	other := corev1.Taint{Key: "example.com/other", Effect: corev1.TaintEffectNoSchedule}
	a, kube, f := newAgentTest(t, twoNodes(), "home-1", interceptor.Funcs{}, other, startupTaint, karpv1.UnregisteredNoExecuteTaint)
	// A home node does not wait for its Cilium agent.
	f.ciliumErr = errors.New("connection refused")

	a.Pass(ctx, true)
	g.Expect(a.Ready()).To(Succeed())
	g.Expect(f.takeCalls()).To(Equal([]string{"metadata", "tunnel"}))
	g.Expect(f.plans).To(HaveLen(1))
	g.Expect(f.plans[0].Regional).To(BeFalse())
	g.Expect(peerNames(f.plans[0])).To(Equal([]string{"regional-1"}))
	// Karpenter's registration puts the startup taints on the Node again, so the taint stays until then.
	g.Expect(taintKeys(t, kube, "home-1")).To(Equal([]string{"example.com/other", DefaultStartupTaintKey, karpv1.UnregisteredTaintKey}))

	node, err := kube.GetNode(ctx, "home-1")
	g.Expect(err).ToNot(HaveOccurred())
	node.Spec.Taints = slices.DeleteFunc(node.Spec.Taints, func(taint corev1.Taint) bool { return taint.Key == karpv1.UnregisteredTaintKey })
	g.Expect(kube.Client.Update(ctx, node)).To(Succeed())
	a.Pass(ctx, false)
	g.Expect(a.Ready()).To(Succeed())
	g.Expect(f.takeCalls()).To(BeEmpty())
	g.Expect(taintKeys(t, kube, "home-1")).To(Equal([]string{"example.com/other"}))

	// A later failure does not put the taint back on a home node.
	f.tunnelErr = errors.New("device busy")
	a.Pass(ctx, true)
	g.Expect(a.Ready()).To(MatchError(ContainSubstring("programming the tunnel: device busy")))
	g.Expect(taintKeys(t, kube, "home-1")).To(Equal([]string{"example.com/other"}))
}

func TestAgentHandlesTheStartupTaintOfARegionalNode(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	a, kube, f := newAgentTest(t, twoNodes(), "regional-1", interceptor.Funcs{}, startupTaint)
	f.ciliumErr = errors.New("connection refused")

	// The CNI configuration waits for a healthy Cilium agent.
	a.Pass(ctx, true)
	g.Expect(a.Ready()).To(MatchError(ContainSubstring("the Cilium agent of the node is not healthy: connection refused")))
	g.Expect(f.takeCalls()).To(Equal([]string{"metadata", "tunnel", "cilium"}))
	g.Expect(f.plans[0].Regional).To(BeTrue())
	g.Expect(peerNames(f.plans[0])).To(Equal([]string{"home-1"}))
	g.Expect(taintKeys(t, kube, "regional-1")).To(Equal([]string{DefaultStartupTaintKey}))

	f.ciliumErr = nil
	a.Pass(ctx, false)
	g.Expect(a.Ready()).To(Succeed())
	g.Expect(f.takeCalls()).To(Equal([]string{"cilium", "cni 10.244.2.0/24"}))
	g.Expect(taintKeys(t, kube, "regional-1")).To(BeEmpty())

	// A regional node that is no longer ready gets the taint back, with the effect NoSchedule.
	f.cniErr = errors.New("the CNI configuration 00-other.conflist sorts before " + CNIConfigName)
	a.Pass(ctx, false)
	g.Expect(a.Ready()).To(MatchError(ContainSubstring("00-other.conflist")))
	node, err := kube.GetNode(ctx, "regional-1")
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(node.Spec.Taints).To(Equal([]corev1.Taint{startupTaint}))

	// But not while its Node is being deleted.
	f.cniErr = nil
	a.Pass(ctx, false)
	g.Expect(taintKeys(t, kube, "regional-1")).To(BeEmpty())
	node, err = kube.GetNode(ctx, "regional-1")
	g.Expect(err).ToNot(HaveOccurred())
	node.Finalizers = append(node.Finalizers, "example.com/hold")
	g.Expect(kube.Client.Update(ctx, node)).To(Succeed())
	g.Expect(kube.Client.Delete(ctx, node)).To(Succeed())
	f.cniErr = errors.New("the CNI plugin host-local is not installed")
	a.Pass(ctx, false)
	g.Expect(a.Ready()).To(HaveOccurred())
	g.Expect(taintKeys(t, kube, "regional-1")).To(BeEmpty())
}

func TestAgentPutsTheTaintBackOnARegionalNodeThatFailsItsChecks(t *testing.T) {
	g := NewWithT(t)
	cl := twoNodes()
	// The NodeNetworkConfig of regional-1 is missing.
	cl.configs = cl.configs[:1]
	a, kube, f := newAgentTest(t, cl, "regional-1", interceptor.Funcs{})
	a.Pass(context.Background(), true)
	g.Expect(a.Ready()).To(MatchError(ContainSubstring("no NodeNetworkConfig")))
	g.Expect(f.takeCalls()).To(BeEmpty())
	g.Expect(taintKeys(t, kube, "regional-1")).To(Equal([]string{DefaultStartupTaintKey}))
}

func TestRegionalLaunch(t *testing.T) {
	g := NewWithT(t)
	c := testConfig(t)
	cl := twoNodes()
	home, regionalNode := &cl.nodes[0], &cl.nodes[1]
	g.Expect(c.regionalLaunch(regionalNode, cl.claims)).To(BeTrue())
	g.Expect(c.regionalLaunch(home, cl.claims)).To(BeFalse())
	g.Expect(c.regionalLaunch(home, []karpv1.NodeClaim{claimFor(home, testGroup, "")})).To(BeFalse())

	upper := cl.claims[0].DeepCopy()
	upper.Status.ProviderID = strings.ToUpper(upper.Status.ProviderID)
	g.Expect(c.regionalLaunch(regionalNode, []karpv1.NodeClaim{*upper})).To(BeTrue())

	for name, change := range map[string]func(claim *karpv1.NodeClaim, node *corev1.Node){
		"a NodeClaim of another group":     func(claim *karpv1.NodeClaim, _ *corev1.Node) { claim.Spec.NodeClassRef.Group = "karpenter.azure.com" },
		"a NodeClaim without a node class": func(claim *karpv1.NodeClaim, _ *corev1.Node) { claim.Spec.NodeClassRef = nil },
		"a NodeClaim without a placement":  func(claim *karpv1.NodeClaim, _ *corev1.Node) { delete(claim.Annotations, PlacementAnnotation) },
		"a NodeClaim of another VM":        func(claim *karpv1.NodeClaim, _ *corev1.Node) { claim.Status.ProviderID += "0" },
		"no provider ID on either": func(claim *karpv1.NodeClaim, node *corev1.Node) {
			claim.Status.ProviderID, node.Spec.ProviderID = "", ""
		},
	} {
		claim, node := cl.claims[0].DeepCopy(), regionalNode.DeepCopy()
		change(claim, node)
		g.Expect(c.regionalLaunch(node, []karpv1.NodeClaim{*claim})).To(BeFalse(), name)
	}
}

func TestDesiredTaints(t *testing.T) {
	g := NewWithT(t)
	a := &Agent{settings: &Settings{StartupTaintKey: DefaultStartupTaintKey}}
	other := corev1.Taint{Key: "example.com/other", Effect: corev1.TaintEffectNoExecute}
	noExecute := corev1.Taint{Key: DefaultStartupTaintKey, Effect: corev1.TaintEffectNoExecute}
	now := metav1.Now()
	for name, tc := range map[string]struct {
		taints   []corev1.Taint
		ready    bool
		regional bool
		deleting bool
		want     []corev1.Taint
		change   bool
	}{
		"a ready node loses the taint":                    {taints: []corev1.Taint{other, startupTaint}, ready: true, want: []corev1.Taint{other}, change: true},
		"whatever its effect":                             {taints: []corev1.Taint{noExecute, other}, ready: true, regional: true, want: []corev1.Taint{other}, change: true},
		"but not before Karpenter registers it":           {taints: []corev1.Taint{startupTaint, karpv1.UnregisteredNoExecuteTaint}, ready: true},
		"a ready node without the taint":                  {taints: []corev1.Taint{other}, ready: true, regional: true},
		"a regional node that is not ready gets it back":  {taints: []corev1.Taint{other}, regional: true, want: []corev1.Taint{other, startupTaint}, change: true},
		"a home node does not":                            {taints: []corev1.Taint{other}},
		"nor a regional node that is being deleted":       {regional: true, deleting: true},
		"a regional node that is not ready keeps a taint": {taints: []corev1.Taint{noExecute}, regional: true},
	} {
		node := &corev1.Node{Spec: corev1.NodeSpec{Taints: slices.Clone(tc.taints)}}
		if tc.deleting {
			node.DeletionTimestamp = &now
		}
		taints, change := a.desiredTaints(node, tc.ready, tc.regional)
		g.Expect(change).To(Equal(tc.change), name)
		g.Expect(taints).To(Equal(tc.want), name)
		g.Expect(node.Spec.Taints).To(Equal(tc.taints), name)
	}
}

func TestAgentRetriesTheTaintAfterAConflict(t *testing.T) {
	bump := func(ctx context.Context, c client.WithWatch, node *corev1.Node) error {
		node.Labels["example.com/changed"] = node.ResourceVersion
		return c.Update(ctx, node)
	}
	for name, tc := range map[string]struct {
		changes int
		change  func(context.Context, client.WithWatch, *corev1.Node) error
		patches int
		removed bool
	}{
		"an unrelated change":             {changes: 1, change: relabel("example.com/changed", "true"), patches: 2, removed: true},
		"a change before every patch":     {changes: 10, change: bump, patches: 3},
		"a change that fails the checks":  {changes: 1, change: relabel(HostRoutingLabel, "true"), patches: 1},
		"a change of the accepted values": {changes: 1, change: relabel(corev1.LabelTopologyRegion, "westus2"), patches: 1},
		"another Node of the same name":   {changes: 1, change: replaceNode, patches: 1},
	} {
		t.Run(name, func(t *testing.T) {
			g := NewWithT(t)
			patches := 0
			a, kube, _ := newAgentTest(t, twoNodes(), "home-1", changeBeforePatches(tc.changes, &patches, tc.change), startupTaint)
			a.Pass(context.Background(), true)
			g.Expect(a.Ready()).To(Succeed())
			g.Expect(patches).To(Equal(tc.patches))
			g.Expect(slices.Contains(taintKeys(t, kube, "home-1"), DefaultStartupTaintKey)).To(Equal(!tc.removed))
		})
	}
}

func TestAgentProgramsNothingOnANodeThatFailsItsOwnChecks(t *testing.T) {
	notFound := apierrors.NewNotFound(schema.GroupResource{Resource: "configmaps"}, "cilium-config")
	for name, tc := range map[string]struct {
		node  func(*corev1.Node)
		fake  func(*fakeNode)
		funcs interceptor.Funcs
		want  string
	}{
		"a Node that does not exist":     {node: func(n *corev1.Node) { n.Name = "home-2" }, want: "the Node home-1 does not exist"},
		"a node with eBPF host routing":  {node: func(n *corev1.Node) { n.Labels[HostRoutingLabel] = "true" }, want: "labeled " + HostRoutingLabel},
		"no cilium-config":               {funcs: failConfigMapGets(notFound), want: "cilium-config does not set enable-host-legacy-routing"},
		"an unreadable cilium-config":    {funcs: failConfigMapGets(apierrors.NewServiceUnavailable("unavailable")), want: "reading cilium-config"},
		"no instance metadata":           {fake: func(f *fakeNode) { f.metadataErr = errors.New("metadata unavailable") }, want: "metadata unavailable"},
		"metadata of another VM":         {fake: func(f *fakeNode) { f.metadata.VMID = "vm-other" }, want: "another VM ID"},
		"metadata of another private IP": {fake: func(f *fakeNode) { f.metadata.PrivateIP = netip.MustParseAddr("10.0.0.5") }, want: "private IP 10.0.0.5"},
	} {
		t.Run(name, func(t *testing.T) {
			g := NewWithT(t)
			cl := twoNodes()
			if tc.node != nil {
				tc.node(&cl.nodes[0])
			}
			a, _, f := newAgentTest(t, cl, "home-1", tc.funcs, startupTaint)
			if tc.fake != nil {
				tc.fake(f)
			}
			a.Pass(context.Background(), true)
			g.Expect(a.Ready()).To(MatchError(ContainSubstring(tc.want)))
			g.Expect(f.plans).To(BeEmpty())
		})
	}
}

func TestAgentReadsMetadataAndCiliumConfigAgainOnFullPasses(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	gets := 0
	var getErr error
	var data map[string]string
	funcs := interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
		config, isConfigMap := obj.(*corev1.ConfigMap)
		if isConfigMap {
			gets++
			if getErr != nil {
				return getErr
			}
		}
		if err := c.Get(ctx, key, obj, opts...); err != nil {
			return err
		}
		if isConfigMap && data != nil {
			config.Data = data
		}
		return nil
	}}
	a, _, f := newAgentTest(t, twoNodes(), "home-1", funcs)

	// Until a read of cilium-config succeeds, every pass reads it.
	getErr = apierrors.NewServiceUnavailable("unavailable")
	a.Pass(ctx, false)
	g.Expect(a.Ready()).To(MatchError(ContainSubstring("reading cilium-config")))
	getErr = nil
	a.Pass(ctx, false)
	g.Expect(a.Ready()).To(Succeed())
	a.Pass(ctx, false)
	g.Expect(gets).To(Equal(2))
	g.Expect(f.count("metadata")).To(Equal(1))

	// A full pass reads both again, and a failed read keeps what the last successful one found.
	getErr = apierrors.NewServiceUnavailable("unavailable")
	f.metadataErr = errors.New("metadata unavailable")
	a.Pass(ctx, true)
	g.Expect(a.Ready()).To(Succeed())
	g.Expect(gets).To(Equal(3))
	g.Expect(f.count("metadata")).To(Equal(2))

	// A change of cilium-config counts from the next full pass.
	getErr, f.metadataErr = nil, nil
	data = map[string]string{"enable-host-legacy-routing": "false", "cni-exclusive": "false"}
	a.Pass(ctx, false)
	g.Expect(a.Ready()).To(Succeed())
	a.Pass(ctx, true)
	g.Expect(a.Ready()).To(MatchError(ContainSubstring("enable-host-legacy-routing")))
	g.Expect(gets).To(Equal(4))
}

func TestAgentProgramsTheTunnelWhenItsPlanChanges(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	a, kube, f := newAgentTest(t, twoNodes(), "home-1", interceptor.Funcs{})
	a.Pass(ctx, true)
	a.Pass(ctx, false)
	g.Expect(f.count("tunnel")).To(Equal(1))
	// A full pass programs the tunnel again.
	a.Pass(ctx, true)
	g.Expect(f.count("tunnel")).To(Equal(2))

	// A new regional node is a new peer.
	more := &cluster{}
	more.add("regional-2", "10.2.0.4", "10.244.3.0/24", "centralus")
	for _, obj := range []client.Object{&more.nodes[0], &more.configs[0], &more.claims[0]} {
		g.Expect(kube.Client.Create(ctx, obj)).To(Succeed())
	}
	a.Pass(ctx, false)
	g.Expect(f.count("tunnel")).To(Equal(3))
	g.Expect(peerNames(f.plans[2])).To(Equal([]string{"regional-1", "regional-2"}))

	// After a failure, the next pass programs the tunnel again.
	f.tunnelErr = errors.New("device busy")
	a.Pass(ctx, true)
	f.tunnelErr = nil
	a.Pass(ctx, false)
	g.Expect(f.count("tunnel")).To(Equal(5))
	g.Expect(a.Ready()).To(Succeed())
}

func TestAgentLogsWhatChanges(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	cl := twoNodes()
	missing := cl.configs[1].DeepCopy()
	cl.configs = cl.configs[:1]
	a, kube, f := newAgentTest(t, cl, "home-1", interceptor.Funcs{}, startupTaint)
	log, lines := testLog()
	a.log = log

	a.Pass(ctx, true)
	a.Pass(ctx, true)
	g.Expect(countLines(lines(), `"msg":"rejected a node"`, `"node":"regional-1"`, `"reason":"no NodeNetworkConfig"`)).To(Equal(1))
	g.Expect(countLines(lines(), `"msg":"programmed the tunnel"`, `"peers":0`)).To(Equal(1))
	g.Expect(countLines(lines(), `"msg":"updated the startup taint"`, `"removed":true`)).To(Equal(1))
	g.Expect(countLines(lines(), `"msg":"the node is ready"`)).To(Equal(1))

	g.Expect(kube.Client.Create(ctx, missing)).To(Succeed())
	a.Pass(ctx, false)
	g.Expect(countLines(lines(), `"msg":"no longer rejects a node"`, `"node":"regional-1"`)).To(Equal(1))
	g.Expect(countLines(lines(), `"msg":"programmed the tunnel"`, `"peers":1`)).To(Equal(1))

	f.tunnelErr = errors.New("device busy")
	a.Pass(ctx, true)
	a.Pass(ctx, true)
	g.Expect(countLines(lines(), `"msg":"the node is not ready"`, `"reason":"programming the tunnel: device busy"`)).To(Equal(1))
}

func TestAgentLeavesTheTaintAloneWhenAPassEndsEarly(t *testing.T) {
	stopped, stop := context.WithCancel(context.Background())
	stop()
	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	for name, ctx := range map[string]context.Context{"by a stop": stopped, "by its timeout": expired} {
		t.Run(name, func(t *testing.T) {
			g := NewWithT(t)
			a, kube, _ := newAgentTest(t, twoNodes(), "home-1", interceptor.Funcs{}, startupTaint)
			a.Pass(ctx, true)
			g.Expect(a.Ready()).To(MatchError(ctx.Err()))
			g.Expect(taintKeys(t, kube, "home-1")).To(Equal([]string{DefaultStartupTaintKey}))
		})
	}
}

func TestAgentProbes(t *testing.T) {
	g := NewWithT(t)
	a, _, _ := newAgentTest(t, twoNodes(), "home-1", interceptor.Funcs{})
	probe := func(path string) (int, string) {
		recorder := httptest.NewRecorder()
		a.Probes().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		return recorder.Code, recorder.Body.String()
	}
	code, body := probe("/readyz")
	g.Expect(code).To(Equal(http.StatusServiceUnavailable))
	g.Expect(body).To(ContainSubstring("no pass has finished yet"))
	code, _ = probe("/healthz")
	g.Expect(code).To(Equal(http.StatusOK))

	a.Pass(context.Background(), true)
	code, body = probe("/readyz")
	g.Expect(code).To(Equal(http.StatusOK))
	g.Expect(body).To(Equal("ok\n"))
	g.Expect(a.stuck(time.Now().Add(time.Hour))).To(BeFalse())

	// A pass that has run for more than five minutes fails the liveness probe.
	a.setPassStart(time.Now().Add(-4 * time.Minute))
	code, _ = probe("/healthz")
	g.Expect(code).To(Equal(http.StatusOK))
	a.setPassStart(time.Now().Add(-6 * time.Minute))
	code, body = probe("/healthz")
	g.Expect(code).To(Equal(http.StatusServiceUnavailable))
	g.Expect(body).To(ContainSubstring("more than five minutes"))
	code, _ = probe("/metrics")
	g.Expect(code).To(Equal(http.StatusNotFound))
}

func TestAgentWatchesTheObjectsOfItsChecks(t *testing.T) {
	g := NewWithT(t)
	a, _, _ := newAgentTest(t, twoNodes(), "home-1", interceptor.Funcs{})
	informers := &fakeInformers{}
	g.Expect(a.Watch(context.Background(), informers)).To(Succeed())
	g.Expect(informers.objects).To(HaveLen(3))
	g.Expect(informers.objects[0]).To(BeAssignableToTypeOf(&corev1.Node{}))
	g.Expect(informers.objects[1]).To(BeAssignableToTypeOf(&karpv1.NodeClaim{}))
	g.Expect(informers.objects[2].GetObjectKind().GroupVersionKind()).To(Equal(networkConfigKind))
	g.Expect(informers.handlers).To(HaveLen(3))
	for _, handler := range informers.handlers {
		for _, event := range []func(){
			func() { handler.OnAdd(&corev1.Node{}, false) },
			func() { handler.OnUpdate(&corev1.Node{}, &corev1.Node{}) },
			func() { handler.OnDelete(&corev1.Node{}) },
		} {
			event()
			g.Expect(a.kick).To(Receive())
			g.Expect(a.kick).ToNot(Receive())
		}
	}

	g.Expect(a.Watch(context.Background(), &fakeInformers{err: errors.New("no such kind")})).To(MatchError(ContainSubstring("no such kind")))
}

func TestAgentRun(t *testing.T) {
	g := NewWithT(t)
	saved := []time.Duration{fullPassInterval, retryInterval, settleTime}
	t.Cleanup(func() { fullPassInterval, retryInterval, settleTime = saved[0], saved[1], saved[2] })
	var lists atomic.Int32
	a, _, f := newAgentTest(t, twoNodes(), "home-1", countPasses(&lists))

	// After its first pass, the agent runs a pass when something changes, and only then while the
	// node is ready.
	fullPassInterval, retryInterval, settleTime = time.Hour, time.Hour, time.Millisecond
	stop := runAgent(a)
	g.Eventually(lists.Load).Should(BeEquivalentTo(1))
	g.Eventually(a.Ready).Should(Succeed())
	a.Kick()
	g.Eventually(lists.Load).Should(BeEquivalentTo(2))
	g.Consistently(lists.Load, 100*time.Millisecond).Should(BeEquivalentTo(2))
	stop()

	// A full pass, which reads instance metadata again, runs every interval.
	fullPassInterval = 10 * time.Millisecond
	stop = runAgent(a)
	g.Eventually(func() int { return f.count("metadata") }).Should(BeNumerically(">=", 3))
	stop()

	// While the node is not ready, a pass runs every retry interval.
	fullPassInterval, retryInterval = time.Hour, 10*time.Millisecond
	f.tunnelErr = errors.New("device busy")
	passes := lists.Load()
	stop = runAgent(a)
	g.Eventually(lists.Load).Should(BeNumerically(">=", passes+3))
	g.Expect(a.Ready()).To(HaveOccurred())
	stop()
}

// TestAgentPutsTheTaintBackOnlyOnTheNodeItChecked changes the Node of a regional node that is not
// ready before the first patch of the taint. The agent retries while the Node has the UID that the
// pass checked, even when the Node now fails the checks: the node is not ready either way.
func TestAgentPutsTheTaintBackOnlyOnTheNodeItChecked(t *testing.T) {
	for name, tc := range map[string]struct {
		change  func(context.Context, client.WithWatch, *corev1.Node) error
		patches int
		tainted bool
	}{
		"an unrelated change":            {change: relabel("example.com/changed", "true"), patches: 2, tainted: true},
		"another Node of the same name":  {change: replaceNode, patches: 1},
		"a change that fails the checks": {change: relabel(HostRoutingLabel, "true"), patches: 2, tainted: true},
	} {
		t.Run(name, func(t *testing.T) {
			g := NewWithT(t)
			patches := 0
			a, kube, f := newAgentTest(t, twoNodes(), "regional-1", changeBeforePatches(1, &patches, tc.change))
			f.ciliumErr = errors.New("connection refused")
			a.Pass(context.Background(), true)
			g.Expect(a.Ready()).To(HaveOccurred())
			g.Expect(patches).To(Equal(tc.patches))
			g.Expect(slices.Contains(taintKeys(t, kube, "regional-1"), DefaultStartupTaintKey)).To(Equal(tc.tainted))
		})
	}
}

func TestAgentDoesNotRetryTheTaintAfterOtherErrors(t *testing.T) {
	g := NewWithT(t)
	patches := 0
	funcs := interceptor.Funcs{Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
		patches++
		return apierrors.NewServiceUnavailable("unavailable")
	}}
	a, kube, _ := newAgentTest(t, twoNodes(), "home-1", funcs, startupTaint)
	a.Pass(context.Background(), true)
	g.Expect(a.Ready()).To(Succeed())
	g.Expect(patches).To(Equal(1))
	g.Expect(taintKeys(t, kube, "home-1")).To(Equal([]string{DefaultStartupTaintKey}))
}

func TestAgentWithdrawsANodeWhoseValuesChange(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	a, kube, f := newAgentTest(t, twoNodes(), "home-1", interceptor.Funcs{})
	a.Pass(ctx, true)
	g.Expect(peerNames(f.plans[0])).To(Equal([]string{"regional-1"}))

	// The NodeNetworkConfig of regional-1 names another pod range, which the agent does not follow.
	config := NetworkConfigObject()
	g.Expect(kube.Client.Get(ctx, client.ObjectKey{Namespace: "kube-system", Name: "regional-1"}, config)).To(Succeed())
	setContainers(config, container("10.244.9.0/24", "10.1.0.4", "overlay"))
	g.Expect(kube.Client.Update(ctx, config)).To(Succeed())
	a.Pass(ctx, false)
	g.Expect(f.plans).To(HaveLen(2))
	g.Expect(f.plans[1].Peers).To(BeEmpty())
}

func TestAgentFinishesAPassWhenItStops(t *testing.T) {
	g := NewWithT(t)
	var passes atomic.Int32
	a, _, f := newAgentTest(t, twoNodes(), "home-1", countPasses(&passes))
	applying, release := make(chan struct{}), make(chan struct{})
	f.applying = func() {
		close(applying)
		<-release
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.Run(ctx)
	}()
	g.Eventually(applying).Should(BeClosed())
	cancel()
	g.Consistently(done, 100*time.Millisecond).ShouldNot(BeClosed())
	close(release)
	g.Eventually(done).Should(BeClosed())
	// The pass that was programming the tunnel finishes, and no other pass starts.
	g.Expect(passes.Load()).To(BeEquivalentTo(1))
	g.Expect(a.Ready()).To(MatchError(context.Canceled))
}
