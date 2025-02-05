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
	"os"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	utilfeature "k8s.io/apiserver/pkg/util/feature"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	"k8s.io/kubernetes/pkg/features"

	cadvisorapi "github.com/google/cadvisor/info/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"

	"k8s.io/kubernetes/pkg/kubelet/cm/containermap"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpumanager/state"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpumanager/topology"
	"k8s.io/kubernetes/pkg/kubelet/cm/topologymanager"
	"k8s.io/utils/cpuset"
)

type mockState struct {
	assignments   state.ContainerCPUAssignments
	defaultCPUSet cpuset.CPUSet
}

func (s *mockState) GetCPUSet(podUID string, containerName string) (cpuset.CPUSet, bool) {
	res, ok := s.assignments[podUID][containerName]
	return res.Clone(), ok
}

func (s *mockState) GetDefaultCPUSet() cpuset.CPUSet {
	return s.defaultCPUSet.Clone()
}

func (s *mockState) GetCPUSetOrDefault(podUID string, containerName string) cpuset.CPUSet {
	if res, ok := s.GetCPUSet(podUID, containerName); ok {
		return res
	}
	return s.GetDefaultCPUSet()
}

func (s *mockState) SetCPUSet(podUID string, containerName string, cset cpuset.CPUSet) {
	if _, exists := s.assignments[podUID]; !exists {
		s.assignments[podUID] = make(map[string]cpuset.CPUSet)
	}
	s.assignments[podUID][containerName] = cset
}

func (s *mockState) SetDefaultCPUSet(cset cpuset.CPUSet) {
	s.defaultCPUSet = cset
}

func (s *mockState) Delete(podUID string, containerName string) {
	delete(s.assignments[podUID], containerName)
	if len(s.assignments[podUID]) == 0 {
		delete(s.assignments, podUID)
	}
}

func (s *mockState) ClearState() {
	s.defaultCPUSet = cpuset.CPUSet{}
	s.assignments = make(state.ContainerCPUAssignments)
}

func (s *mockState) SetCPUAssignments(a state.ContainerCPUAssignments) {
	s.assignments = a.Clone()
}

func (s *mockState) GetCPUAssignments() state.ContainerCPUAssignments {
	return s.assignments.Clone()
}

type mockPolicy struct {
	err error
}

func (p *mockPolicy) Name() string {
	return "mock"
}

func (p *mockPolicy) Start(s state.State) error {
	return p.err
}

func (p *mockPolicy) Allocate(s state.State, pod *v1.Pod, container *v1.Container) error {
	return p.err
}

func (p *mockPolicy) RemoveContainer(s state.State, podUID string, containerName string) error {
	return p.err
}

func (p *mockPolicy) GetTopologyHints(s state.State, pod *v1.Pod, container *v1.Container) map[string][]topologymanager.TopologyHint {
	return nil
}

func (p *mockPolicy) GetPodTopologyHints(s state.State, pod *v1.Pod) map[string][]topologymanager.TopologyHint {
	return nil
}

func (p *mockPolicy) GetAllocatableCPUs(m state.State) cpuset.CPUSet {
	return cpuset.New()
}

type mockRuntimeService struct {
	err error
}

func (rt mockRuntimeService) UpdateContainerResources(_ context.Context, id string, resources *runtimeapi.ContainerResources) error {
	return rt.err
}

type mockPodStatusProvider struct {
	podStatus v1.PodStatus
	found     bool
}

func (psp mockPodStatusProvider) GetPodStatus(uid types.UID) (v1.PodStatus, bool) {
	return psp.podStatus, psp.found
}

func makePod(podUID, containerName, cpuRequest, cpuLimit string) *v1.Pod {
	pod := &v1.Pod{
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{
							v1.ResourceName(v1.ResourceCPU):    resource.MustParse(cpuRequest),
							v1.ResourceName(v1.ResourceMemory): resource.MustParse("1G"),
						},
						Limits: v1.ResourceList{
							v1.ResourceName(v1.ResourceCPU):    resource.MustParse(cpuLimit),
							v1.ResourceName(v1.ResourceMemory): resource.MustParse("1G"),
						},
					},
				},
			},
		},
	}

	pod.UID = types.UID(podUID)
	pod.Name = podUID
	pod.Spec.Containers[0].Name = containerName

	return pod
}

func makeMultiContainerPod(initCPUs, appCPUs []struct{ request, limit string }) *v1.Pod {
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pod",
			UID:  "podUID",
		},
		Spec: v1.PodSpec{
			InitContainers: []v1.Container{},
			Containers:     []v1.Container{},
		},
	}

	for i, cpu := range initCPUs {
		pod.Spec.InitContainers = append(pod.Spec.InitContainers, v1.Container{
			Name: "initContainer-" + strconv.Itoa(i),
			Resources: v1.ResourceRequirements{
				Requests: v1.ResourceList{
					v1.ResourceName(v1.ResourceCPU):    resource.MustParse(cpu.request),
					v1.ResourceName(v1.ResourceMemory): resource.MustParse("1G"),
				},
				Limits: v1.ResourceList{
					v1.ResourceName(v1.ResourceCPU):    resource.MustParse(cpu.limit),
					v1.ResourceName(v1.ResourceMemory): resource.MustParse("1G"),
				},
			},
		})
	}

	for i, cpu := range appCPUs {
		pod.Spec.Containers = append(pod.Spec.Containers, v1.Container{
			Name: "appContainer-" + strconv.Itoa(i),
			Resources: v1.ResourceRequirements{
				Requests: v1.ResourceList{
					v1.ResourceName(v1.ResourceCPU):    resource.MustParse(cpu.request),
					v1.ResourceName(v1.ResourceMemory): resource.MustParse("1G"),
				},
				Limits: v1.ResourceList{
					v1.ResourceName(v1.ResourceCPU):    resource.MustParse(cpu.limit),
					v1.ResourceName(v1.ResourceMemory): resource.MustParse("1G"),
				},
			},
		})
	}

	return pod
}

