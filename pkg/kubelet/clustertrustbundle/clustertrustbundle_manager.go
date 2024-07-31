/*
Copyright 2023 The Kubernetes Authors.

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

// Package clustertrustbundle abstracts access to ClusterTrustBundles so that
// projected volumes can use them.
package clustertrustbundle

import (
	"context"
	"encoding/pem"
	"fmt"
	"math/rand"
	"time"

	certificatesv1beta1 "k8s.io/api/certificates/v1beta1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	lrucache "k8s.io/apimachinery/pkg/util/cache"
	"k8s.io/apimachinery/pkg/util/sets"
	certinformersv1beta1 "k8s.io/client-go/informers/certificates/v1beta1"
	certlistersv1beta1 "k8s.io/client-go/listers/certificates/v1beta1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

const (
	maxLabelSelectorLength = 100 * 1024
)

// Manager abstracts over the ability to get trust anchors.
type Manager interface {
	GetTrustAnchorsByName(name string, allowMissing bool) ([]byte, error)
	GetTrustAnchorsBySigner(signerName string, labelSelector *metav1.LabelSelector, allowMissing bool) ([]byte, error)
}

// InformerManager is the "real" manager.  It uses informers to track
// ClusterTrustBundle objects.
type InformerManager struct {
	ctbInformer cache.SharedIndexInformer
	ctbLister   certlistersv1beta1.ClusterTrustBundleLister

	normalizationCache *lrucache.LRUExpireCache
	cacheTTL           time.Duration
}

var _ Manager = (*InformerManager)(nil)

// NewInformerManager returns an initialized InformerManager.
func NewInformerManager(ctx context.Context, bundles certinformersv1beta1.ClusterTrustBundleInformer, cacheSize int, cacheTTL time.Duration) (*InformerManager, error) {
	// We need to call Informer() before calling start on the shared informer
	// factory, or the informer won't be registered to be started.
	m := &InformerManager{
		ctbInformer:        bundles.Informer(),
		ctbLister:          bundles.Lister(),
		normalizationCache: lrucache.NewLRUExpireCache(cacheSize),
		cacheTTL:           cacheTTL,
	}

	logger := klog.FromContext(ctx)
	// Have the informer bust cache entries when it sees updates that could
	// apply to them.
	_, err := m.ctbInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			ctb, ok := obj.(*certificatesv1beta1.ClusterTrustBundle)
			if !ok {
				return
			}
			logger.Info("Dropping all cache entries for signer", "signerName", ctb.Spec.SignerName)
			m.dropCacheFor(ctb)
		},
		UpdateFunc: func(old, new any) {
			ctb, ok := new.(*certificatesv1beta1.ClusterTrustBundle)
			if !ok {
				return
			}
			logger.Info("Dropping cache for ClusterTrustBundle", "signerName", ctb.Spec.SignerName)
			m.dropCacheFor(new.(*certificatesv1beta1.ClusterTrustBundle))
		},
		DeleteFunc: func(obj any) {
			ctb, ok := obj.(*certificatesv1beta1.ClusterTrustBundle)
			if !ok {
				tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
				if !ok {
					return
				}
				ctb, ok = tombstone.Obj.(*certificatesv1beta1.ClusterTrustBundle)
				if !ok {
					return
				}
			}
			logger.Info("Dropping cache for ClusterTrustBundle", "signerName", ctb.Spec.SignerName)
			m.dropCacheFor(ctb)
		},
	})
	if err != nil {
		return nil, fmt.Errorf("while registering event handler on informer: %w", err)
	}

	return m, nil
}

func (m *InformerManager) dropCacheFor(ctb *certificatesv1beta1.ClusterTrustBundle) {
	if ctb.Spec.SignerName != "" {
		m.normalizationCache.RemoveAll(func(key any) bool {
			return key.(cacheKeyType).signerName == ctb.Spec.SignerName
		})
	} else {
		m.normalizationCache.RemoveAll(func(key any) bool {
			return key.(cacheKeyType).ctbName == ctb.ObjectMeta.Name
		})
	}
}

// GetTrustAnchorsByName returns normalized and deduplicated trust anchors from
// a single named ClusterTrustBundle.
func (m *InformerManager) GetTrustAnchorsByName(name string, allowMissing bool) ([]byte, error) {
	if !m.ctbInformer.HasSynced() {
		return nil, fmt.Errorf("ClusterTrustBundle informer has not yet synced")
	}

	cacheKey := cacheKeyType{ctbName: name}

	if cachedAnchors, ok := m.normalizationCache.Get(cacheKey); ok {
		return cachedAnchors.([]byte), nil
	}

	ctb, err := m.ctbLister.Get(name)
	if k8serrors.IsNotFound(err) && allowMissing {
		return []byte{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("while getting ClusterTrustBundle: %w", err)
	}

	pemTrustAnchors, err := m.normalizeTrustAnchors([]*certificatesv1beta1.ClusterTrustBundle{ctb})
	if err != nil {
		return nil, fmt.Errorf("while normalizing trust anchors: %w", err)
	}

	m.normalizationCache.Add(cacheKey, pemTrustAnchors, m.cacheTTL)

	return pemTrustAnchors, nil
}

// GetTrustAnchorsBySigner returns normalized and deduplicated trust anchors
// from a set of selected ClusterTrustBundles.
func (m *InformerManager) GetTrustAnchorsBySigner(signerName string, labelSelector *metav1.LabelSelector, allowMissing bool) ([]byte, error) {
	if !m.ctbInformer.HasSynced() {
		return nil, fmt.Errorf("ClusterTrustBundle informer has not yet synced")
	}

	// Note that this function treats nil as "match nothing", and non-nil but
	// empty as "match everything".
	selector, err := metav1.LabelSelectorAsSelector(labelSelector)
	if err != nil {
		return nil, fmt.Errorf("while parsing label selector: %w", err)
	}

	cacheKey := cacheKeyType{signerName: signerName, labelSelector: selector.String()}

	if lsLen := len(cacheKey.labelSelector); lsLen > maxLabelSelectorLength {
		return nil, fmt.Errorf("label selector length (%d) is larger than %d", lsLen, maxLabelSelectorLength)
	}

	if cachedAnchors, ok := m.normalizationCache.Get(cacheKey); ok {
		return cachedAnchors.([]byte), nil
	}

	rawCTBList, err := m.ctbLister.List(selector)
	if err != nil {
		return nil, fmt.Errorf("while listing ClusterTrustBundles matching label selector %v: %w", labelSelector, err)
	}

	ctbList := []*certificatesv1beta1.ClusterTrustBundle{}
	for _, ctb := range rawCTBList {
		if ctb.Spec.SignerName == signerName {
			ctbList = append(ctbList, ctb)
		}
	}

	if len(ctbList) == 0 {
		if allowMissing {
			return []byte{}, nil
		}
		return nil, fmt.Errorf("combination of signerName and labelSelector matched zero ClusterTrustBundles")
	}

	pemTrustAnchors, err := m.normalizeTrustAnchors(ctbList)
	if err != nil {
		return nil, fmt.Errorf("while normalizing trust anchors: %w", err)
	}

	m.normalizationCache.Add(cacheKey, pemTrustAnchors, m.cacheTTL)

	return pemTrustAnchors, nil
}

func (m *InformerManager) normalizeTrustAnchors(ctbList []*certificatesv1beta1.ClusterTrustBundle) ([]byte, error) {
	// Deduplicate trust anchors from all ClusterTrustBundles.
	trustAnchorSet := sets.Set[string]{}
	for _, ctb := range ctbList {
		rest := []byte(ctb.Spec.TrustBundle)
		var b *pem.Block
		for {
			b, rest = pem.Decode(rest)
			if b == nil {
				break
			}
			trustAnchorSet = trustAnchorSet.Insert(string(b.Bytes))
		}
	}

	// Give the list a stable ordering that changes each time Kubelet restarts.
	trustAnchorList := sets.List(trustAnchorSet)
	rand.Shuffle(len(trustAnchorList), func(i, j int) {
		trustAnchorList[i], trustAnchorList[j] = trustAnchorList[j], trustAnchorList[i]
	})

	pemTrustAnchors := []byte{}
	for _, ta := range trustAnchorList {
		b := &pem.Block{
			Type:  "CERTIFICATE",
			Bytes: []byte(ta),
		}
		pemTrustAnchors = append(pemTrustAnchors, pem.EncodeToMemory(b)...)
	}

	return pemTrustAnchors, nil
}

type cacheKeyType struct {
	ctbName       string
	signerName    string
	labelSelector string
}

// NoopManager always returns an error, for use in static kubelet mode.
type NoopManager struct{}

var _ Manager = (*NoopManager)(nil)

// GetTrustAnchorsByName implements Manager.
func (m *NoopManager) GetTrustAnchorsByName(name string, allowMissing bool) ([]byte, error) {
	return nil, fmt.Errorf("ClusterTrustBundle projection is not supported in static kubelet mode")
}

// GetTrustAnchorsBySigner implements Manager.
func (m *NoopManager) GetTrustAnchorsBySigner(signerName string, labelSelector *metav1.LabelSelector, allowMissing bool) ([]byte, error) {
	return nil, fmt.Errorf("ClusterTrustBundle projection is not supported in static kubelet mode")
}
