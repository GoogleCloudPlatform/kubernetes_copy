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

package kubelet

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	core "k8s.io/client-go/testing"
	"k8s.io/kubernetes/pkg/volume"
	volumetest "k8s.io/kubernetes/pkg/volume/testing"
	"k8s.io/kubernetes/pkg/volume/util"
	"k8s.io/kubernetes/test/utils/ktesting"
)

func TestListVolumesForPod(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode.")
	}

	testKubelet := newTestKubelet(t, false /* controllerAttachDetachEnabled */)
	defer testKubelet.Cleanup()
	kubelet := testKubelet.kubelet

	pod := podWithUIDNameNsSpec("12345678", "foo", "test", v1.PodSpec{
		Containers: []v1.Container{
			{
				Name: "container1",
				VolumeMounts: []v1.VolumeMount{
					{
						Name:      "vol1",
						MountPath: "/mnt/vol1",
					},
					{
						Name:      "vol2",
						MountPath: "/mnt/vol2",
					},
				},
			},
		},
		Volumes: []v1.Volume{
			{
				Name: "vol1",
				VolumeSource: v1.VolumeSource{
					GCEPersistentDisk: &v1.GCEPersistentDiskVolumeSource{
						PDName: "fake-device1",
					},
				},
			},
			{
				Name: "vol2",
				VolumeSource: v1.VolumeSource{
					GCEPersistentDisk: &v1.GCEPersistentDiskVolumeSource{
						PDName: "fake-device2",
					},
				},
			},
		},
	})

	tCtx := ktesting.Init(t)
	defer tCtx.Cancel("test has completed")
	go kubelet.volumeManager.Run(tCtx, kubelet.sourcesReady)

	kubelet.podManager.SetPods([]*v1.Pod{pod})
	err := kubelet.volumeManager.WaitForAttachAndMount(context.Background(), pod)
	assert.NoError(t, err)

	podName := util.GetUniquePodName(pod)

	volumesToReturn, volumeExsit := kubelet.ListVolumesForPod(types.UID(podName))
	assert.True(t, volumeExsit, "expected to find volumes for pod %q", podName)

	outerVolumeSpecName1 := "vol1"
	assert.NotNil(t, volumesToReturn[outerVolumeSpecName1], "key %s", outerVolumeSpecName1)

	outerVolumeSpecName2 := "vol2"
	assert.NotNil(t, volumesToReturn[outerVolumeSpecName2], "key %s", outerVolumeSpecName2)
}

func TestPodVolumesExist(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode.")
	}

	testKubelet := newTestKubelet(t, false /* controllerAttachDetachEnabled */)
	defer testKubelet.Cleanup()
	kubelet := testKubelet.kubelet

	pods := []*v1.Pod{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: "pod1",
				UID:  "pod1uid",
			},
			Spec: v1.PodSpec{
				Containers: []v1.Container{
					{
						Name: "container1",
						VolumeMounts: []v1.VolumeMount{
							{
								Name:      "vol1",
								MountPath: "/mnt/vol1",
							},
						},
					},
				},
				Volumes: []v1.Volume{
					{
						Name: "vol1",
						VolumeSource: v1.VolumeSource{
							RBD: &v1.RBDVolumeSource{
								RBDImage: "fake1",
							},
						},
					},
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: "pod2",
				UID:  "pod2uid",
			},
			Spec: v1.PodSpec{
				Containers: []v1.Container{
					{
						Name: "container2",
						VolumeMounts: []v1.VolumeMount{
							{
								Name:      "vol2",
								MountPath: "/mnt/vol2",
							},
						},
					},
				},
				Volumes: []v1.Volume{
					{
						Name: "vol2",
						VolumeSource: v1.VolumeSource{
							RBD: &v1.RBDVolumeSource{
								RBDImage: "fake2",
							},
						},
					},
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: "pod3",
				UID:  "pod3uid",
			},
			Spec: v1.PodSpec{
				Containers: []v1.Container{
					{
						Name: "container3",
						VolumeMounts: []v1.VolumeMount{
							{
								Name:      "vol3",
								MountPath: "/mnt/vol3",
							},
						},
					},
				},
				Volumes: []v1.Volume{
					{
						Name: "vol3",
						VolumeSource: v1.VolumeSource{
							RBD: &v1.RBDVolumeSource{
								RBDImage: "fake3",
							},
						},
					},
				},
			},
		},
	}

	tCtx := ktesting.Init(t)
	defer tCtx.Cancel("test has completed")
	go kubelet.volumeManager.Run(tCtx, kubelet.sourcesReady)

	kubelet.podManager.SetPods(pods)
	for _, pod := range pods {
		err := kubelet.volumeManager.WaitForAttachAndMount(context.Background(), pod)
		assert.NoError(t, err)
	}

	for _, pod := range pods {
		podVolumesExist := kubelet.podVolumesExist(pod.UID)
		assert.True(t, podVolumesExist, "pod %q", pod.UID)
	}
}

