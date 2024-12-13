/*
 *
 * Copyright 2018 gRPC authors.
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
	"net"
	"net/url"
	"time"

	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/channelz"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/internal"
	internalbackoff "google.golang.org/grpc/internal/backoff"
	"google.golang.org/grpc/internal/binarylog"
	"google.golang.org/grpc/internal/transport"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/mem"
	"google.golang.org/grpc/resolver"
	"google.golang.org/grpc/stats"
)

const (
	// https://github.com/grpc/proposal/blob/master/A6-client-retries.md#limits-on-retries-and-hedges
	defaultMaxCallAttempts = 5
)

func init() {
	internal.AddGlobalDialOptions = func(opt ...DialOption) {
		globalDialOptions = append(globalDialOptions, opt...)
	}
	internal.ClearGlobalDialOptions = func() {
		globalDialOptions = nil
	}
	internal.AddGlobalPerTargetDialOptions = func(opt any) {
		if ptdo, ok := opt.(perTargetDialOption); ok {
			globalPerTargetDialOptions = append(globalPerTargetDialOptions, ptdo)
		}
	}
	internal.ClearGlobalPerTargetDialOptions = func() {
		globalPerTargetDialOptions = nil
	}
	internal.WithBinaryLogger = withBinaryLogger
	internal.JoinDialOptions = newJoinDialOption
	internal.DisableGlobalDialOptions = newDisableGlobalDialOptions
	internal.WithBufferPool = withBufferPool
}

// dialOptions configure a Dial call. dialOptions are set by the DialOption
// values passed to Dial.
type dialOptions struct {
	unaryInt  UnaryClientInterceptor
	streamInt StreamClientInterceptor

	chainUnaryInts  []UnaryClientInterceptor
	chainStreamInts []StreamClientInterceptor

	cp                          Compressor
	dc                          Decompressor
	bs                          internalbackoff.Strategy
	block                       bool
	returnLastError             bool
	timeout                     time.Duration
	authority                   string
	binaryLogger                binarylog.Logger
	copts                       transport.ConnectOptions
	callOptions                 []CallOption
	channelzParent              channelz.Identifier
	disableServiceConfig        bool
	disableRetry                bool
	disableHealthCheck          bool
	healthCheckFunc             internal.HealthChecker
	minConnectTimeout           func() time.Duration
	defaultServiceConfig        *ServiceConfig // defaultServiceConfig is parsed from defaultServiceConfigRawJSON.
	defaultServiceConfigRawJSON *string
	resolvers                   []resolver.Builder
	idleTimeout                 time.Duration
	defaultScheme               string
	maxCallAttempts             int
}

// DialOption configures how we set up the connection.
type DialOption interface {
	apply(*dialOptions)
}

var globalDialOptions []DialOption

// perTargetDialOption takes a parsed target and returns a dial option to apply.
//
// This gets called after NewClient() parses the target, and allows per target
// configuration set through a returned DialOption. The DialOption will not take
// effect if specifies a resolver builder, as that Dial Option is factored in
// while parsing target.
type perTargetDialOption interface {
	// DialOption returns a Dial Option to apply.
	DialOptionForTarget(parsedTarget url.URL) DialOption
}

var globalPerTargetDialOptions []perTargetDialOption

// EmptyDialOption does not alter the dial configuration. It can be embedded in
// another structure to build custom dial options.
//
// # Experimental
//
// Notice: This type is EXPERIMENTAL and may be changed or removed in a
// later release.
type EmptyDialOption struct{}

func (EmptyDialOption) apply(*dialOptions) {}

type disableGlobalDialOptions struct{}

func (disableGlobalDialOptions) apply(*dialOptions) {}

// newDisableGlobalDialOptions returns a DialOption that prevents the ClientConn
// from applying the global DialOptions (set via AddGlobalDialOptions).
func newDisableGlobalDialOptions() DialOption {
	return &disableGlobalDialOptions{}
}

// funcDialOption wraps a function that modifies dialOptions into an
// implementation of the DialOption interface.
type funcDialOption struct {
	f func(*dialOptions)
}

func (fdo *funcDialOption) apply(do *dialOptions) {
	fdo.f(do)
}

func newFuncDialOption(f func(*dialOptions)) *funcDialOption {
	return &funcDialOption{
		f: f,
	}
}

type joinDialOption struct {
	opts []DialOption
}

func (jdo *joinDialOption) apply(do *dialOptions) {
	for _, opt := range jdo.opts {
		opt.apply(do)
	}
}

func newJoinDialOption(opts ...DialOption) DialOption {
	return &joinDialOption{opts: opts}
}

// WithSharedWriteBuffer allows reusing per-connection transport write buffer.
// If this option is set to true every connection will release the buffer after
// flushing the data on the wire.
//
// # Experimental
//
// Notice: This API is EXPERIMENTAL and may be changed or removed in a
// later release.
func WithSharedWriteBuffer(val bool) DialOption {
	return newFuncDialOption(func(o *dialOptions) {
		o.copts.SharedWriteBuffer = val
	})
}

// WithWriteBufferSize determines how much data can be batched before doing a
// write on the wire. The default value for this buffer is 32KB.
//
// Zero or negative values will disable the write buffer such that each write
// will be on underlying connection. Note: A Send call may not directly
// translate to a write.
func WithWriteBufferSize(s int) DialOption {
	return newFuncDialOption(func(o *dialOptions) {
		o.copts.WriteBufferSize = s
	})
}

// WithReadBufferSize lets you set the size of read buffer, this determines how
// much data can be read at most for each read syscall.
//
// The default value for this buffer is 32KB. Zero or negative values will
// disable read buffer for a connection so data framer can access the
// underlying conn directly.
func WithReadBufferSize(s int) DialOption {
	return newFuncDialOption(func(o *dialOptions) {
		o.copts.ReadBufferSize = s
	})
}

// WithInitialWindowSize returns a DialOption which sets the value for initial
// window size on a stream. The lower bound for window size is 64K and any value
// smaller than that will be ignored.
func WithInitialWindowSize(s int32) DialOption {
	return newFuncDialOption(func(o *dialOptions) {
		o.copts.InitialWindowSize = s
	})
}

// WithInitialConnWindowSize returns a DialOption which sets the value for
// initial window size on a connection. The lower bound for window size is 64K
// and any value smaller than that will be ignored.
func WithInitialConnWindowSize(s int32) DialOption {
	return newFuncDialOption(func(o *dialOptions) {
		o.copts.InitialConnWindowSize = s
	})
}

// WithMaxMsgSize returns a DialOption which sets the maximum message size the
// client can receive.
//
// Deprecated: use WithDefaultCallOptions(MaxCallRecvMsgSize(s)) instead.  Will
// be supported throughout 1.x.
func WithMaxMsgSize(s int) DialOption {
	return WithDefaultCallOptions(MaxCallRecvMsgSize(s))
}

// WithDefaultCallOptions returns a DialOption which sets the default
// CallOptions for calls over the connection.
func WithDefaultCallOptions(cos ...CallOption) DialOption {
	return newFuncDialOption(func(o *dialOptions) {
		o.callOptions = append(o.callOptions, cos...)
	})
}

// WithCodec returns a DialOption which sets a codec for message marshaling and
// unmarshaling.
//
// Deprecated: use WithDefaultCallOptions(ForceCodec(_)) instead.  Will be
// supported throughout 1.x.
func WithCodec(c Codec) DialOption {
	return WithDefaultCallOptions(CallCustomCodec(c))
}

// WithCompressor returns a DialOption which sets a Compressor to use for
// message compression. It has lower priority than the compressor set by the
// UseCompressor CallOption.
//
// Deprecated: use UseCompressor instead.  Will be supported throughout 1.x.
func WithCompressor(cp Compressor) DialOption {
	return newFuncDialOption(func(o *dialOptions) {
		o.cp = cp
	})
}

// WithDecompressor returns a DialOption which sets a Decompressor to use for
// incoming message decompression.  If incoming response messages are encoded
// using the decompressor's Type(), it will be used.  Otherwise, the message
// encoding will be used to look up the compressor registered via
// encoding.RegisterCompressor, which will then be used to decompress the
// message.  If no compressor is registered for the encoding, an Unimplemented
// status error will be returned.
//
// Deprecated: use encoding.RegisterCompressor instead.  Will be supported
// throughout 1.x.
func WithDecompressor(dc Decompressor) DialOption {
	return newFuncDialOption(func(o *dialOptions) {
		o.dc = dc
	})
}

// WithConnectParams configures the ClientConn to use the provided ConnectParams
// for creating and maintaining connections to servers.
//
// The backoff configuration specified as part of the ConnectParams overrides
// all defaults specified in
// https://github.com/grpc/grpc/blob/master/doc/connection-backoff.md. Consider
// using the backoff.DefaultConfig as a base, in cases where you want to
// override only a subset of the backoff configuration.
func WithConnectParams(p ConnectParams) DialOption {
	return newFuncDialOption(func(o *dialOptions) {
		o.bs = internalbackoff.Exponential{Config: p.Backoff}
		o.minConnectTimeout = func() time.Duration {
			return p.MinConnectTimeout
		}
	})
}

// WithBackoffMaxDelay configures the dialer to use the provided maximum delay
// when backing off after failed connection attempts.
//
// Deprecated: use WithConnectParams instead. Will be supported throughout 1.x.
func WithBackoffMaxDelay(md time.Duration) DialOption {
	return WithBackoffConfig(BackoffConfig{MaxDelay: md})
}

// WithBackoffConfig configures the dialer to use the provided backoff
// parameters after connection failures.
//
// Deprecated: use WithConnectParams instead. Will be supported throughout 1.x.
func WithBackoffConfig(b BackoffConfig) DialOption {
	bc := backoff.DefaultConfig
	bc.MaxDelay = b.MaxDelay
	return withBackoff(internalbackoff.Exponential{Config: bc})
}

// withBackoff sets the backoff strategy used for connectRetryNum after a failed
// connection attempt.
//
// This can be exported if arbitrary backoff strategies are allowed by gRPC.
func withBackoff(bs internalbackoff.Strategy) DialOption {
	return newFuncDialOption(func(o *dialOptions) {
		o.bs = bs
	})
}

// WithBlock returns a DialOption which makes callers of Dial block until the
// underlying connection is up. Without this, Dial returns immediately and
// connecting the server happens in background.
//
// Use of this feature is not recommended.  For more information, please see:
// https://github.com/grpc/grpc-go/blob/master/Documentation/anti-patterns.md
//
// Deprecated: this DialOption is not supported by NewClient.
// Will be supported throughout 1.x.
func WithBlock() DialOption {
	return newFuncDialOption(func(o *dialOptions) {
		o.block = true
	})
}

// WithReturnConnectionError returns a DialOption which makes the client connection
// return a string containing both the last connection error that occurred and
// the context.DeadlineExceeded error.
// Implies WithBlock()
//
// Use of this feature is not recommended.  For more information, please see:
// https://github.com/grpc/grpc-go/blob/master/Documentation/anti-patterns.md
//
// Deprecated: this DialOption is not supported by NewClient.
// Will be supported throughout 1.x.
func WithReturnConnectionError() DialOption {
	return newFuncDialOption(func(o *dialOptions) {
		o.block = true
		o.returnLastError = true
	})
}

// WithInsecure returns a DialOption which disables transport security for this
// ClientConn. Under the hood, it uses insecure.NewCredentials().
//
// Note that using this DialOption with per-RPC credentials (through
// WithCredentialsBundle or WithPerRPCCredentials) which require transport
// security is incompatible and will cause grpc.Dial() to fail.
//
// Deprecated: use WithTransportCredentials and insecure.NewCredentials()
// instead. Will be supported throughout 1.x.
func WithInsecure() DialOption {
	return newFuncDialOption(func(o *dialOptions) {
		o.copts.TransportCredentials = insecure.NewCredentials()
	})
}

// WithNoProxy returns a DialOption which disables the use of proxies for this
// ClientConn. This is ignored if WithDialer or WithContextDialer are used.
//
// # Experimental
//
// Notice: This API is EXPERIMENTAL and may be changed or removed in a
// later release.
func WithNoProxy() DialOption {
	return newFuncDialOption(func(o *dialOptions) {
		o.copts.UseProxy = false
	})
}

// WithTransportCredentials returns a DialOption which configures a connection
// level security credentials (e.g., TLS/SSL). This should not be used together
// with WithCredentialsBundle.
func WithTransportCredentials(creds credentials.TransportCredentials) DialOption {
	return newFuncDialOption(func(o *dialOptions) {
		o.copts.TransportCredentials = creds
	})
}

// WithPerRPCCredentials returns a DialOption which sets credentials and places
// auth state on each outbound RPC.
func WithPerRPCCredentials(creds credentials.PerRPCCredentials) DialOption {
	return newFuncDialOption(func(o *dialOptions) {
		o.copts.PerRPCCredentials = append(o.copts.PerRPCCredentials, creds)
	})
}

// WithCredentialsBundle returns a DialOption to set a credentials bundle for
// the ClientConn.WithCreds. This should not be used together with
// WithTransportCredentials.
//
// # Experimental
//
// Notice: This API is EXPERIMENTAL and may be changed or removed in a
// later release.
func WithCredentialsBundle(b credentials.Bundle) DialOption {
	return newFuncDialOption(func(o *dialOptions) {
		o.copts.CredsBundle = b
	})
}

// WithTimeout returns a DialOption that configures a timeout for dialing a
// ClientConn initially. This is valid if and only if WithBlock() is present.
//
// Deprecated: this DialOption is not supported by NewClient.
// Will be supported throughout 1.x.
func WithTimeout(d time.Duration) DialOption {
	return newFuncDialOption(func(o *dialOptions) {
		o.timeout = d
	})
}

// WithContextDialer returns a DialOption that sets a dialer to create
// connections. If FailOnNonTempDialError() is set to true, and an error is
// returned by f, gRPC checks the error's Temporary() method to decide if it
// should try to reconnect to the network address.
//
// Note: All supported releases of Go (as of December 2023) override the OS
// defaults for TCP keepalive time and interval to 15s. To enable TCP keepalive
// with OS defaults for keepalive time and interval, use a net.Dialer that sets
// the KeepAlive field to a negative value, and sets the SO_KEEPALIVE socket
// option to true from the Control field. For a concrete example of how to do
// this, see internal.NetDialerWithTCPKeepalive().
//
// For more information, please see [issue 23459] in the Go GitHub repo.
//
// [issue 23459]: https://github.com/golang/go/issues/23459
func WithContextDialer(f func(context.Context, string) (net.Conn, error)) DialOption {
	return newFuncDialOption(func(o *dialOptions) {
		o.copts.Dialer = f
	})
}

func init() {
	internal.WithHealthCheckFunc = withHealthCheckFunc
}

// WithDialer returns a DialOption that specifies a function to use for dialing
// network addresses. If FailOnNonTempDialError() is set to true, and an error
// is returned by f, gRPC checks the error's Temporary() method to decide if it
// should try to reconnect to the network address.
//
// Deprecated: use WithContextDialer instead.  Will be supported throughout
// 1.x.
func WithDialer(f func(string, time.Duration) (net.Conn, error)) DialOption {
	return WithContextDialer(
		func(ctx context.Context, addr string) (net.Conn, error) {
			if deadline, ok := ctx.Deadline(); ok {
				return f(addr, time.Until(deadline))
			}
			return f(addr, 0)
		})
}

// WithStatsHandler returns a DialOption that specifies the stats handler for
// all the RPCs and underlying network connections in this ClientConn.
func WithStatsHandler(h stats.Handler) DialOption {
	return newFuncDialOption(func(o *dialOptions) {
		if h == nil {
			logger.Error("ignoring nil parameter in grpc.WithStatsHandler ClientOption")
			// Do not allow a nil stats handler, which would otherwise cause
			// panics.
			return
		}
		o.copts.StatsHandlers = append(o.copts.StatsHandlers, h)
	})
}

// withBinaryLogger returns a DialOption that specifies the binary logger for
// this ClientConn.
func withBinaryLogger(bl binarylog.Logger) DialOption {
	return newFuncDialOption(func(o *dialOptions) {
		o.binaryLogger = bl
	})
}

// FailOnNonTempDialError returns a DialOption that specifies if gRPC fails on
// non-temporary dial errors. If f is true, and dialer returns a non-temporary
// error, gRPC will fail the connection to the network address and won't try to
// reconnect. The default value of FailOnNonTempDialError is false.
//
// FailOnNonTempDialError only affects the initial dial, and does not do
// anything useful unless you are also using WithBlock().
//
// Use of this feature is not recommended.  For more information, please see:
// https://github.com/grpc/grpc-go/blob/master/Documentation/anti-patterns.md
//
// Deprecated: this DialOption is not supported by NewClient.
// This API may be changed or removed in a
// later release.
func FailOnNonTempDialError(f bool) DialOption {
	return newFuncDialOption(func(o *dialOptions) {
		o.copts.FailOnNonTempDialError = f
	})
}

// WithUserAgent returns a DialOption that specifies a user agent string for all
// the RPCs.
func WithUserAgent(s string) DialOption {
	return newFuncDialOption(func(o *dialOptions) {
		o.copts.UserAgent = s + " " + grpcUA
	})
}

// WithKeepaliveParams returns a DialOption that specifies keepalive parameters
// for the client transport.
//
// Keepalive is disabled by default.
func WithKeepaliveParams(kp keepalive.ClientParameters) DialOption {
	if kp.Time < internal.KeepaliveMinPingTime {
		logger.Warningf("Adjusting keepalive ping interval to minimum period of %v", internal.KeepaliveMinPingTime)
		kp.Time = internal.KeepaliveMinPingTime
	}
	return newFuncDialOption(func(o *dialOptions) {
		o.copts.KeepaliveParams = kp
	})
}

// WithUnaryInterceptor returns a DialOption that specifies the interceptor for
// unary RPCs.
func WithUnaryInterceptor(f UnaryClientInterceptor) DialOption {
	return newFuncDialOption(func(o *dialOptions) {
		o.unaryInt = f
	})
}

// WithChainUnaryInterceptor returns a DialOption that specifies the chained
// interceptor for unary RPCs. The first interceptor will be the outer most,
// while the last interceptor will be the inner most wrapper around the real call.
// All interceptors added by this method will be chained, and the interceptor
// defined by WithUnaryInterceptor will always be prepended to the chain.
func WithChainUnaryInterceptor(interceptors ...UnaryClientInterceptor) DialOption {
	return newFuncDialOption(func(o *dialOptions) {
		o.chainUnaryInts = append(o.chainUnaryInts, interceptors...)
	})
}

// WithStreamInterceptor returns a DialOption that specifies the interceptor for
// streaming RPCs.
func WithStreamInterceptor(f StreamClientInterceptor) DialOption {
	return newFuncDialOption(func(o *dialOptions) {
		o.streamInt = f
	})
}

// WithChainStreamInterceptor returns a DialOption that specifies the chained
// interceptor for streaming RPCs. The first interceptor will be the outer most,
// while the last interceptor will be the inner most wrapper around the real call.
// All interceptors added by this method will be chained, and the interceptor
// defined by WithStreamInterceptor will always be prepended to the chain.
func WithChainStreamInterceptor(interceptors ...StreamClientInterceptor) DialOption {
	return newFuncDialOption(func(o *dialOptions) {
		o.chainStreamInts = append(o.chainStreamInts, interceptors...)
	})
}

// WithAuthority returns a DialOption that specifies the value to be used as the
// :authority pseudo-header and as the server name in authentication handshake.
func WithAuthority(a string) DialOption {
	return newFuncDialOption(func(o *dialOptions) {
		o.authority = a
	})
}

// WithChannelzParentID returns a DialOption that specifies the channelz ID of
// current ClientConn's parent. This function is used in nested channel creation
// (e.g. grpclb dial).
//
// # Experimental
//
// Notice: This API is EXPERIMENTAL and may be changed or removed in a
// later release.
func WithChannelzParentID(c channelz.Identifier) DialOption {
	return newFuncDialOption(func(o *dialOptions) {
		o.channelzParent = c
	})
}

// WithDisableServiceConfig returns a DialOption that causes gRPC to ignore any
// service config provided by the resolver and provides a hint to the resolver
// to not fetch service configs.
//
// Note that this dial option only disables service config from resolver. If
// default service config is provided, gRPC will use the default service config.
func WithDisableServiceConfig() DialOption {
	return newFuncDialOption(func(o *dialOptions) {
		o.disableServiceConfig = true
	})
}

// WithDefaultServiceConfig returns a DialOption that configures the default
// service config, which will be used in cases where:
//
// 1. WithDisableServiceConfig is also used, or
//
// 2. The name resolver does not provide a service config or provides an
// invalid service config.
//
// The parameter s is the JSON representation of the default service config.
// For more information about service configs, see:
// https://github.com/grpc/grpc/blob/master/doc/service_config.md
// For a simple example of usage, see:
// examples/features/load_balancing/client/main.go
func WithDefaultServiceConfig(s string) DialOption {
	return newFuncDialOption(func(o *dialOptions) {
		o.defaultServiceConfigRawJSON = &s
	})
}

// WithDisableRetry returns a DialOption that disables retries, even if the
// service config enables them.  This does not impact transparent retries, which
// will happen automatically if no data is written to the wire or if the RPC is
// unprocessed by the remote server.
func WithDisableRetry() DialOption {
	return newFuncDialOption(func(o *dialOptions) {
		o.disableRetry = true
	})
}

// MaxHeaderListSizeDialOption is a DialOption that specifies the maximum
// (uncompressed) size of header list that the client is prepared to accept.
type MaxHeaderListSizeDialOption struct {
	MaxHeaderListSize uint32
}

func (o MaxHeaderListSizeDialOption) apply(do *dialOptions) {
	do.copts.MaxHeaderListSize = &o.MaxHeaderListSize
}

// WithMaxHeaderListSize returns a DialOption that specifies the maximum
// (uncompressed) size of header list that the client is prepared to accept.
func WithMaxHeaderListSize(s uint32) DialOption {
	return MaxHeaderListSizeDialOption{
		MaxHeaderListSize: s,
	}
}

// WithDisableHealthCheck disables the LB channel health checking for all
// SubConns of this ClientConn.
//
// # Experimental
//
// Notice: This API is EXPERIMENTAL and may be changed or removed in a
// later release.
func WithDisableHealthCheck() DialOption {
	return newFuncDialOption(func(o *dialOptions) {
		o.disableHealthCheck = true
	})
}

// withHealthCheckFunc replaces the default health check function with the
// provided one. It makes tests easier to change the health check function.
//
// For testing purpose only.
func withHealthCheckFunc(f internal.HealthChecker) DialOption {
	return newFuncDialOption(func(o *dialOptions) {
		o.healthCheckFunc = f
	})
}

func defaultDialOptions() dialOptions {
	return dialOptions{
		copts: transport.ConnectOptions{
			ReadBufferSize:  defaultReadBufSize,
			WriteBufferSize: defaultWriteBufSize,
			UseProxy:        true,
			UserAgent:       grpcUA,
			BufferPool:      mem.DefaultBufferPool(),
		},
		bs:              internalbackoff.DefaultExponential,
		healthCheckFunc: internal.HealthCheckFunc,
		idleTimeout:     30 * time.Minute,
		defaultScheme:   "dns",
		maxCallAttempts: defaultMaxCallAttempts,
	}
}

// withMinConnectDeadline specifies the function that clientconn uses to
// get minConnectDeadline. This can be used to make connection attempts happen
// faster/slower.
//
// For testing purpose only.
func withMinConnectDeadline(f func() time.Duration) DialOption {
	return newFuncDialOption(func(o *dialOptions) {
		o.minConnectTimeout = f
	})
}

// withDefaultScheme is used to allow Dial to use "passthrough" as the default
// name resolver, while NewClient uses "dns" otherwise.
func withDefaultScheme(s string) DialOption {
	return newFuncDialOption(func(o *dialOptions) {
		o.defaultScheme = s
	})
}

// WithResolvers allows a list of resolver implementations to be registered
// locally with the ClientConn without needing to be globally registered via
// resolver.Register.  They will be matched against the scheme used for the
// current Dial only, and will take precedence over the global registry.
//
// # Experimental
//
// Notice: This API is EXPERIMENTAL and may be changed or removed in a
// later release.
func WithResolvers(rs ...resolver.Builder) DialOption {
	return newFuncDialOption(func(o *dialOptions) {
		o.resolvers = append(o.resolvers, rs...)
	})
}

// WithIdleTimeout returns a DialOption that configures an idle timeout for the
// channel. If the channel is idle for the configured timeout, i.e there are no
// ongoing RPCs and no new RPCs are initiated, the channel will enter idle mode
// and as a result the name resolver and load balancer will be shut down. The
// channel will exit idle mode when the Connect() method is called or when an
// RPC is initiated.
//
// A default timeout of 30 minutes will be used if this dial option is not set
// at dial time and idleness can be disabled by passing a timeout of zero.
//
// # Experimental
//
// Notice: This API is EXPERIMENTAL and may be changed or removed in a
// later release.
func WithIdleTimeout(d time.Duration) DialOption {
	return newFuncDialOption(func(o *dialOptions) {
		o.idleTimeout = d
	})
}

// WithMaxCallAttempts returns a DialOption that configures the maximum number
// of attempts per call (including retries and hedging) using the channel.
// Service owners may specify a higher value for these parameters, but higher
// values will be treated as equal to the maximum value by the client
// implementation. This mitigates security concerns related to the service
// config being transferred to the client via DNS.
//
// A value of 5 will be used if this dial option is not set or n < 2.
func WithMaxCallAttempts(n int) DialOption {
	return newFuncDialOption(func(o *dialOptions) {
		if n < 2 {
			n = defaultMaxCallAttempts
		}
		o.maxCallAttempts = n
	})
}

func withBufferPool(bufferPool mem.BufferPool) DialOption {
	return newFuncDialOption(func(o *dialOptions) {
		o.copts.BufferPool = bufferPool
	})
}