func makeMultiContainerPodWithOptions(initCPUs, appCPUs []*containerOptions) *v1.Pod {
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pod",
			UID:  "podUID",
		},
		Spec: v1.PodSpec{
			InitContainers: []v1.Container{},
			Containers:     []v1.Container{},
		},
	}

	for i, cpu := range initCPUs {
		pod.Spec.InitContainers = append(pod.Spec.InitContainers, v1.Container{
			Name: "initContainer-" + strconv.Itoa(i),
			Resources: v1.ResourceRequirements{
				Requests: v1.ResourceList{
					v1.ResourceName(v1.ResourceCPU):    resource.MustParse(cpu.request),
					v1.ResourceName(v1.ResourceMemory): resource.MustParse("1G"),
				},
				Limits: v1.ResourceList{
					v1.ResourceName(v1.ResourceCPU):    resource.MustParse(cpu.limit),
					v1.ResourceName(v1.ResourceMemory): resource.MustParse("1G"),
				},
			},
			RestartPolicy: &cpu.restartPolicy,
		})
	}

	for i, cpu := range appCPUs {
		pod.Spec.Containers = append(pod.Spec.Containers, v1.Container{
			Name: "appContainer-" + strconv.Itoa(i),
			Resources: v1.ResourceRequirements{
				Requests: v1.ResourceList{
					v1.ResourceName(v1.ResourceCPU):    resource.MustParse(cpu.request),
					v1.ResourceName(v1.ResourceMemory): resource.MustParse("1G"),
				},
				Limits: v1.ResourceList{
					v1.ResourceName(v1.ResourceCPU):    resource.MustParse(cpu.limit),
					v1.ResourceName(v1.ResourceMemory): resource.MustParse("1G"),
				},
			},
		})
	}

	return pod
}

func TestCPUManagerAdd(t *testing.T) {
	if runtime.GOOS == "windows" {
		featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.WindowsCPUAndMemoryAffinity, true)
	}

	testPolicy, _ := NewStaticPolicy(
		&topology.CPUTopology{
			NumCPUs:    4,
			NumSockets: 1,
			NumCores:   4,
			CPUDetails: map[int]topology.CPUInfo{
				0: {CoreID: 0, SocketID: 0},
				1: {CoreID: 1, SocketID: 0},
				2: {CoreID: 2, SocketID: 0},
				3: {CoreID: 3, SocketID: 0},
			},
		},
		0,
		cpuset.New(),
		topologymanager.NewFakeManager(),
		nil)
	testCases := []struct {
		description        string
		updateErr          error
		policy             Policy
		expCPUSet          cpuset.CPUSet
		expAllocateErr     error
		expAddContainerErr error
	}{
		{
			description:        "cpu manager add - no error",
			updateErr:          nil,
			policy:             testPolicy,
			expCPUSet:          cpuset.New(3, 4),
			expAllocateErr:     nil,
			expAddContainerErr: nil,
		},
		{
			description: "cpu manager add - policy add container error",
			updateErr:   nil,
			policy: &mockPolicy{
				err: fmt.Errorf("fake reg error"),
			},
			expCPUSet:          cpuset.New(1, 2, 3, 4),
			expAllocateErr:     fmt.Errorf("fake reg error"),
			expAddContainerErr: nil,
		},
	}

	for _, testCase := range testCases {
		mgr := &manager{
			policy: testCase.policy,
			state: &mockState{
				assignments:   state.ContainerCPUAssignments{},
				defaultCPUSet: cpuset.New(1, 2, 3, 4),
			},
			lastUpdateState: state.NewMemoryState(),
			containerRuntime: mockRuntimeService{
				err: testCase.updateErr,
			},
			containerMap:      containermap.NewContainerMap(),
			podStatusProvider: mockPodStatusProvider{},
			sourcesReady:      &sourcesReadyStub{},
		}

		pod := makePod("fakePod", "fakeContainer", "2", "2")
		container := &pod.Spec.Containers[0]
		mgr.activePods = func() []*v1.Pod { return []*v1.Pod{pod} }

		err := mgr.Allocate(pod, container)
		if !reflect.DeepEqual(err, testCase.expAllocateErr) {
			t.Errorf("CPU Manager Allocate() error (%v). expected error: %v but got: %v",
				testCase.description, testCase.expAllocateErr, err)
		}

		mgr.AddContainer(pod, container, "fakeID")
		_, _, err = mgr.containerMap.GetContainerRef("fakeID")
		if !reflect.DeepEqual(err, testCase.expAddContainerErr) {
			t.Errorf("CPU Manager AddContainer() error (%v). expected error: %v but got: %v",
				testCase.description, testCase.expAddContainerErr, err)
		}
		if !testCase.expCPUSet.Equals(mgr.state.GetDefaultCPUSet()) {
			t.Errorf("CPU Manager AddContainer() error (%v). expected cpuset: %v but got: %v",
				testCase.description, testCase.expCPUSet, mgr.state.GetDefaultCPUSet())
		}
	}
}

