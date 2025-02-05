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
	"crypto/rand"
	"fmt"
	"io/ioutil"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/go-logr/logr"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/client/v3/kubernetes"
	"go.etcd.io/etcd/server/v3/embed"
	"google.golang.org/grpc/grpclog"

	"k8s.io/apimachinery/pkg/api/apitesting"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apiserver/pkg/apis/example"
	examplev1 "k8s.io/apiserver/pkg/apis/example/v1"
	genericfeatures "k8s.io/apiserver/pkg/features"
	"k8s.io/apiserver/pkg/storage"
	"k8s.io/apiserver/pkg/storage/etcd3/testserver"
	storagetesting "k8s.io/apiserver/pkg/storage/testing"
	"k8s.io/apiserver/pkg/storage/value"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/klog/v2"
)

var scheme = runtime.NewScheme()
var codecs = serializer.NewCodecFactory(scheme)

const defaultTestPrefix = "test!"

func init() {
	metav1.AddToGroupVersion(scheme, metav1.SchemeGroupVersion)
	utilruntime.Must(example.AddToScheme(scheme))
	utilruntime.Must(examplev1.AddToScheme(scheme))

	grpclog.SetLoggerV2(grpclog.NewLoggerV2(ioutil.Discard, ioutil.Discard, os.Stderr))
}

func newPod() runtime.Object {
	return &example.Pod{}
}

func newPodList() runtime.Object {
	return &example.PodList{}
}

func checkStorageInvariants(etcdClient *clientv3.Client, codec runtime.Codec) storagetesting.KeyValidation {
	return func(ctx context.Context, t *testing.T, key string) {
		getResp, err := etcdClient.KV.Get(ctx, key)
		if err != nil {
			t.Fatalf("etcdClient.KV.Get failed: %v", err)
		}
		if len(getResp.Kvs) == 0 {
			t.Fatalf("expecting non empty result on key: %s", key)
		}
		decoded, err := runtime.Decode(codec, getResp.Kvs[0].Value[len(defaultTestPrefix):])
		if err != nil {
			t.Fatalf("expecting successful decode of object from %v\n%v", err, string(getResp.Kvs[0].Value))
		}
		obj := decoded.(*example.Pod)
		if obj.ResourceVersion != "" {
			t.Errorf("stored object should have empty resource version")
		}
		if obj.SelfLink != "" {
			t.Errorf("stored output should have empty selfLink")
		}
	}
}

func TestCreate(t *testing.T) {
	ctx, store, etcdClient := testSetup(t)
	storagetesting.RunTestCreate(ctx, t, store, checkStorageInvariants(etcdClient.Client, store.codec))
}

func TestCreateWithTTL(t *testing.T) {
	ctx, store, _ := testSetup(t)
	storagetesting.RunTestCreateWithTTL(ctx, t, store)
}

func TestCreateWithKeyExist(t *testing.T) {
	ctx, store, _ := testSetup(t)
	storagetesting.RunTestCreateWithKeyExist(ctx, t, store)
}

func TestGet(t *testing.T) {
	ctx, store, _ := testSetup(t)
	storagetesting.RunTestGet(ctx, t, store)
}

func TestUnconditionalDelete(t *testing.T) {
	ctx, store, _ := testSetup(t)
	storagetesting.RunTestUnconditionalDelete(ctx, t, store)
}

func TestConditionalDelete(t *testing.T) {
	ctx, store, _ := testSetup(t)
	storagetesting.RunTestConditionalDelete(ctx, t, store)
}

func TestDeleteWithSuggestion(t *testing.T) {
	ctx, store, _ := testSetup(t)
	storagetesting.RunTestDeleteWithSuggestion(ctx, t, store)
}

func TestDeleteWithSuggestionAndConflict(t *testing.T) {
	ctx, store, _ := testSetup(t)
	storagetesting.RunTestDeleteWithSuggestionAndConflict(ctx, t, store)
}

func TestDeleteWithSuggestionOfDeletedObject(t *testing.T) {
	ctx, store, _ := testSetup(t)
	storagetesting.RunTestDeleteWithSuggestionOfDeletedObject(ctx, t, store)
}

func TestValidateDeletionWithSuggestion(t *testing.T) {
	ctx, store, _ := testSetup(t)
	storagetesting.RunTestValidateDeletionWithSuggestion(ctx, t, store)
}

