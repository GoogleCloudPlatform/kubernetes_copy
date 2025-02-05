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

// Int64 is a set of int64s, implemented via map[int64]struct{} for minimal memory consumption.
//
// Deprecated: use generic Set instead.
// new ways:
// s1 := Set[int64]{}
// s2 := New[int64]()
type Int64 map[int64]Empty

// NewInt64 creates a Int64 from a list of values.
func NewInt64(items ...int64) Int64 {
	return Int64(New(items...))
}

// Int64KeySet creates a Int64 from a keys of a map[int64](? extends interface{}).
// If the value passed in is not actually a map, this will panic.
func Int64KeySet[T any](theMap map[int64]T) Int64 {
	return Int64(KeySet(theMap))
}

// Insert adds items to the set.
func (s Int64) Insert(items ...int64) Int64 {
	return Int64(cast(s).Insert(items...))
}

// Delete removes all items from the set.
func (s Int64) Delete(items ...int64) Int64 {
	return Int64(cast(s).Delete(items...))
}

// Has returns true if and only if item is contained in the set.
func (s Int64) Has(item int64) bool {
	return cast(s).Has(item)
}

// HasAll returns true if and only if all items are contained in the set.
func (s Int64) HasAll(items ...int64) bool {
	return cast(s).HasAll(items...)
}

// HasAny returns true if any items are contained in the set.
func (s Int64) HasAny(items ...int64) bool {
	return cast(s).HasAny(items...)
}

// Clone returns a new set which is a copy of the current set.
func (s Int64) Clone() Int64 {
	return Int64(cast(s).Clone())
}

// Difference returns a set of objects that are not in s2.
// For example:
// s1 = {a1, a2, a3}
// s2 = {a1, a2, a4, a5}
// s1.Difference(s2) = {a3}
// s2.Difference(s1) = {a4, a5}
func (s1 Int64) Difference(s2 Int64) Int64 {
	return Int64(cast(s1).Difference(cast(s2)))
}

// SymmetricDifference returns a set of elements which are in either of the sets, but not in their intersection.
// For example:
// s1 = {a1, a2, a3}
// s2 = {a1, a2, a4, a5}
// s1.SymmetricDifference(s2) = {a3, a4, a5}
// s2.SymmetricDifference(s1) = {a3, a4, a5}
func (s1 Int64) SymmetricDifference(s2 Int64) Int64 {
	return Int64(cast(s1).SymmetricDifference(cast(s2)))
}

// Union returns a new set which includes items in either s1 or s2.
// For example:
// s1 = {a1, a2}
// s2 = {a3, a4}
// s1.Union(s2) = {a1, a2, a3, a4}
// s2.Union(s1) = {a1, a2, a3, a4}
func (s1 Int64) Union(s2 Int64) Int64 {
	return Int64(cast(s1).Union(cast(s2)))
}

// Intersection returns a new set which includes the item in BOTH s1 and s2
// For example:
// s1 = {a1, a2}
// s2 = {a2, a3}
// s1.Intersection(s2) = {a2}
func (s1 Int64) Intersection(s2 Int64) Int64 {
	return Int64(cast(s1).Intersection(cast(s2)))
}

// IsSuperset returns true if and only if s1 is a superset of s2.
func (s1 Int64) IsSuperset(s2 Int64) bool {
	return cast(s1).IsSuperset(cast(s2))
}

// Equal returns true if and only if s1 is equal (as a set) to s2.
// Two sets are equal if their membership is identical.
// (In practice, this means same elements, order doesn't matter)
func (s1 Int64) Equal(s2 Int64) bool {
	return cast(s1).Equal(cast(s2))
}

// List returns the contents as a sorted int64 slice.
func (s Int64) List() []int64 {
	return List(cast(s))
}

// UnsortedList returns the slice with contents in random order.
func (s Int64) UnsortedList() []int64 {
	return cast(s).UnsortedList()
}

// PopAny returns a single element from the set.
func (s Int64) PopAny() (int64, bool) {
	return cast(s).PopAny()
}

// Len returns the size of the set.
func (s Int64) Len() int {
	return len(s)
}
