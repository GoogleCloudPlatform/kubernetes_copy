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
	"os"
	"reflect"
	"strings"
	"testing"

	"k8s.io/klog/v2"

	cadvisorapi "github.com/google/cadvisor/info/v1"
	"github.com/stretchr/testify/assert"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
	kubeletconfig "k8s.io/kubernetes/pkg/kubelet/apis/config"
	"k8s.io/kubernetes/pkg/kubelet/cm/containermap"
	"k8s.io/kubernetes/pkg/kubelet/cm/memorymanager/state"
	"k8s.io/kubernetes/pkg/kubelet/cm/topologymanager"
)

const (
	hugepages2M = "hugepages-2Mi"
	hugepages1G = "hugepages-1Gi"
)

const policyTypeMock policyType = "mock"

type testMemoryManager struct {
	description                string
	machineInfo                cadvisorapi.MachineInfo
	assignments                state.ContainerMemoryAssignments
	expectedAssignments        state.ContainerMemoryAssignments
	machineState               state.NUMANodeMap
	expectedMachineState       state.NUMANodeMap
	expectedError              error
	expectedAllocateError      error
	expectedAddContainerError  error
	updateError                error
	removeContainerID          string
	nodeAllocatableReservation v1.ResourceList
	policyName                 policyType
	affinity                   topologymanager.Store
	systemReservedMemory       []kubeletconfig.MemoryReservation
	expectedHints              map[string][]topologymanager.TopologyHint
	expectedReserved           systemReservedMemory
	reserved                   systemReservedMemory
	podAllocate                *v1.Pod
	firstPod                   *v1.Pod
	activePods                 []*v1.Pod
}

func returnPolicyByName(testCase testMemoryManager) Policy {
	switch testCase.policyName {
	case policyTypeMock:
		return &mockPolicy{
			err: fmt.Errorf("fake reg error"),
		}
	case policyTypeStatic:
		policy, _ := NewPolicyStatic(&testCase.machineInfo, testCase.reserved, topologymanager.NewFakeManager())
		return policy
	case policyTypeNone:
		return NewPolicyNone()
	}
	return nil
}

type mockPolicy struct {
	err error
}

func (p *mockPolicy) Name() string {
	return string(policyTypeMock)
}

func (p *mockPolicy) Start(s state.State) error {
	return p.err
}

func (p *mockPolicy) Allocate(s state.State, pod *v1.Pod, container *v1.Container) error {
	return p.err
}

func (p *mockPolicy) RemoveContainer(s state.State, podUID string, containerName string) {
}

func (p *mockPolicy) GetTopologyHints(s state.State, pod *v1.Pod, container *v1.Container) map[string][]topologymanager.TopologyHint {
	return nil
}

func (p *mockPolicy) GetPodTopologyHints(s state.State, pod *v1.Pod) map[string][]topologymanager.TopologyHint {
	return nil
}

// GetAllocatableMemory returns the amount of allocatable memory for each NUMA node
func (p *mockPolicy) GetAllocatableMemory(s state.State) []state.Block {
	return []state.Block{}
}

func (p *mockPolicy) CanAllocateExclusively() bool {
	return false
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

func getPod(podUID string, containerName string, requirements *v1.ResourceRequirements) *v1.Pod {
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			UID: types.UID(podUID),
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name:      containerName,
					Resources: *requirements,
				},
			},
		},
	}
}

func getPodWithInitContainers(podUID string, containers []v1.Container, initContainers []v1.Container) *v1.Pod {
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			UID: types.UID(podUID),
		},
		Spec: v1.PodSpec{
			InitContainers: initContainers,
			Containers:     containers,
		},
	}
}

func TestValidateReservedMemory(t *testing.T) {
	machineInfo := &cadvisorapi.MachineInfo{
		Topology: []cadvisorapi.Node{
			{Id: 0},
			{Id: 1},
		},
	}
	const msgNotEqual = "the total amount %q of type %q is not equal to the value %q determined by Node Allocatable feature"
	testCases := []struct {
		description                string
		nodeAllocatableReservation v1.ResourceList
		machineInfo                *cadvisorapi.MachineInfo
		systemReservedMemory       []kubeletconfig.MemoryReservation
		expectedError              string
	}{
		{
			"Node Allocatable not set, reserved not set",
			v1.ResourceList{},
			machineInfo,
			[]kubeletconfig.MemoryReservation{},
			"",
		},
		{
			"Node Allocatable set to zero, reserved set to zero",
			v1.ResourceList{v1.ResourceMemory: *resource.NewQuantity(0, resource.DecimalSI)},
			machineInfo,
			[]kubeletconfig.MemoryReservation{
				{
					NumaNode: 0,
					Limits: v1.ResourceList{
						v1.ResourceMemory: *resource.NewQuantity(0, resource.DecimalSI),
					},
				},
			},
			"",
		},
		{
			"Node Allocatable not set (equal zero), reserved set",
			v1.ResourceList{},
			machineInfo,
			[]kubeletconfig.MemoryReservation{
				{
					NumaNode: 0,
					Limits: v1.ResourceList{
						v1.ResourceMemory: *resource.NewQuantity(12, resource.DecimalSI),
					},
				},
			},
			fmt.Sprintf(msgNotEqual, "12", v1.ResourceMemory, "0"),
		},
		{
			"Node Allocatable set, reserved not set",
			v1.ResourceList{hugepages2M: *resource.NewQuantity(5, resource.DecimalSI)},
			machineInfo,
			[]kubeletconfig.MemoryReservation{},
			fmt.Sprintf(msgNotEqual, "0", hugepages2M, "5"),
		},
		{
			"Reserved not equal to Node Allocatable",
			v1.ResourceList{v1.ResourceMemory: *resource.NewQuantity(5, resource.DecimalSI)},
			machineInfo,
			[]kubeletconfig.MemoryReservation{
				{
					NumaNode: 0,
					Limits: v1.ResourceList{
						v1.ResourceMemory: *resource.NewQuantity(12, resource.DecimalSI),
					},
				},
			},
			fmt.Sprintf(msgNotEqual, "12", v1.ResourceMemory, "5"),
		},
		{
			"Reserved contains the NUMA node that does not exist under the machine",
			v1.ResourceList{v1.ResourceMemory: *resource.NewQuantity(17, resource.DecimalSI)},
			machineInfo,
			[]kubeletconfig.MemoryReservation{
				{
					NumaNode: 0,
					Limits: v1.ResourceList{
						v1.ResourceMemory: *resource.NewQuantity(12, resource.DecimalSI),
					},
				},
				{
					NumaNode: 2,
					Limits: v1.ResourceList{
						v1.ResourceMemory: *resource.NewQuantity(5, resource.DecimalSI),
					},
				},
			},
			"the reserved memory configuration references a NUMA node 2 that does not exist on this machine",
		},
		{
			"Reserved total equal to Node Allocatable",
			v1.ResourceList{v1.ResourceMemory: *resource.NewQuantity(17, resource.DecimalSI),
				hugepages2M: *resource.NewQuantity(77, resource.DecimalSI),
				hugepages1G: *resource.NewQuantity(13, resource.DecimalSI)},
			machineInfo,
			[]kubeletconfig.MemoryReservation{
				{
					NumaNode: 0,
					Limits: v1.ResourceList{
						v1.ResourceMemory: *resource.NewQuantity(12, resource.DecimalSI),
						hugepages2M:       *resource.NewQuantity(70, resource.DecimalSI),
						hugepages1G:       *resource.NewQuantity(13, resource.DecimalSI),
					},
				},
				{
					NumaNode: 1,
					Limits: v1.ResourceList{
						v1.ResourceMemory: *resource.NewQuantity(5, resource.DecimalSI),
						hugepages2M:       *resource.NewQuantity(7, resource.DecimalSI),
					},
				},
			},
			"",
		},
		{
			"Reserved total hugapages-2M not equal to Node Allocatable",
			v1.ResourceList{v1.ResourceMemory: *resource.NewQuantity(17, resource.DecimalSI),
				hugepages2M: *resource.NewQuantity(14, resource.DecimalSI),
				hugepages1G: *resource.NewQuantity(13, resource.DecimalSI)},
			machineInfo,
			[]kubeletconfig.MemoryReservation{
				{
					NumaNode: 0,
					Limits: v1.ResourceList{
						v1.ResourceMemory: *resource.NewQuantity(12, resource.DecimalSI),
						hugepages2M:       *resource.NewQuantity(70, resource.DecimalSI),
						hugepages1G:       *resource.NewQuantity(13, resource.DecimalSI),
					},
				},
				{
					NumaNode: 1,
					Limits: v1.ResourceList{
						v1.ResourceMemory: *resource.NewQuantity(5, resource.DecimalSI),
						hugepages2M:       *resource.NewQuantity(7, resource.DecimalSI),
					},
				},
			},

			fmt.Sprintf(msgNotEqual, "77", hugepages2M, "14"),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.description, func(t *testing.T) {
			err := validateReservedMemory(tc.machineInfo, tc.nodeAllocatableReservation, tc.systemReservedMemory)
			if strings.TrimSpace(tc.expectedError) != "" {
				assert.Error(t, err)
				assert.Equal(t, tc.expectedError, err.Error())
			}
		})
	}
}

