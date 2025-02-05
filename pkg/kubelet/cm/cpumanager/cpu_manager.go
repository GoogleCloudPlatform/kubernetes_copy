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
	"fmt"
	"math"
	"sync"
	"time"

	cadvisorapi "github.com/google/cadvisor/info/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
	"k8s.io/klog/v2"

	"k8s.io/kubernetes/pkg/kubelet/cm/containermap"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpumanager/state"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpumanager/topology"
	"k8s.io/kubernetes/pkg/kubelet/cm/topologymanager"
	"k8s.io/kubernetes/pkg/kubelet/config"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	"k8s.io/kubernetes/pkg/kubelet/status"
	"k8s.io/utils/cpuset"
)

// ActivePodsFunc is a function that returns a list of pods to reconcile.
type ActivePodsFunc func() []*v1.Pod

type runtimeService interface {
	UpdateContainerResources(ctx context.Context, id string, resources *runtimeapi.ContainerResources) error
}

type policyName string

// cpuManagerStateFileName is the file name where cpu manager stores its state
const cpuManagerStateFileName = "cpu_manager_state"

// Manager interface provides methods for Kubelet to manage pod cpus.
type Manager interface {
	// Start is called during Kubelet initialization.
	Start(activePods ActivePodsFunc, sourcesReady config.SourcesReady, podStatusProvider status.PodStatusProvider, containerRuntime runtimeService, initialContainers containermap.ContainerMap) error

	// Called to trigger the allocation of CPUs to a container. This must be
	// called at some point prior to the AddContainer() call for a container,
	// e.g. at pod admission time.
	Allocate(pod *v1.Pod, container *v1.Container) error

	// AddContainer adds the mapping between container ID to pod UID and the container name
	// The mapping used to remove the CPU allocation during the container removal
	AddContainer(p *v1.Pod, c *v1.Container, containerID string)

	// RemoveContainer is called after Kubelet decides to kill or delete a
	// container. After this call, the CPU manager stops trying to reconcile
	// that container and any CPUs dedicated to the container are freed.
	RemoveContainer(containerID string) error

	// State returns a read-only interface to the internal CPU manager state.
	State() state.Reader

	// GetTopologyHints implements the topologymanager.HintProvider Interface
	// and is consulted to achieve NUMA aware resource alignment among this
	// and other resource controllers.
	GetTopologyHints(*v1.Pod, *v1.Container) map[string][]topologymanager.TopologyHint

	// GetExclusiveCPUs implements the podresources.CPUsProvider interface to provide
	// exclusively allocated cpus for the container
	GetExclusiveCPUs(podUID, containerName string) cpuset.CPUSet

	// GetPodTopologyHints implements the topologymanager.HintProvider Interface
	// and is consulted to achieve NUMA aware resource alignment per Pod
	// among this and other resource controllers.
	GetPodTopologyHints(pod *v1.Pod) map[string][]topologymanager.TopologyHint

	// GetAllocatableCPUs returns the total set of CPUs available for allocation.
	GetAllocatableCPUs() cpuset.CPUSet

	// GetCPUAffinity returns cpuset which includes cpus from shared pools
	// as well as exclusively allocated cpus
	GetCPUAffinity(podUID, containerName string) cpuset.CPUSet

	// GetAllCPUs returns all the CPUs known by cpumanager, as reported by the
	// hardware discovery. Maps to the CPU capacity.
	GetAllCPUs() cpuset.CPUSet
}

type manager struct {
	sync.Mutex
	policy Policy

	// reconcilePeriod is the duration between calls to reconcileState.
	reconcilePeriod time.Duration

	// state allows pluggable CPU assignment policies while sharing a common
	// representation of state for the system to inspect and reconcile.
	state state.State

	// lastUpdatedstate holds state for each container from the last time it was updated.
	lastUpdateState state.State

	// containerRuntime is the container runtime service interface needed
	// to make UpdateContainerResources() calls against the containers.
	containerRuntime runtimeService

	// activePods is a method for listing active pods on the node
	// so all the containers can be updated in the reconciliation loop.
	activePods ActivePodsFunc

	// podStatusProvider provides a method for obtaining pod statuses
	// and the containerID of their containers
	podStatusProvider status.PodStatusProvider

	// containerMap provides a mapping from (pod, container) -> containerID
	// for all containers a pod
	containerMap containermap.ContainerMap

	topology *topology.CPUTopology

	nodeAllocatableReservation v1.ResourceList

	// sourcesReady provides the readiness of kubelet configuration sources such as apiserver update readiness.
	// We use it to determine when we can purge inactive pods from checkpointed state.
	sourcesReady config.SourcesReady

	// stateFileDirectory holds the directory where the state file for checkpoints is held.
	stateFileDirectory string

	// allCPUs is the set of online CPUs as reported by the system
	allCPUs cpuset.CPUSet

	// allocatableCPUs is the set of online CPUs as reported by the system,
	// and available for allocation, minus the reserved set
	allocatableCPUs cpuset.CPUSet
}

