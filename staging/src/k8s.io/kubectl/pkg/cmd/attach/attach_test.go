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

package attach

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/rest/fake"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/kubectl/pkg/cmd/exec"
	cmdtesting "k8s.io/kubectl/pkg/cmd/testing"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/cmd/util/podcmd"
	"k8s.io/kubectl/pkg/polymorphichelpers"
	"k8s.io/kubectl/pkg/scheme"
)

type fakeRemoteAttach struct {
	url *url.URL
	err error
}

func (f *fakeRemoteAttach) Attach(url *url.URL, config *restclient.Config, stdin io.Reader, stdout, stderr io.Writer, tty bool, terminalSizeQueue remotecommand.TerminalSizeQueue) error {
	f.url = url
	return f.err
}

func fakeAttachablePodFn(pod *corev1.Pod) polymorphichelpers.AttachablePodForObjectFunc {
	return func(getter genericclioptions.RESTClientGetter, obj runtime.Object, timeout time.Duration) (*corev1.Pod, error) {
		return pod, nil
	}
}

func TestPodAndContainerAttach(t *testing.T) {
	tests := []struct {
		name                  string
		args                  []string
		options               *AttachOptions
		expectError           string
		expectedPodName       string
		expectedContainerName string
		expectOut             string
		obj                   *corev1.Pod
	}{
		{
			name:        "empty",
			options:     &AttachOptions{GetPodTimeout: 1},
			expectError: "at least 1 argument is required",
		},
		{
			name:        "too many args",
			options:     &AttachOptions{GetPodTimeout: 2},
			args:        []string{"one", "two", "three"},
			expectError: "at most 2 arguments",
		},
		{
			name:                  "no container, no flags",
			options:               &AttachOptions{GetPodTimeout: defaultPodLogsTimeout},
			args:                  []string{"foo"},
			expectedPodName:       "foo",
			expectedContainerName: "bar",
			obj:                   attachPod(),
			expectOut:             `Defaulted container "bar" out of: bar, debugger (ephem), initfoo (init)`,
		},
		{
			name:                  "no container, no flags, sets default expected container as annotation",
			options:               &AttachOptions{GetPodTimeout: defaultPodLogsTimeout},
			args:                  []string{"foo"},
			expectedPodName:       "foo",
			expectedContainerName: "bar",
			obj:                   setDefaultContainer(attachPod(), "initfoo"),
			expectOut:             ``,
		},
		{
			name:                  "no container, no flags, sets default missing container as annotation",
			options:               &AttachOptions{GetPodTimeout: defaultPodLogsTimeout},
			args:                  []string{"foo"},
			expectedPodName:       "foo",
			expectedContainerName: "bar",
			obj:                   setDefaultContainer(attachPod(), "does-not-exist"),
			expectOut:             `Defaulted container "bar" out of: bar, debugger (ephem), initfoo (init)`,
		},
		{
			name:                  "container in flag",
			options:               &AttachOptions{StreamOptions: exec.StreamOptions{ContainerName: "bar"}, GetPodTimeout: 10000000},
			args:                  []string{"foo"},
			expectedPodName:       "foo",
			expectedContainerName: "bar",
			obj:                   attachPod(),
		},
		{
			name:                  "init container in flag",
			options:               &AttachOptions{StreamOptions: exec.StreamOptions{ContainerName: "initfoo"}, GetPodTimeout: 30},
			args:                  []string{"foo"},
			expectedPodName:       "foo",
			expectedContainerName: "initfoo",
			obj:                   attachPod(),
		},
		{
			name:                  "ephemeral container in flag",
			options:               &AttachOptions{StreamOptions: exec.StreamOptions{ContainerName: "debugger"}, GetPodTimeout: 30},
			args:                  []string{"foo"},
			expectedPodName:       "foo",
			expectedContainerName: "debugger",
			obj:                   attachPod(),
		},
		{
			name:            "non-existing container",
			options:         &AttachOptions{StreamOptions: exec.StreamOptions{ContainerName: "wrong"}, GetPodTimeout: 10},
			args:            []string{"foo"},
			expectedPodName: "foo",
			expectError:     "container wrong not found in pod foo",
			obj:             attachPod(),
		},
		{
			name:                  "no container, no flags, pods and name",
			options:               &AttachOptions{GetPodTimeout: 10000},
			args:                  []string{"pods", "foo"},
			expectedPodName:       "foo",
			expectedContainerName: "bar",
			obj:                   attachPod(),
		},
		{
			name:                  "invalid get pod timeout value",
			options:               &AttachOptions{GetPodTimeout: 0},
			args:                  []string{"pod/foo"},
			expectedPodName:       "foo",
			expectedContainerName: "bar",
			obj:                   attachPod(),
			expectError:           "must be higher than zero",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// setup opts to fetch our test pod
			test.options.AttachablePodFn = fakeAttachablePodFn(test.obj)
			test.options.Resources = test.args

			if err := test.options.Validate(); err != nil {
				if test.expectError == "" || !strings.Contains(err.Error(), test.expectError) {
					t.Errorf("unexpected error: expected %q, got %q", test.expectError, err)
				}
				return
			}

			pod, err := test.options.findAttachablePod(&corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "test"},
				Spec: corev1.PodSpec{
					InitContainers: []corev1.Container{
						{
							Name: "initfoo",
						},
					},
					Containers: []corev1.Container{
						{
							Name: "foobar",
						},
					},
					EphemeralContainers: []corev1.EphemeralContainer{
						{
							EphemeralContainerCommon: corev1.EphemeralContainerCommon{
								Name: "ephemfoo",
							},
						},
					},
				},
			})
			if err != nil {
				if test.expectError == "" || !strings.Contains(err.Error(), test.expectError) {
					t.Errorf("unexpected error: expected %q, got %q", err, test.expectError)
				}
				return
			}

			if pod.Name != test.expectedPodName {
				t.Errorf("unexpected pod name: expected %q, got %q", test.expectedContainerName, pod.Name)
			}

			var buf bytes.Buffer
			test.options.ErrOut = &buf
			container, err := test.options.containerToAttachTo(attachPod())

			if len(test.expectOut) > 0 && !strings.Contains(buf.String(), test.expectOut) {
				t.Errorf("unexpected output: output did not contain %q\n---\n%s", test.expectOut, buf.String())
			}

			if err != nil {
				if test.expectError == "" || !strings.Contains(err.Error(), test.expectError) {
					t.Errorf("unexpected error: expected %q, got %q", test.expectError, err)
				}
				return
			}

			if container.Name != test.expectedContainerName {
				t.Errorf("unexpected container name: expected %q, got %q", test.expectedContainerName, container.Name)
			}

			if test.options.PodName != test.expectedPodName {
				t.Errorf("%s: expected: %s, got: %s", test.name, test.expectedPodName, test.options.PodName)
			}

			if len(test.expectError) > 0 {
				t.Fatalf("expected error %q, but saw none", test.expectError)
			}
		})
	}
}

