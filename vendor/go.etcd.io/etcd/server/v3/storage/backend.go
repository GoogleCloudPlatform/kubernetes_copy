// Copyright 2017 The etcd Authors
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
	"fmt"
	"os"
	"time"

	"go.uber.org/zap"

	"go.etcd.io/etcd/server/v3/config"
	"go.etcd.io/etcd/server/v3/etcdserver/api/snap"
	"go.etcd.io/etcd/server/v3/storage/backend"
	"go.etcd.io/etcd/server/v3/storage/schema"
	"go.etcd.io/raft/v3/raftpb"
)

func newBackend(cfg config.ServerConfig, hooks backend.Hooks) backend.Backend {
	bcfg := backend.DefaultBackendConfig(cfg.Logger)
	bcfg.Path = cfg.BackendPath()
	bcfg.UnsafeNoFsync = cfg.UnsafeNoFsync
	if cfg.BackendBatchLimit != 0 {
		bcfg.BatchLimit = cfg.BackendBatchLimit
		if cfg.Logger != nil {
			cfg.Logger.Info("setting backend batch limit", zap.Int("batch limit", cfg.BackendBatchLimit))
		}
	}
	if cfg.BackendBatchInterval != 0 {
		bcfg.BatchInterval = cfg.BackendBatchInterval
		if cfg.Logger != nil {
			cfg.Logger.Info("setting backend batch interval", zap.Duration("batch interval", cfg.BackendBatchInterval))
		}
	}
	bcfg.BackendFreelistType = cfg.BackendFreelistType
	bcfg.Logger = cfg.Logger
	if cfg.QuotaBackendBytes > 0 && cfg.QuotaBackendBytes != DefaultQuotaBytes {
		// permit 10% excess over quota for disarm
		bcfg.MmapSize = uint64(cfg.QuotaBackendBytes + cfg.QuotaBackendBytes/10)
	}
	bcfg.Mlock = cfg.ExperimentalMemoryMlock
	bcfg.Hooks = hooks
	return backend.New(bcfg)
}

// OpenSnapshotBackend renames a snapshot db to the current etcd db and opens it.
func OpenSnapshotBackend(cfg config.ServerConfig, ss *snap.Snapshotter, snapshot raftpb.Snapshot, hooks *BackendHooks) (backend.Backend, error) {
	snapPath, err := ss.DBFilePath(snapshot.Metadata.Index)
	if err != nil {
		return nil, fmt.Errorf("failed to find database snapshot file (%w)", err)
	}
	if err := os.Rename(snapPath, cfg.BackendPath()); err != nil {
		return nil, fmt.Errorf("failed to rename database snapshot file (%w)", err)
	}
	return OpenBackend(cfg, hooks), nil
}

// OpenBackend returns a backend using the current etcd db.
func OpenBackend(cfg config.ServerConfig, hooks backend.Hooks) backend.Backend {
	fn := cfg.BackendPath()

	now, beOpened := time.Now(), make(chan backend.Backend)
	go func() {
		beOpened <- newBackend(cfg, hooks)
	}()

	defer func() {
		cfg.Logger.Info("opened backend db", zap.String("path", fn), zap.Duration("took", time.Since(now)))
	}()

	select {
	case be := <-beOpened:
		return be

	case <-time.After(10 * time.Second):
		cfg.Logger.Info(
			"db file is flocked by another process, or taking too long",
			zap.String("path", fn),
			zap.Duration("took", time.Since(now)),
		)
	}

	return <-beOpened
}

// RecoverSnapshotBackend recovers the DB from a snapshot in case etcd crashes
// before updating the backend db after persisting raft snapshot to disk,
// violating the invariant snapshot.Metadata.Index < db.consistentIndex. In this
// case, replace the db with the snapshot db sent by the leader.
func RecoverSnapshotBackend(cfg config.ServerConfig, oldbe backend.Backend, snapshot raftpb.Snapshot, beExist bool, hooks *BackendHooks) (backend.Backend, error) {
	consistentIndex := uint64(0)
	if beExist {
		consistentIndex, _ = schema.ReadConsistentIndex(oldbe.ReadTx())
	}
	if snapshot.Metadata.Index <= consistentIndex {
		cfg.Logger.Info("Skipping snapshot backend", zap.Uint64("consistent-index", consistentIndex), zap.Uint64("snapshot-index", snapshot.Metadata.Index))
		return oldbe, nil
	}
	cfg.Logger.Info("Recovering from snapshot backend", zap.Uint64("consistent-index", consistentIndex), zap.Uint64("snapshot-index", snapshot.Metadata.Index))
	oldbe.Close()
	return OpenSnapshotBackend(cfg, snap.New(cfg.Logger, cfg.SnapDir()), snapshot, hooks)
}
