/*
Copyright 2022 The Kubernetes Authors.

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
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	flowcontrol "k8s.io/api/flowcontrol/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/apiserver/pkg/endpoints/request"
	fq "k8s.io/apiserver/pkg/util/flowcontrol/fairqueuing"
	"k8s.io/apiserver/pkg/util/flowcontrol/fairqueuing/eventclock"
	fqs "k8s.io/apiserver/pkg/util/flowcontrol/fairqueuing/queueset"
	"k8s.io/apiserver/pkg/util/flowcontrol/metrics"
	fcrequest "k8s.io/apiserver/pkg/util/flowcontrol/request"
	"k8s.io/client-go/informers"
	clientsetfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/klog/v2/ktesting"
	"k8s.io/utils/ptr"
)

type borrowingTestConstraints struct {
	lendable, borrowing int32
}

// TestBorrowing tests borrowing of concurrency between priority levels.
// It runs two scenarios, one where the borrowing hits the limit on
// lendable concurrency and one where the borrowing hits the limit on
// borrowing of concurrency.
// Both scenarios are the same except for the limits.
// The test defines two priority levels, identified as "flows" 0 and 1.
// Both priority levels have a nominal concurrency limit of 12.
// The test maintains 24 concurrent clients for priority level 0
// and 6 for level 1,
// using an exec func that simply sleeps for 250 ms, for
// 25 seconds.  The first 10 seconds of behavior are ignored, allowing
// the borrowing to start at any point during that time.  The test
// continues for another 15 seconds, and checks that the delivered
// concurrency is about 16 for flow 0 and 6 for flow 1.
func TestBorrowing(t *testing.T) {
	clientsPerFlow := [2]int{24, 6}
	metrics.Register()
	for _, testCase := range []struct {
		name        string
		constraints []borrowingTestConstraints
	}{
		{
			name: "lendable-limited",
			constraints: []borrowingTestConstraints{
				{lendable: 50, borrowing: 67},
				{lendable: 33, borrowing: 50},
			}},
		{
			name: "borrowing-limited",
			constraints: []borrowingTestConstraints{
				{lendable: 50, borrowing: 33},
				{lendable: 67, borrowing: 50},
			}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			fsObjs := make([]*flowcontrol.FlowSchema, 2)
			plcObjs := make([]*flowcontrol.PriorityLevelConfiguration, 2)
			usernames := make([]string, 2)
			cfgObjs := []runtime.Object{}
			for flow := 0; flow < 2; flow++ {
				usernames[flow] = fmt.Sprintf("test-user%d", flow)
				plName := fmt.Sprintf("test-pl%d", flow)
				fsObjs[flow] = &flowcontrol.FlowSchema{
					ObjectMeta: metav1.ObjectMeta{
						Name: fmt.Sprintf("test-fs%d", flow),
					},
					Spec: flowcontrol.FlowSchemaSpec{
						MatchingPrecedence: 100,
						PriorityLevelConfiguration: flowcontrol.PriorityLevelConfigurationReference{
							Name: plName,
						},
						DistinguisherMethod: &flowcontrol.FlowDistinguisherMethod{
							Type: flowcontrol.FlowDistinguisherMethodByUserType,
						},
						Rules: []flowcontrol.PolicyRulesWithSubjects{{
							Subjects: []flowcontrol.Subject{{
								Kind: flowcontrol.SubjectKindUser,
								User: &flowcontrol.UserSubject{Name: usernames[flow]},
							}},
							NonResourceRules: []flowcontrol.NonResourcePolicyRule{{
								Verbs:           []string{"*"},
								NonResourceURLs: []string{"*"},
							}},
						}},
					},
				}
				plcObjs[flow] = &flowcontrol.PriorityLevelConfiguration{
					ObjectMeta: metav1.ObjectMeta{
						Name: plName,
					},
					Spec: flowcontrol.PriorityLevelConfigurationSpec{
						Type: flowcontrol.PriorityLevelEnablementLimited,
						Limited: &flowcontrol.LimitedPriorityLevelConfiguration{
							NominalConcurrencyShares: ptr.To(int32(100)),
							LendablePercent:          &testCase.constraints[flow].lendable,
							BorrowingLimitPercent:    &testCase.constraints[flow].borrowing,
							LimitResponse: flowcontrol.LimitResponse{
								Type: flowcontrol.LimitResponseTypeQueue,
								Queuing: &flowcontrol.QueuingConfiguration{
									Queues:           10,
									HandSize:         2,
									QueueLengthLimit: 10,
								},
							},
						},
					},
				}
				cfgObjs = append(cfgObjs, fsObjs[flow], plcObjs[flow])
			}
			clientset := clientsetfake.NewSimpleClientset(cfgObjs...)
			informerFactory := informers.NewSharedInformerFactory(clientset, time.Second)
			flowcontrolClient := clientset.FlowcontrolV1()
			clk := eventclock.Real{}
			controller := newTestableController(TestableConfig{
				Name:                   "Controller",
				Clock:                  clk,
				AsFieldManager:         ConfigConsumerAsFieldManager,
				FoundToDangling:        func(found bool) bool { return !found },
				InformerFactory:        informerFactory,
				FlowcontrolClient:      flowcontrolClient,
				ServerConcurrencyLimit: 24,
				ReqsGaugeVec:           metrics.PriorityLevelConcurrencyGaugeVec,
				ExecSeatsGaugeVec:      metrics.PriorityLevelExecutionSeatsGaugeVec,
				QueueSetFactory:        fqs.NewQueueSetFactory(clk),
			})

			_, ctx := ktesting.NewTestContext(t)
			ctx, cancel := context.WithTimeout(ctx, 50*time.Second)

			informerFactory.Start(ctx.Done())

			status := informerFactory.WaitForCacheSync(ctx.Done())
			if names := unsynced(status); len(names) > 0 {
				t.Fatalf("WaitForCacheSync did not successfully complete, resources=%#v", names)
			}

			if err := controller.Start(ctx); err != nil {
				t.Fatalf("error starting controller: %v", err)
			}

			// ensure that the controller has run its first loop.
			err := wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 5*time.Second, true, func(ctx context.Context) (bool, error) {
				return controller.hasPriorityLevelState(plcObjs[0].Name), nil
			})
			if err != nil {
				t.Errorf("expected the controller to reconcile the priority level configuration object: %s, error: %s", plcObjs[0].Name, err)
			}

			concIntegrators := make([]fq.Integrator, 2)
			reqInfo := &request.RequestInfo{
				IsResourceRequest: false,
				Path:              "/foobar",
				Verb:              "GET",
			}
			noteFn := func(fs *flowcontrol.FlowSchema, plc *flowcontrol.PriorityLevelConfiguration, fd string) {}
			workEstr := func() fcrequest.WorkEstimate { return fcrequest.WorkEstimate{InitialSeats: 1} }
			qnf := fq.QueueNoteFn(func(bool) {})
			var startWG sync.WaitGroup
			startWG.Add(clientsPerFlow[0] + clientsPerFlow[1])
			// Launch 20 client threads for each flow
			for flow := 0; flow < 2; flow++ {
				username := usernames[flow]
				flowUser := testUser{name: username}
				rd := RequestDigest{
					RequestInfo: reqInfo,
					User:        flowUser,
				}
				concIntegrator := fq.NewNamedIntegrator(clk, username)
				concIntegrators[flow] = concIntegrator
				exec := func() {
					concIntegrator.Inc()
					clk.Sleep(250 * time.Millisecond)
					concIntegrator.Dec()
				}
				nThreads := clientsPerFlow[flow]
				for thread := 0; thread < nThreads; thread++ {
					go func() {
						startWG.Done()
						wait.Until(func() { controller.Handle(ctx, rd, noteFn, workEstr, qnf, exec) }, 0, ctx.Done())
					}()
				}
			}
			startWG.Wait()
			// Make sure the controller has had time to sense the load and adjust
			clk.Sleep(10 * time.Second)
			// Start the stats that matter from now
			for _, ci := range concIntegrators {
				ci.Reset()
			}
			// Run for 15 seconds
			clk.Sleep(15 * time.Second)
			// Collect the delivered concurrency stats
			results0 := concIntegrators[0].Reset()
			results1 := concIntegrators[1].Reset()
			// shut down all the async stuff
			cancel()

			// Do the checking

			if results0.Average < 15.5 || results0.Average > 16.1 {
				t.Errorf("Flow 0 got average concurrency of %v but expected about 16", results0.Average)
			} else {
				t.Logf("Flow 0 got average concurrency of %v and expected about 16", results0.Average)
			}
			if results1.Average < 5.5 || results1.Average > 6.1 {
				t.Errorf("Flow 1 got average concurrency of %v but expected about 6", results1.Average)
			} else {
				t.Logf("Flow 1 got average concurrency of %v and expected about 6", results1.Average)
			}
		})
	}
}

type testUser struct{ name string }

func (tu testUser) GetName() string               { return tu.name }
func (tu testUser) GetUID() string                { return tu.name }
func (tu testUser) GetGroups() []string           { return []string{user.AllAuthenticated} }
func (tu testUser) GetExtra() map[string][]string { return map[string][]string{} }
