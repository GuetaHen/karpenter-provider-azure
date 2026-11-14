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

package regional

import (
	"context"
	"errors"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	clocktesting "k8s.io/utils/clock/testing"

	"github.com/Azure/karpenter-provider-azure/pkg/apis/v1beta1"
)

func TestValidateSubnetsNamesARegionThatNeverLoaded(t *testing.T) {
	g := NewWithT(t)
	ctx := testContext(t)
	home, remote := testProviders("eastus2", .4), testProviders("westus2", .2)
	remoteTypes := remote.Types.(*testTypes)
	remoteTypes.updateErr = errors.New("SKU API unavailable")
	clk := clocktesting.NewFakeClock(time.Now())
	registry := New("eastus2", home, func(string) *Providers { return remote }, func(context.Context, v1beta1.Region) error { return nil }).WithClock(clk)

	err := registry.ValidateSubnets(ctx, testClass())
	g.Expect(err).To(MatchError(ContainSubstring("region westus2 has no SKU catalog yet: SKU API unavailable")))
	var reasoned interface{ Reason() string }
	g.Expect(errors.As(err, &reasoned)).To(BeTrue())
	g.Expect(reasoned.Reason()).To(Equal(RegionNotLoadedReason))

	remoteTypes.updateErr = nil
	clk.Step(retryBackoff)
	g.Expect(registry.ValidateSubnets(ctx, testClass())).To(Succeed(), "the NodeClass recovers once the region loads")
}
