// Copyright 2020 The Prometheus Authors
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
	"errors"
	"fmt"
	"regexp"
	"strings"

	dto "github.com/prometheus/client_model/go"
)

var camelCase = regexp.MustCompile(`[a-z][A-Z]`)

// LintMetricUnits detects issues with metric unit names.
func LintMetricUnits(mf *dto.MetricFamily) []error {
	var problems []error

	unit, base, ok := metricUnits(*mf.Name)
	if !ok {
		// No known units detected.
		return nil
	}

	// Unit is already a base unit.
	if unit == base {
		return nil
	}

	problems = append(problems, fmt.Errorf("use base unit %q instead of %q", base, unit))

	return problems
}

// LintMetricTypeInName detects when the metric type is included in the metric name.
func LintMetricTypeInName(mf *dto.MetricFamily) []error {
	if mf.GetType() == dto.MetricType_UNTYPED {
		return nil
	}

	var problems []error

	n := strings.ToLower(mf.GetName())
	typename := strings.ToLower(mf.GetType().String())

	if strings.Contains(n, "_"+typename+"_") || strings.HasSuffix(n, "_"+typename) {
		problems = append(problems, fmt.Errorf(`metric name should not include type '%s'`, typename))
	}

	return problems
}

// LintReservedChars detects colons in metric names.
func LintReservedChars(mf *dto.MetricFamily) []error {
	var problems []error
	if strings.Contains(mf.GetName(), ":") {
		problems = append(problems, errors.New("metric names should not contain ':'"))
	}
	return problems
}

// LintCamelCase detects metric names and label names written in camelCase.
func LintCamelCase(mf *dto.MetricFamily) []error {
	var problems []error
	if camelCase.FindString(mf.GetName()) != "" {
		problems = append(problems, errors.New("metric names should be written in 'snake_case' not 'camelCase'"))
	}

	for _, m := range mf.GetMetric() {
		for _, l := range m.GetLabel() {
			if camelCase.FindString(l.GetName()) != "" {
				problems = append(problems, errors.New("label names should be written in 'snake_case' not 'camelCase'"))
			}
		}
	}
	return problems
}

// LintUnitAbbreviations detects abbreviated units in the metric name.
func LintUnitAbbreviations(mf *dto.MetricFamily) []error {
	var problems []error
	n := strings.ToLower(mf.GetName())
	for _, s := range unitAbbreviations {
		if strings.Contains(n, "_"+s+"_") || strings.HasSuffix(n, "_"+s) {
			problems = append(problems, errors.New("metric names should not contain abbreviated units"))
		}
	}
	return problems
}
