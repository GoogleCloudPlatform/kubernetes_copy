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
	context "context"
	time "time"

	apiadmissionregistrationv1alpha1 "k8s.io/api/admissionregistration/v1alpha1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	runtime "k8s.io/apimachinery/pkg/runtime"
	watch "k8s.io/apimachinery/pkg/watch"
	internalinterfaces "k8s.io/client-go/informers/internalinterfaces"
	kubernetes "k8s.io/client-go/kubernetes"
	admissionregistrationv1alpha1 "k8s.io/client-go/listers/admissionregistration/v1alpha1"
	cache "k8s.io/client-go/tools/cache"
)

// MutatingAdmissionPolicyBindingInformer provides access to a shared informer and lister for
// MutatingAdmissionPolicyBindings.
type MutatingAdmissionPolicyBindingInformer interface {
	Informer() cache.SharedIndexInformer
	Lister() admissionregistrationv1alpha1.MutatingAdmissionPolicyBindingLister
}

type mutatingAdmissionPolicyBindingInformer struct {
	factory          internalinterfaces.SharedInformerFactory
	tweakListOptions internalinterfaces.TweakListOptionsFunc
}

// NewMutatingAdmissionPolicyBindingInformer constructs a new informer for MutatingAdmissionPolicyBinding type.
// Always prefer using an informer factory to get a shared informer instead of getting an independent
// one. This reduces memory footprint and number of connections to the server.
func NewMutatingAdmissionPolicyBindingInformer(client kubernetes.Interface, resyncPeriod time.Duration, indexers cache.Indexers) cache.SharedIndexInformer {
	return NewFilteredMutatingAdmissionPolicyBindingInformer(client, resyncPeriod, indexers, nil)
}

// NewFilteredMutatingAdmissionPolicyBindingInformer constructs a new informer for MutatingAdmissionPolicyBinding type.
// Always prefer using an informer factory to get a shared informer instead of getting an independent
// one. This reduces memory footprint and number of connections to the server.
func NewFilteredMutatingAdmissionPolicyBindingInformer(client kubernetes.Interface, resyncPeriod time.Duration, indexers cache.Indexers, tweakListOptions internalinterfaces.TweakListOptionsFunc) cache.SharedIndexInformer {
	return cache.NewSharedIndexInformer(
		&cache.ListWatch{
			ListFunc: func(options v1.ListOptions) (runtime.Object, error) {
				if tweakListOptions != nil {
					tweakListOptions(&options)
				}
				return client.AdmissionregistrationV1alpha1().MutatingAdmissionPolicyBindings().List(context.Background(), options)
			},
			WatchFunc: func(options v1.ListOptions) (watch.Interface, error) {
				if tweakListOptions != nil {
					tweakListOptions(&options)
				}
				return client.AdmissionregistrationV1alpha1().MutatingAdmissionPolicyBindings().Watch(context.Background(), options)
			},
			ListFuncWithContext: func(ctx context.Context, options v1.ListOptions) (runtime.Object, error) {
				if tweakListOptions != nil {
					tweakListOptions(&options)
				}
				return client.AdmissionregistrationV1alpha1().MutatingAdmissionPolicyBindings().List(ctx, options)
			},
			WatchFuncWithContext: func(ctx context.Context, options v1.ListOptions) (watch.Interface, error) {
				if tweakListOptions != nil {
					tweakListOptions(&options)
				}
				return client.AdmissionregistrationV1alpha1().MutatingAdmissionPolicyBindings().Watch(ctx, options)
			},
		},
		&apiadmissionregistrationv1alpha1.MutatingAdmissionPolicyBinding{},
		resyncPeriod,
		indexers,
	)
}

func (f *mutatingAdmissionPolicyBindingInformer) defaultInformer(client kubernetes.Interface, resyncPeriod time.Duration) cache.SharedIndexInformer {
	return NewFilteredMutatingAdmissionPolicyBindingInformer(client, resyncPeriod, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc}, f.tweakListOptions)
}

func (f *mutatingAdmissionPolicyBindingInformer) Informer() cache.SharedIndexInformer {
	return f.factory.InformerFor(&apiadmissionregistrationv1alpha1.MutatingAdmissionPolicyBinding{}, f.defaultInformer)
}

func (f *mutatingAdmissionPolicyBindingInformer) Lister() admissionregistrationv1alpha1.MutatingAdmissionPolicyBindingLister {
	return admissionregistrationv1alpha1.NewMutatingAdmissionPolicyBindingLister(f.Informer().GetIndexer())
}
