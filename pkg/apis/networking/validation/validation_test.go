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

package validation

import (
	"fmt"
	"strings"
	"testing"

	apimachineryvalidation "k8s.io/apimachinery/pkg/api/validation"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation/field"
	api "k8s.io/kubernetes/pkg/apis/core"
	"k8s.io/kubernetes/pkg/apis/networking"
	utilpointer "k8s.io/utils/pointer"
)

func makeValidNetworkPolicy() *networking.NetworkPolicy {
	return &networking.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "foo", Namespace: "bar"},
		Spec: networking.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{"a": "b"},
			},
		},
	}
}

type netpolTweak func(networkPolicy *networking.NetworkPolicy)

func makeNetworkPolicyCustom(tweaks ...netpolTweak) *networking.NetworkPolicy {
	networkPolicy := makeValidNetworkPolicy()
	for _, fn := range tweaks {
		fn(networkPolicy)
	}
	return networkPolicy
}

func makePort(proto *api.Protocol, port intstr.IntOrString, endPort int32) networking.NetworkPolicyPort {
	r := networking.NetworkPolicyPort{
		Protocol: proto,
		Port:     nil,
	}
	if port != intstr.FromInt32(0) && port != intstr.FromString("") && port != intstr.FromString("0") {
		r.Port = &port
	}
	if endPort != 0 {
		r.EndPort = utilpointer.Int32(endPort)
	}
	return r
}

func TestValidateNetworkPolicy(t *testing.T) {
	protocolTCP := api.ProtocolTCP
	protocolUDP := api.ProtocolUDP
	protocolICMP := api.Protocol("ICMP")
	protocolSCTP := api.ProtocolSCTP

	// Tweaks used below.
	setIngressEmptyFirstElement := func(networkPolicy *networking.NetworkPolicy) {
		networkPolicy.Spec.Ingress = []networking.NetworkPolicyIngressRule{{}}
	}

	setIngressFromEmptyFirstElement := func(networkPolicy *networking.NetworkPolicy) {
		if networkPolicy.Spec.Ingress == nil {
			setIngressEmptyFirstElement(networkPolicy)
		}
		networkPolicy.Spec.Ingress[0].From = []networking.NetworkPolicyPeer{{}}
	}

	setIngressFromIfEmpty := func(networkPolicy *networking.NetworkPolicy) {
		if networkPolicy.Spec.Ingress == nil {
			setIngressEmptyFirstElement(networkPolicy)
		}
		if networkPolicy.Spec.Ingress[0].From == nil {
			setIngressFromEmptyFirstElement(networkPolicy)
		}
	}

	setIngressEmptyPorts := func(networkPolicy *networking.NetworkPolicy) {
		networkPolicy.Spec.Ingress = []networking.NetworkPolicyIngressRule{{
			Ports: []networking.NetworkPolicyPort{{}},
		}}
	}

	setIngressPorts := func(ports ...networking.NetworkPolicyPort) netpolTweak {
		return func(np *networking.NetworkPolicy) {
			if np.Spec.Ingress == nil {
				setIngressEmptyFirstElement(np)
			}
			np.Spec.Ingress[0].Ports = make([]networking.NetworkPolicyPort, len(ports))
			copy(np.Spec.Ingress[0].Ports, ports)
		}
	}

	setIngressFromPodSelector := func(k, v string) func(*networking.NetworkPolicy) {
		return func(networkPolicy *networking.NetworkPolicy) {
			setIngressFromIfEmpty(networkPolicy)
			networkPolicy.Spec.Ingress[0].From[0].PodSelector = &metav1.LabelSelector{
				MatchLabels: map[string]string{k: v},
			}
		}
	}

	setIngressFromNamespaceSelector := func(networkPolicy *networking.NetworkPolicy) {
		setIngressFromIfEmpty(networkPolicy)
		networkPolicy.Spec.Ingress[0].From[0].NamespaceSelector = &metav1.LabelSelector{
			MatchLabels: map[string]string{"c": "d"},
		}
	}

	setIngressFromIPBlockIPV4 := func(networkPolicy *networking.NetworkPolicy) {
		setIngressFromIfEmpty(networkPolicy)
		networkPolicy.Spec.Ingress[0].From[0].IPBlock = &networking.IPBlock{
			CIDR:   "192.168.0.0/16",
			Except: []string{"192.168.3.0/24", "192.168.4.0/24"},
		}
	}

	setIngressFromIPBlockIPV6 := func(networkPolicy *networking.NetworkPolicy) {
		setIngressFromIfEmpty(networkPolicy)
		networkPolicy.Spec.Ingress[0].From[0].IPBlock = &networking.IPBlock{
			CIDR:   "fd00:192:168::/48",
			Except: []string{"fd00:192:168:3::/64", "fd00:192:168:4::/64"},
		}
	}

	setEgressEmptyFirstElement := func(networkPolicy *networking.NetworkPolicy) {
		networkPolicy.Spec.Egress = []networking.NetworkPolicyEgressRule{{}}
	}

	setEgressToEmptyFirstElement := func(networkPolicy *networking.NetworkPolicy) {
		if networkPolicy.Spec.Egress == nil {
			setEgressEmptyFirstElement(networkPolicy)
		}
		networkPolicy.Spec.Egress[0].To = []networking.NetworkPolicyPeer{{}}
	}

	setEgressToIfEmpty := func(networkPolicy *networking.NetworkPolicy) {
		if networkPolicy.Spec.Egress == nil {
			setEgressEmptyFirstElement(networkPolicy)
		}
		if networkPolicy.Spec.Egress[0].To == nil {
			setEgressToEmptyFirstElement(networkPolicy)
		}
	}

	setEgressToNamespaceSelector := func(networkPolicy *networking.NetworkPolicy) {
		setEgressToIfEmpty(networkPolicy)
		networkPolicy.Spec.Egress[0].To[0].NamespaceSelector = &metav1.LabelSelector{
			MatchLabels: map[string]string{"c": "d"},
		}
	}

	setEgressToPodSelector := func(networkPolicy *networking.NetworkPolicy) {
		setEgressToIfEmpty(networkPolicy)
		networkPolicy.Spec.Egress[0].To[0].PodSelector = &metav1.LabelSelector{
			MatchLabels: map[string]string{"c": "d"},
		}
	}

	setEgressToIPBlockIPV4 := func(networkPolicy *networking.NetworkPolicy) {
		setEgressToIfEmpty(networkPolicy)
		networkPolicy.Spec.Egress[0].To[0].IPBlock = &networking.IPBlock{
			CIDR:   "192.168.0.0/16",
			Except: []string{"192.168.3.0/24", "192.168.4.0/24"},
		}
	}

	setEgressToIPBlockIPV6 := func(networkPolicy *networking.NetworkPolicy) {
		setEgressToIfEmpty(networkPolicy)
		networkPolicy.Spec.Egress[0].To[0].IPBlock = &networking.IPBlock{
			CIDR:   "fd00:192:168::/48",
			Except: []string{"fd00:192:168:3::/64", "fd00:192:168:4::/64"},
		}
	}

	setEgressPorts := func(ports ...networking.NetworkPolicyPort) netpolTweak {
		return func(np *networking.NetworkPolicy) {
			if np.Spec.Egress == nil {
				setEgressEmptyFirstElement(np)
			}
			np.Spec.Egress[0].Ports = make([]networking.NetworkPolicyPort, len(ports))
			copy(np.Spec.Egress[0].Ports, ports)
		}
	}

	setPolicyTypesEgress := func(networkPolicy *networking.NetworkPolicy) {
		networkPolicy.Spec.PolicyTypes = []networking.PolicyType{networking.PolicyTypeEgress}
	}

	setPolicyTypesIngressEgress := func(networkPolicy *networking.NetworkPolicy) {
		networkPolicy.Spec.PolicyTypes = []networking.PolicyType{networking.PolicyTypeIngress, networking.PolicyTypeEgress}
	}

	successCases := []*networking.NetworkPolicy{
		makeNetworkPolicyCustom(setIngressEmptyFirstElement),
		makeNetworkPolicyCustom(setIngressFromEmptyFirstElement, setIngressEmptyPorts),
		makeNetworkPolicyCustom(setIngressPorts(
			makePort(nil, intstr.FromInt32(80), 0),
			makePort(&protocolTCP, intstr.FromInt32(0), 0),
			makePort(&protocolTCP, intstr.FromInt32(443), 0),
			makePort(&protocolUDP, intstr.FromString("dns"), 0),
			makePort(&protocolSCTP, intstr.FromInt32(7777), 0),
		)),
		makeNetworkPolicyCustom(setIngressFromPodSelector("c", "d")),
		makeNetworkPolicyCustom(setIngressFromNamespaceSelector),
		makeNetworkPolicyCustom(setIngressFromPodSelector("e", "f"), setIngressFromNamespaceSelector),
		makeNetworkPolicyCustom(setEgressToNamespaceSelector, setIngressFromIPBlockIPV4),
		makeNetworkPolicyCustom(setIngressFromIPBlockIPV4),
		makeNetworkPolicyCustom(setEgressToIPBlockIPV4, setPolicyTypesEgress),
		makeNetworkPolicyCustom(setEgressToIPBlockIPV4, setPolicyTypesIngressEgress),
		makeNetworkPolicyCustom(setEgressPorts(
			makePort(nil, intstr.FromInt32(80), 0),
			makePort(&protocolTCP, intstr.FromInt32(0), 0),
			makePort(&protocolTCP, intstr.FromInt32(443), 0),
			makePort(&protocolUDP, intstr.FromString("dns"), 0),
			makePort(&protocolSCTP, intstr.FromInt32(7777), 0),
		)),
		makeNetworkPolicyCustom(setEgressToNamespaceSelector, setIngressFromIPBlockIPV6),
		makeNetworkPolicyCustom(setIngressFromIPBlockIPV6),
		makeNetworkPolicyCustom(setEgressToIPBlockIPV6, setPolicyTypesEgress),
		makeNetworkPolicyCustom(setEgressToIPBlockIPV6, setPolicyTypesIngressEgress),
		makeNetworkPolicyCustom(setEgressPorts(makePort(nil, intstr.FromInt32(32000), 32768), makePort(&protocolUDP, intstr.FromString("dns"), 0))),
		makeNetworkPolicyCustom(
			setEgressToNamespaceSelector,
			setEgressPorts(
				makePort(nil, intstr.FromInt32(30000), 32768),
				makePort(nil, intstr.FromInt32(32000), 32768),
			),
			setIngressFromPodSelector("e", "f"),
			setIngressPorts(makePort(&protocolTCP, intstr.FromInt32(32768), 32768))),
	}

	// Success cases are expected to pass validation.

	for k, v := range successCases {
		if errs := ValidateNetworkPolicy(v, NetworkPolicyValidationOptions{AllowInvalidLabelValueInSelector: true}); len(errs) != 0 {
			t.Errorf("Expected success for the success validation test number %d, got %v", k, errs)
		}
	}

	invalidSelector := map[string]string{"NoUppercaseOrSpecialCharsLike=Equals": "b"}

	errorCases := map[string]*networking.NetworkPolicy{
		"namespaceSelector and ipBlock": makeNetworkPolicyCustom(setIngressFromNamespaceSelector, setIngressFromIPBlockIPV4),
		"podSelector and ipBlock":       makeNetworkPolicyCustom(setEgressToPodSelector, setEgressToIPBlockIPV4),
		"missing from and to type":      makeNetworkPolicyCustom(setIngressFromEmptyFirstElement, setEgressToEmptyFirstElement),
		"invalid spec.podSelector": makeNetworkPolicyCustom(setIngressFromNamespaceSelector, func(networkPolicy *networking.NetworkPolicy) {
			networkPolicy.Spec = networking.NetworkPolicySpec{
				PodSelector: metav1.LabelSelector{
					MatchLabels: invalidSelector,
				},
			}
		}),
		"invalid ingress.ports.protocol":   makeNetworkPolicyCustom(setIngressPorts(makePort(&protocolICMP, intstr.FromInt32(80), 0))),
		"invalid ingress.ports.port (int)": makeNetworkPolicyCustom(setIngressPorts(makePort(&protocolTCP, intstr.FromInt32(123456789), 0))),
		"invalid ingress.ports.port (str)": makeNetworkPolicyCustom(
			setIngressPorts(makePort(&protocolTCP, intstr.FromString("!@#$"), 0))),
		"invalid ingress.from.podSelector": makeNetworkPolicyCustom(setIngressFromEmptyFirstElement, func(networkPolicy *networking.NetworkPolicy) {
			networkPolicy.Spec.Ingress[0].From[0].PodSelector = &metav1.LabelSelector{
				MatchLabels: invalidSelector,
			}
		}),
		"invalid egress.to.podSelector": makeNetworkPolicyCustom(setEgressToEmptyFirstElement, func(networkPolicy *networking.NetworkPolicy) {
			networkPolicy.Spec.Egress[0].To[0].PodSelector = &metav1.LabelSelector{
				MatchLabels: invalidSelector,
			}
		}),
		"invalid egress.ports.protocol":   makeNetworkPolicyCustom(setEgressPorts(makePort(&protocolICMP, intstr.FromInt32(80), 0))),
		"invalid egress.ports.port (int)": makeNetworkPolicyCustom(setEgressPorts(makePort(&protocolTCP, intstr.FromInt32(123456789), 0))),
		"invalid egress.ports.port (str)": makeNetworkPolicyCustom(setEgressPorts(makePort(&protocolTCP, intstr.FromString("!@#$"), 0))),
		"invalid ingress.from.namespaceSelector": makeNetworkPolicyCustom(setIngressFromEmptyFirstElement, func(networkPolicy *networking.NetworkPolicy) {
			networkPolicy.Spec.Ingress[0].From[0].NamespaceSelector = &metav1.LabelSelector{
				MatchLabels: invalidSelector,
			}
		}),
		"missing cidr field": makeNetworkPolicyCustom(setIngressFromIPBlockIPV4, func(networkPolicy *networking.NetworkPolicy) {
			networkPolicy.Spec.Ingress[0].From[0].IPBlock.CIDR = ""
		}),
		"invalid cidr format": makeNetworkPolicyCustom(setIngressFromIPBlockIPV4, func(networkPolicy *networking.NetworkPolicy) {
			networkPolicy.Spec.Ingress[0].From[0].IPBlock.CIDR = "192.168.5.6"
		}),
		"invalid ipv6 cidr format": makeNetworkPolicyCustom(setIngressFromIPBlockIPV6, func(networkPolicy *networking.NetworkPolicy) {
			networkPolicy.Spec.Ingress[0].From[0].IPBlock.CIDR = "fd00:192:168::"
		}),
		"except field is an empty string": makeNetworkPolicyCustom(setIngressFromIPBlockIPV4, func(networkPolicy *networking.NetworkPolicy) {
			networkPolicy.Spec.Ingress[0].From[0].IPBlock.Except = []string{""}
		}),
		"except field is an space string": makeNetworkPolicyCustom(setIngressFromIPBlockIPV4, func(networkPolicy *networking.NetworkPolicy) {
			networkPolicy.Spec.Ingress[0].From[0].IPBlock.Except = []string{" "}
		}),
		"except field is an invalid ip": makeNetworkPolicyCustom(setIngressFromIPBlockIPV4, func(networkPolicy *networking.NetworkPolicy) {
			networkPolicy.Spec.Ingress[0].From[0].IPBlock.Except = []string{"300.300.300.300"}
		}),
		"except IP is outside of CIDR range": makeNetworkPolicyCustom(setIngressFromEmptyFirstElement, func(networkPolicy *networking.NetworkPolicy) {
			networkPolicy.Spec.Ingress[0].From[0].IPBlock = &networking.IPBlock{
				CIDR:   "192.168.8.0/24",
				Except: []string{"192.168.9.1/32"},
			}
		}),
		"except IP is not strictly within CIDR range": makeNetworkPolicyCustom(setIngressFromEmptyFirstElement, func(networkPolicy *networking.NetworkPolicy) {
			networkPolicy.Spec.Ingress[0].From[0].IPBlock = &networking.IPBlock{
				CIDR:   "192.168.0.0/24",
				Except: []string{"192.168.0.0/24"},
			}
		}),
		"except IPv6 is outside of CIDR range": makeNetworkPolicyCustom(setIngressFromEmptyFirstElement, func(networkPolicy *networking.NetworkPolicy) {
			networkPolicy.Spec.Ingress[0].From[0].IPBlock = &networking.IPBlock{
				CIDR:   "fd00:192:168:1::/64",
				Except: []string{"fd00:192:168:2::/64"},
			}
		}),
		"invalid policyTypes": makeNetworkPolicyCustom(setEgressToIPBlockIPV4, func(networkPolicy *networking.NetworkPolicy) {
			networkPolicy.Spec.PolicyTypes = []networking.PolicyType{"foo", "bar"}
		}),
		"too many policyTypes": makeNetworkPolicyCustom(setEgressToIPBlockIPV4, func(networkPolicy *networking.NetworkPolicy) {
			networkPolicy.Spec.PolicyTypes = []networking.PolicyType{"foo", "bar", "baz"}
		}),
		"multiple ports defined, one port range is invalid": makeNetworkPolicyCustom(
			setEgressToNamespaceSelector,
			setEgressPorts(
				makePort(&protocolUDP, intstr.FromInt32(35000), 32768),
				makePort(nil, intstr.FromInt32(32000), 32768),
			),
		),
		"endPort defined with named/string port": makeNetworkPolicyCustom(
			setEgressToNamespaceSelector,
			setEgressPorts(
				makePort(&protocolUDP, intstr.FromString("dns"), 32768),
				makePort(nil, intstr.FromInt32(32000), 32768),
			),
		),
		"endPort defined without port defined": makeNetworkPolicyCustom(
			setEgressToNamespaceSelector,
			setEgressPorts(makePort(&protocolTCP, intstr.FromInt32(0), 32768)),
		),
		"port is greater than endPort": makeNetworkPolicyCustom(
			setEgressToNamespaceSelector,
			setEgressPorts(makePort(&protocolSCTP, intstr.FromInt32(35000), 32768)),
		),
		"multiple invalid port ranges defined": makeNetworkPolicyCustom(
			setEgressToNamespaceSelector,
			setEgressPorts(
				makePort(&protocolUDP, intstr.FromInt32(35000), 32768),
				makePort(&protocolTCP, intstr.FromInt32(0), 32768),
				makePort(&protocolTCP, intstr.FromString("https"), 32768),
			),
		),
		"invalid endport range defined": makeNetworkPolicyCustom(setEgressToNamespaceSelector, setEgressPorts(makePort(&protocolTCP, intstr.FromInt32(30000), 65537))),
	}

	// Error cases are not expected to pass validation.
	for testName, networkPolicy := range errorCases {
		if errs := ValidateNetworkPolicy(networkPolicy, NetworkPolicyValidationOptions{AllowInvalidLabelValueInSelector: true}); len(errs) == 0 {
			t.Errorf("Expected failure for test: %s", testName)
		}
	}
}

