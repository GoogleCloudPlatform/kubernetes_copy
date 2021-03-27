/*
Copyright 2016 The Kubernetes Authors.

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

package kubelet

import (
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/kubernetes/pkg/kubelet/status"
	"k8s.io/utils/clock"
	testingclock "k8s.io/utils/clock/testing"
)

// mockPodStatusProvider returns the status on the specified pod
type mockPodStatusProvider struct {
	pods []*v1.Pod
}

// GetPodStatus returns the status on the associated pod with matching uid (if found)
func (m *mockPodStatusProvider) GetPodStatus(uid types.UID) (v1.PodStatus, bool) {
	for _, pod := range m.pods {
		if pod.UID == uid {
			return pod.Status, true
		}
	}
	return v1.PodStatus{}, false
}

func TestNewActiveDeadlineHandler(t *testing.T) {
	pods := newTestPods(1)
	podStatusProvider := &mockPodStatusProvider{pods: pods}
	fakeRecorder := &events.FakeRecorder{}
	fakeClock := testingclock.NewFakeClock(time.Now())

	testCases := []struct {
		podStatusProvider status.PodStatusProvider
		recorder          events.EventRecorder
		clock             clock.Clock
	}{
		{podStatusProvider, fakeRecorder, fakeClock},
		{podStatusProvider, fakeRecorder, nil},
		{podStatusProvider, nil, fakeClock},
		{podStatusProvider, nil, nil},
		{nil, fakeRecorder, fakeClock},
		{nil, fakeRecorder, nil},
		{nil, nil, fakeClock},
		{nil, nil, nil},
	}

	for i, testCase := range testCases {
		actual, err := newActiveDeadlineHandler(testCase.podStatusProvider, testCase.recorder, testCase.clock)

		// 0th case is the only one expected to pass, and is kept for coverage and confidence check
		if i == 0 {
			expected := &activeDeadlineHandler{
				clock:             fakeClock,
				podStatusProvider: podStatusProvider,
				recorder:          fakeRecorder,
			}
			if !reflect.DeepEqual(actual, expected) {
				t.Errorf("[%d] newActiveDeadlineHandler expected %#v, got %#v", i, expected, actual)
			}
			assert.NoError(t, err)

			continue
		}

		assert.Error(t, err)
	}
}

// TestActiveDeadlineHandler verifies the active deadline handler functions as expected.
func TestActiveDeadlineHandler(t *testing.T) {
	pods := newTestPods(5)
	fakeClock := testingclock.NewFakeClock(time.Now())
	podStatusProvider := &mockPodStatusProvider{pods: pods}
	fakeRecorder := &events.FakeRecorder{}
	handler, err := newActiveDeadlineHandler(podStatusProvider, fakeRecorder, fakeClock)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	now := metav1.Now()
	startTime := metav1.NewTime(now.Time.Add(-1 * time.Minute))

	// this pod has exceeded its active deadline
	exceededActiveDeadlineSeconds := int64(30)
	pods[0].Status.StartTime = &startTime
	pods[0].Spec.ActiveDeadlineSeconds = &exceededActiveDeadlineSeconds

	// this pod has not exceeded its active deadline
	notYetActiveDeadlineSeconds := int64(120)
	pods[1].Status.StartTime = &startTime
	pods[1].Spec.ActiveDeadlineSeconds = &notYetActiveDeadlineSeconds

	// this pod has no deadline
	pods[2].Status.StartTime = &startTime
	pods[2].Spec.ActiveDeadlineSeconds = nil

	// this pod has no start time
	pods[3].Status.StartTime = nil
	pods[3].Spec.ActiveDeadlineSeconds = &notYetActiveDeadlineSeconds

	testCases := []struct {
		pod      *v1.Pod
		expected bool
	}{{pods[0], true}, {pods[1], false}, {pods[2], false}, {pods[3], false}, {pods[4], false}}

	for i, testCase := range testCases {
		if actual := handler.ShouldSync(testCase.pod); actual != testCase.expected {
			t.Errorf("[%d] ShouldSync expected %#v, got %#v", i, testCase.expected, actual)
		}
		actual := handler.ShouldEvict(testCase.pod)
		if actual.Evict != testCase.expected {
			t.Errorf("[%d] ShouldEvict.Evict expected %#v, got %#v", i, testCase.expected, actual.Evict)
		}
		if testCase.expected {
			if actual.Reason != reason {
				t.Errorf("[%d] ShouldEvict.Reason expected %#v, got %#v", i, message, actual.Reason)
			}
			if actual.Message != message {
				t.Errorf("[%d] ShouldEvict.Message expected %#v, got %#v", i, message, actual.Message)
			}
		}
	}
}