var _ Manager = &manager{}

type sourcesReadyStub struct{}

func (s *sourcesReadyStub) AddSource(source string) {}
func (s *sourcesReadyStub) AllReady() bool          { return true }

// NewManager creates new cpu manager based on provided policy
func NewManager(cpuPolicyName string, cpuPolicyOptions map[string]string, reconcilePeriod time.Duration, machineInfo *cadvisorapi.MachineInfo, specificCPUs cpuset.CPUSet, nodeAllocatableReservation v1.ResourceList, stateFileDirectory string, affinity topologymanager.Store) (Manager, error) {
	var topo *topology.CPUTopology
	var policy Policy
	var err error

	topo, err = topology.Discover(machineInfo)
	if err != nil {
		return nil, err
	}

	switch policyName(cpuPolicyName) {

	case PolicyNone:
		policy, err = NewNonePolicy(cpuPolicyOptions)
		if err != nil {
			return nil, fmt.Errorf("new none policy error: %w", err)
		}

	case PolicyStatic:
		klog.InfoS("Detected CPU topology", "topology", topo)

		reservedCPUs, ok := nodeAllocatableReservation[v1.ResourceCPU]
		if !ok {
			// The static policy cannot initialize without this information.
			return nil, fmt.Errorf("[cpumanager] unable to determine reserved CPU resources for static policy")
		}
		if reservedCPUs.IsZero() {
			// The static policy requires this to be nonzero. Zero CPU reservation
			// would allow the shared pool to be completely exhausted. At that point
			// either we would violate our guarantee of exclusivity or need to evict
			// any pod that has at least one container that requires zero CPUs.
			// See the comments in policy_static.go for more details.
			return nil, fmt.Errorf("[cpumanager] the static policy requires systemreserved.cpu + kubereserved.cpu to be greater than zero")
		}

		// Take the ceiling of the reservation, since fractional CPUs cannot be
		// exclusively allocated.
		reservedCPUsFloat := float64(reservedCPUs.MilliValue()) / 1000
		numReservedCPUs := int(math.Ceil(reservedCPUsFloat))
		policy, err = NewStaticPolicy(topo, numReservedCPUs, specificCPUs, affinity, cpuPolicyOptions)
		if err != nil {
			return nil, fmt.Errorf("new static policy error: %w", err)
		}

	default:
		return nil, fmt.Errorf("unknown policy: \"%s\"", cpuPolicyName)
	}

	manager := &manager{
		policy:                     policy,
		reconcilePeriod:            reconcilePeriod,
		lastUpdateState:            state.NewMemoryState(),
		topology:                   topo,
		nodeAllocatableReservation: nodeAllocatableReservation,
		stateFileDirectory:         stateFileDirectory,
		allCPUs:                    topo.CPUDetails.CPUs(),
	}
	manager.sourcesReady = &sourcesReadyStub{}
	return manager, nil
}

func (m *manager) Start(activePods ActivePodsFunc, sourcesReady config.SourcesReady, podStatusProvider status.PodStatusProvider, containerRuntime runtimeService, initialContainers containermap.ContainerMap) error {
	klog.InfoS("Starting CPU manager", "policy", m.policy.Name())
	klog.InfoS("Reconciling", "reconcilePeriod", m.reconcilePeriod)
	m.sourcesReady = sourcesReady
	m.activePods = activePods
	m.podStatusProvider = podStatusProvider
	m.containerRuntime = containerRuntime
	m.containerMap = initialContainers

	stateImpl, err := state.NewCheckpointState(m.stateFileDirectory, cpuManagerStateFileName, m.policy.Name(), m.containerMap)
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

	klog.V(4).InfoS("CPU manager started", "policy", m.policy.Name())

	m.allocatableCPUs = m.policy.GetAllocatableCPUs(m.state)

	if m.policy.Name() == string(PolicyNone) {
		return nil
	}
	// Periodically call m.reconcileState() to continue to keep the CPU sets of
	// all pods in sync with and guaranteed CPUs handed out among them.
	go wait.Until(func() { m.reconcileState() }, m.reconcilePeriod, wait.NeverStop)
	return nil
}

func (m *manager) Allocate(p *v1.Pod, c *v1.Container) error {
	// Garbage collect any stranded resources before allocating CPUs.
	m.removeStaleState()

	m.Lock()
	defer m.Unlock()

	// Call down into the policy to assign this container CPUs if required.
	err := m.policy.Allocate(m.state, p, c)
	if err != nil {
		klog.ErrorS(err, "Allocate error")
		return err
	}

	return nil
}