func TestCPUManagerAddWithInitContainers(t *testing.T) {
	if runtime.GOOS == "windows" {
		featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.WindowsCPUAndMemoryAffinity, true)
	}

	testCases := []struct {
		description      string
		topo             *topology.CPUTopology
		numReservedCPUs  int
		initContainerIDs []string
		containerIDs     []string
		stAssignments    state.ContainerCPUAssignments
		stDefaultCPUSet  cpuset.CPUSet
		pod              *v1.Pod
		expInitCSets     []cpuset.CPUSet
		expCSets         []cpuset.CPUSet
	}{
		{
			description:      "No Guaranteed Init CPUs",
			topo:             topoSingleSocketHT,
			numReservedCPUs:  0,
			stAssignments:    state.ContainerCPUAssignments{},
			stDefaultCPUSet:  cpuset.New(0, 1, 2, 3, 4, 5, 6, 7),
			initContainerIDs: []string{"initFakeID"},
			containerIDs:     []string{"appFakeID"},
			pod: makeMultiContainerPod(
				[]struct{ request, limit string }{{"100m", "100m"}},
				[]struct{ request, limit string }{{"4000m", "4000m"}}),
			expInitCSets: []cpuset.CPUSet{
				cpuset.New()},
			expCSets: []cpuset.CPUSet{
				cpuset.New(0, 4, 1, 5)},
		},
		{
			description:      "Equal Number of Guaranteed CPUs",
			topo:             topoSingleSocketHT,
			numReservedCPUs:  0,
			stAssignments:    state.ContainerCPUAssignments{},
			stDefaultCPUSet:  cpuset.New(0, 1, 2, 3, 4, 5, 6, 7),
			initContainerIDs: []string{"initFakeID"},
			containerIDs:     []string{"appFakeID"},
			pod: makeMultiContainerPod(
				[]struct{ request, limit string }{{"4000m", "4000m"}},
				[]struct{ request, limit string }{{"4000m", "4000m"}}),
			expInitCSets: []cpuset.CPUSet{
				cpuset.New(0, 4, 1, 5)},
			expCSets: []cpuset.CPUSet{
				cpuset.New(0, 4, 1, 5)},
		},
		{
			description:      "More Init Container Guaranteed CPUs",
			topo:             topoSingleSocketHT,
			numReservedCPUs:  0,
			stAssignments:    state.ContainerCPUAssignments{},
			stDefaultCPUSet:  cpuset.New(0, 1, 2, 3, 4, 5, 6, 7),
			initContainerIDs: []string{"initFakeID"},
			containerIDs:     []string{"appFakeID"},
			pod: makeMultiContainerPod(
				[]struct{ request, limit string }{{"6000m", "6000m"}},
				[]struct{ request, limit string }{{"4000m", "4000m"}}),
			expInitCSets: []cpuset.CPUSet{
				cpuset.New(0, 4, 1, 5, 2, 6)},
			expCSets: []cpuset.CPUSet{
				cpuset.New(0, 4, 1, 5)},
		},
		{
			description:      "Less Init Container Guaranteed CPUs",
			topo:             topoSingleSocketHT,
			numReservedCPUs:  0,
			stAssignments:    state.ContainerCPUAssignments{},
			stDefaultCPUSet:  cpuset.New(0, 1, 2, 3, 4, 5, 6, 7),
			initContainerIDs: []string{"initFakeID"},
			containerIDs:     []string{"appFakeID"},
			pod: makeMultiContainerPod(
				[]struct{ request, limit string }{{"2000m", "2000m"}},
				[]struct{ request, limit string }{{"4000m", "4000m"}}),
			expInitCSets: []cpuset.CPUSet{
				cpuset.New(0, 4)},
			expCSets: []cpuset.CPUSet{
				cpuset.New(0, 4, 1, 5)},
		},
		{
			description:      "Multi Init Container Equal CPUs",
			topo:             topoSingleSocketHT,
			numReservedCPUs:  0,
			stAssignments:    state.ContainerCPUAssignments{},
			stDefaultCPUSet:  cpuset.New(0, 1, 2, 3, 4, 5, 6, 7),
			initContainerIDs: []string{"initFakeID-1", "initFakeID-2"},
			containerIDs:     []string{"appFakeID"},
			pod: makeMultiContainerPod(
				[]struct{ request, limit string }{
					{"2000m", "2000m"},
					{"2000m", "2000m"}},
				[]struct{ request, limit string }{
					{"2000m", "2000m"}}),
			expInitCSets: []cpuset.CPUSet{
				cpuset.New(0, 4),
				cpuset.New(0, 4)},
			expCSets: []cpuset.CPUSet{
				cpuset.New(0, 4)},
		},
		{
			description:      "Multi Init Container Less CPUs",
			topo:             topoSingleSocketHT,
			numReservedCPUs:  0,
			stAssignments:    state.ContainerCPUAssignments{},
			stDefaultCPUSet:  cpuset.New(0, 1, 2, 3, 4, 5, 6, 7),
			initContainerIDs: []string{"initFakeID-1", "initFakeID-2"},
			containerIDs:     []string{"appFakeID"},
			pod: makeMultiContainerPod(
				[]struct{ request, limit string }{
					{"4000m", "4000m"},
					{"4000m", "4000m"}},
				[]struct{ request, limit string }{
					{"2000m", "2000m"}}),
			expInitCSets: []cpuset.CPUSet{
				cpuset.New(0, 4, 1, 5),
				cpuset.New(0, 4, 1, 5)},
			expCSets: []cpuset.CPUSet{
				cpuset.New(0, 4)},
		},
		{
			description:      "Multi Init Container More CPUs",
			topo:             topoSingleSocketHT,
			numReservedCPUs:  0,
			stAssignments:    state.ContainerCPUAssignments{},
			stDefaultCPUSet:  cpuset.New(0, 1, 2, 3, 4, 5, 6, 7),
			initContainerIDs: []string{"initFakeID-1", "initFakeID-2"},
			containerIDs:     []string{"appFakeID"},
			pod: makeMultiContainerPod(
				[]struct{ request, limit string }{
					{"2000m", "2000m"},
					{"2000m", "2000m"}},
				[]struct{ request, limit string }{
					{"4000m", "4000m"}}),
			expInitCSets: []cpuset.CPUSet{
				cpuset.New(0, 4),
				cpuset.New(0, 4)},
			expCSets: []cpuset.CPUSet{
				cpuset.New(0, 4, 1, 5)},
		},
		{
			description:      "Multi Init Container Increasing CPUs",
			topo:             topoSingleSocketHT,
			numReservedCPUs:  0,
			stAssignments:    state.ContainerCPUAssignments{},
			stDefaultCPUSet:  cpuset.New(0, 1, 2, 3, 4, 5, 6, 7),
			initContainerIDs: []string{"initFakeID-1", "initFakeID-2"},
			containerIDs:     []string{"appFakeID"},
			pod: makeMultiContainerPod(
				[]struct{ request, limit string }{
					{"2000m", "2000m"},
					{"4000m", "4000m"}},
				[]struct{ request, limit string }{
					{"6000m", "6000m"}}),
			expInitCSets: []cpuset.CPUSet{
				cpuset.New(0, 4),
				cpuset.New(0, 4, 1, 5)},
			expCSets: []cpuset.CPUSet{
				cpuset.New(0, 4, 1, 5, 2, 6)},
		},
		{
			description:      "Multi Init, Multi App Container Split CPUs",
			topo:             topoSingleSocketHT,
			numReservedCPUs:  0,
			stAssignments:    state.ContainerCPUAssignments{},
			stDefaultCPUSet:  cpuset.New(0, 1, 2, 3, 4, 5, 6, 7),
			initContainerIDs: []string{"initFakeID-1", "initFakeID-2"},
			containerIDs:     []string{"appFakeID-1", "appFakeID-2"},
			pod: makeMultiContainerPod(
				[]struct{ request, limit string }{
					{"2000m", "2000m"},
					{"4000m", "4000m"}},
				[]struct{ request, limit string }{
					{"2000m", "2000m"},
					{"2000m", "2000m"}}),
			expInitCSets: []cpuset.CPUSet{
				cpuset.New(0, 4),
				cpuset.New(0, 4, 1, 5)},
			expCSets: []cpuset.CPUSet{
				cpuset.New(0, 4),
				cpuset.New(1, 5)},
		},
	}

	for _, testCase := range testCases {
		policy, _ := NewStaticPolicy(testCase.topo, testCase.numReservedCPUs, cpuset.New(), topologymanager.NewFakeManager(), nil)

		mockState := &mockState{
			assignments:   testCase.stAssignments,
			defaultCPUSet: testCase.stDefaultCPUSet,
		}

		mgr := &manager{
			policy:            policy,
			state:             mockState,
			lastUpdateState:   state.NewMemoryState(),
			containerRuntime:  mockRuntimeService{},
			containerMap:      containermap.NewContainerMap(),
			podStatusProvider: mockPodStatusProvider{},
			sourcesReady:      &sourcesReadyStub{},
			activePods: func() []*v1.Pod {
				return []*v1.Pod{testCase.pod}
			},
		}

		containers := append(
			testCase.pod.Spec.InitContainers,
			testCase.pod.Spec.Containers...)

		containerIDs := append(
			testCase.initContainerIDs,
			testCase.containerIDs...)

		expCSets := append(
			testCase.expInitCSets,
			testCase.expCSets...)

		cumCSet := cpuset.New()

		for i := range containers {
			err := mgr.Allocate(testCase.pod, &containers[i])
			if err != nil {
				t.Errorf("StaticPolicy Allocate() error (%v). unexpected error for container id: %v: %v",
					testCase.description, containerIDs[i], err)
			}

			mgr.AddContainer(testCase.pod, &containers[i], containerIDs[i])
			_, _, err = mgr.containerMap.GetContainerRef(containerIDs[i])
			if err != nil {
				t.Errorf("StaticPolicy AddContainer() error (%v). unexpected error for container id: %v: %v",
					testCase.description, containerIDs[i], err)
			}

			cset, found := mockState.assignments[string(testCase.pod.UID)][containers[i].Name]
			if !expCSets[i].IsEmpty() && !found {
				t.Errorf("StaticPolicy AddContainer() error (%v). expected container %v to be present in assignments %v",
					testCase.description, containers[i].Name, mockState.assignments)
			}

			if found && !cset.Equals(expCSets[i]) {
				t.Errorf("StaticPolicy AddContainer() error (%v). expected cpuset %v for container %v but got %v",
					testCase.description, expCSets[i], containers[i].Name, cset)
			}

			cumCSet = cumCSet.Union(cset)
		}

		if !testCase.stDefaultCPUSet.Difference(cumCSet).Equals(mockState.defaultCPUSet) {
			t.Errorf("StaticPolicy error (%v). expected final state for defaultCPUSet %v but got %v",
				testCase.description, testCase.stDefaultCPUSet.Difference(cumCSet), mockState.defaultCPUSet)
		}
	}
}

