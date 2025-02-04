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

package reconciler

import (
	"crypto/md5"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	csitrans "k8s.io/csi-translation-lib"
	"k8s.io/kubernetes/pkg/volume/csimigration"

	"github.com/stretchr/testify/assert"
	"k8s.io/mount-utils"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes/fake"
	core "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	"k8s.io/klog/v2/ktesting"
	_ "k8s.io/klog/v2/ktesting/init"
	"k8s.io/kubernetes/pkg/kubelet/volumemanager/cache"
	"k8s.io/kubernetes/pkg/volume"
	volumetesting "k8s.io/kubernetes/pkg/volume/testing"
	"k8s.io/kubernetes/pkg/volume/util"
	"k8s.io/kubernetes/pkg/volume/util/hostutil"
	"k8s.io/kubernetes/pkg/volume/util/operationexecutor"
	"k8s.io/kubernetes/pkg/volume/util/types"
)

const (
	// reconcilerLoopSleepDuration is the amount of time the reconciler loop
	// waits between successive executions
	reconcilerLoopSleepDuration = 1 * time.Nanosecond
	// waitForAttachTimeout is the maximum amount of time a
	// operationexecutor.Mount call will wait for a volume to be attached.
	waitForAttachTimeout         = 1 * time.Second
	nodeName                     = k8stypes.NodeName("mynodename")
	kubeletPodsDir               = "fake-dir"
	testOperationBackOffDuration = 100 * time.Millisecond
	reconcilerSyncWaitDuration   = 10 * time.Second
)

func hasAddedPods() bool { return true }

// Calls Run()
// Verifies there are no calls to attach, detach, mount, unmount, etc.
func Test_Run_Positive_DoNothing(t *testing.T) {
	// Arrange
	volumePluginMgr, fakePlugin := volumetesting.GetTestKubeletVolumePluginMgr(t)
	seLinuxTranslator := util.NewFakeSELinuxLabelTranslator()
	dsw := cache.NewDesiredStateOfWorld(volumePluginMgr, seLinuxTranslator)
	asw := cache.NewActualStateOfWorld(nodeName, volumePluginMgr)
	kubeClient := createTestClient()
	fakeRecorder := &record.FakeRecorder{}
	fakeHandler := volumetesting.NewBlockVolumePathHandler()
	oex := operationexecutor.NewOperationExecutor(operationexecutor.NewOperationGenerator(
		kubeClient,
		volumePluginMgr,
		fakeRecorder,
		fakeHandler,
	))
	reconciler := NewReconciler(
		kubeClient,
		false, /* controllerAttachDetachEnabled */
		reconcilerLoopSleepDuration,
		waitForAttachTimeout,
		nodeName,
		dsw,
		asw,
		hasAddedPods,
		oex,
		mount.NewFakeMounter(nil),
		hostutil.NewFakeHostUtil(nil),
		volumePluginMgr,
		kubeletPodsDir)

	// Act
	runReconciler(reconciler)

	// Assert
	assert.NoError(t, volumetesting.VerifyZeroAttachCalls(fakePlugin))
	assert.NoError(t, volumetesting.VerifyZeroWaitForAttachCallCount(fakePlugin))
	assert.NoError(t, volumetesting.VerifyZeroMountDeviceCallCount(fakePlugin))
	assert.NoError(t, volumetesting.VerifyZeroSetUpCallCount(fakePlugin))
	assert.NoError(t, volumetesting.VerifyZeroTearDownCallCount(fakePlugin))
	assert.NoError(t, volumetesting.VerifyZeroDetachCallCount(fakePlugin))
}

// Populates desiredStateOfWorld cache with one volume/pod.
// Calls Run()
// Verifies there is are attach/mount/etc calls and no detach/unmount calls.
func Test_Run_Positive_VolumeAttachAndMount(t *testing.T) {
	// Arrange
	volumePluginMgr, fakePlugin := volumetesting.GetTestKubeletVolumePluginMgr(t)
	seLinuxTranslator := util.NewFakeSELinuxLabelTranslator()
	dsw := cache.NewDesiredStateOfWorld(volumePluginMgr, seLinuxTranslator)
	asw := cache.NewActualStateOfWorld(nodeName, volumePluginMgr)
	kubeClient := createTestClient()
	fakeRecorder := &record.FakeRecorder{}
	fakeHandler := volumetesting.NewBlockVolumePathHandler()
	oex := operationexecutor.NewOperationExecutor(operationexecutor.NewOperationGenerator(
		kubeClient,
		volumePluginMgr,
		fakeRecorder,
		fakeHandler))
	reconciler := NewReconciler(
		kubeClient,
		false, /* controllerAttachDetachEnabled */
		reconcilerLoopSleepDuration,
		waitForAttachTimeout,
		nodeName,
		dsw,
		asw,
		hasAddedPods,
		oex,
		mount.NewFakeMounter(nil),
		hostutil.NewFakeHostUtil(nil),
		volumePluginMgr,
		kubeletPodsDir)
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pod1",
			UID:  "pod1uid",
		},
		Spec: v1.PodSpec{
			Volumes: []v1.Volume{
				{
					Name: "volume-name",
					VolumeSource: v1.VolumeSource{
						GCEPersistentDisk: &v1.GCEPersistentDiskVolumeSource{
							PDName: "fake-device1",
						},
					},
				},
			},
		},
	}

	volumeSpec := &volume.Spec{Volume: &pod.Spec.Volumes[0]}
	podName := util.GetUniquePodName(pod)
	generatedVolumeName, err := dsw.AddPodToVolume(
		podName, pod, volumeSpec, volumeSpec.Name(), "" /* volumeGIDValue */, nil /* seLinuxLabel */)

	// Assert
	if err != nil {
		t.Fatalf("AddPodToVolume failed. Expected: <no error> Actual: <%v>", err)
	}

	// Act
	runReconciler(reconciler)
	waitForMount(t, fakePlugin, generatedVolumeName, asw)
	// Assert
	assert.NoError(t, volumetesting.VerifyAttachCallCount(
		1 /* expectedAttachCallCount */, fakePlugin))
	assert.NoError(t, volumetesting.VerifyWaitForAttachCallCount(
		1 /* expectedWaitForAttachCallCount */, fakePlugin))
	assert.NoError(t, volumetesting.VerifyMountDeviceCallCount(
		1 /* expectedMountDeviceCallCount */, fakePlugin))
	assert.NoError(t, volumetesting.VerifySetUpCallCount(
		1 /* expectedSetUpCallCount */, fakePlugin))
	assert.NoError(t, volumetesting.VerifyZeroTearDownCallCount(fakePlugin))
	assert.NoError(t, volumetesting.VerifyZeroDetachCallCount(fakePlugin))
}

// Populates desiredStateOfWorld cache with one volume/pod.
// Calls Run()
// Verifies there is are attach/mount/etc calls and no detach/unmount calls.
func Test_Run_Positive_VolumeAttachAndMountMigrationEnabled(t *testing.T) {
	// Arrange
	logger, _ := ktesting.NewTestContext(t)
	intreeToCSITranslator := csitrans.New()
	node := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: string(nodeName),
		},
		Spec: v1.NodeSpec{},
		Status: v1.NodeStatus{
			VolumesAttached: []v1.AttachedVolume{
				{
					Name:       v1.UniqueVolumeName(fmt.Sprintf("fake-plugin/%s", "pd.csi.storage.gke.io-fake-device1")),
					DevicePath: "fake/path",
				},
			},
		},
	}
	volumePluginMgr, fakePlugin := volumetesting.GetTestKubeletVolumePluginMgrWithNode(t, node)
	seLinuxTranslator := util.NewFakeSELinuxLabelTranslator()
	dsw := cache.NewDesiredStateOfWorld(volumePluginMgr, seLinuxTranslator)

	asw := cache.NewActualStateOfWorld(nodeName, volumePluginMgr)
	kubeClient := createTestClient(v1.AttachedVolume{
		Name:       v1.UniqueVolumeName(fmt.Sprintf("fake-plugin/%s", "pd.csi.storage.gke.io-fake-device1")),
		DevicePath: "fake/path",
	})

	fakeRecorder := &record.FakeRecorder{}
	fakeHandler := volumetesting.NewBlockVolumePathHandler()
	oex := operationexecutor.NewOperationExecutor(operationexecutor.NewOperationGenerator(
		kubeClient,
		volumePluginMgr,
		fakeRecorder,
		fakeHandler))
	reconciler := NewReconciler(
		kubeClient,
		true, /* controllerAttachDetachEnabled */
		reconcilerLoopSleepDuration,
		waitForAttachTimeout,
		nodeName,
		dsw,
		asw,
		hasAddedPods,
		oex,
		mount.NewFakeMounter(nil),
		hostutil.NewFakeHostUtil(nil),
		volumePluginMgr,
		kubeletPodsDir)
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pod1",
			UID:  "pod1uid",
		},
		Spec: v1.PodSpec{
			Volumes: []v1.Volume{
				{
					Name: "volume-name",
					VolumeSource: v1.VolumeSource{
						GCEPersistentDisk: &v1.GCEPersistentDiskVolumeSource{
							PDName: "fake-device1",
						},
					},
				},
			},
		},
	}

	volumeSpec := &volume.Spec{Volume: &pod.Spec.Volumes[0]}
	migratedSpec, err := csimigration.TranslateInTreeSpecToCSI(logger, volumeSpec, pod.Namespace, intreeToCSITranslator)
	if err != nil {
		t.Fatalf("unexpected error while translating spec %v: %v", volumeSpec, err)
	}

	podName := util.GetUniquePodName(pod)
	generatedVolumeName, err := dsw.AddPodToVolume(
		podName,
		pod,
		migratedSpec,
		migratedSpec.Name(),
		"",  /* volumeGIDValue */
		nil, /* SELinuxContexts */
	)

	// Assert
	if err != nil {
		t.Fatalf("AddPodToVolume failed. Expected: <no error> Actual: <%v>", err)
	}
	dsw.MarkVolumesReportedInUse([]v1.UniqueVolumeName{generatedVolumeName})

	// Act
	runReconciler(reconciler)
	waitForMount(t, fakePlugin, generatedVolumeName, asw)
	// Assert
	assert.NoError(t, volumetesting.VerifyWaitForAttachCallCount(
		1 /* expectedWaitForAttachCallCount */, fakePlugin))
	assert.NoError(t, volumetesting.VerifyMountDeviceCallCount(
		1 /* expectedMountDeviceCallCount */, fakePlugin))
	assert.NoError(t, volumetesting.VerifySetUpCallCount(
		1 /* expectedSetUpCallCount */, fakePlugin))
	assert.NoError(t, volumetesting.VerifyZeroTearDownCallCount(fakePlugin))
	assert.NoError(t, volumetesting.VerifyZeroDetachCallCount(fakePlugin))
}

