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
	"math/rand"
	"net/http"
	"reflect"
	goruntime "runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/klog/v2/ktesting"
	"k8s.io/utils/clock"
	testingclock "k8s.io/utils/clock/testing"
)

var nevererrc chan error

func TestCloseWatchChannelOnError(t *testing.T) {
	_, ctx := ktesting.NewTestContext(t)
	r := NewReflector(&ListWatch{}, &v1.Pod{}, NewStore(MetaNamespaceKeyFunc), 0)
	pod := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "bar"}}
	fw := watch.NewFake()
	r.listerWatcher = &ListWatch{
		WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
			return fw, nil
		},
		ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
			return &v1.PodList{ListMeta: metav1.ListMeta{ResourceVersion: "1"}}, nil
		},
	}
	go func() { assert.NoError(t, r.ListAndWatchWithContext(ctx)) }()
	fw.Error(pod)
	select {
	case _, ok := <-fw.ResultChan():
		if ok {
			t.Errorf("Watch channel left open after cancellation")
		}
	case <-time.After(wait.ForeverTestTimeout):
		t.Errorf("the cancellation is at least %s late", wait.ForeverTestTimeout.String())
		break
	}
}

func TestRunUntil(t *testing.T) {
	_, ctx := ktesting.NewTestContext(t)
	ctx, cancel := context.WithCancelCause(ctx)
	store := NewStore(MetaNamespaceKeyFunc)
	r := NewReflector(&ListWatch{}, &v1.Pod{}, store, 0)
	fw := watch.NewFake()
	r.listerWatcher = &ListWatch{
		WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
			return fw, nil
		},
		ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
			return &v1.PodList{ListMeta: metav1.ListMeta{ResourceVersion: "1"}}, nil
		},
	}
	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)
		r.RunWithContext(ctx)
	}()
	// Synchronously add a dummy pod into the watch channel so we
	// know the RunUntil go routine is in the watch handler.
	fw.Add(&v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "bar"}})

	cancel(errors.New("done"))
	resultCh := fw.ResultChan()
	for {
		select {
		case <-doneCh:
			if resultCh == nil {
				return // both closed
			}
			doneCh = nil
		case _, ok := <-resultCh:
			if ok {
				t.Fatalf("Watch channel left open after stopping the watch")
			}
			if doneCh == nil {
				return // both closed
			}
			resultCh = nil
		case <-time.After(wait.ForeverTestTimeout):
			t.Fatalf("the cancellation is at least %s late", wait.ForeverTestTimeout.String())
		}
	}
}

func TestReflectorResyncChan(t *testing.T) {
	s := NewStore(MetaNamespaceKeyFunc)
	g := NewReflector(&ListWatch{}, &v1.Pod{}, s, time.Millisecond)
	a, _ := g.resyncChan()
	b := time.After(wait.ForeverTestTimeout)
	select {
	case <-a:
		t.Logf("got timeout as expected")
	case <-b:
		t.Errorf("resyncChan() is at least 99 milliseconds late??")
	}
}

// TestReflectorWatchStoppedBefore ensures that neither List nor Watch are
// called if the stop channel is closed before Reflector.watch is called.
func TestReflectorWatchStoppedBefore(t *testing.T) {
	_, ctx := ktesting.NewTestContext(t)
	ctx, cancel := context.WithCancelCause(ctx)
	cancel(errors.New("don't run"))

	lw := &ListWatch{
		ListFunc: func(_ metav1.ListOptions) (runtime.Object, error) {
			t.Fatal("ListFunc called unexpectedly")
			return nil, nil
		},
		WatchFunc: func(_ metav1.ListOptions) (watch.Interface, error) {
			// If WatchFunc is never called, the watcher it returns doesn't need to be stopped.
			t.Fatal("WatchFunc called unexpectedly")
			return nil, nil
		},
	}
	target := NewReflector(lw, &v1.Pod{}, nil, 0)

	err := target.watch(ctx, nil, nil)
	require.NoError(t, err)
}

// TestReflectorWatchStoppedAfter ensures that neither the watcher is stopped if
// the stop channel is closed after Reflector.watch has started watching.
func TestReflectorWatchStoppedAfter(t *testing.T) {
	_, ctx := ktesting.NewTestContext(t)
	ctx, cancel := context.WithCancelCause(ctx)

	var watchers []*watch.FakeWatcher

	lw := &ListWatch{
		ListFunc: func(_ metav1.ListOptions) (runtime.Object, error) {
			t.Fatal("ListFunc called unexpectedly")
			return nil, nil
		},
		WatchFunc: func(_ metav1.ListOptions) (watch.Interface, error) {
			// Simulate the stop channel being closed after watching has started
			go func() {
				time.Sleep(10 * time.Millisecond)
				cancel(errors.New("10ms timeout reached"))
			}()
			// Use a fake watcher that never sends events
			w := watch.NewFake()
			watchers = append(watchers, w)
			return w, nil
		},
	}
	target := NewReflector(lw, &v1.Pod{}, nil, 0)

	err := target.watch(ctx, nil, nil)
	require.NoError(t, err)
	require.Len(t, watchers, 1)
	require.True(t, watchers[0].IsStopped())
}

func BenchmarkReflectorResyncChanMany(b *testing.B) {
	s := NewStore(MetaNamespaceKeyFunc)
	g := NewReflector(&ListWatch{}, &v1.Pod{}, s, 25*time.Millisecond)
	// The improvement to this (calling the timer's Stop() method) makes
	// this benchmark about 40% faster.
	for i := 0; i < b.N; i++ {
		g.resyncPeriod = time.Duration(rand.Float64() * float64(time.Millisecond) * 25)
		_, stop := g.resyncChan()
		stop()
	}
}

// TestReflectorHandleWatchStoppedBefore ensures that handleWatch stops when
// stopCh is already closed before handleWatch was called. It also ensures that
// ResultChan is only called once and that Stop is called after ResultChan.
func TestReflectorHandleWatchStoppedBefore(t *testing.T) {
	s := NewStore(MetaNamespaceKeyFunc)
	g := NewReflector(&ListWatch{}, &v1.Pod{}, s, 0)
	_, ctx := ktesting.NewTestContext(t)
	ctx, cancel := context.WithCancelCause(ctx)
	// Simulate the context being canceled before the watchHandler is called
	cancel(errors.New("don't run"))
	var calls []string
	resultCh := make(chan watch.Event)
	fw := watch.MockWatcher{
		StopFunc: func() {
			calls = append(calls, "Stop")
			close(resultCh)
		},
		ResultChanFunc: func() <-chan watch.Event {
			calls = append(calls, "ResultChan")
			return resultCh
		},
	}
	err := handleWatch(ctx, time.Now(), fw, s, g.expectedType, g.expectedGVK, g.name, g.typeDescription, g.setLastSyncResourceVersion, g.clock, nevererrc)
	if err == nil {
		t.Errorf("unexpected non-error")
	}
	// Ensure the watcher methods are called exactly once in this exact order.
	// TODO(karlkfi): Fix watchHandler to call Stop()
	// assert.Equal(t, []string{"ResultChan", "Stop"}, calls)
	assert.Equal(t, []string{"ResultChan"}, calls)
}

// TestReflectorHandleWatchStoppedAfter ensures that handleWatch stops when
// stopCh is closed after handleWatch was called. It also ensures that
// ResultChan is only called once and that Stop is called after ResultChan.
func TestReflectorHandleWatchStoppedAfter(t *testing.T) {
	s := NewStore(MetaNamespaceKeyFunc)
	g := NewReflector(&ListWatch{}, &v1.Pod{}, s, 0)
	var calls []string
	_, ctx := ktesting.NewTestContext(t)
	ctx, cancel := context.WithCancelCause(ctx)
	resultCh := make(chan watch.Event)
	fw := watch.MockWatcher{
		StopFunc: func() {
			calls = append(calls, "Stop")
			close(resultCh)
		},
		ResultChanFunc: func() <-chan watch.Event {
			calls = append(calls, "ResultChan")
			resultCh = make(chan watch.Event)
			// Simulate the watch handler being stopped asynchronously by the
			// caller, after watching has started.
			go func() {
				time.Sleep(10 * time.Millisecond)
				cancel(errors.New("10ms timeout reached"))
			}()
			return resultCh
		},
	}
	err := handleWatch(ctx, time.Now(), fw, s, g.expectedType, g.expectedGVK, g.name, g.typeDescription, g.setLastSyncResourceVersion, g.clock, nevererrc)
	if err == nil {
		t.Errorf("unexpected non-error")
	}
	// Ensure the watcher methods are called exactly once in this exact order.
	// TODO(karlkfi): Fix watchHandler to call Stop()
	// assert.Equal(t, []string{"ResultChan", "Stop"}, calls)
	assert.Equal(t, []string{"ResultChan"}, calls)
}

