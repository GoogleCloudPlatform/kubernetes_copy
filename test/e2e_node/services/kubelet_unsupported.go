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
	"os/exec"

	kubeletconfig "k8s.io/kubernetes/pkg/kubelet/apis/config"
)

// updateKubeletConfig will update platform specific kubelet configuration.
func adjustPlatformSpecificKubeletConfig(kc *kubeletconfig.KubeletConfiguration) (*kubeletconfig.KubeletConfiguration, error) {
	return kc, nil
}

// updateCmdArgs will update platform specific command arguments.
func adjustPlatformSpecificKubeletArgs(cmdArgs []string, isSystemd bool) ([]string, *exec.Cmd, *exec.Cmd, error) {
	var killCommand, restartCommand *exec.Cmd

	return cmdArgs, killCommand, restartCommand, nil
}
