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
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	v1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/klog/v2/ktesting"
	"k8s.io/utils/pointer"
)

func TestNewEndpointSlice(t *testing.T) {
	ipAddressType := discovery.AddressTypeIPv4
	portName := "foo"
	protocol := v1.ProtocolTCP
	endpointMeta := endpointMeta{
		ports:       []discovery.EndpointPort{{Name: &portName, Protocol: &protocol}},
		addressType: ipAddressType,
	}
	service := v1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "foo", Namespace: "test"},
		Spec: v1.ServiceSpec{
			ClusterIP: "1.1.1.1",
			Ports:     []v1.ServicePort{{Port: 80}},
			Selector:  map[string]string{"foo": "bar"},
		},
	}

	gvk := schema.GroupVersionKind{Version: "v1", Kind: "Service"}
	ownerRef := metav1.NewControllerRef(&service, gvk)

	testCases := []struct {
		name          string
		updateSvc     func(svc v1.Service) v1.Service // given basic valid services, each test case can customize them
		expectedSlice *discovery.EndpointSlice
	}{
		{
			name: "Service without labels",
			updateSvc: func(svc v1.Service) v1.Service {
				return svc
			},
			expectedSlice: &discovery.EndpointSlice{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						discovery.LabelServiceName: service.Name,
						discovery.LabelManagedBy:   controllerName,
					},
					GenerateName:    fmt.Sprintf("%s-", service.Name),
					OwnerReferences: []metav1.OwnerReference{*ownerRef},
					Namespace:       service.Namespace,
				},
				Ports:       endpointMeta.ports,
				AddressType: endpointMeta.addressType,
				Endpoints:   []discovery.Endpoint{},
			},
		},
		{
			name: "Service with labels",
			updateSvc: func(svc v1.Service) v1.Service {
				labels := map[string]string{"foo": "bar"}
				svc.Labels = labels
				return svc
			},
			expectedSlice: &discovery.EndpointSlice{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						discovery.LabelServiceName: service.Name,
						discovery.LabelManagedBy:   controllerName,
						"foo":                      "bar",
					},
					GenerateName:    fmt.Sprintf("%s-", service.Name),
					OwnerReferences: []metav1.OwnerReference{*ownerRef},
					Namespace:       service.Namespace,
				},
				Ports:       endpointMeta.ports,
				AddressType: endpointMeta.addressType,
				Endpoints:   []discovery.Endpoint{},
			},
		},
		{
			name: "Headless Service with labels",
			updateSvc: func(svc v1.Service) v1.Service {
				labels := map[string]string{"foo": "bar"}
				svc.Labels = labels
				svc.Spec.ClusterIP = v1.ClusterIPNone
				return svc
			},
			expectedSlice: &discovery.EndpointSlice{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						discovery.LabelServiceName: service.Name,
						discovery.LabelManagedBy:   controllerName,
						v1.IsHeadlessService:       "",
						"foo":                      "bar",
					},
					GenerateName:    fmt.Sprintf("%s-", service.Name),
					OwnerReferences: []metav1.OwnerReference{*ownerRef},
					Namespace:       service.Namespace,
				},
				Ports:       endpointMeta.ports,
				AddressType: endpointMeta.addressType,
				Endpoints:   []discovery.Endpoint{},
			},
		},
		{
			name: "Service with multiple labels",
			updateSvc: func(svc v1.Service) v1.Service {
				labels := map[string]string{"foo": "bar", "foo2": "bar2"}
				svc.Labels = labels
				return svc
			},
			expectedSlice: &discovery.EndpointSlice{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						discovery.LabelServiceName: service.Name,
						discovery.LabelManagedBy:   controllerName,
						"foo":                      "bar",
						"foo2":                     "bar2",
					},
					GenerateName:    fmt.Sprintf("%s-", service.Name),
					OwnerReferences: []metav1.OwnerReference{*ownerRef},
					Namespace:       service.Namespace,
				},
				Ports:       endpointMeta.ports,
				AddressType: endpointMeta.addressType,
				Endpoints:   []discovery.Endpoint{},
			},
		},
		{
			name: "Evil service hijacking endpoint slices labels",
			updateSvc: func(svc v1.Service) v1.Service {
				labels := map[string]string{
					discovery.LabelServiceName: "bad",
					discovery.LabelManagedBy:   "actor",
					"foo":                      "bar",
				}
				svc.Labels = labels
				return svc
			},
			expectedSlice: &discovery.EndpointSlice{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						discovery.LabelServiceName: service.Name,
						discovery.LabelManagedBy:   controllerName,
						"foo":                      "bar",
					},
					GenerateName:    fmt.Sprintf("%s-", service.Name),
					OwnerReferences: []metav1.OwnerReference{*ownerRef},
					Namespace:       service.Namespace,
				},
				Ports:       endpointMeta.ports,
				AddressType: endpointMeta.addressType,
				Endpoints:   []discovery.Endpoint{},
			},
		},
		{
			name: "Service with annotations",
			updateSvc: func(svc v1.Service) v1.Service {
				annotations := map[string]string{"foo": "bar"}
				svc.Annotations = annotations
				return svc
			},
			expectedSlice: &discovery.EndpointSlice{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						discovery.LabelServiceName: service.Name,
						discovery.LabelManagedBy:   controllerName,
					},
					GenerateName:    fmt.Sprintf("%s-", service.Name),
					OwnerReferences: []metav1.OwnerReference{*ownerRef},
					Namespace:       service.Namespace,
				},
				Ports:       endpointMeta.ports,
				AddressType: endpointMeta.addressType,
				Endpoints:   []discovery.Endpoint{},
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			logger, _ := ktesting.NewTestContext(t)
			svc := tc.updateSvc(service)
			generatedSlice := newEndpointSlice(logger, &svc, &endpointMeta, controllerName)
			assert.EqualValues(t, tc.expectedSlice, generatedSlice)
		})
	}

}

