// Copyright 2013 The Prometheus Authors
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

package model

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	// AlertNameLabel is the name of the label containing the an alert's name.
	AlertNameLabel = "alertname"

	// ExportedLabelPrefix is the prefix to prepend to the label names present in
	// exported metrics if a label of the same name is added by the server.
	ExportedLabelPrefix = "exported_"

	// MetricNameLabel is the label name indicating the metric name of a
	// timeseries.
	MetricNameLabel = "__name__"

	// SchemeLabel is the name of the label that holds the scheme on which to
	// scrape a target.
	SchemeLabel = "__scheme__"

	// AddressLabel is the name of the label that holds the address of
	// a scrape target.
	AddressLabel = "__address__"

	// MetricsPathLabel is the name of the label that holds the path on which to
	// scrape a target.
	MetricsPathLabel = "__metrics_path__"

	// ScrapeIntervalLabel is the name of the label that holds the scrape interval
	// used to scrape a target.
	ScrapeIntervalLabel = "__scrape_interval__"

	// ScrapeTimeoutLabel is the name of the label that holds the scrape
	// timeout used to scrape a target.
	ScrapeTimeoutLabel = "__scrape_timeout__"

	// ReservedLabelPrefix is a prefix which is not legal in user-supplied
	// label names.
	ReservedLabelPrefix = "__"

	// MetaLabelPrefix is a prefix for labels that provide meta information.
	// Labels with this prefix are used for intermediate label processing and
	// will not be attached to time series.
	MetaLabelPrefix = "__meta_"

	// TmpLabelPrefix is a prefix for temporary labels as part of relabelling.
	// Labels with this prefix are used for intermediate label processing and
	// will not be attached to time series. This is reserved for use in
	// Prometheus configuration files by users.
	TmpLabelPrefix = "__tmp_"

	// ParamLabelPrefix is a prefix for labels that provide URL parameters
	// used to scrape a target.
	ParamLabelPrefix = "__param_"

	// JobLabel is the label name indicating the job from which a timeseries
	// was scraped.
	JobLabel = "job"

	// InstanceLabel is the label name used for the instance label.
	InstanceLabel = "instance"

	// BucketLabel is used for the label that defines the upper bound of a
	// bucket of a histogram ("le" -> "less or equal").
	BucketLabel = "le"

	// QuantileLabel is used for the label that defines the quantile in a
	// summary.
	QuantileLabel = "quantile"
)

// LabelNameRE is a regular expression matching valid label names. Note that the
// IsValid method of LabelName performs the same check but faster than a match
// with this regular expression.
var LabelNameRE = regexp.MustCompile("^[a-zA-Z_][a-zA-Z0-9_]*$")

// A LabelName is a key for a LabelSet or Metric.  It has a value associated
// therewith.
type LabelName string

// IsValid returns true iff the name matches the pattern of LabelNameRE when
// NameValidationScheme is set to LegacyValidation, or valid UTF-8 if
// NameValidationScheme is set to UTF8Validation.
func (ln LabelName) IsValid() bool {
	if len(ln) == 0 {
		return false
	}
	switch NameValidationScheme {
	case LegacyValidation:
		return ln.IsValidLegacy()
	case UTF8Validation:
		return utf8.ValidString(string(ln))
	default:
		panic(fmt.Sprintf("Invalid name validation scheme requested: %d", NameValidationScheme))
	}
}

// IsValidLegacy returns true iff name matches the pattern of LabelNameRE for
// legacy names. It does not use LabelNameRE for the check but a much faster
// hardcoded implementation.
func (ln LabelName) IsValidLegacy() bool {
	if len(ln) == 0 {
		return false
	}
	for i, b := range ln {
		if !((b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || b == '_' || (b >= '0' && b <= '9' && i > 0)) {
			return false
		}
	}
	return true
}

// UnmarshalYAML implements the yaml.Unmarshaler interface.
func (ln *LabelName) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var s string
	if err := unmarshal(&s); err != nil {
		return err
	}
	if !LabelName(s).IsValid() {
		return fmt.Errorf("%q is not a valid label name", s)
	}
	*ln = LabelName(s)
	return nil
}

// UnmarshalJSON implements the json.Unmarshaler interface.
func (ln *LabelName) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	if !LabelName(s).IsValid() {
		return fmt.Errorf("%q is not a valid label name", s)
	}
	*ln = LabelName(s)
	return nil
}

// LabelNames is a sortable LabelName slice. In implements sort.Interface.
type LabelNames []LabelName

func (l LabelNames) Len() int {
	return len(l)
}

func (l LabelNames) Less(i, j int) bool {
	return l[i] < l[j]
}

func (l LabelNames) Swap(i, j int) {
	l[i], l[j] = l[j], l[i]
}

func (l LabelNames) String() string {
	labelStrings := make([]string, 0, len(l))
	for _, label := range l {
		labelStrings = append(labelStrings, string(label))
	}
	return strings.Join(labelStrings, ", ")
}

// A LabelValue is an associated value for a LabelName.
type LabelValue string

// IsValid returns true iff the string is a valid UTF-8.
func (lv LabelValue) IsValid() bool {
	return utf8.ValidString(string(lv))
}

// LabelValues is a sortable LabelValue slice. It implements sort.Interface.
type LabelValues []LabelValue

func (l LabelValues) Len() int {
	return len(l)
}

func (l LabelValues) Less(i, j int) bool {
	return string(l[i]) < string(l[j])
}

func (l LabelValues) Swap(i, j int) {
	l[i], l[j] = l[j], l[i]
}

// LabelPair pairs a name with a value.
type LabelPair struct {
	Name  LabelName
	Value LabelValue
}

// LabelPairs is a sortable slice of LabelPair pointers. It implements
// sort.Interface.
type LabelPairs []*LabelPair

func (l LabelPairs) Len() int {
	return len(l)
}

func (l LabelPairs) Less(i, j int) bool {
	switch {
	case l[i].Name > l[j].Name:
		return false
	case l[i].Name < l[j].Name:
		return true
	case l[i].Value > l[j].Value:
		return false
	case l[i].Value < l[j].Value:
		return true
	default:
		return false
	}
}

func (l LabelPairs) Swap(i, j int) {
	l[i], l[j] = l[j], l[i]
}
