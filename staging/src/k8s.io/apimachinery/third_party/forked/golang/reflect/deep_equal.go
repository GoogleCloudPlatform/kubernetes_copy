// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package reflect is a fork of go's standard library reflection package, which
// allows for deep equal with equality functions defined.
package reflect

import (
	"fmt"
	"reflect"
	"strings"
)

// Equalities is a map from type to a function comparing two values of
// that type.
type Equalities map[reflect.Type]reflect.Value

// For convenience, panics on errors
func EqualitiesOrDie(funcs ...interface{}) Equalities {
	e := Equalities{}
	if err := e.AddFuncs(funcs...); err != nil {
		panic(err)
	}
	return e
}

// AddFuncs is a shortcut for multiple calls to AddFunc.
func (e Equalities) AddFuncs(funcs ...interface{}) error {
	for _, f := range funcs {
		if err := e.AddFunc(f); err != nil {
			return err
		}
	}
	return nil
}

// AddFunc uses func as an equality function: it must take
// two parameters of the same type, and return a boolean.
func (e Equalities) AddFunc(eqFunc interface{}) error {
	fv := reflect.ValueOf(eqFunc)
	ft := fv.Type()
	if ft.Kind() != reflect.Func {
		return fmt.Errorf("expected func, got: %v", ft)
	}
	if ft.NumIn() != 2 {
		return fmt.Errorf("expected two 'in' params, got: %v", ft)
	}
	if ft.NumOut() != 1 {
		return fmt.Errorf("expected one 'out' param, got: %v", ft)
	}
	if ft.In(0) != ft.In(1) {
		return fmt.Errorf("expected arg 1 and 2 to have same type, but got %v", ft)
	}
	var forReturnType bool
	boolType := reflect.TypeOf(forReturnType)
	if ft.Out(0) != boolType {
		return fmt.Errorf("expected bool return, got: %v", ft)
	}
	e[ft.In(0)] = fv
	return nil
}

// Below here is forked from go's reflect/deepequal.go

// During deepValueEqual, must keep track of checks that are
// in progress.  The comparison algorithm assumes that all
// checks in progress are true when it reencounters them.
// Visited comparisons are stored in a map indexed by visit.
type visit struct {
	a1  uintptr
	a2  uintptr
	typ reflect.Type
}

// unexportedTypePanic is thrown when you use this DeepEqual on something that has an
// unexported type. It indicates a programmer error, so should not occur at runtime,
// which is why it's not public and thus impossible to catch.
type unexportedTypePanic []reflect.Type

func (u unexportedTypePanic) Error() string { return u.String() }
func (u unexportedTypePanic) String() string {
	strs := make([]string, len(u))
	for i, t := range u {
		strs[i] = fmt.Sprintf("%v", t)
	}
	return "an unexported field was encountered, nested like this: " + strings.Join(strs, " -> ")
}

func makeUsefulPanic(v reflect.Value) {
	if x := recover(); x != nil {
		if u, ok := x.(unexportedTypePanic); ok {
			u = append(unexportedTypePanic{v.Type()}, u...)
			x = u
		}
		panic(x)
	}
}

