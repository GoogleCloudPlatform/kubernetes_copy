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

package etcdserver

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	errorspkg "errors"
	"strconv"
	"time"

	"github.com/gogo/protobuf/proto"
	"go.uber.org/zap"
	"golang.org/x/crypto/bcrypt"

	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/version"
	"go.etcd.io/etcd/pkg/v3/traceutil"
	"go.etcd.io/etcd/server/v3/auth"
	"go.etcd.io/etcd/server/v3/etcdserver/api/membership"
	apply2 "go.etcd.io/etcd/server/v3/etcdserver/apply"
	"go.etcd.io/etcd/server/v3/etcdserver/errors"
	"go.etcd.io/etcd/server/v3/etcdserver/txn"
	"go.etcd.io/etcd/server/v3/lease"
	"go.etcd.io/etcd/server/v3/lease/leasehttp"
	"go.etcd.io/etcd/server/v3/storage/mvcc"
	"go.etcd.io/raft/v3"
)

const (
	// In the health case, there might be a small gap (10s of entries) between
	// the applied index and committed index.
	// However, if the committed entries are very heavy to toApply, the gap might grow.
	// We should stop accepting new proposals if the gap growing to a certain point.
	maxGapBetweenApplyAndCommitIndex = 5000
	traceThreshold                   = 100 * time.Millisecond
	readIndexRetryTime               = 500 * time.Millisecond

	// The timeout for the node to catch up its applied index, and is used in
	// lease related operations, such as LeaseRenew and LeaseTimeToLive.
	applyTimeout = time.Second
)

type RaftKV interface {
	Range(ctx context.Context, r *pb.RangeRequest) (*pb.RangeResponse, error)
	Put(ctx context.Context, r *pb.PutRequest) (*pb.PutResponse, error)
	DeleteRange(ctx context.Context, r *pb.DeleteRangeRequest) (*pb.DeleteRangeResponse, error)
	Txn(ctx context.Context, r *pb.TxnRequest) (*pb.TxnResponse, error)
	Compact(ctx context.Context, r *pb.CompactionRequest) (*pb.CompactionResponse, error)
}

type Lessor interface {
	// LeaseGrant sends LeaseGrant request to raft and toApply it after committed.
	LeaseGrant(ctx context.Context, r *pb.LeaseGrantRequest) (*pb.LeaseGrantResponse, error)
	// LeaseRevoke sends LeaseRevoke request to raft and toApply it after committed.
	LeaseRevoke(ctx context.Context, r *pb.LeaseRevokeRequest) (*pb.LeaseRevokeResponse, error)

	// LeaseRenew renews the lease with given ID. The renewed TTL is returned. Or an error
	// is returned.
	LeaseRenew(ctx context.Context, id lease.LeaseID) (int64, error)

	// LeaseTimeToLive retrieves lease information.
	LeaseTimeToLive(ctx context.Context, r *pb.LeaseTimeToLiveRequest) (*pb.LeaseTimeToLiveResponse, error)

	// LeaseLeases lists all leases.
	LeaseLeases(ctx context.Context, r *pb.LeaseLeasesRequest) (*pb.LeaseLeasesResponse, error)
}

type Authenticator interface {
	AuthEnable(ctx context.Context, r *pb.AuthEnableRequest) (*pb.AuthEnableResponse, error)
	AuthDisable(ctx context.Context, r *pb.AuthDisableRequest) (*pb.AuthDisableResponse, error)
	AuthStatus(ctx context.Context, r *pb.AuthStatusRequest) (*pb.AuthStatusResponse, error)
	Authenticate(ctx context.Context, r *pb.AuthenticateRequest) (*pb.AuthenticateResponse, error)
	UserAdd(ctx context.Context, r *pb.AuthUserAddRequest) (*pb.AuthUserAddResponse, error)
	UserDelete(ctx context.Context, r *pb.AuthUserDeleteRequest) (*pb.AuthUserDeleteResponse, error)
	UserChangePassword(ctx context.Context, r *pb.AuthUserChangePasswordRequest) (*pb.AuthUserChangePasswordResponse, error)
	UserGrantRole(ctx context.Context, r *pb.AuthUserGrantRoleRequest) (*pb.AuthUserGrantRoleResponse, error)
	UserGet(ctx context.Context, r *pb.AuthUserGetRequest) (*pb.AuthUserGetResponse, error)
	UserRevokeRole(ctx context.Context, r *pb.AuthUserRevokeRoleRequest) (*pb.AuthUserRevokeRoleResponse, error)
	RoleAdd(ctx context.Context, r *pb.AuthRoleAddRequest) (*pb.AuthRoleAddResponse, error)
	RoleGrantPermission(ctx context.Context, r *pb.AuthRoleGrantPermissionRequest) (*pb.AuthRoleGrantPermissionResponse, error)
	RoleGet(ctx context.Context, r *pb.AuthRoleGetRequest) (*pb.AuthRoleGetResponse, error)
	RoleRevokePermission(ctx context.Context, r *pb.AuthRoleRevokePermissionRequest) (*pb.AuthRoleRevokePermissionResponse, error)
	RoleDelete(ctx context.Context, r *pb.AuthRoleDeleteRequest) (*pb.AuthRoleDeleteResponse, error)
	UserList(ctx context.Context, r *pb.AuthUserListRequest) (*pb.AuthUserListResponse, error)
	RoleList(ctx context.Context, r *pb.AuthRoleListRequest) (*pb.AuthRoleListResponse, error)
}

