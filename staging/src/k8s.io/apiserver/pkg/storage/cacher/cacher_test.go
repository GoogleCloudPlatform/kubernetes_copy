/*
Copyright 2015 The Kubernetes Authors.

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
	"fmt"
	"testing"
	"time"

	"github.com/go-logr/logr"

	apiequality "k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apiserver/pkg/apis/example"
	examplev1 "k8s.io/apiserver/pkg/apis/example/v1"
	"k8s.io/apiserver/pkg/features"
	"k8s.io/apiserver/pkg/storage"
	etcd3testing "k8s.io/apiserver/pkg/storage/etcd3/testing"
	storagetesting "k8s.io/apiserver/pkg/storage/testing"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/tools/cache"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	"k8s.io/klog/v2"
	"k8s.io/utils/clock"
)

func init() {
	metav1.AddToGroupVersion(scheme, metav1.SchemeGroupVersion)
	utilruntime.Must(example.AddToScheme(scheme))
	utilruntime.Must(examplev1.AddToScheme(scheme))
}

// GetPodAttrs returns labels and fields of a given object for filtering purposes.
func GetPodAttrs(obj runtime.Object) (labels.Set, fields.Set, error) {
	pod, ok := obj.(*example.Pod)
	if !ok {
		return nil, nil, fmt.Errorf("not a pod")
	}
	return labels.Set(pod.ObjectMeta.Labels), PodToSelectableFields(pod), nil
}

// PodToSelectableFields returns a field set that represents the object
// TODO: fields are not labels, and the validation rules for them do not apply.
func PodToSelectableFields(pod *example.Pod) fields.Set {
	// The purpose of allocation with a given number of elements is to reduce
	// amount of allocations needed to create the fields.Set. If you add any
	// field here or the number of object-meta related fields changes, this should
	// be adjusted.
	podSpecificFieldsSet := make(fields.Set, 5)
	podSpecificFieldsSet["spec.nodeName"] = pod.Spec.NodeName
	podSpecificFieldsSet["spec.restartPolicy"] = string(pod.Spec.RestartPolicy)
	podSpecificFieldsSet["status.phase"] = string(pod.Status.Phase)
	return AddObjectMetaFieldsSet(podSpecificFieldsSet, &pod.ObjectMeta, true)
}

func AddObjectMetaFieldsSet(source fields.Set, objectMeta *metav1.ObjectMeta, hasNamespaceField bool) fields.Set {
	source["metadata.name"] = objectMeta.Name
	if hasNamespaceField {
		source["metadata.namespace"] = objectMeta.Namespace
	}
	return source
}

func checkStorageInvariants(ctx context.Context, t *testing.T, key string) {
	// No-op function since cacher simply passes object creation to the underlying storage.
}

func TestCreate(t *testing.T) {
	ctx, cacher, terminate := testSetup(t)
	t.Cleanup(terminate)
	storagetesting.RunTestCreate(ctx, t, cacher, checkStorageInvariants)
}

func TestCreateWithTTL(t *testing.T) {
	ctx, cacher, terminate := testSetup(t)
	t.Cleanup(terminate)
	storagetesting.RunTestCreateWithTTL(ctx, t, cacher)
}

func TestCreateWithKeyExist(t *testing.T) {
	ctx, cacher, terminate := testSetup(t)
	t.Cleanup(terminate)
	storagetesting.RunTestCreateWithKeyExist(ctx, t, cacher)
}

func TestGet(t *testing.T) {
	ctx, cacher, terminate := testSetup(t)
	t.Cleanup(terminate)
	storagetesting.RunTestGet(ctx, t, cacher)
}

func TestUnconditionalDelete(t *testing.T) {
	ctx, cacher, terminate := testSetup(t)
	t.Cleanup(terminate)
	storagetesting.RunTestUnconditionalDelete(ctx, t, cacher)
}

func TestConditionalDelete(t *testing.T) {
	ctx, cacher, terminate := testSetup(t)
	t.Cleanup(terminate)
	storagetesting.RunTestConditionalDelete(ctx, t, cacher)
}

func TestDeleteWithSuggestion(t *testing.T) {
	ctx, cacher, terminate := testSetup(t)
	t.Cleanup(terminate)
	storagetesting.RunTestDeleteWithSuggestion(ctx, t, cacher)
}

func TestDeleteWithSuggestionAndConflict(t *testing.T) {
	ctx, cacher, terminate := testSetup(t)
	t.Cleanup(terminate)
	storagetesting.RunTestDeleteWithSuggestionAndConflict(ctx, t, cacher)
}

func TestDeleteWithSuggestionOfDeletedObject(t *testing.T) {
	ctx, cacher, terminate := testSetup(t)
	t.Cleanup(terminate)
	storagetesting.RunTestDeleteWithSuggestionOfDeletedObject(ctx, t, cacher)
}

func TestValidateDeletionWithSuggestion(t *testing.T) {
	ctx, cacher, terminate := testSetup(t)
	t.Cleanup(terminate)
	storagetesting.RunTestValidateDeletionWithSuggestion(ctx, t, cacher)
}

func TestValidateDeletionWithOnlySuggestionValid(t *testing.T) {
	ctx, cacher, terminate := testSetup(t)
	t.Cleanup(terminate)
	storagetesting.RunTestValidateDeletionWithOnlySuggestionValid(ctx, t, cacher)
}

func TestDeleteWithConflict(t *testing.T) {
	ctx, cacher, terminate := testSetup(t)
	t.Cleanup(terminate)
	storagetesting.RunTestDeleteWithConflict(ctx, t, cacher)
}

func TestPreconditionalDeleteWithSuggestion(t *testing.T) {
	ctx, cacher, terminate := testSetup(t)
	t.Cleanup(terminate)
	storagetesting.RunTestPreconditionalDeleteWithSuggestion(ctx, t, cacher)
}

func TestPreconditionalDeleteWithSuggestionPass(t *testing.T) {
	ctx, cacher, terminate := testSetup(t)
	t.Cleanup(terminate)
	storagetesting.RunTestPreconditionalDeleteWithOnlySuggestionPass(ctx, t, cacher)
}

func TestListPaging(t *testing.T) {
	ctx, cacher, terminate := testSetup(t)
	t.Cleanup(terminate)
	storagetesting.RunTestListPaging(ctx, t, cacher)
}

func TestList(t *testing.T) {
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.ConsistentListFromCache, false)
	ctx, cacher, server, terminate := testSetupWithEtcdServer(t)
	t.Cleanup(terminate)
	storagetesting.RunTestList(ctx, t, cacher, compactStorage(cacher, server.V3Client.Client), true)
}

func TestListWithConsistentListFromCache(t *testing.T) {
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.ConsistentListFromCache, true)
	ctx, cacher, server, terminate := testSetupWithEtcdServer(t)
	t.Cleanup(terminate)
	storagetesting.RunTestList(ctx, t, cacher, compactStorage(cacher, server.V3Client.Client), true)
}

func TestConsistentList(t *testing.T) {
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.ConsistentListFromCache, false)
	ctx, cacher, server, terminate := testSetupWithEtcdServer(t)
	t.Cleanup(terminate)
	storagetesting.RunTestConsistentList(ctx, t, cacher, compactStorage(cacher, server.V3Client.Client), true, false)
}

func TestConsistentListWithConsistentListFromCache(t *testing.T) {
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.ConsistentListFromCache, true)
	ctx, cacher, server, terminate := testSetupWithEtcdServer(t)
	t.Cleanup(terminate)
	storagetesting.RunTestConsistentList(ctx, t, cacher, compactStorage(cacher, server.V3Client.Client), true, true)
}

func TestGetListNonRecursive(t *testing.T) {
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.ConsistentListFromCache, false)
	ctx, cacher, server, terminate := testSetupWithEtcdServer(t)
	t.Cleanup(terminate)
	storagetesting.RunTestGetListNonRecursive(ctx, t, compactStorage(cacher, server.V3Client.Client), cacher)
}

func TestGetListNonRecursiveWithConsistentListFromCache(t *testing.T) {
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.ConsistentListFromCache, true)
	ctx, cacher, server, terminate := testSetupWithEtcdServer(t)
	t.Cleanup(terminate)
	storagetesting.RunTestGetListNonRecursive(ctx, t, compactStorage(cacher, server.V3Client.Client), cacher)
}

func TestGetListRecursivePrefix(t *testing.T) {
	ctx, store, _ := testSetup(t)
	storagetesting.RunTestGetListRecursivePrefix(ctx, t, store)
}

func checkStorageCalls(t *testing.T, pageSize, estimatedProcessedObjects uint64) {
	// No-op function for now, since cacher passes pagination calls to underlying storage.
}

func TestListContinuation(t *testing.T) {
	ctx, cacher, terminate := testSetup(t)
	t.Cleanup(terminate)
	storagetesting.RunTestListContinuation(ctx, t, cacher, checkStorageCalls)
}

func TestListPaginationRareObject(t *testing.T) {
	ctx, cacher, terminate := testSetup(t)
	t.Cleanup(terminate)
	storagetesting.RunTestListPaginationRareObject(ctx, t, cacher, checkStorageCalls)
}

func TestListContinuationWithFilter(t *testing.T) {
	ctx, cacher, terminate := testSetup(t)
	t.Cleanup(terminate)
	storagetesting.RunTestListContinuationWithFilter(ctx, t, cacher, checkStorageCalls)
}

func TestListInconsistentContinuation(t *testing.T) {
	ctx, cacher, terminate := testSetup(t)
	t.Cleanup(terminate)
	// TODO(#109831): Enable use of this by setting compaction.
	storagetesting.RunTestListInconsistentContinuation(ctx, t, cacher, nil)
}

func TestListResourceVersionMatch(t *testing.T) {
	// TODO(#109831): Enable use of this test and run it.
}

func TestNamespaceScopedList(t *testing.T) {
	ctx, cacher, terminate := testSetup(t, withNodeNameAndNamespaceIndex)
	t.Cleanup(terminate)
	storagetesting.RunTestNamespaceScopedList(ctx, t, cacher)
}

func TestGuaranteedUpdate(t *testing.T) {
	// TODO(#109831): Enable use of this test and run it.
}

func TestGuaranteedUpdateWithTTL(t *testing.T) {
	ctx, cacher, terminate := testSetup(t)
	t.Cleanup(terminate)
	storagetesting.RunTestGuaranteedUpdateWithTTL(ctx, t, cacher)
}

func TestGuaranteedUpdateChecksStoredData(t *testing.T) {
	// TODO(#109831): Enable use of this test and run it.
}

func TestGuaranteedUpdateWithConflict(t *testing.T) {
	ctx, cacher, terminate := testSetup(t)
	t.Cleanup(terminate)
	storagetesting.RunTestGuaranteedUpdateWithConflict(ctx, t, cacher)
}

func TestGuaranteedUpdateWithSuggestionAndConflict(t *testing.T) {
	ctx, cacher, terminate := testSetup(t)
	t.Cleanup(terminate)
	storagetesting.RunTestGuaranteedUpdateWithSuggestionAndConflict(ctx, t, cacher)
}

func TestTransformationFailure(t *testing.T) {
	// TODO(#109831): Enable use of this test and run it.
}

func TestCount(t *testing.T) {
	ctx, cacher, terminate := testSetup(t)
	t.Cleanup(terminate)
	storagetesting.RunTestCount(ctx, t, cacher)
}

func TestWatch(t *testing.T) {
	ctx, cacher, terminate := testSetup(t)
	t.Cleanup(terminate)
	storagetesting.RunTestWatch(ctx, t, cacher)
}

func TestWatchFromZero(t *testing.T) {
	ctx, cacher, server, terminate := testSetupWithEtcdServer(t)
	t.Cleanup(terminate)
	storagetesting.RunTestWatchFromZero(ctx, t, cacher, compactStorage(cacher, server.V3Client.Client))
}

func TestDeleteTriggerWatch(t *testing.T) {
	ctx, cacher, terminate := testSetup(t)
	t.Cleanup(terminate)
	storagetesting.RunTestDeleteTriggerWatch(ctx, t, cacher)
}

func TestWatchFromNonZero(t *testing.T) {
	ctx, cacher, terminate := testSetup(t)
	t.Cleanup(terminate)
	storagetesting.RunTestWatchFromNonZero(ctx, t, cacher)
}

func TestDelayedWatchDelivery(t *testing.T) {
	ctx, cacher, terminate := testSetup(t)
	t.Cleanup(terminate)
	storagetesting.RunTestDelayedWatchDelivery(ctx, t, cacher)
}

func TestWatchError(t *testing.T) {
	// TODO(#109831): Enable use of this test and run it.
}

func TestWatchContextCancel(t *testing.T) {
	// TODO(#109831): Enable use of this test and run it.
}

func TestWatcherTimeout(t *testing.T) {
	ctx, cacher, terminate := testSetup(t)
	t.Cleanup(terminate)
	storagetesting.RunTestWatcherTimeout(ctx, t, cacher)
}

func TestWatchDeleteEventObjectHaveLatestRV(t *testing.T) {
	ctx, cacher, terminate := testSetup(t)
	t.Cleanup(terminate)
	storagetesting.RunTestWatchDeleteEventObjectHaveLatestRV(ctx, t, cacher)
}

func TestWatchInitializationSignal(t *testing.T) {
	ctx, cacher, terminate := testSetup(t)
	t.Cleanup(terminate)
	storagetesting.RunTestWatchInitializationSignal(ctx, t, cacher)
}

func TestClusterScopedWatch(t *testing.T) {
	ctx, cacher, terminate := testSetup(t, withClusterScopedKeyFunc, withNodeNameAndNamespaceIndex)
	t.Cleanup(terminate)
	storagetesting.RunTestClusterScopedWatch(ctx, t, cacher)
}

func TestNamespaceScopedWatch(t *testing.T) {
	ctx, cacher, terminate := testSetup(t, withNodeNameAndNamespaceIndex)
	t.Cleanup(terminate)
	storagetesting.RunTestNamespaceScopedWatch(ctx, t, cacher)
}

func TestWatchDispatchBookmarkEvents(t *testing.T) {
	ctx, cacher, terminate := testSetup(t)
	t.Cleanup(terminate)
	storagetesting.RunTestWatchDispatchBookmarkEvents(ctx, t, cacher, true)
}

func TestWatchBookmarksWithCorrectResourceVersion(t *testing.T) {
	ctx, cacher, terminate := testSetup(t)
	t.Cleanup(terminate)
	storagetesting.RunTestOptionalWatchBookmarksWithCorrectResourceVersion(ctx, t, cacher)
}

func TestSendInitialEventsBackwardCompatibility(t *testing.T) {
	ctx, store, terminate := testSetup(t)
	t.Cleanup(terminate)
	storagetesting.RunSendInitialEventsBackwardCompatibility(ctx, t, store)
}

func TestWatchSemantics(t *testing.T) {
	store, terminate := testSetupWithEtcdAndCreateWrapper(t)
	t.Cleanup(terminate)
	storagetesting.RunWatchSemantics(context.TODO(), t, store)
}

func TestWatchSemanticInitialEventsExtended(t *testing.T) {
	store, terminate := testSetupWithEtcdAndCreateWrapper(t)
	t.Cleanup(terminate)
	storagetesting.RunWatchSemanticInitialEventsExtended(context.TODO(), t, store)
}

func TestWatchListMatchSingle(t *testing.T) {
	store, terminate := testSetupWithEtcdAndCreateWrapper(t)
	t.Cleanup(terminate)
	storagetesting.RunWatchListMatchSingle(context.TODO(), t, store)
}

// ===================================================
// Test-setup related function are following.
// ===================================================

type tearDownFunc func()

type setupOptions struct {
	resourcePrefix      string
	keyFunc             func(runtime.Object) (string, error)
	indexerFuncs        map[string]storage.IndexerFunc
	indexers            cache.Indexers
	clock               clock.WithTicker
	eventsHistoryWindow time.Duration
}

type setupOption func(*setupOptions)

func withDefaults(options *setupOptions) {
	prefix := "/pods"

	options.resourcePrefix = prefix
	options.keyFunc = func(obj runtime.Object) (string, error) { return storage.NamespaceKeyFunc(prefix, obj) }
	options.clock = clock.RealClock{}
	options.eventsHistoryWindow = DefaultEventFreshDuration
}

func withClusterScopedKeyFunc(options *setupOptions) {
	options.keyFunc = func(obj runtime.Object) (string, error) {
		return storage.NoNamespaceKeyFunc(options.resourcePrefix, obj)
	}
}

// mirror indexer configuration from pkg/registry/core/pod/strategy.go
func withNodeNameAndNamespaceIndex(options *setupOptions) {
	options.indexerFuncs = map[string]storage.IndexerFunc{
		"spec.nodeName": func(obj runtime.Object) string {
			pod, ok := obj.(*example.Pod)
			if !ok {
				return ""
			}
			return pod.Spec.NodeName
		},
	}
	options.indexers = map[string]cache.IndexFunc{
		"f:spec.nodeName": func(obj interface{}) ([]string, error) {
			pod := obj.(*example.Pod)
			return []string{pod.Spec.NodeName}, nil
		},
		"f:metadata.namespace": func(obj interface{}) ([]string, error) {
			pod := obj.(*example.Pod)
			return []string{pod.ObjectMeta.Namespace}, nil
		},
	}
}

func withEventsHistoryWindow(duration time.Duration) func(options *setupOptions) {
	return func(options *setupOptions) {
		options.eventsHistoryWindow = duration
	}
}

func testSetup(t *testing.T, opts ...setupOption) (context.Context, *Cacher, tearDownFunc) {
	ctx, cacher, _, tearDown := testSetupWithEtcdServer(t, opts...)
	return ctx, cacher, tearDown
}

func testSetupWithEtcdServer(t testing.TB, opts ...setupOption) (context.Context, *Cacher, *etcd3testing.EtcdTestServer, tearDownFunc) {
	setupOpts := setupOptions{}
	opts = append([]setupOption{withDefaults}, opts...)
	for _, opt := range opts {
		opt(&setupOpts)
	}

	server, etcdStorage := newEtcdTestStorage(t, etcd3testing.PathPrefix())
	// Inject one list error to make sure we test the relist case.
	wrappedStorage := &storagetesting.StorageInjectingListErrors{
		Interface: etcdStorage,
		Errors:    1,
	}

	config := Config{
		Storage:             wrappedStorage,
		Versioner:           storage.APIObjectVersioner{},
		GroupResource:       schema.GroupResource{Resource: "pods"},
		EventsHistoryWindow: setupOpts.eventsHistoryWindow,
		ResourcePrefix:      setupOpts.resourcePrefix,
		KeyFunc:             setupOpts.keyFunc,
		GetAttrsFunc:        GetPodAttrs,
		NewFunc:             newPod,
		NewListFunc:         newPodList,
		IndexerFuncs:        setupOpts.indexerFuncs,
		Indexers:            &setupOpts.indexers,
		Codec:               codecs.LegacyCodec(examplev1.SchemeGroupVersion),
		Clock:               setupOpts.clock,
	}
	cacher, err := NewCacherFromConfig(config)
	if err != nil {
		t.Fatalf("Failed to initialize cacher: %v", err)
	}
	ctx := context.Background()
	terminate := func() {
		cacher.Stop()
		server.Terminate(t)
	}

	// Since some tests depend on the fact that GetList shouldn't fail,
	// we wait until the error from the underlying storage is consumed.
	if err := wait.PollInfinite(100*time.Millisecond, wrappedStorage.ErrorsConsumed); err != nil {
		t.Fatalf("Failed to inject list errors: %v", err)
	}

	if utilfeature.DefaultFeatureGate.Enabled(features.ResilientWatchCacheInitialization) {
		// The tests assume that Get/GetList/Watch calls shouldn't fail.
		// However, 429 error can now be returned if watchcache is under initialization.
		// To avoid rewriting all tests, we wait for watchcache to initialize.
		if err := cacher.Wait(ctx); err != nil {
			t.Fatal(err)
		}
	}

	return ctx, cacher, server, terminate
}

func testSetupWithEtcdAndCreateWrapper(t *testing.T, opts ...setupOption) (storage.Interface, tearDownFunc) {
	_, cacher, _, tearDown := testSetupWithEtcdServer(t, opts...)

	if !utilfeature.DefaultFeatureGate.Enabled(features.ResilientWatchCacheInitialization) {
		if err := cacher.ready.wait(context.TODO()); err != nil {
			t.Fatalf("unexpected error waiting for the cache to be ready")
		}
	}
	return &createWrapper{Cacher: cacher}, tearDown
}

type createWrapper struct {
	*Cacher
}

func (c *createWrapper) Create(ctx context.Context, key string, obj, out runtime.Object, ttl uint64) error {
	if err := c.Cacher.Create(ctx, key, obj, out, ttl); err != nil {
		return err
	}
	return wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, wait.ForeverTestTimeout, true, func(ctx context.Context) (bool, error) {
		currentObj := c.Cacher.newFunc()
		err := c.Cacher.Get(ctx, key, storage.GetOptions{ResourceVersion: "0"}, currentObj)
		if err != nil {
			if storage.IsNotFound(err) {
				return false, nil
			}
			return false, err
		}
		if !apiequality.Semantic.DeepEqual(currentObj, out) {
			return false, nil
		}
		return true, nil
	})
}

func BenchmarkStoreCreateList(b *testing.B) {
	klog.SetLogger(logr.Discard())
	storeOptions := []struct {
		name         string
		btreeEnabled bool
	}{
		{
			name:         "Btree",
			btreeEnabled: true,
		},
		{
			name:         "Map",
			btreeEnabled: false,
		},
	}
	for _, store := range storeOptions {
		b.Run(fmt.Sprintf("Store=%s", store.name), func(b *testing.B) {
			featuregatetesting.SetFeatureGateDuringTest(b, utilfeature.DefaultFeatureGate, features.BtreeWatchCache, store.btreeEnabled)
			for _, rvm := range []metav1.ResourceVersionMatch{metav1.ResourceVersionMatchNotOlderThan, metav1.ResourceVersionMatchExact} {
				b.Run(fmt.Sprintf("RV=%s", rvm), func(b *testing.B) {
					for _, useIndex := range []bool{true, false} {
						b.Run(fmt.Sprintf("Indexed=%v", useIndex), func(b *testing.B) {
							opts := []setupOption{withEventsHistoryWindow(10 * time.Hour)}
							if useIndex {
								opts = append(opts, withNodeNameAndNamespaceIndex)
							}
							ctx, cacher, _, terminate := testSetupWithEtcdServer(b, opts...)
							b.Cleanup(terminate)
							storagetesting.RunBenchmarkStoreListCreate(ctx, b, cacher, rvm)
						})
					}
				})
			}
		})
	}
}

func BenchmarkStoreList(b *testing.B) {
	klog.SetLogger(logr.Discard())
	// Based on https://github.com/kubernetes/community/blob/master/sig-scalability/configs-and-limits/thresholds.md
	dimensions := []struct {
		namespaceCount       int
		podPerNamespaceCount int
		nodeCount            int
	}{
		{
			namespaceCount:       10_000,
			podPerNamespaceCount: 15,
			nodeCount:            5_000,
		},
		{
			namespaceCount:       50,
			podPerNamespaceCount: 3_000,
			nodeCount:            5_000,
		},
		{
			namespaceCount:       100,
			podPerNamespaceCount: 1_100,
			nodeCount:            1000,
		},
	}
	for _, dims := range dimensions {
		b.Run(fmt.Sprintf("Namespaces=%d/Pods=%d/Nodes=%d", dims.namespaceCount, dims.namespaceCount*dims.podPerNamespaceCount, dims.nodeCount), func(b *testing.B) {
			data := storagetesting.PrepareBenchchmarkData(dims.namespaceCount, dims.podPerNamespaceCount, dims.nodeCount)
			storeOptions := []struct {
				name         string
				btreeEnabled bool
			}{
				{
					name:         "Btree",
					btreeEnabled: true,
				},
				{
					name:         "Map",
					btreeEnabled: false,
				},
			}
			for _, store := range storeOptions {
				b.Run(fmt.Sprintf("Store=%s", store.name), func(b *testing.B) {
					featuregatetesting.SetFeatureGateDuringTest(b, utilfeature.DefaultFeatureGate, features.BtreeWatchCache, store.btreeEnabled)
					ctx, cacher, _, terminate := testSetupWithEtcdServer(b, withNodeNameAndNamespaceIndex, withEventsHistoryWindow(10*time.Hour))
					b.Cleanup(terminate)
					var out example.Pod
					for _, pod := range data.Pods {
						err := cacher.Create(ctx, computePodKey(pod), pod, &out, 0)
						if err != nil {
							b.Fatal(err)
						}
					}
					for _, useIndex := range []bool{true, false} {
						b.Run(fmt.Sprintf("Indexed=%v", useIndex), func(b *testing.B) {
							storagetesting.RunBenchmarkStoreList(ctx, b, cacher, data, useIndex)
						})
					}
				})
			}
		})
	}
}
