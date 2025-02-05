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

package common

import (
	"reflect"
	"time"
)

// CorrelatedObject represents a node in a tree of objects that are being
// validated. It is used to keep track of the old value of an object during
// traversal of the new value. It is also used to cache the results of
// DeepEqual comparisons between the old and new values of objects.
//
// All receiver functions support being called on `nil` to support ergonomic
// recursive descent. The nil `CorrelatedObject` represents an uncorrelatable
// node in the tree.
//
// CorrelatedObject is not thread-safe. It is the responsibility of the caller
// to handle concurrency, if any.
type CorrelatedObject struct {
	// Currently correlated old value during traversal of the schema/object
	OldValue interface{}

	// Value being validated
	Value interface{}

	// Schema used for validation of this value. The schema is also used
	// to determine how to correlate the old object.
	Schema Schema

	// Duration spent on ratcheting validation for this object and all of its
	// children.
	Duration *time.Duration

	// Scratch space below, may change during validation

	// Cached comparison result of DeepEqual of `value` and `thunk.oldValue`
	comparisonResult *bool

	// Cached map representation of a map-type list, or nil if not map-type list
	mapList MapList

	// Children spawned by a call to `Validate` on this object
	// key is either a string or an index, depending upon whether `value` is
	// a map or a list, respectively.
	//
	// The list of children may be incomplete depending upon if the internal
	// logic of kube-openapi's SchemaValidator short-circuited before
	// reaching all of the children.
	//
	// It should be expected to have an entry for either all of the children, or
	// none of them.
	children map[interface{}]*CorrelatedObject
}

func NewCorrelatedObject(new, old interface{}, schema Schema) *CorrelatedObject {
	d := time.Duration(0)
	return &CorrelatedObject{
		OldValue: old,
		Value:    new,
		Schema:   schema,
		Duration: &d,
	}
}

// If OldValue or Value is not a list, or the index is out of bounds of the
// Value list, returns nil
// If oldValue is a list, this considers the x-list-type to decide how to
// correlate old values:
//
// If listType is map, creates a map representation of the list using the designated
// map-keys, caches it for future calls, and returns the map value, or nil if
// the correlated key is not in the old map
//
// Otherwise, if the list type is not correlatable this function returns nil.
func (r *CorrelatedObject) correlateOldValueForChildAtNewIndex(index int) interface{} {
	oldAsList, ok := r.OldValue.([]interface{})
	if !ok {
		return nil
	}

	asList, ok := r.Value.([]interface{})
	if !ok {
		return nil
	} else if len(asList) <= index {
		// Cannot correlate out of bounds index
		return nil
	}

	listType := r.Schema.XListType()
	switch listType {
	case "map":
		// Look up keys for this index in current object
		currentElement := asList[index]

		oldList := r.mapList
		if oldList == nil {
			oldList = MakeMapList(r.Schema, oldAsList)
			r.mapList = oldList
		}
		return oldList.Get(currentElement)

	case "set":
		// Are sets correlatable? Only if the old value equals the current value.
		// We might be able to support this, but do not currently see a lot
		// of value
		// (would allow you to add/remove items from sets with ratcheting but not change them)
		return nil
	case "":
		fallthrough
	case "atomic":
		// Atomic lists are the default are not correlatable by item
		// Ratcheting is not available on a per-index basis
		return nil
	default:
		// Unrecognized list type. Assume non-correlatable.
		return nil
	}
}

