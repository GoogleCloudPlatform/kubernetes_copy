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

package clientv3

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	v3rpc "go.etcd.io/etcd/api/v3/v3rpc/rpctypes"
)

const (
	EventTypeDelete = mvccpb.DELETE
	EventTypePut    = mvccpb.PUT

	closeSendErrTimeout = 250 * time.Millisecond

	// AutoWatchID is the watcher ID passed in WatchStream.Watch when no
	// user-provided ID is available. If pass, an ID will automatically be assigned.
	AutoWatchID = 0

	// InvalidWatchID represents an invalid watch ID and prevents duplication with an existing watch.
	InvalidWatchID = -1
)

type Event mvccpb.Event

type WatchChan <-chan WatchResponse

type Watcher interface {
	// Watch watches on a key or prefix. The watched events will be returned
	// through the returned channel. If revisions waiting to be sent over the
	// watch are compacted, then the watch will be canceled by the server, the
	// client will post a compacted error watch response, and the channel will close.
	// If the requested revision is 0 or unspecified, the returned channel will
	// return watch events that happen after the server receives the watch request.
	// If the context "ctx" is canceled or timed out, returned "WatchChan" is closed,
	// and "WatchResponse" from this closed channel has zero events and nil "Err()".
	// The context "ctx" MUST be canceled, as soon as watcher is no longer being used,
	// to release the associated resources.
	//
	// If the context is "context.Background/TODO", returned "WatchChan" will
	// not be closed and block until event is triggered, except when server
	// returns a non-recoverable error (e.g. ErrCompacted).
	// For example, when context passed with "WithRequireLeader" and the
	// connected server has no leader (e.g. due to network partition),
	// error "etcdserver: no leader" (ErrNoLeader) will be returned,
	// and then "WatchChan" is closed with non-nil "Err()".
	// In order to prevent a watch stream being stuck in a partitioned node,
	// make sure to wrap context with "WithRequireLeader".
	//
	// Otherwise, as long as the context has not been canceled or timed out,
	// watch will retry on other recoverable errors forever until reconnected.
	//
	// TODO: explicitly set context error in the last "WatchResponse" message and close channel?
	// Currently, client contexts are overwritten with "valCtx" that never closes.
	// TODO(v3.4): configure watch retry policy, limit maximum retry number
	// (see https://github.com/etcd-io/etcd/issues/8980)
	Watch(ctx context.Context, key string, opts ...OpOption) WatchChan

	// RequestProgress requests a progress notify response be sent in all watch channels.
	RequestProgress(ctx context.Context) error

	// Close closes the watcher and cancels all watch requests.
	Close() error
}

type WatchResponse struct {
	Header pb.ResponseHeader
	Events []*Event

	// CompactRevision is the minimum revision the watcher may receive.
	CompactRevision int64

	// Canceled is used to indicate watch failure.
	// If the watch failed and the stream was about to close, before the channel is closed,
	// the channel sends a final response that has Canceled set to true with a non-nil Err().
	Canceled bool

	// Created is used to indicate the creation of the watcher.
	Created bool

	closeErr error

	// cancelReason is a reason of canceling watch
	cancelReason string
}

// IsCreate returns true if the event tells that the key is newly created.
func (e *Event) IsCreate() bool {
	return e.Type == EventTypePut && e.Kv.CreateRevision == e.Kv.ModRevision
}

// IsModify returns true if the event tells that a new value is put on existing key.
func (e *Event) IsModify() bool {
	return e.Type == EventTypePut && e.Kv.CreateRevision != e.Kv.ModRevision
}

// Err is the error value if this WatchResponse holds an error.
func (wr *WatchResponse) Err() error {
	switch {
	case wr.closeErr != nil:
		return v3rpc.Error(wr.closeErr)
	case wr.CompactRevision != 0:
		return v3rpc.ErrCompacted
	case wr.Canceled:
		if len(wr.cancelReason) != 0 {
			return v3rpc.Error(status.Error(codes.FailedPrecondition, wr.cancelReason))
		}
		return v3rpc.ErrFutureRev
	}
	return nil
}

// IsProgressNotify returns true if the WatchResponse is progress notification.
func (wr *WatchResponse) IsProgressNotify() bool {
	return len(wr.Events) == 0 && !wr.Canceled && !wr.Created && wr.CompactRevision == 0 && wr.Header.Revision != 0
}

