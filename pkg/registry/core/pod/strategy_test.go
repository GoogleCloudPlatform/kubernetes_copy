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

package pod

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/stretchr/testify/assert"
	apiv1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/apiserver/pkg/warning"
	"k8s.io/client-go/tools/cache"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	ptr "k8s.io/utils/ptr"

	podtest "k8s.io/kubernetes/pkg/api/pod/testing"
	apitesting "k8s.io/kubernetes/pkg/api/testing"
	api "k8s.io/kubernetes/pkg/apis/core"
	"k8s.io/kubernetes/pkg/features"
	"k8s.io/kubernetes/pkg/kubelet/client"

	// ensure types are installed
	_ "k8s.io/kubernetes/pkg/apis/core/install"
)

func TestMatchPod(t *testing.T) {
	testCases := []struct {
		in            *api.Pod
		fieldSelector fields.Selector
		expectMatch   bool
	}{
		{
			in: &api.Pod{
				Spec: api.PodSpec{NodeName: "nodeA"},
			},
			fieldSelector: fields.ParseSelectorOrDie("spec.nodeName=nodeA"),
			expectMatch:   true,
		},
		{
			in: &api.Pod{
				Spec: api.PodSpec{NodeName: "nodeB"},
			},
			fieldSelector: fields.ParseSelectorOrDie("spec.nodeName=nodeA"),
			expectMatch:   false,
		},
		{
			in: &api.Pod{
				Spec: api.PodSpec{RestartPolicy: api.RestartPolicyAlways},
			},
			fieldSelector: fields.ParseSelectorOrDie("spec.restartPolicy=Always"),
			expectMatch:   true,
		},
		{
			in: &api.Pod{
				Spec: api.PodSpec{RestartPolicy: api.RestartPolicyAlways},
			},
			fieldSelector: fields.ParseSelectorOrDie("spec.restartPolicy=Never"),
			expectMatch:   false,
		},
		{
			in: &api.Pod{
				Spec: api.PodSpec{SchedulerName: "scheduler1"},
			},
			fieldSelector: fields.ParseSelectorOrDie("spec.schedulerName=scheduler1"),
			expectMatch:   true,
		},
		{
			in: &api.Pod{
				Spec: api.PodSpec{SchedulerName: "scheduler1"},
			},
			fieldSelector: fields.ParseSelectorOrDie("spec.schedulerName=scheduler2"),
			expectMatch:   false,
		},
		{
			in: &api.Pod{
				Spec: api.PodSpec{ServiceAccountName: "serviceAccount1"},
			},
			fieldSelector: fields.ParseSelectorOrDie("spec.serviceAccountName=serviceAccount1"),
			expectMatch:   true,
		},
		{
			in: &api.Pod{
				Spec: api.PodSpec{SchedulerName: "serviceAccount1"},
			},
			fieldSelector: fields.ParseSelectorOrDie("spec.serviceAccountName=serviceAccount2"),
			expectMatch:   false,
		},
		{
			in: &api.Pod{
				Status: api.PodStatus{Phase: api.PodRunning},
			},
			fieldSelector: fields.ParseSelectorOrDie("status.phase=Running"),
			expectMatch:   true,
		},
		{
			in: &api.Pod{
				Status: api.PodStatus{Phase: api.PodRunning},
			},
			fieldSelector: fields.ParseSelectorOrDie("status.phase=Pending"),
			expectMatch:   false,
		},
		{
			in: &api.Pod{
				Status: api.PodStatus{
					PodIPs: []api.PodIP{
						{IP: "1.2.3.4"},
					},
				},
			},
			fieldSelector: fields.ParseSelectorOrDie("status.podIP=1.2.3.4"),
			expectMatch:   true,
		},
		{
			in: &api.Pod{
				Status: api.PodStatus{
					PodIPs: []api.PodIP{
						{IP: "1.2.3.4"},
					},
				},
			},
			fieldSelector: fields.ParseSelectorOrDie("status.podIP=4.3.2.1"),
			expectMatch:   false,
		},
		{
			in: &api.Pod{
				Status: api.PodStatus{NominatedNodeName: "node1"},
			},
			fieldSelector: fields.ParseSelectorOrDie("status.nominatedNodeName=node1"),
			expectMatch:   true,
		},
		{
			in: &api.Pod{
				Status: api.PodStatus{NominatedNodeName: "node1"},
			},
			fieldSelector: fields.ParseSelectorOrDie("status.nominatedNodeName=node2"),
			expectMatch:   false,
		},
		{
			in: &api.Pod{
				Status: api.PodStatus{
					PodIPs: []api.PodIP{
						{IP: "2001:db8::"},
					},
				},
			},
			fieldSelector: fields.ParseSelectorOrDie("status.podIP=2001:db8::"),
			expectMatch:   true,
		},
		{
			in: &api.Pod{
				Status: api.PodStatus{
					PodIPs: []api.PodIP{
						{IP: "2001:db8::"},
					},
				},
			},
			fieldSelector: fields.ParseSelectorOrDie("status.podIP=2001:db7::"),
			expectMatch:   false,
		},
		{
			in: &api.Pod{
				Spec: api.PodSpec{
					SecurityContext: &api.PodSecurityContext{
						HostNetwork: true,
					},
				},
			},
			fieldSelector: fields.ParseSelectorOrDie("spec.hostNetwork=true"),
			expectMatch:   true,
		},
		{
			in: &api.Pod{
				Spec: api.PodSpec{
					SecurityContext: &api.PodSecurityContext{
						HostNetwork: true,
					},
				},
			},
			fieldSelector: fields.ParseSelectorOrDie("spec.hostNetwork=false"),
			expectMatch:   false,
		},
		{
			in: &api.Pod{
				Spec: api.PodSpec{
					SecurityContext: &api.PodSecurityContext{
						HostNetwork: false,
					},
				},
			},
			fieldSelector: fields.ParseSelectorOrDie("spec.hostNetwork=false"),
			expectMatch:   true,
		},
		{
			in: &api.Pod{
				Spec: api.PodSpec{},
			},
			fieldSelector: fields.ParseSelectorOrDie("spec.hostNetwork=false"),
			expectMatch:   true,
		},
		{
			in: &api.Pod{
				Spec: api.PodSpec{},
			},
			fieldSelector: fields.ParseSelectorOrDie("spec.hostNetwork=true"),
			expectMatch:   false,
		},
	}
	for _, testCase := range testCases {
		m := MatchPod(labels.Everything(), testCase.fieldSelector)
		result, err := m.Matches(testCase.in)
		if err != nil {
			t.Errorf("Unexpected error %v", err)
		}
		if result != testCase.expectMatch {
			t.Errorf("Result %v, Expected %v, Selector: %v, Pod: %v", result, testCase.expectMatch, testCase.fieldSelector.String(), testCase.in)
		}
	}
}

func getResourceList(cpu, memory string) api.ResourceList {
	res := api.ResourceList{}
	if cpu != "" {
		res[api.ResourceCPU] = resource.MustParse(cpu)
	}
	if memory != "" {
		res[api.ResourceMemory] = resource.MustParse(memory)
	}
	return res
}

func getResourceRequirements(requests, limits api.ResourceList) api.ResourceRequirements {
	res := api.ResourceRequirements{}
	res.Requests = requests
	res.Limits = limits
	return res
}

func newContainer(name string, requests api.ResourceList, limits api.ResourceList) api.Container {
	return api.Container{
		Name:      name,
		Resources: getResourceRequirements(requests, limits),
	}
}

func newPod(name string, containers []api.Container) *api.Pod {
	return &api.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: api.PodSpec{
			Containers: containers,
		},
	}
}

func TestGetPodQOS(t *testing.T) {
	testCases := []struct {
		pod      *api.Pod
		expected api.PodQOSClass
	}{
		{
			pod: newPod("guaranteed", []api.Container{
				newContainer("guaranteed", getResourceList("100m", "100Mi"), getResourceList("100m", "100Mi")),
			}),
			expected: api.PodQOSGuaranteed,
		},
		{
			pod: newPod("best-effort", []api.Container{
				newContainer("best-effort", getResourceList("", ""), getResourceList("", "")),
			}),
			expected: api.PodQOSBestEffort,
		},
		{
			pod: newPod("burstable", []api.Container{
				newContainer("burstable", getResourceList("100m", "100Mi"), getResourceList("", "")),
			}),
			expected: api.PodQOSBurstable,
		},
	}
	for id, testCase := range testCases {
		Strategy.PrepareForCreate(genericapirequest.NewContext(), testCase.pod)
		actual := testCase.pod.Status.QOSClass
		if actual != testCase.expected {
			t.Errorf("[%d]: invalid qos pod %s, expected: %s, actual: %s", id, testCase.pod.Name, testCase.expected, actual)
		}
	}
}

func TestSchedulingGatedCondition(t *testing.T) {
	tests := []struct {
		name string
		pod  *api.Pod
		want api.PodCondition
	}{
		{
			name: "pod without .spec.schedulingGates",
			pod:  &api.Pod{},
			want: api.PodCondition{},
		},
		{
			name: "pod with .spec.schedulingGates",
			pod: &api.Pod{
				Spec: api.PodSpec{
					SchedulingGates: []api.PodSchedulingGate{{Name: "foo"}},
				},
			},
			want: api.PodCondition{
				Type:    api.PodScheduled,
				Status:  api.ConditionFalse,
				Reason:  apiv1.PodReasonSchedulingGated,
				Message: "Scheduling is blocked due to non-empty scheduling gates",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			Strategy.PrepareForCreate(genericapirequest.NewContext(), tt.pod)
			var got api.PodCondition
			for _, condition := range tt.pod.Status.Conditions {
				if condition.Type == api.PodScheduled {
					got = condition
					break
				}
			}

			if got.LastTransitionTime.IsZero() && got.Type != "" {
				t.Errorf("unexpected empty LastTransitionTime in condition")
			}
			if diff := cmp.Diff(tt.want, got, cmpopts.IgnoreFields(api.PodCondition{}, "LastTransitionTime")); diff != "" {
				t.Errorf("unexpected field errors (-want, +got):\n%s", diff)
			}
		})
	}
}

func TestCheckGracefulDelete(t *testing.T) {
	defaultGracePeriod := int64(30)
	tcs := []struct {
		name              string
		pod               *api.Pod
		deleteGracePeriod *int64
		gracePeriod       int64
	}{
		{
			name: "in pending phase with has node name",
			pod: &api.Pod{
				Spec:   api.PodSpec{NodeName: "something"},
				Status: api.PodStatus{Phase: api.PodPending},
			},
			deleteGracePeriod: &defaultGracePeriod,
			gracePeriod:       defaultGracePeriod,
		},
		{
			name: "in failed phase with has node name",
			pod: &api.Pod{
				Spec:   api.PodSpec{NodeName: "something"},
				Status: api.PodStatus{Phase: api.PodFailed},
			},
			deleteGracePeriod: &defaultGracePeriod,
			gracePeriod:       0,
		},
		{
			name: "in failed phase",
			pod: &api.Pod{
				Spec:   api.PodSpec{},
				Status: api.PodStatus{Phase: api.PodPending},
			},
			deleteGracePeriod: &defaultGracePeriod,
			gracePeriod:       0,
		},
		{
			name: "in succeeded phase",
			pod: &api.Pod{
				Spec:   api.PodSpec{},
				Status: api.PodStatus{Phase: api.PodSucceeded},
			},
			deleteGracePeriod: &defaultGracePeriod,
			gracePeriod:       0,
		},
		{
			name: "no phase",
			pod: &api.Pod{
				Spec:   api.PodSpec{},
				Status: api.PodStatus{},
			},
			deleteGracePeriod: &defaultGracePeriod,
			gracePeriod:       0,
		},
		{
			name: "has negative grace period",
			pod: &api.Pod{
				Spec: api.PodSpec{
					NodeName:                      "something",
					TerminationGracePeriodSeconds: ptr.To[int64](-1),
				},
				Status: api.PodStatus{},
			},
			gracePeriod: 1,
		},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			out := &metav1.DeleteOptions{}
			if tc.deleteGracePeriod != nil {
				out.GracePeriodSeconds = ptr.To[int64](*tc.deleteGracePeriod)
			}
			Strategy.CheckGracefulDelete(genericapirequest.NewContext(), tc.pod, out)
			if out.GracePeriodSeconds == nil {
				t.Errorf("out grace period was nil but supposed to be %v", tc.gracePeriod)
			}
			if *(out.GracePeriodSeconds) != tc.gracePeriod {
				t.Errorf("out grace period was %v but was expected to be %v", *out, tc.gracePeriod)
			}
		})
	}
}

