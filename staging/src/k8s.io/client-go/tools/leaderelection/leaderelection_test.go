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
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/stretchr/testify/assert"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes/fake"
	fakeclient "k8s.io/client-go/testing"
	rl "k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2/ktesting"
	"k8s.io/utils/clock"
)

func createLockObject(t *testing.T, objectType, namespace, name string, record *rl.LeaderElectionRecord) (obj runtime.Object) {
	objectMeta := metav1.ObjectMeta{
		Namespace: namespace,
		Name:      name,
	}
	if record != nil {
		recordBytes, _ := json.Marshal(record)
		objectMeta.Annotations = map[string]string{
			rl.LeaderElectionRecordAnnotationKey: string(recordBytes),
		}
	}
	switch objectType {
	case "endpoints":
		obj = &corev1.Endpoints{ObjectMeta: objectMeta}
	case "configmaps":
		obj = &corev1.ConfigMap{ObjectMeta: objectMeta}
	case "leases":
		var spec coordinationv1.LeaseSpec
		if record != nil {
			spec = rl.LeaderElectionRecordToLeaseSpec(record)
		}
		obj = &coordinationv1.Lease{ObjectMeta: objectMeta, Spec: spec}
	default:
		t.Fatal("unexpected objType:" + objectType)
	}
	return
}

type Reactor struct {
	verb       string
	objectType string
	reaction   fakeclient.ReactionFunc
}

