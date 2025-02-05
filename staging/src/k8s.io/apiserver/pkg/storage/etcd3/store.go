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
	"bytes"
	"context"
	"fmt"
	"path"
	"reflect"
	"strings"
	"time"

	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/client/v3/kubernetes"
	"go.opentelemetry.io/otel/attribute"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/conversion"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/apiserver/pkg/audit"
	"k8s.io/apiserver/pkg/features"
	"k8s.io/apiserver/pkg/storage"
	"k8s.io/apiserver/pkg/storage/etcd3/metrics"
	etcdfeature "k8s.io/apiserver/pkg/storage/feature"
	"k8s.io/apiserver/pkg/storage/value"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/component-base/tracing"
	"k8s.io/klog/v2"
)

const (
	// maxLimit is a maximum page limit increase used when fetching objects from etcd.
	// This limit is used only for increasing page size by kube-apiserver. If request
	// specifies larger limit initially, it won't be changed.
	maxLimit = 10000
)

// authenticatedDataString satisfies the value.Context interface. It uses the key to
// authenticate the stored data. This does not defend against reuse of previously
// encrypted values under the same key, but will prevent an attacker from using an
// encrypted value from a different key. A stronger authenticated data segment would
// include the etcd3 Version field (which is incremented on each write to a key and
// reset when the key is deleted), but an attacker with write access to etcd can
// force deletion and recreation of keys to weaken that angle.
type authenticatedDataString string

// AuthenticatedData implements the value.Context interface.
func (d authenticatedDataString) AuthenticatedData() []byte {
	return []byte(string(d))
}

var _ value.Context = authenticatedDataString("")

type store struct {
	client              *kubernetes.Client
	codec               runtime.Codec
	versioner           storage.Versioner
	transformer         value.Transformer
	pathPrefix          string
	groupResource       schema.GroupResource
	groupResourceString string
	watcher             *watcher
	leaseManager        *leaseManager
	decoder             Decoder
	listErrAggrFactory  func() storage.ListErrorAggregator
}

func (s *store) RequestWatchProgress(ctx context.Context) error {
	// Use watchContext to match ctx metadata provided when creating the watch.
	// In best case scenario we would use the same context that watch was created, but there is no way access it from watchCache.
	return s.client.RequestProgress(s.watchContext(ctx))
}

type objState struct {
	obj   runtime.Object
	meta  *storage.ResponseMeta
	rev   int64
	data  []byte
	stale bool
}

// defaultListErrorAggregatorFactory returns the default list error
// aggregator that maintains backward compatibility, which is abort
// the list operation as soon as it encounters the first error
func defaultListErrorAggregatorFactory() storage.ListErrorAggregator { return &abortOnFirstError{} }

// LIST aborts on the first error it encounters (backward compatible)
type abortOnFirstError struct {
	err error
}

func (a *abortOnFirstError) Aggregate(key string, err error) bool {
	a.err = err
	return true
}
func (a *abortOnFirstError) Err() error { return a.err }

// New returns an etcd3 implementation of storage.Interface.
func New(c *kubernetes.Client, codec runtime.Codec, newFunc, newListFunc func() runtime.Object, prefix, resourcePrefix string, groupResource schema.GroupResource, transformer value.Transformer, leaseManagerConfig LeaseManagerConfig, decoder Decoder, versioner storage.Versioner) storage.Interface {
	if utilfeature.DefaultFeatureGate.Enabled(features.AllowUnsafeMalformedObjectDeletion) {
		transformer = WithCorruptObjErrorHandlingTransformer(transformer)
		decoder = WithCorruptObjErrorHandlingDecoder(decoder)
	}
	var store storage.Interface
	store = newStore(c, codec, newFunc, newListFunc, prefix, resourcePrefix, groupResource, transformer, leaseManagerConfig, decoder, versioner)
	if utilfeature.DefaultFeatureGate.Enabled(features.AllowUnsafeMalformedObjectDeletion) {
		store = NewStoreWithUnsafeCorruptObjectDeletion(store, groupResource)
	}
	return store
}