// Populates desiredStateOfWorld cache with one volume/pod.
// Enables controllerAttachDetachEnabled.
// Calls Run()
// Verifies there is one mount call and no unmount calls.
// Verifies there are no attach/detach calls.
func Test_Run_Positive_VolumeMountControllerAttachEnabled(t *testing.T) {
	// Arrange
	node := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: string(nodeName),
		},
		Status: v1.NodeStatus{
			VolumesAttached: []v1.AttachedVolume{
				{
					Name:       "fake-plugin/fake-device1",
					DevicePath: "fake/path",
				},
			},
		},
	}
	volumePluginMgr, fakePlugin := volumetesting.GetTestKubeletVolumePluginMgrWithNode(t, node)
	seLinuxTranslator := util.NewFakeSELinuxLabelTranslator()
	dsw := cache.NewDesiredStateOfWorld(volumePluginMgr, seLinuxTranslator)
	asw := cache.NewActualStateOfWorld(nodeName, volumePluginMgr)
	kubeClient := createTestClient()
	fakeRecorder := &record.FakeRecorder{}
	fakeHandler := volumetesting.NewBlockVolumePathHandler()
	oex := operationexecutor.NewOperationExecutor(operationexecutor.NewOperationGenerator(
		kubeClient,
		volumePluginMgr,
		fakeRecorder,
		fakeHandler))
	reconciler := NewReconciler(
		kubeClient,
		true, /* controllerAttachDetachEnabled */
		reconcilerLoopSleepDuration,
		waitForAttachTimeout,
		nodeName,
		dsw,
		asw,
		hasAddedPods,
		oex,
		mount.NewFakeMounter(nil),
		hostutil.NewFakeHostUtil(nil),
		volumePluginMgr,
		kubeletPodsDir)
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pod1",
			UID:  "pod1uid",
		},
		Spec: v1.PodSpec{
			Volumes: []v1.Volume{
				{
					Name: "volume-name",
					VolumeSource: v1.VolumeSource{
						GCEPersistentDisk: &v1.GCEPersistentDiskVolumeSource{
							PDName: "fake-device1",
						},
					},
				},
			},
		},
	}

	volumeSpec := &volume.Spec{Volume: &pod.Spec.Volumes[0]}
	podName := util.GetUniquePodName(pod)
	generatedVolumeName, err := dsw.AddPodToVolume(
		podName, pod, volumeSpec, volumeSpec.Name(), "" /* volumeGIDValue */, nil /* seLinuxLabel */)
	dsw.MarkVolumesReportedInUse([]v1.UniqueVolumeName{generatedVolumeName})

	// Assert
	if err != nil {
		t.Fatalf("AddPodToVolume failed. Expected: <no error> Actual: <%v>", err)
	}

	// Act
	runReconciler(reconciler)
	waitForMount(t, fakePlugin, generatedVolumeName, asw)

	// Assert
	assert.NoError(t, volumetesting.VerifyZeroAttachCalls(fakePlugin))
	assert.NoError(t, volumetesting.VerifyWaitForAttachCallCount(
		1 /* expectedWaitForAttachCallCount */, fakePlugin))
	assert.NoError(t, volumetesting.VerifyMountDeviceCallCount(
		1 /* expectedMountDeviceCallCount */, fakePlugin))
	assert.NoError(t, volumetesting.VerifySetUpCallCount(
		1 /* expectedSetUpCallCount */, fakePlugin))
	assert.NoError(t, volumetesting.VerifyZeroTearDownCallCount(fakePlugin))
	assert.NoError(t, volumetesting.VerifyZeroDetachCallCount(fakePlugin))
}

// Populates desiredStateOfWorld cache with one volume/pod.
// Enables controllerAttachDetachEnabled.
// volume is not reported-in-use
// Calls Run()
// Verifies that there is not wait-for-mount call
// Verifies that there is no exponential-backoff triggered
func Test_Run_Negative_VolumeMountControllerAttachEnabled(t *testing.T) {
	// Arrange
	volumePluginMgr, fakePlugin := volumetesting.GetTestKubeletVolumePluginMgr(t)
	seLinuxTranslator := util.NewFakeSELinuxLabelTranslator()
	dsw := cache.NewDesiredStateOfWorld(volumePluginMgr, seLinuxTranslator)
	asw := cache.NewActualStateOfWorld(nodeName, volumePluginMgr)
	kubeClient := createTestClient()
	fakeRecorder := &record.FakeRecorder{}
	fakeHandler := volumetesting.NewBlockVolumePathHandler()
	oex := operationexecutor.NewOperationExecutor(operationexecutor.NewOperationGenerator(
		kubeClient,
		volumePluginMgr,
		fakeRecorder,
		fakeHandler))
	reconciler := NewReconciler(
		kubeClient,
		true, /* controllerAttachDetachEnabled */
		reconcilerLoopSleepDuration,
		waitForAttachTimeout,
		nodeName,
		dsw,
		asw,
		hasAddedPods,
		oex,
		mount.NewFakeMounter(nil),
		hostutil.NewFakeHostUtil(nil),
		volumePluginMgr,
		kubeletPodsDir)
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pod1",
			UID:  "pod1uid",
		},
		Spec: v1.PodSpec{
			Volumes: []v1.Volume{
				{
					Name: "volume-name",
					VolumeSource: v1.VolumeSource{
						GCEPersistentDisk: &v1.GCEPersistentDiskVolumeSource{
							PDName: "fake-device1",
						},
					},
				},
			},
		},
	}

	volumeSpec := &volume.Spec{Volume: &pod.Spec.Volumes[0]}
	podName := util.GetUniquePodName(pod)
	generatedVolumeName, err := dsw.AddPodToVolume(
		podName, pod, volumeSpec, volumeSpec.Name(), "" /* volumeGIDValue */, nil /* seLinuxLabel */)

	// Assert
	if err != nil {
		t.Fatalf("AddPodToVolume failed. Expected: <no error> Actual: <%v>", err)
	}

	// Act
	runReconciler(reconciler)
	time.Sleep(reconcilerSyncWaitDuration)

	ok := oex.IsOperationSafeToRetry(generatedVolumeName, podName, nodeName, operationexecutor.VerifyControllerAttachedVolumeOpName)
	if !ok {
		t.Errorf("operation on volume %s is not safe to retry", generatedVolumeName)
	}

	// Assert
	assert.NoError(t, volumetesting.VerifyZeroAttachCalls(fakePlugin))
	assert.NoError(t, volumetesting.VerifyWaitForAttachCallCount(
		0 /* expectedWaitForAttachCallCount */, fakePlugin))
	assert.NoError(t, volumetesting.VerifyMountDeviceCallCount(
		0 /* expectedMountDeviceCallCount */, fakePlugin))
}

// Populates desiredStateOfWorld cache with one volume/pod.
// Calls Run()
// Verifies there is one attach/mount/etc call and no detach calls.
// Deletes volume/pod from desired state of world.
// Verifies detach/unmount calls are issued.
func Test_Run_Positive_VolumeAttachMountUnmountDetach(t *testing.T) {
	// Arrange
	volumePluginMgr, fakePlugin := volumetesting.GetTestKubeletVolumePluginMgr(t)
	seLinuxTranslator := util.NewFakeSELinuxLabelTranslator()
	dsw := cache.NewDesiredStateOfWorld(volumePluginMgr, seLinuxTranslator)
	asw := cache.NewActualStateOfWorld(nodeName, volumePluginMgr)
	kubeClient := createTestClient()
	fakeRecorder := &record.FakeRecorder{}
	fakeHandler := volumetesting.NewBlockVolumePathHandler()
	oex := operationexecutor.NewOperationExecutor(operationexecutor.NewOperationGenerator(
		kubeClient,
		volumePluginMgr,
		fakeRecorder,
		fakeHandler))
	reconciler := NewReconciler(
		kubeClient,
		false, /* controllerAttachDetachEnabled */
		reconcilerLoopSleepDuration,
		waitForAttachTimeout,
		nodeName,
		dsw,
		asw,
		hasAddedPods,
		oex,
		mount.NewFakeMounter(nil),
		hostutil.NewFakeHostUtil(nil),
		volumePluginMgr,
		kubeletPodsDir)
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pod1",
			UID:  "pod1uid",
		},
		Spec: v1.PodSpec{
			Volumes: []v1.Volume{
				{
					Name: "volume-name",
					VolumeSource: v1.VolumeSource{
						GCEPersistentDisk: &v1.GCEPersistentDiskVolumeSource{
							PDName: "fake-device1",
						},
					},
				},
			},
		},
	}

	volumeSpec := &volume.Spec{Volume: &pod.Spec.Volumes[0]}
	podName := util.GetUniquePodName(pod)
	generatedVolumeName, err := dsw.AddPodToVolume(
		podName, pod, volumeSpec, volumeSpec.Name(), "" /* volumeGIDValue */, nil /* seLinuxLabel */)

	// Assert
	if err != nil {
		t.Fatalf("AddPodToVolume failed. Expected: <no error> Actual: <%v>", err)
	}

	// Act
	runReconciler(reconciler)
	waitForMount(t, fakePlugin, generatedVolumeName, asw)
	// Assert
	assert.NoError(t, volumetesting.VerifyAttachCallCount(
		1 /* expectedAttachCallCount */, fakePlugin))
	assert.NoError(t, volumetesting.VerifyWaitForAttachCallCount(
		1 /* expectedWaitForAttachCallCount */, fakePlugin))
	assert.NoError(t, volumetesting.VerifyMountDeviceCallCount(
		1 /* expectedMountDeviceCallCount */, fakePlugin))
	assert.NoError(t, volumetesting.VerifySetUpCallCount(
		1 /* expectedSetUpCallCount */, fakePlugin))
	assert.NoError(t, volumetesting.VerifyZeroTearDownCallCount(fakePlugin))
	assert.NoError(t, volumetesting.VerifyZeroDetachCallCount(fakePlugin))

	// Act
	dsw.DeletePodFromVolume(podName, generatedVolumeName)
	waitForDetach(t, generatedVolumeName, asw)

	// Assert
	assert.NoError(t, volumetesting.VerifyTearDownCallCount(
		1 /* expectedTearDownCallCount */, fakePlugin))
	assert.NoError(t, volumetesting.VerifyDetachCallCount(
		1 /* expectedDetachCallCount */, fakePlugin))
}

