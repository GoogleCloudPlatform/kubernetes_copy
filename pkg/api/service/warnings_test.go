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

package service

import (
	"reflect"
	"testing"

	"github.com/google/go-cmp/cmp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	api "k8s.io/kubernetes/pkg/apis/core"
)

func TestGetWarningsForService(t *testing.T) {
	testCases := []struct {
		name        string
		tweakSvc    func(svc *api.Service) // Given a basic valid service, each test case can customize it.
		numWarnings int
	}{{
		name: "new topology mode set",
		tweakSvc: func(s *api.Service) {
			s.Annotations = map[string]string{api.AnnotationTopologyMode: "foo"}
		},
		numWarnings: 0,
	}, {
		name: "deprecated hints annotation set",
		tweakSvc: func(s *api.Service) {
			s.Annotations = map[string]string{api.DeprecatedAnnotationTopologyAwareHints: "foo"}
		},
		numWarnings: 1,
	}, {
		name: "externalIPs set when type is ExternalName",
		tweakSvc: func(s *api.Service) {
			s.Spec.Type = api.ServiceTypeExternalName
			s.Spec.ExternalIPs = []string{"1.2.3.4"}
		},
		numWarnings: 1,
	}, {
		name: "externalName set when type is not ExternalName",
		tweakSvc: func(s *api.Service) {
			s.Spec.Type = api.ServiceTypeClusterIP
			s.Spec.ExternalName = "example.com"
		},
		numWarnings: 1,
	}}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			svc := &api.Service{}
			tc.tweakSvc(svc)
			warnings := GetWarningsForService(svc, svc)
			if want, got := tc.numWarnings, len(warnings); got != want {
				t.Errorf("Unexpected warning list: expected %d, got %d\n%q", want, got, warnings)
			}
		})
	}
}

func TestGetWarningsForServiceClusterIPs(t *testing.T) {
	service := func(clusterIPs []string) *api.Service {
		svc := api.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "svc-test",
				Namespace: "ns",
			},
			Spec: api.ServiceSpec{
				Type: api.ServiceTypeClusterIP,
			},
		}

		if len(clusterIPs) > 0 {
			svc.Spec.ClusterIP = clusterIPs[0]
			svc.Spec.ClusterIPs = clusterIPs
		}
		return &svc
	}

	tests := []struct {
		name       string
		service    *api.Service
		oldService *api.Service
		want       []string
	}{
		{
			name:    "IPv4 No failures",
			service: service([]string{"192.12.2.2"}),
		},
		{
			name:    "IPv6 No failures",
			service: service([]string{"2001:db8::2"}),
		},
		{
			name:    "IPv4 with leading zeros",
			service: service([]string{"192.012.2.2"}),
			want: []string{
				`spec.clusterIPs[0]: non-standard IP address "192.012.2.2" will be considered invalid in a future Kubernetes release: use "192.12.2.2"`,
			},
		},
		{
			name:    "Dual Stack IPv4-IPv6 and IPv4 with leading zeros",
			service: service([]string{"192.012.2.2", "2001:db8::2"}),
			want: []string{
				`spec.clusterIPs[0]: non-standard IP address "192.012.2.2" will be considered invalid in a future Kubernetes release: use "192.12.2.2"`,
			},
		},
		{
			name:    "Dual Stack IPv6-IPv4 and IPv4 with leading zeros",
			service: service([]string{"2001:db8::2", "192.012.2.2"}),
			want: []string{
				`spec.clusterIPs[1]: non-standard IP address "192.012.2.2" will be considered invalid in a future Kubernetes release: use "192.12.2.2"`,
			},
		},
		{
			name:    "IPv6 non canonical format",
			service: service([]string{"2001:db8:0:0::2"}),
			want: []string{
				`spec.clusterIPs[0]: IPv6 address "2001:db8:0:0::2" should be in RFC 5952 canonical format ("2001:db8::2")`,
			},
		},
		{
			name:    "Dual Stack IPv4-IPv6 and IPv6 non-canonical format",
			service: service([]string{"192.12.2.2", "2001:db8:0:0::2"}),
			want: []string{
				`spec.clusterIPs[1]: IPv6 address "2001:db8:0:0::2" should be in RFC 5952 canonical format ("2001:db8::2")`,
			},
		},
		{
			name:    "Dual Stack IPv6-IPv4 and IPv6 non-canonical formats",
			service: service([]string{"2001:db8:0:0::2", "192.12.2.2"}),
			want: []string{
				`spec.clusterIPs[0]: IPv6 address "2001:db8:0:0::2" should be in RFC 5952 canonical format ("2001:db8::2")`,
			},
		},
		{
			name:    "Dual Stack IPv4-IPv6 and IPv4 with leading zeros and IPv6 non-canonical format",
			service: service([]string{"192.012.2.2", "2001:db8:0:0::2"}),
			want: []string{
				`spec.clusterIPs[0]: non-standard IP address "192.012.2.2" will be considered invalid in a future Kubernetes release: use "192.12.2.2"`,
				`spec.clusterIPs[1]: IPv6 address "2001:db8:0:0::2" should be in RFC 5952 canonical format ("2001:db8::2")`,
			},
		},
		{
			name:    "Dual Stack IPv6-IPv4 and IPv4 with leading zeros and IPv6 non-canonical format",
			service: service([]string{"2001:db8:0:0::2", "192.012.2.2"}),
			want: []string{
				`spec.clusterIPs[0]: IPv6 address "2001:db8:0:0::2" should be in RFC 5952 canonical format ("2001:db8::2")`,
				`spec.clusterIPs[1]: non-standard IP address "192.012.2.2" will be considered invalid in a future Kubernetes release: use "192.12.2.2"`,
			},
		},
		{
			name: "Service with all IPs fields with errors",
			service: &api.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "svc-test",
					Namespace: "ns",
				},
				Spec: api.ServiceSpec{
					ClusterIP:                "2001:db8:0:0::2",
					ClusterIPs:               []string{"2001:db8:0:0::2", "192.012.2.2"},
					ExternalIPs:              []string{"2001:db8:1:0::2", "10.012.2.2"},
					LoadBalancerIP:           "10.001.1.1",
					LoadBalancerSourceRanges: []string{"2001:db8:1:0::/64", "10.012.2.0/24"},
					Type:                     api.ServiceTypeClusterIP,
				},
			},
			want: []string{
				`spec.clusterIPs[0]: IPv6 address "2001:db8:0:0::2" should be in RFC 5952 canonical format ("2001:db8::2")`,
				`spec.clusterIPs[1]: non-standard IP address "192.012.2.2" will be considered invalid in a future Kubernetes release: use "192.12.2.2"`,
				`spec.externalIPs[0]: IPv6 address "2001:db8:1:0::2" should be in RFC 5952 canonical format ("2001:db8:1::2")`,
				`spec.externalIPs[1]: non-standard IP address "10.012.2.2" will be considered invalid in a future Kubernetes release: use "10.12.2.2"`,
				`spec.loadBalancerIP: non-standard IP address "10.001.1.1" will be considered invalid in a future Kubernetes release: use "10.1.1.1"`,
				`spec.loadBalancerSourceRanges[0]: IPv6 CIDR value "2001:db8:1:0::/64" should be in RFC 5952 canonical format ("2001:db8:1::/64")`,
				`spec.loadBalancerSourceRanges[1]: non-standard CIDR value "10.012.2.0/24" will be considered invalid in a future Kubernetes release: use "10.12.2.0/24"`,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := GetWarningsForService(tt.service, tt.oldService); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("GetWarningsForService() = %v", cmp.Diff(got, tt.want))
			}
		})
	}
}
