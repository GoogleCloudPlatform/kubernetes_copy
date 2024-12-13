// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

/*
Package trace provides an implementation of the tracing part of the
OpenTelemetry API.

To participate in distributed traces a Span needs to be created for the
operation being performed as part of a traced workflow. In its simplest form:

	var tracer trace.Tracer

	func init() {
		tracer = otel.Tracer("instrumentation/package/name")
	}

	func operation(ctx context.Context) {
		var span trace.Span
		ctx, span = tracer.Start(ctx, "operation")
		defer span.End()
		// ...
	}

A Tracer is unique to the instrumentation and is used to create Spans.
Instrumentation should be designed to accept a TracerProvider from which it
can create its own unique Tracer. Alternatively, the registered global
TracerProvider from the go.opentelemetry.io/otel package can be used as
a default.

	const (
		name    = "instrumentation/package/name"
		version = "0.1.0"
	)

	type Instrumentation struct {
		tracer trace.Tracer
	}

	func NewInstrumentation(tp trace.TracerProvider) *Instrumentation {
		if tp == nil {
			tp = otel.TracerProvider()
		}
		return &Instrumentation{
			tracer: tp.Tracer(name, trace.WithInstrumentationVersion(version)),
		}
	}

	func operation(ctx context.Context, inst *Instrumentation) {
		var span trace.Span
		ctx, span = inst.tracer.Start(ctx, "operation")
		defer span.End()
		// ...
	}

# API Implementations

This package does not conform to the standard Go versioning policy; all of its
interfaces may have methods added to them without a package major version bump.
This non-standard API evolution could surprise an uninformed implementation
author. They could unknowingly build their implementation in a way that would
result in a runtime panic for their users that update to the new API.

The API is designed to help inform an instrumentation author about this
non-standard API evolution. It requires them to choose a default behavior for
unimplemented interface methods. There are three behavior choices they can
make:

  - Compilation failure
  - Panic
  - Default to another implementation

All interfaces in this API embed a corresponding interface from
[go.opentelemetry.io/otel/trace/embedded]. If an author wants the default
behavior of their implementations to be a compilation failure, signaling to
their users they need to update to the latest version of that implementation,
they need to embed the corresponding interface from
[go.opentelemetry.io/otel/trace/embedded] in their implementation. For
example,

	import "go.opentelemetry.io/otel/trace/embedded"

	type TracerProvider struct {
		embedded.TracerProvider
		// ...
	}

If an author wants the default behavior of their implementations to panic, they
can embed the API interface directly.

	import "go.opentelemetry.io/otel/trace"

	type TracerProvider struct {
		trace.TracerProvider
		// ...
	}

This option is not recommended. It will lead to publishing packages that
contain runtime panics when users update to newer versions of
[go.opentelemetry.io/otel/trace], which may be done with a transitive
dependency.

Finally, an author can embed another implementation in theirs. The embedded
implementation will be used for methods not defined by the author. For example,
an author who wants to default to silently dropping the call can use
[go.opentelemetry.io/otel/trace/noop]:

	import "go.opentelemetry.io/otel/trace/noop"

	type TracerProvider struct {
		noop.TracerProvider
		// ...
	}

It is strongly recommended that authors only embed
[go.opentelemetry.io/otel/trace/noop] if they choose this default behavior.
That implementation is the only one OpenTelemetry authors can guarantee will
fully implement all the API interfaces when a user updates their API.
*/
package trace // import "go.opentelemetry.io/otel/trace"
