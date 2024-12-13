// Code generated with github.com/stretchr/testify/_codegen; DO NOT EDIT.

package assert

import (
	http "net/http"
	url "net/url"
	time "time"
)

// Conditionf uses a Comparison to assert a complex condition.
func Conditionf(t TestingT, comp Comparison, msg string, args ...interface{}) bool {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	return Condition(t, comp, append([]interface{}{msg}, args...)...)
}

// Containsf asserts that the specified string, list(array, slice...) or map contains the
// specified substring or element.
//
//	assert.Containsf(t, "Hello World", "World", "error message %s", "formatted")
//	assert.Containsf(t, ["Hello", "World"], "World", "error message %s", "formatted")
//	assert.Containsf(t, {"Hello": "World"}, "Hello", "error message %s", "formatted")
func Containsf(t TestingT, s interface{}, contains interface{}, msg string, args ...interface{}) bool {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	return Contains(t, s, contains, append([]interface{}{msg}, args...)...)
}

// DirExistsf checks whether a directory exists in the given path. It also fails
// if the path is a file rather a directory or there is an error checking whether it exists.
func DirExistsf(t TestingT, path string, msg string, args ...interface{}) bool {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	return DirExists(t, path, append([]interface{}{msg}, args...)...)
}

// ElementsMatchf asserts that the specified listA(array, slice...) is equal to specified
// listB(array, slice...) ignoring the order of the elements. If there are duplicate elements,
// the number of appearances of each of them in both lists should match.
//
// assert.ElementsMatchf(t, [1, 3, 2, 3], [1, 3, 3, 2], "error message %s", "formatted")
func ElementsMatchf(t TestingT, listA interface{}, listB interface{}, msg string, args ...interface{}) bool {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	return ElementsMatch(t, listA, listB, append([]interface{}{msg}, args...)...)
}

// Emptyf asserts that the specified object is empty.  I.e. nil, "", false, 0 or either
// a slice or a channel with len == 0.
//
//	assert.Emptyf(t, obj, "error message %s", "formatted")
func Emptyf(t TestingT, object interface{}, msg string, args ...interface{}) bool {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	return Empty(t, object, append([]interface{}{msg}, args...)...)
}

// Equalf asserts that two objects are equal.
//
//	assert.Equalf(t, 123, 123, "error message %s", "formatted")
//
// Pointer variable equality is determined based on the equality of the
// referenced values (as opposed to the memory addresses). Function equality
// cannot be determined and will always fail.
func Equalf(t TestingT, expected interface{}, actual interface{}, msg string, args ...interface{}) bool {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	return Equal(t, expected, actual, append([]interface{}{msg}, args...)...)
}

// EqualErrorf asserts that a function returned an error (i.e. not `nil`)
// and that it is equal to the provided error.
//
//	actualObj, err := SomeFunction()
//	assert.EqualErrorf(t, err,  expectedErrorString, "error message %s", "formatted")
func EqualErrorf(t TestingT, theError error, errString string, msg string, args ...interface{}) bool {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	return EqualError(t, theError, errString, append([]interface{}{msg}, args...)...)
}

// EqualExportedValuesf asserts that the types of two objects are equal and their public
// fields are also equal. This is useful for comparing structs that have private fields
// that could potentially differ.
//
//	 type S struct {
//		Exported     	int
//		notExported   	int
//	 }
//	 assert.EqualExportedValuesf(t, S{1, 2}, S{1, 3}, "error message %s", "formatted") => true
//	 assert.EqualExportedValuesf(t, S{1, 2}, S{2, 3}, "error message %s", "formatted") => false
func EqualExportedValuesf(t TestingT, expected interface{}, actual interface{}, msg string, args ...interface{}) bool {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	return EqualExportedValues(t, expected, actual, append([]interface{}{msg}, args...)...)
}

// EqualValuesf asserts that two objects are equal or convertible to the larger
// type and equal.
//
//	assert.EqualValuesf(t, uint32(123), int32(123), "error message %s", "formatted")
func EqualValuesf(t TestingT, expected interface{}, actual interface{}, msg string, args ...interface{}) bool {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	return EqualValues(t, expected, actual, append([]interface{}{msg}, args...)...)
}

