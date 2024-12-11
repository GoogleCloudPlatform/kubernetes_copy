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
	"sync"
	"time"
	"unicode/utf8"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"

	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/v3rpc/rpctypes"
	"go.etcd.io/etcd/client/pkg/v3/types"
	"go.etcd.io/etcd/server/v3/etcdserver"
	"go.etcd.io/etcd/server/v3/etcdserver/api"
	"go.etcd.io/raft/v3"
)

const (
	maxNoLeaderCnt = 3
	snapshotMethod = "/etcdserverpb.Maintenance/Snapshot"
)

type streamsMap struct {
	mu      sync.Mutex
	streams map[grpc.ServerStream]struct{}
}

func newUnaryInterceptor(s *etcdserver.EtcdServer) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if !api.IsCapabilityEnabled(api.V3rpcCapability) {
			return nil, rpctypes.ErrGRPCNotCapable
		}

		if s.IsMemberExist(s.MemberID()) && s.IsLearner() && !isRPCSupportedForLearner(req) {
			return nil, rpctypes.ErrGRPCNotSupportedForLearner
		}

		md, ok := metadata.FromIncomingContext(ctx)
		if ok {
			ver, vs := "unknown", md.Get(rpctypes.MetadataClientAPIVersionKey)
			if len(vs) > 0 {
				ver = vs[0]
			}
			if !utf8.ValidString(ver) {
				return nil, rpctypes.ErrGRPCInvalidClientAPIVersion
			}
			clientRequests.WithLabelValues("unary", ver).Inc()

			if ks := md[rpctypes.MetadataRequireLeaderKey]; len(ks) > 0 && ks[0] == rpctypes.MetadataHasLeader {
				if s.Leader() == types.ID(raft.None) {
					return nil, rpctypes.ErrGRPCNoLeader
				}
			}
		}

		return handler(ctx, req)
	}
}

func newLogUnaryInterceptor(s *etcdserver.EtcdServer) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		startTime := time.Now()
		resp, err := handler(ctx, req)
		lg := s.Logger()
		if lg != nil { // acquire stats if debug level is enabled or RequestInfo is expensive
			defer logUnaryRequestStats(ctx, lg, s.Cfg.WarningUnaryRequestDuration, info, startTime, req, resp)
		}
		return resp, err
	}
}

func logUnaryRequestStats(ctx context.Context, lg *zap.Logger, warnLatency time.Duration, info *grpc.UnaryServerInfo, startTime time.Time, req any, resp any) {
	duration := time.Since(startTime)
	var enabledDebugLevel, expensiveRequest bool
	if lg.Core().Enabled(zap.DebugLevel) {
		enabledDebugLevel = true
	}
	if duration > warnLatency {
		expensiveRequest = true
	}
	if !enabledDebugLevel && !expensiveRequest {
		return
	}
	remote := "No remote client info."
	peerInfo, ok := peer.FromContext(ctx)
	if ok {
		remote = peerInfo.Addr.String()
	}
	responseType := info.FullMethod
	var reqCount, respCount int64
	var reqSize, respSize int
	var reqContent string
	switch _resp := resp.(type) {
	case *pb.RangeResponse:
		_req, ok := req.(*pb.RangeRequest)
		if ok {
			reqCount = 0
			reqSize = _req.Size()
			reqContent = _req.String()
		}
		if _resp != nil {
			respCount = _resp.GetCount()
			respSize = _resp.Size()
		}
	case *pb.PutResponse:
		_req, ok := req.(*pb.PutRequest)
		if ok {
			reqCount = 1
			reqSize = _req.Size()
			reqContent = pb.NewLoggablePutRequest(_req).String()
			// redact value field from request content, see PR #9821
		}
		if _resp != nil {
			respCount = 0
			respSize = _resp.Size()
		}
	case *pb.DeleteRangeResponse:
		_req, ok := req.(*pb.DeleteRangeRequest)
		if ok {
			reqCount = 0
			reqSize = _req.Size()
			reqContent = _req.String()
		}
		if _resp != nil {
			respCount = _resp.GetDeleted()
			respSize = _resp.Size()
		}
	case *pb.TxnResponse:
		_req, ok := req.(*pb.TxnRequest)
		if ok && _resp != nil {
			if _resp.GetSucceeded() { // determine the 'actual' count and size of request based on success or failure
				reqCount = int64(len(_req.GetSuccess()))
				reqSize = 0
				for _, r := range _req.GetSuccess() {
					reqSize += r.Size()
				}
			} else {
				reqCount = int64(len(_req.GetFailure()))
				reqSize = 0
				for _, r := range _req.GetFailure() {
					reqSize += r.Size()
				}
			}
			reqContent = pb.NewLoggableTxnRequest(_req).String()
			// redact value field from request content, see PR #9821
		}
		if _resp != nil {
			respCount = 0
			respSize = _resp.Size()
		}
	default:
		reqCount = -1
		reqSize = -1
		respCount = -1
		respSize = -1
	}

	if enabledDebugLevel {
		logGenericRequestStats(lg, startTime, duration, remote, responseType, reqCount, reqSize, respCount, respSize, reqContent)
	} else if expensiveRequest {
		logExpensiveRequestStats(lg, startTime, duration, remote, responseType, reqCount, reqSize, respCount, respSize, reqContent)
	}
}