// Populates desiredStateOfWorld cache with one volume/pod.
// Enables controllerAttachDetachEnabled.
// Calls Run()
// Verifies one mount call is made and no unmount calls.
// Deletes volume/pod from desired state of world.
// Verifies one unmount call is made.
// Verifies there are no attach/detach calls made.
func Test_Run_Positive_VolumeUnmountControllerAttachEnabled(t *testing.T) {
	// Arrange
	node := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: string(nodeName),
		},
		Status: v1.NodeStatus{
			VolumesAttached: []v1.AttachedVolume{
				{
					Name:       "fake-plugin/fake-device1",
					DevicePath: "fake/path",
				},
			},
		},
	}
	volumePluginMgr, fakePlugin := volumetesting.GetTestKubeletVolumePluginMgrWithNode(t, node)
	seLinuxTranslator := util.NewFakeSELinuxLabelTranslator()
	dsw := cache.NewDesiredStateOfWorld(volumePluginMgr, seLinuxTranslator)
	asw := cache.NewActualStateOfWorld(nodeName, volumePluginMgr)
	kubeClient := createTestClient()
	fakeRecorder := &record.FakeRecorder{}
	fakeHandler := volumetesting.NewBlockVolumePathHandler()
	oex := operationexecutor.NewOperationExecutor(operationexecutor.NewOperationGenerator(
		kubeClient,
		volumePluginMgr,
		fakeRecorder,
		fakeHandler))
	reconciler := NewReconciler(
		kubeClient,
		true, /* controllerAttachDetachEnabled */
		reconcilerLoopSleepDuration,
		waitForAttachTimeout,
		nodeName,
		dsw,
		asw,
		hasAddedPods,
		oex,
		mount.NewFakeMounter(nil),
		hostutil.NewFakeHostUtil(nil),
		volumePluginMgr,
		kubeletPodsDir)
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pod1",
			UID:  "pod1uid",
		},
		Spec: v1.PodSpec{
			Volumes: []v1.Volume{
				{
					Name: "volume-name",
					VolumeSource: v1.VolumeSource{
						GCEPersistentDisk: &v1.GCEPersistentDiskVolumeSource{
							PDName: "fake-device1",
						},
					},
				},
			},
		},
	}

	volumeSpec := &volume.Spec{Volume: &pod.Spec.Volumes[0]}
	podName := util.GetUniquePodName(pod)
	generatedVolumeName, err := dsw.AddPodToVolume(
		podName, pod, volumeSpec, volumeSpec.Name(), "" /* volumeGIDValue */, nil /* seLinuxLabel */)

	// Assert
	if err != nil {
		t.Fatalf("AddPodToVolume failed. Expected: <no error> Actual: <%v>", err)
	}

	// Act
	runReconciler(reconciler)

	dsw.MarkVolumesReportedInUse([]v1.UniqueVolumeName{generatedVolumeName})
	waitForMount(t, fakePlugin, generatedVolumeName, asw)

	// Assert
	assert.NoError(t, volumetesting.VerifyZeroAttachCalls(fakePlugin))
	assert.NoError(t, volumetesting.VerifyWaitForAttachCallCount(
		1 /* expectedWaitForAttachCallCount */, fakePlugin))
	assert.NoError(t, volumetesting.VerifyMountDeviceCallCount(
		1 /* expectedMountDeviceCallCount */, fakePlugin))
	assert.NoError(t, volumetesting.VerifySetUpCallCount(
		1 /* expectedSetUpCallCount */, fakePlugin))
	assert.NoError(t, volumetesting.VerifyZeroTearDownCallCount(fakePlugin))
	assert.NoError(t, volumetesting.VerifyZeroDetachCallCount(fakePlugin))

	// Act
	dsw.DeletePodFromVolume(podName, generatedVolumeName)
	waitForDetach(t, generatedVolumeName, asw)

	// Assert
	assert.NoError(t, volumetesting.VerifyTearDownCallCount(
		1 /* expectedTearDownCallCount */, fakePlugin))
	assert.NoError(t, volumetesting.VerifyZeroDetachCallCount(fakePlugin))
}

// Populates desiredStateOfWorld cache with one volume/pod.
// Calls Run()
// Verifies there are attach/get map paths/setupDevice calls and
// no detach/teardownDevice calls.
func Test_Run_Positive_VolumeAttachAndMap(t *testing.T) {
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod1",
			UID:       "pod1uid",
			Namespace: "ns",
		},
		Spec: v1.PodSpec{},
	}

	mode := v1.PersistentVolumeBlock
	gcepv := &v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{UID: "001", Name: "volume-name"},
		Spec: v1.PersistentVolumeSpec{
			Capacity:               v1.ResourceList{v1.ResourceName(v1.ResourceStorage): resource.MustParse("10G")},
			PersistentVolumeSource: v1.PersistentVolumeSource{GCEPersistentDisk: &v1.GCEPersistentDiskVolumeSource{PDName: "fake-device1"}},
			AccessModes: []v1.PersistentVolumeAccessMode{
				v1.ReadWriteOnce,
				v1.ReadOnlyMany,
			},
			VolumeMode: &mode,
			ClaimRef:   &v1.ObjectReference{Namespace: "ns", Name: "pvc-volume-name"},
		},
	}

	gcepvc := &v1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{UID: "pvc-001", Name: "pvc-volume-name", Namespace: "ns"},
		Spec: v1.PersistentVolumeClaimSpec{
			VolumeName: "volume-name",
			VolumeMode: &mode,
		},
		Status: v1.PersistentVolumeClaimStatus{
			Phase:    v1.ClaimBound,
			Capacity: gcepv.Spec.Capacity,
		},
	}

	// Arrange
	volumePluginMgr, fakePlugin := volumetesting.GetTestKubeletVolumePluginMgr(t)
	seLinuxTranslator := util.NewFakeSELinuxLabelTranslator()
	dsw := cache.NewDesiredStateOfWorld(volumePluginMgr, seLinuxTranslator)
	asw := cache.NewActualStateOfWorld(nodeName, volumePluginMgr)
	kubeClient := createtestClientWithPVPVC(gcepv, gcepvc)
	fakeRecorder := &record.FakeRecorder{}
	fakeHandler := volumetesting.NewBlockVolumePathHandler()
	oex := operationexecutor.NewOperationExecutor(operationexecutor.NewOperationGenerator(
		kubeClient,
		volumePluginMgr,
		fakeRecorder,
		fakeHandler))
	reconciler := NewReconciler(
		kubeClient,
		false, /* controllerAttachDetachEnabled */
		reconcilerLoopSleepDuration,
		waitForAttachTimeout,
		nodeName,
		dsw,
		asw,
		hasAddedPods,
		oex,
		mount.NewFakeMounter(nil),
		hostutil.NewFakeHostUtil(nil),
		volumePluginMgr,
		kubeletPodsDir)

	volumeSpec := &volume.Spec{
		PersistentVolume: gcepv,
	}
	podName := util.GetUniquePodName(pod)
	generatedVolumeName, err := dsw.AddPodToVolume(
		podName, pod, volumeSpec, volumeSpec.Name(), "" /* volumeGIDValue */, nil /* seLinuxLabel */)

	// Assert
	if err != nil {
		t.Fatalf("AddPodToVolume failed. Expected: <no error> Actual: <%v>", err)
	}

	// Act
	runReconciler(reconciler)
	waitForMount(t, fakePlugin, generatedVolumeName, asw)
	// Assert
	assert.NoError(t, volumetesting.VerifyAttachCallCount(
		1 /* expectedAttachCallCount */, fakePlugin))
	assert.NoError(t, volumetesting.VerifyWaitForAttachCallCount(
		1 /* expectedWaitForAttachCallCount */, fakePlugin))
	assert.NoError(t, volumetesting.VerifyGetMapPodDeviceCallCount(
		1 /* expectedGetMapDeviceCallCount */, fakePlugin))
	assert.NoError(t, volumetesting.VerifyZeroTearDownDeviceCallCount(fakePlugin))
	assert.NoError(t, volumetesting.VerifyZeroDetachCallCount(fakePlugin))
}

// Populates desiredStateOfWorld cache with one volume/pod.
// Enables controllerAttachDetachEnabled.
// Calls Run()
// Verifies there are two get map path calls, a setupDevice call
// and no teardownDevice call.
// Verifies there are no attach/detach calls.
func Test_Run_Positive_BlockVolumeMapControllerAttachEnabled(t *testing.T) {
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod1",
			UID:       "pod1uid",
			Namespace: "ns",
		},
		Spec: v1.PodSpec{},
	}

	mode := v1.PersistentVolumeBlock
	gcepv := &v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{UID: "001", Name: "volume-name"},
		Spec: v1.PersistentVolumeSpec{
			Capacity:               v1.ResourceList{v1.ResourceName(v1.ResourceStorage): resource.MustParse("10G")},
			PersistentVolumeSource: v1.PersistentVolumeSource{GCEPersistentDisk: &v1.GCEPersistentDiskVolumeSource{PDName: "fake-device1"}},
			AccessModes: []v1.PersistentVolumeAccessMode{
				v1.ReadWriteOnce,
				v1.ReadOnlyMany,
			},
			VolumeMode: &mode,
			ClaimRef:   &v1.ObjectReference{Namespace: "ns", Name: "pvc-volume-name"},
		},
	}
	gcepvc := &v1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{UID: "pvc-001", Name: "pvc-volume-name", Namespace: "ns"},
		Spec: v1.PersistentVolumeClaimSpec{
			VolumeName: "volume-name",
			VolumeMode: &mode,
		},
		Status: v1.PersistentVolumeClaimStatus{
			Phase:    v1.ClaimBound,
			Capacity: gcepv.Spec.Capacity,
		},
	}

	volumeSpec := &volume.Spec{
		PersistentVolume: gcepv,
	}
	node := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: string(nodeName),
		},
		Status: v1.NodeStatus{
			VolumesAttached: []v1.AttachedVolume{
				{
					Name:       "fake-plugin/fake-device1",
					DevicePath: "fake/path",
				},
			},
		},
	}

	// Arrange
	volumePluginMgr, fakePlugin := volumetesting.GetTestKubeletVolumePluginMgrWithNode(t, node)
	seLinuxTranslator := util.NewFakeSELinuxLabelTranslator()
	dsw := cache.NewDesiredStateOfWorld(volumePluginMgr, seLinuxTranslator)
	asw := cache.NewActualStateOfWorld(nodeName, volumePluginMgr)
	kubeClient := createtestClientWithPVPVC(gcepv, gcepvc, v1.AttachedVolume{
		Name:       "fake-plugin/fake-device1",
		DevicePath: "/fake/path",
	})
	fakeRecorder := &record.FakeRecorder{}
	fakeHandler := volumetesting.NewBlockVolumePathHandler()
	oex := operationexecutor.NewOperationExecutor(operationexecutor.NewOperationGenerator(
		kubeClient,
		volumePluginMgr,
		fakeRecorder,
		fakeHandler))
	reconciler := NewReconciler(
		kubeClient,
		true, /* controllerAttachDetachEnabled */
		reconcilerLoopSleepDuration,
		waitForAttachTimeout,
		nodeName,
		dsw,
		asw,
		hasAddedPods,
		oex,
		mount.NewFakeMounter(nil),
		hostutil.NewFakeHostUtil(nil),
		volumePluginMgr,
		kubeletPodsDir)

	podName := util.GetUniquePodName(pod)
	generatedVolumeName, err := dsw.AddPodToVolume(
		podName, pod, volumeSpec, volumeSpec.Name(), "" /* volumeGIDValue */, nil /* seLinuxLabel */)
	dsw.MarkVolumesReportedInUse([]v1.UniqueVolumeName{generatedVolumeName})

	// Assert
	if err != nil {
		t.Fatalf("AddPodToVolume failed. Expected: <no error> Actual: <%v>", err)
	}

	// Act
	runReconciler(reconciler)
	waitForMount(t, fakePlugin, generatedVolumeName, asw)

	// Assert
	assert.NoError(t, volumetesting.VerifyZeroAttachCalls(fakePlugin))
	assert.NoError(t, volumetesting.VerifyWaitForAttachCallCount(
		1 /* expectedWaitForAttachCallCount */, fakePlugin))
	assert.NoError(t, volumetesting.VerifyGetMapPodDeviceCallCount(
		1 /* expectedGetMapDeviceCallCount */, fakePlugin))
	assert.NoError(t, volumetesting.VerifyZeroTearDownDeviceCallCount(fakePlugin))
	assert.NoError(t, volumetesting.VerifyZeroDetachCallCount(fakePlugin))
}

