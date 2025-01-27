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

package csi

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	api "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	fakeclient "k8s.io/client-go/kubernetes/fake"
	"k8s.io/kubernetes/pkg/volume"
	volumetypes "k8s.io/kubernetes/pkg/volume/util/types"
)

func prepareBlockMapperTest(plug *csiPlugin, specVolumeName string, t *testing.T) (*csiBlockMapper, *volume.Spec, *api.PersistentVolume, error) {
	registerFakePlugin(testDriver, "endpoint", []string{"1.0.0"}, t)
	pv := makeTestPV(specVolumeName, 10, testDriver, testVol)
	spec := volume.NewSpecFromPersistentVolume(pv, pv.Spec.PersistentVolumeSource.CSI.ReadOnly)
	mapper, err := plug.NewBlockVolumeMapper(
		spec,
		&api.Pod{ObjectMeta: metav1.ObjectMeta{UID: testPodUID, Namespace: testns, Name: testPod}},
	)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to make a new Mapper: %w", err)
	}
	csiMapper := mapper.(*csiBlockMapper)
	return csiMapper, spec, pv, nil
}

func TestBlockMapperGetGlobalMapPath(t *testing.T) {
	plug, tmpDir := newTestPlugin(t, nil)
	defer os.RemoveAll(tmpDir)

	// TODO (vladimirvivien) specName with slashes will not work
	testCases := []struct {
		name           string
		specVolumeName string
		path           string
	}{
		{
			name:           "simple specName",
			specVolumeName: "spec-0",
			path:           filepath.Join(tmpDir, fmt.Sprintf("plugins/kubernetes.io/csi/volumeDevices/%s/%s", "spec-0", "dev")),
		},
		{
			name:           "specName with dots",
			specVolumeName: "test.spec.1",
			path:           filepath.Join(tmpDir, fmt.Sprintf("plugins/kubernetes.io/csi/volumeDevices/%s/%s", "test.spec.1", "dev")),
		},
	}
	for _, tc := range testCases {
		t.Logf("test case: %s", tc.name)
		csiMapper, spec, _, err := prepareBlockMapperTest(plug, tc.specVolumeName, t)
		if err != nil {
			t.Fatalf("Failed to make a new Mapper: %v", err)
		}

		path, err := csiMapper.GetGlobalMapPath(spec)
		if err != nil {
			t.Errorf("mapper GetGlobalMapPath failed: %v", err)
		}

		if tc.path != path {
			t.Errorf("expecting path %s, got %s", tc.path, path)
		}
	}
}

func TestBlockMapperGetStagingPath(t *testing.T) {
	plug, tmpDir := newTestPlugin(t, nil)
	defer os.RemoveAll(tmpDir)

	testCases := []struct {
		name           string
		specVolumeName string
		path           string
	}{
		{
			name:           "simple specName",
			specVolumeName: "spec-0",
			path:           filepath.Join(tmpDir, fmt.Sprintf("plugins/kubernetes.io/csi/volumeDevices/staging/%s", "spec-0")),
		},
		{
			name:           "specName with dots",
			specVolumeName: "test.spec.1",
			path:           filepath.Join(tmpDir, fmt.Sprintf("plugins/kubernetes.io/csi/volumeDevices/staging/%s", "test.spec.1")),
		},
	}
	for _, tc := range testCases {
		t.Logf("test case: %s", tc.name)
		csiMapper, _, _, err := prepareBlockMapperTest(plug, tc.specVolumeName, t)
		if err != nil {
			t.Fatalf("Failed to make a new Mapper: %v", err)
		}

		path := csiMapper.GetStagingPath()

		if tc.path != path {
			t.Errorf("expecting path %s, got %s", tc.path, path)
		}
	}
}

