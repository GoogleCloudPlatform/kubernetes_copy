// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package trace // import "go.opentelemetry.io/otel/sdk/trace"

import (
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/instrumentation"
	"go.opentelemetry.io/otel/sdk/resource"
	"go.opentelemetry.io/otel/trace"
)

// snapshot is an record of a spans state at a particular checkpointed time.
// It is used as a read-only representation of that state.
type snapshot struct {
	name                  string
	spanContext           trace.SpanContext
	parent                trace.SpanContext
	spanKind              trace.SpanKind
	startTime             time.Time
	endTime               time.Time
	attributes            []attribute.KeyValue
	events                []Event
	links                 []Link
	status                Status
	childSpanCount        int
	droppedAttributeCount int
	droppedEventCount     int
	droppedLinkCount      int
	resource              *resource.Resource
	instrumentationScope  instrumentation.Scope
}

var _ ReadOnlySpan = snapshot{}

func (s snapshot) private() {}

// Name returns the name of the span.
func (s snapshot) Name() string {
	return s.name
}

// SpanContext returns the unique SpanContext that identifies the span.
func (s snapshot) SpanContext() trace.SpanContext {
	return s.spanContext
}

// Parent returns the unique SpanContext that identifies the parent of the
// span if one exists. If the span has no parent the returned SpanContext
// will be invalid.
func (s snapshot) Parent() trace.SpanContext {
	return s.parent
}

// SpanKind returns the role the span plays in a Trace.
func (s snapshot) SpanKind() trace.SpanKind {
	return s.spanKind
}

// StartTime returns the time the span started recording.
func (s snapshot) StartTime() time.Time {
	return s.startTime
}

// EndTime returns the time the span stopped recording. It will be zero if
// the span has not ended.
func (s snapshot) EndTime() time.Time {
	return s.endTime
}

// Attributes returns the defining attributes of the span.
func (s snapshot) Attributes() []attribute.KeyValue {
	return s.attributes
}

// Links returns all the links the span has to other spans.
func (s snapshot) Links() []Link {
	return s.links
}

// Events returns all the events that occurred within in the spans
// lifetime.
func (s snapshot) Events() []Event {
	return s.events
}

// Status returns the spans status.
func (s snapshot) Status() Status {
	return s.status
}

// InstrumentationScope returns information about the instrumentation
// scope that created the span.
func (s snapshot) InstrumentationScope() instrumentation.Scope {
	return s.instrumentationScope
}

// InstrumentationLibrary returns information about the instrumentation
// library that created the span.
func (s snapshot) InstrumentationLibrary() instrumentation.Library { //nolint:staticcheck // This method needs to be define for backwards compatibility
	return s.instrumentationScope
}

// Resource returns information about the entity that produced the span.
func (s snapshot) Resource() *resource.Resource {
	return s.resource
}

// DroppedAttributes returns the number of attributes dropped by the span
// due to limits being reached.
func (s snapshot) DroppedAttributes() int {
	return s.droppedAttributeCount
}

// DroppedLinks returns the number of links dropped by the span due to limits
// being reached.
func (s snapshot) DroppedLinks() int {
	return s.droppedLinkCount
}

// DroppedEvents returns the number of events dropped by the span due to
// limits being reached.
func (s snapshot) DroppedEvents() int {
	return s.droppedEventCount
}

// ChildSpanCount returns the count of spans that consider the span a
// direct parent.
func (s snapshot) ChildSpanCount() int {
	return s.childSpanCount
}
