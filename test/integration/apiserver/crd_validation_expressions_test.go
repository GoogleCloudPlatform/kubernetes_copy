/*
Copyright 2021 The Kubernetes Authors.

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

package apiserver

import (
	"context"
	"fmt"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsv1beta1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1beta1"
	"k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	"k8s.io/apiextensions-apiserver/test/integration/fixtures"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/json"
	"k8s.io/apiserver/pkg/storage/names"
	"k8s.io/client-go/dynamic"
	apiservertesting "k8s.io/kubernetes/cmd/kube-apiserver/app/testing"
	"k8s.io/kubernetes/test/integration/framework"
)

// TestCustomResourceValidators tests x-kubernetes-validations compile and validate as expected when the feature gate
// is enabled.
func TestCustomResourceValidators(t *testing.T) {
	server, err := apiservertesting.StartTestServer(t, apiservertesting.NewDefaultTestServerOptions(), nil, framework.SharedEtcd())
	if err != nil {
		t.Fatal(err)
	}
	defer server.TearDownFn()
	config := server.ClientConfig

	apiExtensionClient, err := clientset.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("Structural schema", func(t *testing.T) {
		structuralWithValidators := crdWithSchema(t, "Structural", structuralSchemaWithValidators)
		crd, err := fixtures.CreateNewV1CustomResourceDefinition(structuralWithValidators, apiExtensionClient, dynamicClient)
		if err != nil {
			t.Fatal(err)
		}
		gvr := schema.GroupVersionResource{
			Group:    crd.Spec.Group,
			Version:  crd.Spec.Versions[0].Name,
			Resource: crd.Spec.Names.Plural,
		}
		crClient := dynamicClient.Resource(gvr)

		t.Run("CRD creation MUST allow data that is valid according to x-kubernetes-validations", func(t *testing.T) {
			name1 := names.SimpleNameGenerator.GenerateName("cr-1")
			_, err = crClient.Create(context.TODO(), &unstructured.Unstructured{Object: map[string]interface{}{
				"apiVersion": gvr.Group + "/" + gvr.Version,
				"kind":       crd.Spec.Names.Kind,
				"metadata": map[string]interface{}{
					"name": name1,
				},
				"spec": map[string]interface{}{
					"x":     int64(2),
					"y":     int64(2),
					"limit": int64(123),
				},
			}}, metav1.CreateOptions{})
			if err != nil {
				t.Errorf("Failed to create custom resource: %v", err)
			}
		})
		t.Run("custom resource create and update MUST NOT allow data that is invalid according to x-kubernetes-validations if the feature gate is enabled", func(t *testing.T) {
			name1 := names.SimpleNameGenerator.GenerateName("cr-1")

			// a spec create that is invalid MUST fail validation
			cr := &unstructured.Unstructured{Object: map[string]interface{}{
				"apiVersion": gvr.Group + "/" + gvr.Version,
				"kind":       crd.Spec.Names.Kind,
				"metadata": map[string]interface{}{
					"name": name1,
				},
				"spec": map[string]interface{}{
					"x": int64(-1),
					"y": int64(0),
				},
			}}

			// a spec create that is invalid MUST fail validation
			_, err = crClient.Create(context.TODO(), cr, metav1.CreateOptions{})
			if err == nil {
				t.Fatal("Expected create of invalid custom resource to fail")
			} else {
				if !strings.Contains(err.Error(), "failed rule: self.spec.x + self.spec.y") {
					t.Fatalf("Expected error to contain %s but got %v", "failed rule: self.spec.x + self.spec.y", err.Error())
				}
			}

			// a spec create that is valid MUST pass validation
			cr.Object["spec"] = map[string]interface{}{
				"x":     int64(2),
				"y":     int64(2),
				"extra": "anything?",
				"floatMap": map[string]interface{}{
					"key1": 0.2,
					"key2": 0.3,
				},
				"assocList": []interface{}{
					map[string]interface{}{
						"k": "a",
						"v": "1",
					},
				},
				"limit": nil,
			}

			cr, err := crClient.Create(context.TODO(), cr, metav1.CreateOptions{})
			if err != nil {
				t.Fatalf("Unexpected error creating custom resource: %v", err)
			}

			// spec updates that are invalid MUST fail validation
			cases := []struct {
				name string
				spec map[string]interface{}
			}{
				{
					name: "spec vs. status default value",
					spec: map[string]interface{}{
						"x": 3,
						"y": -4,
					},
				},
				{
					name: "nested string field",
					spec: map[string]interface{}{
						"extra": "something",
					},
				},
				{
					name: "nested array",
					spec: map[string]interface{}{
						"floatMap": map[string]interface{}{
							"key1": 0.1,
							"key2": 0.2,
						},
					},
				},
				{
					name: "nested associative list",
					spec: map[string]interface{}{
						"assocList": []interface{}{
							map[string]interface{}{
								"k": "a",
								"v": "2",
							},
						},
					},
				},
			}
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					cr.Object["spec"] = tc.spec

					_, err = crClient.Update(context.TODO(), cr, metav1.UpdateOptions{})
					if err == nil {
						t.Fatal("Expected invalid update of custom resource to fail")
					} else {
						if !strings.Contains(err.Error(), "failed rule") {
							t.Fatalf("Expected error to contain %s but got %v", "failed rule", err.Error())
						}
					}
				})
			}

			// a status update that is invalid MUST fail validation
			cr.Object["status"] = map[string]interface{}{
				"z": int64(5),
			}
			_, err = crClient.UpdateStatus(context.TODO(), cr, metav1.UpdateOptions{})
			if err == nil {
				t.Fatal("Expected invalid update of custom resource status to fail")
			} else {
				if !strings.Contains(err.Error(), "failed rule: self.spec.x + self.spec.y") {
					t.Fatalf("Expected error to contain %s but got %v", "failed rule: self.spec.x + self.spec.y", err.Error())
				}
			}

			// a status update this is valid MUST pass validation
			cr.Object["status"] = map[string]interface{}{
				"z": int64(3),
			}

			_, err = crClient.UpdateStatus(context.TODO(), cr, metav1.UpdateOptions{})
			if err != nil {
				t.Fatalf("Unexpected error updating custom resource status: %v", err)
			}
		})
	})
	t.Run("CRD writes MUST fail for a non-structural schema containing x-kubernetes-validations", func(t *testing.T) {
		// The only way for a non-structural schema to exist is for it to already be persisted in etcd as a non-structural CRD.
		nonStructuralCRD, err := fixtures.CreateCRDUsingRemovedAPI(server.EtcdClient, server.EtcdStoragePrefix, nonStructuralCrdWithValidations(), apiExtensionClient, dynamicClient)
		if err != nil {
			t.Fatalf("Unexpected error non-structural CRD by writing directly to etcd: %v", err)
		}
		// Double check that the schema is non-structural
		crd, err := apiExtensionClient.ApiextensionsV1().CustomResourceDefinitions().Get(context.TODO(), nonStructuralCRD.Name, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}
		nonStructural := false
		for _, c := range crd.Status.Conditions {
			if c.Type == apiextensionsv1.NonStructuralSchema {
				nonStructural = true
			}
		}
		if !nonStructural {
			t.Fatal("Expected CRD to be non-structural")
		}

		//Try to change it
		crd.Spec.Versions[0].Schema.OpenAPIV3Schema.XValidations = apiextensionsv1.ValidationRules{
			{
				Rule: "has(self.foo)",
			},
		}
		_, err = apiExtensionClient.ApiextensionsV1().CustomResourceDefinitions().Update(context.TODO(), crd, metav1.UpdateOptions{})
		if err == nil {
			t.Fatal("Expected error")
		}
	})
	t.Run("CRD creation MUST fail if a x-kubernetes-validations rule accesses a metadata field other than name", func(t *testing.T) {
		structuralWithValidators := crdWithSchema(t, "InvalidStructuralMetadata", structuralSchemaWithInvalidMetadataValidators)
		_, err := fixtures.CreateNewV1CustomResourceDefinition(structuralWithValidators, apiExtensionClient, dynamicClient)
		if err == nil {
			t.Error("Expected error creating custom resource but got none")
		} else if !strings.Contains(err.Error(), "undefined field 'labels'") {
			t.Errorf("Expected error to contain %s but got %v", "undefined field 'labels'", err.Error())
		}
	})
	t.Run("CRD creation MUST pass if a x-kubernetes-validations rule accesses metadata.name", func(t *testing.T) {
		structuralWithValidators := crdWithSchema(t, "ValidStructuralMetadata", structuralSchemaWithValidMetadataValidators)
		_, err := fixtures.CreateNewV1CustomResourceDefinition(structuralWithValidators, apiExtensionClient, dynamicClient)
		if err != nil {
			t.Error("Unexpected error creating custom resource but metadata validation rule")
		}
	})
	t.Run("CRD creation MUST pass for an CRD with empty field", func(t *testing.T) {
		structuralWithValidators := crdWithSchema(t, "WithEmptyObject", structuralSchemaWithEmptyObject)
		_, err := fixtures.CreateNewV1CustomResourceDefinition(structuralWithValidators, apiExtensionClient, dynamicClient)
		if err != nil {
			t.Errorf("unexpected error creating CRD with empty field: %v", err)
		}
	})
	t.Run("CR creation MUST fail if a x-kubernetes-validations rule exceeds the runtime cost limit", func(t *testing.T) {
		structuralWithValidators := crdWithSchema(t, "RuntimeCostLimit", structuralSchemaWithCostLimit)
		crd, err := fixtures.CreateNewV1CustomResourceDefinition(structuralWithValidators, apiExtensionClient, dynamicClient)
		if err != nil {
			t.Errorf("Unexpected error creating custom resource definition: %v", err)
		}
		gvr := schema.GroupVersionResource{
			Group:    crd.Spec.Group,
			Version:  crd.Spec.Versions[0].Name,
			Resource: crd.Spec.Names.Plural,
		}
		crClient := dynamicClient.Resource(gvr)
		name1 := names.SimpleNameGenerator.GenerateName("cr-1")
		cr := &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": gvr.Group + "/" + gvr.Version,
			"kind":       crd.Spec.Names.Kind,
			"metadata": map[string]interface{}{
				"name": name1,
			},
			"spec": map[string]interface{}{
				"list": genLargeArray(725, 20),
			},
		}}
		_, err = crClient.Create(context.TODO(), cr, metav1.CreateOptions{})
		if err == nil {
			t.Fatal("Expected error creating custom resource")
		} else if !strings.Contains(err.Error(), "call cost exceeds limit") {
			t.Errorf("Expected error to contain %s but got %v", "call cost exceeds limit", err.Error())
		}
	})
	t.Run("Schema with valid transition rule", func(t *testing.T) {
		structuralWithValidators := crdWithSchema(t, "ValidTransitionRule", structuralSchemaWithValidTransitionRule)
		crd, err := fixtures.CreateNewV1CustomResourceDefinition(structuralWithValidators, apiExtensionClient, dynamicClient)
		if err != nil {
			t.Fatal(err)
		}
		gvr := schema.GroupVersionResource{
			Group:    crd.Spec.Group,
			Version:  crd.Spec.Versions[0].Name,
			Resource: crd.Spec.Names.Plural,
		}
		crClient := dynamicClient.Resource(gvr)

		t.Run("custom resource update MUST pass if a x-kubernetes-validations rule contains a valid transition rule", func(t *testing.T) {
			name1 := names.SimpleNameGenerator.GenerateName("cr-1")
			cr := &unstructured.Unstructured{Object: map[string]interface{}{
				"apiVersion": gvr.Group + "/" + gvr.Version,
				"kind":       crd.Spec.Names.Kind,
				"metadata": map[string]interface{}{
					"name": name1,
				},
				"spec": map[string]interface{}{
					"someImmutableThing": "original",
					"somethingElse":      "original",
				},
			}}
			cr, err = crClient.Create(context.TODO(), cr, metav1.CreateOptions{})
			if err != nil {
				t.Fatalf("Unexpected error creating custom resource: %v", err)
			}
			cr.Object["spec"].(map[string]interface{})["somethingElse"] = "new value"
			_, err = crClient.Update(context.TODO(), cr, metav1.UpdateOptions{})
			if err != nil {
				t.Fatalf("Unexpected error updating custom resource: %v", err)
			}
		})
		t.Run("custom resource update MUST fail if a x-kubernetes-validations rule contains an invalid transition rule", func(t *testing.T) {
			name1 := names.SimpleNameGenerator.GenerateName("cr-1")
			cr := &unstructured.Unstructured{Object: map[string]interface{}{
				"apiVersion": gvr.Group + "/" + gvr.Version,
				"kind":       crd.Spec.Names.Kind,
				"metadata": map[string]interface{}{
					"name": name1,
				},
				"spec": map[string]interface{}{
					"someImmutableThing": "original",
					"somethingElse":      "original",
				},
			}}
			cr, err = crClient.Create(context.TODO(), cr, metav1.CreateOptions{})
			if err != nil {
				t.Fatalf("Unexpected error creating custom resource: %v", err)
			}
			cr.Object["spec"].(map[string]interface{})["someImmutableThing"] = "new value"
			_, err = crClient.Update(context.TODO(), cr, metav1.UpdateOptions{})
			if err == nil {
				t.Fatalf("Expected error updating custom resource: %v", err)
			} else if !strings.Contains(err.Error(), "failed rule: self.someImmutableThing == oldSelf.someImmutableThing") {
				t.Errorf("Expected error to contain %s but got %v", "failed rule: self.someImmutableThing == oldSelf.someImmutableThing", err.Error())
			}
		})
	})

	t.Run("CRD creation MUST fail if a x-kubernetes-validations rule contains invalid transition rule", func(t *testing.T) {
		structuralWithValidators := crdWithSchema(t, "InvalidTransitionRule", structuralSchemaWithInvalidTransitionRule)
		_, err := fixtures.CreateNewV1CustomResourceDefinition(structuralWithValidators, apiExtensionClient, dynamicClient)
		if err == nil {
			t.Error("Expected error creating custom resource but got none")
		} else if !strings.Contains(err.Error(), "oldSelf cannot be used on the uncorrelatable portion of the schema") {
			t.Errorf("Expected error to contain %s but got %v", "oldSelf cannot be used on the uncorrelatable portion of the schema", err.Error())
		}
	})
	t.Run("Schema with default map key transition rule", func(t *testing.T) {
		structuralWithValidators := crdWithSchema(t, "DefaultMapKeyTransitionRule", structuralSchemaWithDefaultMapKeyTransitionRule)
		crd, err := fixtures.CreateNewV1CustomResourceDefinition(structuralWithValidators, apiExtensionClient, dynamicClient)
		if err != nil {
			t.Fatal(err)
		}
		gvr := schema.GroupVersionResource{
			Group:    crd.Spec.Group,
			Version:  crd.Spec.Versions[0].Name,
			Resource: crd.Spec.Names.Plural,
		}
		crClient := dynamicClient.Resource(gvr)

		t.Run("custom resource update MUST fail if a x-kubernetes-validations if a transition rule contained in a mapList with default map keys fails validation", func(t *testing.T) {
			name1 := names.SimpleNameGenerator.GenerateName("cr-1")
			cr := &unstructured.Unstructured{Object: map[string]interface{}{
				"apiVersion": gvr.Group + "/" + gvr.Version,
				"kind":       crd.Spec.Names.Kind,
				"metadata": map[string]interface{}{
					"name": name1,
				},
				"spec": map[string]interface{}{
					"list": []interface{}{
						map[string]interface{}{
							"k1": "x",
							"v":  "value",
						},
					},
				},
			}}
			cr, err = crClient.Create(context.TODO(), cr, metav1.CreateOptions{})
			if err != nil {
				t.Fatalf("Unexpected error creating custom resource: %v", err)
			}
			item := cr.Object["spec"].(map[string]interface{})["list"].([]interface{})[0].(map[string]interface{})
			item["k2"] = "DEFAULT"
			item["v"] = "new value"
			_, err = crClient.Update(context.TODO(), cr, metav1.UpdateOptions{})
			if err == nil {
				t.Fatalf("Expected error updating custom resource: %v", err)
			} else if !strings.Contains(err.Error(), "failed rule: self.v == oldSelf.v") {
				t.Errorf("Expected error to contain %s but got %v", "failed rule: self.v == oldSelf.v", err.Error())
			}
		})
	})
}

// TestCustomResourceValidatorsWithBlockingErrors tests x-kubernetes-validations is skipped when
// blocking errors occurred.
func TestCustomResourceValidatorsWithBlockingErrors(t *testing.T) {
	server, err := apiservertesting.StartTestServer(t, apiservertesting.NewDefaultTestServerOptions(), nil, framework.SharedEtcd())
	if err != nil {
		t.Fatal(err)
	}
	defer server.TearDownFn()
	config := server.ClientConfig

	apiExtensionClient, err := clientset.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("Structural schema", func(t *testing.T) {
		structuralWithValidators := crdWithSchema(t, "Structural", structuralSchemaWithBlockingErr)
		crd, err := fixtures.CreateNewV1CustomResourceDefinition(structuralWithValidators, apiExtensionClient, dynamicClient)
		if err != nil {
			t.Fatal(err)
		}
		gvr := schema.GroupVersionResource{
			Group:    crd.Spec.Group,
			Version:  crd.Spec.Versions[0].Name,
			Resource: crd.Spec.Names.Plural,
		}
		crClient := dynamicClient.Resource(gvr)

		t.Run("CRD creation MUST allow data that is valid according to x-kubernetes-validations", func(t *testing.T) {
			name1 := names.SimpleNameGenerator.GenerateName("cr-1")
			_, err = crClient.Create(context.TODO(), &unstructured.Unstructured{Object: map[string]interface{}{
				"apiVersion": gvr.Group + "/" + gvr.Version,
				"kind":       crd.Spec.Names.Kind,
				"metadata": map[string]interface{}{
					"name": name1,
				},
				"spec": map[string]interface{}{
					"x":     int64(2),
					"y":     int64(2),
					"limit": int64(123),
				},
			}}, metav1.CreateOptions{})
			if err != nil {
				t.Errorf("Failed to create custom resource: %v", err)
			}
		})
		t.Run("custom resource create and update MUST NOT allow data if failed validation", func(t *testing.T) {
			name1 := names.SimpleNameGenerator.GenerateName("cr-1")

			// a spec create that is invalid MUST fail validation
			cr := &unstructured.Unstructured{Object: map[string]interface{}{
				"apiVersion": gvr.Group + "/" + gvr.Version,
				"kind":       crd.Spec.Names.Kind,
				"metadata": map[string]interface{}{
					"name": name1,
				},
				"spec": map[string]interface{}{
					"x": int64(-1),
					"y": int64(0),
				},
			}}

			// a spec create that is invalid MUST fail validation
			_, err = crClient.Create(context.TODO(), cr, metav1.CreateOptions{})
			if err == nil {
				t.Fatal("Expected create of invalid custom resource to fail")
			} else {
				if !strings.Contains(err.Error(), "self.spec.x + self.spec.y must be greater than or equal to 0") {
					t.Fatalf("Expected error to contain %s but got %v", "self.spec.x + self.spec.y must be greater than or equal to 0", err.Error())
				}
			}
		})
		t.Run("custom resource create and update MUST NOT allow data if there is blocking error of MaxLength", func(t *testing.T) {
			name2 := names.SimpleNameGenerator.GenerateName("cr-2")

			// a spec create that has maxLength err MUST fail validation
			cr := &unstructured.Unstructured{Object: map[string]interface{}{
				"apiVersion": gvr.Group + "/" + gvr.Version,
				"kind":       crd.Spec.Names.Kind,
				"metadata": map[string]interface{}{
					"name": name2,
				},
				"spec": map[string]interface{}{
					"x":     int64(2),
					"y":     int64(2),
					"extra": strings.Repeat("x", 201),
					"floatMap": map[string]interface{}{
						"key1": 0.2,
						"key2": 0.3,
					},
					"limit": nil,
				},
			}}

			_, err := crClient.Create(context.TODO(), cr, metav1.CreateOptions{})
			if err == nil || !strings.Contains(err.Error(), "some validation rules were not checked because the object was invalid; correct the existing errors to complete validation") {
				t.Fatalf("expect error to contain \"some validation rules were not checked because the object was invalid; correct the existing errors to complete validation\" but get: %v", err)
			}
		})
		t.Run("custom resource create and update MUST NOT allow data if there is blocking error of MaxItems", func(t *testing.T) {
			name2 := names.SimpleNameGenerator.GenerateName("cr-2")
			// a spec create that has maxItem err MUST fail validation
			cr := &unstructured.Unstructured{Object: map[string]interface{}{
				"apiVersion": gvr.Group + "/" + gvr.Version,
				"kind":       crd.Spec.Names.Kind,
				"metadata": map[string]interface{}{
					"name": name2,
				},
				"spec": map[string]interface{}{
					"x": int64(2),
					"y": int64(2),
					"floatMap": map[string]interface{}{
						"key1": 0.2,
						"key2": 0.3,
					},
					"assocList": []interface{}{},
					"limit":     nil,
				},
			}}
			assocList := cr.Object["spec"].(map[string]interface{})["assocList"].([]interface{})
			for i := 1; i <= 101; i++ {
				assocList = append(assocList, map[string]interface{}{
					"k": "a",
					"v": fmt.Sprintf("%d", i),
				})
			}
			cr.Object["spec"].(map[string]interface{})["assocList"] = assocList

			_, err = crClient.Create(context.TODO(), cr, metav1.CreateOptions{})
			if err == nil || !strings.Contains(err.Error(), "some validation rules were not checked because the object was invalid; correct the existing errors to complete validation") {
				t.Fatalf("expect error to contain \"some validation rules were not checked because the object was invalid; correct the existing errors to complete validation\" but get: %v", err)
			}
		})
		t.Run("custom resource create and update MUST NOT allow data if there is blocking error of MaxProperties", func(t *testing.T) {
			name2 := names.SimpleNameGenerator.GenerateName("cr-2")
			// a spec create that has maxItem err MUST fail validation
			cr := &unstructured.Unstructured{Object: map[string]interface{}{
				"apiVersion": gvr.Group + "/" + gvr.Version,
				"kind":       crd.Spec.Names.Kind,
				"metadata": map[string]interface{}{
					"name": name2,
				},
				"spec": map[string]interface{}{
					"x":        int64(2),
					"y":        int64(2),
					"floatMap": map[string]interface{}{},
					"assocList": []interface{}{
						map[string]interface{}{
							"k": "a",
							"v": "1",
						},
					},
					"limit": nil,
				},
			}}
			floatMap := cr.Object["spec"].(map[string]interface{})["floatMap"].(map[string]interface{})
			for i := 1; i <= 101; i++ {
				floatMap[fmt.Sprintf("key%d", i)] = float64(i) / 10
			}

			_, err = crClient.Create(context.TODO(), cr, metav1.CreateOptions{})
			if err == nil || !strings.Contains(err.Error(), "some validation rules were not checked because the object was invalid; correct the existing errors to complete validation") {
				t.Fatalf("expect error to contain \"some validation rules were not checked because the object was invalid; correct the existing errors to complete validation\" but get: %v", err)
			}
		})
		t.Run("custom resource create and update MUST NOT allow data if there is blocking error of missing required field", func(t *testing.T) {
			name2 := names.SimpleNameGenerator.GenerateName("cr-2")
			// a spec create that has required err MUST fail validation
			cr := &unstructured.Unstructured{Object: map[string]interface{}{
				"apiVersion": gvr.Group + "/" + gvr.Version,
				"kind":       crd.Spec.Names.Kind,
				"metadata": map[string]interface{}{
					"name": name2,
				},
				"spec": map[string]interface{}{
					"x": int64(2),
					"y": int64(2),
					"floatMap": map[string]interface{}{
						"key1": 0.2,
						"key2": 0.3,
					},
					"assocList": []interface{}{
						map[string]interface{}{
							"k": "1",
						},
					},
					"limit": nil,
				},
			}}

			_, err = crClient.Create(context.TODO(), cr, metav1.CreateOptions{})
			if err == nil || !strings.Contains(err.Error(), "some validation rules were not checked because the object was invalid; correct the existing errors to complete validation") {
				t.Fatalf("expect error to contain \"some validation rules were not checked because the object was invalid; correct the existing errors to complete validation\" but get: %v", err)
			}
		})
		t.Run("custom resource create and update MUST NOT allow data if there is blocking error of type", func(t *testing.T) {
			name2 := names.SimpleNameGenerator.GenerateName("cr-2")
			// a spec create that has required err MUST fail validation
			cr := &unstructured.Unstructured{Object: map[string]interface{}{
				"apiVersion": gvr.Group + "/" + gvr.Version,
				"kind":       crd.Spec.Names.Kind,
				"metadata": map[string]interface{}{
					"name": name2,
				},
				"spec": map[string]interface{}{
					"x": int64(2),
					"y": int64(2),
					"floatMap": map[string]interface{}{
						"key1": 0.2,
						"key2": 0.3,
					},
					"assocList": []interface{}{
						map[string]interface{}{
							"k": "a",
							"v": true,
						},
					},
					"limit": nil,
				},
			}}

			_, err = crClient.Create(context.TODO(), cr, metav1.CreateOptions{})
			if err == nil || !strings.Contains(err.Error(), "some validation rules were not checked because the object was invalid; correct the existing errors to complete validation") {
				t.Fatalf("expect error to contain \"some validation rules were not checked because the object was invalid; correct the existing errors to complete validation\" but get: %v", err)
			}
		})
	})
}

// TestCustomResourceValidatorsWithSchemaConversion tests CRD replacement with schema conversion issue should not panic.
func TestCustomResourceValidatorsWithSchemaConversion(t *testing.T) {
	server, err := apiservertesting.StartTestServer(t, apiservertesting.NewDefaultTestServerOptions(), nil, framework.SharedEtcd())
	if err != nil {
		t.Fatal(err)
	}
	defer server.TearDownFn()
	config := server.ClientConfig

	apiExtensionClient, err := clientset.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}

	// Create CRD with normal items+array schema
	structuralWithValidators := crdWithSchema(t, "Structural", structuralSchemaWithItemsUnderArray)
	crd, err := fixtures.CreateNewV1CustomResourceDefinition(structuralWithValidators, apiExtensionClient, dynamicClient)
	if err != nil {
		t.Fatal(err)
	}
	gvr := schema.GroupVersionResource{
		Group:    crd.Spec.Group,
		Version:  crd.Spec.Versions[0].Name,
		Resource: crd.Spec.Names.Plural,
	}
	crClient := dynamicClient.Resource(gvr)

	// Create a valid CR instance
	name1 := names.SimpleNameGenerator.GenerateName("cr-1")
	_, err = crClient.Create(context.TODO(), &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": gvr.Group + "/" + gvr.Version,
		"kind":       crd.Spec.Names.Kind,
		"metadata": map[string]interface{}{
			"name": name1,
		},
		"spec": map[string]interface{}{
			"backend": []interface{}{
				map[string]interface{}{
					"replicas": 8,
				},
			},
		},
	}}, metav1.CreateOptions{})
	if err != nil {
		t.Errorf("Failed to create custom resource: %v", err)
	}
	crd, err = apiExtensionClient.ApiextensionsV1().CustomResourceDefinitions().Get(context.TODO(), crd.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	structuralSchemaWithItemsUnderObject := crdWithSchema(t, "Structural", structuralSchemaWithItemsUnderObject)
	structuralSchemaWithItemsUnderObject.SetResourceVersion(crd.GetResourceVersion())
	// Update CRD with invalid schema items under object
	crd, err = apiExtensionClient.ApiextensionsV1().CustomResourceDefinitions().Update(context.TODO(), structuralSchemaWithItemsUnderObject, metav1.UpdateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// Make an unrelated update to the previous persisted CR instance to make sure CRD handler doesn't panic
	oldCR, err := crClient.Get(context.TODO(), name1, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	oldCR.Object["metadata"].(map[string]interface{})["labels"] = map[string]interface{}{"key": "value"}
	_, err = crClient.Update(context.TODO(), oldCR, metav1.UpdateOptions{})
	if err == nil || !strings.Contains(err.Error(), "rule compiler initialization error: failed to convert to declType for CEL validation rules") {
		t.Fatalf("expect error to contain \rule compiler initialization error: failed to convert to declType for CEL validation rules\" but get: %v", err)
	}
	// Create another CR instance with an array and be rejected
	name2 := names.SimpleNameGenerator.GenerateName("cr-2")
	_, err = crClient.Create(context.TODO(), &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": gvr.Group + "/" + gvr.Version,
		"kind":       crd.Spec.Names.Kind,
		"metadata": map[string]interface{}{
			"name": name2,
		},
		"spec": map[string]interface{}{
			"backend": []interface{}{
				map[string]interface{}{
					"replicas": 7,
				},
			},
		},
	}}, metav1.CreateOptions{})
	if err == nil || !strings.Contains(err.Error(), "Invalid value: \"array\": spec.backend in body must be of type object: \"array\"") {
		t.Fatalf("expect error to contain \"Invalid value: \"array\": spec.backend in body must be of type object: \"array\"\" but get: %v", err)
	}
	// Delete the CRD
	err = fixtures.DeleteV1CustomResourceDefinition(structuralWithValidators, apiExtensionClient)
	if err != nil {
		t.Fatal(err)
	}
}

func nonStructuralCrdWithValidations() *apiextensionsv1beta1.CustomResourceDefinition {
	return &apiextensionsv1beta1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name: "foos.nonstructural.cr.bar.com",
		},
		Spec: apiextensionsv1beta1.CustomResourceDefinitionSpec{
			Group:   "nonstructural.cr.bar.com",
			Version: "v1",
			Scope:   apiextensionsv1beta1.NamespaceScoped,
			Names: apiextensionsv1beta1.CustomResourceDefinitionNames{
				Plural: "foos",
				Kind:   "Foo",
			},
			Validation: &apiextensionsv1beta1.CustomResourceValidation{
				OpenAPIV3Schema: &apiextensionsv1beta1.JSONSchemaProps{
					Type: "object",
					Properties: map[string]apiextensionsv1beta1.JSONSchemaProps{
						"foo": {},
					},
				},
			},
		},
	}
}

func genLargeArray(n, x int64) []int64 {
	arr := make([]int64, n)
	for i := int64(0); i < n; i++ {
		arr[i] = x
	}
	return arr
}

func crdWithSchema(t *testing.T, kind string, schemaJson []byte) *apiextensionsv1.CustomResourceDefinition {
	plural := strings.ToLower(kind) + "s"
	var c apiextensionsv1.CustomResourceValidation
	err := json.Unmarshal(schemaJson, &c)
	if err != nil {
		t.Fatal(err)
	}

	return &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("%s.mygroup.example.com", plural)},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: "mygroup.example.com",
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{
				Name:    "v1beta1",
				Served:  true,
				Storage: true,
				Schema:  &c,
				Subresources: &apiextensionsv1.CustomResourceSubresources{
					Status: &apiextensionsv1.CustomResourceSubresourceStatus{},
				},
			}},
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Plural: plural,
				Kind:   kind,
			},
			Scope: apiextensionsv1.ClusterScoped,
		},
	}
}

var structuralSchemaWithValidators = []byte(`
{
  "openAPIV3Schema": {
    "description": "CRD with CEL validators",
    "type": "object",
	"x-kubernetes-validations": [
	  {
		"rule": "self.spec.x + self.spec.y >= (has(self.status) ? self.status.z : 0)"
	  }
	],
    "properties": {
      "spec": {
        "type": "object",
        "properties": {
          "x": {
            "type": "integer",
			"default": 0
          },
          "y": {
            "type": "integer",
			"default": 0
          },
          "extra": {
			"type": "string",
			"x-kubernetes-validations": [
			  {
				"rule": "self.startsWith('anything')"
			  }
			]
          },
		  "floatMap": {
			"type": "object",
			"additionalProperties": { "type": "number" },
			"x-kubernetes-validations": [
			  {
				"rule": "self.all(k, self[k] >= 0.2)"
			  }
			]
          },
		  "assocList": {
			"type": "array",
			"maxItems": 100,
			"items": {
			  "type": "object",
			  "properties": {
			    "k": { "type": "string", "maxLength": 200},
			    "v": { "type": "string", "maxLength": 200}
			  },
			  "required": ["k"]
			},
			"x-kubernetes-list-type": "map",
			"x-kubernetes-list-map-keys": ["k"],
			"x-kubernetes-validations": [
			  {
				"rule": "self.exists(e, e.k == 'a' && e.v == '1')"
			  }
			]
          },
          "limit": {
			"nullable": true,
			"x-kubernetes-validations": [
			  {
				"rule": "type(self) == int && self == 123"
			  }
			],
			"x-kubernetes-int-or-string": true
          }
        }
      },
      "status": {
        "type": "object",
		"properties": {
          "z": {
            "type": "integer",
			"default": 0
          }
        }
      }
    }
  }
}`)

var structuralSchemaWithBlockingErr = []byte(`
{
  "openAPIV3Schema": {
    "description": "CRD with CEL validators",
    "type": "object",
	"x-kubernetes-validations": [
	  {
		"rule": "self.spec.x + self.spec.y >= (has(self.status) ? self.status.z : 0)",
		"messageExpression": "\"self.spec.x + self.spec.y must be greater than or equal to 0\""
	  }
	],
    "properties": {
      "spec": {
        "type": "object",
        "properties": {
          "x": {
            "type": "integer",
			"default": 0
          },
          "y": {
            "type": "integer",
			"default": 0
          },
          "extra": {
			"type": "string",
            "maxLength": 200,
			"x-kubernetes-validations": [
			  {
				"rule": "self.startsWith('anything')"
			  }
			]
          },
		  "floatMap": {
			"type": "object",
            "maxProperties": 100,
			"additionalProperties": { "type": "number" },
			"x-kubernetes-validations": [
			  {
				"rule": "self.all(k, self[k] >= 0.2)"
			  }
			]
          },
		  "assocList": {
			"type": "array",
            "maxItems": 100,
			"items": {
			  "type": "object",
			  "properties": {
			    "k": { "type": "string" },
			    "v": { "type": "string" }
			  },
			  "required": ["k", "v"]
			},
			"x-kubernetes-list-type": "map",
			"x-kubernetes-list-map-keys": ["k"],
			"x-kubernetes-validations": [
			  {
				"rule": "self.exists(e, e.k == 'a' && e.v == '1')"
			  }
			]
          },
          "limit": {
			"nullable": true,
			"x-kubernetes-validations": [
			  {
				"rule": "type(self) == int && self == 123"
			  }
			],
			"x-kubernetes-int-or-string": true
          }
        }
      },
      "status": {
        "type": "object",
		"properties": {
          "z": {
            "type": "integer",
			"default": 0
          }
        }
      }
    }
  }
}`)

var structuralSchemaWithItemsUnderArray = []byte(`
{
  "openAPIV3Schema": {
    "description": "CRD with CEL validators",
    "type": "object",
    "properties": {
      "spec": {
        "type": "object",
        "properties": {
          "backend": {
            "type": "array",
            "maxItems": 100,
            "items": {
              "type": "object",
              "properties": {
                "replicas": {
                  "type": "integer"
                }
              },
              "required": [
                "replicas"
              ],
              "x-kubernetes-validations": [
                {
                  "rule": "0 <= self.replicas && self.replicas <= 10"
                }
              ]
            }
          }
        }
      }
    }
  }
}`)

var structuralSchemaWithItemsUnderObject = []byte(`
{
  "openAPIV3Schema": {
    "description": "CRD with CEL validators",
    "type": "object",
    "properties": {
      "spec": {
        "type": "object",
        "properties": {
          "backend": {
            "type": "object",
            "maxItems": 100,
            "items": {
              "type": "object",
              "properties": {
                "replicas": {
                  "type": "integer"
                }
              },
              "required": [
                "replicas"
              ],
              "x-kubernetes-validations": [
                {
                  "rule": "0 <= self.replicas && self.replicas <= 10"
                }
              ]
            }
          }
        }
      }
    }
  }
}`)

var structuralSchemaWithValidMetadataValidators = []byte(`
{
  "openAPIV3Schema": {
    "description": "CRD with CEL validators",
    "type": "object",
	"x-kubernetes-validations": [
	  {
		"rule": "self.metadata.name.size() > 3"
	  }
	],
    "properties": {
	  "metadata": {
        "type": "object",
        "properties": {
		  "name": { "type": "string" }
	    }
      },
      "spec": {
        "type": "object",
        "properties": {}
      },
      "status": {
        "type": "object",
        "properties": {}
	  }
    }
  }
}`)

var structuralSchemaWithInvalidMetadataValidators = []byte(`
{
  "openAPIV3Schema": {
    "description": "CRD with CEL validators",
    "type": "object",
	"x-kubernetes-validations": [
	  {
		"rule": "self.metadata.labels.size() > 0"
	  }
	],
    "properties": {
	  "metadata": {
        "type": "object",
        "properties": {
		  "name": { "type": "string" }
	    }
      },
      "spec": {
        "type": "object",
        "properties": {}
      },
      "status": {
        "type": "object",
        "properties": {}
	  }
    }
  }
}`)

var structuralSchemaWithValidTransitionRule = []byte(`
{
  "openAPIV3Schema": {
    "description": "CRD with CEL validators",
    "type": "object",
    "properties": {
      "spec": {
        "type": "object",
        "properties": {
		  "someImmutableThing": { "type": "string" },
          "somethingElse": { "type": "string" }
	    },
		"x-kubernetes-validations": [
		  {
			"rule": "self.someImmutableThing == oldSelf.someImmutableThing"
		  }
		]
      },
      "status": {
        "type": "object",
        "properties": {}
	  }
    }
  }
}`)

var structuralSchemaWithInvalidTransitionRule = []byte(`
{
  "openAPIV3Schema": {
    "description": "CRD with CEL validators",
    "type": "object",
    "properties": {
      "spec": {
        "type": "object",
        "properties": {
		  "list": {
            "type": "array",
            "items": {
              "type": "string",
		      "x-kubernetes-validations": [
		        {
			      "rule": "self == oldSelf"
                }
		      ]
            }
          }
	    }
      },
      "status": {
        "type": "object",
        "properties": {}
	  }
    }
  }
}`)

var structuralSchemaWithDefaultMapKeyTransitionRule = []byte(`
{
  "openAPIV3Schema": {
    "description": "CRD with CEL validators",
    "type": "object",
    "properties": {
      "spec": {
        "type": "object",
        "properties": {
		  "list": {
            "type": "array",
            "x-kubernetes-list-map-keys": [
              "k1",
              "k2"
            ],
            "x-kubernetes-list-type": "map",
            "maxItems": 1000,
            "items": {
              "type": "object",
              "properties": {
                "k1": { "type": "string" },
                "k2": { "type": "string", "default": "DEFAULT" },
                "v": { "type": "string", "maxLength": 200 }
              },
			  "required": ["k1"],
		      "x-kubernetes-validations": [
		        {
			      "rule": "self.v == oldSelf.v"
                }
		      ]
            }
          }
	    }
      },
      "status": {
        "type": "object",
        "properties": {}
	  }
    }
  }
}`)

var structuralSchemaWithCostLimit = []byte(`
{
  "openAPIV3Schema": {
    "description": "CRD with CEL validators",
    "type": "object",
    "properties": {
      "spec": {
        "type": "object",
        "properties": {
		  "list": {
			"type": "array",
			"maxItems": 725,
            "items": {
			  "type": "integer"
			},
			"x-kubernetes-validations": [
		        {
			      "rule": "self.all(x, self.all(y, x == y))"
                }
		      ]
          }
	    }
      },
      "status": {
        "type": "object",
        "properties": {}
	  }
    }
  }
}`)

var structuralSchemaWithEmptyObject = []byte(`
{
  "openAPIV3Schema": {
    "description": "weird CRD with empty spec, unstructured status. designed to fit test fixtures.",
    "type": "object",
    "x-kubernetes-validations": [
      {
        "rule": "[has(self.spec), has(self.status)].exists_one(x, x)"
      }
    ],
    "properties": {
      "spec": {
        "type": "object"
      },
      "status": {
        "type": "object",
        "additionalProperties": true
      }
    }
  }
}
`)