func TestConvertPreReserved(t *testing.T) {
	machineInfo := cadvisorapi.MachineInfo{
		Topology: []cadvisorapi.Node{
			{Id: 0},
			{Id: 1},
		},
	}

	testCases := []struct {
		description            string
		systemReserved         []kubeletconfig.MemoryReservation
		systemReservedExpected systemReservedMemory
		expectedError          string
	}{
		{
			"Empty",
			[]kubeletconfig.MemoryReservation{},
			systemReservedMemory{
				0: map[v1.ResourceName]uint64{},
				1: map[v1.ResourceName]uint64{},
			},
			"",
		},
		{
			"Single NUMA node is reserved",
			[]kubeletconfig.MemoryReservation{
				{
					NumaNode: 0,
					Limits: v1.ResourceList{
						v1.ResourceMemory: *resource.NewQuantity(12, resource.DecimalSI),
						hugepages2M:       *resource.NewQuantity(70, resource.DecimalSI),
						hugepages1G:       *resource.NewQuantity(13, resource.DecimalSI),
					},
				},
			},
			systemReservedMemory{
				0: map[v1.ResourceName]uint64{
					v1.ResourceMemory: 12,
					hugepages2M:       70,
					hugepages1G:       13,
				},
				1: map[v1.ResourceName]uint64{},
			},
			"",
		},
		{
			"Both NUMA nodes are reserved",
			[]kubeletconfig.MemoryReservation{
				{
					NumaNode: 0,
					Limits: v1.ResourceList{
						v1.ResourceMemory: *resource.NewQuantity(12, resource.DecimalSI),
						hugepages2M:       *resource.NewQuantity(70, resource.DecimalSI),
						hugepages1G:       *resource.NewQuantity(13, resource.DecimalSI),
					},
				},
				{
					NumaNode: 1,
					Limits: v1.ResourceList{
						v1.ResourceMemory: *resource.NewQuantity(5, resource.DecimalSI),
						hugepages2M:       *resource.NewQuantity(7, resource.DecimalSI),
					},
				},
			},
			systemReservedMemory{
				0: map[v1.ResourceName]uint64{
					v1.ResourceMemory: 12,
					hugepages2M:       70,
					hugepages1G:       13,
				},
				1: map[v1.ResourceName]uint64{
					v1.ResourceMemory: 5,
					hugepages2M:       7,
				},
			},
			"",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.description, func(t *testing.T) {
			reserved, _ := convertReserved(&machineInfo, tc.systemReserved)
			if !reflect.DeepEqual(reserved, tc.systemReservedExpected) {
				t.Errorf("got %v, expected %v", reserved, tc.systemReservedExpected)
			}
		})
	}
}

func TestGetSystemReservedMemory(t *testing.T) {
	machineInfo := returnMachineInfo()
	testCases := []testMemoryManager{
		{
			description:                "Should return empty map when reservation is not done",
			nodeAllocatableReservation: v1.ResourceList{},
			systemReservedMemory:       []kubeletconfig.MemoryReservation{},
			expectedReserved: systemReservedMemory{
				0: {},
				1: {},
			},
			expectedError: nil,
			machineInfo:   machineInfo,
		},
		{
			description:                "Should return error when Allocatable reservation is not equal to the reserved memory",
			nodeAllocatableReservation: v1.ResourceList{},
			systemReservedMemory: []kubeletconfig.MemoryReservation{
				{
					NumaNode: 0,
					Limits: v1.ResourceList{
						v1.ResourceMemory: *resource.NewQuantity(gb, resource.BinarySI),
					},
				},
			},
			expectedReserved: nil,
			expectedError:    fmt.Errorf("the total amount \"1Gi\" of type \"memory\" is not equal to the value \"0\" determined by Node Allocatable feature"),
			machineInfo:      machineInfo,
		},
		{
			description:                "Reserved should be equal to systemReservedMemory",
			nodeAllocatableReservation: v1.ResourceList{v1.ResourceMemory: *resource.NewQuantity(2*gb, resource.BinarySI)},
			systemReservedMemory: []kubeletconfig.MemoryReservation{
				{
					NumaNode: 0,
					Limits: v1.ResourceList{
						v1.ResourceMemory: *resource.NewQuantity(gb, resource.BinarySI),
					},
				},
				{
					NumaNode: 1,
					Limits: v1.ResourceList{
						v1.ResourceMemory: *resource.NewQuantity(gb, resource.BinarySI),
					},
				},
			},
			expectedReserved: systemReservedMemory{
				0: map[v1.ResourceName]uint64{
					v1.ResourceMemory: 1 * gb,
				},
				1: map[v1.ResourceName]uint64{
					v1.ResourceMemory: 1 * gb,
				},
			},
			expectedError: nil,
			machineInfo:   machineInfo,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.description, func(t *testing.T) {
			res, err := getSystemReservedMemory(&testCase.machineInfo, testCase.nodeAllocatableReservation, testCase.systemReservedMemory)

			if !reflect.DeepEqual(res, testCase.expectedReserved) {
				t.Errorf("Memory Manager getReservedMemory() error, expected reserved %+v, but got: %+v",
					testCase.expectedReserved, res)
			}
			if !reflect.DeepEqual(err, testCase.expectedError) {
				t.Errorf("Memory Manager getReservedMemory() error, expected error %v, but got: %v",
					testCase.expectedError, err)
			}

		})
	}
}

