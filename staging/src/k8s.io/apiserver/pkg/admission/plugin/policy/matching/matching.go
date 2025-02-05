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

	v1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/admission"

	"k8s.io/apiserver/pkg/admission/plugin/webhook/predicates/namespace"
	"k8s.io/apiserver/pkg/admission/plugin/webhook/predicates/object"
	"k8s.io/apiserver/pkg/admission/plugin/webhook/predicates/rules"
)

type MatchCriteria interface {
	namespace.NamespaceSelectorProvider
	object.ObjectSelectorProvider

	GetMatchResources() v1.MatchResources
}

// Matcher decides if a request matches against matchCriteria
type Matcher struct {
	namespaceMatcher *namespace.Matcher
	objectMatcher    *object.Matcher
}

func (m *Matcher) GetNamespace(name string) (*corev1.Namespace, error) {
	return m.namespaceMatcher.GetNamespace(name)
}

// NewMatcher initialize the matcher with dependencies requires
func NewMatcher(
	namespaceProvider namespace.NamespaceProvider,
) *Matcher {
	return &Matcher{
		namespaceMatcher: &namespace.Matcher{
			NamespaceProvider: namespaceProvider,
		},
		objectMatcher: &object.Matcher{},
	}
}

// ValidateInitialization verify if the matcher is ready before use
func (m *Matcher) ValidateInitialization() error {
	if err := m.namespaceMatcher.Validate(); err != nil {
		return fmt.Errorf("namespaceMatcher is not properly setup: %v", err)
	}
	return nil
}

func (m *Matcher) Matches(attr admission.Attributes, o admission.ObjectInterfaces, criteria MatchCriteria) (bool, schema.GroupVersionResource, schema.GroupVersionKind, error) {
	matches, matchNsErr := m.namespaceMatcher.MatchNamespaceSelector(criteria, attr)
	// Should not return an error here for policy which do not apply to the request, even if err is an unexpected scenario.
	if !matches && matchNsErr == nil {
		return false, schema.GroupVersionResource{}, schema.GroupVersionKind{}, nil
	}

	matches, matchObjErr := m.objectMatcher.MatchObjectSelector(criteria, attr)
	// Should not return an error here for policy which do not apply to the request, even if err is an unexpected scenario.
	if !matches && matchObjErr == nil {
		return false, schema.GroupVersionResource{}, schema.GroupVersionKind{}, nil
	}

	matchResources := criteria.GetMatchResources()
	matchPolicy := matchResources.MatchPolicy
	if isExcluded, _, _, err := matchesResourceRules(matchResources.ExcludeResourceRules, matchPolicy, attr, o); isExcluded || err != nil {
		return false, schema.GroupVersionResource{}, schema.GroupVersionKind{}, err
	}

	var (
		isMatch       bool
		matchResource schema.GroupVersionResource
		matchKind     schema.GroupVersionKind
		matchErr      error
	)
	if len(matchResources.ResourceRules) == 0 {
		isMatch = true
		matchKind = attr.GetKind()
		matchResource = attr.GetResource()
	} else {
		isMatch, matchResource, matchKind, matchErr = matchesResourceRules(matchResources.ResourceRules, matchPolicy, attr, o)
	}
	if matchErr != nil {
		return false, schema.GroupVersionResource{}, schema.GroupVersionKind{}, matchErr
	}
	if !isMatch {
		return false, schema.GroupVersionResource{}, schema.GroupVersionKind{}, nil
	}

	// now that we know this applies to this request otherwise, if there were selector errors, return them
	if matchNsErr != nil {
		return false, schema.GroupVersionResource{}, schema.GroupVersionKind{}, matchNsErr
	}
	if matchObjErr != nil {
		return false, schema.GroupVersionResource{}, schema.GroupVersionKind{}, matchObjErr
	}

	return true, matchResource, matchKind, nil
}

func matchesResourceRules(namedRules []v1.NamedRuleWithOperations, matchPolicy *v1.MatchPolicyType, attr admission.Attributes, o admission.ObjectInterfaces) (bool, schema.GroupVersionResource, schema.GroupVersionKind, error) {
	matchKind := attr.GetKind()
	matchResource := attr.GetResource()

	for _, namedRule := range namedRules {
		rule := v1.RuleWithOperations(namedRule.RuleWithOperations)
		ruleMatcher := rules.Matcher{
			Rule: rule,
			Attr: attr,
		}
		if !ruleMatcher.Matches() {
			continue
		}
		// an empty name list always matches
		if len(namedRule.ResourceNames) == 0 {
			return true, matchResource, matchKind, nil
		}
		// TODO: GetName() can return an empty string if the user is relying on
		// the API server to generate the name... figure out what to do for this edge case
		name := attr.GetName()
		for _, matchedName := range namedRule.ResourceNames {
			if name == matchedName {
				return true, matchResource, matchKind, nil
			}
		}
	}

	// if match policy is undefined or exact, don't perform fuzzy matching
	// note that defaulting to fuzzy matching is set by the API
	if matchPolicy == nil || *matchPolicy == v1.Exact {
		return false, schema.GroupVersionResource{}, schema.GroupVersionKind{}, nil
	}

	attrWithOverride := &attrWithResourceOverride{Attributes: attr}
	equivalents := o.GetEquivalentResourceMapper().EquivalentResourcesFor(attr.GetResource(), attr.GetSubresource())
	for _, namedRule := range namedRules {
		for _, equivalent := range equivalents {
			if equivalent == attr.GetResource() {
				// we have already checked the original resource
				continue
			}
			attrWithOverride.resource = equivalent
			rule := v1.RuleWithOperations(namedRule.RuleWithOperations)
			m := rules.Matcher{
				Rule: rule,
				Attr: attrWithOverride,
			}
			if !m.Matches() {
				continue
			}
			matchKind = o.GetEquivalentResourceMapper().KindFor(equivalent, attr.GetSubresource())
			if matchKind.Empty() {
				return false, schema.GroupVersionResource{}, schema.GroupVersionKind{}, fmt.Errorf("unable to convert to %v: unknown kind", equivalent)
			}
			// an empty name list always matches
			if len(namedRule.ResourceNames) == 0 {
				return true, equivalent, matchKind, nil
			}

			// TODO: GetName() can return an empty string if the user is relying on
			// the API server to generate the name... figure out what to do for this edge case
			name := attr.GetName()
			for _, matchedName := range namedRule.ResourceNames {
				if name == matchedName {
					return true, equivalent, matchKind, nil
				}
			}
		}
	}
	return false, schema.GroupVersionResource{}, schema.GroupVersionKind{}, nil
}

type attrWithResourceOverride struct {
	admission.Attributes
	resource schema.GroupVersionResource
}

func (a *attrWithResourceOverride) GetResource() schema.GroupVersionResource { return a.resource }
