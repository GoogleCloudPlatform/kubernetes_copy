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

package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	fuzz "github.com/google/gofuzz"

	"k8s.io/apimachinery/pkg/api/apitesting"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metainternalversion "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/selection"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apimachinery/pkg/util/version"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apiserver/pkg/apis/example"
	examplev1 "k8s.io/apiserver/pkg/apis/example/v1"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/features"
	"k8s.io/apiserver/pkg/registry/generic"
	"k8s.io/apiserver/pkg/registry/rest"
	"k8s.io/apiserver/pkg/storage"
	cacherstorage "k8s.io/apiserver/pkg/storage/cacher"
	etcd3testing "k8s.io/apiserver/pkg/storage/etcd3/testing"
	"k8s.io/apiserver/pkg/storage/names"
	"k8s.io/apiserver/pkg/storage/storagebackend/factory"
	storagetesting "k8s.io/apiserver/pkg/storage/testing"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/tools/cache"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
)

var scheme = runtime.NewScheme()
var codecs = serializer.NewCodecFactory(scheme)

func init() {
	metav1.AddToGroupVersion(scheme, metav1.SchemeGroupVersion)
	utilruntime.Must(example.AddToScheme(scheme))
	utilruntime.Must(examplev1.AddToScheme(scheme))
}

type testGracefulStrategy struct {
	testRESTStrategy
}

func (t testGracefulStrategy) CheckGracefulDelete(ctx context.Context, obj runtime.Object, options *metav1.DeleteOptions) bool {
	return true
}

var _ rest.RESTGracefulDeleteStrategy = testGracefulStrategy{}

type testOrphanDeleteStrategy struct {
	*testRESTStrategy
}

func (t *testOrphanDeleteStrategy) DefaultGarbageCollectionPolicy(ctx context.Context) rest.GarbageCollectionPolicy {
	return rest.OrphanDependents
}

type mutatingDeleteRESTStrategy struct {
	runtime.ObjectTyper
}

func (t *mutatingDeleteRESTStrategy) CheckGracefulDelete(ctx context.Context, obj runtime.Object, options *metav1.DeleteOptions) bool {
	n := int64(10)
	options.GracePeriodSeconds = &n
	return true
}

type testRESTStrategy struct {
	runtime.ObjectTyper
	names.NameGenerator
	namespaceScoped          bool
	allowCreateOnUpdate      bool
	allowUnconditionalUpdate bool
}

func (t *testRESTStrategy) NamespaceScoped() bool          { return t.namespaceScoped }
func (t *testRESTStrategy) AllowCreateOnUpdate() bool      { return t.allowCreateOnUpdate }
func (t *testRESTStrategy) AllowUnconditionalUpdate() bool { return t.allowUnconditionalUpdate }

func (t *testRESTStrategy) PrepareForCreate(ctx context.Context, obj runtime.Object) {
	metaObj, err := meta.Accessor(obj)
	if err != nil {
		panic(err.Error())
	}
	labels := metaObj.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	labels["prepare_create"] = "true"
	metaObj.SetLabels(labels)
}

func (t *testRESTStrategy) PrepareForUpdate(ctx context.Context, obj, old runtime.Object) {}
func (t *testRESTStrategy) Validate(ctx context.Context, obj runtime.Object) field.ErrorList {
	return nil
}
func (t *testRESTStrategy) WarningsOnCreate(ctx context.Context, obj runtime.Object) []string {
	return nil
}
func (t *testRESTStrategy) ValidateUpdate(ctx context.Context, obj, old runtime.Object) field.ErrorList {
	return nil
}
func (t *testRESTStrategy) WarningsOnUpdate(ctx context.Context, obj, old runtime.Object) []string {
	return nil
}
func (t *testRESTStrategy) Canonicalize(obj runtime.Object) {}

func NewTestGenericStoreRegistry(t *testing.T) (factory.DestroyFunc, *Store) {
	return newTestGenericStoreRegistry(t, scheme, false)
}

func getPodAttrs(obj runtime.Object) (labels.Set, fields.Set, error) {
	pod := obj.(*example.Pod)
	return labels.Set{"name": pod.ObjectMeta.Name}, nil, nil
}

// matchPodName returns selection predicate that matches any pod with name in the set.
// Makes testing simpler.
func matchPodName(names ...string) storage.SelectionPredicate {
	// Note: even if pod name is a field, we have to use labels,
	// because field selector doesn't support "IN" operator.
	l, err := labels.NewRequirement("name", selection.In, names)
	if err != nil {
		panic("Labels requirement must validate successfully")
	}
	return storage.SelectionPredicate{
		Label:    labels.Everything().Add(*l),
		Field:    fields.Everything(),
		GetAttrs: getPodAttrs,
	}
}

func matchEverything() storage.SelectionPredicate {
	return storage.SelectionPredicate{
		Label: labels.Everything(),
		Field: fields.Everything(),
		GetAttrs: func(obj runtime.Object) (label labels.Set, field fields.Set, err error) {
			return nil, nil, nil
		},
	}
}

func TestStoreList(t *testing.T) {
	podA := &example.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "bar"},
		Spec:       example.PodSpec{NodeName: "machine"},
	}
	podB := &example.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "foo"},
		Spec:       example.PodSpec{NodeName: "machine"},
	}

	testContext := genericapirequest.WithNamespace(genericapirequest.NewContext(), "test")
	noNamespaceContext := genericapirequest.NewContext()

	table := map[string]struct {
		in      *example.PodList
		m       storage.SelectionPredicate
		out     runtime.Object
		context context.Context
	}{
		"notFound": {
			in:  nil,
			m:   matchEverything(),
			out: &example.PodList{Items: []example.Pod{}},
		},
		"normal": {
			in:  &example.PodList{Items: []example.Pod{*podA, *podB}},
			m:   matchEverything(),
			out: &example.PodList{Items: []example.Pod{*podA, *podB}},
		},
		"normalFiltered": {
			in:  &example.PodList{Items: []example.Pod{*podA, *podB}},
			m:   matchPodName("foo"),
			out: &example.PodList{Items: []example.Pod{*podB}},
		},
		"normalFilteredNoNamespace": {
			in:      &example.PodList{Items: []example.Pod{*podA, *podB}},
			m:       matchPodName("foo"),
			out:     &example.PodList{Items: []example.Pod{*podB}},
			context: noNamespaceContext,
		},
		"normalFilteredMatchMultiple": {
			in:  &example.PodList{Items: []example.Pod{*podA, *podB}},
			m:   matchPodName("foo", "makeMatchSingleReturnFalse"),
			out: &example.PodList{Items: []example.Pod{*podB}},
		},
	}

	for name, item := range table {
		t.Run(name, func(t *testing.T) {
			ctx := testContext
			if item.context != nil {
				ctx = item.context
			}
			destroyFunc, registry := NewTestGenericStoreRegistry(t)
			defer destroyFunc()

			if item.in != nil {
				if err := storagetesting.CreateList("/pods", registry.Storage.Storage, item.in); err != nil {
					t.Fatalf("Unexpected error %v", err)
				}
			}

			list, err := registry.ListPredicate(ctx, item.m, nil)
			if err != nil {
				t.Fatalf("Unexpected error %v", err)
			}

			// DeepDerivative e,a is needed here b/c the storage layer sets ResourceVersion
			if e, a := item.out, list; !apiequality.Semantic.DeepDerivative(e, a) {
				t.Fatalf("%v: Expected %#v, got %#v", name, e, a)
			}
		})
	}
}

// TestStoreListResourceVersion tests that if List with ResourceVersion > 0, it will wait until
// the results are as fresh as given version.
func TestStoreListResourceVersion(t *testing.T) {
	fooPod := &example.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "foo"},
		Spec:       example.PodSpec{NodeName: "machine"},
	}
	barPod := &example.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "bar"},
		Spec:       example.PodSpec{NodeName: "machine"},
	}
	ctx := genericapirequest.WithNamespace(genericapirequest.NewContext(), "test")

	destroyFunc, registry := newTestGenericStoreRegistry(t, scheme, true)
	defer destroyFunc()

	obj, err := registry.Create(ctx, fooPod, rest.ValidateAllObjectFunc, &metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}

	versioner := storage.APIObjectVersioner{}
	rev, err := versioner.ObjectResourceVersion(obj)
	if err != nil {
		t.Fatal(err)
	}

	waitListCh := make(chan runtime.Object, 1)
	go func(listRev uint64) {
		option := &metainternalversion.ListOptions{ResourceVersion: strconv.FormatUint(listRev, 10)}
		// It will wait until we create the second pod.
		l, err := registry.List(ctx, option)
		if err != nil {
			close(waitListCh)
			t.Error(err)
			return
		}
		waitListCh <- l
	}(rev + 1)

	select {
	case <-time.After(500 * time.Millisecond):
	case l := <-waitListCh:
		t.Fatalf("expected waiting, but get %#v", l)
	}

	if _, err := registry.Create(ctx, barPod, rest.ValidateAllObjectFunc, &metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	select {
	case <-time.After(wait.ForeverTestTimeout):
		t.Fatalf("timeout after %v", wait.ForeverTestTimeout)
	case l, ok := <-waitListCh:
		if !ok {
			return
		}
		pl := l.(*example.PodList).Items
		if len(pl) != 2 {
			t.Errorf("Expected get 2 items, but got %d", len(pl))
		}
	}
}

func TestStoreCreate(t *testing.T) {
	gracefulPeriod := int64(50)
	podA := &example.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "foo", Namespace: "test"},
		Spec:       example.PodSpec{NodeName: "machine"},
	}
	podB := &example.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "foo", Namespace: "test"},
		Spec:       example.PodSpec{NodeName: "machine2"},
	}

	testContext := genericapirequest.WithNamespace(genericapirequest.NewContext(), "test")
	destroyFunc, registry := NewTestGenericStoreRegistry(t)
	defer destroyFunc()
	// re-define delete strategy to have graceful delete capability
	defaultDeleteStrategy := testRESTStrategy{scheme, names.SimpleNameGenerator, true, false, true}
	registry.DeleteStrategy = testGracefulStrategy{defaultDeleteStrategy}

	// create the object with denying admission
	_, err := registry.Create(testContext, podA, denyCreateValidation, &metav1.CreateOptions{})
	if err == nil {
		t.Errorf("Expected admission error: %v", err)
	}

	// create the object
	objA, err := registry.Create(testContext, podA, rest.ValidateAllObjectFunc, &metav1.CreateOptions{})
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}

	// get the object
	checkobj, err := registry.Get(testContext, podA.Name, &metav1.GetOptions{})
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}

	// verify objects are equal
	if e, a := objA, checkobj; !reflect.DeepEqual(e, a) {
		t.Errorf("Expected %#v, got %#v", e, a)
	}

	// now try to create the second pod
	_, err = registry.Create(testContext, podB, rest.ValidateAllObjectFunc, &metav1.CreateOptions{})
	if !errors.IsAlreadyExists(err) {
		t.Errorf("Unexpected error: %v", err)
	}

	// verify graceful delete capability is defined
	_, ok := registry.DeleteStrategy.(rest.RESTGracefulDeleteStrategy)
	if !ok {
		t.Fatalf("No graceful capability set.")
	}

	// now delete pod with graceful period set
	delOpts := &metav1.DeleteOptions{GracePeriodSeconds: &gracefulPeriod}
	_, _, err = registry.Delete(testContext, podA.Name, rest.ValidateAllObjectFunc, delOpts)
	if err != nil {
		t.Fatalf("Failed to delete pod gracefully. Unexpected error: %v", err)
	}

	// try to create before graceful deletion period is over
	_, err = registry.Create(testContext, podA, rest.ValidateAllObjectFunc, &metav1.CreateOptions{})
	if err == nil || !errors.IsAlreadyExists(err) {
		t.Fatalf("Expected 'already exists' error from storage, but got %v", err)
	}

	// check the 'already exists' msg was edited
	msg := &err.(*errors.StatusError).ErrStatus.Message
	if !strings.Contains(*msg, "object is being deleted:") {
		t.Errorf("Unexpected error without the 'object is being deleted:' in message: %v", err)
	}
}

// sequentialNameGenerator generates names by appending a monotonically-increasing integer to the base.
type sequentialNameGenerator struct {
	seq int
}

func (m *sequentialNameGenerator) GenerateName(base string) string {
	generated := fmt.Sprintf("%s%d", base, m.seq)
	m.seq++
	return generated
}

func TestStoreCreateWithRetryNameGenerate(t *testing.T) {

	namedObj := func(id int) *example.Pod {
		return &example.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("prefix-%d", id), Namespace: "test"},
			Spec:       example.PodSpec{NodeName: "machine"},
		}
	}

	generateNameObj := &example.Pod{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "prefix-", Namespace: "test"},
		Spec:       example.PodSpec{NodeName: "machine"},
	}

	testContext := genericapirequest.WithNamespace(genericapirequest.NewContext(), "test")
	destroyFunc, registry := NewTestGenericStoreRegistry(t)
	defer destroyFunc()

	seqNameGenerator := &sequentialNameGenerator{}
	registry.CreateStrategy = &testRESTStrategy{scheme, seqNameGenerator, true, false, true}

	for i := 0; i < 7; i++ {
		_, err := registry.Create(testContext, namedObj(i), rest.ValidateAllObjectFunc, &metav1.CreateOptions{})
		if err != nil {
			t.Errorf("Unexpected error: %v", err)
		}
	}
	generated, err := registry.Create(testContext, generateNameObj, rest.ValidateAllObjectFunc, &metav1.CreateOptions{})
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}
	generatedMeta, err := meta.Accessor(generated)
	if err != nil {
		t.Fatal(err)
	}
	if generatedMeta.GetName() != "prefix-7" {
		t.Errorf("Expected prefix-7 but got %s", generatedMeta.GetName())
	}

	// Now that 8 generated names (0..7) are claimed, 8 name generation attempts will not be enough
	// and create should return an already exists error.
	seqNameGenerator.seq = 0
	_, err = registry.Create(testContext, generateNameObj, rest.ValidateAllObjectFunc, &metav1.CreateOptions{})
	if err == nil || !errors.IsAlreadyExists(err) {
		t.Error("Expected already exists error")
	}
}

