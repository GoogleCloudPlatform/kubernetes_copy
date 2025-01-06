// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package trace // import "go.opentelemetry.io/otel/trace"

import "go.opentelemetry.io/otel/trace/embedded"

// TracerProvider provides Tracers that are used by instrumentation code to
// trace computational workflows.
//
// A TracerProvider is the collection destination of all Spans from Tracers it
// provides, it represents a unique telemetry collection pipeline. How that
// pipeline is defined, meaning how those Spans are collected, processed, and
// where they are exported, depends on its implementation. Instrumentation
// authors do not need to define this implementation, rather just use the
// provided Tracers to instrument code.
//
// Commonly, instrumentation code will accept a TracerProvider implementation
// at runtime from its users or it can simply use the globally registered one
// (see https://pkg.go.dev/go.opentelemetry.io/otel#GetTracerProvider).
//
// Warning: Methods may be added to this interface in minor releases. See
// package documentation on API implementation for information on how to set
// default behavior for unimplemented methods.
type TracerProvider interface {
	// Users of the interface can ignore this. This embedded type is only used
	// by implementations of this interface. See the "API Implementations"
	// section of the package documentation for more information.
	embedded.TracerProvider

	// Tracer returns a unique Tracer scoped to be used by instrumentation code
	// to trace computational workflows. The scope and identity of that
	// instrumentation code is uniquely defined by the name and options passed.
	//
	// The passed name needs to uniquely identify instrumentation code.
	// Therefore, it is recommended that name is the Go package name of the
	// library providing instrumentation (note: not the code being
	// instrumented). Instrumentation libraries can have multiple versions,
	// therefore, the WithInstrumentationVersion option should be used to
	// distinguish these different codebases. Additionally, instrumentation
	// libraries may sometimes use traces to communicate different domains of
	// workflow data (i.e. using spans to communicate workflow events only). If
	// this is the case, the WithScopeAttributes option should be used to
	// uniquely identify Tracers that handle the different domains of workflow
	// data.
	//
	// If the same name and options are passed multiple times, the same Tracer
	// will be returned (it is up to the implementation if this will be the
	// same underlying instance of that Tracer or not). It is not necessary to
	// call this multiple times with the same name and options to get an
	// up-to-date Tracer. All implementations will ensure any TracerProvider
	// configuration changes are propagated to all provided Tracers.
	//
	// If name is empty, then an implementation defined default name will be
	// used instead.
	//
	// This method is safe to call concurrently.
	Tracer(name string, options ...TracerOption) Tracer
}
