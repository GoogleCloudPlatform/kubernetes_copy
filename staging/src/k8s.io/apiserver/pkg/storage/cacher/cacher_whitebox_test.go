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

package cacher

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"reflect"
	goruntime "runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/metadata"

	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/apiserver/pkg/apis/example"
	examplev1 "k8s.io/apiserver/pkg/apis/example/v1"
	"k8s.io/apiserver/pkg/features"
	"k8s.io/apiserver/pkg/storage"
	"k8s.io/apiserver/pkg/storage/cacher/metrics"
	etcd3testing "k8s.io/apiserver/pkg/storage/etcd3/testing"
	etcdfeature "k8s.io/apiserver/pkg/storage/feature"
	storagetesting "k8s.io/apiserver/pkg/storage/testing"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	k8smetrics "k8s.io/component-base/metrics"
	"k8s.io/component-base/metrics/testutil"
	"k8s.io/utils/clock"
	testingclock "k8s.io/utils/clock/testing"
	"k8s.io/utils/pointer"
)

func newTestCacherWithoutSyncing(s storage.Interface) (*Cacher, storage.Versioner, error) {
	prefix := "pods"
	config := Config{
		Storage:             s,
		Versioner:           storage.APIObjectVersioner{},
		GroupResource:       schema.GroupResource{Resource: "pods"},
		EventsHistoryWindow: DefaultEventFreshDuration,
		ResourcePrefix:      prefix,
		KeyFunc:             func(obj runtime.Object) (string, error) { return storage.NamespaceKeyFunc(prefix, obj) },
		GetAttrsFunc: func(obj runtime.Object) (labels.Set, fields.Set, error) {
			pod, ok := obj.(*example.Pod)
			if !ok {
				return storage.DefaultNamespaceScopedAttr(obj)
			}
			labelsSet, fieldsSet, err := storage.DefaultNamespaceScopedAttr(obj)
			if err != nil {
				return nil, nil, err
			}
			fieldsSet["spec.nodeName"] = pod.Spec.NodeName
			return labelsSet, fieldsSet, nil
		},
		NewFunc:     func() runtime.Object { return &example.Pod{} },
		NewListFunc: func() runtime.Object { return &example.PodList{} },
		Codec:       codecs.LegacyCodec(examplev1.SchemeGroupVersion),
		Clock:       clock.RealClock{},
	}
	cacher, err := NewCacherFromConfig(config)

	return cacher, storage.APIObjectVersioner{}, err
}

func newTestCacher(s storage.Interface) (*Cacher, storage.Versioner, error) {
	cacher, versioner, err := newTestCacherWithoutSyncing(s)
	if err != nil {
		return nil, versioner, err
	}

	if utilfeature.DefaultFeatureGate.Enabled(features.ResilientWatchCacheInitialization) {
		// The tests assume that Get/GetList/Watch calls shouldn't fail.
		// However, 429 error can now be returned if watchcache is under initialization.
		// To avoid rewriting all tests, we wait for watcache to initialize.
		if err := cacher.Wait(context.Background()); err != nil {
			return nil, storage.APIObjectVersioner{}, err
		}
	}
	return cacher, versioner, nil
}

type dummyStorage struct {
	sync.RWMutex
	err       error
	getListFn func(_ context.Context, _ string, _ storage.ListOptions, listObj runtime.Object) error
	watchFn   func(_ context.Context, _ string, _ storage.ListOptions) (watch.Interface, error)

	// use getRequestWatchProgressCounter when reading
	// the value of the counter
	requestWatchProgressCounter int
}

func (d *dummyStorage) RequestWatchProgress(ctx context.Context) error {
	d.Lock()
	defer d.Unlock()
	d.requestWatchProgressCounter++
	return nil
}

func (d *dummyStorage) getRequestWatchProgressCounter() int {
	d.RLock()
	defer d.RUnlock()
	return d.requestWatchProgressCounter
}

type dummyWatch struct {
	ch chan watch.Event
}

func (w *dummyWatch) ResultChan() <-chan watch.Event {
	return w.ch
}

func (w *dummyWatch) Stop() {
	close(w.ch)
}

func newDummyWatch() watch.Interface {
	return &dummyWatch{
		ch: make(chan watch.Event),
	}
}

func (d *dummyStorage) Versioner() storage.Versioner { return nil }
func (d *dummyStorage) Create(_ context.Context, _ string, _, _ runtime.Object, _ uint64) error {
	return fmt.Errorf("unimplemented")
}
func (d *dummyStorage) Delete(_ context.Context, _ string, _ runtime.Object, _ *storage.Preconditions, _ storage.ValidateObjectFunc, _ runtime.Object, _ storage.DeleteOptions) error {
	return fmt.Errorf("unimplemented")
}
func (d *dummyStorage) Watch(ctx context.Context, key string, opts storage.ListOptions) (watch.Interface, error) {
	if d.watchFn != nil {
		return d.watchFn(ctx, key, opts)
	}
	d.RLock()
	defer d.RUnlock()

	return newDummyWatch(), d.err
}
func (d *dummyStorage) Get(_ context.Context, _ string, _ storage.GetOptions, _ runtime.Object) error {
	d.RLock()
	defer d.RUnlock()

	return d.err
}
func (d *dummyStorage) GetList(ctx context.Context, resPrefix string, opts storage.ListOptions, listObj runtime.Object) error {
	if d.getListFn != nil {
		return d.getListFn(ctx, resPrefix, opts, listObj)
	}
	d.RLock()
	defer d.RUnlock()
	podList := listObj.(*example.PodList)
	podList.ListMeta = metav1.ListMeta{ResourceVersion: "100"}
	return d.err
}
func (d *dummyStorage) GuaranteedUpdate(_ context.Context, _ string, _ runtime.Object, _ bool, _ *storage.Preconditions, _ storage.UpdateFunc, _ runtime.Object) error {
	return fmt.Errorf("unimplemented")
}
func (d *dummyStorage) Count(_ string) (int64, error) {
	return 0, fmt.Errorf("unimplemented")
}
func (d *dummyStorage) ReadinessCheck() error {
	return nil
}
func (d *dummyStorage) injectError(err error) {
	d.Lock()
	defer d.Unlock()

	d.err = err
}

func TestGetListCacheBypass(t *testing.T) {
	type testCase struct {
		opts         storage.ListOptions
		expectBypass bool
	}
	commonTestCases := []testCase{
		{opts: storage.ListOptions{ResourceVersion: "0"}, expectBypass: false},
		{opts: storage.ListOptions{ResourceVersion: "1"}, expectBypass: false},

		{opts: storage.ListOptions{ResourceVersion: "", Predicate: storage.SelectionPredicate{Continue: "a"}}, expectBypass: true},
		{opts: storage.ListOptions{ResourceVersion: "0", Predicate: storage.SelectionPredicate{Continue: "a"}}, expectBypass: true},
		{opts: storage.ListOptions{ResourceVersion: "1", Predicate: storage.SelectionPredicate{Continue: "a"}}, expectBypass: true},

		{opts: storage.ListOptions{ResourceVersion: "0", Predicate: storage.SelectionPredicate{Limit: 500}}, expectBypass: false},
		{opts: storage.ListOptions{ResourceVersion: "1", Predicate: storage.SelectionPredicate{Limit: 500}}, expectBypass: true},

		{opts: storage.ListOptions{ResourceVersion: "", ResourceVersionMatch: metav1.ResourceVersionMatchExact}, expectBypass: true},
		{opts: storage.ListOptions{ResourceVersion: "0", ResourceVersionMatch: metav1.ResourceVersionMatchExact}, expectBypass: true},
		{opts: storage.ListOptions{ResourceVersion: "1", ResourceVersionMatch: metav1.ResourceVersionMatchExact}, expectBypass: true},
	}

	t.Run("ConsistentListFromStorage", func(t *testing.T) {
		featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.ConsistentListFromCache, false)
		testCases := append(commonTestCases,
			testCase{opts: storage.ListOptions{ResourceVersion: ""}, expectBypass: true},
			testCase{opts: storage.ListOptions{ResourceVersion: "", Predicate: storage.SelectionPredicate{Limit: 500}}, expectBypass: true},
		)
		for _, tc := range testCases {
			testGetListCacheBypass(t, tc.opts, tc.expectBypass)
		}

	})
	t.Run("ConsistentListFromCache", func(t *testing.T) {
		featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.ConsistentListFromCache, true)

		// TODO(p0lyn0mial): the following tests assume that etcdfeature.DefaultFeatureSupportChecker.Supports(storage.RequestWatchProgress)
		// evaluates to true. Otherwise the cache will be bypassed and the test will fail.
		//
		// If you were to run only TestGetListCacheBypass you would see that the test fail.
		// However in CI all test are run and there must be a test(s) that properly
		// initialize the storage layer so that the mentioned method evaluates to true
		forceRequestWatchProgressSupport(t)

		testCases := append(commonTestCases,
			testCase{opts: storage.ListOptions{ResourceVersion: ""}, expectBypass: false},
			testCase{opts: storage.ListOptions{ResourceVersion: "", Predicate: storage.SelectionPredicate{Limit: 500}}, expectBypass: false},
		)
		for _, tc := range testCases {
			testGetListCacheBypass(t, tc.opts, tc.expectBypass)
		}
	})
}

func testGetListCacheBypass(t *testing.T, options storage.ListOptions, expectBypass bool) {
	backingStorage := &dummyStorage{}
	cacher, _, err := newTestCacher(backingStorage)
	if err != nil {
		t.Fatalf("Couldn't create cacher: %v", err)
	}
	defer cacher.Stop()

	result := &example.PodList{}

	if !utilfeature.DefaultFeatureGate.Enabled(features.ResilientWatchCacheInitialization) {
		if err := cacher.ready.wait(context.Background()); err != nil {
			t.Fatalf("unexpected error waiting for the cache to be ready")
		}
	}

	// Inject error to underlying layer and check if cacher is not bypassed.
	backingStorage.getListFn = func(_ context.Context, key string, opts storage.ListOptions, listObj runtime.Object) error {
		currentResourceVersion := "42"
		switch {
		// request made by getCurrentResourceVersionFromStorage by checking Limit
		case key == cacher.resourcePrefix:
			podList := listObj.(*example.PodList)
			podList.ResourceVersion = currentResourceVersion
			return nil
		// request made by storage.GetList with revision from original request or
		// returned by getCurrentResourceVersionFromStorage
		case opts.ResourceVersion == options.ResourceVersion || opts.ResourceVersion == currentResourceVersion:
			return errDummy
		default:
			t.Fatalf("Unexpected request %+v", opts)
			return nil
		}
	}
	err = cacher.GetList(context.TODO(), "pods/ns", options, result)
	if err != nil && err != errDummy {
		t.Fatalf("Unexpected error for List request with options: %v, err: %v", options, err)
	}
	gotBypass := err == errDummy
	if gotBypass != expectBypass {
		t.Errorf("Unexpected bypass result for List request with options %+v, bypass expected: %v, got: %v", options, expectBypass, gotBypass)
	}
}

