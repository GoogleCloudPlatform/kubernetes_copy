/*
Copyright 2022 The Kubernetes Authors.

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
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2/ktesting"
	"k8s.io/kubernetes/pkg/volume"
	volumetesting "k8s.io/kubernetes/pkg/volume/testing"
	"k8s.io/kubernetes/pkg/volume/util"
)

func TestReconstructVolumes(t *testing.T) {
	tests := []struct {
		name                                string
		volumePaths                         []string
		expectedVolumesNeedDevicePath       []string
		expectedVolumesFailedReconstruction []string
		verifyFunc                          func(rcInstance *reconciler, fakePlugin *volumetesting.FakeVolumePlugin) error
	}{
		{
			name: "when two pods are using same volume and both are deleted",
			volumePaths: []string{
				filepath.Join("pod1", "volumes", "fake-plugin", "pvc-abcdef"),
				filepath.Join("pod2", "volumes", "fake-plugin", "pvc-abcdef"),
			},
			expectedVolumesNeedDevicePath:       []string{"fake-plugin/pvc-abcdef", "fake-plugin/pvc-abcdef"},
			expectedVolumesFailedReconstruction: []string{},
			verifyFunc: func(rcInstance *reconciler, fakePlugin *volumetesting.FakeVolumePlugin) error {
				mountedPods := rcInstance.actualStateOfWorld.GetMountedVolumes()
				if len(mountedPods) != 0 {
					return fmt.Errorf("expected 0 certain pods in asw got %d", len(mountedPods))
				}
				allPods := rcInstance.actualStateOfWorld.GetAllMountedVolumes()
				if len(allPods) != 2 {
					return fmt.Errorf("expected 2 uncertain pods in asw got %d", len(allPods))
				}
				volumes := rcInstance.actualStateOfWorld.GetPossiblyMountedVolumesForPod("pod1")
				if len(volumes) != 1 {
					return fmt.Errorf("expected 1 uncertain volume in asw got %d", len(volumes))
				}
				// The volume should be marked as reconstructed in ASW
				if reconstructed := rcInstance.actualStateOfWorld.IsVolumeReconstructed("fake-plugin/pvc-abcdef", "pod1"); !reconstructed {
					t.Errorf("expected volume to be marked as reconstructed, got %v", reconstructed)
				}
				return nil
			},
		},
		{
			name: "when reconstruction fails for a volume, volumes should be cleaned up",
			volumePaths: []string{
				filepath.Join("pod1", "volumes", "missing-plugin", "pvc-abcdef"),
			},
			expectedVolumesNeedDevicePath:       []string{},
			expectedVolumesFailedReconstruction: []string{"pvc-abcdef"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tmpKubeletDir, err := os.MkdirTemp("", "")
			if err != nil {
				t.Fatalf("can't make a temp directory for kubeletPods: %v", err)
			}
			defer os.RemoveAll(tmpKubeletDir)

			// create kubelet pod directory
			tmpKubeletPodDir := filepath.Join(tmpKubeletDir, "pods")
			os.MkdirAll(tmpKubeletPodDir, 0755)

			mountPaths := []string{}

			// create pod and volume directories so as reconciler can find them.
			for _, volumePath := range tc.volumePaths {
				vp := filepath.Join(tmpKubeletPodDir, volumePath)
				mountPaths = append(mountPaths, vp)
				os.MkdirAll(vp, 0755)
			}

			rc, fakePlugin := getReconciler(tmpKubeletDir, t, mountPaths, nil /*custom kubeclient*/)
			rcInstance, _ := rc.(*reconciler)

			// Act
			rcInstance.reconstructVolumes()

			// Assert
			// Convert to []UniqueVolumeName
			expectedVolumes := make([]v1.UniqueVolumeName, len(tc.expectedVolumesNeedDevicePath))
			for i := range tc.expectedVolumesNeedDevicePath {
				expectedVolumes[i] = v1.UniqueVolumeName(tc.expectedVolumesNeedDevicePath[i])
			}
			if !reflect.DeepEqual(expectedVolumes, rcInstance.volumesNeedUpdateFromNodeStatus) {
				t.Errorf("Expected expectedVolumesNeedDevicePath:\n%v\n got:\n%v", expectedVolumes, rcInstance.volumesNeedUpdateFromNodeStatus)
			}

			volumesFailedReconstruction := sets.New[string]()
			for _, vol := range rcInstance.volumesFailedReconstruction {
				volumesFailedReconstruction.Insert(vol.volumeSpecName)
			}
			if !reflect.DeepEqual(sets.List(volumesFailedReconstruction), tc.expectedVolumesFailedReconstruction) {
				t.Errorf("Expected volumesFailedReconstruction:\n%v\n got:\n%v", tc.expectedVolumesFailedReconstruction, sets.List(volumesFailedReconstruction))
			}

			if tc.verifyFunc != nil {
				if err := tc.verifyFunc(rcInstance, fakePlugin); err != nil {
					t.Errorf("Test %s failed: %v", tc.name, err)
				}
			}
		})
	}
}

