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
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-semver/semver"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	grpccredentials "google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/status"

	"go.etcd.io/etcd/api/v3/v3rpc/rpctypes"
	"go.etcd.io/etcd/api/v3/version"
	"go.etcd.io/etcd/client/pkg/v3/logutil"
	"go.etcd.io/etcd/client/pkg/v3/verify"
	"go.etcd.io/etcd/client/v3/credentials"
	"go.etcd.io/etcd/client/v3/internal/endpoint"
	"go.etcd.io/etcd/client/v3/internal/resolver"
)

var (
	ErrNoAvailableEndpoints = errors.New("etcdclient: no available endpoints")
	ErrOldCluster           = errors.New("etcdclient: old cluster version")
)

// Client provides and manages an etcd v3 client session.
type Client struct {
	Cluster
	KV
	Lease
	Watcher
	Auth
	Maintenance

	conn *grpc.ClientConn

	cfg      Config
	creds    grpccredentials.TransportCredentials
	resolver *resolver.EtcdManualResolver

	epMu      *sync.RWMutex
	endpoints []string

	ctx    context.Context
	cancel context.CancelFunc

	// Username is a user name for authentication.
	Username string
	// Password is a password for authentication.
	Password        string
	authTokenBundle credentials.PerRPCCredentialsBundle

	callOpts []grpc.CallOption

	lgMu *sync.RWMutex
	lg   *zap.Logger
}

// New creates a new etcdv3 client from a given configuration.
func New(cfg Config) (*Client, error) {
	if len(cfg.Endpoints) == 0 {
		return nil, ErrNoAvailableEndpoints
	}

	return newClient(&cfg)
}

// NewCtxClient creates a client with a context but no underlying grpc
// connection. This is useful for embedded cases that override the
// service interface implementations and do not need connection management.
func NewCtxClient(ctx context.Context, opts ...Option) *Client {
	cctx, cancel := context.WithCancel(ctx)
	c := &Client{ctx: cctx, cancel: cancel, lgMu: new(sync.RWMutex), epMu: new(sync.RWMutex)}
	for _, opt := range opts {
		opt(c)
	}
	if c.lg == nil {
		c.lg = zap.NewNop()
	}
	return c
}

// Option is a function type that can be passed as argument to NewCtxClient to configure client
type Option func(*Client)

// NewFromURL creates a new etcdv3 client from a URL.
func NewFromURL(url string) (*Client, error) {
	return New(Config{Endpoints: []string{url}})
}

// NewFromURLs creates a new etcdv3 client from URLs.
func NewFromURLs(urls []string) (*Client, error) {
	return New(Config{Endpoints: urls})
}

// WithZapLogger is a NewCtxClient option that overrides the logger
func WithZapLogger(lg *zap.Logger) Option {
	return func(c *Client) {
		c.lg = lg
	}
}

// WithLogger overrides the logger.
//
// Deprecated: Please use WithZapLogger or Logger field in clientv3.Config
//
// Does not changes grpcLogger, that can be explicitly configured
// using grpc_zap.ReplaceGrpcLoggerV2(..) method.
func (c *Client) WithLogger(lg *zap.Logger) *Client {
	c.lgMu.Lock()
	c.lg = lg
	c.lgMu.Unlock()
	return c
}

// GetLogger gets the logger.
// NOTE: This method is for internal use of etcd-client library and should not be used as general-purpose logger.
func (c *Client) GetLogger() *zap.Logger {
	c.lgMu.RLock()
	l := c.lg
	c.lgMu.RUnlock()
	return l
}

// Close shuts down the client's etcd connections.
func (c *Client) Close() error {
	c.cancel()
	if c.Watcher != nil {
		c.Watcher.Close()
	}
	if c.Lease != nil {
		c.Lease.Close()
	}
	if c.conn != nil {
		return ContextError(c.ctx, c.conn.Close())
	}
	return c.ctx.Err()
}

// Ctx is a context for "out of band" messages (e.g., for sending
// "clean up" message when another context is canceled). It is
// canceled on client Close().
func (c *Client) Ctx() context.Context { return c.ctx }

