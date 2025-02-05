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
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/apiserver/pkg/apis/example"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/features"
	"k8s.io/apiserver/pkg/storage"
	"k8s.io/apiserver/pkg/storage/value"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	utilflowcontrol "k8s.io/apiserver/pkg/util/flowcontrol"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	"k8s.io/utils/pointer"
)

func RunTestWatch(ctx context.Context, t *testing.T, store storage.Interface) {
	testWatch(ctx, t, store, false)
	testWatch(ctx, t, store, true)
}

// It tests that
// - first occurrence of objects should notify Add event
// - update should trigger Modified event
// - update that gets filtered should trigger Deleted event
func testWatch(ctx context.Context, t *testing.T, store storage.Interface, recursive bool) {
	basePod := &example.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "foo"},
		Spec:       example.PodSpec{NodeName: ""},
	}
	basePodAssigned := &example.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "foo"},
		Spec:       example.PodSpec{NodeName: "bar"},
	}

	selectedPod := func(pod *example.Pod) *example.Pod {
		result := pod.DeepCopy()
		result.Labels = map[string]string{"select": "true"}
		return result
	}

	tests := []struct {
		name       string
		namespace  string
		key        string
		pred       storage.SelectionPredicate
		watchTests []*testWatchStruct
	}{{
		name:       "create a key",
		namespace:  fmt.Sprintf("test-ns-1-%t", recursive),
		watchTests: []*testWatchStruct{{basePod, true, watch.Added}},
		pred:       storage.Everything,
	}, {
		name:       "key updated to match predicate",
		namespace:  fmt.Sprintf("test-ns-2-%t", recursive),
		watchTests: []*testWatchStruct{{basePod, false, ""}, {basePodAssigned, true, watch.Added}},
		pred: storage.SelectionPredicate{
			Label: labels.Everything(),
			Field: fields.ParseSelectorOrDie("spec.nodeName=bar"),
			GetAttrs: func(obj runtime.Object) (labels.Set, fields.Set, error) {
				pod := obj.(*example.Pod)
				return nil, fields.Set{"spec.nodeName": pod.Spec.NodeName}, nil
			},
		},
	}, {
		name:       "update",
		namespace:  fmt.Sprintf("test-ns-3-%t", recursive),
		watchTests: []*testWatchStruct{{basePod, true, watch.Added}, {basePodAssigned, true, watch.Modified}},
		pred:       storage.Everything,
	}, {
		name:       "delete because of being filtered",
		namespace:  fmt.Sprintf("test-ns-4-%t", recursive),
		watchTests: []*testWatchStruct{{basePod, true, watch.Added}, {basePodAssigned, true, watch.Deleted}},
		pred: storage.SelectionPredicate{
			Label: labels.Everything(),
			Field: fields.ParseSelectorOrDie("spec.nodeName!=bar"),
			GetAttrs: func(obj runtime.Object) (labels.Set, fields.Set, error) {
				pod := obj.(*example.Pod)
				return nil, fields.Set{"spec.nodeName": pod.Spec.NodeName}, nil
			},
		},
	}, {
		name:      "filtering",
		namespace: fmt.Sprintf("test-ns-5-%t", recursive),
		watchTests: []*testWatchStruct{
			{selectedPod(basePod), true, watch.Added},
			{basePod, true, watch.Deleted},
			{selectedPod(basePod), true, watch.Added},
			{selectedPod(basePodAssigned), true, watch.Modified},
			{nil, true, watch.Deleted},
		},
		pred: storage.SelectionPredicate{
			Label: labels.SelectorFromSet(labels.Set{"select": "true"}),
			Field: fields.Everything(),
			GetAttrs: func(obj runtime.Object) (labels.Set, fields.Set, error) {
				pod := obj.(*example.Pod)
				return labels.Set(pod.Labels), nil, nil
			},
		},
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			watchKey := fmt.Sprintf("/pods/%s", tt.namespace)
			key := watchKey + "/foo"
			if !recursive {
				watchKey = key
			}

			// Get the current RV from which we can start watching.
			out := &example.PodList{}
			if err := store.GetList(ctx, watchKey, storage.ListOptions{ResourceVersion: "", Predicate: tt.pred, Recursive: recursive}, out); err != nil {
				t.Fatalf("List failed: %v", err)
			}

			w, err := store.Watch(ctx, watchKey, storage.ListOptions{ResourceVersion: out.ResourceVersion, Predicate: tt.pred, Recursive: recursive})
			if err != nil {
				t.Fatalf("Watch failed: %v", err)
			}

			// Create a pod in a different namespace first to ensure
			// that its corresponding event will not be propagated.
			badKey := fmt.Sprintf("/pods/%s-bad/foo", tt.namespace)
			badOut := &example.Pod{}
			err = store.GuaranteedUpdate(ctx, badKey, badOut, true, nil, storage.SimpleUpdate(
				func(runtime.Object) (runtime.Object, error) {
					obj := basePod.DeepCopy()
					obj.Namespace = fmt.Sprintf("%s-bad", tt.namespace)
					return obj, nil
				}), nil)
			if err != nil {
				t.Fatalf("GuaranteedUpdate of bad pod failed: %v", err)
			}

			var prevObj *example.Pod
			for _, watchTest := range tt.watchTests {
				out := &example.Pod{}
				if watchTest.obj != nil {
					err := store.GuaranteedUpdate(ctx, key, out, true, nil, storage.SimpleUpdate(
						func(runtime.Object) (runtime.Object, error) {
							obj := watchTest.obj.DeepCopy()
							obj.Namespace = tt.namespace
							return obj, nil
						}), nil)
					if err != nil {
						t.Fatalf("GuaranteedUpdate failed: %v", err)
					}
				} else {
					err := store.Delete(ctx, key, out, nil, storage.ValidateAllObjectFunc, nil, storage.DeleteOptions{})
					if err != nil {
						t.Fatalf("Delete failed: %v", err)
					}
				}
				if watchTest.expectEvent {
					expectObj := out
					if watchTest.watchType == watch.Deleted {
						expectObj = prevObj
						expectObj.ResourceVersion = out.ResourceVersion
					}
					testCheckResult(t, w, watch.Event{Type: watchTest.watchType, Object: expectObj})
				}
				prevObj = out
			}
			w.Stop()
			testCheckStop(t, w)
		})
	}
}

// RunTestWatchFromZero tests that
//   - watch from 0 should sync up and grab the object added before
//   - For testing with etcd, watch from 0 is able to return events for objects
//     whose previous version has been compacted. If testing with cacher, we
//     expect compaction to be nil.
func RunTestWatchFromZero(ctx context.Context, t *testing.T, store storage.Interface, compaction Compaction) {
	key, storedObj := testPropagateStore(ctx, t, store, &example.Pod{ObjectMeta: metav1.ObjectMeta{Name: "foo", Namespace: "test-ns"}})

	w, err := store.Watch(ctx, key, storage.ListOptions{ResourceVersion: "0", Predicate: storage.Everything})
	if err != nil {
		t.Fatalf("Watch failed: %v", err)
	}
	testCheckResult(t, w, watch.Event{Type: watch.Added, Object: storedObj})

	// Update
	out := &example.Pod{}
	err = store.GuaranteedUpdate(ctx, key, out, true, nil, storage.SimpleUpdate(
		func(runtime.Object) (runtime.Object, error) {
			return &example.Pod{ObjectMeta: metav1.ObjectMeta{Name: "foo", Namespace: "test-ns", Annotations: map[string]string{"a": "1"}}}, nil
		}), nil)
	if err != nil {
		t.Fatalf("GuaranteedUpdate failed: %v", err)
	}

	// Check that we receive a modified watch event. This check also
	// indirectly ensures that the cache is synced. This is important
	// when testing with the Cacher since we may have to allow for slow
	// processing by allowing updates to propagate to the watch cache.
	// This allows for that.
	testCheckResult(t, w, watch.Event{Type: watch.Modified, Object: out})
	w.Stop()

	// Make sure when we watch from 0 we receive an ADDED event
	w, err = store.Watch(ctx, key, storage.ListOptions{ResourceVersion: "0", Predicate: storage.Everything})
	if err != nil {
		t.Fatalf("Watch failed: %v", err)
	}

	testCheckResult(t, w, watch.Event{Type: watch.Added, Object: out})
	w.Stop()

	// Compact previous versions
	if compaction == nil {
		t.Skip("compaction callback not provided")
	}

	// Update again
	newOut := &example.Pod{}
	err = store.GuaranteedUpdate(ctx, key, newOut, true, nil, storage.SimpleUpdate(
		func(runtime.Object) (runtime.Object, error) {
			return &example.Pod{ObjectMeta: metav1.ObjectMeta{Name: "foo", Namespace: "test-ns"}}, nil
		}), nil)
	if err != nil {
		t.Fatalf("GuaranteedUpdate failed: %v", err)
	}

	// Compact previous versions
	compaction(ctx, t, newOut.ResourceVersion)

	// Make sure we can still watch from 0 and receive an ADDED event
	w, err = store.Watch(ctx, key, storage.ListOptions{ResourceVersion: "0", Predicate: storage.Everything})
	defer w.Stop()
	if err != nil {
		t.Fatalf("Watch failed: %v", err)
	}
	testCheckResult(t, w, watch.Event{Type: watch.Added, Object: newOut})

	// Make sure we can't watch from older resource versions anymoer and get a "Gone" error.
	tooOldWatcher, err := store.Watch(ctx, key, storage.ListOptions{ResourceVersion: out.ResourceVersion, Predicate: storage.Everything})
	if err != nil {
		t.Fatalf("Watch failed: %v", err)
	}
	defer tooOldWatcher.Stop()
	expiredError := errors.NewResourceExpired("").ErrStatus
	// TODO(wojtek-t): It seems that etcd is currently returning a different error,
	// being an Internal error of "etcd event received with PrevKv=nil".
	// We temporary allow both but we should unify here.
	internalError := metav1.Status{
		Status: metav1.StatusFailure,
		Code:   http.StatusInternalServerError,
		Reason: metav1.StatusReasonInternalError,
	}
	testCheckResultFunc(t, tooOldWatcher, func(actualEvent watch.Event) {
		expectNoDiff(t, "incorrect event type", watch.Error, actualEvent.Type)
		if !apiequality.Semantic.DeepDerivative(&expiredError, actualEvent.Object) && !apiequality.Semantic.DeepDerivative(&internalError, actualEvent.Object) {
			t.Errorf("expected: %#v; got %#v", &expiredError, actualEvent.Object)
		}
	})
}

