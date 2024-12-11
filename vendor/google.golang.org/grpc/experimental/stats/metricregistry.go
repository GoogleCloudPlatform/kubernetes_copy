/*
 *
 * Copyright 2024 gRPC authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 */

package stats

import (
	"maps"

	"google.golang.org/grpc/grpclog"
	"google.golang.org/grpc/internal"
)

func init() {
	internal.SnapshotMetricRegistryForTesting = snapshotMetricsRegistryForTesting
}

var logger = grpclog.Component("metrics-registry")

// DefaultMetrics are the default metrics registered through global metrics
// registry. This is written to at initialization time only, and is read only
// after initialization.
var DefaultMetrics = NewMetrics()

// MetricDescriptor is the data for a registered metric.
type MetricDescriptor struct {
	// The name of this metric. This name must be unique across the whole binary
	// (including any per call metrics). See
	// https://github.com/grpc/proposal/blob/master/A79-non-per-call-metrics-architecture.md#metric-instrument-naming-conventions
	// for metric naming conventions.
	Name Metric
	// The description of this metric.
	Description string
	// The unit (e.g. entries, seconds) of this metric.
	Unit string
	// The required label keys for this metric. These are intended to
	// metrics emitted from a stats handler.
	Labels []string
	// The optional label keys for this metric. These are intended to attached
	// to metrics emitted from a stats handler if configured.
	OptionalLabels []string
	// Whether this metric is on by default.
	Default bool
	// The type of metric. This is set by the metric registry, and not intended
	// to be set by a component registering a metric.
	Type MetricType
	// Bounds are the bounds of this metric. This only applies to histogram
	// metrics. If unset or set with length 0, stats handlers will fall back to
	// default bounds.
	Bounds []float64
}

// MetricType is the type of metric.
type MetricType int

// Type of metric supported by this instrument registry.
const (
	MetricTypeIntCount MetricType = iota
	MetricTypeFloatCount
	MetricTypeIntHisto
	MetricTypeFloatHisto
	MetricTypeIntGauge
)

// Int64CountHandle is a typed handle for a int count metric. This handle
// is passed at the recording point in order to know which metric to record
// on.
type Int64CountHandle MetricDescriptor

// Descriptor returns the int64 count handle typecast to a pointer to a
// MetricDescriptor.
func (h *Int64CountHandle) Descriptor() *MetricDescriptor {
	return (*MetricDescriptor)(h)
}

// Record records the int64 count value on the metrics recorder provided.
func (h *Int64CountHandle) Record(recorder MetricsRecorder, incr int64, labels ...string) {
	recorder.RecordInt64Count(h, incr, labels...)
}

// Float64CountHandle is a typed handle for a float count metric. This handle is
// passed at the recording point in order to know which metric to record on.
type Float64CountHandle MetricDescriptor

// Descriptor returns the float64 count handle typecast to a pointer to a
// MetricDescriptor.
func (h *Float64CountHandle) Descriptor() *MetricDescriptor {
	return (*MetricDescriptor)(h)
}

// Record records the float64 count value on the metrics recorder provided.
func (h *Float64CountHandle) Record(recorder MetricsRecorder, incr float64, labels ...string) {
	recorder.RecordFloat64Count(h, incr, labels...)
}

// Int64HistoHandle is a typed handle for an int histogram metric. This handle
// is passed at the recording point in order to know which metric to record on.
type Int64HistoHandle MetricDescriptor

// Descriptor returns the int64 histo handle typecast to a pointer to a
// MetricDescriptor.
func (h *Int64HistoHandle) Descriptor() *MetricDescriptor {
	return (*MetricDescriptor)(h)
}

// Record records the int64 histo value on the metrics recorder provided.
func (h *Int64HistoHandle) Record(recorder MetricsRecorder, incr int64, labels ...string) {
	recorder.RecordInt64Histo(h, incr, labels...)
}

// Float64HistoHandle is a typed handle for a float histogram metric. This
// handle is passed at the recording point in order to know which metric to
// record on.
type Float64HistoHandle MetricDescriptor

// Descriptor returns the float64 histo handle typecast to a pointer to a
// MetricDescriptor.
func (h *Float64HistoHandle) Descriptor() *MetricDescriptor {
	return (*MetricDescriptor)(h)
}

// Record records the float64 histo value on the metrics recorder provided.
func (h *Float64HistoHandle) Record(recorder MetricsRecorder, incr float64, labels ...string) {
	recorder.RecordFloat64Histo(h, incr, labels...)
}

// Int64GaugeHandle is a typed handle for an int gauge metric. This handle is
// passed at the recording point in order to know which metric to record on.
type Int64GaugeHandle MetricDescriptor

// Descriptor returns the int64 gauge handle typecast to a pointer to a
// MetricDescriptor.
func (h *Int64GaugeHandle) Descriptor() *MetricDescriptor {
	return (*MetricDescriptor)(h)
}

