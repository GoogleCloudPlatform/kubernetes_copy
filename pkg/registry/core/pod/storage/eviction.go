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

package storage

import (
	"context"
	"fmt"
	"reflect"
	"time"

	policyv1 "k8s.io/api/policy/v1"
	policyv1beta1 "k8s.io/api/policy/v1beta1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apiserver/pkg/registry/rest"
	"k8s.io/apiserver/pkg/util/dryrun"
	policyclient "k8s.io/client-go/kubernetes/typed/policy/v1"
	"k8s.io/client-go/util/retry"
	pdbhelper "k8s.io/component-helpers/apps/poddisruptionbudget"
	podutil "k8s.io/kubernetes/pkg/api/pod"
	api "k8s.io/kubernetes/pkg/apis/core"
	"k8s.io/kubernetes/pkg/apis/policy"

	"k8s.io/utils/ptr"
)

const (
	// MaxDisruptedPodSize is the max size of PodDisruptionBudgetStatus.DisruptedPods. API server eviction
	// subresource handler will refuse to evict pods covered by the corresponding PDB
	// if the size of the map exceeds this value. It means a large number of
	// evictions have been approved by the API server but not noticed by the PDB controller yet.
	// This situation should self-correct because the PDB controller removes
	// entries from the map automatically after the PDB DeletionTimeout regardless.
	MaxDisruptedPodSize = 2000
)

// EvictionsRetry is the retry for a conflict where multiple clients
// are making changes to the same resource.
var EvictionsRetry = wait.Backoff{
	Steps:    20,
	Duration: 500 * time.Millisecond,
	Factor:   1.0,
	Jitter:   0.1,
}

func newEvictionStorage(store rest.StandardStorage, podDisruptionBudgetClient policyclient.PodDisruptionBudgetsGetter) *EvictionREST {
	return &EvictionREST{store: store, podDisruptionBudgetClient: podDisruptionBudgetClient}
}

// EvictionREST implements the REST endpoint for evicting pods from nodes
type EvictionREST struct {
	store                     rest.StandardStorage
	podDisruptionBudgetClient policyclient.PodDisruptionBudgetsGetter
}

var _ = rest.NamedCreater(&EvictionREST{})
var _ = rest.GroupVersionKindProvider(&EvictionREST{})
var _ = rest.GroupVersionAcceptor(&EvictionREST{})

var v1Eviction = schema.GroupVersionKind{Group: "policy", Version: "v1", Kind: "Eviction"}

// GroupVersionKind specifies a particular GroupVersionKind to discovery
func (r *EvictionREST) GroupVersionKind(containingGV schema.GroupVersion) schema.GroupVersionKind {
	return v1Eviction
}

// AcceptsGroupVersion indicates both v1 and v1beta1 Eviction objects are acceptable
func (r *EvictionREST) AcceptsGroupVersion(gv schema.GroupVersion) bool {
	switch gv {
	case policyv1.SchemeGroupVersion, policyv1beta1.SchemeGroupVersion:
		return true
	default:
		return false
	}
}

// New creates a new eviction resource
func (r *EvictionREST) New() runtime.Object {
	return &policy.Eviction{}
}

// Destroy cleans up resources on shutdown.
func (r *EvictionREST) Destroy() {
	// Given that underlying store is shared with REST,
	// we don't destroy it here explicitly.
}

// Propagate dry-run takes the dry-run option from the request and pushes it into the eviction object.
// It returns an error if they have non-matching dry-run options.
func propagateDryRun(eviction *policy.Eviction, options *metav1.CreateOptions) (*metav1.DeleteOptions, error) {
	if eviction.DeleteOptions == nil {
		return &metav1.DeleteOptions{DryRun: options.DryRun}, nil
	}
	if len(eviction.DeleteOptions.DryRun) == 0 {
		eviction.DeleteOptions.DryRun = options.DryRun
		return eviction.DeleteOptions, nil
	}
	if len(options.DryRun) == 0 {
		return eviction.DeleteOptions, nil
	}

	if !reflect.DeepEqual(options.DryRun, eviction.DeleteOptions.DryRun) {
		return nil, fmt.Errorf("Non-matching dry-run options in request and content: %v and %v", options.DryRun, eviction.DeleteOptions.DryRun)
	}
	return eviction.DeleteOptions, nil
}

