// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package trace // import "go.opentelemetry.io/otel/sdk/trace"

import (
	"context"
	"fmt"
	"reflect"
	"runtime"
	rt "runtime/trace"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/internal/global"
	"go.opentelemetry.io/otel/sdk/instrumentation"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/embedded"
)

// ReadOnlySpan allows reading information from the data structure underlying a
// trace.Span. It is used in places where reading information from a span is
// necessary but changing the span isn't necessary or allowed.
//
// Warning: methods may be added to this interface in minor releases.
type ReadOnlySpan interface {
	// Name returns the name of the span.
	Name() string
	// SpanContext returns the unique SpanContext that identifies the span.
	SpanContext() trace.SpanContext
	// Parent returns the unique SpanContext that identifies the parent of the
	// span if one exists. If the span has no parent the returned SpanContext
	// will be invalid.
	Parent() trace.SpanContext
	// SpanKind returns the role the span plays in a Trace.
	SpanKind() trace.SpanKind
	// StartTime returns the time the span started recording.
	StartTime() time.Time
	// EndTime returns the time the span stopped recording. It will be zero if
	// the span has not ended.
	EndTime() time.Time
	// Attributes returns the defining attributes of the span.
	// The order of the returned attributes is not guaranteed to be stable across invocations.
	Attributes() []attribute.KeyValue
	// Links returns all the links the span has to other spans.
	Links() []Link
	// Events returns all the events that occurred within in the spans
	// lifetime.
	Events() []Event
	// Status returns the spans status.
	Status() Status
	// InstrumentationScope returns information about the instrumentation
	// scope that created the span.
	InstrumentationScope() instrumentation.Scope
	// InstrumentationLibrary returns information about the instrumentation
	// library that created the span.
	// Deprecated: please use InstrumentationScope instead.
	InstrumentationLibrary() instrumentation.Library //nolint:staticcheck // This method needs to be define for backwards compatibility
	// Resource returns information about the entity that produced the span.
	Resource() *resource.Resource
	// DroppedAttributes returns the number of attributes dropped by the span
	// due to limits being reached.
	DroppedAttributes() int
	// DroppedLinks returns the number of links dropped by the span due to
	// limits being reached.
	DroppedLinks() int
	// DroppedEvents returns the number of events dropped by the span due to
	// limits being reached.
	DroppedEvents() int
	// ChildSpanCount returns the count of spans that consider the span a
	// direct parent.
	ChildSpanCount() int

	// A private method to prevent users implementing the
	// interface and so future additions to it will not
	// violate compatibility.
	private()
}

// ReadWriteSpan exposes the same methods as trace.Span and in addition allows
// reading information from the underlying data structure.
// This interface exposes the union of the methods of trace.Span (which is a
// "write-only" span) and ReadOnlySpan. New methods for writing or reading span
// information should be added under trace.Span or ReadOnlySpan, respectively.
//
// Warning: methods may be added to this interface in minor releases.
type ReadWriteSpan interface {
	trace.Span
	ReadOnlySpan
}

// recordingSpan is an implementation of the OpenTelemetry Span API
// representing the individual component of a trace that is sampled.
type recordingSpan struct {
	embedded.Span

	// mu protects the contents of this span.
	mu sync.Mutex

	// parent holds the parent span of this span as a trace.SpanContext.
	parent trace.SpanContext

	// spanKind represents the kind of this span as a trace.SpanKind.
	spanKind trace.SpanKind

	// name is the name of this span.
	name string

	// startTime is the time at which this span was started.
	startTime time.Time

	// endTime is the time at which this span was ended. It contains the zero
	// value of time.Time until the span is ended.
	endTime time.Time

	// status is the status of this span.
	status Status

	// childSpanCount holds the number of child spans created for this span.
	childSpanCount int

	// spanContext holds the SpanContext of this span.
	spanContext trace.SpanContext

	// attributes is a collection of user provided key/values. The collection
	// is constrained by a configurable maximum held by the parent
	// TracerProvider. When additional attributes are added after this maximum
	// is reached these attributes the user is attempting to add are dropped.
	// This dropped number of attributes is tracked and reported in the
	// ReadOnlySpan exported when the span ends.
	attributes        []attribute.KeyValue
	droppedAttributes int
	logDropAttrsOnce  sync.Once

	// events are stored in FIFO queue capped by configured limit.
	events evictedQueue[Event]

	// links are stored in FIFO queue capped by configured limit.
	links evictedQueue[Link]

	// executionTracerTaskEnd ends the execution tracer span.
	executionTracerTaskEnd func()

	// tracer is the SDK tracer that created this span.
	tracer *tracer
}

