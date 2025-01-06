// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

//go:generate stringer -type=ValueKind -trimprefix=ValueKind

package telemetry

import (
	"bytes"
	"cmp"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"strconv"
	"unsafe"
)

// A Value represents a structured value.
// A zero value is valid and represents an empty value.
type Value struct {
	// Ensure forward compatibility by explicitly making this not comparable.
	noCmp [0]func() //nolint: unused  // This is indeed used.

	// num holds the value for Int64, Float64, and Bool. It holds the length
	// for String, Bytes, Slice, Map.
	num uint64
	// any holds either the KindBool, KindInt64, KindFloat64, stringptr,
	// bytesptr, sliceptr, or mapptr. If KindBool, KindInt64, or KindFloat64
	// then the value of Value is in num as described above. Otherwise, it
	// contains the value wrapped in the appropriate type.
	any any
}

type (
	// sliceptr represents a value in Value.any for KindString Values.
	stringptr *byte
	// bytesptr represents a value in Value.any for KindBytes Values.
	bytesptr *byte
	// sliceptr represents a value in Value.any for KindSlice Values.
	sliceptr *Value
	// mapptr represents a value in Value.any for KindMap Values.
	mapptr *Attr
)

// ValueKind is the kind of a [Value].
type ValueKind int

// ValueKind values.
const (
	ValueKindEmpty ValueKind = iota
	ValueKindBool
	ValueKindFloat64
	ValueKindInt64
	ValueKindString
	ValueKindBytes
	ValueKindSlice
	ValueKindMap
)

var valueKindStrings = []string{
	"Empty",
	"Bool",
	"Float64",
	"Int64",
	"String",
	"Bytes",
	"Slice",
	"Map",
}

func (k ValueKind) String() string {
	if k >= 0 && int(k) < len(valueKindStrings) {
		return valueKindStrings[k]
	}
	return "<unknown telemetry.ValueKind>"
}

// StringValue returns a new [Value] for a string.
func StringValue(v string) Value {
	return Value{
		num: uint64(len(v)),
		any: stringptr(unsafe.StringData(v)),
	}
}

// IntValue returns a [Value] for an int.
func IntValue(v int) Value { return Int64Value(int64(v)) }

// Int64Value returns a [Value] for an int64.
func Int64Value(v int64) Value {
	return Value{num: uint64(v), any: ValueKindInt64}
}

// Float64Value returns a [Value] for a float64.
func Float64Value(v float64) Value {
	return Value{num: math.Float64bits(v), any: ValueKindFloat64}
}

// BoolValue returns a [Value] for a bool.
func BoolValue(v bool) Value { //nolint:revive // Not a control flag.
	var n uint64
	if v {
		n = 1
	}
	return Value{num: n, any: ValueKindBool}
}

// BytesValue returns a [Value] for a byte slice. The passed slice must not be
// changed after it is passed.
func BytesValue(v []byte) Value {
	return Value{
		num: uint64(len(v)),
		any: bytesptr(unsafe.SliceData(v)),
	}
}

// SliceValue returns a [Value] for a slice of [Value]. The passed slice must
// not be changed after it is passed.
func SliceValue(vs ...Value) Value {
	return Value{
		num: uint64(len(vs)),
		any: sliceptr(unsafe.SliceData(vs)),
	}
}

// MapValue returns a new [Value] for a slice of key-value pairs. The passed
// slice must not be changed after it is passed.
func MapValue(kvs ...Attr) Value {
	return Value{
		num: uint64(len(kvs)),
		any: mapptr(unsafe.SliceData(kvs)),
	}
}

// AsString returns the value held by v as a string.
func (v Value) AsString() string {
	if sp, ok := v.any.(stringptr); ok {
		return unsafe.String(sp, v.num)
	}
	// TODO: error handle
	return ""
}

// asString returns the value held by v as a string. It will panic if the Value
// is not KindString.
func (v Value) asString() string {
	return unsafe.String(v.any.(stringptr), v.num)
}

// AsInt64 returns the value held by v as an int64.
func (v Value) AsInt64() int64 {
	if v.Kind() != ValueKindInt64 {
		// TODO: error handle
		return 0
	}
	return v.asInt64()
}

// asInt64 returns the value held by v as an int64. If v is not of KindInt64,
// this will return garbage.
func (v Value) asInt64() int64 {
	// Assumes v.num was a valid int64 (overflow not checked).
	return int64(v.num) // nolint: gosec
}

