// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package global // import "go.opentelemetry.io/otel/internal/global"

/*
This file contains the forwarding implementation of the TracerProvider used as
the default global instance. Prior to initialization of an SDK, Tracers
returned by the global TracerProvider will provide no-op functionality. This
means that all Span created prior to initialization are no-op Spans.

Once an SDK has been initialized, all provided no-op Tracers are swapped for
Tracers provided by the SDK defined TracerProvider. However, any Span started
prior to this initialization does not change its behavior. Meaning, the Span
remains a no-op Span.

The implementation to track and swap Tracers locks all new Tracer creation
until the swap is complete. This assumes that this operation is not
performance-critical. If that assumption is incorrect, be sure to configure an
SDK prior to any Tracer creation.
*/

import (
	"context"
	"sync"
	"sync/atomic"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/embedded"
)

// tracerProvider is a placeholder for a configured SDK TracerProvider.
//
// All TracerProvider functionality is forwarded to a delegate once
// configured.
type tracerProvider struct {
	embedded.TracerProvider

	mtx      sync.Mutex
	tracers  map[il]*tracer
	delegate trace.TracerProvider
}

// Compile-time guarantee that tracerProvider implements the TracerProvider
// interface.
var _ trace.TracerProvider = &tracerProvider{}

// setDelegate configures p to delegate all TracerProvider functionality to
// provider.
//
// All Tracers provided prior to this function call are switched out to be
// Tracers provided by provider.
//
// It is guaranteed by the caller that this happens only once.
func (p *tracerProvider) setDelegate(provider trace.TracerProvider) {
	p.mtx.Lock()
	defer p.mtx.Unlock()

	p.delegate = provider

	if len(p.tracers) == 0 {
		return
	}

	for _, t := range p.tracers {
		t.setDelegate(provider)
	}

	p.tracers = nil
}

// Tracer implements TracerProvider.
func (p *tracerProvider) Tracer(name string, opts ...trace.TracerOption) trace.Tracer {
	p.mtx.Lock()
	defer p.mtx.Unlock()

	if p.delegate != nil {
		return p.delegate.Tracer(name, opts...)
	}

	// At this moment it is guaranteed that no sdk is installed, save the tracer in the tracers map.

	c := trace.NewTracerConfig(opts...)
	key := il{
		name:    name,
		version: c.InstrumentationVersion(),
		schema:  c.SchemaURL(),
		attrs:   c.InstrumentationAttributes(),
	}

	if p.tracers == nil {
		p.tracers = make(map[il]*tracer)
	}

	if val, ok := p.tracers[key]; ok {
		return val
	}

	t := &tracer{name: name, opts: opts, provider: p}
	p.tracers[key] = t
	return t
}

type il struct {
	name    string
	version string
	schema  string
	attrs   attribute.Set
}

// tracer is a placeholder for a trace.Tracer.
//
// All Tracer functionality is forwarded to a delegate once configured.
// Otherwise, all functionality is forwarded to a NoopTracer.
type tracer struct {
	embedded.Tracer

	name     string
	opts     []trace.TracerOption
	provider *tracerProvider

	delegate atomic.Value
}

// Compile-time guarantee that tracer implements the trace.Tracer interface.
var _ trace.Tracer = &tracer{}

// setDelegate configures t to delegate all Tracer functionality to Tracers
// created by provider.
//
// All subsequent calls to the Tracer methods will be passed to the delegate.
//
// It is guaranteed by the caller that this happens only once.
func (t *tracer) setDelegate(provider trace.TracerProvider) {
	t.delegate.Store(provider.Tracer(t.name, t.opts...))
}

// Start implements trace.Tracer by forwarding the call to t.delegate if
// set, otherwise it forwards the call to a NoopTracer.
func (t *tracer) Start(ctx context.Context, name string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	delegate := t.delegate.Load()
	if delegate != nil {
		return delegate.(trace.Tracer).Start(ctx, name, opts...)
	}

	s := nonRecordingSpan{sc: trace.SpanContextFromContext(ctx), tracer: t}
	ctx = trace.ContextWithSpan(ctx, s)
	return ctx, s
}

// nonRecordingSpan is a minimal implementation of a Span that wraps a
// SpanContext. It performs no operations other than to return the wrapped
// SpanContext.
type nonRecordingSpan struct {
	embedded.Span

	sc     trace.SpanContext
	tracer *tracer
}

var _ trace.Span = nonRecordingSpan{}

// SpanContext returns the wrapped SpanContext.
func (s nonRecordingSpan) SpanContext() trace.SpanContext { return s.sc }

// IsRecording always returns false.
func (nonRecordingSpan) IsRecording() bool { return false }

// SetStatus does nothing.
func (nonRecordingSpan) SetStatus(codes.Code, string) {}

// SetError does nothing.
func (nonRecordingSpan) SetError(bool) {}

// SetAttributes does nothing.
func (nonRecordingSpan) SetAttributes(...attribute.KeyValue) {}

// End does nothing.
func (nonRecordingSpan) End(...trace.SpanEndOption) {}

// RecordError does nothing.
func (nonRecordingSpan) RecordError(error, ...trace.EventOption) {}

// AddEvent does nothing.
func (nonRecordingSpan) AddEvent(string, ...trace.EventOption) {}

// AddLink does nothing.
func (nonRecordingSpan) AddLink(trace.Link) {}

// SetName does nothing.
func (nonRecordingSpan) SetName(string) {}

func (s nonRecordingSpan) TracerProvider() trace.TracerProvider { return s.tracer.provider }
