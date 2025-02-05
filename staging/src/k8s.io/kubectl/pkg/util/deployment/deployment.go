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
	"sort"
	"strconv"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	intstrutil "k8s.io/apimachinery/pkg/util/intstr"
	runtimeresource "k8s.io/cli-runtime/pkg/resource"
	appsclient "k8s.io/client-go/kubernetes/typed/apps/v1"
)

const (
	// RevisionAnnotation is the revision annotation of a deployment's replica sets which records its rollout sequence
	RevisionAnnotation = "deployment.kubernetes.io/revision"
	// RevisionHistoryAnnotation maintains the history of all old revisions that a replica set has served for a deployment.
	RevisionHistoryAnnotation = "deployment.kubernetes.io/revision-history"
	// DesiredReplicasAnnotation is the desired replicas for a deployment recorded as an annotation
	// in its replica sets. Helps in separating scaling events from the rollout process and for
	// determining if the new replica set for a deployment is really saturated.
	// This value might reflect the desired replicas of an older deployment if the replica set has 0 replicas or if
	// the replica set is not fully scaled yet.
	DesiredReplicasAnnotation = "deployment.kubernetes.io/desired-replicas"
	// MaxReplicasAnnotation is the maximum replicas a deployment can have at a given point, which
	// is deployment.spec.replicas + maxSurge. This is added to the underlying replica sets so that the deployment
	// can estimate their proportions in case the deployment has surge replicas.
	// This value might reflect the maximum replicas of an older deployment if the replica set has 0 replicas or if
	// the replica set is not fully scaled yet.
	MaxReplicasAnnotation = "deployment.kubernetes.io/max-replicas"
	// ReplicaSetReplicasBeforeScaleAnnotation is the number of replicas a replica set had before scaling began.
	// The presence of this annotation indicates that a replica set has not yet fully scaled.
	// The value is added to the underlying replica sets so that the deployment can estimate their scale proportions
	// if the deployment has surge replicas (e.g. terminating pods) and the replica set is partially scaled due to the
	// deployment's .spec.podReplacementPolicy.
	ReplicaSetReplicasBeforeScaleAnnotation = "deployment.kubernetes.io/replicaset-replicas-before-scale"
	// RollbackRevisionNotFound is not found rollback event reason
	RollbackRevisionNotFound = "DeploymentRollbackRevisionNotFound"
	// RollbackTemplateUnchanged is the template unchanged rollback event reason
	RollbackTemplateUnchanged = "DeploymentRollbackTemplateUnchanged"
	// RollbackDone is the done rollback event reason
	RollbackDone = "DeploymentRollback"
	// TimedOutReason is added in a deployment when its newest replica set fails to show any progress
	// within the given deadline (progressDeadlineSeconds).
	TimedOutReason = "ProgressDeadlineExceeded"
)

// GetDeploymentCondition returns the condition with the provided type.
func GetDeploymentCondition(status appsv1.DeploymentStatus, condType appsv1.DeploymentConditionType) *appsv1.DeploymentCondition {
	for i := range status.Conditions {
		c := status.Conditions[i]
		if c.Type == condType {
			return &c
		}
	}
	return nil
}

// Revision returns the revision number of the input object.
func Revision(obj runtime.Object) (int64, error) {
	acc, err := meta.Accessor(obj)
	if err != nil {
		return 0, err
	}
	v, ok := acc.GetAnnotations()[RevisionAnnotation]
	if !ok {
		return 0, nil
	}
	return strconv.ParseInt(v, 10, 64)
}

// GetAllReplicaSets returns the old and new replica sets targeted by the given Deployment. It gets PodList and
// ReplicaSetList from client interface. Note that the first set of old replica sets doesn't include the ones
// with no pods, and the second set of old replica sets include all old replica sets. The third returned value
// is the new replica set, and it may be nil if it doesn't exist yet.
func GetAllReplicaSets(deployment *appsv1.Deployment, c appsclient.AppsV1Interface) ([]*appsv1.ReplicaSet, []*appsv1.ReplicaSet, *appsv1.ReplicaSet, error) {
	rsList, err := listReplicaSets(deployment, rsListFromClient(c), nil)
	if err != nil {
		return nil, nil, nil, err
	}
	newRS := findNewReplicaSet(deployment, rsList)
	oldRSes, allOldRSes := findOldReplicaSets(deployment, rsList, newRS)
	return oldRSes, allOldRSes, newRS, nil
}

