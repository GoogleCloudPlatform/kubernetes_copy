// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package telemetry

// For the semantics of status codes see
// https://github.com/open-telemetry/opentelemetry-specification/blob/main/specification/trace/api.md#set-status
type StatusCode int32

const (
	// The default status.
	StatusCodeUnset StatusCode = 0
	// The Span has been validated by an Application developer or Operator to
	// have completed successfully.
	StatusCodeOK StatusCode = 1
	// The Span contains an error.
	StatusCodeError StatusCode = 2
)

var statusCodeStrings = []string{
	"Unset",
	"OK",
	"Error",
}

func (s StatusCode) String() string {
	if s >= 0 && int(s) < len(statusCodeStrings) {
		return statusCodeStrings[s]
	}
	return "<unknown telemetry.StatusCode>"
}

// The Status type defines a logical error model that is suitable for different
// programming environments, including REST APIs and RPC APIs.
type Status struct {
	// A developer-facing human readable error message.
	Message string `json:"message,omitempty"`
	// The status code.
	Code StatusCode `json:"code,omitempty"`
}
