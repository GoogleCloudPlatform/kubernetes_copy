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
	"math"
	"sort"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/apiserver/pkg/features"
	"k8s.io/apiserver/pkg/storage"
	"k8s.io/apiserver/pkg/storage/cacher/metrics"
	etcdfeature "k8s.io/apiserver/pkg/storage/feature"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/tools/cache"
	"k8s.io/component-base/tracing"
	"k8s.io/klog/v2"
	"k8s.io/utils/clock"
)

const (
	// blockTimeout determines how long we're willing to block the request
	// to wait for a given resource version to be propagated to cache,
	// before terminating request and returning Timeout error with retry
	// after suggestion.
	blockTimeout = 3 * time.Second

	// resourceVersionTooHighRetrySeconds is the seconds before a operation should be retried by the client
	// after receiving a 'too high resource version' error.
	resourceVersionTooHighRetrySeconds = 1

	// defaultLowerBoundCapacity is a default value for event cache capacity's lower bound.
	// TODO: Figure out, to what value we can decreased it.
	defaultLowerBoundCapacity = 100

	// defaultUpperBoundCapacity should be able to keep the required history.
	defaultUpperBoundCapacity = 100 * 1024
)

// watchCacheEvent is a single "watch event" that is send to users of
// watchCache. Additionally to a typical "watch.Event" it contains
// the previous value of the object to enable proper filtering in the
// upper layers.
type watchCacheEvent struct {
	Type            watch.EventType
	Object          runtime.Object
	ObjLabels       labels.Set
	ObjFields       fields.Set
	PrevObject      runtime.Object
	PrevObjLabels   labels.Set
	PrevObjFields   fields.Set
	Key             string
	ResourceVersion uint64
	RecordTime      time.Time
}

// watchCache implements a Store interface.
// However, it depends on the elements implementing runtime.Object interface.
//
// watchCache is a "sliding window" (with a limited capacity) of objects
// observed from a watch.
type watchCache struct {
	sync.RWMutex

	// Condition on which lists are waiting for the fresh enough
	// resource version.
	cond *sync.Cond

	// Maximum size of history window.
	capacity int

	// upper bound of capacity since event cache has a dynamic size.
	upperBoundCapacity int

	// lower bound of capacity since event cache has a dynamic size.
	lowerBoundCapacity int

	// keyFunc is used to get a key in the underlying storage for a given object.
	keyFunc func(runtime.Object) (string, error)

	// getAttrsFunc is used to get labels and fields of an object.
	getAttrsFunc func(runtime.Object) (labels.Set, fields.Set, error)

	// cache is used a cyclic buffer - the "current" contents of it are
	// stored in [start_index%capacity, end_index%capacity) - so the
	// "current" contents have exactly end_index-start_index items.
	cache      []*watchCacheEvent
	startIndex int
	endIndex   int
	// removedEventSinceRelist holds the information whether any of the events
	// were already removed from the `cache` cyclic buffer since the last relist
	removedEventSinceRelist bool

	// store will effectively support LIST operation from the "end of cache
	// history" i.e. from the moment just after the newest cached watched event.
	// It is necessary to effectively allow clients to start watching at now.
	// NOTE: We assume that <store> is thread-safe.
	store storeIndexer

	// ResourceVersion up to which the watchCache is propagated.
	resourceVersion uint64

	// ResourceVersion of the last list result (populated via Replace() method).
	listResourceVersion uint64

	// This handler is run at the end of every successful Replace() method.
	onReplace func()

	// This handler is run at the end of every Add/Update/Delete method
	// and additionally gets the previous value of the object.
	eventHandler func(*watchCacheEvent)

	// for testing timeouts.
	clock clock.Clock

	// eventFreshDuration defines the minimum watch history watchcache will store.
	eventFreshDuration time.Duration

	// An underlying storage.Versioner.
	versioner storage.Versioner

	// cacher's group resource
	groupResource schema.GroupResource

	// For testing cache interval invalidation.
	indexValidator indexValidator

	// Requests progress notification if there are requests waiting for watch
	// to be fresh
	waitingUntilFresh *conditionalProgressRequester
}

