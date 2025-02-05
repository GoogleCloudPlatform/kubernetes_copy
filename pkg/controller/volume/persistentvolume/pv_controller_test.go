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
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	storagelisters "k8s.io/client-go/listers/storage/v1"
	core "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	"k8s.io/component-helpers/storage/volume"
	csitrans "k8s.io/csi-translation-lib"
	"k8s.io/klog/v2/ktesting"
	"k8s.io/kubernetes/pkg/controller"
	pvtesting "k8s.io/kubernetes/pkg/controller/volume/persistentvolume/testing"
	"k8s.io/kubernetes/pkg/features"
	"k8s.io/kubernetes/pkg/volume/csimigration"
	"k8s.io/kubernetes/pkg/volume/util"
)

// Test the real controller methods (add/update/delete claim/volume) with
// a fake API server.
// There is no controller API to 'initiate syncAll now', therefore these tests
// can't reliably simulate periodic sync of volumes/claims - it would be
// either very timing-sensitive or slow to wait for real periodic sync.
func TestControllerSync(t *testing.T) {
	// Default enable the HonorPVReclaimPolicy feature gate.
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.HonorPVReclaimPolicy, true)
	tests := []controllerTest{
		// [Unit test set 5] - controller tests.
		// We test the controller as if
		// it was connected to real API server, i.e. we call add/update/delete
		// Claim/Volume methods. Also, all changes to volumes and claims are
		// sent to add/update/delete Claim/Volume as real controller would do.
		{
			// addClaim gets a new claim. Check it's bound to a volume.
			name:            "5-2 - complete bind",
			initialVolumes:  newVolumeArray("volume5-2", "1Gi", "", "", v1.VolumeAvailable, v1.PersistentVolumeReclaimRetain, classEmpty),
			expectedVolumes: newVolumeArray("volume5-2", "1Gi", "uid5-2", "claim5-2", v1.VolumeBound, v1.PersistentVolumeReclaimRetain, classEmpty, volume.AnnBoundByController),
			initialClaims:   noclaims, /* added in testAddClaim5_2 */
			expectedClaims:  newClaimArray("claim5-2", "uid5-2", "1Gi", "volume5-2", v1.ClaimBound, nil, volume.AnnBoundByController, volume.AnnBindCompleted),
			expectedEvents:  noevents,
			errors:          noerrors,
			// Custom test function that generates an add event
			test: func(ctrl *PersistentVolumeController, reactor *pvtesting.VolumeReactor, test controllerTest) error {
				claim := newClaim("claim5-2", "uid5-2", "1Gi", "", v1.ClaimPending, nil)
				reactor.AddClaimEvent(claim)
				return nil
			},
		},
		{
			name:            "5-2-2 - complete bind when PV and PVC both exist",
			initialVolumes:  newVolumeArray("volume5-2", "1Gi", "", "", v1.VolumeAvailable, v1.PersistentVolumeReclaimRetain, classEmpty),
			expectedVolumes: newVolumeArray("volume5-2", "1Gi", "uid5-2", "claim5-2", v1.VolumeBound, v1.PersistentVolumeReclaimRetain, classEmpty, volume.AnnBoundByController),
			initialClaims:   newClaimArray("claim5-2", "uid5-2", "1Gi", "", v1.ClaimPending, nil),
			expectedClaims:  newClaimArray("claim5-2", "uid5-2", "1Gi", "volume5-2", v1.ClaimBound, nil, volume.AnnBoundByController, volume.AnnBindCompleted),
			expectedEvents:  noevents,
			errors:          noerrors,
			test: func(ctrl *PersistentVolumeController, reactor *pvtesting.VolumeReactor, test controllerTest) error {
				return nil
			},
		},
		{
			name:            "5-2-3 - complete bind when PV and PVC both exist and PV has AnnPreResizeCapacity annotation",
			initialVolumes:  volumesWithAnnotation(util.AnnPreResizeCapacity, "1Gi", newVolumeArray("volume5-2", "2Gi", "", "", v1.VolumeAvailable, v1.PersistentVolumeReclaimRetain, classEmpty, volume.AnnBoundByController)),
			expectedVolumes: volumesWithAnnotation(util.AnnPreResizeCapacity, "1Gi", newVolumeArray("volume5-2", "2Gi", "uid5-2", "claim5-2", v1.VolumeBound, v1.PersistentVolumeReclaimRetain, classEmpty, volume.AnnBoundByController)),
			initialClaims:   withExpectedCapacity("2Gi", newClaimArray("claim5-2", "uid5-2", "2Gi", "", v1.ClaimPending, nil)),
			expectedClaims:  withExpectedCapacity("1Gi", newClaimArray("claim5-2", "uid5-2", "2Gi", "volume5-2", v1.ClaimBound, nil, volume.AnnBoundByController, volume.AnnBindCompleted)),
			expectedEvents:  noevents,
			errors:          noerrors,
			test: func(ctrl *PersistentVolumeController, reactor *pvtesting.VolumeReactor, test controllerTest) error {
				return nil
			},
		},
		{
			// deleteClaim with a bound claim makes bound volume released.
			name:            "5-3 - delete claim",
			initialVolumes:  newVolumeArray("volume5-3", "10Gi", "uid5-3", "claim5-3", v1.VolumeBound, v1.PersistentVolumeReclaimRetain, classEmpty, volume.AnnBoundByController),
			expectedVolumes: newVolumeArray("volume5-3", "10Gi", "uid5-3", "claim5-3", v1.VolumeReleased, v1.PersistentVolumeReclaimRetain, classEmpty, volume.AnnBoundByController),
			initialClaims:   newClaimArray("claim5-3", "uid5-3", "1Gi", "volume5-3", v1.ClaimBound, nil, volume.AnnBoundByController, volume.AnnBindCompleted),
			expectedClaims:  noclaims,
			expectedEvents:  noevents,
			errors:          noerrors,
			// Custom test function that generates a delete event
			test: func(ctrl *PersistentVolumeController, reactor *pvtesting.VolumeReactor, test controllerTest) error {
				obj := ctrl.claims.List()[0]
				claim := obj.(*v1.PersistentVolumeClaim)
				reactor.DeleteClaimEvent(claim)
				return nil
			},
		},
		{
			// deleteVolume with a bound volume. Check the claim is Lost.
			name:            "5-4 - delete volume",
			initialVolumes:  newVolumeArray("volume5-4", "1Gi", "uid5-4", "claim5-4", v1.VolumeBound, v1.PersistentVolumeReclaimRetain, classEmpty),
			expectedVolumes: novolumes,
			initialClaims:   newClaimArray("claim5-4", "uid5-4", "1Gi", "volume5-4", v1.ClaimBound, nil, volume.AnnBoundByController, volume.AnnBindCompleted),
			expectedClaims:  newClaimArray("claim5-4", "uid5-4", "1Gi", "volume5-4", v1.ClaimLost, nil, volume.AnnBoundByController, volume.AnnBindCompleted),
			expectedEvents:  []string{"Warning ClaimLost"},
			errors:          noerrors,
			// Custom test function that generates a delete event
			test: func(ctrl *PersistentVolumeController, reactor *pvtesting.VolumeReactor, test controllerTest) error {
				obj := ctrl.volumes.store.List()[0]
				volume := obj.(*v1.PersistentVolume)
				reactor.DeleteVolumeEvent(volume)
				return nil
			},
		},
		{
			// deleteClaim with a bound claim makes bound volume released with external deleter.
			// delete the corresponding volume from apiserver, and report latency metric
			name: "5-5 - delete claim and delete volume report metric",
			initialVolumes: volumesWithAnnotation(volume.AnnDynamicallyProvisioned, "gcr.io/vendor-csi",
				newVolumeArray("volume5-5", "10Gi", "uid5-5", "claim5-5", v1.VolumeBound, v1.PersistentVolumeReclaimDelete, classExternal, volume.AnnBoundByController)),
			expectedVolumes: novolumes,
			initialClaims: claimWithAnnotation(volume.AnnStorageProvisioner, "gcr.io/vendor-csi",
				newClaimArray("claim5-5", "uid5-5", "1Gi", "volume5-5", v1.ClaimBound, &classExternal, volume.AnnBoundByController, volume.AnnBindCompleted)),
			expectedClaims: noclaims,
			expectedEvents: noevents,
			errors:         noerrors,
			// Custom test function that generates a delete claim event which should have been caught by
			// "deleteClaim" to remove the claim from controller's cache, after that, a volume deleted
			// event will be generated to trigger "deleteVolume" call for metric reporting
			test: func(ctrl *PersistentVolumeController, reactor *pvtesting.VolumeReactor, test controllerTest) error {
				test.initialVolumes[0].Annotations[volume.AnnDynamicallyProvisioned] = "gcr.io/vendor-csi"
				obj := ctrl.claims.List()[0]
				claim := obj.(*v1.PersistentVolumeClaim)
				reactor.DeleteClaimEvent(claim)
				err := wait.Poll(10*time.Millisecond, wait.ForeverTestTimeout, func() (bool, error) {
					return len(ctrl.claims.ListKeys()) == 0, nil
				})
				if err != nil {
					return err
				}
				// claim has been removed from controller's cache, generate a volume deleted event
				volume := ctrl.volumes.store.List()[0].(*v1.PersistentVolume)
				reactor.DeleteVolumeEvent(volume)
				return nil
			},
		},
		{
			// deleteClaim with a bound claim makes bound volume released with external deleter pending
			// there should be an entry in operation timestamps cache in controller
			name:            "5-6 - delete claim and waiting for external volume deletion",
			initialVolumes:  volumesWithAnnotation(volume.AnnDynamicallyProvisioned, "gcr.io/vendor-csi", []*v1.PersistentVolume{newExternalProvisionedVolume("volume5-6", "10Gi", "uid5-6", "claim5-6", v1.VolumeBound, v1.PersistentVolumeReclaimDelete, classExternal, "fake.driver.csi", nil, volume.AnnBoundByController)}),
			expectedVolumes: volumesWithAnnotation(volume.AnnDynamicallyProvisioned, "gcr.io/vendor-csi", []*v1.PersistentVolume{newExternalProvisionedVolume("volume5-6", "10Gi", "uid5-6", "claim5-6", v1.VolumeReleased, v1.PersistentVolumeReclaimDelete, classExternal, "fake.driver.csi", nil, volume.AnnBoundByController)}),
			initialClaims: claimWithAnnotation(volume.AnnStorageProvisioner, "gcr.io/vendor-csi",
				newClaimArray("claim5-6", "uid5-6", "1Gi", "volume5-6", v1.ClaimBound, &classExternal, volume.AnnBoundByController, volume.AnnBindCompleted)),
			expectedClaims: noclaims,
			expectedEvents: noevents,
			errors:         noerrors,
			// Custom test function that generates a delete claim event which should have been caught by
			// "deleteClaim" to remove the claim from controller's cache and mark bound volume to be released
			test: func(ctrl *PersistentVolumeController, reactor *pvtesting.VolumeReactor, test controllerTest) error {
				// should have been provisioned by external provisioner
				obj := ctrl.claims.List()[0]
				claim := obj.(*v1.PersistentVolumeClaim)
				reactor.DeleteClaimEvent(claim)
				// wait until claim is cleared from cache, i.e., deleteClaim is called
				err := wait.Poll(10*time.Millisecond, wait.ForeverTestTimeout, func() (bool, error) {
					return len(ctrl.claims.ListKeys()) == 0, nil
				})
				if err != nil {
					return err
				}
				// wait for volume delete operation to appear once volumeWorker() runs
				return wait.PollImmediate(10*time.Millisecond, wait.ForeverTestTimeout, func() (bool, error) {
					// make sure the operation timestamp cache is NOT empty
					if ctrl.operationTimestamps.Has("volume5-6") {
						return true, nil
					}
					t.Logf("missing volume5-6 from timestamp cache, will retry")
					return false, nil
				})
			},
		},
		{
			// deleteVolume event issued before deleteClaim, no metric should have been reported
			// and no delete operation start timestamp should be inserted into controller.operationTimestamps cache
			name:            "5-7 - delete volume event makes claim lost, delete claim event will not report metric",
			initialVolumes:  newVolumeArray("volume5-7", "10Gi", "uid5-7", "claim5-7", v1.VolumeBound, v1.PersistentVolumeReclaimDelete, classExternal, volume.AnnBoundByController, volume.AnnDynamicallyProvisioned),
			expectedVolumes: novolumes,
			initialClaims: claimWithAnnotation(volume.AnnStorageProvisioner, "gcr.io/vendor-csi",
				newClaimArray("claim5-7", "uid5-7", "1Gi", "volume5-7", v1.ClaimBound, &classExternal, volume.AnnBoundByController, volume.AnnBindCompleted)),
			expectedClaims: noclaims,
			expectedEvents: []string{"Warning ClaimLost"},
			errors:         noerrors,
			// Custom test function that generates a delete claim event which should have been caught by
			// "deleteClaim" to remove the claim from controller's cache and mark bound volume to be released
			test: func(ctrl *PersistentVolumeController, reactor *pvtesting.VolumeReactor, test controllerTest) error {
				volume := ctrl.volumes.store.List()[0].(*v1.PersistentVolume)
				reactor.DeleteVolumeEvent(volume)
				err := wait.Poll(10*time.Millisecond, wait.ForeverTestTimeout, func() (bool, error) {
					return len(ctrl.volumes.store.ListKeys()) == 0, nil
				})
				if err != nil {
					return err
				}

				// Wait for the PVC to get fully processed. This avoids races between PV controller and DeleteClaimEvent
				// below.
				err = wait.Poll(10*time.Millisecond, wait.ForeverTestTimeout, func() (bool, error) {
					obj := ctrl.claims.List()[0]
					claim := obj.(*v1.PersistentVolumeClaim)
					return claim.Status.Phase == v1.ClaimLost, nil
				})
				if err != nil {
					return err
				}

				// trying to remove the claim as well
				obj := ctrl.claims.List()[0]
				claim := obj.(*v1.PersistentVolumeClaim)
				reactor.DeleteClaimEvent(claim)
				// wait until claim is cleared from cache, i.e., deleteClaim is called
				err = wait.Poll(10*time.Millisecond, wait.ForeverTestTimeout, func() (bool, error) {
					return len(ctrl.claims.ListKeys()) == 0, nil
				})
				if err != nil {
					return err
				}
				// make sure operation timestamp cache is empty
				if ctrl.operationTimestamps.Has("volume5-7") {
					return errors.New("failed checking timestamp cache")
				}
				return nil
			},
		},
		{
			// delete a claim waiting for being bound cleans up provision(volume ref == "") entry from timestamp cache
			name:            "5-8 - delete claim cleans up operation timestamp cache for provision",
			initialVolumes:  novolumes,
			expectedVolumes: novolumes,
			initialClaims: claimWithAnnotation(volume.AnnStorageProvisioner, "gcr.io/vendor-csi",
				newClaimArray("claim5-8", "uid5-8", "1Gi", "", v1.ClaimPending, &classExternal)),
			expectedClaims: noclaims,
			expectedEvents: []string{"Normal ExternalProvisioning"},
			errors:         noerrors,
			// Custom test function that generates a delete claim event which should have been caught by
			// "deleteClaim" to remove the claim from controller's cache and mark bound volume to be released
			test: func(ctrl *PersistentVolumeController, reactor *pvtesting.VolumeReactor, test controllerTest) error {
				// wait until the provision timestamp has been inserted
				err := wait.Poll(10*time.Millisecond, wait.ForeverTestTimeout, func() (bool, error) {
					return ctrl.operationTimestamps.Has("default/claim5-8"), nil
				})
				if err != nil {
					return err
				}
				// delete the claim
				obj := ctrl.claims.List()[0]
				claim := obj.(*v1.PersistentVolumeClaim)
				reactor.DeleteClaimEvent(claim)
				// wait until claim is cleared from cache, i.e., deleteClaim is called
				err = wait.Poll(10*time.Millisecond, wait.ForeverTestTimeout, func() (bool, error) {
					return len(ctrl.claims.ListKeys()) == 0, nil
				})
				if err != nil {
					return err
				}
				// make sure operation timestamp cache is empty
				if ctrl.operationTimestamps.Has("default/claim5-8") {
					return errors.New("failed checking timestamp cache")
				}
				return nil
			},
		},
		{
			// Test that the finalizer gets removed if CSI migration is disabled. The in-tree finalizer is added
			// back on the PV since migration is disabled.
			name: "5-9 - volume has its external PV deletion protection finalizer removed as CSI migration is disabled",
			initialVolumes: volumesWithFinalizers(
				volumesWithAnnotation(volume.AnnMigratedTo, "pd.csi.storage.gke.io",
					newVolumeArray("volume-5-9", "1Gi", "", "", v1.VolumeAvailable, v1.PersistentVolumeReclaimDelete, classEmpty, volume.AnnDynamicallyProvisioned)),
				[]string{volume.PVDeletionProtectionFinalizer},
			),
			expectedVolumes: volumesWithFinalizers(newVolumeArray("volume-5-9", "1Gi", "", "", v1.VolumeAvailable, v1.PersistentVolumeReclaimDelete, classEmpty, volume.AnnDynamicallyProvisioned), []string{volume.PVDeletionInTreeProtectionFinalizer}),
			initialClaims:   noclaims,
			expectedClaims:  noclaims,
			expectedEvents:  noevents,
			errors:          noerrors,
			test: func(ctrl *PersistentVolumeController, reactor *pvtesting.VolumeReactor, test controllerTest) error {
				return nil
			},
		},
	}
	logger, ctx := ktesting.NewTestContext(t)
	doit := func(test controllerTest) {
		// Initialize the controller
		client := &fake.Clientset{}

		fakeVolumeWatch := watch.NewFake()
		client.PrependWatchReactor("persistentvolumes", core.DefaultWatchReactor(fakeVolumeWatch, nil))
		fakeClaimWatch := watch.NewFake()
		client.PrependWatchReactor("persistentvolumeclaims", core.DefaultWatchReactor(fakeClaimWatch, nil))
		client.PrependWatchReactor("storageclasses", core.DefaultWatchReactor(watch.NewFake(), nil))
		client.PrependWatchReactor("nodes", core.DefaultWatchReactor(watch.NewFake(), nil))
		client.PrependWatchReactor("pods", core.DefaultWatchReactor(watch.NewFake(), nil))

		informers := informers.NewSharedInformerFactory(client, controller.NoResyncPeriodFunc())
		ctrl, err := newTestController(ctx, client, informers, true)
		if err != nil {
			t.Fatalf("Test %q construct persistent volume failed: %v", test.name, err)
		}

		// Inject storage classes into controller via a custom lister for test [5-5]
		storageClasses := []*storagev1.StorageClass{
			makeStorageClass(classExternal, &modeImmediate),
		}

		storageClasses[0].Provisioner = "gcr.io/vendor-csi"
		indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
		for _, class := range storageClasses {
			indexer.Add(class)
		}
		ctrl.classLister = storagelisters.NewStorageClassLister(indexer)

		reactor := newVolumeReactor(ctx, client, ctrl, fakeVolumeWatch, fakeClaimWatch, test.errors)
		for _, claim := range test.initialClaims {
			claim = claim.DeepCopy()
			reactor.AddClaim(claim)
			go func(claim *v1.PersistentVolumeClaim) {
				fakeClaimWatch.Add(claim)
			}(claim)
		}
		for _, volume := range test.initialVolumes {
			volume = volume.DeepCopy()
			reactor.AddVolume(volume)
			go func(volume *v1.PersistentVolume) {
				fakeVolumeWatch.Add(volume)
			}(volume)
		}

		// Start the controller
		ctx, cancel := context.WithCancel(context.TODO())
		informers.Start(ctx.Done())
		informers.WaitForCacheSync(ctx.Done())
		go ctrl.Run(ctx)

		// Wait for the controller to pass initial sync and fill its caches.
		err = wait.Poll(10*time.Millisecond, wait.ForeverTestTimeout, func() (bool, error) {
			return len(ctrl.claims.ListKeys()) >= len(test.initialClaims) &&
				len(ctrl.volumes.store.ListKeys()) >= len(test.initialVolumes), nil
		})
		if err != nil {
			t.Errorf("Test %q controller sync failed: %v", test.name, err)
		}
		logger.V(4).Info("controller synced, starting test")

		// Call the tested function
		err = test.test(ctrl, reactor.VolumeReactor, test)
		if err != nil {
			t.Errorf("Test %q initial test call failed: %v", test.name, err)
		}
		// Simulate a periodic resync, just in case some events arrived in a
		// wrong order.
		ctrl.resync(ctx)

		err = reactor.waitTest(test)
		if err != nil {
			t.Errorf("Failed to run test %s: %v", test.name, err)
		}
		cancel()

		evaluateTestResults(ctx, ctrl, reactor.VolumeReactor, test, t)
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			doit(test)
		})
	}
}

