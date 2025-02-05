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
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes/fake"
	kubepod "k8s.io/kubernetes/pkg/kubelet/pod"
	"k8s.io/kubernetes/pkg/kubelet/prober/results"
	"k8s.io/kubernetes/pkg/kubelet/status"
	statustest "k8s.io/kubernetes/pkg/kubelet/status/testing"
	kubeletutil "k8s.io/kubernetes/pkg/kubelet/util"
	"k8s.io/kubernetes/pkg/probe"
)

func init() {
}

func TestDoProbe(t *testing.T) {
	m := newTestManager()

	for _, probeType := range [...]probeType{liveness, readiness, startup} {
		// Test statuses.
		runningStatus := getTestRunningStatusWithStarted(probeType != startup)
		pendingStatus := getTestRunningStatusWithStarted(probeType != startup)
		pendingStatus.ContainerStatuses[0].State.Running = nil
		terminatedStatus := getTestRunningStatusWithStarted(probeType != startup)
		terminatedStatus.ContainerStatuses[0].State.Running = nil
		terminatedStatus.ContainerStatuses[0].State.Terminated = &v1.ContainerStateTerminated{
			StartedAt: metav1.Now(),
		}
		otherStatus := getTestRunningStatusWithStarted(probeType != startup)
		otherStatus.ContainerStatuses[0].Name = "otherContainer"
		failedStatus := getTestRunningStatusWithStarted(probeType != startup)
		failedStatus.Phase = v1.PodFailed

		tests := []struct {
			probe                v1.Probe
			podStatus            *v1.PodStatus
			expectContinue       map[string]bool
			expectSet            bool
			expectedResult       results.Result
			setDeletionTimestamp bool
		}{
			{ // No status.
				expectContinue: map[string]bool{
					liveness.String():  true,
					readiness.String(): true,
					startup.String():   true,
				},
			},
			{ // Pod failed
				podStatus: &failedStatus,
			},
			{ // Pod deletion
				podStatus:            &runningStatus,
				setDeletionTimestamp: true,
				expectSet:            true,
				expectContinue: map[string]bool{
					readiness.String(): true,
				},
				expectedResult: results.Success,
			},
			{ // No container status
				podStatus: &otherStatus,
				expectContinue: map[string]bool{
					liveness.String():  true,
					readiness.String(): true,
					startup.String():   true,
				},
			},
			{ // Container waiting
				podStatus: &pendingStatus,
				expectContinue: map[string]bool{
					liveness.String():  true,
					readiness.String(): true,
					startup.String():   true,
				},
				expectSet:      true,
				expectedResult: results.Failure,
			},
			{ // Container terminated
				podStatus:      &terminatedStatus,
				expectSet:      true,
				expectedResult: results.Failure,
			},
			{ // Probe successful.
				podStatus: &runningStatus,
				expectContinue: map[string]bool{
					liveness.String():  true,
					readiness.String(): true,
					startup.String():   true,
				},
				expectSet:      true,
				expectedResult: results.Success,
			},
			{ // Initial delay passed
				podStatus: &runningStatus,
				probe: v1.Probe{
					InitialDelaySeconds: -100,
				},
				expectContinue: map[string]bool{
					liveness.String():  true,
					readiness.String(): true,
					startup.String():   true,
				},
				expectSet:      true,
				expectedResult: results.Success,
			},
		}

		for i, test := range tests {
			ctx := context.Background()
			w := newTestWorker(m, probeType, test.probe)
			if test.podStatus != nil {
				m.statusManager.SetPodStatus(w.pod, *test.podStatus)
			}
			if test.setDeletionTimestamp {
				now := metav1.Now()
				w.pod.ObjectMeta.DeletionTimestamp = &now
			}
			if c := w.doProbe(ctx); c != test.expectContinue[probeType.String()] {
				t.Errorf("[%s-%d] Expected continue to be %v but got %v", probeType, i, test.expectContinue[probeType.String()], c)
			}
			result, ok := resultsManager(m, probeType).Get(testContainerID)
			if ok != test.expectSet {
				t.Errorf("[%s-%d] Expected to have result: %v but got %v", probeType, i, test.expectSet, ok)
			}
			if result != test.expectedResult {
				t.Errorf("[%s-%d] Expected result: %v but got %v", probeType, i, test.expectedResult, result)
			}

			// Clean up.
			testRootDir := ""
			if tempDir, err := os.MkdirTemp("", "kubelet_test."); err != nil {
				t.Fatalf("can't make a temp rootdir: %v", err)
			} else {
				testRootDir = tempDir
			}
			m.statusManager = status.NewManager(&fake.Clientset{}, kubepod.NewBasicPodManager(), &statustest.FakePodDeletionSafetyProvider{}, kubeletutil.NewPodStartupLatencyTracker(), testRootDir)
			resultsManager(m, probeType).Remove(testContainerID)
		}
	}
}

