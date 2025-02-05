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
	"reflect"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
)

// cluster scoped resources don't have namespaces.  Default to the item's namespace, but clear it for cluster scoped resources
func resourceDefaultNamespace(namespaced bool, defaultNamespace string) string {
	if namespaced {
		return defaultNamespace
	}
	return ""
}

// apiResource consults the REST mapper to translate an <apiVersion, kind,
// namespace> tuple to a unversioned.APIResource struct.
func (gc *GarbageCollector) apiResource(apiVersion, kind string) (schema.GroupVersionResource, bool, error) {
	fqKind := schema.FromAPIVersionAndKind(apiVersion, kind)
	mapping, err := gc.restMapper.RESTMapping(fqKind.GroupKind(), fqKind.Version)
	if err != nil {
		return schema.GroupVersionResource{}, false, newRESTMappingError(kind, apiVersion)
	}
	return mapping.Resource, mapping.Scope == meta.RESTScopeNamespace, nil
}

func (gc *GarbageCollector) deleteObject(item objectReference, resourceVersion string, ownersAtResourceVersion []metav1.OwnerReference, policy *metav1.DeletionPropagation) error {
	resource, namespaced, err := gc.apiResource(item.APIVersion, item.Kind)
	if err != nil {
		return err
	}
	uid := item.UID
	preconditions := metav1.Preconditions{UID: &uid}
	if len(resourceVersion) > 0 {
		preconditions.ResourceVersion = &resourceVersion
	}
	deleteOptions := metav1.DeleteOptions{Preconditions: &preconditions, PropagationPolicy: policy}
	resourceClient := gc.metadataClient.Resource(resource).Namespace(resourceDefaultNamespace(namespaced, item.Namespace))
	err = resourceClient.Delete(context.TODO(), item.Name, deleteOptions)
	if errors.IsConflict(err) && len(resourceVersion) > 0 {
		// check if the ownerReferences changed
		liveObject, liveErr := resourceClient.Get(context.TODO(), item.Name, metav1.GetOptions{})
		if errors.IsNotFound(liveErr) {
			// object we wanted to delete is gone, success!
			return nil
		}
		if liveErr == nil && liveObject.UID == item.UID && liveObject.ResourceVersion != resourceVersion && reflect.DeepEqual(liveObject.OwnerReferences, ownersAtResourceVersion) {
			// object changed, causing a conflict error, but ownerReferences did not change.
			// retry delete without resourceVersion precondition so rapid updates unrelated to ownerReferences don't starve GC
			return gc.deleteObject(item, "", nil, policy)
		}
	}
	return err
}

func (gc *GarbageCollector) getObject(item objectReference) (*metav1.PartialObjectMetadata, error) {
	resource, namespaced, err := gc.apiResource(item.APIVersion, item.Kind)
	if err != nil {
		return nil, err
	}
	namespace := resourceDefaultNamespace(namespaced, item.Namespace)
	if namespaced && len(namespace) == 0 {
		// the type is namespaced, but we have no namespace coordinate.
		// the only way this can happen is if a cluster-scoped object referenced this type as an owner.
		return nil, namespacedOwnerOfClusterScopedObjectErr
	}
	return gc.metadataClient.Resource(resource).Namespace(namespace).Get(context.TODO(), item.Name, metav1.GetOptions{})
}

func (gc *GarbageCollector) patchObject(item objectReference, patch []byte, pt types.PatchType) (*metav1.PartialObjectMetadata, error) {
	resource, namespaced, err := gc.apiResource(item.APIVersion, item.Kind)
	if err != nil {
		return nil, err
	}
	return gc.metadataClient.Resource(resource).Namespace(resourceDefaultNamespace(namespaced, item.Namespace)).Patch(context.TODO(), item.Name, pt, patch, metav1.PatchOptions{})
}

func (gc *GarbageCollector) removeFinalizer(logger klog.Logger, owner *node, targetFinalizer string) error {
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		ownerObject, err := gc.getObject(owner.identity)
		if errors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("cannot finalize owner %s, because cannot get it: %v. The garbage collector will retry later", owner.identity, err)
		}
		accessor, err := meta.Accessor(ownerObject)
		if err != nil {
			return fmt.Errorf("cannot access the owner object %v: %v. The garbage collector will retry later", ownerObject, err)
		}
		finalizers := accessor.GetFinalizers()
		var newFinalizers []string
		found := false
		for _, f := range finalizers {
			if f == targetFinalizer {
				found = true
				continue
			}
			newFinalizers = append(newFinalizers, f)
		}
		if !found {
			logger.V(5).Info("finalizer already removed from object", "finalizer", targetFinalizer, "object", owner.identity)
			return nil
		}

		// remove the owner from dependent's OwnerReferences
		patch, err := json.Marshal(&objectForFinalizersPatch{
			ObjectMetaForFinalizersPatch: ObjectMetaForFinalizersPatch{
				ResourceVersion: accessor.GetResourceVersion(),
				Finalizers:      newFinalizers,
			},
		})
		if err != nil {
			return fmt.Errorf("unable to finalize %s due to an error serializing patch: %v", owner.identity, err)
		}
		_, err = gc.patchObject(owner.identity, patch, types.MergePatchType)
		return err
	})
	if errors.IsConflict(err) {
		return fmt.Errorf("updateMaxRetries(%d) has reached. The garbage collector will retry later for owner %v", retry.DefaultBackoff.Steps, owner.identity)
	}
	return err
}