func testTryAcquireOrRenew(t *testing.T, objectType string) {
	clock := clock.RealClock{}
	future := clock.Now().Add(1000 * time.Hour)
	past := clock.Now().Add(-1000 * time.Hour)

	tests := []struct {
		name           string
		observedRecord rl.LeaderElectionRecord
		observedTime   time.Time
		retryAfter     time.Duration
		reactors       []Reactor
		expectedEvents []string

		expectSuccess    bool
		transitionLeader bool
		outHolder        string
	}{
		{
			name: "acquire from no object",
			reactors: []Reactor{
				{
					verb: "get",
					reaction: func(action fakeclient.Action) (handled bool, ret runtime.Object, err error) {
						return true, nil, errors.NewNotFound(action.(fakeclient.GetAction).GetResource().GroupResource(), action.(fakeclient.GetAction).GetName())
					},
				},
				{
					verb: "create",
					reaction: func(action fakeclient.Action) (handled bool, ret runtime.Object, err error) {
						return true, action.(fakeclient.CreateAction).GetObject(), nil
					},
				},
			},
			expectSuccess: true,
			outHolder:     "baz",
		},
		{
			name: "acquire from object without annotations",
			reactors: []Reactor{
				{
					verb: "get",
					reaction: func(action fakeclient.Action) (handled bool, ret runtime.Object, err error) {
						return true, createLockObject(t, objectType, action.GetNamespace(), action.(fakeclient.GetAction).GetName(), nil), nil
					},
				},
				{
					verb: "update",
					reaction: func(action fakeclient.Action) (handled bool, ret runtime.Object, err error) {
						return true, action.(fakeclient.CreateAction).GetObject(), nil
					},
				},
			},
			expectSuccess:    true,
			transitionLeader: true,
			outHolder:        "baz",
		},
		{
			name: "acquire from led object with the lease duration seconds",
			reactors: []Reactor{
				{
					verb: "get",
					reaction: func(action fakeclient.Action) (handled bool, ret runtime.Object, err error) {
						return true, createLockObject(t, objectType, action.GetNamespace(), action.(fakeclient.GetAction).GetName(), &rl.LeaderElectionRecord{HolderIdentity: "bing", LeaseDurationSeconds: 3}), nil
					},
				},
				{
					verb: "get",
					reaction: func(action fakeclient.Action) (handled bool, ret runtime.Object, err error) {
						return true, createLockObject(t, objectType, action.GetNamespace(), action.(fakeclient.GetAction).GetName(), &rl.LeaderElectionRecord{HolderIdentity: "bing", LeaseDurationSeconds: 3}), nil
					},
				},
				{
					verb: "update",
					reaction: func(action fakeclient.Action) (handled bool, ret runtime.Object, err error) {
						return true, action.(fakeclient.CreateAction).GetObject(), nil
					},
				},
			},
			retryAfter:       3 * time.Second,
			expectSuccess:    true,
			transitionLeader: true,
			outHolder:        "baz",
		},
		{
			name: "acquire from unled object",
			reactors: []Reactor{
				{
					verb: "get",
					reaction: func(action fakeclient.Action) (handled bool, ret runtime.Object, err error) {
						return true, createLockObject(t, objectType, action.GetNamespace(), action.(fakeclient.GetAction).GetName(), &rl.LeaderElectionRecord{}), nil
					},
				},
				{
					verb: "update",
					reaction: func(action fakeclient.Action) (handled bool, ret runtime.Object, err error) {
						return true, action.(fakeclient.CreateAction).GetObject(), nil
					},
				},
			},

			expectSuccess:    true,
			transitionLeader: true,
			outHolder:        "baz",
		},
		{
			name: "acquire from led, unacked object",
			reactors: []Reactor{
				{
					verb: "get",
					reaction: func(action fakeclient.Action) (handled bool, ret runtime.Object, err error) {
						return true, createLockObject(t, objectType, action.GetNamespace(), action.(fakeclient.GetAction).GetName(), &rl.LeaderElectionRecord{HolderIdentity: "bing"}), nil
					},
				},
				{
					verb: "update",
					reaction: func(action fakeclient.Action) (handled bool, ret runtime.Object, err error) {
						return true, action.(fakeclient.CreateAction).GetObject(), nil
					},
				},
			},
			observedRecord: rl.LeaderElectionRecord{HolderIdentity: "bing"},
			observedTime:   past,

			expectSuccess:    true,
			transitionLeader: true,
			outHolder:        "baz",
		},
		{
			name: "acquire from empty led, acked object",
			reactors: []Reactor{
				{
					verb: "get",
					reaction: func(action fakeclient.Action) (handled bool, ret runtime.Object, err error) {
						return true, createLockObject(t, objectType, action.GetNamespace(), action.(fakeclient.GetAction).GetName(), &rl.LeaderElectionRecord{HolderIdentity: ""}), nil
					},
				},
				{
					verb: "update",
					reaction: func(action fakeclient.Action) (handled bool, ret runtime.Object, err error) {
						return true, action.(fakeclient.CreateAction).GetObject(), nil
					},
				},
			},
			observedTime: future,

			expectSuccess:    true,
			transitionLeader: true,
			outHolder:        "baz",
		},
		{
			name: "don't acquire from led, acked object",
			reactors: []Reactor{
				{
					verb: "get",
					reaction: func(action fakeclient.Action) (handled bool, ret runtime.Object, err error) {
						return true, createLockObject(t, objectType, action.GetNamespace(), action.(fakeclient.GetAction).GetName(), &rl.LeaderElectionRecord{HolderIdentity: "bing"}), nil
					},
				},
			},
			observedTime: future,

			expectSuccess: false,
			outHolder:     "bing",
		},
		{
			name: "renew already acquired object",
			reactors: []Reactor{
				{
					verb: "get",
					reaction: func(action fakeclient.Action) (handled bool, ret runtime.Object, err error) {
						return true, createLockObject(t, objectType, action.GetNamespace(), action.(fakeclient.GetAction).GetName(), &rl.LeaderElectionRecord{HolderIdentity: "baz"}), nil
					},
				},
				{
					verb: "update",
					reaction: func(action fakeclient.Action) (handled bool, ret runtime.Object, err error) {
						return true, action.(fakeclient.CreateAction).GetObject(), nil
					},
				},
			},
			observedTime:   future,
			observedRecord: rl.LeaderElectionRecord{HolderIdentity: "baz"},

			expectSuccess: true,
			outHolder:     "baz",
		},
	}

	for i := range tests {
		test := &tests[i]
		t.Run(test.name, func(t *testing.T) {
			_, ctx := ktesting.NewTestContext(t)

			// OnNewLeader is called async so we have to wait for it.
			var wg sync.WaitGroup
			wg.Add(1)
			var reportedLeader string
			var lock rl.Interface

			objectMeta := metav1.ObjectMeta{Namespace: "foo", Name: "bar"}
			recorder := record.NewFakeRecorder(100)
			resourceLockConfig := rl.ResourceLockConfig{
				Identity:      "baz",
				EventRecorder: recorder,
			}
			c := &fake.Clientset{}
			for _, reactor := range test.reactors {
				c.AddReactor(reactor.verb, objectType, reactor.reaction)
			}
			c.AddReactor("*", "*", func(action fakeclient.Action) (bool, runtime.Object, error) {
				t.Errorf("unreachable action. testclient called too many times: %+v", action)
				return true, nil, fmt.Errorf("unreachable action")
			})

			switch objectType {
			case "leases":
				lock = &rl.LeaseLock{
					LeaseMeta:  objectMeta,
					LockConfig: resourceLockConfig,
					Client:     c.CoordinationV1(),
				}
			default:
				t.Fatalf("Unknown objectType: %v", objectType)
			}

			lec := LeaderElectionConfig{
				Lock:          lock,
				LeaseDuration: 10 * time.Second,
				Callbacks: LeaderCallbacks{
					OnNewLeader: func(l string) {
						defer wg.Done()
						reportedLeader = l
					},
				},
			}
			observedRawRecord := GetRawRecordOrDie(t, objectType, test.observedRecord)
			le := &LeaderElector{
				config:            lec,
				observedRecord:    test.observedRecord,
				observedRawRecord: observedRawRecord,
				observedTime:      test.observedTime,
				clock:             clock,
				metrics:           globalMetricsFactory.newLeaderMetrics(),
			}
			if test.expectSuccess != le.tryAcquireOrRenew(ctx) {
				if test.retryAfter != 0 {
					time.Sleep(test.retryAfter)
					if test.expectSuccess != le.tryAcquireOrRenew(ctx) {
						t.Errorf("unexpected result of tryAcquireOrRenew: [succeeded=%v]", !test.expectSuccess)
					}
				} else {
					t.Errorf("unexpected result of tryAcquireOrRenew: [succeeded=%v]", !test.expectSuccess)
				}
			}

			le.observedRecord.AcquireTime = metav1.Time{}
			le.observedRecord.RenewTime = metav1.Time{}
			if le.observedRecord.HolderIdentity != test.outHolder {
				t.Errorf("expected holder:\n\t%+v\ngot:\n\t%+v", test.outHolder, le.observedRecord.HolderIdentity)
			}
			if len(test.reactors) != len(c.Actions()) {
				t.Errorf("wrong number of api interactions")
			}
			if test.transitionLeader && le.observedRecord.LeaderTransitions != 1 {
				t.Errorf("leader should have transitioned but did not")
			}
			if !test.transitionLeader && le.observedRecord.LeaderTransitions != 0 {
				t.Errorf("leader should not have transitioned but did")
			}

			le.maybeReportTransition()
			wg.Wait()
			if reportedLeader != test.outHolder {
				t.Errorf("reported leader was not the new leader. expected %q, got %q", test.outHolder, reportedLeader)
			}
			assertEqualEvents(t, test.expectedEvents, recorder.Events)
		})
	}
}