func TestStoreCreateWithRetryNameGenerateFeatureDisabled(t *testing.T) {
	// Preserve testing of disabled RetryGenerateName feature gate since it can still be disabled when emulation version is set.
	featuregatetesting.SetFeatureGateEmulationVersionDuringTest(t, utilfeature.DefaultFeatureGate, version.MustParse("1.31"))
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.RetryGenerateName, false)
	namedObj := func(id int) *example.Pod {
		return &example.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("prefix-%d", id), Namespace: "test"},
			Spec:       example.PodSpec{NodeName: "machine"},
		}
	}

	generateNameObj := &example.Pod{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "prefix-", Namespace: "test"},
		Spec:       example.PodSpec{NodeName: "machine"},
	}

	testContext := genericapirequest.WithNamespace(genericapirequest.NewContext(), "test")
	destroyFunc, registry := NewTestGenericStoreRegistry(t)
	defer destroyFunc()

	registry.CreateStrategy = &testRESTStrategy{scheme, &sequentialNameGenerator{}, true, false, true}

	_, err := registry.Create(testContext, namedObj(0), rest.ValidateAllObjectFunc, &metav1.CreateOptions{})
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}
	_, err = registry.Create(testContext, generateNameObj, rest.ValidateAllObjectFunc, &metav1.CreateOptions{})
	if err == nil || !errors.IsAlreadyExists(err) {
		t.Error("Expected already exists error")
	}
}

func TestNewCreateOptionsFromUpdateOptions(t *testing.T) {
	f := fuzz.New().NilChance(0.0).NumElements(1, 1)

	// The goal here is to trigger when any changes are made to either
	// CreateOptions or UpdateOptions types, so we can update the converter.
	for i := 0; i < 20; i++ {
		in := &metav1.UpdateOptions{}
		f.Fuzz(in)
		in.TypeMeta.SetGroupVersionKind(metav1.SchemeGroupVersion.WithKind("CreateOptions"))

		out := newCreateOptionsFromUpdateOptions(in)

		// This sequence is intending to elide type information, but produce an
		// intermediate structure (map) that can be manually patched up to make
		// the comparison work as needed.

		// Convert both structs to maps of primitives.
		inBytes, err := json.Marshal(in)
		if err != nil {
			t.Fatalf("failed to json.Marshal(in): %v", err)
		}
		outBytes, err := json.Marshal(out)
		if err != nil {
			t.Fatalf("failed to json.Marshal(out): %v", err)
		}
		inMap := map[string]interface{}{}
		if err := json.Unmarshal(inBytes, &inMap); err != nil {
			t.Fatalf("failed to json.Unmarshal(in): %v", err)
		}
		outMap := map[string]interface{}{}
		if err := json.Unmarshal(outBytes, &outMap); err != nil {
			t.Fatalf("failed to json.Unmarshal(out): %v", err)
		}

		// Patch the maps to handle any expected differences before we compare
		// - none for now.

		// Compare the results.
		inBytes, err = json.Marshal(inMap)
		if err != nil {
			t.Fatalf("failed to json.Marshal(in): %v", err)
		}
		outBytes, err = json.Marshal(outMap)
		if err != nil {
			t.Fatalf("failed to json.Marshal(out): %v", err)
		}
		if i, o := string(inBytes), string(outBytes); i != o {
			t.Fatalf("output != input:\n  want: %s\n   got: %s", i, o)
		}
	}
}

func TestNewDeleteOptionsFromUpdateOptions(t *testing.T) {
	f := fuzz.New().NilChance(0.0).NumElements(1, 1)

	// The goal here is to trigger when any changes are made to either
	// DeleteOptions or UpdateOptions types, so we can update the converter.
	for i := 0; i < 20; i++ {
		in := &metav1.UpdateOptions{}
		f.Fuzz(in)
		in.TypeMeta.SetGroupVersionKind(metav1.SchemeGroupVersion.WithKind("DeleteOptions"))

		out := newDeleteOptionsFromUpdateOptions(in)

		// This sequence is intending to elide type information, but produce an
		// intermediate structure (map) that can be manually patched up to make
		// the comparison work as needed.

		// Convert both structs to maps of primitives.
		inBytes, err := json.Marshal(in)
		if err != nil {
			t.Fatalf("failed to json.Marshal(in): %v", err)
		}
		outBytes, err := json.Marshal(out)
		if err != nil {
			t.Fatalf("failed to json.Marshal(out): %v", err)
		}
		inMap := map[string]interface{}{}
		if err := json.Unmarshal(inBytes, &inMap); err != nil {
			t.Fatalf("failed to json.Unmarshal(in): %v", err)
		}
		outMap := map[string]interface{}{}
		if err := json.Unmarshal(outBytes, &outMap); err != nil {
			t.Fatalf("failed to json.Unmarshal(out): %v", err)
		}

		// Patch the maps to handle any expected differences before we compare.

		// DeleteOptions does not have these fields.
		delete(inMap, "fieldManager")
		delete(inMap, "fieldValidation")

		// UpdateOptions does not have these fields.
		delete(outMap, "gracePeriodSeconds")
		delete(outMap, "preconditions")
		delete(outMap, "orphanDependents")
		delete(outMap, "propagationPolicy")

		// Compare the results.
		inBytes, err = json.Marshal(inMap)
		if err != nil {
			t.Fatalf("failed to json.Marshal(in): %v", err)
		}
		outBytes, err = json.Marshal(outMap)
		if err != nil {
			t.Fatalf("failed to json.Marshal(out): %v", err)
		}
		if i, o := string(inBytes), string(outBytes); i != o {
			t.Fatalf("output != input:\n  want: %s\n   got: %s", i, o)
		}
	}
}

func TestStoreCreateHooks(t *testing.T) {
	// To track which hooks were called in what order.  Not all hooks can
	// mutate the object.
	var milestones []string

	setAnn := func(obj runtime.Object, key string) {
		pod := obj.(*example.Pod)
		if pod.Annotations == nil {
			pod.Annotations = make(map[string]string)
		}
		pod.Annotations[key] = "true"
	}
	mile := func(s string) {
		milestones = append(milestones, s)
	}

	testCases := []struct {
		name        string
		decorator   func(runtime.Object)
		beginCreate BeginCreateFunc
		afterCreate AfterCreateFunc
		// the TTLFunc is an easy hook to force a failure
		ttl              func(obj runtime.Object, existing uint64, update bool) (uint64, error)
		expectError      bool
		expectAnnotation string   // to test object mutations
		expectMilestones []string // to test sequence
	}{{
		name: "no hooks",
	}, {
		name: "Decorator mutation",
		decorator: func(obj runtime.Object) {
			setAnn(obj, "DecoratorWasCalled")
		},
		expectAnnotation: "DecoratorWasCalled",
	}, {
		name: "AfterCreate mutation",
		afterCreate: func(obj runtime.Object, opts *metav1.CreateOptions) {
			setAnn(obj, "AfterCreateWasCalled")
		},
		expectAnnotation: "AfterCreateWasCalled",
	}, {
		name: "BeginCreate mutation",
		beginCreate: func(_ context.Context, obj runtime.Object, _ *metav1.CreateOptions) (FinishFunc, error) {
			setAnn(obj, "BeginCreateWasCalled")
			return func(context.Context, bool) {}, nil
		},
		expectAnnotation: "BeginCreateWasCalled",
	}, {
		name: "success ordering",
		decorator: func(obj runtime.Object) {
			mile("Decorator")
		},
		afterCreate: func(obj runtime.Object, opts *metav1.CreateOptions) {
			mile("AfterCreate")
		},
		beginCreate: func(_ context.Context, obj runtime.Object, _ *metav1.CreateOptions) (FinishFunc, error) {
			mile("BeginCreate")
			return func(_ context.Context, success bool) {
				mile(fmt.Sprintf("FinishCreate(%v)", success))
			}, nil
		},
		expectMilestones: []string{"BeginCreate", "FinishCreate(true)", "AfterCreate", "Decorator"},
	}, {
		name: "fail ordering",
		decorator: func(obj runtime.Object) {
			mile("Decorator")
		},
		afterCreate: func(obj runtime.Object, opts *metav1.CreateOptions) {
			mile("AfterCreate")
		},
		beginCreate: func(_ context.Context, obj runtime.Object, _ *metav1.CreateOptions) (FinishFunc, error) {
			mile("BeginCreate")
			return func(_ context.Context, success bool) {
				mile(fmt.Sprintf("FinishCreate(%v)", success))
			}, nil
		},
		ttl: func(_ runtime.Object, existing uint64, _ bool) (uint64, error) {
			mile("TTLError")
			return existing, fmt.Errorf("TTL fail")
		},
		expectMilestones: []string{"BeginCreate", "TTLError", "FinishCreate(false)"},
		expectError:      true,
	}, {
		name:        "fail BeginCreate ordering",
		expectError: true,
		decorator: func(obj runtime.Object) {
			mile("Decorator")
		},
		afterCreate: func(obj runtime.Object, opts *metav1.CreateOptions) {
			mile("AfterCreate")
		},
		beginCreate: func(_ context.Context, obj runtime.Object, _ *metav1.CreateOptions) (FinishFunc, error) {
			mile("BeginCreate")
			return func(_ context.Context, success bool) {
				mile(fmt.Sprintf("FinishCreate(%v)", success))
			}, fmt.Errorf("begin")
		},
		expectMilestones: []string{"BeginCreate"},
	}}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			pod := &example.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "foo", Namespace: "test"},
				Spec:       example.PodSpec{NodeName: "machine"},
			}

			testContext := genericapirequest.WithNamespace(genericapirequest.NewContext(), "test")
			destroyFunc, registry := NewTestGenericStoreRegistry(t)
			defer destroyFunc()
			registry.Decorator = tc.decorator
			registry.BeginCreate = tc.beginCreate
			registry.AfterCreate = tc.afterCreate
			registry.TTLFunc = tc.ttl

			// create the object
			milestones = nil
			obj, err := registry.Create(testContext, pod, rest.ValidateAllObjectFunc, &metav1.CreateOptions{})
			if err != nil && !tc.expectError {
				t.Fatalf("Unexpected error: %v", err)
			}
			if err == nil && tc.expectError {
				t.Fatalf("Unexpected success")
			}

			// verify the results
			if tc.expectAnnotation != "" {
				out := obj.(*example.Pod)
				if v, found := out.Annotations[tc.expectAnnotation]; !found {
					t.Errorf("Expected annotation %q not found", tc.expectAnnotation)
				} else if v != "true" {
					t.Errorf("Expected annotation %q has wrong value: %q", tc.expectAnnotation, v)
				}
			}
			if tc.expectMilestones != nil {
				if !reflect.DeepEqual(milestones, tc.expectMilestones) {
					t.Errorf("Unexpected milestones: wanted %v, got %v", tc.expectMilestones, milestones)
				}
			}
		})
	}
}

func isQualifiedResource(err error, kind, group string) bool {
	if err.(errors.APIStatus).Status().Details.Kind != kind || err.(errors.APIStatus).Status().Details.Group != group {
		return false
	}
	return true
}

func updateAndVerify(t *testing.T, ctx context.Context, registry *Store, pod *example.Pod) bool {
	obj, _, err := registry.Update(ctx, pod.Name, rest.DefaultUpdatedObjectInfo(pod), rest.ValidateAllObjectFunc, rest.ValidateAllObjectUpdateFunc, false, &metav1.UpdateOptions{})
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
		return false
	}
	checkObj, err := registry.Get(ctx, pod.Name, &metav1.GetOptions{})
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
		return false
	}
	if e, a := obj, checkObj; !reflect.DeepEqual(e, a) {
		t.Errorf("Expected %#v, got %#v", e, a)
		return false
	}
	return true
}

func TestStoreUpdate(t *testing.T) {
	podA := &example.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "foo", Namespace: "test"},
		Spec:       example.PodSpec{NodeName: "machine"},
	}
	podB := &example.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "foo", Namespace: "test"},
		Spec:       example.PodSpec{NodeName: "machine2"},
	}
	podAWithResourceVersion := &example.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "foo", Namespace: "test", ResourceVersion: "7"},
		Spec:       example.PodSpec{NodeName: "machine"},
	}

	testContext := genericapirequest.WithNamespace(genericapirequest.NewContext(), "test")
	destroyFunc, registry := NewTestGenericStoreRegistry(t)
	defer destroyFunc()

	// try to update a non-existing node with denying admission, should still return NotFound
	_, _, err := registry.Update(testContext, podA.Name, rest.DefaultUpdatedObjectInfo(podA), denyCreateValidation, denyUpdateValidation, false, &metav1.UpdateOptions{})
	if !errors.IsNotFound(err) {
		t.Errorf("Unexpected error: %v", err)
	}

	// try to update a non-existing node
	_, _, err = registry.Update(testContext, podA.Name, rest.DefaultUpdatedObjectInfo(podA), rest.ValidateAllObjectFunc, rest.ValidateAllObjectUpdateFunc, false, &metav1.UpdateOptions{})
	if !errors.IsNotFound(err) {
		t.Errorf("Unexpected error: %v", err)
	}

	// allow creation
	registry.UpdateStrategy.(*testRESTStrategy).allowCreateOnUpdate = true

	// createIfNotFound with denying create admission
	_, _, err = registry.Update(testContext, podA.Name, rest.DefaultUpdatedObjectInfo(podA), denyCreateValidation, rest.ValidateAllObjectUpdateFunc, false, &metav1.UpdateOptions{})
	if err == nil {
		t.Errorf("expected admission error on create")
	}

	// createIfNotFound and verify
	if !updateAndVerify(t, testContext, registry, podA) {
		t.Errorf("Unexpected error updating podA")
	}

	// forbid creation again
	registry.UpdateStrategy.(*testRESTStrategy).allowCreateOnUpdate = false

	// outofDate
	_, _, err = registry.Update(testContext, podAWithResourceVersion.Name, rest.DefaultUpdatedObjectInfo(podAWithResourceVersion), rest.ValidateAllObjectFunc, rest.ValidateAllObjectUpdateFunc, false, &metav1.UpdateOptions{})
	if !errors.IsConflict(err) {
		t.Errorf("Unexpected error updating podAWithResourceVersion: %v", err)
	}

	// try to update with denying admission
	_, _, err = registry.Update(testContext, podA.Name, rest.DefaultUpdatedObjectInfo(podA), rest.ValidateAllObjectFunc, denyUpdateValidation, false, &metav1.UpdateOptions{})
	if err == nil {
		t.Errorf("expected admission error on update")
	}

	// normal update and verify
	if !updateAndVerify(t, testContext, registry, podB) {
		t.Errorf("Unexpected error updating podB")
	}

	// unconditional update
	// NOTE: The logic for unconditional updates doesn't make sense to me, and imho should be removed.
	// doUnconditionalUpdate := resourceVersion == 0 && e.UpdateStrategy.AllowUnconditionalUpdate()
	// ^^ That condition can *never be true due to the creation of root objects.
	//
	// registry.UpdateStrategy.(*testRESTStrategy).allowUnconditionalUpdate = true
	// updateAndVerify(t, testContext, registry, podAWithResourceVersion)

}

