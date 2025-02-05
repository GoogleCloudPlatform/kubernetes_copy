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

package strategicpatch

import (
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/dump"
	"k8s.io/apimachinery/pkg/util/json"
	"k8s.io/apimachinery/pkg/util/mergepatch"
	"k8s.io/apimachinery/pkg/util/sets"
	sptest "k8s.io/apimachinery/pkg/util/strategicpatch/testing"
)

var (
	fakeMergeItemSchema     = sptest.Fake{Path: filepath.Join("testdata", "swagger-merge-item.json")}
	fakePrecisionItemSchema = sptest.Fake{Path: filepath.Join("testdata", "swagger-precision-item.json")}

	fakeMergeItemV3Schema     = sptest.OpenAPIV3Getter{Path: filepath.Join("testdata", "swagger-merge-item-v3.json")}
	fakePrecisionItemV3Schema = sptest.OpenAPIV3Getter{Path: filepath.Join("testdata", "swagger-precision-item-v3.json")}
)

type SortMergeListTestCases struct {
	TestCases []SortMergeListTestCase
}

type SortMergeListTestCase struct {
	Description string
	Original    map[string]interface{}
	Sorted      map[string]interface{}
}

type StrategicMergePatchTestCases struct {
	TestCases []StrategicMergePatchTestCase
}

type StrategicMergePatchTestCase struct {
	Description string
	StrategicMergePatchTestCaseData
}

type StrategicMergePatchRawTestCase struct {
	Description string
	StrategicMergePatchRawTestCaseData
}

type StrategicMergePatchBackwardCompatibilityTestCase struct {
	Description string
	Original    []byte
	Modified    []byte
	Current     []byte
	Generation  []StrategicMergePatchGenerationRawTestCaseData
	Application []StrategicMergePatchApplicationRawTestCaseData
}

type StrategicMergePatchTestCaseData struct {
	// Original is the original object (last-applied config in annotation)
	Original map[string]interface{}
	// Modified is the modified object (new config we want)
	Modified map[string]interface{}
	// Current is the current object (live config in the server)
	Current map[string]interface{}
	// TwoWay is the expected two-way merge patch diff between original and modified
	TwoWay map[string]interface{}
	// ThreeWay is the expected three-way merge patch
	ThreeWay map[string]interface{}
	// Result is the expected object after applying the three-way patch on current object.
	Result map[string]interface{}
	// TwoWayResult is the expected object after applying the two-way patch on current object.
	// If nil, Modified is used.
	TwoWayResult map[string]interface{}
}

// The meaning of each field is the same as StrategicMergePatchTestCaseData's.
// The difference is that all the fields in StrategicMergePatchRawTestCaseData are json-encoded data.
type StrategicMergePatchRawTestCaseData struct {
	Original      []byte
	Modified      []byte
	Current       []byte
	TwoWay        []byte
	ThreeWay      []byte
	Result        []byte
	TwoWayResult  []byte
	ExpectedError string
}

type StrategicMergePatchGenerationRawTestCaseData struct {
	Description                    string
	DuplicateMergeKeyValuesEnabled bool
	TwoWay                         []byte
	ThreeWay                       []byte
}

type StrategicMergePatchApplicationRawTestCaseData struct {
	Description                    string
	DuplicateMergeKeyValuesEnabled bool
	TwoWay                         []byte
	TwoWayResult                   []byte
	ThreeWay                       []byte
	ThreeWayResult                 []byte
}

type MergeItem struct {
	Name                  string               `json:"name,omitempty"`
	Value                 string               `json:"value,omitempty"`
	Other                 string               `json:"other,omitempty"`
	MergingList           []MergeItem          `json:"mergingList,omitempty" patchStrategy:"merge" patchMergeKey:"name"`
	NonMergingList        []MergeItem          `json:"nonMergingList,omitempty"`
	MergingIntList        []int                `json:"mergingIntList,omitempty" patchStrategy:"merge"`
	NonMergingIntList     []int                `json:"nonMergingIntList,omitempty"`
	MergeItemPtr          *MergeItem           `json:"mergeItemPtr,omitempty" patchStrategy:"merge" patchMergeKey:"name"`
	SimpleMap             map[string]string    `json:"simpleMap,omitempty"`
	ReplacingItem         runtime.RawExtension `json:"replacingItem,omitempty" patchStrategy:"replace"`
	RetainKeysMap         RetainKeysMergeItem  `json:"retainKeysMap,omitempty" patchStrategy:"retainKeys"`
	RetainKeysMergingList []MergeItem          `json:"retainKeysMergingList,omitempty" patchStrategy:"merge,retainKeys" patchMergeKey:"name"`
}

type RetainKeysMergeItem struct {
	Name           string            `json:"name,omitempty"`
	Value          string            `json:"value,omitempty"`
	Other          string            `json:"other,omitempty"`
	SimpleMap      map[string]string `json:"simpleMap,omitempty"`
	MergingIntList []int             `json:"mergingIntList,omitempty" patchStrategy:"merge"`
	MergingList    []MergeItem       `json:"mergingList,omitempty" patchStrategy:"merge" patchMergeKey:"name"`
	NonMergingList []MergeItem       `json:"nonMergingList,omitempty"`
}

var (
	mergeItem             MergeItem
	mergeItemStructSchema = PatchMetaFromStruct{T: GetTagStructTypeOrDie(mergeItem)}
)

// These are test cases for SortMergeList, used to assert that it (recursively)
// sorts both merging and non merging lists correctly.
var sortMergeListTestCaseData = []byte(`
testCases:
  - description: sort one list of maps
    original:
      mergingList:
        - name: 1
        - name: 3
        - name: 2
    sorted:
      mergingList:
        - name: 1
        - name: 2
        - name: 3
  - description: sort lists of maps but not nested lists of maps
    original:
      mergingList:
        - name: 2
          nonMergingList:
            - name: 1
            - name: 3
            - name: 2
        - name: 1
          nonMergingList:
            - name: 2
            - name: 1
    sorted:
      mergingList:
        - name: 1
          nonMergingList:
            - name: 2
            - name: 1
        - name: 2
          nonMergingList:
            - name: 1
            - name: 3
            - name: 2
  - description: sort lists of maps and nested lists of maps
    original:
      mergingList:
        - name: 2
          mergingList:
            - name: 1
            - name: 3
            - name: 2
        - name: 1
          mergingList:
            - name: 2
            - name: 1
    sorted:
      mergingList:
        - name: 1
          mergingList:
            - name: 1
            - name: 2
        - name: 2
          mergingList:
            - name: 1
            - name: 2
            - name: 3
  - description: merging list should NOT sort when nested in non merging list
    original:
      nonMergingList:
        - name: 2
          mergingList:
            - name: 1
            - name: 3
            - name: 2
        - name: 1
          mergingList:
            - name: 2
            - name: 1
    sorted:
      nonMergingList:
        - name: 2
          mergingList:
            - name: 1
            - name: 3
            - name: 2
        - name: 1
          mergingList:
            - name: 2
            - name: 1
  - description: sort very nested list of maps
    fieldTypes:
    original:
      mergingList:
        - mergingList:
            - mergingList:
                - name: 2
                - name: 1
    sorted:
      mergingList:
        - mergingList:
            - mergingList:
                - name: 1
                - name: 2
  - description: sort nested lists of ints
    original:
      mergingList:
        - name: 2
          mergingIntList:
            - 1
            - 3
            - 2
        - name: 1
          mergingIntList:
            - 2
            - 1
    sorted:
      mergingList:
        - name: 1
          mergingIntList:
            - 1
            - 2
        - name: 2
          mergingIntList:
            - 1
            - 2
            - 3
  - description: sort nested pointers of ints
    original:
      mergeItemPtr:
        - name: 2
          mergingIntList:
            - 1
            - 3
            - 2
        - name: 1
          mergingIntList:
            - 2
            - 1
    sorted:
      mergeItemPtr:
        - name: 1
          mergingIntList:
            - 1
            - 2
        - name: 2
          mergingIntList:
            - 1
            - 2
            - 3
  - description: sort merging list by pointer
    original:
      mergeItemPtr:
        - name: 1
        - name: 3
        - name: 2
    sorted:
      mergeItemPtr:
        - name: 1
        - name: 2
        - name: 3
`)

func TestSortMergeLists(t *testing.T) {
	mergeItemOpenapiSchema := PatchMetaFromOpenAPI{
		Schema: sptest.GetSchemaOrDie(&fakeMergeItemSchema, "mergeItem"),
	}
	mergeItemOpenapiV3Schema := PatchMetaFromOpenAPIV3{
		SchemaList: fakeMergeItemV3Schema.SchemaOrDie().Components.Schemas,
		Schema:     fakeMergeItemV3Schema.SchemaOrDie().Components.Schemas["mergeItem"],
	}
	schemas := []LookupPatchMeta{
		mergeItemStructSchema,
		mergeItemOpenapiSchema,
		mergeItemOpenapiV3Schema,
	}

	tc := SortMergeListTestCases{}
	err := yaml.Unmarshal(sortMergeListTestCaseData, &tc)
	if err != nil {
		t.Errorf("can't unmarshal test cases: %s\n", err)
		return
	}

	for _, schema := range schemas {
		for _, c := range tc.TestCases {
			temp := testObjectToJSONOrFail(t, c.Original)
			got := sortJsonOrFail(t, temp, c.Description, schema)
			expected := testObjectToJSONOrFail(t, c.Sorted)
			if !reflect.DeepEqual(got, expected) {
				t.Errorf("using %s error in test case: %s\ncannot sort object:\n%s\nexpected:\n%s\ngot:\n%s\n",
					getSchemaType(schema), c.Description, mergepatch.ToYAMLOrError(c.Original), mergepatch.ToYAMLOrError(c.Sorted), jsonToYAMLOrError(got))
			}
		}
	}
}

// These are test cases for StrategicMergePatch that cannot be generated using
// CreateTwoWayMergePatch because it may be one of the following cases:
// - not use the replace directive.
// - generate duplicate integers for a merging list patch.
// - generate empty merging lists.
// - use patch format from an old client.
var customStrategicMergePatchTestCaseData = []byte(`
testCases:
  - description: unique scalars when merging lists
    original:
      mergingIntList:
        - 1
        - 2
    twoWay:
      mergingIntList:
        - 2
        - 3
    modified:
      mergingIntList:
        - 1
        - 2
        - 3
  - description: delete map from nested map
    original:
      simpleMap:
        key1: 1
        key2: 1
    twoWay:
      simpleMap:
        $patch: delete
    modified:
      simpleMap:
        {}
  - description: delete all items from merging list
    original:
      mergingList:
        - name: 1
        - name: 2
    twoWay:
      mergingList:
        - $patch: replace
    modified:
      mergingList: []
  - description: merge empty merging lists
    original:
      mergingList: []
    twoWay:
      mergingList: []
    modified:
      mergingList: []
  - description: delete all keys from map
    original:
      name: 1
      value: 1
    twoWay:
      $patch: replace
    modified: {}
  - description: add key and delete all keys from map
    original:
      name: 1
      value: 1
    twoWay:
      other: a
      $patch: replace
    modified:
      other: a
  - description: delete all duplicate entries in a merging list
    original:
      mergingList:
        - name: 1
        - name: 1
        - name: 2
          value: a
        - name: 3
        - name: 3
    twoWay:
      mergingList:
        - name: 1
          $patch: delete
        - name: 3
          $patch: delete
    modified:
      mergingList:
        - name: 2
          value: a
  - description: retainKeys map can add a field when no retainKeys directive present
    original:
      retainKeysMap:
        name: foo
    twoWay:
      retainKeysMap:
        value: bar
    modified:
      retainKeysMap:
        name: foo
        value: bar
  - description: retainKeys map can change a field when no retainKeys directive present
    original:
      retainKeysMap:
        name: foo
        value: a
    twoWay:
      retainKeysMap:
        value: b
    modified:
      retainKeysMap:
        name: foo
        value: b
  - description: retainKeys map can delete a field when no retainKeys directive present
    original:
      retainKeysMap:
        name: foo
        value: a
    twoWay:
      retainKeysMap:
        value: null
    modified:
      retainKeysMap:
        name: foo
  - description: retainKeys map merge an empty map
    original:
      retainKeysMap:
        name: foo
        value: a
    twoWay:
      retainKeysMap: {}
    modified:
      retainKeysMap:
        name: foo
        value: a
  - description: retainKeys list can add a field when no retainKeys directive present
    original:
      retainKeysMergingList:
      - name: bar
      - name: foo
    twoWay:
      retainKeysMergingList:
      - name: foo
        value: a
    modified:
      retainKeysMergingList:
      - name: bar
      - name: foo
        value: a
  - description: retainKeys list can change a field when no retainKeys directive present
    original:
      retainKeysMergingList:
      - name: bar
      - name: foo
        value: a
    twoWay:
      retainKeysMergingList:
      - name: foo
        value: b
    modified:
      retainKeysMergingList:
      - name: bar
      - name: foo
        value: b
  - description: retainKeys list can delete a field when no retainKeys directive present
    original:
      retainKeysMergingList:
      - name: bar
      - name: foo
        value: a
    twoWay:
      retainKeysMergingList:
      - name: foo
        value: null
    modified:
      retainKeysMergingList:
      - name: bar
      - name: foo
  - description: preserve the order from the patch in a merging list
    original:
      mergingList:
        - name: 1
        - name: 2
          value: b
        - name: 3
    twoWay:
      mergingList:
        - name: 3
          value: c
        - name: 1
          value: a
        - name: 2
          other: x
    modified:
      mergingList:
        - name: 3
          value: c
        - name: 1
          value: a
        - name: 2
          value: b
          other: x
  - description: preserve the order from the patch in a merging list 2
    original:
      mergingList:
        - name: 1
        - name: 2
          value: b
        - name: 3
    twoWay:
      mergingList:
        - name: 3
          value: c
        - name: 1
          value: a
    modified:
      mergingList:
        - name: 2
          value: b
        - name: 3
          value: c
        - name: 1
          value: a
  - description: preserve the order from the patch in a merging int list
    original:
      mergingIntList:
        - 1
        - 2
        - 3
    twoWay:
      mergingIntList:
        - 3
        - 1
        - 2
    modified:
      mergingIntList:
        - 3
        - 1
        - 2
  - description: preserve the order from the patch in a merging int list
    original:
      mergingIntList:
        - 1
        - 2
        - 3
    twoWay:
      mergingIntList:
        - 3
        - 1
    modified:
      mergingIntList:
        - 2
        - 3
        - 1
`)

var customStrategicMergePatchRawTestCases = []StrategicMergePatchRawTestCase{
	{
		Description: "$setElementOrder contains item that is not present in the list to be merged",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
mergingList:
  - name: 1
  - name: 3
`),
			TwoWay: []byte(`
$setElementOrder/mergingList:
  - name: 3
  - name: 2
  - name: 1
mergingList:
  - name: 3
    value: 3
  - name: 1
    value: 1
`),
			Modified: []byte(`
mergingList:
  - name: 3
    value: 3
  - name: 1
    value: 1
`),
		},
	},
	{
		Description: "$setElementOrder contains item that is not present in the int list to be merged",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
mergingIntList:
  - 1
  - 3
`),
			TwoWay: []byte(`
$setElementOrder/mergingIntList:
  - 3
  - 2
  - 1
`),
			Modified: []byte(`
mergingIntList:
  - 3
  - 1
`),
		},
	},
	{
		Description: "should check if order in $setElementOrder and patch list match",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
mergingList:
  - name: 1
  - name: 3
  - name: 2
`),
			TwoWay: []byte(`
$setElementOrder/mergingList:
  - name: 1
  - name: 2
  - name: 3
mergingList:
  - name: 3
    value: 3
  - name: 1
    value: 1
`),
			ExpectedError: "doesn't match",
		},
	},
	{
		Description: "$setElementOrder contains item that is not present in the int list to be merged",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
mergingIntList:
  - 1
  - 3
  - 2
`),
			TwoWay: []byte(`
$setElementOrder/mergingIntList:
  - 1
  - 2
  - 3
mergingIntList:
  - 3
  - 1
`),
			ExpectedError: "doesn't match",
		},
	},
	{
		Description: "missing merge key should error out",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
mergingList:
  - name: 1
    value: a
`),
			TwoWay: []byte(`
mergingList:
  - value: b
`),
			ExpectedError: "does not contain declared merge key",
		},
	},
	{
		Description: "$deleteFromPrimitiveList of nonexistent item in primitive list should not add the item to the list",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
mergingIntList:
  - 1
  - 2
`),
			TwoWay: []byte(`
$deleteFromPrimitiveList/mergingIntList:
  - 3
`),
			Modified: []byte(`
mergingIntList:
  - 1
  - 2
`),
		},
	},
	{
		Description: "$deleteFromPrimitiveList on empty primitive list should not add the item to the list",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
mergingIntList:
`),
			TwoWay: []byte(`
$deleteFromPrimitiveList/mergingIntList:
  - 3
`),
			Modified: []byte(`
mergingIntList:
`),
		},
	},
	{
		Description: "$deleteFromPrimitiveList on nonexistent primitive list should not add the primitive list",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
foo:
  - bar
`),
			TwoWay: []byte(`
$deleteFromPrimitiveList/mergingIntList:
  - 3
`),
			Modified: []byte(`
foo:
  - bar
`),
		},
	},
	{
		Description: "$deleteFromPrimitiveList should delete item from a list with merge patch strategy",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
mergingIntList:
  - 1
  - 2
  - 3
`),
			TwoWay: []byte(`
$deleteFromPrimitiveList/mergingIntList:
  - 2
`),
			Modified: []byte(`
mergingIntList:
  - 1
  - 3
`),
		},
	},
	{
		Description: "$deleteFromPrimitiveList should delete item from a list without merge patch strategy",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
nonMergingIntList:
  - 1
  - 2
  - 3
`),
			TwoWay: []byte(`
$deleteFromPrimitiveList/nonMergingIntList:
  - 2
`),
			Modified: []byte(`
nonMergingIntList:
  - 1
  - 3
`),
		},
	},
}

func TestCustomStrategicMergePatch(t *testing.T) {
	mergeItemOpenapiSchema := PatchMetaFromOpenAPI{
		Schema: sptest.GetSchemaOrDie(&fakeMergeItemSchema, "mergeItem"),
	}
	mergeItemOpenapiV3Schema := PatchMetaFromOpenAPIV3{
		SchemaList: fakeMergeItemV3Schema.SchemaOrDie().Components.Schemas,
		Schema:     fakeMergeItemV3Schema.SchemaOrDie().Components.Schemas["mergeItem"],
	}
	schemas := []LookupPatchMeta{
		mergeItemStructSchema,
		mergeItemOpenapiSchema,
		mergeItemOpenapiV3Schema,
	}

	tc := StrategicMergePatchTestCases{}
	err := yaml.Unmarshal(customStrategicMergePatchTestCaseData, &tc)
	if err != nil {
		t.Errorf("can't unmarshal test cases: %v\n", err)
		return
	}

	for _, c := range tc.TestCases {
		t.Run(c.Description, func(t *testing.T) {
			for _, schema := range schemas {
				t.Run(schema.Name(), func(t *testing.T) {
					original, expectedTwoWayPatch, _, expectedResult := twoWayTestCaseToJSONOrFail(t, c, schema)
					testPatchApplication(t, original, expectedTwoWayPatch, expectedResult, c.Description, "", schema)
				})

				for _, c := range customStrategicMergePatchRawTestCases {
					original, expectedTwoWayPatch, _, expectedResult := twoWayRawTestCaseToJSONOrFail(t, c)
					testPatchApplication(t, original, expectedTwoWayPatch, expectedResult, c.Description, c.ExpectedError, schema)
				}
			}
		})
	}
}

