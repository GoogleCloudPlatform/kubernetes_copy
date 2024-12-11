// Copyright 2016 The etcd Authors
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

package apply

import (
	"context"

	"github.com/coreos/go-semver/semver"
	"github.com/gogo/protobuf/proto"
	"go.uber.org/zap"

	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/membershippb"
	"go.etcd.io/etcd/client/pkg/v3/types"
	"go.etcd.io/etcd/pkg/v3/traceutil"
	"go.etcd.io/etcd/server/v3/auth"
	"go.etcd.io/etcd/server/v3/etcdserver/api"
	"go.etcd.io/etcd/server/v3/etcdserver/api/membership"
	"go.etcd.io/etcd/server/v3/etcdserver/api/v3alarm"
	"go.etcd.io/etcd/server/v3/etcdserver/cindex"
	"go.etcd.io/etcd/server/v3/etcdserver/errors"
	mvcctxn "go.etcd.io/etcd/server/v3/etcdserver/txn"
	"go.etcd.io/etcd/server/v3/etcdserver/version"
	"go.etcd.io/etcd/server/v3/lease"
	serverstorage "go.etcd.io/etcd/server/v3/storage"
	"go.etcd.io/etcd/server/v3/storage/backend"
	"go.etcd.io/etcd/server/v3/storage/mvcc"
)

const (
	v3Version = "v3"
)

// RaftStatusGetter represents etcd server and Raft progress.
type RaftStatusGetter interface {
	MemberID() types.ID
	Leader() types.ID
	CommittedIndex() uint64
	AppliedIndex() uint64
	Term() uint64
}

type Result struct {
	Resp proto.Message
	Err  error
	// Physc signals the physical effect of the request has completed in addition
	// to being logically reflected by the node. Currently, only used for
	// Compaction requests.
	Physc <-chan struct{}
	Trace *traceutil.Trace
}

type applyFunc func(r *pb.InternalRaftRequest) *Result

// applierV3 is the interface for processing V3 raft messages
type applierV3 interface {
	// Apply executes the generic portion of application logic for the current applier, but
	// delegates the actual execution to the applyFunc method.
	Apply(r *pb.InternalRaftRequest, applyFunc applyFunc) *Result

	Put(p *pb.PutRequest) (*pb.PutResponse, *traceutil.Trace, error)
	Range(r *pb.RangeRequest) (*pb.RangeResponse, *traceutil.Trace, error)
	DeleteRange(dr *pb.DeleteRangeRequest) (*pb.DeleteRangeResponse, *traceutil.Trace, error)
	Txn(rt *pb.TxnRequest) (*pb.TxnResponse, *traceutil.Trace, error)
	Compaction(compaction *pb.CompactionRequest) (*pb.CompactionResponse, <-chan struct{}, *traceutil.Trace, error)

	LeaseGrant(lc *pb.LeaseGrantRequest) (*pb.LeaseGrantResponse, error)
	LeaseRevoke(lc *pb.LeaseRevokeRequest) (*pb.LeaseRevokeResponse, error)

	LeaseCheckpoint(lc *pb.LeaseCheckpointRequest) (*pb.LeaseCheckpointResponse, error)

	Alarm(*pb.AlarmRequest) (*pb.AlarmResponse, error)

	Authenticate(r *pb.InternalAuthenticateRequest) (*pb.AuthenticateResponse, error)

	AuthEnable() (*pb.AuthEnableResponse, error)
	AuthDisable() (*pb.AuthDisableResponse, error)
	AuthStatus() (*pb.AuthStatusResponse, error)

	UserAdd(ua *pb.AuthUserAddRequest) (*pb.AuthUserAddResponse, error)
	UserDelete(ua *pb.AuthUserDeleteRequest) (*pb.AuthUserDeleteResponse, error)
	UserChangePassword(ua *pb.AuthUserChangePasswordRequest) (*pb.AuthUserChangePasswordResponse, error)
	UserGrantRole(ua *pb.AuthUserGrantRoleRequest) (*pb.AuthUserGrantRoleResponse, error)
	UserGet(ua *pb.AuthUserGetRequest) (*pb.AuthUserGetResponse, error)
	UserRevokeRole(ua *pb.AuthUserRevokeRoleRequest) (*pb.AuthUserRevokeRoleResponse, error)
	RoleAdd(ua *pb.AuthRoleAddRequest) (*pb.AuthRoleAddResponse, error)
	RoleGrantPermission(ua *pb.AuthRoleGrantPermissionRequest) (*pb.AuthRoleGrantPermissionResponse, error)
	RoleGet(ua *pb.AuthRoleGetRequest) (*pb.AuthRoleGetResponse, error)
	RoleRevokePermission(ua *pb.AuthRoleRevokePermissionRequest) (*pb.AuthRoleRevokePermissionResponse, error)
	RoleDelete(ua *pb.AuthRoleDeleteRequest) (*pb.AuthRoleDeleteResponse, error)
	UserList(ua *pb.AuthUserListRequest) (*pb.AuthUserListResponse, error)
	RoleList(ua *pb.AuthRoleListRequest) (*pb.AuthRoleListResponse, error)
}

