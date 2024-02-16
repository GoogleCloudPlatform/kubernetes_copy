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

package node

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	apitesting "k8s.io/kubernetes/pkg/api/testing"
	api "k8s.io/kubernetes/pkg/apis/core"

	// ensure types are installed
	_ "k8s.io/kubernetes/pkg/apis/core/install"
)

func TestMatchNode(t *testing.T) {
	testFieldMap := map[bool][]fields.Set{
		true: {
			{"metadata.name": "foo"},
		},
		false: {
			{"foo": "bar"},
		},
	}

	for expectedResult, fieldSet := range testFieldMap {
		for _, field := range fieldSet {
			m := MatchNode(labels.Everything(), field.AsSelector())
			_, matchesSingle := m.MatchesSingle()
			if e, a := expectedResult, matchesSingle; e != a {
				t.Errorf("%+v: expected %v, got %v", fieldSet, e, a)
			}
		}
	}
}

func TestSelectableFieldLabelConversions(t *testing.T) {
	apitesting.TestSelectableFieldLabelConversionsOfKind(t,
		"v1",
		"Node",
		NodeToSelectableFields(&api.Node{}),
		nil,
	)
}

// helper creates a NodeNode with a set of PodCIDRs, Spec.ConfigSource, Status.Config
func makeNode(podCIDRs []string, addSpecDynamicConfig bool, addStatusDynamicConfig bool) *api.Node {
	node := &api.Node{
		Spec: api.NodeSpec{
			PodCIDRs: podCIDRs,
		},
	}
	if addSpecDynamicConfig {
		node.Spec.ConfigSource = &api.NodeConfigSource{}
	}
	if addStatusDynamicConfig {
		node.Status = api.NodeStatus{
			Config: &api.NodeConfigStatus{},
		}
	}

	return node
}

