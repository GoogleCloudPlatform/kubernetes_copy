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

package memorymanager

import (
	v1 "k8s.io/api/core/v1"
	"k8s.io/kubernetes/pkg/kubelet/cm/memorymanager/state"
	"k8s.io/kubernetes/pkg/kubelet/cm/topologymanager"
)

// Type defines the policy type
type policyType string

// Policy implements logic for pod container to a memory assignment.
type Policy interface {
	Name() string
	Start(s state.State) error
	// Allocate call is idempotent
	Allocate(s state.State, pod *v1.Pod, container *v1.Container) error
	// RemoveContainer call is idempotent
	RemoveContainer(s state.State, podUID string, containerName string)
	// GetTopologyHints implements the topologymanager.HintProvider Interface
	// and is consulted to achieve NUMA aware resource alignment among this
	// and other resource controllers.
	GetTopologyHints(s state.State, pod *v1.Pod, container *v1.Container) map[string][]topologymanager.TopologyHint
	// GetPodTopologyHints implements the topologymanager.HintProvider Interface
	// and is consulted to achieve NUMA aware resource alignment among this
	// and other resource controllers.
	GetPodTopologyHints(s state.State, pod *v1.Pod) map[string][]topologymanager.TopologyHint
	// GetAllocatableMemory returns the amount of allocatable memory for each NUMA node
	GetAllocatableMemory(s state.State) []state.Block
	// CanAllocateExclusively returns true if the policy can allocate exclusively CPUs
	CanAllocateExclusively() bool
}
