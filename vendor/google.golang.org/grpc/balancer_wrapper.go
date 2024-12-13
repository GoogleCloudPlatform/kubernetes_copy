/*
 *
 * Copyright 2017 gRPC authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 */

package grpc

import (
	"context"
	"fmt"
	"sync"

	"google.golang.org/grpc/balancer"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/internal"
	"google.golang.org/grpc/internal/balancer/gracefulswitch"
	"google.golang.org/grpc/internal/channelz"
	"google.golang.org/grpc/internal/grpcsync"
	"google.golang.org/grpc/resolver"
	"google.golang.org/grpc/status"
)

var setConnectedAddress = internal.SetConnectedAddress.(func(*balancer.SubConnState, resolver.Address))

// ccBalancerWrapper sits between the ClientConn and the Balancer.
//
// ccBalancerWrapper implements methods corresponding to the ones on the
// balancer.Balancer interface. The ClientConn is free to call these methods
// concurrently and the ccBalancerWrapper ensures that calls from the ClientConn
// to the Balancer happen in order by performing them in the serializer, without
// any mutexes held.
//
// ccBalancerWrapper also implements the balancer.ClientConn interface and is
// passed to the Balancer implementations. It invokes unexported methods on the
// ClientConn to handle these calls from the Balancer.
//
// It uses the gracefulswitch.Balancer internally to ensure that balancer
// switches happen in a graceful manner.
type ccBalancerWrapper struct {
	// The following fields are initialized when the wrapper is created and are
	// read-only afterwards, and therefore can be accessed without a mutex.
	cc               *ClientConn
	opts             balancer.BuildOptions
	serializer       *grpcsync.CallbackSerializer
	serializerCancel context.CancelFunc

	// The following fields are only accessed within the serializer or during
	// initialization.
	curBalancerName string
	balancer        *gracefulswitch.Balancer

	// The following field is protected by mu.  Caller must take cc.mu before
	// taking mu.
	mu     sync.Mutex
	closed bool
}

// newCCBalancerWrapper creates a new balancer wrapper in idle state. The
// underlying balancer is not created until the updateClientConnState() method
// is invoked.
func newCCBalancerWrapper(cc *ClientConn) *ccBalancerWrapper {
	ctx, cancel := context.WithCancel(cc.ctx)
	ccb := &ccBalancerWrapper{
		cc: cc,
		opts: balancer.BuildOptions{
			DialCreds:       cc.dopts.copts.TransportCredentials,
			CredsBundle:     cc.dopts.copts.CredsBundle,
			Dialer:          cc.dopts.copts.Dialer,
			Authority:       cc.authority,
			CustomUserAgent: cc.dopts.copts.UserAgent,
			ChannelzParent:  cc.channelz,
			Target:          cc.parsedTarget,
			MetricsRecorder: cc.metricsRecorderList,
		},
		serializer:       grpcsync.NewCallbackSerializer(ctx),
		serializerCancel: cancel,
	}
	ccb.balancer = gracefulswitch.NewBalancer(ccb, ccb.opts)
	return ccb
}

// updateClientConnState is invoked by grpc to push a ClientConnState update to
// the underlying balancer.  This is always executed from the serializer, so
// it is safe to call into the balancer here.
func (ccb *ccBalancerWrapper) updateClientConnState(ccs *balancer.ClientConnState) error {
	errCh := make(chan error)
	uccs := func(ctx context.Context) {
		defer close(errCh)
		if ctx.Err() != nil || ccb.balancer == nil {
			return
		}
		name := gracefulswitch.ChildName(ccs.BalancerConfig)
		if ccb.curBalancerName != name {
			ccb.curBalancerName = name
			channelz.Infof(logger, ccb.cc.channelz, "Channel switches to new LB policy %q", name)
		}
		err := ccb.balancer.UpdateClientConnState(*ccs)
		if logger.V(2) && err != nil {
			logger.Infof("error from balancer.UpdateClientConnState: %v", err)
		}
		errCh <- err
	}
	onFailure := func() { close(errCh) }

	// UpdateClientConnState can race with Close, and when the latter wins, the
	// serializer is closed, and the attempt to schedule the callback will fail.
	// It is acceptable to ignore this failure. But since we want to handle the
	// state update in a blocking fashion (when we successfully schedule the
	// callback), we have to use the ScheduleOr method and not the MaybeSchedule
	// method on the serializer.
	ccb.serializer.ScheduleOr(uccs, onFailure)
	return <-errCh
}