// watcher implements the Watcher interface
type watcher struct {
	remote   pb.WatchClient
	callOpts []grpc.CallOption

	// mu protects the grpc streams map
	mu sync.Mutex

	// streams holds all the active grpc streams keyed by ctx value.
	streams map[string]*watchGRPCStream
	lg      *zap.Logger
}

// watchGRPCStream tracks all watch resources attached to a single grpc stream.
type watchGRPCStream struct {
	owner    *watcher
	remote   pb.WatchClient
	callOpts []grpc.CallOption

	// ctx controls internal remote.Watch requests
	ctx context.Context
	// ctxKey is the key used when looking up this stream's context
	ctxKey string
	cancel context.CancelFunc

	// substreams holds all active watchers on this grpc stream
	substreams map[int64]*watcherStream
	// resuming holds all resuming watchers on this grpc stream
	resuming []*watcherStream

	// reqc sends a watch request from Watch() to the main goroutine
	reqc chan watchStreamRequest
	// respc receives data from the watch client
	respc chan *pb.WatchResponse
	// donec closes to broadcast shutdown
	donec chan struct{}
	// errc transmits errors from grpc Recv to the watch stream reconnect logic
	errc chan error
	// closingc gets the watcherStream of closing watchers
	closingc chan *watcherStream
	// wg is Done when all substream goroutines have exited
	wg sync.WaitGroup

	// resumec closes to signal that all substreams should begin resuming
	resumec chan struct{}
	// closeErr is the error that closed the watch stream
	closeErr error

	lg *zap.Logger
}

// watchStreamRequest is a union of the supported watch request operation types
type watchStreamRequest interface {
	toPB() *pb.WatchRequest
}

// watchRequest is issued by the subscriber to start a new watcher
type watchRequest struct {
	ctx context.Context
	key string
	end string
	rev int64

	// send created notification event if this field is true
	createdNotify bool
	// progressNotify is for progress updates
	progressNotify bool
	// fragmentation should be disabled by default
	// if true, split watch events when total exceeds
	// "--max-request-bytes" flag value + 512-byte
	fragment bool

	// filters is the list of events to filter out
	filters []pb.WatchCreateRequest_FilterType
	// get the previous key-value pair before the event happens
	prevKV bool
	// retc receives a chan WatchResponse once the watcher is established
	retc chan chan WatchResponse
}

// progressRequest is issued by the subscriber to request watch progress
type progressRequest struct{}

// watcherStream represents a registered watcher
type watcherStream struct {
	// initReq is the request that initiated this request
	initReq watchRequest

	// outc publishes watch responses to subscriber
	outc chan WatchResponse
	// recvc buffers watch responses before publishing
	recvc chan *WatchResponse
	// donec closes when the watcherStream goroutine stops.
	donec chan struct{}
	// closing is set to true when stream should be scheduled to shutdown.
	closing bool
	// id is the registered watch id on the grpc stream
	id int64

	// buf holds all events received from etcd but not yet consumed by the client
	buf []*WatchResponse
}

func NewWatcher(c *Client) Watcher {
	return NewWatchFromWatchClient(pb.NewWatchClient(c.conn), c)
}

func NewWatchFromWatchClient(wc pb.WatchClient, c *Client) Watcher {
	w := &watcher{
		remote:  wc,
		streams: make(map[string]*watchGRPCStream),
	}
	if c != nil {
		w.callOpts = c.callOpts
		w.lg = c.lg
	}
	return w
}

// never closes
var (
	valCtxCh = make(chan struct{})
	zeroTime = time.Unix(0, 0)
)

// ctx with only the values; never Done
type valCtx struct{ context.Context }

func (vc *valCtx) Deadline() (time.Time, bool) { return zeroTime, false }
func (vc *valCtx) Done() <-chan struct{}       { return valCtxCh }
func (vc *valCtx) Err() error                  { return nil }

func (w *watcher) newWatcherGRPCStream(inctx context.Context) *watchGRPCStream {
	ctx, cancel := context.WithCancel(&valCtx{inctx})
	wgs := &watchGRPCStream{
		owner:      w,
		remote:     w.remote,
		callOpts:   w.callOpts,
		ctx:        ctx,
		ctxKey:     streamKeyFromCtx(inctx),
		cancel:     cancel,
		substreams: make(map[int64]*watcherStream),
		respc:      make(chan *pb.WatchResponse),
		reqc:       make(chan watchStreamRequest),
		donec:      make(chan struct{}),
		errc:       make(chan error, 1),
		closingc:   make(chan *watcherStream),
		resumec:    make(chan struct{}),
		lg:         w.lg,
	}
	go wgs.run()
	return wgs
}

