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

package policy

import (
	"os"
	"reflect"
	"strings"
	"testing"

	"k8s.io/apiserver/pkg/apis/audit"

	// import to call webhook's init() function to register audit.Policy to schema
	_ "k8s.io/apiserver/plugin/pkg/audit/webhook"

	"github.com/google/go-cmp/cmp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const policyDefPattern = `
apiVersion: audit.k8s.io/{version}
kind: Policy
rules:
  - level: None
    nonResourceURLs:
      - /healthz*
      - /version
  - level: RequestResponse
    users: ["tim"]
    userGroups: ["testers", "developers"]
    verbs: ["patch", "delete", "create"]
    resources:
      - group: ""
      - group: "rbac.authorization.k8s.io"
        resources: ["clusterroles", "clusterrolebindings"]
    namespaces: ["default", "kube-system"]
  - level: Metadata
`

const policyWithNoVersionOrKind = `
rules:
  - level: None
    nonResourceURLs:
      - /healthz*
      - /version
  - level: RequestResponse
    users: ["tim"]
    userGroups: ["testers", "developers"]
    verbs: ["patch", "delete", "create"]
    resources:
      - group: ""
      - group: "rbac.authorization.k8s.io"
        resources: ["clusterroles", "clusterrolebindings"]
    namespaces: ["default", "kube-system"]
  - level: Metadata
`

const policyWithUnknownField = `
apiVersion: audit.k8s.io/v1
kind: Policy
rules:
- level: None
  resources:
  - group: coordination.k8s.io
    resources:
    - "leases"
    verbs: ["watch", "get", "list"] # invalid indentation on verbs
`

var expectedPolicy = &audit.Policy{
	Rules: []audit.PolicyRule{{
		Level:           audit.LevelNone,
		NonResourceURLs: []string{"/healthz*", "/version"},
	}, {
		Level:      audit.LevelRequestResponse,
		Users:      []string{"tim"},
		UserGroups: []string{"testers", "developers"},
		Verbs:      []string{"patch", "delete", "create"},
		Resources: []audit.GroupResources{{}, {
			Group:     "rbac.authorization.k8s.io",
			Resources: []string{"clusterroles", "clusterrolebindings"},
		}},
		Namespaces: []string{"default", "kube-system"},
	}, {
		Level: audit.LevelMetadata,
	}},
}

func TestParser(t *testing.T) {
	policyDef := strings.Replace(policyDefPattern, "{version}", "v1", 1)
	f, err := writePolicy(t, policyDef)
	require.NoError(t, err)
	defer os.Remove(f)

	policy, err := LoadPolicyFromFile(f)
	require.NoError(t, err)

	assert.Len(t, policy.Rules, 3) // Sanity check.
	if !reflect.DeepEqual(policy, expectedPolicy) {
		t.Errorf("Unexpected policy! Diff:\n%s", cmp.Diff(policy, expectedPolicy))
	}
}

func TestParsePolicyWithNoVersionOrKind(t *testing.T) {
	f, err := writePolicy(t, policyWithNoVersionOrKind)
	require.NoError(t, err)
	defer os.Remove(f)

	_, err = LoadPolicyFromFile(f)
	assert.ErrorContains(t, err, "unknown group version field")
}

func TestParsePolicyWithUnknownField(t *testing.T) {
	f, err := writePolicy(t, policyWithUnknownField)
	require.NoError(t, err)
	defer os.Remove(f)

	_, err = LoadPolicyFromFile(f)
	require.NoError(t, err)
}

func TestPolicyCntCheck(t *testing.T) {
	var testCases = []struct {
		caseName, policy string
	}{
		{
			"policyWithNoRule",
			`apiVersion: audit.k8s.io/v1
kind: Policy`,
		},
		{"emptyPolicyFile", ""},
	}

	for _, tc := range testCases {
		f, err := writePolicy(t, tc.policy)
		require.NoError(t, err)
		defer os.Remove(f)

		_, err = LoadPolicyFromFile(f)
		assert.Errorf(t, err, "loaded illegal policy with 0 rules from testCase %s", tc.caseName)
	}
}

func writePolicy(t *testing.T, policy string) (string, error) {
	f, err := os.CreateTemp("", "policy.yaml")
	require.NoError(t, err)

	_, err = f.WriteString(policy)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	return f.Name(), nil
}
