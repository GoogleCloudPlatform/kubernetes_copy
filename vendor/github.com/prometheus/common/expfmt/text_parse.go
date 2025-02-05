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
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"

	dto "github.com/prometheus/client_model/go"
	"google.golang.org/protobuf/proto"

	"github.com/prometheus/common/model"
)

// A stateFn is a function that represents a state in a state machine. By
// executing it, the state is progressed to the next state. The stateFn returns
// another stateFn, which represents the new state. The end state is represented
// by nil.
type stateFn func() stateFn

// ParseError signals errors while parsing the simple and flat text-based
// exchange format.
type ParseError struct {
	Line int
	Msg  string
}

// Error implements the error interface.
func (e ParseError) Error() string {
	return fmt.Sprintf("text format parsing error in line %d: %s", e.Line, e.Msg)
}

// TextParser is used to parse the simple and flat text-based exchange format. Its
// zero value is ready to use.
type TextParser struct {
	metricFamiliesByName map[string]*dto.MetricFamily
	buf                  *bufio.Reader // Where the parsed input is read through.
	err                  error         // Most recent error.
	lineCount            int           // Tracks the line count for error messages.
	currentByte          byte          // The most recent byte read.
	currentToken         bytes.Buffer  // Re-used each time a token has to be gathered from multiple bytes.
	currentMF            *dto.MetricFamily
	currentMetric        *dto.Metric
	currentLabelPair     *dto.LabelPair
	currentLabelPairs    []*dto.LabelPair // Temporarily stores label pairs while parsing a metric line.

	// The remaining member variables are only used for summaries/histograms.
	currentLabels map[string]string // All labels including '__name__' but excluding 'quantile'/'le'
	// Summary specific.
	summaries       map[uint64]*dto.Metric // Key is created with LabelsToSignature.
	currentQuantile float64
	// Histogram specific.
	histograms    map[uint64]*dto.Metric // Key is created with LabelsToSignature.
	currentBucket float64
	// These tell us if the currently processed line ends on '_count' or
	// '_sum' respectively and belong to a summary/histogram, representing the sample
	// count and sum of that summary/histogram.
	currentIsSummaryCount, currentIsSummarySum     bool
	currentIsHistogramCount, currentIsHistogramSum bool
	// These indicate if the metric name from the current line being parsed is inside
	// braces and if that metric name was found respectively.
	currentMetricIsInsideBraces, currentMetricInsideBracesIsPresent bool
}

// TextToMetricFamilies reads 'in' as the simple and flat text-based exchange
// format and creates MetricFamily proto messages. It returns the MetricFamily
// proto messages in a map where the metric names are the keys, along with any
// error encountered.
//
// If the input contains duplicate metrics (i.e. lines with the same metric name
// and exactly the same label set), the resulting MetricFamily will contain
// duplicate Metric proto messages. Similar is true for duplicate label
// names. Checks for duplicates have to be performed separately, if required.
// Also note that neither the metrics within each MetricFamily are sorted nor
// the label pairs within each Metric. Sorting is not required for the most
// frequent use of this method, which is sample ingestion in the Prometheus
// server. However, for presentation purposes, you might want to sort the
// metrics, and in some cases, you must sort the labels, e.g. for consumption by
// the metric family injection hook of the Prometheus registry.
//
// Summaries and histograms are rather special beasts. You would probably not
// use them in the simple text format anyway. This method can deal with
// summaries and histograms if they are presented in exactly the way the
// text.Create function creates them.
//
// This method must not be called concurrently. If you want to parse different
// input concurrently, instantiate a separate Parser for each goroutine.
func (p *TextParser) TextToMetricFamilies(in io.Reader) (map[string]*dto.MetricFamily, error) {
	p.reset(in)
	for nextState := p.startOfLine; nextState != nil; nextState = nextState() {
		// Magic happens here...
	}
	// Get rid of empty metric families.
	for k, mf := range p.metricFamiliesByName {
		if len(mf.GetMetric()) == 0 {
			delete(p.metricFamiliesByName, k)
		}
	}
	// If p.err is io.EOF now, we have run into a premature end of the input
	// stream. Turn this error into something nicer and more
	// meaningful. (io.EOF is often used as a signal for the legitimate end
	// of an input stream.)
	if p.err != nil && errors.Is(p.err, io.EOF) {
		p.parseError("unexpected end of input stream")
	}
	return p.metricFamiliesByName, p.err
}