func newStore(c *kubernetes.Client, codec runtime.Codec, newFunc, newListFunc func() runtime.Object, prefix, resourcePrefix string, groupResource schema.GroupResource, transformer value.Transformer, leaseManagerConfig LeaseManagerConfig, decoder Decoder, versioner storage.Versioner) *store {
	// for compatibility with etcd2 impl.
	// no-op for default prefix of '/registry'.
	// keeps compatibility with etcd2 impl for custom prefixes that don't start with '/'
	pathPrefix := path.Join("/", prefix)
	if !strings.HasSuffix(pathPrefix, "/") {
		// Ensure the pathPrefix ends in "/" here to simplify key concatenation later.
		pathPrefix += "/"
	}

	listErrAggrFactory := defaultListErrorAggregatorFactory
	if utilfeature.DefaultFeatureGate.Enabled(features.AllowUnsafeMalformedObjectDeletion) {
		listErrAggrFactory = corruptObjErrAggregatorFactory(100)
	}

	w := &watcher{
		client:        c.Client,
		codec:         codec,
		newFunc:       newFunc,
		groupResource: groupResource,
		versioner:     versioner,
		transformer:   transformer,
	}
	if newFunc == nil {
		w.objectType = "<unknown>"
	} else {
		w.objectType = reflect.TypeOf(newFunc()).String()
	}
	s := &store{
		client:              c,
		codec:               codec,
		versioner:           versioner,
		transformer:         transformer,
		pathPrefix:          pathPrefix,
		groupResource:       groupResource,
		groupResourceString: groupResource.String(),
		watcher:             w,
		leaseManager:        newDefaultLeaseManager(c.Client, leaseManagerConfig),
		decoder:             decoder,
		listErrAggrFactory:  listErrAggrFactory,
	}

	w.getCurrentStorageRV = func(ctx context.Context) (uint64, error) {
		return storage.GetCurrentResourceVersionFromStorage(ctx, s, newListFunc, resourcePrefix, w.objectType)
	}
	if utilfeature.DefaultFeatureGate.Enabled(features.ConsistentListFromCache) || utilfeature.DefaultFeatureGate.Enabled(features.WatchList) {
		etcdfeature.DefaultFeatureSupportChecker.CheckClient(c.Ctx(), c, storage.RequestWatchProgress)
	}
	return s
}

// Versioner implements storage.Interface.Versioner.
func (s *store) Versioner() storage.Versioner {
	return s.versioner
}

// Get implements storage.Interface.Get.
func (s *store) Get(ctx context.Context, key string, opts storage.GetOptions, out runtime.Object) error {
	preparedKey, err := s.prepareKey(key)
	if err != nil {
		return err
	}
	startTime := time.Now()
	getResp, err := s.client.Kubernetes.Get(ctx, preparedKey, kubernetes.GetOptions{})
	metrics.RecordEtcdRequest("get", s.groupResourceString, err, startTime)
	if err != nil {
		return err
	}
	if err = s.validateMinimumResourceVersion(opts.ResourceVersion, uint64(getResp.Revision)); err != nil {
		return err
	}

	if getResp.KV == nil {
		if opts.IgnoreNotFound {
			return runtime.SetZeroValue(out)
		}
		return storage.NewKeyNotFoundError(preparedKey, 0)
	}

	data, _, err := s.transformer.TransformFromStorage(ctx, getResp.KV.Value, authenticatedDataString(preparedKey))
	if err != nil {
		return storage.NewInternalError(err)
	}

	err = s.decoder.Decode(data, out, getResp.KV.ModRevision)
	if err != nil {
		recordDecodeError(s.groupResourceString, preparedKey)
		return err
	}
	return nil
}

// Create implements storage.Interface.Create.
func (s *store) Create(ctx context.Context, key string, obj, out runtime.Object, ttl uint64) error {
	preparedKey, err := s.prepareKey(key)
	if err != nil {
		return err
	}
	ctx, span := tracing.Start(ctx, "Create etcd3",
		attribute.String("audit-id", audit.GetAuditIDTruncated(ctx)),
		attribute.String("key", key),
		attribute.String("type", getTypeName(obj)),
		attribute.String("resource", s.groupResourceString),
	)
	defer span.End(500 * time.Millisecond)
	if version, err := s.versioner.ObjectResourceVersion(obj); err == nil && version != 0 {
		return storage.ErrResourceVersionSetOnCreate
	}
	if err := s.versioner.PrepareObjectForStorage(obj); err != nil {
		return fmt.Errorf("PrepareObjectForStorage failed: %v", err)
	}
	span.AddEvent("About to Encode")
	data, err := runtime.Encode(s.codec, obj)
	if err != nil {
		span.AddEvent("Encode failed", attribute.Int("len", len(data)), attribute.String("err", err.Error()))
		return err
	}
	span.AddEvent("Encode succeeded", attribute.Int("len", len(data)))

	var lease clientv3.LeaseID
	if ttl != 0 {
		lease, err = s.leaseManager.GetLease(ctx, int64(ttl))
		if err != nil {
			return err
		}
	}

	newData, err := s.transformer.TransformToStorage(ctx, data, authenticatedDataString(preparedKey))
	if err != nil {
		span.AddEvent("TransformToStorage failed", attribute.String("err", err.Error()))
		return storage.NewInternalError(err)
	}
	span.AddEvent("TransformToStorage succeeded")

	startTime := time.Now()
	txnResp, err := s.client.Kubernetes.OptimisticPut(ctx, preparedKey, newData, 0, kubernetes.PutOptions{LeaseID: lease})
	metrics.RecordEtcdRequest("create", s.groupResourceString, err, startTime)
	if err != nil {
		span.AddEvent("Txn call failed", attribute.String("err", err.Error()))
		return err
	}
	span.AddEvent("Txn call succeeded")

	if !txnResp.Succeeded {
		return storage.NewKeyExistsError(preparedKey, 0)
	}

	if out != nil {
		err = s.decoder.Decode(data, out, txnResp.Revision)
		if err != nil {
			span.AddEvent("decode failed", attribute.Int("len", len(data)), attribute.String("err", err.Error()))
			recordDecodeError(s.groupResourceString, preparedKey)
			return err
		}
		span.AddEvent("decode succeeded", attribute.Int("len", len(data)))
	}
	return nil
}

