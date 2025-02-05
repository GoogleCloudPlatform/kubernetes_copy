/*
Copyright 2023 The Kubernetes Authors.

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

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	jsonpatch "gopkg.in/evanphx/json-patch.v4"

	apiextensionsinternal "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	apiservervalidation "k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	"k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	"k8s.io/apiextensions-apiserver/pkg/features"
	"k8s.io/apiextensions-apiserver/pkg/registry/customresource"
	"k8s.io/apiextensions-apiserver/test/integration/fixtures"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/apimachinery/pkg/util/wait"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/dynamic"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	"k8s.io/kube-openapi/pkg/validation/spec"
	"k8s.io/kube-openapi/pkg/validation/strfmt"
)

var stringSchema *apiextensionsv1.JSONSchemaProps = &apiextensionsv1.JSONSchemaProps{
	Type: "string",
}

var stringMapSchema *apiextensionsv1.JSONSchemaProps = &apiextensionsv1.JSONSchemaProps{
	Type: "object",
	AdditionalProperties: &apiextensionsv1.JSONSchemaPropsOrBool{
		Schema: stringSchema,
	},
}

var numberSchema *apiextensionsv1.JSONSchemaProps = &apiextensionsv1.JSONSchemaProps{
	Type: "integer",
}

var numbersMapSchema *apiextensionsv1.JSONSchemaProps = &apiextensionsv1.JSONSchemaProps{
	Type: "object",
	AdditionalProperties: &apiextensionsv1.JSONSchemaPropsOrBool{
		Schema: numberSchema,
	},
}

type ratchetingTestContext struct {
	*testing.T
	DynamicClient       dynamic.Interface
	APIExtensionsClient clientset.Interface
	StatusSubresource   bool
}

type ratchetingTestOperation interface {
	Do(ctx *ratchetingTestContext) error
	Description() string
}

type expectError struct {
	op ratchetingTestOperation
}

func (e expectError) Do(ctx *ratchetingTestContext) error {
	err := e.op.Do(ctx)
	if err != nil {
		return nil
	}
	return errors.New("expected error")
}

func (e expectError) Description() string {
	return fmt.Sprintf("Expect Error: %v", e.op.Description())
}

// apiextensions-apiserver has discovery disabled, so hardcode this mapping
var fakeRESTMapper map[schema.GroupVersionResource]string = map[schema.GroupVersionResource]string{
	myCRDV1Beta1: "MyCoolCRD",
}

// FixTabsOrDie counts the number of tab characters preceding the first
// line in the given yaml object. It removes that many tabs from every
// line. It panics (it's a test function) if some line has fewer tabs
// than the first line.
//
// The purpose of this is to make it easier to read tests.
func FixTabsOrDie(in string) string {
	lines := bytes.Split([]byte(in), []byte{'\n'})
	if len(lines[0]) == 0 && len(lines) > 1 {
		lines = lines[1:]
	}
	// Create prefix made of tabs that we want to remove.
	var prefix []byte
	for _, c := range lines[0] {
		if c != '\t' {
			break
		}
		prefix = append(prefix, byte('\t'))
	}
	// Remove prefix from all tabs, fail otherwise.
	for i := range lines {
		line := lines[i]
		// It's OK for the last line to be blank (trailing \n)
		if i == len(lines)-1 && len(line) <= len(prefix) && bytes.TrimSpace(line) == nil {
			lines[i] = []byte{}
			break
		}
		if !bytes.HasPrefix(line, prefix) {
			panic(fmt.Errorf("line %d doesn't start with expected number (%d) of tabs: %v", i, len(prefix), string(line)))
		}
		lines[i] = line[len(prefix):]
	}
	joined := string(bytes.Join(lines, []byte{'\n'}))

	// Convert rest of tabs to spaces since yaml doesnt like yabs
	// (assuming 2 space alignment)
	return strings.ReplaceAll(joined, "\t", "  ")
}

type applyPatchOperation struct {
	description string
	gvr         schema.GroupVersionResource
	name        string
	patch       interface{}
}

func (a applyPatchOperation) Do(ctx *ratchetingTestContext) error {
	// Lookup GVK from discovery
	kind, ok := fakeRESTMapper[a.gvr]
	if !ok {
		return fmt.Errorf("no mapping found for Gvr %v, add entry to fakeRESTMapper", a.gvr)
	}

	patch := &unstructured.Unstructured{}
	if obj, ok := a.patch.(map[string]interface{}); ok {
		patch.Object = runtime.DeepCopyJSON(obj)
	} else if str, ok := a.patch.(string); ok {
		str = FixTabsOrDie(str)
		if err := utilyaml.NewYAMLOrJSONDecoder(strings.NewReader(str), len(str)).Decode(&patch.Object); err != nil {
			return err
		}
	} else {
		return fmt.Errorf("invalid patch type: %T", a.patch)
	}

	if ctx.StatusSubresource {
		patch.Object = map[string]interface{}{"status": patch.Object}
	}

	patch.SetKind(kind)
	patch.SetAPIVersion(a.gvr.GroupVersion().String())
	patch.SetName(a.name)
	patch.SetNamespace("default")

	c := ctx.DynamicClient.Resource(a.gvr).Namespace(patch.GetNamespace())
	if ctx.StatusSubresource {
		if _, err := c.Get(context.TODO(), patch.GetName(), metav1.GetOptions{}); apierrors.IsNotFound(err) {
			// ApplyStatus will not automatically create an object, we must make sure it exists before we can
			// apply the status to it.
			_, err := c.Create(context.TODO(), patch, metav1.CreateOptions{})
			if err != nil {
				return err
			}
		}

		_, err := c.ApplyStatus(context.TODO(), patch.GetName(), patch, metav1.ApplyOptions{FieldManager: "manager"})
		return err
	}
	_, err := c.Apply(context.TODO(), patch.GetName(), patch, metav1.ApplyOptions{FieldManager: "manager"})
	return err
}

func (a applyPatchOperation) Description() string {
	return a.description
}

// Replaces schema used for v1beta1 of crd
type updateMyCRDV1Beta1Schema struct {
	newSchema *apiextensionsv1.JSONSchemaProps
}

func (u updateMyCRDV1Beta1Schema) Do(ctx *ratchetingTestContext) error {
	var myCRD *apiextensionsv1.CustomResourceDefinition
	var err error = apierrors.NewConflict(schema.GroupResource{}, "", nil)
	for apierrors.IsConflict(err) {
		myCRD, err = ctx.APIExtensionsClient.ApiextensionsV1().CustomResourceDefinitions().Get(context.TODO(), myCRDV1Beta1.Resource+"."+myCRDV1Beta1.Group, metav1.GetOptions{})
		if err != nil {
			return err
		}

		// Insert a sentinel property that we can probe to detect when the
		// schema takes effect
		sch := u.newSchema.DeepCopy()
		if sch.Properties == nil {
			sch.Properties = map[string]apiextensionsv1.JSONSchemaProps{}
		}

		uuidString := string(uuid.NewUUID())
		sentinelName := "__ratcheting_sentinel_field__"
		sch.Properties[sentinelName] = apiextensionsv1.JSONSchemaProps{
			Type: "string",
			Enum: []apiextensionsv1.JSON{{
				Raw: []byte(`"` + uuidString + `"`),
			}},
		}

		if ctx.StatusSubresource {
			sch = &apiextensionsv1.JSONSchemaProps{
				Type: "object",
				Properties: map[string]apiextensionsv1.JSONSchemaProps{
					"status": *sch,
				},
			}
		}

		for _, v := range myCRD.Spec.Versions {
			if v.Name != myCRDV1Beta1.Version {
				continue
			}

			v.Schema.OpenAPIV3Schema = sch
		}

		_, err = ctx.APIExtensionsClient.ApiextensionsV1().CustomResourceDefinitions().Update(context.TODO(), myCRD, metav1.UpdateOptions{
			FieldManager: "manager",
		})
		if err != nil {
			return err
		}

		// Keep trying to create an invalid instance of the CRD until we
		// get an error containing the message we are looking for
		//
		counter := 0
		return wait.PollUntilContextCancel(context.TODO(), 100*time.Millisecond, true, func(_ context.Context) (done bool, err error) {
			counter += 1
			err = applyPatchOperation{
				gvr:  myCRDV1Beta1,
				name: "sentinel-resource",
				patch: map[string]interface{}{
					sentinelName: fmt.Sprintf("invalid-%d", counter),
				}}.Do(ctx)

			if err == nil {
				return false, errors.New("expected error when creating sentinel resource")
			}

			// Check to see if the returned error message contains our
			// unique string. UUID should be unique enough to just check
			// simple existence in the error.
			if strings.Contains(err.Error(), uuidString) {
				return true, nil
			}

			return false, nil
		})
	}
	return err
}

func (u updateMyCRDV1Beta1Schema) Description() string {
	return "Update CRD schema"
}

type patchMyCRDV1Beta1Schema struct {
	description string
	patch       map[string]interface{}
}

func (p patchMyCRDV1Beta1Schema) Do(ctx *ratchetingTestContext) error {
	patch := p.patch
	if ctx.StatusSubresource {
		patch = map[string]interface{}{
			"properties": map[string]interface{}{
				"status": patch,
			},
		}
	}

	var err error
	patchJSON, err := json.Marshal(patch)
	if err != nil {
		return err
	}

	myCRD, err := ctx.APIExtensionsClient.ApiextensionsV1().CustomResourceDefinitions().Get(context.TODO(), myCRDV1Beta1.Resource+"."+myCRDV1Beta1.Group, metav1.GetOptions{})
	if err != nil {
		return err
	}

	for _, v := range myCRD.Spec.Versions {
		if v.Name != myCRDV1Beta1.Version {
			continue
		}

		jsonSchema, err := json.Marshal(v.Schema.OpenAPIV3Schema)
		if err != nil {
			return err
		}

		merged, err := jsonpatch.MergePatch(jsonSchema, patchJSON)
		if err != nil {
			return err
		}

		var parsed apiextensionsv1.JSONSchemaProps
		if err := json.Unmarshal(merged, &parsed); err != nil {
			return err
		}

		return updateMyCRDV1Beta1Schema{
			newSchema: &parsed,
		}.Do(&ratchetingTestContext{
			T:                   ctx.T,
			DynamicClient:       ctx.DynamicClient,
			APIExtensionsClient: ctx.APIExtensionsClient,
			StatusSubresource:   false, // We have already handled the status subresource.
		})
	}

	return fmt.Errorf("could not find version %v in CRD %v", myCRDV1Beta1.Version, myCRD.Name)
}

func (p patchMyCRDV1Beta1Schema) Description() string {
	return p.description
}

type ratchetingTestCase struct {
	Name       string
	Disabled   bool
	Operations []ratchetingTestOperation
	SkipStatus bool
}

func runTests(t *testing.T, cases []ratchetingTestCase) {
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.CRDValidationRatcheting, true)
	tearDown, apiExtensionClient, dynamicClient, err := fixtures.StartDefaultServerWithClients(t)
	if err != nil {
		t.Fatal(err)
	}
	defer tearDown()

	group := myCRDV1Beta1.Group
	version := myCRDV1Beta1.Version
	resource := myCRDV1Beta1.Resource
	kind := fakeRESTMapper[myCRDV1Beta1]

	myCRD := &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: resource + "." + group},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: group,
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{
				Name:    version,
				Served:  true,
				Storage: true,
				Schema: &apiextensionsv1.CustomResourceValidation{
					OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
						Type: "object",
						Properties: map[string]apiextensionsv1.JSONSchemaProps{
							"content": {
								Type: "object",
								AdditionalProperties: &apiextensionsv1.JSONSchemaPropsOrBool{
									Schema: &apiextensionsv1.JSONSchemaProps{
										Type: "string",
									},
								},
							},
							"num": {
								Type: "object",
								AdditionalProperties: &apiextensionsv1.JSONSchemaPropsOrBool{
									Schema: &apiextensionsv1.JSONSchemaProps{
										Type: "integer",
									},
								},
							},
							"status": {
								Type: "object",
							},
						},
					},
				},
				Subresources: &apiextensionsv1.CustomResourceSubresources{
					Status: &apiextensionsv1.CustomResourceSubresourceStatus{},
				},
			}},
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Plural:   resource,
				Kind:     kind,
				ListKind: kind + "List",
			},
			Scope: apiextensionsv1.NamespaceScoped,
		},
	}

	_, err = fixtures.CreateNewV1CustomResourceDefinition(myCRD, apiExtensionClient, dynamicClient)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		if c.Disabled {
			continue
		}

		run := func(t *testing.T, ctx *ratchetingTestContext) {
			for i, op := range c.Operations {
				t.Logf("Performing Operation: %v", op.Description())
				if err := op.Do(ctx); err != nil {
					t.Fatalf("failed %T operation %v: %v\n%v", op, i, err, op)
				}
			}

			// Reset resources
			err := ctx.DynamicClient.Resource(myCRDV1Beta1).Namespace("default").DeleteCollection(context.TODO(), metav1.DeleteOptions{}, metav1.ListOptions{})
			if err != nil {
				t.Fatal(err)
			}
		}

		t.Run(c.Name, func(t *testing.T) {
			run(t, &ratchetingTestContext{
				T:                   t,
				DynamicClient:       dynamicClient,
				APIExtensionsClient: apiExtensionClient,
			})
		})

		if !c.SkipStatus {
			t.Run("Status: "+c.Name, func(t *testing.T) {
				run(t, &ratchetingTestContext{
					T:                   t,
					DynamicClient:       dynamicClient,
					APIExtensionsClient: apiExtensionClient,
					StatusSubresource:   true,
				})
			})
		}
	}
}

var myCRDV1Beta1 schema.GroupVersionResource = schema.GroupVersionResource{
	Group:    "mygroup.example.com",
	Version:  "v1beta1",
	Resource: "mycrds",
}

var myCRDInstanceName string = "mycrdinstance"

func TestRatchetingFunctionality(t *testing.T) {
	cases := []ratchetingTestCase{
		{
			Name: "Minimum Maximum",
			Operations: []ratchetingTestOperation{
				updateMyCRDV1Beta1Schema{&apiextensionsv1.JSONSchemaProps{
					Type: "object",
					Properties: map[string]apiextensionsv1.JSONSchemaProps{
						"hasMinimum":           *numberSchema,
						"hasMaximum":           *numberSchema,
						"hasMinimumAndMaximum": *numberSchema,
					},
				}},
				applyPatchOperation{
					"Create an object that complies with the schema",
					myCRDV1Beta1,
					myCRDInstanceName,
					map[string]interface{}{
						"hasMinimum":           int64(0),
						"hasMaximum":           int64(1000),
						"hasMinimumAndMaximum": int64(50),
					}},
				patchMyCRDV1Beta1Schema{
					"Add stricter minimums and maximums that violate the previous object",
					map[string]interface{}{
						"properties": map[string]interface{}{
							"hasMinimum": map[string]interface{}{
								"minimum": int64(10),
							},
							"hasMaximum": map[string]interface{}{
								"maximum": int64(20),
							},
							"hasMinimumAndMaximum": map[string]interface{}{
								"minimum": int64(10),
								"maximum": int64(20),
							},
							"noRestrictions": map[string]interface{}{
								"type": "integer",
							},
						},
					}},
				applyPatchOperation{
					"Add new fields that validates successfully without changing old ones",
					myCRDV1Beta1,
					myCRDInstanceName,
					map[string]interface{}{
						"noRestrictions": int64(50),
					}},
				expectError{
					applyPatchOperation{
						"Change a single old field to be invalid",
						myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
							"hasMinimum": int64(5),
						}},
				},
				expectError{
					applyPatchOperation{
						"Change multiple old fields to be invalid",
						myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
							"hasMinimum": int64(5),
							"hasMaximum": int64(21),
						}},
				},
				applyPatchOperation{
					"Change single old field to be valid",
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"hasMinimum": int64(11),
					}},
				applyPatchOperation{
					"Change multiple old fields to be valid",
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"hasMaximum":           int64(19),
						"hasMinimumAndMaximum": int64(15),
					}},
			},
		},
		{
			Name: "Enum",
			Operations: []ratchetingTestOperation{
				// Create schema with some enum element
				updateMyCRDV1Beta1Schema{&apiextensionsv1.JSONSchemaProps{
					Type: "object",
					Properties: map[string]apiextensionsv1.JSONSchemaProps{
						"enumField": *stringSchema,
					},
				}},
				applyPatchOperation{
					"Create an instance with a soon-to-be-invalid value",
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"enumField": "okValueNowBadValueLater",
					}},
				patchMyCRDV1Beta1Schema{
					"restrict `enumField` to an enum of A, B, or C",
					map[string]interface{}{
						"properties": map[string]interface{}{
							"enumField": map[string]interface{}{
								"enum": []interface{}{
									"A", "B", "C",
								},
							},
							"otherField": map[string]interface{}{
								"type": "string",
							},
						},
					}},
				applyPatchOperation{
					"An invalid patch with no changes is a noop",
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"enumField": "okValueNowBadValueLater",
					}},
				applyPatchOperation{
					"Add a new field, and include old value in our patch",
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"enumField":  "okValueNowBadValueLater",
						"otherField": "anythingGoes",
					}},
				expectError{
					applyPatchOperation{
						"Set enumField to invalid value D",
						myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
							"enumField": "D",
						}},
				},
				applyPatchOperation{
					"Set to a valid value",
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"enumField": "A",
					}},
				expectError{
					applyPatchOperation{
						"After setting a valid value, return to the old, accepted value",
						myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
							"enumField": "okValueNowBadValueLater",
						}},
				},
			},
		},
		{
			Name: "AdditionalProperties",
			Operations: []ratchetingTestOperation{
				updateMyCRDV1Beta1Schema{&apiextensionsv1.JSONSchemaProps{
					Type: "object",
					Properties: map[string]apiextensionsv1.JSONSchemaProps{
						"nums":    *numbersMapSchema,
						"content": *stringMapSchema,
					},
				}},
				applyPatchOperation{
					"Create an instance",
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"nums": map[string]interface{}{
							"num1": int64(1),
							"num2": int64(1000000),
						},
						"content": map[string]interface{}{
							"k1": "some content",
							"k2": "other content",
						},
					}},
				patchMyCRDV1Beta1Schema{
					"set minimum value for fields with additionalProperties",
					map[string]interface{}{
						"properties": map[string]interface{}{
							"nums": map[string]interface{}{
								"additionalProperties": map[string]interface{}{
									"minimum": int64(1000),
								},
							},
						},
					}},
				applyPatchOperation{
					"updating validating field num2 to another validating value, but rachet invalid field num1",
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"nums": map[string]interface{}{
							"num1": int64(1),
							"num2": int64(2000),
						},
					}},
				expectError{applyPatchOperation{
					"update field num1 to different invalid value",
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"nums": map[string]interface{}{
							"num1": int64(2),
							"num2": int64(2000),
						},
					}}},
			},
		},
		{
			Name: "MinProperties MaxProperties",
			Operations: []ratchetingTestOperation{
				updateMyCRDV1Beta1Schema{&apiextensionsv1.JSONSchemaProps{
					Type: "object",
					Properties: map[string]apiextensionsv1.JSONSchemaProps{
						"restricted": {
							Type: "object",
							AdditionalProperties: &apiextensionsv1.JSONSchemaPropsOrBool{
								Schema: stringSchema,
							},
						},
						"unrestricted": {
							Type: "object",
							AdditionalProperties: &apiextensionsv1.JSONSchemaPropsOrBool{
								Schema: stringSchema,
							},
						},
					},
				}},
				applyPatchOperation{
					"Create instance",
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"restricted": map[string]interface{}{
							"key1": "hi",
							"key2": "there",
						},
					}},
				patchMyCRDV1Beta1Schema{
					"set both minProperties and maxProperties to 1 to violate the previous object",
					map[string]interface{}{
						"properties": map[string]interface{}{
							"restricted": map[string]interface{}{
								"minProperties": int64(1),
								"maxProperties": int64(1),
							},
						},
					}},
				applyPatchOperation{
					"ratchet violating object 'restricted' around changes to unrelated field",
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"restricted": map[string]interface{}{
							"key1": "hi",
							"key2": "there",
						},
						"unrestricted": map[string]interface{}{
							"key1": "yo",
						},
					}},
				expectError{applyPatchOperation{
					"make invalid changes to previously ratcheted invalid field",
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"restricted": map[string]interface{}{
							"key1": "changed",
							"key2": "there",
						},
						"unrestricted": map[string]interface{}{
							"key1": "yo",
						},
					}}},

				patchMyCRDV1Beta1Schema{
					"remove maxProeprties, set minProperties to 2",
					map[string]interface{}{
						"properties": map[string]interface{}{
							"restricted": map[string]interface{}{
								"minProperties": int64(2),
								"maxProperties": nil,
							},
						},
					}},
				applyPatchOperation{
					"a new value",
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"restricted": map[string]interface{}{
							"key1": "hi",
							"key2": "there",
							"key3": "buddy",
						},
					}},

				expectError{applyPatchOperation{
					"violate new validation by removing keys",
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"restricted": map[string]interface{}{
							"key1": "hi",
							"key2": nil,
							"key3": nil,
						},
					}}},
				patchMyCRDV1Beta1Schema{
					"remove minProperties, set maxProperties to 1",
					map[string]interface{}{
						"properties": map[string]interface{}{
							"restricted": map[string]interface{}{
								"minProperties": nil,
								"maxProperties": int64(1),
							},
						},
					}},
				applyPatchOperation{
					"modify only the other key, ratcheting maxProperties for field `restricted`",
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"restricted": map[string]interface{}{
							"key1": "hi",
							"key2": "there",
							"key3": "buddy",
						},
						"unrestricted": map[string]interface{}{
							"key1": "value",
							"key2": "value",
						},
					}},
				expectError{
					applyPatchOperation{
						"modifying one value in the object with maxProperties restriction, but keeping old fields",
						myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
							"restricted": map[string]interface{}{
								"key1": "hi",
								"key2": "theres",
								"key3": "buddy",
							},
						}}},
			},
		},
		{
			Name: "MinItems",
			Operations: []ratchetingTestOperation{
				updateMyCRDV1Beta1Schema{&apiextensionsv1.JSONSchemaProps{
					Type: "object",
					Properties: map[string]apiextensionsv1.JSONSchemaProps{
						"field": *stringSchema,
						"array": {
							Type: "array",
							Items: &apiextensionsv1.JSONSchemaPropsOrArray{
								Schema: stringSchema,
							},
						},
					},
				}},
				applyPatchOperation{
					"Create instance",
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"array": []interface{}{"value1", "value2", "value3"},
					}},
				patchMyCRDV1Beta1Schema{
					"change minItems on array to 10, invalidates previous object",
					map[string]interface{}{
						"properties": map[string]interface{}{
							"array": map[string]interface{}{
								"minItems": int64(10),
							},
						},
					}},
				applyPatchOperation{
					"keep invalid field `array` unchanged, add new field with ratcheting",
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"array": []interface{}{"value1", "value2", "value3"},
						"field": "value",
					}},
				expectError{
					applyPatchOperation{
						"modify array element without satisfying property",
						myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
							"array": []interface{}{"value2", "value2", "value3"},
						}}},

				expectError{
					applyPatchOperation{
						"add array element without satisfying proeprty",
						myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
							"array": []interface{}{"value1", "value2", "value3", "value4"},
						}}},

				applyPatchOperation{
					"make array valid",
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"array": []interface{}{"value1", "value2", "value3", "4", "5", "6", "7", "8", "9", "10"},
					}},
				expectError{
					applyPatchOperation{
						"revert to original value",
						myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
							"array": []interface{}{"value1", "value2", "value3"},
						}}},
			},
		},
		{
			Name: "MaxItems",
			Operations: []ratchetingTestOperation{
				updateMyCRDV1Beta1Schema{&apiextensionsv1.JSONSchemaProps{
					Type: "object",
					Properties: map[string]apiextensionsv1.JSONSchemaProps{
						"field": *stringSchema,
						"array": {
							Type: "array",
							Items: &apiextensionsv1.JSONSchemaPropsOrArray{
								Schema: stringSchema,
							},
						},
					},
				}},
				applyPatchOperation{
					"create instance",
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"array": []interface{}{"value1", "value2", "value3"},
					}},
				patchMyCRDV1Beta1Schema{
					"change maxItems on array to 1, invalidates previous object",
					map[string]interface{}{
						"properties": map[string]interface{}{
							"array": map[string]interface{}{
								"maxItems": int64(1),
							},
						},
					}},
				applyPatchOperation{
					"ratchet old value of array through an update to another field",
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"array": []interface{}{"value1", "value2", "value3"},
						"field": "value",
					}},
				expectError{
					applyPatchOperation{
						"modify array element without satisfying property",
						myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
							"array": []interface{}{"value2", "value2", "value3"},
						}}},

				expectError{
					applyPatchOperation{
						"remove array element without satisfying proeprty",
						myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
							"array": []interface{}{"value1", "value2"},
						}}},

				applyPatchOperation{
					"change array to valid value that satisfies maxItems",
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"array": []interface{}{"value1"},
					}},
				expectError{
					applyPatchOperation{
						"revert to previous invalid ratcheted value",
						myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
							"array": []interface{}{"value1", "value2", "value3"},
						}}},
			},
		},
		{
			Name: "MinLength MaxLength",
			Operations: []ratchetingTestOperation{
				updateMyCRDV1Beta1Schema{&apiextensionsv1.JSONSchemaProps{
					Type: "object",
					Properties: map[string]apiextensionsv1.JSONSchemaProps{
						"minField":   *stringSchema,
						"maxField":   *stringSchema,
						"otherField": *stringSchema,
					},
				}},
				applyPatchOperation{
					"create instance",
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"minField": "value",
						"maxField": "valueThatsVeryLongSee",
					}},
				patchMyCRDV1Beta1Schema{
					"set minField maxLength to 10, and maxField's minLength to 15",
					map[string]interface{}{
						"properties": map[string]interface{}{
							"minField": map[string]interface{}{
								"minLength": int64(10),
							},
							"maxField": map[string]interface{}{
								"maxLength": int64(15),
							},
						},
					}},
				applyPatchOperation{
					"add new field `otherField`, ratcheting `minField` and `maxField`",
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"minField":   "value",
						"maxField":   "valueThatsVeryLongSee",
						"otherField": "otherValue",
					}},
				applyPatchOperation{
					"make minField valid, ratcheting old value for maxField",
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"minField":   "valuelength13",
						"maxField":   "valueThatsVeryLongSee",
						"otherField": "otherValue",
					}},
				applyPatchOperation{
					"make maxField shorter",
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"maxField": "l2",
					}},
				expectError{
					applyPatchOperation{
						"make maxField too long",
						myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
							"maxField": "valuewithlength17",
						}}},
				expectError{
					applyPatchOperation{
						"revert minFIeld to previously ratcheted value",
						myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
							"minField": "value",
						}}},
				expectError{
					applyPatchOperation{
						"revert maxField to previously ratcheted value",
						myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
							"maxField": "valueThatsVeryLongSee",
						}}},
			},
		},
		{
			Name: "Pattern",
			Operations: []ratchetingTestOperation{
				updateMyCRDV1Beta1Schema{&apiextensionsv1.JSONSchemaProps{
					Type: "object",
					Properties: map[string]apiextensionsv1.JSONSchemaProps{
						"field": *stringSchema,
					},
				}},
				applyPatchOperation{
					"create instance",
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"field": "doesnt abide pattern",
					}},
				patchMyCRDV1Beta1Schema{
					"add pattern validation on `field`",
					map[string]interface{}{
						"properties": map[string]interface{}{
							"field": map[string]interface{}{
								"pattern": "^[1-9]+$",
							},
							"otherField": map[string]interface{}{
								"type": "string",
							},
						},
					}},
				applyPatchOperation{
					"add unrelated field, ratcheting old invalid field",
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"field":      "doesnt abide pattern",
						"otherField": "added",
					}},
				expectError{applyPatchOperation{
					"change field to invalid value",
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"field":      "w123",
						"otherField": "added",
					}}},
				applyPatchOperation{
					"change field to a valid value",
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"field":      "123",
						"otherField": "added",
					}},
			},
		},
		{
			Name: "Format Addition and Change",
			Operations: []ratchetingTestOperation{
				updateMyCRDV1Beta1Schema{&apiextensionsv1.JSONSchemaProps{
					Type: "object",
					Properties: map[string]apiextensionsv1.JSONSchemaProps{
						"field": *stringSchema,
					},
				}},
				applyPatchOperation{
					"create instance",
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"field": "doesnt abide any format",
					}},
				patchMyCRDV1Beta1Schema{
					"change `field`'s format to `byte",
					map[string]interface{}{
						"properties": map[string]interface{}{
							"field": map[string]interface{}{
								"format": "byte",
							},
							"otherField": map[string]interface{}{
								"type": "string",
							},
						},
					}},
				applyPatchOperation{
					"add unrelated otherField, ratchet invalid old field format",
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"field":      "doesnt abide any format",
						"otherField": "value",
					}},
				expectError{applyPatchOperation{
					"change field to an invalid string",
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"field": "asd",
					}}},
				applyPatchOperation{
					"change field to a valid byte string",
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"field": "dGhpcyBpcyBwYXNzd29yZA==",
					}},
				patchMyCRDV1Beta1Schema{
					"change `field`'s format to date-time",
					map[string]interface{}{
						"properties": map[string]interface{}{
							"field": map[string]interface{}{
								"format": "date-time",
							},
						},
					}},
				applyPatchOperation{
					"change otherField, ratchet `field`'s invalid byte format",
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"field":      "dGhpcyBpcyBwYXNzd29yZA==",
						"otherField": "value2",
					}},
				applyPatchOperation{
					"change `field` to a valid value",
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"field":      "2018-11-13T20:20:39+00:00",
						"otherField": "value2",
					}},
				expectError{
					applyPatchOperation{
						"revert `field` to previously ratcheted value",
						myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
							"field":      "dGhpcyBpcyBwYXNzd29yZA==",
							"otherField": "value2",
						}}},
				expectError{
					applyPatchOperation{
						"revert `field` to its initial value from creation",
						myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
							"field": "doesnt abide any format",
						}}},
			},
		},
		{
			Name: "Map Type List Reordering Grandfathers Invalid Key",
			Operations: []ratchetingTestOperation{
				updateMyCRDV1Beta1Schema{&apiextensionsv1.JSONSchemaProps{
					Type: "object",
					Properties: map[string]apiextensionsv1.JSONSchemaProps{
						"field": {
							Type:         "array",
							XListType:    ptr("map"),
							XListMapKeys: []string{"name", "port"},
							Items: &apiextensionsv1.JSONSchemaPropsOrArray{
								Schema: &apiextensionsv1.JSONSchemaProps{
									Type:     "object",
									Required: []string{"name", "port"},
									Properties: map[string]apiextensionsv1.JSONSchemaProps{
										"name":  *stringSchema,
										"port":  *numberSchema,
										"field": *stringSchema,
									},
								},
							},
						},
					},
				}},
				applyPatchOperation{
					"create instance with three soon-to-be-invalid keys",
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"field": []interface{}{
							map[string]interface{}{
								"name":  "nginx",
								"port":  int64(443),
								"field": "value",
							},
							map[string]interface{}{
								"name":  "etcd",
								"port":  int64(2379),
								"field": "value",
							},
							map[string]interface{}{
								"name":  "kube-apiserver",
								"port":  int64(6443),
								"field": "value",
							},
						},
					}},
				patchMyCRDV1Beta1Schema{
					"set `field`'s maxItems to 2, which is exceeded by all of previous object's elements",
					map[string]interface{}{
						"properties": map[string]interface{}{
							"field": map[string]interface{}{
								"maxItems": int64(2),
							},
						},
					}},
				applyPatchOperation{
					"reorder invalid objects which have too many properties, but do not modify them or change keys",
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"field": []interface{}{
							map[string]interface{}{
								"name":  "kube-apiserver",
								"port":  int64(6443),
								"field": "value",
							},
							map[string]interface{}{
								"name":  "nginx",
								"port":  int64(443),
								"field": "value",
							},
							map[string]interface{}{
								"name":  "etcd",
								"port":  int64(2379),
								"field": "value",
							},
						},
					}},
				expectError{
					applyPatchOperation{
						"attempt to change one of the fields of the items which exceed maxItems",
						myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
							"field": []interface{}{
								map[string]interface{}{
									"name":  "kube-apiserver",
									"port":  int64(6443),
									"field": "value",
								},
								map[string]interface{}{
									"name":  "nginx",
									"port":  int64(443),
									"field": "value",
								},
								map[string]interface{}{
									"name":  "etcd",
									"port":  int64(2379),
									"field": "value",
								},
								map[string]interface{}{
									"name":  "dev",
									"port":  int64(8080),
									"field": "value",
								},
							},
						}}},
				patchMyCRDV1Beta1Schema{
					"Require even numbered port in key, remove maxItems requirement",
					map[string]interface{}{
						"properties": map[string]interface{}{
							"field": map[string]interface{}{
								"maxItems": nil,
								"items": map[string]interface{}{
									"properties": map[string]interface{}{
										"port": map[string]interface{}{
											"multipleOf": int64(2),
										},
									},
								},
							},
						},
					}},

				applyPatchOperation{
					"reorder fields without changing anything",
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"field": []interface{}{
							map[string]interface{}{
								"name":  "nginx",
								"port":  int64(443),
								"field": "value",
							},
							map[string]interface{}{
								"name":  "etcd",
								"port":  int64(2379),
								"field": "value",
							},
							map[string]interface{}{
								"name":  "kube-apiserver",
								"port":  int64(6443),
								"field": "value",
							},
						},
					}},

				applyPatchOperation{
					`use "invalid" keys despite changing order and changing sibling fields to the key`,
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"field": []interface{}{
							map[string]interface{}{
								"name":  "nginx",
								"port":  int64(443),
								"field": "value",
							},
							map[string]interface{}{
								"name":  "etcd",
								"port":  int64(2379),
								"field": "value",
							},
							map[string]interface{}{
								"name":  "kube-apiserver",
								"port":  int64(6443),
								"field": "this is a changed value for an an invalid but grandfathered key",
							},
							map[string]interface{}{
								"name":  "dev",
								"port":  int64(8080),
								"field": "value",
							},
						},
					}},
			},
		},
		{
			Name: "ArrayItems do not correlate by index",
			Operations: []ratchetingTestOperation{
				updateMyCRDV1Beta1Schema{&apiextensionsv1.JSONSchemaProps{
					Type: "object",
					Properties: map[string]apiextensionsv1.JSONSchemaProps{
						"values": {
							Type: "array",
							Items: &apiextensionsv1.JSONSchemaPropsOrArray{
								Schema: stringMapSchema,
							},
						},
						"otherField": *stringSchema,
					},
				}},
				applyPatchOperation{
					"create instance with length 5 values",
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"values": []interface{}{
							map[string]interface{}{
								"name": "1",
								"key":  "value",
							},
							map[string]interface{}{
								"name": "2",
								"key":  "value",
							},
						},
					}},
				patchMyCRDV1Beta1Schema{
					"Set minimum length of 6 for values of elements in the items array",
					map[string]interface{}{
						"properties": map[string]interface{}{
							"values": map[string]interface{}{
								"items": map[string]interface{}{
									"additionalProperties": map[string]interface{}{
										"minLength": int64(6),
									},
								},
							},
						},
					}},
				expectError{
					applyPatchOperation{
						"change value to one that exceeds minLength",
						myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
							"values": []interface{}{
								map[string]interface{}{
									"name": "1",
									"key":  "value",
								},
								map[string]interface{}{
									"name": "2",
									"key":  "bad",
								},
							},
						}}},
				applyPatchOperation{
					"add new fields without touching the map",
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"values": []interface{}{
							map[string]interface{}{
								"name": "1",
								"key":  "value",
							},
							map[string]interface{}{
								"name": "2",
								"key":  "value",
							},
						},
						"otherField": "hello world",
					}},
				// (This test shows an array cannot be correlated by index with its old value)
				expectError{applyPatchOperation{
					"add new, valid fields to elements of the array, failing to ratchet unchanged old fields within the array elements by correlating by index due to atomic list",
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"values": []interface{}{
							map[string]interface{}{
								"name": "1",
								"key":  "value",
							},
							map[string]interface{}{
								"name": "2",
								"key":  "value",
								"key2": "valid value",
							},
						},
					}}},
				expectError{
					applyPatchOperation{
						"reorder the array, preventing index correlation",
						myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
							"values": []interface{}{
								map[string]interface{}{
									"name": "2",
									"key":  "value",
									"key2": "valid value",
								},
								map[string]interface{}{
									"name": "1",
									"key":  "value",
								},
							},
						}}},
			},
		},
		{
			Name:       "CEL Optional OldSelf",
			SkipStatus: true, // oldSelf can never be null for a status update.
			Operations: []ratchetingTestOperation{
				updateMyCRDV1Beta1Schema{&apiextensionsv1.JSONSchemaProps{
					Type: "object",
					Properties: map[string]apiextensionsv1.JSONSchemaProps{
						"field": {
							Type: "string",
							XValidations: []apiextensionsv1.ValidationRule{
								{
									Rule:            "!oldSelf.hasValue()",
									Message:         "oldSelf must be null",
									OptionalOldSelf: ptr(true),
								},
							},
						},
					},
				}},

				applyPatchOperation{
					"create instance passes since oldself is null",
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"field": "value",
					}},

				expectError{
					applyPatchOperation{
						"update field fails, since oldself is not null",
						myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
							"field": "value2",
						},
					},
				},

				expectError{
					applyPatchOperation{
						"noop update field fails, since oldself is not null and transition rules are not ratcheted",
						myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
							"field": "value",
						},
					},
				},
			},
		},
		// Features that should not ratchet
		{
			Name: "AllOf_should_not_ratchet",
		},
		{
			Name: "OneOf_should_not_ratchet",
		},
		{
			Name: "AnyOf_should_not_ratchet",
		},
		{
			Name: "Not_should_not_ratchet",
		},
		{
			Name: "CEL_transition_rules_should_not_ratchet",
			Operations: []ratchetingTestOperation{
				updateMyCRDV1Beta1Schema{&apiextensionsv1.JSONSchemaProps{
					Type:                   "object",
					XPreserveUnknownFields: ptr(true),
				}},
				applyPatchOperation{
					"create instance with strings that do not start with k8s",
					myCRDV1Beta1, myCRDInstanceName,
					`
						myStringField: myStringValue
						myOtherField: myOtherField
					`,
				},
				updateMyCRDV1Beta1Schema{&apiextensionsv1.JSONSchemaProps{
					Type:                   "object",
					XPreserveUnknownFields: ptr(true),
					Properties: map[string]apiextensionsv1.JSONSchemaProps{
						"myStringField": {
							Type: "string",
							XValidations: apiextensionsv1.ValidationRules{
								{
									Rule: "oldSelf != 'myStringValue' || self == 'validstring'",
								},
							},
						},
					},
				}},
				expectError{applyPatchOperation{
					"try to change one field to valid value, but unchanged field fails to be ratcheted by transition rule",
					myCRDV1Beta1, myCRDInstanceName,
					`
						myOtherField: myNewOtherField
						myStringField: myStringValue
					`,
				}},
				applyPatchOperation{
					"change both fields to valid values",
					myCRDV1Beta1, myCRDInstanceName,
					`
						myStringField: validstring
						myOtherField: myNewOtherField
					`,
				},
			},
		},
		// Future Functionality, disabled tests
		{
			Name: "CEL Add Change Rule",
			Operations: []ratchetingTestOperation{
				updateMyCRDV1Beta1Schema{&apiextensionsv1.JSONSchemaProps{
					Type: "object",
					Properties: map[string]apiextensionsv1.JSONSchemaProps{
						"field": {
							Type: "object",
							AdditionalProperties: &apiextensionsv1.JSONSchemaPropsOrBool{
								Schema: &apiextensionsv1.JSONSchemaProps{
									Type: "object",
									Properties: map[string]apiextensionsv1.JSONSchemaProps{
										"stringField":   *stringSchema,
										"intField":      *numberSchema,
										"otherIntField": *numberSchema,
									},
								},
							},
						},
					},
				}},
				applyPatchOperation{
					"create instance with strings that do not start with k8s",
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"field": map[string]interface{}{
							"object1": map[string]interface{}{
								"stringField": "a string",
								"intField":    int64(5),
							},
							"object2": map[string]interface{}{
								"stringField": "another string",
								"intField":    int64(15),
							},
							"object3": map[string]interface{}{
								"stringField": "a third string",
								"intField":    int64(7),
							},
						},
					}},
				patchMyCRDV1Beta1Schema{
					"require that stringField value start with `k8s`",
					map[string]interface{}{
						"properties": map[string]interface{}{
							"field": map[string]interface{}{
								"additionalProperties": map[string]interface{}{
									"properties": map[string]interface{}{
										"stringField": map[string]interface{}{
											"x-kubernetes-validations": []interface{}{
												map[string]interface{}{
													"rule":    "self.startsWith('k8s')",
													"message": "strings must have k8s prefix",
												},
											},
										},
									},
								},
							},
						},
					}},
				applyPatchOperation{
					"add a new entry that follows the new rule, ratchet old values",
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"field": map[string]interface{}{
							"object1": map[string]interface{}{
								"stringField": "a string",
								"intField":    int64(5),
							},
							"object2": map[string]interface{}{
								"stringField": "another string",
								"intField":    int64(15),
							},
							"object3": map[string]interface{}{
								"stringField": "a third string",
								"intField":    int64(7),
							},
							"object4": map[string]interface{}{
								"stringField": "k8s third string",
								"intField":    int64(7),
							},
						},
					}},
				applyPatchOperation{
					"modify a sibling to an invalid value, ratcheting the unchanged invalid value",
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"field": map[string]interface{}{
							"object1": map[string]interface{}{
								"stringField": "a string",
								"intField":    int64(15),
							},
							"object2": map[string]interface{}{
								"stringField":   "another string",
								"intField":      int64(10),
								"otherIntField": int64(20),
							},
						},
					}},
				expectError{
					applyPatchOperation{
						"change a previously ratcheted field to an invalid value",
						myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
							"field": map[string]interface{}{
								"object2": map[string]interface{}{
									"stringField": "a changed string",
								},
								"object3": map[string]interface{}{
									"stringField": "a changed third string",
								},
							},
						}}},
				patchMyCRDV1Beta1Schema{
					"require that stringField values are also odd length",
					map[string]interface{}{
						"properties": map[string]interface{}{
							"field": map[string]interface{}{
								"additionalProperties": map[string]interface{}{
									"stringField": map[string]interface{}{
										"x-kubernetes-validations": []interface{}{
											map[string]interface{}{
												"rule":    "self.startsWith('k8s')",
												"message": "strings must have k8s prefix",
											},
											map[string]interface{}{
												"rule":    "len(self) % 2 == 1",
												"message": "strings must have odd length",
											},
										},
									},
								},
							},
						},
					}},
				applyPatchOperation{
					"have mixed ratcheting of one or two CEL rules, object4 is ratcheted by one rule, object1 is ratcheting 2 rules",
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"field": map[string]interface{}{
							"object1": map[string]interface{}{
								"stringField": "a string", // invalid. even number length, no k8s prefix
								"intField":    int64(1000),
							},
							"object4": map[string]interface{}{
								"stringField": "k8s third string", // invalid. even number length. ratcheted
								"intField":    int64(7000),
							},
						},
					}},
				expectError{
					applyPatchOperation{
						"swap keys between valuesin the map",
						myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
							"field": map[string]interface{}{
								"object1": map[string]interface{}{
									"stringField": "k8s third string",
									"intField":    int64(1000),
								},
								"object4": map[string]interface{}{
									"stringField": "a string",
									"intField":    int64(7000),
								},
							},
						}}},
				applyPatchOperation{
					"fix keys",
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"field": map[string]interface{}{
							"object1": map[string]interface{}{
								"stringField": "k8s a stringy",
								"intField":    int64(1000),
							},
							"object4": map[string]interface{}{
								"stringField": "k8s third stringy",
								"intField":    int64(7000),
							},
						},
					}},
			},
		},
		{
			// Changing a list to a set should allow you to keep the items the
			// same, but if you modify any one item the set must be unique
			//
			// Possibly a future area of improvement. As it stands now,
			// SSA implementation is incompatible with ratcheting this field:
			// https://github.com/kubernetes/kubernetes/blob/ec9a8ffb237e391ce9ccc58de93ba4ecc2fabf42/staging/src/k8s.io/apimachinery/pkg/util/managedfields/internal/structuredmerge.go#L146-L149
			//
			// Throws error trying to interpret an invalid existing `liveObj`
			// as a set.
			Name:     "Change list to set",
			Disabled: true,
			Operations: []ratchetingTestOperation{
				updateMyCRDV1Beta1Schema{&apiextensionsv1.JSONSchemaProps{
					Type: "object",
					Properties: map[string]apiextensionsv1.JSONSchemaProps{
						"values": {
							Type: "object",
							AdditionalProperties: &apiextensionsv1.JSONSchemaPropsOrBool{
								Schema: &apiextensionsv1.JSONSchemaProps{
									Type: "array",
									Items: &apiextensionsv1.JSONSchemaPropsOrArray{
										Schema: numberSchema,
									},
								},
							},
						},
					},
				}},
				applyPatchOperation{
					"reate a list of numbers with duplicates using the old simple schema",
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"values": map[string]interface{}{
							"dups": []interface{}{int64(1), int64(2), int64(2), int64(3), int64(1000), int64(2000)},
						},
					}},
				patchMyCRDV1Beta1Schema{
					"change list type to set",
					map[string]interface{}{
						"properties": map[string]interface{}{
							"values": map[string]interface{}{
								"additionalProperties": map[string]interface{}{
									"x-kubernetes-list-type": "set",
								},
							},
						},
					}},
				expectError{
					applyPatchOperation{
						"change original without removing duplicates",
						myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
							"values": map[string]interface{}{
								"dups": []interface{}{int64(1), int64(2), int64(2), int64(3), int64(1000), int64(2000), int64(3)},
							},
						}}},
				expectError{applyPatchOperation{
					"add another list with duplicates",
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"values": map[string]interface{}{
							"dups":  []interface{}{int64(1), int64(2), int64(2), int64(3), int64(1000), int64(2000)},
							"dups2": []interface{}{int64(1), int64(2), int64(2), int64(3), int64(1000), int64(2000)},
						},
					}}},
				// Can add a valid sibling field
				//! Remove this ExpectError if/when we add support for ratcheting
				// the type of a list
				applyPatchOperation{
					"add a valid sibling field",
					myCRDV1Beta1, myCRDInstanceName, map[string]interface{}{
						"values": map[string]interface{}{
							"dups":       []interface{}{int64(1), int64(2), int64(2), int64(3), int64(1000), int64(2000)},
							"otherField": []interface{}{int64(1), int64(2), int64(3)},
						},
					}},
				// Can remove dups to make valid
				//! Normally this would be valid, but SSA is unable to interpret
				// the `liveObj` in the new schema, so fails. Changing
				// x-kubernetes-list-type from anything to a set is unsupported by SSA.
				applyPatchOperation{
					"remove dups to make list valid",
					myCRDV1Beta1,
					myCRDInstanceName,
					map[string]interface{}{
						"values": map[string]interface{}{
							"dups":       []interface{}{int64(1), int64(3), int64(1000), int64(2000)},
							"otherField": []interface{}{int64(1), int64(2), int64(3)},
						},
					}},
			},
		},
	}

	runTests(t, cases)
}

func ptr[T any](v T) *T {
	return &v
}

type validator func(new, old *unstructured.Unstructured)

func newValidator(customResourceValidation *apiextensionsinternal.JSONSchemaProps, kind schema.GroupVersionKind, namespaceScoped bool) (validator, error) {
	// Replicate customResourceStrategy validation
	openapiSchema := &spec.Schema{}
	if customResourceValidation != nil {
		// TODO: replace with NewStructural(...).ToGoOpenAPI
		if err := apiservervalidation.ConvertJSONSchemaPropsWithPostProcess(customResourceValidation, openapiSchema, apiservervalidation.StripUnsupportedFormatsPostProcess); err != nil {
			return nil, err
		}
	}

	schemaValidator := apiservervalidation.NewRatchetingSchemaValidator(
		openapiSchema,
		nil,
		"",
		strfmt.Default)
	sts, err := structuralschema.NewStructural(customResourceValidation)
	if err != nil {
		return nil, err
	}

	strategy := customresource.NewStrategy(
		nil, // No need for typer, since only using validation
		namespaceScoped,
		kind,
		schemaValidator,
		nil, // No status schema validator
		sts,
		nil, // No need for status
		nil, // No need for scale
		nil, // No need for selectable fields
	)

	return func(new, old *unstructured.Unstructured) {
		_ = strategy.ValidateUpdate(context.TODO(), new, old)
	}, nil
}

// Recursively walks the provided directory and parses the YAML files into
// unstructured objects. If there are more than one object in a single file,
// they are all added to the returned slice.
func loadObjects(dir string) []*unstructured.Unstructured {
	result := []*unstructured.Unstructured{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		} else if d.IsDir() {
			return nil
		} else if filepath.Ext(d.Name()) != ".yaml" {
			return nil
		}
		// Read the file in as []byte
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)

		// Split the data by YAML drame
		for {
			parsed := &unstructured.Unstructured{}
			if err := decoder.Decode(parsed); err != nil {
				if errors.Is(err, io.EOF) {
					break
				}
				return err
			}

			result = append(result, parsed)
		}

		return nil
	})
	if err != nil {
		panic(err)
	}
	return result
}

func BenchmarkRatcheting(b *testing.B) {
	// Walk directory with CRDs, for each file parse YAML with multiple CRDs in it.
	// Keep track in a map a validator for each unique gvk
	crdObjects := loadObjects("ratcheting_test_cases/crds")
	invalidFiles := loadObjects("ratcheting_test_cases/invalid")
	validFiles := loadObjects("ratcheting_test_cases/valid")

	// Create a validator for each GVK.
	validators := map[schema.GroupVersionKind]validator{}
	for _, crd := range crdObjects {
		parsed := apiextensionsv1.CustomResourceDefinition{}
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(crd.Object, &parsed); err != nil {
			b.Fatalf("Failed to parse CRD %v", err)
			return
		}

		for _, v := range parsed.Spec.Versions {
			gvk := schema.GroupVersionKind{
				Group:   parsed.Spec.Group,
				Version: v.Name,
				Kind:    parsed.Spec.Names.Kind,
			}

			// Create structural schema from v.Schema.OpenAPIV3Schema
			internalValidation := &apiextensionsinternal.CustomResourceValidation{}
			if err := apiextensionsv1.Convert_v1_CustomResourceValidation_To_apiextensions_CustomResourceValidation(v.Schema, internalValidation, nil); err != nil {
				b.Fatal(fmt.Errorf("failed converting CRD validation to internal version: %v", err))
				return
			}

			validator, err := newValidator(internalValidation.OpenAPIV3Schema, gvk, parsed.Spec.Scope == apiextensionsv1.NamespaceScoped)
			if err != nil {
				b.Fatal(err)
				return
			}
			validators[gvk] = validator
		}

	}

	// Organize all the files by GVK.
	gvksToValidFiles := map[schema.GroupVersionKind][]*unstructured.Unstructured{}
	gvksToInvalidFiles := map[schema.GroupVersionKind][]*unstructured.Unstructured{}

	for _, valid := range validFiles {
		gvk := valid.GroupVersionKind()
		gvksToValidFiles[gvk] = append(gvksToValidFiles[gvk], valid)
	}

	for _, invalid := range invalidFiles {
		gvk := invalid.GroupVersionKind()
		gvksToInvalidFiles[gvk] = append(gvksToInvalidFiles[gvk], invalid)
	}

	// Remove any GVKs for which we dont have both valid and invalid files.
	for gvk := range gvksToValidFiles {
		if _, ok := gvksToInvalidFiles[gvk]; !ok {
			delete(gvksToValidFiles, gvk)
		}
	}

	for gvk := range gvksToInvalidFiles {
		if _, ok := gvksToValidFiles[gvk]; !ok {
			delete(gvksToInvalidFiles, gvk)
		}
	}

	type pair struct {
		old *unstructured.Unstructured
		new *unstructured.Unstructured
	}

	// For each valid file, match it with every invalid file of the same GVK
	validXValidPairs := []pair{}
	validXInvalidPairs := []pair{}
	invalidXInvalidPairs := []pair{}

	for gvk, valids := range gvksToValidFiles {
		for _, validOld := range valids {
			for _, validNew := range gvksToValidFiles[gvk] {
				validXValidPairs = append(validXValidPairs, pair{old: validOld, new: validNew})
			}
		}
	}

	for gvk, valids := range gvksToValidFiles {
		for _, valid := range valids {
			for _, invalid := range gvksToInvalidFiles[gvk] {
				validXInvalidPairs = append(validXInvalidPairs, pair{old: valid, new: invalid})
			}
		}
	}

	// For each invalid file, add pair with every other invalid file of the same
	// GVK including itself
	for gvk, invalids := range gvksToInvalidFiles {
		for _, invalid := range invalids {
			for _, invalid2 := range gvksToInvalidFiles[gvk] {
				invalidXInvalidPairs = append(invalidXInvalidPairs, pair{old: invalid, new: invalid2})
			}
		}
	}

	// For each pair, run the ratcheting algorithm on the update.
	//
	for _, ratchetingEnabled := range []bool{true, false} {
		name := "RatchetingEnabled"
		if !ratchetingEnabled {
			name = "RatchetingDisabled"
		}
		b.Run(name, func(b *testing.B) {
			featuregatetesting.SetFeatureGateDuringTest(b, utilfeature.DefaultFeatureGate, features.CRDValidationRatcheting, ratchetingEnabled)
			b.ResetTimer()

			do := func(pairs []pair) {
				for _, pair := range pairs {
					// Create a validator for the GVK of the valid object.
					validator, ok := validators[pair.old.GroupVersionKind()]
					if !ok {
						b.Log("No validator for GVK", pair.old.GroupVersionKind())
						continue
					}

					// Run the ratcheting algorithm on the update.
					// Don't care about result for benchmark
					validator(pair.old, pair.new)
				}
			}

			b.Run("ValidXValid", func(b *testing.B) {
				for i := 0; i < b.N; i++ {
					do(validXValidPairs)
				}
			})

			b.Run("ValidXInvalid", func(b *testing.B) {
				for i := 0; i < b.N; i++ {
					do(validXInvalidPairs)
				}
			})

			b.Run("InvalidXInvalid", func(b *testing.B) {
				for i := 0; i < b.N; i++ {
					do(invalidXInvalidPairs)
				}
			})
		})
	}
}

func TestRatchetingDropFields(t *testing.T) {
	// Field dropping only takes effect when feature is disabled
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.CRDValidationRatcheting, false)
	tearDown, apiExtensionClient, _, err := fixtures.StartDefaultServerWithClients(t)
	if err != nil {
		t.Fatal(err)
	}
	defer tearDown()

	group := myCRDV1Beta1.Group
	version := myCRDV1Beta1.Version
	resource := myCRDV1Beta1.Resource
	kind := fakeRESTMapper[myCRDV1Beta1]

	myCRD := &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: resource + "." + group},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: group,
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{
				Name:    version,
				Served:  true,
				Storage: true,
				Schema: &apiextensionsv1.CustomResourceValidation{
					OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
						Type: "object",
						Properties: map[string]apiextensionsv1.JSONSchemaProps{
							"spec": {
								Type: "object",
								Properties: map[string]apiextensionsv1.JSONSchemaProps{
									"field": {
										Type: "string",
										XValidations: []apiextensionsv1.ValidationRule{
											{
												// Results in error if field wasn't dropped
												Rule:            "self == oldSelf",
												OptionalOldSelf: ptr(true),
											},
										},
									},
								},
							},
						},
					},
				},
			}},
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Plural:   resource,
				Kind:     kind,
				ListKind: kind + "List",
			},
			Scope: apiextensionsv1.NamespaceScoped,
		},
	}

	created, err := apiExtensionClient.ApiextensionsV1().CustomResourceDefinitions().Create(context.TODO(), myCRD, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if created.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"].Properties["field"].XValidations[0].OptionalOldSelf != nil {
		t.Errorf("Expected OpeiontalOldSelf field to be dropped for create when feature gate is disabled")
	}

	var updated *apiextensionsv1.CustomResourceDefinition
	err = wait.PollUntilContextTimeout(context.TODO(), 100*time.Millisecond, 5*time.Second, true, func(ctx context.Context) (bool, error) {
		existing, err := apiExtensionClient.ApiextensionsV1().CustomResourceDefinitions().Get(context.TODO(), created.Name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		existing.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"].Properties["field"].XValidations[0].OptionalOldSelf = ptr(true)
		updated, err = apiExtensionClient.ApiextensionsV1().CustomResourceDefinitions().Update(context.TODO(), existing, metav1.UpdateOptions{})
		if err != nil {
			if apierrors.IsConflict(err) {
				return false, nil
			}
			return false, err
		}
		return true, nil
	})
	if err != nil {
		t.Fatalf("unexpected error waiting for CRD update: %v", err)
	}

	if updated.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"].Properties["field"].XValidations[0].OptionalOldSelf != nil {
		t.Errorf("Expected OpeiontalOldSelf field to be dropped for update when feature gate is disabled")
	}
}
