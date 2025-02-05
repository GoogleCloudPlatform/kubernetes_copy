//go:build linux
// +build linux

/*
Copyright 2020 The Kubernetes Authors.

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
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"syscall"
	"time"

	"github.com/coreos/go-systemd/v22/dbus"
	"github.com/onsi/gomega"
	libcontainercgroups "github.com/opencontainers/runc/libcontainer/cgroups"

	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	klog "k8s.io/klog/v2"
	"k8s.io/kubernetes/test/e2e/framework"
	"k8s.io/kubernetes/test/e2e_node/criproxy"
	system "k8s.io/system-validators/validators"
)

// IsCgroup2UnifiedMode returns whether we are running in cgroup v2 unified mode.
func IsCgroup2UnifiedMode() bool {
	return libcontainercgroups.IsCgroup2UnifiedMode()
}

// addCRIProxyInjector registers an injector function for the CRIProxy.
func addCRIProxyInjector(proxy *criproxy.RemoteRuntime, injector func(apiName string) error) error {
	if proxy == nil {
		return fmt.Errorf("failed to add injector because the CRI Proxy is undefined")
	}
	proxy.AddInjector(injector)
	return nil
}

// resetCRIProxyInjector resets all injector functions for the CRIProxy.
func resetCRIProxyInjector(proxy *criproxy.RemoteRuntime) error {
	if proxy == nil {
		return fmt.Errorf("failed to reset injector because the CRI Proxy is undefined")
	}
	proxy.ResetInjectors()
	return nil
}

// findKubeletServiceName searches the unit name among the services known to systemd.
// if the `running` parameter is true, restricts the search among currently running services;
// otherwise, also stopped, failed, exited (non-running in general) services are also considered.
// TODO: Find a uniform way to deal with systemctl/initctl/service operations. #34494
func findKubeletServiceName(running bool) string {
	cmdLine := []string{
		"systemctl", "list-units", "*kubelet*",
	}
	if running {
		cmdLine = append(cmdLine, "--state=running")
	}
	stdout, err := exec.Command("sudo", cmdLine...).CombinedOutput()
	framework.ExpectNoError(err)
	regex := regexp.MustCompile(`(kubelet-\w+)`)
	matches := regex.FindStringSubmatch(string(stdout))
	gomega.Expect(matches).ToNot(gomega.BeEmpty(), "Found more than one kubelet service running: %q", stdout)
	kubeletServiceName := matches[0]
	framework.Logf("Get running kubelet with systemctl: %v, %v", string(stdout), kubeletServiceName)
	return kubeletServiceName
}

func findContainerRuntimeServiceName() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, err := dbus.NewWithContext(ctx)
	framework.ExpectNoError(err, "Failed to setup dbus connection")
	defer conn.Close()

	runtimePids, err := getPidsForProcess(framework.TestContext.ContainerRuntimeProcessName, framework.TestContext.ContainerRuntimePidFile)
	framework.ExpectNoError(err, "failed to get list of container runtime pids")
	gomega.Expect(runtimePids).To(gomega.HaveLen(1), "Unexpected number of container runtime pids. Expected 1 but got %v", len(runtimePids))

	containerRuntimePid := runtimePids[0]

	unitName, err := conn.GetUnitNameByPID(ctx, uint32(containerRuntimePid))
	framework.ExpectNoError(err, "Failed to get container runtime unit name")

	return unitName, nil
}

type containerRuntimeUnitOp int

const (
	startContainerRuntimeUnitOp containerRuntimeUnitOp = iota
	stopContainerRuntimeUnitOp
)

func performContainerRuntimeUnitOp(op containerRuntimeUnitOp) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	conn, err := dbus.NewWithContext(ctx)
	framework.ExpectNoError(err, "Failed to setup dbus connection")
	defer conn.Close()

	if containerRuntimeUnitName == "" {
		containerRuntimeUnitName, err = findContainerRuntimeServiceName()
		framework.ExpectNoError(err, "Failed to find container runtime name")
	}

	reschan := make(chan string)

	switch op {
	case startContainerRuntimeUnitOp:
		_, err = conn.StartUnitContext(ctx, containerRuntimeUnitName, "replace", reschan)
	case stopContainerRuntimeUnitOp:
		_, err = conn.StopUnitContext(ctx, containerRuntimeUnitName, "replace", reschan)
	default:
		framework.Failf("Unexpected container runtime op: %v", op)
	}
	framework.ExpectNoError(err, "dbus connection error")

	job := <-reschan
	gomega.Expect(job).To(gomega.Equal("done"), "Expected job to complete with done")

	return nil
}

func stopContainerRuntime() error {
	return performContainerRuntimeUnitOp(stopContainerRuntimeUnitOp)
}

func startContainerRuntime() error {
	return performContainerRuntimeUnitOp(startContainerRuntimeUnitOp)
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
	kubeletServiceName := findKubeletServiceName(running)
	// reset the kubelet service start-limit-hit
	stdout, err := exec.CommandContext(ctx, "sudo", "systemctl", "reset-failed", kubeletServiceName).CombinedOutput()
	framework.ExpectNoError(err, "Failed to reset kubelet start-limit-hit with systemctl: %v, %s", err, string(stdout))

	stdout, err = exec.CommandContext(ctx, "sudo", "systemctl", "restart", kubeletServiceName).CombinedOutput()
	framework.ExpectNoError(err, "Failed to restart kubelet with systemctl: %v, %s", err, string(stdout))
}

// mustStopKubelet will kill the running kubelet, and returns a func that will restart the process again
func mustStopKubelet(ctx context.Context, f *framework.Framework) func(ctx context.Context) {
	kubeletServiceName := findKubeletServiceName(true)

	// reset the kubelet service start-limit-hit
	stdout, err := exec.CommandContext(ctx, "sudo", "systemctl", "reset-failed", kubeletServiceName).CombinedOutput()
	framework.ExpectNoError(err, "Failed to reset kubelet start-limit-hit with systemctl: %v, %s", err, string(stdout))

	stdout, err = exec.CommandContext(ctx, "sudo", "systemctl", "kill", kubeletServiceName).CombinedOutput()
	framework.ExpectNoError(err, "Failed to stop kubelet with systemctl: %v, %s", err, string(stdout))

	// wait until the kubelet health check fail
	gomega.Eventually(ctx, func() bool {
		return kubeletHealthCheck(kubeletHealthCheckURL)
	}, f.Timeouts.PodStart, f.Timeouts.Poll).Should(gomega.BeFalseBecause("kubelet was expected to be stopped but it is still running"))

	return func(ctx context.Context) {
		// we should restart service, otherwise the transient service start will fail
		stdout, err := exec.CommandContext(ctx, "sudo", "systemctl", "restart", kubeletServiceName).CombinedOutput()
		framework.ExpectNoError(err, "Failed to restart kubelet with systemctl: %v, %v", err, stdout)
		waitForKubeletToStart(ctx, f)
	}
}

// When running the containerized conformance test, we'll mount the
// host root filesystem as readonly to /rootfs.
const rootfs = "/rootfs"

func systemValidation(systemSpecFile *string) {
	spec := &system.DefaultSysSpec
	if *systemSpecFile != "" {
		var err error
		spec, err = loadSystemSpecFromFile(*systemSpecFile)
		if err != nil {
			klog.Exitf("Failed to load system spec: %v", err)
		}
	}
	if framework.TestContext.NodeConformance {
		// Chroot to /rootfs to make system validation can check system
		// as in the root filesystem.
		// TODO(random-liu): Consider to chroot the whole test process to make writing
		// // test easier.
		if err := syscall.Chroot(rootfs); err != nil {
			klog.Exitf("chroot %q failed: %v", rootfs, err)
		}
	}
	warns, errs := system.ValidateSpec(*spec, "remote")
	if len(warns) != 0 {
		klog.Warningf("system validation warns: %v", warns)
	}
	if len(errs) != 0 {
		klog.Exitf("system validation failed: %v", errs)
	}
}

// LoadSystemSpecFromFile returns the system spec from the file with the
// filename.
func loadSystemSpecFromFile(filename string) (*system.SysSpec, error) {
	b, err := os.ReadFile(filename)
	if err != nil {
		return nil, err
	}
	data, err := utilyaml.ToJSON(b)
	if err != nil {
		return nil, err
	}
	spec := new(system.SysSpec)
	if err := json.Unmarshal(data, spec); err != nil {
		return nil, err
	}
	return spec, nil
}

// deleteStateFile deletes the state file with the filename.
func deleteStateFile(stateFileName string) {
	err := exec.Command("/bin/sh", "-c", fmt.Sprintf("rm -f %s", stateFileName)).Run()
	framework.ExpectNoError(err, "failed to delete the state file")
}