func (p *TextParser) reset(in io.Reader) {
	p.metricFamiliesByName = map[string]*dto.MetricFamily{}
	if p.buf == nil {
		p.buf = bufio.NewReader(in)
	} else {
		p.buf.Reset(in)
	}
	p.err = nil
	p.lineCount = 0
	if p.summaries == nil || len(p.summaries) > 0 {
		p.summaries = map[uint64]*dto.Metric{}
	}
	if p.histograms == nil || len(p.histograms) > 0 {
		p.histograms = map[uint64]*dto.Metric{}
	}
	p.currentQuantile = math.NaN()
	p.currentBucket = math.NaN()
	p.currentMF = nil
}

// startOfLine represents the state where the next byte read from p.buf is the
// start of a line (or whitespace leading up to it).
func (p *TextParser) startOfLine() stateFn {
	p.lineCount++
	p.currentMetricIsInsideBraces = false
	p.currentMetricInsideBracesIsPresent = false
	if p.skipBlankTab(); p.err != nil {
		// This is the only place that we expect to see io.EOF,
		// which is not an error but the signal that we are done.
		// Any other error that happens to align with the start of
		// a line is still an error.
		if errors.Is(p.err, io.EOF) {
			p.err = nil
		}
		return nil
	}
	switch p.currentByte {
	case '#':
		return p.startComment
	case '\n':
		return p.startOfLine // Empty line, start the next one.
	case '{':
		p.currentMetricIsInsideBraces = true
		return p.readingLabels
	}
	return p.readingMetricName
}

// startComment represents the state where the next byte read from p.buf is the
// start of a comment (or whitespace leading up to it).
func (p *TextParser) startComment() stateFn {
	if p.skipBlankTab(); p.err != nil {
		return nil // Unexpected end of input.
	}
	if p.currentByte == '\n' {
		return p.startOfLine
	}
	if p.readTokenUntilWhitespace(); p.err != nil {
		return nil // Unexpected end of input.
	}
	// If we have hit the end of line already, there is nothing left
	// to do. This is not considered a syntax error.
	if p.currentByte == '\n' {
		return p.startOfLine
	}
	keyword := p.currentToken.String()
	if keyword != "HELP" && keyword != "TYPE" {
		// Generic comment, ignore by fast forwarding to end of line.
		for p.currentByte != '\n' {
			if p.currentByte, p.err = p.buf.ReadByte(); p.err != nil {
				return nil // Unexpected end of input.
			}
		}
		return p.startOfLine
	}
	// There is something. Next has to be a metric name.
	if p.skipBlankTab(); p.err != nil {
		return nil // Unexpected end of input.
	}
	if p.readTokenAsMetricName(); p.err != nil {
		return nil // Unexpected end of input.
	}
	if p.currentByte == '\n' {
		// At the end of the line already.
		// Again, this is not considered a syntax error.
		return p.startOfLine
	}
	if !isBlankOrTab(p.currentByte) {
		p.parseError("invalid metric name in comment")
		return nil
	}
	p.setOrCreateCurrentMF()
	if p.skipBlankTab(); p.err != nil {
		return nil // Unexpected end of input.
	}
	if p.currentByte == '\n' {
		// At the end of the line already.
		// Again, this is not considered a syntax error.
		return p.startOfLine
	}
	switch keyword {
	case "HELP":
		return p.readingHelp
	case "TYPE":
		return p.readingType
	}
	panic(fmt.Sprintf("code error: unexpected keyword %q", keyword))
}