type mockPodGetter struct {
	pod *api.Pod
}

func (g mockPodGetter) Get(context.Context, string, *metav1.GetOptions) (runtime.Object, error) {
	return g.pod, nil
}

func TestCheckLogLocation(t *testing.T) {
	ctx := genericapirequest.NewDefaultContext()
	fakePodName := "test"
	tcs := []struct {
		name              string
		in                *api.Pod
		opts              *api.PodLogOptions
		expectedErr       error
		expectedTransport http.RoundTripper
	}{
		{
			name: "simple",
			in: &api.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: fakePodName},
				Spec: api.PodSpec{
					Containers: []api.Container{
						{Name: "mycontainer"},
					},
					NodeName: "foo",
				},
				Status: api.PodStatus{},
			},
			opts:              &api.PodLogOptions{},
			expectedErr:       nil,
			expectedTransport: fakeSecureRoundTripper,
		},
		{
			name: "insecure",
			in: &api.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: fakePodName},
				Spec: api.PodSpec{
					Containers: []api.Container{
						{Name: "mycontainer"},
					},
					NodeName: "foo",
				},
				Status: api.PodStatus{},
			},
			opts: &api.PodLogOptions{
				InsecureSkipTLSVerifyBackend: true,
			},
			expectedErr:       nil,
			expectedTransport: fakeInsecureRoundTripper,
		},
		{
			name: "missing container",
			in: &api.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: fakePodName},
				Spec:       api.PodSpec{},
				Status:     api.PodStatus{},
			},
			opts:              &api.PodLogOptions{},
			expectedErr:       errors.NewBadRequest("a container name must be specified for pod test"),
			expectedTransport: nil,
		},
		{
			name: "choice of two containers",
			in: &api.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: fakePodName},
				Spec: api.PodSpec{
					Containers: []api.Container{
						{Name: "container1"},
						{Name: "container2"},
					},
				},
				Status: api.PodStatus{},
			},
			opts:              &api.PodLogOptions{},
			expectedErr:       errors.NewBadRequest("a container name must be specified for pod test, choose one of: [container1 container2]"),
			expectedTransport: nil,
		},
		{
			name: "initcontainers",
			in: &api.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: fakePodName},
				Spec: api.PodSpec{
					Containers: []api.Container{
						{Name: "container1"},
						{Name: "container2"},
					},
					InitContainers: []api.Container{
						{Name: "initcontainer1"},
					},
				},
				Status: api.PodStatus{},
			},
			opts:              &api.PodLogOptions{},
			expectedErr:       errors.NewBadRequest("a container name must be specified for pod test, choose one of: [initcontainer1 container1 container2]"),
			expectedTransport: nil,
		},
		{
			in: &api.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: fakePodName},
				Spec: api.PodSpec{
					Containers: []api.Container{
						{Name: "container1"},
						{Name: "container2"},
					},
					InitContainers: []api.Container{
						{Name: "initcontainer1"},
					},
					EphemeralContainers: []api.EphemeralContainer{
						{EphemeralContainerCommon: api.EphemeralContainerCommon{Name: "debugger"}},
					},
				},
				Status: api.PodStatus{},
			},
			opts:              &api.PodLogOptions{},
			expectedErr:       errors.NewBadRequest("a container name must be specified for pod test, choose one of: [initcontainer1 container1 container2 debugger]"),
			expectedTransport: nil,
		},
		{
			name: "bad container",
			in: &api.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: fakePodName},
				Spec: api.PodSpec{
					Containers: []api.Container{
						{Name: "container1"},
						{Name: "container2"},
					},
				},
				Status: api.PodStatus{},
			},
			opts: &api.PodLogOptions{
				Container: "unknown",
			},
			expectedErr:       errors.NewBadRequest("container unknown is not valid for pod test"),
			expectedTransport: nil,
		},
		{
			name: "good with two containers",
			in: &api.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: fakePodName},
				Spec: api.PodSpec{
					Containers: []api.Container{
						{Name: "container1"},
						{Name: "container2"},
					},
					NodeName: "foo",
				},
				Status: api.PodStatus{},
			},
			opts: &api.PodLogOptions{
				Container: "container2",
			},
			expectedErr:       nil,
			expectedTransport: fakeSecureRoundTripper,
		},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			getter := &mockPodGetter{tc.in}
			connectionGetter := &mockConnectionInfoGetter{&client.ConnectionInfo{
				Transport:                      fakeSecureRoundTripper,
				InsecureSkipTLSVerifyTransport: fakeInsecureRoundTripper,
			}}

			_, actualTransport, err := LogLocation(ctx, getter, connectionGetter, fakePodName, tc.opts)
			if !reflect.DeepEqual(err, tc.expectedErr) {
				t.Errorf("expected %q, got %q", tc.expectedErr, err)
			}
			if actualTransport != tc.expectedTransport {
				t.Errorf("expected %q, got %q", tc.expectedTransport, actualTransport)
			}
		})
	}
}

func TestSelectableFieldLabelConversions(t *testing.T) {
	apitesting.TestSelectableFieldLabelConversionsOfKind(t,
		"v1",
		"Pod",
		ToSelectableFields(&api.Pod{}),
		nil,
	)
}

type mockConnectionInfoGetter struct {
	info *client.ConnectionInfo
}

func (g mockConnectionInfoGetter) GetConnectionInfo(ctx context.Context, nodeName types.NodeName) (*client.ConnectionInfo, error) {
	return g.info, nil
}

func TestPortForwardLocation(t *testing.T) {
	ctx := genericapirequest.NewDefaultContext()
	tcs := []struct {
		in          *api.Pod
		info        *client.ConnectionInfo
		opts        *api.PodPortForwardOptions
		expectedErr error
		expectedURL *url.URL
	}{
		{
			in: &api.Pod{
				Spec: api.PodSpec{},
			},
			opts:        &api.PodPortForwardOptions{},
			expectedErr: errors.NewBadRequest("pod test does not have a host assigned"),
		},
		{
			in: &api.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "ns",
					Name:      "pod1",
				},
				Spec: api.PodSpec{
					NodeName: "node1",
				},
			},
			info:        &client.ConnectionInfo{},
			opts:        &api.PodPortForwardOptions{},
			expectedURL: &url.URL{Host: ":", Path: "/portForward/ns/pod1"},
		},
		{
			in: &api.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "ns",
					Name:      "pod1",
				},
				Spec: api.PodSpec{
					NodeName: "node1",
				},
			},
			info:        &client.ConnectionInfo{},
			opts:        &api.PodPortForwardOptions{Ports: []int32{80}},
			expectedURL: &url.URL{Host: ":", Path: "/portForward/ns/pod1", RawQuery: "port=80"},
		},
	}
	for _, tc := range tcs {
		getter := &mockPodGetter{tc.in}
		connectionGetter := &mockConnectionInfoGetter{tc.info}
		loc, _, err := PortForwardLocation(ctx, getter, connectionGetter, "test", tc.opts)
		if !reflect.DeepEqual(err, tc.expectedErr) {
			t.Errorf("expected %v, got %v", tc.expectedErr, err)
		}
		if !reflect.DeepEqual(loc, tc.expectedURL) {
			t.Errorf("expected %v, got %v", tc.expectedURL, loc)
		}
	}
}

func TestGetPodIP(t *testing.T) {
	testCases := []struct {
		name       string
		pod        *api.Pod
		expectedIP string
	}{
		{
			name:       "nil pod",
			pod:        nil,
			expectedIP: "",
		},
		{
			name: "no status object",
			pod: &api.Pod{
				ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "pod1"},
				Spec:       api.PodSpec{},
			},
			expectedIP: "",
		},
		{
			name: "no pod ips",
			pod: &api.Pod{
				ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "pod1"},
				Spec:       api.PodSpec{},
				Status:     api.PodStatus{},
			},
			expectedIP: "",
		},
		{
			name: "empty list",
			pod: &api.Pod{
				ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "pod1"},
				Spec:       api.PodSpec{},
				Status: api.PodStatus{
					PodIPs: []api.PodIP{},
				},
			},
			expectedIP: "",
		},
		{
			name: "1 ip",
			pod: &api.Pod{
				ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "pod1"},
				Spec:       api.PodSpec{},
				Status: api.PodStatus{
					PodIPs: []api.PodIP{
						{IP: "10.0.0.10"},
					},
				},
			},
			expectedIP: "10.0.0.10",
		},
		{
			name: "multiple ips",
			pod: &api.Pod{
				ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "pod1"},
				Spec:       api.PodSpec{},
				Status: api.PodStatus{
					PodIPs: []api.PodIP{
						{IP: "10.0.0.10"},
						{IP: "10.0.0.20"},
					},
				},
			},
			expectedIP: "10.0.0.10",
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			podIP := getPodIP(tc.pod)
			if podIP != tc.expectedIP {
				t.Errorf("expected pod ip:%v does not match actual %v", tc.expectedIP, podIP)
			}
		})
	}
}

type fakeTransport struct {
	val string
}

func (f fakeTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, nil
}

var (
	fakeSecureRoundTripper   = fakeTransport{val: "secure"}
	fakeInsecureRoundTripper = fakeTransport{val: "insecure"}
)

func TestPodIndexFunc(t *testing.T) {
	tcs := []struct {
		name          string
		indexFunc     cache.IndexFunc
		pod           interface{}
		expectedValue string
		expectedErr   error
	}{
		{
			name:      "node name index",
			indexFunc: NodeNameIndexFunc,
			pod: &api.Pod{
				Spec: api.PodSpec{
					NodeName: "test-pod",
				},
			},
			expectedValue: "test-pod",
			expectedErr:   nil,
		},
		{
			name:          "not a pod failed",
			indexFunc:     NodeNameIndexFunc,
			pod:           "not a pod object",
			expectedValue: "test-pod",
			expectedErr:   fmt.Errorf("not a pod"),
		},
	}

	for _, tc := range tcs {
		indexValues, err := tc.indexFunc(tc.pod)
		if !reflect.DeepEqual(err, tc.expectedErr) {
			t.Errorf("name %v, expected %v, got %v", tc.name, tc.expectedErr, err)
		}
		if err == nil && len(indexValues) != 1 && indexValues[0] != tc.expectedValue {
			t.Errorf("name %v, expected %v, got %v", tc.name, tc.expectedValue, indexValues)
		}

	}
}

func newPodWithHugePageValue(resourceName api.ResourceName, value resource.Quantity) *api.Pod {
	return podtest.MakePod("", podtest.SetObjectMeta(metav1.ObjectMeta{
		Namespace:       "default",
		Name:            "foo",
		ResourceVersion: "1",
	}),
		podtest.SetContainers(podtest.MakeContainer("foo",
			podtest.SetContainerResources(api.ResourceRequirements{
				Requests: api.ResourceList{
					api.ResourceCPU: resource.MustParse("10"),
					resourceName:    value,
				},
				Limits: api.ResourceList{
					api.ResourceCPU: resource.MustParse("10"),
					resourceName:    value,
				},
			}),
		)),
	)
}

