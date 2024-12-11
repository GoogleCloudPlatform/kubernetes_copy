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

package mvcc

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"go.uber.org/zap"

	"go.etcd.io/etcd/api/v3/mvccpb"
	"go.etcd.io/etcd/client/pkg/v3/verify"
	"go.etcd.io/etcd/pkg/v3/schedule"
	"go.etcd.io/etcd/pkg/v3/traceutil"
	"go.etcd.io/etcd/server/v3/lease"
	"go.etcd.io/etcd/server/v3/storage/backend"
	"go.etcd.io/etcd/server/v3/storage/schema"
)

var (
	ErrCompacted = errors.New("mvcc: required revision has been compacted")
	ErrFutureRev = errors.New("mvcc: required revision is a future revision")
)

var restoreChunkKeys = 10000 // non-const for testing
var defaultCompactionBatchLimit = 1000
var defaultCompactionSleepInterval = 10 * time.Millisecond

type StoreConfig struct {
	CompactionBatchLimit    int
	CompactionSleepInterval time.Duration
}

type store struct {
	ReadView
	WriteView

	cfg StoreConfig

	// mu read locks for txns and write locks for non-txn store changes.
	mu sync.RWMutex

	b       backend.Backend
	kvindex index

	le lease.Lessor

	// revMuLock protects currentRev and compactMainRev.
	// Locked at end of write txn and released after write txn unlock lock.
	// Locked before locking read txn and released after locking.
	revMu sync.RWMutex
	// currentRev is the revision of the last completed transaction.
	currentRev int64
	// compactMainRev is the main revision of the last compaction.
	compactMainRev int64

	fifoSched schedule.Scheduler

	stopc chan struct{}

	lg     *zap.Logger
	hashes HashStorage
}

// NewStore returns a new store. It is useful to create a store inside
// mvcc pkg. It should only be used for testing externally.
func NewStore(lg *zap.Logger, b backend.Backend, le lease.Lessor, cfg StoreConfig) *store {
	if lg == nil {
		lg = zap.NewNop()
	}
	if cfg.CompactionBatchLimit == 0 {
		cfg.CompactionBatchLimit = defaultCompactionBatchLimit
	}
	if cfg.CompactionSleepInterval == 0 {
		cfg.CompactionSleepInterval = defaultCompactionSleepInterval
	}
	s := &store{
		cfg:     cfg,
		b:       b,
		kvindex: newTreeIndex(lg),

		le: le,

		currentRev:     1,
		compactMainRev: -1,

		fifoSched: schedule.NewFIFOScheduler(lg),

		stopc: make(chan struct{}),

		lg: lg,
	}
	s.hashes = NewHashStorage(lg, s)
	s.ReadView = &readView{s}
	s.WriteView = &writeView{s}
	if s.le != nil {
		s.le.SetRangeDeleter(func() lease.TxnDelete { return s.Write(traceutil.TODO()) })
	}

	tx := s.b.BatchTx()
	tx.LockOutsideApply()
	tx.UnsafeCreateBucket(schema.Key)
	schema.UnsafeCreateMetaBucket(tx)
	tx.Unlock()
	s.b.ForceCommit()

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.restore(); err != nil {
		// TODO: return the error instead of panic here?
		panic("failed to recover store from backend")
	}

	return s
}

func (s *store) compactBarrier(ctx context.Context, ch chan struct{}) {
	if ctx == nil || ctx.Err() != nil {
		select {
		case <-s.stopc:
		default:
			// fix deadlock in mvcc, for more information, please refer to pr 11817.
			// s.stopc is only updated in restore operation, which is called by apply
			// snapshot call, compaction and apply snapshot requests are serialized by
			// raft, and do not happen at the same time.
			s.mu.Lock()
			f := schedule.NewJob("kvstore_compactBarrier", func(ctx context.Context) { s.compactBarrier(ctx, ch) })
			s.fifoSched.Schedule(f)
			s.mu.Unlock()
		}
		return
	}
	close(ch)
}

func (s *store) hash() (hash uint32, revision int64, err error) {
	// TODO: hash and revision could be inconsistent, one possible fix is to add s.revMu.RLock() at the beginning of function, which is costly
	start := time.Now()

	s.b.ForceCommit()
	h, err := s.b.Hash(schema.DefaultIgnores)

	hashSec.Observe(time.Since(start).Seconds())
	return h, s.currentRev, err
}

