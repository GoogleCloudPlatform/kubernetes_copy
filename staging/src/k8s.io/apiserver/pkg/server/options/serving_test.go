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

package options

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"io/ioutil"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/version"
	"k8s.io/apiserver/pkg/server"
	"k8s.io/client-go/discovery"
	restclient "k8s.io/client-go/rest"
	cliflag "k8s.io/component-base/cli/flag"
	basecompatibility "k8s.io/component-base/compatibility"
	"k8s.io/klog/v2/ktesting"
	netutils "k8s.io/utils/net"
)

func setUp(t *testing.T) server.Config {
	scheme := runtime.NewScheme()
	codecs := serializer.NewCodecFactory(scheme)

	config := server.NewConfig(codecs)

	return *config
}

type TestCertSpec struct {
	host       string
	names, ips []string // in certificate
}

type NamedTestCertSpec struct {
	TestCertSpec
	explicitNames []string // as --tls-sni-cert-key explicit names
}

func TestServerRunWithSNI(t *testing.T) {
	tests := map[string]struct {
		Cert              TestCertSpec
		SNICerts          []NamedTestCertSpec
		ExpectedCertIndex int

		// passed in the client hello info, "localhost" if unset
		ServerName string

		// optional ip or hostname to pass to NewLoopbackClientConfig
		LoopbackClientBindAddressOverride string
	}{
		"only one cert": {
			Cert: TestCertSpec{
				host: "localhost",
				ips:  []string{"127.0.0.1"},
			},
			ExpectedCertIndex: -1,
		},
		"cert with multiple alternate names": {
			Cert: TestCertSpec{
				host:  "localhost",
				names: []string{"test.com"},
				ips:   []string{"127.0.0.1"},
			},
			ExpectedCertIndex: -1,
			ServerName:        "test.com",
		},
		"one SNI and the default cert with the same name": {
			Cert: TestCertSpec{
				host: "localhost",
				ips:  []string{"127.0.0.1"},
			},
			SNICerts: []NamedTestCertSpec{
				{
					TestCertSpec: TestCertSpec{
						host: "localhost",
					},
				},
			},
			ExpectedCertIndex: 0,
		},
		"matching SNI cert": {
			Cert: TestCertSpec{
				host: "localhost",
				ips:  []string{"127.0.0.1"},
			},
			SNICerts: []NamedTestCertSpec{
				{
					TestCertSpec: TestCertSpec{
						host: "test.com",
					},
				},
			},
			ExpectedCertIndex: 0,
			ServerName:        "test.com",
		},
		"matching IP in SNI cert and the server cert": {
			// IPs must not be passed via SNI. Hence, the ServerName in the
			// HELLO packet is empty and the server should select the non-SNI cert.
			Cert: TestCertSpec{
				host: "localhost",
				ips:  []string{"10.0.0.1", "127.0.0.1"},
			},
			SNICerts: []NamedTestCertSpec{
				{
					TestCertSpec: TestCertSpec{
						host: "test.com",
						ips:  []string{"10.0.0.1"},
					},
				},
			},
			ExpectedCertIndex: -1,
			ServerName:        "10.0.0.1",
		},
		"wildcards": {
			Cert: TestCertSpec{
				host: "localhost",
				ips:  []string{"127.0.0.1"},
			},
			SNICerts: []NamedTestCertSpec{
				{
					TestCertSpec: TestCertSpec{
						host:  "test.com",
						names: []string{"*.test.com"},
					},
				},
			},
			ExpectedCertIndex: 0,
			ServerName:        "www.test.com",
		},

		"loopback: LoopbackClientServerNameOverride not on any cert": {
			Cert: TestCertSpec{
				host: "test.com",
			},
			SNICerts: []NamedTestCertSpec{
				{
					TestCertSpec: TestCertSpec{
						host: "localhost",
					},
				},
			},
			ExpectedCertIndex: 0,
		},
		"loopback: LoopbackClientServerNameOverride on server cert": {
			Cert: TestCertSpec{
				host: server.LoopbackClientServerNameOverride,
			},
			SNICerts: []NamedTestCertSpec{
				{
					TestCertSpec: TestCertSpec{
						host: "localhost",
					},
				},
			},
			ExpectedCertIndex: 0,
		},
		"loopback: LoopbackClientServerNameOverride on SNI cert": {
			Cert: TestCertSpec{
				host: "localhost",
			},
			SNICerts: []NamedTestCertSpec{
				{
					TestCertSpec: TestCertSpec{
						host: server.LoopbackClientServerNameOverride,
					},
				},
			},
			ExpectedCertIndex: -1,
		},
		"loopback: bind to 0.0.0.0 => loopback uses localhost": {
			Cert: TestCertSpec{
				host: "localhost",
			},
			ExpectedCertIndex:                 -1,
			LoopbackClientBindAddressOverride: "0.0.0.0",
		},
	}

	specToName := func(spec TestCertSpec) string {
		name := spec.host + "_" + strings.Join(spec.names, ",") + "_" + strings.Join(spec.ips, ",")
		return strings.Replace(name, "*", "star", -1)
	}

	for title := range tests {
		test := tests[title]
		t.Run(title, func(t *testing.T) {
			t.Parallel()
			_, ctx := ktesting.NewTestContext(t)
			ctx, cancel := context.WithCancelCause(ctx)
			defer cancel(errors.New("test has completed"))

			// create server cert
			certDir := "testdata/" + specToName(test.Cert)
			serverCertBundleFile := filepath.Join(certDir, "cert")
			serverKeyFile := filepath.Join(certDir, "key")
			err := getOrCreateTestCertFiles(serverCertBundleFile, serverKeyFile, test.Cert)
			if err != nil {
				t.Fatalf("failed to create server cert: %v", err)
			}
			ca, err := caCertFromBundle(serverCertBundleFile)
			if err != nil {
				t.Fatalf("failed to extract ca cert from server cert bundle: %v", err)
			}
			caCerts := []*x509.Certificate{ca}

			// create SNI certs
			var namedCertKeys []cliflag.NamedCertKey
			serverSig, err := certFileSignature(serverCertBundleFile, serverKeyFile)
			if err != nil {
				t.Fatalf("failed to get server cert signature: %v", err)
			}
			signatures := map[string]int{
				serverSig: -1,
			}
			for j, c := range test.SNICerts {
				sniDir := filepath.Join(certDir, specToName(c.TestCertSpec))
				certBundleFile := filepath.Join(sniDir, "cert")
				keyFile := filepath.Join(sniDir, "key")
				err := getOrCreateTestCertFiles(certBundleFile, keyFile, c.TestCertSpec)
				if err != nil {
					t.Fatalf("failed to create SNI cert %d: %v", j, err)
				}

				namedCertKeys = append(namedCertKeys, cliflag.NamedCertKey{
					KeyFile:  keyFile,
					CertFile: certBundleFile,
					Names:    c.explicitNames,
				})

				ca, err := caCertFromBundle(certBundleFile)
				if err != nil {
					t.Fatalf("failed to extract ca cert from SNI cert %d: %v", j, err)
				}
				caCerts = append(caCerts, ca)

				// store index in namedCertKeys with the signature as the key
				sig, err := certFileSignature(certBundleFile, keyFile)
				if err != nil {
					t.Fatalf("failed get SNI cert %d signature: %v", j, err)
				}
				signatures[sig] = j
			}

			// launch server
			config := setUp(t)
			v := fakeVersion()
			config.EffectiveVersion = basecompatibility.NewEffectiveVersionFromString(v.String(), "", "")

			config.EnableIndex = true
			secureOptions := (&SecureServingOptions{
				BindAddress: netutils.ParseIPSloppy("127.0.0.1"),
				BindPort:    6443,
				ServerCert: GeneratableKeyCert{
					CertKey: CertKey{
						CertFile: serverCertBundleFile,
						KeyFile:  serverKeyFile,
					},
				},
				DisableHTTP2Serving: true,
				SNICertKeys:         namedCertKeys,
			}).WithLoopback()
			// use a random free port
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatalf("failed to listen on 127.0.0.1:0")
			}

			secureOptions.Listener = ln
			// get port
			secureOptions.BindPort = ln.Addr().(*net.TCPAddr).Port
			config.LoopbackClientConfig = &restclient.Config{}
			if err := secureOptions.ApplyTo(&config.SecureServing, &config.LoopbackClientConfig); err != nil {
				t.Fatalf("failed applying the SecureServingOptions: %v", err)
			}

			s, err := config.Complete(nil).New("test", server.NewEmptyDelegate())
			if err != nil {
				t.Fatalf("failed creating the server: %v", err)
			}

			// add poststart hook to know when the server is up.
			startedCh := make(chan struct{})
			s.AddPostStartHookOrDie("test-notifier", func(context server.PostStartHookContext) error {
				close(startedCh)
				return nil
			})
			preparedServer := s.PrepareRun()
			preparedServerErrors := make(chan error)
			go func() {
				if err := preparedServer.RunWithContext(ctx); err != nil {
					preparedServerErrors <- err
				}
			}()

			// load ca certificates into a pool
			roots := x509.NewCertPool()
			for _, caCert := range caCerts {
				roots.AddCert(caCert)
			}

			<-startedCh

			// try to dial
			addr := fmt.Sprintf("localhost:%d", secureOptions.BindPort)
			t.Logf("Dialing %s as %q", addr, test.ServerName)
			conn, err := tls.Dial("tcp", addr, &tls.Config{
				RootCAs:    roots,
				ServerName: test.ServerName, // used for SNI in the client HELLO packet
			})
			if err != nil {
				t.Fatalf("failed to connect: %v", err)
			}
			defer conn.Close()

			// check returned server certificate
			sig := x509CertSignature(conn.ConnectionState().PeerCertificates[0])
			gotCertIndex, found := signatures[sig]
			if !found {
				t.Errorf("unknown signature returned from server: %s", sig)
			}
			if gotCertIndex != test.ExpectedCertIndex {
				t.Errorf("expected cert index %d, got cert index %d", test.ExpectedCertIndex, gotCertIndex)
			}

			// check that the loopback client can connect
			host := "127.0.0.1"
			if len(test.LoopbackClientBindAddressOverride) != 0 {
				host = test.LoopbackClientBindAddressOverride
			}
			s.LoopbackClientConfig.Host = net.JoinHostPort(host, strconv.Itoa(secureOptions.BindPort))

			client, err := discovery.NewDiscoveryClientForConfig(s.LoopbackClientConfig)
			if err != nil {
				t.Fatalf("failed to create loopback client: %v", err)
			}
			got, err := client.ServerVersion()
			if err != nil {
				t.Fatalf("failed to connect with loopback client: %v", err)
			}
			if expected := &v; !reflect.DeepEqual(got, expected) {
				t.Errorf("loopback client didn't get correct version info: expected=%v got=%v", expected, got)
			}

			select {
			case err := <-preparedServerErrors:
				t.Fatalf("preparedServer failed with error: %v", err)
			default:
			}
		})
	}
}

