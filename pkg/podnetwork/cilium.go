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
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

const (
	// CiliumConfigName is the ConfigMap in kube-system with the configuration of the Cilium dataplane.
	CiliumConfigName = "cilium-config"
	// HostRoutingLabel marks a node on which AKS enables eBPF host routing, which bypasses the kernel
	// routes of the agent.
	HostRoutingLabel = "kubernetes.azure.com/ebpf-host-routing"
	// ciliumHealthURL is the health endpoint of the node's Cilium agent, which its own probes call.
	ciliumHealthURL = "http://127.0.0.1:9879/healthz"
)

// ciliumSettings are the settings of cilium-config that the agent depends on. Both differ from
// Cilium's defaults, so cilium-config must set them: legacy host routing, because eBPF host routing
// bypasses the kernel routes of the agent, and a Cilium that is not exclusive, because an exclusive
// one renames the CNI configurations of other programs.
var ciliumSettings = []struct {
	key  string
	want bool
}{
	{key: "enable-host-legacy-routing", want: true},
	{key: "cni-exclusive", want: false},
}

// CheckCiliumConfig checks the data of cilium-config for the settings that the agent depends on.
func CheckCiliumConfig(data map[string]string) error {
	for _, s := range ciliumSettings {
		value, ok := data[s.key]
		if !ok {
			return fmt.Errorf("cilium-config does not set %s, which must be %t", s.key, s.want)
		}
		if got, err := strconv.ParseBool(value); err != nil || got != s.want {
			return fmt.Errorf("cilium-config sets %s to %q, but it must be %t", s.key, value, s.want)
		}
	}
	return nil
}

// CiliumHealth checks the Cilium agent of the node the way its own probes do.
type CiliumHealth struct {
	// URL is the health endpoint of the Cilium agent.
	URL  string
	HTTP *http.Client
}

// NewCiliumHealth returns the check of the node's Cilium agent. Its client uses no proxy and follows
// no redirect.
func NewCiliumHealth() *CiliumHealth {
	return &CiliumHealth{
		URL: ciliumHealthURL,
		HTTP: &http.Client{
			Timeout:   5 * time.Second,
			Transport: &http.Transport{Proxy: nil},
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// Check calls the health endpoint with the header brief: true and, like a kubelet probe, counts a
// status from 200 to 399 as healthy.
func (c *CiliumHealth) Check(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.URL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("brief", "true")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("calling the health endpoint of the Cilium agent: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusBadRequest {
		return fmt.Errorf("the health endpoint of the Cilium agent answered %s", resp.Status)
	}
	return nil
}
