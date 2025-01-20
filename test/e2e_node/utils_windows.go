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

package e2enode

import (
	"context"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"github.com/onsi/gomega"

	"k8s.io/klog/v2"
	"k8s.io/kubernetes/test/e2e/framework"
)

const (
	kubeletServiceName = "kubelet"
)

// getKubeletServicePID returns the PID of the kubelet service.
func getKubeletServicePID() int {
	cmdLine := []string{"sc.exe", "queryex", kubeletServiceName}

	// kubelet service should have already been registered
	stdout, err := exec.Command(cmdLine[0], cmdLine[1:]...).CombinedOutput()
	framework.ExpectNoError(err)

	regex := regexp.MustCompile(`PID\s*:\s*(\d+)`)
	matches := regex.FindStringSubmatch(string(stdout))
	gomega.Expect(len(matches)).To(gomega.BeNumerically(">", 1), "Found the matched state: %q", stdout)
	pidStr := matches[1]

	pid, err := strconv.Atoi(pidStr)
	framework.ExpectNoError(err)

	return pid
}

// killProcessByPID kills the process with the given PID.
func killProcessByPID(pid int) {
	cmdLine := []string{"taskkill", "/F", "/PID", strconv.Itoa(pid)}

	// kubelet service should have already been registered
	_, err := exec.Command(cmdLine[0], cmdLine[1:]...).CombinedOutput()
	framework.ExpectNoError(err)
}

// findKubeletServiceState searches the unit name among the services known to systemd.
// if the `running` parameter is true, restricts the search among currently running services;
// otherwise, also stopped, failed, exited (non-running in general) services are also considered.
// TODO: Find a uniform way to deal with systemctl/initctl/service operations. #34494
func findKubeletServiceState() string {
	cmdLine := []string{"sc.exe", "query", kubeletServiceName}

	// Assme kubelet service has already been registered
	stdout, err := exec.Command(cmdLine[0], cmdLine[1:]...).CombinedOutput()
	framework.ExpectNoError(err)

	regex := regexp.MustCompile(`(?m)STATE\s*:\s*\d+\s+(\w+)`)
	matches := regex.FindStringSubmatch(string(stdout))
	gomega.Expect(len(matches)).To(gomega.BeNumerically(">", 1), "Found the matched state: %q", stdout)
	state := matches[1]

	return state
}

// restartKubelet restarts the current kubelet service.
// the "current" kubelet service is the instance managed by the current e2e_node test run.
// If `running` is true, restarts only if the current kubelet is actually running. In some cases,
// the kubelet may have exited or can be stopped, typically because it was intentionally stopped
// earlier during a test, or, sometimes, because it just crashed.
// Warning: the "current" kubelet is poorly defined. The "current" kubelet is assumed to be the most
// recent kubelet service unit, IOW there is not a unique ID we use to bind explicitly a kubelet
// instance to a test run.
func restartKubelet(ctx context.Context, running bool) {
	// Check the state of the kubelet service
	state := findKubeletServiceState()

	if strings.EqualFold(state, "RUNNING") {
		// stop the kubelet service
		stdout, err := exec.CommandContext(ctx, "sc.exe", "stop", kubeletServiceName).CombinedOutput()
		framework.ExpectNoError(err, "Failed to stop kubelet service: %v, %s", err, string(stdout))
	}
	if strings.EqualFold(state, "STOP_PENDING") {
		// stop the kubelet service
		pid := getKubeletServicePID()
		killProcessByPID(pid)
	}

	stdout, err := exec.CommandContext(ctx, "sc.exe", "start", kubeletServiceName).CombinedOutput()
	framework.ExpectNoError(err, "Failed to restart kubelet with systemctl: %v, %v", err, stdout)
}

// mustStopKubelet will kill the running kubelet, and returns a func that will restart the process again
func mustStopKubelet(ctx context.Context, f *framework.Framework) func(ctx context.Context) {
	// TODO: change the windows part
	state := findKubeletServiceState()

	if strings.EqualFold(state, "RUNNING") {
		// stop the kubelet service
		stdout, err := exec.CommandContext(ctx, "sc.exe", "stop", kubeletServiceName).CombinedOutput()
		framework.ExpectNoError(err, "Failed to stop kubelet service: %v, %s", err, string(stdout))
	}
	if strings.EqualFold(state, "STOP_PENDING") {
		// stop the kubelet service
		pid := getKubeletServicePID()
		killProcessByPID(pid)
	}

	// wait until the kubelet health check fail
	gomega.Eventually(ctx, func() bool {
		return kubeletHealthCheck(kubeletHealthCheckURL)
	}, f.Timeouts.PodStart, f.Timeouts.Poll).Should(gomega.BeFalseBecause("kubelet was expected to be stopped but it is still running"))

	return func(ctx context.Context) {
		// we should restart service, otherwise the transient service start will fail
		stdout, err := exec.CommandContext(ctx, "sc.exe", "start", kubeletServiceName).CombinedOutput()
		framework.ExpectNoError(err, "Failed to start kubelet service with sc.exe: %v, %s", err, string(stdout))
		waitForKubeletToStart(ctx, f)
	}
}

// TODO: add the winodws part implementation
func stopContainerRuntime() error {
	return nil
}

func startContainerRuntime() error {
	return nil
}

// IsCgroup2UnifiedMode returns whether we are running in cgroup v2 unified mode.
func IsCgroup2UnifiedMode() bool {
	return false
}

// deleteStateFile deletes the state file with the filename.
func deleteStateFile(stateFileName string) {
	// err := exec.Command("powershell", "-c", fmt.Sprintf("rm -f %s", stateFileName)).Run()
	// framework.ExpectNoError(err, "failed to delete the state file")
}

// systemValidation validates the system spec.
func systemValidation(systemSpecFile *string) {
	klog.Warningf("system spec validation is not supported on platform other than linux yet")

	return
}
