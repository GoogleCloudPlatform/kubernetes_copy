// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package otelhttp // import "go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

import (
	"net/http"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// Attribute keys that can be added to a span.
const (
	ReadBytesKey  = attribute.Key("http.read_bytes")  // if anything was read from the request body, the total number of bytes read
	ReadErrorKey  = attribute.Key("http.read_error")  // If an error occurred while reading a request, the string of the error (io.EOF is not recorded)
	WroteBytesKey = attribute.Key("http.wrote_bytes") // if anything was written to the response writer, the total number of bytes written
	WriteErrorKey = attribute.Key("http.write_error") // if an error occurred while writing a reply, the string of the error (io.EOF is not recorded)
)

// Filter is a predicate used to determine whether a given http.request should
// be traced. A Filter must return true if the request should be traced.
type Filter func(*http.Request) bool

func newTracer(tp trace.TracerProvider) trace.Tracer {
	return tp.Tracer(ScopeName, trace.WithInstrumentationVersion(Version()))
}
