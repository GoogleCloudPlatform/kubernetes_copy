/*
Copyright 2017 The Kubernetes Authors.

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

package rest

import (
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/uuid"
)

// metav1Now returns metav1.Now(), but allows override for unit testing
var metav1Now = func() metav1.Time { return metav1.Now() }

// WipeObjectMetaSystemFields erases fields that are managed by the system on ObjectMeta.
func WipeObjectMetaSystemFields(meta metav1.Object) {
	meta.SetCreationTimestamp(metav1.Time{})
	meta.SetUID("")
	meta.SetDeletionTimestamp(nil)
	meta.SetDeletionGracePeriodSeconds(nil)
	meta.SetSelfLink("")
}

// FillObjectMetaSystemFields populates fields that are managed by the system on ObjectMeta.
func FillObjectMetaSystemFields(meta metav1.Object) {
	meta.SetCreationTimestamp(metav1Now())
	meta.SetUID(uuid.NewUUID())
}

// EnsureObjectNamespaceMatchesRequestNamespace returns an error if obj.Namespace and requestNamespace
// are both populated and do not match. If either is unpopulated, it modifies obj as needed to ensure
// obj.GetNamespace() == requestNamespace.
func EnsureObjectNamespaceMatchesRequestNamespace(requestNamespace string, obj metav1.Object) error {
	objNamespace := obj.GetNamespace()
	switch {
	case objNamespace == requestNamespace:
		// already matches, no-op
		return nil

	case objNamespace == metav1.NamespaceNone:
		// unset, default to request namespace
		obj.SetNamespace(requestNamespace)
		return nil

	case requestNamespace == metav1.NamespaceNone:
		// cluster-scoped, clear namespace
		obj.SetNamespace(metav1.NamespaceNone)
		return nil

	default:
		// mismatch, error
		return errors.NewBadRequest("the namespace of the provided object does not match the namespace sent on the request")
	}
}

// ExpectedNamespaceForScope returns the expected namespace for a resource, given the request namespace and resource scope.
func ExpectedNamespaceForScope(requestNamespace string, namespaceScoped bool) string {
	if namespaceScoped {
		return requestNamespace
	}
	return ""
}

// ExpectedNamespaceForResource returns the expected namespace for a resource, given the request namespace.
func ExpectedNamespaceForResource(requestNamespace string, resource schema.GroupVersionResource) string {
	if resource.Resource == "namespaces" && resource.Group == "" {
		return ""
	}
	return requestNamespace
}