func TestCPUManagerGenerate(t *testing.T) {
	if runtime.GOOS == "windows" {
		featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.WindowsCPUAndMemoryAffinity, true)
	}

	testCases := []struct {
		description                string
		cpuPolicyName              string
		nodeAllocatableReservation v1.ResourceList
		isTopologyBroken           bool
		expectedPolicy             string
		expectedError              error
	}{
		{
			description:                "set none policy",
			cpuPolicyName:              "none",
			nodeAllocatableReservation: nil,
			expectedPolicy:             "none",
		},
		{
			description:                "invalid policy name",
			cpuPolicyName:              "invalid",
			nodeAllocatableReservation: nil,
			expectedError:              fmt.Errorf("unknown policy: \"invalid\""),
		},
		{
			description:                "static policy",
			cpuPolicyName:              "static",
			nodeAllocatableReservation: v1.ResourceList{v1.ResourceCPU: *resource.NewQuantity(3, resource.DecimalSI)},
			expectedPolicy:             "static",
		},
		{
			description:                "static policy - broken topology",
			cpuPolicyName:              "static",
			nodeAllocatableReservation: v1.ResourceList{},
			isTopologyBroken:           true,
			expectedError:              fmt.Errorf("could not detect number of cpus"),
		},
		{
			description:                "static policy - broken reservation",
			cpuPolicyName:              "static",
			nodeAllocatableReservation: v1.ResourceList{},
			expectedError:              fmt.Errorf("unable to determine reserved CPU resources for static policy"),
		},
		{
			description:                "static policy - no CPU resources",
			cpuPolicyName:              "static",
			nodeAllocatableReservation: v1.ResourceList{v1.ResourceCPU: *resource.NewQuantity(0, resource.DecimalSI)},
			expectedError:              fmt.Errorf("the static policy requires systemreserved.cpu + kubereserved.cpu to be greater than zero"),
		},
	}

	mockedMachineInfo := cadvisorapi.MachineInfo{
		NumCores: 4,
		Topology: []cadvisorapi.Node{
			{
				Cores: []cadvisorapi.Core{
					{
						Id:           0,
						Threads:      []int{0},
						UncoreCaches: []cadvisorapi.Cache{{Id: 1}},
					},
					{
						Id:           1,
						Threads:      []int{1},
						UncoreCaches: []cadvisorapi.Cache{{Id: 1}},
					},
					{
						Id:           2,
						Threads:      []int{2},
						UncoreCaches: []cadvisorapi.Cache{{Id: 1}},
					},
					{
						Id:           3,
						Threads:      []int{3},
						UncoreCaches: []cadvisorapi.Cache{{Id: 1}},
					},
				},
			},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.description, func(t *testing.T) {
			machineInfo := &mockedMachineInfo
			if testCase.isTopologyBroken {
				machineInfo = &cadvisorapi.MachineInfo{}
			}
			sDir, err := os.MkdirTemp("", "cpu_manager_test")
			if err != nil {
				t.Errorf("cannot create state file: %s", err.Error())
			}
			defer os.RemoveAll(sDir)

			mgr, err := NewManager(testCase.cpuPolicyName, nil, 5*time.Second, machineInfo, cpuset.New(), testCase.nodeAllocatableReservation, sDir, topologymanager.NewFakeManager())
			if testCase.expectedError != nil {
				if !strings.Contains(err.Error(), testCase.expectedError.Error()) {
					t.Errorf("Unexpected error message. Have: %s wants %s", err.Error(), testCase.expectedError.Error())
				}
			} else {
				rawMgr := mgr.(*manager)
				if rawMgr.policy.Name() != testCase.expectedPolicy {
					t.Errorf("Unexpected policy name. Have: %q wants %q", rawMgr.policy.Name(), testCase.expectedPolicy)
				}
				if rawMgr.topology == nil {
					t.Errorf("Expected topology to be non-nil for policy '%v'. Have: %q", rawMgr.policy.Name(), rawMgr.topology)
				}
			}
		})

	}
}