// Watch posts a watch request to run() and waits for a new watcher channel
func (w *watcher) Watch(ctx context.Context, key string, opts ...OpOption) WatchChan {
	ow := opWatch(key, opts...)

	var filters []pb.WatchCreateRequest_FilterType
	if ow.filterPut {
		filters = append(filters, pb.WatchCreateRequest_NOPUT)
	}
	if ow.filterDelete {
		filters = append(filters, pb.WatchCreateRequest_NODELETE)
	}

	wr := &watchRequest{
		ctx:            ctx,
		createdNotify:  ow.createdNotify,
		key:            string(ow.key),
		end:            string(ow.end),
		rev:            ow.rev,
		progressNotify: ow.progressNotify,
		fragment:       ow.fragment,
		filters:        filters,
		prevKV:         ow.prevKV,
		retc:           make(chan chan WatchResponse, 1),
	}

	ok := false
	ctxKey := streamKeyFromCtx(ctx)

	var closeCh chan WatchResponse
	for {
		// find or allocate appropriate grpc watch stream
		w.mu.Lock()
		if w.streams == nil {
			// closed
			w.mu.Unlock()
			ch := make(chan WatchResponse)
			close(ch)
			return ch
		}
		wgs := w.streams[ctxKey]
		if wgs == nil {
			wgs = w.newWatcherGRPCStream(ctx)
			w.streams[ctxKey] = wgs
		}
		donec := wgs.donec
		reqc := wgs.reqc
		w.mu.Unlock()

		// couldn't create channel; return closed channel
		if closeCh == nil {
			closeCh = make(chan WatchResponse, 1)
		}

		// submit request
		select {
		case reqc <- wr:
			ok = true
		case <-wr.ctx.Done():
			ok = false
		case <-donec:
			ok = false
			if wgs.closeErr != nil {
				closeCh <- WatchResponse{Canceled: true, closeErr: wgs.closeErr}
				break
			}
			// retry; may have dropped stream from no ctxs
			continue
		}

		// receive channel
		if ok {
			select {
			case ret := <-wr.retc:
				return ret
			case <-ctx.Done():
			case <-donec:
				if wgs.closeErr != nil {
					closeCh <- WatchResponse{Canceled: true, closeErr: wgs.closeErr}
					break
				}
				// retry; may have dropped stream from no ctxs
				continue
			}
		}
		break
	}

	close(closeCh)
	return closeCh
}

func (w *watcher) Close() (err error) {
	w.mu.Lock()
	streams := w.streams
	w.streams = nil
	w.mu.Unlock()
	for _, wgs := range streams {
		if werr := wgs.close(); werr != nil {
			err = werr
		}
	}
	// Consider context.Canceled as a successful close
	if errors.Is(err, context.Canceled) {
		err = nil
	}
	return err
}

// RequestProgress requests a progress notify response be sent in all watch channels.
func (w *watcher) RequestProgress(ctx context.Context) (err error) {
	ctxKey := streamKeyFromCtx(ctx)

	w.mu.Lock()
	if w.streams == nil {
		w.mu.Unlock()
		return errors.New("no stream found for context")
	}
	wgs := w.streams[ctxKey]
	if wgs == nil {
		wgs = w.newWatcherGRPCStream(ctx)
		w.streams[ctxKey] = wgs
	}
	donec := wgs.donec
	reqc := wgs.reqc
	w.mu.Unlock()

	pr := &progressRequest{}

	select {
	case reqc <- pr:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-donec:
		if wgs.closeErr != nil {
			return wgs.closeErr
		}
		// retry; may have dropped stream from no ctxs
		return w.RequestProgress(ctx)
	}
}

func (w *watchGRPCStream) close() (err error) {
	w.cancel()
	<-w.donec
	select {
	case err = <-w.errc:
	default:
	}
	return ContextError(w.ctx, err)
}

func (w *watcher) closeStream(wgs *watchGRPCStream) {
	w.mu.Lock()
	close(wgs.donec)
	wgs.cancel()
	if w.streams != nil {
		delete(w.streams, wgs.ctxKey)
	}
	w.mu.Unlock()
}

