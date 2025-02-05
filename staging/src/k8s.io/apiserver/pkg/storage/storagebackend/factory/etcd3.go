/*
Copyright 2016 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package factory

import (
	"context"
	"fmt"
	"log"
	"math/rand"
	"net"
	"net/url"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	grpcprom "github.com/grpc-ecosystem/go-grpc-prometheus"
	"go.etcd.io/etcd/client/pkg/v3/logutil"
	"go.etcd.io/etcd/client/pkg/v3/transport"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/client/v3/kubernetes"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"golang.org/x/time/rate"
	"google.golang.org/grpc"
	"k8s.io/klog/v2"

	"k8s.io/apimachinery/pkg/runtime"
	utilnet "k8s.io/apimachinery/pkg/util/net"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	genericfeatures "k8s.io/apiserver/pkg/features"
	"k8s.io/apiserver/pkg/server/egressselector"
	"k8s.io/apiserver/pkg/storage"
	"k8s.io/apiserver/pkg/storage/etcd3"
	"k8s.io/apiserver/pkg/storage/etcd3/metrics"
	"k8s.io/apiserver/pkg/storage/storagebackend"
	"k8s.io/apiserver/pkg/storage/value/encrypt/identity"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/component-base/metrics/legacyregistry"
	tracing "k8s.io/component-base/tracing"
)

const (
	// The short keepalive timeout and interval have been chosen to aggressively
	// detect a failed etcd server without introducing much overhead.
	keepaliveTime    = 30 * time.Second
	keepaliveTimeout = 10 * time.Second

	// dialTimeout is the timeout for failing to establish a connection.
	// It is set to 20 seconds as times shorter than that will cause TLS connections to fail
	// on heavily loaded arm64 CPUs (issue #64649)
	dialTimeout = 20 * time.Second

	dbMetricsMonitorJitter = 0.5
)

// TODO(negz): Stop using a package scoped logger. At the time of writing we're
// creating an etcd client for each CRD. We need to pass each etcd client a
// logger or each client will create its own, which comes with a significant
// memory cost (around 20% of the API server's memory when hundreds of CRDs are
// present). The correct fix here is to not create a client per CRD. See
// https://github.com/kubernetes/kubernetes/issues/111476 for more.
var etcd3ClientLogger *zap.Logger

func init() {
	// grpcprom auto-registers (via an init function) their client metrics, since we are opting out of
	// using the global prometheus registry and using our own wrapped global registry,
	// we need to explicitly register these metrics to our global registry here.
	// For reference: https://github.com/kubernetes/kubernetes/pull/81387
	legacyregistry.RawMustRegister(grpcprom.DefaultClientMetrics)
	dbMetricsMonitors = make(map[string]struct{})

	l, err := logutil.CreateDefaultZapLogger(etcdClientDebugLevel())
	if err != nil {
		l = zap.NewNop()
	}
	etcd3ClientLogger = l.Named("etcd-client")
}

// etcdClientDebugLevel translates ETCD_CLIENT_DEBUG into zap log level.
// NOTE(negz): This is a copy of a private etcd client function:
// https://github.com/etcd-io/etcd/blob/v3.5.4/client/v3/logger.go#L47
func etcdClientDebugLevel() zapcore.Level {
	envLevel := os.Getenv("ETCD_CLIENT_DEBUG")
	if envLevel == "" || envLevel == "true" {
		return zapcore.InfoLevel
	}
	var l zapcore.Level
	if err := l.Set(envLevel); err == nil {
		log.Printf("Deprecated env ETCD_CLIENT_DEBUG value. Using default level: 'info'")
		return zapcore.InfoLevel
	}
	return l
}

func newETCD3HealthCheck(c storagebackend.Config, stopCh <-chan struct{}) (func() error, error) {
	timeout := storagebackend.DefaultHealthcheckTimeout
	if c.HealthcheckTimeout != time.Duration(0) {
		timeout = c.HealthcheckTimeout
	}
	return newETCD3Check(c, timeout, stopCh)
}

func newETCD3ReadyCheck(c storagebackend.Config, stopCh <-chan struct{}) (func() error, error) {
	timeout := storagebackend.DefaultReadinessTimeout
	if c.ReadycheckTimeout != time.Duration(0) {
		timeout = c.ReadycheckTimeout
	}
	return newETCD3Check(c, timeout, stopCh)
}

// atomic error acts as a cache for atomically store an error
// the error is only updated if the timestamp is more recent than
// current stored error.
type atomicLastError struct {
	mu        sync.RWMutex
	err       error
	timestamp time.Time
}

func (a *atomicLastError) Store(err error, t time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.timestamp.IsZero() || a.timestamp.Before(t) {
		a.err = err
		a.timestamp = t
	}
}

func (a *atomicLastError) Load() error {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.err
}

func newETCD3Check(c storagebackend.Config, timeout time.Duration, stopCh <-chan struct{}) (func() error, error) {
	// constructing the etcd v3 client blocks and times out if etcd is not available.
	// retry in a loop in the background until we successfully create the client, storing the client or error encountered

	lock := sync.RWMutex{}
	var prober *etcd3ProberMonitor
	clientErr := fmt.Errorf("etcd client connection not yet established")

	go wait.PollImmediateUntil(time.Second, func() (bool, error) {
		lock.Lock()
		defer lock.Unlock()
		newProber, err := newETCD3ProberMonitor(c)
		// Ensure that server is already not shutting down.
		select {
		case <-stopCh:
			if err == nil {
				newProber.Close()
			}
			return true, nil
		default:
		}
		if err != nil {
			clientErr = err
			return false, nil
		}
		prober = newProber
		clientErr = nil
		return true, nil
	}, stopCh)

	// Close the client on shutdown.
	go func() {
		defer utilruntime.HandleCrash()
		<-stopCh

		lock.Lock()
		defer lock.Unlock()
		if prober != nil {
			prober.Close()
			clientErr = fmt.Errorf("server is shutting down")
		}
	}()

	// limit to a request every half of the configured timeout with a maximum burst of one
	// rate limited requests will receive the last request sent error (note: not the last received response)
	limiter := rate.NewLimiter(rate.Every(timeout/2), 1)
	// initial state is the clientErr
	lastError := &atomicLastError{err: fmt.Errorf("etcd client connection not yet established")}

	return func() error {
		// Given that client is closed on shutdown we hold the lock for
		// the entire period of healthcheck call to ensure that client will
		// not be closed during healthcheck.
		// Given that healthchecks has a 2s timeout, worst case of blocking
		// shutdown for additional 2s seems acceptable.
		lock.RLock()
		defer lock.RUnlock()

		if clientErr != nil {
			return clientErr
		}
		if limiter.Allow() == false {
			return lastError.Load()
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		now := time.Now()
		err := prober.Probe(ctx)
		lastError.Store(err, now)
		return err
	}, nil
}

func newETCD3ProberMonitor(c storagebackend.Config) (*etcd3ProberMonitor, error) {
	client, err := newETCD3Client(c.Transport)
	if err != nil {
		return nil, err
	}
	return &etcd3ProberMonitor{
		client:    client.Client,
		prefix:    c.Prefix,
		endpoints: c.Transport.ServerList,
	}, nil
}

type etcd3ProberMonitor struct {
	prefix    string
	endpoints []string

	mux    sync.RWMutex
	client *clientv3.Client
	closed bool
}

func (t *etcd3ProberMonitor) Close() error {
	t.mux.Lock()
	defer t.mux.Unlock()
	if !t.closed {
		t.closed = true
		return t.client.Close()
	}
	return fmt.Errorf("closed")
}

func (t *etcd3ProberMonitor) Probe(ctx context.Context) error {
	t.mux.RLock()
	defer t.mux.RUnlock()
	if t.closed {
		return fmt.Errorf("closed")
	}
	// See https://github.com/etcd-io/etcd/blob/c57f8b3af865d1b531b979889c602ba14377420e/etcdctl/ctlv3/command/ep_command.go#L118
	_, err := t.client.Get(ctx, path.Join("/", t.prefix, "health"))
	if err != nil {
		return fmt.Errorf("error getting data from etcd: %w", err)
	}
	return nil
}

func (t *etcd3ProberMonitor) Monitor(ctx context.Context) (metrics.StorageMetrics, error) {
	t.mux.RLock()
	defer t.mux.RUnlock()
	if t.closed {
		return metrics.StorageMetrics{}, fmt.Errorf("closed")
	}
	status, err := t.client.Status(ctx, t.endpoints[rand.Int()%len(t.endpoints)])
	if err != nil {
		return metrics.StorageMetrics{}, err
	}
	return metrics.StorageMetrics{
		Size:      status.DbSize,
		SizeInUse: status.DbSizeInUse,
	}, nil
}

var newETCD3Client = func(c storagebackend.TransportConfig) (*kubernetes.Client, error) {
	tlsInfo := transport.TLSInfo{
		CertFile:      c.CertFile,
		KeyFile:       c.KeyFile,
		TrustedCAFile: c.TrustedCAFile,
	}
	tlsConfig, err := tlsInfo.ClientConfig()
	if err != nil {
		return nil, err
	}
	// NOTE: Client relies on nil tlsConfig
	// for non-secure connections, update the implicit variable
	if len(c.CertFile) == 0 && len(c.KeyFile) == 0 && len(c.TrustedCAFile) == 0 {
		tlsConfig = nil
	}
	networkContext := egressselector.Etcd.AsNetworkContext()
	var egressDialer utilnet.DialFunc
	if c.EgressLookup != nil {
		egressDialer, err = c.EgressLookup(networkContext)
		if err != nil {
			return nil, err
		}
	}
	dialOptions := []grpc.DialOption{
		grpc.WithBlock(), // block until the underlying connection is up
		// use chained interceptors so that the default (retry and backoff) interceptors are added.
		// otherwise they will be overwritten by the metric interceptor.
		//
		// these optional interceptors will be placed after the default ones.
		// which seems to be what we want as the metrics will be collected on each attempt (retry)
		grpc.WithChainUnaryInterceptor(grpcprom.UnaryClientInterceptor),
		grpc.WithChainStreamInterceptor(grpcprom.StreamClientInterceptor),
	}
	if utilfeature.DefaultFeatureGate.Enabled(genericfeatures.APIServerTracing) {
		tracingOpts := []otelgrpc.Option{
			otelgrpc.WithMessageEvents(otelgrpc.ReceivedEvents, otelgrpc.SentEvents),
			otelgrpc.WithPropagators(tracing.Propagators()),
			otelgrpc.WithTracerProvider(c.TracerProvider),
		}
		// Even with Noop  TracerProvider, the otelgrpc still handles context propagation.
		// See https://github.com/open-telemetry/opentelemetry-go/tree/main/example/passthrough
		dialOptions = append(dialOptions,
			grpc.WithUnaryInterceptor(otelgrpc.UnaryClientInterceptor(tracingOpts...)),
			grpc.WithStreamInterceptor(otelgrpc.StreamClientInterceptor(tracingOpts...)))
	}
	if egressDialer != nil {
		dialer := func(ctx context.Context, addr string) (net.Conn, error) {
			if strings.Contains(addr, "//") {
				// etcd client prior to 3.5 passed URLs to dialer, normalize to address
				u, err := url.Parse(addr)
				if err != nil {
					return nil, err
				}
				addr = u.Host
			}
			return egressDialer(ctx, "tcp", addr)
		}
		dialOptions = append(dialOptions, grpc.WithContextDialer(dialer))
	}

	cfg := clientv3.Config{
		DialTimeout:          dialTimeout,
		DialKeepAliveTime:    keepaliveTime,
		DialKeepAliveTimeout: keepaliveTimeout,
		DialOptions:          dialOptions,
		Endpoints:            c.ServerList,
		TLS:                  tlsConfig,
		Logger:               etcd3ClientLogger,
	}

	return kubernetes.New(cfg)
}

type runningCompactor struct {
	interval time.Duration
	cancel   context.CancelFunc
	client   *clientv3.Client
	refs     int
}

var (
	// compactorsMu guards access to compactors map
	compactorsMu sync.Mutex
	compactors   = map[string]*runningCompactor{}
	// dbMetricsMonitorsMu guards access to dbMetricsMonitors map
	dbMetricsMonitorsMu sync.Mutex
	dbMetricsMonitors   map[string]struct{}
)

// startCompactorOnce start one compactor per transport. If the interval get smaller on repeated calls, the
// compactor is replaced. A destroy func is returned. If all destroy funcs with the same transport are called,
// the compactor is stopped.
func startCompactorOnce(c storagebackend.TransportConfig, interval time.Duration) (func(), error) {
	compactorsMu.Lock()
	defer compactorsMu.Unlock()

	if interval == 0 {
		// short circuit, if the compaction request from apiserver is disabled
		return func() {}, nil
	}
	key := fmt.Sprintf("%v", c) // gives: {[server1 server2] keyFile certFile caFile}
	if compactor, foundBefore := compactors[key]; !foundBefore || compactor.interval > interval {
		client, err := newETCD3Client(c)
		if err != nil {
			return nil, err
		}
		compactorClient := client.Client

		if foundBefore {
			// replace compactor
			compactor.cancel()
			compactor.client.Close()
		} else {
			// start new compactor
			compactor = &runningCompactor{}
			compactors[key] = compactor
		}

		ctx, cancel := context.WithCancel(context.Background())

		compactor.interval = interval
		compactor.cancel = cancel
		compactor.client = compactorClient

		etcd3.StartCompactor(ctx, compactorClient, interval)
	}

	compactors[key].refs++

	return func() {
		compactorsMu.Lock()
		defer compactorsMu.Unlock()

		compactor := compactors[key]
		compactor.refs--
		if compactor.refs == 0 {
			compactor.cancel()
			compactor.client.Close()
			delete(compactors, key)
		}
	}, nil
}

func newETCD3Storage(c storagebackend.ConfigForResource, newFunc, newListFunc func() runtime.Object, resourcePrefix string) (storage.Interface, DestroyFunc, error) {
	stopCompactor, err := startCompactorOnce(c.Transport, c.CompactionInterval)
	if err != nil {
		return nil, nil, err
	}

	client, err := newETCD3Client(c.Transport)
	if err != nil {
		stopCompactor()
		return nil, nil, err
	}

	// decorate the KV instance so we can track etcd latency per request.
	client.KV = etcd3.NewETCDLatencyTracker(client.KV)

	stopDBSizeMonitor, err := startDBSizeMonitorPerEndpoint(client.Client, c.DBMetricPollInterval)
	if err != nil {
		return nil, nil, err
	}

	var once sync.Once
	destroyFunc := func() {
		// we know that storage destroy funcs are called multiple times (due to reuse in subresources).
		// Hence, we only destroy once.
		// TODO: fix duplicated storage destroy calls higher level
		once.Do(func() {
			stopCompactor()
			stopDBSizeMonitor()
			client.Close()
		})
	}
	transformer := c.Transformer
	if transformer == nil {
		transformer = identity.NewEncryptCheckTransformer()
	}

	versioner := storage.APIObjectVersioner{}
	decoder := etcd3.NewDefaultDecoder(c.Codec, versioner)
	store := etcd3.New(client, c.Codec, newFunc, newListFunc, c.Prefix, resourcePrefix, c.GroupResource, transformer, c.LeaseManagerConfig, decoder, versioner)
	return store, destroyFunc, nil
}

// startDBSizeMonitorPerEndpoint starts a loop to monitor etcd database size and update the
// corresponding metric etcd_db_total_size_in_bytes for each etcd server endpoint.
// Deprecated: Will be replaced with newETCD3ProberMonitor
func startDBSizeMonitorPerEndpoint(client *clientv3.Client, interval time.Duration) (func(), error) {
	if interval == 0 {
		return func() {}, nil
	}
	dbMetricsMonitorsMu.Lock()
	defer dbMetricsMonitorsMu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	for _, ep := range client.Endpoints() {
		if _, found := dbMetricsMonitors[ep]; found {
			continue
		}
		dbMetricsMonitors[ep] = struct{}{}
		endpoint := ep
		klog.V(4).Infof("Start monitoring storage db size metric for endpoint %s with polling interval %v", endpoint, interval)
		go wait.JitterUntilWithContext(ctx, func(context.Context) {
			epStatus, err := client.Maintenance.Status(ctx, endpoint)
			if err != nil {
				klog.V(4).Infof("Failed to get storage db size for ep %s: %v", endpoint, err)
				metrics.UpdateEtcdDbSize(endpoint, -1)
			} else {
				metrics.UpdateEtcdDbSize(endpoint, epStatus.DbSize)
			}
		}, interval, dbMetricsMonitorJitter, true)
	}

	return func() {
		cancel()
	}, nil
}