func TestCPUManagerRemove(t *testing.T) {
	if runtime.GOOS == "windows" {
		featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.WindowsCPUAndMemoryAffinity, true)
	}

	containerID := "fakeID"
	containerMap := containermap.NewContainerMap()

	mgr := &manager{
		policy: &mockPolicy{
			err: nil,
		},
		state: &mockState{
			assignments:   state.ContainerCPUAssignments{},
			defaultCPUSet: cpuset.New(),
		},
		lastUpdateState:   state.NewMemoryState(),
		containerRuntime:  mockRuntimeService{},
		containerMap:      containerMap,
		activePods:        func() []*v1.Pod { return nil },
		podStatusProvider: mockPodStatusProvider{},
	}

	containerMap.Add("", "", containerID)
	err := mgr.RemoveContainer(containerID)
	if err != nil {
		t.Errorf("CPU Manager RemoveContainer() error. expected error to be nil but got: %v", err)
	}

	mgr = &manager{
		policy: &mockPolicy{
			err: fmt.Errorf("fake error"),
		},
		state:             state.NewMemoryState(),
		containerRuntime:  mockRuntimeService{},
		containerMap:      containerMap,
		activePods:        func() []*v1.Pod { return nil },
		podStatusProvider: mockPodStatusProvider{},
	}

	containerMap.Add("", "", containerID)
	err = mgr.RemoveContainer(containerID)
	if !reflect.DeepEqual(err, fmt.Errorf("fake error")) {
		t.Errorf("CPU Manager RemoveContainer() error. expected error: fake error but got: %v", err)
	}
}

