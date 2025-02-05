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

package resourcequota

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"k8s.io/klog/v2"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apiserver/pkg/admission"
	resourcequotaapi "k8s.io/apiserver/pkg/admission/plugin/resourcequota/apis/resourcequota"
	quota "k8s.io/apiserver/pkg/quota/v1"
	"k8s.io/apiserver/pkg/quota/v1/generic"
	"k8s.io/client-go/util/workqueue"
)

// Evaluator is used to see if quota constraints are satisfied.
type Evaluator interface {
	// Evaluate takes an operation and checks to see if quota constraints are satisfied.  It returns an error if they are not.
	// The default implementation processes related operations in chunks when possible.
	Evaluate(a admission.Attributes) error
}

type quotaEvaluator struct {
	quotaAccessor QuotaAccessor
	// lockAcquisitionFunc acquires any required locks and returns a cleanup method to defer
	lockAcquisitionFunc func([]corev1.ResourceQuota) func()

	ignoredResources map[schema.GroupResource]struct{}

	// registry that knows how to measure usage for objects
	registry quota.Registry

	// TODO these are used together to bucket items by namespace and then batch them up for processing.
	// The technique is valuable for rollup activities to avoid fanout and reduce resource contention.
	// We could move this into a library if another component needed it.
	// queue is indexed by namespace, so that we bundle up on a per-namespace basis
	queue      *workqueue.Typed[string]
	workLock   sync.Mutex
	work       map[string][]*admissionWaiter
	dirtyWork  map[string][]*admissionWaiter
	inProgress sets.String

	// controls the run method so that we can cleanly conform to the Evaluator interface
	workers int
	stopCh  <-chan struct{}
	init    sync.Once

	// lets us know what resources are limited by default
	config *resourcequotaapi.Configuration
}

type admissionWaiter struct {
	attributes admission.Attributes
	finished   chan struct{}
	result     error
}

type defaultDeny struct{}

func (defaultDeny) Error() string {
	return "DEFAULT DENY"
}

// IsDefaultDeny returns true if the error is defaultDeny
func IsDefaultDeny(err error) bool {
	if err == nil {
		return false
	}

	_, ok := err.(defaultDeny)
	return ok
}

func newAdmissionWaiter(a admission.Attributes) *admissionWaiter {
	return &admissionWaiter{
		attributes: a,
		finished:   make(chan struct{}),
		result:     defaultDeny{},
	}
}

// NewQuotaEvaluator configures an admission controller that can enforce quota constraints
// using the provided registry.  The registry must have the capability to handle group/kinds that
// are persisted by the server this admission controller is intercepting
func NewQuotaEvaluator(quotaAccessor QuotaAccessor, ignoredResources map[schema.GroupResource]struct{}, quotaRegistry quota.Registry, lockAcquisitionFunc func([]corev1.ResourceQuota) func(), config *resourcequotaapi.Configuration, workers int, stopCh <-chan struct{}) Evaluator {
	// if we get a nil config, just create an empty default.
	if config == nil {
		config = &resourcequotaapi.Configuration{}
	}

	evaluator := &quotaEvaluator{
		quotaAccessor:       quotaAccessor,
		lockAcquisitionFunc: lockAcquisitionFunc,

		ignoredResources: ignoredResources,
		registry:         quotaRegistry,

		queue:      workqueue.NewTypedWithConfig(workqueue.TypedQueueConfig[string]{Name: "admission_quota_controller"}),
		work:       map[string][]*admissionWaiter{},
		dirtyWork:  map[string][]*admissionWaiter{},
		inProgress: sets.String{},

		workers: workers,
		stopCh:  stopCh,
		config:  config,
	}

	// The queue underneath is starting a goroutine for metrics
	// exporting that is only stopped on calling ShutDown.
	// Given that QuotaEvaluator is created for each layer of apiserver
	// and often not started for some of those (e.g. aggregated apiserver)
	// we explicitly shut it down on stopCh signal even if it wasn't
	// effectively started.
	go evaluator.shutdownOnStop()

	return evaluator
}

// start begins watching and syncing.
func (e *quotaEvaluator) start() {
	defer utilruntime.HandleCrash()

	for i := 0; i < e.workers; i++ {
		go wait.Until(e.doWork, time.Second, e.stopCh)
	}
}

func (e *quotaEvaluator) shutdownOnStop() {
	<-e.stopCh
	klog.Infof("Shutting down quota evaluator")
	e.queue.ShutDown()
}