// Delete implements storage.Interface.Delete.
func (s *store) Delete(
	ctx context.Context, key string, out runtime.Object, preconditions *storage.Preconditions,
	validateDeletion storage.ValidateObjectFunc, cachedExistingObject runtime.Object, opts storage.DeleteOptions) error {
	preparedKey, err := s.prepareKey(key)
	if err != nil {
		return err
	}
	v, err := conversion.EnforcePtr(out)
	if err != nil {
		return fmt.Errorf("unable to convert output object to pointer: %v", err)
	}

	skipTransformDecode := false
	if utilfeature.DefaultFeatureGate.Enabled(features.AllowUnsafeMalformedObjectDeletion) {
		skipTransformDecode = opts.IgnoreStoreReadError
	}
	return s.conditionalDelete(ctx, preparedKey, out, v, preconditions, validateDeletion, cachedExistingObject, skipTransformDecode)
}

func (s *store) conditionalDelete(
	ctx context.Context, key string, out runtime.Object, v reflect.Value, preconditions *storage.Preconditions,
	validateDeletion storage.ValidateObjectFunc, cachedExistingObject runtime.Object, skipTransformDecode bool) error {
	getCurrentState := s.getCurrentState(ctx, key, v, false, skipTransformDecode)

	var origState *objState
	var err error
	var origStateIsCurrent bool
	if cachedExistingObject != nil {
		origState, err = s.getStateFromObject(cachedExistingObject)
	} else {
		origState, err = getCurrentState()
		origStateIsCurrent = true
	}
	if err != nil {
		return err
	}

	for {
		if preconditions != nil {
			if err := preconditions.Check(key, origState.obj); err != nil {
				if origStateIsCurrent {
					return err
				}

				// It's possible we're working with stale data.
				// Remember the revision of the potentially stale data and the resulting update error
				cachedRev := origState.rev
				cachedUpdateErr := err

				// Actually fetch
				origState, err = getCurrentState()
				if err != nil {
					return err
				}
				origStateIsCurrent = true

				// it turns out our cached data was not stale, return the error
				if cachedRev == origState.rev {
					return cachedUpdateErr
				}

				// Retry
				continue
			}
		}
		if err := validateDeletion(ctx, origState.obj); err != nil {
			if origStateIsCurrent {
				return err
			}

			// It's possible we're working with stale data.
			// Remember the revision of the potentially stale data and the resulting update error
			cachedRev := origState.rev
			cachedUpdateErr := err

			// Actually fetch
			origState, err = getCurrentState()
			if err != nil {
				return err
			}
			origStateIsCurrent = true

			// it turns out our cached data was not stale, return the error
			if cachedRev == origState.rev {
				return cachedUpdateErr
			}

			// Retry
			continue
		}

		startTime := time.Now()
		txnResp, err := s.client.Kubernetes.OptimisticDelete(ctx, key, origState.rev, kubernetes.DeleteOptions{
			GetOnFailure: true,
		})
		metrics.RecordEtcdRequest("delete", s.groupResourceString, err, startTime)
		if err != nil {
			return err
		}
		if !txnResp.Succeeded {
			klog.V(4).Infof("deletion of %s failed because of a conflict, going to retry", key)
			origState, err = s.getState(ctx, txnResp.KV, key, v, false, skipTransformDecode)
			if err != nil {
				return err
			}
			origStateIsCurrent = true
			continue
		}

		if !skipTransformDecode {
			err = s.decoder.Decode(origState.data, out, txnResp.Revision)
			if err != nil {
				recordDecodeError(s.groupResourceString, key)
				return err
			}
		}
		return nil
	}
}