func storeVersion(t *testing.T, prefix string, c cache.Store, version string, expectedReturn bool) {
	pv := newVolume("pvName", "1Gi", "", "", v1.VolumeAvailable, v1.PersistentVolumeReclaimDelete, classEmpty)
	pv.ResourceVersion = version
	logger, _ := ktesting.NewTestContext(t)
	ret, err := storeObjectUpdate(logger, c, pv, "volume")
	if err != nil {
		t.Errorf("%s: expected storeObjectUpdate to succeed, got: %v", prefix, err)
	}
	if expectedReturn != ret {
		t.Errorf("%s: expected storeObjectUpdate to return %v, got: %v", prefix, expectedReturn, ret)
	}

	// find the stored version

	pvObj, found, err := c.GetByKey("pvName")
	if err != nil {
		t.Errorf("expected volume 'pvName' in the cache, got error instead: %v", err)
	}
	if !found {
		t.Errorf("expected volume 'pvName' in the cache but it was not found")
	}
	pv, ok := pvObj.(*v1.PersistentVolume)
	if !ok {
		t.Errorf("expected volume in the cache, got different object instead: %#v", pvObj)
	}

	if ret {
		if pv.ResourceVersion != version {
			t.Errorf("expected volume with version %s in the cache, got %s instead", version, pv.ResourceVersion)
		}
	} else {
		if pv.ResourceVersion == version {
			t.Errorf("expected volume with version other than %s in the cache, got %s instead", version, pv.ResourceVersion)
		}
	}
}

