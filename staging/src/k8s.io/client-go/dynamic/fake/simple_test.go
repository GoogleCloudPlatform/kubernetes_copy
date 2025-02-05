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

package fake

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgotesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"
)

const (
	testGroup      = "testgroup"
	testVersion    = "testversion"
	testResource   = "testkinds"
	testNamespace  = "testns"
	testName       = "testname"
	testKind       = "TestKind"
	testAPIVersion = "testgroup/testversion"
)

func newUnstructured(apiVersion, kind, namespace, name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": apiVersion,
			"kind":       kind,
			"metadata": map[string]interface{}{
				"namespace": namespace,
				"name":      name,
			},
		},
	}
}

func newUnstructuredWithNoNamespace(apiVersion, kind, name string) *unstructured.Unstructured {
	u := newUnstructured(apiVersion, kind, "", name)
	delete(u.Object["metadata"].(map[string]interface{}), "namespace")
	return u
}

func newUnstructuredWithSpec(spec map[string]interface{}) *unstructured.Unstructured {
	u := newUnstructured(testAPIVersion, testKind, testNamespace, testName)
	u.Object["spec"] = spec
	return u
}

func TestGet(t *testing.T) {
	scheme := runtime.NewScheme()

	client := NewSimpleDynamicClient(scheme, newUnstructured("group/version", "TheKind", "ns-foo", "name-foo"))
	get, err := client.Resource(schema.GroupVersionResource{Group: "group", Version: "version", Resource: "thekinds"}).Namespace("ns-foo").Get(context.TODO(), "name-foo", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}

	expected := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "group/version",
			"kind":       "TheKind",
			"metadata": map[string]interface{}{
				"name":      "name-foo",
				"namespace": "ns-foo",
			},
		},
	}
	if !equality.Semantic.DeepEqual(get, expected) {
		t.Fatal(cmp.Diff(expected, get))
	}
}

