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
	"errors"
	"fmt"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/util/naming"
	"k8s.io/apimachinery/pkg/util/sets"

	"k8s.io/klog/v2"
	utiltrace "k8s.io/utils/trace"
)

// DeltaFIFOOptions is the configuration parameters for DeltaFIFO. All are
// optional.
type DeltaFIFOOptions struct {

	// KeyFunction is used to figure out what key an object should have. (It's
	// exposed in the returned DeltaFIFO's KeyOf() method, with additional
	// handling around deleted objects and queue state).
	// Optional, the default is MetaNamespaceKeyFunc.
	KeyFunction KeyFunc

	// KnownObjects is expected to return a list of keys that the consumer of
	// this queue "knows about". It is used to decide which items are missing
	// when Replace() is called; 'Deleted' deltas are produced for the missing items.
	// KnownObjects may be nil if you can tolerate missing deletions on Replace().
	KnownObjects KeyListerGetter

	// EmitDeltaTypeReplaced indicates that the queue consumer
	// understands the Replaced DeltaType. Before the `Replaced` event type was
	// added, calls to Replace() were handled the same as Sync(). For
	// backwards-compatibility purposes, this is false by default.
	// When true, `Replaced` events will be sent for items passed to a Replace() call.
	// When false, `Sync` events will be sent instead.
	EmitDeltaTypeReplaced bool

	// If set, will be called for objects before enqueueing them. Please
	// see the comment on TransformFunc for details.
	Transformer TransformFunc

	// If set, log output will go to this logger instead of klog.Background().
	Logger *klog.Logger

	// If set, metrics will be collected for the informer.
	Metrics *informerMetrics
}

// DeltaFIFO is like FIFO, but differs in two ways.  One is that the
// accumulator associated with a given object's key is not that object
// but rather a Deltas, which is a slice of Delta values for that
// object.  Applying an object to a Deltas means to append a Delta
// except when the potentially appended Delta is a Deleted and the
// Deltas already ends with a Deleted.  In that case the Deltas does
// not grow, although the terminal Deleted will be replaced by the new
// Deleted if the older Deleted's object is a
// DeletedFinalStateUnknown.
//
// The other difference is that DeltaFIFO has two additional ways that
// an object can be applied to an accumulator: Replaced and Sync.
// If EmitDeltaTypeReplaced is not set to true, Sync will be used in
// replace events for backwards compatibility.  Sync is used for periodic
// resync events.
//
// DeltaFIFO is a producer-consumer queue, where a Reflector is
// intended to be the producer, and the consumer is whatever calls
// the Pop() method.
//
// DeltaFIFO solves this use case:
//   - You want to process every object change (delta) at most once.
//   - When you process an object, you want to see everything
//     that's happened to it since you last processed it.
//   - You want to process the deletion of some of the objects.
//   - You might want to periodically reprocess objects.
//
// DeltaFIFO's Pop(), Get(), and GetByKey() methods return
// interface{} to satisfy the Store/Queue interfaces, but they
// will always return an object of type Deltas. List() returns
// the newest object from each accumulator in the FIFO.
//
// A DeltaFIFO's knownObjects KeyListerGetter provides the abilities
// to list Store keys and to get objects by Store key.  The objects in
// question are called "known objects" and this set of objects
// modifies the behavior of the Delete, Replace, and Resync methods
// (each in a different way).
//
// A note on threading: If you call Pop() in parallel from multiple
// threads, you could end up with multiple threads processing slightly
// different versions of the same object.
type DeltaFIFO struct {
	// lock/cond protects access to 'items' and 'queue'.
	lock sync.RWMutex
	cond sync.Cond

	// `items` maps a key to a Deltas.
	// Each such Deltas has at least one Delta.
	items map[string]Deltas

	// `queue` maintains FIFO order of keys for consumption in Pop().
	// There are no duplicates in `queue`.
	// A key is in `queue` if and only if it is in `items`.
	queue []string

	// populated is true if the first batch of items inserted by Replace() has been populated
	// or Delete/Add/Update/AddIfNotPresent was called first.
	populated bool
	// initialPopulationCount is the number of items inserted by the first call of Replace()
	initialPopulationCount int

	// keyFunc is used to make the key used for queued item
	// insertion and retrieval, and should be deterministic.
	keyFunc KeyFunc

	// knownObjects list keys that are "known" --- affecting Delete(),
	// Replace(), and Resync()
	knownObjects KeyListerGetter

	// Used to indicate a queue is closed so a control loop can exit when a queue is empty.
	// Currently, not used to gate any of CRUD operations.
	closed bool

	// emitDeltaTypeReplaced is whether to emit the Replaced or Sync
	// DeltaType when Replace() is called (to preserve backwards compat).
	emitDeltaTypeReplaced bool

	// Called with every object if non-nil.
	transformer TransformFunc

	// logger is a per-instance logger. This gets chosen when constructing
	// the instance, with klog.Background() as default.
	logger klog.Logger

	// metrics tracks basic metric information about the informer.
	metrics *informerMetrics
}