// Populates desiredStateOfWorld cache with one volume/pod.
// Calls Run()
// Verifies there is one attach call, two get map path calls,
// setupDevice call and no detach calls.
// Deletes volume/pod from desired state of world.
// Verifies one detach/teardownDevice calls are issued.
func Test_Run_Positive_BlockVolumeAttachMapUnmapDetach(t *testing.T) {
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod1",
			UID:       "pod1uid",
			Namespace: "ns",
		},
		Spec: v1.PodSpec{},
	}

	mode := v1.PersistentVolumeBlock
	gcepv := &v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{UID: "001", Name: "volume-name"},
		Spec: v1.PersistentVolumeSpec{
			Capacity:               v1.ResourceList{v1.ResourceName(v1.ResourceStorage): resource.MustParse("10G")},
			PersistentVolumeSource: v1.PersistentVolumeSource{GCEPersistentDisk: &v1.GCEPersistentDiskVolumeSource{PDName: "fake-device1"}},
			AccessModes: []v1.PersistentVolumeAccessMode{
				v1.ReadWriteOnce,
				v1.ReadOnlyMany,
			},
			VolumeMode: &mode,
			ClaimRef:   &v1.ObjectReference{Namespace: "ns", Name: "pvc-volume-name"},
		},
	}
	gcepvc := &v1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{UID: "pvc-001", Name: "pvc-volume-name", Namespace: "ns"},
		Spec: v1.PersistentVolumeClaimSpec{
			VolumeName: "volume-name",
			VolumeMode: &mode,
		},
		Status: v1.PersistentVolumeClaimStatus{
			Phase:    v1.ClaimBound,
			Capacity: gcepv.Spec.Capacity,
		},
	}

	volumeSpec := &volume.Spec{
		PersistentVolume: gcepv,
	}

	// Arrange
	volumePluginMgr, fakePlugin := volumetesting.GetTestKubeletVolumePluginMgr(t)
	seLinuxTranslator := util.NewFakeSELinuxLabelTranslator()
	dsw := cache.NewDesiredStateOfWorld(volumePluginMgr, seLinuxTranslator)
	asw := cache.NewActualStateOfWorld(nodeName, volumePluginMgr)
	kubeClient := createtestClientWithPVPVC(gcepv, gcepvc)
	fakeRecorder := &record.FakeRecorder{}
	fakeHandler := volumetesting.NewBlockVolumePathHandler()
	oex := operationexecutor.NewOperationExecutor(operationexecutor.NewOperationGenerator(
		kubeClient,
		volumePluginMgr,
		fakeRecorder,
		fakeHandler))
	reconciler := NewReconciler(
		kubeClient,
		false, /* controllerAttachDetachEnabled */
		reconcilerLoopSleepDuration,
		waitForAttachTimeout,
		nodeName,
		dsw,
		asw,
		hasAddedPods,
		oex,
		mount.NewFakeMounter(nil),
		hostutil.NewFakeHostUtil(nil),
		volumePluginMgr,
		kubeletPodsDir)

	podName := util.GetUniquePodName(pod)
	generatedVolumeName, err := dsw.AddPodToVolume(
		podName, pod, volumeSpec, volumeSpec.Name(), "" /* volumeGIDValue */, nil /* seLinuxLabel */)

	// Assert
	if err != nil {
		t.Fatalf("AddPodToVolume failed. Expected: <no error> Actual: <%v>", err)
	}

	// Act
	runReconciler(reconciler)
	waitForMount(t, fakePlugin, generatedVolumeName, asw)
	// Assert
	assert.NoError(t, volumetesting.VerifyAttachCallCount(
		1 /* expectedAttachCallCount */, fakePlugin))
	assert.NoError(t, volumetesting.VerifyWaitForAttachCallCount(
		1 /* expectedWaitForAttachCallCount */, fakePlugin))
	assert.NoError(t, volumetesting.VerifyGetMapPodDeviceCallCount(
		1 /* expectedGetMapDeviceCallCount */, fakePlugin))
	assert.NoError(t, volumetesting.VerifyZeroTearDownDeviceCallCount(fakePlugin))
	assert.NoError(t, volumetesting.VerifyZeroDetachCallCount(fakePlugin))

	// Act
	dsw.DeletePodFromVolume(podName, generatedVolumeName)
	waitForDetach(t, generatedVolumeName, asw)

	// Assert
	assert.NoError(t, volumetesting.VerifyTearDownDeviceCallCount(
		1 /* expectedTearDownDeviceCallCount */, fakePlugin))
	assert.NoError(t, volumetesting.VerifyDetachCallCount(
		1 /* expectedDetachCallCount */, fakePlugin))
}

// Populates desiredStateOfWorld cache with one volume/pod.
// Enables controllerAttachDetachEnabled.
// Calls Run()
// Verifies two map path calls are made and no teardownDevice/detach calls.
// Deletes volume/pod from desired state of world.
// Verifies one teardownDevice call is made.
// Verifies there are no attach/detach calls made.
func Test_Run_Positive_VolumeUnmapControllerAttachEnabled(t *testing.T) {
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod1",
			UID:       "pod1uid",
			Namespace: "ns",
		},
		Spec: v1.PodSpec{},
	}

	mode := v1.PersistentVolumeBlock
	gcepv := &v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{UID: "001", Name: "volume-name"},
		Spec: v1.PersistentVolumeSpec{
			Capacity:               v1.ResourceList{v1.ResourceName(v1.ResourceStorage): resource.MustParse("10G")},
			PersistentVolumeSource: v1.PersistentVolumeSource{GCEPersistentDisk: &v1.GCEPersistentDiskVolumeSource{PDName: "fake-device1"}},
			AccessModes: []v1.PersistentVolumeAccessMode{
				v1.ReadWriteOnce,
				v1.ReadOnlyMany,
			},
			VolumeMode: &mode,
			ClaimRef:   &v1.ObjectReference{Namespace: "ns", Name: "pvc-volume-name"},
		},
	}
	gcepvc := &v1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{UID: "pvc-001", Name: "pvc-volume-name", Namespace: "ns"},
		Spec: v1.PersistentVolumeClaimSpec{
			VolumeName: "volume-name",
			VolumeMode: &mode,
		},
		Status: v1.PersistentVolumeClaimStatus{
			Phase:    v1.ClaimBound,
			Capacity: gcepv.Spec.Capacity,
		},
	}

	volumeSpec := &volume.Spec{
		PersistentVolume: gcepv,
	}

	node := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: string(nodeName),
		},
		Status: v1.NodeStatus{
			VolumesAttached: []v1.AttachedVolume{
				{
					Name:       "fake-plugin/fake-device1",
					DevicePath: "/fake/path",
				},
			},
		},
	}

	// Arrange
	volumePluginMgr, fakePlugin := volumetesting.GetTestKubeletVolumePluginMgrWithNode(t, node)
	seLinuxTranslator := util.NewFakeSELinuxLabelTranslator()
	dsw := cache.NewDesiredStateOfWorld(volumePluginMgr, seLinuxTranslator)
	asw := cache.NewActualStateOfWorld(nodeName, volumePluginMgr)
	kubeClient := createtestClientWithPVPVC(gcepv, gcepvc, v1.AttachedVolume{
		Name:       "fake-plugin/fake-device1",
		DevicePath: "/fake/path",
	})
	fakeRecorder := &record.FakeRecorder{}
	fakeHandler := volumetesting.NewBlockVolumePathHandler()
	oex := operationexecutor.NewOperationExecutor(operationexecutor.NewOperationGenerator(
		kubeClient,
		volumePluginMgr,
		fakeRecorder,
		fakeHandler))
	reconciler := NewReconciler(
		kubeClient,
		true, /* controllerAttachDetachEnabled */
		reconcilerLoopSleepDuration,
		waitForAttachTimeout,
		nodeName,
		dsw,
		asw,
		hasAddedPods,
		oex,
		mount.NewFakeMounter(nil),
		hostutil.NewFakeHostUtil(nil),
		volumePluginMgr,
		kubeletPodsDir)

	podName := util.GetUniquePodName(pod)
	generatedVolumeName, err := dsw.AddPodToVolume(
		podName, pod, volumeSpec, volumeSpec.Name(), "" /* volumeGIDValue */, nil /* seLinuxLabel */)

	// Assert
	if err != nil {
		t.Fatalf("AddPodToVolume failed. Expected: <no error> Actual: <%v>", err)
	}

	// Act
	runReconciler(reconciler)

	dsw.MarkVolumesReportedInUse([]v1.UniqueVolumeName{generatedVolumeName})
	waitForMount(t, fakePlugin, generatedVolumeName, asw)

	// Assert
	assert.NoError(t, volumetesting.VerifyZeroAttachCalls(fakePlugin))
	assert.NoError(t, volumetesting.VerifyWaitForAttachCallCount(
		1 /* expectedWaitForAttachCallCount */, fakePlugin))
	assert.NoError(t, volumetesting.VerifyGetMapPodDeviceCallCount(
		1 /* expectedGetMapDeviceCallCount */, fakePlugin))
	assert.NoError(t, volumetesting.VerifyZeroTearDownDeviceCallCount(fakePlugin))
	assert.NoError(t, volumetesting.VerifyZeroDetachCallCount(fakePlugin))

	// Act
	dsw.DeletePodFromVolume(podName, generatedVolumeName)
	waitForDetach(t, generatedVolumeName, asw)

	// Assert
	assert.NoError(t, volumetesting.VerifyTearDownDeviceCallCount(
		1 /* expectedTearDownDeviceCallCount */, fakePlugin))
	assert.NoError(t, volumetesting.VerifyZeroDetachCallCount(fakePlugin))
}

func Test_GenerateMapVolumeFunc_Plugin_Not_Found(t *testing.T) {
	testCases := map[string]struct {
		volumePlugins  []volume.VolumePlugin
		expectErr      bool
		expectedErrMsg string
	}{
		"volumePlugin is nil": {
			volumePlugins:  []volume.VolumePlugin{},
			expectErr:      true,
			expectedErrMsg: "MapVolume.FindMapperPluginBySpec failed",
		},
		"blockVolumePlugin is nil": {
			volumePlugins:  volumetesting.NewFakeFileVolumePlugin(),
			expectErr:      true,
			expectedErrMsg: "MapVolume.FindMapperPluginBySpec failed to find BlockVolumeMapper plugin. Volume plugin is nil.",
		},
	}

	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			volumePluginMgr := &volume.VolumePluginMgr{}
			volumePluginMgr.InitPlugins(tc.volumePlugins, nil, nil)
			asw := cache.NewActualStateOfWorld(nodeName, volumePluginMgr)
			oex := operationexecutor.NewOperationExecutor(operationexecutor.NewOperationGenerator(
				nil, /* kubeClient */
				volumePluginMgr,
				nil, /* fakeRecorder */
				nil))

			pod := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "pod1",
					UID:  "pod1uid",
				},
				Spec: v1.PodSpec{},
			}
			volumeMode := v1.PersistentVolumeBlock
			tmpSpec := &volume.Spec{PersistentVolume: &v1.PersistentVolume{Spec: v1.PersistentVolumeSpec{VolumeMode: &volumeMode}}}
			volumeToMount := operationexecutor.VolumeToMount{
				Pod:        pod,
				VolumeSpec: tmpSpec}
			err := oex.MountVolume(waitForAttachTimeout, volumeToMount, asw, false)
			// Assert
			if assert.Error(t, err) {
				assert.ErrorContains(t, err, tc.expectedErrMsg)
			}
		})
	}
}

func Test_GenerateUnmapVolumeFunc_Plugin_Not_Found(t *testing.T) {
	testCases := map[string]struct {
		volumePlugins  []volume.VolumePlugin
		expectErr      bool
		expectedErrMsg string
	}{
		"volumePlugin is nil": {
			volumePlugins:  []volume.VolumePlugin{},
			expectErr:      true,
			expectedErrMsg: "UnmapVolume.FindMapperPluginByName failed",
		},
		"blockVolumePlugin is nil": {
			volumePlugins:  volumetesting.NewFakeFileVolumePlugin(),
			expectErr:      true,
			expectedErrMsg: "UnmapVolume.FindMapperPluginByName failed to find BlockVolumeMapper plugin. Volume plugin is nil.",
		},
	}

	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			volumePluginMgr := &volume.VolumePluginMgr{}
			volumePluginMgr.InitPlugins(tc.volumePlugins, nil, nil)
			asw := cache.NewActualStateOfWorld(nodeName, volumePluginMgr)
			oex := operationexecutor.NewOperationExecutor(operationexecutor.NewOperationGenerator(
				nil, /* kubeClient */
				volumePluginMgr,
				nil, /* fakeRecorder */
				nil))
			volumeMode := v1.PersistentVolumeBlock
			tmpSpec := &volume.Spec{PersistentVolume: &v1.PersistentVolume{Spec: v1.PersistentVolumeSpec{VolumeMode: &volumeMode}}}
			volumeToUnmount := operationexecutor.MountedVolume{
				PluginName: "fake-file-plugin",
				VolumeSpec: tmpSpec}
			err := oex.UnmountVolume(volumeToUnmount, asw, "" /* podsDir */)
			// Assert
			if assert.Error(t, err) {
				assert.ErrorContains(t, err, tc.expectedErrMsg)
			}
		})
	}
}

