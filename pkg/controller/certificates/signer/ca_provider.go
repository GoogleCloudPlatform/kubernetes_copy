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

package signer

import (
	"bytes"
	"context"
	"crypto"
	"fmt"
	"sync/atomic"

	"k8s.io/apiserver/pkg/server/dynamiccertificates"
	"k8s.io/client-go/util/cert"
	"k8s.io/client-go/util/keyutil"
	"k8s.io/kubernetes/pkg/controller/certificates/authority"
)

func newCAProvider(ctx context.Context, caFile, caKeyFile string) (*caProvider, error) {
	caLoader, err := dynamiccertificates.NewDynamicServingContentFromFiles(ctx, "csr-controller", caFile, caKeyFile)
	if err != nil {
		return nil, fmt.Errorf("error reading CA cert file %q: %v", caFile, err)
	}

	ret := &caProvider{
		caLoader: caLoader,
	}
	if err := ret.setCA(); err != nil {
		return nil, err
	}

	return ret, nil
}

type caProvider struct {
	caValue  atomic.Value
	caLoader *dynamiccertificates.DynamicCertKeyPairContent
}

// setCA unconditionally stores the current cert/key content
func (p *caProvider) setCA() error {
	certPEM, keyPEM := p.caLoader.CurrentCertKeyContent()

	certs, err := cert.ParseCertsPEM(certPEM)
	if err != nil {
		return fmt.Errorf("error reading CA cert file %q: %v", p.caLoader.Name(), err)
	}
	if len(certs) != 1 {
		return fmt.Errorf("error reading CA cert file %q: expected 1 certificate, found %d", p.caLoader.Name(), len(certs))
	}

	key, err := keyutil.ParsePrivateKeyPEM(keyPEM)
	if err != nil {
		return fmt.Errorf("error reading CA key file %q: %v", p.caLoader.Name(), err)
	}
	priv, ok := key.(crypto.Signer)
	if !ok {
		return fmt.Errorf("error reading CA key file %q: key did not implement crypto.Signer", p.caLoader.Name())
	}

	ca := &authority.CertificateAuthority{
		RawCert: certPEM,
		RawKey:  keyPEM,

		Certificate: certs[0],
		PrivateKey:  priv,
	}
	p.caValue.Store(ca)

	return nil
}

// currentCA provides the current value of the CA.
// It always check for a stale value.  This is cheap because it's all an in memory cache of small slices.
func (p *caProvider) currentCA() (*authority.CertificateAuthority, error) {
	certPEM, keyPEM := p.caLoader.CurrentCertKeyContent()
	currCA := p.caValue.Load().(*authority.CertificateAuthority)
	if bytes.Equal(currCA.RawCert, certPEM) && bytes.Equal(currCA.RawKey, keyPEM) {
		return currCA, nil
	}

	// the bytes weren't equal, so we have to set and then load
	if err := p.setCA(); err != nil {
		return currCA, err
	}
	return p.caValue.Load().(*authority.CertificateAuthority), nil
}
