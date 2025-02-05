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
	"errors"
	"fmt"
	"strconv"
	"strings"
)

type FloatString float64

func (v FloatString) String() string {
	return strconv.FormatFloat(float64(v), 'f', -1, 64)
}

func (v FloatString) MarshalJSON() ([]byte, error) {
	return json.Marshal(v.String())
}

func (v *FloatString) UnmarshalJSON(b []byte) error {
	if len(b) < 2 || b[0] != '"' || b[len(b)-1] != '"' {
		return errors.New("float value must be a quoted string")
	}
	f, err := strconv.ParseFloat(string(b[1:len(b)-1]), 64)
	if err != nil {
		return err
	}
	*v = FloatString(f)
	return nil
}

type HistogramBucket struct {
	Boundaries int32
	Lower      FloatString
	Upper      FloatString
	Count      FloatString
}

func (s HistogramBucket) MarshalJSON() ([]byte, error) {
	b, err := json.Marshal(s.Boundaries)
	if err != nil {
		return nil, err
	}
	l, err := json.Marshal(s.Lower)
	if err != nil {
		return nil, err
	}
	u, err := json.Marshal(s.Upper)
	if err != nil {
		return nil, err
	}
	c, err := json.Marshal(s.Count)
	if err != nil {
		return nil, err
	}
	return []byte(fmt.Sprintf("[%s,%s,%s,%s]", b, l, u, c)), nil
}

func (s *HistogramBucket) UnmarshalJSON(buf []byte) error {
	tmp := []interface{}{&s.Boundaries, &s.Lower, &s.Upper, &s.Count}
	wantLen := len(tmp)
	if err := json.Unmarshal(buf, &tmp); err != nil {
		return err
	}
	if gotLen := len(tmp); gotLen != wantLen {
		return fmt.Errorf("wrong number of fields: %d != %d", gotLen, wantLen)
	}
	return nil
}

func (s *HistogramBucket) Equal(o *HistogramBucket) bool {
	return s == o || (s.Boundaries == o.Boundaries && s.Lower == o.Lower && s.Upper == o.Upper && s.Count == o.Count)
}

func (b HistogramBucket) String() string {
	var sb strings.Builder
	lowerInclusive := b.Boundaries == 1 || b.Boundaries == 3
	upperInclusive := b.Boundaries == 0 || b.Boundaries == 3
	if lowerInclusive {
		sb.WriteRune('[')
	} else {
		sb.WriteRune('(')
	}
	fmt.Fprintf(&sb, "%g,%g", b.Lower, b.Upper)
	if upperInclusive {
		sb.WriteRune(']')
	} else {
		sb.WriteRune(')')
	}
	fmt.Fprintf(&sb, ":%v", b.Count)
	return sb.String()
}

type HistogramBuckets []*HistogramBucket

func (s HistogramBuckets) Equal(o HistogramBuckets) bool {
	if len(s) != len(o) {
		return false
	}

	for i, bucket := range s {
		if !bucket.Equal(o[i]) {
			return false
		}
	}
	return true
}

type SampleHistogram struct {
	Count   FloatString      `json:"count"`
	Sum     FloatString      `json:"sum"`
	Buckets HistogramBuckets `json:"buckets"`
}

func (s SampleHistogram) String() string {
	return fmt.Sprintf("Count: %f, Sum: %f, Buckets: %v", s.Count, s.Sum, s.Buckets)
}

func (s *SampleHistogram) Equal(o *SampleHistogram) bool {
	return s == o || (s.Count == o.Count && s.Sum == o.Sum && s.Buckets.Equal(o.Buckets))
}

type SampleHistogramPair struct {
	Timestamp Time
	// Histogram should never be nil, it's only stored as pointer for efficiency.
	Histogram *SampleHistogram
}

func (s SampleHistogramPair) MarshalJSON() ([]byte, error) {
	if s.Histogram == nil {
		return nil, errors.New("histogram is nil")
	}
	t, err := json.Marshal(s.Timestamp)
	if err != nil {
		return nil, err
	}
	v, err := json.Marshal(s.Histogram)
	if err != nil {
		return nil, err
	}
	return []byte(fmt.Sprintf("[%s,%s]", t, v)), nil
}

func (s *SampleHistogramPair) UnmarshalJSON(buf []byte) error {
	tmp := []interface{}{&s.Timestamp, &s.Histogram}
	wantLen := len(tmp)
	if err := json.Unmarshal(buf, &tmp); err != nil {
		return err
	}
	if gotLen := len(tmp); gotLen != wantLen {
		return fmt.Errorf("wrong number of fields: %d != %d", gotLen, wantLen)
	}
	if s.Histogram == nil {
		return errors.New("histogram is null")
	}
	return nil
}

func (s SampleHistogramPair) String() string {
	return fmt.Sprintf("%s @[%s]", s.Histogram, s.Timestamp)
}

func (s *SampleHistogramPair) Equal(o *SampleHistogramPair) bool {
	return s == o || (s.Histogram.Equal(o.Histogram) && s.Timestamp.Equal(o.Timestamp))
}
