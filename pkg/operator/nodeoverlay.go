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

package operator

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/karpenter/pkg/apis/v1alpha1"

	"github.com/Azure/karpenter-provider-azure/pkg/apis/coexistence"
)

// CoreControllerClient returns the client for the core controllers. In coexistence mode the other
// installation validates the same NodeOverlays and owns their status, so this controller applies
// NodeOverlays to its own NodePools as its own validation decides but never writes their status;
// it logs an overlay that fails that validation instead.
func CoreControllerClient(kubeClient client.Client) client.Client {
	if !coexistence.Enabled() {
		return kubeClient
	}
	return nodeOverlayStatusOwnedElsewhere{Client: kubeClient}
}

type nodeOverlayStatusOwnedElsewhere struct {
	client.Client
}

func (c nodeOverlayStatusOwnedElsewhere) Status() client.SubResourceWriter {
	return nodeOverlayStatusWriter{SubResourceWriter: c.Client.Status()}
}

type nodeOverlayStatusWriter struct {
	client.SubResourceWriter
}

func (w nodeOverlayStatusWriter) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	if overlay, ok := obj.(*v1alpha1.NodeOverlay); ok {
		logFailedValidation(ctx, overlay)
		return nil
	}
	return w.SubResourceWriter.Update(ctx, obj, opts...)
}

func (w nodeOverlayStatusWriter) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
	if overlay, ok := obj.(*v1alpha1.NodeOverlay); ok {
		logFailedValidation(ctx, overlay)
		return nil
	}
	return w.SubResourceWriter.Patch(ctx, obj, patch, opts...)
}

func logFailedValidation(ctx context.Context, overlay *v1alpha1.NodeOverlay) {
	for _, condition := range overlay.Status.Conditions {
		if condition.Type == v1alpha1.ConditionTypeValidationSucceeded && condition.Status == metav1.ConditionFalse {
			log.FromContext(ctx).Info("not applying a NodeOverlay that failed validation; the other installation owns its status",
				"NodeOverlay", overlay.Name, "reason", condition.Reason, "message", condition.Message)
		}
	}
}
