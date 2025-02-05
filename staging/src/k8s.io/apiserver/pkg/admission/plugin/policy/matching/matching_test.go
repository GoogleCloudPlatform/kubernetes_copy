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

package matching

import (
	"fmt"
	"strings"
	"testing"

	v1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/admission"
	"k8s.io/apiserver/pkg/admission/plugin/webhook/predicates/namespace"
	"k8s.io/apiserver/pkg/admission/plugin/webhook/predicates/object"
	"k8s.io/apiserver/pkg/apis/example"
)

var _ MatchCriteria = &fakeCriteria{}

type fakeCriteria struct {
	matchResources v1.MatchResources
}

func (fc *fakeCriteria) GetMatchResources() v1.MatchResources {
	return fc.matchResources
}

func (fc *fakeCriteria) GetParsedNamespaceSelector() (labels.Selector, error) {
	return metav1.LabelSelectorAsSelector(fc.matchResources.NamespaceSelector)
}

func (fc *fakeCriteria) GetParsedObjectSelector() (labels.Selector, error) {
	return metav1.LabelSelectorAsSelector(fc.matchResources.ObjectSelector)
}

func gvr(group, version, resource string) schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: group, Version: version, Resource: resource}
}

func gvk(group, version, kind string) schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: group, Version: version, Kind: kind}
}

