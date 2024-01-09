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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	runtime "k8s.io/apimachinery/pkg/runtime"
	watch "k8s.io/apimachinery/pkg/watch"
	cache "k8s.io/client-go/tools/cache"
	examplev1 "k8s.io/code-generator/examples/crd/apis/example/v1"
	versioned "k8s.io/code-generator/examples/crd/clientset/versioned"
	internalinterfaces "k8s.io/code-generator/examples/crd/informers/externalversions/internalinterfaces"
	v1 "k8s.io/code-generator/examples/crd/listers/example/v1"
)

// ClusterTestTypeInformer provides access to a shared informer and lister for
// ClusterTestTypes.
type ClusterTestTypeInformer interface {
	Informer() cache.SharedIndexInformer
	Lister() v1.ClusterTestTypeLister
}

type clusterTestTypeInformer struct {
	factory          internalinterfaces.SharedInformerFactory
	tweakListOptions internalinterfaces.TweakListOptionsFunc
}

// NewClusterTestTypeInformer constructs a new informer for ClusterTestType type.
// Always prefer using an informer factory to get a shared informer instead of getting an independent
// one. This reduces memory footprint and number of connections to the server.
func NewClusterTestTypeInformer(client versioned.Interface, resyncPeriod time.Duration, indexers cache.Indexers) cache.SharedIndexInformer {
	return NewFilteredClusterTestTypeInformer(client, resyncPeriod, indexers, nil)
}

// NewFilteredClusterTestTypeInformer constructs a new informer for ClusterTestType type.
// Always prefer using an informer factory to get a shared informer instead of getting an independent
// one. This reduces memory footprint and number of connections to the server.
func NewFilteredClusterTestTypeInformer(client versioned.Interface, resyncPeriod time.Duration, indexers cache.Indexers, tweakListOptions internalinterfaces.TweakListOptionsFunc) cache.SharedIndexInformer {
	return NewFilteredNamedClusterTestTypeInformer(client, resyncPeriod, indexers, tweakListOptions, "")
}

// NewFilteredNamedClusterTestTypeInformer constructs a new informer for ClusterTestType type.
// Always prefer using an informer factory to get a shared informer instead of getting an independent
// one. This reduces memory footprint and number of connections to the server.
func NewFilteredNamedClusterTestTypeInformer(client versioned.Interface, resyncPeriod time.Duration, indexers cache.Indexers, tweakListOptions internalinterfaces.TweakListOptionsFunc, informerName string) cache.SharedIndexInformer {
	return cache.NewSharedIndexInformerWithOptions(
		&cache.ListWatch{
			ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
				if tweakListOptions != nil {
					tweakListOptions(&options)
				}
				return client.ExampleV1().ClusterTestTypes().List(context.TODO(), options)
			},
			WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
				if tweakListOptions != nil {
					tweakListOptions(&options)
				}
				return client.ExampleV1().ClusterTestTypes().Watch(context.TODO(), options)
			},
		},
		&examplev1.ClusterTestType{},
		cache.SharedIndexInformerOptions{
			ResyncPeriod: resyncPeriod,
			Indexers:     indexers,
			InformerName: informerName,
		},
	)
}

func (f *clusterTestTypeInformer) defaultInformer(client versioned.Interface, resyncPeriod time.Duration) cache.SharedIndexInformer {
	informerName := f.factory.Name()
	if informerName != "" {
		informerName = informerName + ":k8s.io/code-generator/examples/crd/apis/example/v1.ClusterTestType"
	}
	return NewFilteredNamedClusterTestTypeInformer(client, resyncPeriod, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc}, f.tweakListOptions, informerName)
}

func (f *clusterTestTypeInformer) Informer() cache.SharedIndexInformer {
	return f.factory.InformerFor(&examplev1.ClusterTestType{}, f.defaultInformer)
}

func (f *clusterTestTypeInformer) Lister() v1.ClusterTestTypeLister {
	return v1.NewClusterTestTypeLister(f.Informer().GetIndexer())
}