// readingMetricName represents the state where the last byte read (now in
// p.currentByte) is the first byte of a metric name.
func (p *TextParser) readingMetricName() stateFn {
	if p.readTokenAsMetricName(); p.err != nil {
		return nil
	}
	if p.currentToken.Len() == 0 {
		p.parseError("invalid metric name")
		return nil
	}
	p.setOrCreateCurrentMF()
	// Now is the time to fix the type if it hasn't happened yet.
	if p.currentMF.Type == nil {
		p.currentMF.Type = dto.MetricType_UNTYPED.Enum()
	}
	p.currentMetric = &dto.Metric{}
	// Do not append the newly created currentMetric to
	// currentMF.Metric right now. First wait if this is a summary,
	// and the metric exists already, which we can only know after
	// having read all the labels.
	if p.skipBlankTabIfCurrentBlankTab(); p.err != nil {
		return nil // Unexpected end of input.
	}
	return p.readingLabels
}

// readingLabels represents the state where the last byte read (now in
// p.currentByte) is either the first byte of the label set (i.e. a '{'), or the
// first byte of the value (otherwise).
func (p *TextParser) readingLabels() stateFn {
	// Summaries/histograms are special. We have to reset the
	// currentLabels map, currentQuantile and currentBucket before starting to
	// read labels.
	if p.currentMF.GetType() == dto.MetricType_SUMMARY || p.currentMF.GetType() == dto.MetricType_HISTOGRAM {
		p.currentLabels = map[string]string{}
		p.currentLabels[string(model.MetricNameLabel)] = p.currentMF.GetName()
		p.currentQuantile = math.NaN()
		p.currentBucket = math.NaN()
	}
	if p.currentByte != '{' {
		return p.readingValue
	}
	return p.startLabelName
}

// startLabelName represents the state where the next byte read from p.buf is
// the start of a label name (or whitespace leading up to it).
func (p *TextParser) startLabelName() stateFn {
	if p.skipBlankTab(); p.err != nil {
		return nil // Unexpected end of input.
	}
	if p.currentByte == '}' {
		p.currentMetric.Label = append(p.currentMetric.Label, p.currentLabelPairs...)
		p.currentLabelPairs = nil
		if p.skipBlankTab(); p.err != nil {
			return nil // Unexpected end of input.
		}
		return p.readingValue
	}
	if p.readTokenAsLabelName(); p.err != nil {
		return nil // Unexpected end of input.
	}
	if p.currentToken.Len() == 0 {
		p.parseError(fmt.Sprintf("invalid label name for metric %q", p.currentMF.GetName()))
		return nil
	}
	if p.skipBlankTabIfCurrentBlankTab(); p.err != nil {
		return nil // Unexpected end of input.
	}
	if p.currentByte != '=' {
		if p.currentMetricIsInsideBraces {
			if p.currentMetricInsideBracesIsPresent {
				p.parseError(fmt.Sprintf("multiple metric names for metric %q", p.currentMF.GetName()))
				return nil
			}
			switch p.currentByte {
			case ',':
				p.setOrCreateCurrentMF()
				if p.currentMF.Type == nil {
					p.currentMF.Type = dto.MetricType_UNTYPED.Enum()
				}
				p.currentMetric = &dto.Metric{}
				p.currentMetricInsideBracesIsPresent = true
				return p.startLabelName
			case '}':
				p.setOrCreateCurrentMF()
				if p.currentMF.Type == nil {
					p.currentMF.Type = dto.MetricType_UNTYPED.Enum()
				}
				p.currentMetric = &dto.Metric{}
				p.currentMetric.Label = append(p.currentMetric.Label, p.currentLabelPairs...)
				p.currentLabelPairs = nil
				if p.skipBlankTab(); p.err != nil {
					return nil // Unexpected end of input.
				}
				return p.readingValue
			default:
				p.parseError(fmt.Sprintf("unexpected end of metric name %q", p.currentByte))
				return nil
			}
		}
		p.parseError(fmt.Sprintf("expected '=' after label name, found %q", p.currentByte))
		p.currentLabelPairs = nil
		return nil
	}
	p.currentLabelPair = &dto.LabelPair{Name: proto.String(p.currentToken.String())}
	if p.currentLabelPair.GetName() == string(model.MetricNameLabel) {
		p.parseError(fmt.Sprintf("label name %q is reserved", model.MetricNameLabel))
		return nil
	}
	// Special summary/histogram treatment. Don't add 'quantile' and 'le'
	// labels to 'real' labels.
	if !(p.currentMF.GetType() == dto.MetricType_SUMMARY && p.currentLabelPair.GetName() == model.QuantileLabel) &&
		!(p.currentMF.GetType() == dto.MetricType_HISTOGRAM && p.currentLabelPair.GetName() == model.BucketLabel) {
		p.currentLabelPairs = append(p.currentLabelPairs, p.currentLabelPair)
	}
	// Check for duplicate label names.
	labels := make(map[string]struct{})
	for _, l := range p.currentLabelPairs {
		lName := l.GetName()
		if _, exists := labels[lName]; !exists {
			labels[lName] = struct{}{}
		} else {
			p.parseError(fmt.Sprintf("duplicate label names for metric %q", p.currentMF.GetName()))
			p.currentLabelPairs = nil
			return nil
		}
	}
	return p.startLabelValue
}

