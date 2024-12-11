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

	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/v3rpc/rpctypes"
	"go.etcd.io/etcd/client/pkg/v3/types"
	"go.etcd.io/etcd/server/v3/etcdserver"
	"go.etcd.io/etcd/server/v3/storage"
)

type quotaKVServer struct {
	pb.KVServer
	qa quotaAlarmer
}

type quotaAlarmer struct {
	q  storage.Quota
	a  Alarmer
	id types.ID
}

// check whether request satisfies the quota. If there is not enough space,
// ignore request and raise the free space alarm.
func (qa *quotaAlarmer) check(ctx context.Context, r any) error {
	if qa.q.Available(r) {
		return nil
	}
	req := &pb.AlarmRequest{
		MemberID: uint64(qa.id),
		Action:   pb.AlarmRequest_ACTIVATE,
		Alarm:    pb.AlarmType_NOSPACE,
	}
	qa.a.Alarm(ctx, req)
	return rpctypes.ErrGRPCNoSpace
}

func NewQuotaKVServer(s *etcdserver.EtcdServer) pb.KVServer {
	return &quotaKVServer{
		NewKVServer(s),
		quotaAlarmer{newBackendQuota(s, "kv"), s, s.MemberID()},
	}
}

func (s *quotaKVServer) Put(ctx context.Context, r *pb.PutRequest) (*pb.PutResponse, error) {
	if err := s.qa.check(ctx, r); err != nil {
		return nil, err
	}
	return s.KVServer.Put(ctx, r)
}

func (s *quotaKVServer) Txn(ctx context.Context, r *pb.TxnRequest) (*pb.TxnResponse, error) {
	if err := s.qa.check(ctx, r); err != nil {
		return nil, err
	}
	return s.KVServer.Txn(ctx, r)
}

type quotaLeaseServer struct {
	pb.LeaseServer
	qa quotaAlarmer
}

func (s *quotaLeaseServer) LeaseGrant(ctx context.Context, cr *pb.LeaseGrantRequest) (*pb.LeaseGrantResponse, error) {
	if err := s.qa.check(ctx, cr); err != nil {
		return nil, err
	}
	return s.LeaseServer.LeaseGrant(ctx, cr)
}

func NewQuotaLeaseServer(s *etcdserver.EtcdServer) pb.LeaseServer {
	return &quotaLeaseServer{
		NewLeaseServer(s),
		quotaAlarmer{newBackendQuota(s, "lease"), s, s.MemberID()},
	}
}

func newBackendQuota(s *etcdserver.EtcdServer, name string) storage.Quota {
	return storage.NewBackendQuota(s.Logger(), s.Cfg.QuotaBackendBytes, s.Backend(), name)
}