// TestReflectorHandleWatchResultChanClosedBefore ensures that handleWatch
// stops when the result channel is closed before handleWatch was called.
func TestReflectorHandleWatchResultChanClosedBefore(t *testing.T) {
	s := NewStore(MetaNamespaceKeyFunc)
	g := NewReflector(&ListWatch{}, &v1.Pod{}, s, 0)
	_, ctx := ktesting.NewTestContext(t)
	var calls []string
	resultCh := make(chan watch.Event)
	fw := watch.MockWatcher{
		StopFunc: func() {
			calls = append(calls, "Stop")
		},
		ResultChanFunc: func() <-chan watch.Event {
			calls = append(calls, "ResultChan")
			return resultCh
		},
	}
	// Simulate the result channel being closed by the producer before handleWatch is called.
	close(resultCh)
	err := handleWatch(ctx, time.Now(), fw, s, g.expectedType, g.expectedGVK, g.name, g.typeDescription, g.setLastSyncResourceVersion, g.clock, nevererrc)
	if err == nil {
		t.Errorf("unexpected non-error")
	}
	// Ensure the watcher methods are called exactly once in this exact order.
	// TODO(karlkfi): Fix watchHandler to call Stop()
	// assert.Equal(t, []string{"ResultChan", "Stop"}, calls)
	assert.Equal(t, []string{"ResultChan"}, calls)
}

// TestReflectorHandleWatchResultChanClosedAfter ensures that handleWatch
// stops when the result channel is closed after handleWatch has started watching.
func TestReflectorHandleWatchResultChanClosedAfter(t *testing.T) {
	s := NewStore(MetaNamespaceKeyFunc)
	g := NewReflector(&ListWatch{}, &v1.Pod{}, s, 0)
	_, ctx := ktesting.NewTestContext(t)
	var calls []string
	resultCh := make(chan watch.Event)
	fw := watch.MockWatcher{
		StopFunc: func() {
			calls = append(calls, "Stop")
		},
		ResultChanFunc: func() <-chan watch.Event {
			calls = append(calls, "ResultChan")
			resultCh = make(chan watch.Event)
			// Simulate the result channel being closed by the producer, after
			// watching has started.
			go func() {
				time.Sleep(10 * time.Millisecond)
				close(resultCh)
			}()
			return resultCh
		},
	}
	err := handleWatch(ctx, time.Now(), fw, s, g.expectedType, g.expectedGVK, g.name, g.typeDescription, g.setLastSyncResourceVersion, g.clock, nevererrc)
	if err == nil {
		t.Errorf("unexpected non-error")
	}
	// Ensure the watcher methods are called exactly once in this exact order.
	// TODO(karlkfi): Fix watchHandler to call Stop()
	// assert.Equal(t, []string{"ResultChan", "Stop"}, calls)
	assert.Equal(t, []string{"ResultChan"}, calls)
}

func TestReflectorWatchHandler(t *testing.T) {
	s := NewStore(MetaNamespaceKeyFunc)
	g := NewReflector(&ListWatch{}, &v1.Pod{}, s, 0)
	// Wrap setLastSyncResourceVersion so we can tell the watchHandler to stop
	// watching after all the events have been consumed. This avoids race
	// conditions which can happen if the producer calls Stop(), instead of the
	// consumer.
	_, ctx := ktesting.NewTestContext(t)
	ctx, cancel := context.WithCancelCause(ctx)
	setLastSyncResourceVersion := func(rv string) {
		g.setLastSyncResourceVersion(rv)
		if rv == "32" {
			cancel(errors.New("LastSyncResourceVersion is 32"))
		}
	}
	fw := watch.NewFake()
	s.Add(&v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "foo"}})
	s.Add(&v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "bar"}})
	go func() {
		fw.Add(&v1.Service{ObjectMeta: metav1.ObjectMeta{Name: "rejected"}})
		fw.Delete(&v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "foo"}})
		fw.Modify(&v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "bar", ResourceVersion: "55"}})
		fw.Add(&v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "baz", ResourceVersion: "32"}})
		fw.Stop()
	}()
	err := handleWatch(ctx, time.Now(), fw, s, g.expectedType, g.expectedGVK, g.name, g.typeDescription, setLastSyncResourceVersion, g.clock, nevererrc)
	// TODO(karlkfi): Fix FakeWatcher to avoid race condition between watcher.Stop() & close(stopCh)
	if err != nil && !errors.Is(err, errorStopRequested) {
		t.Errorf("unexpected error %v", err)
	}

	mkPod := func(id string, rv string) *v1.Pod {
		return &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: id, ResourceVersion: rv}}
	}

	// Validate that the Store was updated by the events
	table := []struct {
		Pod    *v1.Pod
		exists bool
	}{
		{mkPod("foo", ""), false},
		{mkPod("rejected", ""), false},
		{mkPod("bar", "55"), true},
		{mkPod("baz", "32"), true},
	}
	for _, item := range table {
		obj, exists, _ := s.Get(item.Pod)
		if e, a := item.exists, exists; e != a {
			t.Errorf("%v: expected %v, got %v", item.Pod, e, a)
		}
		if !exists {
			continue
		}
		if e, a := item.Pod.ResourceVersion, obj.(*v1.Pod).ResourceVersion; e != a {
			t.Errorf("%v: expected %v, got %v", item.Pod, e, a)
		}
	}

	// Validate that setLastSyncResourceVersion was called with the RV from the last event.
	if e, a := "32", g.LastSyncResourceVersion(); e != a {
		t.Errorf("expected %v, got %v", e, a)
	}
}

func TestReflectorStopWatch(t *testing.T) {
	s := NewStore(MetaNamespaceKeyFunc)
	g := NewReflector(&ListWatch{}, &v1.Pod{}, s, 0)
	fw := watch.NewFake()
	_, ctx := ktesting.NewTestContext(t)
	ctx, cancel := context.WithCancelCause(ctx)
	cancel(errors.New("don't run"))
	err := handleWatch(ctx, time.Now(), fw, s, g.expectedType, g.expectedGVK, g.name, g.typeDescription, g.setLastSyncResourceVersion, g.clock, nevererrc)
	if err != errorStopRequested {
		t.Errorf("expected stop error, got %q", err)
	}
}

func TestReflectorListAndWatch(t *testing.T) {
	_, ctx := ktesting.NewTestContext(t)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	createdFakes := make(chan *watch.FakeWatcher)

	// The ListFunc says that it's at revision 1. Therefore, we expect our WatchFunc
	// to get called at the beginning of the watch with 1, and again with 3 when we
	// inject an error.
	expectedRVs := []string{"1", "3"}
	lw := &ListWatch{
		WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
			rv := options.ResourceVersion
			fw := watch.NewFake()
			if e, a := expectedRVs[0], rv; e != a {
				t.Errorf("Expected rv %v, but got %v", e, a)
			}
			expectedRVs = expectedRVs[1:]
			// channel is not buffered because the for loop below needs to block. But
			// we don't want to block here, so report the new fake via a go routine.
			go func() { createdFakes <- fw }()
			return fw, nil
		},
		ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
			return &v1.PodList{ListMeta: metav1.ListMeta{ResourceVersion: "1"}}, nil
		},
	}
	s := NewFIFO(MetaNamespaceKeyFunc)
	r := NewReflector(lw, &v1.Pod{}, s, 0)
	go func() { assert.NoError(t, r.ListAndWatchWithContext(ctx)) }()

	ids := []string{"foo", "bar", "baz", "qux", "zoo"}
	var fw *watch.FakeWatcher
	for i, id := range ids {
		if fw == nil {
			fw = <-createdFakes
		}
		sendingRV := strconv.FormatUint(uint64(i+2), 10)
		fw.Add(&v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: id, ResourceVersion: sendingRV}})
		if sendingRV == "3" {
			// Inject a failure.
			fw.Stop()
			fw = nil
		}
	}

	// Verify we received the right ids with the right resource versions.
	for i, id := range ids {
		pod := Pop(s).(*v1.Pod)
		if e, a := id, pod.Name; e != a {
			t.Errorf("%v: Expected %v, got %v", i, e, a)
		}
		if e, a := strconv.FormatUint(uint64(i+2), 10), pod.ResourceVersion; e != a {
			t.Errorf("%v: Expected %v, got %v", i, e, a)
		}
	}

	if len(expectedRVs) != 0 {
		t.Error("called watchStarter an unexpected number of times")
	}
}