// Create attempts to create a new eviction.  That is, it tries to evict a pod.
func (r *EvictionREST) Create(ctx context.Context, name string, obj runtime.Object, createValidation rest.ValidateObjectFunc, options *metav1.CreateOptions) (runtime.Object, error) {
	eviction, ok := obj.(*policy.Eviction)
	if !ok {
		return nil, errors.NewBadRequest(fmt.Sprintf("not a Eviction object: %T", obj))
	}

	if name != eviction.Name {
		return nil, errors.NewBadRequest("name in URL does not match name in Eviction object")
	}

	if eviction.DeleteOptions != nil && ptr.Deref(eviction.DeleteOptions.IgnoreStoreReadErrorWithClusterBreakingPotential, false) {
		errs := field.ErrorList{
			field.Invalid(field.NewPath("deleteOptions").Child("ignoreStoreReadErrorWithClusterBreakingPotential"), true, "can not be set for pod eviction, try after removing the option"),
		}
		return nil, errors.NewInvalid(v1Eviction.GroupKind(), name, errs)
	}

	originalDeleteOptions, err := propagateDryRun(eviction, options)
	if err != nil {
		return nil, err
	}

	if createValidation != nil {
		if err := createValidation(ctx, eviction.DeepCopyObject()); err != nil {
			return nil, err
		}
	}

	var pod *api.Pod
	deletedPod := false
	// by default, retry conflict errors
	shouldRetry := errors.IsConflict
	if !resourceVersionIsUnset(originalDeleteOptions) {
		// if the original options included a resourceVersion precondition, don't retry
		shouldRetry = func(err error) bool { return false }
	}

	err = retry.OnError(EvictionsRetry, shouldRetry, func() error {
		pod, err = getPod(r, ctx, eviction.Name)
		if err != nil {
			return err
		}

		// Evicting a terminal pod should result in direct deletion of pod as it already caused disruption by the time we are evicting.
		// There is no need to check for pdb.
		if !canIgnorePDB(pod) {
			// Pod is not in a state where we can skip checking PDBs, exit the loop, and continue to PDB checks.
			return nil
		}

		// the PDB can be ignored, so delete the pod
		deleteOptions := originalDeleteOptions

		// We should check if resourceVersion is already set by the requestor
		// as it might be older than the pod we just fetched and should be
		// honored.
		if shouldEnforceResourceVersion(pod) && resourceVersionIsUnset(originalDeleteOptions) {
			// Set deleteOptions.Preconditions.ResourceVersion to ensure we're not
			// racing with another PDB-impacting process elsewhere.
			deleteOptions = deleteOptions.DeepCopy()
			setPreconditionsResourceVersion(deleteOptions, &pod.ResourceVersion)
		}
		err = addConditionAndDeletePod(r, ctx, eviction.Name, rest.ValidateAllObjectFunc, deleteOptions)
		if err != nil {
			return err
		}
		deletedPod = true
		return nil
	})
	switch {
	case err != nil:
		// this can happen in cases where the PDB can be ignored, but there was a problem issuing the pod delete:
		// maybe we conflicted too many times or we didn't have permission or something else weird.
		return nil, err

	case deletedPod:
		// this happens when we successfully deleted the pod.  In this case, we're done executing because we've evicted/deleted the pod
		return &metav1.Status{Status: metav1.StatusSuccess}, nil

	default:
		// this happens when we didn't have an error and we didn't delete the pod. The only branch that happens on is when
		// we cannot ignored the PDB for this pod, so this is the fall through case.
	}

	var rtStatus *metav1.Status
	var pdbName string
	updateDeletionOptions := false

	err = func() error {
		pdbs, err := r.getPodDisruptionBudgets(ctx, pod)
		if err != nil {
			return err
		}

		if len(pdbs) > 1 {
			rtStatus = &metav1.Status{
				Status:  metav1.StatusFailure,
				Message: "This pod has more than one PodDisruptionBudget, which the eviction subresource does not support.",
				Code:    500,
			}
			return nil
		}
		if len(pdbs) == 0 {
			return nil
		}

		pdb := &pdbs[0]
		pdbName = pdb.Name

		// IsPodReady is the current implementation of IsHealthy
		// If the pod is healthy, it should be guarded by the PDB.
		if !podutil.IsPodReady(pod) {
			if pdb.Spec.UnhealthyPodEvictionPolicy != nil && *pdb.Spec.UnhealthyPodEvictionPolicy == policyv1.AlwaysAllow {
				// Delete the unhealthy pod, it doesn't count towards currentHealthy and desiredHealthy and we should not decrement disruptionsAllowed.
				updateDeletionOptions = true
				return nil
			}
			// default nil and IfHealthyBudget policy
			if pdb.Status.CurrentHealthy >= pdb.Status.DesiredHealthy && pdb.Status.DesiredHealthy > 0 {
				// Delete the unhealthy pod, it doesn't count towards currentHealthy and desiredHealthy and we should not decrement disruptionsAllowed.
				// Application guarded by the PDB is not disrupted at the moment and deleting unhealthy (unready) pod will not disrupt it.
				updateDeletionOptions = true
				return nil
			}
			// confirm no disruptions allowed in checkAndDecrement
		}

		refresh := false
		err = retry.RetryOnConflict(EvictionsRetry, func() error {
			if refresh {
				pdb, err = r.podDisruptionBudgetClient.PodDisruptionBudgets(pod.Namespace).Get(context.TODO(), pdbName, metav1.GetOptions{})
				if err != nil {
					return err
				}
			}
			// Try to verify-and-decrement

			// If it was false already, or if it becomes false during the course of our retries,
			// raise an error marked as a 429.
			if err = r.checkAndDecrement(pod.Namespace, pod.Name, *pdb, dryrun.IsDryRun(originalDeleteOptions.DryRun)); err != nil {
				refresh = true
				if errors.IsForbidden(err) { //need a feature gate?
					getLatestPod := latestPodFactory(originalDeleteOptions) //is this right deletion options?

					podUpdatedObjectInfo := rest.DefaultUpdatedObjectInfo(nil, getLatestPod, appendCondition("EvictionBlocked", "Eviction API: evicting")) // order important
					//do we need to give this context a timeout so we still 429 even if this fails
					_, _, updateErr := r.store.Update(ctx, pod.Name, podUpdatedObjectInfo, rest.ValidateAllObjectFunc, rest.ValidateAllObjectUpdateFunc, false, &metav1.UpdateOptions{})
					if updateErr != nil {
						//just log here? or actually mutate err to append details?
						statusErr := err.(*errors.StatusError)
						statusErr.ErrStatus.Message = fmt.Sprintf("%s and failed to updated pod conditon: %s", statusErr.ErrStatus.Message, err.Error())
					}
				}
				return err
			}
			return nil
		})
		return err
	}()
	if err == wait.ErrWaitTimeout {
		err = errors.NewTimeoutError(fmt.Sprintf("couldn't update PodDisruptionBudget %q due to conflicts", pdbName), 10)
	}
	if err != nil {
		return nil, err
	}

	if rtStatus != nil {
		return rtStatus, nil
	}

	// At this point there was either no PDB or we succeeded in decrementing or
	// the pod was unhealthy (unready) and we have enough healthy replicas

	deleteOptions := originalDeleteOptions

	// Set deleteOptions.Preconditions.ResourceVersion to ensure
	// the pod hasn't been considered healthy (ready) since we calculated
	if updateDeletionOptions {
		// Take a copy so we can compare to client-provied Options later.
		deleteOptions = deleteOptions.DeepCopy()
		setPreconditionsResourceVersion(deleteOptions, &pod.ResourceVersion)
	}

	// Try the delete
	err = addConditionAndDeletePod(r, ctx, eviction.Name, rest.ValidateAllObjectFunc, deleteOptions)
	if err != nil {
		if errors.IsConflict(err) && updateDeletionOptions &&
			(originalDeleteOptions.Preconditions == nil || originalDeleteOptions.Preconditions.ResourceVersion == nil) {
			// If we encounter a resource conflict error, we updated the deletion options to include them,
			// and the original deletion options did not specify ResourceVersion, we send back
			// TooManyRequests so clients will retry.
			return nil, createTooManyRequestsError(pdbName)
		}
		return nil, err
	}

	// Success!
	return &metav1.Status{Status: metav1.StatusSuccess}, nil
}

