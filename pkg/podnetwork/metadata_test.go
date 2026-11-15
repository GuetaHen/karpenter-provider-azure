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
	"strings"
	"sync/atomic"
	"testing"
	"time"

	. "github.com/onsi/gomega"
)

const testMetadata = `{"compute": {"vmId": "0A1B2C3D-0000-0000-0000-000000000001", "location": "WestUS2",
"subscriptionId": "00000000-0000-0000-0000-000000000001", "resourceGroupName": "Cluster-Nodes", "name": "regional-1"},
"network": {"interface": [{"ipv4": {"ipAddress": [{"privateIpAddress": "10.1.0.4", "publicIpAddress": ""}],
"subnet": [{"address": "10.1.0.0", "prefix": "24"}]}}]}}`

func metadataClient(t *testing.T, status int, body string) *MetadataClient {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Metadata") != "true" || r.URL.Path != "/metadata/instance" || r.URL.RawQuery != "api-version=2021-02-01" {
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
	// A valid document just under the limit of 1 MiB is read in full.
	underLimit := strings.Replace(testMetadata, `"name": "regional-1"`,
		`"name": "`+strings.Repeat("x", (1<<20)-1-len(testMetadata)+len("regional-1"))+`"`, 1)
	g.Expect(underLimit).To(HaveLen((1 << 20) - 1))
	md, err = metadataClient(t, http.StatusOK, underLimit).Get(t.Context())
	g.Expect(err).ToNot(HaveOccurred(), "a document just under the limit")
	g.Expect(md.PrivateIP).To(Equal(netip.MustParseAddr("10.1.0.4")))
	oversized := strings.Replace(testMetadata, `"name": "regional-1"`, `"name": "`+strings.Repeat("x", 1<<20)+`"`, 1)
	for name, bad := range map[string]struct {
		status int
		body   string
	}{
		"a server error":                 {http.StatusInternalServerError, testMetadata},
		"broken JSON":                    {http.StatusOK, "{"},
		"no network interface":           {http.StatusOK, `{"compute": {"vmId": "x"}}`},
		"a body over the limit of 1 MiB": {http.StatusOK, oversized},
	} {
		_, err := metadataClient(t, bad.status, bad.body).Get(t.Context())
		g.Expect(err).To(HaveOccurred(), name)
	}

	client := NewMetadataClient()
	g.Expect(client.Endpoint).To(Equal("http://169.254.169.254"))
	transport, ok := client.HTTP.Transport.(*http.Transport)
	g.Expect(ok).To(BeTrue())
	g.Expect(transport.Proxy).To(BeNil(), "instance metadata is never read through a proxy")
	g.Expect(client.HTTP.Timeout).To(Equal(10*time.Second), "a request that hangs does not stall the agent")
}

func TestMetadataClientFollowsNoRedirect(t *testing.T) {
	g := NewWithT(t)
	var followed atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/elsewhere" {
			followed.Store(true)
			_, _ = w.Write([]byte(testMetadata))
			return
		}
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	}))
	t.Cleanup(server.Close)
	client := NewMetadataClient()
	client.Endpoint = server.URL
	_, err := client.Get(t.Context())
	g.Expect(err).To(MatchError(ContainSubstring("302")))
	g.Expect(followed.Load()).To(BeFalse())
}

func TestCheckSelf(t *testing.T) {
	g := NewWithT(t)
	c := testConfig(t)
	own := Node{Name: "regional-1", Role: Regional, Region: "westus2", VMID: "0a1b2c3d-4e5f-6a7b-8c9d-000000000001", IP: netip.MustParseAddr("10.1.0.4")}
	md := InstanceMetadata{VMID: "0A1B2C3D-4E5F-6A7B-8C9D-000000000001", Location: "westus2", SubscriptionID: "00000000-0000-0000-0000-000000000001",
		ResourceGroupName: "Cluster-Nodes", PrivateIP: netip.MustParseAddr("10.1.0.4")}
	g.Expect(c.CheckSelf(own, md)).To(Succeed(), "the VM ID and the resource group compare ignoring case")
	generation1 := md
	generation1.VMID = "3D2C1B0A-5F4E-7B6A-8C9D-000000000001"
	g.Expect(c.CheckSelf(own, generation1)).To(Succeed(), "a Generation 1 VM reports the first three groups byte-swapped")
	for name, change := range map[string]func(*InstanceMetadata){
		"another VM ID":                     func(m *InstanceMetadata) { m.VMID = "0a1b2c3d-4e5f-6a7b-8c9d-000000000002" },
		"only the first group byte-swapped": func(m *InstanceMetadata) { m.VMID = "3d2c1b0a-4e5f-6a7b-8c9d-000000000001" },
		"the fourth group byte-swapped too": func(m *InstanceMetadata) { m.VMID = "3d2c1b0a-5f4e-7b6a-9d8c-000000000001" },
		"another private IP":                func(m *InstanceMetadata) { m.PrivateIP = netip.MustParseAddr("10.1.0.5") },
		"another subscription":              func(m *InstanceMetadata) { m.SubscriptionID = "00000000-0000-0000-0000-000000000002" },
		"another resource group":            func(m *InstanceMetadata) { m.ResourceGroupName = "other" },
		"another location":                  func(m *InstanceMetadata) { m.Location = "eastus2" },
	} {
		changed := md
		change(&changed)
		g.Expect(c.CheckSelf(own, changed)).ToNot(Succeed(), name)
	}
}
