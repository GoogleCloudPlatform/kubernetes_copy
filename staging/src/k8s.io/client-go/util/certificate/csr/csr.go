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

package csr

import (
	"context"
	"crypto"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"reflect"
	"time"

	certificatesv1 "k8s.io/api/certificates/v1"
	certificatesv1beta1 "k8s.io/api/certificates/v1beta1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	watchtools "k8s.io/client-go/tools/watch"
	certutil "k8s.io/client-go/util/cert"
	"k8s.io/klog/v2"
	"k8s.io/utils/pointer"
)

// RequestCertificate will either use an existing (if this process has run
// before but not to completion) or create a certificate signing request using the
// PEM encoded CSR and send it to API server.  An optional requestedDuration may be passed
// to set the spec.expirationSeconds field on the CSR to control the lifetime of the issued
// certificate.  This is not guaranteed as the signer may choose to ignore the request.
//
// Deprecated: use RequestCertificateWithContext instead.
func RequestCertificate(client clientset.Interface, csrData []byte, name, signerName string, requestedDuration *time.Duration, usages []certificatesv1.KeyUsage, privateKey interface{}) (reqName string, reqUID types.UID, err error) {
	return RequestCertificateWithContext(context.Background(), client, csrData, name, signerName, requestedDuration, usages, privateKey)
}

// RequestCertificateWithContext will either use an existing (if this process has run
// before but not to completion) or create a certificate signing request using the
// PEM encoded CSR and send it to API server.  An optional requestedDuration may be passed
// to set the spec.expirationSeconds field on the CSR to control the lifetime of the issued
// certificate.  This is not guaranteed as the signer may choose to ignore the request.
func RequestCertificateWithContext(ctx context.Context, client clientset.Interface, csrData []byte, name, signerName string, requestedDuration *time.Duration, usages []certificatesv1.KeyUsage, privateKey interface{}) (reqName string, reqUID types.UID, err error) {
	csr := &certificatesv1.CertificateSigningRequest{
		// Username, UID, Groups will be injected by API server.
		TypeMeta: metav1.TypeMeta{Kind: "CertificateSigningRequest"},
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: certificatesv1.CertificateSigningRequestSpec{
			Request:    csrData,
			Usages:     usages,
			SignerName: signerName,
		},
	}
	if len(csr.Name) == 0 {
		csr.GenerateName = "csr-"
	}
	if requestedDuration != nil {
		csr.Spec.ExpirationSeconds = DurationToExpirationSeconds(*requestedDuration)
	}

	reqName, reqUID, err = create(ctx, client, csr)
	switch {
	case err == nil:
		return reqName, reqUID, err

	case apierrors.IsAlreadyExists(err) && len(name) > 0:
		logger := klog.FromContext(ctx)
		logger.Info("csr for this node already exists, reusing")
		req, err := get(ctx, client, name)
		if err != nil {
			return "", "", formatError("cannot retrieve certificate signing request: %v", err)
		}
		if err := ensureCompatible(req, csr, privateKey); err != nil {
			return "", "", fmt.Errorf("retrieved csr is not compatible: %v", err)
		}
		logger.Info("csr for this node is still valid")
		return req.Name, req.UID, nil

	default:
		return "", "", formatError("cannot create certificate signing request: %v", err)
	}
}

func DurationToExpirationSeconds(duration time.Duration) *int32 {
	return pointer.Int32(int32(duration / time.Second))
}

func ExpirationSecondsToDuration(expirationSeconds int32) time.Duration {
	return time.Duration(expirationSeconds) * time.Second
}

func get(ctx context.Context, client clientset.Interface, name string) (*certificatesv1.CertificateSigningRequest, error) {
	v1req, v1err := client.CertificatesV1().CertificateSigningRequests().Get(ctx, name, metav1.GetOptions{})
	if v1err == nil || !apierrors.IsNotFound(v1err) {
		return v1req, v1err
	}

	v1beta1req, v1beta1err := client.CertificatesV1beta1().CertificateSigningRequests().Get(ctx, name, metav1.GetOptions{})
	if v1beta1err != nil {
		return nil, v1beta1err
	}

	v1req = &certificatesv1.CertificateSigningRequest{
		ObjectMeta: v1beta1req.ObjectMeta,
		Spec: certificatesv1.CertificateSigningRequestSpec{
			Request: v1beta1req.Spec.Request,
		},
	}
	if v1beta1req.Spec.SignerName != nil {
		v1req.Spec.SignerName = *v1beta1req.Spec.SignerName
	}
	for _, usage := range v1beta1req.Spec.Usages {
		v1req.Spec.Usages = append(v1req.Spec.Usages, certificatesv1.KeyUsage(usage))
	}
	return v1req, nil
}

