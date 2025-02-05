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

package reflector

import (
	"k8s.io/client-go/tools/cache"
	k8smetrics "k8s.io/component-base/metrics"
	"k8s.io/component-base/metrics/legacyregistry"
)

const (
	ReflectorSubsystem   = "reflector"
	ListsTotalKey        = "lists_total"
	ListsDurationKey     = "list_duration_seconds"
	ItemsPerListKey      = "items_per_list"
	WatchListsTotalKey   = "watchLists_total"
	WatchesTotalKey      = "watches_total"
	ShortWatchesTotalKey = "short_watches_total"
	WatchDurationKey     = "watch_duration_seconds"
	ItemsPerWatchKey     = "items_per_watch"
)

var (
	listsTotal = k8smetrics.NewCounterVec(&k8smetrics.CounterOpts{
		Subsystem:      ReflectorSubsystem,
		Name:           ListsTotalKey,
		Help:           "Total number of API lists done by the reflectors",
		StabilityLevel: k8smetrics.ALPHA,
	}, []string{"name"})

	listsDuration = k8smetrics.NewHistogramVec(&k8smetrics.HistogramOpts{
		Subsystem:      ReflectorSubsystem,
		Name:           ListsDurationKey,
		Help:           "How long an API list takes to return and decode for the reflectors",
		StabilityLevel: k8smetrics.ALPHA,
		Buckets: []float64{0.05, 0.1, 0.15, 0.2, 0.25, 0.3, 0.35, 0.4, 0.45, 0.5, 0.6, 0.7, 0.8, 0.9, 1.0,
			1.25, 1.5, 1.75, 2.0, 2.5, 3.0, 3.5, 4.0, 4.5, 5, 6, 7, 8, 9, 10, 15, 20, 25, 30, 40, 50, 60},
	}, []string{"name"})

	itemsPerList = k8smetrics.NewGaugeVec(&k8smetrics.GaugeOpts{
		Subsystem:      ReflectorSubsystem,
		Name:           ItemsPerListKey,
		Help:           "Number of items returned by an API list for the reflectors",
		StabilityLevel: k8smetrics.ALPHA,
	}, []string{"name"})

	watchListsTotal = k8smetrics.NewCounterVec(&k8smetrics.CounterOpts{
		Subsystem:      ReflectorSubsystem,
		Name:           WatchListsTotalKey,
		Help:           "Total number of API watch lists done by the reflectors",
		StabilityLevel: k8smetrics.ALPHA,
	}, []string{"name"})

	watchesTotal = k8smetrics.NewCounterVec(&k8smetrics.CounterOpts{
		Subsystem:      ReflectorSubsystem,
		Name:           WatchesTotalKey,
		Help:           "Total number of API watches done by the reflectors",
		StabilityLevel: k8smetrics.ALPHA,
	}, []string{"name"})

	shortWatchesTotal = k8smetrics.NewCounterVec(&k8smetrics.CounterOpts{
		Subsystem:      ReflectorSubsystem,
		Name:           ShortWatchesTotalKey,
		Help:           "Total number of short API watches done by the reflectors",
		StabilityLevel: k8smetrics.ALPHA,
	}, []string{"name"})

	watchDuration = k8smetrics.NewHistogramVec(&k8smetrics.HistogramOpts{
		Subsystem:      ReflectorSubsystem,
		Name:           WatchDurationKey,
		Help:           "How long an API watch takes to return and decode for the reflectors",
		StabilityLevel: k8smetrics.ALPHA,
		Buckets:        []float64{0.05, 0.1, 0.5, 1, 2.5, 5, 10, 15, 30, 60, 120, 300, 600},
	}, []string{"name"})

	itemsPerWatch = k8smetrics.NewGaugeVec(&k8smetrics.GaugeOpts{
		Subsystem:      ReflectorSubsystem,
		Name:           ItemsPerWatchKey,
		Help:           "Number of items returned by an API watch for the reflectors",
		StabilityLevel: k8smetrics.ALPHA,
	}, []string{"name"})

	metrics = []k8smetrics.Registerable{
		listsTotal, listsDuration, itemsPerList, watchListsTotal, watchesTotal, shortWatchesTotal, watchDuration, itemsPerWatch,
	}
)

type reflectorMetricsProvider struct{}

func init() {
	for _, m := range metrics {
		legacyregistry.MustRegister(m)
	}
	cache.SetReflectorMetricsProvider(reflectorMetricsProvider{})
}

func (reflectorMetricsProvider) NewListsMetric(name string) cache.CounterMetric {
	return listsTotal.WithLabelValues(name)
}

func (reflectorMetricsProvider) NewListDurationMetric(name string) cache.HistogramMetric {
	return listsDuration.WithLabelValues(name)
}

func (reflectorMetricsProvider) NewItemsInListMetric(name string) cache.GaugeMetric {
	return itemsPerList.WithLabelValues(name)
}

func (reflectorMetricsProvider) NewWatchesMetric(name string) cache.CounterMetric {
	return watchesTotal.WithLabelValues(name)
}

func (reflectorMetricsProvider) NewShortWatchesMetric(name string) cache.CounterMetric {
	return shortWatchesTotal.WithLabelValues(name)
}

func (reflectorMetricsProvider) NewWatchDurationMetric(name string) cache.HistogramMetric {
	return watchDuration.WithLabelValues(name)
}

func (reflectorMetricsProvider) NewItemsInWatchMetric(name string) cache.GaugeMetric {
	return itemsPerWatch.WithLabelValues(name)
}
