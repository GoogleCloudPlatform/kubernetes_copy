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

package operationexecutor

import (
	"fmt"
	"os"
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	core "k8s.io/client-go/testing"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	"k8s.io/kubernetes/pkg/features"

	"github.com/stretchr/testify/assert"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/uuid"
	fakeclient "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/events"
	"k8s.io/component-base/metrics/testutil"
	"k8s.io/kubernetes/pkg/volume"
	csitesting "k8s.io/kubernetes/pkg/volume/csi/testing"
	volumetesting "k8s.io/kubernetes/pkg/volume/testing"
	"k8s.io/kubernetes/pkg/volume/util"
	volumetypes "k8s.io/kubernetes/pkg/volume/util/types"
)

// this method just tests the volume plugin name that's used in CompleteFunc, the same plugin is also used inside the
// generated func so there is no need to test the plugin name that's used inside generated function
func TestOperationGenerator_GenerateUnmapVolumeFunc_PluginName(t *testing.T) {
	type testcase struct {
		name              string
		pluginName        string
		pvSpec            v1.PersistentVolumeSpec
		probVolumePlugins []volume.VolumePlugin
	}

	testcases := []testcase{
		{
			name:       "gce pd plugin: csi migration disabled",
			pluginName: "fake-plugin",
			pvSpec: v1.PersistentVolumeSpec{
				PersistentVolumeSource: v1.PersistentVolumeSource{
					GCEPersistentDisk: &v1.GCEPersistentDiskVolumeSource{},
				}},
			probVolumePlugins: volumetesting.ProbeVolumePlugins(volume.VolumeConfig{}),
		},
	}

	for _, tc := range testcases {
		expectedPluginName := tc.pluginName
		volumePluginMgr, tmpDir := initTestPlugins(t, tc.probVolumePlugins, tc.pluginName)
		defer os.RemoveAll(tmpDir)

		operationGenerator := getTestOperationGenerator(volumePluginMgr)

		pod := &v1.Pod{ObjectMeta: metav1.ObjectMeta{UID: types.UID(string(uuid.NewUUID()))}}
		volumeToUnmount := getTestVolumeToUnmount(pod, tc.pvSpec, tc.pluginName)

		unmapVolumeFunc, e := operationGenerator.GenerateUnmapVolumeFunc(volumeToUnmount, nil)
		if e != nil {
			t.Fatalf("Error occurred while generating unmapVolumeFunc: %v", e)
		}

		m := util.StorageOperationMetric.WithLabelValues(expectedPluginName, "unmap_volume", "success", "false")
		storageOperationDurationSecondsMetricBefore, _ := testutil.GetHistogramMetricCount(m)

		var ee error
		unmapVolumeFunc.CompleteFunc(volumetypes.CompleteFuncParam{Err: &ee})

		storageOperationDurationSecondsMetricAfter, _ := testutil.GetHistogramMetricCount(m)
		metricValueDiff := storageOperationDurationSecondsMetricAfter - storageOperationDurationSecondsMetricBefore
		assert.Equal(t, uint64(1), metricValueDiff, tc.name)
	}
}

