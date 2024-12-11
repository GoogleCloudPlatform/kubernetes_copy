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

	"google.golang.org/grpc"

	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
)

type mts2mtc struct{ mts pb.MaintenanceServer }

func MaintenanceServerToMaintenanceClient(mts pb.MaintenanceServer) pb.MaintenanceClient {
	return &mts2mtc{mts}
}

func (s *mts2mtc) Alarm(ctx context.Context, r *pb.AlarmRequest, opts ...grpc.CallOption) (*pb.AlarmResponse, error) {
	return s.mts.Alarm(ctx, r)
}

func (s *mts2mtc) Status(ctx context.Context, r *pb.StatusRequest, opts ...grpc.CallOption) (*pb.StatusResponse, error) {
	return s.mts.Status(ctx, r)
}

func (s *mts2mtc) Defragment(ctx context.Context, dr *pb.DefragmentRequest, opts ...grpc.CallOption) (*pb.DefragmentResponse, error) {
	return s.mts.Defragment(ctx, dr)
}

func (s *mts2mtc) Hash(ctx context.Context, r *pb.HashRequest, opts ...grpc.CallOption) (*pb.HashResponse, error) {
	return s.mts.Hash(ctx, r)
}

func (s *mts2mtc) HashKV(ctx context.Context, r *pb.HashKVRequest, opts ...grpc.CallOption) (*pb.HashKVResponse, error) {
	return s.mts.HashKV(ctx, r)
}

func (s *mts2mtc) MoveLeader(ctx context.Context, r *pb.MoveLeaderRequest, opts ...grpc.CallOption) (*pb.MoveLeaderResponse, error) {
	return s.mts.MoveLeader(ctx, r)
}

func (s *mts2mtc) Downgrade(ctx context.Context, r *pb.DowngradeRequest, opts ...grpc.CallOption) (*pb.DowngradeResponse, error) {
	return s.mts.Downgrade(ctx, r)
}

func (s *mts2mtc) Snapshot(ctx context.Context, in *pb.SnapshotRequest, opts ...grpc.CallOption) (pb.Maintenance_SnapshotClient, error) {
	cs := newPipeStream(ctx, func(ss chanServerStream) error {
		return s.mts.Snapshot(in, &ss2scServerStream{ss})
	})
	return &ss2scClientStream{cs}, nil
}

// ss2scClientStream implements Maintenance_SnapshotClient
type ss2scClientStream struct{ chanClientStream }

// ss2scServerStream implements Maintenance_SnapshotServer
type ss2scServerStream struct{ chanServerStream }

func (s *ss2scClientStream) Send(rr *pb.SnapshotRequest) error {
	return s.SendMsg(rr)
}
func (s *ss2scClientStream) Recv() (*pb.SnapshotResponse, error) {
	var v any
	if err := s.RecvMsg(&v); err != nil {
		return nil, err
	}
	return v.(*pb.SnapshotResponse), nil
}

func (s *ss2scServerStream) Send(rr *pb.SnapshotResponse) error {
	return s.SendMsg(rr)
}
func (s *ss2scServerStream) Recv() (*pb.SnapshotRequest, error) {
	var v any
	if err := s.RecvMsg(&v); err != nil {
		return nil, err
	}
	return v.(*pb.SnapshotRequest), nil
}