// GetAllReplicaSetsInChunks is the same as GetAllReplicaSets, but accepts a chunk size argument.
// It returns the old and new replica sets targeted by the given Deployment. It gets PodList and
// ReplicaSetList from client interface. Note that the first set of old replica sets doesn't include the ones
// with no pods, and the second set of old replica sets include all old replica sets. The third returned value
// is the new replica set, and it may be nil if it doesn't exist yet.
func GetAllReplicaSetsInChunks(deployment *appsv1.Deployment, c appsclient.AppsV1Interface, chunkSize int64) ([]*appsv1.ReplicaSet, []*appsv1.ReplicaSet, *appsv1.ReplicaSet, error) {
	rsList, err := listReplicaSets(deployment, rsListFromClient(c), &chunkSize)
	if err != nil {
		return nil, nil, nil, err
	}
	newRS := findNewReplicaSet(deployment, rsList)
	oldRSes, allOldRSes := findOldReplicaSets(deployment, rsList, newRS)
	return oldRSes, allOldRSes, newRS, nil
}

// RsListFromClient returns an rsListFunc that wraps the given client.
func rsListFromClient(c appsclient.AppsV1Interface) rsListFunc {
	return func(namespace string, initialOpts metav1.ListOptions) ([]*appsv1.ReplicaSet, error) {
		rsList := &appsv1.ReplicaSetList{}
		err := runtimeresource.FollowContinue(&initialOpts,
			func(opts metav1.ListOptions) (runtime.Object, error) {
				newRs, err := c.ReplicaSets(namespace).List(context.TODO(), opts)
				if err != nil {
					return nil, runtimeresource.EnhanceListError(err, opts, "replicasets")
				}
				rsList.Items = append(rsList.Items, newRs.Items...)
				return newRs, nil
			})
		if err != nil {
			return nil, err
		}
		var ret []*appsv1.ReplicaSet
		for i := range rsList.Items {
			ret = append(ret, &rsList.Items[i])
		}
		return ret, err
	}
}

// TODO: switch this to full namespacers
type rsListFunc func(string, metav1.ListOptions) ([]*appsv1.ReplicaSet, error)

// listReplicaSets returns a slice of RSes the given deployment targets.
// Note that this does NOT attempt to reconcile ControllerRef (adopt/orphan),
// because only the controller itself should do that.
// However, it does filter out anything whose ControllerRef doesn't match.
func listReplicaSets(deployment *appsv1.Deployment, getRSList rsListFunc, chunkSize *int64) ([]*appsv1.ReplicaSet, error) {
	// TODO: Right now we list replica sets by their labels. We should list them by selector, i.e. the replica set's selector
	//       should be a superset of the deployment's selector, see https://github.com/kubernetes/kubernetes/issues/19830.
	namespace := deployment.Namespace
	selector, err := metav1.LabelSelectorAsSelector(deployment.Spec.Selector)
	if err != nil {
		return nil, err
	}
	options := metav1.ListOptions{LabelSelector: selector.String()}
	if chunkSize != nil {
		options.Limit = *chunkSize
	}
	all, err := getRSList(namespace, options)
	if err != nil {
		return nil, err
	}
	// Only include those whose ControllerRef matches the Deployment.
	owned := make([]*appsv1.ReplicaSet, 0, len(all))
	for _, rs := range all {
		if metav1.IsControlledBy(rs, deployment) {
			owned = append(owned, rs)
		}
	}
	return owned, nil
}

// EqualIgnoreHash returns true if two given podTemplateSpec are equal, ignoring the diff in value of Labels[pod-template-hash]
// We ignore pod-template-hash because:
//  1. The hash result would be different upon podTemplateSpec API changes
//     (e.g. the addition of a new field will cause the hash code to change)
//  2. The deployment template won't have hash labels
func equalIgnoreHash(template1, template2 *corev1.PodTemplateSpec) bool {
	t1Copy := template1.DeepCopy()
	t2Copy := template2.DeepCopy()
	// Remove hash labels from template.Labels before comparing
	delete(t1Copy.Labels, appsv1.DefaultDeploymentUniqueLabelKey)
	delete(t2Copy.Labels, appsv1.DefaultDeploymentUniqueLabelKey)
	return apiequality.Semantic.DeepEqual(t1Copy, t2Copy)
}