func (w *watchGRPCStream) addSubstream(resp *pb.WatchResponse, ws *watcherStream) {
	// check watch ID for backward compatibility (<= v3.3)
	if resp.WatchId == InvalidWatchID || (resp.Canceled && resp.CancelReason != "") {
		w.closeErr = v3rpc.Error(errors.New(resp.CancelReason))
		// failed; no channel
		close(ws.recvc)
		return
	}
	ws.id = resp.WatchId
	w.substreams[ws.id] = ws
}

func (w *watchGRPCStream) sendCloseSubstream(ws *watcherStream, resp *WatchResponse) {
	select {
	case ws.outc <- *resp:
	case <-ws.initReq.ctx.Done():
	case <-time.After(closeSendErrTimeout):
	}
	close(ws.outc)
}

func (w *watchGRPCStream) closeSubstream(ws *watcherStream) {
	// send channel response in case stream was never established
	select {
	case ws.initReq.retc <- ws.outc:
	default:
	}
	// close subscriber's channel
	if closeErr := w.closeErr; closeErr != nil && ws.initReq.ctx.Err() == nil {
		go w.sendCloseSubstream(ws, &WatchResponse{Canceled: true, closeErr: w.closeErr})
	} else if ws.outc != nil {
		close(ws.outc)
	}
	if ws.id != InvalidWatchID {
		delete(w.substreams, ws.id)
		return
	}
	for i := range w.resuming {
		if w.resuming[i] == ws {
			w.resuming[i] = nil
			return
		}
	}
}

