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
	"sync"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/v3rpc/rpctypes"
)

type (
	LeaseRevokeResponse pb.LeaseRevokeResponse
	LeaseID             int64
)

// LeaseGrantResponse wraps the protobuf message LeaseGrantResponse.
type LeaseGrantResponse struct {
	*pb.ResponseHeader
	ID    LeaseID
	TTL   int64
	Error string
}

// LeaseKeepAliveResponse wraps the protobuf message LeaseKeepAliveResponse.
type LeaseKeepAliveResponse struct {
	*pb.ResponseHeader
	ID  LeaseID
	TTL int64
}

// LeaseTimeToLiveResponse wraps the protobuf message LeaseTimeToLiveResponse.
type LeaseTimeToLiveResponse struct {
	*pb.ResponseHeader
	ID LeaseID `json:"id"`

	// TTL is the remaining TTL in seconds for the lease; the lease will expire in under TTL+1 seconds. Expired lease will return -1.
	TTL int64 `json:"ttl"`

	// GrantedTTL is the initial granted time in seconds upon lease creation/renewal.
	GrantedTTL int64 `json:"granted-ttl"`

	// Keys is the list of keys attached to this lease.
	Keys [][]byte `json:"keys"`
}

// LeaseStatus represents a lease status.
type LeaseStatus struct {
	ID LeaseID `json:"id"`
	// TODO: TTL int64
}

// LeaseLeasesResponse wraps the protobuf message LeaseLeasesResponse.
type LeaseLeasesResponse struct {
	*pb.ResponseHeader
	Leases []LeaseStatus `json:"leases"`
}

const (
	// defaultTTL is the assumed lease TTL used for the first keepalive
	// deadline before the actual TTL is known to the client.
	defaultTTL = 5 * time.Second
	// NoLease is a lease ID for the absence of a lease.
	NoLease LeaseID = 0

	// retryConnWait is how long to wait before retrying request due to an error
	retryConnWait = 500 * time.Millisecond
)

// LeaseResponseChSize is the size of buffer to store unsent lease responses.
// WARNING: DO NOT UPDATE.
// Only for testing purposes.
var LeaseResponseChSize = 16

// ErrKeepAliveHalted is returned if client keep alive loop halts with an unexpected error.
//
// This usually means that automatic lease renewal via KeepAlive is broken, but KeepAliveOnce will still work as expected.
type ErrKeepAliveHalted struct {
	Reason error
}

func (e ErrKeepAliveHalted) Error() string {
	s := "etcdclient: leases keep alive halted"
	if e.Reason != nil {
		s += ": " + e.Reason.Error()
	}
	return s
}

type Lease interface {
	// Grant creates a new lease.
	Grant(ctx context.Context, ttl int64) (*LeaseGrantResponse, error)

	// Revoke revokes the given lease.
	Revoke(ctx context.Context, id LeaseID) (*LeaseRevokeResponse, error)

	// TimeToLive retrieves the lease information of the given lease ID.
	TimeToLive(ctx context.Context, id LeaseID, opts ...LeaseOption) (*LeaseTimeToLiveResponse, error)

	// Leases retrieves all leases.
	Leases(ctx context.Context) (*LeaseLeasesResponse, error)

	// KeepAlive attempts to keep the given lease alive forever. If the keepalive responses posted
	// to the channel are not consumed promptly the channel may become full. When full, the lease
	// client will continue sending keep alive requests to the etcd server, but will drop responses
	// until there is capacity on the channel to send more responses.
	//
	// If client keep alive loop halts with an unexpected error (e.g. "etcdserver: no leader") or
	// canceled by the caller (e.g. context.Canceled), KeepAlive returns a ErrKeepAliveHalted error
	// containing the error reason.
	//
	// The returned "LeaseKeepAliveResponse" channel closes if underlying keep
	// alive stream is interrupted in some way the client cannot handle itself;
	// given context "ctx" is canceled or timed out.
	//
	// TODO(v4.0): post errors to last keep alive message before closing
	// (see https://github.com/etcd-io/etcd/pull/7866)
	KeepAlive(ctx context.Context, id LeaseID) (<-chan *LeaseKeepAliveResponse, error)

	// KeepAliveOnce renews the lease once. The response corresponds to the
	// first message from calling KeepAlive. If the response has a recoverable
	// error, KeepAliveOnce will retry the RPC with a new keep alive message.
	//
	// In most of the cases, Keepalive should be used instead of KeepAliveOnce.
	KeepAliveOnce(ctx context.Context, id LeaseID) (*LeaseKeepAliveResponse, error)

	// Close releases all resources Lease keeps for efficient communication
	// with the etcd server.
	Close() error
}

