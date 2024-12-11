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

package backend

import "github.com/prometheus/client_golang/prometheus"

var (
	commitSec = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: "etcd",
		Subsystem: "disk",
		Name:      "backend_commit_duration_seconds",
		Help:      "The latency distributions of commit called by backend.",

		// lowest bucket start of upper bound 0.001 sec (1 ms) with factor 2
		// highest bucket start of 0.001 sec * 2^13 == 8.192 sec
		Buckets: prometheus.ExponentialBuckets(0.001, 2, 14),
	})

	rebalanceSec = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: "etcd_debugging",
		Subsystem: "disk",
		Name:      "backend_commit_rebalance_duration_seconds",
		Help:      "The latency distributions of commit.rebalance called by bboltdb backend.",

		// lowest bucket start of upper bound 0.001 sec (1 ms) with factor 2
		// highest bucket start of 0.001 sec * 2^13 == 8.192 sec
		Buckets: prometheus.ExponentialBuckets(0.001, 2, 14),
	})

	spillSec = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: "etcd_debugging",
		Subsystem: "disk",
		Name:      "backend_commit_spill_duration_seconds",
		Help:      "The latency distributions of commit.spill called by bboltdb backend.",

		// lowest bucket start of upper bound 0.001 sec (1 ms) with factor 2
		// highest bucket start of 0.001 sec * 2^13 == 8.192 sec
		Buckets: prometheus.ExponentialBuckets(0.001, 2, 14),
	})

	writeSec = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: "etcd_debugging",
		Subsystem: "disk",
		Name:      "backend_commit_write_duration_seconds",
		Help:      "The latency distributions of commit.write called by bboltdb backend.",

		// lowest bucket start of upper bound 0.001 sec (1 ms) with factor 2
		// highest bucket start of 0.001 sec * 2^13 == 8.192 sec
		Buckets: prometheus.ExponentialBuckets(0.001, 2, 14),
	})

	defragSec = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: "etcd",
		Subsystem: "disk",
		Name:      "backend_defrag_duration_seconds",
		Help:      "The latency distribution of backend defragmentation.",

		// 100 MB usually takes 1 sec, so start with 10 MB of 100 ms
		// lowest bucket start of upper bound 0.1 sec (100 ms) with factor 2
		// highest bucket start of 0.1 sec * 2^12 == 409.6 sec
		Buckets: prometheus.ExponentialBuckets(.1, 2, 13),
	})

	snapshotTransferSec = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: "etcd",
		Subsystem: "disk",
		Name:      "backend_snapshot_duration_seconds",
		Help:      "The latency distribution of backend snapshots.",

		// lowest bucket start of upper bound 0.01 sec (10 ms) with factor 2
		// highest bucket start of 0.01 sec * 2^16 == 655.36 sec
		Buckets: prometheus.ExponentialBuckets(.01, 2, 17),
	})

	isDefragActive = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "etcd",
		Subsystem: "disk",
		Name:      "defrag_inflight",
		Help:      "Whether or not defrag is active on the member. 1 means active, 0 means not.",
	})
)

func init() {
	prometheus.MustRegister(commitSec)
	prometheus.MustRegister(rebalanceSec)
	prometheus.MustRegister(spillSec)
	prometheus.MustRegister(writeSec)
	prometheus.MustRegister(defragSec)
	prometheus.MustRegister(snapshotTransferSec)
	prometheus.MustRegister(isDefragActive)
}
