/*
 *
 * Copyright 2014 gRPC authors.
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
	"errors"
	"fmt"
	"math"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc/balancer"
	"google.golang.org/grpc/balancer/base"
	"google.golang.org/grpc/balancer/pickfirst"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/internal"
	"google.golang.org/grpc/internal/channelz"
	"google.golang.org/grpc/internal/grpcsync"
	"google.golang.org/grpc/internal/idle"
	iresolver "google.golang.org/grpc/internal/resolver"
	"google.golang.org/grpc/internal/stats"
	"google.golang.org/grpc/internal/transport"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/resolver"
	"google.golang.org/grpc/serviceconfig"
	"google.golang.org/grpc/status"

	_ "google.golang.org/grpc/balancer/roundrobin"           // To register roundrobin.
	_ "google.golang.org/grpc/internal/resolver/passthrough" // To register passthrough resolver.
	_ "google.golang.org/grpc/internal/resolver/unix"        // To register unix resolver.
	_ "google.golang.org/grpc/resolver/dns"                  // To register dns resolver.
)

const (
	// minimum time to give a connection to complete
	minConnectTimeout = 20 * time.Second
)

var (
	// ErrClientConnClosing indicates that the operation is illegal because
	// the ClientConn is closing.
	//
	// Deprecated: this error should not be relied upon by users; use the status
	// code of Canceled instead.
	ErrClientConnClosing = status.Error(codes.Canceled, "grpc: the client connection is closing")
	// errConnDrain indicates that the connection starts to be drained and does not accept any new RPCs.
	errConnDrain = errors.New("grpc: the connection is drained")
	// errConnClosing indicates that the connection is closing.
	errConnClosing = errors.New("grpc: the connection is closing")
	// errConnIdling indicates the connection is being closed as the channel
	// is moving to an idle mode due to inactivity.
	errConnIdling = errors.New("grpc: the connection is closing due to channel idleness")
	// invalidDefaultServiceConfigErrPrefix is used to prefix the json parsing error for the default
	// service config.
	invalidDefaultServiceConfigErrPrefix = "grpc: the provided default service config is invalid"
	// PickFirstBalancerName is the name of the pick_first balancer.
	PickFirstBalancerName = pickfirst.Name
)

// The following errors are returned from Dial and DialContext
var (
	// errNoTransportSecurity indicates that there is no transport security
	// being set for ClientConn. Users should either set one or explicitly
	// call WithInsecure DialOption to disable security.
	errNoTransportSecurity = errors.New("grpc: no transport security set (use grpc.WithTransportCredentials(insecure.NewCredentials()) explicitly or set credentials)")
	// errTransportCredsAndBundle indicates that creds bundle is used together
	// with other individual Transport Credentials.
	errTransportCredsAndBundle = errors.New("grpc: credentials.Bundle may not be used with individual TransportCredentials")
	// errNoTransportCredsInBundle indicated that the configured creds bundle
	// returned a transport credentials which was nil.
	errNoTransportCredsInBundle = errors.New("grpc: credentials.Bundle must return non-nil transport credentials")
	// errTransportCredentialsMissing indicates that users want to transmit
	// security information (e.g., OAuth2 token) which requires secure
	// connection on an insecure connection.
	errTransportCredentialsMissing = errors.New("grpc: the credentials require transport level security (use grpc.WithTransportCredentials() to set)")
)

const (
	defaultClientMaxReceiveMessageSize = 1024 * 1024 * 4
	defaultClientMaxSendMessageSize    = math.MaxInt32
	// http2IOBufSize specifies the buffer size for sending frames.
	defaultWriteBufSize = 32 * 1024
	defaultReadBufSize  = 32 * 1024
)

type defaultConfigSelector struct {
	sc *ServiceConfig
}

func (dcs *defaultConfigSelector) SelectConfig(rpcInfo iresolver.RPCInfo) (*iresolver.RPCConfig, error) {
	return &iresolver.RPCConfig{
		Context:      rpcInfo.Context,
		MethodConfig: getMethodConfig(dcs.sc, rpcInfo.Method),
	}, nil
}

// NewClient creates a new gRPC "channel" for the target URI provided.  No I/O
// is performed.  Use of the ClientConn for RPCs will automatically cause it to
// connect.  Connect may be used to manually create a connection, but for most
// users this is unnecessary.
//
// The target name syntax is defined in
// https://github.com/grpc/grpc/blob/master/doc/naming.md.  e.g. to use dns
// resolver, a "dns:///" prefix should be applied to the target.
//
// The DialOptions returned by WithBlock, WithTimeout,
// WithReturnConnectionError, and FailOnNonTempDialError are ignored by this
// function.
func NewClient(target string, opts ...DialOption) (conn *ClientConn, err error) {
	cc := &ClientConn{
		target: target,
		conns:  make(map[*addrConn]struct{}),
		dopts:  defaultDialOptions(),
	}

	cc.retryThrottler.Store((*retryThrottler)(nil))
	cc.safeConfigSelector.UpdateConfigSelector(&defaultConfigSelector{nil})
	cc.ctx, cc.cancel = context.WithCancel(context.Background())

	// Apply dial options.
	disableGlobalOpts := false
	for _, opt := range opts {
		if _, ok := opt.(*disableGlobalDialOptions); ok {
			disableGlobalOpts = true
			break
		}
	}

	if !disableGlobalOpts {
		for _, opt := range globalDialOptions {
			opt.apply(&cc.dopts)
		}
	}

	for _, opt := range opts {
		opt.apply(&cc.dopts)
	}

	// Determine the resolver to use.
	if err := cc.initParsedTargetAndResolverBuilder(); err != nil {
		return nil, err
	}

	for _, opt := range globalPerTargetDialOptions {
		opt.DialOptionForTarget(cc.parsedTarget.URL).apply(&cc.dopts)
	}

	chainUnaryClientInterceptors(cc)
	chainStreamClientInterceptors(cc)

	if err := cc.validateTransportCredentials(); err != nil {
		return nil, err
	}

	if cc.dopts.defaultServiceConfigRawJSON != nil {
		scpr := parseServiceConfig(*cc.dopts.defaultServiceConfigRawJSON, cc.dopts.maxCallAttempts)
		if scpr.Err != nil {
			return nil, fmt.Errorf("%s: %v", invalidDefaultServiceConfigErrPrefix, scpr.Err)
		}
		cc.dopts.defaultServiceConfig, _ = scpr.Config.(*ServiceConfig)
	}
	cc.mkp = cc.dopts.copts.KeepaliveParams

	if err = cc.initAuthority(); err != nil {
		return nil, err
	}

	// Register ClientConn with channelz. Note that this is only done after
	// channel creation cannot fail.
	cc.channelzRegistration(target)
	channelz.Infof(logger, cc.channelz, "parsed dial target is: %#v", cc.parsedTarget)
	channelz.Infof(logger, cc.channelz, "Channel authority set to %q", cc.authority)

	cc.csMgr = newConnectivityStateManager(cc.ctx, cc.channelz)
	cc.pickerWrapper = newPickerWrapper(cc.dopts.copts.StatsHandlers)

	cc.metricsRecorderList = stats.NewMetricsRecorderList(cc.dopts.copts.StatsHandlers)

	cc.initIdleStateLocked() // Safe to call without the lock, since nothing else has a reference to cc.
	cc.idlenessMgr = idle.NewManager((*idler)(cc), cc.dopts.idleTimeout)

	return cc, nil
}

// Dial calls DialContext(context.Background(), target, opts...).
//
// Deprecated: use NewClient instead.  Will be supported throughout 1.x.
func Dial(target string, opts ...DialOption) (*ClientConn, error) {
	return DialContext(context.Background(), target, opts...)
}

// DialContext calls NewClient and then exits idle mode.  If WithBlock(true) is
// used, it calls Connect and WaitForStateChange until either the context
// expires or the state of the ClientConn is Ready.
//
// One subtle difference between NewClient and Dial and DialContext is that the
// former uses "dns" as the default name resolver, while the latter use
// "passthrough" for backward compatibility.  This distinction should not matter
// to most users, but could matter to legacy users that specify a custom dialer
// and expect it to receive the target string directly.
//
// Deprecated: use NewClient instead.  Will be supported throughout 1.x.
func DialContext(ctx context.Context, target string, opts ...DialOption) (conn *ClientConn, err error) {
	// At the end of this method, we kick the channel out of idle, rather than
	// waiting for the first rpc.
	opts = append([]DialOption{withDefaultScheme("passthrough")}, opts...)
	cc, err := NewClient(target, opts...)
	if err != nil {
		return nil, err
	}

	// We start the channel off in idle mode, but kick it out of idle now,
	// instead of waiting for the first RPC.  This is the legacy behavior of
	// Dial.
	defer func() {
		if err != nil {
			cc.Close()
		}
	}()

	// This creates the name resolver, load balancer, etc.
	if err := cc.idlenessMgr.ExitIdleMode(); err != nil {
		return nil, err
	}

	// Return now for non-blocking dials.
	if !cc.dopts.block {
		return cc, nil
	}

	if cc.dopts.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, cc.dopts.timeout)
		defer cancel()
	}
	defer func() {
		select {
		case <-ctx.Done():
			switch {
			case ctx.Err() == err:
				conn = nil
			case err == nil || !cc.dopts.returnLastError:
				conn, err = nil, ctx.Err()
			default:
				conn, err = nil, fmt.Errorf("%v: %v", ctx.Err(), err)
			}
		default:
		}
	}()

	// A blocking dial blocks until the clientConn is ready.
	for {
		s := cc.GetState()
		if s == connectivity.Idle {
			cc.Connect()
		}
		if s == connectivity.Ready {
			return cc, nil
		} else if cc.dopts.copts.FailOnNonTempDialError && s == connectivity.TransientFailure {
			if err = cc.connectionError(); err != nil {
				terr, ok := err.(interface {
					Temporary() bool
				})
				if ok && !terr.Temporary() {
					return nil, err
				}
			}
		}
		if !cc.WaitForStateChange(ctx, s) {
			// ctx got timeout or canceled.
			if err = cc.connectionError(); err != nil && cc.dopts.returnLastError {
				return nil, err
			}
			return nil, ctx.Err()
		}
	}
}

// addTraceEvent is a helper method to add a trace event on the channel. If the
// channel is a nested one, the same event is also added on the parent channel.
func (cc *ClientConn) addTraceEvent(msg string) {
	ted := &channelz.TraceEvent{
		Desc:     fmt.Sprintf("Channel %s", msg),
		Severity: channelz.CtInfo,
	}
	if cc.dopts.channelzParent != nil {
		ted.Parent = &channelz.TraceEvent{
			Desc:     fmt.Sprintf("Nested channel(id:%d) %s", cc.channelz.ID, msg),
			Severity: channelz.CtInfo,
		}
	}
	channelz.AddTraceEvent(logger, cc.channelz, 0, ted)
}

type idler ClientConn

func (i *idler) EnterIdleMode() {
	(*ClientConn)(i).enterIdleMode()
}

func (i *idler) ExitIdleMode() error {
	return (*ClientConn)(i).exitIdleMode()
}

// exitIdleMode moves the channel out of idle mode by recreating the name
// resolver and load balancer.  This should never be called directly; use
// cc.idlenessMgr.ExitIdleMode instead.
func (cc *ClientConn) exitIdleMode() (err error) {
	cc.mu.Lock()
	if cc.conns == nil {
		cc.mu.Unlock()
		return errConnClosing
	}
	cc.mu.Unlock()

	// This needs to be called without cc.mu because this builds a new resolver
	// which might update state or report error inline, which would then need to
	// acquire cc.mu.
	if err := cc.resolverWrapper.start(); err != nil {
		return err
	}

	cc.addTraceEvent("exiting idle mode")
	return nil
}

// initIdleStateLocked initializes common state to how it should be while idle.
func (cc *ClientConn) initIdleStateLocked() {
	cc.resolverWrapper = newCCResolverWrapper(cc)
	cc.balancerWrapper = newCCBalancerWrapper(cc)
	cc.firstResolveEvent = grpcsync.NewEvent()
	// cc.conns == nil is a proxy for the ClientConn being closed. So, instead
	// of setting it to nil here, we recreate the map. This also means that we
	// don't have to do this when exiting idle mode.
	cc.conns = make(map[*addrConn]struct{})
}

// enterIdleMode puts the channel in idle mode, and as part of it shuts down the
// name resolver, load balancer, and any subchannels.  This should never be
// called directly; use cc.idlenessMgr.EnterIdleMode instead.
func (cc *ClientConn) enterIdleMode() {
	cc.mu.Lock()

	if cc.conns == nil {
		cc.mu.Unlock()
		return
	}

	conns := cc.conns

	rWrapper := cc.resolverWrapper
	rWrapper.close()
	cc.pickerWrapper.reset()
	bWrapper := cc.balancerWrapper
	bWrapper.close()
	cc.csMgr.updateState(connectivity.Idle)
	cc.addTraceEvent("entering idle mode")

	cc.initIdleStateLocked()

	cc.mu.Unlock()

	// Block until the name resolver and LB policy are closed.
	<-rWrapper.serializer.Done()
	<-bWrapper.serializer.Done()

	// Close all subchannels after the LB policy is closed.
	for ac := range conns {
		ac.tearDown(errConnIdling)
	}
}

// validateTransportCredentials performs a series of checks on the configured
// transport credentials. It returns a non-nil error if any of these conditions
// are met:
//   - no transport creds and no creds bundle is configured
//   - both transport creds and creds bundle are configured
//   - creds bundle is configured, but it lacks a transport credentials
//   - insecure transport creds configured alongside call creds that require
//     transport level security
//
// If none of the above conditions are met, the configured credentials are
// deemed valid and a nil error is returned.
func (cc *ClientConn) validateTransportCredentials() error {
	if cc.dopts.copts.TransportCredentials == nil && cc.dopts.copts.CredsBundle == nil {
		return errNoTransportSecurity
	}
	if cc.dopts.copts.TransportCredentials != nil && cc.dopts.copts.CredsBundle != nil {
		return errTransportCredsAndBundle
	}
	if cc.dopts.copts.CredsBundle != nil && cc.dopts.copts.CredsBundle.TransportCredentials() == nil {
		return errNoTransportCredsInBundle
	}
	transportCreds := cc.dopts.copts.TransportCredentials
	if transportCreds == nil {
		transportCreds = cc.dopts.copts.CredsBundle.TransportCredentials()
	}
	if transportCreds.Info().SecurityProtocol == "insecure" {
		for _, cd := range cc.dopts.copts.PerRPCCredentials {
			if cd.RequireTransportSecurity() {
				return errTransportCredentialsMissing
			}
		}
	}
	return nil
}

// channelzRegistration registers the newly created ClientConn with channelz and
// stores the returned identifier in `cc.channelz`.  A channelz trace event is
// emitted for ClientConn creation. If the newly created ClientConn is a nested
// one, i.e a valid parent ClientConn ID is specified via a dial option, the
// trace event is also added to the parent.
//
// Doesn't grab cc.mu as this method is expected to be called only at Dial time.
func (cc *ClientConn) channelzRegistration(target string) {
	parentChannel, _ := cc.dopts.channelzParent.(*channelz.Channel)
	cc.channelz = channelz.RegisterChannel(parentChannel, target)
	cc.addTraceEvent("created")
}

// chainUnaryClientInterceptors chains all unary client interceptors into one.
func chainUnaryClientInterceptors(cc *ClientConn) {
	interceptors := cc.dopts.chainUnaryInts
	// Prepend dopts.unaryInt to the chaining interceptors if it exists, since unaryInt will
	// be executed before any other chained interceptors.
	if cc.dopts.unaryInt != nil {
		interceptors = append([]UnaryClientInterceptor{cc.dopts.unaryInt}, interceptors...)
	}
	var chainedInt UnaryClientInterceptor
	if len(interceptors) == 0 {
		chainedInt = nil
	} else if len(interceptors) == 1 {
		chainedInt = interceptors[0]
	} else {
		chainedInt = func(ctx context.Context, method string, req, reply any, cc *ClientConn, invoker UnaryInvoker, opts ...CallOption) error {
			return interceptors[0](ctx, method, req, reply, cc, getChainUnaryInvoker(interceptors, 0, invoker), opts...)
		}
	}
	cc.dopts.unaryInt = chainedInt
}

// getChainUnaryInvoker recursively generate the chained unary invoker.
func getChainUnaryInvoker(interceptors []UnaryClientInterceptor, curr int, finalInvoker UnaryInvoker) UnaryInvoker {
	if curr == len(interceptors)-1 {
		return finalInvoker
	}
	return func(ctx context.Context, method string, req, reply any, cc *ClientConn, opts ...CallOption) error {
		return interceptors[curr+1](ctx, method, req, reply, cc, getChainUnaryInvoker(interceptors, curr+1, finalInvoker), opts...)
	}
}

// chainStreamClientInterceptors chains all stream client interceptors into one.
func chainStreamClientInterceptors(cc *ClientConn) {
	interceptors := cc.dopts.chainStreamInts
	// Prepend dopts.streamInt to the chaining interceptors if it exists, since streamInt will
	// be executed before any other chained interceptors.
	if cc.dopts.streamInt != nil {
		interceptors = append([]StreamClientInterceptor{cc.dopts.streamInt}, interceptors...)
	}
	var chainedInt StreamClientInterceptor
	if len(interceptors) == 0 {
		chainedInt = nil
	} else if len(interceptors) == 1 {
		chainedInt = interceptors[0]
	} else {
		chainedInt = func(ctx context.Context, desc *StreamDesc, cc *ClientConn, method string, streamer Streamer, opts ...CallOption) (ClientStream, error) {
			return interceptors[0](ctx, desc, cc, method, getChainStreamer(interceptors, 0, streamer), opts...)
		}
	}
	cc.dopts.streamInt = chainedInt
}

// getChainStreamer recursively generate the chained client stream constructor.
func getChainStreamer(interceptors []StreamClientInterceptor, curr int, finalStreamer Streamer) Streamer {
	if curr == len(interceptors)-1 {
		return finalStreamer
	}
	return func(ctx context.Context, desc *StreamDesc, cc *ClientConn, method string, opts ...CallOption) (ClientStream, error) {
		return interceptors[curr+1](ctx, desc, cc, method, getChainStreamer(interceptors, curr+1, finalStreamer), opts...)
	}
}

// newConnectivityStateManager creates an connectivityStateManager with
// the specified channel.
func newConnectivityStateManager(ctx context.Context, channel *channelz.Channel) *connectivityStateManager {
	return &connectivityStateManager{
		channelz: channel,
		pubSub:   grpcsync.NewPubSub(ctx),
	}
}

// connectivityStateManager keeps the connectivity.State of ClientConn.
// This struct will eventually be exported so the balancers can access it.
//
// TODO: If possible, get rid of the `connectivityStateManager` type, and
// provide this functionality using the `PubSub`, to avoid keeping track of
// the connectivity state at two places.
type connectivityStateManager struct {
	mu         sync.Mutex
	state      connectivity.State
	notifyChan chan struct{}
	channelz   *channelz.Channel
	pubSub     *grpcsync.PubSub
}

// updateState updates the connectivity.State of ClientConn.
// If there's a change it notifies goroutines waiting on state change to
// happen.
func (csm *connectivityStateManager) updateState(state connectivity.State) {
	csm.mu.Lock()
	defer csm.mu.Unlock()
	if csm.state == connectivity.Shutdown {
		return
	}
	if csm.state == state {
		return
	}
	csm.state = state
	csm.channelz.ChannelMetrics.State.Store(&state)
	csm.pubSub.Publish(state)

	channelz.Infof(logger, csm.channelz, "Channel Connectivity change to %v", state)
	if csm.notifyChan != nil {
		// There are other goroutines waiting on this channel.
		close(csm.notifyChan)
		csm.notifyChan = nil
	}
}

func (csm *connectivityStateManager) getState() connectivity.State {
	csm.mu.Lock()
	defer csm.mu.Unlock()
	return csm.state
}

func (csm *connectivityStateManager) getNotifyChan() <-chan struct{} {
	csm.mu.Lock()
	defer csm.mu.Unlock()
	if csm.notifyChan == nil {
		csm.notifyChan = make(chan struct{})
	}
	return csm.notifyChan
}

// ClientConnInterface defines the functions clients need to perform unary and
// streaming RPCs.  It is implemented by *ClientConn, and is only intended to
// be referenced by generated code.
type ClientConnInterface interface {
	// Invoke performs a unary RPC and returns after the response is received
	// into reply.
	Invoke(ctx context.Context, method string, args any, reply any, opts ...CallOption) error
	// NewStream begins a streaming RPC.
	NewStream(ctx context.Context, desc *StreamDesc, method string, opts ...CallOption) (ClientStream, error)
}

// Assert *ClientConn implements ClientConnInterface.
var _ ClientConnInterface = (*ClientConn)(nil)

// ClientConn represents a virtual connection to a conceptual endpoint, to
// perform RPCs.
//
// A ClientConn is free to have zero or more actual connections to the endpoint
// based on configuration, load, etc. It is also free to determine which actual
// endpoints to use and may change it every RPC, permitting client-side load
// balancing.
//
// A ClientConn encapsulates a range of functionality including name
// resolution, TCP connection establishment (with retries and backoff) and TLS
// handshakes. It also handles errors on established connections by
// re-resolving the name and reconnecting.
type ClientConn struct {
	ctx    context.Context    // Initialized using the background context at dial time.
	cancel context.CancelFunc // Cancelled on close.

	// The following are initialized at dial time, and are read-only after that.
	target              string            // User's dial target.
	parsedTarget        resolver.Target   // See initParsedTargetAndResolverBuilder().
	authority           string            // See initAuthority().
	dopts               dialOptions       // Default and user specified dial options.
	channelz            *channelz.Channel // Channelz object.
	resolverBuilder     resolver.Builder  // See initParsedTargetAndResolverBuilder().
	idlenessMgr         *idle.Manager
	metricsRecorderList *stats.MetricsRecorderList

	// The following provide their own synchronization, and therefore don't
	// require cc.mu to be held to access them.
	csMgr              *connectivityStateManager
	pickerWrapper      *pickerWrapper
	safeConfigSelector iresolver.SafeConfigSelector
	retryThrottler     atomic.Value // Updated from service config.

	// mu protects the following fields.
	// TODO: split mu so the same mutex isn't used for everything.
	mu              sync.RWMutex
	resolverWrapper *ccResolverWrapper         // Always recreated whenever entering idle to simplify Close.
	balancerWrapper *ccBalancerWrapper         // Always recreated whenever entering idle to simplify Close.
	sc              *ServiceConfig             // Latest service config received from the resolver.
	conns           map[*addrConn]struct{}     // Set to nil on close.
	mkp             keepalive.ClientParameters // May be updated upon receipt of a GoAway.
	// firstResolveEvent is used to track whether the name resolver sent us at
	// least one update. RPCs block on this event.  May be accessed without mu
	// if we know we cannot be asked to enter idle mode while accessing it (e.g.
	// when the idle manager has already been closed, or if we are already
	// entering idle mode).
	firstResolveEvent *grpcsync.Event

	lceMu               sync.Mutex // protects lastConnectionError
	lastConnectionError error
}

// WaitForStateChange waits until the connectivity.State of ClientConn changes from sourceState or
// ctx expires. A true value is returned in former case and false in latter.
func (cc *ClientConn) WaitForStateChange(ctx context.Context, sourceState connectivity.State) bool {
	ch := cc.csMgr.getNotifyChan()
	if cc.csMgr.getState() != sourceState {
		return true
	}
	select {
	case <-ctx.Done():
		return false
	case <-ch:
		return true
	}
}

// GetState returns the connectivity.State of ClientConn.
func (cc *ClientConn) GetState() connectivity.State {
	return cc.csMgr.getState()
}

// Connect causes all subchannels in the ClientConn to attempt to connect if
// the channel is idle.  Does not wait for the connection attempts to begin
// before returning.
//
// # Experimental
//
// Notice: This API is EXPERIMENTAL and may be changed or removed in a later
// release.
func (cc *ClientConn) Connect() {
	if err := cc.idlenessMgr.ExitIdleMode(); err != nil {
		cc.addTraceEvent(err.Error())
		return
	}
	// If the ClientConn was not in idle mode, we need to call ExitIdle on the
	// LB policy so that connections can be created.
	cc.mu.Lock()
	cc.balancerWrapper.exitIdle()
	cc.mu.Unlock()
}

// waitForResolvedAddrs blocks until the resolver has provided addresses or the
// context expires.  Returns nil unless the context expires first; otherwise
// returns a status error based on the context.
func (cc *ClientConn) waitForResolvedAddrs(ctx context.Context) error {
	// This is on the RPC path, so we use a fast path to avoid the
	// more-expensive "select" below after the resolver has returned once.
	if cc.firstResolveEvent.HasFired() {
		return nil
	}
	select {
	case <-cc.firstResolveEvent.Done():
		return nil
	case <-ctx.Done():
		return status.FromContextError(ctx.Err()).Err()
	case <-cc.ctx.Done():
		return ErrClientConnClosing
	}
}

var emptyServiceConfig *ServiceConfig

func init() {
	cfg := parseServiceConfig("{}", defaultMaxCallAttempts)
	if cfg.Err != nil {
		panic(fmt.Sprintf("impossible error parsing empty service config: %v", cfg.Err))
	}
	emptyServiceConfig = cfg.Config.(*ServiceConfig)

	internal.SubscribeToConnectivityStateChanges = func(cc *ClientConn, s grpcsync.Subscriber) func() {
		return cc.csMgr.pubSub.Subscribe(s)
	}
	internal.EnterIdleModeForTesting = func(cc *ClientConn) {
		cc.idlenessMgr.EnterIdleModeForTesting()
	}
	internal.ExitIdleModeForTesting = func(cc *ClientConn) error {
		return cc.idlenessMgr.ExitIdleMode()
	}
}

func (cc *ClientConn) maybeApplyDefaultServiceConfig() {
	if cc.sc != nil {
		cc.applyServiceConfigAndBalancer(cc.sc, nil)
		return
	}
	if cc.dopts.defaultServiceConfig != nil {
		cc.applyServiceConfigAndBalancer(cc.dopts.defaultServiceConfig, &defaultConfigSelector{cc.dopts.defaultServiceConfig})
	} else {
		cc.applyServiceConfigAndBalancer(emptyServiceConfig, &defaultConfigSelector{emptyServiceConfig})
	}
}

func (cc *ClientConn) updateResolverStateAndUnlock(s resolver.State, err error) error {
	defer cc.firstResolveEvent.Fire()
	// Check if the ClientConn is already closed. Some fields (e.g.
	// balancerWrapper) are set to nil when closing the ClientConn, and could
	// cause nil pointer panic if we don't have this check.
	if cc.conns == nil {
		cc.mu.Unlock()
		return nil
	}

	if err != nil {
		// May need to apply the initial service config in case the resolver
		// doesn't support service configs, or doesn't provide a service config
		// with the new addresses.
		cc.maybeApplyDefaultServiceConfig()

		cc.balancerWrapper.resolverError(err)

		// No addresses are valid with err set; return early.
		cc.mu.Unlock()
		return balancer.ErrBadResolverState
	}

	var ret error
	if cc.dopts.disableServiceConfig {
		channelz.Infof(logger, cc.channelz, "ignoring service config from resolver (%v) and applying the default because service config is disabled", s.ServiceConfig)
		cc.maybeApplyDefaultServiceConfig()
	} else if s.ServiceConfig == nil {
		cc.maybeApplyDefaultServiceConfig()
		// TODO: do we need to apply a failing LB policy if there is no
		// default, per the error handling design?
	} else {
		if sc, ok := s.ServiceConfig.Config.(*ServiceConfig); s.ServiceConfig.Err == nil && ok {
			configSelector := iresolver.GetConfigSelector(s)
			if configSelector != nil {
				if len(s.ServiceConfig.Config.(*ServiceConfig).Methods) != 0 {
					channelz.Infof(logger, cc.channelz, "method configs in service config will be ignored due to presence of config selector")
				}
			} else {
				configSelector = &defaultConfigSelector{sc}
			}
			cc.applyServiceConfigAndBalancer(sc, configSelector)
		} else {
			ret = balancer.ErrBadResolverState
			if cc.sc == nil {
				// Apply the failing LB only if we haven't received valid service config
				// from the name resolver in the past.
				cc.applyFailingLBLocked(s.ServiceConfig)
				cc.mu.Unlock()
				return ret
			}
		}
	}

	var balCfg serviceconfig.LoadBalancingConfig
	if cc.sc != nil && cc.sc.lbConfig != nil {
		balCfg = cc.sc.lbConfig
	}
	bw := cc.balancerWrapper
	cc.mu.Unlock()

	uccsErr := bw.updateClientConnState(&balancer.ClientConnState{ResolverState: s, BalancerConfig: balCfg})
	if ret == nil {
		ret = uccsErr // prefer ErrBadResolver state since any other error is
		// currently meaningless to the caller.
	}
	return ret
}

// applyFailingLBLocked is akin to configuring an LB policy on the channel which
// always fails RPCs. Here, an actual LB policy is not configured, but an always
// erroring picker is configured, which returns errors with information about
// what was invalid in the received service config. A config selector with no
// service config is configured, and the connectivity state of the channel is
// set to TransientFailure.
func (cc *ClientConn) applyFailingLBLocked(sc *serviceconfig.ParseResult) {
	var err error
	if sc.Err != nil {
		err = status.Errorf(codes.Unavailable, "error parsing service config: %v", sc.Err)
	} else {
		err = status.Errorf(codes.Unavailable, "illegal service config type: %T", sc.Config)
	}
	cc.safeConfigSelector.UpdateConfigSelector(&defaultConfigSelector{nil})
	cc.pickerWrapper.updatePicker(base.NewErrPicker(err))
	cc.csMgr.updateState(connectivity.TransientFailure)
}

// Makes a copy of the input addresses slice. Addresses are passed during
// subconn creation and address update operations.
func copyAddresses(in []resolver.Address) []resolver.Address {
	out := make([]resolver.Address, len(in))
	copy(out, in)
	return out
}

// newAddrConnLocked creates an addrConn for addrs and adds it to cc.conns.
//
// Caller needs to make sure len(addrs) > 0.
func (cc *ClientConn) newAddrConnLocked(addrs []resolver.Address, opts balancer.NewSubConnOptions) (*addrConn, error) {
	if cc.conns == nil {
		return nil, ErrClientConnClosing
	}

	ac := &addrConn{
		state:        connectivity.Idle,
		cc:           cc,
		addrs:        copyAddresses(addrs),
		scopts:       opts,
		dopts:        cc.dopts,
		channelz:     channelz.RegisterSubChannel(cc.channelz, ""),
		resetBackoff: make(chan struct{}),
	}
	ac.ctx, ac.cancel = context.WithCancel(cc.ctx)
	// Start with our address set to the first address; this may be updated if
	// we connect to different addresses.
	ac.channelz.ChannelMetrics.Target.Store(&addrs[0].Addr)

	channelz.AddTraceEvent(logger, ac.channelz, 0, &channelz.TraceEvent{
		Desc:     "Subchannel created",
		Severity: channelz.CtInfo,
		Parent: &channelz.TraceEvent{
			Desc:     fmt.Sprintf("Subchannel(id:%d) created", ac.channelz.ID),
			Severity: channelz.CtInfo,
		},
	})

	// Track ac in cc. This needs to be done before any getTransport(...) is called.
	cc.conns[ac] = struct{}{}
	return ac, nil
}

// removeAddrConn removes the addrConn in the subConn from clientConn.
// It also tears down the ac with the given error.
func (cc *ClientConn) removeAddrConn(ac *addrConn, err error) {
	cc.mu.Lock()
	if cc.conns == nil {
		cc.mu.Unlock()
		return
	}
	delete(cc.conns, ac)
	cc.mu.Unlock()
	ac.tearDown(err)
}

// Target returns the target string of the ClientConn.
func (cc *ClientConn) Target() string {
	return cc.target
}

// CanonicalTarget returns the canonical target string of the ClientConn.
func (cc *ClientConn) CanonicalTarget() string {
	return cc.parsedTarget.String()
}

func (cc *ClientConn) incrCallsStarted() {
	cc.channelz.ChannelMetrics.CallsStarted.Add(1)
	cc.channelz.ChannelMetrics.LastCallStartedTimestamp.Store(time.Now().UnixNano())
}

func (cc *ClientConn) incrCallsSucceeded() {
	cc.channelz.ChannelMetrics.CallsSucceeded.Add(1)
}

func (cc *ClientConn) incrCallsFailed() {
	cc.channelz.ChannelMetrics.CallsFailed.Add(1)
}

// connect starts creating a transport.
// It does nothing if the ac is not IDLE.
// TODO(bar) Move this to the addrConn section.
func (ac *addrConn) connect() error {
	ac.mu.Lock()
	if ac.state == connectivity.Shutdown {
		if logger.V(2) {
			logger.Infof("connect called on shutdown addrConn; ignoring.")
		}
		ac.mu.Unlock()
		return errConnClosing
	}
	if ac.state != connectivity.Idle {
		if logger.V(2) {
			logger.Infof("connect called on addrConn in non-idle state (%v); ignoring.", ac.state)
		}
		ac.mu.Unlock()
		return nil
	}

	ac.resetTransportAndUnlock()
	return nil
}

// equalAddressIgnoringBalAttributes returns true is a and b are considered equal.
// This is different from the Equal method on the resolver.Address type which
// considers all fields to determine equality. Here, we only consider fields
// that are meaningful to the subConn.
func equalAddressIgnoringBalAttributes(a, b *resolver.Address) bool {
	return a.Addr == b.Addr && a.ServerName == b.ServerName &&
		a.Attributes.Equal(b.Attributes) &&
		a.Metadata == b.Metadata
}

func equalAddressesIgnoringBalAttributes(a, b []resolver.Address) bool {
	return slices.EqualFunc(a, b, func(a, b resolver.Address) bool { return equalAddressIgnoringBalAttributes(&a, &b) })
}

// updateAddrs updates ac.addrs with the new addresses list and handles active
// connections or connection attempts.
func (ac *addrConn) updateAddrs(addrs []resolver.Address) {
	addrs = copyAddresses(addrs)
	limit := len(addrs)
	if limit > 5 {
		limit = 5
	}
	channelz.Infof(logger, ac.channelz, "addrConn: updateAddrs addrs (%d of %d): %v", limit, len(addrs), addrs[:limit])

	ac.mu.Lock()
	if equalAddressesIgnoringBalAttributes(ac.addrs, addrs) {
		ac.mu.Unlock()
		return
	}

	ac.addrs = addrs

	if ac.state == connectivity.Shutdown ||
		ac.state == connectivity.TransientFailure ||
		ac.state == connectivity.Idle {
		// We were not connecting, so do nothing but update the addresses.
		ac.mu.Unlock()
		return
	}

	if ac.state == connectivity.Ready {
		// Try to find the connected address.
		for _, a := range addrs {
			a.ServerName = ac.cc.getServerName(a)
			if equalAddressIgnoringBalAttributes(&a, &ac.curAddr) {
				// We are connected to a valid address, so do nothing but
				// update the addresses.
				ac.mu.Unlock()
				return
			}
		}
	}

	// We are either connected to the wrong address or currently connecting.
	// Stop the current iteration and restart.

	ac.cancel()
	ac.ctx, ac.cancel = context.WithCancel(ac.cc.ctx)

	// We have to defer here because GracefulClose => onClose, which requires
	// locking ac.mu.
	if ac.transport != nil {
		defer ac.transport.GracefulClose()
		ac.transport = nil
	}

	if len(addrs) == 0 {
		ac.updateConnectivityState(connectivity.Idle, nil)
	}

	// Since we were connecting/connected, we should start a new connection
	// attempt.
	go ac.resetTransportAndUnlock()
}

// getServerName determines the serverName to be used in the connection
// handshake. The default value for the serverName is the authority on the
// ClientConn, which either comes from the user's dial target or through an
// authority override specified using the WithAuthority dial option. Name
// resolvers can specify a per-address override for the serverName through the
// resolver.Address.ServerName field which is used only if the WithAuthority
// dial option was not used. The rationale is that per-address authority
// overrides specified by the name resolver can represent a security risk, while
// an override specified by the user is more dependable since they probably know
// what they are doing.
func (cc *ClientConn) getServerName(addr resolver.Address) string {
	if cc.dopts.authority != "" {
		return cc.dopts.authority
	}
	if addr.ServerName != "" {
		return addr.ServerName
	}
	return cc.authority
}

func getMethodConfig(sc *ServiceConfig, method string) MethodConfig {
	if sc == nil {
		return MethodConfig{}
	}
	if m, ok := sc.Methods[method]; ok {
		return m
	}
	i := strings.LastIndex(method, "/")
	if m, ok := sc.Methods[method[:i+1]]; ok {
		return m
	}
	return sc.Methods[""]
}

// GetMethodConfig gets the method config of the input method.
// If there's an exact match for input method (i.e. /service/method), we return
// the corresponding MethodConfig.
// If there isn't an exact match for the input method, we look for the service's default
// config under the service (i.e /service/) and then for the default for all services (empty string).
//
// If there is a default MethodConfig for the service, we return it.
// Otherwise, we return an empty MethodConfig.
func (cc *ClientConn) GetMethodConfig(method string) MethodConfig {
	// TODO: Avoid the locking here.
	cc.mu.RLock()
	defer cc.mu.RUnlock()
	return getMethodConfig(cc.sc, method)
}

func (cc *ClientConn) healthCheckConfig() *healthCheckConfig {
	cc.mu.RLock()
	defer cc.mu.RUnlock()
	if cc.sc == nil {
		return nil
	}
	return cc.sc.healthCheckConfig
}

func (cc *ClientConn) getTransport(ctx context.Context, failfast bool, method string) (transport.ClientTransport, balancer.PickResult, error) {
	return cc.pickerWrapper.pick(ctx, failfast, balancer.PickInfo{
		Ctx:            ctx,
		FullMethodName: method,
	})
}

func (cc *ClientConn) applyServiceConfigAndBalancer(sc *ServiceConfig, configSelector iresolver.ConfigSelector) {
	if sc == nil {
		// should never reach here.
		return
	}
	cc.sc = sc
	if configSelector != nil {
		cc.safeConfigSelector.UpdateConfigSelector(configSelector)
	}

	if cc.sc.retryThrottling != nil {
		newThrottler := &retryThrottler{
			tokens: cc.sc.retryThrottling.MaxTokens,
			max:    cc.sc.retryThrottling.MaxTokens,
			thresh: cc.sc.retryThrottling.MaxTokens / 2,
			ratio:  cc.sc.retryThrottling.TokenRatio,
		}
		cc.retryThrottler.Store(newThrottler)
	} else {
		cc.retryThrottler.Store((*retryThrottler)(nil))
	}
}

func (cc *ClientConn) resolveNow(o resolver.ResolveNowOptions) {
	cc.mu.RLock()
	cc.resolverWrapper.resolveNow(o)
	cc.mu.RUnlock()
}

func (cc *ClientConn) resolveNowLocked(o resolver.ResolveNowOptions) {
	cc.resolverWrapper.resolveNow(o)
}

// ResetConnectBackoff wakes up all subchannels in transient failure and causes
// them to attempt another connection immediately.  It also resets the backoff
// times used for subsequent attempts regardless of the current state.
//
// In general, this function should not be used.  Typical service or network
// outages result in a reasonable client reconnection strategy by default.
// However, if a previously unavailable network becomes available, this may be
// used to trigger an immediate reconnect.
//
// # Experimental
//
// Notice: This API is EXPERIMENTAL and may be changed or removed in a
// later release.
func (cc *ClientConn) ResetConnectBackoff() {
	cc.mu.Lock()
	conns := cc.conns
	cc.mu.Unlock()
	for ac := range conns {
		ac.resetConnectBackoff()
	}
}

// Close tears down the ClientConn and all underlying connections.
func (cc *ClientConn) Close() error {
	defer func() {
		cc.cancel()
		<-cc.csMgr.pubSub.Done()
	}()

	// Prevent calls to enter/exit idle immediately, and ensure we are not
	// currently entering/exiting idle mode.
	cc.idlenessMgr.Close()

	cc.mu.Lock()
	if cc.conns == nil {
		cc.mu.Unlock()
		return ErrClientConnClosing
	}

	conns := cc.conns
	cc.conns = nil
	cc.csMgr.updateState(connectivity.Shutdown)

	// We can safely unlock and continue to access all fields now as
	// cc.conns==nil, preventing any further operations on cc.
	cc.mu.Unlock()

	cc.resolverWrapper.close()
	// The order of closing matters here since the balancer wrapper assumes the
	// picker is closed before it is closed.
	cc.pickerWrapper.close()
	cc.balancerWrapper.close()

	<-cc.resolverWrapper.serializer.Done()
	<-cc.balancerWrapper.serializer.Done()
	var wg sync.WaitGroup
	for ac := range conns {
		wg.Add(1)
		go func(ac *addrConn) {
			defer wg.Done()
			ac.tearDown(ErrClientConnClosing)
		}(ac)
	}
	wg.Wait()
	cc.addTraceEvent("deleted")
	// TraceEvent needs to be called before RemoveEntry, as TraceEvent may add
	// trace reference to the entity being deleted, and thus prevent it from being
	// deleted right away.
	channelz.RemoveEntry(cc.channelz.ID)

	return nil
}

// addrConn is a network connection to a given address.
type addrConn struct {
	ctx    context.Context
	cancel context.CancelFunc

	cc     *ClientConn
	dopts  dialOptions
	acbw   *acBalancerWrapper
	scopts balancer.NewSubConnOptions

	// transport is set when there's a viable transport (note: ac state may not be READY as LB channel
	// health checking may require server to report healthy to set ac to READY), and is reset
	// to nil when the current transport should no longer be used to create a stream (e.g. after GoAway
	// is received, transport is closed, ac has been torn down).
	transport transport.ClientTransport // The current transport.

	// This mutex is used on the RPC path, so its usage should be minimized as
	// much as possible.
	// TODO: Find a lock-free way to retrieve the transport and state from the
	// addrConn.
	mu      sync.Mutex
	curAddr resolver.Address   // The current address.
	addrs   []resolver.Address // All addresses that the resolver resolved to.

	// Use updateConnectivityState for updating addrConn's connectivity state.
	state connectivity.State

	backoffIdx   int // Needs to be stateful for resetConnectBackoff.
	resetBackoff chan struct{}

	channelz *channelz.SubChannel
}

// Note: this requires a lock on ac.mu.
func (ac *addrConn) updateConnectivityState(s connectivity.State, lastErr error) {
	if ac.state == s {
		return
	}
	ac.state = s
	ac.channelz.ChannelMetrics.State.Store(&s)
	if lastErr == nil {
		channelz.Infof(logger, ac.channelz, "Subchannel Connectivity change to %v", s)
	} else {
		channelz.Infof(logger, ac.channelz, "Subchannel Connectivity change to %v, last error: %s", s, lastErr)
	}
	ac.acbw.updateState(s, ac.curAddr, lastErr)
}

// adjustParams updates parameters used to create transports upon
// receiving a GoAway.
func (ac *addrConn) adjustParams(r transport.GoAwayReason) {
	switch r {
	case transport.GoAwayTooManyPings:
		v := 2 * ac.dopts.copts.KeepaliveParams.Time
		ac.cc.mu.Lock()
		if v > ac.cc.mkp.Time {
			ac.cc.mkp.Time = v
		}
		ac.cc.mu.Unlock()
	}
}

// resetTransportAndUnlock unconditionally connects the addrConn.
//
// ac.mu must be held by the caller, and this function will guarantee it is released.
func (ac *addrConn) resetTransportAndUnlock() {
	acCtx := ac.ctx
	if acCtx.Err() != nil {
		ac.mu.Unlock()
		return
	}

	addrs := ac.addrs
	backoffFor := ac.dopts.bs.Backoff(ac.backoffIdx)
	// This will be the duration that dial gets to finish.
	dialDuration := minConnectTimeout
	if ac.dopts.minConnectTimeout != nil {
		dialDuration = ac.dopts.minConnectTimeout()
	}

	if dialDuration < backoffFor {
		// Give dial more time as we keep failing to connect.
		dialDuration = backoffFor
	}
	// We can potentially spend all the time trying the first address, and
	// if the server accepts the connection and then hangs, the following
	// addresses will never be tried.
	//
	// The spec doesn't mention what should be done for multiple addresses.
	// https://github.com/grpc/grpc/blob/master/doc/connection-backoff.md#proposed-backoff-algorithm
	connectDeadline := time.Now().Add(dialDuration)

	ac.updateConnectivityState(connectivity.Connecting, nil)
	ac.mu.Unlock()

	if err := ac.tryAllAddrs(acCtx, addrs, connectDeadline); err != nil {
		// TODO: #7534 - Move re-resolution requests into the pick_first LB policy
		// to ensure one resolution request per pass instead of per subconn failure.
		ac.cc.resolveNow(resolver.ResolveNowOptions{})
		ac.mu.Lock()
		if acCtx.Err() != nil {
			// addrConn was torn down.
			ac.mu.Unlock()
			return
		}
		// After exhausting all addresses, the addrConn enters
		// TRANSIENT_FAILURE.
		ac.updateConnectivityState(connectivity.TransientFailure, err)

		// Backoff.
		b := ac.resetBackoff
		ac.mu.Unlock()

		timer := time.NewTimer(backoffFor)
		select {
		case <-timer.C:
			ac.mu.Lock()
			ac.backoffIdx++
			ac.mu.Unlock()
		case <-b:
			timer.Stop()
		case <-acCtx.Done():
			timer.Stop()
			return
		}

		ac.mu.Lock()
		if acCtx.Err() == nil {
			ac.updateConnectivityState(connectivity.Idle, err)
		}
		ac.mu.Unlock()
		return
	}
	// Success; reset backoff.
	ac.mu.Lock()
	ac.backoffIdx = 0
	ac.mu.Unlock()
}

// tryAllAddrs tries to create a connection to the addresses, and stop when at
// the first successful one. It returns an error if no address was successfully
// connected, or updates ac appropriately with the new transport.
func (ac *addrConn) tryAllAddrs(ctx context.Context, addrs []resolver.Address, connectDeadline time.Time) error {
	var firstConnErr error
	for _, addr := range addrs {
		ac.channelz.ChannelMetrics.Target.Store(&addr.Addr)
		if ctx.Err() != nil {
			return errConnClosing
		}
		ac.mu.Lock()

		ac.cc.mu.RLock()
		ac.dopts.copts.KeepaliveParams = ac.cc.mkp
		ac.cc.mu.RUnlock()

		copts := ac.dopts.copts
		if ac.scopts.CredsBundle != nil {
			copts.CredsBundle = ac.scopts.CredsBundle
		}
		ac.mu.Unlock()

		channelz.Infof(logger, ac.channelz, "Subchannel picks a new address %q to connect", addr.Addr)

		err := ac.createTransport(ctx, addr, copts, connectDeadline)
		if err == nil {
			return nil
		}
		if firstConnErr == nil {
			firstConnErr = err
		}
		ac.cc.updateConnectionError(err)
	}

	// Couldn't connect to any address.
	return firstConnErr
}

// createTransport creates a connection to addr. It returns an error if the
// address was not successfully connected, or updates ac appropriately with the
// new transport.
func (ac *addrConn) createTransport(ctx context.Context, addr resolver.Address, copts transport.ConnectOptions, connectDeadline time.Time) error {
	addr.ServerName = ac.cc.getServerName(addr)
	hctx, hcancel := context.WithCancel(ctx)

	onClose := func(r transport.GoAwayReason) {
		ac.mu.Lock()
		defer ac.mu.Unlock()
		// adjust params based on GoAwayReason
		ac.adjustParams(r)
		if ctx.Err() != nil {
			// Already shut down or connection attempt canceled.  tearDown() or
			// updateAddrs() already cleared the transport and canceled hctx
			// via ac.ctx, and we expected this connection to be closed, so do
			// nothing here.
			return
		}
		hcancel()
		if ac.transport == nil {
			// We're still connecting to this address, which could error.  Do
			// not update the connectivity state or resolve; these will happen
			// at the end of the tryAllAddrs connection loop in the event of an
			// error.
			return
		}
		ac.transport = nil
		// Refresh the name resolver on any connection loss.
		ac.cc.resolveNow(resolver.ResolveNowOptions{})
		// Always go idle and wait for the LB policy to initiate a new
		// connection attempt.
		ac.updateConnectivityState(connectivity.Idle, nil)
	}

	connectCtx, cancel := context.WithDeadline(ctx, connectDeadline)
	defer cancel()
	copts.ChannelzParent = ac.channelz

	newTr, err := transport.NewClientTransport(connectCtx, ac.cc.ctx, addr, copts, onClose)
	if err != nil {
		if logger.V(2) {
			logger.Infof("Creating new client transport to %q: %v", addr, err)
		}
		// newTr is either nil, or closed.
		hcancel()
		channelz.Warningf(logger, ac.channelz, "grpc: addrConn.createTransport failed to connect to %s. Err: %v", addr, err)
		return err
	}

	ac.mu.Lock()
	defer ac.mu.Unlock()
	if ctx.Err() != nil {
		// This can happen if the subConn was removed while in `Connecting`
		// state. tearDown() would have set the state to `Shutdown`, but
		// would not have closed the transport since ac.transport would not
		// have been set at that point.
		//
		// We run this in a goroutine because newTr.Close() calls onClose()
		// inline, which requires locking ac.mu.
		//
		// The error we pass to Close() is immaterial since there are no open
		// streams at this point, so no trailers with error details will be sent
		// out. We just need to pass a non-nil error.
		//
		// This can also happen when updateAddrs is called during a connection
		// attempt.
		go newTr.Close(transport.ErrConnClosing)
		return nil
	}
	if hctx.Err() != nil {
		// onClose was already called for this connection, but the connection
		// was successfully established first.  Consider it a success and set
		// the new state to Idle.
		ac.updateConnectivityState(connectivity.Idle, nil)
		return nil
	}
	ac.curAddr = addr
	ac.transport = newTr
	ac.startHealthCheck(hctx) // Will set state to READY if appropriate.
	return nil
}

// startHealthCheck starts the health checking stream (RPC) to watch the health
// stats of this connection if health checking is requested and configured.
//
// LB channel health checking is enabled when all requirements below are met:
// 1. it is not disabled by the user with the WithDisableHealthCheck DialOption
// 2. internal.HealthCheckFunc is set by importing the grpc/health package
// 3. a service config with non-empty healthCheckConfig field is provided
// 4. the load balancer requests it
//
// It sets addrConn to READY if the health checking stream is not started.
//
// Caller must hold ac.mu.
func (ac *addrConn) startHealthCheck(ctx context.Context) {
	var healthcheckManagingState bool
	defer func() {
		if !healthcheckManagingState {
			ac.updateConnectivityState(connectivity.Ready, nil)
		}
	}()

	if ac.cc.dopts.disableHealthCheck {
		return
	}
	healthCheckConfig := ac.cc.healthCheckConfig()
	if healthCheckConfig == nil {
		return
	}
	if !ac.scopts.HealthCheckEnabled {
		return
	}
	healthCheckFunc := ac.cc.dopts.healthCheckFunc
	if healthCheckFunc == nil {
		// The health package is not imported to set health check function.
		//
		// TODO: add a link to the health check doc in the error message.
		channelz.Error(logger, ac.channelz, "Health check is requested but health check function is not set.")
		return
	}

	healthcheckManagingState = true

	// Set up the health check helper functions.
	currentTr := ac.transport
	newStream := func(method string) (any, error) {
		ac.mu.Lock()
		if ac.transport != currentTr {
			ac.mu.Unlock()
			return nil, status.Error(codes.Canceled, "the provided transport is no longer valid to use")
		}
		ac.mu.Unlock()
		return newNonRetryClientStream(ctx, &StreamDesc{ServerStreams: true}, method, currentTr, ac)
	}
	setConnectivityState := func(s connectivity.State, lastErr error) {
		ac.mu.Lock()
		defer ac.mu.Unlock()
		if ac.transport != currentTr {
			return
		}
		ac.updateConnectivityState(s, lastErr)
	}
	// Start the health checking stream.
	go func() {
		err := ac.cc.dopts.healthCheckFunc(ctx, newStream, setConnectivityState, healthCheckConfig.ServiceName)
		if err != nil {
			if status.Code(err) == codes.Unimplemented {
				channelz.Error(logger, ac.channelz, "Subchannel health check is unimplemented at server side, thus health check is disabled")
			} else {
				channelz.Errorf(logger, ac.channelz, "Health checking failed: %v", err)
			}
		}
	}()
}

func (ac *addrConn) resetConnectBackoff() {
	ac.mu.Lock()
	close(ac.resetBackoff)
	ac.backoffIdx = 0
	ac.resetBackoff = make(chan struct{})
	ac.mu.Unlock()
}

// getReadyTransport returns the transport if ac's state is READY or nil if not.
func (ac *addrConn) getReadyTransport() transport.ClientTransport {
	ac.mu.Lock()
	defer ac.mu.Unlock()
	if ac.state == connectivity.Ready {
		return ac.transport
	}
	return nil
}

// tearDown starts to tear down the addrConn.
//
// Note that tearDown doesn't remove ac from ac.cc.conns, so the addrConn struct
// will leak. In most cases, call cc.removeAddrConn() instead.
func (ac *addrConn) tearDown(err error) {
	ac.mu.Lock()
	if ac.state == connectivity.Shutdown {
		ac.mu.Unlock()
		return
	}
	curTr := ac.transport
	ac.transport = nil
	// We have to set the state to Shutdown before anything else to prevent races
	// between setting the state and logic that waits on context cancellation / etc.
	ac.updateConnectivityState(connectivity.Shutdown, nil)
	ac.cancel()
	ac.curAddr = resolver.Address{}

	channelz.AddTraceEvent(logger, ac.channelz, 0, &channelz.TraceEvent{
		Desc:     "Subchannel deleted",
		Severity: channelz.CtInfo,
		Parent: &channelz.TraceEvent{
			Desc:     fmt.Sprintf("Subchannel(id:%d) deleted", ac.channelz.ID),
			Severity: channelz.CtInfo,
		},
	})
	// TraceEvent needs to be called before RemoveEntry, as TraceEvent may add
	// trace reference to the entity being deleted, and thus prevent it from
	// being deleted right away.
	channelz.RemoveEntry(ac.channelz.ID)
	ac.mu.Unlock()

	// We have to release the lock before the call to GracefulClose/Close here
	// because both of them call onClose(), which requires locking ac.mu.
	if curTr != nil {
		if err == errConnDrain {
			// Close the transport gracefully when the subConn is being shutdown.
			//
			// GracefulClose() may be executed multiple times if:
			// - multiple GoAway frames are received from the server
			// - there are concurrent name resolver or balancer triggered
			//   address removal and GoAway
			curTr.GracefulClose()
		} else {
			// Hard close the transport when the channel is entering idle or is
			// being shutdown. In the case where the channel is being shutdown,
			// closing of transports is also taken care of by cancellation of cc.ctx.
			// But in the case where the channel is entering idle, we need to
			// explicitly close the transports here. Instead of distinguishing
			// between these two cases, it is simpler to close the transport
			// unconditionally here.
			curTr.Close(err)
		}
	}
}

type retryThrottler struct {
	max    float64
	thresh float64
	ratio  float64

	mu     sync.Mutex
	tokens float64 // TODO(dfawley): replace with atomic and remove lock.
}

// throttle subtracts a retry token from the pool and returns whether a retry
// should be throttled (disallowed) based upon the retry throttling policy in
// the service config.
func (rt *retryThrottler) throttle() bool {
	if rt == nil {
		return false
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.tokens--
	if rt.tokens < 0 {
		rt.tokens = 0
	}
	return rt.tokens <= rt.thresh
}

func (rt *retryThrottler) successfulRPC() {
	if rt == nil {
		return
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.tokens += rt.ratio
	if rt.tokens > rt.max {
		rt.tokens = rt.max
	}
}

func (ac *addrConn) incrCallsStarted() {
	ac.channelz.ChannelMetrics.CallsStarted.Add(1)
	ac.channelz.ChannelMetrics.LastCallStartedTimestamp.Store(time.Now().UnixNano())
}

func (ac *addrConn) incrCallsSucceeded() {
	ac.channelz.ChannelMetrics.CallsSucceeded.Add(1)
}

func (ac *addrConn) incrCallsFailed() {
	ac.channelz.ChannelMetrics.CallsFailed.Add(1)
}

// ErrClientConnTimeout indicates that the ClientConn cannot establish the
// underlying connections within the specified timeout.
//
// Deprecated: This error is never returned by grpc and should not be
// referenced by users.
var ErrClientConnTimeout = errors.New("grpc: timed out when dialing")

// getResolver finds the scheme in the cc's resolvers or the global registry.
// scheme should always be lowercase (typically by virtue of url.Parse()
// performing proper RFC3986 behavior).
func (cc *ClientConn) getResolver(scheme string) resolver.Builder {
	for _, rb := range cc.dopts.resolvers {
		if scheme == rb.Scheme() {
			return rb
		}
	}
	return resolver.Get(scheme)
}

func (cc *ClientConn) updateConnectionError(err error) {
	cc.lceMu.Lock()
	cc.lastConnectionError = err
	cc.lceMu.Unlock()
}

func (cc *ClientConn) connectionError() error {
	cc.lceMu.Lock()
	defer cc.lceMu.Unlock()
	return cc.lastConnectionError
}

// initParsedTargetAndResolverBuilder parses the user's dial target and stores
// the parsed target in `cc.parsedTarget`.
//
// The resolver to use is determined based on the scheme in the parsed target
// and the same is stored in `cc.resolverBuilder`.
//
// Doesn't grab cc.mu as this method is expected to be called only at Dial time.
func (cc *ClientConn) initParsedTargetAndResolverBuilder() error {
	logger.Infof("original dial target is: %q", cc.target)

	var rb resolver.Builder
	parsedTarget, err := parseTarget(cc.target)
	if err == nil {
		rb = cc.getResolver(parsedTarget.URL.Scheme)
		if rb != nil {
			cc.parsedTarget = parsedTarget
			cc.resolverBuilder = rb
			return nil
		}
	}

	// We are here because the user's dial target did not contain a scheme or
	// specified an unregistered scheme. We should fallback to the default
	// scheme, except when a custom dialer is specified in which case, we should
	// always use passthrough scheme. For either case, we need to respect any overridden
	// global defaults set by the user.
	defScheme := cc.dopts.defaultScheme
	if internal.UserSetDefaultScheme {
		defScheme = resolver.GetDefaultScheme()
	}

	canonicalTarget := defScheme + ":///" + cc.target

	parsedTarget, err = parseTarget(canonicalTarget)
	if err != nil {
		return err
	}
	rb = cc.getResolver(parsedTarget.URL.Scheme)
	if rb == nil {
		return fmt.Errorf("could not get resolver for default scheme: %q", parsedTarget.URL.Scheme)
	}
	cc.parsedTarget = parsedTarget
	cc.resolverBuilder = rb
	return nil
}

// parseTarget uses RFC 3986 semantics to parse the given target into a
// resolver.Target struct containing url. Query params are stripped from the
// endpoint.
func parseTarget(target string) (resolver.Target, error) {
	u, err := url.Parse(target)
	if err != nil {
		return resolver.Target{}, err
	}

	return resolver.Target{URL: *u}, nil
}

// encodeAuthority escapes the authority string based on valid chars defined in
// https://datatracker.ietf.org/doc/html/rfc3986#section-3.2.
func encodeAuthority(authority string) string {
	const upperhex = "0123456789ABCDEF"

	// Return for characters that must be escaped as per
	// Valid chars are mentioned here:
	// https://datatracker.ietf.org/doc/html/rfc3986#section-3.2
	shouldEscape := func(c byte) bool {
		// Alphanum are always allowed.
		if 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' {
			return false
		}
		switch c {
		case '-', '_', '.', '~': // Unreserved characters
			return false
		case '!', '$', '&', '\'', '(', ')', '*', '+', ',', ';', '=': // Subdelim characters
			return false
		case ':', '[', ']', '@': // Authority related delimiters
			return false
		}
		// Everything else must be escaped.
		return true
	}

	hexCount := 0
	for i := 0; i < len(authority); i++ {
		c := authority[i]
		if shouldEscape(c) {
			hexCount++
		}
	}

	if hexCount == 0 {
		return authority
	}

	required := len(authority) + 2*hexCount
	t := make([]byte, required)

	j := 0
	// This logic is a barebones version of escape in the go net/url library.
	for i := 0; i < len(authority); i++ {
		switch c := authority[i]; {
		case shouldEscape(c):
			t[j] = '%'
			t[j+1] = upperhex[c>>4]
			t[j+2] = upperhex[c&15]
			j += 3
		default:
			t[j] = authority[i]
			j++
		}
	}
	return string(t)
}

// Determine channel authority. The order of precedence is as follows:
// - user specified authority override using `WithAuthority` dial option
// - creds' notion of server name for the authentication handshake
// - endpoint from dial target of the form "scheme://[authority]/endpoint"
//
// Stores the determined authority in `cc.authority`.
//
// Returns a non-nil error if the authority returned by the transport
// credentials do not match the authority configured through the dial option.
//
// Doesn't grab cc.mu as this method is expected to be called only at Dial time.
func (cc *ClientConn) initAuthority() error {
	dopts := cc.dopts
	// Historically, we had two options for users to specify the serverName or
	// authority for a channel. One was through the transport credentials
	// (either in its constructor, or through the OverrideServerName() method).
	// The other option (for cases where WithInsecure() dial option was used)
	// was to use the WithAuthority() dial option.
	//
	// A few things have changed since:
	// - `insecure` package with an implementation of the `TransportCredentials`
	//   interface for the insecure case
	// - WithAuthority() dial option support for secure credentials
	authorityFromCreds := ""
	if creds := dopts.copts.TransportCredentials; creds != nil && creds.Info().ServerName != "" {
		authorityFromCreds = creds.Info().ServerName
	}
	authorityFromDialOption := dopts.authority
	if (authorityFromCreds != "" && authorityFromDialOption != "") && authorityFromCreds != authorityFromDialOption {
		return fmt.Errorf("ClientConn's authority from transport creds %q and dial option %q don't match", authorityFromCreds, authorityFromDialOption)
	}

	endpoint := cc.parsedTarget.Endpoint()
	if authorityFromDialOption != "" {
		cc.authority = authorityFromDialOption
	} else if authorityFromCreds != "" {
		cc.authority = authorityFromCreds
	} else if auth, ok := cc.resolverBuilder.(resolver.AuthorityOverrider); ok {
		cc.authority = auth.OverrideAuthority(cc.parsedTarget)
	} else if strings.HasPrefix(endpoint, ":") {
		cc.authority = "localhost" + endpoint
	} else {
		cc.authority = encodeAuthority(endpoint)
	}
	return nil
}
