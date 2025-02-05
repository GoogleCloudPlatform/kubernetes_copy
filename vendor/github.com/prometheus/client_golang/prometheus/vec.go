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
	"fmt"
	"sync"

	"github.com/prometheus/common/model"
)

// MetricVec is a Collector to bundle metrics of the same name that differ in
// their label values. MetricVec is not used directly but as a building block
// for implementations of vectors of a given metric type, like GaugeVec,
// CounterVec, SummaryVec, and HistogramVec. It is exported so that it can be
// used for custom Metric implementations.
//
// To create a FooVec for custom Metric Foo, embed a pointer to MetricVec in
// FooVec and initialize it with NewMetricVec. Implement wrappers for
// GetMetricWithLabelValues and GetMetricWith that return (Foo, error) rather
// than (Metric, error). Similarly, create a wrapper for CurryWith that returns
// (*FooVec, error) rather than (*MetricVec, error). It is recommended to also
// add the convenience methods WithLabelValues, With, and MustCurryWith, which
// panic instead of returning errors. See also the MetricVec example.
type MetricVec struct {
	*metricMap

	curry []curriedLabelValue

	// hashAdd and hashAddByte can be replaced for testing collision handling.
	hashAdd     func(h uint64, s string) uint64
	hashAddByte func(h uint64, b byte) uint64
}

// NewMetricVec returns an initialized metricVec.
func NewMetricVec(desc *Desc, newMetric func(lvs ...string) Metric) *MetricVec {
	return &MetricVec{
		metricMap: &metricMap{
			metrics:   map[uint64][]metricWithLabelValues{},
			desc:      desc,
			newMetric: newMetric,
		},
		hashAdd:     hashAdd,
		hashAddByte: hashAddByte,
	}
}

// DeleteLabelValues removes the metric where the variable labels are the same
// as those passed in as labels (same order as the VariableLabels in Desc). It
// returns true if a metric was deleted.
//
// It is not an error if the number of label values is not the same as the
// number of VariableLabels in Desc. However, such inconsistent label count can
// never match an actual metric, so the method will always return false in that
// case.
//
// Note that for more than one label value, this method is prone to mistakes
// caused by an incorrect order of arguments. Consider Delete(Labels) as an
// alternative to avoid that type of mistake. For higher label numbers, the
// latter has a much more readable (albeit more verbose) syntax, but it comes
// with a performance overhead (for creating and processing the Labels map).
// See also the CounterVec example.
func (m *MetricVec) DeleteLabelValues(lvs ...string) bool {
	lvs = constrainLabelValues(m.desc, lvs, m.curry)

	h, err := m.hashLabelValues(lvs)
	if err != nil {
		return false
	}

	return m.metricMap.deleteByHashWithLabelValues(h, lvs, m.curry)
}

// Delete deletes the metric where the variable labels are the same as those
// passed in as labels. It returns true if a metric was deleted.
//
// It is not an error if the number and names of the Labels are inconsistent
// with those of the VariableLabels in Desc. However, such inconsistent Labels
// can never match an actual metric, so the method will always return false in
// that case.
//
// This method is used for the same purpose as DeleteLabelValues(...string). See
// there for pros and cons of the two methods.
func (m *MetricVec) Delete(labels Labels) bool {
	labels, closer := constrainLabels(m.desc, labels)
	defer closer()

	h, err := m.hashLabels(labels)
	if err != nil {
		return false
	}

	return m.metricMap.deleteByHashWithLabels(h, labels, m.curry)
}

// DeletePartialMatch deletes all metrics where the variable labels contain all of those
// passed in as labels. The order of the labels does not matter.
// It returns the number of metrics deleted.
//
// Note that curried labels will never be matched if deleting from the curried vector.
// To match curried labels with DeletePartialMatch, it must be called on the base vector.
func (m *MetricVec) DeletePartialMatch(labels Labels) int {
	labels, closer := constrainLabels(m.desc, labels)
	defer closer()

	return m.metricMap.deleteByLabels(labels, m.curry)
}

// Without explicit forwarding of Describe, Collect, Reset, those methods won't
// show up in GoDoc.

// Describe implements Collector.
func (m *MetricVec) Describe(ch chan<- *Desc) { m.metricMap.Describe(ch) }

// Collect implements Collector.
func (m *MetricVec) Collect(ch chan<- Metric) { m.metricMap.Collect(ch) }

// Reset deletes all metrics in this vector.
func (m *MetricVec) Reset() { m.metricMap.Reset() }