func create(ctx context.Context, client clientset.Interface, csr *certificatesv1.CertificateSigningRequest) (reqName string, reqUID types.UID, err error) {
	// only attempt a create via v1 if we specified signerName and usages and are not using the legacy unknown signerName
	if len(csr.Spec.Usages) > 0 && len(csr.Spec.SignerName) > 0 && csr.Spec.SignerName != "kubernetes.io/legacy-unknown" {
		v1req, v1err := client.CertificatesV1().CertificateSigningRequests().Create(ctx, csr, metav1.CreateOptions{})
		switch {
		case v1err != nil && apierrors.IsNotFound(v1err):
			// v1 CSR API was not found, continue to try v1beta1

		case v1err != nil:
			// other creation error
			return "", "", v1err

		default:
			// success
			return v1req.Name, v1req.UID, v1err
		}
	}

	// convert relevant bits to v1beta1
	v1beta1csr := &certificatesv1beta1.CertificateSigningRequest{
		ObjectMeta: csr.ObjectMeta,
		Spec: certificatesv1beta1.CertificateSigningRequestSpec{
			SignerName: &csr.Spec.SignerName,
			Request:    csr.Spec.Request,
		},
	}
	for _, usage := range csr.Spec.Usages {
		v1beta1csr.Spec.Usages = append(v1beta1csr.Spec.Usages, certificatesv1beta1.KeyUsage(usage))
	}

	// create v1beta1
	v1beta1req, v1beta1err := client.CertificatesV1beta1().CertificateSigningRequests().Create(ctx, v1beta1csr, metav1.CreateOptions{})
	if v1beta1err != nil {
		return "", "", v1beta1err
	}
	return v1beta1req.Name, v1beta1req.UID, nil
}

// WaitForCertificate waits for a certificate to be issued until timeout, or returns an error.
func WaitForCertificate(ctx context.Context, client clientset.Interface, reqName string, reqUID types.UID) (certData []byte, err error) {
	fieldSelector := fields.OneTermEqualSelector("metadata.name", reqName).String()
	logger := klog.FromContext(ctx)

	var lw *cache.ListWatch
	var obj runtime.Object
	for {
		// see if the v1 API is available
		if _, err := client.CertificatesV1().CertificateSigningRequests().List(ctx, metav1.ListOptions{FieldSelector: fieldSelector}); err == nil {
			// watch v1 objects
			obj = &certificatesv1.CertificateSigningRequest{}
			lw = &cache.ListWatch{
				ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
					options.FieldSelector = fieldSelector
					return client.CertificatesV1().CertificateSigningRequests().List(ctx, options)
				},
				WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
					options.FieldSelector = fieldSelector
					return client.CertificatesV1().CertificateSigningRequests().Watch(ctx, options)
				},
			}
			break
		} else {
			logger.V(2).Info("Error fetching v1 certificate signing request", "err", err)
		}

		// return if we've timed out
		if err := ctx.Err(); err != nil {
			return nil, wait.ErrWaitTimeout
		}

		// see if the v1beta1 API is available
		if _, err := client.CertificatesV1beta1().CertificateSigningRequests().List(ctx, metav1.ListOptions{FieldSelector: fieldSelector}); err == nil {
			// watch v1beta1 objects
			obj = &certificatesv1beta1.CertificateSigningRequest{}
			lw = &cache.ListWatch{
				ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
					options.FieldSelector = fieldSelector
					return client.CertificatesV1beta1().CertificateSigningRequests().List(ctx, options)
				},
				WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
					options.FieldSelector = fieldSelector
					return client.CertificatesV1beta1().CertificateSigningRequests().Watch(ctx, options)
				},
			}
			break
		} else {
			logger.V(2).Info("Error fetching v1beta1 certificate signing request", "err", err)
		}

		// return if we've timed out
		if err := ctx.Err(); err != nil {
			return nil, wait.ErrWaitTimeout
		}

		// wait and try again
		time.Sleep(time.Second)
	}

	var issuedCertificate []byte
	_, err = watchtools.UntilWithSync(
		ctx,
		lw,
		obj,
		nil,
		func(event watch.Event) (bool, error) {
			switch event.Type {
			case watch.Modified, watch.Added:
			case watch.Deleted:
				return false, fmt.Errorf("csr %q was deleted", reqName)
			default:
				return false, nil
			}

			switch csr := event.Object.(type) {
			case *certificatesv1.CertificateSigningRequest:
				if csr.UID != reqUID {
					return false, fmt.Errorf("csr %q changed UIDs", csr.Name)
				}
				approved := false
				for _, c := range csr.Status.Conditions {
					if c.Type == certificatesv1.CertificateDenied {
						return false, fmt.Errorf("certificate signing request is denied, reason: %v, message: %v", c.Reason, c.Message)
					}
					if c.Type == certificatesv1.CertificateFailed {
						return false, fmt.Errorf("certificate signing request failed, reason: %v, message: %v", c.Reason, c.Message)
					}
					if c.Type == certificatesv1.CertificateApproved {
						approved = true
					}
				}
				if approved {
					if len(csr.Status.Certificate) > 0 {
						logger.V(2).Info("Certificate signing request is issued", "csr", klog.KObj(csr))
						issuedCertificate = csr.Status.Certificate
						return true, nil
					}
					logger.V(2).Info("Certificate signing request is approved, waiting to be issued", "csr", klog.KObj(csr))
				}

			case *certificatesv1beta1.CertificateSigningRequest:
				if csr.UID != reqUID {
					return false, fmt.Errorf("csr %q changed UIDs", csr.Name)
				}
				approved := false
				for _, c := range csr.Status.Conditions {
					if c.Type == certificatesv1beta1.CertificateDenied {
						return false, fmt.Errorf("certificate signing request is denied, reason: %v, message: %v", c.Reason, c.Message)
					}
					if c.Type == certificatesv1beta1.CertificateFailed {
						return false, fmt.Errorf("certificate signing request failed, reason: %v, message: %v", c.Reason, c.Message)
					}
					if c.Type == certificatesv1beta1.CertificateApproved {
						approved = true
					}
				}
				if approved {
					if len(csr.Status.Certificate) > 0 {
						logger.V(2).Info("Certificate signing request is issued", "csr", klog.KObj(csr))
						issuedCertificate = csr.Status.Certificate
						return true, nil
					}
					logger.V(2).Info("Certificate signing request is approved, waiting to be issued", "csr", klog.KObj(csr))
				}

			default:
				return false, fmt.Errorf("unexpected type received: %T", event.Object)
			}

			return false, nil
		},
	)
	if err == wait.ErrWaitTimeout {
		return nil, wait.ErrWaitTimeout
	}
	if err != nil {
		return nil, formatError("cannot watch on the certificate signing request: %v", err)
	}

	return issuedCertificate, nil
}