// FindNewReplicaSet returns the new RS this given deployment targets (the one with the same pod template).
func findNewReplicaSet(deployment *appsv1.Deployment, rsList []*appsv1.ReplicaSet) *appsv1.ReplicaSet {
	sort.Sort(replicaSetsByCreationTimestamp(rsList))
	for i := range rsList {
		if equalIgnoreHash(&rsList[i].Spec.Template, &deployment.Spec.Template) {
			// In rare cases, such as after cluster upgrades, Deployment may end up with
			// having more than one new ReplicaSets that have the same template as its template,
			// see https://github.com/kubernetes/kubernetes/issues/40415
			// We deterministically choose the oldest new ReplicaSet.
			return rsList[i]
		}
	}
	// new ReplicaSet does not exist.
	return nil
}

// replicaSetsByCreationTimestamp sorts a list of ReplicaSet by creation timestamp, using their names as a tie breaker.
type replicaSetsByCreationTimestamp []*appsv1.ReplicaSet

func (o replicaSetsByCreationTimestamp) Len() int      { return len(o) }
func (o replicaSetsByCreationTimestamp) Swap(i, j int) { o[i], o[j] = o[j], o[i] }
func (o replicaSetsByCreationTimestamp) Less(i, j int) bool {
	if o[i].CreationTimestamp.Equal(&o[j].CreationTimestamp) {
		return o[i].Name < o[j].Name
	}
	return o[i].CreationTimestamp.Before(&o[j].CreationTimestamp)
}

// // FindOldReplicaSets returns the old replica sets targeted by the given Deployment, with the given slice of RSes.
// // Note that the first set of old replica sets doesn't include the ones with no pods, and the second set of old replica sets include all old replica sets.
func findOldReplicaSets(deployment *appsv1.Deployment, rsList []*appsv1.ReplicaSet, newRS *appsv1.ReplicaSet) ([]*appsv1.ReplicaSet, []*appsv1.ReplicaSet) {
	var requiredRSs []*appsv1.ReplicaSet
	var allRSs []*appsv1.ReplicaSet
	for _, rs := range rsList {
		// Filter out new replica set
		if newRS != nil && rs.UID == newRS.UID {
			continue
		}
		allRSs = append(allRSs, rs)
		if *(rs.Spec.Replicas) != 0 {
			requiredRSs = append(requiredRSs, rs)
		}
	}
	return requiredRSs, allRSs
}

// ResolveFenceposts resolves both maxSurge and maxUnavailable. This needs to happen in one
// step. For example:
//
// 2 desired, max unavailable 1%, surge 0% - should scale old(-1), then new(+1), then old(-1), then new(+1)
// 1 desired, max unavailable 1%, surge 0% - should scale old(-1), then new(+1)
// 2 desired, max unavailable 25%, surge 1% - should scale new(+1), then old(-1), then new(+1), then old(-1)
// 1 desired, max unavailable 25%, surge 1% - should scale new(+1), then old(-1)
// 2 desired, max unavailable 0%, surge 1% - should scale new(+1), then old(-1), then new(+1), then old(-1)
// 1 desired, max unavailable 0%, surge 1% - should scale new(+1), then old(-1)
func ResolveFenceposts(maxSurge, maxUnavailable *intstrutil.IntOrString, desired int32) (int32, int32, error) {
	surge, err := intstrutil.GetScaledValueFromIntOrPercent(intstrutil.ValueOrDefault(maxSurge, intstrutil.FromInt32(0)), int(desired), true)
	if err != nil {
		return 0, 0, err
	}
	unavailable, err := intstrutil.GetScaledValueFromIntOrPercent(intstrutil.ValueOrDefault(maxUnavailable, intstrutil.FromInt32(0)), int(desired), false)
	if err != nil {
		return 0, 0, err
	}

	if surge == 0 && unavailable == 0 {
		// Validation should never allow the user to explicitly use zero values for both maxSurge
		// maxUnavailable. Due to rounding down maxUnavailable though, it may resolve to zero.
		// If both fenceposts resolve to zero, then we should set maxUnavailable to 1 on the
		// theory that surge might not work due to quota.
		unavailable = 1
	}

	return int32(surge), int32(unavailable), nil
}
