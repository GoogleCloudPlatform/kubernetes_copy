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

package clientv3

import (
	"context"
	"errors"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/v3rpc/rpctypes"
)

type retryPolicy uint8

const (
	repeatable retryPolicy = iota
	nonRepeatable
)

func (rp retryPolicy) String() string {
	switch rp {
	case repeatable:
		return "repeatable"
	case nonRepeatable:
		return "nonRepeatable"
	default:
		return "UNKNOWN"
	}
}

// isSafeRetryImmutableRPC returns "true" when an immutable request is safe for retry.
//
// immutable requests (e.g. Get) should be retried unless it's
// an obvious server-side error (e.g. rpctypes.ErrRequestTooLarge).
//
// Returning "false" means retry should stop, since client cannot
// handle itself even with retries.
func isSafeRetryImmutableRPC(err error) bool {
	eErr := rpctypes.Error(err)
	var serverErr rpctypes.EtcdError
	if errors.As(eErr, &serverErr) && serverErr.Code() != codes.Unavailable {
		// interrupted by non-transient server-side or gRPC-side error
		// client cannot handle itself (e.g. rpctypes.ErrCompacted)
		return false
	}
	// only retry if unavailable
	ev, ok := status.FromError(err)
	if !ok {
		// all errors from RPC is typed "grpc/status.(*statusError)"
		// (ref. https://github.com/grpc/grpc-go/pull/1782)
		//
		// if the error type is not "grpc/status.(*statusError)",
		// it could be from "Dial"
		// TODO: do not retry for now
		// ref. https://github.com/grpc/grpc-go/issues/1581
		return false
	}
	return ev.Code() == codes.Unavailable
}

// isSafeRetryMutableRPC returns "true" when a mutable request is safe for retry.
//
// mutable requests (e.g. Put, Delete, Txn) should only be retried
// when the status code is codes.Unavailable when initial connection
// has not been established (no endpoint is up).
//
// Returning "false" means retry should stop, otherwise it violates
// write-at-most-once semantics.
func isSafeRetryMutableRPC(err error) bool {
	if ev, ok := status.FromError(err); ok && ev.Code() != codes.Unavailable {
		// not safe for mutable RPCs
		// e.g. interrupted by non-transient error that client cannot handle itself,
		// or transient error while the connection has already been established
		return false
	}
	desc := rpctypes.ErrorDesc(err)
	return desc == "there is no address available" || desc == "there is no connection available"
}

type retryKVClient struct {
	kc pb.KVClient
}

// RetryKVClient implements a KVClient.
func RetryKVClient(c *Client) pb.KVClient {
	return &retryKVClient{
		kc: pb.NewKVClient(c.conn),
	}
}

func (rkv *retryKVClient) Range(ctx context.Context, in *pb.RangeRequest, opts ...grpc.CallOption) (resp *pb.RangeResponse, err error) {
	return rkv.kc.Range(ctx, in, append(opts, withRepeatablePolicy())...)
}

func (rkv *retryKVClient) Put(ctx context.Context, in *pb.PutRequest, opts ...grpc.CallOption) (resp *pb.PutResponse, err error) {
	return rkv.kc.Put(ctx, in, opts...)
}

func (rkv *retryKVClient) DeleteRange(ctx context.Context, in *pb.DeleteRangeRequest, opts ...grpc.CallOption) (resp *pb.DeleteRangeResponse, err error) {
	return rkv.kc.DeleteRange(ctx, in, opts...)
}

func (rkv *retryKVClient) Txn(ctx context.Context, in *pb.TxnRequest, opts ...grpc.CallOption) (resp *pb.TxnResponse, err error) {
	return rkv.kc.Txn(ctx, in, opts...)
}

func (rkv *retryKVClient) Compact(ctx context.Context, in *pb.CompactionRequest, opts ...grpc.CallOption) (resp *pb.CompactionResponse, err error) {
	return rkv.kc.Compact(ctx, in, opts...)
}

type retryLeaseClient struct {
	lc pb.LeaseClient
}

// RetryLeaseClient implements a LeaseClient.
func RetryLeaseClient(c *Client) pb.LeaseClient {
	return &retryLeaseClient{
		lc: pb.NewLeaseClient(c.conn),
	}
}

func (rlc *retryLeaseClient) LeaseTimeToLive(ctx context.Context, in *pb.LeaseTimeToLiveRequest, opts ...grpc.CallOption) (resp *pb.LeaseTimeToLiveResponse, err error) {
	return rlc.lc.LeaseTimeToLive(ctx, in, append(opts, withRepeatablePolicy())...)
}

func (rlc *retryLeaseClient) LeaseLeases(ctx context.Context, in *pb.LeaseLeasesRequest, opts ...grpc.CallOption) (resp *pb.LeaseLeasesResponse, err error) {
	return rlc.lc.LeaseLeases(ctx, in, append(opts, withRepeatablePolicy())...)
}

func (rlc *retryLeaseClient) LeaseGrant(ctx context.Context, in *pb.LeaseGrantRequest, opts ...grpc.CallOption) (resp *pb.LeaseGrantResponse, err error) {
	return rlc.lc.LeaseGrant(ctx, in, append(opts, withRepeatablePolicy())...)
}

