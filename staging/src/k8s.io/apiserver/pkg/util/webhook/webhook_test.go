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

package webhook

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	v1 "k8s.io/client-go/tools/clientcmd/api/v1"
	"k8s.io/component-base/metrics"
	"k8s.io/component-base/metrics/legacyregistry"
)

const (
	errBadCertificate    = "Get .*: remote error: tls: (bad certificate|unknown certificate authority)"
	errNoConfiguration   = "invalid configuration: no configuration has been provided"
	errMissingCertPath   = "invalid configuration: unable to read %s %s for %s due to open %s: .*"
	errSignedByUnknownCA = "Get .*: x509: .*(unknown authority|not standards compliant|not trusted)"
)

var (
	defaultCluster = v1.NamedCluster{
		Cluster: v1.Cluster{
			Server:                   "https://webhook.example.com",
			CertificateAuthorityData: caCert,
		},
	}
	defaultUser = v1.NamedAuthInfo{
		AuthInfo: v1.AuthInfo{
			ClientCertificateData: clientCert,
			ClientKeyData:         clientKey,
		},
	}
	namedCluster = v1.NamedCluster{
		Cluster: v1.Cluster{
			Server:                   "https://webhook.example.com",
			CertificateAuthorityData: caCert,
		},
		Name: "test-cluster",
	}
	groupVersions = []schema.GroupVersion{}
	retryBackoff  = DefaultRetryBackoffWithInitialDelay(time.Duration(500) * time.Millisecond)
)

