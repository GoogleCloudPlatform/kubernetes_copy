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

package record

import (
	"context"
	"fmt"
	"math/rand"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/watch"
	restclient "k8s.io/client-go/rest"
	internalevents "k8s.io/client-go/tools/internal/events"
	"k8s.io/client-go/tools/record/util"
	ref "k8s.io/client-go/tools/reference"
	"k8s.io/klog/v2"
	"k8s.io/utils/clock"
)

const maxTriesPerEvent = 12

var defaultSleepDuration = 10 * time.Second

const maxQueuedEvents = 1000

// EventSink knows how to store events (client.Client implements it.)
// EventSink must respect the namespace that will be embedded in 'event'.
// It is assumed that EventSink will return the same sorts of errors as
// pkg/client's REST client.
type EventSink interface {
	Create(event *v1.Event) (*v1.Event, error)
	Update(event *v1.Event) (*v1.Event, error)
	Patch(oldEvent *v1.Event, data []byte) (*v1.Event, error)
}

// CorrelatorOptions allows you to change the default of the EventSourceObjectSpamFilter
// and EventAggregator in EventCorrelator
type CorrelatorOptions struct {
	// The lru cache size used for both EventSourceObjectSpamFilter and the EventAggregator
	// If not specified (zero value), the default specified in events_cache.go will be picked
	// This means that the LRUCacheSize has to be greater than 0.
	LRUCacheSize int
	// The burst size used by the token bucket rate filtering in EventSourceObjectSpamFilter
	// If not specified (zero value), the default specified in events_cache.go will be picked
	// This means that the BurstSize has to be greater than 0.
	BurstSize int
	// The fill rate of the token bucket in queries per second in EventSourceObjectSpamFilter
	// If not specified (zero value), the default specified in events_cache.go will be picked
	// This means that the QPS has to be greater than 0.
	QPS float32
	// The func used by the EventAggregator to group event keys for aggregation
	// If not specified (zero value), EventAggregatorByReasonFunc will be used
	KeyFunc EventAggregatorKeyFunc
	// The func used by the EventAggregator to produced aggregated message
	// If not specified (zero value), EventAggregatorByReasonMessageFunc will be used
	MessageFunc EventAggregatorMessageFunc
	// The number of events in an interval before aggregation happens by the EventAggregator
	// If not specified (zero value), the default specified in events_cache.go will be picked
	// This means that the MaxEvents has to be greater than 0
	MaxEvents int
	// The amount of time in seconds that must transpire since the last occurrence of a similar event before it is considered new by the EventAggregator
	// If not specified (zero value), the default specified in events_cache.go will be picked
	// This means that the MaxIntervalInSeconds has to be greater than 0
	MaxIntervalInSeconds int
	// The clock used by the EventAggregator to allow for testing
	// If not specified (zero value), clock.RealClock{} will be used
	Clock clock.PassiveClock
	// The func used by EventFilterFunc, which returns a key for given event, based on which filtering will take place
	// If not specified (zero value), getSpamKey will be used
	SpamKeyFunc EventSpamKeyFunc
}

// EventRecorder knows how to record events on behalf of an EventSource.
type EventRecorder interface {
	// Event constructs an event from the given information and puts it in the queue for sending.
	// 'object' is the object this event is about. Event will make a reference-- or you may also
	// pass a reference to the object directly.
	// 'eventtype' of this event, and can be one of Normal, Warning. New types could be added in future
	// 'reason' is the reason this event is generated. 'reason' should be short and unique; it
	// should be in UpperCamelCase format (starting with a capital letter). "reason" will be used
	// to automate handling of events, so imagine people writing switch statements to handle them.
	// You want to make that easy.
	// 'message' is intended to be human readable.
	//
	// The resulting event will be created in the same namespace as the reference object.
	Event(object runtime.Object, eventtype, reason, message string)

	// Eventf is just like Event, but with Sprintf for the message field.
	Eventf(object runtime.Object, eventtype, reason, messageFmt string, args ...interface{})

	// AnnotatedEventf is just like eventf, but with annotations attached
	AnnotatedEventf(object runtime.Object, annotations map[string]string, eventtype, reason, messageFmt string, args ...interface{})
}

