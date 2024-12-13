// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package otelhttp // import "go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

import (
	"net/http"
	"time"

	"github.com/felixge/httpsnoop"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp/internal/request"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp/internal/semconv"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// middleware is an http middleware which wraps the next handler in a span.
type middleware struct {
	operation string
	server    string

	tracer            trace.Tracer
	propagators       propagation.TextMapPropagator
	spanStartOptions  []trace.SpanStartOption
	readEvent         bool
	writeEvent        bool
	filters           []Filter
	spanNameFormatter func(string, *http.Request) string
	publicEndpoint    bool
	publicEndpointFn  func(*http.Request) bool

	semconv semconv.HTTPServer
}

func defaultHandlerFormatter(operation string, _ *http.Request) string {
	return operation
}

// NewHandler wraps the passed handler in a span named after the operation and
// enriches it with metrics.
func NewHandler(handler http.Handler, operation string, opts ...Option) http.Handler {
	return NewMiddleware(operation, opts...)(handler)
}

// NewMiddleware returns a tracing and metrics instrumentation middleware.
// The handler returned by the middleware wraps a handler
// in a span named after the operation and enriches it with metrics.
func NewMiddleware(operation string, opts ...Option) func(http.Handler) http.Handler {
	h := middleware{
		operation: operation,
	}

	defaultOpts := []Option{
		WithSpanOptions(trace.WithSpanKind(trace.SpanKindServer)),
		WithSpanNameFormatter(defaultHandlerFormatter),
	}

	c := newConfig(append(defaultOpts, opts...)...)
	h.configure(c)

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h.serveHTTP(w, r, next)
		})
	}
}

func (h *middleware) configure(c *config) {
	h.tracer = c.Tracer
	h.propagators = c.Propagators
	h.spanStartOptions = c.SpanStartOptions
	h.readEvent = c.ReadEvent
	h.writeEvent = c.WriteEvent
	h.filters = c.Filters
	h.spanNameFormatter = c.SpanNameFormatter
	h.publicEndpoint = c.PublicEndpoint
	h.publicEndpointFn = c.PublicEndpointFn
	h.server = c.ServerName
	h.semconv = semconv.NewHTTPServer(c.Meter)
}

// serveHTTP sets up tracing and calls the given next http.Handler with the span
// context injected into the request context.
func (h *middleware) serveHTTP(w http.ResponseWriter, r *http.Request, next http.Handler) {
	requestStartTime := time.Now()
	for _, f := range h.filters {
		if !f(r) {
			// Simply pass through to the handler if a filter rejects the request
			next.ServeHTTP(w, r)
			return
		}
	}

	ctx := h.propagators.Extract(r.Context(), propagation.HeaderCarrier(r.Header))
	opts := []trace.SpanStartOption{
		trace.WithAttributes(h.semconv.RequestTraceAttrs(h.server, r)...),
	}

	opts = append(opts, h.spanStartOptions...)
	if h.publicEndpoint || (h.publicEndpointFn != nil && h.publicEndpointFn(r.WithContext(ctx))) {
		opts = append(opts, trace.WithNewRoot())
		// Linking incoming span context if any for public endpoint.
		if s := trace.SpanContextFromContext(ctx); s.IsValid() && s.IsRemote() {
			opts = append(opts, trace.WithLinks(trace.Link{SpanContext: s}))
		}
	}

	tracer := h.tracer

	if tracer == nil {
		if span := trace.SpanFromContext(r.Context()); span.SpanContext().IsValid() {
			tracer = newTracer(span.TracerProvider())
		} else {
			tracer = newTracer(otel.GetTracerProvider())
		}
	}

	if startTime := StartTimeFromContext(ctx); !startTime.IsZero() {
		opts = append(opts, trace.WithTimestamp(startTime))
		requestStartTime = startTime
	}

	ctx, span := tracer.Start(ctx, h.spanNameFormatter(h.operation, r), opts...)
	defer span.End()

	readRecordFunc := func(int64) {}
	if h.readEvent {
		readRecordFunc = func(n int64) {
			span.AddEvent("read", trace.WithAttributes(ReadBytesKey.Int64(n)))
		}
	}

	// if request body is nil or NoBody, we don't want to mutate the body as it
	// will affect the identity of it in an unforeseeable way because we assert
	// ReadCloser fulfills a certain interface and it is indeed nil or NoBody.
	bw := request.NewBodyWrapper(r.Body, readRecordFunc)
	if r.Body != nil && r.Body != http.NoBody {
		r.Body = bw
	}

	writeRecordFunc := func(int64) {}
	if h.writeEvent {
		writeRecordFunc = func(n int64) {
			span.AddEvent("write", trace.WithAttributes(WroteBytesKey.Int64(n)))
		}
	}

	rww := request.NewRespWriterWrapper(w, writeRecordFunc)

	// Wrap w to use our ResponseWriter methods while also exposing
	// other interfaces that w may implement (http.CloseNotifier,
	// http.Flusher, http.Hijacker, http.Pusher, io.ReaderFrom).

	w = httpsnoop.Wrap(w, httpsnoop.Hooks{
		Header: func(httpsnoop.HeaderFunc) httpsnoop.HeaderFunc {
			return rww.Header
		},
		Write: func(httpsnoop.WriteFunc) httpsnoop.WriteFunc {
			return rww.Write
		},
		WriteHeader: func(httpsnoop.WriteHeaderFunc) httpsnoop.WriteHeaderFunc {
			return rww.WriteHeader
		},
		Flush: func(httpsnoop.FlushFunc) httpsnoop.FlushFunc {
			return rww.Flush
		},
	})

	labeler, found := LabelerFromContext(ctx)
	if !found {
		ctx = ContextWithLabeler(ctx, labeler)
	}

	next.ServeHTTP(w, r.WithContext(ctx))

	statusCode := rww.StatusCode()
	bytesWritten := rww.BytesWritten()
	span.SetStatus(h.semconv.Status(statusCode))
	span.SetAttributes(h.semconv.ResponseTraceAttrs(semconv.ResponseTelemetry{
		StatusCode: statusCode,
		ReadBytes:  bw.BytesRead(),
		ReadError:  bw.Error(),
		WriteBytes: bytesWritten,
		WriteError: rww.Error(),
	})...)

	// Use floating point division here for higher precision (instead of Millisecond method).
	elapsedTime := float64(time.Since(requestStartTime)) / float64(time.Millisecond)

	h.semconv.RecordMetrics(ctx, semconv.ServerMetricData{
		ServerName:   h.server,
		ResponseSize: bytesWritten,
		MetricAttributes: semconv.MetricAttributes{
			Req:                  r,
			StatusCode:           statusCode,
			AdditionalAttributes: labeler.Get(),
		},
		MetricData: semconv.MetricData{
			RequestSize: bw.BytesRead(),
			ElapsedTime: elapsedTime,
		},
	})
}

// WithRouteTag annotates spans and metrics with the provided route name
// with HTTP route attribute.
func WithRouteTag(route string, h http.Handler) http.Handler {
	attr := semconv.NewHTTPServer(nil).Route(route)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		span := trace.SpanFromContext(r.Context())
		span.SetAttributes(attr)

		labeler, _ := LabelerFromContext(r.Context())
		labeler.Add(attr)

		h.ServeHTTP(w, r)
	})
}
