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

	certificatesv1alpha1 "k8s.io/api/certificates/v1alpha1"
	certificatesv1beta1 "k8s.io/api/certificates/v1beta1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apiserver/pkg/server/dynamiccertificates"
	certalpha1informers "k8s.io/client-go/informers/certificates/v1alpha1"
	certbeta1informers "k8s.io/client-go/informers/certificates/v1beta1"
	clientset "k8s.io/client-go/kubernetes"
	certalphav1listers "k8s.io/client-go/listers/certificates/v1alpha1"
	certbetav1listers "k8s.io/client-go/listers/certificates/v1beta1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
)

func init() {
	registerMetrics()
}

type PublisherRunner interface {
	Run(context.Context)
}

type ClusterTrustBundlePublisher[T clusterTrustBundle] struct {
	signerName string
	ca         dynamiccertificates.CAContentProvider

	client clusterTrustBundlesClient[T]

	ctbInformer     cache.SharedIndexInformer
	ctbLister       clusterTrustBundlesLister[T]
	ctbListerSynced cache.InformerSynced

	handlers clusterTrustBundleHandlers[T]

	queue workqueue.TypedRateLimitingInterface[string]
}

// clusterTrustBundle is a type constraint grouping all APIs versions of ClusterTrustBundles
type clusterTrustBundle interface {
	certificatesv1alpha1.ClusterTrustBundle | certificatesv1beta1.ClusterTrustBundle
}

// clusterTrustBundlesClient is an API-version independent client for the ClusterTrustBundles API
type clusterTrustBundlesClient[T clusterTrustBundle] interface {
	Create(context.Context, *T, metav1.CreateOptions) (*T, error)
	Update(context.Context, *T, metav1.UpdateOptions) (*T, error)
	Delete(context.Context, string, metav1.DeleteOptions) error
}

// clusterTrustBundlesLister is an API-version independent lister for the ClusterTrustBundles API
type clusterTrustBundlesLister[T clusterTrustBundle] interface {
	Get(string) (*T, error)
	List(labels.Selector) ([]*T, error)
}

type clusterTrustBundleHandlers[T clusterTrustBundle] interface {
	createClusterTrustBundle(bundleName, signerName, trustBundle string) *T
	updateWithTrustBundle(ctbObject *T, newBundle string) *T
	containsTrustBundle(ctbObject *T, bundle string) bool
	getName(ctbObject *T) string
}

var _ clusterTrustBundleHandlers[certificatesv1beta1.ClusterTrustBundle] = &betaHandlers{}
var _ clusterTrustBundleHandlers[certificatesv1alpha1.ClusterTrustBundle] = &alphaHandlers{}

// betaHandlers groups the `clusterTrustBundleHandlers` for the v1beta1 API of
// clusterTrustBundles
type betaHandlers struct{}

func (w *betaHandlers) createClusterTrustBundle(bundleName, signerName, trustBundle string) *certificatesv1beta1.ClusterTrustBundle {
	return &certificatesv1beta1.ClusterTrustBundle{
		ObjectMeta: metav1.ObjectMeta{
			Name: bundleName,
		},
		Spec: certificatesv1beta1.ClusterTrustBundleSpec{
			SignerName:  signerName,
			TrustBundle: trustBundle,
		},
	}
}

func (w *betaHandlers) updateWithTrustBundle(ctbObject *certificatesv1beta1.ClusterTrustBundle, newBundle string) *certificatesv1beta1.ClusterTrustBundle {
	newObj := ctbObject.DeepCopy()
	newObj.Spec.TrustBundle = newBundle
	return newObj
}

func (w *betaHandlers) containsTrustBundle(ctbObject *certificatesv1beta1.ClusterTrustBundle, bundle string) bool {
	return ctbObject.Spec.TrustBundle == bundle
}

func (w *betaHandlers) getName(ctbObject *certificatesv1beta1.ClusterTrustBundle) string {
	return ctbObject.Name
}

// alphaHandlers groups the `clusterTrustBundleHandlers` for the v1alpha1 API of
// clusterTrustBundles
type alphaHandlers struct{}

func (w *alphaHandlers) createClusterTrustBundle(bundleName, signerName, trustBundle string) *certificatesv1alpha1.ClusterTrustBundle {
	return &certificatesv1alpha1.ClusterTrustBundle{
		ObjectMeta: metav1.ObjectMeta{
			Name: bundleName,
		},
		Spec: certificatesv1alpha1.ClusterTrustBundleSpec{
			SignerName:  signerName,
			TrustBundle: trustBundle,
		},
	}
}

func (w *alphaHandlers) updateWithTrustBundle(ctbObject *certificatesv1alpha1.ClusterTrustBundle, newBundle string) *certificatesv1alpha1.ClusterTrustBundle {
	newObj := ctbObject.DeepCopy()
	newObj.Spec.TrustBundle = newBundle
	return newObj
}

func (w *alphaHandlers) containsTrustBundle(ctbObject *certificatesv1alpha1.ClusterTrustBundle, bundle string) bool {
	return ctbObject.Spec.TrustBundle == bundle
}

func (w *alphaHandlers) getName(ctbObject *certificatesv1alpha1.ClusterTrustBundle) string {
	return ctbObject.Name
}

type caContentListener func()

func (f caContentListener) Enqueue() {
	f()
}