type SnapshotServer interface {
	ForceSnapshot()
}

type applierV3backend struct {
	lg              *zap.Logger
	kv              mvcc.KV
	alarmStore      *v3alarm.AlarmStore
	authStore       auth.AuthStore
	lessor          lease.Lessor
	cluster         *membership.RaftCluster
	raftStatus      RaftStatusGetter
	snapshotServer  SnapshotServer
	consistentIndex cindex.ConsistentIndexer

	txnModeWriteWithSharedBuffer bool
}

func newApplierV3Backend(
	lg *zap.Logger,
	kv mvcc.KV,
	alarmStore *v3alarm.AlarmStore,
	authStore auth.AuthStore,
	lessor lease.Lessor,
	cluster *membership.RaftCluster,
	raftStatus RaftStatusGetter,
	snapshotServer SnapshotServer,
	consistentIndex cindex.ConsistentIndexer,
	txnModeWriteWithSharedBuffer bool) applierV3 {
	return &applierV3backend{
		lg:                           lg,
		kv:                           kv,
		alarmStore:                   alarmStore,
		authStore:                    authStore,
		lessor:                       lessor,
		cluster:                      cluster,
		raftStatus:                   raftStatus,
		snapshotServer:               snapshotServer,
		consistentIndex:              consistentIndex,
		txnModeWriteWithSharedBuffer: txnModeWriteWithSharedBuffer}
}

func (a *applierV3backend) Apply(r *pb.InternalRaftRequest, applyFunc applyFunc) *Result {
	return applyFunc(r)
}

func (a *applierV3backend) Put(p *pb.PutRequest) (resp *pb.PutResponse, trace *traceutil.Trace, err error) {
	return mvcctxn.Put(context.TODO(), a.lg, a.lessor, a.kv, p)
}

func (a *applierV3backend) DeleteRange(dr *pb.DeleteRangeRequest) (*pb.DeleteRangeResponse, *traceutil.Trace, error) {
	return mvcctxn.DeleteRange(context.TODO(), a.lg, a.kv, dr)
}

func (a *applierV3backend) Range(r *pb.RangeRequest) (*pb.RangeResponse, *traceutil.Trace, error) {
	return mvcctxn.Range(context.TODO(), a.lg, a.kv, r)
}

func (a *applierV3backend) Txn(rt *pb.TxnRequest) (*pb.TxnResponse, *traceutil.Trace, error) {
	return mvcctxn.Txn(context.TODO(), a.lg, rt, a.txnModeWriteWithSharedBuffer, a.kv, a.lessor)
}

func (a *applierV3backend) Compaction(compaction *pb.CompactionRequest) (*pb.CompactionResponse, <-chan struct{}, *traceutil.Trace, error) {
	resp := &pb.CompactionResponse{}
	resp.Header = &pb.ResponseHeader{}
	trace := traceutil.New("compact",
		a.lg,
		traceutil.Field{Key: "revision", Value: compaction.Revision},
	)

	ch, err := a.kv.Compact(trace, compaction.Revision)
	if err != nil {
		return nil, ch, nil, err
	}
	// get the current revision. which key to get is not important.
	rr, _ := a.kv.Range(context.TODO(), []byte("compaction"), nil, mvcc.RangeOptions{})
	resp.Header.Revision = rr.Rev
	return resp, ch, trace, err
}

