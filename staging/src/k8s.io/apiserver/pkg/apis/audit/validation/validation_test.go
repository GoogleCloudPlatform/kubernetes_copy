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

package validation

import (
	"testing"

	"k8s.io/apiserver/pkg/apis/audit"
)

func TestValidatePolicy(t *testing.T) {
	validRules := []audit.PolicyRule{
		{ // Defaulting rule
			Level: audit.LevelMetadata,
		}, { // Matching non-humans
			Level:      audit.LevelNone,
			UserGroups: []string{"system:serviceaccounts", "system:nodes"},
		}, { // Specific request
			Level:      audit.LevelRequestResponse,
			Verbs:      []string{"get"},
			Resources:  []audit.GroupResources{{Group: "rbac.authorization.k8s.io", Resources: []string{"roles", "rolebindings"}}},
			Namespaces: []string{"kube-system"},
		}, { // Some non-resource URLs
			Level:      audit.LevelMetadata,
			UserGroups: []string{"developers"},
			NonResourceURLs: []string{
				"/logs*",
				"/healthz*",
				"/metrics",
				"*",
			},
		}, { // Omit RequestReceived stage
			Level: audit.LevelMetadata,
			OmitStages: []audit.Stage{
				audit.Stage("RequestReceived"),
			},
		},
	}
	successCases := []audit.Policy{}
	for _, rule := range validRules {
		successCases = append(successCases, audit.Policy{Rules: []audit.PolicyRule{rule}})
	}
	successCases = append(successCases, audit.Policy{})                         // Empty policy is valid.
	successCases = append(successCases, audit.Policy{OmitStages: []audit.Stage{ // Policy with omitStages
		audit.Stage("RequestReceived")}})
	successCases = append(successCases, audit.Policy{Rules: validRules}) // Multiple rules.

	for i, policy := range successCases {
		if errs := ValidatePolicy(&policy); len(errs) != 0 {
			t.Errorf("[%d] Expected policy %#v to be valid: %v", i, policy, errs)
		}
	}

	invalidRules := []audit.PolicyRule{
		{}, // Empty rule (missing Level)
		{ // Missing level
			Verbs:      []string{"get"},
			Resources:  []audit.GroupResources{{Resources: []string{"secrets"}}},
			Namespaces: []string{"kube-system"},
		}, { // Invalid Level
			Level: "FooBar",
		}, { // NonResourceURLs + Namespaces
			Level:           audit.LevelMetadata,
			Namespaces:      []string{"default"},
			NonResourceURLs: []string{"/logs*"},
		}, { // NonResourceURLs + ResourceKinds
			Level:           audit.LevelMetadata,
			Resources:       []audit.GroupResources{{Resources: []string{"secrets"}}},
			NonResourceURLs: []string{"/logs*"},
		}, { // invalid group name
			Level:     audit.LevelMetadata,
			Resources: []audit.GroupResources{{Group: "rbac.authorization.k8s.io/v1beta1", Resources: []string{"roles"}}},
		}, { // invalid non-resource URLs
			Level: audit.LevelMetadata,
			NonResourceURLs: []string{
				"logs",
				"/healthz*",
			},
		}, { // empty non-resource URLs
			Level: audit.LevelMetadata,
			NonResourceURLs: []string{
				"",
				"/healthz*",
			},
		}, { // invalid non-resource URLs with multi "*"
			Level: audit.LevelMetadata,
			NonResourceURLs: []string{
				"/logs/*/*",
				"/metrics",
			},
		}, { // invalid non-resource URLs with "*" not in the end
			Level: audit.LevelMetadata,
			NonResourceURLs: []string{
				"/logs/*.log",
				"/metrics",
			},
		},
		{ // ResourceNames without Resources
			Level:      audit.LevelMetadata,
			Verbs:      []string{"get"},
			Resources:  []audit.GroupResources{{ResourceNames: []string{"leader"}}},
			Namespaces: []string{"kube-system"},
		},
		{ // invalid omitStages in rule
			Level: audit.LevelMetadata,
			OmitStages: []audit.Stage{
				audit.Stage("foo"),
			},
		},
	}
	errorCases := []audit.Policy{}
	for _, rule := range invalidRules {
		errorCases = append(errorCases, audit.Policy{Rules: []audit.PolicyRule{rule}})
	}

	// Multiple rules.
	errorCases = append(errorCases, audit.Policy{Rules: append(validRules, audit.PolicyRule{})})

	// invalid omitStages in policy
	policy := audit.Policy{OmitStages: []audit.Stage{
		audit.Stage("foo"),
	},
		Rules: []audit.PolicyRule{{
			Level: audit.LevelMetadata,
		}},
	}
	errorCases = append(errorCases, policy)

	for i, policy := range errorCases {
		if errs := ValidatePolicy(&policy); len(errs) == 0 {
			t.Errorf("[%d] Expected policy %#v to be invalid!", i, policy)
		}
	}
}
