// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package reflect

import (
	"testing"
)

func TestEqualities(t *testing.T) {
	e := Equalities{}
	type Bar struct {
		X int
	}
	type Baz struct {
		Y Bar
	}
	type Zap struct {
		A []int
		B map[string][]int
	}
	err := e.AddFuncs(
		func(a, b int) bool {
			return a+1 == b
		},
		func(a, b Bar) bool {
			return a.X*10 == b.X
		},
	)
	if err != nil {
		t.Fatalf("Unexpected: %v", err)
	}

	type Foo struct {
		X int
	}

	type Case struct {
		a, b  interface{}
		equal bool
	}

	table := []Case{
		{1, 2, true},
		{2, 1, false},
		{"foo", "fo", false},
		{"foo", "foo", true},
		{"foo", "foobar", false},
		{Foo{1}, Foo{2}, true},
		{Foo{2}, Foo{1}, false},
		{Bar{1}, Bar{10}, true},
		{&Bar{1}, &Bar{10}, true},
		{Baz{Bar{1}}, Baz{Bar{10}}, true},
		{[...]string{}, [...]string{"1", "2", "3"}, false},
		{[...]string{"1"}, [...]string{"1", "2", "3"}, false},
		{[...]string{"1", "2", "3"}, [...]string{}, false},
		{[...]string{"1", "2", "3"}, [...]string{"1", "2", "3"}, true},
		{map[string]int{"foo": 1}, map[string]int{}, false},
		{map[string]int{"foo": 1}, map[string]int{"foo": 2}, true},
		{map[string]int{"foo": 2}, map[string]int{"foo": 1}, false},
		{map[string]int{"foo": 1}, map[string]int{"foo": 2, "bar": 6}, false},
		{map[string]int{"foo": 1, "bar": 6}, map[string]int{"foo": 2}, false},
		{map[string]int{}, map[string]int(nil), true},
		{[]string(nil), []string(nil), true},
		{[]string{}, []string(nil), true},
		{[]string(nil), []string{}, true},
		{[]string{"1"}, []string(nil), false},
		{[]string{}, []string{"1", "2", "3"}, false},
		{[]string{"1"}, []string{"1", "2", "3"}, false},
		{[]string{"1", "2", "3"}, []string{}, false},
	}

	for _, item := range table {
		if e, a := item.equal, e.DeepEqual(item.a, item.b); e != a {
			t.Errorf("Expected (%+v == %+v) == %v, but got %v", item.a, item.b, e, a)
		}
	}

	// Cases which hinge upon implicit nil/empty map/slice equality
	implicitTable := []Case{
		{map[string][]int{}, map[string][]int(nil), true},
		{[]int{}, []int(nil), true},
		{map[string][]int{"foo": nil}, map[string][]int{"foo": {}}, true},
		{Zap{A: nil, B: map[string][]int{"foo": nil}}, Zap{A: []int{}, B: map[string][]int{"foo": {}}}, true},
	}

	for _, item := range implicitTable {
		if e, a := item.equal, e.DeepEqual(item.a, item.b); e != a {
			t.Errorf("Expected (%+v == %+v) == %v, but got %v", item.a, item.b, e, a)
		}
	}

	for _, item := range implicitTable {
		if e, a := !item.equal, e.DeepEqualWithNilDifferentFromEmpty(item.a, item.b); e != a {
			t.Errorf("Expected (%+v == %+v) == %v, but got %v", item.a, item.b, e, a)
		}
	}
}