type lessor struct {
	mu sync.Mutex // guards all fields

	// donec is closed and loopErr is set when recvKeepAliveLoop stops
	donec   chan struct{}
	loopErr error

	remote pb.LeaseClient

	stream       pb.Lease_LeaseKeepAliveClient
	streamCancel context.CancelFunc

	stopCtx    context.Context
	stopCancel context.CancelFunc

	keepAlives map[LeaseID]*keepAlive

	// firstKeepAliveTimeout is the timeout for the first keepalive request
	// before the actual TTL is known to the lease client
	firstKeepAliveTimeout time.Duration

	// firstKeepAliveOnce ensures stream starts after first KeepAlive call.
	firstKeepAliveOnce sync.Once

	callOpts []grpc.CallOption

	lg *zap.Logger
}

// keepAlive multiplexes a keepalive for a lease over multiple channels
type keepAlive struct {
	chs  []chan<- *LeaseKeepAliveResponse
	ctxs []context.Context
	// deadline is the time the keep alive channels close if no response
	deadline time.Time
	// nextKeepAlive is when to send the next keep alive message
	nextKeepAlive time.Time
	// donec is closed on lease revoke, expiration, or cancel.
	donec chan struct{}
}

func NewLease(c *Client) Lease {
	return NewLeaseFromLeaseClient(RetryLeaseClient(c), c, c.cfg.DialTimeout+time.Second)
}

func NewLeaseFromLeaseClient(remote pb.LeaseClient, c *Client, keepAliveTimeout time.Duration) Lease {
	l := &lessor{
		donec:                 make(chan struct{}),
		keepAlives:            make(map[LeaseID]*keepAlive),
		remote:                remote,
		firstKeepAliveTimeout: keepAliveTimeout,
	}
	if l.firstKeepAliveTimeout == time.Second {
		l.firstKeepAliveTimeout = defaultTTL
	}
	if c != nil {
		l.lg = c.lg
		l.callOpts = c.callOpts
	}
	reqLeaderCtx := WithRequireLeader(context.Background())
	l.stopCtx, l.stopCancel = context.WithCancel(reqLeaderCtx)
	return l
}

func (l *lessor) Grant(ctx context.Context, ttl int64) (*LeaseGrantResponse, error) {
	r := &pb.LeaseGrantRequest{TTL: ttl}
	resp, err := l.remote.LeaseGrant(ctx, r, l.callOpts...)
	if err == nil {
		gresp := &LeaseGrantResponse{
			ResponseHeader: resp.GetHeader(),
			ID:             LeaseID(resp.ID),
			TTL:            resp.TTL,
			Error:          resp.Error,
		}
		return gresp, nil
	}
	return nil, ContextError(ctx, err)
}

func (l *lessor) Revoke(ctx context.Context, id LeaseID) (*LeaseRevokeResponse, error) {
	r := &pb.LeaseRevokeRequest{ID: int64(id)}
	resp, err := l.remote.LeaseRevoke(ctx, r, l.callOpts...)
	if err == nil {
		return (*LeaseRevokeResponse)(resp), nil
	}
	return nil, ContextError(ctx, err)
}

func (l *lessor) TimeToLive(ctx context.Context, id LeaseID, opts ...LeaseOption) (*LeaseTimeToLiveResponse, error) {
	r := toLeaseTimeToLiveRequest(id, opts...)
	resp, err := l.remote.LeaseTimeToLive(ctx, r, l.callOpts...)
	if err != nil {
		return nil, ContextError(ctx, err)
	}
	gresp := &LeaseTimeToLiveResponse{
		ResponseHeader: resp.GetHeader(),
		ID:             LeaseID(resp.ID),
		TTL:            resp.TTL,
		GrantedTTL:     resp.GrantedTTL,
		Keys:           resp.Keys,
	}
	return gresp, nil
}

func (l *lessor) Leases(ctx context.Context) (*LeaseLeasesResponse, error) {
	resp, err := l.remote.LeaseLeases(ctx, &pb.LeaseLeasesRequest{}, l.callOpts...)
	if err == nil {
		leases := make([]LeaseStatus, len(resp.Leases))
		for i := range resp.Leases {
			leases[i] = LeaseStatus{ID: LeaseID(resp.Leases[i].ID)}
		}
		return &LeaseLeasesResponse{ResponseHeader: resp.GetHeader(), Leases: leases}, nil
	}
	return nil, ContextError(ctx, err)
}

