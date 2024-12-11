// Copyright 2016 The etcd Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package contention

import (
	"sync"
	"time"
)

// TimeoutDetector detects routine starvations by
// observing the actual time duration to finish an action
// or between two events that should happen in a fixed
// interval. If the observed duration is longer than
// the expectation, the detector will report the result.
type TimeoutDetector struct {
	mu          sync.Mutex // protects all
	maxDuration time.Duration
	// map from event to last seen time of event.
	records map[uint64]time.Time
}

// NewTimeoutDetector creates the TimeoutDetector.
func NewTimeoutDetector(maxDuration time.Duration) *TimeoutDetector {
	return &TimeoutDetector{
		maxDuration: maxDuration,
		records:     make(map[uint64]time.Time),
	}
}

// Reset resets the TimeoutDetector.
func (td *TimeoutDetector) Reset() {
	td.mu.Lock()
	defer td.mu.Unlock()

	td.records = make(map[uint64]time.Time)
}

// Observe observes an event of given id. It computes
// the time elapsed between successive events of given id.
// It returns whether this time elapsed exceeds the expectation,
// and the amount by which it exceeds the expectation.
func (td *TimeoutDetector) Observe(id uint64) (bool, time.Duration) {
	td.mu.Lock()
	defer td.mu.Unlock()

	ok := true
	now := time.Now()
	exceed := time.Duration(0)

	if pt, found := td.records[id]; found {
		exceed = now.Sub(pt) - td.maxDuration
		if exceed > 0 {
			ok = false
		}
	}
	td.records[id] = now
	return ok, exceed
}