func TestValidateNetworkPolicyUpdate(t *testing.T) {
	type npUpdateTest struct {
		old    networking.NetworkPolicy
		update networking.NetworkPolicy
	}
	successCases := map[string]npUpdateTest{
		"no change": {
			old: networking.NetworkPolicy{
				ObjectMeta: metav1.ObjectMeta{Name: "foo", Namespace: "bar"},
				Spec: networking.NetworkPolicySpec{
					PodSelector: metav1.LabelSelector{
						MatchLabels: map[string]string{"a": "b"},
					},
					Ingress: []networking.NetworkPolicyIngressRule{},
				},
			},
			update: networking.NetworkPolicy{
				ObjectMeta: metav1.ObjectMeta{Name: "foo", Namespace: "bar"},
				Spec: networking.NetworkPolicySpec{
					PodSelector: metav1.LabelSelector{
						MatchLabels: map[string]string{"a": "b"},
					},
					Ingress: []networking.NetworkPolicyIngressRule{},
				},
			},
		},
		"change spec": {
			old: networking.NetworkPolicy{
				ObjectMeta: metav1.ObjectMeta{Name: "foo", Namespace: "bar"},
				Spec: networking.NetworkPolicySpec{
					PodSelector: metav1.LabelSelector{},
					Ingress:     []networking.NetworkPolicyIngressRule{},
				},
			},
			update: networking.NetworkPolicy{
				ObjectMeta: metav1.ObjectMeta{Name: "foo", Namespace: "bar"},
				Spec: networking.NetworkPolicySpec{
					PodSelector: metav1.LabelSelector{
						MatchLabels: map[string]string{"a": "b"},
					},
					Ingress: []networking.NetworkPolicyIngressRule{},
				},
			},
		},
	}

	for testName, successCase := range successCases {
		successCase.old.ObjectMeta.ResourceVersion = "1"
		successCase.update.ObjectMeta.ResourceVersion = "1"
		if errs := ValidateNetworkPolicyUpdate(&successCase.update, &successCase.old, NetworkPolicyValidationOptions{false}); len(errs) != 0 {
			t.Errorf("expected success (%s): %v", testName, errs)
		}
	}

	errorCases := map[string]npUpdateTest{
		"change name": {
			old: networking.NetworkPolicy{
				ObjectMeta: metav1.ObjectMeta{Name: "foo", Namespace: "bar"},
				Spec: networking.NetworkPolicySpec{
					PodSelector: metav1.LabelSelector{},
					Ingress:     []networking.NetworkPolicyIngressRule{},
				},
			},
			update: networking.NetworkPolicy{
				ObjectMeta: metav1.ObjectMeta{Name: "baz", Namespace: "bar"},
				Spec: networking.NetworkPolicySpec{
					PodSelector: metav1.LabelSelector{},
					Ingress:     []networking.NetworkPolicyIngressRule{},
				},
			},
		},
	}

	for testName, errorCase := range errorCases {
		errorCase.old.ObjectMeta.ResourceVersion = "1"
		errorCase.update.ObjectMeta.ResourceVersion = "1"
		if errs := ValidateNetworkPolicyUpdate(&errorCase.update, &errorCase.old, NetworkPolicyValidationOptions{false}); len(errs) == 0 {
			t.Errorf("expected failure: %s", testName)
		}
	}
}