func (s *store) hashByRev(rev int64) (hash KeyValueHash, currentRev int64, err error) {
	var compactRev int64
	start := time.Now()

	s.mu.RLock()
	s.revMu.RLock()
	compactRev, currentRev = s.compactMainRev, s.currentRev
	s.revMu.RUnlock()

	if rev > 0 && rev < compactRev {
		s.mu.RUnlock()
		return KeyValueHash{}, 0, ErrCompacted
	} else if rev > 0 && rev > currentRev {
		s.mu.RUnlock()
		return KeyValueHash{}, currentRev, ErrFutureRev
	}
	if rev == 0 {
		rev = currentRev
	}
	keep := s.kvindex.Keep(rev)

	tx := s.b.ReadTx()
	tx.RLock()
	defer tx.RUnlock()
	s.mu.RUnlock()
	hash, err = unsafeHashByRev(tx, compactRev, rev, keep)
	hashRevSec.Observe(time.Since(start).Seconds())
	return hash, currentRev, err
}

func (s *store) updateCompactRev(rev int64) (<-chan struct{}, int64, error) {
	s.revMu.Lock()
	if rev <= s.compactMainRev {
		ch := make(chan struct{})
		f := schedule.NewJob("kvstore_updateCompactRev_compactBarrier", func(ctx context.Context) { s.compactBarrier(ctx, ch) })
		s.fifoSched.Schedule(f)
		s.revMu.Unlock()
		return ch, 0, ErrCompacted
	}
	if rev > s.currentRev {
		s.revMu.Unlock()
		return nil, 0, ErrFutureRev
	}
	compactMainRev := s.compactMainRev
	s.compactMainRev = rev

	SetScheduledCompact(s.b.BatchTx(), rev)
	// ensure that desired compaction is persisted
	// gofail: var compactBeforeCommitScheduledCompact struct{}
	s.b.ForceCommit()
	// gofail: var compactAfterCommitScheduledCompact struct{}

	s.revMu.Unlock()

	return nil, compactMainRev, nil
}

// checkPrevCompactionCompleted checks whether the previous scheduled compaction is completed.
func (s *store) checkPrevCompactionCompleted() bool {
	tx := s.b.ReadTx()
	tx.RLock()
	defer tx.RUnlock()
	scheduledCompact, scheduledCompactFound := UnsafeReadScheduledCompact(tx)
	finishedCompact, finishedCompactFound := UnsafeReadFinishedCompact(tx)
	return scheduledCompact == finishedCompact && scheduledCompactFound == finishedCompactFound
}

func (s *store) compact(trace *traceutil.Trace, rev, prevCompactRev int64, prevCompactionCompleted bool) <-chan struct{} {
	ch := make(chan struct{})
	j := schedule.NewJob("kvstore_compact", func(ctx context.Context) {
		if ctx.Err() != nil {
			s.compactBarrier(ctx, ch)
			return
		}
		hash, err := s.scheduleCompaction(rev, prevCompactRev)
		if err != nil {
			s.lg.Warn("Failed compaction", zap.Error(err))
			s.compactBarrier(context.TODO(), ch)
			return
		}
		// Only store the hash value if the previous hash is completed, i.e. this compaction
		// hashes every revision from last compaction. For more details, see #15919.
		if prevCompactionCompleted {
			s.hashes.Store(hash)
		} else {
			s.lg.Info("previous compaction was interrupted, skip storing compaction hash value")
		}
		close(ch)
	})

	s.fifoSched.Schedule(j)
	trace.Step("schedule compaction")
	return ch
}

func (s *store) compactLockfree(rev int64) (<-chan struct{}, error) {
	prevCompactionCompleted := s.checkPrevCompactionCompleted()
	ch, prevCompactRev, err := s.updateCompactRev(rev)
	if err != nil {
		return ch, err
	}

	return s.compact(traceutil.TODO(), rev, prevCompactRev, prevCompactionCompleted), nil
}

func (s *store) Compact(trace *traceutil.Trace, rev int64) (<-chan struct{}, error) {
	s.mu.Lock()
	prevCompactionCompleted := s.checkPrevCompactionCompleted()
	ch, prevCompactRev, err := s.updateCompactRev(rev)
	trace.Step("check and update compact revision")
	if err != nil {
		s.mu.Unlock()
		return ch, err
	}
	s.mu.Unlock()

	return s.compact(trace, rev, prevCompactRev, prevCompactionCompleted), nil
}

