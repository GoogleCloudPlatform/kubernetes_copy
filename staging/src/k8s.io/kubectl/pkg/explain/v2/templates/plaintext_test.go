/*
Copyright 2022 The Kubernetes Authors.

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

package templates_test

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"text/template"

	"github.com/google/go-cmp/cmp"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/kube-openapi/pkg/spec3"
	"k8s.io/kube-openapi/pkg/validation/spec"
	v2 "k8s.io/kubectl/pkg/explain/v2"
)

var (
	//go:embed plaintext.tmpl
	plaintextSource string

	//go:embed apiextensions.k8s.io_v1.json
	apiextensionsJSON string

	//go:embed batch.k8s.io_v1.json
	batchJSON string

	apiExtensionsV1OpenAPI map[string]interface{} = func() map[string]interface{} {
		var res map[string]interface{}
		utilruntime.Must(json.Unmarshal([]byte(apiextensionsJSON), &res))
		return res
	}()

	apiExtensionsV1OpenAPIWithoutListVerb map[string]interface{} = func() map[string]interface{} {
		var res map[string]interface{}
		utilruntime.Must(json.Unmarshal([]byte(apiextensionsJSON), &res))
		paths := res["paths"].(map[string]interface{})
		delete(paths, "/apis/apiextensions.k8s.io/v1/customresourcedefinitions")
		return res
	}()

	apiExtensionsV1OpenAPISpec spec3.OpenAPI = func() spec3.OpenAPI {
		var res spec3.OpenAPI
		utilruntime.Must(json.Unmarshal([]byte(apiextensionsJSON), &res))
		return res
	}()

	batchV1OpenAPI map[string]interface{} = func() map[string]interface{} {
		var res map[string]interface{}
		utilruntime.Must(json.Unmarshal([]byte(batchJSON), &res))
		return res
	}()

	batchV1OpenAPIWithoutListVerb map[string]interface{} = func() map[string]interface{} {
		var res map[string]interface{}
		utilruntime.Must(json.Unmarshal([]byte(batchJSON), &res))
		paths := res["paths"].(map[string]interface{})
		delete(paths, "/apis/batch/v1/jobs")
		delete(paths, "/apis/batch/v1/namespaces/{namespace}/jobs")

		delete(paths, "/apis/batch/v1/cronjobs")
		delete(paths, "/apis/batch/v1/namespaces/{namespace}/cronjobs/{name}")
		return res
	}()
)

type testCase struct {
	// test case name
	Name string
	// if empty uses main template
	Subtemplate string
	// context to render withi
	Context any
	// checks to perform on rendered output
	Checks []check
}

type check interface {
	doCheck(output string, err error) error
}

type checkError string

func (c checkError) doCheck(output string, err error) error {
	if !strings.Contains(err.Error(), "error: "+string(c)) {
		return fmt.Errorf("expected error: '%v' in string:\n%v", string(c), err)
	}
	return nil
}

type checkContains string

func (c checkContains) doCheck(output string, err error) error {
	if !strings.Contains(output, string(c)) {
		return fmt.Errorf("expected substring: '%v' in string:\n%v", string(c), output)
	}
	return nil
}

type checkNotContains string

func (c checkNotContains) doCheck(output string, err error) error {
	if strings.Contains(output, string(c)) {
		return fmt.Errorf("not expected substring: '%v' in string:\n%v", string(c), output)
	}
	return nil
}

type checkEquals string

func (c checkEquals) doCheck(output string, err error) error {
	if output != string(c) {
		return fmt.Errorf("output is not equal to expectation:\n%v", cmp.Diff(string(c), output))
	}
	return nil
}

func MapDict[K comparable, V any, N any](accum map[K]V, mapper func(V) N) map[K]N {
	res := make(map[K]N, len(accum))
	for k, v := range accum {
		res[k] = mapper(v)
	}
	return res
}

func ReduceDict[K comparable, V any, N any](val map[K]V, accum N, mapper func(N, K, V) N) N {
	for k, v := range val {
		accum = mapper(accum, k, v)
	}
	return accum
}

func TestPlaintext(t *testing.T) {
	testcases := []testCase{
		{
			// Test case where resource being rendered is not found in OpenAPI schema
			Name: "ResourceNotFound",
			Context: v2.TemplateContext{
				Document:  apiExtensionsV1OpenAPI,
				GVR:       schema.GroupVersionResource{},
				FieldPath: nil,
				Recursive: false,
			},
			Checks: []check{
				checkError("GVR (/, Resource=) not found in OpenAPI schema"),
			},
		},
		{
			// Test basic ability to find a GVR and print its description
			Name: "SchemaFound",
			Context: v2.TemplateContext{
				Document: apiExtensionsV1OpenAPI,
				GVR: schema.GroupVersionResource{
					Group:    "apiextensions.k8s.io",
					Version:  "v1",
					Resource: "customresourcedefinitions",
				},
				FieldPath: nil,
				Recursive: false,
			},
			Checks: []check{
				checkContains("CustomResourceDefinition represents a resource that should be exposed"),
			},
		},
		{
			// Test basic ability to find a namespaced GVR and print its description
			Name: "SchemaFoundNamespaced",
			Context: v2.TemplateContext{
				Document: batchV1OpenAPI,
				GVR: schema.GroupVersionResource{
					Group:    "batch",
					Version:  "v1",
					Resource: "jobs",
				},
				FieldPath: nil,
				Recursive: false,
			},
			Checks: []check{
				checkContains("Job represents the configuration of a single job"),
			},
		},
		{
			// Test basic ability to find a GVR without a list verb and print its description
			Name: "SchemaFoundWithoutListVerb",
			Context: v2.TemplateContext{
				Document: apiExtensionsV1OpenAPIWithoutListVerb,
				GVR: schema.GroupVersionResource{
					Group:    "apiextensions.k8s.io",
					Version:  "v1",
					Resource: "customresourcedefinitions",
				},
				FieldPath: nil,
				Recursive: false,
			},
			Checks: []check{
				checkContains("CustomResourceDefinition represents a resource that should be exposed"),
			},
		},
		{
			// Test basic ability to find a namespaced GVR without a list verb and print its description
			Name: "SchemaFoundNamespacedWithoutListVerb",
			Context: v2.TemplateContext{
				Document: batchV1OpenAPIWithoutListVerb,
				GVR: schema.GroupVersionResource{
					Group:    "batch",
					Version:  "v1",
					Resource: "jobs",
				},
				FieldPath: nil,
				Recursive: false,
			},
			Checks: []check{
				checkContains("Job represents the configuration of a single job"),
			},
		},
		{
			// Test basic ability to find a namespaced GVR without a top level list verb and print its description
			Name: "SchemaFoundNamespacedWithoutTopLevelListVerb",
			Context: v2.TemplateContext{
				Document: batchV1OpenAPIWithoutListVerb,
				GVR: schema.GroupVersionResource{
					Group:    "batch",
					Version:  "v1",
					Resource: "cronjobs",
				},
				FieldPath: nil,
				Recursive: false,
			},
			Checks: []check{
				checkContains("CronJob represents the configuration of a single cron job"),
			},
		},
		{
			// Test that shows trying to find a non-existent field path of an existing
			// schema
			Name: "SchemaFieldPathNotFound",
			Context: v2.TemplateContext{
				Document: apiExtensionsV1OpenAPI,
				GVR: schema.GroupVersionResource{
					Group:    "apiextensions.k8s.io",
					Version:  "v1",
					Resource: "customresourcedefinitions",
				},
				FieldPath: []string{"does", "not", "exist"},
				Recursive: false,
			},
			Checks: []check{
				checkError(`field "exist" does not exist`),
			},
		},
		{
			// Test traversing a single level for scalar field path
			Name: "SchemaFieldPathShallow",
			Context: v2.TemplateContext{
				Document: apiExtensionsV1OpenAPI,
				GVR: schema.GroupVersionResource{
					Group:    "apiextensions.k8s.io",
					Version:  "v1",
					Resource: "customresourcedefinitions",
				},
				FieldPath: []string{"kind"},
				Recursive: false,
			},
			Checks: []check{
				checkContains("FIELD: kind <string>"),
			},
		},
		{
			// Test traversing a multiple levels for scalar field path
			Name: "SchemaFieldPathDeep",
			Context: v2.TemplateContext{
				Document: apiExtensionsV1OpenAPI,
				GVR: schema.GroupVersionResource{
					Group:    "apiextensions.k8s.io",
					Version:  "v1",
					Resource: "customresourcedefinitions",
				},
				FieldPath: []string{"spec", "names", "singular"},
				Recursive: false,
			},
			Checks: []check{
				checkContains("FIELD: singular <string>"),
			},
		},
		{
			// Test traversing a multiple levels for scalar field path
			// through an array field
			Name: "SchemaFieldPathViaList",
			Context: v2.TemplateContext{
				Document: apiExtensionsV1OpenAPI,
				GVR: schema.GroupVersionResource{
					Group:    "apiextensions.k8s.io",
					Version:  "v1",
					Resource: "customresourcedefinitions",
				},
				FieldPath: []string{"spec", "versions", "name"},
				Recursive: false,
			},
			Checks: []check{
				checkContains("FIELD: name <string>"),
			},
		},
		{
			// Test traversing a multiple levels for scalar field path
			// through a map[string]T field.
			Name: "SchemaFieldPathViaMap",
			Context: v2.TemplateContext{
				Document: apiExtensionsV1OpenAPI,
				GVR: schema.GroupVersionResource{
					Group:    "apiextensions.k8s.io",
					Version:  "v1",
					Resource: "customresourcedefinitions",
				},
				FieldPath: []string{"spec", "versions", "schema", "openAPIV3Schema", "properties", "default"},
				Recursive: false,
			},
			Checks: []check{
				checkContains("default is a default value for undefined object fields"),
			},
		},
		{
			// Shows that walking through a recursively specified schema is A-OK!
			Name: "SchemaFieldPathRecursive",
			Context: v2.TemplateContext{
				Document: apiExtensionsV1OpenAPI,
				GVR: schema.GroupVersionResource{
					Group:    "apiextensions.k8s.io",
					Version:  "v1",
					Resource: "customresourcedefinitions",
				},
				FieldPath: []string{"spec", "versions", "schema", "openAPIV3Schema", "properties", "properties", "properties", "properties", "properties", "default"},
				Recursive: false,
			},
			Checks: []check{
				checkContains("default is a default value for undefined object fields"),
			},
		},
		{
			// Shows that all fields are included
			Name: "SchemaAllFields",
			Context: v2.TemplateContext{
				Document: apiExtensionsV1OpenAPI,
				GVR: schema.GroupVersionResource{
					Group:    "apiextensions.k8s.io",
					Version:  "v1",
					Resource: "customresourcedefinitions",
				},
				FieldPath: []string{"spec", "versions", "schema"},
				Recursive: false,
			},
			Checks: ReduceDict(apiExtensionsV1OpenAPISpec.Components.Schemas["io.k8s.apiextensions-apiserver.pkg.apis.apiextensions.v1.CustomResourceValidation"].Properties, []check{}, func(checks []check, k string, v spec.Schema) []check {
				return append(checks, checkContains(k), checkContains(v.Description))
			}),
		},
		{
			// Shows that all fields are included
			Name: "SchemaAllFieldsRecursive",
			Context: v2.TemplateContext{
				Document: apiExtensionsV1OpenAPI,
				GVR: schema.GroupVersionResource{
					Group:    "apiextensions.k8s.io",
					Version:  "v1",
					Resource: "customresourcedefinitions",
				},
				FieldPath: []string{"spec", "versions", "schema"},
				Recursive: true,
			},
			Checks: ReduceDict(apiExtensionsV1OpenAPISpec.Components.Schemas["io.k8s.apiextensions-apiserver.pkg.apis.apiextensions.v1.CustomResourceValidation"].Properties, []check{}, func(checks []check, k string, v spec.Schema) []check {
				return append(checks, checkContains(k))
			}),
		},
		{
			Name: "SchemaAllFieldsRecursiveWithDepthLimit1",
			Context: v2.TemplateContext{
				Document: apiExtensionsV1OpenAPI,
				GVR: schema.GroupVersionResource{
					Group:    "apiextensions.k8s.io",
					Version:  "v1",
					Resource: "customresourcedefinitions",
				},
				FieldPath: []string{"spec", "versions", "schema"},
				Recursive: true,
				Depth:     1,
			},
			Checks: []check{
				checkNotContains(`x-kubernetes-embedded-resource`),
			},
		},
		{
			Name: "SchemaAllFieldsRecursiveWithDepthLimit2",
			Context: v2.TemplateContext{
				Document: apiExtensionsV1OpenAPI,
				GVR: schema.GroupVersionResource{
					Group:    "apiextensions.k8s.io",
					Version:  "v1",
					Resource: "customresourcedefinitions",
				},
				FieldPath: []string{"spec", "versions", "schema"},
				Recursive: true,
				Depth:     2,
			},
			Checks: []check{
				checkContains(`x-kubernetes-embedded-resource`),
			},
		},
		{
			// Shows that the typeguess template works with scalars
			Name:        "Scalar",
			Subtemplate: "typeGuess",
			Context: map[string]any{
				"schema": map[string]any{
					"type": "string",
				},
			},
			Checks: []check{
				checkEquals("string"),
			},
		},
		{
			// Shows that the typeguess template works with boolean additionalProperties
			Name:        "True additionalProperties",
			Subtemplate: "typeGuess",
			Context: map[string]any{
				"schema": map[string]any{
					"type":                 "string",
					"additionalProperties": true,
				},
			},
			Checks: []check{
				checkEquals("string"),
			},
		},
		{
			// Show that a ref to a primitive type uses the referred type's type
			Name:        "PrimitiveRef",
			Subtemplate: "typeGuess",
			Context: map[string]any{
				"schema": map[string]any{
					"description": "a cool field",
					"$ref":        "#/components/schemas/v1.Time",
				},
				"Document": map[string]any{
					"components": map[string]any{
						"schemas": map[string]any{
							"v1.Time": map[string]any{
								"type":   "string",
								"format": "date-time",
							},
						},
					},
				},
			},
			Checks: []check{
				checkEquals("string"),
			},
		},
		{
			// Shows that the typeguess template behaves correctly given an
			// array with unknown items
			Name:        "ArrayUnknown",
			Subtemplate: "typeGuess",
			Context: map[string]any{
				"schema": map[string]any{
					"description": "a cool field",
					"type":        "array",
				},
			},
			Checks: []check{
				checkEquals("array"),
			},
		},
		{
			// Shows that the typeguess puts Object tpye in title case
			Name:        "ObjectTitle",
			Subtemplate: "typeGuess",
			Context: map[string]any{
				"schema": map[string]any{
					"description": "a cool field",
					"type":        "object",
				},
			},
			Checks: []check{
				checkEquals("Object"),
			},
		},
		{
			// Shows that the typeguess template works with scalars
			Name:        "ArrayOfScalar",
			Subtemplate: "typeGuess",
			Context: map[string]any{
				"schema": map[string]any{
					"description": "a cool field",
					"type":        "array",
					"items": map[string]any{
						"type": "number",
					},
				},
			},
			Checks: []check{
				checkEquals("[]number"),
			},
		},
		{
			// Shows that the typeguess template works with arrays containing
			// a items which are a schema specified by a single-element allOf
			// pointing to a $ref
			Name:        "ArrayOfAllOfRef",
			Subtemplate: "typeGuess",
			Context: map[string]any{
				"schema": map[string]any{
					"description": "a cool field",
					"type":        "array",
					"items": map[string]any{
						"type": "object",
						"allOf": []map[string]any{
							{
								"$ref": "io.k8s.apiextensions-apiserver.pkg.apis.apiextensions.v1.CustomResourceValidation",
							},
						},
					},
				},
			},
			Checks: []check{
				checkEquals("[]CustomResourceValidation"),
			},
		},
		{
			// Shows that the typeguess template works with arrays containing
			// a items which are a schema pointing to a $ref
			Name:        "ArrayOfRef",
			Subtemplate: "typeGuess",
			Context: map[string]any{
				"schema": map[string]any{
					"description": "a cool field",
					"type":        "array",
					"items": map[string]any{
						"type": "object",
						"$ref": "io.k8s.apiextensions-apiserver.pkg.apis.apiextensions.v1.CustomResourceValidation",
					},
				},
			},
			Checks: []check{
				checkEquals("[]CustomResourceValidation"),
			},
		},
		{
			// Shows that the typeguess template works with arrays of maps of scalars
			Name:        "ArrayOfMap",
			Subtemplate: "typeGuess",
			Context: map[string]any{
				"schema": map[string]any{
					"description": "a cool field",
					"type":        "array",
					"items": map[string]any{
						"type": "object",
						"additionalProperties": map[string]any{
							"type": "string",
						},
					},
				},
			},
			Checks: []check{
				checkEquals("[]map[string]string"),
			},
		},
		{
			// Shows that the typeguess template works with maps of arrays of scalars
			Name:        "MapOfArrayOfScalar",
			Subtemplate: "typeGuess",
			Context: map[string]any{
				"schema": map[string]any{
					"description": "a cool field",
					"type":        "object",
					"additionalProperties": map[string]any{
						"type": "array",
						"items": map[string]any{
							"type": "string",
						},
					},
				},
			},
			Checks: []check{
				checkEquals("map[string][]string"),
			},
		},
		{
			// Shows that the typeguess template works with maps of ref types
			Name:        "MapOfRef",
			Subtemplate: "typeGuess",
			Context: map[string]any{
				"schema": map[string]any{
					"description": "a cool field",
					"type":        "object",
					"additionalProperties": map[string]any{
						"type": "string",
						"allOf": []map[string]any{
							{
								"$ref": "io.k8s.apiextensions-apiserver.pkg.apis.apiextensions.v1.CustomResourceValidation",
							},
						},
					},
				},
			},
			Checks: []check{
				checkEquals("map[string]CustomResourceValidation"),
			},
		},
		{
			// Shows that the typeguess template prints `Object` if there
			// is absolutely no type information
			Name:        "Unknown",
			Subtemplate: "typeGuess",
			Context: map[string]any{
				"schema": map[string]any{
					"description": "a cool field",
				},
			},
			Checks: []check{
				checkEquals("Object"),
			},
		},
		{
			Name:        "Required",
			Subtemplate: "fieldDetail",
			Context: map[string]any{
				"schema": map[string]any{
					"type":        "object",
					"description": "a description that should not be printed",
					"properties": map[string]any{
						"thefield": map[string]any{
							"type":        "string",
							"description": "a description that should not be printed",
						},
					},
					"required": []string{"thefield"},
				},
				"name":  "thefield",
				"short": true,
			},
			Checks: []check{
				checkEquals("thefield\t<string> -required-\n"),
			},
		},
		{
			Name:        "Description",
			Subtemplate: "fieldDetail",
			Context: map[string]any{
				"schema": map[string]any{
					"type":        "object",
					"description": "a description that should not be printed",
					"properties": map[string]any{
						"thefield": map[string]any{
							"type":        "string",
							"description": "a description that should be printed",
						},
					},
					"required": []string{"thefield"},
				},
				"name":  "thefield",
				"short": false,
			},
			Checks: []check{
				checkEquals("thefield\t<string> -required-\n  a description that should be printed\n\n"),
			},
		},
		{
			Name:        "Indent",
			Subtemplate: "fieldDetail",
			Context: map[string]any{
				"schema": map[string]any{
					"type":        "object",
					"description": "a description that should not be printed",
					"properties": map[string]any{
						"thefield": map[string]any{
							"type":        "string",
							"description": "a description that should not be printed",
						},
					},
					"required": []string{"thefield"},
				},
				"name":  "thefield",
				"short": true,
				"level": 5,
			},
			Checks: []check{
				checkEquals("          thefield\t<string> -required-\n"),
			},
		},
		{
			// show that extractEnum can skip empty enum slice
			Name:        "extractEmptyEnum",
			Subtemplate: "extractEnum",
			Context: map[string]any{
				"schema": map[string]any{
					"type":        "string",
					"description": "a description that should not be printed",
					"enum":        []any{},
				},
			},
			Checks: []check{
				checkEquals(""),
			},
		},
		{
			// show that extractEnum can extract any enum slice and style it uppercase
			Name:        "extractEnumSimpleForm",
			Subtemplate: "extractEnum",
			Context: map[string]any{
				"schema": map[string]any{
					"type":        "string",
					"description": "a description that should not be printed",
					"enum":        []any{0, 1, 2, 3},
				},
				"isLongView": true,
			},
			Checks: []check{
				checkEquals("ENUM:\n    0\n    1\n    2\n    3"),
			},
		},
		{
			// show that extractEnum can extract any enum slice and style it lowercase
			Name:        "extractEnumLongFormWithIndent",
			Subtemplate: "extractEnum",
			Context: map[string]any{
				"schema": map[string]any{
					"type":        "string",
					"description": "a description that should not be printed",
					"enum":        []any{0, 1, 2, 3},
				},
				"isLongView":   false,
				"indentAmount": 2,
			},
			Checks: []check{
				checkEquals("\n  enum: 0, 1, 2, 3"),
			},
		},
		{
			// show that extractEnum can extract any enum slice and style it with truncated enums
			Name:        "extractEnumLongFormWithLimitAndIndent",
			Subtemplate: "extractEnum",
			Context: map[string]any{
				"schema": map[string]any{
					"type":        "string",
					"description": "a description that should not be printed",
					"enum":        []any{0, 1, 2, 3},
				},
				"isLongView":   false,
				"limit":        2,
				"indentAmount": 2,
			},
			Checks: []check{
				checkEquals("\n  enum: 0, 1, ...."),
			},
		},
		{
			// show that extractEnum can extract any enum slice and style it with truncated enums
			Name:        "extractEnumSimpleFormWithLimitAndIndent",
			Subtemplate: "extractEnum",
			Context: map[string]any{
				"schema": map[string]any{
					"type":        "string",
					"description": "a description that should not be printed",
					"enum":        []any{0, 1, 2, 3},
				},
				"isLongView":   true,
				"limit":        2,
				"indentAmount": 2,
			},
			Checks: []check{
				checkEquals("ENUM:\n    0\n    1, ...."),
			},
		},
		{
			// show that extractEnum can extract any enum slice and style it with empty string
			Name:        "extractEnumSimpleFormEmptyString",
			Subtemplate: "extractEnum",
			Context: map[string]any{
				"schema": map[string]any{
					"type":        "string",
					"description": "a description that should not be printed",
					"enum":        []any{"Block", "File", ""},
				},
				"isLongView": true,
			},
			Checks: []check{
				checkEquals("ENUM:\n    Block\n    File\n    \"\""),
			},
		},
	}

	tmpl, err := v2.WithBuiltinTemplateFuncs(template.New("")).Parse(plaintextSource)
	require.NoError(t, err)

	for _, tcase := range testcases {
		testName := tcase.Name
		if len(tcase.Subtemplate) > 0 {
			testName = tcase.Subtemplate + "/" + testName
		}

		t.Run(testName, func(t *testing.T) {
			buf := bytes.NewBuffer(nil)

			var outputErr error
			if len(tcase.Subtemplate) == 0 {
				outputErr = tmpl.Execute(buf, tcase.Context)
			} else {
				outputErr = tmpl.ExecuteTemplate(buf, tcase.Subtemplate, tcase.Context)
			}

			output := buf.String()
			for _, check := range tcase.Checks {
				err = check.doCheck(output, outputErr)

				if err != nil {
					t.Log("test failed on output:\n" + output)
					require.NoError(t, err)
				}
			}
		})
	}
}