var featureGateTestCases = []StrategicMergePatchBackwardCompatibilityTestCase{
	{
		Description: "removing element from a merging list with duplicate",
		Original: []byte(`
mergingList:
- name: 1
- name: 2
  value: dup1
- name: 3
- name: 2
  value: dup2
`),
		Current: []byte(`
mergingList:
- name: 1
- name: 2
  value: dup1
- name: 3
- name: 2
  value: dup2
`),
		Modified: []byte(`
mergingList:
- name: 1
- name: 2
  value: dup2
- name: 3
`),
		Generation: []StrategicMergePatchGenerationRawTestCaseData{
			{
				Description:                    "new client",
				DuplicateMergeKeyValuesEnabled: true,
				TwoWay: []byte(`
$setElementOrder/mergingList:
- name: 1
- name: 2
- name: 3
mergingList:
- name: 2
  value: dup2
- $patch: delete
  name: 2
`),
				ThreeWay: []byte(`
$setElementOrder/mergingList:
- name: 1
- name: 2
- name: 3
mergingList:
- name: 2
  value: dup2
- $patch: delete
  name: 2
`),
			},
			{
				Description:                    "old client",
				DuplicateMergeKeyValuesEnabled: false,
				TwoWay: []byte(`
$setElementOrder/mergingList:
- name: 1
- name: 2
- name: 3
mergingList:
- $patch: delete
  name: 2
`),
				ThreeWay: []byte(`
$setElementOrder/mergingList:
- name: 1
- name: 2
- name: 3
mergingList:
- $patch: delete
  name: 2
`),
			},
		},
		Application: []StrategicMergePatchApplicationRawTestCaseData{
			{
				Description:                    "new client new server",
				DuplicateMergeKeyValuesEnabled: true,
				TwoWay: []byte(`
$setElementOrder/mergingList:
- name: 1
- name: 2
- name: 3
mergingList:
- name: 2
  value: dup2
- $patch: delete
  name: 2
`),
				ThreeWay: []byte(`
$setElementOrder/mergingList:
- name: 1
- name: 2
- name: 3
mergingList:
- name: 2
  value: dup2
- $patch: delete
  name: 2
`),
				TwoWayResult: []byte(`
mergingList:
- name: 1
- name: 2
  value: dup2
- name: 3
`),
				ThreeWayResult: []byte(`
mergingList:
- name: 1
- name: 2
  value: dup2
- name: 3
`),
			},
			{
				Description:                    "old client new server",
				DuplicateMergeKeyValuesEnabled: true,
				TwoWay: []byte(`
$setElementOrder/mergingList:
- name: 1
- name: 2
- name: 3
mergingList:
- $patch: delete
  name: 2
`),
				ThreeWay: []byte(`
$setElementOrder/mergingList:
- name: 1
- name: 2
- name: 3
mergingList:
- $patch: delete
  name: 2
`),
				TwoWayResult: []byte(`
mergingList:
- name: 1
- name: 3
`),
				ThreeWayResult: []byte(`
mergingList:
- name: 1
- name: 3
`),
			},
			{
				Description:                    "new client old server",
				DuplicateMergeKeyValuesEnabled: false,
				TwoWay: []byte(`
$setElementOrder/mergingList:
- name: 1
- name: 2
- name: 3
mergingList:
- name: 2
  value: dup2
- $patch: delete
  name: 2
`),
				ThreeWay: []byte(`
$setElementOrder/mergingList:
- name: 1
- name: 2
- name: 3
mergingList:
- name: 2
  value: dup2
- $patch: delete
  name: 2
`),
				TwoWayResult: []byte(`
mergingList:
- name: 1
- name: 2
  value: dup2
- name: 3
`),
				ThreeWayResult: []byte(`
mergingList:
- name: 1
- name: 2
  value: dup2
- name: 3
`),
			},
			{
				Description:                    "old client old server",
				DuplicateMergeKeyValuesEnabled: false,
				TwoWay: []byte(`
$setElementOrder/mergingList:
- name: 1
- name: 2
- name: 3
mergingList:
- $patch: delete
  name: 2
`),
				ThreeWay: []byte(`
$setElementOrder/mergingList:
- name: 1
- name: 2
- name: 3
mergingList:
- $patch: delete
  name: 2
`),
				TwoWayResult: []byte(`
mergingList:
- name: 1
- name: 3
`),
				ThreeWayResult: []byte(`
mergingList:
- name: 1
- name: 3
`),
			},
		},
	},
	{
		Description: "removing one duplicate element and updating another in merging list",
		Original: []byte(`
mergingList:
- name: 1
- name: 2
  value: dup1
- name: 3
- name: 2
  value: dup2
`),
		Current: []byte(`
mergingList:
- name: 1
- name: 2
  value: dup1
- name: 3
- name: 2
  value: dup2
- name: 4
`),
		Modified: []byte(`
mergingList:
- name: 1
- name: 2
  value: newValue
- name: 3
`),
		Generation: []StrategicMergePatchGenerationRawTestCaseData{
			{
				Description:                    "new client",
				DuplicateMergeKeyValuesEnabled: true,
				TwoWay: []byte(`
$setElementOrder/mergingList:
- name: 1
- name: 2
- name: 3
mergingList:
- name: 2
  value: newValue
- $patch: delete
  name: 2
`),
				ThreeWay: []byte(`
$setElementOrder/mergingList:
- name: 1
- name: 2
- name: 3
mergingList:
- name: 2
  value: newValue
- $patch: delete
  name: 2
`),
			},
			{
				Description:                    "old client",
				DuplicateMergeKeyValuesEnabled: false,
				TwoWay: []byte(`
$setElementOrder/mergingList:
- name: 1
- name: 2
- name: 3
mergingList:
- name: 2
  value: newValue
- $patch: delete
  name: 2
`),
				ThreeWay: []byte(`
$setElementOrder/mergingList:
- name: 1
- name: 2
- name: 3
mergingList:
- $patch: delete
  name: 2
  value: newValue
`),
			},
		},
		Application: []StrategicMergePatchApplicationRawTestCaseData{
			{
				// This test case has wrong ordering in ThreeWayMerge result because of side effect in mergeSliceWithSpecialElements
				// function, same as in case 'behavior of set element order for a merging int list with duplicate'
				Description:                    "new client new server",
				DuplicateMergeKeyValuesEnabled: true,
				TwoWay: []byte(`
$setElementOrder/mergingList:
- name: 1
- name: 2
- name: 3
mergingList:
- name: 2
  value: newValue
- $patch: delete
  name: 2
`),
				ThreeWay: []byte(`
$setElementOrder/mergingList:
- name: 1
- name: 2
- name: 3
mergingList:
- name: 2
  value: newValue
- $patch: delete
  name: 2
`),
				TwoWayResult: []byte(`
mergingList:
- name: 1
- name: 2
  value: newValue
- name: 3
`),
				ThreeWayResult: []byte(`
mergingList:
- name: 1
- name: 4
- name: 2
  value: newValue
- name: 3
`),
			},
			{
				Description:                    "old client new server",
				DuplicateMergeKeyValuesEnabled: true,
				TwoWay: []byte(`
$setElementOrder/mergingList:
- name: 1
- name: 2
- name: 3
mergingList:
- name: 2
  value: newValue
- $patch: delete
  name: 2
`),
				ThreeWay: []byte(`
$setElementOrder/mergingList:
- name: 1
- name: 2
- name: 3
mergingList:
- $patch: delete
  name: 2
  value: newValue
`),
				TwoWayResult: []byte(`
mergingList:
- name: 1
- name: 2
  value: newValue
- name: 3
`),
				ThreeWayResult: []byte(`
mergingList:
- name: 1
- name: 3
- name: 4
`),
			},
			{
				// This test case has wrong ordering in ThreeWayMerge result because of side effect in mergeSliceWithSpecialElements
				// function, same as in case 'behavior of set element order for a merging int list with duplicate'
				Description:                    "new client old server",
				DuplicateMergeKeyValuesEnabled: false,
				TwoWay: []byte(`
$setElementOrder/mergingList:
- name: 1
- name: 2
- name: 3
mergingList:
- name: 2
  value: newValue
- $patch: delete
  name: 2
`),
				ThreeWay: []byte(`
$setElementOrder/mergingList:
- name: 1
- name: 2
- name: 3
mergingList:
- name: 2
  value: newValue
- $patch: delete
  name: 2
`),
				TwoWayResult: []byte(`
mergingList:
- name: 1
- name: 2
  value: newValue
- name: 3
`),
				ThreeWayResult: []byte(`
mergingList:
- name: 1
- name: 4
- name: 2
  value: newValue
- name: 3
`),
			},
			{
				Description:                    "old client old server",
				DuplicateMergeKeyValuesEnabled: false,
				TwoWay: []byte(`
$setElementOrder/mergingList:
- name: 1
- name: 2
- name: 3
mergingList:
- name: 2
  value: newValue
- $patch: delete
  name: 2
`),
				ThreeWay: []byte(`
$setElementOrder/mergingList:
- name: 1
- name: 2
- name: 3
mergingList:
- $patch: delete
  name: 2
  value: newValue
`),
				TwoWayResult: []byte(`
mergingList:
- name: 1
- name: 2
  value: newValue
- name: 3
`),
				ThreeWayResult: []byte(`
mergingList:
- name: 1
- name: 3
- name: 4
`),
			},
		},
	},
}

func TestFeatureGates(t *testing.T) {
	mergeItemOpenapiSchema := PatchMetaFromOpenAPI{
		Schema: sptest.GetSchemaOrDie(&fakeMergeItemSchema, "mergeItem"),
	}
	mergeItemOpenapiV3Schema := PatchMetaFromOpenAPIV3{
		SchemaList: fakeMergeItemV3Schema.SchemaOrDie().Components.Schemas,
		Schema:     fakeMergeItemV3Schema.SchemaOrDie().Components.Schemas["mergeItem"],
	}
	schemas := []LookupPatchMeta{
		mergeItemStructSchema,
		mergeItemOpenapiSchema,
		mergeItemOpenapiV3Schema,
	}

	for _, schema := range schemas {
		// run multiple times to exercise different map traversal orders
		for i := 0; i < 10; i++ {
			for _, c := range featureGateTestCases {
				testFeatureGatePatchCreation(t, schema, c)
				testFeatureGatePatchApplication(t, schema, c)
			}
		}
	}
}

func testFeatureGatePatchCreation(t *testing.T, schema LookupPatchMeta, c StrategicMergePatchBackwardCompatibilityTestCase) {
	original, modified := yamlToJSONOrError(t, c.Original), yamlToJSONOrError(t, c.Modified)

	for _, current := range c.Generation {
		expectedTwoWay := yamlToJSONOrError(t, current.TwoWay)
		desc := fmt.Sprintf("%s/PatchCreation/TwoWay/%s", c.Description, current.Description)
		t.Run(desc, func(t *testing.T) {
			previousSupportDuplicateMergeKeyValues := supportDuplicateMergeKeyValues
			supportDuplicateMergeKeyValues = current.DuplicateMergeKeyValuesEnabled
			defer func() { supportDuplicateMergeKeyValues = previousSupportDuplicateMergeKeyValues }()

			actualPatch, err := CreateTwoWayMergePatchUsingLookupPatchMeta(original, modified, schema)
			if err != nil {
				t.Errorf("error: %s\nin test case: %s\ncannot create two way patch:\noriginal:%s\nmodified:%s\nduplicate MergeKey values enabled:%t\n",
					err, c.Description, c.Original, c.Modified, current.DuplicateMergeKeyValuesEnabled)
				return
			}
			testPatchCreation(t, expectedTwoWay, actualPatch, c.Description)
		})

		currentManifest := yamlToJSONOrError(t, c.Current)
		expectedThreeWay := yamlToJSONOrError(t, current.ThreeWay)
		desc = fmt.Sprintf("%s/PatchCreation/ThreeWay/%s", c.Description, current.Description)
		t.Run(desc, func(t *testing.T) {
			previousSupportDuplicateMergeKeyValues := supportDuplicateMergeKeyValues
			supportDuplicateMergeKeyValues = current.DuplicateMergeKeyValuesEnabled
			defer func() { supportDuplicateMergeKeyValues = previousSupportDuplicateMergeKeyValues }()

			actual, err := CreateThreeWayMergePatch(original, modified, currentManifest, schema, false)
			if err != nil {
				t.Errorf("error: %s\nin test case: %s\ncannot create three way patch:\noriginal:%s\nmodified:%s\ncurrent:%s\nduplicate MergeKey values enabled:%t\n",
					err, c.Description, c.Original, c.Modified, c.Current, current.DuplicateMergeKeyValuesEnabled)
				return
			}
			testPatchCreation(t, expectedThreeWay, actual, c.Description)
		})
	}
}

func testFeatureGatePatchApplication(t *testing.T, schema LookupPatchMeta, c StrategicMergePatchBackwardCompatibilityTestCase) {
	original := yamlToJSONOrError(t, c.Original)

	for _, current := range c.Application {
		patch, expectedResult := yamlToJSONOrError(t, current.TwoWay), yamlToJSONOrError(t, current.TwoWayResult)
		desc := fmt.Sprintf("%s/PatchApplication/TwoWay/%s", c.Description, current.Description)
		t.Run(desc, func(t *testing.T) {
			previousSupportDuplicateMergeKeyValues := supportDuplicateMergeKeyValues
			supportDuplicateMergeKeyValues = current.DuplicateMergeKeyValuesEnabled
			defer func() { supportDuplicateMergeKeyValues = previousSupportDuplicateMergeKeyValues }()

			testPatchApplication(t, original, patch, expectedResult, c.Description, "", schema)
		})

		threeWayPatch, expectedThreeWayResult := yamlToJSONOrError(t, current.ThreeWay), yamlToJSONOrError(t, current.ThreeWayResult)
		currentManifest := yamlToJSONOrError(t, c.Current)
		desc = fmt.Sprintf("%s/PatchApplication/ThreeWay/%s", c.Description, current.Description)
		t.Run(desc, func(t *testing.T) {
			previousSupportDuplicateMergeKeyValues := supportDuplicateMergeKeyValues
			supportDuplicateMergeKeyValues = current.DuplicateMergeKeyValuesEnabled
			defer func() { supportDuplicateMergeKeyValues = previousSupportDuplicateMergeKeyValues }()

			testPatchApplication(t, currentManifest, threeWayPatch, expectedThreeWayResult, c.Description, "", schema)
		})
	}
}

// These are test cases for StrategicMergePatch, to assert that applying  a patch
// yields the correct outcome. They are also test cases for CreateTwoWayMergePatch
// and CreateThreeWayMergePatch, to assert that they both generate the correct patch
// for the given set of input documents.
var createStrategicMergePatchTestCaseData = []byte(`
testCases:
  - description: nil original
    twoWay:
      name: 1
      value: 1
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
    twoWay:
      {}
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
    twoWay:
      value: 1
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
    twoWay:
      value: 1
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
    twoWay:
      name: null
      value: 1
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
    twoWay:
      name: null
      value: 1
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
    twoWay:
      simpleMap:
        key2: null
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
    twoWay:
      simpleMap:
        key2: null
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
    twoWay:
      name: null
      value: null
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
    twoWay:
      name: null
      value: null
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
    twoWay:
      name: null
      value: null
      other: a
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
    twoWay:
      name: null
      value: null
      other: a
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
      nonMergingIntList:
        - 1
        - 2
    twoWay:
      nonMergingIntList:
        - 2
        - 3
    modified:
      nonMergingIntList:
        - 2
        - 3
    current:
      nonMergingIntList:
        - 1
        - 2
    threeWay:
      nonMergingIntList:
        - 2
        - 3
    result:
      nonMergingIntList:
        - 2
        - 3
  - description: replace list of scalars with conflict
    original:
      nonMergingIntList:
        - 1
        - 2
    twoWay:
      nonMergingIntList:
        - 2
        - 3
    modified:
      nonMergingIntList:
        - 2
        - 3
    current:
      nonMergingIntList:
        - 1
        - 4
    threeWay:
      nonMergingIntList:
        - 2
        - 3
    result:
      nonMergingIntList:
        - 2
        - 3
  - description: delete all maps from merging list
    original:
      mergingList:
        - name: 1
        - name: 2
    twoWay:
      mergingList:
        - name: 1
          $patch: delete
        - name: 2
          $patch: delete
    modified:
      mergingList: []
    current:
      mergingList:
        - name: 1
        - name: 2
    threeWay:
      mergingList:
        - name: 1
          $patch: delete
        - name: 2
          $patch: delete
    result:
      mergingList: []
  - description: delete all maps from merging list with conflict
    original:
      mergingList:
        - name: 1
        - name: 2
    twoWay:
      mergingList:
        - name: 1
          $patch: delete
        - name: 2
          $patch: delete
    modified:
      mergingList: []
    current:
      mergingList:
        - name: 1
          other: a
        - name: 2
          other: b
    threeWay:
      mergingList:
        - name: 1
          $patch: delete
        - name: 2
          $patch: delete
    result:
      mergingList: []
  - description: delete all maps from empty merging list
    original:
      mergingList:
        - name: 1
        - name: 2
    twoWay:
      mergingList:
        - name: 1
          $patch: delete
        - name: 2
          $patch: delete
    modified:
      mergingList: []
    current:
      mergingList: []
    threeWay:
      mergingList:
        - name: 1
          $patch: delete
        - name: 2
          $patch: delete
    result:
      mergingList: []
  - description: merge empty merging lists
    original:
      mergingList: []
    twoWay:
      {}
    modified:
      mergingList: []
    current:
      mergingList: []
    threeWay:
      {}
    result:
      mergingList: []
  - description: defined null values should propagate overwrite current fields (with conflict)
    original:
      name: 2
    twoWay:
      name: 1
      value: 1
      other: null
    twoWayResult:
      name: 1
      value: 1
    modified:
      name: 1
      value: 1
      other: null
    current:
      name: a
      other: a
    threeWay:
      name: 1
      value: 1
      other: null
    result:
      name: 1
      value: 1
  - description: defined null values should propagate removing original fields
    original:
      name: original-name
      value: original-value
    current:
      name: original-name
      value: original-value
      other: current-other
    modified:
      name: modified-name
      value: null
    twoWay:
      name: modified-name
      value: null
    twoWayResult:
      name: modified-name
    threeWay:
      name: modified-name
      value: null
    result:
      name: modified-name
      other: current-other
  - description: nil patch with retainKeys map
    original:
      name: a
      retainKeysMap:
        name: foo
    current:
      name: a
      value: b
      retainKeysMap:
        name: foo
    modified:
      name: a
      retainKeysMap:
        name: foo
    twoWay: {}
    threeWay: {}
    result:
      name: a
      value: b
      retainKeysMap:
        name: foo
  - description: retainKeys map with no change should not be present
    original:
      name: a
      retainKeysMap:
        name: foo
    current:
      name: a
      other: c
      retainKeysMap:
        name: foo
    modified:
      name: a
      value: b
      retainKeysMap:
        name: foo
    twoWay:
      value: b
    threeWay:
      value: b
    result:
      name: a
      value: b
      other: c
      retainKeysMap:
        name: foo
`)

