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

package podlogs

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apiserver/pkg/authentication/authenticatorfactory"
	"k8s.io/apiserver/pkg/endpoints/filters"
	"k8s.io/apiserver/pkg/server/dynamiccertificates"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/transport"
	certutil "k8s.io/client-go/util/cert"
	"k8s.io/client-go/util/keyutil"
	"k8s.io/kubernetes/cmd/kube-apiserver/app/options"
	"k8s.io/kubernetes/test/integration/framework"
	"k8s.io/kubernetes/test/utils/ktesting"
)

func TestInsecurePodLogs(t *testing.T) {
	badCA := writeDataToTempFile(t, []byte(`
-----BEGIN CERTIFICATE-----
MIIDMDCCAhigAwIBAgIIHNPD7sig7YIwDQYJKoZIhvcNAQELBQAwNjESMBAGA1UE
CxMJb3BlbnNoaWZ0MSAwHgYDVQQDExdhZG1pbi1rdWJlY29uZmlnLXNpZ25lcjAe
Fw0xOTA1MzAxNTA3MzlaFw0yOTA1MjcxNTA3MzlaMDYxEjAQBgNVBAsTCW9wZW5z
aGlmdDEgMB4GA1UEAxMXYWRtaW4ta3ViZWNvbmZpZy1zaWduZXIwggEiMA0GCSqG
SIb3DQEBAQUAA4IBDwAwggEKAoIBAQD0dHk23lHRcuq06FzYDOl9J9+s8pnGxqA3
IPcARI6ag/98aYe3ENwAB5e1i7AU2F2WiDZgj444w374XLdVgIK8zgQEm9yoqrlc
+/ayO7ceKklrKHOMwh63LvGLEOqzhol2nFmBhXAZt+HyIoZHXN0IqlA92196+Dml
0WOn1F4ce6JbAtEceFHPgLeI7KFmVaPz2796pBXh23ii6r7WvV1Rn9MKlMSBJQR4
0LZzu9/j+GdnFXewdLAAMfgPzwEqv6h3PzvtUCjgdraHEm8Rs7s15S3PUmLK4RQS
PsThx5BhJEGd/W6EzQ3BKoQfochhu3mnAQtW1J07CullySQ5Gg9fAgMBAAGjQjBA
MA4GA1UdDwEB/wQEAwICpDAPBgNVHRMBAf8EBTADAQH/MB0GA1UdDgQWBBQkTaaw
YJSZ5k2Wd+OsM4GFMTGdqzANBgkqhkiG9w0BAQsFAAOCAQEAHK7+zBZPLqK+f9DT
UEnpwRmZ0aeGS4YgbGIkqpjxJymVOwkRd5A1wslvVfGZ6yOQthF6KlCmqnPyJJMR
I7FHw8j0h2ci90fEQ6IS90Y/ZJXkcgiK9Ncwa35GFGs8QrBxN4leGhtm84BnnBHN
cTWpa4zcBwru0CRG7iHc66VX16X8jHB1iFeZ5W/FgY4MsE+G1Vze4mCXSPVI4BZ2
/qlAgogjBivvSwQ9SFuCszg7IPjvT2ksm+Cf+8eT4YBqW41F85vBGR+FYK14yIla
Bgqc+dJN9xS9Ah5gLiGQJ6C4niUA11piCpvMsy+j/LQ1Erx47KMar5fuMXYk7iPq
1vqIwg==
-----END CERTIFICATE-----
`))

	tCtx := ktesting.Init(t)
	clientSet, _, tearDownFn := framework.StartTestServer(tCtx, t, framework.TestServerSetup{
		ModifyServerRunOptions: func(opts *options.ServerRunOptions) {
			opts.GenericServerRunOptions.MaxRequestBodyBytes = 1024 * 1024
			// I have no idea what this cert is, but it doesn't matter, we just want something that always fails validation
			opts.KubeletConfig.TLSClientConfig.CAFile = badCA
		},
	})
	defer tearDownFn()

	fakeKubeletServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Write([]byte("fake-log"))
		w.WriteHeader(http.StatusOK)
	}))
	defer fakeKubeletServer.Close()

	pod := prepareFakeNodeAndPod(tCtx, t, clientSet, fakeKubeletServer)

	insecureResult := clientSet.CoreV1().Pods("ns").GetLogs(pod.Name, &corev1.PodLogOptions{InsecureSkipTLSVerifyBackend: true}).Do(context.TODO())
	if err := insecureResult.Error(); err != nil {
		t.Fatal(err)
	}
	insecureStatusCode := 0
	insecureResult.StatusCode(&insecureStatusCode)
	if insecureStatusCode != http.StatusOK {
		t.Fatal(insecureStatusCode)
	}

	secureResult := clientSet.CoreV1().Pods("ns").GetLogs(pod.Name, &corev1.PodLogOptions{}).Do(tCtx)
	if err := secureResult.Error(); err == nil || !strings.Contains(err.Error(), "x509: certificate signed by unknown authority") {
		t.Fatal(err)
	}
	secureStatusCode := 0
	secureResult.StatusCode(&secureStatusCode)
	if secureStatusCode == http.StatusOK {
		raw, rawErr := secureResult.Raw()
		if rawErr != nil {
			t.Log(rawErr)
		}
		t.Log(string(raw))
		t.Fatal(secureStatusCode)
	}
}

