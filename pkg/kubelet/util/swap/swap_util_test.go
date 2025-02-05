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

package swap

import (
	"fmt"
	"testing"
)

func TestIsSwapEnabled(t *testing.T) {
	testCases := []struct {
		name             string
		procSwapsContent string
		expectedEnabled  bool
	}{
		{
			name:             "empty",
			procSwapsContent: "",
			expectedEnabled:  false,
		},
		{
			name: "with swap enabled, one partition",
			procSwapsContent: `
Filename				Type		Size		Used		Priority
/dev/dm-1               partition	33554428	0		-2
`,
			expectedEnabled: true,
		},
		{
			name: "with swap enabled, 2 partitions",
			procSwapsContent: `
Filename				Type		Size		Used		Priority
/dev/dm-1               partition	33554428	0		-2
/dev/zram0              partition	8388604		0		100
`,
			expectedEnabled: true,
		},
		{
			name: "empty lines",
			procSwapsContent: `

`,
			expectedEnabled: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			isEnabled := isSwapOnAccordingToProcSwaps([]byte(tc.procSwapsContent))
			if isEnabled != tc.expectedEnabled {
				t.Errorf("expected %v, got %v", tc.expectedEnabled, isEnabled)
			}
		})
	}
}

func TestCalcSwapForBurstablePods(t *testing.T) {
	const gb int64 = 1024 * 1024 * 1024

	testCases := []struct {
		containerMemoryRequest int64
		nodeTotalMemory        int64
		totalPodsSwapAvailable int64
		expectedSwap           int64
	}{
		{
			containerMemoryRequest: 1 * gb,
			nodeTotalMemory:        80 * gb,
			totalPodsSwapAvailable: 4 * gb,
			expectedSwap:           53687091,
		},
		{
			containerMemoryRequest: 2 * gb,
			nodeTotalMemory:        40 * gb,
			totalPodsSwapAvailable: 2 * gb,
			expectedSwap:           107374182,
		},
		{
			containerMemoryRequest: 5 * gb,
			nodeTotalMemory:        150 * gb,
			totalPodsSwapAvailable: 40 * gb,
			expectedSwap:           1431655765,
		},
	}

	for _, tc := range testCases {
		name := fmt.Sprintf("memory request bytes: %d, node total memory: %d, total swap available: %d", tc.containerMemoryRequest, tc.nodeTotalMemory, tc.totalPodsSwapAvailable)
		t.Run(name, func(t *testing.T) {
			swap, err := CalcSwapForBurstablePods(tc.containerMemoryRequest, tc.nodeTotalMemory, tc.totalPodsSwapAvailable)
			if err != nil {
				t.Errorf("received an error: %v", err)
			}

			if swap != tc.expectedSwap {
				t.Errorf("expected swap to be %v, got %v", tc.expectedSwap, swap)
			}
		})
	}
}