// Endpoints lists the registered endpoints for the client.
func (c *Client) Endpoints() []string {
	// copy the slice; protect original endpoints from being changed
	c.epMu.RLock()
	defer c.epMu.RUnlock()
	eps := make([]string, len(c.endpoints))
	copy(eps, c.endpoints)
	return eps
}

// SetEndpoints updates client's endpoints.
func (c *Client) SetEndpoints(eps ...string) {
	c.epMu.Lock()
	defer c.epMu.Unlock()
	c.endpoints = eps

	c.resolver.SetEndpoints(eps)
}

// Sync synchronizes client's endpoints with the known endpoints from the etcd membership.
func (c *Client) Sync(ctx context.Context) error {
	mresp, err := c.MemberList(ctx)
	if err != nil {
		return err
	}
	var eps []string
	for _, m := range mresp.Members {
		if len(m.Name) != 0 && !m.IsLearner {
			eps = append(eps, m.ClientURLs...)
		}
	}
	// The linearizable `MemberList` returned successfully, so the
	// endpoints shouldn't be empty.
	verify.Verify(func() {
		if len(eps) == 0 {
			panic("empty endpoints returned from etcd cluster")
		}
	})
	c.SetEndpoints(eps...)
	c.lg.Debug("set etcd endpoints by autoSync", zap.Strings("endpoints", eps))
	return nil
}

func (c *Client) autoSync() {
	if c.cfg.AutoSyncInterval == time.Duration(0) {
		return
	}

	for {
		select {
		case <-c.ctx.Done():
			return
		case <-time.After(c.cfg.AutoSyncInterval):
			ctx, cancel := context.WithTimeout(c.ctx, 5*time.Second)
			err := c.Sync(ctx)
			cancel()
			if err != nil && !errors.Is(err, c.ctx.Err()) {
				c.lg.Info("Auto sync endpoints failed.", zap.Error(err))
			}
		}
	}
}

// dialSetupOpts gives the dial opts prior to any authentication.
func (c *Client) dialSetupOpts(creds grpccredentials.TransportCredentials, dopts ...grpc.DialOption) []grpc.DialOption {
	var opts []grpc.DialOption

	if c.cfg.DialKeepAliveTime > 0 {
		params := keepalive.ClientParameters{
			Time:                c.cfg.DialKeepAliveTime,
			Timeout:             c.cfg.DialKeepAliveTimeout,
			PermitWithoutStream: c.cfg.PermitWithoutStream,
		}
		opts = append(opts, grpc.WithKeepaliveParams(params))
	}
	opts = append(opts, dopts...)

	if creds != nil {
		opts = append(opts, grpc.WithTransportCredentials(creds))
	} else {
		opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}

	unaryMaxRetries := defaultUnaryMaxRetries
	if c.cfg.MaxUnaryRetries > 0 {
		unaryMaxRetries = c.cfg.MaxUnaryRetries
	}

	backoffWaitBetween := defaultBackoffWaitBetween
	if c.cfg.BackoffWaitBetween > 0 {
		backoffWaitBetween = c.cfg.BackoffWaitBetween
	}

	backoffJitterFraction := defaultBackoffJitterFraction
	if c.cfg.BackoffJitterFraction > 0 {
		backoffJitterFraction = c.cfg.BackoffJitterFraction
	}

	// Interceptor retry and backoff.
	// TODO: Replace all of clientv3/retry.go with RetryPolicy:
	// https://github.com/grpc/grpc-proto/blob/cdd9ed5c3d3f87aef62f373b93361cf7bddc620d/grpc/service_config/service_config.proto#L130
	rrBackoff := withBackoff(c.roundRobinQuorumBackoff(backoffWaitBetween, backoffJitterFraction))
	opts = append(opts,
		// Disable stream retry by default since go-grpc-middleware/retry does not support client streams.
		// Streams that are safe to retry are enabled individually.
		grpc.WithStreamInterceptor(c.streamClientInterceptor(withMax(0), rrBackoff)),
		grpc.WithUnaryInterceptor(c.unaryClientInterceptor(withMax(unaryMaxRetries), rrBackoff)),
	)

	return opts
}

