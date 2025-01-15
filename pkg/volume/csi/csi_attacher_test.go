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

package csi

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	storage "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	clientset "k8s.io/client-go/kubernetes"
	fakeclient "k8s.io/client-go/kubernetes/fake"
	core "k8s.io/client-go/testing"
	filesystem "k8s.io/kubernetes/pkg/util/filesystem"
	"k8s.io/kubernetes/pkg/volume"
	fakecsi "k8s.io/kubernetes/pkg/volume/csi/fake"
	volumetypes "k8s.io/kubernetes/pkg/volume/util/types"
)

const (
	testWatchTimeout     = 10 * time.Second
	testWatchFailTimeout = 2 * time.Second
)

var (
	bFalse = false
	bTrue  = true
)

func makeTestAttachment(attachID, nodeName, pvName string) *storage.VolumeAttachment {
	return &storage.VolumeAttachment{
		ObjectMeta: metav1.ObjectMeta{
			Name: attachID,
		},
		Spec: storage.VolumeAttachmentSpec{
			NodeName: nodeName,
			Attacher: "mock",
			Source: storage.VolumeAttachmentSource{
				PersistentVolumeName: &pvName,
			},
		},
		Status: storage.VolumeAttachmentStatus{
			Attached:    false,
			AttachError: nil,
			DetachError: nil,
		},
	}
}

func markVolumeAttached(t *testing.T, client clientset.Interface, watch *watch.RaceFreeFakeWatcher, attachID string, status storage.VolumeAttachmentStatus) {
	ticker := time.NewTicker(10 * time.Millisecond)
	var attach *storage.VolumeAttachment
	var err error
	defer ticker.Stop()
	// wait for attachment to be saved
	for i := 0; i < 100; i++ {
		attach, err = client.StorageV1().VolumeAttachments().Get(context.TODO(), attachID, metav1.GetOptions{})
		if err != nil {
			attach = nil
			if apierrors.IsNotFound(err) {
				<-ticker.C
				continue
			}
			t.Error(err)
		}
		if attach != nil {
			t.Logf("attachment found on try %d, stopping wait...", i)
			break
		}
	}
	t.Logf("stopped waiting for attachment")

	if attach == nil {
		t.Logf("attachment not found for id:%v", attachID)
	} else {
		attach.Status = status
		t.Logf("updating attachment %s with attach status %v", attachID, status)
		_, err := client.StorageV1().VolumeAttachments().Update(context.TODO(), attach, metav1.UpdateOptions{})
		if err != nil {
			t.Error(err)
		}
		if watch != nil {
			watch.Modify(attach)
		}
	}
}

func TestAttacherAttach(t *testing.T) {
	testCases := []struct {
		name                string
		nodeName            string
		driverName          string
		volumeName          string
		attachID            string
		spec                *volume.Spec
		injectAttacherError bool
		shouldFail          bool
		watchTimeout        time.Duration
	}{
		{
			name:       "test ok 1",
			nodeName:   "testnode-01",
			driverName: "testdriver-01",
			volumeName: "testvol-01",
			attachID:   getAttachmentName("testvol-01", "testdriver-01", "testnode-01"),
			spec:       volume.NewSpecFromPersistentVolume(makeTestPV("pv01", 10, "testdriver-01", "testvol-01"), false),
		},
		{
			name:       "test ok 2",
			nodeName:   "node02",
			driverName: "driver02",
			volumeName: "vol02",
			attachID:   getAttachmentName("vol02", "driver02", "node02"),
			spec:       volume.NewSpecFromPersistentVolume(makeTestPV("pv01", 10, "driver02", "vol02"), false),
		},
		{
			name:         "mismatch vol",
			nodeName:     "node02",
			driverName:   "driver02",
			volumeName:   "vol01",
			attachID:     getAttachmentName("vol02", "driver02", "node02"),
			spec:         volume.NewSpecFromPersistentVolume(makeTestPV("pv01", 10, "driver02", "vol01"), false),
			shouldFail:   true,
			watchTimeout: testWatchFailTimeout,
		},
		{
			name:         "mismatch driver",
			nodeName:     "node02",
			driverName:   "driver000",
			volumeName:   "vol02",
			attachID:     getAttachmentName("vol02", "driver02", "node02"),
			spec:         volume.NewSpecFromPersistentVolume(makeTestPV("pv01", 10, "driver01", "vol02"), false),
			shouldFail:   true,
			watchTimeout: testWatchFailTimeout,
		},
		{
			name:         "mismatch node",
			nodeName:     "node000",
			driverName:   "driver000",
			volumeName:   "vol02",
			attachID:     getAttachmentName("vol02", "driver02", "node02"),
			spec:         volume.NewSpecFromPersistentVolume(makeTestPV("pv01", 10, "driver02", "vol02"), false),
			shouldFail:   true,
			watchTimeout: testWatchFailTimeout,
		},
		{
			name:                "attacher error",
			nodeName:            "node02",
			driverName:          "driver02",
			volumeName:          "vol02",
			attachID:            getAttachmentName("vol02", "driver02", "node02"),
			spec:                volume.NewSpecFromPersistentVolume(makeTestPV("pv01", 10, "driver02", "vol02"), false),
			injectAttacherError: true,
			shouldFail:          true,
		},
		{
			name:       "test with volume source",
			nodeName:   "node000",
			driverName: "driver000",
			volumeName: "vol02",
			attachID:   getAttachmentName("vol02", "driver02", "node02"),
			spec:       volume.NewSpecFromVolume(makeTestVol("pv01", "driver02")),
			shouldFail: true, // csi not enabled
		},
		{
			name:       "missing spec",
			nodeName:   "node000",
			driverName: "driver000",
			volumeName: "vol02",
			attachID:   getAttachmentName("vol02", "driver02", "node02"),
			shouldFail: true, // csi not enabled
		},
	}

	// attacher loop
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Logf("test case: %s", tc.name)
			fakeClient := fakeclient.NewSimpleClientset()
			plug, tmpDir := newTestPluginWithAttachDetachVolumeHost(t, fakeClient)
			defer os.RemoveAll(tmpDir)

			attacher, err := plug.NewAttacher()
			if err != nil {
				t.Fatalf("failed to create new attacher: %v", err)
			}

			csiAttacher := getCsiAttacherFromVolumeAttacher(attacher, tc.watchTimeout)

			var wg sync.WaitGroup
			wg.Add(1)
			go func(spec *volume.Spec, nodename string, fail bool) {
				defer wg.Done()
				attachID, err := csiAttacher.Attach(spec, types.NodeName(nodename))
				if !fail && err != nil {
					t.Errorf("expecting no failure, but got err: %v", err)
				}
				if fail && err == nil {
					t.Errorf("expecting failure, but got no err")
				}
				if attachID != "" {
					t.Errorf("expecting empty attachID, got %v", attachID)
				}
			}(tc.spec, tc.nodeName, tc.shouldFail)

			var status storage.VolumeAttachmentStatus
			if tc.injectAttacherError {
				status.Attached = false
				status.AttachError = &storage.VolumeError{
					Message: "attacher error",
				}
			} else {
				status.Attached = true
			}
			markVolumeAttached(t, csiAttacher.k8s, nil, tc.attachID, status)
			wg.Wait()
		})
	}
}