func TestNoOpUpdates(t *testing.T) {
	destroyFunc, registry := NewTestGenericStoreRegistry(t)
	defer destroyFunc()

	newPod := func() *example.Pod {
		return &example.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: metav1.NamespaceDefault,
				Name:      "foo",
				Labels:    map[string]string{"prepare_create": "true"},
			},
			Spec: example.PodSpec{NodeName: "machine"},
		}
	}

	var err error
	var createResult runtime.Object
	if createResult, err = registry.Create(genericapirequest.NewDefaultContext(), newPod(), rest.ValidateAllObjectFunc, &metav1.CreateOptions{}); err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	createdPod, err := registry.Get(genericapirequest.NewDefaultContext(), "foo", &metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	var updateResult runtime.Object
	p := newPod()
	if updateResult, _, err = registry.Update(genericapirequest.NewDefaultContext(), p.Name, rest.DefaultUpdatedObjectInfo(p), rest.ValidateAllObjectFunc, rest.ValidateAllObjectUpdateFunc, false, &metav1.UpdateOptions{}); err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	// Check whether we do not return empty result on no-op update.
	if !reflect.DeepEqual(createResult, updateResult) {
		t.Errorf("no-op update should return a correct value, got: %#v", updateResult)
	}

	updatedPod, err := registry.Get(genericapirequest.NewDefaultContext(), "foo", &metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	createdMeta, err := meta.Accessor(createdPod)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	updatedMeta, err := meta.Accessor(updatedPod)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	if createdMeta.GetResourceVersion() != updatedMeta.GetResourceVersion() {
		t.Errorf("no-op update should be ignored and not written to etcd")
	}
}

func TestStoreUpdateHooks(t *testing.T) {
	// To track which hooks were called in what order.  Not all hooks can
	// mutate the object.
	var milestones []string

	setAnn := func(obj runtime.Object, key string) {
		pod := obj.(*example.Pod)
		if pod.Annotations == nil {
			pod.Annotations = make(map[string]string)
		}
		pod.Annotations[key] = "true"
	}
	mile := func(s string) {
		milestones = append(milestones, s)
	}

	testCases := []struct {
		name      string
		decorator func(runtime.Object)
		// create-on-update is tested elsewhere, but this proves non-use here
		beginCreate      BeginCreateFunc
		afterCreate      AfterCreateFunc
		beginUpdate      BeginUpdateFunc
		afterUpdate      AfterUpdateFunc
		expectError      bool
		expectAnnotation string   // to test object mutations
		expectMilestones []string // to test sequence
	}{{
		name: "no hooks",
	}, {
		name: "Decorator mutation",
		decorator: func(obj runtime.Object) {
			setAnn(obj, "DecoratorWasCalled")
		},
		expectAnnotation: "DecoratorWasCalled",
	}, {
		name: "AfterUpdate mutation",
		afterUpdate: func(obj runtime.Object, opts *metav1.UpdateOptions) {
			setAnn(obj, "AfterUpdateWasCalled")
		},
		expectAnnotation: "AfterUpdateWasCalled",
	}, {
		name: "BeginUpdate mutation",
		beginUpdate: func(_ context.Context, obj, _ runtime.Object, _ *metav1.UpdateOptions) (FinishFunc, error) {
			setAnn(obj, "BeginUpdateWasCalled")
			return func(context.Context, bool) {}, nil
		},
		expectAnnotation: "BeginUpdateWasCalled",
	}, {
		name: "success ordering",
		decorator: func(obj runtime.Object) {
			mile("Decorator")
		},
		afterCreate: func(obj runtime.Object, opts *metav1.CreateOptions) {
			mile("AfterCreate")
		},
		beginCreate: func(_ context.Context, obj runtime.Object, _ *metav1.CreateOptions) (FinishFunc, error) {
			mile("BeginCreate")
			return func(_ context.Context, success bool) {
				mile(fmt.Sprintf("FinishCreate(%v)", success))
			}, nil
		},
		afterUpdate: func(obj runtime.Object, opts *metav1.UpdateOptions) {
			mile("AfterUpdate")
		},
		beginUpdate: func(_ context.Context, obj, _ runtime.Object, _ *metav1.UpdateOptions) (FinishFunc, error) {
			mile("BeginUpdate")
			return func(_ context.Context, success bool) {
				mile(fmt.Sprintf("FinishUpdate(%v)", success))
			}, nil
		},
		expectMilestones: []string{"BeginUpdate", "FinishUpdate(true)", "AfterUpdate", "Decorator"},
	}, /* fail ordering is covered in TestStoreUpdateHooksInnerRetry */ {
		name:        "fail BeginUpdate ordering",
		expectError: true,
		decorator: func(obj runtime.Object) {
			mile("Decorator")
		},
		afterUpdate: func(obj runtime.Object, opts *metav1.UpdateOptions) {
			mile("AfterUpdate")
		},
		beginUpdate: func(_ context.Context, obj, _ runtime.Object, _ *metav1.UpdateOptions) (FinishFunc, error) {
			mile("BeginUpdate")
			return func(_ context.Context, success bool) {
				mile(fmt.Sprintf("FinishUpdate(%v)", success))
			}, fmt.Errorf("begin")
		},
		expectMilestones: []string{"BeginUpdate"},
	}}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			pod := &example.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "foo", Namespace: "test"},
				Spec:       example.PodSpec{NodeName: "machine"},
			}

			testContext := genericapirequest.WithNamespace(genericapirequest.NewContext(), "test")
			destroyFunc, registry := NewTestGenericStoreRegistry(t)
			defer destroyFunc()
			registry.BeginUpdate = tc.beginUpdate
			registry.AfterUpdate = tc.afterUpdate
			registry.BeginCreate = tc.beginCreate
			registry.AfterCreate = tc.afterCreate

			_, err := registry.Create(testContext, pod, rest.ValidateAllObjectFunc, &metav1.CreateOptions{})
			if err != nil {
				t.Fatalf("Unexpected error: %v", err)
			}
			milestones = nil
			registry.Decorator = tc.decorator
			obj, _, err := registry.Update(testContext, pod.Name, rest.DefaultUpdatedObjectInfo(pod), rest.ValidateAllObjectFunc, rest.ValidateAllObjectUpdateFunc, false, &metav1.UpdateOptions{})
			if err != nil && !tc.expectError {
				t.Fatalf("Unexpected error: %v", err)
			}
			if err == nil && tc.expectError {
				t.Fatalf("Unexpected success")
			}

			// verify the results
			if tc.expectAnnotation != "" {
				out := obj.(*example.Pod)
				if v, found := out.Annotations[tc.expectAnnotation]; !found {
					t.Errorf("Expected annotation %q not found", tc.expectAnnotation)
				} else if v != "true" {
					t.Errorf("Expected annotation %q has wrong value: %q", tc.expectAnnotation, v)
				}
			}
			if tc.expectMilestones != nil {
				if !reflect.DeepEqual(milestones, tc.expectMilestones) {
					t.Errorf("Unexpected milestones: wanted %v, got %v", tc.expectMilestones, milestones)
				}
			}
		})
	}
}

func TestStoreCreateOnUpdateHooks(t *testing.T) {
	// To track which hooks were called in what order.  Not all hooks can
	// mutate the object.
	var milestones []string

	mile := func(s string) {
		milestones = append(milestones, s)
	}

	testCases := []struct {
		name        string
		decorator   func(runtime.Object)
		beginCreate BeginCreateFunc
		afterCreate AfterCreateFunc
		beginUpdate BeginUpdateFunc
		afterUpdate AfterUpdateFunc
		// the TTLFunc is an easy hook to force a failure
		ttl              func(obj runtime.Object, existing uint64, update bool) (uint64, error)
		expectError      bool
		expectMilestones []string // to test sequence
	}{{
		name: "no hooks",
	}, {
		name: "success ordering",
		decorator: func(obj runtime.Object) {
			mile("Decorator")
		},
		afterCreate: func(obj runtime.Object, opts *metav1.CreateOptions) {
			mile("AfterCreate")
		},
		beginCreate: func(_ context.Context, obj runtime.Object, _ *metav1.CreateOptions) (FinishFunc, error) {
			mile("BeginCreate")
			return func(_ context.Context, success bool) {
				mile(fmt.Sprintf("FinishCreate(%v)", success))
			}, nil
		},
		afterUpdate: func(obj runtime.Object, opts *metav1.UpdateOptions) {
			mile("AfterUpdate")
		},
		beginUpdate: func(_ context.Context, obj, _ runtime.Object, _ *metav1.UpdateOptions) (FinishFunc, error) {
			mile("BeginUpdate")
			return func(_ context.Context, success bool) {
				mile(fmt.Sprintf("FinishUpdate(%v)", success))
			}, nil
		},
		expectMilestones: []string{"BeginCreate", "FinishCreate(true)", "AfterCreate", "Decorator"},
	}, {
		name: "fail ordering",
		decorator: func(obj runtime.Object) {
			mile("Decorator")
		},
		afterCreate: func(obj runtime.Object, opts *metav1.CreateOptions) {
			mile("AfterCreate")
		},
		beginCreate: func(_ context.Context, obj runtime.Object, _ *metav1.CreateOptions) (FinishFunc, error) {
			mile("BeginCreate")
			return func(_ context.Context, success bool) {
				mile(fmt.Sprintf("FinishCreate(%v)", success))
			}, nil
		},
		afterUpdate: func(obj runtime.Object, opts *metav1.UpdateOptions) {
			mile("AfterUpdate")
		},
		beginUpdate: func(_ context.Context, obj, _ runtime.Object, _ *metav1.UpdateOptions) (FinishFunc, error) {
			mile("BeginUpdate")
			return func(_ context.Context, success bool) {
				mile(fmt.Sprintf("FinishUpdate(%v)", success))
			}, nil
		},
		ttl: func(_ runtime.Object, existing uint64, _ bool) (uint64, error) {
			mile("TTLError")
			return existing, fmt.Errorf("TTL fail")
		},
		expectMilestones: []string{"BeginCreate", "TTLError", "FinishCreate(false)"},
		expectError:      true,
	}, {
		name:        "fail BeginCreate ordering",
		expectError: true,
		decorator: func(obj runtime.Object) {
			mile("Decorator")
		},
		afterCreate: func(obj runtime.Object, opts *metav1.CreateOptions) {
			mile("AfterCreate")
		},
		beginCreate: func(_ context.Context, obj runtime.Object, _ *metav1.CreateOptions) (FinishFunc, error) {
			mile("BeginCreate")
			return func(_ context.Context, success bool) {
				mile(fmt.Sprintf("FinishCreate(%v)", success))
			}, fmt.Errorf("begin")
		},
		afterUpdate: func(obj runtime.Object, opts *metav1.UpdateOptions) {
			mile("AfterUpdate")
		},
		beginUpdate: func(_ context.Context, obj, _ runtime.Object, _ *metav1.UpdateOptions) (FinishFunc, error) {
			mile("BeginUpdate")
			return func(_ context.Context, success bool) {
				mile(fmt.Sprintf("FinishUpdate(%v)", success))
			}, nil
		},
		expectMilestones: []string{"BeginCreate"},
	}}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			pod := &example.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "foo", Namespace: "test"},
				Spec:       example.PodSpec{NodeName: "machine"},
			}

			testContext := genericapirequest.WithNamespace(genericapirequest.NewContext(), "test")
			destroyFunc, registry := NewTestGenericStoreRegistry(t)
			defer destroyFunc()
			registry.Decorator = tc.decorator
			registry.UpdateStrategy.(*testRESTStrategy).allowCreateOnUpdate = true
			registry.BeginUpdate = tc.beginUpdate
			registry.AfterUpdate = tc.afterUpdate
			registry.BeginCreate = tc.beginCreate
			registry.AfterCreate = tc.afterCreate
			registry.TTLFunc = tc.ttl

			// NB: did not create it first.
			milestones = nil
			_, _, err := registry.Update(testContext, pod.Name, rest.DefaultUpdatedObjectInfo(pod), rest.ValidateAllObjectFunc, rest.ValidateAllObjectUpdateFunc, false, &metav1.UpdateOptions{})
			if err != nil && !tc.expectError {
				t.Fatalf("Unexpected error: %v", err)
			}
			if err == nil && tc.expectError {
				t.Fatalf("Unexpected success")
			}

			// verify the results
			if tc.expectMilestones != nil {
				if !reflect.DeepEqual(milestones, tc.expectMilestones) {
					t.Errorf("Unexpected milestones: wanted %v, got %v", tc.expectMilestones, milestones)
				}
			}
		})
	}
}

