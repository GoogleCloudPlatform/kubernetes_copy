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

package cel

import (
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	"github.com/google/cel-go/common/types/traits"
)

// EncodeStyle is a hint for string encoding of parsed values.
type EncodeStyle int

const (
	// BlockValueStyle is the default string encoding which preserves whitespace and newlines.
	BlockValueStyle EncodeStyle = iota

	// FlowValueStyle indicates that the string is an inline representation of complex types.
	FlowValueStyle

	// FoldedValueStyle is a multiline string with whitespace and newlines trimmed to a single
	// a whitespace. Repeated newlines are replaced with a single newline rather than a single
	// whitespace.
	FoldedValueStyle

	// LiteralStyle is a multiline string that preserves newlines, but trims all other whitespace
	// to a single character.
	LiteralStyle
)

// NewEmptyDynValue returns the zero-valued DynValue.
func NewEmptyDynValue() *DynValue {
	// note: 0 is not a valid parse node identifier.
	dv, _ := NewDynValue(0, nil)
	return dv
}

// NewDynValue returns a DynValue that corresponds to a parse node id and value.
func NewDynValue(id int64, val interface{}) (*DynValue, error) {
	dv := &DynValue{ID: id}
	err := dv.SetValue(val)
	return dv, err
}

// DynValue is a dynamically typed value used to describe unstructured content.
// Whether the value has the desired type is determined by where it is used within the Instance or
// Template, and whether there are schemas which might enforce a more rigid type definition.
type DynValue struct {
	ID          int64
	EncodeStyle EncodeStyle
	value       interface{}
	exprValue   ref.Val
	declType    *DeclType
}

// DeclType returns the policy model type of the dyn value.
func (dv *DynValue) DeclType() *DeclType {
	return dv.declType
}

// ConvertToNative is an implementation of the CEL ref.Val method used to adapt between CEL types
// and Go-native types.
//
// The default behavior of this method is to first convert to a CEL type which has a well-defined
// set of conversion behaviors and proxy to the CEL ConvertToNative method for the type.
func (dv *DynValue) ConvertToNative(typeDesc reflect.Type) (interface{}, error) {
	ev := dv.ExprValue()
	if types.IsError(ev) {
		return nil, ev.(*types.Err)
	}
	return ev.ConvertToNative(typeDesc)
}

// Equal returns whether the dyn value is equal to a given CEL value.
func (dv *DynValue) Equal(other ref.Val) ref.Val {
	dvType := dv.Type()
	otherType := other.Type()
	// Preserve CEL's homogeneous equality constraint.
	if dvType.TypeName() != otherType.TypeName() {
		return types.MaybeNoSuchOverloadErr(other)
	}
	switch v := dv.value.(type) {
	case ref.Val:
		return v.Equal(other)
	case PlainTextValue:
		return celBool(string(v) == other.Value().(string))
	case *MultilineStringValue:
		return celBool(v.Value == other.Value().(string))
	case time.Duration:
		otherDuration := other.Value().(time.Duration)
		return celBool(v == otherDuration)
	case time.Time:
		otherTimestamp := other.Value().(time.Time)
		return celBool(v.Equal(otherTimestamp))
	default:
		return celBool(reflect.DeepEqual(v, other.Value()))
	}
}

// ExprValue converts the DynValue into a CEL value.
func (dv *DynValue) ExprValue() ref.Val {
	return dv.exprValue
}

// Value returns the underlying value held by this reference.
func (dv *DynValue) Value() interface{} {
	return dv.value
}

// SetValue updates the underlying value held by this reference.
func (dv *DynValue) SetValue(value interface{}) error {
	dv.value = value
	var err error
	dv.exprValue, dv.declType, err = exprValue(value)
	return err
}

// Type returns the CEL type for the given value.
func (dv *DynValue) Type() ref.Type {
	return dv.ExprValue().Type()
}

func exprValue(value interface{}) (ref.Val, *DeclType, error) {
	switch v := value.(type) {
	case bool:
		return types.Bool(v), BoolType, nil
	case []byte:
		return types.Bytes(v), BytesType, nil
	case float64:
		return types.Double(v), DoubleType, nil
	case int64:
		return types.Int(v), IntType, nil
	case string:
		return types.String(v), StringType, nil
	case uint64:
		return types.Uint(v), UintType, nil
	case time.Duration:
		return types.Duration{Duration: v}, DurationType, nil
	case time.Time:
		return types.Timestamp{Time: v}, TimestampType, nil
	case types.Null:
		return v, NullType, nil
	case *ListValue:
		return v, ListType, nil
	case *MapValue:
		return v, MapType, nil
	case *ObjectValue:
		return v, v.objectType, nil
	default:
		return nil, unknownType, fmt.Errorf("unsupported type: (%T)%v", v, v)
	}
}