func TestAttacherAttachWithInline(t *testing.T) {
	testCases := []struct {
		name                string
		nodeName            string
		driverName          string
		volumeName          string
		attachID            string
		spec                *volume.Spec
		injectAttacherError bool
		shouldFail          bool
		watchTimeout        time.Duration
	}{
		{
			name:     "test ok 1 with PV",
			nodeName: "node01",
			attachID: getAttachmentName("vol01", "driver01", "node01"),
			spec:     volume.NewSpecFromPersistentVolume(makeTestPV("pv01", 10, "driver01", "vol01"), false),
		},
		{
			name:       "test failure, attach with volSrc",
			nodeName:   "node01",
			attachID:   getAttachmentName("vol01", "driver01", "node01"),
			spec:       volume.NewSpecFromVolume(makeTestVol("vol01", "driver01")),
			shouldFail: true,
		},
		{
			name:                "attacher error",
			nodeName:            "node02",
			attachID:            getAttachmentName("vol02", "driver02", "node02"),
			spec:                volume.NewSpecFromPersistentVolume(makeTestPV("pv02", 10, "driver02", "vol02"), false),
			injectAttacherError: true,
			shouldFail:          true,
		},
		{
			name:       "missing spec",
			nodeName:   "node02",
			attachID:   getAttachmentName("vol02", "driver02", "node02"),
			shouldFail: true,
		},
	}

	// attacher loop
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Logf("test case: %s", tc.name)
			fakeClient := fakeclient.NewSimpleClientset()
			plug, tmpDir := newTestPluginWithAttachDetachVolumeHost(t, fakeClient)
			defer os.RemoveAll(tmpDir)

			attacher, err := plug.NewAttacher()
			if err != nil {
				t.Fatalf("failed to create new attacher: %v", err)
			}
			csiAttacher := getCsiAttacherFromVolumeAttacher(attacher, tc.watchTimeout)

			var wg sync.WaitGroup
			wg.Add(1)
			go func(spec *volume.Spec, nodename string, fail bool) {
				defer wg.Done()
				attachID, err := csiAttacher.Attach(spec, types.NodeName(nodename))
				if fail != (err != nil) {
					t.Errorf("expecting no failure, but got err: %v", err)
				}
				if attachID != "" {
					t.Errorf("expecting empty attachID, got %v", attachID)
				}
			}(tc.spec, tc.nodeName, tc.shouldFail)

			var status storage.VolumeAttachmentStatus
			if tc.injectAttacherError {
				status.Attached = false
				status.AttachError = &storage.VolumeError{
					Message: "attacher error",
				}
			} else {
				status.Attached = true
			}
			markVolumeAttached(t, csiAttacher.k8s, nil, tc.attachID, status)
			wg.Wait()
		})
	}
}

func TestAttacherWithCSIDriver(t *testing.T) {
	tests := []struct {
		name                   string
		driver                 string
		expectVolumeAttachment bool
		watchTimeout           time.Duration
	}{
		{
			name:                   "CSIDriver not attachable",
			driver:                 "not-attachable",
			expectVolumeAttachment: false,
		},
		{
			name:                   "CSIDriver is attachable",
			driver:                 "attachable",
			expectVolumeAttachment: true,
		},
		{
			name:                   "CSIDriver.AttachRequired not set  -> failure",
			driver:                 "nil",
			expectVolumeAttachment: true,
		},
		{
			name:                   "CSIDriver does not exist not set  -> failure",
			driver:                 "unknown",
			expectVolumeAttachment: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fakeClient := fakeclient.NewSimpleClientset(
				getTestCSIDriver("not-attachable", nil, &bFalse, nil),
				getTestCSIDriver("attachable", nil, &bTrue, nil),
				getTestCSIDriver("nil", nil, nil, nil),
			)
			plug, tmpDir := newTestPluginWithAttachDetachVolumeHost(t, fakeClient)
			defer os.RemoveAll(tmpDir)

			attacher, err := plug.NewAttacher()
			if err != nil {
				t.Fatalf("failed to create new attacher: %v", err)
			}
			csiAttacher := getCsiAttacherFromVolumeAttacher(attacher, test.watchTimeout)
			spec := volume.NewSpecFromPersistentVolume(makeTestPV("test-pv", 10, test.driver, "test-vol"), false)

			pluginCanAttach, err := plug.CanAttach(spec)
			if err != nil {
				t.Fatalf("attacher.CanAttach failed: %s", err)
			}
			if pluginCanAttach != test.expectVolumeAttachment {
				t.Errorf("attacher.CanAttach does not match expected attachment status %t", test.expectVolumeAttachment)
			}

			if !pluginCanAttach {
				t.Log("plugin is not attachable")
				return
			}
			var wg sync.WaitGroup
			wg.Add(1)
			go func(volSpec *volume.Spec) {
				attachID, err := csiAttacher.Attach(volSpec, "fakeNode")
				defer wg.Done()

				if err != nil {
					t.Errorf("Attach() failed: %s", err)
				}
				if attachID != "" {
					t.Errorf("Expected empty attachID, got %q", attachID)
				}
			}(spec)

			if test.expectVolumeAttachment {
				expectedAttachID := getAttachmentName("test-vol", test.driver, "fakeNode")
				status := storage.VolumeAttachmentStatus{
					Attached: true,
				}
				markVolumeAttached(t, csiAttacher.k8s, nil, expectedAttachID, status)
			}
			wg.Wait()
		})
	}
}

func TestAttacherWaitForVolumeAttachmentWithCSIDriver(t *testing.T) {
	// In order to detect if the volume plugin would skip WaitForAttach for non-attachable drivers,
	// we do not instantiate any VolumeAttachment. So if the plugin does not skip attach,  WaitForVolumeAttachment
	// will return an error that volume attachment was not found.
	tests := []struct {
		name         string
		driver       string
		expectError  bool
		watchTimeout time.Duration
	}{
		{
			name:        "CSIDriver not attachable -> success",
			driver:      "not-attachable",
			expectError: false,
		},
		{
			name:        "CSIDriver is attachable -> failure",
			driver:      "attachable",
			expectError: true,
		},
		{
			name:        "CSIDriver.AttachRequired not set  -> failure",
			driver:      "nil",
			expectError: true,
		},
		{
			name:        "CSIDriver does not exist not set  -> failure",
			driver:      "unknown",
			expectError: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fakeClient := fakeclient.NewSimpleClientset(
				getTestCSIDriver("not-attachable", nil, &bFalse, nil),
				getTestCSIDriver("attachable", nil, &bTrue, nil),
				getTestCSIDriver("nil", nil, nil, nil),
				&v1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name: "fakeNode",
					},
					Spec: v1.NodeSpec{},
				},
			)
			plug, tmpDir := newTestPlugin(t, fakeClient)
			defer os.RemoveAll(tmpDir)

			attacher, err := plug.NewAttacher()
			if err != nil {
				t.Fatalf("failed to create new attacher: %v", err)
			}
			csiAttacher := getCsiAttacherFromVolumeAttacher(attacher, test.watchTimeout)
			spec := volume.NewSpecFromPersistentVolume(makeTestPV("test-pv", 10, test.driver, "test-vol"), false)

			pluginCanAttach, err := plug.CanAttach(spec)
			if err != nil {
				t.Fatalf("plugin.CanAttach test failed: %s", err)
			}
			if !pluginCanAttach {
				t.Log("plugin is not attachable")
				return
			}

			_, err = csiAttacher.WaitForAttach(spec, "", nil, time.Second)
			if err != nil && !test.expectError {
				t.Errorf("Unexpected error: %s", err)
			}
			if err == nil && test.expectError {
				t.Errorf("Expected error, got none")
			}
		})
	}
}

