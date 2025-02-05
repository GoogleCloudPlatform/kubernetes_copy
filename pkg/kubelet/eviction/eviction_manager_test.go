/*
Copyright 2016 The Kubernetes Authors.

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

package eviction

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/tools/record"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	statsapi "k8s.io/kubelet/pkg/apis/stats/v1alpha1"
	kubeapi "k8s.io/kubernetes/pkg/apis/core"
	"k8s.io/kubernetes/pkg/apis/scheduling"
	"k8s.io/kubernetes/pkg/features"
	evictionapi "k8s.io/kubernetes/pkg/kubelet/eviction/api"
	"k8s.io/kubernetes/pkg/kubelet/lifecycle"
	kubelettypes "k8s.io/kubernetes/pkg/kubelet/types"
	testingclock "k8s.io/utils/clock/testing"
	"k8s.io/utils/ptr"
)

const (
	lowPriority     = -1
	defaultPriority = 0
	highPriority    = 1
)

// mockPodKiller is used to testing which pod is killed
type mockPodKiller struct {
	pod                 *v1.Pod
	evict               bool
	statusFn            func(*v1.PodStatus)
	gracePeriodOverride *int64
}

// killPodNow records the pod that was killed
func (m *mockPodKiller) killPodNow(pod *v1.Pod, evict bool, gracePeriodOverride *int64, statusFn func(*v1.PodStatus)) error {
	m.pod = pod
	m.statusFn = statusFn
	m.evict = evict
	m.gracePeriodOverride = gracePeriodOverride
	return nil
}

// mockDiskInfoProvider is used to simulate testing.
type mockDiskInfoProvider struct {
	dedicatedImageFs     *bool
	dedicatedContainerFs *bool
}

// HasDedicatedImageFs returns the mocked value
func (m *mockDiskInfoProvider) HasDedicatedImageFs(_ context.Context) (bool, error) {
	return ptr.Deref(m.dedicatedImageFs, false), nil
}

// HasDedicatedContainerFs returns the mocked value
func (m *mockDiskInfoProvider) HasDedicatedContainerFs(_ context.Context) (bool, error) {
	return ptr.Deref(m.dedicatedContainerFs, false), nil
}

// mockDiskGC is used to simulate invoking image and container garbage collection.
type mockDiskGC struct {
	err                 error
	imageGCInvoked      bool
	containerGCInvoked  bool
	fakeSummaryProvider *fakeSummaryProvider
	summaryAfterGC      *statsapi.Summary
}

// DeleteUnusedImages returns the mocked values.
func (m *mockDiskGC) DeleteUnusedImages(_ context.Context) error {
	m.imageGCInvoked = true
	if m.summaryAfterGC != nil && m.fakeSummaryProvider != nil {
		m.fakeSummaryProvider.result = m.summaryAfterGC
	}
	return m.err
}

// DeleteAllUnusedContainers returns the mocked value
func (m *mockDiskGC) DeleteAllUnusedContainers(_ context.Context) error {
	m.containerGCInvoked = true
	if m.summaryAfterGC != nil && m.fakeSummaryProvider != nil {
		m.fakeSummaryProvider.result = m.summaryAfterGC
	}
	return m.err
}

func makePodWithMemoryStats(name string, priority int32, requests v1.ResourceList, limits v1.ResourceList, memoryWorkingSet string) (*v1.Pod, statsapi.PodStats) {
	pod := newPod(name, priority, []v1.Container{
		newContainer(name, requests, limits),
	}, nil)
	podStats := newPodMemoryStats(pod, resource.MustParse(memoryWorkingSet))
	return pod, podStats
}

func makePodWithPIDStats(name string, priority int32, processCount uint64) (*v1.Pod, statsapi.PodStats) {
	pod := newPod(name, priority, []v1.Container{
		newContainer(name, nil, nil),
	}, nil)
	podStats := newPodProcessStats(pod, processCount)
	return pod, podStats
}

func makePodWithDiskStats(name string, priority int32, requests v1.ResourceList, limits v1.ResourceList, rootFsUsed, logsUsed, perLocalVolumeUsed string, volumes []v1.Volume) (*v1.Pod, statsapi.PodStats) {
	pod := newPod(name, priority, []v1.Container{
		newContainer(name, requests, limits),
	}, volumes)
	podStats := newPodDiskStats(pod, parseQuantity(rootFsUsed), parseQuantity(logsUsed), parseQuantity(perLocalVolumeUsed))
	return pod, podStats
}

func makePodWithLocalStorageCapacityIsolationOpen(name string, priority int32, requests v1.ResourceList, limits v1.ResourceList, memoryWorkingSet string) (*v1.Pod, statsapi.PodStats) {
	vol := newVolume("local-volume", v1.VolumeSource{
		EmptyDir: &v1.EmptyDirVolumeSource{
			SizeLimit: resource.NewQuantity(requests.Memory().Value(), resource.BinarySI),
		},
	})
	var vols []v1.Volume
	vols = append(vols, vol)
	pod := newPod(name, priority, []v1.Container{
		newContainer(name, requests, limits),
	}, vols)

	var podStats statsapi.PodStats
	switch name {
	case "empty-dir":
		podStats = newPodMemoryStats(pod, *resource.NewQuantity(requests.Memory().Value()*2, resource.BinarySI))
	case "container-ephemeral-storage-limit":
		podStats = newPodMemoryStats(pod, *resource.NewQuantity(limits.StorageEphemeral().Value(), resource.BinarySI))
	case "pod-ephemeral-storage-limit":
		podStats = newPodMemoryStats(pod, *resource.NewQuantity(limits.StorageEphemeral().Value()*2, resource.BinarySI))
	default:
		podStats = newPodMemoryStats(pod, resource.MustParse(memoryWorkingSet))
	}
	return pod, podStats
}

func makePIDStats(nodeAvailablePIDs string, numberOfRunningProcesses string, podStats map[*v1.Pod]statsapi.PodStats) *statsapi.Summary {
	val := resource.MustParse(nodeAvailablePIDs)
	availablePIDs := int64(val.Value())

	parsed := resource.MustParse(numberOfRunningProcesses)
	NumberOfRunningProcesses := int64(parsed.Value())
	result := &statsapi.Summary{
		Node: statsapi.NodeStats{
			Rlimit: &statsapi.RlimitStats{
				MaxPID:                &availablePIDs,
				NumOfRunningProcesses: &NumberOfRunningProcesses,
			},
		},
		Pods: []statsapi.PodStats{},
	}
	for _, podStat := range podStats {
		result.Pods = append(result.Pods, podStat)
	}
	return result
}

func makeMemoryStats(nodeAvailableBytes string, podStats map[*v1.Pod]statsapi.PodStats) *statsapi.Summary {
	val := resource.MustParse(nodeAvailableBytes)
	availableBytes := uint64(val.Value())
	WorkingSetBytes := uint64(val.Value())
	result := &statsapi.Summary{
		Node: statsapi.NodeStats{
			Memory: &statsapi.MemoryStats{
				AvailableBytes:  &availableBytes,
				WorkingSetBytes: &WorkingSetBytes,
			},
			Swap: &statsapi.SwapStats{
				SwapAvailableBytes: ptr.To(uint64(0)),
				SwapUsageBytes:     ptr.To(uint64(0)),
			},
			SystemContainers: []statsapi.ContainerStats{
				{
					Name: statsapi.SystemContainerPods,
					Memory: &statsapi.MemoryStats{
						AvailableBytes:  &availableBytes,
						WorkingSetBytes: &WorkingSetBytes,
					},
				},
			},
		},
		Pods: []statsapi.PodStats{},
	}
	for _, podStat := range podStats {
		result.Pods = append(result.Pods, podStat)
	}
	return result
}

type diskStats struct {
	rootFsAvailableBytes  string
	imageFsAvailableBytes string
	// optional fs
	// if not specified, than will assume imagefs=containerfs
	containerFsAvailableBytes string
	podStats                  map[*v1.Pod]statsapi.PodStats
}

func makeDiskStats(diskStats diskStats) *statsapi.Summary {
	rootFsVal := resource.MustParse(diskStats.rootFsAvailableBytes)
	rootFsBytes := uint64(rootFsVal.Value())
	rootFsCapacityBytes := uint64(rootFsVal.Value() * 2)
	imageFsVal := resource.MustParse(diskStats.imageFsAvailableBytes)
	imageFsBytes := uint64(imageFsVal.Value())
	imageFsCapacityBytes := uint64(imageFsVal.Value() * 2)
	if diskStats.containerFsAvailableBytes == "" {
		diskStats.containerFsAvailableBytes = diskStats.imageFsAvailableBytes
	}
	containerFsVal := resource.MustParse(diskStats.containerFsAvailableBytes)
	containerFsBytes := uint64(containerFsVal.Value())
	containerFsCapacityBytes := uint64(containerFsVal.Value() * 2)
	result := &statsapi.Summary{
		Node: statsapi.NodeStats{
			Fs: &statsapi.FsStats{
				AvailableBytes: &rootFsBytes,
				CapacityBytes:  &rootFsCapacityBytes,
			},
			Runtime: &statsapi.RuntimeStats{
				ImageFs: &statsapi.FsStats{
					AvailableBytes: &imageFsBytes,
					CapacityBytes:  &imageFsCapacityBytes,
				},
				ContainerFs: &statsapi.FsStats{
					AvailableBytes: &containerFsBytes,
					CapacityBytes:  &containerFsCapacityBytes,
				},
			},
		},
		Pods: []statsapi.PodStats{},
	}
	for _, podStat := range diskStats.podStats {
		result.Pods = append(result.Pods, podStat)
	}
	return result
}

type podToMake struct {
	name                     string
	priority                 int32
	requests                 v1.ResourceList
	limits                   v1.ResourceList
	memoryWorkingSet         string
	pidUsage                 uint64
	rootFsUsed               string
	logsFsUsed               string
	logsFsInodesUsed         string
	rootFsInodesUsed         string
	perLocalVolumeUsed       string
	perLocalVolumeInodesUsed string
}

func TestMemoryPressure_VerifyPodStatus(t *testing.T) {
	podMaker := makePodWithMemoryStats
	summaryStatsMaker := makeMemoryStats
	podsToMake := []podToMake{
		{name: "below-requests", requests: newResourceList("", "1Gi", ""), limits: newResourceList("", "1Gi", ""), memoryWorkingSet: "900Mi"},
		{name: "above-requests", requests: newResourceList("", "100Mi", ""), limits: newResourceList("", "1Gi", ""), memoryWorkingSet: "700Mi"},
	}
	pods := []*v1.Pod{}
	podStats := map[*v1.Pod]statsapi.PodStats{}
	for _, podToMake := range podsToMake {
		pod, podStat := podMaker(podToMake.name, podToMake.priority, podToMake.requests, podToMake.limits, podToMake.memoryWorkingSet)
		pods = append(pods, pod)
		podStats[pod] = podStat
	}
	activePodsFunc := func() []*v1.Pod {
		return pods
	}

	fakeClock := testingclock.NewFakeClock(time.Now())
	podKiller := &mockPodKiller{}
	diskInfoProvider := &mockDiskInfoProvider{dedicatedImageFs: ptr.To(false)}
	diskGC := &mockDiskGC{err: nil}
	nodeRef := &v1.ObjectReference{Kind: "Node", Name: "test", UID: types.UID("test"), Namespace: ""}

	config := Config{
		PressureTransitionPeriod: time.Minute * 5,
		Thresholds: []evictionapi.Threshold{
			{
				Signal:   evictionapi.SignalMemoryAvailable,
				Operator: evictionapi.OpLessThan,
				Value: evictionapi.ThresholdValue{
					Quantity: quantityMustParse("2Gi"),
				},
			},
		},
	}
	summaryProvider := &fakeSummaryProvider{result: summaryStatsMaker("1500Mi", podStats)}
	manager := &managerImpl{
		clock:                        fakeClock,
		killPodFunc:                  podKiller.killPodNow,
		imageGC:                      diskGC,
		containerGC:                  diskGC,
		config:                       config,
		recorder:                     &record.FakeRecorder{},
		summaryProvider:              summaryProvider,
		nodeRef:                      nodeRef,
		nodeConditionsLastObservedAt: nodeConditionsObservedAt{},
		thresholdsFirstObservedAt:    thresholdsObservedAt{},
	}

	// synchronize to detect the memory pressure
	_, err := manager.synchronize(diskInfoProvider, activePodsFunc)

	if err != nil {
		t.Fatalf("Manager expects no error but got %v", err)
	}
	// verify memory pressure is detected
	if !manager.IsUnderMemoryPressure() {
		t.Fatalf("Manager should have detected memory pressure")
	}

	// verify a pod is selected for eviction
	if podKiller.pod == nil {
		t.Fatalf("Manager should have selected a pod for eviction")
	}

	wantPodStatus := v1.PodStatus{
		Phase:   v1.PodFailed,
		Reason:  "Evicted",
		Message: "The node was low on resource: memory. Threshold quantity: 2Gi, available: 1500Mi. ",
		Conditions: []v1.PodCondition{{
			Type:    "DisruptionTarget",
			Status:  "True",
			Reason:  "TerminationByKubelet",
			Message: "The node was low on resource: memory. Threshold quantity: 2Gi, available: 1500Mi. ",
		}},
	}

	// verify the pod status after applying the status update function
	podKiller.statusFn(&podKiller.pod.Status)
	if diff := cmp.Diff(wantPodStatus, podKiller.pod.Status, cmpopts.IgnoreFields(v1.PodCondition{}, "LastProbeTime", "LastTransitionTime")); diff != "" {
		t.Errorf("Unexpected pod status of the evicted pod (-want,+got):\n%s", diff)
	}
}

func TestPIDPressure_VerifyPodStatus(t *testing.T) {
	testCases := map[string]struct {
		wantPodStatus v1.PodStatus
	}{
		"eviction due to pid pressure": {
			wantPodStatus: v1.PodStatus{
				Phase:   v1.PodFailed,
				Reason:  "Evicted",
				Message: "The node was low on resource: pids. Threshold quantity: 1200, available: 500. ",
			},
		},
	}
	for _, tc := range testCases {
		podMaker := makePodWithPIDStats
		summaryStatsMaker := makePIDStats
		podsToMake := []podToMake{
			{name: "pod1", priority: lowPriority, pidUsage: 500},
			{name: "pod2", priority: defaultPriority, pidUsage: 500},
		}
		pods := []*v1.Pod{}
		podStats := map[*v1.Pod]statsapi.PodStats{}
		for _, podToMake := range podsToMake {
			pod, podStat := podMaker(podToMake.name, podToMake.priority, 2)
			pods = append(pods, pod)
			podStats[pod] = podStat
		}
		activePodsFunc := func() []*v1.Pod {
			return pods
		}

		fakeClock := testingclock.NewFakeClock(time.Now())
		podKiller := &mockPodKiller{}
		diskInfoProvider := &mockDiskInfoProvider{dedicatedImageFs: ptr.To(false), dedicatedContainerFs: ptr.To(false)}
		diskGC := &mockDiskGC{err: nil}
		nodeRef := &v1.ObjectReference{Kind: "Node", Name: "test", UID: types.UID("test"), Namespace: ""}

		config := Config{
			PressureTransitionPeriod: time.Minute * 5,
			Thresholds: []evictionapi.Threshold{
				{
					Signal:   evictionapi.SignalPIDAvailable,
					Operator: evictionapi.OpLessThan,
					Value: evictionapi.ThresholdValue{
						Quantity: quantityMustParse("1200"),
					},
				},
			},
		}
		summaryProvider := &fakeSummaryProvider{result: summaryStatsMaker("1500", "1000", podStats)}
		manager := &managerImpl{
			clock:                        fakeClock,
			killPodFunc:                  podKiller.killPodNow,
			imageGC:                      diskGC,
			containerGC:                  diskGC,
			config:                       config,
			recorder:                     &record.FakeRecorder{},
			summaryProvider:              summaryProvider,
			nodeRef:                      nodeRef,
			nodeConditionsLastObservedAt: nodeConditionsObservedAt{},
			thresholdsFirstObservedAt:    thresholdsObservedAt{},
		}

		// synchronize to detect the PID pressure
		_, err := manager.synchronize(diskInfoProvider, activePodsFunc)

		if err != nil {
			t.Fatalf("Manager expects no error but got %v", err)
		}

		// verify PID pressure is detected
		if !manager.IsUnderPIDPressure() {
			t.Fatalf("Manager should have detected PID pressure")
		}

		// verify a pod is selected for eviction
		if podKiller.pod == nil {
			t.Fatalf("Manager should have selected a pod for eviction")
		}

		wantPodStatus := tc.wantPodStatus.DeepCopy()
		wantPodStatus.Conditions = append(wantPodStatus.Conditions, v1.PodCondition{
			Type:    "DisruptionTarget",
			Status:  "True",
			Reason:  "TerminationByKubelet",
			Message: "The node was low on resource: pids. Threshold quantity: 1200, available: 500. ",
		})

		// verify the pod status after applying the status update function
		podKiller.statusFn(&podKiller.pod.Status)
		if diff := cmp.Diff(*wantPodStatus, podKiller.pod.Status, cmpopts.IgnoreFields(v1.PodCondition{}, "LastProbeTime", "LastTransitionTime")); diff != "" {
			t.Errorf("Unexpected pod status of the evicted pod (-want,+got):\n%s", diff)
		}
	}
}

func TestDiskPressureNodeFs_VerifyPodStatus(t *testing.T) {
	testCases := map[string]struct {
		nodeFsStats                   string
		imageFsStats                  string
		containerFsStats              string
		evictionMessage               string
		kubeletSeparateDiskFeature    bool
		writeableSeparateFromReadOnly bool
		thresholdToMonitor            evictionapi.Threshold
		podToMakes                    []podToMake
		dedicatedImageFs              *bool
		expectErr                     string
	}{
		"eviction due to disk pressure; no image fs": {
			dedicatedImageFs: ptr.To(false),
			nodeFsStats:      "1.5Gi",
			imageFsStats:     "10Gi",
			containerFsStats: "10Gi",
			thresholdToMonitor: evictionapi.Threshold{
				Signal:   evictionapi.SignalNodeFsAvailable,
				Operator: evictionapi.OpLessThan,
				Value: evictionapi.ThresholdValue{
					Quantity: quantityMustParse("2Gi"),
				},
			},
			evictionMessage: "The node was low on resource: ephemeral-storage. Threshold quantity: 2Gi, available: 1536Mi. Container above-requests was using 700Mi, request is 100Mi, has larger consumption of ephemeral-storage. ",
			podToMakes: []podToMake{
				{name: "below-requests", requests: newResourceList("", "", "1Gi"), limits: newResourceList("", "", "1Gi"), rootFsUsed: "900Mi"},
				{name: "above-requests", requests: newResourceList("", "", "100Mi"), limits: newResourceList("", "", "1Gi"), rootFsUsed: "700Mi"},
			},
		},
		"eviction due to image disk pressure; image fs": {
			dedicatedImageFs: ptr.To(true),
			nodeFsStats:      "1Gi",
			imageFsStats:     "10Gi",
			containerFsStats: "10Gi",
			evictionMessage:  "The node was low on resource: ephemeral-storage. Threshold quantity: 50Gi, available: 10Gi. Container above-requests was using 80Gi, request is 50Gi, has larger consumption of ephemeral-storage. ",
			thresholdToMonitor: evictionapi.Threshold{
				Signal:   evictionapi.SignalImageFsAvailable,
				Operator: evictionapi.OpLessThan,
				Value: evictionapi.ThresholdValue{
					Quantity: quantityMustParse("50Gi"),
				},
			},
			podToMakes: []podToMake{
				{name: "below-requests", requests: newResourceList("", "", "1Gi"), limits: newResourceList("", "", "1Gi"), rootFsUsed: "900Mi"},
				{name: "above-requests", requests: newResourceList("", "", "50Gi"), limits: newResourceList("", "", "50Gi"), rootFsUsed: "80Gi"},
			},
		},
		"eviction due to container disk pressure; feature off; error; container fs": {
			dedicatedImageFs:              ptr.To(true),
			kubeletSeparateDiskFeature:    false,
			writeableSeparateFromReadOnly: true,
			expectErr:                     "KubeletSeparateDiskGC is turned off but we still have a split filesystem",
			nodeFsStats:                   "1Gi",
			imageFsStats:                  "100Gi",
			containerFsStats:              "10Gi",
			evictionMessage:               "The node was low on resource: ephemeral-storage. Threshold quantity: 50Gi, available: 10Gi.Container above-requests was using 80Gi, request is 50Gi, has larger consumption of ephemeral-storage. ",
			thresholdToMonitor: evictionapi.Threshold{
				Signal:   evictionapi.SignalContainerFsAvailable,
				Operator: evictionapi.OpLessThan,
				Value: evictionapi.ThresholdValue{
					Quantity: quantityMustParse("50Gi"),
				},
			},
			podToMakes: []podToMake{
				{name: "below-requests", requests: newResourceList("", "", "1Gi"), limits: newResourceList("", "", "1Gi"), rootFsUsed: "900Mi"},
				{name: "above-requests", requests: newResourceList("", "", "50Gi"), limits: newResourceList("", "", "50Gi"), rootFsUsed: "80Gi"},
			},
		},
		"eviction due to container disk pressure; container fs": {
			dedicatedImageFs:              ptr.To(true),
			kubeletSeparateDiskFeature:    true,
			writeableSeparateFromReadOnly: true,
			nodeFsStats:                   "10Gi",
			imageFsStats:                  "100Gi",
			containerFsStats:              "10Gi",
			evictionMessage:               "The node was low on resource: ephemeral-storage. Threshold quantity: 50Gi, available: 10Gi. Container above-requests was using 80Gi, request is 50Gi, has larger consumption of ephemeral-storage. ",
			thresholdToMonitor: evictionapi.Threshold{
				Signal:   evictionapi.SignalNodeFsAvailable,
				Operator: evictionapi.OpLessThan,
				Value: evictionapi.ThresholdValue{
					Quantity: quantityMustParse("50Gi"),
				},
			},
			podToMakes: []podToMake{
				{name: "below-requests", requests: newResourceList("", "", "1Gi"), limits: newResourceList("", "", "1Gi"), rootFsUsed: "900Mi"},
				{name: "above-requests", requests: newResourceList("", "", "50Gi"), limits: newResourceList("", "", "50Gi"), rootFsUsed: "80Gi"},
			},
		},
	}
	for _, tc := range testCases {
		featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.KubeletSeparateDiskGC, tc.kubeletSeparateDiskFeature)

		podMaker := makePodWithDiskStats
		summaryStatsMaker := makeDiskStats
		podsToMake := tc.podToMakes
		wantPodStatus := v1.PodStatus{
			Phase:   v1.PodFailed,
			Reason:  "Evicted",
			Message: tc.evictionMessage,
		}
		pods := []*v1.Pod{}
		podStats := map[*v1.Pod]statsapi.PodStats{}
		for _, podToMake := range podsToMake {
			pod, podStat := podMaker(podToMake.name, podToMake.priority, podToMake.requests, podToMake.limits, podToMake.rootFsUsed, podToMake.logsFsUsed, podToMake.perLocalVolumeUsed, nil)
			pods = append(pods, pod)
			podStats[pod] = podStat
		}
		activePodsFunc := func() []*v1.Pod {
			return pods
		}

		fakeClock := testingclock.NewFakeClock(time.Now())
		podKiller := &mockPodKiller{}
		diskInfoProvider := &mockDiskInfoProvider{dedicatedImageFs: tc.dedicatedImageFs, dedicatedContainerFs: &tc.writeableSeparateFromReadOnly}
		diskGC := &mockDiskGC{err: nil}
		nodeRef := &v1.ObjectReference{Kind: "Node", Name: "test", UID: types.UID("test"), Namespace: ""}

		config := Config{
			PressureTransitionPeriod: time.Minute * 5,
			Thresholds:               []evictionapi.Threshold{tc.thresholdToMonitor},
		}
		diskStat := diskStats{
			rootFsAvailableBytes:      tc.nodeFsStats,
			imageFsAvailableBytes:     tc.imageFsStats,
			containerFsAvailableBytes: tc.containerFsStats,
			podStats:                  podStats,
		}
		summaryProvider := &fakeSummaryProvider{result: summaryStatsMaker(diskStat)}
		manager := &managerImpl{
			clock:                        fakeClock,
			killPodFunc:                  podKiller.killPodNow,
			imageGC:                      diskGC,
			containerGC:                  diskGC,
			config:                       config,
			recorder:                     &record.FakeRecorder{},
			summaryProvider:              summaryProvider,
			nodeRef:                      nodeRef,
			nodeConditionsLastObservedAt: nodeConditionsObservedAt{},
			thresholdsFirstObservedAt:    thresholdsObservedAt{},
		}

		// synchronize
		pods, synchErr := manager.synchronize(diskInfoProvider, activePodsFunc)

		if synchErr == nil && tc.expectErr != "" {
			t.Fatalf("Manager should report error but did not")
		} else if tc.expectErr != "" && synchErr != nil {
			if diff := cmp.Diff(tc.expectErr, synchErr.Error()); diff != "" {
				t.Errorf("Unexpected error (-want,+got):\n%s", diff)
			}
		} else {
			// verify manager detected disk pressure
			if !manager.IsUnderDiskPressure() {
				t.Fatalf("Manager should report disk pressure")
			}

			// verify a pod is selected for eviction
			if podKiller.pod == nil {
				t.Fatalf("Manager should have selected a pod for eviction")
			}

			wantPodStatus.Conditions = append(wantPodStatus.Conditions, v1.PodCondition{
				Type:    "DisruptionTarget",
				Status:  "True",
				Reason:  "TerminationByKubelet",
				Message: tc.evictionMessage,
			})

			// verify the pod status after applying the status update function
			podKiller.statusFn(&podKiller.pod.Status)
			if diff := cmp.Diff(wantPodStatus, podKiller.pod.Status, cmpopts.IgnoreFields(v1.PodCondition{}, "LastProbeTime", "LastTransitionTime")); diff != "" {
				t.Errorf("Unexpected pod status of the evicted pod (-want,+got):\n%s", diff)
			}
		}
	}
}

// TestMemoryPressure
func TestMemoryPressure(t *testing.T) {
	podMaker := makePodWithMemoryStats
	summaryStatsMaker := makeMemoryStats
	podsToMake := []podToMake{
		{name: "guaranteed-low-priority-high-usage", priority: lowPriority, requests: newResourceList("100m", "1Gi", ""), limits: newResourceList("100m", "1Gi", ""), memoryWorkingSet: "900Mi"},
		{name: "burstable-below-requests", priority: defaultPriority, requests: newResourceList("100m", "100Mi", ""), limits: newResourceList("200m", "1Gi", ""), memoryWorkingSet: "50Mi"},
		{name: "burstable-above-requests", priority: defaultPriority, requests: newResourceList("100m", "100Mi", ""), limits: newResourceList("200m", "1Gi", ""), memoryWorkingSet: "400Mi"},
		{name: "best-effort-high-priority-high-usage", priority: highPriority, requests: newResourceList("", "", ""), limits: newResourceList("", "", ""), memoryWorkingSet: "400Mi"},
		{name: "best-effort-low-priority-low-usage", priority: lowPriority, requests: newResourceList("", "", ""), limits: newResourceList("", "", ""), memoryWorkingSet: "100Mi"},
	}
	pods := []*v1.Pod{}
	podStats := map[*v1.Pod]statsapi.PodStats{}
	for _, podToMake := range podsToMake {
		pod, podStat := podMaker(podToMake.name, podToMake.priority, podToMake.requests, podToMake.limits, podToMake.memoryWorkingSet)
		pods = append(pods, pod)
		podStats[pod] = podStat
	}
	podToEvict := pods[4]
	activePodsFunc := func() []*v1.Pod {
		return pods
	}

	fakeClock := testingclock.NewFakeClock(time.Now())
	podKiller := &mockPodKiller{}
	diskInfoProvider := &mockDiskInfoProvider{dedicatedImageFs: ptr.To(false)}
	diskGC := &mockDiskGC{err: nil}
	nodeRef := &v1.ObjectReference{Kind: "Node", Name: "test", UID: types.UID("test"), Namespace: ""}

	config := Config{
		MaxPodGracePeriodSeconds: 5,
		PressureTransitionPeriod: time.Minute * 5,
		Thresholds: []evictionapi.Threshold{
			{
				Signal:   evictionapi.SignalMemoryAvailable,
				Operator: evictionapi.OpLessThan,
				Value: evictionapi.ThresholdValue{
					Quantity: quantityMustParse("1Gi"),
				},
			},
			{
				Signal:   evictionapi.SignalMemoryAvailable,
				Operator: evictionapi.OpLessThan,
				Value: evictionapi.ThresholdValue{
					Quantity: quantityMustParse("2Gi"),
				},
				GracePeriod: time.Minute * 2,
			},
		},
	}
	summaryProvider := &fakeSummaryProvider{result: summaryStatsMaker("2Gi", podStats)}
	manager := &managerImpl{
		clock:                        fakeClock,
		killPodFunc:                  podKiller.killPodNow,
		imageGC:                      diskGC,
		containerGC:                  diskGC,
		config:                       config,
		recorder:                     &record.FakeRecorder{},
		summaryProvider:              summaryProvider,
		nodeRef:                      nodeRef,
		nodeConditionsLastObservedAt: nodeConditionsObservedAt{},
		thresholdsFirstObservedAt:    thresholdsObservedAt{},
	}

	// create a best effort pod to test admission
	bestEffortPodToAdmit, _ := podMaker("best-admit", defaultPriority, newResourceList("", "", ""), newResourceList("", "", ""), "0Gi")
	burstablePodToAdmit, _ := podMaker("burst-admit", defaultPriority, newResourceList("100m", "100Mi", ""), newResourceList("200m", "200Mi", ""), "0Gi")

	// synchronize
	_, err := manager.synchronize(diskInfoProvider, activePodsFunc)

	if err != nil {
		t.Fatalf("Manager expects no error but got %v", err)
	}

	// we should not have memory pressure
	if manager.IsUnderMemoryPressure() {
		t.Errorf("Manager should not report memory pressure")
	}

	// try to admit our pods (they should succeed)
	expected := []bool{true, true}
	for i, pod := range []*v1.Pod{bestEffortPodToAdmit, burstablePodToAdmit} {
		if result := manager.Admit(&lifecycle.PodAdmitAttributes{Pod: pod}); expected[i] != result.Admit {
			t.Errorf("Admit pod: %v, expected: %v, actual: %v", pod, expected[i], result.Admit)
		}
	}

	// induce soft threshold
	fakeClock.Step(1 * time.Minute)
	summaryProvider.result = summaryStatsMaker("1500Mi", podStats)
	_, err = manager.synchronize(diskInfoProvider, activePodsFunc)

	if err != nil {
		t.Fatalf("Manager expects no error but got %v", err)
	}

	// we should have memory pressure
	if !manager.IsUnderMemoryPressure() {
		t.Errorf("Manager should report memory pressure since soft threshold was met")
	}

	// verify no pod was yet killed because there has not yet been enough time passed.
	if podKiller.pod != nil {
		t.Errorf("Manager should not have killed a pod yet, but killed: %v", podKiller.pod.Name)
	}

	// step forward in time pass the grace period
	fakeClock.Step(3 * time.Minute)
	summaryProvider.result = summaryStatsMaker("1500Mi", podStats)
	_, err = manager.synchronize(diskInfoProvider, activePodsFunc)

	if err != nil {
		t.Fatalf("Manager expects no error but got %v", err)
	}

	// we should have memory pressure
	if !manager.IsUnderMemoryPressure() {
		t.Errorf("Manager should report memory pressure since soft threshold was met")
	}

	// verify the right pod was killed with the right grace period.
	if podKiller.pod != podToEvict {
		t.Errorf("Manager chose to kill pod: %v, but should have chosen %v", podKiller.pod.Name, podToEvict.Name)
	}
	if podKiller.gracePeriodOverride == nil {
		t.Errorf("Manager chose to kill pod but should have had a grace period override.")
	}
	observedGracePeriod := *podKiller.gracePeriodOverride
	if observedGracePeriod != manager.config.MaxPodGracePeriodSeconds {
		t.Errorf("Manager chose to kill pod with incorrect grace period.  Expected: %d, actual: %d", manager.config.MaxPodGracePeriodSeconds, observedGracePeriod)
	}
	// reset state
	podKiller.pod = nil
	podKiller.gracePeriodOverride = nil

	// remove memory pressure
	fakeClock.Step(20 * time.Minute)
	summaryProvider.result = summaryStatsMaker("3Gi", podStats)
	_, err = manager.synchronize(diskInfoProvider, activePodsFunc)

	if err != nil {
		t.Fatalf("Manager expects no error but got %v", err)
	}

	// we should not have memory pressure
	if manager.IsUnderMemoryPressure() {
		t.Errorf("Manager should not report memory pressure")
	}

	// induce memory pressure!
	fakeClock.Step(1 * time.Minute)
	summaryProvider.result = summaryStatsMaker("500Mi", podStats)
	_, err = manager.synchronize(diskInfoProvider, activePodsFunc)

	if err != nil {
		t.Fatalf("Manager expects no error but got %v", err)
	}

	// we should have memory pressure
	if !manager.IsUnderMemoryPressure() {
		t.Errorf("Manager should report memory pressure")
	}

	// check the right pod was killed
	if podKiller.pod != podToEvict {
		t.Errorf("Manager chose to kill pod: %v, but should have chosen %v", podKiller.pod.Name, podToEvict.Name)
	}
	observedGracePeriod = *podKiller.gracePeriodOverride
	if observedGracePeriod != int64(1) {
		t.Errorf("Manager chose to kill pod with incorrect grace period.  Expected: %d, actual: %d", 1, observedGracePeriod)
	}

	// the best-effort pod should not admit, burstable should
	expected = []bool{false, true}
	for i, pod := range []*v1.Pod{bestEffortPodToAdmit, burstablePodToAdmit} {
		if result := manager.Admit(&lifecycle.PodAdmitAttributes{Pod: pod}); expected[i] != result.Admit {
			t.Errorf("Admit pod: %v, expected: %v, actual: %v", pod, expected[i], result.Admit)
		}
	}

	// reduce memory pressure
	fakeClock.Step(1 * time.Minute)
	summaryProvider.result = summaryStatsMaker("2Gi", podStats)
	podKiller.pod = nil // reset state
	_, err = manager.synchronize(diskInfoProvider, activePodsFunc)

	if err != nil {
		t.Fatalf("Manager expects no error but got %v", err)
	}

	// we should have memory pressure (because transition period not yet met)
	if !manager.IsUnderMemoryPressure() {
		t.Errorf("Manager should report memory pressure")
	}

	// no pod should have been killed
	if podKiller.pod != nil {
		t.Errorf("Manager chose to kill pod: %v when no pod should have been killed", podKiller.pod.Name)
	}

	// the best-effort pod should not admit, burstable should
	expected = []bool{false, true}
	for i, pod := range []*v1.Pod{bestEffortPodToAdmit, burstablePodToAdmit} {
		if result := manager.Admit(&lifecycle.PodAdmitAttributes{Pod: pod}); expected[i] != result.Admit {
			t.Errorf("Admit pod: %v, expected: %v, actual: %v", pod, expected[i], result.Admit)
		}
	}

	// move the clock past transition period to ensure that we stop reporting pressure
	fakeClock.Step(5 * time.Minute)
	summaryProvider.result = summaryStatsMaker("2Gi", podStats)
	podKiller.pod = nil // reset state
	_, err = manager.synchronize(diskInfoProvider, activePodsFunc)

	if err != nil {
		t.Fatalf("Manager expects no error but got %v", err)
	}

	// we should not have memory pressure (because transition period met)
	if manager.IsUnderMemoryPressure() {
		t.Errorf("Manager should not report memory pressure")
	}

	// no pod should have been killed
	if podKiller.pod != nil {
		t.Errorf("Manager chose to kill pod: %v when no pod should have been killed", podKiller.pod.Name)
	}

	// all pods should admit now
	expected = []bool{true, true}
	for i, pod := range []*v1.Pod{bestEffortPodToAdmit, burstablePodToAdmit} {
		if result := manager.Admit(&lifecycle.PodAdmitAttributes{Pod: pod}); expected[i] != result.Admit {
			t.Errorf("Admit pod: %v, expected: %v, actual: %v", pod, expected[i], result.Admit)
		}
	}
}

func makeContainersByQOS(class v1.PodQOSClass) []v1.Container {
	resource := newResourceList("100m", "1Gi", "")
	switch class {
	case v1.PodQOSGuaranteed:
		return []v1.Container{newContainer("guaranteed-container", resource, resource)}
	case v1.PodQOSBurstable:
		return []v1.Container{newContainer("burtable-container", resource, nil)}
	case v1.PodQOSBestEffort:
		fallthrough
	default:
		return []v1.Container{newContainer("best-effort-container", nil, nil)}
	}
}

func TestPIDPressure(t *testing.T) {
	testCases := []struct {
		name                               string
		podsToMake                         []podToMake
		evictPodIndex                      int
		noPressurePIDUsage                 string
		pressurePIDUsageWithGracePeriod    string
		pressurePIDUsageWithoutGracePeriod string
		totalPID                           string
	}{
		{
			name: "eviction due to pid pressure",
			podsToMake: []podToMake{
				{name: "high-priority-high-usage", priority: highPriority, pidUsage: 900},
				{name: "default-priority-low-usage", priority: defaultPriority, pidUsage: 100},
				{name: "default-priority-medium-usage", priority: defaultPriority, pidUsage: 400},
				{name: "low-priority-high-usage", priority: lowPriority, pidUsage: 600},
				{name: "low-priority-low-usage", priority: lowPriority, pidUsage: 50},
			},
			evictPodIndex:                      3, // we expect the low-priority-high-usage pod to be evicted
			noPressurePIDUsage:                 "300",
			pressurePIDUsageWithGracePeriod:    "700",
			pressurePIDUsageWithoutGracePeriod: "1200",
			totalPID:                           "2000",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			podMaker := makePodWithPIDStats
			summaryStatsMaker := makePIDStats
			pods := []*v1.Pod{}
			podStats := map[*v1.Pod]statsapi.PodStats{}
			for _, podToMake := range tc.podsToMake {
				pod, podStat := podMaker(podToMake.name, podToMake.priority, podToMake.pidUsage)
				pods = append(pods, pod)
				podStats[pod] = podStat
			}
			podToEvict := pods[tc.evictPodIndex]
			activePodsFunc := func() []*v1.Pod { return pods }

			fakeClock := testingclock.NewFakeClock(time.Now())
			podKiller := &mockPodKiller{}
			diskInfoProvider := &mockDiskInfoProvider{dedicatedImageFs: ptr.To(false)}
			diskGC := &mockDiskGC{err: nil}
			nodeRef := &v1.ObjectReference{Kind: "Node", Name: "test", UID: types.UID("test"), Namespace: ""}

			config := Config{
				MaxPodGracePeriodSeconds: 5,
				PressureTransitionPeriod: time.Minute * 5,
				Thresholds: []evictionapi.Threshold{
					{
						Signal:   evictionapi.SignalPIDAvailable,
						Operator: evictionapi.OpLessThan,
						Value: evictionapi.ThresholdValue{
							Quantity: quantityMustParse("1200"),
						},
					},
					{
						Signal:   evictionapi.SignalPIDAvailable,
						Operator: evictionapi.OpLessThan,
						Value: evictionapi.ThresholdValue{
							Quantity: quantityMustParse("1500"),
						},
						GracePeriod: time.Minute * 2,
					},
				},
			}

			summaryProvider := &fakeSummaryProvider{result: summaryStatsMaker(tc.totalPID, tc.noPressurePIDUsage, podStats)}
			manager := &managerImpl{
				clock:                        fakeClock,
				killPodFunc:                  podKiller.killPodNow,
				imageGC:                      diskGC,
				containerGC:                  diskGC,
				config:                       config,
				recorder:                     &record.FakeRecorder{},
				summaryProvider:              summaryProvider,
				nodeRef:                      nodeRef,
				nodeConditionsLastObservedAt: nodeConditionsObservedAt{},
				thresholdsFirstObservedAt:    thresholdsObservedAt{},
			}

			// create a pod to test admission
			podToAdmit, _ := podMaker("pod-to-admit", defaultPriority, 50)

			// synchronize
			_, err := manager.synchronize(diskInfoProvider, activePodsFunc)

			if err != nil {
				t.Fatalf("Manager expects no error but got %v", err)
			}

			// we should not have PID pressure
			if manager.IsUnderPIDPressure() {
				t.Fatalf("Manager should not report PID pressure")
			}

			// try to admit our pod (should succeed)
			if result := manager.Admit(&lifecycle.PodAdmitAttributes{Pod: podToAdmit}); !result.Admit {
				t.Fatalf("Admit pod: %v, expected: %v, actual: %v", podToAdmit, true, result.Admit)
			}

			// induce soft threshold for PID pressure
			fakeClock.Step(1 * time.Minute)
			summaryProvider.result = summaryStatsMaker(tc.totalPID, tc.pressurePIDUsageWithGracePeriod, podStats)
			_, err = manager.synchronize(diskInfoProvider, activePodsFunc)

			if err != nil {
				t.Fatalf("Manager expects no error but got %v", err)
			}

			// now, we should have PID pressure
			if !manager.IsUnderPIDPressure() {
				t.Errorf("Manager should report PID pressure since soft threshold was met")
			}

			// verify no pod was yet killed because there has not yet been enough time passed
			if podKiller.pod != nil {
				t.Errorf("Manager should not have killed a pod yet, but killed: %v", podKiller.pod.Name)
			}

			// step forward in time past the grace period
			fakeClock.Step(3 * time.Minute)
			// no change in PID stats to simulate continued pressure
			_, err = manager.synchronize(diskInfoProvider, activePodsFunc)

			if err != nil {
				t.Fatalf("Manager expects no error but got %v", err)
			}

			// verify PID pressure is still reported
			if !manager.IsUnderPIDPressure() {
				t.Errorf("Manager should still report PID pressure")
			}

			// verify the right pod was killed with the right grace period.
			if podKiller.pod != podToEvict {
				t.Errorf("Manager chose to kill pod: %v, but should have chosen %v", podKiller.pod.Name, podToEvict.Name)
			}
			if podKiller.gracePeriodOverride == nil {
				t.Errorf("Manager chose to kill pod but should have had a grace period override.")
			}
			observedGracePeriod := *podKiller.gracePeriodOverride
			if observedGracePeriod != manager.config.MaxPodGracePeriodSeconds {
				t.Errorf("Manager chose to kill pod with incorrect grace period.  Expected: %d, actual: %d", manager.config.MaxPodGracePeriodSeconds, observedGracePeriod)
			}

			// reset state
			podKiller.pod = nil
			podKiller.gracePeriodOverride = nil

			// remove PID pressure by simulating increased PID availability
			fakeClock.Step(20 * time.Minute)
			summaryProvider.result = summaryStatsMaker(tc.totalPID, tc.noPressurePIDUsage, podStats) // Simulate increased PID availability
			_, err = manager.synchronize(diskInfoProvider, activePodsFunc)

			if err != nil {
				t.Fatalf("Manager expects no error but got %v", err)
			}

			// verify PID pressure is resolved
			if manager.IsUnderPIDPressure() {
				t.Errorf("Manager should not report PID pressure")
			}

			// re-induce PID pressure
			fakeClock.Step(1 * time.Minute)
			summaryProvider.result = summaryStatsMaker(tc.totalPID, tc.pressurePIDUsageWithoutGracePeriod, podStats)
			_, err = manager.synchronize(diskInfoProvider, activePodsFunc)

			if err != nil {
				t.Fatalf("Manager expects no error but got %v", err)
			}

			// verify PID pressure is reported again
			if !manager.IsUnderPIDPressure() {
				t.Errorf("Manager should report PID pressure")
			}

			// verify the right pod was killed with the right grace period.
			if podKiller.pod != podToEvict {
				t.Errorf("Manager chose to kill pod: %v, but should have chosen %v", podKiller.pod.Name, podToEvict.Name)
			}
			if podKiller.gracePeriodOverride == nil {
				t.Errorf("Manager chose to kill pod but should have had a grace period override.")
			}
			observedGracePeriod = *podKiller.gracePeriodOverride
			if observedGracePeriod != int64(1) {
				t.Errorf("Manager chose to kill pod with incorrect grace period.  Expected: %d, actual: %d", 1, observedGracePeriod)
			}

			// try to admit our pod (should fail)
			if result := manager.Admit(&lifecycle.PodAdmitAttributes{Pod: podToAdmit}); result.Admit {
				t.Fatalf("Admit pod: %v, expected: %v, actual: %v", podToAdmit, false, result.Admit)
			}

			// reduce PID pressure
			fakeClock.Step(1 * time.Minute)
			summaryProvider.result = summaryStatsMaker(tc.totalPID, tc.noPressurePIDUsage, podStats)
			podKiller.pod = nil // reset state
			_, err = manager.synchronize(diskInfoProvider, activePodsFunc)

			if err != nil {
				t.Fatalf("Manager expects no error but got %v", err)
			}

			// we should have PID pressure (because transition period not yet met)
			if !manager.IsUnderPIDPressure() {
				t.Errorf("Manager should report PID pressure")
			}

			// no pod should have been killed
			if podKiller.pod != nil {
				t.Errorf("Manager chose to kill pod: %v when no pod should have been killed", podKiller.pod.Name)
			}

			// try to admit our pod (should fail)
			if result := manager.Admit(&lifecycle.PodAdmitAttributes{Pod: podToAdmit}); result.Admit {
				t.Fatalf("Admit pod: %v, expected: %v, actual: %v", podToAdmit, false, result.Admit)
			}

			// move the clock past the transition period
			fakeClock.Step(5 * time.Minute)
			summaryProvider.result = summaryStatsMaker(tc.totalPID, tc.noPressurePIDUsage, podStats)
			_, err = manager.synchronize(diskInfoProvider, activePodsFunc)

			if err != nil {
				t.Fatalf("Manager expects no error but got %v", err)
			}

			// we should not have PID pressure (because transition period met)
			if manager.IsUnderPIDPressure() {
				t.Errorf("Manager should not report PID pressure")
			}

			// no pod should have been killed
			if podKiller.pod != nil {
				t.Errorf("Manager chose to kill pod: %v when no pod should have been killed", podKiller.pod.Name)
			}

			// try to admit our pod (should succeed)
			if result := manager.Admit(&lifecycle.PodAdmitAttributes{Pod: podToAdmit}); !result.Admit {
				t.Fatalf("Admit pod: %v, expected: %v, actual: %v", podToAdmit, true, result.Admit)
			}
		})
	}
}

func TestAdmitUnderNodeConditions(t *testing.T) {
	manager := &managerImpl{}
	pods := []*v1.Pod{
		newPod("guaranteed-pod", scheduling.DefaultPriorityWhenNoDefaultClassExists, makeContainersByQOS(v1.PodQOSGuaranteed), nil),
		newPod("burstable-pod", scheduling.DefaultPriorityWhenNoDefaultClassExists, makeContainersByQOS(v1.PodQOSBurstable), nil),
		newPod("best-effort-pod", scheduling.DefaultPriorityWhenNoDefaultClassExists, makeContainersByQOS(v1.PodQOSBestEffort), nil),
	}

	expected := []bool{true, true, true}
	for i, pod := range pods {
		if result := manager.Admit(&lifecycle.PodAdmitAttributes{Pod: pod}); expected[i] != result.Admit {
			t.Errorf("Admit pod: %v, expected: %v, actual: %v", pod, expected[i], result.Admit)
		}
	}

	manager.nodeConditions = []v1.NodeConditionType{v1.NodeMemoryPressure}
	expected = []bool{true, true, false}
	for i, pod := range pods {
		if result := manager.Admit(&lifecycle.PodAdmitAttributes{Pod: pod}); expected[i] != result.Admit {
			t.Errorf("Admit pod: %v, expected: %v, actual: %v", pod, expected[i], result.Admit)
		}
	}

	manager.nodeConditions = []v1.NodeConditionType{v1.NodeMemoryPressure, v1.NodeDiskPressure}
	expected = []bool{false, false, false}
	for i, pod := range pods {
		if result := manager.Admit(&lifecycle.PodAdmitAttributes{Pod: pod}); expected[i] != result.Admit {
			t.Errorf("Admit pod: %v, expected: %v, actual: %v", pod, expected[i], result.Admit)
		}
	}
}

// parseQuantity parses the specified value (if provided) otherwise returns 0 value
func parseQuantity(value string) resource.Quantity {
	if len(value) == 0 {
		return resource.MustParse("0")
	}
	return resource.MustParse(value)
}

func TestDiskPressureNodeFs(t *testing.T) {

	testCases := map[string]struct {
		nodeFsStats                   string
		imageFsStats                  string
		containerFsStats              string
		kubeletSeparateDiskFeature    bool
		writeableSeparateFromReadOnly bool
		thresholdToMonitor            []evictionapi.Threshold
		podToMakes                    []podToMake
		dedicatedImageFs              *bool
		expectErr                     string
		inducePressureOnWhichFs       string
		softDiskPressure              string
		hardDiskPressure              string
	}{
		"eviction due to disk pressure; no image fs": {
			dedicatedImageFs:        ptr.To(false),
			nodeFsStats:             "16Gi",
			imageFsStats:            "16Gi",
			containerFsStats:        "16Gi",
			inducePressureOnWhichFs: "nodefs",
			softDiskPressure:        "1.5Gi",
			hardDiskPressure:        "750Mi",
			thresholdToMonitor: []evictionapi.Threshold{
				{
					Signal:   evictionapi.SignalNodeFsAvailable,
					Operator: evictionapi.OpLessThan,
					Value: evictionapi.ThresholdValue{
						Quantity: quantityMustParse("1Gi"),
					},
				},
				{
					Signal:   evictionapi.SignalNodeFsAvailable,
					Operator: evictionapi.OpLessThan,
					Value: evictionapi.ThresholdValue{
						Quantity: quantityMustParse("2Gi"),
					},
					GracePeriod: time.Minute * 2,
				},
			},
			podToMakes: []podToMake{
				{name: "low-priority-high-usage", priority: lowPriority, requests: newResourceList("100m", "1Gi", ""), limits: newResourceList("100m", "1Gi", ""), rootFsUsed: "900Mi"},
				{name: "below-requests", priority: defaultPriority, requests: newResourceList("100m", "100Mi", ""), limits: newResourceList("200m", "1Gi", ""), logsFsUsed: "50Mi"},
				{name: "above-requests", priority: defaultPriority, requests: newResourceList("100m", "100Mi", ""), limits: newResourceList("200m", "1Gi", ""), rootFsUsed: "400Mi"},
				{name: "high-priority-high-usage", priority: highPriority, requests: newResourceList("", "", ""), limits: newResourceList("", "", ""), perLocalVolumeUsed: "400Mi"},
				{name: "low-priority-low-usage", priority: lowPriority, requests: newResourceList("", "", ""), limits: newResourceList("", "", ""), rootFsUsed: "100Mi"},
			},
		},
		"eviction due to image disk pressure; image fs": {
			dedicatedImageFs:        ptr.To(true),
			nodeFsStats:             "16Gi",
			imageFsStats:            "16Gi",
			containerFsStats:        "16Gi",
			softDiskPressure:        "1.5Gi",
			hardDiskPressure:        "750Mi",
			inducePressureOnWhichFs: "imagefs",
			thresholdToMonitor: []evictionapi.Threshold{
				{
					Signal:   evictionapi.SignalImageFsAvailable,
					Operator: evictionapi.OpLessThan,
					Value: evictionapi.ThresholdValue{
						Quantity: quantityMustParse("1Gi"),
					},
				},
				{
					Signal:   evictionapi.SignalImageFsAvailable,
					Operator: evictionapi.OpLessThan,
					Value: evictionapi.ThresholdValue{
						Quantity: quantityMustParse("2Gi"),
					},
					GracePeriod: time.Minute * 2,
				},
			},
			podToMakes: []podToMake{
				{name: "low-priority-high-usage", priority: lowPriority, requests: newResourceList("100m", "1Gi", ""), limits: newResourceList("100m", "1Gi", ""), rootFsUsed: "900Mi"},
				{name: "below-requests", priority: defaultPriority, requests: newResourceList("100m", "100Mi", ""), limits: newResourceList("200m", "1Gi", ""), logsFsUsed: "50Mi"},
				{name: "above-requests", priority: defaultPriority, requests: newResourceList("100m", "100Mi", ""), limits: newResourceList("200m", "1Gi", ""), rootFsUsed: "400Mi"},
				{name: "high-priority-high-usage", priority: highPriority, requests: newResourceList("", "", ""), limits: newResourceList("", "", ""), perLocalVolumeUsed: "400Mi"},
				{name: "low-priority-low-usage", priority: lowPriority, requests: newResourceList("", "", ""), limits: newResourceList("", "", ""), rootFsUsed: "100Mi"},
			},
		},
		"eviction due to container disk pressure; container fs": {
			dedicatedImageFs:              ptr.To(true),
			kubeletSeparateDiskFeature:    true,
			writeableSeparateFromReadOnly: true,
			nodeFsStats:                   "16Gi",
			imageFsStats:                  "16Gi",
			containerFsStats:              "16Gi",
			softDiskPressure:              "1.5Gi",
			hardDiskPressure:              "750Mi",
			inducePressureOnWhichFs:       "containerfs",
			thresholdToMonitor: []evictionapi.Threshold{
				{
					Signal:   evictionapi.SignalNodeFsAvailable,
					Operator: evictionapi.OpLessThan,
					Value: evictionapi.ThresholdValue{
						Quantity: quantityMustParse("1Gi"),
					},
				},
				{
					Signal:   evictionapi.SignalNodeFsAvailable,
					Operator: evictionapi.OpLessThan,
					Value: evictionapi.ThresholdValue{
						Quantity: quantityMustParse("2Gi"),
					},
					GracePeriod: time.Minute * 2,
				},
			},
			podToMakes: []podToMake{
				{name: "low-priority-high-usage", priority: lowPriority, requests: newResourceList("100m", "1Gi", ""), limits: newResourceList("100m", "1Gi", ""), rootFsUsed: "900Mi"},
				{name: "below-requests", priority: defaultPriority, requests: newResourceList("100m", "100Mi", ""), limits: newResourceList("200m", "1Gi", ""), logsFsUsed: "50Mi"},
				{name: "above-requests", priority: defaultPriority, requests: newResourceList("100m", "100Mi", ""), limits: newResourceList("200m", "1Gi", ""), rootFsUsed: "400Mi"},
				{name: "high-priority-high-usage", priority: highPriority, requests: newResourceList("", "", ""), limits: newResourceList("", "", ""), perLocalVolumeUsed: "400Mi"},
				{name: "low-priority-low-usage", priority: lowPriority, requests: newResourceList("", "", ""), limits: newResourceList("", "", ""), rootFsUsed: "100Mi"},
			},
		},
	}

	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.KubeletSeparateDiskGC, tc.kubeletSeparateDiskFeature)

			podMaker := makePodWithDiskStats
			summaryStatsMaker := makeDiskStats
			podsToMake := tc.podToMakes
			pods := []*v1.Pod{}
			podStats := map[*v1.Pod]statsapi.PodStats{}
			for _, podToMake := range podsToMake {
				pod, podStat := podMaker(podToMake.name, podToMake.priority, podToMake.requests, podToMake.limits, podToMake.rootFsUsed, podToMake.logsFsUsed, podToMake.perLocalVolumeUsed, nil)
				pods = append(pods, pod)
				podStats[pod] = podStat
			}
			podToEvict := pods[0]
			activePodsFunc := func() []*v1.Pod {
				return pods
			}

			fakeClock := testingclock.NewFakeClock(time.Now())
			podKiller := &mockPodKiller{}
			diskInfoProvider := &mockDiskInfoProvider{dedicatedImageFs: tc.dedicatedImageFs, dedicatedContainerFs: &tc.writeableSeparateFromReadOnly}
			diskGC := &mockDiskGC{err: nil}
			nodeRef := &v1.ObjectReference{Kind: "Node", Name: "test", UID: types.UID("test"), Namespace: ""}

			config := Config{
				MaxPodGracePeriodSeconds: 5,
				PressureTransitionPeriod: time.Minute * 5,
				Thresholds:               tc.thresholdToMonitor,
			}

			diskStatStart := diskStats{
				rootFsAvailableBytes:      tc.nodeFsStats,
				imageFsAvailableBytes:     tc.imageFsStats,
				containerFsAvailableBytes: tc.containerFsStats,
				podStats:                  podStats,
			}
			diskStatConst := diskStatStart
			summaryProvider := &fakeSummaryProvider{result: summaryStatsMaker(diskStatStart)}
			manager := &managerImpl{
				clock:                        fakeClock,
				killPodFunc:                  podKiller.killPodNow,
				imageGC:                      diskGC,
				containerGC:                  diskGC,
				config:                       config,
				recorder:                     &record.FakeRecorder{},
				summaryProvider:              summaryProvider,
				nodeRef:                      nodeRef,
				nodeConditionsLastObservedAt: nodeConditionsObservedAt{},
				thresholdsFirstObservedAt:    thresholdsObservedAt{},
			}

			// create a best effort pod to test admission
			podToAdmit, _ := podMaker("pod-to-admit", defaultPriority, newResourceList("", "", ""), newResourceList("", "", ""), "0Gi", "0Gi", "0Gi", nil)

			// synchronize
			_, err := manager.synchronize(diskInfoProvider, activePodsFunc)

			if err != nil {
				t.Fatalf("Manager expects no error but got %v", err)
			}

			// we should not have disk pressure
			if manager.IsUnderDiskPressure() {
				t.Fatalf("Manager should not report disk pressure")
			}

			// try to admit our pod (should succeed)
			if result := manager.Admit(&lifecycle.PodAdmitAttributes{Pod: podToAdmit}); !result.Admit {
				t.Fatalf("Admit pod: %v, expected: %v, actual: %v", podToAdmit, true, result.Admit)
			}

			// induce soft threshold
			fakeClock.Step(1 * time.Minute)

			if tc.inducePressureOnWhichFs == "nodefs" {
				diskStatStart.rootFsAvailableBytes = tc.softDiskPressure
			} else if tc.inducePressureOnWhichFs == "imagefs" {
				diskStatStart.imageFsAvailableBytes = tc.softDiskPressure
			} else if tc.inducePressureOnWhichFs == "containerfs" {
				diskStatStart.containerFsAvailableBytes = tc.softDiskPressure
			}
			summaryProvider.result = summaryStatsMaker(diskStatStart)
			_, err = manager.synchronize(diskInfoProvider, activePodsFunc)

			if err != nil {
				t.Fatalf("Manager expects no error but got %v", err)
			}

			// we should have disk pressure
			if !manager.IsUnderDiskPressure() {
				t.Fatalf("Manager should report disk pressure since soft threshold was met")
			}

			// verify no pod was yet killed because there has not yet been enough time passed.
			if podKiller.pod != nil {
				t.Fatalf("Manager should not have killed a pod yet, but killed: %v", podKiller.pod.Name)
			}

			// step forward in time pass the grace period
			fakeClock.Step(3 * time.Minute)
			summaryProvider.result = summaryStatsMaker(diskStatStart)
			_, err = manager.synchronize(diskInfoProvider, activePodsFunc)

			if err != nil {
				t.Fatalf("Manager expects no error but got %v", err)
			}

			// we should have disk pressure
			if !manager.IsUnderDiskPressure() {
				t.Fatalf("Manager should report disk pressure since soft threshold was met")
			}

			// verify the right pod was killed with the right grace period.
			if podKiller.pod != podToEvict {
				t.Fatalf("Manager chose to kill pod: %v, but should have chosen %v", podKiller.pod.Name, podToEvict.Name)
			}
			if podKiller.gracePeriodOverride == nil {
				t.Fatalf("Manager chose to kill pod but should have had a grace period override.")
			}
			observedGracePeriod := *podKiller.gracePeriodOverride
			if observedGracePeriod != manager.config.MaxPodGracePeriodSeconds {
				t.Fatalf("Manager chose to kill pod with incorrect grace period.  Expected: %d, actual: %d", manager.config.MaxPodGracePeriodSeconds, observedGracePeriod)
			}
			// reset state
			podKiller.pod = nil
			podKiller.gracePeriodOverride = nil

			// remove disk pressure
			fakeClock.Step(20 * time.Minute)
			summaryProvider.result = summaryStatsMaker(diskStatConst)
			_, err = manager.synchronize(diskInfoProvider, activePodsFunc)

			if err != nil {
				t.Fatalf("Manager expects no error but got %v", err)
			}

			// we should not have disk pressure
			if manager.IsUnderDiskPressure() {
				t.Fatalf("Manager should not report disk pressure")
			}

			// induce disk pressure!
			fakeClock.Step(1 * time.Minute)
			if tc.inducePressureOnWhichFs == "nodefs" {
				diskStatStart.rootFsAvailableBytes = tc.hardDiskPressure
			} else if tc.inducePressureOnWhichFs == "imagefs" {
				diskStatStart.imageFsAvailableBytes = tc.hardDiskPressure
			} else if tc.inducePressureOnWhichFs == "containerfs" {
				diskStatStart.containerFsAvailableBytes = tc.hardDiskPressure
			}
			summaryProvider.result = summaryStatsMaker(diskStatStart)
			_, err = manager.synchronize(diskInfoProvider, activePodsFunc)

			if err != nil {
				t.Fatalf("Manager expects no error but got %v", err)
			}

			// we should have disk pressure
			if !manager.IsUnderDiskPressure() {
				t.Fatalf("Manager should report disk pressure")
			}

			// check the right pod was killed
			if podKiller.pod != podToEvict {
				t.Fatalf("Manager chose to kill pod: %v, but should have chosen %v", podKiller.pod.Name, podToEvict.Name)
			}
			observedGracePeriod = *podKiller.gracePeriodOverride
			if observedGracePeriod != int64(1) {
				t.Fatalf("Manager chose to kill pod with incorrect grace period.  Expected: %d, actual: %d", 1, observedGracePeriod)
			}

			// try to admit our pod (should fail)
			if result := manager.Admit(&lifecycle.PodAdmitAttributes{Pod: podToAdmit}); result.Admit {
				t.Fatalf("Admit pod: %v, expected: %v, actual: %v", podToAdmit, false, result.Admit)
			}

			// reduce disk pressure
			fakeClock.Step(1 * time.Minute)

			summaryProvider.result = summaryStatsMaker(diskStatConst)
			podKiller.pod = nil // reset state
			_, err = manager.synchronize(diskInfoProvider, activePodsFunc)

			if err != nil {
				t.Fatalf("Manager should not have an error %v", err)
			}
			// we should have disk pressure (because transition period not yet met)
			if !manager.IsUnderDiskPressure() {
				t.Fatalf("Manager should report disk pressure")
			}

			// no pod should have been killed
			if podKiller.pod != nil {
				t.Fatalf("Manager chose to kill pod: %v when no pod should have been killed", podKiller.pod.Name)
			}

			// try to admit our pod (should fail)
			if result := manager.Admit(&lifecycle.PodAdmitAttributes{Pod: podToAdmit}); result.Admit {
				t.Fatalf("Admit pod: %v, expected: %v, actual: %v", podToAdmit, false, result.Admit)
			}

			// move the clock past transition period to ensure that we stop reporting pressure
			fakeClock.Step(5 * time.Minute)
			summaryProvider.result = summaryStatsMaker(diskStatConst)
			podKiller.pod = nil // reset state
			_, err = manager.synchronize(diskInfoProvider, activePodsFunc)

			if err != nil {
				t.Fatalf("Manager should not have an error %v", err)
			}

			// we should not have disk pressure (because transition period met)
			if manager.IsUnderDiskPressure() {
				t.Fatalf("Manager should not report disk pressure")
			}

			// no pod should have been killed
			if podKiller.pod != nil {
				t.Fatalf("Manager chose to kill pod: %v when no pod should have been killed", podKiller.pod.Name)
			}

			// try to admit our pod (should succeed)
			if result := manager.Admit(&lifecycle.PodAdmitAttributes{Pod: podToAdmit}); !result.Admit {
				t.Fatalf("Admit pod: %v, expected: %v, actual: %v", podToAdmit, true, result.Admit)
			}
		})
	}
}

// TestMinReclaim verifies that min-reclaim works as desired.
func TestMinReclaim(t *testing.T) {
	podMaker := makePodWithMemoryStats
	summaryStatsMaker := makeMemoryStats
	podsToMake := []podToMake{
		{name: "guaranteed-low-priority-high-usage", priority: lowPriority, requests: newResourceList("100m", "1Gi", ""), limits: newResourceList("100m", "1Gi", ""), memoryWorkingSet: "900Mi"},
		{name: "burstable-below-requests", priority: defaultPriority, requests: newResourceList("100m", "100Mi", ""), limits: newResourceList("200m", "1Gi", ""), memoryWorkingSet: "50Mi"},
		{name: "burstable-above-requests", priority: defaultPriority, requests: newResourceList("100m", "100Mi", ""), limits: newResourceList("200m", "1Gi", ""), memoryWorkingSet: "400Mi"},
		{name: "best-effort-high-priority-high-usage", priority: highPriority, requests: newResourceList("", "", ""), limits: newResourceList("", "", ""), memoryWorkingSet: "400Mi"},
		{name: "best-effort-low-priority-low-usage", priority: lowPriority, requests: newResourceList("", "", ""), limits: newResourceList("", "", ""), memoryWorkingSet: "100Mi"},
	}
	pods := []*v1.Pod{}
	podStats := map[*v1.Pod]statsapi.PodStats{}
	for _, podToMake := range podsToMake {
		pod, podStat := podMaker(podToMake.name, podToMake.priority, podToMake.requests, podToMake.limits, podToMake.memoryWorkingSet)
		pods = append(pods, pod)
		podStats[pod] = podStat
	}
	podToEvict := pods[4]
	activePodsFunc := func() []*v1.Pod {
		return pods
	}

	fakeClock := testingclock.NewFakeClock(time.Now())
	podKiller := &mockPodKiller{}
	diskInfoProvider := &mockDiskInfoProvider{dedicatedImageFs: ptr.To(false)}
	diskGC := &mockDiskGC{err: nil}
	nodeRef := &v1.ObjectReference{Kind: "Node", Name: "test", UID: types.UID("test"), Namespace: ""}

	config := Config{
		MaxPodGracePeriodSeconds: 5,
		PressureTransitionPeriod: time.Minute * 5,
		Thresholds: []evictionapi.Threshold{
			{
				Signal:   evictionapi.SignalMemoryAvailable,
				Operator: evictionapi.OpLessThan,
				Value: evictionapi.ThresholdValue{
					Quantity: quantityMustParse("1Gi"),
				},
				MinReclaim: &evictionapi.ThresholdValue{
					Quantity: quantityMustParse("500Mi"),
				},
			},
		},
	}
	summaryProvider := &fakeSummaryProvider{result: summaryStatsMaker("2Gi", podStats)}
	manager := &managerImpl{
		clock:                        fakeClock,
		killPodFunc:                  podKiller.killPodNow,
		imageGC:                      diskGC,
		containerGC:                  diskGC,
		config:                       config,
		recorder:                     &record.FakeRecorder{},
		summaryProvider:              summaryProvider,
		nodeRef:                      nodeRef,
		nodeConditionsLastObservedAt: nodeConditionsObservedAt{},
		thresholdsFirstObservedAt:    thresholdsObservedAt{},
	}

	// synchronize
	_, err := manager.synchronize(diskInfoProvider, activePodsFunc)
	if err != nil {
		t.Errorf("Manager should not report any errors")
	}
	// we should not have memory pressure
	if manager.IsUnderMemoryPressure() {
		t.Errorf("Manager should not report memory pressure")
	}

	// induce memory pressure!
	fakeClock.Step(1 * time.Minute)
	summaryProvider.result = summaryStatsMaker("500Mi", podStats)
	_, err = manager.synchronize(diskInfoProvider, activePodsFunc)

	if err != nil {
		t.Fatalf("Manager should not have an error %v", err)
	}

	// we should have memory pressure
	if !manager.IsUnderMemoryPressure() {
		t.Errorf("Manager should report memory pressure")
	}

	// check the right pod was killed
	if podKiller.pod != podToEvict {
		t.Errorf("Manager chose to kill pod: %v, but should have chosen %v", podKiller.pod.Name, podToEvict.Name)
	}
	observedGracePeriod := *podKiller.gracePeriodOverride
	if observedGracePeriod != int64(1) {
		t.Errorf("Manager chose to kill pod with incorrect grace period.  Expected: %d, actual: %d", 1, observedGracePeriod)
	}

	// reduce memory pressure, but not below the min-reclaim amount
	fakeClock.Step(1 * time.Minute)
	summaryProvider.result = summaryStatsMaker("1.2Gi", podStats)
	podKiller.pod = nil // reset state
	_, err = manager.synchronize(diskInfoProvider, activePodsFunc)

	if err != nil {
		t.Fatalf("Manager should not have an error %v", err)
	}

	// we should have memory pressure (because transition period not yet met)
	if !manager.IsUnderMemoryPressure() {
		t.Errorf("Manager should report memory pressure")
	}

	// check the right pod was killed
	if podKiller.pod != podToEvict {
		t.Errorf("Manager chose to kill pod: %v, but should have chosen %v", podKiller.pod.Name, podToEvict.Name)
	}
	observedGracePeriod = *podKiller.gracePeriodOverride
	if observedGracePeriod != int64(1) {
		t.Errorf("Manager chose to kill pod with incorrect grace period.  Expected: %d, actual: %d", 1, observedGracePeriod)
	}

	// reduce memory pressure and ensure the min-reclaim amount
	fakeClock.Step(1 * time.Minute)
	summaryProvider.result = summaryStatsMaker("2Gi", podStats)
	podKiller.pod = nil // reset state
	_, err = manager.synchronize(diskInfoProvider, activePodsFunc)

	if err != nil {
		t.Fatalf("Manager should not have an error %v", err)
	}

	// we should have memory pressure (because transition period not yet met)
	if !manager.IsUnderMemoryPressure() {
		t.Errorf("Manager should report memory pressure")
	}

	// no pod should have been killed
	if podKiller.pod != nil {
		t.Errorf("Manager chose to kill pod: %v when no pod should have been killed", podKiller.pod.Name)
	}

	// move the clock past transition period to ensure that we stop reporting pressure
	fakeClock.Step(5 * time.Minute)
	summaryProvider.result = summaryStatsMaker("2Gi", podStats)
	podKiller.pod = nil // reset state
	_, err = manager.synchronize(diskInfoProvider, activePodsFunc)

	if err != nil {
		t.Fatalf("Manager should not have an error %v", err)
	}

	// we should not have memory pressure (because transition period met)
	if manager.IsUnderMemoryPressure() {
		t.Errorf("Manager should not report memory pressure")
	}

	// no pod should have been killed
	if podKiller.pod != nil {
		t.Errorf("Manager chose to kill pod: %v when no pod should have been killed", podKiller.pod.Name)
	}
}

func TestNodeReclaimFuncs(t *testing.T) {
	testCases := map[string]struct {
		nodeFsStats                   string
		imageFsStats                  string
		containerFsStats              string
		kubeletSeparateDiskFeature    bool
		writeableSeparateFromReadOnly bool
		expectContainerGcCall         bool
		expectImageGcCall             bool
		thresholdToMonitor            evictionapi.Threshold
		podToMakes                    []podToMake
		dedicatedImageFs              *bool
		expectErr                     string
		inducePressureOnWhichFs       string
		softDiskPressure              string
		hardDiskPressure              string
	}{
		"eviction due to disk pressure; no image fs": {
			dedicatedImageFs:        ptr.To(false),
			nodeFsStats:             "16Gi",
			imageFsStats:            "16Gi",
			containerFsStats:        "16Gi",
			inducePressureOnWhichFs: "nodefs",
			softDiskPressure:        "1.5Gi",
			hardDiskPressure:        "750Mi",
			expectContainerGcCall:   true,
			expectImageGcCall:       true,
			thresholdToMonitor: evictionapi.Threshold{
				Signal:   evictionapi.SignalNodeFsAvailable,
				Operator: evictionapi.OpLessThan,
				Value: evictionapi.ThresholdValue{
					Quantity: quantityMustParse("1Gi"),
				},
				MinReclaim: &evictionapi.ThresholdValue{
					Quantity: quantityMustParse("500Mi"),
				},
			},
			podToMakes: []podToMake{
				{name: "low-priority-high-usage", priority: lowPriority, requests: newResourceList("100m", "1Gi", ""), limits: newResourceList("100m", "1Gi", ""), rootFsUsed: "900Mi"},
				{name: "below-requests", priority: defaultPriority, requests: newResourceList("100m", "100Mi", ""), limits: newResourceList("200m", "1Gi", ""), logsFsUsed: "50Mi"},
				{name: "above-requests", priority: defaultPriority, requests: newResourceList("100m", "100Mi", ""), limits: newResourceList("200m", "1Gi", ""), rootFsUsed: "400Mi"},
				{name: "high-priority-high-usage", priority: highPriority, requests: newResourceList("", "", ""), limits: newResourceList("", "", ""), perLocalVolumeUsed: "400Mi"},
				{name: "low-priority-low-usage", priority: lowPriority, requests: newResourceList("", "", ""), limits: newResourceList("", "", ""), rootFsUsed: "100Mi"},
			},
		},
		"eviction due to image disk pressure; image fs": {
			dedicatedImageFs:        ptr.To(true),
			nodeFsStats:             "16Gi",
			imageFsStats:            "16Gi",
			containerFsStats:        "16Gi",
			softDiskPressure:        "1.5Gi",
			hardDiskPressure:        "750Mi",
			inducePressureOnWhichFs: "imagefs",
			expectContainerGcCall:   true,
			expectImageGcCall:       true,
			thresholdToMonitor: evictionapi.Threshold{
				Signal:   evictionapi.SignalImageFsAvailable,
				Operator: evictionapi.OpLessThan,
				Value: evictionapi.ThresholdValue{
					Quantity: quantityMustParse("1Gi"),
				},
				MinReclaim: &evictionapi.ThresholdValue{
					Quantity: quantityMustParse("500Mi"),
				},
			},
			podToMakes: []podToMake{
				{name: "low-priority-high-usage", priority: lowPriority, requests: newResourceList("100m", "1Gi", ""), limits: newResourceList("100m", "1Gi", ""), rootFsUsed: "900Mi"},
				{name: "below-requests", priority: defaultPriority, requests: newResourceList("100m", "100Mi", ""), limits: newResourceList("200m", "1Gi", ""), logsFsUsed: "50Mi"},
				{name: "above-requests", priority: defaultPriority, requests: newResourceList("100m", "100Mi", ""), limits: newResourceList("200m", "1Gi", ""), rootFsUsed: "400Mi"},
				{name: "high-priority-high-usage", priority: highPriority, requests: newResourceList("", "", ""), limits: newResourceList("", "", ""), perLocalVolumeUsed: "400Mi"},
				{name: "low-priority-low-usage", priority: lowPriority, requests: newResourceList("", "", ""), limits: newResourceList("", "", ""), rootFsUsed: "100Mi"},
			},
		},
		"eviction due to container disk pressure; container fs": {
			dedicatedImageFs:              ptr.To(true),
			kubeletSeparateDiskFeature:    true,
			writeableSeparateFromReadOnly: true,
			nodeFsStats:                   "16Gi",
			imageFsStats:                  "16Gi",
			containerFsStats:              "16Gi",
			softDiskPressure:              "1.5Gi",
			hardDiskPressure:              "750Mi",
			inducePressureOnWhichFs:       "nodefs",
			expectContainerGcCall:         true,
			expectImageGcCall:             false,
			thresholdToMonitor: evictionapi.Threshold{
				Signal:   evictionapi.SignalNodeFsAvailable,
				Operator: evictionapi.OpLessThan,
				Value: evictionapi.ThresholdValue{
					Quantity: quantityMustParse("1Gi"),
				},
				MinReclaim: &evictionapi.ThresholdValue{
					Quantity: quantityMustParse("500Mi"),
				},
			},
			podToMakes: []podToMake{
				{name: "low-priority-high-usage", priority: lowPriority, requests: newResourceList("100m", "1Gi", ""), limits: newResourceList("100m", "1Gi", ""), rootFsUsed: "900Mi"},
				{name: "below-requests", priority: defaultPriority, requests: newResourceList("100m", "100Mi", ""), limits: newResourceList("200m", "1Gi", ""), logsFsUsed: "50Mi"},
				{name: "above-requests", priority: defaultPriority, requests: newResourceList("100m", "100Mi", ""), limits: newResourceList("200m", "1Gi", ""), rootFsUsed: "400Mi"},
				{name: "high-priority-high-usage", priority: highPriority, requests: newResourceList("", "", ""), limits: newResourceList("", "", ""), perLocalVolumeUsed: "400Mi"},
				{name: "low-priority-low-usage", priority: lowPriority, requests: newResourceList("", "", ""), limits: newResourceList("", "", ""), rootFsUsed: "100Mi"},
			},
		},
		"eviction due to image disk pressure; container fs": {
			dedicatedImageFs:              ptr.To(true),
			kubeletSeparateDiskFeature:    true,
			writeableSeparateFromReadOnly: true,
			nodeFsStats:                   "16Gi",
			imageFsStats:                  "16Gi",
			containerFsStats:              "16Gi",
			softDiskPressure:              "1.5Gi",
			hardDiskPressure:              "750Mi",
			inducePressureOnWhichFs:       "imagefs",
			expectContainerGcCall:         false,
			expectImageGcCall:             true,
			thresholdToMonitor: evictionapi.Threshold{
				Signal:   evictionapi.SignalImageFsAvailable,
				Operator: evictionapi.OpLessThan,
				Value: evictionapi.ThresholdValue{
					Quantity: quantityMustParse("1Gi"),
				},
				MinReclaim: &evictionapi.ThresholdValue{
					Quantity: quantityMustParse("500Mi"),
				},
			},
			podToMakes: []podToMake{
				{name: "low-priority-high-usage", priority: lowPriority, requests: newResourceList("100m", "1Gi", ""), limits: newResourceList("100m", "1Gi", ""), rootFsUsed: "900Mi"},
				{name: "below-requests", priority: defaultPriority, requests: newResourceList("100m", "100Mi", ""), limits: newResourceList("200m", "1Gi", ""), logsFsUsed: "50Mi"},
				{name: "above-requests", priority: defaultPriority, requests: newResourceList("100m", "100Mi", ""), limits: newResourceList("200m", "1Gi", ""), rootFsUsed: "400Mi"},
				{name: "high-priority-high-usage", priority: highPriority, requests: newResourceList("", "", ""), limits: newResourceList("", "", ""), perLocalVolumeUsed: "400Mi"},
				{name: "low-priority-low-usage", priority: lowPriority, requests: newResourceList("", "", ""), limits: newResourceList("", "", ""), rootFsUsed: "100Mi"},
			},
		},
	}

	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.KubeletSeparateDiskGC, tc.kubeletSeparateDiskFeature)

			podMaker := makePodWithDiskStats
			summaryStatsMaker := makeDiskStats
			podsToMake := tc.podToMakes
			pods := []*v1.Pod{}
			podStats := map[*v1.Pod]statsapi.PodStats{}
			for _, podToMake := range podsToMake {
				pod, podStat := podMaker(podToMake.name, podToMake.priority, podToMake.requests, podToMake.limits, podToMake.rootFsUsed, podToMake.logsFsUsed, podToMake.perLocalVolumeUsed, nil)
				pods = append(pods, pod)
				podStats[pod] = podStat
			}
			podToEvict := pods[0]
			activePodsFunc := func() []*v1.Pod {
				return pods
			}

			fakeClock := testingclock.NewFakeClock(time.Now())
			podKiller := &mockPodKiller{}
			diskInfoProvider := &mockDiskInfoProvider{dedicatedImageFs: tc.dedicatedImageFs, dedicatedContainerFs: &tc.writeableSeparateFromReadOnly}
			nodeRef := &v1.ObjectReference{Kind: "Node", Name: "test", UID: types.UID("test"), Namespace: ""}

			config := Config{
				MaxPodGracePeriodSeconds: 5,
				PressureTransitionPeriod: time.Minute * 5,
				Thresholds:               []evictionapi.Threshold{tc.thresholdToMonitor},
			}
			diskStatStart := diskStats{
				rootFsAvailableBytes:      tc.nodeFsStats,
				imageFsAvailableBytes:     tc.imageFsStats,
				containerFsAvailableBytes: tc.containerFsStats,
				podStats:                  podStats,
			}
			// This is a constant that we use to test that disk pressure is over. Don't change!
			diskStatConst := diskStatStart
			summaryProvider := &fakeSummaryProvider{result: summaryStatsMaker(diskStatStart)}
			diskGC := &mockDiskGC{fakeSummaryProvider: summaryProvider, err: nil}
			manager := &managerImpl{
				clock:                        fakeClock,
				killPodFunc:                  podKiller.killPodNow,
				imageGC:                      diskGC,
				containerGC:                  diskGC,
				config:                       config,
				recorder:                     &record.FakeRecorder{},
				summaryProvider:              summaryProvider,
				nodeRef:                      nodeRef,
				nodeConditionsLastObservedAt: nodeConditionsObservedAt{},
				thresholdsFirstObservedAt:    thresholdsObservedAt{},
			}

			// synchronize
			_, err := manager.synchronize(diskInfoProvider, activePodsFunc)

			if err != nil {
				t.Fatalf("Manager should not have an error %v", err)
			}

			// we should not have disk pressure
			if manager.IsUnderDiskPressure() {
				t.Errorf("Manager should not report disk pressure")
			}

			// induce hard threshold
			fakeClock.Step(1 * time.Minute)

			setDiskStatsBasedOnFs := func(whichFs string, diskPressure string, diskStat diskStats) diskStats {
				if tc.inducePressureOnWhichFs == "nodefs" {
					diskStat.rootFsAvailableBytes = diskPressure
				} else if tc.inducePressureOnWhichFs == "imagefs" {
					diskStat.imageFsAvailableBytes = diskPressure
				} else if tc.inducePressureOnWhichFs == "containerfs" {
					diskStat.containerFsAvailableBytes = diskPressure
				}
				return diskStat
			}
			newDiskAfterHardEviction := setDiskStatsBasedOnFs(tc.inducePressureOnWhichFs, tc.hardDiskPressure, diskStatStart)
			summaryProvider.result = summaryStatsMaker(newDiskAfterHardEviction)
			// make GC successfully return disk usage to previous levels
			diskGC.summaryAfterGC = summaryStatsMaker(diskStatConst)
			_, err = manager.synchronize(diskInfoProvider, activePodsFunc)

			if err != nil {
				t.Fatalf("Manager should not have an error %v", err)
			}

			// we should have disk pressure
			if !manager.IsUnderDiskPressure() {
				t.Fatalf("Manager should report disk pressure since soft threshold was met")
			}

			// verify image, container or both gc were called.
			// split filesystem can have container gc called without image.
			// same filesystem should have both.
			if diskGC.imageGCInvoked != tc.expectImageGcCall && diskGC.containerGCInvoked != tc.expectContainerGcCall {
				t.Fatalf("Manager should have invoked image gc")
			}

			// verify no pod was killed because image gc was sufficient
			if podKiller.pod != nil {
				t.Fatalf("Manager should not have killed a pod, but killed: %v", podKiller.pod.Name)
			}

			// reset state
			diskGC.imageGCInvoked = false
			diskGC.containerGCInvoked = false

			// remove disk pressure
			fakeClock.Step(20 * time.Minute)
			summaryProvider.result = summaryStatsMaker(diskStatConst)
			_, err = manager.synchronize(diskInfoProvider, activePodsFunc)

			if err != nil {
				t.Fatalf("Manager should not have an error %v", err)
			}

			// we should not have disk pressure
			if manager.IsUnderDiskPressure() {
				t.Fatalf("Manager should not report disk pressure")
			}

			// synchronize
			_, err = manager.synchronize(diskInfoProvider, activePodsFunc)

			if err != nil {
				t.Fatalf("Manager should not have an error %v", err)
			}

			// we should not have disk pressure
			if manager.IsUnderDiskPressure() {
				t.Fatalf("Manager should not report disk pressure")
			}

			// induce hard threshold
			fakeClock.Step(1 * time.Minute)
			newDiskAfterHardEviction = setDiskStatsBasedOnFs(tc.inducePressureOnWhichFs, tc.hardDiskPressure, diskStatStart)
			summaryProvider.result = summaryStatsMaker(newDiskAfterHardEviction)
			// make GC return disk usage bellow the threshold, but not satisfying minReclaim
			gcBelowThreshold := setDiskStatsBasedOnFs(tc.inducePressureOnWhichFs, "1.1G", newDiskAfterHardEviction)
			diskGC.summaryAfterGC = summaryStatsMaker(gcBelowThreshold)
			_, err = manager.synchronize(diskInfoProvider, activePodsFunc)

			if err != nil {
				t.Fatalf("Manager should not have an error %v", err)
			}

			// we should have disk pressure
			if !manager.IsUnderDiskPressure() {
				t.Fatalf("Manager should report disk pressure since soft threshold was met")
			}

			// verify image, container or both gc were called.
			// split filesystem can have container gc called without image.
			// same filesystem should have both.
			if diskGC.imageGCInvoked != tc.expectImageGcCall && diskGC.containerGCInvoked != tc.expectContainerGcCall {
				t.Fatalf("Manager should have invoked image gc")
			}

			// verify a pod was killed because image gc was not enough to satisfy minReclaim
			if podKiller.pod == nil {
				t.Fatalf("Manager should have killed a pod, but didn't")
			}

			// reset state
			diskGC.imageGCInvoked = false
			diskGC.containerGCInvoked = false
			podKiller.pod = nil

			// remove disk pressure
			fakeClock.Step(20 * time.Minute)
			summaryProvider.result = summaryStatsMaker(diskStatConst)
			_, err = manager.synchronize(diskInfoProvider, activePodsFunc)

			if err != nil {
				t.Fatalf("Manager should not have an error %v", err)
			}

			// we should not have disk pressure
			if manager.IsUnderDiskPressure() {
				t.Fatalf("Manager should not report disk pressure")
			}

			// induce disk pressure!
			fakeClock.Step(1 * time.Minute)
			softDiskPressure := setDiskStatsBasedOnFs(tc.inducePressureOnWhichFs, tc.hardDiskPressure, diskStatStart)
			summaryProvider.result = summaryStatsMaker(softDiskPressure)
			// Don't reclaim any disk
			diskGC.summaryAfterGC = summaryStatsMaker(softDiskPressure)
			_, err = manager.synchronize(diskInfoProvider, activePodsFunc)

			if err != nil {
				t.Fatalf("Manager should not have an error %v", err)
			}

			// we should have disk pressure
			if !manager.IsUnderDiskPressure() {
				t.Fatalf("Manager should report disk pressure")
			}

			// verify image, container or both gc were called.
			// split filesystem can have container gc called without image.
			// same filesystem should have both.
			if diskGC.imageGCInvoked != tc.expectImageGcCall && diskGC.containerGCInvoked != tc.expectContainerGcCall {
				t.Fatalf("Manager should have invoked image gc")
			}

			// check the right pod was killed
			if podKiller.pod != podToEvict {
				t.Fatalf("Manager chose to kill pod: %v, but should have chosen %v", podKiller.pod.Name, podToEvict.Name)
			}
			observedGracePeriod := *podKiller.gracePeriodOverride
			if observedGracePeriod != int64(1) {
				t.Fatalf("Manager chose to kill pod with incorrect grace period.  Expected: %d, actual: %d", 1, observedGracePeriod)
			}

			// reduce disk pressure
			fakeClock.Step(1 * time.Minute)
			summaryProvider.result = summaryStatsMaker(diskStatConst)
			diskGC.imageGCInvoked = false     // reset state
			diskGC.containerGCInvoked = false // reset state
			podKiller.pod = nil               // reset state
			_, err = manager.synchronize(diskInfoProvider, activePodsFunc)

			if err != nil {
				t.Fatalf("Manager should not have an error %v", err)
			}

			// we should have disk pressure (because transition period not yet met)
			if !manager.IsUnderDiskPressure() {
				t.Fatalf("Manager should report disk pressure")
			}

			if diskGC.imageGCInvoked || diskGC.containerGCInvoked {
				t.Errorf("Manager chose to perform image gc when it was not needed")
			}

			// no pod should have been killed
			if podKiller.pod != nil {
				t.Fatalf("Manager chose to kill pod: %v when no pod should have been killed", podKiller.pod.Name)
			}

			// move the clock past transition period to ensure that we stop reporting pressure
			fakeClock.Step(5 * time.Minute)
			summaryProvider.result = summaryStatsMaker(diskStatConst)
			diskGC.imageGCInvoked = false     // reset state
			diskGC.containerGCInvoked = false // reset state
			podKiller.pod = nil               // reset state
			_, err = manager.synchronize(diskInfoProvider, activePodsFunc)

			if err != nil {
				t.Fatalf("Manager should not have an error %v", err)
			}

			// we should not have disk pressure (because transition period met)
			if manager.IsUnderDiskPressure() {
				t.Fatalf("Manager should not report disk pressure")
			}

			if diskGC.imageGCInvoked || diskGC.containerGCInvoked {
				t.Errorf("Manager chose to perform image gc when it was not needed")
			}

			// no pod should have been killed
			if podKiller.pod != nil {
				t.Fatalf("Manager chose to kill pod: %v when no pod should have been killed", podKiller.pod.Name)
			}
		})
	}
}

func TestInodePressureFsInodes(t *testing.T) {
	podMaker := func(name string, priority int32, requests v1.ResourceList, limits v1.ResourceList, rootInodes, logInodes, volumeInodes string) (*v1.Pod, statsapi.PodStats) {
		pod := newPod(name, priority, []v1.Container{
			newContainer(name, requests, limits),
		}, nil)
		podStats := newPodInodeStats(pod, parseQuantity(rootInodes), parseQuantity(logInodes), parseQuantity(volumeInodes))
		return pod, podStats
	}
	summaryStatsMaker := func(rootFsInodesFree, rootFsInodes, imageFsInodesFree, imageFsInodes, containerFsInodesFree, containerFsInodes string, podStats map[*v1.Pod]statsapi.PodStats) *statsapi.Summary {
		rootFsInodesFreeVal := resource.MustParse(rootFsInodesFree)
		internalRootFsInodesFree := uint64(rootFsInodesFreeVal.Value())
		rootFsInodesVal := resource.MustParse(rootFsInodes)
		internalRootFsInodes := uint64(rootFsInodesVal.Value())

		imageFsInodesFreeVal := resource.MustParse(imageFsInodesFree)
		internalImageFsInodesFree := uint64(imageFsInodesFreeVal.Value())
		imageFsInodesVal := resource.MustParse(imageFsInodes)
		internalImageFsInodes := uint64(imageFsInodesVal.Value())

		containerFsInodesFreeVal := resource.MustParse(containerFsInodesFree)
		internalContainerFsInodesFree := uint64(containerFsInodesFreeVal.Value())
		containerFsInodesVal := resource.MustParse(containerFsInodes)
		internalContainerFsInodes := uint64(containerFsInodesVal.Value())

		result := &statsapi.Summary{
			Node: statsapi.NodeStats{
				Fs: &statsapi.FsStats{
					InodesFree: &internalRootFsInodesFree,
					Inodes:     &internalRootFsInodes,
				},
				Runtime: &statsapi.RuntimeStats{
					ImageFs: &statsapi.FsStats{
						InodesFree: &internalImageFsInodesFree,
						Inodes:     &internalImageFsInodes,
					},
					ContainerFs: &statsapi.FsStats{
						InodesFree: &internalContainerFsInodesFree,
						Inodes:     &internalContainerFsInodes,
					},
				},
			},
			Pods: []statsapi.PodStats{},
		}
		for _, podStat := range podStats {
			result.Pods = append(result.Pods, podStat)
		}
		return result
	}

	setINodesFreeBasedOnFs := func(whichFs string, inodesFree string, diskStat *statsapi.Summary) *statsapi.Summary {
		inodesFreeVal := resource.MustParse(inodesFree)
		internalFsInodesFree := uint64(inodesFreeVal.Value())

		if whichFs == "nodefs" {
			diskStat.Node.Fs.InodesFree = &internalFsInodesFree
		} else if whichFs == "imagefs" {
			diskStat.Node.Runtime.ImageFs.InodesFree = &internalFsInodesFree
		} else if whichFs == "containerfs" {
			diskStat.Node.Runtime.ContainerFs.InodesFree = &internalFsInodesFree
		}
		return diskStat
	}

	testCases := map[string]struct {
		nodeFsInodesFree              string
		nodeFsInodes                  string
		imageFsInodesFree             string
		imageFsInodes                 string
		containerFsInodesFree         string
		containerFsInodes             string
		kubeletSeparateDiskFeature    bool
		writeableSeparateFromReadOnly bool
		thresholdToMonitor            []evictionapi.Threshold
		podToMakes                    []podToMake
		dedicatedImageFs              *bool
		expectErr                     string
		inducePressureOnWhichFs       string
		softINodePressure             string
		hardINodePressure             string
	}{
		"eviction due to disk pressure; no image fs": {
			dedicatedImageFs:        ptr.To(false),
			nodeFsInodesFree:        "3Mi",
			nodeFsInodes:            "4Mi",
			imageFsInodesFree:       "3Mi",
			imageFsInodes:           "4Mi",
			containerFsInodesFree:   "3Mi",
			containerFsInodes:       "4Mi",
			inducePressureOnWhichFs: "nodefs",
			softINodePressure:       "1.5Mi",
			hardINodePressure:       "0.5Mi",
			thresholdToMonitor: []evictionapi.Threshold{
				{
					Signal:   evictionapi.SignalNodeFsInodesFree,
					Operator: evictionapi.OpLessThan,
					Value: evictionapi.ThresholdValue{
						Quantity: quantityMustParse("1Mi"),
					},
				},
				{
					Signal:   evictionapi.SignalNodeFsInodesFree,
					Operator: evictionapi.OpLessThan,
					Value: evictionapi.ThresholdValue{
						Quantity: quantityMustParse("2Mi"),
					},
					GracePeriod: time.Minute * 2,
				},
			},
			podToMakes: []podToMake{
				{name: "low-priority-high-usage", priority: lowPriority, requests: newResourceList("100m", "1Gi", ""), limits: newResourceList("100m", "1Gi", ""), rootFsInodesUsed: "900Mi"},
				{name: "below-requests", priority: defaultPriority, requests: newResourceList("100m", "100Mi", ""), limits: newResourceList("200m", "1Gi", ""), rootFsInodesUsed: "50Mi"},
				{name: "above-requests", priority: defaultPriority, requests: newResourceList("100m", "100Mi", ""), limits: newResourceList("200m", "1Gi", ""), rootFsInodesUsed: "400Mi"},
				{name: "high-priority-high-usage", priority: highPriority, requests: newResourceList("", "", ""), limits: newResourceList("", "", ""), rootFsInodesUsed: "400Mi"},
				{name: "low-priority-low-usage", priority: lowPriority, requests: newResourceList("", "", ""), limits: newResourceList("", "", ""), rootFsInodesUsed: "100Mi"},
			},
		},
		"eviction due to image disk pressure; image fs": {
			dedicatedImageFs:        ptr.To(true),
			nodeFsInodesFree:        "3Mi",
			nodeFsInodes:            "4Mi",
			imageFsInodesFree:       "3Mi",
			imageFsInodes:           "4Mi",
			containerFsInodesFree:   "3Mi",
			containerFsInodes:       "4Mi",
			softINodePressure:       "1.5Mi",
			hardINodePressure:       "0.5Mi",
			inducePressureOnWhichFs: "imagefs",
			thresholdToMonitor: []evictionapi.Threshold{
				{
					Signal:   evictionapi.SignalImageFsInodesFree,
					Operator: evictionapi.OpLessThan,
					Value: evictionapi.ThresholdValue{
						Quantity: quantityMustParse("1Mi"),
					},
				},
				{
					Signal:   evictionapi.SignalImageFsInodesFree,
					Operator: evictionapi.OpLessThan,
					Value: evictionapi.ThresholdValue{
						Quantity: quantityMustParse("2Mi"),
					},
					GracePeriod: time.Minute * 2,
				},
			},
			podToMakes: []podToMake{
				{name: "low-priority-high-usage", priority: lowPriority, requests: newResourceList("100m", "1Gi", ""), limits: newResourceList("100m", "1Gi", ""), rootFsInodesUsed: "900Mi"},
				{name: "below-requests", priority: defaultPriority, requests: newResourceList("100m", "100Mi", ""), limits: newResourceList("200m", "1Gi", ""), rootFsInodesUsed: "50Mi"},
				{name: "above-requests", priority: defaultPriority, requests: newResourceList("100m", "100Mi", ""), limits: newResourceList("200m", "1Gi", ""), rootFsInodesUsed: "400Mi"},
				{name: "high-priority-high-usage", priority: highPriority, requests: newResourceList("", "", ""), limits: newResourceList("", "", ""), rootFsInodesUsed: "400Mi"},
				{name: "low-priority-low-usage", priority: lowPriority, requests: newResourceList("", "", ""), limits: newResourceList("", "", ""), rootFsInodesUsed: "100Mi"},
			},
		},
		"eviction due to container disk pressure; container fs": {
			dedicatedImageFs:              ptr.To(true),
			kubeletSeparateDiskFeature:    true,
			writeableSeparateFromReadOnly: true,
			nodeFsInodesFree:              "3Mi",
			nodeFsInodes:                  "4Mi",
			imageFsInodesFree:             "3Mi",
			imageFsInodes:                 "4Mi",
			containerFsInodesFree:         "3Mi",
			containerFsInodes:             "4Mi",
			softINodePressure:             "1.5Mi",
			hardINodePressure:             "0.5Mi",
			inducePressureOnWhichFs:       "nodefs",
			thresholdToMonitor: []evictionapi.Threshold{
				{
					Signal:   evictionapi.SignalNodeFsInodesFree,
					Operator: evictionapi.OpLessThan,
					Value: evictionapi.ThresholdValue{
						Quantity: quantityMustParse("1Mi"),
					},
				},
				{
					Signal:   evictionapi.SignalNodeFsInodesFree,
					Operator: evictionapi.OpLessThan,
					Value: evictionapi.ThresholdValue{
						Quantity: quantityMustParse("2Mi"),
					},
					GracePeriod: time.Minute * 2,
				},
			},
			podToMakes: []podToMake{
				{name: "low-priority-high-usage", priority: lowPriority, requests: newResourceList("100m", "1Gi", ""), limits: newResourceList("100m", "1Gi", ""), rootFsInodesUsed: "900Mi"},
				{name: "below-requests", priority: defaultPriority, requests: newResourceList("100m", "100Mi", ""), limits: newResourceList("200m", "1Gi", ""), rootFsInodesUsed: "50Mi"},
				{name: "above-requests", priority: defaultPriority, requests: newResourceList("100m", "100Mi", ""), limits: newResourceList("200m", "1Gi", ""), rootFsInodesUsed: "400Mi"},
				{name: "high-priority-high-usage", priority: highPriority, requests: newResourceList("", "", ""), limits: newResourceList("", "", ""), rootFsInodesUsed: "400Mi"},
				{name: "low-priority-low-usage", priority: lowPriority, requests: newResourceList("", "", ""), limits: newResourceList("", "", ""), rootFsInodesUsed: "100Mi"},
			},
		},
	}

	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.KubeletSeparateDiskGC, tc.kubeletSeparateDiskFeature)

			podMaker := podMaker
			summaryStatsMaker := summaryStatsMaker
			podsToMake := tc.podToMakes
			pods := []*v1.Pod{}
			podStats := map[*v1.Pod]statsapi.PodStats{}
			for _, podToMake := range podsToMake {
				pod, podStat := podMaker(podToMake.name, podToMake.priority, podToMake.requests, podToMake.limits, podToMake.rootFsInodesUsed, podToMake.logsFsInodesUsed, podToMake.perLocalVolumeInodesUsed)
				pods = append(pods, pod)
				podStats[pod] = podStat
			}
			podToEvict := pods[0]
			activePodsFunc := func() []*v1.Pod {
				return pods
			}

			fakeClock := testingclock.NewFakeClock(time.Now())
			podKiller := &mockPodKiller{}
			diskInfoProvider := &mockDiskInfoProvider{dedicatedImageFs: tc.dedicatedImageFs, dedicatedContainerFs: &tc.writeableSeparateFromReadOnly}
			diskGC := &mockDiskGC{err: nil}
			nodeRef := &v1.ObjectReference{Kind: "Node", Name: "test", UID: types.UID("test"), Namespace: ""}

			config := Config{
				MaxPodGracePeriodSeconds: 5,
				PressureTransitionPeriod: time.Minute * 5,
				Thresholds:               tc.thresholdToMonitor,
			}
			startingStatsConst := summaryStatsMaker(tc.nodeFsInodesFree, tc.nodeFsInodes, tc.imageFsInodesFree, tc.imageFsInodes, tc.containerFsInodesFree, tc.containerFsInodes, podStats)
			startingStatsModified := summaryStatsMaker(tc.nodeFsInodesFree, tc.nodeFsInodes, tc.imageFsInodesFree, tc.imageFsInodes, tc.containerFsInodesFree, tc.containerFsInodes, podStats)
			summaryProvider := &fakeSummaryProvider{result: startingStatsModified}
			manager := &managerImpl{
				clock:                        fakeClock,
				killPodFunc:                  podKiller.killPodNow,
				imageGC:                      diskGC,
				containerGC:                  diskGC,
				config:                       config,
				recorder:                     &record.FakeRecorder{},
				summaryProvider:              summaryProvider,
				nodeRef:                      nodeRef,
				nodeConditionsLastObservedAt: nodeConditionsObservedAt{},
				thresholdsFirstObservedAt:    thresholdsObservedAt{},
			}

			// create a best effort pod to test admission
			podToAdmit, _ := podMaker("pod-to-admit", defaultPriority, newResourceList("", "", ""), newResourceList("", "", ""), "0", "0", "0")

			// synchronize
			_, err := manager.synchronize(diskInfoProvider, activePodsFunc)

			if err != nil {
				t.Fatalf("Manager should not have an error %v", err)
			}

			// we should not have disk pressure
			if manager.IsUnderDiskPressure() {
				t.Fatalf("Manager should not report inode pressure")
			}

			// try to admit our pod (should succeed)
			if result := manager.Admit(&lifecycle.PodAdmitAttributes{Pod: podToAdmit}); !result.Admit {
				t.Fatalf("Admit pod: %v, expected: %v, actual: %v", podToAdmit, true, result.Admit)
			}

			// induce soft threshold
			fakeClock.Step(1 * time.Minute)
			summaryProvider.result = setINodesFreeBasedOnFs(tc.inducePressureOnWhichFs, tc.softINodePressure, startingStatsModified)
			_, err = manager.synchronize(diskInfoProvider, activePodsFunc)

			if err != nil {
				t.Fatalf("Manager should not have an error %v", err)
			}

			// we should have disk pressure
			if !manager.IsUnderDiskPressure() {
				t.Fatalf("Manager should report inode pressure since soft threshold was met")
			}

			// verify no pod was yet killed because there has not yet been enough time passed.
			if podKiller.pod != nil {
				t.Fatalf("Manager should not have killed a pod yet, but killed: %v", podKiller.pod.Name)
			}

			// step forward in time pass the grace period
			fakeClock.Step(3 * time.Minute)
			summaryProvider.result = setINodesFreeBasedOnFs(tc.inducePressureOnWhichFs, tc.softINodePressure, startingStatsModified)
			_, err = manager.synchronize(diskInfoProvider, activePodsFunc)

			if err != nil {
				t.Fatalf("Manager should not have an error %v", err)
			}

			// we should have disk pressure
			if !manager.IsUnderDiskPressure() {
				t.Fatalf("Manager should report inode pressure since soft threshold was met")
			}

			// verify the right pod was killed with the right grace period.
			if podKiller.pod != podToEvict {
				t.Fatalf("Manager chose to kill pod: %v, but should have chosen %v", podKiller.pod.Name, podToEvict.Name)
			}
			if podKiller.gracePeriodOverride == nil {
				t.Fatalf("Manager chose to kill pod but should have had a grace period override.")
			}
			observedGracePeriod := *podKiller.gracePeriodOverride
			if observedGracePeriod != manager.config.MaxPodGracePeriodSeconds {
				t.Fatalf("Manager chose to kill pod with incorrect grace period.  Expected: %d, actual: %d", manager.config.MaxPodGracePeriodSeconds, observedGracePeriod)
			}
			// reset state
			podKiller.pod = nil
			podKiller.gracePeriodOverride = nil

			// remove inode pressure
			fakeClock.Step(20 * time.Minute)
			summaryProvider.result = startingStatsConst
			_, err = manager.synchronize(diskInfoProvider, activePodsFunc)

			if err != nil {
				t.Fatalf("Manager should not have an error %v", err)
			}

			// we should not have disk pressure
			if manager.IsUnderDiskPressure() {
				t.Fatalf("Manager should not report inode pressure")
			}

			// induce inode pressure!
			fakeClock.Step(1 * time.Minute)
			summaryProvider.result = setINodesFreeBasedOnFs(tc.inducePressureOnWhichFs, tc.hardINodePressure, startingStatsModified)
			_, err = manager.synchronize(diskInfoProvider, activePodsFunc)

			if err != nil {
				t.Fatalf("Manager should not have an error %v", err)
			}

			// we should have disk pressure
			if !manager.IsUnderDiskPressure() {
				t.Fatalf("Manager should report inode pressure")
			}

			// check the right pod was killed
			if podKiller.pod != podToEvict {
				t.Fatalf("Manager chose to kill pod: %v, but should have chosen %v", podKiller.pod.Name, podToEvict.Name)
			}
			observedGracePeriod = *podKiller.gracePeriodOverride
			if observedGracePeriod != int64(1) {
				t.Fatalf("Manager chose to kill pod with incorrect grace period.  Expected: %d, actual: %d", 1, observedGracePeriod)
			}

			// try to admit our pod (should fail)
			if result := manager.Admit(&lifecycle.PodAdmitAttributes{Pod: podToAdmit}); result.Admit {
				t.Fatalf("Admit pod: %v, expected: %v, actual: %v", podToAdmit, false, result.Admit)
			}

			// reduce inode pressure
			fakeClock.Step(1 * time.Minute)
			summaryProvider.result = startingStatsConst
			podKiller.pod = nil // reset state
			_, err = manager.synchronize(diskInfoProvider, activePodsFunc)

			if err != nil {
				t.Fatalf("Manager should not have an error %v", err)
			}

			// we should have disk pressure (because transition period not yet met)
			if !manager.IsUnderDiskPressure() {
				t.Fatalf("Manager should report inode pressure")
			}

			// no pod should have been killed
			if podKiller.pod != nil {
				t.Fatalf("Manager chose to kill pod: %v when no pod should have been killed", podKiller.pod.Name)
			}

			// try to admit our pod (should fail)
			if result := manager.Admit(&lifecycle.PodAdmitAttributes{Pod: podToAdmit}); result.Admit {
				t.Fatalf("Admit pod: %v, expected: %v, actual: %v", podToAdmit, false, result.Admit)
			}

			// move the clock past transition period to ensure that we stop reporting pressure
			fakeClock.Step(5 * time.Minute)
			summaryProvider.result = startingStatsConst
			podKiller.pod = nil // reset state
			_, err = manager.synchronize(diskInfoProvider, activePodsFunc)

			if err != nil {
				t.Fatalf("Manager should not have an error %v", err)
			}

			// we should not have disk pressure (because transition period met)
			if manager.IsUnderDiskPressure() {
				t.Fatalf("Manager should not report inode pressure")
			}

			// no pod should have been killed
			if podKiller.pod != nil {
				t.Fatalf("Manager chose to kill pod: %v when no pod should have been killed", podKiller.pod.Name)
			}

			// try to admit our pod (should succeed)
			if result := manager.Admit(&lifecycle.PodAdmitAttributes{Pod: podToAdmit}); !result.Admit {
				t.Fatalf("Admit pod: %v, expected: %v, actual: %v", podToAdmit, true, result.Admit)
			}
		})
	}
}

// TestStaticCriticalPodsAreNotEvicted
func TestStaticCriticalPodsAreNotEvicted(t *testing.T) {
	podMaker := makePodWithMemoryStats
	summaryStatsMaker := makeMemoryStats
	podsToMake := []podToMake{
		{name: "critical", priority: scheduling.SystemCriticalPriority, requests: newResourceList("100m", "1Gi", ""), limits: newResourceList("100m", "1Gi", ""), memoryWorkingSet: "800Mi"},
	}
	pods := []*v1.Pod{}
	podStats := map[*v1.Pod]statsapi.PodStats{}
	for _, podToMake := range podsToMake {
		pod, podStat := podMaker(podToMake.name, podToMake.priority, podToMake.requests, podToMake.limits, podToMake.memoryWorkingSet)
		pods = append(pods, pod)
		podStats[pod] = podStat
	}

	pods[0].Annotations = map[string]string{
		kubelettypes.ConfigSourceAnnotationKey: kubelettypes.FileSource,
	}
	// Mark the pod as critical
	podPriority := scheduling.SystemCriticalPriority
	pods[0].Spec.Priority = &podPriority
	pods[0].Namespace = kubeapi.NamespaceSystem

	podToEvict := pods[0]
	activePodsFunc := func() []*v1.Pod {
		return pods
	}

	fakeClock := testingclock.NewFakeClock(time.Now())
	podKiller := &mockPodKiller{}
	diskInfoProvider := &mockDiskInfoProvider{dedicatedImageFs: ptr.To(false)}
	diskGC := &mockDiskGC{err: nil}
	nodeRef := &v1.ObjectReference{
		Kind: "Node", Name: "test", UID: types.UID("test"), Namespace: "",
	}

	config := Config{
		MaxPodGracePeriodSeconds: 5,
		PressureTransitionPeriod: time.Minute * 5,
		Thresholds: []evictionapi.Threshold{
			{
				Signal:   evictionapi.SignalMemoryAvailable,
				Operator: evictionapi.OpLessThan,
				Value: evictionapi.ThresholdValue{
					Quantity: quantityMustParse("1Gi"),
				},
			},
			{
				Signal:   evictionapi.SignalMemoryAvailable,
				Operator: evictionapi.OpLessThan,
				Value: evictionapi.ThresholdValue{
					Quantity: quantityMustParse("2Gi"),
				},
				GracePeriod: time.Minute * 2,
			},
		},
	}
	summaryProvider := &fakeSummaryProvider{result: summaryStatsMaker("2Gi", podStats)}
	manager := &managerImpl{
		clock:                        fakeClock,
		killPodFunc:                  podKiller.killPodNow,
		imageGC:                      diskGC,
		containerGC:                  diskGC,
		config:                       config,
		recorder:                     &record.FakeRecorder{},
		summaryProvider:              summaryProvider,
		nodeRef:                      nodeRef,
		nodeConditionsLastObservedAt: nodeConditionsObservedAt{},
		thresholdsFirstObservedAt:    thresholdsObservedAt{},
	}

	fakeClock.Step(1 * time.Minute)
	summaryProvider.result = summaryStatsMaker("1500Mi", podStats)
	_, err := manager.synchronize(diskInfoProvider, activePodsFunc)

	if err != nil {
		t.Fatalf("Manager should not have an error %v", err)
	}

	// we should have memory pressure
	if !manager.IsUnderMemoryPressure() {
		t.Errorf("Manager should report memory pressure since soft threshold was met")
	}

	// verify no pod was yet killed because there has not yet been enough time passed.
	if podKiller.pod != nil {
		t.Errorf("Manager should not have killed a pod yet, but killed: %v", podKiller.pod.Name)
	}

	// step forward in time pass the grace period
	fakeClock.Step(3 * time.Minute)
	summaryProvider.result = summaryStatsMaker("1500Mi", podStats)
	_, err = manager.synchronize(diskInfoProvider, activePodsFunc)

	if err != nil {
		t.Fatalf("Manager should not have an error %v", err)
	}

	// we should have memory pressure
	if !manager.IsUnderMemoryPressure() {
		t.Errorf("Manager should report memory pressure since soft threshold was met")
	}

	// verify the right pod was killed with the right grace period.
	if podKiller.pod == podToEvict {
		t.Errorf("Manager chose to kill critical pod: %v, but should have ignored it", podKiller.pod.Name)
	}
	// reset state
	podKiller.pod = nil
	podKiller.gracePeriodOverride = nil

	// remove memory pressure
	fakeClock.Step(20 * time.Minute)
	summaryProvider.result = summaryStatsMaker("3Gi", podStats)
	_, err = manager.synchronize(diskInfoProvider, activePodsFunc)

	if err != nil {
		t.Fatalf("Manager should not have an error %v", err)
	}

	// we should not have memory pressure
	if manager.IsUnderMemoryPressure() {
		t.Errorf("Manager should not report memory pressure")
	}

	pods[0].Annotations = map[string]string{
		kubelettypes.ConfigSourceAnnotationKey: kubelettypes.FileSource,
	}
	pods[0].Spec.Priority = nil
	pods[0].Namespace = kubeapi.NamespaceSystem

	// induce memory pressure!
	fakeClock.Step(1 * time.Minute)
	summaryProvider.result = summaryStatsMaker("500Mi", podStats)
	_, err = manager.synchronize(diskInfoProvider, activePodsFunc)

	if err != nil {
		t.Fatalf("Manager should not have an error %v", err)
	}

	// we should have memory pressure
	if !manager.IsUnderMemoryPressure() {
		t.Errorf("Manager should report memory pressure")
	}
}

func TestStorageLimitEvictions(t *testing.T) {
	volumeSizeLimit := resource.MustParse("1Gi")

	testCases := map[string]struct {
		pod     podToMake
		volumes []v1.Volume
	}{
		"eviction due to rootfs above limit": {
			pod: podToMake{name: "rootfs-above-limits", priority: defaultPriority, requests: newResourceList("", "", "1Gi"), limits: newResourceList("", "", "1Gi"), rootFsUsed: "2Gi"},
		},
		"eviction due to logsfs above limit": {
			pod: podToMake{name: "logsfs-above-limits", priority: defaultPriority, requests: newResourceList("", "", "1Gi"), limits: newResourceList("", "", "1Gi"), logsFsUsed: "2Gi"},
		},
		"eviction due to local volume above limit": {
			pod: podToMake{name: "localvolume-above-limits", priority: defaultPriority, requests: newResourceList("", "", ""), limits: newResourceList("", "", ""), perLocalVolumeUsed: "2Gi"},
			volumes: []v1.Volume{{
				Name: "emptyDirVolume",
				VolumeSource: v1.VolumeSource{
					EmptyDir: &v1.EmptyDirVolumeSource{
						SizeLimit: &volumeSizeLimit,
					},
				},
			}},
		},
	}
	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			podMaker := makePodWithDiskStats
			summaryStatsMaker := makeDiskStats
			podsToMake := []podToMake{
				tc.pod,
			}
			pods := []*v1.Pod{}
			podStats := map[*v1.Pod]statsapi.PodStats{}
			for _, podToMake := range podsToMake {
				pod, podStat := podMaker(podToMake.name, podToMake.priority, podToMake.requests, podToMake.limits, podToMake.rootFsUsed, podToMake.logsFsUsed, podToMake.perLocalVolumeUsed, tc.volumes)
				pods = append(pods, pod)
				podStats[pod] = podStat
			}

			podToEvict := pods[0]
			activePodsFunc := func() []*v1.Pod {
				return pods
			}

			fakeClock := testingclock.NewFakeClock(time.Now())
			podKiller := &mockPodKiller{}
			diskInfoProvider := &mockDiskInfoProvider{dedicatedImageFs: ptr.To(false)}
			diskGC := &mockDiskGC{err: nil}
			nodeRef := &v1.ObjectReference{
				Kind: "Node", Name: "test", UID: types.UID("test"), Namespace: "",
			}

			config := Config{
				MaxPodGracePeriodSeconds: 5,
				PressureTransitionPeriod: time.Minute * 5,
				Thresholds: []evictionapi.Threshold{
					{
						Signal:   evictionapi.SignalNodeFsAvailable,
						Operator: evictionapi.OpLessThan,
						Value: evictionapi.ThresholdValue{
							Quantity: quantityMustParse("1Gi"),
						},
					},
				},
			}

			diskStat := diskStats{
				rootFsAvailableBytes:  "200Mi",
				imageFsAvailableBytes: "200Mi",
				podStats:              podStats,
			}
			summaryProvider := &fakeSummaryProvider{result: summaryStatsMaker(diskStat)}
			manager := &managerImpl{
				clock:                         fakeClock,
				killPodFunc:                   podKiller.killPodNow,
				imageGC:                       diskGC,
				containerGC:                   diskGC,
				config:                        config,
				recorder:                      &record.FakeRecorder{},
				summaryProvider:               summaryProvider,
				nodeRef:                       nodeRef,
				nodeConditionsLastObservedAt:  nodeConditionsObservedAt{},
				thresholdsFirstObservedAt:     thresholdsObservedAt{},
				localStorageCapacityIsolation: true,
			}

			_, err := manager.synchronize(diskInfoProvider, activePodsFunc)
			if err != nil {
				t.Fatalf("Manager expects no error but got %v", err)
			}

			if podKiller.pod == nil {
				t.Fatalf("Manager should have selected a pod for eviction")
			}
			if podKiller.pod != podToEvict {
				t.Errorf("Manager should have killed pod: %v, but instead killed: %v", podToEvict.Name, podKiller.pod.Name)
			}
			if *podKiller.gracePeriodOverride != 1 {
				t.Errorf("Manager should have evicted with gracePeriodOverride of 1, but used: %v", *podKiller.gracePeriodOverride)
			}
		})
	}
}

// TestAllocatableMemoryPressure
func TestAllocatableMemoryPressure(t *testing.T) {
	podMaker := makePodWithMemoryStats
	summaryStatsMaker := makeMemoryStats
	podsToMake := []podToMake{
		{name: "guaranteed-low-priority-high-usage", priority: lowPriority, requests: newResourceList("100m", "1Gi", ""), limits: newResourceList("100m", "1Gi", ""), memoryWorkingSet: "900Mi"},
		{name: "burstable-below-requests", priority: defaultPriority, requests: newResourceList("100m", "100Mi", ""), limits: newResourceList("200m", "1Gi", ""), memoryWorkingSet: "50Mi"},
		{name: "burstable-above-requests", priority: defaultPriority, requests: newResourceList("100m", "100Mi", ""), limits: newResourceList("200m", "1Gi", ""), memoryWorkingSet: "400Mi"},
		{name: "best-effort-high-priority-high-usage", priority: highPriority, requests: newResourceList("", "", ""), limits: newResourceList("", "", ""), memoryWorkingSet: "400Mi"},
		{name: "best-effort-low-priority-low-usage", priority: lowPriority, requests: newResourceList("", "", ""), limits: newResourceList("", "", ""), memoryWorkingSet: "100Mi"},
	}
	pods := []*v1.Pod{}
	podStats := map[*v1.Pod]statsapi.PodStats{}
	for _, podToMake := range podsToMake {
		pod, podStat := podMaker(podToMake.name, podToMake.priority, podToMake.requests, podToMake.limits, podToMake.memoryWorkingSet)
		pods = append(pods, pod)
		podStats[pod] = podStat
	}
	podToEvict := pods[4]
	activePodsFunc := func() []*v1.Pod {
		return pods
	}

	fakeClock := testingclock.NewFakeClock(time.Now())
	podKiller := &mockPodKiller{}
	diskInfoProvider := &mockDiskInfoProvider{dedicatedImageFs: ptr.To(false)}
	diskGC := &mockDiskGC{err: nil}
	nodeRef := &v1.ObjectReference{Kind: "Node", Name: "test", UID: types.UID("test"), Namespace: ""}

	config := Config{
		MaxPodGracePeriodSeconds: 5,
		PressureTransitionPeriod: time.Minute * 5,
		Thresholds: []evictionapi.Threshold{
			{
				Signal:   evictionapi.SignalAllocatableMemoryAvailable,
				Operator: evictionapi.OpLessThan,
				Value: evictionapi.ThresholdValue{
					Quantity: quantityMustParse("1Gi"),
				},
			},
		},
	}
	summaryProvider := &fakeSummaryProvider{result: summaryStatsMaker("4Gi", podStats)}
	manager := &managerImpl{
		clock:                        fakeClock,
		killPodFunc:                  podKiller.killPodNow,
		imageGC:                      diskGC,
		containerGC:                  diskGC,
		config:                       config,
		recorder:                     &record.FakeRecorder{},
		summaryProvider:              summaryProvider,
		nodeRef:                      nodeRef,
		nodeConditionsLastObservedAt: nodeConditionsObservedAt{},
		thresholdsFirstObservedAt:    thresholdsObservedAt{},
	}

	// create a best effort pod to test admission
	bestEffortPodToAdmit, _ := podMaker("best-admit", defaultPriority, newResourceList("", "", ""), newResourceList("", "", ""), "0Gi")
	burstablePodToAdmit, _ := podMaker("burst-admit", defaultPriority, newResourceList("100m", "100Mi", ""), newResourceList("200m", "200Mi", ""), "0Gi")

	// synchronize
	_, err := manager.synchronize(diskInfoProvider, activePodsFunc)

	if err != nil {
		t.Fatalf("Manager should not have an error %v", err)
	}

	// we should not have memory pressure
	if manager.IsUnderMemoryPressure() {
		t.Errorf("Manager should not report memory pressure")
	}

	// try to admit our pods (they should succeed)
	expected := []bool{true, true}
	for i, pod := range []*v1.Pod{bestEffortPodToAdmit, burstablePodToAdmit} {
		if result := manager.Admit(&lifecycle.PodAdmitAttributes{Pod: pod}); expected[i] != result.Admit {
			t.Errorf("Admit pod: %v, expected: %v, actual: %v", pod, expected[i], result.Admit)
		}
	}

	// induce memory pressure!
	fakeClock.Step(1 * time.Minute)
	pod, podStat := podMaker("guaranteed-high-2", defaultPriority, newResourceList("100m", "1Gi", ""), newResourceList("100m", "1Gi", ""), "1Gi")
	podStats[pod] = podStat
	summaryProvider.result = summaryStatsMaker("500Mi", podStats)
	_, err = manager.synchronize(diskInfoProvider, activePodsFunc)

	if err != nil {
		t.Fatalf("Manager should not have an error %v", err)
	}

	// we should have memory pressure
	if !manager.IsUnderMemoryPressure() {
		t.Errorf("Manager should report memory pressure")
	}

	// check the right pod was killed
	if podKiller.pod != podToEvict {
		t.Errorf("Manager chose to kill pod: %v, but should have chosen %v", podKiller.pod.Name, podToEvict.Name)
	}
	observedGracePeriod := *podKiller.gracePeriodOverride
	if observedGracePeriod != int64(1) {
		t.Errorf("Manager chose to kill pod with incorrect grace period.  Expected: %d, actual: %d", 1, observedGracePeriod)
	}
	// reset state
	podKiller.pod = nil
	podKiller.gracePeriodOverride = nil

	// the best-effort pod should not admit, burstable should
	expected = []bool{false, true}
	for i, pod := range []*v1.Pod{bestEffortPodToAdmit, burstablePodToAdmit} {
		if result := manager.Admit(&lifecycle.PodAdmitAttributes{Pod: pod}); expected[i] != result.Admit {
			t.Errorf("Admit pod: %v, expected: %v, actual: %v", pod, expected[i], result.Admit)
		}
	}

	// reduce memory pressure
	fakeClock.Step(1 * time.Minute)
	for pod := range podStats {
		if pod.Name == "guaranteed-high-2" {
			delete(podStats, pod)
		}
	}
	summaryProvider.result = summaryStatsMaker("2Gi", podStats)
	podKiller.pod = nil // reset state
	_, err = manager.synchronize(diskInfoProvider, activePodsFunc)

	if err != nil {
		t.Fatalf("Manager should not have an error %v", err)
	}

	// we should have memory pressure (because transition period not yet met)
	if !manager.IsUnderMemoryPressure() {
		t.Errorf("Manager should report memory pressure")
	}

	// no pod should have been killed
	if podKiller.pod != nil {
		t.Errorf("Manager chose to kill pod: %v when no pod should have been killed", podKiller.pod.Name)
	}

	// the best-effort pod should not admit, burstable should
	expected = []bool{false, true}
	for i, pod := range []*v1.Pod{bestEffortPodToAdmit, burstablePodToAdmit} {
		if result := manager.Admit(&lifecycle.PodAdmitAttributes{Pod: pod}); expected[i] != result.Admit {
			t.Errorf("Admit pod: %v, expected: %v, actual: %v", pod, expected[i], result.Admit)
		}
	}

	// move the clock past transition period to ensure that we stop reporting pressure
	fakeClock.Step(5 * time.Minute)
	summaryProvider.result = summaryStatsMaker("2Gi", podStats)
	podKiller.pod = nil // reset state
	_, err = manager.synchronize(diskInfoProvider, activePodsFunc)

	if err != nil {
		t.Fatalf("Manager should not have an error %v", err)
	}

	// we should not have memory pressure (because transition period met)
	if manager.IsUnderMemoryPressure() {
		t.Errorf("Manager should not report memory pressure")
	}

	// no pod should have been killed
	if podKiller.pod != nil {
		t.Errorf("Manager chose to kill pod: %v when no pod should have been killed", podKiller.pod.Name)
	}

	// all pods should admit now
	expected = []bool{true, true}
	for i, pod := range []*v1.Pod{bestEffortPodToAdmit, burstablePodToAdmit} {
		if result := manager.Admit(&lifecycle.PodAdmitAttributes{Pod: pod}); expected[i] != result.Admit {
			t.Errorf("Admit pod: %v, expected: %v, actual: %v", pod, expected[i], result.Admit)
		}
	}
}

func TestUpdateMemcgThreshold(t *testing.T) {
	activePodsFunc := func() []*v1.Pod {
		return []*v1.Pod{}
	}

	fakeClock := testingclock.NewFakeClock(time.Now())
	podKiller := &mockPodKiller{}
	diskInfoProvider := &mockDiskInfoProvider{dedicatedImageFs: ptr.To(false)}
	diskGC := &mockDiskGC{err: nil}
	nodeRef := &v1.ObjectReference{Kind: "Node", Name: "test", UID: types.UID("test"), Namespace: ""}

	config := Config{
		MaxPodGracePeriodSeconds: 5,
		PressureTransitionPeriod: time.Minute * 5,
		Thresholds: []evictionapi.Threshold{
			{
				Signal:   evictionapi.SignalMemoryAvailable,
				Operator: evictionapi.OpLessThan,
				Value: evictionapi.ThresholdValue{
					Quantity: quantityMustParse("1Gi"),
				},
			},
		},
		PodCgroupRoot: "kubepods",
	}
	summaryProvider := &fakeSummaryProvider{result: makeMemoryStats("2Gi", map[*v1.Pod]statsapi.PodStats{})}

	thresholdNotifier := NewMockThresholdNotifier(t)
	thresholdNotifier.EXPECT().UpdateThreshold(summaryProvider.result).Return(nil).Times(2)

	manager := &managerImpl{
		clock:                        fakeClock,
		killPodFunc:                  podKiller.killPodNow,
		imageGC:                      diskGC,
		containerGC:                  diskGC,
		config:                       config,
		recorder:                     &record.FakeRecorder{},
		summaryProvider:              summaryProvider,
		nodeRef:                      nodeRef,
		nodeConditionsLastObservedAt: nodeConditionsObservedAt{},
		thresholdsFirstObservedAt:    thresholdsObservedAt{},
		thresholdNotifiers:           []ThresholdNotifier{thresholdNotifier},
	}

	// The UpdateThreshold method should have been called once, since this is the first run.
	_, err := manager.synchronize(diskInfoProvider, activePodsFunc)

	if err != nil {
		t.Fatalf("Manager should not have an error %v", err)
	}

	// The UpdateThreshold method should not have been called again, since not enough time has passed
	_, err = manager.synchronize(diskInfoProvider, activePodsFunc)

	if err != nil {
		t.Fatalf("Manager should not have an error %v", err)
	}

	// The UpdateThreshold method should be called again since enough time has passed
	fakeClock.Step(2 * notifierRefreshInterval)
	_, err = manager.synchronize(diskInfoProvider, activePodsFunc)

	if err != nil {
		t.Fatalf("Manager should not have an error %v", err)
	}

	// new memory threshold notifier that returns an error
	thresholdNotifier = NewMockThresholdNotifier(t)
	thresholdNotifier.EXPECT().UpdateThreshold(summaryProvider.result).Return(fmt.Errorf("error updating threshold")).Times(1)
	thresholdNotifier.EXPECT().Description().Return("mock thresholdNotifier").Times(1)
	manager.thresholdNotifiers = []ThresholdNotifier{thresholdNotifier}

	// The UpdateThreshold method should be called because at least notifierRefreshInterval time has passed.
	// The Description method should be called because UpdateThreshold returned an error
	fakeClock.Step(2 * notifierRefreshInterval)
	_, err = manager.synchronize(diskInfoProvider, activePodsFunc)

	if err != nil {
		t.Fatalf("Manager should not have an error %v", err)
	}
}

func TestManagerWithLocalStorageCapacityIsolationOpen(t *testing.T) {
	podMaker := makePodWithLocalStorageCapacityIsolationOpen
	summaryStatsMaker := makeDiskStats
	podsToMake := []podToMake{
		{name: "empty-dir", requests: newResourceList("", "900Mi", ""), limits: newResourceList("", "1Gi", "")},
		{name: "container-ephemeral-storage-limit", requests: newResourceList("", "", "900Mi"), limits: newResourceList("", "", "800Mi")},
		{name: "pod-ephemeral-storage-limit", requests: newResourceList("", "", "1Gi"), limits: newResourceList("", "", "800Mi")},
	}

	pods := []*v1.Pod{}
	podStats := map[*v1.Pod]statsapi.PodStats{}
	for _, podToMake := range podsToMake {
		pod, podStat := podMaker(podToMake.name, podToMake.priority, podToMake.requests, podToMake.limits, podToMake.memoryWorkingSet)
		pods = append(pods, pod)
		podStats[pod] = podStat
	}

	diskStat := diskStats{
		rootFsAvailableBytes:  "1Gi",
		imageFsAvailableBytes: "200Mi",
		podStats:              podStats,
	}
	summaryProvider := &fakeSummaryProvider{result: summaryStatsMaker(diskStat)}

	config := Config{
		MaxPodGracePeriodSeconds: 5,
		PressureTransitionPeriod: time.Minute * 5,
		Thresholds: []evictionapi.Threshold{
			{
				Signal:   evictionapi.SignalAllocatableMemoryAvailable,
				Operator: evictionapi.OpLessThan,
				Value: evictionapi.ThresholdValue{
					Quantity: quantityMustParse("1Gi"),
				},
			},
		},
	}

	podKiller := &mockPodKiller{}
	diskGC := &mockDiskGC{err: nil}
	nodeRef := &v1.ObjectReference{Kind: "Node", Name: "test", UID: types.UID("test"), Namespace: ""}
	fakeClock := testingclock.NewFakeClock(time.Now())
	diskInfoProvider := &mockDiskInfoProvider{dedicatedImageFs: ptr.To(false)}

	mgr := &managerImpl{
		clock:                         fakeClock,
		killPodFunc:                   podKiller.killPodNow,
		imageGC:                       diskGC,
		containerGC:                   diskGC,
		config:                        config,
		recorder:                      &record.FakeRecorder{},
		summaryProvider:               summaryProvider,
		nodeRef:                       nodeRef,
		localStorageCapacityIsolation: true,
		dedicatedImageFs:              diskInfoProvider.dedicatedImageFs,
	}

	activePodsFunc := func() []*v1.Pod {
		return pods
	}

	evictedPods, err := mgr.synchronize(diskInfoProvider, activePodsFunc)

	if err != nil {
		t.Fatalf("Manager should not have error but got %v", err)
	}
	if podKiller.pod == nil {
		t.Fatalf("Manager should have selected a pod for eviction")
	}

	if diff := cmp.Diff(pods, evictedPods); diff != "" {
		t.Fatalf("Unexpected evicted pod (-want,+got):\n%s", diff)
	}
}