func TestBlockMapperGetPublishPath(t *testing.T) {
	plug, tmpDir := newTestPlugin(t, nil)
	defer os.RemoveAll(tmpDir)

	testCases := []struct {
		name           string
		specVolumeName string
		path           string
	}{
		{
			name:           "simple specName",
			specVolumeName: "spec-0",
			path:           filepath.Join(tmpDir, fmt.Sprintf("plugins/kubernetes.io/csi/volumeDevices/publish/%s/%s", "spec-0", testPodUID)),
		},
		{
			name:           "specName with dots",
			specVolumeName: "test.spec.1",
			path:           filepath.Join(tmpDir, fmt.Sprintf("plugins/kubernetes.io/csi/volumeDevices/publish/%s/%s", "test.spec.1", testPodUID)),
		},
	}
	for _, tc := range testCases {
		t.Logf("test case: %s", tc.name)
		csiMapper, _, _, err := prepareBlockMapperTest(plug, tc.specVolumeName, t)
		if err != nil {
			t.Fatalf("Failed to make a new Mapper: %v", err)
		}

		path := csiMapper.getPublishPath()

		if tc.path != path {
			t.Errorf("expecting path %s, got %s", tc.path, path)
		}
	}
}

func TestBlockMapperGetDeviceMapPath(t *testing.T) {
	plug, tmpDir := newTestPlugin(t, nil)
	defer os.RemoveAll(tmpDir)

	testCases := []struct {
		name           string
		specVolumeName string
		path           string
	}{
		{
			name:           "simple specName",
			specVolumeName: "spec-0",
			path:           filepath.Join(tmpDir, fmt.Sprintf("pods/%s/volumeDevices/kubernetes.io~csi", testPodUID)),
		},
		{
			name:           "specName with dots",
			specVolumeName: "test.spec.1",
			path:           filepath.Join(tmpDir, fmt.Sprintf("pods/%s/volumeDevices/kubernetes.io~csi", testPodUID)),
		},
	}
	for _, tc := range testCases {
		t.Logf("test case: %s", tc.name)
		csiMapper, _, _, err := prepareBlockMapperTest(plug, tc.specVolumeName, t)
		if err != nil {
			t.Fatalf("Failed to make a new Mapper: %v", err)
		}

		path, volName := csiMapper.GetPodDeviceMapPath()

		if tc.path != path {
			t.Errorf("expecting path %s, got %s", tc.path, path)
		}

		if tc.specVolumeName != volName {
			t.Errorf("expecting volName %s, got %s", tc.specVolumeName, volName)
		}
	}
}

func TestBlockMapperSetupDevice(t *testing.T) {
	plug, tmpDir := newTestPlugin(t, nil)
	defer os.RemoveAll(tmpDir)

	csiMapper, _, pv, err := prepareBlockMapperTest(plug, "test-pv", t)
	if err != nil {
		t.Fatalf("Failed to make a new Mapper: %v", err)
	}

	pvName := pv.GetName()
	nodeName := string(plug.host.GetNodeName())

	csiMapper.csiClient = setupClient(t, true)

	attachID := getAttachmentName(csiMapper.volumeID, string(csiMapper.driverName), string(nodeName))
	attachment := makeTestAttachment(attachID, nodeName, pvName)
	attachment.Status.Attached = true
	_, err = csiMapper.k8s.StorageV1().VolumeAttachments().Create(context.TODO(), attachment, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("failed to setup VolumeAttachment: %v", err)
	}
	t.Log("created attachment ", attachID)

	stagingPath, err := csiMapper.SetUpDevice()
	if err != nil {
		t.Fatalf("mapper failed to SetupDevice: %v", err)
	}

	// Check if NodeStageVolume staged to the right path
	svols := csiMapper.csiClient.(*fakeCsiDriverClient).nodeClient.GetNodeStagedVolumes()
	svol, ok := svols[csiMapper.volumeID]
	if !ok {
		t.Error("csi server may not have received NodeStageVolume call")
	}
	if svol.Path != stagingPath {
		t.Errorf("csi server expected device path %s, got %s", stagingPath, svol.Path)
	}
}

