// Copyright 2017 The etcd Authors
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

package adapter

import (
	"context"

	grpc "google.golang.org/grpc"

	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
)

type as2ac struct{ as pb.AuthServer }

func AuthServerToAuthClient(as pb.AuthServer) pb.AuthClient {
	return &as2ac{as}
}

func (s *as2ac) AuthEnable(ctx context.Context, in *pb.AuthEnableRequest, opts ...grpc.CallOption) (*pb.AuthEnableResponse, error) {
	return s.as.AuthEnable(ctx, in)
}

func (s *as2ac) AuthDisable(ctx context.Context, in *pb.AuthDisableRequest, opts ...grpc.CallOption) (*pb.AuthDisableResponse, error) {
	return s.as.AuthDisable(ctx, in)
}

func (s *as2ac) AuthStatus(ctx context.Context, in *pb.AuthStatusRequest, opts ...grpc.CallOption) (*pb.AuthStatusResponse, error) {
	return s.as.AuthStatus(ctx, in)
}

func (s *as2ac) Authenticate(ctx context.Context, in *pb.AuthenticateRequest, opts ...grpc.CallOption) (*pb.AuthenticateResponse, error) {
	return s.as.Authenticate(ctx, in)
}

func (s *as2ac) RoleAdd(ctx context.Context, in *pb.AuthRoleAddRequest, opts ...grpc.CallOption) (*pb.AuthRoleAddResponse, error) {
	return s.as.RoleAdd(ctx, in)
}

func (s *as2ac) RoleDelete(ctx context.Context, in *pb.AuthRoleDeleteRequest, opts ...grpc.CallOption) (*pb.AuthRoleDeleteResponse, error) {
	return s.as.RoleDelete(ctx, in)
}

func (s *as2ac) RoleGet(ctx context.Context, in *pb.AuthRoleGetRequest, opts ...grpc.CallOption) (*pb.AuthRoleGetResponse, error) {
	return s.as.RoleGet(ctx, in)
}

func (s *as2ac) RoleList(ctx context.Context, in *pb.AuthRoleListRequest, opts ...grpc.CallOption) (*pb.AuthRoleListResponse, error) {
	return s.as.RoleList(ctx, in)
}

func (s *as2ac) RoleRevokePermission(ctx context.Context, in *pb.AuthRoleRevokePermissionRequest, opts ...grpc.CallOption) (*pb.AuthRoleRevokePermissionResponse, error) {
	return s.as.RoleRevokePermission(ctx, in)
}

func (s *as2ac) RoleGrantPermission(ctx context.Context, in *pb.AuthRoleGrantPermissionRequest, opts ...grpc.CallOption) (*pb.AuthRoleGrantPermissionResponse, error) {
	return s.as.RoleGrantPermission(ctx, in)
}

func (s *as2ac) UserDelete(ctx context.Context, in *pb.AuthUserDeleteRequest, opts ...grpc.CallOption) (*pb.AuthUserDeleteResponse, error) {
	return s.as.UserDelete(ctx, in)
}

func (s *as2ac) UserAdd(ctx context.Context, in *pb.AuthUserAddRequest, opts ...grpc.CallOption) (*pb.AuthUserAddResponse, error) {
	return s.as.UserAdd(ctx, in)
}

func (s *as2ac) UserGet(ctx context.Context, in *pb.AuthUserGetRequest, opts ...grpc.CallOption) (*pb.AuthUserGetResponse, error) {
	return s.as.UserGet(ctx, in)
}

func (s *as2ac) UserList(ctx context.Context, in *pb.AuthUserListRequest, opts ...grpc.CallOption) (*pb.AuthUserListResponse, error) {
	return s.as.UserList(ctx, in)
}

func (s *as2ac) UserGrantRole(ctx context.Context, in *pb.AuthUserGrantRoleRequest, opts ...grpc.CallOption) (*pb.AuthUserGrantRoleResponse, error) {
	return s.as.UserGrantRole(ctx, in)
}

func (s *as2ac) UserRevokeRole(ctx context.Context, in *pb.AuthUserRevokeRoleRequest, opts ...grpc.CallOption) (*pb.AuthUserRevokeRoleResponse, error) {
	return s.as.UserRevokeRole(ctx, in)
}

func (s *as2ac) UserChangePassword(ctx context.Context, in *pb.AuthUserChangePasswordRequest, opts ...grpc.CallOption) (*pb.AuthUserChangePasswordResponse, error) {
	return s.as.UserChangePassword(ctx, in)
}