func TestReconcileState(t *testing.T) {
	if runtime.GOOS == "windows" {
		featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.WindowsCPUAndMemoryAffinity, true)
	}

	testPolicy, _ := NewStaticPolicy(
		&topology.CPUTopology{
			NumCPUs:    8,
			NumSockets: 2,
			NumCores:   4,
			CPUDetails: map[int]topology.CPUInfo{
				0: {CoreID: 0, SocketID: 0},
				1: {CoreID: 1, SocketID: 0},
				2: {CoreID: 2, SocketID: 0},
				3: {CoreID: 3, SocketID: 0},
				4: {CoreID: 0, SocketID: 1},
				5: {CoreID: 1, SocketID: 1},
				6: {CoreID: 2, SocketID: 1},
				7: {CoreID: 3, SocketID: 1},
			},
		},
		0,
		cpuset.New(),
		topologymanager.NewFakeManager(),
		nil)

	testCases := []struct {
		description                  string
		policy                       Policy
		activePods                   []*v1.Pod
		pspPS                        v1.PodStatus
		pspFound                     bool
		updateErr                    error
		stAssignments                state.ContainerCPUAssignments
		stDefaultCPUSet              cpuset.CPUSet
		lastUpdateStAssignments      state.ContainerCPUAssignments
		lastUpdateStDefaultCPUSet    cpuset.CPUSet
		expectStAssignments          state.ContainerCPUAssignments
		expectStDefaultCPUSet        cpuset.CPUSet
		expectSucceededContainerName string
		expectFailedContainerName    string
	}{
		{
			description: "cpu manager reconcile - no error",
			policy:      testPolicy,
			activePods: []*v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "fakePodName",
						UID:  "fakePodUID",
					},
					Spec: v1.PodSpec{
						Containers: []v1.Container{
							{
								Name: "fakeContainerName",
							},
						},
					},
				},
			},
			pspPS: v1.PodStatus{
				ContainerStatuses: []v1.ContainerStatus{
					{
						Name:        "fakeContainerName",
						ContainerID: "docker://fakeContainerID",
						State: v1.ContainerState{
							Running: &v1.ContainerStateRunning{},
						},
					},
				},
			},
			pspFound:  true,
			updateErr: nil,
			stAssignments: state.ContainerCPUAssignments{
				"fakePodUID": map[string]cpuset.CPUSet{
					"fakeContainerName": cpuset.New(1, 2),
				},
			},
			stDefaultCPUSet:           cpuset.New(3, 4, 5, 6, 7),
			lastUpdateStAssignments:   state.ContainerCPUAssignments{},
			lastUpdateStDefaultCPUSet: cpuset.New(),
			expectStAssignments: state.ContainerCPUAssignments{
				"fakePodUID": map[string]cpuset.CPUSet{
					"fakeContainerName": cpuset.New(1, 2),
				},
			},
			expectStDefaultCPUSet:        cpuset.New(3, 4, 5, 6, 7),
			expectSucceededContainerName: "fakeContainerName",
			expectFailedContainerName:    "",
		},
		{
			description: "cpu manager reconcile init container - no error",
			policy:      testPolicy,
			activePods: []*v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "fakePodName",
						UID:  "fakePodUID",
					},
					Spec: v1.PodSpec{
						InitContainers: []v1.Container{
							{
								Name: "fakeContainerName",
							},
						},
					},
				},
			},
			pspPS: v1.PodStatus{
				InitContainerStatuses: []v1.ContainerStatus{
					{
						Name:        "fakeContainerName",
						ContainerID: "docker://fakeContainerID",
						State: v1.ContainerState{
							Running: &v1.ContainerStateRunning{},
						},
					},
				},
			},
			pspFound:  true,
			updateErr: nil,
			stAssignments: state.ContainerCPUAssignments{
				"fakePodUID": map[string]cpuset.CPUSet{
					"fakeContainerName": cpuset.New(1, 2),
				},
			},
			stDefaultCPUSet:           cpuset.New(3, 4, 5, 6, 7),
			lastUpdateStAssignments:   state.ContainerCPUAssignments{},
			lastUpdateStDefaultCPUSet: cpuset.New(),
			expectStAssignments: state.ContainerCPUAssignments{
				"fakePodUID": map[string]cpuset.CPUSet{
					"fakeContainerName": cpuset.New(1, 2),
				},
			},
			expectStDefaultCPUSet:        cpuset.New(3, 4, 5, 6, 7),
			expectSucceededContainerName: "fakeContainerName",
			expectFailedContainerName:    "",
		},
		{
			description: "cpu manager reconcile - pod status not found",
			policy:      testPolicy,
			activePods: []*v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "fakePodName",
						UID:  "fakePodUID",
					},
					Spec: v1.PodSpec{
						Containers: []v1.Container{
							{
								Name: "fakeContainerName",
							},
						},
					},
				},
			},
			pspPS:                        v1.PodStatus{},
			pspFound:                     false,
			updateErr:                    nil,
			stAssignments:                state.ContainerCPUAssignments{},
			stDefaultCPUSet:              cpuset.New(),
			lastUpdateStAssignments:      state.ContainerCPUAssignments{},
			lastUpdateStDefaultCPUSet:    cpuset.New(),
			expectStAssignments:          state.ContainerCPUAssignments{},
			expectStDefaultCPUSet:        cpuset.New(),
			expectSucceededContainerName: "",
			expectFailedContainerName:    "",
		},
		{
			description: "cpu manager reconcile - container state not found",
			policy:      testPolicy,
			activePods: []*v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "fakePodName",
						UID:  "fakePodUID",
					},
					Spec: v1.PodSpec{
						Containers: []v1.Container{
							{
								Name: "fakeContainerName",
							},
						},
					},
				},
			},
			pspPS: v1.PodStatus{
				ContainerStatuses: []v1.ContainerStatus{
					{
						Name:        "fakeContainerName1",
						ContainerID: "docker://fakeContainerID",
					},
				},
			},
			pspFound:                     true,
			updateErr:                    nil,
			stAssignments:                state.ContainerCPUAssignments{},
			stDefaultCPUSet:              cpuset.New(),
			lastUpdateStAssignments:      state.ContainerCPUAssignments{},
			lastUpdateStDefaultCPUSet:    cpuset.New(),
			expectStAssignments:          state.ContainerCPUAssignments{},
			expectStDefaultCPUSet:        cpuset.New(),
			expectSucceededContainerName: "",
			expectFailedContainerName:    "fakeContainerName",
		},
		{
			description: "cpu manager reconclie - cpuset is empty",
			policy:      testPolicy,
			activePods: []*v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "fakePodName",
						UID:  "fakePodUID",
					},
					Spec: v1.PodSpec{
						Containers: []v1.Container{
							{
								Name: "fakeContainerName",
							},
						},
					},
				},
			},
			pspPS: v1.PodStatus{
				ContainerStatuses: []v1.ContainerStatus{
					{
						Name:        "fakeContainerName",
						ContainerID: "docker://fakeContainerID",
						State: v1.ContainerState{
							Running: &v1.ContainerStateRunning{},
						},
					},
				},
			},
			pspFound:  true,
			updateErr: nil,
			stAssignments: state.ContainerCPUAssignments{
				"fakePodUID": map[string]cpuset.CPUSet{
					"fakeContainerName": cpuset.New(),
				},
			},
			stDefaultCPUSet:           cpuset.New(1, 2, 3, 4, 5, 6, 7),
			lastUpdateStAssignments:   state.ContainerCPUAssignments{},
			lastUpdateStDefaultCPUSet: cpuset.New(),
			expectStAssignments: state.ContainerCPUAssignments{
				"fakePodUID": map[string]cpuset.CPUSet{
					"fakeContainerName": cpuset.New(),
				},
			},
			expectStDefaultCPUSet:        cpuset.New(1, 2, 3, 4, 5, 6, 7),
			expectSucceededContainerName: "",
			expectFailedContainerName:    "fakeContainerName",
		},
		{
			description: "cpu manager reconclie - container update error",
			policy:      testPolicy,
			activePods: []*v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "fakePodName",
						UID:  "fakePodUID",
					},
					Spec: v1.PodSpec{
						Containers: []v1.Container{
							{
								Name: "fakeContainerName",
							},
						},
					},
				},
			},
			pspPS: v1.PodStatus{
				ContainerStatuses: []v1.ContainerStatus{
					{
						Name:        "fakeContainerName",
						ContainerID: "docker://fakeContainerID",
						State: v1.ContainerState{
							Running: &v1.ContainerStateRunning{},
						},
					},
				},
			},
			pspFound:  true,
			updateErr: fmt.Errorf("fake container update error"),
			stAssignments: state.ContainerCPUAssignments{
				"fakePodUID": map[string]cpuset.CPUSet{
					"fakeContainerName": cpuset.New(1, 2),
				},
			},
			stDefaultCPUSet:           cpuset.New(3, 4, 5, 6, 7),
			lastUpdateStAssignments:   state.ContainerCPUAssignments{},
			lastUpdateStDefaultCPUSet: cpuset.New(),
			expectStAssignments: state.ContainerCPUAssignments{
				"fakePodUID": map[string]cpuset.CPUSet{
					"fakeContainerName": cpuset.New(1, 2),
				},
			},
			expectStDefaultCPUSet:        cpuset.New(3, 4, 5, 6, 7),
			expectSucceededContainerName: "",
			expectFailedContainerName:    "fakeContainerName",
		},
		{
			description: "cpu manager reconcile - state has inactive container",
			policy:      testPolicy,
			activePods: []*v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "fakePodName",
						UID:  "fakePodUID",
					},
					Spec: v1.PodSpec{
						Containers: []v1.Container{
							{
								Name: "fakeContainerName",
							},
						},
					},
				},
			},
			pspPS: v1.PodStatus{
				ContainerStatuses: []v1.ContainerStatus{
					{
						Name:        "fakeContainerName",
						ContainerID: "docker://fakeContainerID",
						State: v1.ContainerState{
							Running: &v1.ContainerStateRunning{},
						},
					},
				},
			},
			pspFound:  true,
			updateErr: nil,
			stAssignments: state.ContainerCPUAssignments{
				"fakePodUID": map[string]cpuset.CPUSet{
					"fakeContainerName": cpuset.New(1, 2),
				},
				"secondfakePodUID": map[string]cpuset.CPUSet{
					"secondfakeContainerName": cpuset.New(3, 4),
				},
			},
			stDefaultCPUSet:           cpuset.New(5, 6, 7),
			lastUpdateStAssignments:   state.ContainerCPUAssignments{},
			lastUpdateStDefaultCPUSet: cpuset.New(),
			expectStAssignments: state.ContainerCPUAssignments{
				"fakePodUID": map[string]cpuset.CPUSet{
					"fakeContainerName": cpuset.New(1, 2),
				},
			},
			expectStDefaultCPUSet:        cpuset.New(3, 4, 5, 6, 7),
			expectSucceededContainerName: "fakeContainerName",
			expectFailedContainerName:    "",
		},
		{
			description: "cpu manager reconcile - last update state is current",
			policy:      testPolicy,
			activePods: []*v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "fakePodName",
						UID:  "fakePodUID",
					},
					Spec: v1.PodSpec{
						Containers: []v1.Container{
							{
								Name: "fakeContainerName",
							},
						},
					},
				},
			},
			pspPS: v1.PodStatus{
				ContainerStatuses: []v1.ContainerStatus{
					{
						Name:        "fakeContainerName",
						ContainerID: "docker://fakeContainerID",
						State: v1.ContainerState{
							Running: &v1.ContainerStateRunning{},
						},
					},
				},
			},
			pspFound:  true,
			updateErr: nil,
			stAssignments: state.ContainerCPUAssignments{
				"fakePodUID": map[string]cpuset.CPUSet{
					"fakeContainerName": cpuset.New(1, 2),
				},
			},
			stDefaultCPUSet: cpuset.New(5, 6, 7),
			lastUpdateStAssignments: state.ContainerCPUAssignments{
				"fakePodUID": map[string]cpuset.CPUSet{
					"fakeContainerName": cpuset.New(1, 2),
				},
			},
			lastUpdateStDefaultCPUSet: cpuset.New(5, 6, 7),
			expectStAssignments: state.ContainerCPUAssignments{
				"fakePodUID": map[string]cpuset.CPUSet{
					"fakeContainerName": cpuset.New(1, 2),
				},
			},
			expectStDefaultCPUSet:        cpuset.New(5, 6, 7),
			expectSucceededContainerName: "fakeContainerName",
			expectFailedContainerName:    "",
		},
		{
			description: "cpu manager reconcile - last update state is not current",
			policy:      testPolicy,
			activePods: []*v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "fakePodName",
						UID:  "fakePodUID",
					},
					Spec: v1.PodSpec{
						Containers: []v1.Container{
							{
								Name: "fakeContainerName",
							},
						},
					},
				},
			},
			pspPS: v1.PodStatus{
				ContainerStatuses: []v1.ContainerStatus{
					{
						Name:        "fakeContainerName",
						ContainerID: "docker://fakeContainerID",
						State: v1.ContainerState{
							Running: &v1.ContainerStateRunning{},
						},
					},
				},
			},
			pspFound:  true,
			updateErr: nil,
			stAssignments: state.ContainerCPUAssignments{
				"fakePodUID": map[string]cpuset.CPUSet{
					"fakeContainerName": cpuset.New(1, 2),
				},
			},
			stDefaultCPUSet: cpuset.New(3, 4, 5, 6, 7),
			lastUpdateStAssignments: state.ContainerCPUAssignments{
				"fakePodUID": map[string]cpuset.CPUSet{
					"fakeContainerName": cpuset.New(3, 4),
				},
			},
			lastUpdateStDefaultCPUSet: cpuset.New(1, 2, 5, 6, 7),
			expectStAssignments: state.ContainerCPUAssignments{
				"fakePodUID": map[string]cpuset.CPUSet{
					"fakeContainerName": cpuset.New(1, 2),
				},
			},
			expectStDefaultCPUSet:        cpuset.New(3, 4, 5, 6, 7),
			expectSucceededContainerName: "fakeContainerName",
			expectFailedContainerName:    "",
		},
	}

	for _, testCase := range testCases {
		mgr := &manager{
			policy: testCase.policy,
			state: &mockState{
				assignments:   testCase.stAssignments,
				defaultCPUSet: testCase.stDefaultCPUSet,
			},
			lastUpdateState: state.NewMemoryState(),
			containerRuntime: mockRuntimeService{
				err: testCase.updateErr,
			},
			containerMap: containermap.NewContainerMap(),
			activePods: func() []*v1.Pod {
				return testCase.activePods
			},
			podStatusProvider: mockPodStatusProvider{
				podStatus: testCase.pspPS,
				found:     testCase.pspFound,
			},
		}
		mgr.sourcesReady = &sourcesReadyStub{}
		success, failure := mgr.reconcileState()

		if !reflect.DeepEqual(testCase.expectStAssignments, mgr.state.GetCPUAssignments()) {
			t.Errorf("%v", testCase.description)
			t.Errorf("Expected state container cpu assignments: %v, actual: %v", testCase.expectStAssignments, mgr.state.GetCPUAssignments())

		}

		if !reflect.DeepEqual(testCase.expectStDefaultCPUSet, mgr.state.GetDefaultCPUSet()) {
			t.Errorf("%v", testCase.description)
			t.Errorf("Expected state default cpuset: %v, actual: %v", testCase.expectStDefaultCPUSet, mgr.state.GetDefaultCPUSet())

		}

		if testCase.expectSucceededContainerName != "" {
			// Search succeeded reconciled containers for the supplied name.
			foundSucceededContainer := false
			for _, reconciled := range success {
				if reconciled.containerName == testCase.expectSucceededContainerName {
					foundSucceededContainer = true
					break
				}
			}
			if !foundSucceededContainer {
				t.Errorf("%v", testCase.description)
				t.Errorf("Expected reconciliation success for container: %s", testCase.expectSucceededContainerName)
			}
		}

		if testCase.expectFailedContainerName != "" {
			// Search failed reconciled containers for the supplied name.
			foundFailedContainer := false
			for _, reconciled := range failure {
				if reconciled.containerName == testCase.expectFailedContainerName {
					foundFailedContainer = true
					break
				}
			}
			if !foundFailedContainer {
				t.Errorf("%v", testCase.description)
				t.Errorf("Expected reconciliation failure for container: %s", testCase.expectFailedContainerName)
			}
		}
	}
}