// CurryWith returns a vector curried with the provided labels, i.e. the
// returned vector has those labels pre-set for all labeled operations performed
// on it. The cardinality of the curried vector is reduced accordingly. The
// order of the remaining labels stays the same (just with the curried labels
// taken out of the sequence – which is relevant for the
// (GetMetric)WithLabelValues methods). It is possible to curry a curried
// vector, but only with labels not yet used for currying before.
//
// The metrics contained in the MetricVec are shared between the curried and
// uncurried vectors. They are just accessed differently. Curried and uncurried
// vectors behave identically in terms of collection. Only one must be
// registered with a given registry (usually the uncurried version). The Reset
// method deletes all metrics, even if called on a curried vector.
//
// Note that CurryWith is usually not called directly but through a wrapper
// around MetricVec, implementing a vector for a specific Metric
// implementation, for example GaugeVec.
func (m *MetricVec) CurryWith(labels Labels) (*MetricVec, error) {
	var (
		newCurry []curriedLabelValue
		oldCurry = m.curry
		iCurry   int
	)
	for i, labelName := range m.desc.variableLabels.names {
		val, ok := labels[labelName]
		if iCurry < len(oldCurry) && oldCurry[iCurry].index == i {
			if ok {
				return nil, fmt.Errorf("label name %q is already curried", labelName)
			}
			newCurry = append(newCurry, oldCurry[iCurry])
			iCurry++
		} else {
			if !ok {
				continue // Label stays uncurried.
			}
			newCurry = append(newCurry, curriedLabelValue{
				i,
				m.desc.variableLabels.constrain(labelName, val),
			})
		}
	}
	if l := len(oldCurry) + len(labels) - len(newCurry); l > 0 {
		return nil, fmt.Errorf("%d unknown label(s) found during currying", l)
	}

	return &MetricVec{
		metricMap:   m.metricMap,
		curry:       newCurry,
		hashAdd:     m.hashAdd,
		hashAddByte: m.hashAddByte,
	}, nil
}

// GetMetricWithLabelValues returns the Metric for the given slice of label
// values (same order as the variable labels in Desc). If that combination of
// label values is accessed for the first time, a new Metric is created (by
// calling the newMetric function provided during construction of the
// MetricVec).
//
// It is possible to call this method without using the returned Metric to only
// create the new Metric but leave it in its initial state.
//
// Keeping the Metric for later use is possible (and should be considered if
// performance is critical), but keep in mind that Reset, DeleteLabelValues and
// Delete can be used to delete the Metric from the MetricVec. In that case, the
// Metric will still exist, but it will not be exported anymore, even if a
// Metric with the same label values is created later.
//
// An error is returned if the number of label values is not the same as the
// number of variable labels in Desc (minus any curried labels).
//
// Note that for more than one label value, this method is prone to mistakes
// caused by an incorrect order of arguments. Consider GetMetricWith(Labels) as
// an alternative to avoid that type of mistake. For higher label numbers, the
// latter has a much more readable (albeit more verbose) syntax, but it comes
// with a performance overhead (for creating and processing the Labels map).
//
// Note that GetMetricWithLabelValues is usually not called directly but through
// a wrapper around MetricVec, implementing a vector for a specific Metric
// implementation, for example GaugeVec.
func (m *MetricVec) GetMetricWithLabelValues(lvs ...string) (Metric, error) {
	lvs = constrainLabelValues(m.desc, lvs, m.curry)
	h, err := m.hashLabelValues(lvs)
	if err != nil {
		return nil, err
	}

	return m.metricMap.getOrCreateMetricWithLabelValues(h, lvs, m.curry), nil
}

// GetMetricWith returns the Metric for the given Labels map (the label names
// must match those of the variable labels in Desc). If that label map is
// accessed for the first time, a new Metric is created. Implications of
// creating a Metric without using it and keeping the Metric for later use
// are the same as for GetMetricWithLabelValues.
//
// An error is returned if the number and names of the Labels are inconsistent
// with those of the variable labels in Desc (minus any curried labels).
//
// This method is used for the same purpose as
// GetMetricWithLabelValues(...string). See there for pros and cons of the two
// methods.
//
// Note that GetMetricWith is usually not called directly but through a wrapper
// around MetricVec, implementing a vector for a specific Metric implementation,
// for example GaugeVec.
func (m *MetricVec) GetMetricWith(labels Labels) (Metric, error) {
	labels, closer := constrainLabels(m.desc, labels)
	defer closer()

	h, err := m.hashLabels(labels)
	if err != nil {
		return nil, err
	}

	return m.metricMap.getOrCreateMetricWithLabels(h, labels, m.curry), nil
}