func TestOperationGenerator_GenerateExpandAndRecoverVolumeFunc(t *testing.T) {
	nodeResizePending := v1.PersistentVolumeClaimNodeResizePending
	nodeResizeFailed := v1.PersistentVolumeClaimNodeResizeInfeasible
	var tests = []struct {
		name                 string
		pvc                  *v1.PersistentVolumeClaim
		pv                   *v1.PersistentVolume
		recoverFeatureGate   bool
		disableNodeExpansion bool
		// expectations of test
		expectedResizeStatus  v1.ClaimResourceStatus
		expectedAllocatedSize resource.Quantity
		expectResizeCall      bool
	}{
		{
			name:                  "pvc.spec.size > pv.spec.size, recover_expansion=on",
			pvc:                   getTestPVC("test-vol0", "2G", "1G", "", nil),
			pv:                    getTestPV("test-vol0", "1G"),
			recoverFeatureGate:    true,
			expectedResizeStatus:  v1.PersistentVolumeClaimNodeResizePending,
			expectedAllocatedSize: resource.MustParse("2G"),
			expectResizeCall:      true,
		},
		{
			name:                  "pvc.spec.size = pv.spec.size, recover_expansion=on",
			pvc:                   getTestPVC("test-vol0", "1G", "1G", "", nil),
			pv:                    getTestPV("test-vol0", "1G"),
			recoverFeatureGate:    true,
			expectedResizeStatus:  v1.PersistentVolumeClaimNodeResizePending,
			expectedAllocatedSize: resource.MustParse("1G"),
			expectResizeCall:      true,
		},
		{
			name:                  "pvc.spec.size = pv.spec.size, recover_expansion=on",
			pvc:                   getTestPVC("test-vol0", "1G", "1G", "1G", &nodeResizePending),
			pv:                    getTestPV("test-vol0", "1G"),
			recoverFeatureGate:    true,
			expectedResizeStatus:  v1.PersistentVolumeClaimNodeResizePending,
			expectedAllocatedSize: resource.MustParse("1G"),
			expectResizeCall:      false,
		},
		{
			name:                  "pvc.spec.size > pv.spec.size, recover_expansion=on, disable_node_expansion=true",
			pvc:                   getTestPVC("test-vol0", "2G", "1G", "", nil),
			pv:                    getTestPV("test-vol0", "1G"),
			disableNodeExpansion:  true,
			recoverFeatureGate:    true,
			expectedResizeStatus:  "",
			expectedAllocatedSize: resource.MustParse("2G"),
			expectResizeCall:      true,
		},
		{
			name:                  "pv.spec.size >= pvc.spec.size, recover_expansion=on, resize_status=node_expansion_failed",
			pvc:                   getTestPVC("test-vol0", "2G", "1G", "2G", &nodeResizeFailed),
			pv:                    getTestPV("test-vol0", "2G"),
			recoverFeatureGate:    true,
			expectedResizeStatus:  v1.PersistentVolumeClaimNodeResizePending,
			expectedAllocatedSize: resource.MustParse("2G"),
			expectResizeCall:      false,
		},
	}
	for i := range tests {
		test := tests[i]
		t.Run(test.name, func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.RecoverVolumeExpansionFailure, test.recoverFeatureGate)
			volumePluginMgr, fakePlugin := volumetesting.GetTestKubeletVolumePluginMgr(t)
			fakePlugin.DisableNodeExpansion = test.disableNodeExpansion
			pvc := test.pvc
			pv := test.pv
			og := getTestOperationGenerator(volumePluginMgr, pvc, pv)
			rsOpts := inTreeResizeOpts{
				pvc:          pvc,
				pv:           pv,
				resizerName:  fakePlugin.GetPluginName(),
				volumePlugin: fakePlugin,
			}
			ogInstance, _ := og.(*operationGenerator)

			expansionResponse := ogInstance.expandAndRecoverFunction(rsOpts)
			if expansionResponse.err != nil {
				t.Fatalf("GenerateExpandAndRecoverVolumeFunc failed: %v", expansionResponse.err)
			}
			updatedPVC := expansionResponse.pvc
			actualResizeStatus := updatedPVC.Status.AllocatedResourceStatuses[v1.ResourceStorage]
			assert.Equal(t, test.expectedResizeStatus, actualResizeStatus)
			actualAllocatedSize := updatedPVC.Status.AllocatedResources.Storage()
			if test.expectedAllocatedSize.Cmp(*actualAllocatedSize) != 0 {
				t.Fatalf("GenerateExpandAndRecoverVolumeFunc failed: expected allocated size %s, got %s", test.expectedAllocatedSize.String(), actualAllocatedSize.String())
			}
			if test.expectResizeCall != expansionResponse.resizeCalled {
				t.Fatalf("GenerateExpandAndRecoverVolumeFunc failed: expected resize called %t, got %t", test.expectResizeCall, expansionResponse.resizeCalled)
			}
		})
	}
}

