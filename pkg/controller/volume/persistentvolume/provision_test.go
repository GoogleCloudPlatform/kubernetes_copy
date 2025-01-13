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

package persistentvolume

import (
	"errors"
	"testing"

	utilfeature "k8s.io/apiserver/pkg/util/feature"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	"k8s.io/klog/v2/ktesting"
	"k8s.io/kubernetes/pkg/features"

	v1 "k8s.io/api/core/v1"
	storage "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/version"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/component-helpers/storage/volume"
	api "k8s.io/kubernetes/pkg/apis/core"
	pvtesting "k8s.io/kubernetes/pkg/controller/volume/persistentvolume/testing"
)

var class1Parameters = map[string]string{
	"param1": "value1",
}
var class2Parameters = map[string]string{
	"param2": "value2",
}
var deleteReclaimPolicy = v1.PersistentVolumeReclaimDelete
var modeImmediate = storage.VolumeBindingImmediate
var storageClasses = []*storage.StorageClass{
	{
		TypeMeta: metav1.TypeMeta{
			Kind: "StorageClass",
		},

		ObjectMeta: metav1.ObjectMeta{
			Name: "gold",
		},

		Provisioner:       mockPluginName,
		Parameters:        class1Parameters,
		ReclaimPolicy:     &deleteReclaimPolicy,
		VolumeBindingMode: &modeImmediate,
	},
	{
		TypeMeta: metav1.TypeMeta{
			Kind: "StorageClass",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: "silver",
		},
		Provisioner:       mockPluginName,
		Parameters:        class2Parameters,
		ReclaimPolicy:     &deleteReclaimPolicy,
		VolumeBindingMode: &modeImmediate,
	},
	{
		TypeMeta: metav1.TypeMeta{
			Kind: "StorageClass",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: "copper",
		},
		Provisioner:       mockPluginName,
		Parameters:        class1Parameters,
		ReclaimPolicy:     &deleteReclaimPolicy,
		VolumeBindingMode: &modeWait,
	},
	{
		TypeMeta: metav1.TypeMeta{
			Kind: "StorageClass",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: "external",
		},
		Provisioner:       "vendor.com/my-volume",
		Parameters:        class1Parameters,
		ReclaimPolicy:     &deleteReclaimPolicy,
		VolumeBindingMode: &modeImmediate,
	},
	{
		TypeMeta: metav1.TypeMeta{
			Kind: "StorageClass",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: "external-wait",
		},
		Provisioner:       "vendor.com/my-volume-wait",
		Parameters:        class1Parameters,
		ReclaimPolicy:     &deleteReclaimPolicy,
		VolumeBindingMode: &modeWait,
	},
	{
		TypeMeta: metav1.TypeMeta{
			Kind: "StorageClass",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: "unknown-internal",
		},
		Provisioner:       "kubernetes.io/unknown",
		Parameters:        class1Parameters,
		ReclaimPolicy:     &deleteReclaimPolicy,
		VolumeBindingMode: &modeImmediate,
	},
	{
		TypeMeta: metav1.TypeMeta{
			Kind: "StorageClass",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: "unsupported-mountoptions",
		},
		Provisioner:       mockPluginName,
		Parameters:        class1Parameters,
		ReclaimPolicy:     &deleteReclaimPolicy,
		MountOptions:      []string{"foo"},
		VolumeBindingMode: &modeImmediate,
	},
	{
		TypeMeta: metav1.TypeMeta{
			Kind: "StorageClass",
		},

		ObjectMeta: metav1.ObjectMeta{
			Name: "csi",
		},

		Provisioner:       "mydriver.csi.k8s.io",
		Parameters:        class1Parameters,
		ReclaimPolicy:     &deleteReclaimPolicy,
		VolumeBindingMode: &modeImmediate,
	},
}

// call to storageClass 1, returning an error
var provision1Error = provisionCall{
	ret:                errors.New("Mock provisioner error"),
	expectedParameters: class1Parameters,
}

// call to storageClass 1, returning a valid PV
var provision1Success = provisionCall{
	ret:                nil,
	expectedParameters: class1Parameters,
}

// call to storageClass 2, returning a valid PV
var provision2Success = provisionCall{
	ret:                nil,
	expectedParameters: class2Parameters,
}