func TestInitialDelay(t *testing.T) {
	ctx := context.Background()
	m := newTestManager()

	for _, probeType := range [...]probeType{liveness, readiness, startup} {
		w := newTestWorker(m, probeType, v1.Probe{
			InitialDelaySeconds: 10,
		})
		m.statusManager.SetPodStatus(w.pod, getTestRunningStatusWithStarted(probeType != startup))

		expectContinue(t, w, w.doProbe(ctx), "during initial delay")
		// Default value depends on probe, Success for liveness, Failure for readiness, Unknown for startup
		switch probeType {
		case liveness:
			expectResult(t, w, results.Success, "during initial delay")
		case startup:
			expectResult(t, w, results.Unknown, "during initial delay")
		}

		// 100 seconds later...
		laterStatus := getTestRunningStatusWithStarted(probeType != startup)
		laterStatus.ContainerStatuses[0].State.Running.StartedAt.Time =
			time.Now().Add(-100 * time.Second)
		m.statusManager.SetPodStatus(w.pod, laterStatus)

		// Second call should succeed (already waited).
		expectContinue(t, w, w.doProbe(ctx), "after initial delay")
		expectResult(t, w, results.Success, "after initial delay")
	}
}

func TestFailureThreshold(t *testing.T) {
	ctx := context.Background()
	m := newTestManager()
	w := newTestWorker(m, readiness, v1.Probe{SuccessThreshold: 1, FailureThreshold: 3})
	m.statusManager.SetPodStatus(w.pod, getTestRunningStatus())

	for i := 0; i < 2; i++ {
		// First probe should succeed.
		m.prober.exec = fakeExecProber{probe.Success, nil}

		for j := 0; j < 3; j++ {
			msg := fmt.Sprintf("%d success (%d)", j+1, i)
			expectContinue(t, w, w.doProbe(ctx), msg)
			expectResult(t, w, results.Success, msg)
		}

		// Prober starts failing :(
		m.prober.exec = fakeExecProber{probe.Failure, nil}

		// Next 2 probes should still be "success".
		for j := 0; j < 2; j++ {
			msg := fmt.Sprintf("%d failing (%d)", j+1, i)
			expectContinue(t, w, w.doProbe(ctx), msg)
			expectResult(t, w, results.Success, msg)
		}

		// Third & following fail.
		for j := 0; j < 3; j++ {
			msg := fmt.Sprintf("%d failure (%d)", j+3, i)
			expectContinue(t, w, w.doProbe(ctx), msg)
			expectResult(t, w, results.Failure, msg)
		}
	}
}

func TestSuccessThreshold(t *testing.T) {
	ctx := context.Background()
	m := newTestManager()
	w := newTestWorker(m, readiness, v1.Probe{SuccessThreshold: 3, FailureThreshold: 1})
	m.statusManager.SetPodStatus(w.pod, getTestRunningStatus())

	// Start out failure.
	w.resultsManager.Set(testContainerID, results.Failure, &v1.Pod{})

	for i := 0; i < 2; i++ {
		// Probe defaults to Failure.
		for j := 0; j < 2; j++ {
			msg := fmt.Sprintf("%d success (%d)", j+1, i)
			expectContinue(t, w, w.doProbe(ctx), msg)
			expectResult(t, w, results.Failure, msg)
		}

		// Continuing success!
		for j := 0; j < 3; j++ {
			msg := fmt.Sprintf("%d success (%d)", j+3, i)
			expectContinue(t, w, w.doProbe(ctx), msg)
			expectResult(t, w, results.Success, msg)
		}

		// Prober flakes :(
		m.prober.exec = fakeExecProber{probe.Failure, nil}
		msg := fmt.Sprintf("1 failure (%d)", i)
		expectContinue(t, w, w.doProbe(ctx), msg)
		expectResult(t, w, results.Failure, msg)

		// Back to success.
		m.prober.exec = fakeExecProber{probe.Success, nil}
	}
}