func TestRemoveStaleState(t *testing.T) {
	machineInfo := returnMachineInfo()
	testCases := []testMemoryManager{
		{
			description: "Should fail - policy returns an error",
			policyName:  policyTypeMock,
			machineInfo: machineInfo,
			reserved: systemReservedMemory{
				0: map[v1.ResourceName]uint64{
					v1.ResourceMemory: 1 * gb,
				},
				1: map[v1.ResourceName]uint64{
					v1.ResourceMemory: 1 * gb,
				},
			},
			assignments: state.ContainerMemoryAssignments{
				"fakePod1": map[string][]state.Block{
					"fakeContainer1": {
						{
							NUMAAffinity: []int{0},
							Type:         v1.ResourceMemory,
							Size:         1 * gb,
						},
						{
							NUMAAffinity: []int{0},
							Type:         hugepages1Gi,
							Size:         1 * gb,
						},
					},
					"fakeContainer2": {
						{
							NUMAAffinity: []int{0},
							Type:         v1.ResourceMemory,
							Size:         1 * gb,
						},
						{
							NUMAAffinity: []int{0},
							Type:         hugepages1Gi,
							Size:         1 * gb,
						},
					},
				},
			},
			expectedAssignments: state.ContainerMemoryAssignments{
				"fakePod1": map[string][]state.Block{
					"fakeContainer1": {
						{
							NUMAAffinity: []int{0},
							Type:         v1.ResourceMemory,
							Size:         1 * gb,
						},
						{
							NUMAAffinity: []int{0},
							Type:         hugepages1Gi,
							Size:         1 * gb,
						},
					},
					"fakeContainer2": {
						{
							NUMAAffinity: []int{0},
							Type:         v1.ResourceMemory,
							Size:         1 * gb,
						},
						{
							NUMAAffinity: []int{0},
							Type:         hugepages1Gi,
							Size:         1 * gb,
						},
					},
				},
			},
			machineState: state.NUMANodeMap{
				0: &state.NUMANodeState{
					Cells:               []int{0},
					NumberOfAssignments: 4,
					MemoryMap: map[v1.ResourceName]*state.MemoryTable{
						v1.ResourceMemory: {
							Allocatable:    9 * gb,
							Free:           7 * gb,
							Reserved:       2 * gb,
							SystemReserved: 1 * gb,
							TotalMemSize:   10 * gb,
						},
						hugepages1Gi: {
							Allocatable:    5 * gb,
							Free:           3 * gb,
							Reserved:       2 * gb,
							SystemReserved: 0 * gb,
							TotalMemSize:   5 * gb,
						},
					},
				},
				1: &state.NUMANodeState{
					Cells:               []int{1},
					NumberOfAssignments: 0,
					MemoryMap: map[v1.ResourceName]*state.MemoryTable{
						v1.ResourceMemory: {
							Allocatable:    9 * gb,
							Free:           9 * gb,
							Reserved:       0 * gb,
							SystemReserved: 1 * gb,
							TotalMemSize:   10 * gb,
						},
						hugepages1Gi: {
							Allocatable:    5 * gb,
							Free:           5 * gb,
							Reserved:       0,
							SystemReserved: 0,
							TotalMemSize:   5 * gb,
						},
					},
				},
			},
			expectedMachineState: state.NUMANodeMap{
				0: &state.NUMANodeState{
					Cells:               []int{0},
					NumberOfAssignments: 4,
					MemoryMap: map[v1.ResourceName]*state.MemoryTable{
						v1.ResourceMemory: {
							Allocatable:    9 * gb,
							Free:           7 * gb,
							Reserved:       2 * gb,
							SystemReserved: 1 * gb,
							TotalMemSize:   10 * gb,
						},
						hugepages1Gi: {
							Allocatable:    5 * gb,
							Free:           3 * gb,
							Reserved:       2 * gb,
							SystemReserved: 0 * gb,
							TotalMemSize:   5 * gb,
						},
					},
				},
				1: &state.NUMANodeState{
					Cells:               []int{1},
					NumberOfAssignments: 0,
					MemoryMap: map[v1.ResourceName]*state.MemoryTable{
						v1.ResourceMemory: {
							Allocatable:    9 * gb,
							Free:           9 * gb,
							Reserved:       0 * gb,
							SystemReserved: 1 * gb,
							TotalMemSize:   10 * gb,
						},
						hugepages1Gi: {
							Allocatable:    5 * gb,
							Free:           5 * gb,
							Reserved:       0,
							SystemReserved: 0,
							TotalMemSize:   5 * gb,
						},
					},
				},
			},
		},
		{
			description: "Stale state successfully removed, without multi NUMA assignments",
			policyName:  policyTypeStatic,
			machineInfo: machineInfo,
			reserved: systemReservedMemory{
				0: map[v1.ResourceName]uint64{
					v1.ResourceMemory: 1 * gb,
				},
				1: map[v1.ResourceName]uint64{
					v1.ResourceMemory: 1 * gb,
				},
			},
			assignments: state.ContainerMemoryAssignments{
				"fakePod1": map[string][]state.Block{
					"fakeContainer1": {
						{
							NUMAAffinity: []int{0},
							Type:         v1.ResourceMemory,
							Size:         1 * gb,
						},
						{
							NUMAAffinity: []int{0},
							Type:         hugepages1Gi,
							Size:         1 * gb,
						},
					},
					"fakeContainer2": {
						{
							NUMAAffinity: []int{0},
							Type:         v1.ResourceMemory,
							Size:         1 * gb,
						},
						{
							NUMAAffinity: []int{0},
							Type:         hugepages1Gi,
							Size:         1 * gb,
						},
					},
				},
			},
			expectedAssignments: state.ContainerMemoryAssignments{},
			machineState: state.NUMANodeMap{
				0: &state.NUMANodeState{
					Cells:               []int{0},
					NumberOfAssignments: 4,
					MemoryMap: map[v1.ResourceName]*state.MemoryTable{
						v1.ResourceMemory: {
							Allocatable:    9 * gb,
							Free:           7 * gb,
							Reserved:       2 * gb,
							SystemReserved: 1 * gb,
							TotalMemSize:   10 * gb,
						},
						hugepages1Gi: {
							Allocatable:    5 * gb,
							Free:           3 * gb,
							Reserved:       2 * gb,
							SystemReserved: 0 * gb,
							TotalMemSize:   5 * gb,
						},
					},
				},
				1: &state.NUMANodeState{
					Cells:               []int{1},
					NumberOfAssignments: 0,
					MemoryMap: map[v1.ResourceName]*state.MemoryTable{
						v1.ResourceMemory: {
							Allocatable:    9 * gb,
							Free:           9 * gb,
							Reserved:       0 * gb,
							SystemReserved: 1 * gb,
							TotalMemSize:   10 * gb,
						},
						hugepages1Gi: {
							Allocatable:    5 * gb,
							Free:           5 * gb,
							Reserved:       0,
							SystemReserved: 0,
							TotalMemSize:   5 * gb,
						},
					},
				},
			},
			expectedMachineState: state.NUMANodeMap{
				0: &state.NUMANodeState{
					Cells:               []int{0},
					NumberOfAssignments: 0,
					MemoryMap: map[v1.ResourceName]*state.MemoryTable{
						v1.ResourceMemory: {
							Allocatable:    9 * gb,
							Free:           9 * gb,
							Reserved:       0 * gb,
							SystemReserved: 1 * gb,
							TotalMemSize:   10 * gb,
						},
						hugepages1Gi: {
							Allocatable:    5 * gb,
							Free:           5 * gb,
							Reserved:       0,
							SystemReserved: 0,
							TotalMemSize:   5 * gb,
						},
					},
				},
				1: &state.NUMANodeState{
					Cells:               []int{1},
					NumberOfAssignments: 0,
					MemoryMap: map[v1.ResourceName]*state.MemoryTable{
						v1.ResourceMemory: {
							Allocatable:    9 * gb,
							Free:           9 * gb,
							Reserved:       0 * gb,
							SystemReserved: 1 * gb,
							TotalMemSize:   10 * gb,
						},
						hugepages1Gi: {
							Allocatable:    5 * gb,
							Free:           5 * gb,
							Reserved:       0,
							SystemReserved: 0,
							TotalMemSize:   5 * gb,
						},
					},
				},
			},
		},
		{
			description: "Stale state successfully removed, with multi NUMA assignments",
			policyName:  policyTypeStatic,
			machineInfo: machineInfo,
			reserved: systemReservedMemory{
				0: map[v1.ResourceName]uint64{
					v1.ResourceMemory: 1 * gb,
				},
				1: map[v1.ResourceName]uint64{
					v1.ResourceMemory: 1 * gb,
				},
			},
			assignments: state.ContainerMemoryAssignments{
				"fakePod1": map[string][]state.Block{
					"fakeContainer1": {
						{
							NUMAAffinity: []int{0, 1},
							Type:         v1.ResourceMemory,
							Size:         12 * gb,
						},
						{
							NUMAAffinity: []int{0, 1},
							Type:         hugepages1Gi,
							Size:         1 * gb,
						},
					},
					"fakeContainer2": {
						{
							NUMAAffinity: []int{0, 1},
							Type:         v1.ResourceMemory,
							Size:         1 * gb,
						},
						{
							NUMAAffinity: []int{0, 1},
							Type:         hugepages1Gi,
							Size:         1 * gb,
						},
					},
				},
			},
			expectedAssignments: state.ContainerMemoryAssignments{},
			machineState: state.NUMANodeMap{
				0: &state.NUMANodeState{
					Cells:               []int{0, 1},
					NumberOfAssignments: 4,
					MemoryMap: map[v1.ResourceName]*state.MemoryTable{
						v1.ResourceMemory: {
							Allocatable:    9 * gb,
							Free:           0 * gb,
							Reserved:       9 * gb,
							SystemReserved: 1 * gb,
							TotalMemSize:   10 * gb,
						},
						hugepages1Gi: {
							Allocatable:    5 * gb,
							Free:           4 * gb,
							Reserved:       1 * gb,
							SystemReserved: 0 * gb,
							TotalMemSize:   5 * gb,
						},
					},
				},
				1: &state.NUMANodeState{
					Cells:               []int{0, 1},
					NumberOfAssignments: 4,
					MemoryMap: map[v1.ResourceName]*state.MemoryTable{
						v1.ResourceMemory: {
							Allocatable:    9 * gb,
							Free:           5 * gb,
							Reserved:       4 * gb,
							SystemReserved: 1 * gb,
							TotalMemSize:   10 * gb,
						},
						hugepages1Gi: {
							Allocatable:    5 * gb,
							Free:           4 * gb,
							Reserved:       1 * gb,
							SystemReserved: 0,
							TotalMemSize:   5 * gb,
						},
					},
				},
			},
			expectedMachineState: state.NUMANodeMap{
				0: &state.NUMANodeState{
					Cells:               []int{0},
					NumberOfAssignments: 0,
					MemoryMap: map[v1.ResourceName]*state.MemoryTable{
						v1.ResourceMemory: {
							Allocatable:    9 * gb,
							Free:           9 * gb,
							Reserved:       0 * gb,
							SystemReserved: 1 * gb,
							TotalMemSize:   10 * gb,
						},
						hugepages1Gi: {
							Allocatable:    5 * gb,
							Free:           5 * gb,
							Reserved:       0,
							SystemReserved: 0,
							TotalMemSize:   5 * gb,
						},
					},
				},
				1: &state.NUMANodeState{
					Cells:               []int{1},
					NumberOfAssignments: 0,
					MemoryMap: map[v1.ResourceName]*state.MemoryTable{
						v1.ResourceMemory: {
							Allocatable:    9 * gb,
							Free:           9 * gb,
							Reserved:       0 * gb,
							SystemReserved: 1 * gb,
							TotalMemSize:   10 * gb,
						},
						hugepages1Gi: {
							Allocatable:    5 * gb,
							Free:           5 * gb,
							Reserved:       0,
							SystemReserved: 0,
							TotalMemSize:   5 * gb,
						},
					},
				},
			},
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.description, func(t *testing.T) {
			mgr := &manager{
				policy:       returnPolicyByName(testCase),
				state:        state.NewMemoryState(),
				containerMap: containermap.NewContainerMap(),
				containerRuntime: mockRuntimeService{
					err: nil,
				},
				activePods:        func() []*v1.Pod { return nil },
				podStatusProvider: mockPodStatusProvider{},
			}
			mgr.sourcesReady = &sourcesReadyStub{}
			mgr.state.SetMemoryAssignments(testCase.assignments)
			mgr.state.SetMachineState(testCase.machineState)

			mgr.removeStaleState()

			if !areContainerMemoryAssignmentsEqual(t, mgr.state.GetMemoryAssignments(), testCase.expectedAssignments) {
				t.Errorf("Memory Manager removeStaleState() error, expected assignments %v, but got: %v",
					testCase.expectedAssignments, mgr.state.GetMemoryAssignments())
			}
			if !areMachineStatesEqual(mgr.state.GetMachineState(), testCase.expectedMachineState) {
				t.Fatalf("The actual machine state: %v is different from the expected one: %v", mgr.state.GetMachineState(), testCase.expectedMachineState)
			}
		})

	}
}