// GuaranteedUpdate implements storage.Interface.GuaranteedUpdate.
func (s *store) GuaranteedUpdate(
	ctx context.Context, key string, destination runtime.Object, ignoreNotFound bool,
	preconditions *storage.Preconditions, tryUpdate storage.UpdateFunc, cachedExistingObject runtime.Object) error {
	preparedKey, err := s.prepareKey(key)
	if err != nil {
		return err
	}
	ctx, span := tracing.Start(ctx, "GuaranteedUpdate etcd3",
		attribute.String("audit-id", audit.GetAuditIDTruncated(ctx)),
		attribute.String("key", key),
		attribute.String("type", getTypeName(destination)),
		attribute.String("resource", s.groupResourceString))
	defer span.End(500 * time.Millisecond)

	v, err := conversion.EnforcePtr(destination)
	if err != nil {
		return fmt.Errorf("unable to convert output object to pointer: %v", err)
	}

	skipTransformDecode := false
	getCurrentState := s.getCurrentState(ctx, preparedKey, v, ignoreNotFound, skipTransformDecode)

	var origState *objState
	var origStateIsCurrent bool
	if cachedExistingObject != nil {
		origState, err = s.getStateFromObject(cachedExistingObject)
	} else {
		origState, err = getCurrentState()
		origStateIsCurrent = true
	}
	if err != nil {
		return err
	}
	span.AddEvent("initial value restored")

	transformContext := authenticatedDataString(preparedKey)
	for {
		if err := preconditions.Check(preparedKey, origState.obj); err != nil {
			// If our data is already up to date, return the error
			if origStateIsCurrent {
				return err
			}

			// It's possible we were working with stale data
			// Actually fetch
			origState, err = getCurrentState()
			if err != nil {
				return err
			}
			origStateIsCurrent = true
			// Retry
			continue
		}

		ret, ttl, err := s.updateState(origState, tryUpdate)
		if err != nil {
			// If our data is already up to date, return the error
			if origStateIsCurrent {
				return err
			}

			// It's possible we were working with stale data
			// Remember the revision of the potentially stale data and the resulting update error
			cachedRev := origState.rev
			cachedUpdateErr := err

			// Actually fetch
			origState, err = getCurrentState()
			if err != nil {
				return err
			}
			origStateIsCurrent = true

			// it turns out our cached data was not stale, return the error
			if cachedRev == origState.rev {
				return cachedUpdateErr
			}

			// Retry
			continue
		}

		span.AddEvent("About to Encode")
		data, err := runtime.Encode(s.codec, ret)
		if err != nil {
			span.AddEvent("Encode failed", attribute.Int("len", len(data)), attribute.String("err", err.Error()))
			return err
		}
		span.AddEvent("Encode succeeded", attribute.Int("len", len(data)))
		if !origState.stale && bytes.Equal(data, origState.data) {
			// if we skipped the original Get in this loop, we must refresh from
			// etcd in order to be sure the data in the store is equivalent to
			// our desired serialization
			if !origStateIsCurrent {
				origState, err = getCurrentState()
				if err != nil {
					return err
				}
				origStateIsCurrent = true
				if !bytes.Equal(data, origState.data) {
					// original data changed, restart loop
					continue
				}
			}
			// recheck that the data from etcd is not stale before short-circuiting a write
			if !origState.stale {
				err = s.decoder.Decode(origState.data, destination, origState.rev)
				if err != nil {
					recordDecodeError(s.groupResourceString, preparedKey)
					return err
				}
				return nil
			}
		}

		newData, err := s.transformer.TransformToStorage(ctx, data, transformContext)
		if err != nil {
			span.AddEvent("TransformToStorage failed", attribute.String("err", err.Error()))
			return storage.NewInternalError(err)
		}
		span.AddEvent("TransformToStorage succeeded")

		var lease clientv3.LeaseID
		if ttl != 0 {
			lease, err = s.leaseManager.GetLease(ctx, int64(ttl))
			if err != nil {
				return err
			}
		}
		span.AddEvent("Transaction prepared")

		startTime := time.Now()

		txnResp, err := s.client.Kubernetes.OptimisticPut(ctx, preparedKey, newData, origState.rev, kubernetes.PutOptions{
			GetOnFailure: true,
			LeaseID:      lease,
		})
		metrics.RecordEtcdRequest("update", s.groupResourceString, err, startTime)
		if err != nil {
			span.AddEvent("Txn call failed", attribute.String("err", err.Error()))
			return err
		}
		span.AddEvent("Txn call completed")
		span.AddEvent("Transaction committed")
		if !txnResp.Succeeded {
			klog.V(4).Infof("GuaranteedUpdate of %s failed because of a conflict, going to retry", preparedKey)
			origState, err = s.getState(ctx, txnResp.KV, preparedKey, v, ignoreNotFound, skipTransformDecode)
			if err != nil {
				return err
			}
			span.AddEvent("Retry value restored")
			origStateIsCurrent = true
			continue
		}

		err = s.decoder.Decode(data, destination, txnResp.Revision)
		if err != nil {
			span.AddEvent("decode failed", attribute.Int("len", len(data)), attribute.String("err", err.Error()))
			recordDecodeError(s.groupResourceString, preparedKey)
			return err
		}
		span.AddEvent("decode succeeded", attribute.Int("len", len(data)))
		return nil
	}
}

