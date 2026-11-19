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
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
)

const (
	// DefaultNodeClassGroup is the API group of the AKSNodeClasses of this provider.
	DefaultNodeClassGroup = "karpenter.azure.com"
	// DefaultStartupTaintKey is the default key of the startup taint that the agent handles.
	DefaultStartupTaintKey = "karpenter.azure.com/regional-pod-network-not-ready"
	// DefaultProbeAddress is the loopback address on which the agent serves its probes.
	DefaultProbeAddress = "127.0.0.1:9887"
)

// Settings configure the agent. The chart passes them as environment variables.
type Settings struct {
	Config Config
	// NodeName is the name of the Node that the agent runs on.
	NodeName string
	// StartupTaintKey is the key of the startup taint that the agent handles on its own Node.
	StartupTaintKey string
	// ProbeAddress is the host and port on which the agent serves /healthz and /readyz.
	ProbeAddress string
}

// SettingsFromEnv reads the settings from environment variables through lookup and validates them.
// NODE_NAME, POD_CIDR and NODE_RESOURCE_GROUP_ID are required. NODE_CLASS_GROUP, TRANSIT_PREFIX,
// STARTUP_TAINT_KEY and PROBE_ADDRESS have defaults, which an empty value also selects.
func SettingsFromEnv(lookup func(string) (string, bool)) (Settings, error) {
	env := func(name, fallback string) string {
		if value, _ := lookup(name); value != "" {
			return value
		}
		return fallback
	}
	s := Settings{
		Config: Config{
			NodeResourceGroupID: env("NODE_RESOURCE_GROUP_ID", ""),
			NodeClassGroup:      env("NODE_CLASS_GROUP", DefaultNodeClassGroup),
		},
		NodeName:        env("NODE_NAME", ""),
		StartupTaintKey: env("STARTUP_TAINT_KEY", DefaultStartupTaintKey),
		ProbeAddress:    env("PROBE_ADDRESS", DefaultProbeAddress),
	}
	var err error
	if s.Config.PodCIDR, err = netip.ParsePrefix(env("POD_CIDR", "")); err != nil {
		return s, fmt.Errorf("POD_CIDR must hold the pod CIDR of the cluster: %w", err)
	}
	if s.Config.TransitPrefix, err = netip.ParsePrefix(env("TRANSIT_PREFIX", DefaultTransitPrefix)); err != nil {
		return s, fmt.Errorf("TRANSIT_PREFIX must hold a prefix: %w", err)
	}
	if s.NodeName == "" {
		return s, errors.New("NODE_NAME must hold the name of the Node that the agent runs on")
	}
	if errs := validation.IsQualifiedName(s.StartupTaintKey); len(errs) > 0 {
		return s, fmt.Errorf("STARTUP_TAINT_KEY must hold a taint key: %s", strings.Join(errs, "; "))
	}
	if err := checkProbeAddress(s.ProbeAddress); err != nil {
		return s, err
	}
	if err := s.Config.Validate(); err != nil {
		return s, err
	}
	return s, nil
}

// checkProbeAddress requires a host and port whose port is a number from 1 to 65535, so that a
// wrong PROBE_ADDRESS stops the agent at its start and not only when it serves its probes.
func checkProbeAddress(address string) error {
	_, port, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("PROBE_ADDRESS must hold a host and port: %w", err)
	}
	n, err := strconv.ParseUint(port, 10, 16)
	if err != nil || n == 0 {
		return fmt.Errorf("PROBE_ADDRESS must hold a port from 1 to 65535, not %q", port)
	}
	return nil
}
