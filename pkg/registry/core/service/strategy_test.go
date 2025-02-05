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

package service

import (
	"reflect"
	"testing"

	"github.com/google/go-cmp/cmp"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/version"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/registry/rest"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	api "k8s.io/kubernetes/pkg/apis/core"
	_ "k8s.io/kubernetes/pkg/apis/core/install"
	"k8s.io/kubernetes/pkg/features"
	"k8s.io/utils/ptr"
)

func TestCheckGeneratedNameError(t *testing.T) {
	ctx := genericapirequest.WithRequestInfo(genericapirequest.NewContext(), &genericapirequest.RequestInfo{
		Resource: "foos",
	})

	expect := errors.NewNotFound(api.Resource("foos"), "bar")
	if err := rest.CheckGeneratedNameError(ctx, Strategy, expect, &api.Service{}); err != expect {
		t.Errorf("NotFoundError should be ignored: %v", err)
	}

	expect = errors.NewAlreadyExists(api.Resource("foos"), "bar")
	if err := rest.CheckGeneratedNameError(ctx, Strategy, expect, &api.Service{}); err != expect {
		t.Errorf("AlreadyExists should be returned when no GenerateName field: %v", err)
	}

	expect = errors.NewAlreadyExists(api.Resource("foos"), "bar")
	if err := rest.CheckGeneratedNameError(ctx, Strategy, expect, &api.Service{ObjectMeta: metav1.ObjectMeta{GenerateName: "foo"}}); err == nil || !errors.IsAlreadyExists(err) {
		t.Errorf("expected try again later error: %v", err)
	}
}

func makeValidService() *api.Service {
	preferDual := api.IPFamilyPolicyPreferDualStack
	clusterInternalTrafficPolicy := api.ServiceInternalTrafficPolicyCluster

	return &api.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "valid",
			Namespace:       "default",
			Labels:          map[string]string{},
			Annotations:     map[string]string{},
			ResourceVersion: "1",
		},
		Spec: api.ServiceSpec{
			Selector:        map[string]string{"key": "val"},
			SessionAffinity: "None",
			Type:            api.ServiceTypeClusterIP,
			Ports: []api.ServicePort{
				makeValidServicePort("p", "TCP", 8675),
				makeValidServicePort("q", "TCP", 309),
			},
			ClusterIP:             "1.2.3.4",
			ClusterIPs:            []string{"1.2.3.4", "5:6:7::8"},
			IPFamilyPolicy:        &preferDual,
			IPFamilies:            []api.IPFamily{"IPv4", "IPv6"},
			InternalTrafficPolicy: &clusterInternalTrafficPolicy,
		},
	}
}

func makeValidServicePort(name string, proto api.Protocol, port int32) api.ServicePort {
	return api.ServicePort{
		Name:       name,
		Protocol:   proto,
		Port:       port,
		TargetPort: intstr.FromInt32(port),
	}
}

func makeValidServiceCustom(tweaks ...func(svc *api.Service)) *api.Service {
	svc := makeValidService()
	for _, fn := range tweaks {
		fn(svc)
	}
	return svc
}

func TestServiceStatusStrategy(t *testing.T) {
	ctx := genericapirequest.NewDefaultContext()
	if !StatusStrategy.NamespaceScoped() {
		t.Errorf("Service must be namespace scoped")
	}
	oldService := makeValidService()
	oldService.Spec.Type = api.ServiceTypeLoadBalancer
	oldService.ResourceVersion = "4"
	oldService.Spec.SessionAffinity = "None"
	newService := oldService.DeepCopy()
	newService.Spec.SessionAffinity = "ClientIP"
	newService.Status = api.ServiceStatus{
		LoadBalancer: api.LoadBalancerStatus{
			Ingress: []api.LoadBalancerIngress{
				{
					IP:     "127.0.0.2",
					IPMode: ptr.To(api.LoadBalancerIPModeVIP),
				},
			},
		},
	}
	StatusStrategy.PrepareForUpdate(ctx, newService, oldService)
	if newService.Status.LoadBalancer.Ingress[0].IP != "127.0.0.2" {
		t.Errorf("Service status updates should allow change of status fields")
	}
	if newService.Spec.SessionAffinity != "None" {
		t.Errorf("PrepareForUpdate should have preserved old spec")
	}
	errs := StatusStrategy.ValidateUpdate(ctx, newService, oldService)
	if len(errs) != 0 {
		t.Errorf("Unexpected error %v", errs)
	}

	warnings := StatusStrategy.WarningsOnUpdate(ctx, newService, oldService)
	if len(warnings) != 0 {
		t.Errorf("Unexpected warnings %v", errs)
	}

	// Bad IP warning (leading zeros)
	newService.Status.LoadBalancer.Ingress[0].IP = "127.000.000.002"
	warnings = StatusStrategy.WarningsOnUpdate(ctx, newService, oldService)
	if len(warnings) != 1 {
		t.Errorf("Did not get warning for bad IP")
	}
}

func makeServiceWithConditions(conditions []metav1.Condition) *api.Service {
	return &api.Service{
		Status: api.ServiceStatus{
			Conditions: conditions,
		},
	}
}

func makeServiceWithPorts(ports []api.PortStatus) *api.Service {
	return &api.Service{
		Status: api.ServiceStatus{
			LoadBalancer: api.LoadBalancerStatus{
				Ingress: []api.LoadBalancerIngress{
					{
						Ports: ports,
					},
				},
			},
		},
	}
}

