//go:build windows
// +build windows

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
	"path/filepath"
	"strings"

	"k8s.io/klog/v2"
	kubeletconfig "k8s.io/kubernetes/pkg/kubelet/apis/config"
)

// updateKubeletConfig will update platform specific kubelet configuration.
func adjustPlatformSpecificKubeletConfig(kc *kubeletconfig.KubeletConfiguration) (*kubeletconfig.KubeletConfiguration, error) {
	kc.ResolverConfig = ""

	return kc, nil
}

// updateCmdArgs will update platform specific command arguments.
func adjustPlatformSpecificKubeletArgs(cmdArgs []string, isSystemd bool) ([]string, *exec.Cmd, *exec.Cmd, error) {
	// Get the log runner, for kubelet logging purpose
	kubletFullPath := cmdArgs[0]
	dir := filepath.Dir(kubletFullPath)
	fullPath := filepath.Join(dir, "kube-log-runner.exe")

	escapedPath := `"` + fullPath + `"`

	logRunnerArgs := []string{}
	logRunnerArgs = append(logRunnerArgs, escapedPath)
	logfileFullPath := filepath.Join(dir, "kubelet.log")
	logRunnerArgs = append(logRunnerArgs, "--log-file="+logfileFullPath)
	// TODO: Add log rotation after the kube-log-runner is enhanced to support it

	cmdArgs = append(logRunnerArgs, cmdArgs...)

	cmdArgs = append(cmdArgs, "--image-gc-high-threshold", "95")

	cmdArgs = append(cmdArgs, "--windows-service")

	// Create the argument string with proper escaping
	var kubeletArgListStr string
	for _, arg := range cmdArgs {
		kubeletArgListStr += arg
		kubeletArgListStr += " "
	}
	kubeletArgListStr = fmt.Sprintf("'%s'", kubeletArgListStr)

	klog.Infof("Kubelet command line: %s", kubeletArgListStr)

	// Register the new kubelet service, through sc.exe
	var newCmdArgs []string
	newCmdArgs = append(newCmdArgs, "sc.exe")
	newCmdArgs = append(newCmdArgs, "create")
	newCmdArgs = append(newCmdArgs, "kubelet")
	newCmdArgs = append(newCmdArgs, "binPath= "+kubeletArgListStr)
	//newCmdArgs = append(newCmdArgs, "start= auto")
	newCmdArgs = append(newCmdArgs, "depend= containerd")

	cmd := strings.Join(newCmdArgs, " ")

	// First of all, remove the existing kubelet service
	// Safe to ignore the error here, as the service may not exist
	removeCmd := exec.Command("sc.exe", "stop", "kubelet")
	removeCmd.Start()
	err := removeCmd.Wait()
	if err != nil {
		klog.Info("Failed to stop the kubelet service")
	}

	removeCmd = exec.Command("sc.exe", "delete", "kubelet")
	removeCmd.Start()
	err = removeCmd.Wait()
	if err != nil {
		klog.Info("Failed to delete the kubelet service, it could be the kubelet service does not exist")
	}

	// Register the new kubelet service
	// Use powershell as it can handle the escape of the command line arguments correctly
	pscmd := exec.Command("powershell", "-NoProfile", "-Command", cmd)
	pscmd.Start()
	err = pscmd.Wait()
	if err != nil {
		klog.Info("Failed to register the kubelet service")
		return nil, nil, nil, err
	}

	// Return the command which will be used to start the kubelet service
	newCmdArgs = []string{}
	newCmdArgs = append(newCmdArgs, "sc.exe")
	newCmdArgs = append(newCmdArgs, "start")
	newCmdArgs = append(newCmdArgs, "kubelet")

	killCommand := exec.Command("sc.exe", "stop", "kubelet")
	restartCommand := exec.Command("cmd.exe", "/C", "sc.exe stop kubelet && sc.exe start kubelet")

	return newCmdArgs, killCommand, restartCommand, nil
}