func TestBlockMapperSetupDeviceError(t *testing.T) {
	plug, tmpDir := newTestPlugin(t, nil)
	defer os.RemoveAll(tmpDir)

	csiMapper, _, pv, err := prepareBlockMapperTest(plug, "test-pv", t)
	if err != nil {
		t.Fatalf("Failed to make a new Mapper: %v", err)
	}

	pvName := pv.GetName()
	nodeName := string(plug.host.GetNodeName())

	csiMapper.csiClient = setupClient(t, true)
	fClient := csiMapper.csiClient.(*fakeCsiDriverClient)
	fClient.nodeClient.SetNextError(status.Error(codes.InvalidArgument, "mock final error"))

	attachID := getAttachmentName(csiMapper.volumeID, string(csiMapper.driverName), string(nodeName))
	attachment := makeTestAttachment(attachID, nodeName, pvName)
	attachment.Status.Attached = true
	_, err = csiMapper.k8s.StorageV1().VolumeAttachments().Create(context.Background(), attachment, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("failed to setup VolumeAttachment: %v", err)
	}
	t.Log("created attachment ", attachID)

	stagingPath, err := csiMapper.SetUpDevice()
	if err == nil {
		t.Fatal("mapper unexpectedly succeeded")
	}

	// Check that all directories have been cleaned
	// Check that all metadata / staging / publish directories were deleted
	dataDir := getVolumeDeviceDataDir(pv.ObjectMeta.Name, plug.host)
	if _, err := os.Stat(dataDir); err == nil {
		t.Errorf("volume publish data directory %s was not deleted", dataDir)
	}
	devDir := getVolumeDeviceDataDir(pv.ObjectMeta.Name, plug.host)
	if _, err := os.Stat(devDir); err == nil {
		t.Errorf("volume publish device directory %s was not deleted", devDir)
	}

	if _, err := os.Stat(stagingPath); err == nil {
		t.Errorf("volume staging path %s was not deleted", stagingPath)
	}
}

func TestBlockMapperSetupDeviceNoClientError(t *testing.T) {
	transientError := volumetypes.NewTransientOperationFailure("")
	plug, tmpDir := newTestPlugin(t, nil)
	defer os.RemoveAll(tmpDir)

	csiMapper, _, pv, err := prepareBlockMapperTest(plug, "test-pv", t)
	if err != nil {
		t.Fatalf("Failed to make a new Mapper: %v", err)
	}

	pvName := pv.GetName()
	nodeName := string(plug.host.GetNodeName())

	csiMapper.csiClient = setupClient(t, true)

	attachID := getAttachmentName(csiMapper.volumeID, string(csiMapper.driverName), string(nodeName))
	attachment := makeTestAttachment(attachID, nodeName, pvName)
	attachment.Status.Attached = true
	_, err = csiMapper.k8s.StorageV1().VolumeAttachments().Create(context.TODO(), attachment, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("failed to setup VolumeAttachment: %v", err)
	}
	t.Log("created attachment ", attachID)

	// Clear out the clients
	// The lookup to generate a new client will fail when it tries to query a driver with an unknown name
	csiMapper.csiClient = nil
	csiMapper.csiClientGetter.csiClient = nil
	// Note that prepareBlockMapperTest above will create a driver with a name of "test-driver"
	csiMapper.csiClientGetter.driverName = "unknown-driver"

	_, err = csiMapper.SetUpDevice()
	if err == nil {
		t.Errorf("test should fail, but no error occurred")
	} else if reflect.TypeOf(transientError) != reflect.TypeOf(err) {
		t.Fatalf("expected exitError type: %v got: %v (%v)", reflect.TypeOf(transientError), reflect.TypeOf(err), err)
	}
}

func TestBlockMapperMapPodDevice(t *testing.T) {
	plug, tmpDir := newTestPlugin(t, nil)
	defer os.RemoveAll(tmpDir)

	csiMapper, _, pv, err := prepareBlockMapperTest(plug, "test-pv", t)
	if err != nil {
		t.Fatalf("Failed to make a new Mapper: %v", err)
	}

	pvName := pv.GetName()
	nodeName := string(plug.host.GetNodeName())

	csiMapper.csiClient = setupClient(t, true)

	attachID := getAttachmentName(csiMapper.volumeID, string(csiMapper.driverName), nodeName)
	attachment := makeTestAttachment(attachID, nodeName, pvName)
	attachment.Status.Attached = true
	_, err = csiMapper.k8s.StorageV1().VolumeAttachments().Create(context.Background(), attachment, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("failed to setup VolumeAttachment: %v", err)
	}
	t.Log("created attachment ", attachID)

	// Map device to global and pod device map path
	path, err := csiMapper.MapPodDevice()
	if err != nil {
		t.Fatalf("mapper failed to GetGlobalMapPath: %v", err)
	}

	// Check if NodePublishVolume published to the right path
	pvols := csiMapper.csiClient.(*fakeCsiDriverClient).nodeClient.GetNodePublishedVolumes()
	pvol, ok := pvols[csiMapper.volumeID]
	if !ok {
		t.Error("csi server may not have received NodePublishVolume call")
	}

	publishPath := csiMapper.getPublishPath()
	if pvol.Path != publishPath {
		t.Errorf("csi server expected path %s, got %s", publishPath, pvol.Path)
	}
	if path != publishPath {
		t.Errorf("csi server expected path %s, but MapPodDevice returned %s", publishPath, path)
	}
}