// Errorf asserts that a function returned an error (i.e. not `nil`).
//
//	  actualObj, err := SomeFunction()
//	  if assert.Errorf(t, err, "error message %s", "formatted") {
//		   assert.Equal(t, expectedErrorf, err)
//	  }
func Errorf(t TestingT, err error, msg string, args ...interface{}) bool {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	return Error(t, err, append([]interface{}{msg}, args...)...)
}

// ErrorAsf asserts that at least one of the errors in err's chain matches target, and if so, sets target to that error value.
// This is a wrapper for errors.As.
func ErrorAsf(t TestingT, err error, target interface{}, msg string, args ...interface{}) bool {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	return ErrorAs(t, err, target, append([]interface{}{msg}, args...)...)
}

// ErrorContainsf asserts that a function returned an error (i.e. not `nil`)
// and that the error contains the specified substring.
//
//	actualObj, err := SomeFunction()
//	assert.ErrorContainsf(t, err,  expectedErrorSubString, "error message %s", "formatted")
func ErrorContainsf(t TestingT, theError error, contains string, msg string, args ...interface{}) bool {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	return ErrorContains(t, theError, contains, append([]interface{}{msg}, args...)...)
}

// ErrorIsf asserts that at least one of the errors in err's chain matches target.
// This is a wrapper for errors.Is.
func ErrorIsf(t TestingT, err error, target error, msg string, args ...interface{}) bool {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	return ErrorIs(t, err, target, append([]interface{}{msg}, args...)...)
}

// Eventuallyf asserts that given condition will be met in waitFor time,
// periodically checking target function each tick.
//
//	assert.Eventuallyf(t, func() bool { return true; }, time.Second, 10*time.Millisecond, "error message %s", "formatted")
func Eventuallyf(t TestingT, condition func() bool, waitFor time.Duration, tick time.Duration, msg string, args ...interface{}) bool {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	return Eventually(t, condition, waitFor, tick, append([]interface{}{msg}, args...)...)
}

// EventuallyWithTf asserts that given condition will be met in waitFor time,
// periodically checking target function each tick. In contrast to Eventually,
// it supplies a CollectT to the condition function, so that the condition
// function can use the CollectT to call other assertions.
// The condition is considered "met" if no errors are raised in a tick.
// The supplied CollectT collects all errors from one tick (if there are any).
// If the condition is not met before waitFor, the collected errors of
// the last tick are copied to t.
//
//	externalValue := false
//	go func() {
//		time.Sleep(8*time.Second)
//		externalValue = true
//	}()
//	assert.EventuallyWithTf(t, func(c *assert.CollectT, "error message %s", "formatted") {
//		// add assertions as needed; any assertion failure will fail the current tick
//		assert.True(c, externalValue, "expected 'externalValue' to be true")
//	}, 10*time.Second, 1*time.Second, "external state has not changed to 'true'; still false")
func EventuallyWithTf(t TestingT, condition func(collect *CollectT), waitFor time.Duration, tick time.Duration, msg string, args ...interface{}) bool {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	return EventuallyWithT(t, condition, waitFor, tick, append([]interface{}{msg}, args...)...)
}

// Exactlyf asserts that two objects are equal in value and type.
//
//	assert.Exactlyf(t, int32(123), int64(123), "error message %s", "formatted")
func Exactlyf(t TestingT, expected interface{}, actual interface{}, msg string, args ...interface{}) bool {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	return Exactly(t, expected, actual, append([]interface{}{msg}, args...)...)
}

// Failf reports a failure through
func Failf(t TestingT, failureMessage string, msg string, args ...interface{}) bool {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	return Fail(t, failureMessage, append([]interface{}{msg}, args...)...)
}

// FailNowf fails test
func FailNowf(t TestingT, failureMessage string, msg string, args ...interface{}) bool {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	return FailNow(t, failureMessage, append([]interface{}{msg}, args...)...)
}