func logGenericRequestStats(lg *zap.Logger, startTime time.Time, duration time.Duration, remote string, responseType string,
	reqCount int64, reqSize int, respCount int64, respSize int, reqContent string) {
	lg.Debug("request stats",
		zap.Time("start time", startTime),
		zap.Duration("time spent", duration),
		zap.String("remote", remote),
		zap.String("response type", responseType),
		zap.Int64("request count", reqCount),
		zap.Int("request size", reqSize),
		zap.Int64("response count", respCount),
		zap.Int("response size", respSize),
		zap.String("request content", reqContent),
	)
}

func logExpensiveRequestStats(lg *zap.Logger, startTime time.Time, duration time.Duration, remote string, responseType string,
	reqCount int64, reqSize int, respCount int64, respSize int, reqContent string) {
	lg.Warn("request stats",
		zap.Time("start time", startTime),
		zap.Duration("time spent", duration),
		zap.String("remote", remote),
		zap.String("response type", responseType),
		zap.Int64("request count", reqCount),
		zap.Int("request size", reqSize),
		zap.Int64("response count", respCount),
		zap.Int("response size", respSize),
		zap.String("request content", reqContent),
	)
}

func newStreamInterceptor(s *etcdserver.EtcdServer) grpc.StreamServerInterceptor {
	smap := monitorLeader(s)

	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if !api.IsCapabilityEnabled(api.V3rpcCapability) {
			return rpctypes.ErrGRPCNotCapable
		}

		if s.IsMemberExist(s.MemberID()) && s.IsLearner() && info.FullMethod != snapshotMethod { // learner does not support stream RPC except Snapshot
			return rpctypes.ErrGRPCNotSupportedForLearner
		}

		md, ok := metadata.FromIncomingContext(ss.Context())
		if ok {
			ver, vs := "unknown", md.Get(rpctypes.MetadataClientAPIVersionKey)
			if len(vs) > 0 {
				ver = vs[0]
			}
			if !utf8.ValidString(ver) {
				return rpctypes.ErrGRPCInvalidClientAPIVersion
			}
			clientRequests.WithLabelValues("stream", ver).Inc()

			if ks := md[rpctypes.MetadataRequireLeaderKey]; len(ks) > 0 && ks[0] == rpctypes.MetadataHasLeader {
				if s.Leader() == types.ID(raft.None) {
					return rpctypes.ErrGRPCNoLeader
				}

				ctx := newCancellableContext(ss.Context())
				ss = serverStreamWithCtx{ctx: ctx, ServerStream: ss}

				smap.mu.Lock()
				smap.streams[ss] = struct{}{}
				smap.mu.Unlock()

				defer func() {
					smap.mu.Lock()
					delete(smap.streams, ss)
					smap.mu.Unlock()
					// TODO: investigate whether the reason for cancellation here is useful to know
					ctx.Cancel(nil)
				}()
			}
		}

		return handler(srv, ss)
	}
}

// cancellableContext wraps a context with new cancellable context that allows a
// specific cancellation error to be preserved and later retrieved using the
// Context.Err() function. This is so downstream context users can disambiguate
// the reason for the cancellation which could be from the client (for example)
// or from this interceptor code.
type cancellableContext struct {
	context.Context

	lock         sync.RWMutex
	cancel       context.CancelFunc
	cancelReason error
}

func newCancellableContext(parent context.Context) *cancellableContext {
	ctx, cancel := context.WithCancel(parent)
	return &cancellableContext{
		Context: ctx,
		cancel:  cancel,
	}
}

// Cancel stores the cancellation reason and then delegates to context.WithCancel
// against the parent context.
func (c *cancellableContext) Cancel(reason error) {
	c.lock.Lock()
	c.cancelReason = reason
	c.lock.Unlock()
	c.cancel()
}

// Err will return the preserved cancel reason error if present, and will
// otherwise return the underlying error from the parent context.
func (c *cancellableContext) Err() error {
	c.lock.RLock()
	defer c.lock.RUnlock()
	if c.cancelReason != nil {
		return c.cancelReason
	}
	return c.Context.Err()
}

type serverStreamWithCtx struct {
	grpc.ServerStream

	// ctx is used so that we can preserve a reason for cancellation.
	ctx *cancellableContext
}

func (ssc serverStreamWithCtx) Context() context.Context { return ssc.ctx }

func monitorLeader(s *etcdserver.EtcdServer) *streamsMap {
	smap := &streamsMap{
		streams: make(map[grpc.ServerStream]struct{}),
	}

	s.GoAttach(func() {
		election := time.Duration(s.Cfg.TickMs) * time.Duration(s.Cfg.ElectionTicks) * time.Millisecond
		noLeaderCnt := 0

		for {
			select {
			case <-s.StoppingNotify():
				return
			case <-time.After(election):
				if s.Leader() == types.ID(raft.None) {
					noLeaderCnt++
				} else {
					noLeaderCnt = 0
				}

				// We are more conservative on canceling existing streams. Reconnecting streams
				// cost much more than just rejecting new requests. So we wait until the member
				// cannot find a leader for maxNoLeaderCnt election timeouts to cancel existing streams.
				if noLeaderCnt >= maxNoLeaderCnt {
					smap.mu.Lock()
					for ss := range smap.streams {
						if ssWithCtx, ok := ss.(serverStreamWithCtx); ok {
							ssWithCtx.ctx.Cancel(rpctypes.ErrGRPCNoLeader)
							<-ss.Context().Done()
						}
					}
					smap.streams = make(map[grpc.ServerStream]struct{})
					smap.mu.Unlock()
				}
			}
		}
	})

	return smap
}
