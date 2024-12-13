// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

// A Span represents a single operation performed by a single component of the
// system.
type Span struct {
	// A unique identifier for a trace. All spans from the same trace share
	// the same `trace_id`. The ID is a 16-byte array. An ID with all zeroes OR
	// of length other than 16 bytes is considered invalid (empty string in OTLP/JSON
	// is zero-length and thus is also invalid).
	//
	// This field is required.
	TraceID TraceID `json:"traceId,omitempty"`
	// A unique identifier for a span within a trace, assigned when the span
	// is created. The ID is an 8-byte array. An ID with all zeroes OR of length
	// other than 8 bytes is considered invalid (empty string in OTLP/JSON
	// is zero-length and thus is also invalid).
	//
	// This field is required.
	SpanID SpanID `json:"spanId,omitempty"`
	// trace_state conveys information about request position in multiple distributed tracing graphs.
	// It is a trace_state in w3c-trace-context format: https://www.w3.org/TR/trace-context/#tracestate-header
	// See also https://github.com/w3c/distributed-tracing for more details about this field.
	TraceState string `json:"traceState,omitempty"`
	// The `span_id` of this span's parent span. If this is a root span, then this
	// field must be empty. The ID is an 8-byte array.
	ParentSpanID SpanID `json:"parentSpanId,omitempty"`
	// Flags, a bit field.
	//
	// Bits 0-7 (8 least significant bits) are the trace flags as defined in W3C Trace
	// Context specification. To read the 8-bit W3C trace flag, use
	// `flags & SPAN_FLAGS_TRACE_FLAGS_MASK`.
	//
	// See https://www.w3.org/TR/trace-context-2/#trace-flags for the flag definitions.
	//
	// Bits 8 and 9 represent the 3 states of whether a span's parent
	// is remote. The states are (unknown, is not remote, is remote).
	// To read whether the value is known, use `(flags & SPAN_FLAGS_CONTEXT_HAS_IS_REMOTE_MASK) != 0`.
	// To read whether the span is remote, use `(flags & SPAN_FLAGS_CONTEXT_IS_REMOTE_MASK) != 0`.
	//
	// When creating span messages, if the message is logically forwarded from another source
	// with an equivalent flags fields (i.e., usually another OTLP span message), the field SHOULD
	// be copied as-is. If creating from a source that does not have an equivalent flags field
	// (such as a runtime representation of an OpenTelemetry span), the high 22 bits MUST
	// be set to zero.
	// Readers MUST NOT assume that bits 10-31 (22 most significant bits) will be zero.
	//
	// [Optional].
	Flags uint32 `json:"flags,omitempty"`
	// A description of the span's operation.
	//
	// For example, the name can be a qualified method name or a file name
	// and a line number where the operation is called. A best practice is to use
	// the same display name at the same call point in an application.
	// This makes it easier to correlate spans in different traces.
	//
	// This field is semantically required to be set to non-empty string.
	// Empty value is equivalent to an unknown span name.
	//
	// This field is required.
	Name string `json:"name"`
	// Distinguishes between spans generated in a particular context. For example,
	// two spans with the same name may be distinguished using `CLIENT` (caller)
	// and `SERVER` (callee) to identify queueing latency associated with the span.
	Kind SpanKind `json:"kind,omitempty"`
	// start_time_unix_nano is the start time of the span. On the client side, this is the time
	// kept by the local machine where the span execution starts. On the server side, this
	// is the time when the server's application handler starts running.
	// Value is UNIX Epoch time in nanoseconds since 00:00:00 UTC on 1 January 1970.
	//
	// This field is semantically required and it is expected that end_time >= start_time.
	StartTime time.Time `json:"startTimeUnixNano,omitempty"`
	// end_time_unix_nano is the end time of the span. On the client side, this is the time
	// kept by the local machine where the span execution ends. On the server side, this
	// is the time when the server application handler stops running.
	// Value is UNIX Epoch time in nanoseconds since 00:00:00 UTC on 1 January 1970.
	//
	// This field is semantically required and it is expected that end_time >= start_time.
	EndTime time.Time `json:"endTimeUnixNano,omitempty"`
	// attributes is a collection of key/value pairs. Note, global attributes
	// like server name can be set using the resource API. Examples of attributes:
	//
	//     "/http/user_agent": "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_14_2) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/71.0.3578.98 Safari/537.36"
	//     "/http/server_latency": 300
	//     "example.com/myattribute": true
	//     "example.com/score": 10.239
	//
	// The OpenTelemetry API specification further restricts the allowed value types:
	// https://github.com/open-telemetry/opentelemetry-specification/blob/main/specification/common/README.md#attribute
	// Attribute keys MUST be unique (it is not allowed to have more than one
	// attribute with the same key).
	Attrs []Attr `json:"attributes,omitempty"`
	// dropped_attributes_count is the number of attributes that were discarded. Attributes
	// can be discarded because their keys are too long or because there are too many
	// attributes. If this value is 0, then no attributes were dropped.
	DroppedAttrs uint32 `json:"droppedAttributesCount,omitempty"`
	// events is a collection of Event items.
	Events []*SpanEvent `json:"events,omitempty"`
	// dropped_events_count is the number of dropped events. If the value is 0, then no
	// events were dropped.
	DroppedEvents uint32 `json:"droppedEventsCount,omitempty"`
	// links is a collection of Links, which are references from this span to a span
	// in the same or different trace.
	Links []*SpanLink `json:"links,omitempty"`
	// dropped_links_count is the number of dropped links after the maximum size was
	// enforced. If this value is 0, then no links were dropped.
	DroppedLinks uint32 `json:"droppedLinksCount,omitempty"`
	// An optional final status for this span. Semantically when Status isn't set, it means
	// span's status code is unset, i.e. assume STATUS_CODE_UNSET (code = 0).
	Status *Status `json:"status,omitempty"`
}

