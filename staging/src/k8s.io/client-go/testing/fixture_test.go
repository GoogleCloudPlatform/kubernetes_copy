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

package testing

import (
	"fmt"
	"math/rand"
	"strconv"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"sigs.k8s.io/structured-merge-diff/v4/typed"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/json"
	"k8s.io/apimachinery/pkg/util/managedfields"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/utils/ptr"
)

func getArbitraryResource(s schema.GroupVersionResource, name, namespace string) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"kind":       s.Resource,
			"apiVersion": s.Version,
			"metadata": map[string]interface{}{
				"name":            name,
				"namespace":       namespace,
				"generateName":    "test_generateName",
				"uid":             "test_uid",
				"resourceVersion": "test_resourceVersion",
			},
			"data": strconv.Itoa(rand.Int()),
		},
	}
}

func TestWatchCallNonNamespace(t *testing.T) {
	testResource := schema.GroupVersionResource{Group: "", Version: "test_version", Resource: "test_kind"}
	testObj := getArbitraryResource(testResource, "test_name", "test_namespace")
	accessor, err := meta.Accessor(testObj)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	ns := accessor.GetNamespace()
	scheme := runtime.NewScheme()
	codecs := serializer.NewCodecFactory(scheme)
	o := NewObjectTracker(scheme, codecs.UniversalDecoder())
	watch, err := o.Watch(testResource, ns)
	if err != nil {
		t.Fatalf("test resource watch failed in %s: %v ", ns, err)
	}
	go func() {
		err := o.Create(testResource, testObj, ns)
		if err != nil {
			t.Errorf("test resource creation failed: %v", err)
		}
	}()
	out := <-watch.ResultChan()
	assert.Equal(t, testObj, out.Object, "watched object mismatch")
}

func TestWatchCallAllNamespace(t *testing.T) {
	testResource := schema.GroupVersionResource{Group: "", Version: "test_version", Resource: "test_kind"}
	testObj := getArbitraryResource(testResource, "test_name", "test_namespace")
	accessor, err := meta.Accessor(testObj)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	ns := accessor.GetNamespace()
	scheme := runtime.NewScheme()
	codecs := serializer.NewCodecFactory(scheme)
	o := NewObjectTracker(scheme, codecs.UniversalDecoder())
	w, err := o.Watch(testResource, "test_namespace")
	if err != nil {
		t.Fatalf("test resource watch failed in test_namespace: %v", err)
	}
	wAll, err := o.Watch(testResource, "")
	if err != nil {
		t.Fatalf("test resource watch failed in all namespaces: %v", err)
	}
	go func() {
		err := o.Create(testResource, testObj, ns)
		assert.NoError(t, err, "test resource creation failed")
	}()
	out := <-w.ResultChan()
	outAll := <-wAll.ResultChan()
	assert.Equal(t, watch.Added, out.Type, "watch event mismatch")
	assert.Equal(t, watch.Added, outAll.Type, "watch event mismatch")
	assert.Equal(t, testObj, out.Object, "watched created object mismatch")
	assert.Equal(t, testObj, outAll.Object, "watched created object mismatch")
	go func() {
		err := o.Update(testResource, testObj, ns)
		assert.NoError(t, err, "test resource updating failed")
	}()
	out = <-w.ResultChan()
	outAll = <-wAll.ResultChan()
	assert.Equal(t, watch.Modified, out.Type, "watch event mismatch")
	assert.Equal(t, watch.Modified, outAll.Type, "watch event mismatch")
	assert.Equal(t, testObj, out.Object, "watched updated object mismatch")
	assert.Equal(t, testObj, outAll.Object, "watched updated object mismatch")
	go func() {
		err := o.Delete(testResource, "test_namespace", "test_name")
		assert.NoError(t, err, "test resource deletion failed")
	}()
	out = <-w.ResultChan()
	outAll = <-wAll.ResultChan()
	assert.Equal(t, watch.Deleted, out.Type, "watch event mismatch")
	assert.Equal(t, watch.Deleted, outAll.Type, "watch event mismatch")
	assert.Equal(t, testObj, out.Object, "watched deleted object mismatch")
	assert.Equal(t, testObj, outAll.Object, "watched deleted object mismatch")
}

