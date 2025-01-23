/*
Copyright 2025 The Kubernetes Authors.

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

package events

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	v1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"

	eventsv1 "k8s.io/api/events/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilvalidation "k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/watch"
	testclocks "k8s.io/utils/clock/testing"
)

func TestEventf(t *testing.T) {
	fakeClock := testclocks.NewFakeClock(time.Now())

	testCases := []struct {
		desc          string
		regarding     runtime.Object
		related       runtime.Object
		eventtype     string
		reason        string
		action        string
		note          string
		args          []interface{}
		expectedEvent *eventsv1.Event
	}{
		{
			desc: "normal event",
			regarding: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "pod1",
					Namespace: "ns1",
					UID:       "12345",
				},
			},
			eventtype: "Normal",
			reason:    "Started",
			action:    "starting",
			note:      "Pod started",
			expectedEvent: &eventsv1.Event{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("pod1.%x", fakeClock.Now().UnixNano()),
					Namespace: "ns1",
				},
				Regarding: v1.ObjectReference{
					Kind:       "Pod",
					Name:       "pod1",
					Namespace:  "ns1",
					UID:        "12345",
					APIVersion: "v1",
				},
				Type:                "Normal",
				Reason:              "Started",
				Action:              "starting",
				Note:                "Pod started",
				ReportingController: "c1",
				ReportingInstance:   "i1",
			},
		},
		{
			desc: "event with related object and format args",
			regarding: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "pod1",
					Namespace: "ns1",
					UID:       "12345",
				},
			},
			related: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "node1",
					UID:  "67890",
				},
			},
			eventtype: "Warning",
			reason:    "FailedScheduling",
			action:    "scheduling",

			note: "Pod failed to schedule on %s: %s",
			args: []interface{}{"node1", "not enough resources"},
			expectedEvent: &eventsv1.Event{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("pod1.%x", fakeClock.Now().UnixNano()),
					Namespace: "ns1",
				},
				Regarding: v1.ObjectReference{
					Kind:       "Pod",
					Name:       "pod1",
					Namespace:  "ns1",
					UID:        "12345",
					APIVersion: "v1",
				},
				Related: &v1.ObjectReference{
					Kind:       "Node",
					Name:       "node1",
					UID:        "67890",
					APIVersion: "v1",
				},
				Type:                "Warning",
				Reason:              "FailedScheduling",
				Action:              "scheduling",
				Note:                "Pod failed to schedule on node1: not enough resources",
				ReportingController: "c1",
				ReportingInstance:   "i1",
			},
		}, {
			desc: "event with invalid Event name",
			regarding: &networkingv1.IPAddress{
				ObjectMeta: metav1.ObjectMeta{
					Name: "2001:db8::123",
					UID:  "12345",
				},
			},
			eventtype: "Warning",
			reason:    "IPAddressNotAllocated",
			action:    "IPAddressAllocation",
			note:      "Service default/test appears to have leaked",

			expectedEvent: &eventsv1.Event{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("2001-db8--123.%x", fakeClock.Now().UnixNano()),
					Namespace: "default",
				},
				Regarding: v1.ObjectReference{
					Kind:       "IPAddress",
					Name:       "2001:db8::123",
					UID:        "12345",
					APIVersion: "networking.k8s.io/v1",
				},
				Type:                "Warning",
				Reason:              "IPAddressNotAllocated",
				Action:              "IPAddressAllocation",
				Note:                "Service default/test appears to have leaked",
				ReportingController: "c1",
				ReportingInstance:   "i1",
			},
		}, {
			desc: "large event name",
			regarding: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      strings.Repeat("x", utilvalidation.DNS1123SubdomainMaxLength*4),
					Namespace: "ns1",
					UID:       "12345",
				},
			},
			eventtype: "Normal",
			reason:    "Started",
			action:    "starting",
			note:      "Pod started",
			expectedEvent: &eventsv1.Event{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("%s.%x", strings.Repeat("x", 236), fakeClock.Now().UnixNano()),
					Namespace: "ns1",
				},
				Regarding: v1.ObjectReference{
					Kind:       "Pod",
					Name:       strings.Repeat("x", utilvalidation.DNS1123SubdomainMaxLength*4),
					Namespace:  "ns1",
					UID:        "12345",
					APIVersion: "v1",
				},
				Type:                "Normal",
				Reason:              "Started",
				Action:              "starting",
				Note:                "Pod started",
				ReportingController: "c1",
				ReportingInstance:   "i1",
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.desc, func(t *testing.T) {
			broadcaster := watch.NewBroadcaster(1, watch.WaitIfChannelFull)
			recorder := &recorderImpl{
				scheme:              runtime.NewScheme(),
				reportingController: "c1",
				reportingInstance:   "i1",
				Broadcaster:         broadcaster,
				clock:               fakeClock,
			}

			if err := v1.AddToScheme(recorder.scheme); err != nil {
				t.Fatal(err)
			}
			if err := networkingv1.AddToScheme(recorder.scheme); err != nil {
				t.Fatal(err)
			}
			ch, err := broadcaster.Watch()
			if err != nil {
				t.Fatal(err)
			}
			recorder.Eventf(tc.regarding, tc.related, tc.eventtype, tc.reason, tc.action, tc.note, tc.args...)

			select {
			case event := <-ch.ResultChan():
				actualEvent := event.Object.(*eventsv1.Event)
				// Overwrite field that is not relevant for comparison
				tc.expectedEvent.EventTime = actualEvent.EventTime
				if diff := cmp.Diff(tc.expectedEvent, actualEvent); diff != "" {
					t.Errorf("Unexpected event diff (-want, +got):\n%s", diff)
				}
			case <-time.After(time.Second):
				t.Errorf("Timeout waiting for event")
			}

		})
	}
}