func TestPodToEndpoint(t *testing.T) {
	ns := "test"
	svc, _ := newServiceAndEndpointMeta("foo", ns)
	svcPublishNotReady, _ := newServiceAndEndpointMeta("publishnotready", ns)
	svcPublishNotReady.Spec.PublishNotReadyAddresses = true

	readyPod := newPod(1, ns, true, 1, false)
	readyTerminatingPod := newPod(1, ns, true, 1, true)
	readyPodHostname := newPod(1, ns, true, 1, false)
	readyPodHostname.Spec.Subdomain = svc.Name
	readyPodHostname.Spec.Hostname = "example-hostname"

	unreadyPod := newPod(1, ns, false, 1, false)
	unreadyTerminatingPod := newPod(1, ns, false, 1, true)
	multiIPPod := newPod(1, ns, true, 1, false)
	multiIPPod.Status.PodIPs = []v1.PodIP{{IP: "1.2.3.4"}, {IP: "1234::5678:0000:0000:9abc:def0"}}

	node1 := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: readyPod.Spec.NodeName,
			Labels: map[string]string{
				"topology.kubernetes.io/zone":   "us-central1-a",
				"topology.kubernetes.io/region": "us-central1",
			},
		},
	}

	testCases := []struct {
		name                     string
		pod                      *v1.Pod
		node                     *v1.Node
		svc                      *v1.Service
		expectedEndpoint         discovery.Endpoint
		publishNotReadyAddresses bool
	}{
		{
			name: "Ready pod",
			pod:  readyPod,
			svc:  &svc,
			expectedEndpoint: discovery.Endpoint{
				Addresses: []string{"1.2.3.5"},
				Conditions: discovery.EndpointConditions{
					Ready:       pointer.Bool(true),
					Serving:     pointer.Bool(true),
					Terminating: pointer.Bool(false),
				},
				NodeName: pointer.String("node-1"),
				TargetRef: &v1.ObjectReference{
					Kind:      "Pod",
					Namespace: ns,
					Name:      readyPod.Name,
					UID:       readyPod.UID,
				},
			},
		},
		{
			name: "Ready pod + publishNotReadyAddresses",
			pod:  readyPod,
			svc:  &svcPublishNotReady,
			expectedEndpoint: discovery.Endpoint{
				Addresses: []string{"1.2.3.5"},
				Conditions: discovery.EndpointConditions{
					Ready:       pointer.Bool(true),
					Serving:     pointer.Bool(true),
					Terminating: pointer.Bool(false),
				},
				NodeName: pointer.String("node-1"),
				TargetRef: &v1.ObjectReference{
					Kind:      "Pod",
					Namespace: ns,
					Name:      readyPod.Name,
					UID:       readyPod.UID,
				},
			},
		},
		{
			name: "Unready pod",
			pod:  unreadyPod,
			svc:  &svc,
			expectedEndpoint: discovery.Endpoint{
				Addresses: []string{"1.2.3.5"},
				Conditions: discovery.EndpointConditions{
					Ready:       pointer.Bool(false),
					Serving:     pointer.Bool(false),
					Terminating: pointer.Bool(false),
				},
				NodeName: pointer.String("node-1"),
				TargetRef: &v1.ObjectReference{
					Kind:      "Pod",
					Namespace: ns,
					Name:      readyPod.Name,
					UID:       readyPod.UID,
				},
			},
		},
		{
			name: "Unready pod + publishNotReadyAddresses",
			pod:  unreadyPod,
			svc:  &svcPublishNotReady,
			expectedEndpoint: discovery.Endpoint{
				Addresses: []string{"1.2.3.5"},
				Conditions: discovery.EndpointConditions{
					Ready:       pointer.Bool(true),
					Serving:     pointer.Bool(false),
					Terminating: pointer.Bool(false),
				},
				NodeName: pointer.String("node-1"),
				TargetRef: &v1.ObjectReference{
					Kind:      "Pod",
					Namespace: ns,
					Name:      readyPod.Name,
					UID:       readyPod.UID,
				},
			},
		},
		{
			name: "Ready pod + node labels",
			pod:  readyPod,
			node: node1,
			svc:  &svc,
			expectedEndpoint: discovery.Endpoint{
				Addresses: []string{"1.2.3.5"},
				Conditions: discovery.EndpointConditions{
					Ready:       pointer.Bool(true),
					Serving:     pointer.Bool(true),
					Terminating: pointer.Bool(false),
				},
				Zone:     pointer.String("us-central1-a"),
				NodeName: pointer.String("node-1"),
				TargetRef: &v1.ObjectReference{
					Kind:      "Pod",
					Namespace: ns,
					Name:      readyPod.Name,
					UID:       readyPod.UID,
				},
			},
		},
		{
			name: "Multi IP Ready pod + node labels",
			pod:  multiIPPod,
			node: node1,
			svc:  &svc,
			expectedEndpoint: discovery.Endpoint{
				Addresses: []string{"1.2.3.4"},
				Conditions: discovery.EndpointConditions{
					Ready:       pointer.Bool(true),
					Serving:     pointer.Bool(true),
					Terminating: pointer.Bool(false),
				},
				Zone:     pointer.String("us-central1-a"),
				NodeName: pointer.String("node-1"),
				TargetRef: &v1.ObjectReference{
					Kind:      "Pod",
					Namespace: ns,
					Name:      readyPod.Name,
					UID:       readyPod.UID,
				},
			},
		},
		{
			name: "Ready pod + hostname",
			pod:  readyPodHostname,
			node: node1,
			svc:  &svc,
			expectedEndpoint: discovery.Endpoint{
				Addresses: []string{"1.2.3.5"},
				Conditions: discovery.EndpointConditions{
					Ready:       pointer.Bool(true),
					Serving:     pointer.Bool(true),
					Terminating: pointer.Bool(false),
				},
				Hostname: &readyPodHostname.Spec.Hostname,
				Zone:     pointer.String("us-central1-a"),
				NodeName: pointer.String("node-1"),
				TargetRef: &v1.ObjectReference{
					Kind:      "Pod",
					Namespace: ns,
					Name:      readyPodHostname.Name,
					UID:       readyPodHostname.UID,
				},
			},
		},
		{
			name: "Ready pod",
			pod:  readyPod,
			svc:  &svc,
			expectedEndpoint: discovery.Endpoint{
				Addresses: []string{"1.2.3.5"},
				Conditions: discovery.EndpointConditions{
					Ready:       pointer.Bool(true),
					Serving:     pointer.Bool(true),
					Terminating: pointer.Bool(false),
				},
				NodeName: pointer.String("node-1"),
				TargetRef: &v1.ObjectReference{
					Kind:      "Pod",
					Namespace: ns,
					Name:      readyPod.Name,
					UID:       readyPod.UID,
				},
			},
		},
		{
			name: "Ready terminating pod",
			pod:  readyTerminatingPod,
			svc:  &svc,
			expectedEndpoint: discovery.Endpoint{
				Addresses: []string{"1.2.3.5"},
				Conditions: discovery.EndpointConditions{
					Ready:       pointer.Bool(false),
					Serving:     pointer.Bool(true),
					Terminating: pointer.Bool(true),
				},
				NodeName: pointer.String("node-1"),
				TargetRef: &v1.ObjectReference{
					Kind:      "Pod",
					Namespace: ns,
					Name:      readyPod.Name,
					UID:       readyPod.UID,
				},
			},
		},
		{
			name: "Not ready terminating pod",
			pod:  unreadyTerminatingPod,
			svc:  &svc,
			expectedEndpoint: discovery.Endpoint{
				Addresses: []string{"1.2.3.5"},
				Conditions: discovery.EndpointConditions{
					Ready:       pointer.Bool(false),
					Serving:     pointer.Bool(false),
					Terminating: pointer.Bool(true),
				},
				NodeName: pointer.String("node-1"),
				TargetRef: &v1.ObjectReference{
					Kind:      "Pod",
					Namespace: ns,
					Name:      readyPod.Name,
					UID:       readyPod.UID,
				},
			},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			endpoint := podToEndpoint(testCase.pod, testCase.node, testCase.svc, discovery.AddressTypeIPv4)
			if !reflect.DeepEqual(testCase.expectedEndpoint, endpoint) {
				t.Errorf("Expected endpoint: %+v, got: %+v", testCase.expectedEndpoint, endpoint)
			}
		})
	}
}

