// Code generated with github.com/stretchr/testify/_codegen; DO NOT EDIT.

package require

import (
	assert "github.com/stretchr/testify/assert"
	http "net/http"
	url "net/url"
	time "time"
)

// Condition uses a Comparison to assert a complex condition.
func (a *Assertions) Condition(comp assert.Comparison, msgAndArgs ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	Condition(a.t, comp, msgAndArgs...)
}

// Conditionf uses a Comparison to assert a complex condition.
func (a *Assertions) Conditionf(comp assert.Comparison, msg string, args ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	Conditionf(a.t, comp, msg, args...)
}

// Contains asserts that the specified string, list(array, slice...) or map contains the
// specified substring or element.
//
//	a.Contains("Hello World", "World")
//	a.Contains(["Hello", "World"], "World")
//	a.Contains({"Hello": "World"}, "Hello")
func (a *Assertions) Contains(s interface{}, contains interface{}, msgAndArgs ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	Contains(a.t, s, contains, msgAndArgs...)
}

// Containsf asserts that the specified string, list(array, slice...) or map contains the
// specified substring or element.
//
//	a.Containsf("Hello World", "World", "error message %s", "formatted")
//	a.Containsf(["Hello", "World"], "World", "error message %s", "formatted")
//	a.Containsf({"Hello": "World"}, "Hello", "error message %s", "formatted")
func (a *Assertions) Containsf(s interface{}, contains interface{}, msg string, args ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	Containsf(a.t, s, contains, msg, args...)
}

// DirExists checks whether a directory exists in the given path. It also fails
// if the path is a file rather a directory or there is an error checking whether it exists.
func (a *Assertions) DirExists(path string, msgAndArgs ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	DirExists(a.t, path, msgAndArgs...)
}

// DirExistsf checks whether a directory exists in the given path. It also fails
// if the path is a file rather a directory or there is an error checking whether it exists.
func (a *Assertions) DirExistsf(path string, msg string, args ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	DirExistsf(a.t, path, msg, args...)
}

// ElementsMatch asserts that the specified listA(array, slice...) is equal to specified
// listB(array, slice...) ignoring the order of the elements. If there are duplicate elements,
// the number of appearances of each of them in both lists should match.
//
// a.ElementsMatch([1, 3, 2, 3], [1, 3, 3, 2])
func (a *Assertions) ElementsMatch(listA interface{}, listB interface{}, msgAndArgs ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	ElementsMatch(a.t, listA, listB, msgAndArgs...)
}

// ElementsMatchf asserts that the specified listA(array, slice...) is equal to specified
// listB(array, slice...) ignoring the order of the elements. If there are duplicate elements,
// the number of appearances of each of them in both lists should match.
//
// a.ElementsMatchf([1, 3, 2, 3], [1, 3, 3, 2], "error message %s", "formatted")
func (a *Assertions) ElementsMatchf(listA interface{}, listB interface{}, msg string, args ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	ElementsMatchf(a.t, listA, listB, msg, args...)
}

// Empty asserts that the specified object is empty.  I.e. nil, "", false, 0 or either
// a slice or a channel with len == 0.
//
//	a.Empty(obj)
func (a *Assertions) Empty(object interface{}, msgAndArgs ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	Empty(a.t, object, msgAndArgs...)
}

// Emptyf asserts that the specified object is empty.  I.e. nil, "", false, 0 or either
// a slice or a channel with len == 0.
//
//	a.Emptyf(obj, "error message %s", "formatted")
func (a *Assertions) Emptyf(object interface{}, msg string, args ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	Emptyf(a.t, object, msg, args...)
}

// Equal asserts that two objects are equal.
//
//	a.Equal(123, 123)
//
// Pointer variable equality is determined based on the equality of the
// referenced values (as opposed to the memory addresses). Function equality
// cannot be determined and will always fail.
func (a *Assertions) Equal(expected interface{}, actual interface{}, msgAndArgs ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	Equal(a.t, expected, actual, msgAndArgs...)
}

// EqualError asserts that a function returned an error (i.e. not `nil`)
// and that it is equal to the provided error.
//
//	actualObj, err := SomeFunction()
//	a.EqualError(err,  expectedErrorString)
func (a *Assertions) EqualError(theError error, errString string, msgAndArgs ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	EqualError(a.t, theError, errString, msgAndArgs...)
}

// EqualErrorf asserts that a function returned an error (i.e. not `nil`)
// and that it is equal to the provided error.
//
//	actualObj, err := SomeFunction()
//	a.EqualErrorf(err,  expectedErrorString, "error message %s", "formatted")
func (a *Assertions) EqualErrorf(theError error, errString string, msg string, args ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	EqualErrorf(a.t, theError, errString, msg, args...)
}

// EqualExportedValues asserts that the types of two objects are equal and their public
// fields are also equal. This is useful for comparing structs that have private fields
// that could potentially differ.
//
//	 type S struct {
//		Exported     	int
//		notExported   	int
//	 }
//	 a.EqualExportedValues(S{1, 2}, S{1, 3}) => true
//	 a.EqualExportedValues(S{1, 2}, S{2, 3}) => false
func (a *Assertions) EqualExportedValues(expected interface{}, actual interface{}, msgAndArgs ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	EqualExportedValues(a.t, expected, actual, msgAndArgs...)
}

// EqualExportedValuesf asserts that the types of two objects are equal and their public
// fields are also equal. This is useful for comparing structs that have private fields
// that could potentially differ.
//
//	 type S struct {
//		Exported     	int
//		notExported   	int
//	 }
//	 a.EqualExportedValuesf(S{1, 2}, S{1, 3}, "error message %s", "formatted") => true
//	 a.EqualExportedValuesf(S{1, 2}, S{2, 3}, "error message %s", "formatted") => false
func (a *Assertions) EqualExportedValuesf(expected interface{}, actual interface{}, msg string, args ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	EqualExportedValuesf(a.t, expected, actual, msg, args...)
}

// EqualValues asserts that two objects are equal or convertible to the larger
// type and equal.
//
//	a.EqualValues(uint32(123), int32(123))
func (a *Assertions) EqualValues(expected interface{}, actual interface{}, msgAndArgs ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	EqualValues(a.t, expected, actual, msgAndArgs...)
}

// EqualValuesf asserts that two objects are equal or convertible to the larger
// type and equal.
//
//	a.EqualValuesf(uint32(123), int32(123), "error message %s", "formatted")
func (a *Assertions) EqualValuesf(expected interface{}, actual interface{}, msg string, args ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	EqualValuesf(a.t, expected, actual, msg, args...)
}

