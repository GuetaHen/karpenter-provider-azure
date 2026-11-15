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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"time"
)

// InstanceMetadata is what the agent reads about its own VM from the Azure Instance Metadata Service.
type InstanceMetadata struct {
	VMID              string
	Location          string
	SubscriptionID    string
	ResourceGroupName string
	// PrivateIP is the first private IPv4 address of the first network interface of the VM.
	PrivateIP netip.Addr
}

// MetadataClient reads the instance metadata of the VM that the agent runs on.
type MetadataClient struct {
	// Endpoint is the base URL of the Instance Metadata Service.
	Endpoint string
	HTTP     *http.Client
}

// NewMetadataClient returns a client of the Instance Metadata Service of the local VM. It uses no
// proxy, because the service answers only requests that come straight from the VM.
func NewMetadataClient() *MetadataClient {
	return &MetadataClient{
		Endpoint: "http://169.254.169.254",
		HTTP:     &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: nil}},
	}
}

// Get reads the instance metadata.
func (m *MetadataClient) Get(ctx context.Context) (InstanceMetadata, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.Endpoint+"/metadata/instance?api-version=2021-02-01", nil)
	if err != nil {
		return InstanceMetadata{}, err
	}
	req.Header.Set("Metadata", "true")
	resp, err := m.HTTP.Do(req)
	if err != nil {
		return InstanceMetadata{}, fmt.Errorf("reading instance metadata: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return InstanceMetadata{}, fmt.Errorf("reading instance metadata: %s", resp.Status)
	}
	var doc struct {
		Compute struct {
			VMID              string `json:"vmId"`
			Location          string `json:"location"`
			SubscriptionID    string `json:"subscriptionId"`
			ResourceGroupName string `json:"resourceGroupName"`
		} `json:"compute"`
		Network struct {
			Interface []struct {
				IPv4 struct {
					IPAddress []struct {
						PrivateIPAddress string `json:"privateIpAddress"`
					} `json:"ipAddress"`
				} `json:"ipv4"`
			} `json:"interface"`
		} `json:"network"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&doc); err != nil {
		return InstanceMetadata{}, fmt.Errorf("decoding instance metadata: %w", err)
	}
	md := InstanceMetadata{
		VMID:              doc.Compute.VMID,
		Location:          strings.ToLower(doc.Compute.Location),
		SubscriptionID:    doc.Compute.SubscriptionID,
		ResourceGroupName: doc.Compute.ResourceGroupName,
	}
	if len(doc.Network.Interface) == 0 || len(doc.Network.Interface[0].IPv4.IPAddress) == 0 {
		return md, errors.New("instance metadata lists no private IPv4 address")
	}
	if md.PrivateIP, err = netip.ParseAddr(doc.Network.Interface[0].IPv4.IPAddress[0].PrivateIPAddress); err != nil {
		return md, fmt.Errorf("decoding instance metadata: %w", err)
	}
	return md, nil
}

// CheckSelf verifies, before the agent programs anything for its own node, that instance metadata
// describes the VM of that node: the VM ID is the Node's systemUUID, ignoring case and with no
// byte-swapped form, the private IP is its InternalIP, and the VM is in the node resource group and
// in the node's region.
func (c *Config) CheckSelf(own Node, md InstanceMetadata) error {
	switch {
	case !strings.EqualFold(md.VMID, own.VMID):
		return errors.New("instance metadata reports another VM ID than the Node's systemUUID")
	case md.PrivateIP != own.IP:
		return fmt.Errorf("instance metadata reports private IP %s, not the InternalIP %s", md.PrivateIP, own.IP)
	case !strings.EqualFold(md.SubscriptionID, c.nodeResourceGroup.SubscriptionID) ||
		!strings.EqualFold(md.ResourceGroupName, c.nodeResourceGroup.ResourceGroupName):
		return errors.New("instance metadata reports a VM outside the node resource group")
	case !strings.EqualFold(md.Location, own.Region):
		return fmt.Errorf("instance metadata reports location %s, not the region %s of the node", md.Location, own.Region)
	}
	return nil
}