// AsBool returns the value held by v as a bool.
func (v Value) AsBool() bool {
	if v.Kind() != ValueKindBool {
		// TODO: error handle
		return false
	}
	return v.asBool()
}

// asBool returns the value held by v as a bool. If v is not of KindBool, this
// will return garbage.
func (v Value) asBool() bool { return v.num == 1 }

// AsFloat64 returns the value held by v as a float64.
func (v Value) AsFloat64() float64 {
	if v.Kind() != ValueKindFloat64 {
		// TODO: error handle
		return 0
	}
	return v.asFloat64()
}

// asFloat64 returns the value held by v as a float64. If v is not of
// KindFloat64, this will return garbage.
func (v Value) asFloat64() float64 { return math.Float64frombits(v.num) }

// AsBytes returns the value held by v as a []byte.
func (v Value) AsBytes() []byte {
	if sp, ok := v.any.(bytesptr); ok {
		return unsafe.Slice((*byte)(sp), v.num)
	}
	// TODO: error handle
	return nil
}

// asBytes returns the value held by v as a []byte. It will panic if the Value
// is not KindBytes.
func (v Value) asBytes() []byte {
	return unsafe.Slice((*byte)(v.any.(bytesptr)), v.num)
}

// AsSlice returns the value held by v as a []Value.
func (v Value) AsSlice() []Value {
	if sp, ok := v.any.(sliceptr); ok {
		return unsafe.Slice((*Value)(sp), v.num)
	}
	// TODO: error handle
	return nil
}

// asSlice returns the value held by v as a []Value. It will panic if the Value
// is not KindSlice.
func (v Value) asSlice() []Value {
	return unsafe.Slice((*Value)(v.any.(sliceptr)), v.num)
}

// AsMap returns the value held by v as a []Attr.
func (v Value) AsMap() []Attr {
	if sp, ok := v.any.(mapptr); ok {
		return unsafe.Slice((*Attr)(sp), v.num)
	}
	// TODO: error handle
	return nil
}

// asMap returns the value held by v as a []Attr. It will panic if the
// Value is not KindMap.
func (v Value) asMap() []Attr {
	return unsafe.Slice((*Attr)(v.any.(mapptr)), v.num)
}

// Kind returns the Kind of v.
func (v Value) Kind() ValueKind {
	switch x := v.any.(type) {
	case ValueKind:
		return x
	case stringptr:
		return ValueKindString
	case bytesptr:
		return ValueKindBytes
	case sliceptr:
		return ValueKindSlice
	case mapptr:
		return ValueKindMap
	default:
		return ValueKindEmpty
	}
}

// Empty returns if v does not hold any value.
func (v Value) Empty() bool { return v.Kind() == ValueKindEmpty }

// Equal returns if v is equal to w.
func (v Value) Equal(w Value) bool {
	k1 := v.Kind()
	k2 := w.Kind()
	if k1 != k2 {
		return false
	}
	switch k1 {
	case ValueKindInt64, ValueKindBool:
		return v.num == w.num
	case ValueKindString:
		return v.asString() == w.asString()
	case ValueKindFloat64:
		return v.asFloat64() == w.asFloat64()
	case ValueKindSlice:
		return slices.EqualFunc(v.asSlice(), w.asSlice(), Value.Equal)
	case ValueKindMap:
		sv := sortMap(v.asMap())
		sw := sortMap(w.asMap())
		return slices.EqualFunc(sv, sw, Attr.Equal)
	case ValueKindBytes:
		return bytes.Equal(v.asBytes(), w.asBytes())
	case ValueKindEmpty:
		return true
	default:
		// TODO: error handle
		return false
	}
}

func sortMap(m []Attr) []Attr {
	sm := make([]Attr, len(m))
	copy(sm, m)
	slices.SortFunc(sm, func(a, b Attr) int {
		return cmp.Compare(a.Key, b.Key)
	})

	return sm
}

