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
	"os"
	"reflect"
	"sync"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/record"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	kubepod "k8s.io/kubernetes/pkg/kubelet/pod"
	"k8s.io/kubernetes/pkg/kubelet/prober/results"
	"k8s.io/kubernetes/pkg/kubelet/status"
	statustest "k8s.io/kubernetes/pkg/kubelet/status/testing"
	kubeletutil "k8s.io/kubernetes/pkg/kubelet/util"
	"k8s.io/kubernetes/pkg/probe"
	"k8s.io/utils/exec"
)

const (
	testContainerName = "cOnTaInEr_NaMe"
	testPodUID        = "pOd_UiD"
)

var testContainerID = kubecontainer.ContainerID{Type: "test", ID: "cOnTaInEr_Id"}

func getTestRunningStatus() v1.PodStatus {
	return getTestRunningStatusWithStarted(true)
}

func getTestNotRunningStatus() v1.PodStatus {
	return getTestRunningStatusWithStarted(false)
}

func getTestRunningStatusWithStarted(started bool) v1.PodStatus {
	containerStatus := v1.ContainerStatus{
		Name:        testContainerName,
		ContainerID: testContainerID.String(),
	}
	containerStatus.State.Running = &v1.ContainerStateRunning{StartedAt: metav1.Now()}
	containerStatus.Started = &started
	podStatus := v1.PodStatus{
		Phase:             v1.PodRunning,
		ContainerStatuses: []v1.ContainerStatus{containerStatus},
	}
	return podStatus
}

func getTestPod() *v1.Pod {
	container := v1.Container{
		Name: testContainerName,
	}
	pod := v1.Pod{
		Spec: v1.PodSpec{
			Containers:    []v1.Container{container},
			RestartPolicy: v1.RestartPolicyNever,
		},
	}
	pod.Name = "testPod"
	pod.UID = testPodUID
	return &pod
}

func setTestProbe(pod *v1.Pod, probeType probeType, probeSpec v1.Probe) {
	// All tests rely on the fake exec prober.
	probeSpec.ProbeHandler = v1.ProbeHandler{
		Exec: &v1.ExecAction{},
	}

	// Apply test defaults, overridden for test speed.
	defaults := map[string]int64{
		"TimeoutSeconds":   1,
		"PeriodSeconds":    1,
		"SuccessThreshold": 1,
		"FailureThreshold": 1,
	}
	for field, value := range defaults {
		f := reflect.ValueOf(&probeSpec).Elem().FieldByName(field)
		if f.Int() == 0 {
			f.SetInt(value)
		}
	}

	switch probeType {
	case readiness:
		pod.Spec.Containers[0].ReadinessProbe = &probeSpec
	case liveness:
		pod.Spec.Containers[0].LivenessProbe = &probeSpec
	case startup:
		pod.Spec.Containers[0].StartupProbe = &probeSpec
	}
}

func newTestManager() *manager {
	podManager := kubepod.NewBasicPodManager()
	podStartupLatencyTracker := kubeletutil.NewPodStartupLatencyTracker()
	// Add test pod to pod manager, so that status manager can get the pod from pod manager if needed.
	podManager.AddPod(getTestPod())
	testRootDir := ""
	if tempDir, err := os.MkdirTemp("", "kubelet_test."); err != nil {
		return nil
	} else {
		testRootDir = tempDir
	}
	m := NewManager(
		status.NewManager(&fake.Clientset{}, podManager, &statustest.FakePodDeletionSafetyProvider{}, podStartupLatencyTracker, testRootDir),
		results.NewManager(),
		results.NewManager(),
		results.NewManager(),
		nil, // runner
		&record.FakeRecorder{},
	).(*manager)
	// Don't actually execute probes.
	m.prober.exec = fakeExecProber{probe.Success, nil}
	return m
}

func newTestWorker(m *manager, probeType probeType, probeSpec v1.Probe) *worker {
	pod := getTestPod()
	setTestProbe(pod, probeType, probeSpec)
	return newWorker(m, probeType, pod, pod.Spec.Containers[0])
}

type fakeExecProber struct {
	result probe.Result
	err    error
}

func (p fakeExecProber) Probe(c exec.Cmd) (probe.Result, string, error) {
	return p.result, "", p.err
}

type syncExecProber struct {
	sync.RWMutex
	fakeExecProber
}

func (p *syncExecProber) set(result probe.Result, err error) {
	p.Lock()
	defer p.Unlock()
	p.result = result
	p.err = err
}

func (p *syncExecProber) Probe(cmd exec.Cmd) (probe.Result, string, error) {
	p.RLock()
	defer p.RUnlock()
	return p.fakeExecProber.Probe(cmd)
}