func (s *store) Commit() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.b.ForceCommit()
}

func (s *store) Restore(b backend.Backend) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	close(s.stopc)
	s.fifoSched.Stop()

	s.b = b
	s.kvindex = newTreeIndex(s.lg)

	{
		// During restore the metrics might report 'special' values
		s.revMu.Lock()
		s.currentRev = 1
		s.compactMainRev = -1
		s.revMu.Unlock()
	}

	s.fifoSched = schedule.NewFIFOScheduler(s.lg)
	s.stopc = make(chan struct{})

	return s.restore()
}

//nolint:unparam
func (s *store) restore() error {
	s.setupMetricsReporter()

	min, max := NewRevBytes(), NewRevBytes()
	min = RevToBytes(Revision{Main: 1}, min)
	max = RevToBytes(Revision{Main: math.MaxInt64, Sub: math.MaxInt64}, max)

	keyToLease := make(map[string]lease.LeaseID)

	// restore index
	tx := s.b.ReadTx()
	tx.RLock()

	finishedCompact, found := UnsafeReadFinishedCompact(tx)
	if found {
		s.revMu.Lock()
		s.compactMainRev = finishedCompact

		s.lg.Info(
			"restored last compact revision",
			zap.String("meta-bucket-name-key", string(schema.FinishedCompactKeyName)),
			zap.Int64("restored-compact-revision", s.compactMainRev),
		)
		s.revMu.Unlock()
	}
	scheduledCompact, _ := UnsafeReadScheduledCompact(tx)
	// index keys concurrently as they're loaded in from tx
	keysGauge.Set(0)
	rkvc, revc := restoreIntoIndex(s.lg, s.kvindex)
	for {
		keys, vals := tx.UnsafeRange(schema.Key, min, max, int64(restoreChunkKeys))
		if len(keys) == 0 {
			break
		}
		// rkvc blocks if the total pending keys exceeds the restore
		// chunk size to keep keys from consuming too much memory.
		restoreChunk(s.lg, rkvc, keys, vals, keyToLease)
		if len(keys) < restoreChunkKeys {
			// partial set implies final set
			break
		}
		// next set begins after where this one ended
		newMin := BytesToRev(keys[len(keys)-1][:revBytesLen])
		newMin.Sub++
		min = RevToBytes(newMin, min)
	}
	close(rkvc)

	{
		s.revMu.Lock()
		s.currentRev = <-revc

		// keys in the range [compacted revision -N, compaction] might all be deleted due to compaction.
		// the correct revision should be set to compaction revision in the case, not the largest revision
		// we have seen.
		if s.currentRev < s.compactMainRev {
			s.currentRev = s.compactMainRev
		}

		// If the latest revision was a tombstone revision and etcd just compacted
		// it, but crashed right before persisting the FinishedCompactRevision,
		// then it would lead to revision decreasing in bbolt db file. In such
		// a scenario, we should adjust the current revision using the scheduled
		// compact revision on bootstrap when etcd gets started again.
		//
		// See https://github.com/etcd-io/etcd/issues/17780#issuecomment-2061900231
		if s.currentRev < scheduledCompact {
			s.currentRev = scheduledCompact
		}
		s.revMu.Unlock()
	}

	if scheduledCompact <= s.compactMainRev {
		scheduledCompact = 0
	}

	for key, lid := range keyToLease {
		if s.le == nil {
			tx.RUnlock()
			panic("no lessor to attach lease")
		}
		err := s.le.Attach(lid, []lease.LeaseItem{{Key: key}})
		if err != nil {
			s.lg.Error(
				"failed to attach a lease",
				zap.String("lease-id", fmt.Sprintf("%016x", lid)),
				zap.Error(err),
			)
		}
	}
	tx.RUnlock()

	s.lg.Info("kvstore restored", zap.Int64("current-rev", s.currentRev))

	if scheduledCompact != 0 {
		if _, err := s.compactLockfree(scheduledCompact); err != nil {
			s.lg.Warn("compaction encountered error",
				zap.Int64("scheduled-compact-revision", scheduledCompact),
				zap.Error(err),
			)
		} else {
			s.lg.Info(
				"resume scheduled compaction",
				zap.Int64("scheduled-compact-revision", scheduledCompact),
			)
		}
	}

	return nil
}

