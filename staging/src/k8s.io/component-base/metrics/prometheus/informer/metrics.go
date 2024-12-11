/*
Copyright 2024 The Kubernetes Authors.

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

package informer

import (
	"k8s.io/client-go/tools/cache"
	k8smetrics "k8s.io/component-base/metrics"
	"k8s.io/component-base/metrics/legacyregistry"
)

const (
	Subsystem                    = "informer"
	StoreItemTotalKey            = "store_item_total"
	QueuedItemTotalKey           = "queued_item_total"
	PendingNotificationsTotalKey = "pending_notifications_total"
	RingGrowingCapacityKey       = "ring_growing_capacity"
	EventProcessingDurationKey   = "event_processing_duration_seconds"
)

var (
	storeItemTotal = k8smetrics.NewGaugeVec(&k8smetrics.GaugeOpts{
		Subsystem:      Subsystem,
		Name:           StoreItemTotalKey,
		Help:           "Total number of items stored in the informer",
		StabilityLevel: k8smetrics.ALPHA,
	}, []string{"name"})
	queuedItemTotal = k8smetrics.NewGaugeVec(&k8smetrics.GaugeOpts{
		Subsystem:      Subsystem,
		Name:           QueuedItemTotalKey,
		Help:           "Total number of items queued in the informer",
		StabilityLevel: k8smetrics.ALPHA,
	}, []string{"name"})
	pendingNotificationsTotal = k8smetrics.NewGaugeVec(&k8smetrics.GaugeOpts{
		Subsystem:      Subsystem,
		Name:           PendingNotificationsTotalKey,
		Help:           "Total number of pending notifications in the informer",
		StabilityLevel: k8smetrics.ALPHA,
	}, []string{"name"})
	ringGrowingCapacity = k8smetrics.NewGaugeVec(&k8smetrics.GaugeOpts{
		Subsystem:      Subsystem,
		Name:           RingGrowingCapacityKey,
		Help:           "Current capacity of the ring buffer in the informer",
		StabilityLevel: k8smetrics.ALPHA,
	}, []string{"name"})
	eventProcessingDuration = k8smetrics.NewHistogramVec(&k8smetrics.HistogramOpts{
		Subsystem:      Subsystem,
		Name:           EventProcessingDurationKey,
		Help:           "Duration of event processing in the informer",
		StabilityLevel: k8smetrics.ALPHA,
		Buckets: []float64{0.05, 0.1, 0.15, 0.2, 0.25, 0.3, 0.35, 0.4, 0.45, 0.5, 0.6, 0.7, 0.8, 0.9, 1.0,
			1.25, 1.5, 1.75, 2.0, 2.5, 3.0, 3.5, 4.0, 4.5, 5, 6, 7, 8, 9, 10, 15, 20, 25, 30, 40, 50, 60},
	}, []string{"name"})

	metrics = []k8smetrics.Registerable{
		storeItemTotal, queuedItemTotal, pendingNotificationsTotal, ringGrowingCapacity, eventProcessingDuration,
	}
)

type informerMetricsProvider struct{}

func init() {
	for _, m := range metrics {
		legacyregistry.MustRegister(m)
	}
	cache.SetInformerMetricsProvider(informerMetricsProvider{})
}

func (informerMetricsProvider) NewStoredItemMetric(name string) cache.GaugeMetric {
	return storeItemTotal.WithLabelValues(name)
}

func (informerMetricsProvider) NewQueuedItemMetric(name string) cache.GaugeMetric {
	return queuedItemTotal.WithLabelValues(name)
}

func (informerMetricsProvider) NewPendingNotificationsTotalMetric(name string) cache.GaugeMetric {
	return pendingNotificationsTotal.WithLabelValues(name)
}

func (informerMetricsProvider) NewRingGrowingCapacityMetric(name string) cache.GaugeMetric {
	return ringGrowingCapacity.WithLabelValues(name)
}

func (informerMetricsProvider) NewEventProcessingDurationMetric(name string) cache.HistogramMetric {
	return eventProcessingDuration.WithLabelValues(name)
}
