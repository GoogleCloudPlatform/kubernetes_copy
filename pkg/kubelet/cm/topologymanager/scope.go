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

package topologymanager

import (
	"sync"

	"k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/kubelet/cm/admission"
	"k8s.io/kubernetes/pkg/kubelet/cm/containermap"
	"k8s.io/kubernetes/pkg/kubelet/lifecycle"
)

const (
	// containerTopologyScope specifies the TopologyManagerScope per container.
	containerTopologyScope = "container"
	// podTopologyScope specifies the TopologyManagerScope per pod.
	podTopologyScope = "pod"
	// noneTopologyScope specifies the TopologyManagerScope when topologyPolicyName is none.
	noneTopologyScope = "none"
)

type podTopologyHints map[string]map[string]TopologyHint

// Scope interface for Topology Manager
type Scope interface {
	Name() string
	GetPolicy() Policy
	Admit(pod *v1.Pod) lifecycle.PodAdmitResult
	// AddHintProvider adds a hint provider to manager to indicate the hint provider
	// wants to be consulted with when making topology hints
	AddHintProvider(h HintProvider)
	// AddContainer adds pod to Manager for tracking
	AddContainer(pod *v1.Pod, container *v1.Container, containerID string)
	// RemoveContainer removes pod from Manager tracking
	RemoveContainer(containerID string) error
	// Store is the interface for storing pod topology hints
	Store
}

type scope struct {
	mutex sync.Mutex
	name  string
	// Mapping of a Pods mapping of Containers and their TopologyHints
	// Indexed by PodUID to ContainerName
	podTopologyHints podTopologyHints
	// The list of components registered with the Manager
	hintProviders []HintProvider
	// Topology Manager Policy
	policy Policy
	// Mapping of (PodUid, ContainerName) to ContainerID for Adding/Removing Pods from PodTopologyHints mapping
	podMap containermap.ContainerMap
}

func (s *scope) Name() string {
	return s.name
}

func (s *scope) getTopologyHints(podUID string, containerName string) TopologyHint {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	return s.podTopologyHints[podUID][containerName]
}

func (s *scope) setTopologyHints(podUID string, containerName string, th TopologyHint) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	if s.podTopologyHints[podUID] == nil {
		s.podTopologyHints[podUID] = make(map[string]TopologyHint)
	}
	s.podTopologyHints[podUID][containerName] = th
}

func (s *scope) GetAffinity(podUID string, containerName string) TopologyHint {
	return s.getTopologyHints(podUID, containerName)
}

func (s *scope) GetPolicy() Policy {
	return s.policy
}

func (s *scope) AddHintProvider(h HintProvider) {
	s.hintProviders = append(s.hintProviders, h)
}

// It would be better to implement this function in topologymanager instead of scope
// but topologymanager do not track mapping anymore
func (s *scope) AddContainer(pod *v1.Pod, container *v1.Container, containerID string) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	s.podMap.Add(string(pod.UID), container.Name, containerID)
}

// It would be better to implement this function in topologymanager instead of scope
// but topologymanager do not track mapping anymore
func (s *scope) RemoveContainer(containerID string) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	klog.InfoS("RemoveContainer", "containerID", containerID)
	// Get the podUID and containerName associated with the containerID to be removed and remove it
	podUIDString, containerName, err := s.podMap.GetContainerRef(containerID)
	if err != nil {
		return nil
	}
	s.podMap.RemoveByContainerID(containerID)

	// In cases where a container has been restarted, it's possible that the same podUID and
	// containerName are already associated with a *different* containerID now. Only remove
	// the TopologyHints associated with that podUID and containerName if this is not true
	if _, err := s.podMap.GetContainerID(podUIDString, containerName); err != nil {
		delete(s.podTopologyHints[podUIDString], containerName)
		if len(s.podTopologyHints[podUIDString]) == 0 {
			delete(s.podTopologyHints, podUIDString)
		}
	}

	return nil
}

func (s *scope) admitPolicyNone(pod *v1.Pod) lifecycle.PodAdmitResult {
	for _, container := range append(pod.Spec.InitContainers, pod.Spec.Containers...) {
		err := s.allocateAlignedResources(pod, &container)
		if err != nil {
			return admission.GetPodAdmitResult(err)
		}
	}
	return admission.GetPodAdmitResult(nil)
}

// It would be better to implement this function in topologymanager instead of scope
// but topologymanager do not track providers anymore
func (s *scope) allocateAlignedResources(pod *v1.Pod, container *v1.Container) error {
	for _, provider := range s.hintProviders {
		err := provider.Allocate(pod, container)
		if err != nil {
			return err
		}
	}
	return nil
}
