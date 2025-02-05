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

package delete

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	"k8s.io/cli-runtime/pkg/resource"
	"k8s.io/client-go/rest/fake"
	cmdtesting "k8s.io/kubectl/pkg/cmd/testing"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/scheme"
	"k8s.io/utils/ptr"
)

func fakecmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:                   "delete ([-f FILENAME] | TYPE [(NAME | -l label | --all)])",
		DisableFlagsInUseLine: true,
		Run:                   func(cmd *cobra.Command, args []string) {},
	}
	cmdutil.AddDryRunFlag(cmd)
	return cmd
}

func TestDeleteFlagValidation(t *testing.T) {
	f := cmdtesting.NewTestFactory()
	defer f.Cleanup()

	tests := []struct {
		flags       DeleteFlags
		args        [][]string
		expectedErr string
	}{
		{
			flags: DeleteFlags{
				Raw:         ptr.To("test"),
				Interactive: ptr.To(true),
			},
			expectedErr: "--interactive can not be used with --raw",
		},
		{
			flags: DeleteFlags{
				All:         ptr.To(true),
				Concurrent:  ptr.To(true),
				Interactive: ptr.To(true),
			},
			expectedErr: "cannot set --concurrent and --interactive at the same time",
		},
	}

	for _, test := range tests {
		cmd := fakecmd()
		deleteOptions, err := test.flags.ToOptions(nil, genericiooptions.NewTestIOStreamsDiscard())
		if err != nil {
			t.Fatalf("unexpected error creating delete options: %s", err)
		}
		deleteOptions.Filenames = []string{"../../../testdata/redis-master-controller.yaml"}
		err = deleteOptions.Complete(f, nil, cmd)
		if err != nil {
			t.Fatalf("unexpected error creating delete options: %s", err)
		}
		err = deleteOptions.Validate()
		if err == nil {
			t.Fatalf("missing expected error")
		}
		if test.expectedErr != err.Error() {
			t.Errorf("expected error %s, got %s", test.expectedErr, err)
		}
	}
}

func TestDeleteObjectByTuple(t *testing.T) {
	cmdtesting.InitTestErrorHandler(t)
	_, _, rc := cmdtesting.TestData()

	tf := cmdtesting.NewTestFactory().WithNamespace("test")
	defer tf.Cleanup()

	codec := scheme.Codecs.LegacyCodec(scheme.Scheme.PrioritizedVersionsAllGroups()...)

	tf.UnstructuredClient = &fake.RESTClient{
		NegotiatedSerializer: resource.UnstructuredPlusDefaultContentConfig().NegotiatedSerializer,
		Client: fake.CreateHTTPClient(func(req *http.Request) (*http.Response, error) {
			switch p, m := req.URL.Path, req.Method; {

			// replication controller with cascade off
			case p == "/namespaces/test/replicationcontrollers/redis-master-controller" && m == "DELETE":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, &rc.Items[0])}, nil

			// secret with cascade on, but no client-side reaper
			case p == "/namespaces/test/secrets/mysecret" && m == "DELETE":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, &rc.Items[0])}, nil

			default:
				// Ensures no GET is performed when deleting by name
				t.Fatalf("unexpected request: %#v\n%#v", req.URL, req)
				return nil, nil
			}
		}),
	}

	streams, _, buf, _ := genericiooptions.NewTestIOStreams()
	cmd := NewCmdDelete(tf, streams)
	cmd.Flags().Set("namespace", "test")
	cmd.Flags().Set("cascade", "false")
	cmd.Flags().Set("output", "name")
	cmd.Run(cmd, []string{"replicationcontrollers/redis-master-controller"})
	if buf.String() != "replicationcontroller/redis-master-controller\n" {
		t.Errorf("unexpected output: %s", buf.String())
	}

	// Test cascading delete of object without client-side reaper doesn't make GET requests
	streams, _, buf, _ = genericiooptions.NewTestIOStreams()
	cmd = NewCmdDelete(tf, streams)
	cmd.Flags().Set("namespace", "test")
	cmd.Flags().Set("output", "name")
	cmd.Run(cmd, []string{"secrets/mysecret"})
	if buf.String() != "secret/mysecret\n" {
		t.Errorf("unexpected output: %s", buf.String())
	}
}

func hasExpectedPropagationPolicy(body io.ReadCloser, policy *metav1.DeletionPropagation) bool {
	if body == nil || policy == nil {
		return body == nil && policy == nil
	}
	var parsedBody metav1.DeleteOptions
	rawBody, _ := io.ReadAll(body)
	json.Unmarshal(rawBody, &parsedBody)
	if parsedBody.PropagationPolicy == nil {
		return false
	}
	return *policy == *parsedBody.PropagationPolicy
}