// Falsef asserts that the specified value is false.
//
//	assert.Falsef(t, myBool, "error message %s", "formatted")
func Falsef(t TestingT, value bool, msg string, args ...interface{}) bool {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	return False(t, value, append([]interface{}{msg}, args...)...)
}

// FileExistsf checks whether a file exists in the given path. It also fails if
// the path points to a directory or there is an error when trying to check the file.
func FileExistsf(t TestingT, path string, msg string, args ...interface{}) bool {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	return FileExists(t, path, append([]interface{}{msg}, args...)...)
}

// Greaterf asserts that the first element is greater than the second
//
//	assert.Greaterf(t, 2, 1, "error message %s", "formatted")
//	assert.Greaterf(t, float64(2), float64(1), "error message %s", "formatted")
//	assert.Greaterf(t, "b", "a", "error message %s", "formatted")
func Greaterf(t TestingT, e1 interface{}, e2 interface{}, msg string, args ...interface{}) bool {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	return Greater(t, e1, e2, append([]interface{}{msg}, args...)...)
}

// GreaterOrEqualf asserts that the first element is greater than or equal to the second
//
//	assert.GreaterOrEqualf(t, 2, 1, "error message %s", "formatted")
//	assert.GreaterOrEqualf(t, 2, 2, "error message %s", "formatted")
//	assert.GreaterOrEqualf(t, "b", "a", "error message %s", "formatted")
//	assert.GreaterOrEqualf(t, "b", "b", "error message %s", "formatted")
func GreaterOrEqualf(t TestingT, e1 interface{}, e2 interface{}, msg string, args ...interface{}) bool {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	return GreaterOrEqual(t, e1, e2, append([]interface{}{msg}, args...)...)
}

// HTTPBodyContainsf asserts that a specified handler returns a
// body that contains a string.
//
//	assert.HTTPBodyContainsf(t, myHandler, "GET", "www.google.com", nil, "I'm Feeling Lucky", "error message %s", "formatted")
//
// Returns whether the assertion was successful (true) or not (false).
func HTTPBodyContainsf(t TestingT, handler http.HandlerFunc, method string, url string, values url.Values, str interface{}, msg string, args ...interface{}) bool {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	return HTTPBodyContains(t, handler, method, url, values, str, append([]interface{}{msg}, args...)...)
}

// HTTPBodyNotContainsf asserts that a specified handler returns a
// body that does not contain a string.
//
//	assert.HTTPBodyNotContainsf(t, myHandler, "GET", "www.google.com", nil, "I'm Feeling Lucky", "error message %s", "formatted")
//
// Returns whether the assertion was successful (true) or not (false).
func HTTPBodyNotContainsf(t TestingT, handler http.HandlerFunc, method string, url string, values url.Values, str interface{}, msg string, args ...interface{}) bool {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	return HTTPBodyNotContains(t, handler, method, url, values, str, append([]interface{}{msg}, args...)...)
}

// HTTPErrorf asserts that a specified handler returns an error status code.
//
//	assert.HTTPErrorf(t, myHandler, "POST", "/a/b/c", url.Values{"a": []string{"b", "c"}}
//
// Returns whether the assertion was successful (true) or not (false).
func HTTPErrorf(t TestingT, handler http.HandlerFunc, method string, url string, values url.Values, msg string, args ...interface{}) bool {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	return HTTPError(t, handler, method, url, values, append([]interface{}{msg}, args...)...)
}

// HTTPRedirectf asserts that a specified handler returns a redirect status code.
//
//	assert.HTTPRedirectf(t, myHandler, "GET", "/a/b/c", url.Values{"a": []string{"b", "c"}}
//
// Returns whether the assertion was successful (true) or not (false).
func HTTPRedirectf(t TestingT, handler http.HandlerFunc, method string, url string, values url.Values, msg string, args ...interface{}) bool {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	return HTTPRedirect(t, handler, method, url, values, append([]interface{}{msg}, args...)...)
}

