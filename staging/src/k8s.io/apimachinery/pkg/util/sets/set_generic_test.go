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

package sets_test

import (
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/util/sets"
)

func TestSet(t *testing.T) {
	s := sets.Set[string]{}
	s2 := sets.Set[string]{}
	if len(s) != 0 {
		t.Errorf("Expected len=0: %d", len(s))
	}
	s.Insert("a", "b")
	if len(s) != 2 {
		t.Errorf("Expected len=2: %d", len(s))
	}
	s.Insert("c")
	if s.Has("d") {
		t.Errorf("Unexpected contents: %#v", s)
	}
	if !s.Has("a") {
		t.Errorf("Missing contents: %#v", s)
	}
	s.Delete("a")
	if s.Has("a") {
		t.Errorf("Unexpected contents: %#v", s)
	}
	s.Insert("a")
	if s.HasAll("a", "b", "d") {
		t.Errorf("Unexpected contents: %#v", s)
	}
	if !s.HasAll("a", "b") {
		t.Errorf("Missing contents: %#v", s)
	}
	s2.Insert("a", "b", "d")
	if s.IsSuperset(s2) {
		t.Errorf("Unexpected contents: %#v", s)
	}
	s2.Delete("d")
	if !s.IsSuperset(s2) {
		t.Errorf("Missing contents: %#v", s)
	}
}

func TestSetDeleteMultiples(t *testing.T) {
	s := sets.Set[string]{}
	s.Insert("a", "b", "c")
	if len(s) != 3 {
		t.Errorf("Expected len=3: %d", len(s))
	}

	s.Delete("a", "c")
	if len(s) != 1 {
		t.Errorf("Expected len=1: %d", len(s))
	}
	if s.Has("a") {
		t.Errorf("Unexpected contents: %#v", s)
	}
	if s.Has("c") {
		t.Errorf("Unexpected contents: %#v", s)
	}
	if !s.Has("b") {
		t.Errorf("Missing contents: %#v", s)
	}

}

func TestSetClear(t *testing.T) {
	s := sets.Set[string]{}
	s.Insert("a", "b", "c")
	if s.Len() != 3 {
		t.Errorf("Expected len=3: %d", s.Len())
	}

	s.Clear()
	if s.Len() != 0 {
		t.Errorf("Expected len=0: %d", s.Len())
	}
}

func TestSetClearWithSharedReference(t *testing.T) {
	s := sets.Set[string]{}
	s.Insert("a", "b", "c")
	if s.Len() != 3 {
		t.Errorf("Expected len=3: %d", s.Len())
	}

	m := s
	s.Clear()
	if s.Len() != 0 {
		t.Errorf("Expected len=0 on the cleared set: %d", s.Len())
	}
	if m.Len() != 0 {
		t.Errorf("Expected len=0 on the shared reference: %d", m.Len())
	}
}

func TestSetClearInSeparateFunction(t *testing.T) {
	s := sets.Set[string]{}
	s.Insert("a", "b", "c")
	if s.Len() != 3 {
		t.Errorf("Expected len=3: %d", s.Len())
	}

	clearSetAndAdd(s, "d")
	if s.Len() != 1 {
		t.Errorf("Expected len=1: %d", s.Len())
	}
	if !s.Has("d") {
		t.Errorf("Unexpected contents: %#v", s)
	}
}

func clearSetAndAdd[T comparable](s sets.Set[T], a T) {
	s.Clear()
	s.Insert(a)
}

func TestNewSet(t *testing.T) {
	s := sets.New("a", "b", "c")
	if len(s) != 3 {
		t.Errorf("Expected len=3: %d", len(s))
	}
	if !s.Has("a") || !s.Has("b") || !s.Has("c") {
		t.Errorf("Unexpected contents: %#v", s)
	}
}

func TestKeySet(t *testing.T) {
	m := map[string]int{"a": 1, "b": 2, "c": 3}
	ss := sets.KeySet(m)
	if !ss.Equal(sets.New("a", "b", "c")) {
		t.Errorf("Unexpected contents: %#v", sets.List(ss))
	}
}

func TestNewEmptySet(t *testing.T) {
	s := sets.New[string]()
	if len(s) != 0 {
		t.Errorf("Expected len=0: %d", len(s))
	}
	s.Insert("a", "b", "c")
	if len(s) != 3 {
		t.Errorf("Expected len=3: %d", len(s))
	}
	if !s.Has("a") || !s.Has("b") || !s.Has("c") {
		t.Errorf("Unexpected contents: %#v", s)
	}
}

func TestSortedList(t *testing.T) {
	s := sets.New("z", "y", "x", "a")
	if !reflect.DeepEqual(sets.List(s), []string{"a", "x", "y", "z"}) {
		t.Errorf("List gave unexpected result: %#v", sets.List(s))
	}
}

