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

package storage

import (
	"errors"
	"sync"

	"github.com/coreos/go-semver/semver"
	"go.uber.org/zap"

	"go.etcd.io/etcd/server/v3/etcdserver/api/snap"
	"go.etcd.io/etcd/server/v3/storage/wal"
	"go.etcd.io/etcd/server/v3/storage/wal/walpb"
	"go.etcd.io/raft/v3/raftpb"
)

type Storage interface {
	// Save function saves ents and state to the underlying stable storage.
	// Save MUST block until st and ents are on stable storage.
	Save(st raftpb.HardState, ents []raftpb.Entry) error
	// SaveSnap function saves snapshot to the underlying stable storage.
	SaveSnap(snap raftpb.Snapshot) error
	// Close closes the Storage and performs finalization.
	Close() error
	// Release releases the locked wal files older than the provided snapshot.
	Release(snap raftpb.Snapshot) error
	// Sync WAL
	Sync() error
	// MinimalEtcdVersion returns minimal etcd storage able to interpret WAL log.
	MinimalEtcdVersion() *semver.Version
}

type storage struct {
	lg *zap.Logger
	s  *snap.Snapshotter

	// Mutex protected variables
	mux sync.RWMutex
	w   *wal.WAL
}

func NewStorage(lg *zap.Logger, w *wal.WAL, s *snap.Snapshotter) Storage {
	return &storage{lg: lg, w: w, s: s}
}

// SaveSnap saves the snapshot file to disk and writes the WAL snapshot entry.
func (st *storage) SaveSnap(snap raftpb.Snapshot) error {
	st.mux.RLock()
	defer st.mux.RUnlock()
	walsnap := walpb.Snapshot{
		Index:     snap.Metadata.Index,
		Term:      snap.Metadata.Term,
		ConfState: &snap.Metadata.ConfState,
	}
	// save the snapshot file before writing the snapshot to the wal.
	// This makes it possible for the snapshot file to become orphaned, but prevents
	// a WAL snapshot entry from having no corresponding snapshot file.
	err := st.s.SaveSnap(snap)
	if err != nil {
		return err
	}
	// gofail: var raftBeforeWALSaveSnaphot struct{}

	return st.w.SaveSnapshot(walsnap)
}

// Release releases resources older than the given snap and are no longer needed:
// - releases the locks to the wal files that are older than the provided wal for the given snap.
// - deletes any .snap.db files that are older than the given snap.
func (st *storage) Release(snap raftpb.Snapshot) error {
	st.mux.RLock()
	defer st.mux.RUnlock()
	if err := st.w.ReleaseLockTo(snap.Metadata.Index); err != nil {
		return err
	}
	return st.s.ReleaseSnapDBs(snap)
}

func (st *storage) Save(s raftpb.HardState, ents []raftpb.Entry) error {
	st.mux.RLock()
	defer st.mux.RUnlock()
	return st.w.Save(s, ents)
}

func (st *storage) Close() error {
	st.mux.Lock()
	defer st.mux.Unlock()
	return st.w.Close()
}

func (st *storage) Sync() error {
	st.mux.RLock()
	defer st.mux.RUnlock()
	return st.w.Sync()
}

func (st *storage) MinimalEtcdVersion() *semver.Version {
	st.mux.Lock()
	defer st.mux.Unlock()
	walsnap := walpb.Snapshot{}

	sn, err := st.s.Load()
	if err != nil && !errors.Is(err, snap.ErrNoSnapshot) {
		panic(err)
	}
	if sn != nil {
		walsnap.Index = sn.Metadata.Index
		walsnap.Term = sn.Metadata.Term
		walsnap.ConfState = &sn.Metadata.ConfState
	}
	w, err := st.w.Reopen(st.lg, walsnap)
	if err != nil {
		panic(err)
	}
	_, _, ents, err := w.ReadAll()
	if err != nil {
		panic(err)
	}
	v := wal.MinimalEtcdVersion(ents)
	st.w = w
	return v
}
