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

// Package podnetwork holds the pod network agent, which connects the pods of regional nodes with
// the pods of the other nodes of a cluster on Azure CNI Overlay, as docs/multi-region.md describes.
package podnetwork

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"k8s.io/apimachinery/pkg/util/validation"
)

// Config holds the configuration of the agent. Validate must succeed before any other method is used.
type Config struct {
	// PodCIDR is the pod CIDR of the cluster. The pod range of every node is a /24 inside it.
	PodCIDR netip.Prefix
	// NodeResourceGroupID is the ARM ID of the resource group that holds the VMs of all nodes.
	NodeResourceGroupID string
	// NodeClassGroup is the API group of the node classes of the controller that creates regional
	// nodes, karpenter.azure.com unless set.
	NodeClassGroup string
	// TransitPrefix holds the transit addresses of the nodes, one for every /24 of the pod CIDR.
	TransitPrefix netip.Prefix

	nodeResourceGroup *arm.ResourceID
}

// Validate checks the configuration. The agent does not start unless it succeeds.
func (c *Config) Validate() error {
	if err := c.validatePodCIDR(); err != nil {
		return err
	}
	if err := c.validateTransit(); err != nil {
		return err
	}
	id, err := arm.ParseResourceID(c.NodeResourceGroupID)
	if err != nil || !strings.EqualFold(id.ResourceType.String(), "Microsoft.Resources/resourceGroups") || id.SubscriptionID == "" {
		return errors.New("the node resource group ID must be the ARM ID of a resource group in a subscription")
	}
	if errs := validation.IsDNS1123Subdomain(c.NodeClassGroup); len(errs) > 0 {
		return fmt.Errorf("the node class group %q must be an API group: %s", c.NodeClassGroup, strings.Join(errs, "; "))
	}
	c.nodeResourceGroup = id
	return nil
}

// validatePodCIDR checks the pod CIDR. It must not overlap IPv4 link-local space, so that no pod route
// covers the instance metadata address or a transit address.
func (c *Config) validatePodCIDR() error {
	p := c.PodCIDR
	if !p.IsValid() || !p.Addr().Is4() || p.Bits() > 24 || p.Masked() != p {
		return fmt.Errorf("the pod CIDR %s must be an IPv4 prefix of length 24 or less without host bits", p)
	}
	if p.Overlaps(linkLocal) {
		return fmt.Errorf("the pod CIDR %s must not overlap %s", p, linkLocal)
	}
	return nil
}
