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

package apiresources

import (
	"testing"

	"github.com/spf13/cobra"

	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	cmdtesting "k8s.io/kubectl/pkg/cmd/testing"
)

func TestAPIResourcesComplete(t *testing.T) {
	tf := cmdtesting.NewTestFactory()
	defer tf.Cleanup()
	cmd := NewCmdAPIResources(tf, genericiooptions.NewTestIOStreamsDiscard())
	parentCmd := &cobra.Command{Use: "kubectl"}
	parentCmd.AddCommand(cmd)
	o := NewAPIResourceOptions(genericiooptions.NewTestIOStreamsDiscard())

	err := o.Complete(tf, cmd, []string{})
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	err = o.Complete(tf, cmd, []string{"foo"})
	if err == nil {
		t.Fatalf("An error was expected but not returned")
	}
	expectedError := `unexpected arguments: [foo]
See 'kubectl api-resources -h' for help and examples`
	if err.Error() != expectedError {
		t.Fatalf("Unexpected error: %v\n expected: %v", err, expectedError)
	}
}

func TestAPIResourcesValidate(t *testing.T) {
	testCases := []struct {
		name          string
		optionSetupFn func(o *APIResourceOptions)
		expectedError string
	}{
		{
			name:          "no errors",
			optionSetupFn: func(o *APIResourceOptions) {},
			expectedError: "",
		},
		{
			name: "invalid output",
			optionSetupFn: func(o *APIResourceOptions) {
				*o.PrintFlags.OutputFormat = "foo"
			},
			expectedError: "--output foo is not available",
		},
		{
			name: "invalid sort by",
			optionSetupFn: func(o *APIResourceOptions) {
				o.SortBy = "foo"
			},
			expectedError: "--sort-by accepts only name or kind",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(tt *testing.T) {
			o := NewAPIResourceOptions(genericiooptions.NewTestIOStreamsDiscard())
			tc.optionSetupFn(o)
			err := o.Validate()
			if tc.expectedError == "" {
				if err != nil {
					tt.Fatalf("Unexpected error: %v", err)
				}
			} else {
				if err == nil {
					tt.Fatalf("An error was expected but not returned")
				}
				if err.Error() != tc.expectedError {
					tt.Fatalf("Unexpected error: %v, expected: %v", err, tc.expectedError)
				}
			}
		})
	}
}

