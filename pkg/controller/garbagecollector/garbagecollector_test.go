/*
Copyright 2016 The Kubernetes Authors.

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

package garbagecollector

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/time/rate"

	"k8s.io/klog/v2"
	"k8s.io/utils/lru"

	"github.com/google/go-cmp/cmp"
	"github.com/stretchr/testify/assert"

	_ "k8s.io/kubernetes/pkg/apis/core/install"
	"k8s.io/kubernetes/pkg/controller/garbagecollector/metaonly"
	"k8s.io/utils/pointer"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/meta/testrestmapper"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/json"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/metadata"
	fakemetadata "k8s.io/client-go/metadata/fake"
	"k8s.io/client-go/metadata/metadatainformer"
	restclient "k8s.io/client-go/rest"
	clientgotesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	metricsutil "k8s.io/component-base/metrics/testutil"
	"k8s.io/controller-manager/pkg/informerfactory"
	"k8s.io/kubernetes/pkg/api/legacyscheme"
	c "k8s.io/kubernetes/pkg/controller"
	"k8s.io/kubernetes/pkg/controller/garbagecollector/metrics"
	"k8s.io/kubernetes/test/utils/ktesting"
)

type testRESTMapper struct {
	meta.RESTMapper
}

func (m *testRESTMapper) Reset() {
	meta.MaybeResetRESTMapper(m.RESTMapper)
}

func TestGarbageCollectorConstruction(t *testing.T) {
	config := &restclient.Config{}
	tweakableRM := meta.NewDefaultRESTMapper(nil)
	rm := &testRESTMapper{meta.MultiRESTMapper{tweakableRM, testrestmapper.TestOnlyStaticRESTMapper(legacyscheme.Scheme)}}
	metadataClient, err := metadata.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}

	podResource := map[schema.GroupVersionResource]struct{}{
		{Version: "v1", Resource: "pods"}: {},
	}
	twoResources := map[schema.GroupVersionResource]struct{}{
		{Version: "v1", Resource: "pods"}:                     {},
		{Group: "tpr.io", Version: "v1", Resource: "unknown"}: {},
	}
	client := fake.NewSimpleClientset()

	sharedInformers := informers.NewSharedInformerFactory(client, 0)
	metadataInformers := metadatainformer.NewSharedInformerFactory(metadataClient, 0)
	// No monitor will be constructed for the non-core resource, but the GC
	// construction will not fail.
	alwaysStarted := make(chan struct{})
	close(alwaysStarted)
	logger, tCtx := ktesting.NewTestContext(t)
	gc, err := NewGarbageCollector(tCtx, client, metadataClient, rm, map[schema.GroupResource]struct{}{},
		informerfactory.NewInformerFactory(sharedInformers, metadataInformers), alwaysStarted)
	if err != nil {
		t.Fatal(err)
	}
	assert.Empty(t, gc.dependencyGraphBuilder.monitors)

	// Make sure resource monitor syncing creates and stops resource monitors.
	tweakableRM.Add(schema.GroupVersionKind{Group: "tpr.io", Version: "v1", Kind: "unknown"}, nil)
	err = gc.resyncMonitors(logger, twoResources)
	if err != nil {
		t.Errorf("Failed adding a monitor: %v", err)
	}
	assert.Len(t, gc.dependencyGraphBuilder.monitors, 2)

	err = gc.resyncMonitors(logger, podResource)
	if err != nil {
		t.Errorf("Failed removing a monitor: %v", err)
	}
	assert.Len(t, gc.dependencyGraphBuilder.monitors, 1)

	go gc.Run(tCtx, 1, 5*time.Second)

	err = gc.resyncMonitors(logger, twoResources)
	if err != nil {
		t.Errorf("Failed adding a monitor: %v", err)
	}
	assert.Len(t, gc.dependencyGraphBuilder.monitors, 2)

	err = gc.resyncMonitors(logger, podResource)
	if err != nil {
		t.Errorf("Failed removing a monitor: %v", err)
	}
	assert.Len(t, gc.dependencyGraphBuilder.monitors, 1)
}

// fakeAction records information about requests to aid in testing.
type fakeAction struct {
	method string
	path   string
	query  string
}

// String returns method=path to aid in testing
func (f *fakeAction) String() string {
	return strings.Join([]string{f.method, f.path}, "=")
}

type FakeResponse struct {
	statusCode int
	content    []byte
}

// fakeActionHandler holds a list of fakeActions received
type fakeActionHandler struct {
	// statusCode and content returned by this handler for different method + path.
	response map[string]FakeResponse

	lock    sync.Mutex
	actions []fakeAction
}

// ServeHTTP logs the action that occurred and always returns the associated status code
func (f *fakeActionHandler) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	func() {
		f.lock.Lock()
		defer f.lock.Unlock()

		f.actions = append(f.actions, fakeAction{method: request.Method, path: request.URL.Path, query: request.URL.RawQuery})
		fakeResponse, ok := f.response[request.Method+request.URL.Path]
		if !ok {
			fakeResponse.statusCode = 200
			fakeResponse.content = []byte(`{"apiVersion": "v1", "kind": "List"}`)
		}
		response.Header().Set("Content-Type", "application/json")
		response.WriteHeader(fakeResponse.statusCode)
		response.Write(fakeResponse.content)
	}()

	// This is to allow the fakeActionHandler to simulate a watch being opened
	if strings.Contains(request.URL.RawQuery, "watch=true") {
		hijacker, ok := response.(http.Hijacker)
		if !ok {
			return
		}
		connection, _, err := hijacker.Hijack()
		if err != nil {
			return
		}
		defer connection.Close()
		time.Sleep(30 * time.Second)
	}
}

// testServerAndClientConfig returns a server that listens and a config that can reference it
func testServerAndClientConfig(handler func(http.ResponseWriter, *http.Request)) (*httptest.Server, *restclient.Config) {
	srv := httptest.NewServer(http.HandlerFunc(handler))
	config := &restclient.Config{
		Host: srv.URL,
	}
	return srv, config
}

type garbageCollector struct {
	*GarbageCollector
	stop chan struct{}
}

func setupGC(t *testing.T, config *restclient.Config) garbageCollector {
	_, ctx := ktesting.NewTestContext(t)
	metadataClient, err := metadata.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}

	client := fake.NewSimpleClientset()
	sharedInformers := informers.NewSharedInformerFactory(client, 0)
	alwaysStarted := make(chan struct{})
	close(alwaysStarted)
	gc, err := NewGarbageCollector(ctx, client, metadataClient, &testRESTMapper{testrestmapper.TestOnlyStaticRESTMapper(legacyscheme.Scheme)}, ignoredResources, sharedInformers, alwaysStarted)
	if err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	sharedInformers.Start(stop)
	return garbageCollector{gc, stop}
}

func getPod(podName string, ownerReferences []metav1.OwnerReference) *v1.Pod {
	return &v1.Pod{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Pod",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:            podName,
			Namespace:       "ns1",
			UID:             "456",
			OwnerReferences: ownerReferences,
		},
	}
}

func serilizeOrDie(t *testing.T, object interface{}) []byte {
	data, err := json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// test the attemptToDeleteItem function making the expected actions.
func TestAttemptToDeleteItem(t *testing.T) {
	pod := getPod("ToBeDeletedPod", []metav1.OwnerReference{
		{
			Kind:       "ReplicationController",
			Name:       "owner1",
			UID:        "123",
			APIVersion: "v1",
		},
	})
	testHandler := &fakeActionHandler{
		response: map[string]FakeResponse{
			"GET" + "/api/v1/namespaces/ns1/replicationcontrollers/owner1": {
				404,
				[]byte{},
			},
			"GET" + "/api/v1/namespaces/ns1/pods/ToBeDeletedPod": {
				200,
				serilizeOrDie(t, pod),
			},
		},
	}
	srv, clientConfig := testServerAndClientConfig(testHandler.ServeHTTP)
	defer srv.Close()

	gc := setupGC(t, clientConfig)
	defer close(gc.stop)

	item := &node{
		identity: objectReference{
			OwnerReference: metav1.OwnerReference{
				Kind:       pod.Kind,
				APIVersion: pod.APIVersion,
				Name:       pod.Name,
				UID:        pod.UID,
			},
			Namespace: pod.Namespace,
		},
		// owners are intentionally left empty. The attemptToDeleteItem routine should get the latest item from the server.
		owners:  nil,
		virtual: true,
	}
	err := gc.attemptToDeleteItem(context.TODO(), item)
	if err != nil {
		t.Errorf("Unexpected Error: %v", err)
	}
	if !item.virtual {
		t.Errorf("attemptToDeleteItem changed virtual to false unexpectedly")
	}
	expectedActionSet := sets.NewString()
	expectedActionSet.Insert("GET=/api/v1/namespaces/ns1/replicationcontrollers/owner1")
	expectedActionSet.Insert("DELETE=/api/v1/namespaces/ns1/pods/ToBeDeletedPod")
	expectedActionSet.Insert("GET=/api/v1/namespaces/ns1/pods/ToBeDeletedPod")

	actualActionSet := sets.NewString()
	for _, action := range testHandler.actions {
		actualActionSet.Insert(action.String())
	}
	if !expectedActionSet.Equal(actualActionSet) {
		t.Errorf("expected actions:\n%v\n but got:\n%v\nDifference:\n%v", expectedActionSet,
			actualActionSet, expectedActionSet.Difference(actualActionSet))
	}
}

// verifyGraphInvariants verifies that all of a node's owners list the node as a
// dependent and vice versa. uidToNode has all the nodes in the graph.
func verifyGraphInvariants(scenario string, uidToNode map[types.UID]*node, t *testing.T) {
	for myUID, node := range uidToNode {
		for dependentNode := range node.dependents {
			found := false
			for _, owner := range dependentNode.owners {
				if owner.UID == myUID {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("scenario: %s: node %s has node %s as a dependent, but it's not present in the latter node's owners list", scenario, node.identity, dependentNode.identity)
			}
		}

		for _, owner := range node.owners {
			ownerNode, ok := uidToNode[owner.UID]
			if !ok {
				// It's possible that the owner node doesn't exist
				continue
			}
			if _, ok := ownerNode.dependents[node]; !ok {
				t.Errorf("node %s has node %s as an owner, but it's not present in the latter node's dependents list", node.identity, ownerNode.identity)
			}
		}
	}
}

func createEvent(eventType eventType, selfUID string, owners []string) event {
	var ownerReferences []metav1.OwnerReference
	for i := 0; i < len(owners); i++ {
		ownerReferences = append(ownerReferences, metav1.OwnerReference{UID: types.UID(owners[i])})
	}
	return event{
		eventType: eventType,
		obj: &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				UID:             types.UID(selfUID),
				OwnerReferences: ownerReferences,
			},
		},
	}
}

func TestProcessEvent(t *testing.T) {
	var testScenarios = []struct {
		name string
		// a series of events that will be supplied to the
		// GraphBuilder.graphChanges.
		events []event
	}{
		{
			name: "test1",
			events: []event{
				createEvent(addEvent, "1", []string{}),
				createEvent(addEvent, "2", []string{"1"}),
				createEvent(addEvent, "3", []string{"1", "2"}),
			},
		},
		{
			name: "test2",
			events: []event{
				createEvent(addEvent, "1", []string{}),
				createEvent(addEvent, "2", []string{"1"}),
				createEvent(addEvent, "3", []string{"1", "2"}),
				createEvent(addEvent, "4", []string{"2"}),
				createEvent(deleteEvent, "2", []string{"doesn't matter"}),
			},
		},
		{
			name: "test3",
			events: []event{
				createEvent(addEvent, "1", []string{}),
				createEvent(addEvent, "2", []string{"1"}),
				createEvent(addEvent, "3", []string{"1", "2"}),
				createEvent(addEvent, "4", []string{"3"}),
				createEvent(updateEvent, "2", []string{"4"}),
			},
		},
		{
			name: "reverse test2",
			events: []event{
				createEvent(addEvent, "4", []string{"2"}),
				createEvent(addEvent, "3", []string{"1", "2"}),
				createEvent(addEvent, "2", []string{"1"}),
				createEvent(addEvent, "1", []string{}),
				createEvent(deleteEvent, "2", []string{"doesn't matter"}),
			},
		},
	}

	alwaysStarted := make(chan struct{})
	close(alwaysStarted)
	for _, scenario := range testScenarios {
		logger, _ := ktesting.NewTestContext(t)

		dependencyGraphBuilder := &GraphBuilder{
			informersStarted: alwaysStarted,
			graphChanges:     workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[*event]()),
			uidToNode: &concurrentUIDToNode{
				uidToNodeLock: sync.RWMutex{},
				uidToNode:     make(map[types.UID]*node),
			},
			attemptToDelete:  workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[*node]()),
			absentOwnerCache: NewReferenceCache(2),
		}
		for i := 0; i < len(scenario.events); i++ {
			dependencyGraphBuilder.graphChanges.Add(&scenario.events[i])
			dependencyGraphBuilder.processGraphChanges(logger)
			verifyGraphInvariants(scenario.name, dependencyGraphBuilder.uidToNode.uidToNode, t)
		}
	}
}

func BenchmarkReferencesDiffs(t *testing.B) {
	t.ReportAllocs()
	t.ResetTimer()
	for n := 0; n < t.N; n++ {
		old := []metav1.OwnerReference{{UID: "1"}, {UID: "2"}}
		new := []metav1.OwnerReference{{UID: "2"}, {UID: "3"}}
		referencesDiffs(old, new)
	}
}

// TestDependentsRace relies on golang's data race detector to check if there is
// data race among in the dependents field.
func TestDependentsRace(t *testing.T) {
	logger, _ := ktesting.NewTestContext(t)

	gc := setupGC(t, &restclient.Config{})
	defer close(gc.stop)

	const updates = 100
	owner := &node{dependents: make(map[*node]struct{})}
	ownerUID := types.UID("owner")
	gc.dependencyGraphBuilder.uidToNode.Write(owner)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < updates; i++ {
			dependent := &node{}
			gc.dependencyGraphBuilder.addDependentToOwners(logger, dependent, []metav1.OwnerReference{{UID: ownerUID}})
			gc.dependencyGraphBuilder.removeDependentFromOwners(dependent, []metav1.OwnerReference{{UID: ownerUID}})
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < updates; i++ {
			gc.attemptToOrphan.Add(owner)
			gc.processAttemptToOrphanWorker(logger)
		}
	}()
	wg.Wait()
}

func podToGCNode(pod *v1.Pod) *node {
	return &node{
		identity: objectReference{
			OwnerReference: metav1.OwnerReference{
				Kind:       pod.Kind,
				APIVersion: pod.APIVersion,
				Name:       pod.Name,
				UID:        pod.UID,
			},
			Namespace: pod.Namespace,
		},
		// owners are intentionally left empty. The attemptToDeleteItem routine should get the latest item from the server.
		owners: nil,
	}
}

func TestAbsentOwnerCache(t *testing.T) {
	rc1Pod1 := getPod("rc1Pod1", []metav1.OwnerReference{
		{
			Kind:       "ReplicationController",
			Name:       "rc1",
			UID:        "1",
			APIVersion: "v1",
			Controller: pointer.Bool(true),
		},
	})
	rc1Pod2 := getPod("rc1Pod2", []metav1.OwnerReference{
		{
			Kind:       "ReplicationController",
			Name:       "rc1",
			UID:        "1",
			APIVersion: "v1",
			Controller: pointer.Bool(false),
		},
	})
	rc2Pod1 := getPod("rc2Pod1", []metav1.OwnerReference{
		{
			Kind:       "ReplicationController",
			Name:       "rc2",
			UID:        "2",
			APIVersion: "v1",
		},
	})
	rc3Pod1 := getPod("rc3Pod1", []metav1.OwnerReference{
		{
			Kind:       "ReplicationController",
			Name:       "rc3",
			UID:        "3",
			APIVersion: "v1",
		},
	})
	testHandler := &fakeActionHandler{
		response: map[string]FakeResponse{
			"GET" + "/api/v1/namespaces/ns1/pods/rc1Pod1": {
				200,
				serilizeOrDie(t, rc1Pod1),
			},
			"GET" + "/api/v1/namespaces/ns1/pods/rc1Pod2": {
				200,
				serilizeOrDie(t, rc1Pod2),
			},
			"GET" + "/api/v1/namespaces/ns1/pods/rc2Pod1": {
				200,
				serilizeOrDie(t, rc2Pod1),
			},
			"GET" + "/api/v1/namespaces/ns1/pods/rc3Pod1": {
				200,
				serilizeOrDie(t, rc3Pod1),
			},
			"GET" + "/api/v1/namespaces/ns1/replicationcontrollers/rc1": {
				404,
				[]byte{},
			},
			"GET" + "/api/v1/namespaces/ns1/replicationcontrollers/rc2": {
				404,
				[]byte{},
			},
			"GET" + "/api/v1/namespaces/ns1/replicationcontrollers/rc3": {
				404,
				[]byte{},
			},
		},
	}
	srv, clientConfig := testServerAndClientConfig(testHandler.ServeHTTP)
	defer srv.Close()
	gc := setupGC(t, clientConfig)
	defer close(gc.stop)
	gc.absentOwnerCache = NewReferenceCache(2)
	gc.attemptToDeleteItem(context.TODO(), podToGCNode(rc1Pod1))
	gc.attemptToDeleteItem(context.TODO(), podToGCNode(rc2Pod1))
	// rc1 should already be in the cache, no request should be sent. rc1 should be promoted in the UIDCache
	gc.attemptToDeleteItem(context.TODO(), podToGCNode(rc1Pod2))
	// after this call, rc2 should be evicted from the UIDCache
	gc.attemptToDeleteItem(context.TODO(), podToGCNode(rc3Pod1))
	// check cache
	if !gc.absentOwnerCache.Has(objectReference{Namespace: "ns1", OwnerReference: metav1.OwnerReference{Kind: "ReplicationController", Name: "rc1", UID: "1", APIVersion: "v1"}}) {
		t.Errorf("expected rc1 to be in the cache")
	}
	if gc.absentOwnerCache.Has(objectReference{Namespace: "ns1", OwnerReference: metav1.OwnerReference{Kind: "ReplicationController", Name: "rc2", UID: "2", APIVersion: "v1"}}) {
		t.Errorf("expected rc2 to not exist in the cache")
	}
	if !gc.absentOwnerCache.Has(objectReference{Namespace: "ns1", OwnerReference: metav1.OwnerReference{Kind: "ReplicationController", Name: "rc3", UID: "3", APIVersion: "v1"}}) {
		t.Errorf("expected rc3 to be in the cache")
	}
	// check the request sent to the server
	count := 0
	for _, action := range testHandler.actions {
		if action.String() == "GET=/api/v1/namespaces/ns1/replicationcontrollers/rc1" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected only 1 GET rc1 request, got %d", count)
	}
}

func TestDeleteOwnerRefPatch(t *testing.T) {
	original := v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			UID: "100",
			OwnerReferences: []metav1.OwnerReference{
				{UID: "1"},
				{UID: "2"},
				{UID: "3"},
			},
		},
	}
	originalData := serilizeOrDie(t, original)
	expected := v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			UID: "100",
			OwnerReferences: []metav1.OwnerReference{
				{UID: "1"},
			},
		},
	}
	p, err := c.GenerateDeleteOwnerRefStrategicMergeBytes("100", []types.UID{"2", "3"})
	if err != nil {
		t.Fatal(err)
	}
	patched, err := strategicpatch.StrategicMergePatch(originalData, p, v1.Pod{})
	if err != nil {
		t.Fatal(err)
	}
	var got v1.Pod
	if err := json.Unmarshal(patched, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(expected, got) {
		t.Errorf("expected: %#v,\ngot: %#v", expected, got)
	}
}

func TestUnblockOwnerReference(t *testing.T) {
	trueVar := true
	falseVar := false
	original := v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			UID: "100",
			OwnerReferences: []metav1.OwnerReference{
				{UID: "1", BlockOwnerDeletion: &trueVar},
				{UID: "2", BlockOwnerDeletion: &falseVar},
				{UID: "3"},
			},
		},
	}
	originalData := serilizeOrDie(t, original)
	expected := v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			UID: "100",
			OwnerReferences: []metav1.OwnerReference{
				{UID: "1", BlockOwnerDeletion: &falseVar},
				{UID: "2", BlockOwnerDeletion: &falseVar},
				{UID: "3"},
			},
		},
	}
	accessor, err := meta.Accessor(&original)
	if err != nil {
		t.Fatal(err)
	}
	n := node{
		owners: accessor.GetOwnerReferences(),
	}
	patch, err := n.unblockOwnerReferencesStrategicMergePatch()
	if err != nil {
		t.Fatal(err)
	}
	patched, err := strategicpatch.StrategicMergePatch(originalData, patch, v1.Pod{})
	if err != nil {
		t.Fatal(err)
	}
	var got v1.Pod
	if err := json.Unmarshal(patched, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(expected, got) {
		t.Errorf("expected: %#v,\ngot: %#v", expected, got)
		t.Errorf("expected: %#v,\ngot: %#v", expected.OwnerReferences, got.OwnerReferences)
		for _, ref := range got.OwnerReferences {
			t.Errorf("ref.UID=%s, ref.BlockOwnerDeletion=%v", ref.UID, *ref.BlockOwnerDeletion)
		}
	}
}

func TestOrphanDependentsFailure(t *testing.T) {
	logger, _ := ktesting.NewTestContext(t)

	testHandler := &fakeActionHandler{
		response: map[string]FakeResponse{
			"PATCH" + "/api/v1/namespaces/ns1/pods/pod": {
				409,
				[]byte{},
			},
		},
	}
	srv, clientConfig := testServerAndClientConfig(testHandler.ServeHTTP)
	defer srv.Close()

	gc := setupGC(t, clientConfig)
	defer close(gc.stop)

	dependents := []*node{
		{
			identity: objectReference{
				OwnerReference: metav1.OwnerReference{
					Kind:       "Pod",
					APIVersion: "v1",
					Name:       "pod",
				},
				Namespace: "ns1",
			},
		},
	}
	err := gc.orphanDependents(logger, objectReference{}, dependents)
	expected := `the server reported a conflict`
	if err == nil || !strings.Contains(err.Error(), expected) {
		if err != nil {
			t.Errorf("expected error contains text %q, got %q", expected, err.Error())
		} else {
			t.Errorf("expected error contains text %q, got nil", expected)
		}
	}
}

// TestGetDeletableResources ensures GetDeletableResources always returns
// something usable regardless of discovery output.
func TestGetDeletableResources(t *testing.T) {
	tests := map[string]struct {
		serverResources    []*metav1.APIResourceList
		err                error
		deletableResources map[schema.GroupVersionResource]struct{}
	}{
		"no error": {
			serverResources: []*metav1.APIResourceList{
				{
					// Valid GroupVersion
					GroupVersion: "apps/v1",
					APIResources: []metav1.APIResource{
						{Name: "pods", Namespaced: true, Kind: "Pod", Verbs: metav1.Verbs{"delete", "list", "watch"}},
						{Name: "services", Namespaced: true, Kind: "Service"},
					},
				},
				{
					// Invalid GroupVersion, should be ignored
					GroupVersion: "foo//whatever",
					APIResources: []metav1.APIResource{
						{Name: "bars", Namespaced: true, Kind: "Bar", Verbs: metav1.Verbs{"delete", "list", "watch"}},
					},
				},
				{
					// Valid GroupVersion, missing required verbs, should be ignored
					GroupVersion: "acme/v1",
					APIResources: []metav1.APIResource{
						{Name: "widgets", Namespaced: true, Kind: "Widget", Verbs: metav1.Verbs{"delete"}},
					},
				},
			},
			err: nil,
			deletableResources: map[schema.GroupVersionResource]struct{}{
				{Group: "apps", Version: "v1", Resource: "pods"}: {},
			},
		},
		"nonspecific failure, includes usable results": {
			serverResources: []*metav1.APIResourceList{
				{
					GroupVersion: "apps/v1",
					APIResources: []metav1.APIResource{
						{Name: "pods", Namespaced: true, Kind: "Pod", Verbs: metav1.Verbs{"delete", "list", "watch"}},
						{Name: "services", Namespaced: true, Kind: "Service"},
					},
				},
			},
			err: fmt.Errorf("internal error"),
			deletableResources: map[schema.GroupVersionResource]struct{}{
				{Group: "apps", Version: "v1", Resource: "pods"}: {},
			},
		},
		"partial discovery failure, includes usable results": {
			serverResources: []*metav1.APIResourceList{
				{
					GroupVersion: "apps/v1",
					APIResources: []metav1.APIResource{
						{Name: "pods", Namespaced: true, Kind: "Pod", Verbs: metav1.Verbs{"delete", "list", "watch"}},
						{Name: "services", Namespaced: true, Kind: "Service"},
					},
				},
			},
			err: &discovery.ErrGroupDiscoveryFailed{
				Groups: map[schema.GroupVersion]error{
					{Group: "foo", Version: "v1"}: fmt.Errorf("discovery failure"),
				},
			},
			deletableResources: map[schema.GroupVersionResource]struct{}{
				{Group: "apps", Version: "v1", Resource: "pods"}: {},
			},
		},
		"discovery failure, no results": {
			serverResources:    nil,
			err:                fmt.Errorf("internal error"),
			deletableResources: map[schema.GroupVersionResource]struct{}{},
		},
	}

	logger, _ := ktesting.NewTestContext(t)
	for name, test := range tests {
		t.Logf("testing %q", name)
		client := &fakeServerResources{
			PreferredResources: test.serverResources,
			Error:              test.err,
		}
		actual, actualErr := GetDeletableResources(logger, client)
		if !reflect.DeepEqual(test.deletableResources, actual) {
			t.Errorf("expected resources:\n%v\ngot:\n%v", test.deletableResources, actual)
		}
		if !reflect.DeepEqual(test.err, actualErr) {
			t.Errorf("expected error:\n%v\ngot:\n%v", test.err, actualErr)
		}
	}
}

// TestGarbageCollectorSync ensures that a discovery client error
// or an informer sync error will not cause the garbage collector
// to block infinitely.
func TestGarbageCollectorSync(t *testing.T) {
	serverResources := []*metav1.APIResourceList{
		{
			GroupVersion: "v1",
			APIResources: []metav1.APIResource{
				{Name: "pods", Namespaced: true, Kind: "Pod", Verbs: metav1.Verbs{"delete", "list", "watch"}},
			},
		},
		{
			GroupVersion: "apps/v1",
			APIResources: []metav1.APIResource{
				{Name: "deployments", Namespaced: true, Kind: "Deployment", Verbs: metav1.Verbs{"delete", "list", "watch"}},
			},
		},
	}
	appsV1Error := &discovery.ErrGroupDiscoveryFailed{Groups: map[schema.GroupVersion]error{{Group: "apps", Version: "v1"}: fmt.Errorf(":-/")}}

	unsyncableServerResources := []*metav1.APIResourceList{
		{
			GroupVersion: "v1",
			APIResources: []metav1.APIResource{
				{Name: "pods", Namespaced: true, Kind: "Pod", Verbs: metav1.Verbs{"delete", "list", "watch"}},
				{Name: "secrets", Namespaced: true, Kind: "Secret", Verbs: metav1.Verbs{"delete", "list", "watch"}},
			},
		},
	}
	fakeDiscoveryClient := &fakeServerResources{
		PreferredResources: serverResources,
		Error:              nil,
		Lock:               sync.Mutex{},
	}

	testHandler := &fakeActionHandler{
		response: map[string]FakeResponse{
			"GET" + "/api/v1/pods": {
				200,
				[]byte("{}"),
			},
			"GET" + "/apis/apps/v1/deployments": {
				200,
				[]byte("{}"),
			},
			"GET" + "/api/v1/secrets": {
				404,
				[]byte("{}"),
			},
		},
	}

	testHandler2 := &fakeActionHandler{
		response: map[string]FakeResponse{
			"GET" + "/api/v1/secrets": {
				200,
				[]byte("{}"),
			},
		},
	}
	var secretSyncOK atomic.Bool
	var alternativeTestHandler = func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/api/v1/secrets" && secretSyncOK.Load() {
			testHandler2.ServeHTTP(response, request)
			return
		}
		testHandler.ServeHTTP(response, request)
	}
	srv, clientConfig := testServerAndClientConfig(alternativeTestHandler)
	defer srv.Close()
	clientConfig.ContentConfig.NegotiatedSerializer = nil
	client, err := kubernetes.NewForConfig(clientConfig)
	if err != nil {
		t.Fatal(err)
	}

	tweakableRM := meta.NewDefaultRESTMapper(nil)
	tweakableRM.AddSpecific(schema.GroupVersionKind{Version: "v1", Kind: "Pod"}, schema.GroupVersionResource{Version: "v1", Resource: "pods"}, schema.GroupVersionResource{Version: "v1", Resource: "pod"}, meta.RESTScopeNamespace)
	tweakableRM.AddSpecific(schema.GroupVersionKind{Version: "v1", Kind: "Secret"}, schema.GroupVersionResource{Version: "v1", Resource: "secrets"}, schema.GroupVersionResource{Version: "v1", Resource: "secret"}, meta.RESTScopeNamespace)
	tweakableRM.AddSpecific(schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}, schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}, schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployment"}, meta.RESTScopeNamespace)
	rm := &testRESTMapper{meta.MultiRESTMapper{tweakableRM, testrestmapper.TestOnlyStaticRESTMapper(legacyscheme.Scheme)}}
	metadataClient, err := metadata.NewForConfig(clientConfig)
	if err != nil {
		t.Fatal(err)
	}

	sharedInformers := informers.NewSharedInformerFactory(client, 0)

	logger, tCtx := ktesting.NewTestContext(t)
	defer tCtx.Cancel("test has completed")
	alwaysStarted := make(chan struct{})
	close(alwaysStarted)
	gc, err := NewGarbageCollector(tCtx, client, metadataClient, rm, map[schema.GroupResource]struct{}{}, sharedInformers, alwaysStarted)
	if err != nil {
		t.Fatal(err)
	}

	syncPeriod := 200 * time.Millisecond
	go gc.Run(tCtx, 1, syncPeriod)
	// The pseudo-code of GarbageCollector.Sync():
	// GarbageCollector.Sync(client, period, stopCh):
	//    wait.Until() loops with `period` until the `stopCh` is closed :
	//        wait.PollImmediateUntil() loops with 100ms (hardcode) util the `stopCh` is closed:
	//            GetDeletableResources()
	//            gc.resyncMonitors()
	//            cache.WaitForNamedCacheSync() loops with `syncedPollPeriod` (hardcoded to 100ms), until either its stop channel is closed after `period`, or all caches synced.
	//
	// Setting the period to 200ms allows the WaitForCacheSync() to check
	// for cache sync ~2 times in every wait.PollImmediateUntil() loop.
	//
	// The 1s sleep in the test allows GetDeletableResources and
	// gc.resyncMonitors to run ~5 times to ensure the changes to the
	// fakeDiscoveryClient are picked up.
	go gc.Sync(tCtx, fakeDiscoveryClient, syncPeriod)

	// Wait until the sync discovers the initial resources
	time.Sleep(1 * time.Second)

	err = expectSyncNotBlocked(fakeDiscoveryClient)
	if err != nil {
		t.Fatalf("Expected garbagecollector.Sync to still be running but it is blocked: %v", err)
	}
	assertMonitors(t, gc, "pods", "deployments")

	// Simulate the discovery client returning an error
	fakeDiscoveryClient.setPreferredResources(nil, fmt.Errorf("error calling discoveryClient.ServerPreferredResources()"))

	// Wait until sync discovers the change
	time.Sleep(1 * time.Second)
	// No monitor changes
	assertMonitors(t, gc, "pods", "deployments")

	// Remove the error from being returned and see if the garbage collector sync is still working
	fakeDiscoveryClient.setPreferredResources(serverResources, nil)

	err = expectSyncNotBlocked(fakeDiscoveryClient)
	if err != nil {
		t.Fatalf("Expected garbagecollector.Sync to still be running but it is blocked: %v", err)
	}
	assertMonitors(t, gc, "pods", "deployments")

	// Simulate the discovery client returning a resource the restmapper can resolve, but will not sync caches
	fakeDiscoveryClient.setPreferredResources(unsyncableServerResources, nil)

	// Wait until sync discovers the change
	time.Sleep(1 * time.Second)
	assertMonitors(t, gc, "pods", "secrets")

	// Put the resources back to normal and ensure garbage collector sync recovers
	fakeDiscoveryClient.setPreferredResources(serverResources, nil)

	err = expectSyncNotBlocked(fakeDiscoveryClient)
	if err != nil {
		t.Fatalf("Expected garbagecollector.Sync to still be running but it is blocked: %v", err)
	}
	assertMonitors(t, gc, "pods", "deployments")

	// Partial discovery failure
	fakeDiscoveryClient.setPreferredResources(unsyncableServerResources, appsV1Error)
	// Wait until sync discovers the change
	time.Sleep(1 * time.Second)
	// Deployments monitor kept
	assertMonitors(t, gc, "pods", "deployments", "secrets")

	// Put the resources back to normal and ensure garbage collector sync recovers
	fakeDiscoveryClient.setPreferredResources(serverResources, nil)
	// Wait until sync discovers the change
	time.Sleep(1 * time.Second)
	err = expectSyncNotBlocked(fakeDiscoveryClient)
	if err != nil {
		t.Fatalf("Expected garbagecollector.Sync to still be running but it is blocked: %v", err)
	}
	// Unsyncable monitor removed
	assertMonitors(t, gc, "pods", "deployments")

	// Simulate initial not-synced informer which will be synced at the end.
	metrics.GarbageCollectorResourcesSyncError.Reset()
	fakeDiscoveryClient.setPreferredResources(unsyncableServerResources, nil)
	time.Sleep(1 * time.Second)
	assertMonitors(t, gc, "pods", "secrets")
	if gc.IsSynced(logger) {
		t.Fatal("cache from garbage collector should not be synced")
	}
	val, _ := metricsutil.GetCounterMetricValue(metrics.GarbageCollectorResourcesSyncError)
	if val < 1 {
		t.Fatalf("expect sync error metric > 0")
	}

	// The informer is synced now.
	secretSyncOK.Store(true)
	if err := wait.PollUntilContextTimeout(tCtx, time.Second, wait.ForeverTestTimeout, true, func(ctx context.Context) (bool, error) {
		return gc.IsSynced(logger), nil
	}); err != nil {
		t.Fatal(err)
	}
}

func assertMonitors(t *testing.T, gc *GarbageCollector, resources ...string) {
	t.Helper()
	expected := sets.NewString(resources...)
	actual := sets.NewString()
	for m := range gc.dependencyGraphBuilder.monitors {
		actual.Insert(m.Resource)
	}
	if !actual.Equal(expected) {
		t.Fatalf("expected monitors %v, got %v", expected.List(), actual.List())
	}
}

func expectSyncNotBlocked(fakeDiscoveryClient *fakeServerResources) error {
	before := fakeDiscoveryClient.getInterfaceUsedCount()
	t := 1 * time.Second
	time.Sleep(t)
	after := fakeDiscoveryClient.getInterfaceUsedCount()
	if before == after {
		return fmt.Errorf("discoveryClient.ServerPreferredResources() not called over %v", t)
	}
	return nil
}

type fakeServerResources struct {
	PreferredResources []*metav1.APIResourceList
	Error              error
	Lock               sync.Mutex
	InterfaceUsedCount int
}

func (*fakeServerResources) ServerResourcesForGroupVersion(groupVersion string) (*metav1.APIResourceList, error) {
	return nil, nil
}

func (*fakeServerResources) ServerGroupsAndResources() ([]*metav1.APIGroup, []*metav1.APIResourceList, error) {
	return nil, nil, nil
}

func (f *fakeServerResources) ServerPreferredResources() ([]*metav1.APIResourceList, error) {
	f.Lock.Lock()
	defer f.Lock.Unlock()
	f.InterfaceUsedCount++
	return f.PreferredResources, f.Error
}

func (f *fakeServerResources) setPreferredResources(resources []*metav1.APIResourceList, err error) {
	f.Lock.Lock()
	defer f.Lock.Unlock()
	f.PreferredResources = resources
	f.Error = err
}

func (f *fakeServerResources) getInterfaceUsedCount() int {
	f.Lock.Lock()
	defer f.Lock.Unlock()
	return f.InterfaceUsedCount
}

func (*fakeServerResources) ServerPreferredNamespacedResources() ([]*metav1.APIResourceList, error) {
	return nil, nil
}

func TestConflictingData(t *testing.T) {
	pod1ns1 := makeID("v1", "Pod", "ns1", "podname1", "poduid1")
	pod2ns1 := makeID("v1", "Pod", "ns1", "podname2", "poduid2")
	pod2ns2 := makeID("v1", "Pod", "ns2", "podname2", "poduid2")
	node1 := makeID("v1", "Node", "", "nodename", "nodeuid1")

	role1v1beta1 := makeID("rbac.authorization.k8s.io/v1beta1", "Role", "ns1", "role1", "roleuid1")
	role1v1 := makeID("rbac.authorization.k8s.io/v1", "Role", "ns1", "role1", "roleuid1")

	deployment1apps := makeID("apps/v1", "Deployment", "ns1", "deployment1", "deploymentuid1")
	deployment1extensions := makeID("extensions/v1beta1", "Deployment", "ns1", "deployment1", "deploymentuid1") // not served, still referenced

	// when a reference is made to node1 from a namespaced resource, the virtual node inserted has namespace coordinates
	node1WithNamespace := makeID("v1", "Node", "ns1", "nodename", "nodeuid1")

	// when a reference is made to pod1 from a cluster-scoped resource, the virtual node inserted has no namespace
	pod1nonamespace := makeID("v1", "Pod", "", "podname1", "poduid1")

	badSecretReferenceWithDeploymentUID := makeID("v1", "Secret", "ns1", "secretname", string(deployment1apps.UID))
	badChildPod := makeID("v1", "Pod", "ns1", "badpod", "badpoduid")
	goodChildPod := makeID("v1", "Pod", "ns1", "goodpod", "goodpoduid")

	var testScenarios = []struct {
		name           string
		initialObjects []runtime.Object
		steps          []step
	}{
		{
			name: "good child in ns1 -> cluster-scoped owner",
			steps: []step{
				// setup
				createObjectInClient("", "v1", "nodes", "", makeMetadataObj(node1)),
				createObjectInClient("", "v1", "pods", "ns1", makeMetadataObj(pod1ns1, node1)),
				// observe namespaced child with not-yet-observed cluster-scoped parent
				processEvent(makeAddEvent(pod1ns1, node1)),
				assertState(state{
					graphNodes:             []*node{makeNode(pod1ns1, withOwners(node1)), makeNode(node1WithNamespace, virtual)}, // virtual node1 (matching child namespace)
					pendingAttemptToDelete: []*node{makeNode(node1WithNamespace, virtual)},                                       // virtual node1 queued for attempted delete
				}),
				// handle queued delete of virtual node
				processAttemptToDelete(1),
				assertState(state{
					clientActions:          []string{"get /v1, Resource=nodes name=nodename"},
					graphNodes:             []*node{makeNode(pod1ns1, withOwners(node1)), makeNode(node1WithNamespace, virtual)}, // virtual node1 (matching child namespace)
					pendingAttemptToDelete: []*node{makeNode(node1WithNamespace, virtual)},                                       // virtual node1 still not observed, got requeued
				}),
				// observe cluster-scoped parent
				processEvent(makeAddEvent(node1)),
				assertState(state{
					graphNodes:             []*node{makeNode(pod1ns1, withOwners(node1)), makeNode(node1)}, // node1 switched to observed, fixed namespace coordinate
					pendingAttemptToDelete: []*node{makeNode(node1WithNamespace, virtual)},                 // virtual node1 queued for attempted delete
				}),
				// handle queued delete of virtual node
				// final state: child and parent present in graph, no queued actions
				processAttemptToDelete(1),
				assertState(state{
					graphNodes: []*node{makeNode(pod1ns1, withOwners(node1)), makeNode(node1)},
				}),
			},
		},
		// child in namespace A with owner reference to namespaced type in namespace B
		// * should be deleted immediately
		// * event should be logged in namespace A with involvedObject of bad-child indicating the error
		{
			name: "bad child in ns1 -> owner in ns2 (child first)",
			steps: []step{
				// 0,1: setup
				createObjectInClient("", "v1", "pods", "ns1", makeMetadataObj(pod1ns1, pod2ns1)),
				createObjectInClient("", "v1", "pods", "ns2", makeMetadataObj(pod2ns2)),
				// 2,3: observe namespaced child with not-yet-observed namespace-scoped parent
				processEvent(makeAddEvent(pod1ns1, pod2ns2)),
				assertState(state{
					graphNodes:             []*node{makeNode(pod1ns1, withOwners(pod2ns2)), makeNode(pod2ns1, virtual)}, // virtual pod2 (matching child namespace)
					pendingAttemptToDelete: []*node{makeNode(pod2ns1, virtual)},                                         // virtual pod2 queued for attempted delete
				}),
				// 4,5: observe parent
				processEvent(makeAddEvent(pod2ns2)),
				assertState(state{
					graphNodes:             []*node{makeNode(pod1ns1, withOwners(pod2ns2)), makeNode(pod2ns2)}, // pod2 is no longer virtual, namespace coordinate is corrected
					pendingAttemptToDelete: []*node{makeNode(pod2ns1, virtual), makeNode(pod1ns1)},             // virtual pod2 still queued for attempted delete, bad child pod1 queued because it disagreed with observed parent
					events:                 []string{`Warning OwnerRefInvalidNamespace ownerRef [v1/Pod, namespace: ns1, name: podname2, uid: poduid2] does not exist in namespace "ns1" involvedObject{kind=Pod,apiVersion=v1}`},
				}),
				// 6,7: handle queued delete of virtual parent
				processAttemptToDelete(1),
				assertState(state{
					graphNodes:             []*node{makeNode(pod1ns1, withOwners(pod2ns2)), makeNode(pod2ns2)},
					pendingAttemptToDelete: []*node{makeNode(pod1ns1)}, // bad child pod1 queued because it disagreed with observed parent
				}),
				// 8,9: handle queued delete of bad child
				processAttemptToDelete(1),
				assertState(state{
					clientActions: []string{
						"get /v1, Resource=pods ns=ns1 name=podname1",    // lookup of pod1 pre-delete
						"get /v1, Resource=pods ns=ns1 name=podname2",    // verification bad parent reference is absent
						"delete /v1, Resource=pods ns=ns1 name=podname1", // pod1 delete
					},
					graphNodes:       []*node{makeNode(pod1ns1, withOwners(pod2ns2)), makeNode(pod2ns2)},
					absentOwnerCache: []objectReference{pod2ns1}, // cached absence of bad parent
				}),
				// 10,11: observe delete issued in step 8
				// final state: parent present in graph, no queued actions
				processEvent(makeDeleteEvent(pod1ns1)),
				assertState(state{
					graphNodes:       []*node{makeNode(pod2ns2)}, // only good parent remains
					absentOwnerCache: []objectReference{pod2ns1}, // cached absence of bad parent
				}),
			},
		},
		{
			name: "bad child in ns1 -> owner in ns2 (owner first)",
			steps: []step{
				// 0,1: setup
				createObjectInClient("", "v1", "pods", "ns1", makeMetadataObj(pod1ns1, pod2ns1)),
				createObjectInClient("", "v1", "pods", "ns2", makeMetadataObj(pod2ns2)),
				// 2,3: observe parent
				processEvent(makeAddEvent(pod2ns2)),
				assertState(state{
					graphNodes: []*node{makeNode(pod2ns2)},
				}),
				// 4,5: observe namespaced child with invalid cross-namespace reference to parent
				processEvent(makeAddEvent(pod1ns1, pod2ns1)),
				assertState(state{
					graphNodes:             []*node{makeNode(pod1ns1, withOwners(pod2ns1)), makeNode(pod2ns2)},
					pendingAttemptToDelete: []*node{makeNode(pod1ns1)}, // bad child queued for attempted delete
					events:                 []string{`Warning OwnerRefInvalidNamespace ownerRef [v1/Pod, namespace: ns1, name: podname2, uid: poduid2] does not exist in namespace "ns1" involvedObject{kind=Pod,apiVersion=v1}`},
				}),
				// 6,7: handle queued delete of bad child
				processAttemptToDelete(1),
				assertState(state{
					clientActions: []string{
						"get /v1, Resource=pods ns=ns1 name=podname1",    // lookup of pod1 pre-delete
						"get /v1, Resource=pods ns=ns1 name=podname2",    // verification bad parent reference is absent
						"delete /v1, Resource=pods ns=ns1 name=podname1", // pod1 delete
					},
					graphNodes:             []*node{makeNode(pod1ns1, withOwners(pod2ns1)), makeNode(pod2ns2)},
					pendingAttemptToDelete: []*node{},
					absentOwnerCache:       []objectReference{pod2ns1}, // cached absence of bad parent
				}),
				// 8,9: observe delete issued in step 6
				// final state: parent present in graph, no queued actions
				processEvent(makeDeleteEvent(pod1ns1)),
				assertState(state{
					graphNodes:       []*node{makeNode(pod2ns2)}, // only good parent remains
					absentOwnerCache: []objectReference{pod2ns1}, // cached absence of bad parent
				}),
			},
		},
		// child that is cluster-scoped with owner reference to namespaced type in namespace B
		// * should not be deleted
		// * event should be logged in namespace kube-system with involvedObject of bad-child indicating the error
		{
			name: "bad cluster-scoped child -> owner in ns1 (child first)",
			steps: []step{
				// setup
				createObjectInClient("", "v1", "nodes", "", makeMetadataObj(node1, pod1ns1)),
				createObjectInClient("", "v1", "pods", "ns1", makeMetadataObj(pod1ns1)),
				// 2,3: observe cluster-scoped child with not-yet-observed namespaced parent
				processEvent(makeAddEvent(node1, pod1ns1)),
				assertState(state{
					graphNodes:             []*node{makeNode(node1, withOwners(pod1nonamespace)), makeNode(pod1nonamespace, virtual)}, // virtual pod1 (with no namespace)
					pendingAttemptToDelete: []*node{makeNode(pod1nonamespace, virtual)},                                               // virtual pod1 queued for attempted delete
				}),
				// 4,5: handle queued delete of virtual pod1
				processAttemptToDelete(1),
				assertState(state{
					graphNodes:             []*node{makeNode(node1, withOwners(pod1nonamespace)), makeNode(pod1nonamespace, virtual)}, // virtual pod1 (with no namespace)
					pendingAttemptToDelete: []*node{},                                                                                 // namespace-scoped virtual object without a namespace coordinate not re-queued
				}),
				// 6,7: observe namespace-scoped parent
				processEvent(makeAddEvent(pod1ns1)),
				assertState(state{
					graphNodes:             []*node{makeNode(node1, withOwners(pod1nonamespace)), makeNode(pod1ns1)}, // pod1 namespace coordinate corrected, made non-virtual
					events:                 []string{`Warning OwnerRefInvalidNamespace ownerRef [v1/Pod, namespace: , name: podname1, uid: poduid1] does not exist in namespace "" involvedObject{kind=Node,apiVersion=v1}`},
					pendingAttemptToDelete: []*node{makeNode(node1, withOwners(pod1ns1))}, // bad cluster-scoped child added to attemptToDelete queue
				}),
				// 8,9: handle queued attempted delete of bad cluster-scoped child
				// final state: parent and child present in graph, no queued actions
				processAttemptToDelete(1),
				assertState(state{
					clientActions: []string{
						"get /v1, Resource=nodes name=nodename", // lookup of node pre-delete
					},
					graphNodes: []*node{makeNode(node1, withOwners(pod1nonamespace)), makeNode(pod1ns1)},
				}),
			},
		},
		{
			name: "bad cluster-scoped child -> owner in ns1 (owner first)",
			steps: []step{
				// setup
				createObjectInClient("", "v1", "nodes", "", makeMetadataObj(node1, pod1ns1)),
				createObjectInClient("", "v1", "pods", "ns1", makeMetadataObj(pod1ns1)),
				// 2,3: observe namespace-scoped parent
				processEvent(makeAddEvent(pod1ns1)),
				assertState(state{
					graphNodes: []*node{makeNode(pod1ns1)},
				}),
				// 4,5: observe cluster-scoped child
				processEvent(makeAddEvent(node1, pod1ns1)),
				assertState(state{
					graphNodes:             []*node{makeNode(node1, withOwners(pod1nonamespace)), makeNode(pod1ns1)},
					events:                 []string{`Warning OwnerRefInvalidNamespace ownerRef [v1/Pod, namespace: , name: podname1, uid: poduid1] does not exist in namespace "" involvedObject{kind=Node,apiVersion=v1}`},
					pendingAttemptToDelete: []*node{makeNode(node1, withOwners(pod1ns1))}, // bad cluster-scoped child added to attemptToDelete queue
				}),
				// 6,7: handle queued attempted delete of bad cluster-scoped child
				// final state: parent and child present in graph, no queued actions
				processAttemptToDelete(1),
				assertState(state{
					clientActions: []string{
						"get /v1, Resource=nodes name=nodename", // lookup of node pre-delete
					},
					graphNodes: []*node{makeNode(node1, withOwners(pod1nonamespace)), makeNode(pod1ns1)},
				}),
			},
		},
		// child pointing at non-preferred still-served apiVersion of parent object (e.g. rbac/v1beta1)
		// * should not be deleted prematurely
		// * should not repeatedly poll attemptToDelete while waiting
		// * should be deleted when the actual parent is deleted
		{
			name: "good child -> existing owner with non-preferred accessible API version",
			steps: []step{
				// setup
				createObjectInClient("rbac.authorization.k8s.io", "v1", "roles", "ns1", makeMetadataObj(role1v1)),
				createObjectInClient("rbac.authorization.k8s.io", "v1beta1", "roles", "ns1", makeMetadataObj(role1v1beta1)),
				createObjectInClient("", "v1", "pods", "ns1", makeMetadataObj(pod1ns1, role1v1beta1)),
				// 3,4: observe child
				processEvent(makeAddEvent(pod1ns1, role1v1beta1)),
				assertState(state{
					graphNodes:             []*node{makeNode(pod1ns1, withOwners(role1v1beta1)), makeNode(role1v1beta1, virtual)},
					pendingAttemptToDelete: []*node{makeNode(role1v1beta1, virtual)}, // virtual parent enqueued for delete attempt
				}),
				// 5,6: handle queued attempted delete of virtual parent
				processAttemptToDelete(1),
				assertState(state{
					clientActions: []string{
						"get rbac.authorization.k8s.io/v1beta1, Resource=roles ns=ns1 name=role1", // lookup of node pre-delete
					},
					graphNodes:             []*node{makeNode(pod1ns1, withOwners(role1v1beta1)), makeNode(role1v1beta1, virtual)},
					pendingAttemptToDelete: []*node{makeNode(role1v1beta1, virtual)}, // not yet observed, still in the attemptToDelete queue
				}),
				// 7,8: observe parent via v1
				processEvent(makeAddEvent(role1v1)),
				assertState(state{
					graphNodes:             []*node{makeNode(pod1ns1, withOwners(role1v1beta1)), makeNode(role1v1)},               // parent version/virtual state gets corrected
					pendingAttemptToDelete: []*node{makeNode(role1v1beta1, virtual), makeNode(pod1ns1, withOwners(role1v1beta1))}, // virtual parent and mismatched child enqueued for delete attempt
				}),
				// 9,10: process attemptToDelete
				// virtual node dropped from attemptToDelete with no further action because the real node has been observed now
				processAttemptToDelete(1),
				assertState(state{
					graphNodes:             []*node{makeNode(pod1ns1, withOwners(role1v1beta1)), makeNode(role1v1)},
					pendingAttemptToDelete: []*node{makeNode(pod1ns1, withOwners(role1v1beta1))}, // mismatched child enqueued for delete attempt
				}),
				// 11,12: process attemptToDelete for mismatched parent
				processAttemptToDelete(1),
				assertState(state{
					clientActions: []string{
						"get /v1, Resource=pods ns=ns1 name=podname1",                             // lookup of child pre-delete
						"get rbac.authorization.k8s.io/v1beta1, Resource=roles ns=ns1 name=role1", // verifying parent is solid
					},
					graphNodes: []*node{makeNode(pod1ns1, withOwners(role1v1beta1)), makeNode(role1v1)},
				}),
				// 13,14: teardown
				deleteObjectFromClient("rbac.authorization.k8s.io", "v1", "roles", "ns1", "role1"),
				deleteObjectFromClient("rbac.authorization.k8s.io", "v1beta1", "roles", "ns1", "role1"),
				// 15,16: observe delete via v1
				processEvent(makeDeleteEvent(role1v1)),
				assertState(state{
					graphNodes:             []*node{makeNode(pod1ns1, withOwners(role1v1beta1))}, // only child remains
					absentOwnerCache:       []objectReference{role1v1},                           // cached absence of parent via v1
					pendingAttemptToDelete: []*node{makeNode(pod1ns1, withOwners(role1v1beta1))},
				}),
				// 17,18: process attemptToDelete for child
				processAttemptToDelete(1),
				assertState(state{
					clientActions: []string{
						"get /v1, Resource=pods ns=ns1 name=podname1",                             // lookup of child pre-delete
						"get rbac.authorization.k8s.io/v1beta1, Resource=roles ns=ns1 name=role1", // verifying parent is solid
						"delete /v1, Resource=pods ns=ns1 name=podname1",
					},
					absentOwnerCache: []objectReference{role1v1, role1v1beta1}, // cached absence of v1beta1 role
					graphNodes:       []*node{makeNode(pod1ns1, withOwners(role1v1beta1))},
				}),
				// 19,20: observe delete issued in step 17
				// final state: empty graph, no queued actions
				processEvent(makeDeleteEvent(pod1ns1)),
				assertState(state{
					absentOwnerCache: []objectReference{role1v1, role1v1beta1},
				}),
			},
		},
		// child pointing at no-longer-served apiVersion of still-existing parent object (e.g. extensions/v1beta1 deployment)
		// * should not be deleted (this is indistinguishable from referencing an unknown kind/version)
		// * virtual parent should not repeatedly poll attemptToDelete once real parent is observed
		{
			name: "child -> existing owner with inaccessible API version (child first)",
			steps: []step{
				// setup
				createObjectInClient("apps", "v1", "deployments", "ns1", makeMetadataObj(deployment1apps)),
				createObjectInClient("", "v1", "pods", "ns1", makeMetadataObj(pod1ns1, deployment1extensions)),
				// 2,3: observe child
				processEvent(makeAddEvent(pod1ns1, deployment1extensions)),
				assertState(state{
					graphNodes:             []*node{makeNode(pod1ns1, withOwners(deployment1extensions)), makeNode(deployment1extensions, virtual)},
					pendingAttemptToDelete: []*node{makeNode(deployment1extensions, virtual)}, // virtual parent enqueued for delete attempt
				}),
				// 4,5: handle queued attempted delete of virtual parent
				processAttemptToDelete(1),
				assertState(state{
					graphNodes:             []*node{makeNode(pod1ns1, withOwners(deployment1extensions)), makeNode(deployment1extensions, virtual)},
					pendingAttemptToDelete: []*node{makeNode(deployment1extensions, virtual)}, // requeued on restmapper error
				}),
				// 6,7: observe parent via v1
				processEvent(makeAddEvent(deployment1apps)),
				assertState(state{
					graphNodes:             []*node{makeNode(pod1ns1, withOwners(deployment1extensions)), makeNode(deployment1apps)},                // parent version/virtual state gets corrected
					pendingAttemptToDelete: []*node{makeNode(deployment1extensions, virtual), makeNode(pod1ns1, withOwners(deployment1extensions))}, // virtual parent and mismatched child enqueued for delete attempt
				}),
				// 8,9: process attemptToDelete
				// virtual node dropped from attemptToDelete with no further action because the real node has been observed now
				processAttemptToDelete(1),
				assertState(state{
					graphNodes:             []*node{makeNode(pod1ns1, withOwners(deployment1extensions)), makeNode(deployment1apps)},
					pendingAttemptToDelete: []*node{makeNode(pod1ns1, withOwners(deployment1extensions))}, // mismatched child enqueued for delete attempt
				}),
				// 10,11: process attemptToDelete for mismatched child
				processAttemptToDelete(1),
				assertState(state{
					clientActions: []string{
						"get /v1, Resource=pods ns=ns1 name=podname1", // lookup of child pre-delete
					},
					graphNodes:             []*node{makeNode(pod1ns1, withOwners(deployment1extensions)), makeNode(deployment1apps)},
					pendingAttemptToDelete: []*node{makeNode(pod1ns1, withOwners(deployment1extensions))}, // mismatched child still enqueued - restmapper error
				}),
				// 12: teardown
				deleteObjectFromClient("apps", "v1", "deployments", "ns1", "deployment1"),
				// 13,14: observe delete via v1
				processEvent(makeDeleteEvent(deployment1apps)),
				assertState(state{
					graphNodes:             []*node{makeNode(pod1ns1, withOwners(deployment1extensions))}, // only child remains
					absentOwnerCache:       []objectReference{deployment1apps},                            // cached absence of parent via v1
					pendingAttemptToDelete: []*node{makeNode(pod1ns1, withOwners(deployment1extensions))},
				}),
				// 17,18: process attemptToDelete for child
				processAttemptToDelete(1),
				assertState(state{
					clientActions: []string{
						"get /v1, Resource=pods ns=ns1 name=podname1", // lookup of child pre-delete
					},
					graphNodes:             []*node{makeNode(pod1ns1, withOwners(deployment1extensions))}, // only child remains
					absentOwnerCache:       []objectReference{deployment1apps},
					pendingAttemptToDelete: []*node{makeNode(pod1ns1, withOwners(deployment1extensions))}, // mismatched child still enqueued - restmapper error
				}),
			},
		},
		{
			name: "child -> existing owner with inaccessible API version (owner first)",
			steps: []step{
				// setup
				createObjectInClient("apps", "v1", "deployments", "ns1", makeMetadataObj(deployment1apps)),
				createObjectInClient("", "v1", "pods", "ns1", makeMetadataObj(pod1ns1, deployment1extensions)),
				// 2,3: observe parent via v1
				processEvent(makeAddEvent(deployment1apps)),
				assertState(state{
					graphNodes: []*node{makeNode(deployment1apps)},
				}),
				// 4,5: observe child
				processEvent(makeAddEvent(pod1ns1, deployment1extensions)),
				assertState(state{
					graphNodes:             []*node{makeNode(pod1ns1, withOwners(deployment1extensions)), makeNode(deployment1apps)},
					pendingAttemptToDelete: []*node{makeNode(pod1ns1, withOwners(deployment1extensions))}, // mismatched child enqueued for delete attempt
				}),
				// 6,7: process attemptToDelete for mismatched child
				processAttemptToDelete(1),
				assertState(state{
					clientActions: []string{
						"get /v1, Resource=pods ns=ns1 name=podname1", // lookup of child pre-delete
					},
					graphNodes:             []*node{makeNode(pod1ns1, withOwners(deployment1extensions)), makeNode(deployment1apps)},
					pendingAttemptToDelete: []*node{makeNode(pod1ns1, withOwners(deployment1extensions))}, // mismatched child still enqueued - restmapper error
				}),
				// 8: teardown
				deleteObjectFromClient("apps", "v1", "deployments", "ns1", "deployment1"),
				// 9,10: observe delete via v1
				processEvent(makeDeleteEvent(deployment1apps)),
				assertState(state{
					graphNodes:             []*node{makeNode(pod1ns1, withOwners(deployment1extensions))}, // only child remains
					absentOwnerCache:       []objectReference{deployment1apps},                            // cached absence of parent via v1
					pendingAttemptToDelete: []*node{makeNode(pod1ns1, withOwners(deployment1extensions))},
				}),
				// 11,12: process attemptToDelete for child
				// final state: child with unresolvable ownerRef remains, queued in pendingAttemptToDelete
				processAttemptToDelete(1),
				assertState(state{
					clientActions: []string{
						"get /v1, Resource=pods ns=ns1 name=podname1", // lookup of child pre-delete
					},
					graphNodes:             []*node{makeNode(pod1ns1, withOwners(deployment1extensions))}, // only child remains
					absentOwnerCache:       []objectReference{deployment1apps},
					pendingAttemptToDelete: []*node{makeNode(pod1ns1, withOwners(deployment1extensions))}, // mismatched child still enqueued - restmapper error
				}),
			},
		},
		// child pointing at no-longer-served apiVersion of no-longer-existing parent object (e.g. extensions/v1beta1 deployment)
		// * should not be deleted (this is indistinguishable from referencing an unknown kind/version)
		// * should repeatedly poll attemptToDelete
		// * should not block deletion of legitimate children of missing deployment
		{
			name: "child -> non-existent owner with inaccessible API version (inaccessible parent apiVersion first)",
			steps: []step{
				// setup
				createObjectInClient("", "v1", "pods", "ns1", makeMetadataObj(pod1ns1, deployment1extensions)),
				createObjectInClient("", "v1", "pods", "ns1", makeMetadataObj(pod2ns1, deployment1apps)),
				// 2,3: observe child pointing at no-longer-served apiVersion
				processEvent(makeAddEvent(pod1ns1, deployment1extensions)),
				assertState(state{
					graphNodes:             []*node{makeNode(pod1ns1, withOwners(deployment1extensions)), makeNode(deployment1extensions, virtual)},
					pendingAttemptToDelete: []*node{makeNode(deployment1extensions, virtual)}, // virtual parent enqueued for delete attempt
				}),
				// 4,5: observe child pointing at served apiVersion where owner does not exist
				processEvent(makeAddEvent(pod2ns1, deployment1apps)),
				assertState(state{
					graphNodes:             []*node{makeNode(pod1ns1, withOwners(deployment1extensions)), makeNode(deployment1extensions, virtual), makeNode(pod2ns1, withOwners(deployment1apps))},
					pendingAttemptToDelete: []*node{makeNode(deployment1extensions, virtual), makeNode(pod2ns1, withOwners(deployment1apps))}, // mismatched child enqueued for delete attempt
				}),
				// 6,7: handle attempt to delete virtual parent for inaccessible apiVersion
				processAttemptToDelete(1),
				assertState(state{
					graphNodes:             []*node{makeNode(pod1ns1, withOwners(deployment1extensions)), makeNode(deployment1extensions, virtual), makeNode(pod2ns1, withOwners(deployment1apps))},
					pendingAttemptToDelete: []*node{makeNode(pod2ns1, withOwners(deployment1apps)), makeNode(deployment1extensions, virtual)}, // inaccessible parent requeued to end
				}),
				// 8,9: handle attempt to delete mismatched child
				processAttemptToDelete(1),
				assertState(state{
					clientActions: []string{
						"get /v1, Resource=pods ns=ns1 name=podname2",               // lookup of child pre-delete
						"get apps/v1, Resource=deployments ns=ns1 name=deployment1", // lookup of parent
						"delete /v1, Resource=pods ns=ns1 name=podname2",            // delete child
					},
					graphNodes:             []*node{makeNode(pod1ns1, withOwners(deployment1extensions)), makeNode(deployment1extensions, virtual), makeNode(pod2ns1, withOwners(deployment1apps))},
					absentOwnerCache:       []objectReference{deployment1apps},                // verifiably absent parent remembered
					pendingAttemptToDelete: []*node{makeNode(deployment1extensions, virtual)}, // mismatched child with verifiably absent parent deleted
				}),
				// 10,11: observe delete issued in step 8
				processEvent(makeDeleteEvent(pod2ns1)),
				assertState(state{
					graphNodes:             []*node{makeNode(pod1ns1, withOwners(deployment1extensions)), makeNode(deployment1extensions, virtual)},
					absentOwnerCache:       []objectReference{deployment1apps},
					pendingAttemptToDelete: []*node{makeNode(deployment1extensions, virtual)},
				}),
				// 12,13: final state: inaccessible parent requeued in attemptToDelete
				processAttemptToDelete(1),
				assertState(state{
					graphNodes:             []*node{makeNode(pod1ns1, withOwners(deployment1extensions)), makeNode(deployment1extensions, virtual)},
					absentOwnerCache:       []objectReference{deployment1apps},
					pendingAttemptToDelete: []*node{makeNode(deployment1extensions, virtual)},
				}),
			},
		},

		{
			name: "child -> non-existent owner with inaccessible API version (accessible parent apiVersion first)",
			steps: []step{
				// setup
				createObjectInClient("", "v1", "pods", "ns1", makeMetadataObj(pod1ns1, deployment1extensions)),
				createObjectInClient("", "v1", "pods", "ns1", makeMetadataObj(pod2ns1, deployment1apps)),
				// 2,3: observe child pointing at served apiVersion where owner does not exist
				processEvent(makeAddEvent(pod2ns1, deployment1apps)),
				assertState(state{
					graphNodes: []*node{
						makeNode(pod2ns1, withOwners(deployment1apps)),
						makeNode(deployment1apps, virtual)},
					pendingAttemptToDelete: []*node{
						makeNode(deployment1apps, virtual)}, // virtual parent enqueued for delete attempt
				}),
				// 4,5: observe child pointing at no-longer-served apiVersion
				processEvent(makeAddEvent(pod1ns1, deployment1extensions)),
				assertState(state{
					graphNodes: []*node{
						makeNode(pod2ns1, withOwners(deployment1apps)),
						makeNode(deployment1apps, virtual),
						makeNode(pod1ns1, withOwners(deployment1extensions))},
					pendingAttemptToDelete: []*node{
						makeNode(deployment1apps, virtual),
						makeNode(pod1ns1, withOwners(deployment1extensions))}, // mismatched child enqueued for delete attempt
				}),
				// 6,7: handle attempt to delete virtual parent for accessible apiVersion
				processAttemptToDelete(1),
				assertState(state{
					clientActions: []string{
						"get apps/v1, Resource=deployments ns=ns1 name=deployment1", // lookup of parent, gets 404
					},
					pendingGraphChanges: []*event{makeVirtualDeleteEvent(deployment1apps)}, // virtual parent not found, queued virtual delete event
					graphNodes: []*node{
						makeNode(pod2ns1, withOwners(deployment1apps)),
						makeNode(deployment1apps, virtual),
						makeNode(pod1ns1, withOwners(deployment1extensions))},
					pendingAttemptToDelete: []*node{makeNode(pod1ns1, withOwners(deployment1extensions))},
				}),
				// 8,9: handle attempt to delete mismatched child
				processAttemptToDelete(1),
				assertState(state{
					clientActions: []string{
						"get /v1, Resource=pods ns=ns1 name=podname1", // lookup of child pre-delete
					},
					pendingGraphChanges: []*event{makeVirtualDeleteEvent(deployment1apps)},
					graphNodes: []*node{
						makeNode(pod2ns1, withOwners(deployment1apps)),
						makeNode(deployment1apps, virtual),
						makeNode(pod1ns1, withOwners(deployment1extensions))},
					pendingAttemptToDelete: []*node{makeNode(pod1ns1, withOwners(deployment1extensions))}, // restmapper on inaccessible parent, requeued
				}),
				// 10,11: handle queued virtual delete event
				processPendingGraphChanges(1),
				assertState(state{
					graphNodes: []*node{
						makeNode(pod2ns1, withOwners(deployment1apps)),
						makeNode(deployment1extensions, virtual), // deployment node changed identity to alternative virtual identity
						makeNode(pod1ns1, withOwners(deployment1extensions)),
					},
					absentOwnerCache: []objectReference{deployment1apps}, // absent apps/v1 parent remembered
					pendingAttemptToDelete: []*node{
						makeNode(pod1ns1, withOwners(deployment1extensions)), // child referencing inaccessible apiVersion
						makeNode(pod2ns1, withOwners(deployment1apps)),       // children of absent apps/v1 parent queued for delete attempt
						makeNode(deployment1extensions, virtual),             // new virtual parent queued for delete attempt
					},
				}),

				// 12,13: handle attempt to delete child referencing inaccessible apiVersion
				processAttemptToDelete(1),
				assertState(state{
					clientActions: []string{
						"get /v1, Resource=pods ns=ns1 name=podname1", // lookup of child pre-delete
					},
					graphNodes: []*node{
						makeNode(pod2ns1, withOwners(deployment1apps)),
						makeNode(deployment1extensions, virtual),
						makeNode(pod1ns1, withOwners(deployment1extensions))},
					absentOwnerCache: []objectReference{deployment1apps},
					pendingAttemptToDelete: []*node{
						makeNode(pod2ns1, withOwners(deployment1apps)),       // children of absent apps/v1 parent queued for delete attempt
						makeNode(deployment1extensions, virtual),             // new virtual parent queued for delete attempt
						makeNode(pod1ns1, withOwners(deployment1extensions)), // child referencing inaccessible apiVersion - requeued to end
					},
				}),

				// 14,15: handle attempt to delete child referencing accessible apiVersion
				processAttemptToDelete(1),
				assertState(state{
					clientActions: []string{
						"get /v1, Resource=pods ns=ns1 name=podname2",    // lookup of child pre-delete
						"delete /v1, Resource=pods ns=ns1 name=podname2", // parent absent, delete
					},
					graphNodes: []*node{
						makeNode(pod2ns1, withOwners(deployment1apps)),
						makeNode(deployment1extensions, virtual),
						makeNode(pod1ns1, withOwners(deployment1extensions))},
					absentOwnerCache: []objectReference{deployment1apps},
					pendingAttemptToDelete: []*node{
						makeNode(deployment1extensions, virtual),             // new virtual parent queued for delete attempt
						makeNode(pod1ns1, withOwners(deployment1extensions)), // child referencing inaccessible apiVersion
					},
				}),

				// 16,17: handle attempt to delete virtual parent in inaccessible apiVersion
				processAttemptToDelete(1),
				assertState(state{
					graphNodes: []*node{
						makeNode(pod2ns1, withOwners(deployment1apps)),
						makeNode(deployment1extensions, virtual),
						makeNode(pod1ns1, withOwners(deployment1extensions))},
					absentOwnerCache: []objectReference{deployment1apps},
					pendingAttemptToDelete: []*node{
						makeNode(pod1ns1, withOwners(deployment1extensions)), // child referencing inaccessible apiVersion
						makeNode(deployment1extensions, virtual),             // virtual parent with inaccessible apiVersion - requeued to end
					},
				}),

				// 18,19: observe delete of pod2 from step 14
				// final state: virtual parent for inaccessible apiVersion and child of that parent remain in graph, queued for delete attempts with backoff
				processEvent(makeDeleteEvent(pod2ns1)),
				assertState(state{
					graphNodes: []*node{
						makeNode(deployment1extensions, virtual),
						makeNode(pod1ns1, withOwners(deployment1extensions))},
					absentOwnerCache: []objectReference{deployment1apps},
					pendingAttemptToDelete: []*node{
						makeNode(pod1ns1, withOwners(deployment1extensions)), // child referencing inaccessible apiVersion
						makeNode(deployment1extensions, virtual),             // virtual parent with inaccessible apiVersion
					},
				}),
			},
		},
		// child pointing at incorrect apiVersion/kind of still-existing parent object (e.g. core/v1 Secret with uid=123, where an apps/v1 Deployment with uid=123 exists)
		// * should be deleted immediately
		// * should not trigger deletion of legitimate children of parent
		{
			name: "bad child -> existing owner with incorrect API version (bad child, good child, bad parent delete, good parent)",
			steps: []step{
				// setup
				createObjectInClient("apps", "v1", "deployments", "ns1", makeMetadataObj(deployment1apps)),
				createObjectInClient("", "v1", "pods", "ns1", makeMetadataObj(badChildPod, badSecretReferenceWithDeploymentUID)),
				createObjectInClient("", "v1", "pods", "ns1", makeMetadataObj(goodChildPod, deployment1apps)),
				// 3,4: observe bad child
				processEvent(makeAddEvent(badChildPod, badSecretReferenceWithDeploymentUID)),
				assertState(state{
					graphNodes: []*node{
						makeNode(badChildPod, withOwners(badSecretReferenceWithDeploymentUID)),
						makeNode(badSecretReferenceWithDeploymentUID, virtual)},
					pendingAttemptToDelete: []*node{
						makeNode(badSecretReferenceWithDeploymentUID, virtual)}, // virtual parent enqueued for delete attempt
				}),

				// 5,6: observe good child
				processEvent(makeAddEvent(goodChildPod, deployment1apps)),
				assertState(state{
					graphNodes: []*node{
						makeNode(goodChildPod, withOwners(deployment1apps)), // good child added
						makeNode(badChildPod, withOwners(badSecretReferenceWithDeploymentUID)),
						makeNode(badSecretReferenceWithDeploymentUID, virtual)},
					pendingAttemptToDelete: []*node{
						makeNode(badSecretReferenceWithDeploymentUID, virtual), // virtual parent enqueued for delete attempt
						makeNode(goodChildPod, withOwners(deployment1apps)),    // good child enqueued for delete attempt
					},
				}),

				// 7,8: process pending delete of virtual parent
				processAttemptToDelete(1),
				assertState(state{
					clientActions: []string{
						"get /v1, Resource=secrets ns=ns1 name=secretname", // lookup of bad parent reference
					},
					pendingGraphChanges: []*event{makeVirtualDeleteEvent(badSecretReferenceWithDeploymentUID)}, // bad virtual parent not found, queued virtual delete event
					graphNodes: []*node{
						makeNode(goodChildPod, withOwners(deployment1apps)),
						makeNode(badChildPod, withOwners(badSecretReferenceWithDeploymentUID)),
						makeNode(badSecretReferenceWithDeploymentUID, virtual)},
					pendingAttemptToDelete: []*node{
						makeNode(goodChildPod, withOwners(deployment1apps)), // good child enqueued for delete attempt
					},
				}),

				// 9,10: process pending delete of good child, gets 200, remains
				processAttemptToDelete(1),
				assertState(state{
					clientActions: []string{
						"get /v1, Resource=pods ns=ns1 name=goodpod",                // lookup of child pre-delete
						"get apps/v1, Resource=deployments ns=ns1 name=deployment1", // lookup of good parent reference, returns 200
					},
					pendingGraphChanges: []*event{makeVirtualDeleteEvent(badSecretReferenceWithDeploymentUID)}, // bad virtual parent not found, queued virtual delete event
					graphNodes: []*node{
						makeNode(goodChildPod, withOwners(deployment1apps)),
						makeNode(badChildPod, withOwners(badSecretReferenceWithDeploymentUID)),
						makeNode(badSecretReferenceWithDeploymentUID, virtual)},
				}),

				// 11,12: process virtual delete event of bad parent reference
				processPendingGraphChanges(1),
				assertState(state{
					graphNodes: []*node{
						makeNode(goodChildPod, withOwners(deployment1apps)),
						makeNode(badChildPod, withOwners(badSecretReferenceWithDeploymentUID)),
						makeNode(deployment1apps, virtual)}, // parent node switched to alternate identity, still virtual
					absentOwnerCache: []objectReference{badSecretReferenceWithDeploymentUID}, // remember absence of bad parent coordinates
					pendingAttemptToDelete: []*node{
						makeNode(badChildPod, withOwners(badSecretReferenceWithDeploymentUID)), // child of bad parent coordinates enqueued for delete attempt
						makeNode(deployment1apps, virtual),                                     // new alternate virtual parent identity queued for delete attempt
					},
				}),

				// 13,14: process pending delete of bad child
				processAttemptToDelete(1),
				assertState(state{
					clientActions: []string{
						"get /v1, Resource=pods ns=ns1 name=badpod",    // lookup of child pre-delete
						"delete /v1, Resource=pods ns=ns1 name=badpod", // delete of bad child (absence of bad parent is cached)
					},
					graphNodes: []*node{
						makeNode(goodChildPod, withOwners(deployment1apps)),
						makeNode(badChildPod, withOwners(badSecretReferenceWithDeploymentUID)),
						makeNode(deployment1apps, virtual)}, // parent node switched to alternate identity, still virtual
					absentOwnerCache: []objectReference{badSecretReferenceWithDeploymentUID},
					pendingAttemptToDelete: []*node{
						makeNode(deployment1apps, virtual), // new alternate virtual parent identity queued for delete attempt
					},
				}),

				// 15,16: process pending delete of new virtual parent
				processAttemptToDelete(1),
				assertState(state{
					clientActions: []string{
						"get apps/v1, Resource=deployments ns=ns1 name=deployment1", // lookup of virtual parent, returns 200
					},
					graphNodes: []*node{
						makeNode(goodChildPod, withOwners(deployment1apps)),
						makeNode(badChildPod, withOwners(badSecretReferenceWithDeploymentUID)),
						makeNode(deployment1apps, virtual)}, // parent node switched to alternate identity, still virtual
					absentOwnerCache: []objectReference{badSecretReferenceWithDeploymentUID},
					pendingAttemptToDelete: []*node{
						makeNode(deployment1apps, virtual), // requeued, not yet observed
					},
				}),

				// 17,18: observe good parent
				processEvent(makeAddEvent(deployment1apps)),
				assertState(state{
					graphNodes: []*node{
						makeNode(goodChildPod, withOwners(deployment1apps)),
						makeNode(badChildPod, withOwners(badSecretReferenceWithDeploymentUID)),
						makeNode(deployment1apps)}, // parent node made non-virtual
					absentOwnerCache: []objectReference{badSecretReferenceWithDeploymentUID},
					pendingAttemptToDelete: []*node{
						makeNode(deployment1apps), // still queued, no longer virtual
					},
				}),

				// 19,20: observe delete of bad child from step 13
				processEvent(makeDeleteEvent(badChildPod, badSecretReferenceWithDeploymentUID)),
				assertState(state{
					graphNodes: []*node{
						makeNode(goodChildPod, withOwners(deployment1apps)),
						// bad child node removed
						makeNode(deployment1apps)},
					absentOwnerCache: []objectReference{badSecretReferenceWithDeploymentUID},
					pendingAttemptToDelete: []*node{
						makeNode(deployment1apps), // still queued, no longer virtual
					},
				}),

				// 21,22: process pending delete of good parent
				// final state: good parent in graph with correct coordinates, good children remain, no pending deletions
				processAttemptToDelete(1),
				assertState(state{
					clientActions: []string{
						"get apps/v1, Resource=deployments ns=ns1 name=deployment1", // lookup of good parent, returns 200
					},
					graphNodes: []*node{
						makeNode(goodChildPod, withOwners(deployment1apps)),
						makeNode(deployment1apps)},
					absentOwnerCache: []objectReference{badSecretReferenceWithDeploymentUID},
				}),
			},
		},
		{
			name: "bad child -> existing owner with incorrect API version (bad child, good child, good parent, bad parent delete)",
			steps: []step{
				// setup
				createObjectInClient("apps", "v1", "deployments", "ns1", makeMetadataObj(deployment1apps)),
				createObjectInClient("", "v1", "pods", "ns1", makeMetadataObj(badChildPod, badSecretReferenceWithDeploymentUID)),
				createObjectInClient("", "v1", "pods", "ns1", makeMetadataObj(goodChildPod, deployment1apps)),
				// 3,4: observe bad child
				processEvent(makeAddEvent(badChildPod, badSecretReferenceWithDeploymentUID)),
				assertState(state{
					graphNodes: []*node{
						makeNode(badChildPod, withOwners(badSecretReferenceWithDeploymentUID)),
						makeNode(badSecretReferenceWithDeploymentUID, virtual)},
					pendingAttemptToDelete: []*node{
						makeNode(badSecretReferenceWithDeploymentUID, virtual)}, // virtual parent enqueued for delete attempt
				}),

				// 5,6: observe good child
				processEvent(makeAddEvent(goodChildPod, deployment1apps)),
				assertState(state{
					graphNodes: []*node{
						makeNode(goodChildPod, withOwners(deployment1apps)), // good child added
						makeNode(badChildPod, withOwners(badSecretReferenceWithDeploymentUID)),
						makeNode(badSecretReferenceWithDeploymentUID, virtual)},
					pendingAttemptToDelete: []*node{
						makeNode(badSecretReferenceWithDeploymentUID, virtual), // virtual parent enqueued for delete attempt
						makeNode(goodChildPod, withOwners(deployment1apps)),    // good child enqueued for delete attempt
					},
				}),

				// 7,8: process pending delete of virtual parent
				processAttemptToDelete(1),
				assertState(state{
					clientActions: []string{
						"get /v1, Resource=secrets ns=ns1 name=secretname", // lookup of bad parent reference
					},
					pendingGraphChanges: []*event{makeVirtualDeleteEvent(badSecretReferenceWithDeploymentUID)}, // bad virtual parent not found, queued virtual delete event
					graphNodes: []*node{
						makeNode(goodChildPod, withOwners(deployment1apps)),
						makeNode(badChildPod, withOwners(badSecretReferenceWithDeploymentUID)),
						makeNode(badSecretReferenceWithDeploymentUID, virtual)},
					pendingAttemptToDelete: []*node{
						makeNode(goodChildPod, withOwners(deployment1apps)), // good child enqueued for delete attempt
					},
				}),

				// 9,10: process pending delete of good child, gets 200, remains
				processAttemptToDelete(1),
				assertState(state{
					clientActions: []string{
						"get /v1, Resource=pods ns=ns1 name=goodpod",                // lookup of child pre-delete
						"get apps/v1, Resource=deployments ns=ns1 name=deployment1", // lookup of good parent reference, returns 200
					},
					pendingGraphChanges: []*event{makeVirtualDeleteEvent(badSecretReferenceWithDeploymentUID)}, // bad virtual parent not found, queued virtual delete event
					graphNodes: []*node{
						makeNode(goodChildPod, withOwners(deployment1apps)),
						makeNode(badChildPod, withOwners(badSecretReferenceWithDeploymentUID)),
						makeNode(badSecretReferenceWithDeploymentUID, virtual)},
				}),

				// 11,12: good parent add event
				insertEvent(makeAddEvent(deployment1apps)),
				assertState(state{
					pendingGraphChanges: []*event{
						makeAddEvent(deployment1apps),                                // good parent observation sneaked in
						makeVirtualDeleteEvent(badSecretReferenceWithDeploymentUID)}, // bad virtual parent not found, queued virtual delete event
					graphNodes: []*node{
						makeNode(goodChildPod, withOwners(deployment1apps)),
						makeNode(badChildPod, withOwners(badSecretReferenceWithDeploymentUID)),
						makeNode(badSecretReferenceWithDeploymentUID, virtual)},
				}),

				// 13,14: process good parent add
				processPendingGraphChanges(1),
				assertState(state{
					pendingGraphChanges: []*event{
						makeVirtualDeleteEvent(badSecretReferenceWithDeploymentUID)}, // bad virtual parent still queued virtual delete event
					graphNodes: []*node{
						makeNode(goodChildPod, withOwners(deployment1apps)),
						makeNode(badChildPod, withOwners(badSecretReferenceWithDeploymentUID)),
						makeNode(deployment1apps)}, // parent node gets fixed, no longer virtual
					pendingAttemptToDelete: []*node{
						makeNode(badChildPod, withOwners(badSecretReferenceWithDeploymentUID))}, // child of bad parent coordinates enqueued for delete attempt
				}),

				// 15,16: process virtual delete event of bad parent reference
				processPendingGraphChanges(1),
				assertState(state{
					graphNodes: []*node{
						makeNode(goodChildPod, withOwners(deployment1apps)),
						makeNode(badChildPod, withOwners(badSecretReferenceWithDeploymentUID)),
						makeNode(deployment1apps)},
					absentOwnerCache: []objectReference{badSecretReferenceWithDeploymentUID}, // remember absence of bad parent coordinates
					pendingAttemptToDelete: []*node{
						makeNode(badChildPod, withOwners(badSecretReferenceWithDeploymentUID)), // child of bad parent coordinates enqueued for delete attempt
					},
				}),

				// 17,18: process pending delete of bad child
				processAttemptToDelete(1),
				assertState(state{
					clientActions: []string{
						"get /v1, Resource=pods ns=ns1 name=badpod",    // lookup of child pre-delete
						"delete /v1, Resource=pods ns=ns1 name=badpod", // delete of bad child (absence of bad parent is cached)
					},
					graphNodes: []*node{
						makeNode(goodChildPod, withOwners(deployment1apps)),
						makeNode(badChildPod, withOwners(badSecretReferenceWithDeploymentUID)),
						makeNode(deployment1apps)},
					absentOwnerCache: []objectReference{badSecretReferenceWithDeploymentUID},
				}),

				// 19,20: observe delete of bad child from step 17
				// final state: good parent in graph with correct coordinates, good children remain, no pending deletions
				processEvent(makeDeleteEvent(badChildPod, badSecretReferenceWithDeploymentUID)),
				assertState(state{
					graphNodes: []*node{
						makeNode(goodChildPod, withOwners(deployment1apps)),
						// bad child node removed
						makeNode(deployment1apps)},
					absentOwnerCache: []objectReference{badSecretReferenceWithDeploymentUID},
				}),
			},
		},
		{
			name: "bad child -> existing owner with incorrect API version (good child, bad child, good parent)",
			steps: []step{
				// setup
				createObjectInClient("apps", "v1", "deployments", "ns1", makeMetadataObj(deployment1apps)),
				createObjectInClient("", "v1", "pods", "ns1", makeMetadataObj(badChildPod, badSecretReferenceWithDeploymentUID)),
				createObjectInClient("", "v1", "pods", "ns1", makeMetadataObj(goodChildPod, deployment1apps)),
				// 3,4: observe good child
				processEvent(makeAddEvent(goodChildPod, deployment1apps)),
				assertState(state{
					graphNodes: []*node{
						makeNode(goodChildPod, withOwners(deployment1apps)), // good child added
						makeNode(deployment1apps, virtual)},                 // virtual parent added
					pendingAttemptToDelete: []*node{
						makeNode(deployment1apps, virtual), // virtual parent enqueued for delete attempt
					},
				}),

				// 5,6: observe bad child
				processEvent(makeAddEvent(badChildPod, badSecretReferenceWithDeploymentUID)),
				assertState(state{
					graphNodes: []*node{
						makeNode(goodChildPod, withOwners(deployment1apps)),
						makeNode(deployment1apps, virtual),
						makeNode(badChildPod, withOwners(badSecretReferenceWithDeploymentUID))}, // bad child added
					pendingAttemptToDelete: []*node{
						makeNode(deployment1apps, virtual),                                     // virtual parent enqueued for delete attempt
						makeNode(badChildPod, withOwners(badSecretReferenceWithDeploymentUID)), // bad child enqueued for delete attempt
					},
				}),

				// 7,8: process pending delete of virtual parent
				processAttemptToDelete(1),
				assertState(state{
					clientActions: []string{
						"get apps/v1, Resource=deployments ns=ns1 name=deployment1", // lookup of good parent reference, returns 200
					},
					graphNodes: []*node{
						makeNode(goodChildPod, withOwners(deployment1apps)),
						makeNode(deployment1apps, virtual),
						makeNode(badChildPod, withOwners(badSecretReferenceWithDeploymentUID))},
					pendingAttemptToDelete: []*node{
						makeNode(badChildPod, withOwners(badSecretReferenceWithDeploymentUID)), // bad child enqueued for delete attempt
						makeNode(deployment1apps, virtual),                                     // virtual parent requeued to end, still virtual
					},
				}),

				// 9,10: process pending delete of bad child
				processAttemptToDelete(1),
				assertState(state{
					clientActions: []string{
						"get /v1, Resource=pods ns=ns1 name=badpod",        // lookup of child pre-delete
						"get /v1, Resource=secrets ns=ns1 name=secretname", // lookup of bad parent reference, returns 404
						"delete /v1, Resource=pods ns=ns1 name=badpod",     // delete of bad child
					},
					graphNodes: []*node{
						makeNode(goodChildPod, withOwners(deployment1apps)),
						makeNode(deployment1apps, virtual),
						makeNode(badChildPod, withOwners(badSecretReferenceWithDeploymentUID))},
					absentOwnerCache: []objectReference{badSecretReferenceWithDeploymentUID}, // remember absence of bad parent
					pendingAttemptToDelete: []*node{
						makeNode(deployment1apps, virtual), // virtual parent requeued to end, still virtual
					},
				}),

				// 11,12: observe good parent
				processEvent(makeAddEvent(deployment1apps)),
				assertState(state{
					graphNodes: []*node{
						makeNode(goodChildPod, withOwners(deployment1apps)),
						makeNode(deployment1apps), // good parent no longer virtual
						makeNode(badChildPod, withOwners(badSecretReferenceWithDeploymentUID))},
					absentOwnerCache: []objectReference{badSecretReferenceWithDeploymentUID},
					pendingAttemptToDelete: []*node{
						makeNode(deployment1apps), // parent requeued to end, no longer virtual
					},
				}),

				// 13,14: observe delete of bad child from step 9
				processEvent(makeDeleteEvent(badChildPod, badSecretReferenceWithDeploymentUID)),
				assertState(state{
					graphNodes: []*node{
						makeNode(goodChildPod, withOwners(deployment1apps)),
						// bad child node removed
						makeNode(deployment1apps)},
					absentOwnerCache: []objectReference{badSecretReferenceWithDeploymentUID},
					pendingAttemptToDelete: []*node{
						makeNode(deployment1apps), // parent requeued to end, no longer virtual
					},
				}),

				// 15,16: process pending delete of good parent
				// final state: good parent in graph with correct coordinates, good children remain, no pending deletions
				processAttemptToDelete(1),
				assertState(state{
					clientActions: []string{
						"get apps/v1, Resource=deployments ns=ns1 name=deployment1", // lookup of good parent, returns 200
					},
					graphNodes: []*node{
						makeNode(goodChildPod, withOwners(deployment1apps)),
						makeNode(deployment1apps)},
					absentOwnerCache: []objectReference{badSecretReferenceWithDeploymentUID},
				}),
			},
		},
		{
			// https://github.com/kubernetes/kubernetes/issues/98040
			name: "cluster-scoped bad child, namespaced good child, missing parent",
			steps: []step{
				// setup
				createObjectInClient("", "v1", "pods", "ns1", makeMetadataObj(pod2ns1, pod1ns1)),     // good child
				createObjectInClient("", "v1", "nodes", "", makeMetadataObj(node1, pod1nonamespace)), // bad child

				// 2,3: observe bad child
				processEvent(makeAddEvent(node1, pod1nonamespace)),
				assertState(state{
					graphNodes: []*node{
						makeNode(node1, withOwners(pod1nonamespace)),
						makeNode(pod1nonamespace, virtual)},
					pendingAttemptToDelete: []*node{
						makeNode(pod1nonamespace, virtual)}, // virtual parent queued for deletion
				}),

				// 4,5: observe good child
				processEvent(makeAddEvent(pod2ns1, pod1ns1)),
				assertState(state{
					graphNodes: []*node{
						makeNode(node1, withOwners(pod1nonamespace)),
						makeNode(pod2ns1, withOwners(pod1ns1)),
						makeNode(pod1nonamespace, virtual)},
					pendingAttemptToDelete: []*node{
						makeNode(pod1nonamespace, virtual),     // virtual parent queued for deletion
						makeNode(pod2ns1, withOwners(pod1ns1)), // mismatched child queued for deletion
					},
				}),

				// 6,7: process attemptToDelete of bad virtual parent coordinates
				processAttemptToDelete(1),
				assertState(state{
					graphNodes: []*node{
						makeNode(node1, withOwners(pod1nonamespace)),
						makeNode(pod2ns1, withOwners(pod1ns1)),
						makeNode(pod1nonamespace, virtual)},
					pendingAttemptToDelete: []*node{
						makeNode(pod2ns1, withOwners(pod1ns1))}, // mismatched child queued for deletion
				}),

				// 8,9: process attemptToDelete of good child
				processAttemptToDelete(1),
				assertState(state{
					clientActions: []string{
						"get /v1, Resource=pods ns=ns1 name=podname2",    // get good child, returns 200
						"get /v1, Resource=pods ns=ns1 name=podname1",    // get missing parent, returns 404
						"delete /v1, Resource=pods ns=ns1 name=podname2", // delete good child
					},
					graphNodes: []*node{
						makeNode(node1, withOwners(pod1nonamespace)),
						makeNode(pod2ns1, withOwners(pod1ns1)),
						makeNode(pod1nonamespace, virtual)},
					absentOwnerCache: []objectReference{pod1ns1}, // missing parent cached
				}),

				// 10,11: observe deletion of good child
				// steady-state is bad cluster child and bad virtual parent coordinates, with no retries
				processEvent(makeDeleteEvent(pod2ns1, pod1ns1)),
				assertState(state{
					graphNodes: []*node{
						makeNode(node1, withOwners(pod1nonamespace)),
						makeNode(pod1nonamespace, virtual)},
					absentOwnerCache: []objectReference{pod1ns1},
				}),
			},
		},
		{
			// https://github.com/kubernetes/kubernetes/issues/98040
			name: "cluster-scoped bad child, namespaced good child, late observed parent",
			steps: []step{
				// setup
				createObjectInClient("", "v1", "pods", "ns1", makeMetadataObj(pod1ns1)),              // good parent
				createObjectInClient("", "v1", "pods", "ns1", makeMetadataObj(pod2ns1, pod1ns1)),     // good child
				createObjectInClient("", "v1", "nodes", "", makeMetadataObj(node1, pod1nonamespace)), // bad child

				// 3,4: observe bad child
				processEvent(makeAddEvent(node1, pod1nonamespace)),
				assertState(state{
					graphNodes: []*node{
						makeNode(node1, withOwners(pod1nonamespace)),
						makeNode(pod1nonamespace, virtual)},
					pendingAttemptToDelete: []*node{
						makeNode(pod1nonamespace, virtual)}, // virtual parent queued for deletion
				}),

				// 5,6: observe good child
				processEvent(makeAddEvent(pod2ns1, pod1ns1)),
				assertState(state{
					graphNodes: []*node{
						makeNode(node1, withOwners(pod1nonamespace)),
						makeNode(pod2ns1, withOwners(pod1ns1)),
						makeNode(pod1nonamespace, virtual)},
					pendingAttemptToDelete: []*node{
						makeNode(pod1nonamespace, virtual),      // virtual parent queued for deletion
						makeNode(pod2ns1, withOwners(pod1ns1))}, // mismatched child queued for deletion
				}),

				// 7,8: process attemptToDelete of bad virtual parent coordinates
				processAttemptToDelete(1),
				assertState(state{
					graphNodes: []*node{
						makeNode(node1, withOwners(pod1nonamespace)),
						makeNode(pod2ns1, withOwners(pod1ns1)),
						makeNode(pod1nonamespace, virtual)},
					pendingAttemptToDelete: []*node{
						makeNode(pod2ns1, withOwners(pod1ns1))}, // mismatched child queued for deletion
				}),

				// 9,10: process attemptToDelete of good child
				processAttemptToDelete(1),
				assertState(state{
					clientActions: []string{
						"get /v1, Resource=pods ns=ns1 name=podname2", // get good child, returns 200
						"get /v1, Resource=pods ns=ns1 name=podname1", // get late-observed parent, returns 200
					},
					graphNodes: []*node{
						makeNode(node1, withOwners(pod1nonamespace)),
						makeNode(pod2ns1, withOwners(pod1ns1)),
						makeNode(pod1nonamespace, virtual)},
				}),

				// 11,12: late observe good parent
				processEvent(makeAddEvent(pod1ns1)),
				assertState(state{
					graphNodes: []*node{
						makeNode(node1, withOwners(pod1nonamespace)),
						makeNode(pod2ns1, withOwners(pod1ns1)),
						makeNode(pod1ns1)},
					// warn about bad node reference
					events: []string{`Warning OwnerRefInvalidNamespace ownerRef [v1/Pod, namespace: , name: podname1, uid: poduid1] does not exist in namespace "" involvedObject{kind=Node,apiVersion=v1}`},
					pendingAttemptToDelete: []*node{
						makeNode(node1, withOwners(pod1nonamespace))}, // queue bad cluster-scoped child for delete attempt
				}),

				// 13,14: process attemptToDelete of bad child
				// steady state is bad cluster-scoped child remaining with no retries, good parent and good child in graph
				processAttemptToDelete(1),
				assertState(state{
					clientActions: []string{
						"get /v1, Resource=nodes name=nodename", // get bad child, returns 200
					},
					graphNodes: []*node{
						makeNode(node1, withOwners(pod1nonamespace)),
						makeNode(pod2ns1, withOwners(pod1ns1)),
						makeNode(pod1ns1)},
				}),
			},
		},
		{
			// https://github.com/kubernetes/kubernetes/issues/98040
			name: "namespaced good child, cluster-scoped bad child, missing parent",
			steps: []step{
				// setup
				createObjectInClient("", "v1", "pods", "ns1", makeMetadataObj(pod2ns1, pod1ns1)),     // good child
				createObjectInClient("", "v1", "nodes", "", makeMetadataObj(node1, pod1nonamespace)), // bad child

				// 2,3: observe good child
				processEvent(makeAddEvent(pod2ns1, pod1ns1)),
				assertState(state{
					graphNodes: []*node{
						makeNode(pod2ns1, withOwners(pod1ns1)),
						makeNode(pod1ns1, virtual)},
					pendingAttemptToDelete: []*node{
						makeNode(pod1ns1, virtual)}, // virtual parent queued for deletion
				}),

				// 4,5: observe bad child
				processEvent(makeAddEvent(node1, pod1nonamespace)),
				assertState(state{
					graphNodes: []*node{
						makeNode(pod2ns1, withOwners(pod1ns1)),
						makeNode(node1, withOwners(pod1nonamespace)),
						makeNode(pod1ns1, virtual)},
					pendingAttemptToDelete: []*node{
						makeNode(pod1ns1, virtual),                   // virtual parent queued for deletion
						makeNode(node1, withOwners(pod1nonamespace)), // mismatched child queued for deletion
					},
				}),

				// 6,7: process attemptToDelete of good virtual parent coordinates
				processAttemptToDelete(1),
				assertState(state{
					clientActions: []string{
						"get /v1, Resource=pods ns=ns1 name=podname1", // lookup of missing parent, returns 404
					},
					graphNodes: []*node{
						makeNode(node1, withOwners(pod1nonamespace)),
						makeNode(pod2ns1, withOwners(pod1ns1)),
						makeNode(pod1ns1, virtual)},
					pendingGraphChanges: []*event{makeVirtualDeleteEvent(pod1ns1)}, // virtual parent not found, queued virtual delete event
					pendingAttemptToDelete: []*node{
						makeNode(node1, withOwners(pod1nonamespace)), // mismatched child still queued for deletion
					},
				}),

				// 8,9: process attemptToDelete of bad cluster child
				processAttemptToDelete(1),
				assertState(state{
					clientActions: []string{
						"get /v1, Resource=nodes name=nodename", // lookup of existing node
					},
					graphNodes: []*node{
						makeNode(node1, withOwners(pod1nonamespace)),
						makeNode(pod2ns1, withOwners(pod1ns1)),
						makeNode(pod1ns1, virtual)},
					pendingGraphChanges: []*event{makeVirtualDeleteEvent(pod1ns1)}, // virtual parent virtual delete event still enqueued
				}),

				// 10,11: process virtual delete event for good virtual parent coordinates
				processPendingGraphChanges(1),
				assertState(state{
					graphNodes: []*node{
						makeNode(node1, withOwners(pod1nonamespace)),
						makeNode(pod2ns1, withOwners(pod1ns1)),
						makeNode(pod1nonamespace, virtual)}, // missing virtual parent replaced with alternate coordinates, still virtual
					absentOwnerCache: []objectReference{pod1ns1}, // cached absence of missing parent
					pendingAttemptToDelete: []*node{
						makeNode(pod2ns1, withOwners(pod1ns1)), // good child of missing parent enqueued for deletion
						makeNode(pod1nonamespace, virtual),     // new virtual parent coordinates enqueued for deletion
					},
				}),

				// 12,13: process attemptToDelete of good child
				processAttemptToDelete(1),
				assertState(state{
					clientActions: []string{
						"get /v1, Resource=pods ns=ns1 name=podname2",    // lookup of good child
						"delete /v1, Resource=pods ns=ns1 name=podname2", // delete of good child
					},
					graphNodes: []*node{
						makeNode(node1, withOwners(pod1nonamespace)),
						makeNode(pod2ns1, withOwners(pod1ns1)),
						makeNode(pod1nonamespace, virtual)},
					absentOwnerCache: []objectReference{pod1ns1},
					pendingAttemptToDelete: []*node{
						makeNode(pod1nonamespace, virtual), // new virtual parent coordinates enqueued for deletion
					},
				}),

				// 14,15: observe deletion of good child
				processEvent(makeDeleteEvent(pod2ns1, pod1ns1)),
				assertState(state{
					graphNodes: []*node{
						makeNode(node1, withOwners(pod1nonamespace)),
						makeNode(pod1nonamespace, virtual)},
					absentOwnerCache: []objectReference{pod1ns1},
					pendingAttemptToDelete: []*node{
						makeNode(pod1nonamespace, virtual), // new virtual parent coordinates enqueued for deletion
					},
				}),

				// 16,17: process attemptToDelete of bad virtual parent coordinates
				// steady-state is bad cluster child and bad virtual parent coordinates, with no retries
				processAttemptToDelete(1),
				assertState(state{
					graphNodes: []*node{
						makeNode(node1, withOwners(pod1nonamespace)),
						makeNode(pod1nonamespace, virtual)},
					absentOwnerCache: []objectReference{pod1ns1},
				}),
			},
		},
	}

	alwaysStarted := make(chan struct{})
	close(alwaysStarted)
	for _, scenario := range testScenarios {
		t.Run(scenario.name, func(t *testing.T) {

			absentOwnerCache := NewReferenceCache(100)

			eventRecorder := record.NewFakeRecorder(100)
			eventRecorder.IncludeObject = true

			metadataClient := fakemetadata.NewSimpleMetadataClient(fakemetadata.NewTestScheme())

			tweakableRM := meta.NewDefaultRESTMapper(nil)
			tweakableRM.AddSpecific(
				schema.GroupVersionKind{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "Role"},
				schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "roles"},
				schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "role"},
				meta.RESTScopeNamespace,
			)
			tweakableRM.AddSpecific(
				schema.GroupVersionKind{Group: "rbac.authorization.k8s.io", Version: "v1beta1", Kind: "Role"},
				schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1beta1", Resource: "roles"},
				schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1beta1", Resource: "role"},
				meta.RESTScopeNamespace,
			)
			tweakableRM.AddSpecific(
				schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"},
				schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"},
				schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployment"},
				meta.RESTScopeNamespace,
			)
			restMapper := &testRESTMapper{meta.MultiRESTMapper{tweakableRM, testrestmapper.TestOnlyStaticRESTMapper(legacyscheme.Scheme)}}

			// set up our workqueues
			attemptToDelete := newTrackingWorkqueue[*node]()
			attemptToOrphan := newTrackingWorkqueue[*node]()
			graphChanges := newTrackingWorkqueue[*event]()

			gc := &GarbageCollector{
				metadataClient:   metadataClient,
				restMapper:       restMapper,
				attemptToDelete:  attemptToDelete,
				attemptToOrphan:  attemptToOrphan,
				absentOwnerCache: absentOwnerCache,
				dependencyGraphBuilder: &GraphBuilder{
					eventRecorder:    eventRecorder,
					metadataClient:   metadataClient,
					informersStarted: alwaysStarted,
					graphChanges:     graphChanges,
					uidToNode: &concurrentUIDToNode{
						uidToNodeLock: sync.RWMutex{},
						uidToNode:     make(map[types.UID]*node),
					},
					attemptToDelete:  attemptToDelete,
					absentOwnerCache: absentOwnerCache,
				},
			}

			logger, _ := ktesting.NewTestContext(t)

			ctx := stepContext{
				t:               t,
				logger:          logger,
				gc:              gc,
				eventRecorder:   eventRecorder,
				metadataClient:  metadataClient,
				attemptToDelete: attemptToDelete,
				attemptToOrphan: attemptToOrphan,
				graphChanges:    graphChanges,
			}
			for i, s := range scenario.steps {
				ctx.t.Logf("%d: %s", i, s.name)
				s.check(ctx)
				if ctx.t.Failed() {
					return
				}
				verifyGraphInvariants(fmt.Sprintf("after step %d", i), gc.dependencyGraphBuilder.uidToNode.uidToNode, t)
				if ctx.t.Failed() {
					return
				}
			}
		})
	}
}

func makeID(groupVersion string, kind string, namespace, name, uid string) objectReference {
	return objectReference{
		OwnerReference: metav1.OwnerReference{APIVersion: groupVersion, Kind: kind, Name: name, UID: types.UID(uid)},
		Namespace:      namespace,
	}
}

type nodeTweak func(*node) *node

func virtual(n *node) *node {
	n.virtual = true
	return n
}
func withOwners(ownerReferences ...objectReference) nodeTweak {
	return func(n *node) *node {
		var owners []metav1.OwnerReference
		for _, o := range ownerReferences {
			owners = append(owners, o.OwnerReference)
		}
		n.owners = owners
		return n
	}
}

func makeNode(identity objectReference, tweaks ...nodeTweak) *node {
	n := &node{identity: identity}
	for _, tweak := range tweaks {
		n = tweak(n)
	}
	return n
}

func makeAddEvent(identity objectReference, owners ...objectReference) *event {
	gv, err := schema.ParseGroupVersion(identity.APIVersion)
	if err != nil {
		panic(err)
	}
	return &event{
		eventType: addEvent,
		gvk:       gv.WithKind(identity.Kind),
		obj:       makeObj(identity, owners...),
	}
}

func makeVirtualDeleteEvent(identity objectReference, owners ...objectReference) *event {
	e := makeDeleteEvent(identity, owners...)
	e.virtual = true
	return e
}

func makeDeleteEvent(identity objectReference, owners ...objectReference) *event {
	gv, err := schema.ParseGroupVersion(identity.APIVersion)
	if err != nil {
		panic(err)
	}
	return &event{
		eventType: deleteEvent,
		gvk:       gv.WithKind(identity.Kind),
		obj:       makeObj(identity, owners...),
	}
}

func makeObj(identity objectReference, owners ...objectReference) *metaonly.MetadataOnlyObject {
	obj := &metaonly.MetadataOnlyObject{
		TypeMeta:   metav1.TypeMeta{APIVersion: identity.APIVersion, Kind: identity.Kind},
		ObjectMeta: metav1.ObjectMeta{Namespace: identity.Namespace, UID: identity.UID, Name: identity.Name},
	}
	for _, owner := range owners {
		obj.ObjectMeta.OwnerReferences = append(obj.ObjectMeta.OwnerReferences, owner.OwnerReference)
	}
	return obj
}

func makeMetadataObj(identity objectReference, owners ...objectReference) *metav1.PartialObjectMetadata {
	obj := &metav1.PartialObjectMetadata{
		TypeMeta:   metav1.TypeMeta{APIVersion: identity.APIVersion, Kind: identity.Kind},
		ObjectMeta: metav1.ObjectMeta{Namespace: identity.Namespace, UID: identity.UID, Name: identity.Name},
	}
	for _, owner := range owners {
		obj.ObjectMeta.OwnerReferences = append(obj.ObjectMeta.OwnerReferences, owner.OwnerReference)
	}
	return obj
}

type stepContext struct {
	t               *testing.T
	logger          klog.Logger
	gc              *GarbageCollector
	eventRecorder   *record.FakeRecorder
	metadataClient  *fakemetadata.FakeMetadataClient
	attemptToDelete *trackingWorkqueue[*node]
	attemptToOrphan *trackingWorkqueue[*node]
	graphChanges    *trackingWorkqueue[*event]
}

type step struct {
	name  string
	check func(stepContext)
}

func processPendingGraphChanges(count int) step {
	return step{
		name: "processPendingGraphChanges",
		check: func(ctx stepContext) {
			ctx.t.Helper()
			if count <= 0 {
				// process all
				for ctx.gc.dependencyGraphBuilder.graphChanges.Len() != 0 {
					ctx.gc.dependencyGraphBuilder.processGraphChanges(ctx.logger)
				}
			} else {
				for i := 0; i < count; i++ {
					if ctx.gc.dependencyGraphBuilder.graphChanges.Len() == 0 {
						ctx.t.Errorf("expected at least %d pending changes, got %d", count, i+1)
						return
					}
					ctx.gc.dependencyGraphBuilder.processGraphChanges(ctx.logger)
				}
			}
		},
	}
}

func processAttemptToDelete(count int) step {
	return step{
		name: "processAttemptToDelete",
		check: func(ctx stepContext) {
			ctx.t.Helper()
			if count <= 0 {
				// process all
				for ctx.gc.dependencyGraphBuilder.attemptToDelete.Len() != 0 {
					ctx.gc.processAttemptToDeleteWorker(context.TODO())
				}
			} else {
				for i := 0; i < count; i++ {
					if ctx.gc.dependencyGraphBuilder.attemptToDelete.Len() == 0 {
						ctx.t.Errorf("expected at least %d pending changes, got %d", count, i+1)
						return
					}
					ctx.gc.processAttemptToDeleteWorker(context.TODO())
				}
			}
		},
	}
}

func insertEvent(e *event) step {
	return step{
		name: "insertEvent",
		check: func(ctx stepContext) {
			ctx.t.Helper()
			// drain queue into items
			var items []*event
			for ctx.gc.dependencyGraphBuilder.graphChanges.Len() > 0 {
				item, _ := ctx.gc.dependencyGraphBuilder.graphChanges.Get()
				ctx.gc.dependencyGraphBuilder.graphChanges.Done(item)
				items = append(items, item)
			}

			// add the new event
			ctx.gc.dependencyGraphBuilder.graphChanges.Add(e)

			// reappend the items
			for _, item := range items {
				ctx.gc.dependencyGraphBuilder.graphChanges.Add(item)
			}
		},
	}
}

func processEvent(e *event) step {
	return step{
		name: "processEvent",
		check: func(ctx stepContext) {
			ctx.t.Helper()
			if ctx.gc.dependencyGraphBuilder.graphChanges.Len() != 0 {
				ctx.t.Fatalf("events present in graphChanges, must process pending graphChanges before calling processEvent")
			}
			ctx.gc.dependencyGraphBuilder.graphChanges.Add(e)
			ctx.gc.dependencyGraphBuilder.processGraphChanges(ctx.logger)
		},
	}
}

func createObjectInClient(group, version, resource, namespace string, obj *metav1.PartialObjectMetadata) step {
	return step{
		name: "createObjectInClient",
		check: func(ctx stepContext) {
			ctx.t.Helper()
			if len(ctx.metadataClient.Actions()) > 0 {
				ctx.t.Fatal("cannot call createObjectInClient with pending client actions, call assertClientActions to check and clear first")
			}
			gvr := schema.GroupVersionResource{Group: group, Version: version, Resource: resource}
			var c fakemetadata.MetadataClient
			if namespace == "" {
				c = ctx.metadataClient.Resource(gvr).(fakemetadata.MetadataClient)
			} else {
				c = ctx.metadataClient.Resource(gvr).Namespace(namespace).(fakemetadata.MetadataClient)
			}
			if _, err := c.CreateFake(obj, metav1.CreateOptions{}); err != nil {
				ctx.t.Fatal(err)
			}
			ctx.metadataClient.ClearActions()
		},
	}
}

func deleteObjectFromClient(group, version, resource, namespace, name string) step {
	return step{
		name: "deleteObjectFromClient",
		check: func(ctx stepContext) {
			ctx.t.Helper()
			if len(ctx.metadataClient.Actions()) > 0 {
				ctx.t.Fatal("cannot call deleteObjectFromClient with pending client actions, call assertClientActions to check and clear first")
			}
			gvr := schema.GroupVersionResource{Group: group, Version: version, Resource: resource}
			var c fakemetadata.MetadataClient
			if namespace == "" {
				c = ctx.metadataClient.Resource(gvr).(fakemetadata.MetadataClient)
			} else {
				c = ctx.metadataClient.Resource(gvr).Namespace(namespace).(fakemetadata.MetadataClient)
			}
			if err := c.Delete(context.TODO(), name, metav1.DeleteOptions{}); err != nil {
				ctx.t.Fatal(err)
			}
			ctx.metadataClient.ClearActions()
		},
	}
}

type state struct {
	events                 []string
	clientActions          []string
	graphNodes             []*node
	pendingGraphChanges    []*event
	pendingAttemptToDelete []*node
	pendingAttemptToOrphan []*node
	absentOwnerCache       []objectReference
}

func assertState(s state) step {
	return step{
		name: "assertState",
		check: func(ctx stepContext) {
			ctx.t.Helper()

			{
				for _, absent := range s.absentOwnerCache {
					if !ctx.gc.absentOwnerCache.Has(absent) {
						ctx.t.Errorf("expected absent owner %s was not in the absentOwnerCache", absent)
					}
				}
				if len(s.absentOwnerCache) != ctx.gc.absentOwnerCache.cache.Len() {
					// only way to inspect is to drain them all, but that's ok because we're failing the test anyway
					err := ctx.gc.absentOwnerCache.cache.SetEvictionFunc(func(key lru.Key, item interface{}) {
						found := false
						for _, absent := range s.absentOwnerCache {
							if absent == key {
								found = true
								break
							}
						}
						if !found {
							ctx.t.Errorf("unexpected item in absent owner cache: %s", key)
						}
					})
					if err != nil {
						ctx.t.Error("unexpected error setting eviction function: %w", err)
					}
					ctx.gc.absentOwnerCache.cache.Clear()
					ctx.t.Error("unexpected items in absent owner cache")
				}
			}

			{
				var actualEvents []string
				// drain sent events
			loop:
				for {
					select {
					case event := <-ctx.eventRecorder.Events:
						actualEvents = append(actualEvents, event)
					default:
						break loop
					}
				}
				if !reflect.DeepEqual(actualEvents, s.events) {
					ctx.t.Logf("expected:\n%s", strings.Join(s.events, "\n"))
					ctx.t.Logf("actual:\n%s", strings.Join(actualEvents, "\n"))
					ctx.t.Fatalf("did not get expected events")
				}
			}

			{
				var actualClientActions []string
				for _, action := range ctx.metadataClient.Actions() {
					s := fmt.Sprintf("%s %s", action.GetVerb(), action.GetResource())
					if action.GetNamespace() != "" {
						s += " ns=" + action.GetNamespace()
					}
					if get, ok := action.(clientgotesting.GetAction); ok && get.GetName() != "" {
						s += " name=" + get.GetName()
					}
					actualClientActions = append(actualClientActions, s)
				}
				if (len(s.clientActions) > 0 || len(actualClientActions) > 0) && !reflect.DeepEqual(s.clientActions, actualClientActions) {
					ctx.t.Logf("expected:\n%s", strings.Join(s.clientActions, "\n"))
					ctx.t.Logf("actual:\n%s", strings.Join(actualClientActions, "\n"))
					ctx.t.Fatalf("did not get expected client actions")
				}
				ctx.metadataClient.ClearActions()
			}

			{
				if l := len(ctx.gc.dependencyGraphBuilder.uidToNode.uidToNode); l != len(s.graphNodes) {
					ctx.t.Errorf("expected %d nodes, got %d", len(s.graphNodes), l)
				}
				for _, n := range s.graphNodes {
					graphNode, ok := ctx.gc.dependencyGraphBuilder.uidToNode.Read(n.identity.UID)
					if !ok {
						ctx.t.Errorf("%s: no node in graph with uid=%s", n.identity.UID, n.identity.UID)
						continue
					}
					if graphNode.identity != n.identity {
						ctx.t.Errorf("%s: expected identity %v, got %v", n.identity.UID, n.identity, graphNode.identity)
					}
					if graphNode.virtual != n.virtual {
						ctx.t.Errorf("%s: expected virtual %v, got %v", n.identity.UID, n.virtual, graphNode.virtual)
					}
					if (len(graphNode.owners) > 0 || len(n.owners) > 0) && !reflect.DeepEqual(graphNode.owners, n.owners) {
						expectedJSON, _ := json.Marshal(n.owners)
						actualJSON, _ := json.Marshal(graphNode.owners)
						ctx.t.Errorf("%s: expected owners %s, got %s", n.identity.UID, expectedJSON, actualJSON)
					}
				}
			}

			{
				for i := range s.pendingGraphChanges {
					e := s.pendingGraphChanges[i]
					if len(ctx.graphChanges.pendingList) < i+1 {
						ctx.t.Errorf("graphChanges: expected %d events, got %d", len(s.pendingGraphChanges), ctx.graphChanges.Len())
						break
					}

					a := ctx.graphChanges.pendingList[i]
					if !reflect.DeepEqual(e, a) {
						objectDiff := ""
						if !reflect.DeepEqual(e.obj, a.obj) {
							objectDiff = "\nobjectDiff:\n" + cmp.Diff(e.obj, a.obj)
						}
						oldObjectDiff := ""
						if !reflect.DeepEqual(e.oldObj, a.oldObj) {
							oldObjectDiff = "\noldObjectDiff:\n" + cmp.Diff(e.oldObj, a.oldObj)
						}
						ctx.t.Errorf("graphChanges[%d]: expected\n%#v\ngot\n%#v%s%s", i, e, a, objectDiff, oldObjectDiff)
					}
				}
				if ctx.graphChanges.Len() > len(s.pendingGraphChanges) {
					for i, a := range ctx.graphChanges.pendingList[len(s.pendingGraphChanges):] {
						ctx.t.Errorf("graphChanges[%d]: unexpected event: %v", len(s.pendingGraphChanges)+i, a)
					}
				}
			}

			{
				for i := range s.pendingAttemptToDelete {
					e := s.pendingAttemptToDelete[i].identity
					e_virtual := s.pendingAttemptToDelete[i].virtual
					if ctx.attemptToDelete.Len() < i+1 {
						ctx.t.Errorf("attemptToDelete: expected %d events, got %d", len(s.pendingAttemptToDelete), ctx.attemptToDelete.Len())
						break
					}
					a := ctx.attemptToDelete.pendingList[i].identity
					aVirtual := ctx.attemptToDelete.pendingList[i].virtual
					if !reflect.DeepEqual(e, a) {
						ctx.t.Errorf("attemptToDelete[%d]: expected %v, got %v", i, e, a)
					}
					if e_virtual != aVirtual {
						ctx.t.Errorf("attemptToDelete[%d]: expected virtual node %v, got non-virtual node %v", i, e, a)
					}
				}
				if ctx.attemptToDelete.Len() > len(s.pendingAttemptToDelete) {
					for i, a := range ctx.attemptToDelete.pendingList[len(s.pendingAttemptToDelete):] {
						ctx.t.Errorf("attemptToDelete[%d]: unexpected node: %v", len(s.pendingAttemptToDelete)+i, a.identity)
					}
				}
			}

			{
				for i := range s.pendingAttemptToOrphan {
					e := s.pendingAttemptToOrphan[i].identity
					if ctx.attemptToOrphan.Len() < i+1 {
						ctx.t.Errorf("attemptToOrphan: expected %d events, got %d", len(s.pendingAttemptToOrphan), ctx.attemptToOrphan.Len())
						break
					}
					a := ctx.attemptToOrphan.pendingList[i].identity
					if !reflect.DeepEqual(e, a) {
						ctx.t.Errorf("attemptToOrphan[%d]: expected %v, got %v", i, e, a)
					}
				}
				if ctx.attemptToOrphan.Len() > len(s.pendingAttemptToOrphan) {
					for i, a := range ctx.attemptToOrphan.pendingList[len(s.pendingAttemptToOrphan):] {
						ctx.t.Errorf("attemptToOrphan[%d]: unexpected node: %v", len(s.pendingAttemptToOrphan)+i, a.identity)
					}
				}
			}
		},
	}

}

// trackingWorkqueue implements RateLimitingInterface,
// allows introspection of the items in the queue,
// and treats AddAfter and AddRateLimited the same as Add
// so they are always synchronous.
type trackingWorkqueue[T comparable] struct {
	limiter     workqueue.TypedRateLimitingInterface[T]
	pendingList []T
	pendingMap  map[T]struct{}
}

var _ = workqueue.TypedRateLimitingInterface[string](&trackingWorkqueue[string]{})

func newTrackingWorkqueue[T comparable]() *trackingWorkqueue[T] {
	return &trackingWorkqueue[T]{
		limiter:    workqueue.NewTypedRateLimitingQueue[T](&workqueue.TypedBucketRateLimiter[T]{Limiter: rate.NewLimiter(rate.Inf, 100)}),
		pendingMap: map[T]struct{}{},
	}
}

func (t *trackingWorkqueue[T]) Add(item T) {
	t.queue(item)
	t.limiter.Add(item)
}
func (t *trackingWorkqueue[T]) AddAfter(item T, duration time.Duration) {
	t.Add(item)
}
func (t *trackingWorkqueue[T]) AddRateLimited(item T) {
	t.Add(item)
}
func (t *trackingWorkqueue[T]) Get() (T, bool) {
	item, shutdown := t.limiter.Get()
	t.dequeue(item)
	return item, shutdown
}
func (t *trackingWorkqueue[T]) Done(item T) {
	t.limiter.Done(item)
}
func (t *trackingWorkqueue[T]) Forget(item T) {
	t.limiter.Forget(item)
}
func (t *trackingWorkqueue[T]) NumRequeues(item T) int {
	return 0
}
func (t *trackingWorkqueue[T]) Len() int {
	if e, a := len(t.pendingList), len(t.pendingMap); e != a {
		panic(fmt.Errorf("pendingList != pendingMap: %d / %d", e, a))
	}
	if e, a := len(t.pendingList), t.limiter.Len(); e != a {
		panic(fmt.Errorf("pendingList != limiter.Len(): %d / %d", e, a))
	}
	return len(t.pendingList)
}
func (t *trackingWorkqueue[T]) ShutDown() {
	t.limiter.ShutDown()
}
func (t *trackingWorkqueue[T]) ShutDownWithDrain() {
	t.limiter.ShutDownWithDrain()
}
func (t *trackingWorkqueue[T]) ShuttingDown() bool {
	return t.limiter.ShuttingDown()
}

func (t *trackingWorkqueue[T]) queue(item T) {
	if _, queued := t.pendingMap[item]; queued {
		// fmt.Printf("already queued: %#v\n", item)
		return
	}
	t.pendingMap[item] = struct{}{}
	t.pendingList = append(t.pendingList, item)
}
func (t *trackingWorkqueue[T]) dequeue(item T) {
	if _, queued := t.pendingMap[item]; !queued {
		// fmt.Printf("not queued: %#v\n", item)
		return
	}
	delete(t.pendingMap, item)
	newPendingList := []T{}
	for _, p := range t.pendingList {
		if p == item {
			continue
		}
		newPendingList = append(newPendingList, p)
	}
	t.pendingList = newPendingList
}
