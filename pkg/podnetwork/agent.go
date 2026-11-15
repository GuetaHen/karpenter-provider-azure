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
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	toolscache "k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

// The timing of the passes, which tests shorten.
var (
	// fullPassInterval is how often a full pass runs.
	fullPassInterval = time.Minute
	// retryInterval is how soon another pass runs while the node is not ready.
	retryInterval = 5 * time.Second
	// settleTime lets the changes that arrive together start one pass.
	settleTime = time.Second
)

const (
	// passTimeout bounds the calls of one pass.
	passTimeout = 45 * time.Second
	// stuckPassTime is how long a pass may run before the liveness probe fails.
	stuckPassTime = 5 * time.Minute
	// taintAttempts bounds the patches of the startup taint in one pass.
	taintAttempts = 3
)

// errNoPass is why the node is not ready before the first pass of the agent has finished.
var errNoPass = errors.New("no pass has finished yet")

// Metadata reads the instance metadata of the VM that the agent runs on.
type Metadata interface {
	Get(ctx context.Context) (InstanceMetadata, error)
}

// Tunnel programs the tunnel of a plan on the node.
type Tunnel interface {
	Apply(plan Plan) error
}

// CNIWriter puts the CNI configuration of a regional node in place.
type CNIWriter interface {
	Ensure(podRange netip.Prefix) error
}

// HealthCheck checks the Cilium agent of the node.
type HealthCheck interface {
	Check(ctx context.Context) error
}

// InformerGetter returns the informer of a type of object, as a cache does.
type InformerGetter interface {
	GetInformer(ctx context.Context, obj client.Object, opts ...cache.InformerGetOption) (cache.Informer, error)
}

// Agent programs the pod network of the node that it runs on and handles the node's startup taint,
// as docs/multi-region.md describes. It runs one pass at a time.
type Agent struct {
	settings *Settings
	cluster  Cluster
	metadata Metadata
	tunnel   Tunnel
	cni      CNIWriter
	cilium   HealthCheck
	log      logr.Logger

	kick chan struct{}
	pins *Pins
	// lastMetadata and ciliumConfig hold what the last successful reads found.
	lastMetadata *InstanceMetadata
	ciliumConfig map[string]string
	ciliumRead   bool
	// applied is the plan that the tunnel holds, or nil after a failed Apply.
	applied  *Plan
	rejected map[string]string

	mu        sync.Mutex
	notReady  error
	passStart time.Time
}

// NewAgent returns the agent of the node that settings name.
func NewAgent(settings *Settings, cluster Cluster, metadata Metadata, tunnel Tunnel, cni CNIWriter, cilium HealthCheck, log logr.Logger) *Agent {
	return &Agent{settings: settings, cluster: cluster, metadata: metadata, tunnel: tunnel, cni: cni, cilium: cilium, log: log,
		kick: make(chan struct{}, 1), pins: NewPins(), notReady: errNoPass}
}

func (a *Agent) config() *Config {
	return &a.settings.Config
}

// Watch makes every change of a Node, a NodeClaim or a NodeNetworkConfig start a pass.
func (a *Agent) Watch(ctx context.Context, informers InformerGetter) error {
	handler := toolscache.ResourceEventHandlerFuncs{
		AddFunc:    func(any) { a.Kick() },
		UpdateFunc: func(any, any) { a.Kick() },
		DeleteFunc: func(any) { a.Kick() },
	}
	for _, obj := range WatchedObjects() {
		informer, err := informers.GetInformer(ctx, obj)
		if err == nil {
			_, err = informer.AddEventHandler(handler)
		}
		if err != nil {
			return fmt.Errorf("watching %T: %w", obj, err)
		}
	}
	return nil
}

// Kick starts a pass soon, unless one is pending already.
func (a *Agent) Kick() {
	select {
	case a.kick <- struct{}{}:
	default:
	}
}

// Run runs a full pass, and then a pass after the changes that kick it, a full pass every minute and,
// while the node is not ready, a pass every five seconds, until ctx ends.
func (a *Agent) Run(ctx context.Context) {
	a.log.Info("the agent starts", "node", a.settings.NodeName)
	ticker := time.NewTicker(fullPassInterval)
	defer ticker.Stop()
	full := true
	for ctx.Err() == nil {
		a.Pass(ctx, full)
		full = false
		var retry <-chan time.Time
		if a.Ready() != nil {
			retry = time.After(retryInterval)
		}
		select {
		case <-ctx.Done():
		case <-ticker.C:
			full = true
		case <-retry:
		case <-a.kick:
			a.settle(ctx)
		}
	}
}

// settle waits for the changes that arrive together with a kick, unless ctx ends.
func (a *Agent) settle(ctx context.Context) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(settleTime):
	}
	select {
	case <-a.kick:
	default:
	}
}

// Pass checks everything once, programs the node and handles its startup taint. A full pass also
// reads instance metadata and cilium-config again, and programs the tunnel even when its plan has not
// changed.
func (a *Agent) Pass(ctx context.Context, full bool) {
	a.setPassStart(time.Now())
	ctx, cancel := context.WithTimeout(ctx, passTimeout)
	defer cancel()
	a.finish(a.pass(ctx, full))
}