func TestValidateDeletionWithOnlySuggestionValid(t *testing.T) {
	ctx, store, _ := testSetup(t)
	storagetesting.RunTestValidateDeletionWithOnlySuggestionValid(ctx, t, store)
}

func TestDeleteWithConflict(t *testing.T) {
	ctx, store, _ := testSetup(t)
	storagetesting.RunTestDeleteWithConflict(ctx, t, store)
}

func TestPreconditionalDeleteWithSuggestion(t *testing.T) {
	ctx, store, _ := testSetup(t)
	storagetesting.RunTestPreconditionalDeleteWithSuggestion(ctx, t, store)
}

func TestPreconditionalDeleteWithSuggestionPass(t *testing.T) {
	ctx, store, _ := testSetup(t)
	storagetesting.RunTestPreconditionalDeleteWithOnlySuggestionPass(ctx, t, store)
}

func TestListPaging(t *testing.T) {
	ctx, store, _ := testSetup(t)
	storagetesting.RunTestListPaging(ctx, t, store)
}

func TestGetListNonRecursive(t *testing.T) {
	ctx, store, client := testSetup(t)
	storagetesting.RunTestGetListNonRecursive(ctx, t, compactStorage(client.Client), store)
}

func TestGetListRecursivePrefix(t *testing.T) {
	ctx, store, _ := testSetup(t)
	storagetesting.RunTestGetListRecursivePrefix(ctx, t, store)
}

func TestGetListWithErrorAggregation(t *testing.T) {
	storagetesting.RunTestGetListWithErrorAggregation(t, func(t *testing.T) (context.Context, *storagetesting.FakeListErrorAggregator, storagetesting.InterfaceWithCorruptTransformer) {
		ctx, s, _ := testSetup(t)
		var store storage.Interface = s
		if utilfeature.DefaultFeatureGate.Enabled(genericfeatures.AllowUnsafeMalformedObjectDeletion) {
			store = NewStoreWithUnsafeCorruptObjectDeletion(store, s.groupResource)
		}

		f := s.listErrAggrFactory
		// wrap the original error aggregator from the store so
		// the test can keep track of the aggregation.
		wrapper := &storagetesting.FakeListErrorAggregator{}
		s.listErrAggrFactory = func() storage.ListErrorAggregator {
			wrapper.ListErrorAggregator = f()
			return wrapper
		}
		return ctx, wrapper, &storeWithCorruptedTransformer{Interface: store, store: s}
	})
}

type storeWithPrefixTransformer struct {
	*store
}

func (s *storeWithPrefixTransformer) UpdatePrefixTransformer(modifier storagetesting.PrefixTransformerModifier) func() {
	originalTransformer := s.transformer.(*storagetesting.PrefixTransformer)
	transformer := *originalTransformer
	s.transformer = modifier(&transformer)
	s.watcher.transformer = modifier(&transformer)
	return func() {
		s.transformer = originalTransformer
		s.watcher.transformer = originalTransformer
	}
}

type corruptedTransformer struct {
	value.Transformer
	// so certain objects can be made corrupt
	shouldBeCorrupt func(ctx context.Context, data []byte) bool
}

func (f *corruptedTransformer) TransformFromStorage(ctx context.Context, data []byte, dataCtx value.Context) (out []byte, stale bool, err error) {
	// every object is corrupt if shouldBeCorrupt is not provided
	corruptObjErr := &corruptObjectError{err: fmt.Errorf("bits flipped"), errType: untransformable}
	if f.shouldBeCorrupt == nil {
		return nil, true, corruptObjErr
	}

	out, stale, err = f.Transformer.TransformFromStorage(ctx, data, dataCtx)
	// unexpected error
	if err != nil {
		return out, stale, err
	}
	if f.shouldBeCorrupt(ctx, out) {
		return nil, true, corruptObjErr
	}
	return out, stale, err
}

type storeWithCorruptedTransformer struct {
	storage.Interface
	// we need the original *store instance to mutate the transformer
	store *store
}

func (s *storeWithCorruptedTransformer) CorruptTransformer(shouldBeCorrupt storagetesting.TransformPredicate) func() {
	ct := &corruptedTransformer{
		Transformer:     s.store.transformer,
		shouldBeCorrupt: shouldBeCorrupt,
	}
	s.store.transformer = ct
	s.store.watcher.transformer = ct
	return func() {
		s.store.transformer = ct.Transformer
		s.store.watcher.transformer = ct.Transformer
	}
}

