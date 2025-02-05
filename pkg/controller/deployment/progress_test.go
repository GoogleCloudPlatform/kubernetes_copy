/*
Copyright 2017 The Kubernetes Authors.

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

package deployment

import (
	"context"
	"math"
	"testing"
	"time"

	apps "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2/ktesting"
	"k8s.io/kubernetes/pkg/controller/deployment/util"
)

func newDeploymentStatus(replicas, updatedReplicas, availableReplicas int32) apps.DeploymentStatus {
	return apps.DeploymentStatus{
		Replicas:          replicas,
		UpdatedReplicas:   updatedReplicas,
		AvailableReplicas: availableReplicas,
	}
}

// assumes the returned deployment is always observed - not needed to be tested here.
func currentDeployment(pds *int32, replicas, statusReplicas, updatedReplicas, availableReplicas int32, conditions []apps.DeploymentCondition) *apps.Deployment {
	d := &apps.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name: "progress-test",
		},
		Spec: apps.DeploymentSpec{
			ProgressDeadlineSeconds: pds,
			Replicas:                &replicas,
			Strategy: apps.DeploymentStrategy{
				Type: apps.RecreateDeploymentStrategyType,
			},
		},
		Status: newDeploymentStatus(statusReplicas, updatedReplicas, availableReplicas),
	}
	d.Status.Conditions = conditions
	return d
}

// helper to create RS with given availableReplicas
func newRSWithAvailable(name string, specReplicas, statusReplicas, availableReplicas int32) *apps.ReplicaSet {
	rs := rs(name, specReplicas, nil, metav1.Time{})
	rs.Status = apps.ReplicaSetStatus{
		Replicas:          statusReplicas,
		AvailableReplicas: availableReplicas,
	}
	return rs
}

func TestRequeueStuckDeployment(t *testing.T) {
	pds := int32(60)
	infinite := int32(math.MaxInt32)
	failed := []apps.DeploymentCondition{
		{
			Type:   apps.DeploymentProgressing,
			Status: v1.ConditionFalse,
			Reason: util.TimedOutReason,
		},
	}
	stuck := []apps.DeploymentCondition{
		{
			Type:           apps.DeploymentProgressing,
			Status:         v1.ConditionTrue,
			LastUpdateTime: metav1.Date(2017, 2, 15, 18, 49, 00, 00, time.UTC),
		},
	}

	tests := []struct {
		name     string
		d        *apps.Deployment
		status   apps.DeploymentStatus
		nowFn    func() time.Time
		expected time.Duration
	}{
		{
			name:     "nil progressDeadlineSeconds specified",
			d:        currentDeployment(nil, 4, 3, 3, 2, nil),
			status:   newDeploymentStatus(3, 3, 2),
			expected: time.Duration(-1),
		},
		{
			name:     "infinite progressDeadlineSeconds specified",
			d:        currentDeployment(&infinite, 4, 3, 3, 2, nil),
			status:   newDeploymentStatus(3, 3, 2),
			expected: time.Duration(-1),
		},
		{
			name:     "no progressing condition found",
			d:        currentDeployment(&pds, 4, 3, 3, 2, nil),
			status:   newDeploymentStatus(3, 3, 2),
			expected: time.Duration(-1),
		},
		{
			name:     "complete deployment does not need to be requeued",
			d:        currentDeployment(&pds, 3, 3, 3, 3, nil),
			status:   newDeploymentStatus(3, 3, 3),
			expected: time.Duration(-1),
		},
		{
			name:     "already failed deployment does not need to be requeued",
			d:        currentDeployment(&pds, 3, 3, 3, 0, failed),
			status:   newDeploymentStatus(3, 3, 0),
			expected: time.Duration(-1),
		},
		{
			name:     "stuck deployment - 30s",
			d:        currentDeployment(&pds, 3, 3, 3, 1, stuck),
			status:   newDeploymentStatus(3, 3, 1),
			nowFn:    func() time.Time { return metav1.Date(2017, 2, 15, 18, 49, 30, 00, time.UTC).Time },
			expected: 30 * time.Second,
		},
		{
			name:     "stuck deployment - 1s",
			d:        currentDeployment(&pds, 3, 3, 3, 1, stuck),
			status:   newDeploymentStatus(3, 3, 1),
			nowFn:    func() time.Time { return metav1.Date(2017, 2, 15, 18, 49, 59, 00, time.UTC).Time },
			expected: 1 * time.Second,
		},
		{
			name:     "failed deployment - less than a second => now",
			d:        currentDeployment(&pds, 3, 3, 3, 1, stuck),
			status:   newDeploymentStatus(3, 3, 1),
			nowFn:    func() time.Time { return metav1.Date(2017, 2, 15, 18, 49, 59, 1, time.UTC).Time },
			expected: time.Duration(0),
		},
		{
			name:     "failed deployment - now",
			d:        currentDeployment(&pds, 3, 3, 3, 1, stuck),
			status:   newDeploymentStatus(3, 3, 1),
			nowFn:    func() time.Time { return metav1.Date(2017, 2, 15, 18, 50, 00, 00, time.UTC).Time },
			expected: time.Duration(0),
		},
		{
			name:     "failed deployment - 1s after deadline",
			d:        currentDeployment(&pds, 3, 3, 3, 1, stuck),
			status:   newDeploymentStatus(3, 3, 1),
			nowFn:    func() time.Time { return metav1.Date(2017, 2, 15, 18, 50, 01, 00, time.UTC).Time },
			expected: time.Duration(0),
		},
		{
			name:     "failed deployment - 60s after deadline",
			d:        currentDeployment(&pds, 3, 3, 3, 1, stuck),
			status:   newDeploymentStatus(3, 3, 1),
			nowFn:    func() time.Time { return metav1.Date(2017, 2, 15, 18, 51, 00, 00, time.UTC).Time },
			expected: time.Duration(0),
		},
	}

	dc := &DeploymentController{
		queue: workqueue.NewTypedRateLimitingQueueWithConfig(
			workqueue.DefaultTypedControllerRateLimiter[string](),
			workqueue.TypedRateLimitingQueueConfig[string]{
				Name: "doesnt_matter",
			},
		),
	}
	dc.enqueueDeployment = dc.enqueue

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.nowFn != nil {
				nowFn = test.nowFn
			}
			_, ctx := ktesting.NewTestContext(t)
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			got := dc.requeueStuckDeployment(ctx, test.d, test.status)
			if got != test.expected {
				t.Errorf("%s: got duration: %v, expected duration: %v", test.name, got, test.expected)
			}
		})
	}
}

func TestSyncRolloutStatus(t *testing.T) {
	pds := int32(60)
	testTime := metav1.Date(2017, 2, 15, 18, 49, 00, 00, time.UTC)
	failedTimedOut := apps.DeploymentCondition{
		Type:   apps.DeploymentProgressing,
		Status: v1.ConditionFalse,
		Reason: util.TimedOutReason,
	}
	newRSAvailable := apps.DeploymentCondition{
		Type:               apps.DeploymentProgressing,
		Status:             v1.ConditionTrue,
		Reason:             util.NewRSAvailableReason,
		LastUpdateTime:     testTime,
		LastTransitionTime: testTime,
	}
	replicaSetUpdated := apps.DeploymentCondition{
		Type:               apps.DeploymentProgressing,
		Status:             v1.ConditionTrue,
		Reason:             util.ReplicaSetUpdatedReason,
		LastUpdateTime:     testTime,
		LastTransitionTime: testTime,
	}

	tests := []struct {
		name            string
		d               *apps.Deployment
		allRSs          []*apps.ReplicaSet
		newRS           *apps.ReplicaSet
		conditionType   apps.DeploymentConditionType
		conditionStatus v1.ConditionStatus
		conditionReason string
		lastUpdate      metav1.Time
		lastTransition  metav1.Time
	}{
		{
			name:   "General: remove Progressing condition and do not estimate progress if deployment has no Progress Deadline",
			d:      currentDeployment(nil, 3, 2, 2, 2, []apps.DeploymentCondition{replicaSetUpdated}),
			allRSs: []*apps.ReplicaSet{newRSWithAvailable("bar", 0, 1, 1)},
			newRS:  newRSWithAvailable("foo", 3, 2, 2),
		},
		{
			name:            "General: do not estimate progress of deployment with only one active ReplicaSet",
			d:               currentDeployment(&pds, 3, 3, 3, 3, []apps.DeploymentCondition{newRSAvailable}),
			allRSs:          []*apps.ReplicaSet{newRSWithAvailable("bar", 3, 3, 3)},
			conditionType:   apps.DeploymentProgressing,
			conditionStatus: v1.ConditionTrue,
			conditionReason: util.NewRSAvailableReason,
			lastUpdate:      testTime,
			lastTransition:  testTime,
		},
		{
			name:            "DeploymentProgressing: dont update lastTransitionTime if deployment already has Progressing=True",
			d:               currentDeployment(&pds, 3, 2, 2, 2, []apps.DeploymentCondition{replicaSetUpdated}),
			allRSs:          []*apps.ReplicaSet{newRSWithAvailable("bar", 0, 1, 1)},
			newRS:           newRSWithAvailable("foo", 3, 2, 2),
			conditionType:   apps.DeploymentProgressing,
			conditionStatus: v1.ConditionTrue,
			conditionReason: util.ReplicaSetUpdatedReason,
			lastTransition:  testTime,
		},
		{
			name:            "DeploymentProgressing: update everything if deployment has Progressing=False",
			d:               currentDeployment(&pds, 3, 2, 2, 2, []apps.DeploymentCondition{failedTimedOut}),
			allRSs:          []*apps.ReplicaSet{newRSWithAvailable("bar", 0, 1, 1)},
			newRS:           newRSWithAvailable("foo", 3, 2, 2),
			conditionType:   apps.DeploymentProgressing,
			conditionStatus: v1.ConditionTrue,
			conditionReason: util.ReplicaSetUpdatedReason,
		},
		{
			name:            "DeploymentProgressing: create Progressing condition if it does not exist",
			d:               currentDeployment(&pds, 3, 2, 2, 2, []apps.DeploymentCondition{}),
			allRSs:          []*apps.ReplicaSet{newRSWithAvailable("bar", 0, 1, 1)},
			newRS:           newRSWithAvailable("foo", 3, 2, 2),
			conditionType:   apps.DeploymentProgressing,
			conditionStatus: v1.ConditionTrue,
			conditionReason: util.ReplicaSetUpdatedReason,
		},
		{
			name:            "DeploymentComplete: dont update lastTransitionTime if deployment already has Progressing=True",
			d:               currentDeployment(&pds, 3, 3, 3, 3, []apps.DeploymentCondition{replicaSetUpdated}),
			allRSs:          []*apps.ReplicaSet{},
			newRS:           newRSWithAvailable("foo", 3, 3, 3),
			conditionType:   apps.DeploymentProgressing,
			conditionStatus: v1.ConditionTrue,
			conditionReason: util.NewRSAvailableReason,
			lastTransition:  testTime,
		},
		{
			name:            "DeploymentComplete: update everything if deployment has Progressing=False",
			d:               currentDeployment(&pds, 3, 3, 3, 3, []apps.DeploymentCondition{failedTimedOut}),
			allRSs:          []*apps.ReplicaSet{},
			newRS:           newRSWithAvailable("foo", 3, 3, 3),
			conditionType:   apps.DeploymentProgressing,
			conditionStatus: v1.ConditionTrue,
			conditionReason: util.NewRSAvailableReason,
		},
		{
			name:            "DeploymentComplete: create Progressing condition if it does not exist",
			d:               currentDeployment(&pds, 3, 3, 3, 3, []apps.DeploymentCondition{}),
			allRSs:          []*apps.ReplicaSet{},
			newRS:           newRSWithAvailable("foo", 3, 3, 3),
			conditionType:   apps.DeploymentProgressing,
			conditionStatus: v1.ConditionTrue,
			conditionReason: util.NewRSAvailableReason,
		},
		{
			name:            "DeploymentComplete: defend against NPE when newRS=nil",
			d:               currentDeployment(&pds, 0, 3, 3, 3, []apps.DeploymentCondition{replicaSetUpdated}),
			allRSs:          []*apps.ReplicaSet{newRSWithAvailable("foo", 0, 0, 0)},
			conditionType:   apps.DeploymentProgressing,
			conditionStatus: v1.ConditionTrue,
			conditionReason: util.NewRSAvailableReason,
		},
		{
			name:            "DeploymentTimedOut: update status if rollout exceeds Progress Deadline",
			d:               currentDeployment(&pds, 3, 2, 2, 2, []apps.DeploymentCondition{replicaSetUpdated}),
			allRSs:          []*apps.ReplicaSet{},
			newRS:           newRSWithAvailable("foo", 3, 2, 2),
			conditionType:   apps.DeploymentProgressing,
			conditionStatus: v1.ConditionFalse,
			conditionReason: util.TimedOutReason,
		},
		{
			name:            "DeploymentTimedOut: do not update status if deployment has existing timedOut condition",
			d:               currentDeployment(&pds, 3, 2, 2, 2, []apps.DeploymentCondition{failedTimedOut}),
			allRSs:          []*apps.ReplicaSet{},
			newRS:           newRSWithAvailable("foo", 3, 2, 2),
			conditionType:   apps.DeploymentProgressing,
			conditionStatus: v1.ConditionFalse,
			conditionReason: util.TimedOutReason,
			lastUpdate:      testTime,
			lastTransition:  testTime,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := fake.Clientset{}
			dc := &DeploymentController{
				client: &fake,
			}

			if test.newRS != nil {
				test.allRSs = append(test.allRSs, test.newRS)
			}
			_, ctx := ktesting.NewTestContext(t)
			err := dc.syncRolloutStatus(ctx, test.allRSs, test.newRS, test.d)
			if err != nil {
				t.Error(err)
			}

			newCond := util.GetDeploymentCondition(test.d.Status, test.conditionType)
			switch {
			case newCond == nil:
				if test.d.Spec.ProgressDeadlineSeconds != nil && *test.d.Spec.ProgressDeadlineSeconds != math.MaxInt32 {
					t.Errorf("%s: expected deployment condition: %s", test.name, test.conditionType)
				}
			case newCond.Status != test.conditionStatus || newCond.Reason != test.conditionReason:
				t.Errorf("%s: DeploymentProgressing has status %s with reason %s. Expected %s with %s.", test.name, newCond.Status, newCond.Reason, test.conditionStatus, test.conditionReason)
			case !test.lastUpdate.IsZero() && test.lastUpdate != testTime:
				t.Errorf("%s: LastUpdateTime was changed to %s but expected %s;", test.name, test.lastUpdate, testTime)
			case !test.lastTransition.IsZero() && test.lastTransition != testTime:
				t.Errorf("%s: LastTransitionTime was changed to %s but expected %s;", test.name, test.lastTransition, testTime)
			}
		})
	}
}
