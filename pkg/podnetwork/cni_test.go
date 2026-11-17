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
	"encoding/json"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	. "github.com/onsi/gomega"
)

var testPodRange = netip.MustParsePrefix("10.244.3.0/24")

// requireNoError stops the test on an error of its setup.
func requireNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// writeTestFile writes a file with the given content and mode.
func writeTestFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	requireNoError(t, os.WriteFile(path, []byte(content), 0o600))
	requireNoError(t, os.Chmod(path, mode))
}

// testCNI returns a CNI in temporary directories, with the plugins that its configuration names
// installed and no other.
func testCNI(t *testing.T) CNI {
	t.Helper()
	c := CNI{ConfDir: t.TempDir(), BinDir: t.TempDir()}
	for _, plugin := range []string{"cilium-cni", "host-local"} {
		writeTestFile(t, filepath.Join(c.BinDir, plugin), "", 0o700)
	}
	return c
}

// cniConfigState describes the file of the agent's CNI configuration in dir.
func cniConfigState(dir string) string {
	path := filepath.Clean(filepath.Join(dir, CNIConfigName))
	info, err := os.Lstat(path)
	if err != nil {
		return err.Error()
	}
	content, _ := os.ReadFile(path)
	return info.Mode().Type().String() + " " + string(content)
}

// dirNames returns the names in dir.
func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	requireNoError(t, err)
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func TestCNIConfig(t *testing.T) {
	g := NewWithT(t)
	var config map[string]any
	g.Expect(json.Unmarshal(CNIConfig(testPodRange), &config)).To(Succeed())
	g.Expect(config).To(Equal(map[string]any{
		"cniVersion": "0.3.1",
		"name":       "karpenter-pod-network",
		"plugins": []any{
			map[string]any{"type": "cilium-cni", "ipam": map[string]any{
				"type": "host-local", "ranges": []any{[]any{map[string]any{"subnet": "10.244.3.0/24"}}}}},
		},
	}))
}

// TestCNIConfigBytes pins the exact bytes of the CNI configuration for one range. The agent never
// rewrites its file and refuses a node whose file holds other bytes, so any change of these bytes,
// even of the layout, keeps every regional node that holds the file of an earlier agent not ready and
// tainted until the node is replaced.
func TestCNIConfigBytes(t *testing.T) {
	g := NewWithT(t)
	g.Expect(string(CNIConfig(testPodRange))).To(Equal(`{
  "cniVersion": "0.3.1",
  "name": "karpenter-pod-network",
  "plugins": [
    {
      "type": "cilium-cni",
      "ipam": {
        "type": "host-local",
        "ranges": [[{"subnet": "10.244.3.0/24"}]]
      }
    }
  ]
}
`))
}

// skipEnsureOnWindows skips a test of Ensure on Windows, where Go reports no file as executable
// and Ensure therefore refuses every node at its check of the plugins. The agent runs on Linux.
func skipEnsureOnWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Go reports no file as executable on Windows, so Ensure stops at its check of the plugins")
	}
}

func TestCNIEnsure(t *testing.T) {
	skipEnsureOnWindows(t)
	g := NewWithT(t)
	c := testCNI(t)
	written := "---------- " + string(CNIConfig(testPodRange))
	// None of these is a CNI configuration that the container runtime reads.
	writeTestFile(t, filepath.Join(c.ConfDir, "00-a-notes.txt"), "", 0o600)
	requireNoError(t, os.Mkdir(filepath.Join(c.ConfDir, "00-a-directory.conf"), 0o700))
	// A temporary file left by an interrupted write does not stop the next one.
	writeTestFile(t, filepath.Join(c.ConfDir, cniTempName), "left over", 0o600)

	g.Expect(c.Ensure(testPodRange)).To(Succeed())
	g.Expect(cniConfigState(c.ConfDir)).To(Equal(written))
	g.Expect(dirNames(t, c.ConfDir)).To(ConsistOf("00-a-directory.conf", "00-a-notes.txt", CNIConfigName))

	// The agent keeps its own file.
	g.Expect(c.Ensure(testPodRange)).To(Succeed())
	g.Expect(cniConfigState(c.ConfDir)).To(Equal(written))

	// Once that is in place, a configuration that sorts after it does not matter.
	writeTestFile(t, filepath.Join(c.ConfDir, "05-cilium.conflist"), "{}", 0o600)
	g.Expect(c.Ensure(testPodRange)).To(Succeed())

	// One that sorts before it makes the node not ready, and the file stays.
	writeTestFile(t, filepath.Join(c.ConfDir, "00-azure.conflist"), "{}", 0o600)
	g.Expect(c.Ensure(testPodRange)).To(MatchError(ContainSubstring("00-azure.conflist sorts before " + CNIConfigName)))
	g.Expect(cniConfigState(c.ConfDir)).To(Equal(written))
}

