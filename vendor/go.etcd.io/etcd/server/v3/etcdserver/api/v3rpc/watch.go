// Copyright 2015 The etcd Authors
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
	"math/rand"
	"sync"
	"time"

	"go.uber.org/zap"

	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	"go.etcd.io/etcd/api/v3/v3rpc/rpctypes"
	"go.etcd.io/etcd/client/pkg/v3/verify"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/server/v3/auth"
	"go.etcd.io/etcd/server/v3/etcdserver"
	"go.etcd.io/etcd/server/v3/etcdserver/apply"
	"go.etcd.io/etcd/server/v3/storage/mvcc"
)

const minWatchProgressInterval = 100 * time.Millisecond

type watchServer struct {
	lg *zap.Logger

	clusterID int64
	memberID  int64

	maxRequestBytes int

	sg        apply.RaftStatusGetter
	watchable mvcc.WatchableKV
	ag        AuthGetter
}

// NewWatchServer returns a new watch server.
func NewWatchServer(s *etcdserver.EtcdServer) pb.WatchServer {
	srv := &watchServer{
		lg: s.Cfg.Logger,

		clusterID: int64(s.Cluster().ID()),
		memberID:  int64(s.MemberID()),

		maxRequestBytes: int(s.Cfg.MaxRequestBytes + grpcOverheadBytes),

		sg:        s,
		watchable: s.Watchable(),
		ag:        s,
	}
	if srv.lg == nil {
		srv.lg = zap.NewNop()
	}
	if s.Cfg.WatchProgressNotifyInterval > 0 {
		if s.Cfg.WatchProgressNotifyInterval < minWatchProgressInterval {
			srv.lg.Warn(
				"adjusting watch progress notify interval to minimum period",
				zap.Duration("min-watch-progress-notify-interval", minWatchProgressInterval),
			)
			s.Cfg.WatchProgressNotifyInterval = minWatchProgressInterval
		}
		SetProgressReportInterval(s.Cfg.WatchProgressNotifyInterval)
	}
	return srv
}

var (
	// External test can read this with GetProgressReportInterval()
	// and change this to a small value to finish fast with
	// SetProgressReportInterval().
	progressReportInterval   = 10 * time.Minute
	progressReportIntervalMu sync.RWMutex
)

// GetProgressReportInterval returns the current progress report interval (for testing).
func GetProgressReportInterval() time.Duration {
	progressReportIntervalMu.RLock()
	interval := progressReportInterval
	progressReportIntervalMu.RUnlock()

	// add rand(1/10*progressReportInterval) as jitter so that etcdserver will not
	// send progress notifications to watchers around the same time even when watchers
	// are created around the same time (which is common when a client restarts itself).
	jitter := time.Duration(rand.Int63n(int64(interval) / 10))

	return interval + jitter
}

// SetProgressReportInterval updates the current progress report interval (for testing).
func SetProgressReportInterval(newTimeout time.Duration) {
	progressReportIntervalMu.Lock()
	progressReportInterval = newTimeout
	progressReportIntervalMu.Unlock()
}

// We send ctrl response inside the read loop. We do not want
// send to block read, but we still want ctrl response we sent to
// be serialized. Thus we use a buffered chan to solve the problem.
// A small buffer should be OK for most cases, since we expect the
// ctrl requests are infrequent.
const ctrlStreamBufLen = 16