func TestPublicEqualities(t *testing.T) {
	e := Equalities{}

	type Foo struct {
		X int
	}

	type PrivateFoo struct {
		X int
		y int
	}

	type NestedFoo struct {
		X Foo
	}

	type NestedPrivateFoo struct {
		X PrivateFoo
	}

	type MapFoo struct {
		X map[string]Foo
	}

	type NestedMapFoo struct {
		X MapFoo
	}

	type Case struct {
		name          string
		a, b          interface{}
		options       *DeepEqualOptions
		shouldBeEqual bool
		shouldPanic   bool
	}

	table := []Case{
		{"equal values are considered equal", 1, 1, &DeepEqualOptions{}, true, false},
		{"unequal values are not considered equal", 1, 2, &DeepEqualOptions{}, false, false},
		{"equal structs are considered equal", Foo{1}, Foo{1}, &DeepEqualOptions{}, true, false},
		{"unequal structs are not considered equal", Foo{2}, Foo{1}, &DeepEqualOptions{}, false, false},

		{"structs with unexported fields panic", PrivateFoo{1, 1}, PrivateFoo{1, 1}, &DeepEqualOptions{}, true, true},

		{
			"structs with unexported fields do not panic when ignoring unexported fields",
			PrivateFoo{1, 1}, PrivateFoo{1, 1},
			&DeepEqualOptions{IgnoreUnexportedFields: true},
			true, false,
		},

		{"nested structs are considered equal", NestedFoo{Foo{1}}, NestedFoo{Foo{1}}, &DeepEqualOptions{}, true, false},
		{"nested structs are not considered equal", NestedFoo{Foo{1}}, NestedFoo{Foo{2}}, &DeepEqualOptions{}, false, false},

		{
			"nested structs with unexported fields panic",
			NestedPrivateFoo{PrivateFoo{1, 1}}, NestedPrivateFoo{PrivateFoo{1, 1}},
			&DeepEqualOptions{},
			false, true,
		},
		{
			"nested structs with unexported fields do not panic when ignored unexported fields",
			NestedPrivateFoo{PrivateFoo{1, 1}}, NestedPrivateFoo{PrivateFoo{1, 1}},
			&DeepEqualOptions{IgnoreUnexportedFields: true},
			true, false,
		},

		{
			"same-type maps are considered equal",
			map[string]string{"foo": "bar"},
			map[string]string{"foo": "bar"},
			&DeepEqualOptions{}, true, false,
		},
		{
			"same-type maps are not considered equal",
			map[string]string{"foo": "bar"},
			map[string]string{"foo": "foo"},
			&DeepEqualOptions{}, false, false,
		},
		{
			"different-type maps are not considered equal",
			map[string]string{"foo": "bar"},
			map[string]interface{}{"foo": 123},
			&DeepEqualOptions{}, false, false,
		},

		{
			"slices of structs are considered equal",
			[]Foo{{1}, {1}}, []Foo{{1}, {1}},
			&DeepEqualOptions{}, true, false,
		},
		{
			"slices of structs are not considered equal",
			[]Foo{{1}, {1}}, []Foo{{1}, {2}},
			&DeepEqualOptions{}, false, false,
		},
		{
			"slices of structs are not considered equal, variant 2",
			[]Foo{{2}, {1}}, []Foo{{1}, {2}},
			&DeepEqualOptions{}, false, false,
		},

		{
			"slices of nested structs are considered equal",
			[]NestedFoo{{Foo{1}}},
			[]NestedFoo{{Foo{1}}},
			&DeepEqualOptions{}, true, false,
		},
		{
			"slices of nested structs are considered equal, variant 2",
			[]NestedFoo{{Foo{1}}, {Foo{2}}},
			[]NestedFoo{{Foo{1}}, {Foo{2}}},
			&DeepEqualOptions{}, true, false,
		},
		{
			"slices of nested structs are not considered equal",
			[]NestedFoo{{Foo{1}}, {Foo{2}}},
			[]NestedFoo{{Foo{2}}, {Foo{1}}},
			&DeepEqualOptions{}, false, false,
		},

		{
			"slices of nested structs with unexported fields panic",
			[]NestedPrivateFoo{{PrivateFoo{1, 1}}},
			[]NestedPrivateFoo{{PrivateFoo{1, 1}}},
			&DeepEqualOptions{}, true, true,
		},
		{
			"slices of nested structs with unexported fields do not panic when ignore unexported fields",
			[]NestedPrivateFoo{{PrivateFoo{1, 1}}},
			[]NestedPrivateFoo{{PrivateFoo{1, 1}}},
			&DeepEqualOptions{IgnoreUnexportedFields: true}, true, false,
		},
		{
			"slices of nested structs with unexported fields do not panic when ignore unexported fields, variant 2",
			[]NestedPrivateFoo{{PrivateFoo{1, 1}}},
			[]NestedPrivateFoo{{PrivateFoo{2, 1}}},
			&DeepEqualOptions{IgnoreUnexportedFields: true}, false, false,
		},
		{
			"slices of nested structs with unexported fields do not panic when ignore unexported fields, variant 3",
			[]NestedPrivateFoo{{PrivateFoo{2, 1}}},
			[]NestedPrivateFoo{{PrivateFoo{2, 2}}},
			&DeepEqualOptions{IgnoreUnexportedFields: true}, true, false,
		},
	}

	for _, testCase := range table {
		func(tc *Case) {

			defer func() {
				r := recover()
				if r == nil && tc.shouldPanic {
					t.Errorf("%s: did not panic but should have", tc.name)
				}

				if r != nil && !tc.shouldPanic {
					panic(r)
				}
			}()

			eq := e.DeepEqualWithOptions(tc.a, tc.b, tc.options)

			if eq && !tc.shouldBeEqual {
				t.Errorf("%s: expected inequality but got equality for %v and %v", tc.name, tc.a, tc.b)
			}

			if !eq && tc.shouldBeEqual {
				t.Errorf("%s: expected equality but got inequality for %v and %v", tc.name, tc.a, tc.b)
			}
		}(&testCase)
	}
}

