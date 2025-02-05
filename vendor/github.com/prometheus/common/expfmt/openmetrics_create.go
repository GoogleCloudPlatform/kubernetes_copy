// Copyright 2020 The Prometheus Authors
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

package expfmt

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/prometheus/common/model"

	dto "github.com/prometheus/client_model/go"
)

type encoderOption struct {
	withCreatedLines bool
	withUnit         bool
}

type EncoderOption func(*encoderOption)

// WithCreatedLines is an EncoderOption that configures the OpenMetrics encoder
// to include _created lines (See
// https://github.com/prometheus/OpenMetrics/blob/v1.0.0/specification/OpenMetrics.md#counter-1).
// Created timestamps can improve the accuracy of series reset detection, but
// come with a bandwidth cost.
//
// At the time of writing, created timestamp ingestion is still experimental in
// Prometheus and need to be enabled with the feature-flag
// `--feature-flag=created-timestamp-zero-ingestion`, and breaking changes are
// still possible. Therefore, it is recommended to use this feature with caution.
func WithCreatedLines() EncoderOption {
	return func(t *encoderOption) {
		t.withCreatedLines = true
	}
}

// WithUnit is an EncoderOption enabling a set unit to be written to the output
// and to be added to the metric name, if it's not there already, as a suffix.
// Without opting in this way, the unit will not be added to the metric name and,
// on top of that, the unit will not be passed onto the output, even if it
// were declared in the *dto.MetricFamily struct, i.e. even if in.Unit !=nil.
func WithUnit() EncoderOption {
	return func(t *encoderOption) {
		t.withUnit = true
	}
}

