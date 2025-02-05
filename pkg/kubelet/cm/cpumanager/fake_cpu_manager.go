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

package cpumanager

import (
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/kubelet/cm/containermap"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpumanager/state"
	"k8s.io/kubernetes/pkg/kubelet/cm/topologymanager"
	"k8s.io/kubernetes/pkg/kubelet/config"
	"k8s.io/kubernetes/pkg/kubelet/status"
	"k8s.io/utils/cpuset"
)

type fakeManager struct {
	state state.State
}

func (m *fakeManager) Start(activePods ActivePodsFunc, sourcesReady config.SourcesReady, podStatusProvider status.PodStatusProvider, containerRuntime runtimeService, initialContainers containermap.ContainerMap) error {
	klog.InfoS("Start()")
	return nil
}

func (m *fakeManager) Policy() Policy {
	klog.InfoS("Policy()")
	pol, _ := NewNonePolicy(nil)
	return pol
}

func (m *fakeManager) Allocate(pod *v1.Pod, container *v1.Container) error {
	klog.InfoS("Allocate", "pod", klog.KObj(pod), "containerName", container.Name)
	return nil
}

func (m *fakeManager) AddContainer(pod *v1.Pod, container *v1.Container, containerID string) {
	klog.InfoS("AddContainer", "pod", klog.KObj(pod), "containerName", container.Name, "containerID", containerID)
}

func (m *fakeManager) RemoveContainer(containerID string) error {
	klog.InfoS("RemoveContainer", "containerID", containerID)
	return nil
}

func (m *fakeManager) GetTopologyHints(pod *v1.Pod, container *v1.Container) map[string][]topologymanager.TopologyHint {
	klog.InfoS("Get container topology hints")
	return map[string][]topologymanager.TopologyHint{}
}

func (m *fakeManager) GetPodTopologyHints(pod *v1.Pod) map[string][]topologymanager.TopologyHint {
	klog.InfoS("Get pod topology hints")
	return map[string][]topologymanager.TopologyHint{}
}

func (m *fakeManager) State() state.Reader {
	return m.state
}

func (m *fakeManager) GetExclusiveCPUs(podUID, containerName string) cpuset.CPUSet {
	klog.InfoS("GetExclusiveCPUs", "podUID", podUID, "containerName", containerName)
	return cpuset.CPUSet{}
}

func (m *fakeManager) GetAllocatableCPUs() cpuset.CPUSet {
	klog.InfoS("Get Allocatable CPUs")
	return cpuset.CPUSet{}
}

func (m *fakeManager) GetCPUAffinity(podUID, containerName string) cpuset.CPUSet {
	klog.InfoS("GetCPUAffinity", "podUID", podUID, "containerName", containerName)
	return cpuset.CPUSet{}
}

func (m *fakeManager) GetAllCPUs() cpuset.CPUSet {
	klog.InfoS("GetAllCPUs")
	return cpuset.CPUSet{}
}

func (m *fakeManager) CanAllocateExclusively(res v1.ResourceName) bool {
	return false
}

// NewFakeManager creates empty/fake cpu manager
func NewFakeManager() Manager {
	return &fakeManager{
		state: state.NewMemoryState(),
	}
}