func RunTestDeleteTriggerWatch(ctx context.Context, t *testing.T, store storage.Interface) {
	key, storedObj := testPropagateStore(ctx, t, store, &example.Pod{ObjectMeta: metav1.ObjectMeta{Name: "foo", Namespace: "test-ns"}})
	w, err := store.Watch(ctx, key, storage.ListOptions{ResourceVersion: storedObj.ResourceVersion, Predicate: storage.Everything})
	if err != nil {
		t.Fatalf("Watch failed: %v", err)
	}
	if err := store.Delete(ctx, key, &example.Pod{}, nil, storage.ValidateAllObjectFunc, nil, storage.DeleteOptions{}); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}
	testCheckEventType(t, w, watch.Deleted)
}

func RunTestWatchFromNonZero(ctx context.Context, t *testing.T, store storage.Interface) {
	key, storedObj := testPropagateStore(ctx, t, store, &example.Pod{ObjectMeta: metav1.ObjectMeta{Name: "foo", Namespace: "test-ns"}})

	w, err := store.Watch(ctx, key, storage.ListOptions{ResourceVersion: storedObj.ResourceVersion, Predicate: storage.Everything})
	if err != nil {
		t.Fatalf("Watch failed: %v", err)
	}
	out := &example.Pod{}
	store.GuaranteedUpdate(ctx, key, out, true, nil, storage.SimpleUpdate(
		func(runtime.Object) (runtime.Object, error) {
			newObj := storedObj.DeepCopy()
			newObj.Annotations = map[string]string{"version": "2"}
			return newObj, nil
		}), nil)
	testCheckResult(t, w, watch.Event{Type: watch.Modified, Object: out})
}

func RunTestDelayedWatchDelivery(ctx context.Context, t *testing.T, store storage.Interface) {
	_, storedObj := testPropagateStore(ctx, t, store, &example.Pod{ObjectMeta: metav1.ObjectMeta{Name: "foo", Namespace: "test-ns"}})
	startRV := storedObj.ResourceVersion

	watcher, err := store.Watch(ctx, "/pods/test-ns", storage.ListOptions{ResourceVersion: startRV, Predicate: storage.Everything, Recursive: true})
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	// Depending on the implementation, different number of events that
	// should be delivered to the watcher can be created before it will
	// block the implementation and as a result force the watcher to be
	// closed (as otherwise events would have to be dropped).
	// For now, this number is smallest for Cacher and it equals 21 for it.
	totalPods := 21
	for i := 0; i < totalPods; i++ {
		out := &example.Pod{}
		pod := &example.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("foo-%d", i), Namespace: "test-ns"},
		}
		err := store.GuaranteedUpdate(ctx, computePodKey(pod), out, true, nil, storage.SimpleUpdate(
			func(runtime.Object) (runtime.Object, error) {
				return pod, nil
			}), nil)
		if err != nil {
			t.Errorf("GuaranteedUpdate failed: %v", err)
		}
	}

	// Now stop the watcher and check if the consecutive events are being delivered.
	watcher.Stop()

	watched := 0
	for {
		event, ok := <-watcher.ResultChan()
		if !ok {
			break
		}
		object := event.Object
		if co, ok := object.(runtime.CacheableObject); ok {
			object = co.GetObject()
		}
		if a, e := object.(*example.Pod).Name, fmt.Sprintf("foo-%d", watched); e != a {
			t.Errorf("Unexpected object watched: %s, expected %s", a, e)
		}
		watched++
	}
	// We expect at least N events to be delivered, depending on the implementation.
	// For now, this number is smallest for Cacher and it equals 10 (size of the out buffer).
	if watched < 10 {
		t.Errorf("Unexpected number of events: %v, expected: %v", watched, totalPods)
	}
}

func RunTestWatchError(ctx context.Context, t *testing.T, store InterfaceWithPrefixTransformer) {
	obj := &example.Pod{ObjectMeta: metav1.ObjectMeta{Name: "foo", Namespace: "test-ns"}}
	key := computePodKey(obj)

	// Compute the initial resource version from which we can start watching later.
	list := &example.PodList{}
	storageOpts := storage.ListOptions{
		ResourceVersion: "0",
		Predicate:       storage.Everything,
		Recursive:       true,
	}
	if err := store.GetList(ctx, "/pods", storageOpts, list); err != nil {
		t.Errorf("Unexpected error: %v", err)
	}

	if err := store.GuaranteedUpdate(ctx, key, &example.Pod{}, true, nil, storage.SimpleUpdate(
		func(runtime.Object) (runtime.Object, error) {
			return obj, nil
		}), nil); err != nil {
		t.Fatalf("GuaranteedUpdate failed: %v", err)
	}

	// Now trigger watch error by injecting failing transformer.
	revertTransformer := store.UpdatePrefixTransformer(
		func(previousTransformer *PrefixTransformer) value.Transformer {
			return &failingTransformer{}
		})
	defer revertTransformer()

	w, err := store.Watch(ctx, key, storage.ListOptions{ResourceVersion: list.ResourceVersion, Predicate: storage.Everything})
	if err != nil {
		t.Fatalf("Watch failed: %v", err)
	}
	testCheckEventType(t, w, watch.Error)
}

func RunTestWatchWithUnsafeDelete(ctx context.Context, t *testing.T, store InterfaceWithCorruptTransformer) {
	obj := &example.Pod{ObjectMeta: metav1.ObjectMeta{Name: "foo", Namespace: "test-ns"}}
	key := computePodKey(obj)

	out := &example.Pod{}
	if err := store.Create(ctx, key, obj, out, 0); err != nil {
		t.Fatalf("failed to create object in the store: %v", err)
	}

	// Compute the initial resource version from which we can start watching later.
	list := &example.PodList{}
	storageOpts := storage.ListOptions{
		ResourceVersion: "0",
		Predicate:       storage.Everything,
		Recursive:       true,
	}
	if err := store.GetList(ctx, "/pods", storageOpts, list); err != nil {
		t.Errorf("Unexpected error: %v", err)
	}

	// Now trigger watch error by injecting a failing transformer where every object is corrupt.
	revertTransformer := store.CorruptTransformer(nil)
	defer revertTransformer()

	w, err := store.Watch(ctx, key, storage.ListOptions{ResourceVersion: list.ResourceVersion, Predicate: storage.Everything})
	if err != nil {
		t.Fatalf("Watch failed: %v", err)
	}

	// normal deletetion should fail
	if err := store.Delete(ctx, key, &example.Pod{}, nil, storage.ValidateAllObjectFunc, nil, storage.DeleteOptions{}); err == nil {
		t.Fatalf("Expected normal Delete to fail")
	}
	if err := store.Delete(ctx, key, &example.Pod{}, nil, storage.ValidateAllObjectFunc, nil, storage.DeleteOptions{IgnoreStoreReadError: true}); err != nil {
		t.Fatalf("Expected unsafe Delete to succeed, but got: %v", err)
	}

	testCheckResultFunc(t, w, func(got watch.Event) {
		if want, got := watch.Error, got.Type; want != got {
			t.Errorf("Expected event type: %q, but got: %q", want, got)
		}
		switch v := got.Object.(type) {
		case *metav1.Status:
			if want, got := metav1.StatusReasonStoreReadError, v.Reason; want != got {
				t.Errorf("Expected reason: %q, but got: %q", want, got)
			}
			if want := "saw a DELETED event, but object data is corrupt"; !strings.Contains(v.Message, want) {
				t.Errorf("Expected Message to contain: %q, but got: %q", want, v.Message)
			}
		default:
			t.Errorf("expected an metav1 Status object, but got: %v", got.Object)
		}
	})
}

func RunTestWatchContextCancel(ctx context.Context, t *testing.T, store storage.Interface) {
	canceledCtx, cancel := context.WithCancel(ctx)
	cancel()
	// When we watch with a canceled context, we should detect that it's context canceled.
	// We won't take it as error and also close the watcher.
	w, err := store.Watch(canceledCtx, "/pods/not-existing", storage.ListOptions{
		ResourceVersion: "0",
		Predicate:       storage.Everything,
	})
	if err != nil {
		t.Fatal(err)
	}

	select {
	case _, ok := <-w.ResultChan():
		if ok {
			t.Error("ResultChan() should be closed")
		}
	case <-time.After(wait.ForeverTestTimeout):
		t.Errorf("timeout after %v", wait.ForeverTestTimeout)
	}
}

