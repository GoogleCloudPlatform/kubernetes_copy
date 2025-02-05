/*
Copyright 2024 The Kubernetes Authors.

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

package endpoint

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	api "k8s.io/kubernetes/pkg/apis/core"
)

func Test_endpointsWarning(t *testing.T) {
	tests := []struct {
		name      string
		endpoints *api.Endpoints
		warnings  []string
	}{
		{
			name:      "empty Endpoints",
			endpoints: &api.Endpoints{},
			warnings:  nil,
		},
		{
			name: "valid Endpoints",
			endpoints: &api.Endpoints{
				Subsets: []api.EndpointSubset{
					{
						Addresses: []api.EndpointAddress{
							{IP: "1.2.3.4"},
							{IP: "fd00::1234"},
						},
					},
				},
			},
			warnings: nil,
		},
		{
			name: "bad Endpoints",
			endpoints: &api.Endpoints{
				Subsets: []api.EndpointSubset{
					{
						Addresses: []api.EndpointAddress{
							{IP: "fd00::1234"},
							{IP: "01.02.03.04"},
						},
					},
					{
						Addresses: []api.EndpointAddress{
							{IP: "::ffff:1.2.3.4"},
						},
					},
					{
						Addresses: []api.EndpointAddress{
							{IP: "1.2.3.4"},
						},
						NotReadyAddresses: []api.EndpointAddress{
							{IP: "::ffff:1.2.3.4"},
						},
					},
				},
			},
			warnings: []string{
				"subsets[0].addresses[1].ip",
				"subsets[1].addresses[0].ip",
				"subsets[2].notReadyAddresses[0].ip",
			},
		},
		{
			name: "bad Endpoints ignored because of annotation",
			endpoints: &api.Endpoints{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						"endpoints.kubernetes.io/last-change-trigger-time": "Tuesday",
					},
				},
				Subsets: []api.EndpointSubset{
					{
						Addresses: []api.EndpointAddress{
							{IP: "fd00::1234"},
							{IP: "01.02.03.04"},
						},
					},
				},
			},
			warnings: nil,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			warnings := endpointsWarnings(test.endpoints)
			ok := len(warnings) == len(test.warnings)
			if ok {
				for i := range warnings {
					if !strings.HasPrefix(warnings[i], test.warnings[i]) {
						ok = false
						break
					}
				}
			}
			if !ok {
				t.Errorf("Expected warnings for %v, got %v", test.warnings, warnings)
			}
		})
	}
}