func TestBlockMapperMapPodDeviceNotSupportAttach(t *testing.T) {
	fakeClient := fakeclient.NewSimpleClientset()
	attachRequired := false
	fakeDriver := &storagev1.CSIDriver{
		ObjectMeta: metav1.ObjectMeta{
			Name: testDriver,
		},
		Spec: storagev1.CSIDriverSpec{
			AttachRequired: &attachRequired,
		},
	}
	_, err := fakeClient.StorageV1().CSIDrivers().Create(context.TODO(), fakeDriver, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Failed to create a fakeDriver: %v", err)
	}

	// after the driver is created, create the plugin. newTestPlugin waits for the informer to sync,
	// such that csiMapper.SetUpDevice below sees the VolumeAttachment object in the lister.

	plug, tmpDir := newTestPlugin(t, fakeClient)
	defer os.RemoveAll(tmpDir)

	csiMapper, _, _, err := prepareBlockMapperTest(plug, "test-pv", t)
	if err != nil {
		t.Fatalf("Failed to make a new Mapper: %v", err)
	}
	csiMapper.csiClient = setupClient(t, true)

	// Map device to global and pod device map path
	path, err := csiMapper.MapPodDevice()
	if err != nil {
		t.Fatalf("mapper failed to GetGlobalMapPath: %v", err)
	}
	publishPath := csiMapper.getPublishPath()
	if path != publishPath {
		t.Errorf("path %s and %s doesn't match", path, publishPath)
	}
}

func TestBlockMapperMapPodDeviceWithPodInfo(t *testing.T) {
	fakeClient := fakeclient.NewSimpleClientset()
	attachRequired := false
	podInfo := true
	fakeDriver := &storagev1.CSIDriver{
		ObjectMeta: metav1.ObjectMeta{
			Name: testDriver,
		},
		Spec: storagev1.CSIDriverSpec{
			PodInfoOnMount: &podInfo,
			AttachRequired: &attachRequired,
		},
	}
	_, err := fakeClient.StorageV1().CSIDrivers().Create(context.TODO(), fakeDriver, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Failed to create a fakeDriver: %v", err)
	}

	// after the driver is created, create the plugin. newTestPlugin waits for the informer to sync,
	// such that csiMapper.SetUpDevice below sees the VolumeAttachment object in the lister.

	plug, tmpDir := newTestPlugin(t, fakeClient)
	defer os.RemoveAll(tmpDir)

	csiMapper, _, _, err := prepareBlockMapperTest(plug, "test-pv", t)
	if err != nil {
		t.Fatalf("Failed to make a new Mapper: %v", err)
	}
	csiMapper.csiClient = setupClient(t, true)

	// Map device to global and pod device map path
	_, err = csiMapper.MapPodDevice()
	if err != nil {
		t.Fatalf("mapper failed to GetGlobalMapPath: %v", err)
	}
	pvols := csiMapper.csiClient.(*fakeCsiDriverClient).nodeClient.GetNodePublishedVolumes()
	pvol, ok := pvols[csiMapper.volumeID]
	if !ok {
		t.Error("csi server may not have received NodePublishVolume call")
	}

	if !reflect.DeepEqual(pvol.VolumeContext, map[string]string{"csi.storage.k8s.io/pod.uid": "test-pod", "csi.storage.k8s.io/serviceAccount.name": "", "csi.storage.k8s.io/pod.name": "test-pod", "csi.storage.k8s.io/pod.namespace": "test-ns", "csi.storage.k8s.io/ephemeral": "false"}) {
		t.Error("csi mapper check pod info failed")
	}
}

