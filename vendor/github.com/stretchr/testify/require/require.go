// Code generated with github.com/stretchr/testify/_codegen; DO NOT EDIT.

package require

import (
	assert "github.com/stretchr/testify/assert"
	http "net/http"
	url "net/url"
	time "time"
)

// Condition uses a Comparison to assert a complex condition.
func Condition(t TestingT, comp assert.Comparison, msgAndArgs ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.Condition(t, comp, msgAndArgs...) {
		return
	}
	t.FailNow()
}

// Conditionf uses a Comparison to assert a complex condition.
func Conditionf(t TestingT, comp assert.Comparison, msg string, args ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.Conditionf(t, comp, msg, args...) {
		return
	}
	t.FailNow()
}

// Contains asserts that the specified string, list(array, slice...) or map contains the
// specified substring or element.
//
//	require.Contains(t, "Hello World", "World")
//	require.Contains(t, ["Hello", "World"], "World")
//	require.Contains(t, {"Hello": "World"}, "Hello")
func Contains(t TestingT, s interface{}, contains interface{}, msgAndArgs ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.Contains(t, s, contains, msgAndArgs...) {
		return
	}
	t.FailNow()
}

// Containsf asserts that the specified string, list(array, slice...) or map contains the
// specified substring or element.
//
//	require.Containsf(t, "Hello World", "World", "error message %s", "formatted")
//	require.Containsf(t, ["Hello", "World"], "World", "error message %s", "formatted")
//	require.Containsf(t, {"Hello": "World"}, "Hello", "error message %s", "formatted")
func Containsf(t TestingT, s interface{}, contains interface{}, msg string, args ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.Containsf(t, s, contains, msg, args...) {
		return
	}
	t.FailNow()
}

// DirExists checks whether a directory exists in the given path. It also fails
// if the path is a file rather a directory or there is an error checking whether it exists.
func DirExists(t TestingT, path string, msgAndArgs ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.DirExists(t, path, msgAndArgs...) {
		return
	}
	t.FailNow()
}

// DirExistsf checks whether a directory exists in the given path. It also fails
// if the path is a file rather a directory or there is an error checking whether it exists.
func DirExistsf(t TestingT, path string, msg string, args ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.DirExistsf(t, path, msg, args...) {
		return
	}
	t.FailNow()
}

// ElementsMatch asserts that the specified listA(array, slice...) is equal to specified
// listB(array, slice...) ignoring the order of the elements. If there are duplicate elements,
// the number of appearances of each of them in both lists should match.
//
// require.ElementsMatch(t, [1, 3, 2, 3], [1, 3, 3, 2])
func ElementsMatch(t TestingT, listA interface{}, listB interface{}, msgAndArgs ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.ElementsMatch(t, listA, listB, msgAndArgs...) {
		return
	}
	t.FailNow()
}

// ElementsMatchf asserts that the specified listA(array, slice...) is equal to specified
// listB(array, slice...) ignoring the order of the elements. If there are duplicate elements,
// the number of appearances of each of them in both lists should match.
//
// require.ElementsMatchf(t, [1, 3, 2, 3], [1, 3, 3, 2], "error message %s", "formatted")
func ElementsMatchf(t TestingT, listA interface{}, listB interface{}, msg string, args ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.ElementsMatchf(t, listA, listB, msg, args...) {
		return
	}
	t.FailNow()
}

// Empty asserts that the specified object is empty.  I.e. nil, "", false, 0 or either
// a slice or a channel with len == 0.
//
//	require.Empty(t, obj)
func Empty(t TestingT, object interface{}, msgAndArgs ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.Empty(t, object, msgAndArgs...) {
		return
	}
	t.FailNow()
}

// Emptyf asserts that the specified object is empty.  I.e. nil, "", false, 0 or either
// a slice or a channel with len == 0.
//
//	require.Emptyf(t, obj, "error message %s", "formatted")
func Emptyf(t TestingT, object interface{}, msg string, args ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.Emptyf(t, object, msg, args...) {
		return
	}
	t.FailNow()
}

// Equal asserts that two objects are equal.
//
//	require.Equal(t, 123, 123)
//
// Pointer variable equality is determined based on the equality of the
// referenced values (as opposed to the memory addresses). Function equality
// cannot be determined and will always fail.
func Equal(t TestingT, expected interface{}, actual interface{}, msgAndArgs ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.Equal(t, expected, actual, msgAndArgs...) {
		return
	}
	t.FailNow()
}

// EqualError asserts that a function returned an error (i.e. not `nil`)
// and that it is equal to the provided error.
//
//	actualObj, err := SomeFunction()
//	require.EqualError(t, err,  expectedErrorString)
func EqualError(t TestingT, theError error, errString string, msgAndArgs ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.EqualError(t, theError, errString, msgAndArgs...) {
		return
	}
	t.FailNow()
}

// EqualErrorf asserts that a function returned an error (i.e. not `nil`)
// and that it is equal to the provided error.
//
//	actualObj, err := SomeFunction()
//	require.EqualErrorf(t, err,  expectedErrorString, "error message %s", "formatted")
func EqualErrorf(t TestingT, theError error, errString string, msg string, args ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.EqualErrorf(t, theError, errString, msg, args...) {
		return
	}
	t.FailNow()
}

// EqualExportedValues asserts that the types of two objects are equal and their public
// fields are also equal. This is useful for comparing structs that have private fields
// that could potentially differ.
//
//	 type S struct {
//		Exported     	int
//		notExported   	int
//	 }
//	 require.EqualExportedValues(t, S{1, 2}, S{1, 3}) => true
//	 require.EqualExportedValues(t, S{1, 2}, S{2, 3}) => false
func EqualExportedValues(t TestingT, expected interface{}, actual interface{}, msgAndArgs ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.EqualExportedValues(t, expected, actual, msgAndArgs...) {
		return
	}
	t.FailNow()
}

// EqualExportedValuesf asserts that the types of two objects are equal and their public
// fields are also equal. This is useful for comparing structs that have private fields
// that could potentially differ.
//
//	 type S struct {
//		Exported     	int
//		notExported   	int
//	 }
//	 require.EqualExportedValuesf(t, S{1, 2}, S{1, 3}, "error message %s", "formatted") => true
//	 require.EqualExportedValuesf(t, S{1, 2}, S{2, 3}, "error message %s", "formatted") => false
func EqualExportedValuesf(t TestingT, expected interface{}, actual interface{}, msg string, args ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.EqualExportedValuesf(t, expected, actual, msg, args...) {
		return
	}
	t.FailNow()
}

// EqualValues asserts that two objects are equal or convertible to the larger
// type and equal.
//
//	require.EqualValues(t, uint32(123), int32(123))
func EqualValues(t TestingT, expected interface{}, actual interface{}, msgAndArgs ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.EqualValues(t, expected, actual, msgAndArgs...) {
		return
	}
	t.FailNow()
}

// EqualValuesf asserts that two objects are equal or convertible to the larger
// type and equal.
//
//	require.EqualValuesf(t, uint32(123), int32(123), "error message %s", "formatted")
func EqualValuesf(t TestingT, expected interface{}, actual interface{}, msg string, args ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.EqualValuesf(t, expected, actual, msg, args...) {
		return
	}
	t.FailNow()
}

