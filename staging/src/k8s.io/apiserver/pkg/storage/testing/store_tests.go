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

package testing

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp" //nolint:depguard
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/apiserver/pkg/apis/example"
	"k8s.io/apiserver/pkg/storage"
	"k8s.io/apiserver/pkg/storage/value"
	utilpointer "k8s.io/utils/pointer"
)

type KeyValidation func(ctx context.Context, t *testing.T, key string)

func RunTestCreate(ctx context.Context, t *testing.T, store storage.Interface, validation KeyValidation) {
	tests := []struct {
		name          string
		inputObj      *example.Pod
		expectedError error
	}{{
		name:     "successful create",
		inputObj: &example.Pod{ObjectMeta: metav1.ObjectMeta{Name: "foo", Namespace: "test-ns"}},
	}, {
		name:          "create with ResourceVersion set",
		inputObj:      &example.Pod{ObjectMeta: metav1.ObjectMeta{Name: "bar", Namespace: "test-ns", ResourceVersion: "1"}},
		expectedError: storage.ErrResourceVersionSetOnCreate,
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := &example.Pod{} // reset
			// verify that kv pair is empty before set
			key := computePodKey(tt.inputObj)
			if err := store.Get(ctx, key, storage.GetOptions{}, out); !storage.IsNotFound(err) {
				t.Fatalf("expecting empty result on key %s, got %v", key, err)
			}

			err := store.Create(ctx, key, tt.inputObj, out, 0)
			if !errors.Is(err, tt.expectedError) {
				t.Errorf("expecting error %v, but get: %v", tt.expectedError, err)
			}
			if err != nil {
				return
			}
			// basic tests of the output
			if tt.inputObj.ObjectMeta.Name != out.ObjectMeta.Name {
				t.Errorf("pod name want=%s, get=%s", tt.inputObj.ObjectMeta.Name, out.ObjectMeta.Name)
			}
			if out.ResourceVersion == "" {
				t.Errorf("output should have non-empty resource version")
			}
			validation(ctx, t, key)
		})
	}
}

func RunTestCreateWithTTL(ctx context.Context, t *testing.T, store storage.Interface) {
	input := &example.Pod{ObjectMeta: metav1.ObjectMeta{Name: "foo", Namespace: "test-ns"}}
	out := &example.Pod{}

	key := computePodKey(input)
	if err := store.Create(ctx, key, input, out, 1); err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	w, err := store.Watch(ctx, key, storage.ListOptions{ResourceVersion: out.ResourceVersion, Predicate: storage.Everything})
	if err != nil {
		t.Fatalf("Watch failed: %v", err)
	}
	testCheckEventType(t, w, watch.Deleted)
}

func RunTestCreateWithKeyExist(ctx context.Context, t *testing.T, store storage.Interface) {
	obj := &example.Pod{ObjectMeta: metav1.ObjectMeta{Name: "foo", Namespace: "test-ns"}}
	key, _ := testPropagateStore(ctx, t, store, obj)
	out := &example.Pod{}

	err := store.Create(ctx, key, obj, out, 0)
	if err == nil || !storage.IsExist(err) {
		t.Errorf("expecting key exists error, but get: %s", err)
	}
}

func RunTestGet(ctx context.Context, t *testing.T, store storage.Interface) {
	// create an object to test
	key, createdObj := testPropagateStore(ctx, t, store, &example.Pod{ObjectMeta: metav1.ObjectMeta{Name: "foo", Namespace: "test-ns"}})
	// update the object once to allow get by exact resource version to be tested
	updateObj := createdObj.DeepCopy()
	updateObj.Annotations = map[string]string{"test-annotation": "1"}
	storedObj := &example.Pod{}
	err := store.GuaranteedUpdate(ctx, key, storedObj, true, nil,
		func(_ runtime.Object, _ storage.ResponseMeta) (runtime.Object, *uint64, error) {
			ttl := uint64(1)
			return updateObj, &ttl, nil
		}, nil)
	if err != nil {
		t.Fatalf("Update failed: %v", err)
	}
	// create an additional object to increment the resource version for pods above the resource version of the foo object
	secondObj := &example.Pod{ObjectMeta: metav1.ObjectMeta{Name: "bar", Namespace: "test-ns"}}
	lastUpdatedObj := &example.Pod{}
	if err := store.Create(ctx, computePodKey(secondObj), secondObj, lastUpdatedObj, 0); err != nil {
		t.Fatalf("Set failed: %v", err)
	}

	currentRV, _ := strconv.Atoi(storedObj.ResourceVersion)
	lastUpdatedCurrentRV, _ := strconv.Atoi(lastUpdatedObj.ResourceVersion)

	// TODO(jpbetz): Add exact test cases
	tests := []struct {
		name                 string
		key                  string
		ignoreNotFound       bool
		expectNotFoundErr    bool
		expectRVTooLarge     bool
		expectedOut          *example.Pod
		expectedAlternatives []*example.Pod
		rv                   string
	}{{
		name:              "get existing",
		key:               key,
		ignoreNotFound:    false,
		expectNotFoundErr: false,
		expectedOut:       storedObj,
	}, {
		// For RV=0 arbitrarily old version is allowed, including from the moment
		// when the object didn't yet exist.
		// As a result, we allow it by setting ignoreNotFound and allowing an empty
		// object in expectedOut.
		name:                 "resource version 0",
		key:                  key,
		ignoreNotFound:       true,
		expectedAlternatives: []*example.Pod{{}, createdObj, storedObj},
		rv:                   "0",
	}, {
		// Given that Get with set ResourceVersion is effectively always
		// NotOlderThan semantic, both versions of object are allowed.
		name:                 "object created resource version",
		key:                  key,
		expectedAlternatives: []*example.Pod{createdObj, storedObj},
		rv:                   createdObj.ResourceVersion,
	}, {
		name:        "current object resource version, match=NotOlderThan",
		key:         key,
		expectedOut: storedObj,
		rv:          fmt.Sprintf("%d", currentRV),
	}, {
		name:        "latest resource version",
		key:         key,
		expectedOut: storedObj,
		rv:          fmt.Sprintf("%d", lastUpdatedCurrentRV),
	}, {
		name:             "too high resource version",
		key:              key,
		expectRVTooLarge: true,
		rv:               strconv.FormatInt(math.MaxInt64, 10),
	}, {
		name:              "get non-existing",
		key:               "/non-existing",
		ignoreNotFound:    false,
		expectNotFoundErr: true,
	}, {
		name:              "get non-existing, ignore not found",
		key:               "/non-existing",
		ignoreNotFound:    true,
		expectNotFoundErr: false,
		expectedOut:       &example.Pod{},
	}}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			// For some asynchronous implementations of storage interface (in particular watchcache),
			// certain requests may impact result of further requests. As an example, if we first
			// ensure that watchcache is synchronized up to ResourceVersion X (using Get/List requests
			// with NotOlderThan semantic), the further requests (even specifying earlier resource
			// version) will also return the result synchronized to at least ResourceVersion X.
			// By parallelizing test cases we ensure that the order in which test cases are defined
			// doesn't automatically preclude some scenarios from happening.
			t.Parallel()

			out := &example.Pod{}
			err := store.Get(ctx, tt.key, storage.GetOptions{IgnoreNotFound: tt.ignoreNotFound, ResourceVersion: tt.rv}, out)
			if tt.expectNotFoundErr {
				if err == nil || !storage.IsNotFound(err) {
					t.Errorf("expecting not found error, but get: %v", err)
				}
				return
			}
			if tt.expectRVTooLarge {
				if err == nil || !storage.IsTooLargeResourceVersion(err) {
					t.Errorf("expecting resource version too high error, but get: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Get failed: %v", err)
			}

			if tt.expectedAlternatives == nil {
				expectNoDiff(t, fmt.Sprintf("%s: incorrect pod", tt.name), tt.expectedOut, out)
			} else {
				ExpectContains(t, fmt.Sprintf("%s: incorrect pod", tt.name), toInterfaceSlice(tt.expectedAlternatives), out)
			}
		})
	}
}