func getNewItemFunc(listObj runtime.Object, v reflect.Value) func() runtime.Object {
	// For unstructured lists with a target group/version, preserve the group/version in the instantiated list items
	if unstructuredList, isUnstructured := listObj.(*unstructured.UnstructuredList); isUnstructured {
		if apiVersion := unstructuredList.GetAPIVersion(); len(apiVersion) > 0 {
			return func() runtime.Object {
				return &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": apiVersion}}
			}
		}
	}

	// Otherwise just instantiate an empty item
	elem := v.Type().Elem()
	return func() runtime.Object {
		return reflect.New(elem).Interface().(runtime.Object)
	}
}

func (s *store) Count(key string) (int64, error) {
	preparedKey, err := s.prepareKey(key)
	if err != nil {
		return 0, err
	}

	// We need to make sure the key ended with "/" so that we only get children "directories".
	// e.g. if we have key "/a", "/a/b", "/ab", getting keys with prefix "/a" will return all three,
	// while with prefix "/a/" will return only "/a/b" which is the correct answer.
	if !strings.HasSuffix(preparedKey, "/") {
		preparedKey += "/"
	}

	startTime := time.Now()
	count, err := s.client.Kubernetes.Count(context.Background(), preparedKey, kubernetes.CountOptions{})
	metrics.RecordEtcdRequest("listWithCount", preparedKey, err, startTime)
	if err != nil {
		return 0, err
	}
	return count, nil
}

// ReadinessCheck implements storage.Interface.
func (s *store) ReadinessCheck() error {
	return nil
}

// resolveGetListRev is used by GetList to resolve the rev to use in the client.KV.Get request.
func (s *store) resolveGetListRev(continueKey string, continueRV int64, opts storage.ListOptions) (int64, error) {
	var withRev int64
	// Uses continueRV if this is a continuation request.
	if len(continueKey) > 0 {
		if len(opts.ResourceVersion) > 0 && opts.ResourceVersion != "0" {
			return withRev, apierrors.NewBadRequest("specifying resource version is not allowed when using continue")
		}
		// If continueRV > 0, the LIST request needs a specific resource version.
		// continueRV==0 is invalid.
		// If continueRV < 0, the request is for the latest resource version.
		if continueRV > 0 {
			withRev = continueRV
		}
		return withRev, nil
	}
	// Returns 0 if ResourceVersion is not specified.
	if len(opts.ResourceVersion) == 0 {
		return withRev, nil
	}
	parsedRV, err := s.versioner.ParseResourceVersion(opts.ResourceVersion)
	if err != nil {
		return withRev, apierrors.NewBadRequest(fmt.Sprintf("invalid resource version: %v", err))
	}

	switch opts.ResourceVersionMatch {
	case metav1.ResourceVersionMatchNotOlderThan:
		// The not older than constraint is checked after we get a response from etcd,
		// and returnedRV is then set to the revision we get from the etcd response.
	case metav1.ResourceVersionMatchExact:
		withRev = int64(parsedRV)
	case "": // legacy case
		if opts.Recursive && opts.Predicate.Limit > 0 && parsedRV > 0 {
			withRev = int64(parsedRV)
		}
	default:
		return withRev, fmt.Errorf("unknown ResourceVersionMatch value: %v", opts.ResourceVersionMatch)
	}
	return withRev, nil
}