// resolverError is invoked by grpc to push a resolver error to the underlying
// balancer.  The call to the balancer is executed from the serializer.
func (ccb *ccBalancerWrapper) resolverError(err error) {
	ccb.serializer.TrySchedule(func(ctx context.Context) {
		if ctx.Err() != nil || ccb.balancer == nil {
			return
		}
		ccb.balancer.ResolverError(err)
	})
}

// close initiates async shutdown of the wrapper.  cc.mu must be held when
// calling this function.  To determine the wrapper has finished shutting down,
// the channel should block on ccb.serializer.Done() without cc.mu held.
func (ccb *ccBalancerWrapper) close() {
	ccb.mu.Lock()
	ccb.closed = true
	ccb.mu.Unlock()
	channelz.Info(logger, ccb.cc.channelz, "ccBalancerWrapper: closing")
	ccb.serializer.TrySchedule(func(context.Context) {
		if ccb.balancer == nil {
			return
		}
		ccb.balancer.Close()
		ccb.balancer = nil
	})
	ccb.serializerCancel()
}

// exitIdle invokes the balancer's exitIdle method in the serializer.
func (ccb *ccBalancerWrapper) exitIdle() {
	ccb.serializer.TrySchedule(func(ctx context.Context) {
		if ctx.Err() != nil || ccb.balancer == nil {
			return
		}
		ccb.balancer.ExitIdle()
	})
}

func (ccb *ccBalancerWrapper) NewSubConn(addrs []resolver.Address, opts balancer.NewSubConnOptions) (balancer.SubConn, error) {
	ccb.cc.mu.Lock()
	defer ccb.cc.mu.Unlock()

	ccb.mu.Lock()
	if ccb.closed {
		ccb.mu.Unlock()
		return nil, fmt.Errorf("balancer is being closed; no new SubConns allowed")
	}
	ccb.mu.Unlock()

	if len(addrs) == 0 {
		return nil, fmt.Errorf("grpc: cannot create SubConn with empty address list")
	}
	ac, err := ccb.cc.newAddrConnLocked(addrs, opts)
	if err != nil {
		channelz.Warningf(logger, ccb.cc.channelz, "acBalancerWrapper: NewSubConn: failed to newAddrConn: %v", err)
		return nil, err
	}
	acbw := &acBalancerWrapper{
		ccb:           ccb,
		ac:            ac,
		producers:     make(map[balancer.ProducerBuilder]*refCountedProducer),
		stateListener: opts.StateListener,
	}
	ac.acbw = acbw
	return acbw, nil
}

func (ccb *ccBalancerWrapper) RemoveSubConn(balancer.SubConn) {
	// The graceful switch balancer will never call this.
	logger.Errorf("ccb RemoveSubConn(%v) called unexpectedly, sc")
}

func (ccb *ccBalancerWrapper) UpdateAddresses(sc balancer.SubConn, addrs []resolver.Address) {
	acbw, ok := sc.(*acBalancerWrapper)
	if !ok {
		return
	}
	acbw.UpdateAddresses(addrs)
}

func (ccb *ccBalancerWrapper) UpdateState(s balancer.State) {
	ccb.cc.mu.Lock()
	defer ccb.cc.mu.Unlock()
	if ccb.cc.conns == nil {
		// The CC has been closed; ignore this update.
		return
	}

	ccb.mu.Lock()
	if ccb.closed {
		ccb.mu.Unlock()
		return
	}
	ccb.mu.Unlock()
	// Update picker before updating state.  Even though the ordering here does
	// not matter, it can lead to multiple calls of Pick in the common start-up
	// case where we wait for ready and then perform an RPC.  If the picker is
	// updated later, we could call the "connecting" picker when the state is
	// updated, and then call the "ready" picker after the picker gets updated.

	// Note that there is no need to check if the balancer wrapper was closed,
	// as we know the graceful switch LB policy will not call cc if it has been
	// closed.
	ccb.cc.pickerWrapper.updatePicker(s.Picker)
	ccb.cc.csMgr.updateState(s.ConnectivityState)
}

func (ccb *ccBalancerWrapper) ResolveNow(o resolver.ResolveNowOptions) {
	ccb.cc.mu.RLock()
	defer ccb.cc.mu.RUnlock()

	ccb.mu.Lock()
	if ccb.closed {
		ccb.mu.Unlock()
		return
	}
	ccb.mu.Unlock()
	ccb.cc.resolveNowLocked(o)
}

func (ccb *ccBalancerWrapper) Target() string {
	return ccb.cc.target
}