func TestAttach(t *testing.T) {
	version := "v1"
	tests := []struct {
		name, version, podPath, fetchPodPath, attachPath, container string
		pod                                                         *corev1.Pod
		remoteAttachErr                                             bool
		expectedErr                                                 string
		expectedErrOut                                              []string
	}{
		{
			name:         "pod attach",
			version:      version,
			podPath:      "/api/" + version + "/namespaces/test/pods/foo",
			fetchPodPath: "/namespaces/test/pods/foo",
			attachPath:   "/api/" + version + "/namespaces/test/pods/foo/attach",
			pod:          attachPod(),
			container:    "bar",
			expectedErrOut: []string{
				"All commands and output executed when attached to a container will be visible via pods/log.",
				"If you don't see a command prompt, try pressing enter.",
			},
		},
		{
			name:            "pod attach error",
			version:         version,
			podPath:         "/api/" + version + "/namespaces/test/pods/foo",
			fetchPodPath:    "/namespaces/test/pods/foo",
			attachPath:      "/api/" + version + "/namespaces/test/pods/foo/attach",
			pod:             attachPod(),
			remoteAttachErr: true,
			container:       "bar",
			expectedErr:     "attach error",
		},
		{
			name:         "container not found error",
			version:      version,
			podPath:      "/api/" + version + "/namespaces/test/pods/foo",
			fetchPodPath: "/namespaces/test/pods/foo",
			attachPath:   "/api/" + version + "/namespaces/test/pods/foo/attach",
			pod:          attachPod(),
			container:    "foo",
			expectedErr:  "cannot attach to the container: container foo not found in pod foo",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tf := cmdtesting.NewTestFactory().WithNamespace("test")
			defer tf.Cleanup()

			codec := scheme.Codecs.LegacyCodec(scheme.Scheme.PrioritizedVersionsAllGroups()...)
			ns := scheme.Codecs.WithoutConversion()

			tf.Client = &fake.RESTClient{
				GroupVersion:         schema.GroupVersion{Group: "", Version: "v1"},
				NegotiatedSerializer: ns,
				Client: fake.CreateHTTPClient(func(req *http.Request) (*http.Response, error) {
					switch p, m := req.URL.Path, req.Method; {
					case p == test.podPath && m == "GET":
						body := cmdtesting.ObjBody(codec, test.pod)
						return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: body}, nil
					case p == test.fetchPodPath && m == "GET":
						body := cmdtesting.ObjBody(codec, test.pod)
						return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: body}, nil
					default:
						t.Errorf("%s: unexpected request: %s %#v\n%#v", p, req.Method, req.URL, req)
						return nil, fmt.Errorf("unexpected request")
					}
				}),
			}
			tf.ClientConfigVal = &restclient.Config{APIPath: "/api", ContentConfig: restclient.ContentConfig{NegotiatedSerializer: scheme.Codecs, GroupVersion: &schema.GroupVersion{Version: test.version}}}

			remoteAttach := &fakeRemoteAttach{}
			if test.remoteAttachErr {
				remoteAttach.err = fmt.Errorf("attach error")
			}
			streams, _, _, errOut := genericiooptions.NewTestIOStreams()
			options := &AttachOptions{
				StreamOptions: exec.StreamOptions{
					ContainerName: test.container,
					IOStreams:     streams,
				},
				Attach:        remoteAttach,
				GetPodTimeout: 1000,
			}

			options.restClientGetter = tf
			options.Namespace = "test"
			options.Resources = []string{"foo"}
			options.Builder = tf.NewBuilder
			options.AttachablePodFn = fakeAttachablePodFn(test.pod)
			options.AttachFunc = func(opts *AttachOptions, containerToAttach *corev1.Container, raw bool, sizeQueue remotecommand.TerminalSizeQueue) func() error {
				return func() error {
					u, err := url.Parse(fmt.Sprintf("%s?container=%s", test.attachPath, containerToAttach.Name))
					if err != nil {
						return err
					}

					return options.Attach.Attach(u, nil, nil, nil, nil, raw, sizeQueue)
				}
			}

			err := options.Run()
			if test.expectedErr != "" && err.Error() != test.expectedErr {
				t.Errorf("%s: Unexpected exec error: %v", test.name, err)
				return
			}
			if test.expectedErr == "" && err != nil {
				t.Errorf("%s: Unexpected error: %v", test.name, err)
				return
			}
			if test.expectedErr != "" {
				return
			}
			if remoteAttach.url.Path != test.attachPath {
				t.Errorf("%s: Did not get expected path for exec request: %q %q", test.name, test.attachPath, remoteAttach.url.Path)
				return
			}
			if remoteAttach.url.Query().Get("container") != "bar" {
				t.Errorf("%s: Did not have query parameters: %s", test.name, remoteAttach.url.Query())
			}
			if test.expectedErrOut != nil {
				for _, expect := range test.expectedErrOut {
					if !strings.Contains(errOut.String(), expect) {
						t.Errorf("%s: Stderr did not contain expected message for attach request:\n expected:\n\t%s\n got:\n\t%s", test.name, expect, strings.ReplaceAll(errOut.String(), "\n", ""))
						return
					}
				}
			}
		})
	}
}

