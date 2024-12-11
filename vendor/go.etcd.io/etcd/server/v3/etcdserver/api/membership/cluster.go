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

package membership

import (
	"context"
	"crypto/sha1"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-semver/semver"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	"go.etcd.io/etcd/api/v3/version"
	"go.etcd.io/etcd/client/pkg/v3/types"
	"go.etcd.io/etcd/pkg/v3/netutil"
	"go.etcd.io/etcd/pkg/v3/notify"
	"go.etcd.io/etcd/server/v3/etcdserver/api/v2store"
	serverversion "go.etcd.io/etcd/server/v3/etcdserver/version"
	"go.etcd.io/raft/v3"
	"go.etcd.io/raft/v3/raftpb"
)

// RaftCluster is a list of Members that belong to the same raft cluster
type RaftCluster struct {
	lg *zap.Logger

	localID types.ID
	cid     types.ID

	v2store v2store.Store
	be      MembershipBackend

	sync.Mutex // guards the fields below
	version    *semver.Version
	members    map[types.ID]*Member
	// removed contains the ids of removed members in the cluster.
	// removed id cannot be reused.
	removed map[types.ID]bool

	downgradeInfo  *serverversion.DowngradeInfo
	maxLearners    int
	versionChanged *notify.Notifier
}

// ConfigChangeContext represents a context for confChange.
type ConfigChangeContext struct {
	Member
	// IsPromote indicates if the config change is for promoting a learner member.
	// This flag is needed because both adding a new member and promoting a learner member
	// uses the same config change type 'ConfChangeAddNode'.
	IsPromote bool `json:"isPromote"`
}

type ShouldApplyV3 bool

const (
	ApplyBoth        = ShouldApplyV3(true)
	ApplyV2storeOnly = ShouldApplyV3(false)
)

// NewClusterFromURLsMap creates a new raft cluster using provided urls map. Currently, it does not support creating
// cluster with raft learner member.
func NewClusterFromURLsMap(lg *zap.Logger, token string, urlsmap types.URLsMap, opts ...ClusterOption) (*RaftCluster, error) {
	c := NewCluster(lg, opts...)
	for name, urls := range urlsmap {
		m := NewMember(name, urls, token, nil)
		if _, ok := c.members[m.ID]; ok {
			return nil, fmt.Errorf("member exists with identical ID %v", m)
		}
		if uint64(m.ID) == raft.None {
			return nil, fmt.Errorf("cannot use %x as member id", raft.None)
		}
		c.members[m.ID] = m
	}
	c.genID()
	return c, nil
}

func NewClusterFromMembers(lg *zap.Logger, id types.ID, membs []*Member, opts ...ClusterOption) *RaftCluster {
	c := NewCluster(lg, opts...)
	c.cid = id
	for _, m := range membs {
		c.members[m.ID] = m
	}
	return c
}

func NewCluster(lg *zap.Logger, opts ...ClusterOption) *RaftCluster {
	if lg == nil {
		lg = zap.NewNop()
	}
	clOpts := newClusterOpts(opts...)

	return &RaftCluster{
		lg:            lg,
		members:       make(map[types.ID]*Member),
		removed:       make(map[types.ID]bool),
		downgradeInfo: &serverversion.DowngradeInfo{Enabled: false},
		maxLearners:   clOpts.maxLearners,
	}
}

func (c *RaftCluster) ID() types.ID { return c.cid }

func (c *RaftCluster) Members() []*Member {
	c.Lock()
	defer c.Unlock()
	var ms MembersByID
	for _, m := range c.members {
		ms = append(ms, m.Clone())
	}
	sort.Sort(ms)
	return ms
}

func (c *RaftCluster) Member(id types.ID) *Member {
	c.Lock()
	defer c.Unlock()
	return c.members[id].Clone()
}

func (c *RaftCluster) VotingMembers() []*Member {
	c.Lock()
	defer c.Unlock()
	var ms MembersByID
	for _, m := range c.members {
		if !m.IsLearner {
			ms = append(ms, m.Clone())
		}
	}
	sort.Sort(ms)
	return ms
}

