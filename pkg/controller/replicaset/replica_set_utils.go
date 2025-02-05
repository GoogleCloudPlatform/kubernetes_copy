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

// If you make changes to this file, you should also make the corresponding change in ReplicationController.

package replicaset

import (
	"context"
	"fmt"
	"reflect"

	"k8s.io/klog/v2"

	apps "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	appsclient "k8s.io/client-go/kubernetes/typed/apps/v1"
	podutil "k8s.io/kubernetes/pkg/api/v1/pod"
	"k8s.io/kubernetes/pkg/features"
	"k8s.io/utils/ptr"
)

// updateReplicaSetStatus attempts to update the Status.Replicas of the given ReplicaSet, with a single GET/PUT retry.
func updateReplicaSetStatus(logger klog.Logger, c appsclient.ReplicaSetInterface, rs *apps.ReplicaSet, newStatus apps.ReplicaSetStatus) (*apps.ReplicaSet, error) {
	// This is the steady state. It happens when the ReplicaSet doesn't have any expectations, since
	// we do a periodic relist every 30s. If the generations differ but the replicas are
	// the same, a caller might've resized to the same replica count.
	if rs.Status.Replicas == newStatus.Replicas &&
		rs.Status.FullyLabeledReplicas == newStatus.FullyLabeledReplicas &&
		rs.Status.ReadyReplicas == newStatus.ReadyReplicas &&
		rs.Status.AvailableReplicas == newStatus.AvailableReplicas &&
		ptr.Equal(rs.Status.TerminatingReplicas, newStatus.TerminatingReplicas) &&
		rs.Generation == rs.Status.ObservedGeneration &&
		reflect.DeepEqual(rs.Status.Conditions, newStatus.Conditions) {
		return rs, nil
	}

	// Save the generation number we acted on, otherwise we might wrongfully indicate
	// that we've seen a spec update when we retry.
	// TODO: This can clobber an update if we allow multiple agents to write to the
	// same status.
	newStatus.ObservedGeneration = rs.Generation

	var getErr, updateErr error
	var updatedRS *apps.ReplicaSet
	for i, rs := 0, rs; ; i++ {
		terminatingReplicasUpdateInfo := ""
		if utilfeature.DefaultFeatureGate.Enabled(features.DeploymentPodReplacementPolicy) {
			terminatingReplicasUpdateInfo = fmt.Sprintf("terminatingReplicas %s->%s, ", derefInt32ToStr(rs.Status.TerminatingReplicas), derefInt32ToStr(newStatus.TerminatingReplicas))
		}
		logger.V(4).Info(fmt.Sprintf("Updating status for %v: %s/%s, ", rs.Kind, rs.Namespace, rs.Name) +
			fmt.Sprintf("replicas %d->%d (need %d), ", rs.Status.Replicas, newStatus.Replicas, *(rs.Spec.Replicas)) +
			fmt.Sprintf("fullyLabeledReplicas %d->%d, ", rs.Status.FullyLabeledReplicas, newStatus.FullyLabeledReplicas) +
			fmt.Sprintf("readyReplicas %d->%d, ", rs.Status.ReadyReplicas, newStatus.ReadyReplicas) +
			fmt.Sprintf("availableReplicas %d->%d, ", rs.Status.AvailableReplicas, newStatus.AvailableReplicas) +
			terminatingReplicasUpdateInfo +
			fmt.Sprintf("sequence No: %v->%v", rs.Status.ObservedGeneration, newStatus.ObservedGeneration))

		rs.Status = newStatus
		updatedRS, updateErr = c.UpdateStatus(context.TODO(), rs, metav1.UpdateOptions{})
		if updateErr == nil {
			return updatedRS, nil
		}
		// Stop retrying if we exceed statusUpdateRetries - the replicaSet will be requeued with a rate limit.
		if i >= statusUpdateRetries {
			break
		}
		// Update the ReplicaSet with the latest resource version for the next poll
		if rs, getErr = c.Get(context.TODO(), rs.Name, metav1.GetOptions{}); getErr != nil {
			// If the GET fails we can't trust status.Replicas anymore. This error
			// is bound to be more interesting than the update failure.
			return nil, getErr
		}
	}

	return nil, updateErr
}

