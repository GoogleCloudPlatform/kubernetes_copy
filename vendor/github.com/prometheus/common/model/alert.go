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
	"errors"
	"fmt"
	"time"
)

type AlertStatus string

const (
	AlertFiring   AlertStatus = "firing"
	AlertResolved AlertStatus = "resolved"
)

// Alert is a generic representation of an alert in the Prometheus eco-system.
type Alert struct {
	// Label value pairs for purpose of aggregation, matching, and disposition
	// dispatching. This must minimally include an "alertname" label.
	Labels LabelSet `json:"labels"`

	// Extra key/value information which does not define alert identity.
	Annotations LabelSet `json:"annotations"`

	// The known time range for this alert. Both ends are optional.
	StartsAt     time.Time `json:"startsAt,omitempty"`
	EndsAt       time.Time `json:"endsAt,omitempty"`
	GeneratorURL string    `json:"generatorURL"`
}

// Name returns the name of the alert. It is equivalent to the "alertname" label.
func (a *Alert) Name() string {
	return string(a.Labels[AlertNameLabel])
}

// Fingerprint returns a unique hash for the alert. It is equivalent to
// the fingerprint of the alert's label set.
func (a *Alert) Fingerprint() Fingerprint {
	return a.Labels.Fingerprint()
}

func (a *Alert) String() string {
	s := fmt.Sprintf("%s[%s]", a.Name(), a.Fingerprint().String()[:7])
	if a.Resolved() {
		return s + "[resolved]"
	}
	return s + "[active]"
}

// Resolved returns true iff the activity interval ended in the past.
func (a *Alert) Resolved() bool {
	return a.ResolvedAt(time.Now())
}

// ResolvedAt returns true off the activity interval ended before
// the given timestamp.
func (a *Alert) ResolvedAt(ts time.Time) bool {
	if a.EndsAt.IsZero() {
		return false
	}
	return !a.EndsAt.After(ts)
}

// Status returns the status of the alert.
func (a *Alert) Status() AlertStatus {
	return a.StatusAt(time.Now())
}

// StatusAt returns the status of the alert at the given timestamp.
func (a *Alert) StatusAt(ts time.Time) AlertStatus {
	if a.ResolvedAt(ts) {
		return AlertResolved
	}
	return AlertFiring
}

// Validate checks whether the alert data is inconsistent.
func (a *Alert) Validate() error {
	if a.StartsAt.IsZero() {
		return errors.New("start time missing")
	}
	if !a.EndsAt.IsZero() && a.EndsAt.Before(a.StartsAt) {
		return errors.New("start time must be before end time")
	}
	if err := a.Labels.Validate(); err != nil {
		return fmt.Errorf("invalid label set: %w", err)
	}
	if len(a.Labels) == 0 {
		return errors.New("at least one label pair required")
	}
	if err := a.Annotations.Validate(); err != nil {
		return fmt.Errorf("invalid annotations: %w", err)
	}
	return nil
}

// Alert is a list of alerts that can be sorted in chronological order.
type Alerts []*Alert

func (as Alerts) Len() int      { return len(as) }
func (as Alerts) Swap(i, j int) { as[i], as[j] = as[j], as[i] }

func (as Alerts) Less(i, j int) bool {
	if as[i].StartsAt.Before(as[j].StartsAt) {
		return true
	}
	if as[i].EndsAt.Before(as[j].EndsAt) {
		return true
	}
	return as[i].Fingerprint() < as[j].Fingerprint()
}

// HasFiring returns true iff one of the alerts is not resolved.
func (as Alerts) HasFiring() bool {
	for _, a := range as {
		if !a.Resolved() {
			return true
		}
	}
	return false
}

// HasFiringAt returns true iff one of the alerts is not resolved
// at the time ts.
func (as Alerts) HasFiringAt(ts time.Time) bool {
	for _, a := range as {
		if !a.ResolvedAt(ts) {
			return true
		}
	}
	return false
}

// Status returns StatusFiring iff at least one of the alerts is firing.
func (as Alerts) Status() AlertStatus {
	if as.HasFiring() {
		return AlertFiring
	}
	return AlertResolved
}

// StatusAt returns StatusFiring iff at least one of the alerts is firing
// at the time ts.
func (as Alerts) StatusAt(ts time.Time) AlertStatus {
	if as.HasFiringAt(ts) {
		return AlertFiring
	}
	return AlertResolved
}
