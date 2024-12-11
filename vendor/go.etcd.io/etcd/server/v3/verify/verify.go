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

package verify

import (
	"fmt"

	"go.uber.org/zap"

	"go.etcd.io/etcd/client/pkg/v3/fileutil"
	"go.etcd.io/etcd/client/pkg/v3/verify"
	"go.etcd.io/etcd/server/v3/storage/backend"
	"go.etcd.io/etcd/server/v3/storage/datadir"
	"go.etcd.io/etcd/server/v3/storage/schema"
	wal2 "go.etcd.io/etcd/server/v3/storage/wal"
	"go.etcd.io/etcd/server/v3/storage/wal/walpb"
	"go.etcd.io/raft/v3/raftpb"
)

const envVerifyValueStorageWAL verify.VerificationType = "storage_wal"

type Config struct {
	// DataDir is a root directory where the data being verified are stored.
	DataDir string

	// ExactIndex requires consistent_index in backend exactly match the last committed WAL entry.
	// Usually backend's consistent_index needs to be <= WAL.commit, but for backups the match
	// is expected to be exact.
	ExactIndex bool

	Logger *zap.Logger
}

// Verify performs consistency checks of given etcd data-directory.
// The errors are reported as the returned error, but for some situations
// the function can also panic.
// The function is expected to work on not-in-use data model, i.e.
// no file-locks should be taken. Verify does not modified the data.
func Verify(cfg Config) (retErr error) {
	lg := cfg.Logger
	if lg == nil {
		lg = zap.NewNop()
	}

	if !fileutil.Exist(datadir.ToBackendFileName(cfg.DataDir)) {
		lg.Info("verification skipped due to non exist db file")
		return nil
	}

	lg.Info("verification of persisted state", zap.String("data-dir", cfg.DataDir))
	defer func() {
		if retErr != nil {
			lg.Error("verification of persisted state failed",
				zap.String("data-dir", cfg.DataDir),
				zap.Error(retErr))
		} else if r := recover(); r != nil {
			lg.Error("verification of persisted state failed",
				zap.String("data-dir", cfg.DataDir))
			panic(r)
		} else {
			lg.Info("verification of persisted state successful", zap.String("data-dir", cfg.DataDir))
		}
	}()

	be := backend.NewDefaultBackend(lg, datadir.ToBackendFileName(cfg.DataDir))
	defer be.Close()

	snapshot, hardstate, err := validateWAL(cfg)
	if err != nil {
		return err
	}

	// TODO: Perform validation of consistency of membership between
	// backend/members & WAL confstate (and maybe storev2 if still exists).

	return validateConsistentIndex(cfg, hardstate, snapshot, be)
}

// VerifyIfEnabled performs verification according to ETCD_VERIFY env settings.
// See Verify for more information.
func VerifyIfEnabled(cfg Config) error {
	if verify.IsVerificationEnabled(envVerifyValueStorageWAL) {
		return Verify(cfg)
	}
	return nil
}

// MustVerifyIfEnabled performs verification according to ETCD_VERIFY env settings
// and exits in case of found problems.
// See Verify for more information.
func MustVerifyIfEnabled(cfg Config) {
	if err := VerifyIfEnabled(cfg); err != nil {
		cfg.Logger.Fatal("Verification failed",
			zap.String("data-dir", cfg.DataDir),
			zap.Error(err))
	}
}

func validateConsistentIndex(cfg Config, hardstate *raftpb.HardState, snapshot *walpb.Snapshot, be backend.Backend) error {
	index, term := schema.ReadConsistentIndex(be.ReadTx())
	if cfg.ExactIndex && index != hardstate.Commit {
		return fmt.Errorf("backend.ConsistentIndex (%v) expected == WAL.HardState.commit (%v)", index, hardstate.Commit)
	}
	if cfg.ExactIndex && term != hardstate.Term {
		return fmt.Errorf("backend.Term (%v) expected == WAL.HardState.term, (%v)", term, hardstate.Term)
	}
	if index > hardstate.Commit {
		return fmt.Errorf("backend.ConsistentIndex (%v) must be <= WAL.HardState.commit (%v)", index, hardstate.Commit)
	}
	if term > hardstate.Term {
		return fmt.Errorf("backend.Term (%v) must be <= WAL.HardState.term, (%v)", term, hardstate.Term)
	}

	if index < snapshot.Index {
		return fmt.Errorf("backend.ConsistentIndex (%v) must be >= last snapshot index (%v)", index, snapshot.Index)
	}

	cfg.Logger.Info("verification: consistentIndex OK", zap.Uint64("backend-consistent-index", index), zap.Uint64("hardstate-commit", hardstate.Commit))
	return nil
}

func validateWAL(cfg Config) (*walpb.Snapshot, *raftpb.HardState, error) {
	walDir := datadir.ToWALDir(cfg.DataDir)

	walSnaps, err := wal2.ValidSnapshotEntries(cfg.Logger, walDir)
	if err != nil {
		return nil, nil, err
	}

	snapshot := walSnaps[len(walSnaps)-1]
	hardstate, err := wal2.Verify(cfg.Logger, walDir, snapshot)
	if err != nil {
		return nil, nil, err
	}
	return &snapshot, hardstate, nil
}
