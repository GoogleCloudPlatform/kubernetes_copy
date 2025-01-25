/*
Copyright 2023 The Kubernetes Authors.

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

package flowcontrol

import (
	"testing"
	"time"

	flowcontrolv1 "k8s.io/api/flowcontrol/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	fqs "k8s.io/apiserver/pkg/util/flowcontrol/fairqueuing/queueset"
	"k8s.io/apiserver/pkg/util/flowcontrol/fairqueuing/testing/eventclock"
	"k8s.io/apiserver/pkg/util/flowcontrol/metrics"
	"k8s.io/client-go/informers"
	clientsetfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/klog/v2/ktesting"
	"k8s.io/utils/ptr"
)

// Test_GetMaxSeats tests max seats retrieved from MaxSeatsTracker
func Test_GetMaxSeats(t *testing.T) {
	testcases := []struct {
		name             string
		nominalCL        int
		handSize         int32
		expectedMaxSeats uint64
	}{
		{
			name:             "nominalCL=5, handSize=6",
			nominalCL:        5,
			handSize:         6,
			expectedMaxSeats: 1,
		},
		{
			name:             "nominalCL=10, handSize=6",
			nominalCL:        10,
			handSize:         6,
			expectedMaxSeats: 1,
		},
		{
			name:             "nominalCL=15, handSize=6",
			nominalCL:        15,
			handSize:         6,
			expectedMaxSeats: 2,
		},
		{
			name:             "nominalCL=20, handSize=6",
			nominalCL:        20,
			handSize:         6,
			expectedMaxSeats: 3,
		},
		{
			name:             "nominalCL=35, handSize=6",
			nominalCL:        35,
			handSize:         6,
			expectedMaxSeats: 5,
		},
		{
			name:             "nominalCL=100, handSize=6",
			nominalCL:        100,
			handSize:         6,
			expectedMaxSeats: 15,
		},
		{
			name:             "nominalCL=200, handSize=6",
			nominalCL:        200,
			handSize:         6,
			expectedMaxSeats: 30,
		},
		{
			name:             "nominalCL=10, handSize=1",
			nominalCL:        10,
			handSize:         1,
			expectedMaxSeats: 2,
		},
		{
			name:             "nominalCL=100, handSize=20",
			nominalCL:        100,
			handSize:         20,
			expectedMaxSeats: 5,
		},
	}

	for _, testcase := range testcases {
		t.Run(testcase.name, func(t *testing.T) {
			_, ctx := ktesting.NewTestContext(t)

			clientset := clientsetfake.NewSimpleClientset()
			informerFactory := informers.NewSharedInformerFactory(clientset, time.Second)
			flowcontrolClient := clientset.FlowcontrolV1()
			startTime := time.Now()
			clk, _ := eventclock.NewFake(startTime, 0, nil)
			c := newTestableController(TestableConfig{
				Name:              "Controller",
				Clock:             clk,
				InformerFactory:   informerFactory,
				FlowcontrolClient: flowcontrolClient,
				// for the purposes of this test, serverCL ~= nominalCL since there is
				// only 1 PL with large concurrency shares, making mandatory PLs negligible.
				ServerConcurrencyLimit: testcase.nominalCL,
				ReqsGaugeVec:           metrics.PriorityLevelConcurrencyGaugeVec,
				ExecSeatsGaugeVec:      metrics.PriorityLevelExecutionSeatsGaugeVec,
				QueueSetFactory:        fqs.NewQueueSetFactory(clk),
			})

			testPriorityLevel := &flowcontrolv1.PriorityLevelConfiguration{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-pl",
				},
				Spec: flowcontrolv1.PriorityLevelConfigurationSpec{
					Type: flowcontrolv1.PriorityLevelEnablementLimited,
					Limited: &flowcontrolv1.LimitedPriorityLevelConfiguration{
						NominalConcurrencyShares: ptr.To(int32(10000)),
						LimitResponse: flowcontrolv1.LimitResponse{
							Queuing: &flowcontrolv1.QueuingConfiguration{
								HandSize: testcase.handSize,
							},
						},
					},
				},
			}
			if _, err := c.digestConfigObjects(ctx, []*flowcontrolv1.PriorityLevelConfiguration{testPriorityLevel}, nil); err != nil {
				t.Errorf("unexpected error from digestConfigObjects: %v", err)
			}
			maxSeats := c.GetMaxSeats("test-pl")
			if maxSeats != testcase.expectedMaxSeats {
				t.Errorf("unexpected max seats, got=%d, want=%d", maxSeats, testcase.expectedMaxSeats)
			}
		})
	}
}