// CachedDeepEqual is equivalent to reflect.DeepEqual, but caches the
// results in the tree of ratchetInvocationScratch objects on the way:
//
// For objects and arrays, this function will make a best effort to make
// use of past DeepEqual checks performed by this Node's children, if available.
//
// If a lazy computation could not be found for all children possibly due
// to validation logic short circuiting and skipping the children, then
// this function simply defers to reflect.DeepEqual.
func (r *CorrelatedObject) CachedDeepEqual() (res bool) {
	start := time.Now()
	defer func() {
		if r != nil && r.Duration != nil {
			*r.Duration += time.Since(start)
		}
	}()

	if r == nil {
		// Uncorrelatable node is not considered equal to its old value
		return false
	} else if r.comparisonResult != nil {
		return *r.comparisonResult
	}

	defer func() {
		r.comparisonResult = &res
	}()

	if r.Value == nil && r.OldValue == nil {
		return true
	} else if r.Value == nil || r.OldValue == nil {
		return false
	}

	oldAsArray, oldIsArray := r.OldValue.([]interface{})
	newAsArray, newIsArray := r.Value.([]interface{})

	oldAsMap, oldIsMap := r.OldValue.(map[string]interface{})
	newAsMap, newIsMap := r.Value.(map[string]interface{})

	// If old and new are not the same type, they are not equal
	if (oldIsArray != newIsArray) || oldIsMap != newIsMap {
		return false
	}

	// Objects are known to be same type of (map, slice, or primitive)
	switch {
	case oldIsArray:
		// Both arrays case. oldIsArray == newIsArray
		if len(oldAsArray) != len(newAsArray) {
			return false
		}

		for i := range newAsArray {
			child := r.Index(i)
			if child == nil {
				if r.mapList == nil {
					// Treat non-correlatable array as a unit with reflect.DeepEqual
					return reflect.DeepEqual(oldAsArray, newAsArray)
				}

				// If array is correlatable, but old not found. Just short circuit
				// comparison
				return false

			} else if !child.CachedDeepEqual() {
				// If one child is not equal the entire object is not equal
				return false
			}
		}

		return true
	case oldIsMap:
		// Both maps case. oldIsMap == newIsMap
		if len(oldAsMap) != len(newAsMap) {
			return false
		}

		for k := range newAsMap {
			child := r.Key(k)
			if child == nil {
				// Un-correlatable child due to key change.
				// Objects are not equal.
				return false
			} else if !child.CachedDeepEqual() {
				// If one child is not equal the entire object is not equal
				return false
			}
		}

		return true

	default:
		// Primitive: use reflect.DeepEqual
		return reflect.DeepEqual(r.OldValue, r.Value)
	}
}

// Key returns the child of the receiver with the given name.
// Returns nil if the given name is does not exist in the new object, or its
// value is not correlatable to an old value.
// If receiver is nil or if the new value is not an object/map, returns nil.
func (r *CorrelatedObject) Key(field string) *CorrelatedObject {
	start := time.Now()
	defer func() {
		if r != nil && r.Duration != nil {
			*r.Duration += time.Since(start)
		}
	}()

	if r == nil || r.Schema == nil {
		return nil
	} else if existing, exists := r.children[field]; exists {
		return existing
	}

	// Find correlated old value
	oldAsMap, okOld := r.OldValue.(map[string]interface{})
	newAsMap, okNew := r.Value.(map[string]interface{})
	if !okOld || !okNew {
		return nil
	}

	oldValueForField, okOld := oldAsMap[field]
	newValueForField, okNew := newAsMap[field]
	if !okOld || !okNew {
		return nil
	}

	var propertySchema Schema
	if prop, exists := r.Schema.Properties()[field]; exists {
		propertySchema = prop
	} else if addP := r.Schema.AdditionalProperties(); addP != nil && addP.Schema() != nil {
		propertySchema = addP.Schema()
	} else {
		return nil
	}

	if r.children == nil {
		r.children = make(map[interface{}]*CorrelatedObject, len(newAsMap))
	}

	res := &CorrelatedObject{
		OldValue: oldValueForField,
		Value:    newValueForField,
		Schema:   propertySchema,
		Duration: r.Duration,
	}
	r.children[field] = res
	return res
}

// Index returns the child of the receiver at the given index.
// Returns nil if the given index is out of bounds, or its value is not
// correlatable to an old value.
// If receiver is nil or if the new value is not an array, returns nil.
func (r *CorrelatedObject) Index(i int) *CorrelatedObject {
	start := time.Now()
	defer func() {
		if r != nil && r.Duration != nil {
			*r.Duration += time.Since(start)
		}
	}()

	if r == nil || r.Schema == nil {
		return nil
	} else if existing, exists := r.children[i]; exists {
		return existing
	}

	asList, ok := r.Value.([]interface{})
	if !ok || len(asList) <= i {
		return nil
	}

	oldValueForIndex := r.correlateOldValueForChildAtNewIndex(i)
	if oldValueForIndex == nil {
		return nil
	}
	var itemSchema Schema
	if i := r.Schema.Items(); i != nil {
		itemSchema = i
	} else {
		return nil
	}

	if r.children == nil {
		r.children = make(map[interface{}]*CorrelatedObject, len(asList))
	}

	res := &CorrelatedObject{
		OldValue: oldValueForIndex,
		Value:    asList[i],
		Schema:   itemSchema,
		Duration: r.Duration,
	}
	r.children[i] = res
	return res
}
