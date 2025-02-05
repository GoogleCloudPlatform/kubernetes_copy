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

// Package signer implements a CA signer that uses keys stored on local disk.
package signer

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"time"

	capi "k8s.io/api/certificates/v1"
	capiv1beta1 "k8s.io/api/certificates/v1beta1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apiserver/pkg/server/dynamiccertificates"
	certificatesinformers "k8s.io/client-go/informers/certificates/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/certificate/csr"
	capihelper "k8s.io/kubernetes/pkg/apis/certificates"
	"k8s.io/kubernetes/pkg/controller/certificates"
	"k8s.io/kubernetes/pkg/controller/certificates/authority"
)

type CSRSigningController struct {
	certificateController *certificates.CertificateController
	dynamicCertReloader   dynamiccertificates.ControllerRunner
}

func NewKubeletServingCSRSigningController(
	ctx context.Context,
	client clientset.Interface,
	csrInformer certificatesinformers.CertificateSigningRequestInformer,
	caFile, caKeyFile string,
	certTTL time.Duration,
) (*CSRSigningController, error) {
	return NewCSRSigningController(ctx, "csrsigning-kubelet-serving", capi.KubeletServingSignerName, client, csrInformer, caFile, caKeyFile, certTTL)
}

func NewKubeletClientCSRSigningController(
	ctx context.Context,
	client clientset.Interface,
	csrInformer certificatesinformers.CertificateSigningRequestInformer,
	caFile, caKeyFile string,
	certTTL time.Duration,
) (*CSRSigningController, error) {
	return NewCSRSigningController(ctx, "csrsigning-kubelet-client", capi.KubeAPIServerClientKubeletSignerName, client, csrInformer, caFile, caKeyFile, certTTL)
}

func NewKubeAPIServerClientCSRSigningController(
	ctx context.Context,
	client clientset.Interface,
	csrInformer certificatesinformers.CertificateSigningRequestInformer,
	caFile, caKeyFile string,
	certTTL time.Duration,
) (*CSRSigningController, error) {
	return NewCSRSigningController(ctx, "csrsigning-kube-apiserver-client", capi.KubeAPIServerClientSignerName, client, csrInformer, caFile, caKeyFile, certTTL)
}

func NewLegacyUnknownCSRSigningController(
	ctx context.Context,
	client clientset.Interface,
	csrInformer certificatesinformers.CertificateSigningRequestInformer,
	caFile, caKeyFile string,
	certTTL time.Duration,
) (*CSRSigningController, error) {
	return NewCSRSigningController(ctx, "csrsigning-legacy-unknown", capiv1beta1.LegacyUnknownSignerName, client, csrInformer, caFile, caKeyFile, certTTL)
}

func NewCSRSigningController(
	ctx context.Context,
	controllerName string,
	signerName string,
	client clientset.Interface,
	csrInformer certificatesinformers.CertificateSigningRequestInformer,
	caFile, caKeyFile string,
	certTTL time.Duration,
) (*CSRSigningController, error) {
	signer, err := newSigner(ctx, signerName, caFile, caKeyFile, client, certTTL)
	if err != nil {
		return nil, err
	}

	return &CSRSigningController{
		certificateController: certificates.NewCertificateController(
			ctx,
			controllerName,
			client,
			csrInformer,
			signer.handle,
		),
		dynamicCertReloader: signer.caProvider.caLoader,
	}, nil
}

// Run the main goroutine responsible for watching and syncing jobs.
func (c *CSRSigningController) Run(ctx context.Context, workers int) {
	go c.dynamicCertReloader.Run(ctx, workers)

	c.certificateController.Run(ctx, workers)
}

type isRequestForSignerFunc func(req *x509.CertificateRequest, usages []capi.KeyUsage, signerName string) (bool, error)

type signer struct {
	caProvider *caProvider

	client  clientset.Interface
	certTTL time.Duration // max TTL; individual requests may request shorter certs by setting spec.expirationSeconds

	signerName           string
	isRequestForSignerFn isRequestForSignerFunc
}

func newSigner(ctx context.Context, signerName, caFile, caKeyFile string, client clientset.Interface, certificateDuration time.Duration) (*signer, error) {
	isRequestForSignerFn, err := getCSRVerificationFuncForSignerName(signerName)
	if err != nil {
		return nil, err
	}
	caProvider, err := newCAProvider(ctx, caFile, caKeyFile)
	if err != nil {
		return nil, err
	}

	ret := &signer{
		caProvider:           caProvider,
		client:               client,
		certTTL:              certificateDuration,
		signerName:           signerName,
		isRequestForSignerFn: isRequestForSignerFn,
	}
	return ret, nil
}

