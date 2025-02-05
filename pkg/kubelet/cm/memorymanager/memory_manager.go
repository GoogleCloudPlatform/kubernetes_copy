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
	"context"
	"fmt"
	"runtime"
	"sync"

	cadvisorapi "github.com/google/cadvisor/info/v1"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/sets"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
	"k8s.io/klog/v2"
	podutil "k8s.io/kubernetes/pkg/api/v1/pod"
	corev1helper "k8s.io/kubernetes/pkg/apis/core/v1/helper"
	kubeletconfig "k8s.io/kubernetes/pkg/kubelet/apis/config"
	"k8s.io/kubernetes/pkg/kubelet/cm/containermap"
	"k8s.io/kubernetes/pkg/kubelet/cm/memorymanager/state"
	"k8s.io/kubernetes/pkg/kubelet/cm/topologymanager"
	"k8s.io/kubernetes/pkg/kubelet/config"
	"k8s.io/kubernetes/pkg/kubelet/status"
)

// memoryManagerStateFileName is the file name where memory manager stores its state
const memoryManagerStateFileName = "memory_manager_state"

// ActivePodsFunc is a function that returns a list of active pods
type ActivePodsFunc func() []*v1.Pod

type runtimeService interface {
	UpdateContainerResources(ctx context.Context, id string, resources *runtimeapi.ContainerResources) error
}

type sourcesReadyStub struct{}

func (s *sourcesReadyStub) AddSource(source string) {}
func (s *sourcesReadyStub) AllReady() bool          { return true }

// Manager interface provides methods for Kubelet to manage pod memory.
type Manager interface {
	// Start is called during Kubelet initialization.
	Start(activePods ActivePodsFunc, sourcesReady config.SourcesReady, podStatusProvider status.PodStatusProvider, containerRuntime runtimeService, initialContainers containermap.ContainerMap) error

	// AddContainer adds the mapping between container ID to pod UID and the container name
	// The mapping used to remove the memory allocation during the container removal
	AddContainer(p *v1.Pod, c *v1.Container, containerID string)

	// Allocate is called to pre-allocate memory resources during Pod admission.
	// This must be called at some point prior to the AddContainer() call for a container, e.g. at pod admission time.
	Allocate(pod *v1.Pod, container *v1.Container) error

	// RemoveContainer is called after Kubelet decides to kill or delete a
	// container. After this call, any memory allocated to the container is freed.
	RemoveContainer(containerID string) error

	// State returns a read-only interface to the internal memory manager state.
	State() state.Reader

	// GetTopologyHints implements the topologymanager.HintProvider Interface
	// and is consulted to achieve NUMA aware resource alignment among this
	// and other resource controllers.
	GetTopologyHints(*v1.Pod, *v1.Container) map[string][]topologymanager.TopologyHint

	// GetPodTopologyHints implements the topologymanager.HintProvider Interface
	// and is consulted to achieve NUMA aware resource alignment among this
	// and other resource controllers.
	GetPodTopologyHints(*v1.Pod) map[string][]topologymanager.TopologyHint

	// GetMemoryNUMANodes provides NUMA nodes that are used to allocate the container memory
	GetMemoryNUMANodes(pod *v1.Pod, container *v1.Container) sets.Set[int]

	// GetAllocatableMemory returns the amount of allocatable memory for each NUMA node
	GetAllocatableMemory() []state.Block

	// GetMemory returns the memory allocated by a container from NUMA nodes
	GetMemory(podUID, containerName string) []state.Block

	// Returns true if this manager can allocate exclusively to a container the resource(s) it manages.
	CanAllocateExclusively(res v1.ResourceName) bool
}

type manager struct {
	sync.Mutex
	policy Policy

	// state allows to restore information regarding memory allocation for guaranteed pods
	// in the case of the kubelet restart
	state state.State

	// containerRuntime is the container runtime service interface needed
	// to make UpdateContainerResources() calls against the containers.
	containerRuntime runtimeService

	// activePods is a method for listing active pods on the node
	// so all the containers can be updated during call to the removeStaleState.
	activePods ActivePodsFunc

	// podStatusProvider provides a method for obtaining pod statuses
	// and the containerID of their containers
	podStatusProvider status.PodStatusProvider

	// containerMap provides a mapping from (pod, container) -> containerID
	// for all containers a pod
	containerMap containermap.ContainerMap

	// sourcesReady provides the readiness of kubelet configuration sources such as apiserver update readiness.
	// We use it to determine when we can purge inactive pods from checkpointed state.
	sourcesReady config.SourcesReady

	// stateFileDirectory holds the directory where the state file for checkpoints is held.
	stateFileDirectory string

	// allocatableMemory holds the allocatable memory for each NUMA node
	allocatableMemory []state.Block
}