func TestCNIEnsureRefusesAndChangesNothing(t *testing.T) {
	skipEnsureOnWindows(t)
	for name, prepare := range map[string]func(t *testing.T, c CNI){
		"a later .conflist before the first write": func(t *testing.T, c CNI) {
			writeTestFile(t, filepath.Join(c.ConfDir, "05-cilium.conflist"), "{}", 0o600)
		},
		"a later .conf before the first write": func(t *testing.T, c CNI) {
			writeTestFile(t, filepath.Join(c.ConfDir, "10-a.conf"), "{}", 0o600)
		},
		"a later .json before the first write": func(t *testing.T, c CNI) {
			writeTestFile(t, filepath.Join(c.ConfDir, "99-a.json"), "{}", 0o600)
		},
		"an earlier configuration": func(t *testing.T, c CNI) {
			writeTestFile(t, filepath.Join(c.ConfDir, "00-azure.conflist"), "{}", 0o600)
		},
		"a missing plugin": func(t *testing.T, c CNI) {
			requireNoError(t, os.Remove(filepath.Join(c.BinDir, "host-local")))
		},
		"a plugin that is not executable": func(t *testing.T, c CNI) {
			requireNoError(t, os.Chmod(filepath.Join(c.BinDir, "cilium-cni"), 0o600))
		},
		"a plugin that is a directory": func(t *testing.T, c CNI) {
			requireNoError(t, os.Remove(filepath.Join(c.BinDir, "host-local")))
			requireNoError(t, os.Mkdir(filepath.Join(c.BinDir, "host-local"), 0o700))
		},
		"another configuration under its name": func(t *testing.T, c CNI) {
			other := CNIConfig(netip.MustParsePrefix("10.244.4.0/24"))
			writeTestFile(t, filepath.Join(c.ConfDir, CNIConfigName), string(other), 0o600)
		},
		"a directory under its name": func(t *testing.T, c CNI) {
			requireNoError(t, os.Mkdir(filepath.Join(c.ConfDir, CNIConfigName), 0o700))
		},
		"a symbolic link under its name": func(t *testing.T, c CNI) {
			writeTestFile(t, filepath.Join(c.ConfDir, "config.txt"), string(CNIConfig(testPodRange)), 0o600)
			requireNoError(t, os.Symlink("config.txt", filepath.Join(c.ConfDir, CNIConfigName)))
		},
	} {
		t.Run(name, func(t *testing.T) {
			g := NewWithT(t)
			c := testCNI(t)
			prepare(t, c)
			before := cniConfigState(c.ConfDir)
			g.Expect(c.Ensure(testPodRange)).To(HaveOccurred())
			g.Expect(cniConfigState(c.ConfDir)).To(Equal(before))
		})
	}
}

// TestWriteCNIConfigKeepsAFileThatAppearedMeanwhile puts a file under the name of the agent's CNI
// configuration after Ensure looked, as another writer could. The write fails and leaves that file
// as it is.
func TestWriteCNIConfigKeepsAFileThatAppearedMeanwhile(t *testing.T) {
	g := NewWithT(t)
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, CNIConfigName), "{}", 0o600)
	root, err := os.OpenRoot(dir)
	requireNoError(t, err)
	defer root.Close()

	g.Expect(writeCNIConfig(root, CNIConfig(testPodRange))).To(MatchError(ContainSubstring("writing the CNI configuration")))
	g.Expect(cniConfigState(dir)).To(Equal("---------- {}"))
	g.Expect(dirNames(t, dir)).To(ConsistOf(CNIConfigName))
}