func TestOperationGenerator_nodeExpandVolume(t *testing.T) {
	getSizeFunc := func(size string) *resource.Quantity {
		x := resource.MustParse(size)
		return &x
	}

	nodeResizeFailed := v1.PersistentVolumeClaimNodeResizeInfeasible
	nodeResizePending := v1.PersistentVolumeClaimNodeResizePending
	var tests = []struct {
		name string
		pvc  *v1.PersistentVolumeClaim
		pv   *v1.PersistentVolume

		// desired size, defaults to pv.Spec.Capacity
		desiredSize *resource.Quantity
		// actualSize, defaults to pvc.Status.Capacity
		actualSize *resource.Quantity

		// expectations of test
		expectedResizeStatus v1.ClaimResourceStatus
		expectedStatusSize   resource.Quantity
		resizeCallCount      int
		expectError          bool
	}{
		{
			name: "pv.spec.cap > pvc.status.cap, resizeStatus=node_expansion_failed",
			pvc:  getTestPVC("test-vol0", "2G", "1G", "", &nodeResizeFailed),
			pv:   getTestPV("test-vol0", "2G"),

			expectedResizeStatus: v1.PersistentVolumeClaimNodeResizeInfeasible,
			resizeCallCount:      0,
			expectedStatusSize:   resource.MustParse("1G"),
		},
		{
			name:                 "pv.spec.cap > pvc.status.cap, resizeStatus=node_expansion_pending",
			pvc:                  getTestPVC("test-vol0", "2G", "1G", "2G", &nodeResizePending),
			pv:                   getTestPV("test-vol0", "2G"),
			expectedResizeStatus: "",
			resizeCallCount:      1,
			expectedStatusSize:   resource.MustParse("2G"),
		},
		{
			name:                 "pv.spec.cap > pvc.status.cap, resizeStatus=node_expansion_pending, reize_op=failing",
			pvc:                  getTestPVC(volumetesting.InfeasibleNodeExpansion, "2G", "1G", "2G", &nodeResizePending),
			pv:                   getTestPV(volumetesting.InfeasibleNodeExpansion, "2G"),
			expectError:          true,
			expectedResizeStatus: v1.PersistentVolumeClaimNodeResizeInfeasible,
			resizeCallCount:      1,
			expectedStatusSize:   resource.MustParse("1G"),
		},
		{
			name: "pv.spec.cap = pvc.status.cap, resizeStatus='', desiredSize = actualSize",
			pvc:  getTestPVC("test-vol0", "2G", "2G", "2G", nil),
			pv:   getTestPV("test-vol0", "2G"),

			expectedResizeStatus: "",
			resizeCallCount:      0,
			expectedStatusSize:   resource.MustParse("2G"),
		},
		{
			name:        "pv.spec.cap = pvc.status.cap, resizeStatus='', desiredSize > actualSize",
			pvc:         getTestPVC("test-vol0", "2G", "2G", "2G", nil),
			pv:          getTestPV("test-vol0", "2G"),
			desiredSize: getSizeFunc("2G"),
			actualSize:  getSizeFunc("1G"),

			expectedResizeStatus: "",
			resizeCallCount:      0,
			expectedStatusSize:   resource.MustParse("2G"),
		},
		{
			name:        "pv.spec.cap = pvc.status.cap, resizeStatus=node-expansion-failed, desiredSize > actualSize",
			pvc:         getTestPVC("test-vol0", "2G", "2G", "2G", &nodeResizeFailed),
			pv:          getTestPV("test-vol0", "2G"),
			desiredSize: getSizeFunc("2G"),
			actualSize:  getSizeFunc("1G"),

			expectedResizeStatus: v1.PersistentVolumeClaimNodeResizeInfeasible,
			resizeCallCount:      0,
			expectedStatusSize:   resource.MustParse("2G"),
		},
	}
	for i := range tests {
		test := tests[i]
		t.Run(test.name, func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.RecoverVolumeExpansionFailure, true)
			volumePluginMgr, fakePlugin := volumetesting.GetTestKubeletVolumePluginMgr(t)
			test.pv.Spec.ClaimRef = &v1.ObjectReference{
				Namespace: test.pvc.Namespace,
				Name:      test.pvc.Name,
			}

			pvc := test.pvc
			pv := test.pv
			pod := getTestPod("test-pod", pvc.Name)
			og := getTestOperatorGeneratorWithPVPVC(volumePluginMgr, pvc, pv)
			vmt := VolumeToMount{
				Pod:        pod,
				VolumeName: v1.UniqueVolumeName(pv.Name),
				VolumeSpec: volume.NewSpecFromPersistentVolume(pv, false),
			}
			desiredSize := test.desiredSize
			if desiredSize == nil {
				desiredSize = pv.Spec.Capacity.Storage()
			}
			actualSize := test.actualSize
			if actualSize == nil {
				actualSize = pvc.Status.Capacity.Storage()
			}
			pluginResizeOpts := volume.NodeResizeOptions{
				VolumeSpec: vmt.VolumeSpec,
				NewSize:    *desiredSize,
				OldSize:    *actualSize,
			}
			asow := newFakeActualStateOfWorld()

			ogInstance, _ := og.(*operationGenerator)
			_, _, err := ogInstance.nodeExpandVolume(vmt, asow, pluginResizeOpts)

			if !test.expectError && err != nil {
				t.Errorf("For test %s, expected no error got: %v", test.name, err)
			}
			if test.expectError && err == nil {
				t.Errorf("For test %s, expected error but got none", test.name)
			}
			if test.resizeCallCount != fakePlugin.NodeExpandCallCount {
				t.Errorf("for test %s, expected node-expand call count to be %d, got %d", test.name, test.resizeCallCount, fakePlugin.NodeExpandCallCount)
			}
		})
	}
}