// HTTPStatusCodef asserts that a specified handler returns a specified status code.
//
//	assert.HTTPStatusCodef(t, myHandler, "GET", "/notImplemented", nil, 501, "error message %s", "formatted")
//
// Returns whether the assertion was successful (true) or not (false).
func HTTPStatusCodef(t TestingT, handler http.HandlerFunc, method string, url string, values url.Values, statuscode int, msg string, args ...interface{}) bool {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	return HTTPStatusCode(t, handler, method, url, values, statuscode, append([]interface{}{msg}, args...)...)
}

// HTTPSuccessf asserts that a specified handler returns a success status code.
//
//	assert.HTTPSuccessf(t, myHandler, "POST", "http://www.google.com", nil, "error message %s", "formatted")
//
// Returns whether the assertion was successful (true) or not (false).
func HTTPSuccessf(t TestingT, handler http.HandlerFunc, method string, url string, values url.Values, msg string, args ...interface{}) bool {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	return HTTPSuccess(t, handler, method, url, values, append([]interface{}{msg}, args...)...)
}

// Implementsf asserts that an object is implemented by the specified interface.
//
//	assert.Implementsf(t, (*MyInterface)(nil), new(MyObject), "error message %s", "formatted")
func Implementsf(t TestingT, interfaceObject interface{}, object interface{}, msg string, args ...interface{}) bool {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	return Implements(t, interfaceObject, object, append([]interface{}{msg}, args...)...)
}

// InDeltaf asserts that the two numerals are within delta of each other.
//
//	assert.InDeltaf(t, math.Pi, 22/7.0, 0.01, "error message %s", "formatted")
func InDeltaf(t TestingT, expected interface{}, actual interface{}, delta float64, msg string, args ...interface{}) bool {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	return InDelta(t, expected, actual, delta, append([]interface{}{msg}, args...)...)
}

// InDeltaMapValuesf is the same as InDelta, but it compares all values between two maps. Both maps must have exactly the same keys.
func InDeltaMapValuesf(t TestingT, expected interface{}, actual interface{}, delta float64, msg string, args ...interface{}) bool {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	return InDeltaMapValues(t, expected, actual, delta, append([]interface{}{msg}, args...)...)
}

// InDeltaSlicef is the same as InDelta, except it compares two slices.
func InDeltaSlicef(t TestingT, expected interface{}, actual interface{}, delta float64, msg string, args ...interface{}) bool {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	return InDeltaSlice(t, expected, actual, delta, append([]interface{}{msg}, args...)...)
}

// InEpsilonf asserts that expected and actual have a relative error less than epsilon
func InEpsilonf(t TestingT, expected interface{}, actual interface{}, epsilon float64, msg string, args ...interface{}) bool {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	return InEpsilon(t, expected, actual, epsilon, append([]interface{}{msg}, args...)...)
}

// InEpsilonSlicef is the same as InEpsilon, except it compares each value from two slices.
func InEpsilonSlicef(t TestingT, expected interface{}, actual interface{}, epsilon float64, msg string, args ...interface{}) bool {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	return InEpsilonSlice(t, expected, actual, epsilon, append([]interface{}{msg}, args...)...)
}

// IsDecreasingf asserts that the collection is decreasing
//
//	assert.IsDecreasingf(t, []int{2, 1, 0}, "error message %s", "formatted")
//	assert.IsDecreasingf(t, []float{2, 1}, "error message %s", "formatted")
//	assert.IsDecreasingf(t, []string{"b", "a"}, "error message %s", "formatted")
func IsDecreasingf(t TestingT, object interface{}, msg string, args ...interface{}) bool {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	return IsDecreasing(t, object, append([]interface{}{msg}, args...)...)
}

// IsIncreasingf asserts that the collection is increasing
//
//	assert.IsIncreasingf(t, []int{1, 2, 3}, "error message %s", "formatted")
//	assert.IsIncreasingf(t, []float{1, 2}, "error message %s", "formatted")
//	assert.IsIncreasingf(t, []string{"a", "b"}, "error message %s", "formatted")
func IsIncreasingf(t TestingT, object interface{}, msg string, args ...interface{}) bool {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	return IsIncreasing(t, object, append([]interface{}{msg}, args...)...)
}

