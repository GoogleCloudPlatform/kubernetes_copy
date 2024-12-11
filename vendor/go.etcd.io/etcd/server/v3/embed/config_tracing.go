// Copyright 2021 The etcd Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package embed

import (
	"context"
	"fmt"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	tracesdk "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.17.0"
	"go.uber.org/zap"
)

const maxSamplingRatePerMillion = 1000000

func validateTracingConfig(samplingRate int) error {
	if samplingRate < 0 {
		return fmt.Errorf("tracing sampling rate must be positive")
	}
	if samplingRate > maxSamplingRatePerMillion {
		return fmt.Errorf("tracing sampling rate must be less than %d", maxSamplingRatePerMillion)
	}

	return nil
}

type tracingExporter struct {
	exporter tracesdk.SpanExporter
	opts     []otelgrpc.Option
	provider *tracesdk.TracerProvider
}

func newTracingExporter(ctx context.Context, cfg *Config) (*tracingExporter, error) {
	exporter, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithInsecure(),
		otlptracegrpc.WithEndpoint(cfg.ExperimentalDistributedTracingAddress),
	)
	if err != nil {
		return nil, err
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceNameKey.String(cfg.ExperimentalDistributedTracingServiceName),
		),
	)
	if err != nil {
		return nil, err
	}

	if resWithIDKey := determineResourceWithIDKey(cfg.ExperimentalDistributedTracingServiceInstanceID); resWithIDKey != nil {
		// Merge resources into a new
		// resource in case of duplicates.
		res, err = resource.Merge(res, resWithIDKey)
		if err != nil {
			return nil, err
		}
	}

	traceProvider := tracesdk.NewTracerProvider(
		tracesdk.WithBatcher(exporter),
		tracesdk.WithResource(res),
		tracesdk.WithSampler(
			tracesdk.ParentBased(determineSampler(cfg.ExperimentalDistributedTracingSamplingRatePerMillion)),
		),
	)

	options := []otelgrpc.Option{
		otelgrpc.WithPropagators(
			propagation.NewCompositeTextMapPropagator(
				propagation.TraceContext{},
				propagation.Baggage{},
			),
		),
		otelgrpc.WithTracerProvider(
			traceProvider,
		),
	}

	cfg.logger.Debug(
		"distributed tracing enabled",
		zap.String("address", cfg.ExperimentalDistributedTracingAddress),
		zap.String("service-name", cfg.ExperimentalDistributedTracingServiceName),
		zap.String("service-instance-id", cfg.ExperimentalDistributedTracingServiceInstanceID),
		zap.Int("sampling-rate", cfg.ExperimentalDistributedTracingSamplingRatePerMillion),
	)

	return &tracingExporter{
		exporter: exporter,
		opts:     options,
		provider: traceProvider,
	}, nil
}

func (te *tracingExporter) Close(ctx context.Context) {
	if te.provider != nil {
		te.provider.Shutdown(ctx)
	}
	if te.exporter != nil {
		te.exporter.Shutdown(ctx)
	}
}

func determineSampler(samplingRate int) tracesdk.Sampler {
	sampler := tracesdk.NeverSample()
	if samplingRate == 0 {
		return sampler
	}
	return tracesdk.TraceIDRatioBased(float64(samplingRate) / float64(maxSamplingRatePerMillion))
}

// As Tracing service Instance ID must be unique, it should
// never use the empty default string value, it's set if
// if it's a non empty string.
func determineResourceWithIDKey(serviceInstanceID string) *resource.Resource {
	if serviceInstanceID != "" {
		return resource.NewSchemaless(
			(semconv.ServiceInstanceIDKey.String(serviceInstanceID)),
		)
	}
	return nil
}