func TestAddContainer(t *testing.T) {
	machineInfo := returnMachineInfo()
	reserved := systemReservedMemory{
		0: map[v1.ResourceName]uint64{
			v1.ResourceMemory: 1 * gb,
		},
		1: map[v1.ResourceName]uint64{
			v1.ResourceMemory: 1 * gb,
		},
	}
	pod := getPod("fakePod1", "fakeContainer1", requirementsGuaranteed)
	testCases := []testMemoryManager{
		{
			description: "Correct allocation and adding container on NUMA 0",
			policyName:  policyTypeStatic,
			machineInfo: machineInfo,
			reserved:    reserved,
			machineState: state.NUMANodeMap{
				0: &state.NUMANodeState{
					Cells:               []int{0},
					NumberOfAssignments: 0,
					MemoryMap: map[v1.ResourceName]*state.MemoryTable{
						v1.ResourceMemory: {
							Allocatable:    9 * gb,
							Free:           9 * gb,
							Reserved:       0 * gb,
							SystemReserved: 1 * gb,
							TotalMemSize:   10 * gb,
						},
						hugepages1Gi: {
							Allocatable:    5 * gb,
							Free:           5 * gb,
							Reserved:       0,
							SystemReserved: 0,
							TotalMemSize:   5 * gb,
						},
					},
				},
				1: &state.NUMANodeState{
					Cells:               []int{1},
					NumberOfAssignments: 0,
					MemoryMap: map[v1.ResourceName]*state.MemoryTable{
						v1.ResourceMemory: {
							Allocatable:    9 * gb,
							Free:           9 * gb,
							Reserved:       0 * gb,
							SystemReserved: 1 * gb,
							TotalMemSize:   10 * gb,
						},
						hugepages1Gi: {
							Allocatable:    5 * gb,
							Free:           5 * gb,
							Reserved:       0,
							SystemReserved: 0,
							TotalMemSize:   5 * gb,
						},
					},
				},
			},
			expectedMachineState: state.NUMANodeMap{
				0: &state.NUMANodeState{
					Cells:               []int{0},
					NumberOfAssignments: 2,
					MemoryMap: map[v1.ResourceName]*state.MemoryTable{
						v1.ResourceMemory: {
							Allocatable:    9 * gb,
							Free:           8 * gb,
							Reserved:       1 * gb,
							SystemReserved: 1 * gb,
							TotalMemSize:   10 * gb,
						},
						hugepages1Gi: {
							Allocatable:    5 * gb,
							Free:           4 * gb,
							Reserved:       1 * gb,
							SystemReserved: 0,
							TotalMemSize:   5 * gb,
						},
					},
				},
				1: &state.NUMANodeState{
					Cells:               []int{1},
					NumberOfAssignments: 0,
					MemoryMap: map[v1.ResourceName]*state.MemoryTable{
						v1.ResourceMemory: {
							Allocatable:    9 * gb,
							Free:           9 * gb,
							Reserved:       0 * gb,
							SystemReserved: 1 * gb,
							TotalMemSize:   10 * gb,
						},
						hugepages1Gi: {
							Allocatable:    5 * gb,
							Free:           5 * gb,
							Reserved:       0,
							SystemReserved: 0,
							TotalMemSize:   5 * gb,
						},
					},
				},
			},
			expectedAllocateError:     nil,
			expectedAddContainerError: nil,
			updateError:               nil,
			podAllocate:               pod,
			assignments:               state.ContainerMemoryAssignments{},
			activePods:                nil,
		},
		{
			description:               "Shouldn't return any error when policy is set as None",
			updateError:               nil,
			policyName:                policyTypeNone,
			machineInfo:               machineInfo,
			reserved:                  reserved,
			machineState:              state.NUMANodeMap{},
			expectedMachineState:      state.NUMANodeMap{},
			expectedAllocateError:     nil,
			expectedAddContainerError: nil,
			podAllocate:               pod,
			assignments:               state.ContainerMemoryAssignments{},
			activePods:                nil,
		},
		{
			description: "Allocation should fail if policy returns an error",
			updateError: nil,
			policyName:  policyTypeMock,
			machineInfo: machineInfo,
			reserved:    reserved,
			machineState: state.NUMANodeMap{
				0: &state.NUMANodeState{
					Cells:               []int{0},
					NumberOfAssignments: 0,
					MemoryMap: map[v1.ResourceName]*state.MemoryTable{
						v1.ResourceMemory: {
							Allocatable:    9 * gb,
							Free:           9 * gb,
							Reserved:       0 * gb,
							SystemReserved: 1 * gb,
							TotalMemSize:   10 * gb,
						},
						hugepages1Gi: {
							Allocatable:    5 * gb,
							Free:           5 * gb,
							Reserved:       0,
							SystemReserved: 0,
							TotalMemSize:   5 * gb,
						},
					},
				},
				1: &state.NUMANodeState{
					Cells:               []int{1},
					NumberOfAssignments: 0,
					MemoryMap: map[v1.ResourceName]*state.MemoryTable{
						v1.ResourceMemory: {
							Allocatable:    9 * gb,
							Free:           9 * gb,
							Reserved:       0 * gb,
							SystemReserved: 1 * gb,
							TotalMemSize:   10 * gb,
						},
						hugepages1Gi: {
							Allocatable:    5 * gb,
							Free:           5 * gb,
							Reserved:       0,
							SystemReserved: 0,
							TotalMemSize:   5 * gb,
						},
					},
				},
			},
			expectedMachineState: state.NUMANodeMap{
				0: &state.NUMANodeState{
					Cells:               []int{0},
					NumberOfAssignments: 0,
					MemoryMap: map[v1.ResourceName]*state.MemoryTable{
						v1.ResourceMemory: {
							Allocatable:    9 * gb,
							Free:           9 * gb,
							Reserved:       0 * gb,
							SystemReserved: 1 * gb,
							TotalMemSize:   10 * gb,
						},
						hugepages1Gi: {
							Allocatable:    5 * gb,
							Free:           5 * gb,
							Reserved:       0,
							SystemReserved: 0,
							TotalMemSize:   5 * gb,
						},
					},
				},
				1: &state.NUMANodeState{
					Cells:               []int{1},
					NumberOfAssignments: 0,
					MemoryMap: map[v1.ResourceName]*state.MemoryTable{
						v1.ResourceMemory: {
							Allocatable:    9 * gb,
							Free:           9 * gb,
							Reserved:       0 * gb,
							SystemReserved: 1 * gb,
							TotalMemSize:   10 * gb,
						},
						hugepages1Gi: {
							Allocatable:    5 * gb,
							Free:           5 * gb,
							Reserved:       0,
							SystemReserved: 0,
							TotalMemSize:   5 * gb,
						},
					},
				},
			},
			expectedAllocateError:     fmt.Errorf("fake reg error"),
			expectedAddContainerError: nil,
			podAllocate:               pod,
			assignments:               state.ContainerMemoryAssignments{},
			activePods:                nil,
		},
		{
			description: "Correct allocation of container requiring amount of memory higher than capacity of one NUMA node",
			policyName:  policyTypeStatic,
			machineInfo: machineInfo,
			reserved:    reserved,
			machineState: state.NUMANodeMap{
				0: &state.NUMANodeState{
					Cells:               []int{0},
					NumberOfAssignments: 0,
					MemoryMap: map[v1.ResourceName]*state.MemoryTable{
						v1.ResourceMemory: {
							Allocatable:    9 * gb,
							Free:           9 * gb,
							Reserved:       0 * gb,
							SystemReserved: 1 * gb,
							TotalMemSize:   10 * gb,
						},
						hugepages1Gi: {
							Allocatable:    5 * gb,
							Free:           5 * gb,
							Reserved:       0,
							SystemReserved: 0,
							TotalMemSize:   5 * gb,
						},
					},
				},
				1: &state.NUMANodeState{
					Cells:               []int{1},
					NumberOfAssignments: 0,
					MemoryMap: map[v1.ResourceName]*state.MemoryTable{
						v1.ResourceMemory: {
							Allocatable:    9 * gb,
							Free:           9 * gb,
							Reserved:       0 * gb,
							SystemReserved: 1 * gb,
							TotalMemSize:   10 * gb,
						},
						hugepages1Gi: {
							Allocatable:    5 * gb,
							Free:           5 * gb,
							Reserved:       0,
							SystemReserved: 0,
							TotalMemSize:   5 * gb,
						},
					},
				},
			},
			expectedMachineState: state.NUMANodeMap{
				0: &state.NUMANodeState{
					Cells:               []int{0, 1},
					NumberOfAssignments: 2,
					MemoryMap: map[v1.ResourceName]*state.MemoryTable{
						v1.ResourceMemory: {
							Allocatable:    9 * gb,
							Free:           0 * gb,
							Reserved:       9 * gb,
							SystemReserved: 1 * gb,
							TotalMemSize:   10 * gb,
						},
						hugepages1Gi: {
							Allocatable:    5 * gb,
							Free:           1 * gb,
							Reserved:       4 * gb,
							SystemReserved: 0,
							TotalMemSize:   5 * gb,
						},
					},
				},
				1: &state.NUMANodeState{
					Cells:               []int{0, 1},
					NumberOfAssignments: 2,
					MemoryMap: map[v1.ResourceName]*state.MemoryTable{
						v1.ResourceMemory: {
							Allocatable:    9 * gb,
							Free:           6 * gb,
							Reserved:       3 * gb,
							SystemReserved: 1 * gb,
							TotalMemSize:   10 * gb,
						},
						hugepages1Gi: {
							Allocatable:    5 * gb,
							Free:           5 * gb,
							Reserved:       0,
							SystemReserved: 0,
							TotalMemSize:   5 * gb,
						},
					},
				},
			},
			expectedAllocateError:     nil,
			expectedAddContainerError: nil,
			podAllocate: getPod("fakePod1", "fakeContainer1", &v1.ResourceRequirements{
				Limits: v1.ResourceList{
					v1.ResourceCPU:    resource.MustParse("1000Mi"),
					v1.ResourceMemory: resource.MustParse("12Gi"),
					hugepages1Gi:      resource.MustParse("4Gi"),
				},
				Requests: v1.ResourceList{
					v1.ResourceCPU:    resource.MustParse("1000Mi"),
					v1.ResourceMemory: resource.MustParse("12Gi"),
					hugepages1Gi:      resource.MustParse("4Gi"),
				},
			}),
			assignments: state.ContainerMemoryAssignments{},
			activePods:  nil,
		},
		{
			description: "Should fail if try to allocate container requiring amount of memory higher than capacity of one NUMA node but a small pod is already allocated",
			policyName:  policyTypeStatic,
			machineInfo: machineInfo,
			firstPod:    pod,
			reserved:    reserved,
			machineState: state.NUMANodeMap{
				0: &state.NUMANodeState{
					Cells:               []int{0},
					NumberOfAssignments: 2,
					MemoryMap: map[v1.ResourceName]*state.MemoryTable{
						v1.ResourceMemory: {
							Allocatable:    9 * gb,
							Free:           8 * gb,
							Reserved:       1 * gb,
							SystemReserved: 1 * gb,
							TotalMemSize:   10 * gb,
						},
						hugepages1Gi: {
							Allocatable:    5 * gb,
							Free:           4 * gb,
							Reserved:       1 * gb,
							SystemReserved: 0,
							TotalMemSize:   5 * gb,
						},
					},
				},
				1: &state.NUMANodeState{
					Cells:               []int{1},
					NumberOfAssignments: 0,
					MemoryMap: map[v1.ResourceName]*state.MemoryTable{
						v1.ResourceMemory: {
							Allocatable:    9 * gb,
							Free:           9 * gb,
							Reserved:       0 * gb,
							SystemReserved: 1 * gb,
							TotalMemSize:   10 * gb,
						},
						hugepages1Gi: {
							Allocatable:    5 * gb,
							Free:           5 * gb,
							Reserved:       0,
							SystemReserved: 0,
							TotalMemSize:   5 * gb,
						},
					},
				},
			},
			assignments: state.ContainerMemoryAssignments{
				"fakePod1": map[string][]state.Block{
					"fakeContainer1": {
						{
							NUMAAffinity: []int{0},
							Type:         v1.ResourceMemory,
							Size:         1 * gb,
						},
						{
							NUMAAffinity: []int{0},
							Type:         hugepages1Gi,
							Size:         1 * gb,
						},
					},
				},
			},
			expectedMachineState: state.NUMANodeMap{
				0: &state.NUMANodeState{
					Cells:               []int{0},
					NumberOfAssignments: 2,
					MemoryMap: map[v1.ResourceName]*state.MemoryTable{
						v1.ResourceMemory: {
							Allocatable:    9 * gb,
							Free:           8 * gb,
							Reserved:       1 * gb,
							SystemReserved: 1 * gb,
							TotalMemSize:   10 * gb,
						},
						hugepages1Gi: {
							Allocatable:    5 * gb,
							Free:           4 * gb,
							Reserved:       1 * gb,
							SystemReserved: 0,
							TotalMemSize:   5 * gb,
						},
					},
				},
				1: &state.NUMANodeState{
					Cells:               []int{1},
					NumberOfAssignments: 0,
					MemoryMap: map[v1.ResourceName]*state.MemoryTable{
						v1.ResourceMemory: {
							Allocatable:    9 * gb,
							Free:           9 * gb,
							Reserved:       0 * gb,
							SystemReserved: 1 * gb,
							TotalMemSize:   10 * gb,
						},
						hugepages1Gi: {
							Allocatable:    5 * gb,
							Free:           5 * gb,
							Reserved:       0,
							SystemReserved: 0,
							TotalMemSize:   5 * gb,
						},
					},
				},
			},
			expectedAllocateError:     fmt.Errorf("[memorymanager] failed to get the default NUMA affinity, no NUMA nodes with enough memory is available"),
			expectedAddContainerError: nil,
			podAllocate: getPod("fakePod2", "fakeContainer2", &v1.ResourceRequirements{
				Limits: v1.ResourceList{
					v1.ResourceCPU:    resource.MustParse("1000Mi"),
					v1.ResourceMemory: resource.MustParse("12Gi"),
					hugepages1Gi:      resource.MustParse("4Gi"),
				},
				Requests: v1.ResourceList{
					v1.ResourceCPU:    resource.MustParse("1000Mi"),
					v1.ResourceMemory: resource.MustParse("12Gi"),
					hugepages1Gi:      resource.MustParse("4Gi"),
				},
			}),
			activePods: []*v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						UID: types.UID("fakePod1"),
					},
					Spec: v1.PodSpec{
						Containers: []v1.Container{
							{
								Name:      "fakeContainer1",
								Resources: *requirementsGuaranteed,
							},
						},
					},
				},
			},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.description, func(t *testing.T) {
			mgr := &manager{
				policy:       returnPolicyByName(testCase),
				state:        state.NewMemoryState(),
				containerMap: containermap.NewContainerMap(),
				containerRuntime: mockRuntimeService{
					err: testCase.updateError,
				},
				activePods:        func() []*v1.Pod { return testCase.activePods },
				podStatusProvider: mockPodStatusProvider{},
			}
			mgr.sourcesReady = &sourcesReadyStub{}
			mgr.state.SetMachineState(testCase.machineState)
			mgr.state.SetMemoryAssignments(testCase.assignments)
			if testCase.firstPod != nil {
				mgr.containerMap.Add(testCase.firstPod.Name, testCase.firstPod.Spec.Containers[0].Name, "fakeID0")
			}
			pod := testCase.podAllocate
			container := &pod.Spec.Containers[0]
			err := mgr.Allocate(pod, container)
			if !reflect.DeepEqual(err, testCase.expectedAllocateError) {
				t.Errorf("Memory Manager Allocate() error (%v), expected error: %v, but got: %v",
					testCase.description, testCase.expectedAllocateError, err)
			}
			mgr.AddContainer(pod, container, "fakeID")
			_, _, err = mgr.containerMap.GetContainerRef("fakeID")
			if !reflect.DeepEqual(err, testCase.expectedAddContainerError) {
				t.Errorf("Memory Manager AddContainer() error (%v), expected error: %v, but got: %v",
					testCase.description, testCase.expectedAddContainerError, err)
			}

			if !areMachineStatesEqual(mgr.state.GetMachineState(), testCase.expectedMachineState) {
				t.Errorf("[test] %+v", mgr.state.GetMemoryAssignments())
				t.Fatalf("The actual machine state: %v is different from the expected one: %v", mgr.state.GetMachineState(), testCase.expectedMachineState)
			}

		})
	}
}