func TestPodStrategyValidate(t *testing.T) {
	const containerName = "container"

	tests := []struct {
		name    string
		pod     *api.Pod
		wantErr bool
	}{
		{
			name:    "a new pod setting container with indivisible hugepages values",
			pod:     newPodWithHugePageValue(api.ResourceHugePagesPrefix+"1Mi", resource.MustParse("1.1Mi")),
			wantErr: true,
		},
		{
			name: "a new pod setting init-container with indivisible hugepages values",
			pod: podtest.MakePod("foo",
				podtest.SetInitContainers(podtest.MakeContainer(containerName, podtest.SetContainerResources(
					api.ResourceRequirements{
						Requests: api.ResourceList{
							api.ResourceName(api.ResourceHugePagesPrefix + "64Ki"): resource.MustParse("127Ki"),
						},
						Limits: api.ResourceList{
							api.ResourceName(api.ResourceHugePagesPrefix + "64Ki"): resource.MustParse("127Ki"),
						},
					}))),
			),
			wantErr: true,
		},
		{
			name: "a new pod setting init-container with indivisible hugepages values while container with divisible hugepages values",
			pod: podtest.MakePod("foo",
				podtest.SetInitContainers(podtest.MakeContainer(containerName, podtest.SetContainerResources(
					api.ResourceRequirements{
						Requests: api.ResourceList{
							api.ResourceName(api.ResourceHugePagesPrefix + "2Mi"): resource.MustParse("5.1Mi"),
						},
						Limits: api.ResourceList{
							api.ResourceName(api.ResourceHugePagesPrefix + "2Mi"): resource.MustParse("5.1Mi"),
						},
					}))),
				podtest.SetContainers(podtest.MakeContainer(containerName, podtest.SetContainerResources(
					api.ResourceRequirements{
						Requests: api.ResourceList{
							api.ResourceName(api.ResourceHugePagesPrefix + "1Gi"): resource.MustParse("2Gi"),
						},
						Limits: api.ResourceList{
							api.ResourceName(api.ResourceHugePagesPrefix + "1Gi"): resource.MustParse("2Gi"),
						},
					}))),
			),
			wantErr: true,
		},
		{
			name: "a new pod setting container with divisible hugepages values",
			pod: podtest.MakePod("foo",
				podtest.SetContainers(podtest.MakeContainer(containerName, podtest.SetContainerResources(
					api.ResourceRequirements{
						Requests: api.ResourceList{
							api.ResourceName(api.ResourceCPU):                     resource.MustParse("10"),
							api.ResourceName(api.ResourceHugePagesPrefix + "1Mi"): resource.MustParse("2Mi"),
						},
						Limits: api.ResourceList{
							api.ResourceName(api.ResourceCPU):                     resource.MustParse("10"),
							api.ResourceName(api.ResourceHugePagesPrefix + "1Mi"): resource.MustParse("2Mi"),
						},
					}))),
			),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			errs := Strategy.Validate(genericapirequest.NewContext(), tc.pod)
			if tc.wantErr && len(errs) == 0 {
				t.Errorf("expected errors but got none")
			}
			if !tc.wantErr && len(errs) != 0 {
				t.Errorf("unexpected errors: %v", errs.ToAggregate())
			}
		})
	}
}

func TestEphemeralContainerStrategyValidateUpdate(t *testing.T) {

	test := []struct {
		name   string
		newPod *api.Pod
		oldPod *api.Pod
	}{
		{
			name: "add ephemeral container to regular pod and expect success",
			oldPod: podtest.MakePod("test-pod",
				podtest.SetResourceVersion("1"),
			),
			newPod: podtest.MakePod("test-pod",
				podtest.SetResourceVersion("1"),
				podtest.SetEphemeralContainers(api.EphemeralContainer{
					EphemeralContainerCommon: api.EphemeralContainerCommon{
						Name:                     "debugger",
						Image:                    "image",
						ImagePullPolicy:          "IfNotPresent",
						TerminationMessagePolicy: "File",
					},
				}),
			),
		},
	}

	// expect no errors
	for _, tc := range test {
		t.Run(tc.name, func(t *testing.T) {
			if errs := EphemeralContainersStrategy.ValidateUpdate(genericapirequest.NewContext(), tc.newPod, tc.oldPod); len(errs) != 0 {
				t.Errorf("unexpected error:%v", errs)
			}
		})
	}

	test = []struct {
		name   string
		newPod *api.Pod
		oldPod *api.Pod
	}{
		{
			name: "add ephemeral container to static pod and expect failure",
			oldPod: podtest.MakePod("test-pod",
				podtest.SetAnnotations(map[string]string{api.MirrorPodAnnotationKey: "someVal"}),
				podtest.SetNodeName("example.com"),
			),
			newPod: podtest.MakePod("test-pod",
				podtest.SetAnnotations(map[string]string{api.MirrorPodAnnotationKey: "someVal"}),
				podtest.SetEphemeralContainers(api.EphemeralContainer{
					EphemeralContainerCommon: api.EphemeralContainerCommon{
						Name:                     "debugger",
						Image:                    "image",
						ImagePullPolicy:          "IfNotPresent",
						TerminationMessagePolicy: "File",
					},
				}),
				podtest.SetNodeName("example.com"),
			),
		},
		{
			name: "remove ephemeral container from regular pod and expect failure",
			newPod: podtest.MakePod("test-pod",
				podtest.SetResourceVersion("1"),
			),
			oldPod: podtest.MakePod("test-pod",
				podtest.SetResourceVersion("1"),
				podtest.SetEphemeralContainers(api.EphemeralContainer{
					EphemeralContainerCommon: api.EphemeralContainerCommon{
						Name:                     "debugger",
						Image:                    "image",
						ImagePullPolicy:          "IfNotPresent",
						TerminationMessagePolicy: "File",
					},
				}),
			),
		},
		{
			name: "change ephemeral container from regular pod and expect failure",
			newPod: podtest.MakePod("test-pod",
				podtest.SetResourceVersion("1"),
				podtest.SetEphemeralContainers(api.EphemeralContainer{
					EphemeralContainerCommon: api.EphemeralContainerCommon{
						Name:                     "debugger",
						Image:                    "image2",
						ImagePullPolicy:          "IfNotPresent",
						TerminationMessagePolicy: "File",
					},
				}),
			),
			oldPod: podtest.MakePod("test-pod",
				podtest.SetResourceVersion("1"),
				podtest.SetEphemeralContainers(api.EphemeralContainer{
					EphemeralContainerCommon: api.EphemeralContainerCommon{
						Name:                     "debugger",
						Image:                    "image",
						ImagePullPolicy:          "IfNotPresent",
						TerminationMessagePolicy: "File",
					},
				}),
			),
		},
	}

	// expect one error
	for _, tc := range test {
		t.Run(tc.name, func(t *testing.T) {
			errs := EphemeralContainersStrategy.ValidateUpdate(genericapirequest.NewContext(), tc.newPod, tc.oldPod)
			if len(errs) == 0 {
				t.Errorf("unexpected success:ephemeral containers are not supported for static pods")
			} else if len(errs) != 1 {
				t.Errorf("unexpected errors:expected one error about ephemeral containers are not supported for static pods:got:%v:", errs)
			}
		})
	}
}

func TestPodStrategyValidateUpdate(t *testing.T) {
	test := []struct {
		name   string
		newPod *api.Pod
		oldPod *api.Pod
	}{
		{
			name:   "an existing pod with indivisible hugepages values to a new pod with indivisible hugepages values",
			newPod: newPodWithHugePageValue(api.ResourceHugePagesPrefix+"2Mi", resource.MustParse("2.1Mi")),
			oldPod: newPodWithHugePageValue(api.ResourceHugePagesPrefix+"2Mi", resource.MustParse("2.1Mi")),
		},
	}

	for _, tc := range test {
		t.Run(tc.name, func(t *testing.T) {
			if errs := Strategy.ValidateUpdate(genericapirequest.NewContext(), tc.newPod, tc.oldPod); len(errs) != 0 {
				t.Errorf("unexpected error:%v", errs)
			}
		})
	}
}

func TestDropNonEphemeralContainerUpdates(t *testing.T) {
	tests := []struct {
		name                    string
		oldPod, newPod, wantPod *api.Pod
	}{
		{
			name: "simple ephemeral container append",
			oldPod: &api.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:            "test-pod",
					Namespace:       "test-ns",
					ResourceVersion: "1",
				},
				Spec: api.PodSpec{
					Containers: []api.Container{
						{
							Name:  "container",
							Image: "image",
						},
					},
				},
			},
			newPod: &api.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:            "test-pod",
					Namespace:       "test-ns",
					ResourceVersion: "1",
				},
				Spec: api.PodSpec{
					Containers: []api.Container{
						{
							Name:  "container",
							Image: "image",
						},
					},
					EphemeralContainers: []api.EphemeralContainer{
						{
							EphemeralContainerCommon: api.EphemeralContainerCommon{
								Name:  "container",
								Image: "image",
							},
						},
					},
				},
			},
			wantPod: &api.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:            "test-pod",
					Namespace:       "test-ns",
					ResourceVersion: "1",
				},
				Spec: api.PodSpec{
					Containers: []api.Container{
						{
							Name:  "container",
							Image: "image",
						},
					},
					EphemeralContainers: []api.EphemeralContainer{
						{
							EphemeralContainerCommon: api.EphemeralContainerCommon{
								Name:  "container",
								Image: "image",
							},
						},
					},
				},
			},
		},
		{
			name: "whoops wrong pod",
			oldPod: &api.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:            "test-pod",
					Namespace:       "test-ns",
					ResourceVersion: "1",
					UID:             "blue",
				},
			},
			newPod: &api.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:            "new-pod",
					Namespace:       "new-ns",
					ResourceVersion: "1",
					UID:             "green",
				},
			},
			wantPod: &api.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:            "new-pod",
					Namespace:       "new-ns",
					ResourceVersion: "1",
					UID:             "green",
				},
			},
		},
		{
			name: "resource conflict during update",
			oldPod: &api.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:            "test-pod",
					Namespace:       "test-ns",
					ResourceVersion: "2",
					UID:             "blue",
				},
			},
			newPod: &api.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:            "test-pod",
					Namespace:       "test-ns",
					ResourceVersion: "1",
					UID:             "blue",
				},
			},
			wantPod: &api.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:            "test-pod",
					Namespace:       "test-ns",
					ResourceVersion: "1",
					UID:             "blue",
				},
			},
		},
		{
			name: "drop non-ephemeral container changes",
			oldPod: &api.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:            "test-pod",
					Namespace:       "test-ns",
					ResourceVersion: "1",
					Annotations:     map[string]string{"foo": "bar"},
				},
				Spec: api.PodSpec{
					Containers: []api.Container{
						{
							Name:  "container",
							Image: "image",
						},
					},
				},
			},
			newPod: &api.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:            "test-pod",
					Namespace:       "test-ns",
					ResourceVersion: "1",
					Annotations:     map[string]string{"foo": "bar", "whiz": "pop"},
					Finalizers:      []string{"milo"},
				},
				Spec: api.PodSpec{
					Containers: []api.Container{
						{
							Name:  "container",
							Image: "newimage",
						},
					},
					EphemeralContainers: []api.EphemeralContainer{
						{
							EphemeralContainerCommon: api.EphemeralContainerCommon{
								Name:  "container1",
								Image: "image",
							},
						},
						{
							EphemeralContainerCommon: api.EphemeralContainerCommon{
								Name:  "container2",
								Image: "image",
							},
						},
					},
				},
				Status: api.PodStatus{
					Message: "hi.",
				},
			},
			wantPod: &api.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:            "test-pod",
					Namespace:       "test-ns",
					ResourceVersion: "1",
					Annotations:     map[string]string{"foo": "bar"},
				},
				Spec: api.PodSpec{
					Containers: []api.Container{
						{
							Name:  "container",
							Image: "image",
						},
					},
					EphemeralContainers: []api.EphemeralContainer{
						{
							EphemeralContainerCommon: api.EphemeralContainerCommon{
								Name:  "container1",
								Image: "image",
							},
						},
						{
							EphemeralContainerCommon: api.EphemeralContainerCommon{
								Name:  "container2",
								Image: "image",
							},
						},
					},
				},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotPod := dropNonEphemeralContainerUpdates(tc.newPod, tc.oldPod)
			if diff := cmp.Diff(tc.wantPod, gotPod); diff != "" {
				t.Errorf("unexpected diff when dropping fields (-want, +got):\n%s", diff)
			}
		})
	}
}

func TestNodeInclusionPolicyEnablementInCreating(t *testing.T) {
	var (
		honor            = api.NodeInclusionPolicyHonor
		ignore           = api.NodeInclusionPolicyIgnore
		emptyConstraints = []api.TopologySpreadConstraint{
			{
				WhenUnsatisfiable: api.DoNotSchedule,
				TopologyKey:       "kubernetes.io/hostname",
				MaxSkew:           1,
			},
		}
		defaultConstraints = []api.TopologySpreadConstraint{
			{
				NodeAffinityPolicy: &honor,
				NodeTaintsPolicy:   &ignore,
				WhenUnsatisfiable:  api.DoNotSchedule,
				TopologyKey:        "kubernetes.io/hostname",
				MaxSkew:            1,
			},
		}
	)

	tests := []struct {
		name                          string
		topologySpreadConstraints     []api.TopologySpreadConstraint
		wantTopologySpreadConstraints []api.TopologySpreadConstraint
		enableNodeInclusionPolicy     bool
	}{
		{
			name:                          "nodeInclusionPolicy enabled with topology unset",
			topologySpreadConstraints:     emptyConstraints,
			wantTopologySpreadConstraints: emptyConstraints,
			enableNodeInclusionPolicy:     true,
		},
		{
			name:                          "nodeInclusionPolicy enabled with topology configured",
			topologySpreadConstraints:     defaultConstraints,
			wantTopologySpreadConstraints: defaultConstraints,
			enableNodeInclusionPolicy:     true,
		},
		{
			name:                          "nodeInclusionPolicy disabled with topology configured",
			topologySpreadConstraints:     defaultConstraints,
			wantTopologySpreadConstraints: emptyConstraints,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.NodeInclusionPolicyInPodTopologySpread, tc.enableNodeInclusionPolicy)

			pod := podtest.MakePod("foo")
			wantPod := pod.DeepCopy()
			pod.Spec.TopologySpreadConstraints = append(pod.Spec.TopologySpreadConstraints, tc.topologySpreadConstraints...)

			errs := Strategy.Validate(genericapirequest.NewContext(), pod)
			if len(errs) != 0 {
				t.Errorf("Unexpected error: %v", errs.ToAggregate())
			}

			Strategy.PrepareForCreate(genericapirequest.NewContext(), pod)
			wantPod.Spec.TopologySpreadConstraints = append(wantPod.Spec.TopologySpreadConstraints, tc.wantTopologySpreadConstraints...)
			if diff := cmp.Diff(wantPod, pod, cmpopts.IgnoreFields(pod.Status, "Phase", "QOSClass")); diff != "" {
				t.Errorf("%s unexpected result (-want, +got): %s", tc.name, diff)
			}
		})
	}
}