// TransformFunc allows for transforming an object before it will be processed.
//
// The most common usage pattern is to clean-up some parts of the object to
// reduce component memory usage if a given component doesn't care about them.
//
// New in v1.27: TransformFunc sees the object before any other actor, and it
// is now safe to mutate the object in place instead of making a copy.
//
// It's recommended for the TransformFunc to be idempotent.
// It MUST be idempotent if objects already present in the cache are passed to
// the Replace() to avoid re-mutating them. Default informers do not pass
// existing objects to Replace though.
//
// Note that TransformFunc is called while inserting objects into the
// notification queue and is therefore extremely performance sensitive; please
// do not do anything that will take a long time.
type TransformFunc func(interface{}) (interface{}, error)

// DeltaType is the type of a change (addition, deletion, etc)
type DeltaType string

// Change type definition
const (
	Added   DeltaType = "Added"
	Updated DeltaType = "Updated"
	Deleted DeltaType = "Deleted"
	// Replaced is emitted when we encountered watch errors and had to do a
	// relist. We don't know if the replaced object has changed.
	//
	// NOTE: Previous versions of DeltaFIFO would use Sync for Replace events
	// as well. Hence, Replaced is only emitted when the option
	// EmitDeltaTypeReplaced is true.
	Replaced DeltaType = "Replaced"
	// Sync is for synthetic events during a periodic resync.
	Sync DeltaType = "Sync"
)

// Delta is a member of Deltas (a list of Delta objects) which
// in its turn is the type stored by a DeltaFIFO. It tells you what
// change happened, and the object's state after* that change.
//
// [*] Unless the change is a deletion, and then you'll get the final
// state of the object before it was deleted.
type Delta struct {
	Type   DeltaType
	Object interface{}
}

// Deltas is a list of one or more 'Delta's to an individual object.
// The oldest delta is at index 0, the newest delta is the last one.
type Deltas []Delta

// NewDeltaFIFO returns a Queue which can be used to process changes to items.
//
// keyFunc is used to figure out what key an object should have. (It is
// exposed in the returned DeltaFIFO's KeyOf() method, with additional handling
// around deleted objects and queue state).
//
// 'knownObjects' may be supplied to modify the behavior of Delete,
// Replace, and Resync.  It may be nil if you do not need those
// modifications.
//
// TODO: consider merging keyLister with this object, tracking a list of
// "known" keys when Pop() is called. Have to think about how that
// affects error retrying.
//
//	NOTE: It is possible to misuse this and cause a race when using an
//	external known object source.
//	Whether there is a potential race depends on how the consumer
//	modifies knownObjects. In Pop(), process function is called under
//	lock, so it is safe to update data structures in it that need to be
//	in sync with the queue (e.g. knownObjects).
//
//	Example:
//	In case of sharedIndexInformer being a consumer
//	(https://github.com/kubernetes/kubernetes/blob/0cdd940f/staging/src/k8s.io/client-go/tools/cache/shared_informer.go#L192),
//	there is no race as knownObjects (s.indexer) is modified safely
//	under DeltaFIFO's lock. The only exceptions are GetStore() and
//	GetIndexer() methods, which expose ways to modify the underlying
//	storage. Currently these two methods are used for creating Lister
//	and internal tests.
//
// Also see the comment on DeltaFIFO.
//
// Warning: This constructs a DeltaFIFO that does not differentiate between
// events caused by a call to Replace (e.g., from a relist, which may
// contain object updates), and synthetic events caused by a periodic resync
// (which just emit the existing object). See https://issue.k8s.io/86015 for details.
//
// Use `NewDeltaFIFOWithOptions(DeltaFIFOOptions{..., EmitDeltaTypeReplaced: true})`
// instead to receive a `Replaced` event depending on the type.
//
// Deprecated: Equivalent to NewDeltaFIFOWithOptions(DeltaFIFOOptions{KeyFunction: keyFunc, KnownObjects: knownObjects})
func NewDeltaFIFO(keyFunc KeyFunc, knownObjects KeyListerGetter) *DeltaFIFO {
	return NewDeltaFIFOWithOptions(DeltaFIFOOptions{
		KeyFunction:  keyFunc,
		KnownObjects: knownObjects,
	})
}

