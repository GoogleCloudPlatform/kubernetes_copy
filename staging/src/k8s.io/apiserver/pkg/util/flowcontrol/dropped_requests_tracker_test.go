/*
Copyright 2021 The Kubernetes Authors.

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

package flowcontrol

import (
	"fmt"
	"sync"
	"testing"
	"time"

	testingclock "k8s.io/utils/clock/testing"
)

func TestDroppedRequestsTracker(t *testing.T) {
	fakeClock := testingclock.NewFakeClock(time.Now())
	tracker := newDroppedRequestsTracker(fakeClock.Now)

	// The following table represents the list over time of:
	// - seconds elapsed (as computed since the initial time)
	// - requests that will be recorded as dropped in a current second
	steps := []struct {
		secondsElapsed int
		// droppedRequests is the number of requests to drop, after
		// secondsElapsed.
		droppedRequests int
		// retryAfter is the expected retryAfter after all dropped
		// requests are recorded via RecordDroppedRequest.
		retryAfter int64
	}{
		{secondsElapsed: 0, droppedRequests: 5, retryAfter: 1},
		{secondsElapsed: 1, droppedRequests: 11, retryAfter: 2},
		// Check that we don't bump immediately after despite
		// multiple dropped requests.
		{secondsElapsed: 2, droppedRequests: 1, retryAfter: 2},
		{secondsElapsed: 3, droppedRequests: 11, retryAfter: 4},
		{secondsElapsed: 4, droppedRequests: 1, retryAfter: 4},
		{secondsElapsed: 7, droppedRequests: 1, retryAfter: 8},
		{secondsElapsed: 11, droppedRequests: 1, retryAfter: 8},
		{secondsElapsed: 15, droppedRequests: 1, retryAfter: 7},
		{secondsElapsed: 17, droppedRequests: 1, retryAfter: 6},
		{secondsElapsed: 21, droppedRequests: 14, retryAfter: 5},
		{secondsElapsed: 22, droppedRequests: 1, retryAfter: 10},
	}

	for i, step := range steps {
		secondsToAdvance := step.secondsElapsed
		if i > 0 {
			secondsToAdvance -= steps[i-1].secondsElapsed
		}
		fakeClock.Step(time.Duration(secondsToAdvance) * time.Second)

		// Record all dropped requests and recompute retryAfter.
		for r := 0; r < step.droppedRequests; r++ {
			tracker.RecordDroppedRequest("pl")
		}
		if retryAfter := tracker.GetRetryAfter("pl"); retryAfter != step.retryAfter {
			t.Errorf("Unexpected retryAfter: %v, expected: %v", retryAfter, step.retryAfter)
		}

	}
}

func TestDroppedRequestsTrackerPLIndependent(t *testing.T) {
	fakeClock := testingclock.NewFakeClock(time.Now())
	tracker := newDroppedRequestsTracker(fakeClock.Now)

	// Report single dropped requests in multiple PLs.
	// Validate if RetryAfter isn't bumped next second.
	for i := 0; i < 10; i++ {
		tracker.RecordDroppedRequest(fmt.Sprintf("pl-%d", i))
	}
	fakeClock.Step(time.Second)
	for i := 0; i < 10; i++ {
		tracker.RecordDroppedRequest(fmt.Sprintf("pl-%d", i))
		retryAfter := tracker.GetRetryAfter(fmt.Sprintf("pl-%d", i))
		if retryAfter != 1 {
			t.Errorf("Unexpected retryAfter for pl-%d: %v", i, retryAfter)
		}
	}

	// Record few dropped requests on a single PL.
	// Validate that RetryAfter is bumped only for this PL.
	for i := 0; i < 5; i++ {
		tracker.RecordDroppedRequest("pl-0")
	}
	fakeClock.Step(time.Second)
	for i := 0; i < 10; i++ {
		tracker.RecordDroppedRequest(fmt.Sprintf("pl-%d", i))
		retryAfter := tracker.GetRetryAfter(fmt.Sprintf("pl-%d", i))
		switch i {
		case 0:
			if retryAfter != 2 {
				t.Errorf("Unexpected retryAfter for pl-0: %v", retryAfter)
			}
		default:
			if retryAfter != 1 {
				t.Errorf("Unexpected retryAfter for pl-%d: %v", i, retryAfter)
			}
		}
	}
	// Validate also PL for which no dropped requests was recorded.
	if retryAfter := tracker.GetRetryAfter("other-pl"); retryAfter != 1 {
		t.Errorf("Unexpected retryAfter for other-pl: %v", retryAfter)
	}
}

func BenchmarkDroppedRequestsTracker(b *testing.B) {
	b.StopTimer()

	fakeClock := testingclock.NewFakeClock(time.Now())
	tracker := newDroppedRequestsTracker(fakeClock.Now)

	startCh := make(chan struct{})
	wg := sync.WaitGroup{}
	numPLs := 5
	// For all `numPLs` priority levels, create b.N workers each
	// of which will try to record a dropped request every 100ms
	// with a random jitter.
	for i := 0; i < numPLs; i++ {
		plName := fmt.Sprintf("priority-level-%d", i)
		for i := 0; i < b.N; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-startCh

				for a := 0; a < 5; a++ {
					tracker.RecordDroppedRequest(plName)
					time.Sleep(25 * time.Millisecond)
				}
			}()
		}
	}
	// Time-advancing goroutine.
	stopCh := make(chan struct{})
	timeWg := sync.WaitGroup{}
	timeWg.Add(1)
	go func() {
		defer timeWg.Done()
		for {
			select {
			case <-stopCh:
				return
			case <-time.After(25 * time.Millisecond):
				fakeClock.Step(time.Second)
			}
		}
	}()

	b.StartTimer()
	close(startCh)
	wg.Wait()

	close(stopCh)
	timeWg.Wait()
}