func TestMatcher(t *testing.T) {
	a := &Matcher{namespaceMatcher: &namespace.Matcher{}, objectMatcher: &object.Matcher{}}

	allScopes := v1.AllScopes
	exactMatch := v1.Exact
	equivalentMatch := v1.Equivalent

	mapper := runtime.NewEquivalentResourceRegistryWithIdentity(func(resource schema.GroupResource) string {
		if resource.Resource == "deployments" {
			// co-locate deployments in all API groups
			return "/deployments"
		}
		return ""
	})
	mapper.RegisterKindFor(gvr("extensions", "v1beta1", "deployments"), "", gvk("extensions", "v1beta1", "Deployment"))
	mapper.RegisterKindFor(gvr("apps", "v1", "deployments"), "", gvk("apps", "v1", "Deployment"))
	mapper.RegisterKindFor(gvr("apps", "v1beta1", "deployments"), "", gvk("apps", "v1beta1", "Deployment"))
	mapper.RegisterKindFor(gvr("apps", "v1alpha1", "deployments"), "", gvk("apps", "v1alpha1", "Deployment"))

	mapper.RegisterKindFor(gvr("extensions", "v1beta1", "deployments"), "scale", gvk("extensions", "v1beta1", "Scale"))
	mapper.RegisterKindFor(gvr("apps", "v1", "deployments"), "scale", gvk("autoscaling", "v1", "Scale"))
	mapper.RegisterKindFor(gvr("apps", "v1beta1", "deployments"), "scale", gvk("apps", "v1beta1", "Scale"))
	mapper.RegisterKindFor(gvr("apps", "v1alpha1", "deployments"), "scale", gvk("apps", "v1alpha1", "Scale"))

	// register invalid kinds to trigger an error
	mapper.RegisterKindFor(gvr("example.com", "v1", "widgets"), "", gvk("", "", ""))
	mapper.RegisterKindFor(gvr("example.com", "v2", "widgets"), "", gvk("", "", ""))

	interfaces := &admission.RuntimeObjectInterfaces{EquivalentResourceMapper: mapper}

	// TODO write test cases for name matching and exclude matching
	testcases := []struct {
		name string

		criteria *v1.MatchResources
		attrs    admission.Attributes

		expectMatches       bool
		expectMatchKind     schema.GroupVersionKind
		expectMatchResource schema.GroupVersionResource
		expectErr           string
	}{
		{
			name:          "no rules (just write)",
			criteria:      &v1.MatchResources{NamespaceSelector: &metav1.LabelSelector{}, ResourceRules: []v1.NamedRuleWithOperations{}},
			attrs:         admission.NewAttributesRecord(nil, nil, gvk("apps", "v1", "Deployment"), "ns", "name", gvr("apps", "v1", "deployments"), "", admission.Create, &metav1.CreateOptions{}, false, nil),
			expectMatches: false,
		},
		{
			name: "wildcard rule, match as requested",
			criteria: &v1.MatchResources{
				NamespaceSelector: &metav1.LabelSelector{},
				ObjectSelector:    &metav1.LabelSelector{},
				ResourceRules: []v1.NamedRuleWithOperations{{
					RuleWithOperations: v1.RuleWithOperations{
						Operations: []v1.OperationType{"*"},
						Rule:       v1.Rule{APIGroups: []string{"*"}, APIVersions: []string{"*"}, Resources: []string{"*"}, Scope: &allScopes},
					},
				}}},
			attrs:           admission.NewAttributesRecord(nil, nil, gvk("apps", "v1", "Deployment"), "ns", "name", gvr("apps", "v1", "deployments"), "", admission.Create, &metav1.CreateOptions{}, false, nil),
			expectMatches:   true,
			expectMatchKind: gvk("apps", "v1", "Deployment"),
		},
		{
			name: "specific rules, prefer exact match",
			criteria: &v1.MatchResources{
				NamespaceSelector: &metav1.LabelSelector{},
				ObjectSelector:    &metav1.LabelSelector{},
				ResourceRules: []v1.NamedRuleWithOperations{{
					RuleWithOperations: v1.RuleWithOperations{
						Operations: []v1.OperationType{"*"},
						Rule:       v1.Rule{APIGroups: []string{"extensions"}, APIVersions: []string{"v1"}, Resources: []string{"deployments"}, Scope: &allScopes},
					},
				}, {
					RuleWithOperations: v1.RuleWithOperations{
						Operations: []v1.OperationType{"*"},
						Rule:       v1.Rule{APIGroups: []string{"apps"}, APIVersions: []string{"v1beta1"}, Resources: []string{"deployments"}, Scope: &allScopes},
					},
				}, {
					RuleWithOperations: v1.RuleWithOperations{
						Operations: []v1.OperationType{"*"},
						Rule:       v1.Rule{APIGroups: []string{"apps"}, APIVersions: []string{"v1"}, Resources: []string{"deployments"}, Scope: &allScopes},
					},
				}}},
			attrs:           admission.NewAttributesRecord(nil, nil, gvk("apps", "v1", "Deployment"), "ns", "name", gvr("apps", "v1", "deployments"), "", admission.Create, &metav1.CreateOptions{}, false, nil),
			expectMatches:   true,
			expectMatchKind: gvk("apps", "v1", "Deployment"),
		},
		{
			name: "specific rules, match miss",
			criteria: &v1.MatchResources{
				NamespaceSelector: &metav1.LabelSelector{},
				ObjectSelector:    &metav1.LabelSelector{},
				ResourceRules: []v1.NamedRuleWithOperations{{
					RuleWithOperations: v1.RuleWithOperations{
						Operations: []v1.OperationType{"*"},
						Rule:       v1.Rule{APIGroups: []string{"extensions"}, APIVersions: []string{"v1beta1"}, Resources: []string{"deployments"}, Scope: &allScopes},
					},
				}, {
					RuleWithOperations: v1.RuleWithOperations{
						Operations: []v1.OperationType{"*"},
						Rule:       v1.Rule{APIGroups: []string{"apps"}, APIVersions: []string{"v1beta1"}, Resources: []string{"deployments"}, Scope: &allScopes},
					},
				}}},
			attrs:         admission.NewAttributesRecord(nil, nil, gvk("apps", "v1", "Deployment"), "ns", "name", gvr("apps", "v1", "deployments"), "", admission.Create, &metav1.CreateOptions{}, false, nil),
			expectMatches: false,
		},
		{
			name: "specific rules, exact match miss",
			criteria: &v1.MatchResources{
				MatchPolicy:       &exactMatch,
				NamespaceSelector: &metav1.LabelSelector{},
				ObjectSelector:    &metav1.LabelSelector{},
				ResourceRules: []v1.NamedRuleWithOperations{{
					RuleWithOperations: v1.RuleWithOperations{
						Operations: []v1.OperationType{"*"},
						Rule:       v1.Rule{APIGroups: []string{"extensions"}, APIVersions: []string{"v1beta1"}, Resources: []string{"deployments"}, Scope: &allScopes},
					},
				}, {
					RuleWithOperations: v1.RuleWithOperations{
						Operations: []v1.OperationType{"*"},
						Rule:       v1.Rule{APIGroups: []string{"apps"}, APIVersions: []string{"v1beta1"}, Resources: []string{"deployments"}, Scope: &allScopes},
					},
				}}},
			attrs:         admission.NewAttributesRecord(nil, nil, gvk("apps", "v1", "Deployment"), "ns", "name", gvr("apps", "v1", "deployments"), "", admission.Create, &metav1.CreateOptions{}, false, nil),
			expectMatches: false,
		},
		{
			name: "specific rules, equivalent match, prefer extensions",
			criteria: &v1.MatchResources{
				MatchPolicy:       &equivalentMatch,
				NamespaceSelector: &metav1.LabelSelector{},
				ObjectSelector:    &metav1.LabelSelector{},
				ResourceRules: []v1.NamedRuleWithOperations{{
					RuleWithOperations: v1.RuleWithOperations{
						Operations: []v1.OperationType{"*"},
						Rule:       v1.Rule{APIGroups: []string{"extensions"}, APIVersions: []string{"v1beta1"}, Resources: []string{"deployments"}, Scope: &allScopes},
					},
				}, {
					RuleWithOperations: v1.RuleWithOperations{
						Operations: []v1.OperationType{"*"},
						Rule:       v1.Rule{APIGroups: []string{"apps"}, APIVersions: []string{"v1beta1"}, Resources: []string{"deployments"}, Scope: &allScopes},
					},
				}}},
			attrs:               admission.NewAttributesRecord(nil, nil, gvk("apps", "v1", "Deployment"), "ns", "name", gvr("apps", "v1", "deployments"), "", admission.Create, &metav1.CreateOptions{}, false, nil),
			expectMatches:       true,
			expectMatchResource: gvr("extensions", "v1beta1", "deployments"),
			expectMatchKind:     gvk("extensions", "v1beta1", "Deployment"),
		},
		{
			name: "specific rules, equivalent match, prefer apps",
			criteria: &v1.MatchResources{
				MatchPolicy:       &equivalentMatch,
				NamespaceSelector: &metav1.LabelSelector{},
				ObjectSelector:    &metav1.LabelSelector{},
				ResourceRules: []v1.NamedRuleWithOperations{{
					RuleWithOperations: v1.RuleWithOperations{
						Operations: []v1.OperationType{"*"},
						Rule:       v1.Rule{APIGroups: []string{"apps"}, APIVersions: []string{"v1beta1"}, Resources: []string{"deployments"}, Scope: &allScopes},
					},
				}, {
					RuleWithOperations: v1.RuleWithOperations{
						Operations: []v1.OperationType{"*"},
						Rule:       v1.Rule{APIGroups: []string{"extensions"}, APIVersions: []string{"v1beta1"}, Resources: []string{"deployments"}, Scope: &allScopes},
					},
				}}},
			attrs:               admission.NewAttributesRecord(nil, nil, gvk("apps", "v1", "Deployment"), "ns", "name", gvr("apps", "v1", "deployments"), "", admission.Create, &metav1.CreateOptions{}, false, nil),
			expectMatches:       true,
			expectMatchResource: gvr("apps", "v1beta1", "deployments"),
			expectMatchKind:     gvk("apps", "v1beta1", "Deployment"),
		},

		{
			name: "specific rules, subresource prefer exact match",
			criteria: &v1.MatchResources{
				NamespaceSelector: &metav1.LabelSelector{},
				ObjectSelector:    &metav1.LabelSelector{},
				ResourceRules: []v1.NamedRuleWithOperations{{
					RuleWithOperations: v1.RuleWithOperations{
						Operations: []v1.OperationType{"*"},
						Rule:       v1.Rule{APIGroups: []string{"extensions"}, APIVersions: []string{"v1beta1"}, Resources: []string{"deployments", "deployments/scale"}, Scope: &allScopes},
					},
				}, {
					RuleWithOperations: v1.RuleWithOperations{
						Operations: []v1.OperationType{"*"},
						Rule:       v1.Rule{APIGroups: []string{"apps"}, APIVersions: []string{"v1beta1"}, Resources: []string{"deployments", "deployments/scale"}, Scope: &allScopes},
					},
				}, {
					RuleWithOperations: v1.RuleWithOperations{
						Operations: []v1.OperationType{"*"},
						Rule:       v1.Rule{APIGroups: []string{"apps"}, APIVersions: []string{"v1"}, Resources: []string{"deployments", "deployments/scale"}, Scope: &allScopes},
					},
				}}},
			attrs:           admission.NewAttributesRecord(nil, nil, gvk("autoscaling", "v1", "Scale"), "ns", "name", gvr("apps", "v1", "deployments"), "scale", admission.Create, &metav1.CreateOptions{}, false, nil),
			expectMatches:   true,
			expectMatchKind: gvk("autoscaling", "v1", "Scale"),
		},
		{
			name: "specific rules, subresource match miss",
			criteria: &v1.MatchResources{
				NamespaceSelector: &metav1.LabelSelector{},
				ObjectSelector:    &metav1.LabelSelector{},
				ResourceRules: []v1.NamedRuleWithOperations{{
					RuleWithOperations: v1.RuleWithOperations{
						Operations: []v1.OperationType{"*"},
						Rule:       v1.Rule{APIGroups: []string{"extensions"}, APIVersions: []string{"v1beta1"}, Resources: []string{"deployments", "deployments/scale"}, Scope: &allScopes},
					},
				}, {
					RuleWithOperations: v1.RuleWithOperations{
						Operations: []v1.OperationType{"*"},
						Rule:       v1.Rule{APIGroups: []string{"apps"}, APIVersions: []string{"v1beta1"}, Resources: []string{"deployments", "deployments/scale"}, Scope: &allScopes},
					},
				}}},
			attrs:         admission.NewAttributesRecord(nil, nil, gvk("autoscaling", "v1", "Scale"), "ns", "name", gvr("apps", "v1", "deployments"), "scale", admission.Create, &metav1.CreateOptions{}, false, nil),
			expectMatches: false,
		},
		{
			name: "specific rules, subresource exact match miss",
			criteria: &v1.MatchResources{
				MatchPolicy:       &exactMatch,
				NamespaceSelector: &metav1.LabelSelector{},
				ObjectSelector:    &metav1.LabelSelector{},
				ResourceRules: []v1.NamedRuleWithOperations{{
					RuleWithOperations: v1.RuleWithOperations{
						Operations: []v1.OperationType{"*"},
						Rule:       v1.Rule{APIGroups: []string{"extensions"}, APIVersions: []string{"v1beta1"}, Resources: []string{"deployments", "deployments/scale"}, Scope: &allScopes},
					},
				}, {
					RuleWithOperations: v1.RuleWithOperations{
						Operations: []v1.OperationType{"*"},
						Rule:       v1.Rule{APIGroups: []string{"apps"}, APIVersions: []string{"v1beta1"}, Resources: []string{"deployments", "deployments/scale"}, Scope: &allScopes},
					},
				}}},
			attrs:         admission.NewAttributesRecord(nil, nil, gvk("autoscaling", "v1", "Scale"), "ns", "name", gvr("apps", "v1", "deployments"), "scale", admission.Create, &metav1.CreateOptions{}, false, nil),
			expectMatches: false,
		},
		{
			name: "specific rules, subresource equivalent match, prefer extensions",
			criteria: &v1.MatchResources{
				MatchPolicy:       &equivalentMatch,
				NamespaceSelector: &metav1.LabelSelector{},
				ObjectSelector:    &metav1.LabelSelector{},
				ResourceRules: []v1.NamedRuleWithOperations{{
					RuleWithOperations: v1.RuleWithOperations{
						Operations: []v1.OperationType{"*"},
						Rule:       v1.Rule{APIGroups: []string{"extensions"}, APIVersions: []string{"v1beta1"}, Resources: []string{"deployments", "deployments/scale"}, Scope: &allScopes},
					},
				}, {
					RuleWithOperations: v1.RuleWithOperations{
						Operations: []v1.OperationType{"*"},
						Rule:       v1.Rule{APIGroups: []string{"apps"}, APIVersions: []string{"v1beta1"}, Resources: []string{"deployments", "deployments/scale"}, Scope: &allScopes},
					},
				}}},
			attrs:               admission.NewAttributesRecord(nil, nil, gvk("autoscaling", "v1", "Scale"), "ns", "name", gvr("apps", "v1", "deployments"), "scale", admission.Create, &metav1.CreateOptions{}, false, nil),
			expectMatches:       true,
			expectMatchResource: gvr("extensions", "v1beta1", "deployments"),
			expectMatchKind:     gvk("extensions", "v1beta1", "Scale"),
		},
		{
			name: "specific rules, subresource equivalent match, prefer apps",
			criteria: &v1.MatchResources{
				MatchPolicy:       &equivalentMatch,
				NamespaceSelector: &metav1.LabelSelector{},
				ObjectSelector:    &metav1.LabelSelector{},
				ResourceRules: []v1.NamedRuleWithOperations{{
					RuleWithOperations: v1.RuleWithOperations{
						Operations: []v1.OperationType{"*"},
						Rule:       v1.Rule{APIGroups: []string{"apps"}, APIVersions: []string{"v1beta1"}, Resources: []string{"deployments", "deployments/scale"}, Scope: &allScopes},
					},
				}, {
					RuleWithOperations: v1.RuleWithOperations{
						Operations: []v1.OperationType{"*"},
						Rule:       v1.Rule{APIGroups: []string{"extensions"}, APIVersions: []string{"v1beta1"}, Resources: []string{"deployments", "deployments/scale"}, Scope: &allScopes},
					},
				}}},
			attrs:               admission.NewAttributesRecord(nil, nil, gvk("autoscaling", "v1", "Scale"), "ns", "name", gvr("apps", "v1", "deployments"), "scale", admission.Create, &metav1.CreateOptions{}, false, nil),
			expectMatches:       true,
			expectMatchResource: gvr("apps", "v1beta1", "deployments"),
			expectMatchKind:     gvk("apps", "v1beta1", "Scale"),
		},
		{
			name: "specific rules, prefer exact match and name match",
			criteria: &v1.MatchResources{
				NamespaceSelector: &metav1.LabelSelector{},
				ObjectSelector:    &metav1.LabelSelector{},
				ResourceRules: []v1.NamedRuleWithOperations{{
					ResourceNames: []string{"name"},
					RuleWithOperations: v1.RuleWithOperations{
						Operations: []v1.OperationType{"*"},
						Rule:       v1.Rule{APIGroups: []string{"apps"}, APIVersions: []string{"v1"}, Resources: []string{"deployments"}, Scope: &allScopes},
					},
				}}},
			attrs:           admission.NewAttributesRecord(nil, nil, gvk("autoscaling", "v1", "Scale"), "ns", "name", gvr("apps", "v1", "deployments"), "", admission.Create, &metav1.CreateOptions{}, false, nil),
			expectMatches:   true,
			expectMatchKind: gvk("autoscaling", "v1", "Scale"),
		},
		{
			name: "specific rules, prefer exact match and name match miss",
			criteria: &v1.MatchResources{
				NamespaceSelector: &metav1.LabelSelector{},
				ObjectSelector:    &metav1.LabelSelector{},
				ResourceRules: []v1.NamedRuleWithOperations{{
					ResourceNames: []string{"wrong-name"},
					RuleWithOperations: v1.RuleWithOperations{
						Operations: []v1.OperationType{"*"},
						Rule:       v1.Rule{APIGroups: []string{"apps"}, APIVersions: []string{"v1"}, Resources: []string{"deployments"}, Scope: &allScopes},
					},
				}}},
			attrs:         admission.NewAttributesRecord(nil, nil, gvk("autoscaling", "v1", "Scale"), "ns", "name", gvr("apps", "v1", "deployments"), "", admission.Create, &metav1.CreateOptions{}, false, nil),
			expectMatches: false,
		},
		{
			name: "specific rules, subresource equivalent match, prefer extensions and name match",
			criteria: &v1.MatchResources{
				MatchPolicy:       &equivalentMatch,
				NamespaceSelector: &metav1.LabelSelector{},
				ObjectSelector:    &metav1.LabelSelector{},
				ResourceRules: []v1.NamedRuleWithOperations{{
					ResourceNames: []string{"name"},
					RuleWithOperations: v1.RuleWithOperations{
						Operations: []v1.OperationType{"*"},
						Rule:       v1.Rule{APIGroups: []string{"apps"}, APIVersions: []string{"v1"}, Resources: []string{"deployments", "deployments/scale"}, Scope: &allScopes},
					},
				}}},
			attrs:               admission.NewAttributesRecord(nil, nil, gvk("autoscaling", "v1", "Scale"), "ns", "name", gvr("extensions", "v1beta1", "deployments"), "scale", admission.Create, &metav1.CreateOptions{}, false, nil),
			expectMatches:       true,
			expectMatchResource: gvr("apps", "v1", "deployments"),
			expectMatchKind:     gvk("autoscaling", "v1", "Scale"),
		},
		{
			name: "specific rules, subresource equivalent match, prefer extensions and name match miss",
			criteria: &v1.MatchResources{
				MatchPolicy:       &equivalentMatch,
				NamespaceSelector: &metav1.LabelSelector{},
				ObjectSelector:    &metav1.LabelSelector{},
				ResourceRules: []v1.NamedRuleWithOperations{{
					ResourceNames: []string{"wrong-name"},
					RuleWithOperations: v1.RuleWithOperations{
						Operations: []v1.OperationType{"*"},
						Rule:       v1.Rule{APIGroups: []string{"apps"}, APIVersions: []string{"v1"}, Resources: []string{"deployments", "deployments/scale"}, Scope: &allScopes},
					},
				}}},
			attrs:         admission.NewAttributesRecord(nil, nil, gvk("autoscaling", "v1", "Scale"), "ns", "name", gvr("extensions", "v1beta1", "deployments"), "scale", admission.Create, &metav1.CreateOptions{}, false, nil),
			expectMatches: false,
		},
		{
			name: "exclude resource match on miss",
			criteria: &v1.MatchResources{
				NamespaceSelector: &metav1.LabelSelector{},
				ObjectSelector:    &metav1.LabelSelector{},
				ResourceRules: []v1.NamedRuleWithOperations{{
					RuleWithOperations: v1.RuleWithOperations{
						Operations: []v1.OperationType{"*"},
						Rule:       v1.Rule{APIGroups: []string{"*"}, APIVersions: []string{"*"}, Resources: []string{"*"}, Scope: &allScopes},
					},
				}},
				ExcludeResourceRules: []v1.NamedRuleWithOperations{{
					RuleWithOperations: v1.RuleWithOperations{
						Operations: []v1.OperationType{"*"},
						Rule:       v1.Rule{APIGroups: []string{"extensions"}, APIVersions: []string{"v1beta1"}, Resources: []string{"deployments"}, Scope: &allScopes},
					},
				}},
			},
			attrs:           admission.NewAttributesRecord(nil, nil, gvk("autoscaling", "v1", "Scale"), "ns", "name", gvr("apps", "v1", "deployments"), "", admission.Create, &metav1.CreateOptions{}, false, nil),
			expectMatches:   true,
			expectMatchKind: gvk("autoscaling", "v1", "Scale"),
		},
		{
			name: "exclude resource miss on match",
			criteria: &v1.MatchResources{
				NamespaceSelector: &metav1.LabelSelector{},
				ObjectSelector:    &metav1.LabelSelector{},
				ResourceRules: []v1.NamedRuleWithOperations{{
					RuleWithOperations: v1.RuleWithOperations{
						Operations: []v1.OperationType{"*"},
						Rule:       v1.Rule{APIGroups: []string{"*"}, APIVersions: []string{"*"}, Resources: []string{"*"}, Scope: &allScopes},
					},
				}},
				ExcludeResourceRules: []v1.NamedRuleWithOperations{{
					RuleWithOperations: v1.RuleWithOperations{
						Operations: []v1.OperationType{"*"},
						Rule:       v1.Rule{APIGroups: []string{"extensions"}, APIVersions: []string{"v1beta1"}, Resources: []string{"deployments"}, Scope: &allScopes},
					},
				}},
			},
			attrs:         admission.NewAttributesRecord(nil, nil, gvk("autoscaling", "v1", "Scale"), "ns", "name", gvr("extensions", "v1beta1", "deployments"), "", admission.Create, &metav1.CreateOptions{}, false, nil),
			expectMatches: false,
		},
		{
			name: "treat empty ResourceRules as match",
			criteria: &v1.MatchResources{
				NamespaceSelector: &metav1.LabelSelector{},
				ObjectSelector:    &metav1.LabelSelector{},
				ExcludeResourceRules: []v1.NamedRuleWithOperations{{
					RuleWithOperations: v1.RuleWithOperations{
						Operations: []v1.OperationType{"*"},
						Rule:       v1.Rule{APIGroups: []string{"extensions"}, APIVersions: []string{"v1beta1"}, Resources: []string{"deployments"}, Scope: &allScopes},
					},
				}},
			},
			attrs:         admission.NewAttributesRecord(nil, nil, gvk("autoscaling", "v1", "Scale"), "ns", "name", gvr("apps", "v1", "deployments"), "", admission.Create, &metav1.CreateOptions{}, false, nil),
			expectMatches: true,
		},
		{
			name: "treat non-empty ResourceRules as no match",
			criteria: &v1.MatchResources{
				NamespaceSelector: &metav1.LabelSelector{},
				ObjectSelector:    &metav1.LabelSelector{},
				ResourceRules:     []v1.NamedRuleWithOperations{{}},
			},
			attrs:         admission.NewAttributesRecord(nil, nil, gvk("autoscaling", "v1", "Scale"), "ns", "name", gvr("apps", "v1", "deployments"), "", admission.Create, &metav1.CreateOptions{}, false, nil),
			expectMatches: false,
		},
		{
			name: "erroring namespace selector on otherwise non-matching rule doesn't error",
			criteria: &v1.MatchResources{
				NamespaceSelector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "key ", Operator: "In", Values: []string{"bad value"}}}},
				ObjectSelector:    &metav1.LabelSelector{},
				ResourceRules: []v1.NamedRuleWithOperations{{
					RuleWithOperations: v1.RuleWithOperations{
						Rule:       v1.Rule{APIGroups: []string{"*"}, APIVersions: []string{"*"}, Resources: []string{"deployments"}},
						Operations: []v1.OperationType{"*"},
					},
				}},
			},
			attrs:         admission.NewAttributesRecord(&example.Pod{}, nil, gvk("example.apiserver.k8s.io", "v1", "Pod"), "ns", "name", gvr("example.apiserver.k8s.io", "v1", "pods"), "", admission.Create, &metav1.CreateOptions{}, false, nil),
			expectMatches: false,
			expectErr:     "",
		},
		{
			name: "erroring namespace selector on otherwise matching rule errors",
			criteria: &v1.MatchResources{
				NamespaceSelector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "key", Operator: "In", Values: []string{"bad value"}}}},
				ObjectSelector:    &metav1.LabelSelector{},
				ResourceRules: []v1.NamedRuleWithOperations{{
					RuleWithOperations: v1.RuleWithOperations{
						Rule:       v1.Rule{APIGroups: []string{"*"}, APIVersions: []string{"*"}, Resources: []string{"pods"}},
						Operations: []v1.OperationType{"*"},
					},
				}},
			},
			attrs:         admission.NewAttributesRecord(&example.Pod{}, nil, gvk("example.apiserver.k8s.io", "v1", "Pod"), "ns", "name", gvr("example.apiserver.k8s.io", "v1", "pods"), "", admission.Create, &metav1.CreateOptions{}, false, nil),
			expectMatches: false,
			expectErr:     "bad value",
		},
		{
			name: "erroring object selector on otherwise non-matching rule doesn't error",
			criteria: &v1.MatchResources{
				NamespaceSelector: &metav1.LabelSelector{},
				ObjectSelector:    &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "key", Operator: "In", Values: []string{"bad value"}}}},
				ResourceRules: []v1.NamedRuleWithOperations{{
					RuleWithOperations: v1.RuleWithOperations{
						Rule:       v1.Rule{APIGroups: []string{"*"}, APIVersions: []string{"*"}, Resources: []string{"deployments"}},
						Operations: []v1.OperationType{"*"},
					},
				}},
			},
			attrs:         admission.NewAttributesRecord(&example.Pod{}, nil, gvk("example.apiserver.k8s.io", "v1", "Pod"), "ns", "name", gvr("example.apiserver.k8s.io", "v1", "pods"), "", admission.Create, &metav1.CreateOptions{}, false, nil),
			expectMatches: false,
			expectErr:     "",
		},
		{
			name: "erroring object selector on otherwise matching rule errors",
			criteria: &v1.MatchResources{
				NamespaceSelector: &metav1.LabelSelector{},
				ObjectSelector:    &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "key", Operator: "In", Values: []string{"bad value"}}}},
				ResourceRules: []v1.NamedRuleWithOperations{{
					RuleWithOperations: v1.RuleWithOperations{
						Rule:       v1.Rule{APIGroups: []string{"*"}, APIVersions: []string{"*"}, Resources: []string{"pods"}},
						Operations: []v1.OperationType{"*"},
					},
				}},
			},
			attrs:         admission.NewAttributesRecord(&example.Pod{}, nil, gvk("example.apiserver.k8s.io", "v1", "Pod"), "ns", "name", gvr("example.apiserver.k8s.io", "v1", "pods"), "", admission.Create, &metav1.CreateOptions{}, false, nil),
			expectMatches: false,
			expectErr:     "bad value",
		},
	}

	for _, testcase := range testcases {
		t.Run(testcase.name, func(t *testing.T) {
			matches, matchResource, matchKind, err := a.Matches(testcase.attrs, interfaces, &fakeCriteria{matchResources: *testcase.criteria})
			if err != nil {
				if len(testcase.expectErr) == 0 {
					t.Fatal(err)
				}
				if !strings.Contains(err.Error(), testcase.expectErr) {
					t.Fatalf("expected error containing %q, got %s", testcase.expectErr, err.Error())
				}
				return
			} else if len(testcase.expectErr) > 0 {
				t.Fatalf("expected error %q, got no error", testcase.expectErr)
			}
			var emptyGVK schema.GroupVersionKind
			if testcase.expectMatchKind != emptyGVK {
				if testcase.expectMatchKind != matchKind {
					t.Fatalf("expected matchKind %v, got %v", testcase.expectMatchKind, matchKind)
				}
			}

			if matches != testcase.expectMatches {
				t.Fatalf("expected matches = %v; got %v", testcase.expectMatches, matches)
			}

			expectResource := testcase.expectMatchResource
			if !expectResource.Empty() && !matches {
				t.Fatalf("expectResource is non-empty, but did not match")
			} else if expectResource.Empty() {
				// Test for exact match by default. Tests that expect an equivalent
				// resource to match should explicitly state so by supplying
				// expectMatchResource
				expectResource = testcase.attrs.GetResource()
			}

			if matches {
				if matchResource != expectResource {
					t.Fatalf("expected matchResource %v, got %v", expectResource, matchResource)
				}
			}
		})
	}
}

