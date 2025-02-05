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
	"fmt"

	v1 "k8s.io/api/core/v1"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/klog/v2"
	podutil "k8s.io/kubernetes/pkg/api/v1/pod"
	v1qos "k8s.io/kubernetes/pkg/apis/core/v1/helper/qos"
	"k8s.io/kubernetes/pkg/features"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpumanager/state"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpumanager/topology"
	"k8s.io/kubernetes/pkg/kubelet/cm/topologymanager"
	"k8s.io/kubernetes/pkg/kubelet/cm/topologymanager/bitmask"
	"k8s.io/kubernetes/pkg/kubelet/metrics"
	"k8s.io/utils/cpuset"
)

const (

	// PolicyStatic is the name of the static policy.
	// Should options be given, these will be ignored and backward (up to 1.21 included)
	// compatible behaviour will be enforced
	PolicyStatic policyName = "static"
	// ErrorSMTAlignment represents the type of an SMTAlignmentError
	ErrorSMTAlignment = "SMTAlignmentError"
)

// SMTAlignmentError represents an error due to SMT alignment
type SMTAlignmentError struct {
	RequestedCPUs         int
	CpusPerCore           int
	AvailablePhysicalCPUs int
	CausedByPhysicalCPUs  bool
}

func (e SMTAlignmentError) Error() string {
	if e.CausedByPhysicalCPUs {
		return fmt.Sprintf("SMT Alignment Error: not enough free physical CPUs: available physical CPUs = %d, requested CPUs = %d, CPUs per core = %d", e.AvailablePhysicalCPUs, e.RequestedCPUs, e.CpusPerCore)
	}
	return fmt.Sprintf("SMT Alignment Error: requested %d cpus not multiple cpus per core = %d", e.RequestedCPUs, e.CpusPerCore)
}

// Type returns human-readable type of this error. Used in the admission control to populate Admission Failure reason.
func (e SMTAlignmentError) Type() string {
	return ErrorSMTAlignment
}

// staticPolicy is a CPU manager policy that does not change CPU
// assignments for exclusively pinned guaranteed containers after the main
// container process starts.
//
// This policy allocates CPUs exclusively for a container if all the following
// conditions are met:
//
// - The pod QoS class is Guaranteed.
// - The CPU request is a positive integer.
//
// The static policy maintains the following sets of logical CPUs:
//
//   - SHARED: Burstable, BestEffort, and non-integral Guaranteed containers
//     run here. Initially this contains all CPU IDs on the system. As
//     exclusive allocations are created and destroyed, this CPU set shrinks
//     and grows, accordingly. This is stored in the state as the default
//     CPU set.
//
//   - RESERVED: A subset of the shared pool which is not exclusively
//     allocatable. The membership of this pool is static for the lifetime of
//     the Kubelet. The size of the reserved pool is
//     ceil(systemreserved.cpu + kubereserved.cpu).
//     Reserved CPUs are taken topologically starting with lowest-indexed
//     physical core, as reported by cAdvisor.
//
//   - ASSIGNABLE: Equal to SHARED - RESERVED. Exclusive CPUs are allocated
//     from this pool.
//
//   - EXCLUSIVE ALLOCATIONS: CPU sets assigned exclusively to one container.
//     These are stored as explicit assignments in the state.
//
// When an exclusive allocation is made, the static policy also updates the
// default cpuset in the state abstraction. The CPU manager's periodic
// reconcile loop takes care of rewriting the cpuset in cgroupfs for any
// containers that may be running in the shared pool. For this reason,
// applications running within exclusively-allocated containers must tolerate
// potentially sharing their allocated CPUs for up to the CPU manager
// reconcile period.
type staticPolicy struct {
	// cpu socket topology
	topology *topology.CPUTopology
	// set of CPUs that is not available for exclusive assignment
	reservedCPUs cpuset.CPUSet
	// Superset of reservedCPUs. It includes not just the reservedCPUs themselves,
	// but also any siblings of those reservedCPUs on the same physical die.
	// NOTE: If the reserved set includes full physical CPUs from the beginning
	// (e.g. only reserved pairs of core siblings) this set is expected to be
	// identical to the reserved set.
	reservedPhysicalCPUs cpuset.CPUSet
	// topology manager reference to get container Topology affinity
	affinity topologymanager.Store
	// set of CPUs to reuse across allocations in a pod
	cpusToReuse map[string]cpuset.CPUSet
	// options allow to fine-tune the behaviour of the policy
	options StaticPolicyOptions
	// we compute this value multiple time, and it's not supposed to change
	// at runtime - the cpumanager can't deal with runtime topology changes anyway.
	cpuGroupSize int
}