func TestPodVolumeDeadlineAttachAndMount(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode.")
	}

	testKubelet := newTestKubeletWithImageList(t, nil /*imageList*/, false, /* controllerAttachDetachEnabled */
		false /*initFakeVolumePlugin*/, true /*localStorageCapacityIsolation*/)

	defer testKubelet.Cleanup()
	kubelet := testKubelet.kubelet

	// any test cases added here should have volumes that fail to mount
	pods := []*v1.Pod{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: "pod1",
				UID:  "pod1uid",
			},
			Spec: v1.PodSpec{
				Containers: []v1.Container{
					{
						Name: "container1",
						VolumeMounts: []v1.VolumeMount{
							{
								Name:      "vol1",
								MountPath: "/mnt/vol1",
							},
						},
					},
				},
				Volumes: []v1.Volume{
					{
						Name: "vol1",
						VolumeSource: v1.VolumeSource{
							Secret: &v1.SecretVolumeSource{
								SecretName: "non-existent",
							},
						},
					},
				},
			},
		},
	}

	tCtx := ktesting.Init(t)
	defer tCtx.Cancel("test has completed")
	go kubelet.volumeManager.Run(tCtx, kubelet.sourcesReady)

	kubelet.podManager.SetPods(pods)
	for _, pod := range pods {
		start := time.Now()
		// ensure our context times out quickly
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(2*time.Second))
		err := kubelet.volumeManager.WaitForAttachAndMount(ctx, pod)
		delta := time.Since(start)
		// the standard timeout is 2 minutes, so if it's just a few seconds we know that the context timeout was the cause
		assert.Lessf(t, delta, 10*time.Second, "WaitForAttachAndMount should timeout when the context is cancelled")
		assert.ErrorIs(t, err, context.DeadlineExceeded)
		cancel()
	}
}

func TestPodVolumeDeadlineUnmount(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode.")
	}

	testKubelet := newTestKubeletWithImageList(t, nil /*imageList*/, false, /* controllerAttachDetachEnabled */
		true /*initFakeVolumePlugin*/, true /*localStorageCapacityIsolation*/)

	defer testKubelet.Cleanup()
	kubelet := testKubelet.kubelet

	// any test cases added here should have volumes that succeed at mounting
	pods := []*v1.Pod{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: "pod1",
				UID:  "pod1uid",
			},
			Spec: v1.PodSpec{
				Containers: []v1.Container{
					{
						Name: "container1",
						VolumeMounts: []v1.VolumeMount{
							{
								Name:      "vol1",
								MountPath: "/mnt/vol1",
							},
						},
					},
				},
				Volumes: []v1.Volume{
					{
						Name: "vol1",
						VolumeSource: v1.VolumeSource{
							RBD: &v1.RBDVolumeSource{
								RBDImage: "fake-device",
							},
						},
					},
				},
			},
		},
	}

	tCtx := ktesting.Init(t)
	defer tCtx.Cancel("test has completed")
	go kubelet.volumeManager.Run(tCtx, kubelet.sourcesReady)

	kubelet.podManager.SetPods(pods)
	for i, pod := range pods {
		if err := kubelet.volumeManager.WaitForAttachAndMount(context.Background(), pod); err != nil {
			t.Fatalf("pod %d failed: %v", i, err)
		}
		start := time.Now()
		// ensure our context times out quickly
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(2*time.Second))
		err := kubelet.volumeManager.WaitForUnmount(ctx, pod)
		delta := time.Since(start)
		// the standard timeout is 2 minutes, so if it's just a few seconds we know that the context timeout was the cause
		assert.Lessf(t, delta, 10*time.Second, "WaitForUnmount should timeout when the context is cancelled")
		assert.ErrorIs(t, err, context.DeadlineExceeded)
		cancel()
	}
}

