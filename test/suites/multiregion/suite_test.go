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

package multiregion_test

import (
	"encoding/json"
	"os"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/Azure/karpenter-provider-azure/pkg/apis/v1beta1"
	"github.com/Azure/karpenter-provider-azure/pkg/test"
	"github.com/Azure/karpenter-provider-azure/test/pkg/environment/azure"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

var env *azure.Environment
var nodeClass *v1beta1.AKSNodeClass
var nodePool *karpv1.NodePool

// regions are the regions of the AKSNodeClass under test, from MULTI_REGION_TEST_REGIONS.
var regions []v1beta1.Region

func TestMultiRegion(t *testing.T) {
	RegisterFailHandler(Fail)
	BeforeSuite(func() {
		raw := os.Getenv("MULTI_REGION_TEST_REGIONS")
		if raw == "" {
			Skip("MULTI_REGION_TEST_REGIONS is not set: it lists, as JSON, two or more regions with their node subnets, for a cluster whose controller runs with ENABLE_MULTI_REGION=true (see docs/multi-region.md)")
		}
		Expect(json.Unmarshal([]byte(raw), &regions)).To(Succeed(), "parsing MULTI_REGION_TEST_REGIONS")
		Expect(len(regions)).To(BeNumerically(">=", 2), "MULTI_REGION_TEST_REGIONS must list at least two regions")
		env = azure.NewEnvironment(t)
	})
	AfterSuite(func() {
		if env != nil {
			env.Stop()
		}
	})
	RunSpecs(t, "MultiRegion")
}

var _ = BeforeEach(func() {
	env.BeforeEach()
	nodeClass = test.AKSNodeClass()
	nodePool = env.DefaultNodePool(nodeClass)
})
var _ = AfterEach(func() { env.Cleanup() })
var _ = AfterEach(func() { env.AfterEach() })
