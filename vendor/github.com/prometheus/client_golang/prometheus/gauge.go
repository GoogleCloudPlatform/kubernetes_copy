// Copyright 2014 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package prometheus

import (
	"math"
	"sync/atomic"
	"time"

	dto "github.com/prometheus/client_model/go"
)

// Gauge is a Metric that represents a single numerical value that can
// arbitrarily go up and down.
//
// A Gauge is typically used for measured values like temperatures or current
// memory usage, but also "counts" that can go up and down, like the number of
// running goroutines.
//
// To create Gauge instances, use NewGauge.
type Gauge interface {
	Metric
	Collector

	// Set sets the Gauge to an arbitrary value.
	Set(float64)
	// Inc increments the Gauge by 1. Use Add to increment it by arbitrary
	// values.
	Inc()
	// Dec decrements the Gauge by 1. Use Sub to decrement it by arbitrary
	// values.
	Dec()
	// Add adds the given value to the Gauge. (The value can be negative,
	// resulting in a decrease of the Gauge.)
	Add(float64)
	// Sub subtracts the given value from the Gauge. (The value can be
	// negative, resulting in an increase of the Gauge.)
	Sub(float64)

	// SetToCurrentTime sets the Gauge to the current Unix time in seconds.
	SetToCurrentTime()
}

// GaugeOpts is an alias for Opts. See there for doc comments.
type GaugeOpts Opts

// GaugeVecOpts bundles the options to create a GaugeVec metric.
// It is mandatory to set GaugeOpts, see there for mandatory fields. VariableLabels
// is optional and can safely be left to its default value.
type GaugeVecOpts struct {
	GaugeOpts

	// VariableLabels are used to partition the metric vector by the given set
	// of labels. Each label value will be constrained with the optional Constraint
	// function, if provided.
	VariableLabels ConstrainableLabels
}

// NewGauge creates a new Gauge based on the provided GaugeOpts.
//
// The returned implementation is optimized for a fast Set method. If you have a
// choice for managing the value of a Gauge via Set vs. Inc/Dec/Add/Sub, pick
// the former. For example, the Inc method of the returned Gauge is slower than
// the Inc method of a Counter returned by NewCounter. This matches the typical
// scenarios for Gauges and Counters, where the former tends to be Set-heavy and
// the latter Inc-heavy.
func NewGauge(opts GaugeOpts) Gauge {
	desc := NewDesc(
		BuildFQName(opts.Namespace, opts.Subsystem, opts.Name),
		opts.Help,
		nil,
		opts.ConstLabels,
	)
	result := &gauge{desc: desc, labelPairs: desc.constLabelPairs}
	result.init(result) // Init self-collection.
	return result
}

type gauge struct {
	// valBits contains the bits of the represented float64 value. It has
	// to go first in the struct to guarantee alignment for atomic
	// operations.  http://golang.org/pkg/sync/atomic/#pkg-note-BUG
	valBits uint64

	selfCollector

	desc       *Desc
	labelPairs []*dto.LabelPair
}

func (g *gauge) Desc() *Desc {
	return g.desc
}

func (g *gauge) Set(val float64) {
	atomic.StoreUint64(&g.valBits, math.Float64bits(val))
}

func (g *gauge) SetToCurrentTime() {
	g.Set(float64(time.Now().UnixNano()) / 1e9)
}

func (g *gauge) Inc() {
	g.Add(1)
}

func (g *gauge) Dec() {
	g.Add(-1)
}

func (g *gauge) Add(val float64) {
	atomicUpdateFloat(&g.valBits, func(oldVal float64) float64 {
		return oldVal + val
	})
}

func (g *gauge) Sub(val float64) {
	g.Add(val * -1)
}

func (g *gauge) Write(out *dto.Metric) error {
	val := math.Float64frombits(atomic.LoadUint64(&g.valBits))
	return populateMetric(GaugeValue, val, g.labelPairs, nil, out, nil)
}

// GaugeVec is a Collector that bundles a set of Gauges that all share the same
// Desc, but have different values for their variable labels. This is used if
// you want to count the same thing partitioned by various dimensions
// (e.g. number of operations queued, partitioned by user and operation
// type). Create instances with NewGaugeVec.
type GaugeVec struct {
	*MetricVec
}

// NewGaugeVec creates a new GaugeVec based on the provided GaugeOpts and
// partitioned by the given label names.
func NewGaugeVec(opts GaugeOpts, labelNames []string) *GaugeVec {
	return V2.NewGaugeVec(GaugeVecOpts{
		GaugeOpts:      opts,
		VariableLabels: UnconstrainedLabels(labelNames),
	})
}

// NewGaugeVec creates a new GaugeVec based on the provided GaugeVecOpts.
func (v2) NewGaugeVec(opts GaugeVecOpts) *GaugeVec {
	desc := V2.NewDesc(
		BuildFQName(opts.Namespace, opts.Subsystem, opts.Name),
		opts.Help,
		opts.VariableLabels,
		opts.ConstLabels,
	)
	return &GaugeVec{
		MetricVec: NewMetricVec(desc, func(lvs ...string) Metric {
			if len(lvs) != len(desc.variableLabels.names) {
				panic(makeInconsistentCardinalityError(desc.fqName, desc.variableLabels.names, lvs))
			}
			result := &gauge{desc: desc, labelPairs: MakeLabelPairs(desc, lvs)}
			result.init(result) // Init self-collection.
			return result
		}),
	}
}