// GetList implements storage.Interface.
func (s *store) GetList(ctx context.Context, key string, opts storage.ListOptions, listObj runtime.Object) error {
	keyPrefix, err := s.prepareKey(key)
	if err != nil {
		return err
	}
	ctx, span := tracing.Start(ctx, fmt.Sprintf("List(recursive=%v) etcd3", opts.Recursive),
		attribute.String("audit-id", audit.GetAuditIDTruncated(ctx)),
		attribute.String("key", key),
		attribute.String("resourceVersion", opts.ResourceVersion),
		attribute.String("resourceVersionMatch", string(opts.ResourceVersionMatch)),
		attribute.Int("limit", int(opts.Predicate.Limit)),
		attribute.String("continue", opts.Predicate.Continue))
	defer span.End(500 * time.Millisecond)
	listPtr, err := meta.GetItemsPtr(listObj)
	if err != nil {
		return err
	}
	v, err := conversion.EnforcePtr(listPtr)
	if err != nil || v.Kind() != reflect.Slice {
		return fmt.Errorf("need ptr to slice: %v", err)
	}

	// For recursive lists, we need to make sure the key ended with "/" so that we only
	// get children "directories". e.g. if we have key "/a", "/a/b", "/ab", getting keys
	// with prefix "/a" will return all three, while with prefix "/a/" will return only
	// "/a/b" which is the correct answer.
	if opts.Recursive && !strings.HasSuffix(keyPrefix, "/") {
		keyPrefix += "/"
	}

	// set the appropriate clientv3 options to filter the returned data set
	limit := opts.Predicate.Limit
	paging := opts.Predicate.Limit > 0
	newItemFunc := getNewItemFunc(listObj, v)

	var continueRV, withRev int64
	var continueKey string
	if opts.Recursive && len(opts.Predicate.Continue) > 0 {
		continueKey, continueRV, err = storage.DecodeContinue(opts.Predicate.Continue, keyPrefix)
		if err != nil {
			return apierrors.NewBadRequest(fmt.Sprintf("invalid continue token: %v", err))
		}
	}
	if withRev, err = s.resolveGetListRev(continueKey, continueRV, opts); err != nil {
		return err
	}

	// loop until we have filled the requested limit from etcd or there are no more results
	var lastKey []byte
	var hasMore bool
	var getResp kubernetes.ListResponse
	var numFetched int
	var numEvald int
	// Because these metrics are for understanding the costs of handling LIST requests,
	// get them recorded even in error cases.
	defer func() {
		numReturn := v.Len()
		metrics.RecordStorageListMetrics(s.groupResourceString, numFetched, numEvald, numReturn)
	}()

	metricsOp := "get"
	if opts.Recursive {
		metricsOp = "list"
	}

	aggregator := s.listErrAggrFactory()
	for {
		startTime := time.Now()
		getResp, err = s.getList(ctx, keyPrefix, opts.Recursive, kubernetes.ListOptions{
			Revision: withRev,
			Limit:    limit,
			Continue: continueKey,
		})
		metrics.RecordEtcdRequest(metricsOp, s.groupResourceString, err, startTime)
		if err != nil {
			return interpretListError(err, len(opts.Predicate.Continue) > 0, continueKey, keyPrefix)
		}
		numFetched += len(getResp.Kvs)
		if err = s.validateMinimumResourceVersion(opts.ResourceVersion, uint64(getResp.Revision)); err != nil {
			return err
		}
		hasMore = int64(len(getResp.Kvs)) < getResp.Count

		if len(getResp.Kvs) == 0 && hasMore {
			return fmt.Errorf("no results were found, but etcd indicated there were more values remaining")
		}
		// indicate to the client which resource version was returned, and use the same resource version for subsequent requests.
		if withRev == 0 {
			withRev = getResp.Revision
		}

		// avoid small allocations for the result slice, since this can be called in many
		// different contexts and we don't know how significantly the result will be filtered
		if opts.Predicate.Empty() {
			growSlice(v, len(getResp.Kvs))
		} else {
			growSlice(v, 2048, len(getResp.Kvs))
		}

		// take items from the response until the bucket is full, filtering as we go
		for i, kv := range getResp.Kvs {
			if paging && int64(v.Len()) >= opts.Predicate.Limit {
				hasMore = true
				break
			}
			lastKey = kv.Key

			data, _, err := s.transformer.TransformFromStorage(ctx, kv.Value, authenticatedDataString(kv.Key))
			if err != nil {
				if done := aggregator.Aggregate(string(kv.Key), storage.NewInternalError(fmt.Errorf("unable to transform key %q: %w", kv.Key, err))); done {
					return aggregator.Err()
				}
				continue
			}

			// Check if the request has already timed out before decode object
			select {
			case <-ctx.Done():
				// parent context is canceled or timed out, no point in continuing
				return storage.NewTimeoutError(string(kv.Key), "request did not complete within requested timeout")
			default:
			}

			obj, err := s.decoder.DecodeListItem(ctx, data, uint64(kv.ModRevision), newItemFunc)
			if err != nil {
				recordDecodeError(s.groupResourceString, string(kv.Key))
				if done := aggregator.Aggregate(string(kv.Key), err); done {
					return aggregator.Err()
				}
				continue
			}

			// being unable to set the version does not prevent the object from being extracted
			if matched, err := opts.Predicate.Matches(obj); err == nil && matched {
				v.Set(reflect.Append(v, reflect.ValueOf(obj).Elem()))
			}

			numEvald++

			// free kv early. Long lists can take O(seconds) to decode.
			getResp.Kvs[i] = nil
		}
		continueKey = string(lastKey) + "\x00"

		// no more results remain or we didn't request paging
		if !hasMore || !paging {
			break
		}
		// we're paging but we have filled our bucket
		if int64(v.Len()) >= opts.Predicate.Limit {
			break
		}

		if limit < maxLimit {
			// We got incomplete result due to field/label selector dropping the object.
			// Double page size to reduce total number of calls to etcd.
			limit *= 2
			if limit > maxLimit {
				limit = maxLimit
			}
		}
	}

	if err := aggregator.Err(); err != nil {
		return err
	}

	if v.IsNil() {
		// Ensure that we never return a nil Items pointer in the result for consistency.
		v.Set(reflect.MakeSlice(v.Type(), 0, 0))
	}

	continueValue, remainingItemCount, err := storage.PrepareContinueToken(string(lastKey), keyPrefix, withRev, getResp.Count, hasMore, opts)
	if err != nil {
		return err
	}
	return s.versioner.UpdateList(listObj, uint64(withRev), continueValue, remainingItemCount)
}