// Ensure staticPolicy implements Policy interface
var _ Policy = &staticPolicy{}

// NewStaticPolicy returns a CPU manager policy that does not change CPU
// assignments for exclusively pinned guaranteed containers after the main
// container process starts.
func NewStaticPolicy(topology *topology.CPUTopology, numReservedCPUs int, reservedCPUs cpuset.CPUSet, affinity topologymanager.Store, cpuPolicyOptions map[string]string) (Policy, error) {
	opts, err := NewStaticPolicyOptions(cpuPolicyOptions)
	if err != nil {
		return nil, err
	}
	err = ValidateStaticPolicyOptions(opts, topology, affinity)
	if err != nil {
		return nil, err
	}

	cpuGroupSize := topology.CPUsPerCore()
	klog.InfoS("Static policy created with configuration", "options", opts, "cpuGroupSize", cpuGroupSize)

	policy := &staticPolicy{
		topology:     topology,
		affinity:     affinity,
		cpusToReuse:  make(map[string]cpuset.CPUSet),
		options:      opts,
		cpuGroupSize: cpuGroupSize,
	}

	allCPUs := topology.CPUDetails.CPUs()
	var reserved cpuset.CPUSet
	if reservedCPUs.Size() > 0 {
		reserved = reservedCPUs
	} else {
		// takeByTopology allocates CPUs associated with low-numbered cores from
		// allCPUs.
		//
		// For example: Given a system with 8 CPUs available and HT enabled,
		// if numReservedCPUs=2, then reserved={0,4}
		reserved, _ = policy.takeByTopology(allCPUs, numReservedCPUs)
	}

	if reserved.Size() != numReservedCPUs {
		err := fmt.Errorf("[cpumanager] unable to reserve the required amount of CPUs (size of %s did not equal %d)", reserved, numReservedCPUs)
		return nil, err
	}

	var reservedPhysicalCPUs cpuset.CPUSet
	for _, cpu := range reserved.UnsortedList() {
		core, err := topology.CPUCoreID(cpu)
		if err != nil {
			return nil, fmt.Errorf("[cpumanager] unable to build the reserved physical CPUs from the reserved set: %w", err)
		}
		reservedPhysicalCPUs = reservedPhysicalCPUs.Union(topology.CPUDetails.CPUsInCores(core))
	}

	klog.InfoS("Reserved CPUs not available for exclusive assignment", "reservedSize", reserved.Size(), "reserved", reserved, "reservedPhysicalCPUs", reservedPhysicalCPUs)
	policy.reservedCPUs = reserved
	policy.reservedPhysicalCPUs = reservedPhysicalCPUs

	return policy, nil
}

func (p *staticPolicy) Name() string {
	return string(PolicyStatic)
}

func (p *staticPolicy) Start(s state.State) error {
	if err := p.validateState(s); err != nil {
		klog.ErrorS(err, "Static policy invalid state, please drain node and remove policy state file")
		return err
	}
	p.initializeMetrics(s)
	return nil
}