// EventRecorderLogger extends EventRecorder such that a logger can
// be set for methods in EventRecorder. Normally, those methods
// uses the global default logger to record errors and debug messages.
// If that is not desired, use WithLogger to provide a logger instance.
type EventRecorderLogger interface {
	EventRecorder

	// WithLogger replaces the context used for logging. This is a cheap call
	// and meant to be used for contextual logging:
	//    recorder := ...
	//    logger := klog.FromContext(ctx)
	//    recorder.WithLogger(logger).Eventf(...)
	WithLogger(logger klog.Logger) EventRecorderLogger
}

// EventBroadcaster knows how to receive events and send them to any EventSink, watcher, or log.
type EventBroadcaster interface {
	// StartEventWatcher starts sending events received from this EventBroadcaster to the given
	// event handler function. The return value can be ignored or used to stop recording, if
	// desired.
	StartEventWatcher(eventHandler func(*v1.Event)) watch.Interface

	// StartRecordingToSink starts sending events received from this EventBroadcaster to the given
	// sink. The return value can be ignored or used to stop recording, if desired.
	StartRecordingToSink(sink EventSink) watch.Interface

	// StartLogging starts sending events received from this EventBroadcaster to the given logging
	// function. The return value can be ignored or used to stop recording, if desired.
	StartLogging(logf func(format string, args ...interface{})) watch.Interface

	// StartStructuredLogging starts sending events received from this EventBroadcaster to the structured
	// logging function. The return value can be ignored or used to stop recording, if desired.
	StartStructuredLogging(verbosity klog.Level) watch.Interface

	// NewRecorder returns an EventRecorder that can be used to send events to this EventBroadcaster
	// with the event source set to the given event source.
	NewRecorder(scheme *runtime.Scheme, source v1.EventSource) EventRecorderLogger

	// Shutdown shuts down the broadcaster. Once the broadcaster is shut
	// down, it will only try to record an event in a sink once before
	// giving up on it with an error message.
	Shutdown()
}

// EventRecorderAdapter is a wrapper around a "k8s.io/client-go/tools/record".EventRecorder
// implementing the new "k8s.io/client-go/tools/events".EventRecorder interface.
type EventRecorderAdapter struct {
	recorder EventRecorderLogger
}

var _ internalevents.EventRecorder = &EventRecorderAdapter{}

// NewEventRecorderAdapter returns an adapter implementing the new
// "k8s.io/client-go/tools/events".EventRecorder interface.
func NewEventRecorderAdapter(recorder EventRecorderLogger) *EventRecorderAdapter {
	return &EventRecorderAdapter{
		recorder: recorder,
	}
}

// Eventf is a wrapper around v1 Eventf
func (a *EventRecorderAdapter) Eventf(regarding, _ runtime.Object, eventtype, reason, action, note string, args ...interface{}) {
	a.recorder.Eventf(regarding, eventtype, reason, note, args...)
}

func (a *EventRecorderAdapter) WithLogger(logger klog.Logger) internalevents.EventRecorderLogger {
	return &EventRecorderAdapter{
		recorder: a.recorder.WithLogger(logger),
	}
}

// Creates a new event broadcaster.
func NewBroadcaster(opts ...BroadcasterOption) EventBroadcaster {
	c := config{
		sleepDuration: defaultSleepDuration,
	}
	for _, opt := range opts {
		opt(&c)
	}
	eventBroadcaster := &eventBroadcasterImpl{
		Broadcaster:   watch.NewLongQueueBroadcaster(maxQueuedEvents, watch.DropIfChannelFull),
		sleepDuration: c.sleepDuration,
		options:       c.CorrelatorOptions,
	}
	ctx := c.Context
	if ctx == nil {
		ctx = context.Background()
	}
	// The are two scenarios where it makes no sense to wait for context cancelation:
	// - The context was nil.
	// - The context was context.Background() to begin with.
	//
	// Both cases get checked here: we have cancelation if (and only if) there is a channel.
	haveCtxCancelation := ctx.Done() != nil

	eventBroadcaster.cancelationCtx, eventBroadcaster.cancel = context.WithCancel(ctx)

	if haveCtxCancelation {
		// Calling Shutdown is not required when a context was provided:
		// when the context is canceled, this goroutine will shut down
		// the broadcaster.
		//
		// If Shutdown is called first, then this goroutine will
		// also stop.
		go func() {
			<-eventBroadcaster.cancelationCtx.Done()
			eventBroadcaster.Broadcaster.Shutdown()
		}()
	}

	return eventBroadcaster
}

