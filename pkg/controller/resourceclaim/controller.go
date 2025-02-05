/*
Copyright 2020 The Kubernetes Authors.

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

package resourceclaim

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1beta1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	corev1apply "k8s.io/client-go/applyconfigurations/core/v1"
	v1informers "k8s.io/client-go/informers/core/v1"
	resourceinformers "k8s.io/client-go/informers/resource/v1beta1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	v1core "k8s.io/client-go/kubernetes/typed/core/v1"
	v1listers "k8s.io/client-go/listers/core/v1"
	resourcelisters "k8s.io/client-go/listers/resource/v1beta1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/dynamic-resource-allocation/resourceclaim"
	"k8s.io/klog/v2"
	podutil "k8s.io/kubernetes/pkg/api/v1/pod"
	"k8s.io/kubernetes/pkg/controller/resourceclaim/metrics"
	"k8s.io/utils/ptr"
)

const (
	// podResourceClaimIndex is the lookup name for the index function which indexes by pod ResourceClaim templates.
	podResourceClaimIndex = "pod-resource-claim-index"

	// podResourceClaimAnnotation is the special annotation that generated
	// ResourceClaims get. Its value is the pod.spec.resourceClaims[].name
	// for which it was generated. This is used only inside the controller
	// and not documented as part of the Kubernetes API.
	podResourceClaimAnnotation = "resource.kubernetes.io/pod-claim-name"

	// claimPodOwnerIndex is used to find ResourceClaims which have
	// a specific pod as owner. Values for this index are the pod UID.
	claimPodOwnerIndex = "claim-pod-owner-index"

	// Field manager used to update the pod status.
	fieldManager = "ResourceClaimController"

	maxUIDCacheEntries = 500
)

// Controller creates ResourceClaims for ResourceClaimTemplates in a pod spec.
type Controller struct {
	// adminAccessEnabled matches the DRAAdminAccess feature gate state.
	adminAccessEnabled bool

	// kubeClient is the kube API client used to communicate with the API
	// server.
	kubeClient clientset.Interface

	// claimLister is the shared ResourceClaim lister used to fetch and store ResourceClaim
	// objects from the API server. It is shared with other controllers and
	// therefore the ResourceClaim objects in its store should be treated as immutable.
	claimLister  resourcelisters.ResourceClaimLister
	claimsSynced cache.InformerSynced
	claimCache   cache.MutationCache

	// podLister is the shared Pod lister used to fetch Pod
	// objects from the API server. It is shared with other controllers and
	// therefore the Pod objects in its store should be treated as immutable.
	podLister v1listers.PodLister
	podSynced cache.InformerSynced

	// templateLister is the shared ResourceClaimTemplate lister used to
	// fetch template objects from the API server. It is shared with other
	// controllers and therefore the objects in its store should be treated
	// as immutable.
	templateLister  resourcelisters.ResourceClaimTemplateLister
	templatesSynced cache.InformerSynced

	// podIndexer has the common PodResourceClaim indexer installed To
	// limit iteration over pods to those of interest.
	podIndexer cache.Indexer

	// recorder is used to record events in the API server
	recorder record.EventRecorder

	queue workqueue.TypedRateLimitingInterface[string]

	// The deletedObjects cache keeps track of Pods for which we know that
	// they have existed and have been removed. For those we can be sure
	// that a ReservedFor entry needs to be removed.
	deletedObjects *uidCache
}

const (
	claimKeyPrefix = "claim:"
	podKeyPrefix   = "pod:"
)

// NewController creates a ResourceClaim controller.
func NewController(
	logger klog.Logger,
	adminAccessEnabled bool,
	kubeClient clientset.Interface,
	podInformer v1informers.PodInformer,
	claimInformer resourceinformers.ResourceClaimInformer,
	templateInformer resourceinformers.ResourceClaimTemplateInformer) (*Controller, error) {

	ec := &Controller{
		adminAccessEnabled: adminAccessEnabled,
		kubeClient:         kubeClient,
		podLister:          podInformer.Lister(),
		podIndexer:         podInformer.Informer().GetIndexer(),
		podSynced:          podInformer.Informer().HasSynced,
		claimLister:        claimInformer.Lister(),
		claimsSynced:       claimInformer.Informer().HasSynced,
		templateLister:     templateInformer.Lister(),
		templatesSynced:    templateInformer.Informer().HasSynced,
		queue: workqueue.NewTypedRateLimitingQueueWithConfig(
			workqueue.DefaultTypedControllerRateLimiter[string](),
			workqueue.TypedRateLimitingQueueConfig[string]{Name: "resource_claim"},
		),
		deletedObjects: newUIDCache(maxUIDCacheEntries),
	}

	metrics.RegisterMetrics()

	if _, err := podInformer.Informer().AddEventHandlerWithOptions(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			ec.enqueuePod(logger, obj, false)
		},
		UpdateFunc: func(old, updated interface{}) {
			ec.enqueuePod(logger, updated, false)
		},
		DeleteFunc: func(obj interface{}) {
			ec.enqueuePod(logger, obj, true)
		},
	}, cache.HandlerOptions{Logger: &logger}); err != nil {
		return nil, err
	}
	if _, err := claimInformer.Informer().AddEventHandlerWithOptions(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			logger.V(6).Info("new claim", "claimDump", obj)
			ec.enqueueResourceClaim(logger, nil, obj)
		},
		UpdateFunc: func(old, updated interface{}) {
			logger.V(6).Info("updated claim", "claimDump", updated)
			ec.enqueueResourceClaim(logger, old, updated)
		},
		DeleteFunc: func(obj interface{}) {
			logger.V(6).Info("deleted claim", "claimDump", obj)
			ec.enqueueResourceClaim(logger, obj, nil)
		},
	}, cache.HandlerOptions{Logger: &logger}); err != nil {
		return nil, err
	}
	if err := ec.podIndexer.AddIndexers(cache.Indexers{podResourceClaimIndex: podResourceClaimIndexFunc}); err != nil {
		return nil, fmt.Errorf("could not initialize ResourceClaim controller: %w", err)
	}

	// The mutation cache acts as an additional layer for the informer
	// cache and after a create made by the controller returns that
	// object until the informer catches up. That is necessary
	// when a ResourceClaim got created, updating the pod status fails,
	// and then a retry occurs before the informer cache is updated.
	// In that scenario, the controller would create another claim
	// instead of continuing with the existing one.
	claimInformerCache := claimInformer.Informer().GetIndexer()
	if err := claimInformerCache.AddIndexers(cache.Indexers{claimPodOwnerIndex: claimPodOwnerIndexFunc}); err != nil {
		return nil, fmt.Errorf("could not initialize ResourceClaim controller: %w", err)
	}
	ec.claimCache = cache.NewIntegerResourceVersionMutationCache(logger, claimInformerCache, claimInformerCache,
		// Very long time to live, unlikely to be needed because
		// the informer cache should get updated soon.
		time.Hour,
		// Allow storing objects not in the underlying cache - that's the point...
		// It's safe because in case of a race (claim is in mutation cache, claim
		// gets deleted, controller updates status based on mutation cache) the
		// "bad" pod status will get detected and fixed when the informer catches up.
		true,
	)

	return ec, nil
}

func (ec *Controller) enqueuePod(logger klog.Logger, obj interface{}, deleted bool) {
	if d, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = d.Obj
	}
	pod, ok := obj.(*v1.Pod)
	if !ok {
		// Not a pod?!
		logger.Error(nil, "enqueuePod called for unexpected object", "type", fmt.Sprintf("%T", obj))
		return
	}

	if len(pod.Spec.ResourceClaims) == 0 {
		// Nothing to do for it at all.
		return
	}

	if deleted {
		logger.V(6).Info("pod got deleted", "pod", klog.KObj(pod))
		ec.deletedObjects.Add(pod.UID)
	}

	logger.V(6).Info("pod with resource claims changed", "pod", klog.KObj(pod), "deleted", deleted)

	// Release reservations of a deleted or completed pod?
	needsClaims, reason := podNeedsClaims(pod, deleted)
	if needsClaims {
		logger.V(6).Info("Not touching claims", "pod", klog.KObj(pod), "reason", reason)
	} else {
		for _, podClaim := range pod.Spec.ResourceClaims {
			claimName, _, err := resourceclaim.Name(pod, &podClaim)
			switch {
			case err != nil:
				// Either the claim was not created (nothing to do here) or
				// the API changed. The later will also get reported elsewhere,
				// so here it's just a debug message.
				logger.V(6).Info("Nothing to do for claim during pod change", "pod", klog.KObj(pod), "podClaim", podClaim.Name, "err", err, "reason", reason)
			case claimName != nil:
				key := claimKeyPrefix + pod.Namespace + "/" + *claimName
				logger.V(6).Info("Process claim", "pod", klog.KObj(pod), "claim", klog.KRef(pod.Namespace, *claimName), "key", key, "reason", reason)
				ec.queue.Add(key)
			default:
				// Nothing to do, claim wasn't generated.
				logger.V(6).Info("Nothing to do for skipped claim during pod change", "pod", klog.KObj(pod), "podClaim", podClaim.Name, "reason", reason)
			}
		}
	}

	needsWork, reason := ec.podNeedsWork(pod)
	if needsWork {
		logger.V(6).Info("enqueuing pod", "pod", klog.KObj(pod), "reason", reason)
		ec.queue.Add(podKeyPrefix + pod.Namespace + "/" + pod.Name)
		return
	}
	logger.V(6).Info("not enqueuing pod", "pod", klog.KObj(pod), "reason", reason)
}

func podNeedsClaims(pod *v1.Pod, deleted bool) (bool, string) {
	if deleted {
		return false, "pod got removed"
	}
	if podutil.IsPodTerminal(pod) {
		return false, "pod has terminated"
	}
	if pod.DeletionTimestamp != nil && pod.Spec.NodeName == "" {
		return false, "pod got deleted before scheduling"
	}
	// Still needs claims.
	return true, "pod might run"
}

// podNeedsWork checks whether a new or modified pod needs to be processed
// further by a worker. It returns a boolean with the result and an explanation
// for it.
func (ec *Controller) podNeedsWork(pod *v1.Pod) (bool, string) {
	if pod.DeletionTimestamp != nil {
		// Nothing else to do for the pod.
		return false, "pod is deleted"
	}

	for _, podClaim := range pod.Spec.ResourceClaims {
		claimName, checkOwner, err := resourceclaim.Name(pod, &podClaim)
		if err != nil {
			return true, err.Error()
		}
		// If the claimName is nil, then it has been determined before
		// that the claim is not needed.
		if claimName == nil {
			return false, "claim is not needed"
		}
		claim, err := ec.claimLister.ResourceClaims(pod.Namespace).Get(*claimName)
		if apierrors.IsNotFound(err) {
			if podClaim.ResourceClaimTemplateName != nil {
				return true, "must create ResourceClaim from template"
			}
			// User needs to create claim.
			return false, "claim is missing and must be created by user"
		}
		if err != nil {
			// Shouldn't happen.
			return true, fmt.Sprintf("internal error while checking for claim: %v", err)
		}

		if checkOwner {
			if err := resourceclaim.IsForPod(pod, claim); err != nil {
				// Cannot proceed with the pod unless that other claim gets deleted.
				return false, err.Error()
			}
		}

		// This check skips over the reasons below that only apply
		// when a pod has been scheduled already. We need to keep checking
		// for more claims that might need to be created.
		if pod.Spec.NodeName == "" {
			continue
		}

		if claim.Status.Allocation != nil &&
			!resourceclaim.IsReservedForPod(pod, claim) &&
			resourceclaim.CanBeReserved(claim) {
			// Need to reserve it.
			return true, fmt.Sprintf("need to reserve claim %s for pod", klog.KObj(claim))
		}
	}

	return false, "nothing to do"
}

func (ec *Controller) enqueueResourceClaim(logger klog.Logger, oldObj, newObj interface{}) {
	deleted := newObj != nil
	if d, ok := oldObj.(cache.DeletedFinalStateUnknown); ok {
		oldObj = d.Obj
	}
	oldClaim, ok := oldObj.(*resourceapi.ResourceClaim)
	if oldObj != nil && !ok {
		return
	}
	newClaim, ok := newObj.(*resourceapi.ResourceClaim)
	if newObj != nil && !ok {
		return
	}

	// Maintain metrics based on what was observed.
	switch {
	case oldClaim == nil:
		// Added.
		metrics.NumResourceClaims.Inc()
		if newClaim.Status.Allocation != nil {
			metrics.NumAllocatedResourceClaims.Inc()
		}
	case newClaim == nil:
		// Deleted.
		metrics.NumResourceClaims.Dec()
		if oldClaim.Status.Allocation != nil {
			metrics.NumAllocatedResourceClaims.Dec()
		}
	default:
		// Updated.
		switch {
		case oldClaim.Status.Allocation == nil && newClaim.Status.Allocation != nil:
			metrics.NumAllocatedResourceClaims.Inc()
		case oldClaim.Status.Allocation != nil && newClaim.Status.Allocation == nil:
			metrics.NumAllocatedResourceClaims.Dec()
		}
	}

	claim := newClaim
	if claim == nil {
		claim = oldClaim
	}
	if !deleted {
		// When starting up, we have to check all claims to find those with
		// stale pods in ReservedFor. During an update, a pod might get added
		// that already no longer exists.
		key := claimKeyPrefix + claim.Namespace + "/" + claim.Name
		logger.V(6).Info("enqueuing new or updated claim", "claim", klog.KObj(claim), "key", key)
		ec.queue.Add(key)
	} else {
		logger.V(6).Info("not enqueuing deleted claim", "claim", klog.KObj(claim))
	}

	// Also check whether this causes work for any of the currently
	// known pods which use the ResourceClaim.
	objs, err := ec.podIndexer.ByIndex(podResourceClaimIndex, fmt.Sprintf("%s/%s", claim.Namespace, claim.Name))
	if err != nil {
		logger.Error(err, "listing pods from cache")
		return
	}
	if len(objs) == 0 {
		logger.V(6).Info("unrelated to any known pod", "claim", klog.KObj(claim))
		return
	}
	for _, obj := range objs {
		ec.enqueuePod(logger, obj, false)
	}
}

func (ec *Controller) Run(ctx context.Context, workers int) {
	defer runtime.HandleCrash()
	defer ec.queue.ShutDown()

	logger := klog.FromContext(ctx)
	logger.Info("Starting resource claim controller")
	defer logger.Info("Shutting down resource claim controller")

	eventBroadcaster := record.NewBroadcaster(record.WithContext(ctx))
	eventBroadcaster.StartLogging(klog.Infof)
	eventBroadcaster.StartRecordingToSink(&v1core.EventSinkImpl{Interface: ec.kubeClient.CoreV1().Events("")})
	ec.recorder = eventBroadcaster.NewRecorder(scheme.Scheme, v1.EventSource{Component: "resource_claim"})
	defer eventBroadcaster.Shutdown()

	if !cache.WaitForNamedCacheSync("resource_claim", ctx.Done(), ec.podSynced, ec.claimsSynced, ec.templatesSynced) {
		return
	}

	for i := 0; i < workers; i++ {
		go wait.UntilWithContext(ctx, ec.runWorker, time.Second)
	}

	<-ctx.Done()
}

func (ec *Controller) runWorker(ctx context.Context) {
	for ec.processNextWorkItem(ctx) {
	}
}

func (ec *Controller) processNextWorkItem(ctx context.Context) bool {
	key, shutdown := ec.queue.Get()
	if shutdown {
		return false
	}
	defer ec.queue.Done(key)

	err := ec.syncHandler(ctx, key)
	if err == nil {
		ec.queue.Forget(key)
		return true
	}

	runtime.HandleError(fmt.Errorf("%v failed with: %v", key, err))
	ec.queue.AddRateLimited(key)

	return true
}

// syncHandler is invoked for each work item which might need to be processed.
// If an error is returned from this function, the item will be requeued.
func (ec *Controller) syncHandler(ctx context.Context, key string) error {
	sep := strings.Index(key, ":")
	if sep < 0 {
		return fmt.Errorf("unexpected key: %s", key)
	}
	prefix, object := key[0:sep+1], key[sep+1:]
	namespace, name, err := cache.SplitMetaNamespaceKey(object)
	if err != nil {
		return err
	}

	switch prefix {
	case podKeyPrefix:
		return ec.syncPod(ctx, namespace, name)
	case claimKeyPrefix:
		return ec.syncClaim(ctx, namespace, name)
	default:
		return fmt.Errorf("unexpected key prefix: %s", prefix)
	}

}

func (ec *Controller) syncPod(ctx context.Context, namespace, name string) error {
	logger := klog.LoggerWithValues(klog.FromContext(ctx), "pod", klog.KRef(namespace, name))
	ctx = klog.NewContext(ctx, logger)
	pod, err := ec.podLister.Pods(namespace).Get(name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			logger.V(5).Info("nothing to do for pod, it is gone")
			return nil
		}
		return err
	}

	// Ignore pods which are already getting deleted.
	if pod.DeletionTimestamp != nil {
		logger.V(5).Info("nothing to do for pod, it is marked for deletion")
		return nil
	}

	var newPodClaims map[string]string
	for _, podClaim := range pod.Spec.ResourceClaims {
		if err := ec.handleClaim(ctx, pod, podClaim, &newPodClaims); err != nil {
			if ec.recorder != nil {
				ec.recorder.Event(pod, v1.EventTypeWarning, "FailedResourceClaimCreation", fmt.Sprintf("PodResourceClaim %s: %v", podClaim.Name, err))
			}
			return fmt.Errorf("pod %s/%s, PodResourceClaim %s: %v", namespace, name, podClaim.Name, err)
		}
	}

	if newPodClaims != nil {
		// Patch the pod status with the new information about
		// generated ResourceClaims.
		statuses := make([]*corev1apply.PodResourceClaimStatusApplyConfiguration, 0, len(newPodClaims))
		for podClaimName, resourceClaimName := range newPodClaims {
			statuses = append(statuses, corev1apply.PodResourceClaimStatus().WithName(podClaimName).WithResourceClaimName(resourceClaimName))
		}
		podApply := corev1apply.Pod(name, namespace).WithStatus(corev1apply.PodStatus().WithResourceClaimStatuses(statuses...))
		if _, err := ec.kubeClient.CoreV1().Pods(namespace).ApplyStatus(ctx, podApply, metav1.ApplyOptions{FieldManager: fieldManager, Force: true}); err != nil {
			return fmt.Errorf("update pod %s/%s ResourceClaimStatuses: %v", namespace, name, err)
		}
	}

	if pod.Spec.NodeName == "" {
		// Scheduler will handle reservations.
		logger.V(5).Info("nothing to do for pod, scheduler will deal with it")
		return nil
	}

	for _, podClaim := range pod.Spec.ResourceClaims {
		claimName, checkOwner, err := resourceclaim.Name(pod, &podClaim)
		if err != nil {
			return err
		}
		// If nil, then it has been determined that the claim is not needed
		// and can be skipped.
		if claimName == nil {
			continue
		}
		claim, err := ec.claimLister.ResourceClaims(pod.Namespace).Get(*claimName)
		if err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return fmt.Errorf("retrieve claim: %v", err)
		}
		if checkOwner {
			if err := resourceclaim.IsForPod(pod, claim); err != nil {
				return err
			}
		}
		if claim.Status.Allocation != nil &&
			!resourceclaim.IsReservedForPod(pod, claim) &&
			resourceclaim.CanBeReserved(claim) {
			logger.V(5).Info("reserve claim for pod", "resourceClaim", klog.KObj(claim))
			if err := ec.reserveForPod(ctx, pod, claim); err != nil {
				return err
			}
		}
	}

	return nil
}

// handleClaim is invoked for each resource claim of a pod.
func (ec *Controller) handleClaim(ctx context.Context, pod *v1.Pod, podClaim v1.PodResourceClaim, newPodClaims *map[string]string) error {
	logger := klog.LoggerWithValues(klog.FromContext(ctx), "podClaim", podClaim.Name)
	ctx = klog.NewContext(ctx, logger)
	logger.V(5).Info("checking", "podClaim", podClaim.Name)

	// resourceclaim.Name checks for the situation that the client doesn't
	// know some future addition to the API. Therefore it gets called here
	// even if there is no template to work on, because if some new field
	// gets added, the expectation might be that the controller does
	// something for it.
	claimName, mustCheckOwner, err := resourceclaim.Name(pod, &podClaim)
	switch {
	case errors.Is(err, resourceclaim.ErrClaimNotFound):
		// Continue below.
	case err != nil:
		return fmt.Errorf("checking for claim before creating it: %v", err)
	case claimName == nil:
		// Nothing to do, no claim needed.
		return nil
	case *claimName != "":
		claimName := *claimName
		// The ResourceClaim should exist because it is recorded in the pod.status.resourceClaimStatuses,
		// but perhaps it was deleted accidentally. In that case we re-create it.
		claim, err := ec.claimLister.ResourceClaims(pod.Namespace).Get(claimName)
		if err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		if claim != nil {
			var err error
			if mustCheckOwner {
				err = resourceclaim.IsForPod(pod, claim)
			}
			if err == nil {
				// Already created, nothing more to do.
				logger.V(5).Info("claim already created", "podClaim", podClaim.Name, "resourceClaim", claimName)
				return nil
			}
			logger.Error(err, "claim that was created for the pod is no longer owned by the pod, creating a new one", "podClaim", podClaim.Name, "resourceClaim", claimName)
		}
	}

	templateName := podClaim.ResourceClaimTemplateName
	if templateName == nil {
		// Nothing to do.
		return nil
	}

	// Before we create a new ResourceClaim, check if there is an orphaned one.
	// This covers the case that the controller has created it, but then fails
	// before it can update the pod status.
	claim, err := ec.findPodResourceClaim(pod, podClaim)
	if err != nil {
		return fmt.Errorf("finding ResourceClaim for claim %s in pod %s/%s failed: %v", podClaim.Name, pod.Namespace, pod.Name, err)
	}

	if claim == nil {
		template, err := ec.templateLister.ResourceClaimTemplates(pod.Namespace).Get(*templateName)
		if err != nil {
			return fmt.Errorf("resource claim template %q: %v", *templateName, err)
		}

		if !ec.adminAccessEnabled && needsAdminAccess(template) {
			return errors.New("admin access is requested, but the feature is disabled")
		}

		// Create the ResourceClaim with pod as owner, with a generated name that uses
		// <pod>-<claim name> as base.
		isTrue := true
		annotations := template.Spec.ObjectMeta.Annotations
		if annotations == nil {
			annotations = make(map[string]string)
		}
		annotations[podResourceClaimAnnotation] = podClaim.Name
		generateName := pod.Name + "-" + podClaim.Name + "-"
		maxBaseLen := 57 // Leave space for hyphen and 5 random characters in a name with 63 characters.
		if len(generateName) > maxBaseLen {
			// We could leave truncation to the apiserver, but as
			// it removes at the end, we would loose everything
			// from the pod claim name when the pod name is long.
			// We can do better and truncate both strings,
			// proportional to their length.
			generateName = pod.Name[0:len(pod.Name)*maxBaseLen/len(generateName)] +
				"-" +
				podClaim.Name[0:len(podClaim.Name)*maxBaseLen/len(generateName)]
		}
		claim = &resourceapi.ResourceClaim{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: generateName,
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion:         "v1",
						Kind:               "Pod",
						Name:               pod.Name,
						UID:                pod.UID,
						Controller:         &isTrue,
						BlockOwnerDeletion: &isTrue,
					},
				},
				Annotations: annotations,
				Labels:      template.Spec.ObjectMeta.Labels,
			},
			Spec: template.Spec.Spec,
		}
		metrics.ResourceClaimCreateAttempts.Inc()
		claimName := claim.Name
		claim, err = ec.kubeClient.ResourceV1beta1().ResourceClaims(pod.Namespace).Create(ctx, claim, metav1.CreateOptions{})
		if err != nil {
			metrics.ResourceClaimCreateFailures.Inc()
			return fmt.Errorf("create ResourceClaim %s: %v", claimName, err)
		}
		logger.V(4).Info("Created ResourceClaim", "claim", klog.KObj(claim), "pod", klog.KObj(pod))
		ec.claimCache.Mutation(claim)
	}

	// Remember the new ResourceClaim for a batch PodStatus update in our caller.
	if *newPodClaims == nil {
		*newPodClaims = make(map[string]string)
	}
	(*newPodClaims)[podClaim.Name] = claim.Name

	return nil
}

func needsAdminAccess(claimTemplate *resourceapi.ResourceClaimTemplate) bool {
	for _, request := range claimTemplate.Spec.Spec.Devices.Requests {
		if ptr.Deref(request.AdminAccess, false) {
			return true
		}
	}
	return false
}

// findPodResourceClaim looks for an existing ResourceClaim with the right
// annotation (ties it to the pod claim) and the right ownership (ties it to
// the pod).
func (ec *Controller) findPodResourceClaim(pod *v1.Pod, podClaim v1.PodResourceClaim) (*resourceapi.ResourceClaim, error) {
	// Only claims owned by the pod will get returned here.
	claims, err := ec.claimCache.ByIndex(claimPodOwnerIndex, string(pod.UID))
	if err != nil {
		return nil, err
	}
	for _, claimObj := range claims {
		claim, ok := claimObj.(*resourceapi.ResourceClaim)
		if !ok {
			return nil, fmt.Errorf("unexpected object of type %T returned by claim cache", claimObj)
		}
		podClaimName, ok := claim.Annotations[podResourceClaimAnnotation]
		// No annotation? Then it cannot be an automatically generated claim
		// and we need to ignore it.
		if !ok {
			continue
		}

		// Not the claim for this particular pod claim?
		if podClaimName != podClaim.Name {
			continue
		}

		// Pick the first one that matches. There shouldn't be more than one. If there is,
		// then all others will be ignored until the pod gets deleted. Then they also get
		// cleaned up.
		return claim, nil
	}
	return nil, nil
}

func (ec *Controller) reserveForPod(ctx context.Context, pod *v1.Pod, claim *resourceapi.ResourceClaim) error {
	claim = claim.DeepCopy()
	claim.Status.ReservedFor = append(claim.Status.ReservedFor,
		resourceapi.ResourceClaimConsumerReference{
			Resource: "pods",
			Name:     pod.Name,
			UID:      pod.UID,
		})
	if _, err := ec.kubeClient.ResourceV1beta1().ResourceClaims(claim.Namespace).UpdateStatus(ctx, claim, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("reserve claim %s for pod: %w", klog.KObj(claim), err)
	}
	return nil
}

func (ec *Controller) syncClaim(ctx context.Context, namespace, name string) error {
	logger := klog.LoggerWithValues(klog.FromContext(ctx), "claim", klog.KRef(namespace, name))
	ctx = klog.NewContext(ctx, logger)
	claim, err := ec.claimLister.ResourceClaims(namespace).Get(name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			logger.V(5).Info("nothing to do for claim, it is gone")
			return nil
		}
		return err
	}

	// Check if the ReservedFor entries are all still valid.
	valid := make([]resourceapi.ResourceClaimConsumerReference, 0, len(claim.Status.ReservedFor))
	for _, reservedFor := range claim.Status.ReservedFor {
		if reservedFor.APIGroup == "" &&
			reservedFor.Resource == "pods" {
			// A pod falls into one of three categories:
			// - we have it in our cache -> don't remove it until we are told that it got removed
			// - we don't have it in our cache anymore, but we have seen it before -> it was deleted, remove it
			// - not in our cache, not seen -> double-check with API server before removal

			keepEntry := true

			// Tracking deleted pods in the LRU cache is an
			// optimization. Without this cache, the code would
			// have to do the API call below for every deleted pod
			// to ensure that the pod really doesn't exist. With
			// the cache, most of the time the pod will be recorded
			// as deleted and the API call can be avoided.
			if ec.deletedObjects.Has(reservedFor.UID) {
				// We know that the pod was deleted. This is
				// easy to check and thus is done first.
				keepEntry = false
			} else {
				pod, err := ec.podLister.Pods(claim.Namespace).Get(reservedFor.Name)
				switch {
				case err != nil && !apierrors.IsNotFound(err):
					return err
				case err != nil:
					// We might not have it in our informer cache
					// yet. Removing the pod while the scheduler is
					// scheduling it would be bad. We have to be
					// absolutely sure and thus have to check with
					// the API server.
					pod, err := ec.kubeClient.CoreV1().Pods(claim.Namespace).Get(ctx, reservedFor.Name, metav1.GetOptions{})
					if err != nil && !apierrors.IsNotFound(err) {
						return err
					}
					if pod == nil || pod.UID != reservedFor.UID {
						logger.V(6).Info("remove reservation because pod is gone or got replaced", "pod", klog.KObj(pod), "claim", klog.KRef(namespace, name))
						keepEntry = false
					}
				case pod.UID != reservedFor.UID:
					logger.V(6).Info("remove reservation because pod got replaced with new instance", "pod", klog.KObj(pod), "claim", klog.KRef(namespace, name))
					keepEntry = false
				case isPodDone(pod):
					logger.V(6).Info("remove reservation because pod will not run anymore", "pod", klog.KObj(pod), "claim", klog.KRef(namespace, name))
					keepEntry = false
				}
			}

			if keepEntry {
				valid = append(valid, reservedFor)
			}
			continue
		}

		// TODO: support generic object lookup
		return fmt.Errorf("unsupported ReservedFor entry: %v", reservedFor)
	}

	builtinControllerFinalizer := slices.Index(claim.Finalizers, resourceapi.Finalizer)
	logger.V(5).Info("claim reserved for counts", "currentCount", len(claim.Status.ReservedFor), "claim", klog.KRef(namespace, name), "updatedCount", len(valid), "builtinController", builtinControllerFinalizer >= 0)
	if len(valid) < len(claim.Status.ReservedFor) {
		// This is not using a patch because we want the update to fail if anything
		// changed in the meantime.
		claim := claim.DeepCopy()
		claim.Status.ReservedFor = valid

		// DRA always performs delayed allocations. Relatedly, it also
		// deallocates a claim as soon as the last consumer stops using
		// it. This ensures that the claim can be allocated again as needed by
		// some future consumer instead of trying to schedule that consumer
		// onto the node that was chosen for the previous consumer. It also
		// releases the underlying resources for use by other claims.
		//
		// This has to be triggered by the transition from "was being used" to
		// "is not used anymore" because a DRA driver is not required to set
		// `status.reservedFor` together with `status.allocation`, i.e. a claim
		// that is "currently unused" should not get deallocated.
		//
		// This does not matter for claims that were created for a pod. For
		// those, the resource claim controller will trigger deletion when the
		// pod is done. However, it doesn't hurt to also trigger deallocation
		// for such claims and not checking for them keeps this code simpler.
		if len(valid) == 0 {
			// This is a sanity check. There shouldn't be any claims without this
			// finalizer because there's no longer any other way of allocating claims.
			// Classic DRA was the alternative earlier.
			if builtinControllerFinalizer >= 0 {
				// Allocated by scheduler with structured parameters. We can "deallocate"
				// by clearing the allocation.
				claim.Status.Allocation = nil
			}
		}

		claim, err := ec.kubeClient.ResourceV1beta1().ResourceClaims(claim.Namespace).UpdateStatus(ctx, claim, metav1.UpdateOptions{})
		if err != nil {
			return err
		}

		// Now also remove the finalizer if it is not needed anymore.
		// Note that the index may have changed as a result of the UpdateStatus call.
		builtinControllerFinalizer := slices.Index(claim.Finalizers, resourceapi.Finalizer)
		if builtinControllerFinalizer >= 0 && claim.Status.Allocation == nil {
			claim.Finalizers = slices.Delete(claim.Finalizers, builtinControllerFinalizer, builtinControllerFinalizer+1)
			if _, err := ec.kubeClient.ResourceV1beta1().ResourceClaims(claim.Namespace).Update(ctx, claim, metav1.UpdateOptions{}); err != nil {
				return err
			}
		}
	} else if builtinControllerFinalizer >= 0 && claim.DeletionTimestamp != nil && len(valid) == 0 {
		claim := claim.DeepCopy()
		if claim.Status.Allocation != nil {
			// This can happen when a claim with immediate allocation
			// stopped being used, remained allocated, and then got
			// deleted. As above we then need to clear the allocation.
			claim.Status.Allocation = nil
			var err error
			claim, err = ec.kubeClient.ResourceV1beta1().ResourceClaims(claim.Namespace).UpdateStatus(ctx, claim, metav1.UpdateOptions{})
			if err != nil {
				return err
			}
		}
		// Whether it was allocated or not, remove the finalizer to unblock removal.
		claim.Finalizers = slices.Delete(claim.Finalizers, builtinControllerFinalizer, builtinControllerFinalizer+1)
		_, err := ec.kubeClient.ResourceV1beta1().ResourceClaims(claim.Namespace).Update(ctx, claim, metav1.UpdateOptions{})
		if err != nil {
			return err
		}
	}

	if len(valid) == 0 {
		// Claim is not reserved. If it was generated for a pod and
		// that pod is not going to run, the claim can be
		// deleted. Normally the garbage collector does that, but the
		// pod itself might not get deleted for a while.
		podName, podUID := owningPod(claim)
		if podName != "" {
			pod, err := ec.podLister.Pods(claim.Namespace).Get(podName)
			switch {
			case err == nil:
				// Pod already replaced or not going to run?
				if pod.UID != podUID || isPodDone(pod) {
					// We are certain that the owning pod is not going to need
					// the claim and therefore remove the claim.
					logger.V(5).Info("deleting unused generated claim", "claim", klog.KObj(claim), "pod", klog.KObj(pod))
					err := ec.kubeClient.ResourceV1beta1().ResourceClaims(claim.Namespace).Delete(ctx, claim.Name, metav1.DeleteOptions{})
					if err != nil {
						return fmt.Errorf("delete claim %s: %w", klog.KObj(claim), err)
					}
				} else {
					logger.V(6).Info("wrong pod content, not deleting claim", "claim", klog.KObj(claim), "podUID", podUID, "podContent", pod)
				}
			case apierrors.IsNotFound(err):
				// We might not know the pod *yet*. Instead of doing an expensive API call,
				// let the garbage collector handle the case that the pod is truly gone.
				logger.V(5).Info("pod for claim not found", "claim", klog.KObj(claim), "pod", klog.KRef(claim.Namespace, podName))
			default:
				return fmt.Errorf("lookup pod: %v", err)
			}
		} else {
			logger.V(5).Info("claim not generated for a pod", "claim", klog.KObj(claim))
		}
	}

	return nil
}

func owningPod(claim *resourceapi.ResourceClaim) (string, types.UID) {
	for _, owner := range claim.OwnerReferences {
		if ptr.Deref(owner.Controller, false) &&
			owner.APIVersion == "v1" &&
			owner.Kind == "Pod" {
			return owner.Name, owner.UID
		}
	}
	return "", ""
}

// podResourceClaimIndexFunc is an index function that returns ResourceClaim keys (=
// namespace/name) for ResourceClaim or ResourceClaimTemplates in a given pod.
func podResourceClaimIndexFunc(obj interface{}) ([]string, error) {
	pod, ok := obj.(*v1.Pod)
	if !ok {
		return []string{}, nil
	}
	keys := []string{}
	for _, podClaim := range pod.Spec.ResourceClaims {
		claimName, _, err := resourceclaim.Name(pod, &podClaim)
		if err != nil || claimName == nil {
			// Index functions are not supposed to fail, the caller will panic.
			// For both error reasons (claim not created yet, unknown API)
			// we simply don't index.
			continue
		}
		keys = append(keys, fmt.Sprintf("%s/%s", pod.Namespace, *claimName))
	}
	return keys, nil
}

// isPodDone returns true if it is certain that none of the containers are running and never will run.
func isPodDone(pod *v1.Pod) bool {
	return podutil.IsPodPhaseTerminal(pod.Status.Phase) ||
		// Deleted and not scheduled:
		pod.DeletionTimestamp != nil && pod.Spec.NodeName == ""
}

// claimPodOwnerIndexFunc is an index function that returns the pod UIDs of
// all pods which own the resource claim. Should only be one, though.
func claimPodOwnerIndexFunc(obj interface{}) ([]string, error) {
	claim, ok := obj.(*resourceapi.ResourceClaim)
	if !ok {
		return nil, nil
	}
	var keys []string
	for _, owner := range claim.OwnerReferences {
		if owner.Controller != nil &&
			*owner.Controller &&
			owner.APIVersion == "v1" &&
			owner.Kind == "Pod" {
			keys = append(keys, string(owner.UID))
		}
	}
	return keys, nil
}