func TestBlockMapperMapPodDeviceNoClientError(t *testing.T) {
	transientError := volumetypes.NewTransientOperationFailure("")
	plug, tmpDir := newTestPlugin(t, nil)
	defer os.RemoveAll(tmpDir)

	csiMapper, _, pv, err := prepareBlockMapperTest(plug, "test-pv", t)
	if err != nil {
		t.Fatalf("Failed to make a new Mapper: %v", err)
	}

	pvName := pv.GetName()
	nodeName := string(plug.host.GetNodeName())

	csiMapper.csiClient = setupClient(t, true)

	attachID := getAttachmentName(csiMapper.volumeID, string(csiMapper.driverName), nodeName)
	attachment := makeTestAttachment(attachID, nodeName, pvName)
	attachment.Status.Attached = true
	_, err = csiMapper.k8s.StorageV1().VolumeAttachments().Create(context.Background(), attachment, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("failed to setup VolumeAttachment: %v", err)
	}
	t.Log("created attachment ", attachID)

	// Clear out the clients
	// The lookup to generate a new client will fail when it tries to query a driver with an unknown name
	csiMapper.csiClient = nil
	csiMapper.csiClientGetter.csiClient = nil
	// Note that prepareBlockMapperTest above will create a driver with a name of "test-driver"
	csiMapper.csiClientGetter.driverName = "unknown-driver"

	_, err = csiMapper.MapPodDevice()
	if err == nil {
		t.Errorf("test should fail, but no error occurred")
	} else if reflect.TypeOf(transientError) != reflect.TypeOf(err) {
		t.Fatalf("expected exitError type: %v got: %v (%v)", reflect.TypeOf(transientError), reflect.TypeOf(err), err)
	}
}

func TestBlockMapperTearDownDevice(t *testing.T) {
	plug, tmpDir := newTestPlugin(t, nil)
	defer os.RemoveAll(tmpDir)

	_, spec, pv, err := prepareBlockMapperTest(plug, "test-pv", t)
	if err != nil {
		t.Fatalf("Failed to make a new Mapper: %v", err)
	}

	// save volume data
	dir := getVolumeDeviceDataDir(pv.ObjectMeta.Name, plug.host)
	if err := os.MkdirAll(dir, 0755); err != nil && !os.IsNotExist(err) {
		t.Errorf("failed to create dir [%s]: %v", dir, err)
	}

	if err := saveVolumeData(
		dir,
		volDataFileName,
		map[string]string{
			volDataKey.specVolID:  pv.ObjectMeta.Name,
			volDataKey.driverName: testDriver,
			volDataKey.volHandle:  testVol,
		},
	); err != nil {
		t.Fatalf("failed to save volume data: %v", err)
	}

	unmapper, err := plug.NewBlockVolumeUnmapper(pv.ObjectMeta.Name, testPodUID)
	if err != nil {
		t.Fatalf("failed to make a new Unmapper: %v", err)
	}

	csiUnmapper := unmapper.(*csiBlockMapper)
	csiUnmapper.csiClient = setupClient(t, true)

	globalMapPath, err := csiUnmapper.GetGlobalMapPath(spec)
	if err != nil {
		t.Fatalf("unmapper failed to GetGlobalMapPath: %v", err)
	}

	err = csiUnmapper.TearDownDevice(globalMapPath, "/dev/test")
	if err != nil {
		t.Fatal(err)
	}

	// ensure csi client call and node unpblished
	pubs := csiUnmapper.csiClient.(*fakeCsiDriverClient).nodeClient.GetNodePublishedVolumes()
	if _, ok := pubs[csiUnmapper.volumeID]; ok {
		t.Error("csi server may not have received NodeUnpublishVolume call")
	}

	// ensure csi client call and node unstaged
	vols := csiUnmapper.csiClient.(*fakeCsiDriverClient).nodeClient.GetNodeStagedVolumes()
	if _, ok := vols[csiUnmapper.volumeID]; ok {
		t.Error("csi server may not have received NodeUnstageVolume call")
	}
}