func prepareFakeNodeAndPod(ctx context.Context, t *testing.T, clientSet kubernetes.Interface, fakeKubeletServer *httptest.Server) *corev1.Pod {
	t.Helper()

	fakeKubeletURL, err := url.Parse(fakeKubeletServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	fakeKubeletHost, fakeKubeletPortStr, err := net.SplitHostPort(fakeKubeletURL.Host)
	if err != nil {
		t.Fatal(err)
	}
	fakeKubeletPort, err := strconv.ParseUint(fakeKubeletPortStr, 10, 32)
	if err != nil {
		t.Fatal(err)
	}

	node, err := clientSet.CoreV1().Nodes().Create(ctx, &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "fake"},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	node.Status = corev1.NodeStatus{
		Addresses: []corev1.NodeAddress{
			{
				Type:    corev1.NodeExternalIP,
				Address: fakeKubeletHost,
			},
		},
		DaemonEndpoints: corev1.NodeDaemonEndpoints{
			KubeletEndpoint: corev1.DaemonEndpoint{
				Port: int32(fakeKubeletPort),
			},
		},
	}
	node, err = clientSet.CoreV1().Nodes().UpdateStatus(ctx, node, metav1.UpdateOptions{})
	if err != nil {
		t.Fatal(err)
	}

	_, err = clientSet.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "ns"},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}

	_, err = clientSet.CoreV1().ServiceAccounts("ns").Create(ctx, &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "ns"},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}

	falseRef := false
	pod, err := clientSet.CoreV1().Pods("ns").Create(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "ns"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "foo",
					Image: "some/image:latest",
				},
			},
			NodeName:                     node.Name,
			AutomountServiceAccountToken: &falseRef,
		},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}

	return pod
}