func (s *EtcdServer) Range(ctx context.Context, r *pb.RangeRequest) (*pb.RangeResponse, error) {
	trace := traceutil.New("range",
		s.Logger(),
		traceutil.Field{Key: "range_begin", Value: string(r.Key)},
		traceutil.Field{Key: "range_end", Value: string(r.RangeEnd)},
	)
	ctx = context.WithValue(ctx, traceutil.TraceKey{}, trace)

	var resp *pb.RangeResponse
	var err error
	defer func(start time.Time) {
		txn.WarnOfExpensiveReadOnlyRangeRequest(s.Logger(), s.Cfg.WarningApplyDuration, start, r, resp, err)
		if resp != nil {
			trace.AddField(
				traceutil.Field{Key: "response_count", Value: len(resp.Kvs)},
				traceutil.Field{Key: "response_revision", Value: resp.Header.Revision},
			)
		}
		trace.LogIfLong(traceThreshold)
	}(time.Now())

	if !r.Serializable {
		err = s.linearizableReadNotify(ctx)
		trace.Step("agreement among raft nodes before linearized reading")
		if err != nil {
			return nil, err
		}
	}
	chk := func(ai *auth.AuthInfo) error {
		return s.authStore.IsRangePermitted(ai, r.Key, r.RangeEnd)
	}

	get := func() { resp, _, err = txn.Range(ctx, s.Logger(), s.KV(), r) }
	if serr := s.doSerialize(ctx, chk, get); serr != nil {
		err = serr
		return nil, err
	}
	return resp, err
}

func (s *EtcdServer) Put(ctx context.Context, r *pb.PutRequest) (*pb.PutResponse, error) {
	ctx = context.WithValue(ctx, traceutil.StartTimeKey{}, time.Now())
	resp, err := s.raftRequest(ctx, pb.InternalRaftRequest{Put: r})
	if err != nil {
		return nil, err
	}
	return resp.(*pb.PutResponse), nil
}

func (s *EtcdServer) DeleteRange(ctx context.Context, r *pb.DeleteRangeRequest) (*pb.DeleteRangeResponse, error) {
	resp, err := s.raftRequest(ctx, pb.InternalRaftRequest{DeleteRange: r})
	if err != nil {
		return nil, err
	}
	return resp.(*pb.DeleteRangeResponse), nil
}

func (s *EtcdServer) Txn(ctx context.Context, r *pb.TxnRequest) (*pb.TxnResponse, error) {
	if txn.IsTxnReadonly(r) {
		trace := traceutil.New("transaction",
			s.Logger(),
			traceutil.Field{Key: "read_only", Value: true},
		)
		ctx = context.WithValue(ctx, traceutil.TraceKey{}, trace)
		if !txn.IsTxnSerializable(r) {
			err := s.linearizableReadNotify(ctx)
			trace.Step("agreement among raft nodes before linearized reading")
			if err != nil {
				return nil, err
			}
		}
		var resp *pb.TxnResponse
		var err error
		chk := func(ai *auth.AuthInfo) error {
			return txn.CheckTxnAuth(s.authStore, ai, r)
		}

		defer func(start time.Time) {
			txn.WarnOfExpensiveReadOnlyTxnRequest(s.Logger(), s.Cfg.WarningApplyDuration, start, r, resp, err)
			trace.LogIfLong(traceThreshold)
		}(time.Now())

		get := func() {
			resp, _, err = txn.Txn(ctx, s.Logger(), r, s.Cfg.ExperimentalTxnModeWriteWithSharedBuffer, s.KV(), s.lessor)
		}
		if serr := s.doSerialize(ctx, chk, get); serr != nil {
			return nil, serr
		}
		return resp, err
	}

	ctx = context.WithValue(ctx, traceutil.StartTimeKey{}, time.Now())
	resp, err := s.raftRequest(ctx, pb.InternalRaftRequest{Txn: r})
	if err != nil {
		return nil, err
	}
	return resp.(*pb.TxnResponse), nil
}

