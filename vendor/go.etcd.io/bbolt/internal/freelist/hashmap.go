package freelist

import (
	"fmt"
	"reflect"
	"sort"

	"go.etcd.io/bbolt/internal/common"
)

// pidSet holds the set of starting pgids which have the same span size
type pidSet map[common.Pgid]struct{}

type hashMap struct {
	*shared

	freePagesCount uint64                 // count of free pages(hashmap version)
	freemaps       map[uint64]pidSet      // key is the size of continuous pages(span), value is a set which contains the starting pgids of same size
	forwardMap     map[common.Pgid]uint64 // key is start pgid, value is its span size
	backwardMap    map[common.Pgid]uint64 // key is end pgid, value is its span size
}

func (f *hashMap) Init(pgids common.Pgids) {
	// reset the counter when freelist init
	f.freePagesCount = 0
	f.freemaps = make(map[uint64]pidSet)
	f.forwardMap = make(map[common.Pgid]uint64)
	f.backwardMap = make(map[common.Pgid]uint64)

	if len(pgids) == 0 {
		return
	}

	if !sort.SliceIsSorted([]common.Pgid(pgids), func(i, j int) bool { return pgids[i] < pgids[j] }) {
		panic("pgids not sorted")
	}

	size := uint64(1)
	start := pgids[0]

	for i := 1; i < len(pgids); i++ {
		// continuous page
		if pgids[i] == pgids[i-1]+1 {
			size++
		} else {
			f.addSpan(start, size)

			size = 1
			start = pgids[i]
		}
	}

	// init the tail
	if size != 0 && start != 0 {
		f.addSpan(start, size)
	}

	f.reindex()
}

func (f *hashMap) Allocate(txid common.Txid, n int) common.Pgid {
	if n == 0 {
		return 0
	}

	// if we have a exact size match just return short path
	if bm, ok := f.freemaps[uint64(n)]; ok {
		for pid := range bm {
			// remove the span
			f.delSpan(pid, uint64(n))

			f.allocs[pid] = txid

			for i := common.Pgid(0); i < common.Pgid(n); i++ {
				delete(f.cache, pid+i)
			}
			return pid
		}
	}

	// lookup the map to find larger span
	for size, bm := range f.freemaps {
		if size < uint64(n) {
			continue
		}

		for pid := range bm {
			// remove the initial
			f.delSpan(pid, size)

			f.allocs[pid] = txid

			remain := size - uint64(n)

			// add remain span
			f.addSpan(pid+common.Pgid(n), remain)

			for i := common.Pgid(0); i < common.Pgid(n); i++ {
				delete(f.cache, pid+i)
			}
			return pid
		}
	}

	return 0
}

func (f *hashMap) FreeCount() int {
	common.Verify(func() {
		expectedFreePageCount := f.hashmapFreeCountSlow()
		common.Assert(int(f.freePagesCount) == expectedFreePageCount,
			"freePagesCount (%d) is out of sync with free pages map (%d)", f.freePagesCount, expectedFreePageCount)
	})
	return int(f.freePagesCount)
}

func (f *hashMap) freePageIds() common.Pgids {
	count := f.FreeCount()
	if count == 0 {
		return common.Pgids{}
	}

	m := make([]common.Pgid, 0, count)

	startPageIds := make([]common.Pgid, 0, len(f.forwardMap))
	for k := range f.forwardMap {
		startPageIds = append(startPageIds, k)
	}
	sort.Sort(common.Pgids(startPageIds))

	for _, start := range startPageIds {
		if size, ok := f.forwardMap[start]; ok {
			for i := 0; i < int(size); i++ {
				m = append(m, start+common.Pgid(i))
			}
		}
	}

	return m
}

func (f *hashMap) hashmapFreeCountSlow() int {
	count := 0
	for _, size := range f.forwardMap {
		count += int(size)
	}
	return count
}

func (f *hashMap) addSpan(start common.Pgid, size uint64) {
	f.backwardMap[start-1+common.Pgid(size)] = size
	f.forwardMap[start] = size
	if _, ok := f.freemaps[size]; !ok {
		f.freemaps[size] = make(map[common.Pgid]struct{})
	}

	f.freemaps[size][start] = struct{}{}
	f.freePagesCount += size
}

func (f *hashMap) delSpan(start common.Pgid, size uint64) {
	delete(f.forwardMap, start)
	delete(f.backwardMap, start+common.Pgid(size-1))
	delete(f.freemaps[size], start)
	if len(f.freemaps[size]) == 0 {
		delete(f.freemaps, size)
	}
	f.freePagesCount -= size
}

