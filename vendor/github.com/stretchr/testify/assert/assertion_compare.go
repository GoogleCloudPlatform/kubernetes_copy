package assert

import (
	"bytes"
	"fmt"
	"reflect"
	"time"
)

// Deprecated: CompareType has only ever been for internal use and has accidentally been published since v1.6.0. Do not use it.
type CompareType = compareResult

type compareResult int

const (
	compareLess compareResult = iota - 1
	compareEqual
	compareGreater
)

var (
	intType   = reflect.TypeOf(int(1))
	int8Type  = reflect.TypeOf(int8(1))
	int16Type = reflect.TypeOf(int16(1))
	int32Type = reflect.TypeOf(int32(1))
	int64Type = reflect.TypeOf(int64(1))

	uintType   = reflect.TypeOf(uint(1))
	uint8Type  = reflect.TypeOf(uint8(1))
	uint16Type = reflect.TypeOf(uint16(1))
	uint32Type = reflect.TypeOf(uint32(1))
	uint64Type = reflect.TypeOf(uint64(1))

	uintptrType = reflect.TypeOf(uintptr(1))

	float32Type = reflect.TypeOf(float32(1))
	float64Type = reflect.TypeOf(float64(1))

	stringType = reflect.TypeOf("")

	timeType  = reflect.TypeOf(time.Time{})
	bytesType = reflect.TypeOf([]byte{})
)