func TestCleanOrphanVolumes(t *testing.T) {
	type podInfo struct {
		podName         string
		podUID          string
		outerVolumeName string
		innerVolumeName string
	}
	defaultPodInfo := podInfo{
		podName:         "pod1",
		podUID:          "pod1uid",
		outerVolumeName: "volume-name",
		innerVolumeName: "volume-name",
	}
	defaultVolume := podVolume{
		podName:        "pod1uid",
		volumeSpecName: "volume-name",
		volumePath:     "",
		pluginName:     "fake-plugin",
		volumeMode:     v1.PersistentVolumeFilesystem,
	}

	tests := []struct {
		name                        string
		podInfos                    []podInfo
		volumesFailedReconstruction []podVolume
		expectedUnmounts            int
	}{
		{
			name:                        "volume is in DSW and is not cleaned",
			podInfos:                    []podInfo{defaultPodInfo},
			volumesFailedReconstruction: []podVolume{defaultVolume},
			expectedUnmounts:            0,
		},
		{
			name:                        "volume is not in DSW and is cleaned",
			podInfos:                    []podInfo{},
			volumesFailedReconstruction: []podVolume{defaultVolume},
			expectedUnmounts:            1,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange
			tmpKubeletDir, err := os.MkdirTemp("", "")
			if err != nil {
				t.Fatalf("can't make a temp directory for kubeletPods: %v", err)
			}
			defer os.RemoveAll(tmpKubeletDir)

			// create kubelet pod directory
			tmpKubeletPodDir := filepath.Join(tmpKubeletDir, "pods")
			os.MkdirAll(tmpKubeletPodDir, 0755)

			mountPaths := []string{}

			rc, fakePlugin := getReconciler(tmpKubeletDir, t, mountPaths, nil /*custom kubeclient*/)
			rcInstance, _ := rc.(*reconciler)
			rcInstance.volumesFailedReconstruction = tc.volumesFailedReconstruction
			logger, _ := ktesting.NewTestContext(t)
			for _, tpodInfo := range tc.podInfos {
				pod := getInlineFakePod(tpodInfo.podName, tpodInfo.podUID, tpodInfo.outerVolumeName, tpodInfo.innerVolumeName)
				volumeSpec := &volume.Spec{Volume: &pod.Spec.Volumes[0]}
				podName := util.GetUniquePodName(pod)
				volumeName, err := rcInstance.desiredStateOfWorld.AddPodToVolume(
					podName, pod, volumeSpec, volumeSpec.Name(), "" /* volumeGIDValue */, nil /* SELinuxContext */)
				if err != nil {
					t.Fatalf("Error adding volume %s to dsow: %v", volumeSpec.Name(), err)
				}
				rcInstance.actualStateOfWorld.MarkVolumeAsAttached(logger, volumeName, volumeSpec, nodeName, "")
			}

			// Act
			rcInstance.cleanOrphanVolumes()

			// Assert
			if len(rcInstance.volumesFailedReconstruction) != 0 {
				t.Errorf("Expected volumesFailedReconstruction to be empty, got %+v", rcInstance.volumesFailedReconstruction)
			}
			// Unmount runs in a go routine, wait for its finish
			var lastErr error
			err = retryWithExponentialBackOff(testOperationBackOffDuration, func() (bool, error) {
				if err := verifyTearDownCalls(fakePlugin, tc.expectedUnmounts); err != nil {
					lastErr = err
					return false, nil
				}
				return true, nil
			})
			if err != nil {
				t.Errorf("Error waiting for volumes to get unmounted: %s: %s", err, lastErr)
			}
		})
	}
}

