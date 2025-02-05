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
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"testing"
	"time"

	"k8s.io/klog/v2/ktesting"
	netutils "k8s.io/utils/net"

	"github.com/stretchr/testify/assert"
)

type testCertSpec struct {
	host       string
	names, ips []string // in certificate
}

type namedtestCertSpec struct {
	testCertSpec
	explicitNames []string // as --tls-sni-cert-key explicit names
}

func TestBuiltNamedCertificates(t *testing.T) {
	tests := []struct {
		certs         []namedtestCertSpec
		explicitNames []string
		expected      map[string]int // name to certs[*] index
		errorString   string
	}{
		{
			// empty certs
			expected: map[string]int{},
		},
		{
			// only one cert
			certs: []namedtestCertSpec{
				{
					testCertSpec: testCertSpec{
						host: "test.com",
					},
				},
			},
			expected: map[string]int{
				"test.com": 0,
			},
		},
		{
			// ip as cns are ignored
			certs: []namedtestCertSpec{
				{
					testCertSpec: testCertSpec{
						host:  "1.2.3.4",
						names: []string{"test.com"},
					},
				},
			},
			expected: map[string]int{
				"test.com": 0,
			},
		},
		{
			// ips are ignored
			certs: []namedtestCertSpec{
				{
					testCertSpec: testCertSpec{
						host: "test.com",
						ips:  []string{"1.2.3.4"},
					},
				},
			},
			expected: map[string]int{
				"test.com": 0,
			},
		},
		{
			// two certs with the same name
			certs: []namedtestCertSpec{
				{
					testCertSpec: testCertSpec{
						host: "test.com",
					},
				},
				{
					testCertSpec: testCertSpec{
						host: "test.com",
					},
				},
			},
			expected: map[string]int{
				"test.com": 0,
			},
		},
		{
			// two certs with different names
			certs: []namedtestCertSpec{
				{
					testCertSpec: testCertSpec{
						host: "test2.com",
					},
				},
				{
					testCertSpec: testCertSpec{
						host: "test1.com",
					},
				},
			},
			expected: map[string]int{
				"test1.com": 1,
				"test2.com": 0,
			},
		},
		{
			// two certs with the same name, explicit trumps
			certs: []namedtestCertSpec{
				{
					testCertSpec: testCertSpec{
						host: "test.com",
					},
				},
				{
					testCertSpec: testCertSpec{
						host: "test.com",
					},
					explicitNames: []string{"test.com"},
				},
			},
			expected: map[string]int{
				"test.com": 1,
			},
		},
		{
			// certs with partial overlap; ips are ignored
			certs: []namedtestCertSpec{
				{
					testCertSpec: testCertSpec{
						host:  "a",
						names: []string{"a.test.com", "test.com"},
					},
				},
				{
					testCertSpec: testCertSpec{
						host:  "b",
						names: []string{"b.test.com", "test.com"},
					},
				},
			},
			expected: map[string]int{
				"a": 0, "b": 1,
				"a.test.com": 0, "b.test.com": 1,
				"test.com": 0,
			},
		},
		{
			// wildcards
			certs: []namedtestCertSpec{
				{
					testCertSpec: testCertSpec{
						host:  "a",
						names: []string{"a.test.com", "test.com"},
					},
					explicitNames: []string{"*.test.com", "test.com"},
				},
				{
					testCertSpec: testCertSpec{
						host:  "b",
						names: []string{"b.test.com", "test.com"},
					},
					explicitNames: []string{"dev.test.com", "test.com"},
				}},
			expected: map[string]int{
				"test.com":     0,
				"*.test.com":   0,
				"dev.test.com": 1,
			},
		},
	}

NextTest:
	for i, test := range tests {
		var sniCerts []SNICertKeyContentProvider
		bySignature := map[string]int{} // index in test.certs by cert signature
		for j, c := range test.certs {
			certProvider, err := createTestTLSCerts(c.testCertSpec, c.explicitNames)
			if err != nil {
				t.Errorf("%d - failed to create cert %d: %v", i, j, err)
				continue NextTest
			}

			sniCerts = append(sniCerts, certProvider)

			sig, err := certSignature(certProvider)
			if err != nil {
				t.Errorf("%d - failed to get signature for %d: %v", i, j, err)
				continue NextTest
			}
			bySignature[sig] = j
		}

		c := DynamicServingCertificateController{sniCerts: sniCerts}
		content, err := c.newTLSContent()
		assert.NoError(t, err)
		_, ctx := ktesting.NewTestContext(t)
		certMap, err := c.BuildNamedCertificates(ctx, content.sniCerts)
		if err == nil && len(test.errorString) != 0 {
			t.Errorf("%d - expected no error, got: %v", i, err)
		} else if err != nil && err.Error() != test.errorString {
			t.Errorf("%d - expected error %q, got: %v", i, test.errorString, err)
		} else {
			got := map[string]int{}
			for name, cert := range certMap {
				x509Certs, err := x509.ParseCertificates(cert.Certificate[0])
				assert.NoError(t, err, "%d - invalid certificate for %q", i, name)
				assert.NotEmpty(t, x509Certs, "%d - expected at least one x509 cert in tls cert for %q", i, name)
				got[name] = bySignature[x509CertSignature(x509Certs[0])]
			}

			assert.EqualValues(t, test.expected, got, "%d - wrong certificate map", i)
		}
	}
}

func parseIPList(ips []string) []net.IP {
	var netIPs []net.IP
	for _, ip := range ips {
		netIPs = append(netIPs, netutils.ParseIPSloppy(ip))
	}
	return netIPs
}

func createTestTLSCerts(spec testCertSpec, names []string) (certProvider SNICertKeyContentProvider, err error) {
	certPem, keyPem, err := generateSelfSignedCertKey(spec.host, parseIPList(spec.ips), spec.names)
	if err != nil {
		return nil, err
	}

	return NewStaticSNICertKeyContent("test-cert", certPem, keyPem, names...)
}

func x509CertSignature(cert *x509.Certificate) string {
	return base64.StdEncoding.EncodeToString(cert.Signature)
}

func certSignature(certProvider CertKeyContentProvider) (string, error) {
	currentCert, currentKey := certProvider.CurrentCertKeyContent()

	tlsCert, err := tls.X509KeyPair(currentCert, currentKey)
	if err != nil {
		return "", err
	}

	x509Certs, err := x509.ParseCertificates(tlsCert.Certificate[0])
	if err != nil {
		return "", err
	}
	return x509CertSignature(x509Certs[0]), nil
}

// generateSelfSignedCertKey creates a self-signed certificate and key for the given host.
// Host may be an IP or a DNS name
// You may also specify additional subject alt names (either ip or dns names) for the certificate
func generateSelfSignedCertKey(host string, alternateIPs []net.IP, alternateDNS []string) ([]byte, []byte, error) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
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

	derBytes, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
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