func TestDropDisabledField(t *testing.T) {
	testCases := []struct {
		name       string
		svc        *api.Service
		oldSvc     *api.Service
		compareSvc *api.Service
	}{
		/* svc.Status.Conditions */
		{
			name:       "mixed protocol enabled, field not used in old, not used in new",
			svc:        makeServiceWithConditions(nil),
			oldSvc:     makeServiceWithConditions(nil),
			compareSvc: makeServiceWithConditions(nil),
		},
		{
			name:       "mixed protocol enabled, field used in old and in new",
			svc:        makeServiceWithConditions([]metav1.Condition{}),
			oldSvc:     makeServiceWithConditions([]metav1.Condition{}),
			compareSvc: makeServiceWithConditions([]metav1.Condition{}),
		},
		{
			name:       "mixed protocol enabled, field not used in old, used in new",
			svc:        makeServiceWithConditions([]metav1.Condition{}),
			oldSvc:     makeServiceWithConditions(nil),
			compareSvc: makeServiceWithConditions([]metav1.Condition{}),
		},
		{
			name:       "mixed protocol enabled, field used in old, not used in new",
			svc:        makeServiceWithConditions(nil),
			oldSvc:     makeServiceWithConditions([]metav1.Condition{}),
			compareSvc: makeServiceWithConditions(nil),
		},
		/* svc.Status.LoadBalancer.Ingress.Ports */
		{
			name:       "mixed protocol enabled, field not used in old, not used in new",
			svc:        makeServiceWithPorts(nil),
			oldSvc:     makeServiceWithPorts(nil),
			compareSvc: makeServiceWithPorts(nil),
		},
		{
			name:       "mixed protocol enabled, field used in old and in new",
			svc:        makeServiceWithPorts([]api.PortStatus{}),
			oldSvc:     makeServiceWithPorts([]api.PortStatus{}),
			compareSvc: makeServiceWithPorts([]api.PortStatus{}),
		},
		{
			name:       "mixed protocol enabled, field not used in old, used in new",
			svc:        makeServiceWithPorts([]api.PortStatus{}),
			oldSvc:     makeServiceWithPorts(nil),
			compareSvc: makeServiceWithPorts([]api.PortStatus{}),
		},
		{
			name:       "mixed protocol enabled, field used in old, not used in new",
			svc:        makeServiceWithPorts(nil),
			oldSvc:     makeServiceWithPorts([]api.PortStatus{}),
			compareSvc: makeServiceWithPorts(nil),
		},
		/* add more tests for other dropped fields as needed */
	}
	for _, tc := range testCases {
		func() {
			old := tc.oldSvc.DeepCopy()

			// to test against user using IPFamily not set on cluster
			dropServiceDisabledFields(tc.svc, tc.oldSvc)

			// old node should never be changed
			if !reflect.DeepEqual(tc.oldSvc, old) {
				t.Errorf("%v: old svc changed: %v", tc.name, cmp.Diff(tc.oldSvc, old))
			}

			if !reflect.DeepEqual(tc.svc, tc.compareSvc) {
				t.Errorf("%v: unexpected svc spec: %v", tc.name, cmp.Diff(tc.svc, tc.compareSvc))
			}
		}()
	}

}