func TestGuaranteedUpdate(t *testing.T) {
	ctx, store, client := testSetup(t)
	storagetesting.RunTestGuaranteedUpdate(ctx, t, &storeWithPrefixTransformer{store}, checkStorageInvariants(client.Client, store.codec))
}

func TestGuaranteedUpdateWithTTL(t *testing.T) {
	ctx, store, _ := testSetup(t)
	storagetesting.RunTestGuaranteedUpdateWithTTL(ctx, t, store)
}

func TestGuaranteedUpdateChecksStoredData(t *testing.T) {
	ctx, store, _ := testSetup(t)
	storagetesting.RunTestGuaranteedUpdateChecksStoredData(ctx, t, &storeWithPrefixTransformer{store})
}

func TestGuaranteedUpdateWithConflict(t *testing.T) {
	ctx, store, _ := testSetup(t)
	storagetesting.RunTestGuaranteedUpdateWithConflict(ctx, t, store)
}

func TestGuaranteedUpdateWithSuggestionAndConflict(t *testing.T) {
	ctx, store, _ := testSetup(t)
	storagetesting.RunTestGuaranteedUpdateWithSuggestionAndConflict(ctx, t, store)
}

func TestTransformationFailure(t *testing.T) {
	ctx, store, _ := testSetup(t)
	storagetesting.RunTestTransformationFailure(ctx, t, &storeWithPrefixTransformer{store})
}

func TestList(t *testing.T) {
	ctx, store, client := testSetup(t)
	storagetesting.RunTestList(ctx, t, store, compactStorage(client.Client), false)
}

func TestConsistentList(t *testing.T) {
	ctx, store, client := testSetup(t)
	storagetesting.RunTestConsistentList(ctx, t, store, compactStorage(client.Client), false, true)
}

func checkStorageCallsInvariants(transformer *storagetesting.PrefixTransformer, recorder *clientRecorder) storagetesting.CallsValidation {
	return func(t *testing.T, pageSize, estimatedProcessedObjects uint64) {
		if reads := transformer.GetReadsAndReset(); reads != estimatedProcessedObjects {
			t.Errorf("unexpected reads: %d, expected: %d", reads, estimatedProcessedObjects)
		}
		estimatedGetCalls := uint64(1)
		if pageSize != 0 {
			// We expect that kube-apiserver will be increasing page sizes
			// if not full pages are received, so we should see significantly less
			// than 1000 pages (which would be result of talking to etcd with page size
			// copied from pred.Limit).
			// The expected number of calls is n+1 where n is the smallest n so that:
			// pageSize + pageSize * 2 + pageSize * 4 + ... + pageSize * 2^n >= podCount.
			// For pageSize = 1, podCount = 1000, we get n+1 = 10, 2 ^ 10 = 1024.
			currLimit := pageSize
			for sum := uint64(1); sum < estimatedProcessedObjects; {
				currLimit *= 2
				if currLimit > maxLimit {
					currLimit = maxLimit
				}
				sum += currLimit
				estimatedGetCalls++
			}
		}
		if reads := recorder.GetReadsAndReset(); reads != estimatedGetCalls {
			t.Fatalf("unexpected reads: %d, want: %d", reads, estimatedGetCalls)
		}
	}
}

func TestListContinuation(t *testing.T) {
	ctx, store, client := testSetup(t, withRecorder())
	validation := checkStorageCallsInvariants(
		store.transformer.(*storagetesting.PrefixTransformer), client.KV.(*clientRecorder))
	storagetesting.RunTestListContinuation(ctx, t, store, validation)
}

func TestListPaginationRareObject(t *testing.T) {
	ctx, store, client := testSetup(t, withRecorder())
	validation := checkStorageCallsInvariants(
		store.transformer.(*storagetesting.PrefixTransformer), client.KV.(*clientRecorder))
	storagetesting.RunTestListPaginationRareObject(ctx, t, store, validation)
}

func TestListContinuationWithFilter(t *testing.T) {
	ctx, store, client := testSetup(t, withRecorder())
	validation := checkStorageCallsInvariants(
		store.transformer.(*storagetesting.PrefixTransformer), client.KV.(*clientRecorder))
	storagetesting.RunTestListContinuationWithFilter(ctx, t, store, validation)
}

