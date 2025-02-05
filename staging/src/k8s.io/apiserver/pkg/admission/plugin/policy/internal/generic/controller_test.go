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

package generic_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/stretchr/testify/require"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"

	"k8s.io/apiserver/pkg/admission/plugin/policy/internal/generic"

	clienttesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
)

type testInformer struct {
	cache.SharedIndexInformer

	lock          sync.Mutex
	registrations map[interface{}]struct{}
}

func (t *testInformer) AddEventHandler(handler cache.ResourceEventHandler) (cache.ResourceEventHandlerRegistration, error) {
	res, err := t.SharedIndexInformer.AddEventHandler(handler)
	if err != nil {
		return res, err
	}

	func() {
		t.lock.Lock()
		defer t.lock.Unlock()
		if t.registrations == nil {
			t.registrations = make(map[interface{}]struct{})
		}
		t.registrations[res] = struct{}{}
	}()

	return res, err
}

func (t *testInformer) RemoveEventHandler(registration cache.ResourceEventHandlerRegistration) error {
	func() {
		t.lock.Lock()
		defer t.lock.Unlock()

		if _, ok := t.registrations[registration]; !ok {
			panic("removing unknown event handler?")
		}
		delete(t.registrations, registration)
	}()

	return t.SharedIndexInformer.RemoveEventHandler(registration)
}

var (
	scheme  *runtime.Scheme             = runtime.NewScheme()
	codecs  serializer.CodecFactory     = serializer.NewCodecFactory(scheme)
	fakeGVR schema.GroupVersionResource = schema.GroupVersionResource{
		Group:    "fake.example.com",
		Version:  "v1",
		Resource: "fakes",
	}
	fakeGVK     schema.GroupVersionKind = fakeGVR.GroupVersion().WithKind("Fake")
	fakeGVKList schema.GroupVersionKind = fakeGVR.GroupVersion().WithKind("FakeList")
)

func init() {
	scheme.AddKnownTypeWithName(fakeGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(fakeGVKList, &unstructured.UnstructuredList{})
}

func setupTest(ctx context.Context, customReconciler func(string, string, runtime.Object) error) (
	tracker clienttesting.ObjectTracker,
	controller generic.Controller[*unstructured.Unstructured],
	informer *testInformer,
	waitForReconcile func(runtime.Object) error,
	verifyNoMoreEvents func() bool,
) {
	tracker = clienttesting.NewObjectTracker(scheme, codecs.UniversalDecoder())
	reconciledObjects := make(chan runtime.Object)

	// Set up fake informers that return instances of mock Policy definitions
	// and mock policy bindings
	informer = &testInformer{SharedIndexInformer: cache.NewSharedIndexInformer(&cache.ListWatch{
		ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
			return tracker.List(fakeGVR, fakeGVK, "")
		},
		WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
			return tracker.Watch(fakeGVR, "")
		},
	}, &unstructured.Unstructured{}, 30*time.Second, nil)}

	reconciler := func(namespace, name string, newObj *unstructured.Unstructured) error {
		var err error
		copied := newObj.DeepCopyObject()
		if customReconciler != nil {
			err = customReconciler(namespace, name, newObj)
		}
		select {
		case reconciledObjects <- copied:
		case <-ctx.Done():
			panic("timed out attempting to deliver reconcile event")
		}
		return err
	}

	waitForReconcile = func(obj runtime.Object) error {
		select {
		case reconciledObj := <-reconciledObjects:
			if reflect.DeepEqual(obj, reconciledObj) {
				return nil
			}
			return fmt.Errorf("expected equal objects: %v", cmp.Diff(obj, reconciledObj))
		case <-ctx.Done():
			return fmt.Errorf("context done before reconcile: %w", ctx.Err())
		}
	}

	myController := generic.NewController(
		generic.NewInformer[*unstructured.Unstructured](informer),
		reconciler,
		generic.ControllerOptions{},
	)

	verifyNoMoreEvents = func() bool {
		close(reconciledObjects) // closing means that a future attempt to send will crash
		for leftover := range reconciledObjects {
			panic(fmt.Errorf("leftover object which was not anticipated by test: %v", leftover))
		}
		// TODO(alexzielenski): this effectively doesn't test anything since the
		// controller drops any pending events when it shuts down.
		return true
	}

	return tracker, myController, informer, waitForReconcile, verifyNoMoreEvents
}

