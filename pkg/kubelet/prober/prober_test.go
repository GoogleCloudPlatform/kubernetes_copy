/*
Copyright 2015 The Kubernetes Authors.

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

package prober

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/events"
	"k8s.io/kubernetes/pkg/api/legacyscheme"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	containertest "k8s.io/kubernetes/pkg/kubelet/container/testing"
	"k8s.io/kubernetes/pkg/kubelet/prober/results"
	"k8s.io/kubernetes/pkg/kubelet/util/ioutils"
	"k8s.io/kubernetes/pkg/probe"
	execprobe "k8s.io/kubernetes/pkg/probe/exec"
	"k8s.io/kubernetes/test/utils/ktesting"
)

func TestGetURLParts(t *testing.T) {
	testCases := []struct {
		probe *v1.HTTPGetAction
		ok    bool
		host  string
		port  int
		path  string
	}{
		{&v1.HTTPGetAction{Host: "", Port: intstr.FromInt32(-1), Path: ""}, false, "", -1, ""},
		{&v1.HTTPGetAction{Host: "", Port: intstr.FromString(""), Path: ""}, false, "", -1, ""},
		{&v1.HTTPGetAction{Host: "", Port: intstr.FromString("-1"), Path: ""}, false, "", -1, ""},
		{&v1.HTTPGetAction{Host: "", Port: intstr.FromString("not-found"), Path: ""}, false, "", -1, ""},
		{&v1.HTTPGetAction{Host: "", Port: intstr.FromString("found"), Path: ""}, true, "127.0.0.1", 93, ""},
		{&v1.HTTPGetAction{Host: "", Port: intstr.FromInt32(76), Path: ""}, true, "127.0.0.1", 76, ""},
		{&v1.HTTPGetAction{Host: "", Port: intstr.FromString("118"), Path: ""}, true, "127.0.0.1", 118, ""},
		{&v1.HTTPGetAction{Host: "hostname", Port: intstr.FromInt32(76), Path: "path"}, true, "hostname", 76, "path"},
	}

	for _, test := range testCases {
		state := v1.PodStatus{PodIP: "127.0.0.1"}
		container := v1.Container{
			Ports: []v1.ContainerPort{{Name: "found", ContainerPort: 93}},
			LivenessProbe: &v1.Probe{
				ProbeHandler: v1.ProbeHandler{
					HTTPGet: test.probe,
				},
			},
		}

		scheme := test.probe.Scheme
		if scheme == "" {
			scheme = v1.URISchemeHTTP
		}
		host := test.probe.Host
		if host == "" {
			host = state.PodIP
		}
		port, err := probe.ResolveContainerPort(test.probe.Port, &container)
		if test.ok && err != nil {
			t.Errorf("Unexpected error: %v", err)
		}
		path := test.probe.Path

		if !test.ok && err == nil {
			t.Errorf("Expected error for %+v, got %s%s:%d/%s", test, scheme, host, port, path)
		}
		if test.ok {
			if host != test.host || port != test.port || path != test.path {
				t.Errorf("Expected %s:%d/%s, got %s:%d/%s",
					test.host, test.port, test.path, host, port, path)
			}
		}
	}
}

func TestGetTCPAddrParts(t *testing.T) {
	testCases := []struct {
		probe *v1.TCPSocketAction
		ok    bool
		host  string
		port  int
	}{
		{&v1.TCPSocketAction{Port: intstr.FromInt32(-1)}, false, "", -1},
		{&v1.TCPSocketAction{Port: intstr.FromString("")}, false, "", -1},
		{&v1.TCPSocketAction{Port: intstr.FromString("-1")}, false, "", -1},
		{&v1.TCPSocketAction{Port: intstr.FromString("not-found")}, false, "", -1},
		{&v1.TCPSocketAction{Port: intstr.FromString("found")}, true, "1.2.3.4", 93},
		{&v1.TCPSocketAction{Port: intstr.FromInt32(76)}, true, "1.2.3.4", 76},
		{&v1.TCPSocketAction{Port: intstr.FromString("118")}, true, "1.2.3.4", 118},
	}

	for _, test := range testCases {
		host := "1.2.3.4"
		container := v1.Container{
			Ports: []v1.ContainerPort{{Name: "found", ContainerPort: 93}},
			LivenessProbe: &v1.Probe{
				ProbeHandler: v1.ProbeHandler{
					TCPSocket: test.probe,
				},
			},
		}
		port, err := probe.ResolveContainerPort(test.probe.Port, &container)
		if !test.ok && err == nil {
			t.Errorf("Expected error for %+v, got %s:%d", test, host, port)
		}
		if test.ok && err != nil {
			t.Errorf("Unexpected error: %v", err)
		}
		if test.ok {
			if host != test.host || port != test.port {
				t.Errorf("Expected %s:%d, got %s:%d", test.host, test.port, host, port)
			}
		}
	}
}

func TestProbe(t *testing.T) {
	ctx := context.Background()
	containerID := kubecontainer.ContainerID{Type: "test", ID: "foobar"}

	execProbe := &v1.Probe{
		ProbeHandler: v1.ProbeHandler{
			Exec: &v1.ExecAction{},
		},
	}

	tests := []struct {
		probe          *v1.Probe
		env            []v1.EnvVar
		execError      bool
		expectError    bool
		execResult     probe.Result
		expectedResult results.Result
		expectCommand  []string
		unsupported    bool
	}{
		{ // No probe
			probe:          nil,
			expectedResult: results.Success,
		},
		{ // No handler
			probe:          &v1.Probe{},
			expectError:    true,
			expectedResult: results.Failure,
		},
		{ // Probe fails
			probe:          execProbe,
			execResult:     probe.Failure,
			expectedResult: results.Failure,
		},
		{ // Probe succeeds
			probe:          execProbe,
			execResult:     probe.Success,
			expectedResult: results.Success,
		},
		{ // Probe result is warning
			probe:          execProbe,
			execResult:     probe.Warning,
			expectedResult: results.Success,
		},
		{ // Probe result is unknown with no error
			probe:          execProbe,
			execResult:     probe.Unknown,
			expectError:    false,
			expectedResult: results.Failure,
		},
		{ // Probe result is unknown with an error
			probe:          execProbe,
			execError:      true,
			expectError:    true,
			execResult:     probe.Unknown,
			expectedResult: results.Failure,
		},
		{ // Unsupported probe type
			probe:          nil,
			expectedResult: results.Failure,
			expectError:    true,
			unsupported:    true,
		},
		{ // Probe arguments are passed through
			probe: &v1.Probe{
				ProbeHandler: v1.ProbeHandler{
					Exec: &v1.ExecAction{
						Command: []string{"/bin/bash", "-c", "some script"},
					},
				},
			},
			expectCommand:  []string{"/bin/bash", "-c", "some script"},
			execResult:     probe.Success,
			expectedResult: results.Success,
		},
		{ // Probe arguments are passed through
			probe: &v1.Probe{
				ProbeHandler: v1.ProbeHandler{
					Exec: &v1.ExecAction{
						Command: []string{"/bin/bash", "-c", "some $(A) $(B)"},
					},
				},
			},
			env: []v1.EnvVar{
				{Name: "A", Value: "script"},
			},
			expectCommand:  []string{"/bin/bash", "-c", "some script $(B)"},
			execResult:     probe.Success,
			expectedResult: results.Success,
		},
	}

	for i, test := range tests {
		for _, pType := range [...]probeType{liveness, readiness, startup} {

			if test.unsupported {
				pType = probeType(666)
			}
			prober := &prober{
				recorder: &events.FakeRecorder{},
			}
			testID := fmt.Sprintf("%d-%s", i, pType)
			testContainer := v1.Container{Env: test.env}
			switch pType {
			case liveness:
				testContainer.LivenessProbe = test.probe
			case readiness:
				testContainer.ReadinessProbe = test.probe
			case startup:
				testContainer.StartupProbe = test.probe
			}
			if test.execError {
				prober.exec = fakeExecProber{test.execResult, errors.New("exec error")}
			} else {
				prober.exec = fakeExecProber{test.execResult, nil}
			}

			result, err := prober.probe(ctx, pType, &v1.Pod{}, v1.PodStatus{}, testContainer, containerID)

			if test.expectError {
				require.Error(t, err, "[%s] Expected probe error but no error was returned.", testID)
			} else {
				require.NoError(t, err, "[%s] Didn't expect probe error", testID)
			}

			require.Equal(t, test.expectedResult, result, "[%s] Expected result to be %v but was %v", testID, test.expectedResult, result)

			if len(test.expectCommand) > 0 {
				prober.exec = execprobe.New()
				prober.runner = &containertest.FakeContainerCommandRunner{}
				_, err := prober.probe(ctx, pType, &v1.Pod{}, v1.PodStatus{}, testContainer, containerID)
				require.NoError(t, err, "[%s] Didn't expect probe error ", testID)

				if !reflect.DeepEqual(test.expectCommand, prober.runner.(*containertest.FakeContainerCommandRunner).Cmd) {
					t.Errorf("[%s] unexpected probe arguments: %v", testID, prober.runner.(*containertest.FakeContainerCommandRunner).Cmd)
				}
			}
		}
	}
}

func TestNewExecInContainer(t *testing.T) {
	ctx := ktesting.Init(t)
	limit := 1024
	tenKilobyte := strings.Repeat("logs-123", 128*10)

	tests := []struct {
		name     string
		stdout   string
		expected string
		err      error
	}{
		{
			name:     "no error",
			stdout:   "foo",
			expected: "foo",
			err:      nil,
		},
		{
			name:     "no error",
			stdout:   tenKilobyte,
			expected: tenKilobyte[0:limit],
			err:      nil,
		},
		{
			name:     "error - make sure we get output",
			stdout:   "foo",
			expected: "foo",
			err:      errors.New("bad"),
		},
	}

	for _, test := range tests {
		runner := &containertest.FakeContainerCommandRunner{
			Stdout: test.stdout,
			Err:    test.err,
		}
		prober := &prober{
			runner: runner,
		}

		container := v1.Container{}
		containerID := kubecontainer.ContainerID{Type: "docker", ID: "containerID"}
		cmd := []string{"/foo", "bar"}
		exec := prober.newExecInContainer(ctx, container, containerID, cmd, 0)

		var dataBuffer bytes.Buffer
		writer := ioutils.LimitWriter(&dataBuffer, int64(limit))
		exec.SetStderr(writer)
		exec.SetStdout(writer)
		err := exec.Start()
		if err == nil {
			err = exec.Wait()
		}
		actualOutput := dataBuffer.Bytes()

		if e, a := containerID, runner.ContainerID; e != a {
			t.Errorf("%s: container id: expected %v, got %v", test.name, e, a)
		}
		if e, a := cmd, runner.Cmd; !reflect.DeepEqual(e, a) {
			t.Errorf("%s: cmd: expected %v, got %v", test.name, e, a)
		}
		// this isn't 100% foolproof as a bug in a real CommandRunner where it fails to copy to stdout/stderr wouldn't be caught by this test
		if e, a := test.expected, string(actualOutput); e != a {
			t.Errorf("%s: output: expected %q, got %q", test.name, e, a)
		}
		if e, a := fmt.Sprintf("%v", test.err), fmt.Sprintf("%v", err); e != a {
			t.Errorf("%s: error: expected %s, got %s", test.name, e, a)
		}
	}
}

func TestNewProber(t *testing.T) {
	runner := &containertest.FakeContainerCommandRunner{}
	recorder := &events.FakeRecorder{}
	prober := newProber(runner, recorder)

	assert.NotNil(t, prober, "Expected prober to be non-nil")
	assert.Equal(t, runner, prober.runner, "Expected prober runner to match")
	assert.Equal(t, recorder, prober.recorder, "Expected prober recorder to match")

	assert.NotNil(t, prober.exec, "exec probe initialized")
	assert.NotNil(t, prober.http, "http probe initialized")
	assert.NotNil(t, prober.tcp, "tcp probe initialized")
	assert.NotNil(t, prober.grpc, "grpc probe initialized")

}

func TestRecordContainerEventUnknownStatus(t *testing.T) {

	err := v1.AddToScheme(legacyscheme.Scheme)
	require.NoError(t, err, "failed to add v1 to scheme")

	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			UID: "test-probe-pod",
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name: "test-probe-container",
				},
			},
		},
	}

	container := pod.Spec.Containers[0]
	output := "probe output"

	testCases := []struct {
		name      string
		probeType probeType
		result    probe.Result
		expected  []string
	}{
		{
			name:      "Readiness Probe Unknown",
			probeType: readiness,
			result:    probe.Unknown,
			expected: []string{
				"Warning ContainerProbeWarning Readiness probe warning: probe output",
				"Warning ContainerProbeWarning Unknown Readiness probe status: unknown",
			},
		},
		{
			name:      "Liveness Probe Unknown",
			probeType: liveness,
			result:    probe.Unknown,
			expected: []string{
				"Warning ContainerProbeWarning Liveness probe warning: probe output",
				"Warning ContainerProbeWarning Unknown Liveness probe status: unknown",
			},
		},
		{
			name:      "Startup Probe Unknown",
			probeType: startup,
			result:    probe.Unknown,
			expected: []string{
				"Warning ContainerProbeWarning Startup probe warning: probe output",
				"Warning ContainerProbeWarning Unknown Startup probe status: unknown",
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			bufferSize := len(tc.expected) + 1
			fakeRecorder := events.NewFakeRecorder(bufferSize)

			pb := &prober{
				recorder: fakeRecorder,
			}

			pb.recordContainerEvent(pod, &container, v1.EventTypeWarning, "ContainerProbeWarning", "%s probe warning: %s", tc.probeType, output)
			pb.recordContainerEvent(pod, &container, v1.EventTypeWarning, "ContainerProbeWarning", "Unknown %s probe status: %s", tc.probeType, tc.result)

			assert.Equal(t, len(tc.expected), len(fakeRecorder.Events), "unexpected number of events")
			for _, expected := range tc.expected {
				assert.Equal(t, expected, <-fakeRecorder.Events)
			}
		})
	}
}
