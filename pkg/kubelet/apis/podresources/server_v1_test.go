/*
Copyright 2018 The Kubernetes Authors.

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

package podresources

import (
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	podresourcesapi "k8s.io/kubelet/pkg/apis/podresources/v1"
	pkgfeatures "k8s.io/kubernetes/pkg/features"
	podresourcetest "k8s.io/kubernetes/pkg/kubelet/apis/podresources/testing"
	"k8s.io/kubernetes/test/utils/ktesting"
)

func TestListPodResourcesV1(t *testing.T) {
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, pkgfeatures.KubeletPodResourcesDynamicResources, true)

	tCtx := ktesting.Init(t)
	podName := "pod-name"
	podNamespace := "pod-namespace"
	podUID := types.UID("pod-uid")
	containerName := "container-name"
	numaID := int64(1)

	devs := []*podresourcesapi.ContainerDevices{
		{
			ResourceName: "resource",
			DeviceIds:    []string{"dev0", "dev1"},
			Topology:     &podresourcesapi.TopologyInfo{Nodes: []*podresourcesapi.NUMANode{{ID: numaID}}},
		},
	}

	cpus := []int64{12, 23, 30}

	memory := []*podresourcesapi.ContainerMemory{
		{
			MemoryType: "memory",
			Size_:      1073741824,
			Topology:   &podresourcesapi.TopologyInfo{Nodes: []*podresourcesapi.NUMANode{{ID: numaID}}},
		},
		{
			MemoryType: "hugepages-1Gi",
			Size_:      1073741824,
			Topology:   &podresourcesapi.TopologyInfo{Nodes: []*podresourcesapi.NUMANode{{ID: numaID}}},
		},
	}

	containers := []v1.Container{
		{
			Name: containerName,
		},
	}
	pods := []*v1.Pod{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      podName,
				Namespace: podNamespace,
				UID:       podUID,
			},
			Spec: v1.PodSpec{
				Containers: containers,
			},
		},
	}

	pluginCDIDevices := []*podresourcesapi.CDIDevice{{Name: "dra-dev0"}, {Name: "dra-dev1"}}
	draDriverName := "dra.example.com"
	poolName := "worker-1-pool"
	deviceName := "gpu-1"
	draDevs := []*podresourcesapi.DynamicResource{
		{
			ClaimName:      "claim-name",
			ClaimNamespace: "default",
			ClaimResources: []*podresourcesapi.ClaimResource{{CDIDevices: pluginCDIDevices, DriverName: draDriverName, PoolName: poolName, DeviceName: deviceName}},
		},
	}

	for _, tc := range []struct {
		desc             string
		pods             []*v1.Pod
		devices          []*podresourcesapi.ContainerDevices
		cpus             []int64
		memory           []*podresourcesapi.ContainerMemory
		dynamicResources []*podresourcesapi.DynamicResource
		expectedResponse *podresourcesapi.ListPodResourcesResponse
	}{
		{
			desc:             "no pods",
			pods:             []*v1.Pod{},
			devices:          []*podresourcesapi.ContainerDevices{},
			cpus:             []int64{},
			memory:           []*podresourcesapi.ContainerMemory{},
			dynamicResources: []*podresourcesapi.DynamicResource{},
			expectedResponse: &podresourcesapi.ListPodResourcesResponse{},
		},
		{
			desc:             "pod without devices",
			pods:             pods,
			devices:          []*podresourcesapi.ContainerDevices{},
			cpus:             []int64{},
			memory:           []*podresourcesapi.ContainerMemory{},
			dynamicResources: []*podresourcesapi.DynamicResource{},
			expectedResponse: &podresourcesapi.ListPodResourcesResponse{
				PodResources: []*podresourcesapi.PodResources{
					{
						Name:      podName,
						Namespace: podNamespace,
						Containers: []*podresourcesapi.ContainerResources{
							{
								Name:             containerName,
								Devices:          []*podresourcesapi.ContainerDevices{},
								DynamicResources: []*podresourcesapi.DynamicResource{},
							},
						},
					},
				},
			},
		},
		{
			desc:             "pod with devices",
			pods:             pods,
			devices:          devs,
			cpus:             cpus,
			memory:           memory,
			dynamicResources: []*podresourcesapi.DynamicResource{},
			expectedResponse: &podresourcesapi.ListPodResourcesResponse{
				PodResources: []*podresourcesapi.PodResources{
					{
						Name:      podName,
						Namespace: podNamespace,
						Containers: []*podresourcesapi.ContainerResources{
							{
								Name:             containerName,
								Devices:          devs,
								CpuIds:           cpus,
								Memory:           memory,
								DynamicResources: []*podresourcesapi.DynamicResource{},
							},
						},
					},
				},
			},
		},
		{
			desc:             "pod with dynamic resources",
			pods:             pods,
			devices:          []*podresourcesapi.ContainerDevices{},
			cpus:             cpus,
			memory:           memory,
			dynamicResources: draDevs,
			expectedResponse: &podresourcesapi.ListPodResourcesResponse{
				PodResources: []*podresourcesapi.PodResources{
					{
						Name:      podName,
						Namespace: podNamespace,
						Containers: []*podresourcesapi.ContainerResources{
							{
								Name:             containerName,
								Devices:          []*podresourcesapi.ContainerDevices{},
								CpuIds:           cpus,
								Memory:           memory,
								DynamicResources: draDevs,
							},
						},
					},
				},
			},
		},
		{
			desc:             "pod with dynamic resources and devices",
			pods:             pods,
			devices:          devs,
			cpus:             cpus,
			memory:           memory,
			dynamicResources: draDevs,
			expectedResponse: &podresourcesapi.ListPodResourcesResponse{
				PodResources: []*podresourcesapi.PodResources{
					{
						Name:      podName,
						Namespace: podNamespace,
						Containers: []*podresourcesapi.ContainerResources{
							{
								Name:             containerName,
								Devices:          devs,
								CpuIds:           cpus,
								Memory:           memory,
								DynamicResources: draDevs,
							},
						},
					},
				},
			},
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			mockDevicesProvider := podresourcetest.NewMockDevicesProvider(t)
			mockPodsProvider := podresourcetest.NewMockPodsProvider(t)
			mockCPUsProvider := podresourcetest.NewMockCPUsProvider(t)
			mockMemoryProvider := podresourcetest.NewMockMemoryProvider(t)
			mockDynamicResourcesProvider := podresourcetest.NewMockDynamicResourcesProvider(t)

			mockPodsProvider.EXPECT().GetPods().Return(tc.pods).Maybe()
			mockDevicesProvider.EXPECT().GetDevices(string(podUID), containerName).Return(tc.devices).Maybe()
			mockCPUsProvider.EXPECT().GetCPUs(string(podUID), containerName).Return(tc.cpus).Maybe()
			mockMemoryProvider.EXPECT().GetMemory(string(podUID), containerName).Return(tc.memory).Maybe()
			mockDynamicResourcesProvider.EXPECT().GetDynamicResources(pods[0], &containers[0]).Return(tc.dynamicResources).Maybe()
			mockDevicesProvider.EXPECT().UpdateAllocatedDevices().Return().Maybe()
			mockCPUsProvider.EXPECT().GetAllocatableCPUs().Return([]int64{}).Maybe()
			mockDevicesProvider.EXPECT().GetAllocatableDevices().Return([]*podresourcesapi.ContainerDevices{}).Maybe()
			mockMemoryProvider.EXPECT().GetAllocatableMemory().Return([]*podresourcesapi.ContainerMemory{}).Maybe()

			providers := PodResourcesProviders{
				Pods:             mockPodsProvider,
				Devices:          mockDevicesProvider,
				Cpus:             mockCPUsProvider,
				Memory:           mockMemoryProvider,
				DynamicResources: mockDynamicResourcesProvider,
			}
			server := NewV1PodResourcesServer(providers)
			resp, err := server.List(tCtx, &podresourcesapi.ListPodResourcesRequest{})
			if err != nil {
				t.Errorf("want err = %v, got %q", nil, err)
			}
			if diff := cmp.Diff(tc.expectedResponse, resp, cmpopts.EquateEmpty()); diff != "" {
				t.Fatal(diff)
			}
		})
	}
}

func TestListPodResourcesWithInitContainersV1(t *testing.T) {
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, pkgfeatures.KubeletPodResourcesDynamicResources, true)
	tCtx := ktesting.Init(t)
	podName := "pod-name"
	podNamespace := "pod-namespace"
	podUID := types.UID("pod-uid")
	initContainerName := "init-container-name"
	containerName := "container-name"
	numaID := int64(1)
	containerRestartPolicyAlways := v1.ContainerRestartPolicyAlways

	devs := []*podresourcesapi.ContainerDevices{
		{
			ResourceName: "resource",
			DeviceIds:    []string{"dev0", "dev1"},
			Topology:     &podresourcesapi.TopologyInfo{Nodes: []*podresourcesapi.NUMANode{{ID: numaID}}},
		},
	}

	cpus := []int64{12, 23, 30}

	memory := []*podresourcesapi.ContainerMemory{
		{
			MemoryType: "memory",
			Size_:      1073741824,
			Topology:   &podresourcesapi.TopologyInfo{Nodes: []*podresourcesapi.NUMANode{{ID: numaID}}},
		},
		{
			MemoryType: "hugepages-1Gi",
			Size_:      1073741824,
			Topology:   &podresourcesapi.TopologyInfo{Nodes: []*podresourcesapi.NUMANode{{ID: numaID}}},
		},
	}

	containers := []v1.Container{
		{
			Name: containerName,
		},
	}

	for _, tc := range []struct {
		desc     string
		pods     []*v1.Pod
		mockFunc func(
			[]*v1.Pod,
			*podresourcetest.MockDevicesProvider,
			*podresourcetest.MockCPUsProvider,
			*podresourcetest.MockMemoryProvider,
			*podresourcetest.MockDynamicResourcesProvider)
		sidecarContainersEnabled bool
		expectedResponse         *podresourcesapi.ListPodResourcesResponse
	}{
		{
			desc: "pod having an init container",
			pods: []*v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      podName,
						Namespace: podNamespace,
						UID:       podUID,
					},
					Spec: v1.PodSpec{
						InitContainers: []v1.Container{
							{
								Name: initContainerName,
							},
						},
						Containers: containers,
					},
				},
			},
			mockFunc: func(
				pods []*v1.Pod,
				devicesProvider *podresourcetest.MockDevicesProvider,
				cpusProvider *podresourcetest.MockCPUsProvider,
				memoryProvider *podresourcetest.MockMemoryProvider,
				dynamicResourcesProvider *podresourcetest.MockDynamicResourcesProvider) {
				devicesProvider.EXPECT().UpdateAllocatedDevices().Return().Maybe()
				devicesProvider.EXPECT().GetDevices(string(podUID), containerName).Return(devs).Maybe()
				cpusProvider.EXPECT().GetCPUs(string(podUID), containerName).Return(cpus).Maybe()
				memoryProvider.EXPECT().GetMemory(string(podUID), containerName).Return(memory).Maybe()
				dynamicResourcesProvider.EXPECT().GetDynamicResources(pods[0], &pods[0].Spec.Containers[0]).Return([]*podresourcesapi.DynamicResource{}).Maybe()

			},
			expectedResponse: &podresourcesapi.ListPodResourcesResponse{
				PodResources: []*podresourcesapi.PodResources{
					{
						Name:      podName,
						Namespace: podNamespace,
						Containers: []*podresourcesapi.ContainerResources{
							{
								Name:             containerName,
								Devices:          devs,
								CpuIds:           cpus,
								Memory:           memory,
								DynamicResources: []*podresourcesapi.DynamicResource{},
							},
						},
					},
				},
			},
		},
		{
			desc: "pod having an init container with SidecarContainers enabled",
			pods: []*v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      podName,
						Namespace: podNamespace,
						UID:       podUID,
					},
					Spec: v1.PodSpec{
						InitContainers: []v1.Container{
							{
								Name: initContainerName,
							},
						},
						Containers: containers,
					},
				},
			},
			mockFunc: func(
				pods []*v1.Pod,
				devicesProvider *podresourcetest.MockDevicesProvider,
				cpusProvider *podresourcetest.MockCPUsProvider,
				memoryProvider *podresourcetest.MockMemoryProvider,
				dynamicResourcesProvider *podresourcetest.MockDynamicResourcesProvider) {
				devicesProvider.EXPECT().UpdateAllocatedDevices().Return().Maybe()
				devicesProvider.EXPECT().GetDevices(string(podUID), containerName).Return(devs).Maybe()
				cpusProvider.EXPECT().GetCPUs(string(podUID), containerName).Return(cpus).Maybe()
				memoryProvider.EXPECT().GetMemory(string(podUID), containerName).Return(memory).Maybe()
				dynamicResourcesProvider.EXPECT().GetDynamicResources(pods[0], &pods[0].Spec.Containers[0]).Return([]*podresourcesapi.DynamicResource{}).Maybe()

			},
			sidecarContainersEnabled: true,
			expectedResponse: &podresourcesapi.ListPodResourcesResponse{
				PodResources: []*podresourcesapi.PodResources{
					{
						Name:      podName,
						Namespace: podNamespace,
						Containers: []*podresourcesapi.ContainerResources{
							{
								Name:             containerName,
								Devices:          devs,
								CpuIds:           cpus,
								Memory:           memory,
								DynamicResources: []*podresourcesapi.DynamicResource{},
							},
						},
					},
				},
			},
		},
		{
			desc: "pod having a restartable init container with SidecarContainers disabled",
			pods: []*v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      podName,
						Namespace: podNamespace,
						UID:       podUID,
					},
					Spec: v1.PodSpec{
						InitContainers: []v1.Container{
							{
								Name:          initContainerName,
								RestartPolicy: &containerRestartPolicyAlways,
							},
						},
						Containers: containers,
					},
				},
			},
			mockFunc: func(
				pods []*v1.Pod,
				devicesProvider *podresourcetest.MockDevicesProvider,
				cpusProvider *podresourcetest.MockCPUsProvider,
				memoryProvider *podresourcetest.MockMemoryProvider,
				dynamicResourcesProvider *podresourcetest.MockDynamicResourcesProvider) {
				devicesProvider.EXPECT().UpdateAllocatedDevices().Return().Maybe()

				devicesProvider.EXPECT().GetDevices(string(podUID), containerName).Return(devs).Maybe()
				cpusProvider.EXPECT().GetCPUs(string(podUID), containerName).Return(cpus).Maybe()
				memoryProvider.EXPECT().GetMemory(string(podUID), containerName).Return(memory).Maybe()
				dynamicResourcesProvider.EXPECT().GetDynamicResources(pods[0], &pods[0].Spec.Containers[0]).Return([]*podresourcesapi.DynamicResource{}).Maybe()

			},
			expectedResponse: &podresourcesapi.ListPodResourcesResponse{
				PodResources: []*podresourcesapi.PodResources{
					{
						Name:      podName,
						Namespace: podNamespace,
						Containers: []*podresourcesapi.ContainerResources{
							{
								Name:             containerName,
								Devices:          devs,
								CpuIds:           cpus,
								Memory:           memory,
								DynamicResources: []*podresourcesapi.DynamicResource{},
							},
						},
					},
				},
			},
		},
		{
			desc: "pod having an init container with SidecarContainers enabled",
			pods: []*v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      podName,
						Namespace: podNamespace,
						UID:       podUID,
					},
					Spec: v1.PodSpec{
						InitContainers: []v1.Container{
							{
								Name:          initContainerName,
								RestartPolicy: &containerRestartPolicyAlways,
							},
						},
						Containers: containers,
					},
				},
			},
			mockFunc: func(
				pods []*v1.Pod,
				devicesProvider *podresourcetest.MockDevicesProvider,
				cpusProvider *podresourcetest.MockCPUsProvider,
				memoryProvider *podresourcetest.MockMemoryProvider,
				dynamicResourcesProvider *podresourcetest.MockDynamicResourcesProvider) {
				devicesProvider.EXPECT().UpdateAllocatedDevices().Return().Maybe()

				devicesProvider.EXPECT().GetDevices(string(podUID), initContainerName).Return(devs).Maybe()
				cpusProvider.EXPECT().GetCPUs(string(podUID), initContainerName).Return(cpus).Maybe()
				memoryProvider.EXPECT().GetMemory(string(podUID), initContainerName).Return(memory).Maybe()
				dynamicResourcesProvider.EXPECT().GetDynamicResources(pods[0], &pods[0].Spec.InitContainers[0]).Return([]*podresourcesapi.DynamicResource{}).Maybe()

				devicesProvider.EXPECT().GetDevices(string(podUID), containerName).Return(devs).Maybe()
				cpusProvider.EXPECT().GetCPUs(string(podUID), containerName).Return(cpus).Maybe()
				memoryProvider.EXPECT().GetMemory(string(podUID), containerName).Return(memory).Maybe()
				dynamicResourcesProvider.EXPECT().GetDynamicResources(pods[0], &pods[0].Spec.Containers[0]).Return([]*podresourcesapi.DynamicResource{}).Maybe()

			},
			sidecarContainersEnabled: true,
			expectedResponse: &podresourcesapi.ListPodResourcesResponse{
				PodResources: []*podresourcesapi.PodResources{
					{
						Name:      podName,
						Namespace: podNamespace,
						Containers: []*podresourcesapi.ContainerResources{
							{
								Name:             initContainerName,
								Devices:          devs,
								CpuIds:           cpus,
								Memory:           memory,
								DynamicResources: []*podresourcesapi.DynamicResource{},
							},
							{
								Name:             containerName,
								Devices:          devs,
								CpuIds:           cpus,
								Memory:           memory,
								DynamicResources: []*podresourcesapi.DynamicResource{},
							},
						},
					},
				},
			},
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, pkgfeatures.SidecarContainers, tc.sidecarContainersEnabled)

			mockDevicesProvider := podresourcetest.NewMockDevicesProvider(t)
			mockPodsProvider := podresourcetest.NewMockPodsProvider(t)
			mockCPUsProvider := podresourcetest.NewMockCPUsProvider(t)
			mockMemoryProvider := podresourcetest.NewMockMemoryProvider(t)
			mockDynamicResourcesProvider := podresourcetest.NewMockDynamicResourcesProvider(t)

			mockPodsProvider.EXPECT().GetPods().Return(tc.pods).Maybe()
			tc.mockFunc(tc.pods, mockDevicesProvider, mockCPUsProvider, mockMemoryProvider, mockDynamicResourcesProvider)

			providers := PodResourcesProviders{
				Pods:             mockPodsProvider,
				Devices:          mockDevicesProvider,
				Cpus:             mockCPUsProvider,
				Memory:           mockMemoryProvider,
				DynamicResources: mockDynamicResourcesProvider,
			}
			server := NewV1PodResourcesServer(providers)
			resp, err := server.List(tCtx, &podresourcesapi.ListPodResourcesRequest{})
			if err != nil {
				t.Errorf("want err = %v, got %q", nil, err)
			}
			if diff := cmp.Diff(tc.expectedResponse, resp, cmpopts.EquateEmpty()); diff != "" {
				t.Fatal(diff)
			}
		})
	}
}

func TestAllocatableResources(t *testing.T) {
	tCtx := ktesting.Init(t)
	allDevs := []*podresourcesapi.ContainerDevices{
		{
			ResourceName: "resource",
			DeviceIds:    []string{"dev0"},
			Topology: &podresourcesapi.TopologyInfo{
				Nodes: []*podresourcesapi.NUMANode{
					{
						ID: 0,
					},
				},
			},
		},
		{
			ResourceName: "resource",
			DeviceIds:    []string{"dev1"},
			Topology: &podresourcesapi.TopologyInfo{
				Nodes: []*podresourcesapi.NUMANode{
					{
						ID: 1,
					},
				},
			},
		},
		{
			ResourceName: "resource-nt",
			DeviceIds:    []string{"devA"},
		},
		{
			ResourceName: "resource-mm",
			DeviceIds:    []string{"devM0"},
			Topology: &podresourcesapi.TopologyInfo{
				Nodes: []*podresourcesapi.NUMANode{
					{
						ID: 0,
					},
				},
			},
		},
		{
			ResourceName: "resource-mm",
			DeviceIds:    []string{"devMM"},
			Topology: &podresourcesapi.TopologyInfo{
				Nodes: []*podresourcesapi.NUMANode{
					{
						ID: 0,
					},
					{
						ID: 1,
					},
				},
			},
		},
	}

	allCPUs := []int64{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}

	allMemory := []*podresourcesapi.ContainerMemory{
		{
			MemoryType: "memory",
			Size_:      5368709120,
			Topology: &podresourcesapi.TopologyInfo{
				Nodes: []*podresourcesapi.NUMANode{
					{
						ID: 0,
					},
				},
			},
		},
		{
			MemoryType: "hugepages-2Mi",
			Size_:      1073741824,
			Topology: &podresourcesapi.TopologyInfo{
				Nodes: []*podresourcesapi.NUMANode{
					{
						ID: 0,
					},
				},
			},
		},
		{
			MemoryType: "memory",
			Size_:      5368709120,
			Topology: &podresourcesapi.TopologyInfo{
				Nodes: []*podresourcesapi.NUMANode{
					{
						ID: 1,
					},
				},
			},
		},
		{
			MemoryType: "hugepages-2Mi",
			Size_:      1073741824,
			Topology: &podresourcesapi.TopologyInfo{
				Nodes: []*podresourcesapi.NUMANode{
					{
						ID: 1,
					},
				},
			},
		},
	}

	for _, tc := range []struct {
		desc                                 string
		allCPUs                              []int64
		allDevices                           []*podresourcesapi.ContainerDevices
		allMemory                            []*podresourcesapi.ContainerMemory
		expectedAllocatableResourcesResponse *podresourcesapi.AllocatableResourcesResponse
	}{
		{
			desc:                                 "no devices, no CPUs",
			allCPUs:                              []int64{},
			allDevices:                           []*podresourcesapi.ContainerDevices{},
			allMemory:                            []*podresourcesapi.ContainerMemory{},
			expectedAllocatableResourcesResponse: &podresourcesapi.AllocatableResourcesResponse{},
		},
		{
			desc:       "no devices, all CPUs",
			allCPUs:    allCPUs,
			allDevices: []*podresourcesapi.ContainerDevices{},
			expectedAllocatableResourcesResponse: &podresourcesapi.AllocatableResourcesResponse{
				CpuIds: allCPUs,
			},
		},
		{
			desc:       "no devices, no CPUs, all memory",
			allCPUs:    []int64{},
			allDevices: []*podresourcesapi.ContainerDevices{},
			allMemory:  allMemory,
			expectedAllocatableResourcesResponse: &podresourcesapi.AllocatableResourcesResponse{
				Memory: allMemory,
			},
		},
		{
			desc:       "with devices, all CPUs",
			allCPUs:    allCPUs,
			allDevices: allDevs,
			expectedAllocatableResourcesResponse: &podresourcesapi.AllocatableResourcesResponse{
				CpuIds: allCPUs,
				Devices: []*podresourcesapi.ContainerDevices{
					{
						ResourceName: "resource",
						DeviceIds:    []string{"dev0"},
						Topology: &podresourcesapi.TopologyInfo{
							Nodes: []*podresourcesapi.NUMANode{
								{
									ID: 0,
								},
							},
						},
					},
					{
						ResourceName: "resource",
						DeviceIds:    []string{"dev1"},
						Topology: &podresourcesapi.TopologyInfo{
							Nodes: []*podresourcesapi.NUMANode{
								{
									ID: 1,
								},
							},
						},
					},
					{
						ResourceName: "resource-nt",
						DeviceIds:    []string{"devA"},
					},
					{
						ResourceName: "resource-mm",
						DeviceIds:    []string{"devM0"},
						Topology: &podresourcesapi.TopologyInfo{
							Nodes: []*podresourcesapi.NUMANode{
								{
									ID: 0,
								},
							},
						},
					},
					{
						ResourceName: "resource-mm",
						DeviceIds:    []string{"devMM"},
						Topology: &podresourcesapi.TopologyInfo{
							Nodes: []*podresourcesapi.NUMANode{
								{
									ID: 0,
								},
								{
									ID: 1,
								},
							},
						},
					},
				},
			},
		},
		{
			desc:       "with devices, no CPUs",
			allCPUs:    []int64{},
			allDevices: allDevs,
			expectedAllocatableResourcesResponse: &podresourcesapi.AllocatableResourcesResponse{
				Devices: []*podresourcesapi.ContainerDevices{
					{
						ResourceName: "resource",
						DeviceIds:    []string{"dev0"},
						Topology: &podresourcesapi.TopologyInfo{
							Nodes: []*podresourcesapi.NUMANode{
								{
									ID: 0,
								},
							},
						},
					},
					{
						ResourceName: "resource",
						DeviceIds:    []string{"dev1"},
						Topology: &podresourcesapi.TopologyInfo{
							Nodes: []*podresourcesapi.NUMANode{
								{
									ID: 1,
								},
							},
						},
					},
					{
						ResourceName: "resource-nt",
						DeviceIds:    []string{"devA"},
					},
					{
						ResourceName: "resource-mm",
						DeviceIds:    []string{"devM0"},
						Topology: &podresourcesapi.TopologyInfo{
							Nodes: []*podresourcesapi.NUMANode{
								{
									ID: 0,
								},
							},
						},
					},
					{
						ResourceName: "resource-mm",
						DeviceIds:    []string{"devMM"},
						Topology: &podresourcesapi.TopologyInfo{
							Nodes: []*podresourcesapi.NUMANode{
								{
									ID: 0,
								},
								{
									ID: 1,
								},
							},
						},
					},
				},
			},
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			mockDevicesProvider := podresourcetest.NewMockDevicesProvider(t)
			mockPodsProvider := podresourcetest.NewMockPodsProvider(t)
			mockCPUsProvider := podresourcetest.NewMockCPUsProvider(t)
			mockMemoryProvider := podresourcetest.NewMockMemoryProvider(t)

			mockDevicesProvider.EXPECT().GetDevices("", "").Return([]*podresourcesapi.ContainerDevices{}).Maybe()
			mockCPUsProvider.EXPECT().GetCPUs("", "").Return([]int64{}).Maybe()
			mockMemoryProvider.EXPECT().GetMemory("", "").Return([]*podresourcesapi.ContainerMemory{}).Maybe()
			mockDevicesProvider.EXPECT().UpdateAllocatedDevices().Return().Maybe()
			mockDevicesProvider.EXPECT().GetAllocatableDevices().Return(tc.allDevices).Maybe()
			mockCPUsProvider.EXPECT().GetAllocatableCPUs().Return(tc.allCPUs).Maybe()
			mockMemoryProvider.EXPECT().GetAllocatableMemory().Return(tc.allMemory).Maybe()

			providers := PodResourcesProviders{
				Pods:    mockPodsProvider,
				Devices: mockDevicesProvider,
				Cpus:    mockCPUsProvider,
				Memory:  mockMemoryProvider,
			}
			server := NewV1PodResourcesServer(providers)

			resp, err := server.GetAllocatableResources(tCtx, &podresourcesapi.AllocatableResourcesRequest{})
			if err != nil {
				t.Errorf("want err = %v, got %q", nil, err)
			}

			if diff := cmp.Diff(tc.expectedAllocatableResourcesResponse, resp, cmpopts.EquateEmpty()); diff != "" {
				t.Fatal(diff)
			}
		})
	}
}

func TestGetPodResourcesV1(t *testing.T) {
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, pkgfeatures.KubeletPodResourcesGet, true)
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, pkgfeatures.KubeletPodResourcesDynamicResources, true)

	tCtx := ktesting.Init(t)
	podName := "pod-name"
	podNamespace := "pod-namespace"
	podUID := types.UID("pod-uid")
	containerName := "container-name"
	numaID := int64(1)

	devs := []*podresourcesapi.ContainerDevices{
		{
			ResourceName: "resource",
			DeviceIds:    []string{"dev0", "dev1"},
			Topology:     &podresourcesapi.TopologyInfo{Nodes: []*podresourcesapi.NUMANode{{ID: numaID}}},
		},
	}

	cpus := []int64{12, 23, 30}

	memory := []*podresourcesapi.ContainerMemory{
		{
			MemoryType: "memory",
			Size_:      1073741824,
			Topology:   &podresourcesapi.TopologyInfo{Nodes: []*podresourcesapi.NUMANode{{ID: numaID}}},
		},
		{
			MemoryType: "hugepages-1Gi",
			Size_:      1073741824,
			Topology:   &podresourcesapi.TopologyInfo{Nodes: []*podresourcesapi.NUMANode{{ID: numaID}}},
		},
	}

	containers := []v1.Container{
		{
			Name: containerName,
		},
	}

	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: podNamespace,
			UID:       podUID,
		},
		Spec: v1.PodSpec{
			Containers: containers,
		},
	}

	pluginCDIDevices := []*podresourcesapi.CDIDevice{{Name: "dra-dev0"}, {Name: "dra-dev1"}}
	draDevs := []*podresourcesapi.DynamicResource{
		{
			ClaimName:      "claim-name",
			ClaimNamespace: "default",
			ClaimResources: []*podresourcesapi.ClaimResource{{CDIDevices: pluginCDIDevices}},
		},
	}

	for _, tc := range []struct {
		desc             string
		err              error
		exist            bool
		pod              *v1.Pod
		devices          []*podresourcesapi.ContainerDevices
		cpus             []int64
		memory           []*podresourcesapi.ContainerMemory
		dynamicResources []*podresourcesapi.DynamicResource
		expectedResponse *podresourcesapi.GetPodResourcesResponse
	}{
		{
			desc:             "pod not exist",
			err:              fmt.Errorf("pod %s in namespace %s not found", podName, podNamespace),
			exist:            false,
			pod:              nil,
			devices:          []*podresourcesapi.ContainerDevices{},
			cpus:             []int64{},
			memory:           []*podresourcesapi.ContainerMemory{},
			dynamicResources: []*podresourcesapi.DynamicResource{},

			expectedResponse: &podresourcesapi.GetPodResourcesResponse{},
		},
		{
			desc:             "pod without devices",
			err:              nil,
			exist:            true,
			pod:              pod,
			devices:          []*podresourcesapi.ContainerDevices{},
			cpus:             []int64{},
			memory:           []*podresourcesapi.ContainerMemory{},
			dynamicResources: []*podresourcesapi.DynamicResource{},
			expectedResponse: &podresourcesapi.GetPodResourcesResponse{
				PodResources: &podresourcesapi.PodResources{
					Name:      podName,
					Namespace: podNamespace,
					Containers: []*podresourcesapi.ContainerResources{
						{
							Name:             containerName,
							Devices:          []*podresourcesapi.ContainerDevices{},
							DynamicResources: []*podresourcesapi.DynamicResource{},
						},
					},
				},
			},
		},
		{
			desc:             "pod with devices",
			err:              nil,
			exist:            true,
			pod:              pod,
			devices:          devs,
			cpus:             cpus,
			memory:           memory,
			dynamicResources: draDevs,
			expectedResponse: &podresourcesapi.GetPodResourcesResponse{
				PodResources: &podresourcesapi.PodResources{
					Name:      podName,
					Namespace: podNamespace,
					Containers: []*podresourcesapi.ContainerResources{
						{
							Name:             containerName,
							Devices:          devs,
							CpuIds:           cpus,
							Memory:           memory,
							DynamicResources: draDevs,
						},
					},
				},
			},
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			mockDevicesProvider := podresourcetest.NewMockDevicesProvider(t)
			mockPodsProvider := podresourcetest.NewMockPodsProvider(t)
			mockCPUsProvider := podresourcetest.NewMockCPUsProvider(t)
			mockMemoryProvider := podresourcetest.NewMockMemoryProvider(t)
			mockDynamicResourcesProvider := podresourcetest.NewMockDynamicResourcesProvider(t)

			mockPodsProvider.EXPECT().GetPodByName(podNamespace, podName).Return(tc.pod, tc.exist).Maybe()
			mockDevicesProvider.EXPECT().GetDevices(string(podUID), containerName).Return(tc.devices).Maybe()
			mockCPUsProvider.EXPECT().GetCPUs(string(podUID), containerName).Return(tc.cpus).Maybe()
			mockMemoryProvider.EXPECT().GetMemory(string(podUID), containerName).Return(tc.memory).Maybe()
			mockDynamicResourcesProvider.EXPECT().GetDynamicResources(pod, &containers[0]).Return(tc.dynamicResources).Maybe()
			mockDevicesProvider.EXPECT().UpdateAllocatedDevices().Return().Maybe()
			mockCPUsProvider.EXPECT().GetAllocatableCPUs().Return([]int64{}).Maybe()
			mockDevicesProvider.EXPECT().GetAllocatableDevices().Return([]*podresourcesapi.ContainerDevices{}).Maybe()
			mockMemoryProvider.EXPECT().GetAllocatableMemory().Return([]*podresourcesapi.ContainerMemory{}).Maybe()

			providers := PodResourcesProviders{
				Pods:             mockPodsProvider,
				Devices:          mockDevicesProvider,
				Cpus:             mockCPUsProvider,
				Memory:           mockMemoryProvider,
				DynamicResources: mockDynamicResourcesProvider,
			}
			server := NewV1PodResourcesServer(providers)
			podReq := &podresourcesapi.GetPodResourcesRequest{PodName: podName, PodNamespace: podNamespace}
			resp, err := server.Get(tCtx, podReq)

			if err != nil {
				if err.Error() != tc.err.Error() {
					t.Errorf("want exit = %v, got %v", tc.err, err)
				}
			} else {
				if err != tc.err {
					t.Errorf("want exit = %v, got %v", tc.err, err)
				} else {
					if diff := cmp.Diff(tc.expectedResponse, resp, cmpopts.EquateEmpty()); diff != "" {
						t.Fatal(diff)
					}
				}
			}
		})
	}

}

func TestGetPodResourcesWithInitContainersV1(t *testing.T) {
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, pkgfeatures.KubeletPodResourcesGet, true)
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, pkgfeatures.KubeletPodResourcesDynamicResources, true)

	tCtx := ktesting.Init(t)
	podName := "pod-name"
	podNamespace := "pod-namespace"
	podUID := types.UID("pod-uid")
	initContainerName := "init-container-name"
	containerName := "container-name"
	numaID := int64(1)
	containerRestartPolicyAlways := v1.ContainerRestartPolicyAlways

	devs := []*podresourcesapi.ContainerDevices{
		{
			ResourceName: "resource",
			DeviceIds:    []string{"dev0", "dev1"},
			Topology:     &podresourcesapi.TopologyInfo{Nodes: []*podresourcesapi.NUMANode{{ID: numaID}}},
		},
	}

	cpus := []int64{12, 23, 30}

	memory := []*podresourcesapi.ContainerMemory{
		{
			MemoryType: "memory",
			Size_:      1073741824,
			Topology:   &podresourcesapi.TopologyInfo{Nodes: []*podresourcesapi.NUMANode{{ID: numaID}}},
		},
		{
			MemoryType: "hugepages-1Gi",
			Size_:      1073741824,
			Topology:   &podresourcesapi.TopologyInfo{Nodes: []*podresourcesapi.NUMANode{{ID: numaID}}},
		},
	}

	containers := []v1.Container{
		{
			Name: containerName,
		},
	}

	for _, tc := range []struct {
		desc     string
		pod      *v1.Pod
		mockFunc func(
			*v1.Pod,
			*podresourcetest.MockDevicesProvider,
			*podresourcetest.MockCPUsProvider,
			*podresourcetest.MockMemoryProvider,
			*podresourcetest.MockDynamicResourcesProvider)
		sidecarContainersEnabled bool
		expectedResponse         *podresourcesapi.GetPodResourcesResponse
	}{
		{
			desc: "pod having an init container",
			pod: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      podName,
					Namespace: podNamespace,
					UID:       podUID,
				},
				Spec: v1.PodSpec{
					InitContainers: []v1.Container{
						{
							Name: initContainerName,
						},
					},
					Containers: containers,
				},
			},
			mockFunc: func(
				pod *v1.Pod,
				devicesProvider *podresourcetest.MockDevicesProvider,
				cpusProvider *podresourcetest.MockCPUsProvider,
				memoryProvider *podresourcetest.MockMemoryProvider,
				dynamicResourcesProvider *podresourcetest.MockDynamicResourcesProvider) {
				devicesProvider.EXPECT().UpdateAllocatedDevices().Return().Maybe()
				devicesProvider.EXPECT().GetDevices(string(podUID), containerName).Return(devs).Maybe()
				cpusProvider.EXPECT().GetCPUs(string(podUID), containerName).Return(cpus).Maybe()
				memoryProvider.EXPECT().GetMemory(string(podUID), containerName).Return(memory).Maybe()
				dynamicResourcesProvider.EXPECT().GetDynamicResources(pod, &pod.Spec.Containers[0]).Return([]*podresourcesapi.DynamicResource{}).Maybe()

			},
			expectedResponse: &podresourcesapi.GetPodResourcesResponse{
				PodResources: &podresourcesapi.PodResources{
					Name:      podName,
					Namespace: podNamespace,
					Containers: []*podresourcesapi.ContainerResources{
						{
							Name:             containerName,
							Devices:          devs,
							CpuIds:           cpus,
							Memory:           memory,
							DynamicResources: []*podresourcesapi.DynamicResource{},
						},
					},
				},
			},
		},
		{
			desc: "pod having an init container with SidecarContainers enabled",
			pod: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      podName,
					Namespace: podNamespace,
					UID:       podUID,
				},
				Spec: v1.PodSpec{
					InitContainers: []v1.Container{
						{
							Name: initContainerName,
						},
					},
					Containers: containers,
				},
			},
			mockFunc: func(
				pod *v1.Pod,
				devicesProvider *podresourcetest.MockDevicesProvider,
				cpusProvider *podresourcetest.MockCPUsProvider,
				memoryProvider *podresourcetest.MockMemoryProvider,
				dynamicResourcesProvider *podresourcetest.MockDynamicResourcesProvider) {
				devicesProvider.EXPECT().UpdateAllocatedDevices().Return().Maybe()
				devicesProvider.EXPECT().GetDevices(string(podUID), containerName).Return(devs).Maybe()
				cpusProvider.EXPECT().GetCPUs(string(podUID), containerName).Return(cpus).Maybe()
				memoryProvider.EXPECT().GetMemory(string(podUID), containerName).Return(memory).Maybe()
				dynamicResourcesProvider.EXPECT().GetDynamicResources(pod, &pod.Spec.Containers[0]).Return([]*podresourcesapi.DynamicResource{}).Maybe()

			},
			sidecarContainersEnabled: true,
			expectedResponse: &podresourcesapi.GetPodResourcesResponse{
				PodResources: &podresourcesapi.PodResources{
					Name:      podName,
					Namespace: podNamespace,
					Containers: []*podresourcesapi.ContainerResources{
						{
							Name:             containerName,
							Devices:          devs,
							CpuIds:           cpus,
							Memory:           memory,
							DynamicResources: []*podresourcesapi.DynamicResource{},
						},
					},
				},
			},
		},
		{
			desc: "pod having a restartable init container with SidecarContainers disabled",
			pod: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      podName,
					Namespace: podNamespace,
					UID:       podUID,
				},
				Spec: v1.PodSpec{
					InitContainers: []v1.Container{
						{
							Name:          initContainerName,
							RestartPolicy: &containerRestartPolicyAlways,
						},
					},
					Containers: containers,
				},
			},
			mockFunc: func(
				pod *v1.Pod,
				devicesProvider *podresourcetest.MockDevicesProvider,
				cpusProvider *podresourcetest.MockCPUsProvider,
				memoryProvider *podresourcetest.MockMemoryProvider,
				dynamicResourcesProvider *podresourcetest.MockDynamicResourcesProvider) {
				devicesProvider.EXPECT().UpdateAllocatedDevices().Return().Maybe()

				devicesProvider.EXPECT().GetDevices(string(podUID), containerName).Return(devs).Maybe()
				cpusProvider.EXPECT().GetCPUs(string(podUID), containerName).Return(cpus).Maybe()
				memoryProvider.EXPECT().GetMemory(string(podUID), containerName).Return(memory).Maybe()
				dynamicResourcesProvider.EXPECT().GetDynamicResources(pod, &pod.Spec.Containers[0]).Return([]*podresourcesapi.DynamicResource{}).Maybe()

			},
			expectedResponse: &podresourcesapi.GetPodResourcesResponse{
				PodResources: &podresourcesapi.PodResources{
					Name:      podName,
					Namespace: podNamespace,
					Containers: []*podresourcesapi.ContainerResources{
						{
							Name:             containerName,
							Devices:          devs,
							CpuIds:           cpus,
							Memory:           memory,
							DynamicResources: []*podresourcesapi.DynamicResource{},
						},
					},
				},
			},
		},
		{
			desc: "pod having an init container with SidecarContainers enabled",
			pod: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      podName,
					Namespace: podNamespace,
					UID:       podUID,
				},
				Spec: v1.PodSpec{
					InitContainers: []v1.Container{
						{
							Name:          initContainerName,
							RestartPolicy: &containerRestartPolicyAlways,
						},
					},
					Containers: containers,
				},
			},
			mockFunc: func(
				pod *v1.Pod,
				devicesProvider *podresourcetest.MockDevicesProvider,
				cpusProvider *podresourcetest.MockCPUsProvider,
				memoryProvider *podresourcetest.MockMemoryProvider,
				dynamicResourcesProvider *podresourcetest.MockDynamicResourcesProvider) {
				devicesProvider.EXPECT().UpdateAllocatedDevices().Return().Maybe()

				devicesProvider.EXPECT().GetDevices(string(podUID), initContainerName).Return(devs).Maybe()
				cpusProvider.EXPECT().GetCPUs(string(podUID), initContainerName).Return(cpus).Maybe()
				memoryProvider.EXPECT().GetMemory(string(podUID), initContainerName).Return(memory).Maybe()
				dynamicResourcesProvider.EXPECT().GetDynamicResources(pod, &pod.Spec.InitContainers[0]).Return([]*podresourcesapi.DynamicResource{}).Maybe()

				devicesProvider.EXPECT().GetDevices(string(podUID), containerName).Return(devs).Maybe()
				cpusProvider.EXPECT().GetCPUs(string(podUID), containerName).Return(cpus).Maybe()
				memoryProvider.EXPECT().GetMemory(string(podUID), containerName).Return(memory).Maybe()
				dynamicResourcesProvider.EXPECT().GetDynamicResources(pod, &pod.Spec.Containers[0]).Return([]*podresourcesapi.DynamicResource{}).Maybe()

			},
			sidecarContainersEnabled: true,
			expectedResponse: &podresourcesapi.GetPodResourcesResponse{
				PodResources: &podresourcesapi.PodResources{
					Name:      podName,
					Namespace: podNamespace,
					Containers: []*podresourcesapi.ContainerResources{
						{
							Name:             initContainerName,
							Devices:          devs,
							CpuIds:           cpus,
							Memory:           memory,
							DynamicResources: []*podresourcesapi.DynamicResource{},
						},
						{
							Name:             containerName,
							Devices:          devs,
							CpuIds:           cpus,
							Memory:           memory,
							DynamicResources: []*podresourcesapi.DynamicResource{},
						},
					},
				},
			},
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, pkgfeatures.SidecarContainers, tc.sidecarContainersEnabled)

			mockDevicesProvider := podresourcetest.NewMockDevicesProvider(t)
			mockPodsProvider := podresourcetest.NewMockPodsProvider(t)
			mockCPUsProvider := podresourcetest.NewMockCPUsProvider(t)
			mockMemoryProvider := podresourcetest.NewMockMemoryProvider(t)
			mockDynamicResourcesProvider := podresourcetest.NewMockDynamicResourcesProvider(t)

			mockPodsProvider.EXPECT().GetPodByName(podNamespace, podName).Return(tc.pod, true).Maybe()
			tc.mockFunc(tc.pod, mockDevicesProvider, mockCPUsProvider, mockMemoryProvider, mockDynamicResourcesProvider)

			providers := PodResourcesProviders{
				Pods:             mockPodsProvider,
				Devices:          mockDevicesProvider,
				Cpus:             mockCPUsProvider,
				Memory:           mockMemoryProvider,
				DynamicResources: mockDynamicResourcesProvider,
			}
			server := NewV1PodResourcesServer(providers)
			podReq := &podresourcesapi.GetPodResourcesRequest{PodName: podName, PodNamespace: podNamespace}
			resp, err := server.Get(tCtx, podReq)
			if err != nil {
				t.Errorf("want err = %v, got %q", nil, err)
			}
			if diff := cmp.Diff(tc.expectedResponse, resp, cmpopts.EquateEmpty()); diff != "" {
				t.Fatal(diff)
			}
		})
	}
}
