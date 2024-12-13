// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package trace // import "go.opentelemetry.io/otel/trace"

import "context"

type traceContextKeyType int

const currentSpanKey traceContextKeyType = iota

// ContextWithSpan returns a copy of parent with span set as the current Span.
func ContextWithSpan(parent context.Context, span Span) context.Context {
	return context.WithValue(parent, currentSpanKey, span)
}

// ContextWithSpanContext returns a copy of parent with sc as the current
// Span. The Span implementation that wraps sc is non-recording and performs
// no operations other than to return sc as the SpanContext from the
// SpanContext method.
func ContextWithSpanContext(parent context.Context, sc SpanContext) context.Context {
	return ContextWithSpan(parent, nonRecordingSpan{sc: sc})
}

// ContextWithRemoteSpanContext returns a copy of parent with rsc set explicitly
// as a remote SpanContext and as the current Span. The Span implementation
// that wraps rsc is non-recording and performs no operations other than to
// return rsc as the SpanContext from the SpanContext method.
func ContextWithRemoteSpanContext(parent context.Context, rsc SpanContext) context.Context {
	return ContextWithSpanContext(parent, rsc.WithRemote(true))
}

// SpanFromContext returns the current Span from ctx.
//
// If no Span is currently set in ctx an implementation of a Span that
// performs no operations is returned.
func SpanFromContext(ctx context.Context) Span {
	if ctx == nil {
		return noopSpanInstance
	}
	if span, ok := ctx.Value(currentSpanKey).(Span); ok {
		return span
	}
	return noopSpanInstance
}

// SpanContextFromContext returns the current Span's SpanContext.
func SpanContextFromContext(ctx context.Context) SpanContext {
	return SpanFromContext(ctx).SpanContext()
}