// Dial connects to a single endpoint using the client's config.
func (c *Client) Dial(ep string) (*grpc.ClientConn, error) {
	creds := c.credentialsForEndpoint(ep)

	// Using ad-hoc created resolver, to guarantee only explicitly given
	// endpoint is used.
	return c.dial(creds, grpc.WithResolvers(resolver.New(ep)))
}

func (c *Client) getToken(ctx context.Context) error {
	var err error // return last error in a case of fail

	if c.Username == "" || c.Password == "" {
		return nil
	}

	resp, err := c.Auth.Authenticate(ctx, c.Username, c.Password)
	if err != nil {
		if errors.Is(err, rpctypes.ErrAuthNotEnabled) {
			c.authTokenBundle.UpdateAuthToken("")
			return nil
		}
		return err
	}
	c.authTokenBundle.UpdateAuthToken(resp.Token)
	return nil
}

// dialWithBalancer dials the client's current load balanced resolver group.  The scheme of the host
// of the provided endpoint determines the scheme used for all endpoints of the client connection.
func (c *Client) dialWithBalancer(dopts ...grpc.DialOption) (*grpc.ClientConn, error) {
	creds := c.credentialsForEndpoint(c.Endpoints()[0])
	opts := append(dopts, grpc.WithResolvers(c.resolver))
	return c.dial(creds, opts...)
}

// dial configures and dials any grpc balancer target.
func (c *Client) dial(creds grpccredentials.TransportCredentials, dopts ...grpc.DialOption) (*grpc.ClientConn, error) {
	opts := c.dialSetupOpts(creds, dopts...)

	if c.authTokenBundle != nil {
		opts = append(opts, grpc.WithPerRPCCredentials(c.authTokenBundle.PerRPCCredentials()))
	}

	opts = append(opts, c.cfg.DialOptions...)

	dctx := c.ctx
	if c.cfg.DialTimeout > 0 {
		var cancel context.CancelFunc
		dctx, cancel = context.WithTimeout(c.ctx, c.cfg.DialTimeout)
		defer cancel() // TODO: Is this right for cases where grpc.WithBlock() is not set on the dial options?
	}
	target := fmt.Sprintf("%s://%p/%s", resolver.Schema, c, authority(c.endpoints[0]))
	conn, err := grpc.DialContext(dctx, target, opts...)
	if err != nil {
		return nil, err
	}
	return conn, nil
}

func authority(endpoint string) string {
	spl := strings.SplitN(endpoint, "://", 2)
	if len(spl) < 2 {
		if strings.HasPrefix(endpoint, "unix:") {
			return endpoint[len("unix:"):]
		}
		if strings.HasPrefix(endpoint, "unixs:") {
			return endpoint[len("unixs:"):]
		}
		return endpoint
	}
	return spl[1]
}

func (c *Client) credentialsForEndpoint(ep string) grpccredentials.TransportCredentials {
	r := endpoint.RequiresCredentials(ep)
	switch r {
	case endpoint.CredsDrop:
		return nil
	case endpoint.CredsOptional:
		return c.creds
	case endpoint.CredsRequire:
		if c.creds != nil {
			return c.creds
		}
		return credentials.NewTransportCredential(nil)
	default:
		panic(fmt.Errorf("unsupported CredsRequirement: %v", r))
	}
}

