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

package v1alpha1

import (
	"context"
	time "time"

	storagemigrationv1alpha1 "k8s.io/api/storagemigration/v1alpha1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	runtime "k8s.io/apimachinery/pkg/runtime"
	schema "k8s.io/apimachinery/pkg/runtime/schema"
	watch "k8s.io/apimachinery/pkg/watch"
	internalinterfaces "k8s.io/client-go/informers/internalinterfaces"
	kubernetes "k8s.io/client-go/kubernetes"
	v1alpha1 "k8s.io/client-go/listers/storagemigration/v1alpha1"
	cache "k8s.io/client-go/tools/cache"
)

// StorageVersionMigrationInformer provides access to a shared informer and lister for
// StorageVersionMigrations.
type StorageVersionMigrationInformer interface {
	Informer() cache.SharedIndexInformer
	Lister() v1alpha1.StorageVersionMigrationLister
}

type storageVersionMigrationInformer struct {
	factory          internalinterfaces.SharedInformerFactory
	tweakListOptions internalinterfaces.TweakListOptionsFunc
}

// NewStorageVersionMigrationInformer constructs a new informer for StorageVersionMigration type.
// Always prefer using an informer factory to get a shared informer instead of getting an independent
// one. This reduces memory footprint and number of connections to the server.
func NewStorageVersionMigrationInformer(client kubernetes.Interface, resyncPeriod time.Duration, indexers cache.Indexers) cache.SharedIndexInformer {
	return NewFilteredStorageVersionMigrationInformer(client, resyncPeriod, indexers, nil)
}

// NewFilteredStorageVersionMigrationInformer constructs a new informer for StorageVersionMigration type.
// Always prefer using an informer factory to get a shared informer instead of getting an independent
// one. This reduces memory footprint and number of connections to the server.
func NewFilteredStorageVersionMigrationInformer(client kubernetes.Interface, resyncPeriod time.Duration, indexers cache.Indexers, tweakListOptions internalinterfaces.TweakListOptionsFunc) cache.SharedIndexInformer {
	return cache.NewSharedIndexInformerWithOptions(
		&cache.ListWatch{
			ListFunc: func(options v1.ListOptions) (runtime.Object, error) {
				if tweakListOptions != nil {
					tweakListOptions(&options)
				}
				return client.StoragemigrationV1alpha1().StorageVersionMigrations().List(context.TODO(), options)
			},
			WatchFunc: func(options v1.ListOptions) (watch.Interface, error) {
				if tweakListOptions != nil {
					tweakListOptions(&options)
				}
				return client.StoragemigrationV1alpha1().StorageVersionMigrations().Watch(context.TODO(), options)
			},
		},
		&storagemigrationv1alpha1.StorageVersionMigration{},
		cache.SharedIndexInformerOptions{
			ResyncPeriod:         resyncPeriod,
			Indexers:             indexers,
			GroupVersionResource: schema.GroupVersionResource{Group: "storagemigration.k8s.io", Version: "v1alpha1", Resource: "storageversionmigrations"},
		},
	)
}

func (f *storageVersionMigrationInformer) defaultInformer(client kubernetes.Interface, resyncPeriod time.Duration) cache.SharedIndexInformer {
	return NewFilteredStorageVersionMigrationInformer(client, resyncPeriod, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc}, f.tweakListOptions)
}

func (f *storageVersionMigrationInformer) Informer() cache.SharedIndexInformer {
	return f.factory.InformerFor(&storagemigrationv1alpha1.StorageVersionMigration{}, f.defaultInformer)
}

func (f *storageVersionMigrationInformer) Lister() v1alpha1.StorageVersionMigrationLister {
	return v1alpha1.NewStorageVersionMigrationLister(f.Informer().GetIndexer())
}
