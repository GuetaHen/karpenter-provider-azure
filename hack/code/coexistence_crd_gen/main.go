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

// coexistence_crd_gen writes the coexistence AKSNodeClass CRD derived from the default one:
//
//	go run ./hack/code/coexistence_crd_gen <default AKSNodeClass CRD> <output>
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/Azure/karpenter-provider-azure/pkg/apis/coexistence"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: coexistence_crd_gen <default AKSNodeClass CRD> <output>")
		os.Exit(2)
	}
	if err := generate(os.Args[1], os.Args[2]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func generate(input, output string) error {
	defaultCRD, err := os.ReadFile(filepath.Clean(input))
	if err != nil {
		return err
	}
	crd, err := coexistence.DeriveCRD(defaultCRD)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(output), 0o750); err != nil {
		return err
	}
	return os.WriteFile(output, crd, 0o600)
}
