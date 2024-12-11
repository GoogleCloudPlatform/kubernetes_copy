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

package v3rpc

import (
	"context"
	errorspkg "errors"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/v3rpc/rpctypes"
	"go.etcd.io/etcd/server/v3/auth"
	"go.etcd.io/etcd/server/v3/etcdserver/api/membership"
	"go.etcd.io/etcd/server/v3/etcdserver/errors"
	"go.etcd.io/etcd/server/v3/etcdserver/version"
	"go.etcd.io/etcd/server/v3/lease"
	"go.etcd.io/etcd/server/v3/storage/mvcc"
)

var toGRPCErrorMap = map[error]error{
	membership.ErrIDRemoved:           rpctypes.ErrGRPCMemberNotFound,
	membership.ErrIDNotFound:          rpctypes.ErrGRPCMemberNotFound,
	membership.ErrIDExists:            rpctypes.ErrGRPCMemberExist,
	membership.ErrPeerURLexists:       rpctypes.ErrGRPCPeerURLExist,
	membership.ErrMemberNotLearner:    rpctypes.ErrGRPCMemberNotLearner,
	membership.ErrTooManyLearners:     rpctypes.ErrGRPCTooManyLearners,
	errors.ErrNotEnoughStartedMembers: rpctypes.ErrMemberNotEnoughStarted,
	errors.ErrLearnerNotReady:         rpctypes.ErrGRPCLearnerNotReady,

	mvcc.ErrCompacted:         rpctypes.ErrGRPCCompacted,
	mvcc.ErrFutureRev:         rpctypes.ErrGRPCFutureRev,
	errors.ErrRequestTooLarge: rpctypes.ErrGRPCRequestTooLarge,
	errors.ErrNoSpace:         rpctypes.ErrGRPCNoSpace,
	errors.ErrTooManyRequests: rpctypes.ErrTooManyRequests,

	errors.ErrNoLeader:                   rpctypes.ErrGRPCNoLeader,
	errors.ErrNotLeader:                  rpctypes.ErrGRPCNotLeader,
	errors.ErrLeaderChanged:              rpctypes.ErrGRPCLeaderChanged,
	errors.ErrStopped:                    rpctypes.ErrGRPCStopped,
	errors.ErrTimeout:                    rpctypes.ErrGRPCTimeout,
	errors.ErrTimeoutDueToLeaderFail:     rpctypes.ErrGRPCTimeoutDueToLeaderFail,
	errors.ErrTimeoutDueToConnectionLost: rpctypes.ErrGRPCTimeoutDueToConnectionLost,
	errors.ErrTimeoutWaitAppliedIndex:    rpctypes.ErrGRPCTimeoutWaitAppliedIndex,
	errors.ErrUnhealthy:                  rpctypes.ErrGRPCUnhealthy,
	errors.ErrKeyNotFound:                rpctypes.ErrGRPCKeyNotFound,
	errors.ErrCorrupt:                    rpctypes.ErrGRPCCorrupt,
	errors.ErrBadLeaderTransferee:        rpctypes.ErrGRPCBadLeaderTransferee,

	errors.ErrClusterVersionUnavailable:      rpctypes.ErrGRPCClusterVersionUnavailable,
	errors.ErrWrongDowngradeVersionFormat:    rpctypes.ErrGRPCWrongDowngradeVersionFormat,
	version.ErrInvalidDowngradeTargetVersion: rpctypes.ErrGRPCInvalidDowngradeTargetVersion,
	version.ErrDowngradeInProcess:            rpctypes.ErrGRPCDowngradeInProcess,
	version.ErrNoInflightDowngrade:           rpctypes.ErrGRPCNoInflightDowngrade,

	lease.ErrLeaseNotFound:    rpctypes.ErrGRPCLeaseNotFound,
	lease.ErrLeaseExists:      rpctypes.ErrGRPCLeaseExist,
	lease.ErrLeaseTTLTooLarge: rpctypes.ErrGRPCLeaseTTLTooLarge,

	auth.ErrRootUserNotExist:     rpctypes.ErrGRPCRootUserNotExist,
	auth.ErrRootRoleNotExist:     rpctypes.ErrGRPCRootRoleNotExist,
	auth.ErrUserAlreadyExist:     rpctypes.ErrGRPCUserAlreadyExist,
	auth.ErrUserEmpty:            rpctypes.ErrGRPCUserEmpty,
	auth.ErrUserNotFound:         rpctypes.ErrGRPCUserNotFound,
	auth.ErrRoleAlreadyExist:     rpctypes.ErrGRPCRoleAlreadyExist,
	auth.ErrRoleNotFound:         rpctypes.ErrGRPCRoleNotFound,
	auth.ErrRoleEmpty:            rpctypes.ErrGRPCRoleEmpty,
	auth.ErrAuthFailed:           rpctypes.ErrGRPCAuthFailed,
	auth.ErrPermissionNotGiven:   rpctypes.ErrGRPCPermissionNotGiven,
	auth.ErrPermissionDenied:     rpctypes.ErrGRPCPermissionDenied,
	auth.ErrRoleNotGranted:       rpctypes.ErrGRPCRoleNotGranted,
	auth.ErrPermissionNotGranted: rpctypes.ErrGRPCPermissionNotGranted,
	auth.ErrAuthNotEnabled:       rpctypes.ErrGRPCAuthNotEnabled,
	auth.ErrInvalidAuthToken:     rpctypes.ErrGRPCInvalidAuthToken,
	auth.ErrInvalidAuthMgmt:      rpctypes.ErrGRPCInvalidAuthMgmt,
	auth.ErrAuthOldRevision:      rpctypes.ErrGRPCAuthOldRevision,

	// In sync with status.FromContextError
	context.Canceled:         rpctypes.ErrGRPCCanceled,
	context.DeadlineExceeded: rpctypes.ErrGRPCDeadlineExceeded,
}

func togRPCError(err error) error {
	// let gRPC server convert to codes.Canceled, codes.DeadlineExceeded
	if errorspkg.Is(err, context.Canceled) || errorspkg.Is(err, context.DeadlineExceeded) {
		return err
	}
	grpcErr, ok := toGRPCErrorMap[err]
	if !ok {
		return status.Error(codes.Unknown, err.Error())
	}
	return grpcErr
}

func isClientCtxErr(ctxErr error, err error) bool {
	if ctxErr != nil {
		return true
	}

	ev, ok := status.FromError(err)
	if !ok {
		return false
	}

	switch ev.Code() {
	case codes.Canceled, codes.DeadlineExceeded:
		// client-side context cancel or deadline exceeded
		// "rpc error: code = Canceled desc = context canceled"
		// "rpc error: code = DeadlineExceeded desc = context deadline exceeded"
		return true
	case codes.Unavailable:
		msg := ev.Message()
		// client-side context cancel or deadline exceeded with TLS ("http2.errClientDisconnected")
		// "rpc error: code = Unavailable desc = client disconnected"
		if msg == "client disconnected" {
			return true
		}
		// "grpc/transport.ClientTransport.CloseStream" on canceled streams
		// "rpc error: code = Unavailable desc = stream error: stream ID 21; CANCEL")
		if strings.HasPrefix(msg, "stream error: ") && strings.HasSuffix(msg, "; CANCEL") {
			return true
		}
	}
	return false
}

// in v3.4, learner is allowed to serve serializable read and endpoint status
func isRPCSupportedForLearner(req any) bool {
	switch r := req.(type) {
	case *pb.StatusRequest:
		return true
	case *pb.RangeRequest:
		return r.Serializable
	default:
		return false
	}
}