// Record records the int64 histo value on the metrics recorder provided.
func (h *Int64GaugeHandle) Record(recorder MetricsRecorder, incr int64, labels ...string) {
	recorder.RecordInt64Gauge(h, incr, labels...)
}

// registeredMetrics are the registered metric descriptor names.
var registeredMetrics = make(map[Metric]bool)

// metricsRegistry contains all of the registered metrics.
//
// This is written to only at init time, and read only after that.
var metricsRegistry = make(map[Metric]*MetricDescriptor)

// DescriptorForMetric returns the MetricDescriptor from the global registry.
//
// Returns nil if MetricDescriptor not present.
func DescriptorForMetric(metric Metric) *MetricDescriptor {
	return metricsRegistry[metric]
}

func registerMetric(name Metric, def bool) {
	if registeredMetrics[name] {
		logger.Fatalf("metric %v already registered", name)
	}
	registeredMetrics[name] = true
	if def {
		DefaultMetrics = DefaultMetrics.Add(name)
	}
}

// RegisterInt64Count registers the metric description onto the global registry.
// It returns a typed handle to use to recording data.
//
// NOTE: this function must only be called during initialization time (i.e. in
// an init() function), and is not thread-safe. If multiple metrics are
// registered with the same name, this function will panic.
func RegisterInt64Count(descriptor MetricDescriptor) *Int64CountHandle {
	registerMetric(descriptor.Name, descriptor.Default)
	descriptor.Type = MetricTypeIntCount
	descPtr := &descriptor
	metricsRegistry[descriptor.Name] = descPtr
	return (*Int64CountHandle)(descPtr)
}

// RegisterFloat64Count registers the metric description onto the global
// registry. It returns a typed handle to use to recording data.
//
// NOTE: this function must only be called during initialization time (i.e. in
// an init() function), and is not thread-safe. If multiple metrics are
// registered with the same name, this function will panic.
func RegisterFloat64Count(descriptor MetricDescriptor) *Float64CountHandle {
	registerMetric(descriptor.Name, descriptor.Default)
	descriptor.Type = MetricTypeFloatCount
	descPtr := &descriptor
	metricsRegistry[descriptor.Name] = descPtr
	return (*Float64CountHandle)(descPtr)
}

// RegisterInt64Histo registers the metric description onto the global registry.
// It returns a typed handle to use to recording data.
//
// NOTE: this function must only be called during initialization time (i.e. in
// an init() function), and is not thread-safe. If multiple metrics are
// registered with the same name, this function will panic.
func RegisterInt64Histo(descriptor MetricDescriptor) *Int64HistoHandle {
	registerMetric(descriptor.Name, descriptor.Default)
	descriptor.Type = MetricTypeIntHisto
	descPtr := &descriptor
	metricsRegistry[descriptor.Name] = descPtr
	return (*Int64HistoHandle)(descPtr)
}

// RegisterFloat64Histo registers the metric description onto the global
// registry. It returns a typed handle to use to recording data.
//
// NOTE: this function must only be called during initialization time (i.e. in
// an init() function), and is not thread-safe. If multiple metrics are
// registered with the same name, this function will panic.
func RegisterFloat64Histo(descriptor MetricDescriptor) *Float64HistoHandle {
	registerMetric(descriptor.Name, descriptor.Default)
	descriptor.Type = MetricTypeFloatHisto
	descPtr := &descriptor
	metricsRegistry[descriptor.Name] = descPtr
	return (*Float64HistoHandle)(descPtr)
}

// RegisterInt64Gauge registers the metric description onto the global registry.
// It returns a typed handle to use to recording data.
//
// NOTE: this function must only be called during initialization time (i.e. in
// an init() function), and is not thread-safe. If multiple metrics are
// registered with the same name, this function will panic.
func RegisterInt64Gauge(descriptor MetricDescriptor) *Int64GaugeHandle {
	registerMetric(descriptor.Name, descriptor.Default)
	descriptor.Type = MetricTypeIntGauge
	descPtr := &descriptor
	metricsRegistry[descriptor.Name] = descPtr
	return (*Int64GaugeHandle)(descPtr)
}

// snapshotMetricsRegistryForTesting snapshots the global data of the metrics
// registry. Returns a cleanup function that sets the metrics registry to its
// original state.
func snapshotMetricsRegistryForTesting() func() {
	oldDefaultMetrics := DefaultMetrics
	oldRegisteredMetrics := registeredMetrics
	oldMetricsRegistry := metricsRegistry

	registeredMetrics = make(map[Metric]bool)
	metricsRegistry = make(map[Metric]*MetricDescriptor)
	maps.Copy(registeredMetrics, registeredMetrics)
	maps.Copy(metricsRegistry, metricsRegistry)

	return func() {
		DefaultMetrics = oldDefaultMetrics
		registeredMetrics = oldRegisteredMetrics
		metricsRegistry = oldMetricsRegistry
	}
}