func (rlc *retryLeaseClient) LeaseRevoke(ctx context.Context, in *pb.LeaseRevokeRequest, opts ...grpc.CallOption) (resp *pb.LeaseRevokeResponse, err error) {
	return rlc.lc.LeaseRevoke(ctx, in, append(opts, withRepeatablePolicy())...)
}

func (rlc *retryLeaseClient) LeaseKeepAlive(ctx context.Context, opts ...grpc.CallOption) (stream pb.Lease_LeaseKeepAliveClient, err error) {
	return rlc.lc.LeaseKeepAlive(ctx, append(opts, withRepeatablePolicy())...)
}

type retryClusterClient struct {
	cc pb.ClusterClient
}

// RetryClusterClient implements a ClusterClient.
func RetryClusterClient(c *Client) pb.ClusterClient {
	return &retryClusterClient{
		cc: pb.NewClusterClient(c.conn),
	}
}

func (rcc *retryClusterClient) MemberList(ctx context.Context, in *pb.MemberListRequest, opts ...grpc.CallOption) (resp *pb.MemberListResponse, err error) {
	return rcc.cc.MemberList(ctx, in, append(opts, withRepeatablePolicy())...)
}

func (rcc *retryClusterClient) MemberAdd(ctx context.Context, in *pb.MemberAddRequest, opts ...grpc.CallOption) (resp *pb.MemberAddResponse, err error) {
	return rcc.cc.MemberAdd(ctx, in, opts...)
}

func (rcc *retryClusterClient) MemberRemove(ctx context.Context, in *pb.MemberRemoveRequest, opts ...grpc.CallOption) (resp *pb.MemberRemoveResponse, err error) {
	return rcc.cc.MemberRemove(ctx, in, opts...)
}

func (rcc *retryClusterClient) MemberUpdate(ctx context.Context, in *pb.MemberUpdateRequest, opts ...grpc.CallOption) (resp *pb.MemberUpdateResponse, err error) {
	return rcc.cc.MemberUpdate(ctx, in, opts...)
}

func (rcc *retryClusterClient) MemberPromote(ctx context.Context, in *pb.MemberPromoteRequest, opts ...grpc.CallOption) (resp *pb.MemberPromoteResponse, err error) {
	return rcc.cc.MemberPromote(ctx, in, opts...)
}

type retryMaintenanceClient struct {
	mc pb.MaintenanceClient
}

// RetryMaintenanceClient implements a Maintenance.
func RetryMaintenanceClient(c *Client, conn *grpc.ClientConn) pb.MaintenanceClient {
	return &retryMaintenanceClient{
		mc: pb.NewMaintenanceClient(conn),
	}
}

func (rmc *retryMaintenanceClient) Alarm(ctx context.Context, in *pb.AlarmRequest, opts ...grpc.CallOption) (resp *pb.AlarmResponse, err error) {
	return rmc.mc.Alarm(ctx, in, append(opts, withRepeatablePolicy())...)
}

func (rmc *retryMaintenanceClient) Status(ctx context.Context, in *pb.StatusRequest, opts ...grpc.CallOption) (resp *pb.StatusResponse, err error) {
	return rmc.mc.Status(ctx, in, append(opts, withRepeatablePolicy())...)
}

func (rmc *retryMaintenanceClient) Hash(ctx context.Context, in *pb.HashRequest, opts ...grpc.CallOption) (resp *pb.HashResponse, err error) {
	return rmc.mc.Hash(ctx, in, append(opts, withRepeatablePolicy())...)
}

func (rmc *retryMaintenanceClient) HashKV(ctx context.Context, in *pb.HashKVRequest, opts ...grpc.CallOption) (resp *pb.HashKVResponse, err error) {
	return rmc.mc.HashKV(ctx, in, append(opts, withRepeatablePolicy())...)
}

func (rmc *retryMaintenanceClient) Snapshot(ctx context.Context, in *pb.SnapshotRequest, opts ...grpc.CallOption) (stream pb.Maintenance_SnapshotClient, err error) {
	return rmc.mc.Snapshot(ctx, in, append(opts, withRepeatablePolicy())...)
}

func (rmc *retryMaintenanceClient) MoveLeader(ctx context.Context, in *pb.MoveLeaderRequest, opts ...grpc.CallOption) (resp *pb.MoveLeaderResponse, err error) {
	return rmc.mc.MoveLeader(ctx, in, append(opts, withRepeatablePolicy())...)
}

func (rmc *retryMaintenanceClient) Defragment(ctx context.Context, in *pb.DefragmentRequest, opts ...grpc.CallOption) (resp *pb.DefragmentResponse, err error) {
	return rmc.mc.Defragment(ctx, in, opts...)
}

func (rmc *retryMaintenanceClient) Downgrade(ctx context.Context, in *pb.DowngradeRequest, opts ...grpc.CallOption) (resp *pb.DowngradeResponse, err error) {
	return rmc.mc.Downgrade(ctx, in, opts...)
}

type retryAuthClient struct {
	ac pb.AuthClient
}

