/*
Copyright 2019 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package cache

import (
	"container/heap"
	"sync"
	"time"

	"k8s.io/utils/clock"
)

// NewExpiring returns an initialized expiring cache.
func NewExpiring() *Expiring {
	return NewExpiringWithClock(clock.RealClock{})
}

// NewExpiringWithClock is like NewExpiring but allows passing in a custom
// clock for testing.
func NewExpiringWithClock(clock Clock) *Expiring {
	return &Expiring{
		clock: clock,
		cache: make(map[interface{}]entry),
	}
}

// Expiring is a map whose entries expire after a per-entry timeout.
type Expiring struct {
	// AllowExpiredGet causes the expiration check to be skipped on Get.
	// It should only be used when a key always corresponds to the exact same value.
	// Thus when this field is true, expired keys are considered valid
	// until the next call to Set (which causes the GC to run).
	// It may not be changed concurrently with calls to Get.
	AllowExpiredGet bool

	clock Clock

	// mu protects the below fields
	mu sync.RWMutex
	// cache is the internal map that backs the cache.
	cache map[interface{}]entry
	// generation is used as a cheap resource version for cache entries. Cleanups
	// are scheduled with a key and generation. When the cleanup runs, it first
	// compares its generation with the current generation of the entry. It
	// deletes the entry iff the generation matches. This prevents cleanups
	// scheduled for earlier versions of an entry from deleting later versions of
	// an entry when Set() is called multiple times with the same key.
	//
	// The integer value of the generation of an entry is meaningless.
	generation uint64

	heap expiringHeap
}

type entry struct {
	val        interface{}
	expiry     time.Time
	generation uint64
}

// Get looks up an entry in the cache.
func (c *Expiring) Get(key interface{}) (val interface{}, ok bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.cache[key]
	if !ok {
		return nil, false
	}
	if !c.AllowExpiredGet && !c.clock.Now().Before(e.expiry) {
		return nil, false
	}
	return e.val, true
}

// Set sets a key/value/expiry entry in the map, overwriting any previous entry
// with the same key. The entry expires at the given expiry time, but its TTL
// may be lengthened or shortened by additional calls to Set(). Garbage
// collection of expired entries occurs during calls to Set(), however calls to
// Get() will not return expired entries that have not yet been garbage
// collected.
func (c *Expiring) Set(key interface{}, val interface{}, ttl time.Duration) {
	now := c.clock.Now()
	expiry := now.Add(ttl)

	c.mu.Lock()
	defer c.mu.Unlock()

	c.generation++

	c.cache[key] = entry{
		val:        val,
		expiry:     expiry,
		generation: c.generation,
	}

	// Run GC inline before pushing the new entry.
	c.gc(now)

	heap.Push(&c.heap, &expiringHeapEntry{
		key:        key,
		expiry:     expiry,
		generation: c.generation,
	})
}

// Delete deletes an entry in the map.
func (c *Expiring) Delete(key interface{}) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.del(key, 0)
}

// del deletes the entry for the given key. The generation argument is the
// generation of the entry that should be deleted. If the generation has been
// changed (e.g. if a set has occurred on an existing element but the old
// cleanup still runs), this is a noop. If the generation argument is 0, the
// entry's generation is ignored and the entry is deleted.
//
// del must be called under the write lock.
func (c *Expiring) del(key interface{}, generation uint64) {
	e, ok := c.cache[key]
	if !ok {
		return
	}
	if generation != 0 && generation != e.generation {
		return
	}
	delete(c.cache, key)
}

// Len returns the number of items in the cache.
func (c *Expiring) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.cache)
}

func (c *Expiring) gc(now time.Time) {
	for {
		// Return from gc if the heap is empty or the next element is not yet
		// expired.
		//
		// heap[0] is a peek at the next element in the heap, which is not obvious
		// from looking at the (*expiringHeap).Pop() implementation below.
		// heap.Pop() swaps the first entry with the last entry of the heap, then
		// calls (*expiringHeap).Pop() which returns the last element.
		if len(c.heap) == 0 || now.Before(c.heap[0].expiry) {
			return
		}
		cleanup := heap.Pop(&c.heap).(*expiringHeapEntry)
		c.del(cleanup.key, cleanup.generation)
	}
}

type expiringHeapEntry struct {
	key        interface{}
	expiry     time.Time
	generation uint64
}

// expiringHeap is a min-heap ordered by expiration time of its entries. The
// expiring cache uses this as a priority queue to efficiently organize entries
// which will be garbage collected once they expire.
type expiringHeap []*expiringHeapEntry

var _ heap.Interface = &expiringHeap{}

func (cq expiringHeap) Len() int {
	return len(cq)
}

func (cq expiringHeap) Less(i, j int) bool {
	return cq[i].expiry.Before(cq[j].expiry)
}

func (cq expiringHeap) Swap(i, j int) {
	cq[i], cq[j] = cq[j], cq[i]
}

func (cq *expiringHeap) Push(c interface{}) {
	*cq = append(*cq, c.(*expiringHeapEntry))
}

func (cq *expiringHeap) Pop() interface{} {
	c := (*cq)[cq.Len()-1]
	*cq = (*cq)[:cq.Len()-1]
	return c
}