func TestBlockMapperTearDownDeviceNoStagingPath(t *testing.T) {
	plug, tmpDir := newTestPlugin(t, nil)
	defer os.RemoveAll(tmpDir)

	_, spec, pv, err := prepareBlockMapperTest(plug, "test-pv", t)
	if err != nil {
		t.Fatalf("Failed to make a new Mapper: %v", err)
	}

	unmapper, err := plug.NewBlockVolumeUnmapper(pv.ObjectMeta.Name, testPodUID)
	if err != nil {
		t.Fatalf("failed to make a new Unmapper: %v", err)
	}

	csiUnmapper := unmapper.(*csiBlockMapper)
	csiUnmapper.csiClient = setupClient(t, true)

	// ensure staging path does not exist
	stagingPath := csiUnmapper.GetStagingPath()
	if err := os.RemoveAll(stagingPath); err != nil && !os.IsNotExist(err) {
		t.Fatalf("Failed to clean up staging path: %v", err)
	}

	globalMapPath, err := csiUnmapper.GetGlobalMapPath(spec)
	if err != nil {
		t.Fatalf("unmapper failed to GetGlobalMapPath: %v", err)
	}

	err = csiUnmapper.TearDownDevice(globalMapPath, "/dev/test")
	if err != nil {
		t.Fatal(err)
	}

	// ensure csi client call and node unpblished
	pubs := csiUnmapper.csiClient.(*fakeCsiDriverClient).nodeClient.GetNodePublishedVolumes()
	if _, ok := pubs[csiUnmapper.volumeID]; ok {
		t.Error("csi server may not have received NodeUnpublishVolume call")
	}

	// ensure csi client call and node unstaged
	vols := csiUnmapper.csiClient.(*fakeCsiDriverClient).nodeClient.GetNodeStagedVolumes()
	if _, ok := vols[csiUnmapper.volumeID]; ok {
		t.Error("csi server may not have received NodeUnstageVolume call")
	}
}

func TestBlockMapperTearDownDeviceNoClientError(t *testing.T) {
	transientError := volumetypes.NewTransientOperationFailure("")
	plug, tmpDir := newTestPlugin(t, nil)
	defer os.RemoveAll(tmpDir)

	_, spec, pv, err := prepareBlockMapperTest(plug, "test-pv", t)
	if err != nil {
		t.Fatalf("Failed to make a new Mapper: %v", err)
	}

	// save volume data
	dir := getVolumeDeviceDataDir(pv.ObjectMeta.Name, plug.host)
	if err := os.MkdirAll(dir, 0755); err != nil && !os.IsNotExist(err) {
		t.Errorf("failed to create dir [%s]: %v", dir, err)
	}

	if err := saveVolumeData(
		dir,
		volDataFileName,
		map[string]string{
			volDataKey.specVolID:  pv.ObjectMeta.Name,
			volDataKey.driverName: testDriver,
			volDataKey.volHandle:  testVol,
		},
	); err != nil {
		t.Fatalf("failed to save volume data: %v", err)
	}

	unmapper, err := plug.NewBlockVolumeUnmapper(pv.ObjectMeta.Name, testPodUID)
	if err != nil {
		t.Fatalf("failed to make a new Unmapper: %v", err)
	}

	csiUnmapper := unmapper.(*csiBlockMapper)
	csiUnmapper.csiClient = setupClient(t, true)

	globalMapPath, err := csiUnmapper.GetGlobalMapPath(spec)
	if err != nil {
		t.Fatalf("unmapper failed to GetGlobalMapPath: %v", err)
	}

	// Clear out the clients
	// The lookup to generate a new client will fail when it tries to query a driver with an unknown name
	csiUnmapper.csiClient = nil
	csiUnmapper.csiClientGetter.csiClient = nil
	// Note that prepareBlockMapperTest above will create a driver with a name of "test-driver"
	csiUnmapper.csiClientGetter.driverName = "unknown-driver"

	err = csiUnmapper.TearDownDevice(globalMapPath, "/dev/test")
	if err == nil {
		t.Errorf("test should fail, but no error occurred")
	} else if reflect.TypeOf(transientError) != reflect.TypeOf(err) {
		t.Fatalf("expected exitError type: %v got: %v (%v)", reflect.TypeOf(transientError), reflect.TypeOf(err), err)
	}
}