// TestKubeConfigFile ensures that a kube config file, regardless of validity, is handled properly
func TestKubeConfigFile(t *testing.T) {
	badCAPath := "/tmp/missing/ca.pem"
	badClientCertPath := "/tmp/missing/client.pem"
	badClientKeyPath := "/tmp/missing/client-key.pem"
	dir := bootstrapTestDir(t)

	defer os.RemoveAll(dir)

	// These tests check for all of the ways in which a Kubernetes config file could be malformed within the context of
	// configuring a webhook.  Configuration issues that arise while using the webhook are tested elsewhere.
	tests := []struct {
		test           string
		cluster        *v1.NamedCluster
		context        *v1.NamedContext
		currentContext string
		user           *v1.NamedAuthInfo
		errRegex       string
	}{
		{
			test:     "missing context (no default, none specified)",
			cluster:  &namedCluster,
			errRegex: errNoConfiguration,
		},
		{
			test:     "missing context (specified context is missing)",
			cluster:  &namedCluster,
			errRegex: errNoConfiguration,
		},
		{
			test: "context without cluster",
			context: &v1.NamedContext{
				Context: v1.Context{},
				Name:    "testing-context",
			},
			currentContext: "testing-context",
			errRegex:       errNoConfiguration,
		},
		{
			test:    "context without user",
			cluster: &namedCluster,
			context: &v1.NamedContext{
				Context: v1.Context{
					Cluster: namedCluster.Name,
				},
				Name: "testing-context",
			},
			currentContext: "testing-context",
			errRegex:       "", // Not an error at parse time, only when using the webhook
		},
		{
			test:    "context with missing cluster",
			cluster: &namedCluster,
			context: &v1.NamedContext{
				Context: v1.Context{
					Cluster: "missing-cluster",
				},
				Name: "fake",
			},
			errRegex: errNoConfiguration,
		},
		{
			test:    "context with missing user",
			cluster: &namedCluster,
			context: &v1.NamedContext{
				Context: v1.Context{
					Cluster:  namedCluster.Name,
					AuthInfo: "missing-user",
				},
				Name: "testing-context",
			},
			currentContext: "testing-context",
			errRegex:       "", // Not an error at parse time, only when using the webhook
		},
		{
			test: "cluster with invalid CA certificate path",
			cluster: &v1.NamedCluster{
				Cluster: v1.Cluster{
					Server:               namedCluster.Cluster.Server,
					CertificateAuthority: badCAPath,
				},
			},
			user:     &defaultUser,
			errRegex: fmt.Sprintf(errMissingCertPath, "certificate-authority", badCAPath, "", badCAPath),
		},
		{
			test: "cluster with invalid CA certificate",
			cluster: &v1.NamedCluster{
				Cluster: v1.Cluster{
					Server:                   namedCluster.Cluster.Server,
					CertificateAuthorityData: caKey, // pretend user put caKey here instead of caCert
				},
			},
			user:     &defaultUser,
			errRegex: "unable to load root certificates: no valid certificate authority data seen",
		},
		{
			test: "cluster with invalid CA certificate - no PEM",
			cluster: &v1.NamedCluster{
				Cluster: v1.Cluster{
					Server:                   namedCluster.Cluster.Server,
					CertificateAuthorityData: []byte(`not a cert`),
				},
			},
			user:     &defaultUser,
			errRegex: "unable to load root certificates: unable to parse bytes as PEM block",
		},
		{
			test: "cluster with invalid CA certificate - parse error",
			cluster: &v1.NamedCluster{
				Cluster: v1.Cluster{
					Server: namedCluster.Cluster.Server,
					CertificateAuthorityData: []byte(`
-----BEGIN CERTIFICATE-----
MIIDGTCCAgGgAwIBAgIUOS2M
-----END CERTIFICATE-----
`),
				},
			},
			user:     &defaultUser,
			errRegex: "unable to load root certificates: failed to parse certificate: (asn1: syntax error: data truncated|x509: malformed certificate)",
		},
		{
			test:    "user with invalid client certificate path",
			cluster: &defaultCluster,
			user: &v1.NamedAuthInfo{
				AuthInfo: v1.AuthInfo{
					ClientCertificate: badClientCertPath,
					ClientKeyData:     defaultUser.AuthInfo.ClientKeyData,
				},
			},
			errRegex: fmt.Sprintf(errMissingCertPath, "client-cert", badClientCertPath, "", badClientCertPath),
		},
		{
			test:    "user with invalid client certificate",
			cluster: &defaultCluster,
			user: &v1.NamedAuthInfo{
				AuthInfo: v1.AuthInfo{
					ClientCertificateData: clientKey,
					ClientKeyData:         defaultUser.AuthInfo.ClientKeyData,
				},
			},
			errRegex: "tls: failed to find certificate PEM data in certificate input, but did find a private key; PEM inputs may have been switched",
		},
		{
			test:    "user with invalid client certificate path",
			cluster: &defaultCluster,
			user: &v1.NamedAuthInfo{
				AuthInfo: v1.AuthInfo{
					ClientCertificateData: defaultUser.AuthInfo.ClientCertificateData,
					ClientKey:             badClientKeyPath,
				},
			},
			errRegex: fmt.Sprintf(errMissingCertPath, "client-key", badClientKeyPath, "", badClientKeyPath),
		},
		{
			test:    "user with invalid client certificate",
			cluster: &defaultCluster,
			user: &v1.NamedAuthInfo{
				AuthInfo: v1.AuthInfo{
					ClientCertificateData: defaultUser.AuthInfo.ClientCertificateData,
					ClientKeyData:         clientCert,
				},
			},
			errRegex: "tls: found a certificate rather than a key in the PEM for the private key",
		},
		{
			test:     "valid configuration (certificate data embedded in config)",
			cluster:  &defaultCluster,
			user:     &defaultUser,
			errRegex: "",
		},
		{
			test: "valid configuration (certificate files referenced in config)",
			cluster: &v1.NamedCluster{
				Cluster: v1.Cluster{
					Server:               "https://webhook.example.com",
					CertificateAuthority: filepath.Join(dir, "ca.pem"),
				},
			},
			user: &v1.NamedAuthInfo{
				AuthInfo: v1.AuthInfo{
					ClientCertificate: filepath.Join(dir, "client.pem"),
					ClientKey:         filepath.Join(dir, "client-key.pem"),
				},
			},
			errRegex: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.test, func(t *testing.T) {
			// Use a closure so defer statements trigger between loop iterations.
			err := func() error {
				kubeConfig := v1.Config{}

				if tt.cluster != nil {
					kubeConfig.Clusters = []v1.NamedCluster{*tt.cluster}
				}

				if tt.context != nil {
					kubeConfig.Contexts = []v1.NamedContext{*tt.context}
				}

				if tt.user != nil {
					kubeConfig.AuthInfos = []v1.NamedAuthInfo{*tt.user}
				}

				kubeConfig.CurrentContext = tt.currentContext

				kubeConfigFile, err := newKubeConfigFile(kubeConfig)
				if err != nil {
					return err
				}

				defer os.Remove(kubeConfigFile)

				config, err := LoadKubeconfig(kubeConfigFile, nil)
				if err != nil {
					return err
				}

				_, err = NewGenericWebhook(runtime.NewScheme(), scheme.Codecs, config, groupVersions, retryBackoff)
				return err
			}()

			if err == nil {
				if tt.errRegex != "" {
					t.Errorf("%s: expected an error", tt.test)
				}
			} else {
				if tt.errRegex == "" {
					t.Errorf("%s: unexpected error: %v", tt.test, err)
				} else if !regexp.MustCompile(tt.errRegex).MatchString(err.Error()) {
					t.Errorf("%s: unexpected error message to match:\n  Expected: %s\n  Actual:   %s", tt.test, tt.errRegex, err.Error())
				}
			}
		})
	}
}