func TestReconcile(t *testing.T) {
	testContext, testCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer testCancel()

	tracker, myController, informer, waitForReconcile, verifyNoMoreEvents := setupTest(testContext, nil)

	// Add object to informer
	initialObject := &unstructured.Unstructured{}
	initialObject.SetUnstructuredContent(map[string]interface{}{
		"metadata": map[string]interface{}{
			"name":            "object1",
			"resourceVersion": "1",
		},
	})
	initialObject.SetGroupVersionKind(fakeGVK)

	require.NoError(t, tracker.Add(initialObject))

	wg := sync.WaitGroup{}

	// Start informer
	wg.Add(1)
	go func() {
		defer wg.Done()
		informer.Run(testContext.Done())
	}()

	// Start controller
	wg.Add(1)
	go func() {
		defer wg.Done()
		stopReason := myController.Run(testContext)
		require.ErrorIs(t, stopReason, context.Canceled)
	}()

	// The controller is blocked because the reconcile function sends on an
	// unbuffered channel.
	require.False(t, myController.HasSynced())

	// Wait for all enqueued reconciliations
	require.NoError(t, waitForReconcile(initialObject))

	// Now it is safe to wait for it to Sync
	require.True(t, cache.WaitForCacheSync(testContext.Done(), myController.HasSynced))

	// Updated object
	updatedObject := &unstructured.Unstructured{}
	updatedObject.SetUnstructuredContent(map[string]interface{}{
		"metadata": map[string]interface{}{
			"name":            "object1",
			"resourceVersion": "2",
		},
		"newKey": "a key",
	})
	updatedObject.SetGroupVersionKind(fakeGVK)
	require.NoError(t, tracker.Update(fakeGVR, updatedObject, ""))

	// Wait for all enqueued reconciliations
	require.NoError(t, waitForReconcile(updatedObject))
	require.NoError(t, tracker.Delete(fakeGVR, updatedObject.GetNamespace(), updatedObject.GetName()))
	require.NoError(t, waitForReconcile(nil))

	testCancel()
	wg.Wait()

	verifyNoMoreEvents()
}

func TestShutdown(t *testing.T) {
	testContext, testCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer testCancel()

	_, myController, informer, _, verifyNoMoreEvents := setupTest(testContext, nil)

	wg := sync.WaitGroup{}

	// Start informer
	wg.Add(1)
	go func() {
		defer wg.Done()
		informer.Run(testContext.Done())
	}()

	// Start controller
	wg.Add(1)
	go func() {
		defer wg.Done()
		stopReason := myController.Run(testContext)
		require.ErrorIs(t, stopReason, context.Canceled)
	}()

	// Wait for controller and informer to start up
	require.True(t, cache.WaitForCacheSync(testContext.Done(), myController.HasSynced))

	// Stop the controller and informer
	testCancel()

	// Wait for controller and informer to stop
	wg.Wait()

	// Ensure the event handler was cleaned up
	require.Empty(t, informer.registrations)

	verifyNoMoreEvents()
}

// Show an error is thrown informer isn't started when the controller runs
func TestInformerNeverStarts(t *testing.T) {
	testContext, testCancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer testCancel()

	_, myController, informer, _, verifyNoMoreEvents := setupTest(testContext, nil)

	wg := sync.WaitGroup{}

	// Start controller
	wg.Add(1)
	go func() {
		defer wg.Done()
		stopReason := myController.Run(testContext)
		require.ErrorIs(t, stopReason, context.DeadlineExceeded)
	}()

	// Wait for deadline to pass without syncing the cache
	require.False(t, cache.WaitForCacheSync(testContext.Done(), myController.HasSynced))

	// Wait for controller to stop (or context deadline will pass quickly)
	wg.Wait()

	// Ensure there are no event handlers
	require.Empty(t, informer.registrations)

	verifyNoMoreEvents()
}

