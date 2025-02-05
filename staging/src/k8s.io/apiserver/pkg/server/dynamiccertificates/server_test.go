/*
Copyright 2019 The Kubernetes Authors.

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

package dynamiccertificates

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/util/cert"
)

func TestServingCert(t *testing.T) {
	tlsConfig := &tls.Config{
		// Can't use SSLv3 because of POODLE and BEAST
		// Can't use TLSv1.0 because of POODLE and BEAST using CBC cipher
		// Can't use TLSv1.1 because of RC4 cipher usage
		MinVersion: tls.VersionTLS12,
		// enable HTTP2 for go's 1.7 HTTP Server
		NextProtos: []string{"h2", "http/1.1"},
	}

	defaultCertProvider, err := createTestTLSCerts(testCertSpec{
		host:  "172.30.0.1",
		ips:   []string{"172.30.0.1"},
		names: []string{"kubernetes", "kubernetes.default.svc.cluster.local"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	var sniCerts []SNICertKeyContentProvider
	certSpecs := []testCertSpec{
		{
			host:  "172.30.0.1",
			ips:   []string{"172.30.0.1"},
			names: []string{"openshift", "openshift.default.svc.cluster.local"},
		},
		{
			host:  "127.0.0.1",
			ips:   []string{"127.0.0.1"},
			names: []string{"localhost"},
		},
		{
			host:  "2001:abcd:bcda::1",
			ips:   []string{"2001:abcd:bcda::1"},
			names: []string{"openshiftv6", "openshiftv6.default.svc.cluster.local"},
		},
	}

	for _, certSpec := range certSpecs {
		names := append([]string{}, certSpec.ips...)
		names = append(names, certSpec.names...)
		certProvider, err := createTestTLSCerts(certSpec, names)
		if err != nil {
			t.Fatal(err)
		}
		sniCerts = append(sniCerts, certProvider)
	}

	dynamicCertificateController := NewDynamicServingCertificateController(
		tlsConfig,
		&nullCAContent{name: "client-ca"},
		defaultCertProvider,
		sniCerts,
		nil, // TODO see how to plumb an event recorder down in here. For now this results in simply klog messages.
	)
	ctx := context.TODO()
	if err := dynamicCertificateController.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	tlsConfig.GetConfigForClient = dynamicCertificateController.GetConfigForClient
	tlsConfig.GetCertificate = func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return nil, fmt.Errorf("positive failure") }

	stopCh := make(chan struct{})
	defer close(stopCh)
	server := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			w.WriteHeader(http.StatusOK)
		}),
		MaxHeaderBytes: 1 << 20,
		TLSConfig:      tlsConfig,
	}
	listener, _, err := createListener("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	apiPort := listener.Addr().String()
	go func() {
		t.Logf("listening on %s", listener.Addr().String())
		listener = tls.NewListener(listener, server.TLSConfig)
		if err := server.ServeTLS(listener, "", ""); err != nil {
			t.Error(err)
			panic(err)
		}

		<-stopCh
		server.Close()
		t.Logf("stopped listening on %s", listener.Addr().String())
	}()

	time.Sleep(1 * time.Second)

	expectedDefaultCertBytes, _ := defaultCertProvider.CurrentCertKeyContent()
	expectedServiceCertBytes, _ := sniCerts[0].CurrentCertKeyContent()
	expectedLocalhostCertBytes, _ := sniCerts[1].CurrentCertKeyContent()
	expectedServiceV6CertBytes, _ := sniCerts[2].CurrentCertKeyContent()

	tests := []struct {
		name       string
		serverName string
		expected   []byte
	}{
		{
			name:       "default", // the no server name hits 127.0.0.1, which we hope matches by IP
			serverName: "",
			expected:   []byte(strings.TrimSpace(string(expectedLocalhostCertBytes))),
		},
		{
			name:       "invalid",
			serverName: "not-marked",
			expected:   []byte(strings.TrimSpace(string(expectedDefaultCertBytes))),
		},
		{
			name:       "service by dns",
			serverName: "openshift",
			expected:   []byte(strings.TrimSpace(string(expectedServiceCertBytes))),
		},
		{
			name:       "service v6 by dns",
			serverName: "openshiftv6",
			expected:   []byte(strings.TrimSpace(string(expectedServiceV6CertBytes))),
		},
		{
			name:       "localhost by dns",
			serverName: "localhost",
			expected:   []byte(strings.TrimSpace(string(expectedLocalhostCertBytes))),
		},
		{
			name:       "localhost by IP",
			serverName: "127.0.0.1", // this can never actually happen, but let's see
			expected:   []byte(strings.TrimSpace(string(expectedLocalhostCertBytes))),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, actualDefaultCertBytes, err := cert.GetServingCertificates(apiPort, test.serverName)
			if err != nil {
				t.Fatal(err)
			}
			if e, a := test.expected, actualDefaultCertBytes[0]; !bytes.Equal(e, a) {
				eCert, _ := cert.ParseCertsPEM(e)
				aCert, _ := cert.ParseCertsPEM(a)
				t.Fatalf("expected %v\n, got %v", GetHumanCertDetail(eCert[0]), GetHumanCertDetail(aCert[0]))
			}
		})
	}

}

func createListener(network, addr string) (net.Listener, int, error) {
	if len(network) == 0 {
		network = "tcp"
	}
	ln, err := net.Listen(network, addr)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to listen on %v: %v", addr, err)
	}

	// get port
	tcpAddr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		ln.Close()
		return nil, 0, fmt.Errorf("invalid listen address: %q", ln.Addr().String())
	}

	return ln, tcpAddr.Port, nil
}

type nullCAContent struct {
	name string
}

var _ CAContentProvider = &nullCAContent{}

// Name is just an identifier
func (c *nullCAContent) Name() string {
	return c.name
}

func (c *nullCAContent) AddListener(Listener) {}

// CurrentCABundleContent provides ca bundle byte content
func (c *nullCAContent) CurrentCABundleContent() (cabundle []byte) {
	return nil
}

func (c *nullCAContent) VerifyOptions() (x509.VerifyOptions, bool) {
	return x509.VerifyOptions{}, false
}