var (
	_ ReadWriteSpan = (*recordingSpan)(nil)
	_ runtimeTracer = (*recordingSpan)(nil)
)

// SpanContext returns the SpanContext of this span.
func (s *recordingSpan) SpanContext() trace.SpanContext {
	if s == nil {
		return trace.SpanContext{}
	}
	return s.spanContext
}

// IsRecording returns if this span is being recorded. If this span has ended
// this will return false.
func (s *recordingSpan) IsRecording() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.isRecording()
}

// isRecording returns if this span is being recorded. If this span has ended
// this will return false.
//
// This method assumes s.mu.Lock is held by the caller.
func (s *recordingSpan) isRecording() bool {
	if s == nil {
		return false
	}
	return s.endTime.IsZero()
}

// SetStatus sets the status of the Span in the form of a code and a
// description, overriding previous values set. The description is only
// included in the set status when the code is for an error. If this span is
// not being recorded than this method does nothing.
func (s *recordingSpan) SetStatus(code codes.Code, description string) {
	if s == nil {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.isRecording() {
		return
	}
	if s.status.Code > code {
		return
	}

	status := Status{Code: code}
	if code == codes.Error {
		status.Description = description
	}

	s.status = status
}

// SetAttributes sets attributes of this span.
//
// If a key from attributes already exists the value associated with that key
// will be overwritten with the value contained in attributes.
//
// If this span is not being recorded than this method does nothing.
//
// If adding attributes to the span would exceed the maximum amount of
// attributes the span is configured to have, the last added attributes will
// be dropped.
func (s *recordingSpan) SetAttributes(attributes ...attribute.KeyValue) {
	if s == nil || len(attributes) == 0 {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.isRecording() {
		return
	}

	limit := s.tracer.provider.spanLimits.AttributeCountLimit
	if limit == 0 {
		// No attributes allowed.
		s.addDroppedAttr(len(attributes))
		return
	}

	// If adding these attributes could exceed the capacity of s perform a
	// de-duplication and truncation while adding to avoid over allocation.
	if limit > 0 && len(s.attributes)+len(attributes) > limit {
		s.addOverCapAttrs(limit, attributes)
		return
	}

	// Otherwise, add without deduplication. When attributes are read they
	// will be deduplicated, optimizing the operation.
	s.attributes = slices.Grow(s.attributes, len(attributes))
	for _, a := range attributes {
		if !a.Valid() {
			// Drop all invalid attributes.
			s.addDroppedAttr(1)
			continue
		}
		a = truncateAttr(s.tracer.provider.spanLimits.AttributeValueLengthLimit, a)
		s.attributes = append(s.attributes, a)
	}
}

// Declared as a var so tests can override.
var logDropAttrs = func() {
	global.Warn("limit reached: dropping trace Span attributes")
}

// addDroppedAttr adds incr to the count of dropped attributes.
//
// The first, and only the first, time this method is called a warning will be
// logged.
//
// This method assumes s.mu.Lock is held by the caller.
func (s *recordingSpan) addDroppedAttr(incr int) {
	s.droppedAttributes += incr
	s.logDropAttrsOnce.Do(logDropAttrs)
}

// addOverCapAttrs adds the attributes attrs to the span s while
// de-duplicating the attributes of s and attrs and dropping attributes that
// exceed the limit.
//
// This method assumes s.mu.Lock is held by the caller.
//
// This method should only be called when there is a possibility that adding
// attrs to s will exceed the limit. Otherwise, attrs should be added to s
// without checking for duplicates and all retrieval methods of the attributes
// for s will de-duplicate as needed.
//
// This method assumes limit is a value > 0. The argument should be validated
// by the caller.
func (s *recordingSpan) addOverCapAttrs(limit int, attrs []attribute.KeyValue) {
	// In order to not allocate more capacity to s.attributes than needed,
	// prune and truncate this addition of attributes while adding.

	// Do not set a capacity when creating this map. Benchmark testing has
	// showed this to only add unused memory allocations in general use.
	exists := make(map[attribute.Key]int, len(s.attributes))
	s.dedupeAttrsFromRecord(exists)

	// Now that s.attributes is deduplicated, adding unique attributes up to
	// the capacity of s will not over allocate s.attributes.

	// max size = limit
	maxCap := min(len(attrs)+len(s.attributes), limit)
	if cap(s.attributes) < maxCap {
		s.attributes = slices.Grow(s.attributes, maxCap-cap(s.attributes))
	}
	for _, a := range attrs {
		if !a.Valid() {
			// Drop all invalid attributes.
			s.addDroppedAttr(1)
			continue
		}

		if idx, ok := exists[a.Key]; ok {
			// Perform all updates before dropping, even when at capacity.
			a = truncateAttr(s.tracer.provider.spanLimits.AttributeValueLengthLimit, a)
			s.attributes[idx] = a
			continue
		}

		if len(s.attributes) >= limit {
			// Do not just drop all of the remaining attributes, make sure
			// updates are checked and performed.
			s.addDroppedAttr(1)
		} else {
			a = truncateAttr(s.tracer.provider.spanLimits.AttributeValueLengthLimit, a)
			s.attributes = append(s.attributes, a)
			exists[a.Key] = len(s.attributes) - 1
		}
	}
}

// truncateAttr returns a truncated version of attr. Only string and string
// slice attribute values are truncated. String values are truncated to at
// most a length of limit. Each string slice value is truncated in this fashion
// (the slice length itself is unaffected).
//
// No truncation is performed for a negative limit.
func truncateAttr(limit int, attr attribute.KeyValue) attribute.KeyValue {
	if limit < 0 {
		return attr
	}
	switch attr.Value.Type() {
	case attribute.STRING:
		v := attr.Value.AsString()
		return attr.Key.String(truncate(limit, v))
	case attribute.STRINGSLICE:
		v := attr.Value.AsStringSlice()
		for i := range v {
			v[i] = truncate(limit, v[i])
		}
		return attr.Key.StringSlice(v)
	}
	return attr
}

// truncate returns a truncated version of s such that it contains less than
// the limit number of characters. Truncation is applied by returning the limit
// number of valid characters contained in s.
//
// If limit is negative, it returns the original string.
//
// UTF-8 is supported. When truncating, all invalid characters are dropped
// before applying truncation.
//
// If s already contains less than the limit number of bytes, it is returned
// unchanged. No invalid characters are removed.
func truncate(limit int, s string) string {
	// This prioritize performance in the following order based on the most
	// common expected use-cases.
	//
	//  - Short values less than the default limit (128).
	//  - Strings with valid encodings that exceed the limit.
	//  - No limit.
	//  - Strings with invalid encodings that exceed the limit.
	if limit < 0 || len(s) <= limit {
		return s
	}

	// Optimistically, assume all valid UTF-8.
	var b strings.Builder
	count := 0
	for i, c := range s {
		if c != utf8.RuneError {
			count++
			if count > limit {
				return s[:i]
			}
			continue
		}

		_, size := utf8.DecodeRuneInString(s[i:])
		if size == 1 {
			// Invalid encoding.
			b.Grow(len(s) - 1)
			_, _ = b.WriteString(s[:i])
			s = s[i:]
			break
		}
	}

	// Fast-path, no invalid input.
	if b.Cap() == 0 {
		return s
	}

	// Truncate while validating UTF-8.
	for i := 0; i < len(s) && count < limit; {
		c := s[i]
		if c < utf8.RuneSelf {
			// Optimization for single byte runes (common case).
			_ = b.WriteByte(c)
			i++
			count++
			continue
		}

		_, size := utf8.DecodeRuneInString(s[i:])
		if size == 1 {
			// We checked for all 1-byte runes above, this is a RuneError.
			i++
			continue
		}

		_, _ = b.WriteString(s[i : i+size])
		i += size
		count++
	}

	return b.String()
}

// End ends the span. This method does nothing if the span is already ended or
// is not being recorded.
//
// The only SpanEndOption currently supported are [trace.WithTimestamp], and
// [trace.WithStackTrace].
//
// If this method is called while panicking an error event is added to the
// Span before ending it and the panic is continued.
func (s *recordingSpan) End(options ...trace.SpanEndOption) {
	// Do not start by checking if the span is being recorded which requires
	// acquiring a lock. Make a minimal check that the span is not nil.
	if s == nil {
		return
	}

	// Store the end time as soon as possible to avoid artificially increasing
	// the span's duration in case some operation below takes a while.
	et := monotonicEndTime(s.startTime)

	// Lock the span now that we have an end time and see if we need to do any more processing.
	s.mu.Lock()
	if !s.isRecording() {
		s.mu.Unlock()
		return
	}

	config := trace.NewSpanEndConfig(options...)
	if recovered := recover(); recovered != nil {
		// Record but don't stop the panic.
		defer panic(recovered)
		opts := []trace.EventOption{
			trace.WithAttributes(
				semconv.ExceptionType(typeStr(recovered)),
				semconv.ExceptionMessage(fmt.Sprint(recovered)),
			),
		}

		if config.StackTrace() {
			opts = append(opts, trace.WithAttributes(
				semconv.ExceptionStacktrace(recordStackTrace()),
			))
		}

		s.addEvent(semconv.ExceptionEventName, opts...)
	}

	if s.executionTracerTaskEnd != nil {
		s.mu.Unlock()
		s.executionTracerTaskEnd()
		s.mu.Lock()
	}

	// Setting endTime to non-zero marks the span as ended and not recording.
	if config.Timestamp().IsZero() {
		s.endTime = et
	} else {
		s.endTime = config.Timestamp()
	}
	s.mu.Unlock()

	sps := s.tracer.provider.getSpanProcessors()
	if len(sps) == 0 {
		return
	}
	snap := s.snapshot()
	for _, sp := range sps {
		sp.sp.OnEnd(snap)
	}
}

// monotonicEndTime returns the end time at present but offset from start,
// monotonically.
//
// The monotonic clock is used in subtractions hence the duration since start
// added back to start gives end as a monotonic time. See
// https://golang.org/pkg/time/#hdr-Monotonic_Clocks
func monotonicEndTime(start time.Time) time.Time {
	return start.Add(time.Since(start))
}

// RecordError will record err as a span event for this span. An additional call to
// SetStatus is required if the Status of the Span should be set to Error, this method
// does not change the Span status. If this span is not being recorded or err is nil
// than this method does nothing.
func (s *recordingSpan) RecordError(err error, opts ...trace.EventOption) {
	if s == nil || err == nil {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.isRecording() {
		return
	}

	opts = append(opts, trace.WithAttributes(
		semconv.ExceptionType(typeStr(err)),
		semconv.ExceptionMessage(err.Error()),
	))

	c := trace.NewEventConfig(opts...)
	if c.StackTrace() {
		opts = append(opts, trace.WithAttributes(
			semconv.ExceptionStacktrace(recordStackTrace()),
		))
	}

	s.addEvent(semconv.ExceptionEventName, opts...)
}

func typeStr(i interface{}) string {
	t := reflect.TypeOf(i)
	if t.PkgPath() == "" && t.Name() == "" {
		// Likely a builtin type.
		return t.String()
	}
	return fmt.Sprintf("%s.%s", t.PkgPath(), t.Name())
}

func recordStackTrace() string {
	stackTrace := make([]byte, 2048)
	n := runtime.Stack(stackTrace, false)

	return string(stackTrace[0:n])
}

// AddEvent adds an event with the provided name and options. If this span is
// not being recorded then this method does nothing.
func (s *recordingSpan) AddEvent(name string, o ...trace.EventOption) {
	if s == nil {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.isRecording() {
		return
	}
	s.addEvent(name, o...)
}

// addEvent adds an event with the provided name and options.
//
// This method assumes s.mu.Lock is held by the caller.
func (s *recordingSpan) addEvent(name string, o ...trace.EventOption) {
	c := trace.NewEventConfig(o...)
	e := Event{Name: name, Attributes: c.Attributes(), Time: c.Timestamp()}

	// Discard attributes over limit.
	limit := s.tracer.provider.spanLimits.AttributePerEventCountLimit
	if limit == 0 {
		// Drop all attributes.
		e.DroppedAttributeCount = len(e.Attributes)
		e.Attributes = nil
	} else if limit > 0 && len(e.Attributes) > limit {
		// Drop over capacity.
		e.DroppedAttributeCount = len(e.Attributes) - limit
		e.Attributes = e.Attributes[:limit]
	}

	s.events.add(e)
}

// SetName sets the name of this span. If this span is not being recorded than
// this method does nothing.
func (s *recordingSpan) SetName(name string) {
	if s == nil {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.isRecording() {
		return
	}
	s.name = name
}

// Name returns the name of this span.
func (s *recordingSpan) Name() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.name
}

// Name returns the SpanContext of this span's parent span.
func (s *recordingSpan) Parent() trace.SpanContext {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.parent
}

// SpanKind returns the SpanKind of this span.
func (s *recordingSpan) SpanKind() trace.SpanKind {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.spanKind
}

// StartTime returns the time this span started.
func (s *recordingSpan) StartTime() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.startTime
}

// EndTime returns the time this span ended. For spans that have not yet
// ended, the returned value will be the zero value of time.Time.
func (s *recordingSpan) EndTime() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.endTime
}

// Attributes returns the attributes of this span.
//
// The order of the returned attributes is not guaranteed to be stable.
func (s *recordingSpan) Attributes() []attribute.KeyValue {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dedupeAttrs()
	return s.attributes
}

// dedupeAttrs deduplicates the attributes of s to fit capacity.
//
// This method assumes s.mu.Lock is held by the caller.
func (s *recordingSpan) dedupeAttrs() {
	// Do not set a capacity when creating this map. Benchmark testing has
	// showed this to only add unused memory allocations in general use.
	exists := make(map[attribute.Key]int, len(s.attributes))
	s.dedupeAttrsFromRecord(exists)
}

// dedupeAttrsFromRecord deduplicates the attributes of s to fit capacity
// using record as the record of unique attribute keys to their index.
//
// This method assumes s.mu.Lock is held by the caller.
func (s *recordingSpan) dedupeAttrsFromRecord(record map[attribute.Key]int) {
	// Use the fact that slices share the same backing array.
	unique := s.attributes[:0]
	for _, a := range s.attributes {
		if idx, ok := record[a.Key]; ok {
			unique[idx] = a
		} else {
			unique = append(unique, a)
			record[a.Key] = len(unique) - 1
		}
	}
	clear(s.attributes[len(unique):]) // Erase unneeded elements to let GC collect objects.
	s.attributes = unique
}

// Links returns the links of this span.
func (s *recordingSpan) Links() []Link {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.links.queue) == 0 {
		return []Link{}
	}
	return s.links.copy()
}

// Events returns the events of this span.
func (s *recordingSpan) Events() []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.events.queue) == 0 {
		return []Event{}
	}
	return s.events.copy()
}

