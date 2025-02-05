/*
Copyright 2016 The Kubernetes Authors.

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

// This file provides abstractions for setting the provider (e.g., prometheus)
// of metrics.

package cache

import (
	"sync"

	"k8s.io/utils/clock"
)

// GaugeMetric represents a single numerical value that can arbitrarily go up
// and down.
type GaugeMetric interface {
	Set(float64)
}

// CounterMetric represents a single numerical value that only ever
// goes up.
type CounterMetric interface {
	Inc()
}

// SummaryMetric captures individual observations.
type SummaryMetric interface {
	Observe(float64)
}

// HistogramMetric represents a metric that captures observations in buckets
// and provides a histogram of the distribution of the observations.
type HistogramMetric interface {
	Observe(float64)
}

type noopMetric struct{}

func (noopMetric) Inc()            {}
func (noopMetric) Dec()            {}
func (noopMetric) Observe(float64) {}
func (noopMetric) Set(float64)     {}

// reflectorMetrics is a struct that holds the metrics for the reflector.
type reflectorMetrics struct {
	clock clock.Clock

	// numberOfLists represents the total number of lists.
	numberOfLists CounterMetric
	// listDuration represents the duration of the list.
	listDuration HistogramMetric
	// numberOfItemsInList represents the total number of items in the list.
	numberOfItemsInList GaugeMetric

	// numberOfWatches represents the total number of watches.
	numberOfWatches CounterMetric
	// numberOfShortWatches represents the total number of short watches.
	numberOfShortWatches CounterMetric
	// watchDuration represents the duration of the watch.
	watchDuration HistogramMetric
	// numberOfItemsInWatch represents the total number of items in the watch.
	numberOfItemsInWatch GaugeMetric
}

// ReflectorMetricsProvider generates various metrics used by the reflector.
type ReflectorMetricsProvider interface {
	NewListsMetric(name string) CounterMetric
	NewListDurationMetric(name string) HistogramMetric
	NewItemsInListMetric(name string) GaugeMetric

	NewWatchesMetric(name string) CounterMetric
	NewShortWatchesMetric(name string) CounterMetric
	NewWatchDurationMetric(name string) HistogramMetric
	NewItemsInWatchMetric(name string) GaugeMetric
}

type noopReflectorMetricsProvider struct{}

func (noopReflectorMetricsProvider) NewListsMetric(name string) CounterMetric { return noopMetric{} }
func (noopReflectorMetricsProvider) NewListDurationMetric(name string) HistogramMetric {
	return noopMetric{}
}
func (noopReflectorMetricsProvider) NewItemsInListMetric(name string) GaugeMetric {
	return noopMetric{}
}
func (noopReflectorMetricsProvider) NewWatchesMetric(name string) CounterMetric { return noopMetric{} }
func (noopReflectorMetricsProvider) NewShortWatchesMetric(name string) CounterMetric {
	return noopMetric{}
}
func (noopReflectorMetricsProvider) NewWatchDurationMetric(name string) HistogramMetric {
	return noopMetric{}
}
func (noopReflectorMetricsProvider) NewItemsInWatchMetric(name string) GaugeMetric {
	return noopMetric{}
}

// reflectorMetricsFactory is a struct that holds the metrics provider.
type reflectorMetricsFactory struct {
	metricsProvider ReflectorMetricsProvider
	setProviders    sync.Once
}

var metricsFactory = &reflectorMetricsFactory{
	metricsProvider: noopReflectorMetricsProvider{},
}

func (metricsFactory *reflectorMetricsFactory) setMetricsProvider(metricsProvider ReflectorMetricsProvider) {
	metricsFactory.setProviders.Do(func() {
		metricsFactory.metricsProvider = metricsProvider
	})
}

func (metricsFactory *reflectorMetricsFactory) getMetricsProvider() ReflectorMetricsProvider {
	return metricsFactory.metricsProvider
}

// newReflectorMetrics creates a new reflectorMetrics object with the given name.
// If the name is empty, it returns a noopMetrics object.
func newReflectorMetrics(name string) *reflectorMetrics {
	var ret *reflectorMetrics
	if name == "" {
		return ret
	}

	return &reflectorMetrics{
		clock: &clock.RealClock{},

		numberOfLists:       metricsFactory.getMetricsProvider().NewListsMetric(name),
		listDuration:        metricsFactory.getMetricsProvider().NewListDurationMetric(name),
		numberOfItemsInList: metricsFactory.getMetricsProvider().NewItemsInListMetric(name),

		numberOfWatches:      metricsFactory.getMetricsProvider().NewWatchesMetric(name),
		numberOfShortWatches: metricsFactory.getMetricsProvider().NewShortWatchesMetric(name),
		watchDuration:        metricsFactory.getMetricsProvider().NewWatchDurationMetric(name),
		numberOfItemsInWatch: metricsFactory.getMetricsProvider().NewItemsInWatchMetric(name),
	}
}

// SetReflectorMetricsProvider sets the metrics provider.
func SetReflectorMetricsProvider(metricsProvider ReflectorMetricsProvider) {
	metricsFactory.setMetricsProvider(metricsProvider)
}