func newWatchCache(
	keyFunc func(runtime.Object) (string, error),
	eventHandler func(*watchCacheEvent),
	getAttrsFunc func(runtime.Object) (labels.Set, fields.Set, error),
	versioner storage.Versioner,
	indexers *cache.Indexers,
	clock clock.WithTicker,
	eventFreshDuration time.Duration,
	groupResource schema.GroupResource,
	progressRequester *conditionalProgressRequester) *watchCache {
	wc := &watchCache{
		capacity:            defaultLowerBoundCapacity,
		keyFunc:             keyFunc,
		getAttrsFunc:        getAttrsFunc,
		cache:               make([]*watchCacheEvent, defaultLowerBoundCapacity),
		lowerBoundCapacity:  defaultLowerBoundCapacity,
		upperBoundCapacity:  defaultUpperBoundCapacity,
		startIndex:          0,
		endIndex:            0,
		store:               newStoreIndexer(indexers),
		resourceVersion:     0,
		listResourceVersion: 0,
		eventHandler:        eventHandler,
		clock:               clock,
		eventFreshDuration:  eventFreshDuration,
		versioner:           versioner,
		groupResource:       groupResource,
		waitingUntilFresh:   progressRequester,
	}
	metrics.WatchCacheCapacity.WithLabelValues(groupResource.String()).Set(float64(wc.capacity))
	wc.cond = sync.NewCond(wc.RLocker())
	wc.indexValidator = wc.isIndexValidLocked

	return wc
}

// Add takes runtime.Object as an argument.
func (w *watchCache) Add(obj interface{}) error {
	object, resourceVersion, err := w.objectToVersionedRuntimeObject(obj)
	if err != nil {
		return err
	}
	event := watch.Event{Type: watch.Added, Object: object}

	f := func(elem *storeElement) error { return w.store.Add(elem) }
	return w.processEvent(event, resourceVersion, f)
}

// Update takes runtime.Object as an argument.
func (w *watchCache) Update(obj interface{}) error {
	object, resourceVersion, err := w.objectToVersionedRuntimeObject(obj)
	if err != nil {
		return err
	}
	event := watch.Event{Type: watch.Modified, Object: object}

	f := func(elem *storeElement) error { return w.store.Update(elem) }
	return w.processEvent(event, resourceVersion, f)
}

// Delete takes runtime.Object as an argument.
func (w *watchCache) Delete(obj interface{}) error {
	object, resourceVersion, err := w.objectToVersionedRuntimeObject(obj)
	if err != nil {
		return err
	}
	event := watch.Event{Type: watch.Deleted, Object: object}

	f := func(elem *storeElement) error { return w.store.Delete(elem) }
	return w.processEvent(event, resourceVersion, f)
}

func (w *watchCache) objectToVersionedRuntimeObject(obj interface{}) (runtime.Object, uint64, error) {
	object, ok := obj.(runtime.Object)
	if !ok {
		return nil, 0, fmt.Errorf("obj does not implement runtime.Object interface: %v", obj)
	}
	resourceVersion, err := w.versioner.ObjectResourceVersion(object)
	if err != nil {
		return nil, 0, err
	}
	return object, resourceVersion, nil
}