func TestReflectorListAndWatchWithErrors(t *testing.T) {
	mkPod := func(id string, rv string) *v1.Pod {
		return &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: id, ResourceVersion: rv}}
	}
	mkList := func(rv string, pods ...*v1.Pod) *v1.PodList {
		list := &v1.PodList{ListMeta: metav1.ListMeta{ResourceVersion: rv}}
		for _, pod := range pods {
			list.Items = append(list.Items, *pod)
		}
		return list
	}
	table := []struct {
		list     *v1.PodList
		listErr  error
		events   []watch.Event
		watchErr error
	}{
		{
			list: mkList("1"),
			events: []watch.Event{
				{Type: watch.Added, Object: mkPod("foo", "2")},
				{Type: watch.Added, Object: mkPod("bar", "3")},
			},
		}, {
			list: mkList("3", mkPod("foo", "2"), mkPod("bar", "3")),
			events: []watch.Event{
				{Type: watch.Deleted, Object: mkPod("foo", "4")},
				{Type: watch.Added, Object: mkPod("qux", "5")},
			},
		}, {
			listErr: fmt.Errorf("a list error"),
		}, {
			list:     mkList("5", mkPod("bar", "3"), mkPod("qux", "5")),
			watchErr: fmt.Errorf("a watch error"),
		}, {
			list: mkList("5", mkPod("bar", "3"), mkPod("qux", "5")),
			events: []watch.Event{
				{Type: watch.Added, Object: mkPod("baz", "6")},
			},
		}, {
			list: mkList("6", mkPod("bar", "3"), mkPod("qux", "5"), mkPod("baz", "6")),
		},
	}

	s := NewFIFO(MetaNamespaceKeyFunc)
	for line, item := range table {
		if item.list != nil {
			// Test that the list is what currently exists in the store.
			current := s.List()
			checkMap := map[string]string{}
			for _, item := range current {
				pod := item.(*v1.Pod)
				checkMap[pod.Name] = pod.ResourceVersion
			}
			for _, pod := range item.list.Items {
				if e, a := pod.ResourceVersion, checkMap[pod.Name]; e != a {
					t.Errorf("%v: expected %v, got %v for pod %v", line, e, a, pod.Name)
				}
			}
			if e, a := len(item.list.Items), len(checkMap); e != a {
				t.Errorf("%v: expected %v, got %v", line, e, a)
			}
		}
		watchRet, watchErr := item.events, item.watchErr
		_, ctx := ktesting.NewTestContext(t)
		ctx, cancel := context.WithCancelCause(ctx)
		lw := &ListWatch{
			WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
				if watchErr != nil {
					return nil, watchErr
				}
				watchErr = fmt.Errorf("second watch")
				fw := watch.NewFake()
				go func() {
					for _, e := range watchRet {
						fw.Action(e.Type, e.Object)
					}
					// Because FakeWatcher doesn't buffer events, it's safe to
					// close the stop channel immediately without missing events.
					// But usually, the event producer would instead close the
					// result channel, and wait for the consumer to stop the
					// watcher, to avoid race conditions.
					// TODO: Fix the FakeWatcher to separate watcher.Stop from close(resultCh)
					cancel(errors.New("done"))
				}()
				return fw, nil
			},
			ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
				return item.list, item.listErr
			},
		}
		r := NewReflector(lw, &v1.Pod{}, s, 0)
		err := r.ListAndWatchWithContext(ctx)
		if item.listErr != nil && !errors.Is(err, item.listErr) {
			t.Errorf("unexpected ListAndWatch error: %v", err)
		}
		if item.watchErr != nil && !errors.Is(err, item.watchErr) {
			t.Errorf("unexpected ListAndWatch error: %v", err)
		}
		if item.listErr == nil && item.watchErr == nil {
			assert.NoError(t, err)
		}
	}
}

func TestReflectorListAndWatchInitConnBackoff(t *testing.T) {
	maxBackoff := 50 * time.Millisecond
	table := []struct {
		numConnFails  int
		expLowerBound time.Duration
		expUpperBound time.Duration
	}{
		{5, 32 * time.Millisecond, 64 * time.Millisecond}, // case where maxBackoff is not hit, time should grow exponentially
		{40, 35 * 2 * maxBackoff, 40 * 2 * maxBackoff},    // case where maxBoff is hit, backoff time should flatten

	}
	for _, test := range table {
		t.Run(fmt.Sprintf("%d connection failures takes at least %d ms", test.numConnFails, 1<<test.numConnFails),
			func(t *testing.T) {
				_, ctx := ktesting.NewTestContext(t)
				ctx, cancel := context.WithCancelCause(ctx)
				connFails := test.numConnFails
				fakeClock := testingclock.NewFakeClock(time.Unix(0, 0))
				bm := wait.NewExponentialBackoffManager(time.Millisecond, maxBackoff, 100*time.Millisecond, 2.0, 1.0, fakeClock)
				done := make(chan struct{})
				defer close(done)
				go func() {
					i := 0
					for {
						select {
						case <-done:
							return
						default:
						}
						if fakeClock.HasWaiters() {
							step := (1 << (i + 1)) * time.Millisecond
							if step > maxBackoff*2 {
								step = maxBackoff * 2
							}
							fakeClock.Step(step)
							i++
						}
						time.Sleep(100 * time.Microsecond)
					}
				}()
				lw := &ListWatch{
					WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
						if connFails > 0 {
							connFails--
							return nil, syscall.ECONNREFUSED
						}
						cancel(errors.New("done"))
						return watch.NewFake(), nil
					},
					ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
						return &v1.PodList{ListMeta: metav1.ListMeta{ResourceVersion: "1"}}, nil
					},
				}
				r := &Reflector{
					name:              "test-reflector",
					listerWatcher:     lw,
					store:             NewFIFO(MetaNamespaceKeyFunc),
					backoffManager:    bm,
					clock:             fakeClock,
					watchErrorHandler: WatchErrorHandlerWithContext(DefaultWatchErrorHandler),
				}
				start := fakeClock.Now()
				err := r.ListAndWatchWithContext(ctx)
				elapsed := fakeClock.Since(start)
				if err != nil {
					t.Errorf("unexpected error %v", err)
				}
				if elapsed < (test.expLowerBound) {
					t.Errorf("expected lower bound of ListAndWatch: %v, got %v", test.expLowerBound, elapsed)
				}
				if elapsed > (test.expUpperBound) {
					t.Errorf("expected upper bound of ListAndWatch: %v, got %v", test.expUpperBound, elapsed)
				}
			})
	}
}

type fakeBackoff struct {
	clock clock.Clock
	calls int
}

func (f *fakeBackoff) Backoff() clock.Timer {
	f.calls++
	return f.clock.NewTimer(time.Duration(0))
}

func TestBackoffOnTooManyRequests(t *testing.T) {
	_, ctx := ktesting.NewTestContext(t)
	err := apierrors.NewTooManyRequests("too many requests", 1)
	clock := &clock.RealClock{}
	bm := &fakeBackoff{clock: clock}

	lw := &ListWatch{
		ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
			return &v1.PodList{ListMeta: metav1.ListMeta{ResourceVersion: "1"}}, nil
		},
		WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
			switch bm.calls {
			case 0:
				return nil, err
			case 1:
				w := watch.NewFakeWithChanSize(1, false)
				status := err.Status()
				w.Error(&status)
				return w, nil
			default:
				w := watch.NewFake()
				w.Stop()
				return w, nil
			}
		},
	}

	r := &Reflector{
		name:              "test-reflector",
		listerWatcher:     lw,
		store:             NewFIFO(MetaNamespaceKeyFunc),
		backoffManager:    bm,
		clock:             clock,
		watchErrorHandler: WatchErrorHandlerWithContext(DefaultWatchErrorHandler),
	}

	stopCh := make(chan struct{})
	if err := r.ListAndWatchWithContext(ctx); err != nil {
		t.Fatal(err)
	}
	close(stopCh)
	if bm.calls != 2 {
		t.Errorf("unexpected watch backoff calls: %d", bm.calls)
	}
}

func TestNoRelistOnTooManyRequests(t *testing.T) {
	err := apierrors.NewTooManyRequests("too many requests", 1)
	clock := &clock.RealClock{}
	bm := &fakeBackoff{clock: clock}
	listCalls, watchCalls := 0, 0

	lw := &ListWatch{
		ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
			listCalls++
			return &v1.PodList{ListMeta: metav1.ListMeta{ResourceVersion: "1"}}, nil
		},
		WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
			watchCalls++
			if watchCalls < 5 {
				return nil, err
			}
			w := watch.NewFake()
			w.Stop()
			return w, nil
		},
	}

	r := &Reflector{
		name:              "test-reflector",
		listerWatcher:     lw,
		store:             NewFIFO(MetaNamespaceKeyFunc),
		backoffManager:    bm,
		clock:             clock,
		watchErrorHandler: WatchErrorHandlerWithContext(DefaultWatchErrorHandler),
	}

	_, ctx := ktesting.NewTestContext(t)
	ctx, cancel := context.WithCancelCause(ctx)
	if err := r.ListAndWatchWithContext(ctx); err != nil {
		t.Fatal(err)
	}
	cancel(errors.New("done"))
	if listCalls != 1 {
		t.Errorf("unexpected list calls: %d", listCalls)
	}
	if watchCalls != 5 {
		t.Errorf("unexpected watch calls: %d", watchCalls)
	}
}