func compare(obj1, obj2 interface{}, kind reflect.Kind) (compareResult, bool) {
	obj1Value := reflect.ValueOf(obj1)
	obj2Value := reflect.ValueOf(obj2)

	// throughout this switch we try and avoid calling .Convert() if possible,
	// as this has a pretty big performance impact
	switch kind {
	case reflect.Int:
		{
			intobj1, ok := obj1.(int)
			if !ok {
				intobj1 = obj1Value.Convert(intType).Interface().(int)
			}
			intobj2, ok := obj2.(int)
			if !ok {
				intobj2 = obj2Value.Convert(intType).Interface().(int)
			}
			if intobj1 > intobj2 {
				return compareGreater, true
			}
			if intobj1 == intobj2 {
				return compareEqual, true
			}
			if intobj1 < intobj2 {
				return compareLess, true
			}
		}
	case reflect.Int8:
		{
			int8obj1, ok := obj1.(int8)
			if !ok {
				int8obj1 = obj1Value.Convert(int8Type).Interface().(int8)
			}
			int8obj2, ok := obj2.(int8)
			if !ok {
				int8obj2 = obj2Value.Convert(int8Type).Interface().(int8)
			}
			if int8obj1 > int8obj2 {
				return compareGreater, true
			}
			if int8obj1 == int8obj2 {
				return compareEqual, true
			}
			if int8obj1 < int8obj2 {
				return compareLess, true
			}
		}
	case reflect.Int16:
		{
			int16obj1, ok := obj1.(int16)
			if !ok {
				int16obj1 = obj1Value.Convert(int16Type).Interface().(int16)
			}
			int16obj2, ok := obj2.(int16)
			if !ok {
				int16obj2 = obj2Value.Convert(int16Type).Interface().(int16)
			}
			if int16obj1 > int16obj2 {
				return compareGreater, true
			}
			if int16obj1 == int16obj2 {
				return compareEqual, true
			}
			if int16obj1 < int16obj2 {
				return compareLess, true
			}
		}
	case reflect.Int32:
		{
			int32obj1, ok := obj1.(int32)
			if !ok {
				int32obj1 = obj1Value.Convert(int32Type).Interface().(int32)
			}
			int32obj2, ok := obj2.(int32)
			if !ok {
				int32obj2 = obj2Value.Convert(int32Type).Interface().(int32)
			}
			if int32obj1 > int32obj2 {
				return compareGreater, true
			}
			if int32obj1 == int32obj2 {
				return compareEqual, true
			}
			if int32obj1 < int32obj2 {
				return compareLess, true
			}
		}
	case reflect.Int64:
		{
			int64obj1, ok := obj1.(int64)
			if !ok {
				int64obj1 = obj1Value.Convert(int64Type).Interface().(int64)
			}
			int64obj2, ok := obj2.(int64)
			if !ok {
				int64obj2 = obj2Value.Convert(int64Type).Interface().(int64)
			}
			if int64obj1 > int64obj2 {
				return compareGreater, true
			}
			if int64obj1 == int64obj2 {
				return compareEqual, true
			}
			if int64obj1 < int64obj2 {
				return compareLess, true
			}
		}
	case reflect.Uint:
		{
			uintobj1, ok := obj1.(uint)
			if !ok {
				uintobj1 = obj1Value.Convert(uintType).Interface().(uint)
			}
			uintobj2, ok := obj2.(uint)
			if !ok {
				uintobj2 = obj2Value.Convert(uintType).Interface().(uint)
			}
			if uintobj1 > uintobj2 {
				return compareGreater, true
			}
			if uintobj1 == uintobj2 {
				return compareEqual, true
			}
			if uintobj1 < uintobj2 {
				return compareLess, true
			}
		}
	case reflect.Uint8:
		{
			uint8obj1, ok := obj1.(uint8)
			if !ok {
				uint8obj1 = obj1Value.Convert(uint8Type).Interface().(uint8)
			}
			uint8obj2, ok := obj2.(uint8)
			if !ok {
				uint8obj2 = obj2Value.Convert(uint8Type).Interface().(uint8)
			}
			if uint8obj1 > uint8obj2 {
				return compareGreater, true
			}
			if uint8obj1 == uint8obj2 {
				return compareEqual, true
			}
			if uint8obj1 < uint8obj2 {
				return compareLess, true
			}
		}
	case reflect.Uint16:
		{
			uint16obj1, ok := obj1.(uint16)
			if !ok {
				uint16obj1 = obj1Value.Convert(uint16Type).Interface().(uint16)
			}
			uint16obj2, ok := obj2.(uint16)
			if !ok {
				uint16obj2 = obj2Value.Convert(uint16Type).Interface().(uint16)
			}
			if uint16obj1 > uint16obj2 {
				return compareGreater, true
			}
			if uint16obj1 == uint16obj2 {
				return compareEqual, true
			}
			if uint16obj1 < uint16obj2 {
				return compareLess, true
			}
		}
	case reflect.Uint32:
		{
			uint32obj1, ok := obj1.(uint32)
			if !ok {
				uint32obj1 = obj1Value.Convert(uint32Type).Interface().(uint32)
			}
			uint32obj2, ok := obj2.(uint32)
			if !ok {
				uint32obj2 = obj2Value.Convert(uint32Type).Interface().(uint32)
			}
			if uint32obj1 > uint32obj2 {
				return compareGreater, true
			}
			if uint32obj1 == uint32obj2 {
				return compareEqual, true
			}
			if uint32obj1 < uint32obj2 {
				return compareLess, true
			}
		}
	case reflect.Uint64:
		{
			uint64obj1, ok := obj1.(uint64)
			if !ok {
				uint64obj1 = obj1Value.Convert(uint64Type).Interface().(uint64)
			}
			uint64obj2, ok := obj2.(uint64)
			if !ok {
				uint64obj2 = obj2Value.Convert(uint64Type).Interface().(uint64)
			}
			if uint64obj1 > uint64obj2 {
				return compareGreater, true
			}
			if uint64obj1 == uint64obj2 {
				return compareEqual, true
			}
			if uint64obj1 < uint64obj2 {
				return compareLess, true
			}
		}
	case reflect.Float32:
		{
			float32obj1, ok := obj1.(float32)
			if !ok {
				float32obj1 = obj1Value.Convert(float32Type).Interface().(float32)
			}
			float32obj2, ok := obj2.(float32)
			if !ok {
				float32obj2 = obj2Value.Convert(float32Type).Interface().(float32)
			}
			if float32obj1 > float32obj2 {
				return compareGreater, true
			}
			if float32obj1 == float32obj2 {
				return compareEqual, true
			}
			if float32obj1 < float32obj2 {
				return compareLess, true
			}
		}
	case reflect.Float64:
		{
			float64obj1, ok := obj1.(float64)
			if !ok {
				float64obj1 = obj1Value.Convert(float64Type).Interface().(float64)
			}
			float64obj2, ok := obj2.(float64)
			if !ok {
				float64obj2 = obj2Value.Convert(float64Type).Interface().(float64)
			}
			if float64obj1 > float64obj2 {
				return compareGreater, true
			}
			if float64obj1 == float64obj2 {
				return compareEqual, true
			}
			if float64obj1 < float64obj2 {
				return compareLess, true
			}
		}
	case reflect.String:
		{
			stringobj1, ok := obj1.(string)
			if !ok {
				stringobj1 = obj1Value.Convert(stringType).Interface().(string)
			}
			stringobj2, ok := obj2.(string)
			if !ok {
				stringobj2 = obj2Value.Convert(stringType).Interface().(string)
			}
			if stringobj1 > stringobj2 {
				return compareGreater, true
			}
			if stringobj1 == stringobj2 {
				return compareEqual, true
			}
			if stringobj1 < stringobj2 {
				return compareLess, true
			}
		}
	// Check for known struct types we can check for compare results.
	case reflect.Struct:
		{
			// All structs enter here. We're not interested in most types.
			if !obj1Value.CanConvert(timeType) {
				break
			}

			// time.Time can be compared!
			timeObj1, ok := obj1.(time.Time)
			if !ok {
				timeObj1 = obj1Value.Convert(timeType).Interface().(time.Time)
			}

			timeObj2, ok := obj2.(time.Time)
			if !ok {
				timeObj2 = obj2Value.Convert(timeType).Interface().(time.Time)
			}

			if timeObj1.Before(timeObj2) {
				return compareLess, true
			}
			if timeObj1.Equal(timeObj2) {
				return compareEqual, true
			}
			return compareGreater, true
		}
	case reflect.Slice:
		{
			// We only care about the []byte type.
			if !obj1Value.CanConvert(bytesType) {
				break
			}

			// []byte can be compared!
			bytesObj1, ok := obj1.([]byte)
			if !ok {
				bytesObj1 = obj1Value.Convert(bytesType).Interface().([]byte)

			}
			bytesObj2, ok := obj2.([]byte)
			if !ok {
				bytesObj2 = obj2Value.Convert(bytesType).Interface().([]byte)
			}

			return compareResult(bytes.Compare(bytesObj1, bytesObj2)), true
		}
	case reflect.Uintptr:
		{
			uintptrObj1, ok := obj1.(uintptr)
			if !ok {
				uintptrObj1 = obj1Value.Convert(uintptrType).Interface().(uintptr)
			}
			uintptrObj2, ok := obj2.(uintptr)
			if !ok {
				uintptrObj2 = obj2Value.Convert(uintptrType).Interface().(uintptr)
			}
			if uintptrObj1 > uintptrObj2 {
				return compareGreater, true
			}
			if uintptrObj1 == uintptrObj2 {
				return compareEqual, true
			}
			if uintptrObj1 < uintptrObj2 {
				return compareLess, true
			}
		}
	}

	return compareEqual, false
}

