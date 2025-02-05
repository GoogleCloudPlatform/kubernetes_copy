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

package field

import (
	"fmt"
	"strings"
	"testing"
)

func TestMakeFuncs(t *testing.T) {
	testCases := []struct {
		fn       func() *Error
		expected ErrorType
	}{
		{
			func() *Error { return Invalid(NewPath("f"), "v", "d") },
			ErrorTypeInvalid,
		},
		{
			func() *Error { return NotSupported[string](NewPath("f"), "v", nil) },
			ErrorTypeNotSupported,
		},
		{
			func() *Error { return Duplicate(NewPath("f"), "v") },
			ErrorTypeDuplicate,
		},
		{
			func() *Error { return NotFound(NewPath("f"), "v") },
			ErrorTypeNotFound,
		},
		{
			func() *Error { return Required(NewPath("f"), "d") },
			ErrorTypeRequired,
		},
		{
			func() *Error { return InternalError(NewPath("f"), fmt.Errorf("e")) },
			ErrorTypeInternal,
		},
	}

	for _, testCase := range testCases {
		err := testCase.fn()
		if err.Type != testCase.expected {
			t.Errorf("expected Type %q, got %q", testCase.expected, err.Type)
		}
	}
}

func TestErrorUsefulMessage(t *testing.T) {
	{
		s := Invalid(nil, nil, "").Error()
		t.Logf("message: %v", s)
		if !strings.Contains(s, "null") {
			t.Errorf("error message did not contain 'null': %s", s)
		}
	}

	s := Invalid(NewPath("foo"), "bar", "deet").Error()
	t.Logf("message: %v", s)
	for _, part := range []string{"foo", "bar", "deet", ErrorTypeInvalid.String()} {
		if !strings.Contains(s, part) {
			t.Errorf("error message did not contain expected part '%v'", part)
		}
	}

	type complicated struct {
		Baz   int
		Qux   string
		Inner interface{}
		KV    map[string]int
	}
	s = Invalid(
		NewPath("foo"),
		&complicated{
			Baz:   1,
			Qux:   "aoeu",
			Inner: &complicated{Qux: "asdf"},
			KV:    map[string]int{"Billy": 2},
		},
		"detail",
	).Error()
	t.Logf("message: %v", s)
	for _, part := range []string{
		"foo", ErrorTypeInvalid.String(),
		"Baz", "Qux", "Inner", "KV", "detail",
		"1", "aoeu", "Billy", "2",
		// "asdf", TODO: re-enable once we have a better nested printer
	} {
		if !strings.Contains(s, part) {
			t.Errorf("error message did not contain expected part '%v'", part)
		}
	}
}

func TestToAggregate(t *testing.T) {
	testCases := struct {
		ErrList         []ErrorList
		NumExpectedErrs []int
	}{
		[]ErrorList{
			nil,
			{},
			{Invalid(NewPath("f"), "v", "d")},
			{Invalid(NewPath("f"), "v", "d"), Invalid(NewPath("f"), "v", "d")},
			{Invalid(NewPath("f"), "v", "d"), InternalError(NewPath(""), fmt.Errorf("e"))},
		},
		[]int{
			0,
			0,
			1,
			1,
			2,
		},
	}

	if len(testCases.ErrList) != len(testCases.NumExpectedErrs) {
		t.Errorf("Mismatch: length of NumExpectedErrs does not match length of ErrList")
	}
	for i, tc := range testCases.ErrList {
		agg := tc.ToAggregate()
		numErrs := 0

		if agg != nil {
			numErrs = len(agg.Errors())
		}
		if numErrs != testCases.NumExpectedErrs[i] {
			t.Errorf("[%d] Expected %d, got %d", i, testCases.NumExpectedErrs[i], numErrs)
		}

		if len(tc) == 0 {
			if agg != nil {
				t.Errorf("[%d] Expected nil, got %#v", i, agg)
			}
		} else if agg == nil {
			t.Errorf("[%d] Expected non-nil", i)
		}
	}
}

func TestErrListFilter(t *testing.T) {
	list := ErrorList{
		Invalid(NewPath("test.field"), "", ""),
		Invalid(NewPath("field.test"), "", ""),
		Duplicate(NewPath("test"), "value"),
	}
	if len(list.Filter(NewErrorTypeMatcher(ErrorTypeDuplicate))) != 2 {
		t.Errorf("should not filter")
	}
	if len(list.Filter(NewErrorTypeMatcher(ErrorTypeInvalid))) != 1 {
		t.Errorf("should filter")
	}
}

func TestNotSupported(t *testing.T) {
	notSupported := NotSupported(NewPath("f"), "v", []string{"a", "b", "c"})
	expected := `Unsupported value: "v": supported values: "a", "b", "c"`
	if notSupported.ErrorBody() != expected {
		t.Errorf("Expected: %s\n, but got: %s\n", expected, notSupported.ErrorBody())
	}
}

func TestRender(t *testing.T) {
	testCases := []struct {
		name     string
		input    interface{}
		expected renderedVal
	}{
		{
			name:     "string value",
			input:    "test",
			expected: `"test"`,
		},
		{
			name:     "integer value",
			input:    42,
			expected: "42",
		},
		{
			name:     "boolean value",
			input:    true,
			expected: "true",
		},
		{
			name:     "nil value",
			input:    nil,
			expected: "<nil>",
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			result := render(testCase.input)
			if testCase.expected != result {
				t.Errorf("Expected %s\n but got: %s\n", testCase.expected, result)
			}
		})
	}
}

func TestNot(t *testing.T) {
	testCases := []struct {
		name     string
		input    interface{}
		expected renderedVal
	}{
		{
			name:     "string value",
			input:    "test",
			expected: `not "test"`,
		},
		{
			name:     "integer value",
			input:    42,
			expected: "not 42",
		},
		{
			name:     "boolean value",
			input:    true,
			expected: "not true",
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			result := Not(testCase.input)
			if testCase.expected != result {
				t.Errorf("Expected %s\n but got: %s\n", testCase.expected, result)
			}
		})
	}
}

func TestDependsOn(t *testing.T) {
	dependsOn := DependsOn(NewPath("f"), NewPath("o"))
	expected := `Forbidden: may only be specified when o is specified`
	if dependsOn.ErrorBody() != expected {
		t.Errorf("Expected: %s\n, but got: %s\n", expected, dependsOn.ErrorBody())
	}
}

func TestDependsOnValue(t *testing.T) {
	dependsOnValue := DependsOnValue(NewPath("f"), NewPath("o"), renderedVal("specified"))
	expected := `Forbidden: may only be specified when o is specified`
	if dependsOnValue.ErrorBody() != expected {
		t.Errorf("Expected: %s\n, but got: %s\n", expected, dependsOnValue.ErrorBody())
	}
}

func TestRequiredWhen(t *testing.T) {
	requiredwhen := RequiredWhen(NewPath("f"), NewPath("o"))
	expected := "Required value: must be specified when o is specified"
	if requiredwhen.ErrorBody() != expected {
		t.Errorf("Expected: %s\n, but got: %s\n", expected, requiredwhen.ErrorBody())
	}
}

func TestRequiredWhenValue(t *testing.T) {
	requiredWhenValue := RequiredWhenValue(NewPath("f"), NewPath("o"), "v")
	expected := `Required value: must be specified when o is "v"`
	if requiredWhenValue.ErrorBody() != expected {
		t.Errorf("Expected: %s\n, but got: %s\n", expected, requiredWhenValue.ErrorBody())
	}
}