var strategicMergePatchRawTestCases = []StrategicMergePatchRawTestCase{
	{
		Description: "nested patch merge with empty list",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
name: hi
`),
			Current: []byte(`
name: hi
mergingList:
- name: hello2
`),
			Modified: []byte(`
name: hi
mergingList:
- name: hello
- $patch: delete
  name: doesntexist
`),
			TwoWay: []byte(`
mergingList:
- name: hello
- $patch: delete
  name: doesntexist
`),
			ThreeWay: []byte(`
$setElementOrder/mergingList:
- name: hello
- name: doesntexist
mergingList:
- name: hello
`),
			TwoWayResult: []byte(`
name: hi
mergingList:
- name: hello
`),
			Result: []byte(`
name: hi
mergingList:
- name: hello
- name: hello2
`),
		},
	},
	{
		Description: "delete items in lists of scalars",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
mergingIntList:
  - 1
  - 2
  - 3
`),
			TwoWay: []byte(`
$setElementOrder/mergingIntList:
  - 1
  - 2
$deleteFromPrimitiveList/mergingIntList:
  - 3
`),
			Modified: []byte(`
mergingIntList:
  - 1
  - 2
`),
			Current: []byte(`
mergingIntList:
  - 1
  - 2
  - 3
  - 4
`),
			ThreeWay: []byte(`
$setElementOrder/mergingIntList:
  - 1
  - 2
$deleteFromPrimitiveList/mergingIntList:
  - 3
`),
			Result: []byte(`
mergingIntList:
  - 1
  - 2
  - 4
`),
		},
	},
	{
		Description: "delete all duplicate items in lists of scalars",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
mergingIntList:
  - 1
  - 2
  - 3
  - 3
`),
			TwoWay: []byte(`
$setElementOrder/mergingIntList:
  - 1
  - 2
$deleteFromPrimitiveList/mergingIntList:
  - 3
`),
			Modified: []byte(`
mergingIntList:
  - 1
  - 2
`),
			Current: []byte(`
mergingIntList:
  - 1
  - 2
  - 3
  - 3
  - 4
`),
			ThreeWay: []byte(`
$setElementOrder/mergingIntList:
  - 1
  - 2
$deleteFromPrimitiveList/mergingIntList:
  - 3
`),
			Result: []byte(`
mergingIntList:
  - 1
  - 2
  - 4
`),
		},
	},
	{
		Description: "add and delete items in lists of scalars",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
mergingIntList:
  - 1
  - 2
  - 3
`),
			TwoWay: []byte(`
$setElementOrder/mergingIntList:
  - 1
  - 2
  - 4
$deleteFromPrimitiveList/mergingIntList:
  - 3
mergingIntList:
  - 4
`),
			Modified: []byte(`
mergingIntList:
  - 1
  - 2
  - 4
`),
			Current: []byte(`
mergingIntList:
  - 1
  - 2
  - 3
  - 4
`),
			ThreeWay: []byte(`
$setElementOrder/mergingIntList:
  - 1
  - 2
  - 4
$deleteFromPrimitiveList/mergingIntList:
  - 3
`),
			Result: []byte(`
mergingIntList:
  - 1
  - 2
  - 4
`),
		},
	},
	{
		Description: "merge lists of maps",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
mergingList:
  - name: 1
  - name: 2
    value: 2
`),
			TwoWay: []byte(`
$setElementOrder/mergingList:
  - name: 4
  - name: 1
  - name: 2
  - name: 3
mergingList:
  - name: 4
    value: 4
  - name: 3
    value: 3
`),
			Modified: []byte(`
mergingList:
  - name: 4
    value: 4
  - name: 1
  - name: 2
    value: 2
  - name: 3
    value: 3
`),
			Current: []byte(`
mergingList:
  - name: 1
    other: a
  - name: 2
    value: 2
    other: b
`),
			ThreeWay: []byte(`
$setElementOrder/mergingList:
  - name: 4
  - name: 1
  - name: 2
  - name: 3
mergingList:
  - name: 4
    value: 4
  - name: 3
    value: 3
`),
			Result: []byte(`
mergingList:
  - name: 4
    value: 4
  - name: 1
    other: a
  - name: 2
    value: 2
    other: b
  - name: 3
    value: 3
`),
		},
	},
	{
		Description: "merge lists of maps with conflict",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
mergingList:
  - name: 1
  - name: 2
    value: 2
`),
			TwoWay: []byte(`
$setElementOrder/mergingList:
  - name: 1
  - name: 2
  - name: 3
mergingList:
  - name: 3
    value: 3
`),
			Modified: []byte(`
mergingList:
  - name: 1
  - name: 2
    value: 2
  - name: 3
    value: 3
`),
			Current: []byte(`
mergingList:
  - name: 1
    other: a
  - name: 2
    value: 3
    other: b
`),
			ThreeWay: []byte(`
$setElementOrder/mergingList:
  - name: 1
  - name: 2
  - name: 3
mergingList:
  - name: 2
    value: 2
  - name: 3
    value: 3
`),
			Result: []byte(`
mergingList:
  - name: 1
    other: a
  - name: 2
    value: 2
    other: b
  - name: 3
    value: 3
`),
		},
	},
	{
		Description: "add field to map in merging list",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
mergingList:
  - name: 1
  - name: 2
    value: 2
`),
			TwoWay: []byte(`
$setElementOrder/mergingList:
  - name: 1
  - name: 2
mergingList:
  - name: 1
    value: 1
`),
			Modified: []byte(`
mergingList:
  - name: 1
    value: 1
  - name: 2
    value: 2
`),
			Current: []byte(`
mergingList:
  - name: 1
    other: a
  - name: 2
    value: 2
    other: b
`),
			ThreeWay: []byte(`
$setElementOrder/mergingList:
  - name: 1
  - name: 2
mergingList:
  - name: 1
    value: 1
`),
			Result: []byte(`
mergingList:
  - name: 1
    value: 1
    other: a
  - name: 2
    value: 2
    other: b
`),
		},
	},
	{
		Description: "add field to map in merging list",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
mergingList:
  - name: 1
  - name: 2
    value: 2
`),
			TwoWay: []byte(`
$setElementOrder/mergingList:
  - name: 1
  - name: 2
mergingList:
  - name: 1
    value: 1
`),
			Modified: []byte(`
mergingList:
  - name: 1
    value: 1
  - name: 2
    value: 2
`),
			Current: []byte(`
mergingList:
  - name: 1
    other: a
  - name: 2
    value: 2
    other: b
`),
			ThreeWay: []byte(`
$setElementOrder/mergingList:
  - name: 1
  - name: 2
mergingList:
  - name: 1
    value: 1
`),
			Result: []byte(`
mergingList:
  - name: 1
    value: 1
    other: a
  - name: 2
    value: 2
    other: b
`),
		},
	},
	{
		Description: "add field to map in merging list with conflict",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
mergingList:
  - name: 1
  - name: 2
    value: 2
`),
			TwoWay: []byte(`
$setElementOrder/mergingList:
  - name: 1
  - name: 2
mergingList:
  - name: 1
    value: 1
`),
			Modified: []byte(`
mergingList:
  - name: 1
    value: 1
  - name: 2
    value: 2
`),
			Current: []byte(`
mergingList:
  - name: 1
    other: a
  - name: 3
    value: 2
    other: b
`),
			ThreeWay: []byte(`
$setElementOrder/mergingList:
  - name: 1
  - name: 2
mergingList:
  - name: 1
    value: 1
  - name: 2
    value: 2
`),
			Result: []byte(`
mergingList:
  - name: 1
    value: 1
    other: a
  - name: 2
    value: 2
  - name: 3
    value: 2
    other: b
`),
		},
	},
	{
		Description: "add duplicate field to map in merging list",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
mergingList:
  - name: 1
  - name: 2
    value: 2
`),
			TwoWay: []byte(`
$setElementOrder/mergingList:
  - name: 1
  - name: 2
mergingList:
  - name: 1
    value: 1
`),
			Modified: []byte(`
mergingList:
  - name: 1
    value: 1
  - name: 2
    value: 2
`),
			Current: []byte(`
mergingList:
  - name: 1
    value: 1
    other: a
  - name: 2
    value: 2
    other: b
`),
			ThreeWay: []byte(`{}`),
			Result: []byte(`
mergingList:
  - name: 1
    value: 1
    other: a
  - name: 2
    value: 2
    other: b
`),
		},
	},
	{
		Description: "add an item that already exists in current object in merging list",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
mergingList:
  - name: 1
    value: a
  - name: 2
`),
			TwoWay: []byte(`
$setElementOrder/mergingList:
  - name: 1
  - name: 2
  - name: 3
mergingList:
  - name: 3
`),
			Modified: []byte(`
mergingList:
  - name: 1
    value: a
  - name: 2
  - name: 3
`),
			Current: []byte(`
mergingList:
  - name: 1
    value: a
    other: x
  - name: 2
  - name: 3
`),
			ThreeWay: []byte(`{}`),
			Result: []byte(`
mergingList:
  - name: 1
    value: a
    other: x
  - name: 2
  - name: 3
`),
		},
	},
	{
		Description: "add duplicate field to map in merging list with conflict",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
mergingList:
  - name: 1
  - name: 2
    value: 2
`),
			TwoWay: []byte(`
$setElementOrder/mergingList:
  - name: 1
  - name: 2
mergingList:
  - name: 1
    value: 1
`),
			Modified: []byte(`
mergingList:
  - name: 1
    value: 1
  - name: 2
    value: 2
`),
			Current: []byte(`
mergingList:
  - name: 1
    value: 1
    other: a
  - name: 2
    value: 3
    other: b
`),
			ThreeWay: []byte(`
$setElementOrder/mergingList:
  - name: 1
  - name: 2
mergingList:
  - name: 2
    value: 2
`),
			Result: []byte(`
mergingList:
  - name: 1
    value: 1
    other: a
  - name: 2
    value: 2
    other: b
`),
		},
	},
	{
		Description: "replace map field value in merging list",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
mergingList:
  - name: 1
    value: 1
  - name: 2
    value: 2
`),
			TwoWay: []byte(`
$setElementOrder/mergingList:
  - name: 1
  - name: 2
mergingList:
  - name: 1
    value: a
`),
			Modified: []byte(`
mergingList:
  - name: 1
    value: a
  - name: 2
    value: 2
`),
			Current: []byte(`
mergingList:
  - name: 1
    value: 1
    other: a
  - name: 2
    value: 2
    other: b
`),
			ThreeWay: []byte(`
$setElementOrder/mergingList:
  - name: 1
  - name: 2
mergingList:
  - name: 1
    value: a
`),
			Result: []byte(`
mergingList:
  - name: 1
    value: a
    other: a
  - name: 2
    value: 2
    other: b
`),
		},
	},
	{
		Description: "replace map field value in merging list with conflict",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
mergingList:
  - name: 1
    value: 1
  - name: 2
    value: 2
`),
			TwoWay: []byte(`
$setElementOrder/mergingList:
  - name: 1
  - name: 2
mergingList:
  - name: 1
    value: a
`),
			Modified: []byte(`
mergingList:
  - name: 1
    value: a
  - name: 2
    value: 2
`),
			Current: []byte(`
mergingList:
  - name: 1
    value: 3
    other: a
  - name: 2
    value: 2
    other: b
`),
			ThreeWay: []byte(`
$setElementOrder/mergingList:
  - name: 1
  - name: 2
mergingList:
  - name: 1
    value: a
`),
			Result: []byte(`
mergingList:
  - name: 1
    value: a
    other: a
  - name: 2
    value: 2
    other: b
`),
		},
	},
	{
		Description: "delete map from merging list",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
mergingList:
  - name: 1
  - name: 2
`),
			TwoWay: []byte(`
$setElementOrder/mergingList:
  - name: 2
mergingList:
  - name: 1
    $patch: delete
`),
			Modified: []byte(`
mergingList:
  - name: 2
`),
			Current: []byte(`
mergingList:
  - name: 1
  - name: 2
    other: b
`),
			ThreeWay: []byte(`
$setElementOrder/mergingList:
  - name: 2
mergingList:
  - name: 1
    $patch: delete
`),
			Result: []byte(`
mergingList:
  - name: 2
    other: b
`),
		},
	},
	{
		Description: "delete map from merging list with conflict",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
mergingList:
  - name: 1
  - name: 2
`),
			TwoWay: []byte(`
$setElementOrder/mergingList:
  - name: 2
mergingList:
  - name: 1
    $patch: delete
`),
			Modified: []byte(`
mergingList:
  - name: 2
`),
			Current: []byte(`
mergingList:
  - name: 1
    other: a
  - name: 2
    other: b
`),
			ThreeWay: []byte(`
$setElementOrder/mergingList:
  - name: 2
mergingList:
  - name: 1
    $patch: delete
`),
			Result: []byte(`
mergingList:
  - name: 2
    other: b
`),
		},
	},
	{
		Description: "delete missing map from merging list",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
mergingList:
  - name: 1
  - name: 2
`),
			TwoWay: []byte(`
$setElementOrder/mergingList:
  - name: 2
mergingList:
  - name: 1
    $patch: delete
`),
			Modified: []byte(`
mergingList:
  - name: 2
`),
			Current: []byte(`
mergingList:
  - name: 2
    other: b
`),
			ThreeWay: []byte(`
$setElementOrder/mergingList:
  - name: 2
mergingList:
  - name: 1
    $patch: delete
`),
			Result: []byte(`
mergingList:
  - name: 2
    other: b
`),
		},
	},
	{
		Description: "delete missing map from merging list with conflict",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
mergingList:
  - name: 1
  - name: 2
`),
			TwoWay: []byte(`
$setElementOrder/mergingList:
  - name: 2
mergingList:
  - name: 1
    $patch: delete
`),
			Modified: []byte(`
mergingList:
  - name: 2
`),
			Current: []byte(`
mergingList:
  - name: 3
    other: a
`),
			ThreeWay: []byte(`
$setElementOrder/mergingList:
  - name: 2
mergingList:
  - name: 2
  - name: 1
    $patch: delete
`),
			Result: []byte(`
mergingList:
  - name: 2
  - name: 3
    other: a
`),
		},
	},
	{
		Description: "add map and delete map from merging list",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
mergingList:
  - name: 1
  - name: 2
`),
			TwoWay: []byte(`
$setElementOrder/mergingList:
  - name: 2
  - name: 3
mergingList:
  - name: 3
  - name: 1
    $patch: delete
`),
			Modified: []byte(`
mergingList:
  - name: 2
  - name: 3
`),
			Current: []byte(`
mergingList:
  - name: 1
  - name: 2
    other: b
  - name: 4
    other: c
`),
			ThreeWay: []byte(`
$setElementOrder/mergingList:
  - name: 2
  - name: 3
mergingList:
  - name: 3
  - name: 1
    $patch: delete
`),
			Result: []byte(`
mergingList:
  - name: 2
    other: b
  - name: 4
    other: c
  - name: 3
`),
		},
	},
	{
		Description: "add map and delete map from merging list with conflict",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
mergingList:
  - name: 1
  - name: 2
`),
			TwoWay: []byte(`
$setElementOrder/mergingList:
  - name: 2
  - name: 3
mergingList:
  - name: 3
  - name: 1
    $patch: delete
`),
			Modified: []byte(`
mergingList:
  - name: 2
  - name: 3
`),
			Current: []byte(`
mergingList:
  - name: 1
    other: a
  - name: 4
    other: c
`),
			ThreeWay: []byte(`
$setElementOrder/mergingList:
  - name: 2
  - name: 3
mergingList:
  - name: 2
  - name: 3
  - name: 1
    $patch: delete
`),
			Result: []byte(`
mergingList:
  - name: 4
    other: c
  - name: 2
  - name: 3
`),
		},
	},
	{
		Description: "delete field from map in merging list",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
mergingList:
  - name: 1
    value: 1
  - name: 2
    value: 2
`),
			TwoWay: []byte(`
$setElementOrder/mergingList:
  - name: 1
  - name: 2
mergingList:
  - name: 1
    value: null
`),
			Modified: []byte(`
mergingList:
  - name: 1
  - name: 2
    value: 2
`),
			Current: []byte(`
mergingList:
  - name: 1
    value: 1
    other: a
  - name: 2
    value: 2
    other: b
`),
			ThreeWay: []byte(`
$setElementOrder/mergingList:
  - name: 1
  - name: 2
mergingList:
  - name: 1
    value: null
`),
			Result: []byte(`
mergingList:
  - name: 1
    other: a
  - name: 2
    value: 2
    other: b
`),
		},
	},
	{
		Description: "delete field from map in merging list with conflict",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
mergingList:
  - name: 1
    value: 1
  - name: 2
    value: 2
`),
			TwoWay: []byte(`
$setElementOrder/mergingList:
  - name: 1
  - name: 2
mergingList:
  - name: 1
    value: null
`),
			Modified: []byte(`
mergingList:
  - name: 1
  - name: 2
    value: 2
`),
			Current: []byte(`
mergingList:
  - name: 1
    value: a
    other: a
  - name: 2
    value: 2
`),
			ThreeWay: []byte(`
$setElementOrder/mergingList:
  - name: 1
  - name: 2
mergingList:
  - name: 1
    value: null
`),
			Result: []byte(`
mergingList:
  - name: 1
    other: a
  - name: 2
    value: 2
`),
		},
	},
	{
		Description: "delete missing field from map in merging list",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
mergingList:
  - name: 1
    value: 1
  - name: 2
    value: 2
`),
			TwoWay: []byte(`
$setElementOrder/mergingList:
  - name: 1
  - name: 2
mergingList:
  - name: 1
    value: null
`),
			Modified: []byte(`
mergingList:
  - name: 1
  - name: 2
    value: 2
`),
			Current: []byte(`
mergingList:
  - name: 1
    other: a
  - name: 2
    value: 2
    other: b
`),
			ThreeWay: []byte(`
$setElementOrder/mergingList:
  - name: 1
  - name: 2
mergingList:
  - name: 1
    value: null
`),
			Result: []byte(`
mergingList:
  - name: 1
    other: a
  - name: 2
    value: 2
    other: b
`),
		},
	},
	{
		Description: "delete missing field from map in merging list with conflict",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
mergingList:
  - name: 1
    value: 1
  - name: 2
    value: 2
`),
			TwoWay: []byte(`
$setElementOrder/mergingList:
  - name: 1
  - name: 2
mergingList:
  - name: 1
    value: null
`),
			Modified: []byte(`
mergingList:
  - name: 1
  - name: 2
    value: 2
`),
			Current: []byte(`
mergingList:
  - name: 1
    other: a
  - name: 2
    other: b
`),
			ThreeWay: []byte(`
$setElementOrder/mergingList:
  - name: 1
  - name: 2
mergingList:
  - name: 1
    value: null
  - name: 2
    value: 2
`),
			Result: []byte(`
mergingList:
  - name: 1
    other: a
  - name: 2
    value: 2
    other: b
`),
		},
	},
	{
		Description: "replace non merging list nested in merging list",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
mergingList:
  - name: 1
    nonMergingList:
      - name: 1
      - name: 2
        value: 2
  - name: 2
`),
			TwoWay: []byte(`
$setElementOrder/mergingList:
  - name: 1
  - name: 2
mergingList:
  - name: 1
    nonMergingList:
      - name: 1
        value: 1
`),
			Modified: []byte(`
mergingList:
  - name: 1
    nonMergingList:
      - name: 1
        value: 1
  - name: 2
`),
			Current: []byte(`
mergingList:
  - name: 1
    other: a
    nonMergingList:
      - name: 1
      - name: 2
        value: 2
  - name: 2
    other: b
`),
			ThreeWay: []byte(`
$setElementOrder/mergingList:
  - name: 1
  - name: 2
mergingList:
  - name: 1
    nonMergingList:
      - name: 1
        value: 1
`),
			Result: []byte(`
mergingList:
  - name: 1
    other: a
    nonMergingList:
      - name: 1
        value: 1
  - name: 2
    other: b
`),
		},
	},
	{
		Description: "replace non merging list nested in merging list with value conflict",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
mergingList:
  - name: 1
    nonMergingList:
      - name: 1
      - name: 2
        value: 2
  - name: 2
`),
			TwoWay: []byte(`
$setElementOrder/mergingList:
  - name: 1
  - name: 2
mergingList:
  - name: 1
    nonMergingList:
      - name: 1
        value: 1
`),
			Modified: []byte(`
mergingList:
  - name: 1
    nonMergingList:
      - name: 1
        value: 1
  - name: 2
`),
			Current: []byte(`
mergingList:
  - name: 1
    other: a
    nonMergingList:
      - name: 1
        value: c
  - name: 2
    other: b
`),
			ThreeWay: []byte(`
$setElementOrder/mergingList:
  - name: 1
  - name: 2
mergingList:
  - name: 1
    nonMergingList:
      - name: 1
        value: 1
`),
			Result: []byte(`
mergingList:
  - name: 1
    other: a
    nonMergingList:
      - name: 1
        value: 1
  - name: 2
    other: b
`),
		},
	},
	{
		Description: "replace non merging list nested in merging list with deletion conflict",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
mergingList:
  - name: 1
    nonMergingList:
      - name: 1
      - name: 2
        value: 2
  - name: 2
`),
			TwoWay: []byte(`
$setElementOrder/mergingList:
  - name: 1
  - name: 2
mergingList:
  - name: 1
    nonMergingList:
      - name: 1
        value: 1
`),
			Modified: []byte(`
mergingList:
  - name: 1
    nonMergingList:
      - name: 1
        value: 1
  - name: 2
`),
			Current: []byte(`
mergingList:
  - name: 1
    other: a
    nonMergingList:
      - name: 2
        value: 2
  - name: 2
    other: b
`),
			ThreeWay: []byte(`
$setElementOrder/mergingList:
  - name: 1
  - name: 2
mergingList:
  - name: 1
    nonMergingList:
      - name: 1
        value: 1
`),
			Result: []byte(`
mergingList:
  - name: 1
    other: a
    nonMergingList:
      - name: 1
        value: 1
  - name: 2
    other: b
`),
		},
	},
	{
		Description: "add field to map in merging list nested in merging list",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
mergingList:
  - name: 1
    mergingList:
      - name: 1
      - name: 2
        value: 2
  - name: 2
`),
			TwoWay: []byte(`
$setElementOrder/mergingList:
  - name: 1
  - name: 2
mergingList:
  - $setElementOrder/mergingList:
      - name: 1
      - name: 2
    name: 1
    mergingList:
      - name: 1
        value: 1
`),
			Modified: []byte(`
mergingList:
  - name: 1
    mergingList:
      - name: 1
        value: 1
      - name: 2
        value: 2
  - name: 2
`),
			Current: []byte(`
mergingList:
  - name: 1
    other: a
    mergingList:
      - name: 1
      - name: 2
        value: 2
  - name: 2
    other: b
`),
			ThreeWay: []byte(`
$setElementOrder/mergingList:
  - name: 1
  - name: 2
mergingList:
  - $setElementOrder/mergingList:
      - name: 1
      - name: 2
    name: 1
    mergingList:
      - name: 1
        value: 1
`),
			Result: []byte(`
mergingList:
  - name: 1
    other: a
    mergingList:
      - name: 1
        value: 1
      - name: 2
        value: 2
  - name: 2
    other: b
`),
		},
	},
	{
		Description: "add field to map in merging list nested in merging list with value conflict",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
mergingList:
  - name: 1
    mergingList:
      - name: 1
      - name: 2
        value: 2
  - name: 2
`),
			TwoWay: []byte(`
$setElementOrder/mergingList:
  - name: 1
  - name: 2
mergingList:
  - $setElementOrder/mergingList:
      - name: 1
      - name: 2
    name: 1
    mergingList:
      - name: 1
        value: 1
`),
			Modified: []byte(`
mergingList:
  - name: 1
    mergingList:
      - name: 1
        value: 1
      - name: 2
        value: 2
  - name: 2
`),
			Current: []byte(`
mergingList:
  - name: 1
    other: a
    mergingList:
      - name: 1
        value: a
        other: c
      - name: 2
        value: b
        other: d
  - name: 2
    other: b
`),
			ThreeWay: []byte(`
$setElementOrder/mergingList:
  - name: 1
  - name: 2
mergingList:
  - $setElementOrder/mergingList:
      - name: 1
      - name: 2
    name: 1
    mergingList:
      - name: 1
        value: 1
      - name: 2
        value: 2
`),
			Result: []byte(`
mergingList:
  - name: 1
    other: a
    mergingList:
      - name: 1
        value: 1
        other: c
      - name: 2
        value: 2
        other: d
  - name: 2
    other: b
`),
		},
	},
	{
		Description: "add field to map in merging list nested in merging list with deletion conflict",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
mergingList:
  - name: 1
    mergingList:
      - name: 1
      - name: 2
        value: 2
  - name: 2
`),
			TwoWay: []byte(`
$setElementOrder/mergingList:
  - name: 1
  - name: 2
mergingList:
  - $setElementOrder/mergingList:
      - name: 1
      - name: 2
    name: 1
    mergingList:
      - name: 1
        value: 1
`),
			Modified: []byte(`
mergingList:
  - name: 1
    mergingList:
      - name: 1
        value: 1
      - name: 2
        value: 2
  - name: 2
`),
			Current: []byte(`
mergingList:
  - name: 1
    other: a
    mergingList:
      - name: 2
        value: 2
        other: d
  - name: 2
    other: b
`),
			ThreeWay: []byte(`
$setElementOrder/mergingList:
  - name: 1
  - name: 2
mergingList:
  - $setElementOrder/mergingList:
      - name: 1
      - name: 2
    name: 1
    mergingList:
      - name: 1
        value: 1
`),
			Result: []byte(`
mergingList:
  - name: 1
    other: a
    mergingList:
      - name: 1
        value: 1
      - name: 2
        value: 2
        other: d
  - name: 2
    other: b
`),
		},
	},

	{
		Description: "add field to map in merging list nested in merging list with deletion conflict",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
mergingList:
  - name: 1
    mergingList:
      - name: 1
      - name: 2
        value: 2
  - name: 2
`),
			TwoWay: []byte(`
$setElementOrder/mergingList:
  - name: 1
  - name: 2
mergingList:
  - $setElementOrder/mergingList:
      - name: 2
      - name: 1
    name: 1
    mergingList:
      - name: 1
        value: 1
`),
			Modified: []byte(`
mergingList:
  - name: 1
    mergingList:
      - name: 2
        value: 2
      - name: 1
        value: 1
  - name: 2
`),
			Current: []byte(`
mergingList:
  - name: 1
    other: a
    mergingList:
      - name: 2
        value: 2
        other: d
  - name: 2
    other: b
`),
			ThreeWay: []byte(`
$setElementOrder/mergingList:
  - name: 1
  - name: 2
mergingList:
  - $setElementOrder/mergingList:
      - name: 2
      - name: 1
    name: 1
    mergingList:
      - name: 1
        value: 1
`),
			Result: []byte(`
mergingList:
  - name: 1
    other: a
    mergingList:
      - name: 2
        value: 2
        other: d
      - name: 1
        value: 1
  - name: 2
    other: b
`),
		},
	},
	{
		Description: "add map to merging list by pointer",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
mergeItemPtr:
  - name: 1
`),
			TwoWay: []byte(`
$setElementOrder/mergeItemPtr:
  - name: 1
  - name: 2
mergeItemPtr:
  - name: 2
`),
			Modified: []byte(`
mergeItemPtr:
  - name: 1
  - name: 2
`),
			Current: []byte(`
mergeItemPtr:
  - name: 1
    other: a
  - name: 3
`),
			ThreeWay: []byte(`
$setElementOrder/mergeItemPtr:
  - name: 1
  - name: 2
mergeItemPtr:
  - name: 2
`),
			Result: []byte(`
mergeItemPtr:
  - name: 1
    other: a
  - name: 2
  - name: 3
`),
		},
	},
	{
		Description: "add map to merging list by pointer with conflict",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
mergeItemPtr:
  - name: 1
`),
			TwoWay: []byte(`
$setElementOrder/mergeItemPtr:
  - name: 1
  - name: 2
mergeItemPtr:
  - name: 2
`),
			Modified: []byte(`
mergeItemPtr:
  - name: 1
  - name: 2
`),
			Current: []byte(`
mergeItemPtr:
  - name: 3
`),
			ThreeWay: []byte(`
$setElementOrder/mergeItemPtr:
  - name: 1
  - name: 2
mergeItemPtr:
  - name: 1
  - name: 2
`),
			Result: []byte(`
mergeItemPtr:
  - name: 1
  - name: 2
  - name: 3
`),
		},
	},
	{
		Description: "add field to map in merging list by pointer",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
mergeItemPtr:
  - name: 1
    mergeItemPtr:
      - name: 1
      - name: 2
        value: 2
  - name: 2
`),
			TwoWay: []byte(`
$setElementOrder/mergeItemPtr:
  - name: 1
  - name: 2
mergeItemPtr:
  - $setElementOrder/mergeItemPtr:
      - name: 1
      - name: 2
    name: 1
    mergeItemPtr:
      - name: 1
        value: 1
`),
			Modified: []byte(`
mergeItemPtr:
  - name: 1
    mergeItemPtr:
      - name: 1
        value: 1
      - name: 2
        value: 2
  - name: 2
`),
			Current: []byte(`
mergeItemPtr:
  - name: 1
    other: a
    mergeItemPtr:
      - name: 1
        other: a
      - name: 2
        value: 2
        other: b
  - name: 2
    other: b
`),
			ThreeWay: []byte(`
$setElementOrder/mergeItemPtr:
  - name: 1
  - name: 2
mergeItemPtr:
  - $setElementOrder/mergeItemPtr:
      - name: 1
      - name: 2
    name: 1
    mergeItemPtr:
      - name: 1
        value: 1
`),
			Result: []byte(`
mergeItemPtr:
  - name: 1
    other: a
    mergeItemPtr:
      - name: 1
        value: 1
        other: a
      - name: 2
        value: 2
        other: b
  - name: 2
    other: b
`),
		},
	},
	{
		Description: "add field to map in merging list by pointer with conflict",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
mergeItemPtr:
  - name: 1
    mergeItemPtr:
      - name: 1
      - name: 2
        value: 2
  - name: 2
`),
			TwoWay: []byte(`
$setElementOrder/mergeItemPtr:
  - name: 1
  - name: 2
mergeItemPtr:
  - $setElementOrder/mergeItemPtr:
      - name: 1
      - name: 2
    name: 1
    mergeItemPtr:
      - name: 1
        value: 1
`),
			Modified: []byte(`
mergeItemPtr:
  - name: 1
    mergeItemPtr:
      - name: 1
        value: 1
      - name: 2
        value: 2
  - name: 2
`),
			Current: []byte(`
mergeItemPtr:
  - name: 1
    other: a
    mergeItemPtr:
      - name: 1
        value: a
      - name: 2
        value: 2
        other: b
  - name: 2
    other: b
`),
			ThreeWay: []byte(`
$setElementOrder/mergeItemPtr:
  - name: 1
  - name: 2
mergeItemPtr:
  - $setElementOrder/mergeItemPtr:
      - name: 1
      - name: 2
    name: 1
    mergeItemPtr:
      - name: 1
        value: 1
`),
			Result: []byte(`
mergeItemPtr:
  - name: 1
    other: a
    mergeItemPtr:
      - name: 1
        value: 1
      - name: 2
        value: 2
        other: b
  - name: 2
    other: b
`),
		},
	},
	{
		Description: "merge lists of scalars",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
mergingIntList:
- 1
- 2
`),
			TwoWay: []byte(`
$setElementOrder/mergingIntList:
- 1
- 2
- 3
mergingIntList:
- 3
`),
			Modified: []byte(`
mergingIntList:
- 1
- 2
- 3
`),
			Current: []byte(`
mergingIntList:
- 1
- 2
- 4
`),
			ThreeWay: []byte(`
$setElementOrder/mergingIntList:
- 1
- 2
- 3
mergingIntList:
- 3
`),
			Result: []byte(`
mergingIntList:
- 1
- 2
- 3
- 4
`),
		},
	},
	{
		Description: "add duplicate field to map in merging int list",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
mergingIntList:
  - 1
  - 2
`),
			TwoWay: []byte(`
$setElementOrder/mergingIntList:
  - 1
  - 2
  - 3
mergingIntList:
  - 3
`),
			Modified: []byte(`
mergingIntList:
  - 1
  - 2
  - 3
`),
			Current: []byte(`
mergingIntList:
  - 1
  - 2
  - 3
`),
			ThreeWay: []byte(`{}`),
			Result: []byte(`
mergingIntList:
  - 1
  - 2
  - 3
`),
		},
	},
	// test case for setElementOrder
	{
		Description: "add an item in a list of primitives and preserve order",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
mergingIntList:
- 1
- 2
`),
			TwoWay: []byte(`
$setElementOrder/mergingIntList:
- 3
- 1
- 2
mergingIntList:
- 3
`),
			Modified: []byte(`
mergingIntList:
- 3
- 1
- 2
`),
			Current: []byte(`
mergingIntList:
- 1
- 4
- 2
`),
			ThreeWay: []byte(`
$setElementOrder/mergingIntList:
- 3
- 1
- 2
mergingIntList:
- 3
`),
			Result: []byte(`
mergingIntList:
- 3
- 1
- 4
- 2
`),
		},
	},
	{
		Description: "delete an item in a list of primitives and preserve order",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
mergingIntList:
- 1
- 2
- 3
`),
			TwoWay: []byte(`
$setElementOrder/mergingIntList:
- 2
- 1
$deleteFromPrimitiveList/mergingIntList:
- 3
`),
			Modified: []byte(`
mergingIntList:
- 2
- 1
`),
			Current: []byte(`
mergingIntList:
- 1
- 2
- 3
`),
			ThreeWay: []byte(`
$setElementOrder/mergingIntList:
- 2
- 1
$deleteFromPrimitiveList/mergingIntList:
- 3
`),
			Result: []byte(`
mergingIntList:
- 2
- 1
`),
		},
	},
	{
		Description: "add an item in a list and preserve order",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
mergingList:
  - name: 1
  - name: 2
    value: 2
`),
			TwoWay: []byte(`
$setElementOrder/mergingList:
  - name: 3
  - name: 1
  - name: 2
mergingList:
  - name: 3
    value: 3
`),
			Modified: []byte(`
mergingList:
  - name: 3
    value: 3
  - name: 1
  - name: 2
    value: 2
`),
			Current: []byte(`
mergingList:
  - name: 1
    other: a
  - name: 2
    value: 2
    other: b
`),
			ThreeWay: []byte(`
$setElementOrder/mergingList:
  - name: 3
  - name: 1
  - name: 2
mergingList:
  - name: 3
    value: 3
`),
			Result: []byte(`
mergingList:
  - name: 3
    value: 3
  - name: 1
    other: a
  - name: 2
    value: 2
    other: b
`),
		},
	},
	{
		Description: "add multiple items in a list and preserve order",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
mergingList:
  - name: 1
  - name: 2
    value: 2
`),
			TwoWay: []byte(`
$setElementOrder/mergingList:
  - name: 1
  - name: 4
  - name: 2
  - name: 3
mergingList:
  - name: 4
    value: 4
  - name: 3
    value: 3
`),
			Modified: []byte(`
mergingList:
  - name: 1
  - name: 4
    value: 4
  - name: 2
    value: 2
  - name: 3
    value: 3
`),
			Current: []byte(`
mergingList:
  - name: 1
    other: a
  - name: 2
    value: 2
    other: b
`),
			ThreeWay: []byte(`
$setElementOrder/mergingList:
  - name: 1
  - name: 4
  - name: 2
  - name: 3
mergingList:
  - name: 4
    value: 4
  - name: 3
    value: 3
`),
			Result: []byte(`
mergingList:
  - name: 1
    other: a
  - name: 4
    value: 4
  - name: 2
    value: 2
    other: b
  - name: 3
    value: 3
`),
		},
	},
	{
		Description: "delete an item in a list and preserve order",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
mergingList:
  - name: 1
  - name: 3
    value: 3
  - name: 2
    value: 2
`),
			TwoWay: []byte(`
$setElementOrder/mergingList:
  - name: 2
  - name: 1
mergingList:
  - name: 3
    $patch: delete
`),
			Modified: []byte(`
mergingList:
  - name: 2
    value: 2
  - name: 1
`),
			Current: []byte(`
mergingList:
  - name: 1
    other: a
  - name: 2
    value: 2
    other: b
  - name: 3
    value: 3
`),
			ThreeWay: []byte(`
$setElementOrder/mergingList:
  - name: 2
  - name: 1
mergingList:
  - name: 3
    $patch: delete
`),
			Result: []byte(`
mergingList:
  - name: 2
    value: 2
    other: b
  - name: 1
    other: a
`),
		},
	},
	{
		Description: "change an item in a list and preserve order",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
mergingList:
  - name: 1
  - name: 3
    value: 3
  - name: 2
    value: 2
`),
			TwoWay: []byte(`
$setElementOrder/mergingList:
  - name: 2
  - name: 3
  - name: 1
mergingList:
  - name: 3
    value: x
`),
			Modified: []byte(`
mergingList:
  - name: 2
    value: 2
  - name: 3
    value: x
  - name: 1
`),
			Current: []byte(`
mergingList:
  - name: 1
    other: a
  - name: 2
    value: 2
    other: b
  - name: 3
    value: 3
`),
			ThreeWay: []byte(`
$setElementOrder/mergingList:
  - name: 2
  - name: 3
  - name: 1
mergingList:
  - name: 3
    value: x
`),
			Result: []byte(`
mergingList:
  - name: 2
    value: 2
    other: b
  - name: 3
    value: x
  - name: 1
    other: a
`),
		},
	},
	{
		Description: "add and delete an item in a list and preserve order",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
mergingList:
  - name: 1
  - name: 3
    value: 3
  - name: 2
    value: 2
`),
			TwoWay: []byte(`
$setElementOrder/mergingList:
  - name: 4
  - name: 2
  - name: 1
mergingList:
  - name: 4
    value: 4
  - name: 3
    $patch: delete
`),
			Modified: []byte(`
mergingList:
  - name: 4
    value: 4
  - name: 2
    value: 2
  - name: 1
`),
			Current: []byte(`
mergingList:
  - name: 1
    other: a
  - name: 2
    value: 2
    other: b
  - name: 3
    value: 3
`),
			ThreeWay: []byte(`
$setElementOrder/mergingList:
  - name: 4
  - name: 2
  - name: 1
mergingList:
  - name: 4
    value: 4
  - name: 3
    $patch: delete
`),
			Result: []byte(`
mergingList:
  - name: 4
    value: 4
  - name: 2
    value: 2
    other: b
  - name: 1
    other: a
`),
		},
	},
	{
		Description: "set elements order in a list",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
mergingList:
  - name: 1
  - name: 3
    value: 3
  - name: 4
    value: 4
  - name: 2
    value: 2
`),
			TwoWay: []byte(`
$setElementOrder/mergingList:
  - name: 4
  - name: 2
  - name: 3
  - name: 1
`),
			Modified: []byte(`
mergingList:
  - name: 4
    value: 4
  - name: 2
    value: 2
  - name: 3
    value: 3
  - name: 1
`),
			Current: []byte(`
mergingList:
  - name: 1
    other: a
  - name: 3
    value: 3
  - name: 4
    value: 4
  - name: 2
    value: 2
`),
			ThreeWay: []byte(`
$setElementOrder/mergingList:
  - name: 4
  - name: 2
  - name: 3
  - name: 1
`),
			Result: []byte(`
mergingList:
  - name: 4
    value: 4
  - name: 2
    value: 2
  - name: 3
    value: 3
  - name: 1
    other: a
`),
		},
	},
	{
		Description: "set elements order in a list with server-only items",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
mergingList:
  - name: 1
  - name: 3
    value: 3
  - name: 4
    value: 4
  - name: 2
    value: 2
`),
			TwoWay: []byte(`
$setElementOrder/mergingList:
  - name: 4
  - name: 2
  - name: 3
  - name: 1
`),
			Modified: []byte(`
mergingList:
  - name: 4
    value: 4
  - name: 2
    value: 2
  - name: 3
    value: 3
  - name: 1
`),
			Current: []byte(`
mergingList:
  - name: 1
    other: a
  - name: 3
    value: 3
  - name: 4
    value: 4
  - name: 2
    value: 2
  - name: 9
`),
			ThreeWay: []byte(`
$setElementOrder/mergingList:
  - name: 4
  - name: 2
  - name: 3
  - name: 1
`),
			Result: []byte(`
mergingList:
  - name: 4
    value: 4
  - name: 2
    value: 2
  - name: 3
    value: 3
  - name: 1
    other: a
  - name: 9
`),
		},
	},
	{
		Description: "set elements order in a list with server-only items 2",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
mergingList:
  - name: 1
  - name: 2
    value: 2
  - name: 3
    value: 3
  - name: 4
    value: 4
`),
			TwoWay: []byte(`
$setElementOrder/mergingList:
  - name: 2
  - name: 1
  - name: 4
  - name: 3
`),
			Modified: []byte(`
mergingList:
  - name: 2
    value: 2
  - name: 1
  - name: 4
    value: 4
  - name: 3
    value: 3
`),
			Current: []byte(`
mergingList:
  - name: 1
    other: a
  - name: 2
    value: 2
  - name: 9
  - name: 3
    value: 3
  - name: 4
    value: 4
`),
			ThreeWay: []byte(`
$setElementOrder/mergingList:
  - name: 2
  - name: 1
  - name: 4
  - name: 3
`),
			Result: []byte(`
mergingList:
  - name: 2
    value: 2
  - name: 1
    other: a
  - name: 9
  - name: 4
    value: 4
  - name: 3
    value: 3
`),
		},
	},
	{
		Description: "set elements order in a list with server-only items 3",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
mergingList:
  - name: 1
  - name: 2
    value: 2
  - name: 3
    value: 3
  - name: 4
    value: 4
`),
			TwoWay: []byte(`
$setElementOrder/mergingList:
  - name: 2
  - name: 1
  - name: 4
  - name: 3
`),
			Modified: []byte(`
mergingList:
  - name: 2
    value: 2
  - name: 1
  - name: 4
    value: 4
  - name: 3
    value: 3
`),
			Current: []byte(`
mergingList:
  - name: 1
    other: a
  - name: 2
    value: 2
  - name: 7
  - name: 9
  - name: 8
  - name: 3
    value: 3
  - name: 4
    value: 4
`),
			ThreeWay: []byte(`
$setElementOrder/mergingList:
  - name: 2
  - name: 1
  - name: 4
  - name: 3
`),
			Result: []byte(`
mergingList:
  - name: 2
    value: 2
  - name: 1
    other: a
  - name: 7
  - name: 9
  - name: 8
  - name: 4
    value: 4
  - name: 3
    value: 3
`),
		},
	},
	{
		Description: "add an item in a int list and preserve order",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
mergingIntList:
  - 1
  - 2
`),
			TwoWay: []byte(`
$setElementOrder/mergingIntList:
  - 3
  - 1
  - 2
mergingIntList:
  - 3
`),
			Modified: []byte(`
mergingIntList:
  - 3
  - 1
  - 2
`),
			Current: []byte(`
mergingIntList:
  - 1
  - 2
`),
			ThreeWay: []byte(`
$setElementOrder/mergingIntList:
  - 3
  - 1
  - 2
mergingIntList:
  - 3
`),
			Result: []byte(`
mergingIntList:
  - 3
  - 1
  - 2
`),
		},
	},
	{
		Description: "add multiple items in a int list and preserve order",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
mergingIntList:
  - 1
  - 2
`),
			TwoWay: []byte(`
$setElementOrder/mergingIntList:
  - 1
  - 4
  - 2
  - 3
mergingIntList:
  - 4
  - 3
`),
			Modified: []byte(`
mergingIntList:
  - 1
  - 4
  - 2
  - 3
`),
			Current: []byte(`
mergingIntList:
  - 1
  - 2
`),
			ThreeWay: []byte(`
$setElementOrder/mergingIntList:
  - 1
  - 4
  - 2
  - 3
mergingIntList:
  - 4
  - 3
`),
			Result: []byte(`
mergingIntList:
  - 1
  - 4
  - 2
  - 3
`),
		},
	},
	{
		Description: "delete an item in a int list and preserve order",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
mergingIntList:
  - 1
  - 3
  - 2
`),
			TwoWay: []byte(`
$setElementOrder/mergingIntList:
  - 2
  - 1
$deleteFromPrimitiveList/mergingIntList:
  - 3
`),
			Modified: []byte(`
mergingIntList:
  - 2
  - 1
`),
			Current: []byte(`
mergingIntList:
  - 1
  - 2
  - 3
`),
			ThreeWay: []byte(`
$setElementOrder/mergingIntList:
  - 2
  - 1
$deleteFromPrimitiveList/mergingIntList:
  - 3
`),
			Result: []byte(`
mergingIntList:
  - 2
  - 1
`),
		},
	},
	{
		Description: "add and delete an item in a int list and preserve order",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
mergingIntList:
  - 1
  - 3
  - 2
`),
			TwoWay: []byte(`
$setElementOrder/mergingIntList:
  - 4
  - 2
  - 1
mergingIntList:
  - 4
$deleteFromPrimitiveList/mergingIntList:
  - 3
`),
			Modified: []byte(`
mergingIntList:
  - 4
  - 2
  - 1
`),
			Current: []byte(`
mergingIntList:
  - 1
  - 2
  - 3
`),
			ThreeWay: []byte(`
$setElementOrder/mergingIntList:
  - 4
  - 2
  - 1
mergingIntList:
  - 4
$deleteFromPrimitiveList/mergingIntList:
  - 3
`),
			Result: []byte(`
mergingIntList:
  - 4
  - 2
  - 1
`),
		},
	},
	{
		Description: "set elements order in a int list",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
mergingIntList:
  - 1
  - 3
  - 4
  - 2
`),
			TwoWay: []byte(`
$setElementOrder/mergingIntList:
  - 4
  - 2
  - 3
  - 1
`),
			Modified: []byte(`
mergingIntList:
  - 4
  - 2
  - 3
  - 1
`),
			Current: []byte(`
mergingIntList:
  - 1
  - 3
  - 4
  - 2
`),
			ThreeWay: []byte(`
$setElementOrder/mergingIntList:
  - 4
  - 2
  - 3
  - 1
`),
			Result: []byte(`
mergingIntList:
  - 4
  - 2
  - 3
  - 1
`),
		},
	},
	{
		Description: "set elements order in a int list with server-only items",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
mergingIntList:
  - 1
  - 3
  - 4
  - 2
`),
			TwoWay: []byte(`
$setElementOrder/mergingIntList:
  - 4
  - 2
  - 3
  - 1
`),
			Modified: []byte(`
mergingIntList:
  - 4
  - 2
  - 3
  - 1
`),
			Current: []byte(`
mergingIntList:
  - 1
  - 3
  - 4
  - 2
  - 9
`),
			ThreeWay: []byte(`
$setElementOrder/mergingIntList:
  - 4
  - 2
  - 3
  - 1
`),
			Result: []byte(`
mergingIntList:
  - 4
  - 2
  - 3
  - 1
  - 9
`),
		},
	},
	{
		Description: "set elements order in a int list with server-only items 2",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
mergingIntList:
  - 1
  - 2
  - 3
  - 4
`),
			TwoWay: []byte(`
$setElementOrder/mergingIntList:
  - 2
  - 1
  - 4
  - 3
`),
			Modified: []byte(`
mergingIntList:
  - 2
  - 1
  - 4
  - 3
`),
			Current: []byte(`
mergingIntList:
  - 1
  - 2
  - 9
  - 3
  - 4
`),
			ThreeWay: []byte(`
$setElementOrder/mergingIntList:
  - 2
  - 1
  - 4
  - 3
`),
			Result: []byte(`
mergingIntList:
  - 2
  - 1
  - 9
  - 4
  - 3
`),
		},
	},
	{
		Description: "set elements order in a int list with server-only items 3",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
mergingIntList:
  - 1
  - 2
  - 3
  - 4
`),
			TwoWay: []byte(`
$setElementOrder/mergingIntList:
  - 2
  - 1
  - 4
  - 3
`),
			Modified: []byte(`
mergingIntList:
  - 2
  - 1
  - 4
  - 3
`),
			Current: []byte(`
mergingIntList:
  - 1
  - 2
  - 7
  - 9
  - 8
  - 3
  - 4
`),
			ThreeWay: []byte(`
$setElementOrder/mergingIntList:
  - 2
  - 1
  - 4
  - 3
`),
			Result: []byte(`
mergingIntList:
  - 2
  - 1
  - 7
  - 9
  - 8
  - 4
  - 3
`),
		},
	},
	{
		// This test case is used just to demonstrate the behavior when dealing with a list with duplicate
		Description: "behavior of set element order for a merging list with duplicate",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
mergingList:
- name: 1
- name: 2
  value: dup1
- name: 3
- name: 2
  value: dup2
- name: 4
`),
			Current: []byte(`
mergingList:
- name: 1
- name: 2
  value: dup1
- name: 3
- name: 2
  value: dup2
- name: 4
`),
			Modified: []byte(`
mergingList:
- name: 2
  value: dup1
- name: 1
- name: 4
- name: 3
- name: 2
  value: dup2
`),
			TwoWay: []byte(`
$setElementOrder/mergingList:
- name: 2
- name: 1
- name: 4
- name: 3
- name: 2
`),
			TwoWayResult: []byte(`
mergingList:
- name: 2
  value: dup1
- name: 2
  value: dup2
- name: 1
- name: 4
- name: 3
`),
			ThreeWay: []byte(`
$setElementOrder/mergingList:
- name: 2
- name: 1
- name: 4
- name: 3
- name: 2
`),
			Result: []byte(`
mergingList:
- name: 2
  value: dup1
- name: 2
  value: dup2
- name: 1
- name: 4
- name: 3
`),
		},
	},
	{
		// This test case is used just to demonstrate the behavior when dealing with a list with duplicate
		Description: "behavior of set element order for a merging int list with duplicate",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
mergingIntList:
- 1
- 2
- 3
- 2
- 4
`),
			Current: []byte(`
mergingIntList:
- 1
- 2
- 3
- 2
- 4
`),
			Modified: []byte(`
mergingIntList:
- 2
- 1
- 4
- 3
- 2
`),
			TwoWay: []byte(`
$setElementOrder/mergingIntList:
- 2
- 1
- 4
- 3
- 2
`),
			TwoWayResult: []byte(`
mergingIntList:
- 2
- 2
- 1
- 4
- 3
`),
			ThreeWay: []byte(`
$setElementOrder/mergingIntList:
- 2
- 1
- 4
- 3
- 2
`),
			Result: []byte(`
mergingIntList:
- 2
- 2
- 1
- 4
- 3
`),
		},
	},
	{
		Description: "retainKeys map should clear defaulted field",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`{}`),
			Current: []byte(`
retainKeysMap:
  value: foo
`),
			Modified: []byte(`
retainKeysMap:
  other: bar
`),
			TwoWay: []byte(`
retainKeysMap:
  other: bar
`),
			ThreeWay: []byte(`
retainKeysMap:
  $retainKeys:
    - other
  other: bar
`),
			Result: []byte(`
retainKeysMap:
  other: bar
`),
		},
	},
	{
		Description: "retainKeys map should clear defaulted field with conflict (discriminated union)",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`{}`),
			Current: []byte(`
retainKeysMap:
  name: type1
  value: foo
`),
			Modified: []byte(`
retainKeysMap:
  name: type2
  other: bar
`),
			TwoWay: []byte(`
retainKeysMap:
  name: type2
  other: bar
`),
			ThreeWay: []byte(`
retainKeysMap:
  $retainKeys:
    - name
    - other
  name: type2
  other: bar
`),
			Result: []byte(`
retainKeysMap:
  name: type2
  other: bar
`),
		},
	},
	{
		Description: "retainKeys map adds a field",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
retainKeysMap:
  name: foo
`),
			Current: []byte(`
retainKeysMap:
  name: foo
`),
			Modified: []byte(`
retainKeysMap:
  name: foo
  value: bar
`),
			TwoWay: []byte(`
retainKeysMap:
  $retainKeys:
    - name
    - value
  value: bar
`),
			ThreeWay: []byte(`
retainKeysMap:
  $retainKeys:
    - name
    - value
  value: bar
`),
			Result: []byte(`
retainKeysMap:
  name: foo
  value: bar
`),
		},
	},
	{
		Description: "retainKeys map adds a field and clear a field",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
retainKeysMap:
  name: foo
`),
			Current: []byte(`
retainKeysMap:
  name: foo
  other: a
`),
			Modified: []byte(`
retainKeysMap:
  name: foo
  value: bar
`),
			TwoWay: []byte(`
retainKeysMap:
  $retainKeys:
    - name
    - value
  value: bar
`),
			ThreeWay: []byte(`
retainKeysMap:
  $retainKeys:
    - name
    - value
  value: bar
`),
			Result: []byte(`
retainKeysMap:
  name: foo
  value: bar
`),
		},
	},
	{
		Description: "retainKeys map deletes a field",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
retainKeysMap:
  name: foo
  value: bar
`),
			Current: []byte(`
retainKeysMap:
  name: foo
  value: bar
`),
			Modified: []byte(`
retainKeysMap:
  name: foo
`),
			TwoWay: []byte(`
retainKeysMap:
  $retainKeys:
    - name
  value: null
`),
			ThreeWay: []byte(`
retainKeysMap:
  $retainKeys:
    - name
  value: null
`),
			Result: []byte(`
retainKeysMap:
  name: foo
`),
		},
	},
	{
		Description: "retainKeys map deletes a field and clears a field",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
retainKeysMap:
  name: foo
  value: bar
`),
			Current: []byte(`
retainKeysMap:
  name: foo
  value: bar
  other: a
`),
			Modified: []byte(`
retainKeysMap:
  name: foo
`),
			TwoWay: []byte(`
retainKeysMap:
  $retainKeys:
    - name
  value: null
`),
			ThreeWay: []byte(`
retainKeysMap:
  $retainKeys:
    - name
  value: null
`),
			Result: []byte(`
retainKeysMap:
  name: foo
`),
		},
	},
	{
		Description: "retainKeys map clears a field",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
retainKeysMap:
  name: foo
  value: bar
`),
			Current: []byte(`
retainKeysMap:
  name: foo
  value: bar
  other: a
`),
			Modified: []byte(`
retainKeysMap:
  name: foo
  value: bar
`),
			TwoWay: []byte(`{}`),
			ThreeWay: []byte(`
retainKeysMap:
  $retainKeys:
    - name
    - value
`),
			Result: []byte(`
retainKeysMap:
  name: foo
  value: bar
`),
		},
	},
	{
		Description: "retainKeys map nested map with no change",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
retainKeysMap:
  name: foo
  simpleMap:
    key1: a
`),
			Current: []byte(`
retainKeysMap:
  name: foo
  simpleMap:
    key1: a
`),
			Modified: []byte(`
retainKeysMap:
  name: foo
  value: bar
  simpleMap:
    key1: a
`),
			TwoWay: []byte(`
retainKeysMap:
  $retainKeys:
    - name
    - simpleMap
    - value
  value: bar
`),
			ThreeWay: []byte(`
retainKeysMap:
  $retainKeys:
    - name
    - simpleMap
    - value
  value: bar
`),
			Result: []byte(`
retainKeysMap:
  name: foo
  value: bar
  simpleMap:
    key1: a
`),
		},
	},
	{
		Description: "retainKeys map adds a field in a nested map",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
retainKeysMap:
  name: foo
  value: bar
  simpleMap:
    key1: a
`),
			Current: []byte(`
retainKeysMap:
  name: foo
  value: bar
  simpleMap:
    key1: a
    key3: c
`),
			Modified: []byte(`
retainKeysMap:
  name: foo
  value: bar
  simpleMap:
    key1: a
    key2: b
`),
			TwoWay: []byte(`
retainKeysMap:
  $retainKeys:
    - name
    - simpleMap
    - value
  simpleMap:
    key2: b
`),
			ThreeWay: []byte(`
retainKeysMap:
  $retainKeys:
    - name
    - simpleMap
    - value
  simpleMap:
    key2: b
`),
			Result: []byte(`
retainKeysMap:
  name: foo
  value: bar
  simpleMap:
    key1: a
    key2: b
    key3: c
`),
		},
	},
	{
		Description: "retainKeys map deletes a field in a nested map",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
retainKeysMap:
  name: foo
  value: bar
  simpleMap:
    key1: a
    key2: b
`),
			Current: []byte(`
retainKeysMap:
  name: foo
  value: bar
  simpleMap:
    key1: a
    key2: b
    key3: c
`),
			Modified: []byte(`
retainKeysMap:
  name: foo
  value: bar
  simpleMap:
    key1: a
`),
			TwoWay: []byte(`
retainKeysMap:
  $retainKeys:
    - name
    - simpleMap
    - value
  simpleMap:
    key2: null
`),
			ThreeWay: []byte(`
retainKeysMap:
  $retainKeys:
    - name
    - simpleMap
    - value
  simpleMap:
    key2: null
`),
			Result: []byte(`
retainKeysMap:
  name: foo
  value: bar
  simpleMap:
    key1: a
    key3: c
`),
		},
	},
	{
		Description: "retainKeys map changes a field in a nested map",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
retainKeysMap:
  name: foo
  value: bar
  simpleMap:
    key1: a
    key2: b
`),
			Current: []byte(`
retainKeysMap:
  name: foo
  value: bar
  simpleMap:
    key1: a
    key2: b
    key3: c
`),
			Modified: []byte(`
retainKeysMap:
  name: foo
  value: bar
  simpleMap:
    key1: x
    key2: b
`),
			TwoWay: []byte(`
retainKeysMap:
  $retainKeys:
    - name
    - simpleMap
    - value
  simpleMap:
    key1: x
`),
			ThreeWay: []byte(`
retainKeysMap:
  $retainKeys:
    - name
    - simpleMap
    - value
  simpleMap:
    key1: x
`),
			Result: []byte(`
retainKeysMap:
  name: foo
  value: bar
  simpleMap:
    key1: x
    key2: b
    key3: c
`),
		},
	},
	{
		Description: "retainKeys map changes a field in a nested map with conflict",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
retainKeysMap:
  name: foo
  value: bar
  simpleMap:
    key1: old
    key2: b
`),
			Current: []byte(`
retainKeysMap:
  name: foo
  value: bar
  simpleMap:
    key1: new
    key2: b
    key3: c
`),
			Modified: []byte(`
retainKeysMap:
  name: foo
  value: bar
  simpleMap:
    key1: modified
    key2: b
`),
			TwoWay: []byte(`
retainKeysMap:
  $retainKeys:
    - name
    - simpleMap
    - value
  simpleMap:
    key1: modified
`),
			ThreeWay: []byte(`
retainKeysMap:
  $retainKeys:
    - name
    - simpleMap
    - value
  simpleMap:
    key1: modified
`),
			Result: []byte(`
retainKeysMap:
  name: foo
  value: bar
  simpleMap:
    key1: modified
    key2: b
    key3: c
`),
		},
	},
	{
		Description: "retainKeys map replaces non-merging list",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
retainKeysMap:
  name: foo
  value: bar
  nonMergingList:
  - name: a
  - name: b
`),
			Current: []byte(`
retainKeysMap:
  name: foo
  value: bar
  nonMergingList:
  - name: a
  - name: b
`),
			Modified: []byte(`
retainKeysMap:
  name: foo
  value: bar
  nonMergingList:
  - name: a
  - name: c
  - name: b
`),
			TwoWay: []byte(`
retainKeysMap:
  $retainKeys:
    - name
    - nonMergingList
    - value
  nonMergingList:
  - name: a
  - name: c
  - name: b
`),
			ThreeWay: []byte(`
retainKeysMap:
  $retainKeys:
    - name
    - nonMergingList
    - value
  nonMergingList:
  - name: a
  - name: c
  - name: b
`),
			Result: []byte(`
retainKeysMap:
  name: foo
  value: bar
  nonMergingList:
  - name: a
  - name: c
  - name: b
`),
		},
	},
	{
		Description: "retainKeys map nested non-merging list with no change",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
retainKeysMap:
  name: foo
  nonMergingList:
  - name: a
  - name: b
`),
			Current: []byte(`
retainKeysMap:
  name: foo
  nonMergingList:
  - name: a
  - name: b
`),
			Modified: []byte(`
retainKeysMap:
  name: foo
  value: bar
  nonMergingList:
  - name: a
  - name: b
`),
			TwoWay: []byte(`
retainKeysMap:
  $retainKeys:
    - name
    - nonMergingList
    - value
  value: bar
`),
			ThreeWay: []byte(`
retainKeysMap:
  $retainKeys:
    - name
    - nonMergingList
    - value
  value: bar
`),
			Result: []byte(`
retainKeysMap:
  name: foo
  value: bar
  nonMergingList:
  - name: a
  - name: b
`),
		},
	},
	{
		Description: "retainKeys map nested non-merging list with no change with conflict",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
retainKeysMap:
  name: foo
  nonMergingList:
  - name: a
  - name: b
`),
			Current: []byte(`
retainKeysMap:
  name: foo
  nonMergingList:
  - name: a
  - name: b
  - name: c
`),
			Modified: []byte(`
retainKeysMap:
  name: foo
  value: bar
  nonMergingList:
  - name: a
  - name: b
`),
			TwoWay: []byte(`
retainKeysMap:
  $retainKeys:
    - name
    - nonMergingList
    - value
  value: bar
`),
			ThreeWay: []byte(`
retainKeysMap:
  $retainKeys:
    - name
    - nonMergingList
    - value
  value: bar
  nonMergingList:
  - name: a
  - name: b
`),
			Result: []byte(`
retainKeysMap:
  name: foo
  value: bar
  nonMergingList:
  - name: a
  - name: b
`),
		},
	},
	{
		Description: "retainKeys map deletes nested non-merging list",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
retainKeysMap:
  name: foo
  nonMergingList:
  - name: a
  - name: b
`),
			Current: []byte(`
retainKeysMap:
  name: foo
  nonMergingList:
  - name: a
  - name: b
`),
			Modified: []byte(`
retainKeysMap:
  name: foo
  value: bar
`),
			TwoWay: []byte(`
retainKeysMap:
  $retainKeys:
    - name
    - value
  value: bar
  nonMergingList: null
`),
			ThreeWay: []byte(`
retainKeysMap:
  $retainKeys:
    - name
    - value
  value: bar
  nonMergingList: null
`),
			Result: []byte(`
retainKeysMap:
  name: foo
  value: bar
`),
		},
	},
	{
		Description: "retainKeys map delete nested non-merging list with conflict",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
retainKeysMap:
  name: foo
  nonMergingList:
  - name: a
  - name: b
`),
			Current: []byte(`
retainKeysMap:
  name: foo
  nonMergingList:
  - name: a
  - name: b
  - name: c
`),
			Modified: []byte(`
retainKeysMap:
  name: foo
  value: bar
`),
			TwoWay: []byte(`
retainKeysMap:
  $retainKeys:
    - name
    - value
  value: bar
  nonMergingList: null
`),
			ThreeWay: []byte(`
retainKeysMap:
  $retainKeys:
    - name
    - value
  value: bar
  nonMergingList: null
`),
			Result: []byte(`
retainKeysMap:
  name: foo
  value: bar
`),
		},
	},
	{
		Description: "retainKeys map nested merging int list with no change",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
retainKeysMap:
  name: foo
  mergingIntList:
  - 1
  - 2
`),
			Current: []byte(`
retainKeysMap:
  name: foo
  mergingIntList:
  - 1
  - 2
  - 3
`),
			Modified: []byte(`
retainKeysMap:
  name: foo
  value: bar
  mergingIntList:
  - 1
  - 2
`),
			TwoWay: []byte(`
retainKeysMap:
  $retainKeys:
    - mergingIntList
    - name
    - value
  value: bar
`),
			ThreeWay: []byte(`
retainKeysMap:
  $retainKeys:
    - mergingIntList
    - name
    - value
  $setElementOrder/mergingIntList:
    - 1
    - 2
  value: bar
`),
			Result: []byte(`
retainKeysMap:
  name: foo
  value: bar
  mergingIntList:
  - 1
  - 2
  - 3
`),
		},
	},
	{
		Description: "retainKeys map adds an item in nested merging int list",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
retainKeysMap:
  name: foo
  mergingIntList:
  - 1
  - 2
`),
			Current: []byte(`
retainKeysMap:
  name: foo
  mergingIntList:
  - 1
  - 2
  - 3
`),
			Modified: []byte(`
retainKeysMap:
  name: foo
  mergingIntList:
  - 1
  - 2
  - 4
`),
			TwoWay: []byte(`
retainKeysMap:
  $setElementOrder/mergingIntList:
    - 1
    - 2
    - 4
  $retainKeys:
    - mergingIntList
    - name
  mergingIntList:
  - 4
`),
			ThreeWay: []byte(`
retainKeysMap:
  $setElementOrder/mergingIntList:
    - 1
    - 2
    - 4
  $retainKeys:
    - mergingIntList
    - name
  mergingIntList:
  - 4
`),
			Result: []byte(`
retainKeysMap:
  name: foo
  mergingIntList:
  - 1
  - 2
  - 4
  - 3
`),
		},
	},
	{
		Description: "retainKeys map deletes an item in nested merging int list",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
retainKeysMap:
  name: foo
  mergingIntList:
  - 1
  - 2
  - 3
`),
			Current: []byte(`
retainKeysMap:
  name: foo
  mergingIntList:
  - 1
  - 2
  - 3
  - 4
`),
			Modified: []byte(`
retainKeysMap:
  name: foo
  mergingIntList:
  - 1
  - 3
`),
			TwoWay: []byte(`
retainKeysMap:
  $retainKeys:
    - mergingIntList
    - name
  $deleteFromPrimitiveList/mergingIntList:
  - 2
  $setElementOrder/mergingIntList:
    - 1
    - 3
`),
			ThreeWay: []byte(`
retainKeysMap:
  $retainKeys:
    - mergingIntList
    - name
  $deleteFromPrimitiveList/mergingIntList:
  - 2
  $setElementOrder/mergingIntList:
    - 1
    - 3
`),
			Result: []byte(`
retainKeysMap:
  name: foo
  mergingIntList:
  - 1
  - 3
  - 4
`),
		},
	},
	{
		Description: "retainKeys map adds an item and deletes an item in nested merging int list",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
retainKeysMap:
  name: foo
  mergingIntList:
  - 1
  - 2
  - 3
`),
			Current: []byte(`
retainKeysMap:
  name: foo
  mergingIntList:
  - 1
  - 2
  - 3
  - 4
`),
			Modified: []byte(`
retainKeysMap:
  name: foo
  mergingIntList:
  - 1
  - 3
  - 5
`),
			TwoWay: []byte(`
retainKeysMap:
  $retainKeys:
    - mergingIntList
    - name
  mergingIntList:
  - 5
  $deleteFromPrimitiveList/mergingIntList:
  - 2
  $setElementOrder/mergingIntList:
    - 1
    - 3
    - 5
`),
			ThreeWay: []byte(`
retainKeysMap:
  $retainKeys:
    - mergingIntList
    - name
  mergingIntList:
  - 5
  $deleteFromPrimitiveList/mergingIntList:
  - 2
  $setElementOrder/mergingIntList:
    - 1
    - 3
    - 5
`),
			Result: []byte(`
retainKeysMap:
  name: foo
  mergingIntList:
  - 1
  - 3
  - 5
  - 4
`),
		},
	},
	{
		Description: "retainKeys map deletes nested merging int list",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
retainKeysMap:
  name: foo
  mergingIntList:
  - 1
  - 2
  - 3
`),
			Current: []byte(`
retainKeysMap:
  name: foo
  mergingIntList:
  - 1
  - 2
  - 3
`),
			Modified: []byte(`
retainKeysMap:
  name: foo
`),
			TwoWay: []byte(`
retainKeysMap:
  $retainKeys:
    - name
  mergingIntList: null
`),
			ThreeWay: []byte(`
retainKeysMap:
  $retainKeys:
    - name
  mergingIntList: null
`),
			Result: []byte(`
retainKeysMap:
  name: foo
`),
		},
	},
	{
		Description: "retainKeys map nested merging list with no change",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
retainKeysMap:
  name: foo
  mergingList:
  - name: a
  - name: b
`),
			Current: []byte(`
retainKeysMap:
  name: foo
  mergingList:
  - name: a
  - name: b
  - name: c
`),
			Modified: []byte(`
retainKeysMap:
  name: foo
  value: bar
  mergingList:
  - name: a
  - name: b
`),
			TwoWay: []byte(`
retainKeysMap:
  $retainKeys:
    - mergingList
    - name
    - value
  value: bar
`),
			ThreeWay: []byte(`
retainKeysMap:
  $retainKeys:
    - mergingList
    - name
    - value
  $setElementOrder/mergingList:
    - name: a
    - name: b
  value: bar
`),
			Result: []byte(`
retainKeysMap:
  name: foo
  value: bar
  mergingList:
  - name: a
  - name: b
  - name: c
`),
		},
	},
	{
		Description: "retainKeys map adds an item in nested merging list",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
retainKeysMap:
  name: foo
  mergingList:
  - name: a
  - name: b
`),
			Current: []byte(`
retainKeysMap:
  name: foo
  mergingList:
  - name: a
  - name: b
  - name: x
`),
			Modified: []byte(`
retainKeysMap:
  name: foo
  mergingList:
  - name: a
  - name: b
  - name: c
`),
			TwoWay: []byte(`
retainKeysMap:
  $retainKeys:
    - mergingList
    - name
  $setElementOrder/mergingList:
    - name: a
    - name: b
    - name: c
  mergingList:
  - name: c
`),
			ThreeWay: []byte(`
retainKeysMap:
  $retainKeys:
    - mergingList
    - name
  $setElementOrder/mergingList:
    - name: a
    - name: b
    - name: c
  mergingList:
  - name: c
`),
			Result: []byte(`
retainKeysMap:
  name: foo
  mergingList:
  - name: a
  - name: b
  - name: c
  - name: x
`),
		},
	},
	{
		Description: "retainKeys map changes an item in nested merging list",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
retainKeysMap:
  name: foo
  mergingList:
  - name: a
  - name: b
    value: foo
`),
			Current: []byte(`
retainKeysMap:
  name: foo
  mergingList:
  - name: a
  - name: b
    value: foo
  - name: x
`),
			Modified: []byte(`
retainKeysMap:
  name: foo
  mergingList:
  - name: a
  - name: b
    value: bar
`),
			TwoWay: []byte(`
retainKeysMap:
  $retainKeys:
    - mergingList
    - name
  $setElementOrder/mergingList:
    - name: a
    - name: b
  mergingList:
  - name: b
    value: bar
`),
			ThreeWay: []byte(`
retainKeysMap:
  $retainKeys:
    - mergingList
    - name
  $setElementOrder/mergingList:
    - name: a
    - name: b
  mergingList:
  - name: b
    value: bar
`),
			Result: []byte(`
retainKeysMap:
  name: foo
  mergingList:
  - name: a
  - name: b
    value: bar
  - name: x
`),
		},
	},
	{
		Description: "retainKeys map deletes nested merging list",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
retainKeysMap:
  name: foo
  mergingList:
  - name: a
  - name: b
`),
			Current: []byte(`
retainKeysMap:
  name: foo
  mergingList:
  - name: a
  - name: b
`),
			Modified: []byte(`
retainKeysMap:
  name: foo
  value: bar
`),
			TwoWay: []byte(`
retainKeysMap:
  $retainKeys:
    - name
    - value
  value: bar
  mergingList: null
`),
			ThreeWay: []byte(`
retainKeysMap:
  $retainKeys:
    - name
    - value
  value: bar
  mergingList: null
`),
			Result: []byte(`
retainKeysMap:
  name: foo
  value: bar
`),
		},
	},
	{
		Description: "retainKeys map deletes an item in nested merging list",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
retainKeysMap:
  name: foo
  mergingList:
  - name: a
  - name: b
`),
			Current: []byte(`
retainKeysMap:
  name: foo
  mergingList:
  - name: a
  - name: b
  - name: x
`),
			Modified: []byte(`
retainKeysMap:
  name: foo
  mergingList:
  - name: a
`),
			TwoWay: []byte(`
retainKeysMap:
  $retainKeys:
    - mergingList
    - name
  $setElementOrder/mergingList:
    - name: a
  mergingList:
  - name: b
    $patch: delete
`),
			ThreeWay: []byte(`
retainKeysMap:
  $retainKeys:
    - mergingList
    - name
  $setElementOrder/mergingList:
    - name: a
  mergingList:
  - name: b
    $patch: delete
`),
			Result: []byte(`
retainKeysMap:
  name: foo
  mergingList:
  - name: a
  - name: x
`),
		},
	},
	{
		Description: "retainKeys list of maps clears a field",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
retainKeysMergingList:
- name: bar
- name: foo
  value: a
`),
			Current: []byte(`
retainKeysMergingList:
- name: bar
- name: foo
  value: a
  other: x
`),
			Modified: []byte(`
retainKeysMergingList:
- name: bar
- name: foo
  value: a
`),
			TwoWay: []byte(`{}`),
			ThreeWay: []byte(`
$setElementOrder/retainKeysMergingList:
  - name: bar
  - name: foo
retainKeysMergingList:
- $retainKeys:
    - name
    - value
  name: foo
`),
			Result: []byte(`
retainKeysMergingList:
- name: bar
- name: foo
  value: a
`),
		},
	},
	{
		Description: "retainKeys list of maps clears a field with conflict",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
retainKeysMergingList:
- name: bar
- name: foo
  value: old
`),
			Current: []byte(`
retainKeysMergingList:
- name: bar
- name: foo
  value: new
  other: x
`),
			Modified: []byte(`
retainKeysMergingList:
- name: bar
- name: foo
  value: modified
`),
			TwoWay: []byte(`
$setElementOrder/retainKeysMergingList:
  - name: bar
  - name: foo
retainKeysMergingList:
- $retainKeys:
    - name
    - value
  name: foo
  value: modified
`),
			ThreeWay: []byte(`
$setElementOrder/retainKeysMergingList:
  - name: bar
  - name: foo
retainKeysMergingList:
- $retainKeys:
    - name
    - value
  name: foo
  value: modified
`),
			Result: []byte(`
retainKeysMergingList:
- name: bar
- name: foo
  value: modified
`),
		},
	},
	{
		Description: "retainKeys list of maps changes a field and clear a field",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
retainKeysMergingList:
- name: bar
- name: foo
  value: old
`),
			Current: []byte(`
retainKeysMergingList:
- name: bar
- name: foo
  value: old
  other: x
`),
			Modified: []byte(`
retainKeysMergingList:
- name: bar
- name: foo
  value: new
`),
			TwoWay: []byte(`
$setElementOrder/retainKeysMergingList:
  - name: bar
  - name: foo
retainKeysMergingList:
- $retainKeys:
    - name
    - value
  name: foo
  value: new
`),
			ThreeWay: []byte(`
$setElementOrder/retainKeysMergingList:
  - name: bar
  - name: foo
retainKeysMergingList:
- $retainKeys:
    - name
    - value
  name: foo
  value: new
`),
			Result: []byte(`
retainKeysMergingList:
- name: bar
- name: foo
  value: new
`),
		},
	},
	{
		Description: "retainKeys list of maps changes a field and clear a field with conflict",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
retainKeysMergingList:
- name: bar
- name: foo
  value: old
`),
			Current: []byte(`
retainKeysMergingList:
- name: bar
- name: foo
  value: modified
  other: x
`),
			Modified: []byte(`
retainKeysMergingList:
- name: bar
- name: foo
  value: new
`),
			TwoWay: []byte(`
$setElementOrder/retainKeysMergingList:
  - name: bar
  - name: foo
retainKeysMergingList:
- $retainKeys:
    - name
    - value
  name: foo
  value: new
`),
			ThreeWay: []byte(`
$setElementOrder/retainKeysMergingList:
  - name: bar
  - name: foo
retainKeysMergingList:
- $retainKeys:
    - name
    - value
  name: foo
  value: new
`),
			Result: []byte(`
retainKeysMergingList:
- name: bar
- name: foo
  value: new
`),
		},
	},
	{
		Description: "retainKeys list of maps adds a field",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
retainKeysMergingList:
- name: bar
- name: foo
`),
			Current: []byte(`
retainKeysMergingList:
- name: bar
- name: foo
`),
			Modified: []byte(`
retainKeysMergingList:
- name: bar
- name: foo
  value: a
`),
			TwoWay: []byte(`
$setElementOrder/retainKeysMergingList:
  - name: bar
  - name: foo
retainKeysMergingList:
- $retainKeys:
    - name
    - value
  name: foo
  value: a
`),
			ThreeWay: []byte(`
$setElementOrder/retainKeysMergingList:
  - name: bar
  - name: foo
retainKeysMergingList:
- $retainKeys:
    - name
    - value
  name: foo
  value: a
`),
			Result: []byte(`
retainKeysMergingList:
- name: bar
- name: foo
  value: a
`),
		},
	},
	{
		Description: "retainKeys list of maps adds a field and clear a field",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
retainKeysMergingList:
- name: bar
- name: foo
`),
			Current: []byte(`
retainKeysMergingList:
- name: bar
- name: foo
  other: x
`),
			Modified: []byte(`
retainKeysMergingList:
- name: bar
- name: foo
  value: a
`),
			TwoWay: []byte(`
$setElementOrder/retainKeysMergingList:
  - name: bar
  - name: foo
retainKeysMergingList:
- $retainKeys:
    - name
    - value
  name: foo
  value: a
`),
			ThreeWay: []byte(`
$setElementOrder/retainKeysMergingList:
  - name: bar
  - name: foo
retainKeysMergingList:
- $retainKeys:
    - name
    - value
  name: foo
  value: a
`),
			Result: []byte(`
retainKeysMergingList:
- name: bar
- name: foo
  value: a
`),
		},
	},
	{
		Description: "retainKeys list of maps deletes a field",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
retainKeysMergingList:
- name: bar
- name: foo
  value: a
`),
			Current: []byte(`
retainKeysMergingList:
- name: bar
- name: foo
  value: a
`),
			Modified: []byte(`
retainKeysMergingList:
- name: bar
- name: foo
`),
			TwoWay: []byte(`
$setElementOrder/retainKeysMergingList:
  - name: bar
  - name: foo
retainKeysMergingList:
- $retainKeys:
    - name
  name: foo
  value: null
`),
			ThreeWay: []byte(`
$setElementOrder/retainKeysMergingList:
  - name: bar
  - name: foo
retainKeysMergingList:
- $retainKeys:
    - name
  name: foo
  value: null
`),
			Result: []byte(`
retainKeysMergingList:
- name: bar
- name: foo
`),
		},
	},
	{
		Description: "retainKeys list of maps deletes a field and clear a field",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
retainKeysMergingList:
- name: bar
- name: foo
  value: a
`),
			Current: []byte(`
retainKeysMergingList:
- name: bar
- name: foo
  value: a
  other: x
`),
			Modified: []byte(`
retainKeysMergingList:
- name: bar
- name: foo
`),
			TwoWay: []byte(`
$setElementOrder/retainKeysMergingList:
  - name: bar
  - name: foo
retainKeysMergingList:
- $retainKeys:
    - name
  name: foo
  value: null
`),
			ThreeWay: []byte(`
$setElementOrder/retainKeysMergingList:
  - name: bar
  - name: foo
retainKeysMergingList:
- $retainKeys:
    - name
  name: foo
  value: null
`),
			Result: []byte(`
retainKeysMergingList:
- name: bar
- name: foo
`),
		},
	},
	{
		Description: "delete and reorder in one list, reorder in another",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
mergingList:
- name: a
  value: a
- name: b
  value: b
mergeItemPtr:
- name: c
  value: c
- name: d
  value: d
`),
			Current: []byte(`
mergingList:
- name: a
  value: a
- name: b
  value: b
mergeItemPtr:
- name: c
  value: c
- name: d
  value: d
`),
			Modified: []byte(`
mergingList:
- name: b
  value: b
mergeItemPtr:
- name: d
  value: d
- name: c
  value: c
`),
			TwoWay: []byte(`
$setElementOrder/mergingList:
- name: b
$setElementOrder/mergeItemPtr:
- name: d
- name: c
mergingList:
- $patch: delete
  name: a
`),
			ThreeWay: []byte(`
$setElementOrder/mergingList:
- name: b
$setElementOrder/mergeItemPtr:
- name: d
- name: c
mergingList:
- $patch: delete
  name: a
`),
			Result: []byte(`
mergingList:
- name: b
  value: b
mergeItemPtr:
- name: d
  value: d
- name: c
  value: c
`),
		},
	},
}

