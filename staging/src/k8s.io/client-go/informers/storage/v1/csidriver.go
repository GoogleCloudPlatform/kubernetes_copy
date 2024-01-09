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
	"context"
	time "time"

	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	runtime "k8s.io/apimachinery/pkg/runtime"
	watch "k8s.io/apimachinery/pkg/watch"
	internalinterfaces "k8s.io/client-go/informers/internalinterfaces"
	kubernetes "k8s.io/client-go/kubernetes"
	v1 "k8s.io/client-go/listers/storage/v1"
	cache "k8s.io/client-go/tools/cache"
)

// CSIDriverInformer provides access to a shared informer and lister for
// CSIDrivers.
type CSIDriverInformer interface {
	Informer() cache.SharedIndexInformer
	Lister() v1.CSIDriverLister
}

type cSIDriverInformer struct {
	factory          internalinterfaces.SharedInformerFactory
	tweakListOptions internalinterfaces.TweakListOptionsFunc
}

// NewCSIDriverInformer constructs a new informer for CSIDriver type.
// Always prefer using an informer factory to get a shared informer instead of getting an independent
// one. This reduces memory footprint and number of connections to the server.
func NewCSIDriverInformer(client kubernetes.Interface, resyncPeriod time.Duration, indexers cache.Indexers) cache.SharedIndexInformer {
	return NewFilteredCSIDriverInformer(client, resyncPeriod, indexers, nil)
}

// NewFilteredCSIDriverInformer constructs a new informer for CSIDriver type.
// Always prefer using an informer factory to get a shared informer instead of getting an independent
// one. This reduces memory footprint and number of connections to the server.
func NewFilteredCSIDriverInformer(client kubernetes.Interface, resyncPeriod time.Duration, indexers cache.Indexers, tweakListOptions internalinterfaces.TweakListOptionsFunc) cache.SharedIndexInformer {
	return NewFilteredNamedCSIDriverInformer(client, resyncPeriod, indexers, tweakListOptions, "")
}

// NewFilteredNamedCSIDriverInformer constructs a new informer for CSIDriver type.
// Always prefer using an informer factory to get a shared informer instead of getting an independent
// one. This reduces memory footprint and number of connections to the server.
func NewFilteredNamedCSIDriverInformer(client kubernetes.Interface, resyncPeriod time.Duration, indexers cache.Indexers, tweakListOptions internalinterfaces.TweakListOptionsFunc, informerName string) cache.SharedIndexInformer {
	return cache.NewSharedIndexInformerWithOptions(
		&cache.ListWatch{
			ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
				if tweakListOptions != nil {
					tweakListOptions(&options)
				}
				return client.StorageV1().CSIDrivers().List(context.TODO(), options)
			},
			WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
				if tweakListOptions != nil {
					tweakListOptions(&options)
				}
				return client.StorageV1().CSIDrivers().Watch(context.TODO(), options)
			},
		},
		&storagev1.CSIDriver{},
		cache.SharedIndexInformerOptions{
			ResyncPeriod: resyncPeriod,
			Indexers:     indexers,
			InformerName: informerName,
		},
	)
}

func (f *cSIDriverInformer) defaultInformer(client kubernetes.Interface, resyncPeriod time.Duration) cache.SharedIndexInformer {
	informerName := f.factory.Name()
	if informerName != "" {
		informerName = informerName + ":k8s.io/api/storage/v1.CSIDriver"
	}
	return NewFilteredNamedCSIDriverInformer(client, resyncPeriod, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc}, f.tweakListOptions, informerName)
}

func (f *cSIDriverInformer) Informer() cache.SharedIndexInformer {
	return f.factory.InformerFor(&storagev1.CSIDriver{}, f.defaultInformer)
}

func (f *cSIDriverInformer) Lister() v1.CSIDriverLister {
	return v1.NewCSIDriverLister(f.Informer().GetIndexer())
}