// TestCascadingStrategy tests that DeleteOptions.DeletionPropagation is appropriately set while deleting objects.
func TestCascadingStrategy(t *testing.T) {
	cmdtesting.InitTestErrorHandler(t)
	_, _, rc := cmdtesting.TestData()

	tf := cmdtesting.NewTestFactory().WithNamespace("test")
	defer tf.Cleanup()

	codec := scheme.Codecs.LegacyCodec(scheme.Scheme.PrioritizedVersionsAllGroups()...)

	var policy *metav1.DeletionPropagation
	tf.UnstructuredClient = &fake.RESTClient{
		NegotiatedSerializer: resource.UnstructuredPlusDefaultContentConfig().NegotiatedSerializer,
		Client: fake.CreateHTTPClient(func(req *http.Request) (*http.Response, error) {
			switch p, m, b := req.URL.Path, req.Method, req.Body; {

			case p == "/namespaces/test/secrets/mysecret" && m == "DELETE" && hasExpectedPropagationPolicy(b, policy):

				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, &rc.Items[0])}, nil
			default:
				return nil, nil
			}
		}),
	}

	// DeleteOptions.PropagationPolicy should be Background, when cascading strategy is empty (default).
	backgroundPolicy := metav1.DeletePropagationBackground
	policy = &backgroundPolicy
	streams, _, buf, _ := genericiooptions.NewTestIOStreams()
	cmd := NewCmdDelete(tf, streams)
	cmd.Flags().Set("namespace", "test")
	cmd.Flags().Set("output", "name")
	cmd.Run(cmd, []string{"secrets/mysecret"})
	if buf.String() != "secret/mysecret\n" {
		t.Errorf("unexpected output: %s", buf.String())
	}

	// DeleteOptions.PropagationPolicy should be Foreground, when cascading strategy is foreground.
	foregroundPolicy := metav1.DeletePropagationForeground
	policy = &foregroundPolicy
	streams, _, buf, _ = genericiooptions.NewTestIOStreams()
	cmd = NewCmdDelete(tf, streams)
	cmd.Flags().Set("namespace", "test")
	cmd.Flags().Set("cascade", "foreground")
	cmd.Flags().Set("output", "name")
	cmd.Run(cmd, []string{"secrets/mysecret"})
	if buf.String() != "secret/mysecret\n" {
		t.Errorf("unexpected output: %s", buf.String())
	}

	// Test that delete options should be set to orphan when cascading strategy is orphan.
	orphanPolicy := metav1.DeletePropagationOrphan
	policy = &orphanPolicy
	streams, _, buf, _ = genericiooptions.NewTestIOStreams()
	cmd = NewCmdDelete(tf, streams)
	cmd.Flags().Set("namespace", "test")
	cmd.Flags().Set("cascade", "orphan")
	cmd.Flags().Set("output", "name")
	cmd.Run(cmd, []string{"secrets/mysecret"})
	if buf.String() != "secret/mysecret\n" {
		t.Errorf("unexpected output: %s", buf.String())
	}
}

func TestDeleteNamedObject(t *testing.T) {
	cmdtesting.InitTestErrorHandler(t)
	cmdtesting.InitTestErrorHandler(t)
	_, _, rc := cmdtesting.TestData()

	tf := cmdtesting.NewTestFactory().WithNamespace("test")
	defer tf.Cleanup()

	codec := scheme.Codecs.LegacyCodec(scheme.Scheme.PrioritizedVersionsAllGroups()...)

	tf.UnstructuredClient = &fake.RESTClient{
		NegotiatedSerializer: resource.UnstructuredPlusDefaultContentConfig().NegotiatedSerializer,
		Client: fake.CreateHTTPClient(func(req *http.Request) (*http.Response, error) {
			switch p, m := req.URL.Path, req.Method; {

			// replication controller with cascade off
			case p == "/namespaces/test/replicationcontrollers/redis-master-controller" && m == "DELETE":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, &rc.Items[0])}, nil

			// secret with cascade on, but no client-side reaper
			case p == "/namespaces/test/secrets/mysecret" && m == "DELETE":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, &rc.Items[0])}, nil

			default:
				// Ensures no GET is performed when deleting by name
				t.Fatalf("unexpected request: %#v\n%#v", req.URL, req)
				return nil, nil
			}
		}),
	}

	streams, _, buf, _ := genericiooptions.NewTestIOStreams()
	cmd := NewCmdDelete(tf, streams)
	cmd.Flags().Set("namespace", "test")
	cmd.Flags().Set("cascade", "false")
	cmd.Flags().Set("output", "name")
	cmd.Run(cmd, []string{"replicationcontrollers", "redis-master-controller"})
	if buf.String() != "replicationcontroller/redis-master-controller\n" {
		t.Errorf("unexpected output: %s", buf.String())
	}

	// Test cascading delete of object without client-side reaper doesn't make GET requests
	streams, _, buf, _ = genericiooptions.NewTestIOStreams()
	cmd = NewCmdDelete(tf, streams)
	cmd.Flags().Set("namespace", "test")
	cmd.Flags().Set("cascade", "false")
	cmd.Flags().Set("output", "name")
	cmd.Run(cmd, []string{"secrets", "mysecret"})
	if buf.String() != "secret/mysecret\n" {
		t.Errorf("unexpected output: %s", buf.String())
	}
}

