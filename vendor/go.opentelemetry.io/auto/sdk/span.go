// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package sdk

import (
	"encoding/json"
	"fmt"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"go.opentelemetry.io/auto/sdk/internal/telemetry"
)

type span struct {
	noop.Span

	spanContext trace.SpanContext
	sampled     atomic.Bool

	mu     sync.Mutex
	traces *telemetry.Traces
	span   *telemetry.Span
}

func (s *span) SpanContext() trace.SpanContext {
	if s == nil {
		return trace.SpanContext{}
	}
	// s.spanContext is immutable, do not acquire lock s.mu.
	return s.spanContext
}

func (s *span) IsRecording() bool {
	if s == nil {
		return false
	}

	return s.sampled.Load()
}

func (s *span) SetStatus(c codes.Code, msg string) {
	if s == nil || !s.sampled.Load() {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.span.Status == nil {
		s.span.Status = new(telemetry.Status)
	}

	s.span.Status.Message = msg

	switch c {
	case codes.Unset:
		s.span.Status.Code = telemetry.StatusCodeUnset
	case codes.Error:
		s.span.Status.Code = telemetry.StatusCodeError
	case codes.Ok:
		s.span.Status.Code = telemetry.StatusCodeOK
	}
}

func (s *span) SetAttributes(attrs ...attribute.KeyValue) {
	if s == nil || !s.sampled.Load() {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	limit := maxSpan.Attrs
	if limit == 0 {
		// No attributes allowed.
		s.span.DroppedAttrs += uint32(len(attrs))
		return
	}

	m := make(map[string]int)
	for i, a := range s.span.Attrs {
		m[a.Key] = i
	}

	for _, a := range attrs {
		val := convAttrValue(a.Value)
		if val.Empty() {
			s.span.DroppedAttrs++
			continue
		}

		if idx, ok := m[string(a.Key)]; ok {
			s.span.Attrs[idx] = telemetry.Attr{
				Key:   string(a.Key),
				Value: val,
			}
		} else if limit < 0 || len(s.span.Attrs) < limit {
			s.span.Attrs = append(s.span.Attrs, telemetry.Attr{
				Key:   string(a.Key),
				Value: val,
			})
			m[string(a.Key)] = len(s.span.Attrs) - 1
		} else {
			s.span.DroppedAttrs++
		}
	}
}

// convCappedAttrs converts up to limit attrs into a []telemetry.Attr. The
// number of dropped attributes is also returned.
func convCappedAttrs(limit int, attrs []attribute.KeyValue) ([]telemetry.Attr, uint32) {
	if limit == 0 {
		return nil, uint32(len(attrs))
	}

	if limit < 0 {
		// Unlimited.
		return convAttrs(attrs), 0
	}

	limit = min(len(attrs), limit)
	return convAttrs(attrs[:limit]), uint32(len(attrs) - limit)
}

func convAttrs(attrs []attribute.KeyValue) []telemetry.Attr {
	if len(attrs) == 0 {
		// Avoid allocations if not necessary.
		return nil
	}

	out := make([]telemetry.Attr, 0, len(attrs))
	for _, attr := range attrs {
		key := string(attr.Key)
		val := convAttrValue(attr.Value)
		if val.Empty() {
			continue
		}
		out = append(out, telemetry.Attr{Key: key, Value: val})
	}
	return out
}

func convAttrValue(value attribute.Value) telemetry.Value {
	switch value.Type() {
	case attribute.BOOL:
		return telemetry.BoolValue(value.AsBool())
	case attribute.INT64:
		return telemetry.Int64Value(value.AsInt64())
	case attribute.FLOAT64:
		return telemetry.Float64Value(value.AsFloat64())
	case attribute.STRING:
		v := truncate(maxSpan.AttrValueLen, value.AsString())
		return telemetry.StringValue(v)
	case attribute.BOOLSLICE:
		slice := value.AsBoolSlice()
		out := make([]telemetry.Value, 0, len(slice))
		for _, v := range slice {
			out = append(out, telemetry.BoolValue(v))
		}
		return telemetry.SliceValue(out...)
	case attribute.INT64SLICE:
		slice := value.AsInt64Slice()
		out := make([]telemetry.Value, 0, len(slice))
		for _, v := range slice {
			out = append(out, telemetry.Int64Value(v))
		}
		return telemetry.SliceValue(out...)
	case attribute.FLOAT64SLICE:
		slice := value.AsFloat64Slice()
		out := make([]telemetry.Value, 0, len(slice))
		for _, v := range slice {
			out = append(out, telemetry.Float64Value(v))
		}
		return telemetry.SliceValue(out...)
	case attribute.STRINGSLICE:
		slice := value.AsStringSlice()
		out := make([]telemetry.Value, 0, len(slice))
		for _, v := range slice {
			v = truncate(maxSpan.AttrValueLen, v)
			out = append(out, telemetry.StringValue(v))
		}
		return telemetry.SliceValue(out...)
	}
	return telemetry.Value{}
}

// truncate returns a truncated version of s such that it contains less than
// the limit number of characters. Truncation is applied by returning the limit
// number of valid characters contained in s.
//
// If limit is negative, it returns the original string.
//
// UTF-8 is supported. When truncating, all invalid characters are dropped
// before applying truncation.
//
// If s already contains less than the limit number of bytes, it is returned
// unchanged. No invalid characters are removed.
func truncate(limit int, s string) string {
	// This prioritize performance in the following order based on the most
	// common expected use-cases.
	//
	//  - Short values less than the default limit (128).
	//  - Strings with valid encodings that exceed the limit.
	//  - No limit.
	//  - Strings with invalid encodings that exceed the limit.
	if limit < 0 || len(s) <= limit {
		return s
	}

	// Optimistically, assume all valid UTF-8.
	var b strings.Builder
	count := 0
	for i, c := range s {
		if c != utf8.RuneError {
			count++
			if count > limit {
				return s[:i]
			}
			continue
		}

		_, size := utf8.DecodeRuneInString(s[i:])
		if size == 1 {
			// Invalid encoding.
			b.Grow(len(s) - 1)
			_, _ = b.WriteString(s[:i])
			s = s[i:]
			break
		}
	}

	// Fast-path, no invalid input.
	if b.Cap() == 0 {
		return s
	}

	// Truncate while validating UTF-8.
	for i := 0; i < len(s) && count < limit; {
		c := s[i]
		if c < utf8.RuneSelf {
			// Optimization for single byte runes (common case).
			_ = b.WriteByte(c)
			i++
			count++
			continue
		}

		_, size := utf8.DecodeRuneInString(s[i:])
		if size == 1 {
			// We checked for all 1-byte runes above, this is a RuneError.
			i++
			continue
		}

		_, _ = b.WriteString(s[i : i+size])
		i += size
		count++
	}

	return b.String()
}

func (s *span) End(opts ...trace.SpanEndOption) {
	if s == nil || !s.sampled.Swap(false) {
		return
	}

	// s.end exists so the lock (s.mu) is not held while s.ended is called.
	s.ended(s.end(opts))
}

func (s *span) end(opts []trace.SpanEndOption) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()

	cfg := trace.NewSpanEndConfig(opts...)
	if t := cfg.Timestamp(); !t.IsZero() {
		s.span.EndTime = cfg.Timestamp()
	} else {
		s.span.EndTime = time.Now()
	}

	b, _ := json.Marshal(s.traces) // TODO: do not ignore this error.
	return b
}

// Expected to be implemented in eBPF.
//
//go:noinline
func (*span) ended(buf []byte) { ended(buf) }

// ended is used for testing.
var ended = func([]byte) {}

func (s *span) RecordError(err error, opts ...trace.EventOption) {
	if s == nil || err == nil || !s.sampled.Load() {
		return
	}

	cfg := trace.NewEventConfig(opts...)

	attrs := cfg.Attributes()
	attrs = append(attrs,
		semconv.ExceptionType(typeStr(err)),
		semconv.ExceptionMessage(err.Error()),
	)
	if cfg.StackTrace() {
		buf := make([]byte, 2048)
		n := runtime.Stack(buf, false)
		attrs = append(attrs, semconv.ExceptionStacktrace(string(buf[0:n])))
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.addEvent(semconv.ExceptionEventName, cfg.Timestamp(), attrs)
}

func typeStr(i any) string {
	t := reflect.TypeOf(i)
	if t.PkgPath() == "" && t.Name() == "" {
		// Likely a builtin type.
		return t.String()
	}
	return fmt.Sprintf("%s.%s", t.PkgPath(), t.Name())
}

func (s *span) AddEvent(name string, opts ...trace.EventOption) {
	if s == nil || !s.sampled.Load() {
		return
	}

	cfg := trace.NewEventConfig(opts...)

	s.mu.Lock()
	defer s.mu.Unlock()

	s.addEvent(name, cfg.Timestamp(), cfg.Attributes())
}

// addEvent adds an event with name and attrs at tStamp to the span. The span
// lock (s.mu) needs to be held by the caller.
func (s *span) addEvent(name string, tStamp time.Time, attrs []attribute.KeyValue) {
	limit := maxSpan.Events

	if limit == 0 {
		s.span.DroppedEvents++
		return
	}

	if limit > 0 && len(s.span.Events) == limit {
		// Drop head while avoiding allocation of more capacity.
		copy(s.span.Events[:limit-1], s.span.Events[1:])
		s.span.Events = s.span.Events[:limit-1]
		s.span.DroppedEvents++
	}

	e := &telemetry.SpanEvent{Time: tStamp, Name: name}
	e.Attrs, e.DroppedAttrs = convCappedAttrs(maxSpan.EventAttrs, attrs)

	s.span.Events = append(s.span.Events, e)
}

func (s *span) AddLink(link trace.Link) {
	if s == nil || !s.sampled.Load() {
		return
	}

	l := maxSpan.Links

	s.mu.Lock()
	defer s.mu.Unlock()

	if l == 0 {
		s.span.DroppedLinks++
		return
	}

	if l > 0 && len(s.span.Links) == l {
		// Drop head while avoiding allocation of more capacity.
		copy(s.span.Links[:l-1], s.span.Links[1:])
		s.span.Links = s.span.Links[:l-1]
		s.span.DroppedLinks++
	}

	s.span.Links = append(s.span.Links, convLink(link))
}

func convLinks(links []trace.Link) []*telemetry.SpanLink {
	out := make([]*telemetry.SpanLink, 0, len(links))
	for _, link := range links {
		out = append(out, convLink(link))
	}
	return out
}

func convLink(link trace.Link) *telemetry.SpanLink {
	l := &telemetry.SpanLink{
		TraceID:    telemetry.TraceID(link.SpanContext.TraceID()),
		SpanID:     telemetry.SpanID(link.SpanContext.SpanID()),
		TraceState: link.SpanContext.TraceState().String(),
		Flags:      uint32(link.SpanContext.TraceFlags()),
	}
	l.Attrs, l.DroppedAttrs = convCappedAttrs(maxSpan.LinkAttrs, link.Attributes)

	return l
}

func (s *span) SetName(name string) {
	if s == nil || !s.sampled.Load() {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.span.Name = name
}

func (*span) TracerProvider() trace.TracerProvider { return TracerProvider() }
