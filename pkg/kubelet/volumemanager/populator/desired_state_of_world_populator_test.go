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

package populator

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubetypes "k8s.io/apimachinery/pkg/types"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/kubernetes/fake"
	core "k8s.io/client-go/testing"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	csitrans "k8s.io/csi-translation-lib"
	"k8s.io/kubernetes/pkg/features"
	containertest "k8s.io/kubernetes/pkg/kubelet/container/testing"
	kubepod "k8s.io/kubernetes/pkg/kubelet/pod"
	"k8s.io/kubernetes/pkg/kubelet/volumemanager/cache"
	"k8s.io/kubernetes/pkg/volume"
	"k8s.io/kubernetes/pkg/volume/csimigration"
	volumetesting "k8s.io/kubernetes/pkg/volume/testing"
	"k8s.io/kubernetes/pkg/volume/util"
	"k8s.io/kubernetes/pkg/volume/util/operationexecutor"
	"k8s.io/kubernetes/pkg/volume/util/types"
	"k8s.io/kubernetes/test/utils/ktesting"
	"k8s.io/utils/ptr"
)

const (
	Removed     string = "removed"
	Terminating string = "terminating"
	Other       string = "other"
)

func pluginPVOmittingClient(dswp *desiredStateOfWorldPopulator) {
	fakeClient := &fake.Clientset{}
	fakeClient.AddReactor("get", "persistentvolumeclaims", func(action core.Action) (bool, runtime.Object, error) {
		return false, nil, nil
	})
	fakeClient.AddReactor("get", "persistentvolumes", func(action core.Action) (bool, runtime.Object, error) {
		return false, nil, nil
	})
	dswp.kubeClient = fakeClient
}

func prepareDswpWithVolume(t *testing.T) (*desiredStateOfWorldPopulator, kubepod.Manager, *fakePodStateProvider) {
	// create dswp
	mode := v1.PersistentVolumeFilesystem
	pv := &v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: "dswp-test-volume-name",
		},
		Spec: v1.PersistentVolumeSpec{
			ClaimRef:   &v1.ObjectReference{Namespace: "ns", Name: "file-bound"},
			VolumeMode: &mode,
		},
	}
	pvc := &v1.PersistentVolumeClaim{
		Spec: v1.PersistentVolumeClaimSpec{
			VolumeName: "dswp-test-volume-name",
		},
		Status: v1.PersistentVolumeClaimStatus{
			Phase: v1.ClaimBound,
		},
	}
	dswp, fakePodManager, _, _, fakePodStateProvider := createDswpWithVolume(t, pv, pvc)
	return dswp, fakePodManager, fakePodStateProvider
}

func TestFindAndAddNewPods_WithDifferentConditions(t *testing.T) {
	tests := []struct {
		desc          string
		hasAddedPods  bool
		podState      string
		expectedFound bool // Found pod is added to DSW
	}{
		{
			desc:          "HasAddedPods is false, ShouldPodRuntimeBeRemoved and ShouldPodContainerBeTerminating are both true",
			hasAddedPods:  false,
			podState:      Removed,
			expectedFound: false, // Pod should not be added to DSW
		},
		{
			desc:          "HasAddedPods is false, ShouldPodRuntimeBeRemoved is false, ShouldPodContainerBeTerminating is true",
			hasAddedPods:  false,
			podState:      Terminating,
			expectedFound: true, // Pod should be added to DSW
		},
		{
			desc:          "HasAddedPods is false, other condition",
			hasAddedPods:  false,
			podState:      Other,
			expectedFound: true, // Pod should be added to DSW
		},
		{
			desc:          "HasAddedPods is true, ShouldPodRuntimeBeRemoved is false, ShouldPodContainerBeTerminating is true",
			hasAddedPods:  true,
			podState:      Terminating,
			expectedFound: false, // Pod should not be added to DSW
		},
		{
			desc:          "HasAddedPods is true, ShouldPodRuntimeBeRemoved and ShouldPodContainerBeTerminating are both true",
			hasAddedPods:  true,
			podState:      Removed,
			expectedFound: false, // Pod should not be added to DSW
		},
		{
			desc:          "HasAddedPods is true, other condition",
			hasAddedPods:  true,
			podState:      Other,
			expectedFound: true, // Pod should be added to DSW
		},
	}

	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			// create dswp
			dswp, fakePodManager, fakePodState := prepareDswpWithVolume(t)

			// create pod
			containers := []v1.Container{
				{
					VolumeMounts: []v1.VolumeMount{
						{
							Name:      "dswp-test-volume-name",
							MountPath: "/mnt",
						},
					},
				},
			}
			pod := createPodWithVolume("dswp-test-pod", "dswp-test-volume-name", "file-bound", containers)

			fakePodManager.AddPod(pod)

			switch tc.podState {
			case Removed:
				fakePodState.removed = map[kubetypes.UID]struct{}{pod.UID: {}}
			case Terminating:
				fakePodState.terminating = map[kubetypes.UID]struct{}{pod.UID: {}}
			case Other:
				break
			}

			dswp.hasAddedPods = tc.hasAddedPods
			// Action
			tCtx := ktesting.Init(t)
			dswp.findAndAddNewPods(tCtx)

			// Verify
			podsInDSW := dswp.desiredStateOfWorld.GetPods()
			found := false
			if podsInDSW[types.UniquePodName(pod.UID)] {
				found = true
			}

			if found != tc.expectedFound {
				t.Fatalf(
					"Pod with uid %v has expectedFound value %v in pods in DSW %v",
					pod.UID, tc.expectedFound, podsInDSW)
			}
		})
	}
}

func TestFindAndAddNewPods_WithReprocessPodAndVolumeRetrievalError(t *testing.T) {
	// create dswp
	dswp, fakePodManager, _ := prepareDswpWithVolume(t)

	// create pod
	containers := []v1.Container{
		{
			VolumeMounts: []v1.VolumeMount{
				{
					Name:      "dswp-test-volume-name",
					MountPath: "/mnt",
				},
			},
		},
	}
	pod := createPodWithVolume("dswp-test-pod", "dswp-test-volume-name", "file-bound", containers)

	fakePodManager.AddPod(pod)

	podName := util.GetUniquePodName(pod)

	tCtx := ktesting.Init(t)
	dswp.findAndAddNewPods(tCtx)

	if !dswp.podPreviouslyProcessed(podName) {
		t.Fatalf("Failed to record that the volumes for the specified pod: %s have been processed by the populator", podName)
	}
	pluginPVOmittingClient(dswp)

	dswp.ReprocessPod(podName)
	dswp.findAndAddNewPods(tCtx)

	if !dswp.podPreviouslyProcessed(podName) {
		t.Fatalf("Failed to record that the volumes for the specified pod: %s have been processed by the populator", podName)
	}
	fakePodManager.RemovePod(pod)
}

func TestFindAndAddNewPods_WithVolumeRetrievalError(t *testing.T) {
	// create dswp
	dswp, fakePodManager, _ := prepareDswpWithVolume(t)

	pluginPVOmittingClient(dswp)

	// create pod
	containers := []v1.Container{
		{
			VolumeMounts: []v1.VolumeMount{
				{
					Name:      "dswp-test-volume-name",
					MountPath: "/mnt",
				},
			},
		},
	}
	pod := createPodWithVolume("dswp-test-pod", "dswp-test-volume-name", "file-bound", containers)

	fakePodManager.AddPod(pod)

	podName := util.GetUniquePodName(pod)

	tCtx := ktesting.Init(t)
	dswp.findAndAddNewPods(tCtx)

	if dswp.podPreviouslyProcessed(podName) {
		t.Fatalf("The volumes for the specified pod: %s should not have been processed by the populator", podName)
	}
	if dswp.podHasBeenSeenOnce(podName) {
		t.Fatalf("The volumes for the specified pod: %s should not have been processed by the populator", podName)
	}
}

type mutablePodManager interface {
	GetPodByName(string, string) (*v1.Pod, bool)
	RemovePod(*v1.Pod)
}

