// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package attribute // import "go.opentelemetry.io/otel/attribute"

import (
	"cmp"
	"encoding/json"
	"reflect"
	"slices"
	"sort"
)

type (
	// Set is the representation for a distinct attribute set. It manages an
	// immutable set of attributes, with an internal cache for storing
	// attribute encodings.
	//
	// This type will remain comparable for backwards compatibility. The
	// equivalence of Sets across versions is not guaranteed to be stable.
	// Prior versions may find two Sets to be equal or not when compared
	// directly (i.e. ==), but subsequent versions may not. Users should use
	// the Equals method to ensure stable equivalence checking.
	//
	// Users should also use the Distinct returned from Equivalent as a map key
	// instead of a Set directly. In addition to that type providing guarantees
	// on stable equivalence, it may also provide performance improvements.
	Set struct {
		equivalent Distinct
	}

	// Distinct is a unique identifier of a Set.
	//
	// Distinct is designed to be ensures equivalence stability: comparisons
	// will return the save value across versions. For this reason, Distinct
	// should always be used as a map key instead of a Set.
	Distinct struct {
		iface interface{}
	}

	// Sortable implements sort.Interface, used for sorting KeyValue.
	//
	// Deprecated: This type is no longer used. It was added as a performance
	// optimization for Go < 1.21 that is no longer needed (Go < 1.21 is no
	// longer supported by the module).
	Sortable []KeyValue
)

var (
	// keyValueType is used in computeDistinctReflect.
	keyValueType = reflect.TypeOf(KeyValue{})

	// emptySet is returned for empty attribute sets.
	emptySet = &Set{
		equivalent: Distinct{
			iface: [0]KeyValue{},
		},
	}
)

// EmptySet returns a reference to a Set with no elements.
//
// This is a convenience provided for optimized calling utility.
func EmptySet() *Set {
	return emptySet
}

// reflectValue abbreviates reflect.ValueOf(d).
func (d Distinct) reflectValue() reflect.Value {
	return reflect.ValueOf(d.iface)
}

// Valid returns true if this value refers to a valid Set.
func (d Distinct) Valid() bool {
	return d.iface != nil
}

// Len returns the number of attributes in this set.
func (l *Set) Len() int {
	if l == nil || !l.equivalent.Valid() {
		return 0
	}
	return l.equivalent.reflectValue().Len()
}

// Get returns the KeyValue at ordered position idx in this set.
func (l *Set) Get(idx int) (KeyValue, bool) {
	if l == nil || !l.equivalent.Valid() {
		return KeyValue{}, false
	}
	value := l.equivalent.reflectValue()

	if idx >= 0 && idx < value.Len() {
		// Note: The Go compiler successfully avoids an allocation for
		// the interface{} conversion here:
		return value.Index(idx).Interface().(KeyValue), true
	}

	return KeyValue{}, false
}

// Value returns the value of a specified key in this set.
func (l *Set) Value(k Key) (Value, bool) {
	if l == nil || !l.equivalent.Valid() {
		return Value{}, false
	}
	rValue := l.equivalent.reflectValue()
	vlen := rValue.Len()

	idx := sort.Search(vlen, func(idx int) bool {
		return rValue.Index(idx).Interface().(KeyValue).Key >= k
	})
	if idx >= vlen {
		return Value{}, false
	}
	keyValue := rValue.Index(idx).Interface().(KeyValue)
	if k == keyValue.Key {
		return keyValue.Value, true
	}
	return Value{}, false
}

// HasValue tests whether a key is defined in this set.
func (l *Set) HasValue(k Key) bool {
	if l == nil {
		return false
	}
	_, ok := l.Value(k)
	return ok
}

// Iter returns an iterator for visiting the attributes in this set.
func (l *Set) Iter() Iterator {
	return Iterator{
		storage: l,
		idx:     -1,
	}
}

// ToSlice returns the set of attributes belonging to this set, sorted, where
// keys appear no more than once.
func (l *Set) ToSlice() []KeyValue {
	iter := l.Iter()
	return iter.ToSlice()
}