// startLabelValue represents the state where the next byte read from p.buf is
// the start of a (quoted) label value (or whitespace leading up to it).
func (p *TextParser) startLabelValue() stateFn {
	if p.skipBlankTab(); p.err != nil {
		return nil // Unexpected end of input.
	}
	if p.currentByte != '"' {
		p.parseError(fmt.Sprintf("expected '\"' at start of label value, found %q", p.currentByte))
		return nil
	}
	if p.readTokenAsLabelValue(); p.err != nil {
		return nil
	}
	if !model.LabelValue(p.currentToken.String()).IsValid() {
		p.parseError(fmt.Sprintf("invalid label value %q", p.currentToken.String()))
		return nil
	}
	p.currentLabelPair.Value = proto.String(p.currentToken.String())
	// Special treatment of summaries:
	// - Quantile labels are special, will result in dto.Quantile later.
	// - Other labels have to be added to currentLabels for signature calculation.
	if p.currentMF.GetType() == dto.MetricType_SUMMARY {
		if p.currentLabelPair.GetName() == model.QuantileLabel {
			if p.currentQuantile, p.err = parseFloat(p.currentLabelPair.GetValue()); p.err != nil {
				// Create a more helpful error message.
				p.parseError(fmt.Sprintf("expected float as value for 'quantile' label, got %q", p.currentLabelPair.GetValue()))
				p.currentLabelPairs = nil
				return nil
			}
		} else {
			p.currentLabels[p.currentLabelPair.GetName()] = p.currentLabelPair.GetValue()
		}
	}
	// Similar special treatment of histograms.
	if p.currentMF.GetType() == dto.MetricType_HISTOGRAM {
		if p.currentLabelPair.GetName() == model.BucketLabel {
			if p.currentBucket, p.err = parseFloat(p.currentLabelPair.GetValue()); p.err != nil {
				// Create a more helpful error message.
				p.parseError(fmt.Sprintf("expected float as value for 'le' label, got %q", p.currentLabelPair.GetValue()))
				return nil
			}
		} else {
			p.currentLabels[p.currentLabelPair.GetName()] = p.currentLabelPair.GetValue()
		}
	}
	if p.skipBlankTab(); p.err != nil {
		return nil // Unexpected end of input.
	}
	switch p.currentByte {
	case ',':
		return p.startLabelName

	case '}':
		if p.currentMF == nil {
			p.parseError("invalid metric name")
			return nil
		}
		p.currentMetric.Label = append(p.currentMetric.Label, p.currentLabelPairs...)
		p.currentLabelPairs = nil
		if p.skipBlankTab(); p.err != nil {
			return nil // Unexpected end of input.
		}
		return p.readingValue
	default:
		p.parseError(fmt.Sprintf("unexpected end of label value %q", p.currentLabelPair.GetValue()))
		p.currentLabelPairs = nil
		return nil
	}
}