func TestTryCoordinatedRenew(t *testing.T) {
	objectType := "leases"
	clock := clock.RealClock{}
	future := clock.Now().Add(1000 * time.Hour)

	tests := []struct {
		name           string
		observedRecord rl.LeaderElectionRecord
		observedTime   time.Time
		retryAfter     time.Duration
		reactors       []Reactor
		expectedEvents []string

		expectSuccess    bool
		transitionLeader bool
		outHolder        string
	}{
		{
			name: "don't acquire from led, acked object",
			reactors: []Reactor{
				{
					verb: "get",
					reaction: func(action fakeclient.Action) (handled bool, ret runtime.Object, err error) {
						return true, createLockObject(t, objectType, action.GetNamespace(), action.(fakeclient.GetAction).GetName(), &rl.LeaderElectionRecord{HolderIdentity: "bing"}), nil
					},
				},
			},
			observedTime: future,

			expectSuccess: false,
			outHolder:     "bing",
		},
		{
			name: "renew already acquired object",
			reactors: []Reactor{
				{
					verb: "get",
					reaction: func(action fakeclient.Action) (handled bool, ret runtime.Object, err error) {
						return true, createLockObject(t, objectType, action.GetNamespace(), action.(fakeclient.GetAction).GetName(), &rl.LeaderElectionRecord{HolderIdentity: "baz"}), nil
					},
				},
				{
					verb: "update",
					reaction: func(action fakeclient.Action) (handled bool, ret runtime.Object, err error) {
						return true, action.(fakeclient.CreateAction).GetObject(), nil
					},
				},
			},
			observedTime:   future,
			observedRecord: rl.LeaderElectionRecord{HolderIdentity: "baz"},

			expectSuccess: true,
			outHolder:     "baz",
		},
	}

	for i := range tests {
		test := &tests[i]
		t.Run(test.name, func(t *testing.T) {
			_, ctx := ktesting.NewTestContext(t)

			// OnNewLeader is called async so we have to wait for it.
			var wg sync.WaitGroup
			wg.Add(1)
			var reportedLeader string
			var lock rl.Interface

			objectMeta := metav1.ObjectMeta{Namespace: "foo", Name: "bar"}
			recorder := record.NewFakeRecorder(100)
			resourceLockConfig := rl.ResourceLockConfig{
				Identity:      "baz",
				EventRecorder: recorder,
			}
			c := &fake.Clientset{}
			for _, reactor := range test.reactors {
				c.AddReactor(reactor.verb, objectType, reactor.reaction)
			}
			c.AddReactor("*", "*", func(action fakeclient.Action) (bool, runtime.Object, error) {
				t.Errorf("unreachable action. testclient called too many times: %+v", action)
				return true, nil, fmt.Errorf("unreachable action")
			})

			lock = &rl.LeaseLock{
				LeaseMeta:  objectMeta,
				LockConfig: resourceLockConfig,
				Client:     c.CoordinationV1(),
			}
			lec := LeaderElectionConfig{
				Lock:          lock,
				LeaseDuration: 10 * time.Second,
				Callbacks: LeaderCallbacks{
					OnNewLeader: func(l string) {
						defer wg.Done()
						reportedLeader = l
					},
				},
				Coordinated: true,
			}
			observedRawRecord := GetRawRecordOrDie(t, objectType, test.observedRecord)
			le := &LeaderElector{
				config:            lec,
				observedRecord:    test.observedRecord,
				observedRawRecord: observedRawRecord,
				observedTime:      test.observedTime,
				clock:             clock,
				metrics:           globalMetricsFactory.newLeaderMetrics(),
			}
			if test.expectSuccess != le.tryCoordinatedRenew(ctx) {
				if test.retryAfter != 0 {
					time.Sleep(test.retryAfter)
					if test.expectSuccess != le.tryCoordinatedRenew(ctx) {
						t.Errorf("unexpected result of tryCoordinatedRenew: [succeeded=%v]", !test.expectSuccess)
					}
				} else {
					t.Errorf("unexpected result of gryCoordinatedRenew: [succeeded=%v]", !test.expectSuccess)
				}
			}

			le.observedRecord.AcquireTime = metav1.Time{}
			le.observedRecord.RenewTime = metav1.Time{}
			if le.observedRecord.HolderIdentity != test.outHolder {
				t.Errorf("expected holder:\n\t%+v\ngot:\n\t%+v", test.outHolder, le.observedRecord.HolderIdentity)
			}
			if len(test.reactors) != len(c.Actions()) {
				t.Errorf("wrong number of api interactions")
			}
			if test.transitionLeader && le.observedRecord.LeaderTransitions != 1 {
				t.Errorf("leader should have transitioned but did not")
			}
			if !test.transitionLeader && le.observedRecord.LeaderTransitions != 0 {
				t.Errorf("leader should not have transitioned but did")
			}

			le.maybeReportTransition()
			wg.Wait()
			if reportedLeader != test.outHolder {
				t.Errorf("reported leader was not the new leader. expected %q, got %q", test.outHolder, reportedLeader)
			}
			assertEqualEvents(t, test.expectedEvents, recorder.Events)
		})
	}
}