// Equivalent returns a value that may be used as a map key. The Distinct type
// guarantees that the result will equal the equivalent. Distinct value of any
// attribute set with the same elements as this, where sets are made unique by
// choosing the last value in the input for any given key.
func (l *Set) Equivalent() Distinct {
	if l == nil || !l.equivalent.Valid() {
		return emptySet.equivalent
	}
	return l.equivalent
}

// Equals returns true if the argument set is equivalent to this set.
func (l *Set) Equals(o *Set) bool {
	return l.Equivalent() == o.Equivalent()
}

// Encoded returns the encoded form of this set, according to encoder.
func (l *Set) Encoded(encoder Encoder) string {
	if l == nil || encoder == nil {
		return ""
	}

	return encoder.Encode(l.Iter())
}

func empty() Set {
	return Set{
		equivalent: emptySet.equivalent,
	}
}

// NewSet returns a new Set. See the documentation for
// NewSetWithSortableFiltered for more details.
//
// Except for empty sets, this method adds an additional allocation compared
// with calls that include a Sortable.
func NewSet(kvs ...KeyValue) Set {
	s, _ := NewSetWithFiltered(kvs, nil)
	return s
}

// NewSetWithSortable returns a new Set. See the documentation for
// NewSetWithSortableFiltered for more details.
//
// This call includes a Sortable option as a memory optimization.
//
// Deprecated: Use [NewSet] instead.
func NewSetWithSortable(kvs []KeyValue, _ *Sortable) Set {
	s, _ := NewSetWithFiltered(kvs, nil)
	return s
}

// NewSetWithFiltered returns a new Set. See the documentation for
// NewSetWithSortableFiltered for more details.
//
// This call includes a Filter to include/exclude attribute keys from the
// return value. Excluded keys are returned as a slice of attribute values.
func NewSetWithFiltered(kvs []KeyValue, filter Filter) (Set, []KeyValue) {
	// Check for empty set.
	if len(kvs) == 0 {
		return empty(), nil
	}

	// Stable sort so the following de-duplication can implement
	// last-value-wins semantics.
	slices.SortStableFunc(kvs, func(a, b KeyValue) int {
		return cmp.Compare(a.Key, b.Key)
	})

	position := len(kvs) - 1
	offset := position - 1

	// The requirements stated above require that the stable
	// result be placed in the end of the input slice, while
	// overwritten values are swapped to the beginning.
	//
	// De-duplicate with last-value-wins semantics.  Preserve
	// duplicate values at the beginning of the input slice.
	for ; offset >= 0; offset-- {
		if kvs[offset].Key == kvs[position].Key {
			continue
		}
		position--
		kvs[offset], kvs[position] = kvs[position], kvs[offset]
	}
	kvs = kvs[position:]

	if filter != nil {
		if div := filteredToFront(kvs, filter); div != 0 {
			return Set{equivalent: computeDistinct(kvs[div:])}, kvs[:div]
		}
	}
	return Set{equivalent: computeDistinct(kvs)}, nil
}

// NewSetWithSortableFiltered returns a new Set.
//
// Duplicate keys are eliminated by taking the last value.  This
// re-orders the input slice so that unique last-values are contiguous
// at the end of the slice.
//
// This ensures the following:
//
// - Last-value-wins semantics
// - Caller sees the reordering, but doesn't lose values
// - Repeated call preserve last-value wins.
//
// Note that methods are defined on Set, although this returns Set. Callers
// can avoid memory allocations by:
//
// - allocating a Sortable for use as a temporary in this method
// - allocating a Set for storing the return value of this constructor.
//
// The result maintains a cache of encoded attributes, by attribute.EncoderID.
// This value should not be copied after its first use.
//
// The second []KeyValue return value is a list of attributes that were
// excluded by the Filter (if non-nil).
//
// Deprecated: Use [NewSetWithFiltered] instead.
func NewSetWithSortableFiltered(kvs []KeyValue, _ *Sortable, filter Filter) (Set, []KeyValue) {
	return NewSetWithFiltered(kvs, filter)
}

