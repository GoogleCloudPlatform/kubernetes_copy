// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package trace // import "go.opentelemetry.io/otel/trace"

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace/embedded"
)

// Span is the individual component of a trace. It represents a single named
// and timed operation of a workflow that is traced. A Tracer is used to
// create a Span and it is then up to the operation the Span represents to
// properly end the Span when the operation itself ends.
//
// Warning: Methods may be added to this interface in minor releases. See
// package documentation on API implementation for information on how to set
// default behavior for unimplemented methods.
type Span interface {
	// Users of the interface can ignore this. This embedded type is only used
	// by implementations of this interface. See the "API Implementations"
	// section of the package documentation for more information.
	embedded.Span

	// End completes the Span. The Span is considered complete and ready to be
	// delivered through the rest of the telemetry pipeline after this method
	// is called. Therefore, updates to the Span are not allowed after this
	// method has been called.
	End(options ...SpanEndOption)

	// AddEvent adds an event with the provided name and options.
	AddEvent(name string, options ...EventOption)

	// AddLink adds a link.
	// Adding links at span creation using WithLinks is preferred to calling AddLink
	// later, for contexts that are available during span creation, because head
	// sampling decisions can only consider information present during span creation.
	AddLink(link Link)

	// IsRecording returns the recording state of the Span. It will return
	// true if the Span is active and events can be recorded.
	IsRecording() bool

	// RecordError will record err as an exception span event for this span. An
	// additional call to SetStatus is required if the Status of the Span should
	// be set to Error, as this method does not change the Span status. If this
	// span is not being recorded or err is nil then this method does nothing.
	RecordError(err error, options ...EventOption)

	// SpanContext returns the SpanContext of the Span. The returned SpanContext
	// is usable even after the End method has been called for the Span.
	SpanContext() SpanContext

	// SetStatus sets the status of the Span in the form of a code and a
	// description, provided the status hasn't already been set to a higher
	// value before (OK > Error > Unset). The description is only included in a
	// status when the code is for an error.
	SetStatus(code codes.Code, description string)

	// SetName sets the Span name.
	SetName(name string)

	// SetAttributes sets kv as attributes of the Span. If a key from kv
	// already exists for an attribute of the Span it will be overwritten with
	// the value contained in kv.
	SetAttributes(kv ...attribute.KeyValue)

	// TracerProvider returns a TracerProvider that can be used to generate
	// additional Spans on the same telemetry pipeline as the current Span.
	TracerProvider() TracerProvider
}

// Link is the relationship between two Spans. The relationship can be within
// the same Trace or across different Traces.
//
// For example, a Link is used in the following situations:
//
//  1. Batch Processing: A batch of operations may contain operations
//     associated with one or more traces/spans. Since there can only be one
//     parent SpanContext, a Link is used to keep reference to the
//     SpanContext of all operations in the batch.
//  2. Public Endpoint: A SpanContext for an in incoming client request on a
//     public endpoint should be considered untrusted. In such a case, a new
//     trace with its own identity and sampling decision needs to be created,
//     but this new trace needs to be related to the original trace in some
//     form. A Link is used to keep reference to the original SpanContext and
//     track the relationship.
type Link struct {
	// SpanContext of the linked Span.
	SpanContext SpanContext

	// Attributes describe the aspects of the link.
	Attributes []attribute.KeyValue
}

// LinkFromContext returns a link encapsulating the SpanContext in the provided
// ctx.
func LinkFromContext(ctx context.Context, attrs ...attribute.KeyValue) Link {
	return Link{
		SpanContext: SpanContextFromContext(ctx),
		Attributes:  attrs,
	}
}

// SpanKind is the role a Span plays in a Trace.
type SpanKind int

// As a convenience, these match the proto definition, see
// https://github.com/open-telemetry/opentelemetry-proto/blob/30d237e1ff3ab7aa50e0922b5bebdd93505090af/opentelemetry/proto/trace/v1/trace.proto#L101-L129
//
// The unspecified value is not a valid `SpanKind`. Use `ValidateSpanKind()`
// to coerce a span kind to a valid value.
const (
	// SpanKindUnspecified is an unspecified SpanKind and is not a valid
	// SpanKind. SpanKindUnspecified should be replaced with SpanKindInternal
	// if it is received.
	SpanKindUnspecified SpanKind = 0
	// SpanKindInternal is a SpanKind for a Span that represents an internal
	// operation within an application.
	SpanKindInternal SpanKind = 1
	// SpanKindServer is a SpanKind for a Span that represents the operation
	// of handling a request from a client.
	SpanKindServer SpanKind = 2
	// SpanKindClient is a SpanKind for a Span that represents the operation
	// of client making a request to a server.
	SpanKindClient SpanKind = 3
	// SpanKindProducer is a SpanKind for a Span that represents the operation
	// of a producer sending a message to a message broker. Unlike
	// SpanKindClient and SpanKindServer, there is often no direct
	// relationship between this kind of Span and a SpanKindConsumer kind. A
	// SpanKindProducer Span will end once the message is accepted by the
	// message broker which might not overlap with the processing of that
	// message.
	SpanKindProducer SpanKind = 4
	// SpanKindConsumer is a SpanKind for a Span that represents the operation
	// of a consumer receiving a message from a message broker. Like
	// SpanKindProducer Spans, there is often no direct relationship between
	// this Span and the Span that produced the message.
	SpanKindConsumer SpanKind = 5
)

// ValidateSpanKind returns a valid span kind value.  This will coerce
// invalid values into the default value, SpanKindInternal.
func ValidateSpanKind(spanKind SpanKind) SpanKind {
	switch spanKind {
	case SpanKindInternal,
		SpanKindServer,
		SpanKindClient,
		SpanKindProducer,
		SpanKindConsumer:
		// valid
		return spanKind
	default:
		return SpanKindInternal
	}
}

// String returns the specified name of the SpanKind in lower-case.
func (sk SpanKind) String() string {
	switch sk {
	case SpanKindInternal:
		return "internal"
	case SpanKindServer:
		return "server"
	case SpanKindClient:
		return "client"
	case SpanKindProducer:
		return "producer"
	case SpanKindConsumer:
		return "consumer"
	default:
		return "unspecified"
	}
}