func TestAttachWarnings(t *testing.T) {
	version := "v1"
	tests := []struct {
		name, container, version, podPath, fetchPodPath, expectedErr string
		pod                                                          *corev1.Pod
		stdin, tty                                                   bool
	}{
		{
			name:         "fallback tty if not supported",
			version:      version,
			podPath:      "/api/" + version + "/namespaces/test/pods/foo",
			fetchPodPath: "/namespaces/test/pods/foo",
			pod:          attachPod(),
			stdin:        true,
			tty:          true,
			expectedErr:  "Unable to use a TTY - container bar did not allocate one",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tf := cmdtesting.NewTestFactory().WithNamespace("test")
			defer tf.Cleanup()

			streams, _, _, bufErr := genericiooptions.NewTestIOStreams()

			codec := scheme.Codecs.LegacyCodec(scheme.Scheme.PrioritizedVersionsAllGroups()...)
			ns := scheme.Codecs.WithoutConversion()

			tf.Client = &fake.RESTClient{
				GroupVersion:         schema.GroupVersion{Group: "", Version: "v1"},
				NegotiatedSerializer: ns,
				Client: fake.CreateHTTPClient(func(req *http.Request) (*http.Response, error) {
					switch p, m := req.URL.Path, req.Method; {
					case p == test.podPath && m == "GET":
						body := cmdtesting.ObjBody(codec, test.pod)
						return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: body}, nil
					case p == test.fetchPodPath && m == "GET":
						body := cmdtesting.ObjBody(codec, test.pod)
						return &http.Response{StatusCode: http.StatusOK, Header: cmdtesting.DefaultHeader(), Body: body}, nil
					default:
						t.Errorf("%s: unexpected request: %s %#v\n%#v", p, req.Method, req.URL, req)
						return nil, fmt.Errorf("unexpected request")
					}
				}),
			}
			tf.ClientConfigVal = &restclient.Config{APIPath: "/api", ContentConfig: restclient.ContentConfig{NegotiatedSerializer: scheme.Codecs, GroupVersion: &schema.GroupVersion{Version: test.version}}}

			options := &AttachOptions{
				StreamOptions: exec.StreamOptions{
					Stdin:         test.stdin,
					TTY:           test.tty,
					ContainerName: test.container,
					IOStreams:     streams,
				},

				Attach:        &fakeRemoteAttach{},
				GetPodTimeout: 1000,
			}

			options.restClientGetter = tf
			options.Namespace = "test"
			options.Resources = []string{"foo"}
			options.Builder = tf.NewBuilder
			options.AttachablePodFn = fakeAttachablePodFn(test.pod)
			options.AttachFunc = func(opts *AttachOptions, containerToAttach *corev1.Container, raw bool, sizeQueue remotecommand.TerminalSizeQueue) func() error {
				return func() error {
					u, err := url.Parse("http://foo.bar")
					if err != nil {
						return err
					}

					return options.Attach.Attach(u, nil, nil, nil, nil, raw, sizeQueue)
				}
			}

			if err := options.Run(); err != nil {
				t.Fatal(err)
			}

			if test.stdin && test.tty {
				if !test.pod.Spec.Containers[0].TTY {
					if !strings.Contains(bufErr.String(), test.expectedErr) {
						t.Errorf("%s: Expected TTY fallback warning for attach request: %s", test.name, bufErr.String())
						return
					}
				}
			}
		})
	}
}