// Test single call to syncVolume, expecting provisioning to happen.
// 1. Fill in the controller with initial data
// 2. Call the syncVolume *once*.
// 3. Compare resulting volumes with expected volumes.
func TestProvisionSync(t *testing.T) {
	// Default enable the HonorPVReclaimPolicy feature gate.
	// TODO: this will be removed in 1.36
	featuregatetesting.SetFeatureGateEmulationVersionDuringTest(t, utilfeature.DefaultFeatureGate, version.MustParse("1.32"))
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.HonorPVReclaimPolicy, true)
	_, ctx := ktesting.NewTestContext(t)
	tests := []controllerTest{
		{
			// Provision a volume (with a default class)
			name:            "11-1 - successful provision with storage class 1",
			initialVolumes:  novolumes,
			expectedVolumes: volumesWithFinalizers(newVolumeArray("pvc-uid11-1", "1Gi", "uid11-1", "claim11-1", v1.VolumeBound, v1.PersistentVolumeReclaimDelete, classGold, volume.AnnBoundByController, volume.AnnDynamicallyProvisioned), []string{volume.PVDeletionInTreeProtectionFinalizer}),
			// Binding will be completed in the next syncClaim
			initialClaims:  newClaimArray("claim11-1", "uid11-1", "1Gi", "", v1.ClaimPending, &classGold),
			expectedClaims: newClaimArray("claim11-1", "uid11-1", "1Gi", "", v1.ClaimPending, &classGold, volume.AnnStorageProvisioner, volume.AnnBetaStorageProvisioner),
			expectedEvents: []string{"Normal ProvisioningSucceeded"},
			errors:         noerrors,
			test:           wrapTestWithProvisionCalls([]provisionCall{provision1Success}, testSyncClaim),
		},
		{
			// Provision failure - plugin not found
			name:            "11-2 - plugin not found",
			initialVolumes:  novolumes,
			expectedVolumes: novolumes,
			initialClaims:   newClaimArray("claim11-2", "uid11-2", "1Gi", "", v1.ClaimPending, &classGold),
			expectedClaims:  newClaimArray("claim11-2", "uid11-2", "1Gi", "", v1.ClaimPending, &classGold),
			expectedEvents:  []string{"Warning ProvisioningFailed"},
			errors:          noerrors,
			test:            testSyncClaim,
		},
		{
			// Provision failure - newProvisioner returns error
			name:            "11-3 - newProvisioner failure",
			initialVolumes:  novolumes,
			expectedVolumes: novolumes,
			initialClaims:   newClaimArray("claim11-3", "uid11-3", "1Gi", "", v1.ClaimPending, &classGold),
			expectedClaims:  newClaimArray("claim11-3", "uid11-3", "1Gi", "", v1.ClaimPending, &classGold, volume.AnnStorageProvisioner, volume.AnnBetaStorageProvisioner),
			expectedEvents:  []string{"Warning ProvisioningFailed"},
			errors:          noerrors,
			test:            wrapTestWithProvisionCalls([]provisionCall{}, testSyncClaim),
		},
		{
			// Provision failure - Provision returns error
			name:            "11-4 - provision failure",
			initialVolumes:  novolumes,
			expectedVolumes: novolumes,
			initialClaims:   newClaimArray("claim11-4", "uid11-4", "1Gi", "", v1.ClaimPending, &classGold),
			expectedClaims:  newClaimArray("claim11-4", "uid11-4", "1Gi", "", v1.ClaimPending, &classGold, volume.AnnStorageProvisioner, volume.AnnBetaStorageProvisioner),
			expectedEvents:  []string{"Warning ProvisioningFailed"},
			errors:          noerrors,
			test:            wrapTestWithProvisionCalls([]provisionCall{provision1Error}, testSyncClaim),
		},
		{
			// No provisioning if there is a matching volume available
			name:            "11-6 - provisioning when there is a volume available",
			initialVolumes:  newVolumeArray("volume11-6", "1Gi", "", "", v1.VolumeAvailable, v1.PersistentVolumeReclaimRetain, classGold),
			expectedVolumes: newVolumeArray("volume11-6", "1Gi", "uid11-6", "claim11-6", v1.VolumeBound, v1.PersistentVolumeReclaimRetain, classGold, volume.AnnBoundByController),
			initialClaims:   newClaimArray("claim11-6", "uid11-6", "1Gi", "", v1.ClaimPending, &classGold),
			expectedClaims:  newClaimArray("claim11-6", "uid11-6", "1Gi", "volume11-6", v1.ClaimBound, &classGold, volume.AnnBoundByController, volume.AnnBindCompleted),
			expectedEvents:  noevents,
			errors:          noerrors,
			// No provisioning plugin confingure - makes the test fail when
			// the controller erroneously tries to provision something
			test: wrapTestWithProvisionCalls([]provisionCall{provision1Success}, testSyncClaim),
		},
		{
			// Provision success? - claim is bound before provisioner creates
			// a volume.
			name:            "11-7 - claim is bound before provisioning",
			initialVolumes:  novolumes,
			expectedVolumes: newVolumeArray("pvc-uid11-7", "1Gi", "uid11-7", "claim11-7", v1.VolumeBound, v1.PersistentVolumeReclaimDelete, classGold, volume.AnnBoundByController, volume.AnnDynamicallyProvisioned),
			initialClaims:   newClaimArray("claim11-7", "uid11-7", "1Gi", "", v1.ClaimPending, &classGold),
			// The claim would be bound in next syncClaim
			expectedClaims: newClaimArray("claim11-7", "uid11-7", "1Gi", "", v1.ClaimPending, &classGold, volume.AnnStorageProvisioner, volume.AnnBetaStorageProvisioner),
			expectedEvents: noevents,
			errors:         noerrors,
			test: wrapTestWithInjectedOperation(ctx, wrapTestWithProvisionCalls([]provisionCall{}, testSyncClaim), func(ctrl *PersistentVolumeController, reactor *pvtesting.VolumeReactor) {
				// Create a volume before provisionClaimOperation starts.
				// This similates a parallel controller provisioning the volume.
				volume := newVolume("pvc-uid11-7", "1Gi", "uid11-7", "claim11-7", v1.VolumeBound, v1.PersistentVolumeReclaimDelete, classGold, volume.AnnBoundByController, volume.AnnDynamicallyProvisioned)
				reactor.AddVolume(volume)
			}),
		},
		{
			// Provision success - cannot save provisioned PV once,
			// second retry succeeds
			name:            "11-8 - cannot save provisioned volume",
			initialVolumes:  novolumes,
			expectedVolumes: volumesWithFinalizers(newVolumeArray("pvc-uid11-8", "1Gi", "uid11-8", "claim11-8", v1.VolumeBound, v1.PersistentVolumeReclaimDelete, classGold, volume.AnnBoundByController, volume.AnnDynamicallyProvisioned), []string{volume.PVDeletionInTreeProtectionFinalizer}),
			initialClaims:   newClaimArray("claim11-8", "uid11-8", "1Gi", "", v1.ClaimPending, &classGold),
			// Binding will be completed in the next syncClaim
			expectedClaims: newClaimArray("claim11-8", "uid11-8", "1Gi", "", v1.ClaimPending, &classGold, volume.AnnStorageProvisioner, volume.AnnBetaStorageProvisioner),
			expectedEvents: []string{"Normal ProvisioningSucceeded"},
			errors: []pvtesting.ReactorError{
				// Inject error to the first
				// kubeclient.PersistentVolumes.Create() call. All other calls
				// will succeed.
				{Verb: "create", Resource: "persistentvolumes", Error: errors.New("Mock creation error")},
			},
			test: wrapTestWithProvisionCalls([]provisionCall{provision1Success}, testSyncClaim),
		},
		{
			// Provision success? - cannot save provisioned PV five times,
			// volume is deleted and delete succeeds
			name:            "11-9 - cannot save provisioned volume, delete succeeds",
			initialVolumes:  novolumes,
			expectedVolumes: novolumes,
			initialClaims:   newClaimArray("claim11-9", "uid11-9", "1Gi", "", v1.ClaimPending, &classGold),
			expectedClaims:  newClaimArray("claim11-9", "uid11-9", "1Gi", "", v1.ClaimPending, &classGold, volume.AnnStorageProvisioner, volume.AnnBetaStorageProvisioner),
			expectedEvents:  []string{"Warning ProvisioningFailed"},
			errors: []pvtesting.ReactorError{
				// Inject error to five kubeclient.PersistentVolumes.Create()
				// calls
				{Verb: "create", Resource: "persistentvolumes", Error: errors.New("Mock creation error1")},
				{Verb: "create", Resource: "persistentvolumes", Error: errors.New("Mock creation error2")},
				{Verb: "create", Resource: "persistentvolumes", Error: errors.New("Mock creation error3")},
				{Verb: "create", Resource: "persistentvolumes", Error: errors.New("Mock creation error4")},
				{Verb: "create", Resource: "persistentvolumes", Error: errors.New("Mock creation error5")},
			},
			test: wrapTestWithPluginCalls(
				nil,                                // recycle calls
				[]error{nil},                       // delete calls
				[]provisionCall{provision1Success}, // provision calls
				testSyncClaim,
			),
		},
		{
			// Provision failure - cannot save provisioned PV five times,
			// volume delete failed - no plugin found
			name:            "11-10 - cannot save provisioned volume, no delete plugin found",
			initialVolumes:  novolumes,
			expectedVolumes: novolumes,
			initialClaims:   newClaimArray("claim11-10", "uid11-10", "1Gi", "", v1.ClaimPending, &classGold),
			expectedClaims:  newClaimArray("claim11-10", "uid11-10", "1Gi", "", v1.ClaimPending, &classGold, volume.AnnStorageProvisioner, volume.AnnBetaStorageProvisioner),
			expectedEvents:  []string{"Warning ProvisioningFailed", "Warning ProvisioningCleanupFailed"},
			errors: []pvtesting.ReactorError{
				// Inject error to five kubeclient.PersistentVolumes.Create()
				// calls
				{Verb: "create", Resource: "persistentvolumes", Error: errors.New("Mock creation error1")},
				{Verb: "create", Resource: "persistentvolumes", Error: errors.New("Mock creation error2")},
				{Verb: "create", Resource: "persistentvolumes", Error: errors.New("Mock creation error3")},
				{Verb: "create", Resource: "persistentvolumes", Error: errors.New("Mock creation error4")},
				{Verb: "create", Resource: "persistentvolumes", Error: errors.New("Mock creation error5")},
			},
			// No deleteCalls are configured, which results into no deleter plugin available for the volume
			test: wrapTestWithProvisionCalls([]provisionCall{provision1Success}, testSyncClaim),
		},
		{
			// Provision failure - cannot save provisioned PV five times,
			// volume delete failed - deleter returns error five times
			name:            "11-11 - cannot save provisioned volume, deleter fails",
			initialVolumes:  novolumes,
			expectedVolumes: novolumes,
			initialClaims:   newClaimArray("claim11-11", "uid11-11", "1Gi", "", v1.ClaimPending, &classGold),
			expectedClaims:  newClaimArray("claim11-11", "uid11-11", "1Gi", "", v1.ClaimPending, &classGold, volume.AnnStorageProvisioner, volume.AnnBetaStorageProvisioner),
			expectedEvents:  []string{"Warning ProvisioningFailed", "Warning ProvisioningCleanupFailed"},
			errors: []pvtesting.ReactorError{
				// Inject error to five kubeclient.PersistentVolumes.Create()
				// calls
				{Verb: "create", Resource: "persistentvolumes", Error: errors.New("Mock creation error1")},
				{Verb: "create", Resource: "persistentvolumes", Error: errors.New("Mock creation error2")},
				{Verb: "create", Resource: "persistentvolumes", Error: errors.New("Mock creation error3")},
				{Verb: "create", Resource: "persistentvolumes", Error: errors.New("Mock creation error4")},
				{Verb: "create", Resource: "persistentvolumes", Error: errors.New("Mock creation error5")},
			},
			test: wrapTestWithPluginCalls(
				nil, // recycle calls
				[]error{ // delete calls
					errors.New("Mock deletion error1"),
					errors.New("Mock deletion error2"),
					errors.New("Mock deletion error3"),
					errors.New("Mock deletion error4"),
					errors.New("Mock deletion error5"),
				},
				[]provisionCall{provision1Success}, // provision calls
				testSyncClaim),
		},
		{
			// Provision failure - cannot save provisioned PV five times,
			// volume delete succeeds 2nd time
			name:            "11-12 - cannot save provisioned volume, delete succeeds 2nd time",
			initialVolumes:  novolumes,
			expectedVolumes: novolumes,
			initialClaims:   newClaimArray("claim11-12", "uid11-12", "1Gi", "", v1.ClaimPending, &classGold),
			expectedClaims:  newClaimArray("claim11-12", "uid11-12", "1Gi", "", v1.ClaimPending, &classGold, volume.AnnStorageProvisioner, volume.AnnBetaStorageProvisioner),
			expectedEvents:  []string{"Warning ProvisioningFailed"},
			errors: []pvtesting.ReactorError{
				// Inject error to five kubeclient.PersistentVolumes.Create()
				// calls
				{Verb: "create", Resource: "persistentvolumes", Error: errors.New("Mock creation error1")},
				{Verb: "create", Resource: "persistentvolumes", Error: errors.New("Mock creation error2")},
				{Verb: "create", Resource: "persistentvolumes", Error: errors.New("Mock creation error3")},
				{Verb: "create", Resource: "persistentvolumes", Error: errors.New("Mock creation error4")},
				{Verb: "create", Resource: "persistentvolumes", Error: errors.New("Mock creation error5")},
			},
			test: wrapTestWithPluginCalls(
				nil, // recycle calls
				[]error{ // delete calls
					errors.New("Mock deletion error1"),
					nil,
				}, //  provison calls
				[]provisionCall{provision1Success},
				testSyncClaim,
			),
		},
		{
			// Provision a volume (with non-default class)
			name:            "11-13 - successful provision with storage class 2",
			initialVolumes:  novolumes,
			expectedVolumes: volumesWithFinalizers(newVolumeArray("pvc-uid11-13", "1Gi", "uid11-13", "claim11-13", v1.VolumeBound, v1.PersistentVolumeReclaimDelete, classSilver, volume.AnnBoundByController, volume.AnnDynamicallyProvisioned), []string{volume.PVDeletionInTreeProtectionFinalizer}),
			initialClaims:   newClaimArray("claim11-13", "uid11-13", "1Gi", "", v1.ClaimPending, &classSilver),
			// Binding will be completed in the next syncClaim
			expectedClaims: newClaimArray("claim11-13", "uid11-13", "1Gi", "", v1.ClaimPending, &classSilver, volume.AnnStorageProvisioner, volume.AnnBetaStorageProvisioner),
			expectedEvents: []string{"Normal ProvisioningSucceeded"},
			errors:         noerrors,
			test:           wrapTestWithProvisionCalls([]provisionCall{provision2Success}, testSyncClaim),
		},
		{
			// Provision error - non existing class
			name:            "11-14 - fail due to non-existing class",
			initialVolumes:  novolumes,
			expectedVolumes: novolumes,
			initialClaims:   newClaimArray("claim11-14", "uid11-14", "1Gi", "", v1.ClaimPending, &classNonExisting),
			expectedClaims:  newClaimArray("claim11-14", "uid11-14", "1Gi", "", v1.ClaimPending, &classNonExisting),
			expectedEvents:  noevents,
			errors:          noerrors,
			test:            wrapTestWithProvisionCalls([]provisionCall{}, testSyncClaim),
		},
		{
			// No provisioning with class=""
			name:            "11-15 - no provisioning with class=''",
			initialVolumes:  novolumes,
			expectedVolumes: novolumes,
			initialClaims:   newClaimArray("claim11-15", "uid11-15", "1Gi", "", v1.ClaimPending, &classEmpty),
			expectedClaims:  newClaimArray("claim11-15", "uid11-15", "1Gi", "", v1.ClaimPending, &classEmpty),
			expectedEvents:  noevents,
			errors:          noerrors,
			test:            wrapTestWithProvisionCalls([]provisionCall{}, testSyncClaim),
		},
		{
			// No provisioning with class=nil
			name:            "11-16 - no provisioning with class=nil",
			initialVolumes:  novolumes,
			expectedVolumes: novolumes,
			initialClaims:   newClaimArray("claim11-15", "uid11-15", "1Gi", "", v1.ClaimPending, nil),
			expectedClaims:  newClaimArray("claim11-15", "uid11-15", "1Gi", "", v1.ClaimPending, nil),
			expectedEvents:  noevents,
			errors:          noerrors,
			test:            wrapTestWithProvisionCalls([]provisionCall{}, testSyncClaim),
		},
		{
			// No provisioning + normal event with external provisioner
			name:            "11-17 - external provisioner",
			initialVolumes:  novolumes,
			expectedVolumes: novolumes,
			initialClaims:   newClaimArray("claim11-17", "uid11-17", "1Gi", "", v1.ClaimPending, &classExternal),
			expectedClaims: claimWithAnnotation(volume.AnnBetaStorageProvisioner, "vendor.com/my-volume",
				claimWithAnnotation(volume.AnnStorageProvisioner, "vendor.com/my-volume",
					newClaimArray("claim11-17", "uid11-17", "1Gi", "", v1.ClaimPending, &classExternal))),
			expectedEvents: []string{"Normal ExternalProvisioning"},
			errors:         noerrors,
			test:           wrapTestWithProvisionCalls([]provisionCall{}, testSyncClaim),
		},
		{
			// No provisioning + warning event with unknown internal provisioner
			name:            "11-18 - unknown internal provisioner",
			initialVolumes:  novolumes,
			expectedVolumes: novolumes,
			initialClaims:   newClaimArray("claim11-18", "uid11-18", "1Gi", "", v1.ClaimPending, &classUnknownInternal),
			expectedClaims:  newClaimArray("claim11-18", "uid11-18", "1Gi", "", v1.ClaimPending, &classUnknownInternal),
			expectedEvents:  []string{"Warning ProvisioningFailed"},
			errors:          noerrors,
			test:            wrapTestWithProvisionCalls([]provisionCall{}, testSyncClaim),
		},
		{
			// Provision success - first save of a PV to API server fails (API
			// server has written the object to etcd, but crashed before sending
			// 200 OK response to the controller). Controller retries and the
			// second save of the PV returns "AlreadyExists" because the PV
			// object already is in the API server.
			//
			"11-19 - provisioned volume saved but API server crashed",
			novolumes,
			// We don't actually simulate API server saving the object and
			// crashing afterwards, Create() just returns error without saving
			// the volume in this test. So the set of expected volumes at the
			// end of the test is empty.
			novolumes,
			newClaimArray("claim11-19", "uid11-19", "1Gi", "", v1.ClaimPending, &classGold),
			newClaimArray("claim11-19", "uid11-19", "1Gi", "", v1.ClaimPending, &classGold, volume.AnnStorageProvisioner, volume.AnnBetaStorageProvisioner),
			noevents,
			[]pvtesting.ReactorError{
				// Inject errors to simulate crashed API server during
				// kubeclient.PersistentVolumes.Create()
				{Verb: "create", Resource: "persistentvolumes", Error: errors.New("Mock creation error1")},
				{Verb: "create", Resource: "persistentvolumes", Error: apierrors.NewAlreadyExists(api.Resource("persistentvolumes"), "")},
			},
			wrapTestWithPluginCalls(
				nil, // recycle calls
				nil, // delete calls - if Delete was called the test would fail
				[]provisionCall{provision1Success},
				testSyncClaim,
			),
		},
		{
			// No provisioning + warning event with unsupported storageClass.mountOptions
			name:            "11-20 - unsupported storageClass.mountOptions",
			initialVolumes:  novolumes,
			expectedVolumes: novolumes,
			initialClaims:   newClaimArray("claim11-20", "uid11-20", "1Gi", "", v1.ClaimPending, &classUnsupportedMountOptions),
			expectedClaims:  newClaimArray("claim11-20", "uid11-20", "1Gi", "", v1.ClaimPending, &classUnsupportedMountOptions, volume.AnnStorageProvisioner, volume.AnnBetaStorageProvisioner),
			// Expect event to be prefixed with "Mount options" because saving PV will fail anyway
			expectedEvents: []string{"Warning ProvisioningFailed Mount options"},
			errors:         noerrors,
			test:           wrapTestWithProvisionCalls([]provisionCall{}, testSyncClaim),
		},
		{
			// No provisioning due to CSI migration + normal event with external provisioner
			name:            "11-21 - external provisioner for CSI migration",
			initialVolumes:  novolumes,
			expectedVolumes: novolumes,
			initialClaims:   newClaimArray("claim11-21", "uid11-21", "1Gi", "", v1.ClaimPending, &classGold),
			expectedClaims: []*v1.PersistentVolumeClaim{
				annotateClaim(
					newClaim("claim11-21", "uid11-21", "1Gi", "", v1.ClaimPending, &classGold),
					map[string]string{
						volume.AnnStorageProvisioner:     "vendor.com/MockCSIDriver",
						volume.AnnBetaStorageProvisioner: "vendor.com/MockCSIDriver",
						volume.AnnMigratedTo:             "vendor.com/MockCSIDriver",
					}),
			},
			expectedEvents: []string{"Normal ExternalProvisioning"},
			errors:         noerrors,
			test:           wrapTestWithCSIMigrationProvisionCalls(testSyncClaim),
		},
		{
			// volume provisioned and available
			// in this case, NO normal event with external provisioner should be issued
			name:            "11-22 - external provisioner with volume available",
			initialVolumes:  newVolumeArray("volume11-22", "1Gi", "", "", v1.VolumeAvailable, v1.PersistentVolumeReclaimRetain, classExternal),
			expectedVolumes: newVolumeArray("volume11-22", "1Gi", "uid11-22", "claim11-22", v1.VolumeBound, v1.PersistentVolumeReclaimRetain, classExternal, volume.AnnBoundByController),
			initialClaims:   newClaimArray("claim11-22", "uid11-22", "1Gi", "", v1.ClaimPending, &classExternal),
			expectedClaims:  newClaimArray("claim11-22", "uid11-22", "1Gi", "volume11-22", v1.ClaimBound, &classExternal, volume.AnnBoundByController, volume.AnnBindCompleted),
			expectedEvents:  noevents,
			errors:          noerrors,
			test:            wrapTestWithProvisionCalls([]provisionCall{}, testSyncClaim),
		},
		{
			// volume provision for PVC scheduled
			"11-23 - skip finding PV and provision for PVC annotated with AnnSelectedNode",
			newVolumeArray("volume11-23", "1Gi", "", "", v1.VolumeAvailable, v1.PersistentVolumeReclaimDelete, classCopper),
			[]*v1.PersistentVolume{
				newVolumeWithFinalizers("volume11-23", "1Gi", "", "", v1.VolumeAvailable, v1.PersistentVolumeReclaimDelete, classCopper, nil /*No Finalizer is added here since the test doesn't trigger syncVolume, instead just syncClaim*/),
				newVolumeWithFinalizers("pvc-uid11-23", "1Gi", "uid11-23", "claim11-23", v1.VolumeBound, v1.PersistentVolumeReclaimDelete, classCopper, []string{volume.PVDeletionInTreeProtectionFinalizer}, volume.AnnDynamicallyProvisioned, volume.AnnBoundByController),
			},
			claimWithAnnotation(volume.AnnSelectedNode, "node1",
				newClaimArray("claim11-23", "uid11-23", "1Gi", "", v1.ClaimPending, &classCopper)),
			claimWithAnnotation(volume.AnnSelectedNode, "node1",
				newClaimArray("claim11-23", "uid11-23", "1Gi", "", v1.ClaimPending, &classCopper, volume.AnnStorageProvisioner, volume.AnnBetaStorageProvisioner)),
			[]string{"Normal ProvisioningSucceeded"},
			noerrors,
			wrapTestWithInjectedOperation(ctx, wrapTestWithProvisionCalls([]provisionCall{provision1Success}, testSyncClaim),
				func(ctrl *PersistentVolumeController, reactor *pvtesting.VolumeReactor) {
					nodesIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
					node := &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node1"}}
					nodesIndexer.Add(node)
					ctrl.NodeLister = corelisters.NewNodeLister(nodesIndexer)
				}),
		},
		{
			// volume provision for PVC that scheduled
			name:            "11-24 - skip finding PV and wait external provisioner for PVC annotated with AnnSelectedNode",
			initialVolumes:  newVolumeArray("volume11-24", "1Gi", "", "", v1.VolumeAvailable, v1.PersistentVolumeReclaimDelete, classExternalWait),
			expectedVolumes: newVolumeArray("volume11-24", "1Gi", "", "", v1.VolumeAvailable, v1.PersistentVolumeReclaimDelete, classExternalWait),
			initialClaims: claimWithAnnotation(volume.AnnSelectedNode, "node1",
				newClaimArray("claim11-24", "uid11-24", "1Gi", "", v1.ClaimPending, &classExternalWait)),
			expectedClaims: claimWithAnnotation(volume.AnnBetaStorageProvisioner, "vendor.com/my-volume-wait",
				claimWithAnnotation(volume.AnnStorageProvisioner, "vendor.com/my-volume-wait",
					claimWithAnnotation(volume.AnnSelectedNode, "node1",
						newClaimArray("claim11-24", "uid11-24", "1Gi", "", v1.ClaimPending, &classExternalWait)))),
			expectedEvents: []string{"Normal ExternalProvisioning"},
			errors:         noerrors,
			test:           testSyncClaim,
		},
		{
			// Provision a volume with a data source will fail
			// for in-tree plugins
			name:            "11-25 - failed in-tree provision with data source",
			initialVolumes:  novolumes,
			expectedVolumes: novolumes,
			initialClaims:   claimWithDataSource("test-snap", "VolumeSnapshot", "snapshot.storage.k8s.io", newClaimArray("claim11-25", "uid11-25", "1Gi", "", v1.ClaimPending, &classGold)),
			expectedClaims:  claimWithDataSource("test-snap", "VolumeSnapshot", "snapshot.storage.k8s.io", newClaimArray("claim11-25", "uid11-25", "1Gi", "", v1.ClaimPending, &classGold)),
			expectedEvents:  []string{"Warning ProvisioningFailed"},
			errors:          noerrors,
			test:            testSyncClaim,
		},
		{
			// Provision a volume with a data source will proceed
			// for CSI plugins
			"11-26 - csi with data source",
			novolumes,
			novolumes,
			claimWithAnnotation(volume.AnnStorageProvisioner, "mydriver.csi.k8s.io",
				claimWithDataSource("test-snap", "VolumeSnapshot", "snapshot.storage.k8s.io", newClaimArray("claim11-26", "uid11-26", "1Gi", "", v1.ClaimPending, &classCSI))),
			claimWithAnnotation(volume.AnnStorageProvisioner, "mydriver.csi.k8s.io",
				claimWithDataSource("test-snap", "VolumeSnapshot", "snapshot.storage.k8s.io", newClaimArray("claim11-26", "uid11-26", "1Gi", "", v1.ClaimPending, &classCSI))),
			[]string{"Normal ExternalProvisioning"},
			noerrors,
			wrapTestWithProvisionCalls([]provisionCall{}, testSyncClaim),
		},
	}
	runSyncTests(t, ctx, tests, storageClasses, []*v1.Pod{})
}