// processEvent is safe as long as there is at most one call to it in flight
// at any point in time.
func (w *watchCache) processEvent(event watch.Event, resourceVersion uint64, updateFunc func(*storeElement) error) error {
	metrics.EventsReceivedCounter.WithLabelValues(w.groupResource.String()).Inc()

	key, err := w.keyFunc(event.Object)
	if err != nil {
		return fmt.Errorf("couldn't compute key: %v", err)
	}
	elem := &storeElement{Key: key, Object: event.Object}
	elem.Labels, elem.Fields, err = w.getAttrsFunc(event.Object)
	if err != nil {
		return err
	}

	wcEvent := &watchCacheEvent{
		Type:            event.Type,
		Object:          elem.Object,
		ObjLabels:       elem.Labels,
		ObjFields:       elem.Fields,
		Key:             key,
		ResourceVersion: resourceVersion,
		RecordTime:      w.clock.Now(),
	}

	// We can call w.store.Get() outside of a critical section,
	// because the w.store itself is thread-safe and the only
	// place where w.store is modified is below (via updateFunc)
	// and these calls are serialized because reflector is processing
	// events one-by-one.
	previous, exists, err := w.store.Get(elem)
	if err != nil {
		return err
	}
	if exists {
		previousElem := previous.(*storeElement)
		wcEvent.PrevObject = previousElem.Object
		wcEvent.PrevObjLabels = previousElem.Labels
		wcEvent.PrevObjFields = previousElem.Fields
	}

	if err := func() error {
		w.Lock()
		defer w.Unlock()

		w.updateCache(wcEvent)
		w.resourceVersion = resourceVersion
		defer w.cond.Broadcast()

		return updateFunc(elem)
	}(); err != nil {
		return err
	}

	// Avoid calling event handler under lock.
	// This is safe as long as there is at most one call to Add/Update/Delete and
	// UpdateResourceVersion in flight at any point in time, which is true now,
	// because reflector calls them synchronously from its main thread.
	if w.eventHandler != nil {
		w.eventHandler(wcEvent)
	}
	metrics.RecordResourceVersion(w.groupResource.String(), resourceVersion)
	return nil
}

// Assumes that lock is already held for write.
func (w *watchCache) updateCache(event *watchCacheEvent) {
	w.resizeCacheLocked(event.RecordTime)
	if w.isCacheFullLocked() {
		// Cache is full - remove the oldest element.
		w.startIndex++
		w.removedEventSinceRelist = true
	}
	w.cache[w.endIndex%w.capacity] = event
	w.endIndex++
}

// resizeCacheLocked resizes the cache if necessary:
// - increases capacity by 2x if cache is full and all cached events occurred within last eventFreshDuration.
// - decreases capacity by 2x when recent quarter of events occurred outside of eventFreshDuration(protect watchCache from flapping).
func (w *watchCache) resizeCacheLocked(eventTime time.Time) {
	if w.isCacheFullLocked() && eventTime.Sub(w.cache[w.startIndex%w.capacity].RecordTime) < w.eventFreshDuration {
		capacity := min(w.capacity*2, w.upperBoundCapacity)
		if capacity > w.capacity {
			w.doCacheResizeLocked(capacity)
		}
		return
	}
	if w.isCacheFullLocked() && eventTime.Sub(w.cache[(w.endIndex-w.capacity/4)%w.capacity].RecordTime) > w.eventFreshDuration {
		capacity := max(w.capacity/2, w.lowerBoundCapacity)
		if capacity < w.capacity {
			w.doCacheResizeLocked(capacity)
		}
		return
	}
}

// isCacheFullLocked used to judge whether watchCacheEvent is full.
// Assumes that lock is already held for write.
func (w *watchCache) isCacheFullLocked() bool {
	return w.endIndex == w.startIndex+w.capacity
}

// doCacheResizeLocked resize watchCache's event array with different capacity.
// Assumes that lock is already held for write.
func (w *watchCache) doCacheResizeLocked(capacity int) {
	newCache := make([]*watchCacheEvent, capacity)
	if capacity < w.capacity {
		// adjust startIndex if cache capacity shrink.
		w.startIndex = w.endIndex - capacity
	}
	for i := w.startIndex; i < w.endIndex; i++ {
		newCache[i%capacity] = w.cache[i%w.capacity]
	}
	w.cache = newCache
	metrics.RecordsWatchCacheCapacityChange(w.groupResource.String(), w.capacity, capacity)
	w.capacity = capacity
}