// Will test leader election using lease as the resource
func TestTryAcquireOrRenewLeases(t *testing.T) {
	testTryAcquireOrRenew(t, "leases")
}

func TestLeaseSpecToLeaderElectionRecordRoundTrip(t *testing.T) {
	holderIdentity := "foo"
	leaseDurationSeconds := int32(10)
	leaseTransitions := int32(1)
	oldSpec := coordinationv1.LeaseSpec{
		HolderIdentity:       &holderIdentity,
		LeaseDurationSeconds: &leaseDurationSeconds,
		AcquireTime:          &metav1.MicroTime{Time: time.Now()},
		RenewTime:            &metav1.MicroTime{Time: time.Now()},
		LeaseTransitions:     &leaseTransitions,
	}

	oldRecord := rl.LeaseSpecToLeaderElectionRecord(&oldSpec)
	newSpec := rl.LeaderElectionRecordToLeaseSpec(oldRecord)

	if !equality.Semantic.DeepEqual(oldSpec, newSpec) {
		t.Errorf("diff: %v", cmp.Diff(oldSpec, newSpec))
	}

	newRecord := rl.LeaseSpecToLeaderElectionRecord(&newSpec)

	if !equality.Semantic.DeepEqual(oldRecord, newRecord) {
		t.Errorf("diff: %v", cmp.Diff(oldRecord, newRecord))
	}
}

func GetRawRecordOrDie(t *testing.T, objectType string, ler rl.LeaderElectionRecord) (ret []byte) {
	var err error
	switch objectType {
	case "leases":
		ret, err = json.Marshal(ler)
		if err != nil {
			t.Fatalf("lock %s get raw record %v failed: %v", objectType, ler, err)
		}
	default:
		t.Fatal("unexpected objType:" + objectType)
	}
	return
}

