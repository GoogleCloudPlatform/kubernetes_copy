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

package drain

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"reflect"
	"sort"
	"strconv"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	policyv1beta1 "k8s.io/api/policy/v1beta1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes/fake"
	ktest "k8s.io/client-go/testing"
)

func TestDeletePods(t *testing.T) {
	ifHasBeenCalled := map[string]bool{}
	tests := []struct {
		description       string
		interval          time.Duration
		timeout           time.Duration
		ctxTimeoutEarly   bool
		expectPendingPods bool
		expectError       bool
		expectedError     error
		getPodFn          func(namespace, name string) (*corev1.Pod, error)
	}{
		{
			description:       "Wait for deleting to complete",
			interval:          100 * time.Millisecond,
			timeout:           10 * time.Second,
			expectPendingPods: false,
			expectError:       false,
			expectedError:     nil,
			getPodFn: func(namespace, name string) (*corev1.Pod, error) {
				oldPodMap, _ := createPods(false)
				newPodMap, _ := createPods(true)
				if oldPod, found := oldPodMap[name]; found {
					if _, ok := ifHasBeenCalled[name]; !ok {
						ifHasBeenCalled[name] = true
						return &oldPod, nil
					}
					if oldPod.ObjectMeta.Generation < 4 {
						newPod := newPodMap[name]
						return &newPod, nil
					}
					return &corev1.Pod{}, apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, name)
				}
				return &corev1.Pod{}, apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, name)
			},
		},
		{
			description:       "Pod found with same name but different UID",
			interval:          100 * time.Millisecond,
			timeout:           10 * time.Second,
			expectPendingPods: false,
			expectError:       false,
			expectedError:     nil,
			getPodFn: func(namespace, name string) (*corev1.Pod, error) {
				// Return a pod with the same name, but different UID
				return &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: namespace,
						Name:      name,
						UID:       "SOME_OTHER_UID",
					},
				}, nil
			},
		},
		{
			description:       "Deleting could timeout",
			interval:          200 * time.Millisecond,
			timeout:           3 * time.Second,
			expectPendingPods: true,
			expectError:       true,
			expectedError:     wait.ErrorInterrupted(context.DeadlineExceeded),
			getPodFn: func(namespace, name string) (*corev1.Pod, error) {
				oldPodMap, _ := createPods(false)
				if oldPod, found := oldPodMap[name]; found {
					return &oldPod, nil
				}
				return &corev1.Pod{}, fmt.Errorf("%q: not found", name)
			},
		},
		{
			description:       "Context Canceled",
			interval:          1000 * time.Millisecond,
			timeout:           5 * time.Second,
			ctxTimeoutEarly:   true,
			expectPendingPods: true,
			expectError:       true,
			expectedError:     context.Canceled,
			getPodFn: func(namespace, name string) (*corev1.Pod, error) {
				oldPodMap, _ := createPods(false)
				if oldPod, found := oldPodMap[name]; found {
					return &oldPod, nil
				}
				return &corev1.Pod{}, fmt.Errorf("%q: not found", name)
			},
		},
		{
			description:       "Skip Deleted Pod",
			interval:          200 * time.Millisecond,
			timeout:           3 * time.Second,
			expectPendingPods: false,
			expectError:       false,
			expectedError:     nil,
			getPodFn: func(namespace, name string) (*corev1.Pod, error) {
				oldPodMap, _ := createPods(false)
				if oldPod, found := oldPodMap[name]; found {
					dTime := &metav1.Time{Time: time.Now().Add(time.Duration(100) * time.Second * -1)}
					oldPod.ObjectMeta.SetDeletionTimestamp(dTime)
					return &oldPod, nil
				}
				return &corev1.Pod{}, fmt.Errorf("%q: not found", name)
			},
		},
		{
			description:       "Client error could be passed out",
			interval:          200 * time.Millisecond,
			timeout:           5 * time.Second,
			expectPendingPods: true,
			expectError:       true,
			expectedError:     wait.ErrorInterrupted(errors.New("This is a random error for testing")),
			getPodFn: func(namespace, name string) (*corev1.Pod, error) {
				return &corev1.Pod{}, errors.New("This is a random error for testing")
			},
		},
	}

	for _, test := range tests {
		t.Run(test.description, func(t *testing.T) {
			_, pods := createPods(false)
			var ctx context.Context
			var cancel context.CancelFunc
			ctx = context.Background()
			if test.ctxTimeoutEarly {
				ctx, cancel = context.WithTimeout(ctx, 100*time.Millisecond)
				defer cancel()
			}
			params := waitForDeleteParams{
				ctx:                             ctx,
				pods:                            pods,
				interval:                        test.interval,
				timeout:                         test.timeout,
				usingEviction:                   false,
				getPodFn:                        test.getPodFn,
				onDoneFn:                        nil,
				globalTimeout:                   time.Duration(math.MaxInt64),
				out:                             os.Stdout,
				skipWaitForDeleteTimeoutSeconds: 10,
			}
			pendingPods, err := waitForDelete(params)

			if test.expectError {
				if err == nil {
					if !errors.Is(err, wait.ErrorInterrupted(test.expectedError)) {
						t.Fatalf("%s: expected error %v, got %v", test.description, test.expectedError, err)

					}
				}
			}
			if !test.expectError && err != nil {
				t.Fatalf("%s: unexpected error", test.description)
			}
			if test.expectPendingPods && len(pendingPods) == 0 {
				t.Fatalf("%s: unexpected empty pods", test.description)
			}
			if !test.expectPendingPods && len(pendingPods) > 0 {
				t.Fatalf("%s: unexpected pending pods", test.description)
			}
		})
	}
}