func TestConsistentReadFallback(t *testing.T) {
	tcs := []struct {
		name                   string
		consistentReadsEnabled bool
		watchCacheRV           string
		storageRV              string
		fallbackError          bool

		expectError             bool
		expectRV                string
		expectBlock             bool
		expectRequestsToStorage int
		expectMetric            string
	}{
		{
			name:                    "Success",
			consistentReadsEnabled:  true,
			watchCacheRV:            "42",
			storageRV:               "42",
			expectRV:                "42",
			expectRequestsToStorage: 1,
			expectMetric: `
# HELP apiserver_watch_cache_consistent_read_total [ALPHA] Counter for consistent reads from cache.
# TYPE apiserver_watch_cache_consistent_read_total counter
apiserver_watch_cache_consistent_read_total{fallback="false", resource="pods", success="true"} 1
`,
		},
		{
			name:                    "Fallback",
			consistentReadsEnabled:  true,
			watchCacheRV:            "2",
			storageRV:               "42",
			expectRV:                "42",
			expectBlock:             true,
			expectRequestsToStorage: 2,
			expectMetric: `
# HELP apiserver_watch_cache_consistent_read_total [ALPHA] Counter for consistent reads from cache.
# TYPE apiserver_watch_cache_consistent_read_total counter
apiserver_watch_cache_consistent_read_total{fallback="true", resource="pods", success="true"} 1
`,
		},
		{
			name:                    "Fallback Failure",
			consistentReadsEnabled:  true,
			watchCacheRV:            "2",
			storageRV:               "42",
			fallbackError:           true,
			expectError:             true,
			expectBlock:             true,
			expectRequestsToStorage: 2,
			expectMetric: `
# HELP apiserver_watch_cache_consistent_read_total [ALPHA] Counter for consistent reads from cache.
# TYPE apiserver_watch_cache_consistent_read_total counter
apiserver_watch_cache_consistent_read_total{fallback="true", resource="pods", success="false"} 1
`,
		},
		{
			name:                    "Disabled",
			watchCacheRV:            "2",
			storageRV:               "42",
			expectRV:                "42",
			expectRequestsToStorage: 1,
			expectMetric:            ``,
		},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.ConsistentListFromCache, tc.consistentReadsEnabled)
			if tc.consistentReadsEnabled {
				forceRequestWatchProgressSupport(t)
			}

			registry := k8smetrics.NewKubeRegistry()
			metrics.ConsistentReadTotal.Reset()
			if err := registry.Register(metrics.ConsistentReadTotal); err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			backingStorage := &dummyStorage{}
			backingStorage.getListFn = func(_ context.Context, key string, opts storage.ListOptions, listObj runtime.Object) error {
				podList := listObj.(*example.PodList)
				podList.ResourceVersion = tc.watchCacheRV
				return nil
			}
			// TODO: Use fake clock for this test to reduce execution time.
			cacher, _, err := newTestCacher(backingStorage)
			if err != nil {
				t.Fatalf("Couldn't create cacher: %v", err)
			}
			defer cacher.Stop()
			if !utilfeature.DefaultFeatureGate.Enabled(features.ResilientWatchCacheInitialization) {
				if err := cacher.ready.wait(context.Background()); err != nil {
					t.Fatalf("unexpected error waiting for the cache to be ready")
				}
			}

			if fmt.Sprintf("%d", cacher.watchCache.resourceVersion) != tc.watchCacheRV {
				t.Fatalf("Expected watch cache RV to equal watchCacheRV, got: %d, want: %s", cacher.watchCache.resourceVersion, tc.watchCacheRV)
			}
			requestToStorageCount := 0
			backingStorage.getListFn = func(_ context.Context, key string, opts storage.ListOptions, listObj runtime.Object) error {
				requestToStorageCount += 1
				podList := listObj.(*example.PodList)
				if key == cacher.resourcePrefix {
					podList.ResourceVersion = tc.storageRV
					return nil
				}
				if tc.fallbackError {
					return errDummy
				}
				podList.ResourceVersion = tc.storageRV
				return nil
			}
			result := &example.PodList{}
			start := cacher.clock.Now()
			err = cacher.GetList(context.TODO(), "pods/ns", storage.ListOptions{ResourceVersion: ""}, result)
			duration := cacher.clock.Since(start)
			if (err != nil) != tc.expectError {
				t.Fatalf("Unexpected error err: %v", err)
			}
			if result.ResourceVersion != tc.expectRV {
				t.Fatalf("Unexpected List response RV, got: %q, want: %q", result.ResourceVersion, tc.expectRV)
			}
			if requestToStorageCount != tc.expectRequestsToStorage {
				t.Fatalf("Unexpected number of requests to storage, got: %d, want: %d", requestToStorageCount, tc.expectRequestsToStorage)
			}
			blocked := duration >= blockTimeout
			if blocked != tc.expectBlock {
				t.Fatalf("Unexpected block, got: %v, want: %v", blocked, tc.expectBlock)
			}

			if err := testutil.GatherAndCompare(registry, strings.NewReader(tc.expectMetric), "apiserver_watch_cache_consistent_read_total"); err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

func TestGetListNonRecursiveCacheBypass(t *testing.T) {
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.ConsistentListFromCache, false)
	backingStorage := &dummyStorage{}
	cacher, _, err := newTestCacher(backingStorage)
	if err != nil {
		t.Fatalf("Couldn't create cacher: %v", err)
	}
	defer cacher.Stop()

	pred := storage.SelectionPredicate{
		Limit: 500,
	}
	result := &example.PodList{}

	if !utilfeature.DefaultFeatureGate.Enabled(features.ResilientWatchCacheInitialization) {
		if err := cacher.ready.wait(context.Background()); err != nil {
			t.Fatalf("unexpected error waiting for the cache to be ready")
		}
	}

	// Inject error to underlying layer and check if cacher is not bypassed.
	backingStorage.injectError(errDummy)
	err = cacher.GetList(context.TODO(), "pods/ns", storage.ListOptions{
		ResourceVersion: "0",
		Predicate:       pred,
	}, result)
	if err != nil {
		t.Errorf("GetList with Limit and RV=0 should be served from cache: %v", err)
	}

	err = cacher.GetList(context.TODO(), "pods/ns", storage.ListOptions{
		ResourceVersion: "",
		Predicate:       pred,
	}, result)
	if err != errDummy {
		t.Errorf("GetList with Limit without RV=0 should bypass cacher: %v", err)
	}
}

func TestGetCacheBypass(t *testing.T) {
	backingStorage := &dummyStorage{}
	cacher, _, err := newTestCacher(backingStorage)
	if err != nil {
		t.Fatalf("Couldn't create cacher: %v", err)
	}
	defer cacher.Stop()

	result := &example.Pod{}

	if !utilfeature.DefaultFeatureGate.Enabled(features.ResilientWatchCacheInitialization) {
		if err := cacher.ready.wait(context.Background()); err != nil {
			t.Fatalf("unexpected error waiting for the cache to be ready")
		}
	}

	// Inject error to underlying layer and check if cacher is not bypassed.
	backingStorage.injectError(errDummy)
	err = cacher.Get(context.TODO(), "pods/ns/pod-0", storage.GetOptions{
		IgnoreNotFound:  true,
		ResourceVersion: "0",
	}, result)
	if err != nil {
		t.Errorf("Get with RV=0 should be served from cache: %v", err)
	}

	err = cacher.Get(context.TODO(), "pods/ns/pod-0", storage.GetOptions{
		IgnoreNotFound:  true,
		ResourceVersion: "",
	}, result)
	if err != errDummy {
		t.Errorf("Get without RV=0 should bypass cacher: %v", err)
	}
}

func TestWatchCacheBypass(t *testing.T) {
	backingStorage := &dummyStorage{}
	cacher, _, err := newTestCacher(backingStorage)
	if err != nil {
		t.Fatalf("Couldn't create cacher: %v", err)
	}
	defer cacher.Stop()

	if !utilfeature.DefaultFeatureGate.Enabled(features.ResilientWatchCacheInitialization) {
		if err := cacher.ready.wait(context.Background()); err != nil {
			t.Fatalf("unexpected error waiting for the cache to be ready")
		}
	}

	_, err = cacher.Watch(context.TODO(), "pod/ns", storage.ListOptions{
		ResourceVersion: "0",
		Predicate:       storage.Everything,
	})
	if err != nil {
		t.Errorf("Watch with RV=0 should be served from cache: %v", err)
	}

	_, err = cacher.Watch(context.TODO(), "pod/ns", storage.ListOptions{
		ResourceVersion: "",
		Predicate:       storage.Everything,
	})
	if err != nil {
		t.Errorf("Watch with RV=0 should be served from cache: %v", err)
	}

	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.WatchFromStorageWithoutResourceVersion, false)
	_, err = cacher.Watch(context.TODO(), "pod/ns", storage.ListOptions{
		ResourceVersion: "",
		Predicate:       storage.Everything,
	})
	if err != nil {
		t.Errorf("With WatchFromStorageWithoutResourceVersion disabled, watch with unset RV should be served from cache: %v", err)
	}

	// Inject error to underlying layer and check if cacher is not bypassed.
	backingStorage.injectError(errDummy)
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.WatchFromStorageWithoutResourceVersion, true)
	_, err = cacher.Watch(context.TODO(), "pod/ns", storage.ListOptions{
		ResourceVersion: "",
		Predicate:       storage.Everything,
	})
	if !errors.Is(err, errDummy) {
		t.Errorf("With WatchFromStorageWithoutResourceVersion enabled, watch with unset RV should be served from storage: %v", err)
	}
}

func TestTooManyRequestsNotReturned(t *testing.T) {
	// Ensure that with ResilientWatchCacheInitialization feature disabled, we don't return 429
	// errors when watchcache is not initialized.
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.ResilientWatchCacheInitialization, false)

	dummyErr := fmt.Errorf("dummy")
	backingStorage := &dummyStorage{err: dummyErr}
	cacher, _, err := newTestCacherWithoutSyncing(backingStorage)
	if err != nil {
		t.Fatalf("Couldn't create cacher: %v", err)
	}
	defer cacher.Stop()

	opts := storage.ListOptions{
		ResourceVersion: "0",
		Predicate:       storage.Everything,
	}

	// Cancel the request so that it doesn't hang forever.
	listCtx, listCancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer listCancel()

	result := &example.PodList{}
	err = cacher.GetList(listCtx, "/pods/ns", opts, result)
	if err != nil && apierrors.IsTooManyRequests(err) {
		t.Errorf("Unexpected 429 error without ResilientWatchCacheInitialization feature for List")
	}

	watchCtx, watchCancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer watchCancel()

	_, err = cacher.Watch(watchCtx, "/pods/ns", opts)
	if err != nil && apierrors.IsTooManyRequests(err) {
		t.Errorf("Unexpected 429 error without ResilientWatchCacheInitialization feature for Watch")
	}
}

func TestEmptyWatchEventCache(t *testing.T) {
	server, etcdStorage := newEtcdTestStorage(t, etcd3testing.PathPrefix())
	defer server.Terminate(t)

	// add a few objects
	v := storage.APIObjectVersioner{}
	lastRV := uint64(0)
	for i := 0; i < 5; i++ {
		pod := &example.Pod{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("foo-%d", i), Namespace: "test-ns"}}
		out := &example.Pod{}
		key := computePodKey(pod)
		if err := etcdStorage.Create(context.Background(), key, pod, out, 0); err != nil {
			t.Fatalf("Create failed: %v", err)
		}
		var err error
		if lastRV, err = v.ParseResourceVersion(out.ResourceVersion); err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}
	}

	cacher, _, err := newTestCacher(etcdStorage)
	if err != nil {
		t.Fatalf("Couldn't create cacher: %v", err)
	}
	defer cacher.Stop()

	// Given that cacher is always initialized from the "current" version of etcd,
	// we now have a cacher with an empty cache of watch events and a resourceVersion of rv.
	// It should support establishing watches from rv and higher, but not older.

	expectedResourceExpiredError := apierrors.NewResourceExpired("").ErrStatus
	tests := []struct {
		name            string
		resourceVersion uint64
		expectedEvent   *watch.Event
	}{
		{
			name:            "RV-1",
			resourceVersion: lastRV - 1,
			expectedEvent:   &watch.Event{Type: watch.Error, Object: &expectedResourceExpiredError},
		},
		{
			name:            "RV",
			resourceVersion: lastRV,
		},
		{
			name:            "RV+1",
			resourceVersion: lastRV + 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := storage.ListOptions{
				ResourceVersion: strconv.Itoa(int(tt.resourceVersion)),
				Predicate:       storage.Everything,
			}
			watcher, err := cacher.Watch(context.Background(), "/pods/test-ns", opts)
			if err != nil {
				t.Fatalf("Failed to create watch: %v", err)
			}
			defer watcher.Stop()
			select {
			case event := <-watcher.ResultChan():
				if tt.expectedEvent == nil {
					t.Errorf("Unexpected event: type=%#v, object=%#v", event.Type, event.Object)
					break
				}
				if e, a := tt.expectedEvent.Type, event.Type; e != a {
					t.Errorf("Expected: %s, got: %s", e, a)
				}
				if e, a := tt.expectedEvent.Object, event.Object; !apiequality.Semantic.DeepDerivative(e, a) {
					t.Errorf("Expected: %#v, got: %#v", e, a)
				}
			case <-time.After(1 * time.Second):
				// the watch was established otherwise
				// we would be blocking on cache.Watch(...)
				// in addition to that, the tests are serial in nature,
				// meaning that there is no other actor
				// that could add items to the database,
				// which could result in receiving new items.
				// given that waiting 1s seems okay
				if tt.expectedEvent != nil {
					t.Errorf("Failed to get an event")
				}
				// watch remained established successfully
			}
		})
	}
}

func TestWatchNotHangingOnStartupFailure(t *testing.T) {
	// Configure cacher so that it can't initialize, because of
	// constantly failing lists to the underlying storage.
	dummyErr := fmt.Errorf("dummy")
	backingStorage := &dummyStorage{err: dummyErr}
	cacher, _, err := newTestCacherWithoutSyncing(backingStorage)
	if err != nil {
		t.Fatalf("Couldn't create cacher: %v", err)
	}
	defer cacher.Stop()

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel the watch after some time to check if it will properly
	// terminate instead of hanging forever.
	go func() {
		defer cancel()
		cacher.clock.Sleep(1 * time.Second)
	}()

	// Watch hangs waiting on watchcache being initialized.
	// Ensure that it terminates when its context is cancelled
	// (e.g. the request is terminated for whatever reason).
	_, err = cacher.Watch(ctx, "pods/ns", storage.ListOptions{ResourceVersion: "0"})
	if !utilfeature.DefaultFeatureGate.Enabled(features.ResilientWatchCacheInitialization) {
		if err == nil || err.Error() != apierrors.NewServiceUnavailable(context.Canceled.Error()).Error() {
			t.Errorf("Unexpected error: %#v", err)
		}
	} else {
		if err == nil || err.Error() != apierrors.NewTooManyRequests("storage is (re)initializing", 1).Error() {
			t.Errorf("Unexpected error: %#v", err)
		}
	}
}