// Equalf asserts that two objects are equal.
//
//	require.Equalf(t, 123, 123, "error message %s", "formatted")
//
// Pointer variable equality is determined based on the equality of the
// referenced values (as opposed to the memory addresses). Function equality
// cannot be determined and will always fail.
func Equalf(t TestingT, expected interface{}, actual interface{}, msg string, args ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.Equalf(t, expected, actual, msg, args...) {
		return
	}
	t.FailNow()
}

// Error asserts that a function returned an error (i.e. not `nil`).
//
//	  actualObj, err := SomeFunction()
//	  if require.Error(t, err) {
//		   require.Equal(t, expectedError, err)
//	  }
func Error(t TestingT, err error, msgAndArgs ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.Error(t, err, msgAndArgs...) {
		return
	}
	t.FailNow()
}

// ErrorAs asserts that at least one of the errors in err's chain matches target, and if so, sets target to that error value.
// This is a wrapper for errors.As.
func ErrorAs(t TestingT, err error, target interface{}, msgAndArgs ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.ErrorAs(t, err, target, msgAndArgs...) {
		return
	}
	t.FailNow()
}

// ErrorAsf asserts that at least one of the errors in err's chain matches target, and if so, sets target to that error value.
// This is a wrapper for errors.As.
func ErrorAsf(t TestingT, err error, target interface{}, msg string, args ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.ErrorAsf(t, err, target, msg, args...) {
		return
	}
	t.FailNow()
}

// ErrorContains asserts that a function returned an error (i.e. not `nil`)
// and that the error contains the specified substring.
//
//	actualObj, err := SomeFunction()
//	require.ErrorContains(t, err,  expectedErrorSubString)
func ErrorContains(t TestingT, theError error, contains string, msgAndArgs ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.ErrorContains(t, theError, contains, msgAndArgs...) {
		return
	}
	t.FailNow()
}

// ErrorContainsf asserts that a function returned an error (i.e. not `nil`)
// and that the error contains the specified substring.
//
//	actualObj, err := SomeFunction()
//	require.ErrorContainsf(t, err,  expectedErrorSubString, "error message %s", "formatted")
func ErrorContainsf(t TestingT, theError error, contains string, msg string, args ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.ErrorContainsf(t, theError, contains, msg, args...) {
		return
	}
	t.FailNow()
}

// ErrorIs asserts that at least one of the errors in err's chain matches target.
// This is a wrapper for errors.Is.
func ErrorIs(t TestingT, err error, target error, msgAndArgs ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.ErrorIs(t, err, target, msgAndArgs...) {
		return
	}
	t.FailNow()
}

// ErrorIsf asserts that at least one of the errors in err's chain matches target.
// This is a wrapper for errors.Is.
func ErrorIsf(t TestingT, err error, target error, msg string, args ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.ErrorIsf(t, err, target, msg, args...) {
		return
	}
	t.FailNow()
}

// Errorf asserts that a function returned an error (i.e. not `nil`).
//
//	  actualObj, err := SomeFunction()
//	  if require.Errorf(t, err, "error message %s", "formatted") {
//		   require.Equal(t, expectedErrorf, err)
//	  }
func Errorf(t TestingT, err error, msg string, args ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.Errorf(t, err, msg, args...) {
		return
	}
	t.FailNow()
}

// Eventually asserts that given condition will be met in waitFor time,
// periodically checking target function each tick.
//
//	require.Eventually(t, func() bool { return true; }, time.Second, 10*time.Millisecond)
func Eventually(t TestingT, condition func() bool, waitFor time.Duration, tick time.Duration, msgAndArgs ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.Eventually(t, condition, waitFor, tick, msgAndArgs...) {
		return
	}
	t.FailNow()
}