func TestWatchCallMultipleInvocation(t *testing.T) {
	cases := []struct {
		name string
		op   watch.EventType
		ns   string
	}{
		{
			"foo",
			watch.Added,
			"test_namespace",
		},
		{
			"bar",
			watch.Added,
			"test_namespace",
		},
		{
			"baz",
			watch.Added,
			"",
		},
		{
			"bar",
			watch.Modified,
			"test_namespace",
		},
		{
			"baz",
			watch.Modified,
			"",
		},
		{
			"foo",
			watch.Deleted,
			"test_namespace",
		},
		{
			"bar",
			watch.Deleted,
			"test_namespace",
		},
		{
			"baz",
			watch.Deleted,
			"",
		},
	}

	scheme := runtime.NewScheme()
	codecs := serializer.NewCodecFactory(scheme)
	testResource := schema.GroupVersionResource{Group: "", Version: "test_version", Resource: "test_kind"}

	o := NewObjectTracker(scheme, codecs.UniversalDecoder())
	watchNamespaces := []string{
		"",
		"",
		"test_namespace",
		"test_namespace",
	}
	var wg sync.WaitGroup
	wg.Add(len(watchNamespaces))
	for idx, watchNamespace := range watchNamespaces {
		i := idx
		watchNamespace := watchNamespace
		w, err := o.Watch(testResource, watchNamespace)
		if err != nil {
			t.Fatalf("test resource watch failed in %s: %v", watchNamespace, err)
		}
		go func() {
			assert.NoError(t, err, "watch invocation failed")
			for _, c := range cases {
				if watchNamespace == "" || c.ns == watchNamespace {
					fmt.Printf("%#v %#v\n", c, i)
					event := <-w.ResultChan()
					accessor, err := meta.Accessor(event.Object)
					if err != nil {
						t.Errorf("unexpected error: %v", err)
						break
					}
					assert.Equal(t, c.op, event.Type, "watch event mismatched")
					assert.Equal(t, c.name, accessor.GetName(), "watched object mismatch")
					assert.Equal(t, c.ns, accessor.GetNamespace(), "watched object mismatch")
				}
			}
			wg.Done()
		}()
	}
	for _, c := range cases {
		switch c.op {
		case watch.Added:
			obj := getArbitraryResource(testResource, c.name, c.ns)
			o.Create(testResource, obj, c.ns)
		case watch.Modified:
			obj := getArbitraryResource(testResource, c.name, c.ns)
			o.Update(testResource, obj, c.ns)
		case watch.Deleted:
			o.Delete(testResource, c.ns, c.name)
		}
	}
	wg.Wait()
}

func TestWatchAddAfterStop(t *testing.T) {
	testResource := schema.GroupVersionResource{Group: "", Version: "test_version", Resource: "test_kind"}
	testObj := getArbitraryResource(testResource, "test_name", "test_namespace")
	accessor, err := meta.Accessor(testObj)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	ns := accessor.GetNamespace()
	scheme := runtime.NewScheme()
	codecs := serializer.NewCodecFactory(scheme)
	o := NewObjectTracker(scheme, codecs.UniversalDecoder())
	watch, err := o.Watch(testResource, ns)
	if err != nil {
		t.Errorf("watch creation failed: %v", err)
	}

	// When the watch is stopped it should ignore later events without panicking.
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("Watch panicked when it should have ignored create after stop: %v", r)
		}
	}()

	watch.Stop()
	err = o.Create(testResource, testObj, ns)
	if err != nil {
		t.Errorf("test resource creation failed: %v", err)
	}
}

func TestPatchWithMissingObject(t *testing.T) {

		nodesResource := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "nodes"}

		scheme := runtime.NewScheme()
		codecs := serializer.NewCodecFactory(scheme)
		o := NewObjectTracker(scheme, codecs.UniversalDecoder())
		reaction := ObjectReaction(o)
		action := NewRootPatchSubresourceAction(nodesResource, "node-1", types.StrategicMergePatchType, []byte(`{}`))
		handled, node, err := reaction(action)
		assert.True(t, handled)
		assert.Nil(t, node)
		assert.EqualError(t, err, `nodes "node-1" not found`)
}