func TestRemoveContainer(t *testing.T) {
	machineInfo := returnMachineInfo()
	reserved := systemReservedMemory{
		0: map[v1.ResourceName]uint64{
			v1.ResourceMemory: 1 * gb,
		},
		1: map[v1.ResourceName]uint64{
			v1.ResourceMemory: 1 * gb,
		},
	}
	testCases := []testMemoryManager{
		{
			description:       "Correct removing of a container",
			removeContainerID: "fakeID2",
			policyName:        policyTypeStatic,
			machineInfo:       machineInfo,
			reserved:          reserved,
			assignments: state.ContainerMemoryAssignments{
				"fakePod1": map[string][]state.Block{
					"fakeContainer1": {
						{
							NUMAAffinity: []int{0},
							Type:         v1.ResourceMemory,
							Size:         1 * gb,
						},
						{
							NUMAAffinity: []int{0},
							Type:         hugepages1Gi,
							Size:         1 * gb,
						},
					},
					"fakeContainer2": {
						{
							NUMAAffinity: []int{0},
							Type:         v1.ResourceMemory,
							Size:         1 * gb,
						},
						{
							NUMAAffinity: []int{0},
							Type:         hugepages1Gi,
							Size:         1 * gb,
						},
					},
				},
			},
			expectedAssignments: state.ContainerMemoryAssignments{
				"fakePod1": map[string][]state.Block{
					"fakeContainer1": {
						{
							NUMAAffinity: []int{0},
							Type:         v1.ResourceMemory,
							Size:         1 * gb,
						},
						{
							NUMAAffinity: []int{0},
							Type:         hugepages1Gi,
							Size:         1 * gb,
						},
					},
				},
			},
			machineState: state.NUMANodeMap{
				0: &state.NUMANodeState{
					Cells:               []int{0},
					NumberOfAssignments: 4,
					MemoryMap: map[v1.ResourceName]*state.MemoryTable{
						v1.ResourceMemory: {
							Allocatable:    9 * gb,
							Free:           7 * gb,
							Reserved:       2 * gb,
							SystemReserved: 1 * gb,
							TotalMemSize:   10 * gb,
						},
						hugepages1Gi: {
							Allocatable:    5 * gb,
							Free:           3 * gb,
							Reserved:       2 * gb,
							SystemReserved: 0 * gb,
							TotalMemSize:   5 * gb,
						},
					},
				},
				1: &state.NUMANodeState{
					Cells:               []int{1},
					NumberOfAssignments: 0,
					MemoryMap: map[v1.ResourceName]*state.MemoryTable{
						v1.ResourceMemory: {
							Allocatable:    9 * gb,
							Free:           9 * gb,
							Reserved:       0 * gb,
							SystemReserved: 1 * gb,
							TotalMemSize:   10 * gb,
						},
						hugepages1Gi: {
							Allocatable:    5 * gb,
							Free:           5 * gb,
							Reserved:       0,
							SystemReserved: 0,
							TotalMemSize:   5 * gb,
						},
					},
				},
			},
			expectedMachineState: state.NUMANodeMap{
				0: &state.NUMANodeState{
					Cells:               []int{0},
					NumberOfAssignments: 2,
					MemoryMap: map[v1.ResourceName]*state.MemoryTable{
						v1.ResourceMemory: {
							Allocatable:    9 * gb,
							Free:           8 * gb,
							Reserved:       1 * gb,
							SystemReserved: 1 * gb,
							TotalMemSize:   10 * gb,
						},
						hugepages1Gi: {
							Allocatable:    5 * gb,
							Free:           4 * gb,
							Reserved:       1 * gb,
							SystemReserved: 0,
							TotalMemSize:   5 * gb,
						},
					},
				},
				1: &state.NUMANodeState{
					Cells:               []int{1},
					NumberOfAssignments: 0,
					MemoryMap: map[v1.ResourceName]*state.MemoryTable{
						v1.ResourceMemory: {
							Allocatable:    9 * gb,
							Free:           9 * gb,
							Reserved:       0 * gb,
							SystemReserved: 1 * gb,
							TotalMemSize:   10 * gb,
						},
						hugepages1Gi: {
							Allocatable:    5 * gb,
							Free:           5 * gb,
							Reserved:       0,
							SystemReserved: 0,
							TotalMemSize:   5 * gb,
						},
					},
				},
			},
			expectedError: nil,
		},
		{
			description:       "Correct removing of a multi NUMA container",
			removeContainerID: "fakeID2",
			policyName:        policyTypeStatic,
			machineInfo:       machineInfo,
			reserved:          reserved,
			assignments: state.ContainerMemoryAssignments{
				"fakePod1": map[string][]state.Block{
					"fakeContainer1": {
						{
							NUMAAffinity: []int{0, 1},
							Type:         v1.ResourceMemory,
							Size:         1 * gb,
						},
						{
							NUMAAffinity: []int{0, 1},
							Type:         hugepages1Gi,
							Size:         1 * gb,
						},
					},
					"fakeContainer2": {
						{
							NUMAAffinity: []int{0, 1},
							Type:         v1.ResourceMemory,
							Size:         12 * gb,
						},
						{
							NUMAAffinity: []int{0, 1},
							Type:         hugepages1Gi,
							Size:         1 * gb,
						},
					},
				},
			},
			expectedAssignments: state.ContainerMemoryAssignments{
				"fakePod1": map[string][]state.Block{
					"fakeContainer1": {
						{
							NUMAAffinity: []int{0, 1},
							Type:         v1.ResourceMemory,
							Size:         1 * gb,
						},
						{
							NUMAAffinity: []int{0, 1},
							Type:         hugepages1Gi,
							Size:         1 * gb,
						},
					},
				},
			},
			machineState: state.NUMANodeMap{
				0: &state.NUMANodeState{
					Cells:               []int{0, 1},
					NumberOfAssignments: 4,
					MemoryMap: map[v1.ResourceName]*state.MemoryTable{
						v1.ResourceMemory: {
							Allocatable:    9 * gb,
							Free:           0 * gb,
							Reserved:       9 * gb,
							SystemReserved: 1 * gb,
							TotalMemSize:   10 * gb,
						},
						hugepages1Gi: {
							Allocatable:    5 * gb,
							Free:           3 * gb,
							Reserved:       2 * gb,
							SystemReserved: 0 * gb,
							TotalMemSize:   5 * gb,
						},
					},
				},
				1: &state.NUMANodeState{
					Cells:               []int{0, 1},
					NumberOfAssignments: 4,
					MemoryMap: map[v1.ResourceName]*state.MemoryTable{
						v1.ResourceMemory: {
							Allocatable:    9 * gb,
							Free:           5 * gb,
							Reserved:       4 * gb,
							SystemReserved: 1 * gb,
							TotalMemSize:   10 * gb,
						},
						hugepages1Gi: {
							Allocatable:    5 * gb,
							Free:           5 * gb,
							Reserved:       0,
							SystemReserved: 0,
							TotalMemSize:   5 * gb,
						},
					},
				},
			},
			expectedMachineState: state.NUMANodeMap{
				0: &state.NUMANodeState{
					Cells:               []int{0, 1},
					NumberOfAssignments: 2,
					MemoryMap: map[v1.ResourceName]*state.MemoryTable{
						v1.ResourceMemory: {
							Allocatable:    9 * gb,
							Free:           9 * gb,
							Reserved:       0 * gb,
							SystemReserved: 1 * gb,
							TotalMemSize:   10 * gb,
						},
						hugepages1Gi: {
							Allocatable:    5 * gb,
							Free:           4 * gb,
							Reserved:       1 * gb,
							SystemReserved: 0,
							TotalMemSize:   5 * gb,
						},
					},
				},
				1: &state.NUMANodeState{
					Cells:               []int{0, 1},
					NumberOfAssignments: 2,
					MemoryMap: map[v1.ResourceName]*state.MemoryTable{
						v1.ResourceMemory: {
							Allocatable:    9 * gb,
							Free:           8 * gb,
							Reserved:       1 * gb,
							SystemReserved: 1 * gb,
							TotalMemSize:   10 * gb,
						},
						hugepages1Gi: {
							Allocatable:    5 * gb,
							Free:           5 * gb,
							Reserved:       0,
							SystemReserved: 0,
							TotalMemSize:   5 * gb,
						},
					},
				},
			},
			expectedError: nil,
		},
		{
			description:       "Should do nothing if container is not in containerMap",
			removeContainerID: "fakeID3",
			policyName:        policyTypeStatic,
			machineInfo:       machineInfo,
			reserved:          reserved,
			assignments: state.ContainerMemoryAssignments{
				"fakePod1": map[string][]state.Block{
					"fakeContainer1": {
						{
							NUMAAffinity: []int{0},
							Type:         v1.ResourceMemory,
							Size:         1 * gb,
						},
						{
							NUMAAffinity: []int{0},
							Type:         hugepages1Gi,
							Size:         1 * gb,
						},
					},
					"fakeContainer2": {
						{
							NUMAAffinity: []int{0},
							Type:         v1.ResourceMemory,
							Size:         1 * gb,
						},
						{
							NUMAAffinity: []int{0},
							Type:         hugepages1Gi,
							Size:         1 * gb,
						},
					},
				},
			},
			expectedAssignments: state.ContainerMemoryAssignments{
				"fakePod1": map[string][]state.Block{
					"fakeContainer1": {
						{
							NUMAAffinity: []int{0},
							Type:         v1.ResourceMemory,
							Size:         1 * gb,
						},
						{
							NUMAAffinity: []int{0},
							Type:         hugepages1Gi,
							Size:         1 * gb,
						},
					},
					"fakeContainer2": {
						{
							NUMAAffinity: []int{0},
							Type:         v1.ResourceMemory,
							Size:         1 * gb,
						},
						{
							NUMAAffinity: []int{0},
							Type:         hugepages1Gi,
							Size:         1 * gb,
						},
					},
				},
			},
			machineState: state.NUMANodeMap{
				0: &state.NUMANodeState{
					Cells:               []int{0},
					NumberOfAssignments: 4,
					MemoryMap: map[v1.ResourceName]*state.MemoryTable{
						v1.ResourceMemory: {
							Allocatable:    9 * gb,
							Free:           7 * gb,
							Reserved:       2 * gb,
							SystemReserved: 1 * gb,
							TotalMemSize:   10 * gb,
						},
						hugepages1Gi: {
							Allocatable:    5 * gb,
							Free:           3 * gb,
							Reserved:       2 * gb,
							SystemReserved: 0 * gb,
							TotalMemSize:   5 * gb,
						},
					},
				},
				1: &state.NUMANodeState{
					Cells:               []int{1},
					NumberOfAssignments: 0,
					MemoryMap: map[v1.ResourceName]*state.MemoryTable{
						v1.ResourceMemory: {
							Allocatable:    9 * gb,
							Free:           9 * gb,
							Reserved:       0 * gb,
							SystemReserved: 1 * gb,
							TotalMemSize:   10 * gb,
						},
						hugepages1Gi: {
							Allocatable:    5 * gb,
							Free:           5 * gb,
							Reserved:       0,
							SystemReserved: 0,
							TotalMemSize:   5 * gb,
						},
					},
				},
			},
			expectedMachineState: state.NUMANodeMap{
				0: &state.NUMANodeState{
					Cells:               []int{0},
					NumberOfAssignments: 4,
					MemoryMap: map[v1.ResourceName]*state.MemoryTable{
						v1.ResourceMemory: {
							Allocatable:    9 * gb,
							Free:           7 * gb,
							Reserved:       2 * gb,
							SystemReserved: 1 * gb,
							TotalMemSize:   10 * gb,
						},
						hugepages1Gi: {
							Allocatable:    5 * gb,
							Free:           3 * gb,
							Reserved:       2 * gb,
							SystemReserved: 0 * gb,
							TotalMemSize:   5 * gb,
						},
					},
				},
				1: &state.NUMANodeState{
					Cells:               []int{1},
					NumberOfAssignments: 0,
					MemoryMap: map[v1.ResourceName]*state.MemoryTable{
						v1.ResourceMemory: {
							Allocatable:    9 * gb,
							Free:           9 * gb,
							Reserved:       0 * gb,
							SystemReserved: 1 * gb,
							TotalMemSize:   10 * gb,
						},
						hugepages1Gi: {
							Allocatable:    5 * gb,
							Free:           5 * gb,
							Reserved:       0,
							SystemReserved: 0,
							TotalMemSize:   5 * gb,
						},
					},
				},
			},
			expectedError: nil,
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.description, func(t *testing.T) {
			iniContainerMap := containermap.NewContainerMap()
			iniContainerMap.Add("fakePod1", "fakeContainer1", "fakeID1")
			iniContainerMap.Add("fakePod1", "fakeContainer2", "fakeID2")
			mgr := &manager{
				policy:       returnPolicyByName(testCase),
				state:        state.NewMemoryState(),
				containerMap: iniContainerMap,
				containerRuntime: mockRuntimeService{
					err: testCase.expectedError,
				},
				activePods:        func() []*v1.Pod { return nil },
				podStatusProvider: mockPodStatusProvider{},
			}
			mgr.sourcesReady = &sourcesReadyStub{}
			mgr.state.SetMemoryAssignments(testCase.assignments)
			mgr.state.SetMachineState(testCase.machineState)

			err := mgr.RemoveContainer(testCase.removeContainerID)
			if !reflect.DeepEqual(err, testCase.expectedError) {
				t.Errorf("Memory Manager RemoveContainer() error (%v), expected error: %v, but got: %v",
					testCase.description, testCase.expectedError, err)
			}

			if !areContainerMemoryAssignmentsEqual(t, mgr.state.GetMemoryAssignments(), testCase.expectedAssignments) {
				t.Fatalf("Memory Manager RemoveContainer() inconsistent assignment, expected: %+v, but got: %+v, start %+v",
					testCase.expectedAssignments, mgr.state.GetMemoryAssignments(), testCase.expectedAssignments)
			}

			if !areMachineStatesEqual(mgr.state.GetMachineState(), testCase.expectedMachineState) {
				t.Errorf("[test] %+v", mgr.state.GetMemoryAssignments())
				t.Errorf("[test] %+v, %+v", mgr.state.GetMachineState()[0].MemoryMap["memory"], mgr.state.GetMachineState()[1].MemoryMap["memory"])
				t.Fatalf("The actual machine state: %v is different from the expected one: %v", mgr.state.GetMachineState(), testCase.expectedMachineState)
			}
		})
	}
}