func RunTestWatcherTimeout(ctx context.Context, t *testing.T, store storage.Interface) {
	// initialRV is used to initate the watcher at the beginning of the world.
	podList := example.PodList{}
	options := storage.ListOptions{
		Predicate: storage.Everything,
		Recursive: true,
	}
	if err := store.GetList(ctx, "/pods", options, &podList); err != nil {
		t.Fatalf("Failed to list pods: %v", err)
	}
	initialRV := podList.ResourceVersion

	options = storage.ListOptions{
		ResourceVersion: initialRV,
		Predicate:       storage.Everything,
		Recursive:       true,
	}

	// Create a number of watchers that will not be reading any result.
	nonReadingWatchers := 50
	for i := 0; i < nonReadingWatchers; i++ {
		watcher, err := store.Watch(ctx, "/pods/test-ns", options)
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}
		defer watcher.Stop()
	}

	// Create a second watcher that will be reading result.
	readingWatcher, err := store.Watch(ctx, "/pods/test-ns", options)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	defer readingWatcher.Stop()

	// Depending on the implementation, different number of events that
	// should be delivered to the watcher can be created before it will
	// block the implementation and as a result force the watcher to be
	// closed (as otherwise events would have to be dropped).
	// For now, this number is smallest for Cacher and it equals 21 for it.
	//
	// Create more events to ensure that we're not blocking other watchers
	// forever.
	startTime := time.Now()
	for i := 0; i < 22; i++ {
		out := &example.Pod{}
		pod := &example.Pod{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("foo-%d", i), Namespace: "test-ns"}}
		if err := store.Create(ctx, computePodKey(pod), pod, out, 0); err != nil {
			t.Fatalf("Create failed: %v", err)
		}
		testCheckResult(t, readingWatcher, watch.Event{Type: watch.Added, Object: out})
	}
	if time.Since(startTime) > time.Duration(250*nonReadingWatchers)*time.Millisecond {
		t.Errorf("waiting for events took too long: %v", time.Since(startTime))
	}
}

func RunTestWatchDeleteEventObjectHaveLatestRV(ctx context.Context, t *testing.T, store storage.Interface) {
	key, storedObj := testPropagateStore(ctx, t, store, &example.Pod{ObjectMeta: metav1.ObjectMeta{Name: "foo", Namespace: "test-ns"}})

	watchCtx, cancel := context.WithTimeout(ctx, wait.ForeverTestTimeout)
	t.Cleanup(cancel)
	w, err := store.Watch(watchCtx, key, storage.ListOptions{ResourceVersion: storedObj.ResourceVersion, Predicate: storage.Everything})
	if err != nil {
		t.Fatalf("Watch failed: %v", err)
	}

	deletedObj := &example.Pod{}
	if err := store.Delete(ctx, key, deletedObj, &storage.Preconditions{}, storage.ValidateAllObjectFunc, nil, storage.DeleteOptions{}); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}

	// Verify that ResourceVersion has changed on deletion.
	if storedObj.ResourceVersion == deletedObj.ResourceVersion {
		t.Fatalf("ResourceVersion didn't changed on deletion: %s", deletedObj.ResourceVersion)
	}

	testCheckResult(t, w, watch.Event{Type: watch.Deleted, Object: deletedObj})
}

func RunTestWatchInitializationSignal(ctx context.Context, t *testing.T, store storage.Interface) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	t.Cleanup(cancel)
	initSignal := utilflowcontrol.NewInitializationSignal()
	ctx = utilflowcontrol.WithInitializationSignal(ctx, initSignal)

	key, storedObj := testPropagateStore(ctx, t, store, &example.Pod{ObjectMeta: metav1.ObjectMeta{Name: "foo", Namespace: "test-ns"}})
	_, err := store.Watch(ctx, key, storage.ListOptions{ResourceVersion: storedObj.ResourceVersion, Predicate: storage.Everything})
	if err != nil {
		t.Fatalf("Watch failed: %v", err)
	}

	initSignal.Wait()
}

// RunOptionalTestProgressNotify tests ProgressNotify feature of ListOptions.
// Given this feature is currently not explicitly used by higher layers of Kubernetes
// (it rather is used by wrappers of storage.Interface to implement its functionalities)
// this test is currently considered optional.
func RunOptionalTestProgressNotify(ctx context.Context, t *testing.T, store storage.Interface) {
	input := &example.Pod{ObjectMeta: metav1.ObjectMeta{Name: "name", Namespace: "test-ns"}}
	key := computePodKey(input)
	out := &example.Pod{}
	if err := store.Create(ctx, key, input, out, 0); err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	validateResourceVersion := resourceVersionNotOlderThan(out.ResourceVersion)

	opts := storage.ListOptions{
		ResourceVersion: out.ResourceVersion,
		Predicate:       storage.Everything,
		ProgressNotify:  true,
	}
	w, err := store.Watch(ctx, key, opts)
	if err != nil {
		t.Fatalf("Watch failed: %v", err)
	}

	// when we send a bookmark event, the client expects the event to contain an
	// object of the correct type, but with no fields set other than the resourceVersion
	testCheckResultFunc(t, w, func(actualEvent watch.Event) {
		expectNoDiff(t, "incorrect event type", watch.Bookmark, actualEvent.Type)
		// first, check that we have the correct resource version
		obj, ok := actualEvent.Object.(metav1.Object)
		if !ok {
			t.Fatalf("got %T, not metav1.Object", actualEvent.Object)
		}
		if err := validateResourceVersion(obj.GetResourceVersion()); err != nil {
			t.Fatal(err)
		}

		// then, check that we have the right type and content
		pod, ok := actualEvent.Object.(*example.Pod)
		if !ok {
			t.Fatalf("got %T, not *example.Pod", actualEvent.Object)
		}
		pod.ResourceVersion = ""
		expectNoDiff(t, "bookmark event should contain an object with no fields set other than resourceVersion", &example.Pod{}, pod)
	})
}

// It tests watches of cluster-scoped resources.
func RunTestClusterScopedWatch(ctx context.Context, t *testing.T, store storage.Interface) {
	tests := []struct {
		name string
		// For watch request, the name of object is specified with field selector
		// "metadata.name=objectName". So in this watch tests, we should set the
		// requestedName and field selector "metadata.name=requestedName" at the
		// same time or set neighter of them.
		requestedName string
		recursive     bool
		fieldSelector fields.Selector
		indexFields   []string
		watchTests    []*testWatchStruct
	}{
		{
			name:          "cluster-wide watch, request without name, without field selector",
			recursive:     true,
			fieldSelector: fields.Everything(),
			watchTests: []*testWatchStruct{
				{basePod("t1-foo1"), true, watch.Added},
				{basePodUpdated("t1-foo1"), true, watch.Modified},
				{basePodAssigned("t1-foo2", "t1-bar1"), true, watch.Added},
			},
		},
		{
			name:          "cluster-wide watch, request without name, field selector with spec.nodeName",
			recursive:     true,
			fieldSelector: fields.ParseSelectorOrDie("spec.nodeName=t2-bar1"),
			indexFields:   []string{"spec.nodeName"},
			watchTests: []*testWatchStruct{
				{basePod("t2-foo1"), false, ""},
				{basePodAssigned("t2-foo1", "t2-bar1"), true, watch.Added},
			},
		},
		{
			name:          "cluster-wide watch, request without name, field selector with spec.nodeName to filter out watch",
			recursive:     true,
			fieldSelector: fields.ParseSelectorOrDie("spec.nodeName!=t3-bar1"),
			indexFields:   []string{"spec.nodeName"},
			watchTests: []*testWatchStruct{
				{basePod("t3-foo1"), true, watch.Added},
				{basePod("t3-foo2"), true, watch.Added},
				{basePodUpdated("t3-foo1"), true, watch.Modified},
				{basePodAssigned("t3-foo1", "t3-bar1"), true, watch.Deleted},
			},
		},
		{
			name:          "cluster-wide watch, request with name, field selector with metadata.name",
			requestedName: "t4-foo1",
			fieldSelector: fields.ParseSelectorOrDie("metadata.name=t4-foo1"),
			watchTests: []*testWatchStruct{
				{basePod("t4-foo1"), true, watch.Added},
				{basePod("t4-foo2"), false, ""},
				{basePodUpdated("t4-foo1"), true, watch.Modified},
				{basePodUpdated("t4-foo2"), false, ""},
			},
		},
		{
			name:          "cluster-wide watch, request with name, field selector with metadata.name and spec.nodeName",
			requestedName: "t5-foo1",
			fieldSelector: fields.SelectorFromSet(fields.Set{
				"metadata.name": "t5-foo1",
				"spec.nodeName": "t5-bar1",
			}),
			indexFields: []string{"spec.nodeName"},
			watchTests: []*testWatchStruct{
				{basePod("t5-foo1"), false, ""},
				{basePod("t5-foo2"), false, ""},
				{basePodUpdated("t5-foo1"), false, ""},
				{basePodUpdated("t5-foo2"), false, ""},
				{basePodAssigned("t5-foo1", "t5-bar1"), true, watch.Added},
			},
		},
		{
			name:          "cluster-wide watch, request with name, field selector with metadata.name, and with spec.nodeName to filter out watch",
			requestedName: "t6-foo1",
			fieldSelector: fields.AndSelectors(
				fields.ParseSelectorOrDie("spec.nodeName!=t6-bar1"),
				fields.SelectorFromSet(fields.Set{"metadata.name": "t6-foo1"}),
			),
			indexFields: []string{"spec.nodeName"},
			watchTests: []*testWatchStruct{
				{basePod("t6-foo1"), true, watch.Added},
				{basePod("t6-foo2"), false, ""},
				{basePodUpdated("t6-foo1"), true, watch.Modified},
				{basePodAssigned("t6-foo1", "t6-bar1"), true, watch.Deleted},
				{basePodAssigned("t6-foo2", "t6-bar1"), false, ""},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requestInfo := &genericapirequest.RequestInfo{}
			requestInfo.Name = tt.requestedName
			requestInfo.Namespace = ""
			ctx = genericapirequest.WithRequestInfo(ctx, requestInfo)
			ctx = genericapirequest.WithNamespace(ctx, "")

			watchKey := "/pods"
			if tt.requestedName != "" {
				watchKey += "/" + tt.requestedName
			}

			predicate := CreatePodPredicate(tt.fieldSelector, false, tt.indexFields)

			list := &example.PodList{}
			opts := storage.ListOptions{
				ResourceVersion: "",
				Predicate:       predicate,
				Recursive:       true,
			}
			if err := store.GetList(ctx, "/pods", opts, list); err != nil {
				t.Errorf("Unexpected error: %v", err)
			}

			opts.ResourceVersion = list.ResourceVersion
			opts.Recursive = tt.recursive

			w, err := store.Watch(ctx, watchKey, opts)
			if err != nil {
				t.Fatalf("Watch failed: %v", err)
			}

			currentObjs := map[string]*example.Pod{}
			for _, watchTest := range tt.watchTests {
				out := &example.Pod{}
				key := "pods/" + watchTest.obj.Name
				err := store.GuaranteedUpdate(ctx, key, out, true, nil, storage.SimpleUpdate(
					func(runtime.Object) (runtime.Object, error) {
						obj := watchTest.obj.DeepCopy()
						return obj, nil
					}), nil)
				if err != nil {
					t.Fatalf("GuaranteedUpdate failed: %v", err)
				}

				expectObj := out
				if watchTest.watchType == watch.Deleted {
					expectObj = currentObjs[watchTest.obj.Name]
					expectObj.ResourceVersion = out.ResourceVersion
					delete(currentObjs, watchTest.obj.Name)
				} else {
					currentObjs[watchTest.obj.Name] = out
				}
				if watchTest.expectEvent {
					testCheckResult(t, w, watch.Event{Type: watchTest.watchType, Object: expectObj})
				}
			}
			w.Stop()
			testCheckStop(t, w)
		})
	}
}