// IsNonDecreasingf asserts that the collection is not decreasing
//
//	assert.IsNonDecreasingf(t, []int{1, 1, 2}, "error message %s", "formatted")
//	assert.IsNonDecreasingf(t, []float{1, 2}, "error message %s", "formatted")
//	assert.IsNonDecreasingf(t, []string{"a", "b"}, "error message %s", "formatted")
func IsNonDecreasingf(t TestingT, object interface{}, msg string, args ...interface{}) bool {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	return IsNonDecreasing(t, object, append([]interface{}{msg}, args...)...)
}

// IsNonIncreasingf asserts that the collection is not increasing
//
//	assert.IsNonIncreasingf(t, []int{2, 1, 1}, "error message %s", "formatted")
//	assert.IsNonIncreasingf(t, []float{2, 1}, "error message %s", "formatted")
//	assert.IsNonIncreasingf(t, []string{"b", "a"}, "error message %s", "formatted")
func IsNonIncreasingf(t TestingT, object interface{}, msg string, args ...interface{}) bool {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	return IsNonIncreasing(t, object, append([]interface{}{msg}, args...)...)
}

// IsTypef asserts that the specified objects are of the same type.
func IsTypef(t TestingT, expectedType interface{}, object interface{}, msg string, args ...interface{}) bool {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	return IsType(t, expectedType, object, append([]interface{}{msg}, args...)...)
}

// JSONEqf asserts that two JSON strings are equivalent.
//
//	assert.JSONEqf(t, `{"hello": "world", "foo": "bar"}`, `{"foo": "bar", "hello": "world"}`, "error message %s", "formatted")
func JSONEqf(t TestingT, expected string, actual string, msg string, args ...interface{}) bool {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	return JSONEq(t, expected, actual, append([]interface{}{msg}, args...)...)
}

// Lenf asserts that the specified object has specific length.
// Lenf also fails if the object has a type that len() not accept.
//
//	assert.Lenf(t, mySlice, 3, "error message %s", "formatted")
func Lenf(t TestingT, object interface{}, length int, msg string, args ...interface{}) bool {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	return Len(t, object, length, append([]interface{}{msg}, args...)...)
}

// Lessf asserts that the first element is less than the second
//
//	assert.Lessf(t, 1, 2, "error message %s", "formatted")
//	assert.Lessf(t, float64(1), float64(2), "error message %s", "formatted")
//	assert.Lessf(t, "a", "b", "error message %s", "formatted")
func Lessf(t TestingT, e1 interface{}, e2 interface{}, msg string, args ...interface{}) bool {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	return Less(t, e1, e2, append([]interface{}{msg}, args...)...)
}

// LessOrEqualf asserts that the first element is less than or equal to the second
//
//	assert.LessOrEqualf(t, 1, 2, "error message %s", "formatted")
//	assert.LessOrEqualf(t, 2, 2, "error message %s", "formatted")
//	assert.LessOrEqualf(t, "a", "b", "error message %s", "formatted")
//	assert.LessOrEqualf(t, "b", "b", "error message %s", "formatted")
func LessOrEqualf(t TestingT, e1 interface{}, e2 interface{}, msg string, args ...interface{}) bool {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	return LessOrEqual(t, e1, e2, append([]interface{}{msg}, args...)...)
}

// Negativef asserts that the specified element is negative
//
//	assert.Negativef(t, -1, "error message %s", "formatted")
//	assert.Negativef(t, -1.23, "error message %s", "formatted")
func Negativef(t TestingT, e interface{}, msg string, args ...interface{}) bool {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	return Negative(t, e, append([]interface{}{msg}, args...)...)
}

// Neverf asserts that the given condition doesn't satisfy in waitFor time,
// periodically checking the target function each tick.
//
//	assert.Neverf(t, func() bool { return false; }, time.Second, 10*time.Millisecond, "error message %s", "formatted")
func Neverf(t TestingT, condition func() bool, waitFor time.Duration, tick time.Duration, msg string, args ...interface{}) bool {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	return Never(t, condition, waitFor, tick, append([]interface{}{msg}, args...)...)
}

