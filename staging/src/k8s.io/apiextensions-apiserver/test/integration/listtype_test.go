/*
Copyright 2019 The Kubernetes Authors.

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

package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apiextensions-apiserver/test/integration/fixtures"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/yaml"
)

var listTypeResourceFixture = &apiextensionsv1.CustomResourceDefinition{
	ObjectMeta: metav1.ObjectMeta{Name: "foos.tests.example.com"},
	Spec: apiextensionsv1.CustomResourceDefinitionSpec{
		Group: "tests.example.com",
		Versions: []apiextensionsv1.CustomResourceDefinitionVersion{
			{
				Name:    "v1beta1",
				Storage: true,
				Served:  true,
				Schema:  fixtures.AllowAllSchema(),
			},
		},
		Names: apiextensionsv1.CustomResourceDefinitionNames{
			Plural:   "foos",
			Singular: "foo",
			Kind:     "Foo",
			ListKind: "FooList",
		},
		Scope: apiextensionsv1.ClusterScoped,
	},
}

const (
	// structural schema because x-kubernetes-list-type is only allowed for those
	listTypeResourceSchema = `
type: object
properties:
  correct-map:
    type: array
    x-kubernetes-list-type: map
    x-kubernetes-list-map-keys: ["a", "b"]
    items:
      type: object
      required: ["a", "b"]
      properties:
        a:
          type: integer
        b:
          type: integer
  correct-set:
    type: array
    x-kubernetes-list-type: set
    items:
      type: object
      x-kubernetes-map-type: atomic
      additionalProperties:
        type: integer
  invalid-map:
    type: array
    x-kubernetes-list-type: map
    x-kubernetes-list-map-keys: ["a", "b"]
    items:
      type: object
      required: ["a", "b"]
      properties:
        a:
          type: integer
        b:
          type: integer
  invalid-set:
    type: array
    x-kubernetes-list-type: set
    items:
      type: object
      x-kubernetes-map-type: atomic
      additionalProperties:
        type: integer
`

	listTypeResourceInstance = `
kind: Foo
apiVersion: tests.example.com/v1beta1
metadata:
  name: foo
correct-map: [{"a":1,"b":1,c:"1"},{"a":1,"b":2,c:"2"},{"a":1,"b":3,c:"3"}]
correct-set: [{"a":1,"b":1},{"a":1,"b":2},{"a":1},{"a":1,"b":4}]
invalid-map: [{"a":1,"b":1,c:"1"},{"a":1,"b":2,c:"2"},{"a":1,"b":3,c:"3"},{"a":1,"b":1,c:"4"}]
invalid-set: [{"a":1,"b":1},{"a":1,"b":2},{"a":1},{"a":1,"b":4},{"a":1,"b":1}]
`
)

var (
	validListTypeFields   = []string{"correct-map", "correct-set"}
	invalidListTypeFields = []string{"invalid-map", "invalid-set"}
)

func TestListTypes(t *testing.T) {
	tearDownFn, apiExtensionClient, dynamicClient, err := fixtures.StartDefaultServerWithClients(t)
	if err != nil {
		t.Fatal(err)
	}
	defer tearDownFn()

	crd := listTypeResourceFixture.DeepCopy()
	if err := yaml.Unmarshal([]byte(listTypeResourceSchema), &crd.Spec.Versions[0].Schema.OpenAPIV3Schema); err != nil {
		t.Fatal(err)
	}

	crd, err = fixtures.CreateNewV1CustomResourceDefinition(crd, apiExtensionClient, dynamicClient)
	if err != nil {
		t.Fatal(err)
	}

	t.Logf("Creating CR and expect list-type errors")
	fooClient := dynamicClient.Resource(schema.GroupVersionResource{Group: crd.Spec.Group, Version: crd.Spec.Versions[0].Name, Resource: crd.Spec.Names.Plural})
	invalidInstance := &unstructured.Unstructured{}
	if err := yaml.Unmarshal([]byte(listTypeResourceInstance), &invalidInstance.Object); err != nil {
		t.Fatal(err)
	}
	_, createErr := fooClient.Create(context.TODO(), invalidInstance, metav1.CreateOptions{})
	if createErr == nil {
		t.Fatalf("Expected validation errors, but did not get one")
	}

	t.Logf("Checking that valid fields DO NOT show in error")
	for _, valid := range validListTypeFields {
		if strings.Contains(createErr.Error(), valid) {
			t.Errorf("unexpected error about %q: %v", valid, err)
		}
	}

	t.Logf("Checking that invalid fields DO show in error")
	for _, invalid := range invalidListTypeFields {
		if !strings.Contains(createErr.Error(), invalid) {
			t.Errorf("expected %q to show up in the error, but didn't: %v", invalid, err)
		}
	}

	t.Logf("Creating fixed CR")
	validInstance := &unstructured.Unstructured{}
	if err := yaml.Unmarshal([]byte(listTypeResourceInstance), &validInstance.Object); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range invalidListTypeFields {
		unstructured.RemoveNestedField(validInstance.Object, invalid)
	}
	validInstance, err = fooClient.Create(context.TODO(), validInstance, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	t.Logf("Updating with invalid values and expecting errors")
	modifiedInstance := validInstance.DeepCopy()
	for _, valid := range validListTypeFields {
		x := modifiedInstance.Object[valid]
		l := x.([]interface{})
		l = append(l, l[0])
		modifiedInstance.Object[valid] = l
	}
	_, err = fooClient.Update(context.TODO(), modifiedInstance, metav1.UpdateOptions{})
	if err == nil {
		t.Fatalf("Expected validation errors, but did not get one")
	}
	for _, valid := range validListTypeFields {
		if !strings.Contains(err.Error(), valid) {
			t.Errorf("expected %q to show up in the error, but didn't: %v", valid, err)
		}
	}

	t.Logf("Remove \"b\" from the keys in the schema which renders the valid instance invalid")
	err = retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		crd, err := apiExtensionClient.ApiextensionsV1().CustomResourceDefinitions().Get(context.TODO(), crd.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		s := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["correct-map"]
		s.XListMapKeys = []string{"a"}
		crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["correct-map"] = s
		_, err = apiExtensionClient.ApiextensionsV1().CustomResourceDefinitions().Update(context.TODO(), crd, metav1.UpdateOptions{})
		return err
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	t.Logf("Updating again with invalid values, eventually successfully due to ratcheting logic")
	err = wait.PollUntilContextTimeout(context.Background(), time.Millisecond*100, wait.ForeverTestTimeout, true, func(ctx context.Context) (bool, error) {
		var err error
		_, err = fooClient.Update(ctx, modifiedInstance, metav1.UpdateOptions{})
		if err == nil {
			return true, nil
		}
		if errors.IsInvalid(err) {
			// wait until modifiedInstance becomes valid again
			return false, nil
		}
		return false, err
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