// TestMissingKubeConfigFile ensures that a kube config path to a missing file is handled properly
func TestMissingKubeConfigFile(t *testing.T) {
	kubeConfigPath := "/some/missing/path"
	_, err := LoadKubeconfig(kubeConfigPath, nil)

	if err == nil {
		t.Errorf("creating the webhook should had failed")
	} else if strings.Index(err.Error(), fmt.Sprintf("stat %s", kubeConfigPath)) != 0 {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestTLSConfig ensures that the TLS-based communication between client and server works as expected
func TestTLSConfig(t *testing.T) {
	invalidCert := []byte("invalid")
	tests := []struct {
		test                             string
		clientCert, clientKey, clientCA  []byte
		serverCert, serverKey, serverCA  []byte
		errRegex                         string
		increaseSANWarnCounter           bool
		increaseSHA1SignatureWarnCounter bool
	}{
		{
			test:       "invalid server CA",
			clientCert: clientCert, clientKey: clientKey, clientCA: caCert,
			serverCert: serverCert, serverKey: serverKey, serverCA: invalidCert,
			errRegex: errBadCertificate,
		},
		{
			test:       "invalid client certificate",
			clientCert: invalidCert, clientKey: clientKey, clientCA: caCert,
			serverCert: serverCert, serverKey: serverKey, serverCA: caCert,
			errRegex: "tls: failed to find any PEM data in certificate input",
		},
		{
			test:       "invalid client key",
			clientCert: clientCert, clientKey: invalidCert, clientCA: caCert,
			serverCert: serverCert, serverKey: serverKey, serverCA: caCert,
			errRegex: "tls: failed to find any PEM data in key input",
		},
		{
			test:       "client does not trust server",
			clientCert: clientCert, clientKey: clientKey,
			serverCert: serverCert, serverKey: serverKey,
			errRegex: errSignedByUnknownCA,
		},
		{
			test:       "server does not trust client",
			clientCert: clientCert, clientKey: clientKey, clientCA: badCACert,
			serverCert: serverCert, serverKey: serverKey, serverCA: caCert,
			errRegex: errSignedByUnknownCA + " .*",
		},
		{
			test:       "server requires auth, client provides it",
			clientCert: clientCert, clientKey: clientKey, clientCA: caCert,
			serverCert: serverCert, serverKey: serverKey, serverCA: caCert,
			errRegex: "",
		},
		{
			test:       "server does not require client auth",
			clientCA:   caCert,
			serverCert: serverCert, serverKey: serverKey,
			errRegex: "",
		},
		{
			test:       "server does not require client auth, client provides it",
			clientCert: clientCert, clientKey: clientKey, clientCA: caCert,
			serverCert: serverCert, serverKey: serverKey,
			errRegex: "",
		},
		{
			test:       "webhook does not support insecure servers",
			serverCert: serverCert, serverKey: serverKey,
			errRegex: errSignedByUnknownCA,
		},
		{
			// this will fail when GODEBUG is set to x509ignoreCN=0 with
			// expected err, but the SAN counter gets increased
			test:       "server cert does not have SAN extension",
			clientCA:   caCert,
			serverCert: serverCertNoSAN, serverKey: serverKey,
			errRegex:               "x509: certificate relies on legacy Common Name field",
			increaseSANWarnCounter: true,
		},
		{
			test:       "server cert with SHA1 signature",
			clientCA:   caCert,
			serverCert: append(append(sha1ServerCertInter, byte('\n')), caCertInter...), serverKey: serverKey,
			errRegex:                         "x509: cannot verify signature: insecure algorithm SHA1-RSA",
			increaseSHA1SignatureWarnCounter: true,
		},
		{
			test:       "server cert signed by an intermediate CA with SHA1 signature",
			clientCA:   caCert,
			serverCert: append(append(serverCertInterSHA1, byte('\n')), caCertInterSHA1...), serverKey: serverKey,
			errRegex:                         "x509: cannot verify signature: insecure algorithm SHA1-RSA",
			increaseSHA1SignatureWarnCounter: true,
		},
	}

	lastSHA1SigCounter := 0
	for _, tt := range tests {
		// Use a closure so defer statements trigger between loop iterations.
		func() {
			// Create and start a simple HTTPS server
			server, err := newTestServer(tt.serverCert, tt.serverKey, tt.serverCA, nil)
			if err != nil {
				t.Errorf("%s: failed to create server: %v", tt.test, err)
				return
			}

			serverURL, err := url.Parse(server.URL)
			if err != nil {
				t.Errorf("%s: failed to parse the testserver URL: %v", tt.test, err)
				return
			}
			serverURL.Host = net.JoinHostPort("localhost", serverURL.Port())

			defer server.Close()

			// Create a Kubernetes client configuration file
			configFile, err := newKubeConfigFile(v1.Config{
				Clusters: []v1.NamedCluster{
					{
						Cluster: v1.Cluster{
							Server:                   serverURL.String(),
							CertificateAuthorityData: tt.clientCA,
						},
					},
				},
				AuthInfos: []v1.NamedAuthInfo{
					{
						AuthInfo: v1.AuthInfo{
							ClientCertificateData: tt.clientCert,
							ClientKeyData:         tt.clientKey,
						},
					},
				},
			})

			if err != nil {
				t.Errorf("%s: %v", tt.test, err)
				return
			}

			defer os.Remove(configFile)

			config, err := LoadKubeconfig(configFile, nil)
			if err != nil {
				t.Fatal(err)
			}

			wh, err := NewGenericWebhook(runtime.NewScheme(), scheme.Codecs, config, groupVersions, retryBackoff)

			if err == nil {
				err = wh.RestClient.Get().Do(context.TODO()).Error()
			}

			if err == nil {
				if tt.errRegex != "" {
					t.Errorf("%s: expected an error", tt.test)
				}
			} else {
				if tt.errRegex == "" {
					t.Errorf("%s: unexpected error: %v", tt.test, err)
				} else if !regexp.MustCompile(tt.errRegex).MatchString(err.Error()) {
					t.Errorf("%s: unexpected error message mismatch:\n  Expected: %s\n  Actual:   %s", tt.test, tt.errRegex, err.Error())
				}
			}

			if tt.increaseSANWarnCounter {
				errorCounter := getSingleCounterValueFromRegistry(t, legacyregistry.DefaultGatherer, "apiserver_webhooks_x509_missing_san_total")

				if errorCounter == -1 {
					t.Errorf("failed to get the x509_common_name_error_count metrics: %v", err)
				}
				if int(errorCounter) != 1 {
					t.Errorf("expected the x509_common_name_error_count to be 1, but it's %d", errorCounter)
				}
			}

			if tt.increaseSHA1SignatureWarnCounter {
				errorCounter := getSingleCounterValueFromRegistry(t, legacyregistry.DefaultGatherer, "apiserver_webhooks_x509_insecure_sha1_total")

				if errorCounter == -1 {
					t.Errorf("failed to get the apiserver_webhooks_x509_insecure_sha1_total metrics: %v", err)
				}

				if int(errorCounter) != lastSHA1SigCounter+1 {
					t.Errorf("expected the apiserver_webhooks_x509_insecure_sha1_total counter to be 1, but it's %d", errorCounter)
				}

				lastSHA1SigCounter++
			}
		}()
	}
}

func TestRequestTimeout(t *testing.T) {
	done := make(chan struct{})

	handler := func(w http.ResponseWriter, r *http.Request) {
		<-done
	}

	// Create and start a simple HTTPS server
	server, err := newTestServer(clientCert, clientKey, caCert, handler)
	if err != nil {
		t.Errorf("failed to create server: %v", err)
		return
	}
	defer server.Close()
	defer close(done) // done channel must be closed before server is.

	// Create a Kubernetes client configuration file
	configFile, err := newKubeConfigFile(v1.Config{
		Clusters: []v1.NamedCluster{
			{
				Cluster: v1.Cluster{
					Server:                   server.URL,
					CertificateAuthorityData: caCert,
				},
			},
		},
		AuthInfos: []v1.NamedAuthInfo{
			{
				AuthInfo: v1.AuthInfo{
					ClientCertificateData: clientCert,
					ClientKeyData:         clientKey,
				},
			},
		},
	})
	if err != nil {
		t.Errorf("failed to create the client config file: %v", err)
		return
	}
	defer os.Remove(configFile)

	var requestTimeout = 10 * time.Millisecond

	config, err := LoadKubeconfig(configFile, nil)
	if err != nil {
		t.Fatal(err)
	}

	config.Timeout = requestTimeout

	wh, err := NewGenericWebhook(runtime.NewScheme(), scheme.Codecs, config, groupVersions, retryBackoff)
	if err != nil {
		t.Fatalf("failed to create the webhook: %v", err)
	}

	resultCh := make(chan rest.Result)

	go func() { resultCh <- wh.RestClient.Get().Do(context.TODO()) }()
	select {
	case <-time.After(time.Second * 5):
		t.Errorf("expected request to timeout after %s", requestTimeout)
	case <-resultCh:
	}
}

// TestWithExponentialBackoff ensures that the webhook's exponential backoff support works as expected
func TestWithExponentialBackoff(t *testing.T) {
	count := 0 // To keep track of the requests
	gr := schema.GroupResource{
		Group:    "webhook.util.k8s.io",
		Resource: "test",
	}

	// Handler that will handle all backoff CONDITIONS
	ebHandler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch count++; count {
		case 1:
			// Timeout error with retry supplied
			w.WriteHeader(http.StatusGatewayTimeout)
			json.NewEncoder(w).Encode(apierrors.NewServerTimeout(gr, "get", 2))
		case 2:
			// Internal server error
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(apierrors.NewInternalError(fmt.Errorf("nope")))
		case 3:
			// HTTP error that is not retryable
			w.WriteHeader(http.StatusNotAcceptable)
			json.NewEncoder(w).Encode(apierrors.NewGenericServerResponse(http.StatusNotAcceptable, "get", gr, "testing", "nope", 0, false))
		case 4:
			// Successful request
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]string{
				"status": "OK",
			})
		}
	}

	// Create and start a simple HTTPS server
	server, err := newTestServer(clientCert, clientKey, caCert, ebHandler)

	if err != nil {
		t.Errorf("failed to create server: %v", err)
		return
	}

	defer server.Close()

	// Create a Kubernetes client configuration file
	configFile, err := newKubeConfigFile(v1.Config{
		Clusters: []v1.NamedCluster{
			{
				Cluster: v1.Cluster{
					Server:                   server.URL,
					CertificateAuthorityData: caCert,
				},
			},
		},
		AuthInfos: []v1.NamedAuthInfo{
			{
				AuthInfo: v1.AuthInfo{
					ClientCertificateData: clientCert,
					ClientKeyData:         clientKey,
				},
			},
		},
	})

	if err != nil {
		t.Errorf("failed to create the client config file: %v", err)
		return
	}

	defer os.Remove(configFile)

	config, err := LoadKubeconfig(configFile, nil)
	if err != nil {
		t.Fatal(err)
	}

	wh, err := NewGenericWebhook(runtime.NewScheme(), scheme.Codecs, config, groupVersions, retryBackoff)

	if err != nil {
		t.Fatalf("failed to create the webhook: %v", err)
	}

	result := wh.WithExponentialBackoff(context.Background(), func() rest.Result {
		return wh.RestClient.Get().Do(context.TODO())
	})

	var statusCode int

	result.StatusCode(&statusCode)

	if statusCode != http.StatusNotAcceptable {
		t.Errorf("unexpected status code: %d", statusCode)
	}

	result = wh.WithExponentialBackoff(context.Background(), func() rest.Result {
		return wh.RestClient.Get().Do(context.TODO())
	})

	result.StatusCode(&statusCode)

	if statusCode != http.StatusOK {
		t.Errorf("unexpected status code: %d", statusCode)
	}
}