// PlainTextValue is a text string literal which must not be treated as an expression.
type PlainTextValue string

// MultilineStringValue is a multiline string value which has been parsed in a way which omits
// whitespace as well as a raw form which preserves whitespace.
type MultilineStringValue struct {
	Value string
	Raw   string
}

func newStructValue() *structValue {
	return &structValue{
		Fields:   []*Field{},
		fieldMap: map[string]*Field{},
	}
}

type structValue struct {
	Fields   []*Field
	fieldMap map[string]*Field
}

// AddField appends a MapField to the MapValue and indexes the field by name.
func (sv *structValue) AddField(field *Field) {
	sv.Fields = append(sv.Fields, field)
	sv.fieldMap[field.Name] = field
}

// ConvertToNative converts the MapValue type to a native go types.
func (sv *structValue) ConvertToNative(typeDesc reflect.Type) (interface{}, error) {
	if typeDesc.Kind() != reflect.Map &&
		typeDesc.Kind() != reflect.Struct &&
		typeDesc.Kind() != reflect.Pointer &&
		typeDesc.Kind() != reflect.Interface {
		return nil, fmt.Errorf("type conversion error from object to '%v'", typeDesc)
	}

	// Unwrap pointers, but track their use.
	isPtr := false
	if typeDesc.Kind() == reflect.Pointer {
		tk := typeDesc
		typeDesc = typeDesc.Elem()
		if typeDesc.Kind() == reflect.Pointer {
			return nil, fmt.Errorf("unsupported type conversion to '%v'", tk)
		}
		isPtr = true
	}

	if typeDesc.Kind() == reflect.Map {
		keyType := typeDesc.Key()
		if keyType.Kind() != reflect.String && keyType.Kind() != reflect.Interface {
			return nil, fmt.Errorf("object fields cannot be converted to type '%v'", keyType)
		}
		elemType := typeDesc.Elem()
		sz := len(sv.fieldMap)
		ntvMap := reflect.MakeMapWithSize(typeDesc, sz)
		for name, val := range sv.fieldMap {
			refVal, err := val.Ref.ConvertToNative(elemType)
			if err != nil {
				return nil, err
			}
			ntvMap.SetMapIndex(reflect.ValueOf(name), reflect.ValueOf(refVal))
		}
		return ntvMap.Interface(), nil
	}

	if typeDesc.Kind() == reflect.Struct {
		ntvObjPtr := reflect.New(typeDesc)
		ntvObj := ntvObjPtr.Elem()
		for name, val := range sv.fieldMap {
			f := ntvObj.FieldByName(name)
			if !f.IsValid() {
				return nil, fmt.Errorf("type conversion error, no such field %s in type %v",
					name, typeDesc)
			}
			fv, err := val.Ref.ConvertToNative(f.Type())
			if err != nil {
				return nil, err
			}
			f.Set(reflect.ValueOf(fv))
		}
		if isPtr {
			return ntvObjPtr.Interface(), nil
		}
		return ntvObj.Interface(), nil
	}
	return nil, fmt.Errorf("type conversion error from object to '%v'", typeDesc)
}

// GetField returns a MapField by name if one exists.
func (sv *structValue) GetField(name string) (*Field, bool) {
	field, found := sv.fieldMap[name]
	return field, found
}

// IsSet returns whether the given field, which is defined, has also been set.
func (sv *structValue) IsSet(key ref.Val) ref.Val {
	k, ok := key.(types.String)
	if !ok {
		return types.MaybeNoSuchOverloadErr(key)
	}
	name := string(k)
	_, found := sv.fieldMap[name]
	return celBool(found)
}

// NewObjectValue creates a struct value with a schema type and returns the empty ObjectValue.
func NewObjectValue(sType *DeclType) *ObjectValue {
	return &ObjectValue{
		structValue: newStructValue(),
		objectType:  sType,
	}
}

// ObjectValue is a struct with a custom schema type which indicates the fields and types
// associated with the structure.
type ObjectValue struct {
	*structValue
	objectType *DeclType
}

// ConvertToType is an implementation of the CEL ref.Val interface method.
func (o *ObjectValue) ConvertToType(t ref.Type) ref.Val {
	if t == types.TypeType {
		return types.NewObjectTypeValue(o.objectType.TypeName())
	}
	if t.TypeName() == o.objectType.TypeName() {
		return o
	}
	return types.NewErr("type conversion error from '%s' to '%s'", o.Type(), t)
}