// pass returns nil when the conditions of the node's role hold, and otherwise why they do not.
func (a *Agent) pass(ctx context.Context, full bool) error {
	objects, err := a.cluster.List(ctx)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(objects.Nodes, func(n corev1.Node) bool { return n.Name == a.settings.NodeName })
	if i < 0 {
		return fmt.Errorf("the Node %s does not exist", a.settings.NodeName)
	}
	node := &objects.Nodes[i]
	own, notReady := a.program(ctx, full, node, objects)
	// A pass that ends early, by shutdown or by its timeout, leaves the taint alone.
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err := a.syncTaint(ctx, node, own, objects, notReady == nil); err != nil {
		a.log.Error(err, "updating the startup taint")
	}
	return notReady
}

// program programs the agent's own node and returns it, with nil once the conditions of its role
// hold: on every node, the checks pass and the tunnel is programmed; on a regional node, also the
// node's Cilium agent is healthy and the CNI configuration is in place.
func (a *Agent) program(ctx context.Context, full bool, node *corev1.Node, objects Objects) (Node, error) {
	own, peers, err := a.accept(ctx, full, node, objects)
	if err != nil {
		return own, err
	}
	if err := a.applyPlan(a.config().TunnelPlan(own, peers), full); err != nil {
		return own, err
	}
	if own.Role != Regional {
		return own, nil
	}
	// The CNI configuration waits for a healthy Cilium agent, so that the kubelet does not find a
	// network configuration before Cilium can serve it.
	if err := a.cilium.Check(ctx); err != nil {
		return own, fmt.Errorf("the Cilium agent of the node is not healthy: %w", err)
	}
	return own, a.cni.Ensure(own.PodRange)
}

// accept runs the checks of the agent's own node, and returns the node with its peers.
func (a *Agent) accept(ctx context.Context, full bool, node *corev1.Node, objects Objects) (Node, []Node, error) {
	if _, ok := node.Labels[HostRoutingLabel]; ok {
		return Node{}, nil, fmt.Errorf("the Node is labeled %s", HostRoutingLabel)
	}
	if err := a.checkCilium(ctx, full); err != nil {
		return Node{}, nil, err
	}
	eval := a.config().Evaluate(objects.Nodes, objects.NetworkConfigs, objects.NodeClaims)
	a.pins.Apply(&eval, objects.Nodes)
	a.logRejections(eval.Rejected)
	own, ok := eval.Accepted[node.Name]
	if !ok {
		return Node{}, nil, fmt.Errorf("the Node fails a check: %s", eval.Rejected[node.Name])
	}
	md, err := a.readMetadata(ctx, full)
	if err == nil {
		err = a.config().CheckSelf(own, md)
	}
	return own, eval.Peers(own), err
}

// checkCilium checks cilium-config, which it reads again on a full pass and until a read succeeds. A
// failed read keeps what the last successful one found.
func (a *Agent) checkCilium(ctx context.Context, full bool) error {
	if full || !a.ciliumRead {
		data, err := a.cluster.CiliumConfig(ctx)
		switch {
		case err == nil:
			a.ciliumConfig, a.ciliumRead = data, true
		case !a.ciliumRead:
			return err
		default:
			a.log.Error(err, "reading cilium-config again failed; keeping what the last read found")
		}
	}
	return CheckCiliumConfig(a.ciliumConfig)
}

// readMetadata returns the instance metadata of the VM, which it reads again on a full pass and until
// a read succeeds. A failed read keeps what the last successful one found.
func (a *Agent) readMetadata(ctx context.Context, full bool) (InstanceMetadata, error) {
	if full || a.lastMetadata == nil {
		md, err := a.metadata.Get(ctx)
		switch {
		case err == nil:
			a.lastMetadata = &md
		case a.lastMetadata == nil:
			return InstanceMetadata{}, err
		default:
			a.log.Error(err, "reading instance metadata again failed; keeping what the last read found")
		}
	}
	return *a.lastMetadata, nil
}

// applyPlan programs the tunnel of plan, unless a pass that is not full finds it programmed already.
func (a *Agent) applyPlan(plan Plan, full bool) error {
	unchanged := a.applied != nil && reflect.DeepEqual(*a.applied, plan)
	if unchanged && !full {
		return nil
	}
	a.applied = nil
	if err := a.tunnel.Apply(plan); err != nil {
		return fmt.Errorf("programming the tunnel: %w", err)
	}
	a.applied = &plan
	if !unchanged {
		a.log.Info("programmed the tunnel", "regional", plan.Regional, "peers", len(plan.Peers))
	}
	return nil
}

// logRejections logs a node that the agent rejects, or rejects for another reason than before, and a
// node that it no longer rejects.
func (a *Agent) logRejections(rejected map[string]string) {
	for name, reason := range rejected {
		if a.rejected[name] != reason {
			a.log.Info("rejected a node", "node", name, "reason", reason)
		}
	}
	for name := range a.rejected {
		if _, ok := rejected[name]; !ok {
			a.log.Info("no longer rejects a node", "node", name)
		}
	}
	a.rejected = rejected
}