// TestControllerCache tests func storeObjectUpdate()
func TestControllerCache(t *testing.T) {
	// Cache under test
	c := cache.NewStore(cache.DeletionHandlingMetaNamespaceKeyFunc)

	// Store new PV
	storeVersion(t, "Step1", c, "1", true)
	// Store the same PV
	storeVersion(t, "Step2", c, "1", true)
	// Store newer PV
	storeVersion(t, "Step3", c, "2", true)
	// Store older PV - simulating old "PV updated" event or periodic sync with
	// old data
	storeVersion(t, "Step4", c, "1", false)
	// Store newer PV - test integer parsing ("2" > "10" as string,
	// while 2 < 10 as integers)
	storeVersion(t, "Step5", c, "10", true)
}

func TestControllerCacheParsingError(t *testing.T) {
	c := cache.NewStore(cache.DeletionHandlingMetaNamespaceKeyFunc)
	// There must be something in the cache to compare with
	storeVersion(t, "Step1", c, "1", true)

	pv := newVolume("pvName", "1Gi", "", "", v1.VolumeAvailable, v1.PersistentVolumeReclaimDelete, classEmpty)
	pv.ResourceVersion = "xxx"
	logger, _ := ktesting.NewTestContext(t)
	_, err := storeObjectUpdate(logger, c, pv, "volume")
	if err == nil {
		t.Errorf("Expected parsing error, got nil instead")
	}
}

