//go:build windows
// +build windows

/*
Copyright 2015 The Kubernetes Authors.

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

// containerManagerImpl implements container manager on Windows.
// Only GetNodeAllocatableReservation() and GetCapacity() are implemented now.

package cm

import (
	"context"
	"fmt"
	"sync"

	utilfeature "k8s.io/apiserver/pkg/util/feature"
	kubefeatures "k8s.io/kubernetes/pkg/features"
	"k8s.io/kubernetes/pkg/kubelet/cm/memorymanager"

	"k8s.io/klog/v2"
	"k8s.io/mount-utils"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apiserver/pkg/server/healthz"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/record"
	internalapi "k8s.io/cri-api/pkg/apis"
	pluginwatcherapi "k8s.io/kubelet/pkg/apis/pluginregistration/v1"
	podresourcesapi "k8s.io/kubelet/pkg/apis/podresources/v1"
	"k8s.io/kubernetes/pkg/kubelet/cadvisor"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpumanager"
	"k8s.io/kubernetes/pkg/kubelet/cm/devicemanager"
	"k8s.io/kubernetes/pkg/kubelet/cm/resourceupdates"
	"k8s.io/kubernetes/pkg/kubelet/cm/topologymanager"
	"k8s.io/kubernetes/pkg/kubelet/config"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	"k8s.io/kubernetes/pkg/kubelet/lifecycle"
	"k8s.io/kubernetes/pkg/kubelet/pluginmanager/cache"
	"k8s.io/kubernetes/pkg/kubelet/status"
	schedulerframework "k8s.io/kubernetes/pkg/scheduler/framework"
)

type containerManagerImpl struct {
	// Capacity of this node.
	capacity v1.ResourceList
	// Interface for cadvisor.
	cadvisorInterface cadvisor.Interface
	// Config of this node.
	nodeConfig NodeConfig
	// Interface for exporting and allocating devices reported by device plugins.
	deviceManager devicemanager.Manager
	// Interface for Topology resource co-ordination
	topologyManager topologymanager.Manager
	cpuManager      cpumanager.Manager
	memoryManager   memorymanager.Manager
	nodeInfo        *v1.Node
	sync.RWMutex
}

func (cm *containerManagerImpl) Start(ctx context.Context, node *v1.Node,
	activePods ActivePodsFunc,
	getNode GetNodeFunc,
	sourcesReady config.SourcesReady,
	podStatusProvider status.PodStatusProvider,
	runtimeService internalapi.RuntimeService,
	localStorageCapacityIsolation bool) error {
	klog.V(2).InfoS("Starting Windows container manager")

	cm.nodeInfo = node

	if localStorageCapacityIsolation {
		rootfs, err := cm.cadvisorInterface.RootFsInfo()
		if err != nil {
			return fmt.Errorf("failed to get rootfs info: %v", err)
		}
		for rName, rCap := range cadvisor.EphemeralStorageCapacityFromFsInfo(rootfs) {
			cm.capacity[rName] = rCap
		}
	}

	containerMap, containerRunningSet := buildContainerMapAndRunningSetFromRuntime(ctx, runtimeService)

	if utilfeature.DefaultFeatureGate.Enabled(kubefeatures.WindowsCPUAndMemoryAffinity) {
		err := cm.cpuManager.Start(cpumanager.ActivePodsFunc(activePods), sourcesReady, podStatusProvider, runtimeService, containerMap.Clone())
		if err != nil {
			return fmt.Errorf("start cpu manager error: %v", err)
		}

		// Initialize memory manager
		err = cm.memoryManager.Start(memorymanager.ActivePodsFunc(activePods), sourcesReady, podStatusProvider, runtimeService, containerMap.Clone())
		if err != nil {
			return fmt.Errorf("start memory manager error: %v", err)
		}
	}

	// Starts device manager.
	if err := cm.deviceManager.Start(devicemanager.ActivePodsFunc(activePods), sourcesReady, containerMap.Clone(), containerRunningSet); err != nil {
		return err
	}

	return nil
}

// NewContainerManager creates windows container manager.
func NewContainerManager(mountUtil mount.Interface, cadvisorInterface cadvisor.Interface, nodeConfig NodeConfig, failSwapOn bool, recorder record.EventRecorder, kubeClient clientset.Interface) (ContainerManager, error) {
	// It is safe to invoke `MachineInfo` on cAdvisor before logically initializing cAdvisor here because
	// machine info is computed and cached once as part of cAdvisor object creation.
	// But `RootFsInfo` and `ImagesFsInfo` are not available at this moment so they will be called later during manager starts
	machineInfo, err := cadvisorInterface.MachineInfo()
	if err != nil {
		return nil, err
	}
	capacity := cadvisor.CapacityFromMachineInfo(machineInfo)

	cm := &containerManagerImpl{
		capacity:          capacity,
		nodeConfig:        nodeConfig,
		cadvisorInterface: cadvisorInterface,
	}

	cm.topologyManager = topologymanager.NewFakeManager()
	cm.cpuManager = cpumanager.NewFakeManager()
	cm.memoryManager = memorymanager.NewFakeManager()

	if utilfeature.DefaultFeatureGate.Enabled(kubefeatures.WindowsCPUAndMemoryAffinity) {
		klog.InfoS("Creating topology manager")
		cm.topologyManager, err = topologymanager.NewManager(machineInfo.Topology,
			nodeConfig.TopologyManagerPolicy,
			nodeConfig.TopologyManagerScope,
			nodeConfig.TopologyManagerPolicyOptions)
		if err != nil {
			klog.ErrorS(err, "Failed to initialize topology manager")
			return nil, err
		}

		klog.InfoS("Creating cpu manager")
		cm.cpuManager, err = cpumanager.NewManager(
			nodeConfig.CPUManagerPolicy,
			nodeConfig.CPUManagerPolicyOptions,
			nodeConfig.CPUManagerReconcilePeriod,
			machineInfo,
			nodeConfig.NodeAllocatableConfig.ReservedSystemCPUs,
			cm.GetNodeAllocatableReservation(),
			nodeConfig.KubeletRootDir,
			cm.topologyManager,
		)
		if err != nil {
			klog.ErrorS(err, "Failed to initialize cpu manager")
			return nil, err
		}
		cm.topologyManager.AddHintProvider(cm.cpuManager)

		klog.InfoS("Creating memory manager")
		cm.memoryManager, err = memorymanager.NewManager(
			nodeConfig.MemoryManagerPolicy,
			machineInfo,
			cm.GetNodeAllocatableReservation(),
			nodeConfig.MemoryManagerReservedMemory,
			nodeConfig.KubeletRootDir,
			cm.topologyManager,
		)
		if err != nil {
			klog.ErrorS(err, "Failed to initialize memory manager")
			return nil, err
		}
		cm.topologyManager.AddHintProvider(cm.memoryManager)
	}

	klog.InfoS("Creating device plugin manager")
	cm.deviceManager, err = devicemanager.NewManagerImpl(nil, cm.topologyManager)
	if err != nil {
		return nil, err
	}
	cm.topologyManager.AddHintProvider(cm.deviceManager)

	return cm, nil
}

func (cm *containerManagerImpl) SystemCgroupsLimit() v1.ResourceList {
	return v1.ResourceList{}
}

func (cm *containerManagerImpl) GetNodeConfig() NodeConfig {
	cm.RLock()
	defer cm.RUnlock()
	return cm.nodeConfig
}

func (cm *containerManagerImpl) GetMountedSubsystems() *CgroupSubsystems {
	return &CgroupSubsystems{}
}

func (cm *containerManagerImpl) GetQOSContainersInfo() QOSContainersInfo {
	return QOSContainersInfo{}
}

func (cm *containerManagerImpl) UpdateQOSCgroups() error {
	return nil
}

func (cm *containerManagerImpl) Status() Status {
	return Status{}
}

func (cm *containerManagerImpl) GetNodeAllocatableReservation() v1.ResourceList {
	evictionReservation := hardEvictionReservation(cm.nodeConfig.HardEvictionThresholds, cm.capacity)
	result := make(v1.ResourceList)
	for k := range cm.capacity {
		value := resource.NewQuantity(0, resource.DecimalSI)
		if cm.nodeConfig.SystemReserved != nil {
			value.Add(cm.nodeConfig.SystemReserved[k])
		}
		if cm.nodeConfig.KubeReserved != nil {
			value.Add(cm.nodeConfig.KubeReserved[k])
		}
		if evictionReservation != nil {
			value.Add(evictionReservation[k])
		}
		if !value.IsZero() {
			result[k] = *value
		}
	}
	return result
}

func (cm *containerManagerImpl) GetCapacity(localStorageCapacityIsolation bool) v1.ResourceList {
	return cm.capacity
}

func (cm *containerManagerImpl) GetPluginRegistrationHandlers() map[string]cache.PluginHandler {
	// DRA is not supported on Windows, only device plugin is supported
	return map[string]cache.PluginHandler{pluginwatcherapi.DevicePlugin: cm.deviceManager.GetWatcherHandler()}
}

func (cm *containerManagerImpl) GetHealthCheckers() []healthz.HealthChecker {
	return []healthz.HealthChecker{cm.deviceManager.GetHealthChecker()}
}

func (cm *containerManagerImpl) GetDevicePluginResourceCapacity() (v1.ResourceList, v1.ResourceList, []string) {
	return cm.deviceManager.GetCapacity()
}

func (cm *containerManagerImpl) NewPodContainerManager() PodContainerManager {
	return &podContainerManagerStub{}
}

func (cm *containerManagerImpl) GetResources(ctx context.Context, pod *v1.Pod, container *v1.Container) (*kubecontainer.RunContainerOptions, error) {
	opts := &kubecontainer.RunContainerOptions{}
	// Allocate should already be called during predicateAdmitHandler.Admit(),
	// just try to fetch device runtime information from cached state here
	devOpts, err := cm.deviceManager.GetDeviceRunContainerOptions(pod, container)
	if err != nil {
		return nil, err
	} else if devOpts == nil {
		return opts, nil
	}
	opts.Devices = append(opts.Devices, devOpts.Devices...)
	opts.Mounts = append(opts.Mounts, devOpts.Mounts...)
	opts.Envs = append(opts.Envs, devOpts.Envs...)
	opts.Annotations = append(opts.Annotations, devOpts.Annotations...)
	return opts, nil
}

func (cm *containerManagerImpl) UpdateAllocatedResourcesStatus(pod *v1.Pod, status *v1.PodStatus) {
	// For now we only support Device Plugin

	cm.deviceManager.UpdateAllocatedResourcesStatus(pod, status)

	// TODO(SergeyKanzhelev, https://kep.k8s.io/4680): add support for DRA resources when DRA supports Windows
}

func (cm *containerManagerImpl) Updates() <-chan resourceupdates.Update {
	// TODO(SergeyKanzhelev, https://kep.k8s.io/4680): add support for DRA resources, for now only use device plugin updates
	return cm.deviceManager.Updates()
}

func (cm *containerManagerImpl) UpdatePluginResources(node *schedulerframework.NodeInfo, attrs *lifecycle.PodAdmitAttributes) error {
	return cm.deviceManager.UpdatePluginResources(node, attrs)
}

func (cm *containerManagerImpl) InternalContainerLifecycle() InternalContainerLifecycle {
	return &internalContainerLifecycleImpl{cm.cpuManager, cm.memoryManager, cm.topologyManager}
}

func (cm *containerManagerImpl) GetPodCgroupRoot() string {
	return ""
}

func (cm *containerManagerImpl) GetDevices(podUID, containerName string) []*podresourcesapi.ContainerDevices {
	return containerDevicesFromResourceDeviceInstances(cm.deviceManager.GetDevices(podUID, containerName))
}

func (cm *containerManagerImpl) GetAllocatableDevices() []*podresourcesapi.ContainerDevices {
	return nil
}

func (cm *containerManagerImpl) ShouldResetExtendedResourceCapacity() bool {
	return cm.deviceManager.ShouldResetExtendedResourceCapacity()
}

func (cm *containerManagerImpl) GetAllocateResourcesPodAdmitHandler() lifecycle.PodAdmitHandler {
	return cm.topologyManager
}

func (cm *containerManagerImpl) UpdateAllocatedDevices() {
	return
}

func (cm *containerManagerImpl) GetCPUs(podUID, containerName string) []int64 {
	if utilfeature.DefaultFeatureGate.Enabled(kubefeatures.WindowsCPUAndMemoryAffinity) {
		if cm.cpuManager != nil {
			return int64Slice(cm.cpuManager.GetExclusiveCPUs(podUID, containerName).UnsortedList())
		}
		return []int64{}
	}
	return nil
}

func (cm *containerManagerImpl) GetAllocatableCPUs() []int64 {
	if utilfeature.DefaultFeatureGate.Enabled(kubefeatures.WindowsCPUAndMemoryAffinity) {
		if cm.cpuManager != nil {
			return int64Slice(cm.cpuManager.GetAllocatableCPUs().UnsortedList())
		}
		return []int64{}
	}
	return nil
}

func (cm *containerManagerImpl) GetMemory(_, _ string) []*podresourcesapi.ContainerMemory {
	return nil
}

func (cm *containerManagerImpl) GetAllocatableMemory() []*podresourcesapi.ContainerMemory {
	return nil
}

func (cm *containerManagerImpl) GetNodeAllocatableAbsolute() v1.ResourceList {
	return nil
}

func (cm *containerManagerImpl) GetDynamicResources(pod *v1.Pod, container *v1.Container) []*podresourcesapi.DynamicResource {
	return nil
}

func (cm *containerManagerImpl) PrepareDynamicResources(ctx context.Context, pod *v1.Pod) error {
	return nil
}

func (cm *containerManagerImpl) UnprepareDynamicResources(ctx context.Context, pod *v1.Pod) error {
	return nil
}

func (cm *containerManagerImpl) PodMightNeedToUnprepareResources(UID types.UID) bool {
	return false
}

func (cm *containerManagerImpl) PodHasExclusiveCPUs(pod *v1.Pod) bool {
	return podHasExclusiveCPUs(cm.cpuManager, pod)
}

func (cm *containerManagerImpl) ContainerHasExclusiveCPUs(pod *v1.Pod, container *v1.Container) bool {
	return containerHasExclusiveCPUs(cm.cpuManager, pod, container)
}