func NewBroadcasterForTests(sleepDuration time.Duration) EventBroadcaster {
	return NewBroadcaster(WithSleepDuration(sleepDuration))
}

func NewBroadcasterWithCorrelatorOptions(options CorrelatorOptions) EventBroadcaster {
	return NewBroadcaster(WithCorrelatorOptions(options))
}

func WithCorrelatorOptions(options CorrelatorOptions) BroadcasterOption {
	return func(c *config) {
		c.CorrelatorOptions = options
	}
}

// WithContext sets a context for the broadcaster. Canceling the context will
// shut down the broadcaster, Shutdown doesn't need to be called. The context
// can also be used to provide a logger.
func WithContext(ctx context.Context) BroadcasterOption {
	return func(c *config) {
		c.Context = ctx
	}
}

func WithSleepDuration(sleepDuration time.Duration) BroadcasterOption {
	return func(c *config) {
		c.sleepDuration = sleepDuration
	}
}

type BroadcasterOption func(*config)

type config struct {
	CorrelatorOptions
	context.Context
	sleepDuration time.Duration
}

type eventBroadcasterImpl struct {
	*watch.Broadcaster
	sleepDuration  time.Duration
	options        CorrelatorOptions
	cancelationCtx context.Context
	cancel         func()
}

// StartRecordingToSink starts sending events received from the specified eventBroadcaster to the given sink.
// The return value can be ignored or used to stop recording, if desired.
// TODO: make me an object with parameterizable queue length and retry interval
func (e *eventBroadcasterImpl) StartRecordingToSink(sink EventSink) watch.Interface {
	eventCorrelator := NewEventCorrelatorWithOptions(e.options)
	return e.StartEventWatcher(
		func(event *v1.Event) {
			e.recordToSink(sink, event, eventCorrelator)
		})
}

func (e *eventBroadcasterImpl) Shutdown() {
	e.Broadcaster.Shutdown()
	e.cancel()
}

func (e *eventBroadcasterImpl) recordToSink(sink EventSink, event *v1.Event, eventCorrelator *EventCorrelator) {
	// Make a copy before modification, because there could be multiple listeners.
	// Events are safe to copy like this.
	eventCopy := *event
	event = &eventCopy
	result, err := eventCorrelator.EventCorrelate(event)
	if err != nil {
		utilruntime.HandleError(err)
	}
	if result.Skip {
		return
	}
	tries := 0
	for {
		if recordEvent(e.cancelationCtx, sink, result.Event, result.Patch, result.Event.Count > 1, eventCorrelator) {
			break
		}
		tries++
		if tries >= maxTriesPerEvent {
			klog.FromContext(e.cancelationCtx).Error(nil, "Unable to write event (retry limit exceeded!)", "event", event)
			break
		}

		// Randomize the first sleep so that various clients won't all be
		// synced up if the master goes down.
		delay := e.sleepDuration
		if tries == 1 {
			delay = time.Duration(float64(delay) * rand.Float64())
		}
		select {
		case <-e.cancelationCtx.Done():
			klog.FromContext(e.cancelationCtx).Error(nil, "Unable to write event (broadcaster is shut down)", "event", event)
			return
		case <-time.After(delay):
		}
	}
}