// Shows that if RV does not change, the reconciler does not get called
func TestIgnoredUpdate(t *testing.T) {
	testContext, testCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer testCancel()

	tracker, myController, informer, waitForReconcile, verifyNoMoreEvents := setupTest(testContext, nil)

	// Add object to informer
	initialObject := &unstructured.Unstructured{}
	initialObject.SetUnstructuredContent(map[string]interface{}{
		"metadata": map[string]interface{}{
			"name":            "object1",
			"resourceVersion": "1",
		},
	})
	initialObject.SetGroupVersionKind(fakeGVK)

	require.NoError(t, tracker.Add(initialObject))

	wg := sync.WaitGroup{}

	// Start informer
	wg.Add(1)
	go func() {
		defer wg.Done()
		informer.Run(testContext.Done())
	}()

	// Start controller
	wg.Add(1)
	go func() {
		defer wg.Done()
		stopReason := myController.Run(testContext)
		require.ErrorIs(t, stopReason, context.Canceled)
	}()

	// The controller is blocked because the reconcile function sends on an
	// unbuffered channel.
	require.False(t, myController.HasSynced())

	// Wait for all enqueued reconciliations
	require.NoError(t, waitForReconcile(initialObject))

	// Now it is safe to wait for it to Sync
	require.True(t, cache.WaitForCacheSync(testContext.Done(), myController.HasSynced))

	// Send update with the same object
	require.NoError(t, tracker.Update(fakeGVR, initialObject, ""))

	// Don't wait for it to be reconciled

	testCancel()
	wg.Wait()

	// TODO(alexzielenski): Find a better way to test this since the
	// controller drops any pending events when it shuts down.
	verifyNoMoreEvents()
}

// Shows that an object which fails reconciliation will retry
func TestReconcileRetry(t *testing.T) {
	testContext, testCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer testCancel()

	calls := atomic.Uint64{}
	success := atomic.Bool{}
	tracker, myController, _, waitForReconcile, verifyNoMoreEvents := setupTest(testContext, func(s1, s2 string, o runtime.Object) error {

		if calls.Add(1) > 2 {
			// Suddenly start liking the object
			success.Store(true)
			return nil
		}
		return errors.New("i dont like this object")
	})

	// Start informer
	wg := sync.WaitGroup{}

	wg.Add(1)
	go func() {
		defer wg.Done()
		myController.Informer().Run(testContext.Done())
	}()

	// Start controller
	wg.Add(1)
	go func() {
		defer wg.Done()
		stopReason := myController.Run(testContext)
		require.ErrorIs(t, stopReason, context.Canceled)
	}()

	// Add object to informer
	initialObject := &unstructured.Unstructured{}
	initialObject.SetUnstructuredContent(map[string]interface{}{
		"metadata": map[string]interface{}{
			"name":            "object1",
			"resourceVersion": "1",
		},
	})
	initialObject.SetGroupVersionKind(fakeGVK)
	require.NoError(t, tracker.Add(initialObject))

	require.NoError(t, waitForReconcile(initialObject), "initial reconcile")
	require.NoError(t, waitForReconcile(initialObject), "previous reconcile failed, should retry quickly")
	require.NoError(t, waitForReconcile(initialObject), "previous reconcile failed, should retry quickly")
	// Will not try again since calls > 2 for last reconcile
	require.True(t, success.Load(), "last call to reconcile should return success")
	testCancel()
	wg.Wait()

	verifyNoMoreEvents()
}