func RunTestUnconditionalDelete(ctx context.Context, t *testing.T, store storage.Interface) {
	key, storedObj := testPropagateStore(ctx, t, store, &example.Pod{ObjectMeta: metav1.ObjectMeta{Name: "foo", Namespace: "test-ns"}})

	tests := []struct {
		name              string
		key               string
		expectedObj       *example.Pod
		expectNotFoundErr bool
	}{{
		name:              "existing key",
		key:               key,
		expectedObj:       storedObj,
		expectNotFoundErr: false,
	}, {
		name:              "non-existing key",
		key:               "/non-existing",
		expectedObj:       nil,
		expectNotFoundErr: true,
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := &example.Pod{} // reset
			err := store.Delete(ctx, tt.key, out, nil, storage.ValidateAllObjectFunc, nil, storage.DeleteOptions{})
			if tt.expectNotFoundErr {
				if err == nil || !storage.IsNotFound(err) {
					t.Errorf("expecting not found error, but get: %s", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Delete failed: %v", err)
			}
			// We expect the resource version of the returned object to be
			// updated compared to the last existing object.
			if storedObj.ResourceVersion == out.ResourceVersion {
				t.Errorf("expecting resource version to be updated, but get: %s", out.ResourceVersion)
			}
			out.ResourceVersion = storedObj.ResourceVersion
			expectNoDiff(t, "incorrect pod:", tt.expectedObj, out)
		})
	}
}

func RunTestConditionalDelete(ctx context.Context, t *testing.T, store storage.Interface) {
	obj := &example.Pod{ObjectMeta: metav1.ObjectMeta{Name: "foo", Namespace: "test-ns", UID: "A"}}
	key, storedObj := testPropagateStore(ctx, t, store, obj)

	tests := []struct {
		name                string
		precondition        *storage.Preconditions
		expectInvalidObjErr bool
	}{{
		name:                "UID match",
		precondition:        storage.NewUIDPreconditions("A"),
		expectInvalidObjErr: false,
	}, {
		name:                "UID mismatch",
		precondition:        storage.NewUIDPreconditions("B"),
		expectInvalidObjErr: true,
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := &example.Pod{}
			err := store.Delete(ctx, key, out, tt.precondition, storage.ValidateAllObjectFunc, nil, storage.DeleteOptions{})
			if tt.expectInvalidObjErr {
				if err == nil || !storage.IsInvalidObj(err) {
					t.Errorf("expecting invalid UID error, but get: %s", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Delete failed: %v", err)
			}
			// We expect the resource version of the returned object to be
			// updated compared to the last existing object.
			if storedObj.ResourceVersion == out.ResourceVersion {
				t.Errorf("expecting resource version to be updated, but get: %s", out.ResourceVersion)
			}
			out.ResourceVersion = storedObj.ResourceVersion
			expectNoDiff(t, "incorrect pod:", storedObj, out)
			obj := &example.Pod{ObjectMeta: metav1.ObjectMeta{Name: "foo", Namespace: "test-ns", UID: "A"}}
			key, storedObj = testPropagateStore(ctx, t, store, obj)
		})
	}
}

// The following set of Delete tests are testing the logic of adding `suggestion`
// as a parameter with probably value of the current state.
// Introducing it for GuaranteedUpdate cause a number of issues, so we're addressing
// all of those upfront by adding appropriate tests:
// - https://github.com/kubernetes/kubernetes/pull/35415
//   [DONE] Lack of tests originally - added TestDeleteWithSuggestion.
// - https://github.com/kubernetes/kubernetes/pull/40664
//   [DONE] Irrelevant for delete, as Delete doesn't write data (nor compare it).
// - https://github.com/kubernetes/kubernetes/pull/47703
//   [DONE] Irrelevant for delete, because Delete doesn't persist data.
// - https://github.com/kubernetes/kubernetes/pull/48394/
//   [DONE] Irrelevant for delete, because Delete doesn't compare data.
// - https://github.com/kubernetes/kubernetes/pull/43152
//   [DONE] Added TestDeleteWithSuggestionAndConflict
// - https://github.com/kubernetes/kubernetes/pull/54780
//   [DONE] Irrelevant for delete, because Delete doesn't compare data.
// - https://github.com/kubernetes/kubernetes/pull/58375
//   [DONE] Irrelevant for delete, because Delete doesn't compare data.
// - https://github.com/kubernetes/kubernetes/pull/77619
//   [DONE] Added TestValidateDeletionWithSuggestion for corresponding delete checks.
// - https://github.com/kubernetes/kubernetes/pull/78713
//   [DONE] Bug was in getState function which is shared with the new code.
// - https://github.com/kubernetes/kubernetes/pull/78713
//   [DONE] Added TestPreconditionalDeleteWithSuggestion

func RunTestDeleteWithSuggestion(ctx context.Context, t *testing.T, store storage.Interface) {
	key, originalPod := testPropagateStore(ctx, t, store, &example.Pod{ObjectMeta: metav1.ObjectMeta{Name: "name", Namespace: "test-ns"}})

	out := &example.Pod{}
	if err := store.Delete(ctx, key, out, nil, storage.ValidateAllObjectFunc, originalPod, storage.DeleteOptions{}); err != nil {
		t.Errorf("Unexpected failure during deletion: %v", err)
	}

	if err := store.Get(ctx, key, storage.GetOptions{}, &example.Pod{}); !storage.IsNotFound(err) {
		t.Errorf("Unexpected error on reading object: %v", err)
	}
}

func RunTestDeleteWithSuggestionAndConflict(ctx context.Context, t *testing.T, store storage.Interface) {
	key, originalPod := testPropagateStore(ctx, t, store, &example.Pod{ObjectMeta: metav1.ObjectMeta{Name: "name", Namespace: "test-ns"}})

	// First update, so originalPod is outdated.
	updatedPod := &example.Pod{}
	if err := store.GuaranteedUpdate(ctx, key, updatedPod, false, nil,
		storage.SimpleUpdate(func(obj runtime.Object) (runtime.Object, error) {
			pod := obj.(*example.Pod)
			pod.ObjectMeta.Labels = map[string]string{"foo": "bar"}
			return pod, nil
		}), nil); err != nil {
		t.Errorf("Unexpected failure during updated: %v", err)
	}

	out := &example.Pod{}
	if err := store.Delete(ctx, key, out, nil, storage.ValidateAllObjectFunc, originalPod, storage.DeleteOptions{}); err != nil {
		t.Errorf("Unexpected failure during deletion: %v", err)
	}

	if err := store.Get(ctx, key, storage.GetOptions{}, &example.Pod{}); !storage.IsNotFound(err) {
		t.Errorf("Unexpected error on reading object: %v", err)
	}
	updatedPod.ObjectMeta.ResourceVersion = out.ObjectMeta.ResourceVersion
	expectNoDiff(t, "incorrect pod:", updatedPod, out)
}

// RunTestDeleteWithConflict tests the case when another conflicting update happened before the delete completed.
func RunTestDeleteWithConflict(ctx context.Context, t *testing.T, store storage.Interface) {
	key, _ := testPropagateStore(ctx, t, store, &example.Pod{ObjectMeta: metav1.ObjectMeta{Name: "name", Namespace: "test-ns"}})

	// First update, so originalPod is outdated.
	updatedPod := &example.Pod{}
	validateCount := 0
	updateCount := 0
	// Simulate a conflicting update in the middle of delete.
	validateAllWithUpdate := func(_ context.Context, _ runtime.Object) error {
		validateCount++
		if validateCount > 1 {
			return nil
		}
		if err := store.GuaranteedUpdate(ctx, key, updatedPod, false, nil,
			storage.SimpleUpdate(func(obj runtime.Object) (runtime.Object, error) {
				pod := obj.(*example.Pod)
				pod.ObjectMeta.Labels = map[string]string{"foo": "bar"}
				return pod, nil
			}), nil); err != nil {
			t.Errorf("Unexpected failure during updated: %v", err)
		}
		updateCount++
		return nil
	}

	out := &example.Pod{}
	if err := store.Delete(ctx, key, out, nil, validateAllWithUpdate, nil, storage.DeleteOptions{}); err != nil {
		t.Errorf("Unexpected failure during deletion: %v", err)
	}

	if validateCount != 2 {
		t.Errorf("Expect validateCount = %d, but got %d", 2, validateCount)
	}
	if updateCount != 1 {
		t.Errorf("Expect updateCount = %d, but got %d", 1, updateCount)
	}

	if err := store.Get(ctx, key, storage.GetOptions{}, &example.Pod{}); !storage.IsNotFound(err) {
		t.Errorf("Unexpected error on reading object: %v", err)
	}
	updatedPod.ObjectMeta.ResourceVersion = out.ObjectMeta.ResourceVersion
	expectNoDiff(t, "incorrect pod:", updatedPod, out)
}

func RunTestDeleteWithSuggestionOfDeletedObject(ctx context.Context, t *testing.T, store storage.Interface) {
	key, originalPod := testPropagateStore(ctx, t, store, &example.Pod{ObjectMeta: metav1.ObjectMeta{Name: "name", Namespace: "test-ns"}})

	// First delete, so originalPod is outdated.
	deletedPod := &example.Pod{}
	if err := store.Delete(ctx, key, deletedPod, nil, storage.ValidateAllObjectFunc, originalPod, storage.DeleteOptions{}); err != nil {
		t.Errorf("Unexpected failure during deletion: %v", err)
	}

	// Now try deleting with stale object.
	out := &example.Pod{}
	if err := store.Delete(ctx, key, out, nil, storage.ValidateAllObjectFunc, originalPod, storage.DeleteOptions{}); !storage.IsNotFound(err) {
		t.Errorf("Unexpected error during deletion: %v, expected not-found", err)
	}
}

func RunTestValidateDeletionWithSuggestion(ctx context.Context, t *testing.T, store storage.Interface) {
	key, originalPod := testPropagateStore(ctx, t, store, &example.Pod{ObjectMeta: metav1.ObjectMeta{Name: "name", Namespace: "test-ns"}})

	// Check that validating fresh object fails is called once and fails.
	validationCalls := 0
	validationError := fmt.Errorf("validation error")
	validateNothing := func(_ context.Context, _ runtime.Object) error {
		validationCalls++
		return validationError
	}
	out := &example.Pod{}
	// nolint:errorlint // not changing the level of assertion
	if err := store.Delete(ctx, key, out, nil, validateNothing, originalPod, storage.DeleteOptions{}); err != validationError {
		t.Errorf("Unexpected failure during deletion: %v", err)
	}
	if validationCalls != 1 {
		t.Errorf("validate function should have been called once, called %d", validationCalls)
	}

	// First update, so originalPod is outdated.
	updatedPod := &example.Pod{}
	if err := store.GuaranteedUpdate(ctx, key, updatedPod, false, nil,
		storage.SimpleUpdate(func(obj runtime.Object) (runtime.Object, error) {
			pod := obj.(*example.Pod)
			pod.ObjectMeta.Labels = map[string]string{"foo": "bar"}
			return pod, nil
		}), nil); err != nil {
		t.Errorf("Unexpected failure during updated: %v", err)
	}

	calls := 0
	validateFresh := func(_ context.Context, obj runtime.Object) error {
		calls++
		pod := obj.(*example.Pod)
		if pod.ObjectMeta.Labels == nil || pod.ObjectMeta.Labels["foo"] != "bar" {
			return fmt.Errorf("stale object")
		}
		return nil
	}

	if err := store.Delete(ctx, key, out, nil, validateFresh, originalPod, storage.DeleteOptions{}); err != nil {
		t.Errorf("Unexpected failure during deletion: %v", err)
	}

	// Implementations of the storage interface are allowed to ignore the suggestion,
	// in which case just one validation call is possible.
	if calls > 2 {
		t.Errorf("validate function should have been called at most twice, called %d", calls)
	}

	if err := store.Get(ctx, key, storage.GetOptions{}, &example.Pod{}); !storage.IsNotFound(err) {
		t.Errorf("Unexpected error on reading object: %v", err)
	}
}

// RunTestValidateDeletionWithOnlySuggestionValid tests the case of delete with validateDeletion function,
// when the suggested cachedExistingObject passes the validate function while the current version does not pass the validate function.
func RunTestValidateDeletionWithOnlySuggestionValid(ctx context.Context, t *testing.T, store storage.Interface) {
	key, originalPod := testPropagateStore(ctx, t, store, &example.Pod{ObjectMeta: metav1.ObjectMeta{Name: "name", Namespace: "test-ns", Labels: map[string]string{"foo": "bar"}}})

	// Check that validating fresh object fails is called once and fails.
	validationCalls := 0
	validationError := fmt.Errorf("validation error")
	validateNothing := func(_ context.Context, _ runtime.Object) error {
		validationCalls++
		return validationError
	}
	out := &example.Pod{}
	// nolint:errorlint // not changing the level of assertion
	if err := store.Delete(ctx, key, out, nil, validateNothing, originalPod, storage.DeleteOptions{}); err != validationError {
		t.Errorf("Unexpected failure during deletion: %v", err)
	}
	if validationCalls != 1 {
		t.Errorf("validate function should have been called once, called %d", validationCalls)
	}

	// First update, so originalPod is outdated.
	updatedPod := &example.Pod{}
	if err := store.GuaranteedUpdate(ctx, key, updatedPod, false, nil,
		storage.SimpleUpdate(func(obj runtime.Object) (runtime.Object, error) {
			pod := obj.(*example.Pod)
			pod.ObjectMeta.Labels = map[string]string{"foo": "barbar"}
			return pod, nil
		}), nil); err != nil {
		t.Errorf("Unexpected failure during updated: %v", err)
	}

	calls := 0
	validateFresh := func(_ context.Context, obj runtime.Object) error {
		calls++
		pod := obj.(*example.Pod)
		if pod.ObjectMeta.Labels == nil || pod.ObjectMeta.Labels["foo"] != "bar" {
			return fmt.Errorf("stale object")
		}
		return nil
	}

	err := store.Delete(ctx, key, out, nil, validateFresh, originalPod, storage.DeleteOptions{})
	if err == nil || err.Error() != "stale object" {
		t.Errorf("expecting stale object error, but get: %s", err)
	}

	// Implementations of the storage interface are allowed to ignore the suggestion,
	// in which case just one validation call is possible.
	if calls > 2 {
		t.Errorf("validate function should have been called at most twice, called %d", calls)
	}

	if err = store.Get(ctx, key, storage.GetOptions{}, out); err != nil {
		t.Errorf("Unexpected error on reading object: %v", err)
	}
	expectNoDiff(t, "incorrect pod:", updatedPod, out)
}

func RunTestPreconditionalDeleteWithSuggestion(ctx context.Context, t *testing.T, store storage.Interface) {
	key, originalPod := testPropagateStore(ctx, t, store, &example.Pod{ObjectMeta: metav1.ObjectMeta{Name: "name", Namespace: "test-ns"}})

	// First update, so originalPod is outdated.
	updatedPod := &example.Pod{}
	if err := store.GuaranteedUpdate(ctx, key, updatedPod, false, nil,
		storage.SimpleUpdate(func(obj runtime.Object) (runtime.Object, error) {
			pod := obj.(*example.Pod)
			pod.ObjectMeta.UID = "myUID"
			return pod, nil
		}), nil); err != nil {
		t.Errorf("Unexpected failure during updated: %v", err)
	}

	prec := storage.NewUIDPreconditions("myUID")

	out := &example.Pod{}
	if err := store.Delete(ctx, key, out, prec, storage.ValidateAllObjectFunc, originalPod, storage.DeleteOptions{}); err != nil {
		t.Errorf("Unexpected failure during deletion: %v", err)
	}

	if err := store.Get(ctx, key, storage.GetOptions{}, &example.Pod{}); !storage.IsNotFound(err) {
		t.Errorf("Unexpected error on reading object: %v", err)
	}
}

// RunTestPreconditionalDeleteWithOnlySuggestionPass tests the case of delete with preconditions,
// when the suggested cachedExistingObject passes the preconditions while the current version does not pass the preconditions.
func RunTestPreconditionalDeleteWithOnlySuggestionPass(ctx context.Context, t *testing.T, store storage.Interface) {
	key, originalPod := testPropagateStore(ctx, t, store, &example.Pod{ObjectMeta: metav1.ObjectMeta{Name: "name", Namespace: "test-ns", UID: "myUID"}})

	// First update, so originalPod is outdated.
	updatedPod := &example.Pod{}
	if err := store.GuaranteedUpdate(ctx, key, updatedPod, false, nil,
		storage.SimpleUpdate(func(obj runtime.Object) (runtime.Object, error) {
			pod := obj.(*example.Pod)
			pod.ObjectMeta.UID = "otherUID"
			return pod, nil
		}), nil); err != nil {
		t.Errorf("Unexpected failure during updated: %v", err)
	}

	prec := storage.NewUIDPreconditions("myUID")
	// Although originalPod passes the precondition, its delete would fail due to conflict.
	// The 2nd try with updatedPod would fail the precondition.
	out := &example.Pod{}
	err := store.Delete(ctx, key, out, prec, storage.ValidateAllObjectFunc, originalPod, storage.DeleteOptions{})
	if err == nil || !storage.IsInvalidObj(err) {
		t.Errorf("expecting invalid UID error, but get: %s", err)
	}

	if err = store.Get(ctx, key, storage.GetOptions{}, out); err != nil {
		t.Errorf("Unexpected error on reading object: %v", err)
	}
	expectNoDiff(t, "incorrect pod:", updatedPod, out)
}

func RunTestList(ctx context.Context, t *testing.T, store storage.Interface, compaction Compaction, ignoreWatchCacheTests bool) {
	initialRV, preset, err := seedMultiLevelData(ctx, store)
	if err != nil {
		t.Fatal(err)
	}

	list := &example.PodList{}
	storageOpts := storage.ListOptions{
		// Ensure we're listing from "now".
		ResourceVersion: "",
		Predicate:       storage.Everything,
		Recursive:       true,
	}
	if err := store.GetList(ctx, "/second", storageOpts, list); err != nil {
		t.Errorf("Unexpected error: %v", err)
	}
	continueRV, _ := strconv.Atoi(list.ResourceVersion)
	secondContinuation, err := storage.EncodeContinue("/second/foo", "/second/", int64(continueRV))
	if err != nil {
		t.Fatal(err)
	}

	getAttrs := func(obj runtime.Object) (labels.Set, fields.Set, error) {
		pod := obj.(*example.Pod)
		return nil, fields.Set{"metadata.name": pod.Name, "spec.nodeName": pod.Spec.NodeName}, nil
	}
	// Use compact to increase etcd global revision without changes to any resources.
	// The increase in resources version comes from Kubernetes compaction updating hidden key.
	// Used to test consistent List to confirm it returns latest etcd revision.
	compaction(ctx, t, initialRV)
	currentRV := fmt.Sprintf("%d", continueRV+1)

	tests := []struct {
		name                       string
		rv                         string
		rvMatch                    metav1.ResourceVersionMatch
		prefix                     string
		pred                       storage.SelectionPredicate
		ignoreForWatchCache        bool
		expectedOut                []example.Pod
		expectedAlternatives       [][]example.Pod
		expectContinue             bool
		expectedRemainingItemCount *int64
		expectError                bool
		expectRVTooLarge           bool
		expectRV                   string
		expectRVFunc               func(string) error
	}{
		{
			name:        "rejects invalid resource version",
			prefix:      "/pods",
			pred:        storage.Everything,
			rv:          "abc",
			expectError: true,
		},
		{
			name:   "rejects resource version and continue token",
			prefix: "/pods",
			pred: storage.SelectionPredicate{
				Label:    labels.Everything(),
				Field:    fields.Everything(),
				Limit:    1,
				Continue: secondContinuation,
			},
			rv:          "1",
			expectError: true,
		},
		{
			name:             "rejects resource version set too high",
			prefix:           "/pods",
			rv:               strconv.FormatInt(math.MaxInt64, 10),
			expectRVTooLarge: true,
		},
		{
			name:        "test List on existing key",
			prefix:      "/pods/first/",
			pred:        storage.Everything,
			expectedOut: []example.Pod{*preset[0]},
		},
		{
			name:                 "test List on existing key with resource version set to 0",
			prefix:               "/pods/first/",
			pred:                 storage.Everything,
			expectedAlternatives: [][]example.Pod{{}, {*preset[0]}},
			rv:                   "0",
		},
		{
			name:        "test List on existing key with resource version set before first write, match=Exact",
			prefix:      "/pods/first/",
			pred:        storage.Everything,
			expectedOut: []example.Pod{},
			rv:          initialRV,
			rvMatch:     metav1.ResourceVersionMatchExact,
			expectRV:    initialRV,
		},
		{
			name:                 "test List on existing key with resource version set to 0, match=NotOlderThan",
			prefix:               "/pods/first/",
			pred:                 storage.Everything,
			expectedAlternatives: [][]example.Pod{{}, {*preset[0]}},
			rv:                   "0",
			rvMatch:              metav1.ResourceVersionMatchNotOlderThan,
		},
		{
			name:        "test List on existing key with resource version set to 0, match=Invalid",
			prefix:      "/pods/first/",
			pred:        storage.Everything,
			rv:          "0",
			rvMatch:     "Invalid",
			expectError: true,
		},
		{
			name:                 "test List on existing key with resource version set before first write, match=NotOlderThan",
			prefix:               "/pods/first/",
			pred:                 storage.Everything,
			expectedAlternatives: [][]example.Pod{{}, {*preset[0]}},
			rv:                   initialRV,
			rvMatch:              metav1.ResourceVersionMatchNotOlderThan,
		},
		{
			name:        "test List on existing key with resource version set before first write, match=Invalid",
			prefix:      "/pods/first/",
			pred:        storage.Everything,
			rv:          initialRV,
			rvMatch:     "Invalid",
			expectError: true,
		},
		{
			name:        "test List on existing key with resource version set to current resource version",
			prefix:      "/pods/first/",
			pred:        storage.Everything,
			expectedOut: []example.Pod{*preset[0]},
			rv:          list.ResourceVersion,
		},
		{
			name:        "test List on existing key with resource version set to current resource version, match=Exact",
			prefix:      "/pods/first/",
			pred:        storage.Everything,
			expectedOut: []example.Pod{*preset[0]},
			rv:          list.ResourceVersion,
			rvMatch:     metav1.ResourceVersionMatchExact,
			expectRV:    list.ResourceVersion,
		},
		{
			name:        "test List on existing key with resource version set to current resource version, match=NotOlderThan",
			prefix:      "/pods/first/",
			pred:        storage.Everything,
			expectedOut: []example.Pod{*preset[0]},
			rv:          list.ResourceVersion,
			rvMatch:     metav1.ResourceVersionMatchNotOlderThan,
		},
		{
			name:        "test List on non-existing key",
			prefix:      "/pods/non-existing/",
			pred:        storage.Everything,
			expectedOut: []example.Pod{},
		},
		{
			name:   "test List with pod name matching",
			prefix: "/pods/first/",
			pred: storage.SelectionPredicate{
				Label: labels.Everything(),
				Field: fields.ParseSelectorOrDie("metadata.name!=bar"),
			},
			expectedOut: []example.Pod{},
		},
		{
			name:   "test List with pod name matching with resource version set to current resource version, match=NotOlderThan",
			prefix: "/pods/first/",
			pred: storage.SelectionPredicate{
				Label: labels.Everything(),
				Field: fields.ParseSelectorOrDie("metadata.name!=bar"),
			},
			expectedOut: []example.Pod{},
			rv:          list.ResourceVersion,
			rvMatch:     metav1.ResourceVersionMatchNotOlderThan,
		},
		{
			name:   "test List with limit",
			prefix: "/pods/second/",
			pred: storage.SelectionPredicate{
				Label: labels.Everything(),
				Field: fields.Everything(),
				Limit: 1,
			},
			expectedOut:                []example.Pod{*preset[1]},
			expectContinue:             true,
			expectedRemainingItemCount: utilpointer.Int64(1),
		},
		{
			name:   "test List with limit at current resource version",
			prefix: "/pods/second/",
			pred: storage.SelectionPredicate{
				Label: labels.Everything(),
				Field: fields.Everything(),
				Limit: 1,
			},
			expectedOut:                []example.Pod{*preset[1]},
			expectContinue:             true,
			expectedRemainingItemCount: utilpointer.Int64(1),
			rv:                         list.ResourceVersion,
			expectRV:                   list.ResourceVersion,
		},
		{
			name:   "test List with limit at current resource version and match=Exact",
			prefix: "/pods/second/",
			pred: storage.SelectionPredicate{
				Label: labels.Everything(),
				Field: fields.Everything(),
				Limit: 1,
			},
			expectedOut:                []example.Pod{*preset[1]},
			expectContinue:             true,
			expectedRemainingItemCount: utilpointer.Int64(1),
			rv:                         list.ResourceVersion,
			rvMatch:                    metav1.ResourceVersionMatchExact,
			expectRV:                   list.ResourceVersion,
		},
		{
			name:   "test List with limit at current resource version and match=NotOlderThan",
			prefix: "/pods/second/",
			pred: storage.SelectionPredicate{
				Label: labels.Everything(),
				Field: fields.Everything(),
				Limit: 1,
			},
			expectedOut:                []example.Pod{*preset[1]},
			expectContinue:             true,
			expectedRemainingItemCount: utilpointer.Int64(1),
			rv:                         list.ResourceVersion,
			rvMatch:                    metav1.ResourceVersionMatchNotOlderThan,
			expectRVFunc:               resourceVersionNotOlderThan(list.ResourceVersion),
		},
		{
			name:   "test List with limit at resource version 0",
			prefix: "/pods/second/",
			pred: storage.SelectionPredicate{
				Label: labels.Everything(),
				Field: fields.Everything(),
				Limit: 1,
			},
			// TODO(#108003): As of now, watchcache is deliberately ignoring
			// limit if RV=0 is specified, returning whole list of objects.
			// While this should eventually get fixed, for now we're explicitly
			// ignoring this testcase for watchcache.
			ignoreForWatchCache:        true,
			expectedOut:                []example.Pod{*preset[1]},
			expectContinue:             true,
			expectedRemainingItemCount: utilpointer.Int64(1),
			rv:                         "0",
			expectRVFunc:               resourceVersionNotOlderThan(list.ResourceVersion),
		},
		{
			name:   "test List with limit at resource version 0 match=NotOlderThan",
			prefix: "/pods/second/",
			pred: storage.SelectionPredicate{
				Label: labels.Everything(),
				Field: fields.Everything(),
				Limit: 1,
			},
			// TODO(#108003): As of now, watchcache is deliberately ignoring
			// limit if RV=0 is specified, returning whole list of objects.
			// While this should eventually get fixed, for now we're explicitly
			// ignoring this testcase for watchcache.
			ignoreForWatchCache:        true,
			expectedOut:                []example.Pod{*preset[1]},
			expectContinue:             true,
			expectedRemainingItemCount: utilpointer.Int64(1),
			rv:                         "0",
			rvMatch:                    metav1.ResourceVersionMatchNotOlderThan,
			expectRVFunc:               resourceVersionNotOlderThan(list.ResourceVersion),
		},
		{
			name:   "test List with limit at resource version before first write and match=Exact",
			prefix: "/pods/second/",
			pred: storage.SelectionPredicate{
				Label: labels.Everything(),
				Field: fields.Everything(),
				Limit: 1,
			},
			expectedOut:    []example.Pod{},
			expectContinue: false,
			rv:             initialRV,
			rvMatch:        metav1.ResourceVersionMatchExact,
			expectRV:       initialRV,
		},
		{
			name:   "test List with pregenerated continue token",
			prefix: "/pods/second/",
			pred: storage.SelectionPredicate{
				Label:    labels.Everything(),
				Field:    fields.Everything(),
				Limit:    1,
				Continue: secondContinuation,
			},
			expectedOut: []example.Pod{*preset[2]},
		},
		{
			name:   "ignores resource version 0 for List with pregenerated continue token",
			prefix: "/pods/second/",
			pred: storage.SelectionPredicate{
				Label:    labels.Everything(),
				Field:    fields.Everything(),
				Limit:    1,
				Continue: secondContinuation,
			},
			rv:          "0",
			expectedOut: []example.Pod{*preset[2]},
		},
		{
			name:        "test List with multiple levels of directories and expect flattened result",
			prefix:      "/pods/second/",
			pred:        storage.Everything,
			expectedOut: []example.Pod{*preset[1], *preset[2]},
		},
		{
			name:        "test List with multiple levels of directories and expect flattened result with current resource version and match=NotOlderThan",
			prefix:      "/pods/second/",
			pred:        storage.Everything,
			rv:          list.ResourceVersion,
			rvMatch:     metav1.ResourceVersionMatchNotOlderThan,
			expectedOut: []example.Pod{*preset[1], *preset[2]},
		},
		{
			name:   "test List with filter returning only one item, ensure only a single page returned",
			prefix: "/pods",
			pred: storage.SelectionPredicate{
				Field: fields.OneTermEqualSelector("metadata.name", "barfoo"),
				Label: labels.Everything(),
				Limit: 1,
			},
			expectedOut:    []example.Pod{*preset[3]},
			expectContinue: true,
		},
		{
			name:   "test List with filter returning only one item, ensure only a single page returned with current resource version and match=NotOlderThan",
			prefix: "/pods",
			pred: storage.SelectionPredicate{
				Field: fields.OneTermEqualSelector("metadata.name", "barfoo"),
				Label: labels.Everything(),
				Limit: 1,
			},
			rv:             list.ResourceVersion,
			rvMatch:        metav1.ResourceVersionMatchNotOlderThan,
			expectedOut:    []example.Pod{*preset[3]},
			expectContinue: true,
		},
		{
			name:   "test List with filter returning only one item, covers the entire list",
			prefix: "/pods",
			pred: storage.SelectionPredicate{
				Field: fields.OneTermEqualSelector("metadata.name", "barfoo"),
				Label: labels.Everything(),
				Limit: 2,
			},
			expectedOut:    []example.Pod{*preset[3]},
			expectContinue: false,
		},
		{
			name:   "test List with filter returning only one item, covers the entire list with current resource version and match=NotOlderThan",
			prefix: "/pods",
			pred: storage.SelectionPredicate{
				Field: fields.OneTermEqualSelector("metadata.name", "barfoo"),
				Label: labels.Everything(),
				Limit: 2,
			},
			rv:             list.ResourceVersion,
			rvMatch:        metav1.ResourceVersionMatchNotOlderThan,
			expectedOut:    []example.Pod{*preset[3]},
			expectContinue: false,
		},
		{
			name:   "test List with filter returning only one item, covers the entire list, with resource version 0",
			prefix: "/pods",
			pred: storage.SelectionPredicate{
				Field: fields.OneTermEqualSelector("metadata.name", "barfoo"),
				Label: labels.Everything(),
				Limit: 2,
			},
			rv:                   "0",
			expectedAlternatives: [][]example.Pod{{}, {*preset[3]}},
			expectContinue:       false,
		},
		{
			name:   "test List with filter returning two items, more pages possible",
			prefix: "/pods",
			pred: storage.SelectionPredicate{
				Field: fields.OneTermEqualSelector("metadata.name", "bar"),
				Label: labels.Everything(),
				Limit: 2,
			},
			expectContinue: true,
			expectedOut:    []example.Pod{*preset[0], *preset[1]},
		},
		{
			name:   "test List with filter returning two items, more pages possible with current resource version and match=NotOlderThan",
			prefix: "/pods",
			pred: storage.SelectionPredicate{
				Field: fields.OneTermEqualSelector("metadata.name", "bar"),
				Label: labels.Everything(),
				Limit: 2,
			},
			rv:             list.ResourceVersion,
			rvMatch:        metav1.ResourceVersionMatchNotOlderThan,
			expectContinue: true,
			expectedOut:    []example.Pod{*preset[0], *preset[1]},
		},
		{
			name:   "filter returns two items split across multiple pages",
			prefix: "/pods",
			pred: storage.SelectionPredicate{
				Field: fields.OneTermEqualSelector("metadata.name", "foo"),
				Label: labels.Everything(),
				Limit: 2,
			},
			expectedOut: []example.Pod{*preset[2], *preset[4]},
		},
		{
			name:   "filter returns two items split across multiple pages with current resource version and match=NotOlderThan",
			prefix: "/pods",
			pred: storage.SelectionPredicate{
				Field: fields.OneTermEqualSelector("metadata.name", "foo"),
				Label: labels.Everything(),
				Limit: 2,
			},
			rv:          list.ResourceVersion,
			rvMatch:     metav1.ResourceVersionMatchNotOlderThan,
			expectedOut: []example.Pod{*preset[2], *preset[4]},
		},
		{
			name:   "filter returns one item for last page, ends on last item, not full",
			prefix: "/pods",
			pred: storage.SelectionPredicate{
				Field:    fields.OneTermEqualSelector("metadata.name", "foo"),
				Label:    labels.Everything(),
				Limit:    2,
				Continue: encodeContinueOrDie("third/barfoo", int64(continueRV)),
			},
			expectedOut: []example.Pod{*preset[4]},
		},
		{
			name:   "filter returns one item for last page, starts on last item, full",
			prefix: "/pods",
			pred: storage.SelectionPredicate{
				Field:    fields.OneTermEqualSelector("metadata.name", "foo"),
				Label:    labels.Everything(),
				Limit:    1,
				Continue: encodeContinueOrDie("third/barfoo", int64(continueRV)),
			},
			expectedOut: []example.Pod{*preset[4]},
		},
		{
			name:   "filter returns one item for last page, starts on last item, partial page",
			prefix: "/pods",
			pred: storage.SelectionPredicate{
				Field:    fields.OneTermEqualSelector("metadata.name", "foo"),
				Label:    labels.Everything(),
				Limit:    2,
				Continue: encodeContinueOrDie("third/barfoo", int64(continueRV)),
			},
			expectedOut: []example.Pod{*preset[4]},
		},
		{
			name:   "filter returns two items, page size equal to total list size",
			prefix: "/pods",
			pred: storage.SelectionPredicate{
				Field: fields.OneTermEqualSelector("metadata.name", "foo"),
				Label: labels.Everything(),
				Limit: 5,
			},
			expectedOut: []example.Pod{*preset[2], *preset[4]},
		},
		{
			name:   "filter returns two items, page size equal to total list size with current resource version and match=NotOlderThan",
			prefix: "/pods",
			pred: storage.SelectionPredicate{
				Field: fields.OneTermEqualSelector("metadata.name", "foo"),
				Label: labels.Everything(),
				Limit: 5,
			},
			rv:          list.ResourceVersion,
			rvMatch:     metav1.ResourceVersionMatchNotOlderThan,
			expectedOut: []example.Pod{*preset[2], *preset[4]},
		},
		{
			name:   "filter returns one item, page size equal to total list size",
			prefix: "/pods",
			pred: storage.SelectionPredicate{
				Field: fields.OneTermEqualSelector("metadata.name", "barfoo"),
				Label: labels.Everything(),
				Limit: 5,
			},
			expectedOut: []example.Pod{*preset[3]},
		},
		{
			name:   "filter returns one item, page size equal to total list size with current resource version and match=NotOlderThan",
			prefix: "/pods",
			pred: storage.SelectionPredicate{
				Field: fields.OneTermEqualSelector("metadata.name", "barfoo"),
				Label: labels.Everything(),
				Limit: 5,
			},
			rv:          list.ResourceVersion,
			rvMatch:     metav1.ResourceVersionMatchNotOlderThan,
			expectedOut: []example.Pod{*preset[3]},
		},
		{
			name:        "list all items",
			prefix:      "/pods",
			pred:        storage.Everything,
			expectedOut: []example.Pod{*preset[0], *preset[1], *preset[2], *preset[3], *preset[4]},
		},
		{
			name:        "list all items with current resource version and match=NotOlderThan",
			prefix:      "/pods",
			pred:        storage.Everything,
			rv:          list.ResourceVersion,
			rvMatch:     metav1.ResourceVersionMatchNotOlderThan,
			expectedOut: []example.Pod{*preset[0], *preset[1], *preset[2], *preset[3], *preset[4]},
		},
		{
			name:   "verify list returns updated version of object; filter returns one item, page size equal to total list size with current resource version and match=NotOlderThan",
			prefix: "/pods",
			pred: storage.SelectionPredicate{
				Field: fields.OneTermEqualSelector("spec.nodeName", "fakeNode"),
				Label: labels.Everything(),
				Limit: 5,
			},
			rv:          list.ResourceVersion,
			rvMatch:     metav1.ResourceVersionMatchNotOlderThan,
			expectedOut: []example.Pod{*preset[0]},
		},
		{
			name:   "verify list does not return deleted object; filter for deleted object, page size equal to total list size with current resource version and match=NotOlderThan",
			prefix: "/pods",
			pred: storage.SelectionPredicate{
				Field: fields.OneTermEqualSelector("metadata.name", "baz"),
				Label: labels.Everything(),
				Limit: 5,
			},
			rv:          list.ResourceVersion,
			rvMatch:     metav1.ResourceVersionMatchNotOlderThan,
			expectedOut: []example.Pod{},
		},
		{
			name:        "test consistent List",
			prefix:      "/pods/empty",
			pred:        storage.Everything,
			rv:          "",
			expectRV:    currentRV,
			expectedOut: []example.Pod{},
		},
		{
			name:         "test non-consistent List",
			prefix:       "/pods/empty",
			pred:         storage.Everything,
			rv:           "0",
			expectRVFunc: resourceVersionNotOlderThan(list.ResourceVersion),
			expectedOut:  []example.Pod{},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			// For some asynchronous implementations of storage interface (in particular watchcache),
			// certain requests may impact result of further requests. As an example, if we first
			// ensure that watchcache is synchronized up to ResourceVersion X (using Get/List requests
			// with NotOlderThan semantic), the further requests (even specifying earlier resource
			// version) will also return the result synchronized to at least ResourceVersion X.
			// By parallelizing test cases we ensure that the order in which test cases are defined
			// doesn't automatically preclude some scenarios from happening.
			t.Parallel()

			if ignoreWatchCacheTests && tt.ignoreForWatchCache {
				t.Skip()
			}

			if tt.pred.GetAttrs == nil {
				tt.pred.GetAttrs = getAttrs
			}

			out := &example.PodList{}
			storageOpts := storage.ListOptions{
				ResourceVersion:      tt.rv,
				ResourceVersionMatch: tt.rvMatch,
				Predicate:            tt.pred,
				Recursive:            true,
			}
			err := store.GetList(ctx, tt.prefix, storageOpts, out)
			if tt.expectRVTooLarge {
				if err == nil || !apierrors.IsTimeout(err) || !storage.IsTooLargeResourceVersion(err) {
					t.Fatalf("expecting resource version too high error, but get: %s", err)
				}
				return
			}

			if err != nil {
				if !tt.expectError {
					t.Fatalf("GetList failed: %v", err)
				}
				return
			}
			if tt.expectError {
				t.Fatalf("expected error but got none")
			}
			if (len(out.Continue) > 0) != tt.expectContinue {
				t.Errorf("unexpected continue token: %q", out.Continue)
			}

			// If a client requests an exact resource version, it must be echoed back to them.
			if tt.expectRV != "" {
				if tt.expectRV != out.ResourceVersion {
					t.Errorf("resourceVersion in list response want=%s, got=%s", tt.expectRV, out.ResourceVersion)
				}
			}
			if tt.expectRVFunc != nil {
				if err := tt.expectRVFunc(out.ResourceVersion); err != nil {
					t.Errorf("resourceVersion in list response invalid: %v", err)
				}
			}

			if tt.expectedAlternatives == nil {
				sort.Sort(sortablePodList(tt.expectedOut))
				expectNoDiff(t, "incorrect list pods", tt.expectedOut, out.Items)
			} else {
				ExpectContains(t, "incorrect list pods", toInterfaceSlice(tt.expectedAlternatives), out.Items)
			}
			if !cmp.Equal(tt.expectedRemainingItemCount, out.RemainingItemCount) {
				t.Fatalf("unexpected remainingItemCount, diff: %s", cmp.Diff(tt.expectedRemainingItemCount, out.RemainingItemCount))
			}
		})
	}
}

func RunTestConsistentList(ctx context.Context, t *testing.T, store storage.Interface, compaction Compaction, cacheEnabled, consistentReadsSupported bool) {
	outPod := &example.Pod{}
	inPod := &example.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "foo"}}
	err := store.Create(ctx, computePodKey(inPod), inPod, outPod, 0)
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}
	lastObjecRV := outPod.ResourceVersion
	compaction(ctx, t, outPod.ResourceVersion)
	parsedRV, _ := strconv.Atoi(outPod.ResourceVersion)
	currentRV := fmt.Sprintf("%d", parsedRV+1)

	firstNonConsistentReadRV := lastObjecRV
	if consistentReadsSupported && !cacheEnabled {
		firstNonConsistentReadRV = currentRV
	}

	secondNonConsistentReadRV := lastObjecRV
	if consistentReadsSupported {
		secondNonConsistentReadRV = currentRV
	}

	tcs := []struct {
		name             string
		requestRV        string
		expectResponseRV string
	}{
		{
			name:             "Non-consistent list before sync",
			requestRV:        "0",
			expectResponseRV: firstNonConsistentReadRV,
		},
		{
			name:             "Consistent request returns currentRV",
			requestRV:        "",
			expectResponseRV: currentRV,
		},
		{
			name:             "Non-consistent request after sync returns currentRV",
			requestRV:        "0",
			expectResponseRV: secondNonConsistentReadRV,
		},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			out := &example.PodList{}
			opts := storage.ListOptions{
				ResourceVersion: tc.requestRV,
				Predicate:       storage.Everything,
			}
			err = store.GetList(ctx, "/pods/empty", opts, out)
			if err != nil {
				t.Fatalf("GetList failed: %v", err)
			}
			if out.ResourceVersion != tc.expectResponseRV {
				t.Errorf("resourceVersion in list response want=%s, got=%s", tc.expectResponseRV, out.ResourceVersion)
			}
		})
	}
}