func TestVolumeAttachAndMountControllerDisabled(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode.")
	}

	testKubelet := newTestKubelet(t, false /* controllerAttachDetachEnabled */)
	defer testKubelet.Cleanup()
	kubelet := testKubelet.kubelet

	pod := podWithUIDNameNsSpec("12345678", "foo", "test", v1.PodSpec{
		Containers: []v1.Container{
			{
				Name: "container1",
				VolumeMounts: []v1.VolumeMount{
					{
						Name:      "vol1",
						MountPath: "/mnt/vol1",
					},
				},
			},
		},
		Volumes: []v1.Volume{
			{
				Name: "vol1",
				VolumeSource: v1.VolumeSource{
					RBD: &v1.RBDVolumeSource{
						RBDImage: "fake",
					},
				},
			},
		},
	})

	tCtx := ktesting.Init(t)
	defer tCtx.Cancel("test has completed")
	go kubelet.volumeManager.Run(tCtx, kubelet.sourcesReady)

	kubelet.podManager.SetPods([]*v1.Pod{pod})
	err := kubelet.volumeManager.WaitForAttachAndMount(context.Background(), pod)
	assert.NoError(t, err)

	podVolumes := kubelet.volumeManager.GetMountedVolumesForPod(
		util.GetUniquePodName(pod))

	expectedPodVolumes := []string{"vol1"}
	assert.Len(t, podVolumes, len(expectedPodVolumes), "Volumes for pod %+v", pod)
	for _, name := range expectedPodVolumes {
		assert.Contains(t, podVolumes, name, "Volumes for pod %+v", pod)
	}
	assert.GreaterOrEqual(t, testKubelet.volumePlugin.GetNewAttacherCallCount(), 1, "Expected plugin NewAttacher to be called at least once")
	assert.NoError(t, volumetest.VerifyWaitForAttachCallCount(
		1 /* expectedWaitForAttachCallCount */, testKubelet.volumePlugin))
	assert.NoError(t, volumetest.VerifyAttachCallCount(
		1 /* expectedAttachCallCount */, testKubelet.volumePlugin))
	assert.NoError(t, volumetest.VerifyMountDeviceCallCount(
		1 /* expectedMountDeviceCallCount */, testKubelet.volumePlugin))
	assert.NoError(t, volumetest.VerifySetUpCallCount(
		1 /* expectedSetUpCallCount */, testKubelet.volumePlugin))
}