func (s *EtcdServer) Compact(ctx context.Context, r *pb.CompactionRequest) (*pb.CompactionResponse, error) {
	startTime := time.Now()
	result, err := s.processInternalRaftRequestOnce(ctx, pb.InternalRaftRequest{Compaction: r})
	trace := traceutil.TODO()
	if result != nil && result.Trace != nil {
		trace = result.Trace
		defer func() {
			trace.LogIfLong(traceThreshold)
		}()
		applyStart := result.Trace.GetStartTime()
		result.Trace.SetStartTime(startTime)
		trace.InsertStep(0, applyStart, "process raft request")
	}
	if r.Physical && result != nil && result.Physc != nil {
		<-result.Physc
		// The compaction is done deleting keys; the hash is now settled
		// but the data is not necessarily committed. If there's a crash,
		// the hash may revert to a hash prior to compaction completing
		// if the compaction resumes. Force the finished compaction to
		// commit so it won't resume following a crash.
		//
		// `applySnapshot` sets a new backend instance, so we need to acquire the bemu lock.
		s.bemu.RLock()
		s.be.ForceCommit()
		s.bemu.RUnlock()
		trace.Step("physically toApply compaction")
	}
	if err != nil {
		return nil, err
	}
	if result.Err != nil {
		return nil, result.Err
	}
	resp := result.Resp.(*pb.CompactionResponse)
	if resp == nil {
		resp = &pb.CompactionResponse{}
	}
	if resp.Header == nil {
		resp.Header = &pb.ResponseHeader{}
	}
	resp.Header.Revision = s.kv.Rev()
	trace.AddField(traceutil.Field{Key: "response_revision", Value: resp.Header.Revision})
	return resp, nil
}

func (s *EtcdServer) LeaseGrant(ctx context.Context, r *pb.LeaseGrantRequest) (*pb.LeaseGrantResponse, error) {
	// no id given? choose one
	for r.ID == int64(lease.NoLease) {
		// only use positive int64 id's
		r.ID = int64(s.reqIDGen.Next() & ((1 << 63) - 1))
	}
	resp, err := s.raftRequestOnce(ctx, pb.InternalRaftRequest{LeaseGrant: r})
	if err != nil {
		return nil, err
	}
	return resp.(*pb.LeaseGrantResponse), nil
}

func (s *EtcdServer) waitAppliedIndex() error {
	select {
	case <-s.ApplyWait():
	case <-s.stopping:
		return errors.ErrStopped
	case <-time.After(applyTimeout):
		return errors.ErrTimeoutWaitAppliedIndex
	}

	return nil
}

func (s *EtcdServer) LeaseRevoke(ctx context.Context, r *pb.LeaseRevokeRequest) (*pb.LeaseRevokeResponse, error) {
	resp, err := s.raftRequestOnce(ctx, pb.InternalRaftRequest{LeaseRevoke: r})
	if err != nil {
		return nil, err
	}
	return resp.(*pb.LeaseRevokeResponse), nil
}

func (s *EtcdServer) LeaseRenew(ctx context.Context, id lease.LeaseID) (int64, error) {
	if s.isLeader() {
		// If s.isLeader() returns true, but we fail to ensure the current
		// member's leadership, there are a couple of possibilities:
		//   1. current member gets stuck on writing WAL entries;
		//   2. current member is in network isolation status;
		//   3. current member isn't a leader anymore (possibly due to #1 above).
		// In such case, we just return error to client, so that the client can
		// switch to another member to continue the lease keep-alive operation.
		if !s.ensureLeadership() {
			return -1, lease.ErrNotPrimary
		}
		if err := s.waitAppliedIndex(); err != nil {
			return 0, err
		}

		ttl, err := s.lessor.Renew(id)
		if err == nil { // already requested to primary lessor(leader)
			return ttl, nil
		}
		if !errorspkg.Is(err, lease.ErrNotPrimary) {
			return -1, err
		}
	}

	cctx, cancel := context.WithTimeout(ctx, s.Cfg.ReqTimeout())
	defer cancel()

	// renewals don't go through raft; forward to leader manually
	for cctx.Err() == nil {
		leader, lerr := s.waitLeader(cctx)
		if lerr != nil {
			return -1, lerr
		}
		for _, url := range leader.PeerURLs {
			lurl := url + leasehttp.LeasePrefix
			ttl, err := leasehttp.RenewHTTP(cctx, id, lurl, s.peerRt)
			if err == nil || errorspkg.Is(err, lease.ErrLeaseNotFound) {
				return ttl, err
			}
		}
		// Throttle in case of e.g. connection problems.
		time.Sleep(50 * time.Millisecond)
	}

	if errorspkg.Is(cctx.Err(), context.DeadlineExceeded) {
		return -1, errors.ErrTimeout
	}
	return -1, errors.ErrCanceled
}

func (s *EtcdServer) checkLeaseTimeToLive(ctx context.Context, leaseID lease.LeaseID) (uint64, error) {
	rev := s.AuthStore().Revision()
	if !s.AuthStore().IsAuthEnabled() {
		return rev, nil
	}
	authInfo, err := s.AuthInfoFromCtx(ctx)
	if err != nil {
		return rev, err
	}
	if authInfo == nil {
		return rev, auth.ErrUserEmpty
	}

	l := s.lessor.Lookup(leaseID)
	if l != nil {
		for _, key := range l.Keys() {
			if err := s.AuthStore().IsRangePermitted(authInfo, []byte(key), []byte{}); err != nil {
				return 0, err
			}
		}
	}

	return rev, nil
}