// MemberByName returns a Member with the given name if exists.
// If more than one member has the given name, it will panic.
func (c *RaftCluster) MemberByName(name string) *Member {
	c.Lock()
	defer c.Unlock()
	var memb *Member
	for _, m := range c.members {
		if m.Name == name {
			if memb != nil {
				c.lg.Panic("two member with same name found", zap.String("name", name))
			}
			memb = m
		}
	}
	return memb.Clone()
}

func (c *RaftCluster) MemberIDs() []types.ID {
	c.Lock()
	defer c.Unlock()
	var ids []types.ID
	for _, m := range c.members {
		ids = append(ids, m.ID)
	}
	sort.Sort(types.IDSlice(ids))
	return ids
}

func (c *RaftCluster) IsIDRemoved(id types.ID) bool {
	c.Lock()
	defer c.Unlock()
	return c.removed[id]
}

// PeerURLs returns a list of all peer addresses.
// The returned list is sorted in ascending lexicographical order.
func (c *RaftCluster) PeerURLs() []string {
	c.Lock()
	defer c.Unlock()
	urls := make([]string, 0)
	for _, p := range c.members {
		urls = append(urls, p.PeerURLs...)
	}
	sort.Strings(urls)
	return urls
}

// ClientURLs returns a list of all client addresses.
// The returned list is sorted in ascending lexicographical order.
func (c *RaftCluster) ClientURLs() []string {
	c.Lock()
	defer c.Unlock()
	urls := make([]string, 0)
	for _, p := range c.members {
		urls = append(urls, p.ClientURLs...)
	}
	sort.Strings(urls)
	return urls
}

func (c *RaftCluster) String() string {
	c.Lock()
	defer c.Unlock()
	b := &strings.Builder{}
	fmt.Fprintf(b, "{ClusterID:%s ", c.cid)
	var ms []string
	for _, m := range c.members {
		ms = append(ms, fmt.Sprintf("%+v", m))
	}
	fmt.Fprintf(b, "Members:[%s] ", strings.Join(ms, " "))
	var ids []string
	for id := range c.removed {
		ids = append(ids, id.String())
	}
	fmt.Fprintf(b, "RemovedMemberIDs:[%s]}", strings.Join(ids, " "))
	return b.String()
}

func (c *RaftCluster) genID() {
	mIDs := c.MemberIDs()
	b := make([]byte, 8*len(mIDs))
	for i, id := range mIDs {
		binary.BigEndian.PutUint64(b[8*i:], uint64(id))
	}
	hash := sha1.Sum(b)
	c.cid = types.ID(binary.BigEndian.Uint64(hash[:8]))
}

func (c *RaftCluster) SetID(localID, cid types.ID) {
	c.localID = localID
	c.cid = cid
	c.buildMembershipMetric()
}

func (c *RaftCluster) SetStore(st v2store.Store) { c.v2store = st }

func (c *RaftCluster) SetBackend(be MembershipBackend) {
	c.be = be
	c.be.MustCreateBackendBuckets()
}

func (c *RaftCluster) SetVersionChangedNotifier(n *notify.Notifier) {
	c.versionChanged = n
}

func (c *RaftCluster) Recover(onSet func(*zap.Logger, *semver.Version)) {
	c.Lock()
	defer c.Unlock()

	if c.be != nil {
		c.version = c.be.ClusterVersionFromBackend()
		c.members, c.removed = c.be.MustReadMembersFromBackend()
	} else {
		c.version = clusterVersionFromStore(c.lg, c.v2store)
		c.members, c.removed = membersFromStore(c.lg, c.v2store)
	}
	c.buildMembershipMetric()

	if c.be != nil {
		c.downgradeInfo = c.be.DowngradeInfoFromBackend()
	}
	sv := semver.Must(semver.NewVersion(version.Version))
	if c.downgradeInfo != nil && c.downgradeInfo.Enabled {
		c.lg.Info(
			"cluster is downgrading to target version",
			zap.String("target-cluster-version", c.downgradeInfo.TargetVersion),
			zap.String("current-server-version", sv.String()),
		)
	}
	serverversion.MustDetectDowngrade(c.lg, sv, c.version)
	onSet(c.lg, c.version)

	for _, m := range c.members {
		if c.localID == m.ID {
			setIsLearnerMetric(m)
		}

		c.lg.Info(
			"recovered/added member from store",
			zap.String("cluster-id", c.cid.String()),
			zap.String("local-member-id", c.localID.String()),
			zap.String("recovered-remote-peer-id", m.ID.String()),
			zap.Strings("recovered-remote-peer-urls", m.PeerURLs),
			zap.Bool("recovered-remote-peer-is-learner", m.IsLearner),
		)
	}
	if c.version != nil {
		c.lg.Info(
			"set cluster version from store",
			zap.String("cluster-version", version.Cluster(c.version.String())),
		)
	}
}