func (e *quotaEvaluator) doWork() {
	workFunc := func() bool {
		ns, admissionAttributes, quit := e.getWork()
		if quit {
			return true
		}
		defer e.completeWork(ns)
		if len(admissionAttributes) == 0 {
			return false
		}
		e.checkAttributes(ns, admissionAttributes)
		return false
	}
	for {
		if quit := workFunc(); quit {
			klog.Infof("quota evaluator worker shutdown")
			return
		}
	}
}

// checkAttributes iterates evaluates all the waiting admissionAttributes.  It will always notify all waiters
// before returning.  The default is to deny.
func (e *quotaEvaluator) checkAttributes(ns string, admissionAttributes []*admissionWaiter) {
	// notify all on exit
	defer func() {
		for _, admissionAttribute := range admissionAttributes {
			close(admissionAttribute.finished)
		}
	}()

	quotas, err := e.quotaAccessor.GetQuotas(ns)
	if err != nil {
		for _, admissionAttribute := range admissionAttributes {
			admissionAttribute.result = err
		}
		return
	}
	// if limited resources are disabled, we can just return safely when there are no quotas.
	limitedResourcesDisabled := len(e.config.LimitedResources) == 0
	if len(quotas) == 0 && limitedResourcesDisabled {
		for _, admissionAttribute := range admissionAttributes {
			admissionAttribute.result = nil
		}
		return
	}

	if e.lockAcquisitionFunc != nil {
		releaseLocks := e.lockAcquisitionFunc(quotas)
		defer releaseLocks()
	}

	e.checkQuotas(quotas, admissionAttributes, 3)
}

// checkQuotas checks the admission attributes against the passed quotas.  If a quota applies, it will attempt to update it
// AFTER it has checked all the admissionAttributes.  The method breaks down into phase like this:
//  0. make a copy of the quotas to act as a "running" quota so we know what we need to update and can still compare against the
//     originals
//  1. check each admission attribute to see if it fits within *all* the quotas.  If it didn't fit, mark the waiter as failed
//     and the running quota doesn't change.  If it did fit, check to see if any quota was changed.  If there was no quota change
//     mark the waiter as succeeded.  If some quota did change, update the running quotas
//  2. If no running quota was changed, return now since no updates are needed.
//  3. for each quota that has changed, attempt an update.  If all updates succeeded, update all unset waiters to success status and return.  If the some
//     updates failed on conflict errors and we have retries left, re-get the failed quota from our cache for the latest version
//     and recurse into this method with the subset.  It's safe for us to evaluate ONLY the subset, because the other quota
//     documents for these waiters have already been evaluated.  Step 1, will mark all the ones that should already have succeeded.
func (e *quotaEvaluator) checkQuotas(quotas []corev1.ResourceQuota, admissionAttributes []*admissionWaiter, remainingRetries int) {
	// yet another copy to compare against originals to see if we actually have deltas
	originalQuotas, err := copyQuotas(quotas)
	if err != nil {
		utilruntime.HandleError(err)
		return
	}

	atLeastOneChanged := false
	for i := range admissionAttributes {
		admissionAttribute := admissionAttributes[i]
		newQuotas, err := e.checkRequest(quotas, admissionAttribute.attributes)
		if err != nil {
			admissionAttribute.result = err
			continue
		}

		// Don't update quota for admissionAttributes that correspond to dry-run requests
		if admissionAttribute.attributes.IsDryRun() {
			admissionAttribute.result = nil
			continue
		}

		// if the new quotas are the same as the old quotas, then this particular one doesn't issue any updates
		// that means that no quota docs applied, so it can get a pass
		atLeastOneChangeForThisWaiter := false
		for j := range newQuotas {
			if !quota.Equals(quotas[j].Status.Used, newQuotas[j].Status.Used) {
				atLeastOneChanged = true
				atLeastOneChangeForThisWaiter = true
				break
			}
		}

		if !atLeastOneChangeForThisWaiter {
			admissionAttribute.result = nil
		}

		quotas = newQuotas
	}

	// if none of the requests changed anything, there's no reason to issue an update, just fail them all now
	if !atLeastOneChanged {
		return
	}

	// now go through and try to issue updates.  Things get a little weird here:
	// 1. check to see if the quota changed.  If not, skip.
	// 2. if the quota changed and the update passes, be happy
	// 3. if the quota changed and the update fails, add the original to a retry list
	var updatedFailedQuotas []corev1.ResourceQuota
	var lastErr error
	for i := range quotas {
		newQuota := quotas[i]

		// if this quota didn't have its status changed, skip it
		if quota.Equals(originalQuotas[i].Status.Used, newQuota.Status.Used) {
			continue
		}

		if err := e.quotaAccessor.UpdateQuotaStatus(&newQuota); err != nil {
			updatedFailedQuotas = append(updatedFailedQuotas, newQuota)
			lastErr = err
		}
	}

	if len(updatedFailedQuotas) == 0 {
		// all the updates succeeded.  At this point, anything with the default deny error was just waiting to
		// get a successful update, so we can mark and notify
		for _, admissionAttribute := range admissionAttributes {
			if IsDefaultDeny(admissionAttribute.result) {
				admissionAttribute.result = nil
			}
		}
		return
	}

	// at this point, errors are fatal.  Update all waiters without status to failed and return
	if remainingRetries <= 0 {
		for _, admissionAttribute := range admissionAttributes {
			if IsDefaultDeny(admissionAttribute.result) {
				admissionAttribute.result = lastErr
			}
		}
		return
	}

	// this retry logic has the same bug that its possible to be checking against quota in a state that never actually exists where
	// you've added a new documented, then updated an old one, your resource matches both and you're only checking one
	// updates for these quota names failed.  Get the current quotas in the namespace, compare by name, check to see if the
	// resource versions have changed.  If not, we're going to fall through an fail everything.  If they all have, then we can try again
	newQuotas, err := e.quotaAccessor.GetQuotas(quotas[0].Namespace)
	if err != nil {
		// this means that updates failed.  Anything with a default deny error has failed and we need to let them know
		for _, admissionAttribute := range admissionAttributes {
			if IsDefaultDeny(admissionAttribute.result) {
				admissionAttribute.result = lastErr
			}
		}
		return
	}

	// this logic goes through our cache to find the new version of all quotas that failed update.  If something has been removed
	// it is skipped on this retry.  After all, you removed it.
	quotasToCheck := []corev1.ResourceQuota{}
	for _, newQuota := range newQuotas {
		for _, oldQuota := range updatedFailedQuotas {
			if newQuota.Name == oldQuota.Name {
				quotasToCheck = append(quotasToCheck, newQuota)
				break
			}
		}
	}
	e.checkQuotas(quotasToCheck, admissionAttributes, remainingRetries-1)
}