func TestStrategicMergePatch(t *testing.T) {
	testStrategicMergePatchWithCustomArgumentsUsingStruct(t, "bad struct",
		"{}", "{}", []byte("<THIS IS NOT A STRUCT>"), mergepatch.ErrBadArgKind(struct{}{}, []byte{}))

	mergeItemOpenapiSchema := PatchMetaFromOpenAPI{
		Schema: sptest.GetSchemaOrDie(&fakeMergeItemSchema, "mergeItem"),
	}
	mergeItemOpenapiV3Schema := PatchMetaFromOpenAPIV3{
		SchemaList: fakeMergeItemV3Schema.SchemaOrDie().Components.Schemas,
		Schema:     fakeMergeItemV3Schema.SchemaOrDie().Components.Schemas["mergeItem"],
	}
	schemas := []LookupPatchMeta{
		mergeItemStructSchema,
		mergeItemOpenapiSchema,
		mergeItemOpenapiV3Schema,
	}

	tc := StrategicMergePatchTestCases{}
	err := yaml.Unmarshal(createStrategicMergePatchTestCaseData, &tc)
	if err != nil {
		t.Errorf("can't unmarshal test cases: %s\n", err)
		return
	}

	for _, schema := range schemas {
		t.Run(schema.Name(), func(t *testing.T) {

			testStrategicMergePatchWithCustomArguments(t, "bad original",
				"<THIS IS NOT JSON>", "{}", schema, mergepatch.ErrBadJSONDoc)
			testStrategicMergePatchWithCustomArguments(t, "bad patch",
				"{}", "<THIS IS NOT JSON>", schema, mergepatch.ErrBadJSONDoc)
			testStrategicMergePatchWithCustomArguments(t, "nil struct",
				"{}", "{}", nil, mergepatch.ErrBadArgKind(struct{}{}, nil))

			for _, c := range tc.TestCases {
				t.Run(c.Description+"/TwoWay", func(t *testing.T) {
					testTwoWayPatch(t, c, schema)
				})
				t.Run(c.Description+"/ThreeWay", func(t *testing.T) {
					testThreeWayPatch(t, c, schema)
				})
			}
		})

		// run multiple times to exercise different map traversal orders
		for i := 0; i < 10; i++ {
			for _, c := range strategicMergePatchRawTestCases {
				t.Run(c.Description+"/TwoWay", func(t *testing.T) {
					testTwoWayPatchForRawTestCase(t, c, schema)
				})
				t.Run(c.Description+"/ThreeWay", func(t *testing.T) {
					testThreeWayPatchForRawTestCase(t, c, schema)
				})
			}
		}
	}
}

