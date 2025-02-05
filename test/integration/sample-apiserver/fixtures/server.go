/*
Copyright 2025 The Kubernetes Authors.

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

package fixtures

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/pflag"
	clientv3 "go.etcd.io/etcd/client/v3"
	"k8s.io/apimachinery/pkg/util/wait"
	genericapiserver "k8s.io/apiserver/pkg/server"
	sserver "k8s.io/apiserver/pkg/server"
	"k8s.io/apiserver/pkg/storage/storagebackend"
	"k8s.io/apiserver/pkg/storageversion"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/test/utils/kubeconfig"
	"k8s.io/sample-apiserver/pkg/cmd/server"
)

// TearDownFunc is to be called to tear down a test server.
type TearDownFunc func()

// Logger allows t.Testing and b.Testing to be passed to StartTestServer and StartTestServerOrDie
type Logger interface {
	Helper()
	Errorf(format string, args ...interface{})
	Fatalf(format string, args ...interface{})
	Logf(format string, args ...interface{})
	Cleanup(func())
}

// TestServerInstanceOptions Instance options the TestServer
type TestServerInstanceOptions struct {
	// SkipHealthzCheck returns without waiting for the server to become healthy.
	// Useful for testing server configurations expected to prevent /healthz from completing.
	SkipHealthzCheck bool
	// Enable cert-auth for the kube-apiserver
	EnableCertAuth bool
	// Wrap the storage version interface of the created server's generic server.
	StorageVersionWrapFunc func(storageversion.Manager) storageversion.Manager
}

// TestServer return values supplied by kube-test-ApiServer
type TestServer struct {
	ClientConfig      *restclient.Config // Rest client config
	ServerOpts        *server.WardleServerOptions
	TearDownFn        TearDownFunc     // TearDown function
	TmpDir            string           // Temp Dir used, by the apiserver
	EtcdClient        *clientv3.Client // used by tests that need to check data migrated from APIs that are no longer served
	EtcdStoragePrefix string           // storage prefix in etcd
}

// StartDefaultAggregatedServer starts a test server.
func StartDefaultAggregatedServer(t Logger, kubeconfig *rest.Config, instanceOptions *TestServerInstanceOptions, flags ...string) (func(), *rest.Config, *server.WardleServerOptions, error) {
	tearDownFn, s, err := startDefaultAggregatedServer(t, kubeconfig, instanceOptions, flags...)
	if err != nil {
		return nil, nil, nil, err
	}
	return tearDownFn, s.ClientConfig, s.ServerOpts, nil
}

func startDefaultAggregatedServer(t Logger, kubeconfigs *rest.Config, instanceOptions *TestServerInstanceOptions, flags ...string) (func(), TestServer, error) {
	kubeConfig := kubeconfig.CreateKubeConfig(kubeconfigs)
	fakeKubeConfig, err := os.CreateTemp("", "kubeconfig")
	if err != nil {
		return nil, TestServer{}, err
	}
	clientcmd.WriteToFile(*kubeConfig, fakeKubeConfig.Name())

	s, err := StartTestAggregatedServer(t, kubeconfigs, instanceOptions, append([]string{
		"--etcd-prefix", uuid.New().String(),
		"--etcd-servers", strings.Join(IntegrationEtcdServers(), ","),
		"--authentication-skip-lookup",
		"--authentication-kubeconfig", fakeKubeConfig.Name(),
		"--authorization-kubeconfig", fakeKubeConfig.Name(),
		"--kubeconfig", fakeKubeConfig.Name(),
		// disable admission and filters that require talking to kube-apiserver
		"--enable-priority-and-fairness=false",
		"--disable-admission-plugins", "NamespaceLifecycle,MutatingAdmissionWebhook,ValidatingAdmissionWebhook"},
		flags...,
	), nil)
	if err != nil {
		os.Remove(fakeKubeConfig.Name())
		return nil, TestServer{}, err
	}

	tearDownFn := func() {
		defer os.Remove(fakeKubeConfig.Name())
		s.TearDownFn()
	}

	return tearDownFn, s, nil
}

// IntegrationEtcdServers returns etcd server URLs.
func IntegrationEtcdServers() []string {
	if etcdURL, ok := os.LookupEnv("KUBE_INTEGRATION_ETCD_URL"); ok {
		return []string{etcdURL}
	}
	return []string{"http://127.0.0.1:2379"}
}

func StartTestAggregatedServer(t Logger, kubeconfig *rest.Config, instanceOptions *TestServerInstanceOptions, customFlags []string, storageConfig *storagebackend.Config) (result TestServer, err error) {
	stopCh := make(chan struct{})
	var errCh chan error
	tearDown := func() {
		// Closing stopCh is stopping aggregated apiserver and its
		// delegates, which itself is cleaning up after itself,
		// including shutting down its storage layer.
		close(stopCh)

		// If the aggregated apiserver was started, let's wait for
		// it to shutdown clearly.
		if errCh != nil {
			err, ok := <-errCh
			if ok && err != nil {
				klog.Errorf("Failed to shutdown test server clearly: %v", err)
			}
		}

		if len(result.TmpDir) != 0 {
			os.RemoveAll(result.TmpDir)
		}
	}
	defer func() {
		if result.TearDownFn == nil {
			tearDown()
		}
	}()

	result.TmpDir, err = os.MkdirTemp("", "aggregated-apiserver")
	if err != nil {
		return result, fmt.Errorf("failed to create temp dir: %v", err)
	}

	fs := pflag.NewFlagSet("test", pflag.PanicOnError)

	aggregatedServerOptions := server.NewWardleServerOptions(os.Stdout, os.Stderr)
	aggregatedServerOptions.AddFlags(fs)

	aggregatedServerOptions.RecommendedOptions.SecureServing.Listener, aggregatedServerOptions.RecommendedOptions.SecureServing.BindPort, err = createLocalhostListenerOnFreePort()
	if err != nil {
		return result, fmt.Errorf("failed to create listener: %v", err)
	}

	aggregatedServerOptions.RecommendedOptions.SecureServing.ServerCert.CertDirectory = result.TmpDir
	aggregatedServerOptions.RecommendedOptions.SecureServing.ExternalAddress = aggregatedServerOptions.RecommendedOptions.SecureServing.Listener.Addr().(*net.TCPAddr).IP // use listener addr although it is a loopback device

	pkgPath, err := pkgPath(t)
	if err != nil {
		return result, err
	}
	aggregatedServerOptions.RecommendedOptions.SecureServing.ServerCert.FixtureDirectory = filepath.Join(pkgPath, "testdata")

	if storageConfig != nil {
		aggregatedServerOptions.RecommendedOptions.Etcd.StorageConfig = *storageConfig
	}

	fs.Parse(customFlags)

	if err := aggregatedServerOptions.Complete(); err != nil {
		return result, fmt.Errorf("failed to set default options: %v", err)
	}
	if err := aggregatedServerOptions.Validate(customFlags); err != nil {
		return result, fmt.Errorf("failed to validate options: %v", err)
	}

	t.Logf("Starting aggregated-apiserver on port %d...", aggregatedServerOptions.RecommendedOptions.SecureServing.BindPort)

	config, err := aggregatedServerOptions.Config()
	if err != nil {
		return result, fmt.Errorf("failed to create config from options: %v", err)
	}
	completedConfig := config.Complete()
	// check if there's a better way to pass KAS config here
	completedConfig.GenericConfig.ClientConfig = kubeconfig

	server, err := completedConfig.New()
	if err != nil {
		return result, fmt.Errorf("failed to create server: %v", err)
	}

	if instanceOptions.StorageVersionWrapFunc != nil {
		server.GenericAPIServer.StorageVersionManager = instanceOptions.StorageVersionWrapFunc(server.GenericAPIServer.StorageVersionManager)
	}

	server.GenericAPIServer.AddPostStartHookOrDie("start-sample-server-informers", func(context genericapiserver.PostStartHookContext) error {
		config.GenericConfig.SharedInformerFactory.Start(context.StopCh)
		aggregatedServerOptions.SharedInformerFactory.Start(context.StopCh)
		return nil
	})

	errCh = make(chan error)
	go func(stopCh <-chan struct{}) {
		defer close(errCh)
		prepared := server.GenericAPIServer.PrepareRun()
		if err := prepared.Run(stopCh); err != nil {
			errCh <- err
		}
	}(stopCh)

	t.Logf("Waiting for /healthz to be ok...")

	client, err := kubernetes.NewForConfig(server.GenericAPIServer.LoopbackClientConfig)
	if err != nil {
		return result, fmt.Errorf("failed to create a client: %v", err)
	}
	err = wait.Poll(100*time.Millisecond, time.Minute, func() (bool, error) {
		select {
		case err := <-errCh:
			return false, err
		default:
		}

		req := client.CoreV1().RESTClient().Get().AbsPath("/healthz")
		if instanceOptions.StorageVersionWrapFunc != nil {
			// We hardcode the param instead of having a new instanceOptions field
			// to avoid confusing users with more options.
			storageVersionCheck := fmt.Sprintf("poststarthook/%s-%s", sserver.StorageVersionPostStartHookName, sserver.KubeAggregatedAPIServer)
			req.Param("exclude", storageVersionCheck)
		}

		result := req.Do(context.TODO())
		status := 0
		result.StatusCode(&status)
		if status == 200 {
			return true, nil
		}
		return false, nil
	})
	if err != nil {
		return result, fmt.Errorf("failed to wait for /healthz to return ok: %v", err)
	}

	// from here the caller must call tearDown
	result.ServerOpts = aggregatedServerOptions
	result.TearDownFn = tearDown

	return result, nil
}

func createLocalhostListenerOnFreePort() (net.Listener, int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, 0, err
	}

	// get port
	tcpAddr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		ln.Close()
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
func pkgPath(t Logger) (string, error) {
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