func bootstrapTestDir(t *testing.T) string {
	dir, err := os.MkdirTemp("", "")

	if err != nil {
		t.Fatal(err)
	}

	// The certificates needed on disk for the tests
	files := map[string][]byte{
		"ca.pem":         caCert,
		"client.pem":     clientCert,
		"client-key.pem": clientKey,
	}

	// Write the certificate files to disk or fail
	for fileName, fileData := range files {
		if err := os.WriteFile(filepath.Join(dir, fileName), fileData, 0400); err != nil {
			os.RemoveAll(dir)
			t.Fatal(err)
		}
	}

	return dir
}

func newKubeConfigFile(config v1.Config) (string, error) {
	configFile, err := os.CreateTemp("", "")
	if err != nil {
		return "", err
	}
	defer configFile.Close()

	if err != nil {
		return "", fmt.Errorf("unable to create the Kubernetes client config file: %v", err)
	}

	if err = json.NewEncoder(configFile).Encode(config); err != nil {
		return "", fmt.Errorf("unable to write the Kubernetes client configuration to disk: %v", err)
	}

	return configFile.Name(), nil
}

func newTestServer(clientCert, clientKey, caCert []byte, handler func(http.ResponseWriter, *http.Request)) (*httptest.Server, error) {
	var tlsConfig *tls.Config

	if clientCert != nil {
		cert, err := tls.X509KeyPair(clientCert, clientKey)

		if err != nil {
			return nil, err
		}

		tlsConfig = &tls.Config{
			Certificates: []tls.Certificate{cert},
		}
	}

	if caCert != nil {
		rootCAs := x509.NewCertPool()

		rootCAs.AppendCertsFromPEM(caCert)

		if tlsConfig == nil {
			tlsConfig = &tls.Config{}
		}

		tlsConfig.ClientCAs = rootCAs
		tlsConfig.ClientAuth = tls.RequireAndVerifyClientCert
	}

	if handler == nil {
		handler = func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte("OK"))
		}
	}

	server := httptest.NewUnstartedServer(http.HandlerFunc(handler))

	server.TLS = tlsConfig
	server.StartTLS()

	return server, nil
}