// RetryAuthClient implements a AuthClient.
func RetryAuthClient(c *Client) pb.AuthClient {
	return &retryAuthClient{
		ac: pb.NewAuthClient(c.conn),
	}
}

func (rac *retryAuthClient) UserList(ctx context.Context, in *pb.AuthUserListRequest, opts ...grpc.CallOption) (resp *pb.AuthUserListResponse, err error) {
	return rac.ac.UserList(ctx, in, append(opts, withRepeatablePolicy())...)
}

func (rac *retryAuthClient) UserGet(ctx context.Context, in *pb.AuthUserGetRequest, opts ...grpc.CallOption) (resp *pb.AuthUserGetResponse, err error) {
	return rac.ac.UserGet(ctx, in, append(opts, withRepeatablePolicy())...)
}

func (rac *retryAuthClient) RoleGet(ctx context.Context, in *pb.AuthRoleGetRequest, opts ...grpc.CallOption) (resp *pb.AuthRoleGetResponse, err error) {
	return rac.ac.RoleGet(ctx, in, append(opts, withRepeatablePolicy())...)
}

func (rac *retryAuthClient) RoleList(ctx context.Context, in *pb.AuthRoleListRequest, opts ...grpc.CallOption) (resp *pb.AuthRoleListResponse, err error) {
	return rac.ac.RoleList(ctx, in, append(opts, withRepeatablePolicy())...)
}

func (rac *retryAuthClient) AuthEnable(ctx context.Context, in *pb.AuthEnableRequest, opts ...grpc.CallOption) (resp *pb.AuthEnableResponse, err error) {
	return rac.ac.AuthEnable(ctx, in, opts...)
}

func (rac *retryAuthClient) AuthDisable(ctx context.Context, in *pb.AuthDisableRequest, opts ...grpc.CallOption) (resp *pb.AuthDisableResponse, err error) {
	return rac.ac.AuthDisable(ctx, in, opts...)
}

func (rac *retryAuthClient) AuthStatus(ctx context.Context, in *pb.AuthStatusRequest, opts ...grpc.CallOption) (resp *pb.AuthStatusResponse, err error) {
	return rac.ac.AuthStatus(ctx, in, opts...)
}

func (rac *retryAuthClient) UserAdd(ctx context.Context, in *pb.AuthUserAddRequest, opts ...grpc.CallOption) (resp *pb.AuthUserAddResponse, err error) {
	return rac.ac.UserAdd(ctx, in, opts...)
}

func (rac *retryAuthClient) UserDelete(ctx context.Context, in *pb.AuthUserDeleteRequest, opts ...grpc.CallOption) (resp *pb.AuthUserDeleteResponse, err error) {
	return rac.ac.UserDelete(ctx, in, opts...)
}

func (rac *retryAuthClient) UserChangePassword(ctx context.Context, in *pb.AuthUserChangePasswordRequest, opts ...grpc.CallOption) (resp *pb.AuthUserChangePasswordResponse, err error) {
	return rac.ac.UserChangePassword(ctx, in, opts...)
}

func (rac *retryAuthClient) UserGrantRole(ctx context.Context, in *pb.AuthUserGrantRoleRequest, opts ...grpc.CallOption) (resp *pb.AuthUserGrantRoleResponse, err error) {
	return rac.ac.UserGrantRole(ctx, in, opts...)
}

func (rac *retryAuthClient) UserRevokeRole(ctx context.Context, in *pb.AuthUserRevokeRoleRequest, opts ...grpc.CallOption) (resp *pb.AuthUserRevokeRoleResponse, err error) {
	return rac.ac.UserRevokeRole(ctx, in, opts...)
}

func (rac *retryAuthClient) RoleAdd(ctx context.Context, in *pb.AuthRoleAddRequest, opts ...grpc.CallOption) (resp *pb.AuthRoleAddResponse, err error) {
	return rac.ac.RoleAdd(ctx, in, opts...)
}

func (rac *retryAuthClient) RoleDelete(ctx context.Context, in *pb.AuthRoleDeleteRequest, opts ...grpc.CallOption) (resp *pb.AuthRoleDeleteResponse, err error) {
	return rac.ac.RoleDelete(ctx, in, opts...)
}

func (rac *retryAuthClient) RoleGrantPermission(ctx context.Context, in *pb.AuthRoleGrantPermissionRequest, opts ...grpc.CallOption) (resp *pb.AuthRoleGrantPermissionResponse, err error) {
	return rac.ac.RoleGrantPermission(ctx, in, opts...)
}

func (rac *retryAuthClient) RoleRevokePermission(ctx context.Context, in *pb.AuthRoleRevokePermissionRequest, opts ...grpc.CallOption) (resp *pb.AuthRoleRevokePermissionResponse, err error) {
	return rac.ac.RoleRevokePermission(ctx, in, opts...)
}

func (rac *retryAuthClient) Authenticate(ctx context.Context, in *pb.AuthenticateRequest, opts ...grpc.CallOption) (resp *pb.AuthenticateResponse, err error) {
	return rac.ac.Authenticate(ctx, in, opts...)
}
