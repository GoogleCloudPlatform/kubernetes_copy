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
	"fmt"
	"strings"

	"google.golang.org/grpc"

	"go.etcd.io/etcd/api/v3/authpb"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
)

type (
	AuthEnableResponse               pb.AuthEnableResponse
	AuthDisableResponse              pb.AuthDisableResponse
	AuthStatusResponse               pb.AuthStatusResponse
	AuthenticateResponse             pb.AuthenticateResponse
	AuthUserAddResponse              pb.AuthUserAddResponse
	AuthUserDeleteResponse           pb.AuthUserDeleteResponse
	AuthUserChangePasswordResponse   pb.AuthUserChangePasswordResponse
	AuthUserGrantRoleResponse        pb.AuthUserGrantRoleResponse
	AuthUserGetResponse              pb.AuthUserGetResponse
	AuthUserRevokeRoleResponse       pb.AuthUserRevokeRoleResponse
	AuthRoleAddResponse              pb.AuthRoleAddResponse
	AuthRoleGrantPermissionResponse  pb.AuthRoleGrantPermissionResponse
	AuthRoleGetResponse              pb.AuthRoleGetResponse
	AuthRoleRevokePermissionResponse pb.AuthRoleRevokePermissionResponse
	AuthRoleDeleteResponse           pb.AuthRoleDeleteResponse
	AuthUserListResponse             pb.AuthUserListResponse
	AuthRoleListResponse             pb.AuthRoleListResponse

	PermissionType authpb.Permission_Type
	Permission     authpb.Permission
)

const (
	PermRead      = authpb.READ
	PermWrite     = authpb.WRITE
	PermReadWrite = authpb.READWRITE
)

type UserAddOptions authpb.UserAddOptions

type Auth interface {
	// Authenticate login and get token
	Authenticate(ctx context.Context, name string, password string) (*AuthenticateResponse, error)

	// AuthEnable enables auth of an etcd cluster.
	AuthEnable(ctx context.Context) (*AuthEnableResponse, error)

	// AuthDisable disables auth of an etcd cluster.
	AuthDisable(ctx context.Context) (*AuthDisableResponse, error)

	// AuthStatus returns the status of auth of an etcd cluster.
	AuthStatus(ctx context.Context) (*AuthStatusResponse, error)

	// UserAdd adds a new user to an etcd cluster.
	UserAdd(ctx context.Context, name string, password string) (*AuthUserAddResponse, error)

	// UserAddWithOptions adds a new user to an etcd cluster with some options.
	UserAddWithOptions(ctx context.Context, name string, password string, opt *UserAddOptions) (*AuthUserAddResponse, error)

	// UserDelete deletes a user from an etcd cluster.
	UserDelete(ctx context.Context, name string) (*AuthUserDeleteResponse, error)

	// UserChangePassword changes a password of a user.
	UserChangePassword(ctx context.Context, name string, password string) (*AuthUserChangePasswordResponse, error)

	// UserGrantRole grants a role to a user.
	UserGrantRole(ctx context.Context, user string, role string) (*AuthUserGrantRoleResponse, error)

	// UserGet gets a detailed information of a user.
	UserGet(ctx context.Context, name string) (*AuthUserGetResponse, error)

	// UserList gets a list of all users.
	UserList(ctx context.Context) (*AuthUserListResponse, error)

	// UserRevokeRole revokes a role of a user.
	UserRevokeRole(ctx context.Context, name string, role string) (*AuthUserRevokeRoleResponse, error)

	// RoleAdd adds a new role to an etcd cluster.
	RoleAdd(ctx context.Context, name string) (*AuthRoleAddResponse, error)

	// RoleGrantPermission grants a permission to a role.
	RoleGrantPermission(ctx context.Context, name string, key, rangeEnd string, permType PermissionType) (*AuthRoleGrantPermissionResponse, error)

	// RoleGet gets a detailed information of a role.
	RoleGet(ctx context.Context, role string) (*AuthRoleGetResponse, error)

	// RoleList gets a list of all roles.
	RoleList(ctx context.Context) (*AuthRoleListResponse, error)

	// RoleRevokePermission revokes a permission from a role.
	RoleRevokePermission(ctx context.Context, role string, key, rangeEnd string) (*AuthRoleRevokePermissionResponse, error)

	// RoleDelete deletes a role.
	RoleDelete(ctx context.Context, role string) (*AuthRoleDeleteResponse, error)
}