func Test_GenerateUnmapDeviceFunc_Plugin_Not_Found(t *testing.T) {
	testCases := map[string]struct {
		volumePlugins  []volume.VolumePlugin
		expectErr      bool
		expectedErrMsg string
	}{
		"volumePlugin is nil": {
			volumePlugins:  []volume.VolumePlugin{},
			expectErr:      true,
			expectedErrMsg: "UnmapDevice.FindMapperPluginByName failed",
		},
		"blockVolumePlugin is nil": {
			volumePlugins:  volumetesting.NewFakeFileVolumePlugin(),
			expectErr:      true,
			expectedErrMsg: "UnmapDevice.FindMapperPluginByName failed to find BlockVolumeMapper plugin. Volume plugin is nil.",
		},
	}

	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			volumePluginMgr := &volume.VolumePluginMgr{}
			volumePluginMgr.InitPlugins(tc.volumePlugins, nil, nil)
			asw := cache.NewActualStateOfWorld(nodeName, volumePluginMgr)
			oex := operationexecutor.NewOperationExecutor(operationexecutor.NewOperationGenerator(
				nil, /* kubeClient */
				volumePluginMgr,
				nil, /* fakeRecorder */
				nil))
			var hostutil hostutil.HostUtils
			volumeMode := v1.PersistentVolumeBlock
			tmpSpec := &volume.Spec{PersistentVolume: &v1.PersistentVolume{Spec: v1.PersistentVolumeSpec{VolumeMode: &volumeMode}}}
			deviceToDetach := operationexecutor.AttachedVolume{VolumeSpec: tmpSpec, PluginName: "fake-file-plugin"}
			err := oex.UnmountDevice(deviceToDetach, asw, hostutil)
			// Assert
			if assert.Error(t, err) {
				assert.ErrorContains(t, err, tc.expectedErrMsg)
			}
		})
	}
}

// Populates desiredStateOfWorld cache with one volume/pod.
// Enables controllerAttachDetachEnabled.
// Calls Run()
// Wait for volume mounted.
// Mark volume as fsResizeRequired in ASW.
// Verifies volume's fsResizeRequired flag is cleared later.
func Test_Run_Positive_VolumeFSResizeControllerAttachEnabled(t *testing.T) {
	blockMode := v1.PersistentVolumeBlock
	fsMode := v1.PersistentVolumeFilesystem

	var tests = []struct {
		name            string
		volumeMode      *v1.PersistentVolumeMode
		expansionFailed bool
		uncertainTest   bool
		pvName          string
		pvcSize         resource.Quantity
		pvcStatusSize   resource.Quantity
		oldPVSize       resource.Quantity
		newPVSize       resource.Quantity
	}{
		{
			name:          "expand-fs-volume",
			volumeMode:    &fsMode,
			pvName:        "pv",
			pvcSize:       resource.MustParse("10G"),
			pvcStatusSize: resource.MustParse("10G"),
			newPVSize:     resource.MustParse("15G"),
			oldPVSize:     resource.MustParse("10G"),
		},
		{
			name:          "expand-raw-block",
			volumeMode:    &blockMode,
			pvName:        "pv",
			pvcSize:       resource.MustParse("10G"),
			pvcStatusSize: resource.MustParse("10G"),
			newPVSize:     resource.MustParse("15G"),
			oldPVSize:     resource.MustParse("10G"),
		},
		{
			name:            "expand-fs-volume with in-use error",
			volumeMode:      &fsMode,
			expansionFailed: true,
			pvName:          volumetesting.FailWithInUseVolumeName,
			pvcSize:         resource.MustParse("10G"),
			pvcStatusSize:   resource.MustParse("10G"),
			newPVSize:       resource.MustParse("15G"),
			oldPVSize:       resource.MustParse("13G"),
		},
		{
			name:            "expand-fs-volume with unsupported error",
			volumeMode:      &fsMode,
			expansionFailed: false,
			pvName:          volumetesting.FailWithUnSupportedVolumeName,
			pvcSize:         resource.MustParse("10G"),
			pvcStatusSize:   resource.MustParse("10G"),
			newPVSize:       resource.MustParse("15G"),
			oldPVSize:       resource.MustParse("13G"),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pv := getTestPV(tc.pvName, tc.volumeMode, tc.oldPVSize)
			pvc := getTestPVC("pv", tc.volumeMode, tc.pvcSize, tc.pvcStatusSize)
			pod := getTestPod(pvc.Name)

			// deep copy before reconciler runs to avoid data race.
			pvWithSize := pv.DeepCopy()
			node := &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: string(nodeName),
				},
				Spec: v1.NodeSpec{},
				Status: v1.NodeStatus{
					VolumesAttached: []v1.AttachedVolume{
						{
							Name:       v1.UniqueVolumeName(fmt.Sprintf("fake-plugin/%s", tc.pvName)),
							DevicePath: "fake/path",
						},
					},
				},
			}
			volumePluginMgr, fakePlugin := volumetesting.GetTestKubeletVolumePluginMgrWithNode(t, node)
			seLinuxTranslator := util.NewFakeSELinuxLabelTranslator()
			dsw := cache.NewDesiredStateOfWorld(volumePluginMgr, seLinuxTranslator)
			asw := cache.NewActualStateOfWorld(nodeName, volumePluginMgr)
			kubeClient := createtestClientWithPVPVC(pv, pvc, v1.AttachedVolume{
				Name:       v1.UniqueVolumeName(fmt.Sprintf("fake-plugin/%s", tc.pvName)),
				DevicePath: "fake/path",
			})
			fakeRecorder := &record.FakeRecorder{}
			fakeHandler := volumetesting.NewBlockVolumePathHandler()
			oex := operationexecutor.NewOperationExecutor(operationexecutor.NewOperationGenerator(
				kubeClient,
				volumePluginMgr,
				fakeRecorder,
				fakeHandler))

			reconciler := NewReconciler(
				kubeClient,
				true, /* controllerAttachDetachEnabled */
				reconcilerLoopSleepDuration,
				waitForAttachTimeout,
				nodeName,
				dsw,
				asw,
				hasAddedPods,
				oex,
				mount.NewFakeMounter(nil),
				hostutil.NewFakeHostUtil(nil),
				volumePluginMgr,
				kubeletPodsDir)

			volumeSpec := &volume.Spec{PersistentVolume: pv}
			podName := util.GetUniquePodName(pod)
			volumeName, err := dsw.AddPodToVolume(
				podName, pod, volumeSpec, volumeSpec.Name(), "" /* volumeGIDValue */, nil /* seLinuxLabel */)
			// Assert
			if err != nil {
				t.Fatalf("AddPodToVolume failed. Expected: <no error> Actual: <%v>", err)
			}
			dsw.MarkVolumesReportedInUse([]v1.UniqueVolumeName{volumeName})

			// Start the reconciler to fill ASW.
			stopChan, stoppedChan := make(chan struct{}), make(chan struct{})
			go func() {
				defer close(stoppedChan)
				reconciler.Run(stopChan)
			}()
			waitForMount(t, fakePlugin, volumeName, asw)
			// Stop the reconciler.
			close(stopChan)
			<-stoppedChan

			// Simulate what DSOWP does
			pvWithSize.Spec.Capacity[v1.ResourceStorage] = tc.newPVSize
			volumeSpec = &volume.Spec{PersistentVolume: pvWithSize}
			_, err = dsw.AddPodToVolume(podName, pod, volumeSpec, volumeSpec.Name(), "" /* volumeGIDValue */, nil /* seLinuxContexts */)
			if err != nil {
				t.Fatalf("AddPodToVolume failed. Expected: <no error> Actual: <%v>", err)
			}

			t.Logf("Changing size of the volume to %s", tc.newPVSize.String())
			newSize := tc.newPVSize.DeepCopy()
			dsw.UpdatePersistentVolumeSize(volumeName, newSize)

			_, _, podExistErr := asw.PodExistsInVolume(podName, volumeName, newSize, "" /* SELinuxLabel */)
			if tc.expansionFailed {
				if cache.IsFSResizeRequiredError(podExistErr) {
					t.Fatalf("volume %s should not throw fsResizeRequired error: %v", volumeName, podExistErr)
				}
			} else {
				if !cache.IsFSResizeRequiredError(podExistErr) {
					t.Fatalf("Volume should be marked as fsResizeRequired, but receive unexpected error: %v", podExistErr)
				}
				go reconciler.Run(wait.NeverStop)

				waitErr := retryWithExponentialBackOff(testOperationBackOffDuration, func() (done bool, err error) {
					mounted, _, err := asw.PodExistsInVolume(podName, volumeName, newSize, "" /* SELinuxContext */)
					return mounted && err == nil, nil
				})
				if waitErr != nil {
					t.Fatalf("Volume resize should succeeded %v", waitErr)
				}
			}

		})
	}
}

func getTestPVC(pvName string, volumeMode *v1.PersistentVolumeMode, specSize, statusSize resource.Quantity) *v1.PersistentVolumeClaim {
	pvc := &v1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pvc",
			UID:  "pvcuid",
		},
		Spec: v1.PersistentVolumeClaimSpec{
			Resources: v1.VolumeResourceRequirements{
				Requests: v1.ResourceList{
					v1.ResourceStorage: specSize,
				},
			},
			VolumeName: pvName,
			VolumeMode: volumeMode,
		},
		Status: v1.PersistentVolumeClaimStatus{
			Capacity: v1.ResourceList{
				v1.ResourceStorage: statusSize,
			},
		},
	}
	return pvc
}

func getTestPV(pvName string, volumeMode *v1.PersistentVolumeMode, pvSize resource.Quantity) *v1.PersistentVolume {
	pv := &v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: pvName,
			UID:  "pvuid",
		},
		Spec: v1.PersistentVolumeSpec{
			ClaimRef:   &v1.ObjectReference{Name: "pvc"},
			VolumeMode: volumeMode,
			Capacity: v1.ResourceList{
				v1.ResourceStorage: pvSize,
			},
		},
	}
	return pv
}

func getTestPod(claimName string) *v1.Pod {
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pod1",
			UID:  "pod1uid",
		},
		Spec: v1.PodSpec{
			Volumes: []v1.Volume{
				{
					Name: "volume-name",
					VolumeSource: v1.VolumeSource{
						PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{
							ClaimName: claimName,
						},
					},
				},
			},
		},
	}
	return pod
}