// MetricFamilyToOpenMetrics converts a MetricFamily proto message into the
// OpenMetrics text format and writes the resulting lines to 'out'. It returns
// the number of bytes written and any error encountered. The output will have
// the same order as the input, no further sorting is performed. Furthermore,
// this function assumes the input is already sanitized and does not perform any
// sanity checks. If the input contains duplicate metrics or invalid metric or
// label names, the conversion will result in invalid text format output.
//
// If metric names conform to the legacy validation pattern, they will be placed
// outside the brackets in the traditional way, like `foo{}`. If the metric name
// fails the legacy validation check, it will be placed quoted inside the
// brackets: `{"foo"}`. As stated above, the input is assumed to be santized and
// no error will be thrown in this case.
//
// Similar to metric names, if label names conform to the legacy validation
// pattern, they will be unquoted as normal, like `foo{bar="baz"}`. If the label
// name fails the legacy validation check, it will be quoted:
// `foo{"bar"="baz"}`. As stated above, the input is assumed to be santized and
// no error will be thrown in this case.
//
// This function fulfills the type 'expfmt.encoder'.
//
// Note that OpenMetrics requires a final `# EOF` line. Since this function acts
// on individual metric families, it is the responsibility of the caller to
// append this line to 'out' once all metric families have been written.
// Conveniently, this can be done by calling FinalizeOpenMetrics.
//
// The output should be fully OpenMetrics compliant. However, there are a few
// missing features and peculiarities to avoid complications when switching from
// Prometheus to OpenMetrics or vice versa:
//
//   - Counters are expected to have the `_total` suffix in their metric name. In
//     the output, the suffix will be truncated from the `# TYPE`, `# HELP` and `# UNIT`
//     lines. A counter with a missing `_total` suffix is not an error. However,
//     its type will be set to `unknown` in that case to avoid invalid OpenMetrics
//     output.
//
//   - According to the OM specs, the `# UNIT` line is optional, but if populated,
//     the unit has to be present in the metric name as its suffix:
//     (see https://github.com/prometheus/OpenMetrics/blob/v1.0.0/specification/OpenMetrics.md#unit).
//     However, in order to accommodate any potential scenario where such a change in the
//     metric name is not desirable, the users are here given the choice of either explicitly
//     opt in, in case they wish for the unit to be included in the output AND in the metric name
//     as a suffix (see the description of the WithUnit function above),
//     or not to opt in, in case they don't want for any of that to happen.
//
//   - No support for the following (optional) features: info type,
//     stateset type, gaugehistogram type.
//
//   - The size of exemplar labels is not checked (i.e. it's possible to create
//     exemplars that are larger than allowed by the OpenMetrics specification).
//
//   - The value of Counters is not checked. (OpenMetrics doesn't allow counters
//     with a `NaN` value.)
func MetricFamilyToOpenMetrics(out io.Writer, in *dto.MetricFamily, options ...EncoderOption) (written int, err error) {
	toOM := encoderOption{}
	for _, option := range options {
		option(&toOM)
	}

	name := in.GetName()
	if name == "" {
		return 0, fmt.Errorf("MetricFamily has no name: %s", in)
	}

	// Try the interface upgrade. If it doesn't work, we'll use a
	// bufio.Writer from the sync.Pool.
	w, ok := out.(enhancedWriter)
	if !ok {
		b := bufPool.Get().(*bufio.Writer)
		b.Reset(out)
		w = b
		defer func() {
			bErr := b.Flush()
			if err == nil {
				err = bErr
			}
			bufPool.Put(b)
		}()
	}

	var (
		n             int
		metricType    = in.GetType()
		compliantName = name
	)
	if metricType == dto.MetricType_COUNTER && strings.HasSuffix(compliantName, "_total") {
		compliantName = name[:len(name)-6]
	}
	if toOM.withUnit && in.Unit != nil && !strings.HasSuffix(compliantName, "_"+*in.Unit) {
		compliantName = compliantName + "_" + *in.Unit
	}

	// Comments, first HELP, then TYPE.
	if in.Help != nil {
		n, err = w.WriteString("# HELP ")
		written += n
		if err != nil {
			return
		}
		n, err = writeName(w, compliantName)
		written += n
		if err != nil {
			return
		}
		err = w.WriteByte(' ')
		written++
		if err != nil {
			return
		}
		n, err = writeEscapedString(w, *in.Help, true)
		written += n
		if err != nil {
			return
		}
		err = w.WriteByte('\n')
		written++
		if err != nil {
			return
		}
	}
	n, err = w.WriteString("# TYPE ")
	written += n
	if err != nil {
		return
	}
	n, err = writeName(w, compliantName)
	written += n
	if err != nil {
		return
	}
	switch metricType {
	case dto.MetricType_COUNTER:
		if strings.HasSuffix(name, "_total") {
			n, err = w.WriteString(" counter\n")
		} else {
			n, err = w.WriteString(" unknown\n")
		}
	case dto.MetricType_GAUGE:
		n, err = w.WriteString(" gauge\n")
	case dto.MetricType_SUMMARY:
		n, err = w.WriteString(" summary\n")
	case dto.MetricType_UNTYPED:
		n, err = w.WriteString(" unknown\n")
	case dto.MetricType_HISTOGRAM:
		n, err = w.WriteString(" histogram\n")
	default:
		return written, fmt.Errorf("unknown metric type %s", metricType.String())
	}
	written += n
	if err != nil {
		return
	}
	if toOM.withUnit && in.Unit != nil {
		n, err = w.WriteString("# UNIT ")
		written += n
		if err != nil {
			return
		}
		n, err = writeName(w, compliantName)
		written += n
		if err != nil {
			return
		}

		err = w.WriteByte(' ')
		written++
		if err != nil {
			return
		}
		n, err = writeEscapedString(w, *in.Unit, true)
		written += n
		if err != nil {
			return
		}
		err = w.WriteByte('\n')
		written++
		if err != nil {
			return
		}
	}

	var createdTsBytesWritten int

	// Finally the samples, one line for each.
	if metricType == dto.MetricType_COUNTER && strings.HasSuffix(name, "_total") {
		compliantName = compliantName + "_total"
	}
	for _, metric := range in.Metric {
		switch metricType {
		case dto.MetricType_COUNTER:
			if metric.Counter == nil {
				return written, fmt.Errorf(
					"expected counter in metric %s %s", compliantName, metric,
				)
			}
			n, err = writeOpenMetricsSample(
				w, compliantName, "", metric, "", 0,
				metric.Counter.GetValue(), 0, false,
				metric.Counter.Exemplar,
			)
			if toOM.withCreatedLines && metric.Counter.CreatedTimestamp != nil {
				createdTsBytesWritten, err = writeOpenMetricsCreated(w, compliantName, "_total", metric, "", 0, metric.Counter.GetCreatedTimestamp())
				n += createdTsBytesWritten
			}
		case dto.MetricType_GAUGE:
			if metric.Gauge == nil {
				return written, fmt.Errorf(
					"expected gauge in metric %s %s", compliantName, metric,
				)
			}
			n, err = writeOpenMetricsSample(
				w, compliantName, "", metric, "", 0,
				metric.Gauge.GetValue(), 0, false,
				nil,
			)
		case dto.MetricType_UNTYPED:
			if metric.Untyped == nil {
				return written, fmt.Errorf(
					"expected untyped in metric %s %s", compliantName, metric,
				)
			}
			n, err = writeOpenMetricsSample(
				w, compliantName, "", metric, "", 0,
				metric.Untyped.GetValue(), 0, false,
				nil,
			)
		case dto.MetricType_SUMMARY:
			if metric.Summary == nil {
				return written, fmt.Errorf(
					"expected summary in metric %s %s", compliantName, metric,
				)
			}
			for _, q := range metric.Summary.Quantile {
				n, err = writeOpenMetricsSample(
					w, compliantName, "", metric,
					model.QuantileLabel, q.GetQuantile(),
					q.GetValue(), 0, false,
					nil,
				)
				written += n
				if err != nil {
					return
				}
			}
			n, err = writeOpenMetricsSample(
				w, compliantName, "_sum", metric, "", 0,
				metric.Summary.GetSampleSum(), 0, false,
				nil,
			)
			written += n
			if err != nil {
				return
			}
			n, err = writeOpenMetricsSample(
				w, compliantName, "_count", metric, "", 0,
				0, metric.Summary.GetSampleCount(), true,
				nil,
			)
			if toOM.withCreatedLines && metric.Summary.CreatedTimestamp != nil {
				createdTsBytesWritten, err = writeOpenMetricsCreated(w, compliantName, "", metric, "", 0, metric.Summary.GetCreatedTimestamp())
				n += createdTsBytesWritten
			}
		case dto.MetricType_HISTOGRAM:
			if metric.Histogram == nil {
				return written, fmt.Errorf(
					"expected histogram in metric %s %s", compliantName, metric,
				)
			}
			infSeen := false
			for _, b := range metric.Histogram.Bucket {
				n, err = writeOpenMetricsSample(
					w, compliantName, "_bucket", metric,
					model.BucketLabel, b.GetUpperBound(),
					0, b.GetCumulativeCount(), true,
					b.Exemplar,
				)
				written += n
				if err != nil {
					return
				}
				if math.IsInf(b.GetUpperBound(), +1) {
					infSeen = true
				}
			}
			if !infSeen {
				n, err = writeOpenMetricsSample(
					w, compliantName, "_bucket", metric,
					model.BucketLabel, math.Inf(+1),
					0, metric.Histogram.GetSampleCount(), true,
					nil,
				)
				written += n
				if err != nil {
					return
				}
			}
			n, err = writeOpenMetricsSample(
				w, compliantName, "_sum", metric, "", 0,
				metric.Histogram.GetSampleSum(), 0, false,
				nil,
			)
			written += n
			if err != nil {
				return
			}
			n, err = writeOpenMetricsSample(
				w, compliantName, "_count", metric, "", 0,
				0, metric.Histogram.GetSampleCount(), true,
				nil,
			)
			if toOM.withCreatedLines && metric.Histogram.CreatedTimestamp != nil {
				createdTsBytesWritten, err = writeOpenMetricsCreated(w, compliantName, "", metric, "", 0, metric.Histogram.GetCreatedTimestamp())
				n += createdTsBytesWritten
			}
		default:
			return written, fmt.Errorf(
				"unexpected type in metric %s %s", compliantName, metric,
			)
		}
		written += n
		if err != nil {
			return
		}
	}
	return
}

