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

package explain_test

import (
	"errors"
	"path/filepath"
	"regexp"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	sptest "k8s.io/apimachinery/pkg/util/strategicpatch/testing"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	"k8s.io/client-go/discovery"
	openapiclient "k8s.io/client-go/openapi"
	"k8s.io/client-go/rest"
	clienttestutil "k8s.io/client-go/util/testing"
	"k8s.io/kubectl/pkg/cmd/explain"
	cmdtesting "k8s.io/kubectl/pkg/cmd/testing"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/util/openapi"
)

var (
	testDataPath      = filepath.Join("..", "..", "..", "testdata")
	fakeSchema        = sptest.Fake{Path: filepath.Join(testDataPath, "openapi", "swagger.json")}
	FakeOpenAPISchema = testOpenAPISchema{
		OpenAPISchemaFn: func() (openapi.Resources, error) {
			s, err := fakeSchema.OpenAPISchema()
			if err != nil {
				return nil, err
			}
			return openapi.NewOpenAPIData(s)
		},
	}
)

type testOpenAPISchema struct {
	OpenAPISchemaFn func() (openapi.Resources, error)
}

func TestExplainInvalidArgs(t *testing.T) {
	tf := cmdtesting.NewTestFactory()
	defer tf.Cleanup()

	flags := explain.NewExplainFlags(genericiooptions.NewTestIOStreamsDiscard())

	opts, err := flags.ToOptions(tf, "kubectl", []string{})
	if err != nil {
		t.Fatalf("unexpected error %v", err)
	}

	err = opts.Validate()
	if err.Error() != "You must specify the type of resource to explain. Use \"kubectl api-resources\" for a complete list of supported resources.\n" {
		t.Error("unexpected non-error")
	}

	opts, err = flags.ToOptions(tf, "kubectl", []string{"resource1", "resource2"})
	if err != nil {
		t.Fatalf("unexpected error %v", err)
	}

	err = opts.Validate()
	if err.Error() != "We accept only this format: explain RESOURCE\n" {
		t.Error("unexpected non-error")
	}
}

func TestExplainNotExistResource(t *testing.T) {
	tf := cmdtesting.NewTestFactory()
	defer tf.Cleanup()

	flags := explain.NewExplainFlags(genericiooptions.NewTestIOStreamsDiscard())

	opts, err := flags.ToOptions(tf, "kubectl", []string{"foo"})
	if err != nil {
		t.Fatalf("unexpected error %v", err)
	}

	err = opts.Validate()
	if err != nil {
		t.Fatalf("unexpected error %v", err)
	}

	err = opts.Run()
	if _, ok := err.(*meta.NoResourceMatchError); !ok {
		t.Fatalf("unexpected error %v", err)
	}
}

type explainTestCase struct {
	Name               string
	Args               []string
	Flags              map[string]string
	ExpectPattern      []string
	ExpectErrorPattern string

	// Custom OpenAPI V3 client to use for the test. If nil, a default one will
	// be provided
	OpenAPIV3SchemaFn func() (openapiclient.Client, error)
}

var explainV2Cases = []explainTestCase{
	{
		Name:          "Basic",
		Args:          []string{"pods"},
		ExpectPattern: []string{`\s*KIND:[\t ]*Pod\s*`},
	},
	{
		Name:          "Recursive",
		Args:          []string{"pods"},
		Flags:         map[string]string{"recursive": "true"},
		ExpectPattern: []string{`\s*KIND:[\t ]*Pod\s*`},
	},
	{
		Name:          "DefaultAPIVersion",
		Args:          []string{"horizontalpodautoscalers"},
		Flags:         map[string]string{"api-version": "autoscaling/v1"},
		ExpectPattern: []string{`\s*VERSION:[\t ]*(v1|autoscaling/v1)\s*`},
	},
	{
		Name:               "NonExistingAPIVersion",
		Args:               []string{"pods"},
		Flags:              map[string]string{"api-version": "v99"},
		ExpectErrorPattern: `couldn't find resource for \"/v99, (Kind=Pod|Resource=pods)\"`,
	},
	{
		Name:               "NonExistingResource",
		Args:               []string{"foo"},
		ExpectErrorPattern: `the server doesn't have a resource type "foo"`,
	},
}

