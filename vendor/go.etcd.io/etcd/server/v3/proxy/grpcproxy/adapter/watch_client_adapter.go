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

package adapter

import (
	"context"
	"errors"

	"google.golang.org/grpc"

	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
)

var errAlreadySentHeader = errors.New("adapter: already sent header")

type ws2wc struct{ wserv pb.WatchServer }

func WatchServerToWatchClient(wserv pb.WatchServer) pb.WatchClient {
	return &ws2wc{wserv}
}

func (s *ws2wc) Watch(ctx context.Context, opts ...grpc.CallOption) (pb.Watch_WatchClient, error) {
	cs := newPipeStream(ctx, func(ss chanServerStream) error {
		return s.wserv.Watch(&ws2wcServerStream{ss})
	})
	return &ws2wcClientStream{cs}, nil
}

// ws2wcClientStream implements Watch_WatchClient
type ws2wcClientStream struct{ chanClientStream }

// ws2wcServerStream implements Watch_WatchServer
type ws2wcServerStream struct{ chanServerStream }

func (s *ws2wcClientStream) Send(wr *pb.WatchRequest) error {
	return s.SendMsg(wr)
}
func (s *ws2wcClientStream) Recv() (*pb.WatchResponse, error) {
	var v any
	if err := s.RecvMsg(&v); err != nil {
		return nil, err
	}
	return v.(*pb.WatchResponse), nil
}

func (s *ws2wcServerStream) Send(wr *pb.WatchResponse) error {
	return s.SendMsg(wr)
}
func (s *ws2wcServerStream) Recv() (*pb.WatchRequest, error) {
	var v any
	if err := s.RecvMsg(&v); err != nil {
		return nil, err
	}
	return v.(*pb.WatchRequest), nil
}
