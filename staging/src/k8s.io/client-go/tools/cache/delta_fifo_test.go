/*
Copyright 2014 The Kubernetes Authors.

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

package cache

import (
	"fmt"
	"reflect"
	"runtime"
	"testing"
	"time"
)

// List returns a list of all the items; it returns the object
// from the most recent Delta.
// You should treat the items returned inside the deltas as immutable.
// This function was moved here because it is not consistent with normal list semantics, but is used in unit testing.
func (f *DeltaFIFO) List() []interface{} {
	f.lock.RLock()
	defer f.lock.RUnlock()
	return f.listLocked()
}

// This function was moved here because it is not consistent with normal list semantics, but is used in unit testing.
func (f *DeltaFIFO) listLocked() []interface{} {
	list := make([]interface{}, 0, len(f.items))
	for _, item := range f.items {
		list = append(list, item.Newest().Object)
	}
	return list
}

// ListKeys returns a list of all the keys of the objects currently
// in the FIFO.
// This function was moved here because it is not consistent with normal list semantics, but is used in unit testing.
func (f *DeltaFIFO) ListKeys() []string {
	f.lock.RLock()
	defer f.lock.RUnlock()
	list := make([]string, 0, len(f.queue))
	for _, key := range f.queue {
		list = append(list, key)
	}
	return list
}

// Get returns the complete list of deltas for the requested item,
// or sets exists=false.
// You should treat the items returned inside the deltas as immutable.
// This function was moved here because it is not consistent with normal list semantics, but is used in unit testing.
func (f *DeltaFIFO) Get(obj interface{}) (item interface{}, exists bool, err error) {
	key, err := f.KeyOf(obj)
	if err != nil {
		return nil, false, KeyError{obj, err}
	}
	return f.GetByKey(key)
}

// GetByKey returns the complete list of deltas for the requested item,
// setting exists=false if that list is empty.
// You should treat the items returned inside the deltas as immutable.
// This function was moved here because it is not consistent with normal list semantics, but is used in unit testing.
func (f *DeltaFIFO) GetByKey(key string) (item interface{}, exists bool, err error) {
	f.lock.RLock()
	defer f.lock.RUnlock()
	d, exists := f.items[key]
	if exists {
		// Copy item's slice so operations on this slice
		// won't interfere with the object we return.
		d = copyDeltas(d)
	}
	return d, exists, nil
}

// helper function to reduce stuttering
func testPop(f *DeltaFIFO) testFifoObject {
	return Pop(f).(Deltas).Newest().Object.(testFifoObject)
}

// literalListerGetter is a KeyListerGetter that is based on a
// function that returns a slice of objects to list and get.
// The function must list the same objects every time.
type literalListerGetter func() []testFifoObject

var _ KeyListerGetter = literalListerGetter(nil)

// ListKeys just calls kl.
func (kl literalListerGetter) ListKeys() []string {
	result := []string{}
	for _, fifoObj := range kl() {
		result = append(result, fifoObj.name)
	}
	return result
}

// GetByKey returns the key if it exists in the list returned by kl.
func (kl literalListerGetter) GetByKey(key string) (interface{}, bool, error) {
	for _, v := range kl() {
		if v.name == key {
			return v, true, nil
		}
	}
	return nil, false, nil
}

func TestDeltaFIFO_basic(t *testing.T) {
	f := NewDeltaFIFOWithOptions(DeltaFIFOOptions{KeyFunction: testFifoObjectKeyFunc})
	const amount = 500
	go func() {
		for i := 0; i < amount; i++ {
			f.Add(mkFifoObj(string([]rune{'a', rune(i)}), i+1))
		}
	}()
	go func() {
		for u := uint64(0); u < amount; u++ {
			f.Add(mkFifoObj(string([]rune{'b', rune(u)}), u+1))
		}
	}()

	lastInt := int(0)
	lastUint := uint64(0)
	for i := 0; i < amount*2; i++ {
		switch obj := testPop(f).val.(type) {
		case int:
			if obj <= lastInt {
				t.Errorf("got %v (int) out of order, last was %v", obj, lastInt)
			}
			lastInt = obj
		case uint64:
			if obj <= lastUint {
				t.Errorf("got %v (uint) out of order, last was %v", obj, lastUint)
			} else {
				lastUint = obj
			}
		default:
			t.Fatalf("unexpected type %#v", obj)
		}
	}
}

// TestDeltaFIFO_replaceWithDeleteDeltaIn tests that a `Sync` delta for an
// object `O` with ID `X` is added when .Replace is called and `O` is among the
// replacement objects even if the DeltaFIFO already stores in terminal position
// a delta of type `Delete` for ID `X`. Not adding the `Sync` delta causes
// SharedIndexInformers to miss `O`'s create notification, see https://github.com/kubernetes/kubernetes/issues/83810
// for more details.
func TestDeltaFIFO_replaceWithDeleteDeltaIn(t *testing.T) {
	oldObj := mkFifoObj("foo", 1)
	newObj := mkFifoObj("foo", 2)

	f := NewDeltaFIFOWithOptions(DeltaFIFOOptions{
		KeyFunction: testFifoObjectKeyFunc,
		KnownObjects: literalListerGetter(func() []testFifoObject {
			return []testFifoObject{oldObj}
		}),
	})

	f.Delete(oldObj)
	f.Replace([]interface{}{newObj}, "")

	actualDeltas := Pop(f)
	expectedDeltas := Deltas{
		Delta{Type: Deleted, Object: oldObj},
		Delta{Type: Sync, Object: newObj},
	}
	if !reflect.DeepEqual(expectedDeltas, actualDeltas) {
		t.Errorf("expected %#v, got %#v", expectedDeltas, actualDeltas)
	}
}

func TestDeltaFIFOW_ReplaceMakesDeletionsForObjectsOnlyInQueue(t *testing.T) {
	obj := mkFifoObj("foo", 2)
	objV2 := mkFifoObj("foo", 3)
	table := []struct {
		name           string
		operations     func(f *DeltaFIFO)
		expectedDeltas Deltas
	}{
		{
			name: "Added object should be deleted on Replace",
			operations: func(f *DeltaFIFO) {
				f.Add(obj)
				f.Replace([]interface{}{}, "0")
			},
			expectedDeltas: Deltas{
				{Added, obj},
				{Deleted, DeletedFinalStateUnknown{Key: "foo", Obj: obj}},
			},
		},
		{
			name: "Replaced object should have only a single Delete",
			operations: func(f *DeltaFIFO) {
				f.emitDeltaTypeReplaced = true
				f.Add(obj)
				f.Replace([]interface{}{obj}, "0")
				f.Replace([]interface{}{}, "0")
			},
			expectedDeltas: Deltas{
				{Added, obj},
				{Replaced, obj},
				{Deleted, DeletedFinalStateUnknown{Key: "foo", Obj: obj}},
			},
		},
		{
			name: "Deleted object should have only a single Delete",
			operations: func(f *DeltaFIFO) {
				f.Add(obj)
				f.Delete(obj)
				f.Replace([]interface{}{}, "0")
			},
			expectedDeltas: Deltas{
				{Added, obj},
				{Deleted, obj},
			},
		},
		{
			name: "Synced objects should have a single delete",
			operations: func(f *DeltaFIFO) {
				f.Add(obj)
				f.Replace([]interface{}{obj}, "0")
				f.Replace([]interface{}{obj}, "0")
				f.Replace([]interface{}{}, "0")
			},
			expectedDeltas: Deltas{
				{Added, obj},
				{Sync, obj},
				{Sync, obj},
				{Deleted, DeletedFinalStateUnknown{Key: "foo", Obj: obj}},
			},
		},
		{
			name: "Added objects should have a single delete on multiple Replaces",
			operations: func(f *DeltaFIFO) {
				f.Add(obj)
				f.Replace([]interface{}{}, "0")
				f.Replace([]interface{}{}, "1")
			},
			expectedDeltas: Deltas{
				{Added, obj},
				{Deleted, DeletedFinalStateUnknown{Key: "foo", Obj: obj}},
			},
		},
		{
			name: "Added and deleted and added object should be deleted",
			operations: func(f *DeltaFIFO) {
				f.Add(obj)
				f.Delete(obj)
				f.Add(objV2)
				f.Replace([]interface{}{}, "0")
			},
			expectedDeltas: Deltas{
				{Added, obj},
				{Deleted, obj},
				{Added, objV2},
				{Deleted, DeletedFinalStateUnknown{Key: "foo", Obj: objV2}},
			},
		},
	}
	for _, tt := range table {
		tt := tt

		t.Run(tt.name, func(t *testing.T) {
			// Test with a DeltaFIFO with a backing KnownObjects
			fWithKnownObjects := NewDeltaFIFOWithOptions(DeltaFIFOOptions{
				KeyFunction: testFifoObjectKeyFunc,
				KnownObjects: literalListerGetter(func() []testFifoObject {
					return []testFifoObject{}
				}),
			})
			tt.operations(fWithKnownObjects)
			actualDeltasWithKnownObjects := Pop(fWithKnownObjects)
			if !reflect.DeepEqual(tt.expectedDeltas, actualDeltasWithKnownObjects) {
				t.Errorf("expected %#v, got %#v", tt.expectedDeltas, actualDeltasWithKnownObjects)
			}
			if len(fWithKnownObjects.items) != 0 {
				t.Errorf("expected no extra deltas (empty map), got %#v", fWithKnownObjects.items)
			}

			// Test with a DeltaFIFO without a backing KnownObjects
			fWithoutKnownObjects := NewDeltaFIFOWithOptions(DeltaFIFOOptions{
				KeyFunction: testFifoObjectKeyFunc,
			})
			tt.operations(fWithoutKnownObjects)
			actualDeltasWithoutKnownObjects := Pop(fWithoutKnownObjects)
			if !reflect.DeepEqual(tt.expectedDeltas, actualDeltasWithoutKnownObjects) {
				t.Errorf("expected %#v, got %#v", tt.expectedDeltas, actualDeltasWithoutKnownObjects)
			}
			if len(fWithoutKnownObjects.items) != 0 {
				t.Errorf("expected no extra deltas (empty map), got %#v", fWithoutKnownObjects.items)
			}
		})
	}
}

func TestDeltaFIFO_addUpdate(t *testing.T) {
	f := NewDeltaFIFOWithOptions(DeltaFIFOOptions{KeyFunction: testFifoObjectKeyFunc})
	f.Add(mkFifoObj("foo", 10))
	f.Update(mkFifoObj("foo", 12))
	f.Delete(mkFifoObj("foo", 15))

	if e, a := []interface{}{mkFifoObj("foo", 15)}, f.List(); !reflect.DeepEqual(e, a) {
		t.Errorf("Expected %+v, got %+v", e, a)
	}
	if e, a := []string{"foo"}, f.ListKeys(); !reflect.DeepEqual(e, a) {
		t.Errorf("Expected %+v, got %+v", e, a)
	}

	got := make(chan testFifoObject, 2)
	go func() {
		for {
			obj := testPop(f)
			t.Logf("got a thing %#v", obj)
			t.Logf("D len: %v", len(f.queue))
			got <- obj
		}
	}()

	first := <-got
	if e, a := 15, first.val; e != a {
		t.Errorf("Didn't get updated value (%v), got %v", e, a)
	}
	select {
	case unexpected := <-got:
		t.Errorf("Got second value %v", unexpected.val)
	case <-time.After(50 * time.Millisecond):
	}
	_, exists, _ := f.Get(mkFifoObj("foo", ""))
	if exists {
		t.Errorf("item did not get removed")
	}
}

type rvAndXfrm struct {
	rv   int
	xfrm int
}

func TestDeltaFIFO_transformer(t *testing.T) {
	mk := func(name string, rv int) testFifoObject {
		return mkFifoObj(name, &rvAndXfrm{rv, 0})
	}
	xfrm := TransformFunc(func(obj interface{}) (interface{}, error) {
		switch v := obj.(type) {
		case testFifoObject:
			v.val.(*rvAndXfrm).xfrm++
		case DeletedFinalStateUnknown:
			if x := v.Obj.(testFifoObject).val.(*rvAndXfrm).xfrm; x != 1 {
				return nil, fmt.Errorf("object has been transformed wrong number of times: %#v", obj)
			}
		default:
			return nil, fmt.Errorf("unexpected object: %#v", obj)
		}
		return obj, nil
	})

	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}

	f := NewDeltaFIFOWithOptions(DeltaFIFOOptions{
		KeyFunction: testFifoObjectKeyFunc,
		Transformer: xfrm,
	})
	must(f.Add(mk("foo", 10)))
	must(f.Add(mk("bar", 11)))
	must(f.Update(mk("foo", 12)))
	must(f.Delete(mk("foo", 15)))
	must(f.Replace([]interface{}{}, ""))
	must(f.Add(mk("bar", 16)))
	must(f.Replace([]interface{}{}, ""))

	// Should be empty
	if e, a := []string{"foo", "bar"}, f.ListKeys(); !reflect.DeepEqual(e, a) {
		t.Errorf("Expected %+v, got %+v", e, a)
	}

	for i := 0; i < 2; i++ {
		obj, err := f.Pop(func(o interface{}, isInInitialList bool) error { return nil })
		if err != nil {
			t.Fatalf("got nothing on try %v?", i)
		}
		obj = obj.(Deltas).Newest().Object
		switch v := obj.(type) {
		case testFifoObject:
			if v.name != "foo" {
				t.Errorf("expected regular deletion of foo, got %q", v.name)
			}
			rx := v.val.(*rvAndXfrm)
			if rx.rv != 15 {
				t.Errorf("expected last message, got %#v", obj)
			}
			if rx.xfrm != 1 {
				t.Errorf("obj %v transformed wrong number of times.", obj)
			}
		case DeletedFinalStateUnknown:
			tf := v.Obj.(testFifoObject)
			rx := tf.val.(*rvAndXfrm)
			if tf.name != "bar" {
				t.Errorf("expected tombstone deletion of bar, got %q", tf.name)
			}
			if rx.rv != 16 {
				t.Errorf("expected last message, got %#v", obj)
			}
			if rx.xfrm != 1 {
				t.Errorf("tombstoned obj %v transformed wrong number of times.", obj)
			}
		default:
			t.Errorf("unknown item %#v", obj)
		}
	}
}

func TestDeltaFIFO_enqueueingNoLister(t *testing.T) {
	f := NewDeltaFIFOWithOptions(DeltaFIFOOptions{KeyFunction: testFifoObjectKeyFunc})
	f.Add(mkFifoObj("foo", 10))
	f.Update(mkFifoObj("bar", 15))
	f.Add(mkFifoObj("qux", 17))
	f.Delete(mkFifoObj("qux", 18))

	// This delete does not enqueue anything because baz doesn't exist.
	f.Delete(mkFifoObj("baz", 20))

	expectList := []int{10, 15, 18}
	for _, expect := range expectList {
		if e, a := expect, testPop(f).val; e != a {
			t.Errorf("Didn't get updated value (%v), got %v", e, a)
		}
	}
	if e, a := 0, len(f.items); e != a {
		t.Errorf("queue unexpectedly not empty: %v != %v\n%#v", e, a, f.items)
	}
}

func TestDeltaFIFO_enqueueingWithLister(t *testing.T) {
	f := NewDeltaFIFOWithOptions(DeltaFIFOOptions{
		KeyFunction: testFifoObjectKeyFunc,
		KnownObjects: literalListerGetter(func() []testFifoObject {
			return []testFifoObject{mkFifoObj("foo", 5), mkFifoObj("bar", 6), mkFifoObj("baz", 7)}
		}),
	})
	f.Add(mkFifoObj("foo", 10))
	f.Update(mkFifoObj("bar", 15))

	// This delete does enqueue the deletion, because "baz" is in the key lister.
	f.Delete(mkFifoObj("baz", 20))

	expectList := []int{10, 15, 20}
	for _, expect := range expectList {
		if e, a := expect, testPop(f).val; e != a {
			t.Errorf("Didn't get updated value (%v), got %v", e, a)
		}
	}
	if e, a := 0, len(f.items); e != a {
		t.Errorf("queue unexpectedly not empty: %v != %v", e, a)
	}
}

func TestDeltaFIFO_addReplace(t *testing.T) {
	f := NewDeltaFIFOWithOptions(DeltaFIFOOptions{KeyFunction: testFifoObjectKeyFunc})
	f.Add(mkFifoObj("foo", 10))
	f.Replace([]interface{}{mkFifoObj("foo", 15)}, "0")
	got := make(chan testFifoObject, 2)
	go func() {
		for {
			got <- testPop(f)
		}
	}()

	first := <-got
	if e, a := 15, first.val; e != a {
		t.Errorf("Didn't get updated value (%v), got %v", e, a)
	}
	select {
	case unexpected := <-got:
		t.Errorf("Got second value %v", unexpected.val)
	case <-time.After(50 * time.Millisecond):
	}
	_, exists, _ := f.Get(mkFifoObj("foo", ""))
	if exists {
		t.Errorf("item did not get removed")
	}
}

func TestDeltaFIFO_ResyncNonExisting(t *testing.T) {
	f := NewDeltaFIFOWithOptions(DeltaFIFOOptions{
		KeyFunction: testFifoObjectKeyFunc,
		KnownObjects: literalListerGetter(func() []testFifoObject {
			return []testFifoObject{mkFifoObj("foo", 5)}
		}),
	})
	f.Delete(mkFifoObj("foo", 10))
	f.Resync()

	deltas := f.items["foo"]
	if len(deltas) != 1 {
		t.Fatalf("unexpected deltas length: %v", deltas)
	}
	if deltas[0].Type != Deleted {
		t.Errorf("unexpected delta: %v", deltas[0])
	}
}

func TestDeltaFIFO_Resync(t *testing.T) {
	f := NewDeltaFIFOWithOptions(DeltaFIFOOptions{
		KeyFunction: testFifoObjectKeyFunc,
		KnownObjects: literalListerGetter(func() []testFifoObject {
			return []testFifoObject{mkFifoObj("foo", 5)}
		}),
	})
	f.Resync()

	deltas := f.items["foo"]
	if len(deltas) != 1 {
		t.Fatalf("unexpected deltas length: %v", deltas)
	}
	if deltas[0].Type != Sync {
		t.Errorf("unexpected delta: %v", deltas[0])
	}
}

func TestDeltaFIFO_DeleteExistingNonPropagated(t *testing.T) {
	f := NewDeltaFIFOWithOptions(DeltaFIFOOptions{
		KeyFunction: testFifoObjectKeyFunc,
		KnownObjects: literalListerGetter(func() []testFifoObject {
			return []testFifoObject{}
		}),
	})
	f.Add(mkFifoObj("foo", 5))
	f.Delete(mkFifoObj("foo", 6))

	deltas := f.items["foo"]
	if len(deltas) != 2 {
		t.Fatalf("unexpected deltas length: %v", deltas)
	}
	if deltas[len(deltas)-1].Type != Deleted {
		t.Errorf("unexpected delta: %v", deltas[len(deltas)-1])
	}
}

func TestDeltaFIFO_ReplaceMakesDeletions(t *testing.T) {
	// We test with only one pre-existing object because there is no
	// promise about how their deletes are ordered.

	// Try it with a pre-existing Delete
	f := NewDeltaFIFOWithOptions(DeltaFIFOOptions{
		KeyFunction: testFifoObjectKeyFunc,
		KnownObjects: literalListerGetter(func() []testFifoObject {
			return []testFifoObject{mkFifoObj("foo", 5), mkFifoObj("bar", 6), mkFifoObj("baz", 7)}
		}),
	})
	f.Delete(mkFifoObj("baz", 10))
	f.Replace([]interface{}{mkFifoObj("foo", 5)}, "0")

	expectedList := []Deltas{
		{{Deleted, mkFifoObj("baz", 10)}},
		{{Sync, mkFifoObj("foo", 5)}},
		// Since "bar" didn't have a delete event and wasn't in the Replace list
		// it should get a tombstone key with the right Obj.
		{{Deleted, DeletedFinalStateUnknown{Key: "bar", Obj: mkFifoObj("bar", 6)}}},
	}

	for _, expected := range expectedList {
		cur := Pop(f).(Deltas)
		if e, a := expected, cur; !reflect.DeepEqual(e, a) {
			t.Errorf("Expected %#v, got %#v", e, a)
		}
	}

	// Now try starting with an Add instead of a Delete
	f = NewDeltaFIFOWithOptions(DeltaFIFOOptions{
		KeyFunction: testFifoObjectKeyFunc,
		KnownObjects: literalListerGetter(func() []testFifoObject {
			return []testFifoObject{mkFifoObj("foo", 5), mkFifoObj("bar", 6), mkFifoObj("baz", 7)}
		}),
	})
	f.Add(mkFifoObj("baz", 10))
	f.Replace([]interface{}{mkFifoObj("foo", 5)}, "0")

	expectedList = []Deltas{
		{{Added, mkFifoObj("baz", 10)},
			{Deleted, DeletedFinalStateUnknown{Key: "baz", Obj: mkFifoObj("baz", 10)}}},
		{{Sync, mkFifoObj("foo", 5)}},
		// Since "bar" didn't have a delete event and wasn't in the Replace list
		// it should get a tombstone key with the right Obj.
		{{Deleted, DeletedFinalStateUnknown{Key: "bar", Obj: mkFifoObj("bar", 6)}}},
	}

	for _, expected := range expectedList {
		cur := Pop(f).(Deltas)
		if e, a := expected, cur; !reflect.DeepEqual(e, a) {
			t.Errorf("Expected %#v, got %#v", e, a)
		}
	}

	// Now try deleting and recreating the object in the queue, then delete it by a Replace call
	f = NewDeltaFIFOWithOptions(DeltaFIFOOptions{
		KeyFunction: testFifoObjectKeyFunc,
		KnownObjects: literalListerGetter(func() []testFifoObject {
			return []testFifoObject{mkFifoObj("foo", 5), mkFifoObj("bar", 6), mkFifoObj("baz", 7)}
		}),
	})
	f.Delete(mkFifoObj("bar", 6))
	f.Add(mkFifoObj("bar", 100))
	f.Replace([]interface{}{mkFifoObj("foo", 5)}, "0")

	expectedList = []Deltas{
		{
			{Deleted, mkFifoObj("bar", 6)},
			{Added, mkFifoObj("bar", 100)},
			// Since "bar" has a newer object in the queue than in the state,
			// it should get a tombstone key with the latest object from the queue
			{Deleted, DeletedFinalStateUnknown{Key: "bar", Obj: mkFifoObj("bar", 100)}},
		},
		{{Sync, mkFifoObj("foo", 5)}},
		{{Deleted, DeletedFinalStateUnknown{Key: "baz", Obj: mkFifoObj("baz", 7)}}},
	}

	for _, expected := range expectedList {
		cur := Pop(f).(Deltas)
		if e, a := expected, cur; !reflect.DeepEqual(e, a) {
			t.Errorf("Expected %#v, got %#v", e, a)
		}
	}

	// Now try syncing it first to ensure the delete use the latest version
	f = NewDeltaFIFOWithOptions(DeltaFIFOOptions{
		KeyFunction: testFifoObjectKeyFunc,
		KnownObjects: literalListerGetter(func() []testFifoObject {
			return []testFifoObject{mkFifoObj("foo", 5), mkFifoObj("bar", 6), mkFifoObj("baz", 7)}
		}),
	})
	f.Replace([]interface{}{mkFifoObj("bar", 100), mkFifoObj("foo", 5)}, "0")
	f.Replace([]interface{}{mkFifoObj("foo", 5)}, "0")

	expectedList = []Deltas{
		{
			{Sync, mkFifoObj("bar", 100)},
			// Since "bar" didn't have a delete event and wasn't in the Replace list
			// it should get a tombstone key with the right Obj.
			{Deleted, DeletedFinalStateUnknown{Key: "bar", Obj: mkFifoObj("bar", 100)}},
		},
		{
			{Sync, mkFifoObj("foo", 5)},
			{Sync, mkFifoObj("foo", 5)},
		},
		{{Deleted, DeletedFinalStateUnknown{Key: "baz", Obj: mkFifoObj("baz", 7)}}},
	}

	for _, expected := range expectedList {
		cur := Pop(f).(Deltas)
		if e, a := expected, cur; !reflect.DeepEqual(e, a) {
			t.Errorf("Expected %#v, got %#v", e, a)
		}
	}

	// Now try starting without an explicit KeyListerGetter
	f = NewDeltaFIFOWithOptions(DeltaFIFOOptions{KeyFunction: testFifoObjectKeyFunc})
	f.Add(mkFifoObj("baz", 10))
	f.Replace([]interface{}{mkFifoObj("foo", 5)}, "0")

	expectedList = []Deltas{
		{{Added, mkFifoObj("baz", 10)},
			{Deleted, DeletedFinalStateUnknown{Key: "baz", Obj: mkFifoObj("baz", 10)}}},
		{{Sync, mkFifoObj("foo", 5)}},
	}

	for _, expected := range expectedList {
		cur := Pop(f).(Deltas)
		if e, a := expected, cur; !reflect.DeepEqual(e, a) {
			t.Errorf("Expected %#v, got %#v", e, a)
		}
	}
}

// TestDeltaFIFO_ReplaceMakesDeletionsReplaced is the same as the above test, but
// ensures that a Replaced DeltaType is emitted.
func TestDeltaFIFO_ReplaceMakesDeletionsReplaced(t *testing.T) {
	f := NewDeltaFIFOWithOptions(DeltaFIFOOptions{
		KeyFunction: testFifoObjectKeyFunc,
		KnownObjects: literalListerGetter(func() []testFifoObject {
			return []testFifoObject{mkFifoObj("foo", 5), mkFifoObj("bar", 6), mkFifoObj("baz", 7)}
		}),
		EmitDeltaTypeReplaced: true,
	})

	f.Delete(mkFifoObj("baz", 10))
	f.Replace([]interface{}{mkFifoObj("foo", 6)}, "0")

	expectedList := []Deltas{
		{{Deleted, mkFifoObj("baz", 10)}},
		{{Replaced, mkFifoObj("foo", 6)}},
		// Since "bar" didn't have a delete event and wasn't in the Replace list
		// it should get a tombstone key with the right Obj.
		{{Deleted, DeletedFinalStateUnknown{Key: "bar", Obj: mkFifoObj("bar", 6)}}},
	}

	for _, expected := range expectedList {
		cur := Pop(f).(Deltas)
		if e, a := expected, cur; !reflect.DeepEqual(e, a) {
			t.Errorf("Expected %#v, got %#v", e, a)
		}
	}
}

// TestDeltaFIFO_ReplaceDeltaType checks that passing EmitDeltaTypeReplaced
// means that Replaced is correctly emitted.
func TestDeltaFIFO_ReplaceDeltaType(t *testing.T) {
	f := NewDeltaFIFOWithOptions(DeltaFIFOOptions{
		KeyFunction: testFifoObjectKeyFunc,
		KnownObjects: literalListerGetter(func() []testFifoObject {
			return []testFifoObject{mkFifoObj("foo", 5)}
		}),
		EmitDeltaTypeReplaced: true,
	})
	f.Replace([]interface{}{mkFifoObj("foo", 5)}, "0")

	expectedList := []Deltas{
		{{Replaced, mkFifoObj("foo", 5)}},
	}

	for _, expected := range expectedList {
		cur := Pop(f).(Deltas)
		if e, a := expected, cur; !reflect.DeepEqual(e, a) {
			t.Errorf("Expected %#v, got %#v", e, a)
		}
	}
}

func TestDeltaFIFO_UpdateResyncRace(t *testing.T) {
	f := NewDeltaFIFOWithOptions(DeltaFIFOOptions{
		KeyFunction: testFifoObjectKeyFunc,
		KnownObjects: literalListerGetter(func() []testFifoObject {
			return []testFifoObject{mkFifoObj("foo", 5)}
		}),
	})
	f.Update(mkFifoObj("foo", 6))
	f.Resync()

	expectedList := []Deltas{
		{{Updated, mkFifoObj("foo", 6)}},
	}

	for _, expected := range expectedList {
		cur := Pop(f).(Deltas)
		if e, a := expected, cur; !reflect.DeepEqual(e, a) {
			t.Errorf("Expected %#v, got %#v", e, a)
		}
	}
}

// pop2 captures both parameters, unlike Pop().
func pop2[T any](queue Queue) (T, bool) {
	var result interface{}
	var isList bool
	queue.Pop(func(obj interface{}, isInInitialList bool) error {
		result = obj
		isList = isInInitialList
		return nil
	})
	return result.(T), isList
}

func TestDeltaFIFO_HasSyncedCorrectOnDeletion(t *testing.T) {
	f := NewDeltaFIFOWithOptions(DeltaFIFOOptions{
		KeyFunction: testFifoObjectKeyFunc,
		KnownObjects: literalListerGetter(func() []testFifoObject {
			return []testFifoObject{mkFifoObj("foo", 5), mkFifoObj("bar", 6), mkFifoObj("baz", 7)}
		}),
	})
	f.Replace([]interface{}{mkFifoObj("foo", 5)}, "0")

	expectedList := []Deltas{
		{{Sync, mkFifoObj("foo", 5)}},
		// Since "bar" didn't have a delete event and wasn't in the Replace list
		// it should get a tombstone key with the right Obj.
		{{Deleted, DeletedFinalStateUnknown{Key: "bar", Obj: mkFifoObj("bar", 6)}}},
		{{Deleted, DeletedFinalStateUnknown{Key: "baz", Obj: mkFifoObj("baz", 7)}}},
	}

	for _, expected := range expectedList {
		if f.HasSynced() {
			t.Errorf("Expected HasSynced to be false")
		}
		cur, initial := pop2[Deltas](f)
		if e, a := expected, cur; !reflect.DeepEqual(e, a) {
			t.Errorf("Expected %#v, got %#v", e, a)
		}
		if initial != true {
			t.Error("Expected initial list item")
		}
	}
	if !f.HasSynced() {
		t.Errorf("Expected HasSynced to be true")
	}
}

func TestDeltaFIFO_detectLineJumpers(t *testing.T) {
	f := NewDeltaFIFOWithOptions(DeltaFIFOOptions{KeyFunction: testFifoObjectKeyFunc})

	f.Add(mkFifoObj("foo", 10))
	f.Add(mkFifoObj("bar", 1))
	f.Add(mkFifoObj("foo", 11))
	f.Add(mkFifoObj("foo", 13))
	f.Add(mkFifoObj("zab", 30))

	if e, a := 13, testPop(f).val; a != e {
		t.Fatalf("expected %d, got %d", e, a)
	}

	f.Add(mkFifoObj("foo", 14)) // ensure foo doesn't jump back in line

	if e, a := 1, testPop(f).val; a != e {
		t.Fatalf("expected %d, got %d", e, a)
	}

	if e, a := 30, testPop(f).val; a != e {
		t.Fatalf("expected %d, got %d", e, a)
	}

	if e, a := 14, testPop(f).val; a != e {
		t.Fatalf("expected %d, got %d", e, a)
	}
}

func TestDeltaFIFO_KeyOf(t *testing.T) {
	f := DeltaFIFO{keyFunc: testFifoObjectKeyFunc}

	table := []struct {
		obj interface{}
		key string
	}{
		{obj: testFifoObject{name: "A"}, key: "A"},
		{obj: DeletedFinalStateUnknown{Key: "B", Obj: nil}, key: "B"},
		{obj: Deltas{{Object: testFifoObject{name: "C"}}}, key: "C"},
		{obj: Deltas{{Object: DeletedFinalStateUnknown{Key: "D", Obj: nil}}}, key: "D"},
	}

	for _, item := range table {
		got, err := f.KeyOf(item.obj)
		if err != nil {
			t.Errorf("Unexpected error for %q: %v", item.obj, err)
			continue
		}
		if e, a := item.key, got; e != a {
			t.Errorf("Expected %v, got %v", e, a)
		}
	}
}

func TestDeltaFIFO_HasSynced(t *testing.T) {
	tests := []struct {
		actions        []func(f *DeltaFIFO)
		expectedSynced bool
	}{
		{
			actions:        []func(f *DeltaFIFO){},
			expectedSynced: false,
		},
		{
			actions: []func(f *DeltaFIFO){
				func(f *DeltaFIFO) { f.Add(mkFifoObj("a", 1)) },
			},
			expectedSynced: true,
		},
		{
			actions: []func(f *DeltaFIFO){
				func(f *DeltaFIFO) { f.Replace([]interface{}{}, "0") },
			},
			expectedSynced: true,
		},
		{
			actions: []func(f *DeltaFIFO){
				func(f *DeltaFIFO) { f.Replace([]interface{}{mkFifoObj("a", 1), mkFifoObj("b", 2)}, "0") },
			},
			expectedSynced: false,
		},
		{
			actions: []func(f *DeltaFIFO){
				func(f *DeltaFIFO) { f.Replace([]interface{}{mkFifoObj("a", 1), mkFifoObj("b", 2)}, "0") },
				func(f *DeltaFIFO) { Pop(f) },
			},
			expectedSynced: false,
		},
		{
			actions: []func(f *DeltaFIFO){
				func(f *DeltaFIFO) { f.Replace([]interface{}{mkFifoObj("a", 1), mkFifoObj("b", 2)}, "0") },
				func(f *DeltaFIFO) { Pop(f) },
				func(f *DeltaFIFO) { Pop(f) },
			},
			expectedSynced: true,
		},
		{
			// This test case won't happen in practice since a Reflector, the only producer for delta_fifo today, always passes a complete snapshot consistent in time;
			// there cannot be duplicate keys in the list or apiserver is broken.
			actions: []func(f *DeltaFIFO){
				func(f *DeltaFIFO) { f.Replace([]interface{}{mkFifoObj("a", 1), mkFifoObj("a", 2)}, "0") },
				func(f *DeltaFIFO) { Pop(f) },
			},
			expectedSynced: true,
		},
	}

	for i, test := range tests {
		f := NewDeltaFIFOWithOptions(DeltaFIFOOptions{KeyFunction: testFifoObjectKeyFunc})

		for _, action := range test.actions {
			action(f)
		}
		if e, a := test.expectedSynced, f.HasSynced(); a != e {
			t.Errorf("test case %v failed, expected: %v , got %v", i, e, a)
		}
	}
}

// TestDeltaFIFO_PopShouldUnblockWhenClosed checks that any blocking Pop on an empty queue
// should unblock and return after Close is called.
func TestDeltaFIFO_PopShouldUnblockWhenClosed(t *testing.T) {
	f := NewDeltaFIFOWithOptions(DeltaFIFOOptions{
		KeyFunction: testFifoObjectKeyFunc,
		KnownObjects: literalListerGetter(func() []testFifoObject {
			return []testFifoObject{mkFifoObj("foo", 5)}
		}),
	})

	c := make(chan struct{})
	const jobs = 10
	for i := 0; i < jobs; i++ {
		go func() {
			f.Pop(func(obj interface{}, isInInitialList bool) error {
				return nil
			})
			c <- struct{}{}
		}()
	}

	runtime.Gosched()
	f.Close()

	for i := 0; i < jobs; i++ {
		select {
		case <-c:
		case <-time.After(500 * time.Millisecond):
			t.Fatalf("timed out waiting for Pop to return after Close")
		}
	}
}

func TestDeltaFIFO_createListerStateThatNeverExisted(t *testing.T) {
	order1_X_create := testFifoObject{
		name: "x",
		val:  "no ip",
	}
	order2_X_assignIP := testFifoObject{
		name: "x",
		val:  "ip=6",
	}
	order3_Y_create := testFifoObject{
		name: "y",
		val:  "no ip",
	}
	order4_X_delete := testFifoObject{
		name: "x",
		val:  "delete",
	}
	order5_Y_assignIP := testFifoObject{
		name: "y",
		val:  "ip=6",
	}
	// taken together, these event, in order, never have X and Y with the same IP at the same time
	// The challenge in this test is to use this ordering with nothing interrupting, but time to produce a situation
	// where the observer believes that X and Y have the same IP at the same time, which never actually happened.

	keyToVal := map[string]string{}
	processFn := func(deltaObj interface{}, isInInitialList bool) error {
		if deltas, ok := deltaObj.(Deltas); ok {
			// from oldest to newest
			for _, d := range deltas {
				obj := d.Object

				switch d.Type {
				case Sync, Replaced, Added, Updated:
					keyToVal[obj.(testFifoObject).name] = obj.(testFifoObject).val.(string)

				case Deleted:
					delete(keyToVal, obj.(testFifoObject).name)
				}
			}
		}
		return nil
	}

	f := NewDeltaFIFOWithOptions(DeltaFIFOOptions{
		KeyFunction: testFifoObjectKeyFunc,
		KnownObjects: literalListerGetter(func() []testFifoObject {
			ret := []testFifoObject{}
			for k, v := range keyToVal {
				ret = append(ret, testFifoObject{
					name: k,
					val:  v,
				})
			}
			return ret
		}),
	})

	if err := f.Add(order1_X_create); err != nil {
		t.Fatal(err)
	}
	if err := f.Update(order2_X_assignIP); err != nil {
		t.Fatal(err)
	}

	// consume the events for x
	if _, err := f.Pop(processFn); err != nil {
		t.Fatal(err)
	}
	if e, a := "ip=6", keyToVal["x"]; e != a {
		t.Fatalf("expected %q, got %q", e, a)
	}

	if err := f.Add(order3_Y_create); err != nil {
		t.Fatal(err)
	}
	if err := f.Delete(order4_X_delete); err != nil {
		t.Fatal(err)
	}
	if err := f.Update(order5_Y_assignIP); err != nil {
		t.Fatal(err)
	}

	// consume the events for y
	if _, err := f.Pop(processFn); err != nil {
		t.Fatal(err)
	}
	// this verification means we have a lister state that never actually existed
	if e, a := "ip=6", keyToVal["x"]; e != a {
		t.Fatalf("expected %q, got %q", e, a)
	}
	if e, a := "ip=6", keyToVal["y"]; e != a {
		t.Fatalf("expected %q, got %q", e, a)
	}

	if _, err := f.Pop(processFn); err != nil {
		t.Fatal(err)
	}
	if e, a := "", keyToVal["x"]; e != a {
		t.Fatalf("expected %q, got %q", e, a)
	}
	if e, a := "ip=6", keyToVal["y"]; e != a {
		t.Fatalf("expected %q, got %q", e, a)
	}
}

func BenchmarkDeltaFIFOListKeys(b *testing.B) {
	f := NewDeltaFIFOWithOptions(DeltaFIFOOptions{KeyFunction: testFifoObjectKeyFunc})
	const amount = 10000

	for i := 0; i < amount; i++ {
		f.Add(mkFifoObj(string([]rune{'a', rune(i)}), i+1))
	}
	for u := uint64(0); u < amount; u++ {
		f.Add(mkFifoObj(string([]rune{'b', rune(u)}), u+1))
	}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_ = f.ListKeys()
		}
	})
	b.StopTimer()
}
