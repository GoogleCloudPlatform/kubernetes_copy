/*
Copyright 2018 The Kubernetes Authors.

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

package metadatainformer

import (
	"context"
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/metadata"
	"k8s.io/client-go/metadata/metadatalister"
	"k8s.io/client-go/tools/cache"
)

// SharedInformerOption defines the functional option type for metadataSharedInformerFactory.
type SharedInformerOption func(*metadataSharedInformerFactory) *metadataSharedInformerFactory

// WithTransform sets a transform on all informers.
func WithTransform(transform cache.TransformFunc) SharedInformerOption {
	return func(factory *metadataSharedInformerFactory) *metadataSharedInformerFactory {
		factory.transform = transform
		return factory
	}
}

// NewSharedInformerFactory constructs a new instance of metadataSharedInformerFactory for all namespaces.
func NewSharedInformerFactory(client metadata.Interface, defaultResync time.Duration) SharedInformerFactory {
	return NewFilteredSharedInformerFactory(client, defaultResync, metav1.NamespaceAll, nil)
}

// NewFilteredSharedInformerFactory constructs a new instance of metadataSharedInformerFactory.
// Listers obtained via this factory will be subject to the same filters as specified here.
func NewFilteredSharedInformerFactory(client metadata.Interface, defaultResync time.Duration, namespace string, tweakListOptions TweakListOptionsFunc) SharedInformerFactory {
	return &metadataSharedInformerFactory{
		client:           client,
		defaultResync:    defaultResync,
		namespace:        namespace,
		informers:        map[schema.GroupVersionResource]informers.GenericInformer{},
		startedInformers: make(map[schema.GroupVersionResource]bool),
		tweakListOptions: tweakListOptions,
	}
}

// NewSharedInformerFactoryWithOptions constructs a new instance of metadataSharedInformerFactory with additional options.
func NewSharedInformerFactoryWithOptions(client metadata.Interface, defaultResync time.Duration, options ...SharedInformerOption) SharedInformerFactory {
	factory := &metadataSharedInformerFactory{
		client:           client,
		namespace:        v1.NamespaceAll,
		defaultResync:    defaultResync,
		informers:        map[schema.GroupVersionResource]informers.GenericInformer{},
		startedInformers: make(map[schema.GroupVersionResource]bool),
	}

	// Apply all options
	for _, opt := range options {
		factory = opt(factory)
	}

	return factory
}

type metadataSharedInformerFactory struct {
	client        metadata.Interface
	defaultResync time.Duration
	namespace     string
	transform     cache.TransformFunc

	lock      sync.Mutex
	informers map[schema.GroupVersionResource]informers.GenericInformer
	// startedInformers is used for tracking which informers have been started.
	// This allows Start() to be called multiple times safely.
	startedInformers map[schema.GroupVersionResource]bool
	tweakListOptions TweakListOptionsFunc
	// wg tracks how many goroutines were started.
	wg sync.WaitGroup
	// shuttingDown is true when Shutdown has been called. It may still be running
	// because it needs to wait for goroutines.
	shuttingDown bool
}

var _ SharedInformerFactory = &metadataSharedInformerFactory{}

func (f *metadataSharedInformerFactory) ForResource(gvr schema.GroupVersionResource) informers.GenericInformer {
	f.lock.Lock()
	defer f.lock.Unlock()

	key := gvr
	informer, exists := f.informers[key]
	if exists {
		return informer
	}

	informer = NewFilteredMetadataInformer(f.client, gvr, f.namespace, f.defaultResync, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc}, f.tweakListOptions)
	informer.Informer().SetTransform(f.transform)
	f.informers[key] = informer

	return informer
}

// Start initializes all requested informers.
func (f *metadataSharedInformerFactory) Start(stopCh <-chan struct{}) {
	f.lock.Lock()
	defer f.lock.Unlock()

	if f.shuttingDown {
		return
	}

	for informerType, informer := range f.informers {
		if !f.startedInformers[informerType] {
			f.wg.Add(1)
			// We need a new variable in each loop iteration,
			// otherwise the goroutine would use the loop variable
			// and that keeps changing.
			informer := informer.Informer()
			go func() {
				defer f.wg.Done()
				informer.Run(stopCh)
			}()
			f.startedInformers[informerType] = true
		}
	}
}

// WaitForCacheSync waits for all started informers' cache were synced.
func (f *metadataSharedInformerFactory) WaitForCacheSync(stopCh <-chan struct{}) map[schema.GroupVersionResource]bool {
	informers := func() map[schema.GroupVersionResource]cache.SharedIndexInformer {
		f.lock.Lock()
		defer f.lock.Unlock()

		informers := map[schema.GroupVersionResource]cache.SharedIndexInformer{}
		for informerType, informer := range f.informers {
			if f.startedInformers[informerType] {
				informers[informerType] = informer.Informer()
			}
		}
		return informers
	}()

	res := map[schema.GroupVersionResource]bool{}
	for informType, informer := range informers {
		res[informType] = cache.WaitForCacheSync(stopCh, informer.HasSynced)
	}
	return res
}

func (f *metadataSharedInformerFactory) Shutdown() {
	// Will return immediately if there is nothing to wait for.
	defer f.wg.Wait()

	f.lock.Lock()
	defer f.lock.Unlock()
	f.shuttingDown = true
}

// NewFilteredMetadataInformer constructs a new informer for a metadata type.
func NewFilteredMetadataInformer(client metadata.Interface, gvr schema.GroupVersionResource, namespace string, resyncPeriod time.Duration, indexers cache.Indexers, tweakListOptions TweakListOptionsFunc) informers.GenericInformer {
	return &metadataInformer{
		gvr: gvr,
		informer: cache.NewSharedIndexInformer(
			&cache.ListWatch{
				ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
					if tweakListOptions != nil {
						tweakListOptions(&options)
					}
					return client.Resource(gvr).Namespace(namespace).List(context.Background(), options)
				},
				WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
					if tweakListOptions != nil {
						tweakListOptions(&options)
					}
					return client.Resource(gvr).Namespace(namespace).Watch(context.Background(), options)
				},
				ListWithContextFunc: func(ctx context.Context, options metav1.ListOptions) (runtime.Object, error) {
					if tweakListOptions != nil {
						tweakListOptions(&options)
					}
					return client.Resource(gvr).Namespace(namespace).List(ctx, options)
				},
				WatchFuncWithContext: func(ctx context.Context, options metav1.ListOptions) (watch.Interface, error) {
					if tweakListOptions != nil {
						tweakListOptions(&options)
					}
					return client.Resource(gvr).Namespace(namespace).Watch(ctx, options)
				},
			},
			&metav1.PartialObjectMetadata{},
			resyncPeriod,
			indexers,
		),
	}
}

type metadataInformer struct {
	informer cache.SharedIndexInformer
	gvr      schema.GroupVersionResource
}

var _ informers.GenericInformer = &metadataInformer{}

func (d *metadataInformer) Informer() cache.SharedIndexInformer {
	return d.informer
}

func (d *metadataInformer) Lister() cache.GenericLister {
	return metadatalister.NewRuntimeObjectShim(metadatalister.New(d.informer.GetIndexer(), d.gvr))
}
