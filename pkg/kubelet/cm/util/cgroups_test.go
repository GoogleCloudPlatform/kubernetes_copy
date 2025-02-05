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

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestInvalidCPUSharesToCPUWeight(t *testing.T) {
	req := require.New(t)
	_, err := CPUSharesToCPUWeight(0)
	req.Error(err)

	_, err = CPUSharesToCPUWeight(minCPUShares - 1)
	req.Error(err)

	_, err = CPUSharesToCPUWeight(maxCPUShares + 1)
	req.Error(err)
}

func TestInvalidCPUWeightToCPUShares(t *testing.T) {
	req := require.New(t)
	_, err := CPUWeightToCPUShares(0)
	req.Error(err)

	_, err = CPUWeightToCPUShares(minCPUWeight - 1)
	req.Error(err)

	_, err = CPUWeightToCPUShares(maxCPUWeight + 1)
	req.Error(err)
}

func TestValidCPUSharesToCPUWeight(t *testing.T) {
	req := require.New(t)
	weight, err := CPUSharesToCPUWeight(123)
	req.NoError(err)
	req.Equal(uint64(5), weight)

	weight, err = CPUSharesToCPUWeight(12345)
	req.NoError(err)
	req.Equal(uint64(471), weight)
}

func TestValidCPUWeightToCPUShares(t *testing.T) {
	req := require.New(t)
	weight, err := CPUWeightToCPUShares(123)
	req.NoError(err)
	req.Equal(uint64(3200), weight)

	weight, err = CPUWeightToCPUShares(1234)
	req.NoError(err)
	req.Equal(uint64(32327), weight)
}