// seedMultiLevelData creates a set of keys with a multi-level structure, returning a resourceVersion
// from before any were created along with the full set of objects that were persisted
func seedMultiLevelData(ctx context.Context, store storage.Interface) (string, []*example.Pod, error) {
	// Setup storage with the following structure:
	//  /
	//   - first/
	//  |         - bar
	//  |
	//   - second/
	//  |         - bar
	//  |         - foo
	//  |         - [deleted] baz
	//  |
	//   - third/
	//  |         - barfoo
	//  |         - foo
	barFirst := &example.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "first", Name: "bar"}}
	barSecond := &example.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "second", Name: "bar"}}
	fooSecond := &example.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "second", Name: "foo"}}
	bazSecond := &example.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "second", Name: "baz"}}
	barfooThird := &example.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "third", Name: "barfoo"}}
	fooThird := &example.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "third", Name: "foo"}}

	preset := []struct {
		key       string
		obj       *example.Pod
		storedObj *example.Pod
	}{
		{
			key: computePodKey(barFirst),
			obj: barFirst,
		},
		{
			key: computePodKey(barSecond),
			obj: barSecond,
		},
		{
			key: computePodKey(fooSecond),
			obj: fooSecond,
		},
		{
			key: computePodKey(barfooThird),
			obj: barfooThird,
		},
		{
			key: computePodKey(fooThird),
			obj: fooThird,
		},
		{
			key: computePodKey(bazSecond),
			obj: bazSecond,
		},
	}

	// we want to figure out the resourceVersion before we create anything
	initialList := &example.PodList{}
	if err := store.GetList(ctx, "/pods", storage.ListOptions{Predicate: storage.Everything, Recursive: true}, initialList); err != nil {
		return "", nil, fmt.Errorf("failed to determine starting resourceVersion: %w", err)
	}
	initialRV := initialList.ResourceVersion

	for i, ps := range preset {
		preset[i].storedObj = &example.Pod{}
		err := store.Create(ctx, ps.key, ps.obj, preset[i].storedObj, 0)
		if err != nil {
			return "", nil, fmt.Errorf("failed to create object: %w", err)
		}
	}

	// For barFirst, we first create it with key /pods/first/bar and then we update
	// it by changing its spec.nodeName. The point of doing this is to be able to
	// test that if a pod with key /pods/first/bar is in fact returned, the returned
	// pod is the updated one (i.e. with spec.nodeName changed).
	preset[0].storedObj = &example.Pod{}
	if err := store.GuaranteedUpdate(ctx, computePodKey(barFirst), preset[0].storedObj, true, nil,
		func(input runtime.Object, _ storage.ResponseMeta) (output runtime.Object, ttl *uint64, err error) {
			pod := input.(*example.Pod).DeepCopy()
			pod.Spec.NodeName = "fakeNode"
			return pod, nil, nil
		}, nil); err != nil {
		return "", nil, fmt.Errorf("failed to update object: %w", err)
	}

	// We now delete bazSecond provided it has been created first. We do this to enable
	// testing cases that had an object exist initially and then was deleted and how this
	// would be reflected in responses of different calls.
	if err := store.Delete(ctx, computePodKey(bazSecond), preset[len(preset)-1].storedObj, nil, storage.ValidateAllObjectFunc, nil, storage.DeleteOptions{}); err != nil {
		return "", nil, fmt.Errorf("failed to delete object: %w", err)
	}

	// Since we deleted bazSecond (last element of preset), we remove it from preset.
	preset = preset[:len(preset)-1]
	var created []*example.Pod
	for _, item := range preset {
		created = append(created, item.storedObj)
	}
	return initialRV, created, nil
}