func testStrategicMergePatchWithCustomArgumentsUsingStruct(t *testing.T, description, original, patch string, dataStruct interface{}, expected error) {
	schema, actual := NewPatchMetaFromStruct(dataStruct)
	// If actual is not nil, check error. If errors match, return.
	if actual != nil {
		checkErrorsEqual(t, description, expected, actual, schema)
		return
	}
	testStrategicMergePatchWithCustomArguments(t, description, original, patch, schema, expected)
}

func testStrategicMergePatchWithCustomArguments(t *testing.T, description, original, patch string, schema LookupPatchMeta, expected error) {
	_, actual := StrategicMergePatch([]byte(original), []byte(patch), schema)
	checkErrorsEqual(t, description, expected, actual, schema)
}

func checkErrorsEqual(t *testing.T, description string, expected, actual error, schema LookupPatchMeta) {
	if actual != expected {
		if actual == nil {
			t.Errorf("using %s expected error: %s\ndid not occur in test case: %s", getSchemaType(schema), expected, description)
			return
		}

		if expected == nil || actual.Error() != expected.Error() {
			t.Errorf("using %s unexpected error: %s\noccurred in test case: %s", getSchemaType(schema), actual, description)
			return
		}
	}
}

func testTwoWayPatch(t *testing.T, c StrategicMergePatchTestCase, schema LookupPatchMeta) {
	original, expectedPatch, modified, expectedResult := twoWayTestCaseToJSONOrFail(t, c, schema)

	actualPatch, err := CreateTwoWayMergePatchUsingLookupPatchMeta(original, modified, schema)
	if err != nil {
		t.Errorf("using %s error: %s\nin test case: %s\ncannot create two way patch: %s:\n%s\n",
			getSchemaType(schema), err, c.Description, original, mergepatch.ToYAMLOrError(c.StrategicMergePatchTestCaseData))
		return
	}

	testPatchCreation(t, expectedPatch, actualPatch, c.Description)
	testPatchApplication(t, original, actualPatch, expectedResult, c.Description, "", schema)
}