// NewDeltaFIFOWithOptions returns a Queue which can be used to process changes to
// items. See also the comment on DeltaFIFO.
func NewDeltaFIFOWithOptions(opts DeltaFIFOOptions) *DeltaFIFO {
	if opts.KeyFunction == nil {
		opts.KeyFunction = MetaNamespaceKeyFunc
	}

	if opts.Metrics == nil {
		opts.Metrics = newInformerMetrics(makeValidPromethusMetricName(fmt.Sprintf("deltafifo_%s", naming.GetNameFromCallsite(internalPackages...))))
	}

	f := &DeltaFIFO{
		items:        map[string]Deltas{},
		queue:        []string{},
		keyFunc:      opts.KeyFunction,
		knownObjects: opts.KnownObjects,

		emitDeltaTypeReplaced: opts.EmitDeltaTypeReplaced,
		transformer:           opts.Transformer,
		logger:                klog.Background(),
		metrics:               opts.Metrics,
	}
	if opts.Logger != nil {
		f.logger = *opts.Logger
	}
	f.cond.L = &f.lock
	return f
}

var (
	_ = Queue(&DeltaFIFO{}) // DeltaFIFO is a Queue
)

var (
	// ErrZeroLengthDeltasObject is returned in a KeyError if a Deltas
	// object with zero length is encountered (should be impossible,
	// but included for completeness).
	ErrZeroLengthDeltasObject = errors.New("0 length Deltas object; can't get key")
)

// Close the queue.
func (f *DeltaFIFO) Close() {
	f.lock.Lock()
	defer f.lock.Unlock()
	f.closed = true
	f.cond.Broadcast()
}

// KeyOf exposes f's keyFunc, but also detects the key of a Deltas object or
// DeletedFinalStateUnknown objects.
func (f *DeltaFIFO) KeyOf(obj interface{}) (string, error) {
	if d, ok := obj.(Deltas); ok {
		if len(d) == 0 {
			return "", KeyError{obj, ErrZeroLengthDeltasObject}
		}
		obj = d.Newest().Object
	}
	if d, ok := obj.(DeletedFinalStateUnknown); ok {
		return d.Key, nil
	}
	return f.keyFunc(obj)
}

// HasSynced returns true if an Add/Update/Delete/AddIfNotPresent are called first,
// or the first batch of items inserted by Replace() has been popped.
func (f *DeltaFIFO) HasSynced() bool {
	f.lock.Lock()
	defer f.lock.Unlock()
	return f.hasSynced_locked()
}

func (f *DeltaFIFO) hasSynced_locked() bool {
	return f.populated && f.initialPopulationCount == 0
}

// Add inserts an item, and puts it in the queue. The item is only enqueued
// if it doesn't already exist in the set.
func (f *DeltaFIFO) Add(obj interface{}) error {
	f.lock.Lock()
	defer f.lock.Unlock()
	f.populated = true
	return f.queueActionLocked(Added, obj)
}

// Update is just like Add, but makes an Updated Delta.
func (f *DeltaFIFO) Update(obj interface{}) error {
	f.lock.Lock()
	defer f.lock.Unlock()
	f.populated = true
	return f.queueActionLocked(Updated, obj)
}

// Delete is just like Add, but makes a Deleted Delta. If the given
// object does not already exist, it will be ignored. (It may have
// already been deleted by a Replace (re-list), for example.)  In this
// method `f.knownObjects`, if not nil, provides (via GetByKey)
// _additional_ objects that are considered to already exist.
func (f *DeltaFIFO) Delete(obj interface{}) error {
	id, err := f.KeyOf(obj)
	if err != nil {
		return KeyError{obj, err}
	}
	f.lock.Lock()
	defer f.lock.Unlock()
	f.populated = true
	if f.knownObjects == nil {
		if _, exists := f.items[id]; !exists {
			// Presumably, this was deleted when a relist happened.
			// Don't provide a second report of the same deletion.
			return nil
		}
	} else {
		// We only want to skip the "deletion" action if the object doesn't
		// exist in knownObjects and it doesn't have corresponding item in items.
		// Note that even if there is a "deletion" action in items, we can ignore it,
		// because it will be deduped automatically in "queueActionLocked"
		_, exists, err := f.knownObjects.GetByKey(id)
		_, itemsExist := f.items[id]
		if err == nil && !exists && !itemsExist {
			// Presumably, this was deleted when a relist happened.
			// Don't provide a second report of the same deletion.
			return nil
		}
	}

	// exist in items and/or KnownObjects
	return f.queueActionLocked(Deleted, obj)
}