func Test_UncertainDeviceGlobalMounts(t *testing.T) {
	var tests = []struct {
		name                   string
		deviceState            operationexecutor.DeviceMountState
		unmountDeviceCallCount int
		volumeName             string
		supportRemount         bool
	}{
		{
			name:                   "timed out operations should result in device marked as uncertain",
			deviceState:            operationexecutor.DeviceMountUncertain,
			unmountDeviceCallCount: 1,
			volumeName:             volumetesting.TimeoutOnMountDeviceVolumeName,
		},
		{
			name:                   "failed operation should result in not-mounted device",
			deviceState:            operationexecutor.DeviceNotMounted,
			unmountDeviceCallCount: 0,
			volumeName:             volumetesting.FailMountDeviceVolumeName,
		},
		{
			name:                   "timeout followed by failed operation should result in non-mounted device",
			deviceState:            operationexecutor.DeviceNotMounted,
			unmountDeviceCallCount: 0,
			volumeName:             volumetesting.TimeoutAndFailOnMountDeviceVolumeName,
		},
		{
			name:                   "success followed by timeout operation should result in mounted device",
			deviceState:            operationexecutor.DeviceGloballyMounted,
			unmountDeviceCallCount: 1,
			volumeName:             volumetesting.SuccessAndTimeoutDeviceName,
			supportRemount:         true,
		},
		{
			name:                   "success followed by failed operation should result in mounted device",
			deviceState:            operationexecutor.DeviceGloballyMounted,
			unmountDeviceCallCount: 1,
			volumeName:             volumetesting.SuccessAndFailOnMountDeviceName,
			supportRemount:         true,
		},
	}

	modes := []v1.PersistentVolumeMode{v1.PersistentVolumeBlock, v1.PersistentVolumeFilesystem}

	for modeIndex := range modes {
		for tcIndex := range tests {
			mode := modes[modeIndex]
			tc := tests[tcIndex]
			testName := fmt.Sprintf("%s [%s]", tc.name, mode)
			uniqueTestString := fmt.Sprintf("global-mount-%s", testName)
			uniquePodDir := fmt.Sprintf("%s-%x", kubeletPodsDir, md5.Sum([]byte(uniqueTestString)))
			t.Run(testName+"[", func(t *testing.T) {
				t.Parallel()
				pv := &v1.PersistentVolume{
					ObjectMeta: metav1.ObjectMeta{
						Name: tc.volumeName,
						UID:  "pvuid",
					},
					Spec: v1.PersistentVolumeSpec{
						ClaimRef:   &v1.ObjectReference{Name: "pvc"},
						VolumeMode: &mode,
					},
				}
				pvc := &v1.PersistentVolumeClaim{
					ObjectMeta: metav1.ObjectMeta{
						Name: "pvc",
						UID:  "pvcuid",
					},
					Spec: v1.PersistentVolumeClaimSpec{
						VolumeName: tc.volumeName,
						VolumeMode: &mode,
					},
				}
				pod := &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name: "pod1",
						UID:  "pod1uid",
					},
					Spec: v1.PodSpec{
						Volumes: []v1.Volume{
							{
								Name: "volume-name",
								VolumeSource: v1.VolumeSource{
									PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{
										ClaimName: pvc.Name,
									},
								},
							},
						},
					},
				}

				node := &v1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: string(nodeName),
					},
					Spec: v1.NodeSpec{},
					Status: v1.NodeStatus{
						VolumesAttached: []v1.AttachedVolume{
							{
								Name:       v1.UniqueVolumeName(fmt.Sprintf("fake-plugin/%s", tc.volumeName)),
								DevicePath: "fake/path",
							},
						},
					},
				}
				volumePluginMgr, fakePlugin := volumetesting.GetTestKubeletVolumePluginMgrWithNode(t, node)
				fakePlugin.SupportsRemount = tc.supportRemount
				seLinuxTranslator := util.NewFakeSELinuxLabelTranslator()

				dsw := cache.NewDesiredStateOfWorld(volumePluginMgr, seLinuxTranslator)
				asw := cache.NewActualStateOfWorld(nodeName, volumePluginMgr)
				kubeClient := createtestClientWithPVPVC(pv, pvc, v1.AttachedVolume{
					Name:       v1.UniqueVolumeName(fmt.Sprintf("fake-plugin/%s", tc.volumeName)),
					DevicePath: "fake/path",
				})
				fakeRecorder := &record.FakeRecorder{}
				fakeHandler := volumetesting.NewBlockVolumePathHandler()
				oex := operationexecutor.NewOperationExecutor(operationexecutor.NewOperationGenerator(
					kubeClient,
					volumePluginMgr,
					fakeRecorder,
					fakeHandler))

				reconciler := NewReconciler(
					kubeClient,
					true, /* controllerAttachDetachEnabled */
					reconcilerLoopSleepDuration,
					waitForAttachTimeout,
					nodeName,
					dsw,
					asw,
					hasAddedPods,
					oex,
					&mount.FakeMounter{},
					hostutil.NewFakeHostUtil(nil),
					volumePluginMgr,
					uniquePodDir)
				volumeSpec := &volume.Spec{PersistentVolume: pv}
				podName := util.GetUniquePodName(pod)
				volumeName, err := dsw.AddPodToVolume(
					podName, pod, volumeSpec, volumeSpec.Name(), "" /* volumeGIDValue */, nil /* seLinuxLabel */)
				// Assert
				if err != nil {
					t.Fatalf("AddPodToVolume failed. Expected: <no error> Actual: <%v>", err)
				}
				dsw.MarkVolumesReportedInUse([]v1.UniqueVolumeName{volumeName})

				// Start the reconciler to fill ASW.
				stopChan, stoppedChan := make(chan struct{}), make(chan struct{})
				go func() {
					reconciler.Run(stopChan)
					close(stoppedChan)
				}()
				waitForVolumeToExistInASW(t, volumeName, asw)
				if tc.volumeName == volumetesting.TimeoutAndFailOnMountDeviceVolumeName {
					// Wait upto 10s for reconciler to catch up
					time.Sleep(reconcilerSyncWaitDuration)
				}

				if tc.volumeName == volumetesting.SuccessAndFailOnMountDeviceName ||
					tc.volumeName == volumetesting.SuccessAndTimeoutDeviceName {
					// wait for mount and then break it via remount
					waitForMount(t, fakePlugin, volumeName, asw)
					asw.MarkRemountRequired(podName)
					time.Sleep(reconcilerSyncWaitDuration)
				}

				if tc.deviceState == operationexecutor.DeviceMountUncertain {
					waitForUncertainGlobalMount(t, volumeName, asw)
				}

				if tc.deviceState == operationexecutor.DeviceGloballyMounted {
					waitForMount(t, fakePlugin, volumeName, asw)
				}

				dsw.DeletePodFromVolume(podName, volumeName)
				waitForDetach(t, volumeName, asw)
				if mode == v1.PersistentVolumeFilesystem {
					err = volumetesting.VerifyUnmountDeviceCallCount(tc.unmountDeviceCallCount, fakePlugin)
				} else {
					if tc.unmountDeviceCallCount == 0 {
						err = volumetesting.VerifyZeroTearDownDeviceCallCount(fakePlugin)
					} else {
						err = volumetesting.VerifyTearDownDeviceCallCount(tc.unmountDeviceCallCount, fakePlugin)
					}
				}
				if err != nil {
					t.Errorf("Error verifying UnMountDeviceCallCount: %v", err)
				}
			})
		}
	}
}

