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
	"maps"
	"math"
	"sort"

	"k8s.io/klog/v2"

	"k8s.io/kubernetes/pkg/kubelet/cm/cpumanager/topology"
	"k8s.io/utils/cpuset"
)

// LoopControl controls the behavior of the cpu accumulator loop logic
type LoopControl int

// Possible loop control outcomes
const (
	Continue LoopControl = iota
	Break
)

type mapIntInt map[int]int

func (m mapIntInt) Clone() mapIntInt {
	return maps.Clone(m)
}

func (m mapIntInt) Keys() []int {
	var keys []int
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

func (m mapIntInt) Values(keys ...int) []int {
	if keys == nil {
		keys = m.Keys()
	}
	var values []int
	for _, k := range keys {
		values = append(values, m[k])
	}
	return values
}

func sum(xs []int) int {
	var s int
	for _, x := range xs {
		s += x
	}
	return s
}

func mean(xs []int) float64 {
	var sum float64
	for _, x := range xs {
		sum += float64(x)
	}
	m := sum / float64(len(xs))
	return math.Round(m*1000) / 1000
}

func standardDeviation(xs []int) float64 {
	m := mean(xs)
	var sum float64
	for _, x := range xs {
		sum += (float64(x) - m) * (float64(x) - m)
	}
	s := math.Sqrt(sum / float64(len(xs)))
	return math.Round(s*1000) / 1000
}

type numaOrSocketsFirstFuncs interface {
	takeFullFirstLevel()
	takeFullSecondLevel()
	sortAvailableNUMANodes() []int
	sortAvailableSockets() []int
	sortAvailableCores() []int
}

type numaFirst struct{ acc *cpuAccumulator }
type socketsFirst struct{ acc *cpuAccumulator }

var _ numaOrSocketsFirstFuncs = (*numaFirst)(nil)
var _ numaOrSocketsFirstFuncs = (*socketsFirst)(nil)

// If NUMA nodes are higher in the memory hierarchy than sockets, then we take
// from the set of NUMA Nodes as the first level.
func (n *numaFirst) takeFullFirstLevel() {
	n.acc.takeFullNUMANodes()
}

// If NUMA nodes are higher in the memory hierarchy than sockets, then we take
// from the set of sockets as the second level.
func (n *numaFirst) takeFullSecondLevel() {
	n.acc.takeFullSockets()
}

// Sort the UncoreCaches within the NUMA nodes.
func (a *cpuAccumulator) sortAvailableUncoreCaches() []int {
	var result []int
	for _, numa := range a.sortAvailableNUMANodes() {
		uncore := a.details.UncoreInNUMANodes(numa).UnsortedList()
		a.sort(uncore, a.details.CPUsInUncoreCaches)
		result = append(result, uncore...)
	}
	return result
}

// If NUMA nodes are higher in the memory hierarchy than sockets, then just
// sort the NUMA nodes directly, and return them.
func (n *numaFirst) sortAvailableNUMANodes() []int {
	numas := n.acc.details.NUMANodes().UnsortedList()
	n.acc.sort(numas, n.acc.details.CPUsInNUMANodes)
	return numas
}

// If NUMA nodes are higher in the memory hierarchy than sockets, then we need
// to pull the set of sockets out of each sorted NUMA node, and accumulate the
// partial order across them.
func (n *numaFirst) sortAvailableSockets() []int {
	var result []int
	for _, numa := range n.sortAvailableNUMANodes() {
		sockets := n.acc.details.SocketsInNUMANodes(numa).UnsortedList()
		n.acc.sort(sockets, n.acc.details.CPUsInSockets)
		result = append(result, sockets...)
	}
	return result
}

// If NUMA nodes are higher in the memory hierarchy than sockets, then
// cores sit directly below sockets in the memory hierarchy.
func (n *numaFirst) sortAvailableCores() []int {
	var result []int
	for _, socket := range n.acc.sortAvailableSockets() {
		cores := n.acc.details.CoresInSockets(socket).UnsortedList()
		n.acc.sort(cores, n.acc.details.CPUsInCores)
		result = append(result, cores...)
	}
	return result
}

// If sockets are higher in the memory hierarchy than NUMA nodes, then we take
// from the set of sockets as the first level.
func (s *socketsFirst) takeFullFirstLevel() {
	s.acc.takeFullSockets()
}

// If sockets are higher in the memory hierarchy than NUMA nodes, then we take
// from the set of NUMA Nodes as the second level.
func (s *socketsFirst) takeFullSecondLevel() {
	s.acc.takeFullNUMANodes()
}

// If sockets are higher in the memory hierarchy than NUMA nodes, then we need
// to pull the set of NUMA nodes out of each sorted Socket, and accumulate the
// partial order across them.
func (s *socketsFirst) sortAvailableNUMANodes() []int {
	var result []int
	for _, socket := range s.sortAvailableSockets() {
		numas := s.acc.details.NUMANodesInSockets(socket).UnsortedList()
		s.acc.sort(numas, s.acc.details.CPUsInNUMANodes)
		result = append(result, numas...)
	}
	return result
}

// If sockets are higher in the memory hierarchy than NUMA nodes, then just
// sort the sockets directly, and return them.
func (s *socketsFirst) sortAvailableSockets() []int {
	sockets := s.acc.details.Sockets().UnsortedList()
	s.acc.sort(sockets, s.acc.details.CPUsInSockets)
	return sockets
}

// If sockets are higher in the memory hierarchy than NUMA nodes, then cores
// sit directly below NUMA Nodes in the memory hierarchy.
func (s *socketsFirst) sortAvailableCores() []int {
	var result []int
	for _, numa := range s.acc.sortAvailableNUMANodes() {
		cores := s.acc.details.CoresInNUMANodes(numa).UnsortedList()
		s.acc.sort(cores, s.acc.details.CPUsInCores)
		result = append(result, cores...)
	}
	return result
}

type availableCPUSorter interface {
	sort() []int
}

type sortCPUsPacked struct{ acc *cpuAccumulator }
type sortCPUsSpread struct{ acc *cpuAccumulator }

var _ availableCPUSorter = (*sortCPUsPacked)(nil)
var _ availableCPUSorter = (*sortCPUsSpread)(nil)

func (s sortCPUsPacked) sort() []int {
	return s.acc.sortAvailableCPUsPacked()
}

func (s sortCPUsSpread) sort() []int {
	return s.acc.sortAvailableCPUsSpread()
}

// CPUSortingStrategy describes the CPU sorting solution within the socket scope.
// Using topoDualSocketHT (12 CPUs, 2 sockets, 6 cores) as an example:
//
//	CPUDetails: map[int]topology.CPUInfo{
//		0:  {CoreID: 0, SocketID: 0, NUMANodeID: 0},
//		1:  {CoreID: 1, SocketID: 1, NUMANodeID: 1},
//		2:  {CoreID: 2, SocketID: 0, NUMANodeID: 0},
//		3:  {CoreID: 3, SocketID: 1, NUMANodeID: 1},
//		4:  {CoreID: 4, SocketID: 0, NUMANodeID: 0},
//		5:  {CoreID: 5, SocketID: 1, NUMANodeID: 1},
//		6:  {CoreID: 0, SocketID: 0, NUMANodeID: 0},
//		7:  {CoreID: 1, SocketID: 1, NUMANodeID: 1},
//		8:  {CoreID: 2, SocketID: 0, NUMANodeID: 0},
//		9:  {CoreID: 3, SocketID: 1, NUMANodeID: 1},
//		10: {CoreID: 4, SocketID: 0, NUMANodeID: 0},
//		11: {CoreID: 5, SocketID: 1, NUMANodeID: 1},
//	},
//
// - CPUSortingOptionPacked sorts CPUs in a packed manner, where CPUs are grouped by core
// before moving to the next core, resulting in packed cores, like:
// 0, 2, 4, 6, 8, 10, 1, 3, 5, 7, 9, 11
// - CPUSortingOptionSpread sorts CPUs in a spread manner, where CPUs are spread across cores
// before moving to the next CPU, resulting in spread-out cores, like:
// 0, 6, 2, 8, 4, 10, 1, 7, 3, 9, 5, 11
//
// By default, CPUSortingOptionPacked will be used, and CPUSortingOptionSpread will only be activated
// when the user specifies the `DistributeCPUsAcrossCoresOption` static policy option.
type CPUSortingStrategy string

const (
	CPUSortingStrategyPacked CPUSortingStrategy = "packed"
	CPUSortingStrategySpread CPUSortingStrategy = "spread"
)

type cpuAccumulator struct {
	// `topo` describes the layout of CPUs (i.e. hyper-threads if hyperthreading is on) between
	// cores (i.e. physical CPUs if hyper-threading is on), NUMA nodes, and sockets on the K8s
	// cluster node. `topo` is never mutated, meaning that as the cpuAccumulator claims CPUs topo is
	// not modified. Its primary purpose is being a reference of the original (i.e. at the time the
	// cpuAccumulator was created) topology to learn things such as how many CPUs are on each
	// socket, NUMA node, etc... .
	topo *topology.CPUTopology

	// `details` is the set of free CPUs that the cpuAccumulator can claim to accumulate the desired
	// number of CPUS. When a CPU is claimed, it's removed from `details`.
	details topology.CPUDetails

	// `numCPUsNeeded` is the number of CPUs that the accumulator still needs to accumulate to reach
	// the desired number of CPUs. When the cpuAccumulator is created, `numCPUsNeeded` is set to the
	// total number of CPUs to accumulate. Every time a CPU is claimed, `numCPUsNeeded` is decreased
	// by 1 until it has value 0, meaning that all the needed CPUs have been accumulated
	// (success), or a situation where it's bigger than 0 but no more CPUs are available is reached
	// (failure).
	numCPUsNeeded int

	// `result` is the set of CPUs that have been accumulated so far. When a CPU is claimed, it's
	// added to `result`. The cpuAccumulator completed its duty successfully when `result` has
	// cardinality equal to the total number of CPUs to accumulate.
	result cpuset.CPUSet

	numaOrSocketsFirst numaOrSocketsFirstFuncs

	// availableCPUSorter is used to control the cpu sorting result.
	// The sequence of returned CPU IDs depends on the policy.
	// By default, cpus is sorted by sortAvailableCPUsPacked()
	// If packed is false, cpu is sorted by sortAvailableCPUsSpread()
	availableCPUSorter availableCPUSorter
}

func newCPUAccumulator(topo *topology.CPUTopology, availableCPUs cpuset.CPUSet, numCPUs int, cpuSortingStrategy CPUSortingStrategy, reusableCPUsForResize *cpuset.CPUSet, mustKeepCPUsForScaleDown *cpuset.CPUSet) *cpuAccumulator {
	acc := &cpuAccumulator{
		topo:          topo,
		details:       topo.CPUDetails.KeepOnly(availableCPUs),
		numCPUsNeeded: numCPUs,
		result:        cpuset.New(),
	}

	if reusableCPUsForResize != nil {
		if !reusableCPUsForResize.IsEmpty() {
			// Increase of CPU resources ( scale up )
			// Take existing from allocated
			// CPUs
			if numCPUs > reusableCPUsForResize.Size() {
				// scale up ...
				acc.take(reusableCPUsForResize.Clone())
			}

			// Decrease of CPU resources ( scale down )
			// Take delta from allocated CPUs, if mustKeepCPUsForScaleDown
			// is not nil, use explicetely those. If it is nil
			// take delta starting from lowest CoreId of CPUs ( TODO esotsal, perhaps not needed).
			if numCPUs < reusableCPUsForResize.Size() {
				if mustKeepCPUsForScaleDown != nil {
					// If explicetely CPUs to keep
					// during scale down is given ( this requires
					// addition in container[].resources ... which
					// could be possible to patch ? Esotsal Note This means
					// modifying API code
					if !(mustKeepCPUsForScaleDown.Intersection(reusableCPUsForResize.Clone())).IsEmpty() {
						acc.take(mustKeepCPUsForScaleDown.Clone())
					} else {
						return acc
					}
				}
			}

			if numCPUs == reusableCPUsForResize.Size() {
				// nothing to do return as is
				acc.take(reusableCPUsForResize.Clone())
				return acc
			}
		}
	}

	if topo.NumSockets >= topo.NumNUMANodes {
		acc.numaOrSocketsFirst = &numaFirst{acc}
	} else {
		acc.numaOrSocketsFirst = &socketsFirst{acc}
	}

	if cpuSortingStrategy == CPUSortingStrategyPacked {
		acc.availableCPUSorter = &sortCPUsPacked{acc}
	} else {
		acc.availableCPUSorter = &sortCPUsSpread{acc}
	}

	return acc
}

// Returns true if the supplied NUMANode is fully available in `a.details`.
// "fully available" means that all the CPUs in it are free.
func (a *cpuAccumulator) isNUMANodeFree(numaID int) bool {
	return a.details.CPUsInNUMANodes(numaID).Size() == a.topo.CPUDetails.CPUsInNUMANodes(numaID).Size()
}

// Returns true if the supplied socket is fully available in `a.details`.
// "fully available" means that all the CPUs in it are free.
func (a *cpuAccumulator) isSocketFree(socketID int) bool {
	return a.details.CPUsInSockets(socketID).Size() == a.topo.CPUsPerSocket()
}

// Returns true if the supplied UnCoreCache is fully available,
// "fully available" means that all the CPUs in it are free.
func (a *cpuAccumulator) isUncoreCacheFree(uncoreID int) bool {
	return a.details.CPUsInUncoreCaches(uncoreID).Size() == a.topo.CPUDetails.CPUsInUncoreCaches(uncoreID).Size()
}

// Returns true if the supplied core is fully available in `a.details`.
// "fully available" means that all the CPUs in it are free.
func (a *cpuAccumulator) isCoreFree(coreID int) bool {
	return a.details.CPUsInCores(coreID).Size() == a.topo.CPUsPerCore()
}

// Returns free NUMA Node IDs as a slice sorted by sortAvailableNUMANodes().
func (a *cpuAccumulator) freeNUMANodes() []int {
	free := []int{}
	for _, numa := range a.sortAvailableNUMANodes() {
		if a.isNUMANodeFree(numa) {
			free = append(free, numa)
		}
	}
	return free
}

// Returns free socket IDs as a slice sorted by sortAvailableSockets().
func (a *cpuAccumulator) freeSockets() []int {
	free := []int{}
	for _, socket := range a.sortAvailableSockets() {
		if a.isSocketFree(socket) {
			free = append(free, socket)
		}
	}
	return free
}

// Returns free UncoreCache IDs as a slice sorted by sortAvailableUnCoreCache().
func (a *cpuAccumulator) freeUncoreCache() []int {
	free := []int{}
	for _, uncore := range a.sortAvailableUncoreCaches() {
		if a.isUncoreCacheFree(uncore) {
			free = append(free, uncore)
		}
	}
	return free
}

// Returns free core IDs as a slice sorted by sortAvailableCores().
func (a *cpuAccumulator) freeCores() []int {
	free := []int{}
	for _, core := range a.sortAvailableCores() {
		if a.isCoreFree(core) {
			free = append(free, core)
		}
	}
	return free
}

// Returns free CPU IDs as a slice sorted by sortAvailableCPUsPacked().
func (a *cpuAccumulator) freeCPUs() []int {
	return a.availableCPUSorter.sort()
}

// Sorts the provided list of NUMA nodes/sockets/cores/cpus referenced in 'ids'
// by the number of available CPUs contained within them (smallest to largest).
// The 'getCPU()' parameter defines the function that should be called to
// retrieve the list of available CPUs for the type being referenced. If two
// NUMA nodes/sockets/cores/cpus have the same number of available CPUs, they
// are sorted in ascending order by their id.
func (a *cpuAccumulator) sort(ids []int, getCPUs func(ids ...int) cpuset.CPUSet) {
	sort.Slice(ids,
		func(i, j int) bool {
			iCPUs := getCPUs(ids[i])
			jCPUs := getCPUs(ids[j])
			if iCPUs.Size() < jCPUs.Size() {
				return true
			}
			if iCPUs.Size() > jCPUs.Size() {
				return false
			}
			return ids[i] < ids[j]
		})
}

// Sort all NUMA nodes with at least one free CPU.
//
// If NUMA nodes are higher than sockets in the memory hierarchy, they are sorted by ascending number
// of free CPUs that they contain. "higher than sockets in the memory hierarchy" means that NUMA nodes
// contain a bigger number of CPUs (free and busy) than sockets, or equivalently that each NUMA node
// contains more than one socket.
//
// If instead NUMA nodes are lower in the memory hierarchy than sockets, they are sorted as follows.
// First, they are sorted by number of free CPUs in the sockets that contain them. Then, for each
// socket they are sorted by number of free CPUs that they contain. The order is always ascending.
// In other words, the relative order of two NUMA nodes is determined as follows:
//  1. If the two NUMA nodes belong to different sockets, the NUMA node in the socket with the
//     smaller amount of free CPUs appears first.
//  2. If the two NUMA nodes belong to the same socket, the NUMA node with the smaller amount of free
//     CPUs appears first.
func (a *cpuAccumulator) sortAvailableNUMANodes() []int {
	return a.numaOrSocketsFirst.sortAvailableNUMANodes()
}

// Sort all sockets with at least one free CPU.
//
// If sockets are higher than NUMA nodes in the memory hierarchy, they are sorted by ascending number
// of free CPUs that they contain. "higher than NUMA nodes in the memory hierarchy" means that
// sockets contain a bigger number of CPUs (free and busy) than NUMA nodes, or equivalently that each
// socket contains more than one NUMA node.
//
// If instead sockets are lower in the memory hierarchy than NUMA nodes, they are sorted as follows.
// First, they are sorted by number of free CPUs in the NUMA nodes that contain them. Then, for each
// NUMA node they are sorted by number of free CPUs that they contain. The order is always ascending.
// In other words, the relative order of two sockets is determined as follows:
//  1. If the two sockets belong to different NUMA nodes, the socket in the NUMA node with the
//     smaller amount of free CPUs appears first.
//  2. If the two sockets belong to the same NUMA node, the socket with the smaller amount of free
//     CPUs appears first.
func (a *cpuAccumulator) sortAvailableSockets() []int {
	return a.numaOrSocketsFirst.sortAvailableSockets()
}

// Sort all cores with at least one free CPU.
//
// If sockets are higher in the memory hierarchy than NUMA nodes, meaning that sockets contain a
// bigger number of CPUs (free and busy) than NUMA nodes, or equivalently that each socket contains
// more than one NUMA node, the cores are sorted as follows. First, they are sorted by number of
// free CPUs that their sockets contain. Then, for each socket, the cores in it are sorted by number
// of free CPUs that their NUMA nodes contain. Then, for each NUMA node, the cores in it are sorted
// by number of free CPUs that they contain. The order is always ascending. In other words, the
// relative order of two cores is determined as follows:
//  1. If the two cores belong to different sockets, the core in the socket with the smaller amount of
//     free CPUs appears first.
//  2. If the two cores belong to the same socket but different NUMA nodes, the core in the NUMA node
//     with the smaller amount of free CPUs appears first.
//  3. If the two cores belong to the same NUMA node and socket, the core with the smaller amount of
//     free CPUs appears first.
//
// If instead NUMA nodes are higher in the memory hierarchy than sockets, the sorting happens in the
// same way as described in the previous paragraph, except that the priority of NUMA nodes and
// sockets is inverted (e.g. first sort the cores by number of free CPUs in their NUMA nodes, then,
// for each NUMA node, sort the cores by number of free CPUs in their sockets, etc...).
func (a *cpuAccumulator) sortAvailableCores() []int {
	return a.numaOrSocketsFirst.sortAvailableCores()
}

// Sort all free CPUs.
//
// If sockets are higher in the memory hierarchy than NUMA nodes, meaning that sockets contain a
// bigger number of CPUs (free and busy) than NUMA nodes, or equivalently that each socket contains
// more than one NUMA node, the CPUs are sorted as follows. First, they are sorted by number of
// free CPUs that their sockets contain. Then, for each socket, the CPUs in it are sorted by number
// of free CPUs that their NUMA nodes contain. Then, for each NUMA node, the CPUs in it are sorted
// by number of free CPUs that their cores contain. Finally, for each core, the CPUs in it are
// sorted by numerical ID. The order is always ascending. In other words, the relative order of two
// CPUs is determined as follows:
//  1. If the two CPUs belong to different sockets, the CPU in the socket with the smaller amount of
//     free CPUs appears first.
//  2. If the two CPUs belong to the same socket but different NUMA nodes, the CPU in the NUMA node
//     with the smaller amount of free CPUs appears first.
//  3. If the two CPUs belong to the same socket and NUMA node but different cores, the CPU in the
//     core with the smaller amount of free CPUs appears first.
//  4. If the two CPUs belong to the same NUMA node, socket, and core, the CPU with the smaller ID
//     appears first.
//
// If instead NUMA nodes are higher in the memory hierarchy than sockets, the sorting happens in the
// same way as described in the previous paragraph, except that the priority of NUMA nodes and
// sockets is inverted (e.g. first sort the CPUs by number of free CPUs in their NUMA nodes, then,
// for each NUMA node, sort the CPUs by number of free CPUs in their sockets, etc...).
func (a *cpuAccumulator) sortAvailableCPUsPacked() []int {
	var result []int
	for _, core := range a.sortAvailableCores() {
		cpus := a.details.CPUsInCores(core).UnsortedList()
		sort.Ints(cpus)
		result = append(result, cpus...)
	}
	return result
}

// Sort all available CPUs:
// - First by core using sortAvailableSockets().
// - Then within each socket, sort cpus directly using the sort() algorithm defined above.
func (a *cpuAccumulator) sortAvailableCPUsSpread() []int {
	var result []int
	for _, socket := range a.sortAvailableSockets() {
		cpus := a.details.CPUsInSockets(socket).UnsortedList()
		sort.Ints(cpus)
		result = append(result, cpus...)
	}
	return result
}

func (a *cpuAccumulator) take(cpus cpuset.CPUSet) {
	a.result = a.result.Union(cpus)
	a.details = a.details.KeepOnly(a.details.CPUs().Difference(a.result))
	a.numCPUsNeeded -= cpus.Size()
}

func (a *cpuAccumulator) takeFullNUMANodes() {
	for _, numa := range a.freeNUMANodes() {
		cpusInNUMANode := a.topo.CPUDetails.CPUsInNUMANodes(numa)
		if !a.needsAtLeast(cpusInNUMANode.Size()) {
			continue
		}
		klog.V(4).InfoS("takeFullNUMANodes: claiming NUMA node", "numa", numa)
		a.take(cpusInNUMANode)
	}
}

func (a *cpuAccumulator) takeFullSockets() {
	for _, socket := range a.freeSockets() {
		cpusInSocket := a.topo.CPUDetails.CPUsInSockets(socket)
		if !a.needsAtLeast(cpusInSocket.Size()) {
			continue
		}
		klog.V(4).InfoS("takeFullSockets: claiming socket", "socket", socket)
		a.take(cpusInSocket)
	}
}

func (a *cpuAccumulator) takeFullUncore() {
	for _, uncore := range a.freeUncoreCache() {
		cpusInUncore := a.topo.CPUDetails.CPUsInUncoreCaches(uncore)
		if !a.needsAtLeast(cpusInUncore.Size()) {
			continue
		}
		klog.V(4).InfoS("takeFullUncore: claiming uncore", "uncore", uncore)
		a.take(cpusInUncore)
	}
}

func (a *cpuAccumulator) takePartialUncore(uncoreID int) {
	numCoresNeeded := a.numCPUsNeeded / a.topo.CPUsPerCore()

	// determine the N number of free cores (physical cpus) within the UncoreCache, then
	// determine the M number of free cpus (virtual cpus) that correspond with the free cores
	freeCores := a.details.CoresNeededInUncoreCache(numCoresNeeded, uncoreID)
	freeCPUs := a.details.CPUsInCores(freeCores.UnsortedList()...)

	// claim the cpus if the free cpus within the UncoreCache can satisfy the needed cpus
	claimed := (a.numCPUsNeeded == freeCPUs.Size())
	klog.V(4).InfoS("takePartialUncore: trying to claim partial uncore",
		"uncore", uncoreID,
		"claimed", claimed,
		"needed", a.numCPUsNeeded,
		"cores", freeCores.String(),
		"cpus", freeCPUs.String())
	if !claimed {
		return

	}
	a.take(freeCPUs)
}

// First try to take full UncoreCache, if available and need is at least the size of the UncoreCache group.
// Second try to take the partial UncoreCache if available and the request size can fit w/in the UncoreCache.
func (a *cpuAccumulator) takeUncoreCache() {
	numCPUsInUncore := a.topo.CPUsPerUncore()
	for _, uncore := range a.sortAvailableUncoreCaches() {
		// take full UncoreCache if the CPUs needed is greater than free UncoreCache size
		if a.needsAtLeast(numCPUsInUncore) {
			a.takeFullUncore()
		}

		if a.isSatisfied() {
			return
		}

		// take partial UncoreCache if the CPUs needed is less than free UncoreCache size
		a.takePartialUncore(uncore)
		if a.isSatisfied() {
			return
		}
	}
}

func (a *cpuAccumulator) takeFullCores() {
	for _, core := range a.freeCores() {
		cpusInCore := a.topo.CPUDetails.CPUsInCores(core)
		if !a.needsAtLeast(cpusInCore.Size()) {
			continue
		}
		klog.V(4).InfoS("takeFullCores: claiming core", "core", core)
		a.take(cpusInCore)
	}
}

func (a *cpuAccumulator) takeRemainingCPUs() {
	for _, cpu := range a.availableCPUSorter.sort() {
		klog.V(4).InfoS("takeRemainingCPUs: claiming CPU", "cpu", cpu)
		a.take(cpuset.New(cpu))
		if a.isSatisfied() {
			return
		}
	}
}

// rangeNUMANodesNeededToSatisfy returns minimum and maximum (in this order) number of NUMA nodes
// needed to satisfy the cpuAccumulator's goal of accumulating `a.numCPUsNeeded` CPUs, assuming that
// CPU groups have size given by the `cpuGroupSize` argument.
func (a *cpuAccumulator) rangeNUMANodesNeededToSatisfy(cpuGroupSize int) (minNumNUMAs, maxNumNUMAs int) {
	// Get the total number of NUMA nodes in the system.
	numNUMANodes := a.topo.CPUDetails.NUMANodes().Size()

	// Get the total number of NUMA nodes that have CPUs available on them.
	numNUMANodesAvailable := a.details.NUMANodes().Size()

	// Get the total number of CPUs in the system.
	numCPUs := a.topo.CPUDetails.CPUs().Size()

	// Get the total number of 'cpuGroups' in the system.
	numCPUGroups := (numCPUs-1)/cpuGroupSize + 1

	// Calculate the number of 'cpuGroups' per NUMA Node in the system (rounding up).
	numCPUGroupsPerNUMANode := (numCPUGroups-1)/numNUMANodes + 1

	// Calculate the number of available 'cpuGroups' across all NUMA nodes as
	// well as the number of 'cpuGroups' that need to be allocated (rounding up).
	numCPUGroupsNeeded := (a.numCPUsNeeded-1)/cpuGroupSize + 1

	// Calculate the minimum number of numa nodes required to satisfy the
	// allocation (rounding up).
	minNumNUMAs = (numCPUGroupsNeeded-1)/numCPUGroupsPerNUMANode + 1

	// Calculate the maximum number of numa nodes required to satisfy the allocation.
	maxNumNUMAs = min(numCPUGroupsNeeded, numNUMANodesAvailable)

	return
}

// needsAtLeast returns true if and only if the accumulator needs at least `n` CPUs.
// This means that needsAtLeast returns true even if more than `n` CPUs are needed.
func (a *cpuAccumulator) needsAtLeast(n int) bool {
	return a.numCPUsNeeded >= n
}

// isSatisfied returns true if and only if the accumulator has all the CPUs it needs.
func (a *cpuAccumulator) isSatisfied() bool {
	return a.numCPUsNeeded < 1
}

// isFailed returns true if and only if there aren't enough available CPUs in the system.
// (e.g. the accumulator needs 4 CPUs but only 3 are available).
func (a *cpuAccumulator) isFailed() bool {
	return a.numCPUsNeeded > a.details.CPUs().Size()
}

// iterateCombinations walks through all n-choose-k subsets of size k in n and
// calls function 'f()' on each subset. For example, if n={0,1,2}, and k=2,
// then f() will be called on the subsets {0,1}, {0,2}. and {1,2}. If f() ever
// returns 'Break', we break early and exit the loop.
func (a *cpuAccumulator) iterateCombinations(n []int, k int, f func([]int) LoopControl) {
	if k < 1 {
		return
	}

	var helper func(n []int, k int, start int, accum []int, f func([]int) LoopControl) LoopControl
	helper = func(n []int, k int, start int, accum []int, f func([]int) LoopControl) LoopControl {
		if k == 0 {
			return f(accum)
		}
		for i := start; i <= len(n)-k; i++ {
			control := helper(n, k-1, i+1, append(accum, n[i]), f)
			if control == Break {
				return Break
			}
		}
		return Continue
	}

	helper(n, k, 0, []int{}, f)
}

// takeByTopologyNUMAPacked returns a CPUSet containing `numCPUs` CPUs taken from the CPUs in the
// set `availableCPUs`. `topo` describes how the CPUs are arranged between sockets, NUMA nodes
// and physical cores (if hyperthreading is on a "CPU" is a thread rather than a full physical
// core).
//
// If sockets are higher than NUMA nodes in the memory hierarchy (i.e. a socket contains more than
// one NUMA node), the CPUs are selected as follows.
//
// If `numCPUs` is bigger than the total number of CPUs in a socket, and there are free (i.e. all
// CPUs in them are free) sockets, the function takes as many entire free sockets as possible.
// If there are no free sockets, or `numCPUs` is less than a whole socket, or the remaining number
// of CPUs to take after having taken some whole sockets is less than a whole socket, the function
// tries to take whole NUMA nodes.
//
// If the remaining number of CPUs to take is bigger than the total number of CPUs in a NUMA node,
// and there are free (i.e. all CPUs in them are free) NUMA nodes, the function takes as many entire
// free NUMA nodes as possible. The free NUMA nodes are taken from one socket at a time, and the
// sockets are considered by ascending order of free CPUs in them. If there are no free NUMA nodes,
// or the remaining number of CPUs to take after having taken full sockets and NUMA nodes is less
// than a whole NUMA node, the function tries to take whole physical cores (cores).
//
// If `PreferAlignByUncoreCache` is enabled, the function will try to optimally assign Uncorecaches.
// If `numCPUs` is larger than or equal to the total number of CPUs in a Uncorecache, and there are
// free (i.e. all CPUs within the Uncorecache are free) Uncorecaches, the function takes as many entire
// cores from free Uncorecaches as possible. If/Once `numCPUs` is smaller than the total number of
// CPUs in a free Uncorecache, the function scans each Uncorecache index in numerical order to assign
// cores that will fit within the Uncorecache. If `numCPUs` cannot fit within any Uncorecache, the
// function tries to take whole physical cores.
//
// If `numCPUs` is bigger than the total number of CPUs in a core, and there are
// free (i.e. all CPUs in them are free) cores, the function takes as many entire free cores as possible.
// The cores are taken from one socket at a time, and the sockets are considered by
// ascending order of free CPUs in them. For a given socket, the cores are taken one NUMA node at a time,
// and the NUMA nodes are considered by ascending order of free CPUs in them. If there are no free
// cores, or the remaining number of CPUs to take after having taken full sockets, NUMA nodes and
// cores is less than a whole core, the function tries to take individual CPUs.
//
// The individual CPUs are taken from one socket at a time, and the sockets are considered by
// ascending order of free CPUs in them. For a given socket, the CPUs are taken one NUMA node at a time,
// and the NUMA nodes are considered by ascending order of free CPUs in them. For a given NUMA node, the
// CPUs are taken one core at a time, and the core are considered by ascending order of free CPUs in them.
//
// If NUMA nodes are higher than Sockets in the memory hierarchy (i.e. a NUMA node contains more
// than one socket), the CPUs are selected as written above, with the only differences being that
// (1) the order with which full sockets and full NUMA nodes are acquired is swapped, and (2) the
// order with which lower-level topology elements are selected is also swapped accordingly. E.g.
// when selecting full cores, the cores are selected starting from the ones in the NUMA node with
// the least amount of free CPUs to the one with the highest amount of free CPUs (i.e. in ascending
// order of free CPUs). For any NUMA node, the cores are selected from the ones in the socket with
// the least amount of free CPUs to the one with the highest amount of free CPUs.
func takeByTopologyNUMAPacked(topo *topology.CPUTopology, availableCPUs cpuset.CPUSet, numCPUs int, cpuSortingStrategy CPUSortingStrategy, preferAlignByUncoreCache bool, reusableCPUsForResize *cpuset.CPUSet, mustKeepCPUsForScaleDown *cpuset.CPUSet) (cpuset.CPUSet, error) {

	// If the number of CPUs requested to be retained is not a subset
	// of reusableCPUs, then we fail early
	if reusableCPUsForResize != nil && mustKeepCPUsForScaleDown != nil {
		if (mustKeepCPUsForScaleDown.Intersection(reusableCPUsForResize.Clone())).IsEmpty() {
			return cpuset.New(), fmt.Errorf("requested CPUs to be retained %s are not a subset of reusable CPUs %s", mustKeepCPUsForScaleDown.String(), reusableCPUsForResize.String())
		}
	}

	acc := newCPUAccumulator(topo, availableCPUs, numCPUs, cpuSortingStrategy, reusableCPUsForResize, mustKeepCPUsForScaleDown)
	if acc.isSatisfied() {
		return acc.result, nil
	}
	if acc.isFailed() {
		return cpuset.New(), fmt.Errorf("not enough cpus available to satisfy request: requested=%d, available=%d", numCPUs, availableCPUs.Size())
	}
	// Algorithm: topology-aware best-fit
	// 1. Acquire whole NUMA nodes and sockets, if available and the container
	//    requires at least a NUMA node or socket's-worth of CPUs. If NUMA
	//    Nodes map to 1 or more sockets, pull from NUMA nodes first.
	//    Otherwise pull from sockets first.
	acc.numaOrSocketsFirst.takeFullFirstLevel()
	if acc.isSatisfied() {
		return acc.result, nil
	}
	acc.numaOrSocketsFirst.takeFullSecondLevel()
	if acc.isSatisfied() {
		return acc.result, nil
	}

	// 2. If PreferAlignByUncoreCache is enabled, acquire whole UncoreCaches
	//    if available and the container requires at least a UncoreCache's-worth
	//    of CPUs. Otherwise, acquire CPUs from the least amount of UncoreCaches.
	if preferAlignByUncoreCache {
		acc.takeUncoreCache()
		if acc.isSatisfied() {
			return acc.result, nil
		}
	}

	// 3. Acquire whole cores, if available and the container requires at least
	//    a core's-worth of CPUs.
	//    If `CPUSortingStrategySpread` is specified, skip taking the whole core.
	if cpuSortingStrategy != CPUSortingStrategySpread {
		acc.takeFullCores()
		if acc.isSatisfied() {
			return acc.result, nil
		}
	}

	// 4. Acquire single threads, preferring to fill partially-allocated cores
	//    on the same sockets as the whole cores we have already taken in this
	//    allocation.
	acc.takeRemainingCPUs()
	if acc.isSatisfied() {
		return acc.result, nil
	}

	return cpuset.New(), fmt.Errorf("failed to allocate cpus")
}

// takeByTopologyNUMADistributed returns a CPUSet of size 'numCPUs'.
//
// It generates this CPUset by allocating CPUs from 'availableCPUs' according
// to the algorithm outlined in KEP-2902:
//
// https://github.com/kubernetes/enhancements/tree/e7f51ffbe2ee398ffd1fba4a6d854f276bfad9fb/keps/sig-node/2902-cpumanager-distribute-cpus-policy-option
//
// This algorithm evenly distribute CPUs across NUMA nodes in cases where more
// than one NUMA node is required to satisfy the allocation. This is in
// contrast to the takeByTopologyNUMAPacked algorithm, which attempts to 'pack'
// CPUs onto NUMA nodes and fill them up before moving on to the next one.
//
// At a high-level this algorithm can be summarized as:
//
// For each NUMA single node:
//   - If all requested CPUs can be allocated from this NUMA node;
//     --> Do the allocation by running takeByTopologyNUMAPacked() over the
//     available CPUs in that NUMA node and return
//
// Otherwise, for each pair of NUMA nodes:
//   - If the set of requested CPUs (modulo 2) can be evenly split across
//     the 2 NUMA nodes; AND
//   - Any remaining CPUs (after the modulo operation) can be striped across
//     some subset of the NUMA nodes;
//     --> Do the allocation by running takeByTopologyNUMAPacked() over the
//     available CPUs in both NUMA nodes and return
//
// Otherwise, for each 3-tuple of NUMA nodes:
//   - If the set of requested CPUs (modulo 3) can be evenly distributed
//     across the 3 NUMA nodes; AND
//   - Any remaining CPUs (after the modulo operation) can be striped across
//     some subset of the NUMA nodes;
//     --> Do the allocation by running takeByTopologyNUMAPacked() over the
//     available CPUs in all three NUMA nodes and return
//
// ...
//
// Otherwise, for the set of all NUMA nodes:
//   - If the set of requested CPUs (modulo NUM_NUMA_NODES) can be evenly
//     distributed across all NUMA nodes; AND
//   - Any remaining CPUs (after the modulo operation) can be striped across
//     some subset of the NUMA nodes;
//     --> Do the allocation by running takeByTopologyNUMAPacked() over the
//     available CPUs in all NUMA nodes and return
//
// If none of the above conditions can be met, then resort back to a
// best-effort fit of packing CPUs into NUMA nodes by calling
// takeByTopologyNUMAPacked() over all available CPUs.
//
// NOTE: A "balance score" will be calculated to help find the best subset of
// NUMA nodes to allocate any 'remainder' CPUs from (in cases where the total
// number of CPUs to allocate cannot be evenly distributed across the chosen
// set of NUMA nodes). This "balance score" is calculated as the standard
// deviation of how many CPUs will be available on each NUMA node after all
// evenly distributed and remainder CPUs are allocated. The subset with the
// lowest "balance score" will receive the CPUs in order to keep the overall
// allocation of CPUs as "balanced" as possible.
//
// NOTE: This algorithm has been generalized to take an additional
// 'cpuGroupSize' parameter to ensure that CPUs are always allocated in groups
// of size 'cpuGroupSize' according to the algorithm described above. This is
// important, for example, to ensure that all CPUs (i.e. all hyperthreads) from
// a single core are allocated together.
func takeByTopologyNUMADistributed(topo *topology.CPUTopology, availableCPUs cpuset.CPUSet, numCPUs int, cpuGroupSize int, cpuSortingStrategy CPUSortingStrategy, reusableCPUsForResize *cpuset.CPUSet, mustKeepCPUsForScaleDown *cpuset.CPUSet) (cpuset.CPUSet, error) {
	// If the number of CPUs requested cannot be handed out in chunks of
	// 'cpuGroupSize', then we just call out the packing algorithm since we
	// can't distribute CPUs in this chunk size.
	// PreferAlignByUncoreCache feature not implemented here yet and set to false.
	// Support for PreferAlignByUncoreCache to be done at beta release.
	if (numCPUs % cpuGroupSize) != 0 {
		return takeByTopologyNUMAPacked(topo, availableCPUs, numCPUs, cpuSortingStrategy, false, reusableCPUsForResize, mustKeepCPUsForScaleDown)
	}

	// If the number of CPUs requested to be retained is not a subset
	// of reusableCPUs, then we fail early
	if reusableCPUsForResize != nil && mustKeepCPUsForScaleDown != nil {
		if (mustKeepCPUsForScaleDown.Intersection(reusableCPUsForResize.Clone())).IsEmpty() {
			return cpuset.New(), fmt.Errorf("requested CPUs to be retained %s are not a subset of reusable CPUs %s", mustKeepCPUsForScaleDown.String(), reusableCPUsForResize.String())
		}
	}

	// Otherwise build an accumulator to start allocating CPUs from.
	acc := newCPUAccumulator(topo, availableCPUs, numCPUs, cpuSortingStrategy, reusableCPUsForResize, mustKeepCPUsForScaleDown)
	if acc.isSatisfied() {
		return acc.result, nil
	}
	if acc.isFailed() {
		return cpuset.New(), fmt.Errorf("not enough cpus available to satisfy request: requested=%d, available=%d", numCPUs, availableCPUs.Size())
	}
	// Get the list of NUMA nodes represented by the set of CPUs in 'availableCPUs'.
	numas := acc.sortAvailableNUMANodes()

	// Calculate the minimum and maximum possible number of NUMA nodes that
	// could satisfy this request. This is used to optimize how many iterations
	// of the loop we need to go through below.
	minNUMAs, maxNUMAs := acc.rangeNUMANodesNeededToSatisfy(cpuGroupSize)

	// Try combinations of 1,2,3,... NUMA nodes until we find a combination
	// where we can evenly distribute CPUs across them. To optimize things, we
	// don't always start at 1 and end at len(numas). Instead, we use the
	// values of 'minNUMAs' and 'maxNUMAs' calculated above.
	for k := minNUMAs; k <= maxNUMAs; k++ {
		// Iterate through the various n-choose-k NUMA node combinations,
		// looking for the combination of NUMA nodes that can best have CPUs
		// distributed across them.
		var bestBalance float64 = math.MaxFloat64
		var bestRemainder []int = nil
		var bestCombo []int = nil
		acc.iterateCombinations(numas, k, func(combo []int) LoopControl {
			// If we've already found a combo with a balance of 0 in a
			// different iteration, then don't bother checking any others.
			if bestBalance == 0 {
				return Break
			}

			// Check that this combination of NUMA nodes has enough CPUs to
			// satisfy the allocation overall.
			cpus := acc.details.CPUsInNUMANodes(combo...)
			if cpus.Size() < numCPUs {
				return Continue
			}

			// Check that CPUs can be handed out in groups of size
			// 'cpuGroupSize' across the NUMA nodes in this combo.
			numCPUGroups := 0
			for _, numa := range combo {
				numCPUGroups += (acc.details.CPUsInNUMANodes(numa).Size() / cpuGroupSize)
			}
			if (numCPUGroups * cpuGroupSize) < numCPUs {
				return Continue
			}

			// Check that each NUMA node in this combination can allocate an
			// even distribution of CPUs in groups of size 'cpuGroupSize',
			// modulo some remainder.
			distribution := (numCPUs / len(combo) / cpuGroupSize) * cpuGroupSize
			for _, numa := range combo {
				cpus := acc.details.CPUsInNUMANodes(numa)
				if cpus.Size() < distribution {
					return Continue
				}
			}

			// Calculate how many CPUs will be available on each NUMA node in
			// the system after allocating an even distribution of CPU groups
			// of size 'cpuGroupSize' from each NUMA node in 'combo'. This will
			// be used in the "balance score" calculation to help decide if
			// this combo should ultimately be chosen.
			availableAfterAllocation := make(mapIntInt, len(numas))
			for _, numa := range numas {
				availableAfterAllocation[numa] = acc.details.CPUsInNUMANodes(numa).Size()
			}
			for _, numa := range combo {
				availableAfterAllocation[numa] -= distribution
			}

			// Check if there are any remaining CPUs to distribute across the
			// NUMA nodes once CPUs have been evenly distributed in groups of
			// size 'cpuGroupSize'.
			remainder := numCPUs - (distribution * len(combo))

			// Get a list of NUMA nodes to consider pulling the remainder CPUs
			// from. This list excludes NUMA nodes that don't have at least
			// 'cpuGroupSize' CPUs available after being allocated
			// 'distribution' number of CPUs.
			var remainderCombo []int
			for _, numa := range combo {
				if availableAfterAllocation[numa] >= cpuGroupSize {
					remainderCombo = append(remainderCombo, numa)
				}
			}

			// Declare a set of local variables to help track the "balance
			// scores" calculated when using different subsets of
			// 'remainderCombo' to allocate remainder CPUs from.
			var bestLocalBalance float64 = math.MaxFloat64
			var bestLocalRemainder []int = nil

			// If there aren't any remainder CPUs to allocate, then calculate
			// the "balance score" of this combo as the standard deviation of
			// the values contained in 'availableAfterAllocation'.
			if remainder == 0 {
				bestLocalBalance = standardDeviation(availableAfterAllocation.Values())
				bestLocalRemainder = nil
			}

			// Otherwise, find the best "balance score" when allocating the
			// remainder CPUs across different subsets of NUMA nodes in 'remainderCombo'.
			// These remainder CPUs are handed out in groups of size 'cpuGroupSize'.
			// We start from k=len(remainderCombo) and walk down to k=1 so that
			// we continue to distribute CPUs as much as possible across
			// multiple NUMA nodes.
			for k := len(remainderCombo); remainder > 0 && k >= 1; k-- {
				acc.iterateCombinations(remainderCombo, k, func(subset []int) LoopControl {
					// Make a local copy of 'remainder'.
					remainder := remainder

					// Make a local copy of 'availableAfterAllocation'.
					availableAfterAllocation := availableAfterAllocation.Clone()

					// If this subset is not capable of allocating all
					// remainder CPUs, continue to the next one.
					if sum(availableAfterAllocation.Values(subset...)) < remainder {
						return Continue
					}

					// For all NUMA nodes in 'subset', walk through them,
					// removing 'cpuGroupSize' number of CPUs from each
					// until all remainder CPUs have been accounted for.
					for remainder > 0 {
						for _, numa := range subset {
							if remainder == 0 {
								break
							}
							if availableAfterAllocation[numa] < cpuGroupSize {
								continue
							}
							availableAfterAllocation[numa] -= cpuGroupSize
							remainder -= cpuGroupSize
						}
					}

					// Calculate the "balance score" as the standard deviation
					// of the number of CPUs available on all NUMA nodes in the
					// system after the remainder CPUs have been allocated
					// across 'subset' in groups of size 'cpuGroupSize'.
					balance := standardDeviation(availableAfterAllocation.Values())
					if balance < bestLocalBalance {
						bestLocalBalance = balance
						bestLocalRemainder = subset
					}

					return Continue
				})
			}

			// If the best "balance score" for this combo is less than the
			// lowest "balance score" of all previous combos, then update this
			// combo (and remainder set) to be the best one found so far.
			if bestLocalBalance < bestBalance {
				bestBalance = bestLocalBalance
				bestRemainder = bestLocalRemainder
				bestCombo = combo
			}

			return Continue
		})

		// If we made it through all of the iterations above without finding a
		// combination of NUMA nodes that can properly balance CPU allocations,
		// then move on to the next larger set of NUMA node combinations.
		if bestCombo == nil {
			continue
		}

		// Otherwise, start allocating CPUs from the NUMA node combination
		// chosen. First allocate an even distribution of CPUs in groups of
		// size 'cpuGroupSize' from 'bestCombo'.
		distribution := (numCPUs / len(bestCombo) / cpuGroupSize) * cpuGroupSize
		for _, numa := range bestCombo {
			cpus, _ := takeByTopologyNUMAPacked(acc.topo, acc.details.CPUsInNUMANodes(numa), distribution, cpuSortingStrategy, false, reusableCPUsForResize, mustKeepCPUsForScaleDown)
			acc.take(cpus)
		}

		// Then allocate any remaining CPUs in groups of size 'cpuGroupSize'
		// from each NUMA node in the remainder set.
		remainder := numCPUs - (distribution * len(bestCombo))
		for remainder > 0 {
			for _, numa := range bestRemainder {
				if remainder == 0 {
					break
				}
				if acc.details.CPUsInNUMANodes(numa).Size() < cpuGroupSize {
					continue
				}
				cpus, _ := takeByTopologyNUMAPacked(acc.topo, acc.details.CPUsInNUMANodes(numa), cpuGroupSize, cpuSortingStrategy, false, reusableCPUsForResize, mustKeepCPUsForScaleDown)
				acc.take(cpus)
				remainder -= cpuGroupSize
			}
		}

		// If we haven't allocated all of our CPUs at this point, then something
		// went wrong in our accounting and we should error out.
		if acc.numCPUsNeeded > 0 {
			return cpuset.New(), fmt.Errorf("accounting error, not enough CPUs allocated, remaining: %v", acc.numCPUsNeeded)
		}

		// Likewise, if we have allocated too many CPUs at this point, then something
		// went wrong in our accounting and we should error out.
		if acc.numCPUsNeeded < 0 {
			return cpuset.New(), fmt.Errorf("accounting error, too many CPUs allocated, remaining: %v", acc.numCPUsNeeded)
		}

		// Otherwise, return the result
		return acc.result, nil
	}

	// If we never found a combination of NUMA nodes that we could properly
	// distribute CPUs across, fall back to the packing algorithm.
	return takeByTopologyNUMAPacked(topo, availableCPUs, numCPUs, cpuSortingStrategy, false, reusableCPUsForResize, mustKeepCPUsForScaleDown)
}
