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

package etcdserver

import (
	goruntime "runtime"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	"go.etcd.io/etcd/api/v3/version"
	"go.etcd.io/etcd/pkg/v3/runtime"
)

var (
	hasLeader = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "etcd",
		Subsystem: "server",
		Name:      "has_leader",
		Help:      "Whether or not a leader exists. 1 is existence, 0 is not.",
	})
	isLeader = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "etcd",
		Subsystem: "server",
		Name:      "is_leader",
		Help:      "Whether or not this member is a leader. 1 if is, 0 otherwise.",
	})
	leaderChanges = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "etcd",
		Subsystem: "server",
		Name:      "leader_changes_seen_total",
		Help:      "The number of leader changes seen.",
	})
	learnerPromoteFailed = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "etcd",
		Subsystem: "server",
		Name:      "learner_promote_failures",
		Help:      "The total number of failed learner promotions (likely learner not ready) while this member is leader.",
	},
		[]string{"Reason"},
	)
	learnerPromoteSucceed = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "etcd",
		Subsystem: "server",
		Name:      "learner_promote_successes",
		Help:      "The total number of successful learner promotions while this member is leader.",
	})
	heartbeatSendFailures = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "etcd",
		Subsystem: "server",
		Name:      "heartbeat_send_failures_total",
		Help:      "The total number of leader heartbeat send failures (likely overloaded from slow disk).",
	})
	applySnapshotInProgress = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "etcd",
		Subsystem: "server",
		Name:      "snapshot_apply_in_progress_total",
		Help:      "1 if the server is applying the incoming snapshot. 0 if none.",
	})
	proposalsCommitted = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "etcd",
		Subsystem: "server",
		Name:      "proposals_committed_total",
		Help:      "The total number of consensus proposals committed.",
	})
	proposalsApplied = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "etcd",
		Subsystem: "server",
		Name:      "proposals_applied_total",
		Help:      "The total number of consensus proposals applied.",
	})
	proposalsPending = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "etcd",
		Subsystem: "server",
		Name:      "proposals_pending",
		Help:      "The current number of pending proposals to commit.",
	})
	proposalsFailed = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "etcd",
		Subsystem: "server",
		Name:      "proposals_failed_total",
		Help:      "The total number of failed proposals seen.",
	})
	slowReadIndex = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "etcd",
		Subsystem: "server",
		Name:      "slow_read_indexes_total",
		Help:      "The total number of pending read indexes not in sync with leader's or timed out read index requests.",
	})
	readIndexFailed = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "etcd",
		Subsystem: "server",
		Name:      "read_indexes_failed_total",
		Help:      "The total number of failed read indexes seen.",
	})
	leaseExpired = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "etcd_debugging",
		Subsystem: "server",
		Name:      "lease_expired_total",
		Help:      "The total number of expired leases.",
	})

	currentVersion = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "etcd",
		Subsystem: "server",
		Name:      "version",
		Help:      "Which version is running. 1 for 'server_version' label with current version.",
	},
		[]string{"server_version"})
	currentGoVersion = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "etcd",
		Subsystem: "server",
		Name:      "go_version",
		Help:      "Which Go version server is running with. 1 for 'server_go_version' label with current version.",
	},
		[]string{"server_go_version"})
	serverID = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "etcd",
		Subsystem: "server",
		Name:      "id",
		Help:      "Server or member ID in hexadecimal format. 1 for 'server_id' label with current ID.",
	},
		[]string{"server_id"})

	fdUsed = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "os",
		Subsystem: "fd",
		Name:      "used",
		Help:      "The number of used file descriptors.",
	})
	fdLimit = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "os",
		Subsystem: "fd",
		Name:      "limit",
		Help:      "The file descriptor limit.",
	})
)

func init() {
	prometheus.MustRegister(hasLeader)
	prometheus.MustRegister(isLeader)
	prometheus.MustRegister(leaderChanges)
	prometheus.MustRegister(heartbeatSendFailures)
	prometheus.MustRegister(applySnapshotInProgress)
	prometheus.MustRegister(proposalsCommitted)
	prometheus.MustRegister(proposalsApplied)
	prometheus.MustRegister(proposalsPending)
	prometheus.MustRegister(proposalsFailed)
	prometheus.MustRegister(slowReadIndex)
	prometheus.MustRegister(readIndexFailed)
	prometheus.MustRegister(leaseExpired)
	prometheus.MustRegister(currentVersion)
	prometheus.MustRegister(currentGoVersion)
	prometheus.MustRegister(serverID)
	prometheus.MustRegister(learnerPromoteSucceed)
	prometheus.MustRegister(learnerPromoteFailed)
	prometheus.MustRegister(fdUsed)
	prometheus.MustRegister(fdLimit)

	currentVersion.With(prometheus.Labels{
		"server_version": version.Version,
	}).Set(1)
	currentGoVersion.With(prometheus.Labels{
		"server_go_version": goruntime.Version(),
	}).Set(1)
}

func monitorFileDescriptor(lg *zap.Logger, done <-chan struct{}) {
	// This ticker will check File Descriptor Requirements ,and count all fds in used.
	// And recorded some logs when in used >= limit/5*4. Just recorded message.
	// If fds was more than 10K,It's low performance due to FDUsage() works.
	// So need to increase it.
	// See https://github.com/etcd-io/etcd/issues/11969 for more detail.
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for {
		used, err := runtime.FDUsage()
		if err != nil {
			lg.Warn("failed to get file descriptor usage", zap.Error(err))
			return
		}
		fdUsed.Set(float64(used))
		limit, err := runtime.FDLimit()
		if err != nil {
			lg.Warn("failed to get file descriptor limit", zap.Error(err))
			return
		}
		fdLimit.Set(float64(limit))
		if used >= limit/5*4 {
			lg.Warn("80% of file descriptors are used", zap.Uint64("used", used), zap.Uint64("limit", limit))
		}
		select {
		case <-ticker.C:
		case <-done:
			return
		}
	}
}