// It tests watch of namespace-scoped resources.
func RunTestNamespaceScopedWatch(ctx context.Context, t *testing.T, store storage.Interface) {
	tests := []struct {
		name string
		// For watch request, the name of object is specified with field selector
		// "metadata.name=objectName". So in this watch tests, we should set the
		// requestedName and field selector "metadata.name=requestedName" at the
		// same time or set neighter of them.
		requestedName      string
		requestedNamespace string
		recursive          bool
		fieldSelector      fields.Selector
		indexFields        []string
		watchTests         []*testWatchStruct
	}{
		{
			name:          "namespaced watch, request without name, request without namespace, without field selector",
			recursive:     true,
			fieldSelector: fields.Everything(),
			watchTests: []*testWatchStruct{
				{baseNamespacedPod("t1-foo1", "t1-ns1"), true, watch.Added},
				{baseNamespacedPod("t1-foo2", "t1-ns2"), true, watch.Added},
				{baseNamespacedPodUpdated("t1-foo1", "t1-ns1"), true, watch.Modified},
				{baseNamespacedPodUpdated("t1-foo2", "t1-ns2"), true, watch.Modified},
			},
		},
		{
			name:          "namespaced watch, request without name, request without namespace, field selector with metadata.namespace",
			recursive:     true,
			fieldSelector: fields.ParseSelectorOrDie("metadata.namespace=t2-ns1"),
			watchTests: []*testWatchStruct{
				{baseNamespacedPod("t2-foo1", "t2-ns1"), true, watch.Added},
				{baseNamespacedPod("t2-foo1", "t2-ns2"), false, ""},
				{baseNamespacedPodUpdated("t2-foo1", "t2-ns1"), true, watch.Modified},
				{baseNamespacedPodUpdated("t2-foo1", "t2-ns2"), false, ""},
			},
		},
		{
			name:          "namespaced watch, request without name, request without namespace, field selector with spec.nodename",
			recursive:     true,
			fieldSelector: fields.ParseSelectorOrDie("spec.nodeName=t3-bar1"),
			indexFields:   []string{"spec.nodeName"},
			watchTests: []*testWatchStruct{
				{baseNamespacedPod("t3-foo1", "t3-ns1"), false, ""},
				{baseNamespacedPod("t3-foo2", "t3-ns2"), false, ""},
				{baseNamespacedPodAssigned("t3-foo1", "t3-ns1", "t3-bar1"), true, watch.Added},
				{baseNamespacedPodAssigned("t3-foo2", "t3-ns2", "t3-bar1"), true, watch.Added},
			},
		},
		{
			name:          "namespaced watch, request without name, request without namespace, field selector with spec.nodename to filter out watch",
			recursive:     true,
			fieldSelector: fields.ParseSelectorOrDie("spec.nodeName!=t4-bar1"),
			indexFields:   []string{"spec.nodeName"},
			watchTests: []*testWatchStruct{
				{baseNamespacedPod("t4-foo1", "t4-ns1"), true, watch.Added},
				{baseNamespacedPod("t4-foo2", "t4-ns1"), true, watch.Added},
				{baseNamespacedPodUpdated("t4-foo1", "t4-ns1"), true, watch.Modified},
				{baseNamespacedPodAssigned("t4-foo1", "t4-ns1", "t4-bar1"), true, watch.Deleted},
			},
		},
		{
			name:               "namespaced watch, request without name, request with namespace, without field selector",
			requestedNamespace: "t5-ns1",
			recursive:          true,
			fieldSelector:      fields.Everything(),
			watchTests: []*testWatchStruct{
				{baseNamespacedPod("t5-foo1", "t5-ns1"), true, watch.Added},
				{baseNamespacedPod("t5-foo1", "t5-ns2"), false, ""},
				{baseNamespacedPod("t5-foo2", "t5-ns1"), true, watch.Added},
				{baseNamespacedPodUpdated("t5-foo1", "t5-ns1"), true, watch.Modified},
				{baseNamespacedPodUpdated("t5-foo1", "t5-ns2"), false, ""},
			},
		},
		{
			name:               "namespaced watch, request without name, request with namespace, field selector with matched metadata.namespace",
			requestedNamespace: "t6-ns1",
			recursive:          true,
			fieldSelector:      fields.ParseSelectorOrDie("metadata.namespace=t6-ns1"),
			watchTests: []*testWatchStruct{
				{baseNamespacedPod("t6-foo1", "t6-ns1"), true, watch.Added},
				{baseNamespacedPod("t6-foo1", "t6-ns2"), false, ""},
				{baseNamespacedPodUpdated("t6-foo1", "t6-ns1"), true, watch.Modified},
			},
		},
		{
			name:               "namespaced watch, request without name, request with namespace, field selector with non-matched metadata.namespace",
			requestedNamespace: "t7-ns1",
			recursive:          true,
			fieldSelector:      fields.ParseSelectorOrDie("metadata.namespace=t7-ns2"),
			watchTests: []*testWatchStruct{
				{baseNamespacedPod("t7-foo1", "t7-ns1"), false, ""},
				{baseNamespacedPod("t7-foo1", "t7-ns2"), false, ""},
				{baseNamespacedPodUpdated("t7-foo1", "t7-ns1"), false, ""},
				{baseNamespacedPodUpdated("t7-foo1", "t7-ns2"), false, ""},
			},
		},
		{
			name:               "namespaced watch, request without name, request with namespace, field selector with spec.nodename",
			requestedNamespace: "t8-ns1",
			recursive:          true,
			fieldSelector:      fields.ParseSelectorOrDie("spec.nodeName=t8-bar2"),
			indexFields:        []string{"spec.nodeName"},
			watchTests: []*testWatchStruct{
				{baseNamespacedPod("t8-foo1", "t8-ns1"), false, ""},
				{baseNamespacedPodAssigned("t8-foo1", "t8-ns1", "t8-bar1"), false, ""},
				{baseNamespacedPodAssigned("t8-foo1", "t8-ns2", "t8-bar2"), false, ""},
				{baseNamespacedPodAssigned("t8-foo1", "t8-ns1", "t8-bar2"), true, watch.Added},
			},
		},
		{
			name:               "namespaced watch, request without name, request with namespace, field selector with spec.nodename to filter out watch",
			requestedNamespace: "t9-ns2",
			recursive:          true,
			fieldSelector:      fields.ParseSelectorOrDie("spec.nodeName!=t9-bar1"),
			indexFields:        []string{"spec.nodeName"},
			watchTests: []*testWatchStruct{
				{baseNamespacedPod("t9-foo1", "t9-ns1"), false, ""},
				{baseNamespacedPod("t9-foo1", "t9-ns2"), true, watch.Added},
				{baseNamespacedPodAssigned("t9-foo1", "t9-ns2", "t9-bar1"), true, watch.Deleted},
				{baseNamespacedPodAssigned("t9-foo1", "t9-ns2", "t9-bar2"), true, watch.Added},
			},
		},
		{
			name:          "namespaced watch, request with name, request without namespace, field selector with metadata.name",
			requestedName: "t10-foo1",
			recursive:     true,
			fieldSelector: fields.ParseSelectorOrDie("metadata.name=t10-foo1"),
			watchTests: []*testWatchStruct{
				{baseNamespacedPod("t10-foo1", "t10-ns1"), true, watch.Added},
				{baseNamespacedPod("t10-foo1", "t10-ns2"), true, watch.Added},
				{baseNamespacedPod("t10-foo2", "t10-ns1"), false, ""},
				{baseNamespacedPodUpdated("t10-foo1", "t10-ns1"), true, watch.Modified},
				{baseNamespacedPodAssigned("t10-foo1", "t10-ns1", "t10-bar1"), true, watch.Modified},
			},
		},
		{
			name:          "namespaced watch, request with name, request without namespace, field selector with metadata.name and metadata.namespace",
			requestedName: "t11-foo1",
			recursive:     true,
			fieldSelector: fields.SelectorFromSet(fields.Set{
				"metadata.name":      "t11-foo1",
				"metadata.namespace": "t11-ns1",
			}),
			watchTests: []*testWatchStruct{
				{baseNamespacedPod("t11-foo1", "t11-ns1"), true, watch.Added},
				{baseNamespacedPod("t11-foo2", "t11-ns1"), false, ""},
				{baseNamespacedPod("t11-foo1", "t11-ns2"), false, ""},
				{baseNamespacedPodUpdated("t11-foo1", "t11-ns1"), true, watch.Modified},
				{baseNamespacedPodAssigned("t11-foo1", "t11-ns1", "t11-bar1"), true, watch.Modified},
			},
		},
		{
			name:          "namespaced watch, request with name, request without namespace, field selector with metadata.name and spec.nodeName",
			requestedName: "t12-foo1",
			recursive:     true,
			fieldSelector: fields.SelectorFromSet(fields.Set{
				"metadata.name": "t12-foo1",
				"spec.nodeName": "t12-bar1",
			}),
			indexFields: []string{"spec.nodeName"},
			watchTests: []*testWatchStruct{
				{baseNamespacedPod("t12-foo1", "t12-ns1"), false, ""},
				{baseNamespacedPodUpdated("t12-foo1", "t12-ns1"), false, ""},
				{baseNamespacedPodAssigned("t12-foo1", "t12-ns1", "t12-bar1"), true, watch.Added},
			},
		},
		{
			name:          "namespaced watch, request with name, request without namespace, field selector with metadata.name, and with spec.nodeName to filter out watch",
			requestedName: "t15-foo1",
			recursive:     true,
			fieldSelector: fields.AndSelectors(
				fields.ParseSelectorOrDie("spec.nodeName!=t15-bar1"),
				fields.SelectorFromSet(fields.Set{"metadata.name": "t15-foo1"}),
			),
			indexFields: []string{"spec.nodeName"},
			watchTests: []*testWatchStruct{
				{baseNamespacedPod("t15-foo1", "t15-ns1"), true, watch.Added},
				{baseNamespacedPod("t15-foo2", "t15-ns1"), false, ""},
				{baseNamespacedPodUpdated("t15-foo1", "t15-ns1"), true, watch.Modified},
				{baseNamespacedPodAssigned("t15-foo1", "t15-ns1", "t15-bar1"), true, watch.Deleted},
				{baseNamespacedPodAssigned("t15-foo1", "t15-ns1", "t15-bar2"), true, watch.Added},
			},
		},
		{
			name:               "namespaced watch, request with name, request with namespace, with field selector metadata.name",
			requestedName:      "t16-foo1",
			requestedNamespace: "t16-ns1",
			fieldSelector:      fields.ParseSelectorOrDie("metadata.name=t16-foo1"),
			watchTests: []*testWatchStruct{
				{baseNamespacedPod("t16-foo1", "t16-ns1"), true, watch.Added},
				{baseNamespacedPod("t16-foo2", "t16-ns1"), false, ""},
				{baseNamespacedPodUpdated("t16-foo1", "t16-ns1"), true, watch.Modified},
				{baseNamespacedPodAssigned("t16-foo1", "t16-ns1", "t16-bar1"), true, watch.Modified},
			},
		},
		{
			name:               "namespaced watch, request with name, request with namespace, with field selector metadata.name and metadata.namespace",
			requestedName:      "t17-foo2",
			requestedNamespace: "t17-ns1",
			fieldSelector: fields.SelectorFromSet(fields.Set{
				"metadata.name":      "t17-foo2",
				"metadata.namespace": "t17-ns1",
			}),
			watchTests: []*testWatchStruct{
				{baseNamespacedPod("t17-foo1", "t17-ns1"), false, ""},
				{baseNamespacedPod("t17-foo2", "t17-ns1"), true, watch.Added},
				{baseNamespacedPodUpdated("t17-foo1", "t17-ns1"), false, ""},
				{baseNamespacedPodAssigned("t17-foo2", "t17-ns1", "t17-bar1"), true, watch.Modified},
			},
		},
		{
			name:               "namespaced watch, request with name, request with namespace, with field selector metadata.name, metadata.namespace and spec.nodename",
			requestedName:      "t18-foo1",
			requestedNamespace: "t18-ns1",
			fieldSelector: fields.SelectorFromSet(fields.Set{
				"metadata.name":      "t18-foo1",
				"metadata.namespace": "t18-ns1",
				"spec.nodeName":      "t18-bar1",
			}),
			indexFields: []string{"spec.nodeName"},
			watchTests: []*testWatchStruct{
				{baseNamespacedPod("t18-foo1", "t18-ns1"), false, ""},
				{baseNamespacedPod("t18-foo2", "t18-ns1"), false, ""},
				{baseNamespacedPod("t18-foo1", "t18-ns2"), false, ""},
				{baseNamespacedPodUpdated("t18-foo1", "t18-ns1"), false, ""},
				{baseNamespacedPodAssigned("t18-foo1", "t18-ns1", "t18-bar1"), true, watch.Added},
			},
		},
		{
			name:               "namespaced watch, request with name, request with namespace, with field selector metadata.name, metadata.namespace, and with spec.nodename to filter out watch",
			requestedName:      "t19-foo2",
			requestedNamespace: "t19-ns1",
			fieldSelector: fields.AndSelectors(
				fields.ParseSelectorOrDie("spec.nodeName!=t19-bar1"),
				fields.SelectorFromSet(fields.Set{"metadata.name": "t19-foo2", "metadata.namespace": "t19-ns1"}),
			),
			indexFields: []string{"spec.nodeName"},
			watchTests: []*testWatchStruct{
				{baseNamespacedPod("t19-foo1", "t19-ns1"), false, ""},
				{baseNamespacedPod("t19-foo2", "t19-ns2"), false, ""},
				{baseNamespacedPod("t19-foo2", "t19-ns1"), true, watch.Added},
				{baseNamespacedPodUpdated("t19-foo2", "t19-ns1"), true, watch.Modified},
				{baseNamespacedPodAssigned("t19-foo2", "t19-ns1", "t19-bar1"), true, watch.Deleted},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requestInfo := &genericapirequest.RequestInfo{}
			requestInfo.Name = tt.requestedName
			requestInfo.Namespace = tt.requestedNamespace
			ctx = genericapirequest.WithRequestInfo(ctx, requestInfo)
			ctx = genericapirequest.WithNamespace(ctx, tt.requestedNamespace)

			watchKey := "/pods"
			if tt.requestedNamespace != "" {
				watchKey += "/" + tt.requestedNamespace
				if tt.requestedName != "" {
					watchKey += "/" + tt.requestedName
				}
			}

			predicate := CreatePodPredicate(tt.fieldSelector, true, tt.indexFields)

			list := &example.PodList{}
			opts := storage.ListOptions{
				ResourceVersion: "",
				Predicate:       predicate,
				Recursive:       true,
			}
			if err := store.GetList(ctx, "/pods", opts, list); err != nil {
				t.Errorf("Unexpected error: %v", err)
			}

			opts.ResourceVersion = list.ResourceVersion
			opts.Recursive = tt.recursive

			w, err := store.Watch(ctx, watchKey, opts)
			if err != nil {
				t.Fatalf("Watch failed: %v", err)
			}

			currentObjs := map[string]*example.Pod{}
			for _, watchTest := range tt.watchTests {
				out := &example.Pod{}
				key := "pods/" + watchTest.obj.Namespace + "/" + watchTest.obj.Name
				err := store.GuaranteedUpdate(ctx, key, out, true, nil, storage.SimpleUpdate(
					func(runtime.Object) (runtime.Object, error) {
						obj := watchTest.obj.DeepCopy()
						return obj, nil
					}), nil)
				if err != nil {
					t.Fatalf("GuaranteedUpdate failed: %v", err)
				}

				expectObj := out
				podIdentifier := watchTest.obj.Namespace + "/" + watchTest.obj.Name
				if watchTest.watchType == watch.Deleted {
					expectObj = currentObjs[podIdentifier]
					expectObj.ResourceVersion = out.ResourceVersion
					delete(currentObjs, podIdentifier)
				} else {
					currentObjs[podIdentifier] = out
				}
				if watchTest.expectEvent {
					testCheckResult(t, w, watch.Event{Type: watchTest.watchType, Object: expectObj})
				}
			}
			w.Stop()
			testCheckStop(t, w)
		})
	}
}