// Equalf asserts that two objects are equal.
//
//	a.Equalf(123, 123, "error message %s", "formatted")
//
// Pointer variable equality is determined based on the equality of the
// referenced values (as opposed to the memory addresses). Function equality
// cannot be determined and will always fail.
func (a *Assertions) Equalf(expected interface{}, actual interface{}, msg string, args ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	Equalf(a.t, expected, actual, msg, args...)
}

// Error asserts that a function returned an error (i.e. not `nil`).
//
//	  actualObj, err := SomeFunction()
//	  if a.Error(err) {
//		   assert.Equal(t, expectedError, err)
//	  }
func (a *Assertions) Error(err error, msgAndArgs ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	Error(a.t, err, msgAndArgs...)
}

// ErrorAs asserts that at least one of the errors in err's chain matches target, and if so, sets target to that error value.
// This is a wrapper for errors.As.
func (a *Assertions) ErrorAs(err error, target interface{}, msgAndArgs ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	ErrorAs(a.t, err, target, msgAndArgs...)
}

// ErrorAsf asserts that at least one of the errors in err's chain matches target, and if so, sets target to that error value.
// This is a wrapper for errors.As.
func (a *Assertions) ErrorAsf(err error, target interface{}, msg string, args ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	ErrorAsf(a.t, err, target, msg, args...)
}

// ErrorContains asserts that a function returned an error (i.e. not `nil`)
// and that the error contains the specified substring.
//
//	actualObj, err := SomeFunction()
//	a.ErrorContains(err,  expectedErrorSubString)
func (a *Assertions) ErrorContains(theError error, contains string, msgAndArgs ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	ErrorContains(a.t, theError, contains, msgAndArgs...)
}

// ErrorContainsf asserts that a function returned an error (i.e. not `nil`)
// and that the error contains the specified substring.
//
//	actualObj, err := SomeFunction()
//	a.ErrorContainsf(err,  expectedErrorSubString, "error message %s", "formatted")
func (a *Assertions) ErrorContainsf(theError error, contains string, msg string, args ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	ErrorContainsf(a.t, theError, contains, msg, args...)
}

// ErrorIs asserts that at least one of the errors in err's chain matches target.
// This is a wrapper for errors.Is.
func (a *Assertions) ErrorIs(err error, target error, msgAndArgs ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	ErrorIs(a.t, err, target, msgAndArgs...)
}

// ErrorIsf asserts that at least one of the errors in err's chain matches target.
// This is a wrapper for errors.Is.
func (a *Assertions) ErrorIsf(err error, target error, msg string, args ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	ErrorIsf(a.t, err, target, msg, args...)
}

// Errorf asserts that a function returned an error (i.e. not `nil`).
//
//	  actualObj, err := SomeFunction()
//	  if a.Errorf(err, "error message %s", "formatted") {
//		   assert.Equal(t, expectedErrorf, err)
//	  }
func (a *Assertions) Errorf(err error, msg string, args ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	Errorf(a.t, err, msg, args...)
}