func TestStrategicMergePatchWithPropertyPatching(t *testing.T) {
		configmapsResource := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "configmaps"}
		configmapsGvk := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}

		targetObject := &v1.ConfigMap{}

		scheme := runtime.NewScheme()
		scheme.AddKnownTypeWithName(configmapsGvk, targetObject)

		codecs := serializer.NewCodecFactory(scheme)

		targetMeta, err := strategicpatch.NewPatchMetaFromStruct(targetObject)
		assert.NoError(t, err)

		schemeWithPatchMeta := schemeWithPatchMeta{
			Scheme: scheme,
			patchMeta: map[schema.GroupVersionKind]strategicpatch.LookupPatchMeta{
				configmapsGvk: targetMeta,
			},
		}

		o := NewObjectTracker(&schemeWithPatchMeta, codecs.UniversalDecoder())

		unstructuredObj := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"kind":       "ConfigMap",
				"apiVersion": "v1",
				"metadata": map[string]interface{}{
					"name":      "cm-1",
					"namespace": "cm-namespace",
				},
				"data": map[string]interface{}{"field-1": "A", "field-2": "B"},
			},
		}
		expected := unstructuredObj.DeepCopy()
		expected.Object["data"] = map[string]interface{}{"field-1": "C", "field-2": "B"}

		err = o.Add(unstructuredObj)
		assert.NoError(t, err)

		reaction := ObjectReaction(o)
		action := NewPatchAction(configmapsResource, "cm-namespace", "cm-1", types.StrategicMergePatchType, []byte(`{"data":{"field-1":"C"}}`))
		handled, changedCm, err := reaction(action)
		assert.NoError(t, err)
		assert.True(t, handled)
		assert.EqualValues(t, expected, changedCm)

		trackedObject, err := o.Get(configmapsResource, "cm-namespace", "cm-1")
		assert.NoError(t, err)
		assert.EqualValues(t, expected, trackedObject)
}

func TestApplyCreate(t *testing.T) {
	cmResource := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "configMaps"}
	scheme := runtime.NewScheme()
	scheme.AddKnownTypes(cmResource.GroupVersion(), &v1.ConfigMap{})
	codecs := serializer.NewCodecFactory(scheme)
	o := NewFieldManagedObjectTracker(scheme, codecs.UniversalDecoder(), configMapTypeConverter(scheme))

	reaction := ObjectReaction(o)
	patch := []byte(`{"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": "cm-1"}, "data": {"k": "v"}}`)
	action := NewPatchActionWithOptions(cmResource, "default", "cm-1", types.ApplyPatchType, patch,
		metav1.PatchOptions{FieldManager: "test-manager"})
	handled, configMap, err := reaction(action)
	assert.True(t, handled)
	if err != nil {
		t.Fatalf("Failed to create a resource with apply: %v", err)
	}
	cm := configMap.(*v1.ConfigMap)
	assert.Equal(t, map[string]string{"k": "v"}, cm.Data)
}

func TestApplyNoMeta(t *testing.T) {
	cmResource := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "configMaps"}
	scheme := runtime.NewScheme()
	scheme.AddKnownTypes(cmResource.GroupVersion(), &v1.ConfigMap{})
	codecs := serializer.NewCodecFactory(scheme)
	o := NewFieldManagedObjectTracker(scheme, codecs.UniversalDecoder(), configMapTypeConverter(scheme))

	reaction := ObjectReaction(o)
	patch := []byte(`{"apiVersion": "v1", "kind": "ConfigMap", "data": {"k": "v"}}`)
	action := NewPatchActionWithOptions(cmResource, "default", "cm-1", types.ApplyPatchType, patch,
		metav1.PatchOptions{FieldManager: "test-manager"})
	handled, configMap, err := reaction(action)
	assert.True(t, handled)
	if err != nil {
		t.Fatalf("Failed to create a resource with apply: %v", err)
	}
	cm := configMap.(*v1.ConfigMap)
	assert.Equal(t, "cm-1", cm.Name)
	assert.Equal(t, map[string]string{"k": "v"}, cm.Data)
}