func (f *hashMap) mergeSpans(ids common.Pgids) {
	common.Verify(func() {
		ids1Freemap := f.idsFromFreemaps()
		ids2Forward := f.idsFromForwardMap()
		ids3Backward := f.idsFromBackwardMap()

		if !reflect.DeepEqual(ids1Freemap, ids2Forward) {
			panic(fmt.Sprintf("Detected mismatch, f.freemaps: %v, f.forwardMap: %v", f.freemaps, f.forwardMap))
		}
		if !reflect.DeepEqual(ids1Freemap, ids3Backward) {
			panic(fmt.Sprintf("Detected mismatch, f.freemaps: %v, f.backwardMap: %v", f.freemaps, f.backwardMap))
		}

		sort.Sort(ids)
		prev := common.Pgid(0)
		for _, id := range ids {
			// The ids shouldn't have duplicated free ID.
			if prev == id {
				panic(fmt.Sprintf("detected duplicated free ID: %d in ids: %v", id, ids))
			}
			prev = id

			// The ids shouldn't have any overlap with the existing f.freemaps.
			if _, ok := ids1Freemap[id]; ok {
				panic(fmt.Sprintf("detected overlapped free page ID: %d between ids: %v and existing f.freemaps: %v", id, ids, f.freemaps))
			}
		}
	})
	for _, id := range ids {
		// try to see if we can merge and update
		f.mergeWithExistingSpan(id)
	}
}

// mergeWithExistingSpan merges pid to the existing free spans, try to merge it backward and forward
func (f *hashMap) mergeWithExistingSpan(pid common.Pgid) {
	prev := pid - 1
	next := pid + 1

	preSize, mergeWithPrev := f.backwardMap[prev]
	nextSize, mergeWithNext := f.forwardMap[next]
	newStart := pid
	newSize := uint64(1)

	if mergeWithPrev {
		//merge with previous span
		start := prev + 1 - common.Pgid(preSize)
		f.delSpan(start, preSize)

		newStart -= common.Pgid(preSize)
		newSize += preSize
	}

	if mergeWithNext {
		// merge with next span
		f.delSpan(next, nextSize)
		newSize += nextSize
	}

	f.addSpan(newStart, newSize)
}

// idsFromFreemaps get all free page IDs from f.freemaps.
// used by test only.
func (f *hashMap) idsFromFreemaps() map[common.Pgid]struct{} {
	ids := make(map[common.Pgid]struct{})
	for size, idSet := range f.freemaps {
		for start := range idSet {
			for i := 0; i < int(size); i++ {
				id := start + common.Pgid(i)
				if _, ok := ids[id]; ok {
					panic(fmt.Sprintf("detected duplicated free page ID: %d in f.freemaps: %v", id, f.freemaps))
				}
				ids[id] = struct{}{}
			}
		}
	}
	return ids
}

// idsFromForwardMap get all free page IDs from f.forwardMap.
// used by test only.
func (f *hashMap) idsFromForwardMap() map[common.Pgid]struct{} {
	ids := make(map[common.Pgid]struct{})
	for start, size := range f.forwardMap {
		for i := 0; i < int(size); i++ {
			id := start + common.Pgid(i)
			if _, ok := ids[id]; ok {
				panic(fmt.Sprintf("detected duplicated free page ID: %d in f.forwardMap: %v", id, f.forwardMap))
			}
			ids[id] = struct{}{}
		}
	}
	return ids
}

// idsFromBackwardMap get all free page IDs from f.backwardMap.
// used by test only.
func (f *hashMap) idsFromBackwardMap() map[common.Pgid]struct{} {
	ids := make(map[common.Pgid]struct{})
	for end, size := range f.backwardMap {
		for i := 0; i < int(size); i++ {
			id := end - common.Pgid(i)
			if _, ok := ids[id]; ok {
				panic(fmt.Sprintf("detected duplicated free page ID: %d in f.backwardMap: %v", id, f.backwardMap))
			}
			ids[id] = struct{}{}
		}
	}
	return ids
}

func NewHashMapFreelist() Interface {
	hm := &hashMap{
		shared:      newShared(),
		freemaps:    make(map[uint64]pidSet),
		forwardMap:  make(map[common.Pgid]uint64),
		backwardMap: make(map[common.Pgid]uint64),
	}
	hm.Interface = hm
	return hm
}