func TestAttacherWaitForAttach(t *testing.T) {
	tests := []struct {
		name             string
		driver           string
		makeAttachment   func() *storage.VolumeAttachment
		spec             *volume.Spec
		expectedAttachID string
		expectError      bool
		watchTimeout     time.Duration
	}{
		{
			name:   "successful attach",
			driver: "attachable",
			makeAttachment: func() *storage.VolumeAttachment {

				testAttachID := getAttachmentName("test-vol", "attachable", "fakeNode")
				successfulAttachment := makeTestAttachment(testAttachID, "fakeNode", "test-pv")
				successfulAttachment.Status.Attached = true
				return successfulAttachment
			},
			spec:             volume.NewSpecFromPersistentVolume(makeTestPV("test-pv", 10, "attachable", "test-vol"), false),
			expectedAttachID: getAttachmentName("test-vol", "attachable", "fakeNode"),
			expectError:      false,
		},
		{
			name: "failed attach with vol source",
			makeAttachment: func() *storage.VolumeAttachment {

				testAttachID := getAttachmentName("test-vol", "attachable", "fakeNode")
				successfulAttachment := makeTestAttachment(testAttachID, "fakeNode", "volSrc01")
				successfulAttachment.Status.Attached = true
				return successfulAttachment
			},
			spec:        volume.NewSpecFromVolume(makeTestVol("volSrc01", "attachable")),
			expectError: true,
		},
		{
			name:        "failed attach",
			driver:      "attachable",
			expectError: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fakeClient := fakeclient.NewSimpleClientset()
			plug, tmpDir := newTestPlugin(t, fakeClient)
			defer os.RemoveAll(tmpDir)

			attacher, err := plug.NewAttacher()
			if err != nil {
				t.Fatalf("failed to create new attacher: %v", err)
			}
			csiAttacher := getCsiAttacherFromVolumeAttacher(attacher, test.watchTimeout)

			if test.makeAttachment != nil {
				attachment := test.makeAttachment()
				_, err = csiAttacher.k8s.StorageV1().VolumeAttachments().Create(context.TODO(), attachment, metav1.CreateOptions{})
				if err != nil {
					t.Fatalf("failed to create VolumeAttachment: %v", err)
				}
				gotAttachment, err := csiAttacher.k8s.StorageV1().VolumeAttachments().Get(context.TODO(), attachment.Name, metav1.GetOptions{})
				if err != nil {
					t.Fatalf("failed to get created VolumeAttachment: %v", err)
				}
				t.Logf("created test VolumeAttachment %+v", gotAttachment)
			}

			attachID, err := csiAttacher.WaitForAttach(test.spec, "", nil, time.Second)
			if err != nil && !test.expectError {
				t.Errorf("Unexpected error: %s", err)
			}
			if err == nil && test.expectError {
				t.Errorf("Expected error, got none")
			}
			if attachID != test.expectedAttachID {
				t.Errorf("Expected attachID %q, got %q", test.expectedAttachID, attachID)
			}
		})
	}
}

func TestAttacherWaitForAttachWithInline(t *testing.T) {
	tests := []struct {
		name             string
		driver           string
		makeAttachment   func() *storage.VolumeAttachment
		spec             *volume.Spec
		expectedAttachID string
		expectError      bool
		watchTimeout     time.Duration
	}{
		{
			name: "successful attach with PV",
			makeAttachment: func() *storage.VolumeAttachment {

				testAttachID := getAttachmentName("test-vol", "attachable", "fakeNode")
				successfulAttachment := makeTestAttachment(testAttachID, "fakeNode", "test-pv")
				successfulAttachment.Status.Attached = true
				return successfulAttachment
			},
			spec:             volume.NewSpecFromPersistentVolume(makeTestPV("test-pv", 10, "attachable", "test-vol"), false),
			expectedAttachID: getAttachmentName("test-vol", "attachable", "fakeNode"),
			expectError:      false,
		},
		{
			name: "failed attach with volSrc",
			makeAttachment: func() *storage.VolumeAttachment {

				testAttachID := getAttachmentName("test-vol", "attachable", "fakeNode")
				successfulAttachment := makeTestAttachment(testAttachID, "fakeNode", "volSrc01")
				successfulAttachment.Status.Attached = true
				return successfulAttachment
			},
			spec:        volume.NewSpecFromVolume(makeTestVol("volSrc01", "attachable")),
			expectError: true,
		},
		{
			name:        "failed attach",
			driver:      "non-attachable",
			spec:        volume.NewSpecFromPersistentVolume(makeTestPV("test-pv", 10, "non-attachable", "test-vol"), false),
			expectError: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fakeClient := fakeclient.NewSimpleClientset()
			plug, tmpDir := newTestPlugin(t, fakeClient)
			defer os.RemoveAll(tmpDir)

			attacher, err := plug.NewAttacher()
			if err != nil {
				t.Fatalf("failed to create new attacher: %v", err)
			}
			csiAttacher := getCsiAttacherFromVolumeAttacher(attacher, test.watchTimeout)

			if test.makeAttachment != nil {
				attachment := test.makeAttachment()
				_, err = csiAttacher.k8s.StorageV1().VolumeAttachments().Create(context.TODO(), attachment, metav1.CreateOptions{})
				if err != nil {
					t.Fatalf("failed to create VolumeAttachment: %v", err)
				}
				gotAttachment, err := csiAttacher.k8s.StorageV1().VolumeAttachments().Get(context.TODO(), attachment.Name, metav1.GetOptions{})
				if err != nil {
					t.Fatalf("failed to get created VolumeAttachment: %v", err)
				}
				t.Logf("created test VolumeAttachment %+v", gotAttachment)
			}

			attachID, err := csiAttacher.WaitForAttach(test.spec, "", nil, time.Second)
			if test.expectError != (err != nil) {
				t.Errorf("Unexpected error: %s", err)
				return
			}
			if attachID != test.expectedAttachID {
				t.Errorf("Expected attachID %q, got %q", test.expectedAttachID, attachID)
			}
		})
	}
}

