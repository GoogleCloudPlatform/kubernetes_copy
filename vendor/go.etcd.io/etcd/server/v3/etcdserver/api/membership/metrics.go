// Copyright 2018 The etcd Authors
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

package membership

import "github.com/prometheus/client_golang/prometheus"

var (
	ClusterVersionMetrics = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "etcd",
		Subsystem: "cluster",
		Name:      "version",
		Help:      "Which version is running. 1 for 'cluster_version' label with current cluster version",
	},
		[]string{"cluster_version"})
	knownPeers = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "etcd",
		Subsystem: "network",
		Name:      "known_peers",
		Help:      "The current number of known peers.",
	},
		[]string{"Local", "Remote"},
	)
	isLearner = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "etcd",
		Subsystem: "server",
		Name:      "is_learner",
		Help:      "Whether or not this member is a learner. 1 if is, 0 otherwise.",
	})
)

func setIsLearnerMetric(m *Member) {
	if m.IsLearner {
		isLearner.Set(1)
	} else {
		isLearner.Set(0)
	}
}

func init() {
	prometheus.MustRegister(ClusterVersionMetrics)
	prometheus.MustRegister(knownPeers)
	prometheus.MustRegister(isLearner)
}