func TestServiceControllerKey(t *testing.T) {
	testCases := map[string]struct {
		endpointSlice *discovery.EndpointSlice
		expectedKey   string
		expectedErr   error
	}{
		"nil EndpointSlice": {
			endpointSlice: nil,
			expectedKey:   "",
			expectedErr:   fmt.Errorf("nil EndpointSlice passed to ServiceControllerKey()"),
		},
		"empty EndpointSlice": {
			endpointSlice: &discovery.EndpointSlice{},
			expectedKey:   "",
			expectedErr:   fmt.Errorf("EndpointSlice missing kubernetes.io/service-name label"),
		},
		"valid EndpointSlice": {
			endpointSlice: &discovery.EndpointSlice{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "ns",
					Labels: map[string]string{
						discovery.LabelServiceName: "svc",
					},
				},
			},
			expectedKey: "ns/svc",
			expectedErr: nil,
		},
	}

	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			actualKey, actualErr := ServiceControllerKey(tc.endpointSlice)
			if !reflect.DeepEqual(actualErr, tc.expectedErr) {
				t.Errorf("Expected %s, got %s", tc.expectedErr, actualErr)
			}
			if actualKey != tc.expectedKey {
				t.Errorf("Expected %s, got %s", tc.expectedKey, actualKey)
			}
		})
	}
}

