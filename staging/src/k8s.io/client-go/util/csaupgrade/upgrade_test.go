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

package csaupgrade_test

import (
	"encoding/json"
	"reflect"
	"testing"

	jsonpatch "github.com/evanphx/json-patch/v5"
	"github.com/google/go-cmp/cmp"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/util/csaupgrade"
	"sigs.k8s.io/structured-merge-diff/v4/fieldpath"
)

func TestFindOwners(t *testing.T) {
	testCases := []struct {
		Name              string
		ManagedFieldsYAML string
		Operation         metav1.ManagedFieldsOperationType
		Fields            *fieldpath.Set
		Expectation       []string
	}{
		{
			// Field a root field path owner
			Name: "Basic",
			ManagedFieldsYAML: `
      managedFields:
      - apiVersion: v1
        fieldsType: FieldsV1
        fieldsV1:
          f:data:
            .: {}
            f:key: {}
            f:legacy: {}
          f:metadata:
            f:annotations:
              .: {}
              f:kubectl.kubernetes.io/last-applied-configuration: {}
        manager: kubectl-client-side-apply
        operation: Update
        time: "2022-08-22T23:08:23Z"
      `,
			Operation:   metav1.ManagedFieldsOperationUpdate,
			Fields:      fieldpath.NewSet(fieldpath.MakePathOrDie("data")),
			Expectation: []string{"kubectl-client-side-apply"},
		},
		{
			// Find a fieldpath nested inside another field
			Name: "Nested",
			ManagedFieldsYAML: `
      managedFields:
      - apiVersion: v1
        fieldsType: FieldsV1
        fieldsV1:
          f:data:
            .: {}
            f:key: {}
            f:legacy: {}
          f:metadata:
            f:annotations:
              .: {}
              f:kubectl.kubernetes.io/last-applied-configuration: {}
        manager: kubectl-client-side-apply
        operation: Update
        time: "2022-08-22T23:08:23Z"
      `,
			Operation:   metav1.ManagedFieldsOperationUpdate,
			Fields:      fieldpath.NewSet(fieldpath.MakePathOrDie("metadata", "annotations", "kubectl.kubernetes.io/last-applied-configuration")),
			Expectation: []string{"kubectl-client-side-apply"},
		},
		{
			// Search for an operaiton/fieldpath combination that is not found on both
			// axes
			Name: "NotFound",
			ManagedFieldsYAML: `
      managedFields:
      - apiVersion: v1
        fieldsType: FieldsV1
        fieldsV1:
          f:data:
            .: {}
            f:key: {}
            f:legacy: {}
          f:metadata:
            f:annotations:
              .: {}
              f:kubectl.kubernetes.io/last-applied-configuration: {}
        manager: kubectl
        operation: Apply
        time: "2022-08-23T23:08:23Z"
      `,
			Operation:   metav1.ManagedFieldsOperationUpdate,
			Fields:      fieldpath.NewSet(fieldpath.MakePathOrDie("metadata", "annotations", "kubectl.kubernetes.io/last-applied-configuration")),
			Expectation: []string{},
		},
		{
			// Test using apply operation
			Name: "ApplyOperation",
			ManagedFieldsYAML: `
      managedFields:
      - apiVersion: v1
        fieldsType: FieldsV1
        fieldsV1:
          f:data:
            .: {}
            f:key: {}
            f:legacy: {}
          f:metadata:
            f:annotations:
              .: {}
              f:kubectl.kubernetes.io/last-applied-configuration: {}
        manager: kubectl
        operation: Apply
        time: "2022-08-23T23:08:23Z"
      `,
			Operation:   metav1.ManagedFieldsOperationApply,
			Fields:      fieldpath.NewSet(fieldpath.MakePathOrDie("metadata", "annotations", "kubectl.kubernetes.io/last-applied-configuration")),
			Expectation: []string{"kubectl"},
		},
		{
			// Of multiple field managers, match a single one
			Name: "OneOfMultiple",
			ManagedFieldsYAML: `
      managedFields:
      - apiVersion: v1
        fieldsType: FieldsV1
        fieldsV1:
          f:metadata:
            f:annotations:
              .: {}
              f:kubectl.kubernetes.io/last-applied-configuration: {}
        manager: kubectl-client-side-apply
        operation: Update
        time: "2022-08-23T23:08:23Z"
      - apiVersion: v1
        fieldsType: FieldsV1
        fieldsV1:
          f:data:
            .: {}
            f:key: {}
            f:legacy: {}
        manager: kubectl
        operation: Apply
        time: "2022-08-23T23:08:23Z"
      `,
			Operation:   metav1.ManagedFieldsOperationUpdate,
			Fields:      fieldpath.NewSet(fieldpath.MakePathOrDie("metadata", "annotations", "kubectl.kubernetes.io/last-applied-configuration")),
			Expectation: []string{"kubectl-client-side-apply"},
		},
		{
			// have multiple field managers, and match more than one but not all of them
			Name: "ManyOfMultiple",
			ManagedFieldsYAML: `
      managedFields:
      - apiVersion: v1
        fieldsType: FieldsV1
        fieldsV1:
          f:metadata:
            f:annotations:
              .: {}
              f:kubectl.kubernetes.io/last-applied-configuration: {}
        manager: kubectl-client-side-apply
        operation: Update
        time: "2022-08-23T23:08:23Z"
      - apiVersion: v1
        fieldsType: FieldsV1
        fieldsV1:
          f:metadata:
            f:annotations:
              .: {}
              f:kubectl.kubernetes.io/last-applied-configuration: {}
          f:data:
            .: {}
            f:key: {}
            f:legacy: {}
        manager: kubectl
        operation: Apply
        time: "2022-08-23T23:08:23Z"
      - apiVersion: v1
        fieldsType: FieldsV1
        fieldsV1:
          f:metadata:
            f:annotations:
              .: {}
              f:kubectl.kubernetes.io/last-applied-configuration: {}
        manager: kubectl-client-side-apply2
        operation: Update
        time: "2022-08-23T23:08:23Z"
      `,
			Operation:   metav1.ManagedFieldsOperationUpdate,
			Fields:      fieldpath.NewSet(fieldpath.MakePathOrDie("metadata", "annotations", "kubectl.kubernetes.io/last-applied-configuration")),
			Expectation: []string{"kubectl-client-side-apply", "kubectl-client-side-apply2"},
		},
		{
			// Test with multiple fields to match against
			Name: "BasicMultipleFields",
			ManagedFieldsYAML: `
      managedFields:
      - apiVersion: v1
        fieldsType: FieldsV1
        fieldsV1:
          f:metadata:
            f:annotations:
              .: {}
              f:kubectl.kubernetes.io/last-applied-configuration: {}
          f:data:
            .: {}
            f:key: {}
            f:legacy: {}
        manager: kubectl-client-side-apply
        operation: Update
        time: "2022-08-23T23:08:23Z"
      `,
			Operation: metav1.ManagedFieldsOperationUpdate,
			Fields: fieldpath.NewSet(
				fieldpath.MakePathOrDie("data", "key"),
				fieldpath.MakePathOrDie("metadata", "annotations", "kubectl.kubernetes.io/last-applied-configuration"),
			),
			Expectation: []string{"kubectl-client-side-apply"},
		},
		{
			// Test with multiplle fields but the manager is missing one of the fields
			// requested so it does not match
			Name: "MissingOneField",
			ManagedFieldsYAML: `
      managedFields:
      - apiVersion: v1
        fieldsType: FieldsV1
        fieldsV1:
          f:metadata:
            f:annotations:
              .: {}
              f:kubectl.kubernetes.io/last-applied-configuration: {}
          f:data:
            .: {}
            f:legacy: {}
        manager: kubectl-client-side-apply
        operation: Update
        time: "2022-08-23T23:08:23Z"
      `,
			Operation: metav1.ManagedFieldsOperationUpdate,
			Fields: fieldpath.NewSet(
				fieldpath.MakePathOrDie("data", "key"),
				fieldpath.MakePathOrDie("metadata", "annotations", "kubectl.kubernetes.io/last-applied-configuration"),
			),
			Expectation: []string{},
		},
	}
	for _, tcase := range testCases {
		t.Run(tcase.Name, func(t *testing.T) {
			var entries struct {
				ManagedFields []metav1.ManagedFieldsEntry `json:"managedFields"`
			}
			err := yaml.Unmarshal([]byte(tcase.ManagedFieldsYAML), &entries)
			require.NoError(t, err)

			result := csaupgrade.FindFieldsOwners(entries.ManagedFields, tcase.Operation, tcase.Fields)

			// Compare owner names since they uniquely identify the selected entries
			// (given that the operation is provided)
			ownerNames := []string{}
			for _, entry := range result {
				ownerNames = append(ownerNames, entry.Manager)
				require.Equal(t, tcase.Operation, entry.Operation)
			}
			require.ElementsMatch(t, tcase.Expectation, ownerNames)
		})
	}
}