type authClient struct {
	remote   pb.AuthClient
	callOpts []grpc.CallOption
}

func NewAuth(c *Client) Auth {
	api := &authClient{remote: RetryAuthClient(c)}
	if c != nil {
		api.callOpts = c.callOpts
	}
	return api
}

func NewAuthFromAuthClient(remote pb.AuthClient, c *Client) Auth {
	api := &authClient{remote: remote}
	if c != nil {
		api.callOpts = c.callOpts
	}
	return api
}

func (auth *authClient) Authenticate(ctx context.Context, name string, password string) (*AuthenticateResponse, error) {
	resp, err := auth.remote.Authenticate(ctx, &pb.AuthenticateRequest{Name: name, Password: password}, auth.callOpts...)
	return (*AuthenticateResponse)(resp), ContextError(ctx, err)
}

func (auth *authClient) AuthEnable(ctx context.Context) (*AuthEnableResponse, error) {
	resp, err := auth.remote.AuthEnable(ctx, &pb.AuthEnableRequest{}, auth.callOpts...)
	return (*AuthEnableResponse)(resp), ContextError(ctx, err)
}

func (auth *authClient) AuthDisable(ctx context.Context) (*AuthDisableResponse, error) {
	resp, err := auth.remote.AuthDisable(ctx, &pb.AuthDisableRequest{}, auth.callOpts...)
	return (*AuthDisableResponse)(resp), ContextError(ctx, err)
}

func (auth *authClient) AuthStatus(ctx context.Context) (*AuthStatusResponse, error) {
	resp, err := auth.remote.AuthStatus(ctx, &pb.AuthStatusRequest{}, auth.callOpts...)
	return (*AuthStatusResponse)(resp), ContextError(ctx, err)
}

func (auth *authClient) UserAdd(ctx context.Context, name string, password string) (*AuthUserAddResponse, error) {
	resp, err := auth.remote.UserAdd(ctx, &pb.AuthUserAddRequest{Name: name, Password: password, Options: &authpb.UserAddOptions{NoPassword: false}}, auth.callOpts...)
	return (*AuthUserAddResponse)(resp), ContextError(ctx, err)
}

func (auth *authClient) UserAddWithOptions(ctx context.Context, name string, password string, options *UserAddOptions) (*AuthUserAddResponse, error) {
	resp, err := auth.remote.UserAdd(ctx, &pb.AuthUserAddRequest{Name: name, Password: password, Options: (*authpb.UserAddOptions)(options)}, auth.callOpts...)
	return (*AuthUserAddResponse)(resp), ContextError(ctx, err)
}

func (auth *authClient) UserDelete(ctx context.Context, name string) (*AuthUserDeleteResponse, error) {
	resp, err := auth.remote.UserDelete(ctx, &pb.AuthUserDeleteRequest{Name: name}, auth.callOpts...)
	return (*AuthUserDeleteResponse)(resp), ContextError(ctx, err)
}

func (auth *authClient) UserChangePassword(ctx context.Context, name string, password string) (*AuthUserChangePasswordResponse, error) {
	resp, err := auth.remote.UserChangePassword(ctx, &pb.AuthUserChangePasswordRequest{Name: name, Password: password}, auth.callOpts...)
	return (*AuthUserChangePasswordResponse)(resp), ContextError(ctx, err)
}