// recordEvent attempts to write event to a sink. It returns true if the event
// was successfully recorded or discarded, false if it should be retried.
// If updateExistingEvent is false, it creates a new event, otherwise it updates
// existing event.
func recordEvent(ctx context.Context, sink EventSink, event *v1.Event, patch []byte, updateExistingEvent bool, eventCorrelator *EventCorrelator) bool {
	var newEvent *v1.Event
	var err error
	if updateExistingEvent {
		newEvent, err = sink.Patch(event, patch)
	}
	// Update can fail because the event may have been removed and it no longer exists.
	if !updateExistingEvent || (updateExistingEvent && util.IsKeyNotFoundError(err)) {
		// Making sure that ResourceVersion is empty on creation
		event.ResourceVersion = ""
		newEvent, err = sink.Create(event)
	}
	if err == nil {
		// we need to update our event correlator with the server returned state to handle name/resourceversion
		eventCorrelator.UpdateState(newEvent)
		return true
	}

	// If we can't contact the server, then hold everything while we keep trying.
	// Otherwise, something about the event is malformed and we should abandon it.
	switch err.(type) {
	case *restclient.RequestConstructionError:
		// We will construct the request the same next time, so don't keep trying.
		klog.FromContext(ctx).Error(err, "Unable to construct event (will not retry!)", "event", event)
		return true
	case *errors.StatusError:
		if errors.IsAlreadyExists(err) || errors.HasStatusCause(err, v1.NamespaceTerminatingCause) {
			klog.FromContext(ctx).V(5).Info("Server rejected event (will not retry!)", "event", event, "err", err)
		} else {
			klog.FromContext(ctx).Error(err, "Server rejected event (will not retry!)", "event", event)
		}
		return true
	case *errors.UnexpectedObjectError:
		// We don't expect this; it implies the server's response didn't match a
		// known pattern. Go ahead and retry.
	default:
		// This case includes actual http transport errors. Go ahead and retry.
	}
	klog.FromContext(ctx).Error(err, "Unable to write event (may retry after sleeping)", "event", event)
	return false
}

// StartLogging starts sending events received from this EventBroadcaster to the given logging function.
// The return value can be ignored or used to stop recording, if desired.
func (e *eventBroadcasterImpl) StartLogging(logf func(format string, args ...interface{})) watch.Interface {
	return e.StartEventWatcher(
		func(e *v1.Event) {
			logf("Event(%#v): type: '%v' reason: '%v' %v", e.InvolvedObject, e.Type, e.Reason, e.Message)
		})
}

// StartStructuredLogging starts sending events received from this EventBroadcaster to a structured logger.
// The logger is retrieved from a context if the broadcaster was constructed with a context, otherwise
// the global default is used.
// The return value can be ignored or used to stop recording, if desired.
func (e *eventBroadcasterImpl) StartStructuredLogging(verbosity klog.Level) watch.Interface {
	loggerV := klog.FromContext(e.cancelationCtx).V(int(verbosity))
	return e.StartEventWatcher(
		func(e *v1.Event) {
			loggerV.Info("Event occurred", "object", klog.KRef(e.InvolvedObject.Namespace, e.InvolvedObject.Name), "fieldPath", e.InvolvedObject.FieldPath, "kind", e.InvolvedObject.Kind, "apiVersion", e.InvolvedObject.APIVersion, "type", e.Type, "reason", e.Reason, "message", e.Message)
		})
}

// StartEventWatcher starts sending events received from this EventBroadcaster to the given event handler function.
// The return value can be ignored or used to stop recording, if desired.
func (e *eventBroadcasterImpl) StartEventWatcher(eventHandler func(*v1.Event)) watch.Interface {
	watcher, err := e.Watch()
	if err != nil {
		// This function traditionally returns no error even though it can fail.
		// Instead, it logs the error and returns an empty watch. The empty
		// watch ensures that callers don't crash when calling Stop.
		klog.FromContext(e.cancelationCtx).Error(err, "Unable start event watcher (will not retry!)")
		return watch.NewEmptyWatch()
	}
	go func() {
		defer utilruntime.HandleCrash()
		for {
			select {
			case <-e.cancelationCtx.Done():
				watcher.Stop()
				return
			case watchEvent := <-watcher.ResultChan():
				event, ok := watchEvent.Object.(*v1.Event)
				if !ok {
					// This is all local, so there's no reason this should
					// ever happen.
					continue
				}
				eventHandler(event)
			}
		}
	}()
	return watcher
}

// NewRecorder returns an EventRecorder that records events with the given event source.
func (e *eventBroadcasterImpl) NewRecorder(scheme *runtime.Scheme, source v1.EventSource) EventRecorderLogger {
	return &recorderImplLogger{recorderImpl: &recorderImpl{scheme, source, e.Broadcaster, clock.RealClock{}}, logger: klog.Background()}
}