func copyQuotas(in []corev1.ResourceQuota) ([]corev1.ResourceQuota, error) {
	out := make([]corev1.ResourceQuota, 0, len(in))
	for _, quota := range in {
		out = append(out, *quota.DeepCopy())
	}

	return out, nil
}

// filterLimitedResourcesByGroupResource filters the input that match the specified groupResource
func filterLimitedResourcesByGroupResource(input []resourcequotaapi.LimitedResource, groupResource schema.GroupResource) []resourcequotaapi.LimitedResource {
	result := []resourcequotaapi.LimitedResource{}
	for i := range input {
		limitedResource := input[i]
		limitedGroupResource := schema.GroupResource{Group: limitedResource.APIGroup, Resource: limitedResource.Resource}
		if limitedGroupResource == groupResource {
			result = append(result, limitedResource)
		}
	}
	return result
}

// limitedByDefault determines from the specified usage and limitedResources the set of resources names
// that must be present in a covering quota.  It returns empty set if it was unable to determine if
// a resource was not limited by default.
func limitedByDefault(usage corev1.ResourceList, limitedResources []resourcequotaapi.LimitedResource) []corev1.ResourceName {
	result := []corev1.ResourceName{}
	for _, limitedResource := range limitedResources {
		for k, v := range usage {
			// if a resource is consumed, we need to check if it matches on the limited resource list.
			if v.Sign() == 1 {
				// if we get a match, we add it to limited set
				for _, matchContain := range limitedResource.MatchContains {
					if strings.Contains(string(k), matchContain) {
						result = append(result, k)
						break
					}
				}
			}
		}
	}
	return result
}

func getMatchedLimitedScopes(evaluator quota.Evaluator, inputObject runtime.Object, limitedResources []resourcequotaapi.LimitedResource) ([]corev1.ScopedResourceSelectorRequirement, error) {
	scopes := []corev1.ScopedResourceSelectorRequirement{}
	for _, limitedResource := range limitedResources {
		matched, err := evaluator.MatchingScopes(inputObject, limitedResource.MatchScopes)
		if err != nil {
			klog.ErrorS(err, "Error while matching limited Scopes")
			return []corev1.ScopedResourceSelectorRequirement{}, err
		}
		scopes = append(scopes, matched...)
	}
	return scopes, nil
}