func TestNamespaceScopedList(t *testing.T) {
	ctx, store, _ := testSetup(t)
	storagetesting.RunTestNamespaceScopedList(ctx, t, store)
}

func compactStorage(etcdClient *clientv3.Client) storagetesting.Compaction {
	return func(ctx context.Context, t *testing.T, resourceVersion string) {
		versioner := storage.APIObjectVersioner{}
		rv, err := versioner.ParseResourceVersion(resourceVersion)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err = compact(ctx, etcdClient, 0, int64(rv)); err != nil {
			t.Fatalf("Unable to compact, %v", err)
		}
	}
}

func TestListInconsistentContinuation(t *testing.T) {
	ctx, store, client := testSetup(t)
	storagetesting.RunTestListInconsistentContinuation(ctx, t, store, compactStorage(client.Client))
}

func TestListResourceVersionMatch(t *testing.T) {
	ctx, store, _ := testSetup(t)
	storagetesting.RunTestListResourceVersionMatch(ctx, t, &storeWithPrefixTransformer{store})
}

func TestCount(t *testing.T) {
	ctx, store, _ := testSetup(t)
	storagetesting.RunTestCount(ctx, t, store)
}

// =======================================================================
// Implementation-specific tests are following.
// The following tests are exercising the details of the implementation
// not the actual user-facing contract of storage interface.
// As such, they may focus e.g. on non-functional aspects like performance
// impact.
// =======================================================================

func TestPrefix(t *testing.T) {
	testcases := map[string]string{
		"custom/prefix":     "/custom/prefix/",
		"/custom//prefix//": "/custom/prefix/",
		"/registry":         "/registry/",
	}
	for configuredPrefix, effectivePrefix := range testcases {
		_, store, _ := testSetup(t, withPrefix(configuredPrefix))
		if store.pathPrefix != effectivePrefix {
			t.Errorf("configured prefix of %s, expected effective prefix of %s, got %s", configuredPrefix, effectivePrefix, store.pathPrefix)
		}
	}
}

func Test_growSlice(t *testing.T) {
	type args struct {
		initialCapacity int
		initialLen      int
		v               reflect.Value
		maxCapacity     int
		sizes           []int
	}
	tests := []struct {
		name string
		args args
		cap  int
		len  int
	}{
		{
			name: "empty",
			args: args{v: reflect.ValueOf([]example.Pod{})},
			cap:  0,
		},
		{
			name: "no sizes",
			args: args{v: reflect.ValueOf([]example.Pod{}), maxCapacity: 10},
			cap:  10,
		},
		{
			name: "above maxCapacity",
			args: args{v: reflect.ValueOf([]example.Pod{}), maxCapacity: 10, sizes: []int{1, 12}},
			cap:  10,
		},
		{
			name: "takes max",
			args: args{v: reflect.ValueOf([]example.Pod{}), maxCapacity: 10, sizes: []int{8, 4}},
			cap:  8,
		},
		{
			name: "with existing capacity above max",
			args: args{initialCapacity: 12, maxCapacity: 10, sizes: []int{8, 4}},
			cap:  12,
		},
		{
			name: "with existing capacity below max",
			args: args{initialCapacity: 5, maxCapacity: 10, sizes: []int{8, 4}},
			cap:  8,
		},
		{
			name: "with existing capacity and length above max",
			args: args{initialCapacity: 12, initialLen: 5, maxCapacity: 10, sizes: []int{8, 4}},
			cap:  12,
			len:  5,
		},
		{
			name: "with existing capacity and length below max",
			args: args{initialCapacity: 5, initialLen: 3, maxCapacity: 10, sizes: []int{8, 4}},
			cap:  8,
			len:  3,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.args.initialCapacity > 0 {
				val := make([]example.Pod, tt.args.initialLen, tt.args.initialCapacity)
				for i := 0; i < tt.args.initialLen; i++ {
					val[i].Name = fmt.Sprintf("test-%d", i)
				}
				tt.args.v = reflect.ValueOf(val)
			}
			// reflection requires that the value be addressable in order to call set,
			// so we must ensure the value we created is available on the heap (not a problem
			// for normal usage)
			if !tt.args.v.CanAddr() {
				x := reflect.New(tt.args.v.Type())
				x.Elem().Set(tt.args.v)
				tt.args.v = x.Elem()
			}
			growSlice(tt.args.v, tt.args.maxCapacity, tt.args.sizes...)
			if tt.cap != tt.args.v.Cap() {
				t.Errorf("Unexpected capacity: got=%d want=%d", tt.args.v.Cap(), tt.cap)
			}
			if tt.len != tt.args.v.Len() {
				t.Errorf("Unexpected length: got=%d want=%d", tt.args.v.Len(), tt.len)
			}
			for i := 0; i < tt.args.v.Len(); i++ {
				nameWanted := fmt.Sprintf("test-%d", i)
				val := tt.args.v.Index(i).Interface()
				pod, ok := val.(example.Pod)
				if !ok || pod.Name != nameWanted {
					t.Errorf("Unexpected element value: got=%s, want=%s", pod.Name, nameWanted)
				}
			}
		})
	}
}