// Tests for deep equality using reflected types. The map argument tracks
// comparisons that have already been seen, which allows short circuiting on
// recursive types.
// equateNilAndEmpty controls whether empty maps/slices are equivalent to nil
func (e Equalities) deepValueEqual(v1, v2 reflect.Value, visited map[visit]bool, equateNilAndEmpty, ignoreUnexportedFields bool, depth int) bool {
	defer makeUsefulPanic(v1)

	if !v1.IsValid() || !v2.IsValid() {
		return v1.IsValid() == v2.IsValid()
	}
	if v1.Type() != v2.Type() {
		return false
	}
	if fv, ok := e[v1.Type()]; ok {
		return fv.Call([]reflect.Value{v1, v2})[0].Bool()
	}

	hard := func(k reflect.Kind) bool {
		switch k {
		case reflect.Array, reflect.Map, reflect.Slice, reflect.Struct:
			return true
		}
		return false
	}

	if v1.CanAddr() && v2.CanAddr() && hard(v1.Kind()) {
		addr1 := v1.UnsafeAddr()
		addr2 := v2.UnsafeAddr()
		if addr1 > addr2 {
			// Canonicalize order to reduce number of entries in visited.
			addr1, addr2 = addr2, addr1
		}

		// Short circuit if references are identical ...
		if addr1 == addr2 {
			return true
		}

		// ... or already seen
		typ := v1.Type()
		v := visit{addr1, addr2, typ}
		if visited[v] {
			return true
		}

		// Remember for later.
		visited[v] = true
	}

	switch v1.Kind() {
	case reflect.Array:
		// We don't need to check length here because length is part of
		// an array's type, which has already been filtered for.
		for i := 0; i < v1.Len(); i++ {
			if !e.deepValueEqual(v1.Index(i), v2.Index(i), visited, equateNilAndEmpty, ignoreUnexportedFields, depth+1) {
				return false
			}
		}
		return true
	case reflect.Slice:
		if equateNilAndEmpty {
			if (v1.IsNil() || v1.Len() == 0) != (v2.IsNil() || v2.Len() == 0) {
				return false
			}

			if v1.IsNil() || v1.Len() == 0 {
				return true
			}
		} else {
			if v1.IsNil() != v2.IsNil() {
				return false
			}

			// Optimize nil and empty cases
			// Two lists that are BOTH nil are equal
			// No need to check v2 is nil since v1.IsNil == v2.IsNil from above
			if v1.IsNil() {
				return true
			}

			// Two lists that are both empty and both non nil are equal
			if v1.Len() == 0 || v2.Len() == 0 {
				return true
			}
		}
		if v1.Len() != v2.Len() {
			return false
		}
		if v1.Pointer() == v2.Pointer() {
			return true
		}
		for i := 0; i < v1.Len(); i++ {
			if !e.deepValueEqual(v1.Index(i), v2.Index(i), visited, equateNilAndEmpty, ignoreUnexportedFields, depth+1) {
				return false
			}
		}
		return true
	case reflect.Interface:
		if v1.IsNil() || v2.IsNil() {
			return v1.IsNil() == v2.IsNil()
		}
		return e.deepValueEqual(v1.Elem(), v2.Elem(), visited, equateNilAndEmpty, ignoreUnexportedFields, depth+1)
	case reflect.Ptr:
		return e.deepValueEqual(v1.Elem(), v2.Elem(), visited, equateNilAndEmpty, ignoreUnexportedFields, depth+1)
	case reflect.Struct:
		for i, n := 0, v1.NumField(); i < n; i++ {
			if !e.deepValueEqual(v1.Field(i), v2.Field(i), visited, equateNilAndEmpty, ignoreUnexportedFields, depth+1) {
				return false
			}
		}
		return true
	case reflect.Map:
		if equateNilAndEmpty {
			if (v1.IsNil() || v1.Len() == 0) != (v2.IsNil() || v2.Len() == 0) {
				return false
			}
			if v1.IsNil() || v1.Len() == 0 {
				return true
			}
		} else {
			if v1.IsNil() != v2.IsNil() {
				return false
			}

			// Optimize nil and empty cases
			// Two maps that are BOTH nil are equal
			// No need to check v2 is nil since v1.IsNil == v2.IsNil from above
			if v1.IsNil() {
				return true
			}

			// Two maps that are both empty and both non nil are equal
			if v1.Len() == 0 || v2.Len() == 0 {
				return true
			}
		}
		if v1.Len() != v2.Len() {
			return false
		}
		if v1.Pointer() == v2.Pointer() {
			return true
		}
		for _, k := range v1.MapKeys() {
			if !e.deepValueEqual(v1.MapIndex(k), v2.MapIndex(k), visited, equateNilAndEmpty, ignoreUnexportedFields, depth+1) {
				return false
			}
		}
		return true
	case reflect.Func:
		if v1.IsNil() && v2.IsNil() {
			return true
		}
		// Can't do better than this:
		return false
	default:
		// Normal equality suffices
		if !v1.CanInterface() || !v2.CanInterface() {
			if ignoreUnexportedFields {
				return true
			} else {
				panic(unexportedTypePanic{})
			}
		}
		return v1.Interface() == v2.Interface()
	}
}

// DeepEqual is like reflect.DeepEqual, but focused on semantic equality
// instead of memory equality.
//
// It will use e's equality functions if it finds types that match.
//
// An empty slice *is* equal to a nil slice for our purposes; same for maps.
//
// Unexported field members cannot be compared and will cause an informative panic; you must add an Equality
// function for these types.
func (e Equalities) DeepEqual(a1, a2 interface{}) bool {
	return e.deepEqual(a1, a2, true, false)
}

func (e Equalities) DeepEqualWithNilDifferentFromEmpty(a1, a2 interface{}) bool {
	return e.deepEqual(a1, a2, false, false)
}

type DeepEqualOptions struct {
	EquateNilAndEmpty      bool
	IgnoreUnexportedFields bool
}

func (e Equalities) DeepEqualWithOptions(a1, a2 interface{}, opts *DeepEqualOptions) bool {
	return e.deepEqual(a1, a2, opts.EquateNilAndEmpty, opts.IgnoreUnexportedFields)
}