func RunTestGetListNonRecursive(ctx context.Context, t *testing.T, compaction Compaction, store storage.Interface) {
	key, prevStoredObj := testPropagateStore(ctx, t, store, &example.Pod{ObjectMeta: metav1.ObjectMeta{Name: "foo", Namespace: "test-ns"}})
	prevRV, _ := strconv.Atoi(prevStoredObj.ResourceVersion)

	storedObj := &example.Pod{}
	if err := store.GuaranteedUpdate(ctx, key, storedObj, false, nil,
		func(_ runtime.Object, _ storage.ResponseMeta) (runtime.Object, *uint64, error) {
			newPod := prevStoredObj.DeepCopy()
			newPod.Annotations = map[string]string{"version": "second"}
			return newPod, nil, nil
		}, nil); err != nil {
		t.Fatalf("update failed: %v", err)
	}
	objRV, _ := strconv.Atoi(storedObj.ResourceVersion)
	// Use compact to increase etcd global revision without changes to any resources.
	// The increase in resources version comes from Kubernetes compaction updating hidden key.
	// Used to test consistent List to confirm it returns latest etcd revision.
	compaction(ctx, t, prevStoredObj.ResourceVersion)

	tests := []struct {
		name                 string
		key                  string
		pred                 storage.SelectionPredicate
		expectedOut          []example.Pod
		expectedAlternatives [][]example.Pod
		rv                   string
		rvMatch              metav1.ResourceVersionMatch
		expectRVTooLarge     bool
	}{{
		name:        "existing key",
		key:         key,
		pred:        storage.Everything,
		expectedOut: []example.Pod{*storedObj},
	}, {
		name:                 "existing key, resourceVersion=0",
		key:                  key,
		pred:                 storage.Everything,
		expectedAlternatives: [][]example.Pod{{}, {*prevStoredObj}, {*storedObj}},
		rv:                   "0",
	}, {
		name:                 "existing key, resourceVersion=0, resourceVersionMatch=notOlderThan",
		key:                  key,
		pred:                 storage.Everything,
		expectedAlternatives: [][]example.Pod{{}, {*prevStoredObj}, {*storedObj}},
		rv:                   "0",
		rvMatch:              metav1.ResourceVersionMatchNotOlderThan,
	}, {
		name:        "existing key, resourceVersion=current",
		key:         key,
		pred:        storage.Everything,
		expectedOut: []example.Pod{*storedObj},
		rv:          fmt.Sprintf("%d", objRV),
	}, {
		name:        "existing key, resourceVersion=current, resourceVersionMatch=notOlderThan",
		key:         key,
		pred:        storage.Everything,
		expectedOut: []example.Pod{*storedObj},
		rv:          fmt.Sprintf("%d", objRV),
		rvMatch:     metav1.ResourceVersionMatchNotOlderThan,
	}, {
		name:                 "existing key, resourceVersion=previous, resourceVersionMatch=notOlderThan",
		key:                  key,
		pred:                 storage.Everything,
		expectedAlternatives: [][]example.Pod{{*prevStoredObj}, {*storedObj}},
		rv:                   fmt.Sprintf("%d", prevRV),
		rvMatch:              metav1.ResourceVersionMatchNotOlderThan,
	}, {
		name:        "existing key, resourceVersion=current, resourceVersionMatch=exact",
		key:         key,
		pred:        storage.Everything,
		expectedOut: []example.Pod{*storedObj},
		rv:          fmt.Sprintf("%d", objRV),
		rvMatch:     metav1.ResourceVersionMatchExact,
	}, {
		name:        "existing key, resourceVersion=previous, resourceVersionMatch=exact",
		key:         key,
		pred:        storage.Everything,
		expectedOut: []example.Pod{*prevStoredObj},
		rv:          fmt.Sprintf("%d", prevRV),
		rvMatch:     metav1.ResourceVersionMatchExact,
	}, {
		name:             "existing key, resourceVersion=too high",
		key:              key,
		pred:             storage.Everything,
		expectedOut:      []example.Pod{*storedObj},
		rv:               strconv.FormatInt(math.MaxInt64, 10),
		expectRVTooLarge: true,
	}, {
		name:        "non-existing key",
		key:         "/non-existing",
		pred:        storage.Everything,
		expectedOut: []example.Pod{},
	}, {
		name: "with matching pod name",
		key:  "/non-existing",
		pred: storage.SelectionPredicate{
			Label: labels.Everything(),
			Field: fields.ParseSelectorOrDie("metadata.name!=" + storedObj.Name),
			GetAttrs: func(obj runtime.Object) (labels.Set, fields.Set, error) {
				pod := obj.(*example.Pod)
				return nil, fields.Set{"metadata.name": pod.Name}, nil
			},
		},
		expectedOut: []example.Pod{},
	}, {
		name: "existing key, resourceVersion=current, with not matching pod name",
		key:  key,
		pred: storage.SelectionPredicate{
			Label: labels.Everything(),
			Field: fields.ParseSelectorOrDie("metadata.name!=" + storedObj.Name),
			GetAttrs: func(obj runtime.Object) (labels.Set, fields.Set, error) {
				pod := obj.(*example.Pod)
				return nil, fields.Set{"metadata.name": pod.Name}, nil
			},
		},
		expectedOut: []example.Pod{},
		rv:          fmt.Sprintf("%d", objRV),
	}}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			// For some asynchronous implementations of storage interface (in particular watchcache),
			// certain requests may impact result of further requests. As an example, if we first
			// ensure that watchcache is synchronized up to ResourceVersion X (using Get/List requests
			// with NotOlderThan semantic), the further requests (even specifying earlier resource
			// version) will also return the result synchronized to at least ResourceVersion X.
			// By parallelizing test cases we ensure that the order in which test cases are defined
			// doesn't automatically preclude some scenarios from happening.
			t.Parallel()

			out := &example.PodList{}
			storageOpts := storage.ListOptions{
				ResourceVersion:      tt.rv,
				ResourceVersionMatch: tt.rvMatch,
				Predicate:            tt.pred,
				Recursive:            false,
			}
			err := store.GetList(ctx, tt.key, storageOpts, out)

			if tt.expectRVTooLarge {
				if err == nil || !storage.IsTooLargeResourceVersion(err) {
					t.Errorf("%s: expecting resource version too high error, but get: %s", tt.name, err)
				}
				return
			}

			if err != nil {
				t.Fatalf("GetList failed: %v", err)
			}
			if len(out.ResourceVersion) == 0 {
				t.Errorf("%s: unset resourceVersion", tt.name)
			}

			if tt.expectedAlternatives == nil {
				expectNoDiff(t, "incorrect list pods", tt.expectedOut, out.Items)
			} else {
				ExpectContains(t, "incorrect list pods", toInterfaceSlice(tt.expectedAlternatives), out.Items)
			}
		})
	}
}