// re-listing and watching can deliver the same update multiple times in any
// order. This will combine the most recent two deltas if they are the same.
func dedupDeltas(deltas Deltas) Deltas {
	n := len(deltas)
	if n < 2 {
		return deltas
	}
	a := &deltas[n-1]
	b := &deltas[n-2]
	if out := isDup(a, b); out != nil {
		deltas[n-2] = *out
		return deltas[:n-1]
	}
	return deltas
}

// If a & b represent the same event, returns the delta that ought to be kept.
// Otherwise, returns nil.
// TODO: is there anything other than deletions that need deduping?
func isDup(a, b *Delta) *Delta {
	if out := isDeletionDup(a, b); out != nil {
		return out
	}
	// TODO: Detect other duplicate situations? Are there any?
	return nil
}

// keep the one with the most information if both are deletions.
func isDeletionDup(a, b *Delta) *Delta {
	if b.Type != Deleted || a.Type != Deleted {
		return nil
	}
	// Do more sophisticated checks, or is this sufficient?
	if _, ok := b.Object.(DeletedFinalStateUnknown); ok {
		return a
	}
	return b
}

// queueActionLocked appends to the delta list for the object.
// Caller must lock first.
func (f *DeltaFIFO) queueActionLocked(actionType DeltaType, obj interface{}) error {
	return f.queueActionInternalLocked(actionType, actionType, obj)
}

// queueActionInternalLocked appends to the delta list for the object.
// The actionType is emitted and must honor emitDeltaTypeReplaced.
// The internalActionType is only used within this function and must
// ignore emitDeltaTypeReplaced.
// Caller must lock first.
func (f *DeltaFIFO) queueActionInternalLocked(actionType, internalActionType DeltaType, obj interface{}) error {
	defer func() {
		f.metrics.numberOfQueuedItem.Set(float64(len(f.queue)))
		f.metrics.numberOfStoredItem.Set(float64(len(f.items)))
	}()

	id, err := f.KeyOf(obj)
	if err != nil {
		return KeyError{obj, err}
	}

	// Every object comes through this code path once, so this is a good
	// place to call the transform func.
	//
	// If obj is a DeletedFinalStateUnknown tombstone or the action is a Sync,
	// then the object have already gone through the transformer.
	//
	// If the objects already present in the cache are passed to Replace(),
	// the transformer must be idempotent to avoid re-mutating them,
	// or coordinate with all readers from the cache to avoid data races.
	// Default informers do not pass existing objects to Replace.
	if f.transformer != nil {
		_, isTombstone := obj.(DeletedFinalStateUnknown)
		if !isTombstone && internalActionType != Sync {
			var err error
			obj, err = f.transformer(obj)
			if err != nil {
				return err
			}
		}
	}

	oldDeltas := f.items[id]
	newDeltas := append(oldDeltas, Delta{actionType, obj})
	newDeltas = dedupDeltas(newDeltas)

	if len(newDeltas) > 0 {
		if _, exists := f.items[id]; !exists {
			f.queue = append(f.queue, id)
		}
		f.items[id] = newDeltas
		f.cond.Broadcast()
	} else {
		// This never happens, because dedupDeltas never returns an empty list
		// when given a non-empty list (as it is here).
		// If somehow it happens anyway, deal with it but complain.
		if oldDeltas == nil {
			f.logger.Error(nil, "Impossible dedupDeltas, ignoring", "id", id, "oldDeltas", oldDeltas, "obj", obj)
			return nil
		}
		f.logger.Error(nil, "Impossible dedupDeltas, breaking invariant by storing empty Deltas", "id", id, "oldDeltas", oldDeltas, "obj", obj)
		f.items[id] = newDeltas
		return fmt.Errorf("Impossible dedupDeltas for id=%q: oldDeltas=%#+v, obj=%#+v; broke DeltaFIFO invariant by storing empty Deltas", id, oldDeltas, obj)
	}
	return nil
}