// Status returns the status of this span.
func (s *recordingSpan) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}

// InstrumentationScope returns the instrumentation.Scope associated with
// the Tracer that created this span.
func (s *recordingSpan) InstrumentationScope() instrumentation.Scope {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tracer.instrumentationScope
}

// InstrumentationLibrary returns the instrumentation.Library associated with
// the Tracer that created this span.
func (s *recordingSpan) InstrumentationLibrary() instrumentation.Library { //nolint:staticcheck // This method needs to be define for backwards compatibility
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tracer.instrumentationScope
}

// Resource returns the Resource associated with the Tracer that created this
// span.
func (s *recordingSpan) Resource() *resource.Resource {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tracer.provider.resource
}

func (s *recordingSpan) AddLink(link trace.Link) {
	if s == nil {
		return
	}
	if !link.SpanContext.IsValid() && len(link.Attributes) == 0 &&
		link.SpanContext.TraceState().Len() == 0 {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.isRecording() {
		return
	}

	l := Link{SpanContext: link.SpanContext, Attributes: link.Attributes}

	// Discard attributes over limit.
	limit := s.tracer.provider.spanLimits.AttributePerLinkCountLimit
	if limit == 0 {
		// Drop all attributes.
		l.DroppedAttributeCount = len(l.Attributes)
		l.Attributes = nil
	} else if limit > 0 && len(l.Attributes) > limit {
		l.DroppedAttributeCount = len(l.Attributes) - limit
		l.Attributes = l.Attributes[:limit]
	}

	s.links.add(l)
}

// DroppedAttributes returns the number of attributes dropped by the span
// due to limits being reached.
func (s *recordingSpan) DroppedAttributes() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.droppedAttributes
}

