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
	"encoding/json"
	"fmt"
	"path"

	"github.com/coreos/go-semver/semver"
	"go.uber.org/zap"

	"go.etcd.io/etcd/client/pkg/v3/types"
	"go.etcd.io/etcd/server/v3/etcdserver/api/v2store"
)

const (
	// the prefix for storing membership related information in store provided by store pkg.
	storePrefix = "/0"

	attributesSuffix     = "attributes"
	raftAttributesSuffix = "raftAttributes"
)

var (
	StoreMembersPrefix        = path.Join(storePrefix, "members")
	storeRemovedMembersPrefix = path.Join(storePrefix, "removed_members")
)

// IsMetaStoreOnly verifies if the given `store` contains only
// a meta-information (members, version) that can be recovered from the
// backend (storev3) as well as opposed to user-data.
func IsMetaStoreOnly(store v2store.Store) (bool, error) {
	event, err := store.Get("/", true, false)
	if err != nil {
		return false, err
	}
	for _, n := range event.Node.Nodes {
		if n.Key != storePrefix && n.Nodes.Len() > 0 {
			return false, nil
		}
	}

	return true, nil
}

func verifyNoMembersInStore(lg *zap.Logger, s v2store.Store) {
	members, removed := membersFromStore(lg, s)
	if len(members) != 0 || len(removed) != 0 {
		lg.Panic("store has membership info")
	}
}

func mustSaveMemberToStore(lg *zap.Logger, s v2store.Store, m *Member) {
	b, err := json.Marshal(m.RaftAttributes)
	if err != nil {
		lg.Panic("failed to marshal raftAttributes", zap.Error(err))
	}
	p := path.Join(MemberStoreKey(m.ID), raftAttributesSuffix)
	if _, err := s.Create(p, false, string(b), false, v2store.TTLOptionSet{ExpireTime: v2store.Permanent}); err != nil {
		lg.Panic(
			"failed to save member to store",
			zap.String("path", p),
			zap.Error(err),
		)
	}
}

func mustDeleteMemberFromStore(lg *zap.Logger, s v2store.Store, id types.ID) {
	if _, err := s.Delete(MemberStoreKey(id), true, true); err != nil {
		lg.Panic(
			"failed to delete member from store",
			zap.String("path", MemberStoreKey(id)),
			zap.Error(err),
		)
	}

	mustAddToRemovedMembersInStore(lg, s, id)
}

func mustAddToRemovedMembersInStore(lg *zap.Logger, s v2store.Store, id types.ID) {
	if _, err := s.Create(RemovedMemberStoreKey(id), false, "", false, v2store.TTLOptionSet{ExpireTime: v2store.Permanent}); err != nil {
		lg.Panic(
			"failed to create removedMember",
			zap.String("path", RemovedMemberStoreKey(id)),
			zap.Error(err),
		)
	}
}

func mustUpdateMemberInStore(lg *zap.Logger, s v2store.Store, m *Member) {
	b, err := json.Marshal(m.RaftAttributes)
	if err != nil {
		lg.Panic("failed to marshal raftAttributes", zap.Error(err))
	}
	p := path.Join(MemberStoreKey(m.ID), raftAttributesSuffix)
	if _, err := s.Update(p, string(b), v2store.TTLOptionSet{ExpireTime: v2store.Permanent}); err != nil {
		lg.Panic(
			"failed to update raftAttributes",
			zap.String("path", p),
			zap.Error(err),
		)
	}
}

func mustUpdateMemberAttrInStore(lg *zap.Logger, s v2store.Store, m *Member) {
	b, err := json.Marshal(m.Attributes)
	if err != nil {
		lg.Panic("failed to marshal attributes", zap.Error(err))
	}
	p := path.Join(MemberStoreKey(m.ID), attributesSuffix)
	if _, err := s.Set(p, false, string(b), v2store.TTLOptionSet{ExpireTime: v2store.Permanent}); err != nil {
		lg.Panic(
			"failed to update attributes",
			zap.String("path", p),
			zap.Error(err),
		)
	}
}

func mustSaveClusterVersionToStore(lg *zap.Logger, s v2store.Store, ver *semver.Version) {
	if _, err := s.Set(StoreClusterVersionKey(), false, ver.String(), v2store.TTLOptionSet{ExpireTime: v2store.Permanent}); err != nil {
		lg.Panic(
			"failed to save cluster version to store",
			zap.String("path", StoreClusterVersionKey()),
			zap.Error(err),
		)
	}
}

// nodeToMember builds member from a key value node.
// the child nodes of the given node MUST be sorted by key.
func nodeToMember(lg *zap.Logger, n *v2store.NodeExtern) (*Member, error) {
	m := &Member{ID: MustParseMemberIDFromKey(lg, n.Key)}
	attrs := make(map[string][]byte)
	raftAttrKey := path.Join(n.Key, raftAttributesSuffix)
	attrKey := path.Join(n.Key, attributesSuffix)
	for _, nn := range n.Nodes {
		if nn.Key != raftAttrKey && nn.Key != attrKey {
			return nil, fmt.Errorf("unknown key %q", nn.Key)
		}
		attrs[nn.Key] = []byte(*nn.Value)
	}
	if data := attrs[raftAttrKey]; data != nil {
		if err := json.Unmarshal(data, &m.RaftAttributes); err != nil {
			return nil, fmt.Errorf("unmarshal raftAttributes error: %w", err)
		}
	} else {
		return nil, fmt.Errorf("raftAttributes key doesn't exist")
	}
	if data := attrs[attrKey]; data != nil {
		if err := json.Unmarshal(data, &m.Attributes); err != nil {
			return m, fmt.Errorf("unmarshal attributes error: %w", err)
		}
	}
	return m, nil
}

func StoreClusterVersionKey() string {
	return path.Join(storePrefix, "version")
}

func RemovedMemberStoreKey(id types.ID) string {
	return path.Join(storeRemovedMembersPrefix, id.String())
}

func MemberStoreKey(id types.ID) string {
	return path.Join(StoreMembersPrefix, id.String())
}

func MemberAttributesStorePath(id types.ID) string {
	return path.Join(MemberStoreKey(id), attributesSuffix)
}

func clusterVersionFromStore(lg *zap.Logger, st v2store.Store) *semver.Version {
	e, err := st.Get(path.Join(storePrefix, "version"), false, false)
	if err != nil {
		if isKeyNotFound(err) {
			return nil
		}
		lg.Panic(
			"failed to get cluster version from store",
			zap.String("path", path.Join(storePrefix, "version")),
			zap.Error(err),
		)
	}
	return semver.Must(semver.NewVersion(*e.Node.Value))
}
