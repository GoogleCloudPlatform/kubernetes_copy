/*
Copyright 2022 The Kubernetes Authors.

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

package internal

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/structured-merge-diff/v4/fieldpath"
)

// Managed groups a fieldpath.ManagedFields together with the timestamps associated with each operation.
type Managed interface {
	// Fields gets the fieldpath.ManagedFields.
	Fields() fieldpath.ManagedFields

	// Times gets the timestamps associated with each operation.
	Times() map[string]*metav1.Time
}

// Manager updates the managed fields and merges applied configurations.
type Manager interface {
	// Update is used when the object has already been merged (non-apply
	// use-case), and simply updates the managed fields in the output
	// object.
	//  * `liveObj` is not mutated by this function
	//  * `newObj` may be mutated by this function
	// Returns the new object with managedFields removed, and the object's new
	// proposed managedFields separately.
	Update(liveObj, newObj runtime.Object, managed Managed, manager string) (runtime.Object, Managed, error)

	// Apply is used when server-side apply is called, as it merges the
	// object and updates the managed fields.
	//  * `liveObj` is not mutated by this function
	//  * `appliedObj` may be mutated by this function
	// Returns the new object with managedFields removed, and the object's new
	// proposed managedFields separately.
	Apply(liveObj, appliedObj runtime.Object, managed Managed, fieldManager string, force bool) (runtime.Object, Managed, error)
}
