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

package policysuite_test

import (
	"context"
	"slices"
	"time"

	"github.com/Azure/karpenter-provider-azure/pkg/podnetwork"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

// cachedNode is the Node of the agent whose cache and client the spec below runs.
const cachedNode = "cached"

// networkConfig returns the NodeNetworkConfig of cachedNode in the namespace.
func networkConfig(namespace string) *unstructured.Unstructured {
	config := podnetwork.NetworkConfigObject()
	config.SetNamespace(namespace)
	config.SetName(cachedNode)
	return config
}

// A fake client serves every list alike, but the agent's cache serves the NodeNetworkConfigs of
// kube-system only and looks up the scope of their list in the agent's RESTMapper. This spec runs the
// cache and client of the agent as the command builds them, against an API server, with the identity
// and permissions that the chart gives the agent.
var _ = Describe("The cache and client of the pod network agent", func() {
	It("list, read and patch what a pass of the agent needs", func() {
		ciliumConfig := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: metav1.NamespaceSystem, Name: podnetwork.CiliumConfigName},
			Data: map[string]string{"enable-host-legacy-routing": "true", "cni-exclusive": "false"}}
		for _, object := range []client.Object{
			&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: cachedNode}, Spec: corev1.NodeSpec{Taints: []corev1.Taint{taint(startupTaint)}}},
			&karpv1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Name: cachedNode}, Spec: karpv1.NodeClaimSpec{
				NodeClassRef: &karpv1.NodeClassReference{Group: "karpenter.azure.com", Kind: "AKSNodeClass", Name: "default"},
				Requirements: []karpv1.NodeSelectorRequirementWithMinValues{},
			}},
			ciliumConfig,
			networkConfig(podnetwork.NetworkConfigNamespace),
			// The agent does not read NodeNetworkConfigs outside kube-system.
			networkConfig(metav1.NamespaceDefault),
		} {
			Expect(env.Client.Create(ctx, object)).To(Succeed())
			DeferCleanup(func() { Expect(client.IgnoreNotFound(env.Client.Delete(ctx, object))).To(Succeed()) })
		}

		config := agentConfig(cachedNode)
		scheme, err := podnetwork.NewScheme()
		Expect(err).ToNot(HaveOccurred())
		objects, err := cache.New(config, podnetwork.CacheOptions(scheme))
		Expect(err).ToNot(HaveOccurred())
		cacheCtx, stop := context.WithCancel(ctx)
		DeferCleanup(stop)
		// As the agent does, get the informers of the watched objects before the cache starts.
		for _, object := range podnetwork.WatchedObjects() {
			_, informerErr := objects.GetInformer(cacheCtx, object)
			Expect(informerErr).ToNot(HaveOccurred())
		}
		stopped := make(chan struct{})
		go func() {
			defer GinkgoRecover()
			defer close(stopped)
			Expect(objects.Start(cacheCtx)).To(Succeed())
		}()
		DeferCleanup(func() {
			stop()
			<-stopped
		})
		syncCtx, cancelSync := context.WithTimeout(cacheCtx, 30*time.Second)
		defer cancelSync()
		Expect(objects.WaitForCacheSync(syncCtx)).To(BeTrue(), "the cache did not sync with the permissions of the chart")
		kube, err := client.New(config, client.Options{Scheme: scheme, Mapper: podnetwork.NewRESTMapper()})
		Expect(err).ToNot(HaveOccurred())
		cluster := &podnetwork.KubeCluster{Cache: objects, Client: kube}

		listed, err := cluster.List(ctx)
		Expect(err).ToNot(HaveOccurred())
		Expect(listed.NodeClaims).To(ConsistOf(HaveField("ObjectMeta.Name", cachedNode)))
		Expect(listed.NetworkConfigs).To(HaveLen(1))
		Expect(listed.NetworkConfigs[0].GetNamespace()).To(Equal(podnetwork.NetworkConfigNamespace))
		Expect(listed.NetworkConfigs[0].GetName()).To(Equal(cachedNode))
		Expect(listed.NetworkConfigs[0].GetManagedFields()).To(BeEmpty())
		i := slices.IndexFunc(listed.Nodes, func(node corev1.Node) bool { return node.Name == cachedNode })
		Expect(i).To(BeNumerically(">=", 0))
		node := &listed.Nodes[i]
		Expect(node.ManagedFields).To(BeEmpty())

		data, err := cluster.CiliumConfig(ctx)
		Expect(err).ToNot(HaveOccurred())
		Expect(data).To(Equal(ciliumConfig.Data))
		fresh, err := cluster.GetNode(ctx, cachedNode)
		Expect(err).ToNot(HaveOccurred())
		Expect(fresh.UID).To(Equal(node.UID))

		// As a pass does, the agent patches its Node as its cache holds it.
		Expect(cluster.SetTaints(ctx, node, withoutTaint(node.Spec.Taints, startupTaint))).To(Succeed())
		Expect(getNode(cachedNode).Spec.Taints).ToNot(ContainElement(HaveField("Key", startupTaint)))
	})
})
