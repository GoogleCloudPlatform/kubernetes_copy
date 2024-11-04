/*
Copyright 2016 The Kubernetes Authors.

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
	"testing"

	apps "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/kubernetes/fake"
	core "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/record"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	"k8s.io/klog/v2/ktesting"
	"k8s.io/kubernetes/pkg/features"
	"k8s.io/utils/ptr"
)

func TestDeploymentController_reconcileNewReplicaSet(t *testing.T) {
	tests := []struct {
		name                                 string
		enableDeploymentPodReplacementPolicy bool

		deploymentReplicas     int32
		maxSurge               intstr.IntOrString
		podReplacementPolicy   *apps.DeploymentPodReplacementPolicy
		oldReplicas            int32
		newReplicas            int32
		oldTerminatingReplicas *int32
		newTerminatingReplicas *int32
		oldStatusReplicas      int32
		newStatusReplicas      int32
		isNewRSUnsynced        bool
		scaleExpected          bool
		expectedNewReplicas    int32
		expectedScaleErr       bool
	}{
		{
			name:               "should not scale up when old replica set is fully saturated",
			deploymentReplicas: 10,
			maxSurge:           intstr.FromInt32(0),
			oldReplicas:        10,
			newReplicas:        0,
			scaleExpected:      false,
		},
		{
			name:                "should scale up according to the maxSurge when old replica set is fully saturated",
			deploymentReplicas:  10,
			maxSurge:            intstr.FromInt32(2),
			oldReplicas:         10,
			newReplicas:         0,
			scaleExpected:       true,
			expectedNewReplicas: 2,
		},
		{
			name:                "should scale up according to the maxSurge",
			deploymentReplicas:  10,
			maxSurge:            intstr.FromInt32(2),
			oldReplicas:         5,
			newReplicas:         0,
			scaleExpected:       true,
			expectedNewReplicas: 7,
		},
		{
			name:               "should not scale when maxSurge is fully saturated",
			deploymentReplicas: 10,
			maxSurge:           intstr.FromInt32(2),
			oldReplicas:        10,
			newReplicas:        2,
			scaleExpected:      false,
		},
		{
			name:                "should scale down when there are replicas above maxSurge",
			deploymentReplicas:  10,
			maxSurge:            intstr.FromInt32(2),
			oldReplicas:         2,
			newReplicas:         11,
			scaleExpected:       true,
			expectedNewReplicas: 10,
		},
		{
			name:                                 "should scale up according to the maxSurge with empty policy and ignore terminating pods",
			enableDeploymentPodReplacementPolicy: true,
			deploymentReplicas:                   10,
			maxSurge:                             intstr.FromInt32(2),
			oldReplicas:                          5,
			newReplicas:                          0,
			oldTerminatingReplicas:               ptr.To[int32](1),
			newTerminatingReplicas:               ptr.To[int32](10),
			scaleExpected:                        true,
			expectedNewReplicas:                  7,
		},
		{
			name:                                 "should scale up according to the maxSurge with TerminationStarted policy and ignore terminating pods",
			enableDeploymentPodReplacementPolicy: true,
			deploymentReplicas:                   10,
			maxSurge:                             intstr.FromInt32(2),
			podReplacementPolicy:                 ptr.To(apps.TerminationStarted),
			oldReplicas:                          5,
			newReplicas:                          0,
			oldTerminatingReplicas:               ptr.To[int32](1),
			newTerminatingReplicas:               ptr.To[int32](10),
			scaleExpected:                        true,
			expectedNewReplicas:                  7,
		},
		{
			name:                                 "should scale up according to the terminating pods with TerminationComplete policy",
			enableDeploymentPodReplacementPolicy: true,
			deploymentReplicas:                   10,
			maxSurge:                             intstr.FromInt32(0),
			podReplacementPolicy:                 ptr.To(apps.TerminationComplete),
			oldReplicas:                          5,
			newReplicas:                          1,
			oldTerminatingReplicas:               ptr.To[int32](0),
			newTerminatingReplicas:               ptr.To[int32](1),
			scaleExpected:                        true,
			expectedNewReplicas:                  4,
		},
		{
			name:                                 "should scale up according to the terminating pods with TerminationComplete policy and unsynced new RS",
			enableDeploymentPodReplacementPolicy: true,
			deploymentReplicas:                   10,
			maxSurge:                             intstr.FromInt32(0),
			podReplacementPolicy:                 ptr.To(apps.TerminationComplete),
			oldReplicas:                          5,
			newReplicas:                          1,
			oldTerminatingReplicas:               ptr.To[int32](1),
			newTerminatingReplicas:               nil,
			isNewRSUnsynced:                      true,
			scaleExpected:                        true,
			expectedNewReplicas:                  4,
		},
		{
			name:                                 "should error when scaling up according to the terminating pods with TerminationComplete policy and broken new RS (unknown terminatingReplicas)",
			enableDeploymentPodReplacementPolicy: true,
			deploymentReplicas:                   10,
			maxSurge:                             intstr.FromInt32(0),
			podReplacementPolicy:                 ptr.To(apps.TerminationComplete),
			oldReplicas:                          5,
			newReplicas:                          1,
			oldTerminatingReplicas:               ptr.To[int32](1),
			// This should not happen during a normal operation (functioning replica set controller and no 3rd party status tampering).
			newTerminatingReplicas: nil,
			expectedScaleErr:       true,
		},
		{
			name:                                 "should scale up according to the status pods with TerminationComplete policy",
			enableDeploymentPodReplacementPolicy: true,
			deploymentReplicas:                   10,
			maxSurge:                             intstr.FromInt32(0),
			podReplacementPolicy:                 ptr.To(apps.TerminationComplete),
			oldReplicas:                          5,
			newReplicas:                          1,
			oldStatusReplicas:                    6,
			newStatusReplicas:                    1,
			oldTerminatingReplicas:               ptr.To[int32](0),
			newTerminatingReplicas:               ptr.To[int32](0),
			scaleExpected:                        true,
			expectedNewReplicas:                  4,
		},
		{
			name:                                 "should scale up according to the maxSurge and terminating and status pods with TerminationComplete policy",
			enableDeploymentPodReplacementPolicy: true,
			deploymentReplicas:                   10,
			maxSurge:                             intstr.FromInt32(2),
			podReplacementPolicy:                 ptr.To(apps.TerminationComplete),
			oldReplicas:                          5,
			newReplicas:                          0,
			oldTerminatingReplicas:               ptr.To[int32](1),
			newTerminatingReplicas:               ptr.To[int32](2),
			newStatusReplicas:                    1, // this is expected to terminate
			scaleExpected:                        true,
			expectedNewReplicas:                  3,
		},
		{
			name:                                 "should not scale up according to the maxSurge due to terminating pods with TerminationComplete policy",
			enableDeploymentPodReplacementPolicy: true,
			deploymentReplicas:                   10,
			maxSurge:                             intstr.FromInt32(2),
			podReplacementPolicy:                 ptr.To(apps.TerminationComplete),
			oldReplicas:                          5,
			newReplicas:                          0,
			oldTerminatingReplicas:               ptr.To[int32](1),
			newTerminatingReplicas:               ptr.To[int32](10),
			scaleExpected:                        false,
			expectedNewReplicas:                  0,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.DeploymentPodReplacementPolicy, test.enableDeploymentPodReplacementPolicy)
			_, ctx := ktesting.NewTestContext(t)
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()

			newRS := newRSWithFullStatus("foo-v2", test.newReplicas, apps.ReplicaSetStatus{
				Replicas:            test.newStatusReplicas,
				TerminatingReplicas: test.newTerminatingReplicas,
			}, nil, nil, noTimestamp)
			if !test.isNewRSUnsynced {
				newRS.Status.ObservedGeneration = 1
			}
			oldRS := newRSWithFullStatus("foo-v1", test.oldReplicas, apps.ReplicaSetStatus{
				ObservedGeneration:  1,
				Replicas:            test.oldStatusReplicas,
				TerminatingReplicas: test.oldTerminatingReplicas,
			}, nil, nil, noTimestamp)

			allRSs := []*apps.ReplicaSet{newRS, oldRS}
			maxUnavailable := intstr.FromInt32(0)
			deployment := newDeployment("foo", test.deploymentReplicas, nil, &test.maxSurge, &maxUnavailable, map[string]string{"foo": "bar"})
			deployment.Spec.PodReplacementPolicy = test.podReplacementPolicy
			fake := fake.Clientset{}
			controller := &DeploymentController{
				client:        &fake,
				eventRecorder: &record.FakeRecorder{},
			}
			scaled, err := controller.reconcileNewReplicaSet(ctx, allRSs, newRS, deployment)
			if err != nil {
				if !test.expectedScaleErr {
					t.Errorf("unexpected error: %v", err)
				}
				return
			}
			if !test.scaleExpected {
				if scaled || len(fake.Actions()) > 0 {
					t.Errorf("unexpected scaling: %v", fake.Actions())
				}
				return
			}
			if test.scaleExpected && !scaled {
				t.Errorf("expected scaling to occur")
				return
			}
			if len(fake.Actions()) != 1 {
				t.Errorf("expected 1 action during scale, got: %v", fake.Actions())
				return
			}
			updated := fake.Actions()[0].(core.UpdateAction).GetObject().(*apps.ReplicaSet)
			if e, a := test.expectedNewReplicas, *(updated.Spec.Replicas); e != a {
				t.Errorf("expected update to %d replicas, got %d", e, a)
			}
		})
	}
}

func TestDeploymentController_reconcileOldReplicaSets(t *testing.T) {
	tests := []struct {
		deploymentReplicas  int32
		maxUnavailable      intstr.IntOrString
		oldReplicas         int32
		newReplicas         int32
		readyPodsFromOldRS  int
		readyPodsFromNewRS  int
		scaleExpected       bool
		expectedOldReplicas int32
	}{
		{
			deploymentReplicas:  10,
			maxUnavailable:      intstr.FromInt32(0),
			oldReplicas:         10,
			newReplicas:         0,
			readyPodsFromOldRS:  10,
			readyPodsFromNewRS:  0,
			scaleExpected:       true,
			expectedOldReplicas: 9,
		},
		{
			deploymentReplicas:  10,
			maxUnavailable:      intstr.FromInt32(2),
			oldReplicas:         10,
			newReplicas:         0,
			readyPodsFromOldRS:  10,
			readyPodsFromNewRS:  0,
			scaleExpected:       true,
			expectedOldReplicas: 8,
		},
		{ // expect unhealthy replicas from old replica sets been cleaned up
			deploymentReplicas:  10,
			maxUnavailable:      intstr.FromInt32(2),
			oldReplicas:         10,
			newReplicas:         0,
			readyPodsFromOldRS:  8,
			readyPodsFromNewRS:  0,
			scaleExpected:       true,
			expectedOldReplicas: 8,
		},
		{ // expect 1 unhealthy replica from old replica sets been cleaned up, and 1 ready pod been scaled down
			deploymentReplicas:  10,
			maxUnavailable:      intstr.FromInt32(2),
			oldReplicas:         10,
			newReplicas:         0,
			readyPodsFromOldRS:  9,
			readyPodsFromNewRS:  0,
			scaleExpected:       true,
			expectedOldReplicas: 8,
		},
		{ // the unavailable pods from the newRS would not make us scale down old RSs in a further step
			deploymentReplicas: 10,
			maxUnavailable:     intstr.FromInt32(2),
			oldReplicas:        8,
			newReplicas:        2,
			readyPodsFromOldRS: 8,
			readyPodsFromNewRS: 0,
			scaleExpected:      false,
		},
	}
	for i := range tests {
		test := tests[i]
		t.Logf("executing scenario %d", i)

		newSelector := map[string]string{"foo": "new"}
		oldSelector := map[string]string{"foo": "old"}
		newRS := rs("foo-new", test.newReplicas, newSelector, noTimestamp)
		newRS.Status.AvailableReplicas = int32(test.readyPodsFromNewRS)
		oldRS := rs("foo-old", test.oldReplicas, oldSelector, noTimestamp)
		oldRS.Status.AvailableReplicas = int32(test.readyPodsFromOldRS)
		oldRSs := []*apps.ReplicaSet{oldRS}
		allRSs := []*apps.ReplicaSet{oldRS, newRS}
		maxSurge := intstr.FromInt32(0)
		deployment := newDeployment("foo", test.deploymentReplicas, nil, &maxSurge, &test.maxUnavailable, newSelector)
		fakeClientset := fake.Clientset{}
		controller := &DeploymentController{
			client:        &fakeClientset,
			eventRecorder: &record.FakeRecorder{},
		}
		_, ctx := ktesting.NewTestContext(t)
		scaled, err := controller.reconcileOldReplicaSets(ctx, allRSs, oldRSs, newRS, deployment)
		if err != nil {
			t.Errorf("unexpected error: %v", err)
			continue
		}
		if !test.scaleExpected && scaled {
			t.Errorf("unexpected scaling: %v", fakeClientset.Actions())
		}
		if test.scaleExpected && !scaled {
			t.Errorf("expected scaling to occur")
			continue
		}
		continue
	}
}

func TestDeploymentController_cleanupUnhealthyReplicas(t *testing.T) {
	tests := []struct {
		oldReplicas          int32
		readyPods            int
		unHealthyPods        int
		maxCleanupCount      int
		cleanupCountExpected int
	}{
		{
			oldReplicas:          10,
			readyPods:            8,
			unHealthyPods:        2,
			maxCleanupCount:      1,
			cleanupCountExpected: 1,
		},
		{
			oldReplicas:          10,
			readyPods:            8,
			unHealthyPods:        2,
			maxCleanupCount:      3,
			cleanupCountExpected: 2,
		},
		{
			oldReplicas:          10,
			readyPods:            8,
			unHealthyPods:        2,
			maxCleanupCount:      0,
			cleanupCountExpected: 0,
		},
		{
			oldReplicas:          10,
			readyPods:            10,
			unHealthyPods:        0,
			maxCleanupCount:      3,
			cleanupCountExpected: 0,
		},
	}

	for i, test := range tests {
		t.Logf("executing scenario %d", i)
		oldRS := rs("foo-v2", test.oldReplicas, nil, noTimestamp)
		oldRS.Status.AvailableReplicas = int32(test.readyPods)
		oldRSs := []*apps.ReplicaSet{oldRS}
		maxSurge := intstr.FromInt32(2)
		maxUnavailable := intstr.FromInt32(2)
		deployment := newDeployment("foo", 10, nil, &maxSurge, &maxUnavailable, nil)
		fakeClientset := fake.Clientset{}

		controller := &DeploymentController{
			client:        &fakeClientset,
			eventRecorder: &record.FakeRecorder{},
		}
		_, ctx := ktesting.NewTestContext(t)
		_, cleanupCount, err := controller.cleanupUnhealthyReplicas(ctx, oldRSs, deployment, int32(test.maxCleanupCount))
		if err != nil {
			t.Errorf("unexpected error: %v", err)
			continue
		}
		if int(cleanupCount) != test.cleanupCountExpected {
			t.Errorf("expected %v unhealthy replicas been cleaned up, got %v", test.cleanupCountExpected, cleanupCount)
			continue
		}
	}
}

func TestDeploymentController_scaleDownOldReplicaSetsForRollingUpdate(t *testing.T) {
	tests := []struct {
		deploymentReplicas  int32
		maxUnavailable      intstr.IntOrString
		readyPods           int
		oldReplicas         int32
		scaleExpected       bool
		expectedOldReplicas int32
	}{
		{
			deploymentReplicas:  10,
			maxUnavailable:      intstr.FromInt32(0),
			readyPods:           10,
			oldReplicas:         10,
			scaleExpected:       true,
			expectedOldReplicas: 9,
		},
		{
			deploymentReplicas:  10,
			maxUnavailable:      intstr.FromInt32(2),
			readyPods:           10,
			oldReplicas:         10,
			scaleExpected:       true,
			expectedOldReplicas: 8,
		},
		{
			deploymentReplicas: 10,
			maxUnavailable:     intstr.FromInt32(2),
			readyPods:          8,
			oldReplicas:        10,
			scaleExpected:      false,
		},
		{
			deploymentReplicas: 10,
			maxUnavailable:     intstr.FromInt32(2),
			readyPods:          10,
			oldReplicas:        0,
			scaleExpected:      false,
		},
		{
			deploymentReplicas: 10,
			maxUnavailable:     intstr.FromInt32(2),
			readyPods:          1,
			oldReplicas:        10,
			scaleExpected:      false,
		},
	}

	for i := range tests {
		test := tests[i]
		t.Logf("executing scenario %d", i)
		oldRS := rs("foo-v2", test.oldReplicas, nil, noTimestamp)
		oldRS.Status.AvailableReplicas = int32(test.readyPods)
		allRSs := []*apps.ReplicaSet{oldRS}
		oldRSs := []*apps.ReplicaSet{oldRS}
		maxSurge := intstr.FromInt32(0)
		deployment := newDeployment("foo", test.deploymentReplicas, nil, &maxSurge, &test.maxUnavailable, map[string]string{"foo": "bar"})
		fakeClientset := fake.Clientset{}
		controller := &DeploymentController{
			client:        &fakeClientset,
			eventRecorder: &record.FakeRecorder{},
		}
		_, ctx := ktesting.NewTestContext(t)
		scaled, err := controller.scaleDownOldReplicaSetsForRollingUpdate(ctx, allRSs, oldRSs, deployment)
		if err != nil {
			t.Errorf("unexpected error: %v", err)
			continue
		}
		if !test.scaleExpected {
			if scaled != 0 {
				t.Errorf("unexpected scaling: %v", fakeClientset.Actions())
			}
			continue
		}
		if test.scaleExpected && scaled == 0 {
			t.Errorf("expected scaling to occur; actions: %v", fakeClientset.Actions())
			continue
		}
		// There are both list and update actions logged, so extract the update
		// action for verification.
		var updateAction core.UpdateAction
		for _, action := range fakeClientset.Actions() {
			switch a := action.(type) {
			case core.UpdateAction:
				if updateAction != nil {
					t.Errorf("expected only 1 update action; had %v and found %v", updateAction, a)
				} else {
					updateAction = a
				}
			}
		}
		if updateAction == nil {
			t.Errorf("expected an update action")
			continue
		}
		updated := updateAction.GetObject().(*apps.ReplicaSet)
		if e, a := test.expectedOldReplicas, *(updated.Spec.Replicas); e != a {
			t.Errorf("expected update to %d replicas, got %d", e, a)
		}
	}
}
