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
	"maps"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	. "github.com/onsi/gomega"
)

func TestCheckCiliumConfig(t *testing.T) {
	g := NewWithT(t)
	valid := map[string]string{"enable-host-legacy-routing": "true", "cni-exclusive": "false", "routing-mode": "native"}
	g.Expect(CheckCiliumConfig(valid)).To(Succeed())
	for name, change := range map[string]func(map[string]string){
		"no enable-host-legacy-routing": func(d map[string]string) { delete(d, "enable-host-legacy-routing") },
		"eBPF host routing":             func(d map[string]string) { d["enable-host-legacy-routing"] = "false" },
		"no cni-exclusive":              func(d map[string]string) { delete(d, "cni-exclusive") },
		"an exclusive Cilium":           func(d map[string]string) { d["cni-exclusive"] = "true" },
		"a value that is no boolean":    func(d map[string]string) { d["enable-host-legacy-routing"] = "yes" },
	} {
		data := maps.Clone(valid)
		change(data)
		g.Expect(CheckCiliumConfig(data)).To(HaveOccurred(), name)
	}
	g.Expect(CheckCiliumConfig(nil)).To(HaveOccurred())
}

func TestCiliumHealth(t *testing.T) {
	g := NewWithT(t)
	var status atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/healthz" || r.Header.Get("brief") != "true" {
			w.WriteHeader(http.StatusTeapot)
			return
		}
		w.Header().Set("Location", "/elsewhere")
		w.WriteHeader(status.Load().(int))
	}))
	defer server.Close()

	health := NewCiliumHealth()
	g.Expect(health.URL).To(Equal("http://127.0.0.1:9879/healthz"))
	g.Expect(health.HTTP.Timeout).To(Equal(5 * time.Second))
	health.URL = server.URL + "/healthz"
	for code, healthy := range map[int]bool{
		http.StatusOK:                 true,
		http.StatusFound:              true,
		399:                           true,
		http.StatusBadRequest:         false,
		http.StatusServiceUnavailable: false,
	} {
		status.Store(code)
		if healthy {
			g.Expect(health.Check(context.Background())).To(Succeed(), "status %d", code)
		} else {
			g.Expect(health.Check(context.Background())).To(HaveOccurred(), "status %d", code)
		}
	}

	server.Close()
	status.Store(http.StatusOK)
	g.Expect(health.Check(context.Background())).To(HaveOccurred())
}
