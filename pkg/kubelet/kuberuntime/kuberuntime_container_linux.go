//go:build linux
// +build linux

/*
Copyright 2018 The Kubernetes Authors.

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

package kuberuntime

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	cadvisorv1 "github.com/google/cadvisor/info/v1"
	libcontainercgroups "github.com/opencontainers/runc/libcontainer/cgroups"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	resourcehelper "k8s.io/component-helpers/resource"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
	"k8s.io/klog/v2"

	v1helper "k8s.io/kubernetes/pkg/apis/core/v1/helper"
	kubeapiqos "k8s.io/kubernetes/pkg/apis/core/v1/helper/qos"
	kubefeatures "k8s.io/kubernetes/pkg/features"
	"k8s.io/kubernetes/pkg/kubelet/cm"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	"k8s.io/kubernetes/pkg/kubelet/qos"
	kubelettypes "k8s.io/kubernetes/pkg/kubelet/types"
	"k8s.io/utils/ptr"
)

var defaultPageSize = int64(os.Getpagesize())

// applyPlatformSpecificContainerConfig applies platform specific configurations to runtimeapi.ContainerConfig.
func (m *kubeGenericRuntimeManager) applyPlatformSpecificContainerConfig(config *runtimeapi.ContainerConfig, container *v1.Container, pod *v1.Pod, uid *int64, username string, nsTarget *kubecontainer.ContainerID) error {
	enforceMemoryQoS := false
	// Set memory.min and memory.high if MemoryQoS enabled with cgroups v2
	if utilfeature.DefaultFeatureGate.Enabled(kubefeatures.MemoryQoS) &&
		isCgroup2UnifiedMode() {
		enforceMemoryQoS = true
	}
	cl, err := m.generateLinuxContainerConfig(container, pod, uid, username, nsTarget, enforceMemoryQoS)
	if err != nil {
		return err
	}
	config.Linux = cl

	if utilfeature.DefaultFeatureGate.Enabled(kubefeatures.UserNamespacesSupport) {
		if cl.SecurityContext.NamespaceOptions.UsernsOptions != nil {
			for _, mount := range config.Mounts {
				mount.UidMappings = cl.SecurityContext.NamespaceOptions.UsernsOptions.Uids
				mount.GidMappings = cl.SecurityContext.NamespaceOptions.UsernsOptions.Gids
			}
		}
	}
	return nil
}

// generateLinuxContainerConfig generates linux container config for kubelet runtime v1.
func (m *kubeGenericRuntimeManager) generateLinuxContainerConfig(container *v1.Container, pod *v1.Pod, uid *int64, username string, nsTarget *kubecontainer.ContainerID, enforceMemoryQoS bool) (*runtimeapi.LinuxContainerConfig, error) {
	sc, err := m.determineEffectiveSecurityContext(pod, container, uid, username)
	if err != nil {
		return nil, err
	}
	lc := &runtimeapi.LinuxContainerConfig{
		Resources:       m.generateLinuxContainerResources(pod, container, enforceMemoryQoS),
		SecurityContext: sc,
	}

	if nsTarget != nil && lc.SecurityContext.NamespaceOptions.Pid == runtimeapi.NamespaceMode_CONTAINER {
		lc.SecurityContext.NamespaceOptions.Pid = runtimeapi.NamespaceMode_TARGET
		lc.SecurityContext.NamespaceOptions.TargetId = nsTarget.ID
	}

	return lc, nil
}

// getCPULimit returns the memory limit for the container to be used to calculate
// Linux Container Resources.
func getCPULimit(pod *v1.Pod, container *v1.Container) *resource.Quantity {
	if utilfeature.DefaultFeatureGate.Enabled(kubefeatures.PodLevelResources) && resourcehelper.IsPodLevelResourcesSet(pod) {
		// When container-level CPU limit is not set, the pod-level
		// limit is used in the calculation for components relying on linux resource limits
		// to be set.
		if container.Resources.Limits.Cpu().IsZero() {
			return pod.Spec.Resources.Limits.Cpu()
		}
	}
	return container.Resources.Limits.Cpu()
}

// getMemoryLimit returns the memory limit for the container to be used to calculate
// Linux Container Resources.
func getMemoryLimit(pod *v1.Pod, container *v1.Container) *resource.Quantity {
	if utilfeature.DefaultFeatureGate.Enabled(kubefeatures.PodLevelResources) && resourcehelper.IsPodLevelResourcesSet(pod) {
		// When container-level memory limit is not set, the pod-level
		// limit is used in the calculation for components relying on linux resource limits
		// to be set.
		if container.Resources.Limits.Memory().IsZero() {
			return pod.Spec.Resources.Limits.Memory()
		}
	}
	return container.Resources.Limits.Memory()
}

// generateLinuxContainerResources generates linux container resources config for runtime
func (m *kubeGenericRuntimeManager) generateLinuxContainerResources(pod *v1.Pod, container *v1.Container, enforceMemoryQoS bool) *runtimeapi.LinuxContainerResources {
	// set linux container resources
	var cpuRequest *resource.Quantity
	if _, cpuRequestExists := container.Resources.Requests[v1.ResourceCPU]; cpuRequestExists {
		cpuRequest = container.Resources.Requests.Cpu()
	}

	memoryLimit := getMemoryLimit(pod, container)
	cpuLimit := getCPULimit(pod, container)
	lcr := m.calculateLinuxResources(cpuRequest, cpuLimit, memoryLimit)

	lcr.OomScoreAdj = int64(qos.GetContainerOOMScoreAdjust(pod, container,
		int64(m.machineInfo.MemoryCapacity)))

	lcr.HugepageLimits = GetHugepageLimitsFromResources(container.Resources)

	// Configure swap for the container
	m.configureContainerSwapResources(lcr, pod, container)

	// Set memory.min and memory.high to enforce MemoryQoS
	if enforceMemoryQoS {
		unified := map[string]string{}
		memoryRequest := container.Resources.Requests.Memory().Value()
		memoryLimit := container.Resources.Limits.Memory().Value()
		if memoryRequest != 0 {
			unified[cm.Cgroup2MemoryMin] = strconv.FormatInt(memoryRequest, 10)
		}

		// Guaranteed pods by their QoS definition requires that memory request equals memory limit and cpu request must equal cpu limit.
		// Here, we only check from memory perspective. Hence MemoryQoS feature is disabled on those QoS pods by not setting memory.high.
		if memoryRequest != memoryLimit {
			// The formula for memory.high for container cgroup is modified in Alpha stage of the feature in K8s v1.27.
			// It will be set based on formula:
			// `memory.high=floor[(requests.memory + memory throttling factor * (limits.memory or node allocatable memory - requests.memory))/pageSize] * pageSize`
			// where default value of memory throttling factor is set to 0.9
			// More info: https://git.k8s.io/enhancements/keps/sig-node/2570-memory-qos
			memoryHigh := int64(0)
			if memoryLimit != 0 {
				memoryHigh = int64(math.Floor(
					float64(memoryRequest)+
						(float64(memoryLimit)-float64(memoryRequest))*float64(m.memoryThrottlingFactor))/float64(defaultPageSize)) * defaultPageSize
			} else {
				allocatable := m.getNodeAllocatable()
				allocatableMemory, ok := allocatable[v1.ResourceMemory]
				if ok && allocatableMemory.Value() > 0 {
					memoryHigh = int64(math.Floor(
						float64(memoryRequest)+
							(float64(allocatableMemory.Value())-float64(memoryRequest))*float64(m.memoryThrottlingFactor))/float64(defaultPageSize)) * defaultPageSize
				}
			}
			if memoryHigh != 0 && memoryHigh > memoryRequest {
				unified[cm.Cgroup2MemoryHigh] = strconv.FormatInt(memoryHigh, 10)
			}
		}
		if len(unified) > 0 {
			if lcr.Unified == nil {
				lcr.Unified = unified
			} else {
				for k, v := range unified {
					lcr.Unified[k] = v
				}
			}
			klog.V(4).InfoS("MemoryQoS config for container", "pod", klog.KObj(pod), "containerName", container.Name, "unified", unified)
		}
	}

	return lcr
}

// configureContainerSwapResources configures the swap resources for a specified (linux) container.
// Swap is only configured if a swap cgroup controller is available and the NodeSwap feature gate is enabled.
func (m *kubeGenericRuntimeManager) configureContainerSwapResources(lcr *runtimeapi.LinuxContainerResources, pod *v1.Pod, container *v1.Container) {
	if !swapControllerAvailable() {
		return
	}

	swapConfigurationHelper := newSwapConfigurationHelper(*m.machineInfo)
	if m.memorySwapBehavior == kubelettypes.LimitedSwap {
		if !isCgroup2UnifiedMode() {
			swapConfigurationHelper.ConfigureNoSwap(lcr)
			return
		}
	}

	if !utilfeature.DefaultFeatureGate.Enabled(kubefeatures.NodeSwap) {
		swapConfigurationHelper.ConfigureNoSwap(lcr)
		return
	}

	if kubelettypes.IsCriticalPod(pod) {
		swapConfigurationHelper.ConfigureNoSwap(lcr)
		return
	}

	// NOTE(ehashman): Behavior is defined in the opencontainers runtime spec:
	// https://github.com/opencontainers/runtime-spec/blob/1c3f411f041711bbeecf35ff7e93461ea6789220/config-linux.md#memory
	switch m.memorySwapBehavior {
	case kubelettypes.NoSwap:
		swapConfigurationHelper.ConfigureNoSwap(lcr)
	case kubelettypes.LimitedSwap:
		swapConfigurationHelper.ConfigureLimitedSwap(lcr, pod, container)
	default:
		swapConfigurationHelper.ConfigureNoSwap(lcr)
	}
}

// generateContainerResources generates platform specific (linux) container resources config for runtime
func (m *kubeGenericRuntimeManager) generateContainerResources(pod *v1.Pod, container *v1.Container) *runtimeapi.ContainerResources {
	enforceMemoryQoS := false
	// Set memory.min and memory.high if MemoryQoS enabled with cgroups v2
	if utilfeature.DefaultFeatureGate.Enabled(kubefeatures.MemoryQoS) &&
		isCgroup2UnifiedMode() {
		enforceMemoryQoS = true
	}
	return &runtimeapi.ContainerResources{
		Linux: m.generateLinuxContainerResources(pod, container, enforceMemoryQoS),
	}
}

// generateUpdatePodSandboxResourcesRequest generates platform specific (linux) podsandox resources config for runtime
func (m *kubeGenericRuntimeManager) generateUpdatePodSandboxResourcesRequest(sandboxID string, pod *v1.Pod) *runtimeapi.UpdatePodSandboxResourcesRequest {
	return &runtimeapi.UpdatePodSandboxResourcesRequest{
		PodSandboxId: sandboxID,
		Overhead:     m.convertOverheadToLinuxResources(pod),
		Resources:    m.calculateSandboxResources(pod),
	}
}

// calculateLinuxResources will create the linuxContainerResources type based on the provided CPU and memory resource requests, limits
func (m *kubeGenericRuntimeManager) calculateLinuxResources(cpuRequest, cpuLimit, memoryLimit *resource.Quantity) *runtimeapi.LinuxContainerResources {
	resources := runtimeapi.LinuxContainerResources{}
	var cpuShares int64

	memLimit := memoryLimit.Value()

	// If request is not specified, but limit is, we want request to default to limit.
	// API server does this for new containers, but we repeat this logic in Kubelet
	// for containers running on existing Kubernetes clusters.
	if cpuRequest == nil && cpuLimit != nil {
		cpuShares = int64(cm.MilliCPUToShares(cpuLimit.MilliValue()))
	} else {
		// if cpuRequest.Amount is nil, then MilliCPUToShares will return the minimal number
		// of CPU shares.
		cpuShares = int64(cm.MilliCPUToShares(cpuRequest.MilliValue()))
	}
	resources.CpuShares = cpuShares
	if memLimit != 0 {
		resources.MemoryLimitInBytes = memLimit
	}

	if m.cpuCFSQuota {
		// if cpuLimit.Amount is nil, then the appropriate default value is returned
		// to allow full usage of cpu resource.
		cpuPeriod := int64(quotaPeriod)
		if utilfeature.DefaultFeatureGate.Enabled(kubefeatures.CPUCFSQuotaPeriod) {
			// kubeGenericRuntimeManager.cpuCFSQuotaPeriod is provided in time.Duration,
			// but we need to convert it to number of microseconds which is used by kernel.
			cpuPeriod = int64(m.cpuCFSQuotaPeriod.Duration / time.Microsecond)
		}
		cpuQuota := milliCPUToQuota(cpuLimit.MilliValue(), cpuPeriod)
		resources.CpuQuota = cpuQuota
		resources.CpuPeriod = cpuPeriod
	}

	// runc requires cgroupv2 for unified mode
	if isCgroup2UnifiedMode() && !ptr.Deref(m.singleProcessOOMKill, true) {
		resources.Unified = map[string]string{
			// Ask the kernel to kill all processes in the container cgroup in case of OOM.
			// See memory.oom.group in https://www.kernel.org/doc/html/latest/admin-guide/cgroup-v2.html for
			// more info.
			"memory.oom.group": "1",
		}
	}
	return &resources
}

// GetHugepageLimitsFromResources returns limits of each hugepages from resources.
func GetHugepageLimitsFromResources(resources v1.ResourceRequirements) []*runtimeapi.HugepageLimit {
	var hugepageLimits []*runtimeapi.HugepageLimit

	// For each page size, limit to 0.
	for _, pageSize := range libcontainercgroups.HugePageSizes() {
		hugepageLimits = append(hugepageLimits, &runtimeapi.HugepageLimit{
			PageSize: pageSize,
			Limit:    uint64(0),
		})
	}

	requiredHugepageLimits := map[string]uint64{}
	for resourceObj, amountObj := range resources.Limits {
		if !v1helper.IsHugePageResourceName(resourceObj) {
			continue
		}

		pageSize, err := v1helper.HugePageSizeFromResourceName(resourceObj)
		if err != nil {
			klog.InfoS("Failed to get hugepage size from resource", "object", resourceObj, "err", err)
			continue
		}

		sizeString, err := v1helper.HugePageUnitSizeFromByteSize(pageSize.Value())
		if err != nil {
			klog.InfoS("Size is invalid", "object", resourceObj, "err", err)
			continue
		}
		requiredHugepageLimits[sizeString] = uint64(amountObj.Value())
	}

	for _, hugepageLimit := range hugepageLimits {
		if limit, exists := requiredHugepageLimits[hugepageLimit.PageSize]; exists {
			hugepageLimit.Limit = limit
		}
	}

	return hugepageLimits
}

func toKubeContainerResources(statusResources *runtimeapi.ContainerResources) *kubecontainer.ContainerResources {
	var cStatusResources *kubecontainer.ContainerResources
	runtimeStatusResources := statusResources.GetLinux()
	if runtimeStatusResources != nil {
		var cpuLimit, memLimit, cpuRequest *resource.Quantity
		if runtimeStatusResources.CpuPeriod > 0 {
			milliCPU := quotaToMilliCPU(runtimeStatusResources.CpuQuota, runtimeStatusResources.CpuPeriod)
			if milliCPU > 0 {
				cpuLimit = resource.NewMilliQuantity(milliCPU, resource.DecimalSI)
			}
		}
		if runtimeStatusResources.CpuShares > 0 {
			milliCPU := sharesToMilliCPU(runtimeStatusResources.CpuShares)
			if milliCPU > 0 {
				cpuRequest = resource.NewMilliQuantity(milliCPU, resource.DecimalSI)
			}
		}
		if runtimeStatusResources.MemoryLimitInBytes > 0 {
			memLimit = resource.NewQuantity(runtimeStatusResources.MemoryLimitInBytes, resource.BinarySI)
		}
		if cpuLimit != nil || memLimit != nil || cpuRequest != nil {
			cStatusResources = &kubecontainer.ContainerResources{
				CPULimit:    cpuLimit,
				CPURequest:  cpuRequest,
				MemoryLimit: memLimit,
			}
		}
	}
	return cStatusResources
}

// Note: this function variable is being added here so it would be possible to mock
// the cgroup version for unit tests by assigning a new mocked function into it. Without it,
// the cgroup version would solely depend on the environment running the test.
var isCgroup2UnifiedMode = func() bool {
	return libcontainercgroups.IsCgroup2UnifiedMode()
}

// Note: this function variable is being added here so it would be possible to mock
// the swap controller availability for unit tests by assigning a new function to it. Without it,
// the swap controller availability would solely depend on the environment running the test.
var swapControllerAvailable = sync.OnceValue(func() bool {
	// See https://github.com/containerd/containerd/pull/7838/
	const warn = "Failed to detect the availability of the swap controller, assuming not available"
	p := "/sys/fs/cgroup/memory/memory.memsw.limit_in_bytes"
	if isCgroup2UnifiedMode() {
		// memory.swap.max does not exist in the cgroup root, so we check /sys/fs/cgroup/<SELF>/memory.swap.max
		cm, err := libcontainercgroups.ParseCgroupFile("/proc/self/cgroup")
		if err != nil {
			klog.V(5).ErrorS(fmt.Errorf("failed to parse /proc/self/cgroup: %w", err), warn)
			return false
		}
		// Fr cgroup v2 unified hierarchy, there are no per-controller
		// cgroup paths, so the cm map returned by ParseCgroupFile above
		// has a single element where the key is empty string ("") and
		// the value is the cgroup path the <pid> is in.
		p = filepath.Join("/sys/fs/cgroup", cm[""], "memory.swap.max")
	}
	if _, err := os.Stat(p); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			klog.V(5).ErrorS(err, warn)
		}
		return false
	}

	return true
})

type swapConfigurationHelper struct {
	machineInfo cadvisorv1.MachineInfo
}

func newSwapConfigurationHelper(machineInfo cadvisorv1.MachineInfo) *swapConfigurationHelper {
	return &swapConfigurationHelper{machineInfo: machineInfo}
}

func (m swapConfigurationHelper) ConfigureLimitedSwap(lcr *runtimeapi.LinuxContainerResources, pod *v1.Pod, container *v1.Container) {
	podQos := kubeapiqos.GetPodQOS(pod)
	containerDoesNotRequestMemory := container.Resources.Requests.Memory().IsZero() && container.Resources.Limits.Memory().IsZero()
	memoryRequestEqualsToLimit := container.Resources.Requests.Memory().Cmp(*container.Resources.Limits.Memory()) == 0

	if podQos != v1.PodQOSBurstable || containerDoesNotRequestMemory || !isCgroup2UnifiedMode() || memoryRequestEqualsToLimit {
		m.ConfigureNoSwap(lcr)
		return
	}

	containerMemoryRequest := container.Resources.Requests.Memory()
	swapLimit, err := calcSwapForBurstablePods(containerMemoryRequest.Value(), int64(m.machineInfo.MemoryCapacity), int64(m.machineInfo.SwapCapacity))
	if err != nil {
		klog.ErrorS(err, "cannot calculate swap allocation amount; disallowing swap")
		m.ConfigureNoSwap(lcr)
		return
	}

	m.configureSwap(lcr, swapLimit)
}

func (m swapConfigurationHelper) ConfigureNoSwap(lcr *runtimeapi.LinuxContainerResources) {
	if !isCgroup2UnifiedMode() {
		if swapControllerAvailable() {
			// memorySwapLimit = total permitted memory+swap; if equal to memory limit, => 0 swap above memory limit
			// Some swapping is still possible.
			// Note that if memory limit is 0, memory swap limit is ignored.
			lcr.MemorySwapLimitInBytes = lcr.MemoryLimitInBytes
		}
		return
	}

	m.configureSwap(lcr, 0)
}

func (m swapConfigurationHelper) configureSwap(lcr *runtimeapi.LinuxContainerResources, swapMemory int64) {
	if !isCgroup2UnifiedMode() {
		klog.ErrorS(fmt.Errorf("swap configuration is not supported with cgroup v1"), "swap configuration under cgroup v1 is unexpected")
		return
	}

	if lcr.Unified == nil {
		lcr.Unified = map[string]string{}
	}

	lcr.Unified[cm.Cgroup2MaxSwapFilename] = fmt.Sprintf("%d", swapMemory)
}

// The swap limit is calculated as (<containerMemoryRequest>/<nodeTotalMemory>)*<totalPodsSwapAvailable>.
// For more info, please look at the following KEP: https://kep.k8s.io/2400
func calcSwapForBurstablePods(containerMemoryRequest, nodeTotalMemory, totalPodsSwapAvailable int64) (int64, error) {
	if nodeTotalMemory <= 0 {
		return 0, fmt.Errorf("total node memory is 0")
	}
	if containerMemoryRequest > nodeTotalMemory {
		return 0, fmt.Errorf("container request %d is larger than total node memory %d", containerMemoryRequest, nodeTotalMemory)
	}

	containerMemoryProportion := float64(containerMemoryRequest) / float64(nodeTotalMemory)
	swapAllocation := containerMemoryProportion * float64(totalPodsSwapAvailable)

	return int64(swapAllocation), nil
}

func toKubeContainerUser(statusUser *runtimeapi.ContainerUser) *kubecontainer.ContainerUser {
	if statusUser == nil {
		return nil
	}

	user := &kubecontainer.ContainerUser{}
	if statusUser.GetLinux() != nil {
		user.Linux = &kubecontainer.LinuxContainerUser{
			UID:                statusUser.GetLinux().GetUid(),
			GID:                statusUser.GetLinux().GetGid(),
			SupplementalGroups: statusUser.GetLinux().GetSupplementalGroups(),
		}
	}

	return user
}