// String returns Value's value as a string, formatted like [fmt.Sprint].
//
// The returned string is meant for debugging;
// the string representation is not stable.
func (v Value) String() string {
	switch v.Kind() {
	case ValueKindString:
		return v.asString()
	case ValueKindInt64:
		// Assumes v.num was a valid int64 (overflow not checked).
		return strconv.FormatInt(int64(v.num), 10) // nolint: gosec
	case ValueKindFloat64:
		return strconv.FormatFloat(v.asFloat64(), 'g', -1, 64)
	case ValueKindBool:
		return strconv.FormatBool(v.asBool())
	case ValueKindBytes:
		return fmt.Sprint(v.asBytes())
	case ValueKindMap:
		return fmt.Sprint(v.asMap())
	case ValueKindSlice:
		return fmt.Sprint(v.asSlice())
	case ValueKindEmpty:
		return "<nil>"
	default:
		// Try to handle this as gracefully as possible.
		//
		// Don't panic here. The goal here is to have developers find this
		// first if a slog.Kind is is not handled. It is
		// preferable to have user's open issue asking why their attributes
		// have a "unhandled: " prefix than say that their code is panicking.
		return fmt.Sprintf("<unhandled telemetry.ValueKind: %s>", v.Kind())
	}
}

// MarshalJSON encodes v into OTLP formatted JSON.
func (v *Value) MarshalJSON() ([]byte, error) {
	switch v.Kind() {
	case ValueKindString:
		return json.Marshal(struct {
			Value string `json:"stringValue"`
		}{v.asString()})
	case ValueKindInt64:
		return json.Marshal(struct {
			Value string `json:"intValue"`
		}{strconv.FormatInt(int64(v.num), 10)})
	case ValueKindFloat64:
		return json.Marshal(struct {
			Value float64 `json:"doubleValue"`
		}{v.asFloat64()})
	case ValueKindBool:
		return json.Marshal(struct {
			Value bool `json:"boolValue"`
		}{v.asBool()})
	case ValueKindBytes:
		return json.Marshal(struct {
			Value []byte `json:"bytesValue"`
		}{v.asBytes()})
	case ValueKindMap:
		return json.Marshal(struct {
			Value struct {
				Values []Attr `json:"values"`
			} `json:"kvlistValue"`
		}{struct {
			Values []Attr `json:"values"`
		}{v.asMap()}})
	case ValueKindSlice:
		return json.Marshal(struct {
			Value struct {
				Values []Value `json:"values"`
			} `json:"arrayValue"`
		}{struct {
			Values []Value `json:"values"`
		}{v.asSlice()}})
	case ValueKindEmpty:
		return nil, nil
	default:
		return nil, fmt.Errorf("unknown Value kind: %s", v.Kind().String())
	}
}

// UnmarshalJSON decodes the OTLP formatted JSON contained in data into v.
func (v *Value) UnmarshalJSON(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))

	t, err := decoder.Token()
	if err != nil {
		return err
	}
	if t != json.Delim('{') {
		return errors.New("invalid Value type")
	}

	for decoder.More() {
		keyIface, err := decoder.Token()
		if err != nil {
			if errors.Is(err, io.EOF) {
				// Empty.
				return nil
			}
			return err
		}

		key, ok := keyIface.(string)
		if !ok {
			return fmt.Errorf("invalid Value key: %#v", keyIface)
		}

		switch key {
		case "stringValue", "string_value":
			var val string
			err = decoder.Decode(&val)
			*v = StringValue(val)
		case "boolValue", "bool_value":
			var val bool
			err = decoder.Decode(&val)
			*v = BoolValue(val)
		case "intValue", "int_value":
			var val protoInt64
			err = decoder.Decode(&val)
			*v = Int64Value(val.Int64())
		case "doubleValue", "double_value":
			var val float64
			err = decoder.Decode(&val)
			*v = Float64Value(val)
		case "bytesValue", "bytes_value":
			var val64 string
			if err := decoder.Decode(&val64); err != nil {
				return err
			}
			var val []byte
			val, err = base64.StdEncoding.DecodeString(val64)
			*v = BytesValue(val)
		case "arrayValue", "array_value":
			var val struct{ Values []Value }
			err = decoder.Decode(&val)
			*v = SliceValue(val.Values...)
		case "kvlistValue", "kvlist_value":
			var val struct{ Values []Attr }
			err = decoder.Decode(&val)
			*v = MapValue(val.Values...)
		default:
			// Skip unknown.
			continue
		}
		// Use first valid. Ignore the rest.
		return err
	}

	// Only unknown fields. Return nil without unmarshaling any value.
	return nil
}
