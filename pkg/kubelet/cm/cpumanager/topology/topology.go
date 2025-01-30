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

package topology

import (
	"fmt"

	cadvisorapi "github.com/google/cadvisor/info/v1"
	"k8s.io/klog/v2"
	"k8s.io/utils/cpuset"
)

// NUMANodeInfo is a map from NUMANode ID to a list of CPU IDs associated with
// that NUMANode.
type NUMANodeInfo map[int]cpuset.CPUSet

// CPUDetails is a map from CPU ID to Core ID, Socket ID, and NUMA ID.
type CPUDetails map[int]CPUInfo

// CPUTopology contains details of node cpu, where :
// CPU  - logical CPU, cadvisor - thread
// Core - physical CPU, cadvisor - Core
// Socket - socket, cadvisor - Socket
// NUMA Node - NUMA cell, cadvisor - Node
// UncoreCache - Split L3 Cache Topology, cadvisor
type CPUTopology struct {
	NumCPUs        int
	NumCores       int
	NumUncoreCache int
	NumSockets     int
	NumNUMANodes   int
	CPUDetails     CPUDetails
}

// CPUsPerCore returns the number of logical CPUs are associated with
// each core.
func (topo *CPUTopology) CPUsPerCore() int {
	if topo.NumCores == 0 {
		return 0
	}
	return topo.NumCPUs / topo.NumCores
}

// CPUsPerSocket returns the number of logical CPUs are associated with
// each socket.
func (topo *CPUTopology) CPUsPerSocket() int {
	if topo.NumSockets == 0 {
		return 0
	}
	return topo.NumCPUs / topo.NumSockets
}

// CPUsPerUncore returns the number of logicial CPUs that are associated with
// each UncoreCache
func (topo *CPUTopology) CPUsPerUncore() int {
	if topo.NumUncoreCache == 0 {
		return 0
	}
	return topo.NumCPUs / topo.NumUncoreCache
}

// CPUCoreID returns the physical core ID which the given logical CPU
// belongs to.
func (topo *CPUTopology) CPUCoreID(cpu int) (int, error) {
	info, ok := topo.CPUDetails[cpu]
	if !ok {
		return -1, fmt.Errorf("unknown CPU ID: %d", cpu)
	}
	return info.CoreID, nil
}

// CPUCoreID returns the socket ID which the given logical CPU belongs to.
func (topo *CPUTopology) CPUSocketID(cpu int) (int, error) {
	info, ok := topo.CPUDetails[cpu]
	if !ok {
		return -1, fmt.Errorf("unknown CPU ID: %d", cpu)
	}
	return info.SocketID, nil
}

// CPUCoreID returns the NUMA node ID which the given logical CPU belongs to.
func (topo *CPUTopology) CPUNUMANodeID(cpu int) (int, error) {
	info, ok := topo.CPUDetails[cpu]
	if !ok {
		return -1, fmt.Errorf("unknown CPU ID: %d", cpu)
	}
	return info.NUMANodeID, nil
}

// CheckAlignment returns alignment information for the given cpuset in
// the context of the current CPU topology
func (topo *CPUTopology) CheckAlignment(cpus cpuset.CPUSet) Alignment {
	cpuList := cpus.UnsortedList()
	return Alignment{
		UncoreCache: isAlignedAtUncoreCache(topo, cpuList...),
	}
}

// CPUInfo contains the NUMA, socket, UncoreCache and core IDs associated with a CPU.
type CPUInfo struct {
	NUMANodeID    int
	SocketID      int
	CoreID        int
	UncoreCacheID int
}

// KeepOnly returns a new CPUDetails object with only the supplied cpus.
func (d CPUDetails) KeepOnly(cpus cpuset.CPUSet) CPUDetails {
	result := CPUDetails{}
	for cpu, info := range d {
		if cpus.Contains(cpu) {
			result[cpu] = info
		}
	}
	return result
}

// UncoreCaches returns all the uncorecache Id (L3 Index) associated with the CPUs in this CPUDetails
func (d CPUDetails) UncoreCaches() cpuset.CPUSet {
	var numUnCoreIDs []int
	for _, info := range d {
		numUnCoreIDs = append(numUnCoreIDs, info.UncoreCacheID)
	}
	return cpuset.New(numUnCoreIDs...)
}