func TestVolumeUnmountAndDetachControllerDisabled(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode.")
	}

	testKubelet := newTestKubelet(t, false /* controllerAttachDetachEnabled */)
	defer testKubelet.Cleanup()
	kubelet := testKubelet.kubelet

	pod := podWithUIDNameNsSpec("12345678", "foo", "test", v1.PodSpec{
		Containers: []v1.Container{
			{
				Name: "container1",
				VolumeMounts: []v1.VolumeMount{
					{
						Name:      "vol1",
						MountPath: "/mnt/vol1",
					},
				},
			},
		},
		Volumes: []v1.Volume{
			{
				Name: "vol1",
				VolumeSource: v1.VolumeSource{
					RBD: &v1.RBDVolumeSource{
						RBDImage: "fake-device",
					},
				},
			},
		},
	})

	tCtx := ktesting.Init(t)
	defer tCtx.Cancel("test has completed")
	go kubelet.volumeManager.Run(tCtx, kubelet.sourcesReady)

	// Add pod
	kubelet.podManager.SetPods([]*v1.Pod{pod})

	// Verify volumes attached
	err := kubelet.volumeManager.WaitForAttachAndMount(context.Background(), pod)
	assert.NoError(t, err)

	podVolumes := kubelet.volumeManager.GetMountedVolumesForPod(
		util.GetUniquePodName(pod))

	expectedPodVolumes := []string{"vol1"}
	assert.Len(t, podVolumes, len(expectedPodVolumes), "Volumes for pod %+v", pod)
	for _, name := range expectedPodVolumes {
		assert.Contains(t, podVolumes, name, "Volumes for pod %+v", pod)
	}

	assert.GreaterOrEqual(t, testKubelet.volumePlugin.GetNewAttacherCallCount(), 1, "Expected plugin NewAttacher to be called at least once")
	assert.NoError(t, volumetest.VerifyWaitForAttachCallCount(
		1 /* expectedWaitForAttachCallCount */, testKubelet.volumePlugin))
	assert.NoError(t, volumetest.VerifyAttachCallCount(
		1 /* expectedAttachCallCount */, testKubelet.volumePlugin))
	assert.NoError(t, volumetest.VerifyMountDeviceCallCount(
		1 /* expectedMountDeviceCallCount */, testKubelet.volumePlugin))
	assert.NoError(t, volumetest.VerifySetUpCallCount(
		1 /* expectedSetUpCallCount */, testKubelet.volumePlugin))

	// Remove pod
	// TODO: technically waitForVolumeUnmount
	kubelet.podWorkers.(*fakePodWorkers).setPodRuntimeBeRemoved(pod.UID)
	kubelet.podManager.SetPods([]*v1.Pod{})

	assert.NoError(t, kubelet.volumeManager.WaitForUnmount(context.Background(), pod))

	// Verify volumes unmounted
	hasMountedVolumes := kubelet.volumeManager.HasPossiblyMountedVolumesForPod(
		util.GetUniquePodName(pod))

	assert.False(t, hasMountedVolumes, "Expected volumes to be unmounted and detached. But some volumes are still mounted")

	assert.NoError(t, volumetest.VerifyTearDownCallCount(
		1 /* expectedTearDownCallCount */, testKubelet.volumePlugin))

	// Verify volumes detached and no longer reported as in use
	assert.NoError(t, waitForVolumeDetach(v1.UniqueVolumeName("fake/fake-device"), kubelet.volumeManager))
	assert.GreaterOrEqual(t, testKubelet.volumePlugin.GetNewAttacherCallCount(), 1, "Expected plugin NewAttacher to be called at least once")
	assert.NoError(t, volumetest.VerifyDetachCallCount(
		1 /* expectedDetachCallCount */, testKubelet.volumePlugin))
}