func TestDeleteObject(t *testing.T) {
	cmdtesting.InitTestErrorHandler(t)
	_, _, rc := cmdtesting.TestData()

	tf := cmdtesting.NewTestFactory().WithNamespace("test")
	defer tf.Cleanup()

	codec := scheme.Codecs.LegacyCodec(scheme.Scheme.PrioritizedVersionsAllGroups()...)

	tf.UnstructuredClient = &fake.RESTClient{
		NegotiatedSerializer: resource.UnstructuredPlusDefaultContentConfig().NegotiatedSerializer,
		Client: fake.CreateHTTPClient(func(req *http.Request) (*http.Response, error) {
			switch p, m := req.URL.Path, req.Method; {
			case p == "/namespaces/test/replicationcontrollers/redis-master" && m == "DELETE":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, &rc.Items[0])}, nil
			default:
				t.Fatalf("unexpected request: %#v\n%#v", req.URL, req)
				return nil, nil
			}
		}),
	}

	streams, _, buf, _ := genericiooptions.NewTestIOStreams()
	cmd := NewCmdDelete(tf, streams)
	cmd.Flags().Set("filename", "../../../testdata/redis-master-controller.yaml")
	cmd.Flags().Set("cascade", "false")
	cmd.Flags().Set("output", "name")
	cmd.Run(cmd, []string{})

	// uses the name from the file, not the response
	if buf.String() != "replicationcontroller/redis-master\n" {
		t.Errorf("unexpected output: %s", buf.String())
	}
}

func TestPreviewResultEqualToResult(t *testing.T) {
	deleteFlags := NewDeleteCommandFlags("")
	deleteFlags.Interactive = ptr.To(true)

	tf := cmdtesting.NewTestFactory().WithNamespace("test")
	defer tf.Cleanup()

	streams, _, _, _ := genericiooptions.NewTestIOStreams()

	deleteOptions, err := deleteFlags.ToOptions(nil, streams)
	deleteOptions.Filenames = []string{"../../../testdata/redis-master-controller.yaml"}
	if err != nil {
		t.Errorf("unexpected error %v", err)
	}
	err = deleteOptions.Complete(tf, nil, fakecmd())
	if err != nil {
		t.Errorf("unexpected error %v", err)
	}

	infos, err := deleteOptions.Result.Infos()
	if err != nil {
		t.Errorf("unexpected error %v", err)
	}
	previewInfos, err := deleteOptions.PreviewResult.Infos()
	if err != nil {
		t.Errorf("unexpected error %v", err)
	}
	if len(infos) != len(previewInfos) {
		t.Errorf("result and previewResult must match")
	}
}

func TestDeleteObjectWithInteractive(t *testing.T) {
	cmdtesting.InitTestErrorHandler(t)
	_, _, rc := cmdtesting.TestData()

	tf := cmdtesting.NewTestFactory().WithNamespace("test")
	defer tf.Cleanup()

	codec := scheme.Codecs.LegacyCodec(scheme.Scheme.PrioritizedVersionsAllGroups()...)

	tf.UnstructuredClient = &fake.RESTClient{
		NegotiatedSerializer: resource.UnstructuredPlusDefaultContentConfig().NegotiatedSerializer,
		Client: fake.CreateHTTPClient(func(req *http.Request) (*http.Response, error) {
			switch p, m := req.URL.Path, req.Method; {
			case p == "/namespaces/test/replicationcontrollers/redis-master" && m == "DELETE":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, &rc.Items[0])}, nil
			default:
				t.Fatalf("unexpected request: %#v\n%#v", req.URL, req)
				return nil, nil
			}
		}),
	}

	streams, in, buf, _ := genericiooptions.NewTestIOStreams()
	fmt.Fprint(in, "y")
	cmd := NewCmdDelete(tf, streams)
	err := cmd.Flags().Set("filename", "../../../testdata/redis-master-controller.yaml")
	if err != nil {
		t.Errorf("unexpected error %v", err)
	}
	err = cmd.Flags().Set("output", "name")
	if err != nil {
		t.Errorf("unexpected error %v", err)
	}
	err = cmd.Flags().Set("interactive", "true")
	if err != nil {
		t.Errorf("unexpected error %v", err)
	}
	cmd.Run(cmd, []string{})

	if buf.String() != "You are about to delete the following 1 resource(s):\nreplicationcontroller/redis-master\nDo you want to continue? (y/n): replicationcontroller/redis-master\n" {
		t.Errorf("unexpected output: %s", buf.String())
	}

	streams, in, buf, _ = genericiooptions.NewTestIOStreams()
	fmt.Fprint(in, "n")
	cmd = NewCmdDelete(tf, streams)
	err = cmd.Flags().Set("filename", "../../../testdata/redis-master-controller.yaml")
	if err != nil {
		t.Errorf("unexpected error %v", err)
	}
	err = cmd.Flags().Set("output", "name")
	if err != nil {
		t.Errorf("unexpected error %v", err)
	}
	err = cmd.Flags().Set("interactive", "true")
	if err != nil {
		t.Errorf("unexpected error %v", err)
	}
	cmd.Run(cmd, []string{})

	if buf.String() != "You are about to delete the following 1 resource(s):\nreplicationcontroller/redis-master\nDo you want to continue? (y/n): deletion is cancelled\n" {
		t.Errorf("unexpected output: %s", buf.String())
	}
	if buf.String() == ": replicationcontroller/redis-master\n" {
		t.Errorf("unexpected output: %s", buf.String())
	}
}