func createPods(ifCreateNewPods bool) (map[string]corev1.Pod, []corev1.Pod) {
	podMap := make(map[string]corev1.Pod)
	podSlice := []corev1.Pod{}
	for i := 0; i < 8; i++ {
		var uid types.UID
		if ifCreateNewPods {
			uid = types.UID(strconv.Itoa(i))
		} else {
			uid = types.UID(strconv.Itoa(i) + strconv.Itoa(i))
		}
		pod := corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:       "pod" + strconv.Itoa(i),
				Namespace:  "default",
				UID:        uid,
				Generation: int64(i),
			},
		}
		podMap[pod.Name] = pod
		podSlice = append(podSlice, pod)
	}
	return podMap, podSlice
}

func addCoreNonEvictionSupport(t *testing.T, k *fake.Clientset) {
	coreResources := &metav1.APIResourceList{
		GroupVersion: "v1",
	}
	k.Resources = append(k.Resources, coreResources)
}

// addEvictionSupport implements simple fake eviction support on the fake.Clientset
func addEvictionSupport(t *testing.T, k *fake.Clientset, version string) {
	podsEviction := metav1.APIResource{
		Name:    "pods/eviction",
		Kind:    "Eviction",
		Group:   "policy",
		Version: version,
	}
	coreResources := &metav1.APIResourceList{
		GroupVersion: "v1",
		APIResources: []metav1.APIResource{podsEviction},
	}

	policyResources := &metav1.APIResourceList{
		GroupVersion: "policy/v1",
	}
	k.Resources = append(k.Resources, coreResources, policyResources)

	// Delete pods when evict is called
	k.PrependReactor("create", "pods", func(action ktest.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != "eviction" {
			return false, nil, nil
		}

		namespace := ""
		name := ""
		switch version {
		case "v1":
			eviction := *action.(ktest.CreateAction).GetObject().(*policyv1.Eviction)
			namespace = eviction.Namespace
			name = eviction.Name
		case "v1beta1":
			eviction := *action.(ktest.CreateAction).GetObject().(*policyv1beta1.Eviction)
			namespace = eviction.Namespace
			name = eviction.Name
		default:
			t.Errorf("unknown version %s", version)
		}
		// Avoid the lock
		go func() {
			err := k.CoreV1().Pods(namespace).Delete(context.TODO(), name, metav1.DeleteOptions{})
			if err != nil {
				// Errorf because we can't call Fatalf from another goroutine
				t.Errorf("failed to delete pod: %s/%s", namespace, name)
			}
		}()

		return true, nil, nil
	})
}

