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

type cls2clc struct{ cls pb.ClusterServer }

func ClusterServerToClusterClient(cls pb.ClusterServer) pb.ClusterClient {
	return &cls2clc{cls}
}

func (s *cls2clc) MemberList(ctx context.Context, r *pb.MemberListRequest, opts ...grpc.CallOption) (*pb.MemberListResponse, error) {
	return s.cls.MemberList(ctx, r)
}

func (s *cls2clc) MemberAdd(ctx context.Context, r *pb.MemberAddRequest, opts ...grpc.CallOption) (*pb.MemberAddResponse, error) {
	return s.cls.MemberAdd(ctx, r)
}

func (s *cls2clc) MemberUpdate(ctx context.Context, r *pb.MemberUpdateRequest, opts ...grpc.CallOption) (*pb.MemberUpdateResponse, error) {
	return s.cls.MemberUpdate(ctx, r)
}

func (s *cls2clc) MemberRemove(ctx context.Context, r *pb.MemberRemoveRequest, opts ...grpc.CallOption) (*pb.MemberRemoveResponse, error) {
	return s.cls.MemberRemove(ctx, r)
}

func (s *cls2clc) MemberPromote(ctx context.Context, r *pb.MemberPromoteRequest, opts ...grpc.CallOption) (*pb.MemberPromoteResponse, error) {
	return s.cls.MemberPromote(ctx, r)
}