// RunTestGetListRecursivePrefix tests how recursive parameter works for object keys that are prefixes of each other.
func RunTestGetListRecursivePrefix(ctx context.Context, t *testing.T, store storage.Interface) {
	fooKey, fooObj := testPropagateStore(ctx, t, store, &example.Pod{ObjectMeta: metav1.ObjectMeta{Name: "foo", Namespace: "test-ns"}})
	fooBarKey, fooBarObj := testPropagateStore(ctx, t, store, &example.Pod{ObjectMeta: metav1.ObjectMeta{Name: "foobar", Namespace: "test-ns"}})
	_, otherNamespaceObj := testPropagateStore(ctx, t, store, &example.Pod{ObjectMeta: metav1.ObjectMeta{Name: "baz", Namespace: "test-ns2"}})
	lastRev := otherNamespaceObj.ResourceVersion

	tests := []struct {
		name        string
		key         string
		recursive   bool
		expectedOut []example.Pod
	}{
		{
			name:        "NonRecursive on resource prefix doesn't return any objects",
			key:         "/pods/",
			recursive:   false,
			expectedOut: []example.Pod{},
		},
		{
			name:        "Recursive on resource prefix returns all objects",
			key:         "/pods/",
			recursive:   true,
			expectedOut: []example.Pod{*fooObj, *fooBarObj, *otherNamespaceObj},
		},
		{
			name:        "NonRecursive on namespace prefix doesn't return any objects",
			key:         "/pods/test-ns/",
			recursive:   false,
			expectedOut: []example.Pod{},
		},
		{
			name:        "Recursive on resource prefix returns objects in the namespace",
			key:         "/pods/test-ns/",
			recursive:   true,
			expectedOut: []example.Pod{*fooObj, *fooBarObj},
		},
		{
			name:        "NonRecursive on object key (prefix) returns object and no other objects with the same prefix",
			key:         fooKey,
			recursive:   false,
			expectedOut: []example.Pod{*fooObj},
		},
		{
			name:        "Recursive on object key (prefix) doesn't return anything",
			key:         fooKey,
			recursive:   true,
			expectedOut: []example.Pod{},
		},
		{
			name:        "NonRecursive on object key (no-prefix) return object",
			key:         fooBarKey,
			recursive:   false,
			expectedOut: []example.Pod{*fooBarObj},
		},
		{
			name:        "Recursive on object key (no-prefix) doesn't return anything",
			key:         fooBarKey,
			recursive:   true,
			expectedOut: []example.Pod{},
		},
	}

	listTypes := []struct {
		name            string
		ResourceVersion string
		Match           metav1.ResourceVersionMatch
	}{
		{
			name:            "Exact",
			ResourceVersion: lastRev,
			Match:           metav1.ResourceVersionMatchExact,
		},
		{
			name:            "Consistent",
			ResourceVersion: "",
		},
		{
			name:            "NotOlderThan",
			ResourceVersion: "0",
			Match:           metav1.ResourceVersionMatchNotOlderThan,
		},
	}

	for _, listType := range listTypes {
		listType := listType
		t.Run(listType.name, func(t *testing.T) {
			for _, tt := range tests {
				tt := tt
				t.Run(tt.name, func(t *testing.T) {
					out := &example.PodList{}
					storageOpts := storage.ListOptions{
						ResourceVersion:      listType.ResourceVersion,
						ResourceVersionMatch: listType.Match,
						Recursive:            tt.recursive,
						Predicate:            storage.Everything,
					}
					err := store.GetList(ctx, tt.key, storageOpts, out)
					if err != nil {
						t.Fatalf("GetList failed: %v", err)
					}
					expectNoDiff(t, "incorrect list pods", tt.expectedOut, out.Items)
				})
			}
		})
	}
}

type CallsValidation func(t *testing.T, pageSize, estimatedProcessedObjects uint64)

func RunTestListContinuation(ctx context.Context, t *testing.T, store storage.Interface, validation CallsValidation) {
	// Setup storage with the following structure:
	//  /
	//   - first/
	//  |         - bar
	//  |
	//   - second/
	//  |         - bar
	//  |         - foo
	barFirst := &example.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "first", Name: "bar"}}
	barSecond := &example.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "second", Name: "bar"}}
	fooSecond := &example.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "second", Name: "foo"}}
	preset := []struct {
		key       string
		obj       *example.Pod
		storedObj *example.Pod
	}{
		{
			key: computePodKey(barFirst),
			obj: barFirst,
		},
		{
			key: computePodKey(barSecond),
			obj: barSecond,
		},
		{
			key: computePodKey(fooSecond),
			obj: fooSecond,
		},
	}

	var currentRV string
	for i, ps := range preset {
		preset[i].storedObj = &example.Pod{}
		err := store.Create(ctx, ps.key, ps.obj, preset[i].storedObj, 0)
		if err != nil {
			t.Fatalf("Set failed: %v", err)
		}
		currentRV = preset[i].storedObj.ResourceVersion
	}

	// test continuations
	out := &example.PodList{}
	pred := func(limit int64, continueValue string) storage.SelectionPredicate {
		return storage.SelectionPredicate{
			Limit:    limit,
			Continue: continueValue,
			Label:    labels.Everything(),
			Field:    fields.Everything(),
			GetAttrs: func(obj runtime.Object) (labels.Set, fields.Set, error) {
				pod := obj.(*example.Pod)
				return nil, fields.Set{"metadata.name": pod.Name}, nil
			},
		}
	}
	options := storage.ListOptions{
		// Limit is ignored when ResourceVersion is set to 0.
		// Set it to consistent read.
		ResourceVersion: "",
		Predicate:       pred(1, ""),
		Recursive:       true,
	}
	if err := store.GetList(ctx, "/pods", options, out); err != nil {
		t.Fatalf("Unable to get initial list: %v", err)
	}
	if len(out.Continue) == 0 {
		t.Fatalf("No continuation token set")
	}
	expectNoDiff(t, "incorrect first page", []example.Pod{*preset[0].storedObj}, out.Items)
	if out.ResourceVersion != currentRV {
		t.Errorf("Expect output.ResourceVersion = %s, but got %s", currentRV, out.ResourceVersion)
	}
	if validation != nil {
		validation(t, 1, 1)
	}

	continueFromSecondItem := out.Continue

	// no limit, should get two items
	out = &example.PodList{}
	options = storage.ListOptions{
		// ResourceVersion should be unset when setting continuation token.
		ResourceVersion: "",
		Predicate:       pred(0, continueFromSecondItem),
		Recursive:       true,
	}
	if err := store.GetList(ctx, "/pods", options, out); err != nil {
		t.Fatalf("Unable to get second page: %v", err)
	}
	if len(out.Continue) != 0 {
		t.Fatalf("Unexpected continuation token set")
	}
	key, rv, err := storage.DecodeContinue(continueFromSecondItem, "/pods")
	t.Logf("continue token was %d %s %v", rv, key, err)
	expectNoDiff(t, "incorrect second page", []example.Pod{*preset[1].storedObj, *preset[2].storedObj}, out.Items)
	if out.ResourceVersion != currentRV {
		t.Errorf("Expect output.ResourceVersion = %s, but got %s", currentRV, out.ResourceVersion)
	}
	if validation != nil {
		validation(t, 0, 2)
	}

	// limit, should get two more pages
	out = &example.PodList{}
	options = storage.ListOptions{
		// ResourceVersion should be unset when setting continuation token.
		ResourceVersion: "",
		Predicate:       pred(1, continueFromSecondItem),
		Recursive:       true,
	}
	if err := store.GetList(ctx, "/pods", options, out); err != nil {
		t.Fatalf("Unable to get second page: %v", err)
	}
	if len(out.Continue) == 0 {
		t.Fatalf("No continuation token set")
	}
	expectNoDiff(t, "incorrect second page", []example.Pod{*preset[1].storedObj}, out.Items)
	if out.ResourceVersion != currentRV {
		t.Errorf("Expect output.ResourceVersion = %s, but got %s", currentRV, out.ResourceVersion)
	}
	if validation != nil {
		validation(t, 1, 1)
	}

	continueFromThirdItem := out.Continue

	out = &example.PodList{}
	options = storage.ListOptions{
		// ResourceVersion should be unset when setting continuation token.
		ResourceVersion: "",
		Predicate:       pred(1, continueFromThirdItem),
		Recursive:       true,
	}
	if err := store.GetList(ctx, "/pods", options, out); err != nil {
		t.Fatalf("Unable to get second page: %v", err)
	}
	if len(out.Continue) != 0 {
		t.Fatalf("Unexpected continuation token set")
	}
	expectNoDiff(t, "incorrect third page", []example.Pod{*preset[2].storedObj}, out.Items)
	if out.ResourceVersion != currentRV {
		t.Errorf("Expect output.ResourceVersion = %s, but got %s", currentRV, out.ResourceVersion)
	}
	if validation != nil {
		validation(t, 1, 1)
	}
}

func RunTestListPaginationRareObject(ctx context.Context, t *testing.T, store storage.Interface, validation CallsValidation) {
	podCount := 1000
	var pods []*example.Pod
	for i := 0; i < podCount; i++ {
		obj := &example.Pod{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("pod-%d", i)}}
		key := computePodKey(obj)
		storedObj := &example.Pod{}
		err := store.Create(ctx, key, obj, storedObj, 0)
		if err != nil {
			t.Fatalf("Set failed: %v", err)
		}
		pods = append(pods, storedObj)
	}

	out := &example.PodList{}
	options := storage.ListOptions{
		Predicate: storage.SelectionPredicate{
			Limit: 1,
			Label: labels.Everything(),
			Field: fields.OneTermEqualSelector("metadata.name", "pod-999"),
			GetAttrs: func(obj runtime.Object) (labels.Set, fields.Set, error) {
				pod := obj.(*example.Pod)
				return nil, fields.Set{"metadata.name": pod.Name}, nil
			},
		},
		Recursive: true,
	}
	if err := store.GetList(ctx, "/pods", options, out); err != nil {
		t.Fatalf("Unable to get initial list: %v", err)
	}
	if len(out.Continue) != 0 {
		t.Errorf("Unexpected continuation token set")
	}
	if len(out.Items) != 1 || !reflect.DeepEqual(&out.Items[0], pods[999]) {
		t.Fatalf("Unexpected first page: %#v", out.Items)
	}
	if validation != nil {
		validation(t, 1, uint64(podCount))
	}
}

func RunTestListContinuationWithFilter(ctx context.Context, t *testing.T, store storage.Interface, validation CallsValidation) {
	foo1 := &example.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "1", Name: "foo"}}
	bar2 := &example.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "2", Name: "bar"}} // this should not match
	foo3 := &example.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "3", Name: "foo"}}
	foo4 := &example.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "4", Name: "foo"}}
	preset := []struct {
		key       string
		obj       *example.Pod
		storedObj *example.Pod
	}{
		{
			key: computePodKey(foo1),
			obj: foo1,
		},
		{
			key: computePodKey(bar2),
			obj: bar2,
		},
		{
			key: computePodKey(foo3),
			obj: foo3,
		},
		{
			key: computePodKey(foo4),
			obj: foo4,
		},
	}

	var currentRV string
	for i, ps := range preset {
		preset[i].storedObj = &example.Pod{}
		err := store.Create(ctx, ps.key, ps.obj, preset[i].storedObj, 0)
		if err != nil {
			t.Fatalf("Set failed: %v", err)
		}
		currentRV = preset[i].storedObj.ResourceVersion
	}

	// the first list call should try to get 2 items from etcd (and only those items should be returned)
	// the field selector should result in it reading 3 items via the transformer
	// the chunking should result in 2 etcd Gets
	// there should be a continueValue because there is more data
	out := &example.PodList{}
	pred := func(limit int64, continueValue string) storage.SelectionPredicate {
		return storage.SelectionPredicate{
			Limit:    limit,
			Continue: continueValue,
			Label:    labels.Everything(),
			Field:    fields.OneTermNotEqualSelector("metadata.name", "bar"),
			GetAttrs: func(obj runtime.Object) (labels.Set, fields.Set, error) {
				pod := obj.(*example.Pod)
				return nil, fields.Set{"metadata.name": pod.Name}, nil
			},
		}
	}
	options := storage.ListOptions{
		// Limit is ignored when ResourceVersion is set to 0.
		// Set it to consistent read.
		ResourceVersion: "",
		Predicate:       pred(2, ""),
		Recursive:       true,
	}
	if err := store.GetList(ctx, "/pods", options, out); err != nil {
		t.Errorf("Unable to get initial list: %v", err)
	}
	if len(out.Continue) == 0 {
		t.Errorf("No continuation token set")
	}
	expectNoDiff(t, "incorrect first page", []example.Pod{*preset[0].storedObj, *preset[2].storedObj}, out.Items)
	if out.ResourceVersion != currentRV {
		t.Errorf("Expect output.ResourceVersion = %s, but got %s", currentRV, out.ResourceVersion)
	}
	if validation != nil {
		validation(t, 2, 3)
	}

	// the rest of the test does not make sense if the previous call failed
	if t.Failed() {
		return
	}

	cont := out.Continue

	// the second list call should try to get 2 more items from etcd
	// but since there is only one item left, that is all we should get with no continueValue
	// both read counters should be incremented for the singular calls they make in this case
	out = &example.PodList{}
	options = storage.ListOptions{
		// ResourceVersion should be unset when setting continuation token.
		ResourceVersion: "",
		Predicate:       pred(2, cont),
		Recursive:       true,
	}
	if err := store.GetList(ctx, "/pods", options, out); err != nil {
		t.Errorf("Unable to get second page: %v", err)
	}
	if len(out.Continue) != 0 {
		t.Errorf("Unexpected continuation token set")
	}
	expectNoDiff(t, "incorrect second page", []example.Pod{*preset[3].storedObj}, out.Items)
	if out.ResourceVersion != currentRV {
		t.Errorf("Expect output.ResourceVersion = %s, but got %s", currentRV, out.ResourceVersion)
	}
	if validation != nil {
		validation(t, 2, 1)
	}
}