func (l *lessor) KeepAlive(ctx context.Context, id LeaseID) (<-chan *LeaseKeepAliveResponse, error) {
	ch := make(chan *LeaseKeepAliveResponse, LeaseResponseChSize)

	l.mu.Lock()
	// ensure that recvKeepAliveLoop is still running
	select {
	case <-l.donec:
		err := l.loopErr
		l.mu.Unlock()
		close(ch)
		return ch, ErrKeepAliveHalted{Reason: err}
	default:
	}
	ka, ok := l.keepAlives[id]
	if !ok {
		// create fresh keep alive
		ka = &keepAlive{
			chs:           []chan<- *LeaseKeepAliveResponse{ch},
			ctxs:          []context.Context{ctx},
			deadline:      time.Now().Add(l.firstKeepAliveTimeout),
			nextKeepAlive: time.Now(),
			donec:         make(chan struct{}),
		}
		l.keepAlives[id] = ka
	} else {
		// add channel and context to existing keep alive
		ka.ctxs = append(ka.ctxs, ctx)
		ka.chs = append(ka.chs, ch)
	}
	l.mu.Unlock()

	if ctx.Done() != nil {
		go l.keepAliveCtxCloser(ctx, id, ka.donec)
	}
	l.firstKeepAliveOnce.Do(func() {
		go l.recvKeepAliveLoop()
		go l.deadlineLoop()
	})

	return ch, nil
}

func (l *lessor) KeepAliveOnce(ctx context.Context, id LeaseID) (*LeaseKeepAliveResponse, error) {
	for {
		resp, err := l.keepAliveOnce(ctx, id)
		if err == nil {
			if resp.TTL <= 0 {
				err = rpctypes.ErrLeaseNotFound
			}
			return resp, err
		}
		if isHaltErr(ctx, err) {
			return nil, ContextError(ctx, err)
		}
	}
}

func (l *lessor) Close() error {
	l.stopCancel()
	// close for synchronous teardown if stream goroutines never launched
	l.firstKeepAliveOnce.Do(func() { close(l.donec) })
	<-l.donec
	return nil
}

func (l *lessor) keepAliveCtxCloser(ctx context.Context, id LeaseID, donec <-chan struct{}) {
	select {
	case <-donec:
		return
	case <-l.donec:
		return
	case <-ctx.Done():
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	ka, ok := l.keepAlives[id]
	if !ok {
		return
	}

	// close channel and remove context if still associated with keep alive
	for i, c := range ka.ctxs {
		if c == ctx {
			close(ka.chs[i])
			ka.ctxs = append(ka.ctxs[:i], ka.ctxs[i+1:]...)
			ka.chs = append(ka.chs[:i], ka.chs[i+1:]...)
			break
		}
	}
	// remove if no one more listeners
	if len(ka.chs) == 0 {
		delete(l.keepAlives, id)
	}
}

// closeRequireLeader scans keepAlives for ctxs that have require leader
// and closes the associated channels.
func (l *lessor) closeRequireLeader() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, ka := range l.keepAlives {
		reqIdxs := 0
		// find all required leader channels, close, mark as nil
		for i, ctx := range ka.ctxs {
			md, ok := metadata.FromOutgoingContext(ctx)
			if !ok {
				continue
			}
			ks := md[rpctypes.MetadataRequireLeaderKey]
			if len(ks) < 1 || ks[0] != rpctypes.MetadataHasLeader {
				continue
			}
			close(ka.chs[i])
			ka.chs[i] = nil
			reqIdxs++
		}
		if reqIdxs == 0 {
			continue
		}
		// remove all channels that required a leader from keepalive
		newChs := make([]chan<- *LeaseKeepAliveResponse, len(ka.chs)-reqIdxs)
		newCtxs := make([]context.Context, len(newChs))
		newIdx := 0
		for i := range ka.chs {
			if ka.chs[i] == nil {
				continue
			}
			newChs[newIdx], newCtxs[newIdx] = ka.chs[i], ka.ctxs[newIdx]
			newIdx++
		}
		ka.chs, ka.ctxs = newChs, newCtxs
	}
}

func (l *lessor) keepAliveOnce(ctx context.Context, id LeaseID) (karesp *LeaseKeepAliveResponse, ferr error) {
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()

	stream, err := l.remote.LeaseKeepAlive(cctx, l.callOpts...)
	if err != nil {
		return nil, ContextError(ctx, err)
	}

	defer func() {
		if cerr := stream.CloseSend(); cerr != nil {
			if ferr == nil {
				ferr = ContextError(ctx, cerr)
			}
			return
		}
	}()

	err = stream.Send(&pb.LeaseKeepAliveRequest{ID: int64(id)})
	if err != nil {
		return nil, ContextError(ctx, err)
	}

	resp, rerr := stream.Recv()
	if rerr != nil {
		return nil, ContextError(ctx, rerr)
	}

	karesp = &LeaseKeepAliveResponse{
		ResponseHeader: resp.GetHeader(),
		ID:             LeaseID(resp.ID),
		TTL:            resp.TTL,
	}
	return karesp, nil
}

