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

package version

import (
	"context"

	"github.com/coreos/go-semver/semver"
	"go.uber.org/zap"
)

// Manager contains logic to manage etcd cluster version downgrade process.
type Manager struct {
	lg *zap.Logger
	s  Server
}

// NewManager returns a new manager instance
func NewManager(lg *zap.Logger, s Server) *Manager {
	return &Manager{
		lg: lg,
		s:  s,
	}
}

// DowngradeValidate validates if cluster is downloadable to provided target version and returns error if not.
func (m *Manager) DowngradeValidate(ctx context.Context, targetVersion *semver.Version) error {
	// gets leaders commit index and wait for local store to finish applying that index
	// to avoid using stale downgrade information
	err := m.s.LinearizableReadNotify(ctx)
	if err != nil {
		return err
	}
	cv := m.s.GetClusterVersion()
	allowedTargetVersion := allowedDowngradeVersion(cv)
	if !targetVersion.Equal(*allowedTargetVersion) {
		return ErrInvalidDowngradeTargetVersion
	}

	downgradeInfo := m.s.GetDowngradeInfo()
	if downgradeInfo != nil && downgradeInfo.Enabled {
		// Todo: return the downgrade status along with the error msg
		return ErrDowngradeInProcess
	}
	return nil
}

// DowngradeEnable initiates etcd cluster version downgrade process.
func (m *Manager) DowngradeEnable(ctx context.Context, targetVersion *semver.Version) error {
	// validate downgrade capability before starting downgrade
	err := m.DowngradeValidate(ctx, targetVersion)
	if err != nil {
		return err
	}
	return m.s.DowngradeEnable(ctx, targetVersion)
}

// DowngradeCancel cancels ongoing downgrade process.
func (m *Manager) DowngradeCancel(ctx context.Context) error {
	err := m.s.LinearizableReadNotify(ctx)
	if err != nil {
		return err
	}
	downgradeInfo := m.s.GetDowngradeInfo()
	if !downgradeInfo.Enabled {
		return ErrNoInflightDowngrade
	}
	return m.s.DowngradeCancel(ctx)
}