func TestRetryInternalError(t *testing.T) {
	testCases := []struct {
		name                string
		maxInternalDuration time.Duration
		rewindTime          int
		wantRetries         int
	}{
		{
			name:                "retries off",
			maxInternalDuration: time.Duration(0),
			wantRetries:         0,
		},
		{
			name:                "retries on, all calls fail",
			maxInternalDuration: time.Second * 30,
			wantRetries:         31,
		},
		{
			name:                "retries on, one call successful",
			maxInternalDuration: time.Second * 30,
			rewindTime:          10,
			wantRetries:         40,
		},
	}

	for _, tc := range testCases {
		err := apierrors.NewInternalError(fmt.Errorf("etcdserver: no leader"))
		fakeClock := testingclock.NewFakeClock(time.Now())
		bm := &fakeBackoff{clock: fakeClock}

		counter := 0

		lw := &ListWatch{
			ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
				return &v1.PodList{ListMeta: metav1.ListMeta{ResourceVersion: "1"}}, nil
			},
			WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
				counter = counter + 1
				t.Logf("Counter: %v", counter)
				if counter == tc.rewindTime {
					t.Logf("Rewinding")
					fakeClock.Step(time.Minute)
				}

				fakeClock.Step(time.Second)
				w := watch.NewFakeWithChanSize(1, false)
				status := err.Status()
				w.Error(&status)
				return w, nil
			},
		}

		r := &Reflector{
			name:              "test-reflector",
			listerWatcher:     lw,
			store:             NewFIFO(MetaNamespaceKeyFunc),
			backoffManager:    bm,
			clock:             fakeClock,
			watchErrorHandler: WatchErrorHandlerWithContext(DefaultWatchErrorHandler),
		}

		r.MaxInternalErrorRetryDuration = tc.maxInternalDuration

		_, ctx := ktesting.NewTestContext(t)
		ctx, cancel := context.WithCancelCause(ctx)
		require.NoError(t, r.ListAndWatchWithContext(ctx))
		cancel(errors.New("done"))

		if counter-1 != tc.wantRetries {
			t.Errorf("%v unexpected number of retries: %d", tc, counter-1)
		}
	}
}

func TestReflectorResync(t *testing.T) {
	iteration := 0
	_, ctx := ktesting.NewTestContext(t)
	rerr := errors.New("expected resync reached")
	s := &FakeCustomStore{
		ResyncFunc: func() error {
			iteration++
			if iteration == 2 {
				return rerr
			}
			return nil
		},
	}

	lw := &ListWatch{
		WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
			fw := watch.NewFake()
			return fw, nil
		},
		ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
			return &v1.PodList{ListMeta: metav1.ListMeta{ResourceVersion: "0"}}, nil
		},
	}
	resyncPeriod := 1 * time.Millisecond
	r := NewReflector(lw, &v1.Pod{}, s, resyncPeriod)
	if err := r.ListAndWatchWithContext(ctx); err != nil {
		// error from Resync is not propaged up to here.
		t.Errorf("expected error %v", err)
	}
	if iteration != 2 {
		t.Errorf("exactly 2 iterations were expected, got: %v", iteration)
	}
}

func TestReflectorWatchListPageSize(t *testing.T) {
	_, ctx := ktesting.NewTestContext(t)
	ctx, cancel := context.WithCancelCause(ctx)
	s := NewStore(MetaNamespaceKeyFunc)

	lw := &ListWatch{
		WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
			// Stop once the reflector begins watching since we're only interested in the list.
			cancel(errors.New("done"))
			fw := watch.NewFake()
			return fw, nil
		},
		ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
			if options.Limit != 4 {
				t.Fatalf("Expected list Limit of 4 but got %d", options.Limit)
			}
			pods := make([]v1.Pod, 10)
			for i := 0; i < 10; i++ {
				pods[i] = v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("pod-%d", i), ResourceVersion: fmt.Sprintf("%d", i)}}
			}
			switch options.Continue {
			case "":
				return &v1.PodList{ListMeta: metav1.ListMeta{ResourceVersion: "10", Continue: "C1"}, Items: pods[0:4]}, nil
			case "C1":
				return &v1.PodList{ListMeta: metav1.ListMeta{ResourceVersion: "10", Continue: "C2"}, Items: pods[4:8]}, nil
			case "C2":
				return &v1.PodList{ListMeta: metav1.ListMeta{ResourceVersion: "10"}, Items: pods[8:10]}, nil
			default:
				t.Fatalf("Unrecognized continue: %s", options.Continue)
			}
			return nil, nil
		},
	}
	r := NewReflector(lw, &v1.Pod{}, s, 0)
	// Set resource version to test pagination also for not consistent reads.
	r.setLastSyncResourceVersion("10")
	// Set the reflector to paginate the list request in 4 item chunks.
	r.WatchListPageSize = 4
	require.NoError(t, r.ListAndWatchWithContext(ctx))

	results := s.List()
	if len(results) != 10 {
		t.Errorf("Expected 10 results, got %d", len(results))
	}
}

func TestReflectorNotPaginatingNotConsistentReads(t *testing.T) {
	_, ctx := ktesting.NewTestContext(t)
	ctx, cancel := context.WithCancelCause(ctx)
	s := NewStore(MetaNamespaceKeyFunc)

	lw := &ListWatch{
		WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
			// Stop once the reflector begins watching since we're only interested in the list.
			cancel(errors.New("done"))
			fw := watch.NewFake()
			return fw, nil
		},
		ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
			if options.ResourceVersion != "10" {
				t.Fatalf("Expected ResourceVersion: \"10\", got: %s", options.ResourceVersion)
			}
			if options.Limit != 0 {
				t.Fatalf("Expected list Limit of 0 but got %d", options.Limit)
			}
			pods := make([]v1.Pod, 10)
			for i := 0; i < 10; i++ {
				pods[i] = v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("pod-%d", i), ResourceVersion: fmt.Sprintf("%d", i)}}
			}
			return &v1.PodList{ListMeta: metav1.ListMeta{ResourceVersion: "10"}, Items: pods}, nil
		},
	}
	r := NewReflector(lw, &v1.Pod{}, s, 0)
	r.setLastSyncResourceVersion("10")
	require.NoError(t, r.ListAndWatchWithContext(ctx))

	results := s.List()
	if len(results) != 10 {
		t.Errorf("Expected 10 results, got %d", len(results))
	}
}

func TestReflectorPaginatingNonConsistentReadsIfWatchCacheDisabled(t *testing.T) {
	_, ctx := ktesting.NewTestContext(t)
	var cancel func(error)
	s := NewStore(MetaNamespaceKeyFunc)

	lw := &ListWatch{
		WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
			// Stop once the reflector begins watching since we're only interested in the list.
			cancel(errors.New("done"))
			fw := watch.NewFake()
			return fw, nil
		},
		ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
			// Check that default pager limit is set.
			if options.Limit != 500 {
				t.Fatalf("Expected list Limit of 500 but got %d", options.Limit)
			}
			pods := make([]v1.Pod, 10)
			for i := 0; i < 10; i++ {
				pods[i] = v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("pod-%d", i), ResourceVersion: fmt.Sprintf("%d", i)}}
			}
			switch options.Continue {
			case "":
				return &v1.PodList{ListMeta: metav1.ListMeta{ResourceVersion: "10", Continue: "C1"}, Items: pods[0:4]}, nil
			case "C1":
				return &v1.PodList{ListMeta: metav1.ListMeta{ResourceVersion: "10", Continue: "C2"}, Items: pods[4:8]}, nil
			case "C2":
				return &v1.PodList{ListMeta: metav1.ListMeta{ResourceVersion: "10"}, Items: pods[8:10]}, nil
			default:
				t.Fatalf("Unrecognized continue: %s", options.Continue)
			}
			return nil, nil
		},
	}
	r := NewReflector(lw, &v1.Pod{}, s, 0)

	// Initial list should initialize paginatedResult in the reflector.
	var cancelCtx context.Context
	cancelCtx, cancel = context.WithCancelCause(ctx)
	require.NoError(t, r.ListAndWatchWithContext(cancelCtx))
	if results := s.List(); len(results) != 10 {
		t.Errorf("Expected 10 results, got %d", len(results))
	}

	// Since initial list for ResourceVersion="0" was paginated, the subsequent
	// ones should also be paginated.
	cancelCtx, cancel = context.WithCancelCause(ctx)
	require.NoError(t, r.ListAndWatchWithContext(cancelCtx))
	if results := s.List(); len(results) != 10 {
		t.Errorf("Expected 10 results, got %d", len(results))
	}
}