type Compaction func(ctx context.Context, t *testing.T, resourceVersion string)

func RunTestListInconsistentContinuation(ctx context.Context, t *testing.T, store storage.Interface, compaction Compaction) {
	if compaction == nil {
		t.Skipf("compaction callback not provided")
	}

	// Setup storage with the following structure:
	//  /
	//   - first/
	//  |         - bar
	//  |
	//   - second/
	//  |         - bar
	//  |         - foo
	barFirst := &example.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "first", Name: "bar"}}
	barSecond := &example.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "second", Name: "bar"}}
	fooSecond := &example.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "second", Name: "foo"}}
	preset := []struct {
		key       string
		obj       *example.Pod
		storedObj *example.Pod
	}{
		{
			key: computePodKey(barFirst),
			obj: barFirst,
		},
		{
			key: computePodKey(barSecond),
			obj: barSecond,
		},
		{
			key: computePodKey(fooSecond),
			obj: fooSecond,
		},
	}
	for i, ps := range preset {
		preset[i].storedObj = &example.Pod{}
		err := store.Create(ctx, ps.key, ps.obj, preset[i].storedObj, 0)
		if err != nil {
			t.Fatalf("Set failed: %v", err)
		}
	}

	pred := func(limit int64, continueValue string) storage.SelectionPredicate {
		return storage.SelectionPredicate{
			Limit:    limit,
			Continue: continueValue,
			Label:    labels.Everything(),
			Field:    fields.Everything(),
			GetAttrs: func(obj runtime.Object) (labels.Set, fields.Set, error) {
				pod := obj.(*example.Pod)
				return nil, fields.Set{"metadata.name": pod.Name}, nil
			},
		}
	}

	out := &example.PodList{}
	options := storage.ListOptions{
		ResourceVersion: "0",
		Predicate:       pred(1, ""),
		Recursive:       true,
	}
	if err := store.GetList(ctx, "/pods", options, out); err != nil {
		t.Fatalf("Unable to get initial list: %v", err)
	}
	if len(out.Continue) == 0 {
		t.Fatalf("No continuation token set")
	}
	expectNoDiff(t, "incorrect first page", []example.Pod{*preset[0].storedObj}, out.Items)

	continueFromSecondItem := out.Continue

	// update /second/bar
	oldName := preset[2].obj.Name
	newPod := &example.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: oldName,
			Labels: map[string]string{
				"state": "new",
			},
		},
	}
	if err := store.GuaranteedUpdate(ctx, preset[2].key, preset[2].storedObj, false, nil,
		func(_ runtime.Object, _ storage.ResponseMeta) (runtime.Object, *uint64, error) {
			return newPod, nil, nil
		}, newPod); err != nil {
		t.Fatalf("update failed: %v", err)
	}

	// compact to latest revision.
	lastRVString := preset[2].storedObj.ResourceVersion
	compaction(ctx, t, lastRVString)

	// The old continue token should have expired
	options = storage.ListOptions{
		ResourceVersion: "0",
		Predicate:       pred(0, continueFromSecondItem),
		Recursive:       true,
	}
	err := store.GetList(ctx, "/pods", options, out)
	if err == nil {
		t.Fatalf("unexpected no error")
	}
	if !strings.Contains(err.Error(), "The provided continue parameter is too old ") {
		t.Fatalf("unexpected error message %v", err)
	}
	status, ok := err.(apierrors.APIStatus)
	if !ok {
		t.Fatalf("expect error of implements the APIStatus interface, got %v", reflect.TypeOf(err))
	}
	inconsistentContinueFromSecondItem := status.Status().ListMeta.Continue
	if len(inconsistentContinueFromSecondItem) == 0 {
		t.Fatalf("expect non-empty continue token")
	}

	out = &example.PodList{}
	options = storage.ListOptions{
		ResourceVersion: "0",
		Predicate:       pred(1, inconsistentContinueFromSecondItem),
		Recursive:       true,
	}
	if err := store.GetList(ctx, "/pods", options, out); err != nil {
		t.Fatalf("Unable to get second page: %v", err)
	}
	if len(out.Continue) == 0 {
		t.Fatalf("No continuation token set")
	}
	validateResourceVersion := resourceVersionNotOlderThan(lastRVString)
	expectNoDiff(t, "incorrect second page", []example.Pod{*preset[1].storedObj}, out.Items)
	if err := validateResourceVersion(out.ResourceVersion); err != nil {
		t.Fatal(err)
	}
	continueFromThirdItem := out.Continue
	resolvedResourceVersionFromThirdItem := out.ResourceVersion
	out = &example.PodList{}
	options = storage.ListOptions{
		ResourceVersion: "0",
		Predicate:       pred(1, continueFromThirdItem),
		Recursive:       true,
	}
	if err := store.GetList(ctx, "/pods", options, out); err != nil {
		t.Fatalf("Unable to get second page: %v", err)
	}
	if len(out.Continue) != 0 {
		t.Fatalf("Unexpected continuation token set")
	}
	expectNoDiff(t, "incorrect third page", []example.Pod{*preset[2].storedObj}, out.Items)
	if out.ResourceVersion != resolvedResourceVersionFromThirdItem {
		t.Fatalf("Expected list resource version to be %s, got %s", resolvedResourceVersionFromThirdItem, out.ResourceVersion)
	}
}

type PrefixTransformerModifier func(*PrefixTransformer) value.Transformer

type InterfaceWithPrefixTransformer interface {
	storage.Interface

	UpdatePrefixTransformer(PrefixTransformerModifier) func()
}

type InterfaceWithCorruptTransformer interface {
	storage.Interface
	CorruptTransformer() func()
}

func RunTestListResourceVersionMatch(ctx context.Context, t *testing.T, store InterfaceWithPrefixTransformer) {
	nextPod := func(index uint32) (string, *example.Pod) {
		obj := &example.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: fmt.Sprintf("pod-%d", index),
				Labels: map[string]string{
					"even": strconv.FormatBool(index%2 == 0),
				},
			},
		}
		return computePodKey(obj), obj
	}

	transformer := &reproducingTransformer{
		store:      store,
		nextObject: nextPod,
	}

	revertTransformer := store.UpdatePrefixTransformer(
		func(previousTransformer *PrefixTransformer) value.Transformer {
			transformer.wrapped = previousTransformer
			return transformer
		})
	defer revertTransformer()

	for i := 0; i < 5; i++ {
		if err := transformer.createObject(ctx); err != nil {
			t.Fatalf("failed to create object: %v", err)
		}
	}

	getAttrs := func(obj runtime.Object) (labels.Set, fields.Set, error) {
		pod, ok := obj.(*example.Pod)
		if !ok {
			return nil, nil, fmt.Errorf("invalid object")
		}
		return labels.Set(pod.Labels), nil, nil
	}
	predicate := storage.SelectionPredicate{
		Label:    labels.Set{"even": "true"}.AsSelector(),
		GetAttrs: getAttrs,
		Limit:    4,
	}

	result1 := example.PodList{}
	options := storage.ListOptions{
		Predicate: predicate,
		Recursive: true,
	}
	if err := store.GetList(ctx, "/pods", options, &result1); err != nil {
		t.Fatalf("failed to list objects: %v", err)
	}

	// List objects from the returned resource version.
	options = storage.ListOptions{
		Predicate:            predicate,
		ResourceVersion:      result1.ResourceVersion,
		ResourceVersionMatch: metav1.ResourceVersionMatchExact,
		Recursive:            true,
	}

	result2 := example.PodList{}
	if err := store.GetList(ctx, "/pods", options, &result2); err != nil {
		t.Fatalf("failed to list objects: %v", err)
	}

	expectNoDiff(t, "incorrect lists", result1, result2)

	// Now also verify the  ResourceVersionMatchNotOlderThan.
	options.ResourceVersionMatch = metav1.ResourceVersionMatchNotOlderThan

	result3 := example.PodList{}
	if err := store.GetList(ctx, "/pods", options, &result3); err != nil {
		t.Fatalf("failed to list objects: %v", err)
	}

	options.ResourceVersion = result3.ResourceVersion
	options.ResourceVersionMatch = metav1.ResourceVersionMatchExact

	result4 := example.PodList{}
	if err := store.GetList(ctx, "/pods", options, &result4); err != nil {
		t.Fatalf("failed to list objects: %v", err)
	}

	expectNoDiff(t, "incorrect lists", result3, result4)
}

func RunTestGuaranteedUpdate(ctx context.Context, t *testing.T, store InterfaceWithPrefixTransformer, validation KeyValidation) {
	inputObj := &example.Pod{ObjectMeta: metav1.ObjectMeta{Name: "foo", Namespace: "test-ns", UID: "A"}}
	key := computePodKey(inputObj)

	tests := []struct {
		name                string
		key                 string
		ignoreNotFound      bool
		precondition        *storage.Preconditions
		expectNotFoundErr   bool
		expectInvalidObjErr bool
		expectNoUpdate      bool
		transformStale      bool
		hasSelfLink         bool
	}{{
		name:                "non-existing key, ignoreNotFound=false",
		key:                 "/non-existing",
		ignoreNotFound:      false,
		precondition:        nil,
		expectNotFoundErr:   true,
		expectInvalidObjErr: false,
		expectNoUpdate:      false,
	}, {
		name:                "non-existing key, ignoreNotFound=true",
		key:                 "/non-existing",
		ignoreNotFound:      true,
		precondition:        nil,
		expectNotFoundErr:   false,
		expectInvalidObjErr: false,
		expectNoUpdate:      false,
	}, {
		name:                "existing key",
		key:                 key,
		ignoreNotFound:      false,
		precondition:        nil,
		expectNotFoundErr:   false,
		expectInvalidObjErr: false,
		expectNoUpdate:      false,
	}, {
		name:                "same data",
		key:                 key,
		ignoreNotFound:      false,
		precondition:        nil,
		expectNotFoundErr:   false,
		expectInvalidObjErr: false,
		expectNoUpdate:      true,
	}, {
		name:                "same data, a selfLink",
		key:                 key,
		ignoreNotFound:      false,
		precondition:        nil,
		expectNotFoundErr:   false,
		expectInvalidObjErr: false,
		expectNoUpdate:      true,
		hasSelfLink:         true,
	}, {
		name:                "same data, stale",
		key:                 key,
		ignoreNotFound:      false,
		precondition:        nil,
		expectNotFoundErr:   false,
		expectInvalidObjErr: false,
		expectNoUpdate:      false,
		transformStale:      true,
	}, {
		name:                "UID match",
		key:                 key,
		ignoreNotFound:      false,
		precondition:        storage.NewUIDPreconditions("A"),
		expectNotFoundErr:   false,
		expectInvalidObjErr: false,
		expectNoUpdate:      true,
	}, {
		name:                "UID mismatch",
		key:                 key,
		ignoreNotFound:      false,
		precondition:        storage.NewUIDPreconditions("B"),
		expectNotFoundErr:   false,
		expectInvalidObjErr: true,
		expectNoUpdate:      true,
	}}

	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key, storeObj := testPropagateStore(ctx, t, store, inputObj)

			out := &example.Pod{}
			annotations := map[string]string{"version": fmt.Sprintf("%d", i)}
			if tt.expectNoUpdate {
				annotations = nil
			}

			if tt.transformStale {
				revertTransformer := store.UpdatePrefixTransformer(
					func(transformer *PrefixTransformer) value.Transformer {
						transformer.stale = true
						return transformer
					})
				defer revertTransformer()
			}

			version := storeObj.ResourceVersion
			err := store.GuaranteedUpdate(ctx, tt.key, out, tt.ignoreNotFound, tt.precondition,
				storage.SimpleUpdate(func(obj runtime.Object) (runtime.Object, error) {
					if tt.expectNotFoundErr && tt.ignoreNotFound {
						if pod := obj.(*example.Pod); pod.Name != "" {
							t.Errorf("%s: expecting zero value, but get=%#v", tt.name, pod)
						}
					}
					pod := *storeObj
					if tt.hasSelfLink {
						pod.SelfLink = "testlink"
					}
					pod.Annotations = annotations
					return &pod, nil
				}), nil)

			if tt.expectNotFoundErr {
				if err == nil || !storage.IsNotFound(err) {
					t.Errorf("%s: expecting not found error, but get: %v", tt.name, err)
				}
				return
			}
			if tt.expectInvalidObjErr {
				if err == nil || !storage.IsInvalidObj(err) {
					t.Errorf("%s: expecting invalid UID error, but get: %s", tt.name, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("%s: GuaranteedUpdate failed: %v", tt.name, err)
			}
			if !reflect.DeepEqual(out.ObjectMeta.Annotations, annotations) {
				t.Errorf("%s: pod annotations want=%s, get=%s", tt.name, annotations, out.ObjectMeta.Annotations)
			}
			if out.SelfLink != "" {
				t.Errorf("%s: selfLink should not be set", tt.name)
			}

			// verify that kv pair is not empty after set and that the underlying data matches expectations
			validation(ctx, t, key)

			switch tt.expectNoUpdate {
			case true:
				if version != out.ResourceVersion {
					t.Errorf("%s: expect no version change, before=%s, after=%s", tt.name, version, out.ResourceVersion)
				}
			case false:
				if version == out.ResourceVersion {
					t.Errorf("%s: expect version change, but get the same version=%s", tt.name, version)
				}
			}
		})
	}
}