func (s *EtcdServer) leaseTimeToLive(ctx context.Context, r *pb.LeaseTimeToLiveRequest) (*pb.LeaseTimeToLiveResponse, error) {
	if s.isLeader() {
		if err := s.waitAppliedIndex(); err != nil {
			return nil, err
		}

		// gofail: var beforeLookupWhenLeaseTimeToLive struct{}

		// primary; timetolive directly from leader
		le := s.lessor.Lookup(lease.LeaseID(r.ID))
		if le == nil {
			return nil, lease.ErrLeaseNotFound
		}
		// TODO: fill out ResponseHeader
		resp := &pb.LeaseTimeToLiveResponse{Header: &pb.ResponseHeader{}, ID: r.ID, TTL: int64(le.Remaining().Seconds()), GrantedTTL: le.TTL()}
		if r.Keys {
			ks := le.Keys()
			kbs := make([][]byte, len(ks))
			for i := range ks {
				kbs[i] = []byte(ks[i])
			}
			resp.Keys = kbs
		}

		// The leasor could be demoted if leader changed during lookup.
		// We should return error to force retry instead of returning
		// incorrect remaining TTL.
		if le.Demoted() {
			// NOTE: lease.ErrNotPrimary is not retryable error for
			// client. Instead, uses ErrLeaderChanged.
			return nil, errors.ErrLeaderChanged
		}
		return resp, nil
	}

	cctx, cancel := context.WithTimeout(ctx, s.Cfg.ReqTimeout())
	defer cancel()

	// forward to leader
	for cctx.Err() == nil {
		leader, err := s.waitLeader(cctx)
		if err != nil {
			return nil, err
		}
		for _, url := range leader.PeerURLs {
			lurl := url + leasehttp.LeaseInternalPrefix
			resp, err := leasehttp.TimeToLiveHTTP(cctx, lease.LeaseID(r.ID), r.Keys, lurl, s.peerRt)
			if err == nil {
				return resp.LeaseTimeToLiveResponse, nil
			}
			if errorspkg.Is(err, lease.ErrLeaseNotFound) {
				return nil, err
			}
		}
	}

	if errorspkg.Is(cctx.Err(), context.DeadlineExceeded) {
		return nil, errors.ErrTimeout
	}
	return nil, errors.ErrCanceled
}

func (s *EtcdServer) LeaseTimeToLive(ctx context.Context, r *pb.LeaseTimeToLiveRequest) (*pb.LeaseTimeToLiveResponse, error) {
	var rev uint64
	var err error
	if r.Keys {
		// check RBAC permission only if Keys is true
		rev, err = s.checkLeaseTimeToLive(ctx, lease.LeaseID(r.ID))
		if err != nil {
			return nil, err
		}
	}

	resp, err := s.leaseTimeToLive(ctx, r)
	if err != nil {
		return nil, err
	}

	if r.Keys {
		if s.AuthStore().IsAuthEnabled() && rev != s.AuthStore().Revision() {
			return nil, auth.ErrAuthOldRevision
		}
	}
	return resp, nil
}

func (s *EtcdServer) newHeader() *pb.ResponseHeader {
	return &pb.ResponseHeader{
		ClusterId: uint64(s.cluster.ID()),
		MemberId:  uint64(s.MemberID()),
		Revision:  s.KV().Rev(),
		RaftTerm:  s.Term(),
	}
}

// LeaseLeases is really ListLeases !???
func (s *EtcdServer) LeaseLeases(_ context.Context, _ *pb.LeaseLeasesRequest) (*pb.LeaseLeasesResponse, error) {
	ls := s.lessor.Leases()
	lss := make([]*pb.LeaseStatus, len(ls))
	for i := range ls {
		lss[i] = &pb.LeaseStatus{ID: int64(ls[i].ID)}
	}
	return &pb.LeaseLeasesResponse{Header: s.newHeader(), Leases: lss}, nil
}

func (s *EtcdServer) waitLeader(ctx context.Context) (*membership.Member, error) {
	leader := s.cluster.Member(s.Leader())
	for leader == nil {
		// wait an election
		dur := time.Duration(s.Cfg.ElectionTicks) * time.Duration(s.Cfg.TickMs) * time.Millisecond
		select {
		case <-time.After(dur):
			leader = s.cluster.Member(s.Leader())
		case <-s.stopping:
			return nil, errors.ErrStopped
		case <-ctx.Done():
			return nil, errors.ErrNoLeader
		}
	}
	if len(leader.PeerURLs) == 0 {
		return nil, errors.ErrNoLeader
	}
	return leader, nil
}

func (s *EtcdServer) Alarm(ctx context.Context, r *pb.AlarmRequest) (*pb.AlarmResponse, error) {
	resp, err := s.raftRequestOnce(ctx, pb.InternalRaftRequest{Alarm: r})
	if err != nil {
		return nil, err
	}
	return resp.(*pb.AlarmResponse), nil
}

func (s *EtcdServer) AuthEnable(ctx context.Context, r *pb.AuthEnableRequest) (*pb.AuthEnableResponse, error) {
	resp, err := s.raftRequestOnce(ctx, pb.InternalRaftRequest{AuthEnable: r})
	if err != nil {
		return nil, err
	}
	return resp.(*pb.AuthEnableResponse), nil
}