func TestWithExponentialBackoffContextIsAlreadyCanceled(t *testing.T) {
	alwaysRetry := func(e error) bool {
		return true
	}

	attemptsGot := 0
	webhookFunc := func() error {
		attemptsGot++
		return nil
	}

	ctx, cancel := context.WithCancel(context.TODO())
	cancel()

	// We don't expect the webhook function to be called since the context is already canceled.
	retryBackoff := wait.Backoff{Steps: 5}
	err := WithExponentialBackoff(ctx, retryBackoff, webhookFunc, alwaysRetry)

	errExpected := fmt.Errorf("webhook call failed: %s", context.Canceled)
	if errExpected.Error() != err.Error() {
		t.Errorf("expected error: %v, but got: %v", errExpected, err)
	}
	if attemptsGot != 0 {
		t.Errorf("expected %d webhook attempts, but got: %d", 0, attemptsGot)
	}
}

func TestWithExponentialBackoffWebhookErrorIsMostImportant(t *testing.T) {
	alwaysRetry := func(e error) bool {
		return true
	}

	ctx, cancel := context.WithCancel(context.TODO())
	attemptsGot := 0
	errExpected := errors.New("webhook not available")
	webhookFunc := func() error {
		attemptsGot++

		// after the first attempt, the context is canceled
		cancel()

		return errExpected
	}

	// webhook err has higher priority than ctx error. we expect the webhook error to be returned.
	retryBackoff := wait.Backoff{Steps: 5}
	err := WithExponentialBackoff(ctx, retryBackoff, webhookFunc, alwaysRetry)

	if attemptsGot != 1 {
		t.Errorf("expected %d webhook attempts, but got: %d", 1, attemptsGot)
	}
	if errExpected != err {
		t.Errorf("expected error: %v, but got: %v", errExpected, err)
	}
}

