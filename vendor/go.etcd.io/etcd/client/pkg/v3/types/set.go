// Copyright 2015 The etcd Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package types

import (
	"reflect"
	"sort"
	"sync"
)

type Set interface {
	Add(string)
	Remove(string)
	Contains(string) bool
	Equals(Set) bool
	Length() int
	Values() []string
	Copy() Set
	Sub(Set) Set
}

func NewUnsafeSet(values ...string) *unsafeSet {
	set := &unsafeSet{make(map[string]struct{})}
	for _, v := range values {
		set.Add(v)
	}
	return set
}

func NewThreadsafeSet(values ...string) *tsafeSet {
	us := NewUnsafeSet(values...)
	return &tsafeSet{us, sync.RWMutex{}}
}

type unsafeSet struct {
	d map[string]struct{}
}

// Add adds a new value to the set (no-op if the value is already present)
func (us *unsafeSet) Add(value string) {
	us.d[value] = struct{}{}
}

// Remove removes the given value from the set
func (us *unsafeSet) Remove(value string) {
	delete(us.d, value)
}

// Contains returns whether the set contains the given value
func (us *unsafeSet) Contains(value string) (exists bool) {
	_, exists = us.d[value]
	return exists
}

// ContainsAll returns whether the set contains all given values
func (us *unsafeSet) ContainsAll(values []string) bool {
	for _, s := range values {
		if !us.Contains(s) {
			return false
		}
	}
	return true
}

// Equals returns whether the contents of two sets are identical
func (us *unsafeSet) Equals(other Set) bool {
	v1 := sort.StringSlice(us.Values())
	v2 := sort.StringSlice(other.Values())
	v1.Sort()
	v2.Sort()
	return reflect.DeepEqual(v1, v2)
}

// Length returns the number of elements in the set
func (us *unsafeSet) Length() int {
	return len(us.d)
}

// Values returns the values of the Set in an unspecified order.
func (us *unsafeSet) Values() (values []string) {
	values = make([]string, 0, len(us.d))
	for val := range us.d {
		values = append(values, val)
	}
	return values
}

// Copy creates a new Set containing the values of the first
func (us *unsafeSet) Copy() Set {
	cp := NewUnsafeSet()
	for val := range us.d {
		cp.Add(val)
	}

	return cp
}

// Sub removes all elements in other from the set
func (us *unsafeSet) Sub(other Set) Set {
	oValues := other.Values()
	result := us.Copy().(*unsafeSet)

	for _, val := range oValues {
		if _, ok := result.d[val]; !ok {
			continue
		}
		delete(result.d, val)
	}

	return result
}

type tsafeSet struct {
	us *unsafeSet
	m  sync.RWMutex
}

func (ts *tsafeSet) Add(value string) {
	ts.m.Lock()
	defer ts.m.Unlock()
	ts.us.Add(value)
}

func (ts *tsafeSet) Remove(value string) {
	ts.m.Lock()
	defer ts.m.Unlock()
	ts.us.Remove(value)
}

func (ts *tsafeSet) Contains(value string) (exists bool) {
	ts.m.RLock()
	defer ts.m.RUnlock()
	return ts.us.Contains(value)
}

func (ts *tsafeSet) Equals(other Set) bool {
	ts.m.RLock()
	defer ts.m.RUnlock()

	// If ts and other represent the same variable, avoid calling
	// ts.us.Equals(other), to avoid double RLock bug
	if _other, ok := other.(*tsafeSet); ok {
		if _other == ts {
			return true
		}
	}
	return ts.us.Equals(other)
}

func (ts *tsafeSet) Length() int {
	ts.m.RLock()
	defer ts.m.RUnlock()
	return ts.us.Length()
}

func (ts *tsafeSet) Values() (values []string) {
	ts.m.RLock()
	defer ts.m.RUnlock()
	return ts.us.Values()
}

func (ts *tsafeSet) Copy() Set {
	ts.m.RLock()
	defer ts.m.RUnlock()
	usResult := ts.us.Copy().(*unsafeSet)
	return &tsafeSet{usResult, sync.RWMutex{}}
}

func (ts *tsafeSet) Sub(other Set) Set {
	ts.m.RLock()
	defer ts.m.RUnlock()

	// If ts and other represent the same variable, avoid calling
	// ts.us.Sub(other), to avoid double RLock bug
	if _other, ok := other.(*tsafeSet); ok {
		if _other == ts {
			usResult := NewUnsafeSet()
			return &tsafeSet{usResult, sync.RWMutex{}}
		}
	}
	usResult := ts.us.Sub(other).(*unsafeSet)
	return &tsafeSet{usResult, sync.RWMutex{}}
}