// RunOptionalTestWatchDispatchBookmarkEvents tests whether bookmark events are sent.
// This feature is currently implemented in watch cache layer, so this is optional.
//
// TODO(#109831): ProgressNotify feature is effectively implementing the same
//
//	functionality, so we should refactor this functionality to share the same input.
func RunTestWatchDispatchBookmarkEvents(ctx context.Context, t *testing.T, store storage.Interface, expectedWatchBookmarks bool) {
	key, storedObj := testPropagateStore(ctx, t, store, &example.Pod{ObjectMeta: metav1.ObjectMeta{Name: "foo", Namespace: "test-ns"}})
	startRV := storedObj.ResourceVersion

	tests := []struct {
		name                string
		timeout             time.Duration
		expected            bool
		allowWatchBookmarks bool
	}{
		{ // test old client won't get Bookmark event
			name:                "allowWatchBookmarks=false",
			timeout:             3 * time.Second,
			expected:            false,
			allowWatchBookmarks: false,
		},
		{
			name:                "allowWatchBookmarks=true",
			timeout:             3 * time.Second,
			expected:            expectedWatchBookmarks,
			allowWatchBookmarks: true,
		},
	}

	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pred := storage.Everything
			pred.AllowWatchBookmarks = tt.allowWatchBookmarks
			ctx, cancel := context.WithTimeout(context.Background(), tt.timeout)
			defer cancel()

			watcher, err := store.Watch(ctx, key, storage.ListOptions{ResourceVersion: startRV, Predicate: pred})
			if err != nil {
				t.Fatalf("Unexpected error: %v", err)
			}
			defer watcher.Stop()

			// Create events of pods in a different namespace
			out := &example.Pod{}
			obj := &example.Pod{ObjectMeta: metav1.ObjectMeta{Name: "foo", Namespace: fmt.Sprintf("other-ns-%d", i)}}
			objKey := computePodKey(obj)

			if err := store.Create(ctx, objKey, obj, out, 0); err != nil {
				t.Fatalf("Create failed: %v", err)
			}

			// Now wait for Bookmark event
			select {
			case event, ok := <-watcher.ResultChan():
				if !ok && tt.expected {
					t.Errorf("Unexpected object watched (no objects)")
				}
				if tt.expected && event.Type != watch.Bookmark {
					t.Errorf("Unexpected object watched %#v", event)
				}
			case <-time.After(time.Second * 3):
				if tt.expected {
					t.Errorf("Unexpected object watched (timeout)")
				}
			}
		})
	}
}