func TestGracePeriodScenarios(t *testing.T) {
	pods, _, _ := cmdtesting.TestData()

	tf := cmdtesting.NewTestFactory().WithNamespace("test")
	defer tf.Cleanup()

	codec := scheme.Codecs.LegacyCodec(scheme.Scheme.PrioritizedVersionsAllGroups()...)

	tc := []struct {
		name                      string
		cmdArgs                   []string
		forceFlag                 bool
		nowFlag                   bool
		gracePeriodFlag           string
		expectedGracePeriod       string
		expectedOut               string
		expectedErrOut            string
		expectedDeleteRequestPath string
		expectedExitCode          int
	}{
		{
			name:                      "Deleting an object with --force should use grace period = 0",
			cmdArgs:                   []string{"pods/foo"},
			forceFlag:                 true,
			expectedGracePeriod:       "0",
			expectedOut:               "pod/foo\n",
			expectedErrOut:            "Warning: Immediate deletion does not wait for confirmation that the running resource has been terminated. The resource may continue to run on the cluster indefinitely.\n",
			expectedDeleteRequestPath: "/namespaces/test/pods/foo",
		},
		{
			name:                      "Deleting an object with --force and --grace-period 0 should use grade period = 0",
			cmdArgs:                   []string{"pods/foo"},
			forceFlag:                 true,
			gracePeriodFlag:           "0",
			expectedGracePeriod:       "0",
			expectedOut:               "pod/foo\n",
			expectedErrOut:            "Warning: Immediate deletion does not wait for confirmation that the running resource has been terminated. The resource may continue to run on the cluster indefinitely.\n",
			expectedDeleteRequestPath: "/namespaces/test/pods/foo",
		},
		{
			name:             "Deleting an object with --force and --grace-period > 0 should fail",
			cmdArgs:          []string{"pods/foo"},
			forceFlag:        true,
			gracePeriodFlag:  "10",
			expectedErrOut:   "error: --force and --grace-period greater than 0 cannot be specified together",
			expectedExitCode: 1,
		},
		{
			name:                      "Deleting an object with --grace-period 0 should use a grace period of 1",
			cmdArgs:                   []string{"pods/foo"},
			gracePeriodFlag:           "0",
			expectedGracePeriod:       "1",
			expectedOut:               "pod/foo\n",
			expectedDeleteRequestPath: "/namespaces/test/pods/foo",
		},
		{
			name:                      "Deleting an object with --grace-period > 0 should use the specified grace period",
			cmdArgs:                   []string{"pods/foo"},
			gracePeriodFlag:           "10",
			expectedGracePeriod:       "10",
			expectedOut:               "pod/foo\n",
			expectedDeleteRequestPath: "/namespaces/test/pods/foo",
		},
		{
			name:                      "Deleting an object with the --now flag should use grace period = 1",
			cmdArgs:                   []string{"pods/foo"},
			nowFlag:                   true,
			expectedGracePeriod:       "1",
			expectedOut:               "pod/foo\n",
			expectedDeleteRequestPath: "/namespaces/test/pods/foo",
		},
		{
			name:             "Deleting an object with --now and --grace-period should fail",
			cmdArgs:          []string{"pods/foo"},
			nowFlag:          true,
			gracePeriodFlag:  "10",
			expectedErrOut:   "error: --now and --grace-period cannot be specified together",
			expectedExitCode: 1,
		},
	}

	for _, test := range tc {
		t.Run(test.name, func(t *testing.T) {

			// Use a custom fatal behavior with panic/recover so that we can test failure scenarios where
			// os.Exit() would normally be called
			cmdutil.BehaviorOnFatal(func(actualErrOut string, actualExitCode int) {
				if test.expectedExitCode != actualExitCode {
					t.Errorf("unexpected exit code:\n\tExpected: %d\n\tActual:   %d\n", test.expectedExitCode, actualExitCode)
				}
				if test.expectedErrOut != actualErrOut {
					t.Errorf("unexpected error:\n\tExpected: %s\n\tActual:   %s\n", test.expectedErrOut, actualErrOut)
				}
				panic(nil)
			})
			defer func() {
				if test.expectedExitCode != 0 {
					recover()
				}
			}()

			// Setup a fake HTTP Client to capture whether a delete request was made or not and if so,
			// the actual grace period that was used.
			actualGracePeriod := ""
			deleteOccurred := false
			tf.UnstructuredClient = &fake.RESTClient{
				NegotiatedSerializer: resource.UnstructuredPlusDefaultContentConfig().NegotiatedSerializer,
				Client: fake.CreateHTTPClient(func(req *http.Request) (*http.Response, error) {
					switch p, m := req.URL.Path, req.Method; {
					case m == "DELETE" && p == test.expectedDeleteRequestPath:
						data := make(map[string]interface{})
						_ = json.NewDecoder(req.Body).Decode(&data)
						actualGracePeriod = strconv.FormatFloat(data["gracePeriodSeconds"].(float64), 'f', 0, 64)
						deleteOccurred = true
						return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, &pods.Items[0])}, nil
					default:
						t.Fatalf("unexpected request: %#v\n%#v", req.URL, req)
						return nil, nil
					}
				}),
			}

			// Test the command using the flags specified in the test case
			streams, _, out, errOut := genericiooptions.NewTestIOStreams()
			cmd := NewCmdDelete(tf, streams)
			cmd.Flags().Set("output", "name")
			if test.forceFlag {
				cmd.Flags().Set("force", "true")
			}
			if test.nowFlag {
				cmd.Flags().Set("now", "true")
			}
			if len(test.gracePeriodFlag) > 0 {
				cmd.Flags().Set("grace-period", test.gracePeriodFlag)
			}
			cmd.Run(cmd, test.cmdArgs)

			// Check actual vs expected conditions
			if len(test.expectedDeleteRequestPath) > 0 && !deleteOccurred {
				t.Errorf("expected http delete request to %s but it did not occur", test.expectedDeleteRequestPath)
			}
			if test.expectedGracePeriod != actualGracePeriod {
				t.Errorf("unexpected grace period:\n\tExpected: %s\n\tActual:   %s\n", test.expectedGracePeriod, actualGracePeriod)
			}
			if out.String() != test.expectedOut {
				t.Errorf("unexpected output:\n\tExpected: %s\n\tActual:   %s\n", test.expectedOut, out.String())
			}
			if errOut.String() != test.expectedErrOut {
				t.Errorf("unexpected error output:\n\tExpected: %s\n\tActual:   %s\n", test.expectedErrOut, errOut.String())
			}
		})
	}
}