// checkRequest verifies that the request does not exceed any quota constraint. it returns a copy of quotas not yet persisted
// that capture what the usage would be if the request succeeded.  It return an error if there is insufficient quota to satisfy the request
func (e *quotaEvaluator) checkRequest(quotas []corev1.ResourceQuota, a admission.Attributes) ([]corev1.ResourceQuota, error) {
	evaluator := e.registry.Get(a.GetResource().GroupResource())
	if evaluator == nil {
		return quotas, nil
	}
	return CheckRequest(quotas, a, evaluator, e.config.LimitedResources)
}

// CheckRequest is a static version of quotaEvaluator.checkRequest, possible to be called from outside.
func CheckRequest(quotas []corev1.ResourceQuota, a admission.Attributes, evaluator quota.Evaluator,
	limited []resourcequotaapi.LimitedResource) ([]corev1.ResourceQuota, error) {
	if !evaluator.Handles(a) {
		return quotas, nil
	}

	// if we have limited resources enabled for this resource, always calculate usage
	inputObject := a.GetObject()

	// Check if object matches AdmissionConfiguration matchScopes
	limitedScopes, err := getMatchedLimitedScopes(evaluator, inputObject, limited)
	if err != nil {
		return quotas, nil
	}

	// determine the set of resource names that must exist in a covering quota
	limitedResourceNames := []corev1.ResourceName{}
	limitedResources := filterLimitedResourcesByGroupResource(limited, a.GetResource().GroupResource())
	if len(limitedResources) > 0 {
		deltaUsage, err := evaluator.Usage(inputObject)
		if err != nil {
			return quotas, err
		}
		limitedResourceNames = limitedByDefault(deltaUsage, limitedResources)
	}
	limitedResourceNamesSet := quota.ToSet(limitedResourceNames)

	// find the set of quotas that are pertinent to this request
	// reject if we match the quota, but usage is not calculated yet
	// reject if the input object does not satisfy quota constraints
	// if there are no pertinent quotas, we can just return
	interestingQuotaIndexes := []int{}
	// track the cumulative set of resources that were required across all quotas
	// this is needed to know if we have satisfied any constraints where consumption
	// was limited by default.
	restrictedResourcesSet := sets.String{}
	restrictedScopes := []corev1.ScopedResourceSelectorRequirement{}
	for i := range quotas {
		resourceQuota := quotas[i]
		scopeSelectors := getScopeSelectorsFromQuota(resourceQuota)
		localRestrictedScopes, err := evaluator.MatchingScopes(inputObject, scopeSelectors)
		if err != nil {
			return nil, fmt.Errorf("error matching scopes of quota %s, err: %v", resourceQuota.Name, err)
		}
		restrictedScopes = append(restrictedScopes, localRestrictedScopes...)

		match, err := evaluator.Matches(&resourceQuota, inputObject)
		if err != nil {
			klog.ErrorS(err, "Error occurred while matching resource quota against input object",
				"resourceQuota", resourceQuota)
			return quotas, err
		}
		if !match {
			continue
		}

		hardResources := quota.ResourceNames(resourceQuota.Status.Hard)
		restrictedResources := evaluator.MatchingResources(hardResources)
		if err := evaluator.Constraints(restrictedResources, inputObject); err != nil {
			return nil, admission.NewForbidden(a, fmt.Errorf("failed quota: %s: %v", resourceQuota.Name, err))
		}
		if !hasUsageStats(&resourceQuota, restrictedResources) {
			return nil, admission.NewForbidden(a, fmt.Errorf("status unknown for quota: %s, resources: %s", resourceQuota.Name, prettyPrintResourceNames(restrictedResources)))
		}
		interestingQuotaIndexes = append(interestingQuotaIndexes, i)
		localRestrictedResourcesSet := quota.ToSet(restrictedResources)
		restrictedResourcesSet.Insert(localRestrictedResourcesSet.List()...)
	}

	// Usage of some resources cannot be counted in isolation. For example, when
	// the resource represents a number of unique references to external
	// resource. In such a case an evaluator needs to process other objects in
	// the same namespace which needs to be known.
	namespace := a.GetNamespace()
	if accessor, err := meta.Accessor(inputObject); namespace != "" && err == nil {
		if accessor.GetNamespace() == "" {
			accessor.SetNamespace(namespace)
		}
	}
	// there is at least one quota that definitely matches our object
	// as a result, we need to measure the usage of this object for quota
	// on updates, we need to subtract the previous measured usage
	// if usage shows no change, just return since it has no impact on quota
	deltaUsage, err := evaluator.Usage(inputObject)
	if err != nil {
		return quotas, err
	}

	// ensure that usage for input object is never negative (this would mean a resource made a negative resource requirement)
	if negativeUsage := quota.IsNegative(deltaUsage); len(negativeUsage) > 0 {
		return nil, admission.NewForbidden(a, fmt.Errorf("quota usage is negative for resource(s): %s", prettyPrintResourceNames(negativeUsage)))
	}

	if admission.Update == a.GetOperation() {
		prevItem := a.GetOldObject()
		if prevItem == nil {
			return nil, admission.NewForbidden(a, fmt.Errorf("unable to get previous usage since prior version of object was not found"))
		}

		// if we can definitively determine that this is not a case of "create on update",
		// then charge based on the delta.  Otherwise, bill the maximum
		metadata, err := meta.Accessor(prevItem)
		if err == nil && len(metadata.GetResourceVersion()) > 0 {
			prevUsage, innerErr := evaluator.Usage(prevItem)
			if innerErr != nil {
				return quotas, innerErr
			}
			deltaUsage = quota.SubtractWithNonNegativeResult(deltaUsage, prevUsage)
		}
	}

	// ignore items in deltaUsage with zero usage
	deltaUsage = quota.RemoveZeros(deltaUsage)
	// if there is no remaining non-zero usage, short-circuit and return
	if len(deltaUsage) == 0 {
		return quotas, nil
	}

	// verify that for every resource that had limited by default consumption
	// enabled that there was a corresponding quota that covered its use.
	// if not, we reject the request.
	hasNoCoveringQuota := limitedResourceNamesSet.Difference(restrictedResourcesSet)
	if len(hasNoCoveringQuota) > 0 {
		return quotas, admission.NewForbidden(a, fmt.Errorf("insufficient quota to consume: %v", strings.Join(hasNoCoveringQuota.List(), ",")))
	}

	// verify that for every scope that had limited access enabled
	// that there was a corresponding quota that covered it.
	// if not, we reject the request.
	scopesHasNoCoveringQuota, err := evaluator.UncoveredQuotaScopes(limitedScopes, restrictedScopes)
	if err != nil {
		return quotas, err
	}
	if len(scopesHasNoCoveringQuota) > 0 {
		return quotas, fmt.Errorf("insufficient quota to match these scopes: %v", scopesHasNoCoveringQuota)
	}

	if len(interestingQuotaIndexes) == 0 {
		return quotas, nil
	}

	outQuotas, err := copyQuotas(quotas)
	if err != nil {
		return nil, err
	}

	for _, index := range interestingQuotaIndexes {
		resourceQuota := outQuotas[index]

		hardResources := quota.ResourceNames(resourceQuota.Status.Hard)
		requestedUsage := quota.Mask(deltaUsage, hardResources)
		newUsage := quota.Add(resourceQuota.Status.Used, requestedUsage)
		maskedNewUsage := quota.Mask(newUsage, quota.ResourceNames(requestedUsage))

		if allowed, exceeded := quota.LessThanOrEqual(maskedNewUsage, resourceQuota.Status.Hard); !allowed {
			failedRequestedUsage := quota.Mask(requestedUsage, exceeded)
			failedUsed := quota.Mask(resourceQuota.Status.Used, exceeded)
			failedHard := quota.Mask(resourceQuota.Status.Hard, exceeded)
			return nil, admission.NewForbidden(a,
				fmt.Errorf("exceeded quota: %s, requested: %s, used: %s, limited: %s",
					resourceQuota.Name,
					prettyPrint(failedRequestedUsage),
					prettyPrint(failedUsed),
					prettyPrint(failedHard)))
		}

		// update to the new usage number
		outQuotas[index].Status.Used = newUsage
	}

	return outQuotas, nil
}