func TestApplyUpdateMultipleFieldManagers(t *testing.T) {
	cmResource := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "configMaps"}
	scheme := runtime.NewScheme()
	scheme.AddKnownTypes(cmResource.GroupVersion(), &v1.ConfigMap{})
	codecs := serializer.NewCodecFactory(scheme)
	o := NewFieldManagedObjectTracker(scheme, codecs.UniversalDecoder(), configMapTypeConverter(scheme))

	reaction := ObjectReaction(o)
	action := NewCreateAction(cmResource, "default", &v1.ConfigMap{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "ConfigMap",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: "cm-1",
		},
		Data: map[string]string{
			"k0": "v0",
		},
	})
	handled, _, err := reaction(action)
	assert.True(t, handled)
	if err != nil {
		t.Errorf("Failed to create resource: %v", err)
	}

	// Apply with test-manager-1
	// Expect data to be shared with initial create
	patch := []byte(`{"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": "cm-1"}, "data": {"k1": "v1"}}`)
	applyAction := NewPatchActionWithOptions(cmResource, "default", "cm-1", types.ApplyPatchType, patch,
		metav1.PatchOptions{FieldManager: "test-manager-1"})
	handled, configMap, err := reaction(applyAction)
	assert.True(t, handled)
	if err != nil {
		t.Errorf("Failed to apply resource: %v", err)
	}
	cm := configMap.(*v1.ConfigMap)
	assert.Equal(t, map[string]string{"k0": "v0", "k1": "v1"}, cm.Data)

	// Apply conflicting with test-manager-2, expect apply to fail
	patch = []byte(`{"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": "cm-1"}, "data": {"k1": "xyz"}}`)
	applyAction = NewPatchActionWithOptions(cmResource, "default", "cm-1", types.ApplyPatchType, patch,
		metav1.PatchOptions{FieldManager: "test-manager-2"})
	handled, _, err = reaction(applyAction)
	assert.True(t, handled)
	if assert.Error(t, err) {
		assert.Equal(t, "Apply failed with 1 conflict: conflict with \"test-manager-1\": .data.k1", err.Error())
	}

	// Apply with test-manager-2
	// Expect data to be shared with initial create and test-manager-1
	patch = []byte(`{"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": "cm-1"}, "data": {"k2": "v2"}}`)
	applyAction = NewPatchActionWithOptions(cmResource, "default", "cm-1", types.ApplyPatchType, patch,
		metav1.PatchOptions{FieldManager: "test-manager-2"})
	handled, configMap, err = reaction(applyAction)
	assert.True(t, handled)
	if err != nil {
		t.Errorf("Failed to apply resource: %v", err)
	}
	cm = configMap.(*v1.ConfigMap)
	assert.Equal(t, map[string]string{"k0": "v0", "k1": "v1", "k2": "v2"}, cm.Data)

	// Apply with test-manager-1
	// Expect owned data to be updated
	patch = []byte(`{"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": "cm-1"}, "data": {"k1": "v101"}}`)
	applyAction = NewPatchActionWithOptions(cmResource, "default", "cm-1", types.ApplyPatchType, patch,
		metav1.PatchOptions{FieldManager: "test-manager-1"})
	handled, configMap, err = reaction(applyAction)
	assert.True(t, handled)
	if err != nil {
		t.Errorf("Failed to apply resource: %v", err)
	}
	cm = configMap.(*v1.ConfigMap)
	assert.Equal(t, map[string]string{"k0": "v0", "k1": "v101", "k2": "v2"}, cm.Data)

	// Force apply with test-manager-2
	// Expect data owned by test-manager-1 to be updated, expect data already owned but not in apply configuration to be removed
	patch = []byte(`{"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": "cm-1"}, "data": {"k1": "v202"}}`)
	applyAction = NewPatchActionWithOptions(cmResource, "default", "cm-1", types.ApplyPatchType, patch,
		metav1.PatchOptions{FieldManager: "test-manager-2", Force: ptr.To(true)})
	handled, configMap, err = reaction(applyAction)
	assert.True(t, handled)
	if err != nil {
		t.Errorf("Failed to apply resource: %v", err)
	}
	cm = configMap.(*v1.ConfigMap)
	assert.Equal(t, map[string]string{"k0": "v0", "k1": "v202"}, cm.Data)

	// Update with test-manager-1 to perform a force update of the entire resource
	reaction = ObjectReaction(o)
	updateAction := NewUpdateActionWithOptions(cmResource, "default", &v1.ConfigMap{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "ConfigMap",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: "cm-1",
		},
		Data: map[string]string{
			"k99": "v99",
		},
	}, metav1.UpdateOptions{FieldManager: "test-manager-1"})
	handled, configMap, err = reaction(updateAction)
	assert.True(t, handled)
	if err != nil {
		t.Errorf("Failed to apply resource: %v", err)
	}
	typedCm := configMap.(*v1.ConfigMap)
	assert.Equal(t, map[string]string{"k99": "v99"}, typedCm.Data)
}