// EventuallyWithT asserts that given condition will be met in waitFor time,
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
//	require.EventuallyWithT(t, func(c *require.CollectT) {
//		// add assertions as needed; any assertion failure will fail the current tick
//		require.True(c, externalValue, "expected 'externalValue' to be true")
//	}, 10*time.Second, 1*time.Second, "external state has not changed to 'true'; still false")
func EventuallyWithT(t TestingT, condition func(collect *assert.CollectT), waitFor time.Duration, tick time.Duration, msgAndArgs ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.EventuallyWithT(t, condition, waitFor, tick, msgAndArgs...) {
		return
	}
	t.FailNow()
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
//	require.EventuallyWithTf(t, func(c *require.CollectT, "error message %s", "formatted") {
//		// add assertions as needed; any assertion failure will fail the current tick
//		require.True(c, externalValue, "expected 'externalValue' to be true")
//	}, 10*time.Second, 1*time.Second, "external state has not changed to 'true'; still false")
func EventuallyWithTf(t TestingT, condition func(collect *assert.CollectT), waitFor time.Duration, tick time.Duration, msg string, args ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.EventuallyWithTf(t, condition, waitFor, tick, msg, args...) {
		return
	}
	t.FailNow()
}

// Eventuallyf asserts that given condition will be met in waitFor time,
// periodically checking target function each tick.
//
//	require.Eventuallyf(t, func() bool { return true; }, time.Second, 10*time.Millisecond, "error message %s", "formatted")
func Eventuallyf(t TestingT, condition func() bool, waitFor time.Duration, tick time.Duration, msg string, args ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.Eventuallyf(t, condition, waitFor, tick, msg, args...) {
		return
	}
	t.FailNow()
}

// Exactly asserts that two objects are equal in value and type.
//
//	require.Exactly(t, int32(123), int64(123))
func Exactly(t TestingT, expected interface{}, actual interface{}, msgAndArgs ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.Exactly(t, expected, actual, msgAndArgs...) {
		return
	}
	t.FailNow()
}

// Exactlyf asserts that two objects are equal in value and type.
//
//	require.Exactlyf(t, int32(123), int64(123), "error message %s", "formatted")
func Exactlyf(t TestingT, expected interface{}, actual interface{}, msg string, args ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.Exactlyf(t, expected, actual, msg, args...) {
		return
	}
	t.FailNow()
}

// Fail reports a failure through
func Fail(t TestingT, failureMessage string, msgAndArgs ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.Fail(t, failureMessage, msgAndArgs...) {
		return
	}
	t.FailNow()
}

// FailNow fails test
func FailNow(t TestingT, failureMessage string, msgAndArgs ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.FailNow(t, failureMessage, msgAndArgs...) {
		return
	}
	t.FailNow()
}

// FailNowf fails test
func FailNowf(t TestingT, failureMessage string, msg string, args ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.FailNowf(t, failureMessage, msg, args...) {
		return
	}
	t.FailNow()
}

// Failf reports a failure through
func Failf(t TestingT, failureMessage string, msg string, args ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.Failf(t, failureMessage, msg, args...) {
		return
	}
	t.FailNow()
}

// False asserts that the specified value is false.
//
//	require.False(t, myBool)
func False(t TestingT, value bool, msgAndArgs ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.False(t, value, msgAndArgs...) {
		return
	}
	t.FailNow()
}

// Falsef asserts that the specified value is false.
//
//	require.Falsef(t, myBool, "error message %s", "formatted")
func Falsef(t TestingT, value bool, msg string, args ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.Falsef(t, value, msg, args...) {
		return
	}
	t.FailNow()
}

// FileExists checks whether a file exists in the given path. It also fails if
// the path points to a directory or there is an error when trying to check the file.
func FileExists(t TestingT, path string, msgAndArgs ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.FileExists(t, path, msgAndArgs...) {
		return
	}
	t.FailNow()
}

// FileExistsf checks whether a file exists in the given path. It also fails if
// the path points to a directory or there is an error when trying to check the file.
func FileExistsf(t TestingT, path string, msg string, args ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.FileExistsf(t, path, msg, args...) {
		return
	}
	t.FailNow()
}

// Greater asserts that the first element is greater than the second
//
//	require.Greater(t, 2, 1)
//	require.Greater(t, float64(2), float64(1))
//	require.Greater(t, "b", "a")
func Greater(t TestingT, e1 interface{}, e2 interface{}, msgAndArgs ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.Greater(t, e1, e2, msgAndArgs...) {
		return
	}
	t.FailNow()
}

// GreaterOrEqual asserts that the first element is greater than or equal to the second
//
//	require.GreaterOrEqual(t, 2, 1)
//	require.GreaterOrEqual(t, 2, 2)
//	require.GreaterOrEqual(t, "b", "a")
//	require.GreaterOrEqual(t, "b", "b")
func GreaterOrEqual(t TestingT, e1 interface{}, e2 interface{}, msgAndArgs ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.GreaterOrEqual(t, e1, e2, msgAndArgs...) {
		return
	}
	t.FailNow()
}

// GreaterOrEqualf asserts that the first element is greater than or equal to the second
//
//	require.GreaterOrEqualf(t, 2, 1, "error message %s", "formatted")
//	require.GreaterOrEqualf(t, 2, 2, "error message %s", "formatted")
//	require.GreaterOrEqualf(t, "b", "a", "error message %s", "formatted")
//	require.GreaterOrEqualf(t, "b", "b", "error message %s", "formatted")
func GreaterOrEqualf(t TestingT, e1 interface{}, e2 interface{}, msg string, args ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.GreaterOrEqualf(t, e1, e2, msg, args...) {
		return
	}
	t.FailNow()
}

// Greaterf asserts that the first element is greater than the second
//
//	require.Greaterf(t, 2, 1, "error message %s", "formatted")
//	require.Greaterf(t, float64(2), float64(1), "error message %s", "formatted")
//	require.Greaterf(t, "b", "a", "error message %s", "formatted")
func Greaterf(t TestingT, e1 interface{}, e2 interface{}, msg string, args ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.Greaterf(t, e1, e2, msg, args...) {
		return
	}
	t.FailNow()
}

// HTTPBodyContains asserts that a specified handler returns a
// body that contains a string.
//
//	require.HTTPBodyContains(t, myHandler, "GET", "www.google.com", nil, "I'm Feeling Lucky")
//
// Returns whether the assertion was successful (true) or not (false).
func HTTPBodyContains(t TestingT, handler http.HandlerFunc, method string, url string, values url.Values, str interface{}, msgAndArgs ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.HTTPBodyContains(t, handler, method, url, values, str, msgAndArgs...) {
		return
	}
	t.FailNow()
}

// HTTPBodyContainsf asserts that a specified handler returns a
// body that contains a string.
//
//	require.HTTPBodyContainsf(t, myHandler, "GET", "www.google.com", nil, "I'm Feeling Lucky", "error message %s", "formatted")
//
// Returns whether the assertion was successful (true) or not (false).
func HTTPBodyContainsf(t TestingT, handler http.HandlerFunc, method string, url string, values url.Values, str interface{}, msg string, args ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.HTTPBodyContainsf(t, handler, method, url, values, str, msg, args...) {
		return
	}
	t.FailNow()
}

// HTTPBodyNotContains asserts that a specified handler returns a
// body that does not contain a string.
//
//	require.HTTPBodyNotContains(t, myHandler, "GET", "www.google.com", nil, "I'm Feeling Lucky")
//
// Returns whether the assertion was successful (true) or not (false).
func HTTPBodyNotContains(t TestingT, handler http.HandlerFunc, method string, url string, values url.Values, str interface{}, msgAndArgs ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.HTTPBodyNotContains(t, handler, method, url, values, str, msgAndArgs...) {
		return
	}
	t.FailNow()
}

// HTTPBodyNotContainsf asserts that a specified handler returns a
// body that does not contain a string.
//
//	require.HTTPBodyNotContainsf(t, myHandler, "GET", "www.google.com", nil, "I'm Feeling Lucky", "error message %s", "formatted")
//
// Returns whether the assertion was successful (true) or not (false).
func HTTPBodyNotContainsf(t TestingT, handler http.HandlerFunc, method string, url string, values url.Values, str interface{}, msg string, args ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.HTTPBodyNotContainsf(t, handler, method, url, values, str, msg, args...) {
		return
	}
	t.FailNow()
}

// HTTPError asserts that a specified handler returns an error status code.
//
//	require.HTTPError(t, myHandler, "POST", "/a/b/c", url.Values{"a": []string{"b", "c"}}
//
// Returns whether the assertion was successful (true) or not (false).
func HTTPError(t TestingT, handler http.HandlerFunc, method string, url string, values url.Values, msgAndArgs ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.HTTPError(t, handler, method, url, values, msgAndArgs...) {
		return
	}
	t.FailNow()
}

// HTTPErrorf asserts that a specified handler returns an error status code.
//
//	require.HTTPErrorf(t, myHandler, "POST", "/a/b/c", url.Values{"a": []string{"b", "c"}}
//
// Returns whether the assertion was successful (true) or not (false).
func HTTPErrorf(t TestingT, handler http.HandlerFunc, method string, url string, values url.Values, msg string, args ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.HTTPErrorf(t, handler, method, url, values, msg, args...) {
		return
	}
	t.FailNow()
}

// HTTPRedirect asserts that a specified handler returns a redirect status code.
//
//	require.HTTPRedirect(t, myHandler, "GET", "/a/b/c", url.Values{"a": []string{"b", "c"}}
//
// Returns whether the assertion was successful (true) or not (false).
func HTTPRedirect(t TestingT, handler http.HandlerFunc, method string, url string, values url.Values, msgAndArgs ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.HTTPRedirect(t, handler, method, url, values, msgAndArgs...) {
		return
	}
	t.FailNow()
}

// HTTPRedirectf asserts that a specified handler returns a redirect status code.
//
//	require.HTTPRedirectf(t, myHandler, "GET", "/a/b/c", url.Values{"a": []string{"b", "c"}}
//
// Returns whether the assertion was successful (true) or not (false).
func HTTPRedirectf(t TestingT, handler http.HandlerFunc, method string, url string, values url.Values, msg string, args ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.HTTPRedirectf(t, handler, method, url, values, msg, args...) {
		return
	}
	t.FailNow()
}

// HTTPStatusCode asserts that a specified handler returns a specified status code.
//
//	require.HTTPStatusCode(t, myHandler, "GET", "/notImplemented", nil, 501)
//
// Returns whether the assertion was successful (true) or not (false).
func HTTPStatusCode(t TestingT, handler http.HandlerFunc, method string, url string, values url.Values, statuscode int, msgAndArgs ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.HTTPStatusCode(t, handler, method, url, values, statuscode, msgAndArgs...) {
		return
	}
	t.FailNow()
}

// HTTPStatusCodef asserts that a specified handler returns a specified status code.
//
//	require.HTTPStatusCodef(t, myHandler, "GET", "/notImplemented", nil, 501, "error message %s", "formatted")
//
// Returns whether the assertion was successful (true) or not (false).
func HTTPStatusCodef(t TestingT, handler http.HandlerFunc, method string, url string, values url.Values, statuscode int, msg string, args ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.HTTPStatusCodef(t, handler, method, url, values, statuscode, msg, args...) {
		return
	}
	t.FailNow()
}

// HTTPSuccess asserts that a specified handler returns a success status code.
//
//	require.HTTPSuccess(t, myHandler, "POST", "http://www.google.com", nil)
//
// Returns whether the assertion was successful (true) or not (false).
func HTTPSuccess(t TestingT, handler http.HandlerFunc, method string, url string, values url.Values, msgAndArgs ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.HTTPSuccess(t, handler, method, url, values, msgAndArgs...) {
		return
	}
	t.FailNow()
}

// HTTPSuccessf asserts that a specified handler returns a success status code.
//
//	require.HTTPSuccessf(t, myHandler, "POST", "http://www.google.com", nil, "error message %s", "formatted")
//
// Returns whether the assertion was successful (true) or not (false).
func HTTPSuccessf(t TestingT, handler http.HandlerFunc, method string, url string, values url.Values, msg string, args ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.HTTPSuccessf(t, handler, method, url, values, msg, args...) {
		return
	}
	t.FailNow()
}

// Implements asserts that an object is implemented by the specified interface.
//
//	require.Implements(t, (*MyInterface)(nil), new(MyObject))
func Implements(t TestingT, interfaceObject interface{}, object interface{}, msgAndArgs ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.Implements(t, interfaceObject, object, msgAndArgs...) {
		return
	}
	t.FailNow()
}

// Implementsf asserts that an object is implemented by the specified interface.
//
//	require.Implementsf(t, (*MyInterface)(nil), new(MyObject), "error message %s", "formatted")
func Implementsf(t TestingT, interfaceObject interface{}, object interface{}, msg string, args ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.Implementsf(t, interfaceObject, object, msg, args...) {
		return
	}
	t.FailNow()
}

// InDelta asserts that the two numerals are within delta of each other.
//
//	require.InDelta(t, math.Pi, 22/7.0, 0.01)
func InDelta(t TestingT, expected interface{}, actual interface{}, delta float64, msgAndArgs ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.InDelta(t, expected, actual, delta, msgAndArgs...) {
		return
	}
	t.FailNow()
}

// InDeltaMapValues is the same as InDelta, but it compares all values between two maps. Both maps must have exactly the same keys.
func InDeltaMapValues(t TestingT, expected interface{}, actual interface{}, delta float64, msgAndArgs ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.InDeltaMapValues(t, expected, actual, delta, msgAndArgs...) {
		return
	}
	t.FailNow()
}

// InDeltaMapValuesf is the same as InDelta, but it compares all values between two maps. Both maps must have exactly the same keys.
func InDeltaMapValuesf(t TestingT, expected interface{}, actual interface{}, delta float64, msg string, args ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.InDeltaMapValuesf(t, expected, actual, delta, msg, args...) {
		return
	}
	t.FailNow()
}

// InDeltaSlice is the same as InDelta, except it compares two slices.
func InDeltaSlice(t TestingT, expected interface{}, actual interface{}, delta float64, msgAndArgs ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.InDeltaSlice(t, expected, actual, delta, msgAndArgs...) {
		return
	}
	t.FailNow()
}

// InDeltaSlicef is the same as InDelta, except it compares two slices.
func InDeltaSlicef(t TestingT, expected interface{}, actual interface{}, delta float64, msg string, args ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.InDeltaSlicef(t, expected, actual, delta, msg, args...) {
		return
	}
	t.FailNow()
}

// InDeltaf asserts that the two numerals are within delta of each other.
//
//	require.InDeltaf(t, math.Pi, 22/7.0, 0.01, "error message %s", "formatted")
func InDeltaf(t TestingT, expected interface{}, actual interface{}, delta float64, msg string, args ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.InDeltaf(t, expected, actual, delta, msg, args...) {
		return
	}
	t.FailNow()
}

// InEpsilon asserts that expected and actual have a relative error less than epsilon
func InEpsilon(t TestingT, expected interface{}, actual interface{}, epsilon float64, msgAndArgs ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.InEpsilon(t, expected, actual, epsilon, msgAndArgs...) {
		return
	}
	t.FailNow()
}

// InEpsilonSlice is the same as InEpsilon, except it compares each value from two slices.
func InEpsilonSlice(t TestingT, expected interface{}, actual interface{}, epsilon float64, msgAndArgs ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.InEpsilonSlice(t, expected, actual, epsilon, msgAndArgs...) {
		return
	}
	t.FailNow()
}

// InEpsilonSlicef is the same as InEpsilon, except it compares each value from two slices.
func InEpsilonSlicef(t TestingT, expected interface{}, actual interface{}, epsilon float64, msg string, args ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.InEpsilonSlicef(t, expected, actual, epsilon, msg, args...) {
		return
	}
	t.FailNow()
}

// InEpsilonf asserts that expected and actual have a relative error less than epsilon
func InEpsilonf(t TestingT, expected interface{}, actual interface{}, epsilon float64, msg string, args ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.InEpsilonf(t, expected, actual, epsilon, msg, args...) {
		return
	}
	t.FailNow()
}

// IsDecreasing asserts that the collection is decreasing
//
//	require.IsDecreasing(t, []int{2, 1, 0})
//	require.IsDecreasing(t, []float{2, 1})
//	require.IsDecreasing(t, []string{"b", "a"})
func IsDecreasing(t TestingT, object interface{}, msgAndArgs ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.IsDecreasing(t, object, msgAndArgs...) {
		return
	}
	t.FailNow()
}

// IsDecreasingf asserts that the collection is decreasing
//
//	require.IsDecreasingf(t, []int{2, 1, 0}, "error message %s", "formatted")
//	require.IsDecreasingf(t, []float{2, 1}, "error message %s", "formatted")
//	require.IsDecreasingf(t, []string{"b", "a"}, "error message %s", "formatted")
func IsDecreasingf(t TestingT, object interface{}, msg string, args ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.IsDecreasingf(t, object, msg, args...) {
		return
	}
	t.FailNow()
}

// IsIncreasing asserts that the collection is increasing
//
//	require.IsIncreasing(t, []int{1, 2, 3})
//	require.IsIncreasing(t, []float{1, 2})
//	require.IsIncreasing(t, []string{"a", "b"})
func IsIncreasing(t TestingT, object interface{}, msgAndArgs ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.IsIncreasing(t, object, msgAndArgs...) {
		return
	}
	t.FailNow()
}

// IsIncreasingf asserts that the collection is increasing
//
//	require.IsIncreasingf(t, []int{1, 2, 3}, "error message %s", "formatted")
//	require.IsIncreasingf(t, []float{1, 2}, "error message %s", "formatted")
//	require.IsIncreasingf(t, []string{"a", "b"}, "error message %s", "formatted")
func IsIncreasingf(t TestingT, object interface{}, msg string, args ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.IsIncreasingf(t, object, msg, args...) {
		return
	}
	t.FailNow()
}

// IsNonDecreasing asserts that the collection is not decreasing
//
//	require.IsNonDecreasing(t, []int{1, 1, 2})
//	require.IsNonDecreasing(t, []float{1, 2})
//	require.IsNonDecreasing(t, []string{"a", "b"})
func IsNonDecreasing(t TestingT, object interface{}, msgAndArgs ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.IsNonDecreasing(t, object, msgAndArgs...) {
		return
	}
	t.FailNow()
}

// IsNonDecreasingf asserts that the collection is not decreasing
//
//	require.IsNonDecreasingf(t, []int{1, 1, 2}, "error message %s", "formatted")
//	require.IsNonDecreasingf(t, []float{1, 2}, "error message %s", "formatted")
//	require.IsNonDecreasingf(t, []string{"a", "b"}, "error message %s", "formatted")
func IsNonDecreasingf(t TestingT, object interface{}, msg string, args ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.IsNonDecreasingf(t, object, msg, args...) {
		return
	}
	t.FailNow()
}

// IsNonIncreasing asserts that the collection is not increasing
//
//	require.IsNonIncreasing(t, []int{2, 1, 1})
//	require.IsNonIncreasing(t, []float{2, 1})
//	require.IsNonIncreasing(t, []string{"b", "a"})
func IsNonIncreasing(t TestingT, object interface{}, msgAndArgs ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.IsNonIncreasing(t, object, msgAndArgs...) {
		return
	}
	t.FailNow()
}

// IsNonIncreasingf asserts that the collection is not increasing
//
//	require.IsNonIncreasingf(t, []int{2, 1, 1}, "error message %s", "formatted")
//	require.IsNonIncreasingf(t, []float{2, 1}, "error message %s", "formatted")
//	require.IsNonIncreasingf(t, []string{"b", "a"}, "error message %s", "formatted")
func IsNonIncreasingf(t TestingT, object interface{}, msg string, args ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.IsNonIncreasingf(t, object, msg, args...) {
		return
	}
	t.FailNow()
}

// IsType asserts that the specified objects are of the same type.
func IsType(t TestingT, expectedType interface{}, object interface{}, msgAndArgs ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.IsType(t, expectedType, object, msgAndArgs...) {
		return
	}
	t.FailNow()
}

// IsTypef asserts that the specified objects are of the same type.
func IsTypef(t TestingT, expectedType interface{}, object interface{}, msg string, args ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.IsTypef(t, expectedType, object, msg, args...) {
		return
	}
	t.FailNow()
}

// JSONEq asserts that two JSON strings are equivalent.
//
//	require.JSONEq(t, `{"hello": "world", "foo": "bar"}`, `{"foo": "bar", "hello": "world"}`)
func JSONEq(t TestingT, expected string, actual string, msgAndArgs ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.JSONEq(t, expected, actual, msgAndArgs...) {
		return
	}
	t.FailNow()
}

// JSONEqf asserts that two JSON strings are equivalent.
//
//	require.JSONEqf(t, `{"hello": "world", "foo": "bar"}`, `{"foo": "bar", "hello": "world"}`, "error message %s", "formatted")
func JSONEqf(t TestingT, expected string, actual string, msg string, args ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.JSONEqf(t, expected, actual, msg, args...) {
		return
	}
	t.FailNow()
}

// Len asserts that the specified object has specific length.
// Len also fails if the object has a type that len() not accept.
//
//	require.Len(t, mySlice, 3)
func Len(t TestingT, object interface{}, length int, msgAndArgs ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.Len(t, object, length, msgAndArgs...) {
		return
	}
	t.FailNow()
}

// Lenf asserts that the specified object has specific length.
// Lenf also fails if the object has a type that len() not accept.
//
//	require.Lenf(t, mySlice, 3, "error message %s", "formatted")
func Lenf(t TestingT, object interface{}, length int, msg string, args ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.Lenf(t, object, length, msg, args...) {
		return
	}
	t.FailNow()
}

// Less asserts that the first element is less than the second
//
//	require.Less(t, 1, 2)
//	require.Less(t, float64(1), float64(2))
//	require.Less(t, "a", "b")
func Less(t TestingT, e1 interface{}, e2 interface{}, msgAndArgs ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.Less(t, e1, e2, msgAndArgs...) {
		return
	}
	t.FailNow()
}

// LessOrEqual asserts that the first element is less than or equal to the second
//
//	require.LessOrEqual(t, 1, 2)
//	require.LessOrEqual(t, 2, 2)
//	require.LessOrEqual(t, "a", "b")
//	require.LessOrEqual(t, "b", "b")
func LessOrEqual(t TestingT, e1 interface{}, e2 interface{}, msgAndArgs ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.LessOrEqual(t, e1, e2, msgAndArgs...) {
		return
	}
	t.FailNow()
}

// LessOrEqualf asserts that the first element is less than or equal to the second
//
//	require.LessOrEqualf(t, 1, 2, "error message %s", "formatted")
//	require.LessOrEqualf(t, 2, 2, "error message %s", "formatted")
//	require.LessOrEqualf(t, "a", "b", "error message %s", "formatted")
//	require.LessOrEqualf(t, "b", "b", "error message %s", "formatted")
func LessOrEqualf(t TestingT, e1 interface{}, e2 interface{}, msg string, args ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.LessOrEqualf(t, e1, e2, msg, args...) {
		return
	}
	t.FailNow()
}

// Lessf asserts that the first element is less than the second
//
//	require.Lessf(t, 1, 2, "error message %s", "formatted")
//	require.Lessf(t, float64(1), float64(2), "error message %s", "formatted")
//	require.Lessf(t, "a", "b", "error message %s", "formatted")
func Lessf(t TestingT, e1 interface{}, e2 interface{}, msg string, args ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.Lessf(t, e1, e2, msg, args...) {
		return
	}
	t.FailNow()
}

// Negative asserts that the specified element is negative
//
//	require.Negative(t, -1)
//	require.Negative(t, -1.23)
func Negative(t TestingT, e interface{}, msgAndArgs ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.Negative(t, e, msgAndArgs...) {
		return
	}
	t.FailNow()
}

// Negativef asserts that the specified element is negative
//
//	require.Negativef(t, -1, "error message %s", "formatted")
//	require.Negativef(t, -1.23, "error message %s", "formatted")
func Negativef(t TestingT, e interface{}, msg string, args ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.Negativef(t, e, msg, args...) {
		return
	}
	t.FailNow()
}

// Never asserts that the given condition doesn't satisfy in waitFor time,
// periodically checking the target function each tick.
//
//	require.Never(t, func() bool { return false; }, time.Second, 10*time.Millisecond)
func Never(t TestingT, condition func() bool, waitFor time.Duration, tick time.Duration, msgAndArgs ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.Never(t, condition, waitFor, tick, msgAndArgs...) {
		return
	}
	t.FailNow()
}

// Neverf asserts that the given condition doesn't satisfy in waitFor time,
// periodically checking the target function each tick.
//
//	require.Neverf(t, func() bool { return false; }, time.Second, 10*time.Millisecond, "error message %s", "formatted")
func Neverf(t TestingT, condition func() bool, waitFor time.Duration, tick time.Duration, msg string, args ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.Neverf(t, condition, waitFor, tick, msg, args...) {
		return
	}
	t.FailNow()
}

// Nil asserts that the specified object is nil.
//
//	require.Nil(t, err)
func Nil(t TestingT, object interface{}, msgAndArgs ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.Nil(t, object, msgAndArgs...) {
		return
	}
	t.FailNow()
}

// Nilf asserts that the specified object is nil.
//
//	require.Nilf(t, err, "error message %s", "formatted")
func Nilf(t TestingT, object interface{}, msg string, args ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.Nilf(t, object, msg, args...) {
		return
	}
	t.FailNow()
}

// NoDirExists checks whether a directory does not exist in the given path.
// It fails if the path points to an existing _directory_ only.
func NoDirExists(t TestingT, path string, msgAndArgs ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.NoDirExists(t, path, msgAndArgs...) {
		return
	}
	t.FailNow()
}

// NoDirExistsf checks whether a directory does not exist in the given path.
// It fails if the path points to an existing _directory_ only.
func NoDirExistsf(t TestingT, path string, msg string, args ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.NoDirExistsf(t, path, msg, args...) {
		return
	}
	t.FailNow()
}

// NoError asserts that a function returned no error (i.e. `nil`).
//
//	  actualObj, err := SomeFunction()
//	  if require.NoError(t, err) {
//		   require.Equal(t, expectedObj, actualObj)
//	  }
func NoError(t TestingT, err error, msgAndArgs ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.NoError(t, err, msgAndArgs...) {
		return
	}
	t.FailNow()
}

// NoErrorf asserts that a function returned no error (i.e. `nil`).
//
//	  actualObj, err := SomeFunction()
//	  if require.NoErrorf(t, err, "error message %s", "formatted") {
//		   require.Equal(t, expectedObj, actualObj)
//	  }
func NoErrorf(t TestingT, err error, msg string, args ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.NoErrorf(t, err, msg, args...) {
		return
	}
	t.FailNow()
}

// NoFileExists checks whether a file does not exist in a given path. It fails
// if the path points to an existing _file_ only.
func NoFileExists(t TestingT, path string, msgAndArgs ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.NoFileExists(t, path, msgAndArgs...) {
		return
	}
	t.FailNow()
}

// NoFileExistsf checks whether a file does not exist in a given path. It fails
// if the path points to an existing _file_ only.
func NoFileExistsf(t TestingT, path string, msg string, args ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.NoFileExistsf(t, path, msg, args...) {
		return
	}
	t.FailNow()
}

// NotContains asserts that the specified string, list(array, slice...) or map does NOT contain the
// specified substring or element.
//
//	require.NotContains(t, "Hello World", "Earth")
//	require.NotContains(t, ["Hello", "World"], "Earth")
//	require.NotContains(t, {"Hello": "World"}, "Earth")
func NotContains(t TestingT, s interface{}, contains interface{}, msgAndArgs ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.NotContains(t, s, contains, msgAndArgs...) {
		return
	}
	t.FailNow()
}

// NotContainsf asserts that the specified string, list(array, slice...) or map does NOT contain the
// specified substring or element.
//
//	require.NotContainsf(t, "Hello World", "Earth", "error message %s", "formatted")
//	require.NotContainsf(t, ["Hello", "World"], "Earth", "error message %s", "formatted")
//	require.NotContainsf(t, {"Hello": "World"}, "Earth", "error message %s", "formatted")
func NotContainsf(t TestingT, s interface{}, contains interface{}, msg string, args ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.NotContainsf(t, s, contains, msg, args...) {
		return
	}
	t.FailNow()
}

// NotElementsMatch asserts that the specified listA(array, slice...) is NOT equal to specified
// listB(array, slice...) ignoring the order of the elements. If there are duplicate elements,
// the number of appearances of each of them in both lists should not match.
// This is an inverse of ElementsMatch.
//
// require.NotElementsMatch(t, [1, 1, 2, 3], [1, 1, 2, 3]) -> false
//
// require.NotElementsMatch(t, [1, 1, 2, 3], [1, 2, 3]) -> true
//
// require.NotElementsMatch(t, [1, 2, 3], [1, 2, 4]) -> true
func NotElementsMatch(t TestingT, listA interface{}, listB interface{}, msgAndArgs ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.NotElementsMatch(t, listA, listB, msgAndArgs...) {
		return
	}
	t.FailNow()
}

// NotElementsMatchf asserts that the specified listA(array, slice...) is NOT equal to specified
// listB(array, slice...) ignoring the order of the elements. If there are duplicate elements,
// the number of appearances of each of them in both lists should not match.
// This is an inverse of ElementsMatch.
//
// require.NotElementsMatchf(t, [1, 1, 2, 3], [1, 1, 2, 3], "error message %s", "formatted") -> false
//
// require.NotElementsMatchf(t, [1, 1, 2, 3], [1, 2, 3], "error message %s", "formatted") -> true
//
// require.NotElementsMatchf(t, [1, 2, 3], [1, 2, 4], "error message %s", "formatted") -> true
func NotElementsMatchf(t TestingT, listA interface{}, listB interface{}, msg string, args ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.NotElementsMatchf(t, listA, listB, msg, args...) {
		return
	}
	t.FailNow()
}

// NotEmpty asserts that the specified object is NOT empty.  I.e. not nil, "", false, 0 or either
// a slice or a channel with len == 0.
//
//	if require.NotEmpty(t, obj) {
//	  require.Equal(t, "two", obj[1])
//	}
func NotEmpty(t TestingT, object interface{}, msgAndArgs ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.NotEmpty(t, object, msgAndArgs...) {
		return
	}
	t.FailNow()
}

// NotEmptyf asserts that the specified object is NOT empty.  I.e. not nil, "", false, 0 or either
// a slice or a channel with len == 0.
//
//	if require.NotEmptyf(t, obj, "error message %s", "formatted") {
//	  require.Equal(t, "two", obj[1])
//	}
func NotEmptyf(t TestingT, object interface{}, msg string, args ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.NotEmptyf(t, object, msg, args...) {
		return
	}
	t.FailNow()
}

// NotEqual asserts that the specified values are NOT equal.
//
//	require.NotEqual(t, obj1, obj2)
//
// Pointer variable equality is determined based on the equality of the
// referenced values (as opposed to the memory addresses).
func NotEqual(t TestingT, expected interface{}, actual interface{}, msgAndArgs ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.NotEqual(t, expected, actual, msgAndArgs...) {
		return
	}
	t.FailNow()
}

// NotEqualValues asserts that two objects are not equal even when converted to the same type
//
//	require.NotEqualValues(t, obj1, obj2)
func NotEqualValues(t TestingT, expected interface{}, actual interface{}, msgAndArgs ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.NotEqualValues(t, expected, actual, msgAndArgs...) {
		return
	}
	t.FailNow()
}

// NotEqualValuesf asserts that two objects are not equal even when converted to the same type
//
//	require.NotEqualValuesf(t, obj1, obj2, "error message %s", "formatted")
func NotEqualValuesf(t TestingT, expected interface{}, actual interface{}, msg string, args ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.NotEqualValuesf(t, expected, actual, msg, args...) {
		return
	}
	t.FailNow()
}

// NotEqualf asserts that the specified values are NOT equal.
//
//	require.NotEqualf(t, obj1, obj2, "error message %s", "formatted")
//
// Pointer variable equality is determined based on the equality of the
// referenced values (as opposed to the memory addresses).
func NotEqualf(t TestingT, expected interface{}, actual interface{}, msg string, args ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.NotEqualf(t, expected, actual, msg, args...) {
		return
	}
	t.FailNow()
}

// NotErrorAs asserts that none of the errors in err's chain matches target,
// but if so, sets target to that error value.
func NotErrorAs(t TestingT, err error, target interface{}, msgAndArgs ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.NotErrorAs(t, err, target, msgAndArgs...) {
		return
	}
	t.FailNow()
}

// NotErrorAsf asserts that none of the errors in err's chain matches target,
// but if so, sets target to that error value.
func NotErrorAsf(t TestingT, err error, target interface{}, msg string, args ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.NotErrorAsf(t, err, target, msg, args...) {
		return
	}
	t.FailNow()
}

// NotErrorIs asserts that none of the errors in err's chain matches target.
// This is a wrapper for errors.Is.
func NotErrorIs(t TestingT, err error, target error, msgAndArgs ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.NotErrorIs(t, err, target, msgAndArgs...) {
		return
	}
	t.FailNow()
}

// NotErrorIsf asserts that none of the errors in err's chain matches target.
// This is a wrapper for errors.Is.
func NotErrorIsf(t TestingT, err error, target error, msg string, args ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.NotErrorIsf(t, err, target, msg, args...) {
		return
	}
	t.FailNow()
}

// NotImplements asserts that an object does not implement the specified interface.
//
//	require.NotImplements(t, (*MyInterface)(nil), new(MyObject))
func NotImplements(t TestingT, interfaceObject interface{}, object interface{}, msgAndArgs ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.NotImplements(t, interfaceObject, object, msgAndArgs...) {
		return
	}
	t.FailNow()
}

// NotImplementsf asserts that an object does not implement the specified interface.
//
//	require.NotImplementsf(t, (*MyInterface)(nil), new(MyObject), "error message %s", "formatted")
func NotImplementsf(t TestingT, interfaceObject interface{}, object interface{}, msg string, args ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.NotImplementsf(t, interfaceObject, object, msg, args...) {
		return
	}
	t.FailNow()
}

// NotNil asserts that the specified object is not nil.
//
//	require.NotNil(t, err)
func NotNil(t TestingT, object interface{}, msgAndArgs ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.NotNil(t, object, msgAndArgs...) {
		return
	}
	t.FailNow()
}

// NotNilf asserts that the specified object is not nil.
//
//	require.NotNilf(t, err, "error message %s", "formatted")
func NotNilf(t TestingT, object interface{}, msg string, args ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.NotNilf(t, object, msg, args...) {
		return
	}
	t.FailNow()
}

// NotPanics asserts that the code inside the specified PanicTestFunc does NOT panic.
//
//	require.NotPanics(t, func(){ RemainCalm() })
func NotPanics(t TestingT, f assert.PanicTestFunc, msgAndArgs ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.NotPanics(t, f, msgAndArgs...) {
		return
	}
	t.FailNow()
}

// NotPanicsf asserts that the code inside the specified PanicTestFunc does NOT panic.
//
//	require.NotPanicsf(t, func(){ RemainCalm() }, "error message %s", "formatted")
func NotPanicsf(t TestingT, f assert.PanicTestFunc, msg string, args ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.NotPanicsf(t, f, msg, args...) {
		return
	}
	t.FailNow()
}

// NotRegexp asserts that a specified regexp does not match a string.
//
//	require.NotRegexp(t, regexp.MustCompile("starts"), "it's starting")
//	require.NotRegexp(t, "^start", "it's not starting")
func NotRegexp(t TestingT, rx interface{}, str interface{}, msgAndArgs ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.NotRegexp(t, rx, str, msgAndArgs...) {
		return
	}
	t.FailNow()
}

// NotRegexpf asserts that a specified regexp does not match a string.
//
//	require.NotRegexpf(t, regexp.MustCompile("starts"), "it's starting", "error message %s", "formatted")
//	require.NotRegexpf(t, "^start", "it's not starting", "error message %s", "formatted")
func NotRegexpf(t TestingT, rx interface{}, str interface{}, msg string, args ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.NotRegexpf(t, rx, str, msg, args...) {
		return
	}
	t.FailNow()
}

// NotSame asserts that two pointers do not reference the same object.
//
//	require.NotSame(t, ptr1, ptr2)
//
// Both arguments must be pointer variables. Pointer variable sameness is
// determined based on the equality of both type and value.
func NotSame(t TestingT, expected interface{}, actual interface{}, msgAndArgs ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.NotSame(t, expected, actual, msgAndArgs...) {
		return
	}
	t.FailNow()
}

// NotSamef asserts that two pointers do not reference the same object.
//
//	require.NotSamef(t, ptr1, ptr2, "error message %s", "formatted")
//
// Both arguments must be pointer variables. Pointer variable sameness is
// determined based on the equality of both type and value.
func NotSamef(t TestingT, expected interface{}, actual interface{}, msg string, args ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.NotSamef(t, expected, actual, msg, args...) {
		return
	}
	t.FailNow()
}

// NotSubset asserts that the specified list(array, slice...) or map does NOT
// contain all elements given in the specified subset list(array, slice...) or
// map.
//
//	require.NotSubset(t, [1, 3, 4], [1, 2])
//	require.NotSubset(t, {"x": 1, "y": 2}, {"z": 3})
func NotSubset(t TestingT, list interface{}, subset interface{}, msgAndArgs ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.NotSubset(t, list, subset, msgAndArgs...) {
		return
	}
	t.FailNow()
}

// NotSubsetf asserts that the specified list(array, slice...) or map does NOT
// contain all elements given in the specified subset list(array, slice...) or
// map.
//
//	require.NotSubsetf(t, [1, 3, 4], [1, 2], "error message %s", "formatted")
//	require.NotSubsetf(t, {"x": 1, "y": 2}, {"z": 3}, "error message %s", "formatted")
func NotSubsetf(t TestingT, list interface{}, subset interface{}, msg string, args ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.NotSubsetf(t, list, subset, msg, args...) {
		return
	}
	t.FailNow()
}

// NotZero asserts that i is not the zero value for its type.
func NotZero(t TestingT, i interface{}, msgAndArgs ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.NotZero(t, i, msgAndArgs...) {
		return
	}
	t.FailNow()
}

// NotZerof asserts that i is not the zero value for its type.
func NotZerof(t TestingT, i interface{}, msg string, args ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.NotZerof(t, i, msg, args...) {
		return
	}
	t.FailNow()
}

// Panics asserts that the code inside the specified PanicTestFunc panics.
//
//	require.Panics(t, func(){ GoCrazy() })
func Panics(t TestingT, f assert.PanicTestFunc, msgAndArgs ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.Panics(t, f, msgAndArgs...) {
		return
	}
	t.FailNow()
}

// PanicsWithError asserts that the code inside the specified PanicTestFunc
// panics, and that the recovered panic value is an error that satisfies the
// EqualError comparison.
//
//	require.PanicsWithError(t, "crazy error", func(){ GoCrazy() })
func PanicsWithError(t TestingT, errString string, f assert.PanicTestFunc, msgAndArgs ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.PanicsWithError(t, errString, f, msgAndArgs...) {
		return
	}
	t.FailNow()
}

// PanicsWithErrorf asserts that the code inside the specified PanicTestFunc
// panics, and that the recovered panic value is an error that satisfies the
// EqualError comparison.
//
//	require.PanicsWithErrorf(t, "crazy error", func(){ GoCrazy() }, "error message %s", "formatted")
func PanicsWithErrorf(t TestingT, errString string, f assert.PanicTestFunc, msg string, args ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.PanicsWithErrorf(t, errString, f, msg, args...) {
		return
	}
	t.FailNow()
}

// PanicsWithValue asserts that the code inside the specified PanicTestFunc panics, and that
// the recovered panic value equals the expected panic value.
//
//	require.PanicsWithValue(t, "crazy error", func(){ GoCrazy() })
func PanicsWithValue(t TestingT, expected interface{}, f assert.PanicTestFunc, msgAndArgs ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.PanicsWithValue(t, expected, f, msgAndArgs...) {
		return
	}
	t.FailNow()
}

// PanicsWithValuef asserts that the code inside the specified PanicTestFunc panics, and that
// the recovered panic value equals the expected panic value.
//
//	require.PanicsWithValuef(t, "crazy error", func(){ GoCrazy() }, "error message %s", "formatted")
func PanicsWithValuef(t TestingT, expected interface{}, f assert.PanicTestFunc, msg string, args ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.PanicsWithValuef(t, expected, f, msg, args...) {
		return
	}
	t.FailNow()
}

// Panicsf asserts that the code inside the specified PanicTestFunc panics.
//
//	require.Panicsf(t, func(){ GoCrazy() }, "error message %s", "formatted")
func Panicsf(t TestingT, f assert.PanicTestFunc, msg string, args ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.Panicsf(t, f, msg, args...) {
		return
	}
	t.FailNow()
}

// Positive asserts that the specified element is positive
//
//	require.Positive(t, 1)
//	require.Positive(t, 1.23)
func Positive(t TestingT, e interface{}, msgAndArgs ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.Positive(t, e, msgAndArgs...) {
		return
	}
	t.FailNow()
}

// Positivef asserts that the specified element is positive
//
//	require.Positivef(t, 1, "error message %s", "formatted")
//	require.Positivef(t, 1.23, "error message %s", "formatted")
func Positivef(t TestingT, e interface{}, msg string, args ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.Positivef(t, e, msg, args...) {
		return
	}
	t.FailNow()
}

// Regexp asserts that a specified regexp matches a string.
//
//	require.Regexp(t, regexp.MustCompile("start"), "it's starting")
//	require.Regexp(t, "start...$", "it's not starting")
func Regexp(t TestingT, rx interface{}, str interface{}, msgAndArgs ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.Regexp(t, rx, str, msgAndArgs...) {
		return
	}
	t.FailNow()
}

// Regexpf asserts that a specified regexp matches a string.
//
//	require.Regexpf(t, regexp.MustCompile("start"), "it's starting", "error message %s", "formatted")
//	require.Regexpf(t, "start...$", "it's not starting", "error message %s", "formatted")
func Regexpf(t TestingT, rx interface{}, str interface{}, msg string, args ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.Regexpf(t, rx, str, msg, args...) {
		return
	}
	t.FailNow()
}

// Same asserts that two pointers reference the same object.
//
//	require.Same(t, ptr1, ptr2)
//
// Both arguments must be pointer variables. Pointer variable sameness is
// determined based on the equality of both type and value.
func Same(t TestingT, expected interface{}, actual interface{}, msgAndArgs ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.Same(t, expected, actual, msgAndArgs...) {
		return
	}
	t.FailNow()
}

// Samef asserts that two pointers reference the same object.
//
//	require.Samef(t, ptr1, ptr2, "error message %s", "formatted")
//
// Both arguments must be pointer variables. Pointer variable sameness is
// determined based on the equality of both type and value.
func Samef(t TestingT, expected interface{}, actual interface{}, msg string, args ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.Samef(t, expected, actual, msg, args...) {
		return
	}
	t.FailNow()
}

// Subset asserts that the specified list(array, slice...) or map contains all
// elements given in the specified subset list(array, slice...) or map.
//
//	require.Subset(t, [1, 2, 3], [1, 2])
//	require.Subset(t, {"x": 1, "y": 2}, {"x": 1})
func Subset(t TestingT, list interface{}, subset interface{}, msgAndArgs ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.Subset(t, list, subset, msgAndArgs...) {
		return
	}
	t.FailNow()
}

// Subsetf asserts that the specified list(array, slice...) or map contains all
// elements given in the specified subset list(array, slice...) or map.
//
//	require.Subsetf(t, [1, 2, 3], [1, 2], "error message %s", "formatted")
//	require.Subsetf(t, {"x": 1, "y": 2}, {"x": 1}, "error message %s", "formatted")
func Subsetf(t TestingT, list interface{}, subset interface{}, msg string, args ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.Subsetf(t, list, subset, msg, args...) {
		return
	}
	t.FailNow()
}

// True asserts that the specified value is true.
//
//	require.True(t, myBool)
func True(t TestingT, value bool, msgAndArgs ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.True(t, value, msgAndArgs...) {
		return
	}
	t.FailNow()
}

// Truef asserts that the specified value is true.
//
//	require.Truef(t, myBool, "error message %s", "formatted")
func Truef(t TestingT, value bool, msg string, args ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.Truef(t, value, msg, args...) {
		return
	}
	t.FailNow()
}

// WithinDuration asserts that the two times are within duration delta of each other.
//
//	require.WithinDuration(t, time.Now(), time.Now(), 10*time.Second)
func WithinDuration(t TestingT, expected time.Time, actual time.Time, delta time.Duration, msgAndArgs ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.WithinDuration(t, expected, actual, delta, msgAndArgs...) {
		return
	}
	t.FailNow()
}

// WithinDurationf asserts that the two times are within duration delta of each other.
//
//	require.WithinDurationf(t, time.Now(), time.Now(), 10*time.Second, "error message %s", "formatted")
func WithinDurationf(t TestingT, expected time.Time, actual time.Time, delta time.Duration, msg string, args ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.WithinDurationf(t, expected, actual, delta, msg, args...) {
		return
	}
	t.FailNow()
}

// WithinRange asserts that a time is within a time range (inclusive).
//
//	require.WithinRange(t, time.Now(), time.Now().Add(-time.Second), time.Now().Add(time.Second))
func WithinRange(t TestingT, actual time.Time, start time.Time, end time.Time, msgAndArgs ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.WithinRange(t, actual, start, end, msgAndArgs...) {
		return
	}
	t.FailNow()
}

// WithinRangef asserts that a time is within a time range (inclusive).
//
//	require.WithinRangef(t, time.Now(), time.Now().Add(-time.Second), time.Now().Add(time.Second), "error message %s", "formatted")
func WithinRangef(t TestingT, actual time.Time, start time.Time, end time.Time, msg string, args ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.WithinRangef(t, actual, start, end, msg, args...) {
		return
	}
	t.FailNow()
}

// YAMLEq asserts that two YAML strings are equivalent.
func YAMLEq(t TestingT, expected string, actual string, msgAndArgs ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.YAMLEq(t, expected, actual, msgAndArgs...) {
		return
	}
	t.FailNow()
}

// YAMLEqf asserts that two YAML strings are equivalent.
func YAMLEqf(t TestingT, expected string, actual string, msg string, args ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.YAMLEqf(t, expected, actual, msg, args...) {
		return
	}
	t.FailNow()
}

// Zero asserts that i is the zero value for its type.
func Zero(t TestingT, i interface{}, msgAndArgs ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.Zero(t, i, msgAndArgs...) {
		return
	}
	t.FailNow()
}

// Zerof asserts that i is the zero value for its type.
func Zerof(t TestingT, i interface{}, msg string, args ...interface{}) {
	if h, ok := t.(tHelper); ok {
		h.Helper()
	}
	if assert.Zerof(t, i, msg, args...) {
		return
	}
	t.FailNow()
}
