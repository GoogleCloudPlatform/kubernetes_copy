/*
Copyright 2024 The Kubernetes Authors.

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

package clustertrustbundlepublisher

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"time"

	certificatesv1beta1 "k8s.io/api/certificates/v1beta1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apiserver/pkg/server/dynamiccertificates"
	certinformers "k8s.io/client-go/informers/certificates/v1beta1"
	clientset "k8s.io/client-go/kubernetes"
	certlisters "k8s.io/client-go/listers/certificates/v1beta1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
)

func init() {
	registerMetrics()
}

type ClusterTrustBundlePublisher struct {
	signerName string
	ca         dynamiccertificates.CAContentProvider

	client clientset.Interface

	ctbInformer     cache.SharedIndexInformer
	ctbLister       certlisters.ClusterTrustBundleLister
	ctbListerSynced cache.InformerSynced

	queue workqueue.TypedRateLimitingInterface[string]
}

type caContentListener func()

func (f caContentListener) Enqueue() {
	f()
}

// NewClusterTrustBundlePublisher creates and maintains a cluster trust bundle object
// for a signer named `signerName`. The cluster trust bundle object contains the
// CA from the `caProvider` in its .spec.TrustBundle.
func NewClusterTrustBundlePublisher(
	signerName string,
	caProvider dynamiccertificates.CAContentProvider,
	kubeClient clientset.Interface,
) (*ClusterTrustBundlePublisher, error) {
	if len(signerName) == 0 {
		return nil, fmt.Errorf("signerName cannot be empty")
	}

	p := &ClusterTrustBundlePublisher{
		signerName: signerName,
		ca:         caProvider,
		client:     kubeClient,

		queue: workqueue.NewTypedRateLimitingQueueWithConfig(
			workqueue.DefaultTypedControllerRateLimiter[string](),
			workqueue.TypedRateLimitingQueueConfig[string]{
				Name: "ca_cert_publisher_cluster_trust_bundles",
			},
		),
	}
	p.ctbInformer = setupSignerNameFilteredCTBInformer(p.client, p.signerName)
	p.ctbLister = certlisters.NewClusterTrustBundleLister(p.ctbInformer.GetIndexer())
	p.ctbListerSynced = p.ctbInformer.HasSynced

	_, err := p.ctbInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			p.queue.Add("")
		},
		UpdateFunc: func(_, _ interface{}) {
			p.queue.Add("")
		},
		DeleteFunc: func(_ interface{}) {
			p.queue.Add("")
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to register ClusterTrustBundle event handler: %w", err)
	}
	p.ca.AddListener(p.caContentChangedListener())

	return p, nil
}

func (p *ClusterTrustBundlePublisher) caContentChangedListener() dynamiccertificates.Listener {
	return caContentListener(func() {
		p.queue.Add("")
	})
}

func (p *ClusterTrustBundlePublisher) Run(ctx context.Context) {
	defer utilruntime.HandleCrash()
	defer p.queue.ShutDown()

	logger := klog.FromContext(ctx)
	logger.Info("Starting ClusterTrustBundle CA cert publisher controller")
	defer logger.Info("Shutting down ClusterTrustBundle CA cert publisher controller")

	go p.ctbInformer.Run(ctx.Done())

	if !cache.WaitForNamedCacheSync("cluster trust bundle", ctx.Done(), p.ctbListerSynced) {
		return
	}

	// init the signer syncer
	p.queue.Add("")
	go wait.UntilWithContext(ctx, p.runWorker(), time.Second)

	<-ctx.Done()
}

func (p *ClusterTrustBundlePublisher) runWorker() func(context.Context) {
	return func(ctx context.Context) {
		for p.processNextWorkItem(ctx) {
		}
	}
}

// processNextWorkItem deals with one key off the queue. It returns false when
// it's time to quit.
func (p *ClusterTrustBundlePublisher) processNextWorkItem(ctx context.Context) bool {
	key, quit := p.queue.Get()
	if quit {
		return false
	}
	defer p.queue.Done(key)

	if err := p.syncClusterTrustBundle(ctx); err != nil {
		utilruntime.HandleError(fmt.Errorf("syncing %q failed: %w", key, err))
		p.queue.AddRateLimited(key)
		return true
	}

	p.queue.Forget(key)
	return true
}

func (p *ClusterTrustBundlePublisher) syncClusterTrustBundle(ctx context.Context) (err error) {
	startTime := time.Now()
	defer func() {
		recordMetrics(startTime, err)
		klog.FromContext(ctx).V(4).Info("Finished syncing ClusterTrustBundle", "signerName", p.signerName, "elapsedTime", time.Since(startTime))
	}()

	caBundle := string(p.ca.CurrentCABundleContent())
	bundleName := constructBundleName(p.signerName, []byte(caBundle))

	bundle, err := p.ctbLister.Get(bundleName)
	if apierrors.IsNotFound(err) {
		_, err = p.client.CertificatesV1beta1().ClusterTrustBundles().Create(ctx, &certificatesv1beta1.ClusterTrustBundle{
			ObjectMeta: metav1.ObjectMeta{
				Name: bundleName,
			},
			Spec: certificatesv1beta1.ClusterTrustBundleSpec{
				SignerName:  p.signerName,
				TrustBundle: caBundle,
			},
		}, metav1.CreateOptions{})
	} else if err == nil && bundle.Spec.TrustBundle != caBundle {
		bundle = bundle.DeepCopy()
		bundle.Spec.TrustBundle = caBundle
		_, err = p.client.CertificatesV1beta1().ClusterTrustBundles().Update(ctx, bundle, metav1.UpdateOptions{})
	}

	if err != nil {
		return err
	}

	signerTrustBundles, err := p.ctbLister.List(labels.Everything())
	if err != nil {
		return err
	}

	// keep the deletion error to be returned in the end in order to retrigger the reconciliation loop
	var deletionError error
	for _, bundleObject := range signerTrustBundles {
		if bundleObject.Name == bundleName {
			continue
		}

		if err := p.client.CertificatesV1beta1().ClusterTrustBundles().Delete(ctx, bundleObject.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			klog.FromContext(ctx).Error(err, "failed to remove a cluster trust bundle", "bundleName", bundleObject.Name)
			deletionError = err
		}
	}

	return deletionError
}

func setupSignerNameFilteredCTBInformer(client clientset.Interface, signerName string) cache.SharedIndexInformer {
	return certinformers.NewFilteredClusterTrustBundleInformer(client, 0, cache.Indexers{},
		func(options *metav1.ListOptions) {
			options.FieldSelector = fields.OneTermEqualSelector("spec.signerName", signerName).String()
		})
}

func constructBundleName(signerName string, bundleBytes []byte) string {
	namePrefix := strings.ReplaceAll(signerName, "/", ":") + ":"
	bundleHash := sha256.Sum256(bundleBytes)
	return fmt.Sprintf("%s%x", namePrefix, bundleHash[:12])

}