// Nilf asserts that the specified object is nil.
//
//	assert.Nilf(t, err, "error message %s", "formatted")
func Nilf(t TestingT, object interface{}, msg string, args ...interface{}) bool {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	return Nil(t, object, append([]interface{}{msg}, args...)...)
}

// NoDirExistsf checks whether a directory does not exist in the given path.
// It fails if the path points to an existing _directory_ only.
func NoDirExistsf(t TestingT, path string, msg string, args ...interface{}) bool {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	return NoDirExists(t, path, append([]interface{}{msg}, args...)...)
}

// NoErrorf asserts that a function returned no error (i.e. `nil`).
//
//	  actualObj, err := SomeFunction()
//	  if assert.NoErrorf(t, err, "error message %s", "formatted") {
//		   assert.Equal(t, expectedObj, actualObj)
//	  }
func NoErrorf(t TestingT, err error, msg string, args ...interface{}) bool {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	return NoError(t, err, append([]interface{}{msg}, args...)...)
}

// NoFileExistsf checks whether a file does not exist in a given path. It fails
// if the path points to an existing _file_ only.
func NoFileExistsf(t TestingT, path string, msg string, args ...interface{}) bool {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	return NoFileExists(t, path, append([]interface{}{msg}, args...)...)
}

// NotContainsf asserts that the specified string, list(array, slice...) or map does NOT contain the
// specified substring or element.
//
//	assert.NotContainsf(t, "Hello World", "Earth", "error message %s", "formatted")
//	assert.NotContainsf(t, ["Hello", "World"], "Earth", "error message %s", "formatted")
//	assert.NotContainsf(t, {"Hello": "World"}, "Earth", "error message %s", "formatted")
func NotContainsf(t TestingT, s interface{}, contains interface{}, msg string, args ...interface{}) bool {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	return NotContains(t, s, contains, append([]interface{}{msg}, args...)...)
}

// NotElementsMatchf asserts that the specified listA(array, slice...) is NOT equal to specified
// listB(array, slice...) ignoring the order of the elements. If there are duplicate elements,
// the number of appearances of each of them in both lists should not match.
// This is an inverse of ElementsMatch.
//
// assert.NotElementsMatchf(t, [1, 1, 2, 3], [1, 1, 2, 3], "error message %s", "formatted") -> false
//
// assert.NotElementsMatchf(t, [1, 1, 2, 3], [1, 2, 3], "error message %s", "formatted") -> true
//
// assert.NotElementsMatchf(t, [1, 2, 3], [1, 2, 4], "error message %s", "formatted") -> true
func NotElementsMatchf(t TestingT, listA interface{}, listB interface{}, msg string, args ...interface{}) bool {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	return NotElementsMatch(t, listA, listB, append([]interface{}{msg}, args...)...)
}

// NotEmptyf asserts that the specified object is NOT empty.  I.e. not nil, "", false, 0 or either
// a slice or a channel with len == 0.
//
//	if assert.NotEmptyf(t, obj, "error message %s", "formatted") {
//	  assert.Equal(t, "two", obj[1])
//	}
func NotEmptyf(t TestingT, object interface{}, msg string, args ...interface{}) bool {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	return NotEmpty(t, object, append([]interface{}{msg}, args...)...)
}

// NotEqualf asserts that the specified values are NOT equal.
//
//	assert.NotEqualf(t, obj1, obj2, "error message %s", "formatted")
//
// Pointer variable equality is determined based on the equality of the
// referenced values (as opposed to the memory addresses).
func NotEqualf(t TestingT, expected interface{}, actual interface{}, msg string, args ...interface{}) bool {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	return NotEqual(t, expected, actual, append([]interface{}{msg}, args...)...)
}

// NotEqualValuesf asserts that two objects are not equal even when converted to the same type
//
//	assert.NotEqualValuesf(t, obj1, obj2, "error message %s", "formatted")
func NotEqualValuesf(t TestingT, expected interface{}, actual interface{}, msg string, args ...interface{}) bool {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	return NotEqualValues(t, expected, actual, append([]interface{}{msg}, args...)...)
}