// RunOptionalTestWatchBookmarksWithCorrectResourceVersion tests whether bookmark events are
// sent with correct resource versions.
// This feature is currently implemented in watch cache layer, so this is optional.
//
// TODO(#109831): ProgressNotify feature is effectively implementing the same
//
//	functionality, so we should refactor this functionality to share the same input.
func RunTestOptionalWatchBookmarksWithCorrectResourceVersion(ctx context.Context, t *testing.T, store storage.Interface) {
	// Compute the initial resource version.
	list := &example.PodList{}
	storageOpts := storage.ListOptions{
		Predicate: storage.Everything,
		Recursive: true,
	}
	if err := store.GetList(ctx, "/pods", storageOpts, list); err != nil {
		t.Errorf("Unexpected error: %v", err)
	}
	startRV := list.ResourceVersion

	key := "/pods/test-ns"
	pred := storage.Everything
	pred.AllowWatchBookmarks = true

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	watcher, err := store.Watch(ctx, key, storage.ListOptions{ResourceVersion: startRV, Predicate: pred, Recursive: true})
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	defer watcher.Stop()

	done := make(chan struct{})
	errc := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	// We must wait for the waitgroup to exit before we terminate the cache or the server in prior defers.
	defer wg.Wait()
	// Call close first, so the goroutine knows to exit.
	defer close(done)

	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			select {
			case <-done:
				return
			default:
				out := &example.Pod{}
				pod := &example.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      fmt.Sprintf("foo-%d", i),
						Namespace: "test-ns",
					},
				}
				podKey := computePodKey(pod)
				if err := store.Create(ctx, podKey, pod, out, 0); err != nil {
					errc <- fmt.Errorf("failed to create pod %v: %v", pod, err)
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
		}
	}()

	bookmarkReceived := false
	lastObservedResourceVersion := uint64(0)

	for {
		select {
		case err := <-errc:
			t.Fatal(err)
		case event, ok := <-watcher.ResultChan():
			if !ok {
				// Make sure we have received a bookmark event
				if !bookmarkReceived {
					t.Fatalf("Unpexected error, we did not received a bookmark event")
				}
				return
			}
			rv, err := storage.APIObjectVersioner{}.ObjectResourceVersion(event.Object)
			if err != nil {
				t.Fatalf("failed to parse resourceVersion from %#v", event)
			}
			if event.Type == watch.Bookmark {
				bookmarkReceived = true
				// bookmark event has a RV greater than or equal to the before one
				if rv < lastObservedResourceVersion {
					t.Fatalf("Unexpected bookmark resourceVersion %v less than observed %v)", rv, lastObservedResourceVersion)
				}
			} else {
				// non-bookmark event has a RV greater than anything before
				if rv <= lastObservedResourceVersion {
					t.Fatalf("Unexpected event resourceVersion %v less than or equal to bookmark %v)", rv, lastObservedResourceVersion)
				}
			}
			lastObservedResourceVersion = rv
		}
	}
}

// RunSendInitialEventsBackwardCompatibility test backward compatibility
// when SendInitialEvents option is set against various implementations.
// Backward compatibility is defined as RV = "" || RV = "O" and AllowWatchBookmark is set to false.
// In that case we expect a watch request to be established.
func RunSendInitialEventsBackwardCompatibility(ctx context.Context, t *testing.T, store storage.Interface) {
	opts := storage.ListOptions{Predicate: storage.Everything}
	opts.SendInitialEvents = pointer.Bool(true)
	w, err := store.Watch(ctx, "/pods", opts)
	require.NoError(t, err)
	w.Stop()
}