func testReleaseLease(t *testing.T, objectType string) {
	tests := []struct {
		name           string
		observedRecord rl.LeaderElectionRecord
		observedTime   time.Time
		reactors       []Reactor
		expectedEvents []string

		expectSuccess    bool
		transitionLeader bool
		outHolder        string
	}{
		{
			name: "release acquired lock from no object",
			reactors: []Reactor{
				{
					verb:       "get",
					objectType: objectType,
					reaction: func(action fakeclient.Action) (handled bool, ret runtime.Object, err error) {
						return true, nil, errors.NewNotFound(action.(fakeclient.GetAction).GetResource().GroupResource(), action.(fakeclient.GetAction).GetName())
					},
				},
				{
					verb:       "create",
					objectType: objectType,
					reaction: func(action fakeclient.Action) (handled bool, ret runtime.Object, err error) {
						return true, action.(fakeclient.CreateAction).GetObject(), nil
					},
				},
				{
					verb:       "update",
					objectType: objectType,
					reaction: func(action fakeclient.Action) (handled bool, ret runtime.Object, err error) {
						return true, action.(fakeclient.UpdateAction).GetObject(), nil
					},
				},
			},
			expectSuccess: true,
			outHolder:     "",
		},
	}

	for i := range tests {
		test := &tests[i]
		t.Run(test.name, func(t *testing.T) {
			logger, ctx := ktesting.NewTestContext(t)

			// OnNewLeader is called async so we have to wait for it.
			var wg sync.WaitGroup
			wg.Add(1)
			var reportedLeader string
			var lock rl.Interface

			objectMeta := metav1.ObjectMeta{Namespace: "foo", Name: "bar"}
			recorder := record.NewFakeRecorder(100)
			resourceLockConfig := rl.ResourceLockConfig{
				Identity:      "baz",
				EventRecorder: recorder,
			}
			c := &fake.Clientset{}
			for _, reactor := range test.reactors {
				c.AddReactor(reactor.verb, objectType, reactor.reaction)
			}
			c.AddReactor("*", "*", func(action fakeclient.Action) (bool, runtime.Object, error) {
				t.Errorf("unreachable action. testclient called too many times: %+v", action)
				return true, nil, fmt.Errorf("unreachable action")
			})

			switch objectType {
			case "leases":
				lock = &rl.LeaseLock{
					LeaseMeta:  objectMeta,
					LockConfig: resourceLockConfig,
					Client:     c.CoordinationV1(),
				}
			default:
				t.Fatalf("Unknown objectType: %v", objectType)
			}

			lec := LeaderElectionConfig{
				Lock:          lock,
				LeaseDuration: 10 * time.Second,
				Callbacks: LeaderCallbacks{
					OnNewLeader: func(l string) {
						defer wg.Done()
						reportedLeader = l
					},
				},
			}
			observedRawRecord := GetRawRecordOrDie(t, objectType, test.observedRecord)
			le := &LeaderElector{
				config:            lec,
				observedRecord:    test.observedRecord,
				observedRawRecord: observedRawRecord,
				observedTime:      test.observedTime,
				clock:             clock.RealClock{},
				metrics:           globalMetricsFactory.newLeaderMetrics(),
			}
			if !le.tryAcquireOrRenew(ctx) {
				t.Errorf("unexpected result of tryAcquireOrRenew: [succeeded=%v]", true)
			}

			le.maybeReportTransition()

			// Wait for a response to the leader transition, and add 1 so that we can track the final transition.
			wg.Wait()
			wg.Add(1)

			if test.expectSuccess != le.release(logger) {
				t.Errorf("unexpected result of release: [succeeded=%v]", !test.expectSuccess)
			}

			le.observedRecord.AcquireTime = metav1.Time{}
			le.observedRecord.RenewTime = metav1.Time{}
			if le.observedRecord.HolderIdentity != test.outHolder {
				t.Errorf("expected holder:\n\t%+v\ngot:\n\t%+v", test.outHolder, le.observedRecord.HolderIdentity)
			}
			if len(test.reactors) != len(c.Actions()) {
				t.Errorf("wrong number of api interactions")
			}
			if test.transitionLeader && le.observedRecord.LeaderTransitions != 1 {
				t.Errorf("leader should have transitioned but did not")
			}
			if !test.transitionLeader && le.observedRecord.LeaderTransitions != 0 {
				t.Errorf("leader should not have transitioned but did")
			}
			le.maybeReportTransition()
			wg.Wait()
			if reportedLeader != test.outHolder {
				t.Errorf("reported leader was not the new leader. expected %q, got %q", test.outHolder, reportedLeader)
			}
			assertEqualEvents(t, test.expectedEvents, recorder.Events)
		})
	}
}