// above test cases are without kubelet --reserved-cpus cmd option
// the following tests are with --reserved-cpus configured
func TestCPUManagerAddWithResvList(t *testing.T) {
	if runtime.GOOS == "windows" {
		featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.WindowsCPUAndMemoryAffinity, true)
	}

	testPolicy, _ := NewStaticPolicy(
		&topology.CPUTopology{
			NumCPUs:    4,
			NumSockets: 1,
			NumCores:   4,
			CPUDetails: map[int]topology.CPUInfo{
				0: {CoreID: 0, SocketID: 0},
				1: {CoreID: 1, SocketID: 0},
				2: {CoreID: 2, SocketID: 0},
				3: {CoreID: 3, SocketID: 0},
			},
		},
		1,
		cpuset.New(0),
		topologymanager.NewFakeManager(),
		nil)
	testCases := []struct {
		description        string
		updateErr          error
		policy             Policy
		expCPUSet          cpuset.CPUSet
		expAllocateErr     error
		expAddContainerErr error
	}{
		{
			description:        "cpu manager add - no error",
			updateErr:          nil,
			policy:             testPolicy,
			expCPUSet:          cpuset.New(0, 3),
			expAllocateErr:     nil,
			expAddContainerErr: nil,
		},
	}

	for _, testCase := range testCases {
		mgr := &manager{
			policy: testCase.policy,
			state: &mockState{
				assignments:   state.ContainerCPUAssignments{},
				defaultCPUSet: cpuset.New(0, 1, 2, 3),
			},
			lastUpdateState: state.NewMemoryState(),
			containerRuntime: mockRuntimeService{
				err: testCase.updateErr,
			},
			containerMap:      containermap.NewContainerMap(),
			podStatusProvider: mockPodStatusProvider{},
			sourcesReady:      &sourcesReadyStub{},
		}

		pod := makePod("fakePod", "fakeContainer", "2", "2")
		container := &pod.Spec.Containers[0]
		mgr.activePods = func() []*v1.Pod { return []*v1.Pod{pod} }

		err := mgr.Allocate(pod, container)
		if !reflect.DeepEqual(err, testCase.expAllocateErr) {
			t.Errorf("CPU Manager Allocate() error (%v). expected error: %v but got: %v",
				testCase.description, testCase.expAllocateErr, err)
		}

		mgr.AddContainer(pod, container, "fakeID")
		_, _, err = mgr.containerMap.GetContainerRef("fakeID")
		if !reflect.DeepEqual(err, testCase.expAddContainerErr) {
			t.Errorf("CPU Manager AddContainer() error (%v). expected error: %v but got: %v",
				testCase.description, testCase.expAddContainerErr, err)
		}
		if !testCase.expCPUSet.Equals(mgr.state.GetDefaultCPUSet()) {
			t.Errorf("CPU Manager AddContainer() error (%v). expected cpuset: %v but got: %v",
				testCase.description, testCase.expCPUSet, mgr.state.GetDefaultCPUSet())
		}
	}
}

