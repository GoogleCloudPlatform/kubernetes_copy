package freelist

import (
	"fmt"
	"sort"

	"go.etcd.io/bbolt/internal/common"
)

type array struct {
	*shared

	ids []common.Pgid // all free and available free page ids.
}

func (f *array) Init(ids common.Pgids) {
	f.ids = ids
	f.reindex()
}

func (f *array) Allocate(txid common.Txid, n int) common.Pgid {
	if len(f.ids) == 0 {
		return 0
	}

	var initial, previd common.Pgid
	for i, id := range f.ids {
		if id <= 1 {
			panic(fmt.Sprintf("invalid page allocation: %d", id))
		}

		// Reset initial page if this is not contiguous.
		if previd == 0 || id-previd != 1 {
			initial = id
		}

		// If we found a contiguous block then remove it and return it.
		if (id-initial)+1 == common.Pgid(n) {
			// If we're allocating off the beginning then take the fast path
			// and just adjust the existing slice. This will use extra memory
			// temporarily but the append() in free() will realloc the slice
			// as is necessary.
			if (i + 1) == n {
				f.ids = f.ids[i+1:]
			} else {
				copy(f.ids[i-n+1:], f.ids[i+1:])
				f.ids = f.ids[:len(f.ids)-n]
			}

			// Remove from the free cache.
			for i := common.Pgid(0); i < common.Pgid(n); i++ {
				delete(f.cache, initial+i)
			}
			f.allocs[initial] = txid
			return initial
		}

		previd = id
	}
	return 0
}

func (f *array) FreeCount() int {
	return len(f.ids)
}

func (f *array) freePageIds() common.Pgids {
	return f.ids
}

func (f *array) mergeSpans(ids common.Pgids) {
	sort.Sort(ids)
	common.Verify(func() {
		idsIdx := make(map[common.Pgid]struct{})
		for _, id := range f.ids {
			// The existing f.ids shouldn't have duplicated free ID.
			if _, ok := idsIdx[id]; ok {
				panic(fmt.Sprintf("detected duplicated free page ID: %d in existing f.ids: %v", id, f.ids))
			}
			idsIdx[id] = struct{}{}
		}

		prev := common.Pgid(0)
		for _, id := range ids {
			// The ids shouldn't have duplicated free ID. Note page 0 and 1
			// are reserved for meta pages, so they can never be free page IDs.
			if prev == id {
				panic(fmt.Sprintf("detected duplicated free ID: %d in ids: %v", id, ids))
			}
			prev = id

			// The ids shouldn't have any overlap with the existing f.ids.
			if _, ok := idsIdx[id]; ok {
				panic(fmt.Sprintf("detected overlapped free page ID: %d between ids: %v and existing f.ids: %v", id, ids, f.ids))
			}
		}
	})
	f.ids = common.Pgids(f.ids).Merge(ids)
}

func NewArrayFreelist() Interface {
	a := &array{
		shared: newShared(),
		ids:    []common.Pgid{},
	}
	a.Interface = a
	return a
}