// Equal returns true if the two object types are equal and their field values are equal.
func (o *ObjectValue) Equal(other ref.Val) ref.Val {
	// Preserve CEL's homogeneous equality semantics.
	if o.objectType.TypeName() != other.Type().TypeName() {
		return types.MaybeNoSuchOverloadErr(other)
	}
	o2 := other.(traits.Indexer)
	for name := range o.objectType.Fields {
		k := types.String(name)
		v := o.Get(k)
		ov := o2.Get(k)
		vEq := v.Equal(ov)
		if vEq != types.True {
			return vEq
		}
	}
	return types.True
}

// Get returns the value of the specified field.
//
// If the field is set, its value is returned. If the field is not set, the default value for the
// field is returned thus allowing for safe-traversal and preserving proto-like field traversal
// semantics for Open API Schema backed types.
func (o *ObjectValue) Get(name ref.Val) ref.Val {
	n, ok := name.(types.String)
	if !ok {
		return types.MaybeNoSuchOverloadErr(n)
	}
	nameStr := string(n)
	field, found := o.fieldMap[nameStr]
	if found {
		return field.Ref.ExprValue()
	}
	fieldDef, found := o.objectType.Fields[nameStr]
	if !found {
		return types.NewErr("no such field: %s", nameStr)
	}
	defValue := fieldDef.DefaultValue()
	if defValue != nil {
		return defValue
	}
	return types.NewErr("no default for type: %s", fieldDef.TypeName())
}

// Type returns the CEL type value of the object.
func (o *ObjectValue) Type() ref.Type {
	return o.objectType
}

// Value returns the Go-native representation of the object.
func (o *ObjectValue) Value() interface{} {
	return o
}

// NewMapValue returns an empty MapValue.
func NewMapValue() *MapValue {
	return &MapValue{
		structValue: newStructValue(),
	}
}

// MapValue declares an object with a set of named fields whose values are dynamically typed.
type MapValue struct {
	*structValue
}

// ConvertToObject produces an ObjectValue from the MapValue with the associated schema type.
//
// The conversion is shallow and the memory shared between the Object and Map as all references
// to the map are expected to be replaced with the Object reference.
func (m *MapValue) ConvertToObject(declType *DeclType) *ObjectValue {
	return &ObjectValue{
		structValue: m.structValue,
		objectType:  declType,
	}
}

// Contains returns whether the given key is contained in the MapValue.
func (m *MapValue) Contains(key ref.Val) ref.Val {
	v, found := m.Find(key)
	if v != nil && types.IsUnknownOrError(v) {
		return v
	}
	return celBool(found)
}

// ConvertToType converts the MapValue to another CEL type, if possible.
func (m *MapValue) ConvertToType(t ref.Type) ref.Val {
	switch t {
	case types.MapType:
		return m
	case types.TypeType:
		return types.MapType
	}
	return types.NewErr("type conversion error from '%s' to '%s'", m.Type(), t)
}

// Equal returns true if the maps are of the same size, have the same keys, and the key-values
// from each map are equal.
func (m *MapValue) Equal(other ref.Val) ref.Val {
	oMap, isMap := other.(traits.Mapper)
	if !isMap {
		return types.MaybeNoSuchOverloadErr(other)
	}
	if m.Size() != oMap.Size() {
		return types.False
	}
	for name, field := range m.fieldMap {
		k := types.String(name)
		ov, found := oMap.Find(k)
		if !found {
			return types.False
		}
		v := field.Ref.ExprValue()
		vEq := v.Equal(ov)
		if vEq != types.True {
			return vEq
		}
	}
	return types.True
}

// Find returns the value for the key in the map, if found.
func (m *MapValue) Find(name ref.Val) (ref.Val, bool) {
	// Currently only maps with string keys are supported as this is best aligned with JSON,
	// and also much simpler to support.
	n, ok := name.(types.String)
	if !ok {
		return types.MaybeNoSuchOverloadErr(n), true
	}
	nameStr := string(n)
	field, found := m.fieldMap[nameStr]
	if found {
		return field.Ref.ExprValue(), true
	}
	return nil, false
}

// Get returns the value for the key in the map, or error if not found.
func (m *MapValue) Get(key ref.Val) ref.Val {
	v, found := m.Find(key)
	if found {
		return v
	}
	return types.ValOrErr(key, "no such key: %v", key)
}