func (a *applierV3backend) LeaseGrant(lc *pb.LeaseGrantRequest) (*pb.LeaseGrantResponse, error) {
	l, err := a.lessor.Grant(lease.LeaseID(lc.ID), lc.TTL)
	resp := &pb.LeaseGrantResponse{}
	if err == nil {
		resp.ID = int64(l.ID)
		resp.TTL = l.TTL()
		resp.Header = a.newHeader()
	}
	return resp, err
}

func (a *applierV3backend) LeaseRevoke(lc *pb.LeaseRevokeRequest) (*pb.LeaseRevokeResponse, error) {
	err := a.lessor.Revoke(lease.LeaseID(lc.ID))
	return &pb.LeaseRevokeResponse{Header: a.newHeader()}, err
}

func (a *applierV3backend) LeaseCheckpoint(lc *pb.LeaseCheckpointRequest) (*pb.LeaseCheckpointResponse, error) {
	for _, c := range lc.Checkpoints {
		err := a.lessor.Checkpoint(lease.LeaseID(c.ID), c.Remaining_TTL)
		if err != nil {
			return &pb.LeaseCheckpointResponse{Header: a.newHeader()}, err
		}
	}
	return &pb.LeaseCheckpointResponse{Header: a.newHeader()}, nil
}

func (a *applierV3backend) Alarm(ar *pb.AlarmRequest) (*pb.AlarmResponse, error) {
	resp := &pb.AlarmResponse{}

	switch ar.Action {
	case pb.AlarmRequest_GET:
		resp.Alarms = a.alarmStore.Get(ar.Alarm)
	case pb.AlarmRequest_ACTIVATE:
		if ar.Alarm == pb.AlarmType_NONE {
			break
		}
		m := a.alarmStore.Activate(types.ID(ar.MemberID), ar.Alarm)
		if m == nil {
			break
		}
		resp.Alarms = append(resp.Alarms, m)
		alarms.WithLabelValues(types.ID(ar.MemberID).String(), m.Alarm.String()).Inc()
	case pb.AlarmRequest_DEACTIVATE:
		m := a.alarmStore.Deactivate(types.ID(ar.MemberID), ar.Alarm)
		if m == nil {
			break
		}
		resp.Alarms = append(resp.Alarms, m)
		alarms.WithLabelValues(types.ID(ar.MemberID).String(), m.Alarm.String()).Dec()
	default:
		return nil, nil
	}
	return resp, nil
}

type applierV3Capped struct {
	applierV3
	q serverstorage.BackendQuota
}

// newApplierV3Capped creates an applyV3 that will reject Puts and transactions
// with Puts so that the number of keys in the store is capped.
func newApplierV3Capped(base applierV3) applierV3 { return &applierV3Capped{applierV3: base} }

func (a *applierV3Capped) Put(_ *pb.PutRequest) (*pb.PutResponse, *traceutil.Trace, error) {
	return nil, nil, errors.ErrNoSpace
}

func (a *applierV3Capped) Txn(r *pb.TxnRequest) (*pb.TxnResponse, *traceutil.Trace, error) {
	if a.q.Cost(r) > 0 {
		return nil, nil, errors.ErrNoSpace
	}
	return a.applierV3.Txn(r)
}

func (a *applierV3Capped) LeaseGrant(_ *pb.LeaseGrantRequest) (*pb.LeaseGrantResponse, error) {
	return nil, errors.ErrNoSpace
}

func (a *applierV3backend) AuthEnable() (*pb.AuthEnableResponse, error) {
	err := a.authStore.AuthEnable()
	if err != nil {
		return nil, err
	}
	return &pb.AuthEnableResponse{Header: a.newHeader()}, nil
}

func (a *applierV3backend) AuthDisable() (*pb.AuthDisableResponse, error) {
	a.authStore.AuthDisable()
	return &pb.AuthDisableResponse{Header: a.newHeader()}, nil
}

func (a *applierV3backend) AuthStatus() (*pb.AuthStatusResponse, error) {
	enabled := a.authStore.IsAuthEnabled()
	authRevision := a.authStore.Revision()
	return &pb.AuthStatusResponse{Header: a.newHeader(), Enabled: enabled, AuthRevision: authRevision}, nil
}