func TestValidateIngress(t *testing.T) {
	serviceBackend := &networking.IngressServiceBackend{
		Name: "defaultbackend",
		Port: networking.ServiceBackendPort{
			Name:   "",
			Number: 80,
		},
	}
	defaultBackend := networking.IngressBackend{
		Service: serviceBackend,
	}
	pathTypePrefix := networking.PathTypePrefix
	pathTypeImplementationSpecific := networking.PathTypeImplementationSpecific
	pathTypeFoo := networking.PathType("foo")

	baseIngress := networking.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "foo",
			Namespace: metav1.NamespaceDefault,
		},
		Spec: networking.IngressSpec{
			DefaultBackend: &defaultBackend,
			Rules: []networking.IngressRule{{
				Host: "foo.bar.com",
				IngressRuleValue: networking.IngressRuleValue{
					HTTP: &networking.HTTPIngressRuleValue{
						Paths: []networking.HTTPIngressPath{{
							Path:     "/foo",
							PathType: &pathTypeImplementationSpecific,
							Backend:  defaultBackend,
						}},
					},
				},
			}},
		},
		Status: networking.IngressStatus{
			LoadBalancer: networking.IngressLoadBalancerStatus{
				Ingress: []networking.IngressLoadBalancerIngress{
					{IP: "127.0.0.1"},
				},
			},
		},
	}

	testCases := map[string]struct {
		tweakIngress       func(ing *networking.Ingress)
		expectErrsOnFields []string
	}{
		"empty path (implementation specific)": {
			tweakIngress: func(ing *networking.Ingress) {
				ing.Spec.Rules[0].IngressRuleValue.HTTP.Paths[0].Path = ""
			},
			expectErrsOnFields: []string{},
		},
		"valid path": {
			tweakIngress: func(ing *networking.Ingress) {
				ing.Spec.Rules[0].IngressRuleValue.HTTP.Paths[0].Path = "/valid"
			},
			expectErrsOnFields: []string{},
		},
		// invalid use cases
		"backend with no service": {
			tweakIngress: func(ing *networking.Ingress) {
				ing.Spec.DefaultBackend.Service.Name = ""
			},
			expectErrsOnFields: []string{
				"spec.defaultBackend.service.name",
			},
		},
		"invalid path type": {
			tweakIngress: func(ing *networking.Ingress) {
				ing.Spec.Rules[0].IngressRuleValue.HTTP.Paths[0].PathType = &pathTypeFoo
			},
			expectErrsOnFields: []string{
				"spec.rules[0].http.paths[0].pathType",
			},
		},
		"empty path (prefix)": {
			tweakIngress: func(ing *networking.Ingress) {
				ing.Spec.Rules[0].IngressRuleValue.HTTP.Paths[0].Path = ""
				ing.Spec.Rules[0].IngressRuleValue.HTTP.Paths[0].PathType = &pathTypePrefix
			},
			expectErrsOnFields: []string{
				"spec.rules[0].http.paths[0].path",
			},
		},
		"no paths": {
			tweakIngress: func(ing *networking.Ingress) {
				ing.Spec.Rules[0].IngressRuleValue.HTTP.Paths = []networking.HTTPIngressPath{}
			},
			expectErrsOnFields: []string{
				"spec.rules[0].http.paths",
			},
		},
		"invalid host (foobar:80)": {
			tweakIngress: func(ing *networking.Ingress) {
				ing.Spec.Rules[0].Host = "foobar:80"
			},
			expectErrsOnFields: []string{
				"spec.rules[0].host",
			},
		},
		"invalid host (127.0.0.1)": {
			tweakIngress: func(ing *networking.Ingress) {
				ing.Spec.Rules[0].Host = "127.0.0.1"
			},
			expectErrsOnFields: []string{
				"spec.rules[0].host",
			},
		},
		"valid wildcard host": {
			tweakIngress: func(ing *networking.Ingress) {
				ing.Spec.Rules[0].Host = "*.bar.com"
			},
			expectErrsOnFields: []string{},
		},
		"invalid wildcard host (foo.*.bar.com)": {
			tweakIngress: func(ing *networking.Ingress) {
				ing.Spec.Rules[0].Host = "foo.*.bar.com"
			},
			expectErrsOnFields: []string{
				"spec.rules[0].host",
			},
		},
		"invalid wildcard host (*)": {
			tweakIngress: func(ing *networking.Ingress) {
				ing.Spec.Rules[0].Host = "*"
			},
			expectErrsOnFields: []string{
				"spec.rules[0].host",
			},
		},
		"path resource backend and service name are not allowed together": {
			tweakIngress: func(ing *networking.Ingress) {
				ing.Spec.Rules[0].IngressRuleValue = networking.IngressRuleValue{
					HTTP: &networking.HTTPIngressRuleValue{
						Paths: []networking.HTTPIngressPath{{
							Path:     "/foo",
							PathType: &pathTypeImplementationSpecific,
							Backend: networking.IngressBackend{
								Service: serviceBackend,
								Resource: &api.TypedLocalObjectReference{
									APIGroup: utilpointer.String("example.com"),
									Kind:     "foo",
									Name:     "bar",
								},
							},
						}},
					},
				}
			},
			expectErrsOnFields: []string{
				"spec.rules[0].http.paths[0].backend",
			},
		},
		"path resource backend and service port are not allowed together": {
			tweakIngress: func(ing *networking.Ingress) {
				ing.Spec.Rules[0].IngressRuleValue = networking.IngressRuleValue{
					HTTP: &networking.HTTPIngressRuleValue{
						Paths: []networking.HTTPIngressPath{{
							Path:     "/foo",
							PathType: &pathTypeImplementationSpecific,
							Backend: networking.IngressBackend{
								Service: serviceBackend,
								Resource: &api.TypedLocalObjectReference{
									APIGroup: utilpointer.String("example.com"),
									Kind:     "foo",
									Name:     "bar",
								},
							},
						}},
					},
				}
			},
			expectErrsOnFields: []string{
				"spec.rules[0].http.paths[0].backend",
			},
		},
		"spec.backend resource and service name are not allowed together": {
			tweakIngress: func(ing *networking.Ingress) {
				ing.Spec.DefaultBackend = &networking.IngressBackend{
					Service: serviceBackend,
					Resource: &api.TypedLocalObjectReference{
						APIGroup: utilpointer.String("example.com"),
						Kind:     "foo",
						Name:     "bar",
					},
				}
			},
			expectErrsOnFields: []string{
				"spec.defaultBackend",
			},
		},
		"spec.backend resource and service port are not allowed together": {
			tweakIngress: func(ing *networking.Ingress) {
				ing.Spec.DefaultBackend = &networking.IngressBackend{
					Service: serviceBackend,
					Resource: &api.TypedLocalObjectReference{
						APIGroup: utilpointer.String("example.com"),
						Kind:     "foo",
						Name:     "bar",
					},
				}
			},
			expectErrsOnFields: []string{
				"spec.defaultBackend",
			},
		},
	}

	for name, testCase := range testCases {
		t.Run(name, func(t *testing.T) {
			ingress := baseIngress.DeepCopy()
			testCase.tweakIngress(ingress)
			errs := validateIngress(ingress, IngressValidationOptions{})
			if len(testCase.expectErrsOnFields) != len(errs) {
				t.Fatalf("Expected %d errors, got %d errors: %v", len(testCase.expectErrsOnFields), len(errs), errs)
			}
			for i, err := range errs {
				if err.Field != testCase.expectErrsOnFields[i] {
					t.Errorf("Expected error on field: %s, got: %s", testCase.expectErrsOnFields[i], err.Error())
				}
			}
		})
	}
}