type fakeNamespaceLister struct {
	namespaces map[string]*corev1.Namespace
}

func (f fakeNamespaceLister) List(selector labels.Selector) (ret []*corev1.Namespace, err error) {
	return nil, nil
}
func (f fakeNamespaceLister) Get(name string) (*corev1.Namespace, error) {
	ns, ok := f.namespaces[name]
	if ok {
		return ns, nil
	}
	return nil, errors.NewNotFound(corev1.Resource("namespaces"), name)
}

func BenchmarkMatcher(b *testing.B) {
	allScopes := v1.AllScopes
	equivalentMatch := v1.Equivalent

	namespace1Labels := map[string]string{"ns": "ns1"}
	namespace1 := corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "ns1",
			Labels: namespace1Labels,
		},
	}
	namespaceLister := fakeNamespaceLister{map[string]*corev1.Namespace{"ns": &namespace1}}

	mapper := runtime.NewEquivalentResourceRegistryWithIdentity(func(resource schema.GroupResource) string {
		if resource.Resource == "deployments" {
			// co-locate deployments in all API groups
			return "/deployments"
		}
		return ""
	})
	mapper.RegisterKindFor(gvr("extensions", "v1beta1", "deployments"), "", gvk("extensions", "v1beta1", "Deployment"))
	mapper.RegisterKindFor(gvr("apps", "v1", "deployments"), "", gvk("apps", "v1", "Deployment"))
	mapper.RegisterKindFor(gvr("apps", "v1beta1", "deployments"), "", gvk("apps", "v1beta1", "Deployment"))
	mapper.RegisterKindFor(gvr("apps", "v1alpha1", "deployments"), "", gvk("apps", "v1alpha1", "Deployment"))

	mapper.RegisterKindFor(gvr("extensions", "v1beta1", "deployments"), "scale", gvk("extensions", "v1beta1", "Scale"))
	mapper.RegisterKindFor(gvr("apps", "v1", "deployments"), "scale", gvk("autoscaling", "v1", "Scale"))
	mapper.RegisterKindFor(gvr("apps", "v1beta1", "deployments"), "scale", gvk("apps", "v1beta1", "Scale"))
	mapper.RegisterKindFor(gvr("apps", "v1alpha1", "deployments"), "scale", gvk("apps", "v1alpha1", "Scale"))

	mapper.RegisterKindFor(gvr("apps", "v1", "statefulset"), "", gvk("apps", "v1", "StatefulSet"))
	mapper.RegisterKindFor(gvr("apps", "v1beta1", "statefulset"), "", gvk("apps", "v1beta1", "StatefulSet"))
	mapper.RegisterKindFor(gvr("apps", "v1beta2", "statefulset"), "", gvk("apps", "v1beta2", "StatefulSet"))

	mapper.RegisterKindFor(gvr("apps", "v1", "statefulset"), "scale", gvk("apps", "v1", "Scale"))
	mapper.RegisterKindFor(gvr("apps", "v1beta1", "statefulset"), "scale", gvk("apps", "v1beta1", "Scale"))
	mapper.RegisterKindFor(gvr("apps", "v1alpha2", "statefulset"), "scale", gvk("apps", "v1beta2", "Scale"))

	nsSelector := make(map[string]string)
	for i := 0; i < 100; i++ {
		nsSelector[fmt.Sprintf("key-%d", i)] = fmt.Sprintf("val-%d", i)
	}

	mr := v1.MatchResources{
		MatchPolicy:       &equivalentMatch,
		NamespaceSelector: &metav1.LabelSelector{MatchLabels: nsSelector},
		ObjectSelector:    &metav1.LabelSelector{},
		ResourceRules: []v1.NamedRuleWithOperations{
			{
				RuleWithOperations: v1.RuleWithOperations{
					Operations: []v1.OperationType{"*"},
					Rule:       v1.Rule{APIGroups: []string{"apps"}, APIVersions: []string{"v1beta1"}, Resources: []string{"deployments", "deployments/scale"}, Scope: &allScopes},
				},
			},
			{
				RuleWithOperations: v1.RuleWithOperations{
					Operations: []v1.OperationType{"*"},
					Rule:       v1.Rule{APIGroups: []string{"extensions"}, APIVersions: []string{"v1beta1"}, Resources: []string{"deployments", "deployments/scale"}, Scope: &allScopes},
				},
			},
		},
	}

	criteria := &fakeCriteria{matchResources: mr}
	attrs := admission.NewAttributesRecord(nil, nil, gvk("autoscaling", "v1", "Scale"), "ns", "name", gvr("apps", "v1", "deployments"), "scale", admission.Create, &metav1.CreateOptions{}, false, nil)
	interfaces := &admission.RuntimeObjectInterfaces{EquivalentResourceMapper: mapper}
	namespaceProvider := &namespace.Provider{NamespaceLister: namespaceLister}
	matcher := &Matcher{namespaceMatcher: &namespace.Matcher{NamespaceProvider: namespaceProvider}, objectMatcher: &object.Matcher{}}

	for i := 0; i < b.N; i++ {
		matcher.Matches(attrs, interfaces, criteria)
	}
}