func testTwoWayPatchForRawTestCase(t *testing.T, c StrategicMergePatchRawTestCase, schema LookupPatchMeta) {
	original, expectedPatch, modified, expectedResult := twoWayRawTestCaseToJSONOrFail(t, c)

	actualPatch, err := CreateTwoWayMergePatchUsingLookupPatchMeta(original, modified, schema)
	if err != nil {
		t.Errorf("error: %s\nin test case: %s\ncannot create two way patch:\noriginal:%s\ntwoWay:%s\nmodified:%s\ncurrent:%s\nthreeWay:%s\nresult:%s\n",
			err, c.Description, c.Original, c.TwoWay, c.Modified, c.Current, c.ThreeWay, c.Result)
		return
	}

	testPatchCreation(t, expectedPatch, actualPatch, c.Description)
	testPatchApplication(t, original, actualPatch, expectedResult, c.Description, c.ExpectedError, schema)
}

func twoWayTestCaseToJSONOrFail(t *testing.T, c StrategicMergePatchTestCase, schema LookupPatchMeta) ([]byte, []byte, []byte, []byte) {
	expectedResult := c.TwoWayResult
	if expectedResult == nil {
		expectedResult = c.Modified
	}
	return sortJsonOrFail(t, testObjectToJSONOrFail(t, c.Original), c.Description, schema),
		sortJsonOrFail(t, testObjectToJSONOrFail(t, c.TwoWay), c.Description, schema),
		sortJsonOrFail(t, testObjectToJSONOrFail(t, c.Modified), c.Description, schema),
		sortJsonOrFail(t, testObjectToJSONOrFail(t, expectedResult), c.Description, schema)
}