// Eventually asserts that given condition will be met in waitFor time,
// periodically checking target function each tick.
//
//	a.Eventually(func() bool { return true; }, time.Second, 10*time.Millisecond)
func (a *Assertions) Eventually(condition func() bool, waitFor time.Duration, tick time.Duration, msgAndArgs ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	Eventually(a.t, condition, waitFor, tick, msgAndArgs...)
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
//	a.EventuallyWithT(func(c *assert.CollectT) {
//		// add assertions as needed; any assertion failure will fail the current tick
//		assert.True(c, externalValue, "expected 'externalValue' to be true")
//	}, 10*time.Second, 1*time.Second, "external state has not changed to 'true'; still false")
func (a *Assertions) EventuallyWithT(condition func(collect *assert.CollectT), waitFor time.Duration, tick time.Duration, msgAndArgs ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	EventuallyWithT(a.t, condition, waitFor, tick, msgAndArgs...)
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
//	a.EventuallyWithTf(func(c *assert.CollectT, "error message %s", "formatted") {
//		// add assertions as needed; any assertion failure will fail the current tick
//		assert.True(c, externalValue, "expected 'externalValue' to be true")
//	}, 10*time.Second, 1*time.Second, "external state has not changed to 'true'; still false")
func (a *Assertions) EventuallyWithTf(condition func(collect *assert.CollectT), waitFor time.Duration, tick time.Duration, msg string, args ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	EventuallyWithTf(a.t, condition, waitFor, tick, msg, args...)
}

// Eventuallyf asserts that given condition will be met in waitFor time,
// periodically checking target function each tick.
//
//	a.Eventuallyf(func() bool { return true; }, time.Second, 10*time.Millisecond, "error message %s", "formatted")
func (a *Assertions) Eventuallyf(condition func() bool, waitFor time.Duration, tick time.Duration, msg string, args ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	Eventuallyf(a.t, condition, waitFor, tick, msg, args...)
}

// Exactly asserts that two objects are equal in value and type.
//
//	a.Exactly(int32(123), int64(123))
func (a *Assertions) Exactly(expected interface{}, actual interface{}, msgAndArgs ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	Exactly(a.t, expected, actual, msgAndArgs...)
}

// Exactlyf asserts that two objects are equal in value and type.
//
//	a.Exactlyf(int32(123), int64(123), "error message %s", "formatted")
func (a *Assertions) Exactlyf(expected interface{}, actual interface{}, msg string, args ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	Exactlyf(a.t, expected, actual, msg, args...)
}

// Fail reports a failure through
func (a *Assertions) Fail(failureMessage string, msgAndArgs ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	Fail(a.t, failureMessage, msgAndArgs...)
}

// FailNow fails test
func (a *Assertions) FailNow(failureMessage string, msgAndArgs ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	FailNow(a.t, failureMessage, msgAndArgs...)
}

// FailNowf fails test
func (a *Assertions) FailNowf(failureMessage string, msg string, args ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	FailNowf(a.t, failureMessage, msg, args...)
}

// Failf reports a failure through
func (a *Assertions) Failf(failureMessage string, msg string, args ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	Failf(a.t, failureMessage, msg, args...)
}

// False asserts that the specified value is false.
//
//	a.False(myBool)
func (a *Assertions) False(value bool, msgAndArgs ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	False(a.t, value, msgAndArgs...)
}

// Falsef asserts that the specified value is false.
//
//	a.Falsef(myBool, "error message %s", "formatted")
func (a *Assertions) Falsef(value bool, msg string, args ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	Falsef(a.t, value, msg, args...)
}

// FileExists checks whether a file exists in the given path. It also fails if
// the path points to a directory or there is an error when trying to check the file.
func (a *Assertions) FileExists(path string, msgAndArgs ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	FileExists(a.t, path, msgAndArgs...)
}

// FileExistsf checks whether a file exists in the given path. It also fails if
// the path points to a directory or there is an error when trying to check the file.
func (a *Assertions) FileExistsf(path string, msg string, args ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	FileExistsf(a.t, path, msg, args...)
}

// Greater asserts that the first element is greater than the second
//
//	a.Greater(2, 1)
//	a.Greater(float64(2), float64(1))
//	a.Greater("b", "a")
func (a *Assertions) Greater(e1 interface{}, e2 interface{}, msgAndArgs ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	Greater(a.t, e1, e2, msgAndArgs...)
}

// GreaterOrEqual asserts that the first element is greater than or equal to the second
//
//	a.GreaterOrEqual(2, 1)
//	a.GreaterOrEqual(2, 2)
//	a.GreaterOrEqual("b", "a")
//	a.GreaterOrEqual("b", "b")
func (a *Assertions) GreaterOrEqual(e1 interface{}, e2 interface{}, msgAndArgs ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	GreaterOrEqual(a.t, e1, e2, msgAndArgs...)
}

// GreaterOrEqualf asserts that the first element is greater than or equal to the second
//
//	a.GreaterOrEqualf(2, 1, "error message %s", "formatted")
//	a.GreaterOrEqualf(2, 2, "error message %s", "formatted")
//	a.GreaterOrEqualf("b", "a", "error message %s", "formatted")
//	a.GreaterOrEqualf("b", "b", "error message %s", "formatted")
func (a *Assertions) GreaterOrEqualf(e1 interface{}, e2 interface{}, msg string, args ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	GreaterOrEqualf(a.t, e1, e2, msg, args...)
}

// Greaterf asserts that the first element is greater than the second
//
//	a.Greaterf(2, 1, "error message %s", "formatted")
//	a.Greaterf(float64(2), float64(1), "error message %s", "formatted")
//	a.Greaterf("b", "a", "error message %s", "formatted")
func (a *Assertions) Greaterf(e1 interface{}, e2 interface{}, msg string, args ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	Greaterf(a.t, e1, e2, msg, args...)
}

// HTTPBodyContains asserts that a specified handler returns a
// body that contains a string.
//
//	a.HTTPBodyContains(myHandler, "GET", "www.google.com", nil, "I'm Feeling Lucky")
//
// Returns whether the assertion was successful (true) or not (false).
func (a *Assertions) HTTPBodyContains(handler http.HandlerFunc, method string, url string, values url.Values, str interface{}, msgAndArgs ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	HTTPBodyContains(a.t, handler, method, url, values, str, msgAndArgs...)
}

// HTTPBodyContainsf asserts that a specified handler returns a
// body that contains a string.
//
//	a.HTTPBodyContainsf(myHandler, "GET", "www.google.com", nil, "I'm Feeling Lucky", "error message %s", "formatted")
//
// Returns whether the assertion was successful (true) or not (false).
func (a *Assertions) HTTPBodyContainsf(handler http.HandlerFunc, method string, url string, values url.Values, str interface{}, msg string, args ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	HTTPBodyContainsf(a.t, handler, method, url, values, str, msg, args...)
}

// HTTPBodyNotContains asserts that a specified handler returns a
// body that does not contain a string.
//
//	a.HTTPBodyNotContains(myHandler, "GET", "www.google.com", nil, "I'm Feeling Lucky")
//
// Returns whether the assertion was successful (true) or not (false).
func (a *Assertions) HTTPBodyNotContains(handler http.HandlerFunc, method string, url string, values url.Values, str interface{}, msgAndArgs ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	HTTPBodyNotContains(a.t, handler, method, url, values, str, msgAndArgs...)
}

// HTTPBodyNotContainsf asserts that a specified handler returns a
// body that does not contain a string.
//
//	a.HTTPBodyNotContainsf(myHandler, "GET", "www.google.com", nil, "I'm Feeling Lucky", "error message %s", "formatted")
//
// Returns whether the assertion was successful (true) or not (false).
func (a *Assertions) HTTPBodyNotContainsf(handler http.HandlerFunc, method string, url string, values url.Values, str interface{}, msg string, args ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	HTTPBodyNotContainsf(a.t, handler, method, url, values, str, msg, args...)
}

// HTTPError asserts that a specified handler returns an error status code.
//
//	a.HTTPError(myHandler, "POST", "/a/b/c", url.Values{"a": []string{"b", "c"}}
//
// Returns whether the assertion was successful (true) or not (false).
func (a *Assertions) HTTPError(handler http.HandlerFunc, method string, url string, values url.Values, msgAndArgs ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	HTTPError(a.t, handler, method, url, values, msgAndArgs...)
}

// HTTPErrorf asserts that a specified handler returns an error status code.
//
//	a.HTTPErrorf(myHandler, "POST", "/a/b/c", url.Values{"a": []string{"b", "c"}}
//
// Returns whether the assertion was successful (true) or not (false).
func (a *Assertions) HTTPErrorf(handler http.HandlerFunc, method string, url string, values url.Values, msg string, args ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	HTTPErrorf(a.t, handler, method, url, values, msg, args...)
}

// HTTPRedirect asserts that a specified handler returns a redirect status code.
//
//	a.HTTPRedirect(myHandler, "GET", "/a/b/c", url.Values{"a": []string{"b", "c"}}
//
// Returns whether the assertion was successful (true) or not (false).
func (a *Assertions) HTTPRedirect(handler http.HandlerFunc, method string, url string, values url.Values, msgAndArgs ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	HTTPRedirect(a.t, handler, method, url, values, msgAndArgs...)
}

// HTTPRedirectf asserts that a specified handler returns a redirect status code.
//
//	a.HTTPRedirectf(myHandler, "GET", "/a/b/c", url.Values{"a": []string{"b", "c"}}
//
// Returns whether the assertion was successful (true) or not (false).
func (a *Assertions) HTTPRedirectf(handler http.HandlerFunc, method string, url string, values url.Values, msg string, args ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	HTTPRedirectf(a.t, handler, method, url, values, msg, args...)
}

// HTTPStatusCode asserts that a specified handler returns a specified status code.
//
//	a.HTTPStatusCode(myHandler, "GET", "/notImplemented", nil, 501)
//
// Returns whether the assertion was successful (true) or not (false).
func (a *Assertions) HTTPStatusCode(handler http.HandlerFunc, method string, url string, values url.Values, statuscode int, msgAndArgs ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	HTTPStatusCode(a.t, handler, method, url, values, statuscode, msgAndArgs...)
}

// HTTPStatusCodef asserts that a specified handler returns a specified status code.
//
//	a.HTTPStatusCodef(myHandler, "GET", "/notImplemented", nil, 501, "error message %s", "formatted")
//
// Returns whether the assertion was successful (true) or not (false).
func (a *Assertions) HTTPStatusCodef(handler http.HandlerFunc, method string, url string, values url.Values, statuscode int, msg string, args ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	HTTPStatusCodef(a.t, handler, method, url, values, statuscode, msg, args...)
}

// HTTPSuccess asserts that a specified handler returns a success status code.
//
//	a.HTTPSuccess(myHandler, "POST", "http://www.google.com", nil)
//
// Returns whether the assertion was successful (true) or not (false).
func (a *Assertions) HTTPSuccess(handler http.HandlerFunc, method string, url string, values url.Values, msgAndArgs ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	HTTPSuccess(a.t, handler, method, url, values, msgAndArgs...)
}

// HTTPSuccessf asserts that a specified handler returns a success status code.
//
//	a.HTTPSuccessf(myHandler, "POST", "http://www.google.com", nil, "error message %s", "formatted")
//
// Returns whether the assertion was successful (true) or not (false).
func (a *Assertions) HTTPSuccessf(handler http.HandlerFunc, method string, url string, values url.Values, msg string, args ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	HTTPSuccessf(a.t, handler, method, url, values, msg, args...)
}

// Implements asserts that an object is implemented by the specified interface.
//
//	a.Implements((*MyInterface)(nil), new(MyObject))
func (a *Assertions) Implements(interfaceObject interface{}, object interface{}, msgAndArgs ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	Implements(a.t, interfaceObject, object, msgAndArgs...)
}

// Implementsf asserts that an object is implemented by the specified interface.
//
//	a.Implementsf((*MyInterface)(nil), new(MyObject), "error message %s", "formatted")
func (a *Assertions) Implementsf(interfaceObject interface{}, object interface{}, msg string, args ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	Implementsf(a.t, interfaceObject, object, msg, args...)
}

// InDelta asserts that the two numerals are within delta of each other.
//
//	a.InDelta(math.Pi, 22/7.0, 0.01)
func (a *Assertions) InDelta(expected interface{}, actual interface{}, delta float64, msgAndArgs ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	InDelta(a.t, expected, actual, delta, msgAndArgs...)
}

// InDeltaMapValues is the same as InDelta, but it compares all values between two maps. Both maps must have exactly the same keys.
func (a *Assertions) InDeltaMapValues(expected interface{}, actual interface{}, delta float64, msgAndArgs ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	InDeltaMapValues(a.t, expected, actual, delta, msgAndArgs...)
}

// InDeltaMapValuesf is the same as InDelta, but it compares all values between two maps. Both maps must have exactly the same keys.
func (a *Assertions) InDeltaMapValuesf(expected interface{}, actual interface{}, delta float64, msg string, args ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	InDeltaMapValuesf(a.t, expected, actual, delta, msg, args...)
}

// InDeltaSlice is the same as InDelta, except it compares two slices.
func (a *Assertions) InDeltaSlice(expected interface{}, actual interface{}, delta float64, msgAndArgs ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	InDeltaSlice(a.t, expected, actual, delta, msgAndArgs...)
}

// InDeltaSlicef is the same as InDelta, except it compares two slices.
func (a *Assertions) InDeltaSlicef(expected interface{}, actual interface{}, delta float64, msg string, args ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	InDeltaSlicef(a.t, expected, actual, delta, msg, args...)
}

// InDeltaf asserts that the two numerals are within delta of each other.
//
//	a.InDeltaf(math.Pi, 22/7.0, 0.01, "error message %s", "formatted")
func (a *Assertions) InDeltaf(expected interface{}, actual interface{}, delta float64, msg string, args ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	InDeltaf(a.t, expected, actual, delta, msg, args...)
}

// InEpsilon asserts that expected and actual have a relative error less than epsilon
func (a *Assertions) InEpsilon(expected interface{}, actual interface{}, epsilon float64, msgAndArgs ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	InEpsilon(a.t, expected, actual, epsilon, msgAndArgs...)
}

// InEpsilonSlice is the same as InEpsilon, except it compares each value from two slices.
func (a *Assertions) InEpsilonSlice(expected interface{}, actual interface{}, epsilon float64, msgAndArgs ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	InEpsilonSlice(a.t, expected, actual, epsilon, msgAndArgs...)
}

// InEpsilonSlicef is the same as InEpsilon, except it compares each value from two slices.
func (a *Assertions) InEpsilonSlicef(expected interface{}, actual interface{}, epsilon float64, msg string, args ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	InEpsilonSlicef(a.t, expected, actual, epsilon, msg, args...)
}

// InEpsilonf asserts that expected and actual have a relative error less than epsilon
func (a *Assertions) InEpsilonf(expected interface{}, actual interface{}, epsilon float64, msg string, args ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	InEpsilonf(a.t, expected, actual, epsilon, msg, args...)
}

// IsDecreasing asserts that the collection is decreasing
//
//	a.IsDecreasing([]int{2, 1, 0})
//	a.IsDecreasing([]float{2, 1})
//	a.IsDecreasing([]string{"b", "a"})
func (a *Assertions) IsDecreasing(object interface{}, msgAndArgs ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	IsDecreasing(a.t, object, msgAndArgs...)
}

// IsDecreasingf asserts that the collection is decreasing
//
//	a.IsDecreasingf([]int{2, 1, 0}, "error message %s", "formatted")
//	a.IsDecreasingf([]float{2, 1}, "error message %s", "formatted")
//	a.IsDecreasingf([]string{"b", "a"}, "error message %s", "formatted")
func (a *Assertions) IsDecreasingf(object interface{}, msg string, args ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	IsDecreasingf(a.t, object, msg, args...)
}

// IsIncreasing asserts that the collection is increasing
//
//	a.IsIncreasing([]int{1, 2, 3})
//	a.IsIncreasing([]float{1, 2})
//	a.IsIncreasing([]string{"a", "b"})
func (a *Assertions) IsIncreasing(object interface{}, msgAndArgs ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	IsIncreasing(a.t, object, msgAndArgs...)
}

// IsIncreasingf asserts that the collection is increasing
//
//	a.IsIncreasingf([]int{1, 2, 3}, "error message %s", "formatted")
//	a.IsIncreasingf([]float{1, 2}, "error message %s", "formatted")
//	a.IsIncreasingf([]string{"a", "b"}, "error message %s", "formatted")
func (a *Assertions) IsIncreasingf(object interface{}, msg string, args ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	IsIncreasingf(a.t, object, msg, args...)
}

// IsNonDecreasing asserts that the collection is not decreasing
//
//	a.IsNonDecreasing([]int{1, 1, 2})
//	a.IsNonDecreasing([]float{1, 2})
//	a.IsNonDecreasing([]string{"a", "b"})
func (a *Assertions) IsNonDecreasing(object interface{}, msgAndArgs ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	IsNonDecreasing(a.t, object, msgAndArgs...)
}

// IsNonDecreasingf asserts that the collection is not decreasing
//
//	a.IsNonDecreasingf([]int{1, 1, 2}, "error message %s", "formatted")
//	a.IsNonDecreasingf([]float{1, 2}, "error message %s", "formatted")
//	a.IsNonDecreasingf([]string{"a", "b"}, "error message %s", "formatted")
func (a *Assertions) IsNonDecreasingf(object interface{}, msg string, args ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	IsNonDecreasingf(a.t, object, msg, args...)
}

// IsNonIncreasing asserts that the collection is not increasing
//
//	a.IsNonIncreasing([]int{2, 1, 1})
//	a.IsNonIncreasing([]float{2, 1})
//	a.IsNonIncreasing([]string{"b", "a"})
func (a *Assertions) IsNonIncreasing(object interface{}, msgAndArgs ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	IsNonIncreasing(a.t, object, msgAndArgs...)
}

// IsNonIncreasingf asserts that the collection is not increasing
//
//	a.IsNonIncreasingf([]int{2, 1, 1}, "error message %s", "formatted")
//	a.IsNonIncreasingf([]float{2, 1}, "error message %s", "formatted")
//	a.IsNonIncreasingf([]string{"b", "a"}, "error message %s", "formatted")
func (a *Assertions) IsNonIncreasingf(object interface{}, msg string, args ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	IsNonIncreasingf(a.t, object, msg, args...)
}

// IsType asserts that the specified objects are of the same type.
func (a *Assertions) IsType(expectedType interface{}, object interface{}, msgAndArgs ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	IsType(a.t, expectedType, object, msgAndArgs...)
}

// IsTypef asserts that the specified objects are of the same type.
func (a *Assertions) IsTypef(expectedType interface{}, object interface{}, msg string, args ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	IsTypef(a.t, expectedType, object, msg, args...)
}

// JSONEq asserts that two JSON strings are equivalent.
//
//	a.JSONEq(`{"hello": "world", "foo": "bar"}`, `{"foo": "bar", "hello": "world"}`)
func (a *Assertions) JSONEq(expected string, actual string, msgAndArgs ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	JSONEq(a.t, expected, actual, msgAndArgs...)
}

// JSONEqf asserts that two JSON strings are equivalent.
//
//	a.JSONEqf(`{"hello": "world", "foo": "bar"}`, `{"foo": "bar", "hello": "world"}`, "error message %s", "formatted")
func (a *Assertions) JSONEqf(expected string, actual string, msg string, args ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	JSONEqf(a.t, expected, actual, msg, args...)
}

// Len asserts that the specified object has specific length.
// Len also fails if the object has a type that len() not accept.
//
//	a.Len(mySlice, 3)
func (a *Assertions) Len(object interface{}, length int, msgAndArgs ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	Len(a.t, object, length, msgAndArgs...)
}

// Lenf asserts that the specified object has specific length.
// Lenf also fails if the object has a type that len() not accept.
//
//	a.Lenf(mySlice, 3, "error message %s", "formatted")
func (a *Assertions) Lenf(object interface{}, length int, msg string, args ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	Lenf(a.t, object, length, msg, args...)
}

// Less asserts that the first element is less than the second
//
//	a.Less(1, 2)
//	a.Less(float64(1), float64(2))
//	a.Less("a", "b")
func (a *Assertions) Less(e1 interface{}, e2 interface{}, msgAndArgs ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	Less(a.t, e1, e2, msgAndArgs...)
}

// LessOrEqual asserts that the first element is less than or equal to the second
//
//	a.LessOrEqual(1, 2)
//	a.LessOrEqual(2, 2)
//	a.LessOrEqual("a", "b")
//	a.LessOrEqual("b", "b")
func (a *Assertions) LessOrEqual(e1 interface{}, e2 interface{}, msgAndArgs ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	LessOrEqual(a.t, e1, e2, msgAndArgs...)
}

// LessOrEqualf asserts that the first element is less than or equal to the second
//
//	a.LessOrEqualf(1, 2, "error message %s", "formatted")
//	a.LessOrEqualf(2, 2, "error message %s", "formatted")
//	a.LessOrEqualf("a", "b", "error message %s", "formatted")
//	a.LessOrEqualf("b", "b", "error message %s", "formatted")
func (a *Assertions) LessOrEqualf(e1 interface{}, e2 interface{}, msg string, args ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	LessOrEqualf(a.t, e1, e2, msg, args...)
}

// Lessf asserts that the first element is less than the second
//
//	a.Lessf(1, 2, "error message %s", "formatted")
//	a.Lessf(float64(1), float64(2), "error message %s", "formatted")
//	a.Lessf("a", "b", "error message %s", "formatted")
func (a *Assertions) Lessf(e1 interface{}, e2 interface{}, msg string, args ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	Lessf(a.t, e1, e2, msg, args...)
}

// Negative asserts that the specified element is negative
//
//	a.Negative(-1)
//	a.Negative(-1.23)
func (a *Assertions) Negative(e interface{}, msgAndArgs ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	Negative(a.t, e, msgAndArgs...)
}

// Negativef asserts that the specified element is negative
//
//	a.Negativef(-1, "error message %s", "formatted")
//	a.Negativef(-1.23, "error message %s", "formatted")
func (a *Assertions) Negativef(e interface{}, msg string, args ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	Negativef(a.t, e, msg, args...)
}

// Never asserts that the given condition doesn't satisfy in waitFor time,
// periodically checking the target function each tick.
//
//	a.Never(func() bool { return false; }, time.Second, 10*time.Millisecond)
func (a *Assertions) Never(condition func() bool, waitFor time.Duration, tick time.Duration, msgAndArgs ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	Never(a.t, condition, waitFor, tick, msgAndArgs...)
}

// Neverf asserts that the given condition doesn't satisfy in waitFor time,
// periodically checking the target function each tick.
//
//	a.Neverf(func() bool { return false; }, time.Second, 10*time.Millisecond, "error message %s", "formatted")
func (a *Assertions) Neverf(condition func() bool, waitFor time.Duration, tick time.Duration, msg string, args ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	Neverf(a.t, condition, waitFor, tick, msg, args...)
}

// Nil asserts that the specified object is nil.
//
//	a.Nil(err)
func (a *Assertions) Nil(object interface{}, msgAndArgs ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	Nil(a.t, object, msgAndArgs...)
}

// Nilf asserts that the specified object is nil.
//
//	a.Nilf(err, "error message %s", "formatted")
func (a *Assertions) Nilf(object interface{}, msg string, args ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	Nilf(a.t, object, msg, args...)
}

// NoDirExists checks whether a directory does not exist in the given path.
// It fails if the path points to an existing _directory_ only.
func (a *Assertions) NoDirExists(path string, msgAndArgs ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	NoDirExists(a.t, path, msgAndArgs...)
}

// NoDirExistsf checks whether a directory does not exist in the given path.
// It fails if the path points to an existing _directory_ only.
func (a *Assertions) NoDirExistsf(path string, msg string, args ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	NoDirExistsf(a.t, path, msg, args...)
}

// NoError asserts that a function returned no error (i.e. `nil`).
//
//	  actualObj, err := SomeFunction()
//	  if a.NoError(err) {
//		   assert.Equal(t, expectedObj, actualObj)
//	  }
func (a *Assertions) NoError(err error, msgAndArgs ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	NoError(a.t, err, msgAndArgs...)
}

// NoErrorf asserts that a function returned no error (i.e. `nil`).
//
//	  actualObj, err := SomeFunction()
//	  if a.NoErrorf(err, "error message %s", "formatted") {
//		   assert.Equal(t, expectedObj, actualObj)
//	  }
func (a *Assertions) NoErrorf(err error, msg string, args ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	NoErrorf(a.t, err, msg, args...)
}

// NoFileExists checks whether a file does not exist in a given path. It fails
// if the path points to an existing _file_ only.
func (a *Assertions) NoFileExists(path string, msgAndArgs ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	NoFileExists(a.t, path, msgAndArgs...)
}

// NoFileExistsf checks whether a file does not exist in a given path. It fails
// if the path points to an existing _file_ only.
func (a *Assertions) NoFileExistsf(path string, msg string, args ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	NoFileExistsf(a.t, path, msg, args...)
}

// NotContains asserts that the specified string, list(array, slice...) or map does NOT contain the
// specified substring or element.
//
//	a.NotContains("Hello World", "Earth")
//	a.NotContains(["Hello", "World"], "Earth")
//	a.NotContains({"Hello": "World"}, "Earth")
func (a *Assertions) NotContains(s interface{}, contains interface{}, msgAndArgs ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	NotContains(a.t, s, contains, msgAndArgs...)
}

// NotContainsf asserts that the specified string, list(array, slice...) or map does NOT contain the
// specified substring or element.
//
//	a.NotContainsf("Hello World", "Earth", "error message %s", "formatted")
//	a.NotContainsf(["Hello", "World"], "Earth", "error message %s", "formatted")
//	a.NotContainsf({"Hello": "World"}, "Earth", "error message %s", "formatted")
func (a *Assertions) NotContainsf(s interface{}, contains interface{}, msg string, args ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	NotContainsf(a.t, s, contains, msg, args...)
}

// NotElementsMatch asserts that the specified listA(array, slice...) is NOT equal to specified
// listB(array, slice...) ignoring the order of the elements. If there are duplicate elements,
// the number of appearances of each of them in both lists should not match.
// This is an inverse of ElementsMatch.
//
// a.NotElementsMatch([1, 1, 2, 3], [1, 1, 2, 3]) -> false
//
// a.NotElementsMatch([1, 1, 2, 3], [1, 2, 3]) -> true
//
// a.NotElementsMatch([1, 2, 3], [1, 2, 4]) -> true
func (a *Assertions) NotElementsMatch(listA interface{}, listB interface{}, msgAndArgs ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	NotElementsMatch(a.t, listA, listB, msgAndArgs...)
}

// NotElementsMatchf asserts that the specified listA(array, slice...) is NOT equal to specified
// listB(array, slice...) ignoring the order of the elements. If there are duplicate elements,
// the number of appearances of each of them in both lists should not match.
// This is an inverse of ElementsMatch.
//
// a.NotElementsMatchf([1, 1, 2, 3], [1, 1, 2, 3], "error message %s", "formatted") -> false
//
// a.NotElementsMatchf([1, 1, 2, 3], [1, 2, 3], "error message %s", "formatted") -> true
//
// a.NotElementsMatchf([1, 2, 3], [1, 2, 4], "error message %s", "formatted") -> true
func (a *Assertions) NotElementsMatchf(listA interface{}, listB interface{}, msg string, args ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	NotElementsMatchf(a.t, listA, listB, msg, args...)
}

// NotEmpty asserts that the specified object is NOT empty.  I.e. not nil, "", false, 0 or either
// a slice or a channel with len == 0.
//
//	if a.NotEmpty(obj) {
//	  assert.Equal(t, "two", obj[1])
//	}
func (a *Assertions) NotEmpty(object interface{}, msgAndArgs ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	NotEmpty(a.t, object, msgAndArgs...)
}

// NotEmptyf asserts that the specified object is NOT empty.  I.e. not nil, "", false, 0 or either
// a slice or a channel with len == 0.
//
//	if a.NotEmptyf(obj, "error message %s", "formatted") {
//	  assert.Equal(t, "two", obj[1])
//	}
func (a *Assertions) NotEmptyf(object interface{}, msg string, args ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	NotEmptyf(a.t, object, msg, args...)
}

// NotEqual asserts that the specified values are NOT equal.
//
//	a.NotEqual(obj1, obj2)
//
// Pointer variable equality is determined based on the equality of the
// referenced values (as opposed to the memory addresses).
func (a *Assertions) NotEqual(expected interface{}, actual interface{}, msgAndArgs ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	NotEqual(a.t, expected, actual, msgAndArgs...)
}

// NotEqualValues asserts that two objects are not equal even when converted to the same type
//
//	a.NotEqualValues(obj1, obj2)
func (a *Assertions) NotEqualValues(expected interface{}, actual interface{}, msgAndArgs ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	NotEqualValues(a.t, expected, actual, msgAndArgs...)
}

// NotEqualValuesf asserts that two objects are not equal even when converted to the same type
//
//	a.NotEqualValuesf(obj1, obj2, "error message %s", "formatted")
func (a *Assertions) NotEqualValuesf(expected interface{}, actual interface{}, msg string, args ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	NotEqualValuesf(a.t, expected, actual, msg, args...)
}

// NotEqualf asserts that the specified values are NOT equal.
//
//	a.NotEqualf(obj1, obj2, "error message %s", "formatted")
//
// Pointer variable equality is determined based on the equality of the
// referenced values (as opposed to the memory addresses).
func (a *Assertions) NotEqualf(expected interface{}, actual interface{}, msg string, args ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	NotEqualf(a.t, expected, actual, msg, args...)
}

// NotErrorAs asserts that none of the errors in err's chain matches target,
// but if so, sets target to that error value.
func (a *Assertions) NotErrorAs(err error, target interface{}, msgAndArgs ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	NotErrorAs(a.t, err, target, msgAndArgs...)
}

// NotErrorAsf asserts that none of the errors in err's chain matches target,
// but if so, sets target to that error value.
func (a *Assertions) NotErrorAsf(err error, target interface{}, msg string, args ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	NotErrorAsf(a.t, err, target, msg, args...)
}

// NotErrorIs asserts that none of the errors in err's chain matches target.
// This is a wrapper for errors.Is.
func (a *Assertions) NotErrorIs(err error, target error, msgAndArgs ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	NotErrorIs(a.t, err, target, msgAndArgs...)
}

// NotErrorIsf asserts that none of the errors in err's chain matches target.
// This is a wrapper for errors.Is.
func (a *Assertions) NotErrorIsf(err error, target error, msg string, args ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	NotErrorIsf(a.t, err, target, msg, args...)
}

// NotImplements asserts that an object does not implement the specified interface.
//
//	a.NotImplements((*MyInterface)(nil), new(MyObject))
func (a *Assertions) NotImplements(interfaceObject interface{}, object interface{}, msgAndArgs ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	NotImplements(a.t, interfaceObject, object, msgAndArgs...)
}

// NotImplementsf asserts that an object does not implement the specified interface.
//
//	a.NotImplementsf((*MyInterface)(nil), new(MyObject), "error message %s", "formatted")
func (a *Assertions) NotImplementsf(interfaceObject interface{}, object interface{}, msg string, args ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	NotImplementsf(a.t, interfaceObject, object, msg, args...)
}

// NotNil asserts that the specified object is not nil.
//
//	a.NotNil(err)
func (a *Assertions) NotNil(object interface{}, msgAndArgs ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	NotNil(a.t, object, msgAndArgs...)
}

// NotNilf asserts that the specified object is not nil.
//
//	a.NotNilf(err, "error message %s", "formatted")
func (a *Assertions) NotNilf(object interface{}, msg string, args ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	NotNilf(a.t, object, msg, args...)
}

// NotPanics asserts that the code inside the specified PanicTestFunc does NOT panic.
//
//	a.NotPanics(func(){ RemainCalm() })
func (a *Assertions) NotPanics(f assert.PanicTestFunc, msgAndArgs ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	NotPanics(a.t, f, msgAndArgs...)
}

// NotPanicsf asserts that the code inside the specified PanicTestFunc does NOT panic.
//
//	a.NotPanicsf(func(){ RemainCalm() }, "error message %s", "formatted")
func (a *Assertions) NotPanicsf(f assert.PanicTestFunc, msg string, args ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	NotPanicsf(a.t, f, msg, args...)
}

// NotRegexp asserts that a specified regexp does not match a string.
//
//	a.NotRegexp(regexp.MustCompile("starts"), "it's starting")
//	a.NotRegexp("^start", "it's not starting")
func (a *Assertions) NotRegexp(rx interface{}, str interface{}, msgAndArgs ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	NotRegexp(a.t, rx, str, msgAndArgs...)
}

// NotRegexpf asserts that a specified regexp does not match a string.
//
//	a.NotRegexpf(regexp.MustCompile("starts"), "it's starting", "error message %s", "formatted")
//	a.NotRegexpf("^start", "it's not starting", "error message %s", "formatted")
func (a *Assertions) NotRegexpf(rx interface{}, str interface{}, msg string, args ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	NotRegexpf(a.t, rx, str, msg, args...)
}

// NotSame asserts that two pointers do not reference the same object.
//
//	a.NotSame(ptr1, ptr2)
//
// Both arguments must be pointer variables. Pointer variable sameness is
// determined based on the equality of both type and value.
func (a *Assertions) NotSame(expected interface{}, actual interface{}, msgAndArgs ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	NotSame(a.t, expected, actual, msgAndArgs...)
}

// NotSamef asserts that two pointers do not reference the same object.
//
//	a.NotSamef(ptr1, ptr2, "error message %s", "formatted")
//
// Both arguments must be pointer variables. Pointer variable sameness is
// determined based on the equality of both type and value.
func (a *Assertions) NotSamef(expected interface{}, actual interface{}, msg string, args ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	NotSamef(a.t, expected, actual, msg, args...)
}

// NotSubset asserts that the specified list(array, slice...) or map does NOT
// contain all elements given in the specified subset list(array, slice...) or
// map.
//
//	a.NotSubset([1, 3, 4], [1, 2])
//	a.NotSubset({"x": 1, "y": 2}, {"z": 3})
func (a *Assertions) NotSubset(list interface{}, subset interface{}, msgAndArgs ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	NotSubset(a.t, list, subset, msgAndArgs...)
}

// NotSubsetf asserts that the specified list(array, slice...) or map does NOT
// contain all elements given in the specified subset list(array, slice...) or
// map.
//
//	a.NotSubsetf([1, 3, 4], [1, 2], "error message %s", "formatted")
//	a.NotSubsetf({"x": 1, "y": 2}, {"z": 3}, "error message %s", "formatted")
func (a *Assertions) NotSubsetf(list interface{}, subset interface{}, msg string, args ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	NotSubsetf(a.t, list, subset, msg, args...)
}

// NotZero asserts that i is not the zero value for its type.
func (a *Assertions) NotZero(i interface{}, msgAndArgs ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	NotZero(a.t, i, msgAndArgs...)
}

// NotZerof asserts that i is not the zero value for its type.
func (a *Assertions) NotZerof(i interface{}, msg string, args ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	NotZerof(a.t, i, msg, args...)
}

// Panics asserts that the code inside the specified PanicTestFunc panics.
//
//	a.Panics(func(){ GoCrazy() })
func (a *Assertions) Panics(f assert.PanicTestFunc, msgAndArgs ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	Panics(a.t, f, msgAndArgs...)
}

// PanicsWithError asserts that the code inside the specified PanicTestFunc
// panics, and that the recovered panic value is an error that satisfies the
// EqualError comparison.
//
//	a.PanicsWithError("crazy error", func(){ GoCrazy() })
func (a *Assertions) PanicsWithError(errString string, f assert.PanicTestFunc, msgAndArgs ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	PanicsWithError(a.t, errString, f, msgAndArgs...)
}

// PanicsWithErrorf asserts that the code inside the specified PanicTestFunc
// panics, and that the recovered panic value is an error that satisfies the
// EqualError comparison.
//
//	a.PanicsWithErrorf("crazy error", func(){ GoCrazy() }, "error message %s", "formatted")
func (a *Assertions) PanicsWithErrorf(errString string, f assert.PanicTestFunc, msg string, args ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	PanicsWithErrorf(a.t, errString, f, msg, args...)
}

// PanicsWithValue asserts that the code inside the specified PanicTestFunc panics, and that
// the recovered panic value equals the expected panic value.
//
//	a.PanicsWithValue("crazy error", func(){ GoCrazy() })
func (a *Assertions) PanicsWithValue(expected interface{}, f assert.PanicTestFunc, msgAndArgs ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	PanicsWithValue(a.t, expected, f, msgAndArgs...)
}

// PanicsWithValuef asserts that the code inside the specified PanicTestFunc panics, and that
// the recovered panic value equals the expected panic value.
//
//	a.PanicsWithValuef("crazy error", func(){ GoCrazy() }, "error message %s", "formatted")
func (a *Assertions) PanicsWithValuef(expected interface{}, f assert.PanicTestFunc, msg string, args ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	PanicsWithValuef(a.t, expected, f, msg, args...)
}

// Panicsf asserts that the code inside the specified PanicTestFunc panics.
//
//	a.Panicsf(func(){ GoCrazy() }, "error message %s", "formatted")
func (a *Assertions) Panicsf(f assert.PanicTestFunc, msg string, args ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	Panicsf(a.t, f, msg, args...)
}

// Positive asserts that the specified element is positive
//
//	a.Positive(1)
//	a.Positive(1.23)
func (a *Assertions) Positive(e interface{}, msgAndArgs ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	Positive(a.t, e, msgAndArgs...)
}

// Positivef asserts that the specified element is positive
//
//	a.Positivef(1, "error message %s", "formatted")
//	a.Positivef(1.23, "error message %s", "formatted")
func (a *Assertions) Positivef(e interface{}, msg string, args ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	Positivef(a.t, e, msg, args...)
}

// Regexp asserts that a specified regexp matches a string.
//
//	a.Regexp(regexp.MustCompile("start"), "it's starting")
//	a.Regexp("start...$", "it's not starting")
func (a *Assertions) Regexp(rx interface{}, str interface{}, msgAndArgs ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	Regexp(a.t, rx, str, msgAndArgs...)
}

// Regexpf asserts that a specified regexp matches a string.
//
//	a.Regexpf(regexp.MustCompile("start"), "it's starting", "error message %s", "formatted")
//	a.Regexpf("start...$", "it's not starting", "error message %s", "formatted")
func (a *Assertions) Regexpf(rx interface{}, str interface{}, msg string, args ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	Regexpf(a.t, rx, str, msg, args...)
}

// Same asserts that two pointers reference the same object.
//
//	a.Same(ptr1, ptr2)
//
// Both arguments must be pointer variables. Pointer variable sameness is
// determined based on the equality of both type and value.
func (a *Assertions) Same(expected interface{}, actual interface{}, msgAndArgs ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	Same(a.t, expected, actual, msgAndArgs...)
}

// Samef asserts that two pointers reference the same object.
//
//	a.Samef(ptr1, ptr2, "error message %s", "formatted")
//
// Both arguments must be pointer variables. Pointer variable sameness is
// determined based on the equality of both type and value.
func (a *Assertions) Samef(expected interface{}, actual interface{}, msg string, args ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	Samef(a.t, expected, actual, msg, args...)
}

// Subset asserts that the specified list(array, slice...) or map contains all
// elements given in the specified subset list(array, slice...) or map.
//
//	a.Subset([1, 2, 3], [1, 2])
//	a.Subset({"x": 1, "y": 2}, {"x": 1})
func (a *Assertions) Subset(list interface{}, subset interface{}, msgAndArgs ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	Subset(a.t, list, subset, msgAndArgs...)
}

// Subsetf asserts that the specified list(array, slice...) or map contains all
// elements given in the specified subset list(array, slice...) or map.
//
//	a.Subsetf([1, 2, 3], [1, 2], "error message %s", "formatted")
//	a.Subsetf({"x": 1, "y": 2}, {"x": 1}, "error message %s", "formatted")
func (a *Assertions) Subsetf(list interface{}, subset interface{}, msg string, args ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	Subsetf(a.t, list, subset, msg, args...)
}

// True asserts that the specified value is true.
//
//	a.True(myBool)
func (a *Assertions) True(value bool, msgAndArgs ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	True(a.t, value, msgAndArgs...)
}

// Truef asserts that the specified value is true.
//
//	a.Truef(myBool, "error message %s", "formatted")
func (a *Assertions) Truef(value bool, msg string, args ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	Truef(a.t, value, msg, args...)
}

// WithinDuration asserts that the two times are within duration delta of each other.
//
//	a.WithinDuration(time.Now(), time.Now(), 10*time.Second)
func (a *Assertions) WithinDuration(expected time.Time, actual time.Time, delta time.Duration, msgAndArgs ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	WithinDuration(a.t, expected, actual, delta, msgAndArgs...)
}

// WithinDurationf asserts that the two times are within duration delta of each other.
//
//	a.WithinDurationf(time.Now(), time.Now(), 10*time.Second, "error message %s", "formatted")
func (a *Assertions) WithinDurationf(expected time.Time, actual time.Time, delta time.Duration, msg string, args ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	WithinDurationf(a.t, expected, actual, delta, msg, args...)
}

// WithinRange asserts that a time is within a time range (inclusive).
//
//	a.WithinRange(time.Now(), time.Now().Add(-time.Second), time.Now().Add(time.Second))
func (a *Assertions) WithinRange(actual time.Time, start time.Time, end time.Time, msgAndArgs ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	WithinRange(a.t, actual, start, end, msgAndArgs...)
}

// WithinRangef asserts that a time is within a time range (inclusive).
//
//	a.WithinRangef(time.Now(), time.Now().Add(-time.Second), time.Now().Add(time.Second), "error message %s", "formatted")
func (a *Assertions) WithinRangef(actual time.Time, start time.Time, end time.Time, msg string, args ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	WithinRangef(a.t, actual, start, end, msg, args...)
}

// YAMLEq asserts that two YAML strings are equivalent.
func (a *Assertions) YAMLEq(expected string, actual string, msgAndArgs ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	YAMLEq(a.t, expected, actual, msgAndArgs...)
}

// YAMLEqf asserts that two YAML strings are equivalent.
func (a *Assertions) YAMLEqf(expected string, actual string, msg string, args ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	YAMLEqf(a.t, expected, actual, msg, args...)
}

// Zero asserts that i is the zero value for its type.
func (a *Assertions) Zero(i interface{}, msgAndArgs ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	Zero(a.t, i, msgAndArgs...)
}

// Zerof asserts that i is the zero value for its type.
func (a *Assertions) Zerof(i interface{}, msg string, args ...interface{}) {
	if h, ok := a.t.(tHelper); ok {
		h.Helper()
	}
	Zerof(a.t, i, msg, args...)
}