func TestValidateIngressRuleValue(t *testing.T) {
	serviceBackend := networking.IngressServiceBackend{
		Name: "defaultbackend",
		Port: networking.ServiceBackendPort{
			Name:   "",
			Number: 80,
		},
	}
	fldPath := field.NewPath("testing.http.paths[0].path")
	testCases := map[string]struct {
		pathType     networking.PathType
		path         string
		expectedErrs field.ErrorList
	}{
		"implementation specific: no leading slash": {
			pathType:     networking.PathTypeImplementationSpecific,
			path:         "foo",
			expectedErrs: field.ErrorList{field.Invalid(fldPath, "foo", "must be an absolute path")},
		},
		"implementation specific: leading slash": {
			pathType:     networking.PathTypeImplementationSpecific,
			path:         "/foo",
			expectedErrs: field.ErrorList{},
		},
		"implementation specific: many slashes": {
			pathType:     networking.PathTypeImplementationSpecific,
			path:         "/foo/bar/foo",
			expectedErrs: field.ErrorList{},
		},
		"implementation specific: repeating slashes": {
			pathType:     networking.PathTypeImplementationSpecific,
			path:         "/foo//bar/foo",
			expectedErrs: field.ErrorList{},
		},
		"prefix: no leading slash": {
			pathType:     networking.PathTypePrefix,
			path:         "foo",
			expectedErrs: field.ErrorList{field.Invalid(fldPath, "foo", "must be an absolute path")},
		},
		"prefix: leading slash": {
			pathType:     networking.PathTypePrefix,
			path:         "/foo",
			expectedErrs: field.ErrorList{},
		},
		"prefix: many slashes": {
			pathType:     networking.PathTypePrefix,
			path:         "/foo/bar/foo",
			expectedErrs: field.ErrorList{},
		},
		"prefix: repeating slashes": {
			pathType:     networking.PathTypePrefix,
			path:         "/foo//bar/foo",
			expectedErrs: field.ErrorList{field.Invalid(fldPath, "/foo//bar/foo", "must not contain '//'")},
		},
		"exact: no leading slash": {
			pathType:     networking.PathTypeExact,
			path:         "foo",
			expectedErrs: field.ErrorList{field.Invalid(fldPath, "foo", "must be an absolute path")},
		},
		"exact: leading slash": {
			pathType:     networking.PathTypeExact,
			path:         "/foo",
			expectedErrs: field.ErrorList{},
		},
		"exact: many slashes": {
			pathType:     networking.PathTypeExact,
			path:         "/foo/bar/foo",
			expectedErrs: field.ErrorList{},
		},
		"exact: repeating slashes": {
			pathType:     networking.PathTypeExact,
			path:         "/foo//bar/foo",
			expectedErrs: field.ErrorList{field.Invalid(fldPath, "/foo//bar/foo", "must not contain '//'")},
		},
		"prefix: with /./": {
			pathType:     networking.PathTypePrefix,
			path:         "/foo/./foo",
			expectedErrs: field.ErrorList{field.Invalid(fldPath, "/foo/./foo", "must not contain '/./'")},
		},
		"exact: with /../": {
			pathType:     networking.PathTypeExact,
			path:         "/foo/../foo",
			expectedErrs: field.ErrorList{field.Invalid(fldPath, "/foo/../foo", "must not contain '/../'")},
		},
		"prefix: with %2f": {
			pathType:     networking.PathTypePrefix,
			path:         "/foo/%2f/foo",
			expectedErrs: field.ErrorList{field.Invalid(fldPath, "/foo/%2f/foo", "must not contain '%2f'")},
		},
		"exact: with %2F": {
			pathType:     networking.PathTypeExact,
			path:         "/foo/%2F/foo",
			expectedErrs: field.ErrorList{field.Invalid(fldPath, "/foo/%2F/foo", "must not contain '%2F'")},
		},
	}

	for name, testCase := range testCases {
		t.Run(name, func(t *testing.T) {
			irv := &networking.IngressRuleValue{
				HTTP: &networking.HTTPIngressRuleValue{
					Paths: []networking.HTTPIngressPath{{
						Path:     testCase.path,
						PathType: &testCase.pathType,
						Backend: networking.IngressBackend{
							Service: &serviceBackend,
						},
					}},
				},
			}
			errs := validateIngressRuleValue(irv, field.NewPath("testing"), IngressValidationOptions{})
			if len(errs) != len(testCase.expectedErrs) {
				t.Fatalf("Expected %d errors, got %d (%+v)", len(testCase.expectedErrs), len(errs), errs)
			}

			for i, err := range errs {
				if err.Error() != testCase.expectedErrs[i].Error() {
					t.Fatalf("Expected error: %v, got %v", testCase.expectedErrs[i], err)
				}
			}
		})
	}
}

func TestValidateIngressCreate(t *testing.T) {
	implementationPathType := networking.PathTypeImplementationSpecific
	exactPathType := networking.PathTypeExact
	serviceBackend := &networking.IngressServiceBackend{
		Name: "defaultbackend",
		Port: networking.ServiceBackendPort{
			Number: 80,
		},
	}
	defaultBackend := networking.IngressBackend{
		Service: serviceBackend,
	}
	resourceBackend := &api.TypedLocalObjectReference{
		APIGroup: utilpointer.String("example.com"),
		Kind:     "foo",
		Name:     "bar",
	}
	baseIngress := networking.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "test123",
			Namespace:       "test123",
			ResourceVersion: "1234",
		},
		Spec: networking.IngressSpec{
			DefaultBackend: &defaultBackend,
			Rules:          []networking.IngressRule{},
		},
	}

	testCases := map[string]struct {
		tweakIngress func(ingress *networking.Ingress)
		expectedErrs field.ErrorList
	}{
		"class field set": {
			tweakIngress: func(ingress *networking.Ingress) {
				ingress.Spec.IngressClassName = utilpointer.String("bar")
			},
			expectedErrs: field.ErrorList{},
		},
		"class annotation set": {
			tweakIngress: func(ingress *networking.Ingress) {
				ingress.Annotations = map[string]string{annotationIngressClass: "foo"}
			},
			expectedErrs: field.ErrorList{},
		},
		"class field and annotation set with same value": {
			tweakIngress: func(ingress *networking.Ingress) {
				ingress.Spec.IngressClassName = utilpointer.String("foo")
				ingress.Annotations = map[string]string{annotationIngressClass: "foo"}
			},
			expectedErrs: field.ErrorList{},
		},
		"class field and annotation set with different value": {
			tweakIngress: func(ingress *networking.Ingress) {
				ingress.Spec.IngressClassName = utilpointer.String("bar")
				ingress.Annotations = map[string]string{annotationIngressClass: "foo"}
			},
			expectedErrs: field.ErrorList{field.Invalid(field.NewPath("annotations").Child(annotationIngressClass), "foo", "must match `ingressClassName` when both are specified")},
		},
		"valid regex path": {
			tweakIngress: func(ingress *networking.Ingress) {
				ingress.Spec.Rules = []networking.IngressRule{{
					Host: "foo.bar.com",
					IngressRuleValue: networking.IngressRuleValue{
						HTTP: &networking.HTTPIngressRuleValue{
							Paths: []networking.HTTPIngressPath{{
								Path:     "/([a-z0-9]*)",
								PathType: &implementationPathType,
								Backend:  defaultBackend,
							}},
						},
					},
				}}
			},
			expectedErrs: field.ErrorList{},
		},
		"invalid regex path allowed (v1)": {
			tweakIngress: func(ingress *networking.Ingress) {
				ingress.Spec.Rules = []networking.IngressRule{{
					Host: "foo.bar.com",
					IngressRuleValue: networking.IngressRuleValue{
						HTTP: &networking.HTTPIngressRuleValue{
							Paths: []networking.HTTPIngressPath{{
								Path:     "/([a-z0-9]*)[",
								PathType: &implementationPathType,
								Backend:  defaultBackend,
							}},
						},
					},
				}}
			},
			expectedErrs: field.ErrorList{},
		},
		"Spec.Backend.Resource field allowed on create": {
			tweakIngress: func(ingress *networking.Ingress) {
				ingress.Spec.DefaultBackend = &networking.IngressBackend{
					Resource: resourceBackend}
			},
			expectedErrs: field.ErrorList{},
		},
		"Paths.Backend.Resource field allowed on create": {
			tweakIngress: func(ingress *networking.Ingress) {
				ingress.Spec.Rules = []networking.IngressRule{{
					Host: "foo.bar.com",
					IngressRuleValue: networking.IngressRuleValue{
						HTTP: &networking.HTTPIngressRuleValue{
							Paths: []networking.HTTPIngressPath{{
								Path:     "/([a-z0-9]*)",
								PathType: &implementationPathType,
								Backend: networking.IngressBackend{
									Resource: resourceBackend},
							}},
						},
					},
				}}
			},
			expectedErrs: field.ErrorList{},
		},
		"valid secret": {
			tweakIngress: func(ingress *networking.Ingress) {
				ingress.Spec.TLS = []networking.IngressTLS{{SecretName: "valid"}}
			},
		},
		"invalid secret": {
			tweakIngress: func(ingress *networking.Ingress) {
				ingress.Spec.TLS = []networking.IngressTLS{{SecretName: "invalid name"}}
			},
			expectedErrs: field.ErrorList{field.Invalid(field.NewPath("spec").Child("tls").Index(0).Child("secretName"), "invalid name", `a lowercase RFC 1123 subdomain must consist of lower case alphanumeric characters, '-' or '.', and must start and end with an alphanumeric character (e.g. 'example.com', regex used for validation is '[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*')`)},
		},
		"valid rules with wildcard host": {
			tweakIngress: func(ingress *networking.Ingress) {
				ingress.Spec.TLS = []networking.IngressTLS{{Hosts: []string{"*.bar.com"}}}
				ingress.Spec.Rules = []networking.IngressRule{{
					Host: "*.foo.com",
					IngressRuleValue: networking.IngressRuleValue{
						HTTP: &networking.HTTPIngressRuleValue{
							Paths: []networking.HTTPIngressPath{{
								Path:     "/foo",
								PathType: &exactPathType,
								Backend:  defaultBackend,
							}},
						},
					},
				}}
			},
		},
		"invalid rules with wildcard host": {
			tweakIngress: func(ingress *networking.Ingress) {
				ingress.Spec.TLS = []networking.IngressTLS{{Hosts: []string{"*.bar.com"}}}
				ingress.Spec.Rules = []networking.IngressRule{{
					Host: "*.foo.com",
					IngressRuleValue: networking.IngressRuleValue{
						HTTP: &networking.HTTPIngressRuleValue{
							Paths: []networking.HTTPIngressPath{{
								Path:     "foo",
								PathType: &exactPathType,
								Backend:  defaultBackend,
							}},
						},
					},
				}}
			},
			expectedErrs: field.ErrorList{field.Invalid(field.NewPath("spec").Child("rules").Index(0).Child("http").Child("paths").Index(0).Child("path"), "foo", `must be an absolute path`)},
		},
	}

	for name, testCase := range testCases {
		t.Run(name, func(t *testing.T) {
			newIngress := baseIngress.DeepCopy()
			testCase.tweakIngress(newIngress)
			errs := ValidateIngressCreate(newIngress)
			if len(errs) != len(testCase.expectedErrs) {
				t.Fatalf("Expected %d errors, got %d (%+v)", len(testCase.expectedErrs), len(errs), errs)
			}

			for i, err := range errs {
				if err.Error() != testCase.expectedErrs[i].Error() {
					t.Fatalf("Expected error: %v, got %v", testCase.expectedErrs[i].Error(), err.Error())
				}
			}
		})
	}
}