func TestStartupProbeSuccessThreshold(t *testing.T) {
	ctx := context.Background()
	m := newTestManager()
	successThreshold := 1
	failureThreshold := 3
	w := newTestWorker(m, startup, v1.Probe{SuccessThreshold: int32(successThreshold), FailureThreshold: int32(failureThreshold)})
	m.statusManager.SetPodStatus(w.pod, getTestNotRunningStatus())
	m.prober.exec = fakeExecProber{probe.Success, nil}

	for i := 0; i < successThreshold+1; i++ {
		if i < successThreshold {
			// Probe should not be on hold and will continue to be excuted
			// until successThreshold is met
			if w.onHold {
				t.Errorf("Prober should not be on hold")
			}
			msg := fmt.Sprintf("%d success", i+1)
			expectContinue(t, w, w.doProbe(ctx), msg)
			expectResult(t, w, results.Success, msg)
		} else {
			// Probe should be on hold and will not be executed anymore
			// when successThreshold is met
			if !w.onHold {
				t.Errorf("Prober should be on hold because successThreshold is exceeded")
			}
			// Meeting or exceeding successThreshold should cause resultRun to reset to 0
			if w.resultRun != 0 {
				t.Errorf("Prober resultRun should be 0, but %d", w.resultRun)
			}
		}
	}
}

func TestStartupProbeFailureThreshold(t *testing.T) {
	ctx := context.Background()
	m := newTestManager()
	successThreshold := 1
	failureThreshold := 3
	w := newTestWorker(m, startup, v1.Probe{SuccessThreshold: int32(successThreshold), FailureThreshold: int32(failureThreshold)})
	m.statusManager.SetPodStatus(w.pod, getTestNotRunningStatus())
	m.prober.exec = fakeExecProber{probe.Failure, nil}

	for i := 0; i < failureThreshold+1; i++ {
		if i < failureThreshold {
			// Probe should not be on hold and will continue to be excuted
			// until failureThreshold is met
			if w.onHold {
				t.Errorf("Prober should not be on hold")
			}
			msg := fmt.Sprintf("%d failure", i+1)
			expectContinue(t, w, w.doProbe(ctx), msg)
			switch i {
			case 0, 1:
				// At this point, the expected result is Unknown
				// because w.resultsManager.Set() will be called after FailureThreshold is reached
				expectResult(t, w, results.Unknown, msg)
				// resultRun should be incremented until failureThreshold is met
				if w.resultRun != i+1 {
					t.Errorf("Prober resultRun should be %d, but %d", i+1, w.resultRun)
				}
			case 2:
				// The expected result is Failure
				// because w.resultsManager.Set() will be called due to resultRun reaching failureThreshold,
				// updating the cached result to Failure.
				// After that, resultRun will be reset to 0.
				expectResult(t, w, results.Failure, msg)
				// Meeting failureThreshold should cause resultRun to reset to 0
				if w.resultRun != 0 {
					t.Errorf("Prober resultRun should be 0, but %d", w.resultRun)
				}
			}
		} else {
			// Probe should be on hold and will not be executed anymore
			// when failureThreshold is met
			if !w.onHold {
				t.Errorf("Prober should be on hold because failureThreshold is exceeded")
			}
			// Exceeding failureThreshold should cause resultRun to reset to 0
			if w.resultRun != 0 {
				t.Errorf("Prober resultRun should be 0, but %d", w.resultRun)
			}
		}
	}
}

func TestCleanUp(t *testing.T) {
	m := newTestManager()

	for _, probeType := range [...]probeType{liveness, readiness, startup} {
		key := probeKey{testPodUID, testContainerName, probeType}
		w := newTestWorker(m, probeType, v1.Probe{})
		m.statusManager.SetPodStatus(w.pod, getTestRunningStatusWithStarted(probeType != startup))
		go w.run()
		m.workers[key] = w

		// Wait for worker to run.
		condition := func() (bool, error) {
			ready, _ := resultsManager(m, probeType).Get(testContainerID)
			return ready == results.Success, nil
		}
		if ready, _ := condition(); !ready {
			if err := wait.Poll(100*time.Millisecond, wait.ForeverTestTimeout, condition); err != nil {
				t.Fatalf("[%s] Error waiting for worker ready: %v", probeType, err)
			}
		}

		for i := 0; i < 10; i++ {
			w.stop() // Stop should be callable multiple times without consequence.
		}
		if err := waitForWorkerExit(t, m, []probeKey{key}); err != nil {
			t.Fatalf("[%s] error waiting for worker exit: %v", probeType, err)
		}

		if _, ok := resultsManager(m, probeType).Get(testContainerID); ok {
			t.Errorf("[%s] Expected result to be cleared.", probeType)
		}
		if _, ok := m.workers[key]; ok {
			t.Errorf("[%s] Expected worker to be cleared.", probeType)
		}
	}
}