func TestNodeInclusionPolicyEnablementInUpdating(t *testing.T) {
	var (
		honor  = api.NodeInclusionPolicyHonor
		ignore = api.NodeInclusionPolicyIgnore
	)

	// Enable the Feature Gate during the first rule creation
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.NodeInclusionPolicyInPodTopologySpread, true)
	ctx := genericapirequest.NewDefaultContext()

	pod := podtest.MakePod("foo",
		podtest.SetTopologySpreadConstraints(api.TopologySpreadConstraint{
			NodeAffinityPolicy: &ignore,
			NodeTaintsPolicy:   &honor,
			WhenUnsatisfiable:  api.DoNotSchedule,
			TopologyKey:        "kubernetes.io/hostname",
			MaxSkew:            1,
		}),
	)

	errs := Strategy.Validate(ctx, pod)
	if len(errs) != 0 {
		t.Errorf("Unexpected error: %v", errs.ToAggregate())
	}

	createdPod := pod.DeepCopy()
	Strategy.PrepareForCreate(ctx, createdPod)

	if len(createdPod.Spec.TopologySpreadConstraints) != 1 ||
		*createdPod.Spec.TopologySpreadConstraints[0].NodeAffinityPolicy != ignore ||
		*createdPod.Spec.TopologySpreadConstraints[0].NodeTaintsPolicy != honor {
		t.Error("NodeInclusionPolicy created with unexpected result")
	}

	// Disable the Feature Gate and expect these fields still exist after updating.
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.NodeInclusionPolicyInPodTopologySpread, false)

	updatedPod := createdPod.DeepCopy()
	updatedPod.Labels = map[string]string{"foo": "bar"}
	updatedPod.ResourceVersion = "2"

	errs = Strategy.ValidateUpdate(ctx, updatedPod, createdPod)
	if len(errs) != 0 {
		t.Errorf("Unexpected error: %v", errs.ToAggregate())
	}

	Strategy.PrepareForUpdate(ctx, updatedPod, createdPod)

	if len(updatedPod.Spec.TopologySpreadConstraints) != 1 ||
		*updatedPod.Spec.TopologySpreadConstraints[0].NodeAffinityPolicy != ignore ||
		*updatedPod.Spec.TopologySpreadConstraints[0].NodeTaintsPolicy != honor {
		t.Error("NodeInclusionPolicy updated with unexpected result")
	}

	// Enable the Feature Gate again to check whether configured fields still exist after updating.
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.NodeInclusionPolicyInPodTopologySpread, true)

	updatedPod2 := updatedPod.DeepCopy()
	updatedPod2.Labels = map[string]string{"foo": "fuz"}
	updatedPod2.ResourceVersion = "3"

	errs = Strategy.ValidateUpdate(ctx, updatedPod2, updatedPod)
	if len(errs) != 0 {
		t.Errorf("Unexpected error: %v", errs.ToAggregate())
	}

	Strategy.PrepareForUpdate(ctx, updatedPod2, updatedPod)
	if len(updatedPod2.Spec.TopologySpreadConstraints) != 1 ||
		*updatedPod2.Spec.TopologySpreadConstraints[0].NodeAffinityPolicy != ignore ||
		*updatedPod2.Spec.TopologySpreadConstraints[0].NodeTaintsPolicy != honor {
		t.Error("NodeInclusionPolicy updated with unexpected result")
	}
}

func Test_mutatePodAffinity(t *testing.T) {
	tests := []struct {
		name               string
		pod                *api.Pod
		wantPod            *api.Pod
		featureGateEnabled bool
	}{
		{
			name:               "matchLabelKeys are merged into labelSelector with In and mismatchLabelKeys are merged with NotIn",
			featureGateEnabled: true,
			pod: &api.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"country": "Japan",
						"city":    "Kyoto",
					},
				},
				Spec: api.PodSpec{
					Affinity: &api.Affinity{
						PodAffinity: &api.PodAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: []api.PodAffinityTerm{
								{
									LabelSelector: &metav1.LabelSelector{
										MatchLabels: map[string]string{
											"region": "Asia",
										},
									},
									MatchLabelKeys:    []string{"country"},
									MismatchLabelKeys: []string{"city"},
								},
							},
							PreferredDuringSchedulingIgnoredDuringExecution: []api.WeightedPodAffinityTerm{
								{
									PodAffinityTerm: api.PodAffinityTerm{
										LabelSelector: &metav1.LabelSelector{
											MatchLabels: map[string]string{
												"region": "Asia",
											},
										},
										MatchLabelKeys:    []string{"country"},
										MismatchLabelKeys: []string{"city"},
									},
								},
							},
						},
						PodAntiAffinity: &api.PodAntiAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: []api.PodAffinityTerm{
								{
									LabelSelector: &metav1.LabelSelector{
										MatchLabels: map[string]string{
											"region": "Asia",
										},
									},
									MatchLabelKeys:    []string{"country"},
									MismatchLabelKeys: []string{"city"},
								},
							},
							PreferredDuringSchedulingIgnoredDuringExecution: []api.WeightedPodAffinityTerm{
								{
									PodAffinityTerm: api.PodAffinityTerm{
										LabelSelector: &metav1.LabelSelector{
											MatchLabels: map[string]string{
												"region": "Asia",
											},
										},
										MatchLabelKeys:    []string{"country"},
										MismatchLabelKeys: []string{"city"},
									},
								},
							},
						},
					},
				},
			},
			wantPod: &api.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"country": "Japan",
						"city":    "Kyoto",
					},
				},
				Spec: api.PodSpec{
					Affinity: &api.Affinity{
						PodAffinity: &api.PodAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: []api.PodAffinityTerm{
								{
									LabelSelector: &metav1.LabelSelector{
										MatchLabels: map[string]string{
											"region": "Asia",
										},
										MatchExpressions: []metav1.LabelSelectorRequirement{
											{
												Key:      "country",
												Operator: metav1.LabelSelectorOpIn,
												Values:   []string{"Japan"},
											},
											{
												Key:      "city",
												Operator: metav1.LabelSelectorOpNotIn,
												Values:   []string{"Kyoto"},
											},
										},
									},
									MatchLabelKeys:    []string{"country"},
									MismatchLabelKeys: []string{"city"},
								},
							},
							PreferredDuringSchedulingIgnoredDuringExecution: []api.WeightedPodAffinityTerm{
								{
									PodAffinityTerm: api.PodAffinityTerm{
										LabelSelector: &metav1.LabelSelector{
											MatchLabels: map[string]string{
												"region": "Asia",
											},
											MatchExpressions: []metav1.LabelSelectorRequirement{
												{
													Key:      "country",
													Operator: metav1.LabelSelectorOpIn,
													Values:   []string{"Japan"},
												},
												{
													Key:      "city",
													Operator: metav1.LabelSelectorOpNotIn,
													Values:   []string{"Kyoto"},
												},
											},
										},
										MatchLabelKeys:    []string{"country"},
										MismatchLabelKeys: []string{"city"},
									},
								},
							},
						},
						PodAntiAffinity: &api.PodAntiAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: []api.PodAffinityTerm{
								{
									LabelSelector: &metav1.LabelSelector{
										MatchLabels: map[string]string{
											"region": "Asia",
										},
										MatchExpressions: []metav1.LabelSelectorRequirement{
											{
												Key:      "country",
												Operator: metav1.LabelSelectorOpIn,
												Values:   []string{"Japan"},
											},
											{
												Key:      "city",
												Operator: metav1.LabelSelectorOpNotIn,
												Values:   []string{"Kyoto"},
											},
										},
									},
									MatchLabelKeys:    []string{"country"},
									MismatchLabelKeys: []string{"city"},
								},
							},
							PreferredDuringSchedulingIgnoredDuringExecution: []api.WeightedPodAffinityTerm{
								{
									PodAffinityTerm: api.PodAffinityTerm{
										LabelSelector: &metav1.LabelSelector{
											MatchLabels: map[string]string{
												"region": "Asia",
											},
											MatchExpressions: []metav1.LabelSelectorRequirement{
												{
													Key:      "country",
													Operator: metav1.LabelSelectorOpIn,
													Values:   []string{"Japan"},
												},
												{
													Key:      "city",
													Operator: metav1.LabelSelectorOpNotIn,
													Values:   []string{"Kyoto"},
												},
											},
										},
										MatchLabelKeys:    []string{"country"},
										MismatchLabelKeys: []string{"city"},
									},
								},
							},
						},
					},
				},
			},
		},
		{
			name:               "keys, which are not found in Pod labels, are ignored",
			featureGateEnabled: true,
			pod: &api.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"country": "Japan",
						"city":    "Kyoto",
					},
				},
				Spec: api.PodSpec{
					Affinity: &api.Affinity{
						PodAffinity: &api.PodAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: []api.PodAffinityTerm{
								{
									LabelSelector: &metav1.LabelSelector{
										MatchLabels: map[string]string{
											"region": "Asia",
										},
									},
									MatchLabelKeys: []string{"city", "not-found"},
								},
							},
						},
					},
				},
			},
			wantPod: &api.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"country": "Japan",
						"city":    "Kyoto",
					},
				},
				Spec: api.PodSpec{
					Affinity: &api.Affinity{
						PodAffinity: &api.PodAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: []api.PodAffinityTerm{
								{
									LabelSelector: &metav1.LabelSelector{
										MatchLabels: map[string]string{
											"region": "Asia",
										},
										MatchExpressions: []metav1.LabelSelectorRequirement{
											// "city" should be added correctly even if matchLabelKey has "not-found" key.
											{
												Key:      "city",
												Operator: metav1.LabelSelectorOpIn,
												Values:   []string{"Kyoto"},
											},
										},
									},
									MatchLabelKeys: []string{"city", "not-found"},
								},
							},
						},
					},
				},
			},
		},
		{
			name:               "matchLabelKeys is ignored if the labelSelector is nil",
			featureGateEnabled: true,
			pod: &api.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"country": "Japan",
						"city":    "Kyoto",
					},
				},
				Spec: api.PodSpec{
					Affinity: &api.Affinity{
						PodAffinity: &api.PodAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: []api.PodAffinityTerm{
								{
									MatchLabelKeys:    []string{"country"},
									MismatchLabelKeys: []string{"city"},
								},
							},
						},
					},
				},
			},
			wantPod: &api.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"country": "Japan",
						"city":    "Kyoto",
					},
				},
				Spec: api.PodSpec{
					Affinity: &api.Affinity{
						PodAffinity: &api.PodAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: []api.PodAffinityTerm{
								{
									MatchLabelKeys:    []string{"country"},
									MismatchLabelKeys: []string{"city"},
								},
							},
						},
					},
				},
			},
		},
		{
			name: "the feature gate is disabled and matchLabelKeys is ignored",
			pod: &api.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"country": "Japan",
						"city":    "Kyoto",
					},
				},
				Spec: api.PodSpec{
					Affinity: &api.Affinity{
						PodAffinity: &api.PodAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: []api.PodAffinityTerm{
								{
									LabelSelector: &metav1.LabelSelector{
										MatchLabels: map[string]string{
											"region": "Asia",
										},
									},
									MatchLabelKeys:    []string{"country"},
									MismatchLabelKeys: []string{"city"},
								},
							},
						},
					},
				},
			},
			wantPod: &api.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"country": "Japan",
						"city":    "Kyoto",
					},
				},
				Spec: api.PodSpec{
					Affinity: &api.Affinity{
						PodAffinity: &api.PodAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: []api.PodAffinityTerm{
								{
									LabelSelector: &metav1.LabelSelector{
										MatchLabels: map[string]string{
											"region": "Asia",
										},
									},
									MatchLabelKeys:    []string{"country"},
									MismatchLabelKeys: []string{"city"},
								},
							},
						},
					},
				},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.MatchLabelKeysInPodAffinity, tc.featureGateEnabled)

			pod := tc.pod
			mutatePodAffinity(pod)
			if diff := cmp.Diff(tc.wantPod.Spec.Affinity, pod.Spec.Affinity); diff != "" {
				t.Errorf("unexpected affinity (-want, +got): %s\n", diff)
			}
		})
	}
}

