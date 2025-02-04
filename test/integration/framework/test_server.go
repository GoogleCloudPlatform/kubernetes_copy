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

package framework

import (
	"context"
	"net"
	"net/http"
	"os"
	"path"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	apiextensionsapiserver "k8s.io/apiextensions-apiserver/pkg/apiserver"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/wait"
	genericapiserver "k8s.io/apiserver/pkg/server"
	genericapiserveroptions "k8s.io/apiserver/pkg/server/options"
	"k8s.io/apiserver/pkg/util/compatibility"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	client "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/cert"
	basecompatibility "k8s.io/component-base/compatibility"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	aggregatorscheme "k8s.io/kube-aggregator/pkg/apiserver/scheme"
	netutils "k8s.io/utils/net"

	"k8s.io/kubernetes/cmd/kube-apiserver/app"
	"k8s.io/kubernetes/cmd/kube-apiserver/app/options"
	"k8s.io/kubernetes/pkg/api/legacyscheme"
	"k8s.io/kubernetes/pkg/controlplane"
	controlplaneapiserver "k8s.io/kubernetes/pkg/controlplane/apiserver"
	generatedopenapi "k8s.io/kubernetes/pkg/generated/openapi"
	"k8s.io/kubernetes/test/utils"
)

// This key is for testing purposes only and is not considered secure.
const ecdsaPrivateKey = `-----BEGIN EC PRIVATE KEY-----
MHcCAQEEIEZmTmUhuanLjPA2CLquXivuwBDHTt5XYwgIr/kA1LtRoAoGCCqGSM49
AwEHoUQDQgAEH6cuzP8XuD5wal6wf9M6xDljTOPLX2i8uIp/C/ASqiIGUeeKQtX0
/IR3qCXyThP/dbCiHrF3v1cuhBOHY8CLVg==
-----END EC PRIVATE KEY-----`

// TestServerSetup holds configuration information for a kube-apiserver test server.
type TestServerSetup struct {
	ModifyServerRunOptions func(*options.ServerRunOptions)
	ModifyServerConfig     func(*controlplane.Config)
}

type TearDownFunc func()