func (w *watchCache) UpdateResourceVersion(resourceVersion string) {
	rv, err := w.versioner.ParseResourceVersion(resourceVersion)
	if err != nil {
		klog.Errorf("Couldn't parse resourceVersion: %v", err)
		return
	}

	func() {
		w.Lock()
		defer w.Unlock()
		w.resourceVersion = rv
		w.cond.Broadcast()
	}()

	// Avoid calling event handler under lock.
	// This is safe as long as there is at most one call to Add/Update/Delete and
	// UpdateResourceVersion in flight at any point in time, which is true now,
	// because reflector calls them synchronously from its main thread.
	if w.eventHandler != nil {
		wcEvent := &watchCacheEvent{
			Type:            watch.Bookmark,
			ResourceVersion: rv,
		}
		w.eventHandler(wcEvent)
	}
	metrics.RecordResourceVersion(w.groupResource.String(), rv)
}

// List returns list of pointers to <storeElement> objects.
func (w *watchCache) List() []interface{} {
	return w.store.List()
}

// waitUntilFreshAndBlock waits until cache is at least as fresh as given <resourceVersion>.
// NOTE: This function acquired lock and doesn't release it.
// You HAVE TO explicitly call w.RUnlock() after this function.
func (w *watchCache) waitUntilFreshAndBlock(ctx context.Context, resourceVersion uint64) error {
	startTime := w.clock.Now()
	defer func() {
		if resourceVersion > 0 {
			metrics.WatchCacheReadWait.WithContext(ctx).WithLabelValues(w.groupResource.String()).Observe(w.clock.Since(startTime).Seconds())
		}
	}()

	// In case resourceVersion is 0, we accept arbitrarily stale result.
	// As a result, the condition in the below for loop will never be
	// satisfied (w.resourceVersion is never negative), this call will
	// never hit the w.cond.Wait().
	// As a result - we can optimize the code by not firing the wakeup
	// function (and avoid starting a goroutine), especially given that
	// resourceVersion=0 is the most common case.
	if resourceVersion > 0 {
		go func() {
			// Wake us up when the time limit has expired.  The docs
			// promise that time.After (well, NewTimer, which it calls)
			// will wait *at least* the duration given. Since this go
			// routine starts sometime after we record the start time, and
			// it will wake up the loop below sometime after the broadcast,
			// we don't need to worry about waking it up before the time
			// has expired accidentally.
			<-w.clock.After(blockTimeout)
			w.cond.Broadcast()
		}()
	}

	w.RLock()
	span := tracing.SpanFromContext(ctx)
	span.AddEvent("watchCache locked acquired")
	for w.resourceVersion < resourceVersion {
		if w.clock.Since(startTime) >= blockTimeout {
			// Request that the client retry after 'resourceVersionTooHighRetrySeconds' seconds.
			return storage.NewTooLargeResourceVersionError(resourceVersion, w.resourceVersion, resourceVersionTooHighRetrySeconds)
		}
		w.cond.Wait()
	}
	span.AddEvent("watchCache fresh enough")
	return nil
}

type sortableStoreElements []interface{}

func (s sortableStoreElements) Len() int {
	return len(s)
}

func (s sortableStoreElements) Less(i, j int) bool {
	return s[i].(*storeElement).Key < s[j].(*storeElement).Key
}

func (s sortableStoreElements) Swap(i, j int) {
	s[i], s[j] = s[j], s[i]
}

