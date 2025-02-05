/*
Copyright 2017 The Kubernetes Authors.

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

package watch

import (
	"context"
	"reflect"
	goruntime "runtime"
	"sort"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/dump"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	clientfeatures "k8s.io/client-go/features"
	clientfeaturestesting "k8s.io/client-go/features/testing"
	fakeclientset "k8s.io/client-go/kubernetes/fake"
	testcore "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
)

// TestEventProcessorExit is expected to timeout if the event processor fails
// to exit when stopped.
func TestEventProcessorExit(t *testing.T) {
	event := watch.Event{}

	tests := []struct {
		name  string
		write func(e *eventProcessor)
	}{
		{
			name: "exit on blocked read",
			write: func(e *eventProcessor) {
				e.push(event)
			},
		},
		{
			name: "exit on blocked write",
			write: func(e *eventProcessor) {
				e.push(event)
				e.push(event)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			out := make(chan watch.Event)
			e := newEventProcessor(out)

			test.write(e)

			exited := make(chan struct{})
			go func() {
				e.run()
				close(exited)
			}()

			<-out
			e.stop()
			goruntime.Gosched()
			<-exited
		})
	}
}

type apiInt int

func (apiInt) GetObjectKind() schema.ObjectKind { return nil }
func (apiInt) DeepCopyObject() runtime.Object   { return nil }

func TestEventProcessorOrdersEvents(t *testing.T) {
	out := make(chan watch.Event)
	e := newEventProcessor(out)
	go e.run()

	numProcessed := 0
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	go func() {
		for i := 0; i < 1000; i++ {
			e := <-out
			if got, want := int(e.Object.(apiInt)), i; got != want {
				t.Errorf("unexpected event: got=%d, want=%d", got, want)
			}
			numProcessed++
		}
		cancel()
	}()

	for i := 0; i < 1000; i++ {
		e.push(watch.Event{Object: apiInt(i)})
	}

	<-ctx.Done()
	e.stop()

	if numProcessed != 1000 {
		t.Errorf("unexpected number of events processed: %d", numProcessed)
	}

}

type byEventTypeAndName []watch.Event

func (a byEventTypeAndName) Len() int      { return len(a) }
func (a byEventTypeAndName) Swap(i, j int) { a[i], a[j] = a[j], a[i] }
func (a byEventTypeAndName) Less(i, j int) bool {
	if a[i].Type < a[j].Type {
		return true
	}

	if a[i].Type > a[j].Type {
		return false
	}

	return a[i].Object.(*corev1.Secret).Name < a[j].Object.(*corev1.Secret).Name
}

func newTestSecret(name, key, value string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		StringData: map[string]string{
			key: value,
		},
	}
}

func TestNewInformerWatcher(t *testing.T) {
	// Make sure there are no 2 same types of events on a secret with the same name or that might be flaky.
	tt := []struct {
		name                    string
		watchListFeatureEnabled bool
		objects                 []runtime.Object
		inputEvents             []watch.Event
		outputEvents            []watch.Event
	}{
		{
			name:                    "WatchListClient feature disabled",
			watchListFeatureEnabled: false,
			objects: []runtime.Object{
				newTestSecret("pod-1", "foo-1", "initial"),
				newTestSecret("pod-2", "foo-2", "initial"),
				newTestSecret("pod-3", "foo-3", "initial"),
			},
			inputEvents: []watch.Event{
				{
					Type:   watch.Added,
					Object: newTestSecret("pod-4", "foo-4", "initial"),
				},
				{
					Type:   watch.Modified,
					Object: newTestSecret("pod-2", "foo-2", "new"),
				},
				{
					Type:   watch.Deleted,
					Object: newTestSecret("pod-3", "foo-3", "initial"),
				},
			},
			outputEvents: []watch.Event{
				// When WatchListClient is disabled, ListAndWatch creates fake
				// ADDED events for each object listed.
				{
					Type:   watch.Added,
					Object: newTestSecret("pod-1", "foo-1", "initial"),
				},
				{
					Type:   watch.Added,
					Object: newTestSecret("pod-2", "foo-2", "initial"),
				},
				{
					Type:   watch.Added,
					Object: newTestSecret("pod-3", "foo-3", "initial"),
				},
				// Normal events follow.
				{
					Type:   watch.Added,
					Object: newTestSecret("pod-4", "foo-4", "initial"),
				},
				{
					Type:   watch.Modified,
					Object: newTestSecret("pod-2", "foo-2", "new"),
				},
				{
					Type:   watch.Deleted,
					Object: newTestSecret("pod-3", "foo-3", "initial"),
				},
			},
		},
		{
			name:                    "WatchListClient feature enabled",
			watchListFeatureEnabled: true,
			objects: []runtime.Object{
				newTestSecret("pod-1", "foo-1", "initial"),
				newTestSecret("pod-2", "foo-2", "initial"),
				newTestSecret("pod-3", "foo-3", "initial"),
			},
			inputEvents: []watch.Event{
				{
					Type:   watch.Added,
					Object: newTestSecret("pod-1", "foo-1", "initial"),
				},
				{
					Type:   watch.Added,
					Object: newTestSecret("pod-2", "foo-2", "initial"),
				},
				{
					Type:   watch.Added,
					Object: newTestSecret("pod-3", "foo-3", "initial"),
				},
				// ListWatch bookmark indicates that initial listing is done
				{
					Type: watch.Bookmark,
					Object: &corev1.Secret{
						ObjectMeta: metav1.ObjectMeta{
							Annotations: map[string]string{
								metav1.InitialEventsAnnotationKey: "true",
							},
						},
					},
				},
				{
					Type:   watch.Added,
					Object: newTestSecret("pod-4", "foo-4", "initial"),
				},
				{
					Type:   watch.Modified,
					Object: newTestSecret("pod-2", "foo-2", "new"),
				},
				{
					Type:   watch.Deleted,
					Object: newTestSecret("pod-3", "foo-3", "initial"),
				},
			},
			outputEvents: []watch.Event{
				// When WatchListClient is enabled, WatchList receives
				// ADDED events from the server for each existing object.
				{
					Type:   watch.Added,
					Object: newTestSecret("pod-1", "foo-1", "initial"),
				},
				{
					Type:   watch.Added,
					Object: newTestSecret("pod-2", "foo-2", "initial"),
				},
				{
					Type:   watch.Added,
					Object: newTestSecret("pod-3", "foo-3", "initial"),
				},
				// Bookmark event at the end of listing is not sent to the client.
				// Normal events follow.
				{
					Type:   watch.Added,
					Object: newTestSecret("pod-4", "foo-4", "initial"),
				},
				{
					Type:   watch.Modified,
					Object: newTestSecret("pod-2", "foo-2", "new"),
				},
				{
					Type:   watch.Deleted,
					Object: newTestSecret("pod-3", "foo-3", "initial"),
				},
			},
		},
	}

	for _, tc := range tt {
		t.Run(tc.name, func(t *testing.T) {
			clientfeaturestesting.SetFeatureDuringTest(t, clientfeatures.WatchListClient, tc.watchListFeatureEnabled)

			fake := fakeclientset.NewSimpleClientset(tc.objects...)
			inputCh := make(chan watch.Event)
			inputWatcher := watch.NewProxyWatcher(inputCh)
			// Indexer should stop the input watcher when the output watcher is stopped.
			// But stop it at the end of the test, just in case.
			defer inputWatcher.Stop()
			inputStopCh := inputWatcher.StopChan()
			fake.PrependWatchReactor("secrets", testcore.DefaultWatchReactor(inputWatcher, nil))
			// Send events and then close the done channel
			inputDoneCh := make(chan struct{})
			go func() {
				defer close(inputDoneCh)
				for _, e := range tc.inputEvents {
					select {
					case inputCh <- e:
					case <-inputStopCh:
						return
					}
				}
			}()

			lw := &cache.ListWatch{
				ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
					return fake.CoreV1().Secrets("").List(context.TODO(), options)
				},
				WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
					return fake.CoreV1().Secrets("").Watch(context.TODO(), options)
				},
			}
			//nolint:logcheck // Intentionally uses the older API.
			_, _, outputWatcher, informerDoneCh := NewIndexerInformerWatcher(lw, &corev1.Secret{})
			outputCh := outputWatcher.ResultChan()
			timeoutCh := time.After(wait.ForeverTestTimeout)
			var result []watch.Event
		loop:
			for {
				select {
				case event, ok := <-outputCh:
					if !ok {
						t.Errorf("Output result channel closed prematurely")
						break loop
					}
					result = append(result, *event.DeepCopy())
					if len(result) >= len(tc.outputEvents) {
						break loop
					}
				case <-timeoutCh:
					t.Error("Timed out waiting for events")
					break loop
				}
			}

			// Informers don't guarantee event order so we need to sort these arrays to compare them.
			sort.Sort(byEventTypeAndName(tc.outputEvents))
			sort.Sort(byEventTypeAndName(result))

			if !reflect.DeepEqual(tc.outputEvents, result) {
				t.Errorf("\nexpected: %s,\ngot:      %s,\ndiff: %s", dump.Pretty(tc.outputEvents), dump.Pretty(result), cmp.Diff(tc.outputEvents, result))
				return
			}

			// Send some more events, but don't read them.
			// Stop producing events when the consumer stops the watcher.
			go func() {
				defer close(inputCh)
				for _, e := range tc.inputEvents {
					select {
					case inputCh <- e:
					case <-inputStopCh:
						return
					}
				}
			}()

			// Stop before reading all the data to make sure the informer can deal with closed channel
			outputWatcher.Stop()

			select {
			case <-informerDoneCh:
			case <-time.After(wait.ForeverTestTimeout):
				t.Error("Timed out waiting for informer to cleanup")
			}
		})
	}

}

// TestInformerWatcherDeletedFinalStateUnknown tests the code path when `DeleteFunc`
// in `NewIndexerInformerWatcher` receives a `cache.DeletedFinalStateUnknown`
// object from the underlying `DeltaFIFO`. The triggering condition is described
// at https://github.com/kubernetes/kubernetes/blob/dc39ab2417bfddcec37be4011131c59921fdbe98/staging/src/k8s.io/client-go/tools/cache/delta_fifo.go#L736-L739.
//
// Code from @liggitt
func TestInformerWatcherDeletedFinalStateUnknown(t *testing.T) {
	listCalls := 0
	watchCalls := 0
	lw := &cache.ListWatch{
		ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
			retval := &corev1.SecretList{}
			if listCalls == 0 {
				// Return a list with items in it
				retval.ResourceVersion = "1"
				retval.Items = []corev1.Secret{{ObjectMeta: metav1.ObjectMeta{Name: "secret1", Namespace: "ns1", ResourceVersion: "123"}}}
			} else {
				// Return empty lists after the first call
				retval.ResourceVersion = "2"
			}
			listCalls++
			return retval, nil
		},
		WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
			w := watch.NewRaceFreeFake()
			if options.ResourceVersion == "1" {
				go func() {
					// Close with a "Gone" error when trying to start a watch from the first list
					w.Error(&apierrors.NewGone("gone").ErrStatus)
					w.Stop()
				}()
			}
			watchCalls++
			return w, nil
		},
	}
	//nolint:logcheck // Intentionally uses the older API.
	_, _, w, done := NewIndexerInformerWatcher(lw, &corev1.Secret{})
	defer w.Stop()

	// Expect secret add
	select {
	case event, ok := <-w.ResultChan():
		if !ok {
			t.Fatal("unexpected close")
		}
		if event.Type != watch.Added {
			t.Fatalf("expected Added event, got %#v", event)
		}
		if event.Object.(*corev1.Secret).ResourceVersion != "123" {
			t.Fatalf("expected added Secret with rv=123, got %#v", event.Object)
		}
	case <-time.After(time.Second * 10):
		t.Fatal("timeout")
	}

	// Expect secret delete because the relist was missing the secret
	select {
	case event, ok := <-w.ResultChan():
		if !ok {
			t.Fatal("unexpected close")
		}
		if event.Type != watch.Deleted {
			t.Fatalf("expected Deleted event, got %#v", event)
		}
		if event.Object.(*corev1.Secret).ResourceVersion != "123" {
			t.Fatalf("expected deleted Secret with rv=123, got %#v", event.Object)
		}
	case <-time.After(time.Second * 10):
		t.Fatal("timeout")
	}

	w.Stop()
	select {
	case <-done:
	case <-time.After(time.Second * 10):
		t.Fatal("timeout")
	}

	if listCalls < 2 {
		t.Fatalf("expected at least 2 list calls, got %d", listCalls)
	}
	if watchCalls < 1 {
		t.Fatalf("expected at least 1 watch call, got %d", watchCalls)
	}
}