func (m *manager) AddContainer(pod *v1.Pod, container *v1.Container, containerID string) {
	m.Lock()
	defer m.Unlock()
	if cset, exists := m.state.GetCPUSet(string(pod.UID), container.Name); exists {
		m.lastUpdateState.SetCPUSet(string(pod.UID), container.Name, cset)
	}
	m.containerMap.Add(string(pod.UID), container.Name, containerID)
}

func (m *manager) RemoveContainer(containerID string) error {
	m.Lock()
	defer m.Unlock()

	err := m.policyRemoveContainerByID(containerID)
	if err != nil {
		klog.ErrorS(err, "RemoveContainer error")
		return err
	}

	return nil
}

func (m *manager) policyRemoveContainerByID(containerID string) error {
	podUID, containerName, err := m.containerMap.GetContainerRef(containerID)
	if err != nil {
		return nil
	}

	err = m.policy.RemoveContainer(m.state, podUID, containerName)
	if err == nil {
		m.lastUpdateState.Delete(podUID, containerName)
		m.containerMap.RemoveByContainerID(containerID)
	}

	return err
}

func (m *manager) policyRemoveContainerByRef(podUID string, containerName string) error {
	err := m.policy.RemoveContainer(m.state, podUID, containerName)
	if err == nil {
		m.lastUpdateState.Delete(podUID, containerName)
		m.containerMap.RemoveByContainerRef(podUID, containerName)
	}

	return err
}

func (m *manager) State() state.Reader {
	return m.state
}

func (m *manager) GetTopologyHints(pod *v1.Pod, container *v1.Container) map[string][]topologymanager.TopologyHint {
	// Garbage collect any stranded resources before providing TopologyHints
	m.removeStaleState()
	// Delegate to active policy
	return m.policy.GetTopologyHints(m.state, pod, container)
}

func (m *manager) GetPodTopologyHints(pod *v1.Pod) map[string][]topologymanager.TopologyHint {
	// Garbage collect any stranded resources before providing TopologyHints
	m.removeStaleState()
	// Delegate to active policy
	return m.policy.GetPodTopologyHints(m.state, pod)
}

func (m *manager) GetAllocatableCPUs() cpuset.CPUSet {
	return m.allocatableCPUs.Clone()
}

func (m *manager) GetAllCPUs() cpuset.CPUSet {
	return m.allCPUs.Clone()
}

type reconciledContainer struct {
	podName       string
	containerName string
	containerID   string
}

func (m *manager) removeStaleState() {
	// Only once all sources are ready do we attempt to remove any stale state.
	// This ensures that the call to `m.activePods()` below will succeed with
	// the actual active pods list.
	if !m.sourcesReady.AllReady() {
		return
	}

	// We grab the lock to ensure that no new containers will grab CPUs while
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

	// Loop through the CPUManager state. Remove any state for containers not
	// in the `activeContainers` list built above.
	assignments := m.state.GetCPUAssignments()
	for podUID := range assignments {
		for containerName := range assignments[podUID] {
			if _, ok := activeContainers[podUID][containerName]; ok {
				klog.V(5).InfoS("RemoveStaleState: container still active", "podUID", podUID, "containerName", containerName)
				continue
			}
			klog.V(2).InfoS("RemoveStaleState: removing container", "podUID", podUID, "containerName", containerName)
			err := m.policyRemoveContainerByRef(podUID, containerName)
			if err != nil {
				klog.ErrorS(err, "RemoveStaleState: failed to remove container", "podUID", podUID, "containerName", containerName)
			}
		}
	}

	m.containerMap.Visit(func(podUID, containerName, containerID string) {
		if _, ok := activeContainers[podUID][containerName]; ok {
			klog.V(5).InfoS("RemoveStaleState: containerMap: container still active", "podUID", podUID, "containerName", containerName)
			return
		}
		klog.V(2).InfoS("RemoveStaleState: containerMap: removing container", "podUID", podUID, "containerName", containerName)
		err := m.policyRemoveContainerByRef(podUID, containerName)
		if err != nil {
			klog.ErrorS(err, "RemoveStaleState: containerMap: failed to remove container", "podUID", podUID, "containerName", containerName)
		}
	})
}