func TestAttacherVolumesAreAttached(t *testing.T) {
	type attachedSpec struct {
		volName  string
		spec     *volume.Spec
		attached bool
	}
	testCases := []struct {
		name          string
		attachedSpecs []attachedSpec
		watchTimeout  time.Duration
	}{
		{
			name: "attach and detach",
			attachedSpecs: []attachedSpec{
				{"vol0", volume.NewSpecFromPersistentVolume(makeTestPV("pv0", 10, testDriver, "vol0"), false), true},
				{"vol1", volume.NewSpecFromPersistentVolume(makeTestPV("pv1", 20, testDriver, "vol1"), false), true},
				{"vol2", volume.NewSpecFromPersistentVolume(makeTestPV("pv2", 10, testDriver, "vol2"), false), false},
				{"vol3", volume.NewSpecFromPersistentVolume(makeTestPV("pv3", 10, testDriver, "vol3"), false), false},
				{"vol4", volume.NewSpecFromPersistentVolume(makeTestPV("pv4", 20, testDriver, "vol4"), false), true},
			},
		},
		{
			name: "all detached",
			attachedSpecs: []attachedSpec{
				{"vol0", volume.NewSpecFromPersistentVolume(makeTestPV("pv0", 10, testDriver, "vol0"), false), false},
				{"vol1", volume.NewSpecFromPersistentVolume(makeTestPV("pv1", 20, testDriver, "vol1"), false), false},
				{"vol2", volume.NewSpecFromPersistentVolume(makeTestPV("pv2", 10, testDriver, "vol2"), false), false},
			},
		},
		{
			name: "all attached",
			attachedSpecs: []attachedSpec{
				{"vol0", volume.NewSpecFromPersistentVolume(makeTestPV("pv0", 10, testDriver, "vol0"), false), true},
				{"vol1", volume.NewSpecFromPersistentVolume(makeTestPV("pv1", 20, testDriver, "vol1"), false), true},
			},
		},
		{
			name: "include non-attable",
			attachedSpecs: []attachedSpec{
				{"vol0", volume.NewSpecFromPersistentVolume(makeTestPV("pv0", 10, testDriver, "vol0"), false), true},
				{"vol1", volume.NewSpecFromVolume(makeTestVol("pv1", testDriver)), false},
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			plug, tmpDir := newTestPluginWithAttachDetachVolumeHost(t, nil)
			defer os.RemoveAll(tmpDir)

			attacher, err := plug.NewAttacher()
			if err != nil {
				t.Fatalf("failed to create new attacher: %v", err)
			}
			csiAttacher := getCsiAttacherFromVolumeAttacher(attacher, tc.watchTimeout)
			nodeName := "fakeNode"

			var specs []*volume.Spec
			// create and save volume attchments
			for _, attachedSpec := range tc.attachedSpecs {
				specs = append(specs, attachedSpec.spec)
				attachID := getAttachmentName(attachedSpec.volName, testDriver, nodeName)
				attachment := makeTestAttachment(attachID, nodeName, attachedSpec.spec.Name())
				attachment.Status.Attached = attachedSpec.attached
				_, err := csiAttacher.k8s.StorageV1().VolumeAttachments().Create(context.TODO(), attachment, metav1.CreateOptions{})
				if err != nil {
					t.Fatalf("failed to attach: %v", err)
				}
			}

			// retrieve attached status
			stats, err := csiAttacher.VolumesAreAttached(specs, types.NodeName(nodeName))
			if err != nil {
				t.Fatal(err)
			}
			if len(tc.attachedSpecs) != len(stats) {
				t.Errorf("expecting %d attachment status, got %d", len(tc.attachedSpecs), len(stats))
			}

			// compare attachment status for each spec
			for _, attached := range tc.attachedSpecs {
				stat, ok := stats[attached.spec]
				if attached.attached && !ok {
					t.Error("failed to retrieve attached status for:", attached.spec)
				}
				if attached.attached != stat {
					t.Errorf("expecting volume attachment %t, got %t", attached.attached, stat)
				}
			}
		})
	}
}

func TestAttacherVolumesAreAttachedWithInline(t *testing.T) {
	type attachedSpec struct {
		volName  string
		spec     *volume.Spec
		attached bool
	}
	testCases := []struct {
		name          string
		attachedSpecs []attachedSpec
		watchTimeout  time.Duration
	}{
		{
			name: "attach and detach with volume sources",
			attachedSpecs: []attachedSpec{
				{"vol0", volume.NewSpecFromPersistentVolume(makeTestPV("pv0", 10, testDriver, "vol0"), false), true},
				{"vol1", volume.NewSpecFromVolume(makeTestVol("pv1", testDriver)), false},
				{"vol2", volume.NewSpecFromPersistentVolume(makeTestPV("pv2", 10, testDriver, "vol2"), false), true},
				{"vol3", volume.NewSpecFromVolume(makeTestVol("pv3", testDriver)), false},
				{"vol4", volume.NewSpecFromPersistentVolume(makeTestPV("pv4", 20, testDriver, "vol4"), false), true},
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			plug, tmpDir := newTestPlugin(t, nil)
			defer os.RemoveAll(tmpDir)

			attacher, err := plug.NewAttacher()
			if err != nil {
				t.Fatalf("failed to create new attacher: %v", err)
			}
			csiAttacher := getCsiAttacherFromVolumeAttacher(attacher, tc.watchTimeout)
			nodeName := "fakeNode"

			var specs []*volume.Spec
			// create and save volume attchments
			for _, attachedSpec := range tc.attachedSpecs {
				specs = append(specs, attachedSpec.spec)
				attachID := getAttachmentName(attachedSpec.volName, testDriver, nodeName)
				attachment := makeTestAttachment(attachID, nodeName, attachedSpec.spec.Name())
				attachment.Status.Attached = attachedSpec.attached
				_, err := csiAttacher.k8s.StorageV1().VolumeAttachments().Create(context.TODO(), attachment, metav1.CreateOptions{})
				if err != nil {
					t.Fatalf("failed to attach: %v", err)
				}
			}

			// retrieve attached status
			stats, err := csiAttacher.VolumesAreAttached(specs, types.NodeName(nodeName))
			if err != nil {
				t.Fatal(err)
			}
			if len(tc.attachedSpecs) != len(stats) {
				t.Errorf("expecting %d attachment status, got %d", len(tc.attachedSpecs), len(stats))
			}

			// compare attachment status for each spec
			for _, attached := range tc.attachedSpecs {
				stat, ok := stats[attached.spec]
				if attached.attached && !ok {
					t.Error("failed to retrieve attached status for:", attached.spec)
				}
				if attached.attached != stat {
					t.Errorf("expecting volume attachment %t, got %t", attached.attached, stat)
				}
			}
		})
	}
}

func TestAttacherDetach(t *testing.T) {
	nodeName := "fakeNode"
	testCases := []struct {
		name         string
		volID        string
		attachID     string
		shouldFail   bool
		reactor      func(action core.Action) (handled bool, ret runtime.Object, err error)
		watchTimeout time.Duration
	}{
		{name: "normal test", volID: "vol-001", attachID: getAttachmentName("vol-001", testDriver, nodeName)},
		{name: "normal test 2", volID: "vol-002", attachID: getAttachmentName("vol-002", testDriver, nodeName)},
		{name: "object not found", volID: "vol-non-existing", attachID: getAttachmentName("vol-003", testDriver, nodeName)},
		{
			name:       "API error",
			volID:      "vol-004",
			attachID:   getAttachmentName("vol-004", testDriver, nodeName),
			shouldFail: true, // All other API errors should be propagated to caller
			reactor: func(action core.Action) (handled bool, ret runtime.Object, err error) {
				// return Forbidden to all DELETE requests
				if action.Matches("delete", "volumeattachments") {
					return true, nil, apierrors.NewForbidden(action.GetResource().GroupResource(), action.GetNamespace(), fmt.Errorf("mock error"))
				}
				return false, nil, nil
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Logf("running test: %v", tc.name)
			fakeClient := fakeclient.NewSimpleClientset()
			plug, tmpDir := newTestPluginWithAttachDetachVolumeHost(t, fakeClient)
			defer os.RemoveAll(tmpDir)

			if tc.reactor != nil {
				fakeClient.PrependReactor("*", "*", tc.reactor)
			}

			attacher, err0 := plug.NewAttacher()
			if err0 != nil {
				t.Fatalf("failed to create new attacher: %v", err0)
			}
			csiAttacher := getCsiAttacherFromVolumeAttacher(attacher, tc.watchTimeout)

			pv := makeTestPV("test-pv", 10, testDriver, tc.volID)
			spec := volume.NewSpecFromPersistentVolume(pv, pv.Spec.PersistentVolumeSource.CSI.ReadOnly)
			attachment := makeTestAttachment(tc.attachID, nodeName, "test-pv")
			_, err := csiAttacher.k8s.StorageV1().VolumeAttachments().Create(context.TODO(), attachment, metav1.CreateOptions{})
			if err != nil {
				t.Fatalf("failed to attach: %v", err)
			}
			volumeName, err := plug.GetVolumeName(spec)
			if err != nil {
				t.Errorf("test case %s failed: %v", tc.name, err)
			}

			err = csiAttacher.Detach(volumeName, types.NodeName(nodeName))
			if tc.shouldFail && err == nil {
				t.Fatal("expecting failure, but err = nil")
			}
			if !tc.shouldFail && err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			attach, err := csiAttacher.k8s.StorageV1().VolumeAttachments().Get(context.TODO(), tc.attachID, metav1.GetOptions{})
			if err != nil {
				if !apierrors.IsNotFound(err) {
					t.Fatalf("unexpected err: %v", err)
				}
			} else {
				if attach == nil {
					t.Errorf("expecting attachment not to be nil, but it is")
				}
			}
		})
	}
}