// TestReflectorResyncWithResourceVersion ensures that a reflector keeps track of the ResourceVersion and sends
// it in relist requests to prevent the reflector from traveling back in time if the relist is to a api-server or
// etcd that is partitioned and serving older data than the reflector has already processed.
func TestReflectorResyncWithResourceVersion(t *testing.T) {
	_, ctx := ktesting.NewTestContext(t)
	cancelCtx, cancel := context.WithCancelCause(ctx)
	s := NewStore(MetaNamespaceKeyFunc)
	listCallRVs := []string{}

	lw := &ListWatch{
		WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
			// Stop once the reflector begins watching since we're only interested in the list.
			cancel(errors.New("done"))
			fw := watch.NewFake()
			return fw, nil
		},
		ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
			listCallRVs = append(listCallRVs, options.ResourceVersion)
			pods := make([]v1.Pod, 8)
			for i := 0; i < 8; i++ {
				pods[i] = v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("pod-%d", i), ResourceVersion: fmt.Sprintf("%d", i)}}
			}
			switch options.ResourceVersion {
			case "0":
				return &v1.PodList{ListMeta: metav1.ListMeta{ResourceVersion: "10"}, Items: pods[0:4]}, nil
			case "10":
				return &v1.PodList{ListMeta: metav1.ListMeta{ResourceVersion: "11"}, Items: pods[0:8]}, nil
			default:
				t.Fatalf("Unrecognized ResourceVersion: %s", options.ResourceVersion)
			}
			return nil, nil
		},
	}
	r := NewReflector(lw, &v1.Pod{}, s, 0)

	// Initial list should use RV=0
	require.NoError(t, r.ListAndWatchWithContext(cancelCtx))

	results := s.List()
	if len(results) != 4 {
		t.Errorf("Expected 4 results, got %d", len(results))
	}

	// relist should use lastSyncResourceVersions (RV=10)
	cancelCtx, cancel = context.WithCancelCause(ctx)
	require.NoError(t, r.ListAndWatchWithContext(cancelCtx))

	results = s.List()
	if len(results) != 8 {
		t.Errorf("Expected 8 results, got %d", len(results))
	}

	expectedRVs := []string{"0", "10"}
	if !reflect.DeepEqual(listCallRVs, expectedRVs) {
		t.Errorf("Expected series of list calls with resource versiosn of %v but got: %v", expectedRVs, listCallRVs)
	}
}

// TestReflectorExpiredExactResourceVersion tests that a reflector handles the behavior of kubernetes 1.16 an earlier
// where if the exact ResourceVersion requested is not available for a List request for a non-zero ResourceVersion,
// an "Expired" error is returned if the ResourceVersion has expired (etcd has compacted it).
// (In kubernetes 1.17, or when the watch cache is enabled, the List will instead return the list that is no older than
// the requested ResourceVersion).
func TestReflectorExpiredExactResourceVersion(t *testing.T) {
	_, ctx := ktesting.NewTestContext(t)
	var cancelCtx context.Context
	var cancel func(error)
	s := NewStore(MetaNamespaceKeyFunc)
	listCallRVs := []string{}

	lw := &ListWatch{
		WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
			// Stop once the reflector begins watching since we're only interested in the list.
			cancel(errors.New("done"))
			fw := watch.NewFake()
			return fw, nil
		},
		ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
			listCallRVs = append(listCallRVs, options.ResourceVersion)
			pods := make([]v1.Pod, 8)
			for i := 0; i < 8; i++ {
				pods[i] = v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("pod-%d", i), ResourceVersion: fmt.Sprintf("%d", i)}}
			}
			switch options.ResourceVersion {
			case "0":
				return &v1.PodList{ListMeta: metav1.ListMeta{ResourceVersion: "10"}, Items: pods[0:4]}, nil
			case "10":
				// When watch cache is disabled, if the exact ResourceVersion requested is not available, a "Expired" error is returned.
				return nil, apierrors.NewResourceExpired("The resourceVersion for the provided watch is too old.")
			case "":
				return &v1.PodList{ListMeta: metav1.ListMeta{ResourceVersion: "11"}, Items: pods[0:8]}, nil
			default:
				t.Fatalf("Unrecognized ResourceVersion: %s", options.ResourceVersion)
			}
			return nil, nil
		},
	}
	r := NewReflector(lw, &v1.Pod{}, s, 0)

	// Initial list should use RV=0
	cancelCtx, cancel = context.WithCancelCause(ctx)
	require.NoError(t, r.ListAndWatchWithContext(cancelCtx))

	results := s.List()
	if len(results) != 4 {
		t.Errorf("Expected 4 results, got %d", len(results))
	}

	// relist should use lastSyncResourceVersions (RV=10) and since RV=10 is expired, it should retry with RV="".
	cancelCtx, cancel = context.WithCancelCause(ctx)
	require.NoError(t, r.ListAndWatchWithContext(cancelCtx))

	results = s.List()
	if len(results) != 8 {
		t.Errorf("Expected 8 results, got %d", len(results))
	}

	expectedRVs := []string{"0", "10", ""}
	if !reflect.DeepEqual(listCallRVs, expectedRVs) {
		t.Errorf("Expected series of list calls with resource versiosn of %v but got: %v", expectedRVs, listCallRVs)
	}
}

func TestReflectorFullListIfExpired(t *testing.T) {
	_, ctx := ktesting.NewTestContext(t)
	var cancelCtx context.Context
	var cancel func(error)
	s := NewStore(MetaNamespaceKeyFunc)
	listCallRVs := []string{}

	lw := &ListWatch{
		WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
			// Stop once the reflector begins watching since we're only interested in the list.
			cancel(errors.New("done"))
			fw := watch.NewFake()
			return fw, nil
		},
		ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
			listCallRVs = append(listCallRVs, options.ResourceVersion)
			pods := make([]v1.Pod, 8)
			for i := 0; i < 8; i++ {
				pods[i] = v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("pod-%d", i), ResourceVersion: fmt.Sprintf("%d", i)}}
			}
			rvContinueLimit := func(rv, c string, l int64) metav1.ListOptions {
				return metav1.ListOptions{ResourceVersion: rv, Continue: c, Limit: l}
			}
			switch rvContinueLimit(options.ResourceVersion, options.Continue, options.Limit) {
			// initial limited list
			case rvContinueLimit("0", "", 4):
				return &v1.PodList{ListMeta: metav1.ListMeta{ResourceVersion: "10"}, Items: pods[0:4]}, nil
			// first page of the rv=10 list
			case rvContinueLimit("10", "", 4):
				return &v1.PodList{ListMeta: metav1.ListMeta{Continue: "C1", ResourceVersion: "11"}, Items: pods[0:4]}, nil
			// second page of the above list
			case rvContinueLimit("", "C1", 4):
				return nil, apierrors.NewResourceExpired("The resourceVersion for the provided watch is too old.")
			// rv=10 unlimited list
			case rvContinueLimit("10", "", 0):
				return &v1.PodList{ListMeta: metav1.ListMeta{ResourceVersion: "11"}, Items: pods[0:8]}, nil
			default:
				err := fmt.Errorf("unexpected list options: %#v", options)
				t.Error(err)
				return nil, err
			}
		},
	}
	r := NewReflector(lw, &v1.Pod{}, s, 0)
	r.WatchListPageSize = 4

	// Initial list should use RV=0
	cancelCtx, cancel = context.WithCancelCause(ctx)
	if err := r.ListAndWatchWithContext(cancelCtx); err != nil {
		t.Fatal(err)
	}

	results := s.List()
	if len(results) != 4 {
		t.Errorf("Expected 4 results, got %d", len(results))
	}

	// relist should use lastSyncResourceVersions (RV=10) and since second page of that expired, it should full list with RV=10
	cancelCtx, cancel = context.WithCancelCause(ctx)
	if err := r.ListAndWatchWithContext(cancelCtx); err != nil {
		t.Fatal(err)
	}

	results = s.List()
	if len(results) != 8 {
		t.Errorf("Expected 8 results, got %d", len(results))
	}

	expectedRVs := []string{"0", "10", "", "10"}
	if !reflect.DeepEqual(listCallRVs, expectedRVs) {
		t.Errorf("Expected series of list calls with resource versiosn of %#v but got: %#v", expectedRVs, listCallRVs)
	}
}

