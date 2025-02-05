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

package resourcequota

import (
	"context"
	"fmt"
	"time"

	"golang.org/x/sync/singleflight"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apiserver/pkg/storage"
	"k8s.io/client-go/kubernetes"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/utils/lru"
)

// QuotaAccessor abstracts the get/set logic from the rest of the Evaluator.  This could be a test stub, a straight passthrough,
// or most commonly a series of deconflicting caches.
type QuotaAccessor interface {
	// UpdateQuotaStatus is called to persist final status.  This method should write to persistent storage.
	// An error indicates that write didn't complete successfully.
	UpdateQuotaStatus(newQuota *corev1.ResourceQuota) error

	// GetQuotas gets all possible quotas for a given namespace
	GetQuotas(namespace string) ([]corev1.ResourceQuota, error)
}

type quotaAccessor struct {
	client kubernetes.Interface

	// lister can list/get quota objects from a shared informer's cache
	lister corev1listers.ResourceQuotaLister

	// liveLookups holds the last few live lookups we've done to help amortize cost on repeated lookup failures.
	// This lets us handle the case of latent caches, by looking up actual results for a namespace on cache miss/no results.
	// We track the lookup result here so that for repeated requests, we don't look it up very often.
	liveLookupCache *lru.Cache
	group           singleflight.Group
	liveTTL         time.Duration
	// updatedQuotas holds a cache of quotas that we've updated.  This is used to pull the "really latest" during back to
	// back quota evaluations that touch the same quota doc.  This only works because we can compare etcd resourceVersions
	// for the same resource as integers.  Before this change: 22 updates with 12 conflicts.  after this change: 15 updates with 0 conflicts
	updatedQuotas *lru.Cache
}

// newQuotaAccessor creates an object that conforms to the QuotaAccessor interface to be used to retrieve quota objects.
func newQuotaAccessor() (*quotaAccessor, error) {
	liveLookupCache := lru.New(100)
	updatedCache := lru.New(100)

	// client and lister will be set when SetInternalKubeClientSet and SetInternalKubeInformerFactory are invoked
	return &quotaAccessor{
		liveLookupCache: liveLookupCache,
		liveTTL:         time.Duration(30 * time.Second),
		updatedQuotas:   updatedCache,
	}, nil
}

func (e *quotaAccessor) UpdateQuotaStatus(newQuota *corev1.ResourceQuota) error {
	updatedQuota, err := e.client.CoreV1().ResourceQuotas(newQuota.Namespace).UpdateStatus(context.TODO(), newQuota, metav1.UpdateOptions{})
	if err != nil {
		return err
	}

	key := newQuota.Namespace + "/" + newQuota.Name
	e.updatedQuotas.Add(key, updatedQuota)
	return nil
}

var etcdVersioner = storage.APIObjectVersioner{}

// checkCache compares the passed quota against the value in the look-aside cache and returns the newer
// if the cache is out of date, it deletes the stale entry.  This only works because of etcd resourceVersions
// being monotonically increasing integers
func (e *quotaAccessor) checkCache(quota *corev1.ResourceQuota) *corev1.ResourceQuota {
	key := quota.Namespace + "/" + quota.Name
	uncastCachedQuota, ok := e.updatedQuotas.Get(key)
	if !ok {
		return quota
	}
	cachedQuota := uncastCachedQuota.(*corev1.ResourceQuota)

	if etcdVersioner.CompareResourceVersion(quota, cachedQuota) >= 0 {
		e.updatedQuotas.Remove(key)
		return quota
	}
	return cachedQuota
}

func (e *quotaAccessor) GetQuotas(namespace string) ([]corev1.ResourceQuota, error) {
	// determine if there are any quotas in this namespace
	// if there are no quotas, we don't need to do anything
	items, err := e.lister.ResourceQuotas(namespace).List(labels.Everything())
	if err != nil {
		return nil, fmt.Errorf("error resolving quota: %v", err)
	}

	// if there are no items held in our indexer, check our live-lookup LRU, if that misses, do the live lookup to prime it.
	if len(items) == 0 {
		lruItemObj, ok := e.liveLookupCache.Get(namespace)
		if !ok || lruItemObj.(liveLookupEntry).expiry.Before(time.Now()) {
			// use singleflight.Group to avoid flooding the apiserver with repeated
			// requests. See #22422 for details.
			lruItemObj, err, _ = e.group.Do(namespace, func() (interface{}, error) {
				liveList, err := e.client.CoreV1().ResourceQuotas(namespace).List(context.TODO(), metav1.ListOptions{})
				if err != nil {
					return nil, err
				}
				newEntry := liveLookupEntry{expiry: time.Now().Add(e.liveTTL)}
				for i := range liveList.Items {
					newEntry.items = append(newEntry.items, &liveList.Items[i])
				}
				e.liveLookupCache.Add(namespace, newEntry)
				return newEntry, nil
			})
			if err != nil {
				return nil, err
			}
		}
		lruEntry := lruItemObj.(liveLookupEntry)
		for i := range lruEntry.items {
			items = append(items, lruEntry.items[i])
		}
	}

	resourceQuotas := []corev1.ResourceQuota{}
	for i := range items {
		quota := items[i]
		quota = e.checkCache(quota)
		// always make a copy.  We're going to muck around with this and we should never mutate the originals
		resourceQuotas = append(resourceQuotas, *quota)
	}

	return resourceQuotas, nil
}