// FinalizeOpenMetrics writes the final `# EOF\n` line required by OpenMetrics.
func FinalizeOpenMetrics(w io.Writer) (written int, err error) {
	return w.Write([]byte("# EOF\n"))
}

// writeOpenMetricsSample writes a single sample in OpenMetrics text format to
// w, given the metric name, the metric proto message itself, optionally an
// additional label name with a float64 value (use empty string as label name if
// not required), the value (optionally as float64 or uint64, determined by
// useIntValue), and optionally an exemplar (use nil if not required). The
// function returns the number of bytes written and any error encountered.
func writeOpenMetricsSample(
	w enhancedWriter,
	name, suffix string,
	metric *dto.Metric,
	additionalLabelName string, additionalLabelValue float64,
	floatValue float64, intValue uint64, useIntValue bool,
	exemplar *dto.Exemplar,
) (int, error) {
	written := 0
	n, err := writeOpenMetricsNameAndLabelPairs(
		w, name+suffix, metric.Label, additionalLabelName, additionalLabelValue,
	)
	written += n
	if err != nil {
		return written, err
	}
	err = w.WriteByte(' ')
	written++
	if err != nil {
		return written, err
	}
	if useIntValue {
		n, err = writeUint(w, intValue)
	} else {
		n, err = writeOpenMetricsFloat(w, floatValue)
	}
	written += n
	if err != nil {
		return written, err
	}
	if metric.TimestampMs != nil {
		err = w.WriteByte(' ')
		written++
		if err != nil {
			return written, err
		}
		// TODO(beorn7): Format this directly without converting to a float first.
		n, err = writeOpenMetricsFloat(w, float64(*metric.TimestampMs)/1000)
		written += n
		if err != nil {
			return written, err
		}
	}
	if exemplar != nil && len(exemplar.Label) > 0 {
		n, err = writeExemplar(w, exemplar)
		written += n
		if err != nil {
			return written, err
		}
	}
	err = w.WriteByte('\n')
	written++
	if err != nil {
		return written, err
	}
	return written, nil
}

