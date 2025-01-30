/*
Copyright 2025 The Kubernetes Authors.

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

	"k8s.io/utils/cpuset"
)

// Alignment is metadata about a cpuset allocation
type Alignment struct {
	// UncoreCache is true if all the CPUs are uncore-cache aligned,
	// IOW if they all share the same Uncore cache block.
	// If the allocated CPU count is greater than a Uncore Group size,
	// CPUs can't be uncore-aligned; otherwise, they are.
	// This flag tracks alignment, not interference or lack thereof.
	UncoreCache bool
}

func (ca Alignment) String() string {
	return fmt.Sprintf("aligned=<uncore:%v>", ca.UncoreCache)
}

// Allocation represents a CPU set plus alignment metadata
type Allocation struct {
	CPUs    cpuset.CPUSet
	Aligned Alignment
}

func (ca Allocation) String() string {
	return ca.CPUs.String() + " " + ca.Aligned.String()
}

// EmptyAllocation returns a new zero-valued CPU allocation. Please note that
// a empty cpuset is aligned according to every possible way we can consider
func EmptyAllocation() Allocation {
	return Allocation{
		CPUs: cpuset.New(),
		Aligned: Alignment{
			UncoreCache: true,
		},
	}
}

func isAlignedAtUncoreCache(topo *CPUTopology, cpuList ...int) bool {
	if len(cpuList) <= 1 {
		return true
	}
	reference, ok := topo.CPUDetails[cpuList[0]]
	if !ok {
		return false
	}
	for _, cpu := range cpuList[1:] {
		info, ok := topo.CPUDetails[cpu]
		if !ok {
			return false
		}
		if info.UncoreCacheID != reference.UncoreCacheID {
			return false
		}
	}
	return true
}
