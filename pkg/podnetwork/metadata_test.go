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
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"

	. "github.com/onsi/gomega"
)

const testMetadata = `{"compute": {"vmId": "0A1B2C3D-0000-0000-0000-000000000001", "location": "WestUS2",
"subscriptionId": "00000000-0000-0000-0000-000000000001", "resourceGroupName": "Cluster-Nodes", "name": "regional-1"},
"network": {"interface": [{"ipv4": {"ipAddress": [{"privateIpAddress": "10.1.0.4", "publicIpAddress": ""}],
"subnet": [{"address": "10.1.0.0", "prefix": "24"}]}}]}}`

func metadataClient(t *testing.T, status int, body string) *MetadataClient {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Metadata") != "true" || r.URL.Path != "/metadata/instance" || r.URL.Query().Get("api-version") == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return &MetadataClient{Endpoint: server.URL, HTTP: server.Client()}
}

func TestMetadataClientReadsTheLocalVM(t *testing.T) {
	g := NewWithT(t)
	md, err := metadataClient(t, http.StatusOK, testMetadata).Get(t.Context())
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(md).To(Equal(InstanceMetadata{
		VMID:              "0A1B2C3D-0000-0000-0000-000000000001",
		Location:          "westus2",
		SubscriptionID:    "00000000-0000-0000-0000-000000000001",
		ResourceGroupName: "Cluster-Nodes",
		PrivateIP:         netip.MustParseAddr("10.1.0.4"),
	}))
	for _, bad := range []struct {
		status int
		body   string
	}{{http.StatusInternalServerError, testMetadata}, {http.StatusOK, "{"}, {http.StatusOK, `{"compute": {"vmId": "x"}}`}} {
		_, err := metadataClient(t, bad.status, bad.body).Get(t.Context())
		g.Expect(err).To(HaveOccurred(), "status %d, body %s", bad.status, bad.body)
	}

	client := NewMetadataClient()
	g.Expect(client.Endpoint).To(Equal("http://169.254.169.254"))
	transport, ok := client.HTTP.Transport.(*http.Transport)
	g.Expect(ok).To(BeTrue())
	g.Expect(transport.Proxy).To(BeNil(), "instance metadata is never read through a proxy")
}

func TestCheckSelf(t *testing.T) {
	g := NewWithT(t)
	c := testConfig(t)
	own := Node{Name: "regional-1", Role: Regional, Region: "westus2", VMID: "0a1b2c3d-0000-0000-0000-000000000001", IP: netip.MustParseAddr("10.1.0.4")}
	md := InstanceMetadata{VMID: "0A1B2C3D-0000-0000-0000-000000000001", Location: "westus2", SubscriptionID: "00000000-0000-0000-0000-000000000001",
		ResourceGroupName: "Cluster-Nodes", PrivateIP: netip.MustParseAddr("10.1.0.4")}
	g.Expect(c.CheckSelf(own, md)).To(Succeed(), "the VM ID and the resource group compare ignoring case")
	for name, change := range map[string]func(*InstanceMetadata){
		"another VM ID":          func(m *InstanceMetadata) { m.VMID = "0a1b2c3d-0000-0000-0000-000000000002" },
		"the byte-swapped VM ID": func(m *InstanceMetadata) { m.VMID = "3D2C1B0A-0000-0000-0000-000000000001" },
		"another private IP":     func(m *InstanceMetadata) { m.PrivateIP = netip.MustParseAddr("10.1.0.5") },
		"another subscription":   func(m *InstanceMetadata) { m.SubscriptionID = "00000000-0000-0000-0000-000000000002" },
		"another resource group": func(m *InstanceMetadata) { m.ResourceGroupName = "other" },
		"another location":       func(m *InstanceMetadata) { m.Location = "eastus2" },
	} {
		changed := md
		change(&changed)
		g.Expect(c.CheckSelf(own, changed)).ToNot(Succeed(), name)
	}
}