func (a *applierV3backend) Authenticate(r *pb.InternalAuthenticateRequest) (*pb.AuthenticateResponse, error) {
	ctx := context.WithValue(context.WithValue(context.Background(), auth.AuthenticateParamIndex{}, a.consistentIndex.ConsistentIndex()), auth.AuthenticateParamSimpleTokenPrefix{}, r.SimpleToken)
	resp, err := a.authStore.Authenticate(ctx, r.Name, r.Password)
	if resp != nil {
		resp.Header = a.newHeader()
	}
	return resp, err
}

func (a *applierV3backend) UserAdd(r *pb.AuthUserAddRequest) (*pb.AuthUserAddResponse, error) {
	resp, err := a.authStore.UserAdd(r)
	if resp != nil {
		resp.Header = a.newHeader()
	}
	return resp, err
}

func (a *applierV3backend) UserDelete(r *pb.AuthUserDeleteRequest) (*pb.AuthUserDeleteResponse, error) {
	resp, err := a.authStore.UserDelete(r)
	if resp != nil {
		resp.Header = a.newHeader()
	}
	return resp, err
}

func (a *applierV3backend) UserChangePassword(r *pb.AuthUserChangePasswordRequest) (*pb.AuthUserChangePasswordResponse, error) {
	resp, err := a.authStore.UserChangePassword(r)
	if resp != nil {
		resp.Header = a.newHeader()
	}
	return resp, err
}

func (a *applierV3backend) UserGrantRole(r *pb.AuthUserGrantRoleRequest) (*pb.AuthUserGrantRoleResponse, error) {
	resp, err := a.authStore.UserGrantRole(r)
	if resp != nil {
		resp.Header = a.newHeader()
	}
	return resp, err
}

func (a *applierV3backend) UserGet(r *pb.AuthUserGetRequest) (*pb.AuthUserGetResponse, error) {
	resp, err := a.authStore.UserGet(r)
	if resp != nil {
		resp.Header = a.newHeader()
	}
	return resp, err
}

func (a *applierV3backend) UserRevokeRole(r *pb.AuthUserRevokeRoleRequest) (*pb.AuthUserRevokeRoleResponse, error) {
	resp, err := a.authStore.UserRevokeRole(r)
	if resp != nil {
		resp.Header = a.newHeader()
	}
	return resp, err
}

func (a *applierV3backend) RoleAdd(r *pb.AuthRoleAddRequest) (*pb.AuthRoleAddResponse, error) {
	resp, err := a.authStore.RoleAdd(r)
	if resp != nil {
		resp.Header = a.newHeader()
	}
	return resp, err
}

func (a *applierV3backend) RoleGrantPermission(r *pb.AuthRoleGrantPermissionRequest) (*pb.AuthRoleGrantPermissionResponse, error) {
	resp, err := a.authStore.RoleGrantPermission(r)
	if resp != nil {
		resp.Header = a.newHeader()
	}
	return resp, err
}

func (a *applierV3backend) RoleGet(r *pb.AuthRoleGetRequest) (*pb.AuthRoleGetResponse, error) {
	resp, err := a.authStore.RoleGet(r)
	if resp != nil {
		resp.Header = a.newHeader()
	}
	return resp, err
}

func (a *applierV3backend) RoleRevokePermission(r *pb.AuthRoleRevokePermissionRequest) (*pb.AuthRoleRevokePermissionResponse, error) {
	resp, err := a.authStore.RoleRevokePermission(r)
	if resp != nil {
		resp.Header = a.newHeader()
	}
	return resp, err
}

func (a *applierV3backend) RoleDelete(r *pb.AuthRoleDeleteRequest) (*pb.AuthRoleDeleteResponse, error) {
	resp, err := a.authStore.RoleDelete(r)
	if resp != nil {
		resp.Header = a.newHeader()
	}
	return resp, err
}

func (a *applierV3backend) UserList(r *pb.AuthUserListRequest) (*pb.AuthUserListResponse, error) {
	resp, err := a.authStore.UserList(r)
	if resp != nil {
		resp.Header = a.newHeader()
	}
	return resp, err
}

