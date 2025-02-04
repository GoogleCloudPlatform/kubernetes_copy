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

package metrics

import (
	"os"
	"testing"

	"k8s.io/component-base/metrics/testutil"
)

const imagePullDurationKey = "kubelet_" + ImagePullDurationKey

func TestImagePullDurationMetric(t *testing.T) {
	t.Run("register image pull duration", func(t *testing.T) {
		Register()
		defer clearMetrics()

		// Pairs of image size in bytes and pull duration in seconds
		dataPoints := [][]float64{
			// 0 bytes, 0 seconds
			{0, 0},
			// 5MB, 10 seconds
			{5 * 1024 * 1024, 10},
			// 15MB, 20 seconds
			{15 * 1024 * 1024, 20},
			// 500 MB, 200 seconds
			{500 * 1024 * 1024, 200},
			// 15 GB, 6000 seconds,
			{15 * 1024 * 1024 * 1024, 6000},
			// 200 GB, 10000 seconds
			{200 * 1024 * 1024 * 1024, 10000},
		}

		for _, dp := range dataPoints {
			imageSize := int64(dp[0])
			duration := dp[1]
			t.Log(imageSize, duration)
			t.Log(GetImageSizeBucket(uint64(imageSize)))
			ImagePullDuration.WithLabelValues(GetImageSizeBucket(uint64(imageSize))).Observe(duration)
		}

		wants, err := os.Open("testdata/image_pull_duration_metric")
		defer func() {
			if err := wants.Close(); err != nil {
				t.Error(err)
			}
		}()

		if err != nil {
			t.Fatal(err)
		}

		if err := testutil.GatherAndCompare(GetGather(), wants, imagePullDurationKey); err != nil {
			t.Error(err)
		}

	})
}

func clearMetrics() {
	ImagePullDuration.Reset()
}