// IsClosed checks if the queue is closed
func (f *DeltaFIFO) IsClosed() bool {
	f.lock.Lock()
	defer f.lock.Unlock()
	return f.closed
}

// Pop blocks until the queue has some items, and then returns one.  If
// multiple items are ready, they are returned in the order in which they were
// added/updated. The item is removed from the queue (and the store) before it
// is returned, so if you don't successfully process it, you need to add it back
// with AddIfNotPresent().
// process function is called under lock, so it is safe to update data structures
// in it that need to be in sync with the queue (e.g. knownKeys).
// process should avoid expensive I/O operation so that other queue operations, i.e.
// Add() and Get(), won't be blocked for too long.
//
// Pop returns a 'Deltas', which has a complete list of all the things
// that happened to the object (deltas) while it was sitting in the queue.
func (f *DeltaFIFO) Pop(process PopProcessFunc) (interface{}, error) {
	f.lock.Lock()
	defer f.lock.Unlock()
	for {
		for len(f.queue) == 0 {
			// When the queue is empty, invocation of Pop() is blocked until new item is enqueued.
			// When Close() is called, the f.closed is set and the condition is broadcasted.
			// Which causes this loop to continue and return from the Pop().
			if f.closed {
				return nil, ErrFIFOClosed
			}

			f.cond.Wait()
		}
		isInInitialList := !f.hasSynced_locked()
		id := f.queue[0]
		f.queue = f.queue[1:]
		depth := len(f.queue)
		if f.initialPopulationCount > 0 {
			f.initialPopulationCount--
		}
		item, ok := f.items[id]
		if !ok {
			// This should never happen
			f.logger.Error(nil, "Inconceivable! Item was in f.queue but not f.items; ignoring", "id", id)
			continue
		}
		delete(f.items, id)
		// Only log traces if the queue depth is greater than 10 and it takes more than
		// 100 milliseconds to process one item from the queue.
		// Queue depth never goes high because processing an item is locking the queue,
		// and new items can't be added until processing finish.
		// https://github.com/kubernetes/kubernetes/issues/103789
		if depth > 10 {
			trace := utiltrace.New("DeltaFIFO Pop Process",
				utiltrace.Field{Key: "ID", Value: id},
				utiltrace.Field{Key: "Depth", Value: depth},
				utiltrace.Field{Key: "Reason", Value: "slow event handlers blocking the queue"})
			defer trace.LogIfLong(100 * time.Millisecond)
		}
		err := process(item, isInInitialList)
		// Don't need to copyDeltas here, because we're transferring
		// ownership to the caller.
		return item, err
	}
}

// Replace atomically does two things: (1) it adds the given objects
// using the Sync or Replace DeltaType and then (2) it does some deletions.
// In particular: for every pre-existing key K that is not the key of
// an object in `list` there is the effect of
// `Delete(DeletedFinalStateUnknown{K, O})` where O is the latest known
// object of K. The pre-existing keys are those in the union set of the keys in
// `f.items` and `f.knownObjects` (if not nil). The last known object for key K is
// the one present in the last delta in `f.items`. If there is no delta for K
// in `f.items`, it is the object in `f.knownObjects`
func (f *DeltaFIFO) Replace(list []interface{}, _ string) error {
	f.lock.Lock()
	defer f.lock.Unlock()
	keys := make(sets.String, len(list))

	// keep backwards compat for old clients
	action := Sync
	if f.emitDeltaTypeReplaced {
		action = Replaced
	}

	// Add Sync/Replaced action for each new item.
	for _, item := range list {
		key, err := f.KeyOf(item)
		if err != nil {
			return KeyError{item, err}
		}
		keys.Insert(key)
		if err := f.queueActionInternalLocked(action, Replaced, item); err != nil {
			return fmt.Errorf("couldn't enqueue object: %v", err)
		}
	}

	// Do deletion detection against objects in the queue
	queuedDeletions := 0
	for k, oldItem := range f.items {
		if keys.Has(k) {
			continue
		}
		// Delete pre-existing items not in the new list.
		// This could happen if watch deletion event was missed while
		// disconnected from apiserver.
		var deletedObj interface{}
		if n := oldItem.Newest(); n != nil {
			deletedObj = n.Object

			// if the previous object is a DeletedFinalStateUnknown, we have to extract the actual Object
			if d, ok := deletedObj.(DeletedFinalStateUnknown); ok {
				deletedObj = d.Obj
			}
		}
		queuedDeletions++
		if err := f.queueActionLocked(Deleted, DeletedFinalStateUnknown{k, deletedObj}); err != nil {
			return err
		}
	}

	if f.knownObjects != nil {
		// Detect deletions for objects not present in the queue, but present in KnownObjects
		knownKeys := f.knownObjects.ListKeys()
		for _, k := range knownKeys {
			if keys.Has(k) {
				continue
			}
			if len(f.items[k]) > 0 {
				continue
			}

			deletedObj, exists, err := f.knownObjects.GetByKey(k)
			if err != nil {
				deletedObj = nil
				f.logger.Error(err, "Unexpected error during lookup, placing DeleteFinalStateUnknown marker without object", "key", k)
			} else if !exists {
				deletedObj = nil
				f.logger.Info("Key does not exist in known objects store, placing DeleteFinalStateUnknown marker without object", "key", k)
			}
			queuedDeletions++
			if err := f.queueActionLocked(Deleted, DeletedFinalStateUnknown{k, deletedObj}); err != nil {
				return err
			}
		}
	}

	if !f.populated {
		f.populated = true
		f.initialPopulationCount = keys.Len() + queuedDeletions
	}

	return nil
}

