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

package etcd3

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	clientv3 "go.etcd.io/etcd/client/v3"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/apiserver/pkg/apis/example"
	"k8s.io/apiserver/pkg/features"
	"k8s.io/apiserver/pkg/storage"
	"k8s.io/apiserver/pkg/storage/etcd3/testserver"
	storagetesting "k8s.io/apiserver/pkg/storage/testing"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	"k8s.io/utils/ptr"
)

func TestWatch(t *testing.T) {
	ctx, store, _ := testSetup(t)
	storagetesting.RunTestWatch(ctx, t, store)
}

func TestClusterScopedWatch(t *testing.T) {
	ctx, store, _ := testSetup(t)
	storagetesting.RunTestClusterScopedWatch(ctx, t, store)
}

func TestNamespaceScopedWatch(t *testing.T) {
	ctx, store, _ := testSetup(t)
	storagetesting.RunTestNamespaceScopedWatch(ctx, t, store)
}

func TestDeleteTriggerWatch(t *testing.T) {
	ctx, store, _ := testSetup(t)
	storagetesting.RunTestDeleteTriggerWatch(ctx, t, store)
}

func TestWatchFromZero(t *testing.T) {
	ctx, store, client := testSetup(t)
	storagetesting.RunTestWatchFromZero(ctx, t, store, compactStorage(client.Client))
}

// TestWatchFromNonZero tests that
// - watch from non-0 should just watch changes after given version
func TestWatchFromNoneZero(t *testing.T) {
	ctx, store, _ := testSetup(t)
	storagetesting.RunTestWatchFromNonZero(ctx, t, store)
}

func TestDelayedWatchDelivery(t *testing.T) {
	ctx, store, _ := testSetup(t)
	storagetesting.RunTestDelayedWatchDelivery(ctx, t, store)
}

func TestWatchError(t *testing.T) {
	ctx, store, _ := testSetup(t)
	storagetesting.RunTestWatchError(ctx, t, &storeWithPrefixTransformer{store})
}

func TestWatchContextCancel(t *testing.T) {
	ctx, store, _ := testSetup(t)
	storagetesting.RunTestWatchContextCancel(ctx, t, store)
}

func TestWatcherTimeout(t *testing.T) {
	ctx, store, _ := testSetup(t)
	storagetesting.RunTestWatcherTimeout(ctx, t, store)
}

func TestWatchDeleteEventObjectHaveLatestRV(t *testing.T) {
	ctx, store, _ := testSetup(t)
	storagetesting.RunTestWatchDeleteEventObjectHaveLatestRV(ctx, t, store)
}

func TestWatchInitializationSignal(t *testing.T) {
	ctx, store, _ := testSetup(t)
	storagetesting.RunTestWatchInitializationSignal(ctx, t, store)
}

func TestProgressNotify(t *testing.T) {
	clusterConfig := testserver.NewTestConfig(t)
	clusterConfig.ExperimentalWatchProgressNotifyInterval = time.Second
	ctx, store, _ := testSetup(t, withClientConfig(clusterConfig))

	storagetesting.RunOptionalTestProgressNotify(ctx, t, store)
}

func TestWatchWithUnsafeDelete(t *testing.T) {
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.AllowUnsafeMalformedObjectDeletion, true)
	ctx, store, _ := testSetup(t)
	storagetesting.RunTestWatchWithUnsafeDelete(ctx, t, &storeWithCorruptedTransformer{Interface: store, store: store})
}

// TestWatchDispatchBookmarkEvents makes sure that
// setting allowWatchBookmarks query param against
// etcd implementation doesn't have any effect.
func TestWatchDispatchBookmarkEvents(t *testing.T) {
	clusterConfig := testserver.NewTestConfig(t)
	clusterConfig.ExperimentalWatchProgressNotifyInterval = time.Second
	ctx, store, _ := testSetup(t, withClientConfig(clusterConfig))

	storagetesting.RunTestWatchDispatchBookmarkEvents(ctx, t, store, false)
}

