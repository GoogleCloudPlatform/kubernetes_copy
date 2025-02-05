/*
Copyright 2018 The Kubernetes Authors.

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
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/spf13/pflag"

	extensionsapiserver "k8s.io/apiextensions-apiserver/pkg/apiserver"
	"k8s.io/apiextensions-apiserver/pkg/cmd/server/options"
	generatedopenapi "k8s.io/apiextensions-apiserver/pkg/generated/openapi"
	"k8s.io/apimachinery/pkg/util/wait"
	openapinamer "k8s.io/apiserver/pkg/endpoints/openapi"
	genericapiserver "k8s.io/apiserver/pkg/server"
	"k8s.io/apiserver/pkg/storage/storagebackend"
	"k8s.io/apiserver/pkg/util/compatibility"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/apiserver/pkg/util/openapi"
	"k8s.io/client-go/kubernetes"
	restclient "k8s.io/client-go/rest"
	basecompatibility "k8s.io/component-base/compatibility"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	logsapi "k8s.io/component-base/logs/api/v1"
	"k8s.io/klog/v2"
)

func init() {
	// If instantiated more than once or together with other servers, the
	// servers would try to modify the global logging state. This must get
	// ignored during testing.
	logsapi.ReapplyHandling = logsapi.ReapplyHandlingIgnoreUnchanged
}

// TearDownFunc is to be called to tear down a test server.
type TearDownFunc func()

// TestServerInstanceOptions Instance options the TestServer
type TestServerInstanceOptions struct {
}

// TestServer return values supplied by kube-test-ApiServer
type TestServer struct {
	ClientConfig    *restclient.Config                              // Rest client config
	ServerOpts      *options.CustomResourceDefinitionsServerOptions // ServerOpts
	TearDownFn      TearDownFunc                                    // TearDown function
	TmpDir          string                                          // Temp Dir used, by the apiserver
	CompletedConfig extensionsapiserver.CompletedConfig
}

// Logger allows t.Testing and b.Testing to be passed to StartTestServer and StartTestServerOrDie
type Logger interface {
	Errorf(format string, args ...interface{})
	Fatalf(format string, args ...interface{})
	Logf(format string, args ...interface{})
}

// NewDefaultTestServerOptions Default options for TestServer instances
func NewDefaultTestServerOptions() *TestServerInstanceOptions {
	return &TestServerInstanceOptions{}
}

// StartTestServer starts a apiextensions-apiserver. A rest client config and a tear-down func,
// and location of the tmpdir are returned.
//
// Note: we return a tear-down func instead of a stop channel because the later will leak temporary
// files that because Golang testing's call to os.Exit will not give a stop channel go routine
// enough time to remove temporary files.
func StartTestServer(t Logger, _ *TestServerInstanceOptions, customFlags []string, storageConfig *storagebackend.Config) (result TestServer, err error) {
	// TODO: this is a candidate for using what is now test/utils/ktesting,
	// should that become a staging repo.
	ctx, cancel := context.WithCancelCause(context.Background())
	var errCh chan error
	tearDown := func() {
		// Cancel is stopping apiextensions apiserver and its
		// delegates, which itself is cleaning up after itself,
		// including shutting down its storage layer.
		cancel(errors.New("tearing down"))

		// If the apiextensions apiserver was started, let's wait for
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

	result.TmpDir, err = os.MkdirTemp("", "apiextensions-apiserver")
	if err != nil {
		return result, fmt.Errorf("failed to create temp dir: %v", err)
	}

	fs := pflag.NewFlagSet("test", pflag.PanicOnError)

	s := options.NewCustomResourceDefinitionsServerOptions(os.Stdout, os.Stderr)

	// set up new instance of ComponentGlobalsRegistry instead of using the DefaultComponentGlobalsRegistry to avoid contention in parallel tests.
	featureGate := utilfeature.DefaultMutableFeatureGate.DeepCopy()
	effectiveVersion := compatibility.DefaultKubeEffectiveVersionForTest()
	effectiveVersion.SetEmulationVersion(featureGate.EmulationVersion())
	componentGlobalsRegistry := basecompatibility.NewComponentGlobalsRegistry()
	if err := componentGlobalsRegistry.Register(basecompatibility.DefaultKubeComponent, effectiveVersion, featureGate); err != nil {
		return result, err
	}
	s.ServerRunOptions.ComponentGlobalsRegistry = componentGlobalsRegistry
	s.AddFlags(fs)

	s.RecommendedOptions.SecureServing.Listener, s.RecommendedOptions.SecureServing.BindPort, err = createLocalhostListenerOnFreePort()
	if err != nil {
		return result, fmt.Errorf("failed to create listener: %v", err)
	}
	s.RecommendedOptions.SecureServing.ServerCert.CertDirectory = result.TmpDir
	s.RecommendedOptions.SecureServing.ExternalAddress = s.RecommendedOptions.SecureServing.Listener.Addr().(*net.TCPAddr).IP // use listener addr although it is a loopback device

	pkgPath, err := pkgPath(t)
	if err != nil {
		return result, err
	}
	s.RecommendedOptions.SecureServing.ServerCert.FixtureDirectory = filepath.Join(pkgPath, "testdata")

	if storageConfig != nil {
		s.RecommendedOptions.Etcd.StorageConfig = *storageConfig
	}
	s.APIEnablement.RuntimeConfig.Set("api/all=true")

	fs.Parse(customFlags)

	if err := componentGlobalsRegistry.Set(); err != nil {
		return result, fmt.Errorf("%w\nIf you are using SetFeatureGate*DuringTest, try using --emulated-version and --feature-gates flags instead", err)
	}
	// If the local ComponentGlobalsRegistry is changed by the flags,
	// we need to copy the new feature values back to the DefaultFeatureGate because most feature checks still use the DefaultFeatureGate.
	if !featureGate.EmulationVersion().EqualTo(utilfeature.DefaultMutableFeatureGate.EmulationVersion()) {
		featuregatetesting.SetFeatureGateEmulationVersionDuringTest(t.(featuregatetesting.TB), utilfeature.DefaultMutableFeatureGate, effectiveVersion.EmulationVersion())
	}
	for f := range utilfeature.DefaultMutableFeatureGate.GetAll() {
		if featureGate.Enabled(f) != utilfeature.DefaultFeatureGate.Enabled(f) {
			featuregatetesting.SetFeatureGateDuringTest(t.(featuregatetesting.TB), utilfeature.DefaultFeatureGate, f, featureGate.Enabled(f))
		}
	}

	if err := s.Complete(); err != nil {
		return result, fmt.Errorf("failed to set default options: %v", err)
	}
	if err := s.Validate(); err != nil {
		return result, fmt.Errorf("failed to validate options: %v", err)
	}

	t.Logf("runtime-config=%v", s.APIEnablement.RuntimeConfig)
	t.Logf("Starting apiextensions-apiserver on port %d...", s.RecommendedOptions.SecureServing.BindPort)

	config, err := s.Config()
	if err != nil {
		return result, fmt.Errorf("failed to create config from options: %v", err)
	}

	getOpenAPIDefinitions := openapi.GetOpenAPIDefinitionsWithoutDisabledFeatures(generatedopenapi.GetOpenAPIDefinitions)
	namer := openapinamer.NewDefinitionNamer(extensionsapiserver.Scheme)
	config.GenericConfig.OpenAPIConfig = genericapiserver.DefaultOpenAPIConfig(getOpenAPIDefinitions, namer)

	completedConfig := config.Complete()
	server, err := completedConfig.New(genericapiserver.NewEmptyDelegate())
	if err != nil {
		return result, fmt.Errorf("failed to create server: %v", err)
	}

	errCh = make(chan error)
	go func() {
		defer close(errCh)

		if err := server.GenericAPIServer.PrepareRun().RunWithContext(ctx); err != nil {
			errCh <- err
		}
	}()

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

		result := client.CoreV1().RESTClient().Get().AbsPath("/healthz").Do(context.TODO())
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
	result.ClientConfig = server.GenericAPIServer.LoopbackClientConfig
	result.ServerOpts = s
	result.TearDownFn = tearDown
	result.CompletedConfig = completedConfig

	return result, nil
}

// StartTestServerOrDie calls StartTestServer t.Fatal if it does not succeed.
func StartTestServerOrDie(t Logger, instanceOptions *TestServerInstanceOptions, flags []string, storageConfig *storagebackend.Config) *TestServer {
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