// run is the root of the goroutines for managing a watcher client
func (w *watchGRPCStream) run() {
	var wc pb.Watch_WatchClient
	var closeErr error

	// substreams marked to close but goroutine still running; needed for
	// avoiding double-closing recvc on grpc stream teardown
	closing := make(map[*watcherStream]struct{})

	defer func() {
		w.closeErr = closeErr
		// shutdown substreams and resuming substreams
		for _, ws := range w.substreams {
			if _, ok := closing[ws]; !ok {
				close(ws.recvc)
				closing[ws] = struct{}{}
			}
		}
		for _, ws := range w.resuming {
			if _, ok := closing[ws]; ws != nil && !ok {
				close(ws.recvc)
				closing[ws] = struct{}{}
			}
		}
		w.joinSubstreams()
		for range closing {
			w.closeSubstream(<-w.closingc)
		}
		w.wg.Wait()
		w.owner.closeStream(w)
	}()

	// start a stream with the etcd grpc server
	if wc, closeErr = w.newWatchClient(); closeErr != nil {
		return
	}

	cancelSet := make(map[int64]struct{})

	var cur *pb.WatchResponse
	backoff := time.Millisecond
	for {
		select {
		// Watch() requested
		case req := <-w.reqc:
			switch wreq := req.(type) {
			case *watchRequest:
				outc := make(chan WatchResponse, 1)
				// TODO: pass custom watch ID?
				ws := &watcherStream{
					initReq: *wreq,
					id:      InvalidWatchID,
					outc:    outc,
					// unbuffered so resumes won't cause repeat events
					recvc: make(chan *WatchResponse),
				}

				ws.donec = make(chan struct{})
				w.wg.Add(1)
				go w.serveSubstream(ws, w.resumec)

				// queue up for watcher creation/resume
				w.resuming = append(w.resuming, ws)
				if len(w.resuming) == 1 {
					// head of resume queue, can register a new watcher
					if err := wc.Send(ws.initReq.toPB()); err != nil {
						w.lg.Debug("error when sending request", zap.Error(err))
					}
				}
			case *progressRequest:
				if err := wc.Send(wreq.toPB()); err != nil {
					w.lg.Debug("error when sending request", zap.Error(err))
				}
			}

		// new events from the watch client
		case pbresp := <-w.respc:
			if cur == nil || pbresp.Created || pbresp.Canceled {
				cur = pbresp
			} else if cur != nil && cur.WatchId == pbresp.WatchId {
				// merge new events
				cur.Events = append(cur.Events, pbresp.Events...)
				// update "Fragment" field; last response with "Fragment" == false
				cur.Fragment = pbresp.Fragment
			}

			switch {
			case pbresp.Created:
				// response to head of queue creation
				if len(w.resuming) != 0 {
					if ws := w.resuming[0]; ws != nil {
						w.addSubstream(pbresp, ws)
						w.dispatchEvent(pbresp)
						w.resuming[0] = nil
					}
				}

				if ws := w.nextResume(); ws != nil {
					if err := wc.Send(ws.initReq.toPB()); err != nil {
						w.lg.Debug("error when sending request", zap.Error(err))
					}
				}

				// reset for next iteration
				cur = nil

			case pbresp.Canceled && pbresp.CompactRevision == 0:
				delete(cancelSet, pbresp.WatchId)
				if ws, ok := w.substreams[pbresp.WatchId]; ok {
					// signal to stream goroutine to update closingc
					close(ws.recvc)
					closing[ws] = struct{}{}
				}

				// reset for next iteration
				cur = nil

			case cur.Fragment:
				// watch response events are still fragmented
				// continue to fetch next fragmented event arrival
				continue

			default:
				// dispatch to appropriate watch stream
				ok := w.dispatchEvent(cur)

				// reset for next iteration
				cur = nil

				if ok {
					break
				}

				// watch response on unexpected watch id; cancel id
				if _, ok := cancelSet[pbresp.WatchId]; ok {
					break
				}

				cancelSet[pbresp.WatchId] = struct{}{}
				cr := &pb.WatchRequest_CancelRequest{
					CancelRequest: &pb.WatchCancelRequest{
						WatchId: pbresp.WatchId,
					},
				}
				req := &pb.WatchRequest{RequestUnion: cr}
				w.lg.Debug("sending watch cancel request for failed dispatch", zap.Int64("watch-id", pbresp.WatchId))
				if err := wc.Send(req); err != nil {
					w.lg.Debug("failed to send watch cancel request", zap.Int64("watch-id", pbresp.WatchId), zap.Error(err))
				}
			}

		// watch client failed on Recv; spawn another if possible
		case err := <-w.errc:
			if isHaltErr(w.ctx, err) || errors.Is(ContextError(w.ctx, err), v3rpc.ErrNoLeader) {
				closeErr = err
				return
			}
			backoff = w.backoffIfUnavailable(backoff, err)
			if wc, closeErr = w.newWatchClient(); closeErr != nil {
				return
			}
			if ws := w.nextResume(); ws != nil {
				if err := wc.Send(ws.initReq.toPB()); err != nil {
					w.lg.Debug("error when sending request", zap.Error(err))
				}
			}
			cancelSet = make(map[int64]struct{})

		case <-w.ctx.Done():
			return

		case ws := <-w.closingc:
			w.closeSubstream(ws)
			delete(closing, ws)
			// no more watchers on this stream, shutdown, skip cancellation
			if len(w.substreams)+len(w.resuming) == 0 {
				return
			}
			if ws.id != InvalidWatchID {
				// client is closing an established watch; close it on the server proactively instead of waiting
				// to close when the next message arrives
				cancelSet[ws.id] = struct{}{}
				cr := &pb.WatchRequest_CancelRequest{
					CancelRequest: &pb.WatchCancelRequest{
						WatchId: ws.id,
					},
				}
				req := &pb.WatchRequest{RequestUnion: cr}
				w.lg.Debug("sending watch cancel request for closed watcher", zap.Int64("watch-id", ws.id))
				if err := wc.Send(req); err != nil {
					w.lg.Debug("failed to send watch cancel request", zap.Int64("watch-id", ws.id), zap.Error(err))
				}
			}
		}
	}
}

// nextResume chooses the next resuming to register with the grpc stream. Abandoned
// streams are marked as nil in the queue since the head must wait for its inflight registration.
func (w *watchGRPCStream) nextResume() *watcherStream {
	for len(w.resuming) != 0 {
		if w.resuming[0] != nil {
			return w.resuming[0]
		}
		w.resuming = w.resuming[1:len(w.resuming)]
	}
	return nil
}

// dispatchEvent sends a WatchResponse to the appropriate watcher stream
func (w *watchGRPCStream) dispatchEvent(pbresp *pb.WatchResponse) bool {
	events := make([]*Event, len(pbresp.Events))
	for i, ev := range pbresp.Events {
		events[i] = (*Event)(ev)
	}
	// TODO: return watch ID?
	wr := &WatchResponse{
		Header:          *pbresp.Header,
		Events:          events,
		CompactRevision: pbresp.CompactRevision,
		Created:         pbresp.Created,
		Canceled:        pbresp.Canceled,
		cancelReason:    pbresp.CancelReason,
	}

	// watch IDs are zero indexed, so request notify watch responses are assigned a watch ID of InvalidWatchID to
	// indicate they should be broadcast.
	if wr.IsProgressNotify() && pbresp.WatchId == InvalidWatchID {
		return w.broadcastResponse(wr)
	}

	return w.unicastResponse(wr, pbresp.WatchId)
}