func (e Equalities) deepEqual(a1, a2 interface{}, equateNilAndEmpty, ignoreUnexportedFields bool) bool {
	if a1 == nil || a2 == nil {
		return a1 == a2
	}
	v1 := reflect.ValueOf(a1)
	v2 := reflect.ValueOf(a2)
	if v1.Type() != v2.Type() {
		return false
	}
	return e.deepValueEqual(v1, v2, make(map[visit]bool), equateNilAndEmpty, ignoreUnexportedFields, 0)
}

func (e Equalities) deepValueDerive(v1, v2 reflect.Value, visited map[visit]bool, depth int) bool {
	defer makeUsefulPanic(v1)

	if !v1.IsValid() || !v2.IsValid() {
		return v1.IsValid() == v2.IsValid()
	}
	if v1.Type() != v2.Type() {
		return false
	}
	if fv, ok := e[v1.Type()]; ok {
		return fv.Call([]reflect.Value{v1, v2})[0].Bool()
	}

	hard := func(k reflect.Kind) bool {
		switch k {
		case reflect.Array, reflect.Map, reflect.Slice, reflect.Struct:
			return true
		}
		return false
	}

	if v1.CanAddr() && v2.CanAddr() && hard(v1.Kind()) {
		addr1 := v1.UnsafeAddr()
		addr2 := v2.UnsafeAddr()
		if addr1 > addr2 {
			// Canonicalize order to reduce number of entries in visited.
			addr1, addr2 = addr2, addr1
		}

		// Short circuit if references are identical ...
		if addr1 == addr2 {
			return true
		}

		// ... or already seen
		typ := v1.Type()
		v := visit{addr1, addr2, typ}
		if visited[v] {
			return true
		}

		// Remember for later.
		visited[v] = true
	}

	switch v1.Kind() {
	case reflect.Array:
		// We don't need to check length here because length is part of
		// an array's type, which has already been filtered for.
		for i := 0; i < v1.Len(); i++ {
			if !e.deepValueDerive(v1.Index(i), v2.Index(i), visited, depth+1) {
				return false
			}
		}
		return true
	case reflect.Slice:
		if v1.IsNil() || v1.Len() == 0 {
			return true
		}
		if v1.Len() > v2.Len() {
			return false
		}
		if v1.Pointer() == v2.Pointer() {
			return true
		}
		for i := 0; i < v1.Len(); i++ {
			if !e.deepValueDerive(v1.Index(i), v2.Index(i), visited, depth+1) {
				return false
			}
		}
		return true
	case reflect.String:
		if v1.Len() == 0 {
			return true
		}
		if v1.Len() > v2.Len() {
			return false
		}
		return v1.String() == v2.String()
	case reflect.Interface:
		if v1.IsNil() {
			return true
		}
		return e.deepValueDerive(v1.Elem(), v2.Elem(), visited, depth+1)
	case reflect.Pointer:
		if v1.IsNil() {
			return true
		}
		return e.deepValueDerive(v1.Elem(), v2.Elem(), visited, depth+1)
	case reflect.Struct:
		for i, n := 0, v1.NumField(); i < n; i++ {
			if !e.deepValueDerive(v1.Field(i), v2.Field(i), visited, depth+1) {
				return false
			}
		}
		return true
	case reflect.Map:
		if v1.IsNil() || v1.Len() == 0 {
			return true
		}
		if v1.Len() > v2.Len() {
			return false
		}
		if v1.Pointer() == v2.Pointer() {
			return true
		}
		for _, k := range v1.MapKeys() {
			if !e.deepValueDerive(v1.MapIndex(k), v2.MapIndex(k), visited, depth+1) {
				return false
			}
		}
		return true
	case reflect.Func:
		if v1.IsNil() && v2.IsNil() {
			return true
		}
		// Can't do better than this:
		return false
	default:
		// Normal equality suffices
		if !v1.CanInterface() || !v2.CanInterface() {
			panic(unexportedTypePanic{})
		}
		return v1.Interface() == v2.Interface()
	}
}

// DeepDerivative is similar to DeepEqual except that unset fields in a1 are
// ignored (not compared). This allows us to focus on the fields that matter to
// the semantic comparison.
//
// The unset fields include a nil pointer and an empty string.
func (e Equalities) DeepDerivative(a1, a2 interface{}) bool {
	if a1 == nil {
		return true
	}
	v1 := reflect.ValueOf(a1)
	v2 := reflect.ValueOf(a2)
	if v1.Type() != v2.Type() {
		return false
	}
	return e.deepValueDerive(v1, v2, make(map[visit]bool), 0)
}
