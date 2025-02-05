//go:build !linux && !windows
// +build !linux,!windows

/*
Copyright 2025 The Kubernetes Authors.

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

package services

import (
	"fmt"
	"os/exec"
)

// func startProcess starts the process with the given command and arguments
func startProcess(cmd *exec.Cmd, monitorParent bool) error {
	return fmt.Errorf("failed to start process on an unsupported platform")
}