func (m *MetricVec) hashLabelValues(vals []string) (uint64, error) {
	if err := validateLabelValues(vals, len(m.desc.variableLabels.names)-len(m.curry)); err != nil {
		return 0, err
	}

	var (
		h             = hashNew()
		curry         = m.curry
		iVals, iCurry int
	)
	for i := 0; i < len(m.desc.variableLabels.names); i++ {
		if iCurry < len(curry) && curry[iCurry].index == i {
			h = m.hashAdd(h, curry[iCurry].value)
			iCurry++
		} else {
			h = m.hashAdd(h, vals[iVals])
			iVals++
		}
		h = m.hashAddByte(h, model.SeparatorByte)
	}
	return h, nil
}

func (m *MetricVec) hashLabels(labels Labels) (uint64, error) {
	if err := validateValuesInLabels(labels, len(m.desc.variableLabels.names)-len(m.curry)); err != nil {
		return 0, err
	}

	var (
		h      = hashNew()
		curry  = m.curry
		iCurry int
	)
	for i, labelName := range m.desc.variableLabels.names {
		val, ok := labels[labelName]
		if iCurry < len(curry) && curry[iCurry].index == i {
			if ok {
				return 0, fmt.Errorf("label name %q is already curried", labelName)
			}
			h = m.hashAdd(h, curry[iCurry].value)
			iCurry++
		} else {
			if !ok {
				return 0, fmt.Errorf("label name %q missing in label map", labelName)
			}
			h = m.hashAdd(h, val)
		}
		h = m.hashAddByte(h, model.SeparatorByte)
	}
	return h, nil
}

// metricWithLabelValues provides the metric and its label values for
// disambiguation on hash collision.
type metricWithLabelValues struct {
	values []string
	metric Metric
}

// curriedLabelValue sets the curried value for a label at the given index.
type curriedLabelValue struct {
	index int
	value string
}

// metricMap is a helper for metricVec and shared between differently curried
// metricVecs.
type metricMap struct {
	mtx       sync.RWMutex // Protects metrics.
	metrics   map[uint64][]metricWithLabelValues
	desc      *Desc
	newMetric func(labelValues ...string) Metric
}

// Describe implements Collector. It will send exactly one Desc to the provided
// channel.
func (m *metricMap) Describe(ch chan<- *Desc) {
	ch <- m.desc
}

// Collect implements Collector.
func (m *metricMap) Collect(ch chan<- Metric) {
	m.mtx.RLock()
	defer m.mtx.RUnlock()

	for _, metrics := range m.metrics {
		for _, metric := range metrics {
			ch <- metric.metric
		}
	}
}

// Reset deletes all metrics in this vector.
func (m *metricMap) Reset() {
	m.mtx.Lock()
	defer m.mtx.Unlock()

	for h := range m.metrics {
		delete(m.metrics, h)
	}
}

// deleteByHashWithLabelValues removes the metric from the hash bucket h. If
// there are multiple matches in the bucket, use lvs to select a metric and
// remove only that metric.
func (m *metricMap) deleteByHashWithLabelValues(
	h uint64, lvs []string, curry []curriedLabelValue,
) bool {
	m.mtx.Lock()
	defer m.mtx.Unlock()

	metrics, ok := m.metrics[h]
	if !ok {
		return false
	}

	i := findMetricWithLabelValues(metrics, lvs, curry)
	if i >= len(metrics) {
		return false
	}

	if len(metrics) > 1 {
		old := metrics
		m.metrics[h] = append(metrics[:i], metrics[i+1:]...)
		old[len(old)-1] = metricWithLabelValues{}
	} else {
		delete(m.metrics, h)
	}
	return true
}

// deleteByHashWithLabels removes the metric from the hash bucket h. If there
// are multiple matches in the bucket, use lvs to select a metric and remove
// only that metric.
func (m *metricMap) deleteByHashWithLabels(
	h uint64, labels Labels, curry []curriedLabelValue,
) bool {
	m.mtx.Lock()
	defer m.mtx.Unlock()

	metrics, ok := m.metrics[h]
	if !ok {
		return false
	}
	i := findMetricWithLabels(m.desc, metrics, labels, curry)
	if i >= len(metrics) {
		return false
	}

	if len(metrics) > 1 {
		old := metrics
		m.metrics[h] = append(metrics[:i], metrics[i+1:]...)
		old[len(old)-1] = metricWithLabelValues{}
	} else {
		delete(m.metrics, h)
	}
	return true
}