func (p *staticPolicy) validateState(s state.State) error {
	tmpAssignments := s.GetCPUAssignments()
	tmpDefaultCPUset := s.GetDefaultCPUSet()

	allCPUs := p.topology.CPUDetails.CPUs()
	if p.options.StrictCPUReservation {
		allCPUs = allCPUs.Difference(p.reservedCPUs)
	}

	// Default cpuset cannot be empty when assignments exist
	if tmpDefaultCPUset.IsEmpty() {
		if len(tmpAssignments) != 0 {
			return fmt.Errorf("default cpuset cannot be empty")
		}
		// state is empty initialize
		s.SetDefaultCPUSet(allCPUs)
		klog.InfoS("Static policy initialized", "defaultCPUSet", allCPUs)
		return nil
	}

	// State has already been initialized from file (is not empty)
	// 1. Check if the reserved cpuset is not part of default cpuset because:
	// - kube/system reserved have changed (increased) - may lead to some containers not being able to start
	// - user tampered with file
	if p.options.StrictCPUReservation {
		if !p.reservedCPUs.Intersection(tmpDefaultCPUset).IsEmpty() {
			return fmt.Errorf("some of strictly reserved cpus: \"%s\" are present in defaultCpuSet: \"%s\"",
				p.reservedCPUs.Intersection(tmpDefaultCPUset).String(), tmpDefaultCPUset.String())
		}
	} else {
		if !p.reservedCPUs.Intersection(tmpDefaultCPUset).Equals(p.reservedCPUs) {
			return fmt.Errorf("not all reserved cpus: \"%s\" are present in defaultCpuSet: \"%s\"",
				p.reservedCPUs.String(), tmpDefaultCPUset.String())
		}
	}

	// 2. Check if state for static policy is consistent
	for pod := range tmpAssignments {
		for container, cset := range tmpAssignments[pod] {
			// None of the cpu in DEFAULT cset should be in s.assignments
			if !tmpDefaultCPUset.Intersection(cset).IsEmpty() {
				return fmt.Errorf("pod: %s, container: %s cpuset: \"%s\" overlaps with default cpuset \"%s\"",
					pod, container, cset.String(), tmpDefaultCPUset.String())
			}
		}
	}

	// 3. It's possible that the set of available CPUs has changed since
	// the state was written. This can be due to for example
	// offlining a CPU when kubelet is not running. If this happens,
	// CPU manager will run into trouble when later it tries to
	// assign non-existent CPUs to containers. Validate that the
	// topology that was received during CPU manager startup matches with
	// the set of CPUs stored in the state.
	totalKnownCPUs := tmpDefaultCPUset.Clone()
	tmpCPUSets := []cpuset.CPUSet{}
	for pod := range tmpAssignments {
		for _, cset := range tmpAssignments[pod] {
			tmpCPUSets = append(tmpCPUSets, cset)
		}
	}
	totalKnownCPUs = totalKnownCPUs.Union(tmpCPUSets...)
	if !totalKnownCPUs.Equals(allCPUs) {
		return fmt.Errorf("current set of available CPUs \"%s\" doesn't match with CPUs in state \"%s\"",
			allCPUs.String(), totalKnownCPUs.String())

	}

	return nil
}

// GetAllocatableCPUs returns the total set of CPUs available for allocation.
func (p *staticPolicy) GetAllocatableCPUs(s state.State) cpuset.CPUSet {
	return p.topology.CPUDetails.CPUs().Difference(p.reservedCPUs)
}

// GetAvailableCPUs returns the set of unassigned CPUs minus the reserved set.
func (p *staticPolicy) GetAvailableCPUs(s state.State) cpuset.CPUSet {
	return s.GetDefaultCPUSet().Difference(p.reservedCPUs)
}

func (p *staticPolicy) GetAvailablePhysicalCPUs(s state.State) cpuset.CPUSet {
	return s.GetDefaultCPUSet().Difference(p.reservedPhysicalCPUs)
}

func (p *staticPolicy) updateCPUsToReuse(pod *v1.Pod, container *v1.Container, cset cpuset.CPUSet) {
	// If pod entries to m.cpusToReuse other than the current pod exist, delete them.
	for podUID := range p.cpusToReuse {
		if podUID != string(pod.UID) {
			delete(p.cpusToReuse, podUID)
		}
	}
	// If no cpuset exists for cpusToReuse by this pod yet, create one.
	if _, ok := p.cpusToReuse[string(pod.UID)]; !ok {
		p.cpusToReuse[string(pod.UID)] = cpuset.New()
	}
	// Check if the container is an init container.
	// If so, add its cpuset to the cpuset of reusable CPUs for any new allocations.
	for _, initContainer := range pod.Spec.InitContainers {
		if container.Name == initContainer.Name {
			if podutil.IsRestartableInitContainer(&initContainer) {
				// If the container is a restartable init container, we should not
				// reuse its cpuset, as a restartable init container can run with
				// regular containers.
				break
			}
			p.cpusToReuse[string(pod.UID)] = p.cpusToReuse[string(pod.UID)].Union(cset)
			return
		}
	}
	// Otherwise it is an app container.
	// Remove its cpuset from the cpuset of reusable CPUs for any new allocations.
	p.cpusToReuse[string(pod.UID)] = p.cpusToReuse[string(pod.UID)].Difference(cset)
}