func makeStorageClass(scName string, mode *storagev1.VolumeBindingMode) *storagev1.StorageClass {
	return &storagev1.StorageClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: scName,
		},
		Provisioner:       "kubernetes.io/no-provisioner",
		VolumeBindingMode: mode,
	}
}

func makeDefaultStorageClass(scName string, mode *storagev1.VolumeBindingMode) *storagev1.StorageClass {
	return &storagev1.StorageClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: scName,
			Annotations: map[string]string{
				util.IsDefaultStorageClassAnnotation: "true",
			},
		},
		Provisioner:       "kubernetes.io/no-provisioner",
		VolumeBindingMode: mode,
	}
}

func TestAnnealMigrationAnnotations(t *testing.T) {
	// The gce-pd plugin is used to test a migrated plugin (as the feature is
	// locked as of 1.25), and rbd is used as a non-migrated plugin (still alpha
	// as of 1.25). As plugins are migrated, rbd should be changed to a non-
	// migrated plugin. If there are no other non-migrated plugins, then those
	// test cases are moot and they can be removed (keeping only the test cases
	// with gce-pd).
	const testPlugin = "non-migrated-plugin"
	const migratedPlugin = "kubernetes.io/gce-pd"
	const migratedDriver = "pd.csi.storage.gke.io"
	const nonmigratedPlugin = "kubernetes.io/rbd"
	const nonmigratedDriver = "rbd.csi.ceph.com"
	tests := []struct {
		name                 string
		volumeAnnotations    map[string]string
		expVolumeAnnotations map[string]string
		claimAnnotations     map[string]string
		expClaimAnnotations  map[string]string
		testMigration        bool
	}{
		{
			name:                 "migration on",
			volumeAnnotations:    map[string]string{volume.AnnDynamicallyProvisioned: migratedPlugin},
			expVolumeAnnotations: map[string]string{volume.AnnDynamicallyProvisioned: migratedPlugin, volume.AnnMigratedTo: migratedDriver},
			claimAnnotations:     map[string]string{volume.AnnStorageProvisioner: migratedPlugin},
			expClaimAnnotations:  map[string]string{volume.AnnStorageProvisioner: migratedPlugin, volume.AnnMigratedTo: migratedDriver},
		},
		{
			name:                 "migration on with Beta storage provisioner annotation",
			volumeAnnotations:    map[string]string{volume.AnnDynamicallyProvisioned: migratedPlugin},
			expVolumeAnnotations: map[string]string{volume.AnnDynamicallyProvisioned: migratedPlugin, volume.AnnMigratedTo: migratedDriver},
			claimAnnotations:     map[string]string{volume.AnnBetaStorageProvisioner: migratedPlugin},
			expClaimAnnotations:  map[string]string{volume.AnnBetaStorageProvisioner: migratedPlugin, volume.AnnMigratedTo: migratedDriver},
		},
		{
			name:                 "migration off",
			volumeAnnotations:    map[string]string{volume.AnnDynamicallyProvisioned: nonmigratedPlugin},
			expVolumeAnnotations: map[string]string{volume.AnnDynamicallyProvisioned: nonmigratedPlugin},
			claimAnnotations:     map[string]string{volume.AnnStorageProvisioner: nonmigratedPlugin},
			expClaimAnnotations:  map[string]string{volume.AnnStorageProvisioner: nonmigratedPlugin},
		},
		{
			name:                 "migration off removes migrated to (rollback)",
			volumeAnnotations:    map[string]string{volume.AnnDynamicallyProvisioned: nonmigratedPlugin, volume.AnnMigratedTo: nonmigratedDriver},
			expVolumeAnnotations: map[string]string{volume.AnnDynamicallyProvisioned: nonmigratedPlugin},
			claimAnnotations:     map[string]string{volume.AnnStorageProvisioner: nonmigratedPlugin, volume.AnnMigratedTo: nonmigratedDriver},
			expClaimAnnotations:  map[string]string{volume.AnnStorageProvisioner: nonmigratedPlugin},
		},
		{
			name:                 "migration off removes migrated to (rollback) with Beta storage provisioner annotation",
			volumeAnnotations:    map[string]string{volume.AnnDynamicallyProvisioned: nonmigratedPlugin, volume.AnnMigratedTo: nonmigratedDriver},
			expVolumeAnnotations: map[string]string{volume.AnnDynamicallyProvisioned: nonmigratedPlugin},
			claimAnnotations:     map[string]string{volume.AnnBetaStorageProvisioner: nonmigratedPlugin, volume.AnnMigratedTo: nonmigratedDriver},
			expClaimAnnotations:  map[string]string{volume.AnnBetaStorageProvisioner: nonmigratedPlugin},
		},
		{
			name:                 "migration on, other plugin not affected",
			volumeAnnotations:    map[string]string{volume.AnnDynamicallyProvisioned: testPlugin},
			expVolumeAnnotations: map[string]string{volume.AnnDynamicallyProvisioned: testPlugin},
			claimAnnotations:     map[string]string{volume.AnnStorageProvisioner: testPlugin},
			expClaimAnnotations:  map[string]string{volume.AnnStorageProvisioner: testPlugin},
		},
		{
			name:                 "not dynamically provisioned",
			volumeAnnotations:    map[string]string{},
			expVolumeAnnotations: map[string]string{},
			claimAnnotations:     map[string]string{},
			expClaimAnnotations:  map[string]string{},
			testMigration:        false,
		},
		{
			name:                 "nil annotations",
			volumeAnnotations:    nil,
			expVolumeAnnotations: nil,
			claimAnnotations:     nil,
			expClaimAnnotations:  nil,
			testMigration:        false,
		},
	}

	translator := csitrans.New()
	cmpm := csimigration.NewPluginManager(translator, utilfeature.DefaultFeatureGate)
	logger, _ := ktesting.NewTestContext(t)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.volumeAnnotations != nil {
				ann := tc.volumeAnnotations
				updateMigrationAnnotations(logger, cmpm, translator, ann, false)
				if !reflect.DeepEqual(tc.expVolumeAnnotations, ann) {
					t.Errorf("got volume annoations: %v, but expected: %v", ann, tc.expVolumeAnnotations)
				}
			}
			if tc.claimAnnotations != nil {
				ann := tc.claimAnnotations
				updateMigrationAnnotations(logger, cmpm, translator, ann, true)
				if !reflect.DeepEqual(tc.expClaimAnnotations, ann) {
					t.Errorf("got volume annoations: %v, but expected: %v", ann, tc.expVolumeAnnotations)
				}
			}

		})
	}
}