func TestPodLogsKubeletClientCertReload(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)

	origCertCallbackRefreshDuration := transport.CertCallbackRefreshDuration
	origDialerStopCh := transport.DialerStopCh
	transport.CertCallbackRefreshDuration = time.Second // make client cert reloading fast
	transport.DialerStopCh = ctx.Done()
	t.Cleanup(func() {
		transport.CertCallbackRefreshDuration = origCertCallbackRefreshDuration
		transport.DialerStopCh = origDialerStopCh
	})

	// create a CA to sign the API server's kubelet client cert
	startingCerts := generateClientCert(t)

	dynamicCAContentFromFile, err := dynamiccertificates.NewDynamicCAContentFromFile(ctx, "client-ca-bundle", startingCerts.caFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := dynamicCAContentFromFile.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	go dynamicCAContentFromFile.Run(ctx, 1)
	authenticatorConfig := authenticatorfactory.DelegatingAuthenticatorConfig{
		ClientCertificateCAContentProvider: dynamicCAContentFromFile,
	}
	authenticator, _, err := authenticatorConfig.New()
	if err != nil {
		t.Fatal(err)
	}

	// this fake kubelet will perform per request authentication using the configured CA (which is dynamically reloaded)
	fakeKubeletServer := httptest.NewUnstartedServer(
		filters.WithAuthentication(
			http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte("pod-logs-here"))
			}),
			authenticator,
			http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusUnauthorized)
			}),
			nil,
			nil,
		),
	)
	fakeKubeletServer.TLS = &tls.Config{ClientAuth: tls.RequestClientCert}
	fakeKubeletServer.StartTLS()
	t.Cleanup(fakeKubeletServer.Close)

	kubeletCA := writeDataToTempFile(t, pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: fakeKubeletServer.Certificate().Raw,
	}))

	clientSet, _, tearDownFn := framework.StartTestServer(ctx, t, framework.TestServerSetup{
		ModifyServerRunOptions: func(opts *options.ServerRunOptions) {
			opts.GenericServerRunOptions.MaxRequestBodyBytes = 1024 * 1024
			opts.KubeletConfig.TLSClientConfig.CAFile = kubeletCA
			opts.KubeletConfig.TLSClientConfig.CertFile = startingCerts.clientCertFile
			opts.KubeletConfig.TLSClientConfig.KeyFile = startingCerts.clientCertKeyFile
		},
	})
	t.Cleanup(tearDownFn)

	pod := prepareFakeNodeAndPod(ctx, t, clientSet, fakeKubeletServer)

	// verify that the starting state works as expected
	podLogs, err := clientSet.CoreV1().Pods("ns").GetLogs(pod.Name, &corev1.PodLogOptions{}).DoRaw(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if l := string(podLogs); l != "pod-logs-here" {
		t.Fatalf("unexpected pod logs: %s", l)
	}

	// generate a new CA and overwrite the existing CA that the kubelet is using for request authentication
	newCerts := generateClientCert(t)
	if err := os.Rename(newCerts.caFile, startingCerts.caFile); err != nil {
		t.Fatal(err)
	}

	// wait until the kubelet observes the new CA
	if err := wait.PollUntilContextCancel(ctx, time.Second, true, func(ctx context.Context) (bool, error) {
		_, errLog := clientSet.CoreV1().Pods("ns").GetLogs(pod.Name, &corev1.PodLogOptions{}).DoRaw(ctx)
		if errors.IsUnauthorized(errLog) {
			return true, nil
		}
		return false, errLog
	}); err != nil {
		t.Fatal(err)
	}

	// now update the API server's kubelet client cert to use the new cert
	if err := os.Rename(newCerts.clientCertFile, startingCerts.clientCertFile); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(newCerts.clientCertKeyFile, startingCerts.clientCertKeyFile); err != nil {
		t.Fatal(err)
	}

	// confirm that the API server observes the new client cert and closes existing connections to use it
	if err := wait.PollUntilContextCancel(ctx, time.Second, true, func(ctx context.Context) (bool, error) {
		fixedPodLogs, errLog := clientSet.CoreV1().Pods("ns").GetLogs(pod.Name, &corev1.PodLogOptions{}).DoRaw(ctx)
		if errors.IsUnauthorized(errLog) {
			t.Log("api server has not observed new client cert")
			return false, nil
		}
		if errLog != nil {
			return false, errLog
		}
		if l := string(fixedPodLogs); l != "pod-logs-here" {
			return false, fmt.Errorf("unexpected pod logs: %s", l)
		}
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
}

type testCerts struct {
	caFile, clientCertFile, clientCertKeyFile string
}

func generateClientCert(t *testing.T) testCerts {
	t.Helper()

	caPrivateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caCert, err := certutil.NewSelfSignedCACert(certutil.Config{CommonName: "test-ca"}, caPrivateKey)
	if err != nil {
		t.Fatal(err)
	}

	clientCertKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	clientCertKeyBytes, err := keyutil.MarshalPrivateKeyToPEM(clientCertKey)
	if err != nil {
		t.Fatal(err)
	}

	// returns a uniform random value in [0, max-1), then add 1 to serial to make it a uniform random value in [1, max).
	serial, err := rand.Int(rand.Reader, new(big.Int).SetInt64(math.MaxInt64-1))
	if err != nil {
		t.Fatal(err)
	}
	serial = new(big.Int).Add(serial, big.NewInt(1))
	certTmpl := x509.Certificate{
		Subject: pkix.Name{
			CommonName: "the-api-server-user",
		},
		NotBefore:    caCert.NotBefore,
		SerialNumber: serial,
		NotAfter:     time.Now().Add(time.Hour).UTC(),
		KeyUsage:     x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	clientCertDERBytes, err := x509.CreateCertificate(rand.Reader, &certTmpl, caCert, clientCertKey.Public(), caPrivateKey)
	if err != nil {
		t.Fatal(err)
	}

	clientCert, err := x509.ParseCertificate(clientCertDERBytes)
	if err != nil {
		t.Fatal(err)
	}

	return testCerts{
		caFile: writeDataToTempFile(t, pem.EncodeToMemory(&pem.Block{
			Type:  "CERTIFICATE",
			Bytes: caCert.Raw,
		})),
		clientCertFile: writeDataToTempFile(t, pem.EncodeToMemory(&pem.Block{
			Type:  "CERTIFICATE",
			Bytes: clientCert.Raw,
		})),
		clientCertKeyFile: writeDataToTempFile(t, clientCertKeyBytes),
	}
}

func writeDataToTempFile(t *testing.T, data []byte) string {
	t.Helper()

	file, err := os.CreateTemp("", "pod-logs-test-")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(data); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Remove(file.Name())
	})
	return file.Name()
}