// broadcastResponse send a watch response to all watch substreams.
func (w *watchGRPCStream) broadcastResponse(wr *WatchResponse) bool {
	for _, ws := range w.substreams {
		select {
		case ws.recvc <- wr:
		case <-ws.donec:
		}
	}
	return true
}

// unicastResponse sends a watch response to a specific watch substream.
func (w *watchGRPCStream) unicastResponse(wr *WatchResponse, watchID int64) bool {
	ws, ok := w.substreams[watchID]
	if !ok {
		return false
	}
	select {
	case ws.recvc <- wr:
	case <-ws.donec:
		return false
	}
	return true
}

// serveWatchClient forwards messages from the grpc stream to run()
func (w *watchGRPCStream) serveWatchClient(wc pb.Watch_WatchClient) {
	for {
		resp, err := wc.Recv()
		if err != nil {
			select {
			case w.errc <- err:
			case <-w.donec:
			}
			return
		}
		select {
		case w.respc <- resp:
		case <-w.donec:
			return
		}
	}
}

// serveSubstream forwards watch responses from run() to the subscriber
func (w *watchGRPCStream) serveSubstream(ws *watcherStream, resumec chan struct{}) {
	if ws.closing {
		panic("created substream goroutine but substream is closing")
	}

	// nextRev is the minimum expected next revision
	nextRev := ws.initReq.rev
	resuming := false
	defer func() {
		if !resuming {
			ws.closing = true
		}
		close(ws.donec)
		if !resuming {
			w.closingc <- ws
		}
		w.wg.Done()
	}()

	emptyWr := &WatchResponse{}
	for {
		curWr := emptyWr
		outc := ws.outc

		if len(ws.buf) > 0 {
			curWr = ws.buf[0]
		} else {
			outc = nil
		}
		select {
		case outc <- *curWr:
			if ws.buf[0].Err() != nil {
				return
			}
			ws.buf[0] = nil
			ws.buf = ws.buf[1:]
		case wr, ok := <-ws.recvc:
			if !ok {
				// shutdown from closeSubstream
				return
			}

			if wr.Created {
				if ws.initReq.retc != nil {
					ws.initReq.retc <- ws.outc
					// to prevent next write from taking the slot in buffered channel
					// and posting duplicate create events
					ws.initReq.retc = nil

					// send first creation event only if requested
					if ws.initReq.createdNotify {
						ws.outc <- *wr
					}
					// once the watch channel is returned, a current revision
					// watch must resume at the store revision. This is necessary
					// for the following case to work as expected:
					//	wch := m1.Watch("a")
					//	m2.Put("a", "b")
					//	<-wch
					// If the revision is only bound on the first observed event,
					// if wch is disconnected before the Put is issued, then reconnects
					// after it is committed, it'll miss the Put.
					if ws.initReq.rev == 0 {
						nextRev = wr.Header.Revision
					}
				}
			} else {
				// current progress of watch; <= store revision
				nextRev = wr.Header.Revision + 1
			}

			if len(wr.Events) > 0 {
				nextRev = wr.Events[len(wr.Events)-1].Kv.ModRevision + 1
			}

			ws.initReq.rev = nextRev

			// created event is already sent above,
			// watcher should not post duplicate events
			if wr.Created {
				continue
			}

			// TODO pause channel if buffer gets too large
			ws.buf = append(ws.buf, wr)
		case <-w.ctx.Done():
			return
		case <-ws.initReq.ctx.Done():
			return
		case <-resumec:
			resuming = true
			return
		}
	}
	// lazily send cancel message if events on missing id
}

