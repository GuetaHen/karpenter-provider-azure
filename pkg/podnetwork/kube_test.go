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
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

// fakeKubeCluster returns a KubeCluster whose cache and client are one fake client that holds objs.
func fakeKubeCluster(t *testing.T, funcs interceptor.Funcs, objs ...client.Object) *KubeCluster {
	t.Helper()
	scheme, err := NewScheme()
	requireNoError(t, err)
	c := fake.NewClientBuilder().WithScheme(scheme).WithRESTMapper(NewRESTMapper()).WithObjects(objs...).WithInterceptorFuncs(funcs).Build()
	return &KubeCluster{Cache: c, Client: c}
}

func TestKubeClusterList(t *testing.T) {
	g := NewWithT(t)
	cl := &cluster{}
	cl.add("home-1", "10.0.0.4", "10.244.1.0/24", "")
	cl.add("regional-1", "10.1.0.4", "10.244.2.0/24", "westus2")
	objs := make([]client.Object, 0, len(cl.nodes)+len(cl.configs)+len(cl.claims)+1)
	for i := range cl.nodes {
		objs = append(objs, &cl.nodes[i])
	}
	for i := range cl.configs {
		objs = append(objs, &cl.configs[i])
	}
	for i := range cl.claims {
		objs = append(objs, &cl.claims[i])
	}
	elsewhere := cl.configs[0].DeepCopy()
	elsewhere.SetNamespace("default")
	objs = append(objs, elsewhere)

	got, err := fakeKubeCluster(t, interceptor.Funcs{}, objs...).List(context.Background())
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(got.Nodes).To(HaveLen(2))
	g.Expect(got.NodeClaims).To(HaveLen(1))
	g.Expect(got.NetworkConfigs).To(HaveLen(2))
	for _, config := range got.NetworkConfigs {
		g.Expect(config.GetNamespace()).To(Equal("kube-system"))
	}
	eval := testConfig(t).Evaluate(got.Nodes, got.NetworkConfigs, got.NodeClaims)
	g.Expect(eval.Rejected).To(BeEmpty())
	g.Expect(eval.Accepted).To(HaveLen(2))
}

func TestKubeClusterCiliumConfig(t *testing.T) {
	g := NewWithT(t)
	config := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "kube-system", Name: "cilium-config"},
		Data: map[string]string{"cni-exclusive": "false"}}
	elsewhere := config.DeepCopy()
	elsewhere.Namespace = "default"
	elsewhere.Data = map[string]string{"cni-exclusive": "true"}

	data, err := fakeKubeCluster(t, interceptor.Funcs{}, config, elsewhere).CiliumConfig(context.Background())
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(data).To(Equal(map[string]string{"cni-exclusive": "false"}))

	data, err = fakeKubeCluster(t, interceptor.Funcs{}, elsewhere).CiliumConfig(context.Background())
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(data).To(BeEmpty())

	failing := interceptor.Funcs{Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
		return apierrors.NewServiceUnavailable("unavailable")
	}}
	_, err = fakeKubeCluster(t, failing, config).CiliumConfig(context.Background())
	g.Expect(err).To(HaveOccurred())
}

func TestKubeClusterSetTaints(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
		Spec: corev1.NodeSpec{Taints: []corev1.Taint{{Key: "other", Effect: corev1.TaintEffectNoSchedule}}}}
	k := fakeKubeCluster(t, interceptor.Funcs{}, node)
	read := &corev1.Node{}
	g.Expect(k.Client.Get(ctx, client.ObjectKeyFromObject(node), read)).To(Succeed())

	taints := []corev1.Taint{{Key: "other", Effect: corev1.TaintEffectNoSchedule}, {Key: DefaultStartupTaintKey, Effect: corev1.TaintEffectNoSchedule}}
	g.Expect(k.SetTaints(ctx, read, taints)).To(Succeed())
	updated := &corev1.Node{}
	g.Expect(k.Client.Get(ctx, client.ObjectKeyFromObject(node), updated)).To(Succeed())
	g.Expect(updated.Spec.Taints).To(Equal(taints))

	// The Node changed after it was read, so a patch from what was read is refused.
	err := k.SetTaints(ctx, read, nil)
	g.Expect(apierrors.IsConflict(err)).To(BeTrue(), "%v", err)
	g.Expect(k.Client.Get(ctx, client.ObjectKeyFromObject(node), updated)).To(Succeed())
	g.Expect(updated.Spec.Taints).To(Equal(taints))

	fresh, err := k.GetNode(ctx, "node-1")
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(fresh.Spec.Taints).To(Equal(taints))
	_, err = k.GetNode(ctx, "node-2")
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "%v", err)
}