func (m *manager) reconcileState() (success []reconciledContainer, failure []reconciledContainer) {
	ctx := context.Background()
	success = []reconciledContainer{}
	failure = []reconciledContainer{}

	m.removeStaleState()
	for _, pod := range m.activePods() {
		pstatus, ok := m.podStatusProvider.GetPodStatus(pod.UID)
		if !ok {
			klog.V(5).InfoS("ReconcileState: skipping pod; status not found", "pod", klog.KObj(pod))
			failure = append(failure, reconciledContainer{pod.Name, "", ""})
			continue
		}

		allContainers := pod.Spec.InitContainers
		allContainers = append(allContainers, pod.Spec.Containers...)
		for _, container := range allContainers {
			containerID, err := findContainerIDByName(&pstatus, container.Name)
			if err != nil {
				klog.V(5).InfoS("ReconcileState: skipping container; ID not found in pod status", "pod", klog.KObj(pod), "containerName", container.Name, "err", err)
				failure = append(failure, reconciledContainer{pod.Name, container.Name, ""})
				continue
			}

			cstatus, err := findContainerStatusByName(&pstatus, container.Name)
			if err != nil {
				klog.V(5).InfoS("ReconcileState: skipping container; container status not found in pod status", "pod", klog.KObj(pod), "containerName", container.Name, "err", err)
				failure = append(failure, reconciledContainer{pod.Name, container.Name, ""})
				continue
			}

			if cstatus.State.Waiting != nil ||
				(cstatus.State.Waiting == nil && cstatus.State.Running == nil && cstatus.State.Terminated == nil) {
				klog.V(4).InfoS("ReconcileState: skipping container; container still in the waiting state", "pod", klog.KObj(pod), "containerName", container.Name, "err", err)
				failure = append(failure, reconciledContainer{pod.Name, container.Name, ""})
				continue
			}

			m.Lock()
			if cstatus.State.Terminated != nil {
				// The container is terminated but we can't call m.RemoveContainer()
				// here because it could remove the allocated cpuset for the container
				// which may be in the process of being restarted.  That would result
				// in the container losing any exclusively-allocated CPUs that it
				// was allocated.
				_, _, err := m.containerMap.GetContainerRef(containerID)
				if err == nil {
					klog.V(4).InfoS("ReconcileState: ignoring terminated container", "pod", klog.KObj(pod), "containerID", containerID)
				}
				m.Unlock()
				continue
			}

			// Once we make it here we know we have a running container.
			// Idempotently add it to the containerMap incase it is missing.
			// This can happen after a kubelet restart, for example.
			m.containerMap.Add(string(pod.UID), container.Name, containerID)
			m.Unlock()

			cset := m.state.GetCPUSetOrDefault(string(pod.UID), container.Name)
			if cset.IsEmpty() {
				// NOTE: This should not happen outside of tests.
				klog.V(2).InfoS("ReconcileState: skipping container; assigned cpuset is empty", "pod", klog.KObj(pod), "containerName", container.Name)
				failure = append(failure, reconciledContainer{pod.Name, container.Name, containerID})
				continue
			}

			lcset := m.lastUpdateState.GetCPUSetOrDefault(string(pod.UID), container.Name)
			if !cset.Equals(lcset) {
				klog.V(5).InfoS("ReconcileState: updating container", "pod", klog.KObj(pod), "containerName", container.Name, "containerID", containerID, "cpuSet", cset)
				err = m.updateContainerCPUSet(ctx, containerID, cset)
				if err != nil {
					klog.ErrorS(err, "ReconcileState: failed to update container", "pod", klog.KObj(pod), "containerName", container.Name, "containerID", containerID, "cpuSet", cset)
					failure = append(failure, reconciledContainer{pod.Name, container.Name, containerID})
					continue
				}
				m.lastUpdateState.SetCPUSet(string(pod.UID), container.Name, cset)
			}
			success = append(success, reconciledContainer{pod.Name, container.Name, containerID})
		}
	}
	return success, failure
}

func findContainerIDByName(status *v1.PodStatus, name string) (string, error) {
	allStatuses := status.InitContainerStatuses
	allStatuses = append(allStatuses, status.ContainerStatuses...)
	for _, container := range allStatuses {
		if container.Name == name && container.ContainerID != "" {
			cid := &kubecontainer.ContainerID{}
			err := cid.ParseString(container.ContainerID)
			if err != nil {
				return "", err
			}
			return cid.ID, nil
		}
	}
	return "", fmt.Errorf("unable to find ID for container with name %v in pod status (it may not be running)", name)
}

func findContainerStatusByName(status *v1.PodStatus, name string) (*v1.ContainerStatus, error) {
	for _, containerStatus := range append(status.InitContainerStatuses, status.ContainerStatuses...) {
		if containerStatus.Name == name {
			return &containerStatus, nil
		}
	}
	return nil, fmt.Errorf("unable to find status for container with name %v in pod status (it may not be running)", name)
}

func (m *manager) GetExclusiveCPUs(podUID, containerName string) cpuset.CPUSet {
	if result, ok := m.state.GetCPUSet(podUID, containerName); ok {
		return result
	}

	return cpuset.CPUSet{}
}

func (m *manager) GetCPUAffinity(podUID, containerName string) cpuset.CPUSet {
	return m.state.GetCPUSetOrDefault(podUID, containerName)
}