func TestLeaseMaxObjectCount(t *testing.T) {
	ctx, store, _ := testSetup(t, withLeaseConfig(LeaseManagerConfig{
		ReuseDurationSeconds: defaultLeaseReuseDurationSeconds,
		MaxObjectCount:       2,
	}))

	obj := &example.Pod{ObjectMeta: metav1.ObjectMeta{Name: "foo"}}
	out := &example.Pod{}

	testCases := []struct {
		key                 string
		expectAttachedCount int64
	}{
		{
			key:                 "testkey1",
			expectAttachedCount: 1,
		},
		{
			key:                 "testkey2",
			expectAttachedCount: 2,
		},
		{
			key: "testkey3",
			// We assume each time has 1 object attached to the lease
			// so after granting a new lease, the recorded count is set to 1
			expectAttachedCount: 1,
		},
	}

	for _, tc := range testCases {
		err := store.Create(ctx, tc.key, obj, out, 120)
		if err != nil {
			t.Fatalf("Set failed: %v", err)
		}
		if store.leaseManager.leaseAttachedObjectCount != tc.expectAttachedCount {
			t.Errorf("Lease manager recorded count %v should be %v", store.leaseManager.leaseAttachedObjectCount, tc.expectAttachedCount)
		}
	}
}

// ===================================================
// Test-setup related function are following.
// ===================================================

func newTestLeaseManagerConfig() LeaseManagerConfig {
	cfg := NewDefaultLeaseManagerConfig()
	// As 30s is the default timeout for testing in global configuration,
	// we cannot wait longer than that in a single time: change it to 1s
	// for testing purposes. See wait.ForeverTestTimeout
	cfg.ReuseDurationSeconds = 1
	return cfg
}

func newTestTransformer() value.Transformer {
	return storagetesting.NewPrefixTransformer([]byte(defaultTestPrefix), false)
}

type clientRecorder struct {
	reads uint64
	clientv3.KV
}

func (r *clientRecorder) Get(ctx context.Context, key string, opts ...clientv3.OpOption) (*clientv3.GetResponse, error) {
	atomic.AddUint64(&r.reads, 1)
	return r.KV.Get(ctx, key, opts...)
}

func (r *clientRecorder) GetReadsAndReset() uint64 {
	return atomic.SwapUint64(&r.reads, 0)
}

type setupOptions struct {
	client         func(testing.TB) *kubernetes.Client
	codec          runtime.Codec
	newFunc        func() runtime.Object
	newListFunc    func() runtime.Object
	prefix         string
	resourcePrefix string
	groupResource  schema.GroupResource
	transformer    value.Transformer
	leaseConfig    LeaseManagerConfig

	recorderEnabled bool
}

type setupOption func(*setupOptions)

func withClientConfig(config *embed.Config) setupOption {
	return func(options *setupOptions) {
		options.client = func(t testing.TB) *kubernetes.Client {
			return testserver.RunEtcd(t, config)
		}
	}
}

func withPrefix(prefix string) setupOption {
	return func(options *setupOptions) {
		options.prefix = prefix
	}
}

func withLeaseConfig(leaseConfig LeaseManagerConfig) setupOption {
	return func(options *setupOptions) {
		options.leaseConfig = leaseConfig
	}
}

func withRecorder() setupOption {
	return func(options *setupOptions) {
		options.recorderEnabled = true
	}
}

func withDefaults(options *setupOptions) {
	options.client = func(t testing.TB) *kubernetes.Client {
		return testserver.RunEtcd(t, nil)
	}
	options.codec = apitesting.TestCodec(codecs, examplev1.SchemeGroupVersion)
	options.newFunc = newPod
	options.newListFunc = newPodList
	options.prefix = ""
	options.resourcePrefix = "/pods"
	options.groupResource = schema.GroupResource{Resource: "pods"}
	options.transformer = newTestTransformer()
	options.leaseConfig = newTestLeaseManagerConfig()
}

