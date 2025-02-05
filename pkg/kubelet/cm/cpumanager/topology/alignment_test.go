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
	"reflect"
	"testing"

	"k8s.io/utils/cpuset"
)

func TestNewAlignment(t *testing.T) {
	topo := &CPUTopology{
		NumCPUs:        32,
		NumSockets:     1,
		NumCores:       32,
		NumNUMANodes:   1,
		NumUncoreCache: 8,
		CPUDetails: map[int]CPUInfo{
			0:  {CoreID: 0, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 0},
			1:  {CoreID: 1, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 0},
			2:  {CoreID: 2, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 0},
			3:  {CoreID: 3, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 0},
			4:  {CoreID: 4, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 1},
			5:  {CoreID: 5, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 1},
			6:  {CoreID: 6, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 1},
			7:  {CoreID: 7, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 1},
			8:  {CoreID: 8, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 2},
			9:  {CoreID: 9, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 2},
			10: {CoreID: 10, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 2},
			11: {CoreID: 11, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 2},
			12: {CoreID: 12, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 3},
			13: {CoreID: 13, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 3},
			14: {CoreID: 14, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 3},
			15: {CoreID: 15, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 3},
			16: {CoreID: 16, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 4},
			17: {CoreID: 17, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 4},
			18: {CoreID: 18, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 4},
			19: {CoreID: 19, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 4},
			20: {CoreID: 20, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 5},
			21: {CoreID: 21, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 5},
			22: {CoreID: 22, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 5},
			23: {CoreID: 23, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 5},
			24: {CoreID: 24, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 6},
			25: {CoreID: 25, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 6},
			26: {CoreID: 26, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 6},
			27: {CoreID: 27, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 6},
			28: {CoreID: 28, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 7},
			29: {CoreID: 29, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 7},
			30: {CoreID: 30, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 7},
			31: {CoreID: 31, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 7},
		},
	}

	tests := []struct {
		name string
		topo *CPUTopology
		cpus cpuset.CPUSet
		want Alignment
	}{{
		name: "empty cpuset",
		topo: topo,
		cpus: cpuset.New(),
		want: Alignment{
			UncoreCache: true,
		},
	}, {
		name: "single random CPU",
		topo: topo,
		cpus: cpuset.New(11), // any single id is fine, no special meaning
		want: Alignment{
			UncoreCache: true,
		},
	}, {
		name: "less CPUs than a uncore cache group",
		topo: topo,
		cpus: cpuset.New(29, 30, 31), // random cpus as long as they belong to the same uncore cache
		want: Alignment{
			UncoreCache: true,
		},
	}, {
		name: "enough CPUs to fill a uncore cache group",
		topo: topo,
		cpus: cpuset.New(8, 9, 10, 11), // random cpus as long as they belong to the same uncore cache
		want: Alignment{
			UncoreCache: true,
		},
	}, {
		name: "more CPUs than a full a uncore cache group",
		topo: topo,
		cpus: cpuset.New(9, 10, 11, 23), // random cpus as long as they belong to the same uncore cache
		want: Alignment{
			UncoreCache: false,
		},
	}, {
		name: "enough CPUs to exactly fill multiple uncore cache groups",
		topo: topo,
		cpus: cpuset.New(8, 9, 10, 11, 16, 17, 18, 19), // random cpus as long as they belong to the same uncore cache
		want: Alignment{
			UncoreCache: false,
		},
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.topo.CheckAlignment(tt.cpus)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("AlignmentFromCPUSet() = %v, want %v", got, tt.want)
			}
		})
	}
}