func TestValidateIngressUpdate(t *testing.T) {
	implementationPathType := networking.PathTypeImplementationSpecific
	exactPathType := networking.PathTypeExact
	serviceBackend := &networking.IngressServiceBackend{
		Name: "defaultbackend",
		Port: networking.ServiceBackendPort{
			Number: 80,
		},
	}
	defaultBackend := networking.IngressBackend{
		Service: serviceBackend,
	}
	resourceBackend := &api.TypedLocalObjectReference{
		APIGroup: utilpointer.String("example.com"),
		Kind:     "foo",
		Name:     "bar",
	}
	baseIngress := networking.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "test123",
			Namespace:       "test123",
			ResourceVersion: "1234",
		},
		Spec: networking.IngressSpec{
			DefaultBackend: &defaultBackend,
		},
	}

	testCases := map[string]struct {
		tweakIngresses func(newIngress, oldIngress *networking.Ingress)
		expectedErrs   field.ErrorList
	}{
		"class field set": {
			tweakIngresses: func(newIngress, oldIngress *networking.Ingress) {
				newIngress.Spec.IngressClassName = utilpointer.String("bar")
			},
			expectedErrs: field.ErrorList{},
		},
		"class annotation set": {
			tweakIngresses: func(newIngress, oldIngress *networking.Ingress) {
				newIngress.Annotations = map[string]string{annotationIngressClass: "foo"}
			},
			expectedErrs: field.ErrorList{},
		},
		"class field and annotation set": {
			tweakIngresses: func(newIngress, oldIngress *networking.Ingress) {
				newIngress.Spec.IngressClassName = utilpointer.String("bar")
				newIngress.Annotations = map[string]string{annotationIngressClass: "foo"}
			},
			expectedErrs: field.ErrorList{},
		},
		"valid regex path -> valid regex path": {
			tweakIngresses: func(newIngress, oldIngress *networking.Ingress) {
				oldIngress.Spec.Rules = []networking.IngressRule{{
					Host: "foo.bar.com",
					IngressRuleValue: networking.IngressRuleValue{
						HTTP: &networking.HTTPIngressRuleValue{
							Paths: []networking.HTTPIngressPath{{
								Path:     "/([a-z0-9]*)",
								PathType: &implementationPathType,
								Backend:  defaultBackend,
							}},
						},
					},
				}}
				newIngress.Spec.Rules = []networking.IngressRule{{
					Host: "foo.bar.com",
					IngressRuleValue: networking.IngressRuleValue{
						HTTP: &networking.HTTPIngressRuleValue{
							Paths: []networking.HTTPIngressPath{{
								Path:     "/([a-z0-9%]*)",
								PathType: &implementationPathType,
								Backend:  defaultBackend,
							}},
						},
					},
				}}
			},
			expectedErrs: field.ErrorList{},
		},
		"valid regex path -> invalid regex path": {
			tweakIngresses: func(newIngress, oldIngress *networking.Ingress) {
				oldIngress.Spec.Rules = []networking.IngressRule{{
					Host: "foo.bar.com",
					IngressRuleValue: networking.IngressRuleValue{
						HTTP: &networking.HTTPIngressRuleValue{
							Paths: []networking.HTTPIngressPath{{
								Path:     "/([a-z0-9]*)",
								PathType: &implementationPathType,
								Backend:  defaultBackend,
							}},
						},
					},
				}}
				newIngress.Spec.Rules = []networking.IngressRule{{
					Host: "foo.bar.com",
					IngressRuleValue: networking.IngressRuleValue{
						HTTP: &networking.HTTPIngressRuleValue{
							Paths: []networking.HTTPIngressPath{{
								Path:     "/bar[",
								PathType: &implementationPathType,
								Backend:  defaultBackend,
							}},
						},
					},
				}}
			},
			expectedErrs: field.ErrorList{},
		},
		"invalid regex path -> valid regex path": {
			tweakIngresses: func(newIngress, oldIngress *networking.Ingress) {
				oldIngress.Spec.Rules = []networking.IngressRule{{
					Host: "foo.bar.com",
					IngressRuleValue: networking.IngressRuleValue{
						HTTP: &networking.HTTPIngressRuleValue{
							Paths: []networking.HTTPIngressPath{{
								Path:     "/bar[",
								PathType: &implementationPathType,
								Backend:  defaultBackend,
							}},
						},
					},
				}}
				newIngress.Spec.Rules = []networking.IngressRule{{
					Host: "foo.bar.com",
					IngressRuleValue: networking.IngressRuleValue{
						HTTP: &networking.HTTPIngressRuleValue{
							Paths: []networking.HTTPIngressPath{{
								Path:     "/([a-z0-9]*)",
								PathType: &implementationPathType,
								Backend:  defaultBackend,
							}},
						},
					},
				}}
			},
			expectedErrs: field.ErrorList{},
		},
		"invalid regex path -> invalid regex path": {
			tweakIngresses: func(newIngress, oldIngress *networking.Ingress) {
				oldIngress.Spec.Rules = []networking.IngressRule{{
					Host: "foo.bar.com",
					IngressRuleValue: networking.IngressRuleValue{
						HTTP: &networking.HTTPIngressRuleValue{
							Paths: []networking.HTTPIngressPath{{
								Path:     "/foo[",
								PathType: &implementationPathType,
								Backend:  defaultBackend,
							}},
						},
					},
				}}
				newIngress.Spec.Rules = []networking.IngressRule{{
					Host: "foo.bar.com",
					IngressRuleValue: networking.IngressRuleValue{
						HTTP: &networking.HTTPIngressRuleValue{
							Paths: []networking.HTTPIngressPath{{
								Path:     "/bar[",
								PathType: &implementationPathType,
								Backend:  defaultBackend,
							}},
						},
					},
				}}
			},
			expectedErrs: field.ErrorList{},
		},
		"new Backend.Resource allowed on update": {
			tweakIngresses: func(newIngress, oldIngress *networking.Ingress) {
				oldIngress.Spec.DefaultBackend = &defaultBackend
				newIngress.Spec.DefaultBackend = &networking.IngressBackend{
					Resource: resourceBackend}
			},
			expectedErrs: field.ErrorList{},
		},
		"old DefaultBackend.Resource allowed on update": {
			tweakIngresses: func(newIngress, oldIngress *networking.Ingress) {
				oldIngress.Spec.DefaultBackend = &networking.IngressBackend{
					Resource: resourceBackend}
				newIngress.Spec.DefaultBackend = &networking.IngressBackend{
					Resource: resourceBackend}
			},
			expectedErrs: field.ErrorList{},
		},
		"changing spec.backend from resource -> no resource": {
			tweakIngresses: func(newIngress, oldIngress *networking.Ingress) {
				oldIngress.Spec.DefaultBackend = &networking.IngressBackend{
					Resource: resourceBackend}
				newIngress.Spec.DefaultBackend = &defaultBackend
			},
			expectedErrs: field.ErrorList{},
		},
		"changing path backend from resource -> no resource": {
			tweakIngresses: func(newIngress, oldIngress *networking.Ingress) {
				oldIngress.Spec.Rules = []networking.IngressRule{{
					Host: "foo.bar.com",
					IngressRuleValue: networking.IngressRuleValue{
						HTTP: &networking.HTTPIngressRuleValue{
							Paths: []networking.HTTPIngressPath{{
								Path:     "/foo[",
								PathType: &implementationPathType,
								Backend: networking.IngressBackend{
									Resource: resourceBackend},
							}},
						},
					},
				}}
				newIngress.Spec.Rules = []networking.IngressRule{{
					Host: "foo.bar.com",
					IngressRuleValue: networking.IngressRuleValue{
						HTTP: &networking.HTTPIngressRuleValue{
							Paths: []networking.HTTPIngressPath{{
								Path:     "/bar[",
								PathType: &implementationPathType,
								Backend:  defaultBackend,
							}},
						},
					},
				}}
			},
			expectedErrs: field.ErrorList{},
		},
		"changing path backend from resource -> resource": {
			tweakIngresses: func(newIngress, oldIngress *networking.Ingress) {
				oldIngress.Spec.Rules = []networking.IngressRule{{
					Host: "foo.bar.com",
					IngressRuleValue: networking.IngressRuleValue{
						HTTP: &networking.HTTPIngressRuleValue{
							Paths: []networking.HTTPIngressPath{{
								Path:     "/foo[",
								PathType: &implementationPathType,
								Backend: networking.IngressBackend{
									Resource: resourceBackend},
							}},
						},
					},
				}}
				newIngress.Spec.Rules = []networking.IngressRule{{
					Host: "foo.bar.com",
					IngressRuleValue: networking.IngressRuleValue{
						HTTP: &networking.HTTPIngressRuleValue{
							Paths: []networking.HTTPIngressPath{{
								Path:     "/bar[",
								PathType: &implementationPathType,
								Backend: networking.IngressBackend{
									Resource: resourceBackend},
							}},
						},
					},
				}}
			},
			expectedErrs: field.ErrorList{},
		},
		"changing path backend from non-resource -> non-resource": {
			tweakIngresses: func(newIngress, oldIngress *networking.Ingress) {
				oldIngress.Spec.Rules = []networking.IngressRule{{
					Host: "foo.bar.com",
					IngressRuleValue: networking.IngressRuleValue{
						HTTP: &networking.HTTPIngressRuleValue{
							Paths: []networking.HTTPIngressPath{{
								Path:     "/foo[",
								PathType: &implementationPathType,
								Backend:  defaultBackend,
							}},
						},
					},
				}}
				newIngress.Spec.Rules = []networking.IngressRule{{
					Host: "foo.bar.com",
					IngressRuleValue: networking.IngressRuleValue{
						HTTP: &networking.HTTPIngressRuleValue{
							Paths: []networking.HTTPIngressPath{{
								Path:     "/bar[",
								PathType: &implementationPathType,
								Backend:  defaultBackend,
							}},
						},
					},
				}}
			},
			expectedErrs: field.ErrorList{},
		},
		"changing path backend from non-resource -> resource": {
			tweakIngresses: func(newIngress, oldIngress *networking.Ingress) {
				oldIngress.Spec.Rules = []networking.IngressRule{{
					Host: "foo.bar.com",
					IngressRuleValue: networking.IngressRuleValue{
						HTTP: &networking.HTTPIngressRuleValue{
							Paths: []networking.HTTPIngressPath{{
								Path:     "/foo[",
								PathType: &implementationPathType,
								Backend:  defaultBackend,
							}},
						},
					},
				}}
				newIngress.Spec.Rules = []networking.IngressRule{{
					Host: "foo.bar.com",
					IngressRuleValue: networking.IngressRuleValue{
						HTTP: &networking.HTTPIngressRuleValue{
							Paths: []networking.HTTPIngressPath{{
								Path:     "/bar[",
								PathType: &implementationPathType,
								Backend: networking.IngressBackend{
									Resource: resourceBackend},
							}},
						},
					},
				}}
			},
			expectedErrs: field.ErrorList{},
		},
		"change valid secret -> invalid secret": {
			tweakIngresses: func(newIngress, oldIngress *networking.Ingress) {
				oldIngress.Spec.TLS = []networking.IngressTLS{{SecretName: "valid"}}
				newIngress.Spec.TLS = []networking.IngressTLS{{SecretName: "invalid name"}}
			},
			expectedErrs: field.ErrorList{field.Invalid(field.NewPath("spec").Child("tls").Index(0).Child("secretName"), "invalid name", `a lowercase RFC 1123 subdomain must consist of lower case alphanumeric characters, '-' or '.', and must start and end with an alphanumeric character (e.g. 'example.com', regex used for validation is '[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*')`)},
		},
		"change invalid secret -> invalid secret": {
			tweakIngresses: func(newIngress, oldIngress *networking.Ingress) {
				oldIngress.Spec.TLS = []networking.IngressTLS{{SecretName: "invalid name 1"}}
				newIngress.Spec.TLS = []networking.IngressTLS{{SecretName: "invalid name 2"}}
			},
		},
		"change valid rules with wildcard host -> invalid rules": {
			tweakIngresses: func(newIngress, oldIngress *networking.Ingress) {
				oldIngress.Spec.TLS = []networking.IngressTLS{{Hosts: []string{"*.bar.com"}}}
				oldIngress.Spec.Rules = []networking.IngressRule{{
					Host: "*.foo.com",
					IngressRuleValue: networking.IngressRuleValue{
						HTTP: &networking.HTTPIngressRuleValue{
							Paths: []networking.HTTPIngressPath{{
								Path:     "/foo",
								PathType: &exactPathType,
								Backend:  defaultBackend,
							}},
						},
					},
				}}
				newIngress.Spec.TLS = []networking.IngressTLS{{Hosts: []string{"*.bar.com"}}}
				newIngress.Spec.Rules = []networking.IngressRule{{
					Host: "*.foo.com",
					IngressRuleValue: networking.IngressRuleValue{
						HTTP: &networking.HTTPIngressRuleValue{
							Paths: []networking.HTTPIngressPath{{
								Path:     "foo",
								PathType: &exactPathType,
								Backend:  defaultBackend,
							}},
						},
					},
				}}
			},
			expectedErrs: field.ErrorList{field.Invalid(field.NewPath("spec").Child("rules").Index(0).Child("http").Child("paths").Index(0).Child("path"), "foo", `must be an absolute path`)},
		},
		"change invalid rules with wildcard host -> invalid rules": {
			tweakIngresses: func(newIngress, oldIngress *networking.Ingress) {
				oldIngress.Spec.TLS = []networking.IngressTLS{{Hosts: []string{"*.bar.com"}}}
				oldIngress.Spec.Rules = []networking.IngressRule{{
					Host: "*.foo.com",
					IngressRuleValue: networking.IngressRuleValue{
						HTTP: &networking.HTTPIngressRuleValue{
							Paths: []networking.HTTPIngressPath{{
								Path:     "foo",
								PathType: &exactPathType,
								Backend:  defaultBackend,
							}},
						},
					},
				}}
				newIngress.Spec.TLS = []networking.IngressTLS{{Hosts: []string{"*.bar.com"}}}
				newIngress.Spec.Rules = []networking.IngressRule{{
					Host: "*.foo.com",
					IngressRuleValue: networking.IngressRuleValue{
						HTTP: &networking.HTTPIngressRuleValue{
							Paths: []networking.HTTPIngressPath{{
								Path:     "bar",
								PathType: &exactPathType,
								Backend:  defaultBackend,
							}},
						},
					},
				}}
			},
		},
	}

	for name, testCase := range testCases {
		t.Run(name, func(t *testing.T) {
			newIngress := baseIngress.DeepCopy()
			oldIngress := baseIngress.DeepCopy()
			testCase.tweakIngresses(newIngress, oldIngress)

			errs := ValidateIngressUpdate(newIngress, oldIngress)

			if len(errs) != len(testCase.expectedErrs) {
				t.Fatalf("Expected %d errors, got %d (%+v)", len(testCase.expectedErrs), len(errs), errs)
			}

			for i, err := range errs {
				if err.Error() != testCase.expectedErrs[i].Error() {
					t.Fatalf("Expected error: %v, got %v", testCase.expectedErrs[i].Error(), err.Error())
				}
			}
		})
	}
}