// serverWatchStream is an etcd server side stream. It receives requests
// from client side gRPC stream. It receives watch events from mvcc.WatchStream,
// and creates responses that forwarded to gRPC stream.
// It also forwards control message like watch created and canceled.
type serverWatchStream struct {
	lg *zap.Logger

	clusterID int64
	memberID  int64

	maxRequestBytes int

	sg        apply.RaftStatusGetter
	watchable mvcc.WatchableKV
	ag        AuthGetter

	gRPCStream  pb.Watch_WatchServer
	watchStream mvcc.WatchStream
	ctrlStream  chan *pb.WatchResponse

	// mu protects progress, prevKV, fragment
	mu sync.RWMutex
	// tracks the watchID that stream might need to send progress to
	// TODO: combine progress and prevKV into a single struct?
	progress map[mvcc.WatchID]bool
	// record watch IDs that need return previous key-value pair
	prevKV map[mvcc.WatchID]bool
	// records fragmented watch IDs
	fragment map[mvcc.WatchID]bool

	// closec indicates the stream is closed.
	closec chan struct{}

	// wg waits for the send loop to complete
	wg sync.WaitGroup
}

func (ws *watchServer) Watch(stream pb.Watch_WatchServer) (err error) {
	sws := serverWatchStream{
		lg: ws.lg,

		clusterID: ws.clusterID,
		memberID:  ws.memberID,

		maxRequestBytes: ws.maxRequestBytes,

		sg:        ws.sg,
		watchable: ws.watchable,
		ag:        ws.ag,

		gRPCStream:  stream,
		watchStream: ws.watchable.NewWatchStream(),
		// chan for sending control response like watcher created and canceled.
		ctrlStream: make(chan *pb.WatchResponse, ctrlStreamBufLen),

		progress: make(map[mvcc.WatchID]bool),
		prevKV:   make(map[mvcc.WatchID]bool),
		fragment: make(map[mvcc.WatchID]bool),

		closec: make(chan struct{}),
	}

	sws.wg.Add(1)
	go func() {
		sws.sendLoop()
		sws.wg.Done()
	}()

	errc := make(chan error, 1)
	// Ideally recvLoop would also use sws.wg to signal its completion
	// but when stream.Context().Done() is closed, the stream's recv
	// may continue to block since it uses a different context, leading to
	// deadlock when calling sws.close().
	go func() {
		if rerr := sws.recvLoop(); rerr != nil {
			if isClientCtxErr(stream.Context().Err(), rerr) {
				sws.lg.Debug("failed to receive watch request from gRPC stream", zap.Error(rerr))
			} else {
				sws.lg.Warn("failed to receive watch request from gRPC stream", zap.Error(rerr))
				streamFailures.WithLabelValues("receive", "watch").Inc()
			}
			errc <- rerr
		}
	}()

	// TODO: There's a race here. When a stream  is closed (e.g. due to a cancellation),
	// the underlying error (e.g. a gRPC stream error) may be returned and handled
	// through errc if the recv goroutine finishes before the send goroutine.
	// When the recv goroutine wins, the stream error is retained. When recv loses
	// the race, the underlying error is lost (unless the root error is propagated
	// through Context.Err() which is not always the case (as callers have to decide
	// to implement a custom context to do so). The stdlib context package builtins
	// may be insufficient to carry semantically useful errors around and should be
	// revisited.
	select {
	case err = <-errc:
		if errors.Is(err, context.Canceled) {
			err = rpctypes.ErrGRPCWatchCanceled
		}
		close(sws.ctrlStream)
	case <-stream.Context().Done():
		err = stream.Context().Err()
		if errors.Is(err, context.Canceled) {
			err = rpctypes.ErrGRPCWatchCanceled
		}
	}

	sws.close()
	return err
}

func (sws *serverWatchStream) isWatchPermitted(wcr *pb.WatchCreateRequest) error {
	authInfo, err := sws.ag.AuthInfoFromCtx(sws.gRPCStream.Context())
	if err != nil {
		return err
	}
	if authInfo == nil {
		// if auth is enabled, IsRangePermitted() can cause an error
		authInfo = &auth.AuthInfo{}
	}
	return sws.ag.AuthStore().IsRangePermitted(authInfo, wcr.Key, wcr.RangeEnd)
}