func TestWatcherNotGoingBackInTime(t *testing.T) {
	backingStorage := &dummyStorage{}
	cacher, v, err := newTestCacher(backingStorage)
	if err != nil {
		t.Fatalf("Couldn't create cacher: %v", err)
	}
	defer cacher.Stop()

	if !utilfeature.DefaultFeatureGate.Enabled(features.ResilientWatchCacheInitialization) {
		if err := cacher.ready.wait(context.Background()); err != nil {
			t.Fatalf("unexpected error waiting for the cache to be ready")
		}
	}

	// Ensure there is some budget for slowing down processing.
	cacher.dispatchTimeoutBudget.returnUnused(100 * time.Millisecond)

	makePod := func(i int) *examplev1.Pod {
		return &examplev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:            fmt.Sprintf("pod-%d", 1000+i),
				Namespace:       "ns",
				ResourceVersion: fmt.Sprintf("%d", 1000+i),
			},
		}
	}
	if err := cacher.watchCache.Add(makePod(0)); err != nil {
		t.Errorf("error: %v", err)
	}

	totalPods := 100

	// Create watcher that will be slowing down reading.
	w1, err := cacher.Watch(context.TODO(), "pods/ns", storage.ListOptions{
		ResourceVersion: "999",
		Predicate:       storage.Everything,
	})
	if err != nil {
		t.Fatalf("Failed to create watch: %v", err)
	}
	defer w1.Stop()
	go func() {
		a := 0
		for range w1.ResultChan() {
			time.Sleep(time.Millisecond)
			a++
			if a == 100 {
				break
			}
		}
	}()

	// Now push a ton of object to cache.
	for i := 1; i < totalPods; i++ {
		cacher.watchCache.Add(makePod(i))
	}

	// Create fast watcher and ensure it will get each object exactly once.
	w2, err := cacher.Watch(context.TODO(), "pods/ns", storage.ListOptions{ResourceVersion: "999", Predicate: storage.Everything})
	if err != nil {
		t.Fatalf("Failed to create watch: %v", err)
	}
	defer w2.Stop()

	shouldContinue := true
	currentRV := uint64(0)
	for shouldContinue {
		select {
		case event, ok := <-w2.ResultChan():
			if !ok {
				shouldContinue = false
				break
			}
			rv, err := v.ParseResourceVersion(event.Object.(metaRuntimeInterface).GetResourceVersion())
			if err != nil {
				t.Errorf("unexpected parsing error: %v", err)
			} else {
				if rv < currentRV {
					t.Errorf("watcher going back in time")
				}
				currentRV = rv
			}
		case <-time.After(time.Second):
			w2.Stop()
		}
	}
}

func TestCacherDontAcceptRequestsStopped(t *testing.T) {
	backingStorage := &dummyStorage{}
	cacher, _, err := newTestCacher(backingStorage)
	if err != nil {
		t.Fatalf("Couldn't create cacher: %v", err)
	}

	if !utilfeature.DefaultFeatureGate.Enabled(features.ResilientWatchCacheInitialization) {
		if err := cacher.ready.wait(context.Background()); err != nil {
			t.Fatalf("unexpected error waiting for the cache to be ready")
		}
	}

	w, err := cacher.Watch(context.Background(), "pods/ns", storage.ListOptions{ResourceVersion: "0", Predicate: storage.Everything})
	if err != nil {
		t.Fatalf("Failed to create watch: %v", err)
	}

	watchClosed := make(chan struct{})
	go func() {
		defer close(watchClosed)
		for event := range w.ResultChan() {
			switch event.Type {
			case watch.Added, watch.Modified, watch.Deleted:
				// ok
			default:
				t.Errorf("unexpected event %#v", event)
			}
		}
	}()

	cacher.Stop()

	_, err = cacher.Watch(context.Background(), "pods/ns", storage.ListOptions{ResourceVersion: "0", Predicate: storage.Everything})
	if err == nil {
		t.Fatalf("Success to create Watch: %v", err)
	}

	result := &example.Pod{}
	err = cacher.Get(context.TODO(), "pods/ns/pod-0", storage.GetOptions{
		IgnoreNotFound:  true,
		ResourceVersion: "1",
	}, result)
	if !utilfeature.DefaultFeatureGate.Enabled(features.ResilientWatchCacheInitialization) {
		if err == nil {
			t.Fatalf("Success to create Get: %v", err)
		}
	} else {
		if err != nil {
			t.Fatalf("Failed to get object: %v:", err)
		}
	}

	listResult := &example.PodList{}
	err = cacher.GetList(context.TODO(), "pods/ns", storage.ListOptions{
		ResourceVersion: "1",
		Recursive:       true,
		Predicate: storage.SelectionPredicate{
			Limit: 500,
		},
	}, listResult)
	if !utilfeature.DefaultFeatureGate.Enabled(features.ResilientWatchCacheInitialization) {
		if err != nil {
			t.Fatalf("Failed to create GetList: %v", err)
		}
	} else {
		if err != nil {
			t.Fatalf("Failed to list objects: %v", err)
		}
	}

	select {
	case <-watchClosed:
	case <-time.After(wait.ForeverTestTimeout):
		t.Errorf("timed out waiting for watch to close")
	}
}

func TestCacherDontMissEventsOnReinitialization(t *testing.T) {
	makePod := func(i int) *example.Pod {
		return &example.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:            fmt.Sprintf("pod-%d", i),
				Namespace:       "ns",
				ResourceVersion: fmt.Sprintf("%d", i),
			},
		}
	}

	listCalls, watchCalls := 0, 0
	backingStorage := &dummyStorage{
		getListFn: func(_ context.Context, _ string, _ storage.ListOptions, listObj runtime.Object) error {
			podList := listObj.(*example.PodList)
			var err error
			switch listCalls {
			case 0:
				podList.ListMeta = metav1.ListMeta{ResourceVersion: "1"}
			case 1:
				podList.ListMeta = metav1.ListMeta{ResourceVersion: "10"}
			default:
				t.Errorf("unexpected list call: %d", listCalls)
				err = fmt.Errorf("unexpected list call")
			}
			listCalls++
			return err
		},
		watchFn: func(_ context.Context, _ string, _ storage.ListOptions) (watch.Interface, error) {
			var w *watch.FakeWatcher
			var err error
			switch watchCalls {
			case 0:
				w = watch.NewFakeWithChanSize(10, false)
				for i := 2; i < 8; i++ {
					w.Add(makePod(i))
				}
				// Emit an error to force relisting.
				w.Error(nil)
				w.Stop()
			case 1:
				w = watch.NewFakeWithChanSize(10, false)
				for i := 12; i < 18; i++ {
					w.Add(makePod(i))
				}
				// Keep the watch open to avoid another reinitialization,
				// but register it for cleanup.
				t.Cleanup(func() { w.Stop() })
			default:
				t.Errorf("unexpected watch call: %d", watchCalls)
				err = fmt.Errorf("unexpected watch call")
			}
			watchCalls++
			return w, err
		},
	}
	cacher, _, err := newTestCacher(backingStorage)
	if err != nil {
		t.Fatalf("Couldn't create cacher: %v", err)
	}
	defer cacher.Stop()

	concurrency := 1000
	wg := sync.WaitGroup{}
	wg.Add(concurrency)

	// Ensure that test doesn't deadlock if cacher already processed everything
	// and get back into Pending state before some watches get called.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	for i := 0; i < concurrency; i++ {
		go func() {
			defer wg.Done()
			w, err := cacher.Watch(ctx, "pods", storage.ListOptions{ResourceVersion: "1", Predicate: storage.Everything})
			if err != nil {
				// Watch failed to initialize (this most probably means that cacher
				// already moved back to Pending state before watch initialized.
				// Ignore this case.
				return
			}
			defer w.Stop()

			prevRV := -1
			for event := range w.ResultChan() {
				if event.Type == watch.Error {
					break
				}
				object := event.Object
				if co, ok := object.(runtime.CacheableObject); ok {
					object = co.GetObject()
				}
				rv, err := strconv.Atoi(object.(*example.Pod).ResourceVersion)
				if err != nil {
					t.Errorf("incorrect resource version: %v", err)
					return
				}
				if prevRV != -1 && prevRV+1 != rv {
					t.Errorf("unexpected event received, prevRV=%d, rv=%d", prevRV, rv)
					return
				}
				prevRV = rv
			}

		}()
	}
	wg.Wait()
}

func TestCacherNoLeakWithMultipleWatchers(t *testing.T) {
	backingStorage := &dummyStorage{}
	cacher, _, err := newTestCacher(backingStorage)
	if err != nil {
		t.Fatalf("Couldn't create cacher: %v", err)
	}
	defer cacher.Stop()

	if !utilfeature.DefaultFeatureGate.Enabled(features.ResilientWatchCacheInitialization) {
		if err := cacher.ready.wait(context.Background()); err != nil {
			t.Fatalf("unexpected error waiting for the cache to be ready")
		}
	}

	pred := storage.Everything
	pred.AllowWatchBookmarks = true

	// run the collision test for 3 seconds to let ~2 buckets expire
	stopCh := make(chan struct{})
	var watchErr error
	time.AfterFunc(3*time.Second, func() { close(stopCh) })

	wg := &sync.WaitGroup{}

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stopCh:
				return
			default:
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				w, err := cacher.Watch(ctx, "pods/ns", storage.ListOptions{ResourceVersion: "0", Predicate: pred})
				if err != nil {
					watchErr = fmt.Errorf("Failed to create watch: %v", err)
					return
				}
				w.Stop()
			}
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stopCh:
				return
			default:
				cacher.Lock()
				cacher.bookmarkWatchers.popExpiredWatchersThreadUnsafe()
				cacher.Unlock()
			}
		}
	}()

	// wait for adding/removing watchers to end
	wg.Wait()

	if watchErr != nil {
		t.Fatal(watchErr)
	}

	// wait out the expiration period and pop expired watchers
	time.Sleep(2 * time.Second)
	cacher.Lock()
	defer cacher.Unlock()
	cacher.bookmarkWatchers.popExpiredWatchersThreadUnsafe()
	if len(cacher.bookmarkWatchers.watchersBuckets) != 0 {
		numWatchers := 0
		for bucketID, v := range cacher.bookmarkWatchers.watchersBuckets {
			numWatchers += len(v)
			t.Errorf("there are %v watchers at bucket Id %v with start Id %v", len(v), bucketID, cacher.bookmarkWatchers.startBucketID)
		}
		t.Errorf("unexpected bookmark watchers %v", numWatchers)
	}
}

func testCacherSendBookmarkEvents(t *testing.T, allowWatchBookmarks, expectedBookmarks bool) {
	backingStorage := &dummyStorage{}
	cacher, _, err := newTestCacher(backingStorage)
	if err != nil {
		t.Fatalf("Couldn't create cacher: %v", err)
	}
	defer cacher.Stop()

	if !utilfeature.DefaultFeatureGate.Enabled(features.ResilientWatchCacheInitialization) {
		if err := cacher.ready.wait(context.Background()); err != nil {
			t.Fatalf("unexpected error waiting for the cache to be ready")
		}
	}
	pred := storage.Everything
	pred.AllowWatchBookmarks = allowWatchBookmarks

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	w, err := cacher.Watch(ctx, "pods/ns", storage.ListOptions{ResourceVersion: "0", Predicate: pred})
	if err != nil {
		t.Fatalf("Failed to create watch: %v", err)
	}

	resourceVersion := uint64(1000)
	errc := make(chan error, 1)
	go func() {
		deadline := time.Now().Add(time.Second)
		for i := 0; time.Now().Before(deadline); i++ {
			err := cacher.watchCache.Add(&examplev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:            fmt.Sprintf("pod-%d", i),
					Namespace:       "ns",
					ResourceVersion: fmt.Sprintf("%v", resourceVersion+uint64(i)),
				}})
			if err != nil {
				errc <- fmt.Errorf("failed to add a pod: %v", err)
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
	}()

	timeoutCh := time.After(2 * time.Second)
	lastObservedRV := uint64(0)
	for {
		select {
		case err := <-errc:
			t.Fatal(err)
			return
		case event, ok := <-w.ResultChan():
			if !ok {
				t.Fatal("Unexpected closed")
			}
			rv, err := cacher.versioner.ObjectResourceVersion(event.Object)
			if err != nil {
				t.Errorf("failed to parse resource version from %#v: %v", event.Object, err)
			}
			if event.Type == watch.Bookmark {
				if !expectedBookmarks {
					t.Fatalf("Unexpected bookmark events received")
				}

				if rv < lastObservedRV {
					t.Errorf("Unexpected bookmark event resource version %v (last %v)", rv, lastObservedRV)
				}
				return
			}
			lastObservedRV = rv
		case <-timeoutCh:
			if expectedBookmarks {
				t.Fatal("Unexpected timeout to receive a bookmark event")
			}
			return
		}
	}
}

func TestCacherSendBookmarkEvents(t *testing.T) {
	testCases := []struct {
		allowWatchBookmarks bool
		expectedBookmarks   bool
	}{
		{
			allowWatchBookmarks: true,
			expectedBookmarks:   true,
		},
		{
			allowWatchBookmarks: false,
			expectedBookmarks:   false,
		},
	}

	for _, tc := range testCases {
		testCacherSendBookmarkEvents(t, tc.allowWatchBookmarks, tc.expectedBookmarks)
	}
}

