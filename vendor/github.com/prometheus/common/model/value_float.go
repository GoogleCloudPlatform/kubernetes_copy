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
	"math"
	"strconv"
)

// ZeroSamplePair is the pseudo zero-value of SamplePair used to signal a
// non-existing sample pair. It is a SamplePair with timestamp Earliest and
// value 0.0. Note that the natural zero value of SamplePair has a timestamp
// of 0, which is possible to appear in a real SamplePair and thus not
// suitable to signal a non-existing SamplePair.
var ZeroSamplePair = SamplePair{Timestamp: Earliest}

// A SampleValue is a representation of a value for a given sample at a given
// time.
type SampleValue float64

// MarshalJSON implements json.Marshaler.
func (v SampleValue) MarshalJSON() ([]byte, error) {
	return json.Marshal(v.String())
}

// UnmarshalJSON implements json.Unmarshaler.
func (v *SampleValue) UnmarshalJSON(b []byte) error {
	if len(b) < 2 || b[0] != '"' || b[len(b)-1] != '"' {
		return errors.New("sample value must be a quoted string")
	}
	f, err := strconv.ParseFloat(string(b[1:len(b)-1]), 64)
	if err != nil {
		return err
	}
	*v = SampleValue(f)
	return nil
}

// Equal returns true if the value of v and o is equal or if both are NaN. Note
// that v==o is false if both are NaN. If you want the conventional float
// behavior, use == to compare two SampleValues.
func (v SampleValue) Equal(o SampleValue) bool {
	if v == o {
		return true
	}
	return math.IsNaN(float64(v)) && math.IsNaN(float64(o))
}

func (v SampleValue) String() string {
	return strconv.FormatFloat(float64(v), 'f', -1, 64)
}

// SamplePair pairs a SampleValue with a Timestamp.
type SamplePair struct {
	Timestamp Time
	Value     SampleValue
}

func (s SamplePair) MarshalJSON() ([]byte, error) {
	t, err := json.Marshal(s.Timestamp)
	if err != nil {
		return nil, err
	}
	v, err := json.Marshal(s.Value)
	if err != nil {
		return nil, err
	}
	return []byte(fmt.Sprintf("[%s,%s]", t, v)), nil
}

// UnmarshalJSON implements json.Unmarshaler.
func (s *SamplePair) UnmarshalJSON(b []byte) error {
	v := [...]json.Unmarshaler{&s.Timestamp, &s.Value}
	return json.Unmarshal(b, &v)
}

// Equal returns true if this SamplePair and o have equal Values and equal
// Timestamps. The semantics of Value equality is defined by SampleValue.Equal.
func (s *SamplePair) Equal(o *SamplePair) bool {
	return s == o || (s.Value.Equal(o.Value) && s.Timestamp.Equal(o.Timestamp))
}

func (s SamplePair) String() string {
	return fmt.Sprintf("%s @[%s]", s.Value, s.Timestamp)
}
