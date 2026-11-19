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
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Azure/karpenter-provider-azure/pkg/apis"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	coretest "sigs.k8s.io/karpenter/pkg/test"
	. "sigs.k8s.io/karpenter/pkg/utils/testing"
	"sigs.k8s.io/yaml"
)

const (
	// policyFile holds the spec of the policy, which the chart installs.
	policyFile   = "../../../charts/karpenter/files/pod-network-agent-policy.yaml"
	agentName    = "karpenter-pod-network-agent"
	agentUser    = "system:serviceaccount:karpenter:" + agentName
	startupTaint = "karpenter.azure.com/regional-pod-network-not-ready"
	otherTaint   = "example.com/other"
)

var (
	ctx context.Context
	env *coretest.Environment
)

// networkConfigCRD is a minimal CustomResourceDefinition of the NodeNetworkConfigs that Azure CNS
// publishes, with the group, version, names and scope that the agent reads, and no schema checks.
var networkConfigCRD = &apiextensionsv1.CustomResourceDefinition{
	ObjectMeta: metav1.ObjectMeta{Name: "nodenetworkconfigs.acn.azure.com"},
	Spec: apiextensionsv1.CustomResourceDefinitionSpec{
		Group: "acn.azure.com",
		Names: apiextensionsv1.CustomResourceDefinitionNames{Plural: "nodenetworkconfigs", Singular: "nodenetworkconfig",
			Kind: "NodeNetworkConfig", ListKind: "NodeNetworkConfigList"},
		Scope: apiextensionsv1.NamespaceScoped,
		Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{Name: "v1alpha", Served: true, Storage: true,
			Schema: &apiextensionsv1.CustomResourceValidation{OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
				Type: "object", XPreserveUnknownFields: lo.ToPtr(true)}}}},
	},
}

func TestPolicy(t *testing.T) {
	ctx = TestContextWithLogger(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "PodNetworkAgentPolicy")
}

var _ = BeforeSuite(func() {
	env = coretest.NewEnvironment(coretest.WithCRDs(append(slices.Clone(apis.CRDs), networkConfigCRD)...))
	raw, err := os.ReadFile(policyFile)
	Expect(err).ToNot(HaveOccurred())
	filled := strings.NewReplacer("__AGENT_USERNAME__", agentUser, "__STARTUP_TAINT_KEY__", startupTaint).Replace(string(raw))
	var spec admissionregistrationv1.ValidatingAdmissionPolicySpec
	Expect(yaml.UnmarshalStrict([]byte(filled), &spec)).To(Succeed())
	// The API server creates kube-system, which holds the Role of the agent, shortly after it starts.
	Eventually(func() error {
		return env.Client.Get(ctx, client.ObjectKey{Name: metav1.NamespaceSystem}, &corev1.Namespace{})
	}).WithTimeout(30 * time.Second).Should(Succeed())
	agent := []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: agentName, Namespace: "karpenter"}}
	for _, object := range []client.Object{
		&admissionregistrationv1.ValidatingAdmissionPolicy{ObjectMeta: metav1.ObjectMeta{Name: agentName}, Spec: spec},
		&admissionregistrationv1.ValidatingAdmissionPolicyBinding{ObjectMeta: metav1.ObjectMeta{Name: agentName},
			Spec: admissionregistrationv1.ValidatingAdmissionPolicyBindingSpec{PolicyName: agentName,
				ValidationActions: []admissionregistrationv1.ValidationAction{admissionregistrationv1.Deny}}},
		// The permissions of the agent, as the chart grants them: hack/validation/pod-network-agent-chart.sh
		// checks the same rules in the chart.
		&rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: agentName}, Rules: []rbacv1.PolicyRule{
			{APIGroups: []string{""}, Resources: []string{"nodes"}, Verbs: []string{"get", "list", "watch", "patch"}},
			{APIGroups: []string{"karpenter.sh"}, Resources: []string{"nodeclaims"}, Verbs: []string{"list", "watch"}},
		}},
		&rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: agentName},
			RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: agentName},
			Subjects: agent},
		&rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Namespace: metav1.NamespaceSystem, Name: agentName}, Rules: []rbacv1.PolicyRule{
			{APIGroups: []string{"acn.azure.com"}, Resources: []string{"nodenetworkconfigs"}, Verbs: []string{"list", "watch"}},
			{APIGroups: []string{""}, Resources: []string{"configmaps"}, ResourceNames: []string{"cilium-config"}, Verbs: []string{"get"}},
		}},
		&rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Namespace: metav1.NamespaceSystem, Name: agentName},
			RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: agentName},
			Subjects: agent},
	} {
		Expect(env.Client.Create(ctx, object)).To(Succeed())
	}
})

var _ = AfterSuite(func() {
	Expect(env.Stop()).To(Succeed(), "Failed to stop environment")
})

// agentConfig returns the configuration of a client that acts as the pod network agent with a token
// that names the given node, or no node when it is empty.
func agentConfig(node string) *rest.Config {
	config := rest.CopyConfig(env.Config)
	config.Impersonate = rest.ImpersonationConfig{UserName: agentUser}
	if node != "" {
		config.Impersonate.Extra = map[string][]string{"authentication.kubernetes.io/node-name": {node}}
	}
	return config
}