func (sws *serverWatchStream) recvLoop() error {
	for {
		req, err := sws.gRPCStream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}

		switch uv := req.RequestUnion.(type) {
		case *pb.WatchRequest_CreateRequest:
			if uv.CreateRequest == nil {
				break
			}

			creq := uv.CreateRequest
			if len(creq.Key) == 0 {
				// \x00 is the smallest key
				creq.Key = []byte{0}
			}
			if len(creq.RangeEnd) == 0 {
				// force nil since watchstream.Watch distinguishes
				// between nil and []byte{} for single key / >=
				creq.RangeEnd = nil
			}
			if len(creq.RangeEnd) == 1 && creq.RangeEnd[0] == 0 {
				// support  >= key queries
				creq.RangeEnd = []byte{}
			}

			err := sws.isWatchPermitted(creq)
			if err != nil {
				var cancelReason string
				switch {
				case errors.Is(err, auth.ErrInvalidAuthToken):
					cancelReason = rpctypes.ErrGRPCInvalidAuthToken.Error()
				case errors.Is(err, auth.ErrAuthOldRevision):
					cancelReason = rpctypes.ErrGRPCAuthOldRevision.Error()
				case errors.Is(err, auth.ErrUserEmpty):
					cancelReason = rpctypes.ErrGRPCUserEmpty.Error()
				default:
					if !errors.Is(err, auth.ErrPermissionDenied) {
						sws.lg.Error("unexpected error code", zap.Error(err))
					}
					cancelReason = rpctypes.ErrGRPCPermissionDenied.Error()
				}

				wr := &pb.WatchResponse{
					Header:       sws.newResponseHeader(sws.watchStream.Rev()),
					WatchId:      clientv3.InvalidWatchID,
					Canceled:     true,
					Created:      true,
					CancelReason: cancelReason,
				}

				select {
				case sws.ctrlStream <- wr:
					continue
				case <-sws.closec:
					return nil
				}
			}

			filters := FiltersFromRequest(creq)

			wsrev := sws.watchStream.Rev()
			rev := creq.StartRevision
			if rev == 0 {
				rev = wsrev + 1
			}
			id, err := sws.watchStream.Watch(mvcc.WatchID(creq.WatchId), creq.Key, creq.RangeEnd, rev, filters...)
			if err == nil {
				sws.mu.Lock()
				if creq.ProgressNotify {
					sws.progress[id] = true
				}
				if creq.PrevKv {
					sws.prevKV[id] = true
				}
				if creq.Fragment {
					sws.fragment[id] = true
				}
				sws.mu.Unlock()
			} else {
				id = clientv3.InvalidWatchID
			}

			wr := &pb.WatchResponse{
				Header:   sws.newResponseHeader(wsrev),
				WatchId:  int64(id),
				Created:  true,
				Canceled: err != nil,
			}
			if err != nil {
				wr.CancelReason = err.Error()
			}
			select {
			case sws.ctrlStream <- wr:
			case <-sws.closec:
				return nil
			}

		case *pb.WatchRequest_CancelRequest:
			if uv.CancelRequest != nil {
				id := uv.CancelRequest.WatchId
				err := sws.watchStream.Cancel(mvcc.WatchID(id))
				if err == nil {
					wr := &pb.WatchResponse{
						Header:   sws.newResponseHeader(sws.watchStream.Rev()),
						WatchId:  id,
						Canceled: true,
					}
					select {
					case sws.ctrlStream <- wr:
					case <-sws.closec:
						return nil
					}

					sws.mu.Lock()
					delete(sws.progress, mvcc.WatchID(id))
					delete(sws.prevKV, mvcc.WatchID(id))
					delete(sws.fragment, mvcc.WatchID(id))
					sws.mu.Unlock()
				}
			}
		case *pb.WatchRequest_ProgressRequest:
			if uv.ProgressRequest != nil {
				sws.mu.Lock()
				sws.watchStream.RequestProgressAll()
				sws.mu.Unlock()
			}
		default:
			// we probably should not shutdown the entire stream when
			// receive an invalid command.
			// so just do nothing instead.
			sws.lg.Sugar().Infof("invalid watch request type %T received in gRPC stream", uv)
			continue
		}
	}
}

