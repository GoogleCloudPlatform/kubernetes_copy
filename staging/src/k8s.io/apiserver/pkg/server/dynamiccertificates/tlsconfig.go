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
	"errors"
	"fmt"
	"net"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/cert"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
)

const workItemKey = "key"

// DynamicServingCertificateController dynamically loads certificates and provides a golang tls compatible dynamic GetCertificate func.
type DynamicServingCertificateController struct {
	// baseTLSConfig is the static portion of the tlsConfig for serving to clients.  It is copied and the copy is mutated
	// based on the dynamic cert state.
	baseTLSConfig *tls.Config

	// clientCA provides the very latest content of the ca bundle
	clientCA CAContentProvider
	// servingCert provides the very latest content of the default serving certificate
	servingCert CertKeyContentProvider
	// sniCerts are a list of CertKeyContentProvider with associated names used for SNI
	sniCerts []SNICertKeyContentProvider

	// currentlyServedContent holds the original bytes that we are serving. This is used to decide if we need to set a
	// new atomic value. The types used for efficient TLSConfig preclude using the processed value.
	currentlyServedContent *dynamicCertificateContent
	// currentServingTLSConfig holds a *tls.Config that will be used to serve requests
	currentServingTLSConfig atomic.Value

	// queue only ever has one item, but it has nice error handling backoff/retry semantics
	queue         workqueue.TypedRateLimitingInterface[string]
	eventRecorder events.EventRecorder
}

var _ Listener = &DynamicServingCertificateController{}

// NewDynamicServingCertificateController returns a controller that can be used to keep a TLSConfig up to date.
func NewDynamicServingCertificateController(
	baseTLSConfig *tls.Config,
	clientCA CAContentProvider,
	servingCert CertKeyContentProvider,
	sniCerts []SNICertKeyContentProvider,
	eventRecorder events.EventRecorder,
) *DynamicServingCertificateController {
	c := &DynamicServingCertificateController{
		baseTLSConfig: baseTLSConfig,
		clientCA:      clientCA,
		servingCert:   servingCert,
		sniCerts:      sniCerts,

		queue: workqueue.NewTypedRateLimitingQueueWithConfig(
			workqueue.DefaultTypedControllerRateLimiter[string](),
			workqueue.TypedRateLimitingQueueConfig[string]{Name: "DynamicServingCertificateController"},
		),
		eventRecorder: eventRecorder,
	}

	return c
}

// GetConfigForClient is an implementation of tls.Config.GetConfigForClient
func (c *DynamicServingCertificateController) GetConfigForClient(clientHello *tls.ClientHelloInfo) (*tls.Config, error) {
	uncastObj := c.currentServingTLSConfig.Load()
	if uncastObj == nil {
		return nil, errors.New("dynamiccertificates: configuration not ready")
	}
	tlsConfig, ok := uncastObj.(*tls.Config)
	if !ok {
		return nil, errors.New("dynamiccertificates: unexpected config type")
	}

	tlsConfigCopy := tlsConfig.Clone()

	// if the client set SNI information, just use our "normal" SNI flow
	if len(clientHello.ServerName) > 0 {
		return tlsConfigCopy, nil
	}

	// if the client didn't set SNI, then we need to inspect the requested IP so that we can choose
	// a certificate from our list if we specifically handle that IP.  This can happen when an IP is specifically mapped by name.
	host, _, err := net.SplitHostPort(clientHello.Conn.LocalAddr().String())
	if err != nil {
		return tlsConfigCopy, nil
	}

	ipCert, ok := tlsConfigCopy.NameToCertificate[host]
	if !ok {
		return tlsConfigCopy, nil
	}
	tlsConfigCopy.Certificates = []tls.Certificate{*ipCert}
	tlsConfigCopy.NameToCertificate = nil

	return tlsConfigCopy, nil
}

// newTLSContent determines the next set of content for overriding the baseTLSConfig.
func (c *DynamicServingCertificateController) newTLSContent() (*dynamicCertificateContent, error) {
	newContent := &dynamicCertificateContent{}

	if c.clientCA != nil {
		currClientCABundle := c.clientCA.CurrentCABundleContent()
		// we allow removing all client ca bundles because the server is still secure when this happens. it just means
		// that there isn't a hint to clients about which client-cert to used.  this happens when there is no client-ca
		// yet known for authentication, which can happen in aggregated apiservers and some kube-apiserver deployment modes.
		newContent.clientCA = caBundleContent{caBundle: currClientCABundle}
	}

	if c.servingCert != nil {
		currServingCert, currServingKey := c.servingCert.CurrentCertKeyContent()
		if len(currServingCert) == 0 || len(currServingKey) == 0 {
			return nil, fmt.Errorf("not loading an empty serving certificate from %q", c.servingCert.Name())
		}

		newContent.servingCert = certKeyContent{cert: currServingCert, key: currServingKey}
	}

	for i, sniCert := range c.sniCerts {
		currCert, currKey := sniCert.CurrentCertKeyContent()
		if len(currCert) == 0 || len(currKey) == 0 {
			return nil, fmt.Errorf("not loading an empty SNI certificate from %d/%q", i, sniCert.Name())
		}

		newContent.sniCerts = append(newContent.sniCerts, sniCertKeyContent{certKeyContent: certKeyContent{cert: currCert, key: currKey}, sniNames: sniCert.SNINames()})
	}

	return newContent, nil
}

