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

package coexistence_test

import (
	"os"
	"os/exec"
	"testing"

	"github.com/awslabs/operatorpkg/object"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/yaml"

	"github.com/Azure/karpenter-provider-azure/pkg/apis"
	"github.com/Azure/karpenter-provider-azure/pkg/apis/coexistence"
	_ "github.com/Azure/karpenter-provider-azure/pkg/apis/v1alpha2"
	"github.com/Azure/karpenter-provider-azure/pkg/apis/v1beta1"
	"github.com/Azure/karpenter-provider-azure/pkg/providers/launchtemplate"
)

func TestCheckedInCRDIsDerivedFromTheDefaultCRD(t *testing.T) {
	g := NewWithT(t)
	derived, err := coexistence.DeriveCRD(apis.AKSNodeClassCRD)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(string(apis.CoexistenceAKSNodeClassCRD)).To(Equal(string(derived)), "run go generate ./pkg/apis after changing the AKSNodeClass API")
}

func TestCoexistenceCRDServesTheDefaultV1beta1Schema(t *testing.T) {
	g := NewWithT(t)
	var defaultCRD, crd apiextensionsv1.CustomResourceDefinition
	g.Expect(yaml.Unmarshal(apis.AKSNodeClassCRD, &defaultCRD)).To(Succeed())
	g.Expect(yaml.Unmarshal(apis.CoexistenceAKSNodeClassCRD, &crd)).To(Succeed())
	g.Expect(crd.Name).To(Equal(coexistence.CRDName))
	g.Expect(crd.Spec.Group).To(Equal(coexistence.Group))
	g.Expect(crd.Annotations).To(HaveKeyWithValue("helm.sh/resource-policy", "keep"), "uninstalling the CRD chart must not delete every coexistence AKSNodeClass")
	g.Expect(crd.Spec.Scope).To(Equal(defaultCRD.Spec.Scope))
	g.Expect(crd.Spec.Names.Kind).To(Equal(v1beta1.AKSNodeClassKind))
	g.Expect(crd.Spec.Names.ListKind).To(Equal(defaultCRD.Spec.Names.ListKind))
	g.Expect(crd.Spec.Names.Plural).To(Equal(defaultCRD.Spec.Names.Plural))
	g.Expect(crd.Spec.Names.Singular).To(Equal(defaultCRD.Spec.Names.Singular))
	g.Expect(crd.Spec.Names.ShortNames).To(BeEmpty(), "short names would be ambiguous beside the default CRD")
	g.Expect(crd.Spec.Names.Categories).To(ConsistOf("karpenter"))
	g.Expect(crd.Spec.Versions).To(HaveLen(1))
	g.Expect(crd.Spec.Versions[0].Name).To(Equal("v1beta1"))
	g.Expect(crd.Spec.Versions[0].Served && crd.Spec.Versions[0].Storage).To(BeTrue())
	g.Expect(crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"].Properties).To(HaveKey("regions"))

	// The whole version, including its schema and printer columns, is copied unchanged.
	var defaultMap, crdMap map[string]any
	g.Expect(yaml.Unmarshal(apis.AKSNodeClassCRD, &defaultMap)).To(Succeed())
	g.Expect(yaml.Unmarshal(apis.CoexistenceAKSNodeClassCRD, &crdMap)).To(Succeed())
	g.Expect(v1beta1Version(crdMap)).ToNot(BeNil())
	g.Expect(v1beta1Version(crdMap)).To(Equal(v1beta1Version(defaultMap)))
}

func v1beta1Version(crd map[string]any) map[string]any {
	spec, _ := crd["spec"].(map[string]any)
	versions, _ := spec["versions"].([]any)
	for _, v := range versions {
		if version, ok := v.(map[string]any); ok && version["name"] == "v1beta1" {
			return version
		}
	}
	return nil
}

// TestCoexistenceModeIsSelectedAtProcessStart runs this test binary again with the mode
// enabled, because the mode is read once during package initialization.
func TestCoexistenceModeIsSelectedAtProcessStart(t *testing.T) {
	g := NewWithT(t)
	if coexistence.Enabled() {
		expectCoexistenceMode(g)
		return
	}
	expectDefaultMode(g)
	cmd := exec.Command(os.Args[0], "-test.run=^TestCoexistenceModeIsSelectedAtProcessStart$", "-test.v") //nolint:gosec // re-runs this test binary
	cmd.Env = append(os.Environ(), coexistence.EnvVar+"=true")
	out, err := cmd.CombinedOutput()
	g.Expect(err).ToNot(HaveOccurred(), string(out))
	g.Expect(string(out)).To(ContainSubstring("--- PASS: TestCoexistenceModeIsSelectedAtProcessStart"))
}

func crdNames() []string {
	return lo.Map(apis.CRDs, func(crd *apiextensionsv1.CustomResourceDefinition, _ int) string { return crd.Name })
}

func expectDefaultMode(g Gomega) {
	g.Expect(apis.APIGroup).To(Equal(apis.Group))
	g.Expect(object.GVK(&v1beta1.AKSNodeClass{}).Group).To(Equal(apis.Group))
	g.Expect(crdNames()).To(ContainElement("aksnodeclasses." + apis.Group))
	g.Expect(launchtemplate.NodePoolTagKey).To(Equal(launchtemplate.DefaultNodePoolTagKey))
	g.Expect(launchtemplate.OwnerTagValue()).To(BeEmpty())
}

func expectCoexistenceMode(g Gomega) {
	g.Expect(apis.APIGroup).To(Equal(coexistence.Group))
	g.Expect(v1beta1.SchemeGroupVersion.Group).To(Equal(coexistence.Group))
	g.Expect(object.GVK(&v1beta1.AKSNodeClass{})).To(Equal(schema.GroupVersionKind{Group: coexistence.Group, Version: "v1beta1", Kind: v1beta1.AKSNodeClassKind}))
	g.Expect(scheme.Scheme.IsGroupRegistered(apis.Group)).To(BeFalse(), "no AKSNodeClass version is registered under the default group")
	g.Expect(crdNames()).To(ContainElement(coexistence.CRDName))
	g.Expect(crdNames()).ToNot(ContainElement("aksnodeclasses." + apis.Group))
	// Label, annotation, finalizer and tag domains do not follow the API group.
	g.Expect(v1beta1.TerminationFinalizer).To(Equal(apis.Group + "/termination"))
	g.Expect(v1beta1.AnnotationAKSNodeClassHash).To(Equal(apis.Group + "/aksnodeclass-hash"))
	g.Expect(launchtemplate.NodePoolTagKey).To(Equal(launchtemplate.CoexistenceNodePoolTagKey))
	g.Expect(launchtemplate.OwnerTagValue()).To(Equal(coexistence.Group))
}