// ensureCompatible ensures that a CSR object is compatible with an original CSR
func ensureCompatible(new, orig *certificatesv1.CertificateSigningRequest, privateKey interface{}) error {
	newCSR, err := parseCSR(new.Spec.Request)
	if err != nil {
		return fmt.Errorf("unable to parse new csr: %v", err)
	}
	origCSR, err := parseCSR(orig.Spec.Request)
	if err != nil {
		return fmt.Errorf("unable to parse original csr: %v", err)
	}
	if !reflect.DeepEqual(newCSR.Subject, origCSR.Subject) {
		return fmt.Errorf("csr subjects differ: new: %#v, orig: %#v", newCSR.Subject, origCSR.Subject)
	}
	if len(new.Spec.SignerName) > 0 && len(orig.Spec.SignerName) > 0 && new.Spec.SignerName != orig.Spec.SignerName {
		return fmt.Errorf("csr signerNames differ: new %q, orig: %q", new.Spec.SignerName, orig.Spec.SignerName)
	}
	signer, ok := privateKey.(crypto.Signer)
	if !ok {
		return fmt.Errorf("privateKey is not a signer")
	}
	newCSR.PublicKey = signer.Public()
	if err := newCSR.CheckSignature(); err != nil {
		return fmt.Errorf("error validating signature new CSR against old key: %v", err)
	}
	if len(new.Status.Certificate) > 0 {
		certs, err := certutil.ParseCertsPEM(new.Status.Certificate)
		if err != nil {
			return fmt.Errorf("error parsing signed certificate for CSR: %v", err)
		}
		now := time.Now()
		for _, cert := range certs {
			if now.After(cert.NotAfter) {
				return fmt.Errorf("one of the certificates for the CSR has expired: %s", cert.NotAfter)
			}
		}
	}
	return nil
}

// formatError preserves the type of an API message but alters the message. Expects
// a single argument format string, and returns the wrapped error.
func formatError(format string, err error) error {
	if s, ok := err.(apierrors.APIStatus); ok {
		se := &apierrors.StatusError{ErrStatus: s.Status()}
		se.ErrStatus.Message = fmt.Sprintf(format, se.ErrStatus.Message)
		return se
	}
	return fmt.Errorf(format, err)
}

// parseCSR extracts the CSR from the API object and decodes it.
func parseCSR(pemData []byte) (*x509.CertificateRequest, error) {
	// extract PEM from request object
	block, _ := pem.Decode(pemData)
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		return nil, fmt.Errorf("PEM block type must be CERTIFICATE REQUEST")
	}
	return x509.ParseCertificateRequest(block.Bytes)
}