// filteredToFront filters slice in-place using keep function. All KeyValues that need to
// be removed are moved to the front. All KeyValues that need to be kept are
// moved (in-order) to the back. The index for the first KeyValue to be kept is
// returned.
func filteredToFront(slice []KeyValue, keep Filter) int {
	n := len(slice)
	j := n
	for i := n - 1; i >= 0; i-- {
		if keep(slice[i]) {
			j--
			slice[i], slice[j] = slice[j], slice[i]
		}
	}
	return j
}

// Filter returns a filtered copy of this Set. See the documentation for
// NewSetWithSortableFiltered for more details.
func (l *Set) Filter(re Filter) (Set, []KeyValue) {
	if re == nil {
		return *l, nil
	}

	// Iterate in reverse to the first attribute that will be filtered out.
	n := l.Len()
	first := n - 1
	for ; first >= 0; first-- {
		kv, _ := l.Get(first)
		if !re(kv) {
			break
		}
	}

	// No attributes will be dropped, return the immutable Set l and nil.
	if first < 0 {
		return *l, nil
	}

	// Copy now that we know we need to return a modified set.
	//
	// Do not do this in-place on the underlying storage of *Set l. Sets are
	// immutable and filtering should not change this.
	slice := l.ToSlice()

	// Don't re-iterate the slice if only slice[0] is filtered.
	if first == 0 {
		// It is safe to assume len(slice) >= 1 given we found at least one
		// attribute above that needs to be filtered out.
		return Set{equivalent: computeDistinct(slice[1:])}, slice[:1]
	}

	// Move the filtered slice[first] to the front (preserving order).
	kv := slice[first]
	copy(slice[1:first+1], slice[:first])
	slice[0] = kv

	// Do not re-evaluate re(slice[first+1:]).
	div := filteredToFront(slice[1:first+1], re) + 1
	return Set{equivalent: computeDistinct(slice[div:])}, slice[:div]
}

// computeDistinct returns a Distinct using either the fixed- or
// reflect-oriented code path, depending on the size of the input. The input
// slice is assumed to already be sorted and de-duplicated.
func computeDistinct(kvs []KeyValue) Distinct {
	iface := computeDistinctFixed(kvs)
	if iface == nil {
		iface = computeDistinctReflect(kvs)
	}
	return Distinct{
		iface: iface,
	}
}

// computeDistinctFixed computes a Distinct for small slices. It returns nil
// if the input is too large for this code path.
func computeDistinctFixed(kvs []KeyValue) interface{} {
	switch len(kvs) {
	case 1:
		return [1]KeyValue(kvs)
	case 2:
		return [2]KeyValue(kvs)
	case 3:
		return [3]KeyValue(kvs)
	case 4:
		return [4]KeyValue(kvs)
	case 5:
		return [5]KeyValue(kvs)
	case 6:
		return [6]KeyValue(kvs)
	case 7:
		return [7]KeyValue(kvs)
	case 8:
		return [8]KeyValue(kvs)
	case 9:
		return [9]KeyValue(kvs)
	case 10:
		return [10]KeyValue(kvs)
	default:
		return nil
	}
}

// computeDistinctReflect computes a Distinct using reflection, works for any
// size input.
func computeDistinctReflect(kvs []KeyValue) interface{} {
	at := reflect.New(reflect.ArrayOf(len(kvs), keyValueType)).Elem()
	for i, keyValue := range kvs {
		*(at.Index(i).Addr().Interface().(*KeyValue)) = keyValue
	}
	return at.Interface()
}

// MarshalJSON returns the JSON encoding of the Set.
func (l *Set) MarshalJSON() ([]byte, error) {
	return json.Marshal(l.equivalent.iface)
}

// MarshalLog is the marshaling function used by the logging system to represent this Set.
func (l Set) MarshalLog() interface{} {
	kvs := make(map[string]string)
	for _, kv := range l.ToSlice() {
		kvs[string(kv.Key)] = kv.Value.Emit()
	}
	return kvs
}

// Len implements sort.Interface.
func (l *Sortable) Len() int {
	return len(*l)
}

// Swap implements sort.Interface.
func (l *Sortable) Swap(i, j int) {
	(*l)[i], (*l)[j] = (*l)[j], (*l)[i]
}

// Less implements sort.Interface.
func (l *Sortable) Less(i, j int) bool {
	return (*l)[i].Key < (*l)[j].Key
}