func TestUpgradeCSA(t *testing.T) {

	cases := []struct {
		Name           string
		CSAManagers    []string
		SSAManager     string
		Options        []csaupgrade.Option
		OriginalObject []byte
		ExpectedObject []byte
	}{
		{
			// Case where there is a CSA entry with the given name, but no SSA entry
			// is found. Expect that the CSA entry is converted to an SSA entry
			// and renamed.
			Name:        "csa-basic-direct-conversion",
			CSAManagers: []string{"kubectl-client-side-apply"},
			SSAManager:  "kubectl",
			OriginalObject: []byte(`
apiVersion: v1
data: {}
kind: ConfigMap
metadata:
  resourceVersion: "1"
  annotations:
    kubectl.kubernetes.io/last-applied-configuration: |
      {"apiVersion":"v1","data":{"key":"value","legacy":"unused"},"kind":"ConfigMap","metadata":{"annotations":{},"name":"test","namespace":"default"}}
  creationTimestamp: "2022-08-22T23:08:23Z"
  managedFields:
  - apiVersion: v1
    fieldsType: FieldsV1
    fieldsV1:
      f:data:
        .: {}
        f:key: {}
        f:legacy: {}
      f:metadata:
        f:annotations:
          .: {}
          f:kubectl.kubernetes.io/last-applied-configuration: {}
    manager: kubectl-client-side-apply
    operation: Update
    time: "2022-08-22T23:08:23Z"
  name: test
  namespace: default
`),
			ExpectedObject: []byte(`
apiVersion: v1
data: {}
kind: ConfigMap
metadata:
  resourceVersion: "1"
  annotations:
    kubectl.kubernetes.io/last-applied-configuration: |
      {"apiVersion":"v1","data":{"key":"value","legacy":"unused"},"kind":"ConfigMap","metadata":{"annotations":{},"name":"test","namespace":"default"}}
  creationTimestamp: "2022-08-22T23:08:23Z"
  managedFields:
  - apiVersion: v1
    fieldsType: FieldsV1
    fieldsV1:
      f:data:
        .: {}
        f:key: {}
        f:legacy: {}
      f:metadata:
        f:annotations:
          .: {}
          f:kubectl.kubernetes.io/last-applied-configuration: {}
    manager: kubectl
    operation: Apply
    time: "2022-08-22T23:08:23Z"
  name: test
  namespace: default
`),
		},
		{
			// This is the case when kubectl --server-side is used for the first time
			// Server creates duplicate managed fields entry - one for Update and another
			// for Apply. Expect entries to be merged into one entry, which is unchanged
			// from initial SSA.
			Name:        "csa-combine-with-ssa-duplicate-keys",
			CSAManagers: []string{"kubectl-client-side-apply"},
			SSAManager:  "kubectl",
			OriginalObject: []byte(`
apiVersion: v1
data: {}
kind: ConfigMap
metadata:
  resourceVersion: "1"
  annotations:
    kubectl.kubernetes.io/last-applied-configuration: |
      {"apiVersion":"v1","data":{"key":"value","legacy":"unused"},"kind":"ConfigMap","metadata":{"annotations":{},"name":"test","namespace":"default"}}
  creationTimestamp: "2022-08-22T23:08:23Z"
  managedFields:
  - apiVersion: v1
    fieldsType: FieldsV1
    fieldsV1:
      f:data:
        .: {}
        f:key: {}
        f:legacy: {}
      f:metadata:
        f:annotations:
          .: {}
          f:kubectl.kubernetes.io/last-applied-configuration: {}
    manager: kubectl
    operation: Apply
    time: "2022-08-23T23:08:23Z"
  - apiVersion: v1
    fieldsType: FieldsV1
    fieldsV1:
      f:data:
        .: {}
        f:key: {}
        f:legacy: {}
      f:metadata:
        f:annotations:
          .: {}
          f:kubectl.kubernetes.io/last-applied-configuration: {}
    manager: kubectl-client-side-apply
    operation: Update
    time: "2022-08-22T23:08:23Z"
  name: test
  namespace: default
`),
			ExpectedObject: []byte(`
apiVersion: v1
data: {}
kind: ConfigMap
metadata:
  resourceVersion: "1"
  annotations:
    kubectl.kubernetes.io/last-applied-configuration: |
      {"apiVersion":"v1","data":{"key":"value","legacy":"unused"},"kind":"ConfigMap","metadata":{"annotations":{},"name":"test","namespace":"default"}}
  creationTimestamp: "2022-08-22T23:08:23Z"
  managedFields:
  - apiVersion: v1
    fieldsType: FieldsV1
    fieldsV1:
      f:data:
        .: {}
        f:key: {}
        f:legacy: {}
      f:metadata:
        f:annotations:
          .: {}
          f:kubectl.kubernetes.io/last-applied-configuration: {}
    manager: kubectl
    operation: Apply
    time: "2022-08-23T23:08:23Z"
  name: test
  namespace: default
`),
		},
		{
			// This is the case when kubectl --server-side is used for the first time,
			// but then a key is removed. A bug would take place where key is left in
			// CSA entry but no longer present in SSA entry, so it would not be pruned.
			// This shows that upgrading such an object results in correct behavior next
			// time SSA applier
			// Expect final object to have unioned keys from both entries
			Name:        "csa-combine-with-ssa-additional-keys",
			CSAManagers: []string{"kubectl-client-side-apply"},
			SSAManager:  "kubectl",
			OriginalObject: []byte(`
apiVersion: v1
data: {}
kind: ConfigMap
metadata:
  resourceVersion: "1"
  annotations:
    kubectl.kubernetes.io/last-applied-configuration: |
      {"apiVersion":"v1","data":{"key":"value","legacy":"unused"},"kind":"ConfigMap","metadata":{"annotations":{},"name":"test","namespace":"default"}}
  creationTimestamp: "2022-08-22T23:08:23Z"
  managedFields:
  - apiVersion: v1
    fieldsType: FieldsV1
    fieldsV1:
      f:data:
        .: {}
        f:key: {}
      f:metadata:
        f:annotations:
          .: {}
          f:kubectl.kubernetes.io/last-applied-configuration: {}
    manager: kubectl
    operation: Apply
    time: "2022-08-23T23:08:23Z"
  - apiVersion: v1
    fieldsType: FieldsV1
    fieldsV1:
      f:data:
        .: {}
        f:key: {}
        f:legacy: {}
      f:metadata:
        f:annotations:
          .: {}
          f:kubectl.kubernetes.io/last-applied-configuration: {}
    manager: kubectl-client-side-apply
    operation: Update
    time: "2022-08-22T23:08:23Z"
  name: test
  namespace: default
`),
			ExpectedObject: []byte(`
apiVersion: v1
data: {}
kind: ConfigMap
metadata:
  resourceVersion: "1"
  annotations:
    kubectl.kubernetes.io/last-applied-configuration: |
      {"apiVersion":"v1","data":{"key":"value","legacy":"unused"},"kind":"ConfigMap","metadata":{"annotations":{},"name":"test","namespace":"default"}}
  creationTimestamp: "2022-08-22T23:08:23Z"
  managedFields:
  - apiVersion: v1
    fieldsType: FieldsV1
    fieldsV1:
      f:data:
        .: {}
        f:key: {}
        f:legacy: {}
      f:metadata:
        f:annotations:
          .: {}
          f:kubectl.kubernetes.io/last-applied-configuration: {}
    manager: kubectl
    operation: Apply
    time: "2022-08-23T23:08:23Z"
  name: test
  namespace: default
`),
		},
		{
			// Case when there are multiple CSA versions on the object which do not
			// match the version from the apply entry. Shows they are tossed away
			// without being merged.
			Name:        "csa-no-applicable-version",
			CSAManagers: []string{"kubectl-client-side-apply"},
			SSAManager:  "kubectl",
			OriginalObject: []byte(`
apiVersion: v1
data: {}
kind: ConfigMap
metadata:
  resourceVersion: "1"
  annotations:
    kubectl.kubernetes.io/last-applied-configuration: |
      {"apiVersion":"v1","data":{"key":"value","legacy":"unused"},"kind":"ConfigMap","metadata":{"annotations":{},"name":"test","namespace":"default"}}
  creationTimestamp: "2022-08-22T23:08:23Z"
  managedFields:
  - apiVersion: v5
    fieldsType: FieldsV1
    fieldsV1:
      f:data:
        .: {}
        f:key: {}
        f:legacy: {}
      f:metadata:
        f:annotations:
          .: {}
          f:kubectl.kubernetes.io/last-applied-configuration: {}
    manager: kubectl
    operation: Apply
    time: "2022-08-23T23:08:23Z"
  - apiVersion: v1
    fieldsType: FieldsV1
    fieldsV1:
      f:data:
        f:key2: {}
      f:metadata:
        f:annotations:
          f:hello2: {}
    manager: kubectl-client-side-apply
    operation: Update
    time: "2022-08-22T23:08:23Z"
  - apiVersion: v2
    fieldsType: FieldsV1
    fieldsV1:
      f:data:
        f:key3: {}
      f:metadata:
        f:annotations:
          f:hello3: {}
    manager: kubectl-client-side-apply
    operation: Update
    time: "2022-08-22T23:08:23Z"
  - apiVersion: v3
    fieldsType: FieldsV1
    fieldsV1:
      f:data:
        f:key4: {}
      f:metadata:
        f:annotations:
          f:hello3: {}
    manager: kubectl-client-side-apply
    operation: Update
    time: "2022-08-22T23:08:23Z"
  - apiVersion: v4
    fieldsType: FieldsV1
    fieldsV1:
      f:data:
        f:key5: {}
      f:metadata:
        f:annotations:
          f:hello4: {}
    manager: kubectl-client-side-apply
    operation: Update
    time: "2022-08-22T23:08:23Z"
  name: test
  namespace: default
`),
			ExpectedObject: []byte(`
apiVersion: v1
data: {}
kind: ConfigMap
metadata:
  resourceVersion: "1"
  annotations:
    kubectl.kubernetes.io/last-applied-configuration: |
      {"apiVersion":"v1","data":{"key":"value","legacy":"unused"},"kind":"ConfigMap","metadata":{"annotations":{},"name":"test","namespace":"default"}}
  creationTimestamp: "2022-08-22T23:08:23Z"
  managedFields:
  - apiVersion: v5
    fieldsType: FieldsV1
    fieldsV1:
      f:data:
        .: {}
        f:key: {}
        f:legacy: {}
      f:metadata:
        f:annotations:
          .: {}
          f:kubectl.kubernetes.io/last-applied-configuration: {}
    manager: kubectl
    operation: Apply
    time: "2022-08-23T23:08:23Z"
  name: test
  namespace: default
`),
		},
		{
			// Case when there are multiple CSA versions on the object which do not
			// match the version from the apply entry, and one which does.
			// Shows that CSA entry with matching version is unioned into the SSA entry.
			Name:        "csa-single-applicable-version",
			CSAManagers: []string{"kubectl-client-side-apply"},
			SSAManager:  "kubectl",
			OriginalObject: []byte(`
apiVersion: v1
data: {}
kind: ConfigMap
metadata:
  resourceVersion: "1"
  annotations:
    kubectl.kubernetes.io/last-applied-configuration: |
      {"apiVersion":"v1","data":{"key":"value","legacy":"unused"},"kind":"ConfigMap","metadata":{"annotations":{},"name":"test","namespace":"default"}}
  creationTimestamp: "2022-08-22T23:08:23Z"
  managedFields:
  - apiVersion: v5
    fieldsType: FieldsV1
    fieldsV1:
      f:data:
        .: {}
        f:key: {}
        f:legacy: {}
      f:metadata:
        f:annotations:
          .: {}
          f:kubectl.kubernetes.io/last-applied-configuration: {}
    manager: kubectl
    operation: Apply
    time: "2022-08-23T23:08:23Z"
  - apiVersion: v5
    fieldsType: FieldsV1
    fieldsV1:
      f:data:
        f:key2: {}
      f:metadata:
        f:annotations:
          f:hello2: {}
    manager: kubectl-client-side-apply
    operation: Update
    time: "2022-08-22T23:08:23Z"
  - apiVersion: v2
    fieldsType: FieldsV1
    fieldsV1:
      f:data:
        f:key3: {}
      f:metadata:
        f:annotations:
          f:hello3: {}
    manager: kubectl-client-side-apply
    operation: Update
    time: "2022-08-22T23:08:23Z"
  - apiVersion: v3
    fieldsType: FieldsV1
    fieldsV1:
      f:data:
        f:key4: {}
      f:metadata:
        f:annotations:
          f:hello4: {}
    manager: kubectl-client-side-apply
    operation: Update
    time: "2022-08-22T23:08:23Z"
  - apiVersion: v4
    fieldsType: FieldsV1
    fieldsV1:
      f:data:
        f:key5: {}
      f:metadata:
        f:annotations:
          f:hello5: {}
    manager: kubectl-client-side-apply
    operation: Update
    time: "2022-08-22T23:08:23Z"
  name: test
  namespace: default
`),
			ExpectedObject: []byte(`
apiVersion: v1
data: {}
kind: ConfigMap
metadata:
  resourceVersion: "1"
  annotations:
    kubectl.kubernetes.io/last-applied-configuration: |
      {"apiVersion":"v1","data":{"key":"value","legacy":"unused"},"kind":"ConfigMap","metadata":{"annotations":{},"name":"test","namespace":"default"}}
  creationTimestamp: "2022-08-22T23:08:23Z"
  managedFields:
  - apiVersion: v5
    fieldsType: FieldsV1
    fieldsV1:
      f:data:
        .: {}
        f:key: {}
        f:key2: {}
        f:legacy: {}
      f:metadata:
        f:annotations:
          .: {}
          f:hello2: {}
          f:kubectl.kubernetes.io/last-applied-configuration: {}
    manager: kubectl
    operation: Apply
    time: "2022-08-23T23:08:23Z"
  name: test
  namespace: default
`),
		},
		{
			// Do nothing to object with nothing to migrate and no existing SSA manager
			Name:        "noop",
			CSAManagers: []string{"kubectl-client-side-apply"},
			SSAManager:  "not-already-in-object",
			OriginalObject: []byte(`
apiVersion: v1
data: {}
kind: ConfigMap
metadata:
  resourceVersion: "1"
  annotations:
    kubectl.kubernetes.io/last-applied-configuration: |
      {"apiVersion":"v1","data":{"key":"value","legacy":"unused"},"kind":"ConfigMap","metadata":{"annotations":{},"name":"test","namespace":"default"}}
  creationTimestamp: "2022-08-22T23:08:23Z"
  managedFields:
  - apiVersion: v5
    fieldsType: FieldsV1
    fieldsV1:
      f:data:
        .: {}
        f:key: {}
        f:legacy: {}
      f:metadata:
        f:annotations:
          .: {}
          f:kubectl.kubernetes.io/last-applied-configuration: {}
    manager: kubectl
    operation: Apply
    time: "2022-08-23T23:08:23Z"
  name: test
  namespace: default
`),
			ExpectedObject: []byte(`
apiVersion: v1
data: {}
kind: ConfigMap
metadata:
  resourceVersion: "1"
  annotations:
    kubectl.kubernetes.io/last-applied-configuration: |
      {"apiVersion":"v1","data":{"key":"value","legacy":"unused"},"kind":"ConfigMap","metadata":{"annotations":{},"name":"test","namespace":"default"}}
  creationTimestamp: "2022-08-22T23:08:23Z"
  managedFields:
  - apiVersion: v5
    fieldsType: FieldsV1
    fieldsV1:
      f:data:
        .: {}
        f:key: {}
        f:legacy: {}
      f:metadata:
        f:annotations:
          .: {}
          f:kubectl.kubernetes.io/last-applied-configuration: {}
    manager: kubectl
    operation: Apply
    time: "2022-08-23T23:08:23Z"
  name: test
  namespace: default
`),
		},
		{
			// Expect multiple targets to be merged into existing ssa manager
			Name:        "multipleTargetsExisting",
			CSAManagers: []string{"kube-scheduler", "kubectl-client-side-apply"},
			SSAManager:  "kubectl",
			OriginalObject: []byte(`
apiVersion: v1
data: {}
kind: ConfigMap
metadata:
  resourceVersion: "1"
  annotations:
    kubectl.kubernetes.io/last-applied-configuration: |
      {"apiVersion":"v1","data":{"key":"value","legacy":"unused"},"kind":"ConfigMap","metadata":{"annotations":{},"name":"test","namespace":"default"}}
  creationTimestamp: "2022-08-22T23:08:23Z"
  managedFields:
  - apiVersion: v1
    fieldsType: FieldsV1
    fieldsV1:
      f:metadata:
        f:labels:
          f:name: {}
      f:spec:
        f:containers:
          k:{"name":"kubernetes-pause"}:
            .: {}
            f:image: {}
            f:name: {}
    manager: kubectl
    operation: Apply
  - apiVersion: v1
    fieldsType: FieldsV1
    fieldsV1:
      f:status:
        f:conditions:
          .: {}
          k:{"type":"PodScheduled"}:
            .: {}
            f:lastProbeTime: {}
            f:lastTransitionTime: {}
            f:message: {}
            f:reason: {}
            f:status: {}
            f:type: {}
    manager: kube-scheduler
    operation: Update
    time: "2022-11-03T23:22:40Z"
  - apiVersion: v1
    fieldsType: FieldsV1
    fieldsV1:
      f:metadata:
        f:annotations:
          .: {}
          f:kubectl.kubernetes.io/last-applied-configuration: {}
        f:labels:
          .: {}
          f:name: {}
      f:spec:
        f:containers:
          k:{"name":"kubernetes-pause"}:
            .: {}
            f:image: {}
            f:imagePullPolicy: {}
            f:name: {}
            f:resources: {}
            f:terminationMessagePath: {}
            f:terminationMessagePolicy: {}
        f:dnsPolicy: {}
        f:enableServiceLinks: {}
        f:restartPolicy: {}
        f:schedulerName: {}
        f:securityContext: {}
        f:terminationGracePeriodSeconds: {}
    manager: kubectl-client-side-apply
    operation: Update
    time: "2022-11-03T23:22:40Z"
  name: test
  namespace: default
`),
			ExpectedObject: []byte(`
apiVersion: v1
data: {}
kind: ConfigMap
metadata:
  resourceVersion: "1"
  annotations:
    kubectl.kubernetes.io/last-applied-configuration: |
      {"apiVersion":"v1","data":{"key":"value","legacy":"unused"},"kind":"ConfigMap","metadata":{"annotations":{},"name":"test","namespace":"default"}}
  creationTimestamp: "2022-08-22T23:08:23Z"
  managedFields:
  - apiVersion: v1
    fieldsType: FieldsV1
    fieldsV1:
      f:status:
        f:conditions:
          .: {}
          k:{"type":"PodScheduled"}:
            .: {}
            f:lastProbeTime: {}
            f:lastTransitionTime: {}
            f:message: {}
            f:reason: {}
            f:status: {}
            f:type: {}
      f:metadata:
        f:annotations:
          .: {}
          f:kubectl.kubernetes.io/last-applied-configuration: {}
        f:labels:
          .: {}
          f:name: {}
      f:spec:
        f:containers:
          k:{"name":"kubernetes-pause"}:
            .: {}
            f:image: {}
            f:imagePullPolicy: {}
            f:name: {}
            f:resources: {}
            f:terminationMessagePath: {}
            f:terminationMessagePolicy: {}
        f:dnsPolicy: {}
        f:enableServiceLinks: {}
        f:restartPolicy: {}
        f:schedulerName: {}
        f:securityContext: {}
        f:terminationGracePeriodSeconds: {}
    manager: kubectl
    operation: Apply
  name: test
  namespace: default
`),
		},
		{
			// Expect multiple targets to be merged into a new ssa manager
			Name:        "multipleTargetsNewInsertion",
			CSAManagers: []string{"kubectl-client-side-apply", "kube-scheduler"},
			SSAManager:  "newly-inserted-manager",
			OriginalObject: []byte(`
apiVersion: v1
data: {}
kind: ConfigMap
metadata:
  resourceVersion: "1"
  annotations:
    kubectl.kubernetes.io/last-applied-configuration: |
      {"apiVersion":"v1","data":{"key":"value","legacy":"unused"},"kind":"ConfigMap","metadata":{"annotations":{},"name":"test","namespace":"default"}}
  creationTimestamp: "2022-08-22T23:08:23Z"
  managedFields:
  - apiVersion: v1
    fieldsType: FieldsV1
    fieldsV1:
      f:metadata:
        f:labels:
          f:name: {}
      f:spec:
        f:containers:
          k:{"name":"kubernetes-pause"}:
            .: {}
            f:image: {}
            f:name: {}
    manager: kubectl
    operation: Apply
  - apiVersion: v1
    fieldsType: FieldsV1
    fieldsV1:
      f:status:
        f:conditions:
          .: {}
          k:{"type":"PodScheduled"}:
            .: {}
            f:lastProbeTime: {}
            f:lastTransitionTime: {}
            f:message: {}
            f:reason: {}
            f:status: {}
            f:type: {}
    manager: kube-scheduler
    operation: Update
    time: "2022-11-03T23:22:40Z"
  - apiVersion: v1
    fieldsType: FieldsV1
    fieldsV1:
      f:metadata:
        f:annotations:
          .: {}
          f:kubectl.kubernetes.io/last-applied-configuration: {}
        f:labels:
          .: {}
          f:name: {}
      f:spec:
        f:containers:
          k:{"name":"kubernetes-pause"}:
            .: {}
            f:image: {}
            f:imagePullPolicy: {}
            f:name: {}
            f:resources: {}
            f:terminationMessagePath: {}
            f:terminationMessagePolicy: {}
        f:dnsPolicy: {}
        f:enableServiceLinks: {}
        f:restartPolicy: {}
        f:schedulerName: {}
        f:securityContext: {}
        f:terminationGracePeriodSeconds: {}
    manager: kubectl-client-side-apply
    operation: Update
    time: "2022-11-03T23:22:40Z"
  name: test
  namespace: default
`),
			ExpectedObject: []byte(`
  apiVersion: v1
  data: {}
  kind: ConfigMap
  metadata:
    resourceVersion: "1"
    annotations:
      kubectl.kubernetes.io/last-applied-configuration: |
        {"apiVersion":"v1","data":{"key":"value","legacy":"unused"},"kind":"ConfigMap","metadata":{"annotations":{},"name":"test","namespace":"default"}}
    creationTimestamp: "2022-08-22T23:08:23Z"
    managedFields:
    - apiVersion: v1
      fieldsType: FieldsV1
      fieldsV1:
        f:metadata:
          f:labels:
            f:name: {}
        f:spec:
          f:containers:
            k:{"name":"kubernetes-pause"}:
              .: {}
              f:image: {}
              f:name: {}
      manager: kubectl
      operation: Apply
    - apiVersion: v1
      fieldsType: FieldsV1
      fieldsV1:
        f:metadata:
          f:annotations:
            .: {}
            f:kubectl.kubernetes.io/last-applied-configuration: {}
          f:labels:
            .: {}
            f:name: {}
        f:spec:
          f:containers:
            k:{"name":"kubernetes-pause"}:
              .: {}
              f:image: {}
              f:imagePullPolicy: {}
              f:name: {}
              f:resources: {}
              f:terminationMessagePath: {}
              f:terminationMessagePolicy: {}
          f:dnsPolicy: {}
          f:enableServiceLinks: {}
          f:restartPolicy: {}
          f:schedulerName: {}
          f:securityContext: {}
          f:terminationGracePeriodSeconds: {}
        f:status:
          f:conditions:
            .: {}
            k:{"type":"PodScheduled"}:
              .: {}
              f:lastProbeTime: {}
              f:lastTransitionTime: {}
              f:message: {}
              f:reason: {}
              f:status: {}
              f:type: {}
      manager: newly-inserted-manager
      operation: Apply
      time: "2022-11-03T23:22:40Z"
    name: test
    namespace: default
`),
		},
		{
			// Expect multiple targets to be merged into a new ssa manager
			Name:        "subresource",
			CSAManagers: []string{"kube-controller-manager"},
			SSAManager:  "kube-controller-manager",
			Options:     []csaupgrade.Option{csaupgrade.Subresource("status")},
			OriginalObject: []byte(`
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  annotations:
    pv.kubernetes.io/bind-completed: "yes"
    pv.kubernetes.io/bound-by-controller: "yes"
    volume.beta.kubernetes.io/storage-provisioner: openshift-storage.cephfs.csi.ceph.com
    volume.kubernetes.io/storage-provisioner: openshift-storage.cephfs.csi.ceph.com
  creationTimestamp: "2024-02-24T15:24:31Z"
  finalizers:
  - kubernetes.io/pvc-protection
  managedFields:
  - apiVersion: v1
    fieldsType: FieldsV1
    fieldsV1:
      f:spec:
        f:accessModes: {}
        f:resources:
          f:requests:
            .: {}
            f:storage: {}
        f:storageClassName: {}
        f:volumeMode: {}
    manager: Mozilla
    operation: Update
    time: "2024-02-24T15:24:31Z"
  - apiVersion: v1
    fieldsType: FieldsV1
    fieldsV1:
      f:metadata:
        f:annotations:
          .: {}
          f:pv.kubernetes.io/bind-completed: {}
          f:pv.kubernetes.io/bound-by-controller: {}
          f:volume.beta.kubernetes.io/storage-provisioner: {}
          f:volume.kubernetes.io/storage-provisioner: {}
      f:spec:
        f:volumeName: {}
    manager: kube-controller-manager
    operation: Update
    time: "2024-02-24T15:24:32Z"
  - apiVersion: v1
    fieldsType: FieldsV1
    fieldsV1:
      f:status:
        f:accessModes: {}
        f:capacity:
          .: {}
          f:storage: {}
        f:phase: {}
    manager: kube-controller-manager
    operation: Update
    subresource: status
    time: "2024-02-24T15:24:32Z"
  name: test
  namespace: default
  resourceVersion: "948647140"
  uid: f0692a61-0ffe-4fd5-b00f-0b95f3654fb9
spec:
  accessModes:
  - ReadWriteOnce
  resources:
    requests:
      storage: 1Gi
  storageClassName: ocs-storagecluster-cephfs
  volumeMode: Filesystem
  volumeName: pvc-f0692a61-0ffe-4fd5-b00f-0b95f3654fb9
status:
  accessModes:
  - ReadWriteOnce
  capacity:
    storage: 1Gi
  phase: Bound
`),
			ExpectedObject: []byte(`
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  annotations:
    pv.kubernetes.io/bind-completed: "yes"
    pv.kubernetes.io/bound-by-controller: "yes"
    volume.beta.kubernetes.io/storage-provisioner: openshift-storage.cephfs.csi.ceph.com
    volume.kubernetes.io/storage-provisioner: openshift-storage.cephfs.csi.ceph.com
  creationTimestamp: "2024-02-24T15:24:31Z"
  finalizers:
  - kubernetes.io/pvc-protection
  managedFields:
  - apiVersion: v1
    fieldsType: FieldsV1
    fieldsV1:
      f:spec:
        f:accessModes: {}
        f:resources:
          f:requests:
            .: {}
            f:storage: {}
        f:storageClassName: {}
        f:volumeMode: {}
    manager: Mozilla
    operation: Update
    time: "2024-02-24T15:24:31Z"
  - apiVersion: v1
    fieldsType: FieldsV1
    fieldsV1:
      f:metadata:
        f:annotations:
          .: {}
          f:pv.kubernetes.io/bind-completed: {}
          f:pv.kubernetes.io/bound-by-controller: {}
          f:volume.beta.kubernetes.io/storage-provisioner: {}
          f:volume.kubernetes.io/storage-provisioner: {}
      f:spec:
        f:volumeName: {}
    manager: kube-controller-manager
    operation: Update
    time: "2024-02-24T15:24:32Z"
  - apiVersion: v1
    fieldsType: FieldsV1
    fieldsV1:
      f:status:
        f:accessModes: {}
        f:capacity:
          .: {}
          f:storage: {}
        f:phase: {}
    manager: kube-controller-manager
    operation: Apply
    subresource: status
    time: "2024-02-24T15:24:32Z"
  name: test
  namespace: default
  resourceVersion: "948647140"
  uid: f0692a61-0ffe-4fd5-b00f-0b95f3654fb9
spec:
  accessModes:
  - ReadWriteOnce
  resources:
    requests:
      storage: 1Gi
  storageClassName: ocs-storagecluster-cephfs
  volumeMode: Filesystem
  volumeName: pvc-f0692a61-0ffe-4fd5-b00f-0b95f3654fb9
status:
  accessModes:
  - ReadWriteOnce
  capacity:
    storage: 1Gi
  phase: Bound
`),
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.Name, func(t *testing.T) {
			initialObject := unstructured.Unstructured{}
			err := yaml.Unmarshal(testCase.OriginalObject, &initialObject.Object)
			if err != nil {
				t.Fatal(err)
			}

			upgraded := initialObject.DeepCopy()
			err = csaupgrade.UpgradeManagedFields(
				upgraded,
				sets.New(testCase.CSAManagers...),
				testCase.SSAManager,
				testCase.Options...,
			)

			if err != nil {
				t.Fatal(err)
			}

			expectedObject := unstructured.Unstructured{}
			err = yaml.Unmarshal(testCase.ExpectedObject, &expectedObject.Object)
			if err != nil {
				t.Fatal(err)
			}

			if !reflect.DeepEqual(&expectedObject, upgraded) {
				t.Fatal(cmp.Diff(&expectedObject, upgraded))
			}

			// Show that the UpgradeManagedFieldsPatch yields a patch that does
			// nothing more and nothing less than make the object equal to output
			// of UpgradeManagedFields

			initialCopy := initialObject.DeepCopyObject()
			patchBytes, err := csaupgrade.UpgradeManagedFieldsPatch(
				initialCopy, sets.New(testCase.CSAManagers...), testCase.SSAManager, testCase.Options...)

			if err != nil {
				t.Fatal(err)
			} else if patchBytes != nil {
				patch, err := jsonpatch.DecodePatch(patchBytes)
				if err != nil {
					t.Fatal(err)
				}

				initialJSON, err := json.Marshal(initialObject.Object)
				if err != nil {
					t.Fatal(err)
				}

				patchedBytes, err := patch.Apply(initialJSON)
				if err != nil {
					t.Fatal(err)
				}

				var patched unstructured.Unstructured
				if err := json.Unmarshal(patchedBytes, &patched.Object); err != nil {
					t.Fatal(err)
				}

				if !reflect.DeepEqual(&patched, upgraded) {
					t.Fatalf("expected patch to produce an upgraded object: %v", cmp.Diff(patched, upgraded))
				}
			}
		})
	}
}