func parseIPList(ips []string) []net.IP {
	var netIPs []net.IP
	for _, ip := range ips {
		netIPs = append(netIPs, netutils.ParseIPSloppy(ip))
	}
	return netIPs
}

func getOrCreateTestCertFiles(certFileName, keyFileName string, spec TestCertSpec) (err error) {
	if _, err := os.Stat(certFileName); err == nil {
		if _, err := os.Stat(keyFileName); err == nil {
			return nil
		}
	}

	certPem, keyPem, err := generateSelfSignedCertKey(spec.host, parseIPList(spec.ips), spec.names)
	if err != nil {
		return err
	}

	os.MkdirAll(filepath.Dir(certFileName), os.FileMode(0755))
	err = ioutil.WriteFile(certFileName, certPem, os.FileMode(0755))
	if err != nil {
		return err
	}

	os.MkdirAll(filepath.Dir(keyFileName), os.FileMode(0755))
	err = ioutil.WriteFile(keyFileName, keyPem, os.FileMode(0755))
	if err != nil {
		return err
	}

	return nil
}

func caCertFromBundle(bundlePath string) (*x509.Certificate, error) {
	pemData, err := ioutil.ReadFile(bundlePath)
	if err != nil {
		return nil, err
	}

	// fetch last block
	var block *pem.Block
	for {
		var nextBlock *pem.Block
		nextBlock, pemData = pem.Decode(pemData)
		if nextBlock == nil {
			if block == nil {
				return nil, fmt.Errorf("no certificate found in %q", bundlePath)

			}
			return x509.ParseCertificate(block.Bytes)
		}
		block = nextBlock
	}
}