func TestDeleteObjectNotFound(t *testing.T) {
	cmdtesting.InitTestErrorHandler(t)
	tf := cmdtesting.NewTestFactory().WithNamespace("test")
	defer tf.Cleanup()

	tf.UnstructuredClient = &fake.RESTClient{
		NegotiatedSerializer: resource.UnstructuredPlusDefaultContentConfig().NegotiatedSerializer,
		Client: fake.CreateHTTPClient(func(req *http.Request) (*http.Response, error) {
			switch p, m := req.URL.Path, req.Method; {
			case p == "/namespaces/test/replicationcontrollers/redis-master" && m == "DELETE":
				return &http.Response{StatusCode: http.StatusNotFound, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.StringBody("")}, nil
			default:
				t.Fatalf("unexpected request: %#v\n%#v", req.URL, req)
				return nil, nil
			}
		}),
	}

	options := &DeleteOptions{
		FilenameOptions: resource.FilenameOptions{
			Filenames: []string{"../../../testdata/redis-master-controller.yaml"},
		},
		GracePeriod:       -1,
		CascadingStrategy: metav1.DeletePropagationOrphan,
		Output:            "name",
		IOStreams:         genericiooptions.NewTestIOStreamsDiscard(),
	}
	err := options.Complete(tf, []string{}, fakecmd())
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	err = options.RunDelete(nil)
	if err == nil || !errors.IsNotFound(err) {
		t.Errorf("unexpected error: expected NotFound, got %v", err)
	}
}

func TestDeleteObjectIgnoreNotFound(t *testing.T) {
	cmdtesting.InitTestErrorHandler(t)
	tf := cmdtesting.NewTestFactory().WithNamespace("test")
	defer tf.Cleanup()

	tf.UnstructuredClient = &fake.RESTClient{
		NegotiatedSerializer: resource.UnstructuredPlusDefaultContentConfig().NegotiatedSerializer,
		Client: fake.CreateHTTPClient(func(req *http.Request) (*http.Response, error) {
			switch p, m := req.URL.Path, req.Method; {
			case p == "/namespaces/test/replicationcontrollers/redis-master" && m == "DELETE":
				return &http.Response{StatusCode: http.StatusNotFound, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.StringBody("")}, nil
			default:
				t.Fatalf("unexpected request: %#v\n%#v", req.URL, req)
				return nil, nil
			}
		}),
	}
	streams, _, buf, _ := genericiooptions.NewTestIOStreams()

	cmd := NewCmdDelete(tf, streams)
	cmd.Flags().Set("filename", "../../../testdata/redis-master-controller.yaml")
	cmd.Flags().Set("cascade", "false")
	cmd.Flags().Set("ignore-not-found", "true")
	cmd.Flags().Set("output", "name")
	cmd.Run(cmd, []string{})

	if buf.String() != "" {
		t.Errorf("unexpected output: %s", buf.String())
	}
}