func TestPodLifecycleSleepActionEnablement(t *testing.T) {
	getLifecycle := func(pod *api.Pod) *api.Lifecycle {
		return pod.Spec.Containers[0].Lifecycle
	}

	defaultTerminationGracePeriodSeconds := int64(30)

	podWithHandler := func() *api.Pod {
		return &api.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace:       "default",
				Name:            "foo",
				ResourceVersion: "1",
			},
			Spec: api.PodSpec{
				RestartPolicy: api.RestartPolicyAlways,
				DNSPolicy:     api.DNSDefault,
				Containers: []api.Container{
					{
						Name:                     "container",
						Image:                    "image",
						ImagePullPolicy:          "IfNotPresent",
						TerminationMessagePolicy: "File",
						Lifecycle: &api.Lifecycle{
							PreStop: &api.LifecycleHandler{
								Sleep: &api.SleepAction{Seconds: 1},
							},
						},
					},
				},
				TerminationGracePeriodSeconds: &defaultTerminationGracePeriodSeconds,
			},
		}
	}

	podWithoutHandler := func() *api.Pod {
		return &api.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace:       "default",
				Name:            "foo",
				ResourceVersion: "1",
			},
			Spec: api.PodSpec{
				RestartPolicy: api.RestartPolicyAlways,
				DNSPolicy:     api.DNSDefault,
				Containers: []api.Container{
					{
						Name:                     "container",
						Image:                    "image",
						ImagePullPolicy:          "IfNotPresent",
						TerminationMessagePolicy: "File",
					},
				},
				TerminationGracePeriodSeconds: &defaultTerminationGracePeriodSeconds,
			},
		}
	}

	testCases := []struct {
		description string
		gateEnabled bool
		newPod      *api.Pod
		wantPod     *api.Pod
	}{
		{
			description: "gate enabled, creating pods with sleep action",
			gateEnabled: true,
			newPod:      podWithHandler(),
			wantPod:     podWithHandler(),
		},
		{
			description: "gate disabled, creating pods with sleep action",
			gateEnabled: false,
			newPod:      podWithHandler(),
			wantPod:     podWithoutHandler(),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.description, func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.PodLifecycleSleepAction, tc.gateEnabled)

			newPod := tc.newPod

			Strategy.PrepareForCreate(genericapirequest.NewContext(), newPod)
			if errs := Strategy.Validate(genericapirequest.NewContext(), newPod); len(errs) != 0 {
				t.Errorf("Unexpected error: %v", errs.ToAggregate())
			}

			if diff := cmp.Diff(getLifecycle(newPod), getLifecycle(tc.wantPod)); diff != "" {
				t.Fatalf("Unexpected modification to life cycle; diff (-got +want)\n%s", diff)
			}
		})
	}
}

