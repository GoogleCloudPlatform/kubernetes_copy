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

package serving

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"strings"
	"testing"

	"k8s.io/apiserver/pkg/server"
	"k8s.io/apiserver/pkg/server/options"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	cloudprovider "k8s.io/cloud-provider"
	cloudctrlmgrtesting "k8s.io/cloud-provider/app/testing"
	"k8s.io/cloud-provider/fake"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	zpagesfeatures "k8s.io/component-base/zpages/features"
	"k8s.io/klog/v2/ktesting"
	kubeapiservertesting "k8s.io/kubernetes/cmd/kube-apiserver/app/testing"
	kubectrlmgrtesting "k8s.io/kubernetes/cmd/kube-controller-manager/app/testing"
	kubeschedulertesting "k8s.io/kubernetes/cmd/kube-scheduler/app/testing"
	"k8s.io/kubernetes/test/integration/framework"
)

type componentTester interface {
	StartTestServer(ctx context.Context, customFlags []string) (*options.SecureServingOptionsWithLoopback, *server.SecureServingInfo, func(), error)
}

type kubeControllerManagerTester struct{}

func (kubeControllerManagerTester) StartTestServer(ctx context.Context, customFlags []string) (*options.SecureServingOptionsWithLoopback, *server.SecureServingInfo, func(), error) {
	// avoid starting any controller loops, we're just testing serving
	customFlags = append([]string{"--controllers="}, customFlags...)
	gotResult, err := kubectrlmgrtesting.StartTestServer(ctx, customFlags)
	if err != nil {
		return nil, nil, nil, err
	}
	return gotResult.Options.SecureServing, gotResult.Config.SecureServing, gotResult.TearDownFn, err
}

type cloudControllerManagerTester struct{}

func (cloudControllerManagerTester) StartTestServer(ctx context.Context, customFlags []string) (*options.SecureServingOptionsWithLoopback, *server.SecureServingInfo, func(), error) {
	gotResult, err := cloudctrlmgrtesting.StartTestServer(ctx, customFlags)
	if err != nil {
		return nil, nil, nil, err
	}
	return gotResult.Options.SecureServing, gotResult.Config.SecureServing, gotResult.TearDownFn, err
}

type kubeSchedulerTester struct{}

func (kubeSchedulerTester) StartTestServer(ctx context.Context, customFlags []string) (*options.SecureServingOptionsWithLoopback, *server.SecureServingInfo, func(), error) {
	gotResult, err := kubeschedulertesting.StartTestServer(ctx, customFlags)
	if err != nil {
		return nil, nil, nil, err
	}
	return gotResult.Options.SecureServing, gotResult.Config.SecureServing, gotResult.TearDownFn, err
}

func TestComponentSecureServingAndAuth(t *testing.T) {
	if !cloudprovider.IsCloudProvider("fake") {
		cloudprovider.RegisterCloudProvider("fake", fakeCloudProviderFactory)
	}

	// Insulate this test from picking up in-cluster config when run inside a pod
	// We can't assume we have permissions to write to /var/run/secrets/... from a unit test to mock in-cluster config for testing
	if len(os.Getenv("KUBERNETES_SERVICE_HOST")) > 0 {
		t.Setenv("KUBERNETES_SERVICE_HOST", "")
	}

	// authenticate to apiserver via bearer token
	token := "flwqkenfjasasdfmwerasd" // Fake token for testing.
	tokenFile, err := os.CreateTemp("", "kubeconfig")
	if err != nil {
		t.Fatal(err)
	}
	tokenFile.WriteString(fmt.Sprintf(`
%s,system:kube-controller-manager,system:kube-controller-manager,""
`, token))
	tokenFile.Close()

	// start apiserver
	server := kubeapiservertesting.StartTestServerOrDie(t, nil, []string{
		"--token-auth-file", tokenFile.Name(),
		"--authorization-mode", "RBAC",
	}, framework.SharedEtcd())
	defer server.TearDownFn()

	// create kubeconfig for the apiserver
	apiserverConfig, err := os.CreateTemp("", "kubeconfig")
	if err != nil {
		t.Fatal(err)
	}
	apiserverConfig.WriteString(fmt.Sprintf(`
apiVersion: v1
kind: Config
clusters:
- cluster:
    server: %s
    certificate-authority: %s
  name: integration
contexts:
- context:
    cluster: integration
    user: controller-manager
  name: default-context
current-context: default-context
users:
- name: controller-manager
  user:
    token: %s
`, server.ClientConfig.Host, server.ServerOpts.SecureServing.ServerCert.CertKey.CertFile, token))
	apiserverConfig.Close()

	// create BROKEN kubeconfig for the apiserver
	brokenApiserverConfig, err := os.CreateTemp("", "kubeconfig")
	if err != nil {
		t.Fatal(err)
	}
	brokenApiserverConfig.WriteString(fmt.Sprintf(`
apiVersion: v1
kind: Config
clusters:
- cluster:
    server: %s
    certificate-authority: %s
  name: integration
contexts:
- context:
    cluster: integration
    user: controller-manager
  name: default-context
current-context: default-context
users:
- name: controller-manager
  user:
    token: WRONGTOKEN
`, server.ClientConfig.Host, server.ServerOpts.SecureServing.ServerCert.CertKey.CertFile))
	brokenApiserverConfig.Close()

	tests := []struct {
		name       string
		tester     componentTester
		extraFlags []string
	}{
		{"kube-controller-manager", kubeControllerManagerTester{}, nil},
		{"cloud-controller-manager", cloudControllerManagerTester{}, []string{"--cloud-provider=fake", "--webhook-secure-port=0"}},
		{"kube-scheduler", kubeSchedulerTester{}, nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			testComponentWithSecureServing(t, tt.tester, apiserverConfig.Name(), brokenApiserverConfig.Name(), token, tt.extraFlags)
		})
	}
}

