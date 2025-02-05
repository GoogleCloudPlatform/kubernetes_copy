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
	"reflect"
	"testing"

	cadvisorapi "github.com/google/cadvisor/info/v1"
	"github.com/google/go-cmp/cmp"
	"k8s.io/utils/cpuset"
)

func Test_Discover(t *testing.T) {
	tests := []struct {
		name        string
		machineInfo cadvisorapi.MachineInfo
		want        *CPUTopology
		wantErr     bool
	}{
		{
			name: "EmptyUncoreCache",
			machineInfo: cadvisorapi.MachineInfo{
				NumCores:   8,
				NumSockets: 1,
				Topology: []cadvisorapi.Node{
					{Id: 0,
						Cores: []cadvisorapi.Core{
							{SocketID: 0, Id: 0, Threads: []int{0, 4}, UncoreCaches: []cadvisorapi.Cache{}},
							{SocketID: 0, Id: 1, Threads: []int{1, 5}, UncoreCaches: []cadvisorapi.Cache{}},
							{SocketID: 0, Id: 2, Threads: []int{2, 6}, UncoreCaches: []cadvisorapi.Cache{}},
							{SocketID: 0, Id: 3, Threads: []int{3, 7}, UncoreCaches: []cadvisorapi.Cache{}},
						},
					},
				},
			},
			want: &CPUTopology{
				NumCPUs:        8,
				NumSockets:     1,
				NumCores:       4,
				NumNUMANodes:   1,
				NumUncoreCache: 1,
				CPUDetails: map[int]CPUInfo{
					0: {CoreID: 0, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 0},
					1: {CoreID: 1, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 0},
					2: {CoreID: 2, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 0},
					3: {CoreID: 3, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 0},
					4: {CoreID: 0, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 0},
					5: {CoreID: 1, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 0},
					6: {CoreID: 2, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 0},
					7: {CoreID: 3, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 0},
				},
			},
			wantErr: false,
		},
		{
			name: "FailNumCores",
			machineInfo: cadvisorapi.MachineInfo{
				NumCores: 0,
			},
			want:    &CPUTopology{},
			wantErr: true,
		},
		{
			name: "OneSocketHT",
			machineInfo: cadvisorapi.MachineInfo{
				NumCores:   8,
				NumSockets: 1,
				Topology: []cadvisorapi.Node{
					{Id: 0,
						Cores: []cadvisorapi.Core{
							{SocketID: 0, Id: 0, Threads: []int{0, 4}, UncoreCaches: []cadvisorapi.Cache{{Id: 0}}},
							{SocketID: 0, Id: 1, Threads: []int{1, 5}, UncoreCaches: []cadvisorapi.Cache{{Id: 0}}},
							{SocketID: 0, Id: 2, Threads: []int{2, 6}, UncoreCaches: []cadvisorapi.Cache{{Id: 0}}},
							{SocketID: 0, Id: 3, Threads: []int{3, 7}, UncoreCaches: []cadvisorapi.Cache{{Id: 0}}},
						},
					},
				},
			},
			want: &CPUTopology{
				NumCPUs:        8,
				NumSockets:     1,
				NumCores:       4,
				NumNUMANodes:   1,
				NumUncoreCache: 1,
				CPUDetails: map[int]CPUInfo{
					0: {CoreID: 0, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 0},
					1: {CoreID: 1, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 0},
					2: {CoreID: 2, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 0},
					3: {CoreID: 3, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 0},
					4: {CoreID: 0, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 0},
					5: {CoreID: 1, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 0},
					6: {CoreID: 2, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 0},
					7: {CoreID: 3, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 0},
				},
			},
			wantErr: false,
		},
		{
			// dual xeon gold 6230
			name: "DualSocketMultiNumaPerSocketHT",
			machineInfo: cadvisorapi.MachineInfo{
				NumCores:   80,
				NumSockets: 2,
				Topology: []cadvisorapi.Node{
					{Id: 0,
						Cores: []cadvisorapi.Core{
							{SocketID: 0, Id: 0, Threads: []int{0, 40}, UncoreCaches: []cadvisorapi.Cache{{Id: 0}}},
							{SocketID: 0, Id: 1, Threads: []int{1, 41}, UncoreCaches: []cadvisorapi.Cache{{Id: 0}}},
							{SocketID: 0, Id: 2, Threads: []int{2, 42}, UncoreCaches: []cadvisorapi.Cache{{Id: 0}}},
							{SocketID: 0, Id: 8, Threads: []int{3, 43}, UncoreCaches: []cadvisorapi.Cache{{Id: 0}}},
							{SocketID: 0, Id: 9, Threads: []int{4, 44}, UncoreCaches: []cadvisorapi.Cache{{Id: 0}}},
							{SocketID: 0, Id: 16, Threads: []int{5, 45}, UncoreCaches: []cadvisorapi.Cache{{Id: 0}}},
							{SocketID: 0, Id: 17, Threads: []int{6, 46}, UncoreCaches: []cadvisorapi.Cache{{Id: 0}}},
							{SocketID: 0, Id: 18, Threads: []int{7, 47}, UncoreCaches: []cadvisorapi.Cache{{Id: 0}}},
							{SocketID: 0, Id: 24, Threads: []int{8, 48}, UncoreCaches: []cadvisorapi.Cache{{Id: 0}}},
							{SocketID: 0, Id: 25, Threads: []int{9, 49}, UncoreCaches: []cadvisorapi.Cache{{Id: 0}}},
						},
					},
					{Id: 1,
						Cores: []cadvisorapi.Core{
							{SocketID: 0, Id: 3, Threads: []int{10, 50}, UncoreCaches: []cadvisorapi.Cache{{Id: 0}}},
							{SocketID: 0, Id: 4, Threads: []int{11, 51}, UncoreCaches: []cadvisorapi.Cache{{Id: 0}}},
							{SocketID: 0, Id: 10, Threads: []int{12, 52}, UncoreCaches: []cadvisorapi.Cache{{Id: 0}}},
							{SocketID: 0, Id: 11, Threads: []int{13, 53}, UncoreCaches: []cadvisorapi.Cache{{Id: 0}}},
							{SocketID: 0, Id: 12, Threads: []int{14, 54}, UncoreCaches: []cadvisorapi.Cache{{Id: 0}}},
							{SocketID: 0, Id: 19, Threads: []int{15, 55}, UncoreCaches: []cadvisorapi.Cache{{Id: 0}}},
							{SocketID: 0, Id: 20, Threads: []int{16, 56}, UncoreCaches: []cadvisorapi.Cache{{Id: 0}}},
							{SocketID: 0, Id: 26, Threads: []int{17, 57}, UncoreCaches: []cadvisorapi.Cache{{Id: 0}}},
							{SocketID: 0, Id: 27, Threads: []int{18, 58}, UncoreCaches: []cadvisorapi.Cache{{Id: 0}}},
							{SocketID: 0, Id: 28, Threads: []int{19, 59}, UncoreCaches: []cadvisorapi.Cache{{Id: 0}}},
						},
					},
					{Id: 2,
						Cores: []cadvisorapi.Core{
							{SocketID: 1, Id: 0, Threads: []int{20, 60}, UncoreCaches: []cadvisorapi.Cache{{Id: 0}}},
							{SocketID: 1, Id: 1, Threads: []int{21, 61}, UncoreCaches: []cadvisorapi.Cache{{Id: 0}}},
							{SocketID: 1, Id: 2, Threads: []int{22, 62}, UncoreCaches: []cadvisorapi.Cache{{Id: 0}}},
							{SocketID: 1, Id: 8, Threads: []int{23, 63}, UncoreCaches: []cadvisorapi.Cache{{Id: 0}}},
							{SocketID: 1, Id: 9, Threads: []int{24, 64}, UncoreCaches: []cadvisorapi.Cache{{Id: 0}}},
							{SocketID: 1, Id: 16, Threads: []int{25, 65}, UncoreCaches: []cadvisorapi.Cache{{Id: 0}}},
							{SocketID: 1, Id: 17, Threads: []int{26, 66}, UncoreCaches: []cadvisorapi.Cache{{Id: 0}}},
							{SocketID: 1, Id: 18, Threads: []int{27, 67}, UncoreCaches: []cadvisorapi.Cache{{Id: 0}}},
							{SocketID: 1, Id: 24, Threads: []int{28, 68}, UncoreCaches: []cadvisorapi.Cache{{Id: 0}}},
							{SocketID: 1, Id: 25, Threads: []int{29, 69}, UncoreCaches: []cadvisorapi.Cache{{Id: 0}}},
						},
					},
					{Id: 3,
						Cores: []cadvisorapi.Core{
							{SocketID: 1, Id: 3, Threads: []int{30, 70}, UncoreCaches: []cadvisorapi.Cache{{Id: 0}}},
							{SocketID: 1, Id: 4, Threads: []int{31, 71}, UncoreCaches: []cadvisorapi.Cache{{Id: 0}}},
							{SocketID: 1, Id: 10, Threads: []int{32, 72}, UncoreCaches: []cadvisorapi.Cache{{Id: 0}}},
							{SocketID: 1, Id: 11, Threads: []int{33, 73}, UncoreCaches: []cadvisorapi.Cache{{Id: 0}}},
							{SocketID: 1, Id: 12, Threads: []int{34, 74}, UncoreCaches: []cadvisorapi.Cache{{Id: 0}}},
							{SocketID: 1, Id: 19, Threads: []int{35, 75}, UncoreCaches: []cadvisorapi.Cache{{Id: 0}}},
							{SocketID: 1, Id: 20, Threads: []int{36, 76}, UncoreCaches: []cadvisorapi.Cache{{Id: 0}}},
							{SocketID: 1, Id: 26, Threads: []int{37, 77}, UncoreCaches: []cadvisorapi.Cache{{Id: 0}}},
							{SocketID: 1, Id: 27, Threads: []int{38, 78}, UncoreCaches: []cadvisorapi.Cache{{Id: 0}}},
							{SocketID: 1, Id: 28, Threads: []int{39, 79}, UncoreCaches: []cadvisorapi.Cache{{Id: 0}}},
						},
					},
				},
			},
			want: &CPUTopology{
				NumCPUs:        80,
				NumSockets:     2,
				NumCores:       40,
				NumNUMANodes:   4,
				NumUncoreCache: 1,
				CPUDetails: map[int]CPUInfo{
					0:  {CoreID: 0, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 0},
					1:  {CoreID: 1, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 0},
					2:  {CoreID: 2, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 0},
					3:  {CoreID: 3, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 0},
					4:  {CoreID: 4, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 0},
					5:  {CoreID: 5, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 0},
					6:  {CoreID: 6, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 0},
					7:  {CoreID: 7, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 0},
					8:  {CoreID: 8, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 0},
					9:  {CoreID: 9, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 0},
					10: {CoreID: 10, SocketID: 0, NUMANodeID: 1, UncoreCacheID: 0},
					11: {CoreID: 11, SocketID: 0, NUMANodeID: 1, UncoreCacheID: 0},
					12: {CoreID: 12, SocketID: 0, NUMANodeID: 1, UncoreCacheID: 0},
					13: {CoreID: 13, SocketID: 0, NUMANodeID: 1, UncoreCacheID: 0},
					14: {CoreID: 14, SocketID: 0, NUMANodeID: 1, UncoreCacheID: 0},
					15: {CoreID: 15, SocketID: 0, NUMANodeID: 1, UncoreCacheID: 0},
					16: {CoreID: 16, SocketID: 0, NUMANodeID: 1, UncoreCacheID: 0},
					17: {CoreID: 17, SocketID: 0, NUMANodeID: 1, UncoreCacheID: 0},
					18: {CoreID: 18, SocketID: 0, NUMANodeID: 1, UncoreCacheID: 0},
					19: {CoreID: 19, SocketID: 0, NUMANodeID: 1, UncoreCacheID: 0},
					20: {CoreID: 20, SocketID: 1, NUMANodeID: 2, UncoreCacheID: 0},
					21: {CoreID: 21, SocketID: 1, NUMANodeID: 2, UncoreCacheID: 0},
					22: {CoreID: 22, SocketID: 1, NUMANodeID: 2, UncoreCacheID: 0},
					23: {CoreID: 23, SocketID: 1, NUMANodeID: 2, UncoreCacheID: 0},
					24: {CoreID: 24, SocketID: 1, NUMANodeID: 2, UncoreCacheID: 0},
					25: {CoreID: 25, SocketID: 1, NUMANodeID: 2, UncoreCacheID: 0},
					26: {CoreID: 26, SocketID: 1, NUMANodeID: 2, UncoreCacheID: 0},
					27: {CoreID: 27, SocketID: 1, NUMANodeID: 2, UncoreCacheID: 0},
					28: {CoreID: 28, SocketID: 1, NUMANodeID: 2, UncoreCacheID: 0},
					29: {CoreID: 29, SocketID: 1, NUMANodeID: 2, UncoreCacheID: 0},
					30: {CoreID: 30, SocketID: 1, NUMANodeID: 3, UncoreCacheID: 0},
					31: {CoreID: 31, SocketID: 1, NUMANodeID: 3, UncoreCacheID: 0},
					32: {CoreID: 32, SocketID: 1, NUMANodeID: 3, UncoreCacheID: 0},
					33: {CoreID: 33, SocketID: 1, NUMANodeID: 3, UncoreCacheID: 0},
					34: {CoreID: 34, SocketID: 1, NUMANodeID: 3, UncoreCacheID: 0},
					35: {CoreID: 35, SocketID: 1, NUMANodeID: 3, UncoreCacheID: 0},
					36: {CoreID: 36, SocketID: 1, NUMANodeID: 3, UncoreCacheID: 0},
					37: {CoreID: 37, SocketID: 1, NUMANodeID: 3, UncoreCacheID: 0},
					38: {CoreID: 38, SocketID: 1, NUMANodeID: 3, UncoreCacheID: 0},
					39: {CoreID: 39, SocketID: 1, NUMANodeID: 3, UncoreCacheID: 0},
					40: {CoreID: 0, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 0},
					41: {CoreID: 1, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 0},
					42: {CoreID: 2, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 0},
					43: {CoreID: 3, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 0},
					44: {CoreID: 4, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 0},
					45: {CoreID: 5, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 0},
					46: {CoreID: 6, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 0},
					47: {CoreID: 7, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 0},
					48: {CoreID: 8, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 0},
					49: {CoreID: 9, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 0},
					50: {CoreID: 10, SocketID: 0, NUMANodeID: 1, UncoreCacheID: 0},
					51: {CoreID: 11, SocketID: 0, NUMANodeID: 1, UncoreCacheID: 0},
					52: {CoreID: 12, SocketID: 0, NUMANodeID: 1, UncoreCacheID: 0},
					53: {CoreID: 13, SocketID: 0, NUMANodeID: 1, UncoreCacheID: 0},
					54: {CoreID: 14, SocketID: 0, NUMANodeID: 1, UncoreCacheID: 0},
					55: {CoreID: 15, SocketID: 0, NUMANodeID: 1, UncoreCacheID: 0},
					56: {CoreID: 16, SocketID: 0, NUMANodeID: 1, UncoreCacheID: 0},
					57: {CoreID: 17, SocketID: 0, NUMANodeID: 1, UncoreCacheID: 0},
					58: {CoreID: 18, SocketID: 0, NUMANodeID: 1, UncoreCacheID: 0},
					59: {CoreID: 19, SocketID: 0, NUMANodeID: 1, UncoreCacheID: 0},
					60: {CoreID: 20, SocketID: 1, NUMANodeID: 2, UncoreCacheID: 0},
					61: {CoreID: 21, SocketID: 1, NUMANodeID: 2, UncoreCacheID: 0},
					62: {CoreID: 22, SocketID: 1, NUMANodeID: 2, UncoreCacheID: 0},
					63: {CoreID: 23, SocketID: 1, NUMANodeID: 2, UncoreCacheID: 0},
					64: {CoreID: 24, SocketID: 1, NUMANodeID: 2, UncoreCacheID: 0},
					65: {CoreID: 25, SocketID: 1, NUMANodeID: 2, UncoreCacheID: 0},
					66: {CoreID: 26, SocketID: 1, NUMANodeID: 2, UncoreCacheID: 0},
					67: {CoreID: 27, SocketID: 1, NUMANodeID: 2, UncoreCacheID: 0},
					68: {CoreID: 28, SocketID: 1, NUMANodeID: 2, UncoreCacheID: 0},
					69: {CoreID: 29, SocketID: 1, NUMANodeID: 2, UncoreCacheID: 0},
					70: {CoreID: 30, SocketID: 1, NUMANodeID: 3, UncoreCacheID: 0},
					71: {CoreID: 31, SocketID: 1, NUMANodeID: 3, UncoreCacheID: 0},
					72: {CoreID: 32, SocketID: 1, NUMANodeID: 3, UncoreCacheID: 0},
					73: {CoreID: 33, SocketID: 1, NUMANodeID: 3, UncoreCacheID: 0},
					74: {CoreID: 34, SocketID: 1, NUMANodeID: 3, UncoreCacheID: 0},
					75: {CoreID: 35, SocketID: 1, NUMANodeID: 3, UncoreCacheID: 0},
					76: {CoreID: 36, SocketID: 1, NUMANodeID: 3, UncoreCacheID: 0},
					77: {CoreID: 37, SocketID: 1, NUMANodeID: 3, UncoreCacheID: 0},
					78: {CoreID: 38, SocketID: 1, NUMANodeID: 3, UncoreCacheID: 0},
					79: {CoreID: 39, SocketID: 1, NUMANodeID: 3, UncoreCacheID: 0},
				},
			},
			wantErr: false,
		},
		{

			// FAKE Topology from dual xeon gold 6230
			// (see: dual xeon gold 6230).
			// We flip NUMA cells and Sockets to exercise the code.
			// TODO(fromanirh): replace with a real-world topology
			// once we find a suitable one.
			// Note: this is a fake topology. Thus, there is not a "correct"
			// representation. This one was created following the these concepts:
			// 1. be internally consistent (most important rule)
			// 2. be as close as possible as existing HW topologies
			// 3. if possible, minimize chances wrt existing HW topologies.
			name: "DualNumaMultiSocketPerNumaHT",
			machineInfo: cadvisorapi.MachineInfo{
				NumCores:   80,
				NumSockets: 4,
				Topology: []cadvisorapi.Node{
					{Id: 0,
						Cores: []cadvisorapi.Core{
							{SocketID: 0, Id: 0, Threads: []int{0, 40}, UncoreCaches: []cadvisorapi.Cache{{Id: 1}}},
							{SocketID: 0, Id: 1, Threads: []int{1, 41}, UncoreCaches: []cadvisorapi.Cache{{Id: 1}}},
							{SocketID: 0, Id: 2, Threads: []int{2, 42}, UncoreCaches: []cadvisorapi.Cache{{Id: 1}}},
							{SocketID: 0, Id: 8, Threads: []int{3, 43}, UncoreCaches: []cadvisorapi.Cache{{Id: 1}}},
							{SocketID: 0, Id: 9, Threads: []int{4, 44}, UncoreCaches: []cadvisorapi.Cache{{Id: 1}}},
							{SocketID: 0, Id: 16, Threads: []int{5, 45}, UncoreCaches: []cadvisorapi.Cache{{Id: 1}}},
							{SocketID: 0, Id: 17, Threads: []int{6, 46}, UncoreCaches: []cadvisorapi.Cache{{Id: 1}}},
							{SocketID: 0, Id: 18, Threads: []int{7, 47}, UncoreCaches: []cadvisorapi.Cache{{Id: 1}}},
							{SocketID: 0, Id: 24, Threads: []int{8, 48}, UncoreCaches: []cadvisorapi.Cache{{Id: 1}}},
							{SocketID: 0, Id: 25, Threads: []int{9, 49}, UncoreCaches: []cadvisorapi.Cache{{Id: 1}}},
							{SocketID: 1, Id: 3, Threads: []int{10, 50}, UncoreCaches: []cadvisorapi.Cache{{Id: 1}}},
							{SocketID: 1, Id: 4, Threads: []int{11, 51}, UncoreCaches: []cadvisorapi.Cache{{Id: 1}}},
							{SocketID: 1, Id: 10, Threads: []int{12, 52}, UncoreCaches: []cadvisorapi.Cache{{Id: 1}}},
							{SocketID: 1, Id: 11, Threads: []int{13, 53}, UncoreCaches: []cadvisorapi.Cache{{Id: 1}}},
							{SocketID: 1, Id: 12, Threads: []int{14, 54}, UncoreCaches: []cadvisorapi.Cache{{Id: 1}}},
							{SocketID: 1, Id: 19, Threads: []int{15, 55}, UncoreCaches: []cadvisorapi.Cache{{Id: 1}}},
							{SocketID: 1, Id: 20, Threads: []int{16, 56}, UncoreCaches: []cadvisorapi.Cache{{Id: 1}}},
							{SocketID: 1, Id: 26, Threads: []int{17, 57}, UncoreCaches: []cadvisorapi.Cache{{Id: 1}}},
							{SocketID: 1, Id: 27, Threads: []int{18, 58}, UncoreCaches: []cadvisorapi.Cache{{Id: 1}}},
							{SocketID: 1, Id: 28, Threads: []int{19, 59}, UncoreCaches: []cadvisorapi.Cache{{Id: 1}}},
						},
					},
					{Id: 1,
						Cores: []cadvisorapi.Core{
							{SocketID: 2, Id: 0, Threads: []int{20, 60}, UncoreCaches: []cadvisorapi.Cache{{Id: 1}}},
							{SocketID: 2, Id: 1, Threads: []int{21, 61}, UncoreCaches: []cadvisorapi.Cache{{Id: 1}}},
							{SocketID: 2, Id: 2, Threads: []int{22, 62}, UncoreCaches: []cadvisorapi.Cache{{Id: 1}}},
							{SocketID: 2, Id: 8, Threads: []int{23, 63}, UncoreCaches: []cadvisorapi.Cache{{Id: 1}}},
							{SocketID: 2, Id: 9, Threads: []int{24, 64}, UncoreCaches: []cadvisorapi.Cache{{Id: 1}}},
							{SocketID: 2, Id: 16, Threads: []int{25, 65}, UncoreCaches: []cadvisorapi.Cache{{Id: 1}}},
							{SocketID: 2, Id: 17, Threads: []int{26, 66}, UncoreCaches: []cadvisorapi.Cache{{Id: 1}}},
							{SocketID: 2, Id: 18, Threads: []int{27, 67}, UncoreCaches: []cadvisorapi.Cache{{Id: 1}}},
							{SocketID: 2, Id: 24, Threads: []int{28, 68}, UncoreCaches: []cadvisorapi.Cache{{Id: 1}}},
							{SocketID: 2, Id: 25, Threads: []int{29, 69}, UncoreCaches: []cadvisorapi.Cache{{Id: 1}}},
							{SocketID: 3, Id: 3, Threads: []int{30, 70}, UncoreCaches: []cadvisorapi.Cache{{Id: 1}}},
							{SocketID: 3, Id: 4, Threads: []int{31, 71}, UncoreCaches: []cadvisorapi.Cache{{Id: 1}}},
							{SocketID: 3, Id: 10, Threads: []int{32, 72}, UncoreCaches: []cadvisorapi.Cache{{Id: 1}}},
							{SocketID: 3, Id: 11, Threads: []int{33, 73}, UncoreCaches: []cadvisorapi.Cache{{Id: 1}}},
							{SocketID: 3, Id: 12, Threads: []int{34, 74}, UncoreCaches: []cadvisorapi.Cache{{Id: 1}}},
							{SocketID: 3, Id: 19, Threads: []int{35, 75}, UncoreCaches: []cadvisorapi.Cache{{Id: 1}}},
							{SocketID: 3, Id: 20, Threads: []int{36, 76}, UncoreCaches: []cadvisorapi.Cache{{Id: 1}}},
							{SocketID: 3, Id: 26, Threads: []int{37, 77}, UncoreCaches: []cadvisorapi.Cache{{Id: 1}}},
							{SocketID: 3, Id: 27, Threads: []int{38, 78}, UncoreCaches: []cadvisorapi.Cache{{Id: 1}}},
							{SocketID: 3, Id: 28, Threads: []int{39, 79}, UncoreCaches: []cadvisorapi.Cache{{Id: 1}}},
						},
					},
				},
			},
			want: &CPUTopology{
				NumCPUs:        80,
				NumSockets:     4,
				NumCores:       40,
				NumNUMANodes:   2,
				NumUncoreCache: 1,
				CPUDetails: map[int]CPUInfo{
					0:  {CoreID: 0, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 1},
					1:  {CoreID: 1, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 1},
					2:  {CoreID: 2, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 1},
					3:  {CoreID: 3, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 1},
					4:  {CoreID: 4, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 1},
					5:  {CoreID: 5, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 1},
					6:  {CoreID: 6, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 1},
					7:  {CoreID: 7, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 1},
					8:  {CoreID: 8, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 1},
					9:  {CoreID: 9, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 1},
					10: {CoreID: 10, SocketID: 1, NUMANodeID: 0, UncoreCacheID: 1},
					11: {CoreID: 11, SocketID: 1, NUMANodeID: 0, UncoreCacheID: 1},
					12: {CoreID: 12, SocketID: 1, NUMANodeID: 0, UncoreCacheID: 1},
					13: {CoreID: 13, SocketID: 1, NUMANodeID: 0, UncoreCacheID: 1},
					14: {CoreID: 14, SocketID: 1, NUMANodeID: 0, UncoreCacheID: 1},
					15: {CoreID: 15, SocketID: 1, NUMANodeID: 0, UncoreCacheID: 1},
					16: {CoreID: 16, SocketID: 1, NUMANodeID: 0, UncoreCacheID: 1},
					17: {CoreID: 17, SocketID: 1, NUMANodeID: 0, UncoreCacheID: 1},
					18: {CoreID: 18, SocketID: 1, NUMANodeID: 0, UncoreCacheID: 1},
					19: {CoreID: 19, SocketID: 1, NUMANodeID: 0, UncoreCacheID: 1},
					20: {CoreID: 20, SocketID: 2, NUMANodeID: 1, UncoreCacheID: 1},
					21: {CoreID: 21, SocketID: 2, NUMANodeID: 1, UncoreCacheID: 1},
					22: {CoreID: 22, SocketID: 2, NUMANodeID: 1, UncoreCacheID: 1},
					23: {CoreID: 23, SocketID: 2, NUMANodeID: 1, UncoreCacheID: 1},
					24: {CoreID: 24, SocketID: 2, NUMANodeID: 1, UncoreCacheID: 1},
					25: {CoreID: 25, SocketID: 2, NUMANodeID: 1, UncoreCacheID: 1},
					26: {CoreID: 26, SocketID: 2, NUMANodeID: 1, UncoreCacheID: 1},
					27: {CoreID: 27, SocketID: 2, NUMANodeID: 1, UncoreCacheID: 1},
					28: {CoreID: 28, SocketID: 2, NUMANodeID: 1, UncoreCacheID: 1},
					29: {CoreID: 29, SocketID: 2, NUMANodeID: 1, UncoreCacheID: 1},
					30: {CoreID: 30, SocketID: 3, NUMANodeID: 1, UncoreCacheID: 1},
					31: {CoreID: 31, SocketID: 3, NUMANodeID: 1, UncoreCacheID: 1},
					32: {CoreID: 32, SocketID: 3, NUMANodeID: 1, UncoreCacheID: 1},
					33: {CoreID: 33, SocketID: 3, NUMANodeID: 1, UncoreCacheID: 1},
					34: {CoreID: 34, SocketID: 3, NUMANodeID: 1, UncoreCacheID: 1},
					35: {CoreID: 35, SocketID: 3, NUMANodeID: 1, UncoreCacheID: 1},
					36: {CoreID: 36, SocketID: 3, NUMANodeID: 1, UncoreCacheID: 1},
					37: {CoreID: 37, SocketID: 3, NUMANodeID: 1, UncoreCacheID: 1},
					38: {CoreID: 38, SocketID: 3, NUMANodeID: 1, UncoreCacheID: 1},
					39: {CoreID: 39, SocketID: 3, NUMANodeID: 1, UncoreCacheID: 1},
					40: {CoreID: 0, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 1},
					41: {CoreID: 1, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 1},
					42: {CoreID: 2, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 1},
					43: {CoreID: 3, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 1},
					44: {CoreID: 4, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 1},
					45: {CoreID: 5, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 1},
					46: {CoreID: 6, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 1},
					47: {CoreID: 7, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 1},
					48: {CoreID: 8, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 1},
					49: {CoreID: 9, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 1},
					50: {CoreID: 10, SocketID: 1, NUMANodeID: 0, UncoreCacheID: 1},
					51: {CoreID: 11, SocketID: 1, NUMANodeID: 0, UncoreCacheID: 1},
					52: {CoreID: 12, SocketID: 1, NUMANodeID: 0, UncoreCacheID: 1},
					53: {CoreID: 13, SocketID: 1, NUMANodeID: 0, UncoreCacheID: 1},
					54: {CoreID: 14, SocketID: 1, NUMANodeID: 0, UncoreCacheID: 1},
					55: {CoreID: 15, SocketID: 1, NUMANodeID: 0, UncoreCacheID: 1},
					56: {CoreID: 16, SocketID: 1, NUMANodeID: 0, UncoreCacheID: 1},
					57: {CoreID: 17, SocketID: 1, NUMANodeID: 0, UncoreCacheID: 1},
					58: {CoreID: 18, SocketID: 1, NUMANodeID: 0, UncoreCacheID: 1},
					59: {CoreID: 19, SocketID: 1, NUMANodeID: 0, UncoreCacheID: 1},
					60: {CoreID: 20, SocketID: 2, NUMANodeID: 1, UncoreCacheID: 1},
					61: {CoreID: 21, SocketID: 2, NUMANodeID: 1, UncoreCacheID: 1},
					62: {CoreID: 22, SocketID: 2, NUMANodeID: 1, UncoreCacheID: 1},
					63: {CoreID: 23, SocketID: 2, NUMANodeID: 1, UncoreCacheID: 1},
					64: {CoreID: 24, SocketID: 2, NUMANodeID: 1, UncoreCacheID: 1},
					65: {CoreID: 25, SocketID: 2, NUMANodeID: 1, UncoreCacheID: 1},
					66: {CoreID: 26, SocketID: 2, NUMANodeID: 1, UncoreCacheID: 1},
					67: {CoreID: 27, SocketID: 2, NUMANodeID: 1, UncoreCacheID: 1},
					68: {CoreID: 28, SocketID: 2, NUMANodeID: 1, UncoreCacheID: 1},
					69: {CoreID: 29, SocketID: 2, NUMANodeID: 1, UncoreCacheID: 1},
					70: {CoreID: 30, SocketID: 3, NUMANodeID: 1, UncoreCacheID: 1},
					71: {CoreID: 31, SocketID: 3, NUMANodeID: 1, UncoreCacheID: 1},
					72: {CoreID: 32, SocketID: 3, NUMANodeID: 1, UncoreCacheID: 1},
					73: {CoreID: 33, SocketID: 3, NUMANodeID: 1, UncoreCacheID: 1},
					74: {CoreID: 34, SocketID: 3, NUMANodeID: 1, UncoreCacheID: 1},
					75: {CoreID: 35, SocketID: 3, NUMANodeID: 1, UncoreCacheID: 1},
					76: {CoreID: 36, SocketID: 3, NUMANodeID: 1, UncoreCacheID: 1},
					77: {CoreID: 37, SocketID: 3, NUMANodeID: 1, UncoreCacheID: 1},
					78: {CoreID: 38, SocketID: 3, NUMANodeID: 1, UncoreCacheID: 1},
					79: {CoreID: 39, SocketID: 3, NUMANodeID: 1, UncoreCacheID: 1},
				},
			},
			wantErr: false,
		},
		{
			name: "DualSocketNoHT",
			machineInfo: cadvisorapi.MachineInfo{
				NumCores:   4,
				NumSockets: 2,
				Topology: []cadvisorapi.Node{
					{Id: 0,
						Cores: []cadvisorapi.Core{
							{SocketID: 0, Id: 0, Threads: []int{0}, UncoreCaches: []cadvisorapi.Cache{{Id: 0}}},
							{SocketID: 0, Id: 2, Threads: []int{2}, UncoreCaches: []cadvisorapi.Cache{{Id: 0}}},
						},
					},
					{Id: 1,
						Cores: []cadvisorapi.Core{
							{SocketID: 1, Id: 1, Threads: []int{1}, UncoreCaches: []cadvisorapi.Cache{{Id: 0}}},
							{SocketID: 1, Id: 3, Threads: []int{3}, UncoreCaches: []cadvisorapi.Cache{{Id: 0}}},
						},
					},
				},
			},
			want: &CPUTopology{
				NumCPUs:        4,
				NumSockets:     2,
				NumCores:       4,
				NumNUMANodes:   2,
				NumUncoreCache: 1,
				CPUDetails: map[int]CPUInfo{
					0: {CoreID: 0, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 0},
					1: {CoreID: 1, SocketID: 1, NUMANodeID: 1, UncoreCacheID: 0},
					2: {CoreID: 2, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 0},
					3: {CoreID: 3, SocketID: 1, NUMANodeID: 1, UncoreCacheID: 0},
				},
			},
			wantErr: false,
		},
		{
			name: "DualSocketHT - non unique Core'ID's",
			machineInfo: cadvisorapi.MachineInfo{
				NumCores:   12,
				NumSockets: 2,
				Topology: []cadvisorapi.Node{
					{Id: 0,
						Cores: []cadvisorapi.Core{
							{SocketID: 0, Id: 0, Threads: []int{0, 6}, UncoreCaches: []cadvisorapi.Cache{{Id: 0}}},
							{SocketID: 0, Id: 1, Threads: []int{1, 7}, UncoreCaches: []cadvisorapi.Cache{{Id: 0}}},
							{SocketID: 0, Id: 2, Threads: []int{2, 8}, UncoreCaches: []cadvisorapi.Cache{{Id: 0}}},
						},
					},
					{Id: 1,
						Cores: []cadvisorapi.Core{
							{SocketID: 1, Id: 0, Threads: []int{3, 9}, UncoreCaches: []cadvisorapi.Cache{{Id: 0}}},
							{SocketID: 1, Id: 1, Threads: []int{4, 10}, UncoreCaches: []cadvisorapi.Cache{{Id: 0}}},
							{SocketID: 1, Id: 2, Threads: []int{5, 11}, UncoreCaches: []cadvisorapi.Cache{{Id: 0}}},
						},
					},
				},
			},
			want: &CPUTopology{
				NumCPUs:        12,
				NumSockets:     2,
				NumCores:       6,
				NumNUMANodes:   2,
				NumUncoreCache: 1,
				CPUDetails: map[int]CPUInfo{
					0:  {CoreID: 0, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 0},
					1:  {CoreID: 1, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 0},
					2:  {CoreID: 2, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 0},
					3:  {CoreID: 3, SocketID: 1, NUMANodeID: 1, UncoreCacheID: 0},
					4:  {CoreID: 4, SocketID: 1, NUMANodeID: 1, UncoreCacheID: 0},
					5:  {CoreID: 5, SocketID: 1, NUMANodeID: 1, UncoreCacheID: 0},
					6:  {CoreID: 0, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 0},
					7:  {CoreID: 1, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 0},
					8:  {CoreID: 2, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 0},
					9:  {CoreID: 3, SocketID: 1, NUMANodeID: 1, UncoreCacheID: 0},
					10: {CoreID: 4, SocketID: 1, NUMANodeID: 1, UncoreCacheID: 0},
					11: {CoreID: 5, SocketID: 1, NUMANodeID: 1, UncoreCacheID: 0},
				},
			},
			wantErr: false,
		},
		{
			name: "OneSocketHT fail",
			machineInfo: cadvisorapi.MachineInfo{
				NumCores:   8,
				NumSockets: 1,
				Topology: []cadvisorapi.Node{
					{Id: 0,
						Cores: []cadvisorapi.Core{
							{SocketID: 0, Id: 0, Threads: []int{0, 4}, UncoreCaches: []cadvisorapi.Cache{{Id: 1}}},
							{SocketID: 0, Id: 1, Threads: []int{1, 5}, UncoreCaches: []cadvisorapi.Cache{{Id: 1}}},
							{SocketID: 0, Id: 2, Threads: []int{2, 2}, UncoreCaches: []cadvisorapi.Cache{{Id: 1}}}, // Wrong case - should fail here
							{SocketID: 0, Id: 3, Threads: []int{3, 7}, UncoreCaches: []cadvisorapi.Cache{{Id: 1}}},
						},
					},
				},
			},
			want:    &CPUTopology{},
			wantErr: true,
		},
		{
			name: "OneSocketHT fail",
			machineInfo: cadvisorapi.MachineInfo{
				NumCores:   8,
				NumSockets: 1,
				Topology: []cadvisorapi.Node{
					{Id: 0,
						Cores: []cadvisorapi.Core{
							{SocketID: 0, Id: 0, Threads: []int{0, 4}, UncoreCaches: []cadvisorapi.Cache{{Id: 1}}},
							{SocketID: 0, Id: 1, Threads: []int{1, 5}, UncoreCaches: []cadvisorapi.Cache{{Id: 1}}},
							{SocketID: 0, Id: 2, Threads: []int{2, 6}, UncoreCaches: []cadvisorapi.Cache{{Id: 1}}},
							{SocketID: 0, Id: 3, Threads: []int{}, UncoreCaches: []cadvisorapi.Cache{{Id: 1}}}, // Wrong case - should fail here
						},
					},
				},
			},
			want:    &CPUTopology{},
			wantErr: true,
		},
		{
			// Topology from AMD EPYC 7452 (Rome) 32-Core Processor
			// 4 cores per LLC
			// Single-socket SMT-disabled
			// NPS=1
			name: "UncoreOneSocketNoSMT",
			machineInfo: cadvisorapi.MachineInfo{
				NumCores:   32,
				NumSockets: 1,
				Topology: []cadvisorapi.Node{
					{Id: 0,
						Cores: []cadvisorapi.Core{
							{SocketID: 0, Id: 0, Threads: []int{0}, UncoreCaches: []cadvisorapi.Cache{{Id: 0}}},
							{SocketID: 0, Id: 1, Threads: []int{1}, UncoreCaches: []cadvisorapi.Cache{{Id: 0}}},
							{SocketID: 0, Id: 2, Threads: []int{2}, UncoreCaches: []cadvisorapi.Cache{{Id: 0}}},
							{SocketID: 0, Id: 3, Threads: []int{3}, UncoreCaches: []cadvisorapi.Cache{{Id: 0}}},
							{SocketID: 0, Id: 4, Threads: []int{4}, UncoreCaches: []cadvisorapi.Cache{{Id: 1}}},
							{SocketID: 0, Id: 5, Threads: []int{5}, UncoreCaches: []cadvisorapi.Cache{{Id: 1}}},
							{SocketID: 0, Id: 6, Threads: []int{6}, UncoreCaches: []cadvisorapi.Cache{{Id: 1}}},
							{SocketID: 0, Id: 7, Threads: []int{7}, UncoreCaches: []cadvisorapi.Cache{{Id: 1}}},
							{SocketID: 0, Id: 8, Threads: []int{8}, UncoreCaches: []cadvisorapi.Cache{{Id: 2}}},
							{SocketID: 0, Id: 9, Threads: []int{9}, UncoreCaches: []cadvisorapi.Cache{{Id: 2}}},
							{SocketID: 0, Id: 10, Threads: []int{10}, UncoreCaches: []cadvisorapi.Cache{{Id: 2}}},
							{SocketID: 0, Id: 11, Threads: []int{11}, UncoreCaches: []cadvisorapi.Cache{{Id: 2}}},
							{SocketID: 0, Id: 12, Threads: []int{12}, UncoreCaches: []cadvisorapi.Cache{{Id: 3}}},
							{SocketID: 0, Id: 13, Threads: []int{13}, UncoreCaches: []cadvisorapi.Cache{{Id: 3}}},
							{SocketID: 0, Id: 14, Threads: []int{14}, UncoreCaches: []cadvisorapi.Cache{{Id: 3}}},
							{SocketID: 0, Id: 15, Threads: []int{15}, UncoreCaches: []cadvisorapi.Cache{{Id: 3}}},
							{SocketID: 0, Id: 16, Threads: []int{16}, UncoreCaches: []cadvisorapi.Cache{{Id: 4}}},
							{SocketID: 0, Id: 17, Threads: []int{17}, UncoreCaches: []cadvisorapi.Cache{{Id: 4}}},
							{SocketID: 0, Id: 18, Threads: []int{18}, UncoreCaches: []cadvisorapi.Cache{{Id: 4}}},
							{SocketID: 0, Id: 19, Threads: []int{19}, UncoreCaches: []cadvisorapi.Cache{{Id: 4}}},
							{SocketID: 0, Id: 20, Threads: []int{20}, UncoreCaches: []cadvisorapi.Cache{{Id: 5}}},
							{SocketID: 0, Id: 21, Threads: []int{21}, UncoreCaches: []cadvisorapi.Cache{{Id: 5}}},
							{SocketID: 0, Id: 22, Threads: []int{22}, UncoreCaches: []cadvisorapi.Cache{{Id: 5}}},
							{SocketID: 0, Id: 23, Threads: []int{23}, UncoreCaches: []cadvisorapi.Cache{{Id: 5}}},
							{SocketID: 0, Id: 24, Threads: []int{24}, UncoreCaches: []cadvisorapi.Cache{{Id: 6}}},
							{SocketID: 0, Id: 25, Threads: []int{25}, UncoreCaches: []cadvisorapi.Cache{{Id: 6}}},
							{SocketID: 0, Id: 26, Threads: []int{26}, UncoreCaches: []cadvisorapi.Cache{{Id: 6}}},
							{SocketID: 0, Id: 27, Threads: []int{27}, UncoreCaches: []cadvisorapi.Cache{{Id: 6}}},
							{SocketID: 0, Id: 28, Threads: []int{28}, UncoreCaches: []cadvisorapi.Cache{{Id: 7}}},
							{SocketID: 0, Id: 29, Threads: []int{29}, UncoreCaches: []cadvisorapi.Cache{{Id: 7}}},
							{SocketID: 0, Id: 30, Threads: []int{30}, UncoreCaches: []cadvisorapi.Cache{{Id: 7}}},
							{SocketID: 0, Id: 31, Threads: []int{31}, UncoreCaches: []cadvisorapi.Cache{{Id: 7}}},
						},
					},
				},
			},
			want: &CPUTopology{
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
			},
			wantErr: false,
		},
		{
			// Topology from AMD EPYC 24-Core Processor
			// 4 cores per LLC
			// Dual-socket SMT-enabled
			// NPS=2
			name: "UncoreDualSocketSMT",
			machineInfo: cadvisorapi.MachineInfo{
				NumCores:   96,
				NumSockets: 2,
				Topology: []cadvisorapi.Node{
					{Id: 0,
						Cores: []cadvisorapi.Core{
							{SocketID: 0, Id: 0, Threads: []int{0, 48}, UncoreCaches: []cadvisorapi.Cache{{Id: 0}}},
							{SocketID: 0, Id: 1, Threads: []int{1, 49}, UncoreCaches: []cadvisorapi.Cache{{Id: 0}}},
							{SocketID: 0, Id: 2, Threads: []int{2, 50}, UncoreCaches: []cadvisorapi.Cache{{Id: 0}}},
							{SocketID: 0, Id: 3, Threads: []int{3, 51}, UncoreCaches: []cadvisorapi.Cache{{Id: 0}}},
							{SocketID: 0, Id: 4, Threads: []int{4, 52}, UncoreCaches: []cadvisorapi.Cache{{Id: 1}}},
							{SocketID: 0, Id: 5, Threads: []int{5, 53}, UncoreCaches: []cadvisorapi.Cache{{Id: 1}}},
							{SocketID: 0, Id: 6, Threads: []int{6, 54}, UncoreCaches: []cadvisorapi.Cache{{Id: 1}}},
							{SocketID: 0, Id: 7, Threads: []int{7, 55}, UncoreCaches: []cadvisorapi.Cache{{Id: 1}}},
							{SocketID: 0, Id: 8, Threads: []int{8, 56}, UncoreCaches: []cadvisorapi.Cache{{Id: 2}}},
							{SocketID: 0, Id: 9, Threads: []int{9, 57}, UncoreCaches: []cadvisorapi.Cache{{Id: 2}}},
							{SocketID: 0, Id: 10, Threads: []int{10, 58}, UncoreCaches: []cadvisorapi.Cache{{Id: 2}}},
							{SocketID: 0, Id: 11, Threads: []int{11, 59}, UncoreCaches: []cadvisorapi.Cache{{Id: 2}}},
						},
					},
					{Id: 1,
						Cores: []cadvisorapi.Core{
							{SocketID: 0, Id: 12, Threads: []int{12, 60}, UncoreCaches: []cadvisorapi.Cache{{Id: 3}}},
							{SocketID: 0, Id: 13, Threads: []int{13, 61}, UncoreCaches: []cadvisorapi.Cache{{Id: 3}}},
							{SocketID: 0, Id: 14, Threads: []int{14, 62}, UncoreCaches: []cadvisorapi.Cache{{Id: 3}}},
							{SocketID: 0, Id: 15, Threads: []int{15, 63}, UncoreCaches: []cadvisorapi.Cache{{Id: 3}}},
							{SocketID: 0, Id: 16, Threads: []int{16, 64}, UncoreCaches: []cadvisorapi.Cache{{Id: 4}}},
							{SocketID: 0, Id: 17, Threads: []int{17, 65}, UncoreCaches: []cadvisorapi.Cache{{Id: 4}}},
							{SocketID: 0, Id: 18, Threads: []int{18, 66}, UncoreCaches: []cadvisorapi.Cache{{Id: 4}}},
							{SocketID: 0, Id: 19, Threads: []int{19, 67}, UncoreCaches: []cadvisorapi.Cache{{Id: 4}}},
							{SocketID: 0, Id: 20, Threads: []int{20, 68}, UncoreCaches: []cadvisorapi.Cache{{Id: 5}}},
							{SocketID: 0, Id: 21, Threads: []int{21, 69}, UncoreCaches: []cadvisorapi.Cache{{Id: 5}}},
							{SocketID: 0, Id: 22, Threads: []int{22, 70}, UncoreCaches: []cadvisorapi.Cache{{Id: 5}}},
							{SocketID: 0, Id: 23, Threads: []int{23, 71}, UncoreCaches: []cadvisorapi.Cache{{Id: 5}}},
						},
					},
					{Id: 2,
						Cores: []cadvisorapi.Core{
							{SocketID: 1, Id: 24, Threads: []int{24, 72}, UncoreCaches: []cadvisorapi.Cache{{Id: 6}}},
							{SocketID: 1, Id: 25, Threads: []int{25, 73}, UncoreCaches: []cadvisorapi.Cache{{Id: 6}}},
							{SocketID: 1, Id: 26, Threads: []int{26, 74}, UncoreCaches: []cadvisorapi.Cache{{Id: 6}}},
							{SocketID: 1, Id: 27, Threads: []int{27, 75}, UncoreCaches: []cadvisorapi.Cache{{Id: 6}}},
							{SocketID: 1, Id: 28, Threads: []int{28, 76}, UncoreCaches: []cadvisorapi.Cache{{Id: 7}}},
							{SocketID: 1, Id: 29, Threads: []int{29, 77}, UncoreCaches: []cadvisorapi.Cache{{Id: 7}}},
							{SocketID: 1, Id: 30, Threads: []int{30, 78}, UncoreCaches: []cadvisorapi.Cache{{Id: 7}}},
							{SocketID: 1, Id: 31, Threads: []int{31, 79}, UncoreCaches: []cadvisorapi.Cache{{Id: 7}}},
							{SocketID: 1, Id: 32, Threads: []int{32, 80}, UncoreCaches: []cadvisorapi.Cache{{Id: 8}}},
							{SocketID: 1, Id: 33, Threads: []int{33, 81}, UncoreCaches: []cadvisorapi.Cache{{Id: 8}}},
							{SocketID: 1, Id: 34, Threads: []int{34, 82}, UncoreCaches: []cadvisorapi.Cache{{Id: 8}}},
							{SocketID: 1, Id: 35, Threads: []int{35, 83}, UncoreCaches: []cadvisorapi.Cache{{Id: 8}}},
						},
					},
					{Id: 3,
						Cores: []cadvisorapi.Core{
							{SocketID: 1, Id: 36, Threads: []int{36, 84}, UncoreCaches: []cadvisorapi.Cache{{Id: 9}}},
							{SocketID: 1, Id: 37, Threads: []int{37, 85}, UncoreCaches: []cadvisorapi.Cache{{Id: 9}}},
							{SocketID: 1, Id: 38, Threads: []int{38, 86}, UncoreCaches: []cadvisorapi.Cache{{Id: 9}}},
							{SocketID: 1, Id: 39, Threads: []int{39, 87}, UncoreCaches: []cadvisorapi.Cache{{Id: 9}}},
							{SocketID: 1, Id: 40, Threads: []int{40, 88}, UncoreCaches: []cadvisorapi.Cache{{Id: 10}}},
							{SocketID: 1, Id: 41, Threads: []int{41, 89}, UncoreCaches: []cadvisorapi.Cache{{Id: 10}}},
							{SocketID: 1, Id: 42, Threads: []int{42, 90}, UncoreCaches: []cadvisorapi.Cache{{Id: 10}}},
							{SocketID: 1, Id: 43, Threads: []int{43, 91}, UncoreCaches: []cadvisorapi.Cache{{Id: 10}}},
							{SocketID: 1, Id: 44, Threads: []int{44, 92}, UncoreCaches: []cadvisorapi.Cache{{Id: 11}}},
							{SocketID: 1, Id: 45, Threads: []int{45, 93}, UncoreCaches: []cadvisorapi.Cache{{Id: 11}}},
							{SocketID: 1, Id: 46, Threads: []int{46, 94}, UncoreCaches: []cadvisorapi.Cache{{Id: 11}}},
							{SocketID: 1, Id: 47, Threads: []int{47, 95}, UncoreCaches: []cadvisorapi.Cache{{Id: 11}}},
						},
					},
				},
			},
			want: &CPUTopology{
				NumCPUs:        96,
				NumSockets:     2,
				NumCores:       48,
				NumNUMANodes:   4,
				NumUncoreCache: 12,
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
					12: {CoreID: 12, SocketID: 0, NUMANodeID: 1, UncoreCacheID: 3},
					13: {CoreID: 13, SocketID: 0, NUMANodeID: 1, UncoreCacheID: 3},
					14: {CoreID: 14, SocketID: 0, NUMANodeID: 1, UncoreCacheID: 3},
					15: {CoreID: 15, SocketID: 0, NUMANodeID: 1, UncoreCacheID: 3},
					16: {CoreID: 16, SocketID: 0, NUMANodeID: 1, UncoreCacheID: 4},
					17: {CoreID: 17, SocketID: 0, NUMANodeID: 1, UncoreCacheID: 4},
					18: {CoreID: 18, SocketID: 0, NUMANodeID: 1, UncoreCacheID: 4},
					19: {CoreID: 19, SocketID: 0, NUMANodeID: 1, UncoreCacheID: 4},
					20: {CoreID: 20, SocketID: 0, NUMANodeID: 1, UncoreCacheID: 5},
					21: {CoreID: 21, SocketID: 0, NUMANodeID: 1, UncoreCacheID: 5},
					22: {CoreID: 22, SocketID: 0, NUMANodeID: 1, UncoreCacheID: 5},
					23: {CoreID: 23, SocketID: 0, NUMANodeID: 1, UncoreCacheID: 5},
					24: {CoreID: 24, SocketID: 1, NUMANodeID: 2, UncoreCacheID: 6},
					25: {CoreID: 25, SocketID: 1, NUMANodeID: 2, UncoreCacheID: 6},
					26: {CoreID: 26, SocketID: 1, NUMANodeID: 2, UncoreCacheID: 6},
					27: {CoreID: 27, SocketID: 1, NUMANodeID: 2, UncoreCacheID: 6},
					28: {CoreID: 28, SocketID: 1, NUMANodeID: 2, UncoreCacheID: 7},
					29: {CoreID: 29, SocketID: 1, NUMANodeID: 2, UncoreCacheID: 7},
					30: {CoreID: 30, SocketID: 1, NUMANodeID: 2, UncoreCacheID: 7},
					31: {CoreID: 31, SocketID: 1, NUMANodeID: 2, UncoreCacheID: 7},
					32: {CoreID: 32, SocketID: 1, NUMANodeID: 2, UncoreCacheID: 8},
					33: {CoreID: 33, SocketID: 1, NUMANodeID: 2, UncoreCacheID: 8},
					34: {CoreID: 34, SocketID: 1, NUMANodeID: 2, UncoreCacheID: 8},
					35: {CoreID: 35, SocketID: 1, NUMANodeID: 2, UncoreCacheID: 8},
					36: {CoreID: 36, SocketID: 1, NUMANodeID: 3, UncoreCacheID: 9},
					37: {CoreID: 37, SocketID: 1, NUMANodeID: 3, UncoreCacheID: 9},
					38: {CoreID: 38, SocketID: 1, NUMANodeID: 3, UncoreCacheID: 9},
					39: {CoreID: 39, SocketID: 1, NUMANodeID: 3, UncoreCacheID: 9},
					40: {CoreID: 40, SocketID: 1, NUMANodeID: 3, UncoreCacheID: 10},
					41: {CoreID: 41, SocketID: 1, NUMANodeID: 3, UncoreCacheID: 10},
					42: {CoreID: 42, SocketID: 1, NUMANodeID: 3, UncoreCacheID: 10},
					43: {CoreID: 43, SocketID: 1, NUMANodeID: 3, UncoreCacheID: 10},
					44: {CoreID: 44, SocketID: 1, NUMANodeID: 3, UncoreCacheID: 11},
					45: {CoreID: 45, SocketID: 1, NUMANodeID: 3, UncoreCacheID: 11},
					46: {CoreID: 46, SocketID: 1, NUMANodeID: 3, UncoreCacheID: 11},
					47: {CoreID: 47, SocketID: 1, NUMANodeID: 3, UncoreCacheID: 11},
					48: {CoreID: 0, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 0},
					49: {CoreID: 1, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 0},
					50: {CoreID: 2, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 0},
					51: {CoreID: 3, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 0},
					52: {CoreID: 4, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 1},
					53: {CoreID: 5, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 1},
					54: {CoreID: 6, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 1},
					55: {CoreID: 7, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 1},
					56: {CoreID: 8, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 2},
					57: {CoreID: 9, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 2},
					58: {CoreID: 10, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 2},
					59: {CoreID: 11, SocketID: 0, NUMANodeID: 0, UncoreCacheID: 2},
					60: {CoreID: 12, SocketID: 0, NUMANodeID: 1, UncoreCacheID: 3},
					61: {CoreID: 13, SocketID: 0, NUMANodeID: 1, UncoreCacheID: 3},
					62: {CoreID: 14, SocketID: 0, NUMANodeID: 1, UncoreCacheID: 3},
					63: {CoreID: 15, SocketID: 0, NUMANodeID: 1, UncoreCacheID: 3},
					64: {CoreID: 16, SocketID: 0, NUMANodeID: 1, UncoreCacheID: 4},
					65: {CoreID: 17, SocketID: 0, NUMANodeID: 1, UncoreCacheID: 4},
					66: {CoreID: 18, SocketID: 0, NUMANodeID: 1, UncoreCacheID: 4},
					67: {CoreID: 19, SocketID: 0, NUMANodeID: 1, UncoreCacheID: 4},
					68: {CoreID: 20, SocketID: 0, NUMANodeID: 1, UncoreCacheID: 5},
					69: {CoreID: 21, SocketID: 0, NUMANodeID: 1, UncoreCacheID: 5},
					70: {CoreID: 22, SocketID: 0, NUMANodeID: 1, UncoreCacheID: 5},
					71: {CoreID: 23, SocketID: 0, NUMANodeID: 1, UncoreCacheID: 5},
					72: {CoreID: 24, SocketID: 1, NUMANodeID: 2, UncoreCacheID: 6},
					73: {CoreID: 25, SocketID: 1, NUMANodeID: 2, UncoreCacheID: 6},
					74: {CoreID: 26, SocketID: 1, NUMANodeID: 2, UncoreCacheID: 6},
					75: {CoreID: 27, SocketID: 1, NUMANodeID: 2, UncoreCacheID: 6},
					76: {CoreID: 28, SocketID: 1, NUMANodeID: 2, UncoreCacheID: 7},
					77: {CoreID: 29, SocketID: 1, NUMANodeID: 2, UncoreCacheID: 7},
					78: {CoreID: 30, SocketID: 1, NUMANodeID: 2, UncoreCacheID: 7},
					79: {CoreID: 31, SocketID: 1, NUMANodeID: 2, UncoreCacheID: 7},
					80: {CoreID: 32, SocketID: 1, NUMANodeID: 2, UncoreCacheID: 8},
					81: {CoreID: 33, SocketID: 1, NUMANodeID: 2, UncoreCacheID: 8},
					82: {CoreID: 34, SocketID: 1, NUMANodeID: 2, UncoreCacheID: 8},
					83: {CoreID: 35, SocketID: 1, NUMANodeID: 2, UncoreCacheID: 8},
					84: {CoreID: 36, SocketID: 1, NUMANodeID: 3, UncoreCacheID: 9},
					85: {CoreID: 37, SocketID: 1, NUMANodeID: 3, UncoreCacheID: 9},
					86: {CoreID: 38, SocketID: 1, NUMANodeID: 3, UncoreCacheID: 9},
					87: {CoreID: 39, SocketID: 1, NUMANodeID: 3, UncoreCacheID: 9},
					88: {CoreID: 40, SocketID: 1, NUMANodeID: 3, UncoreCacheID: 10},
					89: {CoreID: 41, SocketID: 1, NUMANodeID: 3, UncoreCacheID: 10},
					90: {CoreID: 42, SocketID: 1, NUMANodeID: 3, UncoreCacheID: 10},
					91: {CoreID: 43, SocketID: 1, NUMANodeID: 3, UncoreCacheID: 10},
					92: {CoreID: 44, SocketID: 1, NUMANodeID: 3, UncoreCacheID: 11},
					93: {CoreID: 45, SocketID: 1, NUMANodeID: 3, UncoreCacheID: 11},
					94: {CoreID: 46, SocketID: 1, NUMANodeID: 3, UncoreCacheID: 11},
					95: {CoreID: 47, SocketID: 1, NUMANodeID: 3, UncoreCacheID: 11},
				},
			},
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Discover(&tt.machineInfo)
			if err != nil {
				if tt.wantErr {
					t.Logf("Discover() expected error = %v", err)
				} else {
					t.Errorf("Discover() error = %v, wantErr %v", err, tt.wantErr)
				}
				return
			}
			if diff := cmp.Diff(got, tt.want); diff != "" {
				t.Errorf("Discover() = %v, want %v diff=%s", got, tt.want, diff)
			}
		})
	}
}