// ValidateConfigurationChange takes a proposed ConfChange and
// ensures that it is still valid.
func (c *RaftCluster) ValidateConfigurationChange(cc raftpb.ConfChange) error {
	// TODO: this must be switched to backend as well.
	membersMap, removedMap := membersFromStore(c.lg, c.v2store)
	id := types.ID(cc.NodeID)
	if removedMap[id] {
		return ErrIDRemoved
	}
	switch cc.Type {
	case raftpb.ConfChangeAddNode, raftpb.ConfChangeAddLearnerNode:
		confChangeContext := new(ConfigChangeContext)
		if err := json.Unmarshal(cc.Context, confChangeContext); err != nil {
			c.lg.Panic("failed to unmarshal confChangeContext", zap.Error(err))
		}

		if confChangeContext.IsPromote { // promoting a learner member to voting member
			if membersMap[id] == nil {
				return ErrIDNotFound
			}
			if !membersMap[id].IsLearner {
				return ErrMemberNotLearner
			}
		} else { // adding a new member
			if membersMap[id] != nil {
				return ErrIDExists
			}

			var members []*Member
			urls := make(map[string]bool)
			for _, m := range membersMap {
				members = append(members, m)
				for _, u := range m.PeerURLs {
					urls[u] = true
				}
			}
			for _, u := range confChangeContext.Member.PeerURLs {
				if urls[u] {
					return ErrPeerURLexists
				}
			}

			if confChangeContext.Member.RaftAttributes.IsLearner && cc.Type == raftpb.ConfChangeAddLearnerNode { // the new member is a learner
				scaleUpLearners := true
				if err := ValidateMaxLearnerConfig(c.maxLearners, members, scaleUpLearners); err != nil {
					return err
				}
			}
		}
	case raftpb.ConfChangeRemoveNode:
		if membersMap[id] == nil {
			return ErrIDNotFound
		}

	case raftpb.ConfChangeUpdateNode:
		if membersMap[id] == nil {
			return ErrIDNotFound
		}
		urls := make(map[string]bool)
		for _, m := range membersMap {
			if m.ID == id {
				continue
			}
			for _, u := range m.PeerURLs {
				urls[u] = true
			}
		}
		m := new(Member)
		if err := json.Unmarshal(cc.Context, m); err != nil {
			c.lg.Panic("failed to unmarshal member", zap.Error(err))
		}
		for _, u := range m.PeerURLs {
			if urls[u] {
				return ErrPeerURLexists
			}
		}

	default:
		c.lg.Panic("unknown ConfChange type", zap.String("type", cc.Type.String()))
	}
	return nil
}

// AddMember adds a new Member into the cluster, and saves the given member's
// raftAttributes into the store. The given member should have empty attributes.
// A Member with a matching id must not exist.
func (c *RaftCluster) AddMember(m *Member, shouldApplyV3 ShouldApplyV3) {
	c.Lock()
	defer c.Unlock()
	if c.v2store != nil {
		mustSaveMemberToStore(c.lg, c.v2store, m)
	}

	if m.ID == c.localID {
		setIsLearnerMetric(m)
	}

	if c.be != nil && shouldApplyV3 {
		c.be.MustSaveMemberToBackend(m)

		c.members[m.ID] = m
		c.updateMembershipMetric(m.ID, true)

		c.lg.Info(
			"added member",
			zap.String("cluster-id", c.cid.String()),
			zap.String("local-member-id", c.localID.String()),
			zap.String("added-peer-id", m.ID.String()),
			zap.Strings("added-peer-peer-urls", m.PeerURLs),
			zap.Bool("added-peer-is-learner", m.IsLearner),
		)
	} else {
		c.lg.Info(
			"ignore already added member",
			zap.String("cluster-id", c.cid.String()),
			zap.String("local-member-id", c.localID.String()),
			zap.String("added-peer-id", m.ID.String()),
			zap.Strings("added-peer-peer-urls", m.PeerURLs),
			zap.Bool("added-peer-is-learner", m.IsLearner))
	}
}