func TestDropServiceStatusDisabledFields(t *testing.T) {
	ipModeVIP := api.LoadBalancerIPModeVIP
	ipModeProxy := api.LoadBalancerIPModeProxy

	testCases := []struct {
		name          string
		ipModeEnabled bool
		svc           *api.Service
		oldSvc        *api.Service
		compareSvc    *api.Service
	}{
		/*LoadBalancerIPMode disabled*/
		{
			name:          "LoadBalancerIPMode disabled, ipMode not used in old, not used in new",
			ipModeEnabled: false,
			svc: makeValidServiceCustom(func(svc *api.Service) {
				svc.Spec.Type = api.ServiceTypeLoadBalancer
				svc.Status.LoadBalancer = api.LoadBalancerStatus{
					Ingress: []api.LoadBalancerIngress{{
						IP: "1.2.3.4",
					}},
				}
			}),
			oldSvc: makeValidServiceCustom(func(svc *api.Service) {
				svc.Spec.Type = api.ServiceTypeLoadBalancer
				svc.Status.LoadBalancer = api.LoadBalancerStatus{}
			}),
			compareSvc: makeValidServiceCustom(func(svc *api.Service) {
				svc.Spec.Type = api.ServiceTypeLoadBalancer
				svc.Status.LoadBalancer = api.LoadBalancerStatus{
					Ingress: []api.LoadBalancerIngress{{
						IP: "1.2.3.4",
					}},
				}
			}),
		}, {
			name:          "LoadBalancerIPMode disabled, ipMode used in old and in new",
			ipModeEnabled: false,
			svc: makeValidServiceCustom(func(svc *api.Service) {
				svc.Spec.Type = api.ServiceTypeLoadBalancer
				svc.Status.LoadBalancer = api.LoadBalancerStatus{
					Ingress: []api.LoadBalancerIngress{{
						IP:     "1.2.3.4",
						IPMode: &ipModeProxy,
					}},
				}
			}),
			oldSvc: makeValidServiceCustom(func(svc *api.Service) {
				svc.Spec.Type = api.ServiceTypeLoadBalancer
				svc.Status.LoadBalancer = api.LoadBalancerStatus{
					Ingress: []api.LoadBalancerIngress{{
						IP:     "1.2.3.4",
						IPMode: &ipModeVIP,
					}},
				}
			}),
			compareSvc: makeValidServiceCustom(func(svc *api.Service) {
				svc.Spec.Type = api.ServiceTypeLoadBalancer
				svc.Status.LoadBalancer = api.LoadBalancerStatus{
					Ingress: []api.LoadBalancerIngress{{
						IP:     "1.2.3.4",
						IPMode: &ipModeProxy,
					}},
				}
			}),
		}, {
			name:          "LoadBalancerIPMode disabled, ipMode not used in old, used in new",
			ipModeEnabled: false,
			svc: makeValidServiceCustom(func(svc *api.Service) {
				svc.Spec.Type = api.ServiceTypeLoadBalancer
				svc.Status.LoadBalancer = api.LoadBalancerStatus{
					Ingress: []api.LoadBalancerIngress{{
						IP:     "1.2.3.4",
						IPMode: &ipModeVIP,
					}},
				}
			}),
			oldSvc: makeValidServiceCustom(func(svc *api.Service) {
				svc.Spec.Type = api.ServiceTypeLoadBalancer
				svc.Status.LoadBalancer = api.LoadBalancerStatus{
					Ingress: []api.LoadBalancerIngress{{
						IP: "1.2.3.4",
					}},
				}
			}),
			compareSvc: makeValidServiceCustom(func(svc *api.Service) {
				svc.Spec.Type = api.ServiceTypeLoadBalancer
				svc.Status.LoadBalancer = api.LoadBalancerStatus{
					Ingress: []api.LoadBalancerIngress{{
						IP: "1.2.3.4",
					}},
				}
			}),
		}, {
			name:          "LoadBalancerIPMode disabled, ipMode used in old, not used in new",
			ipModeEnabled: false,
			svc: makeValidServiceCustom(func(svc *api.Service) {
				svc.Spec.Type = api.ServiceTypeLoadBalancer
				svc.Status.LoadBalancer = api.LoadBalancerStatus{
					Ingress: []api.LoadBalancerIngress{{
						IP: "1.2.3.4",
					}},
				}
			}),
			oldSvc: makeValidServiceCustom(func(svc *api.Service) {
				svc.Spec.Type = api.ServiceTypeLoadBalancer
				svc.Status.LoadBalancer = api.LoadBalancerStatus{
					Ingress: []api.LoadBalancerIngress{{
						IP:     "1.2.3.4",
						IPMode: &ipModeProxy,
					}},
				}
			}),
			compareSvc: makeValidServiceCustom(func(svc *api.Service) {
				svc.Spec.Type = api.ServiceTypeLoadBalancer
				svc.Status.LoadBalancer = api.LoadBalancerStatus{
					Ingress: []api.LoadBalancerIngress{{
						IP: "1.2.3.4",
					}},
				}
			}),
		},
		/*LoadBalancerIPMode enabled*/
		{
			name:          "LoadBalancerIPMode enabled, ipMode not used in old, not used in new",
			ipModeEnabled: true,
			svc: makeValidServiceCustom(func(svc *api.Service) {
				svc.Spec.Type = api.ServiceTypeLoadBalancer
				svc.Status.LoadBalancer = api.LoadBalancerStatus{
					Ingress: []api.LoadBalancerIngress{{
						IP: "1.2.3.4",
					}},
				}
			}),
			oldSvc: nil,
			compareSvc: makeValidServiceCustom(func(svc *api.Service) {
				svc.Spec.Type = api.ServiceTypeLoadBalancer
				svc.Status.LoadBalancer = api.LoadBalancerStatus{
					Ingress: []api.LoadBalancerIngress{{
						IP: "1.2.3.4",
					}},
				}
			}),
		}, {
			name:          "LoadBalancerIPMode enabled, ipMode used in old and in new",
			ipModeEnabled: true,
			svc: makeValidServiceCustom(func(svc *api.Service) {
				svc.Spec.Type = api.ServiceTypeLoadBalancer
				svc.Status.LoadBalancer = api.LoadBalancerStatus{
					Ingress: []api.LoadBalancerIngress{{
						IP:     "1.2.3.4",
						IPMode: &ipModeProxy,
					}},
				}
			}),
			oldSvc: makeValidServiceCustom(func(svc *api.Service) {
				svc.Spec.Type = api.ServiceTypeLoadBalancer
				svc.Status.LoadBalancer = api.LoadBalancerStatus{
					Ingress: []api.LoadBalancerIngress{{
						IP:     "1.2.3.4",
						IPMode: &ipModeVIP,
					}},
				}
			}),
			compareSvc: makeValidServiceCustom(func(svc *api.Service) {
				svc.Spec.Type = api.ServiceTypeLoadBalancer
				svc.Status.LoadBalancer = api.LoadBalancerStatus{
					Ingress: []api.LoadBalancerIngress{{
						IP:     "1.2.3.4",
						IPMode: &ipModeProxy,
					}},
				}
			}),
		}, {
			name:          "LoadBalancerIPMode enabled, ipMode not used in old, used in new",
			ipModeEnabled: true,
			svc: makeValidServiceCustom(func(svc *api.Service) {
				svc.Spec.Type = api.ServiceTypeLoadBalancer
				svc.Status.LoadBalancer = api.LoadBalancerStatus{
					Ingress: []api.LoadBalancerIngress{{
						IP:     "1.2.3.4",
						IPMode: &ipModeVIP,
					}},
				}
			}),
			oldSvc: makeValidServiceCustom(func(svc *api.Service) {
				svc.Spec.Type = api.ServiceTypeLoadBalancer
				svc.Status.LoadBalancer = api.LoadBalancerStatus{
					Ingress: []api.LoadBalancerIngress{{
						IP: "1.2.3.4",
					}},
				}
			}),
			compareSvc: makeValidServiceCustom(func(svc *api.Service) {
				svc.Spec.Type = api.ServiceTypeLoadBalancer
				svc.Status.LoadBalancer = api.LoadBalancerStatus{
					Ingress: []api.LoadBalancerIngress{{
						IP:     "1.2.3.4",
						IPMode: &ipModeVIP,
					}},
				}
			}),
		}, {
			name:          "LoadBalancerIPMode enabled, ipMode used in old, not used in new",
			ipModeEnabled: true,
			svc: makeValidServiceCustom(func(svc *api.Service) {
				svc.Spec.Type = api.ServiceTypeLoadBalancer
				svc.Status.LoadBalancer = api.LoadBalancerStatus{
					Ingress: []api.LoadBalancerIngress{{
						IP: "1.2.3.4",
					}},
				}
			}),
			oldSvc: makeValidServiceCustom(func(svc *api.Service) {
				svc.Spec.Type = api.ServiceTypeLoadBalancer
				svc.Status.LoadBalancer = api.LoadBalancerStatus{
					Ingress: []api.LoadBalancerIngress{{
						IP:     "1.2.3.4",
						IPMode: &ipModeProxy,
					}},
				}
			}),
			compareSvc: makeValidServiceCustom(func(svc *api.Service) {
				svc.Spec.Type = api.ServiceTypeLoadBalancer
				svc.Status.LoadBalancer = api.LoadBalancerStatus{
					Ingress: []api.LoadBalancerIngress{{
						IP: "1.2.3.4",
					}},
				}
			}),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if !tc.ipModeEnabled {
				featuregatetesting.SetFeatureGateEmulationVersionDuringTest(t, utilfeature.DefaultFeatureGate, version.MustParse("1.31"))
			}
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.LoadBalancerIPMode, tc.ipModeEnabled)
			dropServiceStatusDisabledFields(tc.svc, tc.oldSvc)

			if !reflect.DeepEqual(tc.svc, tc.compareSvc) {
				t.Errorf("%v: unexpected svc spec: %v", tc.name, cmp.Diff(tc.svc, tc.compareSvc))
			}
		})
	}
}