func TestGetEndpointPorts(t *testing.T) {
	protoTCP := v1.ProtocolTCP
	restartPolicyAlways := v1.ContainerRestartPolicyAlways

	testCases := map[string]struct {
		service       *v1.Service
		pod           *v1.Pod
		expectedPorts []*discovery.EndpointPort
	}{
		"service with AppProtocol on one port": {
			service: &v1.Service{
				Spec: v1.ServiceSpec{
					Ports: []v1.ServicePort{{
						Name:        "http",
						Port:        80,
						TargetPort:  intstr.FromInt32(80),
						Protocol:    protoTCP,
						AppProtocol: pointer.String("example.com/custom-protocol"),
					}},
				},
			},
			pod: &v1.Pod{
				Spec: v1.PodSpec{
					Containers: []v1.Container{{
						Ports: []v1.ContainerPort{},
					}},
				},
			},
			expectedPorts: []*discovery.EndpointPort{{
				Name:        pointer.String("http"),
				Port:        pointer.Int32(80),
				Protocol:    &protoTCP,
				AppProtocol: pointer.String("example.com/custom-protocol"),
			}},
		},
		"service with named port and AppProtocol on one port": {
			service: &v1.Service{
				Spec: v1.ServiceSpec{
					Ports: []v1.ServicePort{{
						Name:       "http",
						Port:       80,
						TargetPort: intstr.FromInt32(80),
						Protocol:   protoTCP,
					}, {
						Name:        "https",
						Protocol:    protoTCP,
						TargetPort:  intstr.FromString("https"),
						AppProtocol: pointer.String("https"),
					}},
				},
			},
			pod: &v1.Pod{
				Spec: v1.PodSpec{
					Containers: []v1.Container{{
						Ports: []v1.ContainerPort{{
							Name:          "https",
							ContainerPort: int32(443),
							Protocol:      protoTCP,
						}},
					}},
				},
			},
			expectedPorts: []*discovery.EndpointPort{{
				Name:     pointer.String("http"),
				Port:     pointer.Int32(80),
				Protocol: &protoTCP,
			}, {
				Name:        pointer.String("https"),
				Port:        pointer.Int32(443),
				Protocol:    &protoTCP,
				AppProtocol: pointer.String("https"),
			}},
		},
		"service with named port for restartable init container": {
			service: &v1.Service{
				Spec: v1.ServiceSpec{
					Ports: []v1.ServicePort{{
						Name:       "http-sidecar",
						Port:       8080,
						TargetPort: intstr.FromInt32(8080),
						Protocol:   protoTCP,
					}, {
						Name:       "http",
						Port:       8090,
						TargetPort: intstr.FromString("http"),
						Protocol:   protoTCP,
					}},
				},
			},
			pod: &v1.Pod{
				Spec: v1.PodSpec{
					InitContainers: []v1.Container{{
						Ports: []v1.ContainerPort{{
							Name:          "http-sidecar",
							ContainerPort: int32(8080),
							Protocol:      protoTCP,
						}},
						RestartPolicy: &restartPolicyAlways,
					}},
					Containers: []v1.Container{{
						Ports: []v1.ContainerPort{{
							Name:          "http",
							ContainerPort: int32(8090),
							Protocol:      protoTCP,
						}},
					}},
				},
			},
			expectedPorts: []*discovery.EndpointPort{{
				Name:     pointer.String("http-sidecar"),
				Port:     pointer.Int32(8080),
				Protocol: &protoTCP,
			}, {
				Name:     pointer.String("http"),
				Port:     pointer.Int32(8090),
				Protocol: &protoTCP,
			}},
		},
		"service with same named port for regular and restartable init container": {
			service: &v1.Service{
				Spec: v1.ServiceSpec{
					Ports: []v1.ServicePort{
						{
							Name:       "http",
							Port:       80,
							TargetPort: intstr.FromString("http"),
							Protocol:   protoTCP,
						}},
				},
			},
			pod: &v1.Pod{
				Spec: v1.PodSpec{
					InitContainers: []v1.Container{{
						Ports: []v1.ContainerPort{{
							Name:          "http",
							ContainerPort: int32(8080),
							Protocol:      protoTCP,
						}},
						RestartPolicy: &restartPolicyAlways,
					}},
					Containers: []v1.Container{{
						Ports: []v1.ContainerPort{{
							Name:          "http",
							ContainerPort: int32(8090),
							Protocol:      protoTCP,
						}},
					}},
				},
			},
			expectedPorts: []*discovery.EndpointPort{{
				Name:     pointer.String("http"),
				Port:     pointer.Int32(8090),
				Protocol: &protoTCP,
			}},
		},
	}

	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			logger, _ := ktesting.NewTestContext(t)
			actualPorts := getEndpointPorts(logger, tc.service, tc.pod)

			if len(actualPorts) != len(tc.expectedPorts) {
				t.Fatalf("Expected %d ports, got %d", len(tc.expectedPorts), len(actualPorts))
			}

			for i, actualPort := range actualPorts {
				if !reflect.DeepEqual(&actualPort, tc.expectedPorts[i]) {
					t.Errorf("Expected port: %+v, got %+v", tc.expectedPorts[i], &actualPort)
				}
			}
		})
	}
}