func TestModifyDeletionFinalizers(t *testing.T) {
	// This set of tests ensures that protection finalizer is removed when CSI migration is disabled
	// and PV controller needs to remove finalizers added by the external-provisioner. The rbd
	// in-tree plugin is used as migration is disabled. When that plugin is migrated, a different
	// non-migrated one should be used. If all plugins are migrated this test can be removed. The
	// gce in-tree plugin is used for a migrated driver as it is feature-locked as of 1.25.
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.HonorPVReclaimPolicy, true)
	const nonmigratedDriver = "rbd.csi.ceph.com"
	const migratedPlugin = "kubernetes.io/gce-pd"
	const migratedDriver = "pd.csi.storage.gke.io"
	const customFinalizer = "test.volume.kubernetes.io/finalizer"
	tests := []struct {
		name                string
		initialVolume       *v1.PersistentVolume
		volumeAnnotations   map[string]string
		expVolumeFinalizers []string
		expModified         bool
	}{
		{
			// Represents a CSI volume provisioned through external-provisioner, no CSI migration enabled.
			name:                "13-1 migration was never enabled, volume has the finalizer",
			initialVolume:       newExternalProvisionedVolume("volume-13-1", "1Gi", "uid11-23", "claim11-23", v1.VolumeBound, v1.PersistentVolumeReclaimDelete, classCopper, nonmigratedDriver, []string{volume.PVDeletionProtectionFinalizer}, volume.AnnDynamicallyProvisioned, volume.AnnBoundByController),
			expVolumeFinalizers: []string{volume.PVDeletionProtectionFinalizer},
			expModified:         false,
		},
		{
			// Represents a volume provisioned through external-provisioner but the external-provisioner has
			// yet to sync the volume to add the new finalizer
			name:                "13-2 migration was never enabled, volume does not have the finalizer",
			initialVolume:       newExternalProvisionedVolume("volume-13-2", "1Gi", "uid11-23", "claim11-23", v1.VolumeBound, v1.PersistentVolumeReclaimDelete, classCopper, nonmigratedDriver, nil, volume.AnnDynamicallyProvisioned, volume.AnnBoundByController),
			expVolumeFinalizers: nil,
			expModified:         false,
		},
		{
			// Represents an in-tree volume that has the migrated-to annotation but the external-provisioner is
			// yet to sync the volume and add the pv deletion protection finalizer. The custom finalizer is some
			// pre-existing finalizer, for example the pv-protection finalizer. When csi-migration is disabled,
			// the migrated-to annotation will be removed shortly when updateVolumeMigrationAnnotationsAndFinalizers
			// is called followed by adding back the in-tree pv protection finalizer.
			name:                "13-3 migration was disabled, volume has existing custom finalizer, does not have in-tree pv deletion protection finalizer",
			initialVolume:       newVolumeWithFinalizers("volume-13-3", "1Gi", "uid11-23", "claim11-23", v1.VolumeBound, v1.PersistentVolumeReclaimDelete, classCopper, []string{customFinalizer}, volume.AnnDynamicallyProvisioned, volume.AnnBoundByController),
			expVolumeFinalizers: []string{customFinalizer, volume.PVDeletionInTreeProtectionFinalizer},
			expModified:         true,
		},
		{
			name:                "13-4 migration was disabled, volume has no finalizers",
			initialVolume:       newVolumeWithFinalizers("volume-13-4", "1Gi", "uid11-23", "claim11-23", v1.VolumeBound, v1.PersistentVolumeReclaimDelete, classCopper, nil, volume.AnnDynamicallyProvisioned, volume.AnnBoundByController),
			expVolumeFinalizers: []string{volume.PVDeletionInTreeProtectionFinalizer},
			expModified:         true,
		},
		{
			// Represents roll back scenario where the external-provisioner has added the pv deletion protection
			// finalizer and later the csi migration was disabled. The pv deletion protection finalizer added through
			// external-provisioner will be removed and the in-tree pv deletion protection finalizer will be added.
			name:                "13-5 migration was disabled, volume has external PV deletion finalizer",
			initialVolume:       newVolumeWithFinalizers("volume-13-5", "1Gi", "uid11-23", "claim11-23", v1.VolumeBound, v1.PersistentVolumeReclaimDelete, classCopper, []string{volume.PVDeletionProtectionFinalizer}, volume.AnnDynamicallyProvisioned, volume.AnnBoundByController),
			expVolumeFinalizers: []string{volume.PVDeletionInTreeProtectionFinalizer},
			expModified:         true,
		},
		{
			// Represents roll-back of csi-migration as 13-5, here there are multiple finalizers, only the pv deletion
			// protection finalizer added by external-provisioner will be removed and the in-tree pv deletion protection
			// finalizer will be added.
			name:                "13-6 migration was disabled, volume has multiple finalizers",
			initialVolume:       newVolumeWithFinalizers("volume-13-6", "1Gi", "uid11-23", "claim11-23", v1.VolumeBound, v1.PersistentVolumeReclaimDelete, classCopper, []string{volume.PVDeletionProtectionFinalizer, customFinalizer}, volume.AnnDynamicallyProvisioned, volume.AnnBoundByController),
			expVolumeFinalizers: []string{customFinalizer, volume.PVDeletionInTreeProtectionFinalizer},
			expModified:         true,
		},
		{
			// csi migration is enabled, the pv controller should not delete the finalizer added by the
			// external-provisioner and the in-tree finalizer should be deleted.
			name:                "13-7 migration is enabled, volume has both the in-tree and external PV deletion protection finalizer",
			initialVolume:       newVolumeWithFinalizers("volume-13-7", "1Gi", "uid11-23", "claim11-23", v1.VolumeBound, v1.PersistentVolumeReclaimDelete, classCopper, []string{volume.PVDeletionProtectionFinalizer, volume.PVDeletionInTreeProtectionFinalizer}, volume.AnnDynamicallyProvisioned, volume.AnnBoundByController),
			volumeAnnotations:   map[string]string{volume.AnnDynamicallyProvisioned: migratedPlugin, volume.AnnMigratedTo: migratedDriver},
			expVolumeFinalizers: []string{volume.PVDeletionProtectionFinalizer},
			expModified:         true,
		},
		{
			// csi-migration is not completely enabled as the specific plugin feature is not present. This is equivalent
			// of disabled csi-migration.
			name:                "13-8 migration is enabled but plugin migration feature is disabled, volume has the external PV deletion protection finalizer",
			initialVolume:       newVolumeWithFinalizers("volume-13-8", "1Gi", "uid11-23", "claim11-23", v1.VolumeBound, v1.PersistentVolumeReclaimDelete, classCopper, []string{volume.PVDeletionProtectionFinalizer}, volume.AnnDynamicallyProvisioned, volume.AnnBoundByController),
			expVolumeFinalizers: []string{volume.PVDeletionInTreeProtectionFinalizer},
			expModified:         true,
		},
		{
			// same as 13-8 but multiple finalizers exists, only the pv deletion protection finalizer needs to be
			// removed and the in-tree pv deletion protection finalizer needs to be added.
			name:                "13-9 migration is enabled but plugin migration feature is disabled, volume has multiple finalizers including external PV deletion protection finalizer",
			initialVolume:       newVolumeWithFinalizers("volume-13-9", "1Gi", "uid11-23", "claim11-23", v1.VolumeBound, v1.PersistentVolumeReclaimDelete, classCopper, []string{volume.PVDeletionProtectionFinalizer, customFinalizer}, volume.AnnDynamicallyProvisioned, volume.AnnBoundByController),
			expVolumeFinalizers: []string{customFinalizer, volume.PVDeletionInTreeProtectionFinalizer},
			expModified:         true,
		},
		{
			// corner error case.
			name:                "13-10 missing annotations but finalizers exist",
			initialVolume:       newVolumeWithFinalizers("volume-13-10", "1Gi", "uid11-23", "claim11-23", v1.VolumeBound, v1.PersistentVolumeReclaimDelete, classCopper, []string{volume.PVDeletionProtectionFinalizer}),
			expVolumeFinalizers: []string{volume.PVDeletionProtectionFinalizer},
			expModified:         false,
		},
		{
			name:                "13-11 missing annotations and finalizers",
			initialVolume:       newVolumeWithFinalizers("volume-13-11", "1Gi", "uid11-23", "claim11-23", v1.VolumeBound, v1.PersistentVolumeReclaimDelete, classCopper, nil),
			expVolumeFinalizers: nil,
			expModified:         false,
		},
		{
			// When ReclaimPolicy is Retain ensure that in-tree pv deletion protection finalizer is not added.
			name:                "13-12 migration is disabled, volume has no finalizers, reclaimPolicy is Retain",
			initialVolume:       newVolumeWithFinalizers("volume-13-12", "1Gi", "uid11-23", "claim11-23", v1.VolumeBound, v1.PersistentVolumeReclaimRetain, classCopper, nil, volume.AnnDynamicallyProvisioned, volume.AnnBoundByController),
			expVolumeFinalizers: nil,
			expModified:         false,
		},
		{
			// When ReclaimPolicy is Recycle ensure that in-tree pv deletion protection finalizer is not added.
			name:                "13-13 migration is disabled, volume has no finalizers, reclaimPolicy is Recycle",
			initialVolume:       newVolumeWithFinalizers("volume-13-13", "1Gi", "uid11-23", "claim11-23", v1.VolumeBound, v1.PersistentVolumeReclaimRecycle, classCopper, nil, volume.AnnDynamicallyProvisioned, volume.AnnBoundByController),
			expVolumeFinalizers: nil,
			expModified:         false,
		},
		{
			// When ReclaimPolicy is Retain ensure that in-tree pv deletion protection finalizer present is removed.
			name:                "13-14 migration is disabled, volume has in-tree pv deletion finalizers, reclaimPolicy is Retain",
			initialVolume:       newVolumeWithFinalizers("volume-13-14", "1Gi", "uid11-23", "claim11-23", v1.VolumeBound, v1.PersistentVolumeReclaimRetain, classCopper, []string{volume.PVDeletionInTreeProtectionFinalizer}, volume.AnnDynamicallyProvisioned, volume.AnnBoundByController),
			expVolumeFinalizers: nil,
			expModified:         true,
		},
		{
			// Statically provisioned volumes should not have the in-tree pv deletion protection finalizer
			name:                "13-15 migration is disabled, statically provisioned PV",
			initialVolume:       newVolumeWithFinalizers("volume-13-14", "1Gi", "", "", v1.VolumeAvailable, v1.PersistentVolumeReclaimDelete, classCopper, nil),
			expVolumeFinalizers: nil,
			expModified:         false,
		},
	}

	translator := csitrans.New()
	cmpm := csimigration.NewPluginManager(translator, utilfeature.DefaultFeatureGate)
	logger, _ := ktesting.NewTestContext(t)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.volumeAnnotations != nil {
				tc.initialVolume.SetAnnotations(tc.volumeAnnotations)
			}
			modifiedFinalizers, modified := modifyDeletionFinalizers(logger, cmpm, tc.initialVolume)
			if modified != tc.expModified {
				t.Errorf("got modified: %v, but expected: %v", modified, tc.expModified)
			}
			if !reflect.DeepEqual(tc.expVolumeFinalizers, modifiedFinalizers) {
				t.Errorf("got volume finaliers: %v, but expected: %v", modifiedFinalizers, tc.expVolumeFinalizers)
			}

		})
	}
}

