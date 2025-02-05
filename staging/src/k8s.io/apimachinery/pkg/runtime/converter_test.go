/*
Copyright 2015 The Kubernetes Authors.

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

// These tests are in a separate package to break cyclic dependency in tests.
// Unstructured type depends on unstructured converter package but we want to test how the converter handles
// the Unstructured type so we need to import both.

package runtime_test

import (
	encodingjson "encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/conversion"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/json"

	"github.com/google/go-cmp/cmp"
	fuzz "github.com/google/gofuzz"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var simpleEquality = conversion.EqualitiesOrDie(
	func(a, b time.Time) bool {
		return a.UTC() == b.UTC()
	},
)

// Define a number of test types.
type A struct {
	A int    `json:"aa,omitempty"`
	B string `json:"ab,omitempty"`
	C bool   `json:"ac,omitempty"`
}

type B struct {
	A A                 `json:"ba"`
	B string            `json:"bb"`
	C map[string]string `json:"bc"`
	D []string          `json:"bd"`
}

type C struct {
	A []A `json:"ca"`
	B `json:",inline"`
	C string         `json:"cc"`
	D *int64         `json:"cd"`
	E map[string]int `json:"ce"`
	F []bool         `json:"cf"`
	G []int          `json:"cg"`
	H float32        `json:"ch"`
	I []interface{}  `json:"ci"`
}

type D struct {
	A []interface{} `json:"da"`
}

type E struct {
	A interface{} `json:"ea"`
}

type F struct {
	A string            `json:"fa"`
	B map[string]string `json:"fb"`
	C []A               `json:"fc"`
	D int               `json:"fd"`
	E float32           `json:"fe"`
	F []string          `json:"ff"`
	G []int             `json:"fg"`
	H []bool            `json:"fh"`
	I []float32         `json:"fi"`
	J []byte            `json:"fj"`
	K uint32            `json:"fk"`
}

type G struct {
	CustomValue1   CustomValue    `json:"customValue1"`
	CustomValue2   *CustomValue   `json:"customValue2"`
	CustomPointer1 CustomPointer  `json:"customPointer1"`
	CustomPointer2 *CustomPointer `json:"customPointer2"`
}

type H struct {
	A A `json:"ha"`
	C `json:",inline"`
}

type I struct {
	A A `json:"ia"`
	H `json:",inline"`

	UL1 UnknownLevel1 `json:"ul1"`
}

type UnknownLevel1 struct {
	A          int64 `json:"a"`
	InlinedAA  `json:",inline"`
	InlinedAAA `json:",inline"`
}
type InlinedAA struct {
	AA int64 `json:"aa"`
}
type InlinedAAA struct {
	AAA   int64         `json:"aaa"`
	Child UnknownLevel2 `json:"child"`
}

type UnknownLevel2 struct {
	B          int64 `json:"b"`
	InlinedBB  `json:",inline"`
	InlinedBBB `json:",inline"`
}
type InlinedBB struct {
	BB int64 `json:"bb"`
}
type InlinedBBB struct {
	BBB   int64         `json:"bbb"`
	Child UnknownLevel3 `json:"child"`
}

type UnknownLevel3 struct {
	C          int64 `json:"c"`
	InlinedCC  `json:",inline"`
	InlinedCCC `json:",inline"`
}
type InlinedCC struct {
	CC int64 `json:"cc"`
}
type InlinedCCC struct {
	CCC int64 `json:"ccc"`
}

type CustomValue struct {
	data []byte
}

// MarshalJSON has a value receiver on this type.
func (c CustomValue) MarshalJSON() ([]byte, error) {
	return c.data, nil
}

type CustomPointer struct {
	data []byte
}

// MarshalJSON has a pointer receiver on this type.
func (c *CustomPointer) MarshalJSON() ([]byte, error) {
	return c.data, nil
}

func doRoundTrip(t *testing.T, item interface{}) {
	data, err := json.Marshal(item)
	if err != nil {
		t.Errorf("Error when marshaling object: %v", err)
		return
	}

	unstr := make(map[string]interface{})
	err = json.Unmarshal(data, &unstr)
	if err != nil {
		t.Errorf("Error when unmarshaling to unstructured: %v", err)
		return
	}

	data, err = json.Marshal(unstr)
	if err != nil {
		t.Errorf("Error when marshaling unstructured: %v", err)
		return
	}
	unmarshalledObj := reflect.New(reflect.TypeOf(item).Elem()).Interface()
	err = json.Unmarshal(data, unmarshalledObj)
	if err != nil {
		t.Errorf("Error when unmarshaling to object: %v", err)
		return
	}
	if !reflect.DeepEqual(item, unmarshalledObj) {
		t.Errorf("Object changed during JSON operations, diff: %v", cmp.Diff(item, unmarshalledObj))
		return
	}

	// TODO: should be using mismatch detection but fails due to another error
	newUnstr, err := runtime.DefaultUnstructuredConverter.ToUnstructured(item)
	if err != nil {
		t.Errorf("ToUnstructured failed: %v", err)
		return
	}

	newObj := reflect.New(reflect.TypeOf(item).Elem()).Interface()
	err = runtime.NewTestUnstructuredConverter(simpleEquality).FromUnstructured(newUnstr, newObj)
	if err != nil {
		t.Errorf("FromUnstructured failed: %v", err)
		return
	}

	if !reflect.DeepEqual(item, newObj) {
		t.Errorf("Object changed, diff: %v", cmp.Diff(item, newObj))
	}
}

func TestRoundTrip(t *testing.T) {
	intVal := int64(42)
	testCases := []struct {
		obj interface{}
	}{
		{
			obj: &unstructured.UnstructuredList{
				Object: map[string]interface{}{
					"kind": "List",
				},
				// Not testing a list with nil Items because items is a non-optional field and hence
				// is always marshaled into an empty array which is not equal to nil when unmarshalled and will fail.
				// That is expected.
				Items: []unstructured.Unstructured{},
			},
		},
		{
			obj: &unstructured.UnstructuredList{
				Object: map[string]interface{}{
					"kind": "List",
				},
				Items: []unstructured.Unstructured{
					{
						Object: map[string]interface{}{
							"kind": "Pod",
						},
					},
				},
			},
		},
		{
			obj: &unstructured.Unstructured{
				Object: map[string]interface{}{
					"kind": "Pod",
				},
			},
		},
		{
			obj: &unstructured.Unstructured{
				Object: map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "Foo",
					"metadata": map[string]interface{}{
						"name": "foo1",
					},
				},
			},
		},
		{
			// This (among others) tests nil map, slice and pointer.
			obj: &C{
				C: "ccc",
			},
		},
		{
			// This (among others) tests empty map and slice.
			obj: &C{
				A: []A{},
				C: "ccc",
				E: map[string]int{},
				I: []interface{}{},
			},
		},
		{
			obj: &C{
				A: []A{
					{
						A: 1,
						B: "11",
						C: true,
					},
					{
						A: 2,
						B: "22",
						C: false,
					},
				},
				B: B{
					A: A{
						A: 3,
						B: "33",
					},
					B: "bbb",
					C: map[string]string{
						"k1": "v1",
						"k2": "v2",
					},
					D: []string{"s1", "s2"},
				},
				C: "ccc",
				D: &intVal,
				E: map[string]int{
					"k1": 1,
					"k2": 2,
				},
				F: []bool{true, false, false},
				G: []int{1, 2, 5},
				H: 3.3,
				I: []interface{}{nil, nil, nil},
			},
		},
		{
			// Test slice of interface{} with empty slices.
			obj: &D{
				A: []interface{}{[]interface{}{}, []interface{}{}},
			},
		},
		{
			// Test slice of interface{} with different values.
			obj: &D{
				A: []interface{}{float64(3.5), int64(4), "3.0", nil},
			},
		},
	}

	for i := range testCases {
		t.Run(fmt.Sprintf("%d", i), func(t *testing.T) {
			doRoundTrip(t, testCases[i].obj)
		})
	}
}

// TestUnknownFields checks for the collection of unknown
// field errors from the various possible locations of
// unknown fields (e.g. fields on struct, inlined struct, slice, etc)
func TestUnknownFields(t *testing.T) {
	// simples checks that basic unknown fields are found
	// in fields, subfields and slices.
	var simplesData = `{
"ca": [
	{
		"aa": 1,
		"ab": "ab",
		"ac": true,
		"unknown1": 24
	}
],
"cc": "ccstring",
"unknown2": "foo"
}`

	var simplesErrs = []string{
		`unknown field "ca[0].unknown1"`,
		`unknown field "unknown2"`,
	}

	// same-name, different-levels checks that
	// fields at a higher level in the json
	// are not persisted to unrecognized fields
	// at lower levels and vice-versa.
	//
	// In this case, the field "cc" exists at the root level
	// but not in the nested field ul1. If we are
	// improperly retaining matched keys, this not
	// see an issue with "cc" existing inside "ul1"
	//
	// The opposite for "aaa", which exists at the
	// nested level but not at the root.
	var sameNameDiffLevelData = `
	{
		"cc": "foo",
		"aaa": 1,
		"ul1": {
			"aa": 1,
			"aaa": 1,
			"cc": 1

		}
}`
	var sameNameDiffLevelErrs = []string{
		`unknown field "aaa"`,
		`unknown field "ul1.cc"`,
	}

	// inlined-inlined confirms that we see
	// fields that are doubly nested and don't recognize
	// those that aren't
	var inlinedInlinedData = `{
		"bb": "foo",
		"bc": {
			"foo": "bar"
		},
		"bd": ["d1", "d2"],
		"aa": 1
}`

	var inlinedInlinedErrs = []string{
		`unknown field "aa"`,
	}

	// combined tests everything together
	var combinedData = `
	{
		"ia": {
			"aa": 1,
			"ab": "ab",
			"unknownI": "foo"
		},
		"ha": {
			"aa": 2,
			"ab": "ab2",
			"unknownH": "foo"
		},
		"ca":[
			{
				"aa":1,
				"ab":"11",
				"ac":true
			},
			{
				"aa":2,
				"ab":"22",
				"unknown1": "foo"
			},
			{
				"aa":3,
				"ab":"33",
				"unknown2": "foo"
			}
		],
		"ba":{
			"aa":3,
			"ab":"33",
			"ac": true,
			"unknown3": 26,
			"unknown4": "foo"
		},
		"unknown5": "foo",
		"bb":"bbb",
		"bc":{
			"k1":"v1",
			"k2":"v2"
		},
		"bd":[
			"s1",
			"s2"
		],
		"cc":"ccc",
		"cd":42,
		"ce":{
			"k1":1,
			"k2":2
		},
		"cf":[
			true,
			false,
			false
		],
		"cg":
		[
			1,
			2,
			5
		],
		"ch":3.3,
		"ci":[
			null,
			null,
			null
		],
		"ul1": {
			"a": 1,
			"aa": 1,
			"aaa": 1,
			"b": 1,
			"bb": 1,
			"bbb": 1,
			"c": 1,
			"cc": 1,
			"ccc": 1,
			"child": {
				"a": 1,
				"aa": 1,
				"aaa": 1,
				"b": 1,
				"bb": 1,
				"bbb": 1,
				"c": 1,
				"cc": 1,
				"ccc": 1,
				"child": {
					"a": 1,
					"aa": 1,
					"aaa": 1,
					"b": 1,
					"bb": 1,
					"bbb": 1,
					"c": 1,
					"cc": 1,
					"ccc": 1
				}
			}
		}
}`

	var combinedErrs = []string{
		`unknown field "ca[1].unknown1"`,
		`unknown field "ca[2].unknown2"`,
		`unknown field "ba.unknown3"`,
		`unknown field "ba.unknown4"`,
		`unknown field "unknown5"`,
		`unknown field "ha.unknownH"`,
		`unknown field "ia.unknownI"`,

		`unknown field "ul1.b"`,
		`unknown field "ul1.bb"`,
		`unknown field "ul1.bbb"`,
		`unknown field "ul1.c"`,
		`unknown field "ul1.cc"`,
		`unknown field "ul1.ccc"`,

		`unknown field "ul1.child.a"`,
		`unknown field "ul1.child.aa"`,
		`unknown field "ul1.child.aaa"`,
		`unknown field "ul1.child.c"`,
		`unknown field "ul1.child.cc"`,
		`unknown field "ul1.child.ccc"`,

		`unknown field "ul1.child.child.a"`,
		`unknown field "ul1.child.child.aa"`,
		`unknown field "ul1.child.child.aaa"`,
		`unknown field "ul1.child.child.b"`,
		`unknown field "ul1.child.child.bb"`,
		`unknown field "ul1.child.child.bbb"`,
	}

	testCases := []struct {
		jsonData            string
		obj                 interface{}
		returnUnknownFields bool
		expectedErrs        []string
	}{
		{
			jsonData:            simplesData,
			obj:                 &C{},
			returnUnknownFields: true,
			expectedErrs:        simplesErrs,
		},
		{
			jsonData:            simplesData,
			obj:                 &C{},
			returnUnknownFields: false,
		},
		{
			jsonData:            sameNameDiffLevelData,
			obj:                 &I{},
			returnUnknownFields: true,
			expectedErrs:        sameNameDiffLevelErrs,
		},
		{
			jsonData:            sameNameDiffLevelData,
			obj:                 &I{},
			returnUnknownFields: false,
		},
		{
			jsonData:            inlinedInlinedData,
			obj:                 &I{},
			returnUnknownFields: true,
			expectedErrs:        inlinedInlinedErrs,
		},
		{
			jsonData:            inlinedInlinedData,
			obj:                 &I{},
			returnUnknownFields: false,
		},
		{
			jsonData:            combinedData,
			obj:                 &I{},
			returnUnknownFields: true,
			expectedErrs:        combinedErrs,
		},
		{
			jsonData:            combinedData,
			obj:                 &I{},
			returnUnknownFields: false,
		},
	}

	for i, tc := range testCases {
		t.Run(fmt.Sprintf("%d", i), func(t *testing.T) {
			unstr := make(map[string]interface{})
			err := json.Unmarshal([]byte(tc.jsonData), &unstr)
			if err != nil {
				t.Errorf("Error when unmarshaling to unstructured: %v", err)
				return
			}
			err = runtime.NewTestUnstructuredConverterWithValidation(simpleEquality).FromUnstructuredWithValidation(unstr, tc.obj, tc.returnUnknownFields)
			if len(tc.expectedErrs) == 0 && err != nil {
				t.Errorf("unexpected err: %v", err)
			}
			var errString string
			if err != nil {
				errString = err.Error()
			}
			missedErrs := []string{}
			failed := false
			for _, expected := range tc.expectedErrs {
				if !strings.Contains(errString, expected) {
					failed = true
					missedErrs = append(missedErrs, expected)
				} else {
					errString = strings.Replace(errString, expected, "", 1)
				}
			}
			if failed {
				for _, e := range missedErrs {
					t.Errorf("missing err: %v\n", e)
				}
			}
			leftoverErrors := strings.TrimSpace(strings.TrimPrefix(strings.ReplaceAll(errString, ",", ""), "strict decoding error:"))
			if leftoverErrors != "" {
				t.Errorf("found unexpected errors: %s", leftoverErrors)
			}
		})
	}
}

// BenchmarkFromUnstructuredWithValidation benchmarks
// the time and memory required to perform FromUnstructured
// with the various validation directives (Ignore, Warn, Strict)
func BenchmarkFromUnstructuredWithValidation(b *testing.B) {
	re := regexp.MustCompile("^I$")
	f := fuzz.NewWithSeed(1).NilChance(0.1).SkipFieldsWithPattern(re)
	iObj := &I{}
	f.Fuzz(&iObj)

	unstr, err := runtime.DefaultUnstructuredConverter.ToUnstructured(iObj)
	if err != nil {
		b.Fatalf("ToUnstructured failed: %v", err)
		return
	}
	for _, shouldReturn := range []bool{false, true} {
		b.Run(fmt.Sprintf("shouldReturn=%t", shouldReturn), func(b *testing.B) {
			newObj := reflect.New(reflect.TypeOf(iObj).Elem()).Interface()
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if err = runtime.NewTestUnstructuredConverterWithValidation(simpleEquality).FromUnstructuredWithValidation(unstr, newObj, shouldReturn); err != nil {
					b.Fatalf("FromUnstructured failed: %v", err)
					return
				}
			}
		})
	}
}

// Verifies that:
// 1) serialized json -> object
// 2) serialized json -> map[string]interface{} -> object
// produces the same object.
func doUnrecognized(t *testing.T, jsonData string, item interface{}, expectedErr error) {
	unmarshalledObj := reflect.New(reflect.TypeOf(item).Elem()).Interface()
	err := json.Unmarshal([]byte(jsonData), unmarshalledObj)
	if (err != nil) != (expectedErr != nil) {
		t.Errorf("Unexpected error when unmarshaling to object: %v, expected: %v", err, expectedErr)
		return
	}

	unstr := make(map[string]interface{})
	err = json.Unmarshal([]byte(jsonData), &unstr)
	if err != nil {
		t.Errorf("Error when unmarshaling to unstructured: %v", err)
		return
	}
	newObj := reflect.New(reflect.TypeOf(item).Elem()).Interface()
	err = runtime.NewTestUnstructuredConverter(simpleEquality).FromUnstructured(unstr, newObj)
	if (err != nil) != (expectedErr != nil) {
		t.Errorf("Unexpected error in FromUnstructured: %v, expected: %v", err, expectedErr)
	}

	if expectedErr == nil && !reflect.DeepEqual(unmarshalledObj, newObj) {
		t.Errorf("Object changed, diff: %v", cmp.Diff(unmarshalledObj, newObj))
	}
}

func TestUnrecognized(t *testing.T) {
	testCases := []struct {
		data string
		obj  interface{}
		err  error
	}{
		{
			data: "{\"da\":[3.5,4,\"3.0\",null]}",
			obj:  &D{},
		},
		{
			data: "{\"ea\":[3.5,4,\"3.0\",null]}",
			obj:  &E{},
		},
		{
			data: "{\"ea\":[null,null,null]}",
			obj:  &E{},
		},
		{
			data: "{\"ea\":[[],[null]]}",
			obj:  &E{},
		},
		{
			data: "{\"ea\":{\"a\":[],\"b\":null}}",
			obj:  &E{},
		},
		{
			data: "{\"fa\":\"fa\",\"fb\":{\"a\":\"a\"}}",
			obj:  &F{},
		},
		{
			data: "{\"fa\":\"fa\",\"fb\":{\"a\":null}}",
			obj:  &F{},
		},
		{
			data: "{\"fc\":[null]}",
			obj:  &F{},
		},
		{
			data: "{\"fc\":[{\"aa\":123,\"ab\":\"bbb\"}]}",
			obj:  &F{},
		},
		{
			// Only unknown fields
			data: "{\"fx\":[{\"aa\":123,\"ab\":\"bbb\"}],\"fz\":123}",
			obj:  &F{},
		},
		{
			data: "{\"fc\":[{\"aa\":\"aaa\",\"ab\":\"bbb\"}]}",
			obj:  &F{},
			err:  fmt.Errorf("json: cannot unmarshal string into Go value of type int"),
		},
		{
			data: "{\"fd\":123,\"fe\":3.5}",
			obj:  &F{},
		},
		{
			data: "{\"ff\":[\"abc\"],\"fg\":[123],\"fh\":[true,false]}",
			obj:  &F{},
		},
		{
			data: "{\"fj\":\"\"}",
			obj:  &F{},
		},
		{
			// Invalid string data
			data: "{\"fa\":123}",
			obj:  &F{},
			err:  fmt.Errorf("json: cannot unmarshal number into Go value of type string"),
		},
		{
			// Invalid string data
			data: "{\"fa\":13.5}",
			obj:  &F{},
			err:  fmt.Errorf("json: cannot unmarshal number into Go value of type string"),
		},
		{
			// Invalid string data
			data: "{\"fa\":true}",
			obj:  &F{},
			err:  fmt.Errorf("json: cannot unmarshal bool into Go value of type string"),
		},
		{
			// Invalid []string data
			data: "{\"ff\":123}",
			obj:  &F{},
			err:  fmt.Errorf("json: cannot unmarshal number into Go value of type []string"),
		},
		{
			// Invalid []string data
			data: "{\"ff\":3.5}",
			obj:  &F{},
			err:  fmt.Errorf("json: cannot unmarshal number into Go value of type []string"),
		},
		{
			// Invalid []string data
			data: "{\"ff\":[123,345]}",
			obj:  &F{},
			err:  fmt.Errorf("json: cannot unmarshal number into Go value of type string"),
		},
		{
			// Invalid []int data
			data: "{\"fg\":123}",
			obj:  &F{},
			err:  fmt.Errorf("json: cannot unmarshal number into Go value of type []int"),
		},
		{
			// Invalid []int data
			data: "{\"fg\":\"abc\"}",
			obj:  &F{},
			err:  fmt.Errorf("json: cannot unmarshal string into Go value of type []int"),
		},
		{
			// Invalid []int data
			data: "{\"fg\":[\"abc\"]}",
			obj:  &F{},
			err:  fmt.Errorf("json: cannot unmarshal string into Go value of type int"),
		},
		{
			// Invalid []int data
			data: "{\"fg\":[3.5]}",
			obj:  &F{},
			err:  fmt.Errorf("json: cannot unmarshal number 3.5 into Go value of type int"),
		},
		{
			// Invalid []int data
			data: "{\"fg\":[true,false]}",
			obj:  &F{},
			err:  fmt.Errorf("json: cannot unmarshal number 3.5 into Go value of type int"),
		},
		{
			// Invalid []bool data
			data: "{\"fh\":123}",
			obj:  &F{},
			err:  fmt.Errorf("json: cannot unmarshal number into Go value of type []bool"),
		},
		{
			// Invalid []bool data
			data: "{\"fh\":\"abc\"}",
			obj:  &F{},
			err:  fmt.Errorf("json: cannot unmarshal string into Go value of type []bool"),
		},
		{
			// Invalid []bool data
			data: "{\"fh\":[\"abc\"]}",
			obj:  &F{},
			err:  fmt.Errorf("json: cannot unmarshal string into Go value of type bool"),
		},
		{
			// Invalid []bool data
			data: "{\"fh\":[3.5]}",
			obj:  &F{},
			err:  fmt.Errorf("json: cannot unmarshal number into Go value of type bool"),
		},
		{
			// Invalid []bool data
			data: "{\"fh\":[123]}",
			obj:  &F{},
			err:  fmt.Errorf("json: cannot unmarshal number into Go value of type bool"),
		},
		{
			// Invalid []float data
			data: "{\"fi\":123}",
			obj:  &F{},
			err:  fmt.Errorf("json: cannot unmarshal number into Go value of type []float32"),
		},
		{
			// Invalid []float data
			data: "{\"fi\":\"abc\"}",
			obj:  &F{},
			err:  fmt.Errorf("json: cannot unmarshal string into Go value of type []float32"),
		},
		{
			// Invalid []float data
			data: "{\"fi\":[\"abc\"]}",
			obj:  &F{},
			err:  fmt.Errorf("json: cannot unmarshal string into Go value of type float32"),
		},
		{
			// Invalid []float data
			data: "{\"fi\":[true]}",
			obj:  &F{},
			err:  fmt.Errorf("json: cannot unmarshal bool into Go value of type float32"),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.data, func(t *testing.T) {
			doUnrecognized(t, tc.data, tc.obj, tc.err)
		})
	}
}

func TestDeepCopyJSON(t *testing.T) {
	src := map[string]interface{}{
		"a": nil,
		"b": int64(123),
		"c": map[string]interface{}{
			"a": "b",
		},
		"d": []interface{}{
			int64(1), int64(2),
		},
		"e": "estr",
		"f": true,
		"g": encodingjson.Number("123"),
	}
	deepCopy := runtime.DeepCopyJSON(src)
	assert.Equal(t, src, deepCopy)
}

func TestDeepCopyToUnstructured(t *testing.T) {
	src := &F{}
	obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(src)
	require.NoError(t, err)

	uns := &unstructured.Unstructured{Object: obj}

	assert.Equal(t, uns, uns.DeepCopy())
}

func TestFloatIntConversion(t *testing.T) {
	unstr := map[string]interface{}{"fd": float64(3)}

	var obj F
	if err := runtime.NewTestUnstructuredConverter(simpleEquality).FromUnstructured(unstr, &obj); err != nil {
		t.Errorf("Unexpected error in FromUnstructured: %v", err)
	}

	data, err := json.Marshal(unstr)
	if err != nil {
		t.Fatalf("Error when marshaling unstructured: %v", err)
	}
	var unmarshalled F
	if err := json.Unmarshal(data, &unmarshalled); err != nil {
		t.Fatalf("Error when unmarshaling to object: %v", err)
	}

	if !reflect.DeepEqual(obj, unmarshalled) {
		t.Errorf("Incorrect conversion, diff: %v", cmp.Diff(obj, unmarshalled))
	}
}

func TestIntFloatConversion(t *testing.T) {
	unstr := map[string]interface{}{"ch": int64(3)}

	var obj C
	if err := runtime.NewTestUnstructuredConverter(simpleEquality).FromUnstructured(unstr, &obj); err != nil {
		t.Errorf("Unexpected error in FromUnstructured: %v", err)
	}

	data, err := json.Marshal(unstr)
	if err != nil {
		t.Fatalf("Error when marshaling unstructured: %v", err)
	}
	var unmarshalled C
	if err := json.Unmarshal(data, &unmarshalled); err != nil {
		t.Fatalf("Error when unmarshaling to object: %v", err)
	}

	if !reflect.DeepEqual(obj, unmarshalled) {
		t.Errorf("Incorrect conversion, diff: %v", cmp.Diff(obj, unmarshalled))
	}
}

func TestCustomToUnstructured(t *testing.T) {
	testcases := []struct {
		Data     string
		Expected interface{}
	}{
		{Data: `null`, Expected: nil},
		{Data: `true`, Expected: true},
		{Data: `false`, Expected: false},
		{Data: `[]`, Expected: []interface{}{}},
		{Data: `[1]`, Expected: []interface{}{int64(1)}},
		{Data: `{}`, Expected: map[string]interface{}{}},
		{Data: `{"a":1}`, Expected: map[string]interface{}{"a": int64(1)}},
		{Data: `0`, Expected: int64(0)},
		{Data: `0.0`, Expected: float64(0)},
	}

	for _, tc := range testcases {
		tc := tc
		t.Run(tc.Data, func(t *testing.T) {
			t.Parallel()
			result, err := runtime.NewTestUnstructuredConverter(simpleEquality).ToUnstructured(&G{
				CustomValue1:   CustomValue{data: []byte(tc.Data)},
				CustomValue2:   &CustomValue{data: []byte(tc.Data)},
				CustomPointer1: CustomPointer{data: []byte(tc.Data)},
				CustomPointer2: &CustomPointer{data: []byte(tc.Data)},
			})
			require.NoError(t, err)
			for field, fieldResult := range result {
				assert.Equal(t, tc.Expected, fieldResult, field)
			}
		})
	}
}

func TestCustomToUnstructuredTopLevel(t *testing.T) {
	// Only objects are supported at the top level
	topLevelCases := []interface{}{
		&CustomValue{data: []byte(`{"a":1}`)},
		&CustomPointer{data: []byte(`{"a":1}`)},
	}
	expected := map[string]interface{}{"a": int64(1)}
	for i, obj := range topLevelCases {
		obj := obj
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			t.Parallel()
			result, err := runtime.NewTestUnstructuredConverter(simpleEquality).ToUnstructured(obj)
			require.NoError(t, err)
			assert.Equal(t, expected, result)
		})
	}
}