func testComponentWithSecureServing(t *testing.T, tester componentTester, kubeconfig, brokenKubeconfig, token string, extraFlags []string) {
	tests := []struct {
		name           string
		flags          []string
		path           string
		anonymous      bool // to use the token or not
		wantErr        bool
		wantSecureCode *int
	}{
		{"no-flags", nil, "/healthz", false, true, nil},
		{"/healthz without authn/authz", []string{
			"--kubeconfig", kubeconfig,
			"--leader-elect=false",
		}, "/healthz", true, false, intPtr(http.StatusOK)},
		{"/metrics without authn/authz", []string{
			"--kubeconfig", kubeconfig,
			"--leader-elect=false",
		}, "/metrics", true, false, intPtr(http.StatusForbidden)},
		{"authorization skipped for /healthz with authn/authz", []string{
			"--authentication-kubeconfig", kubeconfig,
			"--authorization-kubeconfig", kubeconfig,
			"--kubeconfig", kubeconfig,
			"--leader-elect=false",
		}, "/healthz", false, false, intPtr(http.StatusOK)},
		{"authorization skipped for /healthz with BROKEN authn/authz", []string{
			"--authentication-skip-lookup", // to survive inaccessible extensions-apiserver-authentication configmap
			"--authentication-kubeconfig", brokenKubeconfig,
			"--authorization-kubeconfig", brokenKubeconfig,
			"--kubeconfig", kubeconfig,
			"--leader-elect=false",
		}, "/healthz", false, false, intPtr(http.StatusOK)},
		{"not authorized /metrics with BROKEN authn/authz", []string{
			"--authentication-kubeconfig", kubeconfig,
			"--authorization-kubeconfig", brokenKubeconfig,
			"--kubeconfig", kubeconfig,
			"--leader-elect=false",
		}, "/metrics", false, false, intPtr(http.StatusInternalServerError)},
		{"always-allowed /metrics with BROKEN authn/authz", []string{
			"--authentication-skip-lookup", // to survive inaccessible extensions-apiserver-authentication configmap
			"--authentication-kubeconfig", brokenKubeconfig,
			"--authorization-kubeconfig", brokenKubeconfig,
			"--authorization-always-allow-paths", "/healthz,/metrics",
			"--kubeconfig", kubeconfig,
			"--leader-elect=false",
		}, "/metrics", false, false, intPtr(http.StatusOK)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, ctx := ktesting.NewTestContext(t)
			secureOptions, secureInfo, tearDownFn, err := tester.StartTestServer(ctx, append(append([]string{}, tt.flags...), extraFlags...))
			if tearDownFn != nil {
				defer tearDownFn()
			}
			if (err != nil) != tt.wantErr {
				t.Fatalf("StartTestServer() error = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil {
				return
			}

			if want, got := tt.wantSecureCode != nil, secureInfo != nil; want != got {
				t.Errorf("SecureServing enabled: expected=%v got=%v", want, got)
			} else if want {
				url := fmt.Sprintf("https://%s%s", secureInfo.Listener.Addr().String(), tt.path)
				url = strings.Replace(url, "[::]", "127.0.0.1", -1) // switch to IPv4 because the self-signed cert does not support [::]

				// read self-signed server cert disk
				pool := x509.NewCertPool()
				serverCertPath := path.Join(secureOptions.ServerCert.CertDirectory, secureOptions.ServerCert.PairName+".crt")
				serverCert, err := os.ReadFile(serverCertPath)
				if err != nil {
					t.Fatalf("Failed to read component server cert %q: %v", serverCertPath, err)
				}
				pool.AppendCertsFromPEM(serverCert)
				tr := &http.Transport{
					TLSClientConfig: &tls.Config{
						RootCAs: pool,
					},
				}

				client := &http.Client{Transport: tr}
				req, err := http.NewRequest("GET", url, nil)
				if err != nil {
					t.Fatal(err)
				}
				if !tt.anonymous {
					req.Header.Add("Authorization", fmt.Sprintf("Token %s", token))
				}
				r, err := client.Do(req)
				if err != nil {
					t.Fatalf("failed to GET %s from component: %v", tt.path, err)
				}

				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Fatalf("failed to read response body: %v", err)
				}
				defer r.Body.Close()
				if got, expected := r.StatusCode, *tt.wantSecureCode; got != expected {
					t.Fatalf("expected http %d at %s of component, got: %d %q", expected, tt.path, got, string(body))
				}
			}
		})
	}
}