// DroppedLinks returns the number of links dropped by the span due to limits
// being reached.
func (s *recordingSpan) DroppedLinks() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.links.droppedCount
}

// DroppedEvents returns the number of events dropped by the span due to
// limits being reached.
func (s *recordingSpan) DroppedEvents() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.events.droppedCount
}

// ChildSpanCount returns the count of spans that consider the span a
// direct parent.
func (s *recordingSpan) ChildSpanCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.childSpanCount
}

// TracerProvider returns a trace.TracerProvider that can be used to generate
// additional Spans on the same telemetry pipeline as the current Span.
func (s *recordingSpan) TracerProvider() trace.TracerProvider {
	return s.tracer.provider
}

// snapshot creates a read-only copy of the current state of the span.
func (s *recordingSpan) snapshot() ReadOnlySpan {
	var sd snapshot
	s.mu.Lock()
	defer s.mu.Unlock()

	sd.endTime = s.endTime
	sd.instrumentationScope = s.tracer.instrumentationScope
	sd.name = s.name
	sd.parent = s.parent
	sd.resource = s.tracer.provider.resource
	sd.spanContext = s.spanContext
	sd.spanKind = s.spanKind
	sd.startTime = s.startTime
	sd.status = s.status
	sd.childSpanCount = s.childSpanCount

	if len(s.attributes) > 0 {
		s.dedupeAttrs()
		sd.attributes = s.attributes
	}
	sd.droppedAttributeCount = s.droppedAttributes
	if len(s.events.queue) > 0 {
		sd.events = s.events.copy()
		sd.droppedEventCount = s.events.droppedCount
	}
	if len(s.links.queue) > 0 {
		sd.links = s.links.copy()
		sd.droppedLinkCount = s.links.droppedCount
	}
	return &sd
}

