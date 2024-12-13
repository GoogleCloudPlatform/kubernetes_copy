package assert

import (
	"fmt"
	"reflect"
)

// isOrdered checks that collection contains orderable elements.
func isOrdered(t TestingT, object interface{}, allowedComparesResults []compareResult, failMessage string, msgAndArgs ...interface{}) bool {
	objKind := reflect.TypeOf(object).Kind()
	if objKind != reflect.Slice && objKind != reflect.Array {
		return false
	}

	objValue := reflect.ValueOf(object)
	objLen := objValue.Len()

	if objLen <= 1 {
		return true
	}

	value := objValue.Index(0)
	valueInterface := value.Interface()
	firstValueKind := value.Kind()

	for i := 1; i < objLen; i++ {
		prevValue := value
		prevValueInterface := valueInterface

		value = objValue.Index(i)
		valueInterface = value.Interface()

		compareResult, isComparable := compare(prevValueInterface, valueInterface, firstValueKind)

		if !isComparable {
			return Fail(t, fmt.Sprintf("Can not compare type \"%s\" and \"%s\"", reflect.TypeOf(value), reflect.TypeOf(prevValue)), msgAndArgs...)
		}

		if !containsValue(allowedComparesResults, compareResult) {
			return Fail(t, fmt.Sprintf(failMessage, prevValue, value), msgAndArgs...)
		}
	}

	return true
}

// IsIncreasing asserts that the collection is increasing
//
//	assert.IsIncreasing(t, []int{1, 2, 3})
//	assert.IsIncreasing(t, []float{1, 2})
//	assert.IsIncreasing(t, []string{"a", "b"})
func IsIncreasing(t TestingT, object interface{}, msgAndArgs ...interface{}) bool {
	return isOrdered(t, object, []compareResult{compareLess}, "\"%v\" is not less than \"%v\"", msgAndArgs...)
}

// IsNonIncreasing asserts that the collection is not increasing
//
//	assert.IsNonIncreasing(t, []int{2, 1, 1})
//	assert.IsNonIncreasing(t, []float{2, 1})
//	assert.IsNonIncreasing(t, []string{"b", "a"})
func IsNonIncreasing(t TestingT, object interface{}, msgAndArgs ...interface{}) bool {
	return isOrdered(t, object, []compareResult{compareEqual, compareGreater}, "\"%v\" is not greater than or equal to \"%v\"", msgAndArgs...)
}

// IsDecreasing asserts that the collection is decreasing
//
//	assert.IsDecreasing(t, []int{2, 1, 0})
//	assert.IsDecreasing(t, []float{2, 1})
//	assert.IsDecreasing(t, []string{"b", "a"})
func IsDecreasing(t TestingT, object interface{}, msgAndArgs ...interface{}) bool {
	return isOrdered(t, object, []compareResult{compareGreater}, "\"%v\" is not greater than \"%v\"", msgAndArgs...)
}

// IsNonDecreasing asserts that the collection is not decreasing
//
//	assert.IsNonDecreasing(t, []int{1, 1, 2})
//	assert.IsNonDecreasing(t, []float{1, 2})
//	assert.IsNonDecreasing(t, []string{"a", "b"})
func IsNonDecreasing(t TestingT, object interface{}, msgAndArgs ...interface{}) bool {
	return isOrdered(t, object, []compareResult{compareLess, compareEqual}, "\"%v\" is not less than or equal to \"%v\"", msgAndArgs...)
}