func TestReflectorFullListIfTooLarge(t *testing.T) {
	_, ctx := ktesting.NewTestContext(t)
	var cancelCtx context.Context
	var cancel func(error)
	s := NewStore(MetaNamespaceKeyFunc)
	listCallRVs := []string{}
	version := 30

	lw := &ListWatch{
		WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
			// Stop once the reflector begins watching since we're only interested in the list.
			cancel(errors.New("done"))
			fw := watch.NewFake()
			return fw, nil
		},
		ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
			listCallRVs = append(listCallRVs, options.ResourceVersion)
			resourceVersion := strconv.Itoa(version)

			switch options.ResourceVersion {
			// initial list
			case "0":
				return &v1.PodList{ListMeta: metav1.ListMeta{ResourceVersion: "20"}}, nil
			// relist after the initial list
			case "20":
				err := apierrors.NewTimeoutError("too large resource version", 1)
				err.ErrStatus.Details.Causes = []metav1.StatusCause{{Type: metav1.CauseTypeResourceVersionTooLarge}}
				return nil, err
			// relist after the initial list (covers the error format used in api server 1.17.0-1.18.5)
			case "30":
				err := apierrors.NewTimeoutError("too large resource version", 1)
				err.ErrStatus.Details.Causes = []metav1.StatusCause{{Message: "Too large resource version"}}
				return nil, err
			// relist after the initial list (covers the error format used in api server before 1.17.0)
			case "40":
				err := apierrors.NewTimeoutError("Too large resource version", 1)
				return nil, err
			// relist from etcd after "too large" error
			case "":
				version += 10
				return &v1.PodList{ListMeta: metav1.ListMeta{ResourceVersion: resourceVersion}}, nil
			default:
				return nil, fmt.Errorf("unexpected List call: %s", options.ResourceVersion)
			}
		},
	}
	r := NewReflector(lw, &v1.Pod{}, s, 0)

	// Initial list should use RV=0
	cancelCtx, cancel = context.WithCancelCause(ctx)
	if err := r.ListAndWatchWithContext(cancelCtx); err != nil {
		t.Fatal(err)
	}

	// Relist from the future version.
	// This may happen, as watchcache is initialized from "current global etcd resource version"
	// when kube-apiserver is starting and if no objects are changing after that each kube-apiserver
	// may be synced to a different version and they will never converge.
	// TODO: We should use etcd progress-notify feature to avoid this behavior but until this is
	// done we simply try to relist from now to avoid continuous errors on relists.
	for i := 1; i <= 3; i++ {
		// relist twice to cover the two variants of TooLargeResourceVersion api errors
		cancelCtx, cancel = context.WithCancelCause(ctx)
		if err := r.ListAndWatchWithContext(cancelCtx); err != nil {
			t.Fatal(err)
		}
	}

	expectedRVs := []string{"0", "20", "", "30", "", "40", ""}
	if !reflect.DeepEqual(listCallRVs, expectedRVs) {
		t.Errorf("Expected series of list calls with resource version of %#v but got: %#v", expectedRVs, listCallRVs)
	}
}

func TestGetTypeDescriptionFromObject(t *testing.T) {
	obj := &unstructured.Unstructured{}
	gvk := schema.GroupVersionKind{
		Group:   "mygroup",
		Version: "v1",
		Kind:    "MyKind",
	}
	obj.SetGroupVersionKind(gvk)

	testCases := map[string]struct {
		inputType               interface{}
		expectedTypeDescription string
	}{
		"Nil type": {
			expectedTypeDescription: defaultExpectedTypeName,
		},
		"Normal type": {
			inputType:               &v1.Pod{},
			expectedTypeDescription: "*v1.Pod",
		},
		"Unstructured type without GVK": {
			inputType:               &unstructured.Unstructured{},
			expectedTypeDescription: "*unstructured.Unstructured",
		},
		"Unstructured type with GVK": {
			inputType:               obj,
			expectedTypeDescription: gvk.String(),
		},
	}
	for testName, tc := range testCases {
		t.Run(testName, func(t *testing.T) {
			typeDescription := getTypeDescriptionFromObject(tc.inputType)
			if tc.expectedTypeDescription != typeDescription {
				t.Fatalf("Expected typeDescription %v, got %v", tc.expectedTypeDescription, typeDescription)
			}
		})
	}
}

func TestGetExpectedGVKFromObject(t *testing.T) {
	obj := &unstructured.Unstructured{}
	gvk := schema.GroupVersionKind{
		Group:   "mygroup",
		Version: "v1",
		Kind:    "MyKind",
	}
	obj.SetGroupVersionKind(gvk)

	testCases := map[string]struct {
		inputType   interface{}
		expectedGVK *schema.GroupVersionKind
	}{
		"Nil type": {},
		"Some non Unstructured type": {
			inputType: &v1.Pod{},
		},
		"Unstructured type without GVK": {
			inputType: &unstructured.Unstructured{},
		},
		"Unstructured type with GVK": {
			inputType:   obj,
			expectedGVK: &gvk,
		},
	}
	for testName, tc := range testCases {
		t.Run(testName, func(t *testing.T) {
			expectedGVK := getExpectedGVKFromObject(tc.inputType)
			gvkNotEqual := (tc.expectedGVK == nil) != (expectedGVK == nil)
			if tc.expectedGVK != nil && expectedGVK != nil {
				gvkNotEqual = *tc.expectedGVK != *expectedGVK
			}
			if gvkNotEqual {
				t.Fatalf("Expected expectedGVK %v, got %v", tc.expectedGVK, expectedGVK)
			}
		})
	}
}

func TestWatchTimeout(t *testing.T) {

	testCases := []struct {
		name                      string
		minWatchTimeout           time.Duration
		expectedMinTimeoutSeconds int64
	}{
		{
			name:                      "no timeout",
			expectedMinTimeoutSeconds: 5 * 60,
		},
		{
			name:                      "small timeout not honored",
			minWatchTimeout:           time.Second,
			expectedMinTimeoutSeconds: 5 * 60,
		},
		{
			name:                      "30m timeout",
			minWatchTimeout:           30 * time.Minute,
			expectedMinTimeoutSeconds: 30 * 60,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			_, ctx := ktesting.NewTestContext(t)
			ctx, cancel := context.WithCancelCause(ctx)
			s := NewStore(MetaNamespaceKeyFunc)
			var gotTimeoutSeconds int64

			lw := &ListWatch{
				ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
					return &v1.PodList{ListMeta: metav1.ListMeta{ResourceVersion: "10"}}, nil
				},
				WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
					if options.TimeoutSeconds != nil {
						gotTimeoutSeconds = *options.TimeoutSeconds
					}

					// Stop once the reflector begins watching since we're only interested in the list.
					cancel(errors.New("done"))
					return watch.NewFake(), nil
				},
			}

			opts := ReflectorOptions{
				MinWatchTimeout: tc.minWatchTimeout,
			}
			r := NewReflectorWithOptions(lw, &v1.Pod{}, s, opts)
			if err := r.ListAndWatchWithContext(ctx); err != nil {
				t.Fatal(err)
			}

			minExpected := tc.expectedMinTimeoutSeconds
			maxExpected := 2 * tc.expectedMinTimeoutSeconds
			if gotTimeoutSeconds < minExpected || gotTimeoutSeconds > maxExpected {
				t.Errorf("unexpected TimeoutSecond, got %v, expected in [%v, %v]", gotTimeoutSeconds, minExpected, maxExpected)
			}
		})
	}
}

type storeWithRV struct {
	Store

	// resourceVersions tracks values passed by UpdateResourceVersion
	resourceVersions []string
}

func (s *storeWithRV) UpdateResourceVersion(resourceVersion string) {
	s.resourceVersions = append(s.resourceVersions, resourceVersion)
}

func newStoreWithRV() *storeWithRV {
	return &storeWithRV{
		Store: NewStore(MetaNamespaceKeyFunc),
	}
}

func TestReflectorResourceVersionUpdate(t *testing.T) {
	s := newStoreWithRV()

	_, ctx := ktesting.NewTestContext(t)
	ctx, cancel := context.WithCancelCause(ctx)
	fw := watch.NewFake()

	lw := &ListWatch{
		WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
			return fw, nil
		},
		ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
			return &v1.PodList{ListMeta: metav1.ListMeta{ResourceVersion: "10"}}, nil
		},
	}
	r := NewReflector(lw, &v1.Pod{}, s, 0)

	makePod := func(rv string) *v1.Pod {
		return &v1.Pod{ObjectMeta: metav1.ObjectMeta{ResourceVersion: rv}}
	}

	go func() {
		fw.Action(watch.Added, makePod("10"))
		fw.Action(watch.Modified, makePod("20"))
		fw.Action(watch.Bookmark, makePod("30"))
		fw.Action(watch.Deleted, makePod("40"))
		cancel(errors.New("done"))
	}()

	// Initial list should use RV=0
	if err := r.ListAndWatchWithContext(ctx); err != nil {
		t.Fatal(err)
	}

	expectedRVs := []string{"10", "20", "30", "40"}
	if !reflect.DeepEqual(s.resourceVersions, expectedRVs) {
		t.Errorf("Expected series of resource version updates of %#v but got: %#v", expectedRVs, s.resourceVersions)
	}
}

const (
	fakeItemsNum      = 100
	exemptObjectIndex = fakeItemsNum / 4
	pageNum           = 3
)