func TestExpandDuringMount(t *testing.T) {
	nodeResizePending := v1.PersistentVolumeClaimNodeResizePending
	var tests = []struct {
		name string
		pvc  *v1.PersistentVolumeClaim
		pv   *v1.PersistentVolume

		// desired size, defaults to pv.Spec.Capacity
		desiredSize *resource.Quantity
		// actualSize, defaults to pvc.Status.Capacity
		actualSize *resource.Quantity

		// expectations of test
		expectedResizeStatus v1.ClaimResourceStatus
		expectedStatusSize   resource.Quantity
		resizeCallCount      int
		expectError          bool
	}{
		{
			name:                 "pv.spec.cap > pvc.status.cap, resizeStatus=node_expansion_pending",
			pvc:                  getTestPVC("test-vol0", "2G", "1G", "2G", &nodeResizePending),
			pv:                   getTestPV("test-vol0", "2G"),
			expectedResizeStatus: "",
			resizeCallCount:      1,
			expectedStatusSize:   resource.MustParse("2G"),
		},
	}
	for i := range tests {
		test := tests[i]
		t.Run(test.name, func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.RecoverVolumeExpansionFailure, true)
			volumePluginMgr, fakePlugin := volumetesting.GetTestKubeletVolumePluginMgr(t)
			test.pv.Spec.ClaimRef = &v1.ObjectReference{
				Namespace: test.pvc.Namespace,
				Name:      test.pvc.Name,
			}

			pvc := test.pvc
			pv := test.pv
			pod := getTestPod("test-pod", pvc.Name)
			og := getTestOperatorGeneratorWithPVPVC(volumePluginMgr, pvc, pv)
			vmt := VolumeToMount{
				Pod:        pod,
				VolumeName: v1.UniqueVolumeName(pv.Name),
				VolumeSpec: volume.NewSpecFromPersistentVolume(pv, false),
			}
			desiredSize := test.desiredSize
			if desiredSize == nil {
				desiredSize = pv.Spec.Capacity.Storage()
			}
			actualSize := test.actualSize
			if actualSize == nil {
				actualSize = pvc.Status.Capacity.Storage()
			}
			pluginResizeOpts := volume.NodeResizeOptions{
				NewSize: *desiredSize,
				OldSize: *actualSize,
			}
			asow := newFakeActualStateOfWorld()

			ogInstance, _ := og.(*operationGenerator)
			_, err := ogInstance.expandVolumeDuringMount(vmt, asow, pluginResizeOpts)

			if !test.expectError && err != nil {
				t.Errorf("For test %s, expected no error got: %v", test.name, err)
			}
			if test.expectError && err == nil {
				t.Errorf("For test %s, expected error but got none", test.name)
			}
			if test.resizeCallCount != fakePlugin.NodeExpandCallCount {
				t.Errorf("for test %s, expected node-expand call count to be %d, got %d", test.name, test.resizeCallCount, fakePlugin.NodeExpandCallCount)
			}

			if test.resizeCallCount > 0 {
				resizeOptions := fakePlugin.LastResizeOptions
				if resizeOptions.VolumeSpec == nil {
					t.Errorf("for test %s, expected volume spec to be set", test.name)
				}
			}
		})
	}
}

func getTestPod(podName, pvcName string) *v1.Pod {
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			UID:       "test-pod-uid",
			Namespace: "ns",
		},
		Spec: v1.PodSpec{
			Volumes: []v1.Volume{
				{
					Name: pvcName,
					VolumeSource: v1.VolumeSource{
						PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{
							ClaimName: pvcName,
						},
					},
				},
			},
		},
	}
}