func latestPodFactory(options *metav1.DeleteOptions) rest.TransformFunc {
	// Throwaway the newObj. We care only about the latest pod obtained from etcd (oldObj).
	// So we can add DisruptionTarget condition in conditionAppender without conflicts.
	return func(_ context.Context, _, oldObj runtime.Object) (runtime.Object, error) {
		latestPod := oldObj.(*api.Pod).DeepCopy()
		if options.Preconditions != nil {
			if uid := options.Preconditions.UID; uid != nil && len(*uid) > 0 && *uid != latestPod.UID {
				return nil, errors.NewConflict(
					schema.GroupResource{Group: "", Resource: "Pod"},
					latestPod.Name,
					fmt.Errorf("the UID in the precondition (%s) does not match the UID in record (%s). The object might have been deleted and then recreated", *uid, latestPod.UID),
				)
			}
			if rv := options.Preconditions.ResourceVersion; rv != nil && len(*rv) > 0 && *rv != latestPod.ResourceVersion {
				return nil, errors.NewConflict(
					schema.GroupResource{Group: "", Resource: "Pod"},
					latestPod.Name,
					fmt.Errorf("the ResourceVersion in the precondition (%s) does not match the ResourceVersion in record (%s). The object might have been modified", *rv, latestPod.ResourceVersion),
				)
			}
		}
		return latestPod, nil
	}
}

