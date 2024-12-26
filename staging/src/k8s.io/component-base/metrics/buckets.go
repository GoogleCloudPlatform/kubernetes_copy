/*
Copyright 2022 The Kubernetes Authors.

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
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// DefBuckets is a wrapper for prometheus.DefBuckets
var DefBuckets = prometheus.DefBuckets

// DefNativeHistogramBucketFactor defines a sane default for the Prometheus
// native histogram exponential bucket factor. The value of 1.1 means each
// bucket is 10% wider than the previous bucket.
//
// For more information on Prometheus Native Histograms, see the upstream
// client_golang documentation.
//
// https://pkg.go.dev/github.com/prometheus/client_golang/prometheus#HistogramOpts
var DefNativeHistogramBucketFactor = 1.1

// DefNativeHistogramMaxBucketNumber defines a sane default for the Prometheus
// native histogram limit to the number of sparse buckets. This avoids
// unbounded memory requirements for native histogram tracking.
var DefNativeHistogramMaxBucketNumber = 200

// DefNativeHistogramMinResetDuration defines a sane default for the Prometheus
// native histogram minimum reset duration. This is used to reset the histogram
// buckets in case the max bucket number value is reached.
var DefNativeHistogramMinResetDuration = time.Hour

// LinearBuckets is a wrapper for prometheus.LinearBuckets.
func LinearBuckets(start, width float64, count int) []float64 {
	return prometheus.LinearBuckets(start, width, count)
}

// ExponentialBuckets is a wrapper for prometheus.ExponentialBuckets.
func ExponentialBuckets(start, factor float64, count int) []float64 {
	return prometheus.ExponentialBuckets(start, factor, count)
}

// ExponentialBucketsRange creates 'count' buckets, where the lowest bucket is
// 'min' and the highest bucket is 'max'. The final +Inf bucket is not counted
// and not included in the returned slice. The returned slice is meant to be
// used for the Buckets field of HistogramOpts.
//
// The function panics if 'count' is 0 or negative, if 'min' is 0 or negative.
func ExponentialBucketsRange(min, max float64, count int) []float64 {
	return prometheus.ExponentialBucketsRange(min, max, count)
}

// MergeBuckets merges buckets together
func MergeBuckets(buckets ...[]float64) []float64 {
	result := make([]float64, 1)
	for _, s := range buckets {
		result = append(result, s...)
	}
	return result
}