func TestStoreUpdateHooksInnerRetry(t *testing.T) {
	// To track which hooks were called in what order.  Not all hooks can
	// mutate the object.
	var milestones []string

	mile := func(s string) {
		milestones = append(milestones, s)
	}
	ttlFailDone := false
	ttlFailOnce := func(_ runtime.Object, existing uint64, _ bool) (uint64, error) {
		if ttlFailDone {
			mile("TTL")
			return existing, nil
		}
		ttlFailDone = true
		mile("TTLError")
		return existing, fmt.Errorf("TTL fail")
	}
	ttlFailAlways := func(_ runtime.Object, existing uint64, _ bool) (uint64, error) {
		mile("TTLError")
		return existing, fmt.Errorf("TTL fail")
	}

	testCases := []struct {
		name        string
		decorator   func(runtime.Object)
		beginUpdate func(context.Context, runtime.Object, runtime.Object, *metav1.UpdateOptions) (FinishFunc, error)
		afterUpdate AfterUpdateFunc
		// the TTLFunc is an easy hook to force an inner-loop retry
		ttl              func(obj runtime.Object, existing uint64, update bool) (uint64, error)
		expectError      bool
		expectMilestones []string // to test sequence
	}{{
		name: "inner retry success",
		decorator: func(obj runtime.Object) {
			mile("Decorator")
		},
		afterUpdate: func(obj runtime.Object, opts *metav1.UpdateOptions) {
			mile("AfterUpdate")
		},
		beginUpdate: func(_ context.Context, obj, _ runtime.Object, _ *metav1.UpdateOptions) (FinishFunc, error) {
			mile("BeginUpdate")
			return func(_ context.Context, success bool) {
				mile(fmt.Sprintf("FinishUpdate(%v)", success))
			}, nil
		},
		ttl:              ttlFailOnce,
		expectMilestones: []string{"BeginUpdate", "TTLError", "FinishUpdate(false)", "BeginUpdate", "TTL", "FinishUpdate(true)", "AfterUpdate", "Decorator"},
	}, {
		name: "inner retry fail",
		decorator: func(obj runtime.Object) {
			mile("Decorator")
		},
		afterUpdate: func(obj runtime.Object, opts *metav1.UpdateOptions) {
			mile("AfterUpdate")
		},
		beginUpdate: func(_ context.Context, obj, _ runtime.Object, _ *metav1.UpdateOptions) (FinishFunc, error) {
			mile("BeginUpdate")
			return func(_ context.Context, success bool) {
				mile(fmt.Sprintf("FinishUpdate(%v)", success))
			}, nil
		},
		ttl:              ttlFailAlways,
		expectError:      true,
		expectMilestones: []string{"BeginUpdate", "TTLError", "FinishUpdate(false)", "BeginUpdate", "TTLError", "FinishUpdate(false)"},
	}}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			pod := &example.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "foo", Namespace: "test"},
				Spec:       example.PodSpec{NodeName: "machine"},
			}

			testContext := genericapirequest.WithNamespace(genericapirequest.NewContext(), "test")
			destroyFunc, registry := NewTestGenericStoreRegistry(t)
			defer destroyFunc()
			registry.BeginUpdate = tc.beginUpdate
			registry.AfterUpdate = tc.afterUpdate

			created, err := registry.Create(testContext, pod, rest.ValidateAllObjectFunc, &metav1.CreateOptions{})
			if err != nil {
				t.Fatalf("Unexpected error: %v", err)
			}
			milestones = nil
			registry.Decorator = tc.decorator
			ttlFailDone = false
			registry.TTLFunc = tc.ttl
			// force storage to use a cached object with a non-matching resourceVersion to guarantee a live lookup + retry
			created.(*example.Pod).ResourceVersion += "0"
			registry.Storage.Storage = &staleGuaranteedUpdateStorage{Interface: registry.Storage.Storage, cachedObj: created}
			_, _, err = registry.Update(testContext, pod.Name, rest.DefaultUpdatedObjectInfo(pod), rest.ValidateAllObjectFunc, rest.ValidateAllObjectUpdateFunc, false, &metav1.UpdateOptions{})
			if err != nil && !tc.expectError {
				t.Fatalf("Unexpected error: %v", err)
			}
			if err == nil && tc.expectError {
				t.Fatalf("Unexpected success")
			}

			// verify the results
			if tc.expectMilestones != nil {
				if !reflect.DeepEqual(milestones, tc.expectMilestones) {
					t.Errorf("Unexpected milestones: wanted %v, got %v", tc.expectMilestones, milestones)
				}
			}
		})
	}
}

func TestStoreGet(t *testing.T) {
	podA := &example.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "foo"},
		Spec:       example.PodSpec{NodeName: "machine"},
	}

	testContext := genericapirequest.WithNamespace(genericapirequest.NewContext(), "test")
	destroyFunc, registry := NewTestGenericStoreRegistry(t)
	defer destroyFunc()

	_, err := registry.Get(testContext, podA.Name, &metav1.GetOptions{})
	if !errors.IsNotFound(err) {
		t.Errorf("Unexpected error: %v", err)
	}

	registry.UpdateStrategy.(*testRESTStrategy).allowCreateOnUpdate = true
	if !updateAndVerify(t, testContext, registry, podA) {
		t.Errorf("Unexpected error updating podA")
	}
}

func TestStoreDelete(t *testing.T) {
	podA := &example.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "foo"},
		Spec:       example.PodSpec{NodeName: "machine"},
	}

	testContext := genericapirequest.WithNamespace(genericapirequest.NewContext(), "test")
	destroyFunc, registry := NewTestGenericStoreRegistry(t)
	defer destroyFunc()

	afterWasCalled := false
	registry.AfterDelete = func(obj runtime.Object, options *metav1.DeleteOptions) {
		afterWasCalled = true
	}

	// test failure condition
	_, _, err := registry.Delete(testContext, podA.Name, rest.ValidateAllObjectFunc, nil)
	if !errors.IsNotFound(err) {
		t.Errorf("Unexpected error: %v", err)
	}
	if afterWasCalled {
		t.Errorf("Unexpected call to AfterDelete")
	}

	// create pod
	_, err = registry.Create(testContext, podA, rest.ValidateAllObjectFunc, &metav1.CreateOptions{})
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}

	// delete object
	_, wasDeleted, err := registry.Delete(testContext, podA.Name, rest.ValidateAllObjectFunc, nil)
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}
	if !wasDeleted {
		t.Errorf("unexpected, pod %s should have been deleted immediately", podA.Name)
	}
	if !afterWasCalled {
		t.Errorf("Expected call to AfterDelete, but got none")
	}

	// try to get a item which should be deleted
	_, err = registry.Get(testContext, podA.Name, &metav1.GetOptions{})
	if !errors.IsNotFound(err) {
		t.Errorf("Unexpected error: %v", err)
	}
}

func TestStoreGracefulDeleteWithResourceVersion(t *testing.T) {
	podA := &example.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "foo"},
		Spec:       example.PodSpec{NodeName: "machine"},
	}

	testContext := genericapirequest.WithNamespace(genericapirequest.NewContext(), "test")
	destroyFunc, registry := NewTestGenericStoreRegistry(t)
	defer destroyFunc()

	defaultDeleteStrategy := testRESTStrategy{scheme, names.SimpleNameGenerator, true, false, true}
	registry.DeleteStrategy = testGracefulStrategy{defaultDeleteStrategy}

	// test failure condition
	_, _, err := registry.Delete(testContext, podA.Name, rest.ValidateAllObjectFunc, nil)
	if !errors.IsNotFound(err) {
		t.Errorf("Unexpected error: %v", err)
	}

	// create pod
	_, err = registry.Create(testContext, podA, rest.ValidateAllObjectFunc, &metav1.CreateOptions{})
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}

	// try to get a item which should be deleted
	obj, err := registry.Get(testContext, podA.Name, &metav1.GetOptions{})
	if errors.IsNotFound(err) {
		t.Errorf("Unexpected error: %v", err)
	}

	accessor, err := meta.Accessor(obj)
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}

	resourceVersion := accessor.GetResourceVersion()

	options := metav1.NewDeleteOptions(0)
	options.Preconditions = &metav1.Preconditions{ResourceVersion: &resourceVersion}

	// delete object
	_, wasDeleted, err := registry.Delete(testContext, podA.Name, rest.ValidateAllObjectFunc, options)
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}
	if !wasDeleted {
		t.Errorf("unexpected, pod %s should have been deleted immediately", podA.Name)
	}

	// try to get a item which should be deleted
	_, err = registry.Get(testContext, podA.Name, &metav1.GetOptions{})
	if !errors.IsNotFound(err) {
		t.Errorf("Unexpected error: %v", err)
	}
}

// TestGracefulStoreCanDeleteIfExistingGracePeriodZero tests recovery from
// race condition where the graceful delete is unable to complete
// in prior operation, but the pod remains with deletion timestamp
// and grace period set to 0.
func TestGracefulStoreCanDeleteIfExistingGracePeriodZero(t *testing.T) {
	deletionTimestamp := metav1.NewTime(time.Now())
	deletionGracePeriodSeconds := int64(0)
	initialGeneration := int64(1)
	pod := &example.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:                       "foo",
			Generation:                 initialGeneration,
			DeletionGracePeriodSeconds: &deletionGracePeriodSeconds,
			DeletionTimestamp:          &deletionTimestamp,
		},
		Spec: example.PodSpec{NodeName: "machine"},
	}

	testContext := genericapirequest.WithNamespace(genericapirequest.NewContext(), "test")
	destroyFunc, registry := NewTestGenericStoreRegistry(t)
	registry.EnableGarbageCollection = false
	defaultDeleteStrategy := testRESTStrategy{scheme, names.SimpleNameGenerator, true, false, true}
	registry.DeleteStrategy = testGracefulStrategy{defaultDeleteStrategy}
	defer destroyFunc()

	graceful, gracefulPending, err := rest.BeforeDelete(registry.DeleteStrategy, testContext, pod, metav1.NewDeleteOptions(0))
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if graceful {
		t.Fatalf("graceful should be false if object has DeletionTimestamp and DeletionGracePeriodSeconds is 0")
	}
	if gracefulPending {
		t.Fatalf("gracefulPending should be false if object has DeletionTimestamp and DeletionGracePeriodSeconds is 0")
	}
}

func TestGracefulStoreHandleFinalizers(t *testing.T) {
	initialGeneration := int64(1)
	podWithFinalizer := &example.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "foo", Finalizers: []string{"foo.com/x"}, Generation: initialGeneration},
		Spec:       example.PodSpec{NodeName: "machine"},
	}

	testContext := genericapirequest.WithNamespace(genericapirequest.NewContext(), "test")
	destroyFunc, registry := NewTestGenericStoreRegistry(t)
	defaultDeleteStrategy := testRESTStrategy{scheme, names.SimpleNameGenerator, true, false, true}
	registry.DeleteStrategy = testGracefulStrategy{defaultDeleteStrategy}
	defer destroyFunc()

	afterWasCalled := false
	registry.AfterDelete = func(obj runtime.Object, options *metav1.DeleteOptions) {
		afterWasCalled = true
	}

	gcStates := []bool{true, false}
	for _, gcEnabled := range gcStates {
		t.Logf("garbage collection enabled: %t", gcEnabled)
		registry.EnableGarbageCollection = gcEnabled

		afterWasCalled = false // reset

		// create pod
		_, err := registry.Create(testContext, podWithFinalizer, rest.ValidateAllObjectFunc, &metav1.CreateOptions{})
		if err != nil {
			t.Errorf("Unexpected error: %v", err)
		}

		// delete the pod with grace period=0, the pod should still exist because it has a finalizer
		_, wasDeleted, err := registry.Delete(testContext, podWithFinalizer.Name, rest.ValidateAllObjectFunc, metav1.NewDeleteOptions(0))
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}
		if wasDeleted {
			t.Errorf("unexpected, pod %s should not have been deleted immediately", podWithFinalizer.Name)
		}
		if afterWasCalled {
			t.Errorf("unexpected, AfterDelete() was called")
		}
		_, err = registry.Get(testContext, podWithFinalizer.Name, &metav1.GetOptions{})
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}

		updatedPodWithFinalizer := &example.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "foo", Finalizers: []string{"foo.com/x"}, ResourceVersion: podWithFinalizer.ObjectMeta.ResourceVersion},
			Spec:       example.PodSpec{NodeName: "machine"},
		}
		_, _, err = registry.Update(testContext, updatedPodWithFinalizer.ObjectMeta.Name, rest.DefaultUpdatedObjectInfo(updatedPodWithFinalizer), rest.ValidateAllObjectFunc, rest.ValidateAllObjectUpdateFunc, false, &metav1.UpdateOptions{})
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}
		if afterWasCalled {
			t.Errorf("unexpected, AfterDelete() was called")
		}

		// the object should still exist, because it still has a finalizer
		_, err = registry.Get(testContext, podWithFinalizer.Name, &metav1.GetOptions{})
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}

		podWithNoFinalizer := &example.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "foo", ResourceVersion: podWithFinalizer.ObjectMeta.ResourceVersion},
			Spec:       example.PodSpec{NodeName: "anothermachine"},
		}
		_, _, err = registry.Update(testContext, podWithFinalizer.ObjectMeta.Name, rest.DefaultUpdatedObjectInfo(podWithNoFinalizer), rest.ValidateAllObjectFunc, rest.ValidateAllObjectUpdateFunc, false, &metav1.UpdateOptions{})
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}
		if !afterWasCalled {
			t.Errorf("unexpected, AfterDelete() was not called")
		}
		// the pod should be removed, because its finalizer is removed
		_, err = registry.Get(testContext, podWithFinalizer.Name, &metav1.GetOptions{})
		if !errors.IsNotFound(err) {
			t.Fatalf("Unexpected error: %v", err)
		}
	}
}

