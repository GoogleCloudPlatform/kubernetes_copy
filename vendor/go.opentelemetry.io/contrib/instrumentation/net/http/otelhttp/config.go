// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package otelhttp // import "go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

import (
	"context"
	"net/http"
	"net/http/httptrace"

	"go.opentelemetry.io/otel/attribute"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// ScopeName is the instrumentation scope name.
const ScopeName = "go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

// config represents the configuration options available for the http.Handler
// and http.Transport types.
type config struct {
	ServerName        string
	Tracer            trace.Tracer
	Meter             metric.Meter
	Propagators       propagation.TextMapPropagator
	SpanStartOptions  []trace.SpanStartOption
	PublicEndpoint    bool
	PublicEndpointFn  func(*http.Request) bool
	ReadEvent         bool
	WriteEvent        bool
	Filters           []Filter
	SpanNameFormatter func(string, *http.Request) string
	ClientTrace       func(context.Context) *httptrace.ClientTrace

	TracerProvider     trace.TracerProvider
	MeterProvider      metric.MeterProvider
	MetricAttributesFn func(*http.Request) []attribute.KeyValue
}

// Option interface used for setting optional config properties.
type Option interface {
	apply(*config)
}

type optionFunc func(*config)

func (o optionFunc) apply(c *config) {
	o(c)
}

// newConfig creates a new config struct and applies opts to it.
func newConfig(opts ...Option) *config {
	c := &config{
		Propagators:   otel.GetTextMapPropagator(),
		MeterProvider: otel.GetMeterProvider(),
	}
	for _, opt := range opts {
		opt.apply(c)
	}

	// Tracer is only initialized if manually specified. Otherwise, can be passed with the tracing context.
	if c.TracerProvider != nil {
		c.Tracer = newTracer(c.TracerProvider)
	}

	c.Meter = c.MeterProvider.Meter(
		ScopeName,
		metric.WithInstrumentationVersion(Version()),
	)

	return c
}

// WithTracerProvider specifies a tracer provider to use for creating a tracer.
// If none is specified, the global provider is used.
func WithTracerProvider(provider trace.TracerProvider) Option {
	return optionFunc(func(cfg *config) {
		if provider != nil {
			cfg.TracerProvider = provider
		}
	})
}

// WithMeterProvider specifies a meter provider to use for creating a meter.
// If none is specified, the global provider is used.
func WithMeterProvider(provider metric.MeterProvider) Option {
	return optionFunc(func(cfg *config) {
		if provider != nil {
			cfg.MeterProvider = provider
		}
	})
}

// WithPublicEndpoint configures the Handler to link the span with an incoming
// span context. If this option is not provided, then the association is a child
// association instead of a link.
func WithPublicEndpoint() Option {
	return optionFunc(func(c *config) {
		c.PublicEndpoint = true
	})
}

// WithPublicEndpointFn runs with every request, and allows conditionally
// configuring the Handler to link the span with an incoming span context. If
// this option is not provided or returns false, then the association is a
// child association instead of a link.
// Note: WithPublicEndpoint takes precedence over WithPublicEndpointFn.
func WithPublicEndpointFn(fn func(*http.Request) bool) Option {
	return optionFunc(func(c *config) {
		c.PublicEndpointFn = fn
	})
}

// WithPropagators configures specific propagators. If this
// option isn't specified, then the global TextMapPropagator is used.
func WithPropagators(ps propagation.TextMapPropagator) Option {
	return optionFunc(func(c *config) {
		if ps != nil {
			c.Propagators = ps
		}
	})
}

// WithSpanOptions configures an additional set of
// trace.SpanOptions, which are applied to each new span.
func WithSpanOptions(opts ...trace.SpanStartOption) Option {
	return optionFunc(func(c *config) {
		c.SpanStartOptions = append(c.SpanStartOptions, opts...)
	})
}

// WithFilter adds a filter to the list of filters used by the handler.
// If any filter indicates to exclude a request then the request will not be
// traced. All filters must allow a request to be traced for a Span to be created.
// If no filters are provided then all requests are traced.
// Filters will be invoked for each processed request, it is advised to make them
// simple and fast.
func WithFilter(f Filter) Option {
	return optionFunc(func(c *config) {
		c.Filters = append(c.Filters, f)
	})
}

type event int

// Different types of events that can be recorded, see WithMessageEvents.
const (
	ReadEvents event = iota
	WriteEvents
)

// WithMessageEvents configures the Handler to record the specified events
// (span.AddEvent) on spans. By default only summary attributes are added at the
// end of the request.
//
// Valid events are:
//   - ReadEvents: Record the number of bytes read after every http.Request.Body.Read
//     using the ReadBytesKey
//   - WriteEvents: Record the number of bytes written after every http.ResponeWriter.Write
//     using the WriteBytesKey
func WithMessageEvents(events ...event) Option {
	return optionFunc(func(c *config) {
		for _, e := range events {
			switch e {
			case ReadEvents:
				c.ReadEvent = true
			case WriteEvents:
				c.WriteEvent = true
			}
		}
	})
}

// WithSpanNameFormatter takes a function that will be called on every
// request and the returned string will become the Span Name.
func WithSpanNameFormatter(f func(operation string, r *http.Request) string) Option {
	return optionFunc(func(c *config) {
		c.SpanNameFormatter = f
	})
}

// WithClientTrace takes a function that returns client trace instance that will be
// applied to the requests sent through the otelhttp Transport.
func WithClientTrace(f func(context.Context) *httptrace.ClientTrace) Option {
	return optionFunc(func(c *config) {
		c.ClientTrace = f
	})
}

// WithServerName returns an Option that sets the name of the (virtual) server
// handling requests.
func WithServerName(server string) Option {
	return optionFunc(func(c *config) {
		c.ServerName = server
	})
}

// WithMetricAttributesFn returns an Option to set a function that maps an HTTP request to a slice of attribute.KeyValue.
// These attributes will be included in metrics for every request.
func WithMetricAttributesFn(metricAttributesFn func(r *http.Request) []attribute.KeyValue) Option {
	return optionFunc(func(c *config) {
		c.MetricAttributesFn = metricAttributesFn
	})
}
