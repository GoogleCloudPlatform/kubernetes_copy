/*
Copyright 2023 The Kubernetes Authors.

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
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/apiserver/pkg/storage"
	"k8s.io/apiserver/pkg/storage/cacher/metrics"
	utilflowcontrol "k8s.io/apiserver/pkg/util/flowcontrol"

	"k8s.io/klog/v2"
)

// possible states of the cache watcher
const (
	// cacheWatcherWaitingForBookmark indicates the cacher
	// is waiting for a bookmark event with a specific RV set
	cacheWatcherWaitingForBookmark = iota

	// cacheWatcherBookmarkReceived indicates that the cacher
	// has received a bookmark event with required RV
	cacheWatcherBookmarkReceived

	// cacheWatcherBookmarkSent indicates that the cacher
	// has already sent a bookmark event to a client
	cacheWatcherBookmarkSent
)

// cacheWatcher implements watch.Interface
// this is not thread-safe
type cacheWatcher struct {
	input     chan *watchCacheEvent
	result    chan watch.Event
	done      chan struct{}
	filter    filterWithAttrsFunc
	stopped   bool
	forget    func(bool)
	versioner storage.Versioner
	// The watcher will be closed by server after the deadline,
	// save it here to send bookmark events before that.
	deadline            time.Time
	allowWatchBookmarks bool
	groupResource       schema.GroupResource

	// human readable identifier that helps assigning cacheWatcher
	// instance with request
	identifier string

	// drainInputBuffer indicates whether we should delay closing this watcher
	// and send all event in the input buffer.
	drainInputBuffer bool

	// bookmarkAfterResourceVersion holds an RV that indicates
	// when we should start delivering bookmark events.
	// If this field holds the value of 0 that means
	// we don't have any special preferences toward delivering bookmark events.
	// Note that this field is used in conjunction with the state field.
	// It should not be changed once the watcher has been started.
	bookmarkAfterResourceVersion uint64

	// stateMutex protects state
	stateMutex sync.Mutex

	// state holds a numeric value indicating the current state of the watcher
	state int
}

func newCacheWatcher(
	chanSize int,
	filter filterWithAttrsFunc,
	forget func(bool),
	versioner storage.Versioner,
	deadline time.Time,
	allowWatchBookmarks bool,
	groupResource schema.GroupResource,
	identifier string,
) *cacheWatcher {
	return &cacheWatcher{
		input:               make(chan *watchCacheEvent, chanSize),
		result:              make(chan watch.Event, chanSize),
		done:                make(chan struct{}),
		filter:              filter,
		stopped:             false,
		forget:              forget,
		versioner:           versioner,
		deadline:            deadline,
		allowWatchBookmarks: allowWatchBookmarks,
		groupResource:       groupResource,
		identifier:          identifier,
	}
}

// Implements watch.Interface.
func (c *cacheWatcher) ResultChan() <-chan watch.Event {
	return c.result
}

// Implements watch.Interface.
func (c *cacheWatcher) Stop() {
	c.forget(false)
}

// we rely on the fact that stopLocked is actually protected by Cacher.Lock()
func (c *cacheWatcher) stopLocked() {
	if !c.stopped {
		c.stopped = true
		// stop without draining the input channel was requested.
		if !c.drainInputBuffer {
			close(c.done)
		}
		close(c.input)
	}

	// Even if the watcher was already stopped, if it previously was
	// using draining mode and it's not using it now we need to
	// close the done channel now. Otherwise we could leak the
	// processing goroutine if it will be trying to put more objects
	// into result channel, the channel will be full and there will
	// already be noone on the processing the events on the receiving end.
	if !c.drainInputBuffer && !c.isDoneChannelClosedLocked() {
		close(c.done)
	}
}

func (c *cacheWatcher) nonblockingAdd(event *watchCacheEvent) bool {
	// if the bookmarkAfterResourceVersion hasn't been seen
	// we will try to deliver a bookmark event every second.
	// the following check will discard a bookmark event
	// if it is < than the bookmarkAfterResourceVersion
	// so that we don't pollute the input channel
	if event.Type == watch.Bookmark && event.ResourceVersion < c.bookmarkAfterResourceVersion {
		return false
	}
	select {
	case c.input <- event:
		c.markBookmarkAfterRvAsReceived(event)
		return true
	default:
		return false
	}
}

// Nil timer means that add will not block (if it can't send event immediately, it will break the watcher)
//
// Note that bookmark events are never added via the add method only via the nonblockingAdd.
// Changing this behaviour will require moving the markBookmarkAfterRvAsReceived method
func (c *cacheWatcher) add(event *watchCacheEvent, timer *time.Timer) bool {
	// Try to send the event immediately, without blocking.
	if c.nonblockingAdd(event) {
		return true
	}

	closeFunc := func() {
		// This means that we couldn't send event to that watcher.
		// Since we don't want to block on it infinitely,
		// we simply terminate it.
		metrics.TerminatedWatchersCounter.WithLabelValues(c.groupResource.String()).Inc()
		// This means that we couldn't send event to that watcher.
		// Since we don't want to block on it infinitely, we simply terminate it.

		// we are graceful = false, when:
		//
		// (a) The bookmarkAfterResourceVersionReceived hasn't been received,
		//     we can safely terminate the watcher. Because the client is waiting
		//     for this specific bookmark, and we even haven't received one.
		// (b) We have seen the bookmarkAfterResourceVersion, and it was sent already to the client.
		//     We can simply terminate the watcher.

		// we are graceful = true, when:
		//
		// (a) We have seen a bookmark, but it hasn't been sent to the client yet.
		//     That means we should drain the input buffer which contains
		//     the bookmarkAfterResourceVersion we want. We do that to make progress
		//     as clients can re-establish a new watch with the given RV and receive
		//     further notifications.
		graceful := func() bool {
			c.stateMutex.Lock()
			defer c.stateMutex.Unlock()
			return c.state == cacheWatcherBookmarkReceived
		}()
		klog.V(1).Infof("Forcing %v watcher close due to unresponsiveness: %v. len(c.input) = %v, len(c.result) = %v, graceful = %v", c.groupResource.String(), c.identifier, len(c.input), len(c.result), graceful)
		c.forget(graceful)
	}

	if timer == nil {
		closeFunc()
		return false
	}

	// OK, block sending, but only until timer fires.
	select {
	case c.input <- event:
		return true
	case <-timer.C:
		closeFunc()
		return false
	}
}

func (c *cacheWatcher) nextBookmarkTime(now time.Time, bookmarkFrequency time.Duration) (time.Time, bool) {
	// We try to send bookmarks:
	//
	// (a) right before the watcher timeout - for now we simply set it 2s before
	//     the deadline
	//
	// (b) roughly every minute
	//
	// (c) immediately when the bookmarkAfterResourceVersion wasn't confirmed
	//     in this scenario the client have already seen (or is in the process of sending)
	//     all initial data and is interested in seeing
	//     a specific RV value (aka. the bookmarkAfterResourceVersion)
	//     since we don't know when the cacher will see the RV we increase frequency
	//
	// (b) gives us periodicity if the watch breaks due to unexpected
	// conditions, (a) ensures that on timeout the watcher is as close to
	// now as possible - this covers 99% of cases.

	if !c.wasBookmarkAfterRvReceived() {
		return time.Time{}, true // schedule immediately
	}

	heartbeatTime := now.Add(bookmarkFrequency)
	if c.deadline.IsZero() {
		// Timeout is set by our client libraries (e.g. reflector) as well as defaulted by
		// apiserver if properly configured. So this shouldn't happen in practice.
		return heartbeatTime, true
	}
	if pretimeoutTime := c.deadline.Add(-2 * time.Second); pretimeoutTime.Before(heartbeatTime) {
		heartbeatTime = pretimeoutTime
	}

	if heartbeatTime.Before(now) {
		return time.Time{}, false
	}
	return heartbeatTime, true
}

// wasBookmarkAfterRvReceived same as wasBookmarkAfterRvReceivedLocked just acquires a lock
func (c *cacheWatcher) wasBookmarkAfterRvReceived() bool {
	c.stateMutex.Lock()
	defer c.stateMutex.Unlock()
	return c.wasBookmarkAfterRvReceivedLocked()
}

// wasBookmarkAfterRvReceivedLocked checks if the given cacheWatcher
// have seen a bookmark event >= bookmarkAfterResourceVersion
func (c *cacheWatcher) wasBookmarkAfterRvReceivedLocked() bool {
	return c.state != cacheWatcherWaitingForBookmark
}

// markBookmarkAfterRvAsReceived indicates that the given cacheWatcher
// have seen a bookmark event >= bookmarkAfterResourceVersion
func (c *cacheWatcher) markBookmarkAfterRvAsReceived(event *watchCacheEvent) {
	if event.Type == watch.Bookmark {
		c.stateMutex.Lock()
		defer c.stateMutex.Unlock()
		if c.wasBookmarkAfterRvReceivedLocked() {
			return
		}
		// bookmark events are scheduled by startDispatchingBookmarkEvents method
		// since we received a bookmark event that means we have
		// converged towards the expected RV and it is okay to update the state so that
		// this cacher can be scheduler for a regular bookmark events
		c.state = cacheWatcherBookmarkReceived
	}
}

// wasBookmarkAfterRvSentLocked checks if a bookmark event
// with an RV >= the bookmarkAfterResourceVersion has been sent by this watcher
func (c *cacheWatcher) wasBookmarkAfterRvSentLocked() bool {
	return c.state == cacheWatcherBookmarkSent
}

// wasBookmarkAfterRvSent same as wasBookmarkAfterRvSentLocked just acquires a lock
func (c *cacheWatcher) wasBookmarkAfterRvSent() bool {
	c.stateMutex.Lock()
	defer c.stateMutex.Unlock()
	return c.wasBookmarkAfterRvSentLocked()
}

// markBookmarkAfterRvSent indicates that the given cacheWatcher
// have sent a bookmark event with an RV >= the bookmarkAfterResourceVersion
//
// this function relies on the fact that the nonblockingAdd method
// won't admit a bookmark event with an RV < the bookmarkAfterResourceVersion
// so the first received bookmark event is considered to match the bookmarkAfterResourceVersion
func (c *cacheWatcher) markBookmarkAfterRvSent(event *watchCacheEvent) {
	// note that bookmark events are not so common so will acquire a lock every ~60 second or so
	if event.Type == watch.Bookmark {
		c.stateMutex.Lock()
		defer c.stateMutex.Unlock()
		if !c.wasBookmarkAfterRvSentLocked() {
			c.state = cacheWatcherBookmarkSent
		}
	}
}

// setBookmarkAfterResourceVersion sets the bookmarkAfterResourceVersion and the state associated with it
func (c *cacheWatcher) setBookmarkAfterResourceVersion(bookmarkAfterResourceVersion uint64) {
	state := cacheWatcherWaitingForBookmark
	if bookmarkAfterResourceVersion == 0 {
		state = cacheWatcherBookmarkSent // if no specific RV was requested we assume no-op
	}
	c.state = state
	c.bookmarkAfterResourceVersion = bookmarkAfterResourceVersion
}

// setDrainInputBufferLocked if set to true indicates that we should delay closing this watcher
// until we send all events residing in the input buffer.
func (c *cacheWatcher) setDrainInputBufferLocked(drain bool) {
	c.drainInputBuffer = drain
}

// isDoneChannelClosed checks if c.done channel is closed
func (c *cacheWatcher) isDoneChannelClosedLocked() bool {
	select {
	case <-c.done:
		return true
	default:
	}
	return false
}

func getMutableObject(object runtime.Object) runtime.Object {
	if _, ok := object.(*cachingObject); ok {
		// It is safe to return without deep-copy, because the underlying
		// object will lazily perform deep-copy on the first try to change
		// any of its fields.
		return object
	}
	return object.DeepCopyObject()
}

func updateResourceVersion(object runtime.Object, versioner storage.Versioner, resourceVersion uint64) {
	if err := versioner.UpdateObject(object, resourceVersion); err != nil {
		utilruntime.HandleError(fmt.Errorf("failure to version api object (%d) %#v: %v", resourceVersion, object, err))
	}
}

func (c *cacheWatcher) convertToWatchEvent(event *watchCacheEvent) *watch.Event {
	if event.Type == watch.Bookmark {
		e := &watch.Event{Type: watch.Bookmark, Object: event.Object.DeepCopyObject()}
		if !c.wasBookmarkAfterRvSent() {
			if err := storage.AnnotateInitialEventsEndBookmark(e.Object); err != nil {
				utilruntime.HandleError(fmt.Errorf("error while accessing object's metadata gr: %v, identifier: %v, obj: %#v, err: %v", c.groupResource, c.identifier, e.Object, err))
				return nil
			}
		}
		return e
	}

	curObjPasses := event.Type != watch.Deleted && c.filter(event.Key, event.ObjLabels, event.ObjFields)
	oldObjPasses := false
	if event.PrevObject != nil {
		oldObjPasses = c.filter(event.Key, event.PrevObjLabels, event.PrevObjFields)
	}
	if !curObjPasses && !oldObjPasses {
		// Watcher is not interested in that object.
		return nil
	}

	switch {
	case curObjPasses && !oldObjPasses:
		return &watch.Event{Type: watch.Added, Object: getMutableObject(event.Object)}
	case curObjPasses && oldObjPasses:
		return &watch.Event{Type: watch.Modified, Object: getMutableObject(event.Object)}
	case !curObjPasses && oldObjPasses:
		// return a delete event with the previous object content, but with the event's resource version
		oldObj := getMutableObject(event.PrevObject)
		// We know that if oldObj is cachingObject (which can only be set via
		// setCachingObjects), its resourceVersion is already set correctly and
		// we don't need to update it. However, since cachingObject efficiently
		// handles noop updates, we avoid this microoptimization here.
		updateResourceVersion(oldObj, c.versioner, event.ResourceVersion)
		return &watch.Event{Type: watch.Deleted, Object: oldObj}
	}

	return nil
}

// NOTE: sendWatchCacheEvent is assumed to not modify <event> !!!
func (c *cacheWatcher) sendWatchCacheEvent(event *watchCacheEvent) {
	watchEvent := c.convertToWatchEvent(event)
	if watchEvent == nil {
		// Watcher is not interested in that object.
		return
	}

	// We need to ensure that if we put event X to the c.result, all
	// previous events were already put into it before, no matter whether
	// c.done is close or not.
	// Thus we cannot simply select from c.done and c.result and this
	// would give us non-determinism.
	// At the same time, we don't want to block infinitely on putting
	// to c.result, when c.done is already closed.
	//
	// This ensures that with c.done already close, we at most once go
	// into the next select after this. With that, no matter which
	// statement we choose there, we will deliver only consecutive
	// events.
	select {
	case <-c.done:
		return
	default:
	}

	select {
	case c.result <- *watchEvent:
		c.markBookmarkAfterRvSent(event)
	case <-c.done:
	}
}

func (c *cacheWatcher) processInterval(ctx context.Context, cacheInterval *watchCacheInterval, resourceVersion uint64) {
	defer utilruntime.HandleCrash()
	defer close(c.result)
	defer c.Stop()

	// Check how long we are processing initEvents.
	// As long as these are not processed, we are not processing
	// any incoming events, so if it takes long, we may actually
	// block all watchers for some time.
	// TODO: From the logs it seems that there happens processing
	// times even up to 1s which is very long. However, this doesn't
	// depend that much on the number of initEvents. E.g. from the
	// 2000-node Kubemark run we have logs like this, e.g.:
	// ... processing 13862 initEvents took 66.808689ms
	// ... processing 14040 initEvents took 993.532539ms
	// We should understand what is blocking us in those cases (e.g.
	// is it lack of CPU, network, or sth else) and potentially
	// consider increase size of result buffer in those cases.
	const initProcessThreshold = 500 * time.Millisecond
	startTime := time.Now()

	// cacheInterval may be created from a version being more fresh than requested
	// (e.g. for NotOlderThan semantic). In such a case, we need to prevent watch event
	// with lower resourceVersion from being delivered to ensure watch contract.
	if cacheInterval.resourceVersion > resourceVersion {
		resourceVersion = cacheInterval.resourceVersion
	}

	initEventCount := 0
	for {
		event, err := cacheInterval.Next()
		if err != nil {
			// An error indicates that the cache interval
			// has been invalidated and can no longer serve
			// events.
			//
			// Initially we considered sending an "out-of-history"
			// Error event in this case, but because historically
			// such events weren't sent out of the watchCache, we
			// decided not to. This is still ok, because on watch
			// closure, the watcher will try to re-instantiate the
			// watch and then will get an explicit "out-of-history"
			// window. There is potential for optimization, but for
			// now, in order to be on the safe side and not break
			// custom clients, the cost of it is something that we
			// are fully accepting.
			klog.Warningf("couldn't retrieve watch event to serve: %#v", err)
			return
		}
		if event == nil {
			break
		}
		c.sendWatchCacheEvent(event)

		// With some events already sent, update resourceVersion so that
		// events that were buffered and not yet processed won't be delivered
		// to this watcher second time causing going back in time.
		//
		// There is one case where events are not necessary ordered by
		// resourceVersion, being a case of watching from resourceVersion=0,
		// which at the beginning returns the state of each objects.
		// For the purpose of it, we need to max it with the resource version
		// that we have so far.
		if event.ResourceVersion > resourceVersion {
			resourceVersion = event.ResourceVersion
		}
		initEventCount++
	}

	if initEventCount > 0 {
		metrics.InitCounter.WithLabelValues(c.groupResource.String()).Add(float64(initEventCount))
	}
	processingTime := time.Since(startTime)
	if processingTime > initProcessThreshold {
		klog.V(2).Infof("processing %d initEvents of %s (%s) took %v", initEventCount, c.groupResource, c.identifier, processingTime)
	}

	// send bookmark after sending all events in cacheInterval for watchlist request
	if cacheInterval.initialEventsEndBookmark != nil {
		c.sendWatchCacheEvent(cacheInterval.initialEventsEndBookmark)
	}
	c.process(ctx, resourceVersion)
}

func (c *cacheWatcher) process(ctx context.Context, resourceVersion uint64) {
	// At this point we already start processing incoming watch events.
	// However, the init event can still be processed because their serialization
	// and sending to the client happens asynchronously.
	// TODO: As describe in the KEP, we would like to estimate that by delaying
	//   the initialization signal proportionally to the number of events to
	//   process, but we're leaving this to the tuning phase.
	utilflowcontrol.WatchInitialized(ctx)

	for {
		select {
		case event, ok := <-c.input:
			if !ok {
				return
			}
			// only send events newer than resourceVersion
			// or a bookmark event with an RV equal to resourceVersion
			// if we haven't sent one to the client
			if event.ResourceVersion > resourceVersion || (event.Type == watch.Bookmark && event.ResourceVersion == resourceVersion && !c.wasBookmarkAfterRvSent()) {
				c.sendWatchCacheEvent(event)
			}
		case <-ctx.Done():
			return
		}
	}
}