// syncTaint removes the startup taint from the agent's own node once the node is ready and Karpenter
// has registered it, and puts the taint back on a regional node that is not ready, unless the Node is
// being deleted. A patch applies only to the Node as it was read. After a conflict, the agent reads
// the Node again and retries, a bounded number of times, while the Node has the UID that the pass
// checked and, when the pass found the node ready, still passes the checks with the values that the
// pass accepted. Otherwise the next pass decides; the change of the Node starts one.
func (a *Agent) syncTaint(ctx context.Context, node *corev1.Node, own Node, objects Objects, ready bool) error {
	regional := a.config().regionalLaunch(node, objects.NodeClaims)
	for attempt := 1; ; attempt++ {
		taints, change := a.desiredTaints(node, ready, regional)
		if !change {
			return nil
		}
		err := a.cluster.SetTaints(ctx, node, taints)
		if err == nil {
			a.log.Info("updated the startup taint", "removed", ready)
			return nil
		}
		if !apierrors.IsConflict(err) || attempt == taintAttempts {
			return err
		}
		fresh, err := a.cluster.GetNode(ctx, node.Name)
		if err != nil {
			return err
		}
		if fresh.UID != node.UID || (ready && !a.stillAccepted(fresh, own, objects)) {
			a.log.Info("the Node changed before the agent could update its startup taint")
			return nil
		}
		node = fresh
	}
}

// stillAccepted reports whether fresh, the agent's own Node as read again, passes the checks of the
// pass with the values that the pass accepted.
func (a *Agent) stillAccepted(fresh *corev1.Node, own Node, objects Objects) bool {
	if _, ok := fresh.Labels[HostRoutingLabel]; ok {
		return false
	}
	nodes := slices.Clone(objects.Nodes)
	for i := range nodes {
		if nodes[i].Name == fresh.Name {
			nodes[i] = *fresh
		}
	}
	n, ok := a.config().Evaluate(nodes, objects.NetworkConfigs, objects.NodeClaims).Accepted[fresh.Name]
	return ok && n == own
}

// desiredTaints returns the taints that node should carry, and whether they differ from its own.
func (a *Agent) desiredTaints(node *corev1.Node, ready, regional bool) ([]corev1.Taint, bool) {
	tainted := slices.ContainsFunc(node.Spec.Taints, a.isStartupTaint)
	switch {
	case ready && tainted && !unregistered(node):
		return slices.DeleteFunc(slices.Clone(node.Spec.Taints), a.isStartupTaint), true
	case !ready && !tainted && regional && node.DeletionTimestamp == nil:
		return append(slices.Clone(node.Spec.Taints), corev1.Taint{Key: a.settings.StartupTaintKey, Effect: corev1.TaintEffectNoSchedule}), true
	}
	return nil, false
}

func (a *Agent) isStartupTaint(t corev1.Taint) bool {
	return t.Key == a.settings.StartupTaintKey
}

// unregistered reports whether Karpenter has yet to register node. Its registration puts the startup
// taints of the NodeClaim on the Node again.
func unregistered(node *corev1.Node) bool {
	return slices.ContainsFunc(node.Spec.Taints, func(t corev1.Taint) bool { return t.Key == karpv1.UnregisteredTaintKey })
}

// regionalLaunch reports whether a NodeClaim of the configured node class group with the provider ID
// of node carries the placement annotation. The agent puts the startup taint back only on such a node.
func (c *Config) regionalLaunch(node *corev1.Node, claims []karpv1.NodeClaim) bool {
	id := strings.ToLower(node.Spec.ProviderID)
	return id != "" && slices.ContainsFunc(claims, func(claim karpv1.NodeClaim) bool {
		return strings.ToLower(claim.Status.ProviderID) == id && claim.Spec.NodeClassRef != nil &&
			claim.Spec.NodeClassRef.Group == c.NodeClassGroup && placed(&claim)
	})
}

func (a *Agent) setPassStart(t time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.passStart = t
}

// finish records the outcome of a pass, and logs it when it changes.
func (a *Agent) finish(notReady error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.passStart = time.Time{}
	switch {
	case notReady == nil && a.notReady != nil:
		a.log.Info("the node is ready")
	case notReady != nil && (a.notReady == nil || notReady.Error() != a.notReady.Error()):
		a.log.Info("the node is not ready", "reason", notReady.Error())
	}
	a.notReady = notReady
}

// Ready returns nil while the last pass found that the conditions of the node's role hold, and
// otherwise why they do not.
func (a *Agent) Ready() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.notReady
}

// stuck reports whether a pass has run for more than stuckPassTime at now.
func (a *Agent) stuck(now time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return !a.passStart.IsZero() && now.Sub(a.passStart) > stuckPassTime
}

// Probes returns the handler of the agent's probes. /readyz answers 200 while the last pass found that
// the conditions of the node's role hold. /healthz answers 200 unless a pass has run for more than
// five minutes.
func (a *Agent) Probes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if err := a.Ready(); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		if a.stuck(time.Now()) {
			http.Error(w, "a pass has run for more than five minutes", http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, "ok\n")
	})
	return mux
}