func TestDropTypeDependentFields(t *testing.T) {
	// Tweaks used below.
	setTypeExternalName := func(svc *api.Service) {
		svc.Spec.Type = api.ServiceTypeExternalName
	}
	setTypeNodePort := func(svc *api.Service) {
		svc.Spec.Type = api.ServiceTypeNodePort
	}
	setTypeClusterIP := func(svc *api.Service) {
		svc.Spec.Type = api.ServiceTypeClusterIP
	}
	setTypeLoadBalancer := func(svc *api.Service) {
		svc.Spec.Type = api.ServiceTypeLoadBalancer
	}
	clearClusterIPs := func(svc *api.Service) {
		svc.Spec.ClusterIP = ""
		svc.Spec.ClusterIPs = nil
	}
	changeClusterIPs := func(svc *api.Service) {
		svc.Spec.ClusterIP += "0"
		svc.Spec.ClusterIPs[0] += "0"
	}
	setNodePorts := func(svc *api.Service) {
		for i := range svc.Spec.Ports {
			svc.Spec.Ports[i].NodePort = int32(30000 + i)
		}
	}
	changeNodePorts := func(svc *api.Service) {
		for i := range svc.Spec.Ports {
			svc.Spec.Ports[i].NodePort += 100
		}
	}
	setExternalIPs := func(svc *api.Service) {
		svc.Spec.ExternalIPs = []string{"1.1.1.1"}
	}
	clearExternalIPs := func(svc *api.Service) {
		svc.Spec.ExternalIPs = nil
	}
	setExternalTrafficPolicyCluster := func(svc *api.Service) {
		svc.Spec.ExternalTrafficPolicy = api.ServiceExternalTrafficPolicyCluster
	}
	clearExternalTrafficPolicy := func(svc *api.Service) {
		svc.Spec.ExternalTrafficPolicy = ""
	}
	clearIPFamilies := func(svc *api.Service) {
		svc.Spec.IPFamilies = nil
	}
	changeIPFamilies := func(svc *api.Service) {
		svc.Spec.IPFamilies[0] = svc.Spec.IPFamilies[1]
	}
	clearIPFamilyPolicy := func(svc *api.Service) {
		svc.Spec.IPFamilyPolicy = nil
	}
	changeIPFamilyPolicy := func(svc *api.Service) {
		single := api.IPFamilyPolicySingleStack
		svc.Spec.IPFamilyPolicy = &single
	}
	addPort := func(svc *api.Service) {
		svc.Spec.Ports = append(svc.Spec.Ports, makeValidServicePort("new", "TCP", 0))
	}
	delPort := func(svc *api.Service) {
		svc.Spec.Ports = svc.Spec.Ports[0 : len(svc.Spec.Ports)-1]
	}
	changePort := func(svc *api.Service) {
		svc.Spec.Ports[0].Port += 100
		svc.Spec.Ports[0].Protocol = "UDP"
	}
	setHCNodePort := func(svc *api.Service) {
		svc.Spec.ExternalTrafficPolicy = api.ServiceExternalTrafficPolicyLocal
		svc.Spec.HealthCheckNodePort = int32(32000)
	}
	changeHCNodePort := func(svc *api.Service) {
		svc.Spec.HealthCheckNodePort += 100
	}
	patches := func(fns ...func(svc *api.Service)) func(svc *api.Service) {
		return func(svc *api.Service) {
			for _, fn := range fns {
				fn(svc)
			}
		}
	}
	setAllocateLoadBalancerNodePortsTrue := func(svc *api.Service) {
		svc.Spec.AllocateLoadBalancerNodePorts = ptr.To(true)
	}
	setAllocateLoadBalancerNodePortsFalse := func(svc *api.Service) {
		svc.Spec.AllocateLoadBalancerNodePorts = ptr.To(false)
	}
	clearAllocateLoadBalancerNodePorts := func(svc *api.Service) {
		svc.Spec.AllocateLoadBalancerNodePorts = nil
	}
	setLoadBalancerClass := func(svc *api.Service) {
		svc.Spec.LoadBalancerClass = ptr.To("test-load-balancer-class")
	}
	clearLoadBalancerClass := func(svc *api.Service) {
		svc.Spec.LoadBalancerClass = nil
	}
	changeLoadBalancerClass := func(svc *api.Service) {
		svc.Spec.LoadBalancerClass = ptr.To("test-load-balancer-class-changed")
	}

	testCases := []struct {
		name   string
		svc    *api.Service
		patch  func(svc *api.Service)
		expect *api.Service
	}{
		{ // clusterIP cases
			name:   "don't clear clusterIP et al",
			svc:    makeValidService(),
			patch:  nil,
			expect: makeValidService(),
		}, {
			name:   "clear clusterIP et al",
			svc:    makeValidService(),
			patch:  setTypeExternalName,
			expect: makeValidServiceCustom(setTypeExternalName, clearClusterIPs, clearIPFamilies, clearIPFamilyPolicy),
		}, {
			name:   "don't clear changed clusterIP",
			svc:    makeValidService(),
			patch:  patches(setTypeExternalName, changeClusterIPs),
			expect: makeValidServiceCustom(setTypeExternalName, changeClusterIPs, clearIPFamilies, clearIPFamilyPolicy),
		}, {
			name:   "don't clear changed ipFamilies",
			svc:    makeValidService(),
			patch:  patches(setTypeExternalName, changeIPFamilies),
			expect: makeValidServiceCustom(setTypeExternalName, clearClusterIPs, changeIPFamilies, clearIPFamilyPolicy),
		}, {
			name:   "don't clear changed ipFamilyPolicy",
			svc:    makeValidService(),
			patch:  patches(setTypeExternalName, changeIPFamilyPolicy),
			expect: makeValidServiceCustom(setTypeExternalName, clearClusterIPs, clearIPFamilies, changeIPFamilyPolicy),
		}, { // nodePort cases
			name:   "don't clear nodePorts for type=NodePort",
			svc:    makeValidServiceCustom(setTypeNodePort, setNodePorts),
			patch:  nil,
			expect: makeValidServiceCustom(setTypeNodePort, setNodePorts),
		}, {
			name:   "don't clear nodePorts for type=LoadBalancer",
			svc:    makeValidServiceCustom(setTypeLoadBalancer, setNodePorts),
			patch:  nil,
			expect: makeValidServiceCustom(setTypeLoadBalancer, setNodePorts),
		}, {
			name:   "clear nodePorts",
			svc:    makeValidServiceCustom(setTypeLoadBalancer, setNodePorts),
			patch:  setTypeClusterIP,
			expect: makeValidService(),
		}, {
			name:   "don't clear changed nodePorts",
			svc:    makeValidServiceCustom(setTypeLoadBalancer, setNodePorts),
			patch:  patches(setTypeClusterIP, changeNodePorts),
			expect: makeValidServiceCustom(setNodePorts, changeNodePorts),
		}, {
			name:   "clear nodePorts when adding a port",
			svc:    makeValidServiceCustom(setTypeLoadBalancer, setNodePorts),
			patch:  patches(setTypeClusterIP, addPort),
			expect: makeValidServiceCustom(addPort),
		}, {
			name:   "don't clear nodePorts when adding a port with NodePort",
			svc:    makeValidServiceCustom(setTypeLoadBalancer, setNodePorts),
			patch:  patches(setTypeClusterIP, addPort, setNodePorts),
			expect: makeValidServiceCustom(addPort, setNodePorts),
		}, {
			name:   "clear nodePorts when removing a port",
			svc:    makeValidServiceCustom(setTypeLoadBalancer, setNodePorts),
			patch:  patches(setTypeClusterIP, delPort),
			expect: makeValidServiceCustom(delPort),
		}, {
			name:   "clear nodePorts when changing a port",
			svc:    makeValidServiceCustom(setTypeLoadBalancer, setNodePorts),
			patch:  patches(setTypeClusterIP, changePort),
			expect: makeValidServiceCustom(changePort),
		}, { // healthCheckNodePort cases
			name:   "don't clear healthCheckNodePort for type=LoadBalancer",
			svc:    makeValidServiceCustom(setTypeLoadBalancer, setHCNodePort),
			patch:  nil,
			expect: makeValidServiceCustom(setTypeLoadBalancer, setHCNodePort),
		}, {
			name:   "clear healthCheckNodePort",
			svc:    makeValidServiceCustom(setTypeLoadBalancer, setHCNodePort),
			patch:  setTypeClusterIP,
			expect: makeValidService(),
		}, {
			name:   "don't clear changed healthCheckNodePort",
			svc:    makeValidServiceCustom(setTypeLoadBalancer, setHCNodePort),
			patch:  patches(setTypeClusterIP, changeHCNodePort),
			expect: makeValidServiceCustom(setHCNodePort, changeHCNodePort, clearExternalTrafficPolicy),
		}, { // allocatedLoadBalancerNodePorts cases
			name:   "clear allocatedLoadBalancerNodePorts true -> true",
			svc:    makeValidServiceCustom(setTypeLoadBalancer, setAllocateLoadBalancerNodePortsTrue),
			patch:  setTypeNodePort,
			expect: makeValidServiceCustom(setTypeNodePort),
		}, {
			name:   "clear allocatedLoadBalancerNodePorts false -> false",
			svc:    makeValidServiceCustom(setTypeLoadBalancer, setAllocateLoadBalancerNodePortsFalse),
			patch:  setTypeNodePort,
			expect: makeValidServiceCustom(setTypeNodePort),
		}, {
			name:   "set allocatedLoadBalancerNodePorts nil -> true",
			svc:    makeValidServiceCustom(setTypeLoadBalancer),
			patch:  patches(setTypeNodePort, setAllocateLoadBalancerNodePortsTrue),
			expect: makeValidServiceCustom(setTypeNodePort, setAllocateLoadBalancerNodePortsTrue),
		}, {
			name:   "set allocatedLoadBalancerNodePorts nil -> false",
			svc:    makeValidServiceCustom(setTypeLoadBalancer),
			patch:  patches(setTypeNodePort, setAllocateLoadBalancerNodePortsFalse),
			expect: makeValidServiceCustom(setTypeNodePort, setAllocateLoadBalancerNodePortsFalse),
		}, {
			name:   "set allocatedLoadBalancerNodePorts true -> nil",
			svc:    makeValidServiceCustom(setTypeLoadBalancer, setAllocateLoadBalancerNodePortsTrue),
			patch:  patches(setTypeNodePort, clearAllocateLoadBalancerNodePorts),
			expect: makeValidServiceCustom(setTypeNodePort),
		}, {
			name:   "set allocatedLoadBalancerNodePorts false -> nil",
			svc:    makeValidServiceCustom(setTypeLoadBalancer, setAllocateLoadBalancerNodePortsFalse),
			patch:  patches(setTypeNodePort, clearAllocateLoadBalancerNodePorts),
			expect: makeValidServiceCustom(setTypeNodePort),
		}, {
			name:   "set allocatedLoadBalancerNodePorts true -> false",
			svc:    makeValidServiceCustom(setTypeLoadBalancer, setAllocateLoadBalancerNodePortsTrue),
			patch:  patches(setTypeNodePort, setAllocateLoadBalancerNodePortsFalse),
			expect: makeValidServiceCustom(setTypeNodePort, setAllocateLoadBalancerNodePortsFalse),
		}, {
			name:   "set allocatedLoadBalancerNodePorts false -> true",
			svc:    makeValidServiceCustom(setTypeLoadBalancer, setAllocateLoadBalancerNodePortsFalse),
			patch:  patches(setTypeNodePort, setAllocateLoadBalancerNodePortsTrue),
			expect: makeValidServiceCustom(setTypeNodePort, setAllocateLoadBalancerNodePortsTrue),
		}, { // loadBalancerClass cases
			name:   "clear loadBalancerClass when set Service type LoadBalancer -> non LoadBalancer",
			svc:    makeValidServiceCustom(setTypeLoadBalancer, setLoadBalancerClass),
			patch:  setTypeClusterIP,
			expect: makeValidServiceCustom(setTypeClusterIP, clearLoadBalancerClass),
		}, {
			name:   "update loadBalancerClass load balancer class name",
			svc:    makeValidServiceCustom(setTypeLoadBalancer, setLoadBalancerClass),
			patch:  changeLoadBalancerClass,
			expect: makeValidServiceCustom(setTypeLoadBalancer, changeLoadBalancerClass),
		}, {
			name:   "clear load balancer class name",
			svc:    makeValidServiceCustom(setTypeLoadBalancer, setLoadBalancerClass),
			patch:  clearLoadBalancerClass,
			expect: makeValidServiceCustom(setTypeLoadBalancer, clearLoadBalancerClass),
		}, {
			name:   "change service type and load balancer class",
			svc:    makeValidServiceCustom(setTypeLoadBalancer, setLoadBalancerClass),
			patch:  patches(setTypeClusterIP, changeLoadBalancerClass),
			expect: makeValidServiceCustom(setTypeClusterIP, changeLoadBalancerClass),
		}, {
			name:   "change service type to load balancer and set load balancer class",
			svc:    makeValidServiceCustom(setTypeClusterIP),
			patch:  patches(setTypeLoadBalancer, setLoadBalancerClass),
			expect: makeValidServiceCustom(setTypeLoadBalancer, setLoadBalancerClass),
		}, {
			name:   "don't clear load balancer class for Type=LoadBalancer",
			svc:    makeValidServiceCustom(setTypeLoadBalancer, setLoadBalancerClass),
			patch:  nil,
			expect: makeValidServiceCustom(setTypeLoadBalancer, setLoadBalancerClass),
		}, {
			name:   "clear externalTrafficPolicy when removing externalIPs for Type=ClusterIP",
			svc:    makeValidServiceCustom(setTypeClusterIP, setExternalIPs, setExternalTrafficPolicyCluster),
			patch:  patches(clearExternalIPs),
			expect: makeValidServiceCustom(setTypeClusterIP, clearExternalTrafficPolicy),
		}}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result := tc.svc.DeepCopy()
			if tc.patch != nil {
				tc.patch(result)
			}
			dropTypeDependentFields(result, tc.svc)
			if result.Spec.ClusterIP != tc.expect.Spec.ClusterIP {
				t.Errorf("expected clusterIP %q, got %q", tc.expect.Spec.ClusterIP, result.Spec.ClusterIP)
			}
			if !reflect.DeepEqual(result.Spec.ClusterIPs, tc.expect.Spec.ClusterIPs) {
				t.Errorf("expected clusterIPs %q, got %q", tc.expect.Spec.ClusterIP, result.Spec.ClusterIP)
			}
			if !reflect.DeepEqual(result.Spec.IPFamilies, tc.expect.Spec.IPFamilies) {
				t.Errorf("expected ipFamilies %q, got %q", tc.expect.Spec.IPFamilies, result.Spec.IPFamilies)
			}
			if !reflect.DeepEqual(result.Spec.IPFamilyPolicy, tc.expect.Spec.IPFamilyPolicy) {
				t.Errorf("expected ipFamilyPolicy %q, got %q", getIPFamilyPolicy(tc.expect), getIPFamilyPolicy(result))
			}
			for i := range result.Spec.Ports {
				resultPort := result.Spec.Ports[i].NodePort
				expectPort := tc.expect.Spec.Ports[i].NodePort
				if resultPort != expectPort {
					t.Errorf("failed %q: expected Ports[%d].NodePort %d, got %d", tc.name, i, expectPort, resultPort)
				}
			}
			if result.Spec.HealthCheckNodePort != tc.expect.Spec.HealthCheckNodePort {
				t.Errorf("failed %q: expected healthCheckNodePort %d, got %d", tc.name, tc.expect.Spec.HealthCheckNodePort, result.Spec.HealthCheckNodePort)
			}
			if !reflect.DeepEqual(result.Spec.AllocateLoadBalancerNodePorts, tc.expect.Spec.AllocateLoadBalancerNodePorts) {
				t.Errorf("failed %q: expected AllocateLoadBalancerNodePorts %v, got %v", tc.name, tc.expect.Spec.AllocateLoadBalancerNodePorts, result.Spec.AllocateLoadBalancerNodePorts)
			}
			if !reflect.DeepEqual(result.Spec.LoadBalancerClass, tc.expect.Spec.LoadBalancerClass) {
				t.Errorf("failed %q: expected LoadBalancerClass %v, got %v", tc.name, tc.expect.Spec.LoadBalancerClass, result.Spec.LoadBalancerClass)
			}
			if !reflect.DeepEqual(result.Spec.ExternalTrafficPolicy, tc.expect.Spec.ExternalTrafficPolicy) {
				t.Errorf("failed %q: expected ExternalTrafficPolicy %v, got %v", tc.name, tc.expect.Spec.ExternalTrafficPolicy, result.Spec.ExternalTrafficPolicy)
			}
		})
	}
}