func (s *store) getList(ctx context.Context, keyPrefix string, recursive bool, options kubernetes.ListOptions) (kubernetes.ListResponse, error) {
	if recursive {
		return s.client.Kubernetes.List(ctx, keyPrefix, options)
	}
	getResp, err := s.client.Kubernetes.Get(ctx, keyPrefix, kubernetes.GetOptions{
		Revision: options.Revision,
	})
	var resp kubernetes.ListResponse
	if getResp.KV != nil {
		resp.Kvs = []*mvccpb.KeyValue{getResp.KV}
		resp.Count = 1
		resp.Revision = getResp.Revision
	} else {
		resp.Kvs = []*mvccpb.KeyValue{}
		resp.Count = 0
		resp.Revision = getResp.Revision
	}
	return resp, err
}

// growSlice takes a slice value and grows its capacity up
// to the maximum of the passed sizes or maxCapacity, whichever
// is smaller. Above maxCapacity decisions about allocation are left
// to the Go runtime on append. This allows a caller to make an
// educated guess about the potential size of the total list while
// still avoiding overly aggressive initial allocation. If sizes
// is empty maxCapacity will be used as the size to grow.
func growSlice(v reflect.Value, maxCapacity int, sizes ...int) {
	cap := v.Cap()
	max := cap
	for _, size := range sizes {
		if size > max {
			max = size
		}
	}
	if len(sizes) == 0 || max > maxCapacity {
		max = maxCapacity
	}
	if max <= cap {
		return
	}
	if v.Len() > 0 {
		extra := reflect.MakeSlice(v.Type(), v.Len(), max)
		reflect.Copy(extra, v)
		v.Set(extra)
	} else {
		extra := reflect.MakeSlice(v.Type(), 0, max)
		v.Set(extra)
	}
}

// Watch implements storage.Interface.Watch.
func (s *store) Watch(ctx context.Context, key string, opts storage.ListOptions) (watch.Interface, error) {
	preparedKey, err := s.prepareKey(key)
	if err != nil {
		return nil, err
	}
	rev, err := s.versioner.ParseResourceVersion(opts.ResourceVersion)
	if err != nil {
		return nil, err
	}
	return s.watcher.Watch(s.watchContext(ctx), preparedKey, int64(rev), opts)
}

func (s *store) watchContext(ctx context.Context) context.Context {
	// The etcd server waits until it cannot find a leader for 3 election
	// timeouts to cancel existing streams. 3 is currently a hard coded
	// constant. The election timeout defaults to 1000ms. If the cluster is
	// healthy, when the leader is stopped, the leadership transfer should be
	// smooth. (leader transfers its leadership before stopping). If leader is
	// hard killed, other servers will take an election timeout to realize
	// leader lost and start campaign.
	return clientv3.WithRequireLeader(ctx)
}

func (s *store) getCurrentState(ctx context.Context, key string, v reflect.Value, ignoreNotFound bool, skipTransformDecode bool) func() (*objState, error) {
	return func() (*objState, error) {
		startTime := time.Now()
		getResp, err := s.client.Kubernetes.Get(ctx, key, kubernetes.GetOptions{})
		metrics.RecordEtcdRequest("get", s.groupResourceString, err, startTime)
		if err != nil {
			return nil, err
		}
		return s.getState(ctx, getResp.KV, key, v, ignoreNotFound, skipTransformDecode)
	}
}