func RunTestGuaranteedUpdateWithTTL(ctx context.Context, t *testing.T, store storage.Interface) {
	input := &example.Pod{ObjectMeta: metav1.ObjectMeta{Name: "foo", Namespace: "test-ns"}}
	key := computePodKey(input)

	out := &example.Pod{}
	err := store.GuaranteedUpdate(ctx, key, out, true, nil,
		func(_ runtime.Object, _ storage.ResponseMeta) (runtime.Object, *uint64, error) {
			ttl := uint64(1)
			return input, &ttl, nil
		}, nil)
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	w, err := store.Watch(ctx, key, storage.ListOptions{ResourceVersion: out.ResourceVersion, Predicate: storage.Everything})
	if err != nil {
		t.Fatalf("Watch failed: %v", err)
	}
	testCheckEventType(t, w, watch.Deleted)
}

func RunTestGuaranteedUpdateChecksStoredData(ctx context.Context, t *testing.T, store InterfaceWithPrefixTransformer) {
	input := &example.Pod{ObjectMeta: metav1.ObjectMeta{Name: "foo", Namespace: "test-ns"}}
	key := computePodKey(input)

	// serialize input into etcd with data that would be normalized by a write -
	// in this case, leading whitespace
	revertTransformer := store.UpdatePrefixTransformer(
		func(transformer *PrefixTransformer) value.Transformer {
			transformer.prefix = []byte(string(transformer.prefix) + " ")
			return transformer
		})
	_, initial := testPropagateStore(ctx, t, store, input)
	revertTransformer()

	// this update should write the canonical value to etcd because the new serialization differs
	// from the stored serialization
	input.ResourceVersion = initial.ResourceVersion
	out := &example.Pod{}
	err := store.GuaranteedUpdate(ctx, key, out, true, nil,
		func(_ runtime.Object, _ storage.ResponseMeta) (runtime.Object, *uint64, error) {
			return input, nil, nil
		}, input)
	if err != nil {
		t.Fatalf("Update failed: %v", err)
	}
	if out.ResourceVersion == initial.ResourceVersion {
		t.Errorf("guaranteed update should have updated the serialized data, got %#v", out)
	}

	lastVersion := out.ResourceVersion

	// this update should not write to etcd because the input matches the stored data
	input = out
	out = &example.Pod{}
	err = store.GuaranteedUpdate(ctx, key, out, true, nil,
		func(_ runtime.Object, _ storage.ResponseMeta) (runtime.Object, *uint64, error) {
			return input, nil, nil
		}, input)
	if err != nil {
		t.Fatalf("Update failed: %v", err)
	}
	if out.ResourceVersion != lastVersion {
		t.Errorf("guaranteed update should have short-circuited write, got %#v", out)
	}

	revertTransformer = store.UpdatePrefixTransformer(
		func(transformer *PrefixTransformer) value.Transformer {
			transformer.stale = true
			return transformer
		})
	defer revertTransformer()

	// this update should write to etcd because the transformer reported stale
	err = store.GuaranteedUpdate(ctx, key, out, true, nil,
		func(_ runtime.Object, _ storage.ResponseMeta) (runtime.Object, *uint64, error) {
			return input, nil, nil
		}, input)
	if err != nil {
		t.Fatalf("Update failed: %v", err)
	}
	if out.ResourceVersion == lastVersion {
		t.Errorf("guaranteed update should have written to etcd when transformer reported stale, got %#v", out)
	}
}

func RunTestGuaranteedUpdateWithConflict(ctx context.Context, t *testing.T, store storage.Interface) {
	key, _ := testPropagateStore(ctx, t, store, &example.Pod{ObjectMeta: metav1.ObjectMeta{Name: "foo", Namespace: "test-ns"}})

	errChan := make(chan error, 1)
	var firstToFinish sync.WaitGroup
	var secondToEnter sync.WaitGroup
	firstToFinish.Add(1)
	secondToEnter.Add(1)

	go func() {
		err := store.GuaranteedUpdate(ctx, key, &example.Pod{}, false, nil,
			storage.SimpleUpdate(func(obj runtime.Object) (runtime.Object, error) {
				pod := obj.(*example.Pod)
				pod.Name = "foo-1"
				secondToEnter.Wait()
				return pod, nil
			}), nil)
		firstToFinish.Done()
		errChan <- err
	}()

	updateCount := 0
	err := store.GuaranteedUpdate(ctx, key, &example.Pod{}, false, nil,
		storage.SimpleUpdate(func(obj runtime.Object) (runtime.Object, error) {
			if updateCount == 0 {
				secondToEnter.Done()
				firstToFinish.Wait()
			}
			updateCount++
			pod := obj.(*example.Pod)
			pod.Name = "foo-2"
			return pod, nil
		}), nil)
	if err != nil {
		t.Fatalf("Second GuaranteedUpdate error %#v", err)
	}
	if err := <-errChan; err != nil {
		t.Fatalf("First GuaranteedUpdate error %#v", err)
	}

	if updateCount != 2 {
		t.Errorf("Should have conflict and called update func twice")
	}
}

func RunTestGuaranteedUpdateWithSuggestionAndConflict(ctx context.Context, t *testing.T, store storage.Interface) {
	key, originalPod := testPropagateStore(ctx, t, store, &example.Pod{ObjectMeta: metav1.ObjectMeta{Name: "foo", Namespace: "test-ns"}})

	// First, update without a suggestion so originalPod is outdated
	updatedPod := &example.Pod{}
	err := store.GuaranteedUpdate(ctx, key, updatedPod, false, nil,
		storage.SimpleUpdate(func(obj runtime.Object) (runtime.Object, error) {
			pod := obj.(*example.Pod)
			pod.Generation = 2
			return pod, nil
		}),
		nil,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Second, update using the outdated originalPod as the suggestion. Return a conflict error when
	// passed originalPod, and make sure that SimpleUpdate is called a second time after a live lookup
	// with the value of updatedPod.
	sawConflict := false
	updatedPod2 := &example.Pod{}
	err = store.GuaranteedUpdate(ctx, key, updatedPod2, false, nil,
		storage.SimpleUpdate(func(obj runtime.Object) (runtime.Object, error) {
			pod := obj.(*example.Pod)
			if pod.Generation != 2 {
				if sawConflict {
					t.Fatalf("unexpected second conflict")
				}
				sawConflict = true
				// simulated stale object - return a conflict
				return nil, apierrors.NewConflict(example.SchemeGroupVersion.WithResource("pods").GroupResource(), "name", errors.New("foo"))
			}
			pod.Generation = 3
			return pod, nil
		}),
		originalPod,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if updatedPod2.Generation != 3 {
		t.Errorf("unexpected pod generation: %q", updatedPod2.Generation)
	}

	// Third, update using a current version as the suggestion.
	// Return an error and make sure that SimpleUpdate is NOT called a second time,
	// since the live lookup shows the suggestion was already up to date.
	attempts := 0
	updatedPod3 := &example.Pod{}
	err = store.GuaranteedUpdate(ctx, key, updatedPod3, false, nil,
		storage.SimpleUpdate(func(obj runtime.Object) (runtime.Object, error) {
			pod := obj.(*example.Pod)
			if pod.Generation != updatedPod2.Generation || pod.ResourceVersion != updatedPod2.ResourceVersion {
				t.Logf("stale object (rv=%s), expected rv=%s", pod.ResourceVersion, updatedPod2.ResourceVersion)
			}
			attempts++
			return nil, fmt.Errorf("validation or admission error")
		}),
		updatedPod2,
	)
	if err == nil {
		t.Fatalf("expected error, got none")
	}
	// Implementations of the storage interface are allowed to ignore the suggestion,
	// in which case two attempts are possible.
	if attempts > 2 {
		t.Errorf("update function should have been called at most twice, called %d", attempts)
	}
}

func RunTestTransformationFailure(ctx context.Context, t *testing.T, store InterfaceWithPrefixTransformer) {
	barFirst := &example.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "first", Name: "bar"},
		Spec:       DeepEqualSafePodSpec(),
	}
	bazSecond := &example.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "second", Name: "baz"},
		Spec:       DeepEqualSafePodSpec(),
	}

	preset := []struct {
		key       string
		obj       *example.Pod
		storedObj *example.Pod
	}{{
		key: computePodKey(barFirst),
		obj: barFirst,
	}, {
		key: computePodKey(bazSecond),
		obj: bazSecond,
	}}
	for i, ps := range preset[:1] {
		preset[i].storedObj = &example.Pod{}
		err := store.Create(ctx, ps.key, ps.obj, preset[:1][i].storedObj, 0)
		if err != nil {
			t.Fatalf("Set failed: %v", err)
		}
	}

	// create a second resource with an invalid prefix
	revertTransformer := store.UpdatePrefixTransformer(
		func(transformer *PrefixTransformer) value.Transformer {
			return NewPrefixTransformer([]byte("otherprefix!"), false)
		})
	for i, ps := range preset[1:] {
		preset[1:][i].storedObj = &example.Pod{}
		err := store.Create(ctx, ps.key, ps.obj, preset[1:][i].storedObj, 0)
		if err != nil {
			t.Fatalf("Set failed: %v", err)
		}
	}
	revertTransformer()

	// List should fail
	var got example.PodList
	storageOpts := storage.ListOptions{
		Predicate: storage.Everything,
		Recursive: true,
	}
	if err := store.GetList(ctx, "/pods", storageOpts, &got); !storage.IsInternalError(err) {
		t.Errorf("Unexpected error %v", err)
	}

	// Get should fail
	if err := store.Get(ctx, preset[1].key, storage.GetOptions{}, &example.Pod{}); !storage.IsInternalError(err) {
		t.Errorf("Unexpected error: %v", err)
	}

	updateFunc := func(input runtime.Object, res storage.ResponseMeta) (runtime.Object, *uint64, error) {
		return input, nil, nil
	}
	// GuaranteedUpdate without suggestion should return an error
	if err := store.GuaranteedUpdate(ctx, preset[1].key, &example.Pod{}, false, nil, updateFunc, nil); !storage.IsInternalError(err) {
		t.Errorf("Unexpected error: %v", err)
	}
	// GuaranteedUpdate with suggestion should return an error if we don't change the object
	if err := store.GuaranteedUpdate(ctx, preset[1].key, &example.Pod{}, false, nil, updateFunc, preset[1].obj); err == nil {
		t.Errorf("Unexpected error: %v", err)
	}

	// Delete fails with internal error.
	if err := store.Delete(ctx, preset[1].key, &example.Pod{}, nil, storage.ValidateAllObjectFunc, nil, storage.DeleteOptions{}); !storage.IsInternalError(err) {
		t.Errorf("Unexpected error: %v", err)
	}
	if err := store.Get(ctx, preset[1].key, storage.GetOptions{}, &example.Pod{}); !storage.IsInternalError(err) {
		t.Errorf("Unexpected error: %v", err)
	}
}

func RunTestCount(ctx context.Context, t *testing.T, store storage.Interface) {
	resourceA := "/foo.bar.io/abc"

	// resourceA is intentionally a prefix of resourceB to ensure that the count
	// for resourceA does not include any objects from resourceB.
	resourceB := fmt.Sprintf("%sdef", resourceA)

	resourceACountExpected := 5
	for i := 1; i <= resourceACountExpected; i++ {
		obj := &example.Pod{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("foo-%d", i)}}

		key := fmt.Sprintf("%s/%d", resourceA, i)
		if err := store.Create(ctx, key, obj, nil, 0); err != nil {
			t.Fatalf("Create failed: %v", err)
		}
	}

	resourceBCount := 4
	for i := 1; i <= resourceBCount; i++ {
		obj := &example.Pod{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("foo-%d", i)}}

		key := fmt.Sprintf("%s/%d", resourceB, i)
		if err := store.Create(ctx, key, obj, nil, 0); err != nil {
			t.Fatalf("Create failed: %v", err)
		}
	}

	resourceACountGot, err := store.Count(resourceA)
	if err != nil {
		t.Fatalf("store.Count failed: %v", err)
	}

	// count for resourceA should not include the objects for resourceB
	// even though resourceA is a prefix of resourceB.
	if int64(resourceACountExpected) != resourceACountGot {
		t.Fatalf("store.Count for resource %s: expected %d but got %d", resourceA, resourceACountExpected, resourceACountGot)
	}
}

func RunTestListPaging(ctx context.Context, t *testing.T, store storage.Interface) {
	out := &example.Pod{}
	for i := 0; i < 4; i++ {
		name := fmt.Sprintf("test-%d", i)
		pod := &example.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "paging"}}
		key := computePodKey(pod)
		if err := store.Create(ctx, key, pod, out, 0); err != nil {
			t.Fatal(err)
		}
	}

	var names []string
	opts := storage.ListOptions{
		Recursive: true,
		Predicate: storage.SelectionPredicate{
			Label: labels.Everything(),
			Field: fields.Everything(),
			Limit: 1,
		},
	}
	calls := 0
	for {
		calls++
		listOut := &example.PodList{}
		err := store.GetList(ctx, "/pods", opts, listOut)
		if err != nil {
			t.Fatalf("Unexpected error %s", err)
		}
		for _, item := range listOut.Items {
			names = append(names, item.Name)
		}
		if listOut.Continue == "" {
			break
		}
		if calls == 2 {
			name := "test-5"
			pod := &example.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "paging"}}
			key := computePodKey(pod)
			if err := store.Create(ctx, key, pod, out, 0); err != nil {
				t.Fatal(err)
			}
		}
		opts.Predicate.Continue = listOut.Continue
	}
	if calls != 4 {
		t.Errorf("unexpected list invocations: %d", calls)
	}
	if !reflect.DeepEqual(names, []string{"test-0", "test-1", "test-2", "test-3"}) {
		t.Errorf("unexpected items: %#v", names)
	}
}

