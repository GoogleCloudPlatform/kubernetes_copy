// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package trace // import "go.opentelemetry.io/otel/trace"

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
)

const (
	// FlagsSampled is a bitmask with the sampled bit set. A SpanContext
	// with the sampling bit set means the span is sampled.
	FlagsSampled = TraceFlags(0x01)

	errInvalidHexID errorConst = "trace-id and span-id can only contain [0-9a-f] characters, all lowercase"

	errInvalidTraceIDLength errorConst = "hex encoded trace-id must have length equals to 32"
	errNilTraceID           errorConst = "trace-id can't be all zero"

	errInvalidSpanIDLength errorConst = "hex encoded span-id must have length equals to 16"
	errNilSpanID           errorConst = "span-id can't be all zero"
)

type errorConst string

func (e errorConst) Error() string {
	return string(e)
}

// TraceID is a unique identity of a trace.
// nolint:revive // revive complains about stutter of `trace.TraceID`.
type TraceID [16]byte

var (
	nilTraceID TraceID
	_          json.Marshaler = nilTraceID
)

// IsValid checks whether the trace TraceID is valid. A valid trace ID does
// not consist of zeros only.
func (t TraceID) IsValid() bool {
	return !bytes.Equal(t[:], nilTraceID[:])
}

// MarshalJSON implements a custom marshal function to encode TraceID
// as a hex string.
func (t TraceID) MarshalJSON() ([]byte, error) {
	return json.Marshal(t.String())
}

// String returns the hex string representation form of a TraceID.
func (t TraceID) String() string {
	return hex.EncodeToString(t[:])
}

// SpanID is a unique identity of a span in a trace.
type SpanID [8]byte

var (
	nilSpanID SpanID
	_         json.Marshaler = nilSpanID
)

// IsValid checks whether the SpanID is valid. A valid SpanID does not consist
// of zeros only.
func (s SpanID) IsValid() bool {
	return !bytes.Equal(s[:], nilSpanID[:])
}

// MarshalJSON implements a custom marshal function to encode SpanID
// as a hex string.
func (s SpanID) MarshalJSON() ([]byte, error) {
	return json.Marshal(s.String())
}

// String returns the hex string representation form of a SpanID.
func (s SpanID) String() string {
	return hex.EncodeToString(s[:])
}

// TraceIDFromHex returns a TraceID from a hex string if it is compliant with
// the W3C trace-context specification.  See more at
// https://www.w3.org/TR/trace-context/#trace-id
// nolint:revive // revive complains about stutter of `trace.TraceIDFromHex`.
func TraceIDFromHex(h string) (TraceID, error) {
	t := TraceID{}
	if len(h) != 32 {
		return t, errInvalidTraceIDLength
	}

	if err := decodeHex(h, t[:]); err != nil {
		return t, err
	}

	if !t.IsValid() {
		return t, errNilTraceID
	}
	return t, nil
}

// SpanIDFromHex returns a SpanID from a hex string if it is compliant
// with the w3c trace-context specification.
// See more at https://www.w3.org/TR/trace-context/#parent-id
func SpanIDFromHex(h string) (SpanID, error) {
	s := SpanID{}
	if len(h) != 16 {
		return s, errInvalidSpanIDLength
	}

	if err := decodeHex(h, s[:]); err != nil {
		return s, err
	}

	if !s.IsValid() {
		return s, errNilSpanID
	}
	return s, nil
}

func decodeHex(h string, b []byte) error {
	for _, r := range h {
		switch {
		case 'a' <= r && r <= 'f':
			continue
		case '0' <= r && r <= '9':
			continue
		default:
			return errInvalidHexID
		}
	}

	decoded, err := hex.DecodeString(h)
	if err != nil {
		return err
	}

	copy(b, decoded)
	return nil
}

// TraceFlags contains flags that can be set on a SpanContext.
type TraceFlags byte //nolint:revive // revive complains about stutter of `trace.TraceFlags`.

// IsSampled returns if the sampling bit is set in the TraceFlags.
func (tf TraceFlags) IsSampled() bool {
	return tf&FlagsSampled == FlagsSampled
}

// WithSampled sets the sampling bit in a new copy of the TraceFlags.
func (tf TraceFlags) WithSampled(sampled bool) TraceFlags { // nolint:revive  // sampled is not a control flag.
	if sampled {
		return tf | FlagsSampled
	}

	return tf &^ FlagsSampled
}

// MarshalJSON implements a custom marshal function to encode TraceFlags
// as a hex string.
func (tf TraceFlags) MarshalJSON() ([]byte, error) {
	return json.Marshal(tf.String())
}

// String returns the hex string representation form of TraceFlags.
func (tf TraceFlags) String() string {
	return hex.EncodeToString([]byte{byte(tf)}[:])
}