func twoWayRawTestCaseToJSONOrFail(t *testing.T, c StrategicMergePatchRawTestCase) ([]byte, []byte, []byte, []byte) {
	expectedResult := c.TwoWayResult
	if expectedResult == nil {
		expectedResult = c.Modified
	}
	return yamlToJSONOrError(t, c.Original),
		yamlToJSONOrError(t, c.TwoWay),
		yamlToJSONOrError(t, c.Modified),
		yamlToJSONOrError(t, expectedResult)
}

func testThreeWayPatch(t *testing.T, c StrategicMergePatchTestCase, schema LookupPatchMeta) {
	original, modified, current, expected, result := threeWayTestCaseToJSONOrFail(t, c, schema)
	actual, err := CreateThreeWayMergePatch(original, modified, current, schema, false)
	if err != nil {
		if !mergepatch.IsConflict(err) {
			t.Errorf("using %s error: %s\nin test case: %s\ncannot create three way patch:\n%s\n",
				getSchemaType(schema), err, c.Description, mergepatch.ToYAMLOrError(c.StrategicMergePatchTestCaseData))
			return
		}

		if !strings.Contains(c.Description, "conflict") {
			t.Errorf("using %s unexpected conflict: %s\nin test case: %s\ncannot create three way patch:\n%s\n",
				getSchemaType(schema), err, c.Description, mergepatch.ToYAMLOrError(c.StrategicMergePatchTestCaseData))
			return
		}

		if len(c.Result) > 0 {
			actual, err := CreateThreeWayMergePatch(original, modified, current, schema, true)
			if err != nil {
				t.Errorf("using %s error: %s\nin test case: %s\ncannot force three way patch application:\n%s\n",
					getSchemaType(schema), err, c.Description, mergepatch.ToYAMLOrError(c.StrategicMergePatchTestCaseData))
				return
			}

			testPatchCreation(t, expected, actual, c.Description)
			testPatchApplication(t, current, actual, result, c.Description, "", schema)
		}

		return
	}

	if strings.Contains(c.Description, "conflict") || len(c.Result) < 1 {
		t.Errorf("using %s error in test case: %s\nexpected conflict did not occur:\n%s\n",
			getSchemaType(schema), c.Description, mergepatch.ToYAMLOrError(c.StrategicMergePatchTestCaseData))
		return
	}

	testPatchCreation(t, expected, actual, c.Description)
	testPatchApplication(t, current, actual, result, c.Description, "", schema)
}

func testThreeWayPatchForRawTestCase(t *testing.T, c StrategicMergePatchRawTestCase, schema LookupPatchMeta) {
	original, modified, current, expected, result := threeWayRawTestCaseToJSONOrFail(t, c)
	actual, err := CreateThreeWayMergePatch(original, modified, current, schema, false)
	if err != nil {
		if !mergepatch.IsConflict(err) {
			t.Errorf("using %s error: %s\nin test case: %s\ncannot create three way patch:\noriginal:%s\ntwoWay:%s\nmodified:%s\ncurrent:%s\nthreeWay:%s\nresult:%s\n",
				getSchemaType(schema), err, c.Description, c.Original, c.TwoWay, c.Modified, c.Current, c.ThreeWay, c.Result)
			return
		}

		if !strings.Contains(c.Description, "conflict") {
			t.Errorf("using %s unexpected conflict: %s\nin test case: %s\ncannot create three way patch:\noriginal:%s\ntwoWay:%s\nmodified:%s\ncurrent:%s\nthreeWay:%s\nresult:%s\n",
				getSchemaType(schema), err, c.Description, c.Original, c.TwoWay, c.Modified, c.Current, c.ThreeWay, c.Result)
			return
		}

		if len(c.Result) > 0 {
			actual, err := CreateThreeWayMergePatch(original, modified, current, schema, true)
			if err != nil {
				t.Errorf("using %s error: %s\nin test case: %s\ncannot force three way patch application:\noriginal:%s\ntwoWay:%s\nmodified:%s\ncurrent:%s\nthreeWay:%s\nresult:%s\n",
					getSchemaType(schema), err, c.Description, c.Original, c.TwoWay, c.Modified, c.Current, c.ThreeWay, c.Result)
				return
			}

			testPatchCreation(t, expected, actual, c.Description)
			testPatchApplication(t, current, actual, result, c.Description, c.ExpectedError, schema)
		}

		return
	}

	if strings.Contains(c.Description, "conflict") || len(c.Result) < 1 {
		t.Errorf("using %s error: %s\nin test case: %s\nexpected conflict did not occur:\noriginal:%s\ntwoWay:%s\nmodified:%s\ncurrent:%s\nthreeWay:%s\nresult:%s\n",
			getSchemaType(schema), err, c.Description, c.Original, c.TwoWay, c.Modified, c.Current, c.ThreeWay, c.Result)
		return
	}

	testPatchCreation(t, expected, actual, c.Description)
	testPatchApplication(t, current, actual, result, c.Description, c.ExpectedError, schema)
}

func threeWayTestCaseToJSONOrFail(t *testing.T, c StrategicMergePatchTestCase, schema LookupPatchMeta) ([]byte, []byte, []byte, []byte, []byte) {
	return sortJsonOrFail(t, testObjectToJSONOrFail(t, c.Original), c.Description, schema),
		sortJsonOrFail(t, testObjectToJSONOrFail(t, c.Modified), c.Description, schema),
		sortJsonOrFail(t, testObjectToJSONOrFail(t, c.Current), c.Description, schema),
		sortJsonOrFail(t, testObjectToJSONOrFail(t, c.ThreeWay), c.Description, schema),
		sortJsonOrFail(t, testObjectToJSONOrFail(t, c.Result), c.Description, schema)
}

func threeWayRawTestCaseToJSONOrFail(t *testing.T, c StrategicMergePatchRawTestCase) ([]byte, []byte, []byte, []byte, []byte) {
	return yamlToJSONOrError(t, c.Original),
		yamlToJSONOrError(t, c.Modified),
		yamlToJSONOrError(t, c.Current),
		yamlToJSONOrError(t, c.ThreeWay),
		yamlToJSONOrError(t, c.Result)
}

func testPatchCreation(t *testing.T, expected, actual []byte, description string) {
	if !reflect.DeepEqual(actual, expected) {
		t.Errorf("error in test case: %s\nexpected patch:\n%s\ngot:\n%s\n",
			description, jsonToYAMLOrError(expected), jsonToYAMLOrError(actual))
		return
	}
}

