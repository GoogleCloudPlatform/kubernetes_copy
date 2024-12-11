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
	"sync"

	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/pkg/v3/traceutil"
	"go.etcd.io/etcd/server/v3/auth"
	"go.etcd.io/etcd/server/v3/etcdserver/txn"
	"go.etcd.io/etcd/server/v3/lease"
)

type authApplierV3 struct {
	applierV3
	as     auth.AuthStore
	lessor lease.Lessor

	// mu serializes Apply so that user isn't corrupted and so that
	// serialized requests don't leak data from TOCTOU errors
	mu sync.Mutex

	authInfo auth.AuthInfo
}

func newAuthApplierV3(as auth.AuthStore, base applierV3, lessor lease.Lessor) *authApplierV3 {
	return &authApplierV3{applierV3: base, as: as, lessor: lessor}
}

func (aa *authApplierV3) Apply(r *pb.InternalRaftRequest, applyFunc applyFunc) *Result {
	aa.mu.Lock()
	defer aa.mu.Unlock()
	if r.Header != nil {
		// backward-compatible with pre-3.0 releases when internalRaftRequest
		// does not have header field
		aa.authInfo.Username = r.Header.Username
		aa.authInfo.Revision = r.Header.AuthRevision
	}
	if needAdminPermission(r) {
		if err := aa.as.IsAdminPermitted(&aa.authInfo); err != nil {
			aa.authInfo.Username = ""
			aa.authInfo.Revision = 0
			return &Result{Err: err}
		}
	}
	ret := aa.applierV3.Apply(r, applyFunc)
	aa.authInfo.Username = ""
	aa.authInfo.Revision = 0
	return ret
}

func (aa *authApplierV3) Put(r *pb.PutRequest) (*pb.PutResponse, *traceutil.Trace, error) {
	if err := aa.as.IsPutPermitted(&aa.authInfo, r.Key); err != nil {
		return nil, nil, err
	}

	if err := aa.checkLeasePuts(lease.LeaseID(r.Lease)); err != nil {
		// The specified lease is already attached with a key that cannot
		// be written by this user. It means the user cannot revoke the
		// lease so attaching the lease to the newly written key should
		// be forbidden.
		return nil, nil, err
	}

	if r.PrevKv {
		err := aa.as.IsRangePermitted(&aa.authInfo, r.Key, nil)
		if err != nil {
			return nil, nil, err
		}
	}
	return aa.applierV3.Put(r)
}

func (aa *authApplierV3) Range(r *pb.RangeRequest) (*pb.RangeResponse, *traceutil.Trace, error) {
	if err := aa.as.IsRangePermitted(&aa.authInfo, r.Key, r.RangeEnd); err != nil {
		return nil, nil, err
	}
	return aa.applierV3.Range(r)
}

func (aa *authApplierV3) DeleteRange(r *pb.DeleteRangeRequest) (*pb.DeleteRangeResponse, *traceutil.Trace, error) {
	if err := aa.as.IsDeleteRangePermitted(&aa.authInfo, r.Key, r.RangeEnd); err != nil {
		return nil, nil, err
	}
	if r.PrevKv {
		err := aa.as.IsRangePermitted(&aa.authInfo, r.Key, r.RangeEnd)
		if err != nil {
			return nil, nil, err
		}
	}

	return aa.applierV3.DeleteRange(r)
}

func (aa *authApplierV3) Txn(rt *pb.TxnRequest) (*pb.TxnResponse, *traceutil.Trace, error) {
	if err := txn.CheckTxnAuth(aa.as, &aa.authInfo, rt); err != nil {
		return nil, nil, err
	}
	return aa.applierV3.Txn(rt)
}

func (aa *authApplierV3) LeaseRevoke(lc *pb.LeaseRevokeRequest) (*pb.LeaseRevokeResponse, error) {
	if err := aa.checkLeasePuts(lease.LeaseID(lc.ID)); err != nil {
		return nil, err
	}
	return aa.applierV3.LeaseRevoke(lc)
}

func (aa *authApplierV3) checkLeasePuts(leaseID lease.LeaseID) error {
	l := aa.lessor.Lookup(leaseID)
	if l != nil {
		return aa.checkLeasePutsKeys(l)
	}

	return nil
}

func (aa *authApplierV3) checkLeasePutsKeys(l *lease.Lease) error {
	// early return for most-common scenario of either disabled auth or admin user.
	// IsAdminPermitted also checks whether auth is enabled
	if err := aa.as.IsAdminPermitted(&aa.authInfo); err == nil {
		return nil
	}

	for _, key := range l.Keys() {
		if err := aa.as.IsPutPermitted(&aa.authInfo, []byte(key)); err != nil {
			return err
		}
	}
	return nil
}

func (aa *authApplierV3) UserGet(r *pb.AuthUserGetRequest) (*pb.AuthUserGetResponse, error) {
	err := aa.as.IsAdminPermitted(&aa.authInfo)
	if err != nil && r.Name != aa.authInfo.Username {
		aa.authInfo.Username = ""
		aa.authInfo.Revision = 0
		return &pb.AuthUserGetResponse{}, err
	}

	return aa.applierV3.UserGet(r)
}

func (aa *authApplierV3) RoleGet(r *pb.AuthRoleGetRequest) (*pb.AuthRoleGetResponse, error) {
	err := aa.as.IsAdminPermitted(&aa.authInfo)
	if err != nil && !aa.as.HasRole(aa.authInfo.Username, r.Role) {
		aa.authInfo.Username = ""
		aa.authInfo.Revision = 0
		return &pb.AuthRoleGetResponse{}, err
	}

	return aa.applierV3.RoleGet(r)
}

func needAdminPermission(r *pb.InternalRaftRequest) bool {
	switch {
	case r.AuthEnable != nil:
		return true
	case r.AuthDisable != nil:
		return true
	case r.AuthStatus != nil:
		return true
	case r.AuthUserAdd != nil:
		return true
	case r.AuthUserDelete != nil:
		return true
	case r.AuthUserChangePassword != nil:
		return true
	case r.AuthUserGrantRole != nil:
		return true
	case r.AuthUserRevokeRole != nil:
		return true
	case r.AuthRoleAdd != nil:
		return true
	case r.AuthRoleGrantPermission != nil:
		return true
	case r.AuthRoleRevokePermission != nil:
		return true
	case r.AuthRoleDelete != nil:
		return true
	case r.AuthUserList != nil:
		return true
	case r.AuthRoleList != nil:
		return true
	default:
		return false
	}
}