func TestListDecoding(t *testing.T) {
	// this the duplication of logic from the real List API.  This will prove that our dynamic client actually returns the gvk
	uncastObj, err := runtime.Decode(unstructured.UnstructuredJSONScheme, []byte(`{"apiVersion": "group/version", "kind": "TheKindList", "items":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	list := uncastObj.(*unstructured.UnstructuredList)
	expectedList := &unstructured.UnstructuredList{
		Object: map[string]interface{}{
			"apiVersion": "group/version",
			"kind":       "TheKindList",
		},
		Items: []unstructured.Unstructured{},
	}
	if !equality.Semantic.DeepEqual(list, expectedList) {
		t.Fatal(cmp.Diff(expectedList, list))
	}
}

func TestGetDecoding(t *testing.T) {
	// this the duplication of logic from the real Get API.  This will prove that our dynamic client actually returns the gvk
	uncastObj, err := runtime.Decode(unstructured.UnstructuredJSONScheme, []byte(`{"apiVersion": "group/version", "kind": "TheKind"}`))
	if err != nil {
		t.Fatal(err)
	}
	get := uncastObj.(*unstructured.Unstructured)
	expectedObj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "group/version",
			"kind":       "TheKind",
		},
	}
	if !equality.Semantic.DeepEqual(get, expectedObj) {
		t.Fatal(cmp.Diff(expectedObj, get))
	}
}

func TestList(t *testing.T) {
	scheme := runtime.NewScheme()

	client := NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{
			{Group: "group", Version: "version", Resource: "thekinds"}: "TheKindList",
		},
		newUnstructured("group/version", "TheKind", "ns-foo", "name-foo"),
		newUnstructured("group2/version", "TheKind", "ns-foo", "name2-foo"),
		newUnstructured("group/version", "TheKind", "ns-foo", "name-bar"),
		newUnstructured("group/version", "TheKind", "ns-foo", "name-baz"),
		newUnstructured("group2/version", "TheKind", "ns-foo", "name2-baz"),
	)
	listFirst, err := client.Resource(schema.GroupVersionResource{Group: "group", Version: "version", Resource: "thekinds"}).List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}

	expected := []unstructured.Unstructured{
		*newUnstructured("group/version", "TheKind", "ns-foo", "name-bar"),
		*newUnstructured("group/version", "TheKind", "ns-foo", "name-baz"),
		*newUnstructured("group/version", "TheKind", "ns-foo", "name-foo"),
	}
	if !equality.Semantic.DeepEqual(listFirst.Items, expected) {
		t.Fatal(cmp.Diff(expected, listFirst.Items))
	}
}

func Test_ListKind(t *testing.T) {
	scheme := runtime.NewScheme()

	client := NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{
			{Group: "group", Version: "version", Resource: "thekinds"}: "TheKindList",
		},
		&unstructured.UnstructuredList{
			Object: map[string]interface{}{
				"apiVersion": "group/version",
				"kind":       "TheKindList",
			},
			Items: []unstructured.Unstructured{
				*newUnstructured("group/version", "TheKind", "ns-foo", "name-foo"),
				*newUnstructured("group/version", "TheKind", "ns-foo", "name-bar"),
				*newUnstructured("group/version", "TheKind", "ns-foo", "name-baz"),
			},
		},
	)
	listFirst, err := client.Resource(schema.GroupVersionResource{Group: "group", Version: "version", Resource: "thekinds"}).List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}

	expectedList := &unstructured.UnstructuredList{
		Object: map[string]interface{}{
			"apiVersion": "group/version",
			"kind":       "TheKindList",
			"metadata": map[string]interface{}{
				"continue":        "",
				"resourceVersion": "",
			},
		},
		Items: []unstructured.Unstructured{
			*newUnstructured("group/version", "TheKind", "ns-foo", "name-bar"),
			*newUnstructured("group/version", "TheKind", "ns-foo", "name-baz"),
			*newUnstructured("group/version", "TheKind", "ns-foo", "name-foo"),
		},
	}
	if !equality.Semantic.DeepEqual(listFirst, expectedList) {
		t.Fatal(cmp.Diff(expectedList, listFirst))
	}
}

type patchTestCase struct {
	name                  string
	object                runtime.Object
	patchType             types.PatchType
	patchBytes            []byte
	wantErrMsg            string
	expectedPatchedObject runtime.Object
}

func (tc *patchTestCase) runner(t *testing.T) {
	client := NewSimpleDynamicClient(runtime.NewScheme(), tc.object)
	resourceInterface := client.Resource(schema.GroupVersionResource{Group: testGroup, Version: testVersion, Resource: testResource}).Namespace(testNamespace)

	got, recErr := resourceInterface.Patch(context.TODO(), testName, tc.patchType, tc.patchBytes, metav1.PatchOptions{})

	if err := tc.verifyErr(recErr); err != nil {
		t.Error(err)
	}

	if err := tc.verifyResult(got); err != nil {
		t.Error(err)
	}

}

// verifyErr verifies that the given error returned from Patch is the error
// expected by the test case.
func (tc *patchTestCase) verifyErr(err error) error {
	if tc.wantErrMsg != "" && err == nil {
		return fmt.Errorf("want error, got nil")
	}

	if tc.wantErrMsg == "" && err != nil {
		return fmt.Errorf("want no error, got %v", err)
	}

	if err != nil {
		if want, got := tc.wantErrMsg, err.Error(); want != got {
			return fmt.Errorf("incorrect error: want: %q got: %q", want, got)
		}
	}
	return nil
}

func (tc *patchTestCase) verifyResult(result *unstructured.Unstructured) error {
	if tc.expectedPatchedObject == nil && result == nil {
		return nil
	}
	if !equality.Semantic.DeepEqual(result, tc.expectedPatchedObject) {
		return fmt.Errorf("unexpected diff in received object: %s", cmp.Diff(tc.expectedPatchedObject, result))
	}
	return nil
}

func TestPatch(t *testing.T) {
	testCases := []patchTestCase{
		{
			name:       "jsonpatch fails with merge type",
			object:     newUnstructuredWithSpec(map[string]interface{}{"foo": "bar"}),
			patchType:  types.StrategicMergePatchType,
			patchBytes: []byte(`[]`),
			wantErrMsg: "invalid JSON document",
		}, {
			name:      "jsonpatch works with empty patch",
			object:    newUnstructuredWithSpec(map[string]interface{}{"foo": "bar"}),
			patchType: types.JSONPatchType,
			// No-op
			patchBytes:            []byte(`[]`),
			expectedPatchedObject: newUnstructuredWithSpec(map[string]interface{}{"foo": "bar"}),
		}, {
			name:      "jsonpatch works with simple change patch",
			object:    newUnstructuredWithSpec(map[string]interface{}{"foo": "bar"}),
			patchType: types.JSONPatchType,
			// change spec.foo from bar to foobar
			patchBytes:            []byte(`[{"op": "replace", "path": "/spec/foo", "value": "foobar"}]`),
			expectedPatchedObject: newUnstructuredWithSpec(map[string]interface{}{"foo": "foobar"}),
		}, {
			name:      "jsonpatch works with simple addition",
			object:    newUnstructuredWithSpec(map[string]interface{}{"foo": "bar"}),
			patchType: types.JSONPatchType,
			// add spec.newvalue = dummy
			patchBytes:            []byte(`[{"op": "add", "path": "/spec/newvalue", "value": "dummy"}]`),
			expectedPatchedObject: newUnstructuredWithSpec(map[string]interface{}{"foo": "bar", "newvalue": "dummy"}),
		}, {
			name:      "jsonpatch works with simple deletion",
			object:    newUnstructuredWithSpec(map[string]interface{}{"foo": "bar", "toremove": "shouldnotbehere"}),
			patchType: types.JSONPatchType,
			// remove spec.newvalue = dummy
			patchBytes:            []byte(`[{"op": "remove", "path": "/spec/toremove"}]`),
			expectedPatchedObject: newUnstructuredWithSpec(map[string]interface{}{"foo": "bar"}),
		}, {
			name:      "strategic merge patch fails with JSONPatch",
			object:    newUnstructuredWithSpec(map[string]interface{}{"foo": "bar"}),
			patchType: types.StrategicMergePatchType,
			// add spec.newvalue = dummy
			patchBytes: []byte(`[{"op": "add", "path": "/spec/newvalue", "value": "dummy"}]`),
			wantErrMsg: "invalid JSON document",
		}, {
			name:                  "merge patch works with simple replacement",
			object:                newUnstructuredWithSpec(map[string]interface{}{"foo": "bar"}),
			patchType:             types.MergePatchType,
			patchBytes:            []byte(`{ "spec": { "foo": "baz" } }`),
			expectedPatchedObject: newUnstructuredWithSpec(map[string]interface{}{"foo": "baz"}),
		},
		// TODO: Add tests for strategic merge using v1.Pod for example to ensure the test cases
		// demonstrate expected use cases.
	}

	for _, tc := range testCases {
		t.Run(tc.name, tc.runner)
	}
}

// This test ensures list works when the fake dynamic client is seeded with a typed scheme and
// unstructured type fixtures
func TestListWithUnstructuredObjectsAndTypedScheme(t *testing.T) {
	gvr := schema.GroupVersionResource{Group: testGroup, Version: testVersion, Resource: testResource}
	gvk := gvr.GroupVersion().WithKind(testKind)

	listGVK := gvk
	listGVK.Kind += "List"

	u := unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	u.SetName("name")
	u.SetNamespace("namespace")

	typedScheme := runtime.NewScheme()
	typedScheme.AddKnownTypeWithName(gvk, &mockResource{})
	typedScheme.AddKnownTypeWithName(listGVK, &mockResourceList{})

	client := NewSimpleDynamicClient(typedScheme, &u)
	list, err := client.Resource(gvr).Namespace("namespace").List(context.Background(), metav1.ListOptions{})

	if err != nil {
		t.Error("error listing", err)
	}

	expectedList := &unstructured.UnstructuredList{}
	expectedList.SetGroupVersionKind(listGVK)
	expectedList.SetResourceVersion("") // by product of the fake setting resource version
	expectedList.SetContinue("")
	expectedList.Items = append(expectedList.Items, u)

	if diff := cmp.Diff(expectedList, list); diff != "" {
		t.Fatal("unexpected diff (-want, +got): ", diff)
	}
}

func TestListWithNoFixturesAndTypedScheme(t *testing.T) {
	gvr := schema.GroupVersionResource{Group: testGroup, Version: testVersion, Resource: testResource}
	gvk := gvr.GroupVersion().WithKind(testKind)

	listGVK := gvk
	listGVK.Kind += "List"

	typedScheme := runtime.NewScheme()
	typedScheme.AddKnownTypeWithName(gvk, &mockResource{})
	typedScheme.AddKnownTypeWithName(listGVK, &mockResourceList{})

	client := NewSimpleDynamicClient(typedScheme)
	list, err := client.Resource(gvr).Namespace("namespace").List(context.Background(), metav1.ListOptions{})

	if err != nil {
		t.Error("error listing", err)
	}

	expectedList := &unstructured.UnstructuredList{}
	expectedList.SetGroupVersionKind(listGVK)
	expectedList.SetResourceVersion("") // by product of the fake setting resource version
	expectedList.SetContinue("")

	if diff := cmp.Diff(expectedList, list); diff != "" {
		t.Fatal("unexpected diff (-want, +got): ", diff)
	}
}

// This test ensures list works when the dynamic client is seeded with an empty scheme and
// unstructured typed fixtures
func TestListWithNoScheme(t *testing.T) {
	gvr := schema.GroupVersionResource{Group: testGroup, Version: testVersion, Resource: testResource}
	gvk := gvr.GroupVersion().WithKind(testKind)

	listGVK := gvk
	listGVK.Kind += "List"

	u := unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	u.SetName("name")
	u.SetNamespace("namespace")

	emptyScheme := runtime.NewScheme()

	client := NewSimpleDynamicClient(emptyScheme, &u)
	list, err := client.Resource(gvr).Namespace("namespace").List(context.Background(), metav1.ListOptions{})

	if err != nil {
		t.Error("error listing", err)
	}

	expectedList := &unstructured.UnstructuredList{}
	expectedList.SetGroupVersionKind(listGVK)
	expectedList.SetResourceVersion("") // by product of the fake setting resource version
	expectedList.SetContinue("")
	expectedList.Items = append(expectedList.Items, u)

	if diff := cmp.Diff(expectedList, list); diff != "" {
		t.Fatal("unexpected diff (-want, +got): ", diff)
	}
}

// This test ensures list works when the dynamic client is seeded with an empty scheme and
// unstructured typed fixtures
func TestListWithTypedFixtures(t *testing.T) {
	gvr := schema.GroupVersionResource{Group: testGroup, Version: testVersion, Resource: testResource}
	gvk := gvr.GroupVersion().WithKind(testKind)

	listGVK := gvk
	listGVK.Kind += "List"

	r := mockResource{}
	r.SetGroupVersionKind(gvk)
	r.SetName("name")
	r.SetNamespace("namespace")

	u := unstructured.Unstructured{}
	u.SetGroupVersionKind(r.GetObjectKind().GroupVersionKind())
	u.SetName(r.GetName())
	u.SetNamespace(r.GetNamespace())
	// Needed see: https://github.com/kubernetes/kubernetes/issues/67610
	unstructured.SetNestedField(u.Object, nil, "metadata", "creationTimestamp")

	typedScheme := runtime.NewScheme()
	typedScheme.AddKnownTypeWithName(gvk, &mockResource{})
	typedScheme.AddKnownTypeWithName(listGVK, &mockResourceList{})

	client := NewSimpleDynamicClient(typedScheme, &r)
	list, err := client.Resource(gvr).Namespace("namespace").List(context.Background(), metav1.ListOptions{})

	if err != nil {
		t.Error("error listing", err)
	}

	expectedList := &unstructured.UnstructuredList{}
	expectedList.SetGroupVersionKind(listGVK)
	expectedList.SetResourceVersion("") // by product of the fake setting resource version
	expectedList.SetContinue("")
	expectedList.Items = []unstructured.Unstructured{u}

	if diff := cmp.Diff(expectedList, list); diff != "" {
		t.Fatal("unexpected diff (-want, +got): ", diff)
	}
}

func TestDelete(t *testing.T) {
	deleteTests := map[string]struct {
		ns            string
		name          string
		deleteOptions metav1.DeleteOptions
		subresources  []string
		wantActions   []clientgotesting.Action
	}{
		"resource deletion with namespace": {
			name: "name-foo",
			ns:   "ns-foo",
			deleteOptions: metav1.DeleteOptions{
				DryRun: []string{metav1.DryRunAll},
			},
			wantActions: []clientgotesting.Action{
				clientgotesting.DeleteActionImpl{
					Name: "name-foo",
					ActionImpl: clientgotesting.ActionImpl{
						Namespace: "ns-foo",
						Verb:      "delete",
						Resource: schema.GroupVersionResource{
							Group: "group", Version: "version", Resource: "thekinds"},
					},
					DeleteOptions: metav1.DeleteOptions{
						DryRun: []string{metav1.DryRunAll},
					},
				},
			},
		},
		"resource deletion with no namespace": {
			name: "name-foo",
			deleteOptions: metav1.DeleteOptions{
				DryRun: []string{metav1.DryRunAll},
			},
			wantActions: []clientgotesting.Action{
				clientgotesting.DeleteActionImpl{
					Name: "name-foo",
					ActionImpl: clientgotesting.ActionImpl{
						Verb: "delete",
						Resource: schema.GroupVersionResource{
							Group: "group", Version: "version", Resource: "thekinds"},
					},
					DeleteOptions: metav1.DeleteOptions{
						DryRun: []string{metav1.DryRunAll},
					},
				},
			},
		},
		"subresource deletion with namespace": {
			ns:           "ns-foo",
			name:         "test",
			subresources: []string{"status"},
			deleteOptions: metav1.DeleteOptions{
				OrphanDependents: ptr.To[bool](true),
			},
			wantActions: []clientgotesting.Action{
				clientgotesting.DeleteActionImpl{
					Name: "test",

					ActionImpl: clientgotesting.ActionImpl{
						Namespace:   "ns-foo",
						Subresource: "status",
						Verb:        "delete",
						Resource: schema.GroupVersionResource{
							Group: "group", Version: "version", Resource: "thekinds"},
					},
					DeleteOptions: metav1.DeleteOptions{
						OrphanDependents: ptr.To[bool](true),
					},
				},
			},
		},
		"subresource deletion with no namespace": {
			name:         "test",
			subresources: []string{"status"},
			deleteOptions: metav1.DeleteOptions{
				OrphanDependents: ptr.To[bool](true),
			},
			wantActions: []clientgotesting.Action{
				clientgotesting.DeleteActionImpl{
					Name: "test",
					ActionImpl: clientgotesting.ActionImpl{
						Subresource: "status",
						Verb:        "delete",
						Resource: schema.GroupVersionResource{
							Group: "group", Version: "version", Resource: "thekinds"},
					},
					DeleteOptions: metav1.DeleteOptions{
						OrphanDependents: ptr.To[bool](true),
					},
				},
			},
		},
	}

	for name, tt := range deleteTests {
		t.Run(name, func(t *testing.T) {
			scheme := runtime.NewScheme()

			client := NewSimpleDynamicClient(scheme)
			_ = client.Resource(schema.GroupVersionResource{Group: "group", Version: "version", Resource: "thekinds"}).
				Namespace(tt.ns).Delete(context.TODO(), tt.name, tt.deleteOptions, tt.subresources...)

			actions := client.Fake.Actions()
			if diff := cmp.Diff(tt.wantActions, actions); diff != "" {
				t.Fatal("unexpected diff (-want, +got): ", diff)
			}
			_, err := client.Resource(schema.GroupVersionResource{Group: "group", Version: "version", Resource: "thekinds"}).
				Namespace(tt.ns).Get(context.TODO(), "name-foo", metav1.GetOptions{})
			if !apierrors.IsNotFound(err) {
				t.Fatal("resource was not deleted")
			}
		})
	}
}

func TestCreate(t *testing.T) {
	createTests := map[string]struct {
		ns            string
		resource      *unstructured.Unstructured
		createOptions metav1.CreateOptions
		subresources  []string
		wantActions   []clientgotesting.Action
	}{
		"resource creation with namespace": {
			resource: newUnstructured("group/version", "TheKind", "ns-foo", "name-foo"),
			ns:       "ns-foo",
			createOptions: metav1.CreateOptions{
				DryRun: []string{metav1.DryRunAll},
			},
			wantActions: []clientgotesting.Action{
				clientgotesting.CreateActionImpl{
					ActionImpl: clientgotesting.ActionImpl{
						Namespace: "ns-foo",
						Verb:      "create",
						Resource: schema.GroupVersionResource{
							Group:    "group",
							Version:  "version",
							Resource: "thekinds",
						},
					},
					CreateOptions: metav1.CreateOptions{
						DryRun: []string{metav1.DryRunAll},
					},
					Object: newUnstructured("group/version", "TheKind", "ns-foo", "name-foo"),
				},
			},
		},
		"resource creation with no namespace": {
			resource: newUnstructuredWithNoNamespace("group/version", "TheKind", "name-foo"),
			createOptions: metav1.CreateOptions{
				DryRun: []string{metav1.DryRunAll},
			},
			wantActions: []clientgotesting.Action{
				clientgotesting.CreateActionImpl{
					ActionImpl: clientgotesting.ActionImpl{
						Verb: "create",
						Resource: schema.GroupVersionResource{
							Group:    "group",
							Version:  "version",
							Resource: "thekinds",
						},
					},
					CreateOptions: metav1.CreateOptions{
						DryRun: []string{metav1.DryRunAll},
					},
					Object: newUnstructuredWithNoNamespace("group/version", "TheKind", "name-foo"),
				},
			},
		},
		"subresource creation with namespace": {
			ns:           "ns-foo",
			subresources: []string{"status"},
			createOptions: metav1.CreateOptions{
				FieldManager: "test",
			},
			resource: newUnstructured("group/version", "TheKind", "ns-foo", "name-foo"),
			wantActions: []clientgotesting.Action{
				clientgotesting.CreateActionImpl{
					Name: "name-foo",
					ActionImpl: clientgotesting.ActionImpl{
						Namespace:   "ns-foo",
						Verb:        "create",
						Subresource: "status",
						Resource: schema.GroupVersionResource{
							Group:    "group",
							Version:  "version",
							Resource: "thekinds",
						},
					},
					CreateOptions: metav1.CreateOptions{
						FieldManager: "test",
					},
					Object: newUnstructured("group/version", "TheKind", "ns-foo", "name-foo"),
				},
			},
		},
		"subresource creation with no namespace": {
			subresources: []string{"status"},
			createOptions: metav1.CreateOptions{
				FieldManager: "test",
			},
			resource: newUnstructuredWithNoNamespace("group/version", "TheKind", "name-foo"),
			wantActions: []clientgotesting.Action{
				clientgotesting.CreateActionImpl{
					Name: "name-foo",
					ActionImpl: clientgotesting.ActionImpl{
						Verb:        "create",
						Subresource: "status",
						Resource: schema.GroupVersionResource{
							Group:    "group",
							Version:  "version",
							Resource: "thekinds",
						},
					},
					CreateOptions: metav1.CreateOptions{
						FieldManager: "test",
					},
					Object: newUnstructuredWithNoNamespace("group/version", "TheKind", "name-foo"),
				},
			},
		},
	}

	for name, tt := range createTests {
		t.Run(name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			var client *FakeDynamicClient
			if len(tt.subresources) > 0 {
				// Resource must exist before creating a subresource
				client = NewSimpleDynamicClient(scheme, tt.resource)
			} else {
				client = NewSimpleDynamicClient(scheme)
			}

			_, err := client.Resource(schema.GroupVersionResource{Group: "group", Version: "version", Resource: "thekinds"}).
				Namespace(tt.ns).Create(context.TODO(), tt.resource, tt.createOptions, tt.subresources...)
			if err != nil {
				t.Fatal(err)
			}

			actions := client.Fake.Actions()
			if diff := cmp.Diff(tt.wantActions, actions); diff != "" {
				t.Error("unexpected diff (-want, +got): ", diff)
			}
			get, err := client.Resource(schema.GroupVersionResource{Group: "group", Version: "version", Resource: "thekinds"}).
				Namespace(tt.ns).Get(context.TODO(), "name-foo", metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(tt.resource, get); diff != "" {
				t.Error("unexpected diff (-want, +got): ", diff)
			}
		})
	}
}

func TestUpdate(t *testing.T) {
	updateTests := map[string]struct {
		ns            string
		resource      *unstructured.Unstructured
		updateOptions metav1.UpdateOptions
		subresources  []string
		wantActions   []clientgotesting.Action
	}{
		"resource update with namespace": {
			resource: newUnstructured("group/version", "TheKind", "ns-foo", "name-foo"),
			ns:       "ns-foo",
			updateOptions: metav1.UpdateOptions{
				DryRun: []string{metav1.DryRunAll},
			},
			wantActions: []clientgotesting.Action{
				clientgotesting.UpdateActionImpl{
					ActionImpl: clientgotesting.ActionImpl{
						Namespace: "ns-foo",
						Verb:      "update",
						Resource: schema.GroupVersionResource{
							Group:    "group",
							Version:  "version",
							Resource: "thekinds",
						},
					},
					UpdateOptions: metav1.UpdateOptions{
						DryRun: []string{metav1.DryRunAll},
					},
					Object: newUnstructured("group/version", "TheKind", "ns-foo", "name-foo"),
				},
			},
		},
		"resource creation with no namespace": {
			resource: newUnstructuredWithNoNamespace("group/version", "TheKind", "name-foo"),
			updateOptions: metav1.UpdateOptions{
				DryRun: []string{metav1.DryRunAll},
			},
			wantActions: []clientgotesting.Action{
				clientgotesting.UpdateActionImpl{
					ActionImpl: clientgotesting.ActionImpl{
						Verb: "update",
						Resource: schema.GroupVersionResource{
							Group:    "group",
							Version:  "version",
							Resource: "thekinds",
						},
					},
					UpdateOptions: metav1.UpdateOptions{
						DryRun: []string{metav1.DryRunAll},
					},
					Object: newUnstructuredWithNoNamespace("group/version", "TheKind", "name-foo"),
				},
			},
		},
		"subresource update with namespace": {
			ns:           "ns-foo",
			subresources: []string{"status"},
			updateOptions: metav1.UpdateOptions{
				FieldManager: "test",
			},
			resource: newUnstructured("group/version", "TheKind", "ns-foo", "name-foo"),
			wantActions: []clientgotesting.Action{
				clientgotesting.UpdateActionImpl{
					ActionImpl: clientgotesting.ActionImpl{
						Namespace:   "ns-foo",
						Verb:        "update",
						Subresource: "status",
						Resource: schema.GroupVersionResource{
							Group:    "group",
							Version:  "version",
							Resource: "thekinds",
						},
					},
					UpdateOptions: metav1.UpdateOptions{
						FieldManager: "test",
					},
					Object: newUnstructured("group/version", "TheKind", "ns-foo", "name-foo"),
				},
			},
		},
		"subresource creation with no namespace": {
			subresources: []string{"status"},
			updateOptions: metav1.UpdateOptions{
				FieldManager: "test",
			},
			resource: newUnstructuredWithNoNamespace("group/version", "TheKind", "name-foo"),
			wantActions: []clientgotesting.Action{
				clientgotesting.UpdateActionImpl{
					ActionImpl: clientgotesting.ActionImpl{
						Verb:        "update",
						Subresource: "status",
						Resource: schema.GroupVersionResource{
							Group:    "group",
							Version:  "version",
							Resource: "thekinds",
						},
					},
					UpdateOptions: metav1.UpdateOptions{
						FieldManager: "test",
					},
					Object: newUnstructuredWithNoNamespace("group/version", "TheKind", "name-foo"),
				},
			},
		},
	}

	for name, tt := range updateTests {
		t.Run(name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			client := NewSimpleDynamicClient(scheme, tt.resource)

			_, err := client.Resource(schema.GroupVersionResource{Group: "group", Version: "version", Resource: "thekinds"}).
				Namespace(tt.ns).Update(context.TODO(), tt.resource, tt.updateOptions, tt.subresources...)
			if err != nil {
				t.Fatal(err)
			}

			actions := client.Fake.Actions()
			if diff := cmp.Diff(tt.wantActions, actions); diff != "" {
				t.Error("unexpected diff (-want, +got): ", diff)
			}
			get, err := client.Resource(schema.GroupVersionResource{Group: "group", Version: "version", Resource: "thekinds"}).
				Namespace(tt.ns).Get(context.TODO(), "name-foo", metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(tt.resource, get); diff != "" {
				t.Error("unexpected diff (-want, +got): ", diff)
			}
		})
	}
}

type (
	mockResource struct {
		metav1.TypeMeta   `json:",inline"`
		metav1.ObjectMeta `json:"metadata"`
	}
	mockResourceList struct {
		metav1.TypeMeta `json:",inline"`
		metav1.ListMeta `json:"metadata"`

		Items []mockResource
	}
)

func (l *mockResourceList) DeepCopyObject() runtime.Object {
	o := *l
	return &o
}

func (r *mockResource) DeepCopyObject() runtime.Object {
	o := *r
	return &o
}

var _ runtime.Object = (*mockResource)(nil)
var _ runtime.Object = (*mockResourceList)(nil)
