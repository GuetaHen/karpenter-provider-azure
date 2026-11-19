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
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

const (
	// CNIConfigName is the CNI configuration that the agent writes on a regional node. It sorts before
	// the configurations that AKS writes, and the container runtime uses the first one.
	CNIConfigName = "00-karpenter-pod-network.conflist"
	// cniTempName is the temporary file of a new CNI configuration, whose extension the container
	// runtime ignores.
	cniTempName = ".karpenter-pod-network.tmp"
)

var (
	// cniPlugins are the plugins that the CNI configuration of the agent names.
	cniPlugins = []string{"cilium-cni", "host-local"}
	// cniConfigExtensions are the extensions of the files that the container runtime reads as CNI
	// configurations.
	cniConfigExtensions = []string{".conf", ".conflist", ".json"}
)

// cniConfigTemplate is the CNI configuration of a regional node: the cilium-cni plugin with host-local
// IPAM over the whole pod range of the node, of which host-local keeps the first address as the
// gateway. Pods get the MTU that cilium-cni sets, as on home nodes. A lower MTU on the interface of a
// pod would not lower what the pod sends, because cilium-cni gives the routes of the pod the MTU of
// Cilium, which sizes its packets and its TCP MSS; it would only make the veth drop longer packets on
// their way into the pod, without an ICMP error. See TunnelMTU for traffic into the tunnel.
const cniConfigTemplate = `{
  "cniVersion": "0.3.1",
  "name": "karpenter-pod-network",
  "plugins": [
    {
      "type": "cilium-cni",
      "ipam": {
        "type": "host-local",
        "ranges": [[{"subnet": %q}]]
      }
    }
  ]
}
`

// CNIConfig returns the CNI configuration of a regional node whose pod range is podRange.
func CNIConfig(podRange netip.Prefix) []byte {
	return fmt.Appendf(nil, cniConfigTemplate, podRange.String())
}

// previousCNIConfigTemplate is the CNI configuration that the previous version of the agent wrote on
// a regional node, which also chained the tuning plugin for the MTU of the tunnel. The agent replaces
// a file that holds it for the pod range of the node and refuses a file with any other content, so a
// change of cniConfigTemplate must move the current template here.
const previousCNIConfigTemplate = `{
  "cniVersion": "0.3.1",
  "name": "karpenter-pod-network",
  "plugins": [
    {
      "type": "cilium-cni",
      "ipam": {
        "type": "host-local",
        "ranges": [[{"subnet": %q}]]
      }
    },
    {
      "type": "tuning",
      "mtu": 1400
    }
  ]
}
`

// previousCNIConfig returns the CNI configuration that the previous version of the agent wrote on a
// regional node whose pod range is podRange.
func previousCNIConfig(podRange netip.Prefix) []byte {
	return fmt.Appendf(nil, previousCNIConfigTemplate, podRange.String())
}

// CNI writes the CNI configuration of a regional node.
type CNI struct {
	// ConfDir holds the CNI configurations of the node, and BinDir its CNI plugins.
	ConfDir string
	BinDir  string
}

// cniFile is what the file of the agent's CNI configuration holds.
type cniFile int

const (
	// cniFileAbsent means that there is no file under its name.
	cniFileAbsent cniFile = iota
	// cniFileCurrent is the CNI configuration of the agent for the pod range of the node.
	cniFileCurrent
	// cniFilePrevious is the CNI configuration that the previous version of the agent wrote for that
	// range.
	cniFilePrevious
)

// Ensure puts the CNI configuration for podRange in place, and returns nil once it is in place and
// in use: every plugin that it names is installed and no other CNI configuration sorts before it. It
// writes the file only while the directory holds no other CNI configuration, because pods may hold
// addresses that another configuration handed out and host-local does not know; once the file is in
// place, a later configuration does not matter. It replaces a file of its name only when that holds
// what the previous version of the agent wrote for podRange and no other configuration sorts before
// it: both name the same network and range, so host-local keeps the addresses that it handed out. A
// file of its name with other content, or that is not a regular file, is refused and stays.
func (c CNI) Ensure(podRange netip.Prefix) error {
	if err := c.checkPlugins(); err != nil {
		return err
	}
	root, err := os.OpenRoot(c.ConfDir)
	if err != nil {
		return fmt.Errorf("opening the CNI configuration directory: %w", err)
	}
	defer root.Close()
	want := CNIConfig(podRange)
	file, err := ownConfig(root, want, previousCNIConfig(podRange))
	if err != nil {
		return err
	}
	others, err := otherConfigs(root)
	switch {
	case err != nil:
		return err
	case file != cniFileAbsent && sortsBeforeOwn(others):
		return fmt.Errorf("the CNI configuration %s sorts before %s", others[0], CNIConfigName)
	case file == cniFileCurrent:
		return nil
	case file == cniFilePrevious:
		return replaceCNIConfig(root, want)
	case len(others) > 0:
		return fmt.Errorf("%s holds %s before the agent's CNI configuration: pods may hold addresses that host-local does not know",
			c.ConfDir, strings.Join(others, ", "))
	}
	return writeCNIConfig(root, want)
}

