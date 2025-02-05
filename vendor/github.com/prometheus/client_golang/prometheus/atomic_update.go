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
)

// atomicUpdateFloat atomically updates the float64 value pointed to by bits
// using the provided updateFunc, with an exponential backoff on contention.
func atomicUpdateFloat(bits *uint64, updateFunc func(float64) float64) {
	const (
		// both numbers are derived from empirical observations
		// documented in this PR: https://github.com/prometheus/client_golang/pull/1661
		maxBackoff     = 320 * time.Millisecond
		initialBackoff = 10 * time.Millisecond
	)
	backoff := initialBackoff

	for {
		loadedBits := atomic.LoadUint64(bits)
		oldFloat := math.Float64frombits(loadedBits)
		newFloat := updateFunc(oldFloat)
		newBits := math.Float64bits(newFloat)

		if atomic.CompareAndSwapUint64(bits, loadedBits, newBits) {
			break
		} else {
			// Exponential backoff with sleep and cap to avoid infinite wait
			time.Sleep(backoff)
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
	}
}
