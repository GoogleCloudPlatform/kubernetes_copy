// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package tracetest // import "go.opentelemetry.io/otel/sdk/trace/tracetest"

import (
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/instrumentation"
	"go.opentelemetry.io/otel/sdk/resource"
	tracesdk "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// SpanStubs is a slice of SpanStub use for testing an SDK.
type SpanStubs []SpanStub

// SpanStubsFromReadOnlySpans returns SpanStubs populated from ro.
func SpanStubsFromReadOnlySpans(ro []tracesdk.ReadOnlySpan) SpanStubs {
	if len(ro) == 0 {
		return nil
	}

	s := make(SpanStubs, 0, len(ro))
	for _, r := range ro {
		s = append(s, SpanStubFromReadOnlySpan(r))
	}

	return s
}

// Snapshots returns s as a slice of ReadOnlySpans.
func (s SpanStubs) Snapshots() []tracesdk.ReadOnlySpan {
	if len(s) == 0 {
		return nil
	}

	ro := make([]tracesdk.ReadOnlySpan, len(s))
	for i := 0; i < len(s); i++ {
		ro[i] = s[i].Snapshot()
	}
	return ro
}

// SpanStub is a stand-in for a Span.
type SpanStub struct {
	Name                 string
	SpanContext          trace.SpanContext
	Parent               trace.SpanContext
	SpanKind             trace.SpanKind
	StartTime            time.Time
	EndTime              time.Time
	Attributes           []attribute.KeyValue
	Events               []tracesdk.Event
	Links                []tracesdk.Link
	Status               tracesdk.Status
	DroppedAttributes    int
	DroppedEvents        int
	DroppedLinks         int
	ChildSpanCount       int
	Resource             *resource.Resource
	InstrumentationScope instrumentation.Scope

	// Deprecated: use InstrumentationScope instead.
	InstrumentationLibrary instrumentation.Library //nolint:staticcheck // This method needs to be define for backwards compatibility
}

// SpanStubFromReadOnlySpan returns a SpanStub populated from ro.
func SpanStubFromReadOnlySpan(ro tracesdk.ReadOnlySpan) SpanStub {
	if ro == nil {
		return SpanStub{}
	}

	return SpanStub{
		Name:                   ro.Name(),
		SpanContext:            ro.SpanContext(),
		Parent:                 ro.Parent(),
		SpanKind:               ro.SpanKind(),
		StartTime:              ro.StartTime(),
		EndTime:                ro.EndTime(),
		Attributes:             ro.Attributes(),
		Events:                 ro.Events(),
		Links:                  ro.Links(),
		Status:                 ro.Status(),
		DroppedAttributes:      ro.DroppedAttributes(),
		DroppedEvents:          ro.DroppedEvents(),
		DroppedLinks:           ro.DroppedLinks(),
		ChildSpanCount:         ro.ChildSpanCount(),
		Resource:               ro.Resource(),
		InstrumentationScope:   ro.InstrumentationScope(),
		InstrumentationLibrary: ro.InstrumentationScope(),
	}
}

// Snapshot returns a read-only copy of the SpanStub.
func (s SpanStub) Snapshot() tracesdk.ReadOnlySpan {
	scopeOrLibrary := s.InstrumentationScope
	if scopeOrLibrary.Name == "" && scopeOrLibrary.Version == "" && scopeOrLibrary.SchemaURL == "" {
		scopeOrLibrary = s.InstrumentationLibrary
	}

	return spanSnapshot{
		name:                 s.Name,
		spanContext:          s.SpanContext,
		parent:               s.Parent,
		spanKind:             s.SpanKind,
		startTime:            s.StartTime,
		endTime:              s.EndTime,
		attributes:           s.Attributes,
		events:               s.Events,
		links:                s.Links,
		status:               s.Status,
		droppedAttributes:    s.DroppedAttributes,
		droppedEvents:        s.DroppedEvents,
		droppedLinks:         s.DroppedLinks,
		childSpanCount:       s.ChildSpanCount,
		resource:             s.Resource,
		instrumentationScope: scopeOrLibrary,
	}
}

type spanSnapshot struct {
	// Embed the interface to implement the private method.
	tracesdk.ReadOnlySpan

	name                 string
	spanContext          trace.SpanContext
	parent               trace.SpanContext
	spanKind             trace.SpanKind
	startTime            time.Time
	endTime              time.Time
	attributes           []attribute.KeyValue
	events               []tracesdk.Event
	links                []tracesdk.Link
	status               tracesdk.Status
	droppedAttributes    int
	droppedEvents        int
	droppedLinks         int
	childSpanCount       int
	resource             *resource.Resource
	instrumentationScope instrumentation.Scope
}

func (s spanSnapshot) Name() string                     { return s.name }
func (s spanSnapshot) SpanContext() trace.SpanContext   { return s.spanContext }
func (s spanSnapshot) Parent() trace.SpanContext        { return s.parent }
func (s spanSnapshot) SpanKind() trace.SpanKind         { return s.spanKind }
func (s spanSnapshot) StartTime() time.Time             { return s.startTime }
func (s spanSnapshot) EndTime() time.Time               { return s.endTime }
func (s spanSnapshot) Attributes() []attribute.KeyValue { return s.attributes }
func (s spanSnapshot) Links() []tracesdk.Link           { return s.links }
func (s spanSnapshot) Events() []tracesdk.Event         { return s.events }
func (s spanSnapshot) Status() tracesdk.Status          { return s.status }
func (s spanSnapshot) DroppedAttributes() int           { return s.droppedAttributes }
func (s spanSnapshot) DroppedLinks() int                { return s.droppedLinks }
func (s spanSnapshot) DroppedEvents() int               { return s.droppedEvents }
func (s spanSnapshot) ChildSpanCount() int              { return s.childSpanCount }
func (s spanSnapshot) Resource() *resource.Resource     { return s.resource }
func (s spanSnapshot) InstrumentationScope() instrumentation.Scope {
	return s.instrumentationScope
}

func (s spanSnapshot) InstrumentationLibrary() instrumentation.Library { //nolint:staticcheck // This method needs to be define for backwards compatibility
	return s.instrumentationScope
}
