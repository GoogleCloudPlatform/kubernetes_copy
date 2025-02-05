// Copyright 2014 The Prometheus Authors
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
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"sync"

	"github.com/prometheus/common/model"

	dto "github.com/prometheus/client_model/go"
)

// enhancedWriter has all the enhanced write functions needed here. bufio.Writer
// implements it.
type enhancedWriter interface {
	io.Writer
	WriteRune(r rune) (n int, err error)
	WriteString(s string) (n int, err error)
	WriteByte(c byte) error
}

const (
	initialNumBufSize = 24
)

var (
	bufPool = sync.Pool{
		New: func() interface{} {
			return bufio.NewWriter(io.Discard)
		},
	}
	numBufPool = sync.Pool{
		New: func() interface{} {
			b := make([]byte, 0, initialNumBufSize)
			return &b
		},
	}
)

// MetricFamilyToText converts a MetricFamily proto message into text format and
// writes the resulting lines to 'out'. It returns the number of bytes written
// and any error encountered. The output will have the same order as the input,
// no further sorting is performed. Furthermore, this function assumes the input
// is already sanitized and does not perform any sanity checks. If the input
// contains duplicate metrics or invalid metric or label names, the conversion
// will result in invalid text format output.
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
// This method fulfills the type 'prometheus.encoder'.
func MetricFamilyToText(out io.Writer, in *dto.MetricFamily) (written int, err error) {
	// Fail-fast checks.
	if len(in.Metric) == 0 {
		return 0, fmt.Errorf("MetricFamily has no metrics: %s", in)
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

	var n int

	// Comments, first HELP, then TYPE.
	if in.Help != nil {
		n, err = w.WriteString("# HELP ")
		written += n
		if err != nil {
			return
		}
		n, err = writeName(w, name)
		written += n
		if err != nil {
			return
		}
		err = w.WriteByte(' ')
		written++
		if err != nil {
			return
		}
		n, err = writeEscapedString(w, *in.Help, false)
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
	n, err = writeName(w, name)
	written += n
	if err != nil {
		return
	}
	metricType := in.GetType()
	switch metricType {
	case dto.MetricType_COUNTER:
		n, err = w.WriteString(" counter\n")
	case dto.MetricType_GAUGE:
		n, err = w.WriteString(" gauge\n")
	case dto.MetricType_SUMMARY:
		n, err = w.WriteString(" summary\n")
	case dto.MetricType_UNTYPED:
		n, err = w.WriteString(" untyped\n")
	case dto.MetricType_HISTOGRAM:
		n, err = w.WriteString(" histogram\n")
	default:
		return written, fmt.Errorf("unknown metric type %s", metricType.String())
	}
	written += n
	if err != nil {
		return
	}

	// Finally the samples, one line for each.
	for _, metric := range in.Metric {
		switch metricType {
		case dto.MetricType_COUNTER:
			if metric.Counter == nil {
				return written, fmt.Errorf(
					"expected counter in metric %s %s", name, metric,
				)
			}
			n, err = writeSample(
				w, name, "", metric, "", 0,
				metric.Counter.GetValue(),
			)
		case dto.MetricType_GAUGE:
			if metric.Gauge == nil {
				return written, fmt.Errorf(
					"expected gauge in metric %s %s", name, metric,
				)
			}
			n, err = writeSample(
				w, name, "", metric, "", 0,
				metric.Gauge.GetValue(),
			)
		case dto.MetricType_UNTYPED:
			if metric.Untyped == nil {
				return written, fmt.Errorf(
					"expected untyped in metric %s %s", name, metric,
				)
			}
			n, err = writeSample(
				w, name, "", metric, "", 0,
				metric.Untyped.GetValue(),
			)
		case dto.MetricType_SUMMARY:
			if metric.Summary == nil {
				return written, fmt.Errorf(
					"expected summary in metric %s %s", name, metric,
				)
			}
			for _, q := range metric.Summary.Quantile {
				n, err = writeSample(
					w, name, "", metric,
					model.QuantileLabel, q.GetQuantile(),
					q.GetValue(),
				)
				written += n
				if err != nil {
					return
				}
			}
			n, err = writeSample(
				w, name, "_sum", metric, "", 0,
				metric.Summary.GetSampleSum(),
			)
			written += n
			if err != nil {
				return
			}
			n, err = writeSample(
				w, name, "_count", metric, "", 0,
				float64(metric.Summary.GetSampleCount()),
			)
		case dto.MetricType_HISTOGRAM:
			if metric.Histogram == nil {
				return written, fmt.Errorf(
					"expected histogram in metric %s %s", name, metric,
				)
			}
			infSeen := false
			for _, b := range metric.Histogram.Bucket {
				n, err = writeSample(
					w, name, "_bucket", metric,
					model.BucketLabel, b.GetUpperBound(),
					float64(b.GetCumulativeCount()),
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
				n, err = writeSample(
					w, name, "_bucket", metric,
					model.BucketLabel, math.Inf(+1),
					float64(metric.Histogram.GetSampleCount()),
				)
				written += n
				if err != nil {
					return
				}
			}
			n, err = writeSample(
				w, name, "_sum", metric, "", 0,
				metric.Histogram.GetSampleSum(),
			)
			written += n
			if err != nil {
				return
			}
			n, err = writeSample(
				w, name, "_count", metric, "", 0,
				float64(metric.Histogram.GetSampleCount()),
			)
		default:
			return written, fmt.Errorf(
				"unexpected type in metric %s %s", name, metric,
			)
		}
		written += n
		if err != nil {
			return
		}
	}
	return
}

// writeSample writes a single sample in text format to w, given the metric
// name, the metric proto message itself, optionally an additional label name
// with a float64 value (use empty string as label name if not required), and
// the value. The function returns the number of bytes written and any error
// encountered.
func writeSample(
	w enhancedWriter,
	name, suffix string,
	metric *dto.Metric,
	additionalLabelName string, additionalLabelValue float64,
	value float64,
) (int, error) {
	written := 0
	n, err := writeNameAndLabelPairs(
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
	n, err = writeFloat(w, value)
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
		n, err = writeInt(w, *metric.TimestampMs)
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

// writeNameAndLabelPairs converts a slice of LabelPair proto messages plus the
// explicitly given metric name and additional label pair into text formatted as
// required by the text format and writes it to 'w'. An empty slice in
// combination with an empty string 'additionalLabelName' results in nothing
// being written. Otherwise, the label pairs are written, escaped as required by
// the text format, and enclosed in '{...}'. The function returns the number of
// bytes written and any error encountered. If the metric name is not
// legacy-valid, it will be put inside the brackets as well. Legacy-invalid
// label names will also be quoted.
func writeNameAndLabelPairs(
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
		// metric name inside the braces.
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
		n, err = writeFloat(w, additionalLabelValue)
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

// writeEscapedString replaces '\' by '\\', new line character by '\n', and - if
// includeDoubleQuote is true - '"' by '\"'.
var (
	escaper       = strings.NewReplacer("\\", `\\`, "\n", `\n`)
	quotedEscaper = strings.NewReplacer("\\", `\\`, "\n", `\n`, "\"", `\"`)
)

func writeEscapedString(w enhancedWriter, v string, includeDoubleQuote bool) (int, error) {
	if includeDoubleQuote {
		return quotedEscaper.WriteString(w, v)
	}
	return escaper.WriteString(w, v)
}

// writeFloat is equivalent to fmt.Fprint with a float64 argument but hardcodes
// a few common cases for increased efficiency. For non-hardcoded cases, it uses
// strconv.AppendFloat to avoid allocations, similar to writeInt.
func writeFloat(w enhancedWriter, f float64) (int, error) {
	switch {
	case f == 1:
		return 1, w.WriteByte('1')
	case f == 0:
		return 1, w.WriteByte('0')
	case f == -1:
		return w.WriteString("-1")
	case math.IsNaN(f):
		return w.WriteString("NaN")
	case math.IsInf(f, +1):
		return w.WriteString("+Inf")
	case math.IsInf(f, -1):
		return w.WriteString("-Inf")
	default:
		bp := numBufPool.Get().(*[]byte)
		*bp = strconv.AppendFloat((*bp)[:0], f, 'g', -1, 64)
		written, err := w.Write(*bp)
		numBufPool.Put(bp)
		return written, err
	}
}

// writeInt is equivalent to fmt.Fprint with an int64 argument but uses
// strconv.AppendInt with a byte slice taken from a sync.Pool to avoid
// allocations.
func writeInt(w enhancedWriter, i int64) (int, error) {
	bp := numBufPool.Get().(*[]byte)
	*bp = strconv.AppendInt((*bp)[:0], i, 10)
	written, err := w.Write(*bp)
	numBufPool.Put(bp)
	return written, err
}

// writeName writes a string as-is if it complies with the legacy naming
// scheme, or escapes it in double quotes if not.
func writeName(w enhancedWriter, name string) (int, error) {
	if model.IsValidLegacyMetricName(name) {
		return w.WriteString(name)
	}
	var written int
	var err error
	err = w.WriteByte('"')
	written++
	if err != nil {
		return written, err
	}
	var n int
	n, err = writeEscapedString(w, name, true)
	written += n
	if err != nil {
		return written, err
	}
	err = w.WriteByte('"')
	written++
	return written, err
}
