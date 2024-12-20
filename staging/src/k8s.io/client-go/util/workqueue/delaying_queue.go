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

package workqueue

import (
	"container/heap"
	"sync"
	"time"

	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/klog/v2"
	"k8s.io/utils/clock"
)

// DelayingInterface is an Interface that can Add an item at a later time. This makes it easier to
// requeue items after failures without ending up in a hot-loop.
//
// Deprecated: use TypedDelayingInterface instead.
type DelayingInterface TypedDelayingInterface[any]

// TypedDelayingInterface is an Interface that can Add an item at a later time. This makes it easier to
// requeue items after failures without ending up in a hot-loop.
type TypedDelayingInterface[T comparable] interface {
	TypedInterface[T]
	// AddAfter adds an item to the workqueue after the indicated duration has passed
	AddAfter(item T, duration time.Duration)
}

// DelayingQueueConfig specifies optional configurations to customize a DelayingInterface.
//
// Deprecated: use TypedDelayingQueueConfig instead.
type DelayingQueueConfig = TypedDelayingQueueConfig[any]

// TypedDelayingQueueConfig specifies optional configurations to customize a DelayingInterface.
type TypedDelayingQueueConfig[T comparable] struct {
	// An optional logger. The name of the queue does *not* get added to it, this should
	// be done by the caller if desired.
	Logger *klog.Logger

	// Name for the queue. If unnamed, the metrics will not be registered.
	Name string

	// MetricsProvider optionally allows specifying a metrics provider to use for the queue
	// instead of the global provider.
	MetricsProvider MetricsProvider

	// Clock optionally allows injecting a real or fake clock for testing purposes.
	Clock clock.WithTicker

	// Queue optionally allows injecting custom queue Interface instead of the default one.
	Queue TypedInterface[T]
}

// NewDelayingQueue constructs a new workqueue with delayed queuing ability.
// NewDelayingQueue does not emit metrics. For use with a MetricsProvider, please use
// NewDelayingQueueWithConfig instead and specify a name.
//
// Deprecated: use NewTypedDelayingQueue instead.
func NewDelayingQueue() DelayingInterface {
	return NewDelayingQueueWithConfig(DelayingQueueConfig{})
}

// NewTypedDelayingQueue constructs a new workqueue with delayed queuing ability.
// NewTypedDelayingQueue does not emit metrics. For use with a MetricsProvider, please use
// NewTypedDelayingQueueWithConfig instead and specify a name.
func NewTypedDelayingQueue[T comparable]() TypedDelayingInterface[T] {
	return NewTypedDelayingQueueWithConfig(TypedDelayingQueueConfig[T]{})
}

// NewDelayingQueueWithConfig constructs a new workqueue with options to
// customize different properties.
//
// Deprecated: use NewTypedDelayingQueueWithConfig instead.
func NewDelayingQueueWithConfig(config DelayingQueueConfig) DelayingInterface {
	return NewTypedDelayingQueueWithConfig[any](config)
}

// TypedNewDelayingQueue exists for backwards compatibility only.
//
// Deprecated: use NewTypedDelayingQueueWithConfig instead.
func TypedNewDelayingQueue[T comparable]() TypedDelayingInterface[T] {
	return NewTypedDelayingQueue[T]()
}

// NewTypedDelayingQueueWithConfig constructs a new workqueue with options to
// customize different properties.
func NewTypedDelayingQueueWithConfig[T comparable](config TypedDelayingQueueConfig[T]) TypedDelayingInterface[T] {
	logger := klog.Background()
	if config.Logger != nil {
		logger = *config.Logger
	}
	if config.Clock == nil {
		config.Clock = clock.RealClock{}
	}

	if config.Queue == nil {
		config.Queue = NewTypedWithConfig[T](TypedQueueConfig[T]{
			Name:            config.Name,
			MetricsProvider: config.MetricsProvider,
			Clock:           config.Clock,
		})
	}

	return newDelayingQueue(logger, config.Clock, config.Queue, config.Name, config.MetricsProvider)
}

