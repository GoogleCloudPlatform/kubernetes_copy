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
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/klog/v2"
	netutils "k8s.io/utils/net"
)

// BuildNamedCertificates returns a map of *tls.Certificate by name. It's
// suitable for use in tls.Config#NamedCertificates. Returns an error if any of the certs
// is invalid. Returns nil if len(certs) == 0
func (c *DynamicServingCertificateController) BuildNamedCertificates(ctx context.Context, sniCerts []sniCertKeyContent) (map[string]*tls.Certificate, error) {
	nameToCertificate := map[string]*tls.Certificate{}
	byNameExplicit := map[string]*tls.Certificate{}
	logger := klog.FromContext(ctx)

	// Iterate backwards so that earlier certs take precedence in the names map
	for i := len(sniCerts) - 1; i >= 0; i-- {
		cert, err := tls.X509KeyPair(sniCerts[i].cert, sniCerts[i].key)
		if err != nil {
			return nil, fmt.Errorf("invalid SNI cert keypair [%d/%q]: %v", i, c.sniCerts[i].Name(), err)
		}

		// error is not possible given above call to X509KeyPair
		x509Cert, _ := x509.ParseCertificate(cert.Certificate[0])

		names := sniCerts[i].sniNames
		for _, name := range names {
			byNameExplicit[name] = &cert
		}

		logger.V(2).Info("Loaded SNI cert", "index", i, "certName", c.sniCerts[i].Name(), "certDetail", GetHumanCertDetail(x509Cert))
		if c.eventRecorder != nil {
			c.eventRecorder.Eventf(&corev1.ObjectReference{Name: c.sniCerts[i].Name()}, nil, corev1.EventTypeWarning, "TLSConfigChanged", "SNICertificateReload", "loaded SNI cert [%d/%q]: %s with explicit names %v", i, c.sniCerts[i].Name(), GetHumanCertDetail(x509Cert), names)
		}

		if len(names) == 0 {
			names = getCertificateNames(x509Cert)
			for _, name := range names {
				nameToCertificate[name] = &cert
			}
		}
	}

	// Explicitly set names must override
	for k, v := range byNameExplicit {
		nameToCertificate[k] = v
	}

	return nameToCertificate, nil
}

// getCertificateNames returns names for an x509.Certificate. The names are
// suitable for use in tls.Config#NamedCertificates.
func getCertificateNames(cert *x509.Certificate) []string {
	var names []string

	cn := cert.Subject.CommonName
	cnIsIP := netutils.ParseIPSloppy(cn) != nil
	cnIsValidDomain := cn == "*" || len(validation.IsDNS1123Subdomain(strings.TrimPrefix(cn, "*."))) == 0
	// don't use the CN if it is a valid IP because our IP serving detection may unexpectedly use it to terminate the connection.
	if !cnIsIP && cnIsValidDomain {
		names = append(names, cn)
	}
	names = append(names, cert.DNSNames...)
	// intentionally all IPs in the cert are ignored as SNI forbids passing IPs
	// to select a cert. Before go 1.6 the tls happily passed IPs as SNI values.

	return names
}