func (a *applierV3backend) RoleList(r *pb.AuthRoleListRequest) (*pb.AuthRoleListResponse, error) {
	resp, err := a.authStore.RoleList(r)
	if resp != nil {
		resp.Header = a.newHeader()
	}
	return resp, err
}

type applierMembership struct {
	lg             *zap.Logger
	cluster        *membership.RaftCluster
	snapshotServer SnapshotServer
}

func NewApplierMembership(lg *zap.Logger, cluster *membership.RaftCluster, snapshotServer SnapshotServer) *applierMembership {
	return &applierMembership{
		lg:             lg,
		cluster:        cluster,
		snapshotServer: snapshotServer,
	}
}

func (a *applierMembership) ClusterVersionSet(r *membershippb.ClusterVersionSetRequest, shouldApplyV3 membership.ShouldApplyV3) {
	prevVersion := a.cluster.Version()
	newVersion := semver.Must(semver.NewVersion(r.Ver))
	a.cluster.SetVersion(newVersion, api.UpdateCapability, shouldApplyV3)
	// Force snapshot after cluster version downgrade.
	if prevVersion != nil && newVersion.LessThan(*prevVersion) {
		lg := a.lg
		if lg != nil {
			lg.Info("Cluster version downgrade detected, forcing snapshot",
				zap.String("prev-cluster-version", prevVersion.String()),
				zap.String("new-cluster-version", newVersion.String()),
			)
		}
		a.snapshotServer.ForceSnapshot()
	}
}

func (a *applierMembership) ClusterMemberAttrSet(r *membershippb.ClusterMemberAttrSetRequest, shouldApplyV3 membership.ShouldApplyV3) {
	a.cluster.UpdateAttributes(
		types.ID(r.Member_ID),
		membership.Attributes{
			Name:       r.MemberAttributes.Name,
			ClientURLs: r.MemberAttributes.ClientUrls,
		},
		shouldApplyV3,
	)
}

func (a *applierMembership) DowngradeInfoSet(r *membershippb.DowngradeInfoSetRequest, shouldApplyV3 membership.ShouldApplyV3) {
	d := version.DowngradeInfo{Enabled: false}
	if r.Enabled {
		d = version.DowngradeInfo{Enabled: true, TargetVersion: r.Ver}
	}
	a.cluster.SetDowngradeInfo(&d, shouldApplyV3)
}

type quotaApplierV3 struct {
	applierV3
	q serverstorage.Quota
}

func newQuotaApplierV3(lg *zap.Logger, quotaBackendBytesCfg int64, be backend.Backend, app applierV3) applierV3 {
	return &quotaApplierV3{app, serverstorage.NewBackendQuota(lg, quotaBackendBytesCfg, be, "v3-applier")}
}

func (a *quotaApplierV3) Put(p *pb.PutRequest) (*pb.PutResponse, *traceutil.Trace, error) {
	ok := a.q.Available(p)
	resp, trace, err := a.applierV3.Put(p)
	if err == nil && !ok {
		err = errors.ErrNoSpace
	}
	return resp, trace, err
}

func (a *quotaApplierV3) Txn(rt *pb.TxnRequest) (*pb.TxnResponse, *traceutil.Trace, error) {
	ok := a.q.Available(rt)
	resp, trace, err := a.applierV3.Txn(rt)
	if err == nil && !ok {
		err = errors.ErrNoSpace
	}
	return resp, trace, err
}

func (a *quotaApplierV3) LeaseGrant(lc *pb.LeaseGrantRequest) (*pb.LeaseGrantResponse, error) {
	ok := a.q.Available(lc)
	resp, err := a.applierV3.LeaseGrant(lc)
	if err == nil && !ok {
		err = errors.ErrNoSpace
	}
	return resp, err
}

func (a *applierV3backend) newHeader() *pb.ResponseHeader {
	return &pb.ResponseHeader{
		ClusterId: uint64(a.cluster.ID()),
		MemberId:  uint64(a.raftStatus.MemberID()),
		Revision:  a.kv.Rev(),
		RaftTerm:  a.raftStatus.Term(),
	}
}
