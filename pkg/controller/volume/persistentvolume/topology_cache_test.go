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

package persistentvolume

import (
	"fmt"
	"testing"

	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func makePV(name, version string) *v1.PersistentVolume {
	return &v1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: name, ResourceVersion: version}}
}

func TestAssumePV(t *testing.T) {
	scenarios := map[string]struct {
		oldPV         *v1.PersistentVolume
		newPV         *v1.PersistentVolume
		shouldSucceed bool
	}{
		"success-same-version": {
			oldPV:         makePV("pv1", "5"),
			newPV:         makePV("pv1", "5"),
			shouldSucceed: true,
		},
		"success-new-higher-version": {
			oldPV:         makePV("pv1", "5"),
			newPV:         makePV("pv1", "6"),
			shouldSucceed: true,
		},
		"fail-old-not-found": {
			oldPV:         makePV("pv2", "5"),
			newPV:         makePV("pv1", "5"),
			shouldSucceed: false,
		},
		"fail-new-lower-version": {
			oldPV:         makePV("pv1", "5"),
			newPV:         makePV("pv1", "4"),
			shouldSucceed: false,
		},
		"fail-new-bad-version": {
			oldPV:         makePV("pv1", "5"),
			newPV:         makePV("pv1", "a"),
			shouldSucceed: false,
		},
		"fail-old-bad-version": {
			oldPV:         makePV("pv1", "a"),
			newPV:         makePV("pv1", "5"),
			shouldSucceed: false,
		},
	}

	for name, scenario := range scenarios {
		cache := newPVTmpCache(nil)
		internal_cache, ok := cache.(*pvTmpCache)
		if !ok {
			t.Fatalf("Failed to get internal cache")
		}

		// Add oldPV to cache
		internal_cache.add(scenario.oldPV)
		if err := getPV(cache, scenario.oldPV.Name, scenario.oldPV); err != nil {
			t.Errorf("Failed to GetPV() after initial update: %v", err)
			continue
		}

		// TmpUpdate newPV
		err := cache.TmpUpdate(scenario.newPV)
		if scenario.shouldSucceed && err != nil {
			t.Errorf("Test %q failed: TmpUpdate() returned error %v", name, err)
		}
		if !scenario.shouldSucceed && err == nil {
			t.Errorf("Test %q failed: TmpUpdate() returned success but expected error", name)
		}

		// Check that GetPV returns correct PV
		expectedPV := scenario.newPV
		if !scenario.shouldSucceed {
			expectedPV = scenario.oldPV
		}
		if err := getPV(cache, scenario.oldPV.Name, expectedPV); err != nil {
			t.Errorf("Failed to GetPV() after initial update: %v", err)
		}
	}
}

func TestRestorePV(t *testing.T) {
	cache := newPVTmpCache(nil)
	internal_cache, ok := cache.(*pvTmpCache)
	if !ok {
		t.Fatalf("Failed to get internal cache")
	}

	oldPV := makePV("pv1", "5")
	newPV := makePV("pv1", "5")

	// Restore PV that doesn't exist
	cache.Restore("nothing")

	// Add oldPV to cache
	internal_cache.add(oldPV)
	if err := getPV(cache, oldPV.Name, oldPV); err != nil {
		t.Fatalf("Failed to GetPV() after initial update: %v", err)
	}

	// Restore PV
	cache.Restore(oldPV.Name)
	if err := getPV(cache, oldPV.Name, oldPV); err != nil {
		t.Fatalf("Failed to GetPV() after iniital restore: %v", err)
	}

	// Assume newPV
	if err := cache.TmpUpdate(newPV); err != nil {
		t.Fatalf("TmpUpdate() returned error %v", err)
	}
	if err := getPV(cache, oldPV.Name, newPV); err != nil {
		t.Fatalf("Failed to GetPV() after TmpUpdate: %v", err)
	}

	// Restore PV
	cache.Restore(oldPV.Name)
	if err := getPV(cache, oldPV.Name, oldPV); err != nil {
		t.Fatalf("Failed to GetPV() after restore: %v", err)
	}
}

func TestBasicPVCache(t *testing.T) {
	cache := newPVTmpCache(nil)
	internal_cache, ok := cache.(*pvTmpCache)
	if !ok {
		t.Fatalf("Failed to get internal cache")
	}

	// Get object that doesn't exist
	pv := cache.GetPV("nothere")
	if pv != nil {
		t.Errorf("GetPV() returned unexpected PV %q", pv.Name)
	}

	// Add a bunch of PVs
	pvs := map[string]*v1.PersistentVolume{}
	for i := 0; i < 10; i++ {
		pv := makePV(fmt.Sprintf("test-pv%v", i), "1")
		pvs[pv.Name] = pv
		internal_cache.add(pv)
	}

	// List them
	verifyListPVs(t, cache, pvs)

	// Update a PV
	updatedPV := makePV("test-pv3", "2")
	pvs[updatedPV.Name] = updatedPV
	internal_cache.update(nil, updatedPV)

	// List them
	verifyListPVs(t, cache, pvs)

	// Delete a PV
	deletedPV := pvs["test-pv7"]
	delete(pvs, deletedPV.Name)
	internal_cache.delete(deletedPV)

	// List them
	verifyListPVs(t, cache, pvs)
}

func verifyListPVs(t *testing.T, cache PVTmpCache, expectedPVs map[string]*v1.PersistentVolume) {
	pvList := cache.ListPVs()
	if len(pvList) != len(expectedPVs) {
		t.Errorf("ListPVs() returned %v PVs, expected %v", len(pvList), len(expectedPVs))
	}
	for _, pv := range pvList {
		expectedPV, ok := expectedPVs[pv.Name]
		if !ok {
			t.Errorf("ListPVs() returned unexpected PV %q", pv.Name)
		}
		if expectedPV != pv {
			t.Errorf("ListPVs() returned PV %p, expected %p", pv, expectedPV)
		}
	}
}

func getPV(cache PVTmpCache, name string, expectedPV *v1.PersistentVolume) error {
	pv := cache.GetPV(name)
	if pv == nil {
		return fmt.Errorf("PV %q not found in cache", name)
	}
	if pv != expectedPV {
		return fmt.Errorf("GetPV() returned %p, expected %p", pv, expectedPV)
	}
	return nil
}
