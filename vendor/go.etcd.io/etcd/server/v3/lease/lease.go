// Copyright 2022 The etcd Authors
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

package lease

import (
	"math"
	"sync"
	"time"

	"go.etcd.io/etcd/server/v3/lease/leasepb"
	"go.etcd.io/etcd/server/v3/storage/backend"
	"go.etcd.io/etcd/server/v3/storage/schema"
)

type Lease struct {
	ID           LeaseID
	ttl          int64 // time to live of the lease in seconds
	remainingTTL int64 // remaining time to live in seconds, if zero valued it is considered unset and the full ttl should be used
	// expiryMu protects concurrent accesses to expiry
	expiryMu sync.RWMutex
	// expiry is time when lease should expire. no expiration when expiry.IsZero() is true
	expiry time.Time

	// mu protects concurrent accesses to itemSet
	mu      sync.RWMutex
	itemSet map[LeaseItem]struct{}
	revokec chan struct{}
}

func NewLease(id LeaseID, ttl int64) *Lease {
	return &Lease{
		ID:      id,
		ttl:     ttl,
		itemSet: make(map[LeaseItem]struct{}),
		revokec: make(chan struct{}),
	}
}

func (l *Lease) expired() bool {
	return l.Remaining() <= 0
}

func (l *Lease) persistTo(b backend.Backend) {
	lpb := leasepb.Lease{ID: int64(l.ID), TTL: l.ttl, RemainingTTL: l.remainingTTL}
	tx := b.BatchTx()
	tx.LockInsideApply()
	defer tx.Unlock()
	schema.MustUnsafePutLease(tx, &lpb)
}

// TTL returns the TTL of the Lease.
func (l *Lease) TTL() int64 {
	return l.ttl
}

// SetLeaseItem sets the given lease item, this func is thread-safe
func (l *Lease) SetLeaseItem(item LeaseItem) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.itemSet[item] = struct{}{}
}

// getRemainingTTL returns the last checkpointed remaining TTL of the lease.
func (l *Lease) getRemainingTTL() int64 {
	if l.remainingTTL > 0 {
		return l.remainingTTL
	}
	return l.ttl
}

// refresh refreshes the expiry of the lease.
func (l *Lease) refresh(extend time.Duration) {
	newExpiry := time.Now().Add(extend + time.Duration(l.getRemainingTTL())*time.Second)
	l.expiryMu.Lock()
	defer l.expiryMu.Unlock()
	l.expiry = newExpiry
}

// forever sets the expiry of lease to be forever.
func (l *Lease) forever() {
	l.expiryMu.Lock()
	defer l.expiryMu.Unlock()
	l.expiry = forever
}

// Demoted returns true if the lease's expiry has been reset to forever.
func (l *Lease) Demoted() bool {
	l.expiryMu.Lock()
	defer l.expiryMu.Unlock()
	return l.expiry == forever
}

// Keys returns all the keys attached to the lease.
func (l *Lease) Keys() []string {
	l.mu.RLock()
	keys := make([]string, 0, len(l.itemSet))
	for k := range l.itemSet {
		keys = append(keys, k.Key)
	}
	l.mu.RUnlock()
	return keys
}

// Remaining returns the remaining time of the lease.
func (l *Lease) Remaining() time.Duration {
	l.expiryMu.RLock()
	defer l.expiryMu.RUnlock()
	if l.expiry.IsZero() {
		return time.Duration(math.MaxInt64)
	}
	return time.Until(l.expiry)
}

type LeaseItem struct {
	Key string
}

// leasesByExpiry implements the sort.Interface.
type leasesByExpiry []*Lease

func (le leasesByExpiry) Len() int           { return len(le) }
func (le leasesByExpiry) Less(i, j int) bool { return le[i].Remaining() < le[j].Remaining() }
func (le leasesByExpiry) Swap(i, j int)      { le[i], le[j] = le[j], le[i] }