func TestMatchService(t *testing.T) {
	noHeadlessServiceRequirement, err := labels.NewRequirement(v1.IsHeadlessService, selection.DoesNotExist, nil)
	if err != nil {
		t.Fatalf("Error creating no headless service requirement: %v", err)
	}
	noHeadlessServiceLabelSelector := labels.NewSelector().Add(*noHeadlessServiceRequirement)
	testCases := []struct {
		name          string
		in            *api.Service
		fieldSelector fields.Selector
		labelSelector labels.Selector
		expectMatch   bool
	}{
		{
			name: "match on name",
			in: &api.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test",
					Namespace: "testns",
				},
				Spec: api.ServiceSpec{ClusterIP: api.ClusterIPNone},
			},
			fieldSelector: fields.ParseSelectorOrDie("metadata.name=test"),
			labelSelector: labels.Everything(),
			expectMatch:   true,
		},
		{
			name: "match on namespace",
			in: &api.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test",
					Namespace: "testns",
				},
				Spec: api.ServiceSpec{ClusterIP: api.ClusterIPNone},
			},
			fieldSelector: fields.ParseSelectorOrDie("metadata.namespace=testns"),
			labelSelector: labels.Everything(),
			expectMatch:   true,
		},
		{
			name: "no match on name",
			in: &api.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test",
					Namespace: "testns",
				},
				Spec: api.ServiceSpec{ClusterIP: api.ClusterIPNone},
			},
			fieldSelector: fields.ParseSelectorOrDie("metadata.name=nomatch"),
			labelSelector: labels.Everything(),
			expectMatch:   false,
		},
		{
			name: "no match on namespace",
			in: &api.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test",
					Namespace: "testns",
				},
				Spec: api.ServiceSpec{ClusterIP: api.ClusterIPNone},
			},
			fieldSelector: fields.ParseSelectorOrDie("metadata.namespace=nomatch"),
			labelSelector: labels.Everything(),
			expectMatch:   false,
		},
		{
			name: "match on loadbalancer type service",
			in: &api.Service{
				Spec: api.ServiceSpec{Type: api.ServiceTypeLoadBalancer},
			},
			fieldSelector: fields.ParseSelectorOrDie("spec.type=LoadBalancer"),
			labelSelector: labels.Everything(),
			expectMatch:   true,
		},
		{
			name: "no match on nodeport type service",
			in: &api.Service{
				Spec: api.ServiceSpec{Type: api.ServiceTypeNodePort},
			},
			fieldSelector: fields.ParseSelectorOrDie("spec.type=LoadBalancer"),
			labelSelector: labels.Everything(),
			expectMatch:   false,
		},
		{
			name: "match on headless service",
			in: &api.Service{
				Spec: api.ServiceSpec{ClusterIP: api.ClusterIPNone},
			},
			fieldSelector: fields.ParseSelectorOrDie("spec.clusterIP=None"),
			labelSelector: labels.Everything(),
			expectMatch:   true,
		},
		{
			name: "no match on clusterIP service",
			in: &api.Service{
				Spec: api.ServiceSpec{ClusterIP: "192.168.1.1"},
			},
			fieldSelector: fields.ParseSelectorOrDie("spec.clusterIP=None"),
			labelSelector: labels.Everything(),
			expectMatch:   false,
		},
		{
			name: "match on clusterIP service",
			in: &api.Service{
				Spec: api.ServiceSpec{ClusterIP: "192.168.1.1"},
			},
			fieldSelector: fields.ParseSelectorOrDie("spec.clusterIP=192.168.1.1"),
			labelSelector: labels.Everything(),
			expectMatch:   true,
		},
		{
			name: "match on non-headless service",
			in: &api.Service{
				Spec: api.ServiceSpec{ClusterIP: "192.168.1.1"},
			},
			fieldSelector: fields.ParseSelectorOrDie("spec.clusterIP!=None"),
			labelSelector: labels.Everything(),
			expectMatch:   true,
		},
		{
			name: "match on any ClusterIP set service",
			in: &api.Service{
				Spec: api.ServiceSpec{ClusterIP: "192.168.1.1"},
			},
			fieldSelector: fields.ParseSelectorOrDie("spec.clusterIP!=\"\""),
			labelSelector: labels.Everything(),
			expectMatch:   true,
		},
		{
			name: "match on clusterIP IPv6 service",
			in: &api.Service{
				Spec: api.ServiceSpec{ClusterIP: "2001:db2::1"},
			},
			fieldSelector: fields.ParseSelectorOrDie("spec.clusterIP=2001:db2::1"),
			labelSelector: labels.Everything(),
			expectMatch:   true,
		},
		{
			name: "no match on headless service",
			in: &api.Service{
				Spec: api.ServiceSpec{ClusterIP: api.ClusterIPNone},
			},
			fieldSelector: fields.ParseSelectorOrDie("spec.clusterIP=192.168.1.1"),
			labelSelector: labels.Everything(),
			expectMatch:   false,
		},
		{
			name: "no match on headless service",
			in: &api.Service{
				Spec: api.ServiceSpec{ClusterIP: api.ClusterIPNone},
			},
			fieldSelector: fields.ParseSelectorOrDie("spec.clusterIP=2001:db2::1"),
			labelSelector: labels.Everything(),
			expectMatch:   false,
		},
		{
			name:          "no match on empty service",
			in:            &api.Service{},
			fieldSelector: fields.ParseSelectorOrDie("spec.clusterIP=None"),
			labelSelector: labels.Everything(),
			expectMatch:   false,
		},
		{
			name: "no match on headless service",
			in: &api.Service{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1.IsHeadlessService: "",
					},
				},
			},
			fieldSelector: fields.ParseSelectorOrDie("spec.clusterIP!=None"),
			labelSelector: noHeadlessServiceLabelSelector,
			expectMatch:   false,
		},
		{
			name: "no match on headless service",
			in: &api.Service{
				Spec: api.ServiceSpec{ClusterIP: api.ClusterIPNone},
			},
			fieldSelector: fields.ParseSelectorOrDie("spec.clusterIP!=None"),
			labelSelector: noHeadlessServiceLabelSelector,
			expectMatch:   false,
		},
		{
			name:          "match on empty service",
			in:            &api.Service{},
			fieldSelector: fields.ParseSelectorOrDie("spec.clusterIP!=None"),
			labelSelector: noHeadlessServiceLabelSelector,
			expectMatch:   true,
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			m := Matcher(testCase.labelSelector, testCase.fieldSelector)
			result, err := m.Matches(testCase.in)
			if err != nil {
				t.Errorf("Unexpected error %v", err)
			}
			if result != testCase.expectMatch {
				t.Errorf("Result %v, Expected %v, Selector: %v, Service: %v", result, testCase.expectMatch, testCase.fieldSelector.String(), testCase.in)
			}
		})
	}
}
