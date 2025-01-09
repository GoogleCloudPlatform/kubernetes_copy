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

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/record"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	containertest "k8s.io/kubernetes/pkg/kubelet/container/testing"
	"k8s.io/kubernetes/pkg/kubelet/prober/results"
	"k8s.io/kubernetes/pkg/kubelet/util/ioutils"
	"k8s.io/kubernetes/pkg/probe"
	execprobe "k8s.io/kubernetes/pkg/probe/exec"
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
		{ // Probe result is unknown
			probe:          execProbe,
			execResult:     probe.Unknown,
			expectedResult: results.Failure,
		},
		{ // Probe has an error
			probe:          execProbe,
			execError:      true,
			expectError:    true,
			execResult:     probe.Unknown,
			expectedResult: results.Failure,
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
		for _, probeType := range [...]probeType{liveness, readiness, startup} {
			prober := &prober{
				recorder: &record.FakeRecorder{},
			}
			testID := fmt.Sprintf("%d-%s", i, probeType)
			testContainer := v1.Container{Env: test.env}
			switch probeType {
			case liveness:
				testContainer.LivenessProbe = test.probe
			case readiness:
				testContainer.ReadinessProbe = test.probe
			case startup:
				testContainer.StartupProbe = test.probe
			}
			if test.execError {
				prober.exec = fakeExecProber{test.execResult, "", errors.New("exec error")}
			} else {
				prober.exec = fakeExecProber{test.execResult, "", nil}
			}

			result, _, err := prober.probe(ctx, probeType, &v1.Pod{}, v1.PodStatus{}, testContainer, containerID)
			if test.expectError && err == nil {
				t.Errorf("[%s] Expected probe error but no error was returned.", testID)
			}
			if !test.expectError && err != nil {
				t.Errorf("[%s] Didn't expect probe error but got: %v", testID, err)
			}
			if test.expectedResult != result {
				t.Errorf("[%s] Expected result to be %v but was %v", testID, test.expectedResult, result)
			}

			if len(test.expectCommand) > 0 {
				prober.exec = execprobe.New()
				prober.runner = &containertest.FakeContainerCommandRunner{}
				_, _, err := prober.probe(ctx, probeType, &v1.Pod{}, v1.PodStatus{}, testContainer, containerID)
				if err != nil {
					t.Errorf("[%s] Didn't expect probe error but got: %v", testID, err)
					continue
				}
				if !reflect.DeepEqual(test.expectCommand, prober.runner.(*containertest.FakeContainerCommandRunner).Cmd) {
					t.Errorf("[%s] unexpected probe arguments: %v", testID, prober.runner.(*containertest.FakeContainerCommandRunner).Cmd)
				}
			}
		}
	}
}

func TestNewExecInContainer(t *testing.T) {
	ctx := context.Background()
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