func (s *EtcdServer) AuthDisable(ctx context.Context, r *pb.AuthDisableRequest) (*pb.AuthDisableResponse, error) {
	resp, err := s.raftRequest(ctx, pb.InternalRaftRequest{AuthDisable: r})
	if err != nil {
		return nil, err
	}
	return resp.(*pb.AuthDisableResponse), nil
}

func (s *EtcdServer) AuthStatus(ctx context.Context, r *pb.AuthStatusRequest) (*pb.AuthStatusResponse, error) {
	resp, err := s.raftRequest(ctx, pb.InternalRaftRequest{AuthStatus: r})
	if err != nil {
		return nil, err
	}
	return resp.(*pb.AuthStatusResponse), nil
}

func (s *EtcdServer) Authenticate(ctx context.Context, r *pb.AuthenticateRequest) (*pb.AuthenticateResponse, error) {
	if err := s.linearizableReadNotify(ctx); err != nil {
		return nil, err
	}

	lg := s.Logger()

	// fix https://nvd.nist.gov/vuln/detail/CVE-2021-28235
	defer func() {
		if r != nil {
			r.Password = ""
		}
	}()

	var resp proto.Message
	for {
		checkedRevision, err := s.AuthStore().CheckPassword(r.Name, r.Password)
		if err != nil {
			if !errorspkg.Is(err, auth.ErrAuthNotEnabled) {
				lg.Warn(
					"invalid authentication was requested",
					zap.String("user", r.Name),
					zap.Error(err),
				)
			}
			return nil, err
		}

		st, err := s.AuthStore().GenTokenPrefix()
		if err != nil {
			return nil, err
		}

		// internalReq doesn't need to have Password because the above s.AuthStore().CheckPassword() already did it.
		// In addition, it will let a WAL entry not record password as a plain text.
		internalReq := &pb.InternalAuthenticateRequest{
			Name:        r.Name,
			SimpleToken: st,
		}

		resp, err = s.raftRequestOnce(ctx, pb.InternalRaftRequest{Authenticate: internalReq})
		if err != nil {
			return nil, err
		}
		if checkedRevision == s.AuthStore().Revision() {
			break
		}

		lg.Info("revision when password checked became stale; retrying")
	}

	return resp.(*pb.AuthenticateResponse), nil
}

func (s *EtcdServer) UserAdd(ctx context.Context, r *pb.AuthUserAddRequest) (*pb.AuthUserAddResponse, error) {
	if r.Options == nil || !r.Options.NoPassword {
		hashedPassword, err := bcrypt.GenerateFromPassword([]byte(r.Password), s.authStore.BcryptCost())
		if err != nil {
			return nil, err
		}
		r.HashedPassword = base64.StdEncoding.EncodeToString(hashedPassword)
		r.Password = ""
	}

	resp, err := s.raftRequest(ctx, pb.InternalRaftRequest{AuthUserAdd: r})
	if err != nil {
		return nil, err
	}
	return resp.(*pb.AuthUserAddResponse), nil
}

func (s *EtcdServer) UserDelete(ctx context.Context, r *pb.AuthUserDeleteRequest) (*pb.AuthUserDeleteResponse, error) {
	resp, err := s.raftRequest(ctx, pb.InternalRaftRequest{AuthUserDelete: r})
	if err != nil {
		return nil, err
	}
	return resp.(*pb.AuthUserDeleteResponse), nil
}

func (s *EtcdServer) UserChangePassword(ctx context.Context, r *pb.AuthUserChangePasswordRequest) (*pb.AuthUserChangePasswordResponse, error) {
	if r.Password != "" {
		hashedPassword, err := bcrypt.GenerateFromPassword([]byte(r.Password), s.authStore.BcryptCost())
		if err != nil {
			return nil, err
		}
		r.HashedPassword = base64.StdEncoding.EncodeToString(hashedPassword)
		r.Password = ""
	}

	resp, err := s.raftRequest(ctx, pb.InternalRaftRequest{AuthUserChangePassword: r})
	if err != nil {
		return nil, err
	}
	return resp.(*pb.AuthUserChangePasswordResponse), nil
}

func (s *EtcdServer) UserGrantRole(ctx context.Context, r *pb.AuthUserGrantRoleRequest) (*pb.AuthUserGrantRoleResponse, error) {
	resp, err := s.raftRequest(ctx, pb.InternalRaftRequest{AuthUserGrantRole: r})
	if err != nil {
		return nil, err
	}
	return resp.(*pb.AuthUserGrantRoleResponse), nil
}

func (s *EtcdServer) UserGet(ctx context.Context, r *pb.AuthUserGetRequest) (*pb.AuthUserGetResponse, error) {
	resp, err := s.raftRequest(ctx, pb.InternalRaftRequest{AuthUserGet: r})
	if err != nil {
		return nil, err
	}
	return resp.(*pb.AuthUserGetResponse), nil
}

func (s *EtcdServer) UserList(ctx context.Context, r *pb.AuthUserListRequest) (*pb.AuthUserListResponse, error) {
	resp, err := s.raftRequest(ctx, pb.InternalRaftRequest{AuthUserList: r})
	if err != nil {
		return nil, err
	}
	return resp.(*pb.AuthUserListResponse), nil
}