func TestFindAndAddNewPods_FindAndRemoveDeletedPods(t *testing.T) {
	dswp, fakePodState, pod, expectedVolumeName, _ := prepareDSWPWithPodPV(t)
	podName := util.GetUniquePodName(pod)

	//let the pod be terminated
	podGet, exist := dswp.podManager.(mutablePodManager).GetPodByName(pod.Namespace, pod.Name)
	if !exist {
		t.Fatalf("Failed to get pod by pod name: %s and namespace: %s", pod.Name, pod.Namespace)
	}
	podGet.Status.Phase = v1.PodFailed
	dswp.podManager.(mutablePodManager).RemovePod(pod)

	dswp.findAndRemoveDeletedPods()

	if !dswp.pods.processedPods[podName] {
		t.Fatalf("Pod should not been removed from desired state of world since pod state still thinks it exists")
	}

	fakePodState.removed = map[kubetypes.UID]struct{}{pod.UID: {}}

	// the pod state is marked as removed, so here findAndRemoveDeletedPods() will remove the pod and volumes it is mounted
	dswp.findAndRemoveDeletedPods()
	if dswp.pods.processedPods[podName] {
		t.Fatalf("Failed to remove pods from desired state of world since they no longer exist")
	}

	// podWorker may call volume_manager WaitForUnmount() after we processed the pod in findAndRemoveDeletedPods()
	dswp.ReprocessPod(podName)
	dswp.findAndRemoveDeletedPods()

	// findAndRemoveDeletedPods() above must detect orphaned pod and delete it from the map
	if _, ok := dswp.pods.processedPods[podName]; ok {
		t.Fatalf("Failed to remove orphaned pods from internal map")
	}

	volumeExists := dswp.desiredStateOfWorld.VolumeExists(expectedVolumeName, "" /* SELinuxContext */)
	if volumeExists {
		t.Fatalf(
			"VolumeExists(%q) failed. Expected: <false> Actual: <%v>",
			expectedVolumeName,
			volumeExists)
	}

	if podExistsInVolume := dswp.desiredStateOfWorld.PodExistsInVolume(
		podName, expectedVolumeName, "" /* SELinuxContext */); podExistsInVolume {
		t.Fatalf(
			"DSW PodExistsInVolume returned incorrect value. Expected: <false> Actual: <%v>",
			podExistsInVolume)
	}

	volumesToMount := dswp.desiredStateOfWorld.GetVolumesToMount()
	for _, volume := range volumesToMount {
		if volume.VolumeName == expectedVolumeName {
			t.Fatalf(
				"Found volume %v in the list of desired state of world volumes to mount. Expected not",
				expectedVolumeName)
		}
	}
}

func TestFindAndRemoveDeletedPodsWithActualState(t *testing.T) {
	dswp, fakePodState, pod, expectedVolumeName, _ := prepareDSWPWithPodPV(t)
	fakeASW := dswp.actualStateOfWorld
	podName := util.GetUniquePodName(pod)

	//let the pod be terminated
	podGet, exist := dswp.podManager.(mutablePodManager).GetPodByName(pod.Namespace, pod.Name)
	if !exist {
		t.Fatalf("Failed to get pod by pod name: %s and namespace: %s", pod.Name, pod.Namespace)
	}
	podGet.Status.Phase = v1.PodFailed

	dswp.findAndRemoveDeletedPods()
	// Although Pod status is terminated, pod still exists in pod manager and actual state does not has this pod and volume information
	// desired state populator will fail to delete this pod and volume first
	volumeExists := dswp.desiredStateOfWorld.VolumeExists(expectedVolumeName, "" /* SELinuxContext */)
	if !volumeExists {
		t.Fatalf(
			"VolumeExists(%q) failed. Expected: <true> Actual: <%v>",
			expectedVolumeName,
			volumeExists)
	}

	if podExistsInVolume := dswp.desiredStateOfWorld.PodExistsInVolume(
		podName, expectedVolumeName, "" /* SELinuxContext */); !podExistsInVolume {
		t.Fatalf(
			"DSW PodExistsInVolume returned incorrect value. Expected: <true> Actual: <%v>",
			podExistsInVolume)
	}

	// reconcile with actual state so that volume is added into the actual state
	// desired state populator now can successfully delete the pod and volume
	reconcileASW(fakeASW, dswp.desiredStateOfWorld, t)
	dswp.findAndRemoveDeletedPods()
	if !dswp.desiredStateOfWorld.VolumeExists(expectedVolumeName, "" /* SELinuxContext */) {
		t.Fatalf(
			"VolumeExists(%q) failed. Expected: <false> Actual: <%v>",
			expectedVolumeName,
			volumeExists)
	}

	fakePodState.removed = map[kubetypes.UID]struct{}{pod.UID: {}}

	// reconcile with actual state so that volume is added into the actual state
	// desired state populator now can successfully delete the pod and volume
	reconcileASW(fakeASW, dswp.desiredStateOfWorld, t)
	dswp.findAndRemoveDeletedPods()
	volumeExists = dswp.desiredStateOfWorld.VolumeExists(expectedVolumeName, "" /* SELinuxContext */)
	if volumeExists {
		t.Fatalf(
			"VolumeExists(%q) failed. Expected: <false> Actual: <%v>",
			expectedVolumeName,
			volumeExists)
	}

	if podExistsInVolume := dswp.desiredStateOfWorld.PodExistsInVolume(
		podName, expectedVolumeName, "" /* SELinuxContext */); podExistsInVolume {
		t.Fatalf(
			"DSW PodExistsInVolume returned incorrect value. Expected: <false> Actual: <%v>",
			podExistsInVolume)
	}
}

func TestFindAndRemoveDeletedPodsWithUncertain(t *testing.T) {
	dswp, fakePodState, pod, expectedVolumeName, pv := prepareDSWPWithPodPV(t)
	podName := util.GetUniquePodName(pod)

	//let the pod be terminated
	podGet, exist := dswp.podManager.(mutablePodManager).GetPodByName(pod.Namespace, pod.Name)
	if !exist {
		t.Fatalf("Failed to get pod by pod name: %s and namespace: %s", pod.Name, pod.Namespace)
	}
	podGet.Status.Phase = v1.PodFailed
	dswp.podManager.(mutablePodManager).RemovePod(pod)
	fakePodState.removed = map[kubetypes.UID]struct{}{pod.UID: {}}

	// Add the volume to ASW by reconciling.
	fakeASW := dswp.actualStateOfWorld
	reconcileASW(fakeASW, dswp.desiredStateOfWorld, t)

	// Mark the volume as uncertain
	opts := operationexecutor.MarkVolumeOpts{
		PodName:             util.GetUniquePodName(pod),
		PodUID:              pod.UID,
		VolumeName:          expectedVolumeName,
		OuterVolumeSpecName: "dswp-test-volume-name",
		VolumeGIDVolume:     "",
		VolumeSpec:          volume.NewSpecFromPersistentVolume(pv, false),
		VolumeMountState:    operationexecutor.VolumeMountUncertain,
	}
	err := dswp.actualStateOfWorld.MarkVolumeMountAsUncertain(opts)
	if err != nil {
		t.Fatalf("Failed to set the volume as uncertain: %s", err)
	}

	// the pod state now lists the pod as removed, so here findAndRemoveDeletedPods() will remove the pod and volumes it is mounted
	dswp.findAndRemoveDeletedPods()
	if dswp.pods.processedPods[podName] {
		t.Fatalf("Failed to remove pods from desired state of world since they no longer exist")
	}

	volumeExists := dswp.desiredStateOfWorld.VolumeExists(expectedVolumeName, "" /* SELinuxContext */)
	if volumeExists {
		t.Fatalf(
			"VolumeExists(%q) failed. Expected: <false> Actual: <%v>",
			expectedVolumeName,
			volumeExists)
	}

	if podExistsInVolume := dswp.desiredStateOfWorld.PodExistsInVolume(
		podName, expectedVolumeName, "" /* SELinuxContext */); podExistsInVolume {
		t.Fatalf(
			"DSW PodExistsInVolume returned incorrect value. Expected: <false> Actual: <%v>",
			podExistsInVolume)
	}

	volumesToMount := dswp.desiredStateOfWorld.GetVolumesToMount()
	for _, volume := range volumesToMount {
		if volume.VolumeName == expectedVolumeName {
			t.Fatalf(
				"Found volume %v in the list of desired state of world volumes to mount. Expected not",
				expectedVolumeName)
		}
	}
}

