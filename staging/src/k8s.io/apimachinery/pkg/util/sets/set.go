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

package sets

import (
	"cmp"
	"slices"
)

// Set is a set of the same type elements, implemented via map[comparable]struct{} for minimal memory consumption.
type Set[T comparable] map[T]Empty

// cast transforms specified set to generic Set[T].
func cast[T comparable](s map[T]Empty) Set[T] { return s }

// New creates a Set from a list of values.
// NOTE: type param must be explicitly instantiated if given items are empty.
func New[T comparable](items ...T) Set[T] {
	ss := make(Set[T], len(items))
	ss.Insert(items...)
	return ss
}

// KeySet creates a Set from a keys of a map[comparable](? extends interface{}).
// If the value passed in is not actually a map, this will panic.
func KeySet[T comparable, V any](theMap map[T]V) Set[T] {
	ret := make(Set[T], len(theMap))
	for keyValue := range theMap {
		ret.Insert(keyValue)
	}
	return ret
}

// Insert adds items to the set.
func (s Set[T]) Insert(items ...T) Set[T] {
	for _, item := range items {
		s[item] = Empty{}
	}
	return s
}

func Insert[T comparable](set Set[T], items ...T) Set[T] {
	return set.Insert(items...)
}

// Delete removes all items from the set.
func (s Set[T]) Delete(items ...T) Set[T] {
	for _, item := range items {
		delete(s, item)
	}
	return s
}

// Clear empties the set.
// It is preferable to replace the set with a newly constructed set,
// but not all callers can do that (when there are other references to the map).
func (s Set[T]) Clear() Set[T] {
	clear(s)
	return s
}

// Has returns true if and only if item is contained in the set.
func (s Set[T]) Has(item T) bool {
	_, contained := s[item]
	return contained
}

// HasAll returns true if and only if all items are contained in the set.
func (s Set[T]) HasAll(items ...T) bool {
	for _, item := range items {
		if !s.Has(item) {
			return false
		}
	}
	return true
}

// HasAny returns true if any items are contained in the set.
func (s Set[T]) HasAny(items ...T) bool {
	for _, item := range items {
		if s.Has(item) {
			return true
		}
	}
	return false
}

// Clone returns a new set which is a copy of the current set.
func (s Set[T]) Clone() Set[T] {
	result := make(Set[T], len(s))
	for key := range s {
		result.Insert(key)
	}
	return result
}

// Difference returns a set of objects that are not in s2.
// For example:
// s1 = {a1, a2, a3}
// s2 = {a1, a2, a4, a5}
// s1.Difference(s2) = {a3}
// s2.Difference(s1) = {a4, a5}
func (s1 Set[T]) Difference(s2 Set[T]) Set[T] {
	result := New[T]()
	for key := range s1 {
		if !s2.Has(key) {
			result.Insert(key)
		}
	}
	return result
}

// SymmetricDifference returns a set of elements which are in either of the sets, but not in their intersection.
// For example:
// s1 = {a1, a2, a3}
// s2 = {a1, a2, a4, a5}
// s1.SymmetricDifference(s2) = {a3, a4, a5}
// s2.SymmetricDifference(s1) = {a3, a4, a5}
func (s1 Set[T]) SymmetricDifference(s2 Set[T]) Set[T] {
	return s1.Difference(s2).Union(s2.Difference(s1))
}

// Union returns a new set which includes items in either s1 or s2.
// For example:
// s1 = {a1, a2}
// s2 = {a3, a4}
// s1.Union(s2) = {a1, a2, a3, a4}
// s2.Union(s1) = {a1, a2, a3, a4}
func (s1 Set[T]) Union(s2 Set[T]) Set[T] {
	result := s1.Clone()
	for key := range s2 {
		result.Insert(key)
	}
	return result
}

// Intersection returns a new set which includes the item in BOTH s1 and s2
// For example:
// s1 = {a1, a2}
// s2 = {a2, a3}
// s1.Intersection(s2) = {a2}
func (s1 Set[T]) Intersection(s2 Set[T]) Set[T] {
	var walk, other Set[T]
	result := New[T]()
	if s1.Len() < s2.Len() {
		walk = s1
		other = s2
	} else {
		walk = s2
		other = s1
	}
	for key := range walk {
		if other.Has(key) {
			result.Insert(key)
		}
	}
	return result
}

// IsSuperset returns true if and only if s1 is a superset of s2.
func (s1 Set[T]) IsSuperset(s2 Set[T]) bool {
	for item := range s2 {
		if !s1.Has(item) {
			return false
		}
	}
	return true
}

// Equal returns true if and only if s1 is equal (as a set) to s2.
// Two sets are equal if their membership is identical.
// (In practice, this means same elements, order doesn't matter)
func (s1 Set[T]) Equal(s2 Set[T]) bool {
	return len(s1) == len(s2) && s1.IsSuperset(s2)
}

// List returns the contents as a sorted T slice.
//
// This is a separate function and not a method because not all types supported
// by Generic are ordered and only those can be sorted.
func List[T cmp.Ordered](s Set[T]) []T {
	res := s.UnsortedList()
	slices.Sort(res)
	return res
}

// UnsortedList returns the slice with contents in random order.
func (s Set[T]) UnsortedList() []T {
	res := make([]T, 0, len(s))
	for key := range s {
		res = append(res, key)
	}
	return res
}

// PopAny returns a single element from the set.
func (s Set[T]) PopAny() (T, bool) {
	for key := range s {
		s.Delete(key)
		return key, true
	}
	var zeroValue T
	return zeroValue, false
}

// Len returns the size of the set.
func (s Set[T]) Len() int {
	return len(s)
}