func TestAttacherGetDeviceMountPath(t *testing.T) {
	// Setup
	// Create a new attacher
	fakeClient := fakeclient.NewSimpleClientset()
	plug, tmpDir := newTestPlugin(t, fakeClient)
	defer os.RemoveAll(tmpDir)
	attacher, err0 := plug.NewAttacher()
	if err0 != nil {
		t.Fatalf("failed to create new attacher: %v", err0)
	}
	csiAttacher := getCsiAttacherFromVolumeAttacher(attacher, testWatchTimeout)

	pluginDir := csiAttacher.plugin.host.GetPluginDir(plug.GetPluginName())
	testCases := []struct {
		testName           string
		pvName             string
		volumeId           string
		skipPVCSISource    bool // The test clears PV.Spec.CSI
		shouldFail         bool
		addVolSource       bool // The test adds a Volume.VolumeSource.CSI.
		removeVolumeHandle bool // The test force removes CSI volume handle.
	}{
		{
			testName: "success test",
			pvName:   "test-pv1",
			volumeId: "test-vol1",
		},
		{
			testName:        "fail test, failed to create device mount path due to missing CSI source",
			pvName:          "test-pv1",
			volumeId:        "test-vol1",
			skipPVCSISource: true,
			shouldFail:      true,
		},
		{
			testName:     "fail test, failed to create device mount path, CSIVolumeSource found",
			pvName:       "test-pv1",
			volumeId:     "test-vol1",
			addVolSource: true,
			shouldFail:   true,
		},
		{
			testName:           "fail test, failed to create device mount path, missing CSI volume handle",
			pvName:             "test-pv1",
			volumeId:           "test-vol1",
			shouldFail:         true,
			removeVolumeHandle: true,
		},
	}

	for _, tc := range testCases {
		t.Logf("Running test case: %s", tc.testName)
		var spec *volume.Spec

		// Create spec
		pv := makeTestPV(tc.pvName, 10, testDriver, tc.volumeId)
		if tc.removeVolumeHandle {
			pv.Spec.PersistentVolumeSource.CSI.VolumeHandle = ""
		}
		if tc.addVolSource {
			spec = volume.NewSpecFromVolume(makeTestVol(tc.pvName, testDriver))
		} else {
			spec = volume.NewSpecFromPersistentVolume(pv, pv.Spec.PersistentVolumeSource.CSI.ReadOnly)
			if tc.skipPVCSISource {
				spec.PersistentVolume.Spec.CSI = nil
			}
		}
		// Run
		mountPath, err := csiAttacher.GetDeviceMountPath(spec)

		// Verify
		if err != nil && !tc.shouldFail {
			t.Errorf("test should not fail, but error occurred: %v", err)
		} else if err == nil {
			expectedMountPath := filepath.Join(pluginDir, testDriver, generateSha(tc.volumeId), globalMountInGlobalPath)
			if tc.shouldFail {
				t.Errorf("test should fail, but no error occurred")
			} else if mountPath != expectedMountPath {
				t.Errorf("mountPath does not equal expectedMountPath. Got: %s. Expected: %s", mountPath, expectedMountPath)
			}
		}
	}
}

