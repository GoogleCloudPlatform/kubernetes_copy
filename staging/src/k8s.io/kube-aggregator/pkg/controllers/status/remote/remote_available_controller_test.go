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

package remote

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/dump"
	v1listers "k8s.io/client-go/listers/core/v1"
	discoveryv1listers "k8s.io/client-go/listers/discovery/v1"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	apiregistration "k8s.io/kube-aggregator/pkg/apis/apiregistration/v1"
	"k8s.io/kube-aggregator/pkg/client/clientset_generated/clientset/fake"
	apiregistrationclient "k8s.io/kube-aggregator/pkg/client/clientset_generated/clientset/typed/apiregistration/v1"
	listers "k8s.io/kube-aggregator/pkg/client/listers/apiregistration/v1"
	availabilitymetrics "k8s.io/kube-aggregator/pkg/controllers/status/metrics"
	"k8s.io/utils/ptr"
)

const (
	testServicePort     int32 = 1234
	testServicePortName       = "testPort"
)

func newEndpointSlice(namespace, serviceName string) *discoveryv1.EndpointSlice {
	return &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      serviceName + "-xxx",
			Labels: map[string]string{
				discoveryv1.LabelServiceName: serviceName,
			},
		},
	}
}

func newEndpointSliceWithAddress(namespace, serviceName string, port int32, portName string) *discoveryv1.EndpointSlice {
	slice := newEndpointSlice(namespace, serviceName)
	slice.Endpoints = []discoveryv1.Endpoint{{
		Addresses: []string{"val"},
	}}
	slice.Ports = []discoveryv1.EndpointPort{{
		Name: &portName,
		Port: &port,
	}}
	return slice
}

func newUnreadyEndpointSliceWithAddress(namespace, serviceName string, port int32, portName string) *discoveryv1.EndpointSlice {
	slice := newEndpointSlice(namespace, serviceName)
	slice.Endpoints = []discoveryv1.Endpoint{{
		Addresses: []string{"val"},
		Conditions: discoveryv1.EndpointConditions{
			Ready: ptr.To(false),
		},
	}}
	slice.Ports = []discoveryv1.EndpointPort{{
		Name: &portName,
		Port: &port,
	}}
	return slice
}

func newService(namespace, name string, port int32, portName string) *v1.Service {
	return &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec: v1.ServiceSpec{
			Type: v1.ServiceTypeClusterIP,
			Ports: []v1.ServicePort{
				{Port: port, Name: portName},
			},
		},
	}
}

func newLocalAPIService(name string) *apiregistration.APIService {
	return &apiregistration.APIService{
		ObjectMeta: metav1.ObjectMeta{Name: name},
	}
}

func newRemoteAPIService(name string) *apiregistration.APIService {
	return &apiregistration.APIService{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: apiregistration.APIServiceSpec{
			Group:   strings.SplitN(name, ".", 2)[0],
			Version: strings.SplitN(name, ".", 2)[1],
			Service: &apiregistration.ServiceReference{
				Namespace: "foo",
				Name:      "bar",
				Port:      ptr.To(testServicePort),
			},
		},
	}
}

type T interface {
	Fatalf(format string, args ...interface{})
	Errorf(format string, args ...interface{})
}

