// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package sdk

import (
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// TracerProvider returns an auto-instrumentable [trace.TracerProvider].
//
// If an [go.opentelemetry.io/auto.Instrumentation] is configured to instrument
// the process using the returned TracerProvider, all of the telemetry it
// produces will be processed and handled by that Instrumentation. By default,
// if no Instrumentation instruments the TracerProvider it will not generate
// any trace telemetry.
func TracerProvider() trace.TracerProvider { return tracerProviderInstance }

var tracerProviderInstance = new(tracerProvider)

type tracerProvider struct{ noop.TracerProvider }

var _ trace.TracerProvider = tracerProvider{}

func (p tracerProvider) Tracer(name string, opts ...trace.TracerOption) trace.Tracer {
	cfg := trace.NewTracerConfig(opts...)
	return tracer{
		name:      name,
		version:   cfg.InstrumentationVersion(),
		schemaURL: cfg.SchemaURL(),
	}
}