// readingValue represents the state where the last byte read (now in
// p.currentByte) is the first byte of the sample value (i.e. a float).
func (p *TextParser) readingValue() stateFn {
	// When we are here, we have read all the labels, so for the
	// special case of a summary/histogram, we can finally find out
	// if the metric already exists.
	if p.currentMF.GetType() == dto.MetricType_SUMMARY {
		signature := model.LabelsToSignature(p.currentLabels)
		if summary := p.summaries[signature]; summary != nil {
			p.currentMetric = summary
		} else {
			p.summaries[signature] = p.currentMetric
			p.currentMF.Metric = append(p.currentMF.Metric, p.currentMetric)
		}
	} else if p.currentMF.GetType() == dto.MetricType_HISTOGRAM {
		signature := model.LabelsToSignature(p.currentLabels)
		if histogram := p.histograms[signature]; histogram != nil {
			p.currentMetric = histogram
		} else {
			p.histograms[signature] = p.currentMetric
			p.currentMF.Metric = append(p.currentMF.Metric, p.currentMetric)
		}
	} else {
		p.currentMF.Metric = append(p.currentMF.Metric, p.currentMetric)
	}
	if p.readTokenUntilWhitespace(); p.err != nil {
		return nil // Unexpected end of input.
	}
	value, err := parseFloat(p.currentToken.String())
	if err != nil {
		// Create a more helpful error message.
		p.parseError(fmt.Sprintf("expected float as value, got %q", p.currentToken.String()))
		return nil
	}
	switch p.currentMF.GetType() {
	case dto.MetricType_COUNTER:
		p.currentMetric.Counter = &dto.Counter{Value: proto.Float64(value)}
	case dto.MetricType_GAUGE:
		p.currentMetric.Gauge = &dto.Gauge{Value: proto.Float64(value)}
	case dto.MetricType_UNTYPED:
		p.currentMetric.Untyped = &dto.Untyped{Value: proto.Float64(value)}
	case dto.MetricType_SUMMARY:
		// *sigh*
		if p.currentMetric.Summary == nil {
			p.currentMetric.Summary = &dto.Summary{}
		}
		switch {
		case p.currentIsSummaryCount:
			p.currentMetric.Summary.SampleCount = proto.Uint64(uint64(value))
		case p.currentIsSummarySum:
			p.currentMetric.Summary.SampleSum = proto.Float64(value)
		case !math.IsNaN(p.currentQuantile):
			p.currentMetric.Summary.Quantile = append(
				p.currentMetric.Summary.Quantile,
				&dto.Quantile{
					Quantile: proto.Float64(p.currentQuantile),
					Value:    proto.Float64(value),
				},
			)
		}
	case dto.MetricType_HISTOGRAM:
		// *sigh*
		if p.currentMetric.Histogram == nil {
			p.currentMetric.Histogram = &dto.Histogram{}
		}
		switch {
		case p.currentIsHistogramCount:
			p.currentMetric.Histogram.SampleCount = proto.Uint64(uint64(value))
		case p.currentIsHistogramSum:
			p.currentMetric.Histogram.SampleSum = proto.Float64(value)
		case !math.IsNaN(p.currentBucket):
			p.currentMetric.Histogram.Bucket = append(
				p.currentMetric.Histogram.Bucket,
				&dto.Bucket{
					UpperBound:      proto.Float64(p.currentBucket),
					CumulativeCount: proto.Uint64(uint64(value)),
				},
			)
		}
	default:
		p.err = fmt.Errorf("unexpected type for metric name %q", p.currentMF.GetName())
	}
	if p.currentByte == '\n' {
		return p.startOfLine
	}
	return p.startTimestamp
}