var _ Manager = &manager{}

// NewManager returns new instance of the memory manager
func NewManager(policyName string, machineInfo *cadvisorapi.MachineInfo, nodeAllocatableReservation v1.ResourceList, reservedMemory []kubeletconfig.MemoryReservation, stateFileDirectory string, affinity topologymanager.Store) (Manager, error) {
	var policy Policy

	switch policyType(policyName) {

	case policyTypeNone:
		policy = NewPolicyNone()

	case policyTypeStatic:
		if runtime.GOOS == "windows" {
			return nil, fmt.Errorf("policy %q is not available on Windows", policyTypeStatic)
		}

		systemReserved, err := getSystemReservedMemory(machineInfo, nodeAllocatableReservation, reservedMemory)
		if err != nil {
			return nil, err
		}

		policy, err = NewPolicyStatic(machineInfo, systemReserved, affinity)
		if err != nil {
			return nil, err
		}

	case policyTypeBestEffort:
		if runtime.GOOS == "windows" {
			systemReserved, err := getSystemReservedMemory(machineInfo, nodeAllocatableReservation, reservedMemory)
			if err != nil {
				return nil, err
			}
			policy, err = NewPolicyBestEffort(machineInfo, systemReserved, affinity)
			if err != nil {
				return nil, err
			}
		} else {
			return nil, fmt.Errorf("policy %q is not available for platform %q", policyTypeBestEffort, runtime.GOOS)
		}

	default:
		return nil, fmt.Errorf("unknown policy: %q", policyName)
	}

	manager := &manager{
		policy:             policy,
		stateFileDirectory: stateFileDirectory,
	}
	manager.sourcesReady = &sourcesReadyStub{}
	return manager, nil
}

// Start starts the memory manager under the kubelet and calls policy start
func (m *manager) Start(activePods ActivePodsFunc, sourcesReady config.SourcesReady, podStatusProvider status.PodStatusProvider, containerRuntime runtimeService, initialContainers containermap.ContainerMap) error {
	klog.InfoS("Starting memorymanager", "policy", m.policy.Name())
	m.sourcesReady = sourcesReady
	m.activePods = activePods
	m.podStatusProvider = podStatusProvider
	m.containerRuntime = containerRuntime
	m.containerMap = initialContainers

	stateImpl, err := state.NewCheckpointState(m.stateFileDirectory, memoryManagerStateFileName, m.policy.Name())
	if err != nil {
		klog.ErrorS(err, "Could not initialize checkpoint manager, please drain node and remove policy state file")
		return err
	}
	m.state = stateImpl

	err = m.policy.Start(m.state)
	if err != nil {
		klog.ErrorS(err, "Policy start error")
		return err
	}

	m.allocatableMemory = m.policy.GetAllocatableMemory(m.state)

	return nil
}

// AddContainer saves the value of requested memory for the guaranteed pod under the state and set memory affinity according to the topolgy manager
func (m *manager) AddContainer(pod *v1.Pod, container *v1.Container, containerID string) {
	m.Lock()
	defer m.Unlock()

	m.containerMap.Add(string(pod.UID), container.Name, containerID)

	// Since we know that each init container always runs to completion before
	// the next container starts, we can safely remove references to any previously
	// started init containers. This will free up the memory from these init containers
	// for use in other pods. If the current container happens to be an init container,
	// we skip deletion of it until the next container is added, and this is called again.
	for _, initContainer := range pod.Spec.InitContainers {
		if initContainer.Name == container.Name {
			break
		}

		// Since a restartable init container remains running for the full
		// duration of the pod's lifecycle, we should not remove it from the
		// memory manager state.
		if podutil.IsRestartableInitContainer(&initContainer) {
			continue
		}

		m.policyRemoveContainerByRef(string(pod.UID), initContainer.Name)
	}
}

