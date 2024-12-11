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
	"encoding/binary"
	"fmt"
	"time"

	humanize "github.com/dustin/go-humanize"
	"go.uber.org/zap"

	"go.etcd.io/etcd/server/v3/storage/schema"
)

func (s *store) scheduleCompaction(compactMainRev, prevCompactRev int64) (KeyValueHash, error) {
	totalStart := time.Now()
	keep := s.kvindex.Compact(compactMainRev)
	indexCompactionPauseMs.Observe(float64(time.Since(totalStart) / time.Millisecond))

	totalStart = time.Now()
	defer func() { dbCompactionTotalMs.Observe(float64(time.Since(totalStart) / time.Millisecond)) }()
	keyCompactions := 0
	defer func() { dbCompactionKeysCounter.Add(float64(keyCompactions)) }()
	defer func() { dbCompactionLast.Set(float64(time.Now().Unix())) }()

	end := make([]byte, 8)
	binary.BigEndian.PutUint64(end, uint64(compactMainRev+1))

	batchNum := s.cfg.CompactionBatchLimit
	batchTicker := time.NewTicker(s.cfg.CompactionSleepInterval)
	defer batchTicker.Stop()
	h := newKVHasher(prevCompactRev, compactMainRev, keep)
	last := make([]byte, 8+1+8)
	for {
		var rev Revision

		start := time.Now()

		tx := s.b.BatchTx()
		tx.LockOutsideApply()
		keys, values := tx.UnsafeRange(schema.Key, last, end, int64(batchNum))
		for i := range keys {
			rev = BytesToRev(keys[i])
			if _, ok := keep[rev]; !ok {
				tx.UnsafeDelete(schema.Key, keys[i])
				keyCompactions++
			}
			h.WriteKeyValue(keys[i], values[i])
		}

		if len(keys) < batchNum {
			// gofail: var compactBeforeSetFinishedCompact struct{}
			UnsafeSetFinishedCompact(tx, compactMainRev)
			tx.Unlock()
			// gofail: var compactAfterSetFinishedCompact struct{}
			hash := h.Hash()
			size, sizeInUse := s.b.Size(), s.b.SizeInUse()
			s.lg.Info(
				"finished scheduled compaction",
				zap.Int64("compact-revision", compactMainRev),
				zap.Duration("took", time.Since(totalStart)),
				zap.Uint32("hash", hash.Hash),
				zap.Int64("current-db-size-bytes", size),
				zap.String("current-db-size", humanize.Bytes(uint64(size))),
				zap.Int64("current-db-size-in-use-bytes", sizeInUse),
				zap.String("current-db-size-in-use", humanize.Bytes(uint64(sizeInUse))),
			)
			return hash, nil
		}

		tx.Unlock()
		// update last
		last = RevToBytes(Revision{Main: rev.Main, Sub: rev.Sub + 1}, last)
		// Immediately commit the compaction deletes instead of letting them accumulate in the write buffer
		// gofail: var compactBeforeCommitBatch struct{}
		s.b.ForceCommit()
		// gofail: var compactAfterCommitBatch struct{}
		dbCompactionPauseMs.Observe(float64(time.Since(start) / time.Millisecond))

		select {
		case <-batchTicker.C:
		case <-s.stopc:
			return KeyValueHash{}, fmt.Errorf("interrupted due to stop signal")
		}
	}
}