func (p *staticPolicy) Allocate(s state.State, pod *v1.Pod, container *v1.Container) (rerr error) {
	numCPUs := p.guaranteedCPUs(pod, container)
	if numCPUs == 0 {
		// container belongs in the shared pool (nothing to do; use default cpuset)
		return nil
	}

	klog.InfoS("Static policy: Allocate", "pod", klog.KObj(pod), "containerName", container.Name)
	// container belongs in an exclusively allocated pool
	metrics.CPUManagerPinningRequestsTotal.Inc()
	defer func() {
		if rerr != nil {
			metrics.CPUManagerPinningErrorsTotal.Inc()
			return
		}
		if !p.options.FullPhysicalCPUsOnly {
			// increment only if we know we allocate aligned resources
			return
		}
		metrics.ContainerAlignedComputeResources.WithLabelValues(metrics.AlignScopeContainer, metrics.AlignedPhysicalCPU).Inc()
	}()

	if p.options.FullPhysicalCPUsOnly {
		if (numCPUs % p.cpuGroupSize) != 0 {
			// Since CPU Manager has been enabled requesting strict SMT alignment, it means a guaranteed pod can only be admitted
			// if the CPU requested is a multiple of the number of virtual cpus per physical cores.
			// In case CPU request is not a multiple of the number of virtual cpus per physical cores the Pod will be put
			// in Failed state, with SMTAlignmentError as reason. Since the allocation happens in terms of physical cores
			// and the scheduler is responsible for ensuring that the workload goes to a node that has enough CPUs,
			// the pod would be placed on a node where there are enough physical cores available to be allocated.
			// Just like the behaviour in case of static policy, takeByTopology will try to first allocate CPUs from the same socket
			// and only in case the request cannot be sattisfied on a single socket, CPU allocation is done for a workload to occupy all
			// CPUs on a physical core. Allocation of individual threads would never have to occur.
			return SMTAlignmentError{
				RequestedCPUs:        numCPUs,
				CpusPerCore:          p.cpuGroupSize,
				CausedByPhysicalCPUs: false,
			}
		}

		availablePhysicalCPUs := p.GetAvailablePhysicalCPUs(s).Size()

		// It's legal to reserve CPUs which are not core siblings. In this case the CPU allocator can descend to single cores
		// when picking CPUs. This will void the guarantee of FullPhysicalCPUsOnly. To prevent this, we need to additionally consider
		// all the core siblings of the reserved CPUs as unavailable when computing the free CPUs, before to start the actual allocation.
		// This way, by construction all possible CPUs allocation whose number is multiple of the SMT level are now correct again.
		if numCPUs > availablePhysicalCPUs {
			return SMTAlignmentError{
				RequestedCPUs:         numCPUs,
				CpusPerCore:           p.cpuGroupSize,
				AvailablePhysicalCPUs: availablePhysicalCPUs,
				CausedByPhysicalCPUs:  true,
			}
		}
	}
	if cpuset, ok := s.GetCPUSet(string(pod.UID), container.Name); ok {
		p.updateCPUsToReuse(pod, container, cpuset)
		klog.InfoS("Static policy: container already present in state, skipping", "pod", klog.KObj(pod), "containerName", container.Name)
		return nil
	}

	// Call Topology Manager to get the aligned socket affinity across all hint providers.
	hint := p.affinity.GetAffinity(string(pod.UID), container.Name)
	klog.InfoS("Topology Affinity", "pod", klog.KObj(pod), "containerName", container.Name, "affinity", hint)

	// Allocate CPUs according to the NUMA affinity contained in the hint.
	cpuset, err := p.allocateCPUs(s, numCPUs, hint.NUMANodeAffinity, p.cpusToReuse[string(pod.UID)])
	if err != nil {
		klog.ErrorS(err, "Unable to allocate CPUs", "pod", klog.KObj(pod), "containerName", container.Name, "numCPUs", numCPUs)
		return err
	}

	s.SetCPUSet(string(pod.UID), container.Name, cpuset)
	p.updateCPUsToReuse(pod, container, cpuset)
	p.updateMetricsOnAllocate(cpuset)

	return nil
}

// getAssignedCPUsOfSiblings returns assigned cpus of given container's siblings(all containers other than the given container) in the given pod `podUID`.
func getAssignedCPUsOfSiblings(s state.State, podUID string, containerName string) cpuset.CPUSet {
	assignments := s.GetCPUAssignments()
	cset := cpuset.New()
	for name, cpus := range assignments[podUID] {
		if containerName == name {
			continue
		}
		cset = cset.Union(cpus)
	}
	return cset
}