// NewDelayingQueueWithCustomQueue constructs a new workqueue with ability to
// inject custom queue Interface instead of the default one
// Deprecated: Use NewDelayingQueueWithConfig instead.
func NewDelayingQueueWithCustomQueue(q Interface, name string) DelayingInterface {
	return NewDelayingQueueWithConfig(DelayingQueueConfig{
		Name:  name,
		Queue: q,
	})
}

// NewNamedDelayingQueue constructs a new named workqueue with delayed queuing ability.
// Deprecated: Use NewDelayingQueueWithConfig instead.
func NewNamedDelayingQueue(name string) DelayingInterface {
	return NewDelayingQueueWithConfig(DelayingQueueConfig{Name: name})
}

// NewDelayingQueueWithCustomClock constructs a new named workqueue
// with ability to inject real or fake clock for testing purposes.
// Deprecated: Use NewDelayingQueueWithConfig instead.
func NewDelayingQueueWithCustomClock(clock clock.WithTicker, name string) DelayingInterface {
	return NewDelayingQueueWithConfig(DelayingQueueConfig{
		Name:  name,
		Clock: clock,
	})
}

func newDelayingQueue[T comparable](logger klog.Logger, clock clock.WithTicker, q TypedInterface[T], name string, provider MetricsProvider) *delayingType[T] {
	ret := &delayingType[T]{
		TypedInterface:  q,
		clock:           clock,
		heartbeat:       clock.NewTicker(maxWait),
		stopCh:          make(chan struct{}),
		waitingForAddCh: make(chan *waitFor[T], 1000),
		metrics:         newRetryMetrics(name, provider),
	}

	go ret.waitingLoop(logger)
	return ret
}

// delayingType wraps an Interface and provides delayed re-enquing
type delayingType[T comparable] struct {
	TypedInterface[T]

	// clock tracks time for delayed firing
	clock clock.Clock

	// stopCh lets us signal a shutdown to the waiting loop
	stopCh chan struct{}
	// stopOnce guarantees we only signal shutdown a single time
	stopOnce sync.Once

	// heartbeat ensures we wait no more than maxWait before firing
	heartbeat clock.Ticker

	// waitingForAddCh is a buffered channel that feeds waitingForAdd
	waitingForAddCh chan *waitFor[T]

	// metrics counts the number of retries
	metrics retryMetrics
}

// waitFor holds the data to add and the time it should be added
type waitFor[T any] struct {
	data    T
	readyAt time.Time
	// index in the priority queue (heap)
	index int
}

// waitForPriorityQueue implements a priority queue for waitFor items.
//
// waitForPriorityQueue implements heap.Interface. The item occurring next in
// time (i.e., the item with the smallest readyAt) is at the root (index 0).
// Peek returns this minimum item at index 0. Pop returns the minimum item after
// it has been removed from the queue and placed at index Len()-1 by
// container/heap. Push adds an item at index Len(), and container/heap
// percolates it into the correct location.
type waitForPriorityQueue[T any] []*waitFor[T]

func (pq waitForPriorityQueue[T]) Len() int {
	return len(pq)
}
func (pq waitForPriorityQueue[T]) Less(i, j int) bool {
	return pq[i].readyAt.Before(pq[j].readyAt)
}
func (pq waitForPriorityQueue[T]) Swap(i, j int) {
	pq[i], pq[j] = pq[j], pq[i]
	pq[i].index = i
	pq[j].index = j
}

// Push adds an item to the queue. Push should not be called directly; instead,
// use `heap.Push`.
func (pq *waitForPriorityQueue[T]) Push(x interface{}) {
	n := len(*pq)
	item := x.(*waitFor[T])
	item.index = n
	*pq = append(*pq, item)
}

// Pop removes an item from the queue. Pop should not be called directly;
// instead, use `heap.Pop`.
func (pq *waitForPriorityQueue[T]) Pop() interface{} {
	n := len(*pq)
	item := (*pq)[n-1]
	item.index = -1
	*pq = (*pq)[0:(n - 1)]
	return item
}

// Peek returns the item at the beginning of the queue, without removing the
// item or otherwise mutating the queue. It is safe to call directly.
func (pq waitForPriorityQueue[T]) Peek() interface{} {
	return pq[0]
}

