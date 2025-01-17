/*
Copyright 2019 The Kubernetes Authors.

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
	"reflect"
	"sort"
	"testing"

	cadvisorapi "github.com/google/cadvisor/info/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	"k8s.io/klog/v2/ktesting"
	pkgfeatures "k8s.io/kubernetes/pkg/features"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpumanager/state"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpumanager/topology"
	"k8s.io/kubernetes/pkg/kubelet/cm/topologymanager"
	"k8s.io/kubernetes/pkg/kubelet/cm/topologymanager/bitmask"
	"k8s.io/utils/cpuset"
)

type testCase struct {
	name          string
	pod           v1.Pod
	container     v1.Container
	assignments   state.ContainerCPUAssignments
	defaultCPUSet cpuset.CPUSet
	expectedHints []topologymanager.TopologyHint
}

func returnMachineInfo() cadvisorapi.MachineInfo {
	return cadvisorapi.MachineInfo{
		NumCores: 12,
		Topology: []cadvisorapi.Node{
			{Id: 0,
				Cores: []cadvisorapi.Core{
					{SocketID: 0, Id: 0, Threads: []int{0, 6}, UncoreCaches: []cadvisorapi.Cache{{Id: 1}}},
					{SocketID: 0, Id: 1, Threads: []int{1, 7}, UncoreCaches: []cadvisorapi.Cache{{Id: 1}}},
					{SocketID: 0, Id: 2, Threads: []int{2, 8}, UncoreCaches: []cadvisorapi.Cache{{Id: 1}}},
				},
			},
			{Id: 1,
				Cores: []cadvisorapi.Core{
					{SocketID: 1, Id: 0, Threads: []int{3, 9}, UncoreCaches: []cadvisorapi.Cache{{Id: 1}}},
					{SocketID: 1, Id: 1, Threads: []int{4, 10}, UncoreCaches: []cadvisorapi.Cache{{Id: 1}}},
					{SocketID: 1, Id: 2, Threads: []int{5, 11}, UncoreCaches: []cadvisorapi.Cache{{Id: 1}}},
				},
			},
		},
	}
}

type containerOptions struct {
	request       string
	limit         string
	restartPolicy v1.ContainerRestartPolicy
}

func TestPodGuaranteedCPUs(t *testing.T) {
	options := [][]*containerOptions{
		{
			{request: "0", limit: "0"},
		},
		{
			{request: "2", limit: "2"},
		},
		{
			{request: "5", limit: "5"},
		},
		{
			{request: "2", limit: "2"},
			{request: "4", limit: "4"},
		},
	}
	// tc for not guaranteed Pod
	testPod1 := makeMultiContainerPodWithOptions(options[0], options[0])
	testPod2 := makeMultiContainerPodWithOptions(options[0], options[1])
	testPod3 := makeMultiContainerPodWithOptions(options[1], options[0])
	// tc for guaranteed Pod
	testPod4 := makeMultiContainerPodWithOptions(options[1], options[1])
	testPod5 := makeMultiContainerPodWithOptions(options[2], options[2])
	// tc for comparing init containers and user containers
	testPod6 := makeMultiContainerPodWithOptions(options[1], options[2])
	testPod7 := makeMultiContainerPodWithOptions(options[2], options[1])
	// tc for multi containers
	testPod8 := makeMultiContainerPodWithOptions(options[3], options[3])
	// tc for restartable init containers
	testPod9 := makeMultiContainerPodWithOptions([]*containerOptions{
		{request: "1", limit: "1", restartPolicy: v1.ContainerRestartPolicyAlways},
	}, []*containerOptions{
		{request: "1", limit: "1"},
	})
	testPod10 := makeMultiContainerPodWithOptions([]*containerOptions{
		{request: "5", limit: "5"},
		{request: "1", limit: "1", restartPolicy: v1.ContainerRestartPolicyAlways},
		{request: "2", limit: "2", restartPolicy: v1.ContainerRestartPolicyAlways},
		{request: "3", limit: "3", restartPolicy: v1.ContainerRestartPolicyAlways},
	}, []*containerOptions{
		{request: "1", limit: "1"},
	})
	testPod11 := makeMultiContainerPodWithOptions([]*containerOptions{
		{request: "5", limit: "5"},
		{request: "1", limit: "1", restartPolicy: v1.ContainerRestartPolicyAlways},
		{request: "2", limit: "2", restartPolicy: v1.ContainerRestartPolicyAlways},
		{request: "5", limit: "5"},
		{request: "3", limit: "3", restartPolicy: v1.ContainerRestartPolicyAlways},
	}, []*containerOptions{
		{request: "1", limit: "1"},
	})
	testPod12 := makeMultiContainerPodWithOptions([]*containerOptions{
		{request: "10", limit: "10", restartPolicy: v1.ContainerRestartPolicyAlways},
		{request: "200", limit: "200"},
	}, []*containerOptions{
		{request: "100", limit: "100"},
	})

	p := staticPolicy{}

	tcases := []struct {
		name        string
		pod         *v1.Pod
		expectedCPU int
	}{
		{
			name:        "TestCase01: if requestedCPU == 0, Pod is not Guaranteed Qos",
			pod:         testPod1,
			expectedCPU: 0,
		},
		{
			name:        "TestCase02: if requestedCPU == 0, Pod is not Guaranteed Qos",
			pod:         testPod2,
			expectedCPU: 0,
		},
		{
			name:        "TestCase03: if requestedCPU == 0, Pod is not Guaranteed Qos",
			pod:         testPod3,
			expectedCPU: 0,
		},
		{
			name:        "TestCase04: Guaranteed Pod requests 2 CPUs",
			pod:         testPod4,
			expectedCPU: 2,
		},
		{
			name:        "TestCase05: Guaranteed Pod requests 5 CPUs",
			pod:         testPod5,
			expectedCPU: 5,
		},
		{
			name:        "TestCase06: The number of CPUs requested By app is bigger than the number of CPUs requested by init",
			pod:         testPod6,
			expectedCPU: 5,
		},
		{
			name:        "TestCase07: The number of CPUs requested By init is bigger than the number of CPUs requested by app",
			pod:         testPod7,
			expectedCPU: 5,
		},
		{
			name:        "TestCase08: Sum of CPUs requested by multiple containers",
			pod:         testPod8,
			expectedCPU: 6,
		},
		{
			name:        "TestCase09: restartable init container + regular container",
			pod:         testPod9,
			expectedCPU: 2,
		},
		{
			name:        "TestCase09: multiple restartable init containers",
			pod:         testPod10,
			expectedCPU: 7,
		},
		{
			name:        "TestCase11: multiple restartable and regular init containers",
			pod:         testPod11,
			expectedCPU: 8,
		},
		{
			name:        "TestCase12: restartable init, regular init and regular container",
			pod:         testPod12,
			expectedCPU: 210,
		},
	}
	for _, tc := range tcases {
		t.Run(tc.name, func(t *testing.T) {
			requestedCPU := p.podGuaranteedCPUs(tc.pod)

			if requestedCPU != tc.expectedCPU {
				t.Errorf("Expected in result to be %v , got %v", tc.expectedCPU, requestedCPU)
			}
		})
	}
}

func TestGetTopologyHints(t *testing.T) {
	_, ctx := ktesting.NewTestContext(t)
	machineInfo := returnMachineInfo()
	tcases := returnTestCases()

	for _, tc := range tcases {
		topology, _ := topology.Discover(ctx, &machineInfo)

		var activePods []*v1.Pod
		for p := range tc.assignments {
			pod := v1.Pod{}
			pod.UID = types.UID(p)
			for c := range tc.assignments[p] {
				container := v1.Container{}
				container.Name = c
				pod.Spec.Containers = append(pod.Spec.Containers, container)
			}
			activePods = append(activePods, &pod)
		}

		m := manager{
			policy: &staticPolicy{
				topology: topology,
			},
			state: &mockState{
				assignments:   tc.assignments,
				defaultCPUSet: tc.defaultCPUSet,
			},
			topology:          topology,
			activePods:        func() []*v1.Pod { return activePods },
			podStatusProvider: mockPodStatusProvider{},
			sourcesReady:      &sourcesReadyStub{},
		}

		hints := m.GetTopologyHints(&tc.pod, &tc.container)[string(v1.ResourceCPU)]
		if len(tc.expectedHints) == 0 && len(hints) == 0 {
			continue
		}
		sort.SliceStable(hints, func(i, j int) bool {
			return hints[i].LessThan(hints[j])
		})
		sort.SliceStable(tc.expectedHints, func(i, j int) bool {
			return tc.expectedHints[i].LessThan(tc.expectedHints[j])
		})
		if !reflect.DeepEqual(tc.expectedHints, hints) {
			t.Errorf("Expected in result to be %v , got %v", tc.expectedHints, hints)
		}
	}
}

func TestGetPodTopologyHints(t *testing.T) {
	machineInfo := returnMachineInfo()
	_, ctx := ktesting.NewTestContext(t)

	for _, tc := range returnTestCases() {
		topology, _ := topology.Discover(ctx, &machineInfo)

		var activePods []*v1.Pod
		for p := range tc.assignments {
			pod := v1.Pod{}
			pod.UID = types.UID(p)
			for c := range tc.assignments[p] {
				container := v1.Container{}
				container.Name = c
				pod.Spec.Containers = append(pod.Spec.Containers, container)
			}
			activePods = append(activePods, &pod)
		}

		m := manager{
			policy: &staticPolicy{
				topology: topology,
			},
			state: &mockState{
				assignments:   tc.assignments,
				defaultCPUSet: tc.defaultCPUSet,
			},
			topology:          topology,
			activePods:        func() []*v1.Pod { return activePods },
			podStatusProvider: mockPodStatusProvider{},
			sourcesReady:      &sourcesReadyStub{},
		}

		podHints := m.GetPodTopologyHints(&tc.pod)[string(v1.ResourceCPU)]
		if len(tc.expectedHints) == 0 && len(podHints) == 0 {
			continue
		}
		sort.SliceStable(podHints, func(i, j int) bool {
			return podHints[i].LessThan(podHints[j])
		})
		sort.SliceStable(tc.expectedHints, func(i, j int) bool {
			return tc.expectedHints[i].LessThan(tc.expectedHints[j])
		})
		if !reflect.DeepEqual(tc.expectedHints, podHints) {
			t.Errorf("Expected in result to be %v , got %v", tc.expectedHints, podHints)
		}
	}
}

func TestGetPodTopologyHintsWithPolicyOptions(t *testing.T) {
	testPod1 := makePod("fakePod", "fakeContainer", "2", "2")
	testContainer1 := &testPod1.Spec.Containers[0]

	testPod2 := makePod("fakePod", "fakeContainer", "41", "41")
	testContainer2 := &testPod1.Spec.Containers[0]

	cpuSetAcrossSocket, _ := cpuset.Parse("0-28,40-57")

	m0001, _ := bitmask.NewBitMask(0)
	m0011, _ := bitmask.NewBitMask(0, 1)
	m0101, _ := bitmask.NewBitMask(0, 2)
	m1001, _ := bitmask.NewBitMask(0, 3)
	m0111, _ := bitmask.NewBitMask(0, 1, 2)
	m1011, _ := bitmask.NewBitMask(0, 1, 3)
	m1101, _ := bitmask.NewBitMask(0, 2, 3)
	m1111, _ := bitmask.NewBitMask(0, 1, 2, 3)

	testCases := []struct {
		description   string
		pod           v1.Pod
		container     v1.Container
		assignments   state.ContainerCPUAssignments
		defaultCPUSet cpuset.CPUSet
		policyOptions map[string]string
		topology      *topology.CPUTopology
		expectedHints []topologymanager.TopologyHint
	}{
		{
			// CPU available on numa node[0 ,1]. CPU on numa node 0 can satisfy request of 2 CPU's
			description:   "AlignBySocket:false, Preferred hints does not contains socket aligned hints",
			pod:           *testPod1,
			container:     *testContainer1,
			defaultCPUSet: cpuset.New(2, 3, 11),
			topology:      topoDualSocketMultiNumaPerSocketHT,
			policyOptions: map[string]string{AlignBySocketOption: "false"},
			expectedHints: []topologymanager.TopologyHint{
				{
					NUMANodeAffinity: m0001,
					Preferred:        true,
				},
				{
					NUMANodeAffinity: m0011,
					Preferred:        false,
				},
				{
					NUMANodeAffinity: m0101,
					Preferred:        false,
				},
				{
					NUMANodeAffinity: m1001,
					Preferred:        false,
				},
				{
					NUMANodeAffinity: m0111,
					Preferred:        false,
				},
				{
					NUMANodeAffinity: m1011,
					Preferred:        false,
				},
				{
					NUMANodeAffinity: m1101,
					Preferred:        false,
				},
				{
					NUMANodeAffinity: m1111,
					Preferred:        false,
				},
			},
		},
		{
			// CPU available on numa node[0 ,1]. CPU on numa node 0 can satisfy request of 2 CPU's
			description:   "AlignBySocket:true Preferred hints contains socket aligned hints",
			pod:           *testPod1,
			container:     *testContainer1,
			defaultCPUSet: cpuset.New(2, 3, 11),
			topology:      topoDualSocketMultiNumaPerSocketHT,
			policyOptions: map[string]string{AlignBySocketOption: "true"},
			expectedHints: []topologymanager.TopologyHint{
				{
					NUMANodeAffinity: m0001,
					Preferred:        true,
				},
				{
					NUMANodeAffinity: m0011,
					Preferred:        true,
				},
				{
					NUMANodeAffinity: m0101,
					Preferred:        false,
				},
				{
					NUMANodeAffinity: m1001,
					Preferred:        false,
				},
				{
					NUMANodeAffinity: m0111,
					Preferred:        false,
				},
				{
					NUMANodeAffinity: m1011,
					Preferred:        false,
				},
				{
					NUMANodeAffinity: m1101,
					Preferred:        false,
				},
				{
					NUMANodeAffinity: m1111,
					Preferred:        false,
				},
			},
		},
		{
			// CPU available on numa node[0 ,1]. CPU on numa nodes across sockets can satisfy request of 2 CPU's
			description:   "AlignBySocket:true Preferred hints are spread across socket since 2 sockets are required",
			pod:           *testPod2,
			container:     *testContainer2,
			defaultCPUSet: cpuSetAcrossSocket,
			topology:      topoDualSocketMultiNumaPerSocketHT,
			policyOptions: map[string]string{AlignBySocketOption: "true"},
			expectedHints: []topologymanager.TopologyHint{
				{
					NUMANodeAffinity: m0111,
					Preferred:        true,
				},
				{
					NUMANodeAffinity: m1111,
					Preferred:        true,
				},
			},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.description, func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, pkgfeatures.CPUManagerPolicyAlphaOptions, true)

			var activePods []*v1.Pod
			for p := range testCase.assignments {
				pod := v1.Pod{}
				pod.UID = types.UID(p)
				for c := range testCase.assignments[p] {
					container := v1.Container{}
					container.Name = c
					pod.Spec.Containers = append(pod.Spec.Containers, container)
				}
				activePods = append(activePods, &pod)
			}
			policyOpt, _ := NewStaticPolicyOptions(testCase.policyOptions)
			m := manager{
				policy: &staticPolicy{
					topology: testCase.topology,
					options:  policyOpt,
				},
				state: &mockState{
					assignments:   testCase.assignments,
					defaultCPUSet: testCase.defaultCPUSet,
				},
				topology:          testCase.topology,
				activePods:        func() []*v1.Pod { return activePods },
				podStatusProvider: mockPodStatusProvider{},
				sourcesReady:      &sourcesReadyStub{},
			}

			podHints := m.GetPodTopologyHints(&testCase.pod)[string(v1.ResourceCPU)]
			sort.SliceStable(podHints, func(i, j int) bool {
				return podHints[i].LessThan(podHints[j])
			})
			sort.SliceStable(testCase.expectedHints, func(i, j int) bool {
				return testCase.expectedHints[i].LessThan(testCase.expectedHints[j])
			})
			if !reflect.DeepEqual(testCase.expectedHints, podHints) {
				t.Errorf("Expected in result to be %v , got %v", testCase.expectedHints, podHints)
			}
		})
	}
}

func returnTestCases() []testCase {
	testPod1 := makePod("fakePod", "fakeContainer", "2", "2")
	testContainer1 := &testPod1.Spec.Containers[0]
	testPod2 := makePod("fakePod", "fakeContainer", "5", "5")
	testContainer2 := &testPod2.Spec.Containers[0]
	testPod3 := makePod("fakePod", "fakeContainer", "7", "7")
	testContainer3 := &testPod3.Spec.Containers[0]
	testPod4 := makePod("fakePod", "fakeContainer", "11", "11")
	testContainer4 := &testPod4.Spec.Containers[0]

	firstSocketMask, _ := bitmask.NewBitMask(0)
	secondSocketMask, _ := bitmask.NewBitMask(1)
	crossSocketMask, _ := bitmask.NewBitMask(0, 1)

	return []testCase{
		{
			name:          "Request 2 CPUs, 4 available on NUMA 0, 6 available on NUMA 1",
			pod:           *testPod1,
			container:     *testContainer1,
			defaultCPUSet: cpuset.New(2, 3, 4, 5, 6, 7, 8, 9, 10, 11),
			expectedHints: []topologymanager.TopologyHint{
				{
					NUMANodeAffinity: firstSocketMask,
					Preferred:        true,
				},
				{
					NUMANodeAffinity: secondSocketMask,
					Preferred:        true,
				},
				{
					NUMANodeAffinity: crossSocketMask,
					Preferred:        false,
				},
			},
		},
		{
			name:          "Request 5 CPUs, 4 available on NUMA 0, 6 available on NUMA 1",
			pod:           *testPod2,
			container:     *testContainer2,
			defaultCPUSet: cpuset.New(2, 3, 4, 5, 6, 7, 8, 9, 10, 11),
			expectedHints: []topologymanager.TopologyHint{
				{
					NUMANodeAffinity: secondSocketMask,
					Preferred:        true,
				},
				{
					NUMANodeAffinity: crossSocketMask,
					Preferred:        false,
				},
			},
		},
		{
			name:          "Request 7 CPUs, 4 available on NUMA 0, 6 available on NUMA 1",
			pod:           *testPod3,
			container:     *testContainer3,
			defaultCPUSet: cpuset.New(2, 3, 4, 5, 6, 7, 8, 9, 10, 11),
			expectedHints: []topologymanager.TopologyHint{
				{
					NUMANodeAffinity: crossSocketMask,
					Preferred:        true,
				},
			},
		},
		{
			name:          "Request 11 CPUs, 4 available on NUMA 0, 6 available on NUMA 1",
			pod:           *testPod4,
			container:     *testContainer4,
			defaultCPUSet: cpuset.New(2, 3, 4, 5, 6, 7, 8, 9, 10, 11),
			expectedHints: nil,
		},
		{
			name:          "Request 2 CPUs, 1 available on NUMA 0, 1 available on NUMA 1",
			pod:           *testPod1,
			container:     *testContainer1,
			defaultCPUSet: cpuset.New(0, 3),
			expectedHints: []topologymanager.TopologyHint{
				{
					NUMANodeAffinity: crossSocketMask,
					Preferred:        false,
				},
			},
		},
		{
			name:          "Request more CPUs than available",
			pod:           *testPod2,
			container:     *testContainer2,
			defaultCPUSet: cpuset.New(0, 1, 2, 3),
			expectedHints: nil,
		},
		{
			name:      "Regenerate Single-Node NUMA Hints if already allocated 1/2",
			pod:       *testPod1,
			container: *testContainer1,
			assignments: state.ContainerCPUAssignments{
				string(testPod1.UID): map[string]cpuset.CPUSet{
					testContainer1.Name: cpuset.New(0, 6),
				},
			},
			defaultCPUSet: cpuset.New(),
			expectedHints: []topologymanager.TopologyHint{
				{
					NUMANodeAffinity: firstSocketMask,
					Preferred:        true,
				},
				{
					NUMANodeAffinity: crossSocketMask,
					Preferred:        false,
				},
			},
		},
		{
			name:      "Regenerate Single-Node NUMA Hints if already allocated 1/2",
			pod:       *testPod1,
			container: *testContainer1,
			assignments: state.ContainerCPUAssignments{
				string(testPod1.UID): map[string]cpuset.CPUSet{
					testContainer1.Name: cpuset.New(3, 9),
				},
			},
			defaultCPUSet: cpuset.New(),
			expectedHints: []topologymanager.TopologyHint{
				{
					NUMANodeAffinity: secondSocketMask,
					Preferred:        true,
				},
				{
					NUMANodeAffinity: crossSocketMask,
					Preferred:        false,
				},
			},
		},
		{
			name:      "Regenerate Cross-NUMA Hints if already allocated",
			pod:       *testPod4,
			container: *testContainer4,
			assignments: state.ContainerCPUAssignments{
				string(testPod4.UID): map[string]cpuset.CPUSet{
					testContainer4.Name: cpuset.New(0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10),
				},
			},
			defaultCPUSet: cpuset.New(),
			expectedHints: []topologymanager.TopologyHint{
				{
					NUMANodeAffinity: crossSocketMask,
					Preferred:        true,
				},
			},
		},
		{
			name:      "Requested less than already allocated",
			pod:       *testPod1,
			container: *testContainer1,
			assignments: state.ContainerCPUAssignments{
				string(testPod1.UID): map[string]cpuset.CPUSet{
					testContainer1.Name: cpuset.New(0, 6, 3, 9),
				},
			},
			defaultCPUSet: cpuset.New(),
			expectedHints: []topologymanager.TopologyHint{},
		},
		{
			name:      "Requested more than already allocated",
			pod:       *testPod4,
			container: *testContainer4,
			assignments: state.ContainerCPUAssignments{
				string(testPod4.UID): map[string]cpuset.CPUSet{
					testContainer4.Name: cpuset.New(0, 6, 3, 9),
				},
			},
			defaultCPUSet: cpuset.New(),
			expectedHints: []topologymanager.TopologyHint{},
		},
	}
}