// WaitUntilFreshAndList returns list of pointers to `storeElement` objects along
// with their ResourceVersion and the name of the index, if any, that was used.
func (w *watchCache) WaitUntilFreshAndList(ctx context.Context, resourceVersion uint64, key string, matchValues []storage.MatchValue) (resp listResp, index string, err error) {
	requestWatchProgressSupported := etcdfeature.DefaultFeatureSupportChecker.Supports(storage.RequestWatchProgress)
	if utilfeature.DefaultFeatureGate.Enabled(features.ConsistentListFromCache) && requestWatchProgressSupported && w.notFresh(resourceVersion) {
		w.waitingUntilFresh.Add()
		err = w.waitUntilFreshAndBlock(ctx, resourceVersion)
		w.waitingUntilFresh.Remove()
	} else {
		err = w.waitUntilFreshAndBlock(ctx, resourceVersion)
	}

	defer w.RUnlock()
	if err != nil {
		return listResp{}, "", err
	}
	// This isn't the place where we do "final filtering" - only some "prefiltering" is happening here. So the only
	// requirement here is to NOT miss anything that should be returned. We can return as many non-matching items as we
	// want - they will be filtered out later. The fact that we return less things is only further performance improvement.
	// TODO: if multiple indexes match, return the one with the fewest items, so as to do as much filtering as possible.
	for _, matchValue := range matchValues {
		if result, err := w.store.ByIndex(matchValue.IndexName, matchValue.Value); err == nil {
			result, err = filterPrefixAndOrder(key, result)
			return listResp{
				Items:           result,
				ResourceVersion: w.resourceVersion,
			}, matchValue.IndexName, err
		}
	}
	if store, ok := w.store.(orderedLister); ok {
		result, _ := store.ListPrefix(key, "", 0)
		return listResp{
			Items:           result,
			ResourceVersion: w.resourceVersion,
		}, "", nil
	}
	result := w.store.List()
	result, err = filterPrefixAndOrder(key, result)
	return listResp{
		Items:           result,
		ResourceVersion: w.resourceVersion,
	}, "", err
}

func filterPrefixAndOrder(prefix string, items []interface{}) ([]interface{}, error) {
	var result []interface{}
	for _, item := range items {
		elem, ok := item.(*storeElement)
		if !ok {
			return nil, fmt.Errorf("non *storeElement returned from storage: %v", item)
		}
		if !hasPathPrefix(elem.Key, prefix) {
			continue
		}
		result = append(result, item)
	}
	sort.Sort(sortableStoreElements(result))
	return result, nil
}

func (w *watchCache) notFresh(resourceVersion uint64) bool {
	w.RLock()
	defer w.RUnlock()
	return resourceVersion > w.resourceVersion
}

// WaitUntilFreshAndGet returns a pointers to <storeElement> object.
func (w *watchCache) WaitUntilFreshAndGet(ctx context.Context, resourceVersion uint64, key string) (interface{}, bool, uint64, error) {
	var err error
	if utilfeature.DefaultFeatureGate.Enabled(features.ConsistentListFromCache) && w.notFresh(resourceVersion) {
		w.waitingUntilFresh.Add()
		err = w.waitUntilFreshAndBlock(ctx, resourceVersion)
		w.waitingUntilFresh.Remove()
	} else {
		err = w.waitUntilFreshAndBlock(ctx, resourceVersion)
	}
	defer w.RUnlock()
	if err != nil {
		return nil, false, 0, err
	}
	value, exists, err := w.store.GetByKey(key)
	return value, exists, w.resourceVersion, err
}

func (w *watchCache) ListKeys() []string {
	return w.store.ListKeys()
}

// Get takes runtime.Object as a parameter. However, it returns
// pointer to <storeElement>.
func (w *watchCache) Get(obj interface{}) (interface{}, bool, error) {
	object, ok := obj.(runtime.Object)
	if !ok {
		return nil, false, fmt.Errorf("obj does not implement runtime.Object interface: %v", obj)
	}
	key, err := w.keyFunc(object)
	if err != nil {
		return nil, false, fmt.Errorf("couldn't compute key: %v", err)
	}

	return w.store.Get(&storeElement{Key: key, Object: object})
}

