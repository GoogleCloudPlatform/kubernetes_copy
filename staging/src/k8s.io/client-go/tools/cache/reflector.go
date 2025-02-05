/*
Copyright 2014 The Kubernetes Authors.

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

package cache

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"reflect"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/naming"
	utilnet "k8s.io/apimachinery/pkg/util/net"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	clientfeatures "k8s.io/client-go/features"
	"k8s.io/client-go/tools/pager"
	"k8s.io/klog/v2"
	"k8s.io/utils/clock"
	"k8s.io/utils/pointer"
	"k8s.io/utils/ptr"
	"k8s.io/utils/trace"
)

const defaultExpectedTypeName = "<unspecified>"

var (
	// We try to spread the load on apiserver by setting timeouts for
	// watch requests - it is random in [minWatchTimeout, 2*minWatchTimeout].
	defaultMinWatchTimeout = 5 * time.Minute
)

// ReflectorStore is the subset of cache.Store that the reflector uses
type ReflectorStore interface {
	// Add adds the given object to the accumulator associated with the given object's key
	Add(obj interface{}) error

	// Update updates the given object in the accumulator associated with the given object's key
	Update(obj interface{}) error

	// Delete deletes the given object from the accumulator associated with the given object's key
	Delete(obj interface{}) error

	// Replace will delete the contents of the store, using instead the
	// given list. Store takes ownership of the list, you should not reference
	// it after calling this function.
	Replace([]interface{}, string) error

	// Resync is meaningless in the terms appearing here but has
	// meaning in some implementations that have non-trivial
	// additional behavior (e.g., DeltaFIFO).
	Resync() error
}

// Reflector watches a specified resource and causes all changes to be reflected in the given store.
type Reflector struct {
	// name identifies this reflector. By default, it will be a file:line if possible.
	name string
	// The name of the type we expect to place in the store. The name
	// will be the stringification of expectedGVK if provided, and the
	// stringification of expectedType otherwise. It is for display
	// only, and should not be used for parsing or comparison.
	typeDescription string
	// An example object of the type we expect to place in the store.
	// Only the type needs to be right, except that when that is
	// `unstructured.Unstructured` the object's `"apiVersion"` and
	// `"kind"` must also be right.
	expectedType reflect.Type
	// The GVK of the object we expect to place in the store if unstructured.
	expectedGVK *schema.GroupVersionKind
	// The destination to sync up with the watch source
	store ReflectorStore
	// listerWatcher is used to perform lists and watches.
	listerWatcher ListerWatcherWithContext
	// backoff manages backoff of ListWatch
	backoffManager wait.BackoffManager
	resyncPeriod   time.Duration
	// minWatchTimeout defines the minimum timeout for watch requests.
	minWatchTimeout time.Duration
	// clock allows tests to manipulate time
	clock clock.Clock
	// paginatedResult defines whether pagination should be forced for list calls.
	// It is set based on the result of the initial list call.
	paginatedResult bool
	// lastSyncResourceVersion is the resource version token last
	// observed when doing a sync with the underlying store
	// it is thread safe, but not synchronized with the underlying store
	lastSyncResourceVersion string
	// isLastSyncResourceVersionUnavailable is true if the previous list or watch request with
	// lastSyncResourceVersion failed with an "expired" or "too large resource version" error.
	isLastSyncResourceVersionUnavailable bool
	// lastSyncResourceVersionMutex guards read/write access to lastSyncResourceVersion
	lastSyncResourceVersionMutex sync.RWMutex
	// Called whenever the ListAndWatch drops the connection with an error.
	watchErrorHandler WatchErrorHandlerWithContext
	// WatchListPageSize is the requested chunk size of initial and resync watch lists.
	// If unset, for consistent reads (RV="") or reads that opt-into arbitrarily old data
	// (RV="0") it will default to pager.PageSize, for the rest (RV != "" && RV != "0")
	// it will turn off pagination to allow serving them from watch cache.
	// NOTE: It should be used carefully as paginated lists are always served directly from
	// etcd, which is significantly less efficient and may lead to serious performance and
	// scalability problems.
	WatchListPageSize int64
	// ShouldResync is invoked periodically and whenever it returns `true` the Store's Resync operation is invoked
	ShouldResync func() bool
	// MaxInternalErrorRetryDuration defines how long we should retry internal errors returned by watch.
	MaxInternalErrorRetryDuration time.Duration
	// UseWatchList if turned on instructs the reflector to open a stream to bring data from the API server.
	// Streaming has the primary advantage of using fewer server's resources to fetch data.
	//
	// The old behaviour establishes a LIST request which gets data in chunks.
	// Paginated list is less efficient and depending on the actual size of objects
	// might result in an increased memory consumption of the APIServer.
	//
	// See https://github.com/kubernetes/enhancements/tree/master/keps/sig-api-machinery/3157-watch-list#design-details
	//
	// TODO(#115478): Consider making reflector.UseWatchList a private field. Since we implemented "api streaming" on the etcd storage layer it should work.
	UseWatchList *bool
}

func (r *Reflector) Name() string {
	return r.name
}

func (r *Reflector) TypeDescription() string {
	return r.typeDescription
}

// ResourceVersionUpdater is an interface that allows store implementation to
// track the current resource version of the reflector. This is especially
// important if storage bookmarks are enabled.
type ResourceVersionUpdater interface {
	// UpdateResourceVersion is called each time current resource version of the reflector
	// is updated.
	UpdateResourceVersion(resourceVersion string)
}

// The WatchErrorHandler is called whenever ListAndWatch drops the
// connection with an error. After calling this handler, the informer
// will backoff and retry.
//
// The default implementation looks at the error type and tries to log
// the error message at an appropriate level.
//
// Implementations of this handler may display the error message in other
// ways. Implementations should return quickly - any expensive processing
// should be offloaded.
type WatchErrorHandler func(r *Reflector, err error)

// The WatchErrorHandler is called whenever ListAndWatch drops the
// connection with an error. After calling this handler, the informer
// will backoff and retry.
//
// The default implementation looks at the error type and tries to log
// the error message at an appropriate level.
//
// Implementations of this handler may display the error message in other
// ways. Implementations should return quickly - any expensive processing
// should be offloaded.
type WatchErrorHandlerWithContext func(ctx context.Context, r *Reflector, err error)

// DefaultWatchErrorHandler is the default implementation of WatchErrorHandlerWithContext.
func DefaultWatchErrorHandler(ctx context.Context, r *Reflector, err error) {
	switch {
	case isExpiredError(err):
		// Don't set LastSyncResourceVersionUnavailable - LIST call with ResourceVersion=RV already
		// has a semantic that it returns data at least as fresh as provided RV.
		// So first try to LIST with setting RV to resource version of last observed object.
		klog.FromContext(ctx).V(4).Info("Watch closed", "reflector", r.name, "type", r.typeDescription, "err", err)
	case err == io.EOF:
		// watch closed normally
	case err == io.ErrUnexpectedEOF:
		klog.FromContext(ctx).V(1).Info("Watch closed with unexpected EOF", "reflector", r.name, "type", r.typeDescription, "err", err)
	default:
		utilruntime.HandleErrorWithContext(ctx, err, "Failed to watch", "reflector", r.name, "type", r.typeDescription)
	}
}

// NewNamespaceKeyedIndexerAndReflector creates an Indexer and a Reflector
// The indexer is configured to key on namespace
func NewNamespaceKeyedIndexerAndReflector(lw ListerWatcher, expectedType interface{}, resyncPeriod time.Duration) (indexer Indexer, reflector *Reflector) {
	indexer = NewIndexer(MetaNamespaceKeyFunc, Indexers{NamespaceIndex: MetaNamespaceIndexFunc})
	reflector = NewReflector(lw, expectedType, indexer, resyncPeriod)
	return indexer, reflector
}

// NewReflector creates a new Reflector with its name defaulted to the closest source_file.go:line in the call stack
// that is outside this package. See NewReflectorWithOptions for further information.
func NewReflector(lw ListerWatcher, expectedType interface{}, store ReflectorStore, resyncPeriod time.Duration) *Reflector {
	return NewReflectorWithOptions(lw, expectedType, store, ReflectorOptions{ResyncPeriod: resyncPeriod})
}

// NewNamedReflector creates a new Reflector with the specified name. See NewReflectorWithOptions for further
// information.
func NewNamedReflector(name string, lw ListerWatcher, expectedType interface{}, store ReflectorStore, resyncPeriod time.Duration) *Reflector {
	return NewReflectorWithOptions(lw, expectedType, store, ReflectorOptions{Name: name, ResyncPeriod: resyncPeriod})
}

// ReflectorOptions configures a Reflector.
type ReflectorOptions struct {
	// Name is the Reflector's name. If unset/unspecified, the name defaults to the closest source_file.go:line
	// in the call stack that is outside this package.
	Name string

	// TypeDescription is the Reflector's type description. If unset/unspecified, the type description is defaulted
	// using the following rules: if the expectedType passed to NewReflectorWithOptions was nil, the type description is
	// "<unspecified>". If the expectedType is an instance of *unstructured.Unstructured and its apiVersion and kind fields
	// are set, the type description is the string encoding of those. Otherwise, the type description is set to the
	// go type of expectedType..
	TypeDescription string

	// ResyncPeriod is the Reflector's resync period. If unset/unspecified, the resync period defaults to 0
	// (do not resync).
	ResyncPeriod time.Duration

	// MinWatchTimeout, if non-zero, defines the minimum timeout for watch requests send to kube-apiserver.
	// However, values lower than 5m will not be honored to avoid negative performance impact on controlplane.
	MinWatchTimeout time.Duration

	// Clock allows tests to control time. If unset defaults to clock.RealClock{}
	Clock clock.Clock
}

// NewReflectorWithOptions creates a new Reflector object which will keep the
// given store up to date with the server's contents for the given
// resource. Reflector promises to only put things in the store that
// have the type of expectedType, unless expectedType is nil. If
// resyncPeriod is non-zero, then the reflector will periodically
// consult its ShouldResync function to determine whether to invoke
// the Store's Resync operation; `ShouldResync==nil` means always
// "yes".  This enables you to use reflectors to periodically process
// everything as well as incrementally processing the things that
// change.
func NewReflectorWithOptions(lw ListerWatcher, expectedType interface{}, store ReflectorStore, options ReflectorOptions) *Reflector {
	reflectorClock := options.Clock
	if reflectorClock == nil {
		reflectorClock = clock.RealClock{}
	}
	minWatchTimeout := defaultMinWatchTimeout
	if options.MinWatchTimeout > defaultMinWatchTimeout {
		minWatchTimeout = options.MinWatchTimeout
	}
	r := &Reflector{
		name:            options.Name,
		resyncPeriod:    options.ResyncPeriod,
		minWatchTimeout: minWatchTimeout,
		typeDescription: options.TypeDescription,
		listerWatcher:   ToListerWatcherWithContext(lw),
		store:           store,
		// We used to make the call every 1sec (1 QPS), the goal here is to achieve ~98% traffic reduction when
		// API server is not healthy. With these parameters, backoff will stop at [30,60) sec interval which is
		// 0.22 QPS. If we don't backoff for 2min, assume API server is healthy and we reset the backoff.
		backoffManager:    wait.NewExponentialBackoffManager(800*time.Millisecond, 30*time.Second, 2*time.Minute, 2.0, 1.0, reflectorClock),
		clock:             reflectorClock,
		watchErrorHandler: WatchErrorHandlerWithContext(DefaultWatchErrorHandler),
		expectedType:      reflect.TypeOf(expectedType),
	}

	if r.name == "" {
		r.name = naming.GetNameFromCallsite(internalPackages...)
	}

	if r.typeDescription == "" {
		r.typeDescription = getTypeDescriptionFromObject(expectedType)
	}

	if r.expectedGVK == nil {
		r.expectedGVK = getExpectedGVKFromObject(expectedType)
	}

	// don't overwrite UseWatchList if already set
	// because the higher layers (e.g. storage/cacher) disabled it on purpose
	if r.UseWatchList == nil {
		r.UseWatchList = ptr.To(clientfeatures.FeatureGates().Enabled(clientfeatures.WatchListClient))
	}

	return r
}

func getTypeDescriptionFromObject(expectedType interface{}) string {
	if expectedType == nil {
		return defaultExpectedTypeName
	}

	reflectDescription := reflect.TypeOf(expectedType).String()

	obj, ok := expectedType.(*unstructured.Unstructured)
	if !ok {
		return reflectDescription
	}

	gvk := obj.GroupVersionKind()
	if gvk.Empty() {
		return reflectDescription
	}

	return gvk.String()
}

func getExpectedGVKFromObject(expectedType interface{}) *schema.GroupVersionKind {
	obj, ok := expectedType.(*unstructured.Unstructured)
	if !ok {
		return nil
	}

	gvk := obj.GroupVersionKind()
	if gvk.Empty() {
		return nil
	}

	return &gvk
}

// internalPackages are packages that ignored when creating a default reflector name. These packages are in the common
// call chains to NewReflector, so they'd be low entropy names for reflectors
var internalPackages = []string{"client-go/tools/cache/"}

// Run repeatedly uses the reflector's ListAndWatch to fetch all the
// objects and subsequent deltas.
// Run will exit when stopCh is closed.
//
// Contextual logging: RunWithContext should be used instead of Run in code which supports contextual logging.
func (r *Reflector) Run(stopCh <-chan struct{}) {
	r.RunWithContext(wait.ContextForChannel(stopCh))
}

// RunWithContext repeatedly uses the reflector's ListAndWatch to fetch all the
// objects and subsequent deltas.
// Run will exit when the context is canceled.
func (r *Reflector) RunWithContext(ctx context.Context) {
	logger := klog.FromContext(ctx)
	logger.V(3).Info("Starting reflector", "type", r.typeDescription, "resyncPeriod", r.resyncPeriod, "reflector", r.name)
	wait.BackoffUntil(func() {
		if err := r.ListAndWatchWithContext(ctx); err != nil {
			r.watchErrorHandler(ctx, r, err)
		}
	}, r.backoffManager, true, ctx.Done())
	logger.V(3).Info("Stopping reflector", "type", r.typeDescription, "resyncPeriod", r.resyncPeriod, "reflector", r.name)
}

var (
	// nothing will ever be sent down this channel
	neverExitWatch <-chan time.Time = make(chan time.Time)

	// Used to indicate that watching stopped because of a signal from the stop
	// channel passed in from a client of the reflector.
	errorStopRequested = errors.New("stop requested")
)

// resyncChan returns a channel which will receive something when a resync is
// required, and a cleanup function.
func (r *Reflector) resyncChan() (<-chan time.Time, func() bool) {
	if r.resyncPeriod == 0 {
		return neverExitWatch, func() bool { return false }
	}
	// The cleanup function is required: imagine the scenario where watches
	// always fail so we end up listing frequently. Then, if we don't
	// manually stop the timer, we could end up with many timers active
	// concurrently.
	t := r.clock.NewTimer(r.resyncPeriod)
	return t.C(), t.Stop
}

// ListAndWatch first lists all items and get the resource version at the moment of call,
// and then use the resource version to watch.
// It returns error if ListAndWatch didn't even try to initialize watch.
//
// Contextual logging: ListAndWatchWithContext should be used instead of ListAndWatch in code which supports contextual logging.
func (r *Reflector) ListAndWatch(stopCh <-chan struct{}) error {
	return r.ListAndWatchWithContext(wait.ContextForChannel(stopCh))
}

// ListAndWatchWithContext first lists all items and get the resource version at the moment of call,
// and then use the resource version to watch.
// It returns error if ListAndWatchWithContext didn't even try to initialize watch.
func (r *Reflector) ListAndWatchWithContext(ctx context.Context) error {
	logger := klog.FromContext(ctx)
	logger.V(3).Info("Listing and watching", "type", r.typeDescription, "reflector", r.name)
	var err error
	var w watch.Interface
	useWatchList := ptr.Deref(r.UseWatchList, false)
	fallbackToList := !useWatchList

	if useWatchList {
		w, err = r.watchList(ctx)
		if w == nil && err == nil {
			// stopCh was closed
			return nil
		}
		if err != nil {
			logger.Error(err, "The watchlist request ended with an error, falling back to the standard LIST/WATCH semantics because making progress is better than deadlocking")
			fallbackToList = true
			// ensure that we won't accidentally pass some garbage down the watch.
			w = nil
		}
	}

	if fallbackToList {
		err = r.list(ctx)
		if err != nil {
			return err
		}
	}

	logger.V(2).Info("Caches populated", "type", r.typeDescription, "reflector", r.name)
	return r.watchWithResync(ctx, w)
}

// startResync periodically calls r.store.Resync() method.
// Note that this method is blocking and should be
// called in a separate goroutine.
func (r *Reflector) startResync(ctx context.Context, resyncerrc chan error) {
	logger := klog.FromContext(ctx)
	resyncCh, cleanup := r.resyncChan()
	defer func() {
		cleanup() // Call the last one written into cleanup
	}()
	for {
		select {
		case <-resyncCh:
		case <-ctx.Done():
			return
		}
		if r.ShouldResync == nil || r.ShouldResync() {
			logger.V(4).Info("Forcing resync", "reflector", r.name)
			if err := r.store.Resync(); err != nil {
				resyncerrc <- err
				return
			}
		}
		cleanup()
		resyncCh, cleanup = r.resyncChan()
	}
}

// watchWithResync runs watch with startResync in the background.
func (r *Reflector) watchWithResync(ctx context.Context, w watch.Interface) error {
	resyncerrc := make(chan error, 1)
	cancelCtx, cancel := context.WithCancel(ctx)
	// Waiting for completion of the goroutine is relevant for race detector.
	// Without this, there is a race between "this function returns + code
	// waiting for it" and "goroutine does something".
	var wg wait.Group
	defer func() {
		cancel()
		wg.Wait()
	}()
	wg.Start(func() {
		r.startResync(cancelCtx, resyncerrc)
	})
	return r.watch(ctx, w, resyncerrc)
}

// watch simply starts a watch request with the server.
func (r *Reflector) watch(ctx context.Context, w watch.Interface, resyncerrc chan error) error {
	stopCh := ctx.Done()
	logger := klog.FromContext(ctx)
	var err error
	retry := NewRetryWithDeadline(r.MaxInternalErrorRetryDuration, time.Minute, apierrors.IsInternalError, r.clock)

	for {
		// give the stopCh a chance to stop the loop, even in case of continue statements further down on errors
		select {
		case <-stopCh:
			// we can only end up here when the stopCh
			// was closed after a successful watchlist or list request
			if w != nil {
				w.Stop()
			}
			return nil
		default:
		}

		// start the clock before sending the request, since some proxies won't flush headers until after the first watch event is sent
		start := r.clock.Now()

		if w == nil {
			timeoutSeconds := int64(r.minWatchTimeout.Seconds() * (rand.Float64() + 1.0))
			options := metav1.ListOptions{
				ResourceVersion: r.LastSyncResourceVersion(),
				// We want to avoid situations of hanging watchers. Stop any watchers that do not
				// receive any events within the timeout window.
				TimeoutSeconds: &timeoutSeconds,
				// To reduce load on kube-apiserver on watch restarts, you may enable watch bookmarks.
				// Reflector doesn't assume bookmarks are returned at all (if the server do not support
				// watch bookmarks, it will ignore this field).
				AllowWatchBookmarks: true,
			}

			w, err = r.listerWatcher.WatchWithContext(ctx, options)
			if err != nil {
				if canRetry := isWatchErrorRetriable(err); canRetry {
					logger.V(4).Info("Watch failed - backing off", "reflector", r.name, "type", r.typeDescription, "err", err)
					select {
					case <-stopCh:
						return nil
					case <-r.backoffManager.Backoff().C():
						continue
					}
				}
				return err
			}
		}

		err = handleWatch(ctx, start, w, r.store, r.expectedType, r.expectedGVK, r.name, r.typeDescription, r.setLastSyncResourceVersion,
			r.clock, resyncerrc)
		// Ensure that watch will not be reused across iterations.
		w.Stop()
		w = nil
		retry.After(err)
		if err != nil {
			if !errors.Is(err, errorStopRequested) {
				switch {
				case isExpiredError(err):
					// Don't set LastSyncResourceVersionUnavailable - LIST call with ResourceVersion=RV already
					// has a semantic that it returns data at least as fresh as provided RV.
					// So first try to LIST with setting RV to resource version of last observed object.
					logger.V(4).Info("Watch closed", "reflector", r.name, "type", r.typeDescription, "err", err)
				case apierrors.IsTooManyRequests(err):
					logger.V(2).Info("Watch returned 429 - backing off", "reflector", r.name, "type", r.typeDescription)
					select {
					case <-stopCh:
						return nil
					case <-r.backoffManager.Backoff().C():
						continue
					}
				case apierrors.IsInternalError(err) && retry.ShouldRetry():
					logger.V(2).Info("Retrying watch after internal error", "reflector", r.name, "type", r.typeDescription, "err", err)
					continue
				default:
					logger.Info("Warning: watch ended with error", "reflector", r.name, "type", r.typeDescription, "err", err)
				}
			}
			return nil
		}
	}
}

// list simply lists all items and records a resource version obtained from the server at the moment of the call.
// the resource version can be used for further progress notification (aka. watch).
func (r *Reflector) list(ctx context.Context) error {
	var resourceVersion string
	options := metav1.ListOptions{ResourceVersion: r.relistResourceVersion()}

	initTrace := trace.New("Reflector ListAndWatch", trace.Field{Key: "name", Value: r.name})
	defer initTrace.LogIfLong(10 * time.Second)
	var list runtime.Object
	var paginatedResult bool
	var err error
	listCh := make(chan struct{}, 1)
	panicCh := make(chan interface{}, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				panicCh <- r
			}
		}()
		// Attempt to gather list in chunks, if supported by listerWatcher, if not, the first
		// list request will return the full response.
		pager := pager.New(pager.SimplePageFunc(func(opts metav1.ListOptions) (runtime.Object, error) {
			return r.listerWatcher.ListWithContext(ctx, opts)
		}))
		switch {
		case r.WatchListPageSize != 0:
			pager.PageSize = r.WatchListPageSize
		case r.paginatedResult:
			// We got a paginated result initially. Assume this resource and server honor
			// paging requests (i.e. watch cache is probably disabled) and leave the default
			// pager size set.
		case options.ResourceVersion != "" && options.ResourceVersion != "0":
			// User didn't explicitly request pagination.
			//
			// With ResourceVersion != "", we have a possibility to list from watch cache,
			// but we do that (for ResourceVersion != "0") only if Limit is unset.
			// To avoid thundering herd on etcd (e.g. on master upgrades), we explicitly
			// switch off pagination to force listing from watch cache (if enabled).
			// With the existing semantic of RV (result is at least as fresh as provided RV),
			// this is correct and doesn't lead to going back in time.
			//
			// We also don't turn off pagination for ResourceVersion="0", since watch cache
			// is ignoring Limit in that case anyway, and if watch cache is not enabled
			// we don't introduce regression.
			pager.PageSize = 0
		}

		list, paginatedResult, err = pager.ListWithAlloc(context.Background(), options)
		if isExpiredError(err) || isTooLargeResourceVersionError(err) {
			r.setIsLastSyncResourceVersionUnavailable(true)
			// Retry immediately if the resource version used to list is unavailable.
			// The pager already falls back to full list if paginated list calls fail due to an "Expired" error on
			// continuation pages, but the pager might not be enabled, the full list might fail because the
			// resource version it is listing at is expired or the cache may not yet be synced to the provided
			// resource version. So we need to fallback to resourceVersion="" in all to recover and ensure
			// the reflector makes forward progress.
			list, paginatedResult, err = pager.ListWithAlloc(context.Background(), metav1.ListOptions{ResourceVersion: r.relistResourceVersion()})
		}
		close(listCh)
	}()
	select {
	case <-ctx.Done():
		return nil
	case r := <-panicCh:
		panic(r)
	case <-listCh:
	}
	initTrace.Step("Objects listed", trace.Field{Key: "error", Value: err})
	if err != nil {
		return fmt.Errorf("failed to list %v: %w", r.typeDescription, err)
	}

	// We check if the list was paginated and if so set the paginatedResult based on that.
	// However, we want to do that only for the initial list (which is the only case
	// when we set ResourceVersion="0"). The reasoning behind it is that later, in some
	// situations we may force listing directly from etcd (by setting ResourceVersion="")
	// which will return paginated result, even if watch cache is enabled. However, in
	// that case, we still want to prefer sending requests to watch cache if possible.
	//
	// Paginated result returned for request with ResourceVersion="0" mean that watch
	// cache is disabled and there are a lot of objects of a given type. In such case,
	// there is no need to prefer listing from watch cache.
	if options.ResourceVersion == "0" && paginatedResult {
		r.paginatedResult = true
	}

	r.setIsLastSyncResourceVersionUnavailable(false) // list was successful
	listMetaInterface, err := meta.ListAccessor(list)
	if err != nil {
		return fmt.Errorf("unable to understand list result %#v: %v", list, err)
	}
	resourceVersion = listMetaInterface.GetResourceVersion()
	initTrace.Step("Resource version extracted")
	items, err := meta.ExtractListWithAlloc(list)
	if err != nil {
		return fmt.Errorf("unable to understand list result %#v (%v)", list, err)
	}
	initTrace.Step("Objects extracted")
	if err := r.syncWith(items, resourceVersion); err != nil {
		return fmt.Errorf("unable to sync list result: %v", err)
	}
	initTrace.Step("SyncWith done")
	r.setLastSyncResourceVersion(resourceVersion)
	initTrace.Step("Resource version updated")
	return nil
}

// watchList establishes a stream to get a consistent snapshot of data
// from the server as described in https://github.com/kubernetes/enhancements/tree/master/keps/sig-api-machinery/3157-watch-list#proposal
//
// case 1: start at Most Recent (RV="", ResourceVersionMatch=ResourceVersionMatchNotOlderThan)
// Establishes a consistent stream with the server.
// That means the returned data is consistent, as if, served directly from etcd via a quorum read.
// It begins with synthetic "Added" events of all resources up to the most recent ResourceVersion.
// It ends with a synthetic "Bookmark" event containing the most recent ResourceVersion.
// After receiving a "Bookmark" event the reflector is considered to be synchronized.
// It replaces its internal store with the collected items and
// reuses the current watch requests for getting further events.
//
// case 2: start at Exact (RV>"0", ResourceVersionMatch=ResourceVersionMatchNotOlderThan)
// Establishes a stream with the server at the provided resource version.
// To establish the initial state the server begins with synthetic "Added" events.
// It ends with a synthetic "Bookmark" event containing the provided or newer resource version.
// After receiving a "Bookmark" event the reflector is considered to be synchronized.
// It replaces its internal store with the collected items and
// reuses the current watch requests for getting further events.
func (r *Reflector) watchList(ctx context.Context) (watch.Interface, error) {
	stopCh := ctx.Done()
	logger := klog.FromContext(ctx)
	var w watch.Interface
	var err error
	var temporaryStore Store
	var resourceVersion string
	// TODO(#115478): see if this function could be turned
	//  into a method and see if error handling
	//  could be unified with the r.watch method
	isErrorRetriableWithSideEffectsFn := func(err error) bool {
		if canRetry := isWatchErrorRetriable(err); canRetry {
			logger.V(2).Info("watch-list failed - backing off", "reflector", r.name, "type", r.typeDescription, "err", err)
			<-r.backoffManager.Backoff().C()
			return true
		}
		if isExpiredError(err) || isTooLargeResourceVersionError(err) {
			// we tried to re-establish a watch request but the provided RV
			// has either expired or it is greater than the server knows about.
			// In that case we reset the RV and
			// try to get a consistent snapshot from the watch cache (case 1)
			r.setIsLastSyncResourceVersionUnavailable(true)
			return true
		}
		return false
	}

	initTrace := trace.New("Reflector WatchList", trace.Field{Key: "name", Value: r.name})
	defer initTrace.LogIfLong(10 * time.Second)
	for {
		select {
		case <-stopCh:
			return nil, nil
		default:
		}

		resourceVersion = ""
		lastKnownRV := r.rewatchResourceVersion()
		temporaryStore = NewStore(DeletionHandlingMetaNamespaceKeyFunc)
		// TODO(#115478): large "list", slow clients, slow network, p&f
		//  might slow down streaming and eventually fail.
		//  maybe in such a case we should retry with an increased timeout?
		timeoutSeconds := int64(r.minWatchTimeout.Seconds() * (rand.Float64() + 1.0))
		options := metav1.ListOptions{
			ResourceVersion:      lastKnownRV,
			AllowWatchBookmarks:  true,
			SendInitialEvents:    pointer.Bool(true),
			ResourceVersionMatch: metav1.ResourceVersionMatchNotOlderThan,
			TimeoutSeconds:       &timeoutSeconds,
		}
		start := r.clock.Now()

		w, err = r.listerWatcher.WatchWithContext(ctx, options)
		if err != nil {
			if isErrorRetriableWithSideEffectsFn(err) {
				continue
			}
			return nil, err
		}
		watchListBookmarkReceived, err := handleListWatch(ctx, start, w, temporaryStore, r.expectedType, r.expectedGVK, r.name, r.typeDescription,
			func(rv string) { resourceVersion = rv },
			r.clock, make(chan error))
		if err != nil {
			w.Stop() // stop and retry with clean state
			if errors.Is(err, errorStopRequested) {
				return nil, nil
			}
			if isErrorRetriableWithSideEffectsFn(err) {
				continue
			}
			return nil, err
		}
		if watchListBookmarkReceived {
			break
		}
	}
	// We successfully got initial state from watch-list confirmed by the
	// "k8s.io/initial-events-end" bookmark.
	initTrace.Step("Objects streamed", trace.Field{Key: "count", Value: len(temporaryStore.List())})
	r.setIsLastSyncResourceVersionUnavailable(false)

	// we utilize the temporaryStore to ensure independence from the current store implementation.
	// as of today, the store is implemented as a queue and will be drained by the higher-level
	// component as soon as it finishes replacing the content.
	checkWatchListDataConsistencyIfRequested(ctx, r.name, resourceVersion, r.listerWatcher.ListWithContext, temporaryStore.List)

	if err := r.store.Replace(temporaryStore.List(), resourceVersion); err != nil {
		return nil, fmt.Errorf("unable to sync watch-list result: %w", err)
	}
	initTrace.Step("SyncWith done")
	r.setLastSyncResourceVersion(resourceVersion)

	return w, nil
}

// syncWith replaces the store's items with the given list.
func (r *Reflector) syncWith(items []runtime.Object, resourceVersion string) error {
	found := make([]interface{}, 0, len(items))
	for _, item := range items {
		found = append(found, item)
	}
	return r.store.Replace(found, resourceVersion)
}

// handleListWatch consumes events from w, updates the Store, and records the
// last seen ResourceVersion, to allow continuing from that ResourceVersion on
// retry. If successful, the watcher will be left open after receiving the
// initial set of objects, to allow watching for future events.
func handleListWatch(
	ctx context.Context,
	start time.Time,
	w watch.Interface,
	store Store,
	expectedType reflect.Type,
	expectedGVK *schema.GroupVersionKind,
	name string,
	expectedTypeName string,
	setLastSyncResourceVersion func(string),
	clock clock.Clock,
	errCh chan error,
) (bool, error) {
	exitOnWatchListBookmarkReceived := true
	return handleAnyWatch(ctx, start, w, store, expectedType, expectedGVK, name, expectedTypeName,
		setLastSyncResourceVersion, exitOnWatchListBookmarkReceived, clock, errCh)
}

// handleListWatch consumes events from w, updates the Store, and records the
// last seen ResourceVersion, to allow continuing from that ResourceVersion on
// retry. The watcher will always be stopped on exit.
func handleWatch(
	ctx context.Context,
	start time.Time,
	w watch.Interface,
	store ReflectorStore,
	expectedType reflect.Type,
	expectedGVK *schema.GroupVersionKind,
	name string,
	expectedTypeName string,
	setLastSyncResourceVersion func(string),
	clock clock.Clock,
	errCh chan error,
) error {
	exitOnWatchListBookmarkReceived := false
	_, err := handleAnyWatch(ctx, start, w, store, expectedType, expectedGVK, name, expectedTypeName,
		setLastSyncResourceVersion, exitOnWatchListBookmarkReceived, clock, errCh)
	return err
}

// handleAnyWatch consumes events from w, updates the Store, and records the last
// seen ResourceVersion, to allow continuing from that ResourceVersion on retry.
// If exitOnWatchListBookmarkReceived is true, the watch events will be consumed
// until a bookmark event is received with the WatchList annotation present.
// Returns true (watchListBookmarkReceived) if the WatchList bookmark was
// received, even if exitOnWatchListBookmarkReceived is false.
// The watcher will always be stopped, unless exitOnWatchListBookmarkReceived is
// true and watchListBookmarkReceived is true. This allows the same watch stream
// to be re-used by the caller to continue watching for new events.
func handleAnyWatch(
	ctx context.Context,
	start time.Time,
	w watch.Interface,
	store ReflectorStore,
	expectedType reflect.Type,
	expectedGVK *schema.GroupVersionKind,
	name string,
	expectedTypeName string,
	setLastSyncResourceVersion func(string),
	exitOnWatchListBookmarkReceived bool,
	clock clock.Clock,
	errCh chan error,
) (bool, error) {
	watchListBookmarkReceived := false
	eventCount := 0
	logger := klog.FromContext(ctx)
	initialEventsEndBookmarkWarningTicker := newInitialEventsEndBookmarkTicker(logger, name, clock, start, exitOnWatchListBookmarkReceived)
	defer initialEventsEndBookmarkWarningTicker.Stop()

loop:
	for {
		select {
		case <-ctx.Done():
			return watchListBookmarkReceived, errorStopRequested
		case err := <-errCh:
			return watchListBookmarkReceived, err
		case event, ok := <-w.ResultChan():
			if !ok {
				break loop
			}
			if event.Type == watch.Error {
				return watchListBookmarkReceived, apierrors.FromObject(event.Object)
			}
			if expectedType != nil {
				if e, a := expectedType, reflect.TypeOf(event.Object); e != a {
					utilruntime.HandleErrorWithContext(ctx, nil, "Unexpected watch event object type", "reflector", name, "expectedType", e, "actualType", a)
					continue
				}
			}
			if expectedGVK != nil {
				if e, a := *expectedGVK, event.Object.GetObjectKind().GroupVersionKind(); e != a {
					utilruntime.HandleErrorWithContext(ctx, nil, "Unexpected watch event object gvk", "reflector", name, "expectedGVK", e, "actualGVK", a)
					continue
				}
			}
			meta, err := meta.Accessor(event.Object)
			if err != nil {
				utilruntime.HandleErrorWithContext(ctx, err, "Unable to understand watch event", "reflector", name, "event", event)
				continue
			}
			resourceVersion := meta.GetResourceVersion()
			switch event.Type {
			case watch.Added:
				err := store.Add(event.Object)
				if err != nil {
					utilruntime.HandleErrorWithContext(ctx, err, "Unable to add watch event object to store", "reflector", name, "object", event.Object)
				}
			case watch.Modified:
				err := store.Update(event.Object)
				if err != nil {
					utilruntime.HandleErrorWithContext(ctx, err, "Unable to update watch event object to store", "reflector", name, "object", event.Object)
				}
			case watch.Deleted:
				// TODO: Will any consumers need access to the "last known
				// state", which is passed in event.Object? If so, may need
				// to change this.
				err := store.Delete(event.Object)
				if err != nil {
					utilruntime.HandleErrorWithContext(ctx, err, "Unable to delete watch event object from store", "reflector", name, "object", event.Object)
				}
			case watch.Bookmark:
				// A `Bookmark` means watch has synced here, just update the resourceVersion
				if meta.GetAnnotations()[metav1.InitialEventsAnnotationKey] == "true" {
					watchListBookmarkReceived = true
				}
			default:
				utilruntime.HandleErrorWithContext(ctx, err, "Unknown watch event", "reflector", name, "event", event)
			}
			setLastSyncResourceVersion(resourceVersion)
			if rvu, ok := store.(ResourceVersionUpdater); ok {
				rvu.UpdateResourceVersion(resourceVersion)
			}
			eventCount++
			if exitOnWatchListBookmarkReceived && watchListBookmarkReceived {
				watchDuration := clock.Since(start)
				klog.FromContext(ctx).V(4).Info("Exiting watch because received the bookmark that marks the end of initial events stream", "reflector", name, "totalItems", eventCount, "duration", watchDuration)
				return watchListBookmarkReceived, nil
			}
			initialEventsEndBookmarkWarningTicker.observeLastEventTimeStamp(clock.Now())
		case <-initialEventsEndBookmarkWarningTicker.C():
			initialEventsEndBookmarkWarningTicker.warnIfExpired()
		}
	}

	watchDuration := clock.Since(start)
	if watchDuration < 1*time.Second && eventCount == 0 {
		return watchListBookmarkReceived, fmt.Errorf("very short watch: %s: Unexpected watch close - watch lasted less than a second and no items received", name)
	}
	klog.FromContext(ctx).V(4).Info("Watch close", "reflector", name, "type", expectedTypeName, "totalItems", eventCount)
	return watchListBookmarkReceived, nil
}

// LastSyncResourceVersion is the resource version observed when last sync with the underlying store
// The value returned is not synchronized with access to the underlying store and is not thread-safe
func (r *Reflector) LastSyncResourceVersion() string {
	r.lastSyncResourceVersionMutex.RLock()
	defer r.lastSyncResourceVersionMutex.RUnlock()
	return r.lastSyncResourceVersion
}

func (r *Reflector) setLastSyncResourceVersion(v string) {
	r.lastSyncResourceVersionMutex.Lock()
	defer r.lastSyncResourceVersionMutex.Unlock()
	r.lastSyncResourceVersion = v
}

// relistResourceVersion determines the resource version the reflector should list or relist from.
// Returns either the lastSyncResourceVersion so that this reflector will relist with a resource
// versions no older than has already been observed in relist results or watch events, or, if the last relist resulted
// in an HTTP 410 (Gone) status code, returns "" so that the relist will use the latest resource version available in
// etcd via a quorum read.
func (r *Reflector) relistResourceVersion() string {
	r.lastSyncResourceVersionMutex.RLock()
	defer r.lastSyncResourceVersionMutex.RUnlock()

	if r.isLastSyncResourceVersionUnavailable {
		// Since this reflector makes paginated list requests, and all paginated list requests skip the watch cache
		// if the lastSyncResourceVersion is unavailable, we set ResourceVersion="" and list again to re-establish reflector
		// to the latest available ResourceVersion, using a consistent read from etcd.
		return ""
	}
	if r.lastSyncResourceVersion == "" {
		// For performance reasons, initial list performed by reflector uses "0" as resource version to allow it to
		// be served from the watch cache if it is enabled.
		return "0"
	}
	return r.lastSyncResourceVersion
}

// rewatchResourceVersion determines the resource version the reflector should start streaming from.
func (r *Reflector) rewatchResourceVersion() string {
	r.lastSyncResourceVersionMutex.RLock()
	defer r.lastSyncResourceVersionMutex.RUnlock()
	if r.isLastSyncResourceVersionUnavailable {
		// initial stream should return data at the most recent resource version.
		// the returned data must be consistent i.e. as if served from etcd via a quorum read
		return ""
	}
	return r.lastSyncResourceVersion
}

// setIsLastSyncResourceVersionUnavailable sets if the last list or watch request with lastSyncResourceVersion returned
// "expired" or "too large resource version" error.
func (r *Reflector) setIsLastSyncResourceVersionUnavailable(isUnavailable bool) {
	r.lastSyncResourceVersionMutex.Lock()
	defer r.lastSyncResourceVersionMutex.Unlock()
	r.isLastSyncResourceVersionUnavailable = isUnavailable
}

func isExpiredError(err error) bool {
	// In Kubernetes 1.17 and earlier, the api server returns both apierrors.StatusReasonExpired and
	// apierrors.StatusReasonGone for HTTP 410 (Gone) status code responses. In 1.18 the kube server is more consistent
	// and always returns apierrors.StatusReasonExpired. For backward compatibility we can only remove the apierrors.IsGone
	// check when we fully drop support for Kubernetes 1.17 servers from reflectors.
	return apierrors.IsResourceExpired(err) || apierrors.IsGone(err)
}

func isTooLargeResourceVersionError(err error) bool {
	if apierrors.HasStatusCause(err, metav1.CauseTypeResourceVersionTooLarge) {
		return true
	}
	// In Kubernetes 1.17.0-1.18.5, the api server doesn't set the error status cause to
	// metav1.CauseTypeResourceVersionTooLarge to indicate that the requested minimum resource
	// version is larger than the largest currently available resource version. To ensure backward
	// compatibility with these server versions we also need to detect the error based on the content
	// of the error message field.
	if !apierrors.IsTimeout(err) {
		return false
	}
	apierr, ok := err.(apierrors.APIStatus)
	if !ok || apierr == nil || apierr.Status().Details == nil {
		return false
	}
	for _, cause := range apierr.Status().Details.Causes {
		// Matches the message returned by api server 1.17.0-1.18.5 for this error condition
		if cause.Message == "Too large resource version" {
			return true
		}
	}

	// Matches the message returned by api server before 1.17.0
	if strings.Contains(apierr.Status().Message, "Too large resource version") {
		return true
	}

	return false
}

// isWatchErrorRetriable determines if it is safe to retry
// a watch error retrieved from the server.
func isWatchErrorRetriable(err error) bool {
	// If this is "connection refused" error, it means that most likely apiserver is not responsive.
	// It doesn't make sense to re-list all objects because most likely we will be able to restart
	// watch where we ended.
	// If that's the case begin exponentially backing off and resend watch request.
	// Do the same for "429" errors.
	if utilnet.IsConnectionRefused(err) || apierrors.IsTooManyRequests(err) {
		return true
	}
	return false
}

// initialEventsEndBookmarkTicker a ticker that produces a warning if the bookmark event
// which marks the end of the watch stream, has not been received within the defined tick interval.
//
// Note:
// The methods exposed by this type are not thread-safe.
type initialEventsEndBookmarkTicker struct {
	clock.Ticker
	clock  clock.Clock
	name   string
	logger klog.Logger

	watchStart           time.Time
	tickInterval         time.Duration
	lastEventObserveTime time.Time
}

// newInitialEventsEndBookmarkTicker returns a noop ticker if exitOnInitialEventsEndBookmarkRequested is false.
// Otherwise, it returns a ticker that exposes a method producing a warning if the bookmark event,
// which marks the end of the watch stream, has not been received within the defined tick interval.
//
// Note that the caller controls whether to call t.C() and t.Stop().
//
// In practice, the reflector exits the watchHandler as soon as the bookmark event is received and calls the t.C() method.
func newInitialEventsEndBookmarkTicker(logger klog.Logger, name string, c clock.Clock, watchStart time.Time, exitOnWatchListBookmarkReceived bool) *initialEventsEndBookmarkTicker {
	return newInitialEventsEndBookmarkTickerInternal(logger, name, c, watchStart, 10*time.Second, exitOnWatchListBookmarkReceived)
}

func newInitialEventsEndBookmarkTickerInternal(logger klog.Logger, name string, c clock.Clock, watchStart time.Time, tickInterval time.Duration, exitOnWatchListBookmarkReceived bool) *initialEventsEndBookmarkTicker {
	clockWithTicker, ok := c.(clock.WithTicker)
	if !ok || !exitOnWatchListBookmarkReceived {
		if exitOnWatchListBookmarkReceived {
			logger.Info("Warning: clock does not support WithTicker interface but exitOnInitialEventsEndBookmark was requested")
		}
		return &initialEventsEndBookmarkTicker{
			Ticker: &noopTicker{},
		}
	}

	return &initialEventsEndBookmarkTicker{
		Ticker:       clockWithTicker.NewTicker(tickInterval),
		clock:        c,
		name:         name,
		logger:       logger,
		watchStart:   watchStart,
		tickInterval: tickInterval,
	}
}

func (t *initialEventsEndBookmarkTicker) observeLastEventTimeStamp(lastEventObserveTime time.Time) {
	t.lastEventObserveTime = lastEventObserveTime
}

func (t *initialEventsEndBookmarkTicker) warnIfExpired() {
	if err := t.produceWarningIfExpired(); err != nil {
		t.logger.Info("Warning: event bookmark expired", "err", err)
	}
}

// produceWarningIfExpired returns an error that represents a warning when
// the time elapsed since the last received event exceeds the tickInterval.
//
// Note that this method should be called when t.C() yields a value.
func (t *initialEventsEndBookmarkTicker) produceWarningIfExpired() error {
	if _, ok := t.Ticker.(*noopTicker); ok {
		return nil /*noop ticker*/
	}
	if t.lastEventObserveTime.IsZero() {
		return fmt.Errorf("%s: awaiting required bookmark event for initial events stream, no events received for %v", t.name, t.clock.Since(t.watchStart))
	}
	elapsedTime := t.clock.Now().Sub(t.lastEventObserveTime)
	hasBookmarkTimerExpired := elapsedTime >= t.tickInterval

	if !hasBookmarkTimerExpired {
		return nil
	}
	return fmt.Errorf("%s: hasn't received required bookmark event marking the end of initial events stream, received last event %v ago", t.name, elapsedTime)
}

var _ clock.Ticker = &noopTicker{}

// TODO(#115478): move to k8s/utils repo
type noopTicker struct{}

func (t *noopTicker) C() <-chan time.Time { return nil }

func (t *noopTicker) Stop() {}