func calculateStatus(rs *apps.ReplicaSet, activePods []*v1.Pod, terminatingPods []*v1.Pod, manageReplicasErr error) apps.ReplicaSetStatus {
	newStatus := rs.Status
	// Count the number of pods that have labels matching the labels of the pod
	// template of the replica set, the matching pods may have more
	// labels than are in the template. Because the label of podTemplateSpec is
	// a superset of the selector of the replica set, so the possible
	// matching pods must be part of the activePods.
	fullyLabeledReplicasCount := 0
	readyReplicasCount := 0
	availableReplicasCount := 0
	templateLabel := labels.Set(rs.Spec.Template.Labels).AsSelectorPreValidated()
	for _, pod := range activePods {
		if templateLabel.Matches(labels.Set(pod.Labels)) {
			fullyLabeledReplicasCount++
		}
		if podutil.IsPodReady(pod) {
			readyReplicasCount++
			if podutil.IsPodAvailable(pod, rs.Spec.MinReadySeconds, metav1.Now()) {
				availableReplicasCount++
			}
		}
	}

	var terminatingReplicasCount *int32
	if utilfeature.DefaultFeatureGate.Enabled(features.DeploymentPodReplacementPolicy) {
		terminatingReplicasCount = ptr.To(int32(len(terminatingPods)))
	}

	failureCond := GetCondition(rs.Status, apps.ReplicaSetReplicaFailure)
	if manageReplicasErr != nil && failureCond == nil {
		var reason string
		if diff := len(activePods) - int(*(rs.Spec.Replicas)); diff < 0 {
			reason = "FailedCreate"
		} else if diff > 0 {
			reason = "FailedDelete"
		}
		cond := NewReplicaSetCondition(apps.ReplicaSetReplicaFailure, v1.ConditionTrue, reason, manageReplicasErr.Error())
		SetCondition(&newStatus, cond)
	} else if manageReplicasErr == nil && failureCond != nil {
		RemoveCondition(&newStatus, apps.ReplicaSetReplicaFailure)
	} else if manageReplicasErr != nil {
		// Update the condition message if the error message has changed.
		UpdateCondition(&newStatus, apps.ReplicaSetReplicaFailure, manageReplicasErr.Error())
	}

	newStatus.Replicas = int32(len(activePods))
	newStatus.FullyLabeledReplicas = int32(fullyLabeledReplicasCount)
	newStatus.ReadyReplicas = int32(readyReplicasCount)
	newStatus.AvailableReplicas = int32(availableReplicasCount)
	newStatus.TerminatingReplicas = terminatingReplicasCount
	return newStatus
}

// NewReplicaSetCondition creates a new replicaset condition.
func NewReplicaSetCondition(condType apps.ReplicaSetConditionType, status v1.ConditionStatus, reason, msg string) apps.ReplicaSetCondition {
	return apps.ReplicaSetCondition{
		Type:               condType,
		Status:             status,
		LastTransitionTime: metav1.Now(),
		Reason:             reason,
		Message:            msg,
	}
}

// GetCondition returns a replicaset condition with the provided type if it exists.
func GetCondition(status apps.ReplicaSetStatus, condType apps.ReplicaSetConditionType) *apps.ReplicaSetCondition {
	for _, c := range status.Conditions {
		if c.Type == condType {
			return &c
		}
	}
	return nil
}

// SetCondition adds/replaces the given condition in the replicaset status. If the condition that we
// are about to add already exists and has the same status and reason then we are not going to update.
func SetCondition(status *apps.ReplicaSetStatus, condition apps.ReplicaSetCondition) {
	currentCond := GetCondition(*status, condition.Type)
	if currentCond != nil && currentCond.Status == condition.Status && currentCond.Reason == condition.Reason {
		return
	}
	newConditions := filterOutCondition(status.Conditions, condition.Type)
	status.Conditions = append(newConditions, condition)
}

// RemoveCondition removes the condition with the provided type from the replicaset status.
func RemoveCondition(status *apps.ReplicaSetStatus, condType apps.ReplicaSetConditionType) {
	status.Conditions = filterOutCondition(status.Conditions, condType)
}

// UpdateCondition Updates the conditional message of the provided type.
func UpdateCondition(status *apps.ReplicaSetStatus, condType apps.ReplicaSetConditionType, msg string) {
	currentCond := GetCondition(*status, condType)
	if currentCond == nil {
		return
	}
	if currentCond.Message == msg {
		return
	}
	newCond := NewReplicaSetCondition(condType, currentCond.Status, currentCond.Reason, msg)
	status.Conditions = filterOutCondition(status.Conditions, condType)
	status.Conditions = append(status.Conditions, newCond)
}

// filterOutCondition returns a new slice of replicaset conditions without conditions with the provided type.
func filterOutCondition(conditions []apps.ReplicaSetCondition, condType apps.ReplicaSetConditionType) []apps.ReplicaSetCondition {
	var newConditions []apps.ReplicaSetCondition
	for _, c := range conditions {
		if c.Type == condType {
			continue
		}
		newConditions = append(newConditions, c)
	}
	return newConditions
}

func derefInt32ToStr(ptr *int32) string {
	if ptr == nil {
		return "nil"
	}
	return fmt.Sprintf("%d", *ptr)
}