func TestNonGracefulStoreHandleFinalizers(t *testing.T) {
	initialGeneration := int64(1)
	podWithFinalizer := &example.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "foo", Finalizers: []string{"foo.com/x"}, Generation: initialGeneration},
		Spec:       example.PodSpec{NodeName: "machine"},
	}

	testContext := genericapirequest.WithNamespace(genericapirequest.NewContext(), "test")
	destroyFunc, registry := NewTestGenericStoreRegistry(t)
	defer destroyFunc()

	afterWasCalled := false
	registry.AfterDelete = func(obj runtime.Object, options *metav1.DeleteOptions) {
		afterWasCalled = true
	}

	gcStates := []bool{true, false}
	for _, gcEnabled := range gcStates {
		t.Logf("garbage collection enabled: %t", gcEnabled)
		registry.EnableGarbageCollection = gcEnabled

		afterWasCalled = false // reset

		// create pod
		_, err := registry.Create(testContext, podWithFinalizer, rest.ValidateAllObjectFunc, &metav1.CreateOptions{})
		if err != nil {
			t.Errorf("Unexpected error: %v", err)
		}

		// delete object with nil delete options doesn't delete the object
		_, wasDeleted, err := registry.Delete(testContext, podWithFinalizer.Name, rest.ValidateAllObjectFunc, nil)
		if err != nil {
			t.Errorf("Unexpected error: %v", err)
		}
		if wasDeleted {
			t.Errorf("unexpected, pod %s should not have been deleted immediately", podWithFinalizer.Name)
		}
		if afterWasCalled {
			t.Errorf("unexpected, AfterDelete() was called")
		}

		// the object should still exist
		obj, err := registry.Get(testContext, podWithFinalizer.Name, &metav1.GetOptions{})
		if err != nil {
			t.Errorf("Unexpected error: %v", err)
		}
		podWithFinalizer, ok := obj.(*example.Pod)
		if !ok {
			t.Errorf("Unexpected object: %#v", obj)
		}
		if podWithFinalizer.ObjectMeta.DeletionTimestamp == nil {
			t.Errorf("Expect the object to have DeletionTimestamp set, but got %#v", podWithFinalizer.ObjectMeta)
		}
		if podWithFinalizer.ObjectMeta.DeletionGracePeriodSeconds == nil || *podWithFinalizer.ObjectMeta.DeletionGracePeriodSeconds != 0 {
			t.Errorf("Expect the object to have 0 DeletionGracePeriodSecond, but got %#v", podWithFinalizer.ObjectMeta)
		}
		if podWithFinalizer.Generation <= initialGeneration {
			t.Errorf("Deletion didn't increase Generation.")
		}

		updatedPodWithFinalizer := &example.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "foo", Finalizers: []string{"foo.com/x"}, ResourceVersion: podWithFinalizer.ObjectMeta.ResourceVersion},
			Spec:       example.PodSpec{NodeName: "machine"},
		}
		_, _, err = registry.Update(testContext, updatedPodWithFinalizer.ObjectMeta.Name, rest.DefaultUpdatedObjectInfo(updatedPodWithFinalizer), rest.ValidateAllObjectFunc, rest.ValidateAllObjectUpdateFunc, false, &metav1.UpdateOptions{})
		if err != nil {
			t.Errorf("Unexpected error: %v", err)
		}
		if afterWasCalled {
			t.Errorf("unexpected, AfterDelete() was called")
		}

		// the object should still exist, because it still has a finalizer
		obj, err = registry.Get(testContext, podWithFinalizer.Name, &metav1.GetOptions{})
		if err != nil {
			t.Errorf("Unexpected error: %v", err)
		}
		podWithFinalizer, ok = obj.(*example.Pod)
		if !ok {
			t.Errorf("Unexpected object: %#v", obj)
		}

		podWithNoFinalizer := &example.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "foo", ResourceVersion: podWithFinalizer.ObjectMeta.ResourceVersion},
			Spec:       example.PodSpec{NodeName: "anothermachine"},
		}
		_, _, err = registry.Update(testContext, podWithFinalizer.ObjectMeta.Name, rest.DefaultUpdatedObjectInfo(podWithNoFinalizer), rest.ValidateAllObjectFunc, rest.ValidateAllObjectUpdateFunc, false, &metav1.UpdateOptions{})
		if err != nil {
			t.Errorf("Unexpected error: %v", err)
		}
		if !afterWasCalled {
			t.Errorf("unexpected, AfterDelete() was not called")
		}
		// the pod should be removed, because its finalizer is removed
		_, err = registry.Get(testContext, podWithFinalizer.Name, &metav1.GetOptions{})
		if !errors.IsNotFound(err) {
			t.Errorf("Unexpected error: %v", err)
		}
	}
}

func TestStoreDeleteWithOrphanDependents(t *testing.T) {
	initialGeneration := int64(1)
	podWithOrphanFinalizer := func(name string) *example.Pod {
		return &example.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Finalizers: []string{"foo.com/x", metav1.FinalizerOrphanDependents, "bar.com/y"}, Generation: initialGeneration},
			Spec:       example.PodSpec{NodeName: "machine"},
		}
	}
	podWithOtherFinalizers := func(name string) *example.Pod {
		return &example.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Finalizers: []string{"foo.com/x", "bar.com/y"}, Generation: initialGeneration},
			Spec:       example.PodSpec{NodeName: "machine"},
		}
	}
	podWithNoFinalizer := func(name string) *example.Pod {
		return &example.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Generation: initialGeneration},
			Spec:       example.PodSpec{NodeName: "machine"},
		}
	}
	podWithOnlyOrphanFinalizer := func(name string) *example.Pod {
		return &example.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Finalizers: []string{metav1.FinalizerOrphanDependents}, Generation: initialGeneration},
			Spec:       example.PodSpec{NodeName: "machine"},
		}
	}
	trueVar, falseVar := true, false
	orphanOptions := &metav1.DeleteOptions{OrphanDependents: &trueVar}
	nonOrphanOptions := &metav1.DeleteOptions{OrphanDependents: &falseVar}
	nilOrphanOptions := &metav1.DeleteOptions{}

	// defaultDeleteStrategy doesn't implement rest.GarbageCollectionDeleteStrategy.
	defaultDeleteStrategy := &testRESTStrategy{scheme, names.SimpleNameGenerator, true, false, true}
	// orphanDeleteStrategy indicates the default garbage collection policy is
	// to orphan dependencies.
	orphanDeleteStrategy := &testOrphanDeleteStrategy{defaultDeleteStrategy}

	testcases := []struct {
		pod               *example.Pod
		options           *metav1.DeleteOptions
		strategy          rest.RESTDeleteStrategy
		expectNotFound    bool
		updatedFinalizers []string
	}{
		// cases run with DeleteOptions.OrphanDependents=true
		{
			podWithOrphanFinalizer("pod1"),
			orphanOptions,
			defaultDeleteStrategy,
			false,
			[]string{"foo.com/x", metav1.FinalizerOrphanDependents, "bar.com/y"},
		},
		{
			podWithOtherFinalizers("pod2"),
			orphanOptions,
			defaultDeleteStrategy,
			false,
			[]string{"foo.com/x", "bar.com/y", metav1.FinalizerOrphanDependents},
		},
		{
			podWithNoFinalizer("pod3"),
			orphanOptions,
			defaultDeleteStrategy,
			false,
			[]string{metav1.FinalizerOrphanDependents},
		},
		{
			podWithOnlyOrphanFinalizer("pod4"),
			orphanOptions,
			defaultDeleteStrategy,
			false,
			[]string{metav1.FinalizerOrphanDependents},
		},
		// cases run with DeleteOptions.OrphanDependents=false
		// these cases all have orphanDeleteStrategy, which should be ignored
		// because DeleteOptions has the highest priority.
		{
			podWithOrphanFinalizer("pod5"),
			nonOrphanOptions,
			orphanDeleteStrategy,
			false,
			[]string{"foo.com/x", "bar.com/y"},
		},
		{
			podWithOtherFinalizers("pod6"),
			nonOrphanOptions,
			orphanDeleteStrategy,
			false,
			[]string{"foo.com/x", "bar.com/y"},
		},
		{
			podWithNoFinalizer("pod7"),
			nonOrphanOptions,
			orphanDeleteStrategy,
			true,
			[]string{},
		},
		{
			podWithOnlyOrphanFinalizer("pod8"),
			nonOrphanOptions,
			orphanDeleteStrategy,
			true,
			[]string{},
		},
		// cases run with nil DeleteOptions.OrphanDependents. If the object
		// already has the orphan finalizer, then the DeleteStrategy should be
		// ignored. Otherwise the DeleteStrategy decides whether to add the
		// orphan finalizer.
		{
			podWithOrphanFinalizer("pod9"),
			nilOrphanOptions,
			defaultDeleteStrategy,
			false,
			[]string{"foo.com/x", metav1.FinalizerOrphanDependents, "bar.com/y"},
		},
		{
			podWithOrphanFinalizer("pod10"),
			nilOrphanOptions,
			orphanDeleteStrategy,
			false,
			[]string{"foo.com/x", metav1.FinalizerOrphanDependents, "bar.com/y"},
		},
		{
			podWithOtherFinalizers("pod11"),
			nilOrphanOptions,
			defaultDeleteStrategy,
			false,
			[]string{"foo.com/x", "bar.com/y"},
		},
		{
			podWithOtherFinalizers("pod12"),
			nilOrphanOptions,
			orphanDeleteStrategy,
			false,
			[]string{"foo.com/x", "bar.com/y", metav1.FinalizerOrphanDependents},
		},
		{
			podWithNoFinalizer("pod13"),
			nilOrphanOptions,
			defaultDeleteStrategy,
			true,
			[]string{},
		},
		{
			podWithNoFinalizer("pod14"),
			nilOrphanOptions,
			orphanDeleteStrategy,
			false,
			[]string{metav1.FinalizerOrphanDependents},
		},
		{
			podWithOnlyOrphanFinalizer("pod15"),
			nilOrphanOptions,
			defaultDeleteStrategy,
			false,
			[]string{metav1.FinalizerOrphanDependents},
		},
		{
			podWithOnlyOrphanFinalizer("pod16"),
			nilOrphanOptions,
			orphanDeleteStrategy,
			false,
			[]string{metav1.FinalizerOrphanDependents},
		},

		// cases run with nil DeleteOptions should have exact same behavior.
		// They should be exactly the same as above cases where
		// DeleteOptions.OrphanDependents is nil.
		{
			podWithOrphanFinalizer("pod17"),
			nil,
			defaultDeleteStrategy,
			false,
			[]string{"foo.com/x", metav1.FinalizerOrphanDependents, "bar.com/y"},
		},
		{
			podWithOrphanFinalizer("pod18"),
			nil,
			orphanDeleteStrategy,
			false,
			[]string{"foo.com/x", metav1.FinalizerOrphanDependents, "bar.com/y"},
		},
		{
			podWithOtherFinalizers("pod19"),
			nil,
			defaultDeleteStrategy,
			false,
			[]string{"foo.com/x", "bar.com/y"},
		},
		{
			podWithOtherFinalizers("pod20"),
			nil,
			orphanDeleteStrategy,
			false,
			[]string{"foo.com/x", "bar.com/y", metav1.FinalizerOrphanDependents},
		},
		{
			podWithNoFinalizer("pod21"),
			nil,
			defaultDeleteStrategy,
			true,
			[]string{},
		},
		{
			podWithNoFinalizer("pod22"),
			nil,
			orphanDeleteStrategy,
			false,
			[]string{metav1.FinalizerOrphanDependents},
		},
		{
			podWithOnlyOrphanFinalizer("pod23"),
			nil,
			defaultDeleteStrategy,
			false,
			[]string{metav1.FinalizerOrphanDependents},
		},
		{
			podWithOnlyOrphanFinalizer("pod24"),
			nil,
			orphanDeleteStrategy,
			false,
			[]string{metav1.FinalizerOrphanDependents},
		},
	}

	testContext := genericapirequest.WithNamespace(genericapirequest.NewContext(), "test")
	destroyFunc, registry := NewTestGenericStoreRegistry(t)
	registry.EnableGarbageCollection = true
	defer destroyFunc()

	for _, tc := range testcases {
		registry.DeleteStrategy = tc.strategy
		// create pod
		_, err := registry.Create(testContext, tc.pod, rest.ValidateAllObjectFunc, &metav1.CreateOptions{})
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}
		_, _, err = registry.Delete(testContext, tc.pod.Name, rest.ValidateAllObjectFunc, tc.options)
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}
		obj, err := registry.Get(testContext, tc.pod.Name, &metav1.GetOptions{})
		if tc.expectNotFound && (err == nil || !errors.IsNotFound(err)) {
			t.Fatalf("Unexpected error: %v", err)
		}
		if !tc.expectNotFound && err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}
		if !tc.expectNotFound {
			pod, ok := obj.(*example.Pod)
			if !ok {
				t.Fatalf("Expect the object to be a pod, but got %#v", obj)
			}
			if pod.ObjectMeta.DeletionTimestamp == nil {
				t.Errorf("%v: Expect the object to have DeletionTimestamp set, but got %#v", pod.Name, pod.ObjectMeta)
			}
			if pod.ObjectMeta.DeletionGracePeriodSeconds == nil || *pod.ObjectMeta.DeletionGracePeriodSeconds != 0 {
				t.Errorf("%v: Expect the object to have 0 DeletionGracePeriodSecond, but got %#v", pod.Name, pod.ObjectMeta)
			}
			if pod.Generation <= initialGeneration {
				t.Errorf("%v: Deletion didn't increase Generation.", pod.Name)
			}
			if e, a := tc.updatedFinalizers, pod.ObjectMeta.Finalizers; !reflect.DeepEqual(e, a) {
				t.Errorf("%v: Expect object %s to have finalizers %v, got %v", pod.Name, pod.ObjectMeta.Name, e, a)
			}
		}
	}
}

