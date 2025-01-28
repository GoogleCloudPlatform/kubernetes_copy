//go:build linux
// +build linux

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

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/tools/events"
	"k8s.io/klog/v2"

	"github.com/google/cadvisor/utils/oomparser"
)

type streamer interface {
	StreamOoms(chan<- *oomparser.OomInstance)
}

var _ streamer = &oomparser.OomParser{}

type realWatcher struct {
	recorder    events.EventRecorder
	oomStreamer streamer
}

var _ Watcher = &realWatcher{}

// NewWatcher creates and initializes a OOMWatcher backed by Cadvisor as
// the oom streamer.
func NewWatcher(recorder events.EventRecorder) (Watcher, error) {
	// for test purpose
	_, ok := recorder.(*events.FakeRecorder)
	if ok {
		return nil, nil
	}

	oomStreamer, err := oomparser.New()
	if err != nil {
		return nil, err
	}

	watcher := &realWatcher{
		recorder:    recorder,
		oomStreamer: oomStreamer,
	}

	return watcher, nil
}

const (
	systemOOMEvent           = "SystemOOM"
	recordEventContainerName = "/"
)

// Start watches for system oom's and records an event for every system oom encountered.
func (ow *realWatcher) Start(ref *v1.ObjectReference) error {
	outStream := make(chan *oomparser.OomInstance, 10)
	go ow.oomStreamer.StreamOoms(outStream)

	go func() {
		defer runtime.HandleCrash()

		for event := range outStream {
			if event.VictimContainerName == recordEventContainerName {
				klog.V(1).InfoS("Got sys oom event", "event", event)
				eventMsg := "System OOM encountered"
				if event.ProcessName != "" && event.Pid != 0 {
					eventMsg = fmt.Sprintf("%s, victim process: %s, pid: %d", eventMsg, event.ProcessName, event.Pid)
				}
				ow.recorder.Eventf(ref, nil, v1.EventTypeWarning, systemOOMEvent, "OOM", eventMsg)
			}
		}
		klog.ErrorS(nil, "Unexpectedly stopped receiving OOM notifications")
	}()
	return nil
}
