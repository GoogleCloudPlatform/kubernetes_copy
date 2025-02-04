/*
Copyright 2021 The Kubernetes Authors.

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
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/tools/cache"
)

func intervalFromEvents(events []*watchCacheEvent) *watchCacheInterval {
	startIndex, endIndex, locker := 0, len(events), &sync.Mutex{}
	indexer := func(i int) *watchCacheEvent {
		if len(events) == 0 {
			return nil
		}
		return events[i]
	}
	indexValidator := func(_ int) bool { return true }

	return newCacheInterval(startIndex, endIndex, indexer, indexValidator, locker)
}

func bufferFromEvents(events []*watchCacheEvent) *watchCacheIntervalBuffer {
	wcib := &watchCacheIntervalBuffer{
		buffer:     make([]*watchCacheEvent, bufferSize),
		startIndex: 0,
		endIndex:   len(events),
	}
	copy(wcib.buffer, events)

	return wcib
}

func generateEvents(start, end int) []*watchCacheEvent {
	n := end - start
	events := make([]*watchCacheEvent, n)
	for i := 0; i < n; i++ {
		events[i] = &watchCacheEvent{
			Type:   watch.Added,
			Object: makeTestPod(fmt.Sprintf("pod%d", start+i), uint64(start+i)),
		}
	}
	return events
}

func verifyEvent(ok bool, event, expectedEvent *watchCacheEvent) error {
	if !ok {
		return fmt.Errorf("expected event: %#v, got no event", expectedEvent)
	}

	if event == nil {
		return fmt.Errorf("unexpected nil event, expected: %#v", expectedEvent)
	}

	if !reflect.DeepEqual(event, expectedEvent) {
		return fmt.Errorf("expected %v, got %v", *event, *expectedEvent)
	}

	return nil
}

func verifyNoEvent(ok bool, event *watchCacheEvent) error {
	if ok {
		return errors.New("unexpected bool value indicating buffer is not empty")
	}
	if event != nil {
		return fmt.Errorf("unexpected event received, expected: nil, got %v", *event)
	}

	return nil
}

func TestIntervalBufferIsFull(t *testing.T) {
	cases := []struct {
		endIndex int
		expected bool
	}{
		{endIndex: bufferSize - 1, expected: false},
		{endIndex: bufferSize, expected: true},
		{endIndex: bufferSize + 1, expected: true},
	}

	for _, c := range cases {
		wcib := &watchCacheIntervalBuffer{endIndex: c.endIndex}
		actual := wcib.isFull()
		if actual != c.expected {
			t.Errorf("expected %v, got %v", c.expected, actual)
		}
	}
}

func TestIntervalBufferIsEmpty(t *testing.T) {
	cases := []struct {
		startIndex int
		endIndex   int
		expected   bool
	}{
		{startIndex: 0, endIndex: 10, expected: false},
		{startIndex: 5, endIndex: 20, expected: false},
		{startIndex: 50, endIndex: 50, expected: true},
	}

	for _, c := range cases {
		wcib := &watchCacheIntervalBuffer{
			startIndex: c.startIndex,
			endIndex:   c.endIndex,
		}
		actual := wcib.isEmpty()
		if actual != c.expected {
			t.Errorf("expected %v, got %v", c.expected, actual)
		}
	}
}

func TestIntervalBufferNext(t *testing.T) {
	cases := []struct {
		name   string
		events []*watchCacheEvent
	}{
		{
			name: "buffer has elements",
			events: []*watchCacheEvent{
				{Type: watch.Added, Object: makeTestPod("pod1", 1)},
				{Type: watch.Added, Object: makeTestPod("pod2", 2)},
				{Type: watch.Modified, Object: makeTestPod("pod3", 3)},
			},
		},
		{
			name:   "buffer is empty",
			events: []*watchCacheEvent{},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wcib := bufferFromEvents(c.events)
			for i := 0; i < len(c.events); i++ {
				event, ok := wcib.next()
				if err := verifyEvent(ok, event, c.events[i]); err != nil {
					t.Error(err)
				}
			}
			event, ok := wcib.next()
			if err := verifyNoEvent(ok, event); err != nil {
				t.Error(err)
			}
		})
	}
}

func TestFillBuffer(t *testing.T) {
	cases := []struct {
		name            string
		numEventsToFill int
	}{
		{
			name:            "no events to put in buffer",
			numEventsToFill: 0,
		},
		{
			name:            "less than bufferSize events to put in buffer",
			numEventsToFill: 5,
		},
		{
			name:            "equal to bufferSize events to put in buffer",
			numEventsToFill: bufferSize,
		},
		{
			name:            "greater than bufferSize events to put in buffer",
			numEventsToFill: bufferSize + 5,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			events := generateEvents(0, c.numEventsToFill)
			wci := intervalFromEvents(events)

			for i := 0; i < len(events); i++ {
				if i%bufferSize == 0 {
					wci.fillBuffer()
				}
				event, ok := wci.buffer.next()
				if err := verifyEvent(ok, event, events[i]); err != nil {
					t.Error(err)
				}
				// If we have already received bufferSize number of events,
				// buffer should be empty and we should receive no event.
				if i%bufferSize == bufferSize-1 {
					event, ok := wci.buffer.next()
					if err := verifyNoEvent(ok, event); err != nil {
						t.Error(err)
					}
				}
			}
			// buffer should be empty and return no event.
			event, ok := wci.buffer.next()
			if err := verifyNoEvent(ok, event); err != nil {
				t.Error(err)
			}
			// Buffer should be empty now, an additional fillBuffer()
			// should make no difference.
			wci.fillBuffer()
			event, ok = wci.buffer.next()
			if err := verifyNoEvent(ok, event); err != nil {
				t.Error(err)
			}
		})
	}
}

func TestCacheIntervalNextFromWatchCache(t *testing.T) {
	// Have the capacity such that it facilitates
	// filling the interval buffer more than once
	// completely and then some more - 10 here is
	// arbitrary.
	const capacity = 2*bufferSize + 10

	cases := []struct {
		name string
		// The total number of events that the watch
		// cache will be populated with to start with.
		eventsAddedToWatchcache int
		intervalStartIndex      int
	}{
		{
			name:                    "watchCache empty, eventsAddedToWatchcache = 0",
			eventsAddedToWatchcache: 0,
			intervalStartIndex:      0,
		},
		{
			name:                    "watchCache partially propagated, eventsAddedToWatchcache < capacity",
			eventsAddedToWatchcache: bufferSize,
			intervalStartIndex:      0,
		},
		{
			name:                    "watchCache partially propagated, eventsAddedToWatchcache < capacity, intervalStartIndex at some offset",
			eventsAddedToWatchcache: bufferSize,
			intervalStartIndex:      5,
		},
		{
			name:                    "watchCache fully propagated, eventsAddedToWatchcache = capacity",
			eventsAddedToWatchcache: capacity,
			intervalStartIndex:      0,
		},
		{
			name:                    "watchCache fully propagated, eventsAddedToWatchcache = capacity, intervalStartIndex at some offset",
			eventsAddedToWatchcache: capacity,
			intervalStartIndex:      5,
		},
		{
			name:                    "watchCache over propagated, eventsAddedToWatchcache > capacity",
			eventsAddedToWatchcache: capacity + bufferSize,
			intervalStartIndex:      0,
		},
		{
			name:                    "watchCache over propagated, eventsAddedToWatchcache > capacity, intervalStartIndex at some offset",
			eventsAddedToWatchcache: capacity + bufferSize,
			intervalStartIndex:      5,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wc := newTestWatchCache(capacity, DefaultEventFreshDuration, &cache.Indexers{})
			defer wc.Stop()
			for i := 0; i < c.eventsAddedToWatchcache; i++ {
				wc.Add(makeTestPod(fmt.Sprintf("pod%d", i), uint64(i)))
			}
			indexerFunc := func(i int) *watchCacheEvent {
				return wc.cache[i%wc.capacity]
			}

			wci := newCacheInterval(
				c.intervalStartIndex,
				wc.endIndex,
				indexerFunc,
				wc.isIndexValidLocked,
				&wc.RWMutex,
			)

			numExpectedEvents := wc.endIndex - c.intervalStartIndex
			for i := 0; i < numExpectedEvents; i++ {
				// Simulate and test interval invalidation iff
				// the watchCache itself is not empty.
				if c.eventsAddedToWatchcache > 0 {
					// The points at which we want to artificially
					// invalidate the interval and test its behaviour
					// should be multiples of bufferSize. This is because
					// invalidation only needs to be checked when we are
					// copying over events from the underlying watch cache,
					// i.e. freshly filling in the interval buffer.
					if i%bufferSize == 0 && i != c.eventsAddedToWatchcache {
						originalCacheStartIndex := wc.startIndex
						wc.startIndex = wci.startIndex + 1
						event, err := wci.Next()
						if err == nil {
							t.Errorf("expected non-nil error")
						}
						if event != nil {
							t.Errorf("expected nil event, got %v", *event)
						}
						// Restore startIndex.
						wc.startIndex = originalCacheStartIndex
					}
				}

				// Check if the state of the interval buffer is as expected.
				// The interval buffer can be empty either when received is
				// either a multiple of bufferSize (after one complete fill)
				// or when received is equal to the number of expected events.
				// The latter happens when partial filling occurs and no more
				// events are left post the partial fill.
				if wci.buffer.isEmpty() != (i%bufferSize == 0 || i == numExpectedEvents) {
					t.Error("expected empty interval buffer")
					return
				}

				event, err := wci.Next()
				if err != nil {
					t.Errorf("unexpected error: %v", err)
					return
				}

				expectedIndex := (c.intervalStartIndex + i) % wc.capacity
				expectedEvent := wc.cache[expectedIndex]
				if err := verifyEvent(true, event, expectedEvent); err != nil {
					t.Error(err)
				}
			}
			event, err := wci.Next()
			ok := err != nil
			if err := verifyNoEvent(ok, event); err != nil {
				t.Error(err)
			}
		})
	}
}

func TestCacheIntervalNextFromStore(t *testing.T) {
	getAttrsFunc := func(obj runtime.Object) (labels.Set, fields.Set, error) {
		pod, ok := obj.(*v1.Pod)
		if !ok {
			return nil, nil, fmt.Errorf("not a pod")
		}
		return labels.Set(pod.Labels), fields.Set{"spec.nodeName": pod.Spec.NodeName}, nil
	}
	const numEvents = 50
	store := cache.NewIndexer(storeElementKey, storeElementIndexers(nil))
	events := make(map[string]*watchCacheEvent)
	var rv uint64 = 1 // arbitrary number; rv till which the watch cache has progressed.

	for i := 0; i < numEvents; i++ {
		elem := makeTestStoreElement(makeTestPod(fmt.Sprintf("pod%d", i), uint64(i)))
		objLabels, objFields, err := getAttrsFunc(elem.Object)
		if err != nil {
			t.Fatal(err)
		}
		events[elem.Key] = &watchCacheEvent{
			Type:            watch.Added,
			Object:          elem.Object,
			ObjLabels:       objLabels,
			ObjFields:       objFields,
			Key:             elem.Key,
			ResourceVersion: rv,
		}
		store.Add(elem)
	}

	wci, err := newCacheIntervalFromStore(rv, store, getAttrsFunc, "", false)
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < numEvents; i++ {
		// The interval buffer can never be empty unless
		// all elements obtained through List() have been
		// returned.
		if wci.buffer.isEmpty() && i != numEvents {
			t.Fatal("expected non-empty interval buffer")
		}
		event, err := wci.Next()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if event == nil {
			t.Error("unexpected nil event")
			break
		}
		expectedEvent, ok := events[event.Key]
		if !ok {
			t.Fatalf("event with key %s not found", event.Key)
		}
		if !reflect.DeepEqual(event, expectedEvent) {
			t.Errorf("expected: %v, got %v", *events[event.Key], *event)
		}
	}

	// The interval's buffer should now be empty.
	if !wci.buffer.isEmpty() {
		t.Error("expected cache interval's buffer to be empty")
	}
}
