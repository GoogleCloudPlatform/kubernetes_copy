// Copyright 2015 The etcd Authors
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

package txn

import (
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

var (
	slowApplies = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "etcd",
		Subsystem: "server",
		Name:      "slow_apply_total",
		Help:      "The total number of slow apply requests (likely overloaded from slow disk).",
	})
	applySec = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "etcd",
		Subsystem: "server",
		Name:      "apply_duration_seconds",
		Help:      "The latency distributions of v2 apply called by backend.",

		// lowest bucket start of upper bound 0.0001 sec (0.1 ms) with factor 2
		// highest bucket start of 0.0001 sec * 2^19 == 52.4288 sec
		Buckets: prometheus.ExponentialBuckets(0.0001, 2, 20),
	},
		[]string{"version", "op", "success"})
	rangeSec = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "etcd",
		Subsystem: "server",
		Name:      "range_duration_seconds",
		Help:      "The latency distributions of txn.Range",

		// lowest bucket start of upper bound 0.0001 sec (0.1 ms) with factor 2
		// highest bucket start of 0.0001 sec * 2^19 == 52.4288 sec
		Buckets: prometheus.ExponentialBuckets(0.0001, 2, 20),
	},
		[]string{"success"})
)

func ApplySecObserve(version, op string, success bool, latency time.Duration) {
	applySec.WithLabelValues(version, op, strconv.FormatBool(success)).Observe(float64(latency.Microseconds()) / 1000000.0)
}

func RangeSecObserve(success bool, latency time.Duration) {
	rangeSec.WithLabelValues(strconv.FormatBool(success)).Observe(float64(latency.Microseconds()) / 1000000.0)
}

func init() {
	prometheus.MustRegister(applySec)
	prometheus.MustRegister(rangeSec)
	prometheus.MustRegister(slowApplies)
}
