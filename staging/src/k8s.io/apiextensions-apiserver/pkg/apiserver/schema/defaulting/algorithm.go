/*
Copyright 2019 The Kubernetes Authors.

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

package defaulting

import (
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apimachinery/pkg/runtime"
)

// isNonNullableNull returns true if the item is nil AND it's nullable
func isNonNullableNull(x interface{}, s *structuralschema.Structural) bool {
	return x == nil && s != nil && s.Generic.Nullable == false
}

// Default does defaulting of x depending on default values in s.
// Default values from s are deep-copied.
//
// PruneNonNullableNullsWithoutDefaults has left the non-nullable nulls
// that have a default here.
func Default(x interface{}, s *structuralschema.Structural) {
	if s == nil {
		return
	}

	switch x := x.(type) {
	case map[string]interface{}:
		for k, prop := range s.Properties {
			if prop.Default.Object == nil {
				continue
			}
			if _, found := x[k]; !found || isNonNullableNull(x[k], &prop) {
				x[k] = runtime.DeepCopyJSONValue(prop.Default.Object)
			}
		}
		for k := range x {
			if prop, found := s.Properties[k]; found {
				Default(x[k], &prop)
			} else if s.AdditionalProperties != nil {
				if isNonNullableNull(x[k], s.AdditionalProperties.Structural) {
					x[k] = runtime.DeepCopyJSONValue(s.AdditionalProperties.Structural.Default.Object)
				}
				Default(x[k], s.AdditionalProperties.Structural)
			}
		}
	case []interface{}:
		for i := range x {
			if isNonNullableNull(x[i], s.Items) {
				x[i] = runtime.DeepCopyJSONValue(s.Items.Default.Object)
			}
			Default(x[i], s.Items)
		}
	default:
		// scalars, do nothing
	}
}