func (sws *serverWatchStream) sendLoop() {
	// watch ids that are currently active
	ids := make(map[mvcc.WatchID]struct{})
	// watch responses pending on a watch id creation message
	pending := make(map[mvcc.WatchID][]*pb.WatchResponse)

	interval := GetProgressReportInterval()
	progressTicker := time.NewTicker(interval)

	defer func() {
		progressTicker.Stop()
		// drain the chan to clean up pending events
		for ws := range sws.watchStream.Chan() {
			mvcc.ReportEventReceived(len(ws.Events))
		}
		for _, wrs := range pending {
			for _, ws := range wrs {
				mvcc.ReportEventReceived(len(ws.Events))
			}
		}
	}()

	for {
		select {
		case wresp, ok := <-sws.watchStream.Chan():
			if !ok {
				return
			}

			// TODO: evs is []mvccpb.Event type
			// either return []*mvccpb.Event from the mvcc package
			// or define protocol buffer with []mvccpb.Event.
			evs := wresp.Events
			events := make([]*mvccpb.Event, len(evs))
			sws.mu.RLock()
			needPrevKV := sws.prevKV[wresp.WatchID]
			sws.mu.RUnlock()
			for i := range evs {
				events[i] = &evs[i]
				if needPrevKV && !IsCreateEvent(evs[i]) {
					opt := mvcc.RangeOptions{Rev: evs[i].Kv.ModRevision - 1}
					r, err := sws.watchable.Range(context.TODO(), evs[i].Kv.Key, nil, opt)
					if err == nil && len(r.KVs) != 0 {
						events[i].PrevKv = &(r.KVs[0])
					}
				}
			}

			canceled := wresp.CompactRevision != 0
			wr := &pb.WatchResponse{
				Header:          sws.newResponseHeader(wresp.Revision),
				WatchId:         int64(wresp.WatchID),
				Events:          events,
				CompactRevision: wresp.CompactRevision,
				Canceled:        canceled,
			}

			// Progress notifications can have WatchID -1
			// if they announce on behalf of multiple watchers
			if wresp.WatchID != clientv3.InvalidWatchID {
				if _, okID := ids[wresp.WatchID]; !okID {
					// buffer if id not yet announced
					wrs := append(pending[wresp.WatchID], wr)
					pending[wresp.WatchID] = wrs
					continue
				}
			}

			mvcc.ReportEventReceived(len(evs))

			sws.mu.RLock()
			fragmented, ok := sws.fragment[wresp.WatchID]
			sws.mu.RUnlock()

			var serr error
			// gofail: var beforeSendWatchResponse struct{}
			if !fragmented && !ok {
				serr = sws.gRPCStream.Send(wr)
			} else {
				serr = sendFragments(wr, sws.maxRequestBytes, sws.gRPCStream.Send)
			}

			if serr != nil {
				if isClientCtxErr(sws.gRPCStream.Context().Err(), serr) {
					sws.lg.Debug("failed to send watch response to gRPC stream", zap.Error(serr))
				} else {
					sws.lg.Warn("failed to send watch response to gRPC stream", zap.Error(serr))
					streamFailures.WithLabelValues("send", "watch").Inc()
				}
				return
			}

			sws.mu.Lock()
			if len(evs) > 0 && sws.progress[wresp.WatchID] {
				// elide next progress update if sent a key update
				sws.progress[wresp.WatchID] = false
			}
			sws.mu.Unlock()

		case c, ok := <-sws.ctrlStream:
			if !ok {
				return
			}

			if err := sws.gRPCStream.Send(c); err != nil {
				if isClientCtxErr(sws.gRPCStream.Context().Err(), err) {
					sws.lg.Debug("failed to send watch control response to gRPC stream", zap.Error(err))
				} else {
					sws.lg.Warn("failed to send watch control response to gRPC stream", zap.Error(err))
					streamFailures.WithLabelValues("send", "watch").Inc()
				}
				return
			}

			// track id creation
			wid := mvcc.WatchID(c.WatchId)

			verify.Assert(!(c.Canceled && c.Created) || wid == clientv3.InvalidWatchID, "unexpected watchId: %d, wanted: %d, since both 'Canceled' and 'Created' are true", wid, clientv3.InvalidWatchID)

			if c.Canceled && wid != clientv3.InvalidWatchID {
				delete(ids, wid)
				continue
			}
			if c.Created {
				// flush buffered events
				ids[wid] = struct{}{}
				for _, v := range pending[wid] {
					mvcc.ReportEventReceived(len(v.Events))
					if err := sws.gRPCStream.Send(v); err != nil {
						if isClientCtxErr(sws.gRPCStream.Context().Err(), err) {
							sws.lg.Debug("failed to send pending watch response to gRPC stream", zap.Error(err))
						} else {
							sws.lg.Warn("failed to send pending watch response to gRPC stream", zap.Error(err))
							streamFailures.WithLabelValues("send", "watch").Inc()
						}
						return
					}
				}
				delete(pending, wid)
			}

		case <-progressTicker.C:
			sws.mu.Lock()
			for id, ok := range sws.progress {
				if ok {
					sws.watchStream.RequestProgress(id)
				}
				sws.progress[id] = true
			}
			sws.mu.Unlock()

		case <-sws.closec:
			return
		}
	}
}