func (p *staticPolicy) RemoveContainer(s state.State, podUID string, containerName string) error {
	klog.InfoS("Static policy: RemoveContainer", "podUID", podUID, "containerName", containerName)
	cpusInUse := getAssignedCPUsOfSiblings(s, podUID, containerName)
	if toRelease, ok := s.GetCPUSet(podUID, containerName); ok {
		s.Delete(podUID, containerName)
		// Mutate the shared pool, adding released cpus.
		toRelease = toRelease.Difference(cpusInUse)
		s.SetDefaultCPUSet(s.GetDefaultCPUSet().Union(toRelease))
		p.updateMetricsOnRelease(toRelease)
	}
	return nil
}

func (p *staticPolicy) allocateCPUs(s state.State, numCPUs int, numaAffinity bitmask.BitMask, reusableCPUs cpuset.CPUSet) (cpuset.CPUSet, error) {
	klog.InfoS("AllocateCPUs", "numCPUs", numCPUs, "socket", numaAffinity)

	allocatableCPUs := p.GetAvailableCPUs(s).Union(reusableCPUs)

	// If there are aligned CPUs in numaAffinity, attempt to take those first.
	result := cpuset.New()
	if numaAffinity != nil {
		alignedCPUs := p.getAlignedCPUs(numaAffinity, allocatableCPUs)

		numAlignedToAlloc := alignedCPUs.Size()
		if numCPUs < numAlignedToAlloc {
			numAlignedToAlloc = numCPUs
		}

		alignedCPUs, err := p.takeByTopology(alignedCPUs, numAlignedToAlloc)
		if err != nil {
			return cpuset.New(), err
		}

		result = result.Union(alignedCPUs)
	}

	// Get any remaining CPUs from what's leftover after attempting to grab aligned ones.
	remainingCPUs, err := p.takeByTopology(allocatableCPUs.Difference(result), numCPUs-result.Size())
	if err != nil {
		return cpuset.New(), err
	}
	result = result.Union(remainingCPUs)

	// Remove allocated CPUs from the shared CPUSet.
	s.SetDefaultCPUSet(s.GetDefaultCPUSet().Difference(result))

	klog.InfoS("AllocateCPUs", "result", result)
	return result, nil
}

func (p *staticPolicy) guaranteedCPUs(pod *v1.Pod, container *v1.Container) int {
	if v1qos.GetPodQOS(pod) != v1.PodQOSGuaranteed {
		return 0
	}
	cpuQuantity := container.Resources.Requests[v1.ResourceCPU]
	// In-place pod resize feature makes Container.Resources field mutable for CPU & memory.
	// AllocatedResources holds the value of Container.Resources.Requests when the pod was admitted.
	// We should return this value because this is what kubelet agreed to allocate for the container
	// and the value configured with runtime.
	if utilfeature.DefaultFeatureGate.Enabled(features.InPlacePodVerticalScaling) {
		containerStatuses := pod.Status.ContainerStatuses
		if utilfeature.DefaultFeatureGate.Enabled(features.SidecarContainers) && podutil.IsRestartableInitContainer(container) {
			if len(pod.Status.InitContainerStatuses) != 0 {
				containerStatuses = append(containerStatuses, pod.Status.InitContainerStatuses...)
			}
		}
		if cs, ok := podutil.GetContainerStatus(containerStatuses, container.Name); ok {
			cpuQuantity = cs.AllocatedResources[v1.ResourceCPU]
		}
	}
	if cpuQuantity.Value()*1000 != cpuQuantity.MilliValue() {
		return 0
	}
	// Safe downcast to do for all systems with < 2.1 billion CPUs.
	// Per the language spec, `int` is guaranteed to be at least 32 bits wide.
	// https://golang.org/ref/spec#Numeric_types
	return int(cpuQuantity.Value())
}