// writeOpenMetricsNameAndLabelPairs works like writeOpenMetricsSample but
// formats the float in OpenMetrics style.
func writeOpenMetricsNameAndLabelPairs(
	w enhancedWriter,
	name string,
	in []*dto.LabelPair,
	additionalLabelName string, additionalLabelValue float64,
) (int, error) {
	var (
		written            int
		separator          byte = '{'
		metricInsideBraces      = false
	)

	if name != "" {
		// If the name does not pass the legacy validity check, we must put the
		// metric name inside the braces, quoted.
		if !model.IsValidLegacyMetricName(name) {
			metricInsideBraces = true
			err := w.WriteByte(separator)
			written++
			if err != nil {
				return written, err
			}
			separator = ','
		}

		n, err := writeName(w, name)
		written += n
		if err != nil {
			return written, err
		}
	}

	if len(in) == 0 && additionalLabelName == "" {
		if metricInsideBraces {
			err := w.WriteByte('}')
			written++
			if err != nil {
				return written, err
			}
		}
		return written, nil
	}

	for _, lp := range in {
		err := w.WriteByte(separator)
		written++
		if err != nil {
			return written, err
		}
		n, err := writeName(w, lp.GetName())
		written += n
		if err != nil {
			return written, err
		}
		n, err = w.WriteString(`="`)
		written += n
		if err != nil {
			return written, err
		}
		n, err = writeEscapedString(w, lp.GetValue(), true)
		written += n
		if err != nil {
			return written, err
		}
		err = w.WriteByte('"')
		written++
		if err != nil {
			return written, err
		}
		separator = ','
	}
	if additionalLabelName != "" {
		err := w.WriteByte(separator)
		written++
		if err != nil {
			return written, err
		}
		n, err := w.WriteString(additionalLabelName)
		written += n
		if err != nil {
			return written, err
		}
		n, err = w.WriteString(`="`)
		written += n
		if err != nil {
			return written, err
		}
		n, err = writeOpenMetricsFloat(w, additionalLabelValue)
		written += n
		if err != nil {
			return written, err
		}
		err = w.WriteByte('"')
		written++
		if err != nil {
			return written, err
		}
	}
	err := w.WriteByte('}')
	written++
	if err != nil {
		return written, err
	}
	return written, nil
}

