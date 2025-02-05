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

package model

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"
)

// Matcher describes a matches the value of a given label.
type Matcher struct {
	Name    LabelName `json:"name"`
	Value   string    `json:"value"`
	IsRegex bool      `json:"isRegex"`
}

func (m *Matcher) UnmarshalJSON(b []byte) error {
	type plain Matcher
	if err := json.Unmarshal(b, (*plain)(m)); err != nil {
		return err
	}

	if len(m.Name) == 0 {
		return errors.New("label name in matcher must not be empty")
	}
	if m.IsRegex {
		if _, err := regexp.Compile(m.Value); err != nil {
			return err
		}
	}
	return nil
}

// Validate returns true iff all fields of the matcher have valid values.
func (m *Matcher) Validate() error {
	if !m.Name.IsValid() {
		return fmt.Errorf("invalid name %q", m.Name)
	}
	if m.IsRegex {
		if _, err := regexp.Compile(m.Value); err != nil {
			return fmt.Errorf("invalid regular expression %q", m.Value)
		}
	} else if !LabelValue(m.Value).IsValid() || len(m.Value) == 0 {
		return fmt.Errorf("invalid value %q", m.Value)
	}
	return nil
}

// Silence defines the representation of a silence definition in the Prometheus
// eco-system.
type Silence struct {
	ID uint64 `json:"id,omitempty"`

	Matchers []*Matcher `json:"matchers"`

	StartsAt time.Time `json:"startsAt"`
	EndsAt   time.Time `json:"endsAt"`

	CreatedAt time.Time `json:"createdAt,omitempty"`
	CreatedBy string    `json:"createdBy"`
	Comment   string    `json:"comment,omitempty"`
}

// Validate returns true iff all fields of the silence have valid values.
func (s *Silence) Validate() error {
	if len(s.Matchers) == 0 {
		return errors.New("at least one matcher required")
	}
	for _, m := range s.Matchers {
		if err := m.Validate(); err != nil {
			return fmt.Errorf("invalid matcher: %w", err)
		}
	}
	if s.StartsAt.IsZero() {
		return errors.New("start time missing")
	}
	if s.EndsAt.IsZero() {
		return errors.New("end time missing")
	}
	if s.EndsAt.Before(s.StartsAt) {
		return errors.New("start time must be before end time")
	}
	if s.CreatedBy == "" {
		return errors.New("creator information missing")
	}
	if s.Comment == "" {
		return errors.New("comment missing")
	}
	if s.CreatedAt.IsZero() {
		return errors.New("creation timestamp missing")
	}
	return nil
}