// NotErrorAsf asserts that none of the errors in err's chain matches target,
// but if so, sets target to that error value.
func NotErrorAsf(t TestingT, err error, target interface{}, msg string, args ...interface{}) bool {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	return NotErrorAs(t, err, target, append([]interface{}{msg}, args...)...)
}

// NotErrorIsf asserts that none of the errors in err's chain matches target.
// This is a wrapper for errors.Is.
func NotErrorIsf(t TestingT, err error, target error, msg string, args ...interface{}) bool {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	return NotErrorIs(t, err, target, append([]interface{}{msg}, args...)...)
}

// NotImplementsf asserts that an object does not implement the specified interface.
//
//	assert.NotImplementsf(t, (*MyInterface)(nil), new(MyObject), "error message %s", "formatted")
func NotImplementsf(t TestingT, interfaceObject interface{}, object interface{}, msg string, args ...interface{}) bool {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	return NotImplements(t, interfaceObject, object, append([]interface{}{msg}, args...)...)
}

// NotNilf asserts that the specified object is not nil.
//
//	assert.NotNilf(t, err, "error message %s", "formatted")
func NotNilf(t TestingT, object interface{}, msg string, args ...interface{}) bool {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	return NotNil(t, object, append([]interface{}{msg}, args...)...)
}

// NotPanicsf asserts that the code inside the specified PanicTestFunc does NOT panic.
//
//	assert.NotPanicsf(t, func(){ RemainCalm() }, "error message %s", "formatted")
func NotPanicsf(t TestingT, f PanicTestFunc, msg string, args ...interface{}) bool {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	return NotPanics(t, f, append([]interface{}{msg}, args...)...)
}

// NotRegexpf asserts that a specified regexp does not match a string.
//
//	assert.NotRegexpf(t, regexp.MustCompile("starts"), "it's starting", "error message %s", "formatted")
//	assert.NotRegexpf(t, "^start", "it's not starting", "error message %s", "formatted")
func NotRegexpf(t TestingT, rx interface{}, str interface{}, msg string, args ...interface{}) bool {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	return NotRegexp(t, rx, str, append([]interface{}{msg}, args...)...)
}

// NotSamef asserts that two pointers do not reference the same object.
//
//	assert.NotSamef(t, ptr1, ptr2, "error message %s", "formatted")
//
// Both arguments must be pointer variables. Pointer variable sameness is
// determined based on the equality of both type and value.
func NotSamef(t TestingT, expected interface{}, actual interface{}, msg string, args ...interface{}) bool {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	return NotSame(t, expected, actual, append([]interface{}{msg}, args...)...)
}

// NotSubsetf asserts that the specified list(array, slice...) or map does NOT
// contain all elements given in the specified subset list(array, slice...) or
// map.
//
//	assert.NotSubsetf(t, [1, 3, 4], [1, 2], "error message %s", "formatted")
//	assert.NotSubsetf(t, {"x": 1, "y": 2}, {"z": 3}, "error message %s", "formatted")
func NotSubsetf(t TestingT, list interface{}, subset interface{}, msg string, args ...interface{}) bool {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	return NotSubset(t, list, subset, append([]interface{}{msg}, args...)...)
}

// NotZerof asserts that i is not the zero value for its type.
func NotZerof(t TestingT, i interface{}, msg string, args ...interface{}) bool {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	return NotZero(t, i, append([]interface{}{msg}, args...)...)
}

// Panicsf asserts that the code inside the specified PanicTestFunc panics.
//
//	assert.Panicsf(t, func(){ GoCrazy() }, "error message %s", "formatted")
func Panicsf(t TestingT, f PanicTestFunc, msg string, args ...interface{}) bool {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	return Panics(t, f, append([]interface{}{msg}, args...)...)
}

// PanicsWithErrorf asserts that the code inside the specified PanicTestFunc
// panics, and that the recovered panic value is an error that satisfies the
// EqualError comparison.
//
//	assert.PanicsWithErrorf(t, "crazy error", func(){ GoCrazy() }, "error message %s", "formatted")
func PanicsWithErrorf(t TestingT, errString string, f PanicTestFunc, msg string, args ...interface{}) bool {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	return PanicsWithError(t, errString, f, append([]interface{}{msg}, args...)...)
}