func (p *staticPolicy) podGuaranteedCPUs(pod *v1.Pod) int {
	// The maximum of requested CPUs by init containers.
	requestedByInitContainers := 0
	requestedByRestartableInitContainers := 0
	for _, container := range pod.Spec.InitContainers {
		if _, ok := container.Resources.Requests[v1.ResourceCPU]; !ok {
			continue
		}
		requestedCPU := p.guaranteedCPUs(pod, &container)
		// See https://github.com/kubernetes/enhancements/tree/master/keps/sig-node/753-sidecar-containers#resources-calculation-for-scheduling-and-pod-admission
		// for the detail.
		if podutil.IsRestartableInitContainer(&container) {
			requestedByRestartableInitContainers += requestedCPU
		} else if requestedByRestartableInitContainers+requestedCPU > requestedByInitContainers {
			requestedByInitContainers = requestedByRestartableInitContainers + requestedCPU
		}
	}

	// The sum of requested CPUs by app containers.
	requestedByAppContainers := 0
	for _, container := range pod.Spec.Containers {
		if _, ok := container.Resources.Requests[v1.ResourceCPU]; !ok {
			continue
		}
		requestedByAppContainers += p.guaranteedCPUs(pod, &container)
	}

	requestedByLongRunningContainers := requestedByAppContainers + requestedByRestartableInitContainers
	if requestedByInitContainers > requestedByLongRunningContainers {
		return requestedByInitContainers
	}
	return requestedByLongRunningContainers
}

func (p *staticPolicy) takeByTopology(availableCPUs cpuset.CPUSet, numCPUs int) (cpuset.CPUSet, error) {
	cpuSortingStrategy := CPUSortingStrategyPacked
	if p.options.DistributeCPUsAcrossCores {
		cpuSortingStrategy = CPUSortingStrategySpread
	}

	if p.options.DistributeCPUsAcrossNUMA {
		cpuGroupSize := 1
		if p.options.FullPhysicalCPUsOnly {
			cpuGroupSize = p.cpuGroupSize
		}
		return takeByTopologyNUMADistributed(p.topology, availableCPUs, numCPUs, cpuGroupSize, cpuSortingStrategy)
	}

	return takeByTopologyNUMAPacked(p.topology, availableCPUs, numCPUs, cpuSortingStrategy, p.options.PreferAlignByUncoreCacheOption)
}

func (p *staticPolicy) GetTopologyHints(s state.State, pod *v1.Pod, container *v1.Container) map[string][]topologymanager.TopologyHint {
	// Get a count of how many guaranteed CPUs have been requested.
	requested := p.guaranteedCPUs(pod, container)

	// Number of required CPUs is not an integer or a container is not part of the Guaranteed QoS class.
	// It will be treated by the TopologyManager as having no preference and cause it to ignore this
	// resource when considering pod alignment.
	// In terms of hints, this is equal to: TopologyHints[NUMANodeAffinity: nil, Preferred: true].
	if requested == 0 {
		return nil
	}

	// Short circuit to regenerate the same hints if there are already
	// guaranteed CPUs allocated to the Container. This might happen after a
	// kubelet restart, for example.
	if allocated, exists := s.GetCPUSet(string(pod.UID), container.Name); exists {
		if allocated.Size() != requested {
			klog.InfoS("CPUs already allocated to container with different number than request", "pod", klog.KObj(pod), "containerName", container.Name, "requestedSize", requested, "allocatedSize", allocated.Size())
			// An empty list of hints will be treated as a preference that cannot be satisfied.
			// In definition of hints this is equal to: TopologyHint[NUMANodeAffinity: nil, Preferred: false].
			// For all but the best-effort policy, the Topology Manager will throw a pod-admission error.
			return map[string][]topologymanager.TopologyHint{
				string(v1.ResourceCPU): {},
			}
		}
		klog.InfoS("Regenerating TopologyHints for CPUs already allocated", "pod", klog.KObj(pod), "containerName", container.Name)
		return map[string][]topologymanager.TopologyHint{
			string(v1.ResourceCPU): p.generateCPUTopologyHints(allocated, cpuset.CPUSet{}, requested),
		}
	}

	// Get a list of available CPUs.
	available := p.GetAvailableCPUs(s)

	// Get a list of reusable CPUs (e.g. CPUs reused from initContainers).
	// It should be an empty CPUSet for a newly created pod.
	reusable := p.cpusToReuse[string(pod.UID)]

	// Generate hints.
	cpuHints := p.generateCPUTopologyHints(available, reusable, requested)
	klog.InfoS("TopologyHints generated", "pod", klog.KObj(pod), "containerName", container.Name, "cpuHints", cpuHints)

	return map[string][]topologymanager.TopologyHint{
		string(v1.ResourceCPU): cpuHints,
	}
}