func prepareDSWPWithPodPV(t *testing.T) (*desiredStateOfWorldPopulator, *fakePodStateProvider, *v1.Pod, v1.UniqueVolumeName, *v1.PersistentVolume) {
	// create dswp
	mode := v1.PersistentVolumeFilesystem
	pv := &v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: "dswp-test-volume-name",
		},
		Spec: v1.PersistentVolumeSpec{
			ClaimRef:   &v1.ObjectReference{Namespace: "ns", Name: "file-bound"},
			VolumeMode: &mode,
		},
	}
	pvc := &v1.PersistentVolumeClaim{
		Spec: v1.PersistentVolumeClaimSpec{
			VolumeName: "dswp-test-volume-name",
		},
		Status: v1.PersistentVolumeClaimStatus{
			Phase: v1.ClaimBound,
		},
	}
	dswp, fakePodManager, fakesDSW, _, fakePodState := createDswpWithVolume(t, pv, pvc)

	// create pod
	containers := []v1.Container{
		{
			VolumeMounts: []v1.VolumeMount{
				{
					Name:      "dswp-test-volume-name",
					MountPath: "/mnt",
				},
			},
		},
	}
	pod := createPodWithVolume("dswp-test-pod", "dswp-test-volume-name", "file-bound", containers)

	fakePodManager.AddPod(pod)

	podName := util.GetUniquePodName(pod)

	generatedVolumeName := "fake-plugin/" + pod.Spec.Volumes[0].Name

	tCtx := ktesting.Init(t)
	dswp.findAndAddNewPods(tCtx)

	if !dswp.pods.processedPods[podName] {
		t.Fatalf("Failed to record that the volumes for the specified pod: %s have been processed by the populator", podName)
	}

	expectedVolumeName := v1.UniqueVolumeName(generatedVolumeName)

	volumeExists := fakesDSW.VolumeExists(expectedVolumeName, "" /* SELinuxContext */)
	if !volumeExists {
		t.Fatalf(
			"VolumeExists(%q) failed. Expected: <true> Actual: <%v>",
			expectedVolumeName,
			volumeExists)
	}

	if podExistsInVolume := fakesDSW.PodExistsInVolume(
		podName, expectedVolumeName, "" /* SELinuxContext */); !podExistsInVolume {
		t.Fatalf(
			"DSW PodExistsInVolume returned incorrect value. Expected: <true> Actual: <%v>",
			podExistsInVolume)
	}

	verifyVolumeExistsInVolumesToMount(
		t, v1.UniqueVolumeName(generatedVolumeName), false /* expectReportedInUse */, fakesDSW)
	return dswp, fakePodState, pod, expectedVolumeName, pv
}

func TestFindAndRemoveNonattachableVolumes(t *testing.T) {
	// create dswp
	mode := v1.PersistentVolumeFilesystem
	pv := &v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: "dswp-test-volume-name",
		},
		Spec: v1.PersistentVolumeSpec{
			ClaimRef:   &v1.ObjectReference{Namespace: "ns", Name: "file-bound"},
			VolumeMode: &mode,
		},
	}
	pvc := &v1.PersistentVolumeClaim{
		Spec: v1.PersistentVolumeClaimSpec{
			VolumeName: "dswp-test-volume-name",
		},
		Status: v1.PersistentVolumeClaimStatus{
			Phase: v1.ClaimBound,
		},
	}

	fakeVolumePluginMgr, fakeVolumePlugin := volumetesting.GetTestKubeletVolumePluginMgr(t)
	dswp, fakePodManager, fakesDSW, _, _ := createDswpWithVolumeWithCustomPluginMgr(pv, pvc, fakeVolumePluginMgr)

	// create pod
	containers := []v1.Container{
		{
			VolumeMounts: []v1.VolumeMount{
				{
					Name:      "dswp-test-volume-name",
					MountPath: "/mnt",
				},
			},
		},
	}
	pod := createPodWithVolume("dswp-test-pod", "dswp-test-volume-name", "file-bound", containers)

	fakePodManager.AddPod(pod)

	podName := util.GetUniquePodName(pod)

	generatedVolumeName := "fake-plugin/" + pod.Spec.Volumes[0].Name

	tCtx := ktesting.Init(t)
	dswp.findAndAddNewPods(tCtx)

	if !dswp.pods.processedPods[podName] {
		t.Fatalf("Failed to record that the volumes for the specified pod: %s have been processed by the populator", podName)
	}

	expectedVolumeName := v1.UniqueVolumeName(generatedVolumeName)

	volumeExists := fakesDSW.VolumeExists(expectedVolumeName, "" /* SELinuxContext */)
	if !volumeExists {
		t.Fatalf(
			"VolumeExists(%q) failed. Expected: <true> Actual: <%v>",
			expectedVolumeName,
			volumeExists)
	}

	// change the volume plugin from attachable to non-attachable
	fakeVolumePlugin.NonAttachable = true

	// The volume should still exist
	verifyVolumeExistsInVolumesToMount(
		t, v1.UniqueVolumeName(generatedVolumeName), false /* expectReportedInUse */, fakesDSW)

	dswp.findAndRemoveDeletedPods()
	// After the volume plugin changes to nonattachable, the corresponding volume attachable field should change.
	volumesToMount := fakesDSW.GetVolumesToMount()
	for _, volume := range volumesToMount {
		if volume.VolumeName == expectedVolumeName {
			if volume.PluginIsAttachable {
				t.Fatalf(
					"Volume %v in the list of desired state of world volumes to mount is still attachable. Expected not",
					expectedVolumeName)
			}
		}
	}
}

func TestEphemeralVolumeOwnerCheck(t *testing.T) {
	// create dswp
	mode := v1.PersistentVolumeFilesystem
	pod, pv, pvc := createEphemeralVolumeObjects("dswp-test-pod", "dswp-test-volume-name", false /* not owned */, &mode)
	dswp, fakePodManager, _, _, _ := createDswpWithVolume(t, pv, pvc)
	fakePodManager.AddPod(pod)

	podName := util.GetUniquePodName(pod)

	tCtx := ktesting.Init(t)
	dswp.findAndAddNewPods(tCtx)
	if dswp.pods.processedPods[podName] {
		t.Fatalf("%s should not have been processed by the populator", podName)
	}
	require.Equal(t,
		[]string{fmt.Sprintf("PVC %s/%s was not created for pod %s/%s (pod is not owner)",
			pvc.Namespace, pvc.Name,
			pod.Namespace, pod.Name,
		)},
		dswp.desiredStateOfWorld.PopPodErrors(podName),
	)
}