// StartTestServer runs a kube-apiserver, optionally calling out to the setup.ModifyServerRunOptions and setup.ModifyServerConfig functions
// TODO (pohly): convert to ktesting contexts
func StartTestServer(ctx context.Context, t testing.TB, setup TestServerSetup) (client.Interface, *rest.Config, TearDownFunc) {
	ctx, cancel := context.WithCancel(ctx)

	certDir, err := os.MkdirTemp("", "test-integration-"+strings.ReplaceAll(t.Name(), "/", "_"))
	if err != nil {
		t.Fatalf("Couldn't create temp dir: %v", err)
	}

	var errCh chan error
	tearDownFn := func() {
		// Calling cancel function is stopping apiserver and cleaning up
		// after itself, including shutting down its storage layer.
		cancel()

		// If the apiserver was started, let's wait for it to
		// shutdown clearly.
		if errCh != nil {
			err, ok := <-errCh
			if ok && err != nil {
				t.Error(err)
			}
		}
		if err := os.RemoveAll(certDir); err != nil {
			t.Log(err)
		}
	}

	_, defaultServiceClusterIPRange, _ := netutils.ParseCIDRSloppy("10.0.0.0/24")
	proxySigningKey, err := utils.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	proxySigningCert, err := cert.NewSelfSignedCACert(cert.Config{CommonName: "front-proxy-ca"}, proxySigningKey)
	if err != nil {
		t.Fatal(err)
	}
	proxyCACertFile, _ := os.CreateTemp(certDir, "proxy-ca.crt")
	if err := os.WriteFile(proxyCACertFile.Name(), utils.EncodeCertPEM(proxySigningCert), 0644); err != nil {
		t.Fatal(err)
	}
	defer proxyCACertFile.Close()
	clientSigningKey, err := utils.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	clientSigningCert, err := cert.NewSelfSignedCACert(cert.Config{CommonName: "client-ca"}, clientSigningKey)
	if err != nil {
		t.Fatal(err)
	}
	clientCACertFile, _ := os.CreateTemp(certDir, "client-ca.crt")
	if err := os.WriteFile(clientCACertFile.Name(), utils.EncodeCertPEM(clientSigningCert), 0644); err != nil {
		t.Fatal(err)
	}
	defer clientCACertFile.Close()
	listener, _, err := genericapiserveroptions.CreateListener("tcp", "127.0.0.1:0", net.ListenConfig{})
	if err != nil {
		t.Fatal(err)
	}

	saSigningKeyFile, err := os.CreateTemp("/tmp", "insecure_test_key")
	if err != nil {
		t.Fatalf("create temp file failed: %v", err)
	}
	defer saSigningKeyFile.Close()
	if err = os.WriteFile(saSigningKeyFile.Name(), []byte(ecdsaPrivateKey), 0666); err != nil {
		t.Fatalf("write file %s failed: %v", saSigningKeyFile.Name(), err)
	}

	opts := options.NewServerRunOptions()
	// If EmulationVersion of DefaultFeatureGate is set during test, we need to propagate it to the apiserver ComponentGlobalsRegistry.
	featureGate := utilfeature.DefaultMutableFeatureGate.DeepCopy()
	effectiveVersion := compatibility.DefaultKubeEffectiveVersionForTest()
	effectiveVersion.SetEmulationVersion(featureGate.EmulationVersion())
	// set up new instance of ComponentGlobalsRegistry instead of using the DefaultComponentGlobalsRegistry to avoid contention in parallel tests.
	componentGlobalsRegistry := basecompatibility.NewComponentGlobalsRegistry()
	if err := componentGlobalsRegistry.Register(basecompatibility.DefaultKubeComponent, effectiveVersion, featureGate); err != nil {
		t.Fatal(err)
	}
	opts.GenericServerRunOptions.ComponentGlobalsRegistry = componentGlobalsRegistry

	opts.SecureServing.Listener = listener
	opts.SecureServing.BindAddress = netutils.ParseIPSloppy("127.0.0.1")
	opts.SecureServing.ServerCert.CertDirectory = certDir
	opts.ServiceAccountSigningKeyFile = saSigningKeyFile.Name()
	opts.Etcd.StorageConfig.Prefix = path.Join("/", uuid.New().String(), "registry")
	opts.Etcd.StorageConfig.Transport.ServerList = []string{GetEtcdURL()}
	opts.ServiceClusterIPRanges = defaultServiceClusterIPRange.String()
	opts.Authentication.RequestHeader.UsernameHeaders = []string{"X-Remote-User"}
	opts.Authentication.RequestHeader.GroupHeaders = []string{"X-Remote-Group"}
	opts.Authentication.RequestHeader.ExtraHeaderPrefixes = []string{"X-Remote-Extra-"}
	opts.Authentication.RequestHeader.AllowedNames = []string{"kube-aggregator"}
	opts.Authentication.RequestHeader.ClientCAFile = proxyCACertFile.Name()
	opts.Authentication.APIAudiences = []string{"https://foo.bar.example.com"}
	opts.Authentication.ServiceAccounts.Issuers = []string{"https://foo.bar.example.com"}
	opts.Authentication.ServiceAccounts.KeyFiles = []string{saSigningKeyFile.Name()}
	opts.Authentication.ClientCert.ClientCA = clientCACertFile.Name()
	opts.Authorization.Modes = []string{"Node", "RBAC"}

	if setup.ModifyServerRunOptions != nil {
		setup.ModifyServerRunOptions(opts)
	}

	// If the local ComponentGlobalsRegistry is changed by ModifyServerRunOptions,
	// we need to copy the new feature values back to the DefaultFeatureGate because most feature checks still use the DefaultFeatureGate.
	if !featureGate.EmulationVersion().EqualTo(utilfeature.DefaultMutableFeatureGate.EmulationVersion()) {
		featuregatetesting.SetFeatureGateEmulationVersionDuringTest(t, utilfeature.DefaultMutableFeatureGate, effectiveVersion.EmulationVersion())
	}
	for f := range utilfeature.DefaultMutableFeatureGate.GetAll() {
		if featureGate.Enabled(f) != utilfeature.DefaultFeatureGate.Enabled(f) {
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, f, featureGate.Enabled(f))
		}
	}
	utilfeature.DefaultMutableFeatureGate.AddMetrics()

	completedOptions, err := opts.Complete(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if errs := completedOptions.Validate(); len(errs) != 0 {
		t.Fatalf("failed to validate ServerRunOptions: %v", utilerrors.NewAggregate(errs))
	}

	genericConfig, versionedInformers, storageFactory, err := controlplaneapiserver.BuildGenericConfig(
		completedOptions.CompletedOptions,
		[]*runtime.Scheme{legacyscheme.Scheme, apiextensionsapiserver.Scheme, aggregatorscheme.Scheme},
		controlplane.DefaultAPIResourceConfigSource(),
		generatedopenapi.GetOpenAPIDefinitions,
	)
	if err != nil {
		t.Fatal(err)
	}

	kubeAPIServerConfig, _, _, err := app.CreateKubeAPIServerConfig(completedOptions, genericConfig, versionedInformers, storageFactory)
	if err != nil {
		t.Fatal(err)
	}

	if setup.ModifyServerConfig != nil {
		setup.ModifyServerConfig(kubeAPIServerConfig)
	}
	kubeAPIServer, err := kubeAPIServerConfig.Complete().New(genericapiserver.NewEmptyDelegate())
	if err != nil {
		t.Fatal(err)
	}

	errCh = make(chan error)
	go func() {
		defer close(errCh)
		if err := kubeAPIServer.ControlPlane.GenericAPIServer.PrepareRun().RunWithContext(ctx); err != nil {
			errCh <- err
		}
	}()

	// Adjust the loopback config for external use (external server name and CA)
	kubeAPIServerClientConfig := rest.CopyConfig(kubeAPIServerConfig.ControlPlane.Generic.LoopbackClientConfig)
	kubeAPIServerClientConfig.CAFile = path.Join(certDir, "apiserver.crt")
	kubeAPIServerClientConfig.CAData = nil
	kubeAPIServerClientConfig.ServerName = ""

	// wait for health
	err = wait.PollImmediate(100*time.Millisecond, 10*time.Second, func() (done bool, err error) {
		select {
		case err := <-errCh:
			return false, err
		default:
		}

		healthzConfig := rest.CopyConfig(kubeAPIServerClientConfig)
		healthzConfig.ContentType = ""
		healthzConfig.AcceptContentTypes = ""
		kubeClient, err := client.NewForConfig(healthzConfig)
		if err != nil {
			// this happens because we race the API server start
			t.Log(err)
			return false, nil
		}

		healthStatus := 0
		kubeClient.Discovery().RESTClient().Get().AbsPath("/healthz").Do(ctx).StatusCode(&healthStatus)
		if healthStatus != http.StatusOK {
			return false, nil
		}

		if _, err := kubeClient.CoreV1().Namespaces().Get(ctx, "default", metav1.GetOptions{}); err != nil {
			return false, nil
		}
		if _, err := kubeClient.CoreV1().Namespaces().Get(ctx, "kube-system", metav1.GetOptions{}); err != nil {
			return false, nil
		}

		return true, nil
	})
	if err != nil {
		t.Fatal(err)
	}

	kubeAPIServerClient, err := client.NewForConfig(kubeAPIServerClientConfig)
	if err != nil {
		t.Fatal(err)
	}

	return kubeAPIServerClient, kubeAPIServerClientConfig, tearDownFn
}