func x509CertSignature(cert *x509.Certificate) string {
	return base64.StdEncoding.EncodeToString(cert.Signature)
}

func certFileSignature(certFile, keyFile string) (string, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return "", err
	}
	return certSignature(cert)
}

func certSignature(cert tls.Certificate) (string, error) {
	x509Certs, err := x509.ParseCertificates(cert.Certificate[0])
	if err != nil {
		return "", err
	}
	return x509CertSignature(x509Certs[0]), nil
}

func fakeVersion() version.Info {
	return version.Info{
		Major:      "42",
		Minor:      "42",
		GitVersion: "42.42",
	}
}

// generateSelfSignedCertKey creates a self-signed certificate and key for the given host.
// Host may be an IP or a DNS name
// You may also specify additional subject alt names (either ip or dns names) for the certificate
func generateSelfSignedCertKey(host string, alternateIPs []net.IP, alternateDNS []string) ([]byte, []byte, error) {
	priv, err := rsa.GenerateKey(cryptorand.Reader, 2048)
	if err != nil {
		return nil, nil, err
	}

	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			CommonName: fmt.Sprintf("%s@%d", host, time.Now().Unix()),
		},
		NotBefore: time.Unix(0, 0),
		NotAfter:  time.Now().Add(time.Hour * 24 * 365 * 100),

		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	if ip := netutils.ParseIPSloppy(host); ip != nil {
		template.IPAddresses = append(template.IPAddresses, ip)
	} else {
		template.DNSNames = append(template.DNSNames, host)
	}

	template.IPAddresses = append(template.IPAddresses, alternateIPs...)
	template.DNSNames = append(template.DNSNames, alternateDNS...)

	derBytes, err := x509.CreateCertificate(cryptorand.Reader, &template, &template, &priv.PublicKey, priv)
	if err != nil {
		return nil, nil, err
	}

	// Generate cert
	certBuffer := bytes.Buffer{}
	if err := pem.Encode(&certBuffer, &pem.Block{Type: "CERTIFICATE", Bytes: derBytes}); err != nil {
		return nil, nil, err
	}

	// Generate key
	keyBuffer := bytes.Buffer{}
	if err := pem.Encode(&keyBuffer, &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(priv)}); err != nil {
		return nil, nil, err
	}

	return certBuffer.Bytes(), keyBuffer.Bytes(), nil
}
