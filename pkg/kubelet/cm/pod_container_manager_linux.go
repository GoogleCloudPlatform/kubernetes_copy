/*
Copyright 2016 The Kubernetes Authors.

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

package cm

import (
	"errors"
	"fmt"
	"os"
	"path"
	"strings"

	libcontainercgroups "github.com/opencontainers/runc/libcontainer/cgroups"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/klog/v2"
	v1qos "k8s.io/kubernetes/pkg/apis/core/v1/helper/qos"
	kubefeatures "k8s.io/kubernetes/pkg/features"
)

const (
	podCgroupNamePrefix = "pod"
)

// podContainerManagerImpl implements podContainerManager interface.
// It is the general implementation which allows pod level container
// management if qos Cgroup is enabled.
type podContainerManagerImpl struct {
	// qosContainersInfo hold absolute paths of the top level qos containers
	qosContainersInfo QOSContainersInfo
	// Stores the mounted cgroup subsystems
	subsystems *CgroupSubsystems
	// cgroupManager is the cgroup Manager Object responsible for managing all
	// pod cgroups.
	cgroupManager CgroupManager
	// Maximum number of pids in a pod
	podPidsLimit int64
	// enforceCPULimits controls whether cfs quota is enforced or not
	enforceCPULimits bool
	// cpuCFSQuotaPeriod is the cfs period value, cfs_period_us, setting per
	// node for all containers in usec
	cpuCFSQuotaPeriod uint64
	// podContainerManager is the ContainerManager running on the machine
	podContainerManager ContainerManager
}

// Make sure that podContainerManagerImpl implements the PodContainerManager interface
var _ PodContainerManager = &podContainerManagerImpl{}

// Exists checks if the pod's cgroup already exists
func (m *podContainerManagerImpl) Exists(pod *v1.Pod) bool {
	podContainerName, _ := m.GetPodContainerName(pod)
	return m.cgroupManager.Exists(podContainerName)
}

// EnsureExists takes a pod as argument and makes sure that
// pod cgroup exists if qos cgroup hierarchy flag is enabled.
// If the pod level container doesn't already exist it is created.
func (m *podContainerManagerImpl) EnsureExists(pod *v1.Pod) error {
	// check if container already exist
	alreadyExists := m.Exists(pod)
	if !alreadyExists {
		enforceCPULimits := m.enforceCPULimits
		if utilfeature.DefaultFeatureGate.Enabled(kubefeatures.DisableCPUQuotaWithExclusiveCPUs) && m.podContainerManager.PodHasExclusiveCPUs(pod) {
			klog.V(2).InfoS("Disabled CFS quota", "pod", klog.KObj(pod))
			enforceCPULimits = false
		}
		enforceMemoryQoS := false
		if utilfeature.DefaultFeatureGate.Enabled(kubefeatures.MemoryQoS) &&
			libcontainercgroups.IsCgroup2UnifiedMode() {
			enforceMemoryQoS = true
		}
		// Create the pod container
		podContainerName, _ := m.GetPodContainerName(pod)
		containerConfig := &CgroupConfig{
			Name:               podContainerName,
			ResourceParameters: ResourceConfigForPod(pod, enforceCPULimits, m.cpuCFSQuotaPeriod, enforceMemoryQoS),
		}
		if m.podPidsLimit > 0 {
			containerConfig.ResourceParameters.PidsLimit = &m.podPidsLimit
		}
		if enforceMemoryQoS {
			klog.V(4).InfoS("MemoryQoS config for pod", "pod", klog.KObj(pod), "unified", containerConfig.ResourceParameters.Unified)
		}
		if err := m.cgroupManager.Create(containerConfig); err != nil {
			return fmt.Errorf("failed to create container for %v : %v", podContainerName, err)
		}
	}
	return nil
}

// GetPodContainerName returns the CgroupName identifier, and its literal cgroupfs form on the host.
func (m *podContainerManagerImpl) GetPodContainerName(pod *v1.Pod) (CgroupName, string) {
	podQOS := v1qos.GetPodQOS(pod)
	// Get the parent QOS container name
	var parentContainer CgroupName
	switch podQOS {
	case v1.PodQOSGuaranteed:
		parentContainer = m.qosContainersInfo.Guaranteed
	case v1.PodQOSBurstable:
		parentContainer = m.qosContainersInfo.Burstable
	case v1.PodQOSBestEffort:
		parentContainer = m.qosContainersInfo.BestEffort
	}
	podContainer := GetPodCgroupNameSuffix(pod.UID)

	// Get the absolute path of the cgroup
	cgroupName := NewCgroupName(parentContainer, podContainer)
	// Get the literal cgroupfs name
	cgroupfsName := m.cgroupManager.Name(cgroupName)

	return cgroupName, cgroupfsName
}

func (m *podContainerManagerImpl) GetPodCgroupMemoryUsage(pod *v1.Pod) (uint64, error) {
	podCgroupName, _ := m.GetPodContainerName(pod)
	memUsage, err := m.cgroupManager.MemoryUsage(podCgroupName)
	if err != nil {
		return 0, err
	}
	return uint64(memUsage), nil
}

func (m *podContainerManagerImpl) GetPodCgroupConfig(pod *v1.Pod, resource v1.ResourceName) (*ResourceConfig, error) {
	podCgroupName, _ := m.GetPodContainerName(pod)
	return m.cgroupManager.GetCgroupConfig(podCgroupName, resource)
}

func (m *podContainerManagerImpl) SetPodCgroupConfig(pod *v1.Pod, resourceConfig *ResourceConfig) error {
	podCgroupName, _ := m.GetPodContainerName(pod)
	return m.cgroupManager.SetCgroupConfig(podCgroupName, resourceConfig)
}

// Kill one process ID
func (m *podContainerManagerImpl) killOnePid(pid int) error {
	// os.FindProcess never returns an error on POSIX
	// https://go-review.googlesource.com/c/go/+/19093
	p, _ := os.FindProcess(pid)
	if err := p.Kill(); err != nil {
		// If the process already exited, that's fine.
		if errors.Is(err, os.ErrProcessDone) {
			klog.V(3).InfoS("Process no longer exists", "pid", pid)
			return nil
		}
		return err
	}
	return nil
}

// Scan through the whole cgroup directory and kill all processes either
// attached to the pod cgroup or to a container cgroup under the pod cgroup
func (m *podContainerManagerImpl) tryKillingCgroupProcesses(podCgroup CgroupName) error {
	pidsToKill := m.cgroupManager.Pids(podCgroup)
	// No pids charged to the terminated pod cgroup return
	if len(pidsToKill) == 0 {
		return nil
	}

	var errlist []error
	// os.Kill often errors out,
	// We try killing all the pids multiple times
	removed := map[int]bool{}
	for i := 0; i < 5; i++ {
		if i != 0 {
			klog.V(3).InfoS("Attempt failed to kill all unwanted process from cgroup, retrying", "attempt", i, "cgroupName", podCgroup)
		}
		errlist = []error{}
		for _, pid := range pidsToKill {
			if _, ok := removed[pid]; ok {
				continue
			}
			klog.V(3).InfoS("Attempting to kill process from cgroup", "pid", pid, "cgroupName", podCgroup)
			if err := m.killOnePid(pid); err != nil {
				klog.V(3).InfoS("Failed to kill process from cgroup", "pid", pid, "cgroupName", podCgroup, "err", err)
				errlist = append(errlist, err)
			} else {
				removed[pid] = true
			}
		}
		if len(errlist) == 0 {
			klog.V(3).InfoS("Successfully killed all unwanted processes from cgroup", "cgroupName", podCgroup)
			return nil
		}
	}
	return utilerrors.NewAggregate(errlist)
}

// Destroy destroys the pod container cgroup paths
func (m *podContainerManagerImpl) Destroy(podCgroup CgroupName) error {
	// Try killing all the processes attached to the pod cgroup
	if err := m.tryKillingCgroupProcesses(podCgroup); err != nil {
		klog.InfoS("Failed to kill all the processes attached to cgroup", "cgroupName", podCgroup, "err", err)
		return fmt.Errorf("failed to kill all the processes attached to the %v cgroups : %v", podCgroup, err)
	}

	// Now its safe to remove the pod's cgroup
	containerConfig := &CgroupConfig{
		Name:               podCgroup,
		ResourceParameters: &ResourceConfig{},
	}
	if err := m.cgroupManager.Destroy(containerConfig); err != nil {
		klog.InfoS("Failed to delete cgroup paths", "cgroupName", podCgroup, "err", err)
		return fmt.Errorf("failed to delete cgroup paths for %v : %v", podCgroup, err)
	}
	return nil
}

// ReduceCPULimits reduces the CPU CFS values to the minimum amount of shares.
func (m *podContainerManagerImpl) ReduceCPULimits(podCgroup CgroupName) error {
	return m.cgroupManager.ReduceCPULimits(podCgroup)
}

// IsPodCgroup returns true if the literal cgroupfs name corresponds to a pod
func (m *podContainerManagerImpl) IsPodCgroup(cgroupfs string) (bool, types.UID) {
	// convert the literal cgroupfs form to the driver specific value
	cgroupName := m.cgroupManager.CgroupName(cgroupfs)
	qosContainersList := [3]CgroupName{m.qosContainersInfo.BestEffort, m.qosContainersInfo.Burstable, m.qosContainersInfo.Guaranteed}
	basePath := ""
	for _, qosContainerName := range qosContainersList {
		// a pod cgroup is a direct child of a qos node, so check if its a match
		if len(cgroupName) == len(qosContainerName)+1 {
			basePath = cgroupName[len(qosContainerName)]
		}
	}
	if basePath == "" {
		return false, types.UID("")
	}
	if !strings.HasPrefix(basePath, podCgroupNamePrefix) {
		return false, types.UID("")
	}
	parts := strings.Split(basePath, podCgroupNamePrefix)
	if len(parts) != 2 {
		return false, types.UID("")
	}
	return true, types.UID(parts[1])
}

// GetAllPodsFromCgroups scans through all the subsystems of pod cgroups
// Get list of pods whose cgroup still exist on the cgroup mounts
func (m *podContainerManagerImpl) GetAllPodsFromCgroups() (map[types.UID]CgroupName, error) {
	// Map for storing all the found pods on the disk
	foundPods := make(map[types.UID]CgroupName)
	qosContainersList := [3]CgroupName{m.qosContainersInfo.BestEffort, m.qosContainersInfo.Burstable, m.qosContainersInfo.Guaranteed}
	// Scan through all the subsystem mounts
	// and through each QoS cgroup directory for each subsystem mount
	// If a pod cgroup exists in even a single subsystem mount
	// we will attempt to delete it
	for _, val := range m.subsystems.MountPoints {
		for _, qosContainerName := range qosContainersList {
			// get the subsystems QoS cgroup absolute name
			qcConversion := m.cgroupManager.Name(qosContainerName)
			qc := path.Join(val, qcConversion)
			dirInfo, err := os.ReadDir(qc)
			if err != nil {
				if os.IsNotExist(err) {
					continue
				}
				return nil, fmt.Errorf("failed to read the cgroup directory %v : %v", qc, err)
			}
			for i := range dirInfo {
				// its not a directory, so continue on...
				if !dirInfo[i].IsDir() {
					continue
				}
				// convert the concrete cgroupfs name back to an internal identifier
				// this is needed to handle path conversion for systemd environments.
				// we pass the fully qualified path so decoding can work as expected
				// since systemd encodes the path in each segment.
				cgroupfsPath := path.Join(qcConversion, dirInfo[i].Name())
				internalPath := m.cgroupManager.CgroupName(cgroupfsPath)
				// we only care about base segment of the converted path since that
				// is what we are reading currently to know if it is a pod or not.
				basePath := internalPath[len(internalPath)-1]
				if !strings.Contains(basePath, podCgroupNamePrefix) {
					continue
				}
				// we then split the name on the pod prefix to determine the uid
				parts := strings.Split(basePath, podCgroupNamePrefix)
				// the uid is missing, so we log the unexpected cgroup not of form pod<uid>
				if len(parts) != 2 {
					klog.InfoS("Pod cgroup manager ignored unexpected cgroup because it is not a pod", "path", cgroupfsPath)
					continue
				}
				podUID := parts[1]
				foundPods[types.UID(podUID)] = internalPath
			}
		}
	}
	return foundPods, nil
}

// podContainerManagerNoop implements podContainerManager interface.
// It is a no-op implementation and basically does nothing
// podContainerManagerNoop is used in case the QoS cgroup Hierarchy is not
// enabled, so Exists() returns true always as the cgroupRoot
// is expected to always exist.
type podContainerManagerNoop struct {
	cgroupRoot CgroupName
}

// Make sure that podContainerManagerStub implements the PodContainerManager interface
var _ PodContainerManager = &podContainerManagerNoop{}

func (m *podContainerManagerNoop) Exists(_ *v1.Pod) bool {
	return true
}

func (m *podContainerManagerNoop) EnsureExists(_ *v1.Pod) error {
	return nil
}

func (m *podContainerManagerNoop) GetPodContainerName(_ *v1.Pod) (CgroupName, string) {
	return m.cgroupRoot, ""
}

func (m *podContainerManagerNoop) GetPodContainerNameForDriver(_ *v1.Pod) string {
	return ""
}

// Destroy destroys the pod container cgroup paths
func (m *podContainerManagerNoop) Destroy(_ CgroupName) error {
	return nil
}

func (m *podContainerManagerNoop) ReduceCPULimits(_ CgroupName) error {
	return nil
}

func (m *podContainerManagerNoop) GetAllPodsFromCgroups() (map[types.UID]CgroupName, error) {
	return nil, nil
}

func (m *podContainerManagerNoop) IsPodCgroup(cgroupfs string) (bool, types.UID) {
	return false, types.UID("")
}

func (m *podContainerManagerNoop) GetPodCgroupMemoryUsage(_ *v1.Pod) (uint64, error) {
	return 0, nil
}

func (m *podContainerManagerNoop) GetPodCgroupConfig(_ *v1.Pod, _ v1.ResourceName) (*ResourceConfig, error) {
	return nil, nil
}

func (m *podContainerManagerNoop) SetPodCgroupConfig(_ *v1.Pod, _ *ResourceConfig) error {
	return nil
}