// NewBetaClusterTrustBundlePublisher sets up a ClusterTrustBundlePublisher for the
// v1beta1 API
func NewBetaClusterTrustBundlePublisher(
	signerName string,
	caProvider dynamiccertificates.CAContentProvider,
	kubeClient clientset.Interface,
) (
	PublisherRunner,
	error,
) {

	ctbInformer := certbeta1informers.NewFilteredClusterTrustBundleInformer(kubeClient, 0, cache.Indexers{},
		func(options *metav1.ListOptions) {
			options.FieldSelector = fields.OneTermEqualSelector("spec.signerName", signerName).String()
		})

	return newClusterTrustBundlePublisher(
		signerName,
		caProvider,
		kubeClient.CertificatesV1beta1().ClusterTrustBundles(),
		ctbInformer,
		certbetav1listers.NewClusterTrustBundleLister(ctbInformer.GetIndexer()),
		&betaHandlers{},
	)
}

// NewAlphaClusterTrustBundlePublisher sets up a ClusterTrustBundlePublisher for the
// v1alpha1 API
func NewAlphaClusterTrustBundlePublisher(
	signerName string,
	caProvider dynamiccertificates.CAContentProvider,
	kubeClient clientset.Interface,
) (
	PublisherRunner,
	error,
) {

	ctbInformer := certalpha1informers.NewFilteredClusterTrustBundleInformer(kubeClient, 0, cache.Indexers{},
		func(options *metav1.ListOptions) {
			options.FieldSelector = fields.OneTermEqualSelector("spec.signerName", signerName).String()
		})

	return newClusterTrustBundlePublisher(
		signerName,
		caProvider,
		kubeClient.CertificatesV1alpha1().ClusterTrustBundles(),
		ctbInformer,
		certalphav1listers.NewClusterTrustBundleLister(ctbInformer.GetIndexer()),
		&alphaHandlers{},
	)
}

// NewClusterTrustBundlePublisher creates and maintains a cluster trust bundle object
// for a signer named `signerName`. The cluster trust bundle object contains the
// CA from the `caProvider` in its .spec.TrustBundle.
func newClusterTrustBundlePublisher[T clusterTrustBundle](
	signerName string,
	caProvider dynamiccertificates.CAContentProvider,
	bundleClient clusterTrustBundlesClient[T],
	ctbInformer cache.SharedIndexInformer,
	ctbLister clusterTrustBundlesLister[T],
	handlers clusterTrustBundleHandlers[T],
) (PublisherRunner, error) {
	if len(signerName) == 0 {
		return nil, fmt.Errorf("signerName cannot be empty")
	}

	p := &ClusterTrustBundlePublisher[T]{
		signerName: signerName,
		ca:         caProvider,
		client:     bundleClient,

		ctbInformer:     ctbInformer,
		ctbLister:       ctbLister,
		ctbListerSynced: ctbInformer.HasSynced,

		handlers: handlers,

		queue: workqueue.NewTypedRateLimitingQueueWithConfig(
			workqueue.DefaultTypedControllerRateLimiter[string](),
			workqueue.TypedRateLimitingQueueConfig[string]{
				Name: "ca_cert_publisher_cluster_trust_bundles",
			},
		),
	}

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

func (p *ClusterTrustBundlePublisher[T]) caContentChangedListener() dynamiccertificates.Listener {
	return caContentListener(func() {
		p.queue.Add("")
	})
}

func (p *ClusterTrustBundlePublisher[T]) Run(ctx context.Context) {
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

func (p *ClusterTrustBundlePublisher[T]) runWorker() func(context.Context) {
	return func(ctx context.Context) {
		for p.processNextWorkItem(ctx) {
		}
	}
}

// processNextWorkItem deals with one key off the queue. It returns false when
// it's time to quit.
func (p *ClusterTrustBundlePublisher[T]) processNextWorkItem(ctx context.Context) bool {
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

func (p *ClusterTrustBundlePublisher[T]) syncClusterTrustBundle(ctx context.Context) (err error) {
	startTime := time.Now()
	defer func() {
		recordMetrics(startTime, err)
		klog.FromContext(ctx).V(4).Info("Finished syncing ClusterTrustBundle", "signerName", p.signerName, "elapsedTime", time.Since(startTime))
	}()

	caBundle := string(p.ca.CurrentCABundleContent())
	bundleName := constructBundleName(p.signerName, []byte(caBundle))

	bundle, err := p.ctbLister.Get(bundleName)
	if apierrors.IsNotFound(err) {
		_, err = p.client.Create(ctx, p.handlers.createClusterTrustBundle(bundleName, p.signerName, caBundle), metav1.CreateOptions{})
	} else if err == nil && !p.handlers.containsTrustBundle(bundle, caBundle) {
		updatedBundle := p.handlers.updateWithTrustBundle(bundle, caBundle)
		_, err = p.client.Update(ctx, updatedBundle, metav1.UpdateOptions{})
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
		if p.handlers.getName(bundleObject) == bundleName {
			continue
		}

		deleteName := p.handlers.getName(bundleObject)
		if err := p.client.Delete(ctx, deleteName, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			klog.FromContext(ctx).Error(err, "failed to remove a cluster trust bundle", "bundleName", deleteName)
			deletionError = err
		}
	}

	return deletionError
}

func constructBundleName(signerName string, bundleBytes []byte) string {
	namePrefix := strings.ReplaceAll(signerName, "/", ":") + ":"
	bundleHash := sha256.Sum256(bundleBytes)
	return fmt.Sprintf("%s%x", namePrefix, bundleHash[:12])

}
