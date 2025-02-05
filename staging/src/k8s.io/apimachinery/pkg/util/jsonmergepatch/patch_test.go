/*
Copyright 2017 The Kubernetes Authors.

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

package jsonmergepatch

import (
	"fmt"
	"reflect"
	"testing"

	jsonpatch "github.com/evanphx/json-patch/v5"
	"k8s.io/apimachinery/pkg/util/dump"
	"k8s.io/apimachinery/pkg/util/json"
	"sigs.k8s.io/yaml"
)

type FilterNullTestCases struct {
	TestCases []FilterNullTestCase
}

type FilterNullTestCase struct {
	Description         string
	OriginalObj         map[string]interface{}
	ExpectedWithNull    map[string]interface{}
	ExpectedWithoutNull map[string]interface{}
}

var filterNullTestCaseData = []byte(`
testCases:
  - description: nil original
    originalObj: {}
    expectedWithNull: {}
    expectedWithoutNull: {}
  - description: simple map
    originalObj:
      nilKey: null
      nonNilKey: foo
    expectedWithNull:
      nilKey: null
    expectedWithoutNull:
      nonNilKey: foo
  - description: simple map with all nil values
    originalObj:
      nilKey1: null
      nilKey2: null
    expectedWithNull:
      nilKey1: null
      nilKey2: null
    expectedWithoutNull: {}
  - description: simple map with all non-nil values
    originalObj:
      nonNilKey1: foo
      nonNilKey2: bar
    expectedWithNull: {}
    expectedWithoutNull:
      nonNilKey1: foo
      nonNilKey2: bar
  - description: nested map
    originalObj:
      mapKey:
        nilKey: null
        nonNilKey: foo
    expectedWithNull:
      mapKey:
        nilKey: null
    expectedWithoutNull:
      mapKey:
        nonNilKey: foo
  - description: nested map that all subkeys are nil
    originalObj:
      mapKey:
        nilKey1: null
        nilKey2: null
    expectedWithNull:
      mapKey:
        nilKey1: null
        nilKey2: null
    expectedWithoutNull: {}
  - description: nested map that all subkeys are non-nil
    originalObj:
      mapKey:
        nonNilKey1: foo
        nonNilKey2: bar
    expectedWithNull: {}
    expectedWithoutNull:
      mapKey:
        nonNilKey1: foo
        nonNilKey2: bar
  - description: explicitly empty map as value
    originalObj:
      mapKey: {}
    expectedWithNull: {}
    expectedWithoutNull:
      mapKey: {}
  - description: explicitly empty nested map
    originalObj:
      mapKey:
        nonNilKey: {}
    expectedWithNull: {}
    expectedWithoutNull:
      mapKey:
        nonNilKey: {}
  - description: multiple expliclty empty nested maps
    originalObj:
      mapKey:
        nonNilKey1: {}
        nonNilKey2: {}
    expectedWithNull: {}
    expectedWithoutNull:
      mapKey:
        nonNilKey1: {}
        nonNilKey2: {}
  - description: nested map with non-null value as empty map
    originalObj:
      mapKey:
        nonNilKey: {}
        nilKey: null
    expectedWithNull:
      mapKey:
        nilKey: null
    expectedWithoutNull:
      mapKey:
        nonNilKey: {}
  - description: empty list
    originalObj:
      listKey: []
    expectedWithNull: {}
    expectedWithoutNull:
      listKey: []
  - description: list of primitives
    originalObj:
      listKey:
      - 1
      - 2
    expectedWithNull: {}
    expectedWithoutNull:
      listKey:
      - 1
      - 2
  - description: list of maps
    originalObj:
      listKey:
      - k1: v1
      - k2: null
      - k3: v3
        k4: null
    expectedWithNull: {}
    expectedWithoutNull:
      listKey:
      - k1: v1
      - k2: null
      - k3: v3
        k4: null
  - description: list of different types
    originalObj:
      listKey:
      - k1: v1
      - k2: null
      - v3
    expectedWithNull: {}
    expectedWithoutNull:
      listKey:
      - k1: v1
      - k2: null
      - v3
`)

func TestKeepOrDeleteNullInObj(t *testing.T) {
	tc := FilterNullTestCases{}
	err := yaml.Unmarshal(filterNullTestCaseData, &tc)
	if err != nil {
		t.Fatalf("can't unmarshal test cases: %s\n", err)
	}

	for _, test := range tc.TestCases {
		resultWithNull, err := keepOrDeleteNullInObj(test.OriginalObj, true)
		if err != nil {
			t.Errorf("Failed in test case %q when trying to keep null values: %s", test.Description, err)
		}
		if !reflect.DeepEqual(test.ExpectedWithNull, resultWithNull) {
			t.Errorf("Failed in test case %q when trying to keep null values:\nexpected expectedWithNull:\n%+v\nbut got:\n%+v\n", test.Description, test.ExpectedWithNull, resultWithNull)
		}

		resultWithoutNull, err := keepOrDeleteNullInObj(test.OriginalObj, false)
		if err != nil {
			t.Errorf("Failed in test case %q when trying to keep non-null values: %s", test.Description, err)
		}
		if !reflect.DeepEqual(test.ExpectedWithoutNull, resultWithoutNull) {
			t.Errorf("Failed in test case %q when trying to keep non-null values:\n expected expectedWithoutNull:\n%+v\nbut got:\n%+v\n", test.Description, test.ExpectedWithoutNull, resultWithoutNull)
		}
	}
}

type JSONMergePatchTestCases struct {
	TestCases []JSONMergePatchTestCase
}

type JSONMergePatchTestCase struct {
	Description string
	JSONMergePatchTestCaseData
}

type JSONMergePatchTestCaseData struct {
	// Original is the original object (last-applied config in annotation)
	Original map[string]interface{}
	// Modified is the modified object (new config we want)
	Modified map[string]interface{}
	// Current is the current object (live config in the server)
	Current map[string]interface{}
	// ThreeWay is the expected three-way merge patch
	ThreeWay map[string]interface{}
	// Result is the expected object after applying the three-way patch on current object.
	Result map[string]interface{}
}

var createJSONMergePatchTestCaseData = []byte(`
testCases:
  - description: nil original
    modified:
      name: 1
      value: 1
    current:
      name: 1
      other: a
    threeWay:
      value: 1
    result:
      name: 1
      value: 1
      other: a
  - description: nil patch
    original:
      name: 1
    modified:
      name: 1
    current:
      name: 1
    threeWay:
      {}
    result:
      name: 1
  - description: add field to map
    original:
      name: 1
    modified:
      name: 1
      value: 1
    current:
      name: 1
      other: a
    threeWay:
      value: 1
    result:
      name: 1
      value: 1
      other: a
  - description: add field to map with conflict
    original:
      name: 1
    modified:
      name: 1
      value: 1
    current:
      name: a
      other: a
    threeWay:
      name: 1
      value: 1
    result:
      name: 1
      value: 1
      other: a
  - description: add field and delete field from map
    original:
      name: 1
    modified:
      value: 1
    current:
      name: 1
      other: a
    threeWay:
      name: null
      value: 1
    result:
      value: 1
      other: a
  - description: add field and delete field from map with conflict
    original:
      name: 1
    modified:
      value: 1
    current:
      name: a
      other: a
    threeWay:
      name: null
      value: 1
    result:
      value: 1
      other: a
  - description: delete field from nested map
    original:
      simpleMap:
        key1: 1
        key2: 1
    modified:
      simpleMap:
        key1: 1
    current:
      simpleMap:
        key1: 1
        key2: 1
        other: a
    threeWay:
      simpleMap:
        key2: null
    result:
      simpleMap:
        key1: 1
        other: a
  - description: delete field from nested map with conflict
    original:
      simpleMap:
        key1: 1
        key2: 1
    modified:
      simpleMap:
        key1: 1
    current:
      simpleMap:
        key1: a
        key2: 1
        other: a
    threeWay:
      simpleMap:
        key1: 1
        key2: null
    result:
      simpleMap:
        key1: 1
        other: a
  - description: delete all fields from map
    original:
      name: 1
      value: 1
    modified: {}
    current:
      name: 1
      value: 1
      other: a
    threeWay:
      name: null
      value: null
    result:
      other: a
  - description: delete all fields from map with conflict
    original:
      name: 1
      value: 1
    modified: {}
    current:
      name: 1
      value: a
      other: a
    threeWay:
      name: null
      value: null
    result:
      other: a
  - description: add field and delete all fields from map
    original:
      name: 1
      value: 1
    modified:
      other: a
    current:
      name: 1
      value: 1
      other: a
    threeWay:
      name: null
      value: null
    result:
      other: a
  - description: add field and delete all fields from map with conflict
    original:
      name: 1
      value: 1
    modified:
      other: a
    current:
      name: 1
      value: 1
      other: b
    threeWay:
      name: null
      value: null
      other: a
    result:
      other: a
  - description: replace list of scalars
    original:
      intList:
        - 1
        - 2
    modified:
      intList:
        - 2
        - 3
    current:
      intList:
        - 1
        - 2
    threeWay:
      intList:
        - 2
        - 3
    result:
      intList:
        - 2
        - 3
  - description: replace list of scalars with conflict
    original:
      intList:
        - 1
        - 2
    modified:
      intList:
        - 2
        - 3
    current:
      intList:
        - 1
        - 4
    threeWay:
      intList:
        - 2
        - 3
    result:
      intList:
        - 2
        - 3
  - description: patch with different scalar type
    original:
      foo: 1
    modified:
      foo: true
    current:
      foo: 1
      bar: 2
    threeWay:
      foo: true
    result:
      foo: true
      bar: 2
  - description: patch from scalar to list
    original:
      foo: 0
    modified:
      foo:
      - 1
      - 2
    current:
      foo: 0
      bar: 2
    threeWay:
      foo:
      - 1
      - 2
    result:
      foo:
      - 1
      - 2
      bar: 2
  - description: patch from list to scalar
    original:
      foo:
      - 1
      - 2
    modified:
      foo: 0
    current:
      foo:
      - 1
      - 2
      bar: 2
    threeWay:
      foo: 0
    result:
      foo: 0
      bar: 2
  - description: patch from scalar to map
    original:
      foo: 0
    modified:
      foo:
        baz: 1
    current:
      foo: 0
      bar: 2
    threeWay:
      foo:
        baz: 1
    result:
      foo:
        baz: 1
      bar: 2
  - description: patch from map to scalar
    original:
      foo:
        baz: 1
    modified:
      foo: 0
    current:
      foo:
        baz: 1
      bar: 2
    threeWay:
      foo: 0
    result:
      foo: 0
      bar: 2
  - description: patch from map to list
    original:
      foo:
        baz: 1
    modified:
      foo:
      - 1
      - 2
    current:
      foo:
        baz: 1
      bar: 2
    threeWay:
      foo:
      - 1
      - 2
    result:
      foo:
      - 1
      - 2
      bar: 2
  - description: patch from list to map
    original:
      foo:
      - 1
      - 2
    modified:
      foo:
        baz: 0
    current:
      foo:
      - 1
      - 2
      bar: 2
    threeWay:
      foo:
        baz: 0
    result:
      foo:
        baz: 0
      bar: 2
  - description: patch with different nested types
    original:
      foo:
      - a: true
      - 2
      - false
    modified:
      foo:
      - 1
      - false
      - b: 1
    current:
      foo:
      - a: true
      - 2
      - false
      bar: 0
    threeWay:
      foo:
      - 1
      - false
      - b: 1
    result:
      foo:
      - 1
      - false
      - b: 1
      bar: 0
  - description: patch array with nil
    original:
      foo:
      - a: true
      - null
      - false
      bar: []
      drop:
      - 1
    modified:
      foo:
      - 1
      - false
      - b: 1
      bar:
      - c
      - null
      - null
      - a
      drop:
      - null
    current:
      foo:
      - a: true
      - 2
      - false
      bar:
      - c
      - null
      - null
      - a
      drop:
    threeWay:
      foo:
      - 1
      - false
      - b: 1
      drop:
      - null
    result:
      foo:
      - 1
      - false
      - b: 1
      drop:
      - null
      bar:
      - c
      - null
      - null
      - a
`)

func TestCreateThreeWayJSONMergePatch(t *testing.T) {
	tc := JSONMergePatchTestCases{}
	err := yaml.Unmarshal(createJSONMergePatchTestCaseData, &tc)
	if err != nil {
		t.Errorf("can't unmarshal test cases: %s\n", err)
		return
	}

	for _, c := range tc.TestCases {
		testThreeWayPatch(t, c)
	}
}

func testThreeWayPatch(t *testing.T, c JSONMergePatchTestCase) {
	original, modified, current, expected, result := threeWayTestCaseToJSONOrFail(t, c)
	actual, err := CreateThreeWayJSONMergePatch(original, modified, current)
	if err != nil {
		t.Fatalf("error: %s", err)
	}
	testPatchCreation(t, expected, actual, c.Description)
	testPatchApplication(t, current, actual, result, c.Description)
}

func testPatchCreation(t *testing.T, expected, actual []byte, description string) {
	if !reflect.DeepEqual(actual, expected) {
		t.Errorf("error in test case: %s\nexpected patch:\n%s\ngot:\n%s\n",
			description, jsonToYAMLOrError(expected), jsonToYAMLOrError(actual))
		return
	}
}

func testPatchApplication(t *testing.T, original, patch, expected []byte, description string) {
	result, err := jsonpatch.MergePatch(original, patch)
	if err != nil {
		t.Errorf("error: %s\nin test case: %s\ncannot apply patch:\n%s\nto original:\n%s\n",
			err, description, jsonToYAMLOrError(patch), jsonToYAMLOrError(original))
		return
	}

	if !reflect.DeepEqual(result, expected) {
		format := "error in test case: %s\npatch application failed:\noriginal:\n%s\npatch:\n%s\nexpected:\n%s\ngot:\n%s\n"
		t.Errorf(format, description,
			jsonToYAMLOrError(original), jsonToYAMLOrError(patch),
			jsonToYAMLOrError(expected), jsonToYAMLOrError(result))
		return
	}
}

func threeWayTestCaseToJSONOrFail(t *testing.T, c JSONMergePatchTestCase) ([]byte, []byte, []byte, []byte, []byte) {
	return testObjectToJSONOrFail(t, c.Original),
		testObjectToJSONOrFail(t, c.Modified),
		testObjectToJSONOrFail(t, c.Current),
		testObjectToJSONOrFail(t, c.ThreeWay),
		testObjectToJSONOrFail(t, c.Result)
}

func testObjectToJSONOrFail(t *testing.T, o map[string]interface{}) []byte {
	if o == nil {
		return nil
	}
	j, err := toJSON(o)
	if err != nil {
		t.Error(err)
	}
	return j
}

func jsonToYAMLOrError(j []byte) string {
	y, err := jsonToYAML(j)
	if err != nil {
		return err.Error()
	}
	return string(y)
}

func toJSON(v interface{}) ([]byte, error) {
	j, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("json marshal failed: %v\n%v\n", err, dump.Pretty(v))
	}
	return j, nil
}

func jsonToYAML(j []byte) ([]byte, error) {
	y, err := yaml.JSONToYAML(j)
	if err != nil {
		return nil, fmt.Errorf("json to yaml failed: %v\n%v\n", err, j)
	}
	return y, nil
}