func TestApplyAppArmorVersionSkew(t *testing.T) {
	testProfile := "test"

	tests := []struct {
		description   string
		pod           *api.Pod
		validation    func(*testing.T, *api.Pod)
		expectWarning bool
	}{{
		description: "Security context nil",
		pod: &api.Pod{
			Spec: api.PodSpec{
				InitContainers: []api.Container{{Name: "init"}},
				Containers:     []api.Container{{Name: "ctr"}},
			},
		},
		validation: func(t *testing.T, pod *api.Pod) {
			assert.Empty(t, pod.Annotations)
			assert.Nil(t, pod.Spec.SecurityContext)
		},
	}, {
		description: "Security context not nil",
		pod: &api.Pod{
			Spec: api.PodSpec{
				SecurityContext: &api.PodSecurityContext{},
				InitContainers:  []api.Container{{Name: "init"}},
				Containers:      []api.Container{{Name: "ctr"}},
			},
		},
		validation: func(t *testing.T, pod *api.Pod) {
			assert.Empty(t, pod.Annotations)
			assert.Nil(t, pod.Spec.SecurityContext.AppArmorProfile)
		},
	}, {
		description: "Pod field unconfined and no annotation present",
		pod: &api.Pod{
			Spec: api.PodSpec{
				SecurityContext: &api.PodSecurityContext{
					AppArmorProfile: &api.AppArmorProfile{
						Type: api.AppArmorProfileTypeUnconfined,
					},
				},
				InitContainers: []api.Container{{Name: "init"}},
				Containers:     []api.Container{{Name: "ctr"}},
			},
		},
		validation: func(t *testing.T, pod *api.Pod) {
			assert.Equal(t, map[string]string{
				api.DeprecatedAppArmorAnnotationKeyPrefix + "init": api.DeprecatedAppArmorAnnotationValueUnconfined,
				api.DeprecatedAppArmorAnnotationKeyPrefix + "ctr":  api.DeprecatedAppArmorAnnotationValueUnconfined,
			}, pod.Annotations)
		},
	}, {
		description: "Pod field default and no annotation present",
		pod: &api.Pod{
			Spec: api.PodSpec{
				SecurityContext: &api.PodSecurityContext{
					AppArmorProfile: &api.AppArmorProfile{
						Type: api.AppArmorProfileTypeRuntimeDefault,
					},
				},
				InitContainers: []api.Container{{Name: "init"}},
				Containers:     []api.Container{{Name: "ctr"}},
			},
		},
		validation: func(t *testing.T, pod *api.Pod) {
			assert.Equal(t, map[string]string{
				api.DeprecatedAppArmorAnnotationKeyPrefix + "init": api.DeprecatedAppArmorAnnotationValueRuntimeDefault,
				api.DeprecatedAppArmorAnnotationKeyPrefix + "ctr":  api.DeprecatedAppArmorAnnotationValueRuntimeDefault,
			}, pod.Annotations)
		},
	}, {
		description: "Pod field localhost and no annotation present",
		pod: &api.Pod{
			Spec: api.PodSpec{
				SecurityContext: &api.PodSecurityContext{
					AppArmorProfile: &api.AppArmorProfile{
						Type:             api.AppArmorProfileTypeLocalhost,
						LocalhostProfile: &testProfile,
					},
				},
				InitContainers: []api.Container{{Name: "init"}},
				Containers:     []api.Container{{Name: "ctr"}},
			},
		},
		validation: func(t *testing.T, pod *api.Pod) {
			assert.Equal(t, map[string]string{
				api.DeprecatedAppArmorAnnotationKeyPrefix + "init": api.DeprecatedAppArmorAnnotationValueLocalhostPrefix + testProfile,
				api.DeprecatedAppArmorAnnotationKeyPrefix + "ctr":  api.DeprecatedAppArmorAnnotationValueLocalhostPrefix + testProfile,
			}, pod.Annotations)
		},
	}, {
		description: "Pod field localhost but profile is nil",
		pod: &api.Pod{
			Spec: api.PodSpec{
				SecurityContext: &api.PodSecurityContext{
					AppArmorProfile: &api.AppArmorProfile{
						Type: api.AppArmorProfileTypeLocalhost,
					},
				},
				InitContainers: []api.Container{{Name: "init"}},
				Containers:     []api.Container{{Name: "ctr"}},
			},
		},
		validation: func(t *testing.T, pod *api.Pod) {
			assert.Empty(t, pod.Annotations)
		},
	}, {
		description: "Container security context not nil",
		pod: &api.Pod{
			Spec: api.PodSpec{
				Containers: []api.Container{{
					Name:            "ctr",
					SecurityContext: &api.SecurityContext{},
				}},
			},
		},
		validation: func(t *testing.T, pod *api.Pod) {
			assert.Empty(t, pod.Annotations)
		},
	}, {
		description: "Container field RuntimeDefault and no annotation present",
		pod: &api.Pod{
			Spec: api.PodSpec{
				Containers: []api.Container{{
					Name: "ctr",
					SecurityContext: &api.SecurityContext{
						AppArmorProfile: &api.AppArmorProfile{
							Type: api.AppArmorProfileTypeRuntimeDefault,
						},
					},
				}},
			},
		},
		validation: func(t *testing.T, pod *api.Pod) {
			assert.Equal(t, map[string]string{
				api.DeprecatedAppArmorAnnotationKeyPrefix + "ctr": api.DeprecatedAppArmorAnnotationValueRuntimeDefault,
			}, pod.Annotations)
			assert.Nil(t, pod.Spec.SecurityContext)
			assert.Equal(t, api.AppArmorProfileTypeRuntimeDefault, pod.Spec.Containers[0].SecurityContext.AppArmorProfile.Type)
		},
	}, {
		description: "Container field localhost and no annotation present",
		pod: &api.Pod{
			Spec: api.PodSpec{
				Containers: []api.Container{{
					Name: "ctr",
					SecurityContext: &api.SecurityContext{
						AppArmorProfile: &api.AppArmorProfile{
							Type:             api.AppArmorProfileTypeLocalhost,
							LocalhostProfile: &testProfile,
						},
					},
				}},
			},
		},
		validation: func(t *testing.T, pod *api.Pod) {
			assert.Equal(t, map[string]string{
				api.DeprecatedAppArmorAnnotationKeyPrefix + "ctr": api.DeprecatedAppArmorAnnotationValueLocalhostPrefix + testProfile,
			}, pod.Annotations)
			assert.Nil(t, pod.Spec.SecurityContext)
			assert.Equal(t, api.AppArmorProfileTypeLocalhost, pod.Spec.Containers[0].SecurityContext.AppArmorProfile.Type)
		},
	}, {
		description: "Container overrides pod profile",
		pod: &api.Pod{
			Spec: api.PodSpec{
				SecurityContext: &api.PodSecurityContext{
					AppArmorProfile: &api.AppArmorProfile{
						Type: api.AppArmorProfileTypeRuntimeDefault,
					},
				},
				Containers: []api.Container{{
					Name: "ctr",
					SecurityContext: &api.SecurityContext{
						AppArmorProfile: &api.AppArmorProfile{
							Type: api.AppArmorProfileTypeUnconfined,
						},
					},
				}},
			},
		},
		validation: func(t *testing.T, pod *api.Pod) {
			assert.Equal(t, map[string]string{
				api.DeprecatedAppArmorAnnotationKeyPrefix + "ctr": api.DeprecatedAppArmorAnnotationValueUnconfined,
			}, pod.Annotations)
			assert.Equal(t, api.AppArmorProfileTypeRuntimeDefault, pod.Spec.SecurityContext.AppArmorProfile.Type)
			assert.Equal(t, api.AppArmorProfileTypeUnconfined, pod.Spec.Containers[0].SecurityContext.AppArmorProfile.Type)
		},
	}, {
		description: "Multiple containers with fields (container)",
		pod: &api.Pod{
			Spec: api.PodSpec{
				InitContainers: []api.Container{{
					Name: "init",
					SecurityContext: &api.SecurityContext{
						AppArmorProfile: &api.AppArmorProfile{
							Type:             api.AppArmorProfileTypeLocalhost,
							LocalhostProfile: &testProfile,
						},
					},
				}},
				Containers: []api.Container{{
					Name: "a",
					SecurityContext: &api.SecurityContext{
						AppArmorProfile: &api.AppArmorProfile{
							Type: api.AppArmorProfileTypeUnconfined,
						},
					},
				}, {
					Name: "b",
				}, {
					Name: "c",
					SecurityContext: &api.SecurityContext{
						AppArmorProfile: &api.AppArmorProfile{
							Type: api.AppArmorProfileTypeRuntimeDefault,
						},
					},
				}},
			},
		},
		validation: func(t *testing.T, pod *api.Pod) {
			assert.Equal(t, map[string]string{
				api.DeprecatedAppArmorAnnotationKeyPrefix + "init": api.DeprecatedAppArmorAnnotationValueLocalhostPrefix + testProfile,
				api.DeprecatedAppArmorAnnotationKeyPrefix + "a":    api.DeprecatedAppArmorAnnotationValueUnconfined,
				api.DeprecatedAppArmorAnnotationKeyPrefix + "c":    api.DeprecatedAppArmorAnnotationValueRuntimeDefault,
			}, pod.Annotations)
			assert.Nil(t, pod.Spec.SecurityContext)
			assert.Equal(t, api.AppArmorProfileTypeLocalhost, pod.Spec.InitContainers[0].SecurityContext.AppArmorProfile.Type)
			assert.Equal(t, api.AppArmorProfileTypeUnconfined, pod.Spec.Containers[0].SecurityContext.AppArmorProfile.Type)
			assert.Nil(t, pod.Spec.Containers[1].SecurityContext)
			assert.Equal(t, api.AppArmorProfileTypeRuntimeDefault, pod.Spec.Containers[2].SecurityContext.AppArmorProfile.Type)
		},
	}, {
		description: "Annotation 'unconfined' and no fields present",
		pod: &api.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					api.DeprecatedAppArmorAnnotationKeyPrefix + "ctr": api.DeprecatedAppArmorAnnotationValueUnconfined,
				},
			},
			Spec: api.PodSpec{
				Containers: []api.Container{{Name: "ctr"}},
			},
		},
		validation: func(t *testing.T, pod *api.Pod) {
			assert.Equal(t, map[string]string{
				api.DeprecatedAppArmorAnnotationKeyPrefix + "ctr": api.DeprecatedAppArmorAnnotationValueUnconfined,
			}, pod.Annotations)
			assert.Equal(t, api.AppArmorProfileTypeUnconfined, pod.Spec.Containers[0].SecurityContext.AppArmorProfile.Type)
			assert.Nil(t, pod.Spec.Containers[0].SecurityContext.AppArmorProfile.LocalhostProfile)
			assert.Nil(t, pod.Spec.SecurityContext)
		},
		expectWarning: true,
	}, {
		description: "Annotation for non-existent container",
		pod: &api.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					api.DeprecatedAppArmorAnnotationKeyPrefix + "foo-bar": api.DeprecatedAppArmorAnnotationValueUnconfined,
				},
			},
			Spec: api.PodSpec{
				Containers: []api.Container{{Name: "ctr"}},
			},
		},
		validation: func(t *testing.T, pod *api.Pod) {
			assert.Equal(t, map[string]string{
				api.DeprecatedAppArmorAnnotationKeyPrefix + "foo-bar": api.DeprecatedAppArmorAnnotationValueUnconfined,
			}, pod.Annotations)
			assert.Nil(t, pod.Spec.Containers[0].SecurityContext)
			assert.Nil(t, pod.Spec.SecurityContext)
		},
	}, {
		description: "Annotation 'runtime/default' and no fields present",
		pod: &api.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					api.DeprecatedAppArmorAnnotationKeyPrefix + "ctr": api.DeprecatedAppArmorAnnotationValueRuntimeDefault,
				},
			},
			Spec: api.PodSpec{
				SecurityContext: &api.PodSecurityContext{
					AppArmorProfile: &api.AppArmorProfile{
						Type: api.AppArmorProfileTypeUnconfined,
					},
				},
				Containers: []api.Container{{
					Name:            "ctr",
					SecurityContext: &api.SecurityContext{},
				}},
			},
		},
		validation: func(t *testing.T, pod *api.Pod) {
			assert.Equal(t, map[string]string{
				api.DeprecatedAppArmorAnnotationKeyPrefix + "ctr": api.DeprecatedAppArmorAnnotationValueRuntimeDefault,
			}, pod.Annotations)
			assert.Equal(t, api.AppArmorProfileTypeRuntimeDefault, pod.Spec.Containers[0].SecurityContext.AppArmorProfile.Type)
			assert.Nil(t, pod.Spec.Containers[0].SecurityContext.AppArmorProfile.LocalhostProfile)
			assert.Equal(t, api.AppArmorProfileTypeUnconfined, pod.Spec.SecurityContext.AppArmorProfile.Type)
		},
		expectWarning: true,
	}, {
		description: "Multiple containers by annotations",
		pod: &api.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					api.DeprecatedAppArmorAnnotationKeyPrefix + "init": api.DeprecatedAppArmorAnnotationValueUnconfined,
					api.DeprecatedAppArmorAnnotationKeyPrefix + "a":    api.DeprecatedAppArmorAnnotationValueLocalhostPrefix + testProfile,
					api.DeprecatedAppArmorAnnotationKeyPrefix + "c":    api.DeprecatedAppArmorAnnotationValueRuntimeDefault,
				},
			},
			Spec: api.PodSpec{
				SecurityContext: &api.PodSecurityContext{
					AppArmorProfile: &api.AppArmorProfile{
						Type: api.AppArmorProfileTypeRuntimeDefault,
					},
				},
				InitContainers: []api.Container{{Name: "init"}},
				Containers: []api.Container{
					{Name: "a"},
					{Name: "b"},
					{Name: "c"},
				},
			},
		},
		validation: func(t *testing.T, pod *api.Pod) {
			assert.Equal(t, map[string]string{
				api.DeprecatedAppArmorAnnotationKeyPrefix + "init": api.DeprecatedAppArmorAnnotationValueUnconfined,
				api.DeprecatedAppArmorAnnotationKeyPrefix + "a":    api.DeprecatedAppArmorAnnotationValueLocalhostPrefix + testProfile,
				api.DeprecatedAppArmorAnnotationKeyPrefix + "b":    api.DeprecatedAppArmorAnnotationValueRuntimeDefault,
				api.DeprecatedAppArmorAnnotationKeyPrefix + "c":    api.DeprecatedAppArmorAnnotationValueRuntimeDefault,
			}, pod.Annotations)
			assert.Equal(t, api.AppArmorProfileTypeUnconfined, pod.Spec.InitContainers[0].SecurityContext.AppArmorProfile.Type)
			assert.Equal(t, api.AppArmorProfileTypeLocalhost, pod.Spec.Containers[0].SecurityContext.AppArmorProfile.Type)
			assert.Equal(t, testProfile, *pod.Spec.Containers[0].SecurityContext.AppArmorProfile.LocalhostProfile)
			assert.Nil(t, pod.Spec.Containers[1].SecurityContext)
			assert.Nil(t, pod.Spec.Containers[2].SecurityContext)
			assert.Equal(t, api.AppArmorProfileTypeRuntimeDefault, pod.Spec.SecurityContext.AppArmorProfile.Type)
		},
		expectWarning: true,
	}, {
		description: "Conflicting field and annotations",
		pod: &api.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					api.DeprecatedAppArmorAnnotationKeyPrefix + "ctr": api.DeprecatedAppArmorAnnotationValueLocalhostPrefix + testProfile,
				},
			},
			Spec: api.PodSpec{
				Containers: []api.Container{{
					Name: "ctr",
					SecurityContext: &api.SecurityContext{
						AppArmorProfile: &api.AppArmorProfile{
							Type: api.AppArmorProfileTypeRuntimeDefault,
						},
					},
				}},
			},
		},
		validation: func(t *testing.T, pod *api.Pod) {
			assert.Equal(t, map[string]string{
				api.DeprecatedAppArmorAnnotationKeyPrefix + "ctr": api.DeprecatedAppArmorAnnotationValueLocalhostPrefix + testProfile,
			}, pod.Annotations)
			assert.Equal(t, api.AppArmorProfileTypeRuntimeDefault, pod.Spec.Containers[0].SecurityContext.AppArmorProfile.Type)
			assert.Nil(t, pod.Spec.Containers[0].SecurityContext.AppArmorProfile.LocalhostProfile)
			assert.Nil(t, pod.Spec.SecurityContext)
		},
	}, {
		description: "Pod field and matching annotations",
		pod: &api.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					api.DeprecatedAppArmorAnnotationKeyPrefix + "ctr": api.DeprecatedAppArmorAnnotationValueRuntimeDefault,
				},
			},
			Spec: api.PodSpec{
				SecurityContext: &api.PodSecurityContext{
					AppArmorProfile: &api.AppArmorProfile{
						Type: api.AppArmorProfileTypeRuntimeDefault,
					},
				},
				Containers: []api.Container{{
					Name: "ctr",
				}},
			},
		},
		validation: func(t *testing.T, pod *api.Pod) {
			assert.Equal(t, map[string]string{
				api.DeprecatedAppArmorAnnotationKeyPrefix + "ctr": api.DeprecatedAppArmorAnnotationValueRuntimeDefault,
			}, pod.Annotations)
			assert.Equal(t, api.AppArmorProfileTypeRuntimeDefault, pod.Spec.SecurityContext.AppArmorProfile.Type)
			// Annotation shouldn't be synced to container security context
			assert.Nil(t, pod.Spec.Containers[0].SecurityContext)
		},
	}, {
		description: "Annotation overrides pod field",
		pod: &api.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					api.DeprecatedAppArmorAnnotationKeyPrefix + "ctr": api.DeprecatedAppArmorAnnotationValueUnconfined,
				},
			},
			Spec: api.PodSpec{
				SecurityContext: &api.PodSecurityContext{
					AppArmorProfile: &api.AppArmorProfile{
						Type: api.AppArmorProfileTypeRuntimeDefault,
					},
				},
				Containers: []api.Container{{
					Name: "ctr",
				}},
			},
		},
		validation: func(t *testing.T, pod *api.Pod) {
			assert.Equal(t, map[string]string{
				api.DeprecatedAppArmorAnnotationKeyPrefix + "ctr": api.DeprecatedAppArmorAnnotationValueUnconfined,
			}, pod.Annotations)
			assert.Equal(t, api.AppArmorProfileTypeRuntimeDefault, pod.Spec.SecurityContext.AppArmorProfile.Type)
			assert.Equal(t, api.AppArmorProfileTypeUnconfined, pod.Spec.Containers[0].SecurityContext.AppArmorProfile.Type)
		},
		expectWarning: true,
	}, {
		description: "Mixed annotations and fields",
		pod: &api.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					api.DeprecatedAppArmorAnnotationKeyPrefix + "unconf-annot": api.DeprecatedAppArmorAnnotationValueUnconfined,
				},
			},
			Spec: api.PodSpec{
				SecurityContext: &api.PodSecurityContext{
					AppArmorProfile: &api.AppArmorProfile{
						Type: api.AppArmorProfileTypeRuntimeDefault,
					},
				},
				Containers: []api.Container{{
					Name: "unconf-annot",
				}, {
					Name: "unconf-field",
					SecurityContext: &api.SecurityContext{
						AppArmorProfile: &api.AppArmorProfile{
							Type: api.AppArmorProfileTypeUnconfined,
						},
					},
				}, {
					Name: "default-pod",
				}},
			},
		},
		validation: func(t *testing.T, pod *api.Pod) {
			assert.Equal(t, map[string]string{
				api.DeprecatedAppArmorAnnotationKeyPrefix + "unconf-annot": api.DeprecatedAppArmorAnnotationValueUnconfined,
				api.DeprecatedAppArmorAnnotationKeyPrefix + "unconf-field": api.DeprecatedAppArmorAnnotationValueUnconfined,
				api.DeprecatedAppArmorAnnotationKeyPrefix + "default-pod":  api.DeprecatedAppArmorAnnotationValueRuntimeDefault,
			}, pod.Annotations)
			assert.Equal(t, api.AppArmorProfileTypeRuntimeDefault, pod.Spec.SecurityContext.AppArmorProfile.Type)
			assert.Equal(t, api.AppArmorProfileTypeUnconfined, pod.Spec.Containers[0].SecurityContext.AppArmorProfile.Type)
			assert.Equal(t, api.AppArmorProfileTypeUnconfined, pod.Spec.Containers[1].SecurityContext.AppArmorProfile.Type)
			assert.Nil(t, pod.Spec.Containers[2].SecurityContext)
		},
		expectWarning: true,
	}, {
		description: "Invalid annotation value",
		pod: &api.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					api.DeprecatedAppArmorAnnotationKeyPrefix + "ctr": "localhost/",
				},
			},
			Spec: api.PodSpec{
				Containers: []api.Container{{Name: "ctr"}},
			},
		},
		validation: func(t *testing.T, pod *api.Pod) {
			assert.Equal(t, map[string]string{
				api.DeprecatedAppArmorAnnotationKeyPrefix + "ctr": "localhost/",
			}, pod.Annotations)
			assert.Nil(t, pod.Spec.Containers[0].SecurityContext)
			assert.Nil(t, pod.Spec.SecurityContext)
		},
		expectWarning: true,
	}, {
		description: "Invalid localhost annotation",
		pod: &api.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					api.DeprecatedAppArmorAnnotationKeyPrefix + "ctr": api.DeprecatedAppArmorAnnotationValueLocalhostPrefix + strings.Repeat("a", 4096),
				},
			},
			Spec: api.PodSpec{
				Containers: []api.Container{{Name: "ctr"}},
			},
		},
		validation: func(t *testing.T, pod *api.Pod) {
			assert.Contains(t, pod.Annotations, api.DeprecatedAppArmorAnnotationKeyPrefix+"ctr")
			assert.Nil(t, pod.Spec.Containers[0].SecurityContext)
			assert.Nil(t, pod.Spec.SecurityContext)
		},
		expectWarning: true,
	}, {
		description: "Invalid field type",
		pod: &api.Pod{
			Spec: api.PodSpec{
				SecurityContext: &api.PodSecurityContext{
					AppArmorProfile: &api.AppArmorProfile{
						Type: "invalid-type",
					},
				},
				Containers: []api.Container{{Name: "ctr"}},
			},
		},
		validation: func(t *testing.T, pod *api.Pod) {
			assert.Empty(t, pod.Annotations)
			assert.Nil(t, pod.Spec.Containers[0].SecurityContext)
		},
	}, {
		description: "Ignore annotations on windows",
		pod: &api.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					api.DeprecatedAppArmorAnnotationKeyPrefix + "ctr": api.DeprecatedAppArmorAnnotationValueRuntimeDefault,
				},
			},
			Spec: api.PodSpec{
				OS:         &api.PodOS{Name: api.Windows},
				Containers: []api.Container{{Name: "ctr"}},
			},
		},
		validation: func(t *testing.T, pod *api.Pod) {
			assert.Equal(t, map[string]string{
				api.DeprecatedAppArmorAnnotationKeyPrefix + "ctr": api.DeprecatedAppArmorAnnotationValueRuntimeDefault,
			}, pod.Annotations)
			assert.Nil(t, pod.Spec.Containers[0].SecurityContext)
		},
	}}

	for _, test := range tests {
		t.Run(test.description, func(t *testing.T) {
			warnings := &warningRecorder{}
			ctx := warning.WithWarningRecorder(context.Background(), warnings)
			applyAppArmorVersionSkew(ctx, test.pod)
			test.validation(t, test.pod)

			if test.expectWarning {
				if assert.NotEmpty(t, warnings.warnings, "expect warnings") {
					assert.Contains(t, warnings.warnings[0], `deprecated since v1.30; use the "appArmorProfile" field instead`)
				}
			} else {
				assert.Empty(t, warnings.warnings, "shouldn't emit a warning")
			}
		})
	}
}

