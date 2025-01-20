//go:build linux
// +build linux

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
	"reflect"
	"syscall"
)

// func startProcess starts the process with the given command and arguments
func startProcess(cmd *exec.Cmd, monitorParent bool) error {
	if monitorParent {
		// Death of this test process should kill the server as well.
		attrs := &syscall.SysProcAttr{}
		// Hack to set linux-only field without build tags.
		deathSigField := reflect.ValueOf(attrs).Elem().FieldByName("Pdeathsig")
		if deathSigField.IsValid() {
			deathSigField.Set(reflect.ValueOf(syscall.SIGTERM))
		} else {
			return fmt.Errorf("failed to set Pdeathsig field (non-linux build)")
		}

		cmd.SysProcAttr = attrs
	}

	err := cmd.Start()
	return err
}