func TestInitialEventsEndBookmark(t *testing.T) {
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.WatchList, true)
	forceRequestWatchProgressSupport(t)

	backingStorage := &dummyStorage{}
	cacher, _, err := newTestCacher(backingStorage)
	if err != nil {
		t.Fatalf("Couldn't create cacher: %v", err)
	}
	defer cacher.Stop()

	if !utilfeature.DefaultFeatureGate.Enabled(features.ResilientWatchCacheInitialization) {
		if err := cacher.ready.wait(context.Background()); err != nil {
			t.Fatalf("unexpected error waiting for the cache to be ready")
		}
	}

	makePod := func(index uint64) *example.Pod {
		return &example.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:            fmt.Sprintf("pod-%d", index),
				Namespace:       "ns",
				ResourceVersion: fmt.Sprintf("%v", 100+index),
			},
		}
	}

	numberOfPods := 3
	var expectedPodEvents []watch.Event
	for i := 1; i <= numberOfPods; i++ {
		pod := makePod(uint64(i))
		if err := cacher.watchCache.Add(pod); err != nil {
			t.Fatalf("failed to add a pod: %v", err)
		}
		expectedPodEvents = append(expectedPodEvents, watch.Event{Type: watch.Added, Object: pod})
	}
	var currentResourceVersion uint64 = 100 + 3

	trueVal, falseVal := true, false

	scenarios := []struct {
		name                string
		allowWatchBookmarks bool
		sendInitialEvents   *bool
	}{
		{
			name:                "allowWatchBookmarks=false, sendInitialEvents=false",
			allowWatchBookmarks: false,
			sendInitialEvents:   &falseVal,
		},
		{
			name:                "allowWatchBookmarks=false, sendInitialEvents=true",
			allowWatchBookmarks: false,
			sendInitialEvents:   &trueVal,
		},
		{
			name:                "allowWatchBookmarks=true, sendInitialEvents=true",
			allowWatchBookmarks: true,
			sendInitialEvents:   &trueVal,
		},
		{
			name:                "allowWatchBookmarks=true, sendInitialEvents=false",
			allowWatchBookmarks: true,
			sendInitialEvents:   &falseVal,
		},
		{
			name:                "allowWatchBookmarks=false, sendInitialEvents=nil",
			allowWatchBookmarks: true,
		},
	}

	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			expectedWatchEvents := expectedPodEvents
			if scenario.allowWatchBookmarks && scenario.sendInitialEvents != nil && *scenario.sendInitialEvents {
				expectedWatchEvents = append(expectedWatchEvents, watch.Event{
					Type: watch.Bookmark,
					Object: &example.Pod{
						ObjectMeta: metav1.ObjectMeta{
							ResourceVersion: strconv.FormatUint(currentResourceVersion, 10),
							Annotations:     map[string]string{metav1.InitialEventsAnnotationKey: "true"},
						},
					},
				})
			}

			pred := storage.Everything
			pred.AllowWatchBookmarks = scenario.allowWatchBookmarks
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()
			w, err := cacher.Watch(ctx, "pods/ns", storage.ListOptions{ResourceVersion: "100", SendInitialEvents: scenario.sendInitialEvents, Predicate: pred})
			if err != nil {
				t.Fatalf("Failed to create watch: %v", err)
			}
			storagetesting.TestCheckResultsInStrictOrder(t, w, expectedWatchEvents)
			storagetesting.TestCheckNoMoreResultsWithIgnoreFunc(t, w, nil)
		})
	}
}

func TestCacherSendsMultipleWatchBookmarks(t *testing.T) {
	backingStorage := &dummyStorage{}
	cacher, _, err := newTestCacher(backingStorage)
	if err != nil {
		t.Fatalf("Couldn't create cacher: %v", err)
	}
	defer cacher.Stop()
	// Update bookmarkFrequency to speed up test.
	// Note that the frequency lower than 1s doesn't change much due to
	// resolution how frequency we recompute.
	cacher.bookmarkWatchers.bookmarkFrequency = time.Second

	if !utilfeature.DefaultFeatureGate.Enabled(features.ResilientWatchCacheInitialization) {
		if err := cacher.ready.wait(context.Background()); err != nil {
			t.Fatalf("unexpected error waiting for the cache to be ready")
		}
	}
	pred := storage.Everything
	pred.AllowWatchBookmarks = true

	makePod := func(index int) *examplev1.Pod {
		return &examplev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:            fmt.Sprintf("pod-%d", index),
				Namespace:       "ns",
				ResourceVersion: fmt.Sprintf("%v", 100+index),
			},
		}
	}

	// Create pod to initialize watch cache.
	if err := cacher.watchCache.Add(makePod(0)); err != nil {
		t.Fatalf("failed to add a pod: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	w, err := cacher.Watch(ctx, "pods/ns", storage.ListOptions{ResourceVersion: "100", Predicate: pred})
	if err != nil {
		t.Fatalf("Failed to create watch: %v", err)
	}

	// Create one more pod, to ensure that current RV is higher and thus
	// bookmarks will be delievere (events are delivered for RV higher
	// than the max from init events).
	if err := cacher.watchCache.Add(makePod(1)); err != nil {
		t.Fatalf("failed to add a pod: %v", err)
	}

	timeoutCh := time.After(5 * time.Second)
	lastObservedRV := uint64(0)
	// Ensure that a watcher gets two bookmarks.
	for observedBookmarks := 0; observedBookmarks < 2; {
		select {
		case event, ok := <-w.ResultChan():
			if !ok {
				t.Fatal("Unexpected closed")
			}
			rv, err := cacher.versioner.ObjectResourceVersion(event.Object)
			if err != nil {
				t.Errorf("failed to parse resource version from %#v: %v", event.Object, err)
			}
			if event.Type == watch.Bookmark {
				observedBookmarks++
				if rv < lastObservedRV {
					t.Errorf("Unexpected bookmark event resource version %v (last %v)", rv, lastObservedRV)
				}
			}
			lastObservedRV = rv
		case <-timeoutCh:
			t.Fatal("Unexpected timeout to receive bookmark events")
		}
	}
}

func TestDispatchingBookmarkEventsWithConcurrentStop(t *testing.T) {
	backingStorage := &dummyStorage{}
	cacher, _, err := newTestCacher(backingStorage)
	if err != nil {
		t.Fatalf("Couldn't create cacher: %v", err)
	}
	defer cacher.Stop()

	if !utilfeature.DefaultFeatureGate.Enabled(features.ResilientWatchCacheInitialization) {
		if err := cacher.ready.wait(context.Background()); err != nil {
			t.Fatalf("unexpected error waiting for the cache to be ready")
		}
	}

	// Ensure there is some budget for slowing down processing.
	cacher.dispatchTimeoutBudget.returnUnused(100 * time.Millisecond)

	resourceVersion := uint64(1000)
	err = cacher.watchCache.Add(&examplev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "pod-0",
			Namespace:       "ns",
			ResourceVersion: fmt.Sprintf("%v", resourceVersion),
		}})
	if err != nil {
		t.Fatalf("failed to add a pod: %v", err)
	}

	for i := 0; i < 1000; i++ {
		pred := storage.Everything
		pred.AllowWatchBookmarks = true
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		w, err := cacher.Watch(ctx, "pods/ns", storage.ListOptions{ResourceVersion: "999", Predicate: pred})
		if err != nil {
			t.Fatalf("Failed to create watch: %v", err)
		}
		bookmark := &watchCacheEvent{
			Type:            watch.Bookmark,
			ResourceVersion: uint64(i),
			Object:          cacher.newFunc(),
		}
		err = cacher.versioner.UpdateObject(bookmark.Object, bookmark.ResourceVersion)
		if err != nil {
			t.Fatalf("failure to update version of object (%d) %#v", bookmark.ResourceVersion, bookmark.Object)
		}

		wg := sync.WaitGroup{}
		wg.Add(2)
		go func() {
			cacher.processEvent(bookmark)
			wg.Done()
		}()

		go func() {
			w.Stop()
			wg.Done()
		}()

		done := make(chan struct{})
		go func() {
			for range w.ResultChan() {
			}
			close(done)
		}()

		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("receive result timeout")
		}
		w.Stop()
		wg.Wait()
	}
}

func TestBookmarksOnResourceVersionUpdates(t *testing.T) {
	backingStorage := &dummyStorage{}
	cacher, _, err := newTestCacher(backingStorage)
	if err != nil {
		t.Fatalf("Couldn't create cacher: %v", err)
	}
	defer cacher.Stop()

	// Ensure that bookmarks are sent more frequently than every 1m.
	cacher.bookmarkWatchers = newTimeBucketWatchers(clock.RealClock{}, 2*time.Second)

	if !utilfeature.DefaultFeatureGate.Enabled(features.ResilientWatchCacheInitialization) {
		if err := cacher.ready.wait(context.Background()); err != nil {
			t.Fatalf("unexpected error waiting for the cache to be ready")
		}
	}

	makePod := func(i int) *examplev1.Pod {
		return &examplev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:            fmt.Sprintf("pod-%d", i),
				Namespace:       "ns",
				ResourceVersion: fmt.Sprintf("%d", i),
			},
		}
	}
	if err := cacher.watchCache.Add(makePod(1000)); err != nil {
		t.Errorf("error: %v", err)
	}

	pred := storage.Everything
	pred.AllowWatchBookmarks = true

	w, err := cacher.Watch(context.TODO(), "/pods/ns", storage.ListOptions{
		ResourceVersion: "1000",
		Predicate:       pred,
	})
	if err != nil {
		t.Fatalf("Failed to create watch: %v", err)
	}

	expectedRV := 2000

	var rcErr error

	wg := sync.WaitGroup{}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			event, ok := <-w.ResultChan()
			if !ok {
				rcErr = errors.New("Unexpected closed channel")
				return
			}
			rv, err := cacher.versioner.ObjectResourceVersion(event.Object)
			if err != nil {
				t.Errorf("failed to parse resource version from %#v: %v", event.Object, err)
			}
			if event.Type == watch.Bookmark && rv == uint64(expectedRV) {
				return
			}
		}
	}()

	// Simulate progress notify event.
	cacher.watchCache.UpdateResourceVersion(strconv.Itoa(expectedRV))

	wg.Wait()
	if rcErr != nil {
		t.Fatal(rcErr)
	}
}

type fakeTimeBudget struct{}

func (f *fakeTimeBudget) takeAvailable() time.Duration {
	return 2 * time.Second
}

func (f *fakeTimeBudget) returnUnused(_ time.Duration) {}

func TestStartingResourceVersion(t *testing.T) {
	backingStorage := &dummyStorage{}
	cacher, _, err := newTestCacher(backingStorage)
	if err != nil {
		t.Fatalf("Couldn't create cacher: %v", err)
	}
	defer cacher.Stop()

	if !utilfeature.DefaultFeatureGate.Enabled(features.ResilientWatchCacheInitialization) {
		if err := cacher.ready.wait(context.Background()); err != nil {
			t.Fatalf("unexpected error waiting for the cache to be ready")
		}
	}

	// Ensure there is some budget for slowing down processing.
	// We use the fakeTimeBudget to prevent this test from flaking under
	// the following conditions:
	// 1) in total we create 11 events that has to be processed by the watcher
	// 2) the size of the channels are set to 10 for the watcher
	// 3) if the test is cpu-starved and the internal goroutine is not picking
	//    up these events from the channel, after consuming the whole time
	//    budget (defaulted to 100ms) on waiting, we will simply close the watch,
	//    which will cause the test failure
	// Using fakeTimeBudget gives us always a budget to wait and have a test
	// pick up something from ResultCh in the meantime.
	//
	// The same can potentially happen in production, but in that case a watch
	// can be resumed by the client. This doesn't work in the case of this test,
	// because we explicitly want to test the behavior that object changes are
	// happening after the watch was initiated.
	cacher.dispatchTimeoutBudget = &fakeTimeBudget{}

	makePod := func(i int) *examplev1.Pod {
		return &examplev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:            "foo",
				Namespace:       "ns",
				Labels:          map[string]string{"foo": strconv.Itoa(i)},
				ResourceVersion: fmt.Sprintf("%d", i),
			},
		}
	}

	if err := cacher.watchCache.Add(makePod(1000)); err != nil {
		t.Errorf("error: %v", err)
	}
	// Advance RV by 10.
	startVersion := uint64(1010)

	watcher, err := cacher.Watch(context.TODO(), "pods/ns/foo", storage.ListOptions{ResourceVersion: strconv.FormatUint(startVersion, 10), Predicate: storage.Everything})
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	defer watcher.Stop()

	for i := 1; i <= 11; i++ {
		if err := cacher.watchCache.Update(makePod(1000 + i)); err != nil {
			t.Errorf("error: %v", err)
		}
	}

	e, ok := <-watcher.ResultChan()
	if !ok {
		t.Errorf("unexpectedly closed watch")
	}
	object := e.Object
	if co, ok := object.(runtime.CacheableObject); ok {
		object = co.GetObject()
	}
	pod := object.(*examplev1.Pod)
	podRV, err := cacher.versioner.ParseResourceVersion(pod.ResourceVersion)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// event should have at least rv + 1, since we're starting the watch at rv
	if podRV <= startVersion {
		t.Errorf("expected event with resourceVersion of at least %d, got %d", startVersion+1, podRV)
	}
}