func TestExplainOpenAPIV2(t *testing.T) {
	runExplainTestCases(t, explainV2Cases)
}

func TestExplainOpenAPIV3(t *testing.T) {

	fallbackV3SchemaFn := func() (openapiclient.Client, error) {
		fakeDiscoveryClient := discovery.NewDiscoveryClientForConfigOrDie(&rest.Config{Host: "https://not.a.real.site:65543/"})
		return fakeDiscoveryClient.OpenAPIV3(), nil
	}
	// Returns a client that causes fallback to v2 implementation
	cases := []explainTestCase{
		{
			// No --output, but OpenAPIV3 enabled should fall back to v2 if
			// v2 is not available. Shows this by making openapiv3 client
			// point to a bad URL. So the fact the proper data renders is
			// indication v2 was used instead.
			Name:              "Fallback",
			Args:              []string{"pods"},
			ExpectPattern:     []string{`\s*KIND:[\t ]*Pod\s*`},
			OpenAPIV3SchemaFn: fallbackV3SchemaFn,
		},
		{
			Name:          "NonDefaultAPIVersion",
			Args:          []string{"horizontalpodautoscalers"},
			Flags:         map[string]string{"api-version": "autoscaling/v2"},
			ExpectPattern: []string{`\s*VERSION:[\t ]*(v2|autoscaling/v2)\s*`},
		},
		{
			// Show that explicitly specifying --output plaintext-openapiv2 causes
			// old implementation to be used even though OpenAPIV3 is enabled
			Name:              "OutputPlaintextV2",
			Args:              []string{"pods"},
			Flags:             map[string]string{"output": "plaintext-openapiv2"},
			ExpectPattern:     []string{`\s*KIND:[\t ]*Pod\s*`},
			OpenAPIV3SchemaFn: fallbackV3SchemaFn,
		},
	}
	cases = append(cases, explainV2Cases...)

	runExplainTestCases(t, cases)
}

func runExplainTestCases(t *testing.T, cases []explainTestCase) {
	fakeServer, err := clienttestutil.NewFakeOpenAPIV3Server(filepath.Join(testDataPath, "openapi", "v3"))
	if err != nil {
		t.Fatalf("error starting fake openapi server: %v", err.Error())
	}
	defer fakeServer.HttpServer.Close()

	openapiV3SchemaFn := func() (openapiclient.Client, error) {
		fakeDiscoveryClient := discovery.NewDiscoveryClientForConfigOrDie(&rest.Config{Host: fakeServer.HttpServer.URL})
		return fakeDiscoveryClient.OpenAPIV3(), nil
	}

	tf := cmdtesting.NewTestFactory()
	defer tf.Cleanup()

	tf.OpenAPISchemaFunc = FakeOpenAPISchema.OpenAPISchemaFn
	tf.ClientConfigVal = cmdtesting.DefaultClientConfig()

	ioStreams, _, buf, _ := genericiooptions.NewTestIOStreams()

	type catchFatal error

	for _, tcase := range cases {

		t.Run(tcase.Name, func(t *testing.T) {

			// Catch os.Exit calls for tests which expect them
			// and replace them with panics that we catch in each test
			// to check if it is expected.
			cmdutil.BehaviorOnFatal(func(str string, code int) {
				panic(catchFatal(errors.New(str)))
			})
			defer cmdutil.DefaultBehaviorOnFatal()

			var err error

			func() {
				defer func() {
					// Catch panic and check at end of test if it is
					// expected.
					if panicErr := recover(); panicErr != nil {
						if e := panicErr.(catchFatal); e != nil {
							err = e
						} else {
							panic(panicErr)
						}
					}
				}()

				if tcase.OpenAPIV3SchemaFn != nil {
					tf.OpenAPIV3ClientFunc = tcase.OpenAPIV3SchemaFn
				} else {
					tf.OpenAPIV3ClientFunc = openapiV3SchemaFn
				}

				cmd := explain.NewCmdExplain("kubectl", tf, ioStreams)
				for k, v := range tcase.Flags {
					if err := cmd.Flags().Set(k, v); err != nil {
						t.Fatal(err)
					}
				}
				cmd.Run(cmd, tcase.Args)
			}()

			for _, rexp := range tcase.ExpectPattern {
				if matched, err := regexp.MatchString(rexp, buf.String()); err != nil || !matched {
					if err != nil {
						t.Error(err)
					} else {
						t.Errorf("expected output to match regex:\n\t%s\ninstead got:\n\t%s", rexp, buf.String())
					}
				}
			}

			if err != nil {
				if matched, regexErr := regexp.MatchString(tcase.ExpectErrorPattern, err.Error()); len(tcase.ExpectErrorPattern) == 0 || regexErr != nil || !matched {
					t.Fatalf("unexpected error: %s did not match regex %s (%v)", err.Error(),
						tcase.ExpectErrorPattern, regexErr)
				}
			} else if len(tcase.ExpectErrorPattern) > 0 {
				t.Fatalf("did not trigger expected error: %s in output:\n%s", tcase.ExpectErrorPattern, buf.String())
			}
		})

		buf.Reset()
	}
}

