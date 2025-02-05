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

package garbagecollector

import (
	"context"
	"encoding/json"
	"fmt"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/kubernetes/pkg/controller/garbagecollector/metaonly"
)

// getMetadata tries getting object metadata from local cache, and sends GET request to apiserver when
// local cache is not available or not latest.
func (gc *GarbageCollector) getMetadata(item objectReference) (metav1.Object, error) {
	apiResource, namespaced, err := gc.apiResource(item.APIVersion, item.Kind)
	if err != nil {
		return nil, err
	}
	namespace := resourceDefaultNamespace(namespaced, item.Namespace)
	if namespaced && len(namespace) == 0 {
		// the type is namespaced, but we have no namespace coordinate.
		// the only way this can happen is if a cluster-scoped object referenced this type as an owner.
		return nil, namespacedOwnerOfClusterScopedObjectErr
	}

	obj, err := func() (metav1.Object, error) {
		gc.dependencyGraphBuilder.monitorLock.RLock()
		defer gc.dependencyGraphBuilder.monitorLock.RUnlock()
		m, ok := gc.dependencyGraphBuilder.monitors[apiResource]
		if !ok || m == nil {
			return nil, fmt.Errorf("cache not found or not ready for resource %v", apiResource.String())
		}
		key := item.Name
		if namespaced {
			key = namespace + "/" + item.Name
		}
		raw, exist, err := m.store.GetByKey(key)
		if err != nil {
			return nil, err
		}
		if !exist {
			return nil, fmt.Errorf("not found runtime.Object with uid: %v", item.UID)
		}
		obj, ok := raw.(runtime.Object)
		if !ok {
			return nil, fmt.Errorf("expect a runtime.Object, got %v", raw)
		}
		accessor, err := meta.Accessor(obj)
		if err != nil {
			return nil, err
		}
		if accessor.GetUID() != item.UID {
			return nil, fmt.Errorf("expect uid: %v, got uid: %v", item.UID, accessor.GetUID())
		}
		return accessor, nil
	}()
	if err != nil {
		// If local cache do not found, send a GET request to API server
		if namespaced {
			return gc.metadataClient.Resource(apiResource).Namespace(namespace).Get(context.TODO(), item.Name, metav1.GetOptions{})
		} else {
			return gc.metadataClient.Resource(apiResource).Get(context.TODO(), item.Name, metav1.GetOptions{})
		}
	}
	return obj, nil

}

type objectForFinalizersPatch struct {
	ObjectMetaForFinalizersPatch `json:"metadata"`
}

// ObjectMetaForFinalizersPatch defines object meta struct for finalizers patch operation.
type ObjectMetaForFinalizersPatch struct {
	ResourceVersion string   `json:"resourceVersion"`
	Finalizers      []string `json:"finalizers"`
}

type objectForPatch struct {
	ObjectMetaForPatch `json:"metadata"`
}

// ObjectMetaForPatch defines object meta struct for patch operation.
type ObjectMetaForPatch struct {
	ResourceVersion string                  `json:"resourceVersion"`
	OwnerReferences []metav1.OwnerReference `json:"ownerReferences"`
}

// jsonMergePatchFunc defines the interface for functions that construct json merge patches that manipulate
// owner reference array.
type jsonMergePatchFunc func(*node) ([]byte, error)

// patch tries strategic merge patch on item first, and if SMP is not supported, it fallbacks to JSON merge
// patch.
func (gc *GarbageCollector) patch(item *node, smp []byte, jmp jsonMergePatchFunc) (*metav1.PartialObjectMetadata, error) {
	smpResult, err := gc.patchObject(item.identity, smp, types.StrategicMergePatchType)
	if err == nil {
		return smpResult, nil
	}
	if !errors.IsUnsupportedMediaType(err) {
		return nil, err
	}
	// StrategicMergePatch is not supported, use JSON merge patch instead
	patch, err := jmp(item)
	if err != nil {
		return nil, err
	}
	return gc.patchObject(item.identity, patch, types.MergePatchType)
}

// Returns JSON merge patch that removes the ownerReferences matching ownerUIDs.
func (gc *GarbageCollector) deleteOwnerRefJSONMergePatch(item *node, ownerUIDs ...types.UID) ([]byte, error) {
	accessor, err := gc.getMetadata(item.identity)
	if err != nil {
		return nil, err
	}
	expectedObjectMeta := ObjectMetaForPatch{}
	expectedObjectMeta.ResourceVersion = accessor.GetResourceVersion()
	refs := accessor.GetOwnerReferences()
	for _, ref := range refs {
		var skip bool
		for _, ownerUID := range ownerUIDs {
			if ref.UID == ownerUID {
				skip = true
				break
			}
		}
		if !skip {
			expectedObjectMeta.OwnerReferences = append(expectedObjectMeta.OwnerReferences, ref)
		}
	}
	return json.Marshal(objectForPatch{expectedObjectMeta})
}

// Generate a patch that unsets the BlockOwnerDeletion field of all
// ownerReferences of node.
func (n *node) unblockOwnerReferencesStrategicMergePatch() ([]byte, error) {
	var dummy metaonly.MetadataOnlyObject
	var blockingRefs []metav1.OwnerReference
	falseVar := false
	for _, owner := range n.owners {
		if owner.BlockOwnerDeletion != nil && *owner.BlockOwnerDeletion {
			ref := owner
			ref.BlockOwnerDeletion = &falseVar
			blockingRefs = append(blockingRefs, ref)
		}
	}
	dummy.ObjectMeta.SetOwnerReferences(blockingRefs)
	dummy.ObjectMeta.UID = n.identity.UID
	return json.Marshal(dummy)
}

// Generate a JSON merge patch that unsets the BlockOwnerDeletion field of all
// ownerReferences of node.
func (gc *GarbageCollector) unblockOwnerReferencesJSONMergePatch(n *node) ([]byte, error) {
	accessor, err := gc.getMetadata(n.identity)
	if err != nil {
		return nil, err
	}
	expectedObjectMeta := ObjectMetaForPatch{}
	expectedObjectMeta.ResourceVersion = accessor.GetResourceVersion()
	var expectedOwners []metav1.OwnerReference
	falseVar := false
	for _, owner := range n.owners {
		owner.BlockOwnerDeletion = &falseVar
		expectedOwners = append(expectedOwners, owner)
	}
	expectedObjectMeta.OwnerReferences = expectedOwners
	return json.Marshal(objectForPatch{expectedObjectMeta})
}