// startTimestamp represents the state where the next byte read from p.buf is
// the start of the timestamp (or whitespace leading up to it).
func (p *TextParser) startTimestamp() stateFn {
	if p.skipBlankTab(); p.err != nil {
		return nil // Unexpected end of input.
	}
	if p.readTokenUntilWhitespace(); p.err != nil {
		return nil // Unexpected end of input.
	}
	timestamp, err := strconv.ParseInt(p.currentToken.String(), 10, 64)
	if err != nil {
		// Create a more helpful error message.
		p.parseError(fmt.Sprintf("expected integer as timestamp, got %q", p.currentToken.String()))
		return nil
	}
	p.currentMetric.TimestampMs = proto.Int64(timestamp)
	if p.readTokenUntilNewline(false); p.err != nil {
		return nil // Unexpected end of input.
	}
	if p.currentToken.Len() > 0 {
		p.parseError(fmt.Sprintf("spurious string after timestamp: %q", p.currentToken.String()))
		return nil
	}
	return p.startOfLine
}

// readingHelp represents the state where the last byte read (now in
// p.currentByte) is the first byte of the docstring after 'HELP'.
func (p *TextParser) readingHelp() stateFn {
	if p.currentMF.Help != nil {
		p.parseError(fmt.Sprintf("second HELP line for metric name %q", p.currentMF.GetName()))
		return nil
	}
	// Rest of line is the docstring.
	if p.readTokenUntilNewline(true); p.err != nil {
		return nil // Unexpected end of input.
	}
	p.currentMF.Help = proto.String(p.currentToken.String())
	return p.startOfLine
}

// readingType represents the state where the last byte read (now in
// p.currentByte) is the first byte of the type hint after 'HELP'.
func (p *TextParser) readingType() stateFn {
	if p.currentMF.Type != nil {
		p.parseError(fmt.Sprintf("second TYPE line for metric name %q, or TYPE reported after samples", p.currentMF.GetName()))
		return nil
	}
	// Rest of line is the type.
	if p.readTokenUntilNewline(false); p.err != nil {
		return nil // Unexpected end of input.
	}
	metricType, ok := dto.MetricType_value[strings.ToUpper(p.currentToken.String())]
	if !ok {
		p.parseError(fmt.Sprintf("unknown metric type %q", p.currentToken.String()))
		return nil
	}
	p.currentMF.Type = dto.MetricType(metricType).Enum()
	return p.startOfLine
}

// parseError sets p.err to a ParseError at the current line with the given
// message.
func (p *TextParser) parseError(msg string) {
	p.err = ParseError{
		Line: p.lineCount,
		Msg:  msg,
	}
}

// skipBlankTab reads (and discards) bytes from p.buf until it encounters a byte
// that is neither ' ' nor '\t'. That byte is left in p.currentByte.
func (p *TextParser) skipBlankTab() {
	for {
		if p.currentByte, p.err = p.buf.ReadByte(); p.err != nil || !isBlankOrTab(p.currentByte) {
			return
		}
	}
}

// skipBlankTabIfCurrentBlankTab works exactly as skipBlankTab but doesn't do
// anything if p.currentByte is neither ' ' nor '\t'.
func (p *TextParser) skipBlankTabIfCurrentBlankTab() {
	if isBlankOrTab(p.currentByte) {
		p.skipBlankTab()
	}
}

// readTokenUntilWhitespace copies bytes from p.buf into p.currentToken.  The
// first byte considered is the byte already read (now in p.currentByte).  The
// first whitespace byte encountered is still copied into p.currentByte, but not
// into p.currentToken.
func (p *TextParser) readTokenUntilWhitespace() {
	p.currentToken.Reset()
	for p.err == nil && !isBlankOrTab(p.currentByte) && p.currentByte != '\n' {
		p.currentToken.WriteByte(p.currentByte)
		p.currentByte, p.err = p.buf.ReadByte()
	}
}

