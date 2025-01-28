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

package oom

import (
	"fmt"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/events"

	"github.com/google/cadvisor/utils/oomparser"
	"github.com/stretchr/testify/assert"
)

type fakeStreamer struct {
	oomInstancesToStream []*oomparser.OomInstance
}

func (fs *fakeStreamer) StreamOoms(outStream chan<- *oomparser.OomInstance) {
	for _, oomInstance := range fs.oomInstancesToStream {
		outStream <- oomInstance
	}
}

// TestWatcherRecordsEventsForOomEvents ensures that our OomInstances coming
// from `StreamOoms` are translated into events in our recorder.
func TestWatcherRecordsEventsForOomEvents(t *testing.T) {
	oomInstancesToStream := []*oomparser.OomInstance{
		{
			Pid:                 1000,
			ProcessName:         "fakeProcess",
			TimeOfDeath:         time.Now(),
			ContainerName:       recordEventContainerName + "some-container",
			VictimContainerName: recordEventContainerName,
		},
	}
	numExpectedOomEvents := len(oomInstancesToStream)

	fakeStreamer := &fakeStreamer{
		oomInstancesToStream: oomInstancesToStream,
	}

	fakeRecorder := events.NewFakeRecorder(numExpectedOomEvents)
	node := &v1.ObjectReference{}

	oomWatcher := &realWatcher{
		recorder:    fakeRecorder,
		oomStreamer: fakeStreamer,
	}
	assert.NoError(t, oomWatcher.Start(node))

	eventsRecorded := getRecordedEvents(fakeRecorder, numExpectedOomEvents)
	assert.Len(t, eventsRecorded, numExpectedOomEvents)
}

func getRecordedEvents(fakeRecorder *events.FakeRecorder, numExpectedOomEvents int) []string {
	eventsRecorded := []string{}

	select {
	case event := <-fakeRecorder.Events:
		eventsRecorded = append(eventsRecorded, event)

		if len(eventsRecorded) == numExpectedOomEvents {
			break
		}
	case <-time.After(10 * time.Second):
		break
	}

	return eventsRecorded
}

// TestWatcherRecordsEventsForOomEventsCorrectContainerName verifies that we
// only record OOM events when the container name is the one for which we want
// to record events (i.e. /).
func TestWatcherRecordsEventsForOomEventsCorrectContainerName(t *testing.T) {
	// By "incorrect" container name, we mean a container name for which we
	// don't want to record an oom event.
	numOomEventsWithIncorrectContainerName := 1
	oomInstancesToStream := []*oomparser.OomInstance{
		{
			Pid:                 1000,
			ProcessName:         "fakeProcess",
			TimeOfDeath:         time.Now(),
			ContainerName:       recordEventContainerName + "some-container",
			VictimContainerName: recordEventContainerName,
		},
		{
			Pid:                 1000,
			ProcessName:         "fakeProcess",
			TimeOfDeath:         time.Now(),
			ContainerName:       recordEventContainerName + "kubepods/some-container",
			VictimContainerName: recordEventContainerName + "kubepods",
		},
	}
	numExpectedOomEvents := len(oomInstancesToStream) - numOomEventsWithIncorrectContainerName

	fakeStreamer := &fakeStreamer{
		oomInstancesToStream: oomInstancesToStream,
	}

	fakeRecorder := events.NewFakeRecorder(numExpectedOomEvents)
	node := &v1.ObjectReference{}

	oomWatcher := &realWatcher{
		recorder:    fakeRecorder,
		oomStreamer: fakeStreamer,
	}
	assert.NoError(t, oomWatcher.Start(node))

	eventsRecorded := getRecordedEvents(fakeRecorder, numExpectedOomEvents)
	assert.Len(t, eventsRecorded, numExpectedOomEvents)
}

// TestWatcherRecordsEventsForOomEventsWithAdditionalInfo verifies that our the
// emitted event has the proper pid/process data when appropriate.
func TestWatcherRecordsEventsForOomEventsWithAdditionalInfo(t *testing.T) {
	// The process and event info should appear in the event message.
	eventPid := 1000
	processName := "fakeProcess"

	oomInstancesToStream := []*oomparser.OomInstance{
		{
			Pid:                 eventPid,
			ProcessName:         processName,
			TimeOfDeath:         time.Now(),
			ContainerName:       recordEventContainerName + "some-container",
			VictimContainerName: recordEventContainerName,
		},
	}
	numExpectedOomEvents := len(oomInstancesToStream)

	fakeStreamer := &fakeStreamer{
		oomInstancesToStream: oomInstancesToStream,
	}

	fakeRecorder := events.NewFakeRecorder(numExpectedOomEvents)
	node := &v1.ObjectReference{}

	oomWatcher := &realWatcher{
		recorder:    fakeRecorder,
		oomStreamer: fakeStreamer,
	}
	assert.NoError(t, oomWatcher.Start(node))

	eventsRecorded := getRecordedEvents(fakeRecorder, numExpectedOomEvents)

	assert.Len(t, eventsRecorded, numExpectedOomEvents)
	assert.Contains(t, eventsRecorded[0], systemOOMEvent)
	assert.Contains(t, eventsRecorded[0], fmt.Sprintf("pid: %d", eventPid))
	assert.Contains(t, eventsRecorded[0], fmt.Sprintf("victim process: %s", processName))
}