func TestRetroactiveStorageClassAssignment(t *testing.T) {
	tests := []struct {
		storageClasses []*storagev1.StorageClass
		tests          []controllerTest
	}{
		// [Unit test set 15] - retroactive storage class assignment tests
		{
			storageClasses: []*storagev1.StorageClass{},
			tests: []controllerTest{
				{
					name:            "15-1 - pvc storage class is not assigned retroactively if there are no default storage classes",
					initialVolumes:  novolumes,
					expectedVolumes: novolumes,
					initialClaims:   newClaimArray("claim15-1", "uid15-1", "1Gi", "", v1.ClaimPending, nil),
					expectedClaims:  newClaimArray("claim15-1", "uid15-1", "1Gi", "", v1.ClaimPending, nil),
					expectedEvents:  noevents,
					errors:          noerrors,
					test:            testSyncClaim,
				},
			},
		},
		{
			storageClasses: []*storagev1.StorageClass{
				makeDefaultStorageClass(classGold, &modeImmediate),
				makeStorageClass(classSilver, &modeImmediate),
			},
			tests: []controllerTest{
				{
					name:            "15-3 - pvc storage class is not assigned retroactively if claim is already bound",
					initialVolumes:  novolumes,
					expectedVolumes: novolumes,
					initialClaims:   newClaimArray("claim15-3", "uid15-3", "1Gi", "test", v1.ClaimBound, &classCopper, volume.AnnBoundByController, volume.AnnBindCompleted),
					expectedClaims:  newClaimArray("claim15-3", "uid15-3", "1Gi", "test", v1.ClaimLost, &classCopper, volume.AnnBoundByController, volume.AnnBindCompleted),
					expectedEvents:  noevents,
					errors:          noerrors,
					test:            testSyncClaim,
				},
			},
		},
		{
			storageClasses: []*storagev1.StorageClass{
				makeDefaultStorageClass(classGold, &modeImmediate),
				makeStorageClass(classSilver, &modeImmediate),
			},
			tests: []controllerTest{
				{
					name:            "15-4 - pvc storage class is not assigned retroactively if claim is already bound but annotations are missing",
					initialVolumes:  novolumes,
					expectedVolumes: novolumes,
					initialClaims:   newClaimArray("claim15-4", "uid15-4", "1Gi", "test", v1.ClaimBound, &classCopper),
					expectedClaims:  newClaimArray("claim15-4", "uid15-4", "1Gi", "test", v1.ClaimPending, &classCopper),
					expectedEvents:  noevents,
					errors:          noerrors,
					test:            testSyncClaim,
				},
			},
		},
		{
			storageClasses: []*storagev1.StorageClass{
				makeDefaultStorageClass(classGold, &modeImmediate),
				makeStorageClass(classSilver, &modeImmediate),
			},
			tests: []controllerTest{
				{
					name:            "15-5 - pvc storage class is assigned retroactively if there is a default",
					initialVolumes:  novolumes,
					expectedVolumes: novolumes,
					initialClaims:   newClaimArray("claim15-5", "uid15-5", "1Gi", "", v1.ClaimPending, nil),
					expectedClaims:  newClaimArray("claim15-5", "uid15-5", "1Gi", "", v1.ClaimPending, &classGold),
					expectedEvents:  noevents,
					errors:          noerrors,
					test:            testSyncClaim,
				},
			},
		},
		{
			storageClasses: []*storagev1.StorageClass{
				makeDefaultStorageClass(classGold, &modeImmediate),
				makeDefaultStorageClass(classSilver, &modeImmediate)},
			tests: []controllerTest{
				{
					name:            "15-2 - pvc storage class is assigned retroactively if there are multiple default storage classes",
					initialVolumes:  novolumes,
					expectedVolumes: novolumes,
					initialClaims:   newClaimArray("claim15-2", "uid15-2", "1Gi", "", v1.ClaimPending, nil),
					expectedClaims:  newClaimArray("claim15-2", "uid15-2", "1Gi", "", v1.ClaimPending, &classGold),
					expectedEvents:  noevents,
					errors:          noerrors,
					test:            testSyncClaim,
				},
			},
		},
		{
			storageClasses: []*storagev1.StorageClass{
				makeDefaultStorageClass(classGold, &modeImmediate),
				makeStorageClass(classCopper, &modeImmediate),
			},
			tests: []controllerTest{
				{
					name:            "15-6 - pvc storage class is not changed if claim is not bound but already has a storage class",
					initialVolumes:  novolumes,
					expectedVolumes: novolumes,
					initialClaims:   newClaimArray("claim15-6", "uid15-6", "1Gi", "", v1.ClaimPending, &classCopper),
					expectedClaims:  newClaimArray("claim15-6", "uid15-6", "1Gi", "", v1.ClaimPending, &classCopper),
					expectedEvents:  noevents,
					errors:          noerrors,
					test:            testSyncClaim,
				},
			},
		},
		{
			storageClasses: []*storagev1.StorageClass{
				makeDefaultStorageClass(classGold, &modeImmediate),
				makeStorageClass(classCopper, &modeImmediate),
			},
			tests: []controllerTest{
				{
					name:            "15-7 - pvc storage class is not changed if claim is not bound but already set annotation \"volume.beta.kubernetes.io/storage-class\"",
					initialVolumes:  novolumes,
					expectedVolumes: novolumes,
					initialClaims:   newClaimArray("claim15-7", "uid15-7", "1Gi", "", v1.ClaimPending, nil, v1.BetaStorageClassAnnotation),
					expectedClaims:  newClaimArray("claim15-7", "uid15-7", "1Gi", "", v1.ClaimPending, nil, v1.BetaStorageClassAnnotation),
					expectedEvents:  noevents,
					errors:          noerrors,
					test:            testSyncClaim,
				},
			},
		},
	}
	_, ctx := ktesting.NewTestContext(t)
	for _, test := range tests {
		runSyncTests(t, ctx, test.tests, test.storageClasses, nil)
	}
}