func TestWithExponentialBackoffWithRetryExhaustedWhileContextIsNotCanceled(t *testing.T) {
	alwaysRetry := func(e error) bool {
		return true
	}

	ctx, cancel := context.WithCancel(context.TODO())
	defer cancel()

	attemptsGot := 0
	errExpected := errors.New("webhook not available")
	webhookFunc := func() error {
		attemptsGot++
		return errExpected
	}

	// webhook err has higher priority than ctx error. we expect the webhook error to be returned.
	retryBackoff := wait.Backoff{Steps: 5}
	err := WithExponentialBackoff(ctx, retryBackoff, webhookFunc, alwaysRetry)

	if attemptsGot != 5 {
		t.Errorf("expected %d webhook attempts, but got: %d", 1, attemptsGot)
	}
	if errExpected != err {
		t.Errorf("expected error: %v, but got: %v", errExpected, err)
	}
}

func TestWithExponentialBackoffParametersNotSet(t *testing.T) {
	alwaysRetry := func(e error) bool {
		return true
	}

	attemptsGot := 0
	webhookFunc := func() error {
		attemptsGot++
		return nil
	}

	err := WithExponentialBackoff(context.TODO(), wait.Backoff{}, webhookFunc, alwaysRetry)

	errExpected := fmt.Errorf("webhook call failed: %s", wait.ErrWaitTimeout)
	if errExpected.Error() != err.Error() {
		t.Errorf("expected error: %v, but got: %v", errExpected, err)
	}
	if attemptsGot != 0 {
		t.Errorf("expected %d webhook attempts, but got: %d", 0, attemptsGot)
	}
}