func TestFindAndAddNewPods_FindAndRemoveDeletedPods_Valid_Block_VolumeDevices(t *testing.T) {
	// create dswp
	mode := v1.PersistentVolumeBlock
	pv := &v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: "dswp-test-volume-name",
		},
		Spec: v1.PersistentVolumeSpec{
			ClaimRef:   &v1.ObjectReference{Namespace: "ns", Name: "block-bound"},
			VolumeMode: &mode,
		},
	}
	pvc := &v1.PersistentVolumeClaim{
		Spec: v1.PersistentVolumeClaimSpec{
			VolumeName: "dswp-test-volume-name",
		},
		Status: v1.PersistentVolumeClaimStatus{
			Phase: v1.ClaimBound,
		},
	}
	dswp, fakePodManager, fakesDSW, _, fakePodState := createDswpWithVolume(t, pv, pvc)

	// create pod
	containers := []v1.Container{
		{
			VolumeDevices: []v1.VolumeDevice{
				{
					Name:       "dswp-test-volume-name",
					DevicePath: "/dev/sdb",
				},
			},
		},
	}
	pod := createPodWithVolume("dswp-test-pod", "dswp-test-volume-name", "block-bound", containers)

	fakePodManager.AddPod(pod)

	podName := util.GetUniquePodName(pod)

	generatedVolumeName := "fake-plugin/" + pod.Spec.Volumes[0].Name

	tCtx := ktesting.Init(t)
	dswp.findAndAddNewPods(tCtx)

	if !dswp.pods.processedPods[podName] {
		t.Fatalf("Failed to record that the volumes for the specified pod: %s have been processed by the populator", podName)
	}

	expectedVolumeName := v1.UniqueVolumeName(generatedVolumeName)

	volumeExists := fakesDSW.VolumeExists(expectedVolumeName, "" /* SELinuxContext */)
	if !volumeExists {
		t.Fatalf(
			"VolumeExists(%q) failed. Expected: <true> Actual: <%v>",
			expectedVolumeName,
			volumeExists)
	}

	if podExistsInVolume := fakesDSW.PodExistsInVolume(
		podName, expectedVolumeName, "" /* SELinuxContext */); !podExistsInVolume {
		t.Fatalf(
			"DSW PodExistsInVolume returned incorrect value. Expected: <true> Actual: <%v>",
			podExistsInVolume)
	}

	verifyVolumeExistsInVolumesToMount(
		t, v1.UniqueVolumeName(generatedVolumeName), false /* expectReportedInUse */, fakesDSW)

	//let the pod be terminated
	podGet, exist := fakePodManager.GetPodByName(pod.Namespace, pod.Name)
	if !exist {
		t.Fatalf("Failed to get pod by pod name: %s and namespace: %s", pod.Name, pod.Namespace)
	}
	podGet.Status.Phase = v1.PodFailed
	fakePodManager.RemovePod(pod)
	fakePodState.removed = map[kubetypes.UID]struct{}{pod.UID: {}}

	//pod is added to fakePodManager but pod state knows the pod is removed, so here findAndRemoveDeletedPods() will remove the pod and volumes it is mounted
	dswp.findAndRemoveDeletedPods()
	if dswp.pods.processedPods[podName] {
		t.Fatalf("Failed to remove pods from desired state of world since they no longer exist")
	}

	volumeExists = fakesDSW.VolumeExists(expectedVolumeName, "" /* SELinuxContext */)
	if volumeExists {
		t.Fatalf(
			"VolumeExists(%q) failed. Expected: <false> Actual: <%v>",
			expectedVolumeName,
			volumeExists)
	}

	if podExistsInVolume := fakesDSW.PodExistsInVolume(
		podName, expectedVolumeName, "" /* SELinuxContext */); podExistsInVolume {
		t.Fatalf(
			"DSW PodExistsInVolume returned incorrect value. Expected: <false> Actual: <%v>",
			podExistsInVolume)
	}

	volumesToMount := fakesDSW.GetVolumesToMount()
	for _, volume := range volumesToMount {
		if volume.VolumeName == expectedVolumeName {
			t.Fatalf(
				"Found volume %v in the list of desired state of world volumes to mount. Expected not",
				expectedVolumeName)
		}
	}
}

func TestCreateVolumeSpec_Valid_File_VolumeMounts(t *testing.T) {
	// create dswp
	mode := v1.PersistentVolumeFilesystem
	pv := &v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: "dswp-test-volume-name",
		},
		Spec: v1.PersistentVolumeSpec{
			ClaimRef:   &v1.ObjectReference{Namespace: "ns", Name: "file-bound"},
			VolumeMode: &mode,
		},
	}
	pvc := &v1.PersistentVolumeClaim{
		Spec: v1.PersistentVolumeClaimSpec{
			VolumeName: "dswp-test-volume-name",
			VolumeMode: &mode,
		},
		Status: v1.PersistentVolumeClaimStatus{
			Phase: v1.ClaimBound,
		},
	}
	dswp, fakePodManager, _, _, _ := createDswpWithVolume(t, pv, pvc)

	// create pod
	containers := []v1.Container{
		{
			VolumeMounts: []v1.VolumeMount{
				{
					Name:      "dswp-test-volume-name",
					MountPath: "/mnt",
				},
			},
		},
	}
	pod := createPodWithVolume("dswp-test-pod", "dswp-test-volume-name", "file-bound", containers)

	logger, _ := ktesting.NewTestContext(t)
	fakePodManager.AddPod(pod)
	mountsMap, devicesMap, _ := util.GetPodVolumeNames(pod, false /* collectSELinuxOptions */)
	_, volumeSpec, _, err :=
		dswp.createVolumeSpec(logger, pod.Spec.Volumes[0], pod, mountsMap, devicesMap)

	// Assert
	if volumeSpec == nil || err != nil {
		t.Fatalf("Failed to create volumeSpec with combination of filesystem mode and volumeMounts. err: %v", err)
	}
}

func TestCreateVolumeSpec_Valid_Nil_VolumeMounts(t *testing.T) {
	// create dswp
	pv := &v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: "dswp-test-volume-name",
		},
		Spec: v1.PersistentVolumeSpec{
			ClaimRef:   &v1.ObjectReference{Namespace: "ns", Name: "file-bound"},
			VolumeMode: ptr.To(v1.PersistentVolumeFilesystem),
		},
	}
	pvc := &v1.PersistentVolumeClaim{
		Spec: v1.PersistentVolumeClaimSpec{
			VolumeName: "dswp-test-volume-name",
			VolumeMode: ptr.To(v1.PersistentVolumeFilesystem),
		},
		Status: v1.PersistentVolumeClaimStatus{
			Phase: v1.ClaimBound,
		},
	}
	dswp, fakePodManager, _, _, _ := createDswpWithVolume(t, pv, pvc)

	// create pod
	containers := []v1.Container{
		{
			VolumeMounts: []v1.VolumeMount{
				{
					Name:      "dswp-test-volume-name",
					MountPath: "/mnt",
				},
			},
		},
	}
	pod := createPodWithVolume("dswp-test-pod", "dswp-test-volume-name", "file-bound", containers)

	logger, _ := ktesting.NewTestContext(t)
	fakePodManager.AddPod(pod)
	mountsMap, devicesMap, _ := util.GetPodVolumeNames(pod, false /* collectSELinuxOptions */)
	_, volumeSpec, _, err :=
		dswp.createVolumeSpec(logger, pod.Spec.Volumes[0], pod, mountsMap, devicesMap)

	// Assert
	if volumeSpec == nil || err != nil {
		t.Fatalf("Failed to create volumeSpec with combination of filesystem mode and volumeMounts. err: %v", err)
	}
}

func TestCreateVolumeSpec_Valid_Block_VolumeDevices(t *testing.T) {
	// create dswp
	mode := v1.PersistentVolumeBlock
	pv := &v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: "dswp-test-volume-name",
		},
		Spec: v1.PersistentVolumeSpec{
			ClaimRef:   &v1.ObjectReference{Namespace: "ns", Name: "block-bound"},
			VolumeMode: &mode,
		},
	}
	pvc := &v1.PersistentVolumeClaim{
		Spec: v1.PersistentVolumeClaimSpec{
			VolumeName: "dswp-test-volume-name",
		},
		Status: v1.PersistentVolumeClaimStatus{
			Phase: v1.ClaimBound,
		},
	}
	dswp, fakePodManager, _, _, _ := createDswpWithVolume(t, pv, pvc)

	// create pod
	containers := []v1.Container{
		{
			VolumeDevices: []v1.VolumeDevice{
				{
					Name:       "dswp-test-volume-name",
					DevicePath: "/dev/sdb",
				},
			},
		},
	}
	pod := createPodWithVolume("dswp-test-pod", "dswp-test-volume-name", "block-bound", containers)

	logger, _ := ktesting.NewTestContext(t)
	fakePodManager.AddPod(pod)
	mountsMap, devicesMap, _ := util.GetPodVolumeNames(pod, false /* collectSELinuxOptions */)
	_, volumeSpec, _, err :=
		dswp.createVolumeSpec(logger, pod.Spec.Volumes[0], pod, mountsMap, devicesMap)

	// Assert
	if volumeSpec == nil || err != nil {
		t.Fatalf("Failed to create volumeSpec with combination of block mode and volumeDevices. err: %v", err)
	}
}

