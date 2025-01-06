// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package otelrestful // import "go.opentelemetry.io/contrib/instrumentation/github.com/emicklei/go-restful/otelrestful"

import (
	"github.com/emicklei/go-restful/v3"

	"go.opentelemetry.io/contrib/instrumentation/github.com/emicklei/go-restful/otelrestful/internal/semconvutil"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.20.0"
	oteltrace "go.opentelemetry.io/otel/trace"
)

// ScopeName is the instrumentation scope name.
const ScopeName = "go.opentelemetry.io/contrib/instrumentation/github.com/emicklei/go-restful/otelrestful"

// OTelFilter returns a restful.FilterFunction which will trace an incoming request.
//
// The service parameter should describe the name of the (virtual) server handling
// the request.  Options can be applied to configure the tracer and propagators
// used for this filter.
func OTelFilter(service string, opts ...Option) restful.FilterFunction {
	cfg := config{}
	for _, opt := range opts {
		opt.apply(&cfg)
	}
	if cfg.TracerProvider == nil {
		cfg.TracerProvider = otel.GetTracerProvider()
	}
	tracer := cfg.TracerProvider.Tracer(
		ScopeName,
		oteltrace.WithInstrumentationVersion(Version()),
	)
	if cfg.Propagators == nil {
		cfg.Propagators = otel.GetTextMapPropagator()
	}
	return func(req *restful.Request, resp *restful.Response, chain *restful.FilterChain) {
		r := req.Request
		ctx := cfg.Propagators.Extract(r.Context(), propagation.HeaderCarrier(r.Header))
		route := req.SelectedRoutePath()
		spanName := route

		opts := []oteltrace.SpanStartOption{
			oteltrace.WithAttributes(semconvutil.HTTPServerRequest(service, r)...),
			oteltrace.WithSpanKind(oteltrace.SpanKindServer),
		}
		if route != "" {
			rAttr := semconv.HTTPRoute(route)
			opts = append(opts, oteltrace.WithAttributes(rAttr))
		}

		if cfg.PublicEndpoint || (cfg.PublicEndpointFn != nil && cfg.PublicEndpointFn(r.WithContext(ctx))) {
			opts = append(opts, oteltrace.WithNewRoot())
			// Linking incoming span context if any for public endpoint.
			if s := oteltrace.SpanContextFromContext(ctx); s.IsValid() && s.IsRemote() {
				opts = append(opts, oteltrace.WithLinks(oteltrace.Link{SpanContext: s}))
			}
		}

		ctx, span := tracer.Start(ctx, spanName, opts...)
		defer span.End()

		// pass the span through the request context
		req.Request = req.Request.WithContext(ctx)

		chain.ProcessFilter(req, resp)

		status := resp.StatusCode()
		span.SetStatus(semconvutil.HTTPServerStatus(status))
		if status > 0 {
			span.SetAttributes(semconv.HTTPStatusCode(status))
		}
	}
}