func getPodListItems(start int, numItems int) (string, string, *v1.PodList) {
	out := &v1.PodList{
		Items: make([]v1.Pod, numItems),
	}

	for i := 0; i < numItems; i++ {

		out.Items[i] = v1.Pod{
			TypeMeta: metav1.TypeMeta{
				APIVersion: "v1",
				Kind:       "Pod",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("pod-%d", i+start),
				Namespace: "default",
				Labels: map[string]string{
					"label-key-1": "label-value-1",
				},
				Annotations: map[string]string{
					"annotations-key-1": "annotations-value-1",
				},
			},
			Spec: v1.PodSpec{
				Overhead: v1.ResourceList{
					v1.ResourceCPU:    resource.MustParse("3"),
					v1.ResourceMemory: resource.MustParse("8"),
				},
				NodeSelector: map[string]string{
					"foo": "bar",
					"baz": "quux",
				},
				Affinity: &v1.Affinity{
					NodeAffinity: &v1.NodeAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: &v1.NodeSelector{
							NodeSelectorTerms: []v1.NodeSelectorTerm{
								{MatchExpressions: []v1.NodeSelectorRequirement{{Key: `foo`}}},
							},
						},
						PreferredDuringSchedulingIgnoredDuringExecution: []v1.PreferredSchedulingTerm{
							{Preference: v1.NodeSelectorTerm{MatchExpressions: []v1.NodeSelectorRequirement{{Key: `foo`}}}},
						},
					},
				},
				TopologySpreadConstraints: []v1.TopologySpreadConstraint{
					{TopologyKey: `foo`},
				},
				HostAliases: []v1.HostAlias{
					{IP: "1.1.1.1"},
					{IP: "2.2.2.2"},
				},
				ImagePullSecrets: []v1.LocalObjectReference{
					{Name: "secret1"},
					{Name: "secret2"},
				},
				Containers: []v1.Container{
					{
						Name:  "foobar",
						Image: "alpine",
						Resources: v1.ResourceRequirements{
							Requests: v1.ResourceList{
								v1.ResourceName(v1.ResourceCPU):    resource.MustParse("1"),
								v1.ResourceName(v1.ResourceMemory): resource.MustParse("5"),
							},
							Limits: v1.ResourceList{
								v1.ResourceName(v1.ResourceCPU):    resource.MustParse("2"),
								v1.ResourceName(v1.ResourceMemory): resource.MustParse("10"),
							},
						},
					},
					{
						Name:  "foobar2",
						Image: "alpine",
						Resources: v1.ResourceRequirements{
							Requests: v1.ResourceList{
								v1.ResourceName(v1.ResourceCPU):    resource.MustParse("4"),
								v1.ResourceName(v1.ResourceMemory): resource.MustParse("12"),
							},
							Limits: v1.ResourceList{
								v1.ResourceName(v1.ResourceCPU):    resource.MustParse("8"),
								v1.ResourceName(v1.ResourceMemory): resource.MustParse("24"),
							},
						},
					},
				},
				InitContainers: []v1.Container{
					{
						Name:  "small-init",
						Image: "alpine",
						Resources: v1.ResourceRequirements{
							Requests: v1.ResourceList{
								v1.ResourceName(v1.ResourceCPU):    resource.MustParse("1"),
								v1.ResourceName(v1.ResourceMemory): resource.MustParse("5"),
							},
							Limits: v1.ResourceList{
								v1.ResourceName(v1.ResourceCPU):    resource.MustParse("1"),
								v1.ResourceName(v1.ResourceMemory): resource.MustParse("5"),
							},
						},
					},
					{
						Name:  "big-init",
						Image: "alpine",
						Resources: v1.ResourceRequirements{
							Requests: v1.ResourceList{
								v1.ResourceName(v1.ResourceCPU):    resource.MustParse("40"),
								v1.ResourceName(v1.ResourceMemory): resource.MustParse("120"),
							},
							Limits: v1.ResourceList{
								v1.ResourceName(v1.ResourceCPU):    resource.MustParse("80"),
								v1.ResourceName(v1.ResourceMemory): resource.MustParse("240"),
							},
						},
					},
				},
				Hostname: fmt.Sprintf("node-%d", i),
			},
			Status: v1.PodStatus{
				Phase: v1.PodRunning,
				ContainerStatuses: []v1.ContainerStatus{
					{
						ContainerID: "docker://numbers",
						Image:       "alpine",
						Name:        "foobar",
						Ready:       false,
					},
					{
						ContainerID: "docker://numbers",
						Image:       "alpine",
						Name:        "foobar2",
						Ready:       false,
					},
				},
				InitContainerStatuses: []v1.ContainerStatus{
					{
						ContainerID: "docker://numbers",
						Image:       "alpine",
						Name:        "small-init",
						Ready:       false,
					},
					{
						ContainerID: "docker://numbers",
						Image:       "alpine",
						Name:        "big-init",
						Ready:       false,
					},
				},
				Conditions: []v1.PodCondition{
					{
						Type:               v1.PodScheduled,
						Status:             v1.ConditionTrue,
						Reason:             "successfully",
						Message:            "sync pod successfully",
						LastProbeTime:      metav1.Now(),
						LastTransitionTime: metav1.Now(),
					},
				},
			},
		}
	}

	return out.Items[0].GetName(), out.Items[exemptObjectIndex].GetName(), out
}

func getConfigmapListItems(start int, numItems int) (string, string, *v1.ConfigMapList) {
	out := &v1.ConfigMapList{
		Items: make([]v1.ConfigMap, numItems),
	}

	for i := 0; i < numItems; i++ {
		out.Items[i] = v1.ConfigMap{
			TypeMeta: metav1.TypeMeta{
				APIVersion: "v1",
				Kind:       "ConfigMap",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("cm-%d", i+start),
				Namespace: "default",
				Labels: map[string]string{
					"label-key-1": "label-value-1",
				},
				Annotations: map[string]string{
					"annotations-key-1": "annotations-value-1",
				},
			},
			Data: map[string]string{
				"data-1": "value-1",
				"data-2": "value-2",
			},
		}
	}

	return out.Items[0].GetName(), out.Items[exemptObjectIndex].GetName(), out
}

type TestPagingPodsLW struct {
	totalPageCount   int
	fetchedPageCount int

	detectedObjectNameList []string
	exemptObjectNameList   []string
}

func newPageTestLW(totalPageNum int) *TestPagingPodsLW {
	return &TestPagingPodsLW{
		totalPageCount:   totalPageNum,
		fetchedPageCount: 0,
	}
}

func (t *TestPagingPodsLW) List(options metav1.ListOptions) (runtime.Object, error) {
	firstPodName, exemptPodName, list := getPodListItems(t.fetchedPageCount*fakeItemsNum, fakeItemsNum)
	t.detectedObjectNameList = append(t.detectedObjectNameList, firstPodName)
	t.exemptObjectNameList = append(t.exemptObjectNameList, exemptPodName)
	t.fetchedPageCount++
	if t.fetchedPageCount >= t.totalPageCount {
		return list, nil
	}
	list.SetContinue("true")
	return list, nil
}

func (t *TestPagingPodsLW) Watch(options metav1.ListOptions) (watch.Interface, error) {
	return nil, nil
}

func TestReflectorListExtract(t *testing.T) {
	_, ctx := ktesting.NewTestContext(t)
	store := NewStore(func(obj interface{}) (string, error) {
		pod, ok := obj.(*v1.Pod)
		if !ok {
			return "", fmt.Errorf("expect *v1.Pod, but got %T", obj)
		}
		return pod.GetName(), nil
	})

	lw := newPageTestLW(5)
	reflector := NewReflector(lw, &v1.Pod{}, store, 0)
	reflector.WatchListPageSize = fakeItemsNum

	// execute list to fill store
	if err := reflector.list(ctx); err != nil {
		t.Fatal(err)
	}

	// We will not delete exemptPod,
	// in order to see if the existence of this Pod causes other Pods that are not used to be unable to properly clear.
	for _, podName := range lw.exemptObjectNameList {
		_, exist, err := store.GetByKey(podName)
		if err != nil || !exist {
			t.Fatalf("%s should exist in pod store", podName)
		}
	}

	// we will pay attention to whether the memory occupied by the first Pod is released
	// Golang's can only be SetFinalizer for the first element of the array,
	// so pod-0 will be the object of our attention
	detectedPodAlreadyBeCleared := make(chan struct{}, len(lw.detectedObjectNameList))

	for _, firstPodName := range lw.detectedObjectNameList {
		_, exist, err := store.GetByKey(firstPodName)
		if err != nil || !exist {
			t.Fatalf("%s should exist in pod store", firstPodName)
		}
		firstPod, exist, err := store.GetByKey(firstPodName)
		if err != nil || !exist {
			t.Fatalf("%s should exist in pod store", firstPodName)
		}
		goruntime.SetFinalizer(firstPod, func(obj interface{}) {
			t.Logf("%s already be gc\n", obj.(*v1.Pod).GetName())
			detectedPodAlreadyBeCleared <- struct{}{}
		})
	}

	storedObjectKeys := store.ListKeys()
	for _, k := range storedObjectKeys {
		// delete all Pods except the exempted Pods.
		if sets.NewString(lw.exemptObjectNameList...).Has(k) {
			continue
		}
		obj, exist, err := store.GetByKey(k)
		if err != nil || !exist {
			t.Fatalf("%s should exist in pod store", k)
		}

		if err := store.Delete(obj); err != nil {
			t.Fatalf("delete object: %v", err)
		}
		goruntime.GC()
	}

	clearedNum := 0
	for {
		select {
		case <-detectedPodAlreadyBeCleared:
			clearedNum++
			if clearedNum == len(lw.detectedObjectNameList) {
				return
			}
		}
	}
}