func BenchmarkShouldCallHookWithComplexRule(b *testing.B) {
	allScopes := v1.AllScopes
	equivalentMatch := v1.Equivalent

	namespace1Labels := map[string]string{"ns": "ns1"}
	namespace1 := corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "ns1",
			Labels: namespace1Labels,
		},
	}
	namespaceLister := fakeNamespaceLister{map[string]*corev1.Namespace{"ns": &namespace1}}

	mapper := runtime.NewEquivalentResourceRegistryWithIdentity(func(resource schema.GroupResource) string {
		if resource.Resource == "deployments" {
			// co-locate deployments in all API groups
			return "/deployments"
		}
		return ""
	})
	mapper.RegisterKindFor(gvr("extensions", "v1beta1", "deployments"), "", gvk("extensions", "v1beta1", "Deployment"))
	mapper.RegisterKindFor(gvr("apps", "v1", "deployments"), "", gvk("apps", "v1", "Deployment"))
	mapper.RegisterKindFor(gvr("apps", "v1beta1", "deployments"), "", gvk("apps", "v1beta1", "Deployment"))
	mapper.RegisterKindFor(gvr("apps", "v1alpha1", "deployments"), "", gvk("apps", "v1alpha1", "Deployment"))

	mapper.RegisterKindFor(gvr("extensions", "v1beta1", "deployments"), "scale", gvk("extensions", "v1beta1", "Scale"))
	mapper.RegisterKindFor(gvr("apps", "v1", "deployments"), "scale", gvk("autoscaling", "v1", "Scale"))
	mapper.RegisterKindFor(gvr("apps", "v1beta1", "deployments"), "scale", gvk("apps", "v1beta1", "Scale"))
	mapper.RegisterKindFor(gvr("apps", "v1alpha1", "deployments"), "scale", gvk("apps", "v1alpha1", "Scale"))

	mapper.RegisterKindFor(gvr("apps", "v1", "statefulset"), "", gvk("apps", "v1", "StatefulSet"))
	mapper.RegisterKindFor(gvr("apps", "v1beta1", "statefulset"), "", gvk("apps", "v1beta1", "StatefulSet"))
	mapper.RegisterKindFor(gvr("apps", "v1beta2", "statefulset"), "", gvk("apps", "v1beta2", "StatefulSet"))

	mapper.RegisterKindFor(gvr("apps", "v1", "statefulset"), "scale", gvk("apps", "v1", "Scale"))
	mapper.RegisterKindFor(gvr("apps", "v1beta1", "statefulset"), "scale", gvk("apps", "v1beta1", "Scale"))
	mapper.RegisterKindFor(gvr("apps", "v1alpha2", "statefulset"), "scale", gvk("apps", "v1beta2", "Scale"))

	mr := v1.MatchResources{
		MatchPolicy:       &equivalentMatch,
		NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"a": "b"}},
		ObjectSelector:    &metav1.LabelSelector{},
		ResourceRules:     []v1.NamedRuleWithOperations{},
	}

	for i := 0; i < 100; i++ {
		rule := v1.NamedRuleWithOperations{
			RuleWithOperations: v1.RuleWithOperations{
				Operations: []v1.OperationType{"*"},
				Rule: v1.Rule{
					APIGroups:   []string{fmt.Sprintf("app-%d", i)},
					APIVersions: []string{fmt.Sprintf("v%d", i)},
					Resources:   []string{fmt.Sprintf("resource%d", i), fmt.Sprintf("resource%d/scale", i)},
					Scope:       &allScopes,
				},
			},
		}
		mr.ResourceRules = append(mr.ResourceRules, rule)
	}

	criteria := &fakeCriteria{matchResources: mr}
	attrs := admission.NewAttributesRecord(nil, nil, gvk("autoscaling", "v1", "Scale"), "ns", "name", gvr("apps", "v1", "deployments"), "scale", admission.Create, &metav1.CreateOptions{}, false, nil)
	interfaces := &admission.RuntimeObjectInterfaces{EquivalentResourceMapper: mapper}
	namespaceProvider := &namespace.Provider{NamespaceLister: namespaceLister}
	matcher := &Matcher{namespaceMatcher: &namespace.Matcher{NamespaceProvider: namespaceProvider}, objectMatcher: &object.Matcher{}}

	for i := 0; i < b.N; i++ {
		matcher.Matches(attrs, interfaces, criteria)
	}
}

