// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package otelhttp // import "go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

import (
	"context"
	"io"
	"net/http"
	"net/http/httptrace"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp/internal/request"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp/internal/semconv"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"

	"go.opentelemetry.io/otel/trace"
)

// Transport implements the http.RoundTripper interface and wraps
// outbound HTTP(S) requests with a span and enriches it with metrics.
type Transport struct {
	rt http.RoundTripper

	tracer             trace.Tracer
	propagators        propagation.TextMapPropagator
	spanStartOptions   []trace.SpanStartOption
	filters            []Filter
	spanNameFormatter  func(string, *http.Request) string
	clientTrace        func(context.Context) *httptrace.ClientTrace
	metricAttributesFn func(*http.Request) []attribute.KeyValue

	semconv semconv.HTTPClient
}

var _ http.RoundTripper = &Transport{}

// NewTransport wraps the provided http.RoundTripper with one that
// starts a span, injects the span context into the outbound request headers,
// and enriches it with metrics.
//
// If the provided http.RoundTripper is nil, http.DefaultTransport will be used
// as the base http.RoundTripper.
func NewTransport(base http.RoundTripper, opts ...Option) *Transport {
	if base == nil {
		base = http.DefaultTransport
	}

	t := Transport{
		rt: base,
	}

	defaultOpts := []Option{
		WithSpanOptions(trace.WithSpanKind(trace.SpanKindClient)),
		WithSpanNameFormatter(defaultTransportFormatter),
	}

	c := newConfig(append(defaultOpts, opts...)...)
	t.applyConfig(c)

	return &t
}

func (t *Transport) applyConfig(c *config) {
	t.tracer = c.Tracer
	t.propagators = c.Propagators
	t.spanStartOptions = c.SpanStartOptions
	t.filters = c.Filters
	t.spanNameFormatter = c.SpanNameFormatter
	t.clientTrace = c.ClientTrace
	t.semconv = semconv.NewHTTPClient(c.Meter)
	t.metricAttributesFn = c.MetricAttributesFn
}

func defaultTransportFormatter(_ string, r *http.Request) string {
	return "HTTP " + r.Method
}

// RoundTrip creates a Span and propagates its context via the provided request's headers
// before handing the request to the configured base RoundTripper. The created span will
// end when the response body is closed or when a read from the body returns io.EOF.
func (t *Transport) RoundTrip(r *http.Request) (*http.Response, error) {
	requestStartTime := time.Now()
	for _, f := range t.filters {
		if !f(r) {
			// Simply pass through to the base RoundTripper if a filter rejects the request
			return t.rt.RoundTrip(r)
		}
	}

	tracer := t.tracer

	if tracer == nil {
		if span := trace.SpanFromContext(r.Context()); span.SpanContext().IsValid() {
			tracer = newTracer(span.TracerProvider())
		} else {
			tracer = newTracer(otel.GetTracerProvider())
		}
	}

	opts := append([]trace.SpanStartOption{}, t.spanStartOptions...) // start with the configured options

	ctx, span := tracer.Start(r.Context(), t.spanNameFormatter("", r), opts...)

	if t.clientTrace != nil {
		ctx = httptrace.WithClientTrace(ctx, t.clientTrace(ctx))
	}

	labeler, found := LabelerFromContext(ctx)
	if !found {
		ctx = ContextWithLabeler(ctx, labeler)
	}

	r = r.Clone(ctx) // According to RoundTripper spec, we shouldn't modify the origin request.

	// if request body is nil or NoBody, we don't want to mutate the body as it
	// will affect the identity of it in an unforeseeable way because we assert
	// ReadCloser fulfills a certain interface and it is indeed nil or NoBody.
	bw := request.NewBodyWrapper(r.Body, func(int64) {})
	if r.Body != nil && r.Body != http.NoBody {
		r.Body = bw
	}

	span.SetAttributes(t.semconv.RequestTraceAttrs(r)...)
	t.propagators.Inject(ctx, propagation.HeaderCarrier(r.Header))

	res, err := t.rt.RoundTrip(r)
	if err != nil {
		// set error type attribute if the error is part of the predefined
		// error types.
		// otherwise, record it as an exception
		if errType := t.semconv.ErrorType(err); errType.Valid() {
			span.SetAttributes(errType)
		} else {
			span.RecordError(err)
		}

		span.SetStatus(codes.Error, err.Error())
		span.End()
		return res, err
	}

	// metrics
	metricOpts := t.semconv.MetricOptions(semconv.MetricAttributes{
		Req:                  r,
		StatusCode:           res.StatusCode,
		AdditionalAttributes: append(labeler.Get(), t.metricAttributesFromRequest(r)...),
	})

	// For handling response bytes we leverage a callback when the client reads the http response
	readRecordFunc := func(n int64) {
		t.semconv.RecordResponseSize(ctx, n, metricOpts.AddOptions())
	}

	// traces
	span.SetAttributes(t.semconv.ResponseTraceAttrs(res)...)
	span.SetStatus(t.semconv.Status(res.StatusCode))

	res.Body = newWrappedBody(span, readRecordFunc, res.Body)

	// Use floating point division here for higher precision (instead of Millisecond method).
	elapsedTime := float64(time.Since(requestStartTime)) / float64(time.Millisecond)

	t.semconv.RecordMetrics(ctx, semconv.MetricData{
		RequestSize: bw.BytesRead(),
		ElapsedTime: elapsedTime,
	}, metricOpts)

	return res, nil
}