// PanicsWithValuef asserts that the code inside the specified PanicTestFunc panics, and that
// the recovered panic value equals the expected panic value.
//
//	assert.PanicsWithValuef(t, "crazy error", func(){ GoCrazy() }, "error message %s", "formatted")
func PanicsWithValuef(t TestingT, expected interface{}, f PanicTestFunc, msg string, args ...interface{}) bool {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	return PanicsWithValue(t, expected, f, append([]interface{}{msg}, args...)...)
}

// Positivef asserts that the specified element is positive
//
//	assert.Positivef(t, 1, "error message %s", "formatted")
//	assert.Positivef(t, 1.23, "error message %s", "formatted")
func Positivef(t TestingT, e interface{}, msg string, args ...interface{}) bool {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	return Positive(t, e, append([]interface{}{msg}, args...)...)
}

// Regexpf asserts that a specified regexp matches a string.
//
//	assert.Regexpf(t, regexp.MustCompile("start"), "it's starting", "error message %s", "formatted")
//	assert.Regexpf(t, "start...$", "it's not starting", "error message %s", "formatted")
func Regexpf(t TestingT, rx interface{}, str interface{}, msg string, args ...interface{}) bool {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	return Regexp(t, rx, str, append([]interface{}{msg}, args...)...)
}

// Samef asserts that two pointers reference the same object.
//
//	assert.Samef(t, ptr1, ptr2, "error message %s", "formatted")
//
// Both arguments must be pointer variables. Pointer variable sameness is
// determined based on the equality of both type and value.
func Samef(t TestingT, expected interface{}, actual interface{}, msg string, args ...interface{}) bool {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	return Same(t, expected, actual, append([]interface{}{msg}, args...)...)
}

// Subsetf asserts that the specified list(array, slice...) or map contains all
// elements given in the specified subset list(array, slice...) or map.
//
//	assert.Subsetf(t, [1, 2, 3], [1, 2], "error message %s", "formatted")
//	assert.Subsetf(t, {"x": 1, "y": 2}, {"x": 1}, "error message %s", "formatted")
func Subsetf(t TestingT, list interface{}, subset interface{}, msg string, args ...interface{}) bool {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	return Subset(t, list, subset, append([]interface{}{msg}, args...)...)
}

// Truef asserts that the specified value is true.
//
//	assert.Truef(t, myBool, "error message %s", "formatted")
func Truef(t TestingT, value bool, msg string, args ...interface{}) bool {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	return True(t, value, append([]interface{}{msg}, args...)...)
}

// WithinDurationf asserts that the two times are within duration delta of each other.
//
//	assert.WithinDurationf(t, time.Now(), time.Now(), 10*time.Second, "error message %s", "formatted")
func WithinDurationf(t TestingT, expected time.Time, actual time.Time, delta time.Duration, msg string, args ...interface{}) bool {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	return WithinDuration(t, expected, actual, delta, append([]interface{}{msg}, args...)...)
}

// WithinRangef asserts that a time is within a time range (inclusive).
//
//	assert.WithinRangef(t, time.Now(), time.Now().Add(-time.Second), time.Now().Add(time.Second), "error message %s", "formatted")
func WithinRangef(t TestingT, actual time.Time, start time.Time, end time.Time, msg string, args ...interface{}) bool {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	return WithinRange(t, actual, start, end, append([]interface{}{msg}, args...)...)
}

// YAMLEqf asserts that two YAML strings are equivalent.
func YAMLEqf(t TestingT, expected string, actual string, msg string, args ...interface{}) bool {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	return YAMLEq(t, expected, actual, append([]interface{}{msg}, args...)...)
}

// Zerof asserts that i is the zero value for its type.
func Zerof(t TestingT, i interface{}, msg string, args ...interface{}) bool {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	return Zero(t, i, append([]interface{}{msg}, args...)...)
}