// deleteByLabels deletes a metric if the given labels are present in the metric.
func (m *metricMap) deleteByLabels(labels Labels, curry []curriedLabelValue) int {
	m.mtx.Lock()
	defer m.mtx.Unlock()

	var numDeleted int

	for h, metrics := range m.metrics {
		i := findMetricWithPartialLabels(m.desc, metrics, labels, curry)
		if i >= len(metrics) {
			// Didn't find matching labels in this metric slice.
			continue
		}
		delete(m.metrics, h)
		numDeleted++
	}

	return numDeleted
}

// findMetricWithPartialLabel returns the index of the matching metric or
// len(metrics) if not found.
func findMetricWithPartialLabels(
	desc *Desc, metrics []metricWithLabelValues, labels Labels, curry []curriedLabelValue,
) int {
	for i, metric := range metrics {
		if matchPartialLabels(desc, metric.values, labels, curry) {
			return i
		}
	}
	return len(metrics)
}

// indexOf searches the given slice of strings for the target string and returns
// the index or len(items) as well as a boolean whether the search succeeded.
func indexOf(target string, items []string) (int, bool) {
	for i, l := range items {
		if l == target {
			return i, true
		}
	}
	return len(items), false
}

// valueMatchesVariableOrCurriedValue determines if a value was previously curried,
// and returns whether it matches either the "base" value or the curried value accordingly.
// It also indicates whether the match is against a curried or uncurried value.
func valueMatchesVariableOrCurriedValue(targetValue string, index int, values []string, curry []curriedLabelValue) (bool, bool) {
	for _, curriedValue := range curry {
		if curriedValue.index == index {
			// This label was curried. See if the curried value matches our target.
			return curriedValue.value == targetValue, true
		}
	}
	// This label was not curried. See if the current value matches our target label.
	return values[index] == targetValue, false
}

// matchPartialLabels searches the current metric and returns whether all of the target label:value pairs are present.
func matchPartialLabels(desc *Desc, values []string, labels Labels, curry []curriedLabelValue) bool {
	for l, v := range labels {
		// Check if the target label exists in our metrics and get the index.
		varLabelIndex, validLabel := indexOf(l, desc.variableLabels.names)
		if validLabel {
			// Check the value of that label against the target value.
			// We don't consider curried values in partial matches.
			matches, curried := valueMatchesVariableOrCurriedValue(v, varLabelIndex, values, curry)
			if matches && !curried {
				continue
			}
		}
		return false
	}
	return true
}

// getOrCreateMetricWithLabelValues retrieves the metric by hash and label value
// or creates it and returns the new one.
//
// This function holds the mutex.
func (m *metricMap) getOrCreateMetricWithLabelValues(
	hash uint64, lvs []string, curry []curriedLabelValue,
) Metric {
	m.mtx.RLock()
	metric, ok := m.getMetricWithHashAndLabelValues(hash, lvs, curry)
	m.mtx.RUnlock()
	if ok {
		return metric
	}

	m.mtx.Lock()
	defer m.mtx.Unlock()
	metric, ok = m.getMetricWithHashAndLabelValues(hash, lvs, curry)
	if !ok {
		inlinedLVs := inlineLabelValues(lvs, curry)
		metric = m.newMetric(inlinedLVs...)
		m.metrics[hash] = append(m.metrics[hash], metricWithLabelValues{values: inlinedLVs, metric: metric})
	}
	return metric
}

// getOrCreateMetricWithLabels retrieves the metric by hash and label value
// or creates it and returns the new one.
//
// This function holds the mutex.
func (m *metricMap) getOrCreateMetricWithLabels(
	hash uint64, labels Labels, curry []curriedLabelValue,
) Metric {
	m.mtx.RLock()
	metric, ok := m.getMetricWithHashAndLabels(hash, labels, curry)
	m.mtx.RUnlock()
	if ok {
		return metric
	}

	m.mtx.Lock()
	defer m.mtx.Unlock()
	metric, ok = m.getMetricWithHashAndLabels(hash, labels, curry)
	if !ok {
		lvs := extractLabelValues(m.desc, labels, curry)
		metric = m.newMetric(lvs...)
		m.metrics[hash] = append(m.metrics[hash], metricWithLabelValues{values: lvs, metric: metric})
	}
	return metric
}

// getMetricWithHashAndLabelValues gets a metric while handling possible
// collisions in the hash space. Must be called while holding the read mutex.
func (m *metricMap) getMetricWithHashAndLabelValues(
	h uint64, lvs []string, curry []curriedLabelValue,
) (Metric, bool) {
	metrics, ok := m.metrics[h]
	if ok {
		if i := findMetricWithLabelValues(metrics, lvs, curry); i < len(metrics) {
			return metrics[i].metric, true
		}
	}
	return nil, false
}