func TestSendInitialEventsBackwardCompatibility(t *testing.T) {
	ctx, store, _ := testSetup(t)
	storagetesting.RunSendInitialEventsBackwardCompatibility(ctx, t, store)
}

func TestEtcdWatchSemantics(t *testing.T) {
	ctx, store, _ := testSetup(t)
	storagetesting.RunWatchSemantics(ctx, t, store)
}

func TestEtcdWatchSemanticsWithConcurrentDecode(t *testing.T) {
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.ConcurrentWatchObjectDecode, true)
	ctx, store, _ := testSetup(t)
	storagetesting.RunWatchSemantics(ctx, t, store)
}

func TestEtcdWatchSemanticInitialEventsExtended(t *testing.T) {
	ctx, store, _ := testSetup(t)
	storagetesting.RunWatchSemanticInitialEventsExtended(ctx, t, store)
}

func TestWatchListMatchSingle(t *testing.T) {
	ctx, store, _ := testSetup(t)
	storagetesting.RunWatchListMatchSingle(ctx, t, store)
}

// =======================================================================
// Implementation-specific tests are following.
// The following tests are exercising the details of the implementation
// not the actual user-facing contract of storage interface.
// As such, they may focus e.g. on non-functional aspects like performance
// impact.
// =======================================================================

func TestWatchErrResultNotBlockAfterCancel(t *testing.T) {
	origCtx, store, _ := testSetup(t)
	ctx, cancel := context.WithCancel(origCtx)
	w := store.watcher.createWatchChan(ctx, "/abc", 0, false, false, storage.Everything)
	// make resultChan and errChan blocking to ensure ordering.
	w.resultChan = make(chan watch.Event)
	w.errChan = make(chan error)
	// The event flow goes like:
	// - first we send an error, it should block on resultChan.
	// - Then we cancel ctx. The blocking on resultChan should be freed up
	//   and run() goroutine should return.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		w.run(false, true)
		wg.Done()
	}()
	w.errChan <- fmt.Errorf("some error")
	cancel()
	wg.Wait()
}

// TestWatchErrorIncorrectConfiguration checks if an error
// will be returned when the storage hasn't been properly
// initialised for watch requests
func TestWatchErrorIncorrectConfiguration(t *testing.T) {
	scenarios := []struct {
		name            string
		setupFn         func(opts *setupOptions)
		requestOpts     storage.ListOptions
		enableWatchList bool
		expectedErr     error
	}{
		{
			name:        "no newFunc provided",
			setupFn:     func(opts *setupOptions) { opts.newFunc = nil },
			requestOpts: storage.ListOptions{ProgressNotify: true},
			expectedErr: apierrors.NewInternalError(errors.New("progressNotify for watch is unsupported by the etcd storage because no newFunc was provided")),
		},
	}
	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			if scenario.enableWatchList {
				featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.WatchList, true)
			}
			origCtx, store, _ := testSetup(t, scenario.setupFn)
			ctx, cancel := context.WithCancel(origCtx)
			defer cancel()

			w, err := store.watcher.Watch(ctx, "/abc", 0, scenario.requestOpts)
			if err == nil {
				t.Fatalf("expected an error but got none")
			}
			if w != nil {
				t.Fatalf("didn't expect a watcher because the test assumes incorrect store initialisation")
			}
			if err.Error() != scenario.expectedErr.Error() {
				t.Fatalf("unexpected err = %v, expected = %v", err, scenario.expectedErr)
			}
		})
	}
}

