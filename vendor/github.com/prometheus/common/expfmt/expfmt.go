// Copyright 2015 The Prometheus Authors
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

// Package expfmt contains tools for reading and writing Prometheus metrics.
package expfmt

import (
	"errors"
	"strings"

	"github.com/prometheus/common/model"
)

// Format specifies the HTTP content type of the different wire protocols.
type Format string

// Constants to assemble the Content-Type values for the different wire
// protocols. The Content-Type strings here are all for the legacy exposition
// formats, where valid characters for metric names and label names are limited.
// Support for arbitrary UTF-8 characters in those names is already partially
// implemented in this module (see model.ValidationScheme), but to actually use
// it on the wire, new content-type strings will have to be agreed upon and
// added here.
const (
	TextVersion   = "0.0.4"
	ProtoType     = `application/vnd.google.protobuf`
	ProtoProtocol = `io.prometheus.client.MetricFamily`
	// Deprecated: Use expfmt.NewFormat(expfmt.TypeProtoCompact) instead.
	ProtoFmt                 = ProtoType + "; proto=" + ProtoProtocol + ";"
	OpenMetricsType          = `application/openmetrics-text`
	OpenMetricsVersion_0_0_1 = "0.0.1"
	OpenMetricsVersion_1_0_0 = "1.0.0"

	// The Content-Type values for the different wire protocols. Do not do direct
	// comparisons to these constants, instead use the comparison functions.
	// Deprecated: Use expfmt.NewFormat(expfmt.TypeUnknown) instead.
	FmtUnknown Format = `<unknown>`
	// Deprecated: Use expfmt.NewFormat(expfmt.TypeTextPlain) instead.
	FmtText Format = `text/plain; version=` + TextVersion + `; charset=utf-8`
	// Deprecated: Use expfmt.NewFormat(expfmt.TypeProtoDelim) instead.
	FmtProtoDelim Format = ProtoFmt + ` encoding=delimited`
	// Deprecated: Use expfmt.NewFormat(expfmt.TypeProtoText) instead.
	FmtProtoText Format = ProtoFmt + ` encoding=text`
	// Deprecated: Use expfmt.NewFormat(expfmt.TypeProtoCompact) instead.
	FmtProtoCompact Format = ProtoFmt + ` encoding=compact-text`
	// Deprecated: Use expfmt.NewFormat(expfmt.TypeOpenMetrics) instead.
	FmtOpenMetrics_1_0_0 Format = OpenMetricsType + `; version=` + OpenMetricsVersion_1_0_0 + `; charset=utf-8`
	// Deprecated: Use expfmt.NewFormat(expfmt.TypeOpenMetrics) instead.
	FmtOpenMetrics_0_0_1 Format = OpenMetricsType + `; version=` + OpenMetricsVersion_0_0_1 + `; charset=utf-8`
)

const (
	hdrContentType = "Content-Type"
	hdrAccept      = "Accept"
)

// FormatType is a Go enum representing the overall category for the given
// Format. As the number of Format permutations increases, doing basic string
// comparisons are not feasible, so this enum captures the most useful
// high-level attribute of the Format string.
type FormatType int

const (
	TypeUnknown FormatType = iota
	TypeProtoCompact
	TypeProtoDelim
	TypeProtoText
	TypeTextPlain
	TypeOpenMetrics
)

// NewFormat generates a new Format from the type provided. Mostly used for
// tests, most Formats should be generated as part of content negotiation in
// encode.go. If a type has more than one version, the latest version will be
// returned.
func NewFormat(t FormatType) Format {
	switch t {
	case TypeProtoCompact:
		return FmtProtoCompact
	case TypeProtoDelim:
		return FmtProtoDelim
	case TypeProtoText:
		return FmtProtoText
	case TypeTextPlain:
		return FmtText
	case TypeOpenMetrics:
		return FmtOpenMetrics_1_0_0
	default:
		return FmtUnknown
	}
}

// NewOpenMetricsFormat generates a new OpenMetrics format matching the
// specified version number.
func NewOpenMetricsFormat(version string) (Format, error) {
	if version == OpenMetricsVersion_0_0_1 {
		return FmtOpenMetrics_0_0_1, nil
	}
	if version == OpenMetricsVersion_1_0_0 {
		return FmtOpenMetrics_1_0_0, nil
	}
	return FmtUnknown, errors.New("unknown open metrics version string")
}

// WithEscapingScheme returns a copy of Format with the specified escaping
// scheme appended to the end. If an escaping scheme already exists it is
// removed.
func (f Format) WithEscapingScheme(s model.EscapingScheme) Format {
	var terms []string
	for _, p := range strings.Split(string(f), ";") {
		toks := strings.Split(p, "=")
		if len(toks) != 2 {
			trimmed := strings.TrimSpace(p)
			if len(trimmed) > 0 {
				terms = append(terms, trimmed)
			}
			continue
		}
		key := strings.TrimSpace(toks[0])
		if key != model.EscapingKey {
			terms = append(terms, strings.TrimSpace(p))
		}
	}
	terms = append(terms, model.EscapingKey+"="+s.String())
	return Format(strings.Join(terms, "; "))
}

// FormatType deduces an overall FormatType for the given format.
func (f Format) FormatType() FormatType {
	toks := strings.Split(string(f), ";")
	params := make(map[string]string)
	for i, t := range toks {
		if i == 0 {
			continue
		}
		args := strings.Split(t, "=")
		if len(args) != 2 {
			continue
		}
		params[strings.TrimSpace(args[0])] = strings.TrimSpace(args[1])
	}

	switch strings.TrimSpace(toks[0]) {
	case ProtoType:
		if params["proto"] != ProtoProtocol {
			return TypeUnknown
		}
		switch params["encoding"] {
		case "delimited":
			return TypeProtoDelim
		case "text":
			return TypeProtoText
		case "compact-text":
			return TypeProtoCompact
		default:
			return TypeUnknown
		}
	case OpenMetricsType:
		if params["charset"] != "utf-8" {
			return TypeUnknown
		}
		return TypeOpenMetrics
	case "text/plain":
		v, ok := params["version"]
		if !ok {
			return TypeTextPlain
		}
		if v == TextVersion {
			return TypeTextPlain
		}
		return TypeUnknown
	default:
		return TypeUnknown
	}
}

// ToEscapingScheme returns an EscapingScheme depending on the Format. Iff the
// Format contains a escaping=allow-utf-8 term, it will select NoEscaping. If a valid
// "escaping" term exists, that will be used. Otherwise, the global default will
// be returned.
func (format Format) ToEscapingScheme() model.EscapingScheme {
	for _, p := range strings.Split(string(format), ";") {
		toks := strings.Split(p, "=")
		if len(toks) != 2 {
			continue
		}
		key, value := strings.TrimSpace(toks[0]), strings.TrimSpace(toks[1])
		if key == model.EscapingKey {
			scheme, err := model.ToEscapingScheme(value)
			if err != nil {
				return model.NameEscapingScheme
			}
			return scheme
		}
	}
	return model.NameEscapingScheme
}