// RunWatchSemantics test the following cases:
//
// +-----------------+---------------------+-------------------+
// | ResourceVersion | AllowWatchBookmarks | SendInitialEvents |
// +=================+=====================+===================+
// | Unset           | true/false          | true/false        |
// | 0               | true/false          | true/false        |
// | 1               | true/false          | true/false        |
// | Current         | true/false          | true/false        |
// +-----------------+---------------------+-------------------+
// where:
// - false indicates the value of the param was set to "false" by a test case
// - true  indicates the value of the param was set to "true" by a test case
func RunWatchSemantics(ctx context.Context, t *testing.T, store storage.Interface) {
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.WatchList, true)
	trueVal, falseVal := true, false
	addEventsFromCreatedPods := func(createdInitialPods []*example.Pod) []watch.Event {
		var ret []watch.Event
		for _, createdPod := range createdInitialPods {
			ret = append(ret, watch.Event{Type: watch.Added, Object: createdPod})
		}
		return ret
	}
	initialEventsEndFromLastCreatedPod := func(createdInitialPods []*example.Pod) watch.Event {
		return watch.Event{
			Type: watch.Bookmark,
			Object: &example.Pod{
				ObjectMeta: metav1.ObjectMeta{
					ResourceVersion: createdInitialPods[len(createdInitialPods)-1].ResourceVersion,
					Annotations:     map[string]string{metav1.InitialEventsAnnotationKey: "true"},
				},
			},
		}
	}
	scenarios := []struct {
		name                string
		allowWatchBookmarks bool
		sendInitialEvents   *bool
		resourceVersion     string
		// useCurrentRV if set gets the current RV from the storage
		// after adding the initial pods which is then used to establish a new watch request
		useCurrentRV bool

		initialPods                []*example.Pod
		podsAfterEstablishingWatch []*example.Pod

		expectedInitialEvents                      func(createdInitialPods []*example.Pod) []watch.Event
		expectedInitialEventsBookmarkWithMinimalRV func(createdInitialPods []*example.Pod) watch.Event
		expectedEventsAfterEstablishingWatch       func(createdPodsAfterWatch []*example.Pod) []watch.Event
	}{
		{
			name:                  "allowWatchBookmarks=true, sendInitialEvents=true, RV=unset",
			allowWatchBookmarks:   true,
			sendInitialEvents:     &trueVal,
			initialPods:           []*example.Pod{makePod("1"), makePod("2"), makePod("3")},
			expectedInitialEvents: addEventsFromCreatedPods,
			expectedInitialEventsBookmarkWithMinimalRV: initialEventsEndFromLastCreatedPod,
			podsAfterEstablishingWatch:                 []*example.Pod{makePod("4"), makePod("5")},
			expectedEventsAfterEstablishingWatch:       addEventsFromCreatedPods,
		},
		{
			name:                                 "allowWatchBookmarks=true, sendInitialEvents=false, RV=unset",
			allowWatchBookmarks:                  true,
			sendInitialEvents:                    &falseVal,
			initialPods:                          []*example.Pod{makePod("1"), makePod("2"), makePod("3")},
			podsAfterEstablishingWatch:           []*example.Pod{makePod("4"), makePod("5")},
			expectedEventsAfterEstablishingWatch: addEventsFromCreatedPods,
		},
		{
			name:                                 "allowWatchBookmarks=false, sendInitialEvents=false, RV=unset",
			sendInitialEvents:                    &falseVal,
			initialPods:                          []*example.Pod{makePod("1"), makePod("2"), makePod("3")},
			podsAfterEstablishingWatch:           []*example.Pod{makePod("4"), makePod("5")},
			expectedEventsAfterEstablishingWatch: addEventsFromCreatedPods,
		},
		{
			name:                                 "allowWatchBookmarks=false, sendInitialEvents=true, RV=unset",
			sendInitialEvents:                    &trueVal,
			initialPods:                          []*example.Pod{makePod("1"), makePod("2"), makePod("3")},
			expectedInitialEvents:                addEventsFromCreatedPods,
			podsAfterEstablishingWatch:           []*example.Pod{makePod("4"), makePod("5")},
			expectedEventsAfterEstablishingWatch: addEventsFromCreatedPods,
		},

		{
			name:                  "allowWatchBookmarks=true, sendInitialEvents=true, RV=0",
			allowWatchBookmarks:   true,
			sendInitialEvents:     &trueVal,
			resourceVersion:       "0",
			initialPods:           []*example.Pod{makePod("1"), makePod("2"), makePod("3")},
			expectedInitialEvents: addEventsFromCreatedPods,
			expectedInitialEventsBookmarkWithMinimalRV: initialEventsEndFromLastCreatedPod,
			podsAfterEstablishingWatch:                 []*example.Pod{makePod("4"), makePod("5")},
			expectedEventsAfterEstablishingWatch:       addEventsFromCreatedPods,
		},
		{
			name:                                 "allowWatchBookmarks=true, sendInitialEvents=false, RV=0",
			allowWatchBookmarks:                  true,
			sendInitialEvents:                    &falseVal,
			resourceVersion:                      "0",
			initialPods:                          []*example.Pod{makePod("1"), makePod("2"), makePod("3")},
			podsAfterEstablishingWatch:           []*example.Pod{makePod("4"), makePod("5")},
			expectedEventsAfterEstablishingWatch: addEventsFromCreatedPods,
		},
		{
			name:                                 "allowWatchBookmarks=false, sendInitialEvents=false, RV=0",
			sendInitialEvents:                    &falseVal,
			resourceVersion:                      "0",
			initialPods:                          []*example.Pod{makePod("1"), makePod("2"), makePod("3")},
			podsAfterEstablishingWatch:           []*example.Pod{makePod("4"), makePod("5")},
			expectedEventsAfterEstablishingWatch: addEventsFromCreatedPods,
		},
		{
			name:                                 "allowWatchBookmarks=false, sendInitialEvents=true, RV=0",
			sendInitialEvents:                    &trueVal,
			resourceVersion:                      "0",
			initialPods:                          []*example.Pod{makePod("1"), makePod("2"), makePod("3")},
			expectedInitialEvents:                addEventsFromCreatedPods,
			podsAfterEstablishingWatch:           []*example.Pod{makePod("4"), makePod("5")},
			expectedEventsAfterEstablishingWatch: addEventsFromCreatedPods,
		},

		{
			name:                  "allowWatchBookmarks=true, sendInitialEvents=true, RV=1",
			allowWatchBookmarks:   true,
			sendInitialEvents:     &trueVal,
			resourceVersion:       "1",
			initialPods:           []*example.Pod{makePod("1"), makePod("2"), makePod("3")},
			expectedInitialEvents: addEventsFromCreatedPods,
			expectedInitialEventsBookmarkWithMinimalRV: initialEventsEndFromLastCreatedPod,
			podsAfterEstablishingWatch:                 []*example.Pod{makePod("4"), makePod("5")},
			expectedEventsAfterEstablishingWatch:       addEventsFromCreatedPods,
		},
		{
			name:                                 "allowWatchBookmarks=true, sendInitialEvents=false, RV=1",
			allowWatchBookmarks:                  true,
			sendInitialEvents:                    &falseVal,
			resourceVersion:                      "1",
			initialPods:                          []*example.Pod{makePod("1"), makePod("2"), makePod("3")},
			expectedInitialEvents:                addEventsFromCreatedPods,
			podsAfterEstablishingWatch:           []*example.Pod{makePod("4"), makePod("5")},
			expectedEventsAfterEstablishingWatch: addEventsFromCreatedPods,
		},
		{
			name:                                 "allowWatchBookmarks=false, sendInitialEvents=false, RV=1",
			sendInitialEvents:                    &falseVal,
			resourceVersion:                      "1",
			initialPods:                          []*example.Pod{makePod("1"), makePod("2"), makePod("3")},
			expectedInitialEvents:                addEventsFromCreatedPods,
			podsAfterEstablishingWatch:           []*example.Pod{makePod("4"), makePod("5")},
			expectedEventsAfterEstablishingWatch: addEventsFromCreatedPods,
		},
		{
			name:                                 "allowWatchBookmarks=false, sendInitialEvents=true, RV=1",
			sendInitialEvents:                    &trueVal,
			resourceVersion:                      "1",
			initialPods:                          []*example.Pod{makePod("1"), makePod("2"), makePod("3")},
			expectedInitialEvents:                addEventsFromCreatedPods,
			podsAfterEstablishingWatch:           []*example.Pod{makePod("4"), makePod("5")},
			expectedEventsAfterEstablishingWatch: addEventsFromCreatedPods,
		},

		{
			name:                  "allowWatchBookmarks=true, sendInitialEvents=true, RV=useCurrentRV",
			allowWatchBookmarks:   true,
			sendInitialEvents:     &trueVal,
			useCurrentRV:          true,
			initialPods:           []*example.Pod{makePod("1"), makePod("2"), makePod("3")},
			expectedInitialEvents: addEventsFromCreatedPods,
			expectedInitialEventsBookmarkWithMinimalRV: initialEventsEndFromLastCreatedPod,
			podsAfterEstablishingWatch:                 []*example.Pod{makePod("4"), makePod("5")},
			expectedEventsAfterEstablishingWatch:       addEventsFromCreatedPods,
		},
		{
			name:                                 "allowWatchBookmarks=true, sendInitialEvents=false, RV=useCurrentRV",
			allowWatchBookmarks:                  true,
			sendInitialEvents:                    &falseVal,
			useCurrentRV:                         true,
			initialPods:                          []*example.Pod{makePod("1"), makePod("2"), makePod("3")},
			podsAfterEstablishingWatch:           []*example.Pod{makePod("4"), makePod("5")},
			expectedEventsAfterEstablishingWatch: addEventsFromCreatedPods,
		},
		{
			name:                                 "allowWatchBookmarks=false, sendInitialEvents=false, RV=useCurrentRV",
			sendInitialEvents:                    &falseVal,
			useCurrentRV:                         true,
			initialPods:                          []*example.Pod{makePod("1"), makePod("2"), makePod("3")},
			podsAfterEstablishingWatch:           []*example.Pod{makePod("4"), makePod("5")},
			expectedEventsAfterEstablishingWatch: addEventsFromCreatedPods,
		},
		{
			name:                                 "allowWatchBookmarks=false, sendInitialEvents=true, RV=useCurrentRV",
			sendInitialEvents:                    &trueVal,
			useCurrentRV:                         true,
			initialPods:                          []*example.Pod{makePod("1"), makePod("2"), makePod("3")},
			expectedInitialEvents:                addEventsFromCreatedPods,
			podsAfterEstablishingWatch:           []*example.Pod{makePod("4"), makePod("5")},
			expectedEventsAfterEstablishingWatch: addEventsFromCreatedPods,
		},

		{
			name:                                 "legacy, RV=0",
			resourceVersion:                      "0",
			initialPods:                          []*example.Pod{makePod("1"), makePod("2"), makePod("3")},
			expectedInitialEvents:                addEventsFromCreatedPods,
			podsAfterEstablishingWatch:           []*example.Pod{makePod("4"), makePod("5")},
			expectedEventsAfterEstablishingWatch: addEventsFromCreatedPods,
		},
		{
			name:                                 "legacy, RV=unset",
			initialPods:                          []*example.Pod{makePod("1"), makePod("2"), makePod("3")},
			expectedInitialEvents:                addEventsFromCreatedPods,
			podsAfterEstablishingWatch:           []*example.Pod{makePod("4"), makePod("5")},
			expectedEventsAfterEstablishingWatch: addEventsFromCreatedPods,
		},
	}
	for idx, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			t.Parallel()
			// set up env
			if scenario.expectedInitialEvents == nil {
				scenario.expectedInitialEvents = func(_ []*example.Pod) []watch.Event { return nil }
			}
			if scenario.expectedEventsAfterEstablishingWatch == nil {
				scenario.expectedEventsAfterEstablishingWatch = func(_ []*example.Pod) []watch.Event { return nil }
			}

			var createdPods []*example.Pod
			ns := fmt.Sprintf("ns-%v", idx)
			for _, obj := range scenario.initialPods {
				obj.Namespace = ns
				out := &example.Pod{}
				err := store.Create(ctx, computePodKey(obj), obj, out, 0)
				require.NoError(t, err, "failed to add a pod: %v", obj)
				createdPods = append(createdPods, out)
			}

			if len(createdPods) > 0 {
				// this list call ensures that the cache has seen the created pods.
				// this makes the watch request below deterministic.
				listObject := &example.PodList{}
				opts := storage.ListOptions{
					Predicate:            storage.Everything,
					Recursive:            true,
					ResourceVersion:      createdPods[len(createdPods)-1].ResourceVersion,
					ResourceVersionMatch: metav1.ResourceVersionMatchNotOlderThan,
				}
				err := store.GetList(ctx, fmt.Sprintf("/pods/%s", ns), opts, listObject)
				require.NoError(t, err)
				require.Len(t, listObject.Items, len(createdPods))
			}

			if scenario.useCurrentRV {
				currentStorageRV, err := storage.GetCurrentResourceVersionFromStorage(ctx, store, func() runtime.Object { return &example.PodList{} }, "/pods", "")
				require.NoError(t, err)
				scenario.resourceVersion = fmt.Sprintf("%d", currentStorageRV)
			}

			opts := storage.ListOptions{Predicate: storage.Everything, Recursive: true}
			opts.SendInitialEvents = scenario.sendInitialEvents
			opts.Predicate.AllowWatchBookmarks = scenario.allowWatchBookmarks
			if len(scenario.resourceVersion) > 0 {
				opts.ResourceVersion = scenario.resourceVersion
			}

			w, err := store.Watch(context.Background(), fmt.Sprintf("/pods/%s", ns), opts)
			require.NoError(t, err, "failed to create watch: %v")
			defer w.Stop()

			// make sure we only get initial events
			TestCheckResultsInStrictOrder(t, w, scenario.expectedInitialEvents(createdPods))

			// make sure that the actual bookmark has at least RV >= to the expected one
			if scenario.expectedInitialEventsBookmarkWithMinimalRV != nil {
				testCheckResultFunc(t, w, func(actualEvent watch.Event) {
					expectedBookmarkEventWithMinRV := scenario.expectedInitialEventsBookmarkWithMinimalRV(createdPods)
					expectedObj, err := meta.Accessor(expectedBookmarkEventWithMinRV.Object)
					require.NoError(t, err)
					expectedRV, err := storage.APIObjectVersioner{}.ObjectResourceVersion(expectedBookmarkEventWithMinRV.Object)
					require.NoError(t, err)

					actualObj, err := meta.Accessor(actualEvent.Object)
					require.NoError(t, err)
					actualRV, err := storage.APIObjectVersioner{}.ObjectResourceVersion(actualEvent.Object)
					require.NoError(t, err)

					require.GreaterOrEqual(t, actualRV, expectedRV)

					// once we know that the RV is at least >= the expected one
					// rewrite it so that we can compare the objs
					expectedObj.SetResourceVersion(actualObj.GetResourceVersion())
					expectNoDiff(t, "incorrect event", expectedBookmarkEventWithMinRV, actualEvent)
				})
			}

			createdPods = []*example.Pod{}
			// add a pod that is greater than the storage's RV when the watch was started
			for _, obj := range scenario.podsAfterEstablishingWatch {
				obj.Namespace = ns
				out := &example.Pod{}
				err = store.Create(ctx, computePodKey(obj), obj, out, 0)
				require.NoError(t, err, "failed to add a pod: %v")
				createdPods = append(createdPods, out)
			}
			ignoreEventsFn := func(event watch.Event) bool { return event.Type == watch.Bookmark }
			testCheckResultWithIgnoreFunc(t, w, scenario.expectedEventsAfterEstablishingWatch(createdPods), ignoreEventsFn)
			TestCheckNoMoreResultsWithIgnoreFunc(t, w, ignoreEventsFn)
		})
	}
}

