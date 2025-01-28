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

package stats

import (
	"time"

	"k8s.io/client-go/tools/events"
)

// ResourceAnalyzer provides statistics on node resource consumption
type ResourceAnalyzer interface {
	Start()

	fsResourceAnalyzerInterface
	SummaryProvider
}

// resourceAnalyzer implements ResourceAnalyzer
type resourceAnalyzer struct {
	*fsResourceAnalyzer
	SummaryProvider
}

var _ ResourceAnalyzer = &resourceAnalyzer{}

// NewResourceAnalyzer returns a new ResourceAnalyzer
func NewResourceAnalyzer(statsProvider Provider, calVolumeFrequency time.Duration, eventRecorder events.EventRecorder) ResourceAnalyzer {
	fsAnalyzer := newFsResourceAnalyzer(statsProvider, calVolumeFrequency, eventRecorder)
	summaryProvider := NewSummaryProvider(statsProvider)
	return &resourceAnalyzer{fsAnalyzer, summaryProvider}
}

// Start starts background functions necessary for the ResourceAnalyzer to function
func (ra *resourceAnalyzer) Start() {
	ra.fsResourceAnalyzer.Start()
}