func appendCondition(reason, msg string) rest.TransformFunc {
	return func(_ context.Context, newObj, _ runtime.Object) (runtime.Object, error) {
		podObj := newObj.(*api.Pod)
		podutil.UpdatePodCondition(&podObj.Status, &api.PodCondition{
			Type:    api.DisruptionTarget,
			Status:  api.ConditionTrue,
			Reason:  reason,
			Message: msg,
		})
		return podObj, nil
	}
}

func addConditionAndDeletePod(r *EvictionREST, ctx context.Context, name string, validation rest.ValidateObjectFunc, options *metav1.DeleteOptions) error {
	if !dryrun.IsDryRun(options.DryRun) {
		getLatestPod := latestPodFactory(options)
		podUpdatedObjectInfo := rest.DefaultUpdatedObjectInfo(nil, getLatestPod, appendCondition("EvictionByEvictionAPI", "Eviction API: evicting")) // order important

		updatedPodObject, _, err := r.store.Update(ctx, name, podUpdatedObjectInfo, rest.ValidateAllObjectFunc, rest.ValidateAllObjectUpdateFunc, false, &metav1.UpdateOptions{})
		if err != nil {
			return err
		}

		if !resourceVersionIsUnset(options) {
			newResourceVersion, err := meta.NewAccessor().ResourceVersion(updatedPodObject)
			if err != nil {
				return err
			}
			// bump the resource version, since we are the one who modified it via the update
			options = options.DeepCopy()
			options.Preconditions.ResourceVersion = &newResourceVersion
		}
	}
	_, _, err := r.store.Delete(ctx, name, rest.ValidateAllObjectFunc, options)
	return err
}

