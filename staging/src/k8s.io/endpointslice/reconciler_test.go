/*
Copyright 2019 The Kubernetes Authors.

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

package endpointslice

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	corelisters "k8s.io/client-go/listers/core/v1"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/record"
	"k8s.io/component-base/metrics/testutil"
	"k8s.io/endpointslice/metrics"
	"k8s.io/endpointslice/topologycache"
	endpointsliceutil "k8s.io/endpointslice/util"
	"k8s.io/klog/v2/ktesting"
	"k8s.io/utils/pointer"
)

const (
	controllerName = "endpointslice-controller.k8s.io"
)

func expectAction(t *testing.T, actions []k8stesting.Action, index int, verb, resource string) {
	t.Helper()
	if len(actions) <= index {
		t.Fatalf("Expected at least %d actions, got %d", index+1, len(actions))
	}

	action := actions[index]
	if action.GetVerb() != verb {
		t.Errorf("Expected action %d verb to be %s, got %s", index, verb, action.GetVerb())
	}

	if action.GetResource().Resource != resource {
		t.Errorf("Expected action %d resource to be %s, got %s", index, resource, action.GetResource().Resource)
	}
}

// cacheMutationCheck helps ensure that cached objects have not been changed
// in any way throughout a test run.
type cacheMutationCheck struct {
	objects []cacheObject
}

// cacheObject stores a reference to an original object as well as a deep copy
// of that object to track any mutations in the original object.
type cacheObject struct {
	original runtime.Object
	deepCopy runtime.Object
}

// newCacheMutationCheck initializes a cacheMutationCheck with EndpointSlices.
func newCacheMutationCheck(endpointSlices []*discovery.EndpointSlice) cacheMutationCheck {
	cmc := cacheMutationCheck{}
	for _, endpointSlice := range endpointSlices {
		cmc.Add(endpointSlice)
	}
	return cmc
}

// Add appends a runtime.Object and a deep copy of that object into the
// cacheMutationCheck.
func (cmc *cacheMutationCheck) Add(o runtime.Object) {
	cmc.objects = append(cmc.objects, cacheObject{
		original: o,
		deepCopy: o.DeepCopyObject(),
	})
}

// Check verifies that no objects in the cacheMutationCheck have been mutated.
func (cmc *cacheMutationCheck) Check(t *testing.T) {
	for _, o := range cmc.objects {
		if !reflect.DeepEqual(o.original, o.deepCopy) {
			// Cached objects can't be safely mutated and instead should be deep
			// copied before changed in any way.
			t.Errorf("Cached object was unexpectedly mutated. Original: %+v, Mutated: %+v", o.deepCopy, o.original)
		}
	}
}

var defaultMaxEndpointsPerSlice = int32(100)

// Even when there are no pods, we want to have a placeholder slice for each service
func TestReconcileEmpty(t *testing.T) {
	client := newClientset()
	setupMetrics()
	namespace := "test"
	svc, _ := newServiceAndEndpointMeta("foo", namespace)

	r := newReconciler(client, []*corev1.Node{{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}}}, defaultMaxEndpointsPerSlice)
	reconcileHelper(t, r, &svc, []*corev1.Pod{}, []*discovery.EndpointSlice{}, time.Now())
	expectActions(t, client.Actions(), 1, "create", "endpointslices")

	slices := fetchEndpointSlices(t, client, namespace)
	assert.Len(t, slices, 1, "Expected 1 endpoint slices")

	assert.Regexp(t, "^"+svc.Name, slices[0].Name)
	assert.Equal(t, svc.Name, slices[0].Labels[discovery.LabelServiceName])
	assert.EqualValues(t, []discovery.EndpointPort{}, slices[0].Ports)
	assert.EqualValues(t, []discovery.Endpoint{}, slices[0].Endpoints)
	expectTrackedGeneration(t, r.endpointSliceTracker, &slices[0], 1)
	expectMetrics(t, expectedMetrics{desiredSlices: 1, actualSlices: 1, desiredEndpoints: 0, addedPerSync: 0, removedPerSync: 0, numCreated: 1, numUpdated: 0, numDeleted: 0, slicesChangedPerSync: 1})
}

// Given a single pod matching a service selector and no existing endpoint slices,
// a slice should be created
func TestReconcile1Pod(t *testing.T) {
	namespace := "test"
	noFamilyService, _ := newServiceAndEndpointMeta("foo", namespace)
	noFamilyService.Spec.ClusterIP = "10.0.0.10"
	noFamilyService.Spec.IPFamilies = nil

	svcv4, _ := newServiceAndEndpointMeta("foo", namespace)
	svcv4ClusterIP, _ := newServiceAndEndpointMeta("foo", namespace)
	svcv4ClusterIP.Spec.ClusterIP = "1.1.1.1"
	svcv4Labels, _ := newServiceAndEndpointMeta("foo", namespace)
	svcv4Labels.Labels = map[string]string{"foo": "bar"}
	svcv4BadLabels, _ := newServiceAndEndpointMeta("foo", namespace)
	svcv4BadLabels.Labels = map[string]string{discovery.LabelServiceName: "bad",
		discovery.LabelManagedBy: "actor", corev1.IsHeadlessService: "invalid"}
	svcv6, _ := newServiceAndEndpointMeta("foo", namespace)
	svcv6.Spec.IPFamilies = []corev1.IPFamily{corev1.IPv6Protocol}
	svcv6ClusterIP, _ := newServiceAndEndpointMeta("foo", namespace)
	svcv6ClusterIP.Spec.ClusterIP = "1234::5678:0000:0000:9abc:def1"
	// newServiceAndEndpointMeta generates v4 single stack
	svcv6ClusterIP.Spec.IPFamilies = []corev1.IPFamily{corev1.IPv6Protocol}

	// dual stack
	dualStackSvc, _ := newServiceAndEndpointMeta("foo", namespace)
	dualStackSvc.Spec.IPFamilies = []corev1.IPFamily{corev1.IPv4Protocol, corev1.IPv6Protocol}
	dualStackSvc.Spec.ClusterIP = "10.0.0.10"
	dualStackSvc.Spec.ClusterIPs = []string{"10.0.0.10", "2000::1"}

	pod1 := newPod(1, namespace, true, 1, false)
	pod1.Status.PodIPs = []corev1.PodIP{{IP: "1.2.3.4"}, {IP: "1234::5678:0000:0000:9abc:def0"}}
	pod1.Spec.Hostname = "example-hostname"
	node1 := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: pod1.Spec.NodeName,
			Labels: map[string]string{
				"topology.kubernetes.io/zone":   "us-central1-a",
				"topology.kubernetes.io/region": "us-central1",
			},
		},
	}

	testCases := map[string]struct {
		service                  corev1.Service
		expectedAddressType      discovery.AddressType
		expectedEndpoint         discovery.Endpoint
		expectedLabels           map[string]string
		expectedEndpointPerSlice map[discovery.AddressType][]discovery.Endpoint
	}{
		"no-family-service": {
			service: noFamilyService,
			expectedEndpointPerSlice: map[discovery.AddressType][]discovery.Endpoint{
				discovery.AddressTypeIPv4: {
					{
						Addresses: []string{"1.2.3.4"},
						Conditions: discovery.EndpointConditions{
							Ready:       pointer.Bool(true),
							Serving:     pointer.Bool(true),
							Terminating: pointer.Bool(false),
						},
						Zone:     pointer.String("us-central1-a"),
						NodeName: pointer.String("node-1"),
						TargetRef: &corev1.ObjectReference{
							Kind:      "Pod",
							Namespace: namespace,
							Name:      "pod1",
						},
					},
				},
			},
			expectedLabels: map[string]string{
				discovery.LabelManagedBy:   controllerName,
				discovery.LabelServiceName: "foo",
			},
		},
		"ipv4": {
			service: svcv4,
			expectedEndpointPerSlice: map[discovery.AddressType][]discovery.Endpoint{
				discovery.AddressTypeIPv4: {
					{
						Addresses: []string{"1.2.3.4"},
						Conditions: discovery.EndpointConditions{
							Ready:       pointer.Bool(true),
							Serving:     pointer.Bool(true),
							Terminating: pointer.Bool(false),
						},
						Zone:     pointer.String("us-central1-a"),
						NodeName: pointer.String("node-1"),
						TargetRef: &corev1.ObjectReference{
							Kind:      "Pod",
							Namespace: namespace,
							Name:      "pod1",
						},
					},
				},
			},
			expectedLabels: map[string]string{
				discovery.LabelManagedBy:   controllerName,
				discovery.LabelServiceName: "foo",
				corev1.IsHeadlessService:   "",
			},
		},
		"ipv4-clusterip": {
			service: svcv4ClusterIP,
			expectedEndpointPerSlice: map[discovery.AddressType][]discovery.Endpoint{
				discovery.AddressTypeIPv4: {
					{
						Addresses: []string{"1.2.3.4"},
						Conditions: discovery.EndpointConditions{
							Ready:       pointer.Bool(true),
							Serving:     pointer.Bool(true),
							Terminating: pointer.Bool(false),
						},
						Zone:     pointer.String("us-central1-a"),
						NodeName: pointer.String("node-1"),
						TargetRef: &corev1.ObjectReference{
							Kind:      "Pod",
							Namespace: namespace,
							Name:      "pod1",
						},
					},
				},
			},
			expectedAddressType: discovery.AddressTypeIPv4,
			expectedEndpoint: discovery.Endpoint{
				Addresses: []string{"1.2.3.4"},
				Conditions: discovery.EndpointConditions{
					Ready:       pointer.Bool(true),
					Serving:     pointer.Bool(true),
					Terminating: pointer.Bool(false),
				},
				Zone:     pointer.String("us-central1-a"),
				NodeName: pointer.String("node-1"),
				TargetRef: &corev1.ObjectReference{
					Kind:      "Pod",
					Namespace: namespace,
					Name:      "pod1",
				},
			},
			expectedLabels: map[string]string{
				discovery.LabelManagedBy:   controllerName,
				discovery.LabelServiceName: "foo",
			},
		},
		"ipv4-labels": {
			service: svcv4Labels,
			expectedEndpointPerSlice: map[discovery.AddressType][]discovery.Endpoint{
				discovery.AddressTypeIPv4: {
					{
						Addresses: []string{"1.2.3.4"},
						Conditions: discovery.EndpointConditions{
							Ready:       pointer.Bool(true),
							Serving:     pointer.Bool(true),
							Terminating: pointer.Bool(false),
						},
						Zone:     pointer.String("us-central1-a"),
						NodeName: pointer.String("node-1"),
						TargetRef: &corev1.ObjectReference{
							Kind:      "Pod",
							Namespace: namespace,
							Name:      "pod1",
						},
					},
				},
			},
			expectedAddressType: discovery.AddressTypeIPv4,
			expectedEndpoint: discovery.Endpoint{
				Addresses: []string{"1.2.3.4"},
				Conditions: discovery.EndpointConditions{
					Ready:       pointer.Bool(true),
					Serving:     pointer.Bool(true),
					Terminating: pointer.Bool(false),
				},
				Zone:     pointer.String("us-central1-a"),
				NodeName: pointer.String("node-1"),
				TargetRef: &corev1.ObjectReference{
					Kind:      "Pod",
					Namespace: namespace,
					Name:      "pod1",
				},
			},
			expectedLabels: map[string]string{
				discovery.LabelManagedBy:   controllerName,
				discovery.LabelServiceName: "foo",
				"foo":                      "bar",
				corev1.IsHeadlessService:   "",
			},
		},
		"ipv4-bad-labels": {
			service: svcv4BadLabels,
			expectedEndpointPerSlice: map[discovery.AddressType][]discovery.Endpoint{
				discovery.AddressTypeIPv4: {
					{
						Addresses: []string{"1.2.3.4"},
						Conditions: discovery.EndpointConditions{
							Ready:       pointer.Bool(true),
							Serving:     pointer.Bool(true),
							Terminating: pointer.Bool(false),
						},
						Zone:     pointer.String("us-central1-a"),
						NodeName: pointer.String("node-1"),
						TargetRef: &corev1.ObjectReference{
							Kind:      "Pod",
							Namespace: namespace,
							Name:      "pod1",
						},
					},
				},
			},
			expectedAddressType: discovery.AddressTypeIPv4,
			expectedEndpoint: discovery.Endpoint{
				Addresses: []string{"1.2.3.4"},
				Conditions: discovery.EndpointConditions{
					Ready:       pointer.Bool(true),
					Serving:     pointer.Bool(true),
					Terminating: pointer.Bool(false),
				},
				Zone:     pointer.String("us-central1-a"),
				NodeName: pointer.String("node-1"),
				TargetRef: &corev1.ObjectReference{
					Kind:      "Pod",
					Namespace: namespace,
					Name:      "pod1",
				},
			},
			expectedLabels: map[string]string{
				discovery.LabelManagedBy:   controllerName,
				discovery.LabelServiceName: "foo",
				corev1.IsHeadlessService:   "",
			},
		},

		"ipv6": {
			service: svcv6,
			expectedEndpointPerSlice: map[discovery.AddressType][]discovery.Endpoint{
				discovery.AddressTypeIPv6: {
					{
						Addresses: []string{"1234::5678:0000:0000:9abc:def0"},
						Conditions: discovery.EndpointConditions{
							Ready:       pointer.Bool(true),
							Serving:     pointer.Bool(true),
							Terminating: pointer.Bool(false),
						},
						Zone:     pointer.String("us-central1-a"),
						NodeName: pointer.String("node-1"),
						TargetRef: &corev1.ObjectReference{
							Kind:      "Pod",
							Namespace: namespace,
							Name:      "pod1",
						},
					},
				},
			},
			expectedLabels: map[string]string{
				discovery.LabelManagedBy:   controllerName,
				discovery.LabelServiceName: "foo",
				corev1.IsHeadlessService:   "",
			},
		},

		"ipv6-clusterip": {
			service: svcv6ClusterIP,
			expectedEndpointPerSlice: map[discovery.AddressType][]discovery.Endpoint{
				discovery.AddressTypeIPv6: {
					{
						Addresses: []string{"1234::5678:0000:0000:9abc:def0"},
						Conditions: discovery.EndpointConditions{
							Ready:       pointer.Bool(true),
							Serving:     pointer.Bool(true),
							Terminating: pointer.Bool(false),
						},
						Zone:     pointer.String("us-central1-a"),
						NodeName: pointer.String("node-1"),
						TargetRef: &corev1.ObjectReference{
							Kind:      "Pod",
							Namespace: namespace,
							Name:      "pod1",
						},
					},
				},
			},
			expectedLabels: map[string]string{
				discovery.LabelManagedBy:   controllerName,
				discovery.LabelServiceName: "foo",
			},
		},

		"dualstack-service": {
			service: dualStackSvc,
			expectedEndpointPerSlice: map[discovery.AddressType][]discovery.Endpoint{
				discovery.AddressTypeIPv6: {
					{
						Addresses: []string{"1234::5678:0000:0000:9abc:def0"},
						Conditions: discovery.EndpointConditions{
							Ready:       pointer.Bool(true),
							Serving:     pointer.Bool(true),
							Terminating: pointer.Bool(false),
						},
						Zone:     pointer.String("us-central1-a"),
						NodeName: pointer.String("node-1"),
						TargetRef: &corev1.ObjectReference{
							Kind:      "Pod",
							Namespace: namespace,
							Name:      "pod1",
						},
					},
				},
				discovery.AddressTypeIPv4: {
					{
						Addresses: []string{"1.2.3.4"},
						Conditions: discovery.EndpointConditions{
							Ready:       pointer.Bool(true),
							Serving:     pointer.Bool(true),
							Terminating: pointer.Bool(false),
						},
						Zone:     pointer.String("us-central1-a"),
						NodeName: pointer.String("node-1"),
						TargetRef: &corev1.ObjectReference{
							Kind:      "Pod",
							Namespace: namespace,
							Name:      "pod1",
						},
					},
				},
			},
			expectedLabels: map[string]string{
				discovery.LabelManagedBy:   controllerName,
				discovery.LabelServiceName: "foo",
			},
		},
	}

	for name, testCase := range testCases {
		t.Run(name, func(t *testing.T) {
			client := newClientset()
			setupMetrics()
			triggerTime := time.Now().UTC()
			r := newReconciler(client, []*corev1.Node{node1}, defaultMaxEndpointsPerSlice)
			reconcileHelper(t, r, &testCase.service, []*corev1.Pod{pod1}, []*discovery.EndpointSlice{}, triggerTime)

			if len(client.Actions()) != len(testCase.expectedEndpointPerSlice) {
				t.Errorf("Expected %v clientset action, got %d", len(testCase.expectedEndpointPerSlice), len(client.Actions()))
			}

			slices := fetchEndpointSlices(t, client, namespace)

			if len(slices) != len(testCase.expectedEndpointPerSlice) {
				t.Fatalf("Expected %v EndpointSlice, got %d", len(testCase.expectedEndpointPerSlice), len(slices))
			}

			for _, slice := range slices {
				if !strings.HasPrefix(slice.Name, testCase.service.Name) {
					t.Fatalf("Expected EndpointSlice name to start with %s, got %s", testCase.service.Name, slice.Name)
				}

				if !reflect.DeepEqual(testCase.expectedLabels, slice.Labels) {
					t.Errorf("Expected EndpointSlice to have labels: %v , got %v", testCase.expectedLabels, slice.Labels)
				}
				if slice.Labels[discovery.LabelServiceName] != testCase.service.Name {
					t.Fatalf("Expected EndpointSlice to have label set with %s value, got %s", testCase.service.Name, slice.Labels[discovery.LabelServiceName])
				}

				if slice.Annotations[corev1.EndpointsLastChangeTriggerTime] != triggerTime.Format(time.RFC3339Nano) {
					t.Fatalf("Expected EndpointSlice trigger time annotation to be %s, got %s", triggerTime.Format(time.RFC3339Nano), slice.Annotations[corev1.EndpointsLastChangeTriggerTime])
				}

				// validate that this slice has address type matching expected
				expectedEndPointList := testCase.expectedEndpointPerSlice[slice.AddressType]
				if expectedEndPointList == nil {
					t.Fatalf("address type %v is not expected", slice.AddressType)
				}

				if len(slice.Endpoints) != len(expectedEndPointList) {
					t.Fatalf("Expected %v Endpoint, got %d", len(expectedEndPointList), len(slice.Endpoints))
				}

				// test is limited to *ONE* endpoint
				endpoint := slice.Endpoints[0]
				if !reflect.DeepEqual(endpoint, expectedEndPointList[0]) {
					t.Fatalf("Expected endpoint: %+v, got: %+v", expectedEndPointList[0], endpoint)
				}

				expectTrackedGeneration(t, r.endpointSliceTracker, &slice, 1)
			}

			expectSlicesChangedPerSync := 1
			if testCase.service.Spec.IPFamilies != nil && len(testCase.service.Spec.IPFamilies) > 0 {
				expectSlicesChangedPerSync = len(testCase.service.Spec.IPFamilies)
			}
			expectMetrics(t,
				expectedMetrics{
					desiredSlices:        len(testCase.expectedEndpointPerSlice),
					actualSlices:         len(testCase.expectedEndpointPerSlice),
					desiredEndpoints:     len(testCase.expectedEndpointPerSlice),
					addedPerSync:         len(testCase.expectedEndpointPerSlice),
					removedPerSync:       0,
					numCreated:           len(testCase.expectedEndpointPerSlice),
					numUpdated:           0,
					numDeleted:           0,
					slicesChangedPerSync: expectSlicesChangedPerSync,
				})
		})
	}
}

// given an existing placeholder endpoint slice and no pods matching the service, the existing
// slice should not change the placeholder
func TestReconcile1EndpointSlice(t *testing.T) {
	namespace := "test"
	svc, epMeta := newServiceAndEndpointMeta("foo", namespace)
	emptySlice := newEmptyEndpointSlice(1, namespace, epMeta, svc)
	emptySlice.ObjectMeta.Labels = map[string]string{"bar": "baz"}
	logger, _ := ktesting.NewTestContext(t)

	testCases := []struct {
		desc        string
		existing    *discovery.EndpointSlice
		wantUpdate  bool
		wantMetrics expectedMetrics
	}{
		{
			desc:        "No existing placeholder",
			wantUpdate:  true,
			wantMetrics: expectedMetrics{desiredSlices: 1, actualSlices: 1, desiredEndpoints: 0, addedPerSync: 0, removedPerSync: 0, numCreated: 1, numUpdated: 0, numDeleted: 0, slicesChangedPerSync: 1},
		},
		{
			desc:        "Existing placeholder that's the same",
			existing:    newEndpointSlice(logger, &svc, &endpointMeta{ports: []discovery.EndpointPort{}, addressType: discovery.AddressTypeIPv4}, controllerName),
			wantMetrics: expectedMetrics{desiredSlices: 1, actualSlices: 1, desiredEndpoints: 0, addedPerSync: 0, removedPerSync: 0, numCreated: 0, numUpdated: 0, numDeleted: 0, slicesChangedPerSync: 0},
		},
		{
			desc:        "Existing placeholder that's different",
			existing:    emptySlice,
			wantUpdate:  true,
			wantMetrics: expectedMetrics{desiredSlices: 1, actualSlices: 1, desiredEndpoints: 0, addedPerSync: 0, removedPerSync: 0, numCreated: 0, numUpdated: 1, numDeleted: 0, slicesChangedPerSync: 1},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.desc, func(t *testing.T) {
			client := newClientset()
			setupMetrics()

			existingSlices := []*discovery.EndpointSlice{}
			if tc.existing != nil {
				existingSlices = append(existingSlices, tc.existing)
				_, createErr := client.DiscoveryV1().EndpointSlices(namespace).Create(context.TODO(), tc.existing, metav1.CreateOptions{})
				assert.NoError(t, createErr, "Expected no error creating endpoint slice")
			}

			numActionsBefore := len(client.Actions())
			r := newReconciler(client, []*corev1.Node{{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}}}, defaultMaxEndpointsPerSlice)
			reconcileHelper(t, r, &svc, []*corev1.Pod{}, existingSlices, time.Now())

			var numUpdates int
			if tc.wantUpdate {
				numUpdates = 1
			}
			wantActions := numActionsBefore + numUpdates
			assert.Len(t, client.Actions(), wantActions, "Expected %d additional clientset actions", numUpdates)

			slices := fetchEndpointSlices(t, client, namespace)
			assert.Len(t, slices, 1, "Expected 1 endpoint slices")

			if !tc.wantUpdate {
				assert.Regexp(t, "^"+svc.Name, slices[0].Name)
				assert.Equal(t, svc.Name, slices[0].Labels[discovery.LabelServiceName])
				assert.EqualValues(t, []discovery.EndpointPort{}, slices[0].Ports)
				assert.EqualValues(t, []discovery.Endpoint{}, slices[0].Endpoints)
				if tc.existing == nil {
					expectTrackedGeneration(t, r.endpointSliceTracker, &slices[0], 1)
				}
			}
			expectMetrics(t, tc.wantMetrics)
		})
	}
}

func TestPlaceHolderSliceCompare(t *testing.T) {
	testCases := []struct {
		desc string
		x    *discovery.EndpointSlice
		y    *discovery.EndpointSlice
		want bool
	}{
		{
			desc: "Both nil",
			want: true,
		},
		{
			desc: "Y is nil",
			x:    &discovery.EndpointSlice{},
			want: false,
		},
		{
			desc: "X is nil",
			y:    &discovery.EndpointSlice{},
			want: false,
		},
		{
			desc: "Both are empty and non-nil",
			x:    &discovery.EndpointSlice{},
			y:    &discovery.EndpointSlice{},
			want: true,
		},
		{
			desc: "Only ObjectMeta.Name has diff",
			x: &discovery.EndpointSlice{ObjectMeta: metav1.ObjectMeta{
				Name: "foo",
			}},
			y: &discovery.EndpointSlice{ObjectMeta: metav1.ObjectMeta{
				Name: "bar",
			}},
			want: true,
		},
		{
			desc: "Only ObjectMeta.Labels has diff",
			x: &discovery.EndpointSlice{ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					"foo": "true",
				},
			}},
			y: &discovery.EndpointSlice{ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					"bar": "true",
				},
			}},
			want: false,
		},
		{
			desc: "Creation time is different",
			x: &discovery.EndpointSlice{ObjectMeta: metav1.ObjectMeta{
				CreationTimestamp: metav1.Unix(1, 0),
			}},
			y: &discovery.EndpointSlice{ObjectMeta: metav1.ObjectMeta{
				CreationTimestamp: metav1.Unix(2, 0),
			}},
			want: true,
		},
		{
			desc: "Different except for ObjectMeta",
			x:    &discovery.EndpointSlice{AddressType: discovery.AddressTypeIPv4},
			y:    &discovery.EndpointSlice{AddressType: discovery.AddressTypeIPv6},
			want: false,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.desc, func(t *testing.T) {
			got := placeholderSliceCompare.DeepEqual(tc.x, tc.y)
			if got != tc.want {
				t.Errorf("sliceEqual(%v, %v) = %t, want %t", tc.x, tc.y, got, tc.want)
			}
		})
	}
}

// when a Service has PublishNotReadyAddresses set to true, corresponding
// Endpoints should be considered ready, even if the backing Pod is not.
func TestReconcile1EndpointSlicePublishNotReadyAddresses(t *testing.T) {
	client := newClientset()
	namespace := "test"
	svc, _ := newServiceAndEndpointMeta("foo", namespace)
	svc.Spec.PublishNotReadyAddresses = true

	// start with 50 pods, 1/3 not ready
	pods := []*corev1.Pod{}
	for i := 0; i < 50; i++ {
		ready := !(i%3 == 0)
		pods = append(pods, newPod(i, namespace, ready, 1, false))
	}

	r := newReconciler(client, []*corev1.Node{{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}}}, defaultMaxEndpointsPerSlice)
	reconcileHelper(t, r, &svc, pods, []*discovery.EndpointSlice{}, time.Now())

	// Only 1 action, an EndpointSlice create
	assert.Len(t, client.Actions(), 1, "Expected 1 additional clientset action")
	expectActions(t, client.Actions(), 1, "create", "endpointslices")

	// Two endpoint slices should be completely full, the remainder should be in another one
	endpointSlices := fetchEndpointSlices(t, client, namespace)
	for _, endpointSlice := range endpointSlices {
		for i, endpoint := range endpointSlice.Endpoints {
			if !topologycache.EndpointReady(endpoint) {
				t.Errorf("Expected endpoints[%d] to be ready", i)
			}
		}
	}
	expectUnorderedSlicesWithLengths(t, endpointSlices, []int{50})
}

// a simple use case with 250 pods matching a service and no existing slices
// reconcile should create 3 slices, completely filling 2 of them
func TestReconcileManyPods(t *testing.T) {
	client := newClientset()
	setupMetrics()
	namespace := "test"
	svc, _ := newServiceAndEndpointMeta("foo", namespace)

	// start with 250 pods
	pods := []*corev1.Pod{}
	for i := 0; i < 250; i++ {
		ready := !(i%3 == 0)
		pods = append(pods, newPod(i, namespace, ready, 1, false))
	}

	r := newReconciler(client, []*corev1.Node{{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}}}, defaultMaxEndpointsPerSlice)
	reconcileHelper(t, r, &svc, pods, []*discovery.EndpointSlice{}, time.Now())

	// This is an ideal scenario where only 3 actions are required, and they're all creates
	assert.Len(t, client.Actions(), 3, "Expected 3 additional clientset actions")
	expectActions(t, client.Actions(), 3, "create", "endpointslices")

	// Two endpoint slices should be completely full, the remainder should be in another one
	expectUnorderedSlicesWithLengths(t, fetchEndpointSlices(t, client, namespace), []int{100, 100, 50})
	expectMetrics(t, expectedMetrics{desiredSlices: 3, actualSlices: 3, desiredEndpoints: 250, addedPerSync: 250, removedPerSync: 0, numCreated: 3, numUpdated: 0, numDeleted: 0, slicesChangedPerSync: 3})
}

// now with preexisting slices, we have 250 pods matching a service
// the first endpoint slice contains 62 endpoints, all desired
// the second endpoint slice contains 61 endpoints, all desired
// that leaves 127 to add
// to minimize writes, our strategy is to create new slices for multiples of 100
// that leaves 27 to drop in an existing slice
// dropping them in the first slice will result in the slice being closest to full
// this approach requires 1 update + 1 create instead of 2 updates + 1 create
func TestReconcileEndpointSlicesSomePreexisting(t *testing.T) {
	client := newClientset()
	setupMetrics()
	namespace := "test"
	svc, endpointMeta := newServiceAndEndpointMeta("foo", namespace)

	// start with 250 pods
	pods := []*corev1.Pod{}
	for i := 0; i < 250; i++ {
		ready := !(i%3 == 0)
		pods = append(pods, newPod(i, namespace, ready, 1, false))
	}

	// have approximately 1/4 in first slice
	endpointSlice1 := newEmptyEndpointSlice(1, namespace, endpointMeta, svc)
	for i := 1; i < len(pods)-4; i += 4 {
		endpointSlice1.Endpoints = append(endpointSlice1.Endpoints, podToEndpoint(pods[i], &corev1.Node{}, &svc, discovery.AddressTypeIPv4))
	}

	// have approximately 1/4 in second slice
	endpointSlice2 := newEmptyEndpointSlice(2, namespace, endpointMeta, svc)
	for i := 3; i < len(pods)-4; i += 4 {
		endpointSlice2.Endpoints = append(endpointSlice2.Endpoints, podToEndpoint(pods[i], &corev1.Node{}, &svc, discovery.AddressTypeIPv4))
	}

	existingSlices := []*discovery.EndpointSlice{endpointSlice1, endpointSlice2}
	cmc := newCacheMutationCheck(existingSlices)
	createEndpointSlices(t, client, namespace, existingSlices)

	numActionsBefore := len(client.Actions())
	r := newReconciler(client, []*corev1.Node{{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}}}, defaultMaxEndpointsPerSlice)
	reconcileHelper(t, r, &svc, pods, existingSlices, time.Now())

	actions := client.Actions()
	assert.Len(t, actions, numActionsBefore+2, "Expected 2 additional client actions as part of reconcile")
	assert.True(t, actions[numActionsBefore].Matches("create", "endpointslices"), "First action should be create endpoint slice")
	assert.True(t, actions[numActionsBefore+1].Matches("update", "endpointslices"), "Second action should be update endpoint slice")

	// 1 new slice (0->100) + 1 updated slice (62->89)
	expectUnorderedSlicesWithLengths(t, fetchEndpointSlices(t, client, namespace), []int{89, 61, 100})
	expectMetrics(t, expectedMetrics{desiredSlices: 3, actualSlices: 3, desiredEndpoints: 250, addedPerSync: 127, removedPerSync: 0, numCreated: 1, numUpdated: 1, numDeleted: 0, slicesChangedPerSync: 2})

	// ensure cache mutation has not occurred
	cmc.Check(t)
}

// now with preexisting slices, we have 300 pods matching a service
// this scenario will show some less ideal allocation
// the first endpoint slice contains 74 endpoints, all desired
// the second endpoint slice contains 74 endpoints, all desired
// that leaves 152 to add
// to minimize writes, our strategy is to create new slices for multiples of 100
// that leaves 52 to drop in an existing slice
// that capacity could fit if split in the 2 existing slices
// to minimize writes though, reconcile create a new slice with those 52 endpoints
// this approach requires 2 creates instead of 2 updates + 1 create
func TestReconcileEndpointSlicesSomePreexistingWorseAllocation(t *testing.T) {
	client := newClientset()
	setupMetrics()
	namespace := "test"
	svc, endpointMeta := newServiceAndEndpointMeta("foo", namespace)

	// start with 300 pods
	pods := []*corev1.Pod{}
	for i := 0; i < 300; i++ {
		ready := !(i%3 == 0)
		pods = append(pods, newPod(i, namespace, ready, 1, false))
	}

	// have approximately 1/4 in first slice
	endpointSlice1 := newEmptyEndpointSlice(1, namespace, endpointMeta, svc)
	for i := 1; i < len(pods)-4; i += 4 {
		endpointSlice1.Endpoints = append(endpointSlice1.Endpoints, podToEndpoint(pods[i], &corev1.Node{}, &svc, discovery.AddressTypeIPv4))
	}

	// have approximately 1/4 in second slice
	endpointSlice2 := newEmptyEndpointSlice(2, namespace, endpointMeta, svc)
	for i := 3; i < len(pods)-4; i += 4 {
		endpointSlice2.Endpoints = append(endpointSlice2.Endpoints, podToEndpoint(pods[i], &corev1.Node{}, &svc, discovery.AddressTypeIPv4))
	}

	existingSlices := []*discovery.EndpointSlice{endpointSlice1, endpointSlice2}
	cmc := newCacheMutationCheck(existingSlices)
	createEndpointSlices(t, client, namespace, existingSlices)

	numActionsBefore := len(client.Actions())
	r := newReconciler(client, []*corev1.Node{{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}}}, defaultMaxEndpointsPerSlice)
	reconcileHelper(t, r, &svc, pods, existingSlices, time.Now())

	actions := client.Actions()
	assert.Len(t, actions, numActionsBefore+2, "Expected 2 additional client actions as part of reconcile")
	expectActions(t, client.Actions(), 2, "create", "endpointslices")

	// 2 new slices (100, 52) in addition to existing slices (74, 74)
	expectUnorderedSlicesWithLengths(t, fetchEndpointSlices(t, client, namespace), []int{74, 74, 100, 52})
	expectMetrics(t, expectedMetrics{desiredSlices: 3, actualSlices: 4, desiredEndpoints: 300, addedPerSync: 152, removedPerSync: 0, numCreated: 2, numUpdated: 0, numDeleted: 0, slicesChangedPerSync: 2})

	// ensure cache mutation has not occurred
	cmc.Check(t)
}

// In some cases, such as a service port change, all slices for that service will require a change
// This test ensures that we are updating those slices and not calling create + delete for each
func TestReconcileEndpointSlicesUpdating(t *testing.T) {
	client := newClientset()
	namespace := "test"
	svc, _ := newServiceAndEndpointMeta("foo", namespace)

	// start with 250 pods
	pods := []*corev1.Pod{}
	for i := 0; i < 250; i++ {
		ready := !(i%3 == 0)
		pods = append(pods, newPod(i, namespace, ready, 1, false))
	}

	r := newReconciler(client, []*corev1.Node{{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}}}, defaultMaxEndpointsPerSlice)
	reconcileHelper(t, r, &svc, pods, []*discovery.EndpointSlice{}, time.Now())
	numActionsExpected := 3
	assert.Len(t, client.Actions(), numActionsExpected, "Expected 3 additional clientset actions")

	slices := fetchEndpointSlices(t, client, namespace)
	numActionsExpected++
	expectUnorderedSlicesWithLengths(t, slices, []int{100, 100, 50})

	svc.Spec.Ports[0].TargetPort.IntVal = 81
	reconcileHelper(t, r, &svc, pods, []*discovery.EndpointSlice{&slices[0], &slices[1], &slices[2]}, time.Now())

	numActionsExpected += 3
	assert.Len(t, client.Actions(), numActionsExpected, "Expected 3 additional clientset actions")
	expectActions(t, client.Actions(), 3, "update", "endpointslices")

	expectUnorderedSlicesWithLengths(t, fetchEndpointSlices(t, client, namespace), []int{100, 100, 50})
}

// In some cases, such as service labels updates, all slices for that service will require a change
// This test ensures that we are updating those slices and not calling create + delete for each
func TestReconcileEndpointSlicesServicesLabelsUpdating(t *testing.T) {
	client := newClientset()
	namespace := "test"
	svc, _ := newServiceAndEndpointMeta("foo", namespace)

	// start with 250 pods
	pods := []*corev1.Pod{}
	for i := 0; i < 250; i++ {
		ready := !(i%3 == 0)
		pods = append(pods, newPod(i, namespace, ready, 1, false))
	}

	r := newReconciler(client, []*corev1.Node{{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}}}, defaultMaxEndpointsPerSlice)
	reconcileHelper(t, r, &svc, pods, []*discovery.EndpointSlice{}, time.Now())
	numActionsExpected := 3
	assert.Len(t, client.Actions(), numActionsExpected, "Expected 3 additional clientset actions")

	slices := fetchEndpointSlices(t, client, namespace)
	numActionsExpected++
	expectUnorderedSlicesWithLengths(t, slices, []int{100, 100, 50})

	// update service with new labels
	svc.Labels = map[string]string{"foo": "bar"}
	reconcileHelper(t, r, &svc, pods, []*discovery.EndpointSlice{&slices[0], &slices[1], &slices[2]}, time.Now())

	numActionsExpected += 3
	assert.Len(t, client.Actions(), numActionsExpected, "Expected 3 additional clientset actions")
	expectActions(t, client.Actions(), 3, "update", "endpointslices")

	newSlices := fetchEndpointSlices(t, client, namespace)
	expectUnorderedSlicesWithLengths(t, newSlices, []int{100, 100, 50})
	// check that the labels were updated
	for _, slice := range newSlices {
		w, ok := slice.Labels["foo"]
		if !ok {
			t.Errorf("Expected label \"foo\" from parent service not found")
		} else if "bar" != w {
			t.Errorf("Expected EndpointSlice to have parent service labels: have %s value, expected bar", w)
		}
	}
}

// In some cases, such as service labels updates, all slices for that service will require a change
// However, this should not happen for reserved labels
func TestReconcileEndpointSlicesServicesReservedLabels(t *testing.T) {
	client := newClientset()
	namespace := "test"
	svc, _ := newServiceAndEndpointMeta("foo", namespace)

	// start with 250 pods
	pods := []*corev1.Pod{}
	for i := 0; i < 250; i++ {
		ready := !(i%3 == 0)
		pods = append(pods, newPod(i, namespace, ready, 1, false))
	}

	r := newReconciler(client, []*corev1.Node{{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}}}, defaultMaxEndpointsPerSlice)
	reconcileHelper(t, r, &svc, pods, []*discovery.EndpointSlice{}, time.Now())
	numActionsExpected := 3
	assert.Len(t, client.Actions(), numActionsExpected, "Expected 3 additional clientset actions")
	slices := fetchEndpointSlices(t, client, namespace)
	numActionsExpected++
	expectUnorderedSlicesWithLengths(t, slices, []int{100, 100, 50})

	// update service with new labels
	svc.Labels = map[string]string{discovery.LabelServiceName: "bad", discovery.LabelManagedBy: "actor", corev1.IsHeadlessService: "invalid"}
	reconcileHelper(t, r, &svc, pods, []*discovery.EndpointSlice{&slices[0], &slices[1], &slices[2]}, time.Now())
	assert.Len(t, client.Actions(), numActionsExpected, "Expected no additional clientset actions")

	newSlices := fetchEndpointSlices(t, client, namespace)
	expectUnorderedSlicesWithLengths(t, newSlices, []int{100, 100, 50})
}

// In this test, we start with 10 slices that only have 30 endpoints each
// An initial reconcile makes no changes (as desired to limit writes)
// When we change a service port, all slices will need to be updated in some way
// reconcile repacks the endpoints into 3 slices, and deletes the extras
func TestReconcileEndpointSlicesRecycling(t *testing.T) {
	client := newClientset()
	setupMetrics()
	namespace := "test"
	svc, endpointMeta := newServiceAndEndpointMeta("foo", namespace)

	// start with 300 pods
	pods := []*corev1.Pod{}
	for i := 0; i < 300; i++ {
		ready := !(i%3 == 0)
		pods = append(pods, newPod(i, namespace, ready, 1, false))
	}

	// generate 10 existing slices with 30 pods/endpoints each
	existingSlices := []*discovery.EndpointSlice{}
	for i, pod := range pods {
		sliceNum := i / 30
		if i%30 == 0 {
			existingSlices = append(existingSlices, newEmptyEndpointSlice(sliceNum, namespace, endpointMeta, svc))
		}
		existingSlices[sliceNum].Endpoints = append(existingSlices[sliceNum].Endpoints, podToEndpoint(pod, &corev1.Node{}, &svc, discovery.AddressTypeIPv4))
	}

	cmc := newCacheMutationCheck(existingSlices)
	createEndpointSlices(t, client, namespace, existingSlices)

	numActionsBefore := len(client.Actions())
	r := newReconciler(client, []*corev1.Node{{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}}}, defaultMaxEndpointsPerSlice)
	reconcileHelper(t, r, &svc, pods, existingSlices, time.Now())
	// initial reconcile should be a no op, all pods are accounted for in slices, no repacking should be done
	assert.Len(t, client.Actions(), numActionsBefore+0, "Expected 0 additional client actions as part of reconcile")

	// changing a service port should require all slices to be updated, time for a repack
	svc.Spec.Ports[0].TargetPort.IntVal = 81
	reconcileHelper(t, r, &svc, pods, existingSlices, time.Now())

	// this should reflect 3 updates + 7 deletes
	assert.Len(t, client.Actions(), numActionsBefore+10, "Expected 10 additional client actions as part of reconcile")

	// thanks to recycling, we get a free repack of endpoints, resulting in 3 full slices instead of 10 mostly empty slices
	expectUnorderedSlicesWithLengths(t, fetchEndpointSlices(t, client, namespace), []int{100, 100, 100})
	expectMetrics(t, expectedMetrics{desiredSlices: 3, actualSlices: 3, desiredEndpoints: 300, addedPerSync: 300, removedPerSync: 0, numCreated: 0, numUpdated: 3, numDeleted: 7, slicesChangedPerSync: 10})

	// ensure cache mutation has not occurred
	cmc.Check(t)
}

// In this test, we want to verify that endpoints are added to a slice that will
// be closest to full after the operation, even when slices are already marked
// for update.
func TestReconcileEndpointSlicesUpdatePacking(t *testing.T) {
	client := newClientset()
	setupMetrics()
	namespace := "test"
	svc, endpointMeta := newServiceAndEndpointMeta("foo", namespace)

	existingSlices := []*discovery.EndpointSlice{}
	pods := []*corev1.Pod{}

	slice1 := newEmptyEndpointSlice(1, namespace, endpointMeta, svc)
	for i := 0; i < 80; i++ {
		pod := newPod(i, namespace, true, 1, false)
		slice1.Endpoints = append(slice1.Endpoints, podToEndpoint(pod, &corev1.Node{}, &svc, discovery.AddressTypeIPv4))
		pods = append(pods, pod)
	}
	existingSlices = append(existingSlices, slice1)

	slice2 := newEmptyEndpointSlice(2, namespace, endpointMeta, svc)
	for i := 100; i < 120; i++ {
		pod := newPod(i, namespace, true, 1, false)
		slice2.Endpoints = append(slice2.Endpoints, podToEndpoint(pod, &corev1.Node{}, &svc, discovery.AddressTypeIPv4))
		pods = append(pods, pod)
	}
	existingSlices = append(existingSlices, slice2)

	cmc := newCacheMutationCheck(existingSlices)
	createEndpointSlices(t, client, namespace, existingSlices)

	// ensure that endpoints in each slice will be marked for update.
	for i, pod := range pods {
		if i%10 == 0 {
			pod.Status.Conditions = []corev1.PodCondition{{
				Type:   corev1.PodReady,
				Status: corev1.ConditionFalse,
			}}
		}
	}

	// add a few additional endpoints - no more than could fit in either slice.
	for i := 200; i < 215; i++ {
		pods = append(pods, newPod(i, namespace, true, 1, false))
	}

	r := newReconciler(client, []*corev1.Node{{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}}}, defaultMaxEndpointsPerSlice)
	reconcileHelper(t, r, &svc, pods, existingSlices, time.Now())

	// ensure that both endpoint slices have been updated
	expectActions(t, client.Actions(), 2, "update", "endpointslices")
	expectMetrics(t, expectedMetrics{desiredSlices: 2, actualSlices: 2, desiredEndpoints: 115, addedPerSync: 15, removedPerSync: 0, numCreated: 0, numUpdated: 2, numDeleted: 0, slicesChangedPerSync: 2})

	// additional pods should get added to fuller slice
	expectUnorderedSlicesWithLengths(t, fetchEndpointSlices(t, client, namespace), []int{95, 20})

	// ensure cache mutation has not occurred
	cmc.Check(t)
}

// In this test, we want to verify that old EndpointSlices with a deprecated IP
// address type will be replaced with a newer IPv4 type.
func TestReconcileEndpointSlicesReplaceDeprecated(t *testing.T) {
	client := newClientset()
	setupMetrics()
	namespace := "test"

	svc, endpointMeta := newServiceAndEndpointMeta("foo", namespace)
	// "IP" is a deprecated address type, ensuring that it is handled properly.
	endpointMeta.addressType = discovery.AddressType("IP")

	existingSlices := []*discovery.EndpointSlice{}
	pods := []*corev1.Pod{}

	slice1 := newEmptyEndpointSlice(1, namespace, endpointMeta, svc)
	for i := 0; i < 80; i++ {
		pod := newPod(i, namespace, true, 1, false)
		slice1.Endpoints = append(slice1.Endpoints, podToEndpoint(pod, &corev1.Node{}, &corev1.Service{Spec: corev1.ServiceSpec{}}, discovery.AddressTypeIPv4))
		pods = append(pods, pod)
	}
	existingSlices = append(existingSlices, slice1)

	slice2 := newEmptyEndpointSlice(2, namespace, endpointMeta, svc)
	for i := 100; i < 150; i++ {
		pod := newPod(i, namespace, true, 1, false)
		slice2.Endpoints = append(slice2.Endpoints, podToEndpoint(pod, &corev1.Node{}, &corev1.Service{Spec: corev1.ServiceSpec{}}, discovery.AddressTypeIPv4))
		pods = append(pods, pod)
	}
	existingSlices = append(existingSlices, slice2)

	createEndpointSlices(t, client, namespace, existingSlices)

	cmc := newCacheMutationCheck(existingSlices)
	r := newReconciler(client, []*corev1.Node{{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}}}, defaultMaxEndpointsPerSlice)
	reconcileHelper(t, r, &svc, pods, existingSlices, time.Now())

	// ensure that both original endpoint slices have been deleted
	expectActions(t, client.Actions(), 2, "delete", "endpointslices")

	endpointSlices := fetchEndpointSlices(t, client, namespace)

	// since this involved replacing both EndpointSlices, the result should be
	// perfectly packed.
	expectUnorderedSlicesWithLengths(t, endpointSlices, []int{100, 30})

	for _, endpointSlice := range endpointSlices {
		if endpointSlice.AddressType != discovery.AddressTypeIPv4 {
			t.Errorf("Expected address type to be IPv4, got %s", endpointSlice.AddressType)
		}
	}

	// ensure cache mutation has not occurred
	cmc.Check(t)
}

// In this test, we want to verify that a Service recreation will result in new
// EndpointSlices being created.
func TestReconcileEndpointSlicesRecreation(t *testing.T) {
	testCases := []struct {
		name           string
		ownedByService bool
		expectChanges  bool
	}{
		{
			name:           "slice owned by Service",
			ownedByService: true,
			expectChanges:  false,
		}, {
			name:           "slice owned by other Service UID",
			ownedByService: false,
			expectChanges:  true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			client := newClientset()
			setupMetrics()
			namespace := "test"

			svc, endpointMeta := newServiceAndEndpointMeta("foo", namespace)
			slice := newEmptyEndpointSlice(1, namespace, endpointMeta, svc)

			pod := newPod(1, namespace, true, 1, false)
			slice.Endpoints = append(slice.Endpoints, podToEndpoint(pod, &corev1.Node{}, &corev1.Service{Spec: corev1.ServiceSpec{}}, discovery.AddressTypeIPv4))

			if !tc.ownedByService {
				slice.OwnerReferences[0].UID = "different"
			}
			existingSlices := []*discovery.EndpointSlice{slice}
			createEndpointSlices(t, client, namespace, existingSlices)

			cmc := newCacheMutationCheck(existingSlices)

			numActionsBefore := len(client.Actions())
			r := newReconciler(client, []*corev1.Node{{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}}}, defaultMaxEndpointsPerSlice)
			reconcileHelper(t, r, &svc, []*corev1.Pod{pod}, existingSlices, time.Now())

			if tc.expectChanges {
				if len(client.Actions()) != numActionsBefore+2 {
					t.Fatalf("Expected 2 additional actions, got %d", len(client.Actions())-numActionsBefore)
				}

				expectAction(t, client.Actions(), numActionsBefore, "create", "endpointslices")
				expectAction(t, client.Actions(), numActionsBefore+1, "delete", "endpointslices")

				fetchedSlices := fetchEndpointSlices(t, client, namespace)

				if len(fetchedSlices) != 1 {
					t.Fatalf("Expected 1 EndpointSlice to exist, got %d", len(fetchedSlices))
				}
			} else {
				if len(client.Actions()) != numActionsBefore {
					t.Errorf("Expected no additional actions, got %d", len(client.Actions())-numActionsBefore)
				}
			}
			// ensure cache mutation has not occurred
			cmc.Check(t)
		})
	}
}

// Named ports can map to different port numbers on different pods.
// This test ensures that EndpointSlices are grouped correctly in that case.
func TestReconcileEndpointSlicesNamedPorts(t *testing.T) {
	client := newClientset()
	setupMetrics()
	namespace := "test"

	portNameIntStr := intstr.IntOrString{
		Type:   intstr.String,
		StrVal: "http",
	}

	svc := corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "named-port-example", Namespace: namespace},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{{
				TargetPort: portNameIntStr,
				Protocol:   corev1.ProtocolTCP,
			}},
			Selector:   map[string]string{"foo": "bar"},
			IPFamilies: []corev1.IPFamily{corev1.IPv4Protocol},
		},
	}

	// start with 300 pods
	pods := []*corev1.Pod{}
	for i := 0; i < 300; i++ {
		ready := !(i%3 == 0)
		portOffset := i % 5
		pod := newPod(i, namespace, ready, 1, false)
		pod.Spec.Containers[0].Ports = []corev1.ContainerPort{{
			Name:          portNameIntStr.StrVal,
			ContainerPort: int32(8080 + portOffset),
			Protocol:      corev1.ProtocolTCP,
		}}
		pods = append(pods, pod)
	}

	r := newReconciler(client, []*corev1.Node{{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}}}, defaultMaxEndpointsPerSlice)
	reconcileHelper(t, r, &svc, pods, []*discovery.EndpointSlice{}, time.Now())

	// reconcile should create 5 endpoint slices
	assert.Len(t, client.Actions(), 5, "Expected 5 client actions as part of reconcile")
	expectActions(t, client.Actions(), 5, "create", "endpointslices")
	expectMetrics(t, expectedMetrics{desiredSlices: 5, actualSlices: 5, desiredEndpoints: 300, addedPerSync: 300, removedPerSync: 0, numCreated: 5, numUpdated: 0, numDeleted: 0, slicesChangedPerSync: 5})

	fetchedSlices := fetchEndpointSlices(t, client, namespace)

	// each slice should have 60 endpoints to match 5 unique variations of named port mapping
	expectUnorderedSlicesWithLengths(t, fetchedSlices, []int{60, 60, 60, 60, 60})

	// generate data structures for expected slice ports and address types
	protoTCP := corev1.ProtocolTCP
	expectedSlices := []discovery.EndpointSlice{}
	for i := range fetchedSlices {
		expectedSlices = append(expectedSlices, discovery.EndpointSlice{
			Ports: []discovery.EndpointPort{{
				Name:     pointer.String(""),
				Protocol: &protoTCP,
				Port:     pointer.Int32(int32(8080 + i)),
			}},
			AddressType: discovery.AddressTypeIPv4,
		})
	}

	// slices fetched should match expected address type and ports
	expectUnorderedSlicesWithTopLevelAttrs(t, fetchedSlices, expectedSlices)
}

// This test ensures that maxEndpointsPerSlice configuration results in
// appropriate endpoints distribution among slices
func TestReconcileMaxEndpointsPerSlice(t *testing.T) {
	namespace := "test"
	svc, _ := newServiceAndEndpointMeta("foo", namespace)

	// start with 250 pods
	pods := []*corev1.Pod{}
	for i := 0; i < 250; i++ {
		ready := !(i%3 == 0)
		pods = append(pods, newPod(i, namespace, ready, 1, false))
	}

	testCases := []struct {
		maxEndpointsPerSlice int32
		expectedSliceLengths []int
		expectedMetricValues expectedMetrics
	}{
		{
			maxEndpointsPerSlice: int32(50),
			expectedSliceLengths: []int{50, 50, 50, 50, 50},
			expectedMetricValues: expectedMetrics{desiredSlices: 5, actualSlices: 5, desiredEndpoints: 250, addedPerSync: 250, numCreated: 5, slicesChangedPerSync: 5},
		}, {
			maxEndpointsPerSlice: int32(80),
			expectedSliceLengths: []int{80, 80, 80, 10},
			expectedMetricValues: expectedMetrics{desiredSlices: 4, actualSlices: 4, desiredEndpoints: 250, addedPerSync: 250, numCreated: 4, slicesChangedPerSync: 4},
		}, {
			maxEndpointsPerSlice: int32(150),
			expectedSliceLengths: []int{150, 100},
			expectedMetricValues: expectedMetrics{desiredSlices: 2, actualSlices: 2, desiredEndpoints: 250, addedPerSync: 250, numCreated: 2, slicesChangedPerSync: 2},
		}, {
			maxEndpointsPerSlice: int32(250),
			expectedSliceLengths: []int{250},
			expectedMetricValues: expectedMetrics{desiredSlices: 1, actualSlices: 1, desiredEndpoints: 250, addedPerSync: 250, numCreated: 1, slicesChangedPerSync: 1},
		}, {
			maxEndpointsPerSlice: int32(500),
			expectedSliceLengths: []int{250},
			expectedMetricValues: expectedMetrics{desiredSlices: 1, actualSlices: 1, desiredEndpoints: 250, addedPerSync: 250, numCreated: 1, slicesChangedPerSync: 1},
		},
	}

	for _, testCase := range testCases {
		t.Run(fmt.Sprintf("maxEndpointsPerSlice: %d", testCase.maxEndpointsPerSlice), func(t *testing.T) {
			client := newClientset()
			setupMetrics()
			r := newReconciler(client, []*corev1.Node{{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}}}, testCase.maxEndpointsPerSlice)
			reconcileHelper(t, r, &svc, pods, []*discovery.EndpointSlice{}, time.Now())
			expectUnorderedSlicesWithLengths(t, fetchEndpointSlices(t, client, namespace), testCase.expectedSliceLengths)
			expectMetrics(t, testCase.expectedMetricValues)
		})
	}
}

func TestReconcileEndpointSlicesMetrics(t *testing.T) {
	client := newClientset()
	setupMetrics()
	namespace := "test"
	svc, _ := newServiceAndEndpointMeta("foo", namespace)

	// start with 20 pods
	pods := []*corev1.Pod{}
	for i := 0; i < 20; i++ {
		pods = append(pods, newPod(i, namespace, true, 1, false))
	}

	r := newReconciler(client, []*corev1.Node{{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}}}, defaultMaxEndpointsPerSlice)
	reconcileHelper(t, r, &svc, pods, []*discovery.EndpointSlice{}, time.Now())

	actions := client.Actions()
	assert.Len(t, actions, 1, "Expected 1 additional client actions as part of reconcile")
	assert.True(t, actions[0].Matches("create", "endpointslices"), "First action should be create endpoint slice")

	expectMetrics(t, expectedMetrics{desiredSlices: 1, actualSlices: 1, desiredEndpoints: 20, addedPerSync: 20, removedPerSync: 0, numCreated: 1, numUpdated: 0, numDeleted: 0, slicesChangedPerSync: 1})

	fetchedSlices := fetchEndpointSlices(t, client, namespace)
	reconcileHelper(t, r, &svc, pods[0:10], []*discovery.EndpointSlice{&fetchedSlices[0]}, time.Now())
	expectMetrics(t, expectedMetrics{desiredSlices: 1, actualSlices: 1, desiredEndpoints: 10, addedPerSync: 20, removedPerSync: 10, numCreated: 1, numUpdated: 1, numDeleted: 0, slicesChangedPerSync: 2})
}

// When a Service has a non-nil deletionTimestamp we want to avoid creating any
// new EndpointSlices but continue to allow updates and deletes through. This
// test uses 3 EndpointSlices, 1 "to-create", 1 "to-update", and 1 "to-delete".
// Each test case exercises different combinations of calls to finalize with
// those resources.
func TestReconcilerFinalizeSvcDeletionTimestamp(t *testing.T) {
	now := metav1.Now()

	testCases := []struct {
		name               string
		deletionTimestamp  *metav1.Time
		attemptCreate      bool
		attemptUpdate      bool
		attemptDelete      bool
		expectCreatedSlice bool
		expectUpdatedSlice bool
		expectDeletedSlice bool
	}{{
		name:               "Attempt create and update, nil deletion timestamp",
		deletionTimestamp:  nil,
		attemptCreate:      true,
		attemptUpdate:      true,
		expectCreatedSlice: true,
		expectUpdatedSlice: true,
		expectDeletedSlice: true,
	}, {
		name:               "Attempt create and update, deletion timestamp set",
		deletionTimestamp:  &now,
		attemptCreate:      true,
		attemptUpdate:      true,
		expectCreatedSlice: false,
		expectUpdatedSlice: true,
		expectDeletedSlice: true,
	}, {
		// Slice scheduled for creation is transitioned to update of Slice
		// scheduled for deletion.
		name:               "Attempt create, update, and delete, nil deletion timestamp, recycling in action",
		deletionTimestamp:  nil,
		attemptCreate:      true,
		attemptUpdate:      true,
		attemptDelete:      true,
		expectCreatedSlice: false,
		expectUpdatedSlice: true,
		expectDeletedSlice: true,
	}, {
		// Slice scheduled for creation is transitioned to update of Slice
		// scheduled for deletion.
		name:               "Attempt create, update, and delete, deletion timestamp set, recycling in action",
		deletionTimestamp:  &now,
		attemptCreate:      true,
		attemptUpdate:      true,
		attemptDelete:      true,
		expectCreatedSlice: false,
		expectUpdatedSlice: true,
		expectDeletedSlice: true,
	}, {
		// Update and delete continue to work when deletionTimestamp is set.
		name:               "Attempt update delete, deletion timestamp set",
		deletionTimestamp:  &now,
		attemptCreate:      false,
		attemptUpdate:      true,
		attemptDelete:      true,
		expectCreatedSlice: false,
		expectUpdatedSlice: true,
		expectDeletedSlice: false,
	}}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			client := newClientset()
			setupMetrics()
			r := newReconciler(client, []*corev1.Node{{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}}}, defaultMaxEndpointsPerSlice)

			namespace := "test"
			svc, endpointMeta := newServiceAndEndpointMeta("foo", namespace)
			svc.DeletionTimestamp = tc.deletionTimestamp
			gvk := schema.GroupVersionKind{Version: "v1", Kind: "Service"}
			ownerRef := metav1.NewControllerRef(&svc, gvk)

			esToCreate := &discovery.EndpointSlice{
				ObjectMeta: metav1.ObjectMeta{
					Name:            "to-create",
					OwnerReferences: []metav1.OwnerReference{*ownerRef},
				},
				AddressType: endpointMeta.addressType,
				Ports:       endpointMeta.ports,
			}

			// Add EndpointSlice that can be updated.
			esToUpdate, err := client.DiscoveryV1().EndpointSlices(namespace).Create(context.TODO(), &discovery.EndpointSlice{
				ObjectMeta: metav1.ObjectMeta{
					Name:            "to-update",
					OwnerReferences: []metav1.OwnerReference{*ownerRef},
				},
				AddressType: endpointMeta.addressType,
				Ports:       endpointMeta.ports,
			}, metav1.CreateOptions{})
			if err != nil {
				t.Fatalf("Expected no error creating EndpointSlice during test setup, got %v", err)
			}
			// Add an endpoint so we can see if this has actually been updated by
			// finalize func.
			esToUpdate.Endpoints = []discovery.Endpoint{{Addresses: []string{"10.2.3.4"}}}

			// Add EndpointSlice that can be deleted.
			esToDelete, err := client.DiscoveryV1().EndpointSlices(namespace).Create(context.TODO(), &discovery.EndpointSlice{
				ObjectMeta: metav1.ObjectMeta{
					Name:            "to-delete",
					OwnerReferences: []metav1.OwnerReference{*ownerRef},
				},
				AddressType: endpointMeta.addressType,
				Ports:       endpointMeta.ports,
			}, metav1.CreateOptions{})
			if err != nil {
				t.Fatalf("Expected no error creating EndpointSlice during test setup, got %v", err)
			}

			slicesToCreate := []*discovery.EndpointSlice{}
			if tc.attemptCreate {
				slicesToCreate = append(slicesToCreate, esToCreate.DeepCopy())
			}
			slicesToUpdate := []*discovery.EndpointSlice{}
			if tc.attemptUpdate {
				slicesToUpdate = append(slicesToUpdate, esToUpdate.DeepCopy())
			}
			slicesToDelete := []*discovery.EndpointSlice{}
			if tc.attemptDelete {
				slicesToDelete = append(slicesToDelete, esToDelete.DeepCopy())
			}

			err = r.finalize(&svc, slicesToCreate, slicesToUpdate, slicesToDelete, time.Now())
			if err != nil {
				t.Errorf("Error calling r.finalize(): %v", err)
			}

			fetchedSlices := fetchEndpointSlices(t, client, namespace)

			createdSliceFound := false
			updatedSliceFound := false
			deletedSliceFound := false
			for _, epSlice := range fetchedSlices {
				if epSlice.Name == esToCreate.Name {
					createdSliceFound = true
				}
				if epSlice.Name == esToUpdate.Name {
					updatedSliceFound = true
					if tc.attemptUpdate && len(epSlice.Endpoints) != len(esToUpdate.Endpoints) {
						t.Errorf("Expected EndpointSlice to be updated with %d endpoints, got %d endpoints", len(esToUpdate.Endpoints), len(epSlice.Endpoints))
					}
				}
				if epSlice.Name == esToDelete.Name {
					deletedSliceFound = true
				}
			}

			if createdSliceFound != tc.expectCreatedSlice {
				t.Errorf("Expected created EndpointSlice existence to be %t, got %t", tc.expectCreatedSlice, createdSliceFound)
			}

			if updatedSliceFound != tc.expectUpdatedSlice {
				t.Errorf("Expected updated EndpointSlice existence to be %t, got %t", tc.expectUpdatedSlice, updatedSliceFound)
			}

			if deletedSliceFound != tc.expectDeletedSlice {
				t.Errorf("Expected deleted EndpointSlice existence to be %t, got %t", tc.expectDeletedSlice, deletedSliceFound)
			}
		})
	}
}

// When a Pod references a Node that is not present in the informer cache the
// EndpointSlices will continue to allow create, updates and deletes through.
// The Pod with the missing reference will be published or retried depending of
// the service.spec.PublishNotReadyAddresses.
// The test considers two Pods on different nodes, one of the Nodes is not present.
func TestReconcilerPodMissingNode(t *testing.T) {
	namespace := "test"

	nodes := make([]*corev1.Node, 2)
	nodes[0] = &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-0"}}
	nodes[1] = &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}}

	pods := make([]*corev1.Pod, 2)
	pods[0] = newPod(0, namespace, true, 1, false)
	pods[0].Spec.NodeName = nodes[0].Name
	pods[1] = newPod(1, namespace, true, 1, false)
	pods[1].Spec.NodeName = nodes[1].Name

	service, endpointMeta := newServiceAndEndpointMeta("foo", namespace)

	testCases := []struct {
		name            string
		publishNotReady bool
		existingNodes   []*corev1.Node
		existingSlice   func() *discovery.EndpointSlice
		expectedMetrics expectedMetrics
		expectError     bool
	}{{
		name:            "Create and publishNotReady false",
		publishNotReady: false,
		existingNodes:   []*corev1.Node{nodes[0]},
		expectedMetrics: expectedMetrics{
			desiredSlices:        1,
			actualSlices:         1,
			desiredEndpoints:     1,
			addedPerSync:         1,
			removedPerSync:       0,
			numCreated:           1,
			numUpdated:           0,
			numDeleted:           0,
			slicesChangedPerSync: 1,
		},
		expectError: true,
	}, {
		name:            "Create and publishNotReady true",
		publishNotReady: true,
		existingNodes:   []*corev1.Node{nodes[0]},
		expectedMetrics: expectedMetrics{
			desiredSlices:        1,
			actualSlices:         1,
			desiredEndpoints:     2,
			addedPerSync:         2,
			removedPerSync:       0,
			numCreated:           1,
			numUpdated:           0,
			numDeleted:           0,
			slicesChangedPerSync: 1,
		},
		expectError: false,
	}, {
		name:            "Update and publishNotReady false",
		publishNotReady: false,
		existingNodes:   []*corev1.Node{nodes[0]},
		existingSlice: func() *discovery.EndpointSlice {
			slice := newEmptyEndpointSlice(1, namespace, endpointMeta, service)
			slice.Endpoints = append(slice.Endpoints, podToEndpoint(pods[0], nodes[0], &service, discovery.AddressTypeIPv4))
			slice.Endpoints = append(slice.Endpoints, podToEndpoint(pods[1], nodes[1], &service, discovery.AddressTypeIPv4))
			return slice
		},
		expectedMetrics: expectedMetrics{
			desiredSlices:        1,
			actualSlices:         1,
			desiredEndpoints:     1,
			addedPerSync:         0,
			removedPerSync:       1,
			numCreated:           0,
			numUpdated:           1,
			numDeleted:           0,
			slicesChangedPerSync: 1,
		},
		expectError: true,
	}, {
		name:            "Update and publishNotReady true and all nodes missing",
		publishNotReady: true,
		existingSlice: func() *discovery.EndpointSlice {
			slice := newEmptyEndpointSlice(1, namespace, endpointMeta, service)
			slice.Endpoints = append(slice.Endpoints, podToEndpoint(pods[0], nodes[0], &service, discovery.AddressTypeIPv4))
			slice.Endpoints = append(slice.Endpoints, podToEndpoint(pods[1], nodes[1], &service, discovery.AddressTypeIPv4))
			return slice
		},
		expectedMetrics: expectedMetrics{
			desiredSlices:        1,
			actualSlices:         1,
			desiredEndpoints:     2,
			addedPerSync:         0,
			removedPerSync:       0,
			numCreated:           0,
			numUpdated:           0,
			numDeleted:           0,
			slicesChangedPerSync: 0,
		},
		expectError: false,
	}, {
		name:            "Update and publishNotReady true",
		publishNotReady: true,
		existingNodes:   []*corev1.Node{nodes[0]},
		existingSlice: func() *discovery.EndpointSlice {
			slice := newEmptyEndpointSlice(1, namespace, endpointMeta, service)
			slice.Endpoints = append(slice.Endpoints, podToEndpoint(pods[0], nodes[0], &service, discovery.AddressTypeIPv4))
			slice.Endpoints = append(slice.Endpoints, podToEndpoint(pods[1], nodes[1], &service, discovery.AddressTypeIPv4))
			return slice
		},
		expectedMetrics: expectedMetrics{
			desiredSlices:        1,
			actualSlices:         1,
			desiredEndpoints:     2,
			addedPerSync:         0,
			removedPerSync:       0,
			numCreated:           0,
			numUpdated:           0,
			numDeleted:           0,
			slicesChangedPerSync: 0,
		},
		expectError: false,
	}, {
		name:            "Update if publishNotReady false and no nodes are present",
		publishNotReady: false,
		existingNodes:   []*corev1.Node{},
		existingSlice: func() *discovery.EndpointSlice {
			slice := newEmptyEndpointSlice(1, namespace, endpointMeta, service)
			slice.Endpoints = append(slice.Endpoints, podToEndpoint(pods[0], nodes[0], &service, discovery.AddressTypeIPv4))
			slice.Endpoints = append(slice.Endpoints, podToEndpoint(pods[1], nodes[1], &service, discovery.AddressTypeIPv4))
			return slice
		},
		expectedMetrics: expectedMetrics{
			desiredSlices:    1,
			actualSlices:     1,
			desiredEndpoints: 0,
			addedPerSync:     0,
			removedPerSync:   2,
			numCreated:       0,
			// the slice is updated not deleted
			numUpdated:           1,
			numDeleted:           0,
			slicesChangedPerSync: 1,
		},
		expectError: true,
	},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			client := newClientset()
			setupMetrics()
			r := newReconciler(client, tc.existingNodes, defaultMaxEndpointsPerSlice)
			logger, _ := ktesting.NewTestContext(t)

			svc := service.DeepCopy()
			svc.Spec.PublishNotReadyAddresses = tc.publishNotReady
			existingSlices := []*discovery.EndpointSlice{}
			if tc.existingSlice != nil {
				slice := tc.existingSlice()
				existingSlices = append(existingSlices, slice)
				_, createErr := client.DiscoveryV1().EndpointSlices(namespace).Create(context.TODO(), slice, metav1.CreateOptions{})
				if createErr != nil {
					t.Errorf("Expected no error creating endpoint slice")
				}
			}
			err := r.Reconcile(logger, svc, pods, existingSlices, time.Now())
			if err == nil && tc.expectError {
				t.Errorf("Expected error but no error received")
			}
			if err != nil && !tc.expectError {
				t.Errorf("Unexpected error: %v", err)
			}

			fetchedSlices := fetchEndpointSlices(t, client, namespace)
			if len(fetchedSlices) != tc.expectedMetrics.actualSlices {
				t.Fatalf("Actual slices %d doesn't match metric %d", len(fetchedSlices), tc.expectedMetrics.actualSlices)
			}
			expectMetrics(t, tc.expectedMetrics)

		})
	}
}

func TestReconcileTopology(t *testing.T) {
	ns := "testing"
	svc, endpointMeta := newServiceAndEndpointMeta("foo", ns)

	// 3 zones, 10 nodes and pods per zone
	zones := []string{"zone-a", "zone-b", "zone-c"}

	pods := []*corev1.Pod{}
	nodes := []*corev1.Node{}
	nodesByName := map[string]*corev1.Node{}
	for i, zone := range zones {
		for j := 0; j < 10; j++ {
			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: fmt.Sprintf("node-%s-%d", zone, j),
					Labels: map[string]string{
						corev1.LabelTopologyZone: zone,
					},
				},
				Status: corev1.NodeStatus{
					Conditions: []corev1.NodeCondition{{
						Type:   corev1.NodeReady,
						Status: corev1.ConditionTrue,
					}},
					Allocatable: corev1.ResourceList{"cpu": resource.MustParse("100m")},
				},
			}
			nodesByName[node.Name] = node
			nodes = append(nodes, node)

			pod := newPod(i*100+j, ns, true, 1, false)
			pod.Spec.NodeName = node.Name
			pods = append(pods, pod)
		}
	}

	slicesByName := map[string]*discovery.EndpointSlice{}
	slicePods := map[string][]*corev1.Pod{
		"zone-a-b": {pods[7], pods[8], pods[16], pods[17], pods[18]},
		"zone-a-c": {pods[5], pods[6], pods[25], pods[26]},
		"zone-c":   {pods[27], pods[28], pods[29]},
	}

	gvk := schema.GroupVersionKind{Version: "v1", Kind: "Service"}
	ownerRef := metav1.NewControllerRef(&svc, gvk)

	for name, pods := range slicePods {
		endpoints := []discovery.Endpoint{}
		for _, pod := range pods {
			endpoints = append(endpoints, podToEndpoint(pod, nodesByName[pod.Spec.NodeName], &svc, endpointMeta.addressType))
		}

		slicesByName[name] = &discovery.EndpointSlice{
			ObjectMeta: metav1.ObjectMeta{
				Name:            name,
				OwnerReferences: []metav1.OwnerReference{*ownerRef},
				Labels: map[string]string{
					discovery.LabelManagedBy:   controllerName,
					discovery.LabelServiceName: svc.Name,
				},
			},
			AddressType: endpointMeta.addressType,
			Ports:       endpointMeta.ports,
			Endpoints:   endpoints,
		}
	}

	testCases := []struct {
		name                   string
		topologyCacheEnabled   bool
		hintsAnnotation        string
		existingSlices         []*discovery.EndpointSlice
		pods                   []*corev1.Pod
		nodes                  []*corev1.Node
		expectedHints          map[string]int
		expectedCrossZoneHints int
		expectedMetrics        expectedMetrics
	}{{
		name:                   "no change, topologyCache disabled, annotation == auto",
		topologyCacheEnabled:   false,
		hintsAnnotation:        "auto",
		existingSlices:         []*discovery.EndpointSlice{slicesByName["zone-c"]},
		pods:                   slicePods["zone-c"],
		nodes:                  nodes,
		expectedHints:          nil,
		expectedCrossZoneHints: 0,
		expectedMetrics: expectedMetrics{
			desiredSlices:        1,
			actualSlices:         1,
			desiredEndpoints:     3,
			addedPerSync:         0,
			removedPerSync:       0,
			numCreated:           0,
			numUpdated:           0,
			numDeleted:           0,
			slicesChangedPerSync: 0,
		},
	}, {
		name:                 "enabling topologyCache, hintsAnnotation == auto",
		topologyCacheEnabled: true,
		hintsAnnotation:      "auto",
		existingSlices:       []*discovery.EndpointSlice{slicesByName["zone-c"]},
		pods:                 slicePods["zone-c"],
		nodes:                nodes,
		expectedHints: map[string]int{
			"zone-a": 1,
			"zone-b": 1,
			"zone-c": 1,
		},
		expectedCrossZoneHints: 2,
		expectedMetrics: expectedMetrics{
			desiredSlices:                1,
			actualSlices:                 1,
			desiredEndpoints:             3,
			addedPerSync:                 0,
			removedPerSync:               0,
			numCreated:                   0,
			numUpdated:                   1,
			numDeleted:                   0,
			slicesChangedPerSyncTopology: 1,
		},
	}, {
		name:                   "topology enabled,  hintsAnnotation==auto, ratio beyond threshold",
		topologyCacheEnabled:   true,
		hintsAnnotation:        "auto",
		existingSlices:         []*discovery.EndpointSlice{slicesByName["zone-a-c"]},
		pods:                   slicePods["zone-a-c"],
		nodes:                  nodes,
		expectedHints:          nil,
		expectedCrossZoneHints: 0,
		expectedMetrics: expectedMetrics{
			desiredSlices:                1,
			actualSlices:                 1,
			desiredEndpoints:             4,
			addedPerSync:                 0,
			removedPerSync:               0,
			numCreated:                   0,
			numUpdated:                   0,
			numDeleted:                   0,
			slicesChangedPerSyncTopology: 0,
		},
	}, {
		name:                 "topology enabled, hintsAnnotation==Auto, more slices and endpoints",
		topologyCacheEnabled: true,
		hintsAnnotation:      "Auto",
		existingSlices:       []*discovery.EndpointSlice{slicesByName["zone-a-c"], slicesByName["zone-a-b"]},
		pods:                 append(slicePods["zone-a-c"], slicePods["zone-a-b"]...),
		nodes:                nodes,
		expectedHints: map[string]int{
			"zone-a": 3,
			"zone-b": 3,
			"zone-c": 3,
		},
		expectedCrossZoneHints: 1,
		expectedMetrics: expectedMetrics{
			desiredSlices:    1,
			actualSlices:     2,
			desiredEndpoints: 9,
			addedPerSync:     0,
			removedPerSync:   0,
			numCreated:       0,
			// TODO(robscott): Since we're potentially changing more slices when
			// adding topology hints we could use it as a free repacking
			// opportunity. That would make this value 1.
			numUpdated:                   2,
			numDeleted:                   0,
			slicesChangedPerSyncTopology: 2,
		},
	}, {
		name:                   "topology enabled, hintsAnnotation==disabled, more slices and endpoints",
		topologyCacheEnabled:   true,
		hintsAnnotation:        "disabled",
		existingSlices:         []*discovery.EndpointSlice{slicesByName["zone-a-c"], slicesByName["zone-a-b"]},
		pods:                   append(slicePods["zone-a-c"], slicePods["zone-a-b"]...),
		nodes:                  nodes,
		expectedHints:          nil,
		expectedCrossZoneHints: 0,
		expectedMetrics: expectedMetrics{
			desiredSlices:        1,
			actualSlices:         2,
			desiredEndpoints:     9,
			addedPerSync:         0,
			removedPerSync:       0,
			numCreated:           0,
			numUpdated:           0,
			numDeleted:           0,
			slicesChangedPerSync: 0,
		},
	}}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			client := newClientset()
			cmc := newCacheMutationCheck(tc.existingSlices)
			createEndpointSlices(t, client, ns, tc.existingSlices)
			logger, _ := ktesting.NewTestContext(t)

			setupMetrics()
			r := newReconciler(client, tc.nodes, defaultMaxEndpointsPerSlice)
			if tc.topologyCacheEnabled {
				r.topologyCache = topologycache.NewTopologyCache()
				r.topologyCache.SetNodes(logger, tc.nodes)
			}

			service := svc.DeepCopy()
			service.Annotations = map[string]string{
				corev1.DeprecatedAnnotationTopologyAwareHints: tc.hintsAnnotation,
			}
			r.Reconcile(logger, service, tc.pods, tc.existingSlices, time.Now())

			cmc.Check(t)
			expectMetrics(t, tc.expectedMetrics)
			fetchedSlices := fetchEndpointSlices(t, client, ns)
			if len(fetchedSlices) != tc.expectedMetrics.actualSlices {
				t.Fatalf("Actual slices %d doesn't match metric %d", len(fetchedSlices), tc.expectedMetrics.actualSlices)
			}

			if tc.expectedHints == nil {
				for _, slice := range fetchedSlices {
					for _, endpoint := range slice.Endpoints {
						if endpoint.Hints != nil && len(endpoint.Hints.ForZones) > 0 {
							t.Fatalf("Expected endpoint not to have zone hints: %+v", endpoint)
						}
					}
				}
				return
			}

			actualCrossZoneHints := 0
			actualHints := map[string]int{}

			for _, slice := range fetchedSlices {
				for _, endpoint := range slice.Endpoints {
					if endpoint.Hints == nil || len(endpoint.Hints.ForZones) == 0 {
						t.Fatalf("Expected endpoint to have zone hints: %+v", endpoint)
					}
					if len(endpoint.Hints.ForZones) > 1 {
						t.Fatalf("Expected endpoint to only have 1 zone hint, got %d", len(endpoint.Hints.ForZones))
					}

					if endpoint.Zone == nil || *endpoint.Zone == "" {
						t.Fatalf("Expected endpoint to have zone: %+v", endpoint)
					}
					zoneHint := endpoint.Hints.ForZones[0].Name
					if *endpoint.Zone != zoneHint {
						actualCrossZoneHints++
					}
					actualHints[zoneHint]++
				}
			}

			if len(actualHints) != len(tc.expectedHints) {
				t.Errorf("Expected hints for %d zones, got %d", len(tc.expectedHints), len(actualHints))
			}

			for zone, expectedNum := range tc.expectedHints {
				actualNum, _ := actualHints[zone]
				if actualNum != expectedNum {
					t.Errorf("Expected %d hints for %s zone, got %d", expectedNum, zone, actualNum)
				}
			}

			if actualCrossZoneHints != tc.expectedCrossZoneHints {
				t.Errorf("Expected %d cross zone hints, got %d", tc.expectedCrossZoneHints, actualCrossZoneHints)
			}
		})
	}
}

// Test reconciliation behaviour for trafficDistribution field.
func TestReconcile_TrafficDistribution(t *testing.T) {
	// Setup the following topology for the test.
	//
	// 	- node-0 IN zone-a CONTAINS {pod-0}
	// 	- node-1 IN zone-b CONTAINS {pod-1, pod-2, pod-3}
	// 	- node-2 IN zone-c CONTAINS {pod-4, pod-5}
	ns := "ns1"
	svc, _ := newServiceAndEndpointMeta("foo", ns)
	nodes := []*corev1.Node{}
	pods := []*corev1.Pod{}
	for i := 0; i < 3; i++ {
		nodes = append(nodes, &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: fmt.Sprintf("node-%v", i),
				Labels: map[string]string{
					corev1.LabelTopologyZone: fmt.Sprintf("zone-%c", 'a'+i),
				},
			},
			Status: corev1.NodeStatus{
				Conditions: []corev1.NodeCondition{{
					Type:   corev1.NodeReady,
					Status: corev1.ConditionTrue,
				}},
				Allocatable: corev1.ResourceList{"cpu": resource.MustParse("100m")},
			},
		})
	}
	for i := 0; i < 6; i++ {
		pods = append(pods, newPod(i, ns, true, 1, false))
	}
	pods[0].Spec.NodeName = nodes[0].Name
	pods[1].Spec.NodeName = nodes[1].Name
	pods[2].Spec.NodeName = nodes[1].Name
	pods[3].Spec.NodeName = nodes[1].Name
	pods[4].Spec.NodeName = nodes[2].Name
	pods[5].Spec.NodeName = nodes[2].Name

	// Define test cases.

	testCases := []struct {
		name string
		desc string

		trafficDistributionFeatureGateEnabled bool
		trafficDistribution                   string
		topologyAnnotation                    string

		// Defines how many hints belong to a particular zone.
		wantHintsDistributionByZone map[string]int
		// Number of endpoints where the zone hints are different from the zone of
		// the endpoint itself.
		wantEndpointsWithCrossZoneHints int
		wantMetrics                     expectedMetrics
	}{
		{
			name:                                  "trafficDistribution=PreferClose, topologyAnnotation=Disabled",
			desc:                                  "When trafficDistribution is enabled and topologyAnnotation is disabled, hints should be distributed as per the trafficDistribution field",
			trafficDistributionFeatureGateEnabled: true,
			trafficDistribution:                   corev1.ServiceTrafficDistributionPreferClose,
			topologyAnnotation:                    "Disabled",
			wantHintsDistributionByZone: map[string]int{
				"zone-a": 1, // {pod-0}
				"zone-b": 3, // {pod-1, pod-2, pod-3}
				"zone-c": 2, // {pod-4, pod-5}
			},
			wantMetrics: expectedMetrics{
				desiredSlices:                   1,
				actualSlices:                    1,
				desiredEndpoints:                6,
				addedPerSync:                    6,
				removedPerSync:                  0,
				numCreated:                      1,
				numUpdated:                      0,
				numDeleted:                      0,
				slicesChangedPerSync:            0, // 0 means either topologyAnnotation or trafficDistribution was used.
				slicesChangedPerSyncTopology:    0, // 0 means topologyAnnotation was not used.
				slicesChangedPerSyncTrafficDist: 1, // 1 EPS configured using trafficDistribution.
				servicesCountByTrafficDistribution: map[string]int{
					"PreferClose": 1,
				},
			},
		},
		{
			name:                                  "feature gate disabled; trafficDistribution=PreferClose, topologyAnnotation=Disabled",
			desc:                                  "When feature gate is disabled, trafficDistribution should be ignored",
			trafficDistributionFeatureGateEnabled: false,
			trafficDistribution:                   corev1.ServiceTrafficDistributionPreferClose,
			topologyAnnotation:                    "Disabled",
			wantHintsDistributionByZone:           map[string]int{"": 6}, // Equivalent to no hints.
			wantMetrics: expectedMetrics{
				desiredSlices:                   1,
				actualSlices:                    1,
				desiredEndpoints:                6,
				addedPerSync:                    6,
				removedPerSync:                  0,
				numCreated:                      1,
				numUpdated:                      0,
				numDeleted:                      0,
				slicesChangedPerSync:            1, // 1 means both topologyAnnotation and trafficDistribution were not used.
				slicesChangedPerSyncTopology:    0, // 0 means topologyAnnotation was not used.
				slicesChangedPerSyncTrafficDist: 0, // 0 means trafficDistribution was not used.
			},
		},
		{
			name:                                  "trafficDistribution=PreferClose, topologyAnnotation=Auto",
			desc:                                  "When trafficDistribution and topologyAnnotation are both enabled, precedence should be given to topologyAnnotation",
			trafficDistributionFeatureGateEnabled: true,
			trafficDistribution:                   corev1.ServiceTrafficDistributionPreferClose,
			topologyAnnotation:                    "Auto",
			wantHintsDistributionByZone: map[string]int{
				"zone-a": 2, // {pod-0, pod-3} (pod-3 is just an example, it could have also been either of the other two)
				"zone-b": 2, // {pod-1, pod-2}
				"zone-c": 2, // {pod-4, pod-5}
			},
			wantEndpointsWithCrossZoneHints: 1, // since a pod from zone-b is likely assigned a hint for zone-a
			wantMetrics: expectedMetrics{
				desiredSlices:                   1,
				actualSlices:                    1,
				desiredEndpoints:                6,
				addedPerSync:                    6,
				removedPerSync:                  0,
				numCreated:                      1,
				numUpdated:                      0,
				numDeleted:                      0,
				slicesChangedPerSync:            0, // 0 means either topologyAnnotation or trafficDistribution was used.
				slicesChangedPerSyncTopology:    1, // 1 EPS configured using topologyAnnotation.
				slicesChangedPerSyncTrafficDist: 0, // 0 means trafficDistribution was not used.
			},
		},
		{
			name:                                  "trafficDistribution=<empty>, topologyAnnotation=<empty>",
			desc:                                  "When trafficDistribution and topologyAnnotation are both disabled, no hints should be added, but the servicesCountByTrafficDistribution metric should reflect this",
			trafficDistributionFeatureGateEnabled: true,
			trafficDistribution:                   "",
			topologyAnnotation:                    "",
			wantHintsDistributionByZone:           map[string]int{"": 6}, // Equivalent to no hints.
			wantMetrics: expectedMetrics{
				desiredSlices:                   1,
				actualSlices:                    1,
				desiredEndpoints:                6,
				addedPerSync:                    6,
				removedPerSync:                  0,
				numCreated:                      1,
				numUpdated:                      0,
				numDeleted:                      0,
				slicesChangedPerSync:            1, // 1 means both topologyAnnotation and trafficDistribution were not used.
				slicesChangedPerSyncTopology:    0, // 0 means topologyAnnotation was not used.
				slicesChangedPerSyncTrafficDist: 0, // 0 means trafficDistribution was not used.
				servicesCountByTrafficDistribution: map[string]int{
					"ImplementationSpecific": 1,
				},
			},
		},
	}

	// Make assertions.

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			client := newClientset()
			logger, _ := ktesting.NewTestContext(t)
			setupMetrics()

			r := newReconciler(client, nodes, defaultMaxEndpointsPerSlice)
			r.trafficDistributionEnabled = tc.trafficDistributionFeatureGateEnabled
			r.topologyCache = topologycache.NewTopologyCache()
			r.topologyCache.SetNodes(logger, nodes)

			service := svc.DeepCopy()
			service.Spec.TrafficDistribution = &tc.trafficDistribution
			service.Annotations = map[string]string{
				corev1.DeprecatedAnnotationTopologyAwareHints: tc.topologyAnnotation,
			}

			err := r.Reconcile(logger, service, pods, nil, time.Now())

			if err != nil {
				t.Errorf("Reconcile(...): return error = %v; want no error", err)
			}

			fetchedSlices := fetchEndpointSlices(t, client, ns)
			gotHintsDistributionByZone := make(map[string]int)
			gotEndpointsWithCrossZoneHints := 0
			for _, slice := range fetchedSlices {
				for _, endpoint := range slice.Endpoints {
					var zoneHint string
					if endpoint.Hints != nil && len(endpoint.Hints.ForZones) == 1 {
						zoneHint = endpoint.Hints.ForZones[0].Name
					}
					gotHintsDistributionByZone[zoneHint]++
					if zoneHint != "" && *endpoint.Zone != zoneHint {
						gotEndpointsWithCrossZoneHints++
					}
				}
			}

			if diff := cmp.Diff(tc.wantHintsDistributionByZone, gotHintsDistributionByZone); diff != "" {
				t.Errorf("Reconcile(...): Incorrect distribution of endpoints among zones; (-want, +got)\n%v", diff)
			}
			if gotEndpointsWithCrossZoneHints != tc.wantEndpointsWithCrossZoneHints {
				t.Errorf("Reconcile(...): EndpointSlices have endpoints with incorrect number of cross-zone hints; gotEndpointsWithCrossZoneHints=%v, want=%v", gotEndpointsWithCrossZoneHints, tc.wantEndpointsWithCrossZoneHints)
			}

			expectMetrics(t, tc.wantMetrics)
		})
	}
}

// Test Helpers

func newReconciler(client *fake.Clientset, nodes []*corev1.Node, maxEndpointsPerSlice int32) *Reconciler {
	eventRecorder := record.NewFakeRecorder(10)
	informerFactory := informers.NewSharedInformerFactory(client, 0)
	nodeInformer := informerFactory.Core().V1().Nodes()
	indexer := nodeInformer.Informer().GetIndexer()
	for _, node := range nodes {
		indexer.Add(node)
	}

	return NewReconciler(
		client,
		corelisters.NewNodeLister(indexer),
		maxEndpointsPerSlice,
		endpointsliceutil.NewEndpointSliceTracker(),
		nil,
		eventRecorder,
		controllerName,
	)
}

// ensures endpoint slices exist with the desired set of lengths
func expectUnorderedSlicesWithLengths(t *testing.T, endpointSlices []discovery.EndpointSlice, expectedLengths []int) {
	assert.Len(t, endpointSlices, len(expectedLengths), "Expected %d endpoint slices", len(expectedLengths))

	lengthsWithNoMatch := []int{}
	desiredLengths := expectedLengths
	actualLengths := []int{}
	for _, endpointSlice := range endpointSlices {
		actualLen := len(endpointSlice.Endpoints)
		actualLengths = append(actualLengths, actualLen)
		matchFound := false
		for i := 0; i < len(desiredLengths); i++ {
			if desiredLengths[i] == actualLen {
				matchFound = true
				desiredLengths = append(desiredLengths[:i], desiredLengths[i+1:]...)
				break
			}
		}

		if !matchFound {
			lengthsWithNoMatch = append(lengthsWithNoMatch, actualLen)
		}
	}

	if len(lengthsWithNoMatch) > 0 || len(desiredLengths) > 0 {
		t.Errorf("Actual slice lengths (%v) don't match expected (%v)", actualLengths, expectedLengths)
	}
}

// ensures endpoint slices exist with the desired set of ports and address types
func expectUnorderedSlicesWithTopLevelAttrs(t *testing.T, endpointSlices []discovery.EndpointSlice, expectedSlices []discovery.EndpointSlice) {
	t.Helper()
	assert.Len(t, endpointSlices, len(expectedSlices), "Expected %d endpoint slices", len(expectedSlices))

	slicesWithNoMatch := []discovery.EndpointSlice{}
	for _, endpointSlice := range endpointSlices {
		matchFound := false
		for i := 0; i < len(expectedSlices); i++ {
			if portsAndAddressTypeEqual(expectedSlices[i], endpointSlice) {
				matchFound = true
				expectedSlices = append(expectedSlices[:i], expectedSlices[i+1:]...)
				break
			}
		}

		if !matchFound {
			slicesWithNoMatch = append(slicesWithNoMatch, endpointSlice)
		}
	}

	assert.Empty(t, slicesWithNoMatch, "EndpointSlice(s) found without matching attributes")
	assert.Empty(t, expectedSlices, "Expected slices(s) not found in EndpointSlices")
}

func expectActions(t *testing.T, actions []k8stesting.Action, num int, verb, resource string) {
	t.Helper()
	// if actions are less the below logic will panic
	if num > len(actions) {
		t.Fatalf("len of actions %v is unexpected. Expected to be at least %v", len(actions), num+1)
	}

	for i := 0; i < num; i++ {
		relativePos := len(actions) - i - 1
		assert.Equal(t, verb, actions[relativePos].GetVerb(), "Expected action -%d verb to be %s", i, verb)
		assert.Equal(t, resource, actions[relativePos].GetResource().Resource, "Expected action -%d resource to be %s", i, resource)
	}
}

func expectTrackedGeneration(t *testing.T, tracker *endpointsliceutil.EndpointSliceTracker, slice *discovery.EndpointSlice, expectedGeneration int64) {
	gfs, ok := tracker.GenerationsForSliceUnsafe(slice)
	if !ok {
		t.Fatalf("Expected Service to be tracked for EndpointSlices %s", slice.Name)
	}
	generation, ok := gfs[slice.UID]
	if !ok {
		t.Fatalf("Expected EndpointSlice %s to be tracked", slice.Name)
	}
	if generation != expectedGeneration {
		t.Errorf("Expected Generation of %s to be %d, got %d", slice.Name, expectedGeneration, generation)
	}
}

func portsAndAddressTypeEqual(slice1, slice2 discovery.EndpointSlice) bool {
	return apiequality.Semantic.DeepEqual(slice1.Ports, slice2.Ports) && apiequality.Semantic.DeepEqual(slice1.AddressType, slice2.AddressType)
}

func createEndpointSlices(t *testing.T, client *fake.Clientset, namespace string, endpointSlices []*discovery.EndpointSlice) {
	t.Helper()
	for _, endpointSlice := range endpointSlices {
		_, err := client.DiscoveryV1().EndpointSlices(namespace).Create(context.TODO(), endpointSlice, metav1.CreateOptions{})
		if err != nil {
			t.Fatalf("Expected no error creating Endpoint Slice, got: %v", err)
		}
	}
}

func fetchEndpointSlices(t *testing.T, client *fake.Clientset, namespace string) []discovery.EndpointSlice {
	t.Helper()
	fetchedSlices, err := client.DiscoveryV1().EndpointSlices(namespace).List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("Expected no error fetching Endpoint Slices, got: %v", err)
		return []discovery.EndpointSlice{}
	}
	return fetchedSlices.Items
}

func reconcileHelper(t *testing.T, r *Reconciler, service *corev1.Service, pods []*corev1.Pod, existingSlices []*discovery.EndpointSlice, triggerTime time.Time) {
	logger, _ := ktesting.NewTestContext(t)
	t.Helper()
	err := r.Reconcile(logger, service, pods, existingSlices, triggerTime)
	if err != nil {
		t.Fatalf("Expected no error reconciling Endpoint Slices, got: %v", err)
	}
}

// Metrics helpers

type expectedMetrics struct {
	desiredSlices                      int
	actualSlices                       int
	desiredEndpoints                   int
	addedPerSync                       int
	removedPerSync                     int
	numCreated                         int
	numUpdated                         int
	numDeleted                         int
	slicesChangedPerSync               int
	slicesChangedPerSyncTopology       int
	slicesChangedPerSyncTrafficDist    int
	syncSuccesses                      int
	syncErrors                         int
	servicesCountByTrafficDistribution map[string]int
}

func expectMetrics(t *testing.T, em expectedMetrics) {
	t.Helper()

	actualDesiredSlices, err := testutil.GetGaugeMetricValue(metrics.DesiredEndpointSlices.WithLabelValues())
	handleErr(t, err, "desiredEndpointSlices")
	if actualDesiredSlices != float64(em.desiredSlices) {
		t.Errorf("Expected desiredEndpointSlices to be %d, got %v", em.desiredSlices, actualDesiredSlices)
	}

	actualNumSlices, err := testutil.GetGaugeMetricValue(metrics.NumEndpointSlices.WithLabelValues())
	handleErr(t, err, "numEndpointSlices")
	if actualNumSlices != float64(em.actualSlices) {
		t.Errorf("Expected numEndpointSlices to be %d, got %v", em.actualSlices, actualNumSlices)
	}

	actualEndpointsDesired, err := testutil.GetGaugeMetricValue(metrics.EndpointsDesired.WithLabelValues())
	handleErr(t, err, "desiredEndpoints")
	if actualEndpointsDesired != float64(em.desiredEndpoints) {
		t.Errorf("Expected desiredEndpoints to be %d, got %v", em.desiredEndpoints, actualEndpointsDesired)
	}

	actualAddedPerSync, err := testutil.GetHistogramMetricValue(metrics.EndpointsAddedPerSync.WithLabelValues())
	handleErr(t, err, "endpointsAddedPerSync")
	if actualAddedPerSync != float64(em.addedPerSync) {
		t.Errorf("Expected endpointsAddedPerSync to be %d, got %v", em.addedPerSync, actualAddedPerSync)
	}

	actualRemovedPerSync, err := testutil.GetHistogramMetricValue(metrics.EndpointsRemovedPerSync.WithLabelValues())
	handleErr(t, err, "endpointsRemovedPerSync")
	if actualRemovedPerSync != float64(em.removedPerSync) {
		t.Errorf("Expected endpointsRemovedPerSync to be %d, got %v", em.removedPerSync, actualRemovedPerSync)
	}

	actualCreated, err := testutil.GetCounterMetricValue(metrics.EndpointSliceChanges.WithLabelValues("create"))
	handleErr(t, err, "endpointSliceChangesCreated")
	if actualCreated != float64(em.numCreated) {
		t.Errorf("Expected endpointSliceChangesCreated to be %d, got %v", em.numCreated, actualCreated)
	}

	actualUpdated, err := testutil.GetCounterMetricValue(metrics.EndpointSliceChanges.WithLabelValues("update"))
	handleErr(t, err, "endpointSliceChangesUpdated")
	if actualUpdated != float64(em.numUpdated) {
		t.Errorf("Expected endpointSliceChangesUpdated to be %d, got %v", em.numUpdated, actualUpdated)
	}

	actualDeleted, err := testutil.GetCounterMetricValue(metrics.EndpointSliceChanges.WithLabelValues("delete"))
	handleErr(t, err, "endpointSliceChangesDeleted")
	if actualDeleted != float64(em.numDeleted) {
		t.Errorf("Expected endpointSliceChangesDeleted to be %d, got %v", em.numDeleted, actualDeleted)
	}

	actualSlicesChangedPerSync, err := testutil.GetHistogramMetricValue(metrics.EndpointSlicesChangedPerSync.WithLabelValues("Disabled", ""))
	handleErr(t, err, "slicesChangedPerSync")
	if actualSlicesChangedPerSync != float64(em.slicesChangedPerSync) {
		t.Errorf("Expected slicesChangedPerSync to be %d, got %v", em.slicesChangedPerSync, actualSlicesChangedPerSync)
	}

	actualSlicesChangedPerSyncTopology, err := testutil.GetHistogramMetricValue(metrics.EndpointSlicesChangedPerSync.WithLabelValues("Auto", ""))
	handleErr(t, err, "slicesChangedPerSyncTopology")
	if actualSlicesChangedPerSyncTopology != float64(em.slicesChangedPerSyncTopology) {
		t.Errorf("Expected slicesChangedPerSyncTopology to be %d, got %v", em.slicesChangedPerSyncTopology, actualSlicesChangedPerSyncTopology)
	}

	actualSlicesChangedPerSyncTrafficDist, err := testutil.GetHistogramMetricValue(metrics.EndpointSlicesChangedPerSync.WithLabelValues("Disabled", "PreferClose"))
	handleErr(t, err, "slicesChangedPerSyncTrafficDist")
	if actualSlicesChangedPerSyncTrafficDist != float64(em.slicesChangedPerSyncTrafficDist) {
		t.Errorf("Expected slicesChangedPerSyncTrafficDist to be %d, got %v", em.slicesChangedPerSyncTrafficDist, actualSlicesChangedPerSyncTopology)
	}

	actualSyncSuccesses, err := testutil.GetCounterMetricValue(metrics.EndpointSliceSyncs.WithLabelValues("success"))
	handleErr(t, err, "syncSuccesses")
	if actualSyncSuccesses != float64(em.syncSuccesses) {
		t.Errorf("Expected endpointSliceSyncSuccesses to be %d, got %v", em.syncSuccesses, actualSyncSuccesses)
	}

	actualSyncErrors, err := testutil.GetCounterMetricValue(metrics.EndpointSliceSyncs.WithLabelValues("error"))
	handleErr(t, err, "syncErrors")
	if actualSyncErrors != float64(em.syncErrors) {
		t.Errorf("Expected endpointSliceSyncErrors to be %d, got %v", em.syncErrors, actualSyncErrors)
	}

	for _, trafficDistribution := range []string{"PreferClose", "ImplementationSpecific"} {
		gotServicesCount, err := testutil.GetGaugeMetricValue(metrics.ServicesCountByTrafficDistribution.WithLabelValues(trafficDistribution))
		var wantServicesCount int
		if em.servicesCountByTrafficDistribution != nil {
			wantServicesCount = em.servicesCountByTrafficDistribution[trafficDistribution]
		}
		handleErr(t, err, fmt.Sprintf("%v[traffic_distribution=%v]", "services_count_by_traffic_distribution", trafficDistribution))
		if int(gotServicesCount) != wantServicesCount {
			t.Errorf("Expected servicesCountByTrafficDistribution for traffic_distribution=%v to be %v, got %v", trafficDistribution, wantServicesCount, gotServicesCount)
		}
	}
}

func handleErr(t *testing.T, err error, metricName string) {
	if err != nil {
		t.Errorf("Failed to get %s value, err: %v", metricName, err)
	}
}

func setupMetrics() {
	metrics.RegisterMetrics()
	metrics.NumEndpointSlices.Reset()
	metrics.DesiredEndpointSlices.Reset()
	metrics.EndpointsDesired.Reset()
	metrics.EndpointsAddedPerSync.Reset()
	metrics.EndpointsRemovedPerSync.Reset()
	metrics.EndpointSliceChanges.Reset()
	metrics.EndpointSlicesChangedPerSync.Reset()
	metrics.EndpointSliceSyncs.Reset()
	metrics.ServicesCountByTrafficDistribution.Reset()
}
