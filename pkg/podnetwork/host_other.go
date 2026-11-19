//go:build !linux

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

import "errors"

// errNotLinux is what the agent reports on other systems than Linux, where it cannot program a node.
var errNotLinux = errors.New("the pod network agent programs only Linux nodes")

// Host programs the tunnel of a plan on the node that the agent runs on. Only Linux has an
// implementation; on other systems NewHost fails, so that the agent builds everywhere.
type Host struct{}

// NewHost fails on other systems than Linux.
func NewHost() (*Host, error) {
	return nil, errNotLinux
}

// Apply fails on other systems than Linux.
func (*Host) Apply(Plan) error {
	return errNotLinux
}