func TestDeleteAllNotFound(t *testing.T) {
	cmdtesting.InitTestErrorHandler(t)
	_, svc, _ := cmdtesting.TestData()
	// Add an item to the list which will result in a 404 on delete
	svc.Items = append(svc.Items, corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "foo"}})
	notFoundError := &errors.NewNotFound(corev1.Resource("services"), "foo").ErrStatus

	tf := cmdtesting.NewTestFactory().WithNamespace("test")
	defer tf.Cleanup()

	codec := scheme.Codecs.LegacyCodec(scheme.Scheme.PrioritizedVersionsAllGroups()...)

	tf.UnstructuredClient = &fake.RESTClient{
		NegotiatedSerializer: resource.UnstructuredPlusDefaultContentConfig().NegotiatedSerializer,
		Client: fake.CreateHTTPClient(func(req *http.Request) (*http.Response, error) {
			switch p, m := req.URL.Path, req.Method; {
			case p == "/namespaces/test/services" && m == "GET":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, svc)}, nil
			case p == "/namespaces/test/services/foo" && m == "DELETE":
				return &http.Response{StatusCode: http.StatusNotFound, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, notFoundError)}, nil
			case p == "/namespaces/test/services/baz" && m == "DELETE":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, &svc.Items[0])}, nil
			default:
				t.Fatalf("unexpected request: %#v\n%#v", req.URL, req)
				return nil, nil
			}
		}),
	}

	// Make sure we can explicitly choose to fail on NotFound errors, even with --all
	options := &DeleteOptions{
		FilenameOptions:   resource.FilenameOptions{},
		GracePeriod:       -1,
		CascadingStrategy: metav1.DeletePropagationOrphan,
		DeleteAll:         true,
		IgnoreNotFound:    false,
		Output:            "name",
		IOStreams:         genericiooptions.NewTestIOStreamsDiscard(),
	}
	err := options.Complete(tf, []string{"services"}, fakecmd())
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	err = options.RunDelete(nil)
	if err == nil || !errors.IsNotFound(err) {
		t.Errorf("unexpected error: expected NotFound, got %v", err)
	}
}

func TestDeleteAllIgnoreNotFound(t *testing.T) {
	cmdtesting.InitTestErrorHandler(t)
	_, svc, _ := cmdtesting.TestData()

	tf := cmdtesting.NewTestFactory().WithNamespace("test")
	defer tf.Cleanup()

	codec := scheme.Codecs.LegacyCodec(scheme.Scheme.PrioritizedVersionsAllGroups()...)

	// Add an item to the list which will result in a 404 on delete
	svc.Items = append(svc.Items, corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "foo"}})
	notFoundError := &errors.NewNotFound(corev1.Resource("services"), "foo").ErrStatus

	tf.UnstructuredClient = &fake.RESTClient{
		NegotiatedSerializer: resource.UnstructuredPlusDefaultContentConfig().NegotiatedSerializer,
		Client: fake.CreateHTTPClient(func(req *http.Request) (*http.Response, error) {
			switch p, m := req.URL.Path, req.Method; {
			case p == "/namespaces/test/services" && m == "GET":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, svc)}, nil
			case p == "/namespaces/test/services/foo" && m == "DELETE":
				return &http.Response{StatusCode: http.StatusNotFound, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, notFoundError)}, nil
			case p == "/namespaces/test/services/baz" && m == "DELETE":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, &svc.Items[0])}, nil
			default:
				t.Fatalf("unexpected request: %#v\n%#v", req.URL, req)
				return nil, nil
			}
		}),
	}
	streams, _, buf, _ := genericiooptions.NewTestIOStreams()

	cmd := NewCmdDelete(tf, streams)
	cmd.Flags().Set("all", "true")
	cmd.Flags().Set("cascade", "false")
	cmd.Flags().Set("output", "name")
	cmd.Run(cmd, []string{"services"})

	if buf.String() != "service/baz\n" {
		t.Errorf("unexpected output: %s", buf.String())
	}
}

func TestDeleteMultipleObject(t *testing.T) {
	cmdtesting.InitTestErrorHandler(t)
	_, svc, rc := cmdtesting.TestData()

	tf := cmdtesting.NewTestFactory().WithNamespace("test")
	defer tf.Cleanup()

	codec := scheme.Codecs.LegacyCodec(scheme.Scheme.PrioritizedVersionsAllGroups()...)

	tf.UnstructuredClient = &fake.RESTClient{
		NegotiatedSerializer: resource.UnstructuredPlusDefaultContentConfig().NegotiatedSerializer,
		Client: fake.CreateHTTPClient(func(req *http.Request) (*http.Response, error) {
			switch p, m := req.URL.Path, req.Method; {
			case p == "/namespaces/test/replicationcontrollers/redis-master" && m == "DELETE":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, &rc.Items[0])}, nil
			case p == "/namespaces/test/services/frontend" && m == "DELETE":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, &svc.Items[0])}, nil
			default:
				t.Fatalf("unexpected request: %#v\n%#v", req.URL, req)
				return nil, nil
			}
		}),
	}
	streams, _, buf, _ := genericiooptions.NewTestIOStreams()

	cmd := NewCmdDelete(tf, streams)
	cmd.Flags().Set("filename", "../../../testdata/redis-master-controller.yaml")
	cmd.Flags().Set("filename", "../../../testdata/frontend-service.yaml")
	cmd.Flags().Set("cascade", "false")
	cmd.Flags().Set("output", "name")
	cmd.Run(cmd, []string{})

	if buf.String() != "replicationcontroller/redis-master\nservice/frontend\n" {
		t.Errorf("unexpected output: %s", buf.String())
	}
}