// ShutDown stops the queue. After the queue drains, the returned shutdown bool
// on Get() will be true. This method may be invoked more than once.
func (q *delayingType[T]) ShutDown() {
	q.stopOnce.Do(func() {
		q.TypedInterface.ShutDown()
		close(q.stopCh)
		q.heartbeat.Stop()
	})
}

// AddAfter adds the given item to the work queue after the given delay
func (q *delayingType[T]) AddAfter(item T, duration time.Duration) {
	// don't add if we're already shutting down
	if q.ShuttingDown() {
		return
	}

	q.metrics.retry()

	// immediately add things with no delay
	if duration <= 0 {
		q.Add(item)
		return
	}

	select {
	case <-q.stopCh:
		// unblock if ShutDown() is called
	case q.waitingForAddCh <- &waitFor[T]{data: item, readyAt: q.clock.Now().Add(duration)}:
	}
}

// maxWait keeps a max bound on the wait time. It's just insurance against weird things happening.
// Checking the queue every 10 seconds isn't expensive and we know that we'll never end up with an
// expired item sitting for more than 10 seconds.
const maxWait = 10 * time.Second

// waitingLoop runs until the workqueue is shutdown and keeps a check on the list of items to be added.
func (q *delayingType[T]) waitingLoop(logger klog.Logger) {
	defer utilruntime.HandleCrashWithLogger(logger)

	// Make a placeholder channel to use when there are no items in our list
	never := make(<-chan time.Time)

	// Make a timer that expires when the item at the head of the waiting queue is ready
	var nextReadyAtTimer clock.Timer

	waitingForQueue := &waitForPriorityQueue[T]{}
	heap.Init(waitingForQueue)

	waitingEntryByData := map[T]*waitFor[T]{}

	for {
		if q.TypedInterface.ShuttingDown() {
			return
		}

		now := q.clock.Now()

		// Add ready entries
		for waitingForQueue.Len() > 0 {
			entry := waitingForQueue.Peek().(*waitFor[T])
			if entry.readyAt.After(now) {
				break
			}

			entry = heap.Pop(waitingForQueue).(*waitFor[T])
			q.Add(entry.data)
			delete(waitingEntryByData, entry.data)
		}

		// Set up a wait for the first item's readyAt (if one exists)
		nextReadyAt := never
		if waitingForQueue.Len() > 0 {
			if nextReadyAtTimer != nil {
				nextReadyAtTimer.Stop()
			}
			entry := waitingForQueue.Peek().(*waitFor[T])
			nextReadyAtTimer = q.clock.NewTimer(entry.readyAt.Sub(now))
			nextReadyAt = nextReadyAtTimer.C()
		}

		select {
		case <-q.stopCh:
			return

		case <-q.heartbeat.C():
			// continue the loop, which will add ready items

		case <-nextReadyAt:
			// continue the loop, which will add ready items

		case waitEntry := <-q.waitingForAddCh:
			if waitEntry.readyAt.After(q.clock.Now()) {
				insert(waitingForQueue, waitingEntryByData, waitEntry)
			} else {
				q.Add(waitEntry.data)
			}

			drained := false
			for !drained {
				select {
				case waitEntry := <-q.waitingForAddCh:
					if waitEntry.readyAt.After(q.clock.Now()) {
						insert(waitingForQueue, waitingEntryByData, waitEntry)
					} else {
						q.Add(waitEntry.data)
					}
				default:
					drained = true
				}
			}
		}
	}
}

// insert adds the entry to the priority queue, or updates the readyAt if it already exists in the queue
func insert[T comparable](q *waitForPriorityQueue[T], knownEntries map[T]*waitFor[T], entry *waitFor[T]) {
	// if the entry already exists, update the time only if it would cause the item to be queued sooner
	existing, exists := knownEntries[entry.data]
	if exists {
		if existing.readyAt.After(entry.readyAt) {
			existing.readyAt = entry.readyAt
			heap.Fix(q, existing.index)
		}

		return
	}

	heap.Push(q, entry)
	knownEntries[entry.data] = entry
}