func newClient(cfg *Config) (*Client, error) {
	if cfg == nil {
		cfg = &Config{}
	}
	var creds grpccredentials.TransportCredentials
	if cfg.TLS != nil {
		creds = credentials.NewTransportCredential(cfg.TLS)
	}

	// use a temporary skeleton client to bootstrap first connection
	baseCtx := context.TODO()
	if cfg.Context != nil {
		baseCtx = cfg.Context
	}

	ctx, cancel := context.WithCancel(baseCtx)
	client := &Client{
		conn:     nil,
		cfg:      *cfg,
		creds:    creds,
		ctx:      ctx,
		cancel:   cancel,
		epMu:     new(sync.RWMutex),
		callOpts: defaultCallOpts,
		lgMu:     new(sync.RWMutex),
	}

	var err error
	if cfg.Logger != nil {
		client.lg = cfg.Logger
	} else if cfg.LogConfig != nil {
		client.lg, err = cfg.LogConfig.Build()
	} else {
		client.lg, err = logutil.CreateDefaultZapLogger(etcdClientDebugLevel())
		if client.lg != nil {
			client.lg = client.lg.Named("etcd-client")
		}
	}
	if err != nil {
		return nil, err
	}

	if cfg.Username != "" && cfg.Password != "" {
		client.Username = cfg.Username
		client.Password = cfg.Password
		client.authTokenBundle = credentials.NewPerRPCCredentialBundle()
	}
	if cfg.MaxCallSendMsgSize > 0 || cfg.MaxCallRecvMsgSize > 0 {
		if cfg.MaxCallRecvMsgSize > 0 && cfg.MaxCallSendMsgSize > cfg.MaxCallRecvMsgSize {
			return nil, fmt.Errorf("gRPC message recv limit (%d bytes) must be greater than send limit (%d bytes)", cfg.MaxCallRecvMsgSize, cfg.MaxCallSendMsgSize)
		}
		callOpts := []grpc.CallOption{
			defaultWaitForReady,
			defaultMaxCallSendMsgSize,
			defaultMaxCallRecvMsgSize,
		}
		if cfg.MaxCallSendMsgSize > 0 {
			callOpts[1] = grpc.MaxCallSendMsgSize(cfg.MaxCallSendMsgSize)
		}
		if cfg.MaxCallRecvMsgSize > 0 {
			callOpts[2] = grpc.MaxCallRecvMsgSize(cfg.MaxCallRecvMsgSize)
		}
		client.callOpts = callOpts
	}

	client.resolver = resolver.New(cfg.Endpoints...)

	if len(cfg.Endpoints) < 1 {
		client.cancel()
		return nil, errors.New("at least one Endpoint is required in client config")
	}
	client.SetEndpoints(cfg.Endpoints...)

	// Use a provided endpoint target so that for https:// without any tls config given, then
	// grpc will assume the certificate server name is the endpoint host.
	conn, err := client.dialWithBalancer()
	if err != nil {
		client.cancel()
		client.resolver.Close()
		// TODO: Error like `fmt.Errorf(dialing [%s] failed: %v, strings.Join(cfg.Endpoints, ";"), err)` would help with debugging a lot.
		return nil, err
	}
	client.conn = conn

	client.Cluster = NewCluster(client)
	client.KV = NewKV(client)
	client.Lease = NewLease(client)
	client.Watcher = NewWatcher(client)
	client.Auth = NewAuth(client)
	client.Maintenance = NewMaintenance(client)

	// get token with established connection
	ctx, cancel = client.ctx, func() {}
	if client.cfg.DialTimeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, client.cfg.DialTimeout)
	}
	err = client.getToken(ctx)
	if err != nil {
		client.Close()
		cancel()
		// TODO: Consider fmt.Errorf("communicating with [%s] failed: %v", strings.Join(cfg.Endpoints, ";"), err)
		return nil, err
	}
	cancel()

	if cfg.RejectOldCluster {
		if err := client.checkVersion(); err != nil {
			client.Close()
			return nil, err
		}
	}

	go client.autoSync()
	return client, nil
}

// roundRobinQuorumBackoff retries against quorum between each backoff.
// This is intended for use with a round robin load balancer.
func (c *Client) roundRobinQuorumBackoff(waitBetween time.Duration, jitterFraction float64) backoffFunc {
	return func(attempt uint) time.Duration {
		// after each round robin across quorum, backoff for our wait between duration
		n := uint(len(c.Endpoints()))
		quorum := (n/2 + 1)
		if attempt%quorum == 0 {
			c.lg.Debug("backoff", zap.Uint("attempt", attempt), zap.Uint("quorum", quorum), zap.Duration("waitBetween", waitBetween), zap.Float64("jitterFraction", jitterFraction))
			return jitterUp(waitBetween, jitterFraction)
		}
		c.lg.Debug("backoff skipped", zap.Uint("attempt", attempt), zap.Uint("quorum", quorum))
		return 0
	}
}