func TestTooLargeResourceVersionErrorForWatchList(t *testing.T) {
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.WatchList, true)
	origCtx, store, _ := testSetup(t)
	ctx, cancel := context.WithCancel(origCtx)
	defer cancel()
	requestOpts := storage.ListOptions{
		SendInitialEvents: ptr.To(true),
		Recursive:         true,
		Predicate: storage.SelectionPredicate{
			Field:               fields.Everything(),
			Label:               labels.Everything(),
			AllowWatchBookmarks: true,
		},
	}
	var expectedErr *apierrors.StatusError
	if !errors.As(storage.NewTooLargeResourceVersionError(uint64(102), 1, 0), &expectedErr) {
		t.Fatalf("Unable to convert NewTooLargeResourceVersionError to apierrors.StatusError")
	}

	w, err := store.watcher.Watch(ctx, "/abc", int64(102), requestOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Stop()

	actualEvent := <-w.ResultChan()
	if actualEvent.Type != watch.Error {
		t.Fatalf("Unexpected type of the event: %v, expected: %v", actualEvent.Type, watch.Error)
	}
	actualErr, ok := actualEvent.Object.(*metav1.Status)
	if !ok {
		t.Fatalf("Expected *apierrors.StatusError, got: %#v", actualEvent.Object)
	}

	if actualErr.Details.RetryAfterSeconds <= 0 {
		t.Fatalf("RetryAfterSeconds must be > 0, actual value: %v", actualErr.Details.RetryAfterSeconds)
	}
	// rewrite the Details as it contains retry seconds
	// and validate the whole struct
	expectedErr.ErrStatus.Details = actualErr.Details
	if diff := cmp.Diff(*actualErr, expectedErr.ErrStatus); diff != "" {
		t.Fatalf("Unexpected error returned, diff: %v", diff)
	}
}

func TestWatchChanSync(t *testing.T) {
	testCases := []struct {
		name             string
		watchKey         string
		watcherMaxLimit  int64
		expectEventCount int
		expectGetCount   int
	}{
		{
			name:            "None of the current objects match watchKey: sync with empty page",
			watchKey:        "/pods/test/",
			watcherMaxLimit: 1,
			expectGetCount:  1,
		},
		{
			name:             "The number of current objects is less than defaultWatcherMaxLimit: sync with one page",
			watchKey:         "/pods/",
			watcherMaxLimit:  3,
			expectEventCount: 2,
			expectGetCount:   1,
		},
		{
			name:             "a new item added to etcd before returning a second page is not returned: sync with two page",
			watchKey:         "/pods/",
			watcherMaxLimit:  1,
			expectEventCount: 2,
			expectGetCount:   2,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			defaultWatcherMaxLimit = testCase.watcherMaxLimit

			origCtx, store, _ := testSetup(t)
			initList, err := initStoreData(origCtx, store)
			if err != nil {
				t.Fatal(err)
			}

			kvWrapper := newEtcdClientKVWrapper(store.client.KV)
			kvWrapper.getReactors = append(kvWrapper.getReactors, func() {
				barThird := &example.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "third", Name: "bar"}}
				podKey := fmt.Sprintf("/pods/%s/%s", barThird.Namespace, barThird.Name)
				storedObj := &example.Pod{}

				err := store.Create(context.Background(), podKey, barThird, storedObj, 0)
				if err != nil {
					t.Errorf("failed to create object: %v", err)
				}
			})

			store.client.KV = kvWrapper

			w := store.watcher.createWatchChan(
				origCtx,
				testCase.watchKey,
				0,
				true,
				false,
				storage.Everything)

			err = w.sync()
			if err != nil {
				t.Fatal(err)
			}

			// close incomingEventChan so we can read incomingEventChan non-blocking
			close(w.incomingEventChan)

			eventsReceived := 0
			for event := range w.incomingEventChan {
				eventsReceived++
				storagetesting.ExpectContains(t, "incorrect list pods", initList, event.key)
			}

			if eventsReceived != testCase.expectEventCount {
				t.Errorf("Unexpected number of events: %v, expected: %v", eventsReceived, testCase.expectEventCount)
			}

			if kvWrapper.getCallCounter != testCase.expectGetCount {
				t.Errorf("Unexpected called times of client.KV.Get() : %v, expected: %v", kvWrapper.getCallCounter, testCase.expectGetCount)
			}
		})
	}
}