// MarshalJSON encodes s into OTLP formatted JSON.
func (s Span) MarshalJSON() ([]byte, error) {
	startT := s.StartTime.UnixNano()
	if s.StartTime.IsZero() || startT < 0 {
		startT = 0
	}

	endT := s.EndTime.UnixNano()
	if s.EndTime.IsZero() || endT < 0 {
		endT = 0
	}

	// Override non-empty default SpanID marshal and omitempty.
	var parentSpanId string
	if !s.ParentSpanID.IsEmpty() {
		b := make([]byte, hex.EncodedLen(spanIDSize))
		hex.Encode(b, s.ParentSpanID[:])
		parentSpanId = string(b)
	}

	type Alias Span
	return json.Marshal(struct {
		Alias
		ParentSpanID string `json:"parentSpanId,omitempty"`
		StartTime    uint64 `json:"startTimeUnixNano,omitempty"`
		EndTime      uint64 `json:"endTimeUnixNano,omitempty"`
	}{
		Alias:        Alias(s),
		ParentSpanID: parentSpanId,
		StartTime:    uint64(startT),
		EndTime:      uint64(endT),
	})
}

// UnmarshalJSON decodes the OTLP formatted JSON contained in data into s.
func (s *Span) UnmarshalJSON(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))

	t, err := decoder.Token()
	if err != nil {
		return err
	}
	if t != json.Delim('{') {
		return errors.New("invalid Span type")
	}

	for decoder.More() {
		keyIface, err := decoder.Token()
		if err != nil {
			if errors.Is(err, io.EOF) {
				// Empty.
				return nil
			}
			return err
		}

		key, ok := keyIface.(string)
		if !ok {
			return fmt.Errorf("invalid Span field: %#v", keyIface)
		}

		switch key {
		case "traceId", "trace_id":
			err = decoder.Decode(&s.TraceID)
		case "spanId", "span_id":
			err = decoder.Decode(&s.SpanID)
		case "traceState", "trace_state":
			err = decoder.Decode(&s.TraceState)
		case "parentSpanId", "parent_span_id":
			err = decoder.Decode(&s.ParentSpanID)
		case "flags":
			err = decoder.Decode(&s.Flags)
		case "name":
			err = decoder.Decode(&s.Name)
		case "kind":
			err = decoder.Decode(&s.Kind)
		case "startTimeUnixNano", "start_time_unix_nano":
			var val protoUint64
			err = decoder.Decode(&val)
			s.StartTime = time.Unix(0, int64(val.Uint64()))
		case "endTimeUnixNano", "end_time_unix_nano":
			var val protoUint64
			err = decoder.Decode(&val)
			s.EndTime = time.Unix(0, int64(val.Uint64()))
		case "attributes":
			err = decoder.Decode(&s.Attrs)
		case "droppedAttributesCount", "dropped_attributes_count":
			err = decoder.Decode(&s.DroppedAttrs)
		case "events":
			err = decoder.Decode(&s.Events)
		case "droppedEventsCount", "dropped_events_count":
			err = decoder.Decode(&s.DroppedEvents)
		case "links":
			err = decoder.Decode(&s.Links)
		case "droppedLinksCount", "dropped_links_count":
			err = decoder.Decode(&s.DroppedLinks)
		case "status":
			err = decoder.Decode(&s.Status)
		default:
			// Skip unknown.
		}

		if err != nil {
			return err
		}
	}
	return nil
}