// agentClient acts as the pod network agent with a token that names the given node, or no node when it
// is empty.
func agentClient(node string) client.Client {
	c, err := client.New(agentConfig(node), client.Options{Scheme: env.Client.Scheme()})
	Expect(err).ToNot(HaveOccurred())
	return c
}

// getNode returns the Node name as the API server stores it.
func getNode(name string) *corev1.Node {
	node := &corev1.Node{}
	Expect(env.Client.Get(ctx, client.ObjectKey{Name: name}, node)).To(Succeed())
	return node
}

// patchNode applies change to the Node name with a merge patch through c, and returns the error of the
// API server.
func patchNode(c client.Client, name string, change func(*corev1.Node), opts ...client.PatchOption) error {
	node := getNode(name)
	patched := node.DeepCopy()
	change(patched)
	return c.Patch(ctx, patched, client.MergeFrom(node), opts...)
}

func taint(key string) corev1.Taint {
	return corev1.Taint{Key: key, Effect: corev1.TaintEffectNoSchedule}
}

// withoutTaint returns a copy of the taints without the one with the key.
func withoutTaint(taints []corev1.Taint, key string) []corev1.Taint {
	return slices.DeleteFunc(slices.Clone(taints), func(t corev1.Taint) bool { return t.Key == key })
}

// removeTaint returns a change that removes the taint with the key from a Node and keeps its other taints.
func removeTaint(key string) func(*corev1.Node) {
	return func(node *corev1.Node) { node.Spec.Taints = withoutTaint(node.Spec.Taints, key) }
}

// addTaint returns a change that appends a taint with the key to the taints of a Node.
func addTaint(key string) func(*corev1.Node) {
	return func(node *corev1.Node) { node.Spec.Taints = append(node.Spec.Taints, taint(key)) }
}

var _ = Describe("The admission policy of the pod network agent", func() {
	BeforeEach(func() {
		// Besides these taints, the API server gives a new Node the taint node.kubernetes.io/not-ready, which
		// the changes below keep as it is.
		for _, name := range []string{"own", "other"} {
			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"example.com/role": "home"}},
				Spec: corev1.NodeSpec{Taints: []corev1.Taint{taint(startupTaint), taint(otherTaint)}}}
			Expect(env.Client.Create(ctx, node)).To(Succeed())
		}
		// The API server applies a new policy after a moment; until then the dry run of a refused change passes.
		Eventually(func() error {
			return patchNode(agentClient("own"), "other", removeTaint(startupTaint), client.DryRunAll)
		}).WithTimeout(30 * time.Second).Should(MatchError(ContainSubstring("may change only the Node that it runs on")))
	})

	AfterEach(func() {
		for _, name := range []string{"own", "other"} {
			Expect(env.Client.Delete(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}})).To(Succeed())
		}
	})

	It("lets the agent remove and put back the startup taint of its own Node", func() {
		others := withoutTaint(getNode("own").Spec.Taints, startupTaint)
		agent := agentClient("own")
		Expect(patchNode(agent, "own", removeTaint(startupTaint))).To(Succeed())
		Expect(getNode("own").Spec.Taints).To(Equal(others))
		Expect(patchNode(agent, "own", addTaint(startupTaint))).To(Succeed())
		Expect(getNode("own").Spec.Taints).To(Equal(append(others, taint(startupTaint))))
	})

	It("refuses the agent any change to another Node", func() {
		Expect(patchNode(agentClient("own"), "other", removeTaint(startupTaint))).
			To(MatchError(ContainSubstring("may change only the Node that it runs on")))
	})

	It("refuses an agent whose token names no node", func() {
		Expect(patchNode(agentClient(""), "own", removeTaint(startupTaint))).
			To(MatchError(ContainSubstring("may change only the Node that it runs on")))
	})

	DescribeTable("refuses the agent any other change to its own Node",
		func(change func(*corev1.Node)) {
			Expect(patchNode(agentClient("own"), "own", change)).
				To(MatchError(ContainSubstring("may change only its startup taint on the Node")))
		},
		Entry("removing another taint", removeTaint(otherTaint)),
		Entry("adding another taint", addTaint("example.com/new")),
		Entry("a label", func(node *corev1.Node) { node.Labels["example.com/role"] = "regional" }),
		Entry("an annotation", func(node *corev1.Node) { node.Annotations = map[string]string{"example.com/note": "x"} }),
		Entry("a finalizer", func(node *corev1.Node) { node.Finalizers = []string{"example.com/hold"} }),
		Entry("an owner reference", func(node *corev1.Node) {
			node.OwnerReferences = []metav1.OwnerReference{{
				APIVersion: "v1", Kind: "Namespace", Name: "default", UID: "00000000-0000-0000-0000-000000000001",
			}}
		}),
		Entry("unschedulable", func(node *corev1.Node) { node.Spec.Unschedulable = true }),
		Entry("the provider ID", func(node *corev1.Node) { node.Spec.ProviderID = "azure:///example" }),
		Entry("the pod CIDR", func(node *corev1.Node) {
			node.Spec.PodCIDR, node.Spec.PodCIDRs = "10.244.1.0/24", []string{"10.244.1.0/24"}
		}),
	)

	It("leaves the changes of other users alone", func() {
		Expect(patchNode(env.Client, "other", func(node *corev1.Node) {
			node.Spec.Taints, node.Labels["example.com/role"] = nil, "regional"
		})).To(Succeed())
	})
})