// Test the DeleteOptions.PropagationPolicy is handled correctly
func TestStoreDeletionPropagation(t *testing.T) {
	initialGeneration := int64(1)

	// defaultDeleteStrategy doesn't implement rest.GarbageCollectionDeleteStrategy.
	defaultDeleteStrategy := &testRESTStrategy{scheme, names.SimpleNameGenerator, true, false, true}
	// orphanDeleteStrategy indicates the default garbage collection policy is
	// to orphan dependencies.
	orphanDeleteStrategy := &testOrphanDeleteStrategy{defaultDeleteStrategy}

	foregroundPolicy := metav1.DeletePropagationForeground
	backgroundPolicy := metav1.DeletePropagationBackground
	orphanPolicy := metav1.DeletePropagationOrphan

	testcases := map[string]struct {
		options  *metav1.DeleteOptions
		strategy rest.RESTDeleteStrategy
		// finalizers that are already set in the object
		existingFinalizers []string
		expectedNotFound   bool
		expectedFinalizers []string
	}{
		"no existing finalizers, PropagationPolicy=Foreground, defaultDeleteStrategy": {
			options:            &metav1.DeleteOptions{PropagationPolicy: &foregroundPolicy},
			strategy:           defaultDeleteStrategy,
			expectedFinalizers: []string{metav1.FinalizerDeleteDependents},
		},
		"no existing finalizers, PropagationPolicy=Foreground, orphanDeleteStrategy": {
			options:            &metav1.DeleteOptions{PropagationPolicy: &foregroundPolicy},
			strategy:           orphanDeleteStrategy,
			expectedFinalizers: []string{metav1.FinalizerDeleteDependents},
		},
		"no existing finalizers, PropagationPolicy=Background, defaultDeleteStrategy": {
			options:          &metav1.DeleteOptions{PropagationPolicy: &backgroundPolicy},
			strategy:         defaultDeleteStrategy,
			expectedNotFound: true,
		},
		"no existing finalizers, PropagationPolicy=Background, orphanDeleteStrategy": {
			options:          &metav1.DeleteOptions{PropagationPolicy: &backgroundPolicy},
			strategy:         orphanDeleteStrategy,
			expectedNotFound: true,
		},
		"no existing finalizers, PropagationPolicy=OrphanDependents, defaultDeleteStrategy": {
			options:            &metav1.DeleteOptions{PropagationPolicy: &orphanPolicy},
			strategy:           defaultDeleteStrategy,
			expectedFinalizers: []string{metav1.FinalizerOrphanDependents},
		},
		"no existing finalizers, PropagationPolicy=OrphanDependents, orphanDeleteStrategy": {
			options:            &metav1.DeleteOptions{PropagationPolicy: &orphanPolicy},
			strategy:           orphanDeleteStrategy,
			expectedFinalizers: []string{metav1.FinalizerOrphanDependents},
		},
		"no existing finalizers, PropagationPolicy=Default, defaultDeleteStrategy": {
			options:          &metav1.DeleteOptions{PropagationPolicy: nil},
			strategy:         defaultDeleteStrategy,
			expectedNotFound: true,
		},
		"no existing finalizers, PropagationPolicy=Default, orphanDeleteStrategy": {
			options:            &metav1.DeleteOptions{PropagationPolicy: nil},
			strategy:           orphanDeleteStrategy,
			expectedFinalizers: []string{metav1.FinalizerOrphanDependents},
		},

		// all cases in the following block have "existing orphan finalizer"
		"existing orphan finalizer, PropagationPolicy=Foreground, defaultDeleteStrategy": {
			options:            &metav1.DeleteOptions{PropagationPolicy: &foregroundPolicy},
			strategy:           defaultDeleteStrategy,
			existingFinalizers: []string{metav1.FinalizerOrphanDependents},
			expectedFinalizers: []string{metav1.FinalizerDeleteDependents},
		},
		"existing orphan finalizer, PropagationPolicy=Foreground, orphanDeleteStrategy": {
			options:            &metav1.DeleteOptions{PropagationPolicy: &foregroundPolicy},
			strategy:           orphanDeleteStrategy,
			existingFinalizers: []string{metav1.FinalizerOrphanDependents},
			expectedFinalizers: []string{metav1.FinalizerDeleteDependents},
		},
		"existing orphan finalizer, PropagationPolicy=Background, defaultDeleteStrategy": {
			options:            &metav1.DeleteOptions{PropagationPolicy: &backgroundPolicy},
			strategy:           defaultDeleteStrategy,
			existingFinalizers: []string{metav1.FinalizerOrphanDependents},
			expectedNotFound:   true,
		},
		"existing orphan finalizer, PropagationPolicy=Background, orphanDeleteStrategy": {
			options:            &metav1.DeleteOptions{PropagationPolicy: &backgroundPolicy},
			strategy:           orphanDeleteStrategy,
			existingFinalizers: []string{metav1.FinalizerOrphanDependents},
			expectedNotFound:   true,
		},
		"existing orphan finalizer, PropagationPolicy=OrphanDependents, defaultDeleteStrategy": {
			options:            &metav1.DeleteOptions{PropagationPolicy: &orphanPolicy},
			strategy:           defaultDeleteStrategy,
			existingFinalizers: []string{metav1.FinalizerOrphanDependents},
			expectedFinalizers: []string{metav1.FinalizerOrphanDependents},
		},
		"existing orphan finalizer, PropagationPolicy=OrphanDependents, orphanDeleteStrategy": {
			options:            &metav1.DeleteOptions{PropagationPolicy: &orphanPolicy},
			strategy:           orphanDeleteStrategy,
			existingFinalizers: []string{metav1.FinalizerOrphanDependents},
			expectedFinalizers: []string{metav1.FinalizerOrphanDependents},
		},
		"existing orphan finalizer, PropagationPolicy=Default, defaultDeleteStrategy": {
			options:            &metav1.DeleteOptions{PropagationPolicy: nil},
			strategy:           defaultDeleteStrategy,
			existingFinalizers: []string{metav1.FinalizerOrphanDependents},
			expectedFinalizers: []string{metav1.FinalizerOrphanDependents},
		},
		"existing orphan finalizer, PropagationPolicy=Default, orphanDeleteStrategy": {
			options:            &metav1.DeleteOptions{PropagationPolicy: nil},
			strategy:           orphanDeleteStrategy,
			existingFinalizers: []string{metav1.FinalizerOrphanDependents},
			expectedFinalizers: []string{metav1.FinalizerOrphanDependents},
		},

		// all cases in the following block have "existing deleteDependents finalizer"
		"existing deleteDependents finalizer, PropagationPolicy=Foreground, defaultDeleteStrategy": {
			options:            &metav1.DeleteOptions{PropagationPolicy: &foregroundPolicy},
			strategy:           defaultDeleteStrategy,
			existingFinalizers: []string{metav1.FinalizerDeleteDependents},
			expectedFinalizers: []string{metav1.FinalizerDeleteDependents},
		},
		"existing deleteDependents finalizer, PropagationPolicy=Foreground, orphanDeleteStrategy": {
			options:            &metav1.DeleteOptions{PropagationPolicy: &foregroundPolicy},
			strategy:           orphanDeleteStrategy,
			existingFinalizers: []string{metav1.FinalizerDeleteDependents},
			expectedFinalizers: []string{metav1.FinalizerDeleteDependents},
		},
		"existing deleteDependents finalizer, PropagationPolicy=Background, defaultDeleteStrategy": {
			options:            &metav1.DeleteOptions{PropagationPolicy: &backgroundPolicy},
			strategy:           defaultDeleteStrategy,
			existingFinalizers: []string{metav1.FinalizerDeleteDependents},
			expectedNotFound:   true,
		},
		"existing deleteDependents finalizer, PropagationPolicy=Background, orphanDeleteStrategy": {
			options:            &metav1.DeleteOptions{PropagationPolicy: &backgroundPolicy},
			strategy:           orphanDeleteStrategy,
			existingFinalizers: []string{metav1.FinalizerDeleteDependents},
			expectedNotFound:   true,
		},
		"existing deleteDependents finalizer, PropagationPolicy=OrphanDependents, defaultDeleteStrategy": {
			options:            &metav1.DeleteOptions{PropagationPolicy: &orphanPolicy},
			strategy:           defaultDeleteStrategy,
			existingFinalizers: []string{metav1.FinalizerDeleteDependents},
			expectedFinalizers: []string{metav1.FinalizerOrphanDependents},
		},
		"existing deleteDependents finalizer, PropagationPolicy=OrphanDependents, orphanDeleteStrategy": {
			options:            &metav1.DeleteOptions{PropagationPolicy: &orphanPolicy},
			strategy:           orphanDeleteStrategy,
			existingFinalizers: []string{metav1.FinalizerDeleteDependents},
			expectedFinalizers: []string{metav1.FinalizerOrphanDependents},
		},
		"existing deleteDependents finalizer, PropagationPolicy=Default, defaultDeleteStrategy": {
			options:            &metav1.DeleteOptions{PropagationPolicy: nil},
			strategy:           defaultDeleteStrategy,
			existingFinalizers: []string{metav1.FinalizerDeleteDependents},
			expectedFinalizers: []string{metav1.FinalizerDeleteDependents},
		},
		"existing deleteDependents finalizer, PropagationPolicy=Default, orphanDeleteStrategy": {
			options:            &metav1.DeleteOptions{PropagationPolicy: nil},
			strategy:           orphanDeleteStrategy,
			existingFinalizers: []string{metav1.FinalizerDeleteDependents},
			expectedFinalizers: []string{metav1.FinalizerDeleteDependents},
		},
	}

	testContext := genericapirequest.WithNamespace(genericapirequest.NewContext(), "test")
	destroyFunc, registry := NewTestGenericStoreRegistry(t)
	registry.EnableGarbageCollection = true
	defer destroyFunc()

	createPod := func(i int, finalizers []string) *example.Pod {
		return &example.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("pod-%d", i), Finalizers: finalizers, Generation: initialGeneration},
			Spec:       example.PodSpec{NodeName: "machine"},
		}
	}

	i := 0
	for title, tc := range testcases {
		t.Logf("case title: %s", title)
		registry.DeleteStrategy = tc.strategy
		i++
		pod := createPod(i, tc.existingFinalizers)
		// create pod
		_, err := registry.Create(testContext, pod, rest.ValidateAllObjectFunc, &metav1.CreateOptions{})
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}
		_, _, err = registry.Delete(testContext, pod.Name, rest.ValidateAllObjectFunc, tc.options)
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}
		obj, err := registry.Get(testContext, pod.Name, &metav1.GetOptions{})
		if tc.expectedNotFound {
			if err == nil || !errors.IsNotFound(err) {
				t.Fatalf("Unexpected error: %v", err)
			}
			continue
		}
		if !tc.expectedNotFound && err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}
		if !tc.expectedNotFound {
			pod, ok := obj.(*example.Pod)
			if !ok {
				t.Fatalf("Expect the object to be a pod, but got %#v", obj)
			}
			if e, a := tc.expectedFinalizers, pod.ObjectMeta.Finalizers; !reflect.DeepEqual(e, a) {
				t.Errorf("%v: Expect object %s to have finalizers %v, got %v", pod.Name, pod.ObjectMeta.Name, e, a)
			}
			if pod.ObjectMeta.DeletionTimestamp == nil {
				t.Errorf("%v: Expect the object to have DeletionTimestamp set, but got %#v", pod.Name, pod.ObjectMeta)
			}
			if pod.ObjectMeta.DeletionGracePeriodSeconds == nil || *pod.ObjectMeta.DeletionGracePeriodSeconds != 0 {
				t.Errorf("%v: Expect the object to have 0 DeletionGracePeriodSecond, but got %#v", pod.Name, pod.ObjectMeta)
			}
			if pod.Generation <= initialGeneration {
				t.Errorf("%v: Deletion didn't increase Generation.", pod.Name)
			}
		}
	}
}

type storageWithCounter struct {
	storage.Interface

	listCounter int64
}

func (s *storageWithCounter) GetList(ctx context.Context, key string, opts storage.ListOptions, listObj runtime.Object) error {
	atomic.AddInt64(&s.listCounter, 1)
	return s.Interface.GetList(ctx, key, opts, listObj)
}

func TestStoreDeleteCollection(t *testing.T) {
	testContext := genericapirequest.WithNamespace(genericapirequest.NewContext(), "test")
	destroyFunc, registry := NewTestGenericStoreRegistry(t)
	defer destroyFunc()

	// Overwrite the underlying storage interface so that it counts GetList calls
	// and reduce the default page size to 2.
	storeWithCounter := &storageWithCounter{Interface: registry.Storage.Storage}
	registry.Storage.Storage = storeWithCounter
	originalDeleteCollectionPageSize := deleteCollectionPageSize
	deleteCollectionPageSize = 2
	defer func() {
		deleteCollectionPageSize = originalDeleteCollectionPageSize
	}()

	numPods := 10
	for i := 0; i < numPods; i++ {
		pod := &example.Pod{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("foo-%d", i)}}
		if _, err := registry.Create(testContext, pod, rest.ValidateAllObjectFunc, &metav1.CreateOptions{}); err != nil {
			t.Errorf("Unexpected error: %v", err)
		}
	}

	// Delete all pods.
	deleted, err := registry.DeleteCollection(testContext, rest.ValidateAllObjectFunc, nil, &metainternalversion.ListOptions{})
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	deletedPods := deleted.(*example.PodList)
	if len(deletedPods.Items) != numPods {
		t.Errorf("Unexpected number of pods deleted: %d, expected: %d", len(deletedPods.Items), numPods)
	}
	expectedCalls := (int64(numPods) + deleteCollectionPageSize - 1) / deleteCollectionPageSize
	if listCalls := atomic.LoadInt64(&storeWithCounter.listCounter); listCalls != expectedCalls {
		t.Errorf("Unexpected number of list calls: %d, expected: %d", listCalls, expectedCalls)
	}

	for i := 0; i < numPods; i++ {
		if _, err := registry.Get(testContext, fmt.Sprintf("foo-%d", i), &metav1.GetOptions{}); !errors.IsNotFound(err) {
			t.Errorf("Unexpected error: %v", err)
		}
	}
}

func TestStoreDeleteCollectionNoMutateOptions(t *testing.T) {
	podA := &example.Pod{ObjectMeta: metav1.ObjectMeta{Name: "foo"}}
	podB := &example.Pod{ObjectMeta: metav1.ObjectMeta{Name: "bar"}}

	testContext := genericapirequest.WithNamespace(genericapirequest.NewContext(), "test")
	destroyFunc, registry := NewTestGenericStoreRegistry(t)
	registry.DeleteStrategy = &mutatingDeleteRESTStrategy{scheme}
	defer destroyFunc()

	if _, err := registry.Create(testContext, podA, rest.ValidateAllObjectFunc, &metav1.CreateOptions{}); err != nil {
		t.Errorf("Unexpected error: %v", err)
	}
	if _, err := registry.Create(testContext, podB, rest.ValidateAllObjectFunc, &metav1.CreateOptions{}); err != nil {
		t.Errorf("Unexpected error: %v", err)
	}

	n := int64(50)
	inputDeleteOptions := &metav1.DeleteOptions{GracePeriodSeconds: &n}
	safeCopyOfDelete := inputDeleteOptions.DeepCopy()
	// Delete all pods.
	_, err := registry.DeleteCollection(testContext, rest.ValidateAllObjectFunc, inputDeleteOptions, &metainternalversion.ListOptions{})
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	if !reflect.DeepEqual(inputDeleteOptions, safeCopyOfDelete) {
		t.Error(inputDeleteOptions)
	}
}