func TestDispatchEventWillNotBeBlockedByTimedOutWatcher(t *testing.T) {
	backingStorage := &dummyStorage{}
	cacher, _, err := newTestCacher(backingStorage)
	if err != nil {
		t.Fatalf("Couldn't create cacher: %v", err)
	}
	defer cacher.Stop()

	if !utilfeature.DefaultFeatureGate.Enabled(features.ResilientWatchCacheInitialization) {
		if err := cacher.ready.wait(context.Background()); err != nil {
			t.Fatalf("unexpected error waiting for the cache to be ready")
		}
	}

	// Ensure there is some budget for slowing down processing.
	// We use the fakeTimeBudget to prevent this test from flaking under
	// the following conditions:
	// 1) the watch w1 is blocked, so we were consuming the whole budget once
	//    its buffer was filled in (10 items)
	// 2) the budget is refreshed once per second, so it basically wasn't
	//    happening in the test at all
	// 3) if the test was cpu-starved and we weren't able to consume events
	//    from w2 ResultCh it could have happened that its buffer was also
	//    filling in and given we no longer had timeBudget (consumed in (1))
	//    trying to put next item was simply breaking the watch
	// Using fakeTimeBudget gives us always a budget to wait and have a test
	// pick up something from ResultCh in the meantime.
	cacher.dispatchTimeoutBudget = &fakeTimeBudget{}

	makePod := func(i int) *examplev1.Pod {
		return &examplev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:            fmt.Sprintf("pod-%d", 1000+i),
				Namespace:       "ns",
				ResourceVersion: fmt.Sprintf("%d", 1000+i),
			},
		}
	}
	if err := cacher.watchCache.Add(makePod(0)); err != nil {
		t.Errorf("error: %v", err)
	}

	totalPods := 50

	// Create watcher that will be blocked.
	w1, err := cacher.Watch(context.TODO(), "pods/ns", storage.ListOptions{ResourceVersion: "999", Predicate: storage.Everything})
	if err != nil {
		t.Fatalf("Failed to create watch: %v", err)
	}
	defer w1.Stop()

	// Create fast watcher and ensure it will get all objects.
	w2, err := cacher.Watch(context.TODO(), "pods/ns", storage.ListOptions{ResourceVersion: "999", Predicate: storage.Everything})
	if err != nil {
		t.Fatalf("Failed to create watch: %v", err)
	}
	defer w2.Stop()

	// Now push a ton of object to cache.
	for i := 1; i < totalPods; i++ {
		cacher.watchCache.Add(makePod(i))
	}

	shouldContinue := true
	eventsCount := 0
	for shouldContinue {
		select {
		case event, ok := <-w2.ResultChan():
			if !ok {
				shouldContinue = false
				break
			}
			if event.Type == watch.Added {
				eventsCount++
				if eventsCount == totalPods {
					shouldContinue = false
				}
			}
		case <-time.After(wait.ForeverTestTimeout):
			shouldContinue = false
			w2.Stop()
		}
	}
	if eventsCount != totalPods {
		t.Errorf("watcher is blocked by slower one (count: %d)", eventsCount)
	}
}

func verifyEvents(t *testing.T, w watch.Interface, events []watch.Event, strictOrder bool) {
	_, _, line, _ := goruntime.Caller(1)
	actualEvents := make([]watch.Event, len(events))
	for idx := range events {
		select {
		case event := <-w.ResultChan():
			actualEvents[idx] = event
		case <-time.After(wait.ForeverTestTimeout):
			t.Logf("(called from line %d)", line)
			t.Errorf("Timed out waiting for an event")
		}
	}
	validateEvents := func(expected, actual watch.Event) (bool, []string) {
		errors := []string{}
		if e, a := expected.Type, actual.Type; e != a {
			errors = append(errors, fmt.Sprintf("Expected: %s, got: %s", e, a))
		}
		actualObject := actual.Object
		if co, ok := actualObject.(runtime.CacheableObject); ok {
			actualObject = co.GetObject()
		}
		if e, a := expected.Object, actualObject; !apiequality.Semantic.DeepEqual(e, a) {
			errors = append(errors, fmt.Sprintf("Expected: %#v, got: %#v", e, a))
		}
		return len(errors) == 0, errors
	}

	if len(events) != len(actualEvents) {
		t.Fatalf("unexpected number of events: %d, expected: %d, acutalEvents: %#v, expectedEvents:%#v", len(actualEvents), len(events), actualEvents, events)
	}

	if strictOrder {
		for idx, expectedEvent := range events {
			valid, errors := validateEvents(expectedEvent, actualEvents[idx])
			if !valid {
				t.Logf("(called from line %d)", line)
				for _, err := range errors {
					t.Error(err)
				}
			}
		}
	}
	for _, expectedEvent := range events {
		validated := false
		for _, actualEvent := range actualEvents {
			if validated, _ = validateEvents(expectedEvent, actualEvent); validated {
				break
			}
		}
		if !validated {
			t.Fatalf("Expected: %#v but didn't find", expectedEvent)
		}
	}
}

func TestCachingDeleteEvents(t *testing.T) {
	backingStorage := &dummyStorage{}
	cacher, _, err := newTestCacher(backingStorage)
	if err != nil {
		t.Fatalf("Couldn't create cacher: %v", err)
	}
	defer cacher.Stop()

	if !utilfeature.DefaultFeatureGate.Enabled(features.ResilientWatchCacheInitialization) {
		if err := cacher.ready.wait(context.Background()); err != nil {
			t.Fatalf("unexpected error waiting for the cache to be ready")
		}
	}

	fooPredicate := storage.SelectionPredicate{
		Label: labels.SelectorFromSet(map[string]string{"foo": "true"}),
		Field: fields.Everything(),
	}
	barPredicate := storage.SelectionPredicate{
		Label: labels.SelectorFromSet(map[string]string{"bar": "true"}),
		Field: fields.Everything(),
	}

	createWatch := func(pred storage.SelectionPredicate) watch.Interface {
		w, err := cacher.Watch(context.TODO(), "pods/ns", storage.ListOptions{ResourceVersion: "999", Predicate: pred})
		if err != nil {
			t.Fatalf("Failed to create watch: %v", err)
		}
		return w
	}

	allEventsWatcher := createWatch(storage.Everything)
	defer allEventsWatcher.Stop()
	fooEventsWatcher := createWatch(fooPredicate)
	defer fooEventsWatcher.Stop()
	barEventsWatcher := createWatch(barPredicate)
	defer barEventsWatcher.Stop()

	makePod := func(labels map[string]string, rv string) *examplev1.Pod {
		return &examplev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:            "pod",
				Namespace:       "ns",
				Labels:          labels,
				ResourceVersion: rv,
			},
		}
	}
	pod1 := makePod(map[string]string{"foo": "true", "bar": "true"}, "1001")
	pod2 := makePod(map[string]string{"foo": "true"}, "1002")
	pod3 := makePod(map[string]string{}, "1003")
	pod4 := makePod(map[string]string{}, "1004")
	pod1DeletedAt2 := pod1.DeepCopyObject().(*examplev1.Pod)
	pod1DeletedAt2.ResourceVersion = "1002"
	pod2DeletedAt3 := pod2.DeepCopyObject().(*examplev1.Pod)
	pod2DeletedAt3.ResourceVersion = "1003"

	allEvents := []watch.Event{
		{Type: watch.Added, Object: pod1.DeepCopy()},
		{Type: watch.Modified, Object: pod2.DeepCopy()},
		{Type: watch.Modified, Object: pod3.DeepCopy()},
		{Type: watch.Deleted, Object: pod4.DeepCopy()},
	}
	fooEvents := []watch.Event{
		{Type: watch.Added, Object: pod1.DeepCopy()},
		{Type: watch.Modified, Object: pod2.DeepCopy()},
		{Type: watch.Deleted, Object: pod2DeletedAt3.DeepCopy()},
	}
	barEvents := []watch.Event{
		{Type: watch.Added, Object: pod1.DeepCopy()},
		{Type: watch.Deleted, Object: pod1DeletedAt2.DeepCopy()},
	}

	cacher.watchCache.Add(pod1)
	cacher.watchCache.Update(pod2)
	cacher.watchCache.Update(pod3)
	cacher.watchCache.Delete(pod4)

	verifyEvents(t, allEventsWatcher, allEvents, true)
	verifyEvents(t, fooEventsWatcher, fooEvents, true)
	verifyEvents(t, barEventsWatcher, barEvents, true)
}

func testCachingObjects(t *testing.T, watchersCount int) {
	backingStorage := &dummyStorage{}
	cacher, _, err := newTestCacher(backingStorage)
	if err != nil {
		t.Fatalf("Couldn't create cacher: %v", err)
	}
	defer cacher.Stop()

	if !utilfeature.DefaultFeatureGate.Enabled(features.ResilientWatchCacheInitialization) {
		if err := cacher.ready.wait(context.Background()); err != nil {
			t.Fatalf("unexpected error waiting for the cache to be ready")
		}
	}

	dispatchedEvents := []*watchCacheEvent{}
	cacher.watchCache.eventHandler = func(event *watchCacheEvent) {
		dispatchedEvents = append(dispatchedEvents, event)
		cacher.processEvent(event)
	}

	watchers := make([]watch.Interface, 0, watchersCount)
	for i := 0; i < watchersCount; i++ {
		w, err := cacher.Watch(context.TODO(), "pods/ns", storage.ListOptions{ResourceVersion: "1000", Predicate: storage.Everything})
		if err != nil {
			t.Fatalf("Failed to create watch: %v", err)
		}
		defer w.Stop()
		watchers = append(watchers, w)
	}

	makePod := func(name, rv string) *examplev1.Pod {
		return &examplev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:            name,
				Namespace:       "ns",
				ResourceVersion: rv,
			},
		}
	}
	pod1 := makePod("pod", "1001")
	pod2 := makePod("pod", "1002")
	pod3 := makePod("pod", "1003")

	cacher.watchCache.Add(pod1)
	cacher.watchCache.Update(pod2)
	cacher.watchCache.Delete(pod3)

	// At this point, we already have dispatchedEvents fully propagated.

	verifyEvents := func(w watch.Interface) {
		var event watch.Event
		for index := range dispatchedEvents {
			select {
			case event = <-w.ResultChan():
			case <-time.After(wait.ForeverTestTimeout):
				t.Fatalf("timeout watiching for the event")
			}

			var object runtime.Object
			if _, ok := event.Object.(runtime.CacheableObject); !ok {
				t.Fatalf("Object in %s event should support caching: %#v", event.Type, event.Object)
			}
			object = event.Object.(runtime.CacheableObject).GetObject()

			if event.Type == watch.Deleted {
				resourceVersion, err := cacher.versioner.ObjectResourceVersion(cacher.watchCache.cache[index].PrevObject)
				if err != nil {
					t.Fatalf("Failed to parse resource version: %v", err)
				}
				updateResourceVersion(object, cacher.versioner, resourceVersion)
			}

			var e runtime.Object
			switch event.Type {
			case watch.Added, watch.Modified:
				e = cacher.watchCache.cache[index].Object
			case watch.Deleted:
				e = cacher.watchCache.cache[index].PrevObject
			default:
				t.Errorf("unexpected watch event: %#v", event)
			}
			if a := object; !reflect.DeepEqual(a, e) {
				t.Errorf("event object messed up for %s: %#v, expected: %#v", event.Type, a, e)
			}
		}
	}

	for i := range watchers {
		verifyEvents(watchers[i])
	}
}

func TestCachingObjects(t *testing.T) {
	t.Run("single watcher", func(t *testing.T) { testCachingObjects(t, 1) })
	t.Run("many watcher", func(t *testing.T) { testCachingObjects(t, 3) })
}