func TestVolumeAttachAndMountControllerEnabled(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode.")
	}

	testKubelet := newTestKubelet(t, true /* controllerAttachDetachEnabled */)
	defer testKubelet.Cleanup()
	kubelet := testKubelet.kubelet
	kubeClient := testKubelet.fakeKubeClient
	kubeClient.AddReactor("get", "nodes",
		func(action core.Action) (bool, runtime.Object, error) {
			return true, &v1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: testKubeletHostname},
				Status: v1.NodeStatus{
					VolumesAttached: []v1.AttachedVolume{
						{
							Name:       "fake/fake-device",
							DevicePath: "fake/path",
						},
					}},
			}, nil
		})
	kubeClient.AddReactor("*", "*", func(action core.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("no reaction implemented for %s", action)
	})

	pod := podWithUIDNameNsSpec("12345678", "foo", "test", v1.PodSpec{
		Containers: []v1.Container{
			{
				Name: "container1",
				VolumeMounts: []v1.VolumeMount{
					{
						Name:      "vol1",
						MountPath: "/mnt/vol1",
					},
				},
			},
		},
		Volumes: []v1.Volume{
			{
				Name: "vol1",
				VolumeSource: v1.VolumeSource{
					RBD: &v1.RBDVolumeSource{
						RBDImage: "fake-device",
					},
				},
			},
		},
	})

	tCtx := ktesting.Init(t)
	defer tCtx.Cancel("test has completed")
	go kubelet.volumeManager.Run(tCtx, kubelet.sourcesReady)

	kubelet.podManager.SetPods([]*v1.Pod{pod})

	// Fake node status update
	go simulateVolumeInUseUpdate(
		v1.UniqueVolumeName("fake/fake-device"),
		tCtx.Done(),
		kubelet.volumeManager)

	assert.NoError(t, kubelet.volumeManager.WaitForAttachAndMount(context.Background(), pod))

	podVolumes := kubelet.volumeManager.GetMountedVolumesForPod(
		util.GetUniquePodName(pod))
	hasVolumes := kubelet.volumeManager.HasPossiblyMountedVolumesForPod(
		util.GetUniquePodName(pod))
	assert.True(t, hasVolumes, "HasPossiblyMountedVolumesForPod should return true")

	expectedPodVolumes := []string{"vol1"}
	assert.Len(t, podVolumes, len(expectedPodVolumes), "Volumes for pod %+v", pod)
	for _, name := range expectedPodVolumes {
		assert.Contains(t, podVolumes, name, "Volumes for pod %+v", pod)
	}
	assert.GreaterOrEqual(t, testKubelet.volumePlugin.GetNewAttacherCallCount(), 1, "Expected plugin NewAttacher to be called at least once")
	assert.NoError(t, volumetest.VerifyWaitForAttachCallCount(
		1 /* expectedWaitForAttachCallCount */, testKubelet.volumePlugin))
	assert.NoError(t, volumetest.VerifyZeroAttachCalls(testKubelet.volumePlugin))
	assert.NoError(t, volumetest.VerifyMountDeviceCallCount(
		1 /* expectedMountDeviceCallCount */, testKubelet.volumePlugin))
	assert.NoError(t, volumetest.VerifySetUpCallCount(
		1 /* expectedSetUpCallCount */, testKubelet.volumePlugin))
}