func TestVolumeSetupTeardown(t *testing.T) {
	// Follow volume setup + teardown sequences at top of cs_block.go and set up / clean up one CSI block device.
	// Focus on testing that there were no leftover files present after the cleanup.

	plug, tmpDir := newTestPlugin(t, nil)
	defer os.RemoveAll(tmpDir)

	csiMapper, spec, pv, err := prepareBlockMapperTest(plug, "test-pv", t)
	if err != nil {
		t.Fatalf("Failed to make a new Mapper: %v", err)
	}

	pvName := pv.GetName()
	nodeName := string(plug.host.GetNodeName())

	csiMapper.csiClient = setupClient(t, true)

	attachID := getAttachmentName(csiMapper.volumeID, string(csiMapper.driverName), string(nodeName))
	attachment := makeTestAttachment(attachID, nodeName, pvName)
	attachment.Status.Attached = true
	_, err = csiMapper.k8s.StorageV1().VolumeAttachments().Create(context.TODO(), attachment, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("failed to setup VolumeAttachment: %v", err)
	}
	t.Log("created attachment ", attachID)

	stagingPath, err := csiMapper.SetUpDevice()
	if err != nil {
		t.Fatalf("mapper failed to SetupDevice: %v", err)
	}
	// Check if NodeStageVolume staged to the right path
	svols := csiMapper.csiClient.(*fakeCsiDriverClient).nodeClient.GetNodeStagedVolumes()
	svol, ok := svols[csiMapper.volumeID]
	if !ok {
		t.Error("csi server may not have received NodeStageVolume call")
	}
	if svol.Path != stagingPath {
		t.Errorf("csi server expected device path %s, got %s", stagingPath, svol.Path)
	}

	path, err := csiMapper.MapPodDevice()
	if err != nil {
		t.Fatalf("mapper failed to GetGlobalMapPath: %v", err)
	}
	pvols := csiMapper.csiClient.(*fakeCsiDriverClient).nodeClient.GetNodePublishedVolumes()
	pvol, ok := pvols[csiMapper.volumeID]
	if !ok {
		t.Error("csi server may not have received NodePublishVolume call")
	}
	publishPath := csiMapper.getPublishPath()
	if pvol.Path != publishPath {
		t.Errorf("csi server expected path %s, got %s", publishPath, pvol.Path)
	}
	if path != publishPath {
		t.Errorf("csi server expected path %s, but MapPodDevice returned %s", publishPath, path)
	}

	unmapper, err := plug.NewBlockVolumeUnmapper(pv.ObjectMeta.Name, testPodUID)
	if err != nil {
		t.Fatalf("failed to make a new Unmapper: %v", err)
	}

	csiUnmapper := unmapper.(*csiBlockMapper)
	csiUnmapper.csiClient = csiMapper.csiClient

	globalMapPath, err := csiUnmapper.GetGlobalMapPath(spec)
	if err != nil {
		t.Fatalf("unmapper failed to GetGlobalMapPath: %v", err)
	}

	err = csiUnmapper.UnmapPodDevice()
	if err != nil {
		t.Errorf("unmapper failed to call UnmapPodDevice: %v", err)
	}

	// GenerateUnmapDeviceFunc uses "" as pod UUID, it is global operation over all pods that used the volume
	unmapper, err = plug.NewBlockVolumeUnmapper(pv.ObjectMeta.Name, "")
	if err != nil {
		t.Fatalf("failed to make a new Unmapper: %v", err)
	}
	csiUnmapper = unmapper.(*csiBlockMapper)
	csiUnmapper.csiClient = csiMapper.csiClient

	err = csiUnmapper.TearDownDevice(globalMapPath, "/dev/test")
	if err != nil {
		t.Fatal(err)
	}
	pubs := csiUnmapper.csiClient.(*fakeCsiDriverClient).nodeClient.GetNodePublishedVolumes()
	if _, ok := pubs[csiUnmapper.volumeID]; ok {
		t.Error("csi server may not have received NodeUnpublishVolume call")
	}
	vols := csiUnmapper.csiClient.(*fakeCsiDriverClient).nodeClient.GetNodeStagedVolumes()
	if _, ok := vols[csiUnmapper.volumeID]; ok {
		t.Error("csi server may not have received NodeUnstageVolume call")
	}

	// Check that all metadata / staging / publish directories were deleted
	dataDir := getVolumeDeviceDataDir(pv.ObjectMeta.Name, plug.host)
	if _, err := os.Stat(dataDir); err == nil {
		t.Errorf("volume publish data directory %s was not deleted", dataDir)
	}
	devDir := getVolumeDeviceDataDir(pv.ObjectMeta.Name, plug.host)
	if _, err := os.Stat(devDir); err == nil {
		t.Errorf("volume publish device directory %s was not deleted", devDir)
	}
	if _, err := os.Stat(publishPath); err == nil {
		t.Errorf("volume publish path %s was not deleted", publishPath)
	}
	publishDir := filepath.Dir(publishPath)
	if _, err := os.Stat(publishDir); err == nil {
		t.Errorf("volume publish parent directory %s was not deleted", publishDir)
	}
	if _, err := os.Stat(stagingPath); err == nil {
		t.Errorf("volume staging path %s was not deleted", stagingPath)
	}
}

