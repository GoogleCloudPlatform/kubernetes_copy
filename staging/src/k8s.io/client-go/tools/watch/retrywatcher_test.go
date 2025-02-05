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
	"errors"
	"flag"
	"fmt"
	"reflect"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/dump"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	"k8s.io/klog/v2/ktesting"
)

func init() {
	// Enable klog which is used in dependencies
	klog.InitFlags(nil)
	flag.Set("logtostderr", "true")
	flag.Set("v", "9")
}

type testObject struct {
	resourceVersion string
}

func (o testObject) GetObjectKind() schema.ObjectKind { return schema.EmptyObjectKind }
func (o testObject) DeepCopyObject() runtime.Object   { return o }
func (o testObject) GetResourceVersion() string       { return o.resourceVersion }

func withCounter(w cache.Watcher) (*uint32, cache.WatcherWithContext) {
	var counter uint32
	return &counter, &cache.ListWatch{
		WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
			atomic.AddUint32(&counter, 1)
			return w.Watch(options)
		},
	}
}

func makeTestEvent(rv int) watch.Event {
	return watch.Event{
		Type: watch.Added,
		Object: testObject{
			resourceVersion: fmt.Sprintf("%d", rv),
		},
	}
}

func arrayToChannel(array []watch.Event) chan watch.Event {
	ch := make(chan watch.Event, len(array))

	for _, event := range array {
		ch <- event
	}

	return ch
}

// parseResourceVersionOrDie is test-only that code simulating the server and thus can interpret resourceVersion
func parseResourceVersionOrDie(resourceVersion string) uint64 {
	// We can't use etcdstorage.Versioner.ParseResourceVersion() because of imports restrictions

	if resourceVersion == "" {
		return 0
	}
	version, err := strconv.ParseUint(resourceVersion, 10, 64)
	if err != nil {
		panic(fmt.Errorf("failed to parse resourceVersion %q", resourceVersion))
	}
	return version
}

func fromRV(resourceVersion string, array []watch.Event) []watch.Event {
	var result []watch.Event
	rv := parseResourceVersionOrDie(resourceVersion)
	for _, event := range array {
		if event.Type == watch.Error {
			if len(result) == 0 {
				// Skip error events until we find an object matching RV requirement
				continue
			}
		} else {
			rvGetter, ok := event.Object.(resourceVersionGetter)
			if ok {
				if parseResourceVersionOrDie(rvGetter.GetResourceVersion()) <= rv {
					continue
				}
			}
		}

		result = append(result, event)
	}

	return result
}

func closeAfterN(n int, source chan watch.Event) chan watch.Event {
	result := make(chan watch.Event, 0)
	go func() {
		defer close(result)
		defer close(source)
		for i := 0; i < n; i++ {
			result <- <-source
		}
	}()
	return result
}

type unexpectedError struct {
	// Inheriting any struct fulfilling runtime.Object interface would do.
	metav1.Status
}

var _ runtime.Object = &unexpectedError{}

func TestNewRetryWatcher(t *testing.T) {
	tt := []struct {
		name      string
		initialRV string
		err       error
	}{
		{
			name:      "empty RV should fail",
			initialRV: "",
			err:       errors.New("initial RV \"\" is not supported due to issues with underlying WATCH"),
		},
		{
			name:      "RV \"0\" should fail",
			initialRV: "0",
			err:       errors.New("initial RV \"0\" is not supported due to issues with underlying WATCH"),
		},
	}
	for _, tc := range tt {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewRetryWatcher(tc.initialRV, nil)
			if !reflect.DeepEqual(err, tc.err) {
				t.Errorf("Expected error: %v, got: %v", tc.err, err)
			}
		})
	}
}