func intPtr(x int) *int {
	return &x
}

func fakeCloudProviderFactory(io.Reader) (cloudprovider.Interface, error) {
	return &fake.Cloud{
		DisableRoutes: true, // disable routes for server tests, otherwise --cluster-cidr is required
	}, nil
}

func TestKubeControllerManagerServingStatusz(t *testing.T) {

	// authenticate to apiserver via bearer token
	token := "flwqkenfjasasdfmwerasd" // Fake token for testing.
	tokenFile, err := os.CreateTemp("", "kubeconfig")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tokenFile.WriteString(fmt.Sprintf(`
%s,system:kube-controller-manager,system:kube-controller-manager,""
`, token)); err != nil {
		t.Fatal(err)
	}
	if err = tokenFile.Close(); err != nil {
		t.Fatal(err)
	}

	// start apiserver
	server := kubeapiservertesting.StartTestServerOrDie(t, nil, []string{
		"--token-auth-file", tokenFile.Name(),
		"--authorization-mode", "RBAC",
	}, framework.SharedEtcd())
	defer server.TearDownFn()

	// create kubeconfig for the apiserver
	apiserverConfig, err := os.CreateTemp("", "kubeconfig")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = apiserverConfig.WriteString(fmt.Sprintf(`
apiVersion: v1
kind: Config
clusters:
- cluster:
    server: %s
    certificate-authority: %s
  name: integration
contexts:
- context:
    cluster: integration
    user: controller-manager
  name: default-context
current-context: default-context
users:
- name: controller-manager
  user:
    token: %s
`, server.ClientConfig.Host, server.ServerOpts.SecureServing.ServerCert.CertKey.CertFile, token)); err != nil {
		t.Fatal(err)
	}
	if err = apiserverConfig.Close(); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name           string
		flags          []string
		path           string
		anonymous      bool // to use the token or not
		wantErr        bool
		wantSecureCode *int
	}{
		{"serving /statusz", []string{
			"--authentication-skip-lookup", // to survive inaccessible extensions-apiserver-authentication configmap
			"--authentication-kubeconfig", apiserverConfig.Name(),
			"--authorization-kubeconfig", apiserverConfig.Name(),
			"--authorization-always-allow-paths", "/statusz",
			"--kubeconfig", apiserverConfig.Name(),
			"--leader-elect=false",
		}, "/statusz", false, false, intPtr(http.StatusOK)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, zpagesfeatures.ComponentStatusz, true)
			_, ctx := ktesting.NewTestContext(t)
			secureOptions, secureInfo, tearDownFn, err := kubeControllerManagerTester{}.StartTestServer(ctx, append(append([]string{}, tt.flags...), []string{}...))
			if tearDownFn != nil {
				defer tearDownFn()
			}
			if (err != nil) != tt.wantErr {
				t.Fatalf("StartTestServer() error = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil {
				return
			}

			if want, got := tt.wantSecureCode != nil, secureInfo != nil; want != got {
				t.Errorf("SecureServing enabled: expected=%v got=%v", want, got)
			} else if want {
				url := fmt.Sprintf("https://%s%s", secureInfo.Listener.Addr().String(), tt.path)
				url = strings.ReplaceAll(url, "[::]", "127.0.0.1") // switch to IPv4 because the self-signed cert does not support [::]

				// read self-signed server cert disk
				pool := x509.NewCertPool()
				serverCertPath := path.Join(secureOptions.ServerCert.CertDirectory, secureOptions.ServerCert.PairName+".crt")
				serverCert, err := os.ReadFile(serverCertPath)
				if err != nil {
					t.Fatalf("Failed to read component server cert %q: %v", serverCertPath, err)
				}
				pool.AppendCertsFromPEM(serverCert)
				tr := &http.Transport{
					TLSClientConfig: &tls.Config{
						RootCAs: pool,
					},
				}

				client := &http.Client{Transport: tr}
				req, err := http.NewRequest(http.MethodGet, url, nil)
				req.Header.Set("Accept", "text/plain")
				if err != nil {
					t.Fatal(err)
				}
				if !tt.anonymous {
					req.Header.Add("Authorization", fmt.Sprintf("Token %s", token))
				}
				r, err := client.Do(req)
				if err != nil {
					t.Fatalf("failed to GET %s from component: %v", tt.path, err)
				}

				if _, err = io.ReadAll(r.Body); err != nil {
					t.Fatalf("failed to read response body: %v", err)
				}
				defer func() {
					if err := r.Body.Close(); err != nil {
						t.Fatalf("Error closing response body: %v", err)
					}
				}()

				if got, expected := r.StatusCode, *tt.wantSecureCode; got != expected {
					t.Fatalf("expected http %d at %s of component, got: %d", expected, tt.path, got)
				}
			}
		})
	}
}