func TestStoreDeleteCollectionNotFound(t *testing.T) {
	destroyFunc, registry := NewTestGenericStoreRegistry(t)
	defer destroyFunc()

	testContext := genericapirequest.WithNamespace(genericapirequest.NewContext(), "test")

	podA := &example.Pod{ObjectMeta: metav1.ObjectMeta{Name: "foo"}}
	podB := &example.Pod{ObjectMeta: metav1.ObjectMeta{Name: "bar"}}

	for i := 0; i < 10; i++ {
		// Setup
		if _, err := registry.Create(testContext, podA, rest.ValidateAllObjectFunc, &metav1.CreateOptions{}); err != nil {
			t.Errorf("Unexpected error: %v", err)
		}
		if _, err := registry.Create(testContext, podB, rest.ValidateAllObjectFunc, &metav1.CreateOptions{}); err != nil {
			t.Errorf("Unexpected error: %v", err)
		}

		// Kick off multiple delete collection calls to test notfound behavior
		wg := &sync.WaitGroup{}
		for j := 0; j < 2; j++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, err := registry.DeleteCollection(testContext, rest.ValidateAllObjectFunc, nil, &metainternalversion.ListOptions{})
				if err != nil {
					t.Errorf("Unexpected error: %v", err)
				}
			}()
		}
		wg.Wait()

		if _, err := registry.Get(testContext, podA.Name, &metav1.GetOptions{}); !errors.IsNotFound(err) {
			t.Errorf("Unexpected error: %v", err)
		}
		if _, err := registry.Get(testContext, podB.Name, &metav1.GetOptions{}); !errors.IsNotFound(err) {
			t.Errorf("Unexpected error: %v", err)
		}
	}
}

func TestStoreDeleteCollectionWorkDistributorExited(t *testing.T) {
	destroyFunc, registry := NewTestGenericStoreRegistry(t)
	defer destroyFunc()

	testContext := genericapirequest.WithNamespace(genericapirequest.NewContext(), "test")

	for i := 0; i < 100; i++ {
		if _, err := registry.Create(
			testContext,
			&example.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: fmt.Sprintf("foo-%d", i),
				},
			},
			rest.ValidateAllObjectFunc,
			&metav1.CreateOptions{},
		); err != nil {
			t.Errorf("Unexpected error: %v", err)
		}
	}

	expectErr := fmt.Errorf("validate object failed")

	_, err := registry.DeleteCollection(testContext, func(ctx context.Context, obj runtime.Object) error {
		return expectErr
	}, nil, &metainternalversion.ListOptions{})
	if err != expectErr {
		t.Errorf("Unexpected error: %v", err)
	}
}

func TestStoreDeleteCollectionWithContextCancellation(t *testing.T) {
	destroyFunc, registry := NewTestGenericStoreRegistry(t)
	defer destroyFunc()

	testContext := genericapirequest.WithNamespace(genericapirequest.NewContext(), "test")

	for i := 0; i < 100; i++ {
		if _, err := registry.Create(
			testContext,
			&example.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: fmt.Sprintf("foo-%d", i),
				},
			},
			rest.ValidateAllObjectFunc,
			&metav1.CreateOptions{},
		); err != nil {
			t.Errorf("Unexpected error: %v", err)
		}
	}

	ctx, cancel := context.WithCancel(testContext)

	lock := sync.Mutex{}
	called := false

	// We rely on the fact that there is exactly one worker, so it should exit after
	// getting context canceled error on the first Delete call to etcd.
	// With multiple workers, each of them would be calling Delete once.
	_, err := registry.DeleteCollection(ctx, func(ctx context.Context, obj runtime.Object) error {
		lock.Lock()
		defer lock.Unlock()
		if called {
			t.Errorf("Delete called more than once, so context cancellation didn't work")
		} else {
			cancel()
			called = true
		}
		return nil
	}, nil, &metainternalversion.ListOptions{})
	if err != context.Canceled {
		t.Errorf("Unexpected error: %v", err)
	}
}

// Test whether objects deleted with DeleteCollection are correctly delivered
// to watchers.
func TestStoreDeleteCollectionWithWatch(t *testing.T) {
	podA := &example.Pod{ObjectMeta: metav1.ObjectMeta{Name: "foo"}}

	testContext := genericapirequest.WithNamespace(genericapirequest.NewContext(), "test")
	destroyFunc, registry := NewTestGenericStoreRegistry(t)
	defer destroyFunc()

	objCreated, err := registry.Create(testContext, podA, rest.ValidateAllObjectFunc, &metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	podCreated := objCreated.(*example.Pod)

	watcher, err := registry.WatchPredicate(testContext, matchPodName("foo"), podCreated.ResourceVersion, nil)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	defer watcher.Stop()

	if _, err := registry.DeleteCollection(testContext, rest.ValidateAllObjectFunc, nil, &metainternalversion.ListOptions{}); err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	got, open := <-watcher.ResultChan()
	if !open {
		t.Errorf("Unexpected channel close")
	} else {
		if got.Type != "DELETED" {
			t.Errorf("Unexpected event type: %s", got.Type)
		}
		gotObject := got.Object.(*example.Pod)
		gotObject.ResourceVersion = podCreated.ResourceVersion
		if e, a := podCreated, gotObject; !reflect.DeepEqual(e, a) {
			t.Errorf("Expected: %#v, got: %#v", e, a)
		}
	}
}

func TestStoreWatch(t *testing.T) {
	testContext := genericapirequest.WithNamespace(genericapirequest.NewContext(), "test")
	noNamespaceContext := genericapirequest.NewContext()

	table := map[string]struct {
		selectPred storage.SelectionPredicate
		context    context.Context
	}{
		"single": {
			selectPred: matchPodName("foo"),
		},
		"multi": {
			selectPred: matchPodName("foo", "bar"),
		},
		"singleNoNamespace": {
			selectPred: matchPodName("foo"),
			context:    noNamespaceContext,
		},
	}

	for name, m := range table {
		t.Run(name, func(t *testing.T) {
			ctx := testContext
			if m.context != nil {
				ctx = m.context
			}
			podA := &example.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "foo",
					Namespace: "test",
				},
				Spec: example.PodSpec{NodeName: "machine"},
			}

			destroyFunc, registry := NewTestGenericStoreRegistry(t)
			defer destroyFunc()
			wi, err := registry.WatchPredicate(ctx, m.selectPred, "0", nil)
			if err != nil {
				t.Errorf("%v: unexpected error: %v", name, err)
			} else {
				obj, err := registry.Create(testContext, podA, rest.ValidateAllObjectFunc, &metav1.CreateOptions{})
				if err != nil {
					got, open := <-wi.ResultChan()
					if !open {
						t.Errorf("%v: unexpected channel close", name)
					} else {
						if e, a := obj, got.Object; !reflect.DeepEqual(e, a) {
							t.Errorf("Expected %#v, got %#v", e, a)
						}
					}
				}
				wi.Stop()
			}
		})
	}
}

func newTestGenericStoreRegistry(t *testing.T, scheme *runtime.Scheme, hasCacheEnabled bool) (factory.DestroyFunc, *Store) {
	podPrefix := "/pods"
	server, sc := etcd3testing.NewUnsecuredEtcd3TestClientServer(t)
	strategy := &testRESTStrategy{scheme, names.SimpleNameGenerator, true, false, true}

	newFunc := func() runtime.Object { return &example.Pod{} }
	newListFunc := func() runtime.Object { return &example.PodList{} }

	sc.Codec = apitesting.TestStorageCodec(codecs, examplev1.SchemeGroupVersion)
	s, dFunc, err := factory.Create(*sc.ForResource(schema.GroupResource{Resource: "pods"}), newFunc, newListFunc, "/pods")
	if err != nil {
		t.Fatalf("Error creating storage: %v", err)
	}
	destroyFunc := func() {
		dFunc()
		server.Terminate(t)
	}
	if hasCacheEnabled {
		config := cacherstorage.Config{
			Storage:             s,
			Versioner:           storage.APIObjectVersioner{},
			GroupResource:       schema.GroupResource{Resource: "pods"},
			EventsHistoryWindow: cacherstorage.DefaultEventFreshDuration,
			ResourcePrefix:      podPrefix,
			KeyFunc:             func(obj runtime.Object) (string, error) { return storage.NoNamespaceKeyFunc(podPrefix, obj) },
			GetAttrsFunc:        getPodAttrs,
			NewFunc:             newFunc,
			NewListFunc:         newListFunc,
			Codec:               sc.Codec,
		}
		cacher, err := cacherstorage.NewCacherFromConfig(config)
		if err != nil {
			t.Fatalf("Couldn't create cacher: %v", err)
		}
		if utilfeature.DefaultFeatureGate.Enabled(features.ResilientWatchCacheInitialization) {
			// The tests assume that Get/GetList/Watch calls shouldn't fail.
			// However, 429 error can now be returned if watchcache is under initialization.
			// To avoid rewriting all tests, we wait for watchcache to initialize.
			if err := cacher.Wait(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
		d := destroyFunc
		s = cacherstorage.NewCacheProxy(cacher, s)
		destroyFunc = func() {
			cacher.Stop()
			d()
		}
	}

	return destroyFunc, &Store{
		NewFunc:                   func() runtime.Object { return &example.Pod{} },
		NewListFunc:               func() runtime.Object { return &example.PodList{} },
		DefaultQualifiedResource:  example.Resource("pods"),
		SingularQualifiedResource: example.Resource("pod"),
		CreateStrategy:            strategy,
		UpdateStrategy:            strategy,
		DeleteStrategy:            strategy,
		KeyRootFunc: func(ctx context.Context) string {
			return podPrefix
		},
		KeyFunc: func(ctx context.Context, id string) (string, error) {
			if _, ok := genericapirequest.NamespaceFrom(ctx); !ok {
				return "", fmt.Errorf("namespace is required")
			}
			return path.Join(podPrefix, id), nil
		},
		ObjectNameFunc: func(obj runtime.Object) (string, error) { return obj.(*example.Pod).Name, nil },
		PredicateFunc: func(label labels.Selector, field fields.Selector) storage.SelectionPredicate {
			return storage.SelectionPredicate{
				Label: label,
				Field: field,
				GetAttrs: func(obj runtime.Object) (labels.Set, fields.Set, error) {
					pod, ok := obj.(*example.Pod)
					if !ok {
						return nil, nil, fmt.Errorf("not a pod")
					}
					return labels.Set(pod.ObjectMeta.Labels), generic.ObjectMetaFieldsSet(&pod.ObjectMeta, true), nil
				},
			}
		},
		Storage: DryRunnableStorage{Storage: s},
	}
}

func TestFinalizeDelete(t *testing.T) {
	// Verify that it returns the expected Status.
	destroyFunc, s := NewTestGenericStoreRegistry(t)
	defer destroyFunc()
	obj := &example.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "foo", UID: "random-uid"},
	}
	result, err := s.finalizeDelete(genericapirequest.NewContext(), obj, false, &metav1.DeleteOptions{})
	if err != nil {
		t.Fatalf("unexpected err: %s", err)
	}
	returnedObj := result.(*metav1.Status)

	expectedObj := &metav1.Status{
		Status: metav1.StatusSuccess,
		Details: &metav1.StatusDetails{
			Name:  "foo",
			Group: s.DefaultQualifiedResource.Group,
			Kind:  s.DefaultQualifiedResource.Resource,
			UID:   "random-uid",
		},
	}
	if !apiequality.Semantic.DeepEqual(expectedObj, returnedObj) {
		t.Errorf("unexpected obj. expected %#v, got %#v", expectedObj, returnedObj)
	}
}

func fakeRequestInfo(resource, apiGroup string) *genericapirequest.RequestInfo {
	return &genericapirequest.RequestInfo{
		IsResourceRequest: true,
		Path:              "/api/v1/test",
		Verb:              "test",
		APIPrefix:         "api",
		APIGroup:          apiGroup,
		APIVersion:        "v1",
		Namespace:         "",
		Resource:          resource,
		Subresource:       "",
		Name:              "",
		Parts:             []string{"test"},
	}
}

func TestQualifiedResource(t *testing.T) {
	podA := &example.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "foo", Namespace: "test"},
		Spec:       example.PodSpec{NodeName: "machine"},
	}

	qualifiedKind := "pod"
	qualifiedGroup := "test"
	testContext := genericapirequest.WithNamespace(genericapirequest.NewContext(), "test")
	testContext = genericapirequest.WithRequestInfo(testContext, fakeRequestInfo(qualifiedKind, qualifiedGroup))

	destroyFunc, registry := NewTestGenericStoreRegistry(t)
	defer destroyFunc()

	// update a non-exist object
	_, _, err := registry.Update(testContext, podA.Name, rest.DefaultUpdatedObjectInfo(podA), rest.ValidateAllObjectFunc, rest.ValidateAllObjectUpdateFunc, false, &metav1.UpdateOptions{})
	if !errors.IsNotFound(err) {
		t.Fatalf("Unexpected error: %v", err)
	}

	if !isQualifiedResource(err, qualifiedKind, qualifiedGroup) {
		t.Fatalf("Unexpected error: %#v", err)
	}

	// get a non-exist object
	_, err = registry.Get(testContext, podA.Name, &metav1.GetOptions{})

	if !errors.IsNotFound(err) {
		t.Fatalf("Unexpected error: %v", err)
	}

	if !isQualifiedResource(err, qualifiedKind, qualifiedGroup) {
		t.Fatalf("Unexpected error: %#v", err)
	}

	// delete a non-exist object
	_, _, err = registry.Delete(testContext, podA.Name, rest.ValidateAllObjectFunc, nil)

	if !errors.IsNotFound(err) {
		t.Fatalf("Unexpected error: %v", err)
	}

	if !isQualifiedResource(err, qualifiedKind, qualifiedGroup) {
		t.Fatalf("Unexpected error: %#v", err)
	}

	// create a non-exist object
	_, err = registry.Create(testContext, podA, rest.ValidateAllObjectFunc, &metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// create a exist object will fail
	_, err = registry.Create(testContext, podA, rest.ValidateAllObjectFunc, &metav1.CreateOptions{})
	if !errors.IsAlreadyExists(err) {
		t.Fatalf("Unexpected error: %v", err)
	}

	if !isQualifiedResource(err, qualifiedKind, qualifiedGroup) {
		t.Fatalf("Unexpected error: %#v", err)
	}
}

func denyCreateValidation(ctx context.Context, obj runtime.Object) error {
	return fmt.Errorf("admission denied")
}

