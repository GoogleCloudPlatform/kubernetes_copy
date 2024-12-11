// Copyright 2024 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package validations

import (
	"fmt"
	"reflect"

	dto "github.com/prometheus/client_model/go"
)

// LintDuplicateMetric detects duplicate metric.
func LintDuplicateMetric(mf *dto.MetricFamily) []error {
	var problems []error

	for i, m := range mf.Metric {
		for _, k := range mf.Metric[i+1:] {
			if reflect.DeepEqual(m.Label, k.Label) {
				problems = append(problems, fmt.Errorf("metric not unique"))
				break
			}
		}
	}

	return problems
}
