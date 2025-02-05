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
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"sync"
	"testing"
	"time"

	openapi_v2 "github.com/google/gnostic-models/openapiv2"
	"sigs.k8s.io/yaml"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	clientset "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	apiextensionsfeatures "k8s.io/apiextensions-apiserver/pkg/features"
	"k8s.io/apiextensions-apiserver/test/integration/conversion"
	"k8s.io/apiextensions-apiserver/test/integration/fixtures"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/openapi3"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	"k8s.io/klog/v2/ktesting"
	"k8s.io/kube-openapi/pkg/spec3"
)

var selectableFieldFixture = &apiextensionsv1.CustomResourceDefinition{
	ObjectMeta: metav1.ObjectMeta{Name: "shirts.tests.example.com"},
	Spec: apiextensionsv1.CustomResourceDefinitionSpec{
		Group: "tests.example.com",
		Versions: []apiextensionsv1.CustomResourceDefinitionVersion{
			{
				Name:    "v1",
				Storage: true,
				Served:  true,
				Schema: &apiextensionsv1.CustomResourceValidation{
					OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
						Type: "object",
						Properties: map[string]apiextensionsv1.JSONSchemaProps{
							"spec": {
								Type: "object",
								Properties: map[string]apiextensionsv1.JSONSchemaProps{
									"color": {
										Type: "string",
									},
									"quantity": {
										Type: "integer",
									},
									"size": {
										Type: "string",
										Enum: []apiextensionsv1.JSON{
											{Raw: []byte(`"S"`)},
											{Raw: []byte(`"M"`)},
											{Raw: []byte(`"L"`)},
											{Raw: []byte(`"XL"`)},
										},
									},
									"branded": {
										Type: "boolean",
									},
								},
							},
						},
					},
				},
				SelectableFields: []apiextensionsv1.SelectableField{
					{JSONPath: ".spec.color"},
					{JSONPath: ".spec.quantity"},
					{JSONPath: ".spec.size"},
					{JSONPath: ".spec.branded"},
				},
			},
			{
				Name:    "v1beta1",
				Storage: false,
				Served:  true,
				Schema: &apiextensionsv1.CustomResourceValidation{
					OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
						Type: "object",
						Properties: map[string]apiextensionsv1.JSONSchemaProps{
							"spec": {
								Type: "object",
								Properties: map[string]apiextensionsv1.JSONSchemaProps{
									"hue": { // color is renamed as "hue" in this version
										Type: "string",
									},
									"quantity": {
										Type: "integer",
									},
									"size": {
										Type: "string",
										Enum: []apiextensionsv1.JSON{
											{Raw: []byte(`"S"`)},
											{Raw: []byte(`"M"`)},
											{Raw: []byte(`"L"`)},
											{Raw: []byte(`"XL"`)},
										},
									},
									"branded": {
										Type: "boolean",
									},
								},
							},
						},
					},
				},
				SelectableFields: []apiextensionsv1.SelectableField{
					{JSONPath: ".spec.hue"},
					{JSONPath: ".spec.quantity"},
					{JSONPath: ".spec.size"},
					{JSONPath: ".spec.branded"},
				},
			},
		},
		Names: apiextensionsv1.CustomResourceDefinitionNames{
			Plural:   "shirts",
			Singular: "shirt",
			Kind:     "Shirt",
			ListKind: "ShirtList",
		},
		Scope:                 apiextensionsv1.ClusterScoped,
		PreserveUnknownFields: false,
	},
}

const shirtInstance1 = `
kind: Shirt
apiVersion: tests.example.com/v1
metadata:
  name: shirt1
spec:
  color: blue
  quantity: 2
  size: S
  branded: true
`

const shirtInstance2 = `
kind: Shirt
apiVersion: tests.example.com/v1
metadata:
  name: shirt2
spec:
  color: blue
  quantity: 3
  size: M
  branded: false
`

const shirtInstance3 = `
kind: Shirt
apiVersion: tests.example.com/v1
metadata:
  name: shirt3
spec:
  color: green
  quantity: 2
  branded: false
`