func TestUnmapPodDeviceNoClientError(t *testing.T) {
	transientError := volumetypes.NewTransientOperationFailure("")
	plug, tmpDir := newTestPlugin(t, nil)
	defer os.RemoveAll(tmpDir)

	csiMapper, spec, pv, err := prepareBlockMapperTest(plug, "test-pv", t)
	if err != nil {
		t.Fatalf("Failed to make a new Mapper: %v", err)
	}

	pvName := pv.GetName()
	nodeName := string(plug.host.GetNodeName())

	csiMapper.csiClient = setupClient(t, true)

	attachID := getAttachmentName(csiMapper.volumeID, string(csiMapper.driverName), string(nodeName))
	attachment := makeTestAttachment(attachID, nodeName, pvName)
	attachment.Status.Attached = true
	_, err = csiMapper.k8s.StorageV1().VolumeAttachments().Create(context.TODO(), attachment, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("failed to setup VolumeAttachment: %v", err)
	}
	t.Log("created attachment ", attachID)

	stagingPath, err := csiMapper.SetUpDevice()
	if err != nil {
		t.Fatalf("mapper failed to SetupDevice: %v", err)
	}
	// Check if NodeStageVolume staged to the right path
	svols := csiMapper.csiClient.(*fakeCsiDriverClient).nodeClient.GetNodeStagedVolumes()
	svol, ok := svols[csiMapper.volumeID]
	if !ok {
		t.Error("csi server may not have received NodeStageVolume call")
	}
	if svol.Path != stagingPath {
		t.Errorf("csi server expected device path %s, got %s", stagingPath, svol.Path)
	}

	path, err := csiMapper.MapPodDevice()
	if err != nil {
		t.Fatalf("mapper failed to GetGlobalMapPath: %v", err)
	}
	pvols := csiMapper.csiClient.(*fakeCsiDriverClient).nodeClient.GetNodePublishedVolumes()
	pvol, ok := pvols[csiMapper.volumeID]
	if !ok {
		t.Error("csi server may not have received NodePublishVolume call")
	}
	publishPath := csiMapper.getPublishPath()
	if pvol.Path != publishPath {
		t.Errorf("csi server expected path %s, got %s", publishPath, pvol.Path)
	}
	if path != publishPath {
		t.Errorf("csi server expected path %s, but MapPodDevice returned %s", publishPath, path)
	}

	unmapper, err := plug.NewBlockVolumeUnmapper(pv.ObjectMeta.Name, testPodUID)
	if err != nil {
		t.Fatalf("failed to make a new Unmapper: %v", err)
	}

	csiUnmapper := unmapper.(*csiBlockMapper)
	csiUnmapper.csiClient = csiMapper.csiClient

	_, err = csiUnmapper.GetGlobalMapPath(spec)
	if err != nil {
		t.Fatalf("unmapper failed to GetGlobalMapPath: %v", err)
	}

	// Clear out the clients
	// The lookup to generate a new client will fail when it tries to query a driver with an unknown name
	csiUnmapper.csiClient = nil
	csiUnmapper.csiClientGetter.csiClient = nil
	// Note that prepareBlockMapperTest above will create a driver with a name of "test-driver"
	csiUnmapper.csiClientGetter.driverName = "unknown-driver"

	err = csiUnmapper.UnmapPodDevice()
	if err == nil {
		t.Errorf("test should fail, but no error occurred")
	} else if reflect.TypeOf(transientError) != reflect.TypeOf(err) {
		t.Fatalf("expected exitError type: %v got: %v (%v)", reflect.TypeOf(transientError), reflect.TypeOf(err), err)
	}
}