func (s *signer) handle(ctx context.Context, csr *capi.CertificateSigningRequest) error {
	// Ignore unapproved or failed requests
	if !certificates.IsCertificateRequestApproved(csr) || certificates.HasTrueCondition(csr, capi.CertificateFailed) {
		return nil
	}

	// Fast-path to avoid any additional processing if the CSRs signerName does not match
	if csr.Spec.SignerName != s.signerName {
		return nil
	}

	x509cr, err := capihelper.ParseCSR(csr.Spec.Request)
	if err != nil {
		return fmt.Errorf("unable to parse csr %q: %v", csr.Name, err)
	}
	if recognized, err := s.isRequestForSignerFn(x509cr, csr.Spec.Usages, csr.Spec.SignerName); err != nil {
		csr.Status.Conditions = append(csr.Status.Conditions, capi.CertificateSigningRequestCondition{
			Type:           capi.CertificateFailed,
			Status:         v1.ConditionTrue,
			Reason:         "SignerValidationFailure",
			Message:        err.Error(),
			LastUpdateTime: metav1.Now(),
		})
		_, err = s.client.CertificatesV1().CertificateSigningRequests().UpdateStatus(ctx, csr, metav1.UpdateOptions{})
		if err != nil {
			return fmt.Errorf("error adding failure condition for csr: %v", err)
		}
		return nil
	} else if !recognized {
		// Ignore requests for kubernetes.io signerNames we don't recognize
		return nil
	}
	cert, err := s.sign(x509cr, csr.Spec.Usages, csr.Spec.ExpirationSeconds, nil)
	if err != nil {
		return fmt.Errorf("error auto signing csr: %v", err)
	}
	csr.Status.Certificate = cert
	_, err = s.client.CertificatesV1().CertificateSigningRequests().UpdateStatus(ctx, csr, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("error updating signature for csr: %v", err)
	}
	return nil
}

func (s *signer) sign(x509cr *x509.CertificateRequest, usages []capi.KeyUsage, expirationSeconds *int32, now func() time.Time) ([]byte, error) {
	currCA, err := s.caProvider.currentCA()
	if err != nil {
		return nil, err
	}
	der, err := currCA.Sign(x509cr.Raw, authority.PermissiveSigningPolicy{
		TTL:      s.duration(expirationSeconds),
		Usages:   usages,
		Backdate: 5 * time.Minute, // this must always be less than the minimum TTL requested by a user (see sanity check requestedDuration below)
		Short:    8 * time.Hour,   // 5 minutes of backdating is roughly 1% of 8 hours
		Now:      now,
	})
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), nil
}

func (s *signer) duration(expirationSeconds *int32) time.Duration {
	if expirationSeconds == nil {
		return s.certTTL
	}

	// honor requested duration is if it is less than the default TTL
	// use 10 min (2x hard coded backdate above) as a sanity check lower bound
	const min = 10 * time.Minute
	switch requestedDuration := csr.ExpirationSecondsToDuration(*expirationSeconds); {
	case requestedDuration > s.certTTL:
		return s.certTTL

	case requestedDuration < min:
		return min

	default:
		return requestedDuration
	}
}

// getCSRVerificationFuncForSignerName is a function that provides reliable mapping of signer names to verification so that
// we don't have accidents with wiring at some later date.
func getCSRVerificationFuncForSignerName(signerName string) (isRequestForSignerFunc, error) {
	switch signerName {
	case capi.KubeletServingSignerName:
		return isKubeletServing, nil
	case capi.KubeAPIServerClientKubeletSignerName:
		return isKubeletClient, nil
	case capi.KubeAPIServerClientSignerName:
		return isKubeAPIServerClient, nil
	case capiv1beta1.LegacyUnknownSignerName:
		return isLegacyUnknown, nil
	default:
		// TODO type this error so that a different reporting loop (one without a signing cert), can mark
		//  CSRs with unknown kube signers as terminal if we wish.  This largely depends on how tightly we want to control
		//  our signerNames.
		return nil, fmt.Errorf("unrecognized signerName: %q", signerName)
	}
}

func isKubeletServing(req *x509.CertificateRequest, usages []capi.KeyUsage, signerName string) (bool, error) {
	if signerName != capi.KubeletServingSignerName {
		return false, nil
	}
	return true, capihelper.ValidateKubeletServingCSR(req, usagesToSet(usages))
}

func isKubeletClient(req *x509.CertificateRequest, usages []capi.KeyUsage, signerName string) (bool, error) {
	if signerName != capi.KubeAPIServerClientKubeletSignerName {
		return false, nil
	}
	return true, capihelper.ValidateKubeletClientCSR(req, usagesToSet(usages))
}

func isKubeAPIServerClient(req *x509.CertificateRequest, usages []capi.KeyUsage, signerName string) (bool, error) {
	if signerName != capi.KubeAPIServerClientSignerName {
		return false, nil
	}
	return true, validAPIServerClientUsages(usages)
}

func isLegacyUnknown(req *x509.CertificateRequest, usages []capi.KeyUsage, signerName string) (bool, error) {
	if signerName != capiv1beta1.LegacyUnknownSignerName {
		return false, nil
	}
	// No restrictions are applied to the legacy-unknown signerName to
	// maintain backward compatibility in v1.
	return true, nil
}

func validAPIServerClientUsages(usages []capi.KeyUsage) error {
	hasClientAuth := false
	for _, u := range usages {
		switch u {
		// these usages are optional
		case capi.UsageDigitalSignature, capi.UsageKeyEncipherment:
		case capi.UsageClientAuth:
			hasClientAuth = true
		default:
			return fmt.Errorf("invalid usage for client certificate: %s", u)
		}
	}
	if !hasClientAuth {
		return fmt.Errorf("missing required usage for client certificate: %s", capi.UsageClientAuth)
	}
	return nil
}

func usagesToSet(usages []capi.KeyUsage) sets.String {
	result := sets.NewString()
	for _, usage := range usages {
		result.Insert(string(usage))
	}
	return result
}