func (l *lessor) recvKeepAliveLoop() (gerr error) {
	defer func() {
		l.mu.Lock()
		close(l.donec)
		l.loopErr = gerr
		for _, ka := range l.keepAlives {
			ka.close()
		}
		l.keepAlives = make(map[LeaseID]*keepAlive)
		l.mu.Unlock()
	}()

	for {
		stream, err := l.resetRecv()
		if err != nil {
			l.lg.Warn("error occurred during lease keep alive loop",
				zap.Error(err),
			)
			if canceledByCaller(l.stopCtx, err) {
				return err
			}
		} else {
			for {
				resp, err := stream.Recv()
				if err != nil {
					if canceledByCaller(l.stopCtx, err) {
						return err
					}

					if errors.Is(ContextError(l.stopCtx, err), rpctypes.ErrNoLeader) {
						l.closeRequireLeader()
					}
					break
				}

				l.recvKeepAlive(resp)
			}
		}

		select {
		case <-time.After(retryConnWait):
		case <-l.stopCtx.Done():
			return l.stopCtx.Err()
		}
	}
}

// resetRecv opens a new lease stream and starts sending keep alive requests.
func (l *lessor) resetRecv() (pb.Lease_LeaseKeepAliveClient, error) {
	sctx, cancel := context.WithCancel(l.stopCtx)
	stream, err := l.remote.LeaseKeepAlive(sctx, append(l.callOpts, withMax(0))...)
	if err != nil {
		cancel()
		return nil, err
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.stream != nil && l.streamCancel != nil {
		l.streamCancel()
	}

	l.streamCancel = cancel
	l.stream = stream

	go l.sendKeepAliveLoop(stream)
	return stream, nil
}

// recvKeepAlive updates a lease based on its LeaseKeepAliveResponse
func (l *lessor) recvKeepAlive(resp *pb.LeaseKeepAliveResponse) {
	karesp := &LeaseKeepAliveResponse{
		ResponseHeader: resp.GetHeader(),
		ID:             LeaseID(resp.ID),
		TTL:            resp.TTL,
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	ka, ok := l.keepAlives[karesp.ID]
	if !ok {
		return
	}

	if karesp.TTL <= 0 {
		// lease expired; close all keep alive channels
		delete(l.keepAlives, karesp.ID)
		ka.close()
		return
	}

	// send update to all channels
	nextKeepAlive := time.Now().Add((time.Duration(karesp.TTL) * time.Second) / 3.0)
	ka.deadline = time.Now().Add(time.Duration(karesp.TTL) * time.Second)
	for _, ch := range ka.chs {
		select {
		case ch <- karesp:
		default:
			if l.lg != nil {
				l.lg.Warn("lease keepalive response queue is full; dropping response send",
					zap.Int("queue-size", len(ch)),
					zap.Int("queue-capacity", cap(ch)),
				)
			}
		}
		// still advance in order to rate-limit keep-alive sends
		ka.nextKeepAlive = nextKeepAlive
	}
}

// deadlineLoop reaps any keep alive channels that have not received a response
// within the lease TTL
func (l *lessor) deadlineLoop() {
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for {
		timer.Reset(time.Second)
		select {
		case <-timer.C:
		case <-l.donec:
			return
		}
		now := time.Now()
		l.mu.Lock()
		for id, ka := range l.keepAlives {
			if ka.deadline.Before(now) {
				// waited too long for response; lease may be expired
				ka.close()
				delete(l.keepAlives, id)
			}
		}
		l.mu.Unlock()
	}
}

// sendKeepAliveLoop sends keep alive requests for the lifetime of the given stream.
func (l *lessor) sendKeepAliveLoop(stream pb.Lease_LeaseKeepAliveClient) {
	for {
		var tosend []LeaseID

		now := time.Now()
		l.mu.Lock()
		for id, ka := range l.keepAlives {
			if ka.nextKeepAlive.Before(now) {
				tosend = append(tosend, id)
			}
		}
		l.mu.Unlock()

		for _, id := range tosend {
			r := &pb.LeaseKeepAliveRequest{ID: int64(id)}
			if err := stream.Send(r); err != nil {
				l.lg.Warn("error occurred during lease keep alive request sending",
					zap.Error(err),
				)
				return
			}
		}

		select {
		case <-time.After(retryConnWait):
		case <-stream.Context().Done():
			return
		case <-l.donec:
			return
		case <-l.stopCtx.Done():
			return
		}
	}
}

func (ka *keepAlive) close() {
	close(ka.donec)
	for _, ch := range ka.chs {
		close(ch)
	}
}