func TestGenericWebhookWithExponentialBackoff(t *testing.T) {
	attemptsPerCallExpected := 5
	webhook := &GenericWebhook{
		RetryBackoff: wait.Backoff{
			Duration: time.Millisecond,
			Factor:   1.5,
			Jitter:   0.2,
			Steps:    attemptsPerCallExpected,
		},

		ShouldRetry: func(e error) bool {
			return true
		},
	}

	attemptsGot := 0
	webhookFunc := func() rest.Result {
		attemptsGot++
		return rest.Result{}
	}

	// number of retries should always be local to each call.
	totalAttemptsExpected := attemptsPerCallExpected * 2
	webhook.WithExponentialBackoff(context.TODO(), webhookFunc)
	webhook.WithExponentialBackoff(context.TODO(), webhookFunc)

	if totalAttemptsExpected != attemptsGot {
		t.Errorf("expected a total of %d webhook attempts but got: %d", totalAttemptsExpected, attemptsGot)
	}
}

func getSingleCounterValueFromRegistry(t *testing.T, r metrics.Gatherer, name string) int {
	mfs, err := r.Gather()
	if err != nil {
		t.Logf("failed to gather local registry metrics: %v", err)
		return -1
	}

	for _, mf := range mfs {
		if mf.Name != nil && *mf.Name == name {
			mfMetric := mf.GetMetric()
			for _, m := range mfMetric {
				if m.GetCounter() != nil {
					return int(m.GetCounter().GetValue())
				}
			}
		}
	}

	return -1
}