// GetByKey returns pointer to <storeElement>.
func (w *watchCache) GetByKey(key string) (interface{}, bool, error) {
	return w.store.GetByKey(key)
}

// Replace takes slice of runtime.Object as a parameter.
func (w *watchCache) Replace(objs []interface{}, resourceVersion string) error {
	version, err := w.versioner.ParseResourceVersion(resourceVersion)
	if err != nil {
		return err
	}

	toReplace := make([]interface{}, 0, len(objs))
	for _, obj := range objs {
		object, ok := obj.(runtime.Object)
		if !ok {
			return fmt.Errorf("didn't get runtime.Object for replace: %#v", obj)
		}
		key, err := w.keyFunc(object)
		if err != nil {
			return fmt.Errorf("couldn't compute key: %v", err)
		}
		objLabels, objFields, err := w.getAttrsFunc(object)
		if err != nil {
			return err
		}
		toReplace = append(toReplace, &storeElement{
			Key:    key,
			Object: object,
			Labels: objLabels,
			Fields: objFields,
		})
	}

	w.Lock()
	defer w.Unlock()

	// Ensure startIndex never decreases, so that existing watchCacheInterval
	// instances get "invalid" errors if the try to download from the buffer
	// using their own start/end indexes calculated from previous buffer
	// content.

	// Empty the cyclic buffer, ensuring startIndex doesn't decrease.
	w.startIndex = w.endIndex
	w.removedEventSinceRelist = false

	if err := w.store.Replace(toReplace, resourceVersion); err != nil {
		return err
	}
	w.listResourceVersion = version
	w.resourceVersion = version
	if w.onReplace != nil {
		w.onReplace()
	}
	w.cond.Broadcast()

	metrics.RecordResourceVersion(w.groupResource.String(), version)
	klog.V(3).Infof("Replaced watchCache (rev: %v) ", resourceVersion)
	return nil
}

func (w *watchCache) SetOnReplace(onReplace func()) {
	w.Lock()
	defer w.Unlock()
	w.onReplace = onReplace
}

func (w *watchCache) Resync() error {
	// Nothing to do
	return nil
}

func (w *watchCache) getListResourceVersion() uint64 {
	w.RLock()
	defer w.RUnlock()
	return w.listResourceVersion
}

func (w *watchCache) currentCapacity() int {
	w.RLock()
	defer w.RUnlock()
	return w.capacity
}

const (
	// minWatchChanSize is the min size of channels used by the watch.
	// We keep that set to 10 for "backward compatibility" until we
	// convince ourselves based on some metrics that decreasing is safe.
	minWatchChanSize = 10
	// maxWatchChanSizeWithIndexAndTrigger is the max size of the channel
	// used by the watch using the index and trigger selector.
	maxWatchChanSizeWithIndexAndTrigger = 10
	// maxWatchChanSizeWithIndexWithoutTrigger is the max size of the channel
	// used by the watch using the index but without triggering selector.
	// We keep that set to 1000 for "backward compatibility", until we
	// convinced ourselves based on some metrics that decreasing is safe.
	maxWatchChanSizeWithIndexWithoutTrigger = 1000
	// maxWatchChanSizeWithoutIndex is the max size of the channel
	// used by the watch not using the index.
	// TODO(wojtek-t): Figure out if the value shouldn't be higher.
	maxWatchChanSizeWithoutIndex = 100
)