type recorderImpl struct {
	scheme *runtime.Scheme
	source v1.EventSource
	*watch.Broadcaster
	clock clock.PassiveClock
}

var _ EventRecorder = &recorderImpl{}

func (recorder *recorderImpl) generateEvent(logger klog.Logger, object runtime.Object, annotations map[string]string, eventtype, reason, message string) {
	ref, err := ref.GetReference(recorder.scheme, object)
	if err != nil {
		logger.Error(err, "Could not construct reference, will not report event", "object", object, "eventType", eventtype, "reason", reason, "message", message)
		return
	}

	if !util.ValidateEventType(eventtype) {
		logger.Error(nil, "Unsupported event type", "eventType", eventtype)
		return
	}

	event := recorder.makeEvent(ref, annotations, eventtype, reason, message)
	event.Source = recorder.source

	event.ReportingInstance = recorder.source.Host
	event.ReportingController = recorder.source.Component

	// NOTE: events should be a non-blocking operation, but we also need to not
	// put this in a goroutine, otherwise we'll race to write to a closed channel
	// when we go to shut down this broadcaster.  Just drop events if we get overloaded,
	// and log an error if that happens (we've configured the broadcaster to drop
	// outgoing events anyway).
	sent, err := recorder.ActionOrDrop(watch.Added, event)
	if err != nil {
		logger.Error(err, "Unable to record event (will not retry!)")
		return
	}
	if !sent {
		logger.Error(nil, "Unable to record event: too many queued events, dropped event", "event", event)
	}
}

func (recorder *recorderImpl) Event(object runtime.Object, eventtype, reason, message string) {
	recorder.generateEvent(klog.Background(), object, nil, eventtype, reason, message)
}

func (recorder *recorderImpl) Eventf(object runtime.Object, eventtype, reason, messageFmt string, args ...interface{}) {
	recorder.Event(object, eventtype, reason, fmt.Sprintf(messageFmt, args...))
}

func (recorder *recorderImpl) AnnotatedEventf(object runtime.Object, annotations map[string]string, eventtype, reason, messageFmt string, args ...interface{}) {
	recorder.generateEvent(klog.Background(), object, annotations, eventtype, reason, fmt.Sprintf(messageFmt, args...))
}

func (recorder *recorderImpl) makeEvent(ref *v1.ObjectReference, annotations map[string]string, eventtype, reason, message string) *v1.Event {
	t := metav1.Time{Time: recorder.clock.Now()}
	namespace := ref.Namespace
	if namespace == "" {
		namespace = metav1.NamespaceDefault
	}
	return &v1.Event{
		ObjectMeta: metav1.ObjectMeta{
			Name:        util.GenerateEventName(ref.Name, t.UnixNano()),
			Namespace:   namespace,
			Annotations: annotations,
		},
		InvolvedObject: *ref,
		Reason:         reason,
		Message:        message,
		FirstTimestamp: t,
		LastTimestamp:  t,
		Count:          1,
		Type:           eventtype,
	}
}

type recorderImplLogger struct {
	*recorderImpl
	logger klog.Logger
}

var _ EventRecorderLogger = &recorderImplLogger{}

func (recorder recorderImplLogger) Event(object runtime.Object, eventtype, reason, message string) {
	recorder.recorderImpl.generateEvent(recorder.logger, object, nil, eventtype, reason, message)
}

func (recorder recorderImplLogger) Eventf(object runtime.Object, eventtype, reason, messageFmt string, args ...interface{}) {
	recorder.Event(object, eventtype, reason, fmt.Sprintf(messageFmt, args...))
}

func (recorder recorderImplLogger) AnnotatedEventf(object runtime.Object, annotations map[string]string, eventtype, reason, messageFmt string, args ...interface{}) {
	recorder.generateEvent(recorder.logger, object, annotations, eventtype, reason, fmt.Sprintf(messageFmt, args...))
}

func (recorder recorderImplLogger) WithLogger(logger klog.Logger) EventRecorderLogger {
	return recorderImplLogger{recorderImpl: recorder.recorderImpl, logger: logger}
}
