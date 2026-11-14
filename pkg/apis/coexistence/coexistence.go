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

// Package coexistence selects whether this controller runs beside another Karpenter that
// serves AKSNodeClass under the default API group. The selection is read from the
// environment once at process start, because the AKSNodeClass API group is fixed when its
// types register with the scheme during package initialization, before flags are parsed.
package coexistence

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

const (
	// EnvVar enables coexistence mode when it holds a true boolean value.
	EnvVar = "COEXISTENCE_MODE"
	// Group serves AKSNodeClass in coexistence mode.
	Group = "multiregion.io"
	// CRDName is the name of the AKSNodeClass CRD in coexistence mode.
	CRDName = "aksnodeclasses." + Group
)

var enabled, parseErr = parse(os.Getenv(EnvVar))

// Enabled reports whether this process runs in coexistence mode.
func Enabled() bool {
	return enabled
}

// Err reports a value of EnvVar that is not a boolean. Such a value leaves the mode disabled
// and fails option validation, so a typo cannot silently start the controller in the wrong mode.
func Err() error {
	return parseErr
}

func parse(raw string) (bool, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return false, nil
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean, got %q", EnvVar, raw)
	}
	return value, nil
}