// UnCoresInNUMANodes returns all of the uncore IDs associated with the given
// NUMANode IDs in this CPUDetails.
func (d CPUDetails) UncoreInNUMANodes(ids ...int) cpuset.CPUSet {
	var unCoreIDs []int
	for _, id := range ids {
		for _, info := range d {
			if info.NUMANodeID == id {
				unCoreIDs = append(unCoreIDs, info.UncoreCacheID)
			}
		}
	}
	return cpuset.New(unCoreIDs...)
}

// CoresNeededInUncoreCache returns either the full list of all available unique core IDs associated with the given
// UnCoreCache IDs in this CPUDetails or subset that matches the ask.
func (d CPUDetails) CoresNeededInUncoreCache(numCoresNeeded int, ids ...int) cpuset.CPUSet {
	coreIDs := d.coresInUncoreCache(ids...)
	if coreIDs.Size() <= numCoresNeeded {
		return coreIDs
	}
	tmpCoreIDs := coreIDs.List()
	return cpuset.New(tmpCoreIDs[:numCoresNeeded]...)
}

// Helper function that just gets the cores
func (d CPUDetails) coresInUncoreCache(ids ...int) cpuset.CPUSet {
	var coreIDs []int
	for _, id := range ids {
		for _, info := range d {
			if info.UncoreCacheID == id {
				coreIDs = append(coreIDs, info.CoreID)
			}
		}
	}
	return cpuset.New(coreIDs...)
}

// CPUsInUncoreCaches returns all the logical CPU IDs associated with the given
// UnCoreCache IDs in this CPUDetails
func (d CPUDetails) CPUsInUncoreCaches(ids ...int) cpuset.CPUSet {
	var cpuIDs []int
	for _, id := range ids {
		for cpu, info := range d {
			if info.UncoreCacheID == id {
				cpuIDs = append(cpuIDs, cpu)
			}
		}
	}
	return cpuset.New(cpuIDs...)
}

// NUMANodes returns all of the NUMANode IDs associated with the CPUs in this
// CPUDetails.
func (d CPUDetails) NUMANodes() cpuset.CPUSet {
	var numaNodeIDs []int
	for _, info := range d {
		numaNodeIDs = append(numaNodeIDs, info.NUMANodeID)
	}
	return cpuset.New(numaNodeIDs...)
}

// NUMANodesInSockets returns all of the logical NUMANode IDs associated with
// the given socket IDs in this CPUDetails.
func (d CPUDetails) NUMANodesInSockets(ids ...int) cpuset.CPUSet {
	var numaNodeIDs []int
	for _, id := range ids {
		for _, info := range d {
			if info.SocketID == id {
				numaNodeIDs = append(numaNodeIDs, info.NUMANodeID)
			}
		}
	}
	return cpuset.New(numaNodeIDs...)
}

// Sockets returns all of the socket IDs associated with the CPUs in this
// CPUDetails.
func (d CPUDetails) Sockets() cpuset.CPUSet {
	var socketIDs []int
	for _, info := range d {
		socketIDs = append(socketIDs, info.SocketID)
	}
	return cpuset.New(socketIDs...)
}

// CPUsInSockets returns all of the logical CPU IDs associated with the given
// socket IDs in this CPUDetails.
func (d CPUDetails) CPUsInSockets(ids ...int) cpuset.CPUSet {
	var cpuIDs []int
	for _, id := range ids {
		for cpu, info := range d {
			if info.SocketID == id {
				cpuIDs = append(cpuIDs, cpu)
			}
		}
	}
	return cpuset.New(cpuIDs...)
}

// SocketsInNUMANodes returns all of the logical Socket IDs associated with the
// given NUMANode IDs in this CPUDetails.
func (d CPUDetails) SocketsInNUMANodes(ids ...int) cpuset.CPUSet {
	var socketIDs []int
	for _, id := range ids {
		for _, info := range d {
			if info.NUMANodeID == id {
				socketIDs = append(socketIDs, info.SocketID)
			}
		}
	}
	return cpuset.New(socketIDs...)
}

// Cores returns all of the core IDs associated with the CPUs in this
// CPUDetails.
func (d CPUDetails) Cores() cpuset.CPUSet {
	var coreIDs []int
	for _, info := range d {
		coreIDs = append(coreIDs, info.CoreID)
	}
	return cpuset.New(coreIDs...)
}