// Resync adds, with a Sync type of Delta, every object listed by
// `f.knownObjects` whose key is not already queued for processing.
// If `f.knownObjects` is `nil` then Resync does nothing.
func (f *DeltaFIFO) Resync() error {
	f.lock.Lock()
	defer f.lock.Unlock()

	if f.knownObjects == nil {
		return nil
	}

	keys := f.knownObjects.ListKeys()
	for _, k := range keys {
		if err := f.syncKeyLocked(k); err != nil {
			return err
		}
	}
	return nil
}

func (f *DeltaFIFO) syncKeyLocked(key string) error {
	obj, exists, err := f.knownObjects.GetByKey(key)
	if err != nil {
		f.logger.Error(err, "Unexpected error during lookup, unable to queue object for sync", "key", key)
		return nil
	} else if !exists {
		f.logger.Info("Key does not exist in known objects store, unable to queue object for sync", "key", key)
		return nil
	}

	// If we are doing Resync() and there is already an event queued for that object,
	// we ignore the Resync for it. This is to avoid the race, in which the resync
	// comes with the previous value of object (since queueing an event for the object
	// doesn't trigger changing the underlying store <knownObjects>.
	id, err := f.KeyOf(obj)
	if err != nil {
		return KeyError{obj, err}
	}
	if len(f.items[id]) > 0 {
		return nil
	}

	if err := f.queueActionLocked(Sync, obj); err != nil {
		return fmt.Errorf("couldn't queue object: %v", err)
	}
	return nil
}

// A KeyListerGetter is anything that knows how to list its keys and look up by key.
type KeyListerGetter interface {
	KeyLister
	KeyGetter
}

// A KeyLister is anything that knows how to list its keys.
type KeyLister interface {
	ListKeys() []string
}

// A KeyGetter is anything that knows how to get the value stored under a given key.
type KeyGetter interface {
	// GetByKey returns the value associated with the key, or sets exists=false.
	GetByKey(key string) (value interface{}, exists bool, err error)
}

// Oldest is a convenience function that returns the oldest delta, or
// nil if there are no deltas.
func (d Deltas) Oldest() *Delta {
	if len(d) > 0 {
		return &d[0]
	}
	return nil
}

// Newest is a convenience function that returns the newest delta, or
// nil if there are no deltas.
func (d Deltas) Newest() *Delta {
	if n := len(d); n > 0 {
		return &d[n-1]
	}
	return nil
}

// copyDeltas returns a shallow copy of d; that is, it copies the slice but not
// the objects in the slice. This allows Get/List to return an object that we
// know won't be clobbered by a subsequent modifications.
func copyDeltas(d Deltas) Deltas {
	d2 := make(Deltas, len(d))
	copy(d2, d)
	return d2
}

// DeletedFinalStateUnknown is placed into a DeltaFIFO in the case where an object
// was deleted but the watch deletion event was missed while disconnected from
// apiserver. In this case we don't know the final "resting" state of the object, so
// there's a chance the included `Obj` is stale.
type DeletedFinalStateUnknown struct {
	Key string
	Obj interface{}
}