// writeOpenMetricsCreated writes the created timestamp for a single time series
// following OpenMetrics text format to w, given the metric name, the metric proto
// message itself, optionally a suffix to be removed, e.g. '_total' for counters,
// an additional label name with a float64 value (use empty string as label name if
// not required) and the timestamp that represents the created timestamp.
// The function returns the number of bytes written and any error encountered.
func writeOpenMetricsCreated(w enhancedWriter,
	name, suffixToTrim string, metric *dto.Metric,
	additionalLabelName string, additionalLabelValue float64,
	createdTimestamp *timestamppb.Timestamp,
) (int, error) {
	written := 0
	n, err := writeOpenMetricsNameAndLabelPairs(
		w, strings.TrimSuffix(name, suffixToTrim)+"_created", metric.Label, additionalLabelName, additionalLabelValue,
	)
	written += n
	if err != nil {
		return written, err
	}

	err = w.WriteByte(' ')
	written++
	if err != nil {
		return written, err
	}

	// TODO(beorn7): Format this directly from components of ts to
	// avoid overflow/underflow and precision issues of the float
	// conversion.
	n, err = writeOpenMetricsFloat(w, float64(createdTimestamp.AsTime().UnixNano())/1e9)
	written += n
	if err != nil {
		return written, err
	}

	err = w.WriteByte('\n')
	written++
	if err != nil {
		return written, err
	}
	return written, nil
}

// writeExemplar writes the provided exemplar in OpenMetrics format to w. The
// function returns the number of bytes written and any error encountered.
func writeExemplar(w enhancedWriter, e *dto.Exemplar) (int, error) {
	written := 0
	n, err := w.WriteString(" # ")
	written += n
	if err != nil {
		return written, err
	}
	n, err = writeOpenMetricsNameAndLabelPairs(w, "", e.Label, "", 0)
	written += n
	if err != nil {
		return written, err
	}
	err = w.WriteByte(' ')
	written++
	if err != nil {
		return written, err
	}
	n, err = writeOpenMetricsFloat(w, e.GetValue())
	written += n
	if err != nil {
		return written, err
	}
	if e.Timestamp != nil {
		err = w.WriteByte(' ')
		written++
		if err != nil {
			return written, err
		}
		err = (*e).Timestamp.CheckValid()
		if err != nil {
			return written, err
		}
		ts := (*e).Timestamp.AsTime()
		// TODO(beorn7): Format this directly from components of ts to
		// avoid overflow/underflow and precision issues of the float
		// conversion.
		n, err = writeOpenMetricsFloat(w, float64(ts.UnixNano())/1e9)
		written += n
		if err != nil {
			return written, err
		}
	}
	return written, nil
}

// writeOpenMetricsFloat works like writeFloat but appends ".0" if the resulting
// number would otherwise contain neither a "." nor an "e".
func writeOpenMetricsFloat(w enhancedWriter, f float64) (int, error) {
	switch {
	case f == 1:
		return w.WriteString("1.0")
	case f == 0:
		return w.WriteString("0.0")
	case f == -1:
		return w.WriteString("-1.0")
	case math.IsNaN(f):
		return w.WriteString("NaN")
	case math.IsInf(f, +1):
		return w.WriteString("+Inf")
	case math.IsInf(f, -1):
		return w.WriteString("-Inf")
	default:
		bp := numBufPool.Get().(*[]byte)
		*bp = strconv.AppendFloat((*bp)[:0], f, 'g', -1, 64)
		if !bytes.ContainsAny(*bp, "e.") {
			*bp = append(*bp, '.', '0')
		}
		written, err := w.Write(*bp)
		numBufPool.Put(bp)
		return written, err
	}
}

// writeUint is like writeInt just for uint64.
func writeUint(w enhancedWriter, u uint64) (int, error) {
	bp := numBufPool.Get().(*[]byte)
	*bp = strconv.AppendUint((*bp)[:0], u, 10)
	written, err := w.Write(*bp)
	numBufPool.Put(bp)
	return written, err
}