func TestNewManager(t *testing.T) {
	machineInfo := returnMachineInfo()
	expectedReserved := systemReservedMemory{
		0: map[v1.ResourceName]uint64{
			v1.ResourceMemory: 1 * gb,
		},
		1: map[v1.ResourceName]uint64{
			v1.ResourceMemory: 1 * gb,
		},
	}
	testCases := []testMemoryManager{
		{
			description:                "Successful creation of Memory Manager instance",
			policyName:                 policyTypeStatic,
			machineInfo:                machineInfo,
			nodeAllocatableReservation: v1.ResourceList{v1.ResourceMemory: *resource.NewQuantity(2*gb, resource.BinarySI)},
			systemReservedMemory: []kubeletconfig.MemoryReservation{
				{
					NumaNode: 0,
					Limits:   v1.ResourceList{v1.ResourceMemory: *resource.NewQuantity(gb, resource.BinarySI)},
				},
				{
					NumaNode: 1,
					Limits:   v1.ResourceList{v1.ResourceMemory: *resource.NewQuantity(gb, resource.BinarySI)},
				},
			},
			affinity:         topologymanager.NewFakeManager(),
			expectedError:    nil,
			expectedReserved: expectedReserved,
		},
		{
			description:                "Should return an error when systemReservedMemory (configured with kubelet flag) does not comply with Node Allocatable feature values",
			policyName:                 policyTypeStatic,
			machineInfo:                machineInfo,
			nodeAllocatableReservation: v1.ResourceList{v1.ResourceMemory: *resource.NewQuantity(2*gb, resource.BinarySI)},
			systemReservedMemory: []kubeletconfig.MemoryReservation{
				{
					NumaNode: 0,
					Limits: v1.ResourceList{
						v1.ResourceMemory: *resource.NewQuantity(gb, resource.BinarySI),
					},
				},
				{
					NumaNode: 1,
					Limits: v1.ResourceList{
						v1.ResourceMemory: *resource.NewQuantity(2*gb, resource.BinarySI),
					},
				},
			},
			affinity:         topologymanager.NewFakeManager(),
			expectedError:    fmt.Errorf("the total amount \"3Gi\" of type %q is not equal to the value \"2Gi\" determined by Node Allocatable feature", v1.ResourceMemory),
			expectedReserved: expectedReserved,
		},
		{
			description:                "Should return an error when memory reserved for system is empty (systemReservedMemory)",
			policyName:                 policyTypeStatic,
			machineInfo:                machineInfo,
			nodeAllocatableReservation: v1.ResourceList{},
			systemReservedMemory:       []kubeletconfig.MemoryReservation{},
			affinity:                   topologymanager.NewFakeManager(),
			expectedError:              fmt.Errorf("[memorymanager] you should specify the system reserved memory"),
			expectedReserved:           expectedReserved,
		},
		{
			description:                "Should return an error when policy name is not correct",
			policyName:                 "fake",
			machineInfo:                machineInfo,
			nodeAllocatableReservation: v1.ResourceList{},
			systemReservedMemory:       []kubeletconfig.MemoryReservation{},
			affinity:                   topologymanager.NewFakeManager(),
			expectedError:              fmt.Errorf("unknown policy: \"fake\""),
			expectedReserved:           expectedReserved,
		},
		{
			description:                "Should create manager with \"none\" policy",
			policyName:                 policyTypeNone,
			machineInfo:                machineInfo,
			nodeAllocatableReservation: v1.ResourceList{},
			systemReservedMemory:       []kubeletconfig.MemoryReservation{},
			affinity:                   topologymanager.NewFakeManager(),
			expectedError:              nil,
			expectedReserved:           expectedReserved,
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.description, func(t *testing.T) {
			stateFileDirectory, err := os.MkdirTemp("", "memory_manager_tests")
			if err != nil {
				t.Errorf("Cannot create state file: %s", err.Error())
			}
			defer os.RemoveAll(stateFileDirectory)

			mgr, err := NewManager(string(testCase.policyName), &testCase.machineInfo, testCase.nodeAllocatableReservation, testCase.systemReservedMemory, stateFileDirectory, testCase.affinity)

			if !reflect.DeepEqual(err, testCase.expectedError) {
				t.Errorf("Could not create the Memory Manager. Expected error: '%v', but got: '%v'",
					testCase.expectedError, err)
			}

			if testCase.expectedError == nil {
				if mgr != nil {
					rawMgr := mgr.(*manager)
					if !reflect.DeepEqual(rawMgr.policy.Name(), string(testCase.policyName)) {
						t.Errorf("Could not create the Memory Manager. Expected policy name: %v, but got: %v",
							testCase.policyName, rawMgr.policy.Name())
					}
					if testCase.policyName == policyTypeStatic {
						if !reflect.DeepEqual(rawMgr.policy.(*staticPolicy).systemReserved, testCase.expectedReserved) {
							t.Errorf("Could not create the Memory Manager. Expected system reserved: %+v, but got: %+v",
								testCase.expectedReserved, rawMgr.policy.(*staticPolicy).systemReserved)
						}
					}
				} else {
					t.Errorf("Could not create the Memory Manager - manager is nil, but it should not be.")
				}

			}
		})
	}
}