// getState constructs a new objState from the given response from the storage.
// skipTransformDecode: if true, the function will neither transform the data
// from the storage nor decode it into an object; otherwise, data from the
// storage will be transformed and decoded.
// NOTE: when skipTransformDecode is true, the 'data', and the 'obj' fields
// of the objState will be nil, and 'stale' will be set to true.
func (s *store) getState(ctx context.Context, kv *mvccpb.KeyValue, key string, v reflect.Value, ignoreNotFound bool, skipTransformDecode bool) (*objState, error) {
	state := &objState{
		meta: &storage.ResponseMeta{},
	}

	if u, ok := v.Addr().Interface().(runtime.Unstructured); ok {
		state.obj = u.NewEmptyInstance()
	} else {
		state.obj = reflect.New(v.Type()).Interface().(runtime.Object)
	}

	if kv == nil {
		if !ignoreNotFound {
			return nil, storage.NewKeyNotFoundError(key, 0)
		}
		if err := runtime.SetZeroValue(state.obj); err != nil {
			return nil, err
		}
	} else {
		state.rev = kv.ModRevision
		state.meta.ResourceVersion = uint64(state.rev)

		if skipTransformDecode {
			// be explicit that we don't have the object
			state.obj = nil
			state.stale = true // this seems a more sane value here
			return state, nil
		}

		data, stale, err := s.transformer.TransformFromStorage(ctx, kv.Value, authenticatedDataString(key))
		if err != nil {
			return nil, storage.NewInternalError(err)
		}

		state.data = data
		state.stale = stale

		if err := s.decoder.Decode(state.data, state.obj, state.rev); err != nil {
			recordDecodeError(s.groupResourceString, key)
			return nil, err
		}
	}
	return state, nil
}

func (s *store) getStateFromObject(obj runtime.Object) (*objState, error) {
	state := &objState{
		obj:  obj,
		meta: &storage.ResponseMeta{},
	}

	rv, err := s.versioner.ObjectResourceVersion(obj)
	if err != nil {
		return nil, fmt.Errorf("couldn't get resource version: %v", err)
	}
	state.rev = int64(rv)
	state.meta.ResourceVersion = uint64(state.rev)

	// Compute the serialized form - for that we need to temporarily clean
	// its resource version field (those are not stored in etcd).
	if err := s.versioner.PrepareObjectForStorage(obj); err != nil {
		return nil, fmt.Errorf("PrepareObjectForStorage failed: %v", err)
	}
	state.data, err = runtime.Encode(s.codec, obj)
	if err != nil {
		return nil, err
	}
	if err := s.versioner.UpdateObject(state.obj, uint64(rv)); err != nil {
		klog.Errorf("failed to update object version: %v", err)
	}
	return state, nil
}

func (s *store) updateState(st *objState, userUpdate storage.UpdateFunc) (runtime.Object, uint64, error) {
	ret, ttlPtr, err := userUpdate(st.obj, *st.meta)
	if err != nil {
		return nil, 0, err
	}

	if err := s.versioner.PrepareObjectForStorage(ret); err != nil {
		return nil, 0, fmt.Errorf("PrepareObjectForStorage failed: %v", err)
	}
	var ttl uint64
	if ttlPtr != nil {
		ttl = *ttlPtr
	}
	return ret, ttl, nil
}

// validateMinimumResourceVersion returns a 'too large resource' version error when the provided minimumResourceVersion is
// greater than the most recent actualRevision available from storage.
func (s *store) validateMinimumResourceVersion(minimumResourceVersion string, actualRevision uint64) error {
	if minimumResourceVersion == "" {
		return nil
	}
	minimumRV, err := s.versioner.ParseResourceVersion(minimumResourceVersion)
	if err != nil {
		return apierrors.NewBadRequest(fmt.Sprintf("invalid resource version: %v", err))
	}
	// Enforce the storage.Interface guarantee that the resource version of the returned data
	// "will be at least 'resourceVersion'".
	if minimumRV > actualRevision {
		return storage.NewTooLargeResourceVersionError(minimumRV, actualRevision, 0)
	}
	return nil
}

func (s *store) prepareKey(key string) (string, error) {
	if key == ".." ||
		strings.HasPrefix(key, "../") ||
		strings.HasSuffix(key, "/..") ||
		strings.Contains(key, "/../") {
		return "", fmt.Errorf("invalid key: %q", key)
	}
	if key == "." ||
		strings.HasPrefix(key, "./") ||
		strings.HasSuffix(key, "/.") ||
		strings.Contains(key, "/./") {
		return "", fmt.Errorf("invalid key: %q", key)
	}
	if key == "" || key == "/" {
		return "", fmt.Errorf("empty key: %q", key)
	}
	// We ensured that pathPrefix ends in '/' in construction, so skip any leading '/' in the key now.
	startIndex := 0
	if key[0] == '/' {
		startIndex = 1
	}
	return s.pathPrefix + key[startIndex:], nil
}

// recordDecodeError record decode error split by object type.
func recordDecodeError(resource string, key string) {
	metrics.RecordDecodeError(resource)
	klog.V(4).Infof("Decoding %s \"%s\" failed", resource, key)
}

// getTypeName returns type name of an object for reporting purposes.
func getTypeName(obj interface{}) string {
	return reflect.TypeOf(obj).String()
}