func (p *staticPolicy) GetPodTopologyHints(s state.State, pod *v1.Pod) map[string][]topologymanager.TopologyHint {
	// Get a count of how many guaranteed CPUs have been requested by Pod.
	requested := p.podGuaranteedCPUs(pod)

	// Number of required CPUs is not an integer or a pod is not part of the Guaranteed QoS class.
	// It will be treated by the TopologyManager as having no preference and cause it to ignore this
	// resource when considering pod alignment.
	// In terms of hints, this is equal to: TopologyHints[NUMANodeAffinity: nil, Preferred: true].
	if requested == 0 {
		return nil
	}

	assignedCPUs := cpuset.New()
	for _, container := range append(pod.Spec.InitContainers, pod.Spec.Containers...) {
		requestedByContainer := p.guaranteedCPUs(pod, &container)
		// Short circuit to regenerate the same hints if there are already
		// guaranteed CPUs allocated to the Container. This might happen after a
		// kubelet restart, for example.
		if allocated, exists := s.GetCPUSet(string(pod.UID), container.Name); exists {
			if allocated.Size() != requestedByContainer {
				klog.InfoS("CPUs already allocated to container with different number than request", "pod", klog.KObj(pod), "containerName", container.Name, "allocatedSize", requested, "requestedByContainer", requestedByContainer, "allocatedSize", allocated.Size())
				// An empty list of hints will be treated as a preference that cannot be satisfied.
				// In definition of hints this is equal to: TopologyHint[NUMANodeAffinity: nil, Preferred: false].
				// For all but the best-effort policy, the Topology Manager will throw a pod-admission error.
				return map[string][]topologymanager.TopologyHint{
					string(v1.ResourceCPU): {},
				}
			}
			// A set of CPUs already assigned to containers in this pod
			assignedCPUs = assignedCPUs.Union(allocated)
		}
	}
	if assignedCPUs.Size() == requested {
		klog.InfoS("Regenerating TopologyHints for CPUs already allocated", "pod", klog.KObj(pod))
		return map[string][]topologymanager.TopologyHint{
			string(v1.ResourceCPU): p.generateCPUTopologyHints(assignedCPUs, cpuset.CPUSet{}, requested),
		}
	}

	// Get a list of available CPUs.
	available := p.GetAvailableCPUs(s)

	// Get a list of reusable CPUs (e.g. CPUs reused from initContainers).
	// It should be an empty CPUSet for a newly created pod.
	reusable := p.cpusToReuse[string(pod.UID)]

	// Ensure any CPUs already assigned to containers in this pod are included as part of the hint generation.
	reusable = reusable.Union(assignedCPUs)

	// Generate hints.
	cpuHints := p.generateCPUTopologyHints(available, reusable, requested)
	klog.InfoS("TopologyHints generated", "pod", klog.KObj(pod), "cpuHints", cpuHints)

	return map[string][]topologymanager.TopologyHint{
		string(v1.ResourceCPU): cpuHints,
	}
}

// generateCPUTopologyHints generates a set of TopologyHints given the set of
// available CPUs and the number of CPUs being requested.
//
// It follows the convention of marking all hints that have the same number of
// bits set as the narrowest matching NUMANodeAffinity with 'Preferred: true', and
// marking all others with 'Preferred: false'.
func (p *staticPolicy) generateCPUTopologyHints(availableCPUs cpuset.CPUSet, reusableCPUs cpuset.CPUSet, request int) []topologymanager.TopologyHint {
	// Initialize minAffinitySize to include all NUMA Nodes.
	minAffinitySize := p.topology.CPUDetails.NUMANodes().Size()

	// Iterate through all combinations of numa nodes bitmask and build hints from them.
	hints := []topologymanager.TopologyHint{}
	bitmask.IterateBitMasks(p.topology.CPUDetails.NUMANodes().List(), func(mask bitmask.BitMask) {
		// First, update minAffinitySize for the current request size.
		cpusInMask := p.topology.CPUDetails.CPUsInNUMANodes(mask.GetBits()...).Size()
		if cpusInMask >= request && mask.Count() < minAffinitySize {
			minAffinitySize = mask.Count()
		}

		// Then check to see if we have enough CPUs available on the current
		// numa node bitmask to satisfy the CPU request.
		numMatching := 0
		for _, c := range reusableCPUs.List() {
			// Disregard this mask if its NUMANode isn't part of it.
			if !mask.IsSet(p.topology.CPUDetails[c].NUMANodeID) {
				return
			}
			numMatching++
		}

		// Finally, check to see if enough available CPUs remain on the current
		// NUMA node combination to satisfy the CPU request.
		for _, c := range availableCPUs.List() {
			if mask.IsSet(p.topology.CPUDetails[c].NUMANodeID) {
				numMatching++
			}
		}

		// If they don't, then move onto the next combination.
		if numMatching < request {
			return
		}

		// Otherwise, create a new hint from the numa node bitmask and add it to the
		// list of hints.  We set all hint preferences to 'false' on the first
		// pass through.
		hints = append(hints, topologymanager.TopologyHint{
			NUMANodeAffinity: mask,
			Preferred:        false,
		})
	})

	// Loop back through all hints and update the 'Preferred' field based on
	// counting the number of bits sets in the affinity mask and comparing it
	// to the minAffinitySize. Only those with an equal number of bits set (and
	// with a minimal set of numa nodes) will be considered preferred.
	for i := range hints {
		if p.options.AlignBySocket && p.isHintSocketAligned(hints[i], minAffinitySize) {
			hints[i].Preferred = true
			continue
		}
		if hints[i].NUMANodeAffinity.Count() == minAffinitySize {
			hints[i].Preferred = true
		}
	}

	return hints
}

