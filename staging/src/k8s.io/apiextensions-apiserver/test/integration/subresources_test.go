/*
Copyright 2018 The Kubernetes Authors.

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
	"fmt"
	"math"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	autoscaling "k8s.io/api/autoscaling/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	"k8s.io/apiextensions-apiserver/test/integration/fixtures"
)

var labelSelectorPath = ".status.labelSelector"
var anotherLabelSelectorPath = ".status.anotherLabelSelector"

func NewNoxuSubresourcesCRDs(scope apiextensionsv1.ResourceScope) []*apiextensionsv1.CustomResourceDefinition {
	return []*apiextensionsv1.CustomResourceDefinition{
		// CRD that uses per-version subresources
		{
			ObjectMeta: metav1.ObjectMeta{Name: "noxus.mygroup.example.com"},
			Spec: apiextensionsv1.CustomResourceDefinitionSpec{
				Group: "mygroup.example.com",
				Names: apiextensionsv1.CustomResourceDefinitionNames{
					Plural:     "noxus",
					Singular:   "nonenglishnoxu",
					Kind:       "WishIHadChosenNoxu",
					ShortNames: []string{"foo", "bar", "abc", "def"},
					ListKind:   "NoxuItemList",
				},
				Scope: scope,
				Versions: []apiextensionsv1.CustomResourceDefinitionVersion{
					{
						Name:    "v1beta1",
						Served:  true,
						Storage: true,
						Subresources: &apiextensionsv1.CustomResourceSubresources{
							Status: &apiextensionsv1.CustomResourceSubresourceStatus{},
							Scale: &apiextensionsv1.CustomResourceSubresourceScale{
								SpecReplicasPath:   ".spec.replicas",
								StatusReplicasPath: ".status.replicas",
								LabelSelectorPath:  &labelSelectorPath,
							},
						},
						Schema: fixtures.AllowAllSchema(),
					},
					{
						Name:    "v1",
						Served:  true,
						Storage: false,
						Subresources: &apiextensionsv1.CustomResourceSubresources{
							Status: &apiextensionsv1.CustomResourceSubresourceStatus{},
							Scale: &apiextensionsv1.CustomResourceSubresourceScale{
								SpecReplicasPath:   ".spec.replicas",
								StatusReplicasPath: ".status.replicas",
								LabelSelectorPath:  &anotherLabelSelectorPath,
							},
						},
						Schema: fixtures.AllowAllSchema(),
					},
				},
			},
		},
	}
}

func NewNoxuSubresourceInstance(namespace, name, version string) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": fmt.Sprintf("mygroup.example.com/%s", version),
			"kind":       "WishIHadChosenNoxu",
			"metadata": map[string]interface{}{
				"namespace": namespace,
				"name":      name,
			},
			"spec": map[string]interface{}{
				"num":      int64(10),
				"replicas": int64(3),
			},
			"status": map[string]interface{}{
				"replicas": int64(7),
			},
		},
	}
}

func NewNoxuSubresourceInstanceWithReplicas(namespace, name, version, replicasField string) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": fmt.Sprintf("mygroup.example.com/%s", version),
			"kind":       "WishIHadChosenNoxu",
			"metadata": map[string]interface{}{
				"namespace": namespace,
				"name":      name,
			},
			"spec": map[string]interface{}{
				"num":         int64(10),
				replicasField: int64(3),
			},
			"status": map[string]interface{}{
				"replicas": int64(7),
			},
		},
	}
}

func TestStatusSubresource(t *testing.T) {
	tearDown, apiExtensionClient, dynamicClient, err := fixtures.StartDefaultServerWithClients(t)
	if err != nil {
		t.Fatal(err)
	}
	defer tearDown()

	noxuDefinitions := NewNoxuSubresourcesCRDs(apiextensionsv1.NamespaceScoped)
	for _, noxuDefinition := range noxuDefinitions {
		noxuDefinition, err = fixtures.CreateNewV1CustomResourceDefinition(noxuDefinition, apiExtensionClient, dynamicClient)
		if err != nil {
			t.Fatal(err)
		}

		ns := "not-the-default"
		for _, v := range noxuDefinition.Spec.Versions {
			noxuResourceClient := newNamespacedCustomResourceVersionedClient(ns, dynamicClient, noxuDefinition, v.Name)
			_, err = instantiateVersionedCustomResource(t, NewNoxuSubresourceInstance(ns, "foo", v.Name), noxuResourceClient, noxuDefinition, v.Name)
			if err != nil {
				t.Fatalf("unable to create noxu instance: %v", err)
			}
			gottenNoxuInstance, err := noxuResourceClient.Get(context.TODO(), "foo", metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			// status should not be set after creation
			if val, ok := gottenNoxuInstance.Object["status"]; ok {
				t.Fatalf("status should not be set after creation, got %v", val)
			}

			// .status.num = 20
			err = unstructured.SetNestedField(gottenNoxuInstance.Object, int64(20), "status", "num")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			// .spec.num = 20
			err = unstructured.SetNestedField(gottenNoxuInstance.Object, int64(20), "spec", "num")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			// UpdateStatus should not update spec.
			// Check that .spec.num = 10 and .status.num = 20
			updatedStatusInstance, err := noxuResourceClient.UpdateStatus(context.TODO(), gottenNoxuInstance, metav1.UpdateOptions{})
			if err != nil {
				t.Fatalf("unable to update status: %v", err)
			}

			specNum, found, err := unstructured.NestedInt64(updatedStatusInstance.Object, "spec", "num")
			if !found || err != nil {
				t.Fatalf("unable to get .spec.num")
			}
			if specNum != int64(10) {
				t.Fatalf(".spec.num: expected: %v, got: %v", int64(10), specNum)
			}

			statusNum, found, err := unstructured.NestedInt64(updatedStatusInstance.Object, "status", "num")
			if !found || err != nil {
				t.Fatalf("unable to get .status.num")
			}
			if statusNum != int64(20) {
				t.Fatalf(".status.num: expected: %v, got: %v", int64(20), statusNum)
			}

			gottenNoxuInstance, err = noxuResourceClient.Get(context.TODO(), "foo", metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}

			// .status.num = 40
			err = unstructured.SetNestedField(gottenNoxuInstance.Object, int64(40), "status", "num")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			// .spec.num = 40
			err = unstructured.SetNestedField(gottenNoxuInstance.Object, int64(40), "spec", "num")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			// Update should not update status.
			// Check that .spec.num = 40 and .status.num = 20
			updatedInstance, err := noxuResourceClient.Update(context.TODO(), gottenNoxuInstance, metav1.UpdateOptions{})
			if err != nil {
				t.Fatalf("unable to update instance: %v", err)
			}

			specNum, found, err = unstructured.NestedInt64(updatedInstance.Object, "spec", "num")
			if !found || err != nil {
				t.Fatalf("unable to get .spec.num")
			}
			if specNum != int64(40) {
				t.Fatalf(".spec.num: expected: %v, got: %v", int64(40), specNum)
			}

			statusNum, found, err = unstructured.NestedInt64(updatedInstance.Object, "status", "num")
			if !found || err != nil {
				t.Fatalf("unable to get .status.num")
			}
			if statusNum != int64(20) {
				t.Fatalf(".status.num: expected: %v, got: %v", int64(20), statusNum)
			}
			noxuResourceClient.Delete(context.TODO(), "foo", metav1.DeleteOptions{})
		}
		if err := fixtures.DeleteV1CustomResourceDefinition(noxuDefinition, apiExtensionClient); err != nil {
			t.Fatal(err)
		}
	}
}

func TestScaleSubresource(t *testing.T) {
	groupResource := schema.GroupResource{
		Group:    "mygroup.example.com",
		Resource: "noxus",
	}

	tearDown, config, _, err := fixtures.StartDefaultServer(t)
	if err != nil {
		t.Fatal(err)
	}
	defer tearDown()

	apiExtensionClient, err := clientset.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}

	noxuDefinitions := NewNoxuSubresourcesCRDs(apiextensionsv1.NamespaceScoped)
	for _, noxuDefinition := range noxuDefinitions {
		for _, v := range noxuDefinition.Spec.Versions {
			// Start with a new CRD, so that the object doesn't have resourceVersion
			noxuDefinition := noxuDefinition.DeepCopy()

			subresources, err := getSubresourcesForVersion(noxuDefinition, v.Name)
			if err != nil {
				t.Fatal(err)
			}
			// set invalid json path for specReplicasPath
			subresources.Scale.SpecReplicasPath = "foo,bar"
			_, err = fixtures.CreateNewV1CustomResourceDefinition(noxuDefinition, apiExtensionClient, dynamicClient)
			if err == nil {
				t.Fatalf("unexpected non-error: specReplicasPath should be a valid json path under .spec")
			}

			subresources.Scale.SpecReplicasPath = ".spec.replicas"
			noxuDefinition, err = fixtures.CreateNewV1CustomResourceDefinition(noxuDefinition, apiExtensionClient, dynamicClient)
			if err != nil {
				t.Fatal(err)
			}

			ns := "not-the-default"
			noxuResourceClient := newNamespacedCustomResourceVersionedClient(ns, dynamicClient, noxuDefinition, v.Name)
			_, err = instantiateVersionedCustomResource(t, NewNoxuSubresourceInstance(ns, "foo", v.Name), noxuResourceClient, noxuDefinition, v.Name)
			if err != nil {
				t.Fatalf("unable to create noxu instance: %v", err)
			}

			scaleClient, err := fixtures.CreateNewVersionedScaleClient(noxuDefinition, config, v.Name)
			if err != nil {
				t.Fatal(err)
			}

			// set .status.labelSelector = bar
			gottenNoxuInstance, err := noxuResourceClient.Get(context.TODO(), "foo", metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			err = unstructured.SetNestedField(gottenNoxuInstance.Object, "bar", strings.Split((*subresources.Scale.LabelSelectorPath)[1:], ".")...)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			_, err = noxuResourceClient.UpdateStatus(context.TODO(), gottenNoxuInstance, metav1.UpdateOptions{})
			if err != nil {
				t.Fatalf("unable to update status: %v", err)
			}

			// get the scale object
			gottenScale, err := scaleClient.Scales("not-the-default").Get(context.TODO(), groupResource, "foo", metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if gottenScale.Spec.Replicas != 3 {
				t.Fatalf("Scale.Spec.Replicas: expected: %v, got: %v", 3, gottenScale.Spec.Replicas)
			}
			if gottenScale.Status.Selector != "bar" {
				t.Fatalf("Scale.Status.Selector: expected: %v, got: %v", "bar", gottenScale.Status.Selector)
			}

			// update the scale object
			// check that spec is updated, but status is not
			gottenScale.Spec.Replicas = 5
			gottenScale.Status.Selector = "baz"
			updatedScale, err := scaleClient.Scales("not-the-default").Update(context.TODO(), groupResource, gottenScale, metav1.UpdateOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if updatedScale.Spec.Replicas != 5 {
				t.Fatalf("replicas: expected: %v, got: %v", 5, updatedScale.Spec.Replicas)
			}
			if updatedScale.Status.Selector != "bar" {
				t.Fatalf("scale should not update status: expected %v, got: %v", "bar", updatedScale.Status.Selector)
			}

			// check that .spec.replicas = 5, but status is not updated
			updatedNoxuInstance, err := noxuResourceClient.Get(context.TODO(), "foo", metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			specReplicas, found, err := unstructured.NestedInt64(updatedNoxuInstance.Object, "spec", "replicas")
			if !found || err != nil {
				t.Fatalf("unable to get .spec.replicas")
			}
			if specReplicas != 5 {
				t.Fatalf("replicas: expected: %v, got: %v", 5, specReplicas)
			}
			statusLabelSelector, found, err := unstructured.NestedString(updatedNoxuInstance.Object, strings.Split((*subresources.Scale.LabelSelectorPath)[1:], ".")...)
			if !found || err != nil {
				t.Fatalf("unable to get %s", *subresources.Scale.LabelSelectorPath)
			}
			if statusLabelSelector != "bar" {
				t.Fatalf("scale should not update status: expected %v, got: %v", "bar", statusLabelSelector)
			}

			// validate maximum value
			// set .spec.replicas = math.MaxInt64
			gottenNoxuInstance, err = noxuResourceClient.Get(context.TODO(), "foo", metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			err = unstructured.SetNestedField(gottenNoxuInstance.Object, int64(math.MaxInt64), "spec", "replicas")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			_, err = noxuResourceClient.Update(context.TODO(), gottenNoxuInstance, metav1.UpdateOptions{})
			if err == nil {
				t.Fatalf("unexpected non-error: .spec.replicas should be less than 2147483647")
			}
			noxuResourceClient.Delete(context.TODO(), "foo", metav1.DeleteOptions{})
			if err := fixtures.DeleteV1CustomResourceDefinition(noxuDefinition, apiExtensionClient); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestApplyScaleSubresource(t *testing.T) {
	tearDown, config, _, err := fixtures.StartDefaultServer(t)
	if err != nil {
		t.Fatal(err)
	}
	defer tearDown()

	apiExtensionClient, err := clientset.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}

	noxuDefinition := NewNoxuSubresourcesCRDs(apiextensionsv1.NamespaceScoped)[0]
	subresources, err := getSubresourcesForVersion(noxuDefinition, "v1beta1")
	if err != nil {
		t.Fatal(err)
	}
	subresources.Scale.SpecReplicasPath = ".spec.replicas[0]"
	noxuDefinition, err = fixtures.CreateNewV1CustomResourceDefinition(noxuDefinition, apiExtensionClient, dynamicClient)
	if err != nil {
		t.Fatal(err)
	}

	// Create a client for it.
	ns := "not-the-default"
	noxuResourceClient := newNamespacedCustomResourceVersionedClient(ns, dynamicClient, noxuDefinition, "v1beta1")

	obj := NewNoxuSubresourceInstanceWithReplicas(ns, "foo", "v1beta1", "replicas[0]")
	obj, err = noxuResourceClient.Create(context.TODO(), obj, metav1.CreateOptions{})
	if err != nil {
		t.Logf("%#v", obj)
		t.Fatalf("Failed to create CustomResource: %v", err)
	}

	noxuResourceClient = newNamespacedCustomResourceVersionedClient(ns, dynamicClient, noxuDefinition, "v1")
	patch := `{"metadata": {"name": "foo"}, "kind": "WishIHadChosenNoxu", "apiVersion": "mygroup.example.com/v1", "spec": {"replicas": 3}}`
	obj, err = noxuResourceClient.Patch(context.TODO(), "foo", types.ApplyPatchType, []byte(patch), metav1.PatchOptions{FieldManager: "applier"})
	if err != nil {
		t.Logf("%#v", obj)
		t.Fatalf("Failed to Apply CustomResource: %v", err)
	}

	if got := len(obj.GetManagedFields()); got != 2 {
		t.Fatalf("Expected 2 managed fields, got %v: %v", got, obj.GetManagedFields())
	}

	_, err = noxuResourceClient.Patch(context.TODO(), "foo", types.MergePatchType, []byte(`{"spec": {"replicas": 5}}`), metav1.PatchOptions{FieldManager: "scaler"}, "scale")
	if err != nil {
		t.Fatal(err)
	}

	obj, err = noxuResourceClient.Get(context.TODO(), "foo", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Failed to Get CustomResource: %v", err)
	}

	// Managed fields should have 3 entries: one for scale, one for spec, and one for the rest of the fields
	managedFields := obj.GetManagedFields()
	if len(managedFields) != 3 {
		t.Fatalf("Expected 3 managed fields, got %v: %v", len(managedFields), obj.GetManagedFields())
	}
	specEntry := managedFields[0]
	if specEntry.Manager != "applier" || specEntry.APIVersion != "mygroup.example.com/v1" || specEntry.Operation != "Apply" || string(specEntry.FieldsV1.Raw) != `{"f:spec":{}}` || specEntry.Subresource != "" {
		t.Fatalf("Unexpected entry: %v", specEntry)
	}
	scaleEntry := managedFields[1]
	if scaleEntry.Manager != "scaler" || scaleEntry.APIVersion != "mygroup.example.com/v1" || scaleEntry.Operation != "Update" || string(scaleEntry.FieldsV1.Raw) != `{"f:spec":{"f:replicas":{}}}` || scaleEntry.Subresource != "scale" {
		t.Fatalf("Unexpected entry: %v", scaleEntry)
	}
	restEntry := managedFields[2]
	if restEntry.Manager != "integration.test" || restEntry.APIVersion != "mygroup.example.com/v1beta1" {
		t.Fatalf("Unexpected entry: %v", restEntry)
	}
}

func TestValidationSchemaWithStatus(t *testing.T) {
	tearDown, config, _, err := fixtures.StartDefaultServer(t)
	if err != nil {
		t.Fatal(err)
	}
	defer tearDown()

	apiExtensionClient, err := clientset.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}

	noxuDefinition := newNoxuValidationCRDs()[0]

	// make sure we are not restricting fields to properties even in subschemas
	noxuDefinition.Spec.Versions[0].Schema.OpenAPIV3Schema = &apiextensionsv1.JSONSchemaProps{
		Type: "object",
		Properties: map[string]apiextensionsv1.JSONSchemaProps{
			"spec": {
				Type:        "object",
				Description: "Validation for spec",
				Properties: map[string]apiextensionsv1.JSONSchemaProps{
					"replicas": {
						Type: "integer",
					},
				},
			},
		},
		Required:    []string{"spec"},
		Description: "This is a description at the root of the schema",
	}
	noxuDefinition.Spec.Versions[1].Schema.OpenAPIV3Schema = noxuDefinition.Spec.Versions[0].Schema.OpenAPIV3Schema

	_, err = fixtures.CreateNewV1CustomResourceDefinition(noxuDefinition, apiExtensionClient, dynamicClient)
	if err != nil {
		t.Fatalf("unable to created crd %v: %v", noxuDefinition.Name, err)
	}
}

func TestValidateOnlyStatus(t *testing.T) {
	tearDown, apiExtensionClient, dynamicClient, err := fixtures.StartDefaultServerWithClients(t)
	if err != nil {
		t.Fatal(err)
	}
	defer tearDown()

	// UpdateStatus should validate only status
	// 1. create a crd with max value of .spec.num = 10 and .status.num = 10
	// 2. create a cr with .spec.num = 10 and .status.num = 10 (valid)
	// 3. update the spec of the cr with .spec.num = 15 (spec is invalid), expect no error
	// 4. update the spec of the cr with .spec.num = 15 (spec is invalid), expect error

	// max value of spec.num = 10 and status.num = 10
	schema := &apiextensionsv1.JSONSchemaProps{
		Type: "object",
		Properties: map[string]apiextensionsv1.JSONSchemaProps{
			"spec": {
				Type: "object",
				Properties: map[string]apiextensionsv1.JSONSchemaProps{
					"num": {
						Type:    "integer",
						Maximum: float64Ptr(10),
					},
				},
			},
			"status": {
				Type: "object",
				Properties: map[string]apiextensionsv1.JSONSchemaProps{
					"num": {
						Type:    "integer",
						Maximum: float64Ptr(10),
					},
				},
			},
		},
	}

	noxuDefinitions := NewNoxuSubresourcesCRDs(apiextensionsv1.NamespaceScoped)
	for _, noxuDefinition := range noxuDefinitions {
		noxuDefinition.Spec.Versions[0].Schema = &apiextensionsv1.CustomResourceValidation{
			OpenAPIV3Schema: schema.DeepCopy(),
		}
		noxuDefinition.Spec.Versions[1].Schema = &apiextensionsv1.CustomResourceValidation{
			OpenAPIV3Schema: schema.DeepCopy(),
		}

		noxuDefinition, err = fixtures.CreateNewV1CustomResourceDefinition(noxuDefinition, apiExtensionClient, dynamicClient)
		if err != nil {
			t.Fatal(err)
		}
		ns := "not-the-default"
		for _, v := range noxuDefinition.Spec.Versions {
			noxuResourceClient := newNamespacedCustomResourceVersionedClient(ns, dynamicClient, noxuDefinition, v.Name)

			// set .spec.num = 10 and .status.num = 10
			noxuInstance := NewNoxuSubresourceInstance(ns, "foo", v.Name)
			err = unstructured.SetNestedField(noxuInstance.Object, int64(10), "status", "num")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			createdNoxuInstance, err := instantiateVersionedCustomResource(t, noxuInstance, noxuResourceClient, noxuDefinition, v.Name)
			if err != nil {
				t.Fatalf("unable to create noxu instance: %v", err)
			}

			// update the spec with .spec.num = 15, expecting no error
			err = unstructured.SetNestedField(createdNoxuInstance.Object, int64(15), "spec", "num")
			if err != nil {
				t.Fatalf("unexpected error setting .spec.num: %v", err)
			}
			createdNoxuInstance, err = noxuResourceClient.UpdateStatus(context.TODO(), createdNoxuInstance, metav1.UpdateOptions{})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			// update with .status.num = 15, expecting an error
			err = unstructured.SetNestedField(createdNoxuInstance.Object, int64(15), "status", "num")
			if err != nil {
				t.Fatalf("unexpected error setting .status.num: %v", err)
			}
			_, err = noxuResourceClient.UpdateStatus(context.TODO(), createdNoxuInstance, metav1.UpdateOptions{})
			if err == nil {
				t.Fatal("expected error, but got none")
			}
			statusError, isStatus := err.(*apierrors.StatusError)
			if !isStatus || statusError == nil {
				t.Fatalf("expected status error, got %T: %v", err, err)
			}
			expectedErr := "WishIHadChosenNoxu.mygroup.example.com \"foo\" is invalid: status.num: Invalid value: 15: num in body should be less than or equal to 10"
			if !strings.Contains(statusError.Error(), expectedErr) {
				t.Fatalf("expected %q in error, got: %v", expectedErr, err)
			}
			noxuResourceClient.Delete(context.TODO(), "foo", metav1.DeleteOptions{})
		}
		if err := fixtures.DeleteV1CustomResourceDefinition(noxuDefinition, apiExtensionClient); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSubresourcesDiscovery(t *testing.T) {
	tearDown, config, _, err := fixtures.StartDefaultServer(t)
	if err != nil {
		t.Fatal(err)
	}
	defer tearDown()

	apiExtensionClient, err := clientset.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}

	noxuDefinitions := NewNoxuSubresourcesCRDs(apiextensionsv1.NamespaceScoped)
	for _, noxuDefinition := range noxuDefinitions {
		noxuDefinition, err = fixtures.CreateNewV1CustomResourceDefinition(noxuDefinition, apiExtensionClient, dynamicClient)
		if err != nil {
			t.Fatal(err)
		}

		for _, v := range noxuDefinition.Spec.Versions {
			group := "mygroup.example.com"
			version := v.Name

			resources, err := apiExtensionClient.Discovery().ServerResourcesForGroupVersion(group + "/" + version)
			if err != nil {
				t.Fatal(err)
			}

			if len(resources.APIResources) != 3 {
				t.Fatalf("Expected exactly the resources \"noxus\", \"noxus/status\" and \"noxus/scale\" in group version %v/%v via discovery, got: %v", group, version, resources.APIResources)
			}

			// check discovery info for status
			status := resources.APIResources[1]

			if status.Name != "noxus/status" {
				t.Fatalf("incorrect status via discovery: expected name: %v, got: %v", "noxus/status", status.Name)
			}

			if status.Namespaced != true {
				t.Fatalf("incorrect status via discovery: expected namespace: %v, got: %v", true, status.Namespaced)
			}

			if status.Kind != "WishIHadChosenNoxu" {
				t.Fatalf("incorrect status via discovery: expected kind: %v, got: %v", "WishIHadChosenNoxu", status.Kind)
			}

			expectedVerbs := []string{"get", "patch", "update"}
			sort.Strings(status.Verbs)
			if !reflect.DeepEqual([]string(status.Verbs), expectedVerbs) {
				t.Fatalf("incorrect status via discovery: expected: %v, got: %v", expectedVerbs, status.Verbs)
			}

			// check discovery info for scale
			scale := resources.APIResources[2]

			if scale.Group != autoscaling.GroupName {
				t.Fatalf("incorrect scale via discovery: expected group: %v, got: %v", autoscaling.GroupName, scale.Group)
			}

			if scale.Version != "v1" {
				t.Fatalf("incorrect scale via discovery: expected version: %v, got %v", "v1", scale.Version)
			}

			if scale.Name != "noxus/scale" {
				t.Fatalf("incorrect scale via discovery: expected name: %v, got: %v", "noxus/scale", scale.Name)
			}

			if scale.Namespaced != true {
				t.Fatalf("incorrect scale via discovery: expected namespace: %v, got: %v", true, scale.Namespaced)
			}

			if scale.Kind != "Scale" {
				t.Fatalf("incorrect scale via discovery: expected kind: %v, got: %v", "Scale", scale.Kind)
			}

			sort.Strings(scale.Verbs)
			if !reflect.DeepEqual([]string(scale.Verbs), expectedVerbs) {
				t.Fatalf("incorrect scale via discovery: expected: %v, got: %v", expectedVerbs, scale.Verbs)
			}
		}
		if err := fixtures.DeleteV1CustomResourceDefinition(noxuDefinition, apiExtensionClient); err != nil {
			t.Fatal(err)
		}
	}
}

func TestGeneration(t *testing.T) {
	tearDown, apiExtensionClient, dynamicClient, err := fixtures.StartDefaultServerWithClients(t)
	if err != nil {
		t.Fatal(err)
	}
	defer tearDown()

	noxuDefinitions := NewNoxuSubresourcesCRDs(apiextensionsv1.NamespaceScoped)
	for _, noxuDefinition := range noxuDefinitions {
		noxuDefinition, err = fixtures.CreateNewV1CustomResourceDefinition(noxuDefinition, apiExtensionClient, dynamicClient)
		if err != nil {
			t.Fatal(err)
		}

		ns := "not-the-default"
		for _, v := range noxuDefinition.Spec.Versions {
			noxuResourceClient := newNamespacedCustomResourceVersionedClient(ns, dynamicClient, noxuDefinition, v.Name)
			_, err = instantiateVersionedCustomResource(t, NewNoxuSubresourceInstance(ns, "foo", v.Name), noxuResourceClient, noxuDefinition, v.Name)
			if err != nil {
				t.Fatalf("unable to create noxu instance: %v", err)
			}

			// .metadata.generation = 1
			gottenNoxuInstance, err := noxuResourceClient.Get(context.TODO(), "foo", metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if gottenNoxuInstance.GetGeneration() != 1 {
				t.Fatalf(".metadata.generation should be 1 after creation")
			}

			// .status.num = 20
			err = unstructured.SetNestedField(gottenNoxuInstance.Object, int64(20), "status", "num")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			// UpdateStatus does not increment generation
			updatedStatusInstance, err := noxuResourceClient.UpdateStatus(context.TODO(), gottenNoxuInstance, metav1.UpdateOptions{})
			if err != nil {
				t.Fatalf("unable to update status: %v", err)
			}
			if updatedStatusInstance.GetGeneration() != 1 {
				t.Fatalf("updating status should not increment .metadata.generation: expected: %v, got: %v", 1, updatedStatusInstance.GetGeneration())
			}

			gottenNoxuInstance, err = noxuResourceClient.Get(context.TODO(), "foo", metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}

			// .spec.num = 20
			err = unstructured.SetNestedField(gottenNoxuInstance.Object, int64(20), "spec", "num")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			// Update increments generation
			updatedInstance, err := noxuResourceClient.Update(context.TODO(), gottenNoxuInstance, metav1.UpdateOptions{})
			if err != nil {
				t.Fatalf("unable to update instance: %v", err)
			}
			if updatedInstance.GetGeneration() != 2 {
				t.Fatalf("updating spec should increment .metadata.generation: expected: %v, got: %v", 2, updatedStatusInstance.GetGeneration())
			}
			noxuResourceClient.Delete(context.TODO(), "foo", metav1.DeleteOptions{})
		}
		if err := fixtures.DeleteV1CustomResourceDefinition(noxuDefinition, apiExtensionClient); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSubresourcePatch(t *testing.T) {
	groupResource := schema.GroupResource{
		Group:    "mygroup.example.com",
		Resource: "noxus",
	}

	tearDown, config, _, err := fixtures.StartDefaultServer(t)
	if err != nil {
		t.Fatal(err)
	}
	defer tearDown()

	apiExtensionClient, err := clientset.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}

	noxuDefinitions := NewNoxuSubresourcesCRDs(apiextensionsv1.NamespaceScoped)
	for _, noxuDefinition := range noxuDefinitions {
		noxuDefinition, err = fixtures.CreateNewV1CustomResourceDefinition(noxuDefinition, apiExtensionClient, dynamicClient)
		if err != nil {
			t.Fatal(err)
		}

		ns := "not-the-default"
		for _, v := range noxuDefinition.Spec.Versions {
			noxuResourceClient := newNamespacedCustomResourceVersionedClient(ns, dynamicClient, noxuDefinition, v.Name)

			t.Logf("Creating foo")
			_, err = instantiateVersionedCustomResource(t, NewNoxuSubresourceInstance(ns, "foo", v.Name), noxuResourceClient, noxuDefinition, v.Name)
			if err != nil {
				t.Fatalf("unable to create noxu instance: %v", err)
			}

			scaleClient, err := fixtures.CreateNewVersionedScaleClient(noxuDefinition, config, v.Name)
			if err != nil {
				t.Fatal(err)
			}

			t.Logf("Patching .status.num to 999")
			patch := []byte(`{"spec": {"num":999}, "status": {"num":999}}`)
			patchedNoxuInstance, err := noxuResourceClient.Patch(context.TODO(), "foo", types.MergePatchType, patch, metav1.PatchOptions{}, "status")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			expectInt64(t, patchedNoxuInstance.UnstructuredContent(), 999, "status", "num") // .status.num should be 999
			expectInt64(t, patchedNoxuInstance.UnstructuredContent(), 10, "spec", "num")    // .spec.num should remain 10

			// server-side-apply increments resourceVersion if the resource is unchanged for 1 second after the previous write,
			// and by waiting a second we ensure that resourceVersion will be updated if the no-op patch increments resourceVersion
			time.Sleep(time.Second)
			// no-op patch
			rv := patchedNoxuInstance.GetResourceVersion()
			found := false
			t.Logf("Patching .status.num again to 999")
			patchedNoxuInstance, err = noxuResourceClient.Patch(context.TODO(), "foo", types.MergePatchType, patch, metav1.PatchOptions{}, "status")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			// make sure no-op patch does not increment resourceVersion
			expectInt64(t, patchedNoxuInstance.UnstructuredContent(), 999, "status", "num")
			expectInt64(t, patchedNoxuInstance.UnstructuredContent(), 10, "spec", "num")
			expectString(t, patchedNoxuInstance.UnstructuredContent(), rv, "metadata", "resourceVersion")

			// empty patch
			t.Logf("Applying empty patch")
			patchedNoxuInstance, err = noxuResourceClient.Patch(context.TODO(), "foo", types.MergePatchType, []byte(`{}`), metav1.PatchOptions{}, "status")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			// an empty patch is a no-op patch. make sure it does not increment resourceVersion
			expectInt64(t, patchedNoxuInstance.UnstructuredContent(), 999, "status", "num")
			expectInt64(t, patchedNoxuInstance.UnstructuredContent(), 10, "spec", "num")
			expectString(t, patchedNoxuInstance.UnstructuredContent(), rv, "metadata", "resourceVersion")

			t.Logf("Patching .spec.replicas to 7")
			patch = []byte(`{"spec": {"replicas":7}, "status": {"replicas":7}}`)
			patchedNoxuInstance, err = noxuResourceClient.Patch(context.TODO(), "foo", types.MergePatchType, patch, metav1.PatchOptions{}, "scale")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			expectInt64(t, patchedNoxuInstance.UnstructuredContent(), 7, "spec", "replicas")
			expectInt64(t, patchedNoxuInstance.UnstructuredContent(), 0, "status", "replicas") // .status.replicas should remain 0
			rv, found, err = unstructured.NestedString(patchedNoxuInstance.UnstructuredContent(), "metadata", "resourceVersion")
			if err != nil {
				t.Fatal(err)
			}
			if !found {
				t.Fatalf("metadata.resourceVersion not found")
			}

			// Scale.Spec.Replicas = 7 but Scale.Status.Replicas should remain 0
			gottenScale, err := scaleClient.Scales("not-the-default").Get(context.TODO(), groupResource, "foo", metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if gottenScale.Spec.Replicas != 7 {
				t.Fatalf("Scale.Spec.Replicas: expected: %v, got: %v", 7, gottenScale.Spec.Replicas)
			}
			if gottenScale.Status.Replicas != 0 {
				t.Fatalf("Scale.Status.Replicas: expected: %v, got: %v", 0, gottenScale.Spec.Replicas)
			}

			// no-op patch
			t.Logf("Patching .spec.replicas again to 7")
			patchedNoxuInstance, err = noxuResourceClient.Patch(context.TODO(), "foo", types.MergePatchType, patch, metav1.PatchOptions{}, "scale")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			// make sure no-op patch does not increment resourceVersion
			expectInt64(t, patchedNoxuInstance.UnstructuredContent(), 7, "spec", "replicas")
			expectInt64(t, patchedNoxuInstance.UnstructuredContent(), 0, "status", "replicas")
			expectString(t, patchedNoxuInstance.UnstructuredContent(), rv, "metadata", "resourceVersion")

			// empty patch
			t.Logf("Applying empty patch")
			patchedNoxuInstance, err = noxuResourceClient.Patch(context.TODO(), "foo", types.MergePatchType, []byte(`{}`), metav1.PatchOptions{}, "scale")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			// an empty patch is a no-op patch. make sure it does not increment resourceVersion
			expectInt64(t, patchedNoxuInstance.UnstructuredContent(), 7, "spec", "replicas")
			expectInt64(t, patchedNoxuInstance.UnstructuredContent(), 0, "status", "replicas")
			expectString(t, patchedNoxuInstance.UnstructuredContent(), rv, "metadata", "resourceVersion")

			// make sure strategic merge patch is not supported for both status and scale
			_, err = noxuResourceClient.Patch(context.TODO(), "foo", types.StrategicMergePatchType, patch, metav1.PatchOptions{}, "status")
			if err == nil {
				t.Fatalf("unexpected non-error: strategic merge patch is not supported for custom resources")
			}

			_, err = noxuResourceClient.Patch(context.TODO(), "foo", types.StrategicMergePatchType, patch, metav1.PatchOptions{}, "scale")
			if err == nil {
				t.Fatalf("unexpected non-error: strategic merge patch is not supported for custom resources")
			}
			noxuResourceClient.Delete(context.TODO(), "foo", metav1.DeleteOptions{})
		}
		if err := fixtures.DeleteV1CustomResourceDefinition(noxuDefinition, apiExtensionClient); err != nil {
			t.Fatal(err)
		}
	}
}