var _ setupOption = withDefaults

func testSetup(t testing.TB, opts ...setupOption) (context.Context, *store, *kubernetes.Client) {
	setupOpts := setupOptions{}
	opts = append([]setupOption{withDefaults}, opts...)
	for _, opt := range opts {
		opt(&setupOpts)
	}
	client := setupOpts.client(t)
	if setupOpts.recorderEnabled {
		client.KV = &clientRecorder{KV: client.KV}
	}
	versioner := storage.APIObjectVersioner{}
	store := newStore(
		client,
		setupOpts.codec,
		setupOpts.newFunc,
		setupOpts.newListFunc,
		setupOpts.prefix,
		setupOpts.resourcePrefix,
		setupOpts.groupResource,
		setupOpts.transformer,
		setupOpts.leaseConfig,
		NewDefaultDecoder(setupOpts.codec, versioner),
		versioner,
	)
	ctx := context.Background()
	return ctx, store, client
}

func TestValidateKey(t *testing.T) {
	validKeys := []string{
		"/foo/bar/baz/a.b.c/",
		"/foo",
		"foo/bar/baz",
		"/foo/bar..baz/",
		"/foo/bar..",
		"foo",
		"foo/bar",
		"/foo/bar/",
	}
	invalidKeys := []string{
		"/foo/bar/../a.b.c/",
		"..",
		"/..",
		"../",
		"/foo/bar/..",
		"../foo/bar",
		"/../foo",
		"/foo/bar/../",
		".",
		"/.",
		"./",
		"/./",
		"/foo/.",
		"./bar",
		"/foo/./bar/",
	}
	const (
		pathPrefix   = "/first/second"
		expectPrefix = pathPrefix + "/"
	)
	_, store, _ := testSetup(t, withPrefix(pathPrefix))

	for _, key := range validKeys {
		k, err := store.prepareKey(key)
		if err != nil {
			t.Errorf("key %q should be valid; unexpected error: %v", key, err)
		} else if !strings.HasPrefix(k, expectPrefix) {
			t.Errorf("key %q should have prefix %q", k, expectPrefix)
		}
	}

	for _, key := range invalidKeys {
		_, err := store.prepareKey(key)
		if err == nil {
			t.Errorf("key %q should be invalid", key)
		}
	}
}

func TestInvalidKeys(t *testing.T) {
	const invalidKey = "/foo/bar/../baz"
	expectedError := fmt.Sprintf("invalid key: %q", invalidKey)

	expectInvalidKey := func(methodName string, err error) {
		if err == nil {
			t.Errorf("[%s] expected invalid key error; got nil", methodName)
		} else if err.Error() != expectedError {
			t.Errorf("[%s] expected invalid key error; got %v", methodName, err)
		}
	}

	ctx, store, _ := testSetup(t)
	expectInvalidKey("Create", store.Create(ctx, invalidKey, nil, nil, 0))
	expectInvalidKey("Delete", store.Delete(ctx, invalidKey, nil, nil, nil, nil, storage.DeleteOptions{}))
	_, watchErr := store.Watch(ctx, invalidKey, storage.ListOptions{})
	expectInvalidKey("Watch", watchErr)
	expectInvalidKey("Get", store.Get(ctx, invalidKey, storage.GetOptions{}, nil))
	expectInvalidKey("GetList", store.GetList(ctx, invalidKey, storage.ListOptions{}, nil))
	expectInvalidKey("GuaranteedUpdate", store.GuaranteedUpdate(ctx, invalidKey, nil, true, nil, nil, nil))
	_, countErr := store.Count(invalidKey)
	expectInvalidKey("Count", countErr)
}

