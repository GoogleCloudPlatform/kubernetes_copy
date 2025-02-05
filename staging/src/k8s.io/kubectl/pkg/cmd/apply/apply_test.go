/*
Copyright 2014 The Kubernetes Authors.

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

package apply

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	sptest "k8s.io/apimachinery/pkg/util/strategicpatch/testing"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	"k8s.io/cli-runtime/pkg/resource"
	dynamicfakeclient "k8s.io/client-go/dynamic/fake"
	openapiclient "k8s.io/client-go/openapi"
	"k8s.io/client-go/openapi/openapitest"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/rest/fake"
	testing2 "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/client-go/util/csaupgrade"
	cmdtesting "k8s.io/kubectl/pkg/cmd/testing"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/scheme"
	"k8s.io/kubectl/pkg/util/openapi"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/yaml"
)

var (
	fakeSchema            = sptest.Fake{Path: filepath.Join("..", "..", "..", "testdata", "openapi", "swagger.json")}
	fakeOpenAPIV3Legacy   = sptest.OpenAPIV3Getter{Path: filepath.Join("..", "..", "..", "testdata", "openapi", "v3", "api", "v1.json")}
	fakeOpenAPIV3AppsV1   = sptest.OpenAPIV3Getter{Path: filepath.Join("..", "..", "..", "testdata", "openapi", "v3", "apis", "apps", "v1.json")}
	testingOpenAPISchemas = []testOpenAPISchema{AlwaysErrorsOpenAPISchema, FakeOpenAPISchema}

	AlwaysErrorsOpenAPISchema = testOpenAPISchema{
		OpenAPISchemaFn: func() (openapi.Resources, error) {
			return nil, errors.New("cannot get openapi spec")
		},
		OpenAPIV3ClientFunc: func() (openapiclient.Client, error) {
			return nil, errors.New("cannot get openapiv3 client")
		},
	}
	FakeOpenAPISchema = testOpenAPISchema{
		OpenAPISchemaFn: func() (openapi.Resources, error) {
			s, err := fakeSchema.OpenAPISchema()
			if err != nil {
				return nil, err
			}
			return openapi.NewOpenAPIData(s)
		},
		OpenAPIV3ClientFunc: func() (openapiclient.Client, error) {
			c := openapitest.NewFakeClient()
			c.PathsMap["api/v1"] = openapitest.FakeGroupVersion{GVSpec: fakeOpenAPIV3Legacy.SchemaBytesOrDie()}
			c.PathsMap["apis/apps/v1"] = openapitest.FakeGroupVersion{GVSpec: fakeOpenAPIV3AppsV1.SchemaBytesOrDie()}
			return c, nil
		},
	}
	AlwaysPanicSchema = testOpenAPISchema{
		OpenAPISchemaFn: func() (openapi.Resources, error) {
			panic("error, openAPIV2 should not be called")
		},
		OpenAPIV3ClientFunc: func() (openapiclient.Client, error) {
			return &OpenAPIV3ClientAlwaysPanic{}, nil
		},
	}

	codec = scheme.Codecs.LegacyCodec(scheme.Scheme.PrioritizedVersionsAllGroups()...)
)

type OpenAPIV3ClientAlwaysPanic struct{}

func (o *OpenAPIV3ClientAlwaysPanic) Paths() (map[string]openapiclient.GroupVersion, error) {
	panic("Cannot get paths")
}

func noopOpenAPIV3Patch(t *testing.T, f func(t *testing.T)) {
	f(t)
}
func disableOpenAPIV3Patch(t *testing.T, f func(t *testing.T)) {
	cmdtesting.WithAlphaEnvsDisabled([]cmdutil.FeatureGate{cmdutil.OpenAPIV3Patch}, t, f)
}

var applyFeatureToggles = []func(*testing.T, func(t *testing.T)){noopOpenAPIV3Patch, disableOpenAPIV3Patch}

type testOpenAPISchema struct {
	OpenAPISchemaFn     func() (openapi.Resources, error)
	OpenAPIV3ClientFunc func() (openapiclient.Client, error)
}

func TestApplyExtraArgsFail(t *testing.T) {
	f := cmdtesting.NewTestFactory()
	defer f.Cleanup()

	cmd := &cobra.Command{}
	flags := NewApplyFlags(genericiooptions.NewTestIOStreamsDiscard())
	flags.AddFlags(cmd)
	_, err := flags.ToOptions(f, cmd, "kubectl", []string{"rc"})
	require.EqualError(t, err, "Unexpected args: [rc]\nSee ' -h' for help and examples")
}

func TestAlphaEnablement(t *testing.T) {
	alphas := map[cmdutil.FeatureGate]string{
		cmdutil.ApplySet: "applyset",
	}
	for feature, flag := range alphas {
		f := cmdtesting.NewTestFactory()
		defer f.Cleanup()

		cmd := &cobra.Command{}
		flags := NewApplyFlags(genericiooptions.NewTestIOStreamsDiscard())
		flags.AddFlags(cmd)
		require.Nil(t, cmd.Flags().Lookup(flag), "flag %q should not be registered without the %q feature enabled", flag, feature)

		cmdtesting.WithAlphaEnvs([]cmdutil.FeatureGate{feature}, t, func(t *testing.T) {
			cmd := &cobra.Command{}
			flags := NewApplyFlags(genericiooptions.NewTestIOStreamsDiscard())
			flags.AddFlags(cmd)
			require.NotNil(t, cmd.Flags().Lookup(flag), "flag %q should be registered with the %q feature enabled", flag, feature)
		})
	}
}

func TestApplyFlagValidation(t *testing.T) {
	tests := []struct {
		args         [][]string
		enableAlphas []cmdutil.FeatureGate
		expectedErr  string
	}{
		{
			args: [][]string{
				{"force-conflicts", "true"},
			},
			expectedErr: "--force-conflicts only works with --server-side",
		},
		{
			args: [][]string{
				{"server-side", "true"},
				{"dry-run", "client"},
			},
			expectedErr: "--dry-run=client doesn't work with --server-side (did you mean --dry-run=server instead?)",
		},
		{
			args: [][]string{
				{"force", "true"},
				{"server-side", "true"},
			},
			expectedErr: "--force cannot be used with --server-side",
		},
		{
			args: [][]string{
				{"force", "true"},
				{"dry-run", "server"},
			},
			expectedErr: "--dry-run=server cannot be used with --force",
		},
		{
			args: [][]string{
				{"all", "true"},
				{"selector", "unused"},
			},
			expectedErr: "cannot set --all and --selector at the same time",
		},
		{
			args: [][]string{
				{"force", "true"},
				{"prune", "true"},
				{"all", "true"},
			},
			expectedErr: "--force cannot be used with --prune",
		},
		{
			args: [][]string{
				{"prune", "true"},
				{"force", "true"},
				{"applyset", "mySecret"},
				{"namespace", "myNs"},
			},
			enableAlphas: []cmdutil.FeatureGate{cmdutil.ApplySet},
			expectedErr:  "--force cannot be used with --prune",
		},
		{
			args: [][]string{
				{"server-side", "true"},
				{"prune", "true"},
				{"all", "true"},
			},
			expectedErr: "--prune is in alpha and doesn't currently work on objects created by server-side apply",
		},
		{
			args: [][]string{
				{"prune", "true"},
			},
			expectedErr: "all resources selected for prune without explicitly passing --all. To prune all resources, pass the --all flag. If you did not mean to prune all resources, specify a label selector",
		},
		{
			args: [][]string{
				{"prune", "false"},
				{"applyset", "mySecret"},
				{"namespace", "myNs"},
			},
			enableAlphas: []cmdutil.FeatureGate{cmdutil.ApplySet},
			expectedErr:  "--applyset requires --prune",
		},
		{
			args: [][]string{
				{"prune", "true"},
				{"applyset", "mySecret"},
				{"selector", "foo=bar"},
				{"namespace", "myNs"},
			},
			enableAlphas: []cmdutil.FeatureGate{cmdutil.ApplySet},
			expectedErr:  "--selector is incompatible with --applyset",
		},
		{
			args: [][]string{
				{"prune", "true"},
				{"applyset", "mySecret"},
				{"namespace", "myNs"},
				{"all", "true"},
			},
			enableAlphas: []cmdutil.FeatureGate{cmdutil.ApplySet},
			expectedErr:  "--all is incompatible with --applyset",
		},
		{
			args: [][]string{
				{"prune", "true"},
				{"applyset", "mySecret"},
				{"namespace", "myNs"},
				{"prune-allowlist", "core/v1/ConfigMap"},
			},
			enableAlphas: []cmdutil.FeatureGate{cmdutil.ApplySet},
			expectedErr:  "--prune-allowlist is incompatible with --applyset",
		},
	}

	for i, test := range tests {
		t.Run(fmt.Sprintf("case %d", i), func(t *testing.T) {
			f := cmdtesting.NewTestFactory()
			defer f.Cleanup()
			f.Client = &fake.RESTClient{}
			f.UnstructuredClient = f.Client
			cmdtesting.WithAlphaEnvs(test.enableAlphas, t, func(t *testing.T) {
				cmd := &cobra.Command{}
				flags := NewApplyFlags(genericiooptions.NewTestIOStreamsDiscard())
				flags.AddFlags(cmd)
				cmd.Flags().Set("filename", "unused")
				for _, arg := range test.args {
					if arg[0] == "namespace" {
						f.WithNamespace(arg[1])
					} else {
						cmd.Flags().Set(arg[0], arg[1])
					}
				}
				o, err := flags.ToOptions(f, cmd, "kubectl", []string{})
				if err != nil {
					t.Fatalf("unexpected error creating apply options: %s", err)
				}
				err = o.Validate()
				if err == nil {
					t.Fatalf("missing expected error for case %d with args %+v", i, test.args)
				}
				if test.expectedErr != err.Error() {
					t.Errorf("expected error %s, got %s", test.expectedErr, err)
				}
			})
		})
	}
}

const (
	filenameCM                = "../../../testdata/apply/cm.yaml"
	filenameRC                = "../../../testdata/apply/rc.yaml"
	filenameRCArgs            = "../../../testdata/apply/rc-args.yaml"
	filenameRCLastAppliedArgs = "../../../testdata/apply/rc-lastapplied-args.yaml"
	filenameRCNoAnnotation    = "../../../testdata/apply/rc-no-annotation.yaml"
	filenameRCLASTAPPLIED     = "../../../testdata/apply/rc-lastapplied.yaml"
	filenameRCManagedFieldsLA = "../../../testdata/apply/rc-managedfields-lastapplied.yaml"
	filenameSVC               = "../../../testdata/apply/service.yaml"
	filenameRCSVC             = "../../../testdata/apply/rc-service.yaml"
	filenameNoExistRC         = "../../../testdata/apply/rc-noexist.yaml"
	filenameRCPatchTest       = "../../../testdata/apply/patch.json"
	dirName                   = "../../../testdata/apply/testdir"
	filenameRCJSON            = "../../../testdata/apply/rc.json"
	filenamePodGeneratedName  = "../../../testdata/apply/pod-generated-name.yaml"

	filenameWidgetClientside    = "../../../testdata/apply/widget-clientside.yaml"
	filenameWidgetServerside    = "../../../testdata/apply/widget-serverside.yaml"
	filenameDeployObjServerside = "../../../testdata/apply/deploy-serverside.yaml"
	filenameDeployObjClientside = "../../../testdata/apply/deploy-clientside.yaml"
	filenameApplySetCR          = "../../../testdata/apply/applyset-cr.yaml"
	filenameApplySetCRD         = "../../../testdata/apply/applysets-crd.yaml"
)

func readConfigMapList(t *testing.T, filename string) [][]byte {
	data := readBytesFromFile(t, filename)
	cmList := corev1.ConfigMapList{}
	if err := runtime.DecodeInto(codec, data, &cmList); err != nil {
		t.Fatal(err)
	}

	var listCmBytes [][]byte

	for _, cm := range cmList.Items {
		cmBytes, err := runtime.Encode(codec, &cm)
		if err != nil {
			t.Fatal(err)
		}
		listCmBytes = append(listCmBytes, cmBytes)
	}

	return listCmBytes
}

func readBytesFromFile(t *testing.T, filename string) []byte {
	file, err := os.Open(filename)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	data, err := io.ReadAll(file)
	if err != nil {
		t.Fatal(err)
	}

	return data
}

func readReplicationController(t *testing.T, filenameRC string) (string, []byte) {
	rcObj := readReplicationControllerFromFile(t, filenameRC)
	metaAccessor, err := meta.Accessor(rcObj)
	if err != nil {
		t.Fatal(err)
	}
	rcBytes, err := runtime.Encode(codec, rcObj)
	if err != nil {
		t.Fatal(err)
	}

	return metaAccessor.GetName(), rcBytes
}

func readReplicationControllerFromFile(t *testing.T, filename string) *corev1.ReplicationController {
	data := readBytesFromFile(t, filename)
	rc := corev1.ReplicationController{}
	if err := runtime.DecodeInto(codec, data, &rc); err != nil {
		t.Fatal(err)
	}

	return &rc
}

func readUnstructuredFromFile(t *testing.T, filename string) *unstructured.Unstructured {
	data := readBytesFromFile(t, filename)
	unst := unstructured.Unstructured{}
	if err := runtime.DecodeInto(codec, data, &unst); err != nil {
		t.Fatal(err)
	}
	return &unst
}

func readServiceFromFile(t *testing.T, filename string) *corev1.Service {
	data := readBytesFromFile(t, filename)
	svc := corev1.Service{}
	if err := runtime.DecodeInto(codec, data, &svc); err != nil {
		t.Fatal(err)
	}

	return &svc
}

func annotateRuntimeObject(t *testing.T, originalObj, currentObj runtime.Object, kind string) (string, []byte) {
	originalAccessor, err := meta.Accessor(originalObj)
	if err != nil {
		t.Fatal(err)
	}

	// The return value of this function is used in the body of the GET
	// request in the unit tests. Here we are adding a misc label to the object.
	// In tests, the validatePatchApplication() gets called in PATCH request
	// handler in fake round tripper. validatePatchApplication call
	// checks that this DELETE_ME label was deleted by the apply implementation in
	// kubectl.
	originalLabels := originalAccessor.GetLabels()
	originalLabels["DELETE_ME"] = "DELETE_ME"
	originalAccessor.SetLabels(originalLabels)
	original, err := runtime.Encode(unstructured.NewJSONFallbackEncoder(codec), originalObj)
	if err != nil {
		t.Fatal(err)
	}

	currentAccessor, err := meta.Accessor(currentObj)
	if err != nil {
		t.Fatal(err)
	}

	currentAnnotations := currentAccessor.GetAnnotations()
	if currentAnnotations == nil {
		currentAnnotations = make(map[string]string)
	}
	currentAnnotations[corev1.LastAppliedConfigAnnotation] = string(original)
	currentAccessor.SetAnnotations(currentAnnotations)
	current, err := runtime.Encode(unstructured.NewJSONFallbackEncoder(codec), currentObj)
	if err != nil {
		t.Fatal(err)
	}

	return currentAccessor.GetName(), current
}

func readAndAnnotateReplicationController(t *testing.T, filename string) (string, []byte) {
	rc1 := readReplicationControllerFromFile(t, filename)
	rc2 := readReplicationControllerFromFile(t, filename)
	return annotateRuntimeObject(t, rc1, rc2, "ReplicationController")
}

func readAndAnnotateService(t *testing.T, filename string) (string, []byte) {
	svc1 := readServiceFromFile(t, filename)
	svc2 := readServiceFromFile(t, filename)
	return annotateRuntimeObject(t, svc1, svc2, "Service")
}

func readAndAnnotateUnstructured(t *testing.T, filename string) (string, []byte) {
	obj1 := readUnstructuredFromFile(t, filename)
	obj2 := readUnstructuredFromFile(t, filename)
	return annotateRuntimeObject(t, obj1, obj2, "Widget")
}

func validatePatchApplication(t *testing.T, req *http.Request, patchType types.PatchType) {
	if got, wanted := req.Header.Get("Content-Type"), string(patchType); got != wanted {
		t.Fatalf("unexpected content-type expected: %s but actual %s\n", wanted, got)
	}

	patch, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatal(err)
	}

	patchMap := map[string]interface{}{}
	if err := json.Unmarshal(patch, &patchMap); err != nil {
		t.Fatal(err)
	}

	annotationsMap := walkMapPath(t, patchMap, []string{"metadata", "annotations"})
	if _, ok := annotationsMap[corev1.LastAppliedConfigAnnotation]; !ok {
		t.Fatalf("patch does not contain annotation:\n%s\n", patch)
	}

	labelMap := walkMapPath(t, patchMap, []string{"metadata", "labels"})
	if deleteMe, ok := labelMap["DELETE_ME"]; !ok || deleteMe != nil {
		t.Fatalf("patch does not remove deleted key: DELETE_ME:\n%s\n", patch)
	}
}

func walkMapPath(t *testing.T, start map[string]interface{}, path []string) map[string]interface{} {
	finish := start
	for i := 0; i < len(path); i++ {
		var ok bool
		finish, ok = finish[path[i]].(map[string]interface{})
		if !ok {
			t.Fatalf("key:%s of path:%v not found in map:%v", path[i], path, start)
		}
	}

	return finish
}

func TestRunApplyPrintsValidObjectList(t *testing.T) {
	cmdtesting.InitTestErrorHandler(t)
	configMapList := readConfigMapList(t, filenameCM)
	pathCM := "/namespaces/test/configmaps"

	tf := cmdtesting.NewTestFactory().WithNamespace("test")
	defer tf.Cleanup()

	tf.UnstructuredClient = &fake.RESTClient{
		NegotiatedSerializer: resource.UnstructuredPlusDefaultContentConfig().NegotiatedSerializer,
		Client: fake.CreateHTTPClient(func(req *http.Request) (*http.Response, error) {
			switch p, m := req.URL.Path, req.Method; {
			case strings.HasPrefix(p, pathCM) && m == "GET":
				fallthrough
			case strings.HasPrefix(p, pathCM) && m == "PATCH":
				var body io.ReadCloser

				switch p {
				case pathCM + "/test0":
					body = io.NopCloser(bytes.NewReader(configMapList[0]))
				case pathCM + "/test1":
					body = io.NopCloser(bytes.NewReader(configMapList[1]))
				default:
					t.Errorf("unexpected request to %s", p)
				}

				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: body}, nil
			default:
				t.Fatalf("unexpected request: %#v\n%#v", req.URL, req)
				return nil, nil
			}
		}),
	}
	tf.ClientConfigVal = cmdtesting.DefaultClientConfig()

	ioStreams, _, buf, _ := genericiooptions.NewTestIOStreams()
	cmd := NewCmdApply("kubectl", tf, ioStreams)
	cmd.Flags().Set("filename", filenameCM)
	cmd.Flags().Set("output", "json")
	cmd.Flags().Set("dry-run", "client")
	cmd.Run(cmd, []string{})

	// ensure that returned list can be unmarshaled back into a configmap list
	cmList := corev1.List{}
	if err := runtime.DecodeInto(codec, buf.Bytes(), &cmList); err != nil {
		t.Fatal(err)
	}

	if len(cmList.Items) != 2 {
		t.Fatalf("Expected 2 items in the result; got %d", len(cmList.Items))
	}
	if !strings.Contains(string(cmList.Items[0].Raw), "key1") {
		t.Fatalf("Did not get first ConfigMap at the first position")
	}
	if !strings.Contains(string(cmList.Items[1].Raw), "key2") {
		t.Fatalf("Did not get second ConfigMap at the second position")
	}
}

func TestRunApplyViewLastApplied(t *testing.T) {
	_, rcBytesWithConfig := readReplicationController(t, filenameRCLASTAPPLIED)
	_, rcBytesWithArgs := readReplicationController(t, filenameRCLastAppliedArgs)
	nameRC, rcBytes := readReplicationController(t, filenameRC)
	pathRC := "/namespaces/test/replicationcontrollers/" + nameRC

	tests := []struct {
		name, nameRC, pathRC, filePath, outputFormat, expectedErr, expectedOut, selector string
		args                                                                             []string
		respBytes                                                                        []byte
	}{
		{
			name:         "view with file",
			filePath:     filenameRC,
			outputFormat: "",
			expectedErr:  "",
			expectedOut:  "test: 1234\n",
			selector:     "",
			args:         []string{},
			respBytes:    rcBytesWithConfig,
		},
		{
			name:         "test with file include `%s` in arguments",
			filePath:     filenameRCArgs,
			outputFormat: "",
			expectedErr:  "",
			expectedOut:  "args: -random_flag=%s@domain.com\n",
			selector:     "",
			args:         []string{},
			respBytes:    rcBytesWithArgs,
		},
		{
			name:         "view with file json format",
			filePath:     filenameRC,
			outputFormat: "json",
			expectedErr:  "",
			expectedOut:  "{\n  \"test\": 1234\n}\n",
			selector:     "",
			args:         []string{},
			respBytes:    rcBytesWithConfig,
		},
		{
			name:         "view resource/name invalid format",
			filePath:     "",
			outputFormat: "wide",
			expectedErr:  "error: Unexpected -o output mode: wide, the flag 'output' must be one of yaml|json\nSee 'view-last-applied -h' for help and examples",
			expectedOut:  "",
			selector:     "",
			args:         []string{"replicationcontroller", "test-rc"},
			respBytes:    rcBytesWithConfig,
		},
		{
			name:         "view resource with label",
			filePath:     "",
			outputFormat: "",
			expectedErr:  "",
			expectedOut:  "test: 1234\n",
			selector:     "name=test-rc",
			args:         []string{"replicationcontroller"},
			respBytes:    rcBytesWithConfig,
		},
		{
			name:         "view resource without annotations",
			filePath:     "",
			outputFormat: "",
			expectedErr:  "error: no last-applied-configuration annotation found on resource: test-rc",
			expectedOut:  "",
			selector:     "",
			args:         []string{"replicationcontroller", "test-rc"},
			respBytes:    rcBytes,
		},
		{
			name:         "view resource no match",
			filePath:     "",
			outputFormat: "",
			expectedErr:  "Error from server (NotFound): the server could not find the requested resource (get replicationcontrollers no-match)",
			expectedOut:  "",
			selector:     "",
			args:         []string{"replicationcontroller", "no-match"},
			respBytes:    nil,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tf := cmdtesting.NewTestFactory().WithNamespace("test")
			defer tf.Cleanup()

			tf.UnstructuredClient = &fake.RESTClient{
				GroupVersion:         schema.GroupVersion{Version: "v1"},
				NegotiatedSerializer: resource.UnstructuredPlusDefaultContentConfig().NegotiatedSerializer,
				Client: fake.CreateHTTPClient(func(req *http.Request) (*http.Response, error) {
					switch p, m := req.URL.Path, req.Method; {
					case p == pathRC && m == "GET":
						bodyRC := io.NopCloser(bytes.NewReader(test.respBytes))
						return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: bodyRC}, nil
					case p == "/namespaces/test/replicationcontrollers" && m == "GET":
						bodyRC := io.NopCloser(bytes.NewReader(test.respBytes))
						return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: bodyRC}, nil
					case p == "/namespaces/test/replicationcontrollers/no-match" && m == "GET":
						return &http.Response{StatusCode: http.StatusNotFound, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, &corev1.Pod{})}, nil
					case p == "/api/v1/namespaces/test" && m == "GET":
						return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, &corev1.Namespace{})}, nil
					default:
						t.Fatalf("unexpected request: %#v\n%#v", req.URL, req)
						return nil, nil
					}
				}),
			}
			tf.ClientConfigVal = cmdtesting.DefaultClientConfig()

			cmdutil.BehaviorOnFatal(func(str string, code int) {
				if str != test.expectedErr {
					t.Errorf("%s: unexpected error: %s\nexpected: %s", test.name, str, test.expectedErr)
				}
			})

			ioStreams, _, buf, _ := genericiooptions.NewTestIOStreams()
			cmd := NewCmdApplyViewLastApplied(tf, ioStreams)
			if test.filePath != "" {
				cmd.Flags().Set("filename", test.filePath)
			}
			if test.outputFormat != "" {
				cmd.Flags().Set("output", test.outputFormat)
			}
			if test.selector != "" {
				cmd.Flags().Set("selector", test.selector)
			}

			cmd.Run(cmd, test.args)
			if buf.String() != test.expectedOut {
				t.Fatalf("%s: unexpected output: %s\nexpected: %s", test.name, buf.String(), test.expectedOut)
			}
		})
	}
}

func TestApplyObjectWithoutAnnotation(t *testing.T) {
	cmdtesting.InitTestErrorHandler(t)
	nameRC, rcBytes := readReplicationController(t, filenameRC)
	pathRC := "/namespaces/test/replicationcontrollers/" + nameRC

	tf := cmdtesting.NewTestFactory().WithNamespace("test")
	defer tf.Cleanup()

	tf.UnstructuredClient = &fake.RESTClient{
		NegotiatedSerializer: resource.UnstructuredPlusDefaultContentConfig().NegotiatedSerializer,
		Client: fake.CreateHTTPClient(func(req *http.Request) (*http.Response, error) {
			switch p, m := req.URL.Path, req.Method; {
			case p == pathRC && m == "GET":
				bodyRC := io.NopCloser(bytes.NewReader(rcBytes))
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: bodyRC}, nil
			case p == pathRC && m == "PATCH":
				bodyRC := io.NopCloser(bytes.NewReader(rcBytes))
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: bodyRC}, nil
			default:
				t.Fatalf("unexpected request: %#v\n%#v", req.URL, req)
				return nil, nil
			}
		}),
	}
	tf.ClientConfigVal = cmdtesting.DefaultClientConfig()
	tf.OpenAPIV3ClientFunc = FakeOpenAPISchema.OpenAPIV3ClientFunc

	ioStreams, _, buf, errBuf := genericiooptions.NewTestIOStreams()
	cmd := NewCmdApply("kubectl", tf, ioStreams)
	cmd.Flags().Set("filename", filenameRC)
	cmd.Flags().Set("output", "name")
	cmd.Run(cmd, []string{})

	// uses the name from the file, not the response
	expectRC := "replicationcontroller/" + nameRC + "\n"
	expectWarning := fmt.Sprintf(warningNoLastAppliedConfigAnnotation, "replicationcontrollers/test-rc", corev1.LastAppliedConfigAnnotation, "kubectl")
	if errBuf.String() != expectWarning {
		t.Fatalf("unexpected non-warning: %s\nexpected: %s", errBuf.String(), expectWarning)
	}
	if buf.String() != expectRC {
		t.Fatalf("unexpected output: %s\nexpected: %s", buf.String(), expectRC)
	}
}

func TestOpenAPIV3PatchFeatureFlag(t *testing.T) {
	// OpenAPIV3 smp apply is on by default.
	// Test that users can disable it to use OpenAPI V2 smp
	// An OpenAPI V3 root that always panics is used to ensure
	// the v3 code path is never exercised when the feature is disabled
	cmdtesting.InitTestErrorHandler(t)
	nameRC, currentRC := readAndAnnotateReplicationController(t, filenameRC)
	pathRC := "/namespaces/test/replicationcontrollers/" + nameRC

	t.Run("test apply when a local object is specified - openapi v2 smp", func(t *testing.T) {
		disableOpenAPIV3Patch(t, func(t *testing.T) {
			tf := cmdtesting.NewTestFactory().WithNamespace("test")
			defer tf.Cleanup()

			tf.UnstructuredClient = &fake.RESTClient{
				NegotiatedSerializer: resource.UnstructuredPlusDefaultContentConfig().NegotiatedSerializer,
				Client: fake.CreateHTTPClient(func(req *http.Request) (*http.Response, error) {
					switch p, m := req.URL.Path, req.Method; {
					case p == pathRC && m == "GET":
						bodyRC := io.NopCloser(bytes.NewReader(currentRC))
						return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: bodyRC}, nil
					case p == pathRC && m == "PATCH":
						validatePatchApplication(t, req, types.StrategicMergePatchType)
						bodyRC := io.NopCloser(bytes.NewReader(currentRC))
						return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: bodyRC}, nil
					default:
						t.Fatalf("unexpected request: %#v\n%#v", req.URL, req)
						return nil, nil
					}
				}),
			}
			tf.OpenAPISchemaFunc = FakeOpenAPISchema.OpenAPISchemaFn
			tf.OpenAPIV3ClientFunc = AlwaysPanicSchema.OpenAPIV3ClientFunc
			tf.ClientConfigVal = cmdtesting.DefaultClientConfig()

			ioStreams, _, buf, errBuf := genericiooptions.NewTestIOStreams()
			cmd := NewCmdApply("kubectl", tf, ioStreams)
			cmd.Flags().Set("filename", filenameRC)
			cmd.Flags().Set("output", "name")
			cmd.Run(cmd, []string{})

			// uses the name from the file, not the response
			expectRC := "replicationcontroller/" + nameRC + "\n"
			if buf.String() != expectRC {
				t.Fatalf("unexpected output: %s\nexpected: %s", buf.String(), expectRC)
			}
			if errBuf.String() != "" {
				t.Fatalf("unexpected error output: %s", errBuf.String())
			}
		})
	})

}

func TestOpenAPIV3DoesNotLoadV2(t *testing.T) {
	cmdtesting.InitTestErrorHandler(t)
	nameRC, currentRC := readAndAnnotateReplicationController(t, filenameRC)
	pathRC := "/namespaces/test/replicationcontrollers/" + nameRC

	t.Run("test apply when a local object is specified - openapi v3 smp", func(t *testing.T) {
		tf := cmdtesting.NewTestFactory().WithNamespace("test")
		defer tf.Cleanup()

		tf.UnstructuredClient = &fake.RESTClient{
			NegotiatedSerializer: resource.UnstructuredPlusDefaultContentConfig().NegotiatedSerializer,
			Client: fake.CreateHTTPClient(func(req *http.Request) (*http.Response, error) {
				switch p, m := req.URL.Path, req.Method; {
				case p == pathRC && m == "GET":
					bodyRC := io.NopCloser(bytes.NewReader(currentRC))
					return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: bodyRC}, nil
				case p == pathRC && m == "PATCH":
					validatePatchApplication(t, req, types.StrategicMergePatchType)
					bodyRC := io.NopCloser(bytes.NewReader(currentRC))
					return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: bodyRC}, nil
				default:
					t.Fatalf("unexpected request: %#v\n%#v", req.URL, req)
					return nil, nil
				}
			}),
		}
		tf.OpenAPISchemaFunc = AlwaysPanicSchema.OpenAPISchemaFn
		tf.OpenAPIV3ClientFunc = FakeOpenAPISchema.OpenAPIV3ClientFunc
		tf.ClientConfigVal = cmdtesting.DefaultClientConfig()

		ioStreams, _, buf, errBuf := genericiooptions.NewTestIOStreams()
		cmd := NewCmdApply("kubectl", tf, ioStreams)
		cmd.Flags().Set("filename", filenameRC)
		cmd.Flags().Set("output", "name")
		cmd.Run(cmd, []string{})

		// uses the name from the file, not the response
		expectRC := "replicationcontroller/" + nameRC + "\n"
		if buf.String() != expectRC {
			t.Fatalf("unexpected output: %s\nexpected: %s", buf.String(), expectRC)
		}
		if errBuf.String() != "" {
			t.Fatalf("unexpected error output: %s", errBuf.String())
		}
	})

}

func TestApplyObject(t *testing.T) {
	cmdtesting.InitTestErrorHandler(t)
	nameRC, currentRC := readAndAnnotateReplicationController(t, filenameRC)
	pathRC := "/namespaces/test/replicationcontrollers/" + nameRC

	for _, testingOpenAPISchema := range testingOpenAPISchemas {
		for _, openAPIFeatureToggle := range applyFeatureToggles {
			t.Run("test apply when a local object is specified - openapi v3 smp", func(t *testing.T) {
				openAPIFeatureToggle(t, func(t *testing.T) {
					tf := cmdtesting.NewTestFactory().WithNamespace("test")

					defer tf.Cleanup()

					tf.UnstructuredClient = &fake.RESTClient{
						NegotiatedSerializer: resource.UnstructuredPlusDefaultContentConfig().NegotiatedSerializer,
						Client: fake.CreateHTTPClient(func(req *http.Request) (*http.Response, error) {
							switch p, m := req.URL.Path, req.Method; {
							case p == pathRC && m == "GET":
								bodyRC := io.NopCloser(bytes.NewReader(currentRC))
								return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: bodyRC}, nil
							case p == pathRC && m == "PATCH":
								validatePatchApplication(t, req, types.StrategicMergePatchType)
								bodyRC := io.NopCloser(bytes.NewReader(currentRC))
								return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: bodyRC}, nil
							default:
								t.Fatalf("unexpected request: %#v\n%#v", req.URL, req)
								return nil, nil
							}
						}),
					}
					tf.OpenAPISchemaFunc = testingOpenAPISchema.OpenAPISchemaFn
					tf.OpenAPIV3ClientFunc = testingOpenAPISchema.OpenAPIV3ClientFunc
					tf.ClientConfigVal = cmdtesting.DefaultClientConfig()

					ioStreams, _, buf, errBuf := genericiooptions.NewTestIOStreams()
					cmd := NewCmdApply("kubectl", tf, ioStreams)
					cmd.Flags().Set("filename", filenameRC)
					cmd.Flags().Set("output", "name")
					cmd.Run(cmd, []string{})

					// uses the name from the file, not the response
					expectRC := "replicationcontroller/" + nameRC + "\n"
					if buf.String() != expectRC {
						t.Fatalf("unexpected output: %s\nexpected: %s", buf.String(), expectRC)
					}
					if errBuf.String() != "" {
						t.Fatalf("unexpected error output: %s", errBuf.String())
					}
				})
			})
		}
	}
}

func TestApplyPruneObjects(t *testing.T) {
	cmdtesting.InitTestErrorHandler(t)
	nameRC, currentRC := readAndAnnotateReplicationController(t, filenameRC)
	pathRC := "/namespaces/test/replicationcontrollers/" + nameRC

	for _, testingOpenAPISchema := range testingOpenAPISchemas {
		for _, openAPIFeatureToggle := range applyFeatureToggles {

			t.Run("test apply returns correct output", func(t *testing.T) {
				openAPIFeatureToggle(t, func(t *testing.T) {
					tf := cmdtesting.NewTestFactory().WithNamespace("test")
					defer tf.Cleanup()

					tf.UnstructuredClient = &fake.RESTClient{
						NegotiatedSerializer: resource.UnstructuredPlusDefaultContentConfig().NegotiatedSerializer,
						Client: fake.CreateHTTPClient(func(req *http.Request) (*http.Response, error) {
							switch p, m := req.URL.Path, req.Method; {
							case p == pathRC && m == "GET":
								bodyRC := io.NopCloser(bytes.NewReader(currentRC))
								return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: bodyRC}, nil
							case p == pathRC && m == "PATCH":
								validatePatchApplication(t, req, types.StrategicMergePatchType)
								bodyRC := io.NopCloser(bytes.NewReader(currentRC))
								return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: bodyRC}, nil
							default:
								t.Fatalf("unexpected request: %#v\n%#v", req.URL, req)
								return nil, nil
							}
						}),
					}
					tf.OpenAPISchemaFunc = testingOpenAPISchema.OpenAPISchemaFn
					tf.OpenAPIV3ClientFunc = testingOpenAPISchema.OpenAPIV3ClientFunc
					tf.ClientConfigVal = cmdtesting.DefaultClientConfig()

					ioStreams, _, buf, errBuf := genericiooptions.NewTestIOStreams()
					cmd := NewCmdApply("kubectl", tf, ioStreams)
					cmd.Flags().Set("filename", filenameRC)
					cmd.Flags().Set("prune", "true")
					cmd.Flags().Set("namespace", "test")
					cmd.Flags().Set("output", "yaml")
					cmd.Flags().Set("all", "true")
					cmd.Run(cmd, []string{})

					if !strings.Contains(buf.String(), "test-rc") {
						t.Fatalf("unexpected output: %s\nexpected to contain: %s", buf.String(), "test-rc")
					}
					if errBuf.String() != "" {
						t.Fatalf("unexpected error output: %s", errBuf.String())
					}
				})
			})
		}
	}
}

func TestApplyPruneObjectsWithAllowlist(t *testing.T) {
	cmdtesting.InitTestErrorHandler(t)

	// Read ReplicationController from the file we will use to apply. This one will not be pruned because it exists in the file.
	rc := readUnstructuredFromFile(t, filenameRC)
	err := setLastAppliedConfigAnnotation(rc)
	if err != nil {
		t.Fatal(err)
	}

	// Create another ReplicationController that can be pruned
	rc2 := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"kind":       "ReplicationController",
			"apiVersion": "v1",
			"metadata": map[string]interface{}{
				"name":      "test-rc2",
				"namespace": "test",
				"uid":       "uid-rc2",
			},
		},
	}
	err = setLastAppliedConfigAnnotation(rc2)
	if err != nil {
		t.Fatal(err)
	}

	// Create a ConfigMap that can be pruned
	cm := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"kind":       "ConfigMap",
			"apiVersion": "v1",
			"metadata": map[string]interface{}{
				"name":      "test-cm",
				"namespace": "test",
				"uid":       "uid-cm",
			},
		},
	}
	err = setLastAppliedConfigAnnotation(cm)
	if err != nil {
		t.Fatal(err)
	}

	// Create Namespace that can be pruned
	ns := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"kind":       "Namespace",
			"apiVersion": "v1",
			"metadata": map[string]interface{}{
				"name": "test-apply",
				"uid":  "uid-ns",
			},
		},
	}
	err = setLastAppliedConfigAnnotation(ns)
	if err != nil {
		t.Fatal(err)
	}

	// Create a ConfigMap without a UID. Resources without a UID will not be pruned.
	cmNoUID := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"kind":       "ConfigMap",
			"apiVersion": "v1",
			"metadata": map[string]interface{}{
				"name":      "test-cm-nouid",
				"namespace": "test",
			},
		},
	}
	err = setLastAppliedConfigAnnotation(cmNoUID)
	if err != nil {
		t.Fatal(err)
	}

	// Create a ConfigMap without a last applied annotation. Resources without a last applied annotation will not be pruned.
	cmNoLastApplied := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"kind":       "ConfigMap",
			"apiVersion": "v1",
			"metadata": map[string]interface{}{
				"name":      "test-cm-nolastapplied",
				"namespace": "test",
				"uid":       "uid-cm-nolastapplied",
			},
		},
	}

	testCases := map[string]struct {
		currentResources        []runtime.Object
		pruneAllowlist          []string
		namespace               string
		expectedPrunedResources []string
		expectedOutputs         []string
	}{
		"prune without namespace and allowlist should delete resources that are not in the specified file": {
			currentResources:        []runtime.Object{rc, rc2, cm, ns},
			expectedPrunedResources: []string{"test/test-cm", "test/test-rc2", "/test-apply"},
			expectedOutputs: []string{
				"replicationcontroller/test-rc unchanged",
				"configmap/test-cm pruned",
				"replicationcontroller/test-rc2 pruned",
				"namespace/test-apply pruned",
			},
		},
		"prune with namespace and without allowlist should delete resources that are not in the specified file": {
			currentResources:        []runtime.Object{rc, rc2, cm, ns},
			namespace:               "test",
			expectedPrunedResources: []string{"test/test-cm", "test/test-rc2"},
			expectedOutputs: []string{
				"replicationcontroller/test-rc unchanged",
				"configmap/test-cm pruned",
				"replicationcontroller/test-rc2 pruned",
			},
		},
		// Even namespace is a non-namespaced resource, it will be pruned if specified in pruneAllowList in the future
		"prune with namespace and allowlist should delete all matching resources": {
			currentResources:        []runtime.Object{rc, cm, ns},
			pruneAllowlist:          []string{"core/v1/ConfigMap", "core/v1/Namespace"},
			namespace:               "test",
			expectedPrunedResources: []string{"test/test-cm", "/test-apply"},
			expectedOutputs: []string{
				"replicationcontroller/test-rc unchanged",
				"configmap/test-cm pruned",
				"namespace/test-apply pruned",
			},
		},
		"prune with allowlist should delete only matching resources": {
			currentResources:        []runtime.Object{rc, rc2, cm},
			pruneAllowlist:          []string{"core/v1/ConfigMap"},
			namespace:               "test",
			expectedPrunedResources: []string{"test/test-cm"},
			expectedOutputs: []string{
				"replicationcontroller/test-rc unchanged",
				"configmap/test-cm pruned",
			},
		},
		"prune with allowlist specifying the same resource type multiple times should not fail": {
			currentResources:        []runtime.Object{rc, rc2, cm},
			pruneAllowlist:          []string{"core/v1/ConfigMap", "core/v1/ConfigMap"},
			namespace:               "test",
			expectedPrunedResources: []string{"test/test-cm"},
			expectedOutputs: []string{
				"replicationcontroller/test-rc unchanged",
				"configmap/test-cm pruned",
			},
		},
		"prune with allowlist should not delete resources that exist in the specified file": {
			currentResources:        []runtime.Object{rc, rc2, cm},
			pruneAllowlist:          []string{"core/v1/ReplicationController"},
			namespace:               "test",
			expectedPrunedResources: []string{"test/test-rc2"},
			expectedOutputs: []string{
				"replicationcontroller/test-rc unchanged",
				"replicationcontroller/test-rc2 pruned",
			},
		},
		"prune with allowlist specifying multiple resource types should delete matching resources": {
			currentResources:        []runtime.Object{rc, rc2, cm},
			pruneAllowlist:          []string{"core/v1/ConfigMap", "core/v1/ReplicationController"},
			namespace:               "test",
			expectedPrunedResources: []string{"test/test-cm", "test/test-rc2"},
			expectedOutputs: []string{
				"replicationcontroller/test-rc unchanged",
				"configmap/test-cm pruned",
				"replicationcontroller/test-rc2 pruned",
			},
		},
		"prune should not delete resources that are missing a UID": {
			currentResources:        []runtime.Object{rc, cm, cmNoUID},
			expectedPrunedResources: []string{"test/test-cm"},
			expectedOutputs: []string{
				"replicationcontroller/test-rc unchanged",
				"configmap/test-cm pruned",
			},
		},
		"prune should not delete resources that are missing the last applied config annotation": {
			currentResources:        []runtime.Object{rc, cm, cmNoLastApplied},
			expectedPrunedResources: []string{"test/test-cm"},
			expectedOutputs: []string{
				"replicationcontroller/test-rc unchanged",
				"configmap/test-cm pruned",
			},
		},
	}

	for testCaseName, tc := range testCases {
		for _, testingOpenAPISchema := range testingOpenAPISchemas {
			t.Run(testCaseName, func(t *testing.T) {
				tf := cmdtesting.NewTestFactory().WithNamespace(tc.namespace)
				defer tf.Cleanup()

				tf.UnstructuredClient = &fake.RESTClient{
					NegotiatedSerializer: resource.UnstructuredPlusDefaultContentConfig().NegotiatedSerializer,
					Client: fake.CreateHTTPClient(func(req *http.Request) (*http.Response, error) {
						switch p, m := req.URL.Path, req.Method; {
						case p == "/namespaces/test/replicationcontrollers/test-rc" && m == "GET":
							encoded := runtime.EncodeOrDie(unstructured.UnstructuredJSONScheme, rc)
							bodyRC := io.NopCloser(strings.NewReader(encoded))
							return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: bodyRC}, nil
						case p == "/namespaces/test/replicationcontrollers/test-rc" && m == "PATCH":
							encoded := runtime.EncodeOrDie(unstructured.UnstructuredJSONScheme, rc)
							bodyRC := io.NopCloser(strings.NewReader(encoded))
							return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: bodyRC}, nil
						default:
							t.Fatalf("unexpected request: %#v\n%#v", req.URL, req)
							return nil, nil
						}
					}),
				}
				tf.OpenAPISchemaFunc = testingOpenAPISchema.OpenAPISchemaFn
				tf.OpenAPIV3ClientFunc = testingOpenAPISchema.OpenAPIV3ClientFunc
				tf.ClientConfigVal = cmdtesting.DefaultClientConfig()

				for _, resource := range tc.currentResources {
					if err := tf.FakeDynamicClient.Tracker().Add(resource); err != nil {
						t.Fatal(err)
					}
				}

				ioStreams, _, buf, errBuf := genericiooptions.NewTestIOStreams()
				cmd := NewCmdApply("kubectl", tf, ioStreams)
				cmd.Flags().Set("filename", filenameRC)
				cmd.Flags().Set("prune", "true")
				cmd.Flags().Set("all", "true")
				for _, allow := range tc.pruneAllowlist {
					cmd.Flags().Set("prune-allowlist", allow)
				}
				cmd.Run(cmd, []string{})

				if errBuf.String() != "" {
					t.Fatalf("unexpected error output: %s", errBuf.String())
				}

				actualOutput := buf.String()
				for _, expectedOutput := range tc.expectedOutputs {
					if !strings.Contains(actualOutput, expectedOutput) {
						t.Fatalf("expected output to contain %q, but it did not. Actual Output:\n%s", expectedOutput, actualOutput)
					}
				}

				var prunedResources []string
				for _, action := range tf.FakeDynamicClient.Actions() {
					if action.GetVerb() == "delete" {
						deleteAction := action.(testing2.DeleteAction)
						prunedResources = append(prunedResources, deleteAction.GetNamespace()+"/"+deleteAction.GetName())
					}
				}

				// Make sure nothing unexpected was pruned
				for _, resource := range prunedResources {
					if !slices.Contains(tc.expectedPrunedResources, resource) {
						t.Fatalf("expected %s not to be pruned, but it was", resource)
					}
				}

				// Make sure everything that was expected to be pruned was pruned
				for _, resource := range tc.expectedPrunedResources {
					if !slices.Contains(prunedResources, resource) {
						t.Fatalf("expected %s to be pruned, but it was not", resource)
					}
				}

			})
		}
	}
}

func setLastAppliedConfigAnnotation(obj runtime.Object) error {
	accessor, err := meta.Accessor(obj)
	if err != nil {
		return err
	}
	annotations := accessor.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string)
		accessor.SetAnnotations(annotations)
	}
	annotations[corev1.LastAppliedConfigAnnotation] = runtime.EncodeOrDie(unstructured.NewJSONFallbackEncoder(codec), obj)
	accessor.SetAnnotations(annotations)
	return nil
}

// Tests that apply of object in need of CSA migration results in a call
// to patch it.
func TestApplyCSAMigration(t *testing.T) {
	cmdtesting.InitTestErrorHandler(t)
	nameRC, rcWithManagedFields := readAndAnnotateReplicationController(t, filenameRCManagedFieldsLA)
	pathRC := "/namespaces/test/replicationcontrollers/" + nameRC

	for _, openAPIFeatureToggle := range applyFeatureToggles {
		openAPIFeatureToggle(t, func(t *testing.T) {
			tf := cmdtesting.NewTestFactory().WithNamespace("test")
			defer tf.Cleanup()

			// The object after patch should be equivalent to the output of
			// csaupgrade.UpgradeManagedFields
			//
			// Parse object into unstructured, apply patch
			postPatchObj := &unstructured.Unstructured{}
			err := json.Unmarshal(rcWithManagedFields, &postPatchObj.Object)
			require.NoError(t, err)

			expectedPatch, err := csaupgrade.UpgradeManagedFieldsPatch(postPatchObj, sets.New(FieldManagerClientSideApply), "kubectl")
			require.NoError(t, err)

			err = csaupgrade.UpgradeManagedFields(postPatchObj, sets.New("kubectl-client-side-apply"), "kubectl")
			require.NoError(t, err)

			postPatchData, err := json.Marshal(postPatchObj)
			require.NoError(t, err)

			patches := 0
			targetPatches := 2
			applies := 0

			tf.UnstructuredClient = &fake.RESTClient{
				NegotiatedSerializer: resource.UnstructuredPlusDefaultContentConfig().NegotiatedSerializer,
				Client: fake.CreateHTTPClient(func(req *http.Request) (*http.Response, error) {
					switch p, m := req.URL.Path, req.Method; {
					case p == pathRC && m == "GET":
						// During retry loop for patch fetch is performed.
						// keep returning the unchanged data
						if patches < targetPatches {
							bodyRC := io.NopCloser(bytes.NewReader(rcWithManagedFields))
							return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: bodyRC}, nil
						}

						t.Fatalf("should not do a fetch in serverside-apply")
						return nil, nil
					case p == pathRC && m == "PATCH":
						if got := req.Header.Get("Content-Type"); got == string(types.ApplyPatchType) {
							defer func() {
								applies += 1
							}()

							switch applies {
							case 0:
								// initial apply.
								// Just return the same object but with managed fields
								bodyRC := io.NopCloser(bytes.NewReader(rcWithManagedFields))
								return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: bodyRC}, nil
							case 1:
								// Second apply should include only last apply annotation unmodified
								// Return new object
								// NOTE: on a real server this would also modify the managed fields
								// just return the same object unmodified. It is not so important
								// for this test for the last-applied to appear in new field
								// manager response, only that the client asks the server to do it
								bodyRC := io.NopCloser(bytes.NewReader(rcWithManagedFields))
								return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: bodyRC}, nil
							case 2, 3:
								// Before the last apply we have formed our JSONPAtch so it
								// should reply now with the upgraded object
								bodyRC := io.NopCloser(bytes.NewReader(postPatchData))
								return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: bodyRC}, nil
							default:
								require.Fail(t, "sent more apply requests than expected")
								return &http.Response{StatusCode: http.StatusBadRequest, Header: cmdtesting.DefaultHeader()}, nil
							}
						} else if got == string(types.JSONPatchType) {
							defer func() {
								patches += 1
							}()

							// Require that the patch is equal to what is expected
							body, err := io.ReadAll(req.Body)
							require.NoError(t, err)
							require.Equal(t, expectedPatch, body)

							switch patches {
							case targetPatches - 1:
								bodyRC := io.NopCloser(bytes.NewReader(postPatchData))
								return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: bodyRC}, nil
							default:
								// Return conflict until the client has retried enough times
								return &http.Response{StatusCode: http.StatusConflict, Header: cmdtesting.DefaultHeader()}, nil

							}
						} else {
							t.Fatalf("unexpected content-type: %s\n", got)
							return nil, nil
						}

					default:
						t.Fatalf("unexpected request: %#v\n%#v", req.URL, req)
						return nil, nil
					}
				}),
			}
			tf.OpenAPISchemaFunc = FakeOpenAPISchema.OpenAPISchemaFn
			tf.OpenAPIV3ClientFunc = FakeOpenAPISchema.OpenAPIV3ClientFunc
			tf.ClientConfigVal = cmdtesting.DefaultClientConfig()

			ioStreams, _, buf, errBuf := genericiooptions.NewTestIOStreams()
			cmd := NewCmdApply("kubectl", tf, ioStreams)
			cmd.Flags().Set("filename", filenameRC)
			cmd.Flags().Set("output", "yaml")
			cmd.Flags().Set("server-side", "true")
			cmd.Flags().Set("show-managed-fields", "true")
			cmd.Run(cmd, []string{})

			// JSONPatch should have been attempted exactly the given number of times
			require.Equal(t, targetPatches, patches, "should retry as many times as a conflict was returned")
			require.Equal(t, 3, applies, "should perform specified # of apply calls upon migration")
			require.Empty(t, errBuf.String())

			// ensure that in the future there will be no migrations necessary
			// (by showing migration is a no-op)

			rc := &corev1.ReplicationController{}
			if err := runtime.DecodeInto(codec, buf.Bytes(), rc); err != nil {
				t.Fatal(err)
			}

			upgradedRC := rc.DeepCopyObject()
			err = csaupgrade.UpgradeManagedFields(upgradedRC, sets.New("kubectl-client-side-apply"), "kubectl")
			require.NoError(t, err)
			require.NotEmpty(t, rc.ManagedFields)
			require.Equal(t, rc, upgradedRC, "upgrading should be no-op in future")

			// Apply the upgraded object.
			// Expect only a single PATCH call to apiserver
			ioStreams, _, _, errBuf = genericiooptions.NewTestIOStreams()
			cmd = NewCmdApply("kubectl", tf, ioStreams)
			cmd.Flags().Set("filename", filenameRC)
			cmd.Flags().Set("output", "yaml")
			cmd.Flags().Set("server-side", "true")
			cmd.Flags().Set("show-managed-fields", "true")
			cmd.Run(cmd, []string{})

			require.Empty(t, errBuf)
			require.Equal(t, 4, applies, "only a single call to server-side apply should have been performed")
			require.Equal(t, targetPatches, patches, "no more json patches should have been needed")
		})
	}
}

func TestApplyObjectOutput(t *testing.T) {
	cmdtesting.InitTestErrorHandler(t)
	nameRC, currentRC := readAndAnnotateReplicationController(t, filenameRC)
	pathRC := "/namespaces/test/replicationcontrollers/" + nameRC

	// Add some extra data to the post-patch object
	postPatchObj := &unstructured.Unstructured{}
	if err := json.Unmarshal(currentRC, &postPatchObj.Object); err != nil {
		t.Fatal(err)
	}
	postPatchLabels := postPatchObj.GetLabels()
	if postPatchLabels == nil {
		postPatchLabels = map[string]string{}
	}
	postPatchLabels["post-patch"] = "value"
	postPatchObj.SetLabels(postPatchLabels)
	postPatchData, err := json.Marshal(postPatchObj)
	if err != nil {
		t.Fatal(err)
	}

	for _, testingOpenAPISchema := range testingOpenAPISchemas {
		for _, openAPIFeatureToggle := range applyFeatureToggles {
			t.Run("test apply returns correct output", func(t *testing.T) {
				openAPIFeatureToggle(t, func(t *testing.T) {
					tf := cmdtesting.NewTestFactory().WithNamespace("test")
					defer tf.Cleanup()

					tf.UnstructuredClient = &fake.RESTClient{
						NegotiatedSerializer: resource.UnstructuredPlusDefaultContentConfig().NegotiatedSerializer,
						Client: fake.CreateHTTPClient(func(req *http.Request) (*http.Response, error) {
							switch p, m := req.URL.Path, req.Method; {
							case p == pathRC && m == "GET":
								bodyRC := io.NopCloser(bytes.NewReader(currentRC))
								return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: bodyRC}, nil
							case p == pathRC && m == "PATCH":
								validatePatchApplication(t, req, types.StrategicMergePatchType)
								bodyRC := io.NopCloser(bytes.NewReader(postPatchData))
								return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: bodyRC}, nil
							default:
								t.Fatalf("unexpected request: %#v\n%#v", req.URL, req)
								return nil, nil
							}
						}),
					}
					tf.OpenAPISchemaFunc = testingOpenAPISchema.OpenAPISchemaFn
					tf.OpenAPIV3ClientFunc = testingOpenAPISchema.OpenAPIV3ClientFunc
					tf.ClientConfigVal = cmdtesting.DefaultClientConfig()

					ioStreams, _, buf, errBuf := genericiooptions.NewTestIOStreams()
					cmd := NewCmdApply("kubectl", tf, ioStreams)
					cmd.Flags().Set("filename", filenameRC)
					cmd.Flags().Set("output", "yaml")
					cmd.Run(cmd, []string{})

					if !strings.Contains(buf.String(), "test-rc") {
						t.Fatalf("unexpected output: %s\nexpected to contain: %s", buf.String(), "test-rc")
					}
					if !strings.Contains(buf.String(), "post-patch: value") {
						t.Fatalf("unexpected output: %s\nexpected to contain: %s", buf.String(), "post-patch: value")
					}
					if errBuf.String() != "" {
						t.Fatalf("unexpected error output: %s", errBuf.String())
					}
				})
			})
		}
	}
}

func TestApplyRetry(t *testing.T) {
	cmdtesting.InitTestErrorHandler(t)
	nameRC, currentRC := readAndAnnotateReplicationController(t, filenameRC)
	pathRC := "/namespaces/test/replicationcontrollers/" + nameRC

	for _, testingOpenAPISchema := range testingOpenAPISchemas {
		for _, openAPIFeatureToggle := range applyFeatureToggles {

			t.Run("test apply retries on conflict error", func(t *testing.T) {
				openAPIFeatureToggle(t, func(t *testing.T) {
					firstPatch := true
					retry := false
					getCount := 0
					tf := cmdtesting.NewTestFactory().WithNamespace("test")
					defer tf.Cleanup()

					tf.UnstructuredClient = &fake.RESTClient{
						NegotiatedSerializer: resource.UnstructuredPlusDefaultContentConfig().NegotiatedSerializer,
						Client: fake.CreateHTTPClient(func(req *http.Request) (*http.Response, error) {
							switch p, m := req.URL.Path, req.Method; {
							case p == pathRC && m == "GET":
								getCount++
								bodyRC := io.NopCloser(bytes.NewReader(currentRC))
								return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: bodyRC}, nil
							case p == pathRC && m == "PATCH":
								if firstPatch {
									firstPatch = false
									statusErr := apierrors.NewConflict(schema.GroupResource{Group: "", Resource: "rc"}, "test-rc", fmt.Errorf("the object has been modified. Please apply at first"))
									bodyBytes, _ := json.Marshal(statusErr)
									bodyErr := io.NopCloser(bytes.NewReader(bodyBytes))
									return &http.Response{StatusCode: http.StatusConflict, Header: cmdtesting.DefaultHeader(), Body: bodyErr}, nil
								}
								retry = true
								validatePatchApplication(t, req, types.StrategicMergePatchType)
								bodyRC := io.NopCloser(bytes.NewReader(currentRC))
								return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: bodyRC}, nil
							default:
								t.Fatalf("unexpected request: %#v\n%#v", req.URL, req)
								return nil, nil
							}
						}),
					}
					tf.OpenAPISchemaFunc = testingOpenAPISchema.OpenAPISchemaFn
					tf.OpenAPIV3ClientFunc = testingOpenAPISchema.OpenAPIV3ClientFunc
					tf.ClientConfigVal = cmdtesting.DefaultClientConfig()

					ioStreams, _, buf, errBuf := genericiooptions.NewTestIOStreams()
					cmd := NewCmdApply("kubectl", tf, ioStreams)
					cmd.Flags().Set("filename", filenameRC)
					cmd.Flags().Set("output", "name")
					cmd.Run(cmd, []string{})

					if !retry || getCount != 2 {
						t.Fatalf("apply didn't retry when get conflict error")
					}

					// uses the name from the file, not the response
					expectRC := "replicationcontroller/" + nameRC + "\n"
					if buf.String() != expectRC {
						t.Fatalf("unexpected output: %s\nexpected: %s", buf.String(), expectRC)
					}
					if errBuf.String() != "" {
						t.Fatalf("unexpected error output: %s", errBuf.String())
					}
				})
			})
		}
	}
}

func TestApplyNonExistObject(t *testing.T) {
	nameRC, currentRC := readAndAnnotateReplicationController(t, filenameRC)
	pathRC := "/namespaces/test/replicationcontrollers"
	pathNameRC := pathRC + "/" + nameRC

	tf := cmdtesting.NewTestFactory().WithNamespace("test")
	defer tf.Cleanup()

	tf.UnstructuredClient = &fake.RESTClient{
		NegotiatedSerializer: resource.UnstructuredPlusDefaultContentConfig().NegotiatedSerializer,
		Client: fake.CreateHTTPClient(func(req *http.Request) (*http.Response, error) {
			switch p, m := req.URL.Path, req.Method; {
			case p == "/api/v1/namespaces/test" && m == "GET":
				return &http.Response{StatusCode: http.StatusNotFound, Header: cmdtesting.DefaultHeader(), Body: io.NopCloser(bytes.NewReader(nil))}, nil
			case p == pathNameRC && m == "GET":
				return &http.Response{StatusCode: http.StatusNotFound, Header: cmdtesting.DefaultHeader(), Body: io.NopCloser(bytes.NewReader(nil))}, nil
			case p == pathRC && m == "POST":
				bodyRC := io.NopCloser(bytes.NewReader(currentRC))
				return &http.Response{StatusCode: http.StatusCreated, Header: cmdtesting.DefaultHeader(), Body: bodyRC}, nil
			default:
				t.Fatalf("unexpected request: %#v\n%#v", req.URL, req)
				return nil, nil
			}
		}),
	}
	tf.ClientConfigVal = cmdtesting.DefaultClientConfig()

	ioStreams, _, buf, _ := genericiooptions.NewTestIOStreams()
	cmd := NewCmdApply("kubectl", tf, ioStreams)
	cmd.Flags().Set("filename", filenameRC)
	cmd.Flags().Set("output", "name")
	cmd.Run(cmd, []string{})

	// uses the name from the file, not the response
	expectRC := "replicationcontroller/" + nameRC + "\n"
	if buf.String() != expectRC {
		t.Errorf("unexpected output: %s\nexpected: %s", buf.String(), expectRC)
	}
}

func TestApplyEmptyPatch(t *testing.T) {
	cmdtesting.InitTestErrorHandler(t)
	nameRC, _ := readAndAnnotateReplicationController(t, filenameRC)
	pathRC := "/namespaces/test/replicationcontrollers"
	pathNameRC := pathRC + "/" + nameRC

	verifyPost := false

	var body []byte

	tf := cmdtesting.NewTestFactory().WithNamespace("test")
	defer tf.Cleanup()

	tf.UnstructuredClient = &fake.RESTClient{
		GroupVersion:         schema.GroupVersion{Version: "v1"},
		NegotiatedSerializer: resource.UnstructuredPlusDefaultContentConfig().NegotiatedSerializer,
		Client: fake.CreateHTTPClient(func(req *http.Request) (*http.Response, error) {
			switch p, m := req.URL.Path, req.Method; {
			case p == "/api/v1/namespaces/test" && m == "GET":
				return &http.Response{StatusCode: http.StatusNotFound, Header: cmdtesting.DefaultHeader(), Body: io.NopCloser(bytes.NewReader(nil))}, nil
			case p == pathNameRC && m == "GET":
				if body == nil {
					return &http.Response{StatusCode: http.StatusNotFound, Header: cmdtesting.DefaultHeader(), Body: io.NopCloser(bytes.NewReader(nil))}, nil
				}
				bodyRC := io.NopCloser(bytes.NewReader(body))
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: bodyRC}, nil
			case p == pathRC && m == "POST":
				body, _ = io.ReadAll(req.Body)
				verifyPost = true
				bodyRC := io.NopCloser(bytes.NewReader(body))
				return &http.Response{StatusCode: http.StatusCreated, Header: cmdtesting.DefaultHeader(), Body: bodyRC}, nil
			default:
				t.Fatalf("unexpected request: %#v\n%#v", req.URL, req)
				return nil, nil
			}
		}),
	}
	tf.ClientConfigVal = cmdtesting.DefaultClientConfig()

	// 1. apply non exist object
	ioStreams, _, buf, _ := genericiooptions.NewTestIOStreams()
	cmd := NewCmdApply("kubectl", tf, ioStreams)
	cmd.Flags().Set("filename", filenameRC)
	cmd.Flags().Set("output", "name")
	cmd.Run(cmd, []string{})

	expectRC := "replicationcontroller/" + nameRC + "\n"
	if buf.String() != expectRC {
		t.Fatalf("unexpected output: %s\nexpected: %s", buf.String(), expectRC)
	}
	if !verifyPost {
		t.Fatal("No server-side post call detected")
	}

	// 2. test apply already exist object, will not send empty patch request
	ioStreams, _, buf, _ = genericiooptions.NewTestIOStreams()
	cmd = NewCmdApply("kubectl", tf, ioStreams)
	cmd.Flags().Set("filename", filenameRC)
	cmd.Flags().Set("output", "name")
	cmd.Run(cmd, []string{})

	if buf.String() != expectRC {
		t.Fatalf("unexpected output: %s\nexpected: %s", buf.String(), expectRC)
	}
}

func TestApplyMultipleObjectsAsList(t *testing.T) {
	testApplyMultipleObjects(t, true)
}

func TestApplyMultipleObjectsAsFiles(t *testing.T) {
	testApplyMultipleObjects(t, false)
}

func testApplyMultipleObjects(t *testing.T, asList bool) {
	nameRC, currentRC := readAndAnnotateReplicationController(t, filenameRC)
	pathRC := "/namespaces/test/replicationcontrollers/" + nameRC

	nameSVC, currentSVC := readAndAnnotateService(t, filenameSVC)
	pathSVC := "/namespaces/test/services/" + nameSVC

	for _, testingOpenAPISchema := range testingOpenAPISchemas {
		t.Run("test apply on multiple objects", func(t *testing.T) {
			tf := cmdtesting.NewTestFactory().WithNamespace("test")
			defer tf.Cleanup()

			tf.UnstructuredClient = &fake.RESTClient{
				NegotiatedSerializer: resource.UnstructuredPlusDefaultContentConfig().NegotiatedSerializer,
				Client: fake.CreateHTTPClient(func(req *http.Request) (*http.Response, error) {
					switch p, m := req.URL.Path, req.Method; {
					case p == pathRC && m == "GET":
						bodyRC := io.NopCloser(bytes.NewReader(currentRC))
						return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: bodyRC}, nil
					case p == pathRC && m == "PATCH":
						validatePatchApplication(t, req, types.StrategicMergePatchType)
						bodyRC := io.NopCloser(bytes.NewReader(currentRC))
						return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: bodyRC}, nil
					case p == pathSVC && m == "GET":
						bodySVC := io.NopCloser(bytes.NewReader(currentSVC))
						return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: bodySVC}, nil
					case p == pathSVC && m == "PATCH":
						validatePatchApplication(t, req, types.StrategicMergePatchType)
						bodySVC := io.NopCloser(bytes.NewReader(currentSVC))
						return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: bodySVC}, nil
					default:
						t.Fatalf("unexpected request: %#v\n%#v", req.URL, req)
						return nil, nil
					}
				}),
			}
			tf.OpenAPISchemaFunc = testingOpenAPISchema.OpenAPISchemaFn
			tf.OpenAPIV3ClientFunc = testingOpenAPISchema.OpenAPIV3ClientFunc
			tf.ClientConfigVal = cmdtesting.DefaultClientConfig()

			ioStreams, _, buf, errBuf := genericiooptions.NewTestIOStreams()
			cmd := NewCmdApply("kubectl", tf, ioStreams)
			if asList {
				cmd.Flags().Set("filename", filenameRCSVC)
			} else {
				cmd.Flags().Set("filename", filenameRC)
				cmd.Flags().Set("filename", filenameSVC)
			}
			cmd.Flags().Set("output", "name")

			cmd.Run(cmd, []string{})

			// Names should come from the REST response, NOT the files
			expectRC := "replicationcontroller/" + nameRC + "\n"
			expectSVC := "service/" + nameSVC + "\n"
			// Test both possible orders since output is non-deterministic.
			expectOne := expectRC + expectSVC
			expectTwo := expectSVC + expectRC
			if buf.String() != expectOne && buf.String() != expectTwo {
				t.Fatalf("unexpected output: %s\nexpected: %s OR %s", buf.String(), expectOne, expectTwo)
			}
			if errBuf.String() != "" {
				t.Fatalf("unexpected error output: %s", errBuf.String())
			}
		})
	}
}

func readDeploymentFromFile(t *testing.T, file string) []byte {
	raw := readBytesFromFile(t, file)
	obj := &appsv1.Deployment{}
	if err := runtime.DecodeInto(codec, raw, obj); err != nil {
		t.Fatal(err)
	}
	objJSON, err := runtime.Encode(codec, obj)
	if err != nil {
		t.Fatal(err)
	}
	return objJSON
}

func TestApplyNULLPreservation(t *testing.T) {
	cmdtesting.InitTestErrorHandler(t)
	deploymentName := "nginx-deployment"
	deploymentPath := "/namespaces/test/deployments/" + deploymentName

	verifiedPatch := false
	deploymentBytes := readDeploymentFromFile(t, filenameDeployObjServerside)

	for _, testingOpenAPISchema := range testingOpenAPISchemas {
		for _, openAPIFeatureToggle := range applyFeatureToggles {

			t.Run("test apply preserves NULL fields", func(t *testing.T) {
				openAPIFeatureToggle(t, func(t *testing.T) {
					tf := cmdtesting.NewTestFactory().WithNamespace("test")
					defer tf.Cleanup()

					tf.UnstructuredClient = &fake.RESTClient{
						NegotiatedSerializer: resource.UnstructuredPlusDefaultContentConfig().NegotiatedSerializer,
						Client: fake.CreateHTTPClient(func(req *http.Request) (*http.Response, error) {
							switch p, m := req.URL.Path, req.Method; {
							case p == deploymentPath && m == "GET":
								body := io.NopCloser(bytes.NewReader(deploymentBytes))
								return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: body}, nil
							case p == deploymentPath && m == "PATCH":
								patch, err := io.ReadAll(req.Body)
								if err != nil {
									t.Fatal(err)
								}

								patchMap := map[string]interface{}{}
								if err := json.Unmarshal(patch, &patchMap); err != nil {
									t.Fatal(err)
								}
								annotationMap := walkMapPath(t, patchMap, []string{"metadata", "annotations"})
								if _, ok := annotationMap[corev1.LastAppliedConfigAnnotation]; !ok {
									t.Fatalf("patch does not contain annotation:\n%s\n", patch)
								}
								strategy := walkMapPath(t, patchMap, []string{"spec", "strategy"})
								if value, ok := strategy["rollingUpdate"]; !ok || value != nil {
									t.Fatalf("patch did not retain null value in key: rollingUpdate:\n%s\n", patch)
								}
								verifiedPatch = true

								// The real API server would had returned the patched object but Kubectl
								// is ignoring the actual return object.
								// TODO: Make this match actual server behavior by returning the patched object.
								body := io.NopCloser(bytes.NewReader(deploymentBytes))
								return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: body}, nil
							default:
								t.Fatalf("unexpected request: %#v\n%#v", req.URL, req)
								return nil, nil
							}
						}),
					}
					tf.OpenAPISchemaFunc = testingOpenAPISchema.OpenAPISchemaFn
					tf.OpenAPIV3ClientFunc = testingOpenAPISchema.OpenAPIV3ClientFunc
					tf.ClientConfigVal = cmdtesting.DefaultClientConfig()

					ioStreams, _, buf, errBuf := genericiooptions.NewTestIOStreams()
					cmd := NewCmdApply("kubectl", tf, ioStreams)
					cmd.Flags().Set("filename", filenameDeployObjClientside)
					cmd.Flags().Set("output", "name")

					cmd.Run(cmd, []string{})

					expected := "deployment.apps/" + deploymentName + "\n"
					if buf.String() != expected {
						t.Fatalf("unexpected output: %s\nexpected: %s", buf.String(), expected)
					}
					if errBuf.String() != "" {
						t.Fatalf("unexpected error output: %s", errBuf.String())
					}
					if !verifiedPatch {
						t.Fatal("No server-side patch call detected")
					}
				})
			})
		}
	}
}

// TestUnstructuredApply checks apply operations on an unstructured object
func TestUnstructuredApply(t *testing.T) {
	cmdtesting.InitTestErrorHandler(t)
	name, curr := readAndAnnotateUnstructured(t, filenameWidgetClientside)
	path := "/namespaces/test/widgets/" + name

	verifiedPatch := false

	for _, testingOpenAPISchema := range testingOpenAPISchemas {
		for _, openAPIFeatureToggle := range applyFeatureToggles {

			t.Run("test apply works correctly with unstructured objects", func(t *testing.T) {
				openAPIFeatureToggle(t, func(t *testing.T) {
					tf := cmdtesting.NewTestFactory().WithNamespace("test")
					defer tf.Cleanup()

					tf.UnstructuredClient = &fake.RESTClient{
						NegotiatedSerializer: resource.UnstructuredPlusDefaultContentConfig().NegotiatedSerializer,
						Client: fake.CreateHTTPClient(func(req *http.Request) (*http.Response, error) {
							switch p, m := req.URL.Path, req.Method; {
							case p == path && m == "GET":
								body := io.NopCloser(bytes.NewReader(curr))
								return &http.Response{
									StatusCode: http.StatusOK,
									Header:     cmdtesting.DefaultHeader(),
									Body:       body}, nil
							case p == path && m == "PATCH":
								validatePatchApplication(t, req, types.MergePatchType)
								verifiedPatch = true

								body := io.NopCloser(bytes.NewReader(curr))
								return &http.Response{
									StatusCode: http.StatusOK,
									Header:     cmdtesting.DefaultHeader(),
									Body:       body}, nil
							default:
								t.Fatalf("unexpected request: %#v\n%#v", req.URL, req)
								return nil, nil
							}
						}),
					}
					tf.OpenAPISchemaFunc = testingOpenAPISchema.OpenAPISchemaFn
					tf.OpenAPIV3ClientFunc = testingOpenAPISchema.OpenAPIV3ClientFunc
					tf.ClientConfigVal = cmdtesting.DefaultClientConfig()

					ioStreams, _, buf, errBuf := genericiooptions.NewTestIOStreams()
					cmd := NewCmdApply("kubectl", tf, ioStreams)
					cmd.Flags().Set("filename", filenameWidgetClientside)
					cmd.Flags().Set("output", "name")
					cmd.Run(cmd, []string{})

					expected := "widget.unit-test.test.com/" + name + "\n"
					if buf.String() != expected {
						t.Fatalf("unexpected output: %s\nexpected: %s", buf.String(), expected)
					}
					if errBuf.String() != "" {
						t.Fatalf("unexpected error output: %s", errBuf.String())
					}
					if !verifiedPatch {
						t.Fatal("No server-side patch call detected")
					}
				})
			})
		}
	}
}

// TestUnstructuredIdempotentApply checks repeated apply operation on an unstructured object
func TestUnstructuredIdempotentApply(t *testing.T) {
	cmdtesting.InitTestErrorHandler(t)

	serversideObject := readUnstructuredFromFile(t, filenameWidgetServerside)
	serversideData, err := runtime.Encode(unstructured.NewJSONFallbackEncoder(codec), serversideObject)
	if err != nil {
		t.Fatal(err)
	}
	path := "/namespaces/test/widgets/widget"

	for _, testingOpenAPISchema := range testingOpenAPISchemas {
		for _, openAPIFeatureToggle := range applyFeatureToggles {

			t.Run("test repeated apply operations on an unstructured object", func(t *testing.T) {
				openAPIFeatureToggle(t, func(t *testing.T) {

					tf := cmdtesting.NewTestFactory().WithNamespace("test")
					defer tf.Cleanup()

					tf.UnstructuredClient = &fake.RESTClient{
						NegotiatedSerializer: resource.UnstructuredPlusDefaultContentConfig().NegotiatedSerializer,
						Client: fake.CreateHTTPClient(func(req *http.Request) (*http.Response, error) {
							switch p, m := req.URL.Path, req.Method; {
							case p == path && m == "GET":
								body := io.NopCloser(bytes.NewReader(serversideData))
								return &http.Response{
									StatusCode: http.StatusOK,
									Header:     cmdtesting.DefaultHeader(),
									Body:       body}, nil
							case p == path && m == "PATCH":
								// In idempotent updates, kubectl will resolve to an empty patch and not send anything to the server
								// Thus, if we reach this branch, kubectl is unnecessarily sending a patch.
								patch, err := io.ReadAll(req.Body)
								if err != nil {
									t.Fatal(err)
								}
								t.Fatalf("Unexpected Patch: %s", patch)
								return nil, nil
							default:
								t.Fatalf("unexpected request: %#v\n%#v", req.URL, req)
								return nil, nil
							}
						}),
					}
					tf.OpenAPISchemaFunc = testingOpenAPISchema.OpenAPISchemaFn
					tf.OpenAPIV3ClientFunc = testingOpenAPISchema.OpenAPIV3ClientFunc
					tf.ClientConfigVal = cmdtesting.DefaultClientConfig()

					ioStreams, _, buf, errBuf := genericiooptions.NewTestIOStreams()
					cmd := NewCmdApply("kubectl", tf, ioStreams)
					cmd.Flags().Set("filename", filenameWidgetClientside)
					cmd.Flags().Set("output", "name")
					cmd.Run(cmd, []string{})

					expected := "widget.unit-test.test.com/widget\n"
					if buf.String() != expected {
						t.Fatalf("unexpected output: %s\nexpected: %s", buf.String(), expected)
					}
					if errBuf.String() != "" {
						t.Fatalf("unexpected error output: %s", errBuf.String())
					}
				})
			})
		}
	}
}

func TestRunApplySetLastApplied(t *testing.T) {
	cmdtesting.InitTestErrorHandler(t)
	nameRC, currentRC := readAndAnnotateReplicationController(t, filenameRC)
	pathRC := "/namespaces/test/replicationcontrollers/" + nameRC

	noExistRC, _ := readAndAnnotateReplicationController(t, filenameNoExistRC)
	noExistPath := "/namespaces/test/replicationcontrollers/" + noExistRC

	noAnnotationName, noAnnotationRC := readReplicationController(t, filenameRCNoAnnotation)
	noAnnotationPath := "/namespaces/test/replicationcontrollers/" + noAnnotationName

	tests := []struct {
		name, nameRC, pathRC, filePath, expectedErr, expectedOut, output string
	}{
		{
			name:        "set with exist object",
			filePath:    filenameRC,
			expectedErr: "",
			expectedOut: "replicationcontroller/test-rc\n",
			output:      "name",
		},
		{
			name:        "set with no-exist object",
			filePath:    filenameNoExistRC,
			expectedErr: "Error from server (NotFound): the server could not find the requested resource (get replicationcontrollers no-exist)",
			expectedOut: "",
			output:      "name",
		},
		{
			name:        "set for the annotation does not exist on the live object",
			filePath:    filenameRCNoAnnotation,
			expectedErr: "error: no last-applied-configuration annotation found on resource: no-annotation, to create the annotation, run the command with --create-annotation",
			expectedOut: "",
			output:      "name",
		},
		{
			name:        "set with exist object output json",
			filePath:    filenameRCJSON,
			expectedErr: "",
			expectedOut: "replicationcontroller/test-rc\n",
			output:      "name",
		},
		{
			name:        "set test for a directory of files",
			filePath:    dirName,
			expectedErr: "",
			expectedOut: "replicationcontroller/test-rc\nreplicationcontroller/test-rc\n",
			output:      "name",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tf := cmdtesting.NewTestFactory().WithNamespace("test")
			defer tf.Cleanup()

			tf.UnstructuredClient = &fake.RESTClient{
				GroupVersion:         schema.GroupVersion{Version: "v1"},
				NegotiatedSerializer: resource.UnstructuredPlusDefaultContentConfig().NegotiatedSerializer,
				Client: fake.CreateHTTPClient(func(req *http.Request) (*http.Response, error) {
					switch p, m := req.URL.Path, req.Method; {
					case p == pathRC && m == "GET":
						bodyRC := io.NopCloser(bytes.NewReader(currentRC))
						return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: bodyRC}, nil
					case p == noAnnotationPath && m == "GET":
						bodyRC := io.NopCloser(bytes.NewReader(noAnnotationRC))
						return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: bodyRC}, nil
					case p == noExistPath && m == "GET":
						return &http.Response{StatusCode: http.StatusNotFound, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, &corev1.Pod{})}, nil
					case p == pathRC && m == "PATCH":
						checkPatchString(t, req)
						bodyRC := io.NopCloser(bytes.NewReader(currentRC))
						return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: bodyRC}, nil
					case p == "/api/v1/namespaces/test" && m == "GET":
						return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, &corev1.Namespace{})}, nil
					default:
						t.Fatalf("unexpected request: %#v\n%#v", req.URL, req)
						return nil, nil
					}
				}),
			}
			tf.ClientConfigVal = cmdtesting.DefaultClientConfig()

			cmdutil.BehaviorOnFatal(func(str string, code int) {
				if str != test.expectedErr {
					t.Errorf("%s: unexpected error: %s\nexpected: %s", test.name, str, test.expectedErr)
				}
			})

			ioStreams, _, buf, _ := genericiooptions.NewTestIOStreams()
			cmd := NewCmdApplySetLastApplied(tf, ioStreams)
			cmd.Flags().Set("filename", test.filePath)
			cmd.Flags().Set("output", test.output)
			cmd.Run(cmd, []string{})

			if buf.String() != test.expectedOut {
				t.Fatalf("%s: unexpected output: %s\nexpected: %s", test.name, buf.String(), test.expectedOut)
			}
		})
	}
	cmdutil.BehaviorOnFatal(func(str string, code int) {})
}

func checkPatchString(t *testing.T, req *http.Request) {
	checkString := string(readBytesFromFile(t, filenameRCPatchTest))
	patch, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatal(err)
	}

	patchMap := map[string]interface{}{}
	if err := json.Unmarshal(patch, &patchMap); err != nil {
		t.Fatal(err)
	}

	annotationsMap := walkMapPath(t, patchMap, []string{"metadata", "annotations"})
	if _, ok := annotationsMap[corev1.LastAppliedConfigAnnotation]; !ok {
		t.Fatalf("patch does not contain annotation:\n%s\n", patch)
	}

	resultString := annotationsMap["kubectl.kubernetes.io/last-applied-configuration"]
	if resultString != checkString {
		t.Fatalf("patch annotation is not correct, expect:%s\n but got:%s\n", checkString, resultString)
	}
}

func TestForceApply(t *testing.T) {
	cmdtesting.InitTestErrorHandler(t)
	scheme := runtime.NewScheme()
	nameRC, currentRC := readAndAnnotateReplicationController(t, filenameRC)
	pathRC := "/namespaces/test/replicationcontrollers/" + nameRC
	pathRCList := "/namespaces/test/replicationcontrollers"
	expected := map[string]int{
		"getOk":       6,
		"getNotFound": 1,
		"getList":     0,
		"patch":       6,
		"delete":      1,
		"post":        1,
	}

	// Set the patch retry back off period to something low, so the test can run more quickly
	patchRetryBackOffPeriod = 1 * time.Millisecond

	for _, testingOpenAPISchema := range testingOpenAPISchemas {
		for _, openAPIFeatureToggle := range applyFeatureToggles {

			t.Run("test apply with --force", func(t *testing.T) {
				openAPIFeatureToggle(t, func(t *testing.T) {
					deleted := false
					isScaledDownToZero := false
					counts := map[string]int{}
					tf := cmdtesting.NewTestFactory().WithNamespace("test")
					defer tf.Cleanup()

					tf.ClientConfigVal = cmdtesting.DefaultClientConfig()
					tf.UnstructuredClient = &fake.RESTClient{
						NegotiatedSerializer: resource.UnstructuredPlusDefaultContentConfig().NegotiatedSerializer,
						Client: fake.CreateHTTPClient(func(req *http.Request) (*http.Response, error) {
							switch p, m := req.URL.Path, req.Method; {
							case strings.HasSuffix(p, pathRC) && m == "GET":
								if deleted {
									counts["getNotFound"]++
									return &http.Response{StatusCode: http.StatusNotFound, Header: cmdtesting.DefaultHeader(), Body: io.NopCloser(bytes.NewReader([]byte{}))}, nil
								}
								counts["getOk"]++
								var bodyRC io.ReadCloser
								if isScaledDownToZero {
									rcObj := readReplicationControllerFromFile(t, filenameRC)
									rcObj.Spec.Replicas = ptr.To[int32](0)
									rcBytes, err := runtime.Encode(codec, rcObj)
									if err != nil {
										t.Fatal(err)
									}
									bodyRC = io.NopCloser(bytes.NewReader(rcBytes))
								} else {
									bodyRC = io.NopCloser(bytes.NewReader(currentRC))
								}
								return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: bodyRC}, nil
							case strings.HasSuffix(p, pathRCList) && m == "GET":
								counts["getList"]++
								rcObj := readUnstructuredFromFile(t, filenameRC)
								list := &unstructured.UnstructuredList{
									Object: map[string]interface{}{
										"apiVersion": "v1",
										"kind":       "ReplicationControllerList",
									},
									Items: []unstructured.Unstructured{*rcObj},
								}
								listBytes, err := runtime.Encode(codec, list)
								if err != nil {
									t.Fatal(err)
								}
								bodyRCList := io.NopCloser(bytes.NewReader(listBytes))
								return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: bodyRCList}, nil
							case strings.HasSuffix(p, pathRC) && m == "PATCH":
								counts["patch"]++
								if counts["patch"] <= 6 {
									statusErr := apierrors.NewConflict(schema.GroupResource{Group: "", Resource: "rc"}, "test-rc", fmt.Errorf("the object has been modified. Please apply at first"))
									bodyBytes, _ := json.Marshal(statusErr)
									bodyErr := io.NopCloser(bytes.NewReader(bodyBytes))
									return &http.Response{StatusCode: http.StatusConflict, Header: cmdtesting.DefaultHeader(), Body: bodyErr}, nil
								}
								t.Fatalf("unexpected request: %#v after %v tries\n%#v", req.URL, counts["patch"], req)
								return nil, nil
							case strings.HasSuffix(p, pathRC) && m == "DELETE":
								counts["delete"]++
								deleted = true
								bodyRC := io.NopCloser(bytes.NewReader(currentRC))
								return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: bodyRC}, nil
							case strings.HasSuffix(p, pathRC) && m == "PUT":
								counts["put"]++
								bodyRC := io.NopCloser(bytes.NewReader(currentRC))
								isScaledDownToZero = true
								return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: bodyRC}, nil
							case strings.HasSuffix(p, pathRCList) && m == "POST":
								counts["post"]++
								deleted = false
								isScaledDownToZero = false
								bodyRC := io.NopCloser(bytes.NewReader(currentRC))
								return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: bodyRC}, nil
							default:
								t.Fatalf("unexpected request: %#v\n%#v", req.URL, req)
								return nil, nil
							}
						}),
					}
					fakeDynamicClient := dynamicfakeclient.NewSimpleDynamicClient(scheme)
					tf.FakeDynamicClient = fakeDynamicClient
					tf.OpenAPISchemaFunc = testingOpenAPISchema.OpenAPISchemaFn
					tf.OpenAPIV3ClientFunc = testingOpenAPISchema.OpenAPIV3ClientFunc
					tf.Client = tf.UnstructuredClient
					tf.ClientConfigVal = &restclient.Config{}

					ioStreams, _, buf, errBuf := genericiooptions.NewTestIOStreams()
					cmd := NewCmdApply("kubectl", tf, ioStreams)
					cmd.Flags().Set("filename", filenameRC)
					cmd.Flags().Set("output", "name")
					cmd.Flags().Set("force", "true")
					cmd.Run(cmd, []string{})

					for method, exp := range expected {
						if exp != counts[method] {
							t.Errorf("Unexpected amount of %q API calls, wanted %v got %v", method, exp, counts[method])
						}
					}

					if expected := "replicationcontroller/" + nameRC + "\n"; buf.String() != expected {
						t.Fatalf("unexpected output: %s\nexpected: %s", buf.String(), expected)
					}
					if errBuf.String() != "" {
						t.Fatalf("unexpected error output: %s", errBuf.String())
					}
				})
			})
		}
	}
}

func TestDontAllowForceApplyWithServerDryRun(t *testing.T) {
	expectedError := "error: --dry-run=server cannot be used with --force"

	cmdutil.BehaviorOnFatal(func(str string, code int) {
		panic(str)
	})
	defer func() {
		actualError := recover()
		if expectedError != actualError {
			t.Fatalf(`expected error "%s", but got "%s"`, expectedError, actualError)
		}
	}()

	tf := cmdtesting.NewTestFactory().WithNamespace("test")
	defer tf.Cleanup()

	tf.ClientConfigVal = cmdtesting.DefaultClientConfig()

	ioStreams, _, _, _ := genericiooptions.NewTestIOStreams()
	cmd := NewCmdApply("kubectl", tf, ioStreams)
	cmd.Flags().Set("filename", filenameRC)
	cmd.Flags().Set("dry-run", "server")
	cmd.Flags().Set("force", "true")
	cmd.Run(cmd, []string{})

	t.Fatalf(`expected error "%s"`, expectedError)
}

func TestDontAllowForceApplyWithServerSide(t *testing.T) {
	expectedError := "error: --force cannot be used with --server-side"

	cmdutil.BehaviorOnFatal(func(str string, code int) {
		panic(str)
	})
	defer func() {
		actualError := recover()
		if expectedError != actualError {
			t.Fatalf(`expected error "%s", but got "%s"`, expectedError, actualError)
		}
	}()

	tf := cmdtesting.NewTestFactory().WithNamespace("test")
	defer tf.Cleanup()

	tf.ClientConfigVal = cmdtesting.DefaultClientConfig()

	ioStreams, _, _, _ := genericiooptions.NewTestIOStreams()
	cmd := NewCmdApply("kubectl", tf, ioStreams)
	cmd.Flags().Set("filename", filenameRC)
	cmd.Flags().Set("server-side", "true")
	cmd.Flags().Set("force", "true")
	cmd.Run(cmd, []string{})

	t.Fatalf(`expected error "%s"`, expectedError)
}

func TestDontAllowApplyWithPodGeneratedName(t *testing.T) {
	expectedError := "error: from testing-: cannot use generate name with apply"
	cmdutil.BehaviorOnFatal(func(str string, code int) {
		if str != expectedError {
			t.Fatalf(`expected error "%s", but got "%s"`, expectedError, str)
		}
	})

	tf := cmdtesting.NewTestFactory().WithNamespace("test")
	defer tf.Cleanup()
	tf.ClientConfigVal = cmdtesting.DefaultClientConfig()

	ioStreams, _, _, _ := genericiooptions.NewTestIOStreams()
	cmd := NewCmdApply("kubectl", tf, ioStreams)
	cmd.Flags().Set("filename", filenamePodGeneratedName)
	cmd.Flags().Set("dry-run", "client")
	cmd.Run(cmd, []string{})
}

func TestApplySetParentValidation(t *testing.T) {
	for name, test := range map[string]struct {
		applysetFlag        string
		namespaceFlag       string
		setup               func(*testing.T, *cmdtesting.TestFactory)
		expectParentKind    string
		expectBlankParentNs bool
		expectErr           string
	}{
		"parent type must be valid": {
			applysetFlag: "doesnotexist/thename",
			expectErr:    "invalid parent reference \"doesnotexist/thename\": no matches for /, Resource=doesnotexist",
		},
		"parent name must be present": {
			applysetFlag: "secret/",
			expectErr:    "invalid parent reference \"secret/\": name cannot be blank",
		},
		"configmap parents are valid": {
			applysetFlag:     "configmap/thename",
			namespaceFlag:    "mynamespace",
			expectParentKind: "ConfigMap",
		},
		"secret parents are valid": {
			applysetFlag:     "secret/thename",
			namespaceFlag:    "mynamespace",
			expectParentKind: "Secret",
		},
		"plural resource works": {
			applysetFlag:     "secrets/thename",
			namespaceFlag:    "mynamespace",
			expectParentKind: "Secret",
		},
		"other namespaced builtin parents types are correctly parsed but invalid": {
			applysetFlag:     "deployments.apps/thename",
			expectParentKind: "Deployment",
			expectErr:        "[namespace is required to use namespace-scoped ApplySet, resource \"apps/v1, Resource=deployments\" is not permitted as an ApplySet parent]",
		},
		"namespaced builtin parents with multi-segment groups are correctly parsed but invalid": {
			applysetFlag:     "priorityclasses.scheduling.k8s.io/thename",
			expectParentKind: "PriorityClass",
			expectErr:        "resource \"scheduling.k8s.io/v1alpha1, Resource=priorityclasses\" is not permitted as an ApplySet parent",
		},
		"non-namespaced builtin types are correctly parsed but invalid": {
			applysetFlag:        "namespaces/thename",
			expectParentKind:    "Namespace",
			namespaceFlag:       "somenamespace",
			expectBlankParentNs: true,
			expectErr:           "resource \"/v1, Resource=namespaces\" is not permitted as an ApplySet parent",
		},
		"parent namespace should use the value of the namespace flag": {
			applysetFlag:     "mysecret",
			namespaceFlag:    "mynamespace",
			expectParentKind: "Secret",
		},
		"parent namespace should not use the default namespace from ClientConfig": {
			applysetFlag: "mysecret",
			setup: func(t *testing.T, f *cmdtesting.TestFactory) {
				// by default, the value "default" is used for the namespace
				// make sure this assumption still holds
				ns, overridden, err := f.ToRawKubeConfigLoader().Namespace()
				require.NoError(t, err)
				require.Falsef(t, overridden, "namespace unexpectedly overridden")
				require.Equal(t, "default", ns)
			},
			expectBlankParentNs: true,
			expectParentKind:    "Secret",
			expectErr:           "namespace is required to use namespace-scoped ApplySet",
		},
		"parent namespace should not use the default namespace from the user's kubeconfig": {
			applysetFlag: "mysecret",
			setup: func(t *testing.T, f *cmdtesting.TestFactory) {
				kubeConfig := clientcmdapi.NewConfig()
				kubeConfig.CurrentContext = "default"
				kubeConfig.Contexts["default"] = &clientcmdapi.Context{Namespace: "bar"}
				clientConfig := clientcmd.NewDefaultClientConfig(*kubeConfig, &clientcmd.ConfigOverrides{
					ClusterDefaults: clientcmdapi.Cluster{Server: "http://localhost:8080"}})
				f.WithClientConfig(clientConfig)
			},
			expectBlankParentNs: true,
			expectParentKind:    "Secret",
			expectErr:           "namespace is required to use namespace-scoped ApplySet",
		},
	} {
		t.Run(name, func(t *testing.T) {
			cmdtesting.WithAlphaEnvs([]cmdutil.FeatureGate{cmdutil.ApplySet}, t, func(t *testing.T) {
				cmd := &cobra.Command{}
				flags := NewApplyFlags(genericiooptions.NewTestIOStreamsDiscard())
				flags.AddFlags(cmd)
				cmd.Flags().Set("filename", filenameRC)
				cmd.Flags().Set("applyset", test.applysetFlag)
				cmd.Flags().Set("prune", "true")
				f := cmdtesting.NewTestFactory()
				defer f.Cleanup()
				setUpClientsForApplySetWithSSA(t, f)

				var expectedParentNs string
				if test.namespaceFlag != "" {
					f.WithNamespace(test.namespaceFlag)
					if !test.expectBlankParentNs {
						expectedParentNs = test.namespaceFlag
					}
				}

				if test.setup != nil {
					test.setup(t, f)
				}

				o, err := flags.ToOptions(f, cmd, "kubectl", []string{})
				if test.expectErr == "" {
					require.NoError(t, err, "ToOptions error")
				} else if err != nil {
					require.EqualError(t, err, test.expectErr)
					return
				}

				assert.Equal(t, expectedParentNs, o.ApplySet.parentRef.Namespace)
				assert.Equal(t, test.expectParentKind, o.ApplySet.parentRef.GroupVersionKind.Kind)

				err = o.Validate()
				if test.expectErr != "" {
					require.EqualError(t, err, test.expectErr)
				} else {
					require.NoError(t, err, "Validate error")
				}
			})
		})
	}
}

func setUpClientsForApplySetWithSSA(t *testing.T, tf *cmdtesting.TestFactory, objects ...runtime.Object) {
	listMapping := map[schema.GroupVersionResource]string{
		{Group: "", Version: "v1", Resource: "services"}:                                      "ServiceList",
		{Group: "", Version: "v1", Resource: "replicationcontrollers"}:                        "ReplicationControllerList",
		{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}: "CustomResourceDefinitionList",
	}
	fakeDynamicClient := dynamicfakeclient.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listMapping, objects...)
	tf.FakeDynamicClient = fakeDynamicClient

	tf.UnstructuredClient = &fake.RESTClient{
		NegotiatedSerializer: resource.UnstructuredPlusDefaultContentConfig().NegotiatedSerializer,
		Client: fake.CreateHTTPClient(func(req *http.Request) (*http.Response, error) {
			tokens := strings.Split(strings.TrimPrefix(req.URL.Path, "/"), "/")
			var gvr schema.GroupVersionResource
			var name, namespace string

			if len(tokens) == 4 && tokens[0] == "namespaces" { // e.g. namespaces/my-ns/secrets/my-secret
				namespace = tokens[1]
				name = tokens[3]
				gvr = schema.GroupVersionResource{Version: "v1", Resource: tokens[2]}
			} else if len(tokens) == 2 && tokens[0] == "applysets" {
				gvr = schema.GroupVersionResource{Group: "company.com", Version: "v1", Resource: tokens[0]}
				name = tokens[1]
			} else {
				t.Fatalf("unexpected request: path segments %v: request: \n%#v", tokens, req)
				return nil, nil
			}

			switch req.Method {
			case "GET":
				obj, err := fakeDynamicClient.Tracker().Get(gvr, namespace, name)
				if err == nil {
					objJson, err := json.Marshal(obj)
					require.NoError(t, err)
					return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.BytesBody(objJson)}, nil
				} else if apierrors.IsNotFound(err) {
					return &http.Response{StatusCode: http.StatusNotFound, Header: cmdtesting.DefaultHeader()}, nil
				} else {
					t.Fatalf("error getting object: %v", err)
				}
			case "PATCH":
				require.Equal(t, string(types.ApplyPatchType), req.Header.Get("Content-Type"), "received patch request with unexpected patch type")

				var existing *unstructured.Unstructured
				existingObj, err := fakeDynamicClient.Tracker().Get(gvr, namespace, name)
				if err != nil {
					if !apierrors.IsNotFound(err) {
						t.Fatalf("error getting object: %v", err)
					}
				} else {
					existing = existingObj.(*unstructured.Unstructured)
				}

				data, err := io.ReadAll(req.Body)
				require.NoError(t, err)

				patch := &unstructured.Unstructured{}
				err = runtime.DecodeInto(codec, data, patch)
				require.NoError(t, err)

				var returnData []byte
				if existing == nil {
					patch.SetUID("a-static-fake-uid")
					err := fakeDynamicClient.Tracker().Create(gvr, patch, namespace)
					require.NoError(t, err, "error creating object")

					returnData, err = json.Marshal(patch)
					require.NoError(t, err, "error marshalling response: %v", err)
				} else {
					uid := existing.GetUID()
					patch.DeepCopyInto(existing)
					existing.SetUID(uid)

					err = fakeDynamicClient.Tracker().Update(gvr, existing, namespace)
					require.NoError(t, err, "error updating object")

					returnData, err = json.Marshal(existing)
					require.NoError(t, err, "error marshalling response")
				}
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: io.NopCloser(bytes.NewReader(returnData))}, nil

			default:
				t.Fatalf("unexpected request: %s\n%#v", req.URL.Path, req)
				return nil, nil
			}
			return nil, nil
		}),
	}
	tf.Client = tf.UnstructuredClient
}

func TestLoadObjects(t *testing.T) {
	f := cmdtesting.NewTestFactory().WithNamespace("test")
	defer f.Cleanup()
	f.Client = &fake.RESTClient{}
	f.UnstructuredClient = f.Client

	testFiles := []string{"testdata/prune/simple/manifest1", "testdata/prune/simple/manifest2"}
	for _, testFile := range testFiles {
		t.Run(testFile, func(t *testing.T) {
			cmdtesting.WithAlphaEnvs([]cmdutil.FeatureGate{cmdutil.ApplySet}, t, func(t *testing.T) {

				cmd := &cobra.Command{}
				flags := NewApplyFlags(genericiooptions.NewTestIOStreamsDiscard())
				flags.AddFlags(cmd)
				cmd.Flags().Set("filename", testFile+".yaml")
				cmd.Flags().Set("applyset", filepath.Base(filepath.Dir(testFile)))
				cmd.Flags().Set("prune", "true")

				o, err := flags.ToOptions(f, cmd, "kubectl", []string{})
				if err != nil {
					t.Fatalf("unexpected error creating apply options: %v", err)
				}

				err = o.Validate()
				if err != nil {
					t.Fatalf("unexpected error from validate: %v", err)
				}

				resources, err := o.GetObjects()
				if err != nil {
					t.Fatalf("GetObjects gave unexpected error %v", err)
				}

				var objectYAMLs []string
				for _, obj := range resources {
					y, err := yaml.Marshal(obj.Object)
					if err != nil {
						t.Fatalf("error marshaling object: %v", err)
					}
					objectYAMLs = append(objectYAMLs, string(y))
				}
				got := strings.Join(objectYAMLs, "\n---\n\n")

				p := testFile + "-expected-getobjects.yaml"
				wantBytes, err := os.ReadFile(p)
				if err != nil {
					t.Fatalf("error reading file %q: %v", p, err)
				}
				want := string(wantBytes)
				if diff := cmp.Diff(want, got); diff != "" {
					t.Errorf("GetObjects returned unexpected diff (-want +got):\n%s", diff)
				}
			})
		})
	}
}

func TestApplySetParentManagement(t *testing.T) {
	nameParentSecret := "my-set"
	tf := cmdtesting.NewTestFactory().WithNamespace("test")
	defer tf.Cleanup()

	replicationController := readUnstructuredFromFile(t, filenameRC)
	setUpClientsForApplySetWithSSA(t, tf, replicationController)
	failDeletes := false
	tf.FakeDynamicClient.PrependReactor("delete", "*", func(action testing2.Action) (handled bool, ret runtime.Object, err error) {
		if failDeletes {
			return true, nil, fmt.Errorf("an error on the server (\"\") has prevented the request from succeeding")
		}
		return false, nil, nil
	})
	cmdutil.BehaviorOnFatal(func(s string, i int) {
		if failDeletes && s == `error: pruning ReplicationController test/test-rc: an error on the server ("") has prevented the request from succeeding` {
			t.Logf("got expected error %q", s)
		} else {
			t.Fatalf("unexpected exit %d: %s", i, s)
		}
	})
	defer cmdutil.DefaultBehaviorOnFatal()

	// Initially, the rc 'exists' server side but the svc and applyset secret do not
	// This should 'update' the rc and create the secret
	ioStreams, _, outbuff, errbuff := genericiooptions.NewTestIOStreams()
	cmdtesting.WithAlphaEnvs([]cmdutil.FeatureGate{cmdutil.ApplySet}, t, func(t *testing.T) {
		cmd := NewCmdApply("kubectl", tf, ioStreams)
		cmd.Flags().Set("filename", filenameRC)
		cmd.Flags().Set("server-side", "true")
		cmd.Flags().Set("applyset", nameParentSecret)
		cmd.Flags().Set("prune", "true")
		cmd.Run(cmd, []string{})
	})
	assert.Equal(t, "replicationcontroller/test-rc serverside-applied\n", outbuff.String())
	assert.Equal(t, "", errbuff.String())

	createdSecret, err := tf.FakeDynamicClient.Tracker().Get(schema.GroupVersionResource{Resource: "secrets", Version: "v1"}, "test", nameParentSecret)
	require.NoError(t, err)
	createSecretYaml, err := yaml.Marshal(createdSecret)
	require.NoError(t, err)
	require.Equal(t, `apiVersion: v1
kind: Secret
metadata:
  annotations:
    applyset.kubernetes.io/additional-namespaces: ""
    applyset.kubernetes.io/contains-group-kinds: ReplicationController
    applyset.kubernetes.io/tooling: kubectl/v0.0.0-master+$Format:%H$
  creationTimestamp: null
  labels:
    applyset.kubernetes.io/id: applyset-0eFHV8ySqp7XoShsGvyWFQD3s96yqwHmzc4e0HR1dsY-v1
  name: my-set
  namespace: test
  uid: a-static-fake-uid
`, string(createSecretYaml))

	// Next, do an apply that creates a second resource, the svc, and updates the applyset secret
	outbuff.Reset()
	errbuff.Reset()
	cmdtesting.WithAlphaEnvs([]cmdutil.FeatureGate{cmdutil.ApplySet}, t, func(t *testing.T) {
		cmd := NewCmdApply("kubectl", tf, ioStreams)
		cmd.Flags().Set("filename", filenameRC)
		cmd.Flags().Set("filename", filenameSVC)
		cmd.Flags().Set("server-side", "true")
		cmd.Flags().Set("applyset", nameParentSecret)
		cmd.Flags().Set("prune", "true")
		cmd.Run(cmd, []string{})
	})
	assert.Equal(t, "replicationcontroller/test-rc serverside-applied\nservice/test-service serverside-applied\n", outbuff.String())
	assert.Equal(t, "", errbuff.String())

	updatedSecret, err := tf.FakeDynamicClient.Tracker().Get(schema.GroupVersionResource{Resource: "secrets", Version: "v1"}, "test", nameParentSecret)
	require.NoError(t, err)
	updatedSecretYaml, err := yaml.Marshal(updatedSecret)
	require.NoError(t, err)
	require.Equal(t, `apiVersion: v1
kind: Secret
metadata:
  annotations:
    applyset.kubernetes.io/additional-namespaces: ""
    applyset.kubernetes.io/contains-group-kinds: ReplicationController,Service
    applyset.kubernetes.io/tooling: kubectl/v0.0.0-master+$Format:%H$
  creationTimestamp: null
  labels:
    applyset.kubernetes.io/id: applyset-0eFHV8ySqp7XoShsGvyWFQD3s96yqwHmzc4e0HR1dsY-v1
  name: my-set
  namespace: test
  uid: a-static-fake-uid
`, string(updatedSecretYaml))

	// Next, do an apply that attempts to remove the rc from the set, but pruning fails
	// Both types remain in the ApplySet
	failDeletes = true
	outbuff.Reset()
	errbuff.Reset()
	cmdtesting.WithAlphaEnvs([]cmdutil.FeatureGate{cmdutil.ApplySet}, t, func(t *testing.T) {
		cmd := NewCmdApply("kubectl", tf, ioStreams)
		cmd.Flags().Set("filename", filenameSVC)
		cmd.Flags().Set("server-side", "true")
		cmd.Flags().Set("applyset", nameParentSecret)
		cmd.Flags().Set("prune", "true")
		cmd.Run(cmd, []string{})
	})
	assert.Equal(t, "service/test-service serverside-applied\n", outbuff.String())
	assert.Equal(t, "", errbuff.String())

	updatedSecret, err = tf.FakeDynamicClient.Tracker().Get(schema.GroupVersionResource{Resource: "secrets", Version: "v1"}, "test", nameParentSecret)
	require.NoError(t, err)
	updatedSecretYaml, err = yaml.Marshal(updatedSecret)
	require.NoError(t, err)
	require.Equal(t, `apiVersion: v1
kind: Secret
metadata:
  annotations:
    applyset.kubernetes.io/additional-namespaces: ""
    applyset.kubernetes.io/contains-group-kinds: ReplicationController,Service
    applyset.kubernetes.io/tooling: kubectl/v0.0.0-master+$Format:%H$
  creationTimestamp: null
  labels:
    applyset.kubernetes.io/id: applyset-0eFHV8ySqp7XoShsGvyWFQD3s96yqwHmzc4e0HR1dsY-v1
  name: my-set
  namespace: test
  uid: a-static-fake-uid
`, string(updatedSecretYaml))

	// Finally, do an apply that successfully removes the rc and updates the set
	failDeletes = false

	outbuff.Reset()
	errbuff.Reset()
	cmdtesting.WithAlphaEnvs([]cmdutil.FeatureGate{cmdutil.ApplySet}, t, func(t *testing.T) {
		cmd := NewCmdApply("kubectl", tf, ioStreams)
		cmd.Flags().Set("filename", filenameSVC)
		cmd.Flags().Set("server-side", "true")
		cmd.Flags().Set("applyset", nameParentSecret)
		cmd.Flags().Set("prune", "true")
		cmd.Run(cmd, []string{})
	})
	assert.Equal(t, "service/test-service serverside-applied\nreplicationcontroller/test-rc pruned\n", outbuff.String())
	assert.Equal(t, "", errbuff.String())

	updatedSecret, err = tf.FakeDynamicClient.Tracker().Get(schema.GroupVersionResource{Resource: "secrets", Version: "v1"}, "test", nameParentSecret)
	require.NoError(t, err)
	updatedSecretYaml, err = yaml.Marshal(updatedSecret)
	require.NoError(t, err)
	require.Equal(t, `apiVersion: v1
kind: Secret
metadata:
  annotations:
    applyset.kubernetes.io/additional-namespaces: ""
    applyset.kubernetes.io/contains-group-kinds: Service
    applyset.kubernetes.io/tooling: kubectl/v0.0.0-master+$Format:%H$
  creationTimestamp: null
  labels:
    applyset.kubernetes.io/id: applyset-0eFHV8ySqp7XoShsGvyWFQD3s96yqwHmzc4e0HR1dsY-v1
  name: my-set
  namespace: test
  uid: a-static-fake-uid
`, string(updatedSecretYaml))
}

func TestApplySetInvalidLiveParent(t *testing.T) {
	nameParentSecret := "my-set"
	tf := cmdtesting.NewTestFactory().WithNamespace("test")
	defer tf.Cleanup()

	type testCase struct {
		gksAnnotation     string
		toolingAnnotation string
		idLabel           string
		expectErr         string
	}
	validIDLabel := "applyset-0eFHV8ySqp7XoShsGvyWFQD3s96yqwHmzc4e0HR1dsY-v1"
	validToolingAnnotation := "kubectl/v1.27.0"
	validGksAnnotation := "Deployment.apps,Namespace,Secret"

	for name, test := range map[string]testCase{
		"group-resources annotation is required": {
			gksAnnotation:     "",
			toolingAnnotation: validToolingAnnotation,
			idLabel:           validIDLabel,
			expectErr:         "error: parsing ApplySet annotation on \"secrets./my-set\": kubectl requires the \"applyset.kubernetes.io/contains-group-kinds\" annotation to be set on all ApplySet parent objects",
		},
		"group-resources annotation should not contain invalid resources": {
			gksAnnotation:     "does-not-exist",
			toolingAnnotation: validToolingAnnotation,
			idLabel:           validIDLabel,
			expectErr:         "error: parsing ApplySet annotation on \"secrets./my-set\": could not find mapping for kind in \"applyset.kubernetes.io/contains-group-kinds\" annotation: no matches for kind \"does-not-exist\" in group \"\"",
		},
		"tooling annotation is required": {
			gksAnnotation:     validGksAnnotation,
			toolingAnnotation: "",
			idLabel:           validIDLabel,
			expectErr:         "error: ApplySet parent object \"secrets./my-set\" already exists and is missing required annotation \"applyset.kubernetes.io/tooling\"",
		},
		"tooling annotation must have kubectl prefix": {
			gksAnnotation:     validGksAnnotation,
			toolingAnnotation: "helm/v3",
			idLabel:           validIDLabel,
			expectErr:         "error: ApplySet parent object \"secrets./my-set\" already exists and is managed by tooling \"helm\" instead of \"kubectl\"",
		},
		"tooling annotation with invalid prefix with one segment can be parsed": {
			gksAnnotation:     validGksAnnotation,
			toolingAnnotation: "helm",
			idLabel:           validIDLabel,
			expectErr:         "error: ApplySet parent object \"secrets./my-set\" already exists and is managed by tooling \"helm\" instead of \"kubectl\"",
		},
		"tooling annotation with invalid prefix with many segments can be parsed": {
			gksAnnotation:     validGksAnnotation,
			toolingAnnotation: "example.com/tool/why/v1",
			idLabel:           validIDLabel,
			expectErr:         "error: ApplySet parent object \"secrets./my-set\" already exists and is managed by tooling \"example.com/tool/why\" instead of \"kubectl\"",
		},
		"ID label is required": {
			gksAnnotation:     validGksAnnotation,
			toolingAnnotation: validToolingAnnotation,
			idLabel:           "",
			expectErr:         "error: ApplySet parent object \"secrets./my-set\" exists and does not have required label applyset.kubernetes.io/id",
		},
		"ID label must match the ApplySet's real ID": {
			gksAnnotation:     validGksAnnotation,
			toolingAnnotation: validToolingAnnotation,
			idLabel:           "somethingelse",
			expectErr:         fmt.Sprintf("error: ApplySet parent object \"secrets./my-set\" exists and has incorrect value for label \"applyset.kubernetes.io/id\" (got: somethingelse, want: %s)", validIDLabel),
		},
	} {
		t.Run(name, func(t *testing.T) {
			require.NotEmpty(t, test.expectErr, "invalid test case")
			cmdutil.BehaviorOnFatal(func(s string, i int) {
				assert.Equal(t, test.expectErr, s)
			})
			defer cmdutil.DefaultBehaviorOnFatal()
			secret := &unstructured.Unstructured{}
			secret.SetKind("Secret")
			secret.SetAPIVersion("v1")
			secret.SetName(nameParentSecret)
			secret.SetNamespace("test")
			annotations := make(map[string]string)
			labels := make(map[string]string)
			if test.gksAnnotation != "" {
				annotations[ApplySetGKsAnnotation] = test.gksAnnotation
			}
			if test.toolingAnnotation != "" {
				annotations[ApplySetToolingAnnotation] = test.toolingAnnotation
			}
			if test.idLabel != "" {
				labels[ApplySetParentIDLabel] = test.idLabel
			}
			secret.SetAnnotations(annotations)
			secret.SetLabels(labels)
			setUpClientsForApplySetWithSSA(t, tf, secret)

			cmdtesting.WithAlphaEnvs([]cmdutil.FeatureGate{cmdutil.ApplySet}, t, func(t *testing.T) {
				ioStreams, _, _, _ := genericiooptions.NewTestIOStreams()
				cmd := NewCmdApply("kubectl", tf, ioStreams)
				cmd.Flags().Set("filename", filenameSVC)
				cmd.Flags().Set("server-side", "true")
				cmd.Flags().Set("applyset", nameParentSecret)
				cmd.Flags().Set("prune", "true")
				cmd.Run(cmd, []string{})
			})
		})
	}
}

func TestApplySet_ClusterScopedCustomResourceParent(t *testing.T) {
	tf := cmdtesting.NewTestFactory()
	defer tf.Cleanup()

	replicationController := readUnstructuredFromFile(t, filenameRC)
	crd := readUnstructuredFromFile(t, filenameApplySetCRD)
	cr := readUnstructuredFromFile(t, filenameApplySetCR)
	setUpClientsForApplySetWithSSA(t, tf, replicationController, crd)

	ioStreams, _, outbuff, errbuff := genericiooptions.NewTestIOStreams()
	cmdutil.BehaviorOnFatal(func(s string, i int) {
		require.Equal(t, "error: custom resource ApplySet parents cannot be created automatically", s)
	})
	defer cmdutil.DefaultBehaviorOnFatal()

	// Initially, the rc 'exists' server side the parent CR does not. This should fail.
	cmdtesting.WithAlphaEnvs([]cmdutil.FeatureGate{cmdutil.ApplySet}, t, func(t *testing.T) {
		cmd := NewCmdApply("kubectl", tf, ioStreams)
		cmd.Flags().Set("filename", filenameRC)
		cmd.Flags().Set("server-side", "true")
		cmd.Flags().Set("applyset", fmt.Sprintf("applysets.company.com/my-set"))
		cmd.Flags().Set("prune", "true")
		cmd.Run(cmd, []string{})
	})
	cmdtesting.InitTestErrorHandler(t)

	// Simulate creating the CR parent out of band
	require.NoError(t, tf.FakeDynamicClient.Tracker().Add(cr))
	cmdtesting.WithAlphaEnvs([]cmdutil.FeatureGate{cmdutil.ApplySet}, t, func(t *testing.T) {
		cmd := NewCmdApply("kubectl", tf, ioStreams)
		cmd.Flags().Set("filename", filenameRC)
		cmd.Flags().Set("server-side", "true")
		cmd.Flags().Set("applyset", fmt.Sprintf("applysets.company.com/my-set"))
		cmd.Flags().Set("prune", "true")
		cmd.Run(cmd, []string{})
	})
	assert.Equal(t, "replicationcontroller/test-rc serverside-applied\n", outbuff.String())
	assert.Equal(t, "", errbuff.String())

	updatedCR, err := tf.FakeDynamicClient.Tracker().Get(schema.GroupVersionResource{Resource: "applysets", Version: "v1", Group: "company.com"}, "", "my-set")
	require.NoError(t, err)
	updatedCRYaml, err := yaml.Marshal(updatedCR)
	require.NoError(t, err)
	require.Equal(t, `apiVersion: company.com/v1
kind: ApplySet
metadata:
  annotations:
    applyset.kubernetes.io/additional-namespaces: test
    applyset.kubernetes.io/contains-group-kinds: ReplicationController
    applyset.kubernetes.io/tooling: kubectl/v0.0.0-master+$Format:%H$
  creationTimestamp: null
  labels:
    applyset.kubernetes.io/id: applyset-rhp1a-HVAVT_dFgyEygyA1BEB82HPp2o10UiFTpqtAs-v1
  name: my-set
`, string(updatedCRYaml))
}

func TestApplyWithPruneV2(t *testing.T) {
	testdirs := []string{"testdata/prune/simple"}
	for _, testdir := range testdirs {
		t.Run(testdir, func(t *testing.T) {
			tf := cmdtesting.NewTestFactory().WithNamespace("test")
			defer tf.Cleanup()

			codec := scheme.Codecs.LegacyCodec(scheme.Scheme.PrioritizedVersionsAllGroups()...)

			scheme := runtime.NewScheme()

			listMapping := map[schema.GroupVersionResource]string{
				{Group: "", Version: "v1", Resource: "namespaces"}: "NamespaceList",
			}

			fakeDynamicClient := dynamicfakeclient.NewSimpleDynamicClientWithCustomListKinds(scheme, listMapping)
			tf.FakeDynamicClient = fakeDynamicClient

			tf.UnstructuredClient = &fake.RESTClient{
				NegotiatedSerializer: resource.UnstructuredPlusDefaultContentConfig().NegotiatedSerializer,
				Client: fake.CreateHTTPClient(func(req *http.Request) (*http.Response, error) {
					method := req.Method

					tokens := strings.Split(strings.TrimPrefix(req.URL.Path, "/"), "/")

					if len(tokens) == 2 && tokens[0] == "namespaces" && method == "GET" {
						name := tokens[1]
						gvr := schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}
						ns, err := fakeDynamicClient.Tracker().Get(gvr, "", name)
						if err != nil {
							if apierrors.IsNotFound(err) {
								return &http.Response{StatusCode: http.StatusNotFound, Header: cmdtesting.DefaultHeader()}, nil
							}
							t.Fatalf("error getting object: %v", err)
						}
						return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, ns)}, nil
					}

					if len(tokens) == 4 && tokens[0] == "namespaces" && tokens[2] == "secrets" && method == "GET" {
						namespace := tokens[1]
						name := tokens[3]
						gvr := schema.GroupVersionResource{Version: "v1", Resource: "secrets"}
						obj, err := fakeDynamicClient.Tracker().Get(gvr, namespace, name)
						if err != nil {
							if apierrors.IsNotFound(err) {
								return &http.Response{StatusCode: http.StatusNotFound, Header: cmdtesting.DefaultHeader()}, nil
							}
							t.Fatalf("error getting object: %v", err)
						}
						return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, obj)}, nil
					}

					if len(tokens) == 4 && tokens[0] == "namespaces" && tokens[2] == "secrets" && method == "PATCH" {
						namespace := tokens[1]
						name := tokens[3]
						gvr := schema.GroupVersionResource{Version: "v1", Resource: "secrets"}
						var existing *unstructured.Unstructured
						existingObj, err := fakeDynamicClient.Tracker().Get(gvr, namespace, name)
						if err != nil {
							if !apierrors.IsNotFound(err) {
								t.Fatalf("error getting object: %v", err)
							}
						} else {
							existing = existingObj.(*unstructured.Unstructured)
						}

						data, err := io.ReadAll(req.Body)
						if err != nil {
							t.Fatalf("unexpected error: %v", err)
						}

						patch := &unstructured.Unstructured{}
						if err := runtime.DecodeInto(codec, data, patch); err != nil {
							t.Fatalf("unexpected error: %v", err)
						}

						var returnData []byte
						if existing == nil {
							uid := types.UID(fmt.Sprintf("%v", time.Now().UnixNano()))
							patch.SetUID(uid)

							if err := fakeDynamicClient.Tracker().Create(gvr, patch, namespace); err != nil {
								t.Fatalf("error creating object: %v", err)
							}

							b, err := json.Marshal(patch)
							if err != nil {
								t.Fatalf("error marshalling response: %v", err)
							}
							returnData = b
						} else {
							patch.DeepCopyInto(existing)
							if err := fakeDynamicClient.Tracker().Update(gvr, existing, namespace); err != nil {
								t.Fatalf("error updating object: %v", err)
							}
							b, err := json.Marshal(existing)
							if err != nil {
								t.Fatalf("error marshalling response: %v", err)
							}
							returnData = b
						}

						return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: io.NopCloser(bytes.NewReader(returnData))}, nil
					}

					if len(tokens) == 1 && tokens[0] == "namespaces" && method == "POST" {
						data, err := io.ReadAll(req.Body)
						if err != nil {
							t.Fatalf("unexpected error: %v", err)
						}

						u := &unstructured.Unstructured{}
						if err := runtime.DecodeInto(codec, data, u); err != nil {
							t.Fatalf("unexpected error: %v", err)
						}

						name := u.GetName()
						ns := u.GetNamespace()
						gvr := schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}

						existing, err := fakeDynamicClient.Tracker().Get(gvr, ns, name)
						if err != nil {
							if apierrors.IsNotFound(err) {
								existing = nil
							} else {
								t.Fatalf("error fetching object: %v", err)
							}
						}

						if existing != nil {
							return &http.Response{StatusCode: http.StatusConflict, Header: cmdtesting.DefaultHeader()}, nil
						}

						uid := types.UID(fmt.Sprintf("%v", time.Now().UnixNano()))
						u.SetUID(uid)

						if err := fakeDynamicClient.Tracker().Create(gvr, u, ns); err != nil {
							t.Fatalf("error creating object: %v", err)
						}

						body := cmdtesting.ObjBody(codec, u)

						return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: body}, nil
					}

					t.Fatalf("unexpected request: %v %v\n%#v", req.Method, req.URL, req)
					return nil, nil
				}),
			}

			tf.Client = tf.UnstructuredClient
			tf.OpenAPIV3ClientFunc = FakeOpenAPISchema.OpenAPIV3ClientFunc

			cmdtesting.WithAlphaEnvs([]cmdutil.FeatureGate{cmdutil.ApplySet}, t, func(t *testing.T) {
				manifests := []string{"manifest1", "manifest2"}
				for _, manifest := range manifests {
					t.Logf("applying manifest %v", manifest)

					cmd := &cobra.Command{}
					flags := NewApplyFlags(genericiooptions.NewTestIOStreamsDiscard())
					flags.AddFlags(cmd)
					cmd.Flags().Set("filename", filepath.Join(testdir, manifest+".yaml"))
					cmd.Flags().Set("applyset", filepath.Base(testdir))
					cmd.Flags().Set("prune", "true")
					cmd.Flags().Set("validate", "false")

					o, err := flags.ToOptions(tf, cmd, "kubectl", []string{})
					if err != nil {
						t.Fatalf("unexpected error creating apply options: %v", err)
					}

					err = o.Validate()
					if err != nil {
						t.Fatalf("unexpected error from validate: %v", err)
					}

					var unifiedOutput bytes.Buffer
					o.Out = &unifiedOutput
					o.ErrOut = &unifiedOutput

					if err := o.Run(); err != nil {
						t.Errorf("error running apply: %v", err)
					}

					got := unifiedOutput.String()

					p := filepath.Join(testdir, manifest+"-expected-apply.txt")
					wantBytes, err := os.ReadFile(p)
					if err != nil {
						t.Fatalf("error reading file %q: %v", p, err)
					}
					want := string(wantBytes)
					if diff := cmp.Diff(want, got); diff != "" {
						t.Errorf("apply output has unexpected diff (-want +got):\n%s", diff)
					}
				}
			})
		})
	}
}

func TestApplySetUpdateConflictsAreRetried(t *testing.T) {
	nameParentSecret := "my-set"
	pathSecret := "/namespaces/test/secrets/" + nameParentSecret
	secretYaml := `apiVersion: v1
kind: Secret
metadata:
  annotations:
    applyset.kubernetes.io/additional-namespaces: ""
    applyset.kubernetes.io/contains-group-resources: replicationcontrollers
    applyset.kubernetes.io/tooling: kubectl/v0.0.0-master+$Format:%H$
  creationTimestamp: null
  labels:
    applyset.kubernetes.io/id: applyset-0eFHV8ySqp7XoShsGvyWFQD3s96yqwHmzc4e0HR1dsY-v1
  name: my-set
  namespace: test
`
	tf := cmdtesting.NewTestFactory().WithNamespace("test")
	defer tf.Cleanup()

	applyReturnedConflict := false
	appliedWithConflictsForced := false
	tf.Client = &fake.RESTClient{
		NegotiatedSerializer: resource.UnstructuredPlusDefaultContentConfig().NegotiatedSerializer,
		Client: fake.CreateHTTPClient(func(req *http.Request) (*http.Response, error) {
			if req.Method == "GET" && req.URL.Path == pathSecret {
				data, err := yaml.YAMLToJSON([]byte(secretYaml))
				require.NoError(t, err)
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: io.NopCloser(bytes.NewReader(data))}, nil
			}

			contentType := req.Header.Get("Content-Type")
			forceConflicts := req.URL.Query().Get("force") == "true"
			if req.Method == "PATCH" && contentType == string(types.ApplyPatchType) {
				// make the ApplySet secret SSA request fail unless conflicts are forced
				if req.URL.Path == pathSecret {
					if !forceConflicts {
						applyReturnedConflict = true
						return &http.Response{StatusCode: http.StatusConflict, Header: cmdtesting.DefaultHeader(), Body: io.NopCloser(strings.NewReader("Apply failed with 1 conflict: conflict with \"other\": .metadata.annotations.applyset.kubernetes.io/contains-group-resources"))}, nil
					}
					appliedWithConflictsForced = true
				}
				data, err := io.ReadAll(req.Body)
				require.NoError(t, err)
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: io.NopCloser(bytes.NewReader(data))}, nil
			}
			t.Fatalf("unexpected request to %s\n%#v", req.URL.Path, req)
			return nil, nil
		}),
	}
	tf.UnstructuredClient = tf.Client

	ioStreams, _, outbuff, errbuff := genericiooptions.NewTestIOStreams()
	cmdutil.BehaviorOnFatal(fatalNoExit(t, ioStreams))
	defer cmdutil.DefaultBehaviorOnFatal()

	cmdtesting.WithAlphaEnvs([]cmdutil.FeatureGate{cmdutil.ApplySet}, t, func(t *testing.T) {
		cmd := NewCmdApply("kubectl", tf, ioStreams)
		cmd.Flags().Set("filename", filenameRC)
		cmd.Flags().Set("server-side", "true")
		cmd.Flags().Set("applyset", nameParentSecret)
		cmd.Flags().Set("prune", "true")
		cmd.Run(cmd, []string{})
	})
	assert.Equal(t, "replicationcontroller/test-rc serverside-applied\n", outbuff.String())
	assert.Equal(t, "", errbuff.String())
	assert.Truef(t, applyReturnedConflict, "test did not simulate a conflict scenario")
	assert.Truef(t, appliedWithConflictsForced, "conflicts were never forced")
}

func TestApplyWithPruneV2Fail(t *testing.T) {
	tf := cmdtesting.NewTestFactory().WithNamespace("test")
	defer tf.Cleanup()

	codec := scheme.Codecs.LegacyCodec(scheme.Scheme.PrioritizedVersionsAllGroups()...)

	scheme := runtime.NewScheme()

	listMapping := map[schema.GroupVersionResource]string{
		{Group: "", Version: "v1", Resource: "namespaces"}: "NamespaceList",
	}

	fakeDynamicClient := dynamicfakeclient.NewSimpleDynamicClientWithCustomListKinds(scheme, listMapping)
	tf.FakeDynamicClient = fakeDynamicClient

	failDelete := false
	fakeDynamicClient.PrependReactor("delete", "*", func(action testing2.Action) (handled bool, ret runtime.Object, err error) {
		if failDelete {
			return true, nil, fmt.Errorf("an error on the server (\"\") has prevented the request from succeeding")
		}
		return false, nil, nil
	})

	tf.UnstructuredClient = &fake.RESTClient{
		NegotiatedSerializer: resource.UnstructuredPlusDefaultContentConfig().NegotiatedSerializer,
		Client: fake.CreateHTTPClient(func(req *http.Request) (*http.Response, error) {
			method := req.Method

			tokens := strings.Split(strings.TrimPrefix(req.URL.Path, "/"), "/")

			if len(tokens) == 2 && tokens[0] == "namespaces" && method == "GET" {
				name := tokens[1]
				gvr := schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}
				ns, err := fakeDynamicClient.Tracker().Get(gvr, "", name)
				if err != nil {
					if apierrors.IsNotFound(err) {
						return &http.Response{StatusCode: http.StatusNotFound, Header: cmdtesting.DefaultHeader()}, nil
					}
					t.Fatalf("error getting object: %v", err)
				}
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, ns)}, nil
			}

			if len(tokens) == 4 && tokens[0] == "namespaces" && tokens[2] == "secrets" && method == "GET" {
				namespace := tokens[1]
				name := tokens[3]
				gvr := schema.GroupVersionResource{Version: "v1", Resource: "secrets"}
				obj, err := fakeDynamicClient.Tracker().Get(gvr, namespace, name)
				if err != nil {
					if apierrors.IsNotFound(err) {
						return &http.Response{StatusCode: http.StatusNotFound, Header: cmdtesting.DefaultHeader()}, nil
					}
					t.Fatalf("error getting object: %v", err)
				}
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, obj)}, nil
			}

			if len(tokens) == 4 && tokens[0] == "namespaces" && tokens[2] == "secrets" && method == "PATCH" {
				namespace := tokens[1]
				name := tokens[3]
				gvr := schema.GroupVersionResource{Version: "v1", Resource: "secrets"}
				var existing *unstructured.Unstructured
				existingObj, err := fakeDynamicClient.Tracker().Get(gvr, namespace, name)
				if err != nil {
					if !apierrors.IsNotFound(err) {
						t.Fatalf("error getting object: %v", err)
					}
				} else {
					existing = existingObj.(*unstructured.Unstructured)
				}

				data, err := io.ReadAll(req.Body)
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}

				patch := &unstructured.Unstructured{}
				if err := runtime.DecodeInto(codec, data, patch); err != nil {
					t.Fatalf("unexpected error: %v", err)
				}

				var returnData []byte
				if existing == nil {
					uid := types.UID(fmt.Sprintf("%v", time.Now().UnixNano()))
					patch.SetUID(uid)

					if err := fakeDynamicClient.Tracker().Create(gvr, patch, namespace); err != nil {
						t.Fatalf("error creating object: %v", err)
					}

					b, err := json.Marshal(patch)
					if err != nil {
						t.Fatalf("error marshalling response: %v", err)
					}
					returnData = b
				} else {
					patch.DeepCopyInto(existing)
					if err := fakeDynamicClient.Tracker().Update(gvr, existing, namespace); err != nil {
						t.Fatalf("error updating object: %v", err)
					}
					b, err := json.Marshal(existing)
					if err != nil {
						t.Fatalf("error marshalling response: %v", err)
					}
					returnData = b
				}

				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: io.NopCloser(bytes.NewReader(returnData))}, nil
			}

			if len(tokens) == 1 && tokens[0] == "namespaces" && method == "POST" {
				data, err := io.ReadAll(req.Body)
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}

				u := &unstructured.Unstructured{}
				if err := runtime.DecodeInto(codec, data, u); err != nil {
					t.Fatalf("unexpected error: %v", err)
				}

				name := u.GetName()
				ns := u.GetNamespace()
				gvr := schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}

				existing, err := fakeDynamicClient.Tracker().Get(gvr, ns, name)
				if err != nil {
					if apierrors.IsNotFound(err) {
						existing = nil
					} else {
						t.Fatalf("error fetching object: %v", err)
					}
				}

				if existing != nil {
					return &http.Response{StatusCode: http.StatusConflict, Header: cmdtesting.DefaultHeader()}, nil
				}

				uid := types.UID(fmt.Sprintf("%v", time.Now().UnixNano()))
				u.SetUID(uid)

				if err := fakeDynamicClient.Tracker().Create(gvr, u, ns); err != nil {
					t.Fatalf("error creating object: %v", err)
				}

				body := cmdtesting.ObjBody(codec, u)

				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: body}, nil
			}

			t.Fatalf("unexpected request: %v %v\n%#v", req.Method, req.URL, req)
			return nil, nil
		}),
	}

	tf.Client = tf.UnstructuredClient
	tf.OpenAPIV3ClientFunc = FakeOpenAPISchema.OpenAPIV3ClientFunc

	testdirs := []string{"testdata/prune/simple"}
	for _, testdir := range testdirs {
		t.Run(testdir, func(t *testing.T) {
			cmdtesting.WithAlphaEnvs([]cmdutil.FeatureGate{cmdutil.ApplySet}, t, func(t *testing.T) {
				manifests := []string{"manifest1", "manifest2"}
				for i, manifest := range manifests {
					if i != 0 {
						t.Logf("will inject failures into future delete operations")
						failDelete = true
					}
					t.Logf("applying manifest %v", manifest)

					var unifiedOutput bytes.Buffer
					ioStreams := genericiooptions.IOStreams{
						ErrOut: &unifiedOutput,
						Out:    &unifiedOutput,
						In:     bytes.NewBufferString(""),
					}
					cmdutil.BehaviorOnFatal(fatalNoExit(t, ioStreams))
					defer cmdutil.DefaultBehaviorOnFatal()

					rootCmd := &cobra.Command{
						Use: "kubectl",
					}
					kubeConfigFlags := genericclioptions.NewConfigFlags(true).WithDeprecatedPasswordFlag().WithDiscoveryBurst(300).WithDiscoveryQPS(50.0)
					kubeConfigFlags.AddFlags(rootCmd.PersistentFlags())

					applyCmd := NewCmdApply("kubectl", tf, ioStreams)
					rootCmd.AddCommand(applyCmd)

					rootCmd.SetArgs([]string{
						"apply",
						"--filename=" + filepath.Join(testdir, manifest+".yaml"),
						"--applyset=" + filepath.Base(testdir),
						"--namespace=default",
						"--prune=true",
						"--validate=false",
					})
					if err := rootCmd.Execute(); err != nil {
						t.Errorf("error running apply command: %v", err)
					}

					got := unifiedOutput.String()

					p := filepath.Join(testdir, "scenarios", "error-on-apply", manifest+"-expected-apply.txt")
					wantBytes, err := os.ReadFile(p)
					if err != nil {
						t.Fatalf("error reading file %q: %v", p, err)
					}
					want := string(wantBytes)
					if diff := cmp.Diff(want, got); diff != "" {
						t.Errorf("apply output has unexpected diff (-want +got):\n%s", diff)
					}
				}
			})
		})
	}
}

// fatalNoExit is a handler that replaces the default cmdutil.BehaviorOnFatal,
// that still prints as expected, but does not call os.Exit (which terminates our tests)
func fatalNoExit(t *testing.T, ioStreams genericiooptions.IOStreams) func(msg string, code int) {
	return func(msg string, code int) {
		if len(msg) > 0 {
			// add newline if needed
			if !strings.HasSuffix(msg, "\n") {
				msg += "\n"
			}
			fmt.Fprint(ioStreams.ErrOut, msg)
		}
	}
}

func TestApplySetDryRun(t *testing.T) {
	cmdtesting.InitTestErrorHandler(t)
	nameRC, rc := readReplicationController(t, filenameRC)
	pathRC := "/namespaces/test/replicationcontrollers/" + nameRC
	nameParentSecret := "my-set"
	pathSecret := "/namespaces/test/secrets/" + nameParentSecret

	tf := cmdtesting.NewTestFactory().WithNamespace("test")
	defer tf.Cleanup()

	// Scenario: the rc 'exists' server side but the applyset secret does not
	// In dry run mode, non-dry run patch requests should not be made, and the secret should not be created
	serverSideData := map[string][]byte{
		pathRC: rc,
	}
	fakeDryRunClient := func(t *testing.T, allowPatch bool) *fake.RESTClient {
		return &fake.RESTClient{
			NegotiatedSerializer: resource.UnstructuredPlusDefaultContentConfig().NegotiatedSerializer,
			Client: fake.CreateHTTPClient(func(req *http.Request) (*http.Response, error) {
				if req.Method == "GET" {
					data, ok := serverSideData[req.URL.Path]
					if !ok {
						return &http.Response{StatusCode: http.StatusNotFound, Header: cmdtesting.DefaultHeader(), Body: io.NopCloser(bytes.NewReader(nil))}, nil
					}
					return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: io.NopCloser(bytes.NewReader(data))}, nil
				}
				if req.Method == "PATCH" && allowPatch && req.URL.Query().Get("dryRun") == "All" {
					data, err := io.ReadAll(req.Body)
					require.NoError(t, err)
					return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: io.NopCloser(bytes.NewReader(data))}, nil
				}

				t.Fatalf("unexpected request: to %s\n%#v", req.URL.Path, req)
				return nil, nil
			}),
		}
	}

	t.Run("server side dry run", func(t *testing.T) {
		ioStreams, _, outbuff, _ := genericiooptions.NewTestIOStreams()
		tf.Client = fakeDryRunClient(t, true)
		tf.UnstructuredClient = tf.Client
		cmdtesting.WithAlphaEnvs([]cmdutil.FeatureGate{cmdutil.ApplySet}, t, func(t *testing.T) {
			cmd := NewCmdApply("kubectl", tf, ioStreams)
			cmd.Flags().Set("filename", filenameRC)
			cmd.Flags().Set("server-side", "true")
			cmd.Flags().Set("applyset", nameParentSecret)
			cmd.Flags().Set("prune", "true")
			cmd.Flags().Set("dry-run", "server")
			cmd.Run(cmd, []string{})
		})
		assert.Equal(t, "replicationcontroller/test-rc serverside-applied (server dry run)\n", outbuff.String())
		assert.Len(t, serverSideData, 1, "unexpected creation")
		require.Nil(t, serverSideData[pathSecret], "secret was created")
	})

	t.Run("client side dry run", func(t *testing.T) {
		ioStreams, _, outbuff, _ := genericiooptions.NewTestIOStreams()
		tf.Client = fakeDryRunClient(t, false)
		tf.UnstructuredClient = tf.Client
		cmdtesting.WithAlphaEnvs([]cmdutil.FeatureGate{cmdutil.ApplySet}, t, func(t *testing.T) {
			cmd := NewCmdApply("kubectl", tf, ioStreams)
			cmd.Flags().Set("filename", filenameRC)
			cmd.Flags().Set("applyset", nameParentSecret)
			cmd.Flags().Set("prune", "true")
			cmd.Flags().Set("dry-run", "client")
			cmd.Run(cmd, []string{})
		})
		assert.Equal(t, "replicationcontroller/test-rc configured (dry run)\n", outbuff.String())
		assert.Len(t, serverSideData, 1, "unexpected creation")
		require.Nil(t, serverSideData[pathSecret], "secret was created")
	})
}