// RemoveMember removes a member from the store.
// The given id MUST exist, or the function panics.
func (c *RaftCluster) RemoveMember(id types.ID, shouldApplyV3 ShouldApplyV3) {
	c.Lock()
	defer c.Unlock()
	if c.v2store != nil {
		mustDeleteMemberFromStore(c.lg, c.v2store, id)
	}
	if c.be != nil && shouldApplyV3 {
		c.be.MustDeleteMemberFromBackend(id)

		m, ok := c.members[id]
		delete(c.members, id)
		c.removed[id] = true
		c.updateMembershipMetric(id, false)

		if ok {
			c.lg.Info(
				"removed member",
				zap.String("cluster-id", c.cid.String()),
				zap.String("local-member-id", c.localID.String()),
				zap.String("removed-remote-peer-id", id.String()),
				zap.Strings("removed-remote-peer-urls", m.PeerURLs),
				zap.Bool("removed-remote-peer-is-learner", m.IsLearner),
			)
		} else {
			c.lg.Warn(
				"skipped removing already removed member",
				zap.String("cluster-id", c.cid.String()),
				zap.String("local-member-id", c.localID.String()),
				zap.String("removed-remote-peer-id", id.String()),
			)
		}
	} else {
		c.lg.Info(
			"ignore already removed member",
			zap.String("cluster-id", c.cid.String()),
			zap.String("local-member-id", c.localID.String()),
			zap.String("removed-remote-peer-id", id.String()),
		)
	}
}

func (c *RaftCluster) UpdateAttributes(id types.ID, attr Attributes, shouldApplyV3 ShouldApplyV3) {
	c.Lock()
	defer c.Unlock()

	if m, ok := c.members[id]; ok {
		m.Attributes = attr
		if c.v2store != nil {
			mustUpdateMemberAttrInStore(c.lg, c.v2store, m)
		}
		if c.be != nil && shouldApplyV3 {
			c.be.MustSaveMemberToBackend(m)
		}
		return
	}

	_, ok := c.removed[id]
	if !ok {
		c.lg.Panic(
			"failed to update; member unknown",
			zap.String("cluster-id", c.cid.String()),
			zap.String("local-member-id", c.localID.String()),
			zap.String("unknown-remote-peer-id", id.String()),
		)
	}

	c.lg.Warn(
		"skipped attributes update of removed member",
		zap.String("cluster-id", c.cid.String()),
		zap.String("local-member-id", c.localID.String()),
		zap.String("updated-peer-id", id.String()),
	)
}

// PromoteMember marks the member's IsLearner RaftAttributes to false.
func (c *RaftCluster) PromoteMember(id types.ID, shouldApplyV3 ShouldApplyV3) {
	c.Lock()
	defer c.Unlock()

	if c.v2store != nil {
		m := *(c.members[id])
		m.RaftAttributes.IsLearner = false
		mustUpdateMemberInStore(c.lg, c.v2store, &m)
	}

	if id == c.localID {
		isLearner.Set(0)
	}

	if c.be != nil && shouldApplyV3 {
		c.members[id].RaftAttributes.IsLearner = false
		c.updateMembershipMetric(id, true)
		c.be.MustSaveMemberToBackend(c.members[id])

		c.lg.Info(
			"promote member",
			zap.String("cluster-id", c.cid.String()),
			zap.String("local-member-id", c.localID.String()),
		)
	} else {
		c.lg.Info(
			"ignore already promoted member",
			zap.String("cluster-id", c.cid.String()),
			zap.String("local-member-id", c.localID.String()),
		)
	}
}