func (auth *authClient) UserGrantRole(ctx context.Context, user string, role string) (*AuthUserGrantRoleResponse, error) {
	resp, err := auth.remote.UserGrantRole(ctx, &pb.AuthUserGrantRoleRequest{User: user, Role: role}, auth.callOpts...)
	return (*AuthUserGrantRoleResponse)(resp), ContextError(ctx, err)
}

func (auth *authClient) UserGet(ctx context.Context, name string) (*AuthUserGetResponse, error) {
	resp, err := auth.remote.UserGet(ctx, &pb.AuthUserGetRequest{Name: name}, auth.callOpts...)
	return (*AuthUserGetResponse)(resp), ContextError(ctx, err)
}

func (auth *authClient) UserList(ctx context.Context) (*AuthUserListResponse, error) {
	resp, err := auth.remote.UserList(ctx, &pb.AuthUserListRequest{}, auth.callOpts...)
	return (*AuthUserListResponse)(resp), ContextError(ctx, err)
}

func (auth *authClient) UserRevokeRole(ctx context.Context, name string, role string) (*AuthUserRevokeRoleResponse, error) {
	resp, err := auth.remote.UserRevokeRole(ctx, &pb.AuthUserRevokeRoleRequest{Name: name, Role: role}, auth.callOpts...)
	return (*AuthUserRevokeRoleResponse)(resp), ContextError(ctx, err)
}

func (auth *authClient) RoleAdd(ctx context.Context, name string) (*AuthRoleAddResponse, error) {
	resp, err := auth.remote.RoleAdd(ctx, &pb.AuthRoleAddRequest{Name: name}, auth.callOpts...)
	return (*AuthRoleAddResponse)(resp), ContextError(ctx, err)
}

func (auth *authClient) RoleGrantPermission(ctx context.Context, name string, key, rangeEnd string, permType PermissionType) (*AuthRoleGrantPermissionResponse, error) {
	perm := &authpb.Permission{
		Key:      []byte(key),
		RangeEnd: []byte(rangeEnd),
		PermType: authpb.Permission_Type(permType),
	}
	resp, err := auth.remote.RoleGrantPermission(ctx, &pb.AuthRoleGrantPermissionRequest{Name: name, Perm: perm}, auth.callOpts...)
	return (*AuthRoleGrantPermissionResponse)(resp), ContextError(ctx, err)
}

func (auth *authClient) RoleGet(ctx context.Context, role string) (*AuthRoleGetResponse, error) {
	resp, err := auth.remote.RoleGet(ctx, &pb.AuthRoleGetRequest{Role: role}, auth.callOpts...)
	return (*AuthRoleGetResponse)(resp), ContextError(ctx, err)
}

func (auth *authClient) RoleList(ctx context.Context) (*AuthRoleListResponse, error) {
	resp, err := auth.remote.RoleList(ctx, &pb.AuthRoleListRequest{}, auth.callOpts...)
	return (*AuthRoleListResponse)(resp), ContextError(ctx, err)
}

func (auth *authClient) RoleRevokePermission(ctx context.Context, role string, key, rangeEnd string) (*AuthRoleRevokePermissionResponse, error) {
	resp, err := auth.remote.RoleRevokePermission(ctx, &pb.AuthRoleRevokePermissionRequest{Role: role, Key: []byte(key), RangeEnd: []byte(rangeEnd)}, auth.callOpts...)
	return (*AuthRoleRevokePermissionResponse)(resp), ContextError(ctx, err)
}

func (auth *authClient) RoleDelete(ctx context.Context, role string) (*AuthRoleDeleteResponse, error) {
	resp, err := auth.remote.RoleDelete(ctx, &pb.AuthRoleDeleteRequest{Role: role}, auth.callOpts...)
	return (*AuthRoleDeleteResponse)(resp), ContextError(ctx, err)
}

func StrToPermissionType(s string) (PermissionType, error) {
	val, ok := authpb.Permission_Type_value[strings.ToUpper(s)]
	if ok {
		return PermissionType(val), nil
	}
	return PermissionType(-1), fmt.Errorf("invalid permission type: %s", s)
}