func TestSetDifference(t *testing.T) {
	a := sets.New("1", "2", "3")
	b := sets.New("1", "2", "4", "5")
	c := a.Difference(b)
	d := b.Difference(a)
	if len(c) != 1 {
		t.Errorf("Expected len=1: %d", len(c))
	}
	if !c.Has("3") {
		t.Errorf("Unexpected contents: %#v", sets.List(c))
	}
	if len(d) != 2 {
		t.Errorf("Expected len=2: %d", len(d))
	}
	if !d.Has("4") || !d.Has("5") {
		t.Errorf("Unexpected contents: %#v", sets.List(d))
	}
}

func TestSetSymmetricDifference(t *testing.T) {
	a := sets.New("1", "2", "3")
	b := sets.New("1", "2", "4", "5")
	c := a.SymmetricDifference(b)
	d := b.SymmetricDifference(a)
	if !c.Equal(sets.New("3", "4", "5")) {
		t.Errorf("Unexpected contents: %#v", sets.List(c))
	}
	if !d.Equal(sets.New("3", "4", "5")) {
		t.Errorf("Unexpected contents: %#v", sets.List(d))
	}
}

func TestSetHasAny(t *testing.T) {
	a := sets.New("1", "2", "3")

	if !a.HasAny("1", "4") {
		t.Errorf("expected true, got false")
	}

	if a.HasAny("0", "4") {
		t.Errorf("expected false, got true")
	}
}

func TestSetEquals(t *testing.T) {
	// Simple case (order doesn't matter)
	a := sets.New("1", "2")
	b := sets.New("2", "1")
	if !a.Equal(b) {
		t.Errorf("Expected to be equal: %v vs %v", a, b)
	}

	// It is a set; duplicates are ignored
	b = sets.New("2", "2", "1")
	if !a.Equal(b) {
		t.Errorf("Expected to be equal: %v vs %v", a, b)
	}

	// Edge cases around empty sets / empty strings
	a = sets.New[string]()
	b = sets.New[string]()
	if !a.Equal(b) {
		t.Errorf("Expected to be equal: %v vs %v", a, b)
	}

	b = sets.New("1", "2", "3")
	if a.Equal(b) {
		t.Errorf("Expected to be not-equal: %v vs %v", a, b)
	}

	b = sets.New("1", "2", "")
	if a.Equal(b) {
		t.Errorf("Expected to be not-equal: %v vs %v", a, b)
	}

	// Check for equality after mutation
	a = sets.New[string]()
	a.Insert("1")
	if a.Equal(b) {
		t.Errorf("Expected to be not-equal: %v vs %v", a, b)
	}

	a.Insert("2")
	if a.Equal(b) {
		t.Errorf("Expected to be not-equal: %v vs %v", a, b)
	}

	a.Insert("")
	if !a.Equal(b) {
		t.Errorf("Expected to be equal: %v vs %v", a, b)
	}

	a.Delete("")
	if a.Equal(b) {
		t.Errorf("Expected to be not-equal: %v vs %v", a, b)
	}
}

func TestUnion(t *testing.T) {
	tests := []struct {
		s1       sets.Set[string]
		s2       sets.Set[string]
		expected sets.Set[string]
	}{
		{
			sets.New("1", "2", "3", "4"),
			sets.New("3", "4", "5", "6"),
			sets.New("1", "2", "3", "4", "5", "6"),
		},
		{
			sets.New("1", "2", "3", "4"),
			sets.New[string](),
			sets.New("1", "2", "3", "4"),
		},
		{
			sets.New[string](),
			sets.New("1", "2", "3", "4"),
			sets.New("1", "2", "3", "4"),
		},
		{
			sets.New[string](),
			sets.New[string](),
			sets.New[string](),
		},
	}

	for _, test := range tests {
		union := test.s1.Union(test.s2)
		if union.Len() != test.expected.Len() {
			t.Errorf("Expected union.Len()=%d but got %d", test.expected.Len(), union.Len())
		}

		if !union.Equal(test.expected) {
			t.Errorf("Expected union.Equal(expected) but not true.  union:%v expected:%v", sets.List(union), sets.List(test.expected))
		}
	}
}

func TestIntersection(t *testing.T) {
	tests := []struct {
		s1       sets.Set[string]
		s2       sets.Set[string]
		expected sets.Set[string]
	}{
		{
			sets.New("1", "2", "3", "4"),
			sets.New("3", "4", "5", "6"),
			sets.New("3", "4"),
		},
		{
			sets.New("1", "2", "3", "4"),
			sets.New("1", "2", "3", "4"),
			sets.New("1", "2", "3", "4"),
		},
		{
			sets.New("1", "2", "3", "4"),
			sets.New[string](),
			sets.New[string](),
		},
		{
			sets.New[string](),
			sets.New("1", "2", "3", "4"),
			sets.New[string](),
		},
		{
			sets.New[string](),
			sets.New[string](),
			sets.New[string](),
		},
	}

	for _, test := range tests {
		intersection := test.s1.Intersection(test.s2)
		if intersection.Len() != test.expected.Len() {
			t.Errorf("Expected intersection.Len()=%d but got %d", test.expected.Len(), intersection.Len())
		}

		if !intersection.Equal(test.expected) {
			t.Errorf("Expected intersection.Equal(expected) but not true.  intersection:%v expected:%v", sets.List(intersection), sets.List(intersection))
		}
	}
}
