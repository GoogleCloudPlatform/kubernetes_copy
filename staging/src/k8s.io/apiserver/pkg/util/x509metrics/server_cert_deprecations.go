/*
Copyright 2020 The Kubernetes Authors.

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

package x509metrics

import (
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"

	utilnet "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/apiserver/pkg/audit"
	"k8s.io/component-base/metrics"
	"k8s.io/klog/v2"
)

var _ utilnet.RoundTripperWrapper = &x509DeprecatedCertificateMetricsRTWrapper{}

type x509DeprecatedCertificateMetricsRTWrapper struct {
	rt http.RoundTripper

	checkers []deprecatedCertificateAttributeChecker
}

type deprecatedCertificateAttributeChecker interface {
	// CheckRoundTripError returns true if the err is an error specific
	// to this deprecated certificate attribute
	CheckRoundTripError(err error) bool
	// CheckPeerCertificates returns true if the deprecated attribute/value pair
	// was found in a given certificate in the http.Response.TLS.PeerCertificates bundle
	CheckPeerCertificates(certs []*x509.Certificate) bool
	// IncreaseCounter increases the counter internal to this interface
	// Use the req to derive and log information useful for troubleshooting the certificate issue
	IncreaseMetricsCounter(req *http.Request)
}

// counterRaiser is a helper structure to include in certificate deprecation checkers.
// It implements the IncreaseMetricsCounter() method so that, when included in the checker,
// it does not have to be reimplemented.
type counterRaiser struct {
	counter *metrics.Counter
	// programmatic id used in log and audit annotations prefixes
	id string
	// human readable explanation
	reason string
}

func (c *counterRaiser) IncreaseMetricsCounter(req *http.Request) {
	if req != nil && req.URL != nil {
		if hostname := req.URL.Hostname(); len(hostname) > 0 {
			prefix := fmt.Sprintf("%s.invalid-cert.kubernetes.io", c.id)
			klog.Infof("%s: invalid certificate detected connecting to %q: %s", prefix, hostname, c.reason)
			audit.AddAuditAnnotation(req.Context(), prefix+"/"+hostname, c.reason)
		}
	}
	c.counter.Inc()
}

// NewDeprecatedCertificateRoundTripperWrapperConstructor returns a RoundTripper wrapper that's usable within ClientConfig.Wrap.
//
// It increases the `missingSAN` counter whenever:
//  1. we get a x509.HostnameError with string `x509: certificate relies on legacy Common Name field`
//     which indicates an error caused by the deprecation of Common Name field when verifying remote
//     hostname
//  2. the server certificate in response contains no SAN. This indicates that this binary run
//     with the GODEBUG=x509ignoreCN=0 in env
//
// It increases the `sha1` counter whenever:
//  1. we get a x509.InsecureAlgorithmError with string `SHA1`
//     which indicates an error caused by an insecure SHA1 signature
//  2. the server certificate in response contains a SHA1WithRSA or ECDSAWithSHA1 signature.
//     This indicates that this binary run with the GODEBUG=x509sha1=1 in env
func NewDeprecatedCertificateRoundTripperWrapperConstructor(missingSAN, sha1 *metrics.Counter) func(rt http.RoundTripper) http.RoundTripper {
	return func(rt http.RoundTripper) http.RoundTripper {
		return &x509DeprecatedCertificateMetricsRTWrapper{
			rt: rt,
			checkers: []deprecatedCertificateAttributeChecker{
				NewSANDeprecatedChecker(missingSAN),
				NewSHA1SignatureDeprecatedChecker(sha1),
			},
		}
	}
}

func (w *x509DeprecatedCertificateMetricsRTWrapper) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := w.rt.RoundTrip(req)

	if err != nil {
		for _, checker := range w.checkers {
			if checker.CheckRoundTripError(err) {
				checker.IncreaseMetricsCounter(req)
			}
		}
	} else if resp != nil {
		if resp.TLS != nil && len(resp.TLS.PeerCertificates) > 0 {
			for _, checker := range w.checkers {
				if checker.CheckPeerCertificates(resp.TLS.PeerCertificates) {
					checker.IncreaseMetricsCounter(req)
				}
			}
		}
	}

	return resp, err
}

func (w *x509DeprecatedCertificateMetricsRTWrapper) WrappedRoundTripper() http.RoundTripper {
	return w.rt
}

var _ deprecatedCertificateAttributeChecker = &missingSANChecker{}

type missingSANChecker struct {
	counterRaiser
}

func NewSANDeprecatedChecker(counter *metrics.Counter) *missingSANChecker {
	return &missingSANChecker{
		counterRaiser: counterRaiser{
			counter: counter,
			id:      "missing-san",
			reason:  "relies on a legacy Common Name field instead of the SAN extension for subject validation",
		},
	}
}

// CheckRoundTripError returns true when we're running w/o GODEBUG=x509ignoreCN=0
// and the client reports a HostnameError about the legacy CN fields
func (c *missingSANChecker) CheckRoundTripError(err error) bool {
	if err != nil && errors.As(err, &x509.HostnameError{}) && strings.Contains(err.Error(), "x509: certificate relies on legacy Common Name field") {
		// increase the count of registered failures due to Go 1.15 x509 cert Common Name deprecation
		return true
	}

	return false
}

// CheckPeerCertificates returns true when the server response contains
// a leaf certificate w/o the SAN extension
func (c *missingSANChecker) CheckPeerCertificates(peerCertificates []*x509.Certificate) bool {
	if len(peerCertificates) > 0 {
		if serverCert := peerCertificates[0]; !hasSAN(serverCert) {
			return true
		}
	}

	return false
}

func hasSAN(c *x509.Certificate) bool {
	sanOID := []int{2, 5, 29, 17}

	for _, e := range c.Extensions {
		if e.Id.Equal(sanOID) {
			return true
		}
	}
	return false
}

type sha1SignatureChecker struct {
	*counterRaiser
}

func NewSHA1SignatureDeprecatedChecker(counter *metrics.Counter) *sha1SignatureChecker {
	return &sha1SignatureChecker{
		counterRaiser: &counterRaiser{
			counter: counter,
			id:      "insecure-sha1",
			reason:  "uses an insecure SHA-1 signature",
		},
	}
}

// CheckRoundTripError returns true when we're running w/o GODEBUG=x509sha1=1
// and the client reports an InsecureAlgorithmError about a SHA1 signature
func (c *sha1SignatureChecker) CheckRoundTripError(err error) bool {
	var unknownAuthorityError x509.UnknownAuthorityError
	if err == nil {
		return false
	}
	if !errors.As(err, &unknownAuthorityError) {
		return false
	}

	errMsg := err.Error()
	if strIdx := strings.Index(errMsg, "x509: cannot verify signature: insecure algorithm"); strIdx != -1 && strings.Contains(errMsg[strIdx:], "SHA1") {
		// increase the count of registered failures due to Go 1.18 x509 sha1 signature deprecation
		return true
	}

	return false
}

// CheckPeerCertificates returns true when the server response contains
// a non-root non-self-signed  certificate with a deprecated SHA1 signature
func (c *sha1SignatureChecker) CheckPeerCertificates(peerCertificates []*x509.Certificate) bool {
	// check all received non-self-signed certificates for deprecated signing algorithms
	for _, cert := range peerCertificates {
		if cert.SignatureAlgorithm == x509.SHA1WithRSA || cert.SignatureAlgorithm == x509.ECDSAWithSHA1 {
			// the SHA-1 deprecation does not involve self-signed root certificates
			if !reflect.DeepEqual(cert.Issuer, cert.Subject) {
				return true
			}
		}
	}

	return false
}