func (w *watchCache) suggestedWatchChannelSize(indexExists, triggerUsed bool) int {
	// To estimate the channel size we use a heuristic that a channel
	// should roughly be able to keep one second of history.
	// We don't have an exact data, but given we store updates from
	// the last <eventFreshDuration>, we approach it by dividing the
	// capacity by the length of the history window.
	chanSize := int(math.Ceil(float64(w.currentCapacity()) / w.eventFreshDuration.Seconds()))

	// Finally we adjust the size to avoid ending with too low or
	// to large values.
	if chanSize < minWatchChanSize {
		chanSize = minWatchChanSize
	}
	var maxChanSize int
	switch {
	case indexExists && triggerUsed:
		maxChanSize = maxWatchChanSizeWithIndexAndTrigger
	case indexExists && !triggerUsed:
		maxChanSize = maxWatchChanSizeWithIndexWithoutTrigger
	case !indexExists:
		maxChanSize = maxWatchChanSizeWithoutIndex
	}
	if chanSize > maxChanSize {
		chanSize = maxChanSize
	}
	return chanSize
}

// isIndexValidLocked checks if a given index is still valid.
// This assumes that the lock is held.
func (w *watchCache) isIndexValidLocked(index int) bool {
	return index >= w.startIndex
}

// getAllEventsSinceLocked returns a watchCacheInterval that can be used to
// retrieve events since a certain resourceVersion. This function assumes to
// be called under the watchCache lock.
func (w *watchCache) getAllEventsSinceLocked(resourceVersion uint64, key string, opts storage.ListOptions) (*watchCacheInterval, error) {
	_, matchesSingle := opts.Predicate.MatchesSingle()
	matchesSingle = matchesSingle && !opts.Recursive
	if opts.SendInitialEvents != nil && *opts.SendInitialEvents {
		return w.getIntervalFromStoreLocked(key, matchesSingle)
	}

	size := w.endIndex - w.startIndex
	var oldest uint64
	switch {
	case w.listResourceVersion > 0 && !w.removedEventSinceRelist:
		// If no event was removed from the buffer since last relist, the oldest watch
		// event we can deliver is one greater than the resource version of the list.
		oldest = w.listResourceVersion + 1
	case size > 0:
		// If the previous condition is not satisfied: either some event was already
		// removed from the buffer or we've never completed a list (the latter can
		// only happen in unit tests that populate the buffer without performing
		// list/replace operations), the oldest watch event we can deliver is the first
		// one in the buffer.
		oldest = w.cache[w.startIndex%w.capacity].ResourceVersion
	default:
		return nil, fmt.Errorf("watch cache isn't correctly initialized")
	}

	if resourceVersion == 0 {
		if opts.SendInitialEvents == nil {
			// resourceVersion = 0 means that we don't require any specific starting point
			// and we would like to start watching from ~now.
			// However, to keep backward compatibility, we additionally need to return the
			// current state and only then start watching from that point.
			//
			// TODO: In v2 api, we should stop returning the current state - #13969.
			return w.getIntervalFromStoreLocked(key, matchesSingle)
		}
		// SendInitialEvents = false and resourceVersion = 0
		// means that the request would like to start watching
		// from Any resourceVersion
		resourceVersion = w.resourceVersion
	}
	if resourceVersion < oldest-1 {
		return nil, errors.NewResourceExpired(fmt.Sprintf("too old resource version: %d (%d)", resourceVersion, oldest-1))
	}

	// Binary search the smallest index at which resourceVersion is greater than the given one.
	f := func(i int) bool {
		return w.cache[(w.startIndex+i)%w.capacity].ResourceVersion > resourceVersion
	}
	first := sort.Search(size, f)
	indexerFunc := func(i int) *watchCacheEvent {
		return w.cache[i%w.capacity]
	}
	ci := newCacheInterval(w.startIndex+first, w.endIndex, indexerFunc, w.indexValidator, resourceVersion, w.RWMutex.RLocker())
	return ci, nil
}

// getIntervalFromStoreLocked returns a watchCacheInterval
// that covers the entire storage state.
// This function assumes to be called under the watchCache lock.
func (w *watchCache) getIntervalFromStoreLocked(key string, matchesSingle bool) (*watchCacheInterval, error) {
	ci, err := newCacheIntervalFromStore(w.resourceVersion, w.store, key, matchesSingle)
	if err != nil {
		return nil, err
	}
	return ci, nil
}
