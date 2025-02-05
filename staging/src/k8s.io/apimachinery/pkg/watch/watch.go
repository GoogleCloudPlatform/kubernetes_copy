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

package watch

import (
	"fmt"
	"sync"

	"k8s.io/klog/v2"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
)

// Interface can be implemented by anything that knows how to watch and report changes.
type Interface interface {
	// Stop tells the producer that the consumer is done watching, so the
	// producer should stop sending events and close the result channel. The
	// consumer should keep watching for events until the result channel is
	// closed.
	//
	// Because some implementations may create channels when constructed, Stop
	// must always be called, even if the consumer has not yet called
	// ResultChan().
	//
	// Only the consumer should call Stop(), not the producer. If the producer
	// errors and needs to stop the watch prematurely, it should instead send
	// an error event and close the result channel.
	Stop()

	// ResultChan returns a channel which will receive events from the event
	// producer. If an error occurs or Stop() is called, the producer must
	// close this channel and release any resources used by the watch.
	// Closing the result channel tells the consumer that no more events will be
	// sent.
	ResultChan() <-chan Event
}

// EventType defines the possible types of events.
type EventType string

const (
	Added    EventType = "ADDED"
	Modified EventType = "MODIFIED"
	Deleted  EventType = "DELETED"
	Bookmark EventType = "BOOKMARK"
	Error    EventType = "ERROR"
)

var (
	DefaultChanSize int32 = 100
)

// Event represents a single event to a watched resource.
// +k8s:deepcopy-gen=true
type Event struct {
	Type EventType

	// Object is:
	//  * If Type is Added or Modified: the new state of the object.
	//  * If Type is Deleted: the state of the object immediately before deletion.
	//  * If Type is Bookmark: the object (instance of a type being watched) where
	//    only ResourceVersion field is set. On successful restart of watch from a
	//    bookmark resourceVersion, client is guaranteed to not get repeat event
	//    nor miss any events.
	//  * If Type is Error: *api.Status is recommended; other types may make sense
	//    depending on context.
	Object runtime.Object
}

type emptyWatch chan Event

// NewEmptyWatch returns a watch interface that returns no results and is closed.
// May be used in certain error conditions where no information is available but
// an error is not warranted.
func NewEmptyWatch() Interface {
	ch := make(chan Event)
	close(ch)
	return emptyWatch(ch)
}

// Stop implements Interface
func (w emptyWatch) Stop() {
}

// ResultChan implements Interface
func (w emptyWatch) ResultChan() <-chan Event {
	return chan Event(w)
}

// FakeWatcher lets you test anything that consumes a watch.Interface; threadsafe.
type FakeWatcher struct {
	logger  klog.Logger
	result  chan Event
	stopped bool
	sync.Mutex
}

var _ Interface = &FakeWatcher{}

// Contextual logging: NewFakeWithOptions and a logger in the FakeOptions should be used instead in code which supports contextual logging.
func NewFake() *FakeWatcher {
	return NewFakeWithOptions(FakeOptions{})
}

// Contextual logging: NewFakeWithOptions and a logger in the FakeOptions should be used instead in code which supports contextual logging.
func NewFakeWithChanSize(size int, blocking bool) *FakeWatcher {
	return NewFakeWithOptions(FakeOptions{ChannelSize: size})
}

func NewFakeWithOptions(options FakeOptions) *FakeWatcher {
	return &FakeWatcher{
		logger: ptr.Deref(options.Logger, klog.Background()),
		result: make(chan Event, options.ChannelSize),
	}
}

type FakeOptions struct {
	Logger      *klog.Logger
	ChannelSize int
}

// Stop implements Interface.Stop().
func (f *FakeWatcher) Stop() {
	f.Lock()
	defer f.Unlock()
	if !f.stopped {
		f.logger.V(4).Info("Stopping fake watcher")
		close(f.result)
		f.stopped = true
	}
}

func (f *FakeWatcher) IsStopped() bool {
	f.Lock()
	defer f.Unlock()
	return f.stopped
}

// Reset prepares the watcher to be reused.
func (f *FakeWatcher) Reset() {
	f.Lock()
	defer f.Unlock()
	f.stopped = false
	f.result = make(chan Event)
}

func (f *FakeWatcher) ResultChan() <-chan Event {
	return f.result
}

// Add sends an add event.
func (f *FakeWatcher) Add(obj runtime.Object) {
	f.result <- Event{Added, obj}
}

// Modify sends a modify event.
func (f *FakeWatcher) Modify(obj runtime.Object) {
	f.result <- Event{Modified, obj}
}

// Delete sends a delete event.
func (f *FakeWatcher) Delete(lastValue runtime.Object) {
	f.result <- Event{Deleted, lastValue}
}

// Error sends an Error event.
func (f *FakeWatcher) Error(errValue runtime.Object) {
	f.result <- Event{Error, errValue}
}

// Action sends an event of the requested type, for table-based testing.
func (f *FakeWatcher) Action(action EventType, obj runtime.Object) {
	f.result <- Event{action, obj}
}