func TestCacheIntervalInvalidationStopsWatch(t *testing.T) {
	backingStorage := &dummyStorage{}
	cacher, _, err := newTestCacher(backingStorage)
	if err != nil {
		t.Fatalf("Couldn't create cacher: %v", err)
	}
	defer cacher.Stop()

	if !utilfeature.DefaultFeatureGate.Enabled(features.ResilientWatchCacheInitialization) {
		if err := cacher.ready.wait(context.Background()); err != nil {
			t.Fatalf("unexpected error waiting for the cache to be ready")
		}
	}

	// Ensure there is enough budget for slow processing since
	// the entire watch cache is going to be served through the
	// interval and events won't be popped from the cacheWatcher's
	// input channel until much later.
	cacher.dispatchTimeoutBudget.returnUnused(100 * time.Millisecond)

	// We define a custom index validator such that the interval is
	// able to serve the first bufferSize elements successfully, but
	// on trying to fill it's buffer again, the indexValidator simulates
	// an invalidation leading to the watch being closed and the number
	// of events we actually process to be bufferSize, each event of
	// type watch.Added.
	valid := true
	invalidateCacheInterval := func() {
		valid = false
	}
	once := sync.Once{}
	indexValidator := func(index int) bool {
		isValid := valid && (index >= cacher.watchCache.startIndex)
		once.Do(invalidateCacheInterval)
		return isValid
	}
	cacher.watchCache.indexValidator = indexValidator

	makePod := func(i int) *examplev1.Pod {
		return &examplev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:            fmt.Sprintf("pod-%d", 1000+i),
				Namespace:       "ns",
				ResourceVersion: fmt.Sprintf("%d", 1000+i),
			},
		}
	}

	// 250 is arbitrary, point is to have enough elements such that
	// it generates more than bufferSize number of events allowing
	// us to simulate the invalidation of the cache interval.
	totalPods := 250
	for i := 0; i < totalPods; i++ {
		err := cacher.watchCache.Add(makePod(i))
		if err != nil {
			t.Errorf("error: %v", err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w, err := cacher.Watch(ctx, "pods/ns", storage.ListOptions{
		ResourceVersion: "999",
		Predicate:       storage.Everything,
	})
	if err != nil {
		t.Fatalf("Failed to create watch: %v", err)
	}
	defer w.Stop()

	received := 0
	resChan := w.ResultChan()
	for event := range resChan {
		received++
		t.Logf("event type: %v, events received so far: %d", event.Type, received)
		if event.Type != watch.Added {
			t.Errorf("unexpected event type, expected: %s, got: %s, event: %v", watch.Added, event.Type, event)
		}
	}
	// Since the watch is stopped after the interval is invalidated,
	// we should have processed exactly bufferSize number of elements.
	if received != bufferSize {
		t.Errorf("unexpected number of events received, expected: %d, got: %d", bufferSize+1, received)
	}
}

func TestWaitUntilWatchCacheFreshAndForceAllEvents(t *testing.T) {
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.WatchList, true)
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.ConsistentListFromCache, true)
	forceRequestWatchProgressSupport(t)

	scenarios := []struct {
		name               string
		opts               storage.ListOptions
		backingStorage     *dummyStorage
		verifyBackingStore func(t *testing.T, s *dummyStorage)
	}{
		{
			name: "allowWatchBookmarks=true, sendInitialEvents=true, RV=105",
			opts: storage.ListOptions{
				Predicate: func() storage.SelectionPredicate {
					p := storage.Everything
					p.AllowWatchBookmarks = true
					return p
				}(),
				SendInitialEvents: pointer.Bool(true),
				ResourceVersion:   "105",
			},
			verifyBackingStore: func(t *testing.T, s *dummyStorage) {
				require.NotEqual(t, 0, s.getRequestWatchProgressCounter(), "expected store.RequestWatchProgressCounter to be > 0. It looks like watch progress wasn't requested!")
			},
		},

		{
			name: "legacy: allowWatchBookmarks=false, sendInitialEvents=true, RV=unset",
			opts: storage.ListOptions{
				Predicate: func() storage.SelectionPredicate {
					p := storage.Everything
					p.AllowWatchBookmarks = false
					return p
				}(),
				SendInitialEvents: pointer.Bool(true),
			},
			backingStorage: func() *dummyStorage {
				hasBeenPrimed := false
				s := &dummyStorage{}
				s.getListFn = func(_ context.Context, key string, opts storage.ListOptions, listObj runtime.Object) error {
					listAccessor, err := meta.ListAccessor(listObj)
					if err != nil {
						return err
					}
					// the first call to this function
					// primes the cacher
					if !hasBeenPrimed {
						listAccessor.SetResourceVersion("100")
						hasBeenPrimed = true
						return nil
					}
					listAccessor.SetResourceVersion("105")
					return nil
				}
				return s
			}(),
			verifyBackingStore: func(t *testing.T, s *dummyStorage) {
				require.NotEqual(t, 0, s.getRequestWatchProgressCounter(), "expected store.RequestWatchProgressCounter to be > 0. It looks like watch progress wasn't requested!")
			},
		},

		{
			name: "allowWatchBookmarks=true, sendInitialEvents=true, RV=unset",
			opts: storage.ListOptions{
				Predicate: func() storage.SelectionPredicate {
					p := storage.Everything
					p.AllowWatchBookmarks = true
					return p
				}(),
				SendInitialEvents: pointer.Bool(true),
			},
			backingStorage: func() *dummyStorage {
				hasBeenPrimed := false
				s := &dummyStorage{}
				s.getListFn = func(_ context.Context, key string, opts storage.ListOptions, listObj runtime.Object) error {
					listAccessor, err := meta.ListAccessor(listObj)
					if err != nil {
						return err
					}
					// the first call to this function
					// primes the cacher
					if !hasBeenPrimed {
						listAccessor.SetResourceVersion("100")
						hasBeenPrimed = true
						return nil
					}
					listAccessor.SetResourceVersion("105")
					return nil
				}
				return s
			}(),
			verifyBackingStore: func(t *testing.T, s *dummyStorage) {
				require.NotEqual(t, 0, s.getRequestWatchProgressCounter(), "expected store.RequestWatchProgressCounter to be > 0. It looks like watch progress wasn't requested!")
			},
		},
	}

	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			t.Parallel()
			var backingStorage *dummyStorage
			if scenario.backingStorage != nil {
				backingStorage = scenario.backingStorage
			} else {
				backingStorage = &dummyStorage{}
			}
			cacher, _, err := newTestCacher(backingStorage)
			if err != nil {
				t.Fatalf("Couldn't create cacher: %v", err)
			}
			defer cacher.Stop()

			if !utilfeature.DefaultFeatureGate.Enabled(features.ResilientWatchCacheInitialization) {
				if err := cacher.ready.wait(context.Background()); err != nil {
					t.Fatalf("unexpected error waiting for the cache to be ready")
				}
			}

			w, err := cacher.Watch(context.Background(), "pods/ns", scenario.opts)
			require.NoError(t, err, "failed to create watch: %v")
			defer w.Stop()
			var expectedErr *apierrors.StatusError
			if !errors.As(storage.NewTooLargeResourceVersionError(105, 100, resourceVersionTooHighRetrySeconds), &expectedErr) {
				t.Fatalf("Unable to convert NewTooLargeResourceVersionError to apierrors.StatusError")
			}
			verifyEvents(t, w, []watch.Event{
				{
					Type: watch.Error,
					Object: &metav1.Status{
						Status:  metav1.StatusFailure,
						Message: expectedErr.Error(),
						Details: expectedErr.ErrStatus.Details,
						Reason:  metav1.StatusReasonTimeout,
						Code:    504,
					},
				},
			}, true)

			go func(t *testing.T) {
				err := cacher.watchCache.Add(makeTestPodDetails("pod1", 105, "node1", map[string]string{"label": "value1"}))
				require.NoError(t, err, "failed adding a pod to the watchCache")
			}(t)
			w, err = cacher.Watch(context.Background(), "pods/ns", scenario.opts)
			require.NoError(t, err, "failed to create watch: %v")
			defer w.Stop()
			verifyEvents(t, w, []watch.Event{
				{
					Type:   watch.Added,
					Object: makeTestPodDetails("pod1", 105, "node1", map[string]string{"label": "value1"}),
				},
			}, true)
			if scenario.verifyBackingStore != nil {
				scenario.verifyBackingStore(t, backingStorage)
			}
		})
	}
}

type fakeStorage struct {
	pods []example.Pod
	storage.Interface
}

func newObjectStorage(fakePods []example.Pod) *fakeStorage {
	return &fakeStorage{
		pods: fakePods,
	}
}

func (m fakeStorage) GetList(ctx context.Context, key string, opts storage.ListOptions, listObj runtime.Object) error {
	podList := listObj.(*example.PodList)
	podList.ListMeta = metav1.ListMeta{ResourceVersion: "12345"}
	podList.Items = m.pods
	return nil
}
func (m fakeStorage) Watch(_ context.Context, _ string, _ storage.ListOptions) (watch.Interface, error) {
	return newDummyWatch(), nil
}

func BenchmarkCacher_GetList(b *testing.B) {
	testCases := []struct {
		totalObjectNum  int
		expectObjectNum int
	}{
		{
			totalObjectNum:  5000,
			expectObjectNum: 50,
		},
		{
			totalObjectNum:  5000,
			expectObjectNum: 500,
		},
		{
			totalObjectNum:  5000,
			expectObjectNum: 1000,
		},
		{
			totalObjectNum:  5000,
			expectObjectNum: 2500,
		},
		{
			totalObjectNum:  5000,
			expectObjectNum: 5000,
		},
	}
	for _, tc := range testCases {
		b.Run(
			fmt.Sprintf("totalObjectNum=%d, expectObjectNum=%d", tc.totalObjectNum, tc.expectObjectNum),
			func(b *testing.B) {
				// create sample pods
				fakePods := make([]example.Pod, tc.totalObjectNum, tc.totalObjectNum)
				for i := range fakePods {
					fakePods[i].Namespace = "default"
					fakePods[i].Name = fmt.Sprintf("pod-%d", i)
					fakePods[i].ResourceVersion = strconv.Itoa(i)
					if i%(tc.totalObjectNum/tc.expectObjectNum) == 0 {
						fakePods[i].Spec.NodeName = "node-0"
					}
					data := make([]byte, 1024*2, 1024*2) // 2k labels
					rand.Read(data)
					fakePods[i].Spec.NodeSelector = map[string]string{
						"key": string(data),
					}
				}

				// build test cacher
				cacher, _, err := newTestCacher(newObjectStorage(fakePods))
				if err != nil {
					b.Fatalf("new cacher: %v", err)
				}
				defer cacher.Stop()

				// prepare result and pred
				parsedField, err := fields.ParseSelector("spec.nodeName=node-0")
				if err != nil {
					b.Fatalf("parse selector: %v", err)
				}
				pred := storage.SelectionPredicate{
					Label: labels.Everything(),
					Field: parsedField,
				}

				// now we start benchmarking
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					result := &example.PodList{}
					err = cacher.GetList(context.TODO(), "pods", storage.ListOptions{
						Predicate:       pred,
						Recursive:       true,
						ResourceVersion: "12345",
					}, result)
					if err != nil {
						b.Fatalf("GetList cache: %v", err)
					}
					if len(result.Items) != tc.expectObjectNum {
						b.Fatalf("expect %d but got %d", tc.expectObjectNum, len(result.Items))
					}
				}
			})
	}
}

// TestWatchListIsSynchronisedWhenNoEventsFromStoreReceived makes sure that
// a bookmark event will be delivered even if the cacher has not received an event.
func TestWatchListIsSynchronisedWhenNoEventsFromStoreReceived(t *testing.T) {
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.WatchList, true)
	backingStorage := &dummyStorage{}
	cacher, _, err := newTestCacher(backingStorage)
	require.NoError(t, err, "failed to create cacher")
	defer cacher.Stop()

	if !utilfeature.DefaultFeatureGate.Enabled(features.ResilientWatchCacheInitialization) {
		if err := cacher.ready.wait(context.Background()); err != nil {
			t.Fatalf("unexpected error waiting for the cache to be ready")
		}
	}

	pred := storage.Everything
	pred.AllowWatchBookmarks = true
	opts := storage.ListOptions{
		Predicate:         pred,
		SendInitialEvents: pointer.Bool(true),
	}
	w, err := cacher.Watch(context.Background(), "pods/ns", opts)
	require.NoError(t, err, "failed to create watch: %v")
	defer w.Stop()

	verifyEvents(t, w, []watch.Event{
		{Type: watch.Bookmark, Object: &example.Pod{
			ObjectMeta: metav1.ObjectMeta{
				ResourceVersion: "100",
				Annotations:     map[string]string{metav1.InitialEventsAnnotationKey: "true"},
			},
		}},
	}, true)
}

func TestForgetWatcher(t *testing.T) {
	backingStorage := &dummyStorage{}
	cacher, _, err := newTestCacher(backingStorage)
	require.NoError(t, err)
	defer cacher.Stop()

	if !utilfeature.DefaultFeatureGate.Enabled(features.ResilientWatchCacheInitialization) {
		if err := cacher.ready.wait(context.Background()); err != nil {
			t.Fatalf("unexpected error waiting for the cache to be ready")
		}
	}

	assertCacherInternalState := func(expectedWatchersCounter, expectedValueWatchersCounter int) {
		cacher.Lock()
		defer cacher.Unlock()

		require.Len(t, cacher.watchers.allWatchers, expectedWatchersCounter)
		require.Len(t, cacher.watchers.valueWatchers, expectedValueWatchersCounter)
	}
	assertCacherInternalState(0, 0)

	var forgetWatcherFn func(bool)
	var forgetCounter int
	forgetWatcherWrapped := func(drainWatcher bool) {
		forgetCounter++
		forgetWatcherFn(drainWatcher)
	}
	w := newCacheWatcher(
		0,
		func(_ string, _ labels.Set, _ fields.Set) bool { return true },
		nil,
		storage.APIObjectVersioner{},
		testingclock.NewFakeClock(time.Now()).Now().Add(2*time.Minute),
		true,
		schema.GroupResource{Resource: "pods"},
		"1",
	)
	forgetWatcherFn = forgetWatcher(cacher, w, 0, namespacedName{}, "", false)
	addWatcher := func(w *cacheWatcher) {
		cacher.Lock()
		defer cacher.Unlock()

		cacher.watchers.addWatcher(w, 0, namespacedName{}, "", false)
	}

	addWatcher(w)
	assertCacherInternalState(1, 0)

	forgetWatcherWrapped(false)
	assertCacherInternalState(0, 0)
	require.Equal(t, 1, forgetCounter)

	forgetWatcherWrapped(false)
	assertCacherInternalState(0, 0)
	require.Equal(t, 2, forgetCounter)
}