// Test multiple calls to syncClaim/syncVolume and periodic sync of all
// volume/claims. The test follows this pattern:
//  0. Load the controller with initial data.
//  1. Call controllerTest.testCall() once as in TestSync()
//  2. For all volumes/claims changed by previous syncVolume/syncClaim calls,
//     call appropriate syncVolume/syncClaim (simulating "volume/claim changed"
//     events). Go to 2. if these calls change anything.
//  3. When all changes are processed and no new changes were made, call
//     syncVolume/syncClaim on all volumes/claims (simulating "periodic sync").
//  4. If some changes were done by step 3., go to 2. (simulation of
//     "volume/claim updated" events, eventually performing step 3. again)
//  5. When 3. does not do any changes, finish the tests and compare final set
//     of volumes/claims with expected claims/volumes and report differences.
//
// Some limit of calls in enforced to prevent endless loops.
func TestProvisionMultiSync(t *testing.T) {
	// Default enable the HonorPVReclaimPolicy feature gate.
	// TODO: this will be removed in 1.36
	featuregatetesting.SetFeatureGateEmulationVersionDuringTest(t, utilfeature.DefaultFeatureGate, version.MustParse("1.32"))
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.HonorPVReclaimPolicy, true)

	_, ctx := ktesting.NewTestContext(t)
	tests := []controllerTest{
		{
			// Provision a volume with binding
			name:            "12-1 - successful provision",
			initialVolumes:  novolumes,
			expectedVolumes: volumesWithFinalizers(newVolumeArray("pvc-uid12-1", "1Gi", "uid12-1", "claim12-1", v1.VolumeBound, v1.PersistentVolumeReclaimDelete, classGold, volume.AnnBoundByController, volume.AnnDynamicallyProvisioned), []string{volume.PVDeletionInTreeProtectionFinalizer}),
			initialClaims:   newClaimArray("claim12-1", "uid12-1", "1Gi", "", v1.ClaimPending, &classGold),
			expectedClaims:  newClaimArray("claim12-1", "uid12-1", "1Gi", "pvc-uid12-1", v1.ClaimBound, &classGold, volume.AnnBoundByController, volume.AnnBindCompleted, volume.AnnStorageProvisioner, volume.AnnBetaStorageProvisioner),
			expectedEvents:  noevents,
			errors:          noerrors,
			test:            wrapTestWithProvisionCalls([]provisionCall{provision1Success}, testSyncClaim),
		},
		{
			// provision a volume (external provisioner) and binding + normal event with external provisioner
			name:            "12-2 - external provisioner with volume provisioned success",
			initialVolumes:  novolumes,
			expectedVolumes: newVolumeArray("pvc-uid12-2", "1Gi", "uid12-2", "claim12-2", v1.VolumeBound, v1.PersistentVolumeReclaimRetain, classExternal, volume.AnnBoundByController),
			initialClaims:   newClaimArray("claim12-2", "uid12-2", "1Gi", "", v1.ClaimPending, &classExternal),
			expectedClaims: claimWithAnnotation(volume.AnnBetaStorageProvisioner, "vendor.com/my-volume",
				claimWithAnnotation(volume.AnnStorageProvisioner, "vendor.com/my-volume",
					newClaimArray("claim12-2", "uid12-2", "1Gi", "pvc-uid12-2", v1.ClaimBound, &classExternal, volume.AnnBoundByController, volume.AnnBindCompleted))),
			expectedEvents: []string{"Normal ExternalProvisioning"},
			errors:         noerrors,
			test: wrapTestWithInjectedOperation(ctx, wrapTestWithProvisionCalls([]provisionCall{}, testSyncClaim), func(ctrl *PersistentVolumeController, reactor *pvtesting.VolumeReactor) {
				// Create a volume before syncClaim tries to bind a PV to PVC
				// This simulates external provisioner creating a volume while the controller
				// is waiting for a volume to bind to the existed claim
				// the external provisioner workflow implemented in "provisionClaimOperationCSI"
				// should issue an ExternalProvisioning event to signal that some external provisioner
				// is working on provisioning the PV, also add the operation start timestamp into local cache
				// operationTimestamps. Rely on the existences of the start time stamp to create a PV for binding
				if ctrl.operationTimestamps.Has("default/claim12-2") {
					volume := newVolume("pvc-uid12-2", "1Gi", "", "", v1.VolumeAvailable, v1.PersistentVolumeReclaimRetain, classExternal)
					ctrl.volumes.store.Add(volume) // add the volume to controller
					reactor.AddVolume(volume)
				}
			}),
		},
		{
			// provision a volume (external provisioner) but binding will not happen + normal event with external provisioner
			name:            "12-3 - external provisioner with volume to be provisioned",
			initialVolumes:  novolumes,
			expectedVolumes: novolumes,
			initialClaims:   newClaimArray("claim12-3", "uid12-3", "1Gi", "", v1.ClaimPending, &classExternal),
			expectedClaims: claimWithAnnotation(volume.AnnBetaStorageProvisioner, "vendor.com/my-volume",
				claimWithAnnotation(volume.AnnStorageProvisioner, "vendor.com/my-volume",
					newClaimArray("claim12-3", "uid12-3", "1Gi", "", v1.ClaimPending, &classExternal))),
			expectedEvents: []string{"Normal ExternalProvisioning"},
			errors:         noerrors,
			test:           wrapTestWithProvisionCalls([]provisionCall{provision1Success}, testSyncClaim),
		},
		{
			// provision a volume (external provisioner) and binding + normal event with external provisioner
			name:            "12-4 - external provisioner with volume provisioned/bound success",
			initialVolumes:  novolumes,
			expectedVolumes: newVolumeArray("pvc-uid12-4", "1Gi", "uid12-4", "claim12-4", v1.VolumeBound, v1.PersistentVolumeReclaimRetain, classExternal, volume.AnnBoundByController),
			initialClaims:   newClaimArray("claim12-4", "uid12-4", "1Gi", "", v1.ClaimPending, &classExternal),
			expectedClaims: claimWithAnnotation(volume.AnnBetaStorageProvisioner, "vendor.com/my-volume",
				claimWithAnnotation(volume.AnnStorageProvisioner, "vendor.com/my-volume",
					newClaimArray("claim12-4", "uid12-4", "1Gi", "pvc-uid12-4", v1.ClaimBound, &classExternal, volume.AnnBoundByController, volume.AnnBindCompleted))),
			expectedEvents: []string{"Normal ExternalProvisioning"},
			errors:         noerrors,
			test: wrapTestWithInjectedOperation(ctx, wrapTestWithProvisionCalls([]provisionCall{}, testSyncClaim), func(ctrl *PersistentVolumeController, reactor *pvtesting.VolumeReactor) {
				// Create a volume before syncClaim tries to bind a PV to PVC
				// This simulates external provisioner creating a volume while the controller
				// is waiting for a volume to bind to the existed claim
				// the external provisioner workflow implemented in "provisionClaimOperationCSI"
				// should issue an ExternalProvisioning event to signal that some external provisioner
				// is working on provisioning the PV, also add the operation start timestamp into local cache
				// operationTimestamps. Rely on the existences of the start time stamp to create a PV for binding
				if ctrl.operationTimestamps.Has("default/claim12-4") {
					volume := newVolume("pvc-uid12-4", "1Gi", "uid12-4", "claim12-4", v1.VolumeBound, v1.PersistentVolumeReclaimRetain, classExternal, volume.AnnBoundByController)
					ctrl.volumes.store.Add(volume) // add the volume to controller
					reactor.AddVolume(volume)
				}
			}),
		},
	}

	runMultisyncTests(t, ctx, tests, storageClasses, storageClasses[0].Name)
}

// When provisioning is disabled, provisioning a claim should instantly return nil
func TestDisablingDynamicProvisioner(t *testing.T) {
	_, ctx := ktesting.NewTestContext(t)
	ctrl, err := newTestController(ctx, nil, nil, false)
	if err != nil {
		t.Fatalf("Construct PersistentVolume controller failed: %v", err)
	}
	retVal := ctrl.provisionClaim(ctx, nil)
	if retVal != nil {
		t.Errorf("Expected nil return but got %v", retVal)
	}
}