func testPatchApplication(t *testing.T, original, patch, expected []byte, description, expectedError string, schema LookupPatchMeta) {
	result, err := StrategicMergePatchUsingLookupPatchMeta(original, patch, schema)
	if len(expectedError) != 0 {
		if err != nil && strings.Contains(err.Error(), expectedError) {
			return
		}
		t.Errorf("using %s expected error should contain:\n%s\nin test case: %s\nbut got:\n%s\n", getSchemaType(schema), expectedError, description, err)
	}
	if err != nil {
		t.Errorf("using %s error: %s\nin test case: %s\ncannot apply patch:\n%s\nto original:\n%s\n",
			getSchemaType(schema), err, description, jsonToYAMLOrError(patch), jsonToYAMLOrError(original))
		return
	}

	if !reflect.DeepEqual(result, expected) {
		format := "using error in test case: %s\npatch application failed:\noriginal:\n%s\npatch:\n%s\nexpected:\n%s\ngot:\n%s\n"
		t.Errorf(format, description,
			jsonToYAMLOrError(original), jsonToYAMLOrError(patch),
			jsonToYAMLOrError(expected), jsonToYAMLOrError(result))
		return
	}
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

func sortJsonOrFail(t *testing.T, j []byte, description string, schema LookupPatchMeta) []byte {
	if j == nil {
		return nil
	}
	r, err := sortMergeListsByName(j, schema)
	if err != nil {
		t.Errorf("using %s error: %s\n in test case: %s\ncannot sort object:\n%s\n", getSchemaType(schema), err, description, j)
		return nil
	}

	return r
}

func getSchemaType(schema LookupPatchMeta) string {
	return reflect.TypeOf(schema).String()
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

func yamlToJSON(y []byte) ([]byte, error) {
	j, err := yaml.YAMLToJSON(y)
	if err != nil {
		return nil, fmt.Errorf("yaml to json failed: %v\n%v\n", err, y)
	}

	return j, nil
}

func yamlToJSONOrError(t *testing.T, y []byte) []byte {
	j, err := yamlToJSON(y)
	if err != nil {
		t.Errorf("%v", err)
	}

	return j
}

type PrecisionItem struct {
	Name    string  `json:"name,omitempty"`
	Int32   int32   `json:"int32,omitempty"`
	Int64   int64   `json:"int64,omitempty"`
	Float32 float32 `json:"float32,omitempty"`
	Float64 float64 `json:"float64,omitempty"`
}

var (
	precisionItem             PrecisionItem
	precisionItemStructSchema = PatchMetaFromStruct{T: GetTagStructTypeOrDie(precisionItem)}
)

func TestNumberConversion(t *testing.T) {
	testcases := map[string]struct {
		Old            string
		New            string
		ExpectedPatch  string
		ExpectedResult string
	}{
		"empty": {
			Old:            `{}`,
			New:            `{}`,
			ExpectedPatch:  `{}`,
			ExpectedResult: `{}`,
		},
		"int32 medium": {
			Old:            `{"int32":1000000}`,
			New:            `{"int32":1000000,"name":"newname"}`,
			ExpectedPatch:  `{"name":"newname"}`,
			ExpectedResult: `{"int32":1000000,"name":"newname"}`,
		},
		"int32 max": {
			Old:            `{"int32":2147483647}`,
			New:            `{"int32":2147483647,"name":"newname"}`,
			ExpectedPatch:  `{"name":"newname"}`,
			ExpectedResult: `{"int32":2147483647,"name":"newname"}`,
		},
		"int64 medium": {
			Old:            `{"int64":1000000}`,
			New:            `{"int64":1000000,"name":"newname"}`,
			ExpectedPatch:  `{"name":"newname"}`,
			ExpectedResult: `{"int64":1000000,"name":"newname"}`,
		},
		"int64 max": {
			Old:            `{"int64":9223372036854775807}`,
			New:            `{"int64":9223372036854775807,"name":"newname"}`,
			ExpectedPatch:  `{"name":"newname"}`,
			ExpectedResult: `{"int64":9223372036854775807,"name":"newname"}`,
		},
		"float32 max": {
			Old:            `{"float32":3.4028234663852886e+38}`,
			New:            `{"float32":3.4028234663852886e+38,"name":"newname"}`,
			ExpectedPatch:  `{"name":"newname"}`,
			ExpectedResult: `{"float32":3.4028234663852886e+38,"name":"newname"}`,
		},
		"float64 max": {
			Old:            `{"float64":1.7976931348623157e+308}`,
			New:            `{"float64":1.7976931348623157e+308,"name":"newname"}`,
			ExpectedPatch:  `{"name":"newname"}`,
			ExpectedResult: `{"float64":1.7976931348623157e+308,"name":"newname"}`,
		},
	}

	precisionItemOpenapiSchema := PatchMetaFromOpenAPI{
		Schema: sptest.GetSchemaOrDie(&fakePrecisionItemSchema, "precisionItem"),
	}
	precisionItemOpenapiV3Schema := PatchMetaFromOpenAPIV3{
		SchemaList: fakePrecisionItemV3Schema.SchemaOrDie().Components.Schemas,
		Schema:     fakePrecisionItemV3Schema.SchemaOrDie().Components.Schemas["precisionItem"],
	}
	precisionItemSchemas := []LookupPatchMeta{
		precisionItemStructSchema,
		precisionItemOpenapiSchema,
		precisionItemOpenapiV3Schema,
	}

	for _, schema := range precisionItemSchemas {
		for k, tc := range testcases {
			patch, err := CreateTwoWayMergePatchUsingLookupPatchMeta([]byte(tc.Old), []byte(tc.New), schema)
			if err != nil {
				t.Errorf("using %s in testcase %s: unexpected error %v", getSchemaType(schema), k, err)
				continue
			}
			if tc.ExpectedPatch != string(patch) {
				t.Errorf("using %s in testcase %s: expected %s, got %s", getSchemaType(schema), k, tc.ExpectedPatch, string(patch))
				continue
			}

			result, err := StrategicMergePatchUsingLookupPatchMeta([]byte(tc.Old), patch, schema)
			if err != nil {
				t.Errorf("using %s in testcase %s: unexpected error %v", getSchemaType(schema), k, err)
				continue
			}
			if tc.ExpectedResult != string(result) {
				t.Errorf("using %s in testcase %s: expected %s, got %s", getSchemaType(schema), k, tc.ExpectedResult, string(result))
				continue
			}
		}
	}
}

var replaceRawExtensionPatchTestCases = []StrategicMergePatchRawTestCase{
	{
		Description: "replace RawExtension field, rest unchanched",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
name: my-object
value: some-value
other: current-other
replacingItem:
  Some: Generic
  Yaml: Inside
  The: RawExtension
  Field: Period
`),
			Current: []byte(`
name: my-object
value: some-value
other: current-other
mergingList:
  - name: 1
  - name: 2
  - name: 3
replacingItem:
  Some: Generic
  Yaml: Inside
  The: RawExtension
  Field: Period
`),
			Modified: []byte(`
name: my-object
value: some-value
other: current-other
mergingList:
  - name: 1
  - name: 2
  - name: 3
replacingItem:
  Newly: Modified
  Yaml: Inside
  The: RawExtension
`),
			TwoWay: []byte(`
mergingList:
  - name: 1
  - name: 2
  - name: 3
replacingItem:
  Newly: Modified
  Yaml: Inside
  The: RawExtension
`),
			TwoWayResult: []byte(`
name: my-object
value: some-value
other: current-other
mergingList:
  - name: 1
  - name: 2
  - name: 3
replacingItem:
  Newly: Modified
  Yaml: Inside
  The: RawExtension
`),
			ThreeWay: []byte(`
replacingItem:
  Newly: Modified
  Yaml: Inside
  The: RawExtension
`),
			Result: []byte(`
name: my-object
value: some-value
other: current-other
mergingList:
  - name: 1
  - name: 2
  - name: 3
replacingItem:
  Newly: Modified
  Yaml: Inside
  The: RawExtension
`),
		},
	},
	{
		Description: "replace RawExtension field and merge list",
		StrategicMergePatchRawTestCaseData: StrategicMergePatchRawTestCaseData{
			Original: []byte(`
name: my-object
value: some-value
other: current-other
mergingList:
  - name: 1
replacingItem:
  Some: Generic
  Yaml: Inside
  The: RawExtension
  Field: Period
`),
			Current: []byte(`
name: my-object
value: some-value
other: current-other
mergingList:
  - name: 1
  - name: 3
replacingItem:
  Some: Generic
  Yaml: Inside
  The: RawExtension
  Field: Period
`),
			Modified: []byte(`
name: my-object
value: some-value
other: current-other
mergingList:
  - name: 1
  - name: 2
replacingItem:
  Newly: Modified
  Yaml: Inside
  The: RawExtension
`),
			TwoWay: []byte(`
$setElementOrder/mergingList:
  - name: 1
  - name: 2
mergingList:
  - name: 2
replacingItem:
  Newly: Modified
  Yaml: Inside
  The: RawExtension
`),
			TwoWayResult: []byte(`
name: my-object
value: some-value
other: current-other
mergingList:
  - name: 1
  - name: 2
replacingItem:
  Newly: Modified
  Yaml: Inside
  The: RawExtension
`),
			ThreeWay: []byte(`
$setElementOrder/mergingList:
  - name: 1
  - name: 2
mergingList:
  - name: 2
replacingItem:
  Newly: Modified
  Yaml: Inside
  The: RawExtension
`),
			Result: []byte(`
name: my-object
value: some-value
other: current-other
mergingList:
  - name: 1
  - name: 2
  - name: 3
replacingItem:
  Newly: Modified
  Yaml: Inside
  The: RawExtension
`),
		},
	},
}

func TestReplaceWithRawExtension(t *testing.T) {
	mergeItemOpenapiSchema := PatchMetaFromOpenAPI{
		Schema: sptest.GetSchemaOrDie(&fakeMergeItemSchema, "mergeItem"),
	}
	mergeItemOpenapiV3Schema := PatchMetaFromOpenAPIV3{
		SchemaList: fakeMergeItemV3Schema.SchemaOrDie().Components.Schemas,
		Schema:     fakeMergeItemV3Schema.SchemaOrDie().Components.Schemas["mergeItem"],
	}
	schemas := []LookupPatchMeta{
		mergeItemStructSchema,
		mergeItemOpenapiSchema,
		mergeItemOpenapiV3Schema,
	}

	for _, schema := range schemas {
		for _, c := range replaceRawExtensionPatchTestCases {
			testTwoWayPatchForRawTestCase(t, c, schema)
			testThreeWayPatchForRawTestCase(t, c, schema)
		}
	}
}

func TestUnknownField(t *testing.T) {
	testcases := map[string]struct {
		Original string
		Current  string
		Modified string

		ExpectedTwoWay         string
		ExpectedTwoWayErr      string
		ExpectedTwoWayResult   string
		ExpectedThreeWay       string
		ExpectedThreeWayErr    string
		ExpectedThreeWayResult string
	}{
		// cases we can successfully strategically merge
		"no diff": {
			Original: `{"array":[1,2,3],"complex":{"nested":true},"name":"foo","scalar":true}`,
			Current:  `{"array":[1,2,3],"complex":{"nested":true},"name":"foo","scalar":true}`,
			Modified: `{"array":[1,2,3],"complex":{"nested":true},"name":"foo","scalar":true}`,

			ExpectedTwoWay:         `{}`,
			ExpectedTwoWayResult:   `{"array":[1,2,3],"complex":{"nested":true},"name":"foo","scalar":true}`,
			ExpectedThreeWay:       `{}`,
			ExpectedThreeWayResult: `{"array":[1,2,3],"complex":{"nested":true},"name":"foo","scalar":true}`,
		},
		"no diff even if modified null": {
			Original: `{"array":[1,2,3],"complex":{"nested":true},"name":"foo","scalar":true}`,
			Current:  `{"array":[1,2,3],"complex":{"nested":true},"name":"foo","scalar":true}`,
			Modified: `{"array":[1,2,3],"complex":{"nested":true},"complex_nullable":{"key":null},"name":"foo","scalar":true}`,

			ExpectedTwoWay:         `{"complex_nullable":{"key":null}}`,
			ExpectedTwoWayResult:   `{"array":[1,2,3],"complex":{"nested":true},"complex_nullable":{},"name":"foo","scalar":true}`,
			ExpectedThreeWay:       `{"complex_nullable":{"key":null}}`,
			ExpectedThreeWayResult: `{"array":[1,2,3],"complex":{"nested":true},"complex_nullable":{},"name":"foo","scalar":true}`,
		},
		"discard nulls in nested and adds not nulls": {
			Original: `{"array":[1,2,3],"complex":{"nested":true},"name":"foo","scalar":true}`,
			Current:  `{"array":[1,2,3],"complex":{"nested":true},"name":"foo","scalar":true}`,
			Modified: `{"array":[1,2,3],"complex":{"nested":true},"complex_nullable":{"key":{"keynotnull":"value","keynull":null}},"name":"foo","scalar":true}`,

			ExpectedTwoWay:         `{"complex_nullable":{"key":{"keynotnull":"value","keynull":null}}}`,
			ExpectedTwoWayResult:   `{"array":[1,2,3],"complex":{"nested":true},"complex_nullable":{"key":{"keynotnull":"value"}},"name":"foo","scalar":true}`,
			ExpectedThreeWay:       `{"complex_nullable":{"key":{"keynotnull":"value","keynull":null}}}`,
			ExpectedThreeWayResult: `{"array":[1,2,3],"complex":{"nested":true},"complex_nullable":{"key":{"keynotnull":"value"}},"name":"foo","scalar":true}`,
		},
		"discard if modified all nulls": {
			Original: `{}`,
			Current:  `{}`,
			Modified: `{"complex":{"nested":null}}`,

			ExpectedTwoWay:         `{"complex":{"nested":null}}`,
			ExpectedTwoWayResult:   `{"complex":{}}`,
			ExpectedThreeWay:       `{"complex":{"nested":null}}`,
			ExpectedThreeWayResult: `{"complex":{}}`,
		},
		"add only not nulls": {
			Original: `{}`,
			Current:  `{}`,
			Modified: `{"complex":{"nested":null,"nested2":"foo"}}`,

			ExpectedTwoWay:         `{"complex":{"nested":null,"nested2":"foo"}}`,
			ExpectedTwoWayResult:   `{"complex":{"nested2":"foo"}}`,
			ExpectedThreeWay:       `{"complex":{"nested":null,"nested2":"foo"}}`,
			ExpectedThreeWayResult: `{"complex":{"nested2":"foo"}}`,
		},
		"null values in original are preserved": {
			Original: `{"thing":null}`,
			Current:  `{"thing":null}`,
			Modified: `{"nested":{"value":5},"thing":null}`,

			ExpectedTwoWay:         `{"nested":{"value":5}}`,
			ExpectedTwoWayResult:   `{"nested":{"value":5},"thing":null}`,
			ExpectedThreeWay:       `{"nested":{"value":5}}`,
			ExpectedThreeWayResult: `{"nested":{"value":5},"thing":null}`,
		},
		"nested null values in original are preserved": {
			Original: `{"complex":{"key":null},"thing":null}`,
			Current:  `{"complex":{"key":null},"thing":null}`,
			Modified: `{"complex":{"key":null},"nested":{"value":5},"thing":null}`,

			ExpectedTwoWay:         `{"nested":{"value":5}}`,
			ExpectedTwoWayResult:   `{"complex":{"key":null},"nested":{"value":5},"thing":null}`,
			ExpectedThreeWay:       `{"nested":{"value":5}}`,
			ExpectedThreeWayResult: `{"complex":{"key":null},"nested":{"value":5},"thing":null}`,
		},
		"add empty slices": {
			Original: `{"array":[1,2,3],"complex":{"nested":true},"name":"foo","scalar":true}`,
			Current:  `{"array":[1,2,3],"complex":{"nested":true},"name":"foo","scalar":true}`,
			Modified: `{"array":[1,2,3],"complex":{"nested":true},"complex_nullable":[],"name":"foo","scalar":true}`,

			ExpectedTwoWay:         `{"complex_nullable":[]}`,
			ExpectedTwoWayResult:   `{"array":[1,2,3],"complex":{"nested":true},"complex_nullable":[],"name":"foo","scalar":true}`,
			ExpectedThreeWay:       `{"complex_nullable":[]}`,
			ExpectedThreeWayResult: `{"array":[1,2,3],"complex":{"nested":true},"complex_nullable":[],"name":"foo","scalar":true}`,
		},
		"filter nulls from nested slices": {
			Original: `{}`,
			Current:  `{}`,
			Modified: `{"complex_nullable":[{"inner_one":{"key_one":"foo","key_two":null}}]}`,

			ExpectedTwoWay:         `{"complex_nullable":[{"inner_one":{"key_one":"foo","key_two":null}}]}`,
			ExpectedTwoWayResult:   `{"complex_nullable":[{"inner_one":{"key_one":"foo"}}]}`,
			ExpectedThreeWay:       `{"complex_nullable":[{"inner_one":{"key_one":"foo","key_two":null}}]}`,
			ExpectedThreeWayResult: `{"complex_nullable":[{"inner_one":{"key_one":"foo"}}]}`,
		},
		"filter if slice is all empty": {
			Original: `{}`,
			Current:  `{}`,
			Modified: `{"complex_nullable":[{"inner_one":{"key_one":null,"key_two":null}}]}`,

			ExpectedTwoWay:         `{"complex_nullable":[{"inner_one":{"key_one":null,"key_two":null}}]}`,
			ExpectedTwoWayResult:   `{"complex_nullable":[{"inner_one":{}}]}`,
			ExpectedThreeWay:       `{"complex_nullable":[{"inner_one":{"key_one":null,"key_two":null}}]}`,
			ExpectedThreeWayResult: `{"complex_nullable":[{"inner_one":{}}]}`,
		},
		"not filter nulls from non-associative slice": {
			Original: `{}`,
			Current:  `{}`,
			Modified: `{"complex_nullable":["key1",null,"key2"]}`,

			ExpectedTwoWay:         `{"complex_nullable":["key1",null,"key2"]}`,
			ExpectedTwoWayResult:   `{"complex_nullable":["key1",null,"key2"]}`,
			ExpectedThreeWay:       `{"complex_nullable":["key1",null,"key2"]}`,
			ExpectedThreeWayResult: `{"complex_nullable":["key1",null,"key2"]}`,
		},
		"added only": {
			Original: `{"name":"foo"}`,
			Current:  `{"name":"foo"}`,
			Modified: `{"name":"foo","scalar":true,"complex":{"nested":true},"array":[1,2,3]}`,

			ExpectedTwoWay:         `{"array":[1,2,3],"complex":{"nested":true},"scalar":true}`,
			ExpectedTwoWayResult:   `{"array":[1,2,3],"complex":{"nested":true},"name":"foo","scalar":true}`,
			ExpectedThreeWay:       `{"array":[1,2,3],"complex":{"nested":true},"scalar":true}`,
			ExpectedThreeWayResult: `{"array":[1,2,3],"complex":{"nested":true},"name":"foo","scalar":true}`,
		},
		"removed only": {
			Original: `{"name":"foo","scalar":true,"complex":{"nested":true}}`,
			Current:  `{"name":"foo","scalar":true,"complex":{"nested":true},"array":[1,2,3]}`,
			Modified: `{"name":"foo"}`,

			ExpectedTwoWay:         `{"complex":null,"scalar":null}`,
			ExpectedTwoWayResult:   `{"name":"foo"}`,
			ExpectedThreeWay:       `{"complex":null,"scalar":null}`,
			ExpectedThreeWayResult: `{"array":[1,2,3],"name":"foo"}`,
		},

		// cases we cannot successfully strategically merge (expect errors)
		"diff": {
			Original: `{"array":[1,2,3],"complex":{"nested":true},"name":"foo","scalar":true}`,
			Current:  `{"array":[1,2,3],"complex":{"nested":true},"name":"foo","scalar":true}`,
			Modified: `{"array":[1,2,3],"complex":{"nested":false},"name":"foo","scalar":true}`,

			ExpectedTwoWayErr:   `unable to find api field`,
			ExpectedThreeWayErr: `unable to find api field`,
		},
	}

	mergeItemOpenapiSchema := PatchMetaFromOpenAPI{
		Schema: sptest.GetSchemaOrDie(&fakeMergeItemSchema, "mergeItem"),
	}
	mergeItemOpenapiV3Schema := PatchMetaFromOpenAPIV3{
		SchemaList: fakeMergeItemV3Schema.SchemaOrDie().Components.Schemas,
		Schema:     fakeMergeItemV3Schema.SchemaOrDie().Components.Schemas["mergeItem"],
	}
	schemas := []LookupPatchMeta{
		mergeItemStructSchema,
		mergeItemOpenapiSchema,
		mergeItemOpenapiV3Schema,
	}

	for _, k := range sets.StringKeySet(testcases).List() {
		t.Run(k, func(t *testing.T) {
			tc := testcases[k]
			for _, schema := range schemas {
				t.Run(schema.Name()+"/TwoWay", func(t *testing.T) {
					twoWay, err := CreateTwoWayMergePatchUsingLookupPatchMeta([]byte(tc.Original), []byte(tc.Modified), schema)
					if err != nil {
						if len(tc.ExpectedTwoWayErr) == 0 {
							t.Errorf("using %s in testcase %s: error making two-way patch: %v", getSchemaType(schema), k, err)
						}
						if !strings.Contains(err.Error(), tc.ExpectedTwoWayErr) {
							t.Errorf("using %s in testcase %s: expected error making two-way patch to contain '%s', got %s", getSchemaType(schema), k, tc.ExpectedTwoWayErr, err)
						}
						return
					}

					if string(twoWay) != tc.ExpectedTwoWay {
						t.Errorf("using %s in testcase %s: expected two-way patch:\n\t%s\ngot\n\t%s", getSchemaType(schema), k, string(tc.ExpectedTwoWay), string(twoWay))
						return
					}

					twoWayResult, err := StrategicMergePatchUsingLookupPatchMeta([]byte(tc.Original), twoWay, schema)
					if err != nil {
						t.Errorf("using %s in testcase %s: error applying two-way patch: %v", getSchemaType(schema), k, err)
						return
					}
					if string(twoWayResult) != tc.ExpectedTwoWayResult {
						t.Errorf("using %s in testcase %s: expected two-way result:\n\t%s\ngot\n\t%s", getSchemaType(schema), k, string(tc.ExpectedTwoWayResult), string(twoWayResult))
						return
					}
				})

				t.Run(schema.Name()+"/ThreeWay", func(t *testing.T) {
					threeWay, err := CreateThreeWayMergePatch([]byte(tc.Original), []byte(tc.Modified), []byte(tc.Current), schema, false)
					if err != nil {
						if len(tc.ExpectedThreeWayErr) == 0 {
							t.Errorf("using %s in testcase %s: error making three-way patch: %v", getSchemaType(schema), k, err)
						} else if !strings.Contains(err.Error(), tc.ExpectedThreeWayErr) {
							t.Errorf("using %s in testcase %s: expected error making three-way patch to contain '%s', got %s", getSchemaType(schema), k, tc.ExpectedThreeWayErr, err)
						}
						return
					}

					if string(threeWay) != tc.ExpectedThreeWay {
						t.Errorf("using %s in testcase %s: expected three-way patch:\n\t%s\ngot\n\t%s", getSchemaType(schema), k, string(tc.ExpectedThreeWay), string(threeWay))
						return
					}

					threeWayResult, err := StrategicMergePatch([]byte(tc.Current), threeWay, schema)
					if err != nil {
						t.Errorf("using %s in testcase %s: error applying three-way patch: %v", getSchemaType(schema), k, err)
						return
					} else if string(threeWayResult) != tc.ExpectedThreeWayResult {
						t.Errorf("using %s in testcase %s: expected three-way result:\n\t%s\ngot\n\t%s", getSchemaType(schema), k, string(tc.ExpectedThreeWayResult), string(threeWayResult))
						return
					}
				})
			}
		})
	}
}