// OpenAPI V2 specifications retrieval -- should never be called.
func panicOpenAPISchemaFn() (openapi.Resources, error) {
	panic("should never be called")
}

// OpenAPI V3 specifications retrieval does *not* retrieve V2 specifications.
func TestExplainOpenAPIV3DoesNotLoadOpenAPIV2Specs(t *testing.T) {
	// Set up OpenAPI V3 specifications endpoint for explain.
	fakeServer, err := clienttestutil.NewFakeOpenAPIV3Server(filepath.Join(testDataPath, "openapi", "v3"))
	if err != nil {
		t.Fatalf("error starting fake openapi server: %v", err.Error())
	}
	defer fakeServer.HttpServer.Close()
	tf := cmdtesting.NewTestFactory()
	defer tf.Cleanup()
	tf.OpenAPIV3ClientFunc = func() (openapiclient.Client, error) {
		fakeDiscoveryClient := discovery.NewDiscoveryClientForConfigOrDie(&rest.Config{Host: fakeServer.HttpServer.URL})
		return fakeDiscoveryClient.OpenAPIV3(), nil
	}
	// OpenAPI V2 specifications retrieval will panic if called.
	tf.OpenAPISchemaFunc = panicOpenAPISchemaFn

	// Explain the following resources, validating the command does not panic.
	cmd := explain.NewCmdExplain("kubectl", tf, genericiooptions.NewTestIOStreamsDiscard())
	resources := []string{"pods", "services", "endpoints", "configmaps"}
	for _, resource := range resources {
		cmd.Run(cmd, []string{resource})
	}
	// Verify retrieving OpenAPI V2 specifications will panic.
	defer func() {
		if panicErr := recover(); panicErr == nil {
			t.Fatal("expecting panic for openapi v2 retrieval")
		}
	}()
	// Set OpenAPI V2 output flag for explain.
	if err := cmd.Flags().Set("output", "plaintext-openapiv2"); err != nil {
		t.Fatal(err)
	}
	cmd.Run(cmd, []string{"pods"})
}

var depthRecursiveErrorCases = []explainTestCase{
	{
		Name:               "DepthWithoutRecursive",
		Args:               []string{"pods"},
		Flags:              map[string]string{"recursive": "false", "depth": "1"},
		ExpectErrorPattern: `The --depth flag can only be used when --recursive is specified\n`,
	},
	{
		Name:               "NegativeDepthWithRecursive",
		Args:               []string{"pods"},
		Flags:              map[string]string{"recursive": "true", "depth": "-1"},
		ExpectErrorPattern: `Depth must be a non-negative number\n`,
	},
}

func TestExplainDepthRecursiveErrorCases(t *testing.T) {
	runExplainTestCases(t, depthRecursiveErrorCases)
}

