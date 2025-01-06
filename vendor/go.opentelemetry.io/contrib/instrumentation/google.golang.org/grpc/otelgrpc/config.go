// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package otelgrpc // import "go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"

import (
	"google.golang.org/grpc/stats"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.17.0"
	"go.opentelemetry.io/otel/trace"
)

const (
	// ScopeName is the instrumentation scope name.
	ScopeName = "go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	// GRPCStatusCodeKey is convention for numeric status code of a gRPC request.
	GRPCStatusCodeKey = attribute.Key("rpc.grpc.status_code")
)

// InterceptorFilter is a predicate used to determine whether a given request in
// interceptor info should be instrumented. A InterceptorFilter must return true if
// the request should be traced.
//
// Deprecated: Use stats handlers instead.
type InterceptorFilter func(*InterceptorInfo) bool

// Filter is a predicate used to determine whether a given request in
// should be instrumented by the attached RPC tag info.
// A Filter must return true if the request should be instrumented.
type Filter func(*stats.RPCTagInfo) bool

// config is a group of options for this instrumentation.
type config struct {
	Filter            Filter
	InterceptorFilter InterceptorFilter
	Propagators       propagation.TextMapPropagator
	TracerProvider    trace.TracerProvider
	MeterProvider     metric.MeterProvider
	SpanStartOptions  []trace.SpanStartOption
	SpanAttributes    []attribute.KeyValue
	MetricAttributes  []attribute.KeyValue

	ReceivedEvent bool
	SentEvent     bool

	tracer trace.Tracer
	meter  metric.Meter

	rpcDuration    metric.Float64Histogram
	rpcInBytes     metric.Int64Histogram
	rpcOutBytes    metric.Int64Histogram
	rpcInMessages  metric.Int64Histogram
	rpcOutMessages metric.Int64Histogram
}

// Option applies an option value for a config.
type Option interface {
	apply(*config)
}

// newConfig returns a config configured with all the passed Options.
func newConfig(opts []Option, role string) *config {
	c := &config{
		Propagators:    otel.GetTextMapPropagator(),
		TracerProvider: otel.GetTracerProvider(),
		MeterProvider:  otel.GetMeterProvider(),
	}
	for _, o := range opts {
		o.apply(c)
	}

	c.tracer = c.TracerProvider.Tracer(
		ScopeName,
		trace.WithInstrumentationVersion(SemVersion()),
	)

	c.meter = c.MeterProvider.Meter(
		ScopeName,
		metric.WithInstrumentationVersion(Version()),
		metric.WithSchemaURL(semconv.SchemaURL),
	)

	var err error
	c.rpcDuration, err = c.meter.Float64Histogram("rpc."+role+".duration",
		metric.WithDescription("Measures the duration of inbound RPC."),
		metric.WithUnit("ms"))
	if err != nil {
		otel.Handle(err)
		if c.rpcDuration == nil {
			c.rpcDuration = noop.Float64Histogram{}
		}
	}

	rpcRequestSize, err := c.meter.Int64Histogram("rpc."+role+".request.size",
		metric.WithDescription("Measures size of RPC request messages (uncompressed)."),
		metric.WithUnit("By"))
	if err != nil {
		otel.Handle(err)
		if rpcRequestSize == nil {
			rpcRequestSize = noop.Int64Histogram{}
		}
	}

	rpcResponseSize, err := c.meter.Int64Histogram("rpc."+role+".response.size",
		metric.WithDescription("Measures size of RPC response messages (uncompressed)."),
		metric.WithUnit("By"))
	if err != nil {
		otel.Handle(err)
		if rpcResponseSize == nil {
			rpcResponseSize = noop.Int64Histogram{}
		}
	}

	rpcRequestsPerRPC, err := c.meter.Int64Histogram("rpc."+role+".requests_per_rpc",
		metric.WithDescription("Measures the number of messages received per RPC. Should be 1 for all non-streaming RPCs."),
		metric.WithUnit("{count}"))
	if err != nil {
		otel.Handle(err)
		if rpcRequestsPerRPC == nil {
			rpcRequestsPerRPC = noop.Int64Histogram{}
		}
	}

	rpcResponsesPerRPC, err := c.meter.Int64Histogram("rpc."+role+".responses_per_rpc",
		metric.WithDescription("Measures the number of messages received per RPC. Should be 1 for all non-streaming RPCs."),
		metric.WithUnit("{count}"))
	if err != nil {
		otel.Handle(err)
		if rpcResponsesPerRPC == nil {
			rpcResponsesPerRPC = noop.Int64Histogram{}
		}
	}

	switch role {
	case "client":
		c.rpcInBytes = rpcResponseSize
		c.rpcInMessages = rpcResponsesPerRPC
		c.rpcOutBytes = rpcRequestSize
		c.rpcOutMessages = rpcRequestsPerRPC
	case "server":
		c.rpcInBytes = rpcRequestSize
		c.rpcInMessages = rpcRequestsPerRPC
		c.rpcOutBytes = rpcResponseSize
		c.rpcOutMessages = rpcResponsesPerRPC
	default:
		c.rpcInBytes = noop.Int64Histogram{}
		c.rpcInMessages = noop.Int64Histogram{}
		c.rpcOutBytes = noop.Int64Histogram{}
		c.rpcOutMessages = noop.Int64Histogram{}
	}

	return c
}