func TestCheckEvictionSupport(t *testing.T) {
	for _, evictionVersion := range []string{"", "v1", "v1beta1"} {
		t.Run(fmt.Sprintf("evictionVersion=%v", evictionVersion),
			func(t *testing.T) {
				k := fake.NewSimpleClientset()
				if len(evictionVersion) > 0 {
					addEvictionSupport(t, k, evictionVersion)
				} else {
					addCoreNonEvictionSupport(t, k)
				}

				apiGroup, err := CheckEvictionSupport(k)
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				expectedAPIGroup := schema.GroupVersion{}
				if len(evictionVersion) > 0 {
					expectedAPIGroup = schema.GroupVersion{Group: "policy", Version: evictionVersion}
				}
				if apiGroup != expectedAPIGroup {
					t.Fatalf("expected apigroup %q, actual=%q", expectedAPIGroup, apiGroup)
				}
			})
	}
}

func TestDeleteOrEvict(t *testing.T) {
	tests := []struct {
		description       string
		evictionSupported bool
		disableEviction   bool
	}{
		{
			description:       "eviction supported/enabled",
			evictionSupported: true,
			disableEviction:   false,
		},
		{
			description:       "eviction unsupported/disabled",
			evictionSupported: false,
			disableEviction:   false,
		},
		{
			description:       "eviction supported/disabled",
			evictionSupported: true,
			disableEviction:   true,
		},
		{
			description:       "eviction unsupported/disabled",
			evictionSupported: false,
			disableEviction:   false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.description, func(t *testing.T) {
			h := &Helper{
				Out:                os.Stdout,
				GracePeriodSeconds: 10,
				OnPodDeletionOrEvictionStarted: func(pod *corev1.Pod, usingEviction bool) {
					if tc.evictionSupported && !tc.disableEviction {
						if !usingEviction {
							t.Errorf("%s: OnPodDeletionOrEvictionStarted callback failed while evicting; actual\n\t%v\nexpected\n\t%v", tc.description, usingEviction, !usingEviction)
						}
					} else if tc.evictionSupported && tc.disableEviction {
						if usingEviction {
							t.Errorf("%s: OnPodDeletionOrEvictionStarted callback failed while deleting; actual\n\t%v\nexpected\n\t%v", tc.description, !usingEviction, usingEviction)
						}
					}
				},
				OnPodDeletedOrEvicted: func(pod *corev1.Pod, usingEviction bool) {
					if tc.evictionSupported && !tc.disableEviction {
						if !usingEviction {
							t.Errorf("%s: OnPodDeletedOrEvicted callback failed while evicting; actual\n\t%v\nexpected\n\t%v", tc.description, usingEviction, !usingEviction)
						}
					} else if tc.evictionSupported && tc.disableEviction {
						if usingEviction {
							t.Errorf("%s: OnPodDeletedOrEvicted callback failed while deleting; actual\n\t%v\nexpected\n\t%v", tc.description, !usingEviction, usingEviction)
						}
					}
				},
				OnPodDeletionOrEvictionFinished: func(pod *corev1.Pod, usingEviction bool, err error) {
					if tc.evictionSupported && !tc.disableEviction {
						if !usingEviction {
							t.Errorf("%s: OnPodDeletionOrEvictionFinished callback failed while evicting; actual\n\t%v\nexpected\n\t%v", tc.description, usingEviction, !usingEviction)
						}
					} else if tc.evictionSupported && tc.disableEviction {
						if usingEviction {
							t.Errorf("%s: OnPodDeletionOrEvictionFinished callback failed while deleting; actual\n\t%v\nexpected\n\t%v", tc.description, !usingEviction, usingEviction)
						}
					}
				},
			}

			// Create 4 pods, and try to remove the first 2
			var expectedEvictions []policyv1.Eviction
			var create []runtime.Object
			deletePods := []corev1.Pod{}
			for i := 1; i <= 4; i++ {
				pod := &corev1.Pod{}
				pod.Name = fmt.Sprintf("mypod-%d", i)
				pod.Namespace = "default"

				create = append(create, pod)
				if i <= 2 {
					deletePods = append(deletePods, *pod)

					if tc.evictionSupported && !tc.disableEviction {
						eviction := policyv1.Eviction{}
						eviction.Namespace = pod.Namespace
						eviction.Name = pod.Name

						gracePeriodSeconds := int64(h.GracePeriodSeconds)
						eviction.DeleteOptions = &metav1.DeleteOptions{
							GracePeriodSeconds: &gracePeriodSeconds,
						}

						expectedEvictions = append(expectedEvictions, eviction)
					}
				}
			}

			// Build the fake client
			k := fake.NewSimpleClientset(create...)
			if tc.evictionSupported {
				addEvictionSupport(t, k, "v1")
			} else {
				addCoreNonEvictionSupport(t, k)
			}
			h.Client = k
			h.DisableEviction = tc.disableEviction
			// Do the eviction
			if err := h.DeleteOrEvictPods(deletePods); err != nil {
				t.Fatalf("error from DeleteOrEvictPods: %v", err)
			}

			// Test that other pods are still there
			var remainingPods []string
			{
				podList, err := k.CoreV1().Pods("").List(context.TODO(), metav1.ListOptions{})
				if err != nil {
					t.Fatalf("error listing pods: %v", err)
				}

				for _, pod := range podList.Items {
					remainingPods = append(remainingPods, pod.Namespace+"/"+pod.Name)
				}
				sort.Strings(remainingPods)
			}
			expected := []string{"default/mypod-3", "default/mypod-4"}
			if !reflect.DeepEqual(remainingPods, expected) {
				t.Errorf("%s: unexpected remaining pods after DeleteOrEvictPods; actual %v; expected %v", tc.description, remainingPods, expected)
			}

			// Test that pods were evicted as expected
			var actualEvictions []policyv1.Eviction
			for _, action := range k.Actions() {
				if action.GetVerb() != "create" || action.GetResource().Resource != "pods" || action.GetSubresource() != "eviction" {
					continue
				}
				eviction := *action.(ktest.CreateAction).GetObject().(*policyv1.Eviction)
				actualEvictions = append(actualEvictions, eviction)
			}
			sort.Slice(actualEvictions, func(i, j int) bool {
				return actualEvictions[i].Name < actualEvictions[j].Name
			})
			if !reflect.DeepEqual(actualEvictions, expectedEvictions) {
				t.Errorf("%s: unexpected evictions; actual\n\t%v\nexpected\n\t%v", tc.description, actualEvictions, expectedEvictions)
			}
		})
	}
}