// Iterator produces a traits.Iterator which walks over the map keys.
//
// The Iterator is frequently used within comprehensions.
func (m *MapValue) Iterator() traits.Iterator {
	keys := make([]ref.Val, len(m.fieldMap))
	i := 0
	for k := range m.fieldMap {
		keys[i] = types.String(k)
		i++
	}
	return &baseMapIterator{
		baseVal: &baseVal{},
		keys:    keys,
	}
}

// Size returns the number of keys in the map.
func (m *MapValue) Size() ref.Val {
	return types.Int(len(m.Fields))
}

// Type returns the CEL ref.Type for the map.
func (m *MapValue) Type() ref.Type {
	return types.MapType
}

// Value returns the Go-native representation of the MapValue.
func (m *MapValue) Value() interface{} {
	return m
}

type baseMapIterator struct {
	*baseVal
	keys []ref.Val
	idx  int
}

// HasNext implements the traits.Iterator interface method.
func (it *baseMapIterator) HasNext() ref.Val {
	if it.idx < len(it.keys) {
		return types.True
	}
	return types.False
}

// Next implements the traits.Iterator interface method.
func (it *baseMapIterator) Next() ref.Val {
	key := it.keys[it.idx]
	it.idx++
	return key
}

// Type implements the CEL ref.Val interface method.
func (it *baseMapIterator) Type() ref.Type {
	return types.IteratorType
}

// NewField returns a MapField instance with an empty DynValue that refers to the
// specified parse node id and field name.
func NewField(id int64, name string) *Field {
	return &Field{
		ID:   id,
		Name: name,
		Ref:  NewEmptyDynValue(),
	}
}

// Field specifies a field name and a reference to a dynamic value.
type Field struct {
	ID   int64
	Name string
	Ref  *DynValue
}

// NewListValue returns an empty ListValue instance.
func NewListValue() *ListValue {
	return &ListValue{
		Entries: []*DynValue{},
	}
}

// ListValue contains a list of dynamically typed entries.
type ListValue struct {
	Entries      []*DynValue
	initValueSet sync.Once
	valueSet     map[ref.Val]struct{}
}

// Add concatenates two lists together to produce a new CEL list value.
func (lv *ListValue) Add(other ref.Val) ref.Val {
	oArr, isArr := other.(traits.Lister)
	if !isArr {
		return types.MaybeNoSuchOverloadErr(other)
	}
	szRight := len(lv.Entries)
	szLeft := int(oArr.Size().(types.Int))
	sz := szRight + szLeft
	combo := make([]ref.Val, sz)
	for i := 0; i < szRight; i++ {
		combo[i] = lv.Entries[i].ExprValue()
	}
	for i := 0; i < szLeft; i++ {
		combo[i+szRight] = oArr.Get(types.Int(i))
	}
	return types.DefaultTypeAdapter.NativeToValue(combo)
}

// Append adds another entry into the ListValue.
func (lv *ListValue) Append(entry *DynValue) {
	lv.Entries = append(lv.Entries, entry)
	// The append resets all previously built indices.
	lv.initValueSet = sync.Once{}
}

// Contains returns whether the input `val` is equal to an element in the list.
//
// If any pair-wise comparison between the input value and the list element is an error, the
// operation will return an error.
func (lv *ListValue) Contains(val ref.Val) ref.Val {
	if types.IsUnknownOrError(val) {
		return val
	}
	lv.initValueSet.Do(lv.finalizeValueSet)
	if lv.valueSet != nil {
		_, found := lv.valueSet[val]
		if found {
			return types.True
		}
		// Instead of returning false, ensure that CEL's heterogeneous equality constraint
		// is satisfied by allowing pair-wise equality behavior to determine the outcome.
	}
	var err ref.Val
	sz := len(lv.Entries)
	for i := 0; i < sz; i++ {
		elem := lv.Entries[i]
		cmp := elem.Equal(val)
		b, ok := cmp.(types.Bool)
		if !ok && err == nil {
			err = types.MaybeNoSuchOverloadErr(cmp)
		}
		if b == types.True {
			return types.True
		}
	}
	if err != nil {
		return err
	}
	return types.False
}