func TestSetEndpointSliceLabels(t *testing.T) {

	service := v1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "foo", Namespace: "test"},
		Spec: v1.ServiceSpec{
			Ports:     []v1.ServicePort{{Port: 80}},
			Selector:  map[string]string{"foo": "bar"},
			ClusterIP: "1.1.1.1",
		},
	}

	testCases := []struct {
		name           string
		epSlice        *discovery.EndpointSlice
		updateSvc      func(svc v1.Service) v1.Service // given basic valid services, each test case can customize them
		expectedLabels map[string]string
		expectedUpdate bool
	}{
		{
			name:    "Service without labels and empty endpoint slice",
			epSlice: &discovery.EndpointSlice{},
			updateSvc: func(svc v1.Service) v1.Service {
				return svc
			},
			expectedLabels: map[string]string{
				discovery.LabelServiceName: service.Name,
				discovery.LabelManagedBy:   controllerName,
			},
			expectedUpdate: false,
		},
		{
			name:    "Headless service with labels and empty endpoint slice",
			epSlice: &discovery.EndpointSlice{},
			updateSvc: func(svc v1.Service) v1.Service {
				labels := map[string]string{"foo": "bar"}
				svc.Spec.ClusterIP = v1.ClusterIPNone
				svc.Labels = labels
				return svc
			},
			expectedLabels: map[string]string{
				discovery.LabelServiceName: service.Name,
				discovery.LabelManagedBy:   controllerName,
				v1.IsHeadlessService:       "",
				"foo":                      "bar",
			},
			expectedUpdate: true,
		},
		{
			name:    "Headless service without labels and empty endpoint slice",
			epSlice: &discovery.EndpointSlice{},
			updateSvc: func(svc v1.Service) v1.Service {
				svc.Spec.ClusterIP = v1.ClusterIPNone
				return svc
			},
			expectedLabels: map[string]string{
				discovery.LabelServiceName: service.Name,
				discovery.LabelManagedBy:   controllerName,
				v1.IsHeadlessService:       "",
			},
			expectedUpdate: false,
		},
		{
			name:    "Non Headless service with Headless label and empty endpoint slice",
			epSlice: &discovery.EndpointSlice{},
			updateSvc: func(svc v1.Service) v1.Service {
				labels := map[string]string{v1.IsHeadlessService: ""}
				svc.Labels = labels
				return svc
			},
			expectedLabels: map[string]string{
				discovery.LabelServiceName: service.Name,
				discovery.LabelManagedBy:   controllerName,
			},
			expectedUpdate: false,
		},
		{
			name: "Headless Service change to ClusterIP Service with headless label",
			epSlice: &discovery.EndpointSlice{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						discovery.LabelServiceName: service.Name,
						discovery.LabelManagedBy:   controllerName,
						v1.IsHeadlessService:       "",
					},
				},
			},
			updateSvc: func(svc v1.Service) v1.Service {
				labels := map[string]string{v1.IsHeadlessService: ""}
				svc.Labels = labels
				return svc
			},
			expectedLabels: map[string]string{
				discovery.LabelServiceName: service.Name,
				discovery.LabelManagedBy:   controllerName,
			},
			expectedUpdate: false,
		},
		{
			name: "Headless Service change to ClusterIP Service",
			epSlice: &discovery.EndpointSlice{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						discovery.LabelServiceName: service.Name,
						discovery.LabelManagedBy:   controllerName,
						v1.IsHeadlessService:       "",
					},
				},
			},
			updateSvc: func(svc v1.Service) v1.Service {
				return svc
			},
			expectedLabels: map[string]string{
				discovery.LabelServiceName: service.Name,
				discovery.LabelManagedBy:   controllerName,
			},
			expectedUpdate: false,
		},
		{
			name: "Headless service and endpoint slice with same labels",
			epSlice: &discovery.EndpointSlice{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						discovery.LabelServiceName: service.Name,
						discovery.LabelManagedBy:   controllerName,
						"foo":                      "bar",
					},
				},
			}, updateSvc: func(svc v1.Service) v1.Service {
				labels := map[string]string{"foo": "bar"}
				svc.Spec.ClusterIP = v1.ClusterIPNone
				svc.Labels = labels
				return svc
			},
			expectedLabels: map[string]string{
				discovery.LabelServiceName: service.Name,
				discovery.LabelManagedBy:   controllerName,
				v1.IsHeadlessService:       "",
				"foo":                      "bar",
			},
			expectedUpdate: false,
		},
		{
			name:    "Service with labels and empty endpoint slice",
			epSlice: &discovery.EndpointSlice{},
			updateSvc: func(svc v1.Service) v1.Service {
				labels := map[string]string{"foo": "bar"}
				svc.Labels = labels
				return svc
			},
			expectedLabels: map[string]string{
				discovery.LabelServiceName: service.Name,
				discovery.LabelManagedBy:   controllerName,
				"foo":                      "bar",
			},
			expectedUpdate: true,
		},
		{
			name: "Slice with labels and service without labels",
			epSlice: &discovery.EndpointSlice{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						discovery.LabelServiceName: service.Name,
						discovery.LabelManagedBy:   controllerName,
						"foo":                      "bar",
					},
				},
			},
			updateSvc: func(svc v1.Service) v1.Service {
				return svc
			},
			expectedLabels: map[string]string{
				discovery.LabelServiceName: service.Name,
				discovery.LabelManagedBy:   controllerName,
			},
			expectedUpdate: true,
		},
		{
			name: "Slice with headless label and service with ClusterIP",
			epSlice: &discovery.EndpointSlice{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						discovery.LabelServiceName: service.Name,
						discovery.LabelManagedBy:   controllerName,
						v1.IsHeadlessService:       "",
					},
				},
			},
			updateSvc: func(svc v1.Service) v1.Service {
				return svc
			},
			expectedLabels: map[string]string{
				discovery.LabelServiceName: service.Name,
				discovery.LabelManagedBy:   controllerName,
			},
			expectedUpdate: false,
		},
		{
			name: "Slice with reserved labels and service with labels",
			epSlice: &discovery.EndpointSlice{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						discovery.LabelServiceName: service.Name,
						discovery.LabelManagedBy:   controllerName,
					},
				},
			},
			updateSvc: func(svc v1.Service) v1.Service {
				labels := map[string]string{"foo": "bar"}
				svc.Labels = labels
				return svc
			},
			expectedLabels: map[string]string{
				discovery.LabelServiceName: service.Name,
				discovery.LabelManagedBy:   controllerName,
				"foo":                      "bar",
			},
			expectedUpdate: true,
		},
		{
			name: "Evil service trying to hijack slice labels only well-known slice labels",
			epSlice: &discovery.EndpointSlice{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						discovery.LabelServiceName: service.Name,
						discovery.LabelManagedBy:   controllerName,
					},
				},
			},
			updateSvc: func(svc v1.Service) v1.Service {
				labels := map[string]string{
					discovery.LabelServiceName: "bad",
					discovery.LabelManagedBy:   "actor",
					v1.IsHeadlessService:       "invalid",
				}
				svc.Labels = labels
				return svc
			},
			expectedLabels: map[string]string{
				discovery.LabelServiceName: service.Name,
				discovery.LabelManagedBy:   controllerName,
			},
			expectedUpdate: false,
		},
		{
			name: "Evil service trying to hijack slice labels with updates",
			epSlice: &discovery.EndpointSlice{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						discovery.LabelServiceName: service.Name,
						discovery.LabelManagedBy:   controllerName,
					},
				},
			},
			updateSvc: func(svc v1.Service) v1.Service {
				labels := map[string]string{
					discovery.LabelServiceName: "bad",
					discovery.LabelManagedBy:   "actor",
					"foo":                      "bar",
				}
				svc.Labels = labels
				return svc
			},
			expectedLabels: map[string]string{
				discovery.LabelServiceName: service.Name,
				discovery.LabelManagedBy:   controllerName,
				"foo":                      "bar",
			},
			expectedUpdate: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			logger, _ := ktesting.NewTestContext(t)
			svc := tc.updateSvc(service)
			labels, updated := setEndpointSliceLabels(logger, tc.epSlice, &svc, controllerName)
			assert.EqualValues(t, tc.expectedUpdate, updated)
			assert.EqualValues(t, tc.expectedLabels, labels)
		})
	}

}

// Test helpers

func newPod(n int, namespace string, ready bool, nPorts int, terminating bool) *v1.Pod {
	status := v1.ConditionTrue
	if !ready {
		status = v1.ConditionFalse
	}

	var deletionTimestamp *metav1.Time
	if terminating {
		deletionTimestamp = &metav1.Time{
			Time: time.Now(),
		}
	}

	p := &v1.Pod{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1"},
		ObjectMeta: metav1.ObjectMeta{
			Namespace:         namespace,
			Name:              fmt.Sprintf("pod%d", n),
			Labels:            map[string]string{"foo": "bar"},
			DeletionTimestamp: deletionTimestamp,
			ResourceVersion:   fmt.Sprint(n),
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{{
				Name: "container-1",
			}},
			NodeName: "node-1",
		},
		Status: v1.PodStatus{
			PodIP: fmt.Sprintf("1.2.3.%d", 4+n),
			PodIPs: []v1.PodIP{{
				IP: fmt.Sprintf("1.2.3.%d", 4+n),
			}},
			Conditions: []v1.PodCondition{
				{
					Type:   v1.PodReady,
					Status: status,
				},
			},
		},
	}

	return p
}