// TestGetWatchCacheResourceVersion test the following cases:
//
// +-----------------+---------------------+-----------------------+
// | ResourceVersion | AllowWatchBookmarks | SendInitialEvents     |
// +=================+=====================+=======================+
// | Unset           | true/false          | nil/true/false        |
// | 0               | true/false          | nil/true/false        |
// | 95             | true/false          | nil/true/false         |
// +-----------------+---------------------+-----------------------+
// where:
// - false indicates the value of the param was set to "false" by a test case
// - true  indicates the value of the param was set to "true" by a test case
func TestGetWatchCacheResourceVersion(t *testing.T) {
	listOptions := func(allowBookmarks bool, sendInitialEvents *bool, rv string) storage.ListOptions {
		p := storage.Everything
		p.AllowWatchBookmarks = allowBookmarks

		opts := storage.ListOptions{}
		opts.Predicate = p
		opts.SendInitialEvents = sendInitialEvents
		opts.ResourceVersion = rv
		return opts
	}

	scenarios := []struct {
		name string
		opts storage.ListOptions

		expectedWatchResourceVersion int
	}{
		// +-----------------+---------------------+-----------------------+
		// | ResourceVersion | AllowWatchBookmarks | SendInitialEvents     |
		// +=================+=====================+=======================+
		// | Unset           | true/false          | nil/true/false        |
		// +-----------------+---------------------+-----------------------+
		{
			name: "RV=unset, allowWatchBookmarks=true, sendInitialEvents=nil",
			opts: listOptions(true, nil, ""),
			// Expecting RV 0, due to https://github.com/kubernetes/kubernetes/pull/123935 reverted to serving those requests from watch cache.
			// Set to 100, when WatchFromStorageWithoutResourceVersion is set to true.
			expectedWatchResourceVersion: 0,
		},
		{
			name:                         "RV=unset, allowWatchBookmarks=true, sendInitialEvents=true",
			opts:                         listOptions(true, pointer.Bool(true), ""),
			expectedWatchResourceVersion: 100,
		},
		{
			name:                         "RV=unset, allowWatchBookmarks=true, sendInitialEvents=false",
			opts:                         listOptions(true, pointer.Bool(false), ""),
			expectedWatchResourceVersion: 100,
		},
		{
			name: "RV=unset, allowWatchBookmarks=false, sendInitialEvents=nil",
			opts: listOptions(false, nil, ""),
			// Expecting RV 0, due to https://github.com/kubernetes/kubernetes/pull/123935 reverted to serving those requests from watch cache.
			// Set to 100, when WatchFromStorageWithoutResourceVersion is set to true.
			expectedWatchResourceVersion: 0,
		},
		{
			name:                         "RV=unset, allowWatchBookmarks=false, sendInitialEvents=true, legacy",
			opts:                         listOptions(false, pointer.Bool(true), ""),
			expectedWatchResourceVersion: 100,
		},
		{
			name:                         "RV=unset, allowWatchBookmarks=false, sendInitialEvents=false",
			opts:                         listOptions(false, pointer.Bool(false), ""),
			expectedWatchResourceVersion: 100,
		},
		// +-----------------+---------------------+-----------------------+
		// | ResourceVersion | AllowWatchBookmarks | SendInitialEvents     |
		// +=================+=====================+=======================+
		// | 0               | true/false          | nil/true/false        |
		// +-----------------+---------------------+-----------------------+
		{
			name:                         "RV=0, allowWatchBookmarks=true, sendInitialEvents=nil",
			opts:                         listOptions(true, nil, "0"),
			expectedWatchResourceVersion: 0,
		},
		{
			name:                         "RV=0, allowWatchBookmarks=true, sendInitialEvents=true",
			opts:                         listOptions(true, pointer.Bool(true), "0"),
			expectedWatchResourceVersion: 0,
		},
		{
			name:                         "RV=0, allowWatchBookmarks=true, sendInitialEvents=false",
			opts:                         listOptions(true, pointer.Bool(false), "0"),
			expectedWatchResourceVersion: 0,
		},
		{
			name:                         "RV=0, allowWatchBookmarks=false, sendInitialEvents=nil",
			opts:                         listOptions(false, nil, "0"),
			expectedWatchResourceVersion: 0,
		},
		{
			name:                         "RV=0, allowWatchBookmarks=false, sendInitialEvents=true",
			opts:                         listOptions(false, pointer.Bool(true), "0"),
			expectedWatchResourceVersion: 0,
		},
		{
			name:                         "RV=0, allowWatchBookmarks=false, sendInitialEvents=false",
			opts:                         listOptions(false, pointer.Bool(false), "0"),
			expectedWatchResourceVersion: 0,
		},
		// +-----------------+---------------------+-----------------------+
		// | ResourceVersion | AllowWatchBookmarks | SendInitialEvents     |
		// +=================+=====================+=======================+
		// | 95             | true/false          | nil/true/false         |
		// +-----------------+---------------------+-----------------------+
		{
			name:                         "RV=95, allowWatchBookmarks=true, sendInitialEvents=nil",
			opts:                         listOptions(true, nil, "95"),
			expectedWatchResourceVersion: 95,
		},
		{
			name:                         "RV=95, allowWatchBookmarks=true, sendInitialEvents=true",
			opts:                         listOptions(true, pointer.Bool(true), "95"),
			expectedWatchResourceVersion: 95,
		},
		{
			name:                         "RV=95, allowWatchBookmarks=true, sendInitialEvents=false",
			opts:                         listOptions(true, pointer.Bool(false), "95"),
			expectedWatchResourceVersion: 95,
		},
		{
			name:                         "RV=95, allowWatchBookmarks=false, sendInitialEvents=nil",
			opts:                         listOptions(false, nil, "95"),
			expectedWatchResourceVersion: 95,
		},
		{
			name:                         "RV=95, allowWatchBookmarks=false, sendInitialEvents=true",
			opts:                         listOptions(false, pointer.Bool(true), "95"),
			expectedWatchResourceVersion: 95,
		},
		{
			name:                         "RV=95, allowWatchBookmarks=false, sendInitialEvents=false",
			opts:                         listOptions(false, pointer.Bool(false), "95"),
			expectedWatchResourceVersion: 95,
		},
	}

	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			backingStorage := &dummyStorage{}
			cacher, _, err := newTestCacher(backingStorage)
			require.NoError(t, err, "couldn't create cacher")
			defer cacher.Stop()

			parsedResourceVersion := 0
			if len(scenario.opts.ResourceVersion) > 0 {
				parsedResourceVersion, err = strconv.Atoi(scenario.opts.ResourceVersion)
				require.NoError(t, err)
			}

			actualResourceVersion, err := cacher.getWatchCacheResourceVersion(context.TODO(), uint64(parsedResourceVersion), scenario.opts)
			require.NoError(t, err)
			require.Equal(t, uint64(scenario.expectedWatchResourceVersion), actualResourceVersion, "received unexpected ResourceVersion")
		})
	}
}

// TestGetBookmarkAfterResourceVersionLockedFunc test the following cases:
//
// +-----------------+---------------------+-----------------------+
// | ResourceVersion | AllowWatchBookmarks | SendInitialEvents     |
// +=================+=====================+=======================+
// | Unset           | true/false          | nil/true/false        |
// | 0               | true/false          | nil/true/false        |
// | 95             | true/false          | nil/true/false         |
// +-----------------+---------------------+-----------------------+
// where:
// - false indicates the value of the param was set to "false" by a test case
// - true  indicates the value of the param was set to "true" by a test case
func TestGetBookmarkAfterResourceVersionLockedFunc(t *testing.T) {
	listOptions := func(allowBookmarks bool, sendInitialEvents *bool, rv string) storage.ListOptions {
		p := storage.Everything
		p.AllowWatchBookmarks = allowBookmarks

		opts := storage.ListOptions{}
		opts.Predicate = p
		opts.SendInitialEvents = sendInitialEvents
		opts.ResourceVersion = rv
		return opts
	}

	scenarios := []struct {
		name                      string
		opts                      storage.ListOptions
		requiredResourceVersion   int
		watchCacheResourceVersion int

		expectedBookmarkResourceVersion int
	}{
		// +-----------------+---------------------+-----------------------+
		// | ResourceVersion | AllowWatchBookmarks | SendInitialEvents     |
		// +=================+=====================+=======================+
		// | Unset           | true/false          | nil/true/false        |
		// +-----------------+---------------------+-----------------------+
		{
			name:                            "RV=unset, allowWatchBookmarks=true, sendInitialEvents=nil",
			opts:                            listOptions(true, nil, ""),
			requiredResourceVersion:         100,
			watchCacheResourceVersion:       99,
			expectedBookmarkResourceVersion: 0,
		},
		{
			name:                            "RV=unset, allowWatchBookmarks=true, sendInitialEvents=true",
			opts:                            listOptions(true, pointer.Bool(true), ""),
			requiredResourceVersion:         100,
			watchCacheResourceVersion:       99,
			expectedBookmarkResourceVersion: 100,
		},
		{
			name:                            "RV=unset, allowWatchBookmarks=true, sendInitialEvents=false",
			opts:                            listOptions(true, pointer.Bool(false), ""),
			requiredResourceVersion:         100,
			watchCacheResourceVersion:       99,
			expectedBookmarkResourceVersion: 0,
		},
		{
			name:                            "RV=unset, allowWatchBookmarks=false, sendInitialEvents=nil",
			opts:                            listOptions(false, nil, ""),
			requiredResourceVersion:         100,
			watchCacheResourceVersion:       99,
			expectedBookmarkResourceVersion: 0,
		},
		{
			name:                            "RV=unset, allowWatchBookmarks=false, sendInitialEvents=true",
			opts:                            listOptions(false, pointer.Bool(true), ""),
			requiredResourceVersion:         100,
			watchCacheResourceVersion:       99,
			expectedBookmarkResourceVersion: 0,
		},
		{
			name:                            "RV=unset, allowWatchBookmarks=false, sendInitialEvents=false",
			opts:                            listOptions(false, pointer.Bool(false), ""),
			requiredResourceVersion:         100,
			watchCacheResourceVersion:       99,
			expectedBookmarkResourceVersion: 0,
		},
		// +-----------------+---------------------+-----------------------+
		// | ResourceVersion | AllowWatchBookmarks | SendInitialEvents     |
		// +=================+=====================+=======================+
		// | 0               | true/false          | nil/true/false        |
		// +-----------------+---------------------+-----------------------+
		{
			name:                            "RV=0, allowWatchBookmarks=true, sendInitialEvents=nil",
			opts:                            listOptions(true, nil, "0"),
			requiredResourceVersion:         0,
			watchCacheResourceVersion:       99,
			expectedBookmarkResourceVersion: 0,
		},
		{
			name:                            "RV=0, allowWatchBookmarks=true, sendInitialEvents=true",
			opts:                            listOptions(true, pointer.Bool(true), "0"),
			requiredResourceVersion:         0,
			watchCacheResourceVersion:       99,
			expectedBookmarkResourceVersion: 99,
		},
		{
			name:                            "RV=0, allowWatchBookmarks=true, sendInitialEvents=false",
			opts:                            listOptions(true, pointer.Bool(false), "0"),
			requiredResourceVersion:         0,
			watchCacheResourceVersion:       99,
			expectedBookmarkResourceVersion: 0,
		},
		{
			name:                            "RV=0, allowWatchBookmarks=false, sendInitialEvents=nil",
			opts:                            listOptions(false, nil, "0"),
			requiredResourceVersion:         0,
			watchCacheResourceVersion:       99,
			expectedBookmarkResourceVersion: 0,
		},
		{
			name:                            "RV=0, allowWatchBookmarks=false, sendInitialEvents=true",
			opts:                            listOptions(false, pointer.Bool(true), "0"),
			requiredResourceVersion:         0,
			watchCacheResourceVersion:       99,
			expectedBookmarkResourceVersion: 0,
		},
		{
			name:                            "RV=0, allowWatchBookmarks=false, sendInitialEvents=false",
			opts:                            listOptions(false, pointer.Bool(false), "0"),
			requiredResourceVersion:         0,
			watchCacheResourceVersion:       99,
			expectedBookmarkResourceVersion: 0,
		},
		// +-----------------+---------------------+-----------------------+
		// | ResourceVersion | AllowWatchBookmarks | SendInitialEvents     |
		// +=================+=====================+=======================+
		// | 95             | true/false          | nil/true/false         |
		// +-----------------+---------------------+-----------------------+
		{
			name:                            "RV=95, allowWatchBookmarks=true, sendInitialEvents=nil",
			opts:                            listOptions(true, nil, "95"),
			requiredResourceVersion:         0,
			watchCacheResourceVersion:       99,
			expectedBookmarkResourceVersion: 0,
		},
		{
			name:                            "RV=95, allowWatchBookmarks=true, sendInitialEvents=true",
			opts:                            listOptions(true, pointer.Bool(true), "95"),
			requiredResourceVersion:         0,
			watchCacheResourceVersion:       99,
			expectedBookmarkResourceVersion: 95,
		},
		{
			name:                            "RV=95, allowWatchBookmarks=true, sendInitialEvents=false",
			opts:                            listOptions(true, pointer.Bool(false), "95"),
			requiredResourceVersion:         0,
			watchCacheResourceVersion:       99,
			expectedBookmarkResourceVersion: 0,
		},
		{
			name:                            "RV=95, allowWatchBookmarks=false, sendInitialEvents=nil",
			opts:                            listOptions(false, nil, "95"),
			requiredResourceVersion:         100,
			watchCacheResourceVersion:       99,
			expectedBookmarkResourceVersion: 0,
		},
		{
			name:                            "RV=95, allowWatchBookmarks=false, sendInitialEvents=true",
			opts:                            listOptions(false, pointer.Bool(true), "95"),
			requiredResourceVersion:         0,
			watchCacheResourceVersion:       99,
			expectedBookmarkResourceVersion: 0,
		},
		{
			name:                            "RV=95, allowWatchBookmarks=false, sendInitialEvents=false",
			opts:                            listOptions(false, pointer.Bool(false), "95"),
			requiredResourceVersion:         0,
			watchCacheResourceVersion:       99,
			expectedBookmarkResourceVersion: 0,
		},
	}
	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			backingStorage := &dummyStorage{}
			cacher, _, err := newTestCacher(backingStorage)
			require.NoError(t, err, "couldn't create cacher")

			defer cacher.Stop()

			if !utilfeature.DefaultFeatureGate.Enabled(features.ResilientWatchCacheInitialization) {
				if err := cacher.ready.wait(context.Background()); err != nil {
					t.Fatalf("unexpected error waiting for the cache to be ready")
				}
			}

			cacher.watchCache.UpdateResourceVersion(fmt.Sprintf("%d", scenario.watchCacheResourceVersion))
			parsedResourceVersion := 0
			if len(scenario.opts.ResourceVersion) > 0 {
				parsedResourceVersion, err = strconv.Atoi(scenario.opts.ResourceVersion)
				require.NoError(t, err)
			}

			getBookMarkFn, err := cacher.getBookmarkAfterResourceVersionLockedFunc(uint64(parsedResourceVersion), uint64(scenario.requiredResourceVersion), scenario.opts)
			require.NoError(t, err)
			cacher.watchCache.RLock()
			defer cacher.watchCache.RUnlock()
			getBookMarkResourceVersion := getBookMarkFn()
			require.Equal(t, uint64(scenario.expectedBookmarkResourceVersion), getBookMarkResourceVersion, "received unexpected ResourceVersion")
		})
	}
}