// minSupportedVersion returns the minimum version supported, which is the previous minor release.
func minSupportedVersion() *semver.Version {
	ver := semver.Must(semver.NewVersion(version.Version))
	// consider only major and minor version
	ver = &semver.Version{Major: ver.Major, Minor: ver.Minor}
	for i := range version.AllVersions {
		if version.AllVersions[i].Equal(*ver) {
			if i == 0 {
				return ver
			}
			return &version.AllVersions[i-1]
		}
	}
	panic("current version is not in the version list")
}

func (c *Client) checkVersion() (err error) {
	var wg sync.WaitGroup

	eps := c.Endpoints()
	errc := make(chan error, len(eps))
	ctx, cancel := context.WithCancel(c.ctx)
	if c.cfg.DialTimeout > 0 {
		cancel()
		ctx, cancel = context.WithTimeout(c.ctx, c.cfg.DialTimeout)
	}

	wg.Add(len(eps))
	for _, ep := range eps {
		// if cluster is current, any endpoint gives a recent version
		go func(e string) {
			defer wg.Done()
			resp, rerr := c.Status(ctx, e)
			if rerr != nil {
				errc <- rerr
				return
			}
			vs, serr := semver.NewVersion(resp.Version)
			if serr != nil {
				errc <- serr
				return
			}

			if vs.LessThan(*minSupportedVersion()) {
				rerr = ErrOldCluster
			}
			errc <- rerr
		}(ep)
	}
	// wait for success
	for range eps {
		if err = <-errc; err != nil {
			break
		}
	}
	cancel()
	wg.Wait()
	return err
}

// ActiveConnection returns the current in-use connection
func (c *Client) ActiveConnection() *grpc.ClientConn { return c.conn }

// isHaltErr returns true if the given error and context indicate no forward
// progress can be made, even after reconnecting.
func isHaltErr(ctx context.Context, err error) bool {
	if ctx != nil && ctx.Err() != nil {
		return true
	}
	if err == nil {
		return false
	}
	ev, _ := status.FromError(err)
	// Unavailable codes mean the system will be right back.
	// (e.g., can't connect, lost leader)
	// Treat Internal codes as if something failed, leaving the
	// system in an inconsistent state, but retrying could make progress.
	// (e.g., failed in middle of send, corrupted frame)
	// TODO: are permanent Internal errors possible from grpc?
	return ev.Code() != codes.Unavailable && ev.Code() != codes.Internal
}

// isUnavailableErr returns true if the given error is an unavailable error
func isUnavailableErr(ctx context.Context, err error) bool {
	if ctx != nil && ctx.Err() != nil {
		return false
	}
	if err == nil {
		return false
	}
	ev, ok := status.FromError(err)
	if ok {
		// Unavailable codes mean the system will be right back.
		// (e.g., can't connect, lost leader)
		return ev.Code() == codes.Unavailable
	}
	return false
}

// ContextError converts the error into an EtcdError if the error message matches one of
// the defined messages; otherwise, it tries to retrieve the context error.
func ContextError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	err = rpctypes.Error(err)
	var serverErr rpctypes.EtcdError
	if errors.As(err, &serverErr) {
		return err
	}
	if ev, ok := status.FromError(err); ok {
		code := ev.Code()
		switch code {
		case codes.DeadlineExceeded:
			fallthrough
		case codes.Canceled:
			if ctx.Err() != nil {
				err = ctx.Err()
			}
		}
	}
	return err
}

func canceledByCaller(stopCtx context.Context, err error) bool {
	if stopCtx.Err() == nil || err == nil {
		return false
	}

	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// IsConnCanceled returns true, if error is from a closed gRPC connection.
// ref. https://github.com/grpc/grpc-go/pull/1854
func IsConnCanceled(err error) bool {
	if err == nil {
		return false
	}

	// >= gRPC v1.23.x
	s, ok := status.FromError(err)
	if ok {
		// connection is canceled or server has already closed the connection
		return s.Code() == codes.Canceled || s.Message() == "transport is closing"
	}

	// >= gRPC v1.10.x
	if errors.Is(err, context.Canceled) {
		return true
	}

	// <= gRPC v1.7.x returns 'errors.New("grpc: the client connection is closing")'
	return strings.Contains(err.Error(), "grpc: the client connection is closing")
}