func getScopeSelectorsFromQuota(quota corev1.ResourceQuota) []corev1.ScopedResourceSelectorRequirement {
	selectors := []corev1.ScopedResourceSelectorRequirement{}
	for _, scope := range quota.Spec.Scopes {
		selectors = append(selectors, corev1.ScopedResourceSelectorRequirement{
			ScopeName: scope,
			Operator:  corev1.ScopeSelectorOpExists})
	}
	if quota.Spec.ScopeSelector != nil {
		selectors = append(selectors, quota.Spec.ScopeSelector.MatchExpressions...)
	}
	return selectors
}

func (e *quotaEvaluator) Evaluate(a admission.Attributes) error {
	e.init.Do(e.start)

	// is this resource ignored?
	gvr := a.GetResource()
	gr := gvr.GroupResource()
	if _, ok := e.ignoredResources[gr]; ok {
		return nil
	}

	// if we do not know how to evaluate use for this resource, create an evaluator
	evaluator := e.registry.Get(gr)
	if evaluator == nil {
		// create an object count evaluator if no evaluator previously registered
		// note, we do not need aggregate usage here, so we pass a nil informer func
		evaluator = generic.NewObjectCountEvaluator(gr, nil, "")
		e.registry.Add(evaluator)
		klog.Infof("quota admission added evaluator for: %s", gr)
	}
	// for this kind, check if the operation could mutate any quota resources
	// if no resources tracked by quota are impacted, then just return
	if !evaluator.Handles(a) {
		return nil
	}
	waiter := newAdmissionWaiter(a)

	e.addWork(waiter)

	// wait for completion or timeout
	select {
	case <-waiter.finished:
	case <-time.After(10 * time.Second):
		return apierrors.NewInternalError(fmt.Errorf("resource quota evaluation timed out"))
	}

	return waiter.result
}