func TestReflectorReplacesStoreOnUnsafeDelete(t *testing.T) {
	mkPod := func(id string, rv string) *v1.Pod {
		return &v1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: id, ResourceVersion: rv}}
	}
	mkList := func(rv string, pods ...*v1.Pod) *v1.PodList {
		list := &v1.PodList{ListMeta: metav1.ListMeta{ResourceVersion: rv}}
		for _, pod := range pods {
			list.Items = append(list.Items, *pod)
		}
		return list
	}
	makeStatus := func() *metav1.Status {
		return &metav1.Status{
			Status:  metav1.StatusFailure,
			Code:    http.StatusInternalServerError,
			Reason:  metav1.StatusReasonStoreReadError,
			Message: "failed to prepare current and previous objects: corrupt object has been deleted",
		}
	}

	// these pods preexist and never get updated/deleted
	preExisting := mkPod("foo-1", "1")
	pods := []*v1.Pod{preExisting, mkPod("foo-2", "2"), mkPod("foo-3", "3")}
	lastExpectedRV := "5"
	lists := []*v1.PodList{
		mkList("3", pods...),            // initial list
		mkList(lastExpectedRV, pods...), // re-list due to watch error
	}
	corruptObj := mkPod("foo", "4")
	events := []watch.Event{
		{Type: watch.Added, Object: corruptObj},
		// the object becomes corrupt, and it gets unsafe-deleted, and
		// watch sends the following Error event, note the RV has
		// advanced to "5" in the storage due to the delete operation
		{Type: watch.Error, Object: makeStatus()},
	}

	s := NewFIFO(MetaNamespaceKeyFunc)
	var replaceInvoked atomic.Int32
	store := &fakeStore{
		Store: s,
		beforeReplace: func(list []interface{}, rv string) {
			// interested in the Replace call that happens after the Error event
			if rv == lastExpectedRV {
				replaceInvoked.Add(1)
				_, exists, err := s.Get(corruptObj)
				if err != nil || !exists {
					t.Errorf("expected the object to exist in the store, exists: %t, err: %v", exists, err)
				}
				_, exists, err = s.Get(preExisting)
				if err != nil || !exists {
					t.Errorf("expected the pre-existing object to be in the store, exists: %t, err: %v", exists, err)
				}
			}
		},
		afterReplace: func(rv string, err error) {
			if rv == lastExpectedRV {
				replaceInvoked.Add(1)
				if err != nil {
					t.Errorf("expected Replace to have succeeded, but got error: %v", err)
				}
				_, exists, err := s.Get(corruptObj)
				if err != nil || exists {
					t.Errorf("expected the object to have been removed from the store, exists: %t, err: %v", exists, err)
				}
				// show that a pre-existing pod is still in the cache
				_, exists, err = s.Get(preExisting)
				if err != nil || !exists {
					t.Errorf("expected the pre-existing object to be in the store, exists: %t, err: %v", exists, err)
				}
			}
		},
	}

	var once sync.Once
	lw := &ListWatch{
		WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
			fw := watch.NewFake()
			go func() {
				once.Do(func() {
					for _, e := range events {
						fw.Action(e.Type, e.Object)
					}
				})
			}()
			return fw, nil
		},
		ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
			var list runtime.Object
			if len(lists) > 0 {
				list = lists[0]
				lists = lists[1:]
			}
			return list, nil
		},
	}

	r := NewReflector(lw, &v1.Pod{}, store, 0)
	doneCh, stopCh := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(doneCh)
		//nolint:logcheck // Intentionally uses the old API.
		r.Run(stopCh)
	}()

	// wait for the RV to sync to the version returned by the final list
	err := wait.PollUntilContextTimeout(context.Background(), 100*time.Millisecond, wait.ForeverTestTimeout, true, func(ctx context.Context) (done bool, err error) {
		if rv := r.LastSyncResourceVersion(); rv == lastExpectedRV {
			return true, nil
		}
		return false, nil
	})
	if err != nil {
		t.Fatalf("reflector never caught up with expected revision: %q, err: %v", lastExpectedRV, err)
	}

	if want, got := lastExpectedRV, r.LastSyncResourceVersion(); want != got {
		t.Errorf("expected LastSyncResourceVersion to be %q, but got: %q", want, got)
	}
	if want, got := 2, int(replaceInvoked.Load()); want != got {
		t.Errorf("expected store Delete hooks to be invoked %d times, but got: %d", want, got)
	}
	if want, got := len(pods), len(s.List()); want != got {
		t.Errorf("expected the store to have %d objects, but got: %d", want, got)
	}

	close(stopCh)
	select {
	case <-doneCh:
	case <-time.After(wait.ForeverTestTimeout):
		t.Errorf("timed out waiting for Run to return")
	}
}

type fakeStore struct {
	Store
	beforeReplace func(list []interface{}, s string)
	afterReplace  func(rv string, err error)
}

func (f *fakeStore) Replace(list []interface{}, rv string) error {
	f.beforeReplace(list, rv)
	err := f.Store.Replace(list, rv)
	f.afterReplace(rv, err)
	return err
}

func BenchmarkExtractList(b *testing.B) {
	_, _, podList := getPodListItems(0, fakeItemsNum)
	_, _, configMapList := getConfigmapListItems(0, fakeItemsNum)
	tests := []struct {
		name string
		list runtime.Object
	}{
		{
			name: "PodList",
			list: podList,
		},
		{
			name: "ConfigMapList",
			list: configMapList,
		},
	}

	for _, tc := range tests {
		b.Run(tc.name, func(b *testing.B) {
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_, err := meta.ExtractList(tc.list)
				if err != nil {
					b.Errorf("extract list: %v", err)
				}
			}
			b.StopTimer()
		})
	}
}

func BenchmarkEachListItem(b *testing.B) {
	_, _, podList := getPodListItems(0, fakeItemsNum)
	_, _, configMapList := getConfigmapListItems(0, fakeItemsNum)
	tests := []struct {
		name string
		list runtime.Object
	}{
		{
			name: "PodList",
			list: podList,
		},
		{
			name: "ConfigMapList",
			list: configMapList,
		},
	}

	for _, tc := range tests {
		b.Run(tc.name, func(b *testing.B) {
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				err := meta.EachListItem(tc.list, func(object runtime.Object) error {
					return nil
				})
				if err != nil {
					b.Errorf("each list: %v", err)
				}
			}
			b.StopTimer()
		})
	}
}

func BenchmarkExtractListWithAlloc(b *testing.B) {
	_, _, podList := getPodListItems(0, fakeItemsNum)
	_, _, configMapList := getConfigmapListItems(0, fakeItemsNum)
	tests := []struct {
		name string
		list runtime.Object
	}{
		{
			name: "PodList",
			list: podList,
		},
		{
			name: "ConfigMapList",
			list: configMapList,
		},
	}

	for _, tc := range tests {
		b.Run(tc.name, func(b *testing.B) {
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_, err := meta.ExtractListWithAlloc(tc.list)
				if err != nil {
					b.Errorf("extract list with alloc: %v", err)
				}
			}
			b.StopTimer()
		})
	}
}

func BenchmarkEachListItemWithAlloc(b *testing.B) {
	_, _, podList := getPodListItems(0, fakeItemsNum)
	_, _, configMapList := getConfigmapListItems(0, fakeItemsNum)
	tests := []struct {
		name string
		list runtime.Object
	}{
		{
			name: "PodList",
			list: podList,
		},
		{
			name: "ConfigMapList",
			list: configMapList,
		},
	}

	for _, tc := range tests {
		b.Run(tc.name, func(b *testing.B) {
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				err := meta.EachListItemWithAlloc(tc.list, func(object runtime.Object) error {
					return nil
				})
				if err != nil {
					b.Errorf("each list with alloc: %v", err)
				}
			}
			b.StopTimer()
		})
	}
}

func BenchmarkReflectorList(b *testing.B) {
	store := NewStore(func(obj interface{}) (string, error) {
		o, err := meta.Accessor(obj)
		if err != nil {
			return "", err
		}
		return o.GetName(), nil
	})

	_, _, podList := getPodListItems(0, fakeItemsNum)
	_, _, configMapList := getConfigmapListItems(0, fakeItemsNum)
	tests := []struct {
		name   string
		sample func() interface{}
		list   runtime.Object
	}{
		{
			name: "PodList",
			sample: func() interface{} {
				return v1.Pod{}
			},
			list: podList,
		},
		{
			name: "ConfigMapList",
			sample: func() interface{} {
				return v1.ConfigMap{}
			},
			list: configMapList,
		},
	}

	for _, tc := range tests {
		b.Run(tc.name, func(b *testing.B) {
			_, ctx := ktesting.NewTestContext(b)

			sample := tc.sample()
			reflector := NewReflector(newPageTestLW(pageNum), &sample, store, 0)
			reflector.WatchListPageSize = fakeItemsNum

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				err := reflector.list(ctx)
				if err != nil {
					b.Fatalf("reflect list: %v", err)
				}
			}
			b.StopTimer()
		})
	}
}