func BenchmarkShouldCallHookWithComplexSelectorAndRule(b *testing.B) {
	allScopes := v1.AllScopes
	equivalentMatch := v1.Equivalent

	namespace1Labels := map[string]string{"ns": "ns1"}
	namespace1 := corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "ns1",
			Labels: namespace1Labels,
		},
	}
	namespaceLister := fakeNamespaceLister{map[string]*corev1.Namespace{"ns": &namespace1}}

	mapper := runtime.NewEquivalentResourceRegistryWithIdentity(func(resource schema.GroupResource) string {
		if resource.Resource == "deployments" {
			// co-locate deployments in all API groups
			return "/deployments"
		}
		return ""
	})
	mapper.RegisterKindFor(gvr("extensions", "v1beta1", "deployments"), "", gvk("extensions", "v1beta1", "Deployment"))
	mapper.RegisterKindFor(gvr("apps", "v1", "deployments"), "", gvk("apps", "v1", "Deployment"))
	mapper.RegisterKindFor(gvr("apps", "v1beta1", "deployments"), "", gvk("apps", "v1beta1", "Deployment"))
	mapper.RegisterKindFor(gvr("apps", "v1alpha1", "deployments"), "", gvk("apps", "v1alpha1", "Deployment"))

	mapper.RegisterKindFor(gvr("extensions", "v1beta1", "deployments"), "scale", gvk("extensions", "v1beta1", "Scale"))
	mapper.RegisterKindFor(gvr("apps", "v1", "deployments"), "scale", gvk("autoscaling", "v1", "Scale"))
	mapper.RegisterKindFor(gvr("apps", "v1beta1", "deployments"), "scale", gvk("apps", "v1beta1", "Scale"))
	mapper.RegisterKindFor(gvr("apps", "v1alpha1", "deployments"), "scale", gvk("apps", "v1alpha1", "Scale"))

	mapper.RegisterKindFor(gvr("apps", "v1", "statefulset"), "", gvk("apps", "v1", "StatefulSet"))
	mapper.RegisterKindFor(gvr("apps", "v1beta1", "statefulset"), "", gvk("apps", "v1beta1", "StatefulSet"))
	mapper.RegisterKindFor(gvr("apps", "v1beta2", "statefulset"), "", gvk("apps", "v1beta2", "StatefulSet"))

	mapper.RegisterKindFor(gvr("apps", "v1", "statefulset"), "scale", gvk("apps", "v1", "Scale"))
	mapper.RegisterKindFor(gvr("apps", "v1beta1", "statefulset"), "scale", gvk("apps", "v1beta1", "Scale"))
	mapper.RegisterKindFor(gvr("apps", "v1alpha2", "statefulset"), "scale", gvk("apps", "v1beta2", "Scale"))

	nsSelector := make(map[string]string)
	for i := 0; i < 100; i++ {
		nsSelector[fmt.Sprintf("key-%d", i)] = fmt.Sprintf("val-%d", i)
	}

	mr := v1.MatchResources{
		MatchPolicy:       &equivalentMatch,
		NamespaceSelector: &metav1.LabelSelector{MatchLabels: nsSelector},
		ObjectSelector:    &metav1.LabelSelector{},
		ResourceRules:     []v1.NamedRuleWithOperations{},
	}

	for i := 0; i < 100; i++ {
		rule := v1.NamedRuleWithOperations{
			RuleWithOperations: v1.RuleWithOperations{
				Operations: []v1.OperationType{"*"},
				Rule: v1.Rule{
					APIGroups:   []string{fmt.Sprintf("app-%d", i)},
					APIVersions: []string{fmt.Sprintf("v%d", i)},
					Resources:   []string{fmt.Sprintf("resource%d", i), fmt.Sprintf("resource%d/scale", i)},
					Scope:       &allScopes,
				},
			},
		}
		mr.ResourceRules = append(mr.ResourceRules, rule)
	}

	criteria := &fakeCriteria{matchResources: mr}
	attrs := admission.NewAttributesRecord(nil, nil, gvk("autoscaling", "v1", "Scale"), "ns", "name", gvr("apps", "v1", "deployments"), "scale", admission.Create, &metav1.CreateOptions{}, false, nil)
	interfaces := &admission.RuntimeObjectInterfaces{EquivalentResourceMapper: mapper}
	namespaceProvider := &namespace.Provider{NamespaceLister: namespaceLister}
	matcher := &Matcher{namespaceMatcher: &namespace.Matcher{NamespaceProvider: namespaceProvider}, objectMatcher: &object.Matcher{}}

	for i := 0; i < b.N; i++ {
		matcher.Matches(attrs, interfaces, criteria)
	}
}