func TestGetWithExactMatch(t *testing.T) {
	scheme := runtime.NewScheme()
	codecs := serializer.NewCodecFactory(scheme)

	constructObject := func(s schema.GroupVersionResource, name, namespace string) (*unstructured.Unstructured, schema.GroupVersionResource) {
		obj := getArbitraryResource(s, name, namespace)
		gvks, _, err := scheme.ObjectKinds(obj)
		assert.NoError(t, err)
		gvr, _ := meta.UnsafeGuessKindToResource(gvks[0])
		return obj, gvr
	}

	var err error
	// Object with empty namespace
	o := NewObjectTracker(scheme, codecs.UniversalDecoder())
	nodeResource := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "node"}
	node, gvr := constructObject(nodeResource, "node", "")

	assert.NoError(t, o.Add(node))

	// Exact match
	_, err = o.Get(gvr, "", "node")
	assert.NoError(t, err)

	// Unexpected namespace provided
	_, err = o.Get(gvr, "ns", "node")
	assert.Error(t, err)
	errNotFound := errors.NewNotFound(gvr.GroupResource(), "node")
	assert.EqualError(t, err, errNotFound.Error())

	// Object with non-empty namespace
	o = NewObjectTracker(scheme, codecs.UniversalDecoder())
	podResource := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "pod"}
	pod, gvr := constructObject(podResource, "pod", "default")
	assert.NoError(t, o.Add(pod))

	// Exact match
	_, err = o.Get(gvr, "default", "pod")
	assert.NoError(t, err)

	// Missing namespace
	_, err = o.Get(gvr, "", "pod")
	assert.Error(t, err)
	errNotFound = errors.NewNotFound(gvr.GroupResource(), "pod")
	assert.EqualError(t, err, errNotFound.Error())
}

func Test_resourceCovers(t *testing.T) {
	type args struct {
		resource string
		action   Action
	}
	tests := []struct {
		name string
		args args
		want bool
	}{
		{
			args: args{
				resource: "*",
				action:   ActionImpl{},
			},
			want: true,
		},
		{
			args: args{
				resource: "serviceaccounts",
				action:   ActionImpl{},
			},
			want: false,
		},
		{
			args: args{
				resource: "serviceaccounts",
				action: ActionImpl{
					Resource: schema.GroupVersionResource{
						Resource: "serviceaccounts",
					},
				},
			},
			want: true,
		},
		{
			args: args{
				resource: "serviceaccounts/token",
				action: ActionImpl{
					Resource: schema.GroupVersionResource{},
				},
			},
			want: false,
		},
		{
			args: args{
				resource: "serviceaccounts/token",
				action: ActionImpl{
					Resource: schema.GroupVersionResource{
						Resource: "serviceaccounts",
					},
				},
			},
			want: false,
		},
		{
			args: args{
				resource: "serviceaccounts/token",
				action: ActionImpl{
					Resource:    schema.GroupVersionResource{},
					Subresource: "token",
				},
			},
			want: false,
		},
		{
			args: args{
				resource: "serviceaccounts/token",
				action: ActionImpl{
					Resource: schema.GroupVersionResource{
						Resource: "serviceaccounts",
					},
					Subresource: "token",
				},
			},
			want: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resourceCovers(tt.args.resource, tt.args.action); got != tt.want {
				t.Errorf("resourceCovers() = %v, want %v", got, tt.want)
			}
		})
	}
}

func configMapTypeConverter(scheme *runtime.Scheme) managedfields.TypeConverter {
	parser, err := typed.NewParser(configMapTypedSchema)
	if err != nil {
		panic(fmt.Sprintf("Failed to parse schema: %v", err))
	}

	return TypeConverter{Scheme: scheme, TypeResolver: parser}
}