func (s *recordingSpan) addChild() {
	if s == nil {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.isRecording() {
		return
	}
	s.childSpanCount++
}

func (*recordingSpan) private() {}

// runtimeTrace starts a "runtime/trace".Task for the span and returns a
// context containing the task.
func (s *recordingSpan) runtimeTrace(ctx context.Context) context.Context {
	if !rt.IsEnabled() {
		// Avoid additional overhead if runtime/trace is not enabled.
		return ctx
	}
	nctx, task := rt.NewTask(ctx, s.name)

	s.mu.Lock()
	s.executionTracerTaskEnd = task.End
	s.mu.Unlock()

	return nctx
}

// nonRecordingSpan is a minimal implementation of the OpenTelemetry Span API
// that wraps a SpanContext. It performs no operations other than to return
// the wrapped SpanContext or TracerProvider that created it.
type nonRecordingSpan struct {
	embedded.Span

	// tracer is the SDK tracer that created this span.
	tracer *tracer
	sc     trace.SpanContext
}

var _ trace.Span = nonRecordingSpan{}

// SpanContext returns the wrapped SpanContext.
func (s nonRecordingSpan) SpanContext() trace.SpanContext { return s.sc }

// IsRecording always returns false.
func (nonRecordingSpan) IsRecording() bool { return false }

// SetStatus does nothing.
func (nonRecordingSpan) SetStatus(codes.Code, string) {}

// SetError does nothing.
func (nonRecordingSpan) SetError(bool) {}

// SetAttributes does nothing.
func (nonRecordingSpan) SetAttributes(...attribute.KeyValue) {}

// End does nothing.
func (nonRecordingSpan) End(...trace.SpanEndOption) {}

// RecordError does nothing.
func (nonRecordingSpan) RecordError(error, ...trace.EventOption) {}

// AddEvent does nothing.
func (nonRecordingSpan) AddEvent(string, ...trace.EventOption) {}

// AddLink does nothing.
func (nonRecordingSpan) AddLink(trace.Link) {}

// SetName does nothing.
func (nonRecordingSpan) SetName(string) {}

// TracerProvider returns the trace.TracerProvider that provided the Tracer
// that created this span.
func (s nonRecordingSpan) TracerProvider() trace.TracerProvider { return s.tracer.provider }

func isRecording(s SamplingResult) bool {
	return s.Decision == RecordOnly || s.Decision == RecordAndSample
}

func isSampled(s SamplingResult) bool {
	return s.Decision == RecordAndSample
}

// Status is the classified state of a Span.
type Status struct {
	// Code is an identifier of a Spans state classification.
	Code codes.Code
	// Description is a user hint about why that status was set. It is only
	// applicable when Code is Error.
	Description string
}
