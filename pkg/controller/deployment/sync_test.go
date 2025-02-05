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
	"fmt"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	apps "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/sets"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	testclient "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/record"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	"k8s.io/klog/v2/ktesting"
	"k8s.io/kubernetes/pkg/controller"
	deploymentutil "k8s.io/kubernetes/pkg/controller/deployment/util"
	"k8s.io/kubernetes/pkg/features"
	"k8s.io/utils/ptr"
)

func TestScale(t *testing.T) {
	newTimestamp := metav1.Date(2016, 5, 20, 2, 0, 0, 0, time.UTC)
	oldTimestamp := metav1.Date(2016, 5, 20, 1, 0, 0, 0, time.UTC)
	olderTimestamp := metav1.Date(2016, 5, 20, 0, 0, 0, 0, time.UTC)

	var updatedTemplate = func(replicas int32) *apps.Deployment {
		d := newDeployment("foo", replicas, nil, nil, nil, map[string]string{"foo": "bar"})
		d.Spec.Template.Labels["another"] = "label"
		return d
	}

	tests := []struct {
		name                                 string
		enableDeploymentPodReplacementPolicy bool

		deployment           *apps.Deployment
		oldDeployment        *apps.Deployment
		podReplacementPolicy *apps.DeploymentPodReplacementPolicy

		newRS  *apps.ReplicaSet
		oldRSs []*apps.ReplicaSet

		expectedNew     *apps.ReplicaSet
		expectedOld     []*apps.ReplicaSet
		expectedSyncErr bool
		wasntUpdated    map[string]bool

		desiredReplicasAnnotations map[string]int32
	}{
		{
			name:          "normal scaling event: 10 -> 12",
			deployment:    newDeployment("foo", 12, nil, nil, nil, nil),
			oldDeployment: newDeployment("foo", 10, nil, nil, nil, nil),

			newRS:  rs("foo-v1", 10, nil, newTimestamp),
			oldRSs: []*apps.ReplicaSet{},

			expectedNew: rs("foo-v1", 12, nil, newTimestamp),
			expectedOld: []*apps.ReplicaSet{},
		},
		{
			name:          "normal scaling event: 10 -> 5",
			deployment:    newDeployment("foo", 5, nil, nil, nil, nil),
			oldDeployment: newDeployment("foo", 10, nil, nil, nil, nil),

			newRS:  rs("foo-v1", 10, nil, newTimestamp),
			oldRSs: []*apps.ReplicaSet{},

			expectedNew: rs("foo-v1", 5, nil, newTimestamp),
			expectedOld: []*apps.ReplicaSet{},
		},
		{
			name:          "proportional scaling: 5 -> 10",
			deployment:    newDeployment("foo", 10, nil, nil, nil, nil),
			oldDeployment: newDeployment("foo", 5, nil, nil, nil, nil),

			newRS:  rs("foo-v2", 2, nil, newTimestamp),
			oldRSs: []*apps.ReplicaSet{rs("foo-v1", 3, nil, oldTimestamp)},

			expectedNew: rs("foo-v2", 4, nil, newTimestamp),
			expectedOld: []*apps.ReplicaSet{rs("foo-v1", 6, nil, oldTimestamp)},
		},
		{
			name:          "proportional scaling: 5 -> 3",
			deployment:    newDeployment("foo", 3, nil, nil, nil, nil),
			oldDeployment: newDeployment("foo", 5, nil, nil, nil, nil),

			newRS:  rs("foo-v2", 2, nil, newTimestamp),
			oldRSs: []*apps.ReplicaSet{rs("foo-v1", 3, nil, oldTimestamp)},

			expectedNew: rs("foo-v2", 1, nil, newTimestamp),
			expectedOld: []*apps.ReplicaSet{rs("foo-v1", 2, nil, oldTimestamp)},
		},
		{
			name:          "proportional scaling: 9 -> 4",
			deployment:    newDeployment("foo", 4, nil, nil, nil, nil),
			oldDeployment: newDeployment("foo", 9, nil, nil, nil, nil),

			newRS:  rs("foo-v2", 8, nil, newTimestamp),
			oldRSs: []*apps.ReplicaSet{rs("foo-v1", 1, nil, oldTimestamp)},

			expectedNew: rs("foo-v2", 4, nil, newTimestamp),
			expectedOld: []*apps.ReplicaSet{rs("foo-v1", 0, nil, oldTimestamp)},
		},
		{
			name:          "proportional scaling: 7 -> 10",
			deployment:    newDeployment("foo", 10, nil, nil, nil, nil),
			oldDeployment: newDeployment("foo", 7, nil, nil, nil, nil),

			newRS:  rs("foo-v3", 2, nil, newTimestamp),
			oldRSs: []*apps.ReplicaSet{rs("foo-v2", 3, nil, oldTimestamp), rs("foo-v1", 2, nil, olderTimestamp)},

			expectedNew: rs("foo-v3", 3, nil, newTimestamp),
			expectedOld: []*apps.ReplicaSet{rs("foo-v2", 4, nil, oldTimestamp), rs("foo-v1", 3, nil, olderTimestamp)},
		},
		{
			name:          "proportional scaling: 13 -> 8",
			deployment:    newDeployment("foo", 8, nil, nil, nil, nil),
			oldDeployment: newDeployment("foo", 13, nil, nil, nil, nil),

			newRS:  rs("foo-v3", 2, nil, newTimestamp),
			oldRSs: []*apps.ReplicaSet{rs("foo-v2", 8, nil, oldTimestamp), rs("foo-v1", 3, nil, olderTimestamp)},

			expectedNew: rs("foo-v3", 1, nil, newTimestamp),
			expectedOld: []*apps.ReplicaSet{rs("foo-v2", 5, nil, oldTimestamp), rs("foo-v1", 2, nil, olderTimestamp)},
		},
		// Scales up the new replica set.
		{
			name:          "leftover distribution: 3 -> 4",
			deployment:    newDeployment("foo", 4, nil, nil, nil, nil),
			oldDeployment: newDeployment("foo", 3, nil, nil, nil, nil),

			newRS:  rs("foo-v3", 1, nil, newTimestamp),
			oldRSs: []*apps.ReplicaSet{rs("foo-v2", 1, nil, oldTimestamp), rs("foo-v1", 1, nil, olderTimestamp)},

			expectedNew: rs("foo-v3", 2, nil, newTimestamp),
			expectedOld: []*apps.ReplicaSet{rs("foo-v2", 1, nil, oldTimestamp), rs("foo-v1", 1, nil, olderTimestamp)},
		},
		// Scales down the older replica set.
		{
			name:          "leftover distribution: 3 -> 2",
			deployment:    newDeployment("foo", 2, nil, nil, nil, nil),
			oldDeployment: newDeployment("foo", 3, nil, nil, nil, nil),

			newRS:  rs("foo-v3", 1, nil, newTimestamp),
			oldRSs: []*apps.ReplicaSet{rs("foo-v2", 1, nil, oldTimestamp), rs("foo-v1", 1, nil, olderTimestamp)},

			expectedNew: rs("foo-v3", 1, nil, newTimestamp),
			expectedOld: []*apps.ReplicaSet{rs("foo-v2", 1, nil, oldTimestamp), rs("foo-v1", 0, nil, olderTimestamp)},
		},
		// Scales up the latest replica set first.
		{
			name:          "proportional scaling (no new rs): 4 -> 5",
			deployment:    newDeployment("foo", 5, nil, nil, nil, nil),
			oldDeployment: newDeployment("foo", 4, nil, nil, nil, nil),

			newRS:  nil,
			oldRSs: []*apps.ReplicaSet{rs("foo-v2", 2, nil, oldTimestamp), rs("foo-v1", 2, nil, olderTimestamp)},

			expectedNew: nil,
			expectedOld: []*apps.ReplicaSet{rs("foo-v2", 3, nil, oldTimestamp), rs("foo-v1", 2, nil, olderTimestamp)},
		},
		// Scales down to zero
		{
			name:          "proportional scaling: 6 -> 0",
			deployment:    newDeployment("foo", 0, nil, nil, nil, nil),
			oldDeployment: newDeployment("foo", 6, nil, nil, nil, nil),

			newRS:  rs("foo-v3", 3, nil, newTimestamp),
			oldRSs: []*apps.ReplicaSet{rs("foo-v2", 2, nil, oldTimestamp), rs("foo-v1", 1, nil, olderTimestamp)},

			expectedNew: rs("foo-v3", 0, nil, newTimestamp),
			expectedOld: []*apps.ReplicaSet{rs("foo-v2", 0, nil, oldTimestamp), rs("foo-v1", 0, nil, olderTimestamp)},
		},
		// Scales up from zero
		{
			name:          "proportional scaling: 0 -> 6",
			deployment:    newDeployment("foo", 6, nil, nil, nil, nil),
			oldDeployment: newDeployment("foo", 6, nil, nil, nil, nil),

			newRS:  rs("foo-v3", 0, nil, newTimestamp),
			oldRSs: []*apps.ReplicaSet{rs("foo-v2", 0, nil, oldTimestamp), rs("foo-v1", 0, nil, olderTimestamp)},

			expectedNew:  rs("foo-v3", 6, nil, newTimestamp),
			expectedOld:  []*apps.ReplicaSet{rs("foo-v2", 0, nil, oldTimestamp), rs("foo-v1", 0, nil, olderTimestamp)},
			wasntUpdated: map[string]bool{"foo-v2": true, "foo-v1": true},
		},
		// Scenario: deployment.spec.replicas == 3 ( foo-v1.spec.replicas == foo-v2.spec.replicas == foo-v3.spec.replicas == 1 )
		// Deployment is scaled to 5. foo-v3.spec.replicas and foo-v2.spec.replicas should increment by 1 but foo-v2 fails to
		// update.
		{
			name:          "failed rs update",
			deployment:    newDeployment("foo", 5, nil, nil, nil, nil),
			oldDeployment: newDeployment("foo", 5, nil, nil, nil, nil),

			newRS:  rs("foo-v3", 2, nil, newTimestamp),
			oldRSs: []*apps.ReplicaSet{rs("foo-v2", 1, nil, oldTimestamp), rs("foo-v1", 1, nil, olderTimestamp)},

			expectedNew:  rs("foo-v3", 2, nil, newTimestamp),
			expectedOld:  []*apps.ReplicaSet{rs("foo-v2", 2, nil, oldTimestamp), rs("foo-v1", 1, nil, olderTimestamp)},
			wasntUpdated: map[string]bool{"foo-v3": true, "foo-v1": true},

			desiredReplicasAnnotations: map[string]int32{"foo-v2": int32(3)},
		},
		{
			name:          "deployment with surge pods",
			deployment:    newDeployment("foo", 20, nil, ptr.To(intstr.FromInt32(2)), nil, nil),
			oldDeployment: newDeployment("foo", 10, nil, ptr.To(intstr.FromInt32(2)), nil, nil),

			newRS:  rs("foo-v2", 6, nil, newTimestamp),
			oldRSs: []*apps.ReplicaSet{rs("foo-v1", 6, nil, oldTimestamp)},

			expectedNew: rs("foo-v2", 11, nil, newTimestamp),
			expectedOld: []*apps.ReplicaSet{rs("foo-v1", 11, nil, oldTimestamp)},
		},
		{
			name:          "change both surge and size",
			deployment:    newDeployment("foo", 50, nil, ptr.To(intstr.FromInt32(6)), nil, nil),
			oldDeployment: newDeployment("foo", 10, nil, ptr.To(intstr.FromInt32(3)), nil, nil),

			newRS:  rs("foo-v2", 5, nil, newTimestamp),
			oldRSs: []*apps.ReplicaSet{rs("foo-v1", 8, nil, oldTimestamp)},

			expectedNew: rs("foo-v2", 22, nil, newTimestamp),
			expectedOld: []*apps.ReplicaSet{rs("foo-v1", 34, nil, oldTimestamp)},
		},
		{
			name:          "change both size and template",
			deployment:    updatedTemplate(14),
			oldDeployment: newDeployment("foo", 10, nil, nil, nil, map[string]string{"foo": "bar"}),

			newRS:  nil,
			oldRSs: []*apps.ReplicaSet{rs("foo-v2", 7, nil, newTimestamp), rs("foo-v1", 3, nil, oldTimestamp)},

			expectedNew: nil,
			expectedOld: []*apps.ReplicaSet{rs("foo-v2", 10, nil, newTimestamp), rs("foo-v1", 4, nil, oldTimestamp)},
		},
		{
			name:          "saturated but broken new replica set does not affect old pods",
			deployment:    newDeployment("foo", 2, nil, ptr.To(intstr.FromInt32(1)), ptr.To(intstr.FromInt32(1)), nil),
			oldDeployment: newDeployment("foo", 2, nil, ptr.To(intstr.FromInt32(1)), ptr.To(intstr.FromInt32(1)), nil),

			newRS: func() *apps.ReplicaSet {
				rs := rs("foo-v2", 2, nil, newTimestamp)
				rs.Status.AvailableReplicas = 0
				return rs
			}(),
			oldRSs: []*apps.ReplicaSet{rs("foo-v1", 1, nil, oldTimestamp)},

			expectedNew: rs("foo-v2", 2, nil, newTimestamp),
			expectedOld: []*apps.ReplicaSet{rs("foo-v1", 1, nil, oldTimestamp)},
		},
		// Ignores terminating pods.
		{
			name:                                 "scaling event with terminating pods is not partial with empty policy:: 10 -> 14",
			enableDeploymentPodReplacementPolicy: true,
			deployment:                           newDeployment("foo", 14, nil, nil, nil, nil),
			oldDeployment:                        newDeployment("foo", 10, nil, nil, nil, nil),

			newRS:  newRSWithFullStatus("foo-v1", 10, apps.ReplicaSetStatus{TerminatingReplicas: ptr.To[int32](2)}, nil, nil, newTimestamp),
			oldRSs: []*apps.ReplicaSet{},

			expectedNew: newRSWithFullStatus("foo-v1", 14, apps.ReplicaSetStatus{TerminatingReplicas: ptr.To[int32](2)}, map[string]string{
				deploymentutil.DesiredReplicasAnnotation: "14",
				deploymentutil.MaxReplicasAnnotation:     "14",
			}, nil, newTimestamp),
			expectedOld: []*apps.ReplicaSet{},
		},
		// Ignores terminating pods.
		{
			name:                                 "scaling event with terminating pods is not partial with TerminationStarted policy:: 10 -> 14",
			enableDeploymentPodReplacementPolicy: true,
			deployment:                           newDeployment("foo", 14, nil, nil, nil, nil),
			oldDeployment:                        newDeployment("foo", 10, nil, nil, nil, nil),
			podReplacementPolicy:                 ptr.To(apps.TerminationStarted),

			newRS:  newRSWithFullStatus("foo-v1", 10, apps.ReplicaSetStatus{TerminatingReplicas: ptr.To[int32](2)}, nil, nil, newTimestamp),
			oldRSs: []*apps.ReplicaSet{},

			expectedNew: newRSWithFullStatus("foo-v1", 14, apps.ReplicaSetStatus{TerminatingReplicas: ptr.To[int32](2)}, map[string]string{
				deploymentutil.DesiredReplicasAnnotation: "14",
				deploymentutil.MaxReplicasAnnotation:     "14",
			}, nil, newTimestamp),
			expectedOld: []*apps.ReplicaSet{},
		},
		// Takes terminating pods into account and scales partially.
		{
			name:                                 "scaling event with terminating pods and TerminationComplete policy: 10 -> 12 (14)",
			enableDeploymentPodReplacementPolicy: true,
			deployment:                           newDeployment("foo", 14, nil, nil, nil, nil),
			oldDeployment:                        newDeployment("foo", 10, nil, nil, nil, nil),
			podReplacementPolicy:                 ptr.To(apps.TerminationComplete),

			newRS:  newRSWithFullStatus("foo-v1", 10, apps.ReplicaSetStatus{TerminatingReplicas: ptr.To[int32](2)}, nil, nil, newTimestamp),
			oldRSs: []*apps.ReplicaSet{},

			expectedNew: newRSWithFullStatus("foo-v1", 12, apps.ReplicaSetStatus{TerminatingReplicas: ptr.To[int32](2)}, map[string]string{
				deploymentutil.DesiredReplicasAnnotation:               "10",
				deploymentutil.MaxReplicasAnnotation:                   "10",
				deploymentutil.ReplicaSetReplicasBeforeScaleAnnotation: "10",
			}, nil, newTimestamp),
			expectedOld: []*apps.ReplicaSet{},
		},
		// Takes terminating pods into account and scales partially for Recreate Deployment.
		{
			name:                                 "scaling event (Recreate Deployment) with terminating pods and TerminationComplete policy: 10 -> 12 (14)",
			enableDeploymentPodReplacementPolicy: true,
			deployment: func() *apps.Deployment {
				d := newDeployment("foo", 14, nil, nil, nil, nil)
				d.Spec.Strategy = apps.DeploymentStrategy{Type: apps.RecreateDeploymentStrategyType}
				return d
			}(),
			oldDeployment: func() *apps.Deployment {
				d := newDeployment("foo", 10, nil, nil, nil, nil)
				d.Spec.Strategy = apps.DeploymentStrategy{Type: apps.RecreateDeploymentStrategyType}
				return d
			}(),
			podReplacementPolicy: ptr.To(apps.TerminationComplete),

			newRS:  newRSWithFullStatus("foo-v1", 10, apps.ReplicaSetStatus{TerminatingReplicas: ptr.To[int32](2)}, nil, nil, newTimestamp),
			oldRSs: []*apps.ReplicaSet{},

			expectedNew: newRSWithFullStatus("foo-v1", 12, apps.ReplicaSetStatus{TerminatingReplicas: ptr.To[int32](2)}, map[string]string{
				deploymentutil.DesiredReplicasAnnotation:               "10",
				deploymentutil.MaxReplicasAnnotation:                   "10",
				deploymentutil.ReplicaSetReplicasBeforeScaleAnnotation: "10",
			}, nil, newTimestamp),
			expectedOld: []*apps.ReplicaSet{},
		},
		// Takes surge pods into account and scales partially.
		// ReplicaSet with a greater number of .status.replicas than .spec.replicas is not synced and the extra pod will likely be terminated shortly.
		// We account for this by downscaling the RS by one pod. If the pod does not terminate in the meantime, we will scale to 14 shortly.
		{
			name:                                 "scaling event with surge pods and TerminationComplete policy: 10 -> 13 (14)",
			enableDeploymentPodReplacementPolicy: true,
			deployment:                           newDeployment("foo", 14, nil, nil, nil, nil),
			oldDeployment:                        newDeployment("foo", 10, nil, nil, nil, nil),
			podReplacementPolicy:                 ptr.To(apps.TerminationComplete),

			newRS:  newRSWithFullStatus("foo-v1", 10, apps.ReplicaSetStatus{ObservedGeneration: 1, Replicas: 11, TerminatingReplicas: ptr.To[int32](0)}, nil, nil, newTimestamp),
			oldRSs: []*apps.ReplicaSet{},

			expectedNew: newRSWithFullStatus("foo-v1", 13, apps.ReplicaSetStatus{ObservedGeneration: 1, Replicas: 11, TerminatingReplicas: ptr.To[int32](0)}, map[string]string{
				deploymentutil.DesiredReplicasAnnotation:               "10",
				deploymentutil.MaxReplicasAnnotation:                   "10",
				deploymentutil.ReplicaSetReplicasBeforeScaleAnnotation: "10",
			}, nil, newTimestamp),
			expectedOld: []*apps.ReplicaSet{},
		},
		// Takes surge pods into account and scales partially for Recreate deployment.
		// ReplicaSet with a greater number of .status.replicas than .spec.replicas is not synced and the extra pod will likely be terminated shortly.
		// We account for this by downscaling the RS by one pod. If the pod does not terminate in the meantime, we will scale to 14 shortly.
		{
			name:                                 "scaling event (Recreate Deployment) with surge pods and TerminationComplete policy: 10 -> 12 (14)",
			enableDeploymentPodReplacementPolicy: true,
			deployment: func() *apps.Deployment {
				d := newDeployment("foo", 14, nil, nil, nil, nil)
				d.Spec.Strategy = apps.DeploymentStrategy{Type: apps.RecreateDeploymentStrategyType}
				return d
			}(),
			oldDeployment: func() *apps.Deployment {
				d := newDeployment("foo", 10, nil, nil, nil, nil)
				d.Spec.Strategy = apps.DeploymentStrategy{Type: apps.RecreateDeploymentStrategyType}
				return d
			}(),
			podReplacementPolicy: ptr.To(apps.TerminationComplete),

			newRS:  newRSWithFullStatus("foo-v1", 10, apps.ReplicaSetStatus{Replicas: 11}, nil, nil, newTimestamp),
			oldRSs: []*apps.ReplicaSet{},

			expectedNew: newRSWithFullStatus("foo-v1", 13, apps.ReplicaSetStatus{Replicas: 11}, map[string]string{
				deploymentutil.DesiredReplicasAnnotation:               "10",
				deploymentutil.MaxReplicasAnnotation:                   "10",
				deploymentutil.ReplicaSetReplicasBeforeScaleAnnotation: "10",
			}, nil, newTimestamp),
			expectedOld: []*apps.ReplicaSet{},
		},

		// Scaling of RecreateDeploymentStrategyType with terminationComplete that is unsynced and has not started any pods yet.
		{
			name:                                 "scaling event (Recreate Deployment) with no running pods (unsynced newRS) and TerminationComplete policy: 10 -> 14",
			enableDeploymentPodReplacementPolicy: true,
			deployment: func() *apps.Deployment {
				d := newDeployment("foo", 14, nil, nil, nil, nil)
				d.Spec.Strategy = apps.DeploymentStrategy{Type: apps.RecreateDeploymentStrategyType}
				return d
			}(),
			oldDeployment: func() *apps.Deployment {
				d := newDeployment("foo", 10, nil, nil, nil, nil)
				d.Spec.Strategy = apps.DeploymentStrategy{Type: apps.RecreateDeploymentStrategyType}
				return d
			}(),
			podReplacementPolicy: ptr.To(apps.TerminationComplete),

			newRS:  newRSWithFullStatus("foo-v1", 10, apps.ReplicaSetStatus{}, nil, nil, newTimestamp),
			oldRSs: []*apps.ReplicaSet{},

			expectedNew: newRSWithFullStatus("foo-v1", 14, apps.ReplicaSetStatus{Replicas: 14}, map[string]string{
				deploymentutil.DesiredReplicasAnnotation: "14",
				deploymentutil.MaxReplicasAnnotation:     "14",
			}, nil, newTimestamp),
			expectedOld: []*apps.ReplicaSet{},
		},
		// Scaling of RecreateDeploymentStrategyType with terminationComplete that has broken rs (unknown terminatingReplicas).
		// This should not happen during a normal operation (functioning replica set controller and no 3rd party status tampering).
		{
			name:                                 "no scaling event (Recreate Deployment) with broken old rs (unknown terminatingReplicas) and TerminationComplete policy should error",
			enableDeploymentPodReplacementPolicy: true,
			deployment: func() *apps.Deployment {
				d := newDeployment("foo", 14, nil, nil, nil, nil)
				d.Spec.Strategy = apps.DeploymentStrategy{Type: apps.RecreateDeploymentStrategyType}
				return d
			}(),
			oldDeployment: func() *apps.Deployment {
				d := newDeployment("foo", 10, nil, nil, nil, nil)
				d.Spec.Strategy = apps.DeploymentStrategy{Type: apps.RecreateDeploymentStrategyType}
				return d
			}(),
			podReplacementPolicy: ptr.To(apps.TerminationComplete),

			newRS:           newRSWithFullStatus("foo-v2", 10, apps.ReplicaSetStatus{}, nil, nil, newTimestamp),
			oldRSs:          []*apps.ReplicaSet{newRSWithFullStatus("foo-v1", 0, apps.ReplicaSetStatus{ObservedGeneration: 1, TerminatingReplicas: nil, Replicas: 0}, nil, nil, oldTimestamp)},
			expectedSyncErr: true,
		},
		// Takes terminating pods into account and scales partially.
		{
			name:                                 "scaling event with maxSurge, terminating pods and TerminationComplete policy [part 1]: 10 -> 13 (14)",
			enableDeploymentPodReplacementPolicy: true,
			deployment:                           newDeployment("foo", 14, nil, ptr.To(intstr.FromInt32(3)), nil, nil),
			oldDeployment:                        newDeployment("foo", 10, nil, ptr.To(intstr.FromInt32(3)), nil, nil),
			podReplacementPolicy:                 ptr.To(apps.TerminationComplete),

			newRS:  newRSWithFullStatus("foo-v1", 10, apps.ReplicaSetStatus{TerminatingReplicas: ptr.To[int32](4)}, nil, nil, newTimestamp),
			oldRSs: []*apps.ReplicaSet{},

			expectedNew: newRSWithFullStatus("foo-v1", 13, apps.ReplicaSetStatus{TerminatingReplicas: ptr.To[int32](4)}, map[string]string{
				deploymentutil.DesiredReplicasAnnotation:               "10",
				deploymentutil.MaxReplicasAnnotation:                   "13",
				deploymentutil.ReplicaSetReplicasBeforeScaleAnnotation: "10",
			}, nil, newTimestamp),
			expectedOld: []*apps.ReplicaSet{},
		},
		// Completes partial scaling once terminating pods are gone. Also covers unsynced newRS status case.
		{
			name:                                 "scaling event with maxSurge, terminating pods and TerminationComplete policy [part 2]: 10 -> 14 (14)",
			enableDeploymentPodReplacementPolicy: true,
			deployment:                           newDeployment("foo", 14, nil, ptr.To(intstr.FromInt32(3)), nil, nil),
			oldDeployment:                        newDeployment("foo", 10, nil, ptr.To(intstr.FromInt32(3)), nil, nil),
			podReplacementPolicy:                 ptr.To(apps.TerminationComplete),

			newRS: newRSWithFullStatus("foo-v1", 13, apps.ReplicaSetStatus{}, map[string]string{
				deploymentutil.DesiredReplicasAnnotation:               "10",
				deploymentutil.MaxReplicasAnnotation:                   "13",
				deploymentutil.ReplicaSetReplicasBeforeScaleAnnotation: "10",
			}, nil, newTimestamp),
			oldRSs: []*apps.ReplicaSet{},

			expectedNew: newRSWithFullStatus("foo-v1", 14, apps.ReplicaSetStatus{}, map[string]string{
				deploymentutil.DesiredReplicasAnnotation: "14",
				deploymentutil.MaxReplicasAnnotation:     "17",
			}, nil, newTimestamp),
			expectedOld: []*apps.ReplicaSet{},
		},
		// Ignores terminating pods.
		{
			name:                                 "proportional scaling with terminating pods is not partial with empty policy: 5 -> 10",
			enableDeploymentPodReplacementPolicy: true,
			deployment:                           newDeployment("foo", 10, nil, nil, nil, nil),
			oldDeployment:                        newDeployment("foo", 5, nil, nil, nil, nil),

			newRS:  newRSWithFullStatus("foo-v2", 2, apps.ReplicaSetStatus{TerminatingReplicas: ptr.To[int32](1)}, nil, nil, newTimestamp),
			oldRSs: []*apps.ReplicaSet{newRSWithFullStatus("foo-v1", 3, apps.ReplicaSetStatus{TerminatingReplicas: ptr.To[int32](1), Replicas: 4}, nil, nil, oldTimestamp)},

			expectedNew: newRSWithFullStatus("foo-v2", 4, apps.ReplicaSetStatus{TerminatingReplicas: ptr.To[int32](1)}, map[string]string{
				deploymentutil.DesiredReplicasAnnotation: "10",
				deploymentutil.MaxReplicasAnnotation:     "10",
			}, nil, newTimestamp),
			expectedOld: []*apps.ReplicaSet{newRSWithFullStatus("foo-v1", 6, apps.ReplicaSetStatus{TerminatingReplicas: ptr.To[int32](1), Replicas: 4}, map[string]string{
				deploymentutil.DesiredReplicasAnnotation: "10",
				deploymentutil.MaxReplicasAnnotation:     "10",
			}, nil, oldTimestamp)},
		},
		// Ignores terminating pods.
		{
			name:                                 "proportional scaling with terminating pods is not partial with TerminationStarted policy: 5 -> 10",
			enableDeploymentPodReplacementPolicy: true,
			deployment:                           newDeployment("foo", 10, nil, nil, nil, nil),
			oldDeployment:                        newDeployment("foo", 5, nil, nil, nil, nil),
			podReplacementPolicy:                 ptr.To(apps.TerminationStarted),

			newRS:  newRSWithFullStatus("foo-v2", 2, apps.ReplicaSetStatus{TerminatingReplicas: ptr.To[int32](1)}, nil, nil, newTimestamp),
			oldRSs: []*apps.ReplicaSet{newRSWithFullStatus("foo-v1", 3, apps.ReplicaSetStatus{TerminatingReplicas: ptr.To[int32](1), Replicas: 4}, nil, nil, oldTimestamp)},

			expectedNew: newRSWithFullStatus("foo-v2", 4, apps.ReplicaSetStatus{TerminatingReplicas: ptr.To[int32](1)}, map[string]string{
				deploymentutil.DesiredReplicasAnnotation: "10",
				deploymentutil.MaxReplicasAnnotation:     "10",
			}, nil, newTimestamp),
			expectedOld: []*apps.ReplicaSet{newRSWithFullStatus("foo-v1", 6, apps.ReplicaSetStatus{TerminatingReplicas: ptr.To[int32](1), Replicas: 4}, map[string]string{
				deploymentutil.DesiredReplicasAnnotation: "10",
				deploymentutil.MaxReplicasAnnotation:     "10",
			}, nil, oldTimestamp)},
		},
		// Takes terminating pods into account and scales partially in proportional scaling.
		{
			name:                                 "proportional partial scaling with terminating pods and TerminationComplete policy: 5 -> 8 (10)",
			enableDeploymentPodReplacementPolicy: true,
			deployment:                           newDeployment("foo", 10, nil, nil, nil, nil),
			oldDeployment:                        newDeployment("foo", 5, nil, nil, nil, nil),
			podReplacementPolicy:                 ptr.To(apps.TerminationComplete),

			newRS:  newRSWithFullStatus("foo-v2", 2, apps.ReplicaSetStatus{TerminatingReplicas: ptr.To[int32](1)}, nil, nil, newTimestamp),
			oldRSs: []*apps.ReplicaSet{newRSWithFullStatus("foo-v1", 3, apps.ReplicaSetStatus{TerminatingReplicas: ptr.To[int32](1)}, nil, nil, oldTimestamp)},

			expectedNew: newRSWithFullStatus("foo-v2", 2, apps.ReplicaSetStatus{TerminatingReplicas: ptr.To[int32](1)}, map[string]string{
				deploymentutil.DesiredReplicasAnnotation:               "5",
				deploymentutil.MaxReplicasAnnotation:                   "5",
				deploymentutil.ReplicaSetReplicasBeforeScaleAnnotation: "2",
			}, nil, newTimestamp),
			expectedOld: []*apps.ReplicaSet{newRSWithFullStatus("foo-v1", 6, apps.ReplicaSetStatus{TerminatingReplicas: ptr.To[int32](1)}, map[string]string{
				deploymentutil.DesiredReplicasAnnotation: "10",
				deploymentutil.MaxReplicasAnnotation:     "10",
			}, nil, oldTimestamp)},
		},

		// Partial scaling with terminationComplete that has broken rs (unknown terminatingReplicas).
		// This should not happen during a normal operation (functioning replica set controller and no 3rd party status tampering).
		{
			name:                                 "no scaling event (targeted proportional) with broken old rs (unknown terminatingReplicas) and TerminationComplete policy should error",
			enableDeploymentPodReplacementPolicy: true,
			deployment:                           newDeployment("foo", 10, nil, nil, nil, nil),
			oldDeployment:                        newDeployment("foo", 5, nil, nil, nil, nil),
			podReplacementPolicy:                 ptr.To(apps.TerminationComplete),

			newRS:           newRSWithFullStatus("foo-v2", 2, apps.ReplicaSetStatus{ObservedGeneration: 1, TerminatingReplicas: ptr.To[int32](1)}, nil, nil, newTimestamp),
			oldRSs:          []*apps.ReplicaSet{newRSWithFullStatus("foo-v1", 3, apps.ReplicaSetStatus{ObservedGeneration: 1, TerminatingReplicas: nil}, nil, nil, oldTimestamp)},
			expectedSyncErr: true,
		},
		// Takes terminating pods into account and scales partially in proportional scaling.
		{
			name:                                 "proportional partial scaling with terminating and surge pods and TerminationComplete policy [part 1]: 7 -> 10 (12)",
			enableDeploymentPodReplacementPolicy: true,
			deployment:                           newDeployment("foo", 12, nil, nil, nil, nil),
			oldDeployment:                        newDeployment("foo", 7, nil, nil, nil, nil),
			podReplacementPolicy:                 ptr.To(apps.TerminationComplete),

			newRS: newRSWithFullStatus("foo-v3", 2, apps.ReplicaSetStatus{TerminatingReplicas: ptr.To[int32](1)}, nil, nil, newTimestamp),
			oldRSs: []*apps.ReplicaSet{newRSWithFullStatus("foo-v2", 3, apps.ReplicaSetStatus{}, nil, nil, oldTimestamp),
				newRSWithFullStatus("foo-v1", 2, apps.ReplicaSetStatus{ObservedGeneration: 1, Replicas: 3, TerminatingReplicas: ptr.To[int32](0)}, nil, nil, olderTimestamp)},

			expectedNew: newRSWithFullStatus("foo-v3", 3, apps.ReplicaSetStatus{TerminatingReplicas: ptr.To[int32](1)}, map[string]string{
				deploymentutil.DesiredReplicasAnnotation: "12",
				deploymentutil.MaxReplicasAnnotation:     "12",
			}, nil, newTimestamp),
			expectedOld: []*apps.ReplicaSet{newRSWithFullStatus("foo-v2", 5, apps.ReplicaSetStatus{}, map[string]string{
				deploymentutil.DesiredReplicasAnnotation:               "7",
				deploymentutil.MaxReplicasAnnotation:                   "7",
				deploymentutil.ReplicaSetReplicasBeforeScaleAnnotation: "3",
			}, nil, oldTimestamp), newRSWithFullStatus("foo-v1", 2, apps.ReplicaSetStatus{ObservedGeneration: 1, Replicas: 3, TerminatingReplicas: ptr.To[int32](0)}, map[string]string{
				deploymentutil.DesiredReplicasAnnotation:               "7",
				deploymentutil.MaxReplicasAnnotation:                   "7",
				deploymentutil.ReplicaSetReplicasBeforeScaleAnnotation: "2",
			}, nil, olderTimestamp)},
		},
		// Completes partial scaling once terminating pods are gone in proportional scaling. Also covers unsynced RS status case.
		{
			name:                                 "proportional partial scaling with terminating and surge pods and TerminationComplete policy [part 2]: 7 -> 12 (12)",
			enableDeploymentPodReplacementPolicy: true,
			deployment:                           newDeployment("foo", 12, nil, nil, nil, nil),
			oldDeployment:                        newDeployment("foo", 7, nil, nil, nil, nil),
			podReplacementPolicy:                 ptr.To(apps.TerminationComplete),

			newRS: newRSWithFullStatus("foo-v3", 3, apps.ReplicaSetStatus{}, map[string]string{
				deploymentutil.DesiredReplicasAnnotation: "12",
				deploymentutil.MaxReplicasAnnotation:     "12",
			}, nil, newTimestamp),
			oldRSs: []*apps.ReplicaSet{newRSWithFullStatus("foo-v2", 5, apps.ReplicaSetStatus{}, map[string]string{
				deploymentutil.DesiredReplicasAnnotation:               "7",
				deploymentutil.MaxReplicasAnnotation:                   "7",
				deploymentutil.ReplicaSetReplicasBeforeScaleAnnotation: "3",
			}, nil, oldTimestamp), newRSWithFullStatus("foo-v1", 2, apps.ReplicaSetStatus{ObservedGeneration: 1, Replicas: 2, TerminatingReplicas: ptr.To[int32](0)}, map[string]string{
				deploymentutil.DesiredReplicasAnnotation:               "7",
				deploymentutil.MaxReplicasAnnotation:                   "7",
				deploymentutil.ReplicaSetReplicasBeforeScaleAnnotation: "2",
			}, nil, olderTimestamp)},

			expectedNew: newRSWithFullStatus("foo-v3", 3, apps.ReplicaSetStatus{}, map[string]string{
				deploymentutil.DesiredReplicasAnnotation: "12",
				deploymentutil.MaxReplicasAnnotation:     "12",
			}, nil, newTimestamp),
			expectedOld: []*apps.ReplicaSet{newRSWithFullStatus("foo-v2", 6, apps.ReplicaSetStatus{}, map[string]string{
				deploymentutil.DesiredReplicasAnnotation: "12",
				deploymentutil.MaxReplicasAnnotation:     "12",
			}, nil, oldTimestamp), newRSWithFullStatus("foo-v1", 3, apps.ReplicaSetStatus{ObservedGeneration: 1, Replicas: 2, TerminatingReplicas: ptr.To[int32](0)}, map[string]string{
				deploymentutil.DesiredReplicasAnnotation: "12",
				deploymentutil.MaxReplicasAnnotation:     "12",
			}, nil, olderTimestamp)},
		},
		// Takes terminating pods into account and scales partially in proportional scaling.
		{
			name:                                 "proportional partial scaling with maxSurge, terminating and surge pods and TerminationComplete policy [part 1]: 100 -> 115 (130)",
			enableDeploymentPodReplacementPolicy: true,
			deployment:                           newDeployment("foo", 120, nil, ptr.To(intstr.FromInt32(10)), nil, nil),
			oldDeployment:                        newDeployment("foo", 100, nil, ptr.To(intstr.FromInt32(10)), nil, nil),
			podReplacementPolicy:                 ptr.To(apps.TerminationComplete),

			newRS: newRSWithFullStatus("foo-v3", 50, apps.ReplicaSetStatus{TerminatingReplicas: ptr.To[int32](6)}, map[string]string{
				deploymentutil.DesiredReplicasAnnotation: "100",
				deploymentutil.MaxReplicasAnnotation:     "110",
			}, nil, newTimestamp),
			oldRSs: []*apps.ReplicaSet{newRSWithFullStatus("foo-v2", 30, apps.ReplicaSetStatus{TerminatingReplicas: ptr.To[int32](4)}, map[string]string{
				deploymentutil.DesiredReplicasAnnotation: "100",
				deploymentutil.MaxReplicasAnnotation:     "110",
			}, nil, oldTimestamp), newRSWithFullStatus("foo-v1", 20, apps.ReplicaSetStatus{TerminatingReplicas: ptr.To[int32](5)}, map[string]string{
				deploymentutil.DesiredReplicasAnnotation: "100",
				deploymentutil.MaxReplicasAnnotation:     "110",
			}, nil, olderTimestamp)},

			expectedNew: newRSWithFullStatus("foo-v3", 59, apps.ReplicaSetStatus{TerminatingReplicas: ptr.To[int32](6)}, map[string]string{
				deploymentutil.DesiredReplicasAnnotation:               "100",
				deploymentutil.MaxReplicasAnnotation:                   "110",
				deploymentutil.ReplicaSetReplicasBeforeScaleAnnotation: "50",
			}, nil, newTimestamp),
			expectedOld: []*apps.ReplicaSet{newRSWithFullStatus("foo-v2", 35, apps.ReplicaSetStatus{TerminatingReplicas: ptr.To[int32](4)}, map[string]string{
				deploymentutil.DesiredReplicasAnnotation: "120",
				deploymentutil.MaxReplicasAnnotation:     "130",
			}, nil, oldTimestamp), newRSWithFullStatus("foo-v1", 21, apps.ReplicaSetStatus{TerminatingReplicas: ptr.To[int32](5)}, map[string]string{
				deploymentutil.DesiredReplicasAnnotation:               "100",
				deploymentutil.MaxReplicasAnnotation:                   "110",
				deploymentutil.ReplicaSetReplicasBeforeScaleAnnotation: "20",
			}, nil, olderTimestamp)},
		},
		// Takes terminating pods into account and scales partially in proportional scaling.
		{
			name:                                 "proportional partial scaling with maxSurge, terminating and surge pods and TerminationComplete policy [part 2]: 100 -> 125 (130)",
			enableDeploymentPodReplacementPolicy: true,
			deployment:                           newDeployment("foo", 120, nil, ptr.To(intstr.FromInt32(10)), nil, nil),
			oldDeployment:                        newDeployment("foo", 100, nil, ptr.To(intstr.FromInt32(10)), nil, nil),
			podReplacementPolicy:                 ptr.To(apps.TerminationComplete),

			newRS: newRSWithFullStatus("foo-v3", 59, apps.ReplicaSetStatus{TerminatingReplicas: ptr.To[int32](2)}, map[string]string{
				deploymentutil.DesiredReplicasAnnotation:               "100",
				deploymentutil.MaxReplicasAnnotation:                   "110",
				deploymentutil.ReplicaSetReplicasBeforeScaleAnnotation: "50",
			}, nil, newTimestamp),
			oldRSs: []*apps.ReplicaSet{newRSWithFullStatus("foo-v2", 35, apps.ReplicaSetStatus{TerminatingReplicas: ptr.To[int32](3)}, map[string]string{
				deploymentutil.DesiredReplicasAnnotation: "120",
				deploymentutil.MaxReplicasAnnotation:     "130",
			}, nil, oldTimestamp), newRSWithFullStatus("foo-v1", 21, apps.ReplicaSetStatus{}, map[string]string{
				deploymentutil.DesiredReplicasAnnotation:               "100",
				deploymentutil.MaxReplicasAnnotation:                   "110",
				deploymentutil.ReplicaSetReplicasBeforeScaleAnnotation: "20",
			}, nil, olderTimestamp)},

			expectedNew: newRSWithFullStatus("foo-v3", 66, apps.ReplicaSetStatus{TerminatingReplicas: ptr.To[int32](2)}, map[string]string{
				deploymentutil.DesiredReplicasAnnotation:               "100",
				deploymentutil.MaxReplicasAnnotation:                   "110",
				deploymentutil.ReplicaSetReplicasBeforeScaleAnnotation: "50",
			}, nil, newTimestamp),
			expectedOld: []*apps.ReplicaSet{newRSWithFullStatus("foo-v2", 35, apps.ReplicaSetStatus{TerminatingReplicas: ptr.To[int32](3)}, map[string]string{
				deploymentutil.DesiredReplicasAnnotation: "120",
				deploymentutil.MaxReplicasAnnotation:     "130",
			}, nil, oldTimestamp), newRSWithFullStatus("foo-v1", 24, apps.ReplicaSetStatus{}, map[string]string{
				deploymentutil.DesiredReplicasAnnotation: "120",
				deploymentutil.MaxReplicasAnnotation:     "130",
			}, nil, olderTimestamp)},
		},
		// Completes partial scaling once terminating pods are gone in proportional scaling.
		{
			name:                                 "proportional partial scaling with maxSurge, terminating and surge pods and TerminationComplete policy [part 3]: 100 -> 130 (130)",
			enableDeploymentPodReplacementPolicy: true,
			deployment:                           newDeployment("foo", 120, nil, ptr.To(intstr.FromInt32(10)), nil, nil),
			oldDeployment:                        newDeployment("foo", 100, nil, ptr.To(intstr.FromInt32(10)), nil, nil),
			podReplacementPolicy:                 ptr.To(apps.TerminationComplete),

			newRS: newRSWithFullStatus("foo-v3", 66, apps.ReplicaSetStatus{}, map[string]string{
				deploymentutil.DesiredReplicasAnnotation:               "100",
				deploymentutil.MaxReplicasAnnotation:                   "110",
				deploymentutil.ReplicaSetReplicasBeforeScaleAnnotation: "50",
			}, nil, newTimestamp),
			oldRSs: []*apps.ReplicaSet{newRSWithFullStatus("foo-v2", 35, apps.ReplicaSetStatus{}, map[string]string{
				deploymentutil.DesiredReplicasAnnotation: "120",
				deploymentutil.MaxReplicasAnnotation:     "130",
			}, nil, oldTimestamp), newRSWithFullStatus("foo-v1", 24, apps.ReplicaSetStatus{}, map[string]string{
				deploymentutil.DesiredReplicasAnnotation: "120",
				deploymentutil.MaxReplicasAnnotation:     "130",
			}, nil, olderTimestamp)},

			expectedNew: newRSWithFullStatus("foo-v3", 71, apps.ReplicaSetStatus{}, map[string]string{
				deploymentutil.DesiredReplicasAnnotation: "120",
				deploymentutil.MaxReplicasAnnotation:     "130",
			}, nil, newTimestamp),
			expectedOld: []*apps.ReplicaSet{newRSWithFullStatus("foo-v2", 35, apps.ReplicaSetStatus{}, map[string]string{
				deploymentutil.DesiredReplicasAnnotation: "120",
				deploymentutil.MaxReplicasAnnotation:     "130",
			}, nil, oldTimestamp), newRSWithFullStatus("foo-v1", 24, apps.ReplicaSetStatus{}, map[string]string{
				deploymentutil.DesiredReplicasAnnotation: "120",
				deploymentutil.MaxReplicasAnnotation:     "130",
			}, nil, olderTimestamp)},
		},
	}

	for _, testScaleVersion := range []string{"v1", "v2"} {
		for _, test := range tests {
			if test.enableDeploymentPodReplacementPolicy && testScaleVersion == "v1" {
				// only v2 scaling supports PodReplacementPolicy
				continue
			}
			t.Run(fmt.Sprintf("%v with %v scaling", test.name, testScaleVersion), func(t *testing.T) {
				featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.DeploymentPodReplacementPolicy, test.enableDeploymentPodReplacementPolicy)
				test.deployment.Spec.PodReplacementPolicy = test.podReplacementPolicy
				test.oldDeployment.Spec.PodReplacementPolicy = test.podReplacementPolicy

				_ = olderTimestamp
				t.Log(test.name)
				fake := fake.Clientset{}
				dc := &DeploymentController{
					client:        &fake,
					eventRecorder: &record.FakeRecorder{},
				}

				if test.newRS != nil && test.newRS.Annotations == nil {
					tmpDesiredReplicas := test.oldDeployment.Spec.Replicas
					if desired, ok := test.desiredReplicasAnnotations[test.newRS.Name]; ok {
						test.oldDeployment.Spec.Replicas = ptr.To(desired)
					}
					switch testScaleVersion {
					case "v1":
						desiredReplicas := *test.oldDeployment.Spec.Replicas
						deploymentutil.SetReplicasAnnotations(test.newRS, desiredReplicas, desiredReplicas+deploymentutil.MaxSurge(*test.oldDeployment))
					case "v2":
						annotationUpdate, _ := deploymentutil.ComputeReplicaSetScaleAnnotationsV2(test.newRS, test.oldDeployment, false)
						deploymentutil.SetReplicaSetScaleAnnotationsV2(test.newRS, annotationUpdate)
					}
					test.oldDeployment.Spec.Replicas = tmpDesiredReplicas
				}
				for i := range test.oldRSs {
					rs := test.oldRSs[i]
					if rs == nil || rs.Annotations != nil {
						continue
					}
					tmpDesiredReplicas := test.oldDeployment.Spec.Replicas
					if desired, ok := test.desiredReplicasAnnotations[rs.Name]; ok {
						test.oldDeployment.Spec.Replicas = ptr.To(desired)
					}
					switch testScaleVersion {
					case "v1":
						desiredReplicas := *test.oldDeployment.Spec.Replicas
						deploymentutil.SetReplicasAnnotations(rs, desiredReplicas, desiredReplicas+deploymentutil.MaxSurge(*test.oldDeployment))
					case "v2":
						annotationUpdate, _ := deploymentutil.ComputeReplicaSetScaleAnnotationsV2(rs, test.oldDeployment, false)
						deploymentutil.SetReplicaSetScaleAnnotationsV2(rs, annotationUpdate)
					}
					test.oldDeployment.Spec.Replicas = tmpDesiredReplicas
				}

				_, ctx := ktesting.NewTestContext(t)

				var syncErr error
				switch testScaleVersion {
				case "v1":
					syncErr = dc.scale(ctx, test.deployment, test.newRS, test.oldRSs)
				case "v2":
					syncErr = dc.scaleV2(ctx, test.deployment, test.newRS, test.oldRSs)
				}
				if syncErr != nil && !test.expectedSyncErr {
					t.Errorf("got unexpected error %v", syncErr)
					return
				}
				if test.expectedSyncErr {
					if syncErr == nil {
						t.Error("expected error, got nil")
					}
					// sync error, do not check the rest of the expectations
					return
				}

				// Construct the nameToUpdatedRS map that will hold all the replicaset.spec.replicas sizes we got from our tests.
				// Skip updating the map if the replica set wasn't updated since there will be no update action for it.
				nameToUpdatedRS := make(map[string]*apps.ReplicaSet)
				if test.newRS != nil {
					nameToUpdatedRS[test.newRS.Name] = test.newRS
				}
				for i := range test.oldRSs {
					rs := test.oldRSs[i]
					nameToUpdatedRS[rs.Name] = rs
				}
				// Get all the UPDATE actions and update nameToUpdatedRS with all the updated replica sets.
				for _, action := range fake.Actions() {
					rs := action.(testclient.UpdateAction).GetObject().(*apps.ReplicaSet)
					if !test.wasntUpdated[rs.Name] {
						nameToUpdatedRS[rs.Name] = rs
					}
				}

				if test.expectedNew != nil && test.newRS != nil {
					updatedRS := nameToUpdatedRS[test.newRS.Name]
					if *(test.expectedNew.Spec.Replicas) != *(updatedRS.Spec.Replicas) {
						t.Errorf("expected new replicas: %d, got: %d", *(test.expectedNew.Spec.Replicas), *(updatedRS.Spec.Replicas))
						return
					}
					if test.expectedNew.Annotations != nil {
						if !reflect.DeepEqual(test.expectedNew.Annotations, updatedRS.Annotations) {
							t.Fatalf("unexpected %q annotations: %s", test.expectedNew.Name, cmp.Diff(test.expectedNew.Annotations, updatedRS.Annotations))
						}
					}
				}
				if len(test.expectedOld) != len(test.oldRSs) {
					t.Errorf("expected %d old replica sets, got %d", len(test.expectedOld), len(test.oldRSs))
					return
				}
				for n := range test.oldRSs {
					rs := test.oldRSs[n]
					expected := test.expectedOld[n]
					updatedRS := nameToUpdatedRS[rs.Name]
					if *(expected.Spec.Replicas) != *(updatedRS.Spec.Replicas) {
						t.Errorf("expected old (%s) replicas: %d, got: %d", rs.Name, *(expected.Spec.Replicas), *(updatedRS.Spec.Replicas))
					}
					if expected.Annotations != nil {
						if !reflect.DeepEqual(expected.Annotations, updatedRS.Annotations) {
							t.Fatalf("unexpected %q annotations: %s", expected.Name, cmp.Diff(expected.Annotations, updatedRS.Annotations))
						}
					}

				}
			})
		}
	}
}