// Greater asserts that the first element is greater than the second
//
//	assert.Greater(t, 2, 1)
//	assert.Greater(t, float64(2), float64(1))
//	assert.Greater(t, "b", "a")
func Greater(t TestingT, e1 interface{}, e2 interface{}, msgAndArgs ...interface{}) bool {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	return compareTwoValues(t, e1, e2, []compareResult{compareGreater}, "\"%v\" is not greater than \"%v\"", msgAndArgs...)
}

// GreaterOrEqual asserts that the first element is greater than or equal to the second
//
//	assert.GreaterOrEqual(t, 2, 1)
//	assert.GreaterOrEqual(t, 2, 2)
//	assert.GreaterOrEqual(t, "b", "a")
//	assert.GreaterOrEqual(t, "b", "b")
func GreaterOrEqual(t TestingT, e1 interface{}, e2 interface{}, msgAndArgs ...interface{}) bool {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	return compareTwoValues(t, e1, e2, []compareResult{compareGreater, compareEqual}, "\"%v\" is not greater than or equal to \"%v\"", msgAndArgs...)
}

// Less asserts that the first element is less than the second
//
//	assert.Less(t, 1, 2)
//	assert.Less(t, float64(1), float64(2))
//	assert.Less(t, "a", "b")
func Less(t TestingT, e1 interface{}, e2 interface{}, msgAndArgs ...interface{}) bool {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	return compareTwoValues(t, e1, e2, []compareResult{compareLess}, "\"%v\" is not less than \"%v\"", msgAndArgs...)
}

// LessOrEqual asserts that the first element is less than or equal to the second
//
//	assert.LessOrEqual(t, 1, 2)
//	assert.LessOrEqual(t, 2, 2)
//	assert.LessOrEqual(t, "a", "b")
//	assert.LessOrEqual(t, "b", "b")
func LessOrEqual(t TestingT, e1 interface{}, e2 interface{}, msgAndArgs ...interface{}) bool {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	return compareTwoValues(t, e1, e2, []compareResult{compareLess, compareEqual}, "\"%v\" is not less than or equal to \"%v\"", msgAndArgs...)
}

// Positive asserts that the specified element is positive
//
//	assert.Positive(t, 1)
//	assert.Positive(t, 1.23)
func Positive(t TestingT, e interface{}, msgAndArgs ...interface{}) bool {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	zero := reflect.Zero(reflect.TypeOf(e))
	return compareTwoValues(t, e, zero.Interface(), []compareResult{compareGreater}, "\"%v\" is not positive", msgAndArgs...)
}

// Negative asserts that the specified element is negative
//
//	assert.Negative(t, -1)
//	assert.Negative(t, -1.23)
func Negative(t TestingT, e interface{}, msgAndArgs ...interface{}) bool {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	zero := reflect.Zero(reflect.TypeOf(e))
	return compareTwoValues(t, e, zero.Interface(), []compareResult{compareLess}, "\"%v\" is not negative", msgAndArgs...)
}

func compareTwoValues(t TestingT, e1 interface{}, e2 interface{}, allowedComparesResults []compareResult, failMessage string, msgAndArgs ...interface{}) bool {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}

	e1Kind := reflect.ValueOf(e1).Kind()
	e2Kind := reflect.ValueOf(e2).Kind()
	if e1Kind != e2Kind {
		return Fail(t, "Elements should be the same type", msgAndArgs...)
	}

	compareResult, isComparable := compare(e1, e2, e1Kind)
	if !isComparable {
		return Fail(t, fmt.Sprintf("Can not compare type \"%s\"", reflect.TypeOf(e1)), msgAndArgs...)
	}

	if !containsValue(allowedComparesResults, compareResult) {
		return Fail(t, fmt.Sprintf(failMessage, e1, e2), msgAndArgs...)
	}

	return true
}

func containsValue(values []compareResult, value compareResult) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}

	return false
}