// readTokenUntilNewline copies bytes from p.buf into p.currentToken.  The first
// byte considered is the byte already read (now in p.currentByte).  The first
// newline byte encountered is still copied into p.currentByte, but not into
// p.currentToken. If recognizeEscapeSequence is true, two escape sequences are
// recognized: '\\' translates into '\', and '\n' into a line-feed character.
// All other escape sequences are invalid and cause an error.
func (p *TextParser) readTokenUntilNewline(recognizeEscapeSequence bool) {
	p.currentToken.Reset()
	escaped := false
	for p.err == nil {
		if recognizeEscapeSequence && escaped {
			switch p.currentByte {
			case '\\':
				p.currentToken.WriteByte(p.currentByte)
			case 'n':
				p.currentToken.WriteByte('\n')
			case '"':
				p.currentToken.WriteByte('"')
			default:
				p.parseError(fmt.Sprintf("invalid escape sequence '\\%c'", p.currentByte))
				return
			}
			escaped = false
		} else {
			switch p.currentByte {
			case '\n':
				return
			case '\\':
				escaped = true
			default:
				p.currentToken.WriteByte(p.currentByte)
			}
		}
		p.currentByte, p.err = p.buf.ReadByte()
	}
}

// readTokenAsMetricName copies a metric name from p.buf into p.currentToken.
// The first byte considered is the byte already read (now in p.currentByte).
// The first byte not part of a metric name is still copied into p.currentByte,
// but not into p.currentToken.
func (p *TextParser) readTokenAsMetricName() {
	p.currentToken.Reset()
	// A UTF-8 metric name must be quoted and may have escaped characters.
	quoted := false
	escaped := false
	if !isValidMetricNameStart(p.currentByte) {
		return
	}
	for p.err == nil {
		if escaped {
			switch p.currentByte {
			case '\\':
				p.currentToken.WriteByte(p.currentByte)
			case 'n':
				p.currentToken.WriteByte('\n')
			case '"':
				p.currentToken.WriteByte('"')
			default:
				p.parseError(fmt.Sprintf("invalid escape sequence '\\%c'", p.currentByte))
				return
			}
			escaped = false
		} else {
			switch p.currentByte {
			case '"':
				quoted = !quoted
				if !quoted {
					p.currentByte, p.err = p.buf.ReadByte()
					return
				}
			case '\n':
				p.parseError(fmt.Sprintf("metric name %q contains unescaped new-line", p.currentToken.String()))
				return
			case '\\':
				escaped = true
			default:
				p.currentToken.WriteByte(p.currentByte)
			}
		}
		p.currentByte, p.err = p.buf.ReadByte()
		if !isValidMetricNameContinuation(p.currentByte, quoted) || (!quoted && p.currentByte == ' ') {
			return
		}
	}
}

// readTokenAsLabelName copies a label name from p.buf into p.currentToken.
// The first byte considered is the byte already read (now in p.currentByte).
// The first byte not part of a label name is still copied into p.currentByte,
// but not into p.currentToken.
func (p *TextParser) readTokenAsLabelName() {
	p.currentToken.Reset()
	// A UTF-8 label name must be quoted and may have escaped characters.
	quoted := false
	escaped := false
	if !isValidLabelNameStart(p.currentByte) {
		return
	}
	for p.err == nil {
		if escaped {
			switch p.currentByte {
			case '\\':
				p.currentToken.WriteByte(p.currentByte)
			case 'n':
				p.currentToken.WriteByte('\n')
			case '"':
				p.currentToken.WriteByte('"')
			default:
				p.parseError(fmt.Sprintf("invalid escape sequence '\\%c'", p.currentByte))
				return
			}
			escaped = false
		} else {
			switch p.currentByte {
			case '"':
				quoted = !quoted
				if !quoted {
					p.currentByte, p.err = p.buf.ReadByte()
					return
				}
			case '\n':
				p.parseError(fmt.Sprintf("label name %q contains unescaped new-line", p.currentToken.String()))
				return
			case '\\':
				escaped = true
			default:
				p.currentToken.WriteByte(p.currentByte)
			}
		}
		p.currentByte, p.err = p.buf.ReadByte()
		if !isValidLabelNameContinuation(p.currentByte, quoted) || (!quoted && p.currentByte == '=') {
			return
		}
	}
}