type netIngressTweak func(ingressClass *networking.IngressClass)

func makeValidIngressClass(name, controller string, tweaks ...netIngressTweak) *networking.IngressClass {
	ingressClass := &networking.IngressClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: networking.IngressClassSpec{
			Controller: controller,
		},
	}

	for _, fn := range tweaks {
		fn(ingressClass)
	}
	return ingressClass
}

func makeIngressClassParams(apiGroup *string, kind, name string, scope, namespace *string) *networking.IngressClassParametersReference {
	return &networking.IngressClassParametersReference{
		APIGroup:  apiGroup,
		Kind:      kind,
		Name:      name,
		Scope:     scope,
		Namespace: namespace,
	}
}

func TestValidateIngressClass(t *testing.T) {
	setParams := func(params *networking.IngressClassParametersReference) netIngressTweak {
		return func(ingressClass *networking.IngressClass) {
			ingressClass.Spec.Parameters = params
		}
	}

	testCases := map[string]struct {
		ingressClass *networking.IngressClass
		expectedErrs field.ErrorList
	}{
		"valid name, valid controller": {
			ingressClass: makeValidIngressClass("test123", "foo.co/bar"),
			expectedErrs: field.ErrorList{},
		},
		"invalid name, valid controller": {
			ingressClass: makeValidIngressClass("test*123", "foo.co/bar"),
			expectedErrs: field.ErrorList{field.Invalid(field.NewPath("metadata.name"), "test*123", "a lowercase RFC 1123 subdomain must consist of lower case alphanumeric characters, '-' or '.', and must start and end with an alphanumeric character (e.g. 'example.com', regex used for validation is '[a-z0-9]([-a-z0-9]*[a-z0-9])?(\\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*')")},
		},
		"valid name, empty controller": {
			ingressClass: makeValidIngressClass("test123", ""),
			expectedErrs: field.ErrorList{field.Required(field.NewPath("spec.controller"), "")},
		},
		"valid name, controller max length": {
			ingressClass: makeValidIngressClass("test123", "foo.co/"+strings.Repeat("a", 243)),
			expectedErrs: field.ErrorList{},
		},
		"valid name, controller too long": {
			ingressClass: makeValidIngressClass("test123", "foo.co/"+strings.Repeat("a", 244)),
			expectedErrs: field.ErrorList{field.TooLong(field.NewPath("spec.controller"), "" /*unused*/, 250)},
		},
		"valid name, valid controller, valid params": {
			ingressClass: makeValidIngressClass("test123", "foo.co/bar",
				setParams(makeIngressClassParams(utilpointer.String("example.com"), "foo", "bar", utilpointer.String("Cluster"), nil)),
			),
			expectedErrs: field.ErrorList{},
		},
		"valid name, valid controller, invalid params (no kind)": {
			ingressClass: makeValidIngressClass("test123", "foo.co/bar",
				setParams(makeIngressClassParams(utilpointer.String("example.com"), "", "bar", utilpointer.String("Cluster"), nil)),
			),
			expectedErrs: field.ErrorList{field.Required(field.NewPath("spec.parameters.kind"), "kind is required")},
		},
		"valid name, valid controller, invalid params (no name)": {
			ingressClass: makeValidIngressClass("test123", "foo.co/bar",
				setParams(makeIngressClassParams(utilpointer.String("example.com"), "foo", "", utilpointer.String("Cluster"), nil)),
			),
			expectedErrs: field.ErrorList{field.Required(field.NewPath("spec.parameters.name"), "name is required")},
		},
		"valid name, valid controller, invalid params (bad kind)": {
			ingressClass: makeValidIngressClass("test123", "foo.co/bar",
				setParams(makeIngressClassParams(nil, "foo/", "bar", utilpointer.String("Cluster"), nil)),
			),
			expectedErrs: field.ErrorList{field.Invalid(field.NewPath("spec.parameters.kind"), "foo/", "may not contain '/'")},
		},
		"valid name, valid controller, invalid params (bad scope)": {
			ingressClass: makeValidIngressClass("test123", "foo.co/bar",
				setParams(makeIngressClassParams(nil, "foo", "bar", utilpointer.String("bad-scope"), nil)),
			),
			expectedErrs: field.ErrorList{field.NotSupported(field.NewPath("spec.parameters.scope"),
				"bad-scope", []string{"Cluster", "Namespace"})},
		},
		"valid name, valid controller, valid Namespace scope": {
			ingressClass: makeValidIngressClass("test123", "foo.co/bar",
				setParams(makeIngressClassParams(nil, "foo", "bar", utilpointer.String("Namespace"), utilpointer.String("foo-ns"))),
			),
			expectedErrs: field.ErrorList{},
		},
		"valid name, valid controller, valid scope, invalid namespace": {
			ingressClass: makeValidIngressClass("test123", "foo.co/bar",
				setParams(makeIngressClassParams(nil, "foo", "bar", utilpointer.String("Namespace"), utilpointer.String("foo_ns"))),
			),
			expectedErrs: field.ErrorList{field.Invalid(field.NewPath("spec.parameters.namespace"), "foo_ns",
				"a lowercase RFC 1123 label must consist of lower case alphanumeric characters or '-',"+
					" and must start and end with an alphanumeric character (e.g. 'my-name',  or '123-abc', "+
					"regex used for validation is '[a-z0-9]([-a-z0-9]*[a-z0-9])?')")},
		},
		"valid name, valid controller, valid Cluster scope": {
			ingressClass: makeValidIngressClass("test123", "foo.co/bar",
				setParams(makeIngressClassParams(nil, "foo", "bar", utilpointer.String("Cluster"), nil)),
			),
			expectedErrs: field.ErrorList{},
		},
		"valid name, valid controller, invalid scope": {
			ingressClass: makeValidIngressClass("test123", "foo.co/bar",
				setParams(makeIngressClassParams(nil, "foo", "bar", nil, utilpointer.String("foo_ns"))),
			),
			expectedErrs: field.ErrorList{
				field.Required(field.NewPath("spec.parameters.scope"), ""),
			},
		},
		"namespace not set when scope is Namespace": {
			ingressClass: makeValidIngressClass("test123", "foo.co/bar",
				setParams(makeIngressClassParams(nil, "foo", "bar", utilpointer.String("Namespace"), nil)),
			),
			expectedErrs: field.ErrorList{field.Required(field.NewPath("spec.parameters.namespace"),
				"`parameters.scope` is set to 'Namespace'")},
		},
		"namespace is forbidden when scope is Cluster": {
			ingressClass: makeValidIngressClass("test123", "foo.co/bar",
				setParams(makeIngressClassParams(nil, "foo", "bar", utilpointer.String("Cluster"), utilpointer.String("foo-ns"))),
			),
			expectedErrs: field.ErrorList{field.Forbidden(field.NewPath("spec.parameters.namespace"),
				"`parameters.scope` is set to 'Cluster'")},
		},
		"empty namespace is forbidden when scope is Cluster": {
			ingressClass: makeValidIngressClass("test123", "foo.co/bar",
				setParams(makeIngressClassParams(nil, "foo", "bar", utilpointer.String("Cluster"), utilpointer.String(""))),
			),
			expectedErrs: field.ErrorList{field.Forbidden(field.NewPath("spec.parameters.namespace"),
				"`parameters.scope` is set to 'Cluster'")},
		},
	}

	for name, testCase := range testCases {
		t.Run(name, func(t *testing.T) {
			errs := ValidateIngressClass(testCase.ingressClass)

			if len(errs) != len(testCase.expectedErrs) {
				t.Fatalf("Expected %d errors, got %d (%+v)", len(testCase.expectedErrs), len(errs), errs)
			}

			for i, err := range errs {
				if err.Error() != testCase.expectedErrs[i].Error() {
					t.Fatalf("Expected error: %v, got %v", testCase.expectedErrs[i].Error(), err.Error())
				}
			}
		})
	}
}