func (c *RaftCluster) UpdateRaftAttributes(id types.ID, raftAttr RaftAttributes, shouldApplyV3 ShouldApplyV3) {
	c.Lock()
	defer c.Unlock()

	if c.v2store != nil {
		m := *(c.members[id])
		m.RaftAttributes = raftAttr
		mustUpdateMemberInStore(c.lg, c.v2store, &m)
	}
	if c.be != nil && shouldApplyV3 {
		c.members[id].RaftAttributes = raftAttr
		c.be.MustSaveMemberToBackend(c.members[id])

		c.lg.Info(
			"updated member",
			zap.String("cluster-id", c.cid.String()),
			zap.String("local-member-id", c.localID.String()),
			zap.String("updated-remote-peer-id", id.String()),
			zap.Strings("updated-remote-peer-urls", raftAttr.PeerURLs),
			zap.Bool("updated-remote-peer-is-learner", raftAttr.IsLearner),
		)
	} else {
		c.lg.Info(
			"ignored already updated member",
			zap.String("cluster-id", c.cid.String()),
			zap.String("local-member-id", c.localID.String()),
			zap.String("updated-remote-peer-id", id.String()),
			zap.Strings("updated-remote-peer-urls", raftAttr.PeerURLs),
			zap.Bool("updated-remote-peer-is-learner", raftAttr.IsLearner),
		)
	}
}

func (c *RaftCluster) Version() *semver.Version {
	c.Lock()
	defer c.Unlock()
	if c.version == nil {
		return nil
	}
	return semver.Must(semver.NewVersion(c.version.String()))
}

func (c *RaftCluster) SetVersion(ver *semver.Version, onSet func(*zap.Logger, *semver.Version), shouldApplyV3 ShouldApplyV3) {
	c.Lock()
	defer c.Unlock()
	if c.version != nil {
		c.lg.Info(
			"updated cluster version",
			zap.String("cluster-id", c.cid.String()),
			zap.String("local-member-id", c.localID.String()),
			zap.String("from", version.Cluster(c.version.String())),
			zap.String("to", version.Cluster(ver.String())),
		)
	} else {
		c.lg.Info(
			"set initial cluster version",
			zap.String("cluster-id", c.cid.String()),
			zap.String("local-member-id", c.localID.String()),
			zap.String("cluster-version", version.Cluster(ver.String())),
		)
	}
	oldVer := c.version
	c.version = ver
	sv := semver.Must(semver.NewVersion(version.Version))
	serverversion.MustDetectDowngrade(c.lg, sv, c.version)
	if c.v2store != nil {
		mustSaveClusterVersionToStore(c.lg, c.v2store, ver)
	}
	if c.be != nil && shouldApplyV3 {
		c.be.MustSaveClusterVersionToBackend(ver)
	}
	if oldVer != nil {
		ClusterVersionMetrics.With(prometheus.Labels{"cluster_version": version.Cluster(oldVer.String())}).Set(0)
	}
	ClusterVersionMetrics.With(prometheus.Labels{"cluster_version": version.Cluster(ver.String())}).Set(1)
	if c.versionChanged != nil {
		c.versionChanged.Notify()
	}
	onSet(c.lg, ver)
}

func (c *RaftCluster) IsReadyToAddVotingMember() bool {
	nmembers := 1
	nstarted := 0

	for _, member := range c.VotingMembers() {
		if member.IsStarted() {
			nstarted++
		}
		nmembers++
	}

	if nstarted == 1 && nmembers == 2 {
		// a case of adding a new node to 1-member cluster for restoring cluster data
		// https://github.com/etcd-io/website/blob/main/content/docs/v2/admin_guide.md#restoring-the-cluster
		c.lg.Debug("number of started member is 1; can accept add member request")
		return true
	}

	nquorum := nmembers/2 + 1
	if nstarted < nquorum {
		c.lg.Warn(
			"rejecting member add; started member will be less than quorum",
			zap.Int("number-of-started-member", nstarted),
			zap.Int("quorum", nquorum),
			zap.String("cluster-id", c.cid.String()),
			zap.String("local-member-id", c.localID.String()),
		)
		return false
	}

	return true
}