func expectResult(t *testing.T, w *worker, expectedResult results.Result, msg string) {
	result, ok := resultsManager(w.probeManager, w.probeType).Get(w.containerID)
	if !ok {
		t.Errorf("[%s - %s] Expected result to be set, but was not set", w.probeType, msg)
	} else if result != expectedResult {
		t.Errorf("[%s - %s] Expected result to be %v, but was %v",
			w.probeType, msg, expectedResult, result)
	}
}

func expectContinue(t *testing.T, w *worker, c bool, msg string) {
	if !c {
		t.Errorf("[%s - %s] Expected to continue, but did not", w.probeType, msg)
	}
}

func resultsManager(m *manager, probeType probeType) results.Manager {
	switch probeType {
	case readiness:
		return m.readinessManager
	case liveness:
		return m.livenessManager
	case startup:
		return m.startupManager
	}
	panic(fmt.Errorf("Unhandled case: %v", probeType))
}

func TestOnHoldOnLivenessOrStartupCheckFailure(t *testing.T) {
	ctx := context.Background()
	m := newTestManager()

	for _, probeType := range [...]probeType{liveness, startup} {
		w := newTestWorker(m, probeType, v1.Probe{SuccessThreshold: 1, FailureThreshold: 1})
		status := getTestRunningStatusWithStarted(probeType != startup)
		m.statusManager.SetPodStatus(w.pod, status)

		// First probe should fail.
		m.prober.exec = fakeExecProber{probe.Failure, nil}
		msg := "first probe"
		expectContinue(t, w, w.doProbe(ctx), msg)
		expectResult(t, w, results.Failure, msg)
		if !w.onHold {
			t.Errorf("Prober should be on hold due to %s check failure", probeType)
		}
		// Set fakeExecProber to return success. However, the result will remain
		// failure because the worker is on hold and won't probe.
		m.prober.exec = fakeExecProber{probe.Success, nil}
		msg = "while on hold"
		expectContinue(t, w, w.doProbe(ctx), msg)
		expectResult(t, w, results.Failure, msg)
		if !w.onHold {
			t.Errorf("Prober should be on hold due to %s check failure", probeType)
		}

		// Set a new container ID to lift the hold. The next probe will succeed.
		status.ContainerStatuses[0].ContainerID = "test://newCont_ID"
		m.statusManager.SetPodStatus(w.pod, status)
		msg = "hold lifted"
		expectContinue(t, w, w.doProbe(ctx), msg)
		expectResult(t, w, results.Success, msg)
		if probeType == liveness && w.onHold {
			t.Errorf("Prober should not be on hold anymore")
		} else if probeType == startup && !w.onHold {
			t.Errorf("Prober should be on hold due to %s check success", probeType)
		}
	}
}

func TestResultRunOnLivenessCheckFailure(t *testing.T) {
	ctx := context.Background()
	m := newTestManager()
	w := newTestWorker(m, liveness, v1.Probe{SuccessThreshold: 1, FailureThreshold: 3})
	m.statusManager.SetPodStatus(w.pod, getTestRunningStatus())

	m.prober.exec = fakeExecProber{probe.Success, nil}
	msg := "initial probe success"
	expectContinue(t, w, w.doProbe(ctx), msg)
	expectResult(t, w, results.Success, msg)
	if w.resultRun != 1 {
		t.Errorf("Prober resultRun should be 1")
	}

	m.prober.exec = fakeExecProber{probe.Failure, nil}
	msg = "probe failure, result success"
	expectContinue(t, w, w.doProbe(ctx), msg)
	expectResult(t, w, results.Success, msg)
	if w.resultRun != 1 {
		t.Errorf("Prober resultRun should be 1")
	}

	m.prober.exec = fakeExecProber{probe.Failure, nil}
	msg = "2nd probe failure, result success"
	expectContinue(t, w, w.doProbe(ctx), msg)
	expectResult(t, w, results.Success, msg)
	if w.resultRun != 2 {
		t.Errorf("Prober resultRun should be 2")
	}

	// Exceeding FailureThreshold should cause resultRun to
	// reset to 0 so that the probe on the restarted pod
	// also gets FailureThreshold attempts to succeed.
	m.prober.exec = fakeExecProber{probe.Failure, nil}
	msg = "3rd probe failure, result failure"
	expectContinue(t, w, w.doProbe(ctx), msg)
	expectResult(t, w, results.Failure, msg)
	if w.resultRun != 0 {
		t.Errorf("Prober resultRun should be reset to 0")
	}
}