func TestAttacherMountDevice(t *testing.T) {
	pvName := "test-pv"
	var testFSGroup int64 = 3000
	nonFinalError := volumetypes.NewUncertainProgressError("")
	transientError := volumetypes.NewTransientOperationFailure("")

	testCases := []struct {
		testName                       string
		volName                        string
		devicePath                     string
		deviceMountPath                string
		stageUnstageSet                bool
		fsGroup                        *int64
		expectedVolumeMountGroup       string
		driverSupportsVolumeMountGroup bool
		shouldFail                     bool
		createAttachment               bool
		populateDeviceMountPath        bool
		exitError                      error
		spec                           *volume.Spec
		watchTimeout                   time.Duration
		skipClientSetup                bool
	}{
		{
			testName:         "normal PV",
			volName:          "test-vol1",
			devicePath:       "path1",
			deviceMountPath:  "path2",
			stageUnstageSet:  true,
			createAttachment: true,
			spec:             volume.NewSpecFromPersistentVolume(makeTestPV(pvName, 10, testDriver, "test-vol1"), false),
		},
		{
			testName:         "normal PV with mount options",
			volName:          "test-vol1",
			devicePath:       "path1",
			deviceMountPath:  "path2",
			stageUnstageSet:  true,
			createAttachment: true,
			spec:             volume.NewSpecFromPersistentVolume(makeTestPVWithMountOptions(pvName, 10, testDriver, "test-vol1", []string{"test-op"}), false),
		},
		{
			testName:         "normal PV but with missing attachment should result in no-change",
			volName:          "test-vol1",
			devicePath:       "path1",
			deviceMountPath:  "path2",
			stageUnstageSet:  true,
			createAttachment: false,
			shouldFail:       true,
			exitError:        transientError,
			spec:             volume.NewSpecFromPersistentVolume(makeTestPVWithMountOptions(pvName, 10, testDriver, "test-vol1", []string{"test-op"}), false),
		},
		{
			testName:         "no vol name",
			volName:          "",
			devicePath:       "path1",
			deviceMountPath:  "path2",
			stageUnstageSet:  true,
			shouldFail:       true,
			createAttachment: true,
			spec:             volume.NewSpecFromPersistentVolume(makeTestPV(pvName, 10, testDriver, ""), false),
		},
		{
			testName:         "no device path",
			volName:          "test-vol1",
			devicePath:       "",
			deviceMountPath:  "path2",
			stageUnstageSet:  true,
			shouldFail:       false,
			createAttachment: true,
			spec:             volume.NewSpecFromPersistentVolume(makeTestPV(pvName, 10, testDriver, "test-vol1"), false),
		},
		{
			testName:         "no device mount path",
			volName:          "test-vol1",
			devicePath:       "path1",
			deviceMountPath:  "",
			stageUnstageSet:  true,
			shouldFail:       true,
			createAttachment: true,
			spec:             volume.NewSpecFromPersistentVolume(makeTestPV(pvName, 10, testDriver, "test-vol1"), false),
		},
		{
			testName:         "stage_unstage cap not set",
			volName:          "test-vol1",
			devicePath:       "path1",
			deviceMountPath:  "path2",
			stageUnstageSet:  false,
			createAttachment: true,
			spec:             volume.NewSpecFromPersistentVolume(makeTestPV(pvName, 10, testDriver, "test-vol1"), false),
		},
		{
			testName:         "failure with volume source",
			volName:          "test-vol1",
			devicePath:       "path1",
			deviceMountPath:  "path2",
			shouldFail:       true,
			createAttachment: true,
			spec:             volume.NewSpecFromVolume(makeTestVol(pvName, testDriver)),
		},
		{
			testName:         "pv with nodestage timeout should result in in-progress device",
			volName:          fakecsi.NodeStageTimeOut_VolumeID,
			devicePath:       "path1",
			deviceMountPath:  "path2",
			stageUnstageSet:  true,
			createAttachment: true,
			spec:             volume.NewSpecFromPersistentVolume(makeTestPV(pvName, 10, testDriver, fakecsi.NodeStageTimeOut_VolumeID), false),
			exitError:        nonFinalError,
			shouldFail:       true,
		},
		{
			testName:                "failure PV with existing data",
			volName:                 "test-vol1",
			devicePath:              "path1",
			deviceMountPath:         "path2",
			stageUnstageSet:         true,
			createAttachment:        true,
			populateDeviceMountPath: true,
			shouldFail:              true,
			spec:                    volume.NewSpecFromPersistentVolume(makeTestPV(pvName, 10, testDriver, "test-vol1"), true),
		},
		{
			testName:                       "fsgroup provided, driver supports volume mount group; expect fsgroup to be passed to NodeStageVolume",
			volName:                        "test-vol1",
			devicePath:                     "path1",
			deviceMountPath:                "path2",
			fsGroup:                        &testFSGroup,
			driverSupportsVolumeMountGroup: true,
			expectedVolumeMountGroup:       "3000",
			stageUnstageSet:                true,
			createAttachment:               true,
			spec:                           volume.NewSpecFromPersistentVolume(makeTestPV(pvName, 10, testDriver, "test-vol1"), false),
		},
		{
			testName:                       "fsgroup not provided, driver supports volume mount group; expect fsgroup not to be passed to NodeStageVolume",
			volName:                        "test-vol1",
			devicePath:                     "path1",
			deviceMountPath:                "path2",
			driverSupportsVolumeMountGroup: true,
			expectedVolumeMountGroup:       "",
			stageUnstageSet:                true,
			createAttachment:               true,
			spec:                           volume.NewSpecFromPersistentVolume(makeTestPV(pvName, 10, testDriver, "test-vol1"), false),
		},
		{
			testName:                       "fsgroup provided, driver does not support volume mount group; expect fsgroup not to be passed to NodeStageVolume",
			volName:                        "test-vol1",
			devicePath:                     "path1",
			deviceMountPath:                "path2",
			fsGroup:                        &testFSGroup,
			driverSupportsVolumeMountGroup: false,
			expectedVolumeMountGroup:       "",
			stageUnstageSet:                true,
			createAttachment:               true,
			spec:                           volume.NewSpecFromPersistentVolume(makeTestPV(pvName, 10, testDriver, "test-vol1"), false),
		},
		{
			testName:                "driver not specified",
			volName:                 "test-vol1",
			devicePath:              "path1",
			deviceMountPath:         "path2",
			fsGroup:                 &testFSGroup,
			stageUnstageSet:         true,
			createAttachment:        true,
			populateDeviceMountPath: false,
			spec:                    volume.NewSpecFromPersistentVolume(makeTestPV(pvName, 10, "not-found", "test-vol1"), false),
			exitError:               transientError,
			shouldFail:              true,
			skipClientSetup:         true,
		},
	}

	for _, tc := range testCases {
		user, err := user.Current()
		if err != nil {
			t.Logf("Current user could not be determined, assuming non-root: %v", err)
		} else {
			if tc.populateDeviceMountPath && user.Uid == "0" {
				t.Skipf("Skipping intentional failure on existing data when running as root.")
			}
		}
		t.Run(tc.testName, func(t *testing.T) {
			t.Logf("Running test case: %s", tc.testName)

			// Setup
			// Create a new attacher
			fakeClient := fakeclient.NewSimpleClientset()
			plug, tmpDir := newTestPlugin(t, fakeClient)
			defer os.RemoveAll(tmpDir)

			attacher, err0 := plug.NewAttacher()
			if err0 != nil {
				t.Fatalf("failed to create new attacher: %v", err0)
			}
			csiAttacher := getCsiAttacherFromVolumeAttacher(attacher, tc.watchTimeout)
			if !tc.skipClientSetup {
				csiAttacher.csiClient = setupClientWithVolumeMountGroup(t, tc.stageUnstageSet, tc.driverSupportsVolumeMountGroup)
			}

			if tc.deviceMountPath != "" {
				tc.deviceMountPath = filepath.Join(tmpDir, tc.deviceMountPath)
			}

			nodeName := string(csiAttacher.plugin.host.GetNodeName())
			attachID := getAttachmentName(tc.volName, testDriver, nodeName)

			if tc.createAttachment {
				// Set up volume attachment
				attachment := makeTestAttachment(attachID, nodeName, pvName)
				_, err := csiAttacher.k8s.StorageV1().VolumeAttachments().Create(context.TODO(), attachment, metav1.CreateOptions{})
				if err != nil {
					t.Fatalf("failed to attach: %v", err)
				}
			}

			parent := filepath.Dir(tc.deviceMountPath)
			filePath := filepath.Join(parent, "newfile")
			if tc.populateDeviceMountPath {
				// We need to create the deviceMountPath before we Mount,
				// so that we can correctly create the file without errors.
				err := os.MkdirAll(tc.deviceMountPath, 0750)
				if err != nil {
					t.Errorf("error attempting to create the directory")
				}
				_, err = os.Create(filePath)
				if err != nil {
					t.Errorf("error attempting to populate file on parent path: %v", err)
				}
				err = filesystem.Chmod(parent, 0555)
				if err != nil {
					t.Errorf("error attempting to modify directory permissions: %v", err)
				}
			}

			// Run
			err := csiAttacher.MountDevice(
				tc.spec,
				tc.devicePath,
				tc.deviceMountPath,
				volume.DeviceMounterArgs{FsGroup: tc.fsGroup})

			// Verify
			if err != nil {
				if !tc.shouldFail {
					t.Errorf("test should not fail, but error occurred: %v", err)
				}
				if tc.populateDeviceMountPath {
					// We're expecting saveVolumeData to fail, which is responsible
					// for creating this file. It shouldn't exist.
					_, err := os.Stat(filepath.Join(parent, volDataFileName))
					if !os.IsNotExist(err) {
						t.Errorf("vol_data.json should not exist: %v", err)
					}
					_, err = os.Stat(filePath)
					if os.IsNotExist(err) {
						t.Errorf("expecting file to exist after err received: %v", err)
					}
					err = filesystem.Chmod(parent, 0777)
					if err != nil {
						t.Errorf("failed to modify permissions after test: %v", err)
					}
				}
				if tc.exitError != nil && reflect.TypeOf(tc.exitError) != reflect.TypeOf(err) {
					t.Fatalf("expected exitError type: %v got: %v (%v)", reflect.TypeOf(tc.exitError), reflect.TypeOf(err), err)
				}
				return
			}
			if tc.shouldFail {
				t.Errorf("test should fail, but no error occurred")
			}

			// Verify call goes through all the way
			numStaged := 1
			if !tc.stageUnstageSet {
				numStaged = 0
			}

			cdc := csiAttacher.csiClient.(*fakeCsiDriverClient)
			staged := cdc.nodeClient.GetNodeStagedVolumes()
			if len(staged) != numStaged {
				t.Errorf("got wrong number of staged volumes, expecting %v got: %v", numStaged, len(staged))
			}
			if tc.stageUnstageSet {
				vol, ok := staged[tc.volName]
				if !ok {
					t.Errorf("could not find staged volume: %s", tc.volName)
				}
				if vol.Path != tc.deviceMountPath {
					t.Errorf("expected mount path: %s. got: %s", tc.deviceMountPath, vol.Path)
				}
				if !reflect.DeepEqual(vol.MountFlags, tc.spec.PersistentVolume.Spec.MountOptions) {
					t.Errorf("expected mount options: %v, got: %v", tc.spec.PersistentVolume.Spec.MountOptions, vol.MountFlags)
				}
				if vol.VolumeMountGroup != tc.expectedVolumeMountGroup {
					t.Errorf("expected volume mount group %q, got: %q", tc.expectedVolumeMountGroup, vol.VolumeMountGroup)
				}
			}

			// Verify the deviceMountPath was created by the plugin
			if tc.stageUnstageSet {
				s, err := os.Stat(tc.deviceMountPath)
				if err != nil {
					t.Errorf("expected staging directory %s to be created and be a directory, got error: %s", tc.deviceMountPath, err)
				} else {
					if !s.IsDir() {
						t.Errorf("expected staging directory %s to be directory, got something else", tc.deviceMountPath)
					}
				}
			}
		})
	}
}

