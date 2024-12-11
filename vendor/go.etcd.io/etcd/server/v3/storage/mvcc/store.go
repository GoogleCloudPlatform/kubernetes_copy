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
	"go.etcd.io/etcd/server/v3/storage/backend"
	"go.etcd.io/etcd/server/v3/storage/schema"
)

func UnsafeReadFinishedCompact(tx backend.UnsafeReader) (int64, bool) {
	_, finishedCompactBytes := tx.UnsafeRange(schema.Meta, schema.FinishedCompactKeyName, nil, 0)
	if len(finishedCompactBytes) != 0 {
		return BytesToRev(finishedCompactBytes[0]).Main, true
	}
	return 0, false
}

func UnsafeReadScheduledCompact(tx backend.UnsafeReader) (int64, bool) {
	_, scheduledCompactBytes := tx.UnsafeRange(schema.Meta, schema.ScheduledCompactKeyName, nil, 0)
	if len(scheduledCompactBytes) != 0 {
		return BytesToRev(scheduledCompactBytes[0]).Main, true
	}
	return 0, false
}

func SetScheduledCompact(tx backend.BatchTx, value int64) {
	tx.LockInsideApply()
	defer tx.Unlock()
	UnsafeSetScheduledCompact(tx, value)
}

func UnsafeSetScheduledCompact(tx backend.UnsafeWriter, value int64) {
	rbytes := NewRevBytes()
	rbytes = RevToBytes(Revision{Main: value}, rbytes)
	tx.UnsafePut(schema.Meta, schema.ScheduledCompactKeyName, rbytes)
}

func SetFinishedCompact(tx backend.BatchTx, value int64) {
	tx.LockInsideApply()
	defer tx.Unlock()
	UnsafeSetFinishedCompact(tx, value)
}

func UnsafeSetFinishedCompact(tx backend.UnsafeWriter, value int64) {
	rbytes := NewRevBytes()
	rbytes = RevToBytes(Revision{Main: value}, rbytes)
	tx.UnsafePut(schema.Meta, schema.FinishedCompactKeyName, rbytes)
}