func setupAPIServices(t T, apiServices []runtime.Object) (*AvailableConditionController, *fake.Clientset) {
	fakeClient := fake.NewSimpleClientset()
	apiServiceIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	serviceIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	endpointSliceIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})

	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer testServer.Close()

	for _, o := range apiServices {
		if err := apiServiceIndexer.Add(o); err != nil {
			t.Fatalf("failed to add APIService: %v", err)
		}
	}

	c := AvailableConditionController{
		apiServiceClient:    fakeClient.ApiregistrationV1(),
		apiServiceLister:    listers.NewAPIServiceLister(apiServiceIndexer),
		serviceLister:       v1listers.NewServiceLister(serviceIndexer),
		endpointSliceLister: discoveryv1listers.NewEndpointSliceLister(endpointSliceIndexer),
		serviceResolver:     &fakeServiceResolver{url: testServer.URL},
		queue: workqueue.NewTypedRateLimitingQueueWithConfig(
			// We want a fairly tight requeue time.  The controller listens to the API, but because it relies on the routability of the
			// service network, it is possible for an external, non-watchable factor to affect availability.  This keeps
			// the maximum disruption time to a minimum, but it does prevent hot loops.
			workqueue.NewTypedItemExponentialFailureRateLimiter[string](5*time.Millisecond, 30*time.Second),
			workqueue.TypedRateLimitingQueueConfig[string]{Name: "AvailableConditionController"},
		),
		metrics: availabilitymetrics.New(),
	}
	for _, svc := range apiServices {
		c.addAPIService(svc)
	}
	return &c, fakeClient
}

func BenchmarkBuildCache(b *testing.B) {
	apiServiceName := "remote.group"
	// model 1 APIService pointing at a given service, and 30 pointing at local group/versions
	apiServices := []runtime.Object{newRemoteAPIService(apiServiceName)}
	for i := 0; i < 30; i++ {
		apiServices = append(apiServices, newLocalAPIService(fmt.Sprintf("local.group%d", i)))
	}
	// model one service backing an API service, and 100 unrelated services
	services := []*v1.Service{newService("foo", "bar", testServicePort, testServicePortName)}
	for i := 0; i < 100; i++ {
		services = append(services, newService("foo", fmt.Sprintf("bar%d", i), testServicePort, testServicePortName))
	}
	c, _ := setupAPIServices(b, apiServices)
	b.ReportAllocs()
	b.ResetTimer()
	for n := 1; n <= b.N; n++ {
		for _, svc := range services {
			c.addService(svc)
		}
		for _, svc := range services {
			c.updateService(svc, svc)
		}
		for _, svc := range services {
			c.deleteService(svc)
		}
	}
}

func TestBuildCache(t *testing.T) {
	tests := []struct {
		name string

		apiServiceName string
		apiServices    []runtime.Object
		services       []*v1.Service

		expectedAvailability apiregistration.APIServiceCondition
	}{
		{
			name:           "api service",
			apiServiceName: "remote.group",
			apiServices:    []runtime.Object{newRemoteAPIService("remote.group")},
			services:       []*v1.Service{newService("foo", "bar", testServicePort, testServicePortName)},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, fakeClient := setupAPIServices(t, tc.apiServices)
			for _, svc := range tc.services {
				c.addService(svc)
			}

			c.sync(tc.apiServiceName)

			// ought to have one action writing status
			if e, a := 1, len(fakeClient.Actions()); e != a {
				t.Fatalf("%v expected %v, got %v", tc.name, e, fakeClient.Actions())
			}
		})
	}
}