func (e *quotaEvaluator) addWork(a *admissionWaiter) {
	e.workLock.Lock()
	defer e.workLock.Unlock()

	ns := a.attributes.GetNamespace()
	// this Add can trigger a Get BEFORE the work is added to a list, but this is ok because the getWork routine
	// waits the worklock before retrieving the work to do, so the writes in this method will be observed
	e.queue.Add(ns)

	if e.inProgress.Has(ns) {
		e.dirtyWork[ns] = append(e.dirtyWork[ns], a)
		return
	}

	e.work[ns] = append(e.work[ns], a)
}

func (e *quotaEvaluator) completeWork(ns string) {
	e.workLock.Lock()
	defer e.workLock.Unlock()

	e.queue.Done(ns)
	e.work[ns] = e.dirtyWork[ns]
	delete(e.dirtyWork, ns)
	e.inProgress.Delete(ns)
}

// getWork returns a namespace, a list of work items in that
// namespace, and a shutdown boolean.  If not shutdown then the return
// must eventually be followed by a call on completeWork for the
// returned namespace (regardless of whether the work item list is
// empty).
func (e *quotaEvaluator) getWork() (string, []*admissionWaiter, bool) {
	ns, shutdown := e.queue.Get()
	if shutdown {
		return "", []*admissionWaiter{}, shutdown
	}

	e.workLock.Lock()
	defer e.workLock.Unlock()
	// at this point, we know we have a coherent view of e.work.  It is entirely possible
	// that our workqueue has another item requeued to it, but we'll pick it up early.  This ok
	// because the next time will go into our dirty list

	work := e.work[ns]
	delete(e.work, ns)
	delete(e.dirtyWork, ns)
	e.inProgress.Insert(ns)
	return ns, work, false
}

// prettyPrint formats a resource list for usage in errors
// it outputs resources sorted in increasing order
func prettyPrint(item corev1.ResourceList) string {
	parts := []string{}
	keys := []string{}
	for key := range item {
		keys = append(keys, string(key))
	}
	sort.Strings(keys)
	for _, key := range keys {
		value := item[corev1.ResourceName(key)]
		constraint := key + "=" + value.String()
		parts = append(parts, constraint)
	}
	return strings.Join(parts, ",")
}

func prettyPrintResourceNames(a []corev1.ResourceName) string {
	values := []string{}
	for _, value := range a {
		values = append(values, string(value))
	}
	sort.Strings(values)
	return strings.Join(values, ",")
}

// hasUsageStats returns true if for each hard constraint in interestingResources there is a value for its current usage
func hasUsageStats(resourceQuota *corev1.ResourceQuota, interestingResources []corev1.ResourceName) bool {
	interestingSet := quota.ToSet(interestingResources)
	for resourceName := range resourceQuota.Status.Hard {
		if !interestingSet.Has(string(resourceName)) {
			continue
		}
		if _, found := resourceQuota.Status.Used[resourceName]; !found {
			return false
		}
	}
	return true
}
