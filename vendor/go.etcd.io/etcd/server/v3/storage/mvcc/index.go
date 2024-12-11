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
	"sync"

	"github.com/google/btree"
	"go.uber.org/zap"
)

type index interface {
	Get(key []byte, atRev int64) (rev, created Revision, ver int64, err error)
	Range(key, end []byte, atRev int64) ([][]byte, []Revision)
	Revisions(key, end []byte, atRev int64, limit int) ([]Revision, int)
	CountRevisions(key, end []byte, atRev int64) int
	Put(key []byte, rev Revision)
	Tombstone(key []byte, rev Revision) error
	Compact(rev int64) map[Revision]struct{}
	Keep(rev int64) map[Revision]struct{}
	Equal(b index) bool

	Insert(ki *keyIndex)
	KeyIndex(ki *keyIndex) *keyIndex
}

type treeIndex struct {
	sync.RWMutex
	tree *btree.BTreeG[*keyIndex]
	lg   *zap.Logger
}

func newTreeIndex(lg *zap.Logger) index {
	return &treeIndex{
		tree: btree.NewG(32, func(aki *keyIndex, bki *keyIndex) bool {
			return aki.Less(bki)
		}),
		lg: lg,
	}
}

func (ti *treeIndex) Put(key []byte, rev Revision) {
	keyi := &keyIndex{key: key}

	ti.Lock()
	defer ti.Unlock()
	okeyi, ok := ti.tree.Get(keyi)
	if !ok {
		keyi.put(ti.lg, rev.Main, rev.Sub)
		ti.tree.ReplaceOrInsert(keyi)
		return
	}
	okeyi.put(ti.lg, rev.Main, rev.Sub)
}

func (ti *treeIndex) Get(key []byte, atRev int64) (modified, created Revision, ver int64, err error) {
	ti.RLock()
	defer ti.RUnlock()
	return ti.unsafeGet(key, atRev)
}

func (ti *treeIndex) unsafeGet(key []byte, atRev int64) (modified, created Revision, ver int64, err error) {
	keyi := &keyIndex{key: key}
	if keyi = ti.keyIndex(keyi); keyi == nil {
		return Revision{}, Revision{}, 0, ErrRevisionNotFound
	}
	return keyi.get(ti.lg, atRev)
}

func (ti *treeIndex) KeyIndex(keyi *keyIndex) *keyIndex {
	ti.RLock()
	defer ti.RUnlock()
	return ti.keyIndex(keyi)
}

func (ti *treeIndex) keyIndex(keyi *keyIndex) *keyIndex {
	if ki, ok := ti.tree.Get(keyi); ok {
		return ki
	}
	return nil
}

func (ti *treeIndex) unsafeVisit(key, end []byte, f func(ki *keyIndex) bool) {
	keyi, endi := &keyIndex{key: key}, &keyIndex{key: end}

	ti.tree.AscendGreaterOrEqual(keyi, func(item *keyIndex) bool {
		if len(endi.key) > 0 && !item.Less(endi) {
			return false
		}
		if !f(item) {
			return false
		}
		return true
	})
}

// Revisions returns limited number of revisions from key(included) to end(excluded)
// at the given rev. The returned slice is sorted in the order of key. There is no limit if limit <= 0.
// The second return parameter isn't capped by the limit and reflects the total number of revisions.
func (ti *treeIndex) Revisions(key, end []byte, atRev int64, limit int) (revs []Revision, total int) {
	ti.RLock()
	defer ti.RUnlock()

	if end == nil {
		rev, _, _, err := ti.unsafeGet(key, atRev)
		if err != nil {
			return nil, 0
		}
		return []Revision{rev}, 1
	}
	ti.unsafeVisit(key, end, func(ki *keyIndex) bool {
		if rev, _, _, err := ki.get(ti.lg, atRev); err == nil {
			if limit <= 0 || len(revs) < limit {
				revs = append(revs, rev)
			}
			total++
		}
		return true
	})
	return revs, total
}

// CountRevisions returns the number of revisions
// from key(included) to end(excluded) at the given rev.
func (ti *treeIndex) CountRevisions(key, end []byte, atRev int64) int {
	ti.RLock()
	defer ti.RUnlock()

	if end == nil {
		_, _, _, err := ti.unsafeGet(key, atRev)
		if err != nil {
			return 0
		}
		return 1
	}
	total := 0
	ti.unsafeVisit(key, end, func(ki *keyIndex) bool {
		if _, _, _, err := ki.get(ti.lg, atRev); err == nil {
			total++
		}
		return true
	})
	return total
}

func (ti *treeIndex) Range(key, end []byte, atRev int64) (keys [][]byte, revs []Revision) {
	ti.RLock()
	defer ti.RUnlock()

	if end == nil {
		rev, _, _, err := ti.unsafeGet(key, atRev)
		if err != nil {
			return nil, nil
		}
		return [][]byte{key}, []Revision{rev}
	}
	ti.unsafeVisit(key, end, func(ki *keyIndex) bool {
		if rev, _, _, err := ki.get(ti.lg, atRev); err == nil {
			revs = append(revs, rev)
			keys = append(keys, ki.key)
		}
		return true
	})
	return keys, revs
}

func (ti *treeIndex) Tombstone(key []byte, rev Revision) error {
	keyi := &keyIndex{key: key}

	ti.Lock()
	defer ti.Unlock()
	ki, ok := ti.tree.Get(keyi)
	if !ok {
		return ErrRevisionNotFound
	}

	return ki.tombstone(ti.lg, rev.Main, rev.Sub)
}

func (ti *treeIndex) Compact(rev int64) map[Revision]struct{} {
	available := make(map[Revision]struct{})
	ti.lg.Info("compact tree index", zap.Int64("revision", rev))
	ti.Lock()
	clone := ti.tree.Clone()
	ti.Unlock()

	clone.Ascend(func(keyi *keyIndex) bool {
		// Lock is needed here to prevent modification to the keyIndex while
		// compaction is going on or revision added to empty before deletion
		ti.Lock()
		keyi.compact(ti.lg, rev, available)
		if keyi.isEmpty() {
			_, ok := ti.tree.Delete(keyi)
			if !ok {
				ti.lg.Panic("failed to delete during compaction")
			}
		}
		ti.Unlock()
		return true
	})
	return available
}

// Keep finds all revisions to be kept for a Compaction at the given rev.
func (ti *treeIndex) Keep(rev int64) map[Revision]struct{} {
	available := make(map[Revision]struct{})
	ti.RLock()
	defer ti.RUnlock()
	ti.tree.Ascend(func(keyi *keyIndex) bool {
		keyi.keep(rev, available)
		return true
	})
	return available
}

func (ti *treeIndex) Equal(bi index) bool {
	b := bi.(*treeIndex)

	if ti.tree.Len() != b.tree.Len() {
		return false
	}

	equal := true

	ti.tree.Ascend(func(aki *keyIndex) bool {
		bki, _ := b.tree.Get(aki)
		if !aki.equal(bki) {
			equal = false
			return false
		}
		return true
	})

	return equal
}

func (ti *treeIndex) Insert(ki *keyIndex) {
	ti.Lock()
	defer ti.Unlock()
	ti.tree.ReplaceOrInsert(ki)
}