func attachPod() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "foo", Namespace: "test", ResourceVersion: "10"},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyAlways,
			DNSPolicy:     corev1.DNSClusterFirst,
			Containers: []corev1.Container{
				{
					Name: "bar",
				},
			},
			InitContainers: []corev1.Container{
				{
					Name: "initfoo",
				},
			},
			EphemeralContainers: []corev1.EphemeralContainer{
				{
					EphemeralContainerCommon: corev1.EphemeralContainerCommon{
						Name: "debugger",
					},
				},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
		},
	}
}

func setDefaultContainer(pod *corev1.Pod, name string) *corev1.Pod {
	if pod.Annotations == nil {
		pod.Annotations = make(map[string]string)
	}
	pod.Annotations[podcmd.DefaultContainerAnnotationName] = name
	return pod
}

func TestReattachMessage(t *testing.T) {
	tests := []struct {
		name          string
		pod           *corev1.Pod
		rawTTY, stdin bool
		container     string
		expected      string
	}{
		{
			name:      "normal interactive session",
			pod:       attachPod(),
			container: "bar",
			rawTTY:    true,
			stdin:     true,
			expected:  "Session ended, resume using",
		},
		{
			name:      "no stdin",
			pod:       attachPod(),
			container: "bar",
			rawTTY:    true,
			stdin:     false,
			expected:  "",
		},
		{
			name:      "not connected to a real TTY",
			pod:       attachPod(),
			container: "bar",
			rawTTY:    false,
			stdin:     true,
			expected:  "",
		},
		{
			name: "no restarts",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "foo", Namespace: "test"},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers:    []corev1.Container{{Name: "bar"}},
				},
				Status: corev1.PodStatus{Phase: corev1.PodRunning},
			},
			container: "bar",
			rawTTY:    true,
			stdin:     true,
			expected:  "",
		},
		{
			name:      "ephemeral container",
			pod:       attachPod(),
			container: "debugger",
			rawTTY:    true,
			stdin:     true,
			expected:  "Session ended, the ephemeral container will not be restarted",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			options := &AttachOptions{
				StreamOptions: exec.StreamOptions{
					Stdin: test.stdin,
				},
				Pod: test.pod,
			}
			if msg := options.reattachMessage(test.container, test.rawTTY); test.expected == "" && msg != "" {
				t.Errorf("reattachMessage(%v, %v) = %q, want empty string", test.container, test.rawTTY, msg)
			} else if !strings.Contains(msg, test.expected) {
				t.Errorf("reattachMessage(%v, %v) = %q, want string containing %q", test.container, test.rawTTY, msg, test.expected)
			}
		})
	}
}