// RunWatchSemanticInitialEventsExtended checks if the bookmark event
// marking the end of the list stream contains the global RV.
//
// note that this scenario differs from the one in RunWatchSemantics
// by adding the pod to a different ns to advance the global RV
func RunWatchSemanticInitialEventsExtended(ctx context.Context, t *testing.T, store storage.Interface) {
	trueVal := true
	expectedInitialEventsInStrictOrder := func(initialPods []*example.Pod, globalResourceVersion string) []watch.Event {
		watchEvents := []watch.Event{}
		for _, initialPod := range initialPods {
			watchEvents = append(watchEvents, watch.Event{Type: watch.Added, Object: initialPod})
		}
		watchEvents = append(watchEvents, watch.Event{Type: watch.Bookmark, Object: &example.Pod{
			ObjectMeta: metav1.ObjectMeta{
				ResourceVersion: globalResourceVersion,
				Annotations:     map[string]string{metav1.InitialEventsAnnotationKey: "true"},
			},
		}})
		return watchEvents
	}
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.WatchList, true)

	initialPods := []*example.Pod{}
	ns := "ns-foo"
	for _, initialPod := range []*example.Pod{makePod("1"), makePod("2"), makePod("3"), makePod("4"), makePod("5")} {
		initialPod.Namespace = ns
		out := &example.Pod{}
		err := store.Create(ctx, computePodKey(initialPod), initialPod, out, 0)
		require.NoError(t, err, "failed to add a pod: %v")
		initialPods = append(initialPods, out)
	}

	// add the pod to a different ns to advance the global RV
	pod := makePod("1")
	pod.Namespace = "other-ns-foo"
	otherNsPod := &example.Pod{}
	err := store.Create(ctx, computePodKey(pod), pod, otherNsPod, 0)
	require.NoError(t, err, "failed to add a pod: %v")

	opts := storage.ListOptions{Predicate: storage.Everything, Recursive: true}
	opts.SendInitialEvents = &trueVal
	opts.Predicate.AllowWatchBookmarks = true

	w, err := store.Watch(context.Background(), fmt.Sprintf("/pods/%s", ns), opts)
	require.NoError(t, err, "failed to create watch: %v")
	defer w.Stop()

	// make sure we only get initial events from the first ns
	// followed by the bookmark with the global RV
	TestCheckResultsInStrictOrder(t, w, expectedInitialEventsInStrictOrder(initialPods, otherNsPod.ResourceVersion))
	TestCheckNoMoreResultsWithIgnoreFunc(t, w, nil)
}

func RunWatchListMatchSingle(ctx context.Context, t *testing.T, store storage.Interface) {
	trueVal := true
	expectedInitialEventsInStrictOrder := func(initialPod *example.Pod, globalResourceVersion string) []watch.Event {
		watchEvents := []watch.Event{}
		watchEvents = append(watchEvents, watch.Event{Type: watch.Added, Object: initialPod})
		watchEvents = append(watchEvents, watch.Event{Type: watch.Bookmark, Object: &example.Pod{
			ObjectMeta: metav1.ObjectMeta{
				ResourceVersion: globalResourceVersion,
				Annotations:     map[string]string{metav1.InitialEventsAnnotationKey: "true"},
			},
		}})
		return watchEvents
	}
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.WatchList, true)

	// add the pod for which the field selector will be constructed
	ns := "ns-foo"
	expectedPod := &example.Pod{}
	initialPod := makePod("1")
	initialPod.Namespace = ns
	err := store.Create(ctx, computePodKey(initialPod), initialPod, expectedPod, 0)
	require.NoError(t, err, "failed to add a pod: %v")

	// add more pods that won't match the field selector
	lastAddedPod := &example.Pod{}
	for _, otherPod := range []*example.Pod{makePod("2"), makePod("3"), makePod("4"), makePod("5")} {
		otherPod.Namespace = ns
		err = store.Create(ctx, computePodKey(otherPod), otherPod, lastAddedPod, 0)
		require.NoError(t, err, "failed to add a pod: %v")
	}

	opts := storage.ListOptions{
		Predicate: storage.SelectionPredicate{
			Label: labels.Everything(),
			Field: fields.ParseSelectorOrDie("metadata.name=pod-1"),
			GetAttrs: func(obj runtime.Object) (labels.Set, fields.Set, error) {
				pod := obj.(*example.Pod)
				return nil, fields.Set{"metadata.name": pod.Name}, nil
			},
		},
		Recursive: true,
	}
	opts.SendInitialEvents = &trueVal
	opts.Predicate.AllowWatchBookmarks = true

	w, err := store.Watch(context.Background(), "/pods", opts)
	require.NoError(t, err, "failed to create watch: %v")
	defer w.Stop()

	// make sure we only get a single pod matching the field selector
	// followed by the bookmark with the global RV
	TestCheckResultsInStrictOrder(t, w, expectedInitialEventsInStrictOrder(expectedPod, lastAddedPod.ResourceVersion))
	TestCheckNoMoreResultsWithIgnoreFunc(t, w, nil)
}

func makePod(namePrefix string) *example.Pod {
	return &example.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: fmt.Sprintf("pod-%s", namePrefix),
		},
	}
}

type testWatchStruct struct {
	obj         *example.Pod
	expectEvent bool
	watchType   watch.EventType
}

func basePod(podName string) *example.Pod {
	return baseNamespacedPod(podName, "")
}

func basePodUpdated(podName string) *example.Pod {
	return baseNamespacedPodUpdated(podName, "")
}

func basePodAssigned(podName, nodeName string) *example.Pod {
	return baseNamespacedPodAssigned(podName, "", nodeName)
}

func baseNamespacedPod(podName, namespace string) *example.Pod {
	return &example.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: namespace},
	}
}

func baseNamespacedPodUpdated(podName, namespace string) *example.Pod {
	return &example.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: namespace},
		Status:     example.PodStatus{Phase: "Running"},
	}
}

func baseNamespacedPodAssigned(podName, namespace, nodeName string) *example.Pod {
	return &example.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: namespace},
		Spec:       example.PodSpec{NodeName: nodeName},
	}
}
