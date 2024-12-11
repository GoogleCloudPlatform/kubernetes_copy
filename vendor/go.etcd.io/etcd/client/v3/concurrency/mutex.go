// Copyright 2016 The etcd Authors
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

package concurrency

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	v3 "go.etcd.io/etcd/client/v3"
)

// ErrLocked is returned by TryLock when Mutex is already locked by another session.
var (
	ErrLocked         = errors.New("mutex: Locked by another session")
	ErrSessionExpired = errors.New("mutex: session is expired")
	ErrLockReleased   = errors.New("mutex: lock has already been released")
)

// Mutex implements the sync Locker interface with etcd
type Mutex struct {
	s *Session

	pfx   string
	myKey string
	myRev int64
	hdr   *pb.ResponseHeader
}

func NewMutex(s *Session, pfx string) *Mutex {
	return &Mutex{s, pfx + "/", "", -1, nil}
}

// TryLock locks the mutex if not already locked by another session.
// If lock is held by another session, return immediately after attempting necessary cleanup
// The ctx argument is used for the sending/receiving Txn RPC.
func (m *Mutex) TryLock(ctx context.Context) error {
	resp, err := m.tryAcquire(ctx)
	if err != nil {
		return err
	}
	// if no key on prefix / the minimum rev is key, already hold the lock
	ownerKey := resp.Responses[1].GetResponseRange().Kvs
	if len(ownerKey) == 0 || ownerKey[0].CreateRevision == m.myRev {
		m.hdr = resp.Header
		return nil
	}
	client := m.s.Client()
	// Cannot lock, so delete the key
	if _, err := client.Delete(ctx, m.myKey); err != nil {
		return err
	}
	m.myKey = "\x00"
	m.myRev = -1
	return ErrLocked
}

// Lock locks the mutex with a cancelable context. If the context is canceled
// while trying to acquire the lock, the mutex tries to clean its stale lock entry.
func (m *Mutex) Lock(ctx context.Context) error {
	resp, err := m.tryAcquire(ctx)
	if err != nil {
		return err
	}
	// if no key on prefix / the minimum rev is key, already hold the lock
	ownerKey := resp.Responses[1].GetResponseRange().Kvs
	if len(ownerKey) == 0 || ownerKey[0].CreateRevision == m.myRev {
		m.hdr = resp.Header
		return nil
	}
	client := m.s.Client()
	// wait for deletion revisions prior to myKey
	// TODO: early termination if the session key is deleted before other session keys with smaller revisions.
	werr := waitDeletes(ctx, client, m.pfx, m.myRev-1)
	// release lock key if wait failed
	if werr != nil {
		m.Unlock(client.Ctx())
		return werr
	}

	// make sure the session is not expired, and the owner key still exists.
	gresp, werr := client.Get(ctx, m.myKey)
	if werr != nil {
		m.Unlock(client.Ctx())
		return werr
	}

	if len(gresp.Kvs) == 0 { // is the session key lost?
		return ErrSessionExpired
	}
	m.hdr = gresp.Header

	return nil
}

func (m *Mutex) tryAcquire(ctx context.Context) (*v3.TxnResponse, error) {
	s := m.s
	client := m.s.Client()

	m.myKey = fmt.Sprintf("%s%x", m.pfx, s.Lease())
	cmp := v3.Compare(v3.CreateRevision(m.myKey), "=", 0)
	// put self in lock waiters via myKey; oldest waiter holds lock
	put := v3.OpPut(m.myKey, "", v3.WithLease(s.Lease()))
	// reuse key in case this session already holds the lock
	get := v3.OpGet(m.myKey)
	// fetch current holder to complete uncontended path with only one RPC
	getOwner := v3.OpGet(m.pfx, v3.WithFirstCreate()...)
	resp, err := client.Txn(ctx).If(cmp).Then(put, getOwner).Else(get, getOwner).Commit()
	if err != nil {
		return nil, err
	}
	m.myRev = resp.Header.Revision
	if !resp.Succeeded {
		m.myRev = resp.Responses[0].GetResponseRange().Kvs[0].CreateRevision
	}
	return resp, nil
}

func (m *Mutex) Unlock(ctx context.Context) error {
	if m.myKey == "" || m.myRev <= 0 || m.myKey == "\x00" {
		return ErrLockReleased
	}

	if !strings.HasPrefix(m.myKey, m.pfx) {
		return fmt.Errorf("invalid key %q, it should have prefix %q", m.myKey, m.pfx)
	}

	client := m.s.Client()
	if _, err := client.Delete(ctx, m.myKey); err != nil {
		return err
	}
	m.myKey = "\x00"
	m.myRev = -1
	return nil
}

func (m *Mutex) IsOwner() v3.Cmp {
	return v3.Compare(v3.CreateRevision(m.myKey), "=", m.myRev)
}

func (m *Mutex) Key() string { return m.myKey }

// Header is the response header received from etcd on acquiring the lock.
func (m *Mutex) Header() *pb.ResponseHeader { return m.hdr }

type lockerMutex struct{ *Mutex }

func (lm *lockerMutex) Lock() {
	client := lm.s.Client()
	if err := lm.Mutex.Lock(client.Ctx()); err != nil {
		panic(err)
	}
}

func (lm *lockerMutex) Unlock() {
	client := lm.s.Client()
	if err := lm.Mutex.Unlock(client.Ctx()); err != nil {
		panic(err)
	}
}

// NewLocker creates a sync.Locker backed by an etcd mutex.
func NewLocker(s *Session, pfx string) sync.Locker {
	return &lockerMutex{NewMutex(s, pfx)}
}
