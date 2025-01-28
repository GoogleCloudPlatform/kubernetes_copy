/*
Copyright 2024 The Kubernetes Authors.

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

package statusz

import (
	"time"

	"k8s.io/apimachinery/pkg/util/version"
	"k8s.io/klog/v2"

	"k8s.io/component-base/compatibility"
	compbasemetrics "k8s.io/component-base/metrics"
	utilversion "k8s.io/component-base/version"
)

type statuszRegistry interface {
	processStartTime() time.Time
	goVersion() string
	binaryVersion() *version.Version
	emulationVersion() *version.Version
}

type registry struct {
	// componentGlobalsRegistry compatibility.ComponentGlobalsRegistry
	effectiveVersion compatibility.EffectiveVersion
}

func (*registry) processStartTime() time.Time {
	start, err := compbasemetrics.GetProcessStart()
	if err != nil {
		klog.Errorf("Could not get process start time, %v", err)
	}

	return time.Unix(int64(start), 0)
}

func (*registry) goVersion() string {
	return utilversion.Get().GoVersion
}

func (r *registry) binaryVersion() *version.Version {
	if r.effectiveVersion != nil {
		return r.effectiveVersion.BinaryVersion()
	}
	return version.MustParse(utilversion.Get().String())
}

func (r *registry) emulationVersion() *version.Version {
	if r.effectiveVersion != nil {
		return r.effectiveVersion.EmulationVersion()
	}

	return nil
}