func TestDeploymentController_cleanupDeployment(t *testing.T) {
	selector := map[string]string{"foo": "bar"}
	alreadyDeleted := newRSWithStatus("foo-1", 0, 0, selector)
	now := metav1.Now()
	alreadyDeleted.DeletionTimestamp = &now

	tests := []struct {
		oldRSs                               []*apps.ReplicaSet
		revisionHistoryLimit                 int32
		expectedDeletions                    int
		enableDeploymentPodReplacementPolicy bool
	}{
		{
			oldRSs: []*apps.ReplicaSet{
				newRSWithStatus("foo-1", 0, 0, selector),
				newRSWithStatus("foo-2", 0, 0, selector),
				newRSWithStatus("foo-3", 0, 0, selector),
			},
			revisionHistoryLimit: 1,
			expectedDeletions:    2,
		},
		{
			// Only delete the replica set with Spec.Replicas = Status.Replicas = 0.
			oldRSs: []*apps.ReplicaSet{
				newRSWithStatus("foo-1", 0, 0, selector),
				newRSWithStatus("foo-2", 0, 1, selector),
				newRSWithStatus("foo-3", 1, 0, selector),
				newRSWithStatus("foo-4", 1, 1, selector),
			},
			revisionHistoryLimit: 0,
			expectedDeletions:    1,
		},
		{
			// Only delete the replica set with Spec.Replicas = Status.Replicas = Status.TerminatingReplicas = 0.
			oldRSs: []*apps.ReplicaSet{
				newRSWithFullStatus("foo-1", 0, apps.ReplicaSetStatus{ObservedGeneration: 1, TerminatingReplicas: ptr.To[int32](0)}, selector, nil, noTimestamp),
				newRSWithFullStatus("foo-2", 0, apps.ReplicaSetStatus{ObservedGeneration: 1, TerminatingReplicas: ptr.To[int32](1)}, selector, nil, noTimestamp),
				newRSWithFullStatus("foo-3", 0, apps.ReplicaSetStatus{ObservedGeneration: 1, TerminatingReplicas: nil}, selector, nil, noTimestamp),
				newRSWithFullStatus("foo-4", 0, apps.ReplicaSetStatus{ObservedGeneration: 1, Replicas: 1, TerminatingReplicas: ptr.To[int32](0)}, selector, nil, noTimestamp),
				newRSWithFullStatus("foo-5", 1, apps.ReplicaSetStatus{ObservedGeneration: 1, Replicas: 0, TerminatingReplicas: ptr.To[int32](0)}, selector, nil, noTimestamp),
				newRSWithFullStatus("foo-6", 1, apps.ReplicaSetStatus{ObservedGeneration: 1, Replicas: 1, TerminatingReplicas: ptr.To[int32](0)}, selector, nil, noTimestamp),
			},
			revisionHistoryLimit:                 0,
			expectedDeletions:                    1,
			enableDeploymentPodReplacementPolicy: true,
		},

		{
			oldRSs: []*apps.ReplicaSet{
				newRSWithStatus("foo-1", 0, 0, selector),
				newRSWithStatus("foo-2", 0, 0, selector),
			},
			revisionHistoryLimit: 0,
			expectedDeletions:    2,
		},
		{
			oldRSs: []*apps.ReplicaSet{
				newRSWithStatus("foo-1", 1, 1, selector),
				newRSWithStatus("foo-2", 1, 1, selector),
			},
			revisionHistoryLimit: 0,
			expectedDeletions:    0,
		},
		{
			oldRSs: []*apps.ReplicaSet{
				alreadyDeleted,
			},
			revisionHistoryLimit: 0,
			expectedDeletions:    0,
		},
		{
			// with unlimited revisionHistoryLimit
			oldRSs: []*apps.ReplicaSet{
				newRSWithStatus("foo-1", 0, 0, selector),
				newRSWithStatus("foo-2", 0, 0, selector),
				newRSWithStatus("foo-3", 0, 0, selector),
			},
			revisionHistoryLimit: math.MaxInt32,
			expectedDeletions:    0,
		},
	}

	for i := range tests {
		test := tests[i]
		// TODO: once the feature gating is removed, tests with enableDeploymentPodReplacementPolicy=false should set .status.terminatingReplicas=0 (non nil) to their replica sets
		featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.DeploymentPodReplacementPolicy, test.enableDeploymentPodReplacementPolicy)
		t.Logf("scenario %d", i)

		_, ctx := ktesting.NewTestContext(t)

		fake := &fake.Clientset{}
		informers := informers.NewSharedInformerFactory(fake, controller.NoResyncPeriodFunc())
		controller, err := NewDeploymentController(ctx, informers.Apps().V1().Deployments(), informers.Apps().V1().ReplicaSets(), informers.Core().V1().Pods(), fake)
		if err != nil {
			t.Fatalf("error creating Deployment controller: %v", err)
		}

		controller.eventRecorder = &record.FakeRecorder{}
		controller.dListerSynced = alwaysReady
		controller.rsListerSynced = alwaysReady
		controller.podListerSynced = alwaysReady
		for _, rs := range test.oldRSs {
			informers.Apps().V1().ReplicaSets().Informer().GetIndexer().Add(rs)
		}

		stopCh := make(chan struct{})
		defer close(stopCh)
		informers.Start(stopCh)
		informers.WaitForCacheSync(stopCh)

		t.Logf(" &test.revisionHistoryLimit: %d", test.revisionHistoryLimit)
		d := newDeployment("foo", 1, &test.revisionHistoryLimit, nil, nil, map[string]string{"foo": "bar"})
		controller.cleanupDeployment(ctx, test.oldRSs, d)

		gotDeletions := 0
		for _, action := range fake.Actions() {
			if action.GetVerb() == "delete" {
				gotDeletions++
			}
		}
		if gotDeletions != test.expectedDeletions {
			t.Errorf("expect %v old replica sets been deleted, but got %v", test.expectedDeletions, gotDeletions)
			continue
		}
	}
}