func TestDropFields(t *testing.T) {
	testCases := []struct {
		name        string
		node        *api.Node
		oldNode     *api.Node
		compareNode *api.Node
	}{
		{
			name:        "nil pod cidrs",
			node:        makeNode(nil, false, false),
			oldNode:     nil,
			compareNode: makeNode(nil, false, false),
		},
		{
			name:        "empty pod ips",
			node:        makeNode([]string{}, false, false),
			oldNode:     nil,
			compareNode: makeNode([]string{}, false, false),
		},
		{
			name:        "single family ipv6",
			node:        makeNode([]string{"2000::/10"}, false, false),
			compareNode: makeNode([]string{"2000::/10"}, false, false),
		},
		{
			name:        "single family ipv4",
			node:        makeNode([]string{"10.0.0.0/8"}, false, false),
			compareNode: makeNode([]string{"10.0.0.0/8"}, false, false),
		},
		{
			name:        "dualstack 4-6",
			node:        makeNode([]string{"10.0.0.0/8", "2000::/10"}, false, false),
			compareNode: makeNode([]string{"10.0.0.0/8", "2000::/10"}, false, false),
		},
		{
			name:        "dualstack 6-4",
			node:        makeNode([]string{"2000::/10", "10.0.0.0/8"}, false, false),
			compareNode: makeNode([]string{"2000::/10", "10.0.0.0/8"}, false, false),
		},
		{
			name:        "new with no Spec.ConfigSource and no Status.Config",
			node:        makeNode(nil, false, false),
			oldNode:     nil,
			compareNode: makeNode(nil, false, false),
		},
		{
			name:        "new with Spec.ConfigSource and no Status.Config",
			node:        makeNode(nil, true, false),
			oldNode:     nil,
			compareNode: makeNode(nil, false, false),
		},
		{
			name:        "new with Spec.ConfigSource and Status.Config",
			node:        makeNode(nil, true, true),
			oldNode:     nil,
			compareNode: makeNode(nil, false, false),
		},
		{
			name:        "update with Spec.ConfigSource and Status.Config (old has none)",
			node:        makeNode(nil, true, true),
			oldNode:     makeNode(nil, false, false),
			compareNode: makeNode(nil, false, true),
		},
		{
			name:        "update with Spec.ConfigSource and Status.Config (old has them)",
			node:        makeNode(nil, true, true),
			oldNode:     makeNode(nil, true, true),
			compareNode: makeNode(nil, true, true),
		},
		{
			name:        "update with Spec.ConfigSource and Status.Config (old has Status.Config)",
			node:        makeNode(nil, true, true),
			oldNode:     makeNode(nil, false, true),
			compareNode: makeNode(nil, false, true),
		},
	}

	for _, tc := range testCases {
		func() {
			dropDisabledFields(tc.node, tc.oldNode)

			old := tc.oldNode.DeepCopy()
			// old node  should never be changed
			if !reflect.DeepEqual(tc.oldNode, old) {
				t.Errorf("%v: old node changed: %v", tc.name, cmp.Diff(tc.oldNode, old))
			}

			if !reflect.DeepEqual(tc.node, tc.compareNode) {
				t.Errorf("%v: unexpected node spec: %v", tc.name, cmp.Diff(tc.node, tc.compareNode))
			}
		}()
	}
}
func TestValidateUpdate(t *testing.T) {
	tests := []struct {
		oldNode api.Node
		node    api.Node
		valid   bool
	}{
		{api.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "hugepage-change-values-from-0",
			},
			Status: api.NodeStatus{
				Capacity: api.ResourceList{
					api.ResourceName("hugepages-2Mi"): resource.MustParse("0"),
					api.ResourceName("hugepages-1Gi"): resource.MustParse("2Gi"),
				},
			},
		}, api.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "hugepage-change-values-from-0",
			},
			Status: api.NodeStatus{
				Capacity: api.ResourceList{
					api.ResourceName("hugepages-2Mi"): resource.MustParse("2Gi"),
					api.ResourceName("hugepages-1Gi"): resource.MustParse("2Gi"),
				},
			},
		}, true},
		{api.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "hugepage-change-values",
			},
			Status: api.NodeStatus{
				Capacity: api.ResourceList{
					api.ResourceName("hugepages-2Mi"): resource.MustParse("1Gi"),
					api.ResourceName("hugepages-1Gi"): resource.MustParse("2Gi"),
				},
			},
		}, api.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "hugepage-change-values",
			},
			Status: api.NodeStatus{
				Capacity: api.ResourceList{
					api.ResourceName("hugepages-2Mi"): resource.MustParse("2Gi"),
					api.ResourceName("hugepages-1Gi"): resource.MustParse("2Gi"),
				},
			},
		}, true},
	}
	for i, test := range tests {
		test.node.ObjectMeta.ResourceVersion = "1"
		errs := (nodeStrategy{}).ValidateUpdate(context.Background(), &test.node, &test.oldNode)
		if test.valid && len(errs) > 0 {
			t.Errorf("%d: Unexpected error: %v", i, errs)
			t.Logf("%#v vs %#v", test.oldNode.ObjectMeta, test.node.ObjectMeta)
		}
		if !test.valid && len(errs) == 0 {
			t.Errorf("%d: Unexpected non-error", i)
		}
	}
}
func TestValidate(t *testing.T) {
	tests := []struct {
		node  api.Node
		valid bool
	}{
		{api.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "one-hugepage-size",
			},
			Status: api.NodeStatus{
				Capacity: api.ResourceList{
					api.ResourceCPU:                   resource.MustParse("100"),
					api.ResourceMemory:                resource.MustParse("10000"),
					api.ResourceName("hugepages-2Mi"): resource.MustParse("0"),
					api.ResourceName("hugepages-1Gi"): resource.MustParse("2Gi"),
				},
			},
		}, true},
		{api.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "multiple-hugepage-sizes",
			},
			Status: api.NodeStatus{
				Capacity: api.ResourceList{
					api.ResourceCPU:                   resource.MustParse("100"),
					api.ResourceMemory:                resource.MustParse("10000"),
					api.ResourceName("hugepages-2Mi"): resource.MustParse("2Gi"),
					api.ResourceName("hugepages-1Gi"): resource.MustParse("2Gi"),
				},
			},
		}, true},
	}
	for i, test := range tests {
		test.node.ObjectMeta.ResourceVersion = "1"
		errs := (nodeStrategy{}).Validate(context.Background(), &test.node)
		if test.valid && len(errs) > 0 {
			t.Errorf("%d: Unexpected error: %v", i, errs)
		}
		if !test.valid && len(errs) == 0 {
			t.Errorf("%d: Unexpected non-error", i)
		}
	}
}