func TestAPIResourcesRun(t *testing.T) {
	dc := cmdtesting.NewFakeCachedDiscoveryClient()
	dc.PreferredResources = []*v1.APIResourceList{
		{
			GroupVersion: "v1",
			APIResources: []v1.APIResource{
				{
					Name:       "foos",
					Namespaced: false,
					Kind:       "Foo",
					Verbs:      []string{"get", "list"},
					ShortNames: []string{"f", "fo"},
					Categories: []string{"some-category"},
				},
				{
					Name:       "bars",
					Namespaced: true,
					Kind:       "Bar",
					Verbs:      []string{"get", "list", "create"},
					ShortNames: []string{},
					Categories: []string{},
				},
			},
		},
		{
			GroupVersion: "somegroup/v1",
			APIResources: []v1.APIResource{
				{
					Name:       "bazzes",
					Namespaced: true,
					Kind:       "Baz",
					Verbs:      []string{"get", "list", "create", "delete"},
					ShortNames: []string{"b"},
					Categories: []string{"some-category", "another-category"},
				},
				{
					Name:       "NoVerbs",
					Namespaced: true,
					Kind:       "NoVerbs",
					Verbs:      []string{},
					ShortNames: []string{"b"},
					Categories: []string{},
				},
			},
		},
		{
			GroupVersion: "someothergroup/v1",
			APIResources: []v1.APIResource{},
		},
	}
	tf := cmdtesting.NewTestFactory().WithDiscoveryClient(dc)
	defer tf.Cleanup()

	testCases := []struct {
		name                  string
		commandSetupFn        func(cmd *cobra.Command)
		expectedOutput        string
		expectedInvalidations int
	}{
		{
			name:           "defaults",
			commandSetupFn: func(cmd *cobra.Command) {},
			expectedOutput: `NAME     SHORTNAMES   APIVERSION     NAMESPACED   KIND
bars                  v1             true         Bar
foos     f,fo         v1             false        Foo
bazzes   b            somegroup/v1   true         Baz
`,
			expectedInvalidations: 1,
		},
		{
			name: "no headers",
			commandSetupFn: func(cmd *cobra.Command) {
				cmd.Flags().Set("no-headers", "true")
			},
			expectedOutput: `bars            v1             true    Bar
foos     f,fo   v1             false   Foo
bazzes   b      somegroup/v1   true    Baz
`,
			expectedInvalidations: 1,
		},
		{
			name: "specific api group",
			commandSetupFn: func(cmd *cobra.Command) {
				cmd.Flags().Set("api-group", "somegroup")
			},
			expectedOutput: `NAME     SHORTNAMES   APIVERSION     NAMESPACED   KIND
bazzes   b            somegroup/v1   true         Baz
`,
			expectedInvalidations: 1,
		},
		{
			name: "output wide",
			commandSetupFn: func(cmd *cobra.Command) {
				cmd.Flags().Set("output", "wide")
			},
			expectedOutput: `NAME     SHORTNAMES   APIVERSION     NAMESPACED   KIND   VERBS                    CATEGORIES
bars                  v1             true         Bar    get,list,create          
foos     f,fo         v1             false        Foo    get,list                 some-category
bazzes   b            somegroup/v1   true         Baz    get,list,create,delete   some-category,another-category
`,
			expectedInvalidations: 1,
		},
		{
			name: "output wide no-headers",
			commandSetupFn: func(cmd *cobra.Command) {
				cmd.Flags().Set("output", "wide")
				cmd.Flags().Set("no-headers", "true")
			},
			expectedOutput: `bars            v1             true    Bar   get,list,create          
foos     f,fo   v1             false   Foo   get,list                 some-category
bazzes   b      somegroup/v1   true    Baz   get,list,create,delete   some-category,another-category
`,
			expectedInvalidations: 1,
		},
		{
			name: "output name",
			commandSetupFn: func(cmd *cobra.Command) {
				cmd.Flags().Set("output", "name")
			},
			expectedOutput: `bars
foos
bazzes.somegroup
`,
			expectedInvalidations: 1,
		},
		{
			name: "namespaced",
			commandSetupFn: func(cmd *cobra.Command) {
				cmd.Flags().Set("namespaced", "true")
			},
			expectedOutput: `NAME     SHORTNAMES   APIVERSION     NAMESPACED   KIND
bars                  v1             true         Bar
bazzes   b            somegroup/v1   true         Baz
`,
			expectedInvalidations: 1,
		},
		{
			name: "single verb",
			commandSetupFn: func(cmd *cobra.Command) {
				cmd.Flags().Set("verbs", "create")
			},
			expectedOutput: `NAME     SHORTNAMES   APIVERSION     NAMESPACED   KIND
bars                  v1             true         Bar
bazzes   b            somegroup/v1   true         Baz
`,
			expectedInvalidations: 1,
		},
		{
			name: "multiple verbs",
			commandSetupFn: func(cmd *cobra.Command) {
				cmd.Flags().Set("verbs", "create,delete")
			},
			expectedOutput: `NAME     SHORTNAMES   APIVERSION     NAMESPACED   KIND
bazzes   b            somegroup/v1   true         Baz
`,
			expectedInvalidations: 1,
		},
		{
			name: "single category",
			commandSetupFn: func(cmd *cobra.Command) {
				cmd.Flags().Set("categories", "some-category")
			},
			expectedOutput: `NAME     SHORTNAMES   APIVERSION     NAMESPACED   KIND
foos     f,fo         v1             false        Foo
bazzes   b            somegroup/v1   true         Baz
`,
			expectedInvalidations: 1,
		},
		{
			name: "multiple categories",
			commandSetupFn: func(cmd *cobra.Command) {
				cmd.Flags().Set("categories", "some-category,another-category")
			},
			expectedOutput: `NAME     SHORTNAMES   APIVERSION     NAMESPACED   KIND
bazzes   b            somegroup/v1   true         Baz
`,
			expectedInvalidations: 1,
		},
		{
			name: "sort by name",
			commandSetupFn: func(cmd *cobra.Command) {
				cmd.Flags().Set("sort-by", "name")
			},
			expectedOutput: `NAME     SHORTNAMES   APIVERSION     NAMESPACED   KIND
bars                  v1             true         Bar
bazzes   b            somegroup/v1   true         Baz
foos     f,fo         v1             false        Foo
`,
			expectedInvalidations: 1,
		},
		{
			name: "sort by kind",
			commandSetupFn: func(cmd *cobra.Command) {
				cmd.Flags().Set("sort-by", "kind")
			},
			expectedOutput: `NAME     SHORTNAMES   APIVERSION     NAMESPACED   KIND
bars                  v1             true         Bar
bazzes   b            somegroup/v1   true         Baz
foos     f,fo         v1             false        Foo
`,
			expectedInvalidations: 1,
		},
		{
			name: "cached",
			commandSetupFn: func(cmd *cobra.Command) {
				cmd.Flags().Set("cached", "true")
			},
			expectedOutput: `NAME     SHORTNAMES   APIVERSION     NAMESPACED   KIND
bars                  v1             true         Bar
foos     f,fo         v1             false        Foo
bazzes   b            somegroup/v1   true         Baz
`,
			expectedInvalidations: 0,
		},
		{
			name: "json",
			commandSetupFn: func(cmd *cobra.Command) {
				cmd.Flags().Set("output", "json")
			},
			expectedOutput: `{
    "kind": "APIResourceList",
    "apiVersion": "v1",
    "groupVersion": "",
    "resources": [
        {
            "name": "foos",
            "singularName": "",
            "namespaced": false,
            "kind": "Foo",
            "verbs": [
                "get",
                "list"
            ],
            "shortNames": [
                "f",
                "fo"
            ],
            "categories": [
                "some-category"
            ]
        },
        {
            "name": "bars",
            "singularName": "",
            "namespaced": true,
            "kind": "Bar",
            "verbs": [
                "get",
                "list",
                "create"
            ]
        },
        {
            "name": "bazzes",
            "singularName": "",
            "namespaced": true,
            "kind": "Baz",
            "verbs": [
                "get",
                "list",
                "create",
                "delete"
            ],
            "shortNames": [
                "b"
            ],
            "categories": [
                "some-category",
                "another-category"
            ]
        }
    ]
}
`,
			expectedInvalidations: 1,
		},
		{
			name: "yaml",
			commandSetupFn: func(cmd *cobra.Command) {
				cmd.Flags().Set("output", "yaml")
			},
			expectedOutput: `apiVersion: v1
groupVersion: ""
kind: APIResourceList
resources:
- categories:
  - some-category
  kind: Foo
  name: foos
  namespaced: false
  shortNames:
  - f
  - fo
  singularName: ""
  verbs:
  - get
  - list
- kind: Bar
  name: bars
  namespaced: true
  singularName: ""
  verbs:
  - get
  - list
  - create
- categories:
  - some-category
  - another-category
  kind: Baz
  name: bazzes
  namespaced: true
  shortNames:
  - b
  singularName: ""
  verbs:
  - get
  - list
  - create
  - delete
`,
			expectedInvalidations: 1,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(tt *testing.T) {
			dc.Invalidations = 0
			ioStreams, _, out, errOut := genericiooptions.NewTestIOStreams()
			cmd := NewCmdAPIResources(tf, ioStreams)
			tc.commandSetupFn(cmd)
			cmd.Run(cmd, []string{})

			if errOut.Len() > 0 {
				t.Fatalf("unexpected error output: %s", errOut.String())
			}
			if out.String() != tc.expectedOutput {
				tt.Fatalf("unexpected output: %s\nexpected: %s", out.String(), tc.expectedOutput)
			}
			if dc.Invalidations != tc.expectedInvalidations {
				tt.Fatalf("unexpected invalidations: %d, expected: %d", dc.Invalidations, tc.expectedInvalidations)
			}
		})
	}
}