type revKeyValue struct {
	key  []byte
	kv   mvccpb.KeyValue
	kstr string
}

func restoreIntoIndex(lg *zap.Logger, idx index) (chan<- revKeyValue, <-chan int64) {
	rkvc, revc := make(chan revKeyValue, restoreChunkKeys), make(chan int64, 1)
	go func() {
		currentRev := int64(1)
		defer func() { revc <- currentRev }()
		// restore the tree index from streaming the unordered index.
		kiCache := make(map[string]*keyIndex, restoreChunkKeys)
		for rkv := range rkvc {
			ki, ok := kiCache[rkv.kstr]
			// purge kiCache if many keys but still missing in the cache
			if !ok && len(kiCache) >= restoreChunkKeys {
				i := 10
				for k := range kiCache {
					delete(kiCache, k)
					if i--; i == 0 {
						break
					}
				}
			}
			// cache miss, fetch from tree index if there
			if !ok {
				ki = &keyIndex{key: rkv.kv.Key}
				if idxKey := idx.KeyIndex(ki); idxKey != nil {
					kiCache[rkv.kstr], ki = idxKey, idxKey
					ok = true
				}
			}

			rev := BytesToRev(rkv.key)
			verify.Verify(func() {
				if rev.Main < currentRev {
					panic(fmt.Errorf("revision %d shouldn't be less than the previous revision %d", rev.Main, currentRev))
				}
			})
			currentRev = rev.Main

			if ok {
				if isTombstone(rkv.key) {
					if err := ki.tombstone(lg, rev.Main, rev.Sub); err != nil {
						lg.Warn("tombstone encountered error", zap.Error(err))
					}
					continue
				}
				ki.put(lg, rev.Main, rev.Sub)
			} else if !isTombstone(rkv.key) {
				ki.restore(lg, Revision{Main: rkv.kv.CreateRevision}, rev, rkv.kv.Version)
				idx.Insert(ki)
				kiCache[rkv.kstr] = ki
			}
		}
	}()
	return rkvc, revc
}

func restoreChunk(lg *zap.Logger, kvc chan<- revKeyValue, keys, vals [][]byte, keyToLease map[string]lease.LeaseID) {
	for i, key := range keys {
		rkv := revKeyValue{key: key}
		if err := rkv.kv.Unmarshal(vals[i]); err != nil {
			lg.Fatal("failed to unmarshal mvccpb.KeyValue", zap.Error(err))
		}
		rkv.kstr = string(rkv.kv.Key)
		if isTombstone(key) {
			delete(keyToLease, rkv.kstr)
		} else if lid := lease.LeaseID(rkv.kv.Lease); lid != lease.NoLease {
			keyToLease[rkv.kstr] = lid
		} else {
			delete(keyToLease, rkv.kstr)
		}
		kvc <- rkv
	}
}

func (s *store) Close() error {
	close(s.stopc)
	s.fifoSched.Stop()
	return nil
}

func (s *store) setupMetricsReporter() {
	b := s.b
	reportDbTotalSizeInBytesMu.Lock()
	reportDbTotalSizeInBytes = func() float64 { return float64(b.Size()) }
	reportDbTotalSizeInBytesMu.Unlock()
	reportDbTotalSizeInUseInBytesMu.Lock()
	reportDbTotalSizeInUseInBytes = func() float64 { return float64(b.SizeInUse()) }
	reportDbTotalSizeInUseInBytesMu.Unlock()
	reportDbOpenReadTxNMu.Lock()
	reportDbOpenReadTxN = func() float64 { return float64(b.OpenReadTxN()) }
	reportDbOpenReadTxNMu.Unlock()
	reportCurrentRevMu.Lock()
	reportCurrentRev = func() float64 {
		s.revMu.RLock()
		defer s.revMu.RUnlock()
		return float64(s.currentRev)
	}
	reportCurrentRevMu.Unlock()
	reportCompactRevMu.Lock()
	reportCompactRev = func() float64 {
		s.revMu.RLock()
		defer s.revMu.RUnlock()
		return float64(s.compactMainRev)
	}
	reportCompactRevMu.Unlock()
}

func (s *store) HashStorage() HashStorage {
	return s.hashes
}
