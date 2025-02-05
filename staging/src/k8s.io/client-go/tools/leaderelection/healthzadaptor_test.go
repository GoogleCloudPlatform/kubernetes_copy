/*
Copyright 2015 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package leaderelection

import (
	"context"
	"fmt"
	"testing"
	"time"

	"net/http"

	rl "k8s.io/client-go/tools/leaderelection/resourcelock"
	testingclock "k8s.io/utils/clock/testing"
)

type fakeLock struct {
	identity string
}

// Get is a dummy to allow us to have a fakeLock for testing.
func (fl *fakeLock) Get(ctx context.Context) (ler *rl.LeaderElectionRecord, rawRecord []byte, err error) {
	return nil, nil, nil
}

// GetFromCache is a dummy to allow us to have a fakeLock for testing.
func (fl *fakeLock) GetFromCache(ctx context.Context) (ler *rl.LeaderElectionRecord, rawRecord []byte, err error) {
	return nil, nil, nil
}

// Create is a dummy to allow us to have a fakeLock for testing.
func (fl *fakeLock) Create(ctx context.Context, ler rl.LeaderElectionRecord) error {
	return nil
}

// Update is a dummy to allow us to have a fakeLock for testing.
func (fl *fakeLock) Update(ctx context.Context, ler rl.LeaderElectionRecord) error {
	return nil
}

// StartSync is a dummy to allow us to have a fakeLock for testing.
func (fl *fakeLock) StartSync(_ <-chan struct{}) {}

// RecordEvent is a dummy to allow us to have a fakeLock for testing.
func (fl *fakeLock) RecordEvent(string) {}

// Identity is a dummy to allow us to have a fakeLock for testing.
func (fl *fakeLock) Identity() string {
	return fl.identity
}

// Describe is a dummy to allow us to have a fakeLock for testing.
func (fl *fakeLock) Describe() string {
	return "Dummy implementation of lock for testing"
}

// TestLeaderElectionHealthChecker tests that the healthcheck for leader election handles its edge cases.
func TestLeaderElectionHealthChecker(t *testing.T) {
	current := time.Now()
	req := &http.Request{}

	tests := []struct {
		description    string
		expected       error
		adaptorTimeout time.Duration
		elector        *LeaderElector
	}{
		{
			description:    "call check before leader elector initialized",
			expected:       nil,
			adaptorTimeout: time.Second * 20,
			elector:        nil,
		},
		{
			description:    "call check when the lease is far expired",
			expected:       fmt.Errorf("failed election to renew leadership on lease %s", "foo"),
			adaptorTimeout: time.Second * 20,
			elector: &LeaderElector{
				config: LeaderElectionConfig{
					Lock:          &fakeLock{identity: "healthTest"},
					LeaseDuration: time.Minute,
					Name:          "foo",
				},
				observedRecord: rl.LeaderElectionRecord{
					HolderIdentity: "healthTest",
				},
				observedTime: current,
				clock:        testingclock.NewFakeClock(current.Add(time.Hour)),
			},
		},
		{
			description:    "call check when the lease is far expired but held by another server",
			expected:       nil,
			adaptorTimeout: time.Second * 20,
			elector: &LeaderElector{
				config: LeaderElectionConfig{
					Lock:          &fakeLock{identity: "healthTest"},
					LeaseDuration: time.Minute,
					Name:          "foo",
				},
				observedRecord: rl.LeaderElectionRecord{
					HolderIdentity: "otherServer",
				},
				observedTime: current,
				clock:        testingclock.NewFakeClock(current.Add(time.Hour)),
			},
		},
		{
			description:    "call check when the lease is not expired",
			expected:       nil,
			adaptorTimeout: time.Second * 20,
			elector: &LeaderElector{
				config: LeaderElectionConfig{
					Lock:          &fakeLock{identity: "healthTest"},
					LeaseDuration: time.Minute,
					Name:          "foo",
				},
				observedRecord: rl.LeaderElectionRecord{
					HolderIdentity: "healthTest",
				},
				observedTime: current,
				clock:        testingclock.NewFakeClock(current),
			},
		},
		{
			description:    "call check when the lease is expired but inside the timeout",
			expected:       nil,
			adaptorTimeout: time.Second * 20,
			elector: &LeaderElector{
				config: LeaderElectionConfig{
					Lock:          &fakeLock{identity: "healthTest"},
					LeaseDuration: time.Minute,
					Name:          "foo",
				},
				observedRecord: rl.LeaderElectionRecord{
					HolderIdentity: "healthTest",
				},
				observedTime: current,
				clock:        testingclock.NewFakeClock(current.Add(time.Minute).Add(time.Second)),
			},
		},
	}

	for _, test := range tests {
		adaptor := NewLeaderHealthzAdaptor(test.adaptorTimeout)
		if adaptor.le != nil {
			t.Errorf("[%s] leaderChecker started with a LeaderElector %v", test.description, adaptor.le)
		}
		if test.elector != nil {
			test.elector.config.WatchDog = adaptor
			adaptor.SetLeaderElection(test.elector)
			if adaptor.le == nil {
				t.Errorf("[%s] adaptor failed to set the LeaderElector", test.description)
			}
		}
		err := adaptor.Check(req)
		if test.expected == nil {
			if err == nil {
				continue
			}
			t.Errorf("[%s] called check, expected no error but received \"%v\"", test.description, err)
		} else {
			if err == nil {
				t.Errorf("[%s] called check and failed to received the expected error \"%v\"", test.description, test.expected)
			}
			if err.Error() != test.expected.Error() {
				t.Errorf("[%s] called check, expected %v, received %v", test.description, test.expected, err)
			}
		}
	}
}