// isHintSocketAligned function return true if numa nodes in hint are socket aligned.
func (p *staticPolicy) isHintSocketAligned(hint topologymanager.TopologyHint, minAffinitySize int) bool {
	numaNodesBitMask := hint.NUMANodeAffinity.GetBits()
	numaNodesPerSocket := p.topology.NumNUMANodes / p.topology.NumSockets
	if numaNodesPerSocket == 0 {
		return false
	}
	// minSockets refers to minimum number of socket required to satify allocation.
	// A hint is considered socket aligned if sockets across which numa nodes span is equal to minSockets
	minSockets := (minAffinitySize + numaNodesPerSocket - 1) / numaNodesPerSocket
	return p.topology.CPUDetails.SocketsInNUMANodes(numaNodesBitMask...).Size() == minSockets
}

// getAlignedCPUs return set of aligned CPUs based on numa affinity mask and configured policy options.
func (p *staticPolicy) getAlignedCPUs(numaAffinity bitmask.BitMask, allocatableCPUs cpuset.CPUSet) cpuset.CPUSet {
	alignedCPUs := cpuset.New()
	numaBits := numaAffinity.GetBits()

	// If align-by-socket policy option is enabled, NUMA based hint is expanded to
	// socket aligned hint. It will ensure that first socket aligned available CPUs are
	// allocated before we try to find CPUs across socket to satisfy allocation request.
	if p.options.AlignBySocket {
		socketBits := p.topology.CPUDetails.SocketsInNUMANodes(numaBits...).UnsortedList()
		for _, socketID := range socketBits {
			alignedCPUs = alignedCPUs.Union(allocatableCPUs.Intersection(p.topology.CPUDetails.CPUsInSockets(socketID)))
		}
		return alignedCPUs
	}

	for _, numaNodeID := range numaBits {
		alignedCPUs = alignedCPUs.Union(allocatableCPUs.Intersection(p.topology.CPUDetails.CPUsInNUMANodes(numaNodeID)))
	}

	return alignedCPUs
}

func (p *staticPolicy) initializeMetrics(s state.State) {
	metrics.CPUManagerSharedPoolSizeMilliCores.Set(float64(p.GetAvailableCPUs(s).Size() * 1000))
	metrics.CPUManagerExclusiveCPUsAllocationCount.Set(float64(countExclusiveCPUs(s)))
}

func (p *staticPolicy) updateMetricsOnAllocate(cset cpuset.CPUSet) {
	ncpus := cset.Size()
	metrics.CPUManagerExclusiveCPUsAllocationCount.Add(float64(ncpus))
	metrics.CPUManagerSharedPoolSizeMilliCores.Add(float64(-ncpus * 1000))
}

func (p *staticPolicy) updateMetricsOnRelease(cset cpuset.CPUSet) {
	ncpus := cset.Size()
	metrics.CPUManagerExclusiveCPUsAllocationCount.Add(float64(-ncpus))
	metrics.CPUManagerSharedPoolSizeMilliCores.Add(float64(ncpus * 1000))
}

func countExclusiveCPUs(s state.State) int {
	exclusiveCPUs := 0
	for _, cpuAssign := range s.GetCPUAssignments() {
		for _, cset := range cpuAssign {
			exclusiveCPUs += cset.Size()
		}
	}
	return exclusiveCPUs
}