func TestResolveGetListRev(t *testing.T) {
	_, store, _ := testSetup(t)
	testCases := []struct {
		name          string
		continueKey   string
		continueRV    int64
		rv            string
		rvMatch       metav1.ResourceVersionMatch
		recursive     bool
		expectedError string
		limit         int64
		expectedRev   int64
	}{
		{
			name:          "specifying resource versionwhen using continue",
			continueKey:   "continue",
			continueRV:    100,
			rv:            "200",
			expectedError: "specifying resource version is not allowed when using continue",
		},
		{
			name:          "invalid resource version",
			rv:            "invalid",
			expectedError: "invalid resource version",
		},
		{
			name:          "unknown ResourceVersionMatch value",
			rv:            "200",
			rvMatch:       "unknown",
			expectedError: "unknown ResourceVersionMatch value",
		},
		{
			name:        "use continueRV",
			continueKey: "continue",
			continueRV:  100,
			rv:          "0",
			expectedRev: 100,
		},
		{
			name:        "use continueRV with empty rv",
			continueKey: "continue",
			continueRV:  100,
			rv:          "",
			expectedRev: 100,
		},
		{
			name:        "continueRV = 0",
			continueKey: "continue",
			continueRV:  0,
			rv:          "",
			expectedRev: 0,
		},
		{
			name:        "continueRV < 0",
			continueKey: "continue",
			continueRV:  -1,
			rv:          "",
			expectedRev: 0,
		},
		{
			name:        "default",
			expectedRev: 0,
		},
		{
			name:        "rev resolve to 0 if ResourceVersionMatchNotOlderThan",
			rv:          "200",
			rvMatch:     metav1.ResourceVersionMatchNotOlderThan,
			expectedRev: 0,
		},
		{
			name:        "specified rev if ResourceVersionMatchExact",
			rv:          "200",
			rvMatch:     metav1.ResourceVersionMatchExact,
			expectedRev: 200,
		},
		{
			name:        "rev resolve to 0 if not recursive",
			rv:          "200",
			limit:       1,
			expectedRev: 0,
		},
		{
			name:        "rev resolve to 0 if limit unspecified",
			rv:          "200",
			recursive:   true,
			expectedRev: 0,
		},
		{
			name:        "specified rev if recursive with limit",
			rv:          "200",
			recursive:   true,
			limit:       1,
			expectedRev: 200,
		},
	}
	for _, tt := range testCases {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			storageOpts := storage.ListOptions{
				ResourceVersion:      tt.rv,
				ResourceVersionMatch: tt.rvMatch,
				Predicate: storage.SelectionPredicate{
					Limit: tt.limit,
				},
				Recursive: tt.recursive,
			}
			rev, err := store.resolveGetListRev(tt.continueKey, tt.continueRV, storageOpts)
			if len(tt.expectedError) > 0 {
				if err == nil || !strings.Contains(err.Error(), tt.expectedError) {
					t.Fatalf("expected error: %s, but got: %v", tt.expectedError, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveRevForGetList failed: %v", err)
			}
			if rev != tt.expectedRev {
				t.Errorf("%s: expecting rev = %d, but get %d", tt.name, tt.expectedRev, rev)
			}
		})
	}
}