func TestGetTopologyHints(t *testing.T) {
	testCases := []testMemoryManager{
		{
			description: "Successful hint generation",
			policyName:  policyTypeStatic,
			machineInfo: returnMachineInfo(),
			reserved: systemReservedMemory{
				0: map[v1.ResourceName]uint64{
					v1.ResourceMemory: 1 * gb,
				},
				1: map[v1.ResourceName]uint64{
					v1.ResourceMemory: 1 * gb,
				},
			},
			assignments: state.ContainerMemoryAssignments{
				"fakePod1": map[string][]state.Block{
					"fakeContainer1": {
						{
							NUMAAffinity: []int{0},
							Type:         v1.ResourceMemory,
							Size:         1 * gb,
						},
						{
							NUMAAffinity: []int{0},
							Type:         hugepages1Gi,
							Size:         1 * gb,
						},
					},
					"fakeContainer2": {
						{
							NUMAAffinity: []int{0},
							Type:         v1.ResourceMemory,
							Size:         1 * gb,
						},
						{
							NUMAAffinity: []int{0},
							Type:         hugepages1Gi,
							Size:         1 * gb,
						},
					},
				},
			},
			machineState: state.NUMANodeMap{
				0: &state.NUMANodeState{
					Cells:               []int{0},
					NumberOfAssignments: 4,
					MemoryMap: map[v1.ResourceName]*state.MemoryTable{
						v1.ResourceMemory: {
							Allocatable:    9 * gb,
							Free:           7 * gb,
							Reserved:       2 * gb,
							SystemReserved: 1 * gb,
							TotalMemSize:   10 * gb,
						},
						hugepages1Gi: {
							Allocatable:    5 * gb,
							Free:           3 * gb,
							Reserved:       2 * gb,
							SystemReserved: 0 * gb,
							TotalMemSize:   5 * gb,
						},
					},
				},
				1: &state.NUMANodeState{
					Cells:               []int{1},
					NumberOfAssignments: 0,
					MemoryMap: map[v1.ResourceName]*state.MemoryTable{
						v1.ResourceMemory: {
							Allocatable:    9 * gb,
							Free:           9 * gb,
							Reserved:       0 * gb,
							SystemReserved: 1 * gb,
							TotalMemSize:   10 * gb,
						},
						hugepages1Gi: {
							Allocatable:    5 * gb,
							Free:           5 * gb,
							Reserved:       0,
							SystemReserved: 0,
							TotalMemSize:   5 * gb,
						},
					},
				},
			},
			expectedError: nil,
			expectedHints: map[string][]topologymanager.TopologyHint{
				string(v1.ResourceMemory): {
					{
						NUMANodeAffinity: newNUMAAffinity(0),
						Preferred:        true,
					},
					{
						NUMANodeAffinity: newNUMAAffinity(1),
						Preferred:        true,
					},
					{
						NUMANodeAffinity: newNUMAAffinity(0, 1),
						Preferred:        false,
					},
				},
				string(hugepages1Gi): {
					{
						NUMANodeAffinity: newNUMAAffinity(0),
						Preferred:        true,
					},
					{
						NUMANodeAffinity: newNUMAAffinity(1),
						Preferred:        true,
					},
					{
						NUMANodeAffinity: newNUMAAffinity(0, 1),
						Preferred:        false,
					},
				},
			},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.description, func(t *testing.T) {
			mgr := &manager{
				policy:       returnPolicyByName(testCase),
				state:        state.NewMemoryState(),
				containerMap: containermap.NewContainerMap(),
				containerRuntime: mockRuntimeService{
					err: nil,
				},
				activePods:        func() []*v1.Pod { return nil },
				podStatusProvider: mockPodStatusProvider{},
			}
			mgr.sourcesReady = &sourcesReadyStub{}
			mgr.state.SetMachineState(testCase.machineState.Clone())
			mgr.state.SetMemoryAssignments(testCase.assignments.Clone())

			pod := getPod("fakePod1", "fakeContainer1", requirementsGuaranteed)
			container := &pod.Spec.Containers[0]
			hints := mgr.GetTopologyHints(pod, container)
			if !reflect.DeepEqual(hints, testCase.expectedHints) {
				t.Errorf("Hints were not generated correctly. Hints generated: %+v, hints expected: %+v",
					hints, testCase.expectedHints)
			}
		})
	}
}