func TestCreateVolumeSpec_Invalid_File_VolumeDevices(t *testing.T) {
	// create dswp
	mode := v1.PersistentVolumeFilesystem
	pv := &v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: "dswp-test-volume-name",
		},
		Spec: v1.PersistentVolumeSpec{
			ClaimRef:   &v1.ObjectReference{Namespace: "ns", Name: "file-bound"},
			VolumeMode: &mode,
		},
	}
	pvc := &v1.PersistentVolumeClaim{
		Spec: v1.PersistentVolumeClaimSpec{
			VolumeName: "dswp-test-volume-name",
		},
		Status: v1.PersistentVolumeClaimStatus{
			Phase: v1.ClaimBound,
		},
	}
	dswp, fakePodManager, _, _, _ := createDswpWithVolume(t, pv, pvc)

	// create pod
	containers := []v1.Container{
		{
			VolumeDevices: []v1.VolumeDevice{
				{
					Name:       "dswp-test-volume-name",
					DevicePath: "/dev/sdb",
				},
			},
		},
	}
	pod := createPodWithVolume("dswp-test-pod", "dswp-test-volume-name", "file-bound", containers)

	logger, _ := ktesting.NewTestContext(t)
	fakePodManager.AddPod(pod)
	mountsMap, devicesMap, _ := util.GetPodVolumeNames(pod, false /* collectSELinuxOptions */)
	_, volumeSpec, _, err :=
		dswp.createVolumeSpec(logger, pod.Spec.Volumes[0], pod, mountsMap, devicesMap)

	// Assert
	if volumeSpec != nil || err == nil {
		t.Fatalf("Unexpected volumeMode and volumeMounts/volumeDevices combination is accepted")
	}
}

func TestCreateVolumeSpec_Invalid_Block_VolumeMounts(t *testing.T) {
	// create dswp
	mode := v1.PersistentVolumeBlock
	pv := &v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: "dswp-test-volume-name",
		},
		Spec: v1.PersistentVolumeSpec{
			ClaimRef:   &v1.ObjectReference{Namespace: "ns", Name: "block-bound"},
			VolumeMode: &mode,
		},
	}
	pvc := &v1.PersistentVolumeClaim{
		Spec: v1.PersistentVolumeClaimSpec{
			VolumeName: "dswp-test-volume-name",
		},
		Status: v1.PersistentVolumeClaimStatus{
			Phase: v1.ClaimBound,
		},
	}
	dswp, fakePodManager, _, _, _ := createDswpWithVolume(t, pv, pvc)

	// create pod
	containers := []v1.Container{
		{
			VolumeMounts: []v1.VolumeMount{
				{
					Name:      "dswp-test-volume-name",
					MountPath: "/mnt",
				},
			},
		},
	}
	pod := createPodWithVolume("dswp-test-pod", "dswp-test-volume-name", "block-bound", containers)

	logger, _ := ktesting.NewTestContext(t)
	fakePodManager.AddPod(pod)
	mountsMap, devicesMap, _ := util.GetPodVolumeNames(pod, false /* collectSELinuxOptions */)
	_, volumeSpec, _, err :=
		dswp.createVolumeSpec(logger, pod.Spec.Volumes[0], pod, mountsMap, devicesMap)

	// Assert
	if volumeSpec != nil || err == nil {
		t.Fatalf("Unexpected volumeMode and volumeMounts/volumeDevices combination is accepted")
	}
}

func TestCheckVolumeFSResize(t *testing.T) {
	setCapacity := func(pv *v1.PersistentVolume, pvc *v1.PersistentVolumeClaim, capacity int) {
		pv.Spec.Capacity = volumeCapacity(capacity)
		pvc.Spec.Resources.Requests = volumeCapacity(capacity)
	}

	testcases := []struct {
		resize      func(*testing.T, *v1.PersistentVolume, *v1.PersistentVolumeClaim, *desiredStateOfWorldPopulator)
		verify      func(*testing.T, []v1.UniqueVolumeName, v1.UniqueVolumeName)
		readOnlyVol bool
		volumeMode  v1.PersistentVolumeMode
		volumeType  string
	}{
		{
			// No resize request for volume, volumes in ASW shouldn't be marked as fsResizeRequired
			resize: func(*testing.T, *v1.PersistentVolume, *v1.PersistentVolumeClaim, *desiredStateOfWorldPopulator) {
			},
			verify: func(t *testing.T, vols []v1.UniqueVolumeName, _ v1.UniqueVolumeName) {
				if len(vols) > 0 {
					t.Errorf("No resize request for any volumes, but found resize required volumes in ASW: %v", vols)
				}
			},
			volumeMode: v1.PersistentVolumeFilesystem,
		},

		{
			// Make volume used as ReadOnly, so volume shouldn't be marked as fsResizeRequired
			resize: func(_ *testing.T, pv *v1.PersistentVolume, pvc *v1.PersistentVolumeClaim, _ *desiredStateOfWorldPopulator) {
				setCapacity(pv, pvc, 2)
			},
			verify: func(t *testing.T, vols []v1.UniqueVolumeName, _ v1.UniqueVolumeName) {
				if len(vols) > 0 {
					t.Errorf("volume mounted as ReadOnly, but found resize required volumes in ASW: %v", vols)
				}
			},
			readOnlyVol: true,
			volumeMode:  v1.PersistentVolumeFilesystem,
		},
		{
			// Clear ASW, so volume shouldn't be marked as fsResizeRequired because they are not mounted
			resize: func(_ *testing.T, pv *v1.PersistentVolume, pvc *v1.PersistentVolumeClaim, dswp *desiredStateOfWorldPopulator) {
				setCapacity(pv, pvc, 2)
				clearASW(dswp.actualStateOfWorld, dswp.desiredStateOfWorld, t)
			},
			verify: func(t *testing.T, vols []v1.UniqueVolumeName, _ v1.UniqueVolumeName) {
				if len(vols) > 0 {
					t.Errorf("volume hasn't been mounted, but found resize required volumes in ASW: %v", vols)
				}
			},
			volumeMode: v1.PersistentVolumeFilesystem,
		},
		{
			// volume in ASW should be marked as fsResizeRequired
			resize: func(_ *testing.T, pv *v1.PersistentVolume, pvc *v1.PersistentVolumeClaim, _ *desiredStateOfWorldPopulator) {
				setCapacity(pv, pvc, 2)
			},
			verify: func(t *testing.T, vols []v1.UniqueVolumeName, volName v1.UniqueVolumeName) {
				if len(vols) == 0 {
					t.Fatalf("Request resize for volume, but volume in ASW hasn't been marked as fsResizeRequired")
				}
				if len(vols) != 1 {
					t.Errorf("Some unexpected volumes are marked as fsResizeRequired: %v", vols)
				}
				if vols[0] != volName {
					t.Fatalf("Mark wrong volume as fsResizeRequired: %s", vols[0])
				}
			},
			volumeMode: v1.PersistentVolumeFilesystem,
		},
		{
			// volume in ASW should be marked as fsResizeRequired
			resize: func(_ *testing.T, pv *v1.PersistentVolume, pvc *v1.PersistentVolumeClaim, _ *desiredStateOfWorldPopulator) {
				setCapacity(pv, pvc, 2)
			},
			verify: func(t *testing.T, vols []v1.UniqueVolumeName, volName v1.UniqueVolumeName) {
				if len(vols) == 0 {
					t.Fatalf("Requested resize for volume, but volume in ASW hasn't been marked as fsResizeRequired")
				}
				if len(vols) != 1 {
					t.Errorf("Some unexpected volumes are marked as fsResizeRequired: %v", vols)
				}
				if vols[0] != volName {
					t.Fatalf("Mark wrong volume as fsResizeRequired: %s", vols[0])
				}
			},
			volumeMode: v1.PersistentVolumeBlock,
		},
		{
			// volume in ASW should be marked as fsResizeRequired
			resize: func(_ *testing.T, pv *v1.PersistentVolume, pvc *v1.PersistentVolumeClaim, _ *desiredStateOfWorldPopulator) {
				setCapacity(pv, pvc, 2)
			},
			verify: func(t *testing.T, vols []v1.UniqueVolumeName, volName v1.UniqueVolumeName) {
				if len(vols) == 0 {
					t.Fatalf("Requested resize for volume, but volume in ASW hasn't been marked as fsResizeRequired")
				}
				if len(vols) != 1 {
					t.Errorf("Some unexpected volumes are marked as fsResizeRequired: %v", vols)
				}
				if vols[0] != volName {
					t.Fatalf("Mark wrong volume as fsResizeRequired: %s", vols[0])
				}
			},
			volumeMode: v1.PersistentVolumeFilesystem,
			volumeType: "ephemeral",
		},
	}

	for _, tc := range testcases {
		var pod *v1.Pod
		var pvc *v1.PersistentVolumeClaim
		var pv *v1.PersistentVolume

		if tc.volumeType == "ephemeral" {
			pod, pv, pvc = createEphemeralVolumeObjects("dswp-test-pod", "dswp-test-volume-name", true, &tc.volumeMode)
		} else {
			pv, pvc = createResizeRelatedVolumes(&tc.volumeMode)
			containers := []v1.Container{}

			if tc.volumeMode == v1.PersistentVolumeFilesystem {
				containers = append(containers, v1.Container{
					VolumeMounts: []v1.VolumeMount{
						{
							Name:      pv.Name,
							MountPath: "/mnt",
							ReadOnly:  tc.readOnlyVol,
						},
					},
				})
			} else {
				containers = append(containers, v1.Container{
					VolumeDevices: []v1.VolumeDevice{
						{
							Name:       pv.Name,
							DevicePath: "/mnt/foobar",
						},
					},
				})
			}

			pod = createPodWithVolume("dswp-test-pod", "dswp-test-volume-name", "file-bound", containers)
			pod.Spec.Volumes[0].VolumeSource.PersistentVolumeClaim.ReadOnly = tc.readOnlyVol
		}

		dswp, fakePodManager, fakeDSW, _, _ := createDswpWithVolume(t, pv, pvc)
		fakeASW := dswp.actualStateOfWorld
		uniquePodName := types.UniquePodName(pod.UID)
		uniqueVolumeName := v1.UniqueVolumeName("fake-plugin/" + pod.Spec.Volumes[0].Name)

		fakePodManager.AddPod(pod)
		// Fill the dsw to contains volumes and pods.
		tCtx := ktesting.Init(t)
		dswp.findAndAddNewPods(tCtx)
		reconcileASW(fakeASW, fakeDSW, t)

		func() {
			tc.resize(t, pv, pvc, dswp)

			resizeRequiredVolumes := reprocess(tCtx, dswp, uniquePodName, fakeDSW, fakeASW, *pv.Spec.Capacity.Storage())

			tc.verify(t, resizeRequiredVolumes, uniqueVolumeName)
		}()
	}
}

