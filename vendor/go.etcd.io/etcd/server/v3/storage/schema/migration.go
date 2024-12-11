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
	"fmt"

	"github.com/coreos/go-semver/semver"
	"go.uber.org/zap"

	"go.etcd.io/etcd/api/v3/version"
	"go.etcd.io/etcd/server/v3/storage/backend"
)

type migrationPlan []migrationStep

func newPlan(lg *zap.Logger, current semver.Version, target semver.Version) (plan migrationPlan, err error) {
	current = trimToMinor(current)
	target = trimToMinor(target)
	if current.Major != target.Major {
		lg.Error("Changing major storage version is not supported",
			zap.String("storage-version", current.String()),
			zap.String("target-storage-version", target.String()),
		)
		return plan, fmt.Errorf("changing major storage version is not supported")
	}
	for !current.Equal(target) {
		isUpgrade := current.Minor < target.Minor

		changes, err := schemaChangesForVersion(current, isUpgrade)
		if err != nil {
			return plan, err
		}
		step := newMigrationStep(current, isUpgrade, changes)
		plan = append(plan, step)
		current = step.target
	}
	return plan, nil
}

func (p migrationPlan) Execute(lg *zap.Logger, tx backend.BatchTx) error {
	tx.LockOutsideApply()
	defer tx.Unlock()
	return p.unsafeExecute(lg, tx)
}

func (p migrationPlan) unsafeExecute(lg *zap.Logger, tx backend.UnsafeReadWriter) (err error) {
	for _, s := range p {
		err = s.unsafeExecute(lg, tx)
		if err != nil {
			return err
		}
		lg.Info("updated storage version", zap.String("new-storage-version", s.target.String()))
	}
	return nil
}

// migrationStep represents a single migrationStep of migrating etcd storage between two minor versions.
type migrationStep struct {
	target  semver.Version
	actions ActionList
}

func newMigrationStep(v semver.Version, isUpgrade bool, changes []schemaChange) (step migrationStep) {
	step.actions = make(ActionList, len(changes))
	for i, change := range changes {
		if isUpgrade {
			step.actions[i] = change.upgradeAction()
		} else {
			step.actions[len(changes)-1-i] = change.downgradeAction()
		}
	}
	if isUpgrade {
		step.target = semver.Version{Major: v.Major, Minor: v.Minor + 1}
	} else {
		step.target = semver.Version{Major: v.Major, Minor: v.Minor - 1}
	}
	return step
}

// unsafeExecute is non thread-safe version of execute.
func (s migrationStep) unsafeExecute(lg *zap.Logger, tx backend.UnsafeReadWriter) error {
	err := s.actions.unsafeExecute(lg, tx)
	if err != nil {
		return err
	}
	// Storage version is available since v3.6, downgrading target v3.5 should clean this field.
	if !s.target.LessThan(version.V3_6) {
		UnsafeSetStorageVersion(tx, &s.target)
	}
	return nil
}

func trimToMinor(ver semver.Version) semver.Version {
	return semver.Version{Major: ver.Major, Minor: ver.Minor}
}