func newClientset() *fake.Clientset {
	client := fake.NewSimpleClientset()

	client.PrependReactor("create", "endpointslices", k8stesting.ReactionFunc(func(action k8stesting.Action) (bool, runtime.Object, error) {
		endpointSlice := action.(k8stesting.CreateAction).GetObject().(*discovery.EndpointSlice)

		if endpointSlice.ObjectMeta.GenerateName != "" {
			endpointSlice.ObjectMeta.Name = fmt.Sprintf("%s-%s", endpointSlice.ObjectMeta.GenerateName, rand.String(8))
			endpointSlice.ObjectMeta.GenerateName = ""
		}
		endpointSlice.Generation = 1

		return false, endpointSlice, nil
	}))
	client.PrependReactor("update", "endpointslices", k8stesting.ReactionFunc(func(action k8stesting.Action) (bool, runtime.Object, error) {
		endpointSlice := action.(k8stesting.CreateAction).GetObject().(*discovery.EndpointSlice)
		endpointSlice.Generation++
		return false, endpointSlice, nil
	}))

	return client
}

func newServiceAndEndpointMeta(name, namespace string) (v1.Service, endpointMeta) {
	portNum := int32(80)
	portNameIntStr := intstr.IntOrString{
		Type:   intstr.Int,
		IntVal: portNum,
	}

	svc := v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			UID:       types.UID(namespace + "-" + name),
		},
		Spec: v1.ServiceSpec{
			Ports: []v1.ServicePort{{
				TargetPort: portNameIntStr,
				Protocol:   v1.ProtocolTCP,
				Name:       name,
			}},
			Selector:   map[string]string{"foo": "bar"},
			IPFamilies: []v1.IPFamily{v1.IPv4Protocol},
		},
	}

	addressType := discovery.AddressTypeIPv4
	protocol := v1.ProtocolTCP
	endpointMeta := endpointMeta{
		addressType: addressType,
		ports:       []discovery.EndpointPort{{Name: &name, Port: &portNum, Protocol: &protocol}},
	}

	return svc, endpointMeta
}

func newEmptyEndpointSlice(n int, namespace string, endpointMeta endpointMeta, svc v1.Service) *discovery.EndpointSlice {
	gvk := schema.GroupVersionKind{Version: "v1", Kind: "Service"}
	ownerRef := metav1.NewControllerRef(&svc, gvk)

	return &discovery.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:            fmt.Sprintf("%s-%d", svc.Name, n),
			Namespace:       namespace,
			OwnerReferences: []metav1.OwnerReference{*ownerRef},
		},
		Ports:       endpointMeta.ports,
		AddressType: endpointMeta.addressType,
		Endpoints:   []discovery.Endpoint{},
	}
}

func TestSupportedServiceAddressType(t *testing.T) {
	testCases := []struct {
		name                 string
		service              v1.Service
		expectedAddressTypes []discovery.AddressType
	}{
		{
			name:                 "v4 service with no ip families (cluster upgrade)",
			expectedAddressTypes: []discovery.AddressType{discovery.AddressTypeIPv4},
			service: v1.Service{
				Spec: v1.ServiceSpec{
					ClusterIP:  "10.0.0.10",
					IPFamilies: nil,
				},
			},
		},
		{
			name:                 "v6 service with no ip families (cluster upgrade)",
			expectedAddressTypes: []discovery.AddressType{discovery.AddressTypeIPv6},
			service: v1.Service{
				Spec: v1.ServiceSpec{
					ClusterIP:  "2000::1",
					IPFamilies: nil,
				},
			},
		},
		{
			name:                 "v4 service",
			expectedAddressTypes: []discovery.AddressType{discovery.AddressTypeIPv4},
			service: v1.Service{
				Spec: v1.ServiceSpec{
					IPFamilies: []v1.IPFamily{v1.IPv4Protocol},
				},
			},
		},
		{
			name:                 "v6 services",
			expectedAddressTypes: []discovery.AddressType{discovery.AddressTypeIPv6},
			service: v1.Service{
				Spec: v1.ServiceSpec{
					IPFamilies: []v1.IPFamily{v1.IPv6Protocol},
				},
			},
		},
		{
			name:                 "v4,v6 service",
			expectedAddressTypes: []discovery.AddressType{discovery.AddressTypeIPv4, discovery.AddressTypeIPv6},
			service: v1.Service{
				Spec: v1.ServiceSpec{
					IPFamilies: []v1.IPFamily{v1.IPv4Protocol, v1.IPv6Protocol},
				},
			},
		},
		{
			name:                 "v6,v4 service",
			expectedAddressTypes: []discovery.AddressType{discovery.AddressTypeIPv6, discovery.AddressTypeIPv4},
			service: v1.Service{
				Spec: v1.ServiceSpec{
					IPFamilies: []v1.IPFamily{v1.IPv6Protocol, v1.IPv4Protocol},
				},
			},
		},
		{
			name:                 "headless with no selector and no families (old api-server)",
			expectedAddressTypes: []discovery.AddressType{discovery.AddressTypeIPv6, discovery.AddressTypeIPv4},
			service: v1.Service{
				Spec: v1.ServiceSpec{
					ClusterIP:  v1.ClusterIPNone,
					IPFamilies: nil,
				},
			},
		},
		{
			name:                 "headless with selector and no families (old api-server)",
			expectedAddressTypes: []discovery.AddressType{discovery.AddressTypeIPv6, discovery.AddressTypeIPv4},
			service: v1.Service{
				Spec: v1.ServiceSpec{
					Selector:   map[string]string{"foo": "bar"},
					ClusterIP:  v1.ClusterIPNone,
					IPFamilies: nil,
				},
			},
		},

		{
			name:                 "headless with no selector with families",
			expectedAddressTypes: []discovery.AddressType{discovery.AddressTypeIPv4, discovery.AddressTypeIPv6},
			service: v1.Service{
				Spec: v1.ServiceSpec{
					ClusterIP:  v1.ClusterIPNone,
					IPFamilies: []v1.IPFamily{v1.IPv4Protocol, v1.IPv6Protocol},
				},
			},
		},
		{
			name:                 "headless with selector with families",
			expectedAddressTypes: []discovery.AddressType{discovery.AddressTypeIPv4, discovery.AddressTypeIPv6},
			service: v1.Service{
				Spec: v1.ServiceSpec{
					Selector:   map[string]string{"foo": "bar"},
					ClusterIP:  v1.ClusterIPNone,
					IPFamilies: []v1.IPFamily{v1.IPv4Protocol, v1.IPv6Protocol},
				},
			},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			logger, _ := ktesting.NewTestContext(t)
			addressTypes := getAddressTypesForService(logger, &testCase.service)
			if len(addressTypes) != len(testCase.expectedAddressTypes) {
				t.Fatalf("expected count address types %v got %v", len(testCase.expectedAddressTypes), len(addressTypes))
			}

			// compare
			for _, expectedAddressType := range testCase.expectedAddressTypes {
				found := false
				for key := range addressTypes {
					if key == expectedAddressType {
						found = true
						break

					}
				}
				if !found {
					t.Fatalf("expected address type %v was not found in the result", expectedAddressType)
				}
			}
		})
	}
}