func TestVolumeUnmountAndDetachControllerEnabled(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping test in short mode.")
	}

	testKubelet := newTestKubelet(t, true /* controllerAttachDetachEnabled */)
	defer testKubelet.Cleanup()
	kubelet := testKubelet.kubelet
	kubeClient := testKubelet.fakeKubeClient
	kubeClient.AddReactor("get", "nodes",
		func(action core.Action) (bool, runtime.Object, error) {
			return true, &v1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: testKubeletHostname},
				Status: v1.NodeStatus{
					VolumesAttached: []v1.AttachedVolume{
						{
							Name:       "fake/fake-device",
							DevicePath: "fake/path",
						},
					}},
			}, nil
		})
	kubeClient.AddReactor("*", "*", func(action core.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("no reaction implemented for %s", action)
	})

	pod := podWithUIDNameNsSpec("12345678", "foo", "test", v1.PodSpec{
		Containers: []v1.Container{
			{
				Name: "container1",
				VolumeMounts: []v1.VolumeMount{
					{
						Name:      "vol1",
						MountPath: "/mnt/vol1",
					},
				},
			},
		},
		Volumes: []v1.Volume{
			{
				Name: "vol1",
				VolumeSource: v1.VolumeSource{
					RBD: &v1.RBDVolumeSource{
						RBDImage: "fake-device",
					},
				},
			},
		},
	})

	tCtx := ktesting.Init(t)
	defer tCtx.Cancel("test has completed")
	go kubelet.volumeManager.Run(tCtx, kubelet.sourcesReady)

	// Add pod
	kubelet.podManager.SetPods([]*v1.Pod{pod})

	// Fake node status update
	go simulateVolumeInUseUpdate(
		v1.UniqueVolumeName("fake/fake-device"),
		tCtx.Done(),
		kubelet.volumeManager)

	// Verify volumes attached
	assert.NoError(t, kubelet.volumeManager.WaitForAttachAndMount(context.Background(), pod))

	podVolumes := kubelet.volumeManager.GetMountedVolumesForPod(
		util.GetUniquePodName(pod))
	hasVolumes := kubelet.volumeManager.HasPossiblyMountedVolumesForPod(
		util.GetUniquePodName(pod))
	assert.True(t, hasVolumes, "HasPossiblyMountedVolumesForPod should return true")

	expectedPodVolumes := []string{"vol1"}
	assert.Len(t, podVolumes, len(expectedPodVolumes), "Volumes for pod %+v", pod)
	for _, name := range expectedPodVolumes {
		assert.Contains(t, podVolumes, name, "Volumes for pod %+v", pod)
	}

	assert.GreaterOrEqual(t, testKubelet.volumePlugin.GetNewAttacherCallCount(), 1, "Expected plugin NewAttacher to be called at least once")
	assert.NoError(t, volumetest.VerifyWaitForAttachCallCount(
		1 /* expectedWaitForAttachCallCount */, testKubelet.volumePlugin))
	assert.NoError(t, volumetest.VerifyZeroAttachCalls(testKubelet.volumePlugin))
	assert.NoError(t, volumetest.VerifyMountDeviceCallCount(
		1 /* expectedMountDeviceCallCount */, testKubelet.volumePlugin))
	assert.NoError(t, volumetest.VerifySetUpCallCount(
		1 /* expectedSetUpCallCount */, testKubelet.volumePlugin))

	// Remove pod
	kubelet.podWorkers.(*fakePodWorkers).setPodRuntimeBeRemoved(pod.UID)
	kubelet.podManager.SetPods([]*v1.Pod{})

	assert.NoError(t, waitForVolumeUnmount(kubelet.volumeManager, pod))

	// Verify volumes unmounted
	podVolumes = kubelet.volumeManager.GetMountedVolumesForPod(
		util.GetUniquePodName(pod))
	hasVolumes = kubelet.volumeManager.HasPossiblyMountedVolumesForPod(
		util.GetUniquePodName(pod))
	assert.False(t, hasVolumes, "HasPossiblyMountedVolumesForPod should return false")

	assert.Empty(t, podVolumes,
		"Expected volumes to be unmounted and detached. But some volumes are still mounted")

	assert.NoError(t, volumetest.VerifyTearDownCallCount(
		1 /* expectedTearDownCallCount */, testKubelet.volumePlugin))

	// Verify volumes detached and no longer reported as in use
	assert.NoError(t, waitForVolumeDetach(v1.UniqueVolumeName("fake/fake-device"), kubelet.volumeManager))
	assert.GreaterOrEqual(t, testKubelet.volumePlugin.GetNewAttacherCallCount(), 1, "Expected plugin NewAttacher to be called at least once")
	assert.NoError(t, volumetest.VerifyZeroDetachCallCount(testKubelet.volumePlugin))
}

type stubVolume struct {
	path       string
	attributes volume.Attributes
	volume.MetricsNil
}

func (f *stubVolume) GetPath() string {
	return f.path
}

func (f *stubVolume) GetAttributes() volume.Attributes {
	return f.attributes
}

func (f *stubVolume) SetUp(mounterArgs volume.MounterArgs) error {
	return nil
}

func (f *stubVolume) SetUpAt(dir string, mounterArgs volume.MounterArgs) error {
	return nil
}

type stubBlockVolume struct {
	dirPath string
	volName string
}

func (f *stubBlockVolume) GetGlobalMapPath(spec *volume.Spec) (string, error) {
	return "", nil
}

func (f *stubBlockVolume) GetPodDeviceMapPath() (string, string) {
	return f.dirPath, f.volName
}

func (f *stubBlockVolume) SetUpDevice() (string, error) {
	return "", nil
}

func (f stubBlockVolume) MapPodDevice() error {
	return nil
}

func (f *stubBlockVolume) TearDownDevice(mapPath string, devicePath string) error {
	return nil
}

func (f *stubBlockVolume) UnmapPodDevice() error {
	return nil
}

func (f *stubBlockVolume) SupportsMetrics() bool {
	return false
}

func (f *stubBlockVolume) GetMetrics() (*volume.Metrics, error) {
	return nil, nil
}
