/*
Copyright The Kubernetes Authors.

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

// Code generated by informer-gen. DO NOT EDIT.

package v1

import (
	context "context"
	time "time"

	apistoragev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	runtime "k8s.io/apimachinery/pkg/runtime"
	watch "k8s.io/apimachinery/pkg/watch"
	internalinterfaces "k8s.io/client-go/informers/internalinterfaces"
	kubernetes "k8s.io/client-go/kubernetes"
	storagev1 "k8s.io/client-go/listers/storage/v1"
	cache "k8s.io/client-go/tools/cache"
)

// CSIStorageCapacityInformer provides access to a shared informer and lister for
// CSIStorageCapacities.
type CSIStorageCapacityInformer interface {
	Informer() cache.SharedIndexInformer
	Lister() storagev1.CSIStorageCapacityLister
}

type cSIStorageCapacityInformer struct {
	factory          internalinterfaces.SharedInformerFactory
	tweakListOptions internalinterfaces.TweakListOptionsFunc
	namespace        string
}

// NewCSIStorageCapacityInformer constructs a new informer for CSIStorageCapacity type.
// Always prefer using an informer factory to get a shared informer instead of getting an independent
// one. This reduces memory footprint and number of connections to the server.
func NewCSIStorageCapacityInformer(client kubernetes.Interface, namespace string, resyncPeriod time.Duration, indexers cache.Indexers) cache.SharedIndexInformer {
	return NewFilteredCSIStorageCapacityInformer(client, namespace, resyncPeriod, indexers, nil)
}

// NewFilteredCSIStorageCapacityInformer constructs a new informer for CSIStorageCapacity type.
// Always prefer using an informer factory to get a shared informer instead of getting an independent
// one. This reduces memory footprint and number of connections to the server.
func NewFilteredCSIStorageCapacityInformer(client kubernetes.Interface, namespace string, resyncPeriod time.Duration, indexers cache.Indexers, tweakListOptions internalinterfaces.TweakListOptionsFunc) cache.SharedIndexInformer {
	return cache.NewSharedIndexInformer(
		&cache.ListWatch{
			ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
				if tweakListOptions != nil {
					tweakListOptions(&options)
				}
				return client.StorageV1().CSIStorageCapacities(namespace).List(context.Background(), options)
			},
			WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
				if tweakListOptions != nil {
					tweakListOptions(&options)
				}
				return client.StorageV1().CSIStorageCapacities(namespace).Watch(context.Background(), options)
			},
			ListFuncWithContext: func(ctx context.Context, options metav1.ListOptions) (runtime.Object, error) {
				if tweakListOptions != nil {
					tweakListOptions(&options)
				}
				return client.StorageV1().CSIStorageCapacities(namespace).List(ctx, options)
			},
			WatchFuncWithContext: func(ctx context.Context, options metav1.ListOptions) (watch.Interface, error) {
				if tweakListOptions != nil {
					tweakListOptions(&options)
				}
				return client.StorageV1().CSIStorageCapacities(namespace).Watch(ctx, options)
			},
		},
		&apistoragev1.CSIStorageCapacity{},
		resyncPeriod,
		indexers,
	)
}

func (f *cSIStorageCapacityInformer) defaultInformer(client kubernetes.Interface, resyncPeriod time.Duration) cache.SharedIndexInformer {
	return NewFilteredCSIStorageCapacityInformer(client, f.namespace, resyncPeriod, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc}, f.tweakListOptions)
}

func (f *cSIStorageCapacityInformer) Informer() cache.SharedIndexInformer {
	return f.factory.InformerFor(&apistoragev1.CSIStorageCapacity{}, f.defaultInformer)
}

func (f *cSIStorageCapacityInformer) Lister() storagev1.CSIStorageCapacityLister {
	return storagev1.NewCSIStorageCapacityLister(f.Informer().GetIndexer())
}