func TestCheckVolumeSELinux(t *testing.T) {
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.SELinuxMountReadWriteOncePod, true)
	fullOpts := &v1.SELinuxOptions{
		User:  "system_u",
		Role:  "object_r",
		Type:  "container_t",
		Level: "s0:c1,c2",
	}
	differentFullOpts := &v1.SELinuxOptions{
		User:  "system_u",
		Role:  "object_r",
		Type:  "container_t",
		Level: "s0:c9998,c9999",
	}
	partialOpts := &v1.SELinuxOptions{
		Level: "s0:c3,c4",
	}

	testcases := []struct {
		name                         string
		accessModes                  []v1.PersistentVolumeAccessMode
		existingContainerSELinuxOpts *v1.SELinuxOptions
		newContainerSELinuxOpts      *v1.SELinuxOptions
		seLinuxMountFeatureEnabled   bool
		pluginSupportsSELinux        bool
		expectError                  bool
		expectedContext              string
	}{
		{
			name:                    "RWOP with plugin with SELinux with full context in pod",
			accessModes:             []v1.PersistentVolumeAccessMode{v1.ReadWriteOncePod},
			newContainerSELinuxOpts: fullOpts,
			pluginSupportsSELinux:   true,
			expectedContext:         "system_u:object_r:container_file_t:s0:c1,c2",
		},
		{
			name:                    "RWOP with plugin with SELinux with partial context in pod",
			accessModes:             []v1.PersistentVolumeAccessMode{v1.ReadWriteOncePod},
			newContainerSELinuxOpts: partialOpts,
			pluginSupportsSELinux:   true,
			expectedContext:         "system_u:object_r:container_file_t:s0:c3,c4",
		},
		{
			name:                    "RWX with plugin with SELinux with full context in pod and SELinuxMount feature disabled",
			accessModes:             []v1.PersistentVolumeAccessMode{v1.ReadWriteMany},
			newContainerSELinuxOpts: fullOpts,
			pluginSupportsSELinux:   true,
			expectedContext:         "", // RWX volumes don't support SELinux
		},
		{
			name:                       "RWX with plugin with SELinux with full context in pod and SELinuxMount feature enabled",
			accessModes:                []v1.PersistentVolumeAccessMode{v1.ReadWriteMany},
			newContainerSELinuxOpts:    fullOpts,
			pluginSupportsSELinux:      true,
			seLinuxMountFeatureEnabled: true,
			expectedContext:            "system_u:object_r:container_file_t:s0:c1,c2",
		},
		{
			name:                    "RWOP with plugin with no SELinux with full context in pod",
			accessModes:             []v1.PersistentVolumeAccessMode{v1.ReadWriteOncePod},
			newContainerSELinuxOpts: fullOpts,
			pluginSupportsSELinux:   false,
			expectedContext:         "", // plugin doesn't support SELinux
		},
		{
			name:                    "RWOP with plugin with SELinux with no context in pod",
			accessModes:             []v1.PersistentVolumeAccessMode{v1.ReadWriteOncePod},
			newContainerSELinuxOpts: nil,
			pluginSupportsSELinux:   true,
			expectedContext:         "",
		},
		{
			name:                         "RWOP with plugin with SELinux with full context in pod with existing pod",
			accessModes:                  []v1.PersistentVolumeAccessMode{v1.ReadWriteOncePod},
			existingContainerSELinuxOpts: fullOpts,
			newContainerSELinuxOpts:      fullOpts,
			pluginSupportsSELinux:        true,
			expectedContext:              "system_u:object_r:container_file_t:s0:c1,c2",
		},
		{
			name:                         "mismatched SELinux with RWX - success",
			accessModes:                  []v1.PersistentVolumeAccessMode{v1.ReadWriteMany},
			existingContainerSELinuxOpts: fullOpts,
			newContainerSELinuxOpts:      differentFullOpts,
			pluginSupportsSELinux:        true,
			expectedContext:              "",
		},
		{
			name:                         "mismatched SELinux with RWX and SELinuxMount feature disabled",
			accessModes:                  []v1.PersistentVolumeAccessMode{v1.ReadWriteMany},
			existingContainerSELinuxOpts: fullOpts,
			newContainerSELinuxOpts:      differentFullOpts,
			pluginSupportsSELinux:        true,
			expectedContext:              "",
		},
		{
			name:                         "mismatched SELinux with RWX and SELinuxMount feature enabled",
			accessModes:                  []v1.PersistentVolumeAccessMode{v1.ReadWriteMany},
			existingContainerSELinuxOpts: fullOpts,
			newContainerSELinuxOpts:      differentFullOpts,
			pluginSupportsSELinux:        true,
			seLinuxMountFeatureEnabled:   true,
			expectError:                  true,
			// The original seLinuxOpts are kept in DSW
			expectedContext: "system_u:object_r:container_file_t:s0:c1,c2",
		},
		{
			name:                         "mismatched SELinux with RWOP - failure",
			accessModes:                  []v1.PersistentVolumeAccessMode{v1.ReadWriteOncePod},
			existingContainerSELinuxOpts: fullOpts,
			newContainerSELinuxOpts:      differentFullOpts,
			pluginSupportsSELinux:        true,
			expectError:                  true,
			// The original seLinuxOpts are kept in DSW
			expectedContext: "system_u:object_r:container_file_t:s0:c1,c2",
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			pv := &v1.PersistentVolume{
				ObjectMeta: metav1.ObjectMeta{
					Name: "dswp-test-volume-name",
				},
				Spec: v1.PersistentVolumeSpec{
					VolumeMode:             ptr.To(v1.PersistentVolumeFilesystem),
					PersistentVolumeSource: v1.PersistentVolumeSource{RBD: &v1.RBDPersistentVolumeSource{}},
					Capacity:               volumeCapacity(1),
					ClaimRef:               &v1.ObjectReference{Namespace: "ns", Name: "file-bound"},
					AccessModes:            tc.accessModes,
				},
			}
			pvc := &v1.PersistentVolumeClaim{
				Spec: v1.PersistentVolumeClaimSpec{
					VolumeMode: ptr.To(v1.PersistentVolumeFilesystem),
					VolumeName: pv.Name,
					Resources: v1.VolumeResourceRequirements{
						Requests: pv.Spec.Capacity,
					},
					AccessModes: tc.accessModes,
				},
				Status: v1.PersistentVolumeClaimStatus{
					Phase:    v1.ClaimBound,
					Capacity: pv.Spec.Capacity,
				},
			}

			container := v1.Container{
				SecurityContext: &v1.SecurityContext{
					SELinuxOptions: nil,
				},
				VolumeMounts: []v1.VolumeMount{
					{
						Name:      pv.Name,
						MountPath: "/mnt",
					},
				},
			}
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.SELinuxMount, tc.seLinuxMountFeatureEnabled)

			fakeVolumePluginMgr, plugin := volumetesting.GetTestKubeletVolumePluginMgr(t)
			plugin.SupportsSELinux = tc.pluginSupportsSELinux
			dswp, fakePodManager, fakeDSW, _, _ := createDswpWithVolumeWithCustomPluginMgr(pv, pvc, fakeVolumePluginMgr)

			tCtx := ktesting.Init(t)
			var existingPod *v1.Pod
			if tc.existingContainerSELinuxOpts != nil {
				// Add existing pod + volume
				existingContainer := container
				existingContainer.SecurityContext.SELinuxOptions = tc.existingContainerSELinuxOpts
				existingPod = createPodWithVolume("dswp-old-pod", "dswp-test-volume-name", "file-bound", []v1.Container{existingContainer})
				fakePodManager.AddPod(existingPod)
				dswp.findAndAddNewPods(tCtx)
			}

			newContainer := container
			newContainer.SecurityContext.SELinuxOptions = tc.newContainerSELinuxOpts
			newPod := createPodWithVolume("dswp-test-pod", "dswp-test-volume-name", "file-bound", []v1.Container{newContainer})

			// Act - add the new Pod
			fakePodManager.AddPod(newPod)
			dswp.findAndAddNewPods(tCtx)

			// Assert

			// Check the global volume state
			uniquePodName := types.UniquePodName(newPod.UID)
			uniqueVolumeName := v1.UniqueVolumeName("fake-plugin/" + newPod.Spec.Volumes[0].Name)
			volumeExists := fakeDSW.VolumeExists(uniqueVolumeName, tc.expectedContext)
			if !volumeExists {
				t.Errorf(
					"VolumeExists(%q) failed. Expected: <true> Actual: <%v>",
					uniqueVolumeName,
					volumeExists)
			}

			// Check the Pod local volume state
			podExistsInVolume := fakeDSW.PodExistsInVolume(uniquePodName, uniqueVolumeName, tc.expectedContext)
			if !podExistsInVolume && !tc.expectError {
				t.Errorf(
					"DSW PodExistsInVolume returned incorrect value. Expected: <true> Actual: <%v>",
					podExistsInVolume)
			}
			if podExistsInVolume && tc.expectError {
				t.Errorf(
					"DSW PodExistsInVolume returned incorrect value. Expected: <false> Actual: <%v>",
					podExistsInVolume)
			}
			errors := fakeDSW.GetPodsWithErrors()
			if tc.expectError && len(errors) == 0 {
				t.Errorf("Expected Pod error, got none")
			}
			if !tc.expectError && len(errors) > 0 {
				t.Errorf("Unexpected Pod errors: %v", errors)
			}
			verifyVolumeExistsInVolumesToMount(t, uniqueVolumeName, false /* expectReportedInUse */, fakeDSW)
		})
	}
}