func Test_UncertainVolumeMountState(t *testing.T) {
	var tests = []struct {
		name                   string
		volumeState            operationexecutor.VolumeMountState
		unmountDeviceCallCount int
		unmountVolumeCount     int
		volumeName             string
		supportRemount         bool
		pvcStatusSize          resource.Quantity
		pvSize                 resource.Quantity
	}{
		{
			name:                   "timed out operations should result in volume marked as uncertain",
			volumeState:            operationexecutor.VolumeMountUncertain,
			unmountDeviceCallCount: 1,
			unmountVolumeCount:     1,
			volumeName:             volumetesting.TimeoutOnSetupVolumeName,
		},
		{
			name:                   "failed operation should result in not-mounted volume",
			volumeState:            operationexecutor.VolumeNotMounted,
			unmountDeviceCallCount: 1,
			unmountVolumeCount:     0,
			volumeName:             volumetesting.FailOnSetupVolumeName,
		},
		{
			name:                   "timeout followed by failed operation should result in non-mounted volume",
			volumeState:            operationexecutor.VolumeNotMounted,
			unmountDeviceCallCount: 1,
			unmountVolumeCount:     0,
			volumeName:             volumetesting.TimeoutAndFailOnSetupVolumeName,
		},
		{
			name:                   "success followed by timeout operation should result in mounted volume",
			volumeState:            operationexecutor.VolumeMounted,
			unmountDeviceCallCount: 1,
			unmountVolumeCount:     1,
			volumeName:             volumetesting.SuccessAndTimeoutSetupVolumeName,
			supportRemount:         true,
		},
		{
			name:                   "success followed by failed operation should result in mounted volume",
			volumeState:            operationexecutor.VolumeMounted,
			unmountDeviceCallCount: 1,
			unmountVolumeCount:     1,
			volumeName:             volumetesting.SuccessAndFailOnSetupVolumeName,
			supportRemount:         true,
		},
		{
			name:                   "mount success but fail to expand filesystem",
			volumeState:            operationexecutor.VolumeMountUncertain,
			unmountDeviceCallCount: 1,
			unmountVolumeCount:     1,
			volumeName:             volumetesting.FailVolumeExpansion,
			supportRemount:         true,
			pvSize:                 resource.MustParse("10G"),
			pvcStatusSize:          resource.MustParse("2G"),
		},
	}
	modes := []v1.PersistentVolumeMode{v1.PersistentVolumeBlock, v1.PersistentVolumeFilesystem}

	for modeIndex := range modes {
		for tcIndex := range tests {
			mode := modes[modeIndex]
			tc := tests[tcIndex]
			testName := fmt.Sprintf("%s [%s]", tc.name, mode)
			uniqueTestString := fmt.Sprintf("local-mount-%s", testName)
			uniquePodDir := fmt.Sprintf("%s-%x", kubeletPodsDir, md5.Sum([]byte(uniqueTestString)))
			t.Run(testName, func(t *testing.T) {
				t.Parallel()
				pv := &v1.PersistentVolume{
					ObjectMeta: metav1.ObjectMeta{
						Name: tc.volumeName,
						UID:  "pvuid",
					},
					Spec: v1.PersistentVolumeSpec{
						ClaimRef:   &v1.ObjectReference{Name: "pvc"},
						VolumeMode: &mode,
					},
				}
				if tc.pvSize.CmpInt64(0) > 0 {
					pv.Spec.Capacity = v1.ResourceList{
						v1.ResourceStorage: tc.pvSize,
					}
				}
				pvc := &v1.PersistentVolumeClaim{
					ObjectMeta: metav1.ObjectMeta{
						Name: "pvc",
						UID:  "pvcuid",
					},
					Spec: v1.PersistentVolumeClaimSpec{
						VolumeName: tc.volumeName,
						VolumeMode: &mode,
					},
				}
				if tc.pvcStatusSize.CmpInt64(0) > 0 {
					pvc.Status = v1.PersistentVolumeClaimStatus{
						Capacity: v1.ResourceList{
							v1.ResourceStorage: tc.pvcStatusSize,
						},
					}
				}
				pod := &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name: "pod1",
						UID:  "pod1uid",
					},
					Spec: v1.PodSpec{
						Volumes: []v1.Volume{
							{
								Name: "volume-name",
								VolumeSource: v1.VolumeSource{
									PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{
										ClaimName: pvc.Name,
									},
								},
							},
						},
					},
				}

				node := &v1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: string(nodeName),
					},
					Status: v1.NodeStatus{
						VolumesAttached: []v1.AttachedVolume{
							{
								Name:       v1.UniqueVolumeName(fmt.Sprintf("fake-plugin/%s", tc.volumeName)),
								DevicePath: "fake/path",
							},
						},
					},
				}

				volumePluginMgr, fakePlugin := volumetesting.GetTestKubeletVolumePluginMgrWithNode(t, node)
				fakePlugin.SupportsRemount = tc.supportRemount
				seLinuxTranslator := util.NewFakeSELinuxLabelTranslator()
				dsw := cache.NewDesiredStateOfWorld(volumePluginMgr, seLinuxTranslator)
				asw := cache.NewActualStateOfWorld(nodeName, volumePluginMgr)
				kubeClient := createtestClientWithPVPVC(pv, pvc, v1.AttachedVolume{
					Name:       v1.UniqueVolumeName(fmt.Sprintf("fake-plugin/%s", tc.volumeName)),
					DevicePath: "fake/path",
				})
				fakeRecorder := &record.FakeRecorder{}
				fakeHandler := volumetesting.NewBlockVolumePathHandler()
				oex := operationexecutor.NewOperationExecutor(operationexecutor.NewOperationGenerator(
					kubeClient,
					volumePluginMgr,
					fakeRecorder,
					fakeHandler))

				reconciler := NewReconciler(
					kubeClient,
					true, /* controllerAttachDetachEnabled */
					reconcilerLoopSleepDuration,
					waitForAttachTimeout,
					nodeName,
					dsw,
					asw,
					hasAddedPods,
					oex,
					&mount.FakeMounter{},
					hostutil.NewFakeHostUtil(nil),
					volumePluginMgr,
					uniquePodDir)
				volumeSpec := &volume.Spec{PersistentVolume: pv}
				podName := util.GetUniquePodName(pod)
				volumeName, err := dsw.AddPodToVolume(
					podName, pod, volumeSpec, volumeSpec.Name(), "" /* volumeGIDValue */, nil /* seLinuxLabel */)
				// Assert
				if err != nil {
					t.Fatalf("AddPodToVolume failed. Expected: <no error> Actual: <%v>", err)
				}
				dsw.MarkVolumesReportedInUse([]v1.UniqueVolumeName{volumeName})

				// Start the reconciler to fill ASW.
				stopChan, stoppedChan := make(chan struct{}), make(chan struct{})
				go func() {
					reconciler.Run(stopChan)
					close(stoppedChan)
				}()
				waitForVolumeToExistInASW(t, volumeName, asw)
				// all of these tests rely on device to be globally mounted and hence waiting for global
				// mount ensures that unmountDevice is called as expected.
				waitForGlobalMount(t, volumeName, asw)
				if tc.volumeName == volumetesting.TimeoutAndFailOnSetupVolumeName {
					// Wait upto 10s for reconciler to catchup
					time.Sleep(reconcilerSyncWaitDuration)
				}

				if tc.volumeName == volumetesting.SuccessAndFailOnSetupVolumeName ||
					tc.volumeName == volumetesting.SuccessAndTimeoutSetupVolumeName {
					// wait for mount and then break it via remount
					waitForMount(t, fakePlugin, volumeName, asw)
					asw.MarkRemountRequired(podName)
					time.Sleep(reconcilerSyncWaitDuration)
				}

				if tc.volumeState == operationexecutor.VolumeMountUncertain {
					waitForUncertainPodMount(t, volumeName, podName, asw)
				}

				if tc.volumeState == operationexecutor.VolumeMounted {
					waitForMount(t, fakePlugin, volumeName, asw)
				}

				dsw.DeletePodFromVolume(podName, volumeName)
				waitForDetach(t, volumeName, asw)

				if mode == v1.PersistentVolumeFilesystem {
					if err := volumetesting.VerifyUnmountDeviceCallCount(tc.unmountDeviceCallCount, fakePlugin); err != nil {
						t.Errorf("Error verifying UnMountDeviceCallCount: %v", err)
					}
					if err := volumetesting.VerifyTearDownCallCount(tc.unmountVolumeCount, fakePlugin); err != nil {
						t.Errorf("Error verifying UnMountDeviceCallCount: %v", err)
					}
				} else {
					if tc.unmountVolumeCount == 0 {
						if err := volumetesting.VerifyZeroUnmapPodDeviceCallCount(fakePlugin); err != nil {
							t.Errorf("Error verifying UnMountDeviceCallCount: %v", err)
						}
					} else {
						if err := volumetesting.VerifyUnmapPodDeviceCallCount(tc.unmountVolumeCount, fakePlugin); err != nil {
							t.Errorf("Error verifying UnMountDeviceCallCount: %v", err)
						}
					}
					if tc.unmountDeviceCallCount == 0 {
						if err := volumetesting.VerifyZeroTearDownDeviceCallCount(fakePlugin); err != nil {
							t.Errorf("Error verifying UnMountDeviceCallCount: %v", err)
						}
					} else {
						if err := volumetesting.VerifyTearDownDeviceCallCount(tc.unmountDeviceCallCount, fakePlugin); err != nil {
							t.Errorf("Error verifying UnMountDeviceCallCount: %v", err)
						}
					}
				}
			})
		}
	}
}

func waitForUncertainGlobalMount(t *testing.T, volumeName v1.UniqueVolumeName, asw cache.ActualStateOfWorld) {
	// check if volume is globally mounted in uncertain state
	err := retryWithExponentialBackOff(
		testOperationBackOffDuration,
		func() (bool, error) {
			unmountedVolumes := asw.GetUnmountedVolumes()
			for _, v := range unmountedVolumes {
				if v.VolumeName == volumeName && v.DeviceMountState == operationexecutor.DeviceMountUncertain {
					return true, nil
				}
			}
			return false, nil
		},
	)

	if err != nil {
		t.Fatalf("expected volumes %s to be mounted in uncertain state globally", volumeName)
	}
}

func waitForGlobalMount(t *testing.T, volumeName v1.UniqueVolumeName, asw cache.ActualStateOfWorld) {
	// check if volume is globally mounted
	err := retryWithExponentialBackOff(
		testOperationBackOffDuration,
		func() (bool, error) {
			mountedVolumes := asw.GetGloballyMountedVolumes()
			for _, v := range mountedVolumes {
				if v.VolumeName == volumeName {
					return true, nil
				}
			}
			return false, nil
		},
	)

	if err != nil {
		t.Fatalf("expected volume devices %s to be mounted globally", volumeName)
	}
}

func waitForUncertainPodMount(t *testing.T, volumeName v1.UniqueVolumeName, podName types.UniquePodName, asw cache.ActualStateOfWorld) {
	// check if volume is locally pod mounted in uncertain state
	err := retryWithExponentialBackOff(
		testOperationBackOffDuration,
		func() (bool, error) {
			mounted, _, err := asw.PodExistsInVolume(podName, volumeName, resource.Quantity{}, "" /* SELinuxContext */)
			if mounted || err != nil {
				return false, nil
			}
			allMountedVolumes := asw.GetAllMountedVolumes()
			for _, v := range allMountedVolumes {
				if v.VolumeName == volumeName {
					return true, nil
				}
			}
			return false, nil
		},
	)

	if err != nil {
		t.Fatalf("expected volumes %s to be mounted in uncertain state for pod", volumeName)
	}
}

func waitForMount(
	t *testing.T,
	fakePlugin *volumetesting.FakeVolumePlugin,
	volumeName v1.UniqueVolumeName,
	asw cache.ActualStateOfWorld) {
	err := retryWithExponentialBackOff(
		testOperationBackOffDuration,
		func() (bool, error) {
			mountedVolumes := asw.GetMountedVolumes()
			for _, mountedVolume := range mountedVolumes {
				if mountedVolume.VolumeName == volumeName {
					return true, nil
				}
			}

			return false, nil
		},
	)

	if err != nil {
		t.Fatalf("Timed out waiting for volume %q to be attached.", volumeName)
	}
}

func waitForVolumeToExistInASW(t *testing.T, volumeName v1.UniqueVolumeName, asw cache.ActualStateOfWorld) {
	err := retryWithExponentialBackOff(
		testOperationBackOffDuration,
		func() (bool, error) {
			if asw.VolumeExists(volumeName) {
				return true, nil
			}
			return false, nil
		},
	)
	if err != nil {
		t.Fatalf("Timed out waiting for volume %q to be exist in asw.", volumeName)
	}
}

func waitForDetach(
	t *testing.T,
	volumeName v1.UniqueVolumeName,
	asw cache.ActualStateOfWorld) {
	err := retryWithExponentialBackOff(
		testOperationBackOffDuration,
		func() (bool, error) {
			if asw.VolumeExists(volumeName) {
				return false, nil
			}

			return true, nil
		},
	)

	if err != nil {
		t.Fatalf("Timed out waiting for volume %q to be detached.", volumeName)
	}
}

func retryWithExponentialBackOff(initialDuration time.Duration, fn wait.ConditionFunc) error {
	backoff := wait.Backoff{
		Duration: initialDuration,
		Factor:   3,
		Jitter:   0,
		Steps:    6,
	}
	return wait.ExponentialBackoff(backoff, fn)
}

func createTestClient(attachedVolumes ...v1.AttachedVolume) *fake.Clientset {
	fakeClient := &fake.Clientset{}
	if len(attachedVolumes) == 0 {
		attachedVolumes = append(attachedVolumes, v1.AttachedVolume{
			Name:       "fake-plugin/fake-device1",
			DevicePath: "fake/path",
		})
	}
	fakeClient.AddReactor("get", "nodes",
		func(action core.Action) (bool, runtime.Object, error) {
			return true, &v1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: string(nodeName)},
				Status: v1.NodeStatus{
					VolumesAttached: attachedVolumes,
				},
			}, nil
		},
	)

	fakeClient.AddReactor("*", "*", func(action core.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("no reaction implemented for %s", action)
	})
	return fakeClient
}

func runReconciler(reconciler Reconciler) {
	go reconciler.Run(wait.NeverStop)
}

func createtestClientWithPVPVC(pv *v1.PersistentVolume, pvc *v1.PersistentVolumeClaim, attachedVolumes ...v1.AttachedVolume) *fake.Clientset {
	fakeClient := &fake.Clientset{}
	if len(attachedVolumes) == 0 {
		attachedVolumes = append(attachedVolumes, v1.AttachedVolume{
			Name:       "fake-plugin/pv",
			DevicePath: "fake/path",
		})
	}
	fakeClient.AddReactor("get", "nodes",
		func(action core.Action) (bool, runtime.Object, error) {
			return true, &v1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: string(nodeName)},
				Status: v1.NodeStatus{
					VolumesAttached: attachedVolumes,
				},
			}, nil
		})
	fakeClient.AddReactor("get", "persistentvolumeclaims", func(action core.Action) (bool, runtime.Object, error) {
		return true, pvc, nil
	})
	fakeClient.AddReactor("get", "persistentvolumes", func(action core.Action) (bool, runtime.Object, error) {
		return true, pv, nil
	})
	fakeClient.AddReactor("patch", "persistentvolumeclaims", func(action core.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() == "status" {
			return true, pvc, nil
		}
		return true, nil, fmt.Errorf("no reaction implemented for %s", action)
	})
	fakeClient.AddReactor("*", "*", func(action core.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("no reaction implemented for %s", action)
	})
	return fakeClient
}

