/*
Copyright 2021 The Kubernetes Authors.

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
	"context"
	"sync"

	v1 "k8s.io/api/core/v1"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apiserver/pkg/server/healthz"
	internalapi "k8s.io/cri-api/pkg/apis"
	podresourcesapi "k8s.io/kubelet/pkg/apis/podresources/v1"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpumanager"
	"k8s.io/kubernetes/pkg/kubelet/cm/memorymanager"
	"k8s.io/kubernetes/pkg/kubelet/cm/resourceupdates"
	"k8s.io/kubernetes/pkg/kubelet/cm/topologymanager"
	"k8s.io/kubernetes/pkg/kubelet/config"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	"k8s.io/kubernetes/pkg/kubelet/lifecycle"
	"k8s.io/kubernetes/pkg/kubelet/pluginmanager/cache"
	"k8s.io/kubernetes/pkg/kubelet/status"
	schedulerframework "k8s.io/kubernetes/pkg/scheduler/framework"
)

type FakeContainerManager struct {
	sync.Mutex
	CalledFunctions                     []string
	PodContainerManager                 *FakePodContainerManager
	shouldResetExtendedResourceCapacity bool
}

var _ ContainerManager = &FakeContainerManager{}

func NewFakeContainerManager() *FakeContainerManager {
	return &FakeContainerManager{
		PodContainerManager: NewFakePodContainerManager(),
	}
}

func (cm *FakeContainerManager) Start(_ context.Context, _ *v1.Node, _ ActivePodsFunc, _ GetNodeFunc, _ config.SourcesReady, _ status.PodStatusProvider, _ internalapi.RuntimeService, _ bool) error {
	cm.Lock()
	defer cm.Unlock()
	cm.CalledFunctions = append(cm.CalledFunctions, "Start")
	return nil
}

func (cm *FakeContainerManager) SystemCgroupsLimit() v1.ResourceList {
	cm.Lock()
	defer cm.Unlock()
	cm.CalledFunctions = append(cm.CalledFunctions, "SystemCgroupsLimit")
	return v1.ResourceList{}
}

func (cm *FakeContainerManager) GetNodeConfig() NodeConfig {
	cm.Lock()
	defer cm.Unlock()
	cm.CalledFunctions = append(cm.CalledFunctions, "GetNodeConfig")
	return NodeConfig{}
}

func (cm *FakeContainerManager) GetMountedSubsystems() *CgroupSubsystems {
	cm.Lock()
	defer cm.Unlock()
	cm.CalledFunctions = append(cm.CalledFunctions, "GetMountedSubsystems")
	return &CgroupSubsystems{}
}

func (cm *FakeContainerManager) GetQOSContainersInfo() QOSContainersInfo {
	cm.Lock()
	defer cm.Unlock()
	cm.CalledFunctions = append(cm.CalledFunctions, "QOSContainersInfo")
	return QOSContainersInfo{}
}

func (cm *FakeContainerManager) UpdateQOSCgroups() error {
	cm.Lock()
	defer cm.Unlock()
	cm.CalledFunctions = append(cm.CalledFunctions, "UpdateQOSCgroups")
	return nil
}

func (cm *FakeContainerManager) Status() Status {
	cm.Lock()
	defer cm.Unlock()
	cm.CalledFunctions = append(cm.CalledFunctions, "Status")
	return Status{}
}

func (cm *FakeContainerManager) GetNodeAllocatableReservation() v1.ResourceList {
	cm.Lock()
	defer cm.Unlock()
	cm.CalledFunctions = append(cm.CalledFunctions, "GetNodeAllocatableReservation")
	return nil
}

func (cm *FakeContainerManager) GetCapacity(localStorageCapacityIsolation bool) v1.ResourceList {
	cm.Lock()
	defer cm.Unlock()
	cm.CalledFunctions = append(cm.CalledFunctions, "GetCapacity")
	if !localStorageCapacityIsolation {
		return v1.ResourceList{}
	}
	c := v1.ResourceList{
		v1.ResourceEphemeralStorage: *resource.NewQuantity(
			int64(0),
			resource.BinarySI),
	}
	return c
}

func (cm *FakeContainerManager) GetPluginRegistrationHandlers() map[string]cache.PluginHandler {
	cm.Lock()
	defer cm.Unlock()
	cm.CalledFunctions = append(cm.CalledFunctions, "GetPluginRegistrationHandlers")
	return nil
}

func (cm *FakeContainerManager) GetHealthCheckers() []healthz.HealthChecker {
	cm.Lock()
	defer cm.Unlock()
	cm.CalledFunctions = append(cm.CalledFunctions, "GetPluginRegistrationServerChecker")
	return []healthz.HealthChecker{}
}

func (cm *FakeContainerManager) GetDevicePluginResourceCapacity() (v1.ResourceList, v1.ResourceList, []string) {
	cm.Lock()
	defer cm.Unlock()
	cm.CalledFunctions = append(cm.CalledFunctions, "GetDevicePluginResourceCapacity")
	return nil, nil, []string{}
}

func (cm *FakeContainerManager) NewPodContainerManager() PodContainerManager {
	cm.Lock()
	defer cm.Unlock()
	cm.CalledFunctions = append(cm.CalledFunctions, "PodContainerManager")
	return cm.PodContainerManager
}

func (cm *FakeContainerManager) GetResources(ctx context.Context, pod *v1.Pod, container *v1.Container) (*kubecontainer.RunContainerOptions, error) {
	cm.Lock()
	defer cm.Unlock()
	cm.CalledFunctions = append(cm.CalledFunctions, "GetResources")
	return &kubecontainer.RunContainerOptions{}, nil
}

func (cm *FakeContainerManager) UpdatePluginResources(*schedulerframework.NodeInfo, *lifecycle.PodAdmitAttributes) error {
	cm.Lock()
	defer cm.Unlock()
	cm.CalledFunctions = append(cm.CalledFunctions, "UpdatePluginResources")
	return nil
}

func (cm *FakeContainerManager) InternalContainerLifecycle() InternalContainerLifecycle {
	cm.Lock()
	defer cm.Unlock()
	cm.CalledFunctions = append(cm.CalledFunctions, "InternalContainerLifecycle")
	return &internalContainerLifecycleImpl{cpumanager.NewFakeManager(), memorymanager.NewFakeManager(), topologymanager.NewFakeManager()}
}

func (cm *FakeContainerManager) GetPodCgroupRoot() string {
	cm.Lock()
	defer cm.Unlock()
	cm.CalledFunctions = append(cm.CalledFunctions, "GetPodCgroupRoot")
	return ""
}

func (cm *FakeContainerManager) GetDevices(_, _ string) []*podresourcesapi.ContainerDevices {
	cm.Lock()
	defer cm.Unlock()
	cm.CalledFunctions = append(cm.CalledFunctions, "GetDevices")
	return nil
}

func (cm *FakeContainerManager) GetAllocatableDevices() []*podresourcesapi.ContainerDevices {
	cm.Lock()
	defer cm.Unlock()
	cm.CalledFunctions = append(cm.CalledFunctions, "GetAllocatableDevices")
	return nil
}

func (cm *FakeContainerManager) ShouldResetExtendedResourceCapacity() bool {
	cm.Lock()
	defer cm.Unlock()
	cm.CalledFunctions = append(cm.CalledFunctions, "ShouldResetExtendedResourceCapacity")
	return cm.shouldResetExtendedResourceCapacity
}

func (cm *FakeContainerManager) GetAllocateResourcesPodAdmitHandler() lifecycle.PodAdmitHandler {
	cm.Lock()
	defer cm.Unlock()
	cm.CalledFunctions = append(cm.CalledFunctions, "GetAllocateResourcesPodAdmitHandler")
	return topologymanager.NewFakeManager()
}

func (cm *FakeContainerManager) UpdateAllocatedDevices() {
	cm.Lock()
	defer cm.Unlock()
	cm.CalledFunctions = append(cm.CalledFunctions, "UpdateAllocatedDevices")
	return
}

func (cm *FakeContainerManager) GetCPUs(_, _ string) []int64 {
	cm.Lock()
	defer cm.Unlock()
	cm.CalledFunctions = append(cm.CalledFunctions, "GetCPUs")
	return nil
}

func (cm *FakeContainerManager) GetAllocatableCPUs() []int64 {
	cm.Lock()
	defer cm.Unlock()
	return nil
}

func (cm *FakeContainerManager) GetMemory(_, _ string) []*podresourcesapi.ContainerMemory {
	cm.Lock()
	defer cm.Unlock()
	cm.CalledFunctions = append(cm.CalledFunctions, "GetMemory")
	return nil
}

func (cm *FakeContainerManager) GetAllocatableMemory() []*podresourcesapi.ContainerMemory {
	cm.Lock()
	defer cm.Unlock()
	return nil
}

func (cm *FakeContainerManager) GetDynamicResources(pod *v1.Pod, container *v1.Container) []*podresourcesapi.DynamicResource {
	return nil
}

func (cm *FakeContainerManager) GetNodeAllocatableAbsolute() v1.ResourceList {
	cm.Lock()
	defer cm.Unlock()
	return nil
}

func (cm *FakeContainerManager) PrepareDynamicResources(ctx context.Context, pod *v1.Pod) error {
	return nil
}

func (cm *FakeContainerManager) UnprepareDynamicResources(context.Context, *v1.Pod) error {
	return nil
}

func (cm *FakeContainerManager) PodMightNeedToUnprepareResources(UID types.UID) bool {
	return false
}
func (cm *FakeContainerManager) UpdateAllocatedResourcesStatus(pod *v1.Pod, status *v1.PodStatus) {
}
func (cm *FakeContainerManager) Updates() <-chan resourceupdates.Update {
	return nil
}

func (cm *FakeContainerManager) PodHasExclusiveCPUs(pod *v1.Pod) bool {
	return false
}

func (cm *FakeContainerManager) ContainerHasExclusiveCPUs(pod *v1.Pod, container *v1.Container) bool {
	return false
}