// CoresInNUMANodes returns all of the core IDs associated with the given
// NUMANode IDs in this CPUDetails.
func (d CPUDetails) CoresInNUMANodes(ids ...int) cpuset.CPUSet {
	var coreIDs []int
	for _, id := range ids {
		for _, info := range d {
			if info.NUMANodeID == id {
				coreIDs = append(coreIDs, info.CoreID)
			}
		}
	}
	return cpuset.New(coreIDs...)
}

// CoresInSockets returns all of the core IDs associated with the given socket
// IDs in this CPUDetails.
func (d CPUDetails) CoresInSockets(ids ...int) cpuset.CPUSet {
	var coreIDs []int
	for _, id := range ids {
		for _, info := range d {
			if info.SocketID == id {
				coreIDs = append(coreIDs, info.CoreID)
			}
		}
	}
	return cpuset.New(coreIDs...)
}

// CPUs returns all of the logical CPU IDs in this CPUDetails.
func (d CPUDetails) CPUs() cpuset.CPUSet {
	var cpuIDs []int
	for cpuID := range d {
		cpuIDs = append(cpuIDs, cpuID)
	}
	return cpuset.New(cpuIDs...)
}

// CPUsInNUMANodes returns all of the logical CPU IDs associated with the given
// NUMANode IDs in this CPUDetails.
func (d CPUDetails) CPUsInNUMANodes(ids ...int) cpuset.CPUSet {
	var cpuIDs []int
	for _, id := range ids {
		for cpu, info := range d {
			if info.NUMANodeID == id {
				cpuIDs = append(cpuIDs, cpu)
			}
		}
	}
	return cpuset.New(cpuIDs...)
}

// CPUsInCores returns all of the logical CPU IDs associated with the given
// core IDs in this CPUDetails.
func (d CPUDetails) CPUsInCores(ids ...int) cpuset.CPUSet {
	var cpuIDs []int
	for _, id := range ids {
		for cpu, info := range d {
			if info.CoreID == id {
				cpuIDs = append(cpuIDs, cpu)
			}
		}
	}
	return cpuset.New(cpuIDs...)
}

func getUncoreCacheID(core cadvisorapi.Core) int {
	if len(core.UncoreCaches) < 1 {
		// In case cAdvisor is nil, failback to socket alignment since uncorecache is not shared
		return core.SocketID
	}
	// Even though cadvisor API returns a slice, we only expect either 0 or a 1 uncore caches,
	// so everything past the first entry should be discarded or ignored
	return core.UncoreCaches[0].Id
}

// Discover returns CPUTopology based on cadvisor node info
func Discover(machineInfo *cadvisorapi.MachineInfo) (*CPUTopology, error) {
	if machineInfo.NumCores == 0 {
		return nil, fmt.Errorf("could not detect number of cpus")
	}

	CPUDetails := CPUDetails{}
	numPhysicalCores := 0

	for _, node := range machineInfo.Topology {
		numPhysicalCores += len(node.Cores)
		for _, core := range node.Cores {
			if coreID, err := getUniqueCoreID(core.Threads); err == nil {
				for _, cpu := range core.Threads {
					CPUDetails[cpu] = CPUInfo{
						CoreID:        coreID,
						SocketID:      core.SocketID,
						NUMANodeID:    node.Id,
						UncoreCacheID: getUncoreCacheID(core),
					}
				}
			} else {
				klog.ErrorS(nil, "Could not get unique coreID for socket", "socket", core.SocketID, "core", core.Id, "threads", core.Threads)
				return nil, err
			}
		}
	}

	return &CPUTopology{
		NumCPUs:        machineInfo.NumCores,
		NumSockets:     machineInfo.NumSockets,
		NumCores:       numPhysicalCores,
		NumNUMANodes:   CPUDetails.NUMANodes().Size(),
		NumUncoreCache: CPUDetails.UncoreCaches().Size(),
		CPUDetails:     CPUDetails,
	}, nil
}

// getUniqueCoreID computes coreId as the lowest cpuID
// for a given Threads []int slice. This will assure that coreID's are
// platform unique (opposite to what cAdvisor reports)
func getUniqueCoreID(threads []int) (coreID int, err error) {
	if len(threads) == 0 {
		return 0, fmt.Errorf("no cpus provided")
	}

	if len(threads) != cpuset.New(threads...).Size() {
		return 0, fmt.Errorf("cpus provided are not unique")
	}

	min := threads[0]
	for _, thread := range threads[1:] {
		if thread < min {
			min = thread
		}
	}

	return min, nil
}