func BenchmarkStore_GetList(b *testing.B) {
	generateBigPod := func(index int, total int, expect int) runtime.Object {
		l := map[string]string{}
		if index%(total/expect) == 0 {
			l["foo"] = "bar"
		}
		terminationGracePeriodSeconds := int64(42)
		activeDeadlineSeconds := int64(42)
		pod := &examplev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Labels: l,
			},
			Spec: examplev1.PodSpec{
				RestartPolicy:                 examplev1.RestartPolicy("Always"),
				TerminationGracePeriodSeconds: &terminationGracePeriodSeconds,
				ActiveDeadlineSeconds:         &activeDeadlineSeconds,
				NodeSelector:                  map[string]string{},
				ServiceAccountName:            "demo-sa",
			},
		}
		pod.Name = fmt.Sprintf("object-%d", index)
		data := make([]byte, 1024*2, 1024*2) // 2k labels
		rand.Read(data)
		pod.Spec.NodeSelector["key"] = string(data)
		return pod
	}
	testCases := []struct {
		name              string
		objectNum         int
		expectNum         int
		selector          labels.Selector
		newObjectFunc     func(index int, total int, expect int) runtime.Object
		newListObjectFunc func() runtime.Object
	}{
		{
			name:              "pick 50 pods out of 5000 pod",
			objectNum:         5000,
			expectNum:         50,
			selector:          labels.SelectorFromSet(map[string]string{"foo": "bar"}),
			newObjectFunc:     generateBigPod,
			newListObjectFunc: func() runtime.Object { return &examplev1.PodList{} },
		},
		{
			name:              "pick 500 pods out of 5000 pod",
			objectNum:         5000,
			expectNum:         500,
			selector:          labels.SelectorFromSet(map[string]string{"foo": "bar"}),
			newObjectFunc:     generateBigPod,
			newListObjectFunc: func() runtime.Object { return &examplev1.PodList{} },
		},
		{
			name:              "pick 1000 pods out of 5000 pod",
			objectNum:         5000,
			expectNum:         1000,
			selector:          labels.SelectorFromSet(map[string]string{"foo": "bar"}),
			newObjectFunc:     generateBigPod,
			newListObjectFunc: func() runtime.Object { return &examplev1.PodList{} },
		},
		{
			name:              "pick 2500 pods out of 5000 pod",
			objectNum:         5000,
			expectNum:         2500,
			selector:          labels.SelectorFromSet(map[string]string{"foo": "bar"}),
			newObjectFunc:     generateBigPod,
			newListObjectFunc: func() runtime.Object { return &examplev1.PodList{} },
		},
		{
			name:              "pick 5000 pods out of 5000 pod",
			objectNum:         5000,
			expectNum:         5000,
			selector:          labels.SelectorFromSet(map[string]string{"foo": "bar"}),
			newObjectFunc:     generateBigPod,
			newListObjectFunc: func() runtime.Object { return &examplev1.PodList{} },
		},
	}
	for _, tc := range testCases {
		b.Run(tc.name, func(b *testing.B) {
			// booting etcd instance
			ctx, store, etcdClient := testSetup(b)
			defer etcdClient.Close()

			// make fake objects..
			dir := "/testing"
			originalRevision := ""
			for i := 0; i < tc.objectNum; i++ {
				obj := tc.newObjectFunc(i, tc.objectNum, tc.expectNum)
				o := obj.(metav1.Object)
				key := fmt.Sprintf("/testing/testkey/%s", o.GetName())
				out := tc.newObjectFunc(i, tc.objectNum, tc.expectNum)
				if err := store.Create(ctx, key, obj, out, 0); err != nil {
					b.Fatalf("Set failed: %v", err)
				}
				originalRevision = out.(metav1.Object).GetResourceVersion()
			}

			// prepare result and pred
			pred := storage.SelectionPredicate{
				Label: tc.selector,
				Field: fields.Everything(),
				GetAttrs: func(obj runtime.Object) (labels.Set, fields.Set, error) {
					pod, ok := obj.(*examplev1.Pod)
					if !ok {
						return nil, nil, fmt.Errorf("not a pod")
					}
					return pod.ObjectMeta.Labels, fields.Set{
						"metadata.name": pod.Name,
					}, nil
				},
			}

			// now we start benchmarking
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				list := tc.newListObjectFunc()
				if err := store.GetList(ctx, dir, storage.ListOptions{Predicate: pred, Recursive: true}, list); err != nil {
					b.Errorf("Unexpected List error: %v", err)
				}
				listObject := list.(*examplev1.PodList)
				if originalRevision != listObject.GetResourceVersion() {
					b.Fatalf("original revision (%s) did not match final revision after linearized reads (%s)", originalRevision, listObject.GetResourceVersion())
				}
				if len(listObject.Items) != tc.expectNum {
					b.Fatalf("expect (%d) items but got (%d)", tc.expectNum, len(listObject.Items))
				}
			}
		})
	}
}

func BenchmarkStoreListCreate(b *testing.B) {
	klog.SetLogger(logr.Discard())
	b.Run("RV=NotOlderThan", func(b *testing.B) {
		ctx, store, _ := testSetup(b)
		storagetesting.RunBenchmarkStoreListCreate(ctx, b, store, metav1.ResourceVersionMatchNotOlderThan)
	})
	b.Run("RV=ExactMatch", func(b *testing.B) {
		ctx, store, _ := testSetup(b)
		storagetesting.RunBenchmarkStoreListCreate(ctx, b, store, metav1.ResourceVersionMatchExact)
	})
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
			ctx, store, _ := testSetup(b)
			var out example.Pod
			for _, pod := range data.Pods {
				err := store.Create(ctx, computePodKey(pod), pod, &out, 0)
				if err != nil {
					b.Fatal(err)
				}
			}
			storagetesting.RunBenchmarkStoreList(ctx, b, store, data, false)
		})
	}
}

func computePodKey(obj *example.Pod) string {
	return fmt.Sprintf("/pods/%s/%s", obj.Namespace, obj.Name)
}
