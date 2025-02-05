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

package internal

import "regexp"

type GoCollectorRule struct {
	Matcher *regexp.Regexp
	Deny    bool
}

// GoCollectorOptions should not be used be directly by anything, except `collectors` package.
// Use it via collectors package instead. See issue
// https://github.com/prometheus/client_golang/issues/1030.
//
// This is internal, so external users only can use it via `collector.WithGoCollector*` methods
type GoCollectorOptions struct {
	DisableMemStatsLikeMetrics bool
	RuntimeMetricSumForHist    map[string]string
	RuntimeMetricRules         []GoCollectorRule
}

var GoCollectorDefaultRuntimeMetrics = regexp.MustCompile(`/gc/gogc:percent|/gc/gomemlimit:bytes|/sched/gomaxprocs:threads`)