func (s *EtcdServer) UserRevokeRole(ctx context.Context, r *pb.AuthUserRevokeRoleRequest) (*pb.AuthUserRevokeRoleResponse, error) {
	resp, err := s.raftRequest(ctx, pb.InternalRaftRequest{AuthUserRevokeRole: r})
	if err != nil {
		return nil, err
	}
	return resp.(*pb.AuthUserRevokeRoleResponse), nil
}

func (s *EtcdServer) RoleAdd(ctx context.Context, r *pb.AuthRoleAddRequest) (*pb.AuthRoleAddResponse, error) {
	resp, err := s.raftRequest(ctx, pb.InternalRaftRequest{AuthRoleAdd: r})
	if err != nil {
		return nil, err
	}
	return resp.(*pb.AuthRoleAddResponse), nil
}

func (s *EtcdServer) RoleGrantPermission(ctx context.Context, r *pb.AuthRoleGrantPermissionRequest) (*pb.AuthRoleGrantPermissionResponse, error) {
	resp, err := s.raftRequest(ctx, pb.InternalRaftRequest{AuthRoleGrantPermission: r})
	if err != nil {
		return nil, err
	}
	return resp.(*pb.AuthRoleGrantPermissionResponse), nil
}

func (s *EtcdServer) RoleGet(ctx context.Context, r *pb.AuthRoleGetRequest) (*pb.AuthRoleGetResponse, error) {
	resp, err := s.raftRequest(ctx, pb.InternalRaftRequest{AuthRoleGet: r})
	if err != nil {
		return nil, err
	}
	return resp.(*pb.AuthRoleGetResponse), nil
}

func (s *EtcdServer) RoleList(ctx context.Context, r *pb.AuthRoleListRequest) (*pb.AuthRoleListResponse, error) {
	resp, err := s.raftRequest(ctx, pb.InternalRaftRequest{AuthRoleList: r})
	if err != nil {
		return nil, err
	}
	return resp.(*pb.AuthRoleListResponse), nil
}

func (s *EtcdServer) RoleRevokePermission(ctx context.Context, r *pb.AuthRoleRevokePermissionRequest) (*pb.AuthRoleRevokePermissionResponse, error) {
	resp, err := s.raftRequest(ctx, pb.InternalRaftRequest{AuthRoleRevokePermission: r})
	if err != nil {
		return nil, err
	}
	return resp.(*pb.AuthRoleRevokePermissionResponse), nil
}

func (s *EtcdServer) RoleDelete(ctx context.Context, r *pb.AuthRoleDeleteRequest) (*pb.AuthRoleDeleteResponse, error) {
	resp, err := s.raftRequest(ctx, pb.InternalRaftRequest{AuthRoleDelete: r})
	if err != nil {
		return nil, err
	}
	return resp.(*pb.AuthRoleDeleteResponse), nil
}

func (s *EtcdServer) raftRequestOnce(ctx context.Context, r pb.InternalRaftRequest) (proto.Message, error) {
	result, err := s.processInternalRaftRequestOnce(ctx, r)
	if err != nil {
		return nil, err
	}
	if result.Err != nil {
		return nil, result.Err
	}
	if startTime, ok := ctx.Value(traceutil.StartTimeKey{}).(time.Time); ok && result.Trace != nil {
		applyStart := result.Trace.GetStartTime()
		// The trace object is created in toApply. Here reset the start time to trace
		// the raft request time by the difference between the request start time
		// and toApply start time
		result.Trace.SetStartTime(startTime)
		result.Trace.InsertStep(0, applyStart, "process raft request")
		result.Trace.LogIfLong(traceThreshold)
	}
	return result.Resp, nil
}

func (s *EtcdServer) raftRequest(ctx context.Context, r pb.InternalRaftRequest) (proto.Message, error) {
	return s.raftRequestOnce(ctx, r)
}

// doSerialize handles the auth logic, with permissions checked by "chk", for a serialized request "get". Returns a non-nil error on authentication failure.
func (s *EtcdServer) doSerialize(ctx context.Context, chk func(*auth.AuthInfo) error, get func()) error {
	trace := traceutil.Get(ctx)
	ai, err := s.AuthInfoFromCtx(ctx)
	if err != nil {
		return err
	}
	if ai == nil {
		// chk expects non-nil AuthInfo; use empty credentials
		ai = &auth.AuthInfo{}
	}
	if err = chk(ai); err != nil {
		return err
	}
	trace.Step("get authentication metadata")
	// fetch response for serialized request
	get()
	// check for stale token revision in case the auth store was updated while
	// the request has been handled.
	if ai.Revision != 0 && ai.Revision != s.authStore.Revision() {
		return auth.ErrAuthOldRevision
	}
	return nil
}

