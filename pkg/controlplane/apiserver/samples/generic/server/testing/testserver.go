/*
Copyright 2017 The Kubernetes Authors.

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

package testing

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/spf13/pflag"
	"go.etcd.io/etcd/client/pkg/v3/transport"
	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/grpc"
	"k8s.io/kubernetes/pkg/controlplane/apiserver/samples/generic/server"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apiserver/pkg/storage/storagebackend"
	"k8s.io/client-go/kubernetes"
	restclient "k8s.io/client-go/rest"
	cliflag "k8s.io/component-base/cli/flag"
	logsapi "k8s.io/component-base/logs/api/v1"
	"k8s.io/klog/v2"
	controlplaneapiserver "k8s.io/kubernetes/pkg/controlplane/apiserver/options"
	"k8s.io/kubernetes/test/utils/ktesting"
)

func init() {
	// If instantiated more than once or together with other servers, the
	// servers would try to modify the global logging state. This must get
	// ignored during testing.
	logsapi.ReapplyHandling = logsapi.ReapplyHandlingIgnoreUnchanged
}

// This key is for testing purposes only and is not considered secure.
const ecdsaPrivateKey = `-----BEGIN EC PRIVATE KEY-----
MHcCAQEEIEZmTmUhuanLjPA2CLquXivuwBDHTt5XYwgIr/kA1LtRoAoGCCqGSM49
AwEHoUQDQgAEH6cuzP8XuD5wal6wf9M6xDljTOPLX2i8uIp/C/ASqiIGUeeKQtX0
/IR3qCXyThP/dbCiHrF3v1cuhBOHY8CLVg==
-----END EC PRIVATE KEY-----`

// TearDownFunc is to be called to tear down a test server.
type TearDownFunc func()

// TestServerInstanceOptions Instance options the TestServer
type TestServerInstanceOptions struct {
	// SkipHealthzCheck returns without waiting for the server to become healthy.
	// Useful for testing server configurations expected to prevent /healthz from completing.
	SkipHealthzCheck bool
}

// TestServer represents a running test server with everything to access it and
// its backing store etcd.
type TestServer struct {
	ClientConfig      *restclient.Config             // Rest client config
	ServerOpts        *controlplaneapiserver.Options // ServerOpts
	TearDownFn        TearDownFunc                   // TearDown function
	TmpDir            string                         // Temp Dir used, by the apiserver
	EtcdClient        *clientv3.Client               // used by tests that need to check data migrated from APIs that are no longer served
	EtcdStoragePrefix string                         // storage prefix in etcd
}

// NewDefaultTestServerOptions default options for TestServer instances
func NewDefaultTestServerOptions() *TestServerInstanceOptions {
	return &TestServerInstanceOptions{}
}

// StartTestServer starts an etcd server and sample-generic-controlplane and
// returns a TestServer struct with a tear-down func and clients to access it
// and its backing store.
func StartTestServer(t ktesting.TB, instanceOptions *TestServerInstanceOptions, customFlags []string, storageConfig *storagebackend.Config) (result TestServer, err error) {
	tCtx := ktesting.Init(t)

	if instanceOptions == nil {
		instanceOptions = NewDefaultTestServerOptions()
	}

	result.TmpDir, err = os.MkdirTemp("", "sample-generic-controlplane")
	if err != nil {
		return result, fmt.Errorf("failed to create temp dir: %w", err)
	}

	var errCh chan error
	tearDown := func() {
		// Cancel is stopping apiserver and cleaning up
		// after itself, including shutting down its storage layer.
		tCtx.Cancel("tearing down")

		// If the apiserver was started, let's wait for it to
		// shutdown clearly.
		if errCh != nil {
			err, ok := <-errCh
			if ok && err != nil {
				klog.Errorf("Failed to shutdown test server clearly: %v", err)
			}
		}
		os.RemoveAll(result.TmpDir) //nolint:errcheck // best effort
	}
	defer func() {
		if result.TearDownFn == nil {
			tearDown()
		}
	}()

	o := server.NewOptions()
	var fss cliflag.NamedFlagSets
	o.AddFlags(&fss)

	fs := pflag.NewFlagSet("test", pflag.PanicOnError)
	for _, f := range fss.FlagSets {
		fs.AddFlagSet(f)
	}

	o.SecureServing.Listener, o.SecureServing.BindPort, err = createLocalhostListenerOnFreePort()
	if err != nil {
		return result, fmt.Errorf("failed to create listener: %w", err)
	}
	o.SecureServing.ServerCert.CertDirectory = result.TmpDir
	o.SecureServing.ExternalAddress = o.SecureServing.Listener.Addr().(*net.TCPAddr).IP // use listener addr although it is a loopback device

	pkgPath, err := pkgPath(t)
	if err != nil {
		return result, err
	}
	o.SecureServing.ServerCert.FixtureDirectory = filepath.Join(pkgPath, "testdata")

	o.Etcd.StorageConfig = *storageConfig
	utilruntime.Must(o.APIEnablement.RuntimeConfig.Set("api/all=true"))

	if err := fs.Parse(customFlags); err != nil {
		return result, err
	}

	saSigningKeyFile, err := os.CreateTemp("/tmp", "insecure_test_key")
	if err != nil {
		t.Fatalf("create temp file failed: %v", err)
	}
	defer os.RemoveAll(saSigningKeyFile.Name()) //nolint:errcheck // best effort
	if err = os.WriteFile(saSigningKeyFile.Name(), []byte(ecdsaPrivateKey), 0666); err != nil {
		t.Fatalf("write file %s failed: %v", saSigningKeyFile.Name(), err)
	}
	o.ServiceAccountSigningKeyFile = saSigningKeyFile.Name()
	o.Authentication.ServiceAccounts.Issuers = []string{"https://foo.bar.example.com"}
	o.Authentication.ServiceAccounts.KeyFiles = []string{saSigningKeyFile.Name()}

	completedOptions, err := o.Complete(tCtx, fss, nil, nil)
	if err != nil {
		return result, fmt.Errorf("failed to set default ServerRunOptions: %w", err)
	}

	if errs := completedOptions.Validate(); len(errs) != 0 {
		return result, fmt.Errorf("failed to validate ServerRunOptions: %w", utilerrors.NewAggregate(errs))
	}

	t.Logf("runtime-config=%v", completedOptions.APIEnablement.RuntimeConfig)
	t.Logf("Starting sample-generic-controlplane on port %d...", o.SecureServing.BindPort)

	config, err := server.NewConfig(completedOptions)
	if err != nil {
		return result, err
	}
	completed, err := config.Complete()
	if err != nil {
		return result, err
	}
	s, err := server.CreateServerChain(tCtx, completed)
	if err != nil {
		return result, fmt.Errorf("failed to create server chain: %w", err)
	}

	errCh = make(chan error)
	go func() {
		defer close(errCh)
		prepared, err := s.PrepareRun()
		if err != nil {
			errCh <- err
		} else if err := prepared.Run(tCtx); err != nil {
			errCh <- err
		}
	}()

	client, err := kubernetes.NewForConfig(s.GenericAPIServer.LoopbackClientConfig)
	if err != nil {
		return result, fmt.Errorf("failed to create a client: %w", err)
	}

	if !instanceOptions.SkipHealthzCheck {
		t.Logf("Waiting for /healthz to be ok...")

		// wait until healthz endpoint returns ok
		err = wait.PollUntilContextTimeout(tCtx, 100*time.Millisecond, wait.ForeverTestTimeout, true, func(ctx context.Context) (bool, error) {
			select {
			case err := <-errCh:
				return false, err
			default:
			}

			req := client.CoreV1().RESTClient().Get().AbsPath("/healthz")
			result := req.Do(ctx)
			status := 0
			result.StatusCode(&status)
			if status == 200 {
				return true, nil
			}
			return false, nil
		})
		if err != nil {
			return result, fmt.Errorf("failed to wait for /healthz to return ok: %w", err)
		}
	}

	// wait until default namespace is created
	err = wait.PollUntilContextTimeout(tCtx, 100*time.Millisecond, wait.ForeverTestTimeout, true, func(ctx context.Context) (bool, error) {
		select {
		case err := <-errCh:
			return false, err
		default:
		}

		if _, err := client.CoreV1().Namespaces().Get(ctx, "default", metav1.GetOptions{}); err != nil {
			if !errors.IsNotFound(err) {
				t.Logf("Unable to get default namespace: %v", err)
			}
			return false, nil
		}
		return true, nil
	})
	if err != nil {
		return result, fmt.Errorf("failed to wait for default namespace to be created: %w", err)
	}

	tlsInfo := transport.TLSInfo{
		CertFile:      storageConfig.Transport.CertFile,
		KeyFile:       storageConfig.Transport.KeyFile,
		TrustedCAFile: storageConfig.Transport.TrustedCAFile,
	}
	tlsConfig, err := tlsInfo.ClientConfig()
	if err != nil {
		return result, err
	}
	etcdConfig := clientv3.Config{
		Endpoints:   storageConfig.Transport.ServerList,
		DialTimeout: 20 * time.Second,
		DialOptions: []grpc.DialOption{
			grpc.WithBlock(), // block until the underlying connection is up
		},
		TLS: tlsConfig,
	}
	etcdClient, err := clientv3.New(etcdConfig)
	if err != nil {
		return result, err
	}

	// from here the caller must call tearDown
	result.ClientConfig = restclient.CopyConfig(s.GenericAPIServer.LoopbackClientConfig)
	result.ClientConfig.QPS = 1000
	result.ClientConfig.Burst = 10000
	result.ServerOpts = o
	result.TearDownFn = func() {
		tearDown()
		etcdClient.Close() //nolint:errcheck // best effort
	}
	result.EtcdClient = etcdClient
	result.EtcdStoragePrefix = storageConfig.Prefix

	return result, nil
}

// StartTestServerOrDie calls StartTestServer t.Fatal if it does not succeed.
func StartTestServerOrDie(t ktesting.TB, instanceOptions *TestServerInstanceOptions, flags []string, storageConfig *storagebackend.Config) *TestServer {
	result, err := StartTestServer(t, instanceOptions, flags, storageConfig)
	if err == nil {
		return &result
	}

	t.Fatalf("failed to launch server: %v", err)
	return nil
}

func createLocalhostListenerOnFreePort() (net.Listener, int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, 0, err
	}

	// get port
	tcpAddr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		ln.Close() //nolint:errcheck // best effort
		return nil, 0, fmt.Errorf("invalid listen address: %q", ln.Addr().String())
	}

	return ln, tcpAddr.Port, nil
}

// pkgPath returns the absolute file path to this package's directory. With go
// test, we can just look at the runtime call stack. However, bazel compiles go
// binaries with the -trimpath option so the simple approach fails however we
// can consult environment variables to derive the path.
//
// The approach taken here works for both go test and bazel on the assumption
// that if and only if trimpath is passed, we are running under bazel.
func pkgPath(t ktesting.TB) (string, error) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("failed to get current file")
	}

	pkgPath := filepath.Dir(thisFile)

	// If we find bazel env variables, then -trimpath was passed so we need to
	// construct the path from the environment.
	if testSrcdir, testWorkspace := os.Getenv("TEST_SRCDIR"), os.Getenv("TEST_WORKSPACE"); testSrcdir != "" && testWorkspace != "" {
		t.Logf("Detected bazel env varaiables: TEST_SRCDIR=%q TEST_WORKSPACE=%q", testSrcdir, testWorkspace)
		pkgPath = filepath.Join(testSrcdir, testWorkspace, pkgPath)
	}

	// If the path is still not absolute, something other than bazel compiled
	// with -trimpath.
	if !filepath.IsAbs(pkgPath) {
		return "", fmt.Errorf("can't construct an absolute path from %q", pkgPath)
	}

	t.Logf("Resolved testserver package path to: %q", pkgPath)

	return pkgPath, nil
}