func RunTestNamespaceScopedList(ctx context.Context, t *testing.T, store storage.Interface) {
	tests := []struct {
		name               string
		requestedNamespace string
		recursive          bool
		indexFields        []string
		fieldSelector      func(namespace string) fields.Selector
		inputPods          func(namespace string) []example.Pod
		expectPods         func(namespace string) []example.Pod
	}{
		{
			name:          "request without namespace, without field selector",
			recursive:     true,
			fieldSelector: func(namespace string) fields.Selector { return fields.Everything() },
			inputPods: func(namespace string) []example.Pod {
				return []example.Pod{
					*baseNamespacedPod("foo1", "ns1"),
					*baseNamespacedPod("foo2", "ns2"),
				}
			},
			expectPods: func(namespace string) []example.Pod {
				return []example.Pod{
					*baseNamespacedPod("foo1", "ns1"),
					*baseNamespacedPod("foo2", "ns2"),
				}
			},
		},
		{
			name:      "request without namespace, field selector with metadata.namespace",
			recursive: true,
			fieldSelector: func(namespace string) fields.Selector {
				return fields.SelectorFromSet(fields.Set{"metadata.namespace": namespace + "ns1"})
			},
			inputPods: func(namespace string) []example.Pod {
				return []example.Pod{
					*baseNamespacedPod("foo1", namespace+"ns1"),
					*baseNamespacedPod("foo1", namespace+"ns2"),
				}
			},
			expectPods: func(namespace string) []example.Pod {
				return []example.Pod{
					*baseNamespacedPod("foo1", namespace+"ns1"),
				}
			},
		},
		{
			name:      "request without namespace, field selector with spec.nodename",
			recursive: true,
			fieldSelector: func(namespace string) fields.Selector {
				return fields.ParseSelectorOrDie("spec.nodeName=bar1")
			},
			indexFields: []string{"spec.nodeName"},
			inputPods: func(namespace string) []example.Pod {
				return []example.Pod{
					*baseNamespacedPodAssigned("foo1", namespace+"ns1", "bar1"),
					*baseNamespacedPodAssigned("foo2", namespace+"ns2", "bar2"),
				}
			},
			expectPods: func(namespace string) []example.Pod {
				return []example.Pod{
					*baseNamespacedPodAssigned("foo1", namespace+"ns1", "bar1"),
				}
			},
		},
		{
			name:          "request without namespace, field selector with spec.nodename to filter out",
			recursive:     true,
			fieldSelector: func(namespace string) fields.Selector { return fields.ParseSelectorOrDie("spec.nodeName!=bar1") },
			indexFields:   []string{"spec.nodeName"},
			inputPods: func(namespace string) []example.Pod {
				return []example.Pod{
					*baseNamespacedPodAssigned("foo1", namespace+"ns1", "bar1"),
					*baseNamespacedPodAssigned("foo2", namespace+"ns2", "bar2"),
				}
			},
			expectPods: func(namespace string) []example.Pod {
				return []example.Pod{
					*baseNamespacedPodAssigned("foo2", namespace+"ns2", "bar2"),
				}
			},
		},
		{
			name:               "request with namespace, without field selector",
			requestedNamespace: "ns1",
			recursive:          true,
			fieldSelector:      func(namespace string) fields.Selector { return fields.Everything() },
			inputPods: func(namespace string) []example.Pod {
				return []example.Pod{
					*baseNamespacedPod("foo1", namespace+"ns1"),
					*baseNamespacedPod("foo1", namespace+"ns2"),
					*baseNamespacedPod("foo2", namespace+"ns1"),
				}
			},
			expectPods: func(namespace string) []example.Pod {
				return []example.Pod{
					*baseNamespacedPod("foo1", namespace+"ns1"),
					*baseNamespacedPod("foo2", namespace+"ns1"),
				}
			},
		},
		{
			name:               "request with namespace, field selector with matched metadata.namespace",
			requestedNamespace: "ns1",
			recursive:          true,
			fieldSelector: func(namespace string) fields.Selector {
				return fields.SelectorFromSet(fields.Set{"metadata.namespace": namespace + "ns1"})
			},
			inputPods: func(namespace string) []example.Pod {
				return []example.Pod{
					*baseNamespacedPod("foo1", namespace+"ns1"),
					*baseNamespacedPod("foo1", namespace+"ns2"),
				}
			},
			expectPods: func(namespace string) []example.Pod {
				return []example.Pod{
					*baseNamespacedPod("foo1", namespace+"ns1"),
				}
			},
		},
		{
			name:               "request with namespace, field selector with non-matched metadata.namespace",
			requestedNamespace: "ns1",
			recursive:          true,
			fieldSelector: func(namespace string) fields.Selector {
				return fields.SelectorFromSet(fields.Set{"metadata.namespace": namespace + "ns2"})
			},
			inputPods: func(namespace string) []example.Pod {
				return []example.Pod{
					*baseNamespacedPod("foo1", namespace+"ns1"),
					*baseNamespacedPod("foo1", namespace+"ns2"),
					*baseNamespacedPodUpdated("foo2", namespace+"ns1"),
					*baseNamespacedPodUpdated("foo2", namespace+"ns2"),
				}
			},
			expectPods: func(namespace string) []example.Pod { return []example.Pod{} },
		},
		{
			name:               "request with namespace, field selector with spec.nodename",
			requestedNamespace: "ns1",
			recursive:          true,
			fieldSelector:      func(namespace string) fields.Selector { return fields.ParseSelectorOrDie("spec.nodeName=bar2") },
			indexFields:        []string{"spec.nodeName"},
			inputPods: func(namespace string) []example.Pod {
				return []example.Pod{
					*baseNamespacedPodAssigned("foo1", namespace+"ns1", "bar1"),
					*baseNamespacedPodAssigned("foo1", namespace+"ns2", "bar2"),
					*baseNamespacedPodAssigned("foo2", namespace+"ns1", "bar2"),
				}
			},
			expectPods: func(namespace string) []example.Pod {
				return []example.Pod{
					*baseNamespacedPodAssigned("foo2", namespace+"ns1", "bar2"),
				}
			},
		},
		{
			name:               "request with namespace, field selector with spec.nodename to filter out",
			requestedNamespace: "ns2",
			recursive:          true,
			fieldSelector:      func(namespace string) fields.Selector { return fields.ParseSelectorOrDie("spec.nodeName!=bar1") },
			indexFields:        []string{"spec.nodeName"},
			inputPods: func(namespace string) []example.Pod {
				return []example.Pod{
					*baseNamespacedPod("foo1", namespace+"ns1"),
					*baseNamespacedPod("foo1", namespace+"ns2"),
					*baseNamespacedPodAssigned("foo3", namespace+"ns2", "bar1"),
					*baseNamespacedPodAssigned("foo2", namespace+"ns2", "bar2"),
				}
			},
			expectPods: func(namespace string) []example.Pod {
				return []example.Pod{
					*baseNamespacedPod("foo1", namespace+"ns2"),
					*baseNamespacedPodAssigned("foo2", namespace+"ns2", "bar2"),
				}
			},
		},
		{
			name:          "request without namespace, field selector with metadata.name",
			recursive:     true,
			fieldSelector: func(namespace string) fields.Selector { return fields.ParseSelectorOrDie("metadata.name=foo1") },
			inputPods: func(namespace string) []example.Pod {
				return []example.Pod{
					*baseNamespacedPod("foo1", namespace+"ns1"),
					*baseNamespacedPod("foo1", namespace+"ns2"),
					*baseNamespacedPod("foo2", namespace+"ns1"),
				}
			},
			expectPods: func(namespace string) []example.Pod {
				return []example.Pod{
					*baseNamespacedPod("foo1", namespace+"ns1"),
					*baseNamespacedPod("foo1", namespace+"ns2"),
				}
			},
		},
		{
			name:      "request without namespace, field selector with metadata.name and metadata.namespace",
			recursive: true,
			fieldSelector: func(namespace string) fields.Selector {
				return fields.SelectorFromSet(fields.Set{
					"metadata.name":      "foo1",
					"metadata.namespace": namespace + "ns1",
				})
			},
			inputPods: func(namespace string) []example.Pod {
				return []example.Pod{
					*baseNamespacedPod("foo1", namespace+"ns1"),
					*baseNamespacedPod("foo2", namespace+"ns1"),
					*baseNamespacedPod("foo1", namespace+"ns2"),
				}
			},
			expectPods: func(namespace string) []example.Pod {
				return []example.Pod{
					*baseNamespacedPod("foo1", namespace+"ns1"),
				}
			},
		},
		{
			name:      "request without namespace, field selector with metadata.name and spec.nodeName",
			recursive: true,
			fieldSelector: func(namespace string) fields.Selector {
				return fields.SelectorFromSet(fields.Set{
					"metadata.name": "foo1",
					"spec.nodeName": "bar1",
				})
			},
			indexFields: []string{"spec.nodeName"},
			inputPods: func(namespace string) []example.Pod {
				return []example.Pod{
					*baseNamespacedPodAssigned("foo1", namespace+"ns1", "bar1"),
				}
			},
			expectPods: func(namespace string) []example.Pod {
				return []example.Pod{
					*baseNamespacedPodAssigned("foo1", namespace+"ns1", "bar1"),
				}
			},
		},
		{
			name:      "request without namespace, field selector with metadata.name, and with spec.nodeName to filter out watch",
			recursive: true,
			fieldSelector: func(namespace string) fields.Selector {
				return fields.AndSelectors(
					fields.ParseSelectorOrDie("spec.nodeName!=bar1"),
					fields.SelectorFromSet(fields.Set{"metadata.name": "foo1"}),
				)
			},
			indexFields: []string{"spec.nodeName"},
			inputPods: func(namespace string) []example.Pod {
				return []example.Pod{
					*baseNamespacedPod("foo1", namespace+"ns1"),
					*baseNamespacedPod("foo2", namespace+"ns1"),
					*baseNamespacedPodAssigned("foo1", namespace+"ns2", "bar1"),
					*baseNamespacedPodAssigned("foo2", namespace+"ns2", "bar1"),
					*baseNamespacedPodAssigned("foo1", namespace+"ns3", "bar2"),
					*baseNamespacedPodAssigned("foo2", namespace+"ns3", "bar2"),
				}
			},
			expectPods: func(namespace string) []example.Pod {
				return []example.Pod{
					*baseNamespacedPod("foo1", namespace+"ns1"),
					*baseNamespacedPodAssigned("foo1", namespace+"ns3", "bar2"),
				}
			},
		},
		{
			name:               "request with namespace, with field selector metadata.name",
			requestedNamespace: "ns1",
			fieldSelector:      func(namespace string) fields.Selector { return fields.ParseSelectorOrDie("metadata.name=foo1") },
			inputPods: func(namespace string) []example.Pod {
				return []example.Pod{
					*baseNamespacedPod("foo1", namespace+"ns1"),
					*baseNamespacedPod("foo2", namespace+"ns1"),
					*baseNamespacedPod("foo1", namespace+"ns2"),
					*baseNamespacedPod("foo2", namespace+"ns2"),
				}
			},
			expectPods: func(namespace string) []example.Pod {
				return []example.Pod{
					*baseNamespacedPod("foo1", namespace+"ns1"),
				}
			},
		},
		{
			name:               "request with namespace, with field selector metadata.name and metadata.namespace",
			requestedNamespace: "ns1",
			fieldSelector: func(namespace string) fields.Selector {
				return fields.SelectorFromSet(fields.Set{
					"metadata.name":      "foo1",
					"metadata.namespace": namespace + "ns1",
				})
			},
			inputPods: func(namespace string) []example.Pod {
				return []example.Pod{
					*baseNamespacedPod("foo1", namespace+"ns1"),
					*baseNamespacedPod("foo2", namespace+"ns1"),
					*baseNamespacedPod("foo1", namespace+"ns2"),
					*baseNamespacedPod("foo2", namespace+"ns2"),
				}
			},
			expectPods: func(namespace string) []example.Pod {
				return []example.Pod{
					*baseNamespacedPod("foo1", namespace+"ns1"),
				}
			},
		},
		{
			name:               "request with namespace, with field selector metadata.name, metadata.namespace and spec.nodename",
			requestedNamespace: "ns2",
			fieldSelector: func(namespace string) fields.Selector {
				return fields.SelectorFromSet(fields.Set{
					"metadata.name":      "foo2",
					"metadata.namespace": namespace + "ns2",
					"spec.nodeName":      "bar1",
				})
			},
			indexFields: []string{"spec.nodeName"},
			inputPods: func(namespace string) []example.Pod {
				return []example.Pod{
					*baseNamespacedPod("foo1", namespace+"ns1"),
					*baseNamespacedPod("foo2", namespace+"ns1"),
					*baseNamespacedPod("foo1", namespace+"ns2"),
					*baseNamespacedPodAssigned("foo2", namespace+"ns2", "bar1"),
					*baseNamespacedPodAssigned("foo1", namespace+"ns3", "bar1"),
					*baseNamespacedPodAssigned("foo2", namespace+"ns3", "bar1"),
				}
			},
			expectPods: func(namespace string) []example.Pod {
				return []example.Pod{
					*baseNamespacedPodAssigned("foo2", namespace+"ns2", "bar1"),
				}
			},
		},
		{
			name:               "request with namespace, with field selector metadata.name, metadata.namespace, and with spec.nodename to filter out",
			requestedNamespace: "ns2",
			fieldSelector: func(namespace string) fields.Selector {
				return fields.AndSelectors(
					fields.ParseSelectorOrDie("spec.nodeName!=bar2"),
					fields.SelectorFromSet(fields.Set{"metadata.name": "foo1", "metadata.namespace": namespace + "ns2"}),
				)
			},
			indexFields: []string{"spec.nodeName"},
			inputPods: func(namespace string) []example.Pod {
				return []example.Pod{
					*baseNamespacedPod("foo1", namespace+"ns1"),
					*baseNamespacedPod("foo2", namespace+"ns1"),
					*baseNamespacedPodAssigned("foo1", namespace+"ns2", "bar1"),
					*baseNamespacedPodAssigned("foo2", namespace+"ns2", "bar2"),
					*baseNamespacedPodAssigned("foo1", namespace+"ns3", "bar2"),
				}
			},
			expectPods: func(namespace string) []example.Pod {
				return []example.Pod{
					*baseNamespacedPodAssigned("foo1", namespace+"ns2", "bar1"),
				}
			},
		},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			podNames := map[string]struct{}{}
			namespace := fmt.Sprintf("t%d-", i)
			for _, pod := range tt.inputPods(namespace) {
				out := &example.Pod{}
				key := computePodKey(&pod)
				podNames[key] = struct{}{}
				err := store.Create(ctx, key, &pod, out, 0)
				if err != nil {
					t.Fatalf("GuaranteedUpdate failed: %v", err)
				}
			}
			opts := storage.ListOptions{
				ResourceVersion: "",
				Predicate:       CreatePodPredicate(tt.fieldSelector(namespace), true, tt.indexFields),
				Recursive:       true,
			}
			listOut := &example.PodList{}
			path := "/pods/"
			if tt.requestedNamespace != "" {
				path += namespace + tt.requestedNamespace
			}
			if err := store.GetList(ctx, path, opts, listOut); err != nil {
				t.Errorf("Unexpected error: %v", err)
			}
			i := 0
			for _, pod := range listOut.Items {
				if _, found := podNames[computePodKey(&pod)]; !found {
					continue
				}
				pod.ResourceVersion = ""
				listOut.Items[i] = pod
				i++
			}
			listOut.Items = listOut.Items[:i]

			expectNoDiff(t, "incorrect list pods", tt.expectPods(namespace), listOut.Items)
		})
	}
}
