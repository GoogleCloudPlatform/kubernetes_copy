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

	"go.etcd.io/etcd/server/v3/etcdserver/api/v3election/v3electionpb"
)

type es2ec struct{ es v3electionpb.ElectionServer }

func ElectionServerToElectionClient(es v3electionpb.ElectionServer) v3electionpb.ElectionClient {
	return &es2ec{es}
}

func (s *es2ec) Campaign(ctx context.Context, r *v3electionpb.CampaignRequest, opts ...grpc.CallOption) (*v3electionpb.CampaignResponse, error) {
	return s.es.Campaign(ctx, r)
}

func (s *es2ec) Proclaim(ctx context.Context, r *v3electionpb.ProclaimRequest, opts ...grpc.CallOption) (*v3electionpb.ProclaimResponse, error) {
	return s.es.Proclaim(ctx, r)
}

func (s *es2ec) Leader(ctx context.Context, r *v3electionpb.LeaderRequest, opts ...grpc.CallOption) (*v3electionpb.LeaderResponse, error) {
	return s.es.Leader(ctx, r)
}

func (s *es2ec) Resign(ctx context.Context, r *v3electionpb.ResignRequest, opts ...grpc.CallOption) (*v3electionpb.ResignResponse, error) {
	return s.es.Resign(ctx, r)
}

func (s *es2ec) Observe(ctx context.Context, in *v3electionpb.LeaderRequest, opts ...grpc.CallOption) (v3electionpb.Election_ObserveClient, error) {
	cs := newPipeStream(ctx, func(ss chanServerStream) error {
		return s.es.Observe(in, &es2ecServerStream{ss})
	})
	return &es2ecClientStream{cs}, nil
}

// es2ecClientStream implements Election_ObserveClient
type es2ecClientStream struct{ chanClientStream }

// es2ecServerStream implements Election_ObserveServer
type es2ecServerStream struct{ chanServerStream }

func (s *es2ecClientStream) Send(rr *v3electionpb.LeaderRequest) error {
	return s.SendMsg(rr)
}
func (s *es2ecClientStream) Recv() (*v3electionpb.LeaderResponse, error) {
	var v any
	if err := s.RecvMsg(&v); err != nil {
		return nil, err
	}
	return v.(*v3electionpb.LeaderResponse), nil
}

func (s *es2ecServerStream) Send(rr *v3electionpb.LeaderResponse) error {
	return s.SendMsg(rr)
}
func (s *es2ecServerStream) Recv() (*v3electionpb.LeaderRequest, error) {
	var v any
	if err := s.RecvMsg(&v); err != nil {
		return nil, err
	}
	return v.(*v3electionpb.LeaderRequest), nil
}