func getPod(r *EvictionREST, ctx context.Context, name string) (*api.Pod, error) {
	obj, err := r.store.Get(ctx, name, &metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	return obj.(*api.Pod), nil
}

func setPreconditionsResourceVersion(deleteOptions *metav1.DeleteOptions, resourceVersion *string) {
	if deleteOptions.Preconditions == nil {
		deleteOptions.Preconditions = &metav1.Preconditions{}
	}
	deleteOptions.Preconditions.ResourceVersion = resourceVersion
}

// canIgnorePDB returns true for pod conditions that allow the pod to be deleted
// without checking PDBs.
func canIgnorePDB(pod *api.Pod) bool {
	if pod.Status.Phase == api.PodSucceeded || pod.Status.Phase == api.PodFailed ||
		pod.Status.Phase == api.PodPending || !pod.ObjectMeta.DeletionTimestamp.IsZero() {
		return true
	}
	return false
}

func shouldEnforceResourceVersion(pod *api.Pod) bool {
	// We don't need to enforce ResourceVersion for terminal pods
	if pod.Status.Phase == api.PodSucceeded || pod.Status.Phase == api.PodFailed || !pod.ObjectMeta.DeletionTimestamp.IsZero() {
		return false
	}
	// Return true for all other pods to ensure we don't race against a pod becoming
	// healthy (ready) and violating PDBs.
	return true
}

func resourceVersionIsUnset(options *metav1.DeleteOptions) bool {
	return options.Preconditions == nil || options.Preconditions.ResourceVersion == nil
}

func createTooManyRequestsError(name string) error {
	// TODO: Once there are time-based
	// budgets, we can sometimes compute a sensible suggested value.  But
	// even without that, we can give a suggestion (even if small) that
	// prevents well-behaved clients from hammering us.
	err := errors.NewTooManyRequests("Cannot evict pod as it would violate the pod's disruption budget.", 10)
	err.ErrStatus.Details.Causes = append(err.ErrStatus.Details.Causes, metav1.StatusCause{Type: policyv1.DisruptionBudgetCause, Message: fmt.Sprintf("The disruption budget %s is still being processed by the server.", name)})
	return err
}

// checkAndDecrement checks if the provided PodDisruptionBudget allows any disruption.
// getting a bit long
func (r *EvictionREST) checkAndDecrement(namespace string, podName string, pdb policyv1.PodDisruptionBudget, dryRun bool) error {
	if pdb.Status.ObservedGeneration < pdb.Generation {

		return createTooManyRequestsError(pdb.Name)
	}
	if pdb.Status.DisruptionsAllowed < 0 {
		return errors.NewForbidden(policy.Resource("poddisruptionbudget"), pdb.Name, fmt.Errorf("pdb disruptions allowed is negative"))
	}
	if len(pdb.Status.DisruptedPods) > MaxDisruptedPodSize {
		return errors.NewForbidden(policy.Resource("poddisruptionbudget"), pdb.Name, fmt.Errorf("DisruptedPods map too big - too many evictions not confirmed by PDB controller"))
	}
	if pdb.Status.DisruptionsAllowed == 0 {
		//would it be good if this included  the pdb name?
		err := errors.NewTooManyRequests("Cannot evict pod as it would violate the pod's disruption budget.", 0)
		err.ErrStatus.Details.Causes = append(err.ErrStatus.Details.Causes, metav1.StatusCause{Type: policyv1.DisruptionBudgetCause, Message: fmt.Sprintf("The disruption budget %s needs %d healthy pods and has %d currently", pdb.Name, pdb.Status.DesiredHealthy, pdb.Status.CurrentHealthy)})
		return err
	}

	pdb.Status.DisruptionsAllowed--
	if pdb.Status.DisruptionsAllowed == 0 {
		pdbhelper.UpdateDisruptionAllowedCondition(&pdb)
	}

	// If this is a dry-run, we don't need to go any further than that.
	if dryRun {
		return nil
	}

	if pdb.Status.DisruptedPods == nil {
		pdb.Status.DisruptedPods = make(map[string]metav1.Time)
	}

	// Eviction handler needs to inform the PDB controller that it is about to delete a pod
	// so it should not consider it as available in calculations when updating PodDisruptions allowed.
	// If the pod is not deleted within a reasonable time limit PDB controller will assume that it won't
	// be deleted at all and remove it from DisruptedPod map.
	pdb.Status.DisruptedPods[podName] = metav1.Time{Time: time.Now()}
	if _, err := r.podDisruptionBudgetClient.PodDisruptionBudgets(namespace).UpdateStatus(context.TODO(), &pdb, metav1.UpdateOptions{}); err != nil {
		return err
	}

	return nil
}

// getPodDisruptionBudgets returns any PDBs that match the pod or err if there's an error.
func (r *EvictionREST) getPodDisruptionBudgets(ctx context.Context, pod *api.Pod) ([]policyv1.PodDisruptionBudget, error) {
	pdbList, err := r.podDisruptionBudgetClient.PodDisruptionBudgets(pod.Namespace).List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return nil, err
	}

	var pdbs []policyv1.PodDisruptionBudget
	for _, pdb := range pdbList.Items {
		if pdb.Namespace != pod.Namespace {
			continue
		}
		selector, err := metav1.LabelSelectorAsSelector(pdb.Spec.Selector)
		if err != nil {
			// This object has an invalid selector, it does not match the pod
			continue
		}
		if !selector.Matches(labels.Set(pod.Labels)) {
			continue
		}

		pdbs = append(pdbs, pdb)
	}

	return pdbs, nil
}
