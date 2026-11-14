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

// Package apis contains Kubernetes API groups.
package apis

import (
	_ "embed"

	"github.com/awslabs/operatorpkg/object"
	"github.com/samber/lo"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	"github.com/Azure/karpenter-provider-azure/pkg/apis/coexistence"
)

//go:generate controller-gen crd object:headerFile="../../hack/boilerplate.go.txt" paths="./..." output:crd:artifacts:config=crds
//go:generate go run ../../hack/code/coexistence_crd_gen crds/karpenter.azure.com_aksnodeclasses.yaml crds/coexistence/multiregion.io_aksnodeclasses.yaml
var (
	// Group is the label, annotation, finalizer and tag domain in every mode. It is also the
	// AKSNodeClass API group unless coexistence mode is enabled.
	Group = "karpenter.azure.com"
	// APIGroup is the API group that serves AKSNodeClass in this process: Group by default, or
	// coexistence.Group when COEXISTENCE_MODE is enabled at process start.
	APIGroup = lo.Ternary(coexistence.Enabled(), coexistence.Group, Group)
	//CompatibilityGroup = "compatibility." + Group
	//go:embed crds/autoscaling.x-k8s.io_capacitybuffers.yaml
	CapacityBufferCRD []byte
	//go:embed crds/karpenter.azure.com_aksnodeclasses.yaml
	AKSNodeClassCRD []byte
	// CoexistenceAKSNodeClassCRD serves the v1beta1 schema of AKSNodeClassCRD under
	// coexistence.Group. go generate derives it, and a unit test keeps the two in sync.
	//go:embed crds/coexistence/multiregion.io_aksnodeclasses.yaml
	CoexistenceAKSNodeClassCRD []byte
	//go:embed crds/karpenter.sh_nodepools.yaml
	NodePoolCRD []byte
	//go:embed crds/karpenter.sh_nodeclaims.yaml
	NodeClaimCRD []byte
	//go:embed crds/karpenter.sh_nodeoverlays.yaml
	NodeOverlayCRD []byte
	CRDs           = []*apiextensionsv1.CustomResourceDefinition{
		object.Unmarshal[apiextensionsv1.CustomResourceDefinition](CapacityBufferCRD),
		object.Unmarshal[apiextensionsv1.CustomResourceDefinition](lo.Ternary(coexistence.Enabled(), CoexistenceAKSNodeClassCRD, AKSNodeClassCRD)),
		object.Unmarshal[apiextensionsv1.CustomResourceDefinition](NodePoolCRD),
		object.Unmarshal[apiextensionsv1.CustomResourceDefinition](NodeClaimCRD),
		object.Unmarshal[apiextensionsv1.CustomResourceDefinition](NodeOverlayCRD),
	}
)

// CoexistenceMode reports whether COEXISTENCE_MODE was enabled at process start.
func CoexistenceMode() bool {
	return coexistence.Enabled()
}