func TestCPUDetailsKeepOnly(t *testing.T) {
	details := CPUDetails{
		0: {},
		1: {},
		2: {},
	}

	tests := []struct {
		name string
		cpus cpuset.CPUSet
		want CPUDetails
	}{{
		name: "cpus is in CPUDetails.",
		cpus: cpuset.New(0, 1),
		want: map[int]CPUInfo{
			0: {},
			1: {},
		},
	}, {
		name: "cpus is not in CPUDetails.",
		cpus: cpuset.New(3),
		want: CPUDetails{},
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := details.KeepOnly(tt.cpus)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("KeepOnly() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCPUDetailsNUMANodes(t *testing.T) {
	tests := []struct {
		name    string
		details CPUDetails
		want    cpuset.CPUSet
	}{{
		name: "Get CPUset of NUMANode IDs",
		details: map[int]CPUInfo{
			0: {NUMANodeID: 0},
			1: {NUMANodeID: 0},
			2: {NUMANodeID: 1},
			3: {NUMANodeID: 1},
		},
		want: cpuset.New(0, 1),
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.details.NUMANodes()
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("NUMANodes() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCPUDetailsNUMANodesInSockets(t *testing.T) {
	details1 := CPUDetails{
		0: {SocketID: 0, NUMANodeID: 0},
		1: {SocketID: 1, NUMANodeID: 0},
		2: {SocketID: 2, NUMANodeID: 1},
		3: {SocketID: 3, NUMANodeID: 1},
	}

	// poorly designed mainboards
	details2 := CPUDetails{
		0: {SocketID: 0, NUMANodeID: 0},
		1: {SocketID: 0, NUMANodeID: 1},
		2: {SocketID: 1, NUMANodeID: 2},
		3: {SocketID: 1, NUMANodeID: 3},
	}

	tests := []struct {
		name    string
		details CPUDetails
		ids     []int
		want    cpuset.CPUSet
	}{{
		name:    "Socket IDs is in CPUDetails.",
		details: details1,
		ids:     []int{0, 1, 2},
		want:    cpuset.New(0, 1),
	}, {
		name:    "Socket IDs is not in CPUDetails.",
		details: details1,
		ids:     []int{4},
		want:    cpuset.New(),
	}, {
		name:    "Socket IDs is in CPUDetails. (poorly designed mainboards)",
		details: details2,
		ids:     []int{0},
		want:    cpuset.New(0, 1),
	}, {
		name:    "Socket IDs is not in CPUDetails. (poorly designed mainboards)",
		details: details2,
		ids:     []int{3},
		want:    cpuset.New(),
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.details.NUMANodesInSockets(tt.ids...)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("NUMANodesInSockets() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCPUDetailsSockets(t *testing.T) {
	tests := []struct {
		name    string
		details CPUDetails
		want    cpuset.CPUSet
	}{{
		name: "Get CPUset of Socket IDs",
		details: map[int]CPUInfo{
			0: {SocketID: 0},
			1: {SocketID: 0},
			2: {SocketID: 1},
			3: {SocketID: 1},
		},
		want: cpuset.New(0, 1),
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.details.Sockets()
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Sockets() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCPUDetailsCPUsInSockets(t *testing.T) {
	details := CPUDetails{
		0: {SocketID: 0},
		1: {SocketID: 0},
		2: {SocketID: 1},
		3: {SocketID: 2},
	}

	tests := []struct {
		name string
		ids  []int
		want cpuset.CPUSet
	}{{
		name: "Socket IDs is in CPUDetails.",
		ids:  []int{0, 1},
		want: cpuset.New(0, 1, 2),
	}, {
		name: "Socket IDs is not in CPUDetails.",
		ids:  []int{3},
		want: cpuset.New(),
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := details.CPUsInSockets(tt.ids...)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("CPUsInSockets() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCPUDetailsSocketsInNUMANodes(t *testing.T) {
	details := CPUDetails{
		0: {NUMANodeID: 0, SocketID: 0},
		1: {NUMANodeID: 0, SocketID: 1},
		2: {NUMANodeID: 1, SocketID: 2},
		3: {NUMANodeID: 2, SocketID: 3},
	}

	tests := []struct {
		name string
		ids  []int
		want cpuset.CPUSet
	}{{
		name: "NUMANodes IDs is in CPUDetails.",
		ids:  []int{0, 1},
		want: cpuset.New(0, 1, 2),
	}, {
		name: "NUMANodes IDs is not in CPUDetails.",
		ids:  []int{3},
		want: cpuset.New(),
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := details.SocketsInNUMANodes(tt.ids...)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("SocketsInNUMANodes() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCPUDetailsCores(t *testing.T) {
	tests := []struct {
		name    string
		details CPUDetails
		want    cpuset.CPUSet
	}{{
		name: "Get CPUset of Cores",
		details: map[int]CPUInfo{
			0: {CoreID: 0},
			1: {CoreID: 0},
			2: {CoreID: 1},
			3: {CoreID: 1},
		},
		want: cpuset.New(0, 1),
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.details.Cores()
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Cores() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCPUDetailsCoresInNUMANodes(t *testing.T) {
	details := CPUDetails{
		0: {NUMANodeID: 0, CoreID: 0},
		1: {NUMANodeID: 0, CoreID: 1},
		2: {NUMANodeID: 1, CoreID: 2},
		3: {NUMANodeID: 2, CoreID: 3},
	}

	tests := []struct {
		name string
		ids  []int
		want cpuset.CPUSet
	}{{
		name: "NUMANodes IDs is in CPUDetails.",
		ids:  []int{0, 1},
		want: cpuset.New(0, 1, 2),
	}, {
		name: "NUMANodes IDs is not in CPUDetails.",
		ids:  []int{3},
		want: cpuset.New(),
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := details.CoresInNUMANodes(tt.ids...)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("CoresInNUMANodes() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCPUDetailsCoresInSockets(t *testing.T) {
	details := CPUDetails{
		0: {SocketID: 0, CoreID: 0},
		1: {SocketID: 0, CoreID: 1},
		2: {SocketID: 1, CoreID: 2},
		3: {SocketID: 2, CoreID: 3},
	}

	tests := []struct {
		name string
		ids  []int
		want cpuset.CPUSet
	}{{
		name: "Socket IDs is in CPUDetails.",
		ids:  []int{0, 1},
		want: cpuset.New(0, 1, 2),
	}, {
		name: "Socket IDs is not in CPUDetails.",
		ids:  []int{3},
		want: cpuset.New(),
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := details.CoresInSockets(tt.ids...)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("CoresInSockets() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCPUDetailsCPUs(t *testing.T) {
	tests := []struct {
		name    string
		details CPUDetails
		want    cpuset.CPUSet
	}{{
		name: "Get CPUset of CPUs",
		details: map[int]CPUInfo{
			0: {},
			1: {},
		},
		want: cpuset.New(0, 1),
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.details.CPUs()
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("CPUs() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCPUDetailsCPUsInNUMANodes(t *testing.T) {
	details := CPUDetails{
		0: {NUMANodeID: 0},
		1: {NUMANodeID: 0},
		2: {NUMANodeID: 1},
		3: {NUMANodeID: 2},
	}

	tests := []struct {
		name string
		ids  []int
		want cpuset.CPUSet
	}{{
		name: "NUMANode IDs is in CPUDetails.",
		ids:  []int{0, 1},
		want: cpuset.New(0, 1, 2),
	}, {
		name: "NUMANode IDs is not in CPUDetails.",
		ids:  []int{3},
		want: cpuset.New(),
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := details.CPUsInNUMANodes(tt.ids...)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("CPUsInNUMANodes() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCPUDetailsCPUsInCores(t *testing.T) {
	details := CPUDetails{
		0: {CoreID: 0},
		1: {CoreID: 0},
		2: {CoreID: 1},
		3: {CoreID: 2},
	}

	tests := []struct {
		name string
		ids  []int
		want cpuset.CPUSet
	}{{
		name: "Core IDs is in CPUDetails.",
		ids:  []int{0, 1},
		want: cpuset.New(0, 1, 2),
	}, {
		name: "Core IDs is not in CPUDetails.",
		ids:  []int{3},
		want: cpuset.New(),
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := details.CPUsInCores(tt.ids...)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("CPUsInCores() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCPUCoreID(t *testing.T) {
	topoDualSocketHT := &CPUTopology{
		NumCPUs:    12,
		NumSockets: 2,
		NumCores:   6,
		CPUDetails: map[int]CPUInfo{
			0:  {CoreID: 0, SocketID: 0, NUMANodeID: 0},
			1:  {CoreID: 1, SocketID: 1, NUMANodeID: 1},
			2:  {CoreID: 2, SocketID: 0, NUMANodeID: 0},
			3:  {CoreID: 3, SocketID: 1, NUMANodeID: 1},
			4:  {CoreID: 4, SocketID: 0, NUMANodeID: 0},
			5:  {CoreID: 5, SocketID: 1, NUMANodeID: 1},
			6:  {CoreID: 0, SocketID: 0, NUMANodeID: 0},
			7:  {CoreID: 1, SocketID: 1, NUMANodeID: 1},
			8:  {CoreID: 2, SocketID: 0, NUMANodeID: 0},
			9:  {CoreID: 3, SocketID: 1, NUMANodeID: 1},
			10: {CoreID: 4, SocketID: 0, NUMANodeID: 0},
			11: {CoreID: 5, SocketID: 1, NUMANodeID: 1},
		},
	}

	tests := []struct {
		name    string
		topo    *CPUTopology
		id      int
		want    int
		wantErr bool
	}{{
		name: "Known Core ID",
		topo: topoDualSocketHT,
		id:   2,
		want: 2,
	}, {
		name: "Known Core ID (core sibling).",
		topo: topoDualSocketHT,
		id:   8,
		want: 2,
	}, {
		name:    "Unknown Core ID.",
		topo:    topoDualSocketHT,
		id:      -2,
		want:    -1,
		wantErr: true,
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.topo.CPUCoreID(tt.id)
			gotErr := (err != nil)
			if gotErr != tt.wantErr {
				t.Errorf("CPUCoreID() returned err %v, want %v", gotErr, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("CPUCoreID() returned %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCPUSocketID(t *testing.T) {
	topoDualSocketHT := &CPUTopology{
		NumCPUs:    12,
		NumSockets: 2,
		NumCores:   6,
		CPUDetails: map[int]CPUInfo{
			0:  {CoreID: 0, SocketID: 0, NUMANodeID: 0},
			1:  {CoreID: 1, SocketID: 1, NUMANodeID: 1},
			2:  {CoreID: 2, SocketID: 0, NUMANodeID: 0},
			3:  {CoreID: 3, SocketID: 1, NUMANodeID: 1},
			4:  {CoreID: 4, SocketID: 0, NUMANodeID: 0},
			5:  {CoreID: 5, SocketID: 1, NUMANodeID: 1},
			6:  {CoreID: 0, SocketID: 0, NUMANodeID: 0},
			7:  {CoreID: 1, SocketID: 1, NUMANodeID: 1},
			8:  {CoreID: 2, SocketID: 0, NUMANodeID: 0},
			9:  {CoreID: 3, SocketID: 1, NUMANodeID: 1},
			10: {CoreID: 4, SocketID: 0, NUMANodeID: 0},
			11: {CoreID: 5, SocketID: 1, NUMANodeID: 1},
		},
	}

	tests := []struct {
		name    string
		topo    *CPUTopology
		id      int
		want    int
		wantErr bool
	}{{
		name: "Known Core ID",
		topo: topoDualSocketHT,
		id:   3,
		want: 1,
	}, {
		name: "Known Core ID (core sibling).",
		topo: topoDualSocketHT,
		id:   9,
		want: 1,
	}, {
		name:    "Unknown Core ID.",
		topo:    topoDualSocketHT,
		id:      1000,
		want:    -1,
		wantErr: true,
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.topo.CPUSocketID(tt.id)
			gotErr := (err != nil)
			if gotErr != tt.wantErr {
				t.Errorf("CPUSocketID() returned err %v, want %v", gotErr, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("CPUSocketID() returned %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCPUNUMANodeID(t *testing.T) {
	topoDualSocketHT := &CPUTopology{
		NumCPUs:    12,
		NumSockets: 2,
		NumCores:   6,
		CPUDetails: map[int]CPUInfo{
			0:  {CoreID: 0, SocketID: 0, NUMANodeID: 0},
			1:  {CoreID: 1, SocketID: 1, NUMANodeID: 1},
			2:  {CoreID: 2, SocketID: 0, NUMANodeID: 0},
			3:  {CoreID: 3, SocketID: 1, NUMANodeID: 1},
			4:  {CoreID: 4, SocketID: 0, NUMANodeID: 0},
			5:  {CoreID: 5, SocketID: 1, NUMANodeID: 1},
			6:  {CoreID: 0, SocketID: 0, NUMANodeID: 0},
			7:  {CoreID: 1, SocketID: 1, NUMANodeID: 1},
			8:  {CoreID: 2, SocketID: 0, NUMANodeID: 0},
			9:  {CoreID: 3, SocketID: 1, NUMANodeID: 1},
			10: {CoreID: 4, SocketID: 0, NUMANodeID: 0},
			11: {CoreID: 5, SocketID: 1, NUMANodeID: 1},
		},
	}

	tests := []struct {
		name    string
		topo    *CPUTopology
		id      int
		want    int
		wantErr bool
	}{{
		name: "Known Core ID",
		topo: topoDualSocketHT,
		id:   0,
		want: 0,
	}, {
		name: "Known Core ID (core sibling).",
		topo: topoDualSocketHT,
		id:   6,
		want: 0,
	}, {
		name:    "Unknown Core ID.",
		topo:    topoDualSocketHT,
		id:      1000,
		want:    -1,
		wantErr: true,
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.topo.CPUNUMANodeID(tt.id)
			gotErr := (err != nil)
			if gotErr != tt.wantErr {
				t.Errorf("CPUSocketID() returned err %v, want %v", gotErr, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("CPUSocketID() returned %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCPUsInUncoreCaches(t *testing.T) {
	tests := []struct {
		name    string
		details CPUDetails
		ids     []int
		want    cpuset.CPUSet
	}{
		{
			name: "Single Uncore Cache",
			details: map[int]CPUInfo{
				0: {UncoreCacheID: 0},
				1: {UncoreCacheID: 0},
			},
			ids:  []int{0},
			want: cpuset.New(0, 1),
		},
		{
			name: "Multiple Uncore Caches",
			details: map[int]CPUInfo{
				0: {UncoreCacheID: 0},
				1: {UncoreCacheID: 0},
				2: {UncoreCacheID: 1},
			},
			ids:  []int{0, 1},
			want: cpuset.New(0, 1, 2),
		},
		{
			name: "Uncore Cache does not exist",
			details: map[int]CPUInfo{
				0: {UncoreCacheID: 0},
			},
			ids:  []int{1},
			want: cpuset.New(),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.details.CPUsInUncoreCaches(tt.ids...)
			if !reflect.DeepEqual(tt.want, got) {
				t.Errorf("CPUsInUncoreCaches() = %v, want %v", got, tt.want)
			}
		})
	}
}
func TestUncoreInNUMANodes(t *testing.T) {
	tests := []struct {
		name    string
		details CPUDetails
		ids     []int
		want    cpuset.CPUSet
	}{
		{
			name: "Single NUMA Node",
			details: map[int]CPUInfo{
				0: {NUMANodeID: 0, UncoreCacheID: 0},
				1: {NUMANodeID: 0, UncoreCacheID: 1},
			},
			ids:  []int{0},
			want: cpuset.New(0, 1),
		},
		{
			name: "Multiple NUMANode",
			details: map[int]CPUInfo{
				0:  {NUMANodeID: 0, UncoreCacheID: 0},
				1:  {NUMANodeID: 0, UncoreCacheID: 0},
				20: {NUMANodeID: 1, UncoreCacheID: 1},
				21: {NUMANodeID: 1, UncoreCacheID: 1},
			},
			ids:  []int{0, 1},
			want: cpuset.New(0, 1),
		},
		{
			name: "Non-Existent NUMANode",
			details: map[int]CPUInfo{
				0: {NUMANodeID: 1, UncoreCacheID: 0},
			},
			ids:  []int{0},
			want: cpuset.New(),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.details.UncoreInNUMANodes(tt.ids...)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("UncoreInNUMANodes()= %v, want %v", got, tt.want)
			}
		})

	}
}

func TestUncoreCaches(t *testing.T) {
	tests := []struct {
		name    string
		details CPUDetails
		want    cpuset.CPUSet
	}{
		{
			name: "Get CPUSet of UncoreCache IDs",
			details: map[int]CPUInfo{
				0: {UncoreCacheID: 0},
				1: {UncoreCacheID: 1},
				2: {UncoreCacheID: 2},
			},
			want: cpuset.New(0, 1, 2),
		},
		{
			name:    "Empty CPUDetails",
			details: map[int]CPUInfo{},
			want:    cpuset.New(),
		},
		{
			name: "Shared UncoreCache",
			details: map[int]CPUInfo{
				0: {UncoreCacheID: 0},
				1: {UncoreCacheID: 0},
				2: {UncoreCacheID: 0},
			},
			want: cpuset.New(0),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.details.UncoreCaches()
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("UncoreCaches() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCPUsPerUncore(t *testing.T) {
	tests := []struct {
		name string
		topo *CPUTopology
		want int
	}{
		{
			name: "Zero Number of UncoreCache",
			topo: &CPUTopology{
				NumCPUs:        8,
				NumUncoreCache: 0,
			},
			want: 0,
		},
		{
			name: "Normal case",
			topo: &CPUTopology{
				NumCPUs:        16,
				NumUncoreCache: 2,
			},
			want: 8,
		},
		{
			name: "Single shared UncoreCache",
			topo: &CPUTopology{
				NumCPUs:        8,
				NumUncoreCache: 1,
			},
			want: 8,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.topo.CPUsPerUncore()
			if got != tt.want {
				t.Errorf("CPUsPerUncore() = %v, want %v", got, tt.want)
			}
		})
	}

}
func Test_getUncoreCacheID(t *testing.T) {
	tests := []struct {
		name string
		args cadvisorapi.Core
		want int
	}{
		{
			name: "Core with uncore cache info",
			args: cadvisorapi.Core{
				SocketID: 1,
				UncoreCaches: []cadvisorapi.Cache{
					{Id: 5},
					{Id: 6},
				},
			},
			want: 5,
		},
		{
			name: "Core with empty uncore cache info",
			args: cadvisorapi.Core{
				SocketID:     2,
				UncoreCaches: []cadvisorapi.Cache{},
			},
			want: 2,
		},
		{
			name: "Core with nil uncore cache info",
			args: cadvisorapi.Core{
				SocketID: 1,
			},
			want: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := getUncoreCacheID(tt.args); got != tt.want {
				t.Errorf("getUncoreCacheID() = %v, want %v", got, tt.want)
			}
		})
	}
}