func TestSync(t *testing.T) {
	tests := []struct {
		name string

		apiServiceName  string
		apiServices     []runtime.Object
		services        []*v1.Service
		endpointSlices  []*discoveryv1.EndpointSlice
		backendStatus   int
		backendLocation string

		expectedAvailability apiregistration.APIServiceCondition
		expectedSyncError    string
		expectedSkipped      bool
	}{
		{
			name:           "local",
			apiServiceName: "local.group",
			apiServices:    []runtime.Object{newLocalAPIService("local.group")},
			backendStatus:  http.StatusOK,
			expectedAvailability: apiregistration.APIServiceCondition{
				Type:    apiregistration.Available,
				Status:  apiregistration.ConditionTrue,
				Reason:  "Local",
				Message: "Local APIServices are always available",
			},
			expectedSkipped: true,
		},
		{
			name:           "no service",
			apiServiceName: "remote.group",
			apiServices:    []runtime.Object{newRemoteAPIService("remote.group")},
			services:       []*v1.Service{newService("foo", "not-bar", testServicePort, testServicePortName)},
			backendStatus:  http.StatusOK,
			expectedAvailability: apiregistration.APIServiceCondition{
				Type:    apiregistration.Available,
				Status:  apiregistration.ConditionFalse,
				Reason:  "ServiceNotFound",
				Message: `service/bar in "foo" is not present`,
			},
		},
		{
			name:           "service on bad port",
			apiServiceName: "remote.group",
			apiServices:    []runtime.Object{newRemoteAPIService("remote.group")},
			services: []*v1.Service{{
				ObjectMeta: metav1.ObjectMeta{Namespace: "foo", Name: "bar"},
				Spec: v1.ServiceSpec{
					Type: v1.ServiceTypeClusterIP,
					Ports: []v1.ServicePort{
						{Port: 6443},
					},
				},
			}},
			endpointSlices: []*discoveryv1.EndpointSlice{newEndpointSliceWithAddress("foo", "bar", testServicePort, testServicePortName)},
			backendStatus:  http.StatusOK,
			expectedAvailability: apiregistration.APIServiceCondition{
				Type:    apiregistration.Available,
				Status:  apiregistration.ConditionFalse,
				Reason:  "ServicePortError",
				Message: fmt.Sprintf(`service/bar in "foo" is not listening on port %d`, testServicePort),
			},
		},
		{
			name:           "no endpoints",
			apiServiceName: "remote.group",
			apiServices:    []runtime.Object{newRemoteAPIService("remote.group")},
			services:       []*v1.Service{newService("foo", "bar", testServicePort, testServicePortName)},
			backendStatus:  http.StatusOK,
			expectedAvailability: apiregistration.APIServiceCondition{
				Type:    apiregistration.Available,
				Status:  apiregistration.ConditionFalse,
				Reason:  "EndpointsNotFound",
				Message: `cannot find endpointslices for service/bar in "foo"`,
			},
		},
		{
			name:           "missing endpoints",
			apiServiceName: "remote.group",
			apiServices:    []runtime.Object{newRemoteAPIService("remote.group")},
			services:       []*v1.Service{newService("foo", "bar", testServicePort, testServicePortName)},
			endpointSlices: []*discoveryv1.EndpointSlice{newEndpointSlice("foo", "bar")},
			backendStatus:  http.StatusOK,
			expectedAvailability: apiregistration.APIServiceCondition{
				Type:    apiregistration.Available,
				Status:  apiregistration.ConditionFalse,
				Reason:  "MissingEndpoints",
				Message: `endpointslices for service/bar in "foo" have no addresses with port name "testPort"`,
			},
		},
		{
			name:           "wrong endpoint port name",
			apiServiceName: "remote.group",
			apiServices:    []runtime.Object{newRemoteAPIService("remote.group")},
			services:       []*v1.Service{newService("foo", "bar", testServicePort, testServicePortName)},
			endpointSlices: []*discoveryv1.EndpointSlice{newEndpointSliceWithAddress("foo", "bar", testServicePort, "wrongName")},
			backendStatus:  http.StatusOK,
			expectedAvailability: apiregistration.APIServiceCondition{
				Type:    apiregistration.Available,
				Status:  apiregistration.ConditionFalse,
				Reason:  "MissingEndpoints",
				Message: fmt.Sprintf(`endpointslices for service/bar in "foo" have no addresses with port name "%s"`, testServicePortName),
			},
		},
		{
			name:           "endpoints not ready",
			apiServiceName: "remote.group",
			apiServices:    []runtime.Object{newRemoteAPIService("remote.group")},
			services:       []*v1.Service{newService("foo", "bar", testServicePort, testServicePortName)},
			endpointSlices: []*discoveryv1.EndpointSlice{newUnreadyEndpointSliceWithAddress("foo", "bar", testServicePort, testServicePortName)},
			backendStatus:  http.StatusOK,
			expectedAvailability: apiregistration.APIServiceCondition{
				Type:    apiregistration.Available,
				Status:  apiregistration.ConditionFalse,
				Reason:  "MissingEndpoints",
				Message: fmt.Sprintf(`endpointslices for service/bar in "foo" have no addresses with port name "%s"`, testServicePortName),
			},
		},
		{
			name:           "remote",
			apiServiceName: "remote.group",
			apiServices:    []runtime.Object{newRemoteAPIService("remote.group")},
			services:       []*v1.Service{newService("foo", "bar", testServicePort, testServicePortName)},
			endpointSlices: []*discoveryv1.EndpointSlice{newEndpointSliceWithAddress("foo", "bar", testServicePort, testServicePortName)},
			backendStatus:  http.StatusOK,
			expectedAvailability: apiregistration.APIServiceCondition{
				Type:    apiregistration.Available,
				Status:  apiregistration.ConditionTrue,
				Reason:  "Passed",
				Message: `all checks passed`,
			},
		},
		{
			name:           "remote-bad-return",
			apiServiceName: "remote.group",
			apiServices:    []runtime.Object{newRemoteAPIService("remote.group")},
			services:       []*v1.Service{newService("foo", "bar", testServicePort, testServicePortName)},
			endpointSlices: []*discoveryv1.EndpointSlice{newEndpointSliceWithAddress("foo", "bar", testServicePort, testServicePortName)},
			backendStatus:  http.StatusForbidden,
			expectedAvailability: apiregistration.APIServiceCondition{
				Type:    apiregistration.Available,
				Status:  apiregistration.ConditionFalse,
				Reason:  "FailedDiscoveryCheck",
				Message: `failing or missing response from`,
			},
			expectedSyncError: "failing or missing response from",
		},
		{
			name:            "remote-redirect",
			apiServiceName:  "remote.group",
			apiServices:     []runtime.Object{newRemoteAPIService("remote.group")},
			services:        []*v1.Service{newService("foo", "bar", testServicePort, testServicePortName)},
			endpointSlices:  []*discoveryv1.EndpointSlice{newEndpointSliceWithAddress("foo", "bar", testServicePort, testServicePortName)},
			backendStatus:   http.StatusFound,
			backendLocation: "/test",
			expectedAvailability: apiregistration.APIServiceCondition{
				Type:    apiregistration.Available,
				Status:  apiregistration.ConditionFalse,
				Reason:  "FailedDiscoveryCheck",
				Message: `failing or missing response from`,
			},
			expectedSyncError: "failing or missing response from",
		},
		{
			name:           "remote-304",
			apiServiceName: "remote.group",
			apiServices:    []runtime.Object{newRemoteAPIService("remote.group")},
			services:       []*v1.Service{newService("foo", "bar", testServicePort, testServicePortName)},
			endpointSlices: []*discoveryv1.EndpointSlice{newEndpointSliceWithAddress("foo", "bar", testServicePort, testServicePortName)},
			backendStatus:  http.StatusNotModified,
			expectedAvailability: apiregistration.APIServiceCondition{
				Type:    apiregistration.Available,
				Status:  apiregistration.ConditionFalse,
				Reason:  "FailedDiscoveryCheck",
				Message: `failing or missing response from`,
			},
			expectedSyncError: "failing or missing response from",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fakeClient := fake.NewSimpleClientset(tc.apiServices...)
			apiServiceIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
			serviceIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
			endpointSliceIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
			for _, obj := range tc.apiServices {
				apiServiceIndexer.Add(obj) //nolint:errcheck
			}
			for _, obj := range tc.services {
				serviceIndexer.Add(obj) //nolint:errcheck
			}
			for _, obj := range tc.endpointSlices {
				endpointSliceIndexer.Add(obj) //nolint:errcheck
			}

			testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.backendLocation != "" {
					w.Header().Set("Location", tc.backendLocation)
				}
				w.WriteHeader(tc.backendStatus)
			}))
			defer testServer.Close()

			c := AvailableConditionController{
				apiServiceClient:           fakeClient.ApiregistrationV1(),
				apiServiceLister:           listers.NewAPIServiceLister(apiServiceIndexer),
				serviceLister:              v1listers.NewServiceLister(serviceIndexer),
				endpointSliceLister:        discoveryv1listers.NewEndpointSliceLister(endpointSliceIndexer),
				serviceResolver:            &fakeServiceResolver{url: testServer.URL},
				proxyCurrentCertKeyContent: func() ([]byte, []byte) { return emptyCert(), emptyCert() },
				metrics:                    availabilitymetrics.New(),
			}
			err := c.sync(tc.apiServiceName)
			if tc.expectedSyncError != "" {
				if err == nil {
					t.Fatalf("%v expected error with %q, got none", tc.name, tc.expectedSyncError)
				} else if !strings.Contains(err.Error(), tc.expectedSyncError) {
					t.Fatalf("%v expected error with %q, got %q", tc.name, tc.expectedSyncError, err.Error())
				}
			} else if err != nil {
				t.Fatalf("%v unexpected sync error: %v", tc.name, err)
			}

			if tc.expectedSkipped {
				if len(fakeClient.Actions()) > 0 {
					t.Fatalf("%v expected no actions, got %v", tc.name, fakeClient.Actions())
				}
				return
			}

			// ought to have one action writing status
			if e, a := 1, len(fakeClient.Actions()); e != a {
				t.Fatalf("%v expected %v, got %v", tc.name, e, fakeClient.Actions())
			}

			action, ok := fakeClient.Actions()[0].(clienttesting.UpdateAction)
			if !ok {
				t.Fatalf("%v got %v", tc.name, ok)
			}

			if e, a := 1, len(action.GetObject().(*apiregistration.APIService).Status.Conditions); e != a {
				t.Fatalf("%v expected %v, got %v", tc.name, e, action.GetObject())
			}
			condition := action.GetObject().(*apiregistration.APIService).Status.Conditions[0]
			if e, a := tc.expectedAvailability.Type, condition.Type; e != a {
				t.Errorf("%v expected %v, got %#v", tc.name, e, condition)
			}
			if e, a := tc.expectedAvailability.Status, condition.Status; e != a {
				t.Errorf("%v expected %v, got %#v", tc.name, e, condition)
			}
			if e, a := tc.expectedAvailability.Reason, condition.Reason; e != a {
				t.Errorf("%v expected %v, got %#v", tc.name, e, condition)
			}
			if e, a := tc.expectedAvailability.Message, condition.Message; !strings.HasPrefix(a, e) {
				t.Errorf("%v expected %v, got %#v", tc.name, e, condition)
			}
			if condition.LastTransitionTime.IsZero() {
				t.Error("expected lastTransitionTime to be non-zero")
			}
		})
	}
}