func (s *EtcdServer) processInternalRaftRequestOnce(ctx context.Context, r pb.InternalRaftRequest) (*apply2.Result, error) {
	ai := s.getAppliedIndex()
	ci := s.getCommittedIndex()
	if ci > ai+maxGapBetweenApplyAndCommitIndex {
		return nil, errors.ErrTooManyRequests
	}

	r.Header = &pb.RequestHeader{
		ID: s.reqIDGen.Next(),
	}

	// check authinfo if it is not InternalAuthenticateRequest
	if r.Authenticate == nil {
		authInfo, err := s.AuthInfoFromCtx(ctx)
		if err != nil {
			return nil, err
		}
		if authInfo != nil {
			r.Header.Username = authInfo.Username
			r.Header.AuthRevision = authInfo.Revision
		}
	}

	data, err := r.Marshal()
	if err != nil {
		return nil, err
	}

	if len(data) > int(s.Cfg.MaxRequestBytes) {
		return nil, errors.ErrRequestTooLarge
	}

	id := r.ID
	if id == 0 {
		id = r.Header.ID
	}
	ch := s.w.Register(id)

	cctx, cancel := context.WithTimeout(ctx, s.Cfg.ReqTimeout())
	defer cancel()

	start := time.Now()
	err = s.r.Propose(cctx, data)
	if err != nil {
		proposalsFailed.Inc()
		s.w.Trigger(id, nil) // GC wait
		return nil, err
	}
	proposalsPending.Inc()
	defer proposalsPending.Dec()

	select {
	case x := <-ch:
		return x.(*apply2.Result), nil
	case <-cctx.Done():
		proposalsFailed.Inc()
		s.w.Trigger(id, nil) // GC wait
		return nil, s.parseProposeCtxErr(cctx.Err(), start)
	case <-s.done:
		return nil, errors.ErrStopped
	}
}

// Watchable returns a watchable interface attached to the etcdserver.
func (s *EtcdServer) Watchable() mvcc.WatchableKV { return s.KV() }

func (s *EtcdServer) linearizableReadLoop() {
	for {
		requestID := s.reqIDGen.Next()
		leaderChangedNotifier := s.leaderChanged.Receive()
		select {
		case <-leaderChangedNotifier:
			continue
		case <-s.readwaitc:
		case <-s.stopping:
			return
		}

		// as a single loop is can unlock multiple reads, it is not very useful
		// to propagate the trace from Txn or Range.
		trace := traceutil.New("linearizableReadLoop", s.Logger())

		nextnr := newNotifier()
		s.readMu.Lock()
		nr := s.readNotifier
		s.readNotifier = nextnr
		s.readMu.Unlock()

		confirmedIndex, err := s.requestCurrentIndex(leaderChangedNotifier, requestID)
		if isStopped(err) {
			return
		}
		if err != nil {
			nr.notify(err)
			continue
		}

		trace.Step("read index received")

		trace.AddField(traceutil.Field{Key: "readStateIndex", Value: confirmedIndex})

		appliedIndex := s.getAppliedIndex()
		trace.AddField(traceutil.Field{Key: "appliedIndex", Value: strconv.FormatUint(appliedIndex, 10)})

		if appliedIndex < confirmedIndex {
			select {
			case <-s.applyWait.Wait(confirmedIndex):
			case <-s.stopping:
				return
			}
		}
		// unblock all l-reads requested at indices before confirmedIndex
		nr.notify(nil)
		trace.Step("applied index is now lower than readState.Index")

		trace.LogAllStepsIfLong(traceThreshold)
	}
}

func isStopped(err error) bool {
	return errorspkg.Is(err, raft.ErrStopped) || errorspkg.Is(err, errors.ErrStopped)
}

func (s *EtcdServer) requestCurrentIndex(leaderChangedNotifier <-chan struct{}, requestID uint64) (uint64, error) {
	err := s.sendReadIndex(requestID)
	if err != nil {
		return 0, err
	}

	lg := s.Logger()
	errorTimer := time.NewTimer(s.Cfg.ReqTimeout())
	defer errorTimer.Stop()
	retryTimer := time.NewTimer(readIndexRetryTime)
	defer retryTimer.Stop()

	firstCommitInTermNotifier := s.firstCommitInTerm.Receive()

	for {
		select {
		case rs := <-s.r.readStateC:
			requestIDBytes := uint64ToBigEndianBytes(requestID)
			gotOwnResponse := bytes.Equal(rs.RequestCtx, requestIDBytes)
			if !gotOwnResponse {
				// a previous request might time out. now we should ignore the response of it and
				// continue waiting for the response of the current requests.
				responseID := uint64(0)
				if len(rs.RequestCtx) == 8 {
					responseID = binary.BigEndian.Uint64(rs.RequestCtx)
				}
				lg.Warn(
					"ignored out-of-date read index response; local node read indexes queueing up and waiting to be in sync with leader",
					zap.Uint64("sent-request-id", requestID),
					zap.Uint64("received-request-id", responseID),
				)
				slowReadIndex.Inc()
				continue
			}
			return rs.Index, nil
		case <-leaderChangedNotifier:
			readIndexFailed.Inc()
			// return a retryable error.
			return 0, errors.ErrLeaderChanged
		case <-firstCommitInTermNotifier:
			firstCommitInTermNotifier = s.firstCommitInTerm.Receive()
			lg.Info("first commit in current term: resending ReadIndex request")
			err := s.sendReadIndex(requestID)
			if err != nil {
				return 0, err
			}
			retryTimer.Reset(readIndexRetryTime)
			continue
		case <-retryTimer.C:
			lg.Warn(
				"waiting for ReadIndex response took too long, retrying",
				zap.Uint64("sent-request-id", requestID),
				zap.Duration("retry-timeout", readIndexRetryTime),
			)
			err := s.sendReadIndex(requestID)
			if err != nil {
				return 0, err
			}
			retryTimer.Reset(readIndexRetryTime)
			continue
		case <-errorTimer.C:
			lg.Warn(
				"timed out waiting for read index response (local node might have slow network)",
				zap.Duration("timeout", s.Cfg.ReqTimeout()),
			)
			slowReadIndex.Inc()
			return 0, errors.ErrTimeout
		case <-s.stopping:
			return 0, errors.ErrStopped
		}
	}
}