func TestCPUManagerHandlePolicyOptions(t *testing.T) {
	if runtime.GOOS == "windows" {
		featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.WindowsCPUAndMemoryAffinity, true)
	}

	testCases := []struct {
		description      string
		cpuPolicyName    string
		cpuPolicyOptions map[string]string
		expectedError    error
	}{
		{
			description:   "options to none policy",
			cpuPolicyName: "none",
			cpuPolicyOptions: map[string]string{
				FullPCPUsOnlyOption: "true",
			},
			expectedError: fmt.Errorf("received unsupported options"),
		},
	}

	// any correct realistic topology is fine. We pick a simple one.
	mockedMachineInfo := cadvisorapi.MachineInfo{
		NumCores: 4,
		Topology: []cadvisorapi.Node{
			{
				Cores: []cadvisorapi.Core{
					{
						Id:      0,
						Threads: []int{0},
					},
					{
						Id:      1,
						Threads: []int{1},
					},
					{
						Id:      2,
						Threads: []int{2},
					},
					{
						Id:      3,
						Threads: []int{3},
					},
				},
			},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.description, func(t *testing.T) {
			machineInfo := &mockedMachineInfo
			nodeAllocatableReservation := v1.ResourceList{}
			sDir, err := os.MkdirTemp("", "cpu_manager_test")
			if err != nil {
				t.Errorf("cannot create state file: %s", err.Error())
			}
			defer os.RemoveAll(sDir)

			_, err = NewManager(testCase.cpuPolicyName, testCase.cpuPolicyOptions, 5*time.Second, machineInfo, cpuset.New(), nodeAllocatableReservation, sDir, topologymanager.NewFakeManager())
			if err == nil {
				t.Errorf("Expected error, but NewManager succeeded")
			}
			if !strings.Contains(err.Error(), testCase.expectedError.Error()) {
				t.Errorf("Unexpected error message. Have: %s wants %s", err.Error(), testCase.expectedError.Error())
			}
		})

	}
}

func TestCPUManagerGetAllocatableCPUs(t *testing.T) {
	if runtime.GOOS == "windows" {
		featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.WindowsCPUAndMemoryAffinity, true)
	}

	nonePolicy, _ := NewNonePolicy(nil)
	staticPolicy, _ := NewStaticPolicy(
		&topology.CPUTopology{
			NumCPUs:    4,
			NumSockets: 1,
			NumCores:   4,
			CPUDetails: map[int]topology.CPUInfo{
				0: {CoreID: 0, SocketID: 0},
				1: {CoreID: 1, SocketID: 0},
				2: {CoreID: 2, SocketID: 0},
				3: {CoreID: 3, SocketID: 0},
			},
		},
		1,
		cpuset.New(0),
		topologymanager.NewFakeManager(),
		nil)

	testCases := []struct {
		description        string
		policy             Policy
		expAllocatableCPUs cpuset.CPUSet
	}{
		{
			description:        "None Policy",
			policy:             nonePolicy,
			expAllocatableCPUs: cpuset.New(),
		},
		{
			description:        "Static Policy",
			policy:             staticPolicy,
			expAllocatableCPUs: cpuset.New(1, 2, 3),
		},
	}
	for _, testCase := range testCases {
		mgr := &manager{
			policy:     testCase.policy,
			activePods: func() []*v1.Pod { return nil },
			state: &mockState{
				assignments:   state.ContainerCPUAssignments{},
				defaultCPUSet: cpuset.New(0, 1, 2, 3),
			},
			lastUpdateState:   state.NewMemoryState(),
			containerMap:      containermap.NewContainerMap(),
			podStatusProvider: mockPodStatusProvider{},
			sourcesReady:      &sourcesReadyStub{},
		}
		mgr.sourcesReady = &sourcesReadyStub{}
		mgr.allocatableCPUs = testCase.policy.GetAllocatableCPUs(mgr.state)

		pod := makePod("fakePod", "fakeContainer", "2", "2")
		container := &pod.Spec.Containers[0]

		_ = mgr.Allocate(pod, container)

		if !mgr.GetAllocatableCPUs().Equals(testCase.expAllocatableCPUs) {
			t.Errorf("Policy GetAllocatableCPUs() error (%v). expected cpuset %v for container %v but got %v",
				testCase.description, testCase.expAllocatableCPUs, "fakeContainer", mgr.GetAllocatableCPUs())
		}
	}
}