// GetMemoryNUMANodes provides NUMA nodes that used to allocate the container memory
func (m *manager) GetMemoryNUMANodes(pod *v1.Pod, container *v1.Container) sets.Set[int] {
	// Get NUMA node affinity of blocks assigned to the container during Allocate()
	numaNodes := sets.New[int]()
	for _, block := range m.state.GetMemoryBlocks(string(pod.UID), container.Name) {
		for _, nodeID := range block.NUMAAffinity {
			// avoid nodes duplication when hugepages and memory blocks pinned to the same NUMA node
			numaNodes.Insert(nodeID)
		}
	}

	if numaNodes.Len() == 0 {
		klog.V(5).InfoS("No allocation is available", "pod", klog.KObj(pod), "containerName", container.Name)
		return nil
	}

	klog.InfoS("Memory affinity", "pod", klog.KObj(pod), "containerName", container.Name, "numaNodes", numaNodes)
	return numaNodes
}

// Allocate is called to pre-allocate memory resources during Pod admission.
func (m *manager) Allocate(pod *v1.Pod, container *v1.Container) error {
	// Garbage collect any stranded resources before allocation
	m.removeStaleState()

	m.Lock()
	defer m.Unlock()

	// Call down into the policy to assign this container memory if required.
	if err := m.policy.Allocate(m.state, pod, container); err != nil {
		klog.ErrorS(err, "Allocate error")
		return err
	}
	return nil
}

// RemoveContainer removes the container from the state
func (m *manager) RemoveContainer(containerID string) error {
	m.Lock()
	defer m.Unlock()

	// if error appears it means container entry already does not exist under the container map
	podUID, containerName, err := m.containerMap.GetContainerRef(containerID)
	if err != nil {
		klog.InfoS("Failed to get container from container map", "containerID", containerID, "err", err)
		return nil
	}

	m.policyRemoveContainerByRef(podUID, containerName)

	return nil
}

// State returns the state of the manager
func (m *manager) State() state.Reader {
	return m.state
}

// GetPodTopologyHints returns the topology hints for the topology manager
func (m *manager) GetPodTopologyHints(pod *v1.Pod) map[string][]topologymanager.TopologyHint {
	// Garbage collect any stranded resources before providing TopologyHints
	m.removeStaleState()
	// Delegate to active policy
	return m.policy.GetPodTopologyHints(m.state, pod)
}

// GetTopologyHints returns the topology hints for the topology manager
func (m *manager) GetTopologyHints(pod *v1.Pod, container *v1.Container) map[string][]topologymanager.TopologyHint {
	// Garbage collect any stranded resources before providing TopologyHints
	m.removeStaleState()
	// Delegate to active policy
	return m.policy.GetTopologyHints(m.state, pod, container)
}

// TODO: move the method to the upper level, to re-use it under the CPU and memory managers
func (m *manager) removeStaleState() {
	// Only once all sources are ready do we attempt to remove any stale state.
	// This ensures that the call to `m.activePods()` below will succeed with
	// the actual active pods list.
	if !m.sourcesReady.AllReady() {
		return
	}

	// We grab the lock to ensure that no new containers will grab memory block while
	// executing the code below. Without this lock, its possible that we end up
	// removing state that is newly added by an asynchronous call to
	// AddContainer() during the execution of this code.
	m.Lock()
	defer m.Unlock()

	// Get the list of active pods.
	activePods := m.activePods()

	// Build a list of (podUID, containerName) pairs for all containers in all active Pods.
	activeContainers := make(map[string]map[string]struct{})
	for _, pod := range activePods {
		activeContainers[string(pod.UID)] = make(map[string]struct{})
		for _, container := range append(pod.Spec.InitContainers, pod.Spec.Containers...) {
			activeContainers[string(pod.UID)][container.Name] = struct{}{}
		}
	}

	// Loop through the MemoryManager state. Remove any state for containers not
	// in the `activeContainers` list built above.
	assignments := m.state.GetMemoryAssignments()
	for podUID := range assignments {
		for containerName := range assignments[podUID] {
			if _, ok := activeContainers[podUID][containerName]; !ok {
				klog.InfoS("RemoveStaleState removing state", "podUID", podUID, "containerName", containerName)
				m.policyRemoveContainerByRef(podUID, containerName)
			}
		}
	}

	m.containerMap.Visit(func(podUID, containerName, containerID string) {
		if _, ok := activeContainers[podUID][containerName]; !ok {
			klog.InfoS("RemoveStaleState removing state", "podUID", podUID, "containerName", containerName)
			m.policyRemoveContainerByRef(podUID, containerName)
		}
	})
}

func (m *manager) policyRemoveContainerByRef(podUID string, containerName string) {
	m.policy.RemoveContainer(m.state, podUID, containerName)
	m.containerMap.RemoveByContainerRef(podUID, containerName)
}