// RaceFreeFakeWatcher lets you test anything that consumes a watch.Interface; threadsafe.
type RaceFreeFakeWatcher struct {
	logger  klog.Logger
	result  chan Event
	Stopped bool
	sync.Mutex
}

var _ Interface = &RaceFreeFakeWatcher{}

// Contextual logging: RaceFreeFakeWatcherWithLogger should be used instead of NewRaceFreeFake in code which supports contextual logging.
func NewRaceFreeFake() *RaceFreeFakeWatcher {
	return NewRaceFreeFakeWithLogger(klog.Background())
}

func NewRaceFreeFakeWithLogger(logger klog.Logger) *RaceFreeFakeWatcher {
	return &RaceFreeFakeWatcher{
		logger: logger,
		result: make(chan Event, DefaultChanSize),
	}
}

// Stop implements Interface.Stop().
func (f *RaceFreeFakeWatcher) Stop() {
	f.Lock()
	defer f.Unlock()
	if !f.Stopped {
		f.logger.V(4).Info("Stopping fake watcher")
		close(f.result)
		f.Stopped = true
	}
}

func (f *RaceFreeFakeWatcher) IsStopped() bool {
	f.Lock()
	defer f.Unlock()
	return f.Stopped
}

// Reset prepares the watcher to be reused.
func (f *RaceFreeFakeWatcher) Reset() {
	f.Lock()
	defer f.Unlock()
	f.Stopped = false
	f.result = make(chan Event, DefaultChanSize)
}

func (f *RaceFreeFakeWatcher) ResultChan() <-chan Event {
	f.Lock()
	defer f.Unlock()
	return f.result
}

// Add sends an add event.
func (f *RaceFreeFakeWatcher) Add(obj runtime.Object) {
	f.Lock()
	defer f.Unlock()
	if !f.Stopped {
		select {
		case f.result <- Event{Added, obj}:
			return
		default:
			panic(fmt.Errorf("channel full"))
		}
	}
}

// Modify sends a modify event.
func (f *RaceFreeFakeWatcher) Modify(obj runtime.Object) {
	f.Lock()
	defer f.Unlock()
	if !f.Stopped {
		select {
		case f.result <- Event{Modified, obj}:
			return
		default:
			panic(fmt.Errorf("channel full"))
		}
	}
}

// Delete sends a delete event.
func (f *RaceFreeFakeWatcher) Delete(lastValue runtime.Object) {
	f.Lock()
	defer f.Unlock()
	if !f.Stopped {
		select {
		case f.result <- Event{Deleted, lastValue}:
			return
		default:
			panic(fmt.Errorf("channel full"))
		}
	}
}

// Error sends an Error event.
func (f *RaceFreeFakeWatcher) Error(errValue runtime.Object) {
	f.Lock()
	defer f.Unlock()
	if !f.Stopped {
		select {
		case f.result <- Event{Error, errValue}:
			return
		default:
			panic(fmt.Errorf("channel full"))
		}
	}
}

// Action sends an event of the requested type, for table-based testing.
func (f *RaceFreeFakeWatcher) Action(action EventType, obj runtime.Object) {
	f.Lock()
	defer f.Unlock()
	if !f.Stopped {
		select {
		case f.result <- Event{action, obj}:
			return
		default:
			panic(fmt.Errorf("channel full"))
		}
	}
}

// ProxyWatcher lets you wrap your channel in watch Interface. threadsafe.
type ProxyWatcher struct {
	result chan Event
	stopCh chan struct{}

	mutex   sync.Mutex
	stopped bool
}

var _ Interface = &ProxyWatcher{}

// NewProxyWatcher creates new ProxyWatcher by wrapping a channel
func NewProxyWatcher(ch chan Event) *ProxyWatcher {
	return &ProxyWatcher{
		result:  ch,
		stopCh:  make(chan struct{}),
		stopped: false,
	}
}

// Stop implements Interface
func (pw *ProxyWatcher) Stop() {
	pw.mutex.Lock()
	defer pw.mutex.Unlock()
	if !pw.stopped {
		pw.stopped = true
		close(pw.stopCh)
	}
}

// Stopping returns true if Stop() has been called
func (pw *ProxyWatcher) Stopping() bool {
	pw.mutex.Lock()
	defer pw.mutex.Unlock()
	return pw.stopped
}

// ResultChan implements Interface
func (pw *ProxyWatcher) ResultChan() <-chan Event {
	return pw.result
}

// StopChan returns stop channel
func (pw *ProxyWatcher) StopChan() <-chan struct{} {
	return pw.stopCh
}

// MockWatcher implements watch.Interface with mockable functions.
type MockWatcher struct {
	StopFunc       func()
	ResultChanFunc func() <-chan Event
}

var _ Interface = &MockWatcher{}

// Stop calls StopFunc
func (mw MockWatcher) Stop() {
	mw.StopFunc()
}

// ResultChan calls ResultChanFunc
func (mw MockWatcher) ResultChan() <-chan Event {
	return mw.ResultChanFunc()
}
