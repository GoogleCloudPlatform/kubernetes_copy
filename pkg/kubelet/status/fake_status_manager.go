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

package status

import (
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/klog/v2"
	podutil "k8s.io/kubernetes/pkg/api/v1/pod"
	"k8s.io/kubernetes/pkg/features"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	"k8s.io/kubernetes/pkg/kubelet/status/state"
)

type fakeManager struct {
	state state.State
}

func (m *fakeManager) Start() {
	klog.InfoS("Start()")
	return
}

func (m *fakeManager) GetPodStatus(uid types.UID) (v1.PodStatus, bool) {
	klog.InfoS("GetPodStatus()")
	return v1.PodStatus{}, false
}

func (m *fakeManager) SetPodStatus(pod *v1.Pod, status v1.PodStatus) {
	klog.InfoS("SetPodStatus()")
	return
}

func (m *fakeManager) SetContainerReadiness(podUID types.UID, containerID kubecontainer.ContainerID, ready bool) {
	klog.InfoS("SetContainerReadiness()")
	return
}

func (m *fakeManager) SetContainerStartup(podUID types.UID, containerID kubecontainer.ContainerID, started bool) {
	klog.InfoS("SetContainerStartup()")
	return
}

func (m *fakeManager) TerminatePod(pod *v1.Pod) {
	klog.InfoS("TerminatePod()")
	return
}

func (m *fakeManager) RemoveOrphanedStatuses(podUIDs map[types.UID]bool) {
	klog.InfoS("RemoveOrphanedStatuses()")
	return
}

func (m *fakeManager) GetContainerResourceAllocation(podUID string, containerName string) (v1.ResourceRequirements, bool) {
	klog.InfoS("GetContainerResourceAllocation()")
	return m.state.GetContainerResourceAllocation(podUID, containerName)
}

func (m *fakeManager) GetPodResizeStatus(podUID types.UID) v1.PodResizeStatus {
	return m.state.GetPodResizeStatus(string(podUID))
}

func (m *fakeManager) UpdatePodFromAllocation(pod *v1.Pod) (*v1.Pod, bool) {
	allocs := m.state.GetPodResourceAllocation()
	return updatePodFromAllocation(pod, allocs)
}

func (m *fakeManager) SetPodAllocation(pod *v1.Pod) error {
	klog.InfoS("SetPodAllocation()")
	for _, container := range pod.Spec.Containers {
		alloc := *container.Resources.DeepCopy()
		if err := m.state.SetContainerResourceAllocation(string(pod.UID), container.Name, alloc); err != nil {
			return err
		}
	}
	if utilfeature.DefaultFeatureGate.Enabled(features.SidecarContainers) {
		for _, container := range pod.Spec.InitContainers {
			if podutil.IsRestartableInitContainer(&container) {
				alloc := *container.Resources.DeepCopy()
				if err := m.state.SetContainerResourceAllocation(string(pod.UID), container.Name, alloc); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (m *fakeManager) SetPodResizeStatus(podUID types.UID, resizeStatus v1.PodResizeStatus) {
	m.state.SetPodResizeStatus(string(podUID), resizeStatus)
}

// NewFakeManager creates empty/fake memory manager
func NewFakeManager() Manager {
	return &fakeManager{
		state: state.NewStateMemory(state.PodResourceAllocation{}),
	}
}
