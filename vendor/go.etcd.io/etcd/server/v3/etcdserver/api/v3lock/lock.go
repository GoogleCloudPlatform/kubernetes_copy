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

package v3lock

import (
	"context"

	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/client/v3/concurrency"
	"go.etcd.io/etcd/server/v3/etcdserver/api/v3lock/v3lockpb"
)

type lockServer struct {
	c *clientv3.Client
}

func NewLockServer(c *clientv3.Client) v3lockpb.LockServer {
	return &lockServer{c}
}

func (ls *lockServer) Lock(ctx context.Context, req *v3lockpb.LockRequest) (*v3lockpb.LockResponse, error) {
	s, err := concurrency.NewSession(
		ls.c,
		concurrency.WithLease(clientv3.LeaseID(req.Lease)),
		concurrency.WithContext(ctx),
	)
	if err != nil {
		return nil, err
	}
	s.Orphan()
	m := concurrency.NewMutex(s, string(req.Name))
	if err = m.Lock(ctx); err != nil {
		return nil, err
	}
	return &v3lockpb.LockResponse{Header: m.Header(), Key: []byte(m.Key())}, nil
}

func (ls *lockServer) Unlock(ctx context.Context, req *v3lockpb.UnlockRequest) (*v3lockpb.UnlockResponse, error) {
	resp, err := ls.c.Delete(ctx, string(req.Key))
	if err != nil {
		return nil, err
	}
	return &v3lockpb.UnlockResponse{Header: resp.Header}, nil
}