func TestWatchStreamSeparation(t *testing.T) {
	server, etcdStorage := newEtcdTestStorage(t, etcd3testing.PathPrefix())
	t.Cleanup(func() {
		server.Terminate(t)
	})
	setupOpts := &setupOptions{}
	withDefaults(setupOpts)
	config := Config{
		Storage:             etcdStorage,
		Versioner:           storage.APIObjectVersioner{},
		GroupResource:       schema.GroupResource{Resource: "pods"},
		EventsHistoryWindow: DefaultEventFreshDuration,
		ResourcePrefix:      setupOpts.resourcePrefix,
		KeyFunc:             setupOpts.keyFunc,
		GetAttrsFunc:        GetPodAttrs,
		NewFunc:             newPod,
		NewListFunc:         newPodList,
		IndexerFuncs:        setupOpts.indexerFuncs,
		Codec:               codecs.LegacyCodec(examplev1.SchemeGroupVersion),
		Clock:               setupOpts.clock,
	}
	tcs := []struct {
		name                         string
		separateCacheWatchRPC        bool
		useWatchCacheContextMetadata bool
		expectBookmarkOnWatchCache   bool
		expectBookmarkOnEtcd         bool
	}{
		{
			name:                       "common RPC > both get bookmarks",
			separateCacheWatchRPC:      false,
			expectBookmarkOnEtcd:       true,
			expectBookmarkOnWatchCache: true,
		},
		{
			name:                         "common RPC & watch cache context > both get bookmarks",
			separateCacheWatchRPC:        false,
			useWatchCacheContextMetadata: true,
			expectBookmarkOnEtcd:         true,
			expectBookmarkOnWatchCache:   true,
		},
		{
			name:                       "separate RPC > only etcd gets bookmarks",
			separateCacheWatchRPC:      true,
			expectBookmarkOnEtcd:       true,
			expectBookmarkOnWatchCache: false,
		},
		{
			name:                         "separate RPC & watch cache context > only watch cache gets bookmarks",
			separateCacheWatchRPC:        true,
			useWatchCacheContextMetadata: true,
			expectBookmarkOnEtcd:         false,
			expectBookmarkOnWatchCache:   true,
		},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.SeparateCacheWatchRPC, tc.separateCacheWatchRPC)
			cacher, err := NewCacherFromConfig(config)
			if err != nil {
				t.Fatalf("Failed to initialize cacher: %v", err)
			}

			if !utilfeature.DefaultFeatureGate.Enabled(features.ResilientWatchCacheInitialization) {
				if err := cacher.ready.wait(context.TODO()); err != nil {
					t.Fatalf("unexpected error waiting for the cache to be ready")
				}
			}

			getCacherRV := func() uint64 {
				cacher.watchCache.RLock()
				defer cacher.watchCache.RUnlock()
				return cacher.watchCache.resourceVersion
			}
			waitContext, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			waitForEtcdBookmark := watchAndWaitForBookmark(t, waitContext, cacher.storage)

			var out example.Pod
			err = cacher.Create(context.Background(), "foo", &example.Pod{}, &out, 0)
			if err != nil {
				t.Fatal(err)
			}
			err = cacher.Delete(context.Background(), "foo", &out, nil, storage.ValidateAllObjectFunc, &example.Pod{}, storage.DeleteOptions{})
			if err != nil {
				t.Fatal(err)
			}
			versioner := storage.APIObjectVersioner{}
			var lastResourceVersion uint64
			lastResourceVersion, err = versioner.ObjectResourceVersion(&out)
			if err != nil {
				t.Fatal(err)
			}

			var contextMetadata metadata.MD
			if tc.useWatchCacheContextMetadata {
				contextMetadata = cacher.watchCache.waitingUntilFresh.contextMetadata
			}
			// For the first 100ms from watch creation, watch progress requests are ignored.
			time.Sleep(200 * time.Millisecond)
			err = cacher.storage.RequestWatchProgress(metadata.NewOutgoingContext(context.Background(), contextMetadata))
			if err != nil {
				t.Fatal(err)
			}
			// Give time for bookmark to arrive
			time.Sleep(time.Second)

			etcdWatchResourceVersion := waitForEtcdBookmark()
			gotEtcdWatchBookmark := etcdWatchResourceVersion == lastResourceVersion
			if gotEtcdWatchBookmark != tc.expectBookmarkOnEtcd {
				t.Errorf("Unexpected etcd bookmark check result, rv: %d, lastRV: %d, wantMatching: %v", etcdWatchResourceVersion, lastResourceVersion, tc.expectBookmarkOnEtcd)
			}

			watchCacheResourceVersion := getCacherRV()
			cacherGotBookmark := watchCacheResourceVersion == lastResourceVersion
			if cacherGotBookmark != tc.expectBookmarkOnWatchCache {
				t.Errorf("Unexpected watch cache bookmark check result, rv: %d, lastRV: %d, wantMatching: %v", watchCacheResourceVersion, lastResourceVersion, tc.expectBookmarkOnWatchCache)
			}
		})
	}
}

func TestComputeListLimit(t *testing.T) {
	scenarios := []struct {
		name          string
		opts          storage.ListOptions
		expectedLimit int64
	}{
		{
			name: "limit is zero",
			opts: storage.ListOptions{
				Predicate: storage.SelectionPredicate{
					Limit: 0,
				},
			},
			expectedLimit: 0,
		},
		{
			name: "limit is positive, RV is unset",
			opts: storage.ListOptions{
				Predicate: storage.SelectionPredicate{
					Limit: 1,
				},
				ResourceVersion: "",
			},
			expectedLimit: 1,
		},
		{
			name: "limit is positive, RV = 100",
			opts: storage.ListOptions{
				Predicate: storage.SelectionPredicate{
					Limit: 1,
				},
				ResourceVersion: "100",
			},
			expectedLimit: 1,
		},
		{
			name: "legacy case: limit is positive, RV = 0",
			opts: storage.ListOptions{
				Predicate: storage.SelectionPredicate{
					Limit: 1,
				},
				ResourceVersion: "0",
			},
			expectedLimit: 0,
		},
	}

	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			actualLimit := computeListLimit(scenario.opts)
			if actualLimit != scenario.expectedLimit {
				t.Errorf("computeListLimit returned = %v, expected %v", actualLimit, scenario.expectedLimit)
			}
		})
	}
}

func watchAndWaitForBookmark(t *testing.T, ctx context.Context, etcdStorage storage.Interface) func() (resourceVersion uint64) {
	opts := storage.ListOptions{ResourceVersion: "", Predicate: storage.Everything, Recursive: true}
	opts.Predicate.AllowWatchBookmarks = true
	w, err := etcdStorage.Watch(ctx, "/pods/", opts)
	if err != nil {
		t.Fatal(err)
	}

	versioner := storage.APIObjectVersioner{}
	var rv uint64
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for event := range w.ResultChan() {
			if event.Type == watch.Bookmark {
				rv, err = versioner.ObjectResourceVersion(event.Object)
				break
			}
		}
	}()
	return func() (resourceVersion uint64) {
		defer w.Stop()
		wg.Wait()
		if err != nil {
			t.Fatal(err)
		}
		return rv
	}
}

// TODO(p0lyn0mial): forceRequestWatchProgressSupport inits the storage layer
// so that tests that require storage.RequestWatchProgress pass
//
// In the future we could have a function that would allow for setting the feature
// only for duration of a test.
func forceRequestWatchProgressSupport(t *testing.T) {
	if etcdfeature.DefaultFeatureSupportChecker.Supports(storage.RequestWatchProgress) {
		return
	}

	server, _ := newEtcdTestStorage(t, etcd3testing.PathPrefix())
	defer server.Terminate(t)
	if err := wait.PollUntilContextTimeout(context.Background(), 100*time.Millisecond, wait.ForeverTestTimeout, true, func(_ context.Context) (bool, error) {
		return etcdfeature.DefaultFeatureSupportChecker.Supports(storage.RequestWatchProgress), nil
	}); err != nil {
		t.Fatalf("failed to wait for required %v storage feature to initialize", storage.RequestWatchProgress)
	}
}

func TestListIndexer(t *testing.T) {
	ctx, cacher, terminate := testSetup(t, withNodeNameAndNamespaceIndex)
	t.Cleanup(terminate)
	tests := []struct {
		name               string
		requestedNamespace string
		recursive          bool
		fieldSelector      fields.Selector
		indexFields        []string
		expectIndex        string
	}{
		{
			name:          "request without namespace, without field selector",
			recursive:     true,
			fieldSelector: fields.Everything(),
		},
		{
			name:          "request without namespace, field selector with metadata.namespace",
			recursive:     true,
			fieldSelector: fields.ParseSelectorOrDie("metadata.namespace=namespace"),
		},
		{
			name:          "request without namespace, field selector with spec.nodename",
			recursive:     true,
			fieldSelector: fields.ParseSelectorOrDie("spec.nodeName=node"),
			indexFields:   []string{"spec.nodeName"},
			expectIndex:   "f:spec.nodeName",
		},
		{
			name:          "request without namespace, field selector with spec.nodename to filter out",
			recursive:     true,
			fieldSelector: fields.ParseSelectorOrDie("spec.nodeName!=node"),
			indexFields:   []string{"spec.nodeName"},
		},
		{
			name:               "request with namespace, without field selector",
			requestedNamespace: "namespace",
			recursive:          true,
			fieldSelector:      fields.Everything(),
		},
		{
			name:               "request with namespace, field selector with matched metadata.namespace",
			requestedNamespace: "namespace",
			recursive:          true,
			fieldSelector:      fields.ParseSelectorOrDie("metadata.namespace=namespace"),
		},
		{
			name:               "request with namespace, field selector with non-matched metadata.namespace",
			requestedNamespace: "namespace",
			recursive:          true,
			fieldSelector:      fields.ParseSelectorOrDie("metadata.namespace=namespace"),
		},
		{
			name:               "request with namespace, field selector with spec.nodename",
			requestedNamespace: "namespace",
			recursive:          true,
			fieldSelector:      fields.ParseSelectorOrDie("spec.nodeName=node"),
			indexFields:        []string{"spec.nodeName"},
			expectIndex:        "f:spec.nodeName",
		},
		{
			name:               "request with namespace, field selector with spec.nodename to filter out",
			requestedNamespace: "namespace",
			recursive:          true,
			fieldSelector:      fields.ParseSelectorOrDie("spec.nodeName!=node"),
			indexFields:        []string{"spec.nodeName"},
		},
		{
			name:          "request without namespace, field selector with metadata.name",
			recursive:     true,
			fieldSelector: fields.ParseSelectorOrDie("metadata.name=name"),
		},
		{
			name:      "request without namespace, field selector with metadata.name and metadata.namespace",
			recursive: true,
			fieldSelector: fields.SelectorFromSet(fields.Set{
				"metadata.name":      "name",
				"metadata.namespace": "namespace",
			}),
		},
		{
			name:      "request without namespace, field selector with metadata.name and spec.nodeName",
			recursive: true,
			fieldSelector: fields.SelectorFromSet(fields.Set{
				"metadata.name": "name",
				"spec.nodeName": "node",
			}),
			indexFields: []string{"spec.nodeName"},
			expectIndex: "f:spec.nodeName",
		},
		{
			name:      "request without namespace, field selector with metadata.name, and with spec.nodeName to filter out watch",
			recursive: true,
			fieldSelector: fields.AndSelectors(
				fields.ParseSelectorOrDie("spec.nodeName!=node"),
				fields.SelectorFromSet(fields.Set{"metadata.name": "name"}),
			),
			indexFields: []string{"spec.nodeName"},
		},
		{
			name:               "request with namespace, with field selector metadata.name",
			requestedNamespace: "namespace",
			fieldSelector:      fields.ParseSelectorOrDie("metadata.name=name"),
		},
		{
			name:               "request with namespace, with field selector metadata.name and metadata.namespace",
			requestedNamespace: "namespace",
			fieldSelector: fields.SelectorFromSet(fields.Set{
				"metadata.name":      "name",
				"metadata.namespace": "namespace",
			}),
		},
		{
			name:               "request with namespace, with field selector metadata.name, metadata.namespace and spec.nodename",
			requestedNamespace: "namespace",
			fieldSelector: fields.SelectorFromSet(fields.Set{
				"metadata.name":      "name",
				"metadata.namespace": "namespace",
				"spec.nodeName":      "node",
			}),
			indexFields: []string{"spec.nodeName"},
		},
		{
			name:               "request with namespace, with field selector metadata.name, metadata.namespace, and with spec.nodename to filter out",
			requestedNamespace: "namespace",
			fieldSelector: fields.AndSelectors(
				fields.ParseSelectorOrDie("spec.nodeName!=node"),
				fields.SelectorFromSet(fields.Set{"metadata.name": "name", "metadata.namespace": "namespace"}),
			),
			indexFields: []string{"spec.nodeName"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pred := storagetesting.CreatePodPredicate(tt.fieldSelector, true, tt.indexFields)
			_, usedIndex, err := cacher.listItems(ctx, 0, "/pods/"+tt.requestedNamespace, pred, tt.recursive)
			if err != nil {
				t.Errorf("Unexpected error: %v", err)
			}
			if usedIndex != tt.expectIndex {
				t.Errorf("Index doesn't match, expected: %q, got: %q", tt.expectIndex, usedIndex)
			}
		})
	}
}
