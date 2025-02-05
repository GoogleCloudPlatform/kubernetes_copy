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

package workqueue

import (
	"k8s.io/client-go/util/workqueue"
	k8smetrics "k8s.io/component-base/metrics"
	"k8s.io/component-base/metrics/legacyregistry"
)

// Package prometheus sets the workqueue DefaultMetricsFactory to produce
// prometheus metrics. To use this package, you just have to import it.

// Metrics subsystem and keys used by the workqueue.
const (
	WorkQueueSubsystem         = "workqueue"
	DepthKey                   = "depth"
	AddsKey                    = "adds_total"
	QueueLatencyKey            = "queue_duration_seconds"
	WorkDurationKey            = "work_duration_seconds"
	UnfinishedWorkKey          = "unfinished_work_seconds"
	LongestRunningProcessorKey = "longest_running_processor_seconds"
	RetriesKey                 = "retries_total"
)

var (
	depth = k8smetrics.NewGaugeVec(&k8smetrics.GaugeOpts{
		Subsystem:      WorkQueueSubsystem,
		Name:           DepthKey,
		StabilityLevel: k8smetrics.ALPHA,
		Help:           "Current depth of workqueue",
	}, []string{"name"})

	adds = k8smetrics.NewCounterVec(&k8smetrics.CounterOpts{
		Subsystem:      WorkQueueSubsystem,
		Name:           AddsKey,
		StabilityLevel: k8smetrics.ALPHA,
		Help:           "Total number of adds handled by workqueue",
	}, []string{"name"})

	latency = k8smetrics.NewHistogramVec(&k8smetrics.HistogramOpts{
		Subsystem:      WorkQueueSubsystem,
		Name:           QueueLatencyKey,
		StabilityLevel: k8smetrics.ALPHA,
		Help:           "How long in seconds an item stays in workqueue before being requested.",
		Buckets:        k8smetrics.ExponentialBuckets(10e-7, 10, 10),
	}, []string{"name"})

	workDuration = k8smetrics.NewHistogramVec(&k8smetrics.HistogramOpts{
		Subsystem:      WorkQueueSubsystem,
		Name:           WorkDurationKey,
		StabilityLevel: k8smetrics.ALPHA,
		Help:           "How long in seconds processing an item from workqueue takes.",
		Buckets:        k8smetrics.ExponentialBuckets(10e-7, 10, 10),
	}, []string{"name"})

	unfinished = k8smetrics.NewGaugeVec(&k8smetrics.GaugeOpts{
		Subsystem:      WorkQueueSubsystem,
		Name:           UnfinishedWorkKey,
		StabilityLevel: k8smetrics.ALPHA,
		Help: "How many seconds of work has done that " +
			"is in progress and hasn't been observed by work_duration. Large " +
			"values indicate stuck threads. One can deduce the number of stuck " +
			"threads by observing the rate at which this increases.",
	}, []string{"name"})

	longestRunningProcessor = k8smetrics.NewGaugeVec(&k8smetrics.GaugeOpts{
		Subsystem:      WorkQueueSubsystem,
		Name:           LongestRunningProcessorKey,
		StabilityLevel: k8smetrics.ALPHA,
		Help: "How many seconds has the longest running " +
			"processor for workqueue been running.",
	}, []string{"name"})

	retries = k8smetrics.NewCounterVec(&k8smetrics.CounterOpts{
		Subsystem:      WorkQueueSubsystem,
		Name:           RetriesKey,
		StabilityLevel: k8smetrics.ALPHA,
		Help:           "Total number of retries handled by workqueue",
	}, []string{"name"})

	metrics = []k8smetrics.Registerable{
		depth, adds, latency, workDuration, unfinished, longestRunningProcessor, retries,
	}
)

type prometheusMetricsProvider struct {
}

func init() {
	for _, m := range metrics {
		legacyregistry.MustRegister(m)
	}
	workqueue.SetProvider(prometheusMetricsProvider{})
}

func (prometheusMetricsProvider) NewDepthMetric(name string) workqueue.GaugeMetric {
	return depth.WithLabelValues(name)
}

func (prometheusMetricsProvider) NewAddsMetric(name string) workqueue.CounterMetric {
	return adds.WithLabelValues(name)
}

func (prometheusMetricsProvider) NewLatencyMetric(name string) workqueue.HistogramMetric {
	return latency.WithLabelValues(name)
}

func (prometheusMetricsProvider) NewWorkDurationMetric(name string) workqueue.HistogramMetric {
	return workDuration.WithLabelValues(name)
}

func (prometheusMetricsProvider) NewUnfinishedWorkSecondsMetric(name string) workqueue.SettableGaugeMetric {
	return unfinished.WithLabelValues(name)
}

func (prometheusMetricsProvider) NewLongestRunningProcessorSecondsMetric(name string) workqueue.SettableGaugeMetric {
	return longestRunningProcessor.WithLabelValues(name)
}

func (prometheusMetricsProvider) NewRetriesMetric(name string) workqueue.CounterMetric {
	return retries.WithLabelValues(name)
}