func verifyTearDownCalls(plugin *volumetesting.FakeVolumePlugin, expected int) error {
	unmounters := plugin.GetUnmounters()
	if len(unmounters) == 0 && (expected == 0) {
		return nil
	}
	actualCallCount := 0
	for _, unmounter := range unmounters {
		actualCallCount = unmounter.GetTearDownCallCount()
		if actualCallCount == expected {
			return nil
		}
	}
	return fmt.Errorf("expected TearDown calls %d, got %d", expected, actualCallCount)
}

func TestReconstructVolumesMount(t *testing.T) {
	// This test checks volume reconstruction + subsequent failed mount.
	// Since the volume is reconstructed, it must be marked as uncertain
	// even after a final SetUp error, see https://github.com/kubernetes/kubernetes/issues/96635
	// and https://github.com/kubernetes/kubernetes/pull/110670.

	tests := []struct {
		name            string
		volumePath      string
		expectMount     bool
		volumeMode      v1.PersistentVolumeMode
		deviceMountPath string
		podNotAdded     bool
	}{
		{
			name:       "reconstructed volume is mounted",
			volumePath: filepath.Join("pod1uid", "volumes", "fake-plugin", "volumename"),

			expectMount: true,
			volumeMode:  v1.PersistentVolumeFilesystem,
		},
		{
			name: "reconstructed volume fails to mount",
			// FailOnSetupVolumeName: MountDevice succeeds, SetUp fails
			volumePath:  filepath.Join("pod1uid", "volumes", "fake-plugin", volumetesting.FailOnSetupVolumeName),
			expectMount: false,
			volumeMode:  v1.PersistentVolumeFilesystem,
		},
		{
			name:            "reconstructed volume device map fails",
			volumePath:      filepath.Join("pod1uid", "volumeDevices", "fake-plugin", volumetesting.FailMountDeviceVolumeName),
			volumeMode:      v1.PersistentVolumeBlock,
			deviceMountPath: filepath.Join("plugins", "fake-plugin", "volumeDevices", "pluginDependentPath"),
		},
		{
			name:            "reconstructed volume, pod hasn't been added",
			volumePath:      filepath.Join("pod1uid", "volumeDevices", "fake-plugin", volumetesting.FailMountDeviceVolumeName),
			volumeMode:      v1.PersistentVolumeBlock,
			deviceMountPath: filepath.Join("plugins", "fake-plugin", "volumeDevices", "pluginDependentPath"),
			podNotAdded:     true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tmpKubeletDir, err := os.MkdirTemp("", "")
			if err != nil {
				t.Fatalf("can't make a temp directory for kubeletPods: %v", err)
			}
			defer os.RemoveAll(tmpKubeletDir)

			// create kubelet pod directory
			tmpKubeletPodDir := filepath.Join(tmpKubeletDir, "pods")
			os.MkdirAll(tmpKubeletPodDir, 0755)

			// create pod and volume directories so as reconciler can find them.
			vp := filepath.Join(tmpKubeletPodDir, tc.volumePath)
			mountPaths := []string{vp}
			os.MkdirAll(vp, 0755)

			// Arrange 2 - populate DSW
			outerName := filepath.Base(tc.volumePath)
			pod, pv, pvc := getPodPVCAndPV(tc.volumeMode, "pod1", outerName, "pvc1")
			volumeSpec := &volume.Spec{PersistentVolume: pv}
			kubeClient := createtestClientWithPVPVC(pv, pvc, v1.AttachedVolume{
				Name:       v1.UniqueVolumeName(fmt.Sprintf("fake-plugin/%s", outerName)),
				DevicePath: "fake/path",
			})

			rc, fakePlugin := getReconciler(tmpKubeletDir, t, mountPaths, kubeClient /*custom kubeclient*/)
			rcInstance, _ := rc.(*reconciler)

			// Act 1 - reconstruction
			rcInstance.reconstructVolumes()

			// Assert 1 - the volume is Uncertain
			mountedPods := rcInstance.actualStateOfWorld.GetMountedVolumes()
			if len(mountedPods) != 0 {
				t.Errorf("expected 0 mounted volumes, got %+v", mountedPods)
			}
			allPods := rcInstance.actualStateOfWorld.GetAllMountedVolumes()
			if len(allPods) != 1 {
				t.Errorf("expected 1 uncertain volume in asw, got %+v", allPods)
			}

			podName := util.GetUniquePodName(pod)
			volumeName, err := rcInstance.desiredStateOfWorld.AddPodToVolume(
				podName, pod, volumeSpec, volumeSpec.Name(), "" /* volumeGIDValue */, nil /* SELinuxContext */)
			if err != nil {
				t.Fatalf("Error adding volume %s to dsow: %v", volumeSpec.Name(), err)
			}
			logger, _ := ktesting.NewTestContext(t)
			rcInstance.actualStateOfWorld.MarkVolumeAsAttached(logger, volumeName, volumeSpec, nodeName, "")

			rcInstance.populatorHasAddedPods = func() bool {
				// Mark DSW populated to allow unmounting of volumes.
				return !tc.podNotAdded
			}
			// Mark devices paths as reconciled to allow unmounting of volumes.
			rcInstance.volumesNeedUpdateFromNodeStatus = nil

			// Act 2 - reconcile once
			rcInstance.reconcile()

			// Assert 2
			// MountDevice was attempted
			var lastErr error
			err = retryWithExponentialBackOff(testOperationBackOffDuration, func() (bool, error) {
				if tc.volumeMode == v1.PersistentVolumeFilesystem {
					if err := volumetesting.VerifyMountDeviceCallCount(1, fakePlugin); err != nil {
						lastErr = err
						return false, nil
					}
					return true, nil
				} else {
					return true, nil
				}
			})
			if err != nil {
				t.Errorf("Error waiting for volumes to get mounted: %s: %s", err, lastErr)
			}

			if tc.expectMount {
				// The volume should be fully mounted
				waitForMount(t, fakePlugin, volumeName, rcInstance.actualStateOfWorld)
				// SetUp was called and succeeded
				if err := volumetesting.VerifySetUpCallCount(1, fakePlugin); err != nil {
					t.Errorf("Expected SetUp() to be called, got %s", err)
				}
			} else {
				// The test does not expect any change in ASW, yet it needs to wait for volume operations to finish
				err = retryWithExponentialBackOff(testOperationBackOffDuration, func() (bool, error) {
					return !rcInstance.operationExecutor.IsOperationPending(volumeName, "pod1uid", nodeName), nil
				})
				if err != nil {
					t.Errorf("Error waiting for operation to get finished: %s", err)
				}
				// The volume is uncertain
				mountedPods := rcInstance.actualStateOfWorld.GetMountedVolumes()
				if len(mountedPods) != 0 {
					t.Errorf("expected 0 mounted volumes after reconcile, got %+v", mountedPods)
				}
				allPods := rcInstance.actualStateOfWorld.GetAllMountedVolumes()
				if len(allPods) != 1 {
					t.Errorf("expected 1 mounted or uncertain volumes after reconcile, got %+v", allPods)
				}
				if tc.deviceMountPath != "" {
					expectedDeviceMountPath := filepath.Join(tmpKubeletDir, tc.deviceMountPath)
					deviceMountPath := allPods[0].DeviceMountPath
					if expectedDeviceMountPath != deviceMountPath {
						t.Errorf("expected deviceMountPath to be %s, got %s", expectedDeviceMountPath, deviceMountPath)
					}
				}

			}

			// Unmount was *not* attempted in any case
			verifyTearDownCalls(fakePlugin, 0)
		})
	}
}

func getPodPVCAndPV(volumeMode v1.PersistentVolumeMode, podName, pvName, pvcName string) (*v1.Pod, *v1.PersistentVolume, *v1.PersistentVolumeClaim) {
	pv := &v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: pvName,
			UID:  "pvuid",
		},
		Spec: v1.PersistentVolumeSpec{
			ClaimRef:   &v1.ObjectReference{Name: pvcName},
			VolumeMode: &volumeMode,
		},
	}
	pvc := &v1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: pvcName,
			UID:  "pvcuid",
		},
		Spec: v1.PersistentVolumeClaimSpec{
			VolumeName: pvName,
			VolumeMode: &volumeMode,
		},
	}
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: podName,
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
	return pod, pv, pvc
}