func (c *RaftCluster) IsReadyToRemoveVotingMember(id uint64) bool {
	nmembers := 0
	nstarted := 0

	for _, member := range c.VotingMembers() {
		if uint64(member.ID) == id {
			continue
		}

		if member.IsStarted() {
			nstarted++
		}
		nmembers++
	}

	nquorum := nmembers/2 + 1
	if nstarted < nquorum {
		c.lg.Warn(
			"rejecting member remove; started member will be less than quorum",
			zap.Int("number-of-started-member", nstarted),
			zap.Int("quorum", nquorum),
			zap.String("cluster-id", c.cid.String()),
			zap.String("local-member-id", c.localID.String()),
		)
		return false
	}

	return true
}

func (c *RaftCluster) IsReadyToPromoteMember(id uint64) bool {
	nmembers := 1 // We count the learner to be promoted for the future quorum
	nstarted := 1 // and we also count it as started.

	for _, member := range c.VotingMembers() {
		if member.IsStarted() {
			nstarted++
		}
		nmembers++
	}

	nquorum := nmembers/2 + 1
	if nstarted < nquorum {
		c.lg.Warn(
			"rejecting member promote; started member will be less than quorum",
			zap.Int("number-of-started-member", nstarted),
			zap.Int("quorum", nquorum),
			zap.String("cluster-id", c.cid.String()),
			zap.String("local-member-id", c.localID.String()),
		)
		return false
	}

	return true
}

func membersFromStore(lg *zap.Logger, st v2store.Store) (map[types.ID]*Member, map[types.ID]bool) {
	members := make(map[types.ID]*Member)
	removed := make(map[types.ID]bool)
	e, err := st.Get(StoreMembersPrefix, true, true)
	if err != nil {
		if isKeyNotFound(err) {
			return members, removed
		}
		lg.Panic("failed to get members from store", zap.String("path", StoreMembersPrefix), zap.Error(err))
	}
	for _, n := range e.Node.Nodes {
		var m *Member
		m, err = nodeToMember(lg, n)
		if err != nil {
			lg.Panic("failed to nodeToMember", zap.Error(err))
		}
		members[m.ID] = m
	}

	e, err = st.Get(storeRemovedMembersPrefix, true, true)
	if err != nil {
		if isKeyNotFound(err) {
			return members, removed
		}
		lg.Panic(
			"failed to get removed members from store",
			zap.String("path", storeRemovedMembersPrefix),
			zap.Error(err),
		)
	}
	for _, n := range e.Node.Nodes {
		removed[MustParseMemberIDFromKey(lg, n.Key)] = true
	}
	return members, removed
}

// ValidateClusterAndAssignIDs validates the local cluster by matching the PeerURLs
// with the existing cluster. If the validation succeeds, it assigns the IDs
// from the existing cluster to the local cluster.
// If the validation fails, an error will be returned.
func ValidateClusterAndAssignIDs(lg *zap.Logger, local *RaftCluster, existing *RaftCluster) error {
	ems := existing.Members()
	lms := local.Members()
	if len(ems) != len(lms) {
		return fmt.Errorf("member count is unequal")
	}

	ctx, cancel := context.WithTimeout(context.TODO(), 30*time.Second)
	defer cancel()
	for i := range ems {
		var err error
		ok := false
		for j := range lms {
			if ok, err = netutil.URLStringsEqual(ctx, lg, ems[i].PeerURLs, lms[j].PeerURLs); ok {
				lms[j].ID = ems[i].ID
				break
			}
		}
		if !ok {
			return fmt.Errorf("PeerURLs: no match found for existing member (%v, %v), last resolver error (%w)", ems[i].ID, ems[i].PeerURLs, err)
		}
	}
	local.members = make(map[types.ID]*Member)
	for _, m := range lms {
		local.members[m.ID] = m
	}
	local.buildMembershipMetric()
	return nil
}