func TestAttacherMountDeviceWithInline(t *testing.T) {
	pvName := "test-pv"
	var testFSGroup int64 = 3000
	testCases := []struct {
		testName                 string
		volName                  string
		devicePath               string
		deviceMountPath          string
		fsGroup                  *int64
		expectedVolumeMountGroup string
		stageUnstageSet          bool
		shouldFail               bool
		spec                     *volume.Spec
		watchTimeout             time.Duration
	}{
		{
			testName:        "normal PV",
			volName:         "test-vol1",
			devicePath:      "path1",
			deviceMountPath: "path2",
			stageUnstageSet: true,
			spec:            volume.NewSpecFromPersistentVolume(makeTestPV(pvName, 10, testDriver, "test-vol1"), false),
		},
		{
			testName:        "failure with volSrc",
			volName:         "test-vol1",
			devicePath:      "path1",
			deviceMountPath: "path2",
			shouldFail:      true,
			spec:            volume.NewSpecFromVolume(makeTestVol(pvName, testDriver)),
		},
		{
			testName:        "no vol name",
			volName:         "",
			devicePath:      "path1",
			deviceMountPath: "path2",
			stageUnstageSet: true,
			shouldFail:      true,
			spec:            volume.NewSpecFromPersistentVolume(makeTestPV(pvName, 10, testDriver, ""), false),
		},
		{
			testName:        "no device path",
			volName:         "test-vol1",
			devicePath:      "",
			deviceMountPath: "path2",
			stageUnstageSet: true,
			shouldFail:      false,
			spec:            volume.NewSpecFromPersistentVolume(makeTestPV(pvName, 10, testDriver, "test-vol1"), false),
		},
		{
			testName:        "no device mount path",
			volName:         "test-vol1",
			devicePath:      "path1",
			deviceMountPath: "",
			stageUnstageSet: true,
			shouldFail:      true,
			spec:            volume.NewSpecFromPersistentVolume(makeTestPV(pvName, 10, testDriver, "test-vol1"), false),
		},
		{
			testName:        "stage_unstage cap not set",
			volName:         "test-vol1",
			devicePath:      "path1",
			deviceMountPath: "path2",
			stageUnstageSet: false,
			spec:            volume.NewSpecFromPersistentVolume(makeTestPV(pvName, 10, testDriver, "test-vol1"), false),
		},
		{
			testName:        "missing spec",
			volName:         "test-vol1",
			devicePath:      "path1",
			deviceMountPath: "path2",
			shouldFail:      true,
		},
		{
			testName:                 "fsgroup set",
			volName:                  "test-vol1",
			devicePath:               "path1",
			deviceMountPath:          "path2",
			fsGroup:                  &testFSGroup,
			expectedVolumeMountGroup: "3000",
			stageUnstageSet:          true,
			spec:                     volume.NewSpecFromPersistentVolume(makeTestPV(pvName, 10, testDriver, "test-vol1"), false),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.testName, func(t *testing.T) {
			t.Logf("Running test case: %s", tc.testName)

			// Setup
			// Create a new attacher
			fakeClient := fakeclient.NewSimpleClientset()
			plug, tmpDir := newTestPlugin(t, fakeClient)
			defer os.RemoveAll(tmpDir)

			fakeWatcher := watch.NewRaceFreeFake()
			fakeClient.Fake.PrependWatchReactor("volumeattachments", core.DefaultWatchReactor(fakeWatcher, nil))

			attacher, err0 := plug.NewAttacher()
			if err0 != nil {
				t.Fatalf("failed to create new attacher: %v", err0)
			}
			csiAttacher := getCsiAttacherFromVolumeAttacher(attacher, tc.watchTimeout)
			csiAttacher.csiClient = setupClientWithVolumeMountGroup(t, tc.stageUnstageSet, true /* volumeMountGroup */)

			if tc.deviceMountPath != "" {
				tc.deviceMountPath = filepath.Join(tmpDir, tc.deviceMountPath)
			}

			nodeName := string(csiAttacher.plugin.host.GetNodeName())
			attachID := getAttachmentName(tc.volName, testDriver, nodeName)

			// Set up volume attachment
			attachment := makeTestAttachment(attachID, nodeName, pvName)
			_, err := csiAttacher.k8s.StorageV1().VolumeAttachments().Create(context.TODO(), attachment, metav1.CreateOptions{})
			if err != nil {
				t.Fatalf("failed to attach: %v", err)
			}

			var wg sync.WaitGroup
			wg.Add(1)

			go func() {
				defer wg.Done()
				fakeWatcher.Delete(attachment)
			}()

			// Run
			err = csiAttacher.MountDevice(
				tc.spec,
				tc.devicePath,
				tc.deviceMountPath,
				volume.DeviceMounterArgs{FsGroup: tc.fsGroup})

			// Verify
			if err != nil {
				if !tc.shouldFail {
					t.Errorf("test should not fail, but error occurred: %v", err)
				}
				return
			}
			if tc.shouldFail {
				t.Errorf("test should fail, but no error occurred")
			}

			// Verify call goes through all the way
			numStaged := 1
			if !tc.stageUnstageSet {
				numStaged = 0
			}

			cdc := csiAttacher.csiClient.(*fakeCsiDriverClient)
			staged := cdc.nodeClient.GetNodeStagedVolumes()
			if len(staged) != numStaged {
				t.Errorf("got wrong number of staged volumes, expecting %v got: %v", numStaged, len(staged))
			}
			if tc.stageUnstageSet {
				vol, ok := staged[tc.volName]
				if !ok {
					t.Errorf("could not find staged volume: %s", tc.volName)
				}
				if vol.Path != tc.deviceMountPath {
					t.Errorf("expected mount path: %s. got: %s", tc.deviceMountPath, vol.Path)
				}
				if vol.VolumeMountGroup != tc.expectedVolumeMountGroup {
					t.Errorf("expected volume mount group %q, got: %q", tc.expectedVolumeMountGroup, vol.VolumeMountGroup)
				}
			}

			wg.Wait()
		})
	}
}