func TestDeleteMultipleObjectContinueOnMissing(t *testing.T) {
	cmdtesting.InitTestErrorHandler(t)
	_, svc, _ := cmdtesting.TestData()

	tf := cmdtesting.NewTestFactory().WithNamespace("test")
	defer tf.Cleanup()

	codec := scheme.Codecs.LegacyCodec(scheme.Scheme.PrioritizedVersionsAllGroups()...)

	tf.UnstructuredClient = &fake.RESTClient{
		NegotiatedSerializer: resource.UnstructuredPlusDefaultContentConfig().NegotiatedSerializer,
		Client: fake.CreateHTTPClient(func(req *http.Request) (*http.Response, error) {
			switch p, m := req.URL.Path, req.Method; {
			case p == "/namespaces/test/replicationcontrollers/redis-master" && m == "DELETE":
				return &http.Response{StatusCode: http.StatusNotFound, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.StringBody("")}, nil
			case p == "/namespaces/test/services/frontend" && m == "DELETE":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, &svc.Items[0])}, nil
			default:
				t.Fatalf("unexpected request: %#v\n%#v", req.URL, req)
				return nil, nil
			}
		}),
	}
	streams, _, buf, _ := genericiooptions.NewTestIOStreams()

	options := &DeleteOptions{
		FilenameOptions: resource.FilenameOptions{
			Filenames: []string{"../../../testdata/redis-master-controller.yaml", "../../../testdata/frontend-service.yaml"},
		},
		GracePeriod:       -1,
		CascadingStrategy: metav1.DeletePropagationOrphan,
		Output:            "name",
		IOStreams:         streams,
	}
	err := options.Complete(tf, []string{}, fakecmd())
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	err = options.RunDelete(nil)
	if err == nil || !errors.IsNotFound(err) {
		t.Errorf("unexpected error: expected NotFound, got %v", err)
	}

	if buf.String() != "service/frontend\n" {
		t.Errorf("unexpected output: %s", buf.String())
	}
}

func TestDeleteMultipleResourcesWithTheSameName(t *testing.T) {
	cmdtesting.InitTestErrorHandler(t)
	_, svc, rc := cmdtesting.TestData()
	tf := cmdtesting.NewTestFactory().WithNamespace("test")
	defer tf.Cleanup()

	codec := scheme.Codecs.LegacyCodec(scheme.Scheme.PrioritizedVersionsAllGroups()...)

	tf.UnstructuredClient = &fake.RESTClient{
		NegotiatedSerializer: resource.UnstructuredPlusDefaultContentConfig().NegotiatedSerializer,
		Client: fake.CreateHTTPClient(func(req *http.Request) (*http.Response, error) {
			switch p, m := req.URL.Path, req.Method; {
			case p == "/namespaces/test/replicationcontrollers/baz" && m == "DELETE":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, &rc.Items[0])}, nil
			case p == "/namespaces/test/replicationcontrollers/foo" && m == "DELETE":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, &rc.Items[0])}, nil
			case p == "/namespaces/test/services/baz" && m == "DELETE":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, &svc.Items[0])}, nil
			case p == "/namespaces/test/services/foo" && m == "DELETE":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, &svc.Items[0])}, nil
			default:
				// Ensures no GET is performed when deleting by name
				t.Fatalf("unexpected request: %#v\n%#v", req.URL, req)
				return nil, nil
			}
		}),
	}
	streams, _, buf, _ := genericiooptions.NewTestIOStreams()

	cmd := NewCmdDelete(tf, streams)
	cmd.Flags().Set("namespace", "test")
	cmd.Flags().Set("cascade", "false")
	cmd.Flags().Set("output", "name")
	cmd.Run(cmd, []string{"replicationcontrollers,services", "baz", "foo"})
	if buf.String() != "replicationcontroller/baz\nreplicationcontroller/foo\nservice/baz\nservice/foo\n" {
		t.Errorf("unexpected output: %s", buf.String())
	}
}