// acBalancerWrapper is a wrapper on top of ac for balancers.
// It implements balancer.SubConn interface.
type acBalancerWrapper struct {
	ac            *addrConn          // read-only
	ccb           *ccBalancerWrapper // read-only
	stateListener func(balancer.SubConnState)

	producersMu sync.Mutex
	producers   map[balancer.ProducerBuilder]*refCountedProducer
}

// updateState is invoked by grpc to push a subConn state update to the
// underlying balancer.
func (acbw *acBalancerWrapper) updateState(s connectivity.State, curAddr resolver.Address, err error) {
	acbw.ccb.serializer.TrySchedule(func(ctx context.Context) {
		if ctx.Err() != nil || acbw.ccb.balancer == nil {
			return
		}
		// Invalidate all producers on any state change.
		acbw.closeProducers()

		// Even though it is optional for balancers, gracefulswitch ensures
		// opts.StateListener is set, so this cannot ever be nil.
		// TODO: delete this comment when UpdateSubConnState is removed.
		scs := balancer.SubConnState{ConnectivityState: s, ConnectionError: err}
		if s == connectivity.Ready {
			setConnectedAddress(&scs, curAddr)
		}
		acbw.stateListener(scs)
	})
}

func (acbw *acBalancerWrapper) String() string {
	return fmt.Sprintf("SubConn(id:%d)", acbw.ac.channelz.ID)
}

func (acbw *acBalancerWrapper) UpdateAddresses(addrs []resolver.Address) {
	acbw.ac.updateAddrs(addrs)
}

func (acbw *acBalancerWrapper) Connect() {
	go acbw.ac.connect()
}

func (acbw *acBalancerWrapper) Shutdown() {
	acbw.closeProducers()
	acbw.ccb.cc.removeAddrConn(acbw.ac, errConnDrain)
}

// NewStream begins a streaming RPC on the addrConn.  If the addrConn is not
// ready, blocks until it is or ctx expires.  Returns an error when the context
// expires or the addrConn is shut down.
func (acbw *acBalancerWrapper) NewStream(ctx context.Context, desc *StreamDesc, method string, opts ...CallOption) (ClientStream, error) {
	transport := acbw.ac.getReadyTransport()
	if transport == nil {
		return nil, status.Errorf(codes.Unavailable, "SubConn state is not Ready")

	}
	return newNonRetryClientStream(ctx, desc, method, transport, acbw.ac, opts...)
}

// Invoke performs a unary RPC.  If the addrConn is not ready, returns
// errSubConnNotReady.
func (acbw *acBalancerWrapper) Invoke(ctx context.Context, method string, args any, reply any, opts ...CallOption) error {
	cs, err := acbw.NewStream(ctx, unaryStreamDesc, method, opts...)
	if err != nil {
		return err
	}
	if err := cs.SendMsg(args); err != nil {
		return err
	}
	return cs.RecvMsg(reply)
}

type refCountedProducer struct {
	producer balancer.Producer
	refs     int    // number of current refs to the producer
	close    func() // underlying producer's close function
}

func (acbw *acBalancerWrapper) GetOrBuildProducer(pb balancer.ProducerBuilder) (balancer.Producer, func()) {
	acbw.producersMu.Lock()
	defer acbw.producersMu.Unlock()

	// Look up existing producer from this builder.
	pData := acbw.producers[pb]
	if pData == nil {
		// Not found; create a new one and add it to the producers map.
		p, closeFn := pb.Build(acbw)
		pData = &refCountedProducer{producer: p, close: closeFn}
		acbw.producers[pb] = pData
	}
	// Account for this new reference.
	pData.refs++

	// Return a cleanup function wrapped in a OnceFunc to remove this reference
	// and delete the refCountedProducer from the map if the total reference
	// count goes to zero.
	unref := func() {
		acbw.producersMu.Lock()
		// If closeProducers has already closed this producer instance, refs is
		// set to 0, so the check after decrementing will never pass, and the
		// producer will not be double-closed.
		pData.refs--
		if pData.refs == 0 {
			defer pData.close() // Run outside the acbw mutex
			delete(acbw.producers, pb)
		}
		acbw.producersMu.Unlock()
	}
	return pData.producer, grpcsync.OnceFunc(unref)
}

func (acbw *acBalancerWrapper) closeProducers() {
	acbw.producersMu.Lock()
	defer acbw.producersMu.Unlock()
	for pb, pData := range acbw.producers {
		pData.refs = 0
		pData.close()
		delete(acbw.producers, pb)
	}
}
