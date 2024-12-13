// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package otelrestful // import "go.opentelemetry.io/contrib/instrumentation/github.com/emicklei/go-restful/otelrestful"

import (
	"net/http"

	"go.opentelemetry.io/otel/propagation"
	oteltrace "go.opentelemetry.io/otel/trace"
)

// config is used to configure the go-restful middleware.
type config struct {
	TracerProvider   oteltrace.TracerProvider
	Propagators      propagation.TextMapPropagator
	PublicEndpoint   bool
	PublicEndpointFn func(*http.Request) bool
}

// Option applies a configuration value.
type Option interface {
	apply(*config)
}

type optionFunc func(*config)

func (o optionFunc) apply(c *config) {
	o(c)
}

// WithPublicEndpoint configures the Handler to link the span with an incoming
// span context. If this option is not provided, then the association is a child
// association instead of a link.
func WithPublicEndpoint() Option {
	return optionFunc(func(c *config) {
		c.PublicEndpoint = true
	})
}

// WithPropagators specifies propagators to use for extracting
// information from the HTTP requests. If none are specified, global
// ones will be used.
func WithPropagators(propagators propagation.TextMapPropagator) Option {
	return optionFunc(func(cfg *config) {
		if propagators != nil {
			cfg.Propagators = propagators
		}
	})
}

// WithTracerProvider specifies a tracer provider to use for creating a tracer.
// If none is specified, the global provider is used.
func WithTracerProvider(provider oteltrace.TracerProvider) Option {
	return optionFunc(func(cfg *config) {
		if provider != nil {
			cfg.TracerProvider = provider
		}
	})
}

// WithPublicEndpointFn runs with every request, and allows conditionally
// configuring the Handler to link the span with an incoming span context. If
// this option is not provided or returns false, then the association is a
// child association instead of a link.
// Note: [WithPublicEndpoint] takes precedence over WithPublicEndpointFn.
func WithPublicEndpointFn(fn func(*http.Request) bool) Option {
	return optionFunc(func(c *config) {
		c.PublicEndpointFn = fn
	})
}