func TestValidateIngressClassUpdate(t *testing.T) {
	setResourceVersion := func(version string) netIngressTweak {
		return func(ingressClass *networking.IngressClass) {
			ingressClass.ObjectMeta.ResourceVersion = version
		}
	}

	setParams := func(params *networking.IngressClassParametersReference) netIngressTweak {
		return func(ingressClass *networking.IngressClass) {
			ingressClass.Spec.Parameters = params
		}
	}

	testCases := map[string]struct {
		newIngressClass *networking.IngressClass
		oldIngressClass *networking.IngressClass
		expectedErrs    field.ErrorList
	}{
		"name change": {
			newIngressClass: makeValidIngressClass("test123", "foo.co/bar", setResourceVersion("2")),
			oldIngressClass: makeValidIngressClass("test123", "foo.co/different"),
			expectedErrs:    field.ErrorList{field.Invalid(field.NewPath("spec").Child("controller"), "foo.co/bar", apimachineryvalidation.FieldImmutableErrorMsg)},
		},
		"parameters change": {
			newIngressClass: makeValidIngressClass("test123", "foo.co/bar",
				setResourceVersion("2"),
				setParams(
					makeIngressClassParams(utilpointer.String("v1"), "ConfigMap", "foo", utilpointer.String("Namespace"), utilpointer.String("bar")),
				),
			),
			oldIngressClass: makeValidIngressClass("test123", "foo.co/bar"),
			expectedErrs:    field.ErrorList{},
		},
	}

	for name, testCase := range testCases {
		t.Run(name, func(t *testing.T) {
			errs := ValidateIngressClassUpdate(testCase.newIngressClass, testCase.oldIngressClass)

			if len(errs) != len(testCase.expectedErrs) {
				t.Fatalf("Expected %d errors, got %d (%+v)", len(testCase.expectedErrs), len(errs), errs)
			}

			for i, err := range errs {
				if err.Error() != testCase.expectedErrs[i].Error() {
					t.Fatalf("Expected error: %v, got %v", testCase.expectedErrs[i].Error(), err.Error())
				}
			}
		})
	}
}

func TestValidateIngressTLS(t *testing.T) {
	pathTypeImplementationSpecific := networking.PathTypeImplementationSpecific
	serviceBackend := &networking.IngressServiceBackend{
		Name: "defaultbackend",
		Port: networking.ServiceBackendPort{
			Number: 80,
		},
	}
	defaultBackend := networking.IngressBackend{
		Service: serviceBackend,
	}
	newValid := func() networking.Ingress {
		return networking.Ingress{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "foo",
				Namespace: metav1.NamespaceDefault,
			},
			Spec: networking.IngressSpec{
				DefaultBackend: &defaultBackend,
				Rules: []networking.IngressRule{{
					Host: "foo.bar.com",
					IngressRuleValue: networking.IngressRuleValue{
						HTTP: &networking.HTTPIngressRuleValue{
							Paths: []networking.HTTPIngressPath{{
								Path:     "/foo",
								PathType: &pathTypeImplementationSpecific,
								Backend:  defaultBackend,
							}},
						},
					},
				}},
			},
			Status: networking.IngressStatus{
				LoadBalancer: networking.IngressLoadBalancerStatus{
					Ingress: []networking.IngressLoadBalancerIngress{
						{IP: "127.0.0.1"},
					},
				},
			},
		}
	}

	errorCases := map[string]networking.Ingress{}

	wildcardHost := "foo.*.bar.com"
	badWildcardTLS := newValid()
	badWildcardTLS.Spec.Rules[0].Host = "*.foo.bar.com"
	badWildcardTLS.Spec.TLS = []networking.IngressTLS{{
		Hosts: []string{wildcardHost},
	}}
	badWildcardTLSErr := fmt.Sprintf("spec.tls[0].hosts[0]: Invalid value: '%v'", wildcardHost)
	errorCases[badWildcardTLSErr] = badWildcardTLS

	for k, v := range errorCases {
		errs := validateIngress(&v, IngressValidationOptions{})
		if len(errs) == 0 {
			t.Errorf("expected failure for %q", k)
		} else {
			s := strings.Split(k, ":")
			err := errs[0]
			if err.Field != s[0] || !strings.Contains(err.Error(), s[1]) {
				t.Errorf("unexpected error: %q, expected: %q", err, k)
			}
		}
	}

	// Test for wildcard host and wildcard TLS
	validCases := map[string]networking.Ingress{}
	wildHost := "*.bar.com"
	goodWildcardTLS := newValid()
	goodWildcardTLS.Spec.Rules[0].Host = "*.bar.com"
	goodWildcardTLS.Spec.TLS = []networking.IngressTLS{{
		Hosts: []string{wildHost},
	}}
	validCases[fmt.Sprintf("spec.tls[0].hosts: Valid value: '%v'", wildHost)] = goodWildcardTLS
	for k, v := range validCases {
		errs := validateIngress(&v, IngressValidationOptions{})
		if len(errs) != 0 {
			t.Errorf("expected success for %q", k)
		}
	}
}

// TestValidateEmptyIngressTLS verifies that an empty TLS configuration can be
// specified, which ingress controllers may interpret to mean that TLS should be
// used with a default certificate that the ingress controller furnishes.
func TestValidateEmptyIngressTLS(t *testing.T) {
	pathTypeImplementationSpecific := networking.PathTypeImplementationSpecific
	serviceBackend := &networking.IngressServiceBackend{
		Name: "defaultbackend",
		Port: networking.ServiceBackendPort{
			Number: 443,
		},
	}
	defaultBackend := networking.IngressBackend{
		Service: serviceBackend,
	}
	newValid := func() networking.Ingress {
		return networking.Ingress{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "foo",
				Namespace: metav1.NamespaceDefault,
			},
			Spec: networking.IngressSpec{
				Rules: []networking.IngressRule{{
					Host: "foo.bar.com",
					IngressRuleValue: networking.IngressRuleValue{
						HTTP: &networking.HTTPIngressRuleValue{
							Paths: []networking.HTTPIngressPath{{
								PathType: &pathTypeImplementationSpecific,
								Backend:  defaultBackend,
							}},
						},
					},
				}},
			},
		}
	}

	validCases := map[string]networking.Ingress{}
	goodEmptyTLS := newValid()
	goodEmptyTLS.Spec.TLS = []networking.IngressTLS{
		{},
	}
	validCases[fmt.Sprintf("spec.tls[0]: Valid value: %v", goodEmptyTLS.Spec.TLS[0])] = goodEmptyTLS
	goodEmptyHosts := newValid()
	goodEmptyHosts.Spec.TLS = []networking.IngressTLS{{
		Hosts: []string{},
	}}
	validCases[fmt.Sprintf("spec.tls[0]: Valid value: %v", goodEmptyHosts.Spec.TLS[0])] = goodEmptyHosts
	for k, v := range validCases {
		errs := validateIngress(&v, IngressValidationOptions{})
		if len(errs) != 0 {
			t.Errorf("expected success for %q", k)
		}
	}
}

func TestValidateIngressStatusUpdate(t *testing.T) {
	serviceBackend := &networking.IngressServiceBackend{
		Name: "defaultbackend",
		Port: networking.ServiceBackendPort{
			Number: 80,
		},
	}
	defaultBackend := networking.IngressBackend{
		Service: serviceBackend,
	}

	newValid := func() networking.Ingress {
		return networking.Ingress{
			ObjectMeta: metav1.ObjectMeta{
				Name:            "foo",
				Namespace:       metav1.NamespaceDefault,
				ResourceVersion: "9",
			},
			Spec: networking.IngressSpec{
				DefaultBackend: &defaultBackend,
				Rules: []networking.IngressRule{{
					Host: "foo.bar.com",
					IngressRuleValue: networking.IngressRuleValue{
						HTTP: &networking.HTTPIngressRuleValue{
							Paths: []networking.HTTPIngressPath{{
								Path:    "/foo",
								Backend: defaultBackend,
							}},
						},
					},
				}},
			},
			Status: networking.IngressStatus{
				LoadBalancer: networking.IngressLoadBalancerStatus{
					Ingress: []networking.IngressLoadBalancerIngress{
						{IP: "127.0.0.1", Hostname: "foo.bar.com"},
					},
				},
			},
		}
	}
	oldValue := newValid()
	newValue := newValid()
	newValue.Status = networking.IngressStatus{
		LoadBalancer: networking.IngressLoadBalancerStatus{
			Ingress: []networking.IngressLoadBalancerIngress{
				{IP: "127.0.0.2", Hostname: "foo.com"},
			},
		},
	}
	invalidIP := newValid()
	invalidIP.Status = networking.IngressStatus{
		LoadBalancer: networking.IngressLoadBalancerStatus{
			Ingress: []networking.IngressLoadBalancerIngress{
				{IP: "abcd", Hostname: "foo.com"},
			},
		},
	}
	invalidHostname := newValid()
	invalidHostname.Status = networking.IngressStatus{
		LoadBalancer: networking.IngressLoadBalancerStatus{
			Ingress: []networking.IngressLoadBalancerIngress{
				{IP: "127.0.0.1", Hostname: "127.0.0.1"},
			},
		},
	}

	errs := ValidateIngressStatusUpdate(&newValue, &oldValue)
	if len(errs) != 0 {
		t.Errorf("Unexpected error %v", errs)
	}

	errorCases := map[string]networking.Ingress{
		"status.loadBalancer.ingress[0].ip: Invalid value":       invalidIP,
		"status.loadBalancer.ingress[0].hostname: Invalid value": invalidHostname,
	}
	for k, v := range errorCases {
		errs := ValidateIngressStatusUpdate(&v, &oldValue)
		if len(errs) == 0 {
			t.Errorf("expected failure for %s", k)
		} else {
			s := strings.Split(k, ":")
			err := errs[0]
			if err.Field != s[0] || !strings.Contains(err.Error(), s[1]) {
				t.Errorf("unexpected error: %q, expected: %q", err, k)
			}
		}
	}
}