// SpanContextConfig contains mutable fields usable for constructing
// an immutable SpanContext.
type SpanContextConfig struct {
	TraceID    TraceID
	SpanID     SpanID
	TraceFlags TraceFlags
	TraceState TraceState
	Remote     bool
}

// NewSpanContext constructs a SpanContext using values from the provided
// SpanContextConfig.
func NewSpanContext(config SpanContextConfig) SpanContext {
	return SpanContext{
		traceID:    config.TraceID,
		spanID:     config.SpanID,
		traceFlags: config.TraceFlags,
		traceState: config.TraceState,
		remote:     config.Remote,
	}
}

// SpanContext contains identifying trace information about a Span.
type SpanContext struct {
	traceID    TraceID
	spanID     SpanID
	traceFlags TraceFlags
	traceState TraceState
	remote     bool
}

var _ json.Marshaler = SpanContext{}

// IsValid returns if the SpanContext is valid. A valid span context has a
// valid TraceID and SpanID.
func (sc SpanContext) IsValid() bool {
	return sc.HasTraceID() && sc.HasSpanID()
}

// IsRemote indicates whether the SpanContext represents a remotely-created Span.
func (sc SpanContext) IsRemote() bool {
	return sc.remote
}

// WithRemote returns a copy of sc with the Remote property set to remote.
func (sc SpanContext) WithRemote(remote bool) SpanContext {
	return SpanContext{
		traceID:    sc.traceID,
		spanID:     sc.spanID,
		traceFlags: sc.traceFlags,
		traceState: sc.traceState,
		remote:     remote,
	}
}

// TraceID returns the TraceID from the SpanContext.
func (sc SpanContext) TraceID() TraceID {
	return sc.traceID
}

// HasTraceID checks if the SpanContext has a valid TraceID.
func (sc SpanContext) HasTraceID() bool {
	return sc.traceID.IsValid()
}

// WithTraceID returns a new SpanContext with the TraceID replaced.
func (sc SpanContext) WithTraceID(traceID TraceID) SpanContext {
	return SpanContext{
		traceID:    traceID,
		spanID:     sc.spanID,
		traceFlags: sc.traceFlags,
		traceState: sc.traceState,
		remote:     sc.remote,
	}
}

// SpanID returns the SpanID from the SpanContext.
func (sc SpanContext) SpanID() SpanID {
	return sc.spanID
}

// HasSpanID checks if the SpanContext has a valid SpanID.
func (sc SpanContext) HasSpanID() bool {
	return sc.spanID.IsValid()
}

// WithSpanID returns a new SpanContext with the SpanID replaced.
func (sc SpanContext) WithSpanID(spanID SpanID) SpanContext {
	return SpanContext{
		traceID:    sc.traceID,
		spanID:     spanID,
		traceFlags: sc.traceFlags,
		traceState: sc.traceState,
		remote:     sc.remote,
	}
}

// TraceFlags returns the flags from the SpanContext.
func (sc SpanContext) TraceFlags() TraceFlags {
	return sc.traceFlags
}

// IsSampled returns if the sampling bit is set in the SpanContext's TraceFlags.
func (sc SpanContext) IsSampled() bool {
	return sc.traceFlags.IsSampled()
}

// WithTraceFlags returns a new SpanContext with the TraceFlags replaced.
func (sc SpanContext) WithTraceFlags(flags TraceFlags) SpanContext {
	return SpanContext{
		traceID:    sc.traceID,
		spanID:     sc.spanID,
		traceFlags: flags,
		traceState: sc.traceState,
		remote:     sc.remote,
	}
}

// TraceState returns the TraceState from the SpanContext.
func (sc SpanContext) TraceState() TraceState {
	return sc.traceState
}

// WithTraceState returns a new SpanContext with the TraceState replaced.
func (sc SpanContext) WithTraceState(state TraceState) SpanContext {
	return SpanContext{
		traceID:    sc.traceID,
		spanID:     sc.spanID,
		traceFlags: sc.traceFlags,
		traceState: state,
		remote:     sc.remote,
	}
}

// Equal is a predicate that determines whether two SpanContext values are equal.
func (sc SpanContext) Equal(other SpanContext) bool {
	return sc.traceID == other.traceID &&
		sc.spanID == other.spanID &&
		sc.traceFlags == other.traceFlags &&
		sc.traceState.String() == other.traceState.String() &&
		sc.remote == other.remote
}

// MarshalJSON implements a custom marshal function to encode a SpanContext.
func (sc SpanContext) MarshalJSON() ([]byte, error) {
	return json.Marshal(SpanContextConfig{
		TraceID:    sc.traceID,
		SpanID:     sc.spanID,
		TraceFlags: sc.traceFlags,
		TraceState: sc.traceState,
		Remote:     sc.remote,
	})
}