// syncCerts gets newTLSContent, if it has changed from the existing, the content is parsed and stored for usage in
// GetConfigForClient.
func (c *DynamicServingCertificateController) syncCerts(ctx context.Context) error {
	logger := klog.FromContext(ctx)
	newContent, err := c.newTLSContent()
	if err != nil {
		return err
	}
	// if the content is the same as what we currently have, we can simply skip it.  This works because we are single
	// threaded.  If you ever make this multi-threaded, add a lock.
	if newContent.Equal(c.currentlyServedContent) {
		return nil
	}

	// make a shallow copy and override the dynamic pieces which have changed.
	newTLSConfigCopy := c.baseTLSConfig.Clone()

	// parse new content to add to TLSConfig
	if len(newContent.clientCA.caBundle) > 0 {
		newClientCAPool := x509.NewCertPool()
		newClientCAs, err := cert.ParseCertsPEM(newContent.clientCA.caBundle)
		if err != nil {
			return fmt.Errorf("unable to load client CA file %q: %v", string(newContent.clientCA.caBundle), err)
		}
		for i, cert := range newClientCAs {
			logger.V(2).Info("Loaded client CA", "index", i, "certName", c.clientCA.Name(), "certDetail", GetHumanCertDetail(cert))
			if c.eventRecorder != nil {
				c.eventRecorder.Eventf(&corev1.ObjectReference{Name: c.clientCA.Name()}, nil, corev1.EventTypeWarning, "TLSConfigChanged", "CACertificateReload", "loaded client CA [%d/%q]: %s", i, c.clientCA.Name(), GetHumanCertDetail(cert))
			}

			newClientCAPool.AddCert(cert)
		}

		newTLSConfigCopy.ClientCAs = newClientCAPool
	}

	if len(newContent.servingCert.cert) > 0 && len(newContent.servingCert.key) > 0 {
		cert, err := tls.X509KeyPair(newContent.servingCert.cert, newContent.servingCert.key)
		if err != nil {
			return fmt.Errorf("invalid serving cert keypair: %v", err)
		}

		x509Cert, err := x509.ParseCertificate(cert.Certificate[0])
		if err != nil {
			return fmt.Errorf("invalid serving cert: %v", err)
		}

		logger.V(2).Info("Loaded serving cert", "certName", c.servingCert.Name(), "certDetail", GetHumanCertDetail(x509Cert))
		if c.eventRecorder != nil {
			c.eventRecorder.Eventf(&corev1.ObjectReference{Name: c.servingCert.Name()}, nil, corev1.EventTypeWarning, "TLSConfigChanged", "ServingCertificateReload", "loaded serving cert [%q]: %s", c.servingCert.Name(), GetHumanCertDetail(x509Cert))
		}

		newTLSConfigCopy.Certificates = []tls.Certificate{cert}
	}

	if len(newContent.sniCerts) > 0 {
		newTLSConfigCopy.NameToCertificate, err = c.BuildNamedCertificates(ctx, newContent.sniCerts)
		if err != nil {
			return fmt.Errorf("unable to build named certificate map: %v", err)
		}

		// append all named certs. Otherwise, the go tls stack will think no SNI processing
		// is necessary because there is only one cert anyway.
		// Moreover, if servingCert is not set, the first SNI
		// cert will become the default cert. That's what we expect anyway.
		for _, sniCert := range newTLSConfigCopy.NameToCertificate {
			newTLSConfigCopy.Certificates = append(newTLSConfigCopy.Certificates, *sniCert)
		}
	}

	// store new values of content for serving.
	c.currentServingTLSConfig.Store(newTLSConfigCopy)
	c.currentlyServedContent = newContent // this is single threaded, so we have no locking issue

	return nil
}

// RunOnce runs a single sync step to ensure that we have a valid starting configuration.
func (c *DynamicServingCertificateController) RunOnce(ctx context.Context) error {
	return c.syncCerts(ctx)
}

// Run starts the kube-apiserver and blocks until stopCh is closed.
func (c *DynamicServingCertificateController) Run(ctx context.Context, workers int, stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()
	defer c.queue.ShutDown()

	logger := klog.FromContext(ctx)
	logger.Info("Starting DynamicServingCertificateController")
	defer logger.Info("Shutting down DynamicServingCertificateController")

	// synchronously load once.  We will trigger again, so ignoring any error is fine
	_ = c.RunOnce(ctx)

	// doesn't matter what workers say, only start one.
	go wait.UntilWithContext(ctx, func(ctx context.Context) {
		c.runWorker(ctx)
	}, time.Second)

	// start timer that rechecks every minute, just in case.  this also serves to prime the controller quickly.
	go wait.Until(func() {
		c.Enqueue()
	}, 1*time.Minute, stopCh)

	<-stopCh
}

func (c *DynamicServingCertificateController) runWorker(ctx context.Context) {
	for c.processNextWorkItem(ctx) {
	}
}

func (c *DynamicServingCertificateController) processNextWorkItem(ctx context.Context) bool {
	dsKey, quit := c.queue.Get()
	if quit {
		return false
	}
	defer c.queue.Done(dsKey)

	err := c.syncCerts(ctx)
	if err == nil {
		c.queue.Forget(dsKey)
		return true
	}

	utilruntime.HandleError(fmt.Errorf("%v failed with : %v", dsKey, err))
	c.queue.AddRateLimited(dsKey)

	return true
}

// Enqueue a method to allow separate control loops to cause the certificate controller to trigger and read content.
func (c *DynamicServingCertificateController) Enqueue() {
	c.queue.Add(workItemKey)
}
