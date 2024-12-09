/*
Copyright 2019 The Kubernetes Authors.

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

package events

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/json"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedv1core "k8s.io/client-go/kubernetes/typed/core/v1"
	typedeventsv1 "k8s.io/client-go/kubernetes/typed/events/v1"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/tools/record/util"
	"k8s.io/klog/v2"
	"k8s.io/utils/clock"
)

const (
	maxTriesPerEvent = 12
	finishTime       = 6 * time.Minute
	refreshTime      = 30 * time.Minute
	maxQueuedEvents  = 1000
)

var defaultSleepDuration = 10 * time.Second

// TODO: validate impact of copying and investigate hashing
type eventKey struct {
	eventType           string
	action              string
	reason              string
	reportingController string
	reportingInstance   string
	regarding           corev1.ObjectReference
	related             corev1.ObjectReference
}

type eventBroadcasterImpl struct {
	*watch.Broadcaster
	mu            sync.Mutex
	eventCache    map[eventKey]*eventsv1.Event
	sleepDuration time.Duration
	sink          EventSink
}

// EventSinkImpl wraps EventsV1Interface to implement EventSink.
// TODO: this makes it easier for testing purpose and masks the logic of performing API calls.
// Note that rollbacking to raw clientset should also be transparent.
type EventSinkImpl struct {
	Interface typedeventsv1.EventsV1Interface
}

// Create takes the representation of a event and creates it. Returns the server's representation of the event, and an error, if there is any.
func (e *EventSinkImpl) Create(ctx context.Context, event *eventsv1.Event) (*eventsv1.Event, error) {
	if event.Namespace == "" {
		return nil, fmt.Errorf("can't create an event with empty namespace")
	}
	return e.Interface.Events(event.Namespace).Create(ctx, event, metav1.CreateOptions{})
}

// Update takes the representation of a event and updates it. Returns the server's representation of the event, and an error, if there is any.
func (e *EventSinkImpl) Update(ctx context.Context, event *eventsv1.Event) (*eventsv1.Event, error) {
	if event.Namespace == "" {
		return nil, fmt.Errorf("can't update an event with empty namespace")
	}
	return e.Interface.Events(event.Namespace).Update(ctx, event, metav1.UpdateOptions{})
}

// Patch applies the patch and returns the patched event, and an error, if there is any.
func (e *EventSinkImpl) Patch(ctx context.Context, event *eventsv1.Event, data []byte) (*eventsv1.Event, error) {
	if event.Namespace == "" {
		return nil, fmt.Errorf("can't patch an event with empty namespace")
	}
	return e.Interface.Events(event.Namespace).Patch(ctx, event.Name, types.StrategicMergePatchType, data, metav1.PatchOptions{})
}

// NewBroadcaster Creates a new event broadcaster.
func NewBroadcaster(sink EventSink) EventBroadcaster {
	return newBroadcaster(sink, defaultSleepDuration, map[eventKey]*eventsv1.Event{})
}

// NewBroadcasterForTest Creates a new event broadcaster for test purposes.
func newBroadcaster(sink EventSink, sleepDuration time.Duration, eventCache map[eventKey]*eventsv1.Event) EventBroadcaster {
	return &eventBroadcasterImpl{
		Broadcaster:   watch.NewBroadcaster(maxQueuedEvents, watch.DropIfChannelFull),
		eventCache:    eventCache,
		sleepDuration: sleepDuration,
		sink:          sink,
	}
}

func (e *eventBroadcasterImpl) Shutdown() {
	e.Broadcaster.Shutdown()
}

// refreshExistingEventSeries refresh events TTL
func (e *eventBroadcasterImpl) refreshExistingEventSeries(ctx context.Context) {
	// TODO: Investigate whether lock contention won't be a problem
	e.mu.Lock()
	defer e.mu.Unlock()
	for isomorphicKey, event := range e.eventCache {
		if event.Series != nil {
			if recordedEvent, retry := recordEvent(ctx, e.sink, event); !retry {
				if recordedEvent != nil {
					e.eventCache[isomorphicKey] = recordedEvent
				}
			}
		}
	}
}

// finishSeries checks if a series has ended and either:
// - write final count to the apiserver
// - delete a singleton event (i.e. series field is nil) from the cache
func (e *eventBroadcasterImpl) finishSeries(ctx context.Context) {
	// TODO: Investigate whether lock contention won't be a problem
	e.mu.Lock()
	defer e.mu.Unlock()
	for isomorphicKey, event := range e.eventCache {
		eventSerie := event.Series
		if eventSerie != nil {
			if eventSerie.LastObservedTime.Time.Before(time.Now().Add(-finishTime)) {
				if _, retry := recordEvent(ctx, e.sink, event); !retry {
					delete(e.eventCache, isomorphicKey)
				}
			}
		} else if event.EventTime.Time.Before(time.Now().Add(-finishTime)) {
			delete(e.eventCache, isomorphicKey)
		}
	}
}

// NewRecorder returns an EventRecorder that records events with the given event source.
func (e *eventBroadcasterImpl) NewRecorder(scheme *runtime.Scheme, reportingController string) EventRecorderLogger {
	hostname, _ := os.Hostname()
	reportingInstance := reportingController + "-" + hostname
	return &recorderImplLogger{recorderImpl: &recorderImpl{scheme, reportingController, reportingInstance, e.Broadcaster, clock.RealClock{}}, logger: klog.Background()}
}

func (e *eventBroadcasterImpl) recordToSink(ctx context.Context, event *eventsv1.Event, clock clock.Clock) {
	// Make a copy before modification, because there could be multiple listeners.
	eventCopy := event.DeepCopy()
	go func() {
		evToRecord := func() *eventsv1.Event {
			e.mu.Lock()
			defer e.mu.Unlock()
			eventKey := getKey(eventCopy)
			isomorphicEvent, isIsomorphic := e.eventCache[eventKey]
			if isIsomorphic {
				if isomorphicEvent.Series != nil {
					isomorphicEvent.Series.Count++
					isomorphicEvent.Series.LastObservedTime = metav1.MicroTime{Time: clock.Now()}
					return nil
				}
				isomorphicEvent.Series = &eventsv1.EventSeries{
					Count:            2,
					LastObservedTime: metav1.MicroTime{Time: clock.Now()},
				}
				// Make a copy of the Event to make sure that recording it
				// doesn't mess with the object stored in cache.
				return isomorphicEvent.DeepCopy()
			}
			e.eventCache[eventKey] = eventCopy
			// Make a copy of the Event to make sure that recording it doesn't
			// mess with the object stored in cache.
			return eventCopy.DeepCopy()
		}()
		if evToRecord != nil {
			// TODO: Add a metric counting the number of recording attempts
			e.attemptRecording(ctx, evToRecord)
			// We don't want the new recorded Event to be reflected in the
			// client's cache because server-side mutations could mess with the
			// aggregation mechanism used by the client.
		}
	}()
}

func (e *eventBroadcasterImpl) attemptRecording(ctx context.Context, event *eventsv1.Event) {
	tries := 0
	for {
		if _, retry := recordEvent(ctx, e.sink, event); !retry {
			return
		}
		tries++
		if tries >= maxTriesPerEvent {
			klog.FromContext(ctx).Error(nil, "Unable to write event (retry limit exceeded!)", "event", event)
			return
		}
		// Randomize sleep so that various clients won't all be
		// synced up if the master goes down. Give up when
		// the context is canceled.
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait.Jitter(e.sleepDuration, 0.25)):
		}
	}
}

func recordEvent(ctx context.Context, sink EventSink, event *eventsv1.Event) (*eventsv1.Event, bool) {
	var newEvent *eventsv1.Event
	var err error
	isEventSeries := event.Series != nil
	if isEventSeries {
		patch, patchBytesErr := createPatchBytesForSeries(event)
		if patchBytesErr != nil {
			klog.FromContext(ctx).Error(patchBytesErr, "Unable to calculate diff, no merge is possible")
			return nil, false
		}
		newEvent, err = sink.Patch(ctx, event, patch)
	}
	// Update can fail because the event may have been removed and it no longer exists.
	if !isEventSeries || (isEventSeries && util.IsKeyNotFoundError(err)) {
		// Making sure that ResourceVersion is empty on creation
		event.ResourceVersion = ""
		newEvent, err = sink.Create(ctx, event)
	}
	if err == nil {
		return newEvent, false
	}
	// If we can't contact the server, then hold everything while we keep trying.
	// Otherwise, something about the event is malformed and we should abandon it.
	switch err.(type) {
	case *restclient.RequestConstructionError:
		// We will construct the request the same next time, so don't keep trying.
		klog.FromContext(ctx).Error(err, "Unable to construct event (will not retry!)", "event", event)
		return nil, false
	case *errors.StatusError:
		if errors.IsAlreadyExists(err) {
			// If we tried to create an Event from an EventSerie, it means that
			// the original Patch request failed because the Event we were
			// trying to patch didn't exist. If the creation failed because the
			// Event now exists, it is safe to retry.  This occurs when a new
			// Event is emitted twice in a very short period of time.
			if isEventSeries {
				return nil, true
			}
			klog.FromContext(ctx).V(5).Info("Server rejected event (will not retry!)", "event", event, "err", err)
		} else {
			klog.FromContext(ctx).Error(err, "Server rejected event (will not retry!)", "event", event)
		}
		return nil, false
	case *errors.UnexpectedObjectError:
		// We don't expect this; it implies the server's response didn't match a
		// known pattern. Go ahead and retry.
	default:
		// This case includes actual http transport errors. Go ahead and retry.
	}
	klog.FromContext(ctx).Error(err, "Unable to write event (may retry after sleeping)")
	return nil, true
}

func createPatchBytesForSeries(event *eventsv1.Event) ([]byte, error) {
	oldEvent := event.DeepCopy()
	oldEvent.Series = nil
	oldData, err := json.Marshal(oldEvent)
	if err != nil {
		return nil, err
	}
	newData, err := json.Marshal(event)
	if err != nil {
		return nil, err
	}
	return strategicpatch.CreateTwoWayMergePatch(oldData, newData, eventsv1.Event{})
}

func getKey(event *eventsv1.Event) eventKey {
	key := eventKey{
		eventType:           event.Type,
		action:              event.Action,
		reason:              event.Reason,
		reportingController: event.ReportingController,
		reportingInstance:   event.ReportingInstance,
		regarding:           event.Regarding,
	}
	if event.Related != nil {
		key.related = *event.Related
	}
	return key
}

// StartStructuredLogging starts sending events received from this EventBroadcaster to the structured logging function.
// The return value can be ignored or used to stop recording, if desired.
// TODO: this function should also return an error.
//
// Deprecated: use StartLogging instead.
func (e *eventBroadcasterImpl) StartStructuredLogging(verbosity klog.Level) func() {
	logger := klog.Background().V(int(verbosity))
	stopWatcher, err := e.StartLogging(logger)
	if err != nil {
		logger.Error(err, "Failed to start event watcher")
		return func() {}
	}
	return stopWatcher
}

// StartLogging starts sending events received from this EventBroadcaster to the structured logger.
// To adjust verbosity, use the logger's V method (i.e. pass `logger.V(3)` instead of `logger`).
// The returned function can be ignored or used to stop recording, if desired.
func (e *eventBroadcasterImpl) StartLogging(logger klog.Logger) (func(), error) {
	return e.startEventWatcher(logger,
		func(obj runtime.Object) {
			event, ok := obj.(*eventsv1.Event)
			if !ok {
				logger.Error(nil, "unexpected type, expected eventsv1.Event")
				return
			}
			logger.Info("Event occurred", "object", klog.KRef(event.Regarding.Namespace, event.Regarding.Name), "kind", event.Regarding.Kind, "apiVersion", event.Regarding.APIVersion, "type", event.Type, "reason", event.Reason, "action", event.Action, "note", event.Note)
		})
}

// StartEventWatcher starts sending events received from this EventBroadcaster to the given event handler function.
// The return value is used to stop recording
func (e *eventBroadcasterImpl) StartEventWatcher(eventHandler func(event runtime.Object)) (func(), error) {
	return e.startEventWatcher(klog.Background(), eventHandler)
}

func (e *eventBroadcasterImpl) startEventWatcher(logger klog.Logger, eventHandler func(event runtime.Object)) (func(), error) {
	watcher, err := e.Watch()
	if err != nil {
		return nil, err
	}
	go func() {
		defer utilruntime.HandleCrashWithLogger(logger)
		for {
			watchEvent, ok := <-watcher.ResultChan()
			if !ok {
				return
			}
			eventHandler(watchEvent.Object)
		}
	}()
	return watcher.Stop, nil
}

func (e *eventBroadcasterImpl) startRecordingEvents(ctx context.Context) error {
	eventHandler := func(obj runtime.Object) {
		event, ok := obj.(*eventsv1.Event)
		if !ok {
			klog.FromContext(ctx).Error(nil, "unexpected type, expected eventsv1.Event")
			return
		}
		e.recordToSink(ctx, event, clock.RealClock{})
	}
	stopWatcher, err := e.StartEventWatcher(eventHandler)
	if err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		stopWatcher()
	}()
	return nil
}

// StartRecordingToSink starts sending events received from the specified eventBroadcaster to the given sink.
// Deprecated: use StartRecordingToSinkWithContext instead.
func (e *eventBroadcasterImpl) StartRecordingToSink(stopCh <-chan struct{}) {
	err := e.StartRecordingToSinkWithContext(wait.ContextForChannel(stopCh))
	if err != nil {
		klog.Background().Error(err, "Failed to start recording to sink")
	}
}

// StartRecordingToSinkWithContext starts sending events received from the specified eventBroadcaster to the given sink.
func (e *eventBroadcasterImpl) StartRecordingToSinkWithContext(ctx context.Context) error {
	go wait.UntilWithContext(ctx, e.refreshExistingEventSeries, refreshTime)
	go wait.UntilWithContext(ctx, e.finishSeries, finishTime)
	return e.startRecordingEvents(ctx)
}

type eventBroadcasterAdapterImpl struct {
	coreClient          typedv1core.EventsGetter
	coreBroadcaster     record.EventBroadcaster
	eventsv1Client      typedeventsv1.EventsV1Interface
	eventsv1Broadcaster EventBroadcaster
}

// NewEventBroadcasterAdapter creates a wrapper around new and legacy broadcasters to simplify
// migration of individual components to the new Event API.
//
//logcheck:context // NewEventBroadcasterAdapterWithContext should be used instead because record.NewBroadcaster is called and works better when a context is supplied (contextual logging, cancellation).
func NewEventBroadcasterAdapter(client clientset.Interface) EventBroadcasterAdapter {
	return NewEventBroadcasterAdapterWithContext(context.Background(), client)
}

// NewEventBroadcasterAdapterWithContext creates a wrapper around new and legacy broadcasters to simplify
// migration of individual components to the new Event API.
func NewEventBroadcasterAdapterWithContext(ctx context.Context, client clientset.Interface) EventBroadcasterAdapter {
	eventClient := &eventBroadcasterAdapterImpl{}
	if _, err := client.Discovery().ServerResourcesForGroupVersion(eventsv1.SchemeGroupVersion.String()); err == nil {
		eventClient.eventsv1Client = client.EventsV1()
		eventClient.eventsv1Broadcaster = NewBroadcaster(&EventSinkImpl{Interface: eventClient.eventsv1Client})
	}
	// Even though there can soon exist cases when coreBroadcaster won't really be needed,
	// we create it unconditionally because its overhead is minor and will simplify using usage
	// patterns of this library in all components.
	eventClient.coreClient = client.CoreV1()
	eventClient.coreBroadcaster = record.NewBroadcaster(record.WithContext(ctx))
	return eventClient
}

// StartRecordingToSink starts sending events received from the specified eventBroadcaster to the given sink.
func (e *eventBroadcasterAdapterImpl) StartRecordingToSink(stopCh <-chan struct{}) {
	if e.eventsv1Broadcaster != nil && e.eventsv1Client != nil {
		e.eventsv1Broadcaster.StartRecordingToSink(stopCh)
	}
	if e.coreBroadcaster != nil && e.coreClient != nil {
		e.coreBroadcaster.StartRecordingToSink(&typedv1core.EventSinkImpl{Interface: e.coreClient.Events("")})
	}
}

func (e *eventBroadcasterAdapterImpl) NewRecorder(name string) EventRecorderLogger {
	if e.eventsv1Broadcaster != nil && e.eventsv1Client != nil {
		return e.eventsv1Broadcaster.NewRecorder(scheme.Scheme, name)
	}
	return record.NewEventRecorderAdapter(e.DeprecatedNewLegacyRecorder(name))
}

func (e *eventBroadcasterAdapterImpl) DeprecatedNewLegacyRecorder(name string) record.EventRecorderLogger {
	return e.coreBroadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: name})
}

func (e *eventBroadcasterAdapterImpl) Shutdown() {
	if e.coreBroadcaster != nil {
		e.coreBroadcaster.Shutdown()
	}
	if e.eventsv1Broadcaster != nil {
		e.eventsv1Broadcaster.Shutdown()
	}
}