// getMetricWithHashAndLabels gets a metric while handling possible collisions in
// the hash space. Must be called while holding read mutex.
func (m *metricMap) getMetricWithHashAndLabels(
	h uint64, labels Labels, curry []curriedLabelValue,
) (Metric, bool) {
	metrics, ok := m.metrics[h]
	if ok {
		if i := findMetricWithLabels(m.desc, metrics, labels, curry); i < len(metrics) {
			return metrics[i].metric, true
		}
	}
	return nil, false
}

// findMetricWithLabelValues returns the index of the matching metric or
// len(metrics) if not found.
func findMetricWithLabelValues(
	metrics []metricWithLabelValues, lvs []string, curry []curriedLabelValue,
) int {
	for i, metric := range metrics {
		if matchLabelValues(metric.values, lvs, curry) {
			return i
		}
	}
	return len(metrics)
}

// findMetricWithLabels returns the index of the matching metric or len(metrics)
// if not found.
func findMetricWithLabels(
	desc *Desc, metrics []metricWithLabelValues, labels Labels, curry []curriedLabelValue,
) int {
	for i, metric := range metrics {
		if matchLabels(desc, metric.values, labels, curry) {
			return i
		}
	}
	return len(metrics)
}

func matchLabelValues(values, lvs []string, curry []curriedLabelValue) bool {
	if len(values) != len(lvs)+len(curry) {
		return false
	}
	var iLVs, iCurry int
	for i, v := range values {
		if iCurry < len(curry) && curry[iCurry].index == i {
			if v != curry[iCurry].value {
				return false
			}
			iCurry++
			continue
		}
		if v != lvs[iLVs] {
			return false
		}
		iLVs++
	}
	return true
}

func matchLabels(desc *Desc, values []string, labels Labels, curry []curriedLabelValue) bool {
	if len(values) != len(labels)+len(curry) {
		return false
	}
	iCurry := 0
	for i, k := range desc.variableLabels.names {
		if iCurry < len(curry) && curry[iCurry].index == i {
			if values[i] != curry[iCurry].value {
				return false
			}
			iCurry++
			continue
		}
		if values[i] != labels[k] {
			return false
		}
	}
	return true
}

func extractLabelValues(desc *Desc, labels Labels, curry []curriedLabelValue) []string {
	labelValues := make([]string, len(labels)+len(curry))
	iCurry := 0
	for i, k := range desc.variableLabels.names {
		if iCurry < len(curry) && curry[iCurry].index == i {
			labelValues[i] = curry[iCurry].value
			iCurry++
			continue
		}
		labelValues[i] = labels[k]
	}
	return labelValues
}

func inlineLabelValues(lvs []string, curry []curriedLabelValue) []string {
	labelValues := make([]string, len(lvs)+len(curry))
	var iCurry, iLVs int
	for i := range labelValues {
		if iCurry < len(curry) && curry[iCurry].index == i {
			labelValues[i] = curry[iCurry].value
			iCurry++
			continue
		}
		labelValues[i] = lvs[iLVs]
		iLVs++
	}
	return labelValues
}

var labelsPool = &sync.Pool{
	New: func() interface{} {
		return make(Labels)
	},
}

func constrainLabels(desc *Desc, labels Labels) (Labels, func()) {
	if len(desc.variableLabels.labelConstraints) == 0 {
		// Fast path when there's no constraints
		return labels, func() {}
	}

	constrainedLabels := labelsPool.Get().(Labels)
	for l, v := range labels {
		constrainedLabels[l] = desc.variableLabels.constrain(l, v)
	}

	return constrainedLabels, func() {
		for k := range constrainedLabels {
			delete(constrainedLabels, k)
		}
		labelsPool.Put(constrainedLabels)
	}
}

func constrainLabelValues(desc *Desc, lvs []string, curry []curriedLabelValue) []string {
	if len(desc.variableLabels.labelConstraints) == 0 {
		// Fast path when there's no constraints
		return lvs
	}

	constrainedValues := make([]string, len(lvs))
	var iCurry, iLVs int
	for i := 0; i < len(lvs)+len(curry); i++ {
		if iCurry < len(curry) && curry[iCurry].index == i {
			iCurry++
			continue
		}

		if i < len(desc.variableLabels.names) {
			constrainedValues[iLVs] = desc.variableLabels.constrain(
				desc.variableLabels.names[i],
				lvs[iLVs],
			)
		} else {
			constrainedValues[iLVs] = lvs[iLVs]
		}
		iLVs++
	}
	return constrainedValues
}