func createResizeRelatedVolumes(volumeMode *v1.PersistentVolumeMode) (pv *v1.PersistentVolume, pvc *v1.PersistentVolumeClaim) {
	pv = &v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: "dswp-test-volume-name",
		},
		Spec: v1.PersistentVolumeSpec{
			PersistentVolumeSource: v1.PersistentVolumeSource{RBD: &v1.RBDPersistentVolumeSource{}},
			Capacity:               volumeCapacity(1),
			ClaimRef:               &v1.ObjectReference{Namespace: "ns", Name: "file-bound"},
			VolumeMode:             volumeMode,
		},
	}
	pvc = &v1.PersistentVolumeClaim{
		Spec: v1.PersistentVolumeClaimSpec{
			VolumeName: pv.Name,
			Resources: v1.VolumeResourceRequirements{
				Requests: pv.Spec.Capacity,
			},
		},
		Status: v1.PersistentVolumeClaimStatus{
			Phase:    v1.ClaimBound,
			Capacity: pv.Spec.Capacity,
		},
	}
	return
}

func volumeCapacity(size int) v1.ResourceList {
	return v1.ResourceList{v1.ResourceStorage: resource.MustParse(fmt.Sprintf("%dGi", size))}
}

func reconcileASW(asw cache.ActualStateOfWorld, dsw cache.DesiredStateOfWorld, t *testing.T) {
	logger, _ := ktesting.NewTestContext(t)
	for _, volumeToMount := range dsw.GetVolumesToMount() {
		err := asw.MarkVolumeAsAttached(logger, volumeToMount.VolumeName, volumeToMount.VolumeSpec, "", "")
		if err != nil {
			t.Fatalf("Unexpected error when MarkVolumeAsAttached: %v", err)
		}
		markVolumeOpts := operationexecutor.MarkVolumeOpts{
			PodName:             volumeToMount.PodName,
			PodUID:              volumeToMount.Pod.UID,
			VolumeName:          volumeToMount.VolumeName,
			OuterVolumeSpecName: volumeToMount.OuterVolumeSpecName,
			VolumeGIDVolume:     volumeToMount.VolumeGIDValue,
			VolumeSpec:          volumeToMount.VolumeSpec,
			VolumeMountState:    operationexecutor.VolumeMounted,
		}
		err = asw.MarkVolumeAsMounted(markVolumeOpts)
		if err != nil {
			t.Fatalf("Unexpected error when MarkVolumeAsMounted: %v", err)
		}
	}
}

func clearASW(asw cache.ActualStateOfWorld, dsw cache.DesiredStateOfWorld, t *testing.T) {
	for _, volumeToMount := range dsw.GetVolumesToMount() {
		err := asw.MarkVolumeAsUnmounted(volumeToMount.PodName, volumeToMount.VolumeName)
		if err != nil {
			t.Fatalf("Unexpected error when MarkVolumeAsUnmounted: %v", err)
		}
	}
	for _, volumeToMount := range dsw.GetVolumesToMount() {
		asw.MarkVolumeAsDetached(volumeToMount.VolumeName, "")
	}
}