func TestWarningOnUpdateAndCreate(t *testing.T) {
	tests := []struct {
		oldNode     api.Node
		node        api.Node
		warningText string
	}{
		{
			api.Node{},
			api.Node{},
			"",
		},
		{
			api.Node{},
			//nolint:staticcheck // ignore deprecation warning
			api.Node{Spec: api.NodeSpec{ConfigSource: &api.NodeConfigSource{}}},
			"spec.configSource",
		},
		{
			//nolint:staticcheck // ignore deprecation warning
			api.Node{Spec: api.NodeSpec{ConfigSource: &api.NodeConfigSource{}}},
			//nolint:staticcheck // ignore deprecation warning
			api.Node{Spec: api.NodeSpec{ConfigSource: &api.NodeConfigSource{}}},
			"spec.configSource",
		},
		{
			//nolint:staticcheck // ignore deprecation warning
			api.Node{Spec: api.NodeSpec{ConfigSource: &api.NodeConfigSource{}}},
			api.Node{},
			"",
		},
		{
			api.Node{},
			api.Node{Spec: api.NodeSpec{DoNotUseExternalID: "externalID"}},
			"spec.externalID",
		},
		{
			api.Node{Spec: api.NodeSpec{DoNotUseExternalID: "externalID"}},
			api.Node{Spec: api.NodeSpec{DoNotUseExternalID: "externalID"}},
			"spec.externalID",
		},
		{
			api.Node{Spec: api.NodeSpec{DoNotUseExternalID: "externalID"}},
			api.Node{},
			"",
		},
		{
			api.Node{},
			api.Node{Spec: api.NodeSpec{PodCIDRs: []string{"10.0.1.0/24", "fd00::/64"}}},
			"",
		},
		{
			api.Node{},
			api.Node{Spec: api.NodeSpec{PodCIDRs: []string{"010.000.001.000/24", "fd00::/64"}}},
			"spec.podCIDRs[0]",
		},
		{
			api.Node{},
			api.Node{Spec: api.NodeSpec{PodCIDRs: []string{"10.0.1.0/24", "fd00::1234/64"}}},
			"spec.podCIDRs[1]",
		},
		{
			api.Node{Spec: api.NodeSpec{PodCIDRs: []string{"010.000.001.000/24", "fd00::1234/64"}}},
			api.Node{Spec: api.NodeSpec{PodCIDRs: []string{"10.0.1.0/24", "fd00::/64"}}},
			"",
		},
	}
	for i, test := range tests {
		warnings := (nodeStrategy{}).WarningsOnUpdate(context.Background(), &test.node, &test.oldNode)
		if (test.warningText != "" && len(warnings) != 1) || (test.warningText == "" && len(warnings) != 0) {
			t.Errorf("%d: Unexpected warnings count: %v", i, warnings)
			t.Logf("%#v vs %#v", test.oldNode.ObjectMeta, test.node.ObjectMeta)
		} else if test.warningText != "" && !strings.Contains(warnings[0], test.warningText) {
			t.Errorf("%d: Wrong warning message: %v", i, warnings[0])
		}

		warnings = (nodeStrategy{}).WarningsOnCreate(context.Background(), &test.node)
		if (test.warningText != "" && len(warnings) != 1) || (test.warningText == "" && len(warnings) != 0) {
			t.Errorf("%d: Unexpected warnings count: %v", i, warnings)
			t.Logf("%#v vs %#v", test.oldNode.ObjectMeta, test.node.ObjectMeta)
		} else if test.warningText != "" && !strings.Contains(warnings[0], test.warningText) {
			t.Errorf("%d: Wrong warning message: %v", i, warnings[0])
		}
	}
}
