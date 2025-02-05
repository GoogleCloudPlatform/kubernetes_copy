// Copyright 2021 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build go1.17
// +build go1.17

package internal

import (
	"math"
	"path"
	"runtime/metrics"
	"strings"

	"github.com/prometheus/common/model"
)

// RuntimeMetricsToProm produces a Prometheus metric name from a runtime/metrics
// metric description and validates whether the metric is suitable for integration
// with Prometheus.
//
// Returns false if a name could not be produced, or if Prometheus does not understand
// the runtime/metrics Kind.
//
// Note that the main reason a name couldn't be produced is if the runtime/metrics
// package exports a name with characters outside the valid Prometheus metric name
// character set. This is theoretically possible, but should never happen in practice.
// Still, don't rely on it.
func RuntimeMetricsToProm(d *metrics.Description) (string, string, string, bool) {
	namespace := "go"

	comp := strings.SplitN(d.Name, ":", 2)
	key := comp[0]
	unit := comp[1]

	// The last path element in the key is the name,
	// the rest is the subsystem.
	subsystem := path.Dir(key[1:] /* remove leading / */)
	name := path.Base(key)

	// subsystem is translated by replacing all / and - with _.
	subsystem = strings.ReplaceAll(subsystem, "/", "_")
	subsystem = strings.ReplaceAll(subsystem, "-", "_")

	// unit is translated assuming that the unit contains no
	// non-ASCII characters.
	unit = strings.ReplaceAll(unit, "-", "_")
	unit = strings.ReplaceAll(unit, "*", "_")
	unit = strings.ReplaceAll(unit, "/", "_per_")

	// name has - replaced with _ and is concatenated with the unit and
	// other data.
	name = strings.ReplaceAll(name, "-", "_")
	name += "_" + unit
	if d.Cumulative && d.Kind != metrics.KindFloat64Histogram {
		name += "_total"
	}

	// Our current conversion moves to legacy naming, so use legacy validation.
	valid := model.IsValidLegacyMetricName(namespace + "_" + subsystem + "_" + name)
	switch d.Kind {
	case metrics.KindUint64:
	case metrics.KindFloat64:
	case metrics.KindFloat64Histogram:
	default:
		valid = false
	}
	return namespace, subsystem, name, valid
}

// RuntimeMetricsBucketsForUnit takes a set of buckets obtained for a runtime/metrics histogram
// type (so, lower-bound inclusive) and a unit from a runtime/metrics name, and produces
// a reduced set of buckets. This function always removes any -Inf bucket as it's represented
// as the bottom-most upper-bound inclusive bucket in Prometheus.
func RuntimeMetricsBucketsForUnit(buckets []float64, unit string) []float64 {
	switch unit {
	case "bytes":
		// Re-bucket as powers of 2.
		return reBucketExp(buckets, 2)
	case "seconds":
		// Re-bucket as powers of 10 and then merge all buckets greater
		// than 1 second into the +Inf bucket.
		b := reBucketExp(buckets, 10)
		for i := range b {
			if b[i] <= 1 {
				continue
			}
			b[i] = math.Inf(1)
			b = b[:i+1]
			break
		}
		return b
	}
	return buckets
}

// reBucketExp takes a list of bucket boundaries (lower bound inclusive) and
// downsamples the buckets to those a multiple of base apart. The end result
// is a roughly exponential (in many cases, perfectly exponential) bucketing
// scheme.
func reBucketExp(buckets []float64, base float64) []float64 {
	bucket := buckets[0]
	var newBuckets []float64
	// We may see a -Inf here, in which case, add it and skip it
	// since we risk producing NaNs otherwise.
	//
	// We need to preserve -Inf values to maintain runtime/metrics
	// conventions. We'll strip it out later.
	if bucket == math.Inf(-1) {
		newBuckets = append(newBuckets, bucket)
		buckets = buckets[1:]
		bucket = buckets[0]
	}
	// From now on, bucket should always have a non-Inf value because
	// Infs are only ever at the ends of the bucket lists, so
	// arithmetic operations on it are non-NaN.
	for i := 1; i < len(buckets); i++ {
		if bucket >= 0 && buckets[i] < bucket*base {
			// The next bucket we want to include is at least bucket*base.
			continue
		} else if bucket < 0 && buckets[i] < bucket/base {
			// In this case the bucket we're targeting is negative, and since
			// we're ascending through buckets here, we need to divide to get
			// closer to zero exponentially.
			continue
		}
		// The +Inf bucket will always be the last one, and we'll always
		// end up including it here because bucket
		newBuckets = append(newBuckets, bucket)
		bucket = buckets[i]
	}
	return append(newBuckets, bucket)
}