func (t *Transport) metricAttributesFromRequest(r *http.Request) []attribute.KeyValue {
	var attributeForRequest []attribute.KeyValue
	if t.metricAttributesFn != nil {
		attributeForRequest = t.metricAttributesFn(r)
	}
	return attributeForRequest
}

// newWrappedBody returns a new and appropriately scoped *wrappedBody as an
// io.ReadCloser. If the passed body implements io.Writer, the returned value
// will implement io.ReadWriteCloser.
func newWrappedBody(span trace.Span, record func(n int64), body io.ReadCloser) io.ReadCloser {
	// The successful protocol switch responses will have a body that
	// implement an io.ReadWriteCloser. Ensure this interface type continues
	// to be satisfied if that is the case.
	if _, ok := body.(io.ReadWriteCloser); ok {
		return &wrappedBody{span: span, record: record, body: body}
	}

	// Remove the implementation of the io.ReadWriteCloser and only implement
	// the io.ReadCloser.
	return struct{ io.ReadCloser }{&wrappedBody{span: span, record: record, body: body}}
}

// wrappedBody is the response body type returned by the transport
// instrumentation to complete a span. Errors encountered when using the
// response body are recorded in span tracking the response.
//
// The span tracking the response is ended when this body is closed.
//
// If the response body implements the io.Writer interface (i.e. for
// successful protocol switches), the wrapped body also will.
type wrappedBody struct {
	span     trace.Span
	recorded atomic.Bool
	record   func(n int64)
	body     io.ReadCloser
	read     atomic.Int64
}

var _ io.ReadWriteCloser = &wrappedBody{}

func (wb *wrappedBody) Write(p []byte) (int, error) {
	// This will not panic given the guard in newWrappedBody.
	n, err := wb.body.(io.Writer).Write(p)
	if err != nil {
		wb.span.RecordError(err)
		wb.span.SetStatus(codes.Error, err.Error())
	}
	return n, err
}

func (wb *wrappedBody) Read(b []byte) (int, error) {
	n, err := wb.body.Read(b)
	// Record the number of bytes read
	wb.read.Add(int64(n))

	switch err {
	case nil:
		// nothing to do here but fall through to the return
	case io.EOF:
		wb.recordBytesRead()
		wb.span.End()
	default:
		wb.span.RecordError(err)
		wb.span.SetStatus(codes.Error, err.Error())
	}
	return n, err
}

// recordBytesRead is a function that ensures the number of bytes read is recorded once and only once.
func (wb *wrappedBody) recordBytesRead() {
	// note: it is more performant (and equally correct) to use atomic.Bool over sync.Once here. In the event that
	// two goroutines are racing to call this method, the number of bytes read will no longer increase. Using
	// CompareAndSwap allows later goroutines to return quickly and not block waiting for the race winner to finish
	// calling wb.record(wb.read.Load()).
	if wb.recorded.CompareAndSwap(false, true) {
		// Record the total number of bytes read
		wb.record(wb.read.Load())
	}
}

func (wb *wrappedBody) Close() error {
	wb.recordBytesRead()
	wb.span.End()
	if wb.body != nil {
		return wb.body.Close()
	}
	return nil
}
