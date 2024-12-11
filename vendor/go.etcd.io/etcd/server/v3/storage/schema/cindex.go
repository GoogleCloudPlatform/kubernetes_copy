// Copyright 2021 The etcd Authors
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

package schema

import (
	"encoding/binary"
	"fmt"

	"go.etcd.io/etcd/client/pkg/v3/verify"
	"go.etcd.io/etcd/server/v3/storage/backend"
)

// UnsafeCreateMetaBucket creates the `meta` bucket (if it does not exist yet).
func UnsafeCreateMetaBucket(tx backend.UnsafeWriter) {
	tx.UnsafeCreateBucket(Meta)
}

// CreateMetaBucket creates the `meta` bucket (if it does not exist yet).
func CreateMetaBucket(tx backend.BatchTx) {
	tx.LockOutsideApply()
	defer tx.Unlock()
	tx.UnsafeCreateBucket(Meta)
}

// UnsafeReadConsistentIndex loads consistent index & term from given transaction.
// returns 0,0 if the data are not found.
// Term is persisted since v3.5.
func UnsafeReadConsistentIndex(tx backend.UnsafeReader) (uint64, uint64) {
	_, vs := tx.UnsafeRange(Meta, MetaConsistentIndexKeyName, nil, 0)
	if len(vs) == 0 {
		return 0, 0
	}
	v := binary.BigEndian.Uint64(vs[0])
	_, ts := tx.UnsafeRange(Meta, MetaTermKeyName, nil, 0)
	if len(ts) == 0 {
		return v, 0
	}
	t := binary.BigEndian.Uint64(ts[0])
	return v, t
}

// ReadConsistentIndex loads consistent index and term from given transaction.
// returns 0 if the data are not found.
func ReadConsistentIndex(tx backend.ReadTx) (uint64, uint64) {
	tx.RLock()
	defer tx.RUnlock()
	return UnsafeReadConsistentIndex(tx)
}

func UnsafeUpdateConsistentIndexForce(tx backend.UnsafeReadWriter, index uint64, term uint64) {
	unsafeUpdateConsistentIndex(tx, index, term, true)
}

func UnsafeUpdateConsistentIndex(tx backend.UnsafeReadWriter, index uint64, term uint64) {
	unsafeUpdateConsistentIndex(tx, index, term, false)
}

func unsafeUpdateConsistentIndex(tx backend.UnsafeReadWriter, index uint64, term uint64, allowDecreasing bool) {
	if index == 0 {
		// Never save 0 as it means that we didn't load the real index yet.
		return
	}
	bs1 := make([]byte, 8)
	binary.BigEndian.PutUint64(bs1, index)

	if !allowDecreasing {
		verify.Verify(func() {
			previousIndex, _ := UnsafeReadConsistentIndex(tx)
			if index < previousIndex {
				panic(fmt.Errorf("update of consistent index not advancing: previous: %v new: %v", previousIndex, index))
			}
		})
	}

	// put the index into the underlying backend
	// tx has been locked in TxnBegin, so there is no need to lock it again
	tx.UnsafePut(Meta, MetaConsistentIndexKeyName, bs1)
	if term > 0 {
		bs2 := make([]byte, 8)
		binary.BigEndian.PutUint64(bs2, term)
		tx.UnsafePut(Meta, MetaTermKeyName, bs2)
	}
}
