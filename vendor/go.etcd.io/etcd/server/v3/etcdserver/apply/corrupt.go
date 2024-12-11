// Copyright 2022 The etcd Authors
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
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/pkg/v3/traceutil"
	"go.etcd.io/etcd/server/v3/etcdserver/errors"
)

type applierV3Corrupt struct {
	applierV3
}

func newApplierV3Corrupt(a applierV3) *applierV3Corrupt { return &applierV3Corrupt{a} }

func (a *applierV3Corrupt) Put(_ *pb.PutRequest) (*pb.PutResponse, *traceutil.Trace, error) {
	return nil, nil, errors.ErrCorrupt
}

func (a *applierV3Corrupt) Range(_ *pb.RangeRequest) (*pb.RangeResponse, *traceutil.Trace, error) {
	return nil, nil, errors.ErrCorrupt
}

func (a *applierV3Corrupt) DeleteRange(_ *pb.DeleteRangeRequest) (*pb.DeleteRangeResponse, *traceutil.Trace, error) {
	return nil, nil, errors.ErrCorrupt
}

func (a *applierV3Corrupt) Txn(_ *pb.TxnRequest) (*pb.TxnResponse, *traceutil.Trace, error) {
	return nil, nil, errors.ErrCorrupt
}

func (a *applierV3Corrupt) Compaction(_ *pb.CompactionRequest) (*pb.CompactionResponse, <-chan struct{}, *traceutil.Trace, error) {
	return nil, nil, nil, errors.ErrCorrupt
}

func (a *applierV3Corrupt) LeaseGrant(_ *pb.LeaseGrantRequest) (*pb.LeaseGrantResponse, error) {
	return nil, errors.ErrCorrupt
}

func (a *applierV3Corrupt) LeaseRevoke(_ *pb.LeaseRevokeRequest) (*pb.LeaseRevokeResponse, error) {
	return nil, errors.ErrCorrupt
}
