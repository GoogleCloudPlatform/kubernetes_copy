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

package membership

import (
	"path"

	"github.com/coreos/go-semver/semver"
	"go.uber.org/zap"

	"go.etcd.io/etcd/client/pkg/v3/types"
	"go.etcd.io/etcd/server/v3/etcdserver/version"
)

type MembershipBackend interface {
	ClusterVersionBackend
	MemberBackend
	DowngradeInfoBackend
	MustCreateBackendBuckets()
}

type ClusterVersionBackend interface {
	ClusterVersionFromBackend() *semver.Version
	MustSaveClusterVersionToBackend(version *semver.Version)
}

type MemberBackend interface {
	MustReadMembersFromBackend() (map[types.ID]*Member, map[types.ID]bool)
	MustSaveMemberToBackend(*Member)
	TrimMembershipFromBackend() error
	MustDeleteMemberFromBackend(types.ID)
}

type DowngradeInfoBackend interface {
	MustSaveDowngradeToBackend(*version.DowngradeInfo)
	DowngradeInfoFromBackend() *version.DowngradeInfo
}

func MustParseMemberIDFromKey(lg *zap.Logger, key string) types.ID {
	id, err := types.IDFromString(path.Base(key))
	if err != nil {
		lg.Panic("failed to parse member id from key", zap.Error(err))
	}
	return id
}
