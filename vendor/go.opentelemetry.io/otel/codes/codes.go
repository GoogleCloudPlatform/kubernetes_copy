// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package codes // import "go.opentelemetry.io/otel/codes"

import (
	"encoding/json"
	"fmt"
	"strconv"
)

const (
	// Unset is the default status code.
	Unset Code = 0

	// Error indicates the operation contains an error.
	//
	// NOTE: The error code in OTLP is 2.
	// The value of this enum is only relevant to the internals
	// of the Go SDK.
	Error Code = 1

	// Ok indicates operation has been validated by an Application developers
	// or Operator to have completed successfully, or contain no error.
	//
	// NOTE: The Ok code in OTLP is 1.
	// The value of this enum is only relevant to the internals
	// of the Go SDK.
	Ok Code = 2

	maxCode = 3
)

// Code is an 32-bit representation of a status state.
type Code uint32

var codeToStr = map[Code]string{
	Unset: "Unset",
	Error: "Error",
	Ok:    "Ok",
}

var strToCode = map[string]Code{
	`"Unset"`: Unset,
	`"Error"`: Error,
	`"Ok"`:    Ok,
}

// String returns the Code as a string.
func (c Code) String() string {
	return codeToStr[c]
}

// UnmarshalJSON unmarshals b into the Code.
//
// This is based on the functionality in the gRPC codes package:
// https://github.com/grpc/grpc-go/blob/bb64fee312b46ebee26be43364a7a966033521b1/codes/codes.go#L218-L244
func (c *Code) UnmarshalJSON(b []byte) error {
	// From json.Unmarshaler: By convention, to approximate the behavior of
	// Unmarshal itself, Unmarshalers implement UnmarshalJSON([]byte("null")) as
	// a no-op.
	if string(b) == "null" {
		return nil
	}
	if c == nil {
		return fmt.Errorf("nil receiver passed to UnmarshalJSON")
	}

	var x interface{}
	if err := json.Unmarshal(b, &x); err != nil {
		return err
	}
	switch x.(type) {
	case string:
		if jc, ok := strToCode[string(b)]; ok {
			*c = jc
			return nil
		}
		return fmt.Errorf("invalid code: %q", string(b))
	case float64:
		if ci, err := strconv.ParseUint(string(b), 10, 32); err == nil {
			if ci >= maxCode {
				return fmt.Errorf("invalid code: %q", ci)
			}

			*c = Code(ci) // nolint: gosec  // Bit size of 32 check above.
			return nil
		}
		return fmt.Errorf("invalid code: %q", string(b))
	default:
		return fmt.Errorf("invalid code: %q", string(b))
	}
}

// MarshalJSON returns c as the JSON encoding of c.
func (c *Code) MarshalJSON() ([]byte, error) {
	if c == nil {
		return []byte("null"), nil
	}
	str, ok := codeToStr[*c]
	if !ok {
		return nil, fmt.Errorf("invalid code: %d", *c)
	}
	return []byte(fmt.Sprintf("%q", str)), nil
}