func TestValidateIPAddress(t *testing.T) {
	testCases := map[string]struct {
		expectedErrors int
		ipAddress      *networking.IPAddress
	}{
		"empty-ipaddress-bad-name": {
			expectedErrors: 1,
			ipAddress: &networking.IPAddress{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-name",
				},
				Spec: networking.IPAddressSpec{
					ParentRef: &networking.ParentReference{
						Group:     "",
						Resource:  "services",
						Name:      "foo",
						Namespace: "bar",
					},
				},
			},
		},
		"empty-ipaddress-bad-name-no-parent-reference": {
			expectedErrors: 2,
			ipAddress: &networking.IPAddress{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-name",
				},
			},
		},

		"good-ipaddress": {
			expectedErrors: 0,
			ipAddress: &networking.IPAddress{
				ObjectMeta: metav1.ObjectMeta{
					Name: "192.168.1.1",
				},
				Spec: networking.IPAddressSpec{
					ParentRef: &networking.ParentReference{
						Group:     "",
						Resource:  "services",
						Name:      "foo",
						Namespace: "bar",
					},
				},
			},
		},
		"good-ipaddress-gateway": {
			expectedErrors: 0,
			ipAddress: &networking.IPAddress{
				ObjectMeta: metav1.ObjectMeta{
					Name: "192.168.1.1",
				},
				Spec: networking.IPAddressSpec{
					ParentRef: &networking.ParentReference{
						Group:     "gateway.networking.k8s.io",
						Resource:  "gateway",
						Name:      "foo",
						Namespace: "bar",
					},
				},
			},
		},
		"good-ipv6address": {
			expectedErrors: 0,
			ipAddress: &networking.IPAddress{
				ObjectMeta: metav1.ObjectMeta{
					Name: "2001:4860:4860::8888",
				},
				Spec: networking.IPAddressSpec{
					ParentRef: &networking.ParentReference{
						Group:     "",
						Resource:  "services",
						Name:      "foo",
						Namespace: "bar",
					},
				},
			},
		},
		"non-canonica-ipv6address": {
			expectedErrors: 1,
			ipAddress: &networking.IPAddress{
				ObjectMeta: metav1.ObjectMeta{
					Name: "2001:4860:4860:0::8888",
				},
				Spec: networking.IPAddressSpec{
					ParentRef: &networking.ParentReference{
						Group:     "",
						Resource:  "services",
						Name:      "foo",
						Namespace: "bar",
					},
				},
			},
		},
		"missing-ipaddress-reference": {
			expectedErrors: 1,
			ipAddress: &networking.IPAddress{
				ObjectMeta: metav1.ObjectMeta{
					Name: "192.168.1.1",
				},
			},
		},
		"wrong-ipaddress-reference": {
			expectedErrors: 1,
			ipAddress: &networking.IPAddress{
				ObjectMeta: metav1.ObjectMeta{
					Name: "192.168.1.1",
				},
				Spec: networking.IPAddressSpec{
					ParentRef: &networking.ParentReference{
						Group:     "custom.resource.com",
						Resource:  "services",
						Name:      "foo$%&",
						Namespace: "",
					},
				},
			},
		},
		"wrong-ipaddress-reference-multiple-errors": {
			expectedErrors: 4,
			ipAddress: &networking.IPAddress{
				ObjectMeta: metav1.ObjectMeta{
					Name: "192.168.1.1",
				},
				Spec: networking.IPAddressSpec{
					ParentRef: &networking.ParentReference{
						Group:     ".cust@m.resource.com",
						Resource:  "",
						Name:      "",
						Namespace: "bar$$$$$%@",
					},
				},
			},
		},
	}

	for name, testCase := range testCases {
		t.Run(name, func(t *testing.T) {
			errs := ValidateIPAddress(testCase.ipAddress)
			if len(errs) != testCase.expectedErrors {
				t.Errorf("Expected %d errors, got %d errors: %v", testCase.expectedErrors, len(errs), errs)
			}
		})
	}
}

func TestValidateIPAddressUpdate(t *testing.T) {
	old := &networking.IPAddress{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "192.168.1.1",
			ResourceVersion: "1",
		},
		Spec: networking.IPAddressSpec{
			ParentRef: &networking.ParentReference{
				Group:     "custom.resource.com",
				Resource:  "services",
				Name:      "foo",
				Namespace: "bar",
			},
		},
	}

	testCases := []struct {
		name      string
		new       func(svc *networking.IPAddress) *networking.IPAddress
		expectErr bool
	}{{
		name: "Successful update, no changes",
		new: func(old *networking.IPAddress) *networking.IPAddress {
			out := old.DeepCopy()
			return out
		},
		expectErr: false,
	},

		{
			name: "Failed update, update spec.ParentRef",
			new: func(svc *networking.IPAddress) *networking.IPAddress {
				out := svc.DeepCopy()
				out.Spec.ParentRef = &networking.ParentReference{
					Group:     "custom.resource.com",
					Resource:  "Gateway",
					Name:      "foo",
					Namespace: "bar",
				}

				return out
			}, expectErr: true,
		}, {
			name: "Failed update, delete spec.ParentRef",
			new: func(svc *networking.IPAddress) *networking.IPAddress {
				out := svc.DeepCopy()
				out.Spec.ParentRef = nil
				return out
			}, expectErr: true,
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			err := ValidateIPAddressUpdate(testCase.new(old), old)
			if !testCase.expectErr && err != nil {
				t.Errorf("ValidateIPAddressUpdate must be successful for test '%s', got %v", testCase.name, err)
			}
			if testCase.expectErr && err == nil {
				t.Errorf("ValidateIPAddressUpdate must return error for test: %s, but got nil", testCase.name)
			}
		})
	}
}

func TestValidateServiceCIDR(t *testing.T) {

	testCases := map[string]struct {
		expectedErrors int
		ipRange        *networking.ServiceCIDR
	}{
		"empty-iprange": {
			expectedErrors: 1,
			ipRange: &networking.ServiceCIDR{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-name",
				},
			},
		},
		"three-ipranges": {
			expectedErrors: 1,
			ipRange: &networking.ServiceCIDR{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-name",
				},
				Spec: networking.ServiceCIDRSpec{
					CIDRs: []string{"192.168.0.0/24", "fd00::/64", "10.0.0.0/16"},
				},
			},
		},
		"good-iprange-ipv4": {
			expectedErrors: 0,
			ipRange: &networking.ServiceCIDR{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-name",
				},
				Spec: networking.ServiceCIDRSpec{
					CIDRs: []string{"192.168.0.0/24"},
				},
			},
		},
		"good-iprange-ipv6": {
			expectedErrors: 0,
			ipRange: &networking.ServiceCIDR{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-name",
				},
				Spec: networking.ServiceCIDRSpec{
					CIDRs: []string{"fd00:1234::/64"},
				},
			},
		},
		"good-iprange-ipv4-ipv6": {
			expectedErrors: 0,
			ipRange: &networking.ServiceCIDR{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-name",
				},
				Spec: networking.ServiceCIDRSpec{
					CIDRs: []string{"192.168.0.0/24", "fd00:1234::/64"},
				},
			},
		},
		"not-iprange-ipv4": {
			expectedErrors: 1,
			ipRange: &networking.ServiceCIDR{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-name",
				},
				Spec: networking.ServiceCIDRSpec{
					CIDRs: []string{"asdasdasd"},
				},
			},
		},
		"iponly-iprange-ipv4": {
			expectedErrors: 1,
			ipRange: &networking.ServiceCIDR{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-name",
				},
				Spec: networking.ServiceCIDRSpec{
					CIDRs: []string{"192.168.0.1"},
				},
			},
		},
		"badip-iprange-ipv4": {
			expectedErrors: 1,
			ipRange: &networking.ServiceCIDR{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-name",
				},
				Spec: networking.ServiceCIDRSpec{
					CIDRs: []string{"192.168.0.1/24"},
				},
			},
		},
		"badip-iprange-ipv6": {
			expectedErrors: 1,
			ipRange: &networking.ServiceCIDR{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-name",
				},
				Spec: networking.ServiceCIDRSpec{
					CIDRs: []string{"fd00:1234::2/64"},
				},
			},
		},
		"badip-iprange-caps-ipv6": {
			expectedErrors: 2,
			ipRange: &networking.ServiceCIDR{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-name",
				},
				Spec: networking.ServiceCIDRSpec{
					CIDRs: []string{"FD00:1234::2/64"},
				},
			},
		},
		"good-iprange-ipv4-bad-ipv6": {
			expectedErrors: 1,
			ipRange: &networking.ServiceCIDR{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-name",
				},
				Spec: networking.ServiceCIDRSpec{
					CIDRs: []string{"192.168.0.0/24", "FD00:1234::/64"},
				},
			},
		},
		"good-iprange-ipv6-bad-ipv4": {
			expectedErrors: 1,
			ipRange: &networking.ServiceCIDR{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-name",
				},
				Spec: networking.ServiceCIDRSpec{
					CIDRs: []string{"192.168.007.0/24", "fd00:1234::/64"},
				},
			},
		},
	}

	for name, testCase := range testCases {
		t.Run(name, func(t *testing.T) {
			errs := ValidateServiceCIDR(testCase.ipRange)
			if len(errs) != testCase.expectedErrors {
				t.Errorf("Expected %d errors, got %d errors: %v", testCase.expectedErrors, len(errs), errs)
			}
		})
	}
}

func TestValidateServiceCIDRUpdate(t *testing.T) {
	oldServiceCIDR := &networking.ServiceCIDR{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "mysvc",
			ResourceVersion: "1",
		},
		Spec: networking.ServiceCIDRSpec{
			CIDRs: []string{"192.168.0.0/24", "fd00:1234::/64"},
		},
	}

	testCases := []struct {
		name      string
		svc       func(svc *networking.ServiceCIDR) *networking.ServiceCIDR
		expectErr bool
	}{
		{
			name: "Successful update, no changes",
			svc: func(svc *networking.ServiceCIDR) *networking.ServiceCIDR {
				out := svc.DeepCopy()
				return out
			},
			expectErr: false,
		},

		{
			name: "Failed update, update spec.CIDRs single stack",
			svc: func(svc *networking.ServiceCIDR) *networking.ServiceCIDR {
				out := svc.DeepCopy()
				out.Spec.CIDRs = []string{"10.0.0.0/16"}
				return out
			}, expectErr: true,
		},
		{
			name: "Failed update, update spec.CIDRs dual stack",
			svc: func(svc *networking.ServiceCIDR) *networking.ServiceCIDR {
				out := svc.DeepCopy()
				out.Spec.CIDRs = []string{"10.0.0.0/24", "fd00:1234::/64"}
				return out
			}, expectErr: true,
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			err := ValidateServiceCIDRUpdate(testCase.svc(oldServiceCIDR), oldServiceCIDR)
			if !testCase.expectErr && err != nil {
				t.Errorf("ValidateServiceCIDRUpdate must be successful for test '%s', got %v", testCase.name, err)
			}
			if testCase.expectErr && err == nil {
				t.Errorf("ValidateServiceCIDRUpdate must return error for test: %s, but got nil", testCase.name)
			}
		})
	}
}
