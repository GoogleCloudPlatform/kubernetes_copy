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

type ls2lc struct {
	leaseServer pb.LeaseServer
}

func LeaseServerToLeaseClient(ls pb.LeaseServer) pb.LeaseClient {
	return &ls2lc{ls}
}

func (c *ls2lc) LeaseGrant(ctx context.Context, in *pb.LeaseGrantRequest, opts ...grpc.CallOption) (*pb.LeaseGrantResponse, error) {
	return c.leaseServer.LeaseGrant(ctx, in)
}

func (c *ls2lc) LeaseRevoke(ctx context.Context, in *pb.LeaseRevokeRequest, opts ...grpc.CallOption) (*pb.LeaseRevokeResponse, error) {
	return c.leaseServer.LeaseRevoke(ctx, in)
}

func (c *ls2lc) LeaseKeepAlive(ctx context.Context, opts ...grpc.CallOption) (pb.Lease_LeaseKeepAliveClient, error) {
	cs := newPipeStream(ctx, func(ss chanServerStream) error {
		return c.leaseServer.LeaseKeepAlive(&ls2lcServerStream{ss})
	})
	return &ls2lcClientStream{cs}, nil
}

func (c *ls2lc) LeaseTimeToLive(ctx context.Context, in *pb.LeaseTimeToLiveRequest, opts ...grpc.CallOption) (*pb.LeaseTimeToLiveResponse, error) {
	return c.leaseServer.LeaseTimeToLive(ctx, in)
}

func (c *ls2lc) LeaseLeases(ctx context.Context, in *pb.LeaseLeasesRequest, opts ...grpc.CallOption) (*pb.LeaseLeasesResponse, error) {
	return c.leaseServer.LeaseLeases(ctx, in)
}

// ls2lcClientStream implements Lease_LeaseKeepAliveClient
type ls2lcClientStream struct{ chanClientStream }

// ls2lcServerStream implements Lease_LeaseKeepAliveServer
type ls2lcServerStream struct{ chanServerStream }

func (s *ls2lcClientStream) Send(rr *pb.LeaseKeepAliveRequest) error {
	return s.SendMsg(rr)
}
func (s *ls2lcClientStream) Recv() (*pb.LeaseKeepAliveResponse, error) {
	var v any
	if err := s.RecvMsg(&v); err != nil {
		return nil, err
	}
	return v.(*pb.LeaseKeepAliveResponse), nil
}

func (s *ls2lcServerStream) Send(rr *pb.LeaseKeepAliveResponse) error {
	return s.SendMsg(rr)
}
func (s *ls2lcServerStream) Recv() (*pb.LeaseKeepAliveRequest, error) {
	var v any
	if err := s.RecvMsg(&v); err != nil {
		return nil, err
	}
	return v.(*pb.LeaseKeepAliveRequest), nil
}