func TestResultRunOnStartupCheckFailure(t *testing.T) {
	ctx := context.Background()
	m := newTestManager()
	w := newTestWorker(m, startup, v1.Probe{SuccessThreshold: 1, FailureThreshold: 3})
	m.statusManager.SetPodStatus(w.pod, getTestRunningStatusWithStarted(false))

	// Below FailureThreshold leaves probe state unchanged
	// which is failed for startup at first.
	m.prober.exec = fakeExecProber{probe.Failure, nil}
	msg := "probe failure, result unknown"
	expectContinue(t, w, w.doProbe(ctx), msg)
	expectResult(t, w, results.Unknown, msg)
	if w.resultRun != 1 {
		t.Errorf("Prober resultRun should be 1")
	}

	m.prober.exec = fakeExecProber{probe.Failure, nil}
	msg = "2nd probe failure, result unknown"
	expectContinue(t, w, w.doProbe(ctx), msg)
	expectResult(t, w, results.Unknown, msg)
	if w.resultRun != 2 {
		t.Errorf("Prober resultRun should be 2")
	}

	// Exceeding FailureThreshold should cause resultRun to
	// reset to 0 so that the probe on the restarted pod
	// also gets FailureThreshold attempts to succeed.
	m.prober.exec = fakeExecProber{probe.Failure, nil}
	msg = "3rd probe failure, result failure"
	expectContinue(t, w, w.doProbe(ctx), msg)
	expectResult(t, w, results.Failure, msg)
	if w.resultRun != 0 {
		t.Errorf("Prober resultRun should be reset to 0")
	}
}

func TestLivenessProbeDisabledByStarted(t *testing.T) {
	ctx := context.Background()
	m := newTestManager()
	w := newTestWorker(m, liveness, v1.Probe{SuccessThreshold: 1, FailureThreshold: 1})
	m.statusManager.SetPodStatus(w.pod, getTestRunningStatusWithStarted(false))
	// livenessProbe fails, but is disabled
	m.prober.exec = fakeExecProber{probe.Failure, nil}
	msg := "Not started, probe failure, result success"
	expectContinue(t, w, w.doProbe(ctx), msg)
	expectResult(t, w, results.Success, msg)
	// setting started state
	m.statusManager.SetContainerStartup(w.pod.UID, w.containerID, true)
	// livenessProbe fails
	m.prober.exec = fakeExecProber{probe.Failure, nil}
	msg = "Started, probe failure, result failure"
	expectContinue(t, w, w.doProbe(ctx), msg)
	expectResult(t, w, results.Failure, msg)
}

func TestStartupProbeDisabledByStarted(t *testing.T) {
	ctx := context.Background()
	m := newTestManager()
	w := newTestWorker(m, startup, v1.Probe{SuccessThreshold: 1, FailureThreshold: 2})
	m.statusManager.SetPodStatus(w.pod, getTestRunningStatusWithStarted(false))
	// startupProbe fails < FailureThreshold, stays unknown
	m.prober.exec = fakeExecProber{probe.Failure, nil}
	msg := "Not started, probe failure, result unknown"
	expectContinue(t, w, w.doProbe(ctx), msg)
	expectResult(t, w, results.Unknown, msg)
	// startupProbe succeeds
	m.prober.exec = fakeExecProber{probe.Success, nil}
	msg = "Started, probe success, result success"
	expectContinue(t, w, w.doProbe(ctx), msg)
	expectResult(t, w, results.Success, msg)
	// setting started state
	m.statusManager.SetContainerStartup(w.pod.UID, w.containerID, true)
	// startupProbe fails, but is disabled
	m.prober.exec = fakeExecProber{probe.Failure, nil}
	msg = "Started, probe failure, result success"
	expectContinue(t, w, w.doProbe(ctx), msg)
	expectResult(t, w, results.Success, msg)
}