func uint64ToBigEndianBytes(number uint64) []byte {
	byteResult := make([]byte, 8)
	binary.BigEndian.PutUint64(byteResult, number)
	return byteResult
}

func (s *EtcdServer) sendReadIndex(requestIndex uint64) error {
	ctxToSend := uint64ToBigEndianBytes(requestIndex)

	cctx, cancel := context.WithTimeout(context.Background(), s.Cfg.ReqTimeout())
	err := s.r.ReadIndex(cctx, ctxToSend)
	cancel()
	if errorspkg.Is(err, raft.ErrStopped) {
		return err
	}
	if err != nil {
		lg := s.Logger()
		lg.Warn("failed to get read index from Raft", zap.Error(err))
		readIndexFailed.Inc()
		return err
	}
	return nil
}

func (s *EtcdServer) LinearizableReadNotify(ctx context.Context) error {
	return s.linearizableReadNotify(ctx)
}

func (s *EtcdServer) linearizableReadNotify(ctx context.Context) error {
	s.readMu.RLock()
	nc := s.readNotifier
	s.readMu.RUnlock()

	// signal linearizable loop for current notify if it hasn't been already
	select {
	case s.readwaitc <- struct{}{}:
	default:
	}

	// wait for read state notification
	select {
	case <-nc.c:
		return nc.err
	case <-ctx.Done():
		return ctx.Err()
	case <-s.done:
		return errors.ErrStopped
	}
}

func (s *EtcdServer) AuthInfoFromCtx(ctx context.Context) (*auth.AuthInfo, error) {
	authInfo, err := s.AuthStore().AuthInfoFromCtx(ctx)
	if authInfo != nil || err != nil {
		return authInfo, err
	}
	if !s.Cfg.ClientCertAuthEnabled {
		return nil, nil
	}
	authInfo = s.AuthStore().AuthInfoFromTLS(ctx)
	return authInfo, nil
}

func (s *EtcdServer) Downgrade(ctx context.Context, r *pb.DowngradeRequest) (*pb.DowngradeResponse, error) {
	switch r.Action {
	case pb.DowngradeRequest_VALIDATE:
		return s.downgradeValidate(ctx, r.Version)
	case pb.DowngradeRequest_ENABLE:
		return s.downgradeEnable(ctx, r)
	case pb.DowngradeRequest_CANCEL:
		return s.downgradeCancel(ctx)
	default:
		return nil, errors.ErrUnknownMethod
	}
}

func (s *EtcdServer) downgradeValidate(ctx context.Context, v string) (*pb.DowngradeResponse, error) {
	resp := &pb.DowngradeResponse{}

	targetVersion, err := convertToClusterVersion(v)
	if err != nil {
		return nil, err
	}

	cv := s.ClusterVersion()
	if cv == nil {
		return nil, errors.ErrClusterVersionUnavailable
	}
	resp.Version = version.Cluster(cv.String())
	err = s.Version().DowngradeValidate(ctx, targetVersion)
	if err != nil {
		return nil, err
	}

	return resp, nil
}

func (s *EtcdServer) downgradeEnable(ctx context.Context, r *pb.DowngradeRequest) (*pb.DowngradeResponse, error) {
	lg := s.Logger()
	targetVersion, err := convertToClusterVersion(r.Version)
	if err != nil {
		lg.Warn("reject downgrade request", zap.Error(err))
		return nil, err
	}
	err = s.Version().DowngradeEnable(ctx, targetVersion)
	if err != nil {
		lg.Warn("reject downgrade request", zap.Error(err))
		return nil, err
	}
	resp := pb.DowngradeResponse{Version: version.Cluster(s.ClusterVersion().String())}
	return &resp, nil
}

func (s *EtcdServer) downgradeCancel(ctx context.Context) (*pb.DowngradeResponse, error) {
	err := s.Version().DowngradeCancel(ctx)
	if err != nil {
		s.lg.Warn("failed to cancel downgrade", zap.Error(err))
	}
	resp := pb.DowngradeResponse{Version: version.Cluster(s.ClusterVersion().String())}
	return &resp, nil
}