// SpanFlags represents constants used to interpret the
// Span.flags field, which is protobuf 'fixed32' type and is to
// be used as bit-fields. Each non-zero value defined in this enum is
// a bit-mask.  To extract the bit-field, for example, use an
// expression like:
//
//	(span.flags & SPAN_FLAGS_TRACE_FLAGS_MASK)
//
// See https://www.w3.org/TR/trace-context-2/#trace-flags for the flag definitions.
//
// Note that Span flags were introduced in version 1.1 of the
// OpenTelemetry protocol.  Older Span producers do not set this
// field, consequently consumers should not rely on the absence of a
// particular flag bit to indicate the presence of a particular feature.
type SpanFlags int32

const (
	// Bits 0-7 are used for trace flags.
	SpanFlagsTraceFlagsMask SpanFlags = 255
	// Bits 8 and 9 are used to indicate that the parent span or link span is remote.
	// Bit 8 (`HAS_IS_REMOTE`) indicates whether the value is known.
	// Bit 9 (`IS_REMOTE`) indicates whether the span or link is remote.
	SpanFlagsContextHasIsRemoteMask SpanFlags = 256
	// SpanFlagsContextHasIsRemoteMask indicates the Span is remote.
	SpanFlagsContextIsRemoteMask SpanFlags = 512
)

// SpanKind is the type of span. Can be used to specify additional relationships between spans
// in addition to a parent/child relationship.
type SpanKind int32

const (
	// Indicates that the span represents an internal operation within an application,
	// as opposed to an operation happening at the boundaries. Default value.
	SpanKindInternal SpanKind = 1
	// Indicates that the span covers server-side handling of an RPC or other
	// remote network request.
	SpanKindServer SpanKind = 2
	// Indicates that the span describes a request to some remote service.
	SpanKindClient SpanKind = 3
	// Indicates that the span describes a producer sending a message to a broker.
	// Unlike CLIENT and SERVER, there is often no direct critical path latency relationship
	// between producer and consumer spans. A PRODUCER span ends when the message was accepted
	// by the broker while the logical processing of the message might span a much longer time.
	SpanKindProducer SpanKind = 4
	// Indicates that the span describes consumer receiving a message from a broker.
	// Like the PRODUCER kind, there is often no direct critical path latency relationship
	// between producer and consumer spans.
	SpanKindConsumer SpanKind = 5
)

// Event is a time-stamped annotation of the span, consisting of user-supplied
// text description and key-value pairs.
type SpanEvent struct {
	// time_unix_nano is the time the event occurred.
	Time time.Time `json:"timeUnixNano,omitempty"`
	// name of the event.
	// This field is semantically required to be set to non-empty string.
	Name string `json:"name,omitempty"`
	// attributes is a collection of attribute key/value pairs on the event.
	// Attribute keys MUST be unique (it is not allowed to have more than one
	// attribute with the same key).
	Attrs []Attr `json:"attributes,omitempty"`
	// dropped_attributes_count is the number of dropped attributes. If the value is 0,
	// then no attributes were dropped.
	DroppedAttrs uint32 `json:"droppedAttributesCount,omitempty"`
}

// MarshalJSON encodes e into OTLP formatted JSON.
func (e SpanEvent) MarshalJSON() ([]byte, error) {
	t := e.Time.UnixNano()
	if e.Time.IsZero() || t < 0 {
		t = 0
	}

	type Alias SpanEvent
	return json.Marshal(struct {
		Alias
		Time uint64 `json:"timeUnixNano,omitempty"`
	}{
		Alias: Alias(e),
		Time:  uint64(t),
	})
}