func getTestPVC(volumeName string, specSize, statusSize, allocatedSize string, resizeStatus *v1.ClaimResourceStatus) *v1.PersistentVolumeClaim {
	pvc := &v1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "claim01",
			Namespace: "ns",
			UID:       "test-uid",
		},
		Spec: v1.PersistentVolumeClaimSpec{
			AccessModes: []v1.PersistentVolumeAccessMode{v1.ReadWriteOnce},
			Resources:   v1.VolumeResourceRequirements{Requests: v1.ResourceList{v1.ResourceStorage: resource.MustParse(specSize)}},
			VolumeName:  volumeName,
		},
		Status: v1.PersistentVolumeClaimStatus{
			Phase: v1.ClaimBound,
		},
	}
	if len(statusSize) > 0 {
		pvc.Status.Capacity = v1.ResourceList{v1.ResourceStorage: resource.MustParse(statusSize)}
	}
	if len(allocatedSize) > 0 {
		pvc.Status.AllocatedResources = v1.ResourceList{v1.ResourceStorage: resource.MustParse(allocatedSize)}
	}
	if resizeStatus != nil {
		pvc.Status.AllocatedResourceStatuses = map[v1.ResourceName]v1.ClaimResourceStatus{
			v1.ResourceStorage: *resizeStatus,
		}
	}
	return pvc
}

func addAccessMode(pvc *v1.PersistentVolumeClaim, mode v1.PersistentVolumeAccessMode) *v1.PersistentVolumeClaim {
	pvc.Spec.AccessModes = append(pvc.Spec.AccessModes, mode)
	return pvc
}

func getTestPV(volumeName string, specSize string) *v1.PersistentVolume {
	return &v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: volumeName,
			UID:  "test-uid",
		},
		Spec: v1.PersistentVolumeSpec{
			AccessModes: []v1.PersistentVolumeAccessMode{v1.ReadWriteOnce},
			Capacity: v1.ResourceList{
				v1.ResourceStorage: resource.MustParse(specSize),
			},
		},
		Status: v1.PersistentVolumeStatus{
			Phase: v1.VolumeBound,
		},
	}
}

func getTestOperationGenerator(volumePluginMgr *volume.VolumePluginMgr, objects ...runtime.Object) OperationGenerator {
	fakeKubeClient := fakeclient.NewSimpleClientset(objects...)
	fakeRecorder := &events.FakeRecorder{}
	fakeHandler := volumetesting.NewBlockVolumePathHandler()
	operationGenerator := NewOperationGenerator(
		fakeKubeClient,
		volumePluginMgr,
		fakeRecorder,
		fakeHandler)
	return operationGenerator
}

func getTestOperatorGeneratorWithPVPVC(volumePluginMgr *volume.VolumePluginMgr, pvc *v1.PersistentVolumeClaim, pv *v1.PersistentVolume) OperationGenerator {
	fakeKubeClient := fakeclient.NewSimpleClientset(pvc, pv)
	fakeKubeClient.AddReactor("get", "persistentvolumeclaims", func(action core.Action) (bool, runtime.Object, error) {
		return true, pvc, nil
	})
	fakeKubeClient.AddReactor("get", "persistentvolumes", func(action core.Action) (bool, runtime.Object, error) {
		return true, pv, nil
	})
	fakeKubeClient.AddReactor("patch", "persistentvolumeclaims", func(action core.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() == "status" {
			return true, pvc, nil
		}
		return true, nil, fmt.Errorf("no reaction implemented for %s", action)
	})

	fakeRecorder := &events.FakeRecorder{}
	fakeHandler := volumetesting.NewBlockVolumePathHandler()
	operationGenerator := NewOperationGenerator(
		fakeKubeClient,
		volumePluginMgr,
		fakeRecorder,
		fakeHandler)
	return operationGenerator
}

func getTestVolumeToUnmount(pod *v1.Pod, pvSpec v1.PersistentVolumeSpec, pluginName string) MountedVolume {
	volumeSpec := &volume.Spec{
		PersistentVolume: &v1.PersistentVolume{
			Spec: pvSpec,
		},
	}
	volumeToUnmount := MountedVolume{
		VolumeName: v1.UniqueVolumeName("pd-volume"),
		PodUID:     pod.UID,
		PluginName: pluginName,
		VolumeSpec: volumeSpec,
	}
	return volumeToUnmount
}

func initTestPlugins(t *testing.T, plugs []volume.VolumePlugin, pluginName string) (*volume.VolumePluginMgr, string) {
	client := fakeclient.NewSimpleClientset()
	pluginMgr, _, tmpDir := csitesting.NewTestPlugin(t, client)

	err := pluginMgr.InitPlugins(plugs, nil, pluginMgr.Host)
	if err != nil {
		t.Fatalf("Can't init volume plugins: %v", err)
	}

	_, e := pluginMgr.FindPluginByName(pluginName)
	if e != nil {
		t.Fatalf("Can't find the plugin by name: %s", pluginName)
	}

	return pluginMgr, tmpDir
}
