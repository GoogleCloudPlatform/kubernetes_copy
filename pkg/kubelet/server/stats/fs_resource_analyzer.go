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

package stats

import (
	"sync"
	"sync/atomic"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/events"

	"k8s.io/klog/v2"
)

type statCache map[types.UID]*volumeStatCalculator

// fsResourceAnalyzerInterface is for embedding fs functions into ResourceAnalyzer
type fsResourceAnalyzerInterface interface {
	GetPodVolumeStats(uid types.UID) (PodVolumeStats, bool)
}

// fsResourceAnalyzer provides stats about fs resource usage
type fsResourceAnalyzer struct {
	statsProvider     Provider
	calcPeriod        time.Duration
	cachedVolumeStats atomic.Value
	startOnce         sync.Once
	eventRecorder     events.EventRecorder
}

var _ fsResourceAnalyzerInterface = &fsResourceAnalyzer{}

// newFsResourceAnalyzer returns a new fsResourceAnalyzer implementation
func newFsResourceAnalyzer(statsProvider Provider, calcVolumePeriod time.Duration, eventRecorder events.EventRecorder) *fsResourceAnalyzer {
	r := &fsResourceAnalyzer{
		statsProvider: statsProvider,
		calcPeriod:    calcVolumePeriod,
		eventRecorder: eventRecorder,
	}
	r.cachedVolumeStats.Store(make(statCache))
	return r
}

// Start eager background caching of volume stats.
func (s *fsResourceAnalyzer) Start() {
	s.startOnce.Do(func() {
		if s.calcPeriod <= 0 {
			klog.InfoS("Volume stats collection disabled")
			return
		}
		klog.InfoS("Starting FS ResourceAnalyzer")
		go wait.Forever(func() { s.updateCachedPodVolumeStats() }, s.calcPeriod)
	})
}

// updateCachedPodVolumeStats calculates and caches the PodVolumeStats for every Pod known to the kubelet.
func (s *fsResourceAnalyzer) updateCachedPodVolumeStats() {
	oldCache := s.cachedVolumeStats.Load().(statCache)
	newCache := make(statCache)

	// Copy existing entries to new map, creating/starting new entries for pods missing from the cache
	for _, pod := range s.statsProvider.GetPods() {
		if value, found := oldCache[pod.GetUID()]; !found {
			newCache[pod.GetUID()] = newVolumeStatCalculator(s.statsProvider, s.calcPeriod, pod, s.eventRecorder).StartOnce()
		} else {
			newCache[pod.GetUID()] = value
		}
	}

	// Stop entries for pods that have been deleted
	for uid, entry := range oldCache {
		if _, found := newCache[uid]; !found {
			entry.StopOnce()
		}
	}

	// Update the cache reference
	s.cachedVolumeStats.Store(newCache)
}

// GetPodVolumeStats returns the PodVolumeStats for a given pod.  Results are looked up from a cache that
// is eagerly populated in the background, and never calculated on the fly.
func (s *fsResourceAnalyzer) GetPodVolumeStats(uid types.UID) (PodVolumeStats, bool) {
	cache := s.cachedVolumeStats.Load().(statCache)
	statCalc, found := cache[uid]
	if !found {
		// TODO: Differentiate between stats being empty
		// See issue #20679
		return PodVolumeStats{}, false
	}
	return statCalc.GetLatest()
}