// TestKubeClusterReadsTheCacheOnlyToList gives the cache and the client different versions of a
// Node, a NodeNetworkConfig in kube-system and a NodeClaim, as when the cache lags behind the API
// server. List reads all three from the cache, and GetNode, which the agent uses after a conflict,
// and CiliumConfig read the API server.
func TestKubeClusterReadsTheCacheOnlyToList(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	versions := func(source string) []client.Object {
		labels := map[string]string{"example.com/source": source}
		networkConfig := NetworkConfigObject()
		networkConfig.SetNamespace(NetworkConfigNamespace)
		networkConfig.SetName("node-1")
		networkConfig.SetLabels(labels)
		return []client.Object{
			&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-1", Labels: labels}},
			networkConfig,
			&karpv1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Name: "nodeclaim-1", Labels: labels}},
		}
	}
	config := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "kube-system", Name: "cilium-config"},
		Data: map[string]string{"cni-exclusive": "false"}}
	k := &KubeCluster{
		Cache:  fakeKubeCluster(t, interceptor.Funcs{}, versions("cache")...).Cache,
		Client: fakeKubeCluster(t, interceptor.Funcs{}, append(versions("server"), config)...).Client,
	}

	objects, err := k.List(ctx)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(objects.Nodes).To(HaveLen(1))
	g.Expect(objects.Nodes[0].Labels).To(HaveKeyWithValue("example.com/source", "cache"))
	g.Expect(objects.NetworkConfigs).To(HaveLen(1))
	g.Expect(objects.NetworkConfigs[0].GetLabels()).To(HaveKeyWithValue("example.com/source", "cache"))
	g.Expect(objects.NodeClaims).To(HaveLen(1))
	g.Expect(objects.NodeClaims[0].Labels).To(HaveKeyWithValue("example.com/source", "cache"))
	fresh, err := k.GetNode(ctx, "node-1")
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(fresh.Labels).To(HaveKeyWithValue("example.com/source", "server"))
	data, err := k.CiliumConfig(ctx)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(data).To(Equal(map[string]string{"cni-exclusive": "false"}))
}

func TestCacheOptions(t *testing.T) {
	g := NewWithT(t)
	scheme, err := NewScheme()
	g.Expect(err).ToNot(HaveOccurred())
	opts := CacheOptions(scheme)
	g.Expect(opts.Scheme).To(BeIdenticalTo(scheme))
	g.Expect(opts.DefaultTransform).ToNot(BeNil())
	g.Expect(opts.Mapper).ToNot(BeNil())
	g.Expect(opts.ByObject).To(HaveLen(2))
	for obj, by := range opts.ByObject {
		switch obj.(type) {
		case *corev1.Node:
			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "node-1", ManagedFields: []metav1.ManagedFieldsEntry{{Manager: "kubelet"}}},
				Status: corev1.NodeStatus{Images: []corev1.ContainerImage{{Names: []string{"image"}}},
					NodeInfo: corev1.NodeSystemInfo{SystemUUID: "vm-1"}},
			}
			stripped, err := by.Transform(node)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(stripped).To(Equal(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
				Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{SystemUUID: "vm-1"}}}))
		case *unstructured.Unstructured:
			g.Expect(obj.GetObjectKind().GroupVersionKind()).To(Equal(networkConfigKind))
			g.Expect(by.Namespaces).To(HaveLen(1))
			g.Expect(by.Namespaces).To(HaveKey("kube-system"))
		default:
			t.Errorf("unexpected object %T", obj)
		}
	}
	g.Expect(WatchedObjects()).To(HaveLen(3))
}

func TestRESTMapper(t *testing.T) {
	g := NewWithT(t)
	mapper := NewRESTMapper()
	for kind, resource := range map[schema.GroupVersionKind]string{
		{Version: "v1", Kind: "Node"}:                                           "nodes",
		{Version: "v1", Kind: "ConfigMap"}:                                      "configmaps",
		{Group: "karpenter.sh", Version: "v1", Kind: "NodeClaim"}:               "nodeclaims",
		{Group: "acn.azure.com", Version: "v1alpha", Kind: "NodeNetworkConfig"}: "nodenetworkconfigs",
	} {
		mapping, err := mapper.RESTMapping(kind.GroupKind(), kind.Version)
		g.Expect(err).ToNot(HaveOccurred(), kind.Kind)
		g.Expect(mapping.Resource).To(Equal(kind.GroupVersion().WithResource(resource)), kind.Kind)
		namespaced := mapping.Scope.Name() == meta.RESTScopeNameNamespace
		g.Expect(namespaced).To(Equal(kind.Kind == "ConfigMap" || kind.Kind == "NodeNetworkConfig"), kind.Kind)
	}

	// A cache that holds the objects of only some namespaces, as the one of the NodeNetworkConfigs does,
	// looks up the scope of a list by the kind of the list, so the list of every kind must map with the
	// scope of its items.
	scheme, err := NewScheme()
	g.Expect(err).ToNot(HaveOccurred())
	configs := &unstructured.UnstructuredList{}
	configs.SetGroupVersionKind(NetworkConfigs.GroupVersion().WithKind("NodeNetworkConfigList"))
	for _, tc := range []struct {
		name       string
		list       client.ObjectList
		namespaced bool
	}{
		{"NodeList", &corev1.NodeList{}, false},
		{"ConfigMapList", &corev1.ConfigMapList{}, true},
		{"NodeClaimList", &karpv1.NodeClaimList{}, false},
		{"NodeNetworkConfigList", configs, true},
	} {
		namespaced, mapErr := apiutil.IsObjectNamespaced(tc.list, scheme, mapper)
		g.Expect(mapErr).ToNot(HaveOccurred(), tc.name)
		g.Expect(namespaced).To(Equal(tc.namespaced), tc.name)
	}
}
