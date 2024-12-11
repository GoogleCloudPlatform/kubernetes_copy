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

package storage

import (
	"sync"

	humanize "github.com/dustin/go-humanize"
	"go.uber.org/zap"

	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/server/v3/storage/backend"
)

const (
	// DefaultQuotaBytes is the number of bytes the backend Size may
	// consume before exceeding the space quota.
	DefaultQuotaBytes = int64(2 * 1024 * 1024 * 1024) // 2GB
	// MaxQuotaBytes is the maximum number of bytes suggested for a backend
	// quota. A larger quota may lead to degraded performance.
	MaxQuotaBytes = int64(8 * 1024 * 1024 * 1024) // 8GB
)

// Quota represents an arbitrary quota against arbitrary requests. Each request
// costs some charge; if there is not enough remaining charge, then there are
// too few resources available within the quota to apply the request.
type Quota interface {
	// Available judges whether the given request fits within the quota.
	Available(req any) bool
	// Cost computes the charge against the quota for a given request.
	Cost(req any) int
	// Remaining is the amount of charge left for the quota.
	Remaining() int64
}

type passthroughQuota struct{}

func (*passthroughQuota) Available(any) bool { return true }
func (*passthroughQuota) Cost(any) int       { return 0 }
func (*passthroughQuota) Remaining() int64   { return 1 }

type BackendQuota struct {
	be              backend.Backend
	maxBackendBytes int64
}

const (
	// leaseOverhead is an estimate for the cost of storing a lease
	leaseOverhead = 64
	// kvOverhead is an estimate for the cost of storing a key's Metadata
	kvOverhead = 256
)

var (
	// only log once
	quotaLogOnce sync.Once

	DefaultQuotaSize = humanize.Bytes(uint64(DefaultQuotaBytes))
	maxQuotaSize     = humanize.Bytes(uint64(MaxQuotaBytes))
)

// NewBackendQuota creates a quota layer with the given storage limit.
func NewBackendQuota(lg *zap.Logger, quotaBackendBytesCfg int64, be backend.Backend, name string) Quota {
	quotaBackendBytes.Set(float64(quotaBackendBytesCfg))
	if quotaBackendBytesCfg < 0 {
		// disable quotas if negative
		quotaLogOnce.Do(func() {
			lg.Info(
				"disabled backend quota",
				zap.String("quota-name", name),
				zap.Int64("quota-size-bytes", quotaBackendBytesCfg),
			)
		})
		return &passthroughQuota{}
	}

	if quotaBackendBytesCfg == 0 {
		// use default size if no quota size given
		quotaLogOnce.Do(func() {
			if lg != nil {
				lg.Info(
					"enabled backend quota with default value",
					zap.String("quota-name", name),
					zap.Int64("quota-size-bytes", DefaultQuotaBytes),
					zap.String("quota-size", DefaultQuotaSize),
				)
			}
		})
		quotaBackendBytes.Set(float64(DefaultQuotaBytes))
		return &BackendQuota{be, DefaultQuotaBytes}
	}

	quotaLogOnce.Do(func() {
		if quotaBackendBytesCfg > MaxQuotaBytes {
			lg.Warn(
				"quota exceeds the maximum value",
				zap.String("quota-name", name),
				zap.Int64("quota-size-bytes", quotaBackendBytesCfg),
				zap.String("quota-size", humanize.Bytes(uint64(quotaBackendBytesCfg))),
				zap.Int64("quota-maximum-size-bytes", MaxQuotaBytes),
				zap.String("quota-maximum-size", maxQuotaSize),
			)
		}
		lg.Info(
			"enabled backend quota",
			zap.String("quota-name", name),
			zap.Int64("quota-size-bytes", quotaBackendBytesCfg),
			zap.String("quota-size", humanize.Bytes(uint64(quotaBackendBytesCfg))),
		)
	})
	return &BackendQuota{be, quotaBackendBytesCfg}
}

func (b *BackendQuota) Available(v any) bool {
	cost := b.Cost(v)
	// if there are no mutating requests, it's safe to pass through
	if cost == 0 {
		return true
	}
	// TODO: maybe optimize Backend.Size()
	return b.be.Size()+int64(cost) < b.maxBackendBytes
}

func (b *BackendQuota) Cost(v any) int {
	switch r := v.(type) {
	case *pb.PutRequest:
		return costPut(r)
	case *pb.TxnRequest:
		return costTxn(r)
	case *pb.LeaseGrantRequest:
		return leaseOverhead
	default:
		panic("unexpected cost")
	}
}

func costPut(r *pb.PutRequest) int { return kvOverhead + len(r.Key) + len(r.Value) }

func costTxnReq(u *pb.RequestOp) int {
	r := u.GetRequestPut()
	if r == nil {
		return 0
	}
	return costPut(r)
}

func costTxn(r *pb.TxnRequest) int {
	sizeSuccess := 0
	for _, u := range r.Success {
		sizeSuccess += costTxnReq(u)
	}
	sizeFailure := 0
	for _, u := range r.Failure {
		sizeFailure += costTxnReq(u)
	}
	if sizeFailure > sizeSuccess {
		return sizeFailure
	}
	return sizeSuccess
}

func (b *BackendQuota) Remaining() int64 {
	return b.maxBackendBytes - b.be.Size()
}