func TestAllocateAndAddPodWithInitContainers(t *testing.T) {
	testCases := []testMemoryManager{
		{
			description: "should remove init containers from the state file, once app container started",
			policyName:  policyTypeStatic,
			machineInfo: returnMachineInfo(),
			assignments: state.ContainerMemoryAssignments{},
			expectedAssignments: state.ContainerMemoryAssignments{
				"pod1": map[string][]state.Block{
					"container1": {
						{
							NUMAAffinity: []int{0},
							Type:         v1.ResourceMemory,
							Size:         4 * gb,
						},
						{
							NUMAAffinity: []int{0},
							Type:         hugepages1Gi,
							Size:         4 * gb,
						},
					},
				},
			},
			machineState: state.NUMANodeMap{
				0: &state.NUMANodeState{
					MemoryMap: map[v1.ResourceName]*state.MemoryTable{
						v1.ResourceMemory: {
							Allocatable:    9728 * mb,
							Free:           9728 * mb,
							Reserved:       0,
							SystemReserved: 512 * mb,
							TotalMemSize:   10 * gb,
						},
						hugepages1Gi: {
							Allocatable:    5 * gb,
							Free:           5 * gb,
							Reserved:       0,
							SystemReserved: 0,
							TotalMemSize:   5 * gb,
						},
					},
					Cells: []int{0},
				},
				1: &state.NUMANodeState{
					MemoryMap: map[v1.ResourceName]*state.MemoryTable{
						v1.ResourceMemory: {
							Allocatable:    9728 * mb,
							Free:           9728 * mb,
							Reserved:       0,
							SystemReserved: 512 * mb,
							TotalMemSize:   10 * gb,
						},
						hugepages1Gi: {
							Allocatable:    5 * gb,
							Free:           5 * gb,
							Reserved:       0,
							SystemReserved: 0,
							TotalMemSize:   5 * gb,
						},
					},
					Cells: []int{1},
				},
			},
			expectedMachineState: state.NUMANodeMap{
				0: &state.NUMANodeState{
					MemoryMap: map[v1.ResourceName]*state.MemoryTable{
						v1.ResourceMemory: {
							Allocatable:    9728 * mb,
							Free:           5632 * mb,
							Reserved:       4 * gb,
							SystemReserved: 512 * mb,
							TotalMemSize:   10 * gb,
						},
						hugepages1Gi: {
							Allocatable:    5 * gb,
							Free:           gb,
							Reserved:       4 * gb,
							SystemReserved: 0,
							TotalMemSize:   5 * gb,
						},
					},
					Cells:               []int{0},
					NumberOfAssignments: 2,
				},
				1: &state.NUMANodeState{
					MemoryMap: map[v1.ResourceName]*state.MemoryTable{
						v1.ResourceMemory: {
							Allocatable:    9728 * mb,
							Free:           9728 * mb,
							Reserved:       0,
							SystemReserved: 512 * mb,
							TotalMemSize:   10 * gb,
						},
						hugepages1Gi: {
							Allocatable:    5 * gb,
							Free:           5 * gb,
							Reserved:       0,
							SystemReserved: 0,
							TotalMemSize:   5 * gb,
						},
					},
					Cells: []int{1},
				},
			},
			reserved: systemReservedMemory{
				0: map[v1.ResourceName]uint64{
					v1.ResourceMemory: 512 * mb,
				},
			},
			podAllocate: getPodWithInitContainers(
				"pod1",
				[]v1.Container{
					{
						Name: "container1",
						Resources: v1.ResourceRequirements{
							Limits: v1.ResourceList{
								v1.ResourceCPU:    resource.MustParse("1000Mi"),
								v1.ResourceMemory: resource.MustParse("4Gi"),
								hugepages1Gi:      resource.MustParse("4Gi"),
							},
							Requests: v1.ResourceList{
								v1.ResourceCPU:    resource.MustParse("1000Mi"),
								v1.ResourceMemory: resource.MustParse("4Gi"),
								hugepages1Gi:      resource.MustParse("4Gi"),
							},
						},
					},
				},
				[]v1.Container{
					{
						Name: "initContainer1",
						Resources: v1.ResourceRequirements{
							Limits: v1.ResourceList{
								v1.ResourceCPU:    resource.MustParse("1000Mi"),
								v1.ResourceMemory: resource.MustParse("7Gi"),
								hugepages1Gi:      resource.MustParse("5Gi"),
							},
							Requests: v1.ResourceList{
								v1.ResourceCPU:    resource.MustParse("1000Mi"),
								v1.ResourceMemory: resource.MustParse("7Gi"),
								hugepages1Gi:      resource.MustParse("5Gi"),
							},
						},
					},
				},
			),
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.description, func(t *testing.T) {
			klog.InfoS("TestAllocateAndAddPodWithInitContainers", "name", testCase.description)
			mgr := &manager{
				policy:       returnPolicyByName(testCase),
				state:        state.NewMemoryState(),
				containerMap: containermap.NewContainerMap(),
				containerRuntime: mockRuntimeService{
					err: nil,
				},
				activePods:        func() []*v1.Pod { return []*v1.Pod{testCase.podAllocate} },
				podStatusProvider: mockPodStatusProvider{},
			}
			mgr.sourcesReady = &sourcesReadyStub{}
			mgr.state.SetMachineState(testCase.machineState.Clone())
			mgr.state.SetMemoryAssignments(testCase.assignments.Clone())

			// Allocates memory for init containers
			for i := range testCase.podAllocate.Spec.InitContainers {
				err := mgr.Allocate(testCase.podAllocate, &testCase.podAllocate.Spec.InitContainers[i])
				if !reflect.DeepEqual(err, testCase.expectedError) {
					t.Fatalf("The actual error %v is different from the expected one %v", err, testCase.expectedError)
				}
			}

			// Allocates memory for apps containers
			for i := range testCase.podAllocate.Spec.Containers {
				err := mgr.Allocate(testCase.podAllocate, &testCase.podAllocate.Spec.Containers[i])
				if !reflect.DeepEqual(err, testCase.expectedError) {
					t.Fatalf("The actual error %v is different from the expected one %v", err, testCase.expectedError)
				}
			}

			// Calls AddContainer for init containers
			for i, initContainer := range testCase.podAllocate.Spec.InitContainers {
				mgr.AddContainer(testCase.podAllocate, &testCase.podAllocate.Spec.InitContainers[i], initContainer.Name)
			}

			// Calls AddContainer for apps containers
			for i, appContainer := range testCase.podAllocate.Spec.Containers {
				mgr.AddContainer(testCase.podAllocate, &testCase.podAllocate.Spec.Containers[i], appContainer.Name)
			}

			assignments := mgr.state.GetMemoryAssignments()
			if !areContainerMemoryAssignmentsEqual(t, assignments, testCase.expectedAssignments) {
				t.Fatalf("Actual assignments %v are different from the expected %v", assignments, testCase.expectedAssignments)
			}

			machineState := mgr.state.GetMachineState()
			if !areMachineStatesEqual(machineState, testCase.expectedMachineState) {
				t.Fatalf("The actual machine state %v is different from the expected %v", machineState, testCase.expectedMachineState)
			}
		})
	}
}

func returnMachineInfo() cadvisorapi.MachineInfo {
	return cadvisorapi.MachineInfo{
		Topology: []cadvisorapi.Node{
			{
				Id:     0,
				Memory: 10 * gb,
				HugePages: []cadvisorapi.HugePagesInfo{
					{
						PageSize: pageSize1Gb,
						NumPages: 5,
					},
				},
			},
			{
				Id:     1,
				Memory: 10 * gb,
				HugePages: []cadvisorapi.HugePagesInfo{
					{
						PageSize: pageSize1Gb,
						NumPages: 5,
					},
				},
			},
		},
	}
}