func TestInformerList(t *testing.T) {
	testContext, testCancel := context.WithTimeout(context.Background(), 2*time.Second)

	tracker, myController, _, _, _ := setupTest(testContext, nil)

	wg := sync.WaitGroup{}

	wg.Add(1)
	go func() {
		defer wg.Done()
		myController.Informer().Run(testContext.Done())
	}()

	defer func() {
		testCancel()
		wg.Wait()
	}()

	require.True(t, cache.WaitForCacheSync(testContext.Done(), myController.Informer().HasSynced))

	object1 := &unstructured.Unstructured{}
	object1.SetUnstructuredContent(map[string]interface{}{
		"metadata": map[string]interface{}{
			"name":            "object1",
			"resourceVersion": "object1",
		},
	})
	object1.SetGroupVersionKind(fakeGVK)

	object1v2 := &unstructured.Unstructured{}
	object1v2.SetUnstructuredContent(map[string]interface{}{
		"metadata": map[string]interface{}{
			"name":            "object1",
			"resourceVersion": "object1v2",
		},
	})
	object1v2.SetGroupVersionKind(fakeGVK)

	object2 := &unstructured.Unstructured{}
	object2.SetUnstructuredContent(map[string]interface{}{
		"metadata": map[string]interface{}{
			"name":            "object2",
			"resourceVersion": "object2",
		},
	})
	object2.SetGroupVersionKind(fakeGVK)

	object3 := &unstructured.Unstructured{}
	object3.SetUnstructuredContent(map[string]interface{}{
		"metadata": map[string]interface{}{
			"name":            "object3",
			"resourceVersion": "object3",
		},
	})
	object3.SetGroupVersionKind(fakeGVK)

	namespacedObject1 := &unstructured.Unstructured{}
	namespacedObject1.SetUnstructuredContent(map[string]interface{}{
		"metadata": map[string]interface{}{
			"name":            "namespacedObject1",
			"namespace":       "test",
			"resourceVersion": "namespacedObject1",
		},
	})
	namespacedObject1.SetGroupVersionKind(fakeGVK)

	namespacedObject2 := &unstructured.Unstructured{}
	namespacedObject2.SetUnstructuredContent(map[string]interface{}{
		"metadata": map[string]interface{}{
			"name":            "namespacedObject2",
			"namespace":       "test",
			"resourceVersion": "namespacedObject2",
		},
	})
	namespacedObject2.SetGroupVersionKind(fakeGVK)

	require.NoError(t, tracker.Add(object1))
	require.NoError(t, tracker.Add(object2))

	require.NoError(t, wait.PollUntilContextTimeout(testContext, 100*time.Millisecond, 500*time.Millisecond, false, func(ctx context.Context) (done bool, err error) {
		return myController.Informer().LastSyncResourceVersion() == object2.GetResourceVersion(), nil
	}))

	values, err := myController.Informer().List(labels.Everything())
	require.NoError(t, err)
	require.ElementsMatch(t, []*unstructured.Unstructured{object1, object2}, values)

	require.NoError(t, tracker.Update(fakeGVR, object1v2, object1v2.GetNamespace()))
	require.NoError(t, tracker.Delete(fakeGVR, object2.GetNamespace(), object2.GetName()))
	require.NoError(t, tracker.Add(object3))

	require.NoError(t, wait.PollUntilContextTimeout(testContext, 100*time.Millisecond, 500*time.Millisecond, false, func(ctx context.Context) (done bool, err error) {
		return myController.Informer().LastSyncResourceVersion() == object3.GetResourceVersion(), nil
	}))

	values, err = myController.Informer().List(labels.Everything())
	require.NoError(t, err)
	require.ElementsMatch(t, []*unstructured.Unstructured{object1v2, object3}, values)

	require.NoError(t, tracker.Add(namespacedObject1))
	require.NoError(t, tracker.Add(namespacedObject2))

	require.NoError(t, wait.PollUntilContextTimeout(testContext, 100*time.Millisecond, 500*time.Millisecond, false, func(ctx context.Context) (done bool, err error) {
		return myController.Informer().LastSyncResourceVersion() == namespacedObject2.GetResourceVersion(), nil
	}))
	values, err = myController.Informer().Namespaced(namespacedObject1.GetNamespace()).List(labels.Everything())
	require.NoError(t, err)
	require.ElementsMatch(t, []*unstructured.Unstructured{namespacedObject1, namespacedObject2}, values)

	value, err := myController.Informer().Get(object3.GetName())
	require.NoError(t, err)
	require.Equal(t, value, object3)

	value, err = myController.Informer().Namespaced(namespacedObject1.GetNamespace()).Get(namespacedObject1.GetName())
	require.NoError(t, err)
	require.Equal(t, value, namespacedObject1)

	_, err = myController.Informer().Get("fakeobjectname")
	require.True(t, k8serrors.IsNotFound(err))

	_, err = myController.Informer().Namespaced("test").Get("fakeobjectname")
	require.True(t, k8serrors.IsNotFound(err))

	_, err = myController.Informer().Namespaced("fakenamespace").Get("fakeobjectname")
	require.True(t, k8serrors.IsNotFound(err))
}