func TestDerivatives(t *testing.T) {
	e := Equalities{}
	type Bar struct {
		X int
	}
	type Baz struct {
		Y Bar
	}
	err := e.AddFuncs(
		func(a, b int) bool {
			return a+1 == b
		},
		func(a, b Bar) bool {
			return a.X*10 == b.X
		},
	)
	if err != nil {
		t.Fatalf("Unexpected: %v", err)
	}

	type Foo struct {
		X int
	}

	table := []struct {
		a, b  interface{}
		equal bool
	}{
		{1, 2, true},
		{2, 1, false},
		{"foo", "fo", false},
		{"foo", "foo", true},
		{"foo", "foobar", false},
		{Foo{1}, Foo{2}, true},
		{Foo{2}, Foo{1}, false},
		{Bar{1}, Bar{10}, true},
		{&Bar{1}, &Bar{10}, true},
		{Baz{Bar{1}}, Baz{Bar{10}}, true},
		{[...]string{}, [...]string{"1", "2", "3"}, false},
		{[...]string{"1"}, [...]string{"1", "2", "3"}, false},
		{[...]string{"1", "2", "3"}, [...]string{}, false},
		{[...]string{"1", "2", "3"}, [...]string{"1", "2", "3"}, true},
		{map[string]int{"foo": 1}, map[string]int{}, false},
		{map[string]int{"foo": 1}, map[string]int{"foo": 2}, true},
		{map[string]int{"foo": 2}, map[string]int{"foo": 1}, false},
		{map[string]int{"foo": 1}, map[string]int{"foo": 2, "bar": 6}, true},
		{map[string]int{"foo": 1, "bar": 6}, map[string]int{"foo": 2}, false},
		{map[string]int{}, map[string]int(nil), true},
		{[]string(nil), []string(nil), true},
		{[]string{}, []string(nil), true},
		{[]string(nil), []string{}, true},
		{[]string{"1"}, []string(nil), false},
		{[]string{}, []string{"1", "2", "3"}, true},
		{[]string{"1"}, []string{"1", "2", "3"}, true},
		{[]string{"1", "2", "3"}, []string{}, false},
	}

	for _, item := range table {
		if e, a := item.equal, e.DeepDerivative(item.a, item.b); e != a {
			t.Errorf("Expected (%+v ~ %+v) == %v, but got %v", item.a, item.b, e, a)
		}
	}
}
