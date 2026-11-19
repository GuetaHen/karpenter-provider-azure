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
	"maps"
	"net/netip"
	"testing"

	. "github.com/onsi/gomega"
)

// lookupIn returns a lookup of environment variables in env.
func lookupIn(env map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		value, ok := env[name]
		return value, ok
	}
}

func TestSettingsFromEnv(t *testing.T) {
	g := NewWithT(t)
	required := map[string]string{"NODE_NAME": "node-1", "POD_CIDR": "10.244.0.0/16", "NODE_RESOURCE_GROUP_ID": testResourceGroupID}

	s, err := SettingsFromEnv(lookupIn(required))
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(s.NodeName).To(Equal("node-1"))
	g.Expect(s.StartupTaintKey).To(Equal("karpenter.azure.com/regional-pod-network-not-ready"))
	g.Expect(s.ProbeAddress).To(Equal("127.0.0.1:9887"))
	g.Expect(s.Config.PodCIDR).To(Equal(netip.MustParsePrefix("10.244.0.0/16")))
	g.Expect(s.Config.NodeResourceGroupID).To(Equal(testResourceGroupID))
	g.Expect(s.Config.NodeClassGroup).To(Equal("karpenter.azure.com"))
	g.Expect(s.Config.TransitPrefix).To(Equal(netip.MustParsePrefix("169.254.64.0/18")))
	// The VNI derives from the parsed node resource group, which only Validate sets.
	g.Expect(s.Config.VNI()).To(Equal(testConfig(t).VNI()))

	all := maps.Clone(required)
	maps.Copy(all, map[string]string{"NODE_CLASS_GROUP": "example.com", "TRANSIT_PREFIX": "169.254.128.0/20",
		"STARTUP_TAINT_KEY": "example.com/pod-network", "PROBE_ADDRESS": "127.0.0.1:9999"})
	s, err = SettingsFromEnv(lookupIn(all))
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(s.Config.NodeClassGroup).To(Equal("example.com"))
	g.Expect(s.Config.TransitPrefix).To(Equal(netip.MustParsePrefix("169.254.128.0/20")))
	g.Expect(s.StartupTaintKey).To(Equal("example.com/pod-network"))
	g.Expect(s.ProbeAddress).To(Equal("127.0.0.1:9999"))

	all["STARTUP_TAINT_KEY"] = ""
	s, err = SettingsFromEnv(lookupIn(all))
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(s.StartupTaintKey).To(Equal(DefaultStartupTaintKey))

	for name, change := range map[string][2]string{
		"no NODE_NAME":                              {"NODE_NAME", ""},
		"no POD_CIDR":                               {"POD_CIDR", ""},
		"a POD_CIDR without a length":               {"POD_CIDR", "10.244.0.0"},
		"no NODE_RESOURCE_GROUP_ID":                 {"NODE_RESOURCE_GROUP_ID", ""},
		"a NODE_CLASS_GROUP that is no API group":   {"NODE_CLASS_GROUP", "Not_A_Group"},
		"a TRANSIT_PREFIX without a length":         {"TRANSIT_PREFIX", "169.254.64.0"},
		"a TRANSIT_PREFIX outside link-local space": {"TRANSIT_PREFIX", "10.0.0.0/18"},
		"a STARTUP_TAINT_KEY that is no taint key":  {"STARTUP_TAINT_KEY", "not a key"},
		"a PROBE_ADDRESS without a port":            {"PROBE_ADDRESS", "127.0.0.1"},
		"a PROBE_ADDRESS whose port is no number":   {"PROBE_ADDRESS", "127.0.0.1:abc"},
		"a PROBE_ADDRESS whose port is too large":   {"PROBE_ADDRESS", "127.0.0.1:99999"},
		"a PROBE_ADDRESS with port 0":               {"PROBE_ADDRESS", "127.0.0.1:0"},
		"a PROBE_ADDRESS that is only a colon":      {"PROBE_ADDRESS", ":"},
	} {
		env := maps.Clone(required)
		env[change[0]] = change[1]
		_, err := SettingsFromEnv(lookupIn(env))
		g.Expect(err).To(HaveOccurred(), name)
	}
}
