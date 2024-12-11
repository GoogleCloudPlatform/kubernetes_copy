// Copyright 2017 The etcd Authors
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

package v3compactor

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/jonboulle/clockwork"
	"go.uber.org/zap"

	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/server/v3/storage/mvcc"
)

// Revision compacts the log by purging revisions older than
// the configured reivison number. Compaction happens every 5 minutes.
type Revision struct {
	lg *zap.Logger

	clock     clockwork.Clock
	retention int64

	rg RevGetter
	c  Compactable

	ctx    context.Context
	cancel context.CancelFunc

	mu     sync.Mutex
	paused bool
}

// newRevision creates a new instance of Revisonal compactor that purges
// the log older than retention revisions from the current revision.
func newRevision(lg *zap.Logger, clock clockwork.Clock, retention int64, rg RevGetter, c Compactable) *Revision {
	rc := &Revision{
		lg:        lg,
		clock:     clock,
		retention: retention,
		rg:        rg,
		c:         c,
	}
	rc.ctx, rc.cancel = context.WithCancel(context.Background())
	return rc
}

const revInterval = 5 * time.Minute

// Run runs revision-based compactor.
func (rc *Revision) Run() {
	prev := int64(0)
	go func() {
		for {
			select {
			case <-rc.ctx.Done():
				return
			case <-rc.clock.After(revInterval):
				rc.mu.Lock()
				p := rc.paused
				rc.mu.Unlock()
				if p {
					continue
				}
			}

			rev := rc.rg.Rev() - rc.retention
			if rev <= 0 || rev == prev {
				continue
			}

			now := time.Now()
			rc.lg.Info(
				"starting auto revision compaction",
				zap.Int64("revision", rev),
				zap.Int64("revision-compaction-retention", rc.retention),
			)
			_, err := rc.c.Compact(rc.ctx, &pb.CompactionRequest{Revision: rev})
			if err == nil || errors.Is(err, mvcc.ErrCompacted) {
				prev = rev
				rc.lg.Info(
					"completed auto revision compaction",
					zap.Int64("revision", rev),
					zap.Int64("revision-compaction-retention", rc.retention),
					zap.Duration("took", time.Since(now)),
				)
			} else {
				rc.lg.Warn(
					"failed auto revision compaction",
					zap.Int64("revision", rev),
					zap.Int64("revision-compaction-retention", rc.retention),
					zap.Duration("retry-interval", revInterval),
					zap.Error(err),
				)
			}
		}
	}()
}

// Stop stops revision-based compactor.
func (rc *Revision) Stop() {
	rc.cancel()
}

// Pause pauses revision-based compactor.
func (rc *Revision) Pause() {
	rc.mu.Lock()
	rc.paused = true
	rc.mu.Unlock()
}

// Resume resumes revision-based compactor.
func (rc *Revision) Resume() {
	rc.mu.Lock()
	rc.paused = false
	rc.mu.Unlock()
}
