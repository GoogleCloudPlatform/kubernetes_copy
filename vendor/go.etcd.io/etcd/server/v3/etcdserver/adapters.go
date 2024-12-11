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

package etcdserver

import (
	"context"

	"github.com/coreos/go-semver/semver"

	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/membershippb"
	"go.etcd.io/etcd/api/v3/version"
	serverversion "go.etcd.io/etcd/server/v3/etcdserver/version"
	"go.etcd.io/etcd/server/v3/storage/schema"
)

// serverVersionAdapter implements the interface Server defined in package
// go.etcd.io/etcd/server/v3/etcdserver/version, and it's needed by Monitor
// in the same package.
type serverVersionAdapter struct {
	*EtcdServer
}

func NewServerVersionAdapter(s *EtcdServer) *serverVersionAdapter {
	return &serverVersionAdapter{
		EtcdServer: s,
	}
}

var _ serverversion.Server = (*serverVersionAdapter)(nil)

func (s *serverVersionAdapter) UpdateClusterVersion(version string) {
	s.GoAttach(func() { s.updateClusterVersionV3(version) })
}

func (s *serverVersionAdapter) LinearizableReadNotify(ctx context.Context) error {
	return s.linearizableReadNotify(ctx)
}

func (s *serverVersionAdapter) DowngradeEnable(ctx context.Context, targetVersion *semver.Version) error {
	raftRequest := membershippb.DowngradeInfoSetRequest{Enabled: true, Ver: targetVersion.String()}
	_, err := s.raftRequest(ctx, pb.InternalRaftRequest{DowngradeInfoSet: &raftRequest})
	return err
}

func (s *serverVersionAdapter) DowngradeCancel(ctx context.Context) error {
	raftRequest := membershippb.DowngradeInfoSetRequest{Enabled: false}
	_, err := s.raftRequest(ctx, pb.InternalRaftRequest{DowngradeInfoSet: &raftRequest})
	return err
}

func (s *serverVersionAdapter) GetClusterVersion() *semver.Version {
	return s.cluster.Version()
}

func (s *serverVersionAdapter) GetDowngradeInfo() *serverversion.DowngradeInfo {
	return s.cluster.DowngradeInfo()
}

func (s *serverVersionAdapter) GetMembersVersions() map[string]*version.Versions {
	return getMembersVersions(s.lg, s.cluster, s.MemberID(), s.peerRt, s.Cfg.ReqTimeout())
}

func (s *serverVersionAdapter) GetStorageVersion() *semver.Version {
	return s.StorageVersion()
}

func (s *serverVersionAdapter) UpdateStorageVersion(target semver.Version) error {
	// `applySnapshot` sets a new backend instance, so we need to acquire the bemu lock.
	s.bemu.RLock()
	defer s.bemu.RUnlock()

	tx := s.be.BatchTx()
	tx.LockOutsideApply()
	defer tx.Unlock()
	return schema.UnsafeMigrate(s.lg, tx, s.r.storage, target)
}