type propagatorsOption struct{ p propagation.TextMapPropagator }

func (o propagatorsOption) apply(c *config) {
	if o.p != nil {
		c.Propagators = o.p
	}
}

// WithPropagators returns an Option to use the Propagators when extracting
// and injecting trace context from requests.
func WithPropagators(p propagation.TextMapPropagator) Option {
	return propagatorsOption{p: p}
}

type tracerProviderOption struct{ tp trace.TracerProvider }

func (o tracerProviderOption) apply(c *config) {
	if o.tp != nil {
		c.TracerProvider = o.tp
	}
}

// WithInterceptorFilter returns an Option to use the request filter.
//
// Deprecated: Use stats handlers instead.
func WithInterceptorFilter(f InterceptorFilter) Option {
	return interceptorFilterOption{f: f}
}

type interceptorFilterOption struct {
	f InterceptorFilter
}

func (o interceptorFilterOption) apply(c *config) {
	if o.f != nil {
		c.InterceptorFilter = o.f
	}
}

// WithFilter returns an Option to use the request filter.
func WithFilter(f Filter) Option {
	return filterOption{f: f}
}

type filterOption struct {
	f Filter
}

func (o filterOption) apply(c *config) {
	if o.f != nil {
		c.Filter = o.f
	}
}

// WithTracerProvider returns an Option to use the TracerProvider when
// creating a Tracer.
func WithTracerProvider(tp trace.TracerProvider) Option {
	return tracerProviderOption{tp: tp}
}

type meterProviderOption struct{ mp metric.MeterProvider }

func (o meterProviderOption) apply(c *config) {
	if o.mp != nil {
		c.MeterProvider = o.mp
	}
}

// WithMeterProvider returns an Option to use the MeterProvider when
// creating a Meter. If this option is not provide the global MeterProvider will be used.
func WithMeterProvider(mp metric.MeterProvider) Option {
	return meterProviderOption{mp: mp}
}

// Event type that can be recorded, see WithMessageEvents.
type Event int

// Different types of events that can be recorded, see WithMessageEvents.
const (
	ReceivedEvents Event = iota
	SentEvents
)

type messageEventsProviderOption struct {
	events []Event
}

func (m messageEventsProviderOption) apply(c *config) {
	for _, e := range m.events {
		switch e {
		case ReceivedEvents:
			c.ReceivedEvent = true
		case SentEvents:
			c.SentEvent = true
		}
	}
}

// WithMessageEvents configures the Handler to record the specified events
// (span.AddEvent) on spans. By default only summary attributes are added at the
// end of the request.
//
// Valid events are:
//   - ReceivedEvents: Record the number of bytes read after every gRPC read operation.
//   - SentEvents: Record the number of bytes written after every gRPC write operation.
func WithMessageEvents(events ...Event) Option {
	return messageEventsProviderOption{events: events}
}

type spanStartOption struct{ opts []trace.SpanStartOption }

func (o spanStartOption) apply(c *config) {
	c.SpanStartOptions = append(c.SpanStartOptions, o.opts...)
}

// WithSpanOptions configures an additional set of
// trace.SpanOptions, which are applied to each new span.
func WithSpanOptions(opts ...trace.SpanStartOption) Option {
	return spanStartOption{opts}
}

type spanAttributesOption struct{ a []attribute.KeyValue }

func (o spanAttributesOption) apply(c *config) {
	if o.a != nil {
		c.SpanAttributes = o.a
	}
}

// WithSpanAttributes returns an Option to add custom attributes to the spans.
func WithSpanAttributes(a ...attribute.KeyValue) Option {
	return spanAttributesOption{a: a}
}

type metricAttributesOption struct{ a []attribute.KeyValue }

func (o metricAttributesOption) apply(c *config) {
	if o.a != nil {
		c.MetricAttributes = o.a
	}
}

// WithMetricAttributes returns an Option to add custom attributes to the metrics.
func WithMetricAttributes(a ...attribute.KeyValue) Option {
	return metricAttributesOption{a: a}
}