// ConvertToNative is an implementation of the CEL ref.Val method used to adapt between CEL types
// and Go-native array-like types.
func (lv *ListValue) ConvertToNative(typeDesc reflect.Type) (interface{}, error) {
	// Non-list conversion.
	if typeDesc.Kind() != reflect.Slice &&
		typeDesc.Kind() != reflect.Array &&
		typeDesc.Kind() != reflect.Interface {
		return nil, fmt.Errorf("type conversion error from list to '%v'", typeDesc)
	}

	// If the list is already assignable to the desired type return it.
	if reflect.TypeOf(lv).AssignableTo(typeDesc) {
		return lv, nil
	}

	// List conversion.
	otherElem := typeDesc.Elem()

	// Allow the element ConvertToNative() function to determine whether conversion is possible.
	sz := len(lv.Entries)
	nativeList := reflect.MakeSlice(typeDesc, int(sz), int(sz))
	for i := 0; i < sz; i++ {
		elem := lv.Entries[i]
		nativeElemVal, err := elem.ConvertToNative(otherElem)
		if err != nil {
			return nil, err
		}
		nativeList.Index(int(i)).Set(reflect.ValueOf(nativeElemVal))
	}
	return nativeList.Interface(), nil
}

// ConvertToType converts the ListValue to another CEL type.
func (lv *ListValue) ConvertToType(t ref.Type) ref.Val {
	switch t {
	case types.ListType:
		return lv
	case types.TypeType:
		return types.ListType
	}
	return types.NewErr("type conversion error from '%s' to '%s'", ListType, t)
}

// Equal returns true if two lists are of the same size, and the values at each index are also
// equal.
func (lv *ListValue) Equal(other ref.Val) ref.Val {
	oArr, isArr := other.(traits.Lister)
	if !isArr {
		return types.MaybeNoSuchOverloadErr(other)
	}
	sz := types.Int(len(lv.Entries))
	if sz != oArr.Size() {
		return types.False
	}
	for i := types.Int(0); i < sz; i++ {
		cmp := lv.Get(i).Equal(oArr.Get(i))
		if cmp != types.True {
			return cmp
		}
	}
	return types.True
}

// Get returns the value at the given index.
//
// If the index is negative or greater than the size of the list, an error is returned.
func (lv *ListValue) Get(idx ref.Val) ref.Val {
	iv, isInt := idx.(types.Int)
	if !isInt {
		return types.ValOrErr(idx, "unsupported index: %v", idx)
	}
	i := int(iv)
	if i < 0 || i >= len(lv.Entries) {
		return types.NewErr("index out of bounds: %v", idx)
	}
	return lv.Entries[i].ExprValue()
}

// Iterator produces a traits.Iterator suitable for use in CEL comprehension macros.
func (lv *ListValue) Iterator() traits.Iterator {
	return &baseListIterator{
		getter: lv.Get,
		sz:     len(lv.Entries),
	}
}

// Size returns the number of elements in the list.
func (lv *ListValue) Size() ref.Val {
	return types.Int(len(lv.Entries))
}

// Type returns the CEL ref.Type for the list.
func (lv *ListValue) Type() ref.Type {
	return types.ListType
}

// Value returns the Go-native value.
func (lv *ListValue) Value() interface{} {
	return lv
}

// finalizeValueSet inspects the ListValue entries in order to make internal optimizations once all list
// entries are known.
func (lv *ListValue) finalizeValueSet() {
	valueSet := make(map[ref.Val]struct{})
	for _, e := range lv.Entries {
		switch e.value.(type) {
		case bool, float64, int64, string, uint64, types.Null, PlainTextValue:
			valueSet[e.ExprValue()] = struct{}{}
		default:
			lv.valueSet = nil
			return
		}
	}
	lv.valueSet = valueSet
}

type baseVal struct{}

func (*baseVal) ConvertToNative(typeDesc reflect.Type) (interface{}, error) {
	return nil, fmt.Errorf("unsupported native conversion to: %v", typeDesc)
}

func (*baseVal) ConvertToType(t ref.Type) ref.Val {
	return types.NewErr("unsupported type conversion to: %v", t)
}

func (*baseVal) Equal(other ref.Val) ref.Val {
	return types.NewErr("unsupported equality test between instances")
}

func (v *baseVal) Value() interface{} {
	return nil
}

type baseListIterator struct {
	*baseVal
	getter func(idx ref.Val) ref.Val
	sz     int
	idx    int
}

func (it *baseListIterator) HasNext() ref.Val {
	if it.idx < it.sz {
		return types.True
	}
	return types.False
}

func (it *baseListIterator) Next() ref.Val {
	v := it.getter(types.Int(it.idx))
	it.idx++
	return v
}

func (it *baseListIterator) Type() ref.Type {
	return types.IteratorType
}

func celBool(pred bool) ref.Val {
	if pred {
		return types.True
	}
	return types.False
}

var unknownType = &DeclType{name: "unknown", MinSerializedSize: 1}