func TestDeleteDirectory(t *testing.T) {
	cmdtesting.InitTestErrorHandler(t)
	_, _, rc := cmdtesting.TestData()

	tf := cmdtesting.NewTestFactory().WithNamespace("test")
	defer tf.Cleanup()

	codec := scheme.Codecs.LegacyCodec(scheme.Scheme.PrioritizedVersionsAllGroups()...)

	tf.UnstructuredClient = &fake.RESTClient{
		NegotiatedSerializer: resource.UnstructuredPlusDefaultContentConfig().NegotiatedSerializer,
		Client: fake.CreateHTTPClient(func(req *http.Request) (*http.Response, error) {
			switch p, m := req.URL.Path, req.Method; {
			case strings.HasPrefix(p, "/namespaces/test/replicationcontrollers/") && m == "DELETE":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, &rc.Items[0])}, nil
			default:
				t.Fatalf("unexpected request: %#v\n%#v", req.URL, req)
				return nil, nil
			}
		}),
	}
	streams, _, buf, _ := genericiooptions.NewTestIOStreams()

	cmd := NewCmdDelete(tf, streams)
	cmd.Flags().Set("filename", "../../../testdata/replace/legacy")
	cmd.Flags().Set("cascade", "false")
	cmd.Flags().Set("output", "name")
	cmd.Run(cmd, []string{})

	if buf.String() != "replicationcontroller/frontend\nreplicationcontroller/redis-master\nreplicationcontroller/redis-slave\n" {
		t.Errorf("unexpected output: %s", buf.String())
	}
}

func TestDeleteMultipleSelector(t *testing.T) {
	cmdtesting.InitTestErrorHandler(t)
	pods, svc, _ := cmdtesting.TestData()

	tf := cmdtesting.NewTestFactory().WithNamespace("test")
	defer tf.Cleanup()

	codec := scheme.Codecs.LegacyCodec(scheme.Scheme.PrioritizedVersionsAllGroups()...)

	tf.UnstructuredClient = &fake.RESTClient{
		NegotiatedSerializer: resource.UnstructuredPlusDefaultContentConfig().NegotiatedSerializer,
		Client: fake.CreateHTTPClient(func(req *http.Request) (*http.Response, error) {
			switch p, m := req.URL.Path, req.Method; {
			case p == "/namespaces/test/pods" && m == "GET":
				if req.URL.Query().Get(metav1.LabelSelectorQueryParam("v1")) != "a=b" {
					t.Fatalf("unexpected request: %#v\n%#v", req.URL, req)
				}
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, pods)}, nil
			case p == "/namespaces/test/services" && m == "GET":
				if req.URL.Query().Get(metav1.LabelSelectorQueryParam("v1")) != "a=b" {
					t.Fatalf("unexpected request: %#v\n%#v", req.URL, req)
				}
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, svc)}, nil
			case strings.HasPrefix(p, "/namespaces/test/pods/") && m == "DELETE":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, &pods.Items[0])}, nil
			case strings.HasPrefix(p, "/namespaces/test/services/") && m == "DELETE":
				return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: cmdtesting.ObjBody(codec, &svc.Items[0])}, nil
			default:
				t.Fatalf("unexpected request: %#v\n%#v", req.URL, req)
				return nil, nil
			}
		}),
	}
	streams, _, buf, _ := genericiooptions.NewTestIOStreams()

	cmd := NewCmdDelete(tf, streams)
	cmd.Flags().Set("selector", "a=b")
	cmd.Flags().Set("cascade", "false")
	cmd.Flags().Set("output", "name")
	cmd.Run(cmd, []string{"pods,services"})

	if buf.String() != "pod/foo\npod/bar\nservice/baz\n" {
		t.Errorf("unexpected output: %s", buf.String())
	}
}

func TestResourceErrors(t *testing.T) {
	cmdtesting.InitTestErrorHandler(t)
	testCases := map[string]struct {
		args  []string
		errFn func(error) bool
	}{
		"no args": {
			args:  []string{},
			errFn: func(err error) bool { return strings.Contains(err.Error(), "You must provide one or more resources") },
		},
		"resources but no selectors": {
			args: []string{"pods"},
			errFn: func(err error) bool {
				return strings.Contains(err.Error(), "resource(s) were provided, but no name was specified")
			},
		},
		"multiple resources but no selectors": {
			args: []string{"pods,deployments"},
			errFn: func(err error) bool {
				return strings.Contains(err.Error(), "resource(s) were provided, but no name was specified")
			},
		},
	}

	for k, testCase := range testCases {
		t.Run(k, func(t *testing.T) {
			tf := cmdtesting.NewTestFactory().WithNamespace("test")
			defer tf.Cleanup()

			tf.ClientConfigVal = cmdtesting.DefaultClientConfig()

			streams, _, buf, _ := genericiooptions.NewTestIOStreams()
			options := &DeleteOptions{
				FilenameOptions:   resource.FilenameOptions{},
				GracePeriod:       -1,
				CascadingStrategy: metav1.DeletePropagationOrphan,
				Output:            "name",
				IOStreams:         streams,
			}
			err := options.Complete(tf, testCase.args, fakecmd())
			if !testCase.errFn(err) {
				t.Errorf("%s: unexpected error: %v", k, err)
				return
			}

			if buf.Len() > 0 {
				t.Errorf("buffer should be empty: %s", buf.String())
			}
		})
	}
}