// readTokenAsLabelValue copies a label value from p.buf into p.currentToken.
// In contrast to the other 'readTokenAs...' functions, which start with the
// last read byte in p.currentByte, this method ignores p.currentByte and starts
// with reading a new byte from p.buf. The first byte not part of a label value
// is still copied into p.currentByte, but not into p.currentToken.
func (p *TextParser) readTokenAsLabelValue() {
	p.currentToken.Reset()
	escaped := false
	for {
		if p.currentByte, p.err = p.buf.ReadByte(); p.err != nil {
			return
		}
		if escaped {
			switch p.currentByte {
			case '"', '\\':
				p.currentToken.WriteByte(p.currentByte)
			case 'n':
				p.currentToken.WriteByte('\n')
			default:
				p.parseError(fmt.Sprintf("invalid escape sequence '\\%c'", p.currentByte))
				p.currentLabelPairs = nil
				return
			}
			escaped = false
			continue
		}
		switch p.currentByte {
		case '"':
			return
		case '\n':
			p.parseError(fmt.Sprintf("label value %q contains unescaped new-line", p.currentToken.String()))
			return
		case '\\':
			escaped = true
		default:
			p.currentToken.WriteByte(p.currentByte)
		}
	}
}

func (p *TextParser) setOrCreateCurrentMF() {
	p.currentIsSummaryCount = false
	p.currentIsSummarySum = false
	p.currentIsHistogramCount = false
	p.currentIsHistogramSum = false
	name := p.currentToken.String()
	if p.currentMF = p.metricFamiliesByName[name]; p.currentMF != nil {
		return
	}
	// Try out if this is a _sum or _count for a summary/histogram.
	summaryName := summaryMetricName(name)
	if p.currentMF = p.metricFamiliesByName[summaryName]; p.currentMF != nil {
		if p.currentMF.GetType() == dto.MetricType_SUMMARY {
			if isCount(name) {
				p.currentIsSummaryCount = true
			}
			if isSum(name) {
				p.currentIsSummarySum = true
			}
			return
		}
	}
	histogramName := histogramMetricName(name)
	if p.currentMF = p.metricFamiliesByName[histogramName]; p.currentMF != nil {
		if p.currentMF.GetType() == dto.MetricType_HISTOGRAM {
			if isCount(name) {
				p.currentIsHistogramCount = true
			}
			if isSum(name) {
				p.currentIsHistogramSum = true
			}
			return
		}
	}
	p.currentMF = &dto.MetricFamily{Name: proto.String(name)}
	p.metricFamiliesByName[name] = p.currentMF
}

func isValidLabelNameStart(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || b == '_' || b == '"'
}

func isValidLabelNameContinuation(b byte, quoted bool) bool {
	return isValidLabelNameStart(b) || (b >= '0' && b <= '9') || (quoted && utf8.ValidString(string(b)))
}

func isValidMetricNameStart(b byte) bool {
	return isValidLabelNameStart(b) || b == ':'
}

func isValidMetricNameContinuation(b byte, quoted bool) bool {
	return isValidLabelNameContinuation(b, quoted) || b == ':'
}

func isBlankOrTab(b byte) bool {
	return b == ' ' || b == '\t'
}

func isCount(name string) bool {
	return len(name) > 6 && name[len(name)-6:] == "_count"
}

func isSum(name string) bool {
	return len(name) > 4 && name[len(name)-4:] == "_sum"
}

func isBucket(name string) bool {
	return len(name) > 7 && name[len(name)-7:] == "_bucket"
}

func summaryMetricName(name string) string {
	switch {
	case isCount(name):
		return name[:len(name)-6]
	case isSum(name):
		return name[:len(name)-4]
	default:
		return name
	}
}

func histogramMetricName(name string) string {
	switch {
	case isCount(name):
		return name[:len(name)-6]
	case isSum(name):
		return name[:len(name)-4]
	case isBucket(name):
		return name[:len(name)-7]
	default:
		return name
	}
}

func parseFloat(s string) (float64, error) {
	if strings.ContainsAny(s, "pP_") {
		return 0, errors.New("unsupported character in float")
	}
	return strconv.ParseFloat(s, 64)
}
