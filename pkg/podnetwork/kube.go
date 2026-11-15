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
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/karpenter/pkg/apis"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

// Objects are the objects of the cluster that a pass of the agent checks.
type Objects struct {
	Nodes          []corev1.Node
	NetworkConfigs []unstructured.Unstructured
	NodeClaims     []karpv1.NodeClaim
}

// Cluster is what the agent reads from the API server and writes to it.
type Cluster interface {
	// List returns the Nodes, the NodeNetworkConfigs in kube-system and the NodeClaims.
	List(ctx context.Context) (Objects, error)
	// CiliumConfig returns the data of kube-system/cilium-config, which is empty when it does not exist.
	CiliumConfig(ctx context.Context) (map[string]string, error)
	// GetNode reads a Node from the API server.
	GetNode(ctx context.Context, name string) (*corev1.Node, error)
	// SetTaints replaces the taints of node, which must be unchanged since it was read.
	SetTaints(ctx context.Context, node *corev1.Node, taints []corev1.Taint) error
}

var (
	// networkConfigKind is the kind of the NodeNetworkConfigs.
	networkConfigKind = NetworkConfigs.GroupVersion().WithKind("NodeNetworkConfig")
	// nodeClaimVersion is the API version of Karpenter's NodeClaims.
	nodeClaimVersion = schema.GroupVersion{Group: apis.Group, Version: "v1"}
)

// NewScheme returns the types that the agent reads, apart from the NodeNetworkConfigs, which it reads
// as unstructured objects: the core types and Karpenter's NodeClaims.
func NewScheme() (*runtime.Scheme, error) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	scheme.AddKnownTypes(nodeClaimVersion, &karpv1.NodeClaim{}, &karpv1.NodeClaimList{})
	metav1.AddToGroupVersion(scheme, nodeClaimVersion)
	return scheme, nil
}

// NewRESTMapper returns the resources of the kinds that the agent reads and writes, so that it needs
// no discovery request before it watches them.
func NewRESTMapper() meta.RESTMapper {
	mapper := meta.NewDefaultRESTMapper(nil)
	mapper.Add(corev1.SchemeGroupVersion.WithKind("Node"), meta.RESTScopeRoot)
	mapper.Add(corev1.SchemeGroupVersion.WithKind("ConfigMap"), meta.RESTScopeNamespace)
	mapper.Add(nodeClaimVersion.WithKind("NodeClaim"), meta.RESTScopeRoot)
	mapper.Add(networkConfigKind, meta.RESTScopeNamespace)
	return mapper
}

// NetworkConfigObject returns an empty NodeNetworkConfig, which names its type to a cache.
func NetworkConfigObject() *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(networkConfigKind)
	return u
}

// WatchedObjects returns the objects whose changes start a pass of the agent.
func WatchedObjects() []client.Object {
	return []client.Object{&corev1.Node{}, &karpv1.NodeClaim{}, NetworkConfigObject()}
}

// CacheOptions returns the options of the agent's cache for scheme: every Node without its list of
// images, every NodeClaim, and the NodeNetworkConfigs in kube-system, all without managed fields.
func CacheOptions(scheme *runtime.Scheme) cache.Options {
	return cache.Options{
		Scheme:           scheme,
		Mapper:           NewRESTMapper(),
		DefaultTransform: cache.TransformStripManagedFields(),
		ByObject: map[client.Object]cache.ByObject{
			&corev1.Node{}:        {Transform: stripNode},
			NetworkConfigObject(): {Namespaces: map[string]cache.Config{NetworkConfigNamespace: {}}},
		},
	}
}

// stripNode drops the large fields of a Node that the agent does not read: its managed fields and its
// list of images.
func stripNode(obj any) (any, error) {
	if node, ok := obj.(*corev1.Node); ok {
		node.ManagedFields = nil
		node.Status.Images = nil
	}
	return obj, nil
}

// KubeCluster is the Cluster of a cache and a client of the API server.
type KubeCluster struct {
	// Cache holds the watched objects. Client reads cilium-config and patches Nodes.
	Cache  client.Reader
	Client client.Client
}

var _ Cluster = (*KubeCluster)(nil)

// List returns the Nodes, the NodeNetworkConfigs in kube-system and the NodeClaims from the cache.
func (k *KubeCluster) List(ctx context.Context) (Objects, error) {
	var nodes corev1.NodeList
	if err := k.Cache.List(ctx, &nodes); err != nil {
		return Objects{}, fmt.Errorf("listing Nodes: %w", err)
	}
	var configs unstructured.UnstructuredList
	configs.SetGroupVersionKind(NetworkConfigs.GroupVersion().WithKind("NodeNetworkConfigList"))
	if err := k.Cache.List(ctx, &configs, client.InNamespace(NetworkConfigNamespace)); err != nil {
		return Objects{}, fmt.Errorf("listing NodeNetworkConfigs: %w", err)
	}
	var claims karpv1.NodeClaimList
	if err := k.Cache.List(ctx, &claims); err != nil {
		return Objects{}, fmt.Errorf("listing NodeClaims: %w", err)
	}
	return Objects{Nodes: nodes.Items, NetworkConfigs: configs.Items, NodeClaims: claims.Items}, nil
}

// CiliumConfig reads kube-system/cilium-config from the API server.
func (k *KubeCluster) CiliumConfig(ctx context.Context) (map[string]string, error) {
	var config corev1.ConfigMap
	err := k.Client.Get(ctx, client.ObjectKey{Namespace: metav1.NamespaceSystem, Name: CiliumConfigName}, &config)
	switch {
	case apierrors.IsNotFound(err):
		return map[string]string{}, nil
	case err != nil:
		return nil, fmt.Errorf("reading %s: %w", CiliumConfigName, err)
	}
	return config.Data, nil
}

// GetNode reads a Node from the API server, not from the cache.
func (k *KubeCluster) GetNode(ctx context.Context, name string) (*corev1.Node, error) {
	node := &corev1.Node{}
	if err := k.Client.Get(ctx, client.ObjectKey{Name: name}, node); err != nil {
		return nil, fmt.Errorf("reading Node %s: %w", name, err)
	}
	return node, nil
}

// SetTaints patches the taints of node. The patch carries the resourceVersion of node, so the API
// server applies it only to that object, with its UID, and only while it is unchanged.
func (k *KubeCluster) SetTaints(ctx context.Context, node *corev1.Node, taints []corev1.Taint) error {
	patched := node.DeepCopy()
	patched.Spec.Taints = taints
	if err := k.Client.Patch(ctx, patched, client.MergeFromWithOptions(node, client.MergeFromWithOptimisticLock{})); err != nil {
		return fmt.Errorf("patching the taints of Node %s: %w", node.Name, err)
	}
	return nil
}
