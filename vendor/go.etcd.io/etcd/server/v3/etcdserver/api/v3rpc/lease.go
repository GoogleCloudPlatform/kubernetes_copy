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
	"errors"
	"io"

	"go.uber.org/zap"

	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/v3rpc/rpctypes"
	"go.etcd.io/etcd/server/v3/etcdserver"
	"go.etcd.io/etcd/server/v3/lease"
)

type LeaseServer struct {
	lg  *zap.Logger
	hdr header
	le  etcdserver.Lessor
}

func NewLeaseServer(s *etcdserver.EtcdServer) pb.LeaseServer {
	srv := &LeaseServer{lg: s.Cfg.Logger, le: s, hdr: newHeader(s)}
	if srv.lg == nil {
		srv.lg = zap.NewNop()
	}
	return srv
}

func (ls *LeaseServer) LeaseGrant(ctx context.Context, cr *pb.LeaseGrantRequest) (*pb.LeaseGrantResponse, error) {
	resp, err := ls.le.LeaseGrant(ctx, cr)

	if err != nil {
		return nil, togRPCError(err)
	}
	ls.hdr.fill(resp.Header)
	return resp, nil
}

func (ls *LeaseServer) LeaseRevoke(ctx context.Context, rr *pb.LeaseRevokeRequest) (*pb.LeaseRevokeResponse, error) {
	resp, err := ls.le.LeaseRevoke(ctx, rr)
	if err != nil {
		return nil, togRPCError(err)
	}
	ls.hdr.fill(resp.Header)
	return resp, nil
}

func (ls *LeaseServer) LeaseTimeToLive(ctx context.Context, rr *pb.LeaseTimeToLiveRequest) (*pb.LeaseTimeToLiveResponse, error) {
	resp, err := ls.le.LeaseTimeToLive(ctx, rr)
	if err != nil && !errors.Is(err, lease.ErrLeaseNotFound) {
		return nil, togRPCError(err)
	}
	if errors.Is(err, lease.ErrLeaseNotFound) {
		resp = &pb.LeaseTimeToLiveResponse{
			Header: &pb.ResponseHeader{},
			ID:     rr.ID,
			TTL:    -1,
		}
	}
	ls.hdr.fill(resp.Header)
	return resp, nil
}

func (ls *LeaseServer) LeaseLeases(ctx context.Context, rr *pb.LeaseLeasesRequest) (*pb.LeaseLeasesResponse, error) {
	resp, err := ls.le.LeaseLeases(ctx, rr)
	if err != nil && !errors.Is(err, lease.ErrLeaseNotFound) {
		return nil, togRPCError(err)
	}
	if errors.Is(err, lease.ErrLeaseNotFound) {
		resp = &pb.LeaseLeasesResponse{
			Header: &pb.ResponseHeader{},
			Leases: []*pb.LeaseStatus{},
		}
	}
	ls.hdr.fill(resp.Header)
	return resp, nil
}

func (ls *LeaseServer) LeaseKeepAlive(stream pb.Lease_LeaseKeepAliveServer) (err error) {
	errc := make(chan error, 1)
	go func() {
		errc <- ls.leaseKeepAlive(stream)
	}()
	select {
	case err = <-errc:
	case <-stream.Context().Done():
		// the only server-side cancellation is noleader for now.
		err = stream.Context().Err()
		if errors.Is(err, context.Canceled) {
			err = rpctypes.ErrGRPCNoLeader
		}
	}
	return err
}

func (ls *LeaseServer) leaseKeepAlive(stream pb.Lease_LeaseKeepAliveServer) error {
	for {
		req, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			if isClientCtxErr(stream.Context().Err(), err) {
				ls.lg.Debug("failed to receive lease keepalive request from gRPC stream", zap.Error(err))
			} else {
				ls.lg.Warn("failed to receive lease keepalive request from gRPC stream", zap.Error(err))
				streamFailures.WithLabelValues("receive", "lease-keepalive").Inc()
			}
			return err
		}

		// Create header before we sent out the renew request.
		// This can make sure that the revision is strictly smaller or equal to
		// when the keepalive happened at the local server (when the local server is the leader)
		// or remote leader.
		// Without this, a lease might be revoked at rev 3 but client can see the keepalive succeeded
		// at rev 4.
		resp := &pb.LeaseKeepAliveResponse{ID: req.ID, Header: &pb.ResponseHeader{}}
		ls.hdr.fill(resp.Header)

		ttl, err := ls.le.LeaseRenew(stream.Context(), lease.LeaseID(req.ID))
		if errors.Is(err, lease.ErrLeaseNotFound) {
			err = nil
			ttl = 0
		}

		if err != nil {
			return togRPCError(err)
		}

		resp.TTL = ttl
		err = stream.Send(resp)
		if err != nil {
			if isClientCtxErr(stream.Context().Err(), err) {
				ls.lg.Debug("failed to send lease keepalive response to gRPC stream", zap.Error(err))
			} else {
				ls.lg.Warn("failed to send lease keepalive response to gRPC stream", zap.Error(err))
				streamFailures.WithLabelValues("send", "lease-keepalive").Inc()
			}
			return err
		}
	}
}