func TestCreateExecutor(t *testing.T) {
	url, err := url.Parse("http://localhost:8080/index.html")
	if err != nil {
		t.Fatalf("unable to parse test url: %v", err)
	}
	config := cmdtesting.DefaultClientConfig()
	// First, ensure that no environment variable creates the fallback executor.
	executor, err := createExecutor(url, config)
	if err != nil {
		t.Fatalf("unable to create executor: %v", err)
	}
	if _, isFallback := executor.(*remotecommand.FallbackExecutor); !isFallback {
		t.Errorf("expected fallback executor, got %#v", executor)
	}
	// Next, check turning on feature flag explicitly also creates fallback executor.
	t.Setenv(string(cmdutil.RemoteCommandWebsockets), "true")
	executor, err = createExecutor(url, config)
	if err != nil {
		t.Fatalf("unable to create executor: %v", err)
	}
	if _, isFallback := executor.(*remotecommand.FallbackExecutor); !isFallback {
		t.Errorf("expected fallback executor, got %#v", executor)
	}
	// Finally, check explicit disabling does NOT create the fallback executor.
	t.Setenv(string(cmdutil.RemoteCommandWebsockets), "false")
	executor, err = createExecutor(url, config)
	if err != nil {
		t.Fatalf("unable to create executor: %v", err)
	}
	if _, isFallback := executor.(*remotecommand.FallbackExecutor); isFallback {
		t.Errorf("expected fallback executor, got %#v", executor)
	}
}