// Will test leader election using endpoints as the resource
func TestReleaseLeaseLeases(t *testing.T) {
	testReleaseLease(t, "leases")
}

func TestReleaseOnCancellation_Leases(t *testing.T) {
	testReleaseOnCancellation(t, "leases")
}

func testReleaseOnCancellation(t *testing.T, objectType string) {
	var (
		onNewLeader   = make(chan struct{})
		onRenewCalled = make(chan struct{})
		onRenewResume = make(chan struct{})
		onRelease     = make(chan struct{})

		lockObj runtime.Object
		gets    int
		updates int
		wg      sync.WaitGroup
	)
	resetVars := func() {
		onNewLeader = make(chan struct{})
		onRenewCalled = make(chan struct{})
		onRenewResume = make(chan struct{})
		onRelease = make(chan struct{})

		lockObj = nil
		gets = 0
		updates = 0
	}
	lec := LeaderElectionConfig{
		LeaseDuration: 15 * time.Second,
		RenewDeadline: 2 * time.Second,
		RetryPeriod:   1 * time.Second,

		// This is what we're testing
		ReleaseOnCancel: true,

		Callbacks: LeaderCallbacks{
			OnNewLeader:      func(identity string) {},
			OnStoppedLeading: func() {},
			OnStartedLeading: func(context.Context) {
				close(onNewLeader)
			},
		},
	}

	tests := []struct {
		name           string
		reactors       []Reactor
		expectedEvents []string
	}{
		{
			name: "release acquired lock on cancellation of update",
			reactors: []Reactor{
				{
					verb:       "get",
					objectType: objectType,
					reaction: func(action fakeclient.Action) (handled bool, ret runtime.Object, err error) {
						gets++
						if lockObj != nil {
							return true, lockObj, nil
						}
						return true, nil, errors.NewNotFound(action.(fakeclient.GetAction).GetResource().GroupResource(), action.(fakeclient.GetAction).GetName())
					},
				},
				{
					verb:       "create",
					objectType: objectType,
					reaction: func(action fakeclient.Action) (handled bool, ret runtime.Object, err error) {
						lockObj = action.(fakeclient.CreateAction).GetObject()
						return true, lockObj, nil
					},
				},
				{
					verb:       "update",
					objectType: objectType,
					reaction: func(action fakeclient.Action) (handled bool, ret runtime.Object, err error) {
						updates++
						// Skip initial two fast path renews
						if updates%2 == 1 && updates < 5 {
							return true, nil, context.Canceled
						}

						// Second update (first renew) should return our canceled error
						// FakeClient doesn't do anything with the context so we're doing this ourselves
						if updates == 4 {
							close(onRenewCalled)
							<-onRenewResume
							return true, nil, context.Canceled
						} else if updates == 5 {
							// We update the lock after the cancellation to release it
							// This wg is to avoid the data race on lockObj
							defer wg.Done()
							close(onRelease)
						}

						lockObj = action.(fakeclient.UpdateAction).GetObject()
						return true, lockObj, nil
					},
				},
			},
			expectedEvents: []string{
				"Normal LeaderElection baz became leader",
				"Normal LeaderElection baz stopped leading",
			},
		},
		{
			name: "release acquired lock on cancellation of get",
			reactors: []Reactor{
				{
					verb:       "get",
					objectType: objectType,
					reaction: func(action fakeclient.Action) (handled bool, ret runtime.Object, err error) {
						gets++
						if lockObj != nil {
							// Third and more get (first create, second renew) should return our canceled error
							// FakeClient doesn't do anything with the context so we're doing this ourselves
							if gets >= 3 {
								close(onRenewCalled)
								<-onRenewResume
								return true, nil, context.Canceled
							}
							return true, lockObj, nil
						}
						return true, nil, errors.NewNotFound(action.(fakeclient.GetAction).GetResource().GroupResource(), action.(fakeclient.GetAction).GetName())
					},
				},
				{
					verb:       "create",
					objectType: objectType,
					reaction: func(action fakeclient.Action) (handled bool, ret runtime.Object, err error) {
						lockObj = action.(fakeclient.CreateAction).GetObject()
						return true, lockObj, nil
					},
				},
				{
					verb:       "update",
					objectType: objectType,
					reaction: func(action fakeclient.Action) (handled bool, ret runtime.Object, err error) {
						updates++
						// Always skip fast path renew
						if updates%2 == 1 {
							return true, nil, context.Canceled
						}
						// Second update (first renew) should release the lock
						if updates == 4 {
							// We update the lock after the cancellation to release it
							// This wg is to avoid the data race on lockObj
							defer wg.Done()
							close(onRelease)
						}

						lockObj = action.(fakeclient.UpdateAction).GetObject()
						return true, lockObj, nil
					},
				},
			},
			expectedEvents: []string{
				"Normal LeaderElection baz became leader",
				"Normal LeaderElection baz stopped leading",
			},
		},
	}

	for i := range tests {
		test := &tests[i]
		t.Run(test.name, func(t *testing.T) {
			_, ctx := ktesting.NewTestContext(t)

			wg.Add(1)
			resetVars()

			recorder := record.NewFakeRecorder(100)
			resourceLockConfig := rl.ResourceLockConfig{
				Identity:      "baz",
				EventRecorder: recorder,
			}
			c := &fake.Clientset{}
			for _, reactor := range test.reactors {
				c.AddReactor(reactor.verb, objectType, reactor.reaction)
			}
			c.AddReactor("*", "*", func(action fakeclient.Action) (bool, runtime.Object, error) {
				t.Errorf("unreachable action. testclient called too many times: %+v", action)
				return true, nil, fmt.Errorf("unreachable action")
			})
			lock, err := rl.New(objectType, "foo", "bar", c.CoreV1(), c.CoordinationV1(), resourceLockConfig)
			if err != nil {
				t.Fatal("resourcelock.New() = ", err)
			}

			lec.Lock = lock
			elector, err := NewLeaderElector(lec)
			if err != nil {
				t.Fatal("Failed to create leader elector: ", err)
			}

			ctx, cancel := context.WithCancel(ctx)

			go elector.Run(ctx)

			// Wait for us to become the leader
			select {
			case <-onNewLeader:
			case <-time.After(10 * time.Second):
				t.Fatal("failed to become the leader")
			}

			// Wait for renew (update) to be invoked
			select {
			case <-onRenewCalled:
			case <-time.After(10 * time.Second):
				t.Fatal("the elector failed to renew the lock")
			}

			// Cancel the context - stopping the elector while
			// it's running
			cancel()

			// Resume the tryAcquireOrRenew call to return the cancellation
			// which should trigger the release flow
			close(onRenewResume)

			select {
			case <-onRelease:
			case <-time.After(10 * time.Second):
				t.Fatal("the lock was not released")
			}
			wg.Wait()
			assertEqualEvents(t, test.expectedEvents, recorder.Events)
		})
	}
}