// UnmarshalJSON decodes the OTLP formatted JSON contained in data into se.
func (se *SpanEvent) UnmarshalJSON(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))

	t, err := decoder.Token()
	if err != nil {
		return err
	}
	if t != json.Delim('{') {
		return errors.New("invalid SpanEvent type")
	}

	for decoder.More() {
		keyIface, err := decoder.Token()
		if err != nil {
			if errors.Is(err, io.EOF) {
				// Empty.
				return nil
			}
			return err
		}

		key, ok := keyIface.(string)
		if !ok {
			return fmt.Errorf("invalid SpanEvent field: %#v", keyIface)
		}

		switch key {
		case "timeUnixNano", "time_unix_nano":
			var val protoUint64
			err = decoder.Decode(&val)
			se.Time = time.Unix(0, int64(val.Uint64()))
		case "name":
			err = decoder.Decode(&se.Name)
		case "attributes":
			err = decoder.Decode(&se.Attrs)
		case "droppedAttributesCount", "dropped_attributes_count":
			err = decoder.Decode(&se.DroppedAttrs)
		default:
			// Skip unknown.
		}

		if err != nil {
			return err
		}
	}
	return nil
}

// A pointer from the current span to another span in the same trace or in a
// different trace. For example, this can be used in batching operations,
// where a single batch handler processes multiple requests from different
// traces or when the handler receives a request from a different project.
type SpanLink struct {
	// A unique identifier of a trace that this linked span is part of. The ID is a
	// 16-byte array.
	TraceID TraceID `json:"traceId,omitempty"`
	// A unique identifier for the linked span. The ID is an 8-byte array.
	SpanID SpanID `json:"spanId,omitempty"`
	// The trace_state associated with the link.
	TraceState string `json:"traceState,omitempty"`
	// attributes is a collection of attribute key/value pairs on the link.
	// Attribute keys MUST be unique (it is not allowed to have more than one
	// attribute with the same key).
	Attrs []Attr `json:"attributes,omitempty"`
	// dropped_attributes_count is the number of dropped attributes. If the value is 0,
	// then no attributes were dropped.
	DroppedAttrs uint32 `json:"droppedAttributesCount,omitempty"`
	// Flags, a bit field.
	//
	// Bits 0-7 (8 least significant bits) are the trace flags as defined in W3C Trace
	// Context specification. To read the 8-bit W3C trace flag, use
	// `flags & SPAN_FLAGS_TRACE_FLAGS_MASK`.
	//
	// See https://www.w3.org/TR/trace-context-2/#trace-flags for the flag definitions.
	//
	// Bits 8 and 9 represent the 3 states of whether the link is remote.
	// The states are (unknown, is not remote, is remote).
	// To read whether the value is known, use `(flags & SPAN_FLAGS_CONTEXT_HAS_IS_REMOTE_MASK) != 0`.
	// To read whether the link is remote, use `(flags & SPAN_FLAGS_CONTEXT_IS_REMOTE_MASK) != 0`.
	//
	// Readers MUST NOT assume that bits 10-31 (22 most significant bits) will be zero.
	// When creating new spans, bits 10-31 (most-significant 22-bits) MUST be zero.
	//
	// [Optional].
	Flags uint32 `json:"flags,omitempty"`
}

// UnmarshalJSON decodes the OTLP formatted JSON contained in data into sl.
func (sl *SpanLink) UnmarshalJSON(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))

	t, err := decoder.Token()
	if err != nil {
		return err
	}
	if t != json.Delim('{') {
		return errors.New("invalid SpanLink type")
	}

	for decoder.More() {
		keyIface, err := decoder.Token()
		if err != nil {
			if errors.Is(err, io.EOF) {
				// Empty.
				return nil
			}
			return err
		}

		key, ok := keyIface.(string)
		if !ok {
			return fmt.Errorf("invalid SpanLink field: %#v", keyIface)
		}

		switch key {
		case "traceId", "trace_id":
			err = decoder.Decode(&sl.TraceID)
		case "spanId", "span_id":
			err = decoder.Decode(&sl.SpanID)
		case "traceState", "trace_state":
			err = decoder.Decode(&sl.TraceState)
		case "attributes":
			err = decoder.Decode(&sl.Attrs)
		case "droppedAttributesCount", "dropped_attributes_count":
			err = decoder.Decode(&sl.DroppedAttrs)
		case "flags":
			err = decoder.Decode(&sl.Flags)
		default:
			// Skip unknown.
		}

		if err != nil {
			return err
		}
	}
	return nil
}