func TestDeploymentController_cleanupDeploymentOrder(t *testing.T) {
	selector := map[string]string{"foo": "bar"}
	now := metav1.Now()
	duration := time.Minute

	newRSWithRevisionAndCreationTimestamp := func(name string, replicas int32, selector map[string]string, timestamp time.Time, revision string) *apps.ReplicaSet {
		rs := rs(name, replicas, selector, metav1.NewTime(timestamp))
		if revision != "" {
			rs.Annotations = map[string]string{
				deploymentutil.RevisionAnnotation: revision,
			}
		}
		rs.Status = apps.ReplicaSetStatus{
			Replicas: int32(replicas),
		}
		return rs
	}

	// for all test cases, creationTimestamp order keeps as: rs1 < rs2 < rs3 < r4
	tests := []struct {
		oldRSs               []*apps.ReplicaSet
		revisionHistoryLimit int32
		expectedDeletedRSs   sets.String
	}{
		{
			// revision order: rs1 < rs2, delete rs1
			oldRSs: []*apps.ReplicaSet{
				newRSWithRevisionAndCreationTimestamp("foo-1", 0, selector, now.Add(-1*duration), "1"),
				newRSWithRevisionAndCreationTimestamp("foo-2", 0, selector, now.Time, "2"),
			},
			revisionHistoryLimit: 1,
			expectedDeletedRSs:   sets.NewString("foo-1"),
		},
		{
			// revision order: rs2 < rs1, delete rs2
			oldRSs: []*apps.ReplicaSet{
				newRSWithRevisionAndCreationTimestamp("foo-1", 0, selector, now.Add(-1*duration), "2"),
				newRSWithRevisionAndCreationTimestamp("foo-2", 0, selector, now.Time, "1"),
			},
			revisionHistoryLimit: 1,
			expectedDeletedRSs:   sets.NewString("foo-2"),
		},
		{
			// rs1 has revision but rs2 doesn't have revision, delete rs2
			oldRSs: []*apps.ReplicaSet{
				newRSWithRevisionAndCreationTimestamp("foo-1", 0, selector, now.Add(-1*duration), "1"),
				newRSWithRevisionAndCreationTimestamp("foo-2", 0, selector, now.Time, ""),
			},
			revisionHistoryLimit: 1,
			expectedDeletedRSs:   sets.NewString("foo-2"),
		},
		{
			// rs1 doesn't have revision while rs2 has revision, delete rs1
			oldRSs: []*apps.ReplicaSet{
				newRSWithRevisionAndCreationTimestamp("foo-1", 0, selector, now.Add(-1*duration), ""),
				newRSWithRevisionAndCreationTimestamp("foo-2", 0, selector, now.Time, "2"),
			},
			revisionHistoryLimit: 1,
			expectedDeletedRSs:   sets.NewString("foo-1"),
		},
		{
			// revision order: rs1 < rs2 < r3, but rs1 has replicas, delete rs2
			oldRSs: []*apps.ReplicaSet{
				newRSWithRevisionAndCreationTimestamp("foo-1", 1, selector, now.Add(-1*duration), "1"),
				newRSWithRevisionAndCreationTimestamp("foo-2", 0, selector, now.Time, "2"),
				newRSWithRevisionAndCreationTimestamp("foo-3", 0, selector, now.Add(duration), "3"),
			},
			revisionHistoryLimit: 1,
			expectedDeletedRSs:   sets.NewString("foo-2"),
		},
		{
			// revision order: rs1 < rs2 < r3, both rs1 && rs2 have replicas, don't delete
			oldRSs: []*apps.ReplicaSet{
				newRSWithRevisionAndCreationTimestamp("foo-1", 1, selector, now.Add(-1*duration), "1"),
				newRSWithRevisionAndCreationTimestamp("foo-2", 1, selector, now.Time, "2"),
				newRSWithRevisionAndCreationTimestamp("foo-3", 0, selector, now.Add(duration), "3"),
			},
			revisionHistoryLimit: 1,
			expectedDeletedRSs:   sets.NewString(),
		},
		{
			// revision order: rs2 < rs4 < rs1 < rs3, delete rs2 && rs4
			oldRSs: []*apps.ReplicaSet{
				newRSWithRevisionAndCreationTimestamp("foo-1", 0, selector, now.Add(-1*duration), "3"),
				newRSWithRevisionAndCreationTimestamp("foo-2", 0, selector, now.Time, "1"),
				newRSWithRevisionAndCreationTimestamp("foo-3", 0, selector, now.Add(duration), "4"),
				newRSWithRevisionAndCreationTimestamp("foo-4", 0, selector, now.Add(2*duration), "2"),
			},
			revisionHistoryLimit: 2,
			expectedDeletedRSs:   sets.NewString("foo-2", "foo-4"),
		},
	}

	for i := range tests {
		test := tests[i]
		t.Logf("scenario %d", i)

		_, ctx := ktesting.NewTestContext(t)

		fake := &fake.Clientset{}
		informers := informers.NewSharedInformerFactory(fake, controller.NoResyncPeriodFunc())
		controller, err := NewDeploymentController(ctx, informers.Apps().V1().Deployments(), informers.Apps().V1().ReplicaSets(), informers.Core().V1().Pods(), fake)
		if err != nil {
			t.Fatalf("error creating Deployment controller: %v", err)
		}

		controller.eventRecorder = &record.FakeRecorder{}
		controller.dListerSynced = alwaysReady
		controller.rsListerSynced = alwaysReady
		controller.podListerSynced = alwaysReady
		for _, rs := range test.oldRSs {
			informers.Apps().V1().ReplicaSets().Informer().GetIndexer().Add(rs)
		}

		stopCh := make(chan struct{})
		defer close(stopCh)
		informers.Start(stopCh)

		d := newDeployment("foo", 1, &test.revisionHistoryLimit, nil, nil, map[string]string{"foo": "bar"})
		controller.cleanupDeployment(ctx, test.oldRSs, d)

		deletedRSs := sets.String{}
		for _, action := range fake.Actions() {
			deleteAction, ok := action.(testclient.DeleteActionImpl)
			if !ok {
				t.Logf("Found not-delete action with verb %v. Ignoring.", action.GetVerb())
				continue
			}

			if deleteAction.GetResource().Resource != "replicasets" {
				continue
			}

			deletedRSs.Insert(deleteAction.GetName())
		}
		t.Logf("&test.revisionHistoryLimit: %d, &test.deletedReplicaSets: %v", test.revisionHistoryLimit, deletedRSs)

		if !test.expectedDeletedRSs.Equal(deletedRSs) {
			t.Errorf("expect to delete old replica sets %v, but got %v", test.expectedDeletedRSs, deletedRSs)
			continue
		}
	}
}