func Test_Run_Positive_VolumeMountControllerAttachEnabledRace(t *testing.T) {
	// Arrange
	node := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: string(nodeName),
		},
		Status: v1.NodeStatus{
			VolumesAttached: []v1.AttachedVolume{
				{
					Name:       "fake-plugin/fake-device1",
					DevicePath: "/fake/path",
				},
			},
		},
	}
	volumePluginMgr, fakePlugin := volumetesting.GetTestKubeletVolumePluginMgrWithNode(t, node)
	seLinuxTranslator := util.NewFakeSELinuxLabelTranslator()

	dsw := cache.NewDesiredStateOfWorld(volumePluginMgr, seLinuxTranslator)
	asw := cache.NewActualStateOfWorld(nodeName, volumePluginMgr)
	kubeClient := createTestClient()
	fakeRecorder := &record.FakeRecorder{}
	fakeHandler := volumetesting.NewBlockVolumePathHandler()
	oex := operationexecutor.NewOperationExecutor(operationexecutor.NewOperationGenerator(
		kubeClient,
		volumePluginMgr,
		fakeRecorder,
		fakeHandler))
	reconciler := NewReconciler(
		kubeClient,
		true, /* controllerAttachDetachEnabled */
		reconcilerLoopSleepDuration,
		waitForAttachTimeout,
		nodeName,
		dsw,
		asw,
		hasAddedPods,
		oex,
		mount.NewFakeMounter(nil),
		hostutil.NewFakeHostUtil(nil),
		volumePluginMgr,
		kubeletPodsDir)
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pod1",
			UID:  "pod1uid",
		},
		Spec: v1.PodSpec{
			Volumes: []v1.Volume{
				{
					Name: "volume-name",
					VolumeSource: v1.VolumeSource{
						GCEPersistentDisk: &v1.GCEPersistentDiskVolumeSource{
							PDName: "fake-device1",
						},
					},
				},
			},
		},
	}

	// Some steps are executes out of order in callbacks, follow the numbers.

	// 1. Add a volume to DSW and wait until it's mounted
	volumeSpec := &volume.Spec{Volume: &pod.Spec.Volumes[0]}
	// copy before reconciler runs to avoid data race.
	volumeSpecCopy := &volume.Spec{Volume: &pod.Spec.Volumes[0]}
	podName := util.GetUniquePodName(pod)
	generatedVolumeName, err := dsw.AddPodToVolume(
		podName, pod, volumeSpec, volumeSpec.Name(), "" /* volumeGIDValue */, nil /* seLinuxLabel */)
	dsw.MarkVolumesReportedInUse([]v1.UniqueVolumeName{generatedVolumeName})

	if err != nil {
		t.Fatalf("AddPodToVolume failed. Expected: <no error> Actual: <%v>", err)
	}
	// Start the reconciler to fill ASW.
	stopChan, stoppedChan := make(chan struct{}), make(chan struct{})
	go func() {
		reconciler.Run(stopChan)
		close(stoppedChan)
	}()
	waitForMount(t, fakePlugin, generatedVolumeName, asw)
	// Stop the reconciler.
	close(stopChan)
	<-stoppedChan

	finished := make(chan interface{})
	finishedOnce := &sync.Once{}

	fakePlugin.Lock()
	fakePlugin.UnmountDeviceHook = func(mountPath string) error {
		// Act:
		// 3. While a volume is being unmounted, add it back to the desired state of world
		klog.InfoS("UnmountDevice called")
		var generatedVolumeNameCopy v1.UniqueVolumeName
		generatedVolumeNameCopy, err = dsw.AddPodToVolume(
			podName, pod, volumeSpecCopy, volumeSpec.Name(), "" /* volumeGIDValue */, nil /* seLinuxLabel */)
		dsw.MarkVolumesReportedInUse([]v1.UniqueVolumeName{generatedVolumeNameCopy})
		return nil
	}

	fakePlugin.WaitForAttachHook = func(spec *volume.Spec, devicePath string, pod *v1.Pod, spectimeout time.Duration) (string, error) {
		defer finishedOnce.Do(func() {
			close(finished)
		})
		// Assert
		// 4. When the volume is mounted again, expect that UnmountDevice operation did not clear devicePath
		if devicePath == "" {
			klog.ErrorS(nil, "Expected WaitForAttach called with devicePath from Node.Status")
			return "", fmt.Errorf("Expected devicePath from Node.Status")
		}
		return devicePath, nil
	}
	fakePlugin.Unlock()

	// Start the reconciler again.
	go reconciler.Run(wait.NeverStop)

	// 2. Delete the volume from DSW (and wait for callbacks)
	dsw.DeletePodFromVolume(podName, generatedVolumeName)

	<-finished
	waitForMount(t, fakePlugin, generatedVolumeName, asw)
}

func getFakeNode() *v1.Node {
	return &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: string(nodeName),
		},
		Status: v1.NodeStatus{
			VolumesAttached: []v1.AttachedVolume{
				{
					Name:       "fake-plugin/fake-device1",
					DevicePath: "/fake/path",
				},
			},
		},
	}
}

func getInlineFakePod(podName, podUUID, outerName, innerName string) *v1.Pod {
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: podName,
			UID:  k8stypes.UID(podUUID),
		},
		Spec: v1.PodSpec{
			Volumes: []v1.Volume{
				{
					Name: outerName,
					VolumeSource: v1.VolumeSource{
						GCEPersistentDisk: &v1.GCEPersistentDiskVolumeSource{
							PDName: innerName,
						},
					},
				},
			},
		},
	}
	return pod
}

func getReconciler(kubeletDir string, t *testing.T, volumePaths []string, kubeClient *fake.Clientset) (Reconciler, *volumetesting.FakeVolumePlugin) {
	node := getFakeNode()
	volumePluginMgr, fakePlugin := volumetesting.GetTestKubeletVolumePluginMgrWithNodeAndRoot(t, node, kubeletDir)
	tmpKubeletPodDir := filepath.Join(kubeletDir, "pods")
	seLinuxTranslator := util.NewFakeSELinuxLabelTranslator()

	dsw := cache.NewDesiredStateOfWorld(volumePluginMgr, seLinuxTranslator)
	asw := cache.NewActualStateOfWorld(nodeName, volumePluginMgr)
	if kubeClient == nil {
		kubeClient = createTestClient()
	}

	fakeRecorder := &record.FakeRecorder{}
	fakeHandler := volumetesting.NewBlockVolumePathHandler()
	oex := operationexecutor.NewOperationExecutor(operationexecutor.NewOperationGenerator(
		kubeClient,
		volumePluginMgr,
		fakeRecorder,
		fakeHandler))
	mountPoints := []mount.MountPoint{}
	for _, volumePath := range volumePaths {
		mountPoints = append(mountPoints, mount.MountPoint{Path: volumePath})
	}
	rc := NewReconciler(
		kubeClient,
		true, /* controllerAttachDetachEnabled */
		reconcilerLoopSleepDuration,
		waitForAttachTimeout,
		nodeName,
		dsw,
		asw,
		hasAddedPods,
		oex,
		mount.NewFakeMounter(mountPoints),
		hostutil.NewFakeHostUtil(nil),
		volumePluginMgr,
		tmpKubeletPodDir)
	return rc, fakePlugin
}

func TestReconcileWithUpdateReconstructedFromAPIServer(t *testing.T) {
	// Calls Run() with two reconstructed volumes.
	// Verifies the devicePaths + volume attachability are reconstructed from node.status.

	// Arrange
	node := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: string(nodeName),
		},
		Status: v1.NodeStatus{
			VolumesAttached: []v1.AttachedVolume{
				{
					Name:       "fake-plugin/fake-device1",
					DevicePath: "fake/path",
				},
			},
		},
	}
	volumePluginMgr, fakePlugin := volumetesting.GetTestKubeletVolumePluginMgrWithNode(t, node)
	seLinuxTranslator := util.NewFakeSELinuxLabelTranslator()
	dsw := cache.NewDesiredStateOfWorld(volumePluginMgr, seLinuxTranslator)
	asw := cache.NewActualStateOfWorld(nodeName, volumePluginMgr)
	kubeClient := createTestClient()
	fakeRecorder := &record.FakeRecorder{}
	fakeHandler := volumetesting.NewBlockVolumePathHandler()
	oex := operationexecutor.NewOperationExecutor(operationexecutor.NewOperationGenerator(
		kubeClient,
		volumePluginMgr,
		fakeRecorder,
		fakeHandler))
	rc := NewReconciler(
		kubeClient,
		true, /* controllerAttachDetachEnabled */
		reconcilerLoopSleepDuration,
		waitForAttachTimeout,
		nodeName,
		dsw,
		asw,
		hasAddedPods,
		oex,
		mount.NewFakeMounter(nil),
		hostutil.NewFakeHostUtil(nil),
		volumePluginMgr,
		kubeletPodsDir)
	reconciler := rc.(*reconciler)

	// The pod has two volumes, fake-device1 is attachable, fake-device2 is not.
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pod1",
			UID:  "pod1uid",
		},
		Spec: v1.PodSpec{
			Volumes: []v1.Volume{
				{
					Name: "volume-name",
					VolumeSource: v1.VolumeSource{
						GCEPersistentDisk: &v1.GCEPersistentDiskVolumeSource{
							PDName: "fake-device1",
						},
					},
				},
				{
					Name: "volume-name2",
					VolumeSource: v1.VolumeSource{
						GCEPersistentDisk: &v1.GCEPersistentDiskVolumeSource{
							PDName: "fake-device2",
						},
					},
				},
			},
		},
	}

	volumeSpec1 := &volume.Spec{Volume: &pod.Spec.Volumes[0]}
	volumeName1 := util.GetUniqueVolumeName(fakePlugin.GetPluginName(), "fake-device1")
	volumeSpec2 := &volume.Spec{Volume: &pod.Spec.Volumes[1]}
	volumeName2 := util.GetUniqueVolumeName(fakePlugin.GetPluginName(), "fake-device2")

	assert.NoError(t, asw.AddAttachUncertainReconstructedVolume(volumeName1, volumeSpec1, nodeName, ""))
	assert.NoError(t, asw.MarkDeviceAsUncertain(volumeName1, "/dev/badly/reconstructed", "/var/lib/kubelet/plugins/global1", ""))
	assert.NoError(t, asw.AddAttachUncertainReconstructedVolume(volumeName2, volumeSpec2, nodeName, ""))
	assert.NoError(t, asw.MarkDeviceAsUncertain(volumeName2, "/dev/reconstructed", "/var/lib/kubelet/plugins/global2", ""))

	assert.False(t, reconciler.StatesHasBeenSynced())

	reconciler.volumesNeedUpdateFromNodeStatus = append(reconciler.volumesNeedUpdateFromNodeStatus, volumeName1, volumeName2)
	// Act - run reconcile loop just once.
	// "volumesNeedUpdateFromNodeStatus" is not empty, so no unmount will be triggered.
	reconciler.reconcile()

	// Assert
	assert.True(t, reconciler.StatesHasBeenSynced())
	assert.Empty(t, reconciler.volumesNeedUpdateFromNodeStatus)

	attachedVolumes := asw.GetAttachedVolumes()
	assert.Lenf(t, attachedVolumes, 2, "two volumes in ASW expected")
	for _, vol := range attachedVolumes {
		if vol.VolumeName == volumeName1 {
			// devicePath + attachability must have been updated from node.status
			assert.True(t, vol.PluginIsAttachable)
			assert.Equal(t, "fake/path", vol.DevicePath)
		}
		if vol.VolumeName == volumeName2 {
			// only attachability was updated from node.status
			assert.False(t, vol.PluginIsAttachable)
			assert.Equal(t, "/dev/reconstructed", vol.DevicePath)
		}
	}
}
