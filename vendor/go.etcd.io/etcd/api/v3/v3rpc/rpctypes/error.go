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

package rpctypes

import (
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// server-side error
var (
	ErrGRPCEmptyKey                = status.Error(codes.InvalidArgument, "etcdserver: key is not provided")
	ErrGRPCKeyNotFound             = status.Error(codes.InvalidArgument, "etcdserver: key not found")
	ErrGRPCValueProvided           = status.Error(codes.InvalidArgument, "etcdserver: value is provided")
	ErrGRPCLeaseProvided           = status.Error(codes.InvalidArgument, "etcdserver: lease is provided")
	ErrGRPCTooManyOps              = status.Error(codes.InvalidArgument, "etcdserver: too many operations in txn request")
	ErrGRPCDuplicateKey            = status.Error(codes.InvalidArgument, "etcdserver: duplicate key given in txn request")
	ErrGRPCInvalidClientAPIVersion = status.Error(codes.InvalidArgument, "etcdserver: invalid client api version")
	ErrGRPCInvalidSortOption       = status.Error(codes.InvalidArgument, "etcdserver: invalid sort option")
	ErrGRPCCompacted               = status.Error(codes.OutOfRange, "etcdserver: mvcc: required revision has been compacted")
	ErrGRPCFutureRev               = status.Error(codes.OutOfRange, "etcdserver: mvcc: required revision is a future revision")
	ErrGRPCNoSpace                 = status.Error(codes.ResourceExhausted, "etcdserver: mvcc: database space exceeded")

	ErrGRPCLeaseNotFound    = status.Error(codes.NotFound, "etcdserver: requested lease not found")
	ErrGRPCLeaseExist       = status.Error(codes.FailedPrecondition, "etcdserver: lease already exists")
	ErrGRPCLeaseTTLTooLarge = status.Error(codes.OutOfRange, "etcdserver: too large lease TTL")

	ErrGRPCWatchCanceled = status.Error(codes.Canceled, "etcdserver: watch canceled")

	ErrGRPCMemberExist            = status.Error(codes.FailedPrecondition, "etcdserver: member ID already exist")
	ErrGRPCPeerURLExist           = status.Error(codes.FailedPrecondition, "etcdserver: Peer URLs already exists")
	ErrGRPCMemberNotEnoughStarted = status.Error(codes.FailedPrecondition, "etcdserver: re-configuration failed due to not enough started members")
	ErrGRPCMemberBadURLs          = status.Error(codes.InvalidArgument, "etcdserver: given member URLs are invalid")
	ErrGRPCMemberNotFound         = status.Error(codes.NotFound, "etcdserver: member not found")
	ErrGRPCMemberNotLearner       = status.Error(codes.FailedPrecondition, "etcdserver: can only promote a learner member")
	ErrGRPCLearnerNotReady        = status.Error(codes.FailedPrecondition, "etcdserver: can only promote a learner member which is in sync with leader")
	ErrGRPCTooManyLearners        = status.Error(codes.FailedPrecondition, "etcdserver: too many learner members in cluster")
	ErrGRPCClusterIDMismatch      = status.Error(codes.FailedPrecondition, "etcdserver: cluster ID mismatch")
	//revive:disable:var-naming
	// Deprecated: Please use ErrGRPCClusterIDMismatch.
	ErrGRPCClusterIdMismatch = ErrGRPCClusterIDMismatch
	//revive:enable:var-naming

	ErrGRPCRequestTooLarge        = status.Error(codes.InvalidArgument, "etcdserver: request is too large")
	ErrGRPCRequestTooManyRequests = status.Error(codes.ResourceExhausted, "etcdserver: too many requests")

	ErrGRPCRootUserNotExist     = status.Error(codes.FailedPrecondition, "etcdserver: root user does not exist")
	ErrGRPCRootRoleNotExist     = status.Error(codes.FailedPrecondition, "etcdserver: root user does not have root role")
	ErrGRPCUserAlreadyExist     = status.Error(codes.FailedPrecondition, "etcdserver: user name already exists")
	ErrGRPCUserEmpty            = status.Error(codes.InvalidArgument, "etcdserver: user name is empty")
	ErrGRPCUserNotFound         = status.Error(codes.FailedPrecondition, "etcdserver: user name not found")
	ErrGRPCRoleAlreadyExist     = status.Error(codes.FailedPrecondition, "etcdserver: role name already exists")
	ErrGRPCRoleNotFound         = status.Error(codes.FailedPrecondition, "etcdserver: role name not found")
	ErrGRPCRoleEmpty            = status.Error(codes.InvalidArgument, "etcdserver: role name is empty")
	ErrGRPCAuthFailed           = status.Error(codes.InvalidArgument, "etcdserver: authentication failed, invalid user ID or password")
	ErrGRPCPermissionNotGiven   = status.Error(codes.InvalidArgument, "etcdserver: permission not given")
	ErrGRPCPermissionDenied     = status.Error(codes.PermissionDenied, "etcdserver: permission denied")
	ErrGRPCRoleNotGranted       = status.Error(codes.FailedPrecondition, "etcdserver: role is not granted to the user")
	ErrGRPCPermissionNotGranted = status.Error(codes.FailedPrecondition, "etcdserver: permission is not granted to the role")
	ErrGRPCAuthNotEnabled       = status.Error(codes.FailedPrecondition, "etcdserver: authentication is not enabled")
	ErrGRPCInvalidAuthToken     = status.Error(codes.Unauthenticated, "etcdserver: invalid auth token")
	ErrGRPCInvalidAuthMgmt      = status.Error(codes.InvalidArgument, "etcdserver: invalid auth management")
	ErrGRPCAuthOldRevision      = status.Error(codes.InvalidArgument, "etcdserver: revision of auth store is old")

	ErrGRPCNoLeader                   = status.Error(codes.Unavailable, "etcdserver: no leader")
	ErrGRPCNotLeader                  = status.Error(codes.FailedPrecondition, "etcdserver: not leader")
	ErrGRPCLeaderChanged              = status.Error(codes.Unavailable, "etcdserver: leader changed")
	ErrGRPCNotCapable                 = status.Error(codes.FailedPrecondition, "etcdserver: not capable")
	ErrGRPCStopped                    = status.Error(codes.Unavailable, "etcdserver: server stopped")
	ErrGRPCTimeout                    = status.Error(codes.Unavailable, "etcdserver: request timed out")
	ErrGRPCTimeoutDueToLeaderFail     = status.Error(codes.Unavailable, "etcdserver: request timed out, possibly due to previous leader failure")
	ErrGRPCTimeoutDueToConnectionLost = status.Error(codes.Unavailable, "etcdserver: request timed out, possibly due to connection lost")
	ErrGRPCTimeoutWaitAppliedIndex    = status.Error(codes.Unavailable, "etcdserver: request timed out, waiting for the applied index took too long")
	ErrGRPCUnhealthy                  = status.Error(codes.Unavailable, "etcdserver: unhealthy cluster")
	ErrGRPCCorrupt                    = status.Error(codes.DataLoss, "etcdserver: corrupt cluster")
	ErrGRPCNotSupportedForLearner     = status.Error(codes.FailedPrecondition, "etcdserver: rpc not supported for learner")
	ErrGRPCBadLeaderTransferee        = status.Error(codes.FailedPrecondition, "etcdserver: bad leader transferee")

	ErrGRPCWrongDowngradeVersionFormat   = status.Error(codes.InvalidArgument, "etcdserver: wrong downgrade target version format")
	ErrGRPCInvalidDowngradeTargetVersion = status.Error(codes.InvalidArgument, "etcdserver: invalid downgrade target version")
	ErrGRPCClusterVersionUnavailable     = status.Error(codes.FailedPrecondition, "etcdserver: cluster version not found during downgrade")
	ErrGRPCDowngradeInProcess            = status.Error(codes.FailedPrecondition, "etcdserver: cluster has a downgrade job in progress")
	ErrGRPCNoInflightDowngrade           = status.Error(codes.FailedPrecondition, "etcdserver: no inflight downgrade job")

	ErrGRPCCanceled         = status.Error(codes.Canceled, "etcdserver: request canceled")
	ErrGRPCDeadlineExceeded = status.Error(codes.DeadlineExceeded, "etcdserver: context deadline exceeded")

	errStringToError = map[string]error{
		ErrorDesc(ErrGRPCEmptyKey):      ErrGRPCEmptyKey,
		ErrorDesc(ErrGRPCKeyNotFound):   ErrGRPCKeyNotFound,
		ErrorDesc(ErrGRPCValueProvided): ErrGRPCValueProvided,
		ErrorDesc(ErrGRPCLeaseProvided): ErrGRPCLeaseProvided,

		ErrorDesc(ErrGRPCTooManyOps):        ErrGRPCTooManyOps,
		ErrorDesc(ErrGRPCDuplicateKey):      ErrGRPCDuplicateKey,
		ErrorDesc(ErrGRPCInvalidSortOption): ErrGRPCInvalidSortOption,
		ErrorDesc(ErrGRPCCompacted):         ErrGRPCCompacted,
		ErrorDesc(ErrGRPCFutureRev):         ErrGRPCFutureRev,
		ErrorDesc(ErrGRPCNoSpace):           ErrGRPCNoSpace,

		ErrorDesc(ErrGRPCLeaseNotFound):    ErrGRPCLeaseNotFound,
		ErrorDesc(ErrGRPCLeaseExist):       ErrGRPCLeaseExist,
		ErrorDesc(ErrGRPCLeaseTTLTooLarge): ErrGRPCLeaseTTLTooLarge,

		ErrorDesc(ErrGRPCMemberExist):            ErrGRPCMemberExist,
		ErrorDesc(ErrGRPCPeerURLExist):           ErrGRPCPeerURLExist,
		ErrorDesc(ErrGRPCMemberNotEnoughStarted): ErrGRPCMemberNotEnoughStarted,
		ErrorDesc(ErrGRPCMemberBadURLs):          ErrGRPCMemberBadURLs,
		ErrorDesc(ErrGRPCMemberNotFound):         ErrGRPCMemberNotFound,
		ErrorDesc(ErrGRPCMemberNotLearner):       ErrGRPCMemberNotLearner,
		ErrorDesc(ErrGRPCLearnerNotReady):        ErrGRPCLearnerNotReady,
		ErrorDesc(ErrGRPCTooManyLearners):        ErrGRPCTooManyLearners,
		ErrorDesc(ErrGRPCClusterIDMismatch):      ErrGRPCClusterIDMismatch,

		ErrorDesc(ErrGRPCRequestTooLarge):        ErrGRPCRequestTooLarge,
		ErrorDesc(ErrGRPCRequestTooManyRequests): ErrGRPCRequestTooManyRequests,

		ErrorDesc(ErrGRPCRootUserNotExist):     ErrGRPCRootUserNotExist,
		ErrorDesc(ErrGRPCRootRoleNotExist):     ErrGRPCRootRoleNotExist,
		ErrorDesc(ErrGRPCUserAlreadyExist):     ErrGRPCUserAlreadyExist,
		ErrorDesc(ErrGRPCUserEmpty):            ErrGRPCUserEmpty,
		ErrorDesc(ErrGRPCUserNotFound):         ErrGRPCUserNotFound,
		ErrorDesc(ErrGRPCRoleAlreadyExist):     ErrGRPCRoleAlreadyExist,
		ErrorDesc(ErrGRPCRoleNotFound):         ErrGRPCRoleNotFound,
		ErrorDesc(ErrGRPCRoleEmpty):            ErrGRPCRoleEmpty,
		ErrorDesc(ErrGRPCAuthFailed):           ErrGRPCAuthFailed,
		ErrorDesc(ErrGRPCPermissionDenied):     ErrGRPCPermissionDenied,
		ErrorDesc(ErrGRPCRoleNotGranted):       ErrGRPCRoleNotGranted,
		ErrorDesc(ErrGRPCPermissionNotGranted): ErrGRPCPermissionNotGranted,
		ErrorDesc(ErrGRPCAuthNotEnabled):       ErrGRPCAuthNotEnabled,
		ErrorDesc(ErrGRPCInvalidAuthToken):     ErrGRPCInvalidAuthToken,
		ErrorDesc(ErrGRPCInvalidAuthMgmt):      ErrGRPCInvalidAuthMgmt,
		ErrorDesc(ErrGRPCAuthOldRevision):      ErrGRPCAuthOldRevision,

		ErrorDesc(ErrGRPCNoLeader):                   ErrGRPCNoLeader,
		ErrorDesc(ErrGRPCNotLeader):                  ErrGRPCNotLeader,
		ErrorDesc(ErrGRPCLeaderChanged):              ErrGRPCLeaderChanged,
		ErrorDesc(ErrGRPCNotCapable):                 ErrGRPCNotCapable,
		ErrorDesc(ErrGRPCStopped):                    ErrGRPCStopped,
		ErrorDesc(ErrGRPCTimeout):                    ErrGRPCTimeout,
		ErrorDesc(ErrGRPCTimeoutDueToLeaderFail):     ErrGRPCTimeoutDueToLeaderFail,
		ErrorDesc(ErrGRPCTimeoutDueToConnectionLost): ErrGRPCTimeoutDueToConnectionLost,
		ErrorDesc(ErrGRPCUnhealthy):                  ErrGRPCUnhealthy,
		ErrorDesc(ErrGRPCCorrupt):                    ErrGRPCCorrupt,
		ErrorDesc(ErrGRPCNotSupportedForLearner):     ErrGRPCNotSupportedForLearner,
		ErrorDesc(ErrGRPCBadLeaderTransferee):        ErrGRPCBadLeaderTransferee,

		ErrorDesc(ErrGRPCClusterVersionUnavailable):     ErrGRPCClusterVersionUnavailable,
		ErrorDesc(ErrGRPCWrongDowngradeVersionFormat):   ErrGRPCWrongDowngradeVersionFormat,
		ErrorDesc(ErrGRPCInvalidDowngradeTargetVersion): ErrGRPCInvalidDowngradeTargetVersion,
		ErrorDesc(ErrGRPCDowngradeInProcess):            ErrGRPCDowngradeInProcess,
		ErrorDesc(ErrGRPCNoInflightDowngrade):           ErrGRPCNoInflightDowngrade,
	}
)

// client-side error
var (
	ErrEmptyKey          = Error(ErrGRPCEmptyKey)
	ErrKeyNotFound       = Error(ErrGRPCKeyNotFound)
	ErrValueProvided     = Error(ErrGRPCValueProvided)
	ErrLeaseProvided     = Error(ErrGRPCLeaseProvided)
	ErrTooManyOps        = Error(ErrGRPCTooManyOps)
	ErrDuplicateKey      = Error(ErrGRPCDuplicateKey)
	ErrInvalidSortOption = Error(ErrGRPCInvalidSortOption)
	ErrCompacted         = Error(ErrGRPCCompacted)
	ErrFutureRev         = Error(ErrGRPCFutureRev)
	ErrNoSpace           = Error(ErrGRPCNoSpace)

	ErrLeaseNotFound    = Error(ErrGRPCLeaseNotFound)
	ErrLeaseExist       = Error(ErrGRPCLeaseExist)
	ErrLeaseTTLTooLarge = Error(ErrGRPCLeaseTTLTooLarge)

	ErrMemberExist            = Error(ErrGRPCMemberExist)
	ErrPeerURLExist           = Error(ErrGRPCPeerURLExist)
	ErrMemberNotEnoughStarted = Error(ErrGRPCMemberNotEnoughStarted)
	ErrMemberBadURLs          = Error(ErrGRPCMemberBadURLs)
	ErrMemberNotFound         = Error(ErrGRPCMemberNotFound)
	ErrMemberNotLearner       = Error(ErrGRPCMemberNotLearner)
	ErrMemberLearnerNotReady  = Error(ErrGRPCLearnerNotReady)
	ErrTooManyLearners        = Error(ErrGRPCTooManyLearners)

	ErrRequestTooLarge = Error(ErrGRPCRequestTooLarge)
	ErrTooManyRequests = Error(ErrGRPCRequestTooManyRequests)

	ErrRootUserNotExist     = Error(ErrGRPCRootUserNotExist)
	ErrRootRoleNotExist     = Error(ErrGRPCRootRoleNotExist)
	ErrUserAlreadyExist     = Error(ErrGRPCUserAlreadyExist)
	ErrUserEmpty            = Error(ErrGRPCUserEmpty)
	ErrUserNotFound         = Error(ErrGRPCUserNotFound)
	ErrRoleAlreadyExist     = Error(ErrGRPCRoleAlreadyExist)
	ErrRoleNotFound         = Error(ErrGRPCRoleNotFound)
	ErrRoleEmpty            = Error(ErrGRPCRoleEmpty)
	ErrAuthFailed           = Error(ErrGRPCAuthFailed)
	ErrPermissionDenied     = Error(ErrGRPCPermissionDenied)
	ErrRoleNotGranted       = Error(ErrGRPCRoleNotGranted)
	ErrPermissionNotGranted = Error(ErrGRPCPermissionNotGranted)
	ErrAuthNotEnabled       = Error(ErrGRPCAuthNotEnabled)
	ErrInvalidAuthToken     = Error(ErrGRPCInvalidAuthToken)
	ErrAuthOldRevision      = Error(ErrGRPCAuthOldRevision)
	ErrInvalidAuthMgmt      = Error(ErrGRPCInvalidAuthMgmt)
	ErrClusterIDMismatch    = Error(ErrGRPCClusterIDMismatch)
	//revive:disable:var-naming
	// Deprecated: Please use ErrGRPCClusterIDMismatch.
	ErrClusterIdMismatch = ErrClusterIDMismatch
	//revive:enable:var-naming

	ErrNoLeader                   = Error(ErrGRPCNoLeader)
	ErrNotLeader                  = Error(ErrGRPCNotLeader)
	ErrLeaderChanged              = Error(ErrGRPCLeaderChanged)
	ErrNotCapable                 = Error(ErrGRPCNotCapable)
	ErrStopped                    = Error(ErrGRPCStopped)
	ErrTimeout                    = Error(ErrGRPCTimeout)
	ErrTimeoutDueToLeaderFail     = Error(ErrGRPCTimeoutDueToLeaderFail)
	ErrTimeoutDueToConnectionLost = Error(ErrGRPCTimeoutDueToConnectionLost)
	ErrTimeoutWaitAppliedIndex    = Error(ErrGRPCTimeoutWaitAppliedIndex)
	ErrUnhealthy                  = Error(ErrGRPCUnhealthy)
	ErrCorrupt                    = Error(ErrGRPCCorrupt)
	ErrBadLeaderTransferee        = Error(ErrGRPCBadLeaderTransferee)

	ErrClusterVersionUnavailable     = Error(ErrGRPCClusterVersionUnavailable)
	ErrWrongDowngradeVersionFormat   = Error(ErrGRPCWrongDowngradeVersionFormat)
	ErrInvalidDowngradeTargetVersion = Error(ErrGRPCInvalidDowngradeTargetVersion)
	ErrDowngradeInProcess            = Error(ErrGRPCDowngradeInProcess)
	ErrNoInflightDowngrade           = Error(ErrGRPCNoInflightDowngrade)
)

// EtcdError defines gRPC server errors.
// (https://github.com/grpc/grpc-go/blob/master/rpc_util.go#L319-L323)
type EtcdError struct {
	code codes.Code
	desc string
}

// Code returns grpc/codes.Code.
// TODO: define clientv3/codes.Code.
func (e EtcdError) Code() codes.Code {
	return e.code
}

func (e EtcdError) Error() string {
	return e.desc
}

func Error(err error) error {
	if err == nil {
		return nil
	}
	verr, ok := errStringToError[ErrorDesc(err)]
	if !ok { // not gRPC error
		return err
	}
	ev, ok := status.FromError(verr)
	var desc string
	if ok {
		desc = ev.Message()
	} else {
		desc = verr.Error()
	}
	return EtcdError{code: ev.Code(), desc: desc}
}

func ErrorDesc(err error) string {
	if s, ok := status.FromError(err); ok {
		return s.Message()
	}
	return err.Error()
}