// sortsBeforeOwn reports whether the first of the sorted other CNI configurations sorts before the
// agent's.
func sortsBeforeOwn(others []string) bool {
	return len(others) > 0 && others[0] < CNIConfigName
}

// ownConfig reports what the file of the agent's CNI configuration holds: nothing, the content want
// or the content previous. A file of its name that is not a regular file or holds other content is
// an error.
func ownConfig(root *os.Root, want, previous []byte) (cniFile, error) {
	info, err := root.Lstat(CNIConfigName)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return cniFileAbsent, nil
	case err != nil:
		return cniFileAbsent, fmt.Errorf("reading the CNI configuration: %w", err)
	case !info.Mode().IsRegular():
		return cniFileAbsent, fmt.Errorf("%s is not a regular file", CNIConfigName)
	}
	got, err := root.ReadFile(CNIConfigName)
	switch {
	case err != nil:
		return cniFileAbsent, fmt.Errorf("reading the CNI configuration: %w", err)
	case bytes.Equal(got, want):
		return cniFileCurrent, nil
	case bytes.Equal(got, previous):
		return cniFilePrevious, nil
	}
	return cniFileAbsent, fmt.Errorf("%s holds another configuration than the agent's", CNIConfigName)
}

// checkPlugins requires every plugin that the CNI configuration names to be an executable file.
func (c CNI) checkPlugins() error {
	for _, plugin := range cniPlugins {
		info, err := os.Stat(filepath.Join(c.BinDir, plugin))
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
			return fmt.Errorf("the CNI plugin %s is not installed in %s", plugin, c.BinDir)
		}
	}
	return nil
}

// otherConfigs returns, sorted, the other CNI configurations in the directory: the files, not
// directories, with an extension that the container runtime reads.
func otherConfigs(root *os.Root) ([]string, error) {
	dir, err := root.Open(".")
	if err != nil {
		return nil, fmt.Errorf("listing the CNI configurations: %w", err)
	}
	defer dir.Close()
	entries, err := dir.ReadDir(-1)
	if err != nil {
		return nil, fmt.Errorf("listing the CNI configurations: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if name := entry.Name(); name != CNIConfigName && !entry.IsDir() && slices.Contains(cniConfigExtensions, filepath.Ext(name)) {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names, nil
}

// writeCNIConfig creates the CNI configuration without replacing a file that appeared meanwhile: it
// links the temporary file to its name.
func writeCNIConfig(root *os.Root, content []byte) error {
	return putCNIConfig(root, content, root.Link)
}

// replaceCNIConfig replaces the CNI configuration: it renames the temporary file over the old one, so
// that the container runtime reads either the old or the new configuration whole.
func replaceCNIConfig(root *os.Root, content []byte) error {
	return putCNIConfig(root, content, root.Rename)
}

// putCNIConfig writes content to a temporary file, which the container runtime ignores, and puts that
// file under the name of the CNI configuration with put.
func putCNIConfig(root *os.Root, content []byte, put func(oldname, newname string) error) error {
	if err := root.Remove(cniTempName); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("removing a temporary CNI configuration: %w", err)
	}
	f, err := root.OpenFile(cniTempName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return fmt.Errorf("writing the CNI configuration: %w", err)
	}
	defer func() { _ = root.Remove(cniTempName) }()
	_, err = f.Write(content)
	if syncErr := f.Sync(); err == nil {
		err = syncErr
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = put(cniTempName, CNIConfigName)
	}
	if err != nil {
		return fmt.Errorf("writing the CNI configuration: %w", err)
	}
	return nil
}
