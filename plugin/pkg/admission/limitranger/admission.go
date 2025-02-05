/*
Copyright 2014 The Kubernetes Authors.

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

package limitranger

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"golang.org/x/sync/singleflight"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apiserver/pkg/admission"
	genericadmissioninitailizer "k8s.io/apiserver/pkg/admission/initializer"
	"k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/utils/lru"

	api "k8s.io/kubernetes/pkg/apis/core"
	"k8s.io/kubernetes/pkg/features"
)

const (
	limitRangerAnnotation = "kubernetes.io/limit-ranger"
	// PluginName indicates name of admission plugin.
	PluginName = "LimitRanger"
)

// Register registers a plugin
func Register(plugins *admission.Plugins) {
	plugins.Register(PluginName, func(config io.Reader) (admission.Interface, error) {
		return NewLimitRanger(&DefaultLimitRangerActions{})
	})
}

// LimitRanger enforces usage limits on a per resource basis in the namespace
type LimitRanger struct {
	*admission.Handler
	client  kubernetes.Interface
	actions LimitRangerActions
	lister  corev1listers.LimitRangeLister

	// liveLookups holds the last few live lookups we've done to help amortize cost on repeated lookup failures.
	// This let's us handle the case of latent caches, by looking up actual results for a namespace on cache miss/no results.
	// We track the lookup result here so that for repeated requests, we don't look it up very often.
	liveLookupCache *lru.Cache
	group           singleflight.Group
	liveTTL         time.Duration
}

var _ admission.MutationInterface = &LimitRanger{}
var _ admission.ValidationInterface = &LimitRanger{}

var _ genericadmissioninitailizer.WantsExternalKubeInformerFactory = &LimitRanger{}
var _ genericadmissioninitailizer.WantsExternalKubeClientSet = &LimitRanger{}

type liveLookupEntry struct {
	expiry time.Time
	items  []*corev1.LimitRange
}

// SetExternalKubeInformerFactory registers an informer factory into the LimitRanger
func (l *LimitRanger) SetExternalKubeInformerFactory(f informers.SharedInformerFactory) {
	limitRangeInformer := f.Core().V1().LimitRanges()
	l.SetReadyFunc(limitRangeInformer.Informer().HasSynced)
	l.lister = limitRangeInformer.Lister()
}

// SetExternalKubeClientSet registers the client into LimitRanger
func (l *LimitRanger) SetExternalKubeClientSet(client kubernetes.Interface) {
	l.client = client
}

// ValidateInitialization verifies the LimitRanger object has been properly initialized
func (l *LimitRanger) ValidateInitialization() error {
	if l.lister == nil {
		return fmt.Errorf("missing limitRange lister")
	}
	if l.client == nil {
		return fmt.Errorf("missing client")
	}
	return nil
}

// Admit admits resources into cluster that do not violate any defined LimitRange in the namespace
func (l *LimitRanger) Admit(ctx context.Context, a admission.Attributes, o admission.ObjectInterfaces) (err error) {
	return l.runLimitFunc(a, l.actions.MutateLimit)
}

// Validate admits resources into cluster that do not violate any defined LimitRange in the namespace
func (l *LimitRanger) Validate(ctx context.Context, a admission.Attributes, o admission.ObjectInterfaces) (err error) {
	return l.runLimitFunc(a, l.actions.ValidateLimit)
}

func (l *LimitRanger) runLimitFunc(a admission.Attributes, limitFn func(limitRange *corev1.LimitRange, kind string, obj runtime.Object) error) (err error) {
	if !l.actions.SupportsAttributes(a) {
		return nil
	}

	// ignore all objects marked for deletion
	oldObj := a.GetOldObject()
	if oldObj != nil {
		oldAccessor, err := meta.Accessor(oldObj)
		if err != nil {
			return admission.NewForbidden(a, err)
		}
		if oldAccessor.GetDeletionTimestamp() != nil {
			return nil
		}
	}

	items, err := l.GetLimitRanges(a)
	if err != nil {
		return err
	}

	// ensure it meets each prescribed min/max
	for i := range items {
		limitRange := items[i]

		if !l.actions.SupportsLimit(limitRange) {
			continue
		}

		err = limitFn(limitRange, a.GetResource().Resource, a.GetObject())
		if err != nil {
			return admission.NewForbidden(a, err)
		}
	}
	return nil
}

// GetLimitRanges returns a LimitRange object with the items held in
// the indexer if available, or do alive lookup of the value.
func (l *LimitRanger) GetLimitRanges(a admission.Attributes) ([]*corev1.LimitRange, error) {
	items, err := l.lister.LimitRanges(a.GetNamespace()).List(labels.Everything())
	if err != nil {
		return nil, admission.NewForbidden(a, fmt.Errorf("unable to %s %v at this time because there was an error enforcing limit ranges", a.GetOperation(), a.GetResource()))
	}

	// if there are no items held in our indexer, check our live-lookup LRU, if that misses, do the live lookup to prime it.
	if len(items) == 0 {
		lruItemObj, ok := l.liveLookupCache.Get(a.GetNamespace())
		if !ok || lruItemObj.(liveLookupEntry).expiry.Before(time.Now()) {
			// Fixed: #22422
			// use singleflight to alleviate simultaneous calls to
			lruItemObj, err, _ = l.group.Do(a.GetNamespace(), func() (interface{}, error) {
				liveList, err := l.client.CoreV1().LimitRanges(a.GetNamespace()).List(context.TODO(), metav1.ListOptions{})
				if err != nil {
					return nil, admission.NewForbidden(a, err)
				}
				newEntry := liveLookupEntry{expiry: time.Now().Add(l.liveTTL)}
				for i := range liveList.Items {
					newEntry.items = append(newEntry.items, &liveList.Items[i])
				}
				l.liveLookupCache.Add(a.GetNamespace(), newEntry)
				return newEntry, nil
			})
			if err != nil {
				return nil, err
			}
		}
		lruEntry := lruItemObj.(liveLookupEntry)

		items = append(items, lruEntry.items...)

	}

	return items, nil
}

// NewLimitRanger returns an object that enforces limits based on the supplied limit function
func NewLimitRanger(actions LimitRangerActions) (*LimitRanger, error) {
	liveLookupCache := lru.New(10000)

	if actions == nil {
		actions = &DefaultLimitRangerActions{}
	}

	return &LimitRanger{
		Handler:         admission.NewHandler(admission.Create, admission.Update),
		actions:         actions,
		liveLookupCache: liveLookupCache,
		liveTTL:         time.Duration(30 * time.Second),
	}, nil
}

// defaultContainerResourceRequirements returns the default requirements for a container
// the requirement.Limits are taken from the LimitRange defaults (if specified)
// the requirement.Requests are taken from the LimitRange default request (if specified)
func defaultContainerResourceRequirements(limitRange *corev1.LimitRange) api.ResourceRequirements {
	requirements := api.ResourceRequirements{}
	requirements.Requests = api.ResourceList{}
	requirements.Limits = api.ResourceList{}

	for i := range limitRange.Spec.Limits {
		limit := limitRange.Spec.Limits[i]
		if limit.Type == corev1.LimitTypeContainer {
			for k, v := range limit.DefaultRequest {
				requirements.Requests[api.ResourceName(k)] = v.DeepCopy()
			}
			for k, v := range limit.Default {
				requirements.Limits[api.ResourceName(k)] = v.DeepCopy()
			}
		}
	}
	return requirements
}

// mergeContainerResources handles defaulting all of the resources on a container.
func mergeContainerResources(container *api.Container, defaultRequirements *api.ResourceRequirements, annotationPrefix string, annotations []string) []string {
	setRequests := []string{}
	setLimits := []string{}
	if container.Resources.Limits == nil {
		container.Resources.Limits = api.ResourceList{}
	}
	if container.Resources.Requests == nil {
		container.Resources.Requests = api.ResourceList{}
	}
	for k, v := range defaultRequirements.Limits {
		_, found := container.Resources.Limits[k]
		if !found {
			container.Resources.Limits[k] = v.DeepCopy()
			setLimits = append(setLimits, string(k))
		}
	}
	for k, v := range defaultRequirements.Requests {
		_, found := container.Resources.Requests[k]
		if !found {
			container.Resources.Requests[k] = v.DeepCopy()
			setRequests = append(setRequests, string(k))
		}
	}
	if len(setRequests) > 0 {
		sort.Strings(setRequests)
		a := strings.Join(setRequests, ", ") + fmt.Sprintf(" request for %s %s", annotationPrefix, container.Name)
		annotations = append(annotations, a)
	}
	if len(setLimits) > 0 {
		sort.Strings(setLimits)
		a := strings.Join(setLimits, ", ") + fmt.Sprintf(" limit for %s %s", annotationPrefix, container.Name)
		annotations = append(annotations, a)
	}
	return annotations
}

// mergePodResourceRequirements merges enumerated requirements with default requirements
// it annotates the pod with information about what requirements were modified
func mergePodResourceRequirements(pod *api.Pod, defaultRequirements *api.ResourceRequirements) {
	annotations := []string{}

	for i := range pod.Spec.Containers {
		annotations = mergeContainerResources(&pod.Spec.Containers[i], defaultRequirements, "container", annotations)
	}

	for i := range pod.Spec.InitContainers {
		annotations = mergeContainerResources(&pod.Spec.InitContainers[i], defaultRequirements, "init container", annotations)
	}

	if len(annotations) > 0 {
		if pod.ObjectMeta.Annotations == nil {
			pod.ObjectMeta.Annotations = make(map[string]string)
		}
		val := "LimitRanger plugin set: " + strings.Join(annotations, "; ")
		pod.ObjectMeta.Annotations[limitRangerAnnotation] = val
	}
}

// requestLimitEnforcedValues returns the specified values at a common precision to support comparability
func requestLimitEnforcedValues(requestQuantity, limitQuantity, enforcedQuantity resource.Quantity) (request, limit, enforced int64) {
	request = requestQuantity.Value()
	limit = limitQuantity.Value()
	enforced = enforcedQuantity.Value()
	// do a more precise comparison if possible (if the value won't overflow)
	if request <= resource.MaxMilliValue && limit <= resource.MaxMilliValue && enforced <= resource.MaxMilliValue {
		request = requestQuantity.MilliValue()
		limit = limitQuantity.MilliValue()
		enforced = enforcedQuantity.MilliValue()
	}
	return
}

// minConstraint enforces the min constraint over the specified resource
func minConstraint(limitType string, resourceName string, enforced resource.Quantity, request api.ResourceList, limit api.ResourceList) error {
	req, reqExists := request[api.ResourceName(resourceName)]
	lim, limExists := limit[api.ResourceName(resourceName)]
	observedReqValue, observedLimValue, enforcedValue := requestLimitEnforcedValues(req, lim, enforced)

	if !reqExists {
		return fmt.Errorf("minimum %s usage per %s is %s.  No request is specified", resourceName, limitType, enforced.String())
	}
	if observedReqValue < enforcedValue {
		return fmt.Errorf("minimum %s usage per %s is %s, but request is %s", resourceName, limitType, enforced.String(), req.String())
	}
	if limExists && (observedLimValue < enforcedValue) {
		return fmt.Errorf("minimum %s usage per %s is %s, but limit is %s", resourceName, limitType, enforced.String(), lim.String())
	}
	return nil
}

// maxRequestConstraint enforces the max constraint over the specified resource
// use when specify LimitType resource doesn't recognize limit values
func maxRequestConstraint(limitType string, resourceName string, enforced resource.Quantity, request api.ResourceList) error {
	req, reqExists := request[api.ResourceName(resourceName)]
	observedReqValue, _, enforcedValue := requestLimitEnforcedValues(req, resource.Quantity{}, enforced)

	if !reqExists {
		return fmt.Errorf("maximum %s usage per %s is %s.  No request is specified", resourceName, limitType, enforced.String())
	}
	if observedReqValue > enforcedValue {
		return fmt.Errorf("maximum %s usage per %s is %s, but request is %s", resourceName, limitType, enforced.String(), req.String())
	}
	return nil
}

// maxConstraint enforces the max constraint over the specified resource
func maxConstraint(limitType string, resourceName string, enforced resource.Quantity, request api.ResourceList, limit api.ResourceList) error {
	req, reqExists := request[api.ResourceName(resourceName)]
	lim, limExists := limit[api.ResourceName(resourceName)]
	observedReqValue, observedLimValue, enforcedValue := requestLimitEnforcedValues(req, lim, enforced)

	if !limExists {
		return fmt.Errorf("maximum %s usage per %s is %s.  No limit is specified", resourceName, limitType, enforced.String())
	}
	if observedLimValue > enforcedValue {
		return fmt.Errorf("maximum %s usage per %s is %s, but limit is %s", resourceName, limitType, enforced.String(), lim.String())
	}
	if reqExists && (observedReqValue > enforcedValue) {
		return fmt.Errorf("maximum %s usage per %s is %s, but request is %s", resourceName, limitType, enforced.String(), req.String())
	}
	return nil
}

// limitRequestRatioConstraint enforces the limit to request ratio over the specified resource
func limitRequestRatioConstraint(limitType string, resourceName string, enforced resource.Quantity, request api.ResourceList, limit api.ResourceList) error {
	req, reqExists := request[api.ResourceName(resourceName)]
	lim, limExists := limit[api.ResourceName(resourceName)]
	observedReqValue, observedLimValue, _ := requestLimitEnforcedValues(req, lim, enforced)

	if !reqExists || (observedReqValue == int64(0)) {
		return fmt.Errorf("%s max limit to request ratio per %s is %s, but no request is specified or request is 0", resourceName, limitType, enforced.String())
	}
	if !limExists || (observedLimValue == int64(0)) {
		return fmt.Errorf("%s max limit to request ratio per %s is %s, but no limit is specified or limit is 0", resourceName, limitType, enforced.String())
	}

	observedRatio := float64(observedLimValue) / float64(observedReqValue)
	displayObservedRatio := observedRatio
	maxLimitRequestRatio := float64(enforced.Value())
	if enforced.Value() <= resource.MaxMilliValue {
		observedRatio = observedRatio * 1000
		maxLimitRequestRatio = float64(enforced.MilliValue())
	}

	if observedRatio > maxLimitRequestRatio {
		return fmt.Errorf("%s max limit to request ratio per %s is %s, but provided ratio is %f", resourceName, limitType, enforced.String(), displayObservedRatio)
	}

	return nil
}

// DefaultLimitRangerActions is the default implementation of LimitRangerActions.
type DefaultLimitRangerActions struct{}

// ensure DefaultLimitRangerActions implements the LimitRangerActions interface.
var _ LimitRangerActions = &DefaultLimitRangerActions{}

// MutateLimit enforces resource requirements of incoming resources
// against enumerated constraints on the LimitRange.  It may modify
// the incoming object to apply default resource requirements if not
// specified, and enumerated on the LimitRange
func (d *DefaultLimitRangerActions) MutateLimit(limitRange *corev1.LimitRange, resourceName string, obj runtime.Object) error {
	switch resourceName {
	case "pods":
		return PodMutateLimitFunc(limitRange, obj.(*api.Pod))
	}
	return nil
}

// ValidateLimit verifies the resource requirements of incoming
// resources against enumerated constraints on the LimitRange are
// valid
func (d *DefaultLimitRangerActions) ValidateLimit(limitRange *corev1.LimitRange, resourceName string, obj runtime.Object) error {
	switch resourceName {
	case "pods":
		return PodValidateLimitFunc(limitRange, obj.(*api.Pod))
	case "persistentvolumeclaims":
		return PersistentVolumeClaimValidateLimitFunc(limitRange, obj.(*api.PersistentVolumeClaim))
	}
	return nil
}

// SupportsAttributes ignores all calls that do not deal with pod resources or storage requests (PVCs).
// Also ignores any call that has a subresource defined.
func (d *DefaultLimitRangerActions) SupportsAttributes(a admission.Attributes) bool {
	// Handle in-place vertical scaling of pods, where users modify container
	// resources using the resize subresource.
	if a.GetSubresource() == "resize" && a.GetKind().GroupKind() == api.Kind("Pod") && a.GetOperation() == admission.Update {
		return true
	}

	// No other subresources are supported
	if a.GetSubresource() != "" {
		return false
	}

	// Since containers and initContainers cannot currently be added, removed, or updated, it is unnecessary
	// to mutate and validate limitrange on pod updates. Trying to mutate containers or initContainers on a pod
	// update request will always fail pod validation because those fields are immutable once the object is created.
	if a.GetKind().GroupKind() == api.Kind("Pod") && a.GetOperation() == admission.Update {
		return false
	}

	return a.GetKind().GroupKind() == api.Kind("Pod") || a.GetKind().GroupKind() == api.Kind("PersistentVolumeClaim")
}

// SupportsLimit always returns true.
func (d *DefaultLimitRangerActions) SupportsLimit(limitRange *corev1.LimitRange) bool {
	return true
}

// PersistentVolumeClaimValidateLimitFunc enforces storage limits for PVCs.
// Users request storage via pvc.Spec.Resources.Requests.  Min/Max is enforced by an admin with LimitRange.
// Claims will not be modified with default values because storage is a required part of pvc.Spec.
// All storage enforced values *only* apply to pvc.Spec.Resources.Requests.
func PersistentVolumeClaimValidateLimitFunc(limitRange *corev1.LimitRange, pvc *api.PersistentVolumeClaim) error {
	var errs []error
	for i := range limitRange.Spec.Limits {
		limit := limitRange.Spec.Limits[i]
		limitType := limit.Type
		if limitType == corev1.LimitTypePersistentVolumeClaim {
			for k, v := range limit.Min {
				// normal usage of minConstraint. pvc.Spec.Resources.Limits is not recognized as user input
				if err := minConstraint(string(limitType), string(k), v, pvc.Spec.Resources.Requests, api.ResourceList{}); err != nil {
					errs = append(errs, err)
				}
			}
			for k, v := range limit.Max {
				// We want to enforce the max of the LimitRange against what
				// the user requested.
				if err := maxRequestConstraint(string(limitType), string(k), v, pvc.Spec.Resources.Requests); err != nil {
					errs = append(errs, err)
				}
			}
		}
	}
	return utilerrors.NewAggregate(errs)
}

// PodMutateLimitFunc sets resource requirements enumerated by the pod against
// the specified LimitRange.  The pod may be modified to apply default resource
// requirements if not specified, and enumerated on the LimitRange
func PodMutateLimitFunc(limitRange *corev1.LimitRange, pod *api.Pod) error {
	defaultResources := defaultContainerResourceRequirements(limitRange)
	mergePodResourceRequirements(pod, &defaultResources)
	return nil
}

// PodValidateLimitFunc enforces resource requirements enumerated by the pod against
// the specified LimitRange.
func PodValidateLimitFunc(limitRange *corev1.LimitRange, pod *api.Pod) error {
	var errs []error

	for i := range limitRange.Spec.Limits {
		limit := limitRange.Spec.Limits[i]
		limitType := limit.Type
		// enforce container limits
		if limitType == corev1.LimitTypeContainer {
			for j := range pod.Spec.Containers {
				container := &pod.Spec.Containers[j]
				for k, v := range limit.Min {
					if err := minConstraint(string(limitType), string(k), v, container.Resources.Requests, container.Resources.Limits); err != nil {
						errs = append(errs, err)
					}
				}
				for k, v := range limit.Max {
					if err := maxConstraint(string(limitType), string(k), v, container.Resources.Requests, container.Resources.Limits); err != nil {
						errs = append(errs, err)
					}
				}
				for k, v := range limit.MaxLimitRequestRatio {
					if err := limitRequestRatioConstraint(string(limitType), string(k), v, container.Resources.Requests, container.Resources.Limits); err != nil {
						errs = append(errs, err)
					}
				}
			}
			for j := range pod.Spec.InitContainers {
				container := &pod.Spec.InitContainers[j]
				for k, v := range limit.Min {
					if err := minConstraint(string(limitType), string(k), v, container.Resources.Requests, container.Resources.Limits); err != nil {
						errs = append(errs, err)
					}
				}
				for k, v := range limit.Max {
					if err := maxConstraint(string(limitType), string(k), v, container.Resources.Requests, container.Resources.Limits); err != nil {
						errs = append(errs, err)
					}
				}
				for k, v := range limit.MaxLimitRequestRatio {
					if err := limitRequestRatioConstraint(string(limitType), string(k), v, container.Resources.Requests, container.Resources.Limits); err != nil {
						errs = append(errs, err)
					}
				}
			}
		}

		// enforce pod limits on init containers
		if limitType == corev1.LimitTypePod {
			opts := podResourcesOptions{
				PodLevelResourcesEnabled: feature.DefaultFeatureGate.Enabled(features.PodLevelResources),
			}
			podRequests := podRequests(pod, opts)
			podLimits := podLimits(pod, opts)
			for k, v := range limit.Min {
				if err := minConstraint(string(limitType), string(k), v, podRequests, podLimits); err != nil {
					errs = append(errs, err)
				}
			}
			for k, v := range limit.Max {
				if err := maxConstraint(string(limitType), string(k), v, podRequests, podLimits); err != nil {
					errs = append(errs, err)
				}
			}
			for k, v := range limit.MaxLimitRequestRatio {
				if err := limitRequestRatioConstraint(string(limitType), string(k), v, podRequests, podLimits); err != nil {
					errs = append(errs, err)
				}
			}
		}
	}
	return utilerrors.NewAggregate(errs)
}

type podResourcesOptions struct {
	// PodLevelResourcesEnabled indicates that the PodLevelResources feature gate is
	// enabled.
	PodLevelResourcesEnabled bool
}

// podRequests is a simplified version of pkg/api/v1/resource/PodRequests that operates against the core version of
// pod. Any changes to that calculation should be reflected here.
// NOTE: We do not want to check status resources here, only the spec. This is equivalent to setting
// UseStatusResources=false in the common helper.
// TODO: Maybe we can consider doing a partial conversion of the pod to a v1
// type and then using the pkg/api/v1/resource/PodRequests.
// TODO(ndixita): PodRequests method exists in
// staging/src/k8s.io/component-helpers/resource/helpers.go. Refactor the code to
// avoid duplicating podRequests method.
func podRequests(pod *api.Pod, opts podResourcesOptions) api.ResourceList {
	reqs := api.ResourceList{}

	for _, container := range pod.Spec.Containers {
		containerReqs := container.Resources.Requests
		addResourceList(reqs, containerReqs)
	}

	restartableInitContainerReqs := api.ResourceList{}
	initContainerReqs := api.ResourceList{}
	// init containers define the minimum of any resource
	// Note: In-place resize is not allowed for InitContainers, so no need to check for ResizeStatus value
	for _, container := range pod.Spec.InitContainers {
		containerReqs := container.Resources.Requests

		if container.RestartPolicy != nil && *container.RestartPolicy == api.ContainerRestartPolicyAlways {
			// and add them to the resulting cumulative container requests
			addResourceList(reqs, containerReqs)

			// track our cumulative restartable init container resources
			addResourceList(restartableInitContainerReqs, containerReqs)
			containerReqs = restartableInitContainerReqs
		} else {
			tmp := api.ResourceList{}
			addResourceList(tmp, containerReqs)
			addResourceList(tmp, restartableInitContainerReqs)
			containerReqs = tmp
		}

		maxResourceList(initContainerReqs, containerReqs)
	}

	maxResourceList(reqs, initContainerReqs)

	// If PodLevelResources feature is enabled and resources are set at pod-level,
	// override aggregated container requests of resources supported by pod-level
	// resources with quantities specified at pod-level.
	if opts.PodLevelResourcesEnabled && pod.Spec.Resources != nil {
		for resourceName, quantity := range pod.Spec.Resources.Requests {
			if isSupportedPodLevelResource(resourceName) {
				// override with pod-level resource requests
				reqs[resourceName] = quantity
			}
		}
	}

	return reqs
}

// podLimits is a simplified version of pkg/api/v1/resource/PodLimits that operates against the core version of
// pod. Any changes to that calculation should be reflected here.
// NOTE: We do not want to check status resources here, only the spec. This is equivalent to setting
// UseStatusResources=false in the common helper.
// TODO: Maybe we can consider doing a partial conversion of the pod to a v1
// type and then using the pkg/api/v1/resource/PodLimits.
// TODO(ndixita): PodLimits method exists in
// staging/src/k8s.io/component-helpers/resource/helpers.go. Refactor the code to
// avoid duplicating podLimits method.
func podLimits(pod *api.Pod, opts podResourcesOptions) api.ResourceList {
	limits := api.ResourceList{}

	for _, container := range pod.Spec.Containers {
		addResourceList(limits, container.Resources.Limits)
	}

	restartableInitContainerLimits := api.ResourceList{}
	initContainerLimits := api.ResourceList{}
	// init containers define the minimum of any resource
	for _, container := range pod.Spec.InitContainers {
		containerLimits := container.Resources.Limits
		// Is the init container marked as a sidecar?
		if container.RestartPolicy != nil && *container.RestartPolicy == api.ContainerRestartPolicyAlways {
			addResourceList(limits, containerLimits)

			// track our cumulative restartable init container resources
			addResourceList(restartableInitContainerLimits, containerLimits)
			containerLimits = restartableInitContainerLimits
		} else {
			tmp := api.ResourceList{}
			addResourceList(tmp, containerLimits)
			addResourceList(tmp, restartableInitContainerLimits)
			containerLimits = tmp
		}
		maxResourceList(initContainerLimits, containerLimits)
	}

	maxResourceList(limits, initContainerLimits)

	// If PodLevelResources feature is enabled and resources are set at pod-level,
	// override aggregated container limits of resources supported by pod-level
	// resources with quantities specified at pod-level.
	if opts.PodLevelResourcesEnabled && pod.Spec.Resources != nil {
		for resourceName, quantity := range pod.Spec.Resources.Limits {
			if isSupportedPodLevelResource(resourceName) {
				// override with pod-level resource limits
				limits[resourceName] = quantity
			}
		}
	}

	return limits
}

var supportedPodLevelResources = sets.New(api.ResourceCPU, api.ResourceMemory)

// isSupportedPodLevelResources checks if a given resource is supported by pod-level
// resource management through the PodLevelResources feature. Returns true if
// the resource is supported.
// isSupportedPodLevelResource method exists in
// staging/src/k8s.io/component-helpers/resource/helpers.go.
// isSupportedPodLevelResource is added here to avoid conversion of v1.
// Pod to api.Pod.
// TODO(ndixita): Find alternatives to avoid duplicating the code.
func isSupportedPodLevelResource(name api.ResourceName) bool {
	return supportedPodLevelResources.Has(name)
}

// addResourceList adds the resources in newList to list.
func addResourceList(list, newList api.ResourceList) {
	for name, quantity := range newList {
		if value, ok := list[name]; !ok {
			list[name] = quantity.DeepCopy()
		} else {
			value.Add(quantity)
			list[name] = value
		}
	}
}

// maxResourceList sets list to the greater of list/newList for every resource in newList
func maxResourceList(list, newList api.ResourceList) {
	for name, quantity := range newList {
		if value, ok := list[name]; !ok || quantity.Cmp(value) > 0 {
			list[name] = quantity.DeepCopy()
		}
	}
}