type fakeServiceResolver struct {
	url string
}

func (f *fakeServiceResolver) ResolveEndpoint(namespace, name string, port int32) (*url.URL, error) {
	return url.Parse(f.url)
}

func TestUpdateAPIServiceStatus(t *testing.T) {
	foo := &apiregistration.APIService{Status: apiregistration.APIServiceStatus{Conditions: []apiregistration.APIServiceCondition{{Type: "foo"}}}}
	bar := &apiregistration.APIService{Status: apiregistration.APIServiceStatus{Conditions: []apiregistration.APIServiceCondition{{Type: "bar"}}}}

	fakeClient := fake.NewSimpleClientset(foo)
	c := AvailableConditionController{
		apiServiceClient: fakeClient.ApiregistrationV1().(apiregistrationclient.APIServicesGetter),
		metrics:          availabilitymetrics.New(),
	}

	if _, err := c.updateAPIServiceStatus(foo, foo); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if e, a := 0, len(fakeClient.Actions()); e != a {
		t.Error(dump.Pretty(fakeClient.Actions()))
	}

	fakeClient.ClearActions()
	if _, err := c.updateAPIServiceStatus(foo, bar); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if e, a := 1, len(fakeClient.Actions()); e != a {
		t.Error(dump.Pretty(fakeClient.Actions()))
	}
}

func emptyCert() []byte {
	return []byte{}
}