// IsLocalMemberLearner returns if the local member is raft learner
func (c *RaftCluster) IsLocalMemberLearner() bool {
	c.Lock()
	defer c.Unlock()
	localMember, ok := c.members[c.localID]
	if !ok {
		c.lg.Panic(
			"failed to find local ID in cluster members",
			zap.String("cluster-id", c.cid.String()),
			zap.String("local-member-id", c.localID.String()),
		)
	}
	return localMember.IsLearner
}

// DowngradeInfo returns the downgrade status of the cluster
func (c *RaftCluster) DowngradeInfo() *serverversion.DowngradeInfo {
	c.Lock()
	defer c.Unlock()
	if c.downgradeInfo == nil {
		return &serverversion.DowngradeInfo{Enabled: false}
	}
	d := &serverversion.DowngradeInfo{Enabled: c.downgradeInfo.Enabled, TargetVersion: c.downgradeInfo.TargetVersion}
	return d
}

func (c *RaftCluster) SetDowngradeInfo(d *serverversion.DowngradeInfo, shouldApplyV3 ShouldApplyV3) {
	c.Lock()
	defer c.Unlock()

	if c.be != nil && shouldApplyV3 {
		c.be.MustSaveDowngradeToBackend(d)
	}

	c.downgradeInfo = d
}

// IsMemberExist returns if the member with the given id exists in cluster.
func (c *RaftCluster) IsMemberExist(id types.ID) bool {
	c.Lock()
	_, ok := c.members[id]
	c.Unlock()

	// gofail: var afterIsMemberExist struct{}
	return ok
}

// VotingMemberIDs returns the ID of voting members in cluster.
func (c *RaftCluster) VotingMemberIDs() []types.ID {
	c.Lock()
	defer c.Unlock()
	var ids []types.ID
	for _, m := range c.members {
		if !m.IsLearner {
			ids = append(ids, m.ID)
		}
	}
	sort.Sort(types.IDSlice(ids))
	return ids
}

// buildMembershipMetric sets the knownPeers metric based on the current
// members of the cluster.
func (c *RaftCluster) buildMembershipMetric() {
	if c.localID == 0 {
		// We don't know our own id yet.
		return
	}
	for p := range c.members {
		knownPeers.WithLabelValues(c.localID.String(), p.String()).Set(1)
	}
	for p := range c.removed {
		knownPeers.WithLabelValues(c.localID.String(), p.String()).Set(0)
	}
}

// updateMembershipMetric updates the knownPeers metric to indicate that
// the given peer is now (un)known.
func (c *RaftCluster) updateMembershipMetric(peer types.ID, known bool) {
	if c.localID == 0 {
		// We don't know our own id yet.
		return
	}
	v := float64(0)
	if known {
		v = 1
	}
	knownPeers.WithLabelValues(c.localID.String(), peer.String()).Set(v)
}

// ValidateMaxLearnerConfig verifies the existing learner members in the cluster membership and an optional N+1 learner
// scale up are not more than maxLearners.
func ValidateMaxLearnerConfig(maxLearners int, members []*Member, scaleUpLearners bool) error {
	numLearners := 0
	for _, m := range members {
		if m.IsLearner {
			numLearners++
		}
	}
	// Validate config can accommodate scale up.
	if scaleUpLearners {
		numLearners++
	}

	if numLearners > maxLearners {
		return ErrTooManyLearners
	}

	return nil
}

func (c *RaftCluster) Store(store v2store.Store) {
	c.Lock()
	defer c.Unlock()

	verifyNoMembersInStore(c.lg, store)

	for _, m := range c.members {
		mustSaveMemberToStore(c.lg, store, m)
		if m.ClientURLs != nil {
			mustUpdateMemberAttrInStore(c.lg, store, m)
		}
		c.lg.Info(
			"snapshot storing member",
			zap.String("id", m.ID.String()),
			zap.Strings("peer-urls", m.PeerURLs),
			zap.Bool("is-learner", m.IsLearner),
		)
	}
	for id := range c.removed {
		//We do not need to delete the member since the store is empty.
		mustAddToRemovedMembersInStore(c.lg, store, id)
	}
	if c.version != nil {
		mustSaveClusterVersionToStore(c.lg, store, c.version)
	}
}
