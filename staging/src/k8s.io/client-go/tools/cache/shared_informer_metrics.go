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

package cache

import (
	"sync"

	"k8s.io/utils/clock"
)

// informerMetrics is the metrics for a shared informer.
type informerMetrics struct {
	clock clock.Clock

	// numberOfStoredItem represents the total number of items currently stored in the informer's Store.
	numberOfStoredItem GaugeMetric

	// numberOfQueuedItem represents the total number of items currently in the informer's DeltaFIFO.
	numberOfQueuedItem GaugeMetric

	// eventHandlerMetrics tracks the metrics for each event handler.
	eventHandlerMetrics map[string]eventHandlerMetrics
}

type eventHandlerMetrics struct {
	// numberOfPendingNotifications represents the total number of pending notifications for the event handler.
	numberOfPendingNotifications GaugeMetric

	// sizeOfRingGrowing represents the size of the RingGrowing data structure.
	sizeOfRingGrowing GaugeMetric

	// processDuration represents the duration of processing an item from informer reflector.
	processDuration HistogramMetric
}

// InformerMetricsProvider generates various metrics used by the informer.
type InformerMetricsProvider interface {
	// NewStoredItemMetric returns metric for total number of item in store.
	NewStoredItemMetric(name string) GaugeMetric
	// NewQueuedItemMetric returns metric for total number of item in queue.
	NewQueuedItemMetric(name string) GaugeMetric

	// NewPendingNotificationsTotalMetric returns metric for pending notifications.
	NewPendingNotificationsTotalMetric(name string) GaugeMetric
	// NewRingGrowingCapacityMetric returns metric for ring buffer capacity.
	NewRingGrowingCapacityMetric(name string) GaugeMetric
	// NewEventProcessingDurationMetric returns metric for event processing duration.
	NewEventProcessingDurationMetric(name string) HistogramMetric
}

type noopInformerMetricsProvider struct{}

func (noopInformerMetricsProvider) NewStoredItemMetric(name string) GaugeMetric {
	return noopMetric{}
}
func (noopInformerMetricsProvider) NewQueuedItemMetric(name string) GaugeMetric {
	return noopMetric{}
}

func (noopInformerMetricsProvider) NewPendingNotificationsTotalMetric(name string) GaugeMetric {
	return noopMetric{}
}

func (noopInformerMetricsProvider) NewRingGrowingCapacityMetric(name string) GaugeMetric {
	return noopMetric{}
}

func (noopInformerMetricsProvider) NewEventProcessingDurationMetric(name string) HistogramMetric {
	return noopMetric{}
}

type informerMetricsFactory struct {
	metricsProvider InformerMetricsProvider
	setProviders    sync.Once
}

var sharedInformerMetricsFactory = &informerMetricsFactory{
	metricsProvider: noopInformerMetricsProvider{},
}

func (metricsFactory *informerMetricsFactory) setMetricsProvider(metricsProvider InformerMetricsProvider) {
	metricsFactory.setProviders.Do(func() {
		metricsFactory.metricsProvider = metricsProvider
	})
}

func (metricsFactory *informerMetricsFactory) getMetricsProvider() InformerMetricsProvider {
	return metricsFactory.metricsProvider
}

// newInformerMetrics creates a new informerMetrics struct with the given name.
// If the name is empty, it returns nil.
func newInformerMetrics(name string) *informerMetrics {
	var ret *informerMetrics
	if name == "" {
		return ret
	}

	return &informerMetrics{
		clock:               &clock.RealClock{},
		numberOfStoredItem:  sharedInformerMetricsFactory.getMetricsProvider().NewStoredItemMetric(name),
		numberOfQueuedItem:  sharedInformerMetricsFactory.getMetricsProvider().NewQueuedItemMetric(name),
		eventHandlerMetrics: make(map[string]eventHandlerMetrics),
	}
}

// SetInformerMetricsProvider sets the metrics provider for the informer.
func SetInformerMetricsProvider(metricsProvider InformerMetricsProvider) {
	sharedInformerMetricsFactory.setMetricsProvider(metricsProvider)
}