type selectableFieldTestCase struct {
	version              string
	fieldSelector        string
	expectedByName       sets.Set[string]
	expectObserveRemoval sets.Set[string]
	expectError          string
}

func (sf selectableFieldTestCase) Name() string {
	return fmt.Sprintf("%s/%s", sf.version, sf.fieldSelector)
}

func TestSelectableFields(t *testing.T) {
	// start a conversion webhook
	handler := conversion.NewObjectConverterWebhookHandler(t, crdConverter)
	upCh, handler := closeOnCall(handler)
	whTearDown, webhookClientConfig, err := conversion.StartConversionWebhookServer(handler)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(whTearDown)

	_, ctx := ktesting.NewTestContext(t)
	tearDown, apiExtensionClient, dynamicClient, err := fixtures.StartDefaultServerWithClients(t)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tearDown)

	crd := selectableFieldFixture.DeepCopy()

	if webhookClientConfig != nil {
		crd.Spec.Conversion = &apiextensionsv1.CustomResourceConversion{
			Strategy: apiextensionsv1.WebhookConverter,
			Webhook: &apiextensionsv1.WebhookConversion{
				ClientConfig:             webhookClientConfig,
				ConversionReviewVersions: []string{"v1", "v1beta1"},
			},
		}
	}

	// create the CRD
	crd, err = fixtures.CreateNewV1CustomResourceDefinition(crd, apiExtensionClient, dynamicClient)
	if err != nil {
		t.Fatal(err)
	}

	// use the v1 client to create a resource, stored at v1
	shirtv1Client := dynamicClient.Resource(schema.GroupVersionResource{Group: crd.Spec.Group, Version: crd.Spec.Versions[0].Name, Resource: crd.Spec.Names.Plural})
	for _, instance := range []string{shirtInstance1} {
		shirt := &unstructured.Unstructured{}
		if err := yaml.Unmarshal([]byte(instance), &shirt.Object); err != nil {
			t.Fatal(err)
		}

		_, err = shirtv1Client.Create(ctx, shirt, metav1.CreateOptions{})
		if err != nil {
			t.Fatalf("Unable to create CR: %v", err)
		}
	}

	shirtv1beta1Client := dynamicClient.Resource(schema.GroupVersionResource{Group: crd.Spec.Group, Version: crd.Spec.Versions[0].Name, Resource: crd.Spec.Names.Plural})

	// read CRs with the v1beta1 client and
	// wait until conversion webhook is called the first time
	if err := wait.PollUntilContextTimeout(ctx, time.Millisecond*100, wait.ForeverTestTimeout, true, func(ctx context.Context) (bool, error) {
		_, err := shirtv1beta1Client.Get(ctx, shirtInstance1, metav1.GetOptions{})
		select {
		case <-upCh:
			return true, nil
		default:
			t.Logf("Waiting for webhook to become effective, getting marker object: %v", err)
			return false, nil
		}
	}); err != nil {
		t.Fatal(err)
	}

	var tcs []selectableFieldTestCase
	for _, version := range []string{"v1", "v1beta1"} {
		var colorSelector string
		switch version {
		case "v1":
			colorSelector = "spec.color"
		case "v1beta1":
			colorSelector = "spec.hue"
		}

		tcs = append(tcs, []selectableFieldTestCase{
			{
				version:              version,
				fieldSelector:        fmt.Sprintf("%s=blue", colorSelector),
				expectedByName:       sets.New("shirt1", "shirt2"),
				expectObserveRemoval: sets.New("shirt1", "shirt2"), // shirt 1 is deleted, shirt 2 is updated to not match the selector
			},
			{
				version:              version,
				fieldSelector:        "spec.quantity=2",
				expectedByName:       sets.New("shirt1", "shirt3"),
				expectObserveRemoval: sets.New("shirt1"), // shirt 1 is deleted
			},
			{
				version:        version,
				fieldSelector:  "spec.size=M",
				expectedByName: sets.New("shirt2"),
			},
			{
				version:        version,
				fieldSelector:  "spec.branded=false",
				expectedByName: sets.New("shirt2", "shirt3"),
			},
			{
				version:              version,
				fieldSelector:        fmt.Sprintf("%s=blue,spec.quantity=2", colorSelector),
				expectedByName:       sets.New("shirt1"),
				expectObserveRemoval: sets.New("shirt1"), // shirt 1 is deleted
			},
			{
				version:              version,
				fieldSelector:        fmt.Sprintf("%s=blue,spec.branded=false", colorSelector),
				expectedByName:       sets.New("shirt2"),
				expectObserveRemoval: sets.New("shirt2"), // shirt 2 is updated to not match the selector
			},
			{
				version:        version,
				fieldSelector:  "spec.nosuchfield=xyz",
				expectedByName: sets.New[string](),
				expectError:    "field label not supported: spec.nosuchfield",
			},
		}...)
	}

	t.Run("watch", func(t *testing.T) {
		testWatch(ctx, t, tcs, dynamicClient)
	})
	t.Run("list", func(t *testing.T) {
		testList(ctx, t, tcs, dynamicClient)
	})
	t.Run("deleteCollection", func(t *testing.T) {
		testDeleteCollection(ctx, t, tcs, dynamicClient)
	})
}

