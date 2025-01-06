// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"encoding/hex"
	"errors"
	"fmt"
)

const (
	traceIDSize = 16
	spanIDSize  = 8
)

// TraceID is a custom data type that is used for all trace IDs.
type TraceID [traceIDSize]byte

// String returns the hex string representation form of a TraceID.
func (tid TraceID) String() string {
	return hex.EncodeToString(tid[:])
}

// IsEmpty returns false if id contains at least one non-zero byte.
func (tid TraceID) IsEmpty() bool {
	return tid == [traceIDSize]byte{}
}

// MarshalJSON converts the trace ID into a hex string enclosed in quotes.
func (tid TraceID) MarshalJSON() ([]byte, error) {
	if tid.IsEmpty() {
		return []byte(`""`), nil
	}
	return marshalJSON(tid[:])
}

// UnmarshalJSON inflates the trace ID from hex string, possibly enclosed in
// quotes.
func (tid *TraceID) UnmarshalJSON(data []byte) error {
	*tid = [traceIDSize]byte{}
	return unmarshalJSON(tid[:], data)
}

// SpanID is a custom data type that is used for all span IDs.
type SpanID [spanIDSize]byte

// String returns the hex string representation form of a SpanID.
func (sid SpanID) String() string {
	return hex.EncodeToString(sid[:])
}

// IsEmpty returns true if the span ID contains at least one non-zero byte.
func (sid SpanID) IsEmpty() bool {
	return sid == [spanIDSize]byte{}
}

// MarshalJSON converts span ID into a hex string enclosed in quotes.
func (sid SpanID) MarshalJSON() ([]byte, error) {
	if sid.IsEmpty() {
		return []byte(`""`), nil
	}
	return marshalJSON(sid[:])
}

// UnmarshalJSON decodes span ID from hex string, possibly enclosed in quotes.
func (sid *SpanID) UnmarshalJSON(data []byte) error {
	*sid = [spanIDSize]byte{}
	return unmarshalJSON(sid[:], data)
}

// marshalJSON converts id into a hex string enclosed in quotes.
func marshalJSON(id []byte) ([]byte, error) {
	// Plus 2 quote chars at the start and end.
	hexLen := hex.EncodedLen(len(id)) + 2

	b := make([]byte, hexLen)
	hex.Encode(b[1:hexLen-1], id)
	b[0], b[hexLen-1] = '"', '"'

	return b, nil
}

// unmarshalJSON inflates trace id from hex string, possibly enclosed in quotes.
func unmarshalJSON(dst []byte, src []byte) error {
	if l := len(src); l >= 2 && src[0] == '"' && src[l-1] == '"' {
		src = src[1 : l-1]
	}
	nLen := len(src)
	if nLen == 0 {
		return nil
	}

	if len(dst) != hex.DecodedLen(nLen) {
		return errors.New("invalid length for ID")
	}

	_, err := hex.Decode(dst, src)
	if err != nil {
		return fmt.Errorf("cannot unmarshal ID from string '%s': %w", string(src), err)
	}
	return nil
}
