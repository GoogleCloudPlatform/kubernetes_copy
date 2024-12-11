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
	"errors"

	"github.com/coreos/go-semver/semver"
	"go.uber.org/zap"

	"go.etcd.io/etcd/api/v3/version"
)

// Monitor contains logic used by cluster leader to monitor version changes and decide on cluster version or downgrade progress.
type Monitor struct {
	lg *zap.Logger
	s  Server
}

// Server lists EtcdServer methods needed by Monitor
type Server interface {
	GetClusterVersion() *semver.Version
	GetDowngradeInfo() *DowngradeInfo
	GetMembersVersions() map[string]*version.Versions
	UpdateClusterVersion(string)
	LinearizableReadNotify(ctx context.Context) error
	DowngradeEnable(ctx context.Context, targetVersion *semver.Version) error
	DowngradeCancel(ctx context.Context) error

	GetStorageVersion() *semver.Version
	UpdateStorageVersion(semver.Version) error
}

func NewMonitor(lg *zap.Logger, storage Server) *Monitor {
	return &Monitor{
		lg: lg,
		s:  storage,
	}
}

// UpdateClusterVersionIfNeeded updates the cluster version.
func (m *Monitor) UpdateClusterVersionIfNeeded() error {
	newClusterVersion, err := m.decideClusterVersion()
	if newClusterVersion != nil {
		newClusterVersion = &semver.Version{Major: newClusterVersion.Major, Minor: newClusterVersion.Minor}
		m.s.UpdateClusterVersion(newClusterVersion.String())
	}
	return err
}

// decideClusterVersion decides whether to change cluster version and its next value.
// New cluster version is based on the members versions server and whether cluster is downgrading.
// Returns nil if cluster version should be left unchanged.
func (m *Monitor) decideClusterVersion() (*semver.Version, error) {
	clusterVersion := m.s.GetClusterVersion()
	minimalServerVersion := m.membersMinimalServerVersion()
	if clusterVersion == nil {
		if minimalServerVersion != nil {
			return minimalServerVersion, nil
		}
		return semver.New(version.MinClusterVersion), nil
	}
	if minimalServerVersion == nil {
		return nil, nil
	}
	downgrade := m.s.GetDowngradeInfo()
	if downgrade != nil && downgrade.Enabled {
		if downgrade.GetTargetVersion().Equal(*clusterVersion) {
			return nil, nil
		}
		if !isValidDowngrade(clusterVersion, downgrade.GetTargetVersion()) {
			m.lg.Error("Cannot downgrade from cluster-version to downgrade-target",
				zap.String("downgrade-target", downgrade.TargetVersion),
				zap.String("cluster-version", clusterVersion.String()),
			)
			return nil, errors.New("invalid downgrade target")
		}
		if !isValidDowngrade(minimalServerVersion, downgrade.GetTargetVersion()) {
			m.lg.Error("Cannot downgrade from minimal-server-version to downgrade-target",
				zap.String("downgrade-target", downgrade.TargetVersion),
				zap.String("minimal-server-version", minimalServerVersion.String()),
			)
			return nil, errors.New("invalid downgrade target")
		}
		return downgrade.GetTargetVersion(), nil
	}
	if clusterVersion.LessThan(*minimalServerVersion) && IsValidClusterVersionChange(clusterVersion, minimalServerVersion) {
		return minimalServerVersion, nil
	}
	return nil, nil
}

// UpdateStorageVersionIfNeeded updates the storage version if it differs from cluster version.
func (m *Monitor) UpdateStorageVersionIfNeeded() {
	cv := m.s.GetClusterVersion()
	if cv == nil {
		return
	}
	sv := m.s.GetStorageVersion()

	if sv == nil || sv.Major != cv.Major || sv.Minor != cv.Minor {
		if sv != nil {
			m.lg.Info("cluster version differs from storage version.", zap.String("cluster-version", cv.String()), zap.String("storage-version", sv.String()))
		}
		err := m.s.UpdateStorageVersion(semver.Version{Major: cv.Major, Minor: cv.Minor})
		if err != nil {
			m.lg.Error("failed to update storage version", zap.String("cluster-version", cv.String()), zap.Error(err))
			return
		}
		d := m.s.GetDowngradeInfo()
		if d != nil && d.Enabled {
			m.lg.Info(
				"The server is ready to downgrade",
				zap.String("target-version", d.TargetVersion),
				zap.String("server-version", version.Version),
			)
		}
	}
}

func (m *Monitor) CancelDowngradeIfNeeded() {
	d := m.s.GetDowngradeInfo()
	if d == nil || !d.Enabled {
		return
	}

	targetVersion := d.TargetVersion
	v := semver.Must(semver.NewVersion(targetVersion))
	if m.versionsMatchTarget(v) {
		m.lg.Info("the cluster has been downgraded", zap.String("cluster-version", targetVersion))
		err := m.s.DowngradeCancel(context.Background())
		if err != nil {
			m.lg.Warn("failed to cancel downgrade", zap.Error(err))
		}
	}
}

// membersMinimalServerVersion returns the min server version in the map, or nil if the min
// version in unknown.
// It prints out log if there is a member with a higher version than the
// local version.
func (m *Monitor) membersMinimalServerVersion() *semver.Version {
	vers := m.s.GetMembersVersions()
	var minV *semver.Version
	lv := semver.Must(semver.NewVersion(version.Version))

	for mid, ver := range vers {
		if ver == nil {
			return nil
		}
		v, err := semver.NewVersion(ver.Server)
		if err != nil {
			m.lg.Warn(
				"failed to parse server version of remote member",
				zap.String("remote-member-id", mid),
				zap.String("remote-member-version", ver.Server),
				zap.Error(err),
			)
			return nil
		}
		if lv.LessThan(*v) {
			m.lg.Warn(
				"leader found higher-versioned member",
				zap.String("local-member-version", lv.String()),
				zap.String("remote-member-id", mid),
				zap.String("remote-member-version", ver.Server),
			)
		}
		if minV == nil {
			minV = v
		} else if v.LessThan(*minV) {
			minV = v
		}
	}
	return minV
}

// versionsMatchTarget returns true if all server versions are equal to target version, otherwise return false.
// It can be used to decide the whether the cluster finishes downgrading to target version.
func (m *Monitor) versionsMatchTarget(targetVersion *semver.Version) bool {
	vers := m.s.GetMembersVersions()
	targetVersion = &semver.Version{Major: targetVersion.Major, Minor: targetVersion.Minor}
	for mid, ver := range vers {
		if ver == nil {
			return false
		}
		v, err := semver.NewVersion(ver.Server)
		if err != nil {
			m.lg.Warn(
				"failed to parse server version of remote member",
				zap.String("remote-member-id", mid),
				zap.String("remote-member-version", ver.Server),
				zap.Error(err),
			)
			return false
		}
		v = &semver.Version{Major: v.Major, Minor: v.Minor}
		if !targetVersion.Equal(*v) {
			m.lg.Warn("remotes server has mismatching etcd version",
				zap.String("remote-member-id", mid),
				zap.String("current-server-version", v.String()),
				zap.String("target-version", targetVersion.String()),
			)
			return false
		}
	}
	return true
}
