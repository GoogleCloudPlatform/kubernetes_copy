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

package prune

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

type testRESTMapper struct {
	meta.RESTMapper
	scope meta.RESTScope
}

func (m *testRESTMapper) RESTMapping(gk schema.GroupKind, versions ...string) (*meta.RESTMapping, error) {
	return &meta.RESTMapping{
		Resource: schema.GroupVersionResource{
			Group:    gk.Group,
			Version:  "",
			Resource: "",
		},
		GroupVersionKind: schema.GroupVersionKind{
			Group:   gk.Group,
			Version: "",
			Kind:    gk.Kind,
		},
		Scope: m.scope,
	}, nil
}

func TestGetRESTMappings(t *testing.T) {
	tests := []struct {
		mapper             *testRESTMapper
		pr                 []Resource
		namespaceSpecified bool
		expectedns         int
		expectednns        int
		expectederr        error
	}{
		{
			mapper:             &testRESTMapper{},
			pr:                 []Resource{},
			namespaceSpecified: false,
			expectedns:         14,
			expectednns:        2,
			expectederr:        nil,
		},
		{
			mapper:             &testRESTMapper{},
			pr:                 []Resource{},
			namespaceSpecified: true,
			expectedns:         14,
			expectednns:        0,
			expectederr:        nil,
		},
		{
			mapper: &testRESTMapper{},
			pr: []Resource{
				{"apps", "v1", "DaemonSet", true},
				{"core", "v1", "Pod", true},
				{"", "v1", "Foo2", false},
				{"foo", "v1", "Foo3", false},
			},
			namespaceSpecified: false,
			expectedns:         2,
			expectednns:        2,
			expectederr:        nil,
		},
	}

	for _, tc := range tests {
		actualns, actualnns, actualerr := GetRESTMappings(tc.mapper, tc.pr, tc.namespaceSpecified)
		if tc.expectederr != nil {
			assert.NotEmptyf(t, actualerr, "getRESTMappings error expected but not fired")
		}
		assert.Len(t, actualns, tc.expectedns, "getRESTMappings failed expected number namespaced %d actual %d", tc.expectedns, len(actualns))
		assert.Len(t, actualnns, tc.expectednns, "getRESTMappings failed expected number nonnamespaced %d actual %d", tc.expectednns, len(actualnns))
	}
}

func TestParsePruneResources(t *testing.T) {
	tests := []struct {
		mapper   *testRESTMapper
		gvks     []string
		expected []Resource
		err      bool
	}{
		{
			mapper: &testRESTMapper{
				scope: meta.RESTScopeNamespace,
			},
			gvks:     nil,
			expected: []Resource{},
			err:      false,
		},
		{
			mapper: &testRESTMapper{
				scope: meta.RESTScopeNamespace,
			},
			gvks:     []string{"group/kind/version/test"},
			expected: []Resource{},
			err:      true,
		},
		{
			mapper: &testRESTMapper{
				scope: meta.RESTScopeNamespace,
			},
			gvks:     []string{"group/kind/version"},
			expected: []Resource{{group: "group", version: "kind", kind: "version", namespaced: true}},
			err:      false,
		},
		{
			mapper: &testRESTMapper{
				scope: meta.RESTScopeRoot,
			},
			gvks:     []string{"group/kind/version"},
			expected: []Resource{{group: "group", version: "kind", kind: "version", namespaced: false}},
			err:      false,
		},
	}

	for _, tc := range tests {
		actual, err := ParseResources(tc.mapper, tc.gvks)
		if tc.err {
			assert.NotEmptyf(t, err, "parsePruneResources error expected but not fired")
		} else {
			assert.Equalf(t, tc.expected, actual, "parsePruneResources failed expected %v actual %v", tc.expected, actual)
		}
	}
}