func denyUpdateValidation(ctx context.Context, obj, old runtime.Object) error {
	return fmt.Errorf("admission denied")
}

type fakeStrategy struct {
	runtime.ObjectTyper
	names.NameGenerator
}

func (fakeStrategy) DefaultGarbageCollectionPolicy(ctx context.Context) rest.GarbageCollectionPolicy {
	appsv1beta1 := schema.GroupVersion{Group: "apps", Version: "v1beta1"}
	appsv1beta2 := schema.GroupVersion{Group: "apps", Version: "v1beta2"}
	extensionsv1beta1 := schema.GroupVersion{Group: "extensions", Version: "v1beta1"}
	if requestInfo, found := genericapirequest.RequestInfoFrom(ctx); found {
		groupVersion := schema.GroupVersion{Group: requestInfo.APIGroup, Version: requestInfo.APIVersion}
		switch groupVersion {
		case appsv1beta1, appsv1beta2, extensionsv1beta1:
			// for back compatibility
			return rest.OrphanDependents
		default:
			return rest.DeleteDependents
		}
	}
	return rest.OrphanDependents
}

func TestDeletionFinalizersForGarbageCollection(t *testing.T) {
	destroyFunc, registry := NewTestGenericStoreRegistry(t)
	defer destroyFunc()

	registry.DeleteStrategy = fakeStrategy{}
	registry.EnableGarbageCollection = true

	tests := []struct {
		requestInfo       genericapirequest.RequestInfo
		desiredFinalizers []string
		isNilRequestInfo  bool
		changed           bool
	}{
		{
			genericapirequest.RequestInfo{
				APIGroup:   "extensions",
				APIVersion: "v1beta1",
			},
			[]string{metav1.FinalizerOrphanDependents},
			false,
			true,
		},
		{
			genericapirequest.RequestInfo{
				APIGroup:   "apps",
				APIVersion: "v1beta1",
			},
			[]string{metav1.FinalizerOrphanDependents},
			false,
			true,
		},
		{
			genericapirequest.RequestInfo{
				APIGroup:   "apps",
				APIVersion: "v1beta2",
			},
			[]string{metav1.FinalizerOrphanDependents},
			false,
			true,
		},
		{
			genericapirequest.RequestInfo{
				APIGroup:   "apps",
				APIVersion: "v1",
			},
			[]string{},
			false,
			false,
		},
	}

	for _, test := range tests {
		context := genericapirequest.NewContext()
		if !test.isNilRequestInfo {
			context = genericapirequest.WithRequestInfo(context, &test.requestInfo)
		}
		changed, finalizers := deletionFinalizersForGarbageCollection(context, registry, &example.ReplicaSet{}, &metav1.DeleteOptions{})
		if !changed {
			if test.changed {
				t.Errorf("%s/%s: no new finalizers are added", test.requestInfo.APIGroup, test.requestInfo.APIVersion)
			}
		} else if !reflect.DeepEqual(finalizers, test.desiredFinalizers) {
			t.Errorf("%s/%s: want %#v, got %#v", test.requestInfo.APIGroup, test.requestInfo.APIVersion,
				test.desiredFinalizers, finalizers)
		}
	}
}

func TestMarkAsDeleting(t *testing.T) {
	now := time.Now()
	soon := now.Add(time.Second)
	past := now.Add(-time.Second)

	newTimePointer := func(t time.Time) *metav1.Time {
		metaTime := metav1.NewTime(t)
		return &metaTime
	}
	testcases := []struct {
		name                    string
		deletionTimestamp       *metav1.Time
		expectDeletionTimestamp *metav1.Time
	}{
		{
			name:                    "unset",
			deletionTimestamp:       nil,
			expectDeletionTimestamp: newTimePointer(now),
		},
		{
			name:                    "set to future",
			deletionTimestamp:       newTimePointer(soon),
			expectDeletionTimestamp: newTimePointer(now),
		},
		{
			name:                    "set to past",
			deletionTimestamp:       newTimePointer(past),
			expectDeletionTimestamp: newTimePointer(past),
		},
	}
	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			rs := &example.ReplicaSet{}
			rs.DeletionTimestamp = tc.deletionTimestamp
			if err := markAsDeleting(rs, now); err != nil {
				t.Error(err)
			}
			if reflect.DeepEqual(*rs.DeletionTimestamp, tc.expectDeletionTimestamp) {
				t.Errorf("expected %v, got %v", tc.expectDeletionTimestamp, *rs.DeletionTimestamp)
			}
		})
	}
}

type staleGuaranteedUpdateStorage struct {
	storage.Interface
	cachedObj runtime.Object
}

// GuaranteedUpdate overwrites the method with one that always suggests the cachedObj.
func (s *staleGuaranteedUpdateStorage) GuaranteedUpdate(
	ctx context.Context, key string, destination runtime.Object, ignoreNotFound bool,
	preconditions *storage.Preconditions, tryUpdate storage.UpdateFunc, _ runtime.Object) error {
	return s.Interface.GuaranteedUpdate(ctx, key, destination, ignoreNotFound, preconditions, tryUpdate, s.cachedObj)
}

func TestDeleteWithCachedObject(t *testing.T) {
	podName := "foo"
	podWithFinalizer := &example.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: podName, Finalizers: []string{"foo.com/x"}},
		Spec:       example.PodSpec{NodeName: "machine"},
	}
	podWithNoFinalizer := &example.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: podName},
		Spec:       example.PodSpec{NodeName: "machine"},
	}
	ctx := genericapirequest.WithNamespace(genericapirequest.NewContext(), "test")
	destroyFunc, registry := newTestGenericStoreRegistry(t, scheme, false)
	defer destroyFunc()
	// cached object does not have any finalizer.
	registry.Storage.Storage = &staleGuaranteedUpdateStorage{Interface: registry.Storage.Storage, cachedObj: podWithNoFinalizer}
	// created object with pending finalizer.
	_, err := registry.Create(ctx, podWithFinalizer, rest.ValidateAllObjectFunc, &metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// The object shouldn't be deleted, because the persisted object has pending finalizers.
	_, _, err = registry.Delete(ctx, podName, rest.ValidateAllObjectFunc, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The object should still be there
	_, err = registry.Get(ctx, podName, &metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
}

func TestPreconditionalUpdateWithCachedObject(t *testing.T) {
	podName := "foo"
	pod := &example.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: podName},
		Spec:       example.PodSpec{NodeName: "machine"},
	}
	ctx := genericapirequest.WithNamespace(genericapirequest.NewContext(), "test")
	destroyFunc, registry := newTestGenericStoreRegistry(t, scheme, false)
	defer destroyFunc()

	// cached object has old UID
	oldPod, err := registry.Create(ctx, pod, rest.ValidateAllObjectFunc, &metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	registry.Storage.Storage = &staleGuaranteedUpdateStorage{Interface: registry.Storage.Storage, cachedObj: oldPod}

	// delete and re-create the same object with new UID
	_, _, err = registry.Delete(ctx, podName, rest.ValidateAllObjectFunc, nil)
	if err != nil {
		t.Fatal(err)
	}
	obj, err := registry.Create(ctx, pod, rest.ValidateAllObjectFunc, &metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	newPod, ok := obj.(*example.Pod)
	if !ok {
		t.Fatalf("unexpected object: %#v", obj)
	}

	// update the object should not fail precondition
	newPod.Spec.NodeName = "machine2"
	res, _, err := registry.Update(ctx, podName, rest.DefaultUpdatedObjectInfo(newPod), rest.ValidateAllObjectFunc, rest.ValidateAllObjectUpdateFunc, false, &metav1.UpdateOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// the update should have succeeded
	r, ok := res.(*example.Pod)
	if !ok {
		t.Fatalf("unexpected update result: %#v", res)
	}
	if r.Spec.NodeName != "machine2" {
		t.Fatalf("unexpected, update didn't take effect: %#v", r)
	}
}

// TestRetryDeleteValidation checks if the deleteValidation is called again if
// the GuaranteedUpdate in the Delete handler conflicts with a simultaneous
// Update.
func TestRetryDeleteValidation(t *testing.T) {
	testContext := genericapirequest.WithNamespace(genericapirequest.NewContext(), "test")
	destroyFunc, registry := NewTestGenericStoreRegistry(t)
	defer destroyFunc()

	tests := []struct {
		pod     *example.Pod
		deleted bool
	}{
		{
			pod: &example.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "foo", Namespace: "test", Finalizers: []string{"pending"}},
				Spec:       example.PodSpec{NodeName: "machine"},
			},
			deleted: false,
		},

		{
			pod: &example.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "bar", Namespace: "test"},
				Spec:       example.PodSpec{NodeName: "machine"},
			},
			deleted: true,
		},
	}

	for _, test := range tests {
		ready := make(chan struct{})
		updated := make(chan struct{})
		var readyOnce, updatedOnce sync.Once
		var called int
		deleteValidation := func(ctx context.Context, obj runtime.Object) error {
			readyOnce.Do(func() {
				close(ready)
			})
			// wait for the update completes
			<-updated
			called++
			return nil
		}

		if _, err := registry.Create(testContext, test.pod, rest.ValidateAllObjectFunc, &metav1.CreateOptions{}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		transformer := func(ctx context.Context, newObj runtime.Object, oldObj runtime.Object) (transformedNewObj runtime.Object, err error) {
			<-ready
			pod, ok := newObj.(*example.Pod)
			if !ok {
				t.Fatalf("unexpected object %v", newObj)
			}
			pod.Labels = map[string]string{
				"modified": "true",
			}
			return pod, nil
		}

		go func() {
			// This update will cause the Delete to retry due to conflict.
			_, _, err := registry.Update(testContext, test.pod.Name, rest.DefaultUpdatedObjectInfo(test.pod, transformer), rest.ValidateAllObjectFunc, rest.ValidateAllObjectUpdateFunc, false, &metav1.UpdateOptions{})
			if err != nil {
				t.Error(err)
			}
			updatedOnce.Do(func() {
				close(updated)
			})
		}()

		_, deleted, err := registry.Delete(testContext, test.pod.Name, deleteValidation, &metav1.DeleteOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if a, e := deleted, test.deleted; a != e {
			t.Fatalf("expected deleted to be %v, got %v", e, a)
		}
		if called != 2 {
			t.Fatalf("expected deleteValidation to be called twice")
		}
	}
}

func emptyIndexFunc(obj interface{}) ([]string, error) {
	return []string{}, nil
}

func TestValidateIndexers(t *testing.T) {
	testcases := []struct {
		name          string
		indexers      *cache.Indexers
		expectedError bool
	}{
		{
			name:          "nil indexers",
			indexers:      nil,
			expectedError: false,
		},
		{
			name: "normal indexers",
			indexers: &cache.Indexers{
				"f:spec.nodeName":            emptyIndexFunc,
				"l:controller-revision-hash": emptyIndexFunc,
			},
			expectedError: false,
		},
		{
			name: "too short indexers",
			indexers: &cache.Indexers{
				"f": emptyIndexFunc,
			},
			expectedError: true,
		},
		{
			name: "invalid indexers",
			indexers: &cache.Indexers{
				"spec.nodeName": emptyIndexFunc,
			},
			expectedError: true,
		},
	}

	for _, tc := range testcases {
		err := validateIndexers(tc.indexers)
		if tc.expectedError && err == nil {
			t.Errorf("%v: expected error, but got nil", tc.name)
		}
		if !tc.expectedError && err != nil {
			t.Errorf("%v: expected no error, but got %v", tc.name, err)
		}
	}
}

type predictableNameGenerator struct {
	index int
}

func (p *predictableNameGenerator) GenerateName(base string) string {
	p.index++
	return fmt.Sprintf("%s%d", base, p.index)
}

func TestStoreCreateGenerateNameConflict(t *testing.T) {
	// Preserve testing of disabled RetryGenerateName feature gate since it can still be disabled when emulation version is set.
	featuregatetesting.SetFeatureGateEmulationVersionDuringTest(t, utilfeature.DefaultFeatureGate, version.MustParse("1.31"))
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.RetryGenerateName, false)

	// podA will be stored with name foo12345
	podA := &example.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "foo1", Namespace: "test"},
		Spec:       example.PodSpec{NodeName: "machine"},
	}
	// podB will generate the same name as podA "foo1" in the first try
	podB := &example.Pod{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "foo", Namespace: "test"},
		Spec:       example.PodSpec{NodeName: "machine2"},
	}

	testContext := genericapirequest.WithNamespace(genericapirequest.NewContext(), "test")
	destroyFunc, registry := NewTestGenericStoreRegistry(t)
	defer destroyFunc()
	// re-define delete strategy to have graceful delete capability
	defaultCreateStrategy := &testRESTStrategy{scheme, &predictableNameGenerator{}, true, false, true}
	registry.CreateStrategy = defaultCreateStrategy

	// create the object (DeepCopy because the registry mutates the objects)
	objA, err := registry.Create(testContext, podA.DeepCopy(), rest.ValidateAllObjectFunc, &metav1.CreateOptions{})
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}

	// get the object
	checkobjA, err := registry.Get(testContext, podA.Name, &metav1.GetOptions{})
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}

	// verify objects are equal
	if e, a := objA, checkobjA; !reflect.DeepEqual(e, a) {
		t.Errorf("Expected %#v, got %#v", e, a)
	}

	// now try to create the second pod (DeepCopy because the registry mutate the objects)
	_, err = registry.Create(testContext, podB.DeepCopy(), rest.ValidateAllObjectFunc, &metav1.CreateOptions{})
	if !errors.IsAlreadyExists(err) {
		t.Errorf("Unexpected error: %+v", err)
	}

	// check the 'already exists' msg correspond to the generated name
	msg := &err.(*errors.StatusError).ErrStatus.Message
	if !strings.Contains(*msg, "already exists, the server was not able to generate a unique name for the object") {
		t.Errorf("Unexpected error without the 'was not able to generate a unique name' in message: %v", err)
	}

	// now try to create the second pod again
	objB, err := registry.Create(testContext, podB.DeepCopy(), rest.ValidateAllObjectFunc, &metav1.CreateOptions{})
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}

	if objB.(*example.Pod).Name != "foo2" && objB.(*example.Pod).GenerateName != "foo" {
		t.Errorf("Unexpected object: %+v", objB)
	}

}