func TestLeaderElectionConfigValidation(t *testing.T) {
	resourceLockConfig := rl.ResourceLockConfig{
		Identity: "baz",
	}

	lock := &rl.LeaseLock{
		LockConfig: resourceLockConfig,
	}

	lec := LeaderElectionConfig{
		Lock:          lock,
		LeaseDuration: 15 * time.Second,
		RenewDeadline: 2 * time.Second,
		RetryPeriod:   1 * time.Second,

		ReleaseOnCancel: true,

		Callbacks: LeaderCallbacks{
			OnNewLeader:      func(identity string) {},
			OnStoppedLeading: func() {},
			OnStartedLeading: func(context.Context) {},
		},
	}

	_, err := NewLeaderElector(lec)
	assert.NoError(t, err)

	// Invalid lock identity
	resourceLockConfig.Identity = ""
	lock.LockConfig = resourceLockConfig
	lec.Lock = lock
	_, err = NewLeaderElector(lec)
	assert.EqualError(t, err, "Lock identity is empty")
}

func assertEqualEvents(t *testing.T, expected []string, actual <-chan string) {
	c := time.After(wait.ForeverTestTimeout)
	for _, e := range expected {
		select {
		case a := <-actual:
			if e != a {
				t.Errorf("Expected event %q, got %q", e, a)
				return
			}
		case <-c:
			t.Errorf("Expected event %q, got nothing", e)
			// continue iterating to print all expected events
		}
	}
	for {
		select {
		case a := <-actual:
			t.Errorf("Unexpected event: %q", a)
		default:
			return // No more events, as expected.
		}
	}
}