func Test_hintsEnabled(t *testing.T) {
	testCases := []struct {
		name          string
		annotations   map[string]string
		expectEnabled bool
	}{{
		name:          "empty annotations",
		expectEnabled: false,
	}, {
		name:          "different annotations",
		annotations:   map[string]string{"topology-hints": "enabled"},
		expectEnabled: false,
	}, {
		name:          "hints annotation == enabled",
		annotations:   map[string]string{v1.DeprecatedAnnotationTopologyAwareHints: "enabled"},
		expectEnabled: false,
	}, {
		name:          "hints annotation == aUto",
		annotations:   map[string]string{v1.DeprecatedAnnotationTopologyAwareHints: "aUto"},
		expectEnabled: false,
	}, {
		name:          "hints annotation == auto",
		annotations:   map[string]string{v1.DeprecatedAnnotationTopologyAwareHints: "auto"},
		expectEnabled: true,
	}, {
		name:          "hints annotation == Auto",
		annotations:   map[string]string{v1.DeprecatedAnnotationTopologyAwareHints: "Auto"},
		expectEnabled: true,
	}, {
		name:          "hints annotation == disabled",
		annotations:   map[string]string{v1.DeprecatedAnnotationTopologyAwareHints: "disabled"},
		expectEnabled: false,
	}, {
		name:          "mode annotation == enabled",
		annotations:   map[string]string{v1.AnnotationTopologyMode: "enabled"},
		expectEnabled: false,
	}, {
		name:          "mode annotation == aUto",
		annotations:   map[string]string{v1.AnnotationTopologyMode: "aUto"},
		expectEnabled: false,
	}, {
		name:          "mode annotation == auto",
		annotations:   map[string]string{v1.AnnotationTopologyMode: "auto"},
		expectEnabled: true,
	}, {
		name:          "mode annotation == Auto",
		annotations:   map[string]string{v1.AnnotationTopologyMode: "Auto"},
		expectEnabled: true,
	}, {
		name:          "mode annotation == disabled",
		annotations:   map[string]string{v1.AnnotationTopologyMode: "disabled"},
		expectEnabled: false,
	}, {
		name:          "mode annotation == enabled",
		annotations:   map[string]string{v1.AnnotationTopologyMode: "enabled"},
		expectEnabled: false,
	}, {
		name:          "mode annotation == aUto",
		annotations:   map[string]string{v1.AnnotationTopologyMode: "aUto"},
		expectEnabled: false,
	}, {
		name:          "mode annotation == auto",
		annotations:   map[string]string{v1.AnnotationTopologyMode: "auto"},
		expectEnabled: true,
	}, {
		name:          "mode annotation == Auto",
		annotations:   map[string]string{v1.AnnotationTopologyMode: "Auto"},
		expectEnabled: true,
	}, {
		name:          "mode annotation == disabled",
		annotations:   map[string]string{v1.AnnotationTopologyMode: "disabled"},
		expectEnabled: false,
	}, {
		name:          "mode annotation == disabled, hints annotation == auto",
		annotations:   map[string]string{v1.AnnotationTopologyMode: "disabled", v1.DeprecatedAnnotationTopologyAwareHints: "auto"},
		expectEnabled: true,
	}, {
		name:          "mode annotation == auto, hints annotation == disabled",
		annotations:   map[string]string{v1.AnnotationTopologyMode: "auto", v1.DeprecatedAnnotationTopologyAwareHints: "disabled"},
		expectEnabled: false,
	}}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			actualEnabled := hintsEnabled(tc.annotations)
			if actualEnabled != tc.expectEnabled {
				t.Errorf("Expected %t, got %t", tc.expectEnabled, actualEnabled)
			}
		})
	}
}

