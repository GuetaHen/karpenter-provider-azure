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

package coexistence

import (
	"testing"

	. "github.com/onsi/gomega"
)

func TestParseAcceptsOnlyBooleans(t *testing.T) {
	g := NewWithT(t)
	for raw, expected := range map[string]bool{"": false, "  ": false, "true": true, "TRUE": true, "1": true, " true ": true, "false": false, "0": false} {
		value, err := parse(raw)
		g.Expect(err).ToNot(HaveOccurred(), raw)
		g.Expect(value).To(Equal(expected), raw)
	}
	for _, raw := range []string{"yes", "enabled", "on"} {
		value, err := parse(raw)
		g.Expect(err).To(MatchError(ContainSubstring(EnvVar)), raw)
		g.Expect(value).To(BeFalse(), raw)
	}
}

func TestDeriveCRDRequiresASingleServedVersion(t *testing.T) {
	g := NewWithT(t)
	_, err := DeriveCRD([]byte("metadata: {}\nspec:\n  names: {}\n  versions:\n  - name: v1alpha2\n"))
	g.Expect(err).To(MatchError(ContainSubstring(servedVersion)))
	_, err = DeriveCRD([]byte("kind: CustomResourceDefinition\n"))
	g.Expect(err).To(HaveOccurred())
}