type warningRecorder struct {
	warnings []string
}

var _ warning.Recorder = &warningRecorder{}

func (w *warningRecorder) AddWarning(_, text string) {
	w.warnings = append(w.warnings, text)
}

func TestPodResizePrepareForUpdate(t *testing.T) {
	containerRestartPolicyAlways := api.ContainerRestartPolicyAlways
	tests := []struct {
		name     string
		oldPod   *api.Pod
		newPod   *api.Pod
		expected *api.Pod
	}{
		{
			name: "no resize",
			oldPod: podtest.MakePod("test-pod",
				podtest.SetResourceVersion("1"),
				podtest.SetContainers(podtest.MakeContainer("container1",
					podtest.SetContainerResources(api.ResourceRequirements{
						Requests: api.ResourceList{
							api.ResourceCPU:    resource.MustParse("100m"),
							api.ResourceMemory: resource.MustParse("1Gi"),
						},
					}),
				)),
				podtest.SetStatus(podtest.MakePodStatus(
					podtest.SetContainerStatuses(podtest.MakeContainerStatus("container1",
						api.ResourceList{
							api.ResourceCPU:    resource.MustParse("100m"),
							api.ResourceMemory: resource.MustParse("1Gi"),
						}),
					),
				)),
			),
			newPod: podtest.MakePod("test-pod",
				podtest.SetResourceVersion("1"),
				podtest.SetContainers(podtest.MakeContainer("container1",
					podtest.SetContainerResources(api.ResourceRequirements{
						Requests: api.ResourceList{
							api.ResourceCPU:    resource.MustParse("100m"),
							api.ResourceMemory: resource.MustParse("1Gi"),
						},
					}),
				)),
				podtest.SetStatus(podtest.MakePodStatus(
					podtest.SetContainerStatuses(podtest.MakeContainerStatus("container1",
						api.ResourceList{
							api.ResourceCPU:    resource.MustParse("100m"),
							api.ResourceMemory: resource.MustParse("1Gi"),
						}),
					),
				)),
			),
			expected: podtest.MakePod("test-pod",
				podtest.SetResourceVersion("1"),
				podtest.SetContainers(podtest.MakeContainer("container1",
					podtest.SetContainerResources(api.ResourceRequirements{
						Requests: api.ResourceList{
							api.ResourceCPU:    resource.MustParse("100m"),
							api.ResourceMemory: resource.MustParse("1Gi"),
						},
					}),
				)),
				podtest.SetStatus(podtest.MakePodStatus(
					podtest.SetContainerStatuses(podtest.MakeContainerStatus("container1",
						api.ResourceList{
							api.ResourceCPU:    resource.MustParse("100m"),
							api.ResourceMemory: resource.MustParse("1Gi"),
						}),
					),
				)),
			),
		},
		{
			name: "update resizepolicy",
			oldPod: podtest.MakePod("test-pod",
				podtest.SetResourceVersion("1"),
				podtest.SetContainers(podtest.MakeContainer("container1",
					podtest.SetContainerResources(api.ResourceRequirements{
						Requests: api.ResourceList{
							api.ResourceCPU:    resource.MustParse("100m"),
							api.ResourceMemory: resource.MustParse("1Gi"),
						},
					}),
				)),
				podtest.SetStatus(podtest.MakePodStatus(
					podtest.SetContainerStatuses(podtest.MakeContainerStatus("container1",
						api.ResourceList{
							api.ResourceCPU:    resource.MustParse("100m"),
							api.ResourceMemory: resource.MustParse("1Gi"),
						})),
				)),
			),
			newPod: podtest.MakePod("test-pod",
				podtest.SetResourceVersion("2"),
				podtest.SetContainers(podtest.MakeContainer("container1",
					podtest.SetContainerResources(api.ResourceRequirements{
						Requests: api.ResourceList{
							api.ResourceCPU:    resource.MustParse("100m"),
							api.ResourceMemory: resource.MustParse("1Gi"),
						},
					}),
					podtest.SetContainerResizePolicy(
						api.ContainerResizePolicy{ResourceName: "cpu", RestartPolicy: "NotRequired"},
						api.ContainerResizePolicy{ResourceName: "memory", RestartPolicy: "RestartContainer"},
					),
				)),
				podtest.SetStatus(podtest.MakePodStatus(
					podtest.SetContainerStatuses(podtest.MakeContainerStatus("container1",
						api.ResourceList{
							api.ResourceCPU:    resource.MustParse("100m"),
							api.ResourceMemory: resource.MustParse("1Gi"),
						})),
				)),
			),
			expected: podtest.MakePod("test-pod",
				podtest.SetResourceVersion("2"),
				podtest.SetContainers(podtest.MakeContainer("container1",
					podtest.SetContainerResources(api.ResourceRequirements{
						Requests: api.ResourceList{
							api.ResourceCPU:    resource.MustParse("100m"),
							api.ResourceMemory: resource.MustParse("1Gi"),
						},
					}),
					podtest.SetContainerResizePolicy(
						api.ContainerResizePolicy{ResourceName: "cpu", RestartPolicy: "NotRequired"},
						api.ContainerResizePolicy{ResourceName: "memory", RestartPolicy: "RestartContainer"},
					),
				)),
				podtest.SetStatus(podtest.MakePodStatus(
					podtest.SetContainerStatuses(podtest.MakeContainerStatus("container1",
						api.ResourceList{
							api.ResourceCPU:    resource.MustParse("100m"),
							api.ResourceMemory: resource.MustParse("1Gi"),
						})),
				)),
			),
		},
		{
			name: "add new container",
			oldPod: podtest.MakePod("test-pod",
				podtest.SetResourceVersion("1"),
				podtest.SetContainers(podtest.MakeContainer("container1",
					podtest.SetContainerResources(api.ResourceRequirements{
						Requests: api.ResourceList{
							api.ResourceCPU:    resource.MustParse("100m"),
							api.ResourceMemory: resource.MustParse("1Gi"),
						},
					}),
				)),
				podtest.SetStatus(podtest.MakePodStatus(
					podtest.SetContainerStatuses(podtest.MakeContainerStatus("container1",
						api.ResourceList{
							api.ResourceCPU:    resource.MustParse("100m"),
							api.ResourceMemory: resource.MustParse("1Gi"),
						})),
				)),
			),
			newPod: podtest.MakePod("test-pod",
				podtest.SetResourceVersion("2"),
				podtest.SetContainers(
					podtest.MakeContainer("container1",
						podtest.SetContainerResources(api.ResourceRequirements{
							Requests: api.ResourceList{
								api.ResourceCPU:    resource.MustParse("100m"),
								api.ResourceMemory: resource.MustParse("1Gi"),
							},
						}),
					),
					podtest.MakeContainer("container2",
						podtest.SetContainerResources(api.ResourceRequirements{
							Requests: api.ResourceList{
								api.ResourceCPU:    resource.MustParse("100m"),
								api.ResourceMemory: resource.MustParse("1Gi"),
							},
						}),
					),
				),
				podtest.SetStatus(podtest.MakePodStatus(
					podtest.SetContainerStatuses(podtest.MakeContainerStatus("container1",
						api.ResourceList{
							api.ResourceCPU:    resource.MustParse("100m"),
							api.ResourceMemory: resource.MustParse("1Gi"),
						})),
				)),
			),
			expected: podtest.MakePod("test-pod",
				podtest.SetResourceVersion("2"),
				podtest.SetContainers(podtest.MakeContainer("container1",
					podtest.SetContainerResources(api.ResourceRequirements{
						Requests: api.ResourceList{
							api.ResourceCPU:    resource.MustParse("100m"),
							api.ResourceMemory: resource.MustParse("1Gi"),
						},
					}),
				)),
				podtest.SetStatus(podtest.MakePodStatus(
					podtest.SetContainerStatuses(podtest.MakeContainerStatus("container1",
						api.ResourceList{
							api.ResourceCPU:    resource.MustParse("100m"),
							api.ResourceMemory: resource.MustParse("1Gi"),
						})),
				)),
			),
		},
		{
			name: "add new container and update resources of existing container",
			oldPod: podtest.MakePod("test-pod",
				podtest.SetResourceVersion("1"),
				podtest.SetContainers(podtest.MakeContainer("container1",
					podtest.SetContainerResources(api.ResourceRequirements{
						Requests: api.ResourceList{
							api.ResourceCPU:    resource.MustParse("100m"),
							api.ResourceMemory: resource.MustParse("1Gi"),
						},
					}),
				)),
				podtest.SetStatus(podtest.MakePodStatus(
					podtest.SetContainerStatuses(podtest.MakeContainerStatus("container1",
						api.ResourceList{
							api.ResourceCPU:    resource.MustParse("100m"),
							api.ResourceMemory: resource.MustParse("1Gi"),
						})),
				)),
			),
			newPod: podtest.MakePod("test-pod",
				podtest.SetResourceVersion("2"),
				podtest.SetContainers(
					podtest.MakeContainer("container1",
						podtest.SetContainerResources(api.ResourceRequirements{
							Requests: api.ResourceList{
								api.ResourceCPU:    resource.MustParse("100m"),
								api.ResourceMemory: resource.MustParse("2Gi"), // Updated resource
							},
						}),
					),
					podtest.MakeContainer("container2",
						podtest.SetContainerResources(api.ResourceRequirements{
							Requests: api.ResourceList{
								api.ResourceCPU:    resource.MustParse("100m"),
								api.ResourceMemory: resource.MustParse("1Gi"),
							},
						}),
					),
				),
				podtest.SetStatus(podtest.MakePodStatus(
					podtest.SetContainerStatuses(podtest.MakeContainerStatus("container1",
						api.ResourceList{
							api.ResourceCPU:    resource.MustParse("100m"),
							api.ResourceMemory: resource.MustParse("1Gi"),
						})),
				)),
			),
			expected: podtest.MakePod("test-pod",
				podtest.SetResourceVersion("2"),
				podtest.SetContainers(podtest.MakeContainer("container1",
					podtest.SetContainerResources(api.ResourceRequirements{
						Requests: api.ResourceList{
							api.ResourceCPU:    resource.MustParse("100m"),
							api.ResourceMemory: resource.MustParse("2Gi"), // Updated resource
						},
					}),
				)),
				podtest.SetStatus(podtest.MakePodStatus(
					podtest.SetResizeStatus(api.PodResizeStatusProposed), // Resize status set
					podtest.SetContainerStatuses(podtest.MakeContainerStatus("container1",
						api.ResourceList{
							api.ResourceCPU:    resource.MustParse("100m"),
							api.ResourceMemory: resource.MustParse("1Gi"),
						})),
				)),
			),
		},
		{
			name: "change container order and update resources",
			oldPod: podtest.MakePod("test-pod",
				podtest.SetResourceVersion("1"),
				podtest.SetContainers(
					podtest.MakeContainer("container1",
						podtest.SetContainerResources(api.ResourceRequirements{
							Requests: api.ResourceList{
								api.ResourceCPU:    resource.MustParse("100m"),
								api.ResourceMemory: resource.MustParse("1Gi"),
							},
						}),
					),
					podtest.MakeContainer("container2",
						podtest.SetContainerResources(api.ResourceRequirements{
							Requests: api.ResourceList{
								api.ResourceCPU:    resource.MustParse("100m"),
								api.ResourceMemory: resource.MustParse("1Gi"),
							},
						}),
					),
				),
				podtest.SetStatus(podtest.MakePodStatus(
					podtest.SetContainerStatuses(
						podtest.MakeContainerStatus("container1",
							api.ResourceList{
								api.ResourceCPU:    resource.MustParse("100m"),
								api.ResourceMemory: resource.MustParse("1Gi"),
							}),
						podtest.MakeContainerStatus("container2",
							api.ResourceList{
								api.ResourceCPU:    resource.MustParse("100m"),
								api.ResourceMemory: resource.MustParse("1Gi"),
							}),
					),
				)),
			),
			newPod: podtest.MakePod("test-pod",
				podtest.SetResourceVersion("2"),
				podtest.SetContainers(
					podtest.MakeContainer("container2", // Order changed
						podtest.SetContainerResources(api.ResourceRequirements{
							Requests: api.ResourceList{
								api.ResourceCPU:    resource.MustParse("200m"), // Updated resource
								api.ResourceMemory: resource.MustParse("2Gi"),  // Updated resource
							},
						}),
					),
					podtest.MakeContainer("container1", // Order changed
						podtest.SetContainerResources(api.ResourceRequirements{
							Requests: api.ResourceList{
								api.ResourceCPU:    resource.MustParse("200m"), // Updated resource
								api.ResourceMemory: resource.MustParse("4Gi"),  // Updated resource
							},
						}),
					),
				),
				podtest.SetStatus(podtest.MakePodStatus(
					podtest.SetContainerStatuses(
						podtest.MakeContainerStatus("container1",
							api.ResourceList{
								api.ResourceCPU:    resource.MustParse("100m"),
								api.ResourceMemory: resource.MustParse("1Gi"),
							}),
						podtest.MakeContainerStatus("container2",
							api.ResourceList{
								api.ResourceCPU:    resource.MustParse("100m"),
								api.ResourceMemory: resource.MustParse("1Gi"),
							}),
					),
				)),
			),
			expected: podtest.MakePod("test-pod",
				podtest.SetResourceVersion("2"),
				podtest.SetContainers(
					podtest.MakeContainer("container1",
						podtest.SetContainerResources(api.ResourceRequirements{
							Requests: api.ResourceList{
								api.ResourceCPU:    resource.MustParse("200m"), // Updated resource
								api.ResourceMemory: resource.MustParse("4Gi"),  // Updated resource
							},
						}),
					),
					podtest.MakeContainer("container2",
						podtest.SetContainerResources(api.ResourceRequirements{
							Requests: api.ResourceList{
								api.ResourceCPU:    resource.MustParse("200m"), // Updated resource
								api.ResourceMemory: resource.MustParse("2Gi"),  // Updated resource
							},
						}),
					),
				),
				podtest.SetStatus(podtest.MakePodStatus(
					podtest.SetResizeStatus(api.PodResizeStatusProposed), // Resize status set
					podtest.SetContainerStatuses(
						podtest.MakeContainerStatus("container1",
							api.ResourceList{
								api.ResourceCPU:    resource.MustParse("100m"),
								api.ResourceMemory: resource.MustParse("1Gi"),
							}),
						podtest.MakeContainerStatus("container2",
							api.ResourceList{
								api.ResourceCPU:    resource.MustParse("100m"),
								api.ResourceMemory: resource.MustParse("1Gi"),
							}),
					),
				)),
			),
		},
		{
			name: "change pod labels",
			oldPod: podtest.MakePod("test-pod",
				podtest.SetResourceVersion("1"),
				podtest.SetLabels(map[string]string{"foo": "bar"}),
				podtest.SetContainers(
					podtest.MakeContainer("container1",
						podtest.SetContainerResources(api.ResourceRequirements{
							Requests: api.ResourceList{
								api.ResourceCPU:    resource.MustParse("100m"),
								api.ResourceMemory: resource.MustParse("1Gi"),
							},
						}),
					),
				),
				podtest.SetStatus(podtest.MakePodStatus(
					podtest.SetContainerStatuses(
						podtest.MakeContainerStatus("container1",
							api.ResourceList{
								api.ResourceCPU:    resource.MustParse("100m"),
								api.ResourceMemory: resource.MustParse("1Gi"),
							}),
					),
				)),
			),
			newPod: podtest.MakePod("test-pod",
				podtest.SetResourceVersion("2"),
				podtest.SetLabels(map[string]string{"foo": "bar", "baz": "qux"}),
				podtest.SetContainers(
					podtest.MakeContainer("container1",
						podtest.SetContainerResources(api.ResourceRequirements{
							Requests: api.ResourceList{
								api.ResourceCPU:    resource.MustParse("100m"),
								api.ResourceMemory: resource.MustParse("1Gi"),
							},
						}),
					),
				),
				podtest.SetStatus(podtest.MakePodStatus(
					podtest.SetContainerStatuses(
						podtest.MakeContainerStatus("container1",
							api.ResourceList{
								api.ResourceCPU:    resource.MustParse("100m"),
								api.ResourceMemory: resource.MustParse("1Gi"),
							}),
					),
				)),
			),
			expected: podtest.MakePod("test-pod",
				podtest.SetResourceVersion("2"),
				podtest.SetLabels(map[string]string{"foo": "bar"}),
				podtest.SetContainers(
					podtest.MakeContainer("container1",
						podtest.SetContainerResources(api.ResourceRequirements{
							Requests: api.ResourceList{
								api.ResourceCPU:    resource.MustParse("100m"),
								api.ResourceMemory: resource.MustParse("1Gi"),
							},
						}),
					),
				),
				podtest.SetStatus(podtest.MakePodStatus(
					podtest.SetContainerStatuses(
						podtest.MakeContainerStatus("container1",
							api.ResourceList{
								api.ResourceCPU:    resource.MustParse("100m"),
								api.ResourceMemory: resource.MustParse("1Gi"),
							}),
					),
				)),
			),
		},
		{
			name: "Update resources for sidecar container",
			oldPod: podtest.MakePod("test-pod",
				podtest.SetResourceVersion("1"),
				podtest.SetInitContainers(
					podtest.MakeContainer("init-container1",
						podtest.SetContainerRestartPolicy(containerRestartPolicyAlways),
						podtest.SetContainerResources(api.ResourceRequirements{
							Requests: api.ResourceList{
								api.ResourceCPU:    resource.MustParse("100m"),
								api.ResourceMemory: resource.MustParse("1Gi"),
							},
						}),
					),
				),
				podtest.SetStatus(podtest.MakePodStatus(
					podtest.SetContainerStatuses(
						podtest.MakeContainerStatus("init-container1",
							api.ResourceList{
								api.ResourceCPU:    resource.MustParse("100m"),
								api.ResourceMemory: resource.MustParse("1Gi"),
							}),
					),
				)),
			),
			newPod: podtest.MakePod("test-pod",
				podtest.SetResourceVersion("2"),
				podtest.SetInitContainers(
					podtest.MakeContainer("init-container1",
						podtest.SetContainerRestartPolicy(containerRestartPolicyAlways),
						podtest.SetContainerResources(api.ResourceRequirements{
							Requests: api.ResourceList{
								api.ResourceCPU:    resource.MustParse("200m"), // Updated resource
								api.ResourceMemory: resource.MustParse("4Gi"),  // Updated resource
							},
						}),
					),
				),
				podtest.SetStatus(podtest.MakePodStatus(
					podtest.SetContainerStatuses(
						podtest.MakeContainerStatus("init-container1",
							api.ResourceList{
								api.ResourceCPU:    resource.MustParse("100m"),
								api.ResourceMemory: resource.MustParse("1Gi"),
							}),
					),
				)),
			),
			expected: podtest.MakePod("test-pod",
				podtest.SetResourceVersion("2"),
				podtest.SetInitContainers(
					podtest.MakeContainer("init-container1",
						podtest.SetContainerRestartPolicy(containerRestartPolicyAlways),
						podtest.SetContainerResources(api.ResourceRequirements{
							Requests: api.ResourceList{
								api.ResourceCPU:    resource.MustParse("200m"), // Updated resource
								api.ResourceMemory: resource.MustParse("4Gi"),  // Updated resource
							},
						}),
					),
				),
				podtest.SetStatus(podtest.MakePodStatus(
					podtest.SetResizeStatus(api.PodResizeStatusProposed), // Resize status set
					podtest.SetContainerStatuses(
						podtest.MakeContainerStatus("init-container1",
							api.ResourceList{
								api.ResourceCPU:    resource.MustParse("100m"),
								api.ResourceMemory: resource.MustParse("1Gi"),
							}),
					),
				)),
			),
		},
		{
			name: "Update resources should fail for non-restartable init container",
			oldPod: podtest.MakePod("test-pod",
				podtest.SetResourceVersion("1"),
				podtest.SetInitContainers(
					podtest.MakeContainer("init-container1",
						podtest.SetContainerResources(api.ResourceRequirements{
							Requests: api.ResourceList{
								api.ResourceCPU:    resource.MustParse("100m"),
								api.ResourceMemory: resource.MustParse("1Gi"),
							},
						}),
					),
				),
				podtest.SetStatus(podtest.MakePodStatus(
					podtest.SetContainerStatuses(
						podtest.MakeContainerStatus("init-container1",
							api.ResourceList{
								api.ResourceCPU:    resource.MustParse("100m"),
								api.ResourceMemory: resource.MustParse("1Gi"),
							}),
					),
				)),
			),
			newPod: podtest.MakePod("test-pod",
				podtest.SetResourceVersion("2"),
				podtest.SetInitContainers(
					podtest.MakeContainer("init-container1",
						podtest.SetContainerResources(api.ResourceRequirements{
							Requests: api.ResourceList{
								api.ResourceCPU:    resource.MustParse("200m"), // Updated resource
								api.ResourceMemory: resource.MustParse("4Gi"),  // Updated resource
							},
						}),
					),
				),
				podtest.SetStatus(podtest.MakePodStatus(
					podtest.SetContainerStatuses(
						podtest.MakeContainerStatus("init-container1",
							api.ResourceList{
								api.ResourceCPU:    resource.MustParse("100m"),
								api.ResourceMemory: resource.MustParse("1Gi"),
							}),
					),
				)),
			),
			expected: podtest.MakePod("test-pod",
				podtest.SetResourceVersion("2"),
				podtest.SetInitContainers(
					podtest.MakeContainer("init-container1",
						podtest.SetContainerResources(api.ResourceRequirements{
							Requests: api.ResourceList{
								api.ResourceCPU:    resource.MustParse("200m"), // Updated resource
								api.ResourceMemory: resource.MustParse("4Gi"),  // Updated resource
							},
						}),
					),
				),
				podtest.SetStatus(podtest.MakePodStatus(
					podtest.SetResizeStatus(""), // Resize status not set
					podtest.SetContainerStatuses(
						podtest.MakeContainerStatus("init-container1",
							api.ResourceList{
								api.ResourceCPU:    resource.MustParse("100m"),
								api.ResourceMemory: resource.MustParse("1Gi"),
							}),
					),
				)),
			),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.InPlacePodVerticalScaling, true)
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.InPlacePodVerticalScalingAllocatedStatus, true)
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.SidecarContainers, true)
			ctx := context.Background()
			ResizeStrategy.PrepareForUpdate(ctx, tc.newPod, tc.oldPod)
			if !cmp.Equal(tc.expected, tc.newPod) {
				t.Errorf("ResizeStrategy.PrepareForUpdate() diff = %v", cmp.Diff(tc.expected, tc.newPod))
			}
		})
	}
}