func (w *watchGRPCStream) newWatchClient() (pb.Watch_WatchClient, error) {
	// mark all substreams as resuming
	close(w.resumec)
	w.resumec = make(chan struct{})
	w.joinSubstreams()
	for _, ws := range w.substreams {
		ws.id = InvalidWatchID
		w.resuming = append(w.resuming, ws)
	}
	// strip out nils, if any
	var resuming []*watcherStream
	for _, ws := range w.resuming {
		if ws != nil {
			resuming = append(resuming, ws)
		}
	}
	w.resuming = resuming
	w.substreams = make(map[int64]*watcherStream)

	// connect to grpc stream while accepting watcher cancelation
	stopc := make(chan struct{})
	donec := w.waitCancelSubstreams(stopc)
	wc, err := w.openWatchClient()
	close(stopc)
	<-donec

	// serve all non-closing streams, even if there's a client error
	// so that the teardown path can shutdown the streams as expected.
	for _, ws := range w.resuming {
		if ws.closing {
			continue
		}
		ws.donec = make(chan struct{})
		w.wg.Add(1)
		go w.serveSubstream(ws, w.resumec)
	}

	if err != nil {
		return nil, v3rpc.Error(err)
	}

	// receive data from new grpc stream
	go w.serveWatchClient(wc)
	return wc, nil
}

func (w *watchGRPCStream) waitCancelSubstreams(stopc <-chan struct{}) <-chan struct{} {
	var wg sync.WaitGroup
	wg.Add(len(w.resuming))
	donec := make(chan struct{})
	for i := range w.resuming {
		go func(ws *watcherStream) {
			defer wg.Done()
			if ws.closing {
				if ws.initReq.ctx.Err() != nil && ws.outc != nil {
					close(ws.outc)
					ws.outc = nil
				}
				return
			}
			select {
			case <-ws.initReq.ctx.Done():
				// closed ws will be removed from resuming
				ws.closing = true
				close(ws.outc)
				ws.outc = nil
				w.wg.Add(1)
				go func() {
					defer w.wg.Done()
					w.closingc <- ws
				}()
			case <-stopc:
			}
		}(w.resuming[i])
	}
	go func() {
		defer close(donec)
		wg.Wait()
	}()
	return donec
}

// joinSubstreams waits for all substream goroutines to complete.
func (w *watchGRPCStream) joinSubstreams() {
	for _, ws := range w.substreams {
		<-ws.donec
	}
	for _, ws := range w.resuming {
		if ws != nil {
			<-ws.donec
		}
	}
}

var maxBackoff = 100 * time.Millisecond

func (w *watchGRPCStream) backoffIfUnavailable(backoff time.Duration, err error) time.Duration {
	if isUnavailableErr(w.ctx, err) {
		// retry, but backoff
		if backoff < maxBackoff {
			// 25% backoff factor
			backoff = backoff + backoff/4
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
		time.Sleep(backoff)
	}
	return backoff
}

// openWatchClient retries opening a watch client until success or halt.
// manually retry in case "ws==nil && err==nil"
// TODO: remove FailFast=false
func (w *watchGRPCStream) openWatchClient() (ws pb.Watch_WatchClient, err error) {
	backoff := time.Millisecond
	for {
		select {
		case <-w.ctx.Done():
			if err == nil {
				return nil, w.ctx.Err()
			}
			return nil, err
		default:
		}
		if ws, err = w.remote.Watch(w.ctx, w.callOpts...); ws != nil && err == nil {
			break
		}
		if isHaltErr(w.ctx, err) {
			return nil, v3rpc.Error(err)
		}
		backoff = w.backoffIfUnavailable(backoff, err)
	}
	return ws, nil
}

// toPB converts an internal watch request structure to its protobuf WatchRequest structure.
func (wr *watchRequest) toPB() *pb.WatchRequest {
	req := &pb.WatchCreateRequest{
		StartRevision:  wr.rev,
		Key:            []byte(wr.key),
		RangeEnd:       []byte(wr.end),
		ProgressNotify: wr.progressNotify,
		Filters:        wr.filters,
		PrevKv:         wr.prevKV,
		Fragment:       wr.fragment,
	}
	cr := &pb.WatchRequest_CreateRequest{CreateRequest: req}
	return &pb.WatchRequest{RequestUnion: cr}
}

// toPB converts an internal progress request structure to its protobuf WatchRequest structure.
func (pr *progressRequest) toPB() *pb.WatchRequest {
	req := &pb.WatchProgressRequest{}
	cr := &pb.WatchRequest_ProgressRequest{ProgressRequest: req}
	return &pb.WatchRequest{RequestUnion: cr}
}

func streamKeyFromCtx(ctx context.Context) string {
	if md, ok := metadata.FromOutgoingContext(ctx); ok {
		return fmt.Sprintf("%+v", map[string][]string(md))
	}
	return ""
}