var depthTestCases = []explainTestCase{
	{
		Name:  "Depth=1",
		Args:  []string{"pods.spec.affinity"},
		Flags: map[string]string{"recursive": "true", "depth": "1"},
		ExpectPattern: []string{
			`\s*KIND:[\t ]*Pod\s*`,
			`\s*VERSION:[\t ]*v1\s*`,
			`\s*FIELD:[\t ]*affinity\s*`,
			`\s*DESCRIPTION:\s*`,
			`If specified, the pod's scheduling constraints`,
			`\s*FIELDS:\s*`,
			`\s*nodeAffinity\s*<NodeAffinity>`,
			`\s*podAffinity\s*<PodAffinity>`,
			`\s*podAntiAffinity\s*<PodAntiAffinity>`,
		},
	},
	{
		Name:  "Depth=2",
		Args:  []string{"pods.spec.affinity"},
		Flags: map[string]string{"recursive": "true", "depth": "2"},
		ExpectPattern: []string{
			`\s*KIND:[\t ]*Pod\s*`,
			`\s*VERSION:[\t ]*v1\s*`,
			`\s*FIELD:[\t ]*affinity\s*`,
			`\s*DESCRIPTION:\s*`,
			`If specified, the pod's scheduling constraints`,
			`\s*FIELDS:\s*`,
			`\s*nodeAffinity\s*<NodeAffinity>`,
			`\s*preferredDuringSchedulingIgnoredDuringExecution\s*<\[\]PreferredSchedulingTerm>`,
			`\s*requiredDuringSchedulingIgnoredDuringExecution\s*<NodeSelector>`,
			`\s*podAffinity\s*<PodAffinity>`,
			`\s*preferredDuringSchedulingIgnoredDuringExecution\s*<\[\]WeightedPodAffinityTerm>`,
			`\s*requiredDuringSchedulingIgnoredDuringExecution\s*<\[\]PodAffinityTerm>`,
			`\s*podAntiAffinity\s*<PodAntiAffinity>`,
			`\s*preferredDuringSchedulingIgnoredDuringExecution\s*<\[\]WeightedPodAffinityTerm>`,
			`\s*requiredDuringSchedulingIgnoredDuringExecution\s*<\[\]PodAffinityTerm>`,
		},
	},
	{
		Name:  "Depth=1_plaintext-openapiv2",
		Args:  []string{"pods.spec.affinity"},
		Flags: map[string]string{"recursive": "true", "depth": "1", "output": "plaintext-openapiv2"},
		ExpectPattern: []string{
			`\s*KIND:[\t ]*Pod\s*`,
			`\s*VERSION:[\t ]*v1\s*`,
			`\s*RESOURCE:[\t ]*affinity\s*<Object>\s*`,
			`\s*DESCRIPTION:\s*`,
			`If specified, the pod's scheduling constraints`,
			`Affinity is a group of affinity scheduling rules\.`,
			`\s*FIELDS:\s*`,
			`\s*nodeAffinity\s*<Object>\s*`,
			`\s*podAffinity\s*<Object>\s*`,
			`\s*podAntiAffinity\s*<Object>\s*`,
		},
	},
	{
		Name:  "Depth=2_plaintext-openapiv2",
		Args:  []string{"pods.spec.affinity"},
		Flags: map[string]string{"recursive": "true", "depth": "2", "output": "plaintext-openapiv2"},
		ExpectPattern: []string{
			`\s*KIND:[\t ]*Pod\s*`,
			`\s*VERSION:[\t ]*v1\s*`,
			`\s*RESOURCE:[\t ]*affinity\s*<Object>\s*`,
			`\s*DESCRIPTION:\s*`,
			`If specified, the pod's scheduling constraints`,
			`Affinity is a group of affinity scheduling rules\.`,
			`\s*FIELDS:\s*`,
			`\s*nodeAffinity\s*<Object>\s*`,
			`\s*preferredDuringSchedulingIgnoredDuringExecution\s*<\[\]Object>\s*`,
			`\s*requiredDuringSchedulingIgnoredDuringExecution\s*<Object>\s*`,
			`\s*podAffinity\s*<Object>\s*`,
			`\s*preferredDuringSchedulingIgnoredDuringExecution\s*<\[\]Object>\s*`,
			`\s*requiredDuringSchedulingIgnoredDuringExecution\s*<\[\]Object>\s*`,
			`\s*podAntiAffinity\s*<Object>\s*`,
			`\s*preferredDuringSchedulingIgnoredDuringExecution\s*<\[\]Object>\s*`,
			`\s*requiredDuringSchedulingIgnoredDuringExecution\s*<\[\]Object>\s*`,
		},
	},
}

func TestExplainDepthVariable(t *testing.T) {
	runExplainTestCases(t, depthTestCases)
}