func reprocess(ctx context.Context, dswp *desiredStateOfWorldPopulator, uniquePodName types.UniquePodName,
	dsw cache.DesiredStateOfWorld, asw cache.ActualStateOfWorld, newSize resource.Quantity) []v1.UniqueVolumeName {
	dswp.ReprocessPod(uniquePodName)
	dswp.findAndAddNewPods(ctx)
	return getResizeRequiredVolumes(dsw, asw, newSize)
}

func getResizeRequiredVolumes(dsw cache.DesiredStateOfWorld, asw cache.ActualStateOfWorld, newSize resource.Quantity) []v1.UniqueVolumeName {
	resizeRequiredVolumes := []v1.UniqueVolumeName{}
	for _, volumeToMount := range dsw.GetVolumesToMount() {
		_, _, err := asw.PodExistsInVolume(volumeToMount.PodName, volumeToMount.VolumeName, newSize, "" /* SELinuxContext */)
		if cache.IsFSResizeRequiredError(err) {
			resizeRequiredVolumes = append(resizeRequiredVolumes, volumeToMount.VolumeName)
		}
	}
	return resizeRequiredVolumes
}

func verifyVolumeExistsInVolumesToMount(t *testing.T, expectedVolumeName v1.UniqueVolumeName, expectReportedInUse bool, dsw cache.DesiredStateOfWorld) {
	volumesToMount := dsw.GetVolumesToMount()
	for _, volume := range volumesToMount {
		if volume.VolumeName == expectedVolumeName {
			if volume.ReportedInUse != expectReportedInUse {
				t.Fatalf(
					"Found volume %v in the list of VolumesToMount, but ReportedInUse incorrect. Expected: <%v> Actual: <%v>",
					expectedVolumeName,
					expectReportedInUse,
					volume.ReportedInUse)
			}
			return
		}
	}

	t.Fatalf(
		"Could not find volume %v in the list of desired state of world volumes to mount %+v",
		expectedVolumeName,
		volumesToMount)
}

func createPodWithVolume(pod, pv, pvc string, containers []v1.Container) *v1.Pod {
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pod,
			UID:       kubetypes.UID(pod + "-uid"),
			Namespace: "dswp-test",
		},
		Spec: v1.PodSpec{
			Volumes: []v1.Volume{
				{
					Name: pv,
					VolumeSource: v1.VolumeSource{
						GCEPersistentDisk: &v1.GCEPersistentDiskVolumeSource{
							PDName: "dswp-test-fake-device",
						},
						PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{
							ClaimName: pvc,
						},
					},
				},
			},
			Containers: containers,
		},
		Status: v1.PodStatus{
			Phase: v1.PodPhase("Running"),
		},
	}
}

func createEphemeralVolumeObjects(podName, volumeName string, owned bool, volumeMode *v1.PersistentVolumeMode) (pod *v1.Pod, pv *v1.PersistentVolume, pvc *v1.PersistentVolumeClaim) {
	pod = &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			UID:       "dswp-test-pod-uid",
			Namespace: "dswp-test",
		},
		Spec: v1.PodSpec{
			Volumes: []v1.Volume{
				{
					Name: volumeName,
					VolumeSource: v1.VolumeSource{
						Ephemeral: &v1.EphemeralVolumeSource{
							VolumeClaimTemplate: &v1.PersistentVolumeClaimTemplate{},
						},
					},
				},
			},
			Containers: []v1.Container{
				{
					VolumeMounts: []v1.VolumeMount{
						{
							Name:      volumeName,
							MountPath: "/mnt",
						},
					},
				},
			},
		},
		Status: v1.PodStatus{
			Phase: v1.PodPhase("Running"),
		},
	}
	pv = &v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: volumeName,
		},
		Spec: v1.PersistentVolumeSpec{
			ClaimRef:   &v1.ObjectReference{Namespace: "ns", Name: "file-bound"},
			Capacity:   volumeCapacity(1),
			VolumeMode: volumeMode,
		},
	}
	pvc = &v1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName + "-" + volumeName,
			Namespace: pod.Namespace,
		},
		Spec: v1.PersistentVolumeClaimSpec{
			VolumeName: volumeName,
			Resources: v1.VolumeResourceRequirements{
				Requests: pv.Spec.Capacity,
			},
		},
		Status: v1.PersistentVolumeClaimStatus{
			Phase:    v1.ClaimBound,
			Capacity: pv.Spec.Capacity,
		},
	}
	if owned {
		controller := true
		pvc.OwnerReferences = []metav1.OwnerReference{
			{
				UID:        pod.UID,
				Name:       podName,
				Controller: &controller,
			},
		}
	}
	return
}

func createDswpWithVolume(t *testing.T, pv *v1.PersistentVolume, pvc *v1.PersistentVolumeClaim) (*desiredStateOfWorldPopulator, kubepod.Manager, cache.DesiredStateOfWorld, *containertest.FakeRuntime, *fakePodStateProvider) {
	fakeVolumePluginMgr, _ := volumetesting.GetTestKubeletVolumePluginMgr(t)
	dswp, fakePodManager, fakesDSW, fakeRuntime, fakeStateProvider := createDswpWithVolumeWithCustomPluginMgr(pv, pvc, fakeVolumePluginMgr)
	return dswp, fakePodManager, fakesDSW, fakeRuntime, fakeStateProvider
}

type fakePodStateProvider struct {
	terminating map[kubetypes.UID]struct{}
	removed     map[kubetypes.UID]struct{}
}

func (p *fakePodStateProvider) ShouldPodContainersBeTerminating(uid kubetypes.UID) bool {
	_, ok := p.terminating[uid]
	// if ShouldPodRuntimeBeRemoved returns true, ShouldPodContainerBeTerminating should also return true
	if !ok {
		_, ok = p.removed[uid]
	}
	return ok
}

func (p *fakePodStateProvider) ShouldPodRuntimeBeRemoved(uid kubetypes.UID) bool {
	_, ok := p.removed[uid]
	return ok
}

func createDswpWithVolumeWithCustomPluginMgr(pv *v1.PersistentVolume, pvc *v1.PersistentVolumeClaim,
	fakeVolumePluginMgr *volume.VolumePluginMgr) (*desiredStateOfWorldPopulator, kubepod.Manager, cache.DesiredStateOfWorld, *containertest.FakeRuntime, *fakePodStateProvider) {
	fakeClient := &fake.Clientset{}
	fakeClient.AddReactor("get", "persistentvolumeclaims", func(action core.Action) (bool, runtime.Object, error) {
		return true, pvc, nil
	})
	fakeClient.AddReactor("get", "persistentvolumes", func(action core.Action) (bool, runtime.Object, error) {
		return true, pv, nil
	})

	fakePodManager := kubepod.NewBasicPodManager()

	seLinuxTranslator := util.NewFakeSELinuxLabelTranslator()
	fakesDSW := cache.NewDesiredStateOfWorld(fakeVolumePluginMgr, seLinuxTranslator)
	fakeASW := cache.NewActualStateOfWorld("fake", fakeVolumePluginMgr)
	fakeRuntime := &containertest.FakeRuntime{}
	fakeStateProvider := &fakePodStateProvider{}

	csiTranslator := csitrans.New()
	dswp := &desiredStateOfWorldPopulator{
		kubeClient:          fakeClient,
		loopSleepDuration:   100 * time.Millisecond,
		podManager:          fakePodManager,
		podStateProvider:    fakeStateProvider,
		desiredStateOfWorld: fakesDSW,
		actualStateOfWorld:  fakeASW,
		pods: processedPods{
			processedPods: make(map[types.UniquePodName]bool)},
		kubeContainerRuntime:     fakeRuntime,
		csiMigratedPluginManager: csimigration.NewPluginManager(csiTranslator, utilfeature.DefaultFeatureGate),
		intreeToCSITranslator:    csiTranslator,
		volumePluginMgr:          fakeVolumePluginMgr,
	}
	return dswp, fakePodManager, fakesDSW, fakeRuntime, fakeStateProvider
}