func TestFastPathLeaderElection(t *testing.T) {
	objectType := "leases"
	var (
		lockObj    runtime.Object
		updates    int
		lockOps    []string
		cancelFunc func()
	)
	resetVars := func() {
		lockObj = nil
		updates = 0
		lockOps = []string{}
		cancelFunc = nil
	}
	lec := LeaderElectionConfig{
		LeaseDuration: 15 * time.Second,
		RenewDeadline: 2 * time.Second,
		RetryPeriod:   1 * time.Second,

		Callbacks: LeaderCallbacks{
			OnNewLeader:      func(identity string) {},
			OnStoppedLeading: func() {},
			OnStartedLeading: func(context.Context) {
			},
		},
	}

	tests := []struct {
		name            string
		reactors        []Reactor
		expectedLockOps []string
	}{
		{
			name: "Exercise fast path after lock acquired",
			reactors: []Reactor{
				{
					verb:       "get",
					objectType: objectType,
					reaction: func(action fakeclient.Action) (handled bool, ret runtime.Object, err error) {
						lockOps = append(lockOps, "get")
						if lockObj != nil {
							return true, lockObj, nil
						}
						return true, nil, errors.NewNotFound(action.(fakeclient.GetAction).GetResource().GroupResource(), action.(fakeclient.GetAction).GetName())
					},
				},
				{
					verb:       "create",
					objectType: objectType,
					reaction: func(action fakeclient.Action) (handled bool, ret runtime.Object, err error) {
						lockOps = append(lockOps, "create")
						lockObj = action.(fakeclient.CreateAction).GetObject()
						return true, lockObj, nil
					},
				},
				{
					verb:       "update",
					objectType: objectType,
					reaction: func(action fakeclient.Action) (handled bool, ret runtime.Object, err error) {
						updates++
						lockOps = append(lockOps, "update")
						if updates == 2 {
							cancelFunc()
						}
						lockObj = action.(fakeclient.UpdateAction).GetObject()
						return true, lockObj, nil
					},
				},
			},
			expectedLockOps: []string{"get", "create", "update", "update"},
		},
		{
			name: "Fallback to slow path after fast path fails",
			reactors: []Reactor{
				{
					verb:       "get",
					objectType: objectType,
					reaction: func(action fakeclient.Action) (handled bool, ret runtime.Object, err error) {
						lockOps = append(lockOps, "get")
						if lockObj != nil {
							return true, lockObj, nil
						}
						return true, nil, errors.NewNotFound(action.(fakeclient.GetAction).GetResource().GroupResource(), action.(fakeclient.GetAction).GetName())
					},
				},
				{
					verb:       "create",
					objectType: objectType,
					reaction: func(action fakeclient.Action) (handled bool, ret runtime.Object, err error) {
						lockOps = append(lockOps, "create")
						lockObj = action.(fakeclient.CreateAction).GetObject()
						return true, lockObj, nil
					},
				},
				{
					verb:       "update",
					objectType: objectType,
					reaction: func(action fakeclient.Action) (handled bool, ret runtime.Object, err error) {
						updates++
						lockOps = append(lockOps, "update")
						switch updates {
						case 2:
							return true, nil, errors.NewConflict(action.(fakeclient.UpdateAction).GetResource().GroupResource(), "fake conflict", nil)
						case 4:
							cancelFunc()
						}
						lockObj = action.(fakeclient.UpdateAction).GetObject()
						return true, lockObj, nil
					},
				},
			},
			expectedLockOps: []string{"get", "create", "update", "update", "get", "update", "update"},
		},
	}

	for i := range tests {
		test := &tests[i]
		t.Run(test.name, func(t *testing.T) {
			_, ctx := ktesting.NewTestContext(t)

			resetVars()

			recorder := record.NewFakeRecorder(100)
			resourceLockConfig := rl.ResourceLockConfig{
				Identity:      "baz",
				EventRecorder: recorder,
			}
			c := &fake.Clientset{}
			for _, reactor := range test.reactors {
				c.AddReactor(reactor.verb, objectType, reactor.reaction)
			}
			c.AddReactor("*", "*", func(action fakeclient.Action) (bool, runtime.Object, error) {
				t.Errorf("unreachable action. testclient called too many times: %+v", action)
				return true, nil, fmt.Errorf("unreachable action")
			})
			lock, err := rl.New("leases", "foo", "bar", c.CoreV1(), c.CoordinationV1(), resourceLockConfig)
			if err != nil {
				t.Fatal("resourcelock.New() = ", err)
			}

			lec.Lock = lock
			elector, err := NewLeaderElector(lec)
			if err != nil {
				t.Fatal("Failed to create leader elector: ", err)
			}

			ctx, cancel := context.WithCancel(ctx)
			cancelFunc = cancel

			elector.Run(ctx)
			assert.Equal(t, test.expectedLockOps, lockOps, "Expected lock ops %q, got %q", test.expectedLockOps, lockOps)
		})
	}
}