// NOTE: it's not thread-safe
type etcdClientKVWrapper struct {
	clientv3.KV
	// keeps track of the number of times Get method is called
	getCallCounter int
	// getReactors is called after the etcd KV's get function is executed.
	getReactors []func()
}

func newEtcdClientKVWrapper(kv clientv3.KV) *etcdClientKVWrapper {
	return &etcdClientKVWrapper{
		KV:             kv,
		getCallCounter: 0,
	}
}

func (ecw *etcdClientKVWrapper) Get(ctx context.Context, key string, opts ...clientv3.OpOption) (*clientv3.GetResponse, error) {
	resp, err := ecw.KV.Get(ctx, key, opts...)
	ecw.getCallCounter++
	if err != nil {
		return nil, err
	}

	if len(ecw.getReactors) > 0 {
		reactor := ecw.getReactors[0]
		ecw.getReactors = ecw.getReactors[1:]
		reactor()
	}

	return resp, nil
}

func initStoreData(ctx context.Context, store storage.Interface) ([]interface{}, error) {
	barFirst := &example.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "first", Name: "bar"}}
	barSecond := &example.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "second", Name: "bar"}}

	preset := []struct {
		key       string
		obj       *example.Pod
		storedObj *example.Pod
	}{
		{
			key: fmt.Sprintf("/pods/%s/%s", barFirst.Namespace, barFirst.Name),
			obj: barFirst,
		},
		{
			key: fmt.Sprintf("/pods/%s/%s", barSecond.Namespace, barSecond.Name),
			obj: barSecond,
		},
	}

	for i, ps := range preset {
		preset[i].storedObj = &example.Pod{}
		err := store.Create(ctx, ps.key, ps.obj, preset[i].storedObj, 0)
		if err != nil {
			return nil, fmt.Errorf("failed to create object: %w", err)
		}
	}

	var created []interface{}
	for _, item := range preset {
		created = append(created, item.key)
	}
	return created, nil
}

func TestTransformIfCorruptObjectError(t *testing.T) {
	tests := []struct {
		name  string
		event *event
		err   error
		// whether the above error should be wrapped into a corruptObjectDeletedError
		transform bool
	}{
		{
			name:      "Deleted, corrupt object error",
			event:     &event{isDeleted: true},
			err:       &corruptObjectError{err: fmt.Errorf("bits flipped"), errType: untransformable},
			transform: true,
		},
		{
			name:      "Deleted, not a corrupt object error",
			event:     &event{isDeleted: true},
			err:       fmt.Errorf("bits flipped"),
			transform: false,
		},
		{
			name:      "not Deleted, corrupt object error",
			event:     &event{isCreated: true},
			err:       &corruptObjectError{err: fmt.Errorf("bits flipped"), errType: untransformable},
			transform: false,
		},
		{
			name:      "not Deleted, not a corrupt object error",
			event:     &event{},
			err:       fmt.Errorf("bits flipped"),
			transform: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			w := watcher{}
			err := w.transformIfCorruptObjectError(test.event, test.err)
			switch test.transform {
			case true:
				var deletedErr *corruptObjectDeletedError
				if !errors.As(err, &deletedErr) {
					t.Fatalf("expected an error of type: %T, but got: %T", &corruptObjectDeletedError{}, err)
				}
				// nolint:errorlint // Unwrap should return the original
				// error as is, so the test is asserting with identity.
				if want, got := test.err, deletedErr.Unwrap(); got == nil || want != got {
					t.Errorf("expected the inner error to be identical of the given error, want=%p, but got: %p", want, got)
				}
			default:
				// nolint:errorlint // the function should return the
				// error as is, so the test is asserting with identity.
				if want, got := test.err, err; want != got {
					t.Errorf("expected the error returned to be identical to the given error, want=%p, but got: %p", want, got)
				}
			}
		})
	}
}