func TestRetryWatcher(t *testing.T) {
	tt := []struct {
		name        string
		initialRV   string
		watchClient cache.Watcher
		watchCount  uint32
		expected    []watch.Event
	}{
		{
			name:      "recovers if watchClient returns error",
			initialRV: "1",
			watchClient: &cache.ListWatch{
				WatchFunc: func() func(options metav1.ListOptions) (watch.Interface, error) {
					firstRun := true
					return func(options metav1.ListOptions) (watch.Interface, error) {
						if firstRun {
							firstRun = false
							return nil, fmt.Errorf("test error")
						}

						return watch.NewProxyWatcher(arrayToChannel(fromRV(options.ResourceVersion, []watch.Event{
							makeTestEvent(2),
						}))), nil
					}
				}(),
			},
			watchCount: 2,
			expected: []watch.Event{
				makeTestEvent(2),
			},
		},
		{
			name:      "recovers if watchClient returns nil watcher",
			initialRV: "1",
			watchClient: &cache.ListWatch{
				WatchFunc: func() func(options metav1.ListOptions) (watch.Interface, error) {
					firstRun := true
					return func(options metav1.ListOptions) (watch.Interface, error) {
						if firstRun {
							firstRun = false
							return nil, nil
						}

						return watch.NewProxyWatcher(arrayToChannel(fromRV(options.ResourceVersion, []watch.Event{
							makeTestEvent(2),
						}))), nil
					}
				}(),
			},
			watchCount: 2,
			expected: []watch.Event{
				makeTestEvent(2),
			},
		},
		{
			name:      "works with empty initialRV",
			initialRV: "1",
			watchClient: &cache.ListWatch{
				WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
					return watch.NewProxyWatcher(arrayToChannel(fromRV(options.ResourceVersion, []watch.Event{
						makeTestEvent(2),
					}))), nil
				},
			},
			watchCount: 1,
			expected: []watch.Event{
				makeTestEvent(2),
			},
		},
		{
			name:      "works with initialRV set, skipping the preceding items but reading those directly following",
			initialRV: "1",
			watchClient: &cache.ListWatch{
				WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
					return watch.NewProxyWatcher(arrayToChannel(fromRV(options.ResourceVersion, []watch.Event{
						makeTestEvent(1),
						makeTestEvent(2),
					}))), nil
				},
			},
			watchCount: 1,
			expected: []watch.Event{
				makeTestEvent(2),
			},
		},
		{
			name:      "works with initialRV set, skipping the preceding items with none following",
			initialRV: "3",
			watchClient: &cache.ListWatch{
				WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
					return watch.NewProxyWatcher(arrayToChannel(fromRV(options.ResourceVersion, []watch.Event{
						makeTestEvent(2),
					}))), nil
				},
			},
			watchCount: 1,
			expected:   nil,
		},
		{
			name:      "fails on Gone (RV too old error)",
			initialRV: "5",
			watchClient: &cache.ListWatch{
				WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
					return watch.NewProxyWatcher(arrayToChannel(fromRV(options.ResourceVersion, []watch.Event{
						makeTestEvent(5),
						makeTestEvent(6),
						{Type: watch.Error, Object: &apierrors.NewGone("").ErrStatus},
						makeTestEvent(7),
						makeTestEvent(8),
					}))), nil
				},
			},
			watchCount: 1,
			expected: []watch.Event{
				makeTestEvent(6),
				{
					Type:   watch.Error,
					Object: &apierrors.NewGone("").ErrStatus,
				},
			},
		},
		{
			name:      "fails on Forbidden",
			initialRV: "5",
			watchClient: &cache.ListWatch{
				WatchFunc: func() func(options metav1.ListOptions) (watch.Interface, error) {
					return func(options metav1.ListOptions) (watch.Interface, error) {
						return nil, apierrors.NewForbidden(schema.GroupResource{}, "", errors.New("unknown"))
					}
				}(),
			},
			watchCount: 1,
			expected: []watch.Event{
				{
					Type:   watch.Error,
					Object: &apierrors.NewForbidden(schema.GroupResource{}, "", errors.New("unknown")).ErrStatus,
				},
			},
		},
		{
			name:      "fails on Unauthorized",
			initialRV: "5",
			watchClient: &cache.ListWatch{
				WatchFunc: func() func(options metav1.ListOptions) (watch.Interface, error) {
					return func(options metav1.ListOptions) (watch.Interface, error) {
						return nil, apierrors.NewUnauthorized("")
					}
				}(),
			},
			watchCount: 1,
			expected: []watch.Event{
				{
					Type:   watch.Error,
					Object: &apierrors.NewUnauthorized("").ErrStatus,
				},
			},
		},
		{
			name:      "recovers from timeout error",
			initialRV: "5",
			watchClient: &cache.ListWatch{
				WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
					return watch.NewProxyWatcher(arrayToChannel(fromRV(options.ResourceVersion, []watch.Event{
						makeTestEvent(6),
						{
							Type:   watch.Error,
							Object: &apierrors.NewTimeoutError("", 0).ErrStatus,
						},
						makeTestEvent(7),
					}))), nil
				},
			},
			watchCount: 2,
			expected: []watch.Event{
				makeTestEvent(6),
				makeTestEvent(7),
			},
		},
		{
			name:      "recovers from internal server error",
			initialRV: "5",
			watchClient: &cache.ListWatch{
				WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
					return watch.NewProxyWatcher(arrayToChannel(fromRV(options.ResourceVersion, []watch.Event{
						makeTestEvent(6),
						{
							Type:   watch.Error,
							Object: &apierrors.NewInternalError(errors.New("")).ErrStatus,
						},
						makeTestEvent(7),
					}))), nil
				},
			},
			watchCount: 2,
			expected: []watch.Event{
				makeTestEvent(6),
				makeTestEvent(7),
			},
		},
		{
			name:      "recovers from unexpected error code",
			initialRV: "5",
			watchClient: &cache.ListWatch{
				WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
					return watch.NewProxyWatcher(arrayToChannel(fromRV(options.ResourceVersion, []watch.Event{
						makeTestEvent(6),
						{
							Type: watch.Error,
							Object: &metav1.Status{
								Code: 666,
							},
						},
						makeTestEvent(7),
					}))), nil
				},
			},
			watchCount: 2,
			expected: []watch.Event{
				makeTestEvent(6),
				makeTestEvent(7),
			},
		},
		{
			name:      "recovers from unexpected error type",
			initialRV: "5",
			watchClient: &cache.ListWatch{
				WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
					return watch.NewProxyWatcher(arrayToChannel(fromRV(options.ResourceVersion, []watch.Event{
						makeTestEvent(6),
						{
							Type:   watch.Error,
							Object: &unexpectedError{},
						},
						makeTestEvent(7),
					}))), nil
				},
			},
			watchCount: 2,
			expected: []watch.Event{
				makeTestEvent(6),
				makeTestEvent(7),
			},
		},
		{
			name:      "survives 1 closed watch and reads 1 item",
			initialRV: "5",
			watchClient: &cache.ListWatch{
				WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
					return watch.NewProxyWatcher(closeAfterN(1, arrayToChannel(fromRV(options.ResourceVersion, []watch.Event{
						makeTestEvent(6),
					})))), nil
				},
			},
			watchCount: 2,
			expected: []watch.Event{
				makeTestEvent(6),
			},
		},
		{
			name:      "survives 2 closed watches and reads 2 items",
			initialRV: "4",
			watchClient: &cache.ListWatch{
				WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
					return watch.NewProxyWatcher(closeAfterN(1, arrayToChannel(fromRV(options.ResourceVersion, []watch.Event{
						makeTestEvent(5),
						makeTestEvent(6),
					})))), nil
				},
			},
			watchCount: 3,
			expected: []watch.Event{
				makeTestEvent(5),
				makeTestEvent(6),
			},
		},
		{
			name:      "survives 2 closed watches and reads 2 items for nonconsecutive RVs",
			initialRV: "4",
			watchClient: &cache.ListWatch{
				WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
					return watch.NewProxyWatcher(closeAfterN(1, arrayToChannel(fromRV(options.ResourceVersion, []watch.Event{
						makeTestEvent(5),
						makeTestEvent(7),
					})))), nil
				},
			},
			watchCount: 3,
			expected: []watch.Event{
				makeTestEvent(5),
				makeTestEvent(7),
			},
		},
		{
			name:      "survives 2 closed watches and reads 2 items for nonconsecutive RVs starting at much lower RV",
			initialRV: "2",
			watchClient: &cache.ListWatch{
				WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
					return watch.NewProxyWatcher(closeAfterN(1, arrayToChannel(fromRV(options.ResourceVersion, []watch.Event{
						makeTestEvent(5),
						makeTestEvent(7),
					})))), nil
				},
			},
			watchCount: 3,
			expected: []watch.Event{
				makeTestEvent(5),
				makeTestEvent(7),
			},
		},
		{
			name:      "survives 4 closed watches and reads 4 items for nonconsecutive, spread RVs",
			initialRV: "2",
			watchClient: &cache.ListWatch{
				WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
					return watch.NewProxyWatcher(closeAfterN(1, arrayToChannel(fromRV(options.ResourceVersion, []watch.Event{
						makeTestEvent(5),
						makeTestEvent(6),
						makeTestEvent(7),
						makeTestEvent(11),
					})))), nil
				},
			},
			watchCount: 5,
			expected: []watch.Event{
				makeTestEvent(5),
				makeTestEvent(6),
				makeTestEvent(7),
				makeTestEvent(11),
			},
		},
		{
			name:      "survives 4 closed watches and reads 4 items for nonconsecutive, spread RVs and skips those with lower or equal RV",
			initialRV: "2",
			watchClient: &cache.ListWatch{
				WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
					return watch.NewProxyWatcher(closeAfterN(1, arrayToChannel(fromRV(options.ResourceVersion, []watch.Event{
						makeTestEvent(1),
						makeTestEvent(2),
						makeTestEvent(5),
						makeTestEvent(6),
						makeTestEvent(7),
						makeTestEvent(11),
					})))), nil
				},
			},
			watchCount: 5,
			expected: []watch.Event{
				makeTestEvent(5),
				makeTestEvent(6),
				makeTestEvent(7),
				makeTestEvent(11),
			},
		},
		{
			name:      "survives 2 closed watches and reads 2+2+1 items skipping those with equal RV",
			initialRV: "1",
			watchClient: &cache.ListWatch{
				WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
					return watch.NewProxyWatcher(closeAfterN(2, arrayToChannel(fromRV(options.ResourceVersion, []watch.Event{
						makeTestEvent(1),
						makeTestEvent(2),
						makeTestEvent(5),
						makeTestEvent(6),
						makeTestEvent(7),
						makeTestEvent(11),
					})))), nil
				},
			},
			watchCount: 3,
			expected: []watch.Event{
				makeTestEvent(2),
				makeTestEvent(5),
				makeTestEvent(6),
				makeTestEvent(7),
				makeTestEvent(11),
			},
		},
	}

	for _, tc := range tt {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			_, ctx := ktesting.NewTestContext(t)
			t.Parallel()

			atomicCounter, watchFunc := withCounter(tc.watchClient)
			watcher, err := newRetryWatcher(ctx, tc.initialRV, watchFunc, time.Duration(0))
			if err != nil {
				t.Fatalf("failed to create a RetryWatcher: %v", err)
			}
			defer func() {
				watcher.Stop()
				t.Log("Waiting on RetryWatcher to stop...")
				<-watcher.Done()
			}()

			var got []watch.Event
			for i := 0; i < len(tc.expected); i++ {
				event, ok := <-watcher.ResultChan()
				if !ok {
					t.Errorf("expected event %s, but channel is closed", dump.Pretty(tc.expected[i]))
					break
				}

				got = append(got, event)
			}

			// (Sanity check, best effort) Make sure there are no more events to be received
			// RetryWatcher proxies the source channel so we can't try reading it immediately
			// but have to tolerate some delay. Given this is best effort detection we can use short duration.
			// It also makes sure that for 0 events the watchFunc has time to be called.
			select {
			case event, ok := <-watcher.ResultChan():
				if ok {
					t.Errorf("Unexpected event received after reading all the expected ones: %s", dump.Pretty(event))
				}
			case <-time.After(10 * time.Millisecond):
				break
			}

			var counter uint32
			// We always count with the last watch reestablishing which is imminent but still a race.
			// We will wait for the last watch to reestablish to avoid it.
			err = wait.PollImmediate(10*time.Millisecond, 10*time.Second, func() (done bool, err error) {
				counter = atomic.LoadUint32(atomicCounter)
				return counter == tc.watchCount, nil
			})
			if err == wait.ErrWaitTimeout {
				t.Errorf("expected %d watcher starts, but it has started %d times", tc.watchCount, counter)
			} else if err != nil {
				t.Fatal(err)
			}

			if !reflect.DeepEqual(tc.expected, got) {
				t.Fatalf("expected %s, got %s;\ndiff: %s", dump.Pretty(tc.expected), dump.Pretty(got), cmp.Diff(tc.expected, got))
			}
		})
	}
}

func TestRetryWatcherToFinishWithUnreadEvents(t *testing.T) {
	watcher, err := NewRetryWatcher("1", &cache.ListWatch{
		WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
			return watch.NewProxyWatcher(arrayToChannel([]watch.Event{
				makeTestEvent(2),
			})), nil
		},
	})
	if err != nil {
		t.Fatalf("failed to create a RetryWatcher: %v", err)
	}

	// Give the watcher a chance to get to sending events (blocking)
	time.Sleep(10 * time.Millisecond)

	watcher.Stop()
	// Verify a second stop does not cause a panic
	watcher.Stop()

	maxTime := time.Second
	select {
	case <-watcher.Done():
		break
	case <-time.After(maxTime):
		t.Errorf("The watcher failed to be closed in %s", maxTime)
	}

	// RetryWatcher result channel should be closed
	_, ok := <-watcher.ResultChan()
	if ok {
		t.Error("ResultChan is not closed")
	}
}
