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

package util

import "fmt"

const (
	minCPUShares = uint64(2)
	maxCPUShares = uint64(262144)
	minCPUWeight = uint64(1)
	maxCPUWeight = uint64(10000)
)

// Convert cgroup v1 cpu.shares value to cgroup v2 cpu.weight
// https://github.com/kubernetes/enhancements/tree/master/keps/sig-node/2254-cgroup-v2#phase-1-convert-from-cgroups-v1-settings-to-v2
func CPUSharesToCPUWeight(cpuShares uint64) (uint64, error) {
	if err := checkBoundaries(cpuShares, minCPUShares, maxCPUShares); err != nil {
		return 0, fmt.Errorf("CPU shares %s", err.Error())
	}

	return (((cpuShares - 2) * 9999) / 262142) + 1, nil
}

// Convert cgroup v2 cpu.weight value to cgroup v1 cpu.shares
// https://github.com/kubernetes/enhancements/tree/master/keps/sig-node/2254-cgroup-v2#phase-1-convert-from-cgroups-v1-settings-to-v2
func CPUWeightToCPUShares(cpuWeight uint64) (uint64, error) {
	if err := checkBoundaries(cpuWeight, minCPUWeight, maxCPUWeight); err != nil {
		return 0, fmt.Errorf("CPU weight %s", err.Error())
	}

	return (((cpuWeight - 1) * 262142) / 9999) + 2, nil
}

func checkBoundaries(valueToValidate, minAllowed, maxAllowed uint64) error {
	if valueToValidate <= 0 {
		return fmt.Errorf("must be greater than zero, but equals to %d", valueToValidate)
	}
	if valueToValidate < minAllowed {
		return fmt.Errorf("must be greater than or equal to %d, but equals to %d", minAllowed, valueToValidate)
	}
	if valueToValidate > maxAllowed {
		return fmt.Errorf("must be less than or equal to %d, but equals to %d", maxAllowed, valueToValidate)
	}

	return nil
}
