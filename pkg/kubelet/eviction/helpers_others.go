//go:build !windows
// +build !windows

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

package eviction

import (
	"k8s.io/apimachinery/pkg/api/resource"
	statsapi "k8s.io/kubelet/pkg/apis/stats/v1alpha1"
)

func makeMemoryAvailableSignalObservation(summary *statsapi.Summary, accessibleSwap, swapUsageBytes uint64) *signalObservation {
	if memory := summary.Node.Memory; memory != nil && memory.AvailableBytes != nil && memory.WorkingSetBytes != nil {
		return &signalObservation{
			available: resource.NewQuantity(int64(*memory.AvailableBytes)+int64(accessibleSwap)-int64(swapUsageBytes), resource.BinarySI),
			capacity:  resource.NewQuantity(int64(*memory.AvailableBytes+*memory.WorkingSetBytes)+int64(accessibleSwap), resource.BinarySI),
			time:      memory.Time,
		}
	}

	return nil
}