func testWatch(ctx context.Context, t *testing.T, tcs []selectableFieldTestCase, dynamicClient dynamic.Interface) {
	clients := map[string]dynamic.NamespaceableResourceInterface{}
	for _, version := range []string{"v1", "v1beta1"} {
		clients[version] = dynamicClient.Resource(schema.GroupVersionResource{Group: "tests.example.com", Version: version, Resource: "shirts"})
	}

	deleteTestResources(ctx, t, dynamicClient)
	watches := map[string]watch.Interface{}
	for _, tc := range tcs {
		shirtClient := clients[tc.version]
		w, err := shirtClient.Watch(ctx, metav1.ListOptions{FieldSelector: tc.fieldSelector})
		if len(tc.expectError) > 0 {
			if err == nil {
				t.Errorf("Expected error but got none while creating watch for %s", tc.Name())
			}
			continue
		}
		if err != nil {
			t.Fatalf("failed to create watch for %s: %v", tc.Name(), err)
		} else {
			watches[tc.Name()] = w
		}
	}
	defer func() {
		for _, w := range watches {
			w.Stop()
		}
	}()

	createTestResources(ctx, t, dynamicClient)

	// after creating resources, delete one to make sure deletions can be observed
	toDelete := "shirt1"
	var gracePeriod int64 = 0
	err := clients["v1"].Delete(ctx, toDelete, metav1.DeleteOptions{GracePeriodSeconds: &gracePeriod})
	if err != nil {
		t.Fatal(err)
	}

	// after creating resources, update the color of one CR to longer appear in a field selected watch.
	toUpdate := "shirt2"
	u, err := clients["v1"].Get(ctx, toUpdate, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	u.Object["spec"].(map[string]any)["color"] = "green"
	_, err = clients["v1"].Update(ctx, u, metav1.UpdateOptions{})
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range tcs {
		t.Run(tc.Name(), func(t *testing.T) {
			added := sets.New[string]()
			deleted := sets.New[string]()
			if len(tc.expectError) > 0 {
				return // No watch events to check for error cases. The failure happens at watch creation.
			}
			w := watches[tc.Name()]
			for {
				select {
				case <-time.After(100 * time.Millisecond):
					// Check after a wait to ensure we don't eagerly assume
					// the right watch events were received.
					if added.Equal(tc.expectedByName) && deleted.Equal(tc.expectObserveRemoval) {
						return
					} else {
						t.Fatalf("Timed out waiting for watch events, expected added: %v, removed: %v, but got added: %v, removed: %v", tc.expectedByName, tc.expectObserveRemoval, added, deleted)
					}
				case event := <-w.ResultChan():
					obj, err := meta.Accessor(event.Object)
					if err != nil {
						t.Fatal(err)
					}
					switch event.Type {
					case watch.Added:
						added.Insert(obj.GetName())
					case watch.Deleted:
						deleted.Insert(obj.GetName())
					default:
						// ignore everything else
					}
				}
			}
		})
	}
}

func testList(ctx context.Context, t *testing.T, tcs []selectableFieldTestCase, dynamicClient dynamic.Interface) {
	clients := map[string]dynamic.NamespaceableResourceInterface{}
	for _, version := range []string{"v1", "v1beta1"} {
		clients[version] = dynamicClient.Resource(schema.GroupVersionResource{Group: "tests.example.com", Version: version, Resource: "shirts"})
	}

	deleteTestResources(ctx, t, dynamicClient)
	createTestResources(ctx, t, dynamicClient)

	for _, tc := range tcs {
		t.Run(tc.Name(), func(t *testing.T) {
			shirtClient := clients[tc.version]
			list, err := shirtClient.List(ctx, metav1.ListOptions{FieldSelector: tc.fieldSelector})
			if len(tc.expectError) > 0 {
				if err == nil {
					t.Fatal("Expected error but got none")
				}
				if tc.expectError != err.Error() {
					t.Errorf("Expected error '%s' but got '%s'", tc.expectError, err.Error())
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			found := sets.New[string]()
			for _, i := range list.Items {
				found.Insert(i.GetName())
			}
			if !found.Equal(tc.expectedByName) {
				t.Errorf("Expected %v but got %v", tc.expectedByName, found)
			}
		})
	}
}

func testDeleteCollection(ctx context.Context, t *testing.T, tcs []selectableFieldTestCase, dynamicClient dynamic.Interface) {
	clients := map[string]dynamic.NamespaceableResourceInterface{}
	for _, version := range []string{"v1", "v1beta1"} {
		clients[version] = dynamicClient.Resource(schema.GroupVersionResource{Group: "tests.example.com", Version: version, Resource: "shirts"})
	}

	for _, tc := range tcs {
		t.Run(tc.Name(), func(t *testing.T) {
			deleteTestResources(ctx, t, dynamicClient)
			createTestResources(ctx, t, dynamicClient)
			shirtClient := clients[tc.version]
			var gracePeriod int64 = 0
			err := shirtClient.DeleteCollection(ctx, metav1.DeleteOptions{GracePeriodSeconds: &gracePeriod}, metav1.ListOptions{FieldSelector: tc.fieldSelector})
			if len(tc.expectError) > 0 {
				if err == nil {
					t.Fatal("Expected error but got none")
				}
				if tc.expectError != err.Error() {
					t.Errorf("Expected error '%s' but got '%s'", tc.expectError, err.Error())
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			list, err := shirtClient.List(ctx, metav1.ListOptions{})
			if err != nil {
				t.Fatal(err)
			}
			removed := sets.New[string]("shirt1", "shirt2", "shirt3")
			for _, i := range list.Items {
				removed.Delete(i.GetName()) // drop remaining CRs from removed set
			}
			if !removed.Equal(tc.expectedByName) {
				t.Errorf("Expected %v but got %v", tc.expectedByName, removed)
			}
		})
	}
}

func TestFieldSelectorOpenAPI(t *testing.T) {
	_, ctx := ktesting.NewTestContext(t)
	tearDown, config, _, err := fixtures.StartDefaultServer(t)
	if err != nil {
		t.Fatal(err)
	}
	defer tearDown()

	apiExtensionsClient, err := clientset.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}

	discoveryClient, err := discovery.NewDiscoveryClientForConfig(config)
	if err != nil {
		t.Fatal(err)
	}

	crd := selectableFieldFixture.DeepCopy()
	crd, err = fixtures.CreateNewV1CustomResourceDefinitionWatchUnsafe(crd, apiExtensionsClient)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("OpenAPIv3", func(t *testing.T) {
		var spec *spec3.OpenAPI
		err = wait.PollUntilContextCancel(ctx, 100*time.Millisecond, true, func(ctx context.Context) (done bool, err error) {
			// wait for the CRD to be published.
			root := openapi3.NewRoot(discoveryClient.OpenAPIV3())
			spec, err = root.GVSpec(schema.GroupVersion{Group: crd.Spec.Group, Version: "v1"})
			return err == nil, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		shirtSchema, ok := spec.Components.Schemas["com.example.tests.v1.Shirt"]
		if !ok {
			t.Fatal("Expected com.example.tests.v1.Shirt in discovery schemas")
		}
		selectableFields, ok := shirtSchema.VendorExtensible.Extensions["x-kubernetes-selectable-fields"]
		if !ok {
			t.Fatal("Expected x-kubernetes-selectable-fields in extensions")
		}

		expected := []any{
			map[string]any{
				"fieldPath": "spec.color",
			},
			map[string]any{
				"fieldPath": "spec.quantity",
			},
			map[string]any{
				"fieldPath": "spec.size",
			},
			map[string]any{
				"fieldPath": "spec.branded",
			},
		}
		if !reflect.DeepEqual(selectableFields, expected) {
			t.Errorf("expected %v but got %v", selectableFields, expected)
		}
	})

	t.Run("OpenAPIv2", func(t *testing.T) {
		v2, err := discoveryClient.OpenAPISchema()
		if err != nil {
			t.Fatal(err)
		}
		var v2Prop *openapi_v2.NamedSchema
		for _, prop := range v2.Definitions.AdditionalProperties {
			if prop.Name == "com.example.tests.v1.Shirt" {
				v2Prop = prop
			}
		}
		if v2Prop == nil {
			t.Fatal("Expected com.example.tests.v1.Shirt definition")
		}
		var v2selectableFields *openapi_v2.NamedAny
		for _, ve := range v2Prop.Value.VendorExtension {
			if ve.Name == "x-kubernetes-selectable-fields" {
				v2selectableFields = ve
			}
		}
		if v2selectableFields == nil {
			t.Fatal("Expected x-kubernetes-selectable-fields")
		}
		expected := `- fieldPath: spec.color
- fieldPath: spec.quantity
- fieldPath: spec.size
- fieldPath: spec.branded
`
		if v2selectableFields.Value.Yaml != expected {
			t.Errorf("Expected %s but got %s", v2selectableFields.Value.Yaml, expected)
		}
	})
}

func TestFieldSelectorDropFields(t *testing.T) {
	_, ctx := ktesting.NewTestContext(t)
	tearDown, apiExtensionClient, _, err := fixtures.StartDefaultServerWithClients(t, "--emulated-version=1.31", "--feature-gates=CustomResourceFieldSelectors=false")
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
									"field": {Type: "string"},
								},
								Required: []string{"field"},
							},
						},
					},
				},
				SelectableFields: []apiextensionsv1.SelectableField{
					{JSONPath: ".spec.field"},
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

	created, err := apiExtensionClient.ApiextensionsV1().CustomResourceDefinitions().Create(ctx, myCRD, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if created.Spec.Versions[0].SelectableFields != nil {
		t.Errorf("Expected SelectableFields field to be dropped for create when feature gate is disabled")
	}

	var updated *apiextensionsv1.CustomResourceDefinition
	err = wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 5*time.Second, true, func(ctx context.Context) (bool, error) {
		existing, err := apiExtensionClient.ApiextensionsV1().CustomResourceDefinitions().Get(ctx, created.Name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		existing.Spec.Versions[0].SelectableFields = []apiextensionsv1.SelectableField{{JSONPath: ".spec.field"}}
		updated, err = apiExtensionClient.ApiextensionsV1().CustomResourceDefinitions().Update(ctx, existing, metav1.UpdateOptions{})
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

	if updated.Spec.Versions[0].SelectableFields != nil {
		t.Errorf("Expected SelectableFields field to be dropped for create when feature gate is disabled")
	}
}

func TestFieldSelectorDisablement(t *testing.T) {
	_, ctx := ktesting.NewTestContext(t)
	tearDown, config, _, err := fixtures.StartDefaultServer(t, "--emulated-version=1.31")
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

	discoveryClient, err := discovery.NewDiscoveryClientForConfig(config)
	if err != nil {
		t.Fatal(err)
	}

	crd := selectableFieldFixture.DeepCopy()
	// Write a field that uses the feature while the feature gate is enabled
	t.Run("CustomResourceFieldSelectors", func(t *testing.T) {
		featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, apiextensionsfeatures.CustomResourceFieldSelectors, true)
		crd, err = fixtures.CreateNewV1CustomResourceDefinition(crd, apiExtensionClient, dynamicClient)
		if err != nil {
			t.Fatal(err)
		}
	})
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, apiextensionsfeatures.CustomResourceFieldSelectors, false)

	// Now that the feature gate is disabled again, update the CRD to trigger an openAPI update
	crd, err = apiExtensionClient.ApiextensionsV1().CustomResourceDefinitions().Get(ctx, crd.Name, metav1.GetOptions{})
	crd.Spec.Versions[0].SelectableFields = []apiextensionsv1.SelectableField{
		{JSONPath: ".spec.color"},
		{JSONPath: ".spec.quantity"},
	}
	crd, err = apiExtensionClient.ApiextensionsV1().CustomResourceDefinitions().Update(ctx, crd, metav1.UpdateOptions{})
	if err != nil {
		t.Fatal(err)
	}

	shirtClient := dynamicClient.Resource(schema.GroupVersionResource{Group: crd.Spec.Group, Version: crd.Spec.Versions[0].Name, Resource: crd.Spec.Names.Plural})

	invalidRequestCases := []struct {
		fieldSelector string
	}{
		{
			fieldSelector: "spec.color=blue",
		},
	}

	t.Run("watch", func(t *testing.T) {
		for _, tc := range invalidRequestCases {
			t.Run(tc.fieldSelector, func(t *testing.T) {
				w, err := shirtClient.Watch(ctx, metav1.ListOptions{FieldSelector: tc.fieldSelector})
				if err == nil {
					w.Stop()
					t.Fatal("Expected error but got none")
				}
				if !apierrors.IsBadRequest(err) {
					t.Errorf("Expected BadRequest but got %v", err)
				}
			})
		}
	})

	for _, instance := range []string{shirtInstance1, shirtInstance2, shirtInstance3} {
		shirt := &unstructured.Unstructured{}
		if err := yaml.Unmarshal([]byte(instance), &shirt.Object); err != nil {
			t.Fatal(err)
		}

		_, err = shirtClient.Create(ctx, shirt, metav1.CreateOptions{})
		if err != nil {
			t.Fatalf("Unable to create CR: %v", err)
		}
	}

	t.Run("list", func(t *testing.T) {
		for _, tc := range invalidRequestCases {
			t.Run(tc.fieldSelector, func(t *testing.T) {
				_, err := shirtClient.List(ctx, metav1.ListOptions{FieldSelector: tc.fieldSelector})
				if err == nil {
					t.Error("Expected error but got none")
				}
				if !apierrors.IsBadRequest(err) {
					t.Errorf("Expected BadRequest but got %v", err)
				}
				expected := "field label not supported: spec.color"
				if err.Error() != expected {
					t.Errorf("Expected '%s' but got '%s'", expected, err.Error())
				}
			})
		}
	})

	t.Run("OpenAPIv3", func(t *testing.T) {
		var spec *spec3.OpenAPI
		err = wait.PollUntilContextCancel(ctx, 100*time.Millisecond, true, func(ctx context.Context) (done bool, err error) {
			// wait for the CRD to be published.
			root := openapi3.NewRoot(discoveryClient.OpenAPIV3())
			spec, err = root.GVSpec(schema.GroupVersion{Group: crd.Spec.Group, Version: "v1"})
			if err != nil {
				return false, nil
			}
			shirtSchema, ok := spec.Components.Schemas["com.example.tests.v1.Shirt"]
			if !ok {
				return false, nil
			}
			_, found := shirtSchema.VendorExtensible.Extensions["x-kubernetes-selectable-fields"]
			return !found, nil // the feature gate is disabled, so selectable fields should be absent
		})
		if err != nil {
			t.Fatal(err)
		}
	})

	t.Run("OpenAPIv2", func(t *testing.T) {
		v2, err := discoveryClient.OpenAPISchema()
		if err != nil {
			t.Fatal(err)
		}
		var v2Prop *openapi_v2.NamedSchema
		for _, prop := range v2.Definitions.AdditionalProperties {
			if prop.Name == "com.example.tests.v1.Shirt" {
				v2Prop = prop
			}
		}
		if v2Prop == nil {
			t.Fatal("Expected com.example.tests.v1.Shirt definition")
		}
		var v2selectableFields *openapi_v2.NamedAny
		for _, ve := range v2Prop.Value.VendorExtension {
			if ve.Name == "x-kubernetes-selectable-fields" {
				v2selectableFields = ve
			}
		}
		if v2selectableFields != nil {
			t.Fatal("Did not expect to find x-kubernetes-selectable-fields")
		}
	})
}

func createTestResources(ctx context.Context, t *testing.T, dynamicClient dynamic.Interface) {
	v1Client := dynamicClient.Resource(schema.GroupVersionResource{Group: "tests.example.com", Version: "v1", Resource: "shirts"})
	for _, instance := range []string{shirtInstance1, shirtInstance2, shirtInstance3} {
		shirt := &unstructured.Unstructured{}
		if err := yaml.Unmarshal([]byte(instance), &shirt.Object); err != nil {
			t.Fatal(err)
		}

		_, err := v1Client.Create(ctx, shirt, metav1.CreateOptions{})
		if err != nil {
			t.Fatalf("Unable to create CR: %v", err)
		}
	}
}

func deleteTestResources(ctx context.Context, t *testing.T, dynamicClient dynamic.Interface) {
	v1Client := dynamicClient.Resource(schema.GroupVersionResource{Group: "tests.example.com", Version: "v1", Resource: "shirts"})

	var gracePeriod int64 = 0
	err := v1Client.DeleteCollection(ctx, metav1.DeleteOptions{GracePeriodSeconds: &gracePeriod}, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
}

func closeOnCall(h http.Handler) (chan struct{}, http.Handler) {
	ch := make(chan struct{})
	once := sync.Once{}
	return ch, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() {
			close(ch)
		})
		h.ServeHTTP(w, r)
	})
}

func crdConverter(desiredAPIVersion string, obj runtime.RawExtension) (runtime.RawExtension, error) {
	u := &unstructured.Unstructured{Object: map[string]interface{}{}}
	if err := json.Unmarshal(obj.Raw, u); err != nil {
		return runtime.RawExtension{}, fmt.Errorf("failed to deserialize object: %s with error: %w", string(obj.Raw), err)
	}

	currentAPIVersion := u.GetAPIVersion()

	if currentAPIVersion == "tests.example.com/v1beta1" && desiredAPIVersion == "tests.example.com/v1" {
		spec := u.Object["spec"].(map[string]any)
		spec["color"] = spec["hue"]
		delete(spec, "hue")
	} else if currentAPIVersion == "tests.example.com/v1" && desiredAPIVersion == "tests.example.com/v1beta1" {
		spec := u.Object["spec"].(map[string]any)
		spec["hue"] = spec["color"]
		delete(spec, "color")
	} else if currentAPIVersion != desiredAPIVersion {
		return runtime.RawExtension{}, fmt.Errorf("cannot convert from %s to %s", currentAPIVersion, desiredAPIVersion)
	}
	u.Object["apiVersion"] = desiredAPIVersion
	raw, err := json.Marshal(u)
	if err != nil {
		return runtime.RawExtension{}, fmt.Errorf("failed to serialize object: %v with error: %w", u, err)
	}
	return runtime.RawExtension{Raw: raw}, nil
}
