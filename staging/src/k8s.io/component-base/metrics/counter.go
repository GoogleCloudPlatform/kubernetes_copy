/*
Copyright 2019 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package metrics

import (
	"context"
	"sync"

	"github.com/blang/semver/v4"
	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/trace"

	dto "github.com/prometheus/client_model/go"
)

// Counter is our internal representation for our wrapping struct around prometheus
// counters. Counter implements both kubeCollector and CounterMetric.
type Counter struct {
	CounterMetric
	*CounterOpts
	lazyMetric
	selfCollector
}

type CounterWithContext struct {
	*Counter
	ctx context.Context
}

// The implementation of the Metric interface is expected by testutil.GetCounterMetricValue.
var _ Metric = &Counter{}

// All supported exemplar metric types implement the metricWithExemplar interface.
var _ metricWithExemplar = &CounterWithContext{}

// NewCounter returns an object which satisfies the kubeCollector and CounterMetric interfaces.
// However, the object returned will not measure anything unless the collector is first
// registered, since the metric is lazily instantiated.
func NewCounter(opts *CounterOpts) *Counter {
	opts.StabilityLevel.setDefaults()

	kc := &Counter{
		CounterOpts: opts,
		lazyMetric:  lazyMetric{stabilityLevel: opts.StabilityLevel},
	}
	kc.setPrometheusCounter(noop)
	kc.lazyInit(kc, BuildFQName(opts.Namespace, opts.Subsystem, opts.Name))
	return kc
}

func (c *Counter) Desc() *prometheus.Desc {
	return c.metric.Desc()
}

func (c *Counter) Write(to *dto.Metric) error {
	return c.metric.Write(to)
}

// Reset resets the underlying prometheus Counter to start counting from 0 again
func (c *Counter) Reset() {
	if !c.IsCreated() {
		return
	}
	c.setPrometheusCounter(prometheus.NewCounter(c.CounterOpts.toPromCounterOpts()))
}

// setPrometheusCounter sets the underlying CounterMetric object, i.e. the thing that does the measurement.
func (c *Counter) setPrometheusCounter(counter prometheus.Counter) {
	c.CounterMetric = counter
	c.initSelfCollection(counter)
}

// DeprecatedVersion returns a pointer to the Version or nil
func (c *Counter) DeprecatedVersion() *semver.Version {
	return parseSemver(c.CounterOpts.DeprecatedVersion)
}

// initializeMetric invocation creates the actual underlying Counter. Until this method is called
// the underlying counter is a no-op.
func (c *Counter) initializeMetric() {
	c.CounterOpts.annotateStabilityLevel()
	// this actually creates the underlying prometheus counter.
	c.setPrometheusCounter(prometheus.NewCounter(c.CounterOpts.toPromCounterOpts()))
}

// initializeDeprecatedMetric invocation creates the actual (but deprecated) Counter. Until this method
// is called the underlying counter is a no-op.
func (c *Counter) initializeDeprecatedMetric() {
	c.CounterOpts.markDeprecated()
	c.initializeMetric()
}

// WithContext allows the normal Counter metric to pass in context.
func (c *Counter) WithContext(ctx context.Context) *CounterWithContext {
	return &CounterWithContext{
		ctx:     ctx,
		Counter: c,
	}
}

// withExemplar attaches an exemplar to the metric.
func (c *CounterWithContext) withExemplar(v float64) {
	if m, ok := c.CounterMetric.(prometheus.ExemplarAdder); ok {
		maybeSpanCtx := trace.SpanContextFromContext(c.ctx)
		if maybeSpanCtx.IsValid() && maybeSpanCtx.IsSampled() {
			exemplarLabels := prometheus.Labels{
				"trace_id": maybeSpanCtx.TraceID().String(),
				"span_id":  maybeSpanCtx.SpanID().String(),
			}
			m.AddWithExemplar(v, exemplarLabels)
			return
		}
	}

	c.CounterMetric.Add(v)
}

func (c *Counter) Add(v float64) {
	c.CounterMetric.Add(v)
}

func (c *Counter) Inc() {
	c.CounterMetric.Inc()
}

func (c *CounterWithContext) Add(v float64) {
	c.withExemplar(v)
}

func (c *CounterWithContext) Inc() {
	c.withExemplar(1)
}

// CounterVec is the internal representation of our wrapping struct around prometheus
// counterVecs. CounterVec implements both kubeCollector and CounterVecMetric.
type CounterVec struct {
	*prometheus.CounterVec
	*CounterOpts
	lazyMetric
	originalLabels []string
}

var _ kubeCollector = &CounterVec{}

// TODO: make this true: var _ CounterVecMetric = &CounterVec{}

// NewCounterVec returns an object which satisfies the kubeCollector and (almost) CounterVecMetric interfaces.
// However, the object returned will not measure anything unless the collector is first
// registered, since the metric is lazily instantiated, and only members extracted after
// registration will actually measure anything.
func NewCounterVec(opts *CounterOpts, labels []string) *CounterVec {
	opts.StabilityLevel.setDefaults()

	fqName := BuildFQName(opts.Namespace, opts.Subsystem, opts.Name)

	cv := &CounterVec{
		CounterVec:     noopCounterVec,
		CounterOpts:    opts,
		originalLabels: labels,
		lazyMetric:     lazyMetric{stabilityLevel: opts.StabilityLevel},
	}
	cv.lazyInit(cv, fqName)
	return cv
}

// DeprecatedVersion returns a pointer to the Version or nil
func (v *CounterVec) DeprecatedVersion() *semver.Version {
	return parseSemver(v.CounterOpts.DeprecatedVersion)

}

// initializeMetric invocation creates the actual underlying CounterVec. Until this method is called
// the underlying counterVec is a no-op.
func (v *CounterVec) initializeMetric() {
	v.CounterOpts.annotateStabilityLevel()
	v.CounterVec = prometheus.NewCounterVec(v.CounterOpts.toPromCounterOpts(), v.originalLabels)
}

// initializeDeprecatedMetric invocation creates the actual (but deprecated) CounterVec. Until this method is called
// the underlying counterVec is a no-op.
func (v *CounterVec) initializeDeprecatedMetric() {
	v.CounterOpts.markDeprecated()
	v.initializeMetric()
}

// Default Prometheus Vec behavior is that member extraction results in creation of a new element
// if one with the unique label values is not found in the underlying stored metricMap.
// This means  that if this function is called but the underlying metric is not registered
// (which means it will never be exposed externally nor consumed), the metric will exist in memory
// for perpetuity (i.e. throughout application lifecycle).
//
// For reference: https://github.com/prometheus/client_golang/blob/v0.9.2/prometheus/counter.go#L179-L197
//
// In contrast, the Vec behavior in this package is that member extraction before registration
// returns a permanent noop object.

// WithLabelValues returns the Counter for the given slice of label
// values (same order as the VariableLabels in Desc). If that combination of
// label values is accessed for the first time, a new Counter is created IFF the counterVec
// has been registered to a metrics registry.
func (v *CounterVec) WithLabelValues(lvs ...string) CounterMetric {
	if !v.IsCreated() {
		return noop // return no-op counter
	}
	if v.LabelValueAllowLists != nil {
		v.LabelValueAllowLists.ConstrainToAllowedList(v.originalLabels, lvs)
	} else {
		v.initializeLabelAllowListsOnce.Do(func() {
			allowListLock.RLock()
			if allowList, ok := labelValueAllowLists[v.FQName()]; ok {
				v.LabelValueAllowLists = allowList
				allowList.ConstrainToAllowedList(v.originalLabels, lvs)
			}
			allowListLock.RUnlock()
		})
	}

	return v.CounterVec.WithLabelValues(lvs...)
}

// With returns the Counter for the given Labels map (the label names
// must match those of the VariableLabels in Desc). If that label map is
// accessed for the first time, a new Counter is created IFF the counterVec has
// been registered to a metrics registry.
func (v *CounterVec) With(labels map[string]string) CounterMetric {
	if !v.IsCreated() {
		return noop // return no-op counter
	}
	if v.LabelValueAllowLists != nil {
		v.LabelValueAllowLists.ConstrainLabelMap(labels)
	} else {
		v.initializeLabelAllowListsOnce.Do(func() {
			allowListLock.RLock()
			if allowList, ok := labelValueAllowLists[v.FQName()]; ok {
				v.LabelValueAllowLists = allowList
				allowList.ConstrainLabelMap(labels)
			}
			allowListLock.RUnlock()
		})
	}
	return v.CounterVec.With(labels)
}

// Delete deletes the metric where the variable labels are the same as those
// passed in as labels. It returns true if a metric was deleted.
//
// It is not an error if the number and names of the Labels are inconsistent
// with those of the VariableLabels in Desc. However, such inconsistent Labels
// can never match an actual metric, so the method will always return false in
// that case.
func (v *CounterVec) Delete(labels map[string]string) bool {
	if !v.IsCreated() {
		return false // since we haven't created the metric, we haven't deleted a metric with the passed in values
	}
	return v.CounterVec.Delete(labels)
}

// Reset deletes all metrics in this vector.
func (v *CounterVec) Reset() {
	if !v.IsCreated() {
		return
	}

	v.CounterVec.Reset()
}

// ResetLabelAllowLists resets the label allow list for the CounterVec.
// NOTE: This should only be used in test.
func (v *CounterVec) ResetLabelAllowLists() {
	v.initializeLabelAllowListsOnce = sync.Once{}
	v.LabelValueAllowLists = nil
}

// WithContext returns wrapped CounterVec with context
func (v *CounterVec) WithContext(ctx context.Context) *CounterVecWithContext {
	return &CounterVecWithContext{
		ctx:        ctx,
		CounterVec: v,
	}
}

// CounterVecWithContext is the wrapper of CounterVec with context.
type CounterVecWithContext struct {
	*CounterVec
	ctx context.Context
}

// CounterVecWithContextWithCounter is the wrapper of CounterVecWithContext with counter.
type CounterVecWithContextWithCounter struct {
	*CounterVecWithContext
	counter CounterMetric
}

// withExemplar attaches an exemplar to the metric.
func (vcc *CounterVecWithContextWithCounter) withExemplar(v float64) {
	if m, ok := vcc.counter.(prometheus.ExemplarAdder); ok {
		maybeSpanCtx := trace.SpanContextFromContext(vcc.ctx)
		if maybeSpanCtx.IsValid() && maybeSpanCtx.IsSampled() {
			exemplarLabels := prometheus.Labels{
				"trace_id": maybeSpanCtx.TraceID().String(),
				"span_id":  maybeSpanCtx.SpanID().String(),
			}
			m.AddWithExemplar(v, exemplarLabels)
			return
		}
	}

	vcc.counter.Add(v)
}

// Add adds the given value to the counter with the provided labels.
func (vcc *CounterVecWithContextWithCounter) Add(v float64) {
	vcc.withExemplar(v)
}

// Inc increments the counter with the provided labels.
func (vcc *CounterVecWithContextWithCounter) Inc() {
	vcc.withExemplar(1)
}

// WithLabelValues is the wrapper of CounterVec.WithLabelValues.
func (vc *CounterVecWithContext) WithLabelValues(lvs ...string) *CounterVecWithContextWithCounter {
	return &CounterVecWithContextWithCounter{
		CounterVecWithContext: vc,
		counter:               vc.CounterVec.WithLabelValues(lvs...),
	}
}

// With is the wrapper of CounterVec.With.
func (vc *CounterVecWithContext) With(labels map[string]string) *CounterVecWithContextWithCounter {
	return &CounterVecWithContextWithCounter{
		CounterVecWithContext: vc,
		counter:               vc.CounterVec.With(labels),
	}
}
