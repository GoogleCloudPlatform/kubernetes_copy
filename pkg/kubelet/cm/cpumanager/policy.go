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
	"context"

	v1 "k8s.io/api/core/v1"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpumanager/state"
	"k8s.io/kubernetes/pkg/kubelet/cm/topologymanager"
	"k8s.io/utils/cpuset"
)

// Policy implements logic for pod container to CPU assignment.
type Policy interface {
	Name() string
	Start(ctx context.Context, s state.State) error
	// Allocate call is idempotent
	Allocate(ctx context.Context, s state.State, pod *v1.Pod, container *v1.Container) error
	// RemoveContainer call is idempotent
	RemoveContainer(ctx context.Context, s state.State, podUID string, containerName string) error
	// GetTopologyHints implements the topologymanager.HintProvider Interface
	// and is consulted to achieve NUMA aware resource alignment among this
	// and other resource controllers.
	GetTopologyHints(ctx context.Context, s state.State, pod *v1.Pod, container *v1.Container) map[string][]topologymanager.TopologyHint
	// GetPodTopologyHints implements the topologymanager.HintProvider Interface
	// and is consulted to achieve NUMA aware resource alignment per Pod
	// among this and other resource controllers.
	GetPodTopologyHints(ctx context.Context, s state.State, pod *v1.Pod) map[string][]topologymanager.TopologyHint
	// GetAllocatableCPUs returns the total set of CPUs available for allocation.
	GetAllocatableCPUs(ctx context.Context, m state.State) cpuset.CPUSet
}