func getTotalMemoryTypeReserved(machineInfo *cadvisorapi.MachineInfo, reservedMemory []kubeletconfig.MemoryReservation) (map[v1.ResourceName]resource.Quantity, error) {
	totalMemoryType := map[v1.ResourceName]resource.Quantity{}

	numaNodes := map[int]bool{}
	for _, numaNode := range machineInfo.Topology {
		numaNodes[numaNode.Id] = true
	}

	for _, reservation := range reservedMemory {
		if !numaNodes[int(reservation.NumaNode)] {
			return nil, fmt.Errorf("the reserved memory configuration references a NUMA node %d that does not exist on this machine", reservation.NumaNode)
		}

		for resourceName, q := range reservation.Limits {
			if value, ok := totalMemoryType[resourceName]; ok {
				q.Add(value)
			}
			totalMemoryType[resourceName] = q
		}
	}

	return totalMemoryType, nil
}

func validateReservedMemory(machineInfo *cadvisorapi.MachineInfo, nodeAllocatableReservation v1.ResourceList, reservedMemory []kubeletconfig.MemoryReservation) error {
	totalMemoryType, err := getTotalMemoryTypeReserved(machineInfo, reservedMemory)
	if err != nil {
		return err
	}

	commonMemoryTypeSet := make(map[v1.ResourceName]bool)
	for resourceType := range totalMemoryType {
		commonMemoryTypeSet[resourceType] = true
	}

	for resourceType := range nodeAllocatableReservation {
		if !(corev1helper.IsHugePageResourceName(resourceType) || resourceType == v1.ResourceMemory) {
			continue
		}
		commonMemoryTypeSet[resourceType] = true
	}

	for resourceType := range commonMemoryTypeSet {
		nodeAllocatableMemory := resource.NewQuantity(0, resource.DecimalSI)
		if memValue, set := nodeAllocatableReservation[resourceType]; set {
			nodeAllocatableMemory.Add(memValue)
		}

		reservedMemory := resource.NewQuantity(0, resource.DecimalSI)
		if memValue, set := totalMemoryType[resourceType]; set {
			reservedMemory.Add(memValue)
		}

		if !(*nodeAllocatableMemory).Equal(*reservedMemory) {
			return fmt.Errorf("the total amount %q of type %q is not equal to the value %q determined by Node Allocatable feature", reservedMemory.String(), resourceType, nodeAllocatableMemory.String())
		}
	}

	return nil
}

func convertReserved(machineInfo *cadvisorapi.MachineInfo, reservedMemory []kubeletconfig.MemoryReservation) (systemReservedMemory, error) {
	reservedMemoryConverted := make(map[int]map[v1.ResourceName]uint64)
	for _, node := range machineInfo.Topology {
		reservedMemoryConverted[node.Id] = make(map[v1.ResourceName]uint64)
	}

	for _, reservation := range reservedMemory {
		for resourceName, q := range reservation.Limits {
			val, success := q.AsInt64()
			if !success {
				return nil, fmt.Errorf("could not covert a variable of type Quantity to int64")
			}
			reservedMemoryConverted[int(reservation.NumaNode)][resourceName] = uint64(val)
		}
	}

	return reservedMemoryConverted, nil
}

func getSystemReservedMemory(machineInfo *cadvisorapi.MachineInfo, nodeAllocatableReservation v1.ResourceList, reservedMemory []kubeletconfig.MemoryReservation) (systemReservedMemory, error) {
	if err := validateReservedMemory(machineInfo, nodeAllocatableReservation, reservedMemory); err != nil {
		return nil, err
	}

	reservedMemoryConverted, err := convertReserved(machineInfo, reservedMemory)
	if err != nil {
		return nil, err
	}

	return reservedMemoryConverted, nil
}

// GetAllocatableMemory returns the amount of allocatable memory for each NUMA node
func (m *manager) GetAllocatableMemory() []state.Block {
	return m.allocatableMemory
}

// GetMemory returns the memory allocated by a container from NUMA nodes
func (m *manager) GetMemory(podUID, containerName string) []state.Block {
	return m.state.GetMemoryBlocks(podUID, containerName)
}
func (m *manager) CanAllocateExclusively(res v1.ResourceName) bool {
	if res != v1.ResourceMemory && !corev1helper.IsHugePageResourceName(res) {
		return false
	}
	return m.policy.CanAllocateExclusively()
}