func TestFindPort(t *testing.T) {
	restartAlways := v1.ContainerRestartPolicyAlways
	testCases := []struct {
		name           string
		containers     []v1.Container
		initContainers []v1.Container
		port           intstr.IntOrString
		expected       int
		pass           bool
	}{{
		name:       "valid int, no ports",
		containers: []v1.Container{{}},
		port:       intstr.FromInt32(93),
		expected:   93,
		pass:       true,
	}, {
		name: "valid int, with ports",
		containers: []v1.Container{{Ports: []v1.ContainerPort{{
			Name:          "",
			ContainerPort: 11,
			Protocol:      "TCP",
		}, {
			Name:          "p",
			ContainerPort: 22,
			Protocol:      "TCP",
		}}}},
		port:     intstr.FromInt32(93),
		expected: 93,
		pass:     true,
	}, {
		name:       "valid str, no ports",
		containers: []v1.Container{{}},
		port:       intstr.FromString("p"),
		expected:   0,
		pass:       false,
	}, {
		name: "valid str, one ctr with ports",
		containers: []v1.Container{{Ports: []v1.ContainerPort{{
			Name:          "",
			ContainerPort: 11,
			Protocol:      "UDP",
		}, {
			Name:          "p",
			ContainerPort: 22,
			Protocol:      "TCP",
		}, {
			Name:          "q",
			ContainerPort: 33,
			Protocol:      "TCP",
		}}}},
		port:     intstr.FromString("q"),
		expected: 33,
		pass:     true,
	}, {
		name: "valid str, two ctr with ports",
		containers: []v1.Container{{}, {Ports: []v1.ContainerPort{{
			Name:          "",
			ContainerPort: 11,
			Protocol:      "UDP",
		}, {
			Name:          "p",
			ContainerPort: 22,
			Protocol:      "TCP",
		}, {
			Name:          "q",
			ContainerPort: 33,
			Protocol:      "TCP",
		}}}},
		port:     intstr.FromString("q"),
		expected: 33,
		pass:     true,
	}, {
		name: "valid str, two ctr with same port",
		containers: []v1.Container{{}, {Ports: []v1.ContainerPort{{
			Name:          "",
			ContainerPort: 11,
			Protocol:      "UDP",
		}, {
			Name:          "p",
			ContainerPort: 22,
			Protocol:      "TCP",
		}, {
			Name:          "q",
			ContainerPort: 22,
			Protocol:      "TCP",
		}}}},
		port:     intstr.FromString("q"),
		expected: 22,
		pass:     true,
	}, {
		name: "valid str, invalid protocol",
		containers: []v1.Container{{}, {Ports: []v1.ContainerPort{{
			Name:          "a",
			ContainerPort: 11,
			Protocol:      "snmp",
		},
		}}},
		port:     intstr.FromString("a"),
		expected: 0,
		pass:     false,
	}, {
		name: "valid hostPort",
		containers: []v1.Container{{}, {Ports: []v1.ContainerPort{{
			Name:          "a",
			ContainerPort: 11,
			HostPort:      81,
			Protocol:      "TCP",
		},
		}}},
		port:     intstr.FromString("a"),
		expected: 11,
		pass:     true,
	},
		{
			name: "invalid hostPort",
			containers: []v1.Container{{}, {Ports: []v1.ContainerPort{{
				Name:          "a",
				ContainerPort: 11,
				HostPort:      -1,
				Protocol:      "TCP",
			},
			}}},
			port:     intstr.FromString("a"),
			expected: 11,
			pass:     true,
			// this should fail but passes.
		},
		{
			name: "invalid ContainerPort",
			containers: []v1.Container{{}, {Ports: []v1.ContainerPort{{
				Name:          "a",
				ContainerPort: -1,
				Protocol:      "TCP",
			},
			}}},
			port:     intstr.FromString("a"),
			expected: -1,
			pass:     true,
			// this should fail but passes
		},
		{
			name: "HostIP Address",
			containers: []v1.Container{{}, {Ports: []v1.ContainerPort{{
				Name:          "a",
				ContainerPort: 11,
				HostIP:        "192.168.1.1",
				Protocol:      "TCP",
			},
			}}},
			port:     intstr.FromString("a"),
			expected: 11,
			pass:     true,
		},
		{
			name: "Sidecar initContainer named port",
			initContainers: []v1.Container{{
				RestartPolicy: &restartAlways,
				Ports: []v1.ContainerPort{{
					Name:          "a",
					ContainerPort: 80,
					HostPort:      -1,
					Protocol:      "TCP",
				}},
			}},
			port:     intstr.FromString("a"),
			expected: 80,
			pass:     true,
		},
		{
			name: "Invalid(restartPolicy != Always) initContainer named port",
			initContainers: []v1.Container{{
				Ports: []v1.ContainerPort{{
					Name:          "a",
					ContainerPort: 80,
					HostPort:      -1,
					Protocol:      "TCP",
				}},
			}},
			port:     intstr.FromString("a"),
			expected: 0,
			pass:     false,
		},
		{
			name: "App and sidecar containers have the same named port, first app container port will be used",
			initContainers: []v1.Container{{
				RestartPolicy: &restartAlways,
				Ports: []v1.ContainerPort{{
					Name:          "a",
					ContainerPort: 80,
					HostPort:      -1,
					Protocol:      "TCP",
				}},
			}},
			containers: []v1.Container{{
				Ports: []v1.ContainerPort{{
					Name:          "a",
					ContainerPort: 81,
					HostPort:      -1,
					Protocol:      "TCP",
				}},
			}, {
				Ports: []v1.ContainerPort{{
					Name:          "a",
					ContainerPort: 82,
					HostPort:      -1,
					Protocol:      "TCP",
				}},
			}},
			port:     intstr.FromString("a"),
			expected: 81,
			pass:     true,
		},
	}

	for _, tc := range testCases {
		port, err := FindPort(&v1.Pod{Spec: v1.PodSpec{Containers: tc.containers, InitContainers: tc.initContainers}},
			&v1.ServicePort{Protocol: "TCP", TargetPort: tc.port})
		if err != nil && tc.pass {
			t.Errorf("unexpected error for %s: %v", tc.name, err)
		}
		if err == nil && !tc.pass {
			t.Errorf("unexpected non-error for %s: %d", tc.name, port)
		}
		if port != tc.expected {
			t.Errorf("wrong result for %s: expected %d, got %d", tc.name, tc.expected, port)
		}
	}
}
