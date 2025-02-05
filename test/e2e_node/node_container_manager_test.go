//go:build linux
// +build linux

/*
Copyright 2017 The Kubernetes Authors.

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
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubeletconfig "k8s.io/kubernetes/pkg/kubelet/apis/config"
	"k8s.io/kubernetes/pkg/kubelet/cm"
	"k8s.io/kubernetes/pkg/kubelet/cm/util"
	"k8s.io/kubernetes/pkg/kubelet/stats/pidlimit"
	admissionapi "k8s.io/pod-security-admission/api"

	"k8s.io/kubernetes/test/e2e/feature"
	"k8s.io/kubernetes/test/e2e/framework"
	e2enodekubelet "k8s.io/kubernetes/test/e2e_node/kubeletconfig"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
)

func setDesiredConfiguration(initialConfig *kubeletconfig.KubeletConfiguration, cgroupManager cm.CgroupManager) {
	initialConfig.EnforceNodeAllocatable = []string{"pods", kubeReservedCgroup, systemReservedCgroup}
	initialConfig.SystemReserved = map[string]string{
		string(v1.ResourceCPU):    "100m",
		string(v1.ResourceMemory): "100Mi",
		string(pidlimit.PIDs):     "1000",
	}
	initialConfig.KubeReserved = map[string]string{
		string(v1.ResourceCPU):    "100m",
		string(v1.ResourceMemory): "100Mi",
		string(pidlimit.PIDs):     "738",
	}
	initialConfig.EvictionHard = map[string]string{"memory.available": "100Mi"}
	// Necessary for allocatable cgroup creation.
	initialConfig.CgroupsPerQOS = true
	initialConfig.KubeReservedCgroup = kubeReservedCgroup
	initialConfig.SystemReservedCgroup = systemReservedCgroup

	if initialConfig.CgroupDriver == "systemd" {
		initialConfig.KubeReservedCgroup = cm.NewCgroupName(cm.RootCgroupName, kubeReservedCgroup).ToSystemd()
		initialConfig.SystemReservedCgroup = cm.NewCgroupName(cm.RootCgroupName, systemReservedCgroup).ToSystemd()
	}
}

var _ = SIGDescribe("Node Container Manager", framework.WithSerial(), func() {
	f := framework.NewDefaultFramework("node-container-manager")
	f.NamespacePodSecurityLevel = admissionapi.LevelPrivileged
	f.Describe("Validate Node Allocatable", feature.NodeAllocatable, func() {
		ginkgo.It("sets up the node and runs the test", func(ctx context.Context) {
			framework.ExpectNoError(runTest(ctx, f))
		})
	})
	f.Describe("Validate CGroup management", func() {
		// Regression test for https://issues.k8s.io/125923
		// In this issue there's a race involved with systemd which seems to manifest most likely, or perhaps only
		// (data gathered so far seems inconclusive) on the very first boot of the machine, so restarting the kubelet
		// seems not sufficient. OTOH, the exact reproducer seems to require a dedicate lane with only this test, or
		// to reboot the machine before to run this test. Both are practically unrealistic in CI.
		// The closest approximation is this test in this current form, using a kubelet restart. This at least
		// acts as non regression testing, so it still brings value.
		ginkgo.It("should correctly start with cpumanager none policy in use with systemd", func(ctx context.Context) {
			if !IsCgroup2UnifiedMode() {
				ginkgo.Skip("this test requires cgroups v2")
			}

			var err error
			var oldCfg *kubeletconfig.KubeletConfiguration
			// Get current kubelet configuration
			oldCfg, err = getCurrentKubeletConfig(ctx)
			framework.ExpectNoError(err)

			ginkgo.DeferCleanup(func(ctx context.Context) {
				if oldCfg != nil {
					// Update the Kubelet configuration.
					framework.ExpectNoError(e2enodekubelet.WriteKubeletConfigFile(oldCfg))

					ginkgo.By("Restarting the kubelet")
					restartKubelet(ctx, true)

					waitForKubeletToStart(ctx, f)
					ginkgo.By("Started the kubelet")
				}
			})

			newCfg := oldCfg.DeepCopy()
			// Change existing kubelet configuration
			newCfg.CPUManagerPolicy = "none"
			newCfg.CgroupDriver = "systemd"
			newCfg.FailCgroupV1 = true // extra safety. We want to avoid false negatives though, so we added the skip check earlier

			// Update the Kubelet configuration.
			framework.ExpectNoError(e2enodekubelet.WriteKubeletConfigFile(newCfg))

			ginkgo.By("Restarting the kubelet")
			restartKubelet(ctx, true)

			waitForKubeletToStart(ctx, f)
			ginkgo.By("Started the kubelet")

			gomega.Consistently(ctx, func(ctx context.Context) bool {
				return getNodeReadyStatus(ctx, f) && kubeletHealthCheck(kubeletHealthCheckURL)
			}).WithTimeout(2 * time.Minute).WithPolling(2 * time.Second).Should(gomega.BeTrueBecause("node keeps reporting ready status"))
		})
	})
})

func expectFileValToEqual(filePath string, expectedValue, delta int64) error {
	out, err := os.ReadFile(filePath)
	if err != nil {
		return fmt.Errorf("failed to read file %q", filePath)
	}
	actual, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return fmt.Errorf("failed to parse output %v", err)
	}

	// Ensure that values are within a delta range to work around rounding errors.
	if (actual < (expectedValue - delta)) || (actual > (expectedValue + delta)) {
		return fmt.Errorf("Expected value at %q to be between %d and %d. Got %d", filePath, (expectedValue - delta), (expectedValue + delta), actual)
	}
	return nil
}

func getAllocatableLimits(cpu, memory, pids string, capacity v1.ResourceList) (*resource.Quantity, *resource.Quantity, *resource.Quantity) {
	var allocatableCPU, allocatableMemory, allocatablePIDs *resource.Quantity
	// Total cpu reservation is 200m.
	for k, v := range capacity {
		if k == v1.ResourceCPU {
			c := v.DeepCopy()
			allocatableCPU = &c
			allocatableCPU.Sub(resource.MustParse(cpu))
		}
		if k == v1.ResourceMemory {
			c := v.DeepCopy()
			allocatableMemory = &c
			allocatableMemory.Sub(resource.MustParse(memory))
		}
	}
	// Process IDs are not a node allocatable, so we have to do this ad hoc
	pidlimits, err := pidlimit.Stats()
	if err == nil && pidlimits != nil && pidlimits.MaxPID != nil {
		allocatablePIDs = resource.NewQuantity(int64(*pidlimits.MaxPID), resource.DecimalSI)
		allocatablePIDs.Sub(resource.MustParse(pids))
	}
	return allocatableCPU, allocatableMemory, allocatablePIDs
}

const (
	kubeReservedCgroup    = "kube-reserved"
	systemReservedCgroup  = "system-reserved"
	nodeAllocatableCgroup = "kubepods"
)

func createIfNotExists(cm cm.CgroupManager, cgroupConfig *cm.CgroupConfig) error {
	if !cm.Exists(cgroupConfig.Name) {
		if err := cm.Create(cgroupConfig); err != nil {
			return err
		}
	}
	return nil
}

func createTemporaryCgroupsForReservation(cgroupManager cm.CgroupManager) error {
	// Create kube reserved cgroup
	cgroupConfig := &cm.CgroupConfig{
		Name: cm.NewCgroupName(cm.RootCgroupName, kubeReservedCgroup),
	}
	if err := createIfNotExists(cgroupManager, cgroupConfig); err != nil {
		return err
	}
	// Create system reserved cgroup
	cgroupConfig.Name = cm.NewCgroupName(cm.RootCgroupName, systemReservedCgroup)

	return createIfNotExists(cgroupManager, cgroupConfig)
}

func destroyTemporaryCgroupsForReservation(cgroupManager cm.CgroupManager) error {
	// Create kube reserved cgroup
	cgroupConfig := &cm.CgroupConfig{
		Name: cm.NewCgroupName(cm.RootCgroupName, kubeReservedCgroup),
	}
	if err := cgroupManager.Destroy(cgroupConfig); err != nil {
		return err
	}
	cgroupConfig.Name = cm.NewCgroupName(cm.RootCgroupName, systemReservedCgroup)
	return cgroupManager.Destroy(cgroupConfig)
}

func runTest(ctx context.Context, f *framework.Framework) error {
	var oldCfg *kubeletconfig.KubeletConfiguration
	subsystems, err := cm.GetCgroupSubsystems()
	if err != nil {
		return err
	}
	// Get current kubelet configuration
	oldCfg, err = getCurrentKubeletConfig(ctx)
	if err != nil {
		return err
	}

	// Create a cgroup manager object for manipulating cgroups.
	cgroupManager := cm.NewCgroupManager(subsystems, oldCfg.CgroupDriver)

	ginkgo.DeferCleanup(destroyTemporaryCgroupsForReservation, cgroupManager)
	ginkgo.DeferCleanup(func(ctx context.Context) {
		if oldCfg != nil {
			// Update the Kubelet configuration.
			ginkgo.By("Stopping the kubelet")
			restartKubelet := mustStopKubelet(ctx, f)

			// wait until the kubelet health check will fail
			gomega.Eventually(ctx, func() bool {
				return kubeletHealthCheck(kubeletHealthCheckURL)
			}, time.Minute, time.Second).Should(gomega.BeFalseBecause("expected kubelet health check to be failed"))

			framework.ExpectNoError(e2enodekubelet.WriteKubeletConfigFile(oldCfg))

			ginkgo.By("Restarting the kubelet")
			restartKubelet(ctx)

			// wait until the kubelet health check will succeed
			gomega.Eventually(ctx, func(ctx context.Context) bool {
				return kubeletHealthCheck(kubeletHealthCheckURL)
			}, 2*time.Minute, 5*time.Second).Should(gomega.BeTrueBecause("expected kubelet to be in healthy state"))
		}
	})
	if err := createTemporaryCgroupsForReservation(cgroupManager); err != nil {
		return err
	}

	newCfg := oldCfg.DeepCopy()
	// Change existing kubelet configuration
	setDesiredConfiguration(newCfg, cgroupManager)
	// Set the new kubelet configuration.
	// Update the Kubelet configuration.
	ginkgo.By("Stopping the kubelet")
	restartKubelet := mustStopKubelet(ctx, f)

	expectedNAPodCgroup := cm.NewCgroupName(cm.RootCgroupName, nodeAllocatableCgroup)

	// Cleanup from the previous kubelet, to verify the new one creates it correctly
	if err := cgroupManager.Destroy(&cm.CgroupConfig{
		Name: cm.NewCgroupName(expectedNAPodCgroup),
	}); err != nil {
		return err
	}
	if cgroupManager.Exists(expectedNAPodCgroup) {
		return fmt.Errorf("Expected Node Allocatable Cgroup %q not to exist", expectedNAPodCgroup)
	}

	framework.ExpectNoError(e2enodekubelet.WriteKubeletConfigFile(newCfg))

	ginkgo.By("Starting the kubelet")
	restartKubelet(ctx)

	// wait until the kubelet health check will succeed
	gomega.Eventually(ctx, func() bool {
		return kubeletHealthCheck(kubeletHealthCheckURL)
	}, 2*time.Minute, 5*time.Second).Should(gomega.BeTrueBecause("expected kubelet to be in healthy state"))

	if err != nil {
		return err
	}
	// Set new config and current config.
	currentConfig := newCfg

	if !cgroupManager.Exists(expectedNAPodCgroup) {
		return fmt.Errorf("Expected Node Allocatable Cgroup %q to exist", expectedNAPodCgroup)
	}

	memoryLimitFile := "memory.limit_in_bytes"
	if IsCgroup2UnifiedMode() {
		memoryLimitFile = "memory.max"
	}

	// TODO: Update cgroupManager to expose a Status interface to get current Cgroup Settings.
	// The node may not have updated capacity and allocatable yet, so check that it happens eventually.
	gomega.Eventually(ctx, func(ctx context.Context) error {
		nodeList, err := f.ClientSet.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
		if err != nil {
			return err
		}
		if len(nodeList.Items) != 1 {
			return fmt.Errorf("Unexpected number of node objects for node e2e. Expects only one node: %+v", nodeList)
		}
		cgroupName := nodeAllocatableCgroup
		if currentConfig.CgroupDriver == "systemd" {
			cgroupName = nodeAllocatableCgroup + ".slice"
		}

		node := nodeList.Items[0]
		capacity := node.Status.Capacity
		allocatableCPU, allocatableMemory, allocatablePIDs := getAllocatableLimits("200m", "200Mi", "1738", capacity)
		// Total Memory reservation is 200Mi excluding eviction thresholds.
		// Expect CPU shares on node allocatable cgroup to equal allocatable.
		shares := int64(cm.MilliCPUToShares(allocatableCPU.MilliValue()))
		if IsCgroup2UnifiedMode() {
			// convert to the cgroup v2 cpu.weight value
			cpuShares, err := util.CPUSharesToCPUWeight(uint64(shares))
			if err != nil {
				return err
			}
			if err := expectFileValToEqual(filepath.Join(subsystems.MountPoints["cpu"], cgroupName, "cpu.weight"), int64(cpuShares), 10); err != nil {
				return err
			}
		} else {
			if err := expectFileValToEqual(filepath.Join(subsystems.MountPoints["cpu"], cgroupName, "cpu.shares"), shares, 10); err != nil {
				return err
			}
		}
		// Expect Memory limit on node allocatable cgroup to equal allocatable.
		if err := expectFileValToEqual(filepath.Join(subsystems.MountPoints["memory"], cgroupName, memoryLimitFile), allocatableMemory.Value(), 0); err != nil {
			return err
		}
		// Expect PID limit on node allocatable cgroup to equal allocatable.
		if err := expectFileValToEqual(filepath.Join(subsystems.MountPoints["pids"], cgroupName, "pids.max"), allocatablePIDs.Value(), 0); err != nil {
			return err
		}

		// Check that Allocatable reported to scheduler includes eviction thresholds.
		schedulerAllocatable := node.Status.Allocatable
		// Memory allocatable should take into account eviction thresholds.
		// Process IDs are not a scheduler resource and as such cannot be tested here.
		allocatableCPU, allocatableMemory, _ = getAllocatableLimits("200m", "300Mi", "1738", capacity)
		// Expect allocatable to include all resources in capacity.
		if len(schedulerAllocatable) != len(capacity) {
			return fmt.Errorf("Expected all resources in capacity to be found in allocatable")
		}
		// CPU based evictions are not supported.
		if allocatableCPU.Cmp(schedulerAllocatable[v1.ResourceCPU]) != 0 {
			return fmt.Errorf("Unexpected cpu allocatable value exposed by the node. Expected: %v, got: %v, capacity: %v", allocatableCPU, schedulerAllocatable[v1.ResourceCPU], capacity[v1.ResourceCPU])
		}
		if allocatableMemory.Cmp(schedulerAllocatable[v1.ResourceMemory]) != 0 {
			return fmt.Errorf("Unexpected memory allocatable value exposed by the node. Expected: %v, got: %v, capacity: %v", allocatableMemory, schedulerAllocatable[v1.ResourceMemory], capacity[v1.ResourceMemory])
		}
		return nil
	}, time.Minute, 5*time.Second).Should(gomega.Succeed())

	cgroupPath := ""
	if currentConfig.CgroupDriver == "systemd" {
		cgroupPath = cm.NewCgroupName(cm.RootCgroupName, kubeReservedCgroup).ToSystemd()
	} else {
		cgroupPath = cgroupManager.Name(cm.NewCgroupName(cm.RootCgroupName, kubeReservedCgroup))
	}
	// Expect CPU shares on kube reserved cgroup to equal it's reservation which is `100m`.
	kubeReservedCPU := resource.MustParse(currentConfig.KubeReserved[string(v1.ResourceCPU)])
	shares := int64(cm.MilliCPUToShares(kubeReservedCPU.MilliValue()))
	if IsCgroup2UnifiedMode() {
		cpuShares, err := util.CPUSharesToCPUWeight(uint64(shares))
		if err != nil {
			return err
		}
		if err := expectFileValToEqual(filepath.Join(subsystems.MountPoints["cpu"], cgroupPath, "cpu.weight"), int64(cpuShares), 10); err != nil {
			return err
		}
	} else {
		if err := expectFileValToEqual(filepath.Join(subsystems.MountPoints["cpu"], cgroupPath, "cpu.shares"), shares, 10); err != nil {
			return err
		}
	}
	// Expect Memory limit kube reserved cgroup to equal configured value `100Mi`.
	kubeReservedMemory := resource.MustParse(currentConfig.KubeReserved[string(v1.ResourceMemory)])
	if err := expectFileValToEqual(filepath.Join(subsystems.MountPoints["memory"], cgroupPath, memoryLimitFile), kubeReservedMemory.Value(), 0); err != nil {
		return err
	}
	// Expect process ID limit kube reserved cgroup to equal configured value `738`.
	kubeReservedPIDs := resource.MustParse(currentConfig.KubeReserved[string(pidlimit.PIDs)])
	if err := expectFileValToEqual(filepath.Join(subsystems.MountPoints["pids"], cgroupPath, "pids.max"), kubeReservedPIDs.Value(), 0); err != nil {
		return err
	}

	if currentConfig.CgroupDriver == "systemd" {
		cgroupPath = cm.NewCgroupName(cm.RootCgroupName, systemReservedCgroup).ToSystemd()
	} else {
		cgroupPath = cgroupManager.Name(cm.NewCgroupName(cm.RootCgroupName, systemReservedCgroup))
	}

	// Expect CPU shares on system reserved cgroup to equal it's reservation which is `100m`.
	systemReservedCPU := resource.MustParse(currentConfig.SystemReserved[string(v1.ResourceCPU)])
	shares = int64(cm.MilliCPUToShares(systemReservedCPU.MilliValue()))
	if IsCgroup2UnifiedMode() {
		cpuShares, err := util.CPUSharesToCPUWeight(uint64(shares))
		if err != nil {
			return err
		}
		if err := expectFileValToEqual(filepath.Join(subsystems.MountPoints["cpu"], cgroupPath, "cpu.weight"), int64(cpuShares), 10); err != nil {
			return err
		}
	} else {
		if err := expectFileValToEqual(filepath.Join(subsystems.MountPoints["cpu"], cgroupPath, "cpu.shares"), shares, 10); err != nil {
			return err
		}
	}
	// Expect Memory limit on node allocatable cgroup to equal allocatable.
	systemReservedMemory := resource.MustParse(currentConfig.SystemReserved[string(v1.ResourceMemory)])
	if err := expectFileValToEqual(filepath.Join(subsystems.MountPoints["memory"], cgroupPath, memoryLimitFile), systemReservedMemory.Value(), 0); err != nil {
		return err
	}
	// Expect process ID limit system reserved cgroup to equal configured value `1000`.
	systemReservedPIDs := resource.MustParse(currentConfig.SystemReserved[string(pidlimit.PIDs)])
	if err := expectFileValToEqual(filepath.Join(subsystems.MountPoints["pids"], cgroupPath, "pids.max"), systemReservedPIDs.Value(), 0); err != nil {
		return err
	}
	return nil
}
