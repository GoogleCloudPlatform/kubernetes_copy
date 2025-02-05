/*
Copyright 2023 The Kubernetes Authors.

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

package flowcontrol

import (
	"sync"
	"sync/atomic"
	"time"

	"k8s.io/utils/clock"
)

const (
	// maxRetryAfter represents the maximum possible retryAfter.
	maxRetryAfter = int64(32)
)

// DroppedRequestsTracker is an interface that allows tracking
// a history od dropped requests in the system for the purpose
// of adjusting RetryAfter header to avoid system overload.
type DroppedRequestsTracker interface {
	// RecordDroppedRequest records a request that was just
	// dropped from processing.
	RecordDroppedRequest(plName string)

	// GetRetryAfter returns the current suggested value of
	// RetryAfter value.
	GetRetryAfter(plName string) int64
}

// unixStat keeps a statistic how many requests were dropped within
// a single second.
type unixStat struct {
	unixTime int64
	requests int64
}

type droppedRequestsStats struct {
	lock sync.RWMutex

	// history stores the history of dropped requests.
	history []unixStat

	// To reduce lock-contention, we store the information about
	// the current second here, which we can then access under
	// reader lock.
	currentUnix  int64
	currentCount atomic.Int64

	retryAfter           atomic.Int64
	retryAfterUpdateUnix int64
}

func newDroppedRequestsStats(nowUnix int64) *droppedRequestsStats {
	result := &droppedRequestsStats{
		// We assume that we can bump at any time after first dropped request.
		retryAfterUpdateUnix: 0,
	}
	result.retryAfter.Store(1)
	return result
}

func (s *droppedRequestsStats) recordDroppedRequest(unixTime int64) {
	// Short path - if the current second matches passed time,
	// just update the stats.
	if done := func() bool {
		s.lock.RLock()
		defer s.lock.RUnlock()
		if s.currentUnix == unixTime {
			s.currentCount.Add(1)
			return true
		}
		return false
	}(); done {
		return
	}

	// We trigger the change of <currentUnix>.
	s.lock.Lock()
	defer s.lock.Unlock()
	if s.currentUnix == unixTime {
		s.currentCount.Add(1)
		return
	}

	s.updateHistory(s.currentUnix, s.currentCount.Load())
	s.currentUnix = unixTime
	s.currentCount.Store(1)

	// We only consider updating retryAfter when bumping the current second.
	// However, given that we didn't report anything for the current second,
	// we recompute it based on statistics from the previous one.
	s.updateRetryAfterIfNeededLocked(unixTime)
}

func (s *droppedRequestsStats) updateHistory(unixTime int64, count int64) {
	s.history = append(s.history, unixStat{unixTime: unixTime, requests: count})

	startIndex := 0
	// Entries that exceed 2*retryAfter or maxRetryAfter are never going to be needed.
	maxHistory := 2 * s.retryAfter.Load()
	if maxHistory > maxRetryAfter {
		maxHistory = maxRetryAfter
	}
	for ; startIndex < len(s.history) && unixTime-s.history[startIndex].unixTime > maxHistory; startIndex++ {
	}
	if startIndex > 0 {
		s.history = s.history[startIndex:]
	}
}

// updateRetryAfterIfNeededLocked updates the retryAfter based on the number of
// dropped requests in the last `retryAfter` seconds:
//   - if there were less than `retryAfter` dropped requests, it decreases
//     retryAfter
//   - if there were at least 3*`retryAfter` dropped requests, it increases
//     retryAfter
//
// The rationale behind these numbers being fairly low is that APF is queuing
// requests and rejecting (dropping) them is a last resort, which is not expected
// unless a given priority level is actually overloaded.
//
// Additionally, we rate-limit the increases of retryAfter to wait at least
// `retryAfter' seconds after the previous increase to avoid multiple bumps
// on a single spike.
//
// We're working with the interval [unixTime-retryAfter, unixTime).
func (s *droppedRequestsStats) updateRetryAfterIfNeededLocked(unixTime int64) {
	retryAfter := s.retryAfter.Load()

	droppedRequests := int64(0)
	for i := len(s.history) - 1; i >= 0; i-- {
		if unixTime-s.history[i].unixTime > retryAfter {
			break
		}
		if s.history[i].unixTime < unixTime {
			droppedRequests += s.history[i].requests
		}
	}

	if unixTime-s.retryAfterUpdateUnix >= retryAfter && droppedRequests >= 3*retryAfter {
		// We try to mimic the TCP algorithm and thus are doubling
		// the retryAfter here.
		retryAfter *= 2
		if retryAfter >= maxRetryAfter {
			retryAfter = maxRetryAfter
		}
		s.retryAfter.Store(retryAfter)
		s.retryAfterUpdateUnix = unixTime
		return
	}

	if droppedRequests < retryAfter && retryAfter > 1 {
		// We try to mimic the TCP algorithm and thus are linearly
		// scaling down the retryAfter here.
		retryAfter--
		s.retryAfter.Store(retryAfter)
		return
	}
}

// droppedRequestsTracker implement DroppedRequestsTracker interface
// for the purpose of adjusting RetryAfter header for newly dropped
// requests to avoid system overload.
type droppedRequestsTracker struct {
	now func() time.Time

	lock    sync.RWMutex
	plStats map[string]*droppedRequestsStats
}

// NewDroppedRequestsTracker is creating a new instance of
// DroppedRequestsTracker.
func NewDroppedRequestsTracker() DroppedRequestsTracker {
	return newDroppedRequestsTracker(clock.RealClock{}.Now)
}

func newDroppedRequestsTracker(now func() time.Time) *droppedRequestsTracker {
	return &droppedRequestsTracker{
		now:     now,
		plStats: make(map[string]*droppedRequestsStats),
	}
}

func (t *droppedRequestsTracker) RecordDroppedRequest(plName string) {
	unixTime := t.now().Unix()

	stats := func() *droppedRequestsStats {
		// The list of priority levels should change very infrequently,
		// so in almost all cases, the fast path should be enough.
		t.lock.RLock()
		if plStats, ok := t.plStats[plName]; ok {
			t.lock.RUnlock()
			return plStats
		}
		t.lock.RUnlock()

		// Slow path taking writer lock to update the map.
		t.lock.Lock()
		defer t.lock.Unlock()
		if plStats, ok := t.plStats[plName]; ok {
			return plStats
		}
		stats := newDroppedRequestsStats(unixTime)
		t.plStats[plName] = stats
		return stats
	}()

	stats.recordDroppedRequest(unixTime)
}

func (t *droppedRequestsTracker) GetRetryAfter(plName string) int64 {
	t.lock.RLock()
	defer t.lock.RUnlock()

	if plStats, ok := t.plStats[plName]; ok {
		return plStats.retryAfter.Load()
	}
	return 1
}