func mockFilterSkip(_ corev1.Pod) PodDeleteStatus {
	return MakePodDeleteStatusSkip()
}

func mockFilterOkay(_ corev1.Pod) PodDeleteStatus {
	return MakePodDeleteStatusOkay()
}

func TestFilterPods(t *testing.T) {
	tCases := []struct {
		description        string
		expectedPodListLen int
		additionalFilters  []PodFilter
	}{
		{
			description:        "AdditionalFilter skip all",
			expectedPodListLen: 0,
			additionalFilters: []PodFilter{
				mockFilterSkip,
				mockFilterOkay,
			},
		},
		{
			description:        "AdditionalFilter okay all",
			expectedPodListLen: 1,
			additionalFilters: []PodFilter{
				mockFilterOkay,
			},
		},
		{
			description:        "AdditionalFilter Skip after Okay all skip",
			expectedPodListLen: 0,
			additionalFilters: []PodFilter{
				mockFilterOkay,
				mockFilterSkip,
			},
		},
		{
			description:        "No additionalFilters okay all",
			expectedPodListLen: 1,
		},
	}
	for _, tc := range tCases {
		t.Run(tc.description, func(t *testing.T) {
			h := &Helper{
				Force:             true,
				AdditionalFilters: tc.additionalFilters,
			}
			pod := corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "pod",
					Namespace: "default",
				},
			}
			podList := corev1.PodList{
				Items: []corev1.Pod{
					pod,
				},
			}

			list := filterPods(&podList, h.makeFilters())
			podsLen := len(list.Pods())
			if podsLen != tc.expectedPodListLen {
				t.Errorf("%s: unexpected evictions; actual %v; expected %v", tc.description, podsLen, tc.expectedPodListLen)
			}
		})
	}
}