func IsCreateEvent(e mvccpb.Event) bool {
	return e.Type == mvccpb.PUT && e.Kv.CreateRevision == e.Kv.ModRevision
}

func sendFragments(
	wr *pb.WatchResponse,
	maxRequestBytes int,
	sendFunc func(*pb.WatchResponse) error) error {
	// no need to fragment if total request size is smaller
	// than max request limit or response contains only one event
	if wr.Size() < maxRequestBytes || len(wr.Events) < 2 {
		return sendFunc(wr)
	}

	ow := *wr
	ow.Events = make([]*mvccpb.Event, 0)
	ow.Fragment = true

	var idx int
	for {
		cur := ow
		for _, ev := range wr.Events[idx:] {
			cur.Events = append(cur.Events, ev)
			if len(cur.Events) > 1 && cur.Size() >= maxRequestBytes {
				cur.Events = cur.Events[:len(cur.Events)-1]
				break
			}
			idx++
		}
		if idx == len(wr.Events) {
			// last response has no more fragment
			cur.Fragment = false
		}
		if err := sendFunc(&cur); err != nil {
			return err
		}
		if !cur.Fragment {
			break
		}
	}
	return nil
}

func (sws *serverWatchStream) close() {
	sws.watchStream.Close()
	close(sws.closec)
	sws.wg.Wait()
}

func (sws *serverWatchStream) newResponseHeader(rev int64) *pb.ResponseHeader {
	return &pb.ResponseHeader{
		ClusterId: uint64(sws.clusterID),
		MemberId:  uint64(sws.memberID),
		Revision:  rev,
		RaftTerm:  sws.sg.Term(),
	}
}

func filterNoDelete(e mvccpb.Event) bool {
	return e.Type == mvccpb.DELETE
}

func filterNoPut(e mvccpb.Event) bool {
	return e.Type == mvccpb.PUT
}

// FiltersFromRequest returns "mvcc.FilterFunc" from a given watch create request.
func FiltersFromRequest(creq *pb.WatchCreateRequest) []mvcc.FilterFunc {
	filters := make([]mvcc.FilterFunc, 0, len(creq.Filters))
	for _, ft := range creq.Filters {
		switch ft {
		case pb.WatchCreateRequest_NOPUT:
			filters = append(filters, filterNoPut)
		case pb.WatchCreateRequest_NODELETE:
			filters = append(filters, filterNoDelete)
		default:
		}
	}
	return filters
}