// GetMetricWithLabelValues returns the Gauge for the given slice of label
// values (same order as the variable labels in Desc). If that combination of
// label values is accessed for the first time, a new Gauge is created.
//
// It is possible to call this method without using the returned Gauge to only
// create the new Gauge but leave it at its starting value 0. See also the
// SummaryVec example.
//
// Keeping the Gauge for later use is possible (and should be considered if
// performance is critical), but keep in mind that Reset, DeleteLabelValues and
// Delete can be used to delete the Gauge from the GaugeVec. In that case, the
// Gauge will still exist, but it will not be exported anymore, even if a
// Gauge with the same label values is created later. See also the CounterVec
// example.
//
// An error is returned if the number of label values is not the same as the
// number of variable labels in Desc (minus any curried labels).
//
// Note that for more than one label value, this method is prone to mistakes
// caused by an incorrect order of arguments. Consider GetMetricWith(Labels) as
// an alternative to avoid that type of mistake. For higher label numbers, the
// latter has a much more readable (albeit more verbose) syntax, but it comes
// with a performance overhead (for creating and processing the Labels map).
func (v *GaugeVec) GetMetricWithLabelValues(lvs ...string) (Gauge, error) {
	metric, err := v.MetricVec.GetMetricWithLabelValues(lvs...)
	if metric != nil {
		return metric.(Gauge), err
	}
	return nil, err
}

// GetMetricWith returns the Gauge for the given Labels map (the label names
// must match those of the variable labels in Desc). If that label map is
// accessed for the first time, a new Gauge is created. Implications of
// creating a Gauge without using it and keeping the Gauge for later use are
// the same as for GetMetricWithLabelValues.
//
// An error is returned if the number and names of the Labels are inconsistent
// with those of the variable labels in Desc (minus any curried labels).
//
// This method is used for the same purpose as
// GetMetricWithLabelValues(...string). See there for pros and cons of the two
// methods.
func (v *GaugeVec) GetMetricWith(labels Labels) (Gauge, error) {
	metric, err := v.MetricVec.GetMetricWith(labels)
	if metric != nil {
		return metric.(Gauge), err
	}
	return nil, err
}

// WithLabelValues works as GetMetricWithLabelValues, but panics where
// GetMetricWithLabelValues would have returned an error. Not returning an
// error allows shortcuts like
//
//	myVec.WithLabelValues("404", "GET").Add(42)
func (v *GaugeVec) WithLabelValues(lvs ...string) Gauge {
	g, err := v.GetMetricWithLabelValues(lvs...)
	if err != nil {
		panic(err)
	}
	return g
}

// With works as GetMetricWith, but panics where GetMetricWithLabels would have
// returned an error. Not returning an error allows shortcuts like
//
//	myVec.With(prometheus.Labels{"code": "404", "method": "GET"}).Add(42)
func (v *GaugeVec) With(labels Labels) Gauge {
	g, err := v.GetMetricWith(labels)
	if err != nil {
		panic(err)
	}
	return g
}

// CurryWith returns a vector curried with the provided labels, i.e. the
// returned vector has those labels pre-set for all labeled operations performed
// on it. The cardinality of the curried vector is reduced accordingly. The
// order of the remaining labels stays the same (just with the curried labels
// taken out of the sequence – which is relevant for the
// (GetMetric)WithLabelValues methods). It is possible to curry a curried
// vector, but only with labels not yet used for currying before.
//
// The metrics contained in the GaugeVec are shared between the curried and
// uncurried vectors. They are just accessed differently. Curried and uncurried
// vectors behave identically in terms of collection. Only one must be
// registered with a given registry (usually the uncurried version). The Reset
// method deletes all metrics, even if called on a curried vector.
func (v *GaugeVec) CurryWith(labels Labels) (*GaugeVec, error) {
	vec, err := v.MetricVec.CurryWith(labels)
	if vec != nil {
		return &GaugeVec{vec}, err
	}
	return nil, err
}

// MustCurryWith works as CurryWith but panics where CurryWith would have
// returned an error.
func (v *GaugeVec) MustCurryWith(labels Labels) *GaugeVec {
	vec, err := v.CurryWith(labels)
	if err != nil {
		panic(err)
	}
	return vec
}

// GaugeFunc is a Gauge whose value is determined at collect time by calling a
// provided function.
//
// To create GaugeFunc instances, use NewGaugeFunc.
type GaugeFunc interface {
	Metric
	Collector
}

// NewGaugeFunc creates a new GaugeFunc based on the provided GaugeOpts. The
// value reported is determined by calling the given function from within the
// Write method. Take into account that metric collection may happen
// concurrently. Therefore, it must be safe to call the provided function
// concurrently.
//
// NewGaugeFunc is a good way to create an “info” style metric with a constant
// value of 1. Example:
// https://github.com/prometheus/common/blob/8558a5b7db3c84fa38b4766966059a7bd5bfa2ee/version/info.go#L36-L56
func NewGaugeFunc(opts GaugeOpts, function func() float64) GaugeFunc {
	return newValueFunc(NewDesc(
		BuildFQName(opts.Namespace, opts.Subsystem, opts.Name),
		opts.Help,
		nil,
		opts.ConstLabels,
	), GaugeValue, function)
}