var configMapTypedSchema = typed.YAMLObject(`types:
- name: io.k8s.api.core.v1.ConfigMap
  map:
    fields:
    - name: apiVersion
      type:
        scalar: string
    - name: data
      type:
        map:
          elementType:
            scalar: string
    - name: kind
      type:
        scalar: string
    - name: metadata
      type:
        namedType: io.k8s.apimachinery.pkg.apis.meta.v1.ObjectMeta
      default: {}
- name: io.k8s.apimachinery.pkg.apis.meta.v1.ObjectMeta
  map:
    fields:
    - name: creationTimestamp
      type:
        namedType: io.k8s.apimachinery.pkg.apis.meta.v1.Time
    - name: managedFields
      type:
        list:
          elementType:
            namedType: io.k8s.apimachinery.pkg.apis.meta.v1.ManagedFieldsEntry
          elementRelationship: atomic
    - name: name
      type:
        scalar: string
    - name: namespace
      type:
        scalar: string
- name: io.k8s.apimachinery.pkg.apis.meta.v1.ManagedFieldsEntry
  map:
    fields:
    - name: apiVersion
      type:
        scalar: string
    - name: fieldsType
      type:
        scalar: string
    - name: fieldsV1
      type:
        namedType: io.k8s.apimachinery.pkg.apis.meta.v1.FieldsV1
    - name: manager
      type:
        scalar: string
    - name: operation
      type:
        scalar: string
    - name: subresource
      type:
        scalar: string
    - name: time
      type:
        namedType: io.k8s.apimachinery.pkg.apis.meta.v1.Time
- name: io.k8s.apimachinery.pkg.apis.meta.v1.FieldsV1
  map:
    elementType:
      scalar: untyped
      list:
        elementType:
          namedType: __untyped_atomic_
        elementRelationship: atomic
      map:
        elementType:
          namedType: __untyped_deduced_
        elementRelationship: separable
- name: io.k8s.apimachinery.pkg.apis.meta.v1.Time
  scalar: untyped
- name: __untyped_deduced_
  scalar: untyped
  list:
    elementType:
      namedType: __untyped_atomic_
    elementRelationship: atomic
  map:
    elementType:
      namedType: __untyped_deduced_
    elementRelationship: separable
`)

func TestStrategicMergePatch(t *testing.T) {
	//StrategicMergePatch is not supposed to be called with an *unstructured.Unstructured as reference for the underlying schema
	//This is for sanity only; strategicpatch.StrategicMergePatch will be used to track the elements inside the fake object tracker

	//local entries in dynamic fake client will always be of type *unstructured.Unstructured (see: dynamic/fake/simple.go:52 "objects, err := convertObjectsToUnstructured(scheme, objects)")
	unstructuredObj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"kind":       "ConfigMap",
			"apiVersion": "v1",
			"metadata": map[string]interface{}{
				"name":      "some-config-map",
				"namespace": "some-namespace",
			},
			"data": map[string]interface{}{
				"changed-prop": "getsChanged",
				"static-prop":  "doesNotGetChanges",
			},
		},
	}

	//the expected object shall have a modified 'data' property
	expectedObj := unstructuredObj.DeepCopy()
	expectedObj.Object["data"] = map[string]interface{}{"changed-prop": "newValue", "static-prop": "doesNotGetChanges"}
	expectedObjBytes, err := json.Marshal(expectedObj)
	assert.NoError(t, err)

	unstructuredObjBytes, err := json.Marshal(unstructuredObj)
	assert.NoError(t, err)

	t.Run("using unstructured.Unstructured", func(t *testing.T) {
		outObj, err := strategicpatch.StrategicMergePatch(unstructuredObjBytes, []byte(`{"data":{"changed-prop": "newValue"}}`), unstructuredObj)
		assert.EqualError(t, err, `unable to find api field in struct Unstructured for the json field "data"`)
		assert.Nil(t, outObj)
	})

	t.Run("using structured object ConfigMap", func(t *testing.T) {
		outObj, err := strategicpatch.StrategicMergePatch(unstructuredObjBytes, []byte(`{"data":{"changed-prop": "newValue"}}`), &v1.ConfigMap{})
		assert.NoError(t, err)
		assert.Equal(t, expectedObjBytes, outObj, "object mismatch")
	})
}