func TestAttacherUnmountDevice(t *testing.T) {
	transientError := volumetypes.NewTransientOperationFailure("")
	testCases := []struct {
		testName        string
		volID           string
		deviceMountPath string
		jsonFile        string
		createPV        bool
		stageUnstageSet bool
		shouldFail      bool
		watchTimeout    time.Duration
		exitError       error
		unsetClient     bool
	}{
		// PV agnostic path positive test cases
		{
			testName:        "success, json file exists",
			volID:           "project/zone/test-vol1",
			deviceMountPath: "plugins/csi/" + generateSha("project/zone/test-vol1") + "/globalmount",
			jsonFile:        `{"driverName": "csi", "volumeHandle":"project/zone/test-vol1"}`,
			stageUnstageSet: true,
		},
		{
			testName:        "stage_unstage not set, PV agnostic path, unmount device is skipped",
			deviceMountPath: "plugins/csi/" + generateSha("project/zone/test-vol1") + "/globalmount",
			jsonFile:        `{"driverName":"test-driver","volumeHandle":"test-vol1"}`,
			stageUnstageSet: false,
		},
		// PV agnostic path negative test cases
		{
			testName:        "success: json file doesn't exist, unmount device is skipped",
			deviceMountPath: "plugins/csi/" + generateSha("project/zone/test-vol1") + "/globalmount",
			jsonFile:        "",
			stageUnstageSet: true,
			createPV:        true,
		},
		{
			testName:        "fail: invalid json, fail to retrieve driver and volumeID from globalpath",
			volID:           "project/zone/test-vol1",
			deviceMountPath: "plugins/csi/" + generateSha("project/zone/test-vol1") + "/globalmount",
			jsonFile:        `{"driverName"}}`,
			stageUnstageSet: true,
			shouldFail:      true,
		},
		// Ensure that a transient error is returned if the client is not established
		{
			testName:        "fail with transient error, json file exists but client not found",
			volID:           "project/zone/test-vol1",
			deviceMountPath: "plugins/csi/" + generateSha("project/zone/test-vol1") + "/globalmount",
			jsonFile:        `{"driverName": "unknown-driver", "volumeHandle":"project/zone/test-vol1"}`,
			stageUnstageSet: true,
			shouldFail:      true,
			exitError:       transientError,
			unsetClient:     true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.testName, func(t *testing.T) {
			t.Logf("Running test case: %s", tc.testName)
			// Setup
			// Create a new attacher
			fakeClient := fakeclient.NewSimpleClientset()
			plug, tmpDir := newTestPlugin(t, fakeClient)
			defer os.RemoveAll(tmpDir)
			attacher, err0 := plug.NewAttacher()
			if err0 != nil {
				t.Fatalf("failed to create new attacher: %v", err0)
			}
			csiAttacher := getCsiAttacherFromVolumeAttacher(attacher, tc.watchTimeout)
			csiAttacher.csiClient = setupClient(t, tc.stageUnstageSet)

			if tc.deviceMountPath != "" {
				tc.deviceMountPath = filepath.Join(tmpDir, tc.deviceMountPath)
			}
			// Add the volume to NodeStagedVolumes
			cdc := csiAttacher.csiClient.(*fakeCsiDriverClient)
			cdc.nodeClient.AddNodeStagedVolume(tc.volID, tc.deviceMountPath, nil)

			// Make the device staged path
			if tc.deviceMountPath != "" {
				if err := os.MkdirAll(tc.deviceMountPath, 0755); err != nil {
					t.Fatalf("error creating directory %s: %s", tc.deviceMountPath, err)
				}
			}
			dir := filepath.Dir(tc.deviceMountPath)
			// Make JSON for this object
			if tc.jsonFile != "" {
				dataPath := filepath.Join(dir, volDataFileName)
				if err := os.WriteFile(dataPath, []byte(tc.jsonFile), 0644); err != nil {
					t.Fatalf("error creating %s: %s", dataPath, err)
				}
			}
			if tc.createPV {
				// Make the PV for this object
				pvName := filepath.Base(dir)
				pv := makeTestPV(pvName, 5, "csi", tc.volID)
				_, err := csiAttacher.k8s.CoreV1().PersistentVolumes().Create(context.TODO(), pv, metav1.CreateOptions{})
				if err != nil && !tc.shouldFail {
					t.Fatalf("Failed to create PV: %v", err)
				}
			}
			// Clear out the client if specified
			// The lookup to generate a new client will fail
			if tc.unsetClient {
				csiAttacher.csiClient = nil
			}

			// Run
			err := csiAttacher.UnmountDevice(tc.deviceMountPath)
			// Verify
			if err != nil {
				if !tc.shouldFail {
					t.Errorf("test should not fail, but error occurred: %v", err)
				}
				if tc.exitError != nil && reflect.TypeOf(tc.exitError) != reflect.TypeOf(err) {
					t.Fatalf("expected exitError type: %v got: %v (%v)", reflect.TypeOf(tc.exitError), reflect.TypeOf(err), err)
				}
				return
			}
			if tc.shouldFail {
				t.Errorf("test should fail, but no error occurred")
			}

			// Verify call goes through all the way
			expectedSet := 0
			if !tc.stageUnstageSet || tc.volID == "" {
				expectedSet = 1
			}
			staged := cdc.nodeClient.GetNodeStagedVolumes()
			if len(staged) != expectedSet {
				t.Errorf("got wrong number of staged volumes, expecting %v got: %v", expectedSet, len(staged))
			}

			_, ok := staged[tc.volID]
			if ok && tc.stageUnstageSet && tc.volID != "" {
				t.Errorf("found unexpected staged volume: %s", tc.volID)
			} else if !ok && !tc.stageUnstageSet {
				t.Errorf("could not find expected staged volume: %s", tc.volID)
			}

			if tc.jsonFile != "" && !tc.shouldFail {
				dataPath := filepath.Join(dir, volDataFileName)
				if _, err := os.Stat(dataPath); !os.IsNotExist(err) {
					if err != nil {
						t.Errorf("error checking file %s: %s", dataPath, err)
					} else {
						t.Errorf("json file %s should not exists, but it does", dataPath)
					}
				} else {
					t.Logf("json file %s was correctly removed", dataPath)
				}
			}
		})
	}
}

func getCsiAttacherFromVolumeAttacher(attacher volume.Attacher, watchTimeout time.Duration) *csiAttacher {
	if watchTimeout == 0 {
		watchTimeout = testWatchTimeout
	}
	csiAttacher := attacher.(*csiAttacher)
	csiAttacher.watchTimeout = watchTimeout
	return csiAttacher
}

func getCsiAttacherFromVolumeDetacher(detacher volume.Detacher, watchTimeout time.Duration) *csiAttacher {
	if watchTimeout == 0 {
		watchTimeout = testWatchTimeout
	}
	csiAttacher := detacher.(*csiAttacher)
	csiAttacher.watchTimeout = watchTimeout
	return csiAttacher
}

func getCsiAttacherFromDeviceMounter(deviceMounter volume.DeviceMounter, watchTimeout time.Duration) *csiAttacher {
	if watchTimeout == 0 {
		watchTimeout = testWatchTimeout
	}
	csiAttacher := deviceMounter.(*csiAttacher)
	csiAttacher.watchTimeout = watchTimeout
	return csiAttacher
}

func getCsiAttacherFromDeviceUnmounter(deviceUnmounter volume.DeviceUnmounter, watchTimeout time.Duration) *csiAttacher {
	if watchTimeout == 0 {
		watchTimeout = testWatchTimeout
	}
	csiAttacher := deviceUnmounter.(*csiAttacher)
	csiAttacher.watchTimeout = watchTimeout
	return csiAttacher
}

func generateSha(handle string) string {
	result := sha256.Sum256([]byte(fmt.Sprintf("%s", handle)))
	return fmt.Sprintf("%x", result)
}
