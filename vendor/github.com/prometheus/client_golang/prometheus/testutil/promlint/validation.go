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

package promlint

import (
	dto "github.com/prometheus/client_model/go"

	"github.com/prometheus/client_golang/prometheus/testutil/promlint/validations"
)

type Validation = func(mf *dto.MetricFamily) []error

var defaultValidations = []Validation{
	validations.LintHelp,
	validations.LintMetricUnits,
	validations.LintCounter,
	validations.LintHistogramSummaryReserved,
	validations.LintMetricTypeInName,
	validations.LintReservedChars,
	validations.LintCamelCase,
	validations.LintUnitAbbreviations,
	validations.LintDuplicateMetric,
}
