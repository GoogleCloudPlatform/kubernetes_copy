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

package disruption

import (
	"context"
	"fmt"
	"time"

	apps "k8s.io/api/apps/v1beta1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/api/extensions/v1beta1"
	policy "k8s.io/api/policy/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/discovery"
	appsv1informers "k8s.io/client-go/informers/apps/v1"
	coreinformers "k8s.io/client-go/informers/core/v1"
	policyinformers "k8s.io/client-go/informers/policy/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	v1core "k8s.io/client-go/kubernetes/typed/core/v1"
	appsv1listers "k8s.io/client-go/listers/apps/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	policylisters "k8s.io/client-go/listers/policy/v1"
	scaleclient "k8s.io/client-go/scale"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	pdbhelper "k8s.io/component-helpers/apps/poddisruptionbudget"
	"k8s.io/klog/v2"
	apipod "k8s.io/kubernetes/pkg/api/v1/pod"
	"k8s.io/kubernetes/pkg/controller"
	"k8s.io/utils/clock"
)

const (
	// DeletionTimeout sets maximum time from the moment a pod is added to DisruptedPods in PDB.Status
	// to the time when the pod is expected to be seen by PDB controller as having been marked for deletion.
	// If the pod was not marked for deletion during that time it is assumed that it won't be deleted at
	// all and the corresponding entry can be removed from pdb.Status.DisruptedPods. It is assumed that
	// pod/pdb apiserver to controller latency is relatively small (like 1-2sec) so the below value should
	// be more than enough.
	// If the controller is running on a different node it is important that the two nodes have synced
	// clock (via ntp for example). Otherwise PodDisruptionBudget controller may not provide enough
	// protection against unwanted pod disruptions.
	DeletionTimeout = 2 * time.Minute

	// stalePodDisruptionTimeout sets the maximum time a pod can have a stale
	// DisruptionTarget condition (the condition is present, but the Pod doesn't
	// have a DeletionTimestamp).
	// Once the timeout is reached, this controller attempts to set the status
	// of the condition to False.
	stalePodDisruptionTimeout = 2 * time.Minute
)

type updater func(context.Context, *policy.PodDisruptionBudget) error

type DisruptionController struct {
	kubeClient clientset.Interface
	mapper     apimeta.RESTMapper

	scaleNamespacer scaleclient.ScalesGetter
	discoveryClient discovery.DiscoveryInterface

	pdbLister       policylisters.PodDisruptionBudgetLister
	pdbListerSynced cache.InformerSynced

	podLister       corelisters.PodLister
	podListerSynced cache.InformerSynced

	rcLister       corelisters.ReplicationControllerLister
	rcListerSynced cache.InformerSynced

	rsLister       appsv1listers.ReplicaSetLister
	rsListerSynced cache.InformerSynced

	dLister       appsv1listers.DeploymentLister
	dListerSynced cache.InformerSynced

	ssLister       appsv1listers.StatefulSetLister
	ssListerSynced cache.InformerSynced

	// PodDisruptionBudget keys that need to be synced.
	queue        workqueue.TypedRateLimitingInterface[string]
	recheckQueue workqueue.TypedDelayingInterface[string]

	// pod keys that need to be synced due to a stale DisruptionTarget condition.
	stalePodDisruptionQueue   workqueue.TypedRateLimitingInterface[string]
	stalePodDisruptionTimeout time.Duration

	broadcaster record.EventBroadcaster
	recorder    record.EventRecorder

	getUpdater func() updater

	clock clock.Clock
}

// controllerAndScale is used to return (controller, scale) pairs from the
// controller finder functions.
type controllerAndScale struct {
	types.UID
	scale int32
}

// podControllerFinder is a function type that maps a pod to a list of
// controllers and their scale.
type podControllerFinder func(ctx context.Context, controllerRef *metav1.OwnerReference, namespace string) (*controllerAndScale, error)

func NewDisruptionController(
	ctx context.Context,
	podInformer coreinformers.PodInformer,
	pdbInformer policyinformers.PodDisruptionBudgetInformer,
	rcInformer coreinformers.ReplicationControllerInformer,
	rsInformer appsv1informers.ReplicaSetInformer,
	dInformer appsv1informers.DeploymentInformer,
	ssInformer appsv1informers.StatefulSetInformer,
	kubeClient clientset.Interface,
	restMapper apimeta.RESTMapper,
	scaleNamespacer scaleclient.ScalesGetter,
	discoveryClient discovery.DiscoveryInterface,
) *DisruptionController {
	return NewDisruptionControllerInternal(
		ctx,
		podInformer,
		pdbInformer,
		rcInformer,
		rsInformer,
		dInformer,
		ssInformer,
		kubeClient,
		restMapper,
		scaleNamespacer,
		discoveryClient,
		clock.RealClock{},
		stalePodDisruptionTimeout)
}

// NewDisruptionControllerInternal allows to set a clock and
// stalePodDisruptionTimeout
// It is only supposed to be used by tests.
func NewDisruptionControllerInternal(ctx context.Context,
	podInformer coreinformers.PodInformer,
	pdbInformer policyinformers.PodDisruptionBudgetInformer,
	rcInformer coreinformers.ReplicationControllerInformer,
	rsInformer appsv1informers.ReplicaSetInformer,
	dInformer appsv1informers.DeploymentInformer,
	ssInformer appsv1informers.StatefulSetInformer,
	kubeClient clientset.Interface,
	restMapper apimeta.RESTMapper,
	scaleNamespacer scaleclient.ScalesGetter,
	discoveryClient discovery.DiscoveryInterface,
	clock clock.WithTicker,
	stalePodDisruptionTimeout time.Duration,
) *DisruptionController {
	logger := klog.FromContext(ctx)
	dc := &DisruptionController{
		kubeClient: kubeClient,
		queue: workqueue.NewTypedRateLimitingQueueWithConfig(
			workqueue.DefaultTypedControllerRateLimiter[string](),
			workqueue.TypedRateLimitingQueueConfig[string]{
				DelayingQueue: workqueue.NewTypedDelayingQueueWithConfig(workqueue.TypedDelayingQueueConfig[string]{
					Clock: clock,
					Name:  "disruption",
				}),
			},
		),
		recheckQueue: workqueue.NewTypedDelayingQueueWithConfig(workqueue.TypedDelayingQueueConfig[string]{
			Clock: clock,
			Name:  "disruption_recheck",
		}),
		stalePodDisruptionQueue: workqueue.NewTypedRateLimitingQueueWithConfig(
			workqueue.DefaultTypedControllerRateLimiter[string](),
			workqueue.TypedRateLimitingQueueConfig[string]{
				DelayingQueue: workqueue.NewTypedDelayingQueueWithConfig(workqueue.TypedDelayingQueueConfig[string]{
					Clock: clock,
					Name:  "stale_pod_disruption",
				}),
			},
		),
		broadcaster:               record.NewBroadcaster(record.WithContext(ctx)),
		stalePodDisruptionTimeout: stalePodDisruptionTimeout,
	}
	dc.recorder = dc.broadcaster.NewRecorder(scheme.Scheme, v1.EventSource{Component: "controllermanager"})

	dc.getUpdater = func() updater { return dc.writePdbStatus }

	podInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			dc.addPod(logger, obj)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			dc.updatePod(logger, oldObj, newObj)
		},
		DeleteFunc: func(obj interface{}) {
			dc.deletePod(logger, obj)
		},
	})
	dc.podLister = podInformer.Lister()
	dc.podListerSynced = podInformer.Informer().HasSynced

	pdbInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			dc.addDB(logger, obj)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			dc.updateDB(logger, oldObj, newObj)
		},
		DeleteFunc: func(obj interface{}) {
			dc.removeDB(logger, obj)
		},
	})
	dc.pdbLister = pdbInformer.Lister()
	dc.pdbListerSynced = pdbInformer.Informer().HasSynced

	dc.rcLister = rcInformer.Lister()
	dc.rcListerSynced = rcInformer.Informer().HasSynced

	dc.rsLister = rsInformer.Lister()
	dc.rsListerSynced = rsInformer.Informer().HasSynced

	dc.dLister = dInformer.Lister()
	dc.dListerSynced = dInformer.Informer().HasSynced

	dc.ssLister = ssInformer.Lister()
	dc.ssListerSynced = ssInformer.Informer().HasSynced

	dc.mapper = restMapper
	dc.scaleNamespacer = scaleNamespacer
	dc.discoveryClient = discoveryClient

	dc.clock = clock

	return dc
}

// The workload resources do implement the scale subresource, so it would
// be possible to only check the scale subresource here. But since there is no
// way to take advantage of listers with scale subresources, we use the workload
// resources directly and only fall back to the scale subresource when needed.
func (dc *DisruptionController) finders() []podControllerFinder {
	return []podControllerFinder{dc.getPodReplicationController, dc.getPodDeployment, dc.getPodReplicaSet,
		dc.getPodStatefulSet, dc.getScaleController}
}

var (
	controllerKindRS  = v1beta1.SchemeGroupVersion.WithKind("ReplicaSet")
	controllerKindSS  = apps.SchemeGroupVersion.WithKind("StatefulSet")
	controllerKindRC  = v1.SchemeGroupVersion.WithKind("ReplicationController")
	controllerKindDep = v1beta1.SchemeGroupVersion.WithKind("Deployment")
)

// getPodReplicaSet finds a replicaset which has no matching deployments.
func (dc *DisruptionController) getPodReplicaSet(ctx context.Context, controllerRef *metav1.OwnerReference, namespace string) (*controllerAndScale, error) {
	ok, err := verifyGroupKind(controllerRef, controllerKindRS.Kind, []string{"apps", "extensions"})
	if !ok || err != nil {
		return nil, err
	}
	rs, err := dc.rsLister.ReplicaSets(namespace).Get(controllerRef.Name)
	if err != nil {
		// The only possible error is NotFound, which is ok here.
		return nil, nil
	}
	if rs.UID != controllerRef.UID {
		return nil, nil
	}
	controllerRef = metav1.GetControllerOf(rs)
	if controllerRef != nil && controllerRef.Kind == controllerKindDep.Kind {
		// Skip RS if it's controlled by a Deployment.
		return nil, nil
	}
	return &controllerAndScale{rs.UID, *(rs.Spec.Replicas)}, nil
}

// getPodStatefulSet returns the statefulset referenced by the provided controllerRef.
func (dc *DisruptionController) getPodStatefulSet(ctx context.Context, controllerRef *metav1.OwnerReference, namespace string) (*controllerAndScale, error) {
	ok, err := verifyGroupKind(controllerRef, controllerKindSS.Kind, []string{"apps"})
	if !ok || err != nil {
		return nil, err
	}
	ss, err := dc.ssLister.StatefulSets(namespace).Get(controllerRef.Name)
	if err != nil {
		// The only possible error is NotFound, which is ok here.
		return nil, nil
	}
	if ss.UID != controllerRef.UID {
		return nil, nil
	}

	return &controllerAndScale{ss.UID, *(ss.Spec.Replicas)}, nil
}

// getPodDeployments finds deployments for any replicasets which are being managed by deployments.
func (dc *DisruptionController) getPodDeployment(ctx context.Context, controllerRef *metav1.OwnerReference, namespace string) (*controllerAndScale, error) {
	ok, err := verifyGroupKind(controllerRef, controllerKindRS.Kind, []string{"apps", "extensions"})
	if !ok || err != nil {
		return nil, err
	}
	rs, err := dc.rsLister.ReplicaSets(namespace).Get(controllerRef.Name)
	if err != nil {
		// The only possible error is NotFound, which is ok here.
		return nil, nil
	}
	if rs.UID != controllerRef.UID {
		return nil, nil
	}
	controllerRef = metav1.GetControllerOf(rs)
	if controllerRef == nil {
		return nil, nil
	}

	ok, err = verifyGroupKind(controllerRef, controllerKindDep.Kind, []string{"apps", "extensions"})
	if !ok || err != nil {
		return nil, err
	}
	deployment, err := dc.dLister.Deployments(rs.Namespace).Get(controllerRef.Name)
	if err != nil {
		// The only possible error is NotFound, which is ok here.
		return nil, nil
	}
	if deployment.UID != controllerRef.UID {
		return nil, nil
	}
	return &controllerAndScale{deployment.UID, *(deployment.Spec.Replicas)}, nil
}

func (dc *DisruptionController) getPodReplicationController(ctx context.Context, controllerRef *metav1.OwnerReference, namespace string) (*controllerAndScale, error) {
	ok, err := verifyGroupKind(controllerRef, controllerKindRC.Kind, []string{""})
	if !ok || err != nil {
		return nil, err
	}
	rc, err := dc.rcLister.ReplicationControllers(namespace).Get(controllerRef.Name)
	if err != nil {
		// The only possible error is NotFound, which is ok here.
		return nil, nil
	}
	if rc.UID != controllerRef.UID {
		return nil, nil
	}
	return &controllerAndScale{rc.UID, *(rc.Spec.Replicas)}, nil
}

func (dc *DisruptionController) getScaleController(ctx context.Context, controllerRef *metav1.OwnerReference, namespace string) (*controllerAndScale, error) {
	gv, err := schema.ParseGroupVersion(controllerRef.APIVersion)
	if err != nil {
		return nil, err
	}

	gk := schema.GroupKind{
		Group: gv.Group,
		Kind:  controllerRef.Kind,
	}

	mapping, err := dc.mapper.RESTMapping(gk, gv.Version)
	if err != nil {
		return nil, err
	}
	gr := mapping.Resource.GroupResource()
	scale, err := dc.scaleNamespacer.Scales(namespace).Get(ctx, gr, controllerRef.Name, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			// The IsNotFound error can mean either that the resource does not exist,
			// or it exist but doesn't implement the scale subresource. We check which
			// situation we are facing so we can give an appropriate error message.
			isScale, err := dc.implementsScale(mapping.Resource)
			if err != nil {
				return nil, err
			}
			if !isScale {
				return nil, fmt.Errorf("%s does not implement the scale subresource", gr.String())
			}
			return nil, nil
		}
		return nil, err
	}
	if scale.UID != controllerRef.UID {
		return nil, nil
	}
	return &controllerAndScale{scale.UID, scale.Spec.Replicas}, nil
}

func (dc *DisruptionController) implementsScale(gvr schema.GroupVersionResource) (bool, error) {
	resourceList, err := dc.discoveryClient.ServerResourcesForGroupVersion(gvr.GroupVersion().String())
	if err != nil {
		return false, err
	}

	scaleSubresourceName := fmt.Sprintf("%s/scale", gvr.Resource)
	for _, resource := range resourceList.APIResources {
		if resource.Name != scaleSubresourceName {
			continue
		}

		for _, scaleGv := range scaleclient.NewScaleConverter().ScaleVersions() {
			if resource.Group == scaleGv.Group &&
				resource.Version == scaleGv.Version &&
				resource.Kind == "Scale" {
				return true, nil
			}
		}
	}
	return false, nil
}

func verifyGroupKind(controllerRef *metav1.OwnerReference, expectedKind string, expectedGroups []string) (bool, error) {
	gv, err := schema.ParseGroupVersion(controllerRef.APIVersion)
	if err != nil {
		return false, err
	}

	if controllerRef.Kind != expectedKind {
		return false, nil
	}

	for _, group := range expectedGroups {
		if group == gv.Group {
			return true, nil
		}
	}

	return false, nil
}

func (dc *DisruptionController) Run(ctx context.Context) {
	defer utilruntime.HandleCrash()

	logger := klog.FromContext(ctx)
	// Start events processing pipeline.
	if dc.kubeClient != nil {
		logger.Info("Sending events to api server.")
		dc.broadcaster.StartRecordingToSink(&v1core.EventSinkImpl{Interface: dc.kubeClient.CoreV1().Events("")})
	} else {
		logger.Info("No api server defined - no events will be sent to API server.")
	}
	defer dc.broadcaster.Shutdown()

	defer dc.queue.ShutDown()
	defer dc.recheckQueue.ShutDown()
	defer dc.stalePodDisruptionQueue.ShutDown()

	logger.Info("Starting disruption controller")
	defer logger.Info("Shutting down disruption controller")

	if !cache.WaitForNamedCacheSync("disruption", ctx.Done(), dc.podListerSynced, dc.pdbListerSynced, dc.rcListerSynced, dc.rsListerSynced, dc.dListerSynced, dc.ssListerSynced) {
		return
	}

	go wait.UntilWithContext(ctx, dc.worker, time.Second)
	go wait.Until(dc.recheckWorker, time.Second, ctx.Done())
	go wait.UntilWithContext(ctx, dc.stalePodDisruptionWorker, time.Second)

	<-ctx.Done()
}

func (dc *DisruptionController) addDB(logger klog.Logger, obj interface{}) {
	pdb := obj.(*policy.PodDisruptionBudget)
	logger.V(4).Info("Add DB", "podDisruptionBudget", klog.KObj(pdb))
	dc.enqueuePdb(logger, pdb)
}

func (dc *DisruptionController) updateDB(logger klog.Logger, old, cur interface{}) {
	// TODO(mml) ignore updates where 'old' is equivalent to 'cur'.
	pdb := cur.(*policy.PodDisruptionBudget)
	logger.V(4).Info("Update DB", "podDisruptionBudget", klog.KObj(pdb))
	dc.enqueuePdb(logger, pdb)
}

func (dc *DisruptionController) removeDB(logger klog.Logger, obj interface{}) {
	pdb, ok := obj.(*policy.PodDisruptionBudget)
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			logger.Error(nil, "Couldn't get object from tombstone", "obj", obj)
			return
		}
		pdb, ok = tombstone.Obj.(*policy.PodDisruptionBudget)
		if !ok {
			logger.Error(nil, "Tombstone contained object that is not a PDB", "obj", obj)
			return
		}
	}
	logger.V(4).Info("Remove DB", "podDisruptionBudget", klog.KObj(pdb))
	dc.enqueuePdb(logger, pdb)
}

func (dc *DisruptionController) addPod(logger klog.Logger, obj interface{}) {
	pod := obj.(*v1.Pod)
	logger.V(4).Info("AddPod called on pod", "pod", klog.KObj(pod))
	pdb := dc.getPdbForPod(logger, pod)
	if pdb == nil {
		logger.V(4).Info("No matching PDB for pod", "pod", klog.KObj(pod))
	} else {
		logger.V(4).Info("addPod -> PDB", "pod", klog.KObj(pod), "podDisruptionBudget", klog.KObj(pdb))
		dc.enqueuePdb(logger, pdb)
	}
	if has, cleanAfter := dc.nonTerminatingPodHasStaleDisruptionCondition(pod); has {
		dc.enqueueStalePodDisruptionCleanup(logger, pod, cleanAfter)
	}
}

func (dc *DisruptionController) updatePod(logger klog.Logger, _, cur interface{}) {
	pod := cur.(*v1.Pod)
	logger.V(4).Info("UpdatePod called on pod", "pod", klog.KObj(pod))
	pdb := dc.getPdbForPod(logger, pod)
	if pdb == nil {
		logger.V(4).Info("No matching PDB for pod", "pod", klog.KObj(pod))
	} else {
		logger.V(4).Info("updatePod -> PDB", "pod", klog.KObj(pod), "podDisruptionBudget", klog.KObj(pdb))
		dc.enqueuePdb(logger, pdb)
	}
	if has, cleanAfter := dc.nonTerminatingPodHasStaleDisruptionCondition(pod); has {
		dc.enqueueStalePodDisruptionCleanup(logger, pod, cleanAfter)
	}
}

func (dc *DisruptionController) deletePod(logger klog.Logger, obj interface{}) {
	pod, ok := obj.(*v1.Pod)
	// When a delete is dropped, the relist will notice a pod in the store not
	// in the list, leading to the insertion of a tombstone object which contains
	// the deleted key/value. Note that this value might be stale. If the pod
	// changed labels the new ReplicaSet will not be woken up till the periodic
	// resync.
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			logger.Error(nil, "Couldn't get object from tombstone", "obj", obj)
			return
		}
		pod, ok = tombstone.Obj.(*v1.Pod)
		if !ok {
			logger.Error(nil, "Tombstone contained object that is not a pod", "obj", obj)
			return
		}
	}
	logger.V(4).Info("DeletePod called on pod", "pod", klog.KObj(pod))
	pdb := dc.getPdbForPod(logger, pod)
	if pdb == nil {
		logger.V(4).Info("No matching PDB for pod", "pod", klog.KObj(pod))
		return
	}
	logger.V(4).Info("DeletePod -> PDB", "pod", klog.KObj(pod), "podDisruptionBudget", klog.KObj(pdb))
	dc.enqueuePdb(logger, pdb)
}

func (dc *DisruptionController) enqueuePdb(logger klog.Logger, pdb *policy.PodDisruptionBudget) {
	key, err := controller.KeyFunc(pdb)
	if err != nil {
		logger.Error(err, "Couldn't get key for PodDisruptionBudget", "podDisruptionBudget", klog.KObj(pdb))
		return
	}
	dc.queue.Add(key)
}

func (dc *DisruptionController) enqueuePdbForRecheck(logger klog.Logger, pdb *policy.PodDisruptionBudget, delay time.Duration) {
	key, err := controller.KeyFunc(pdb)
	if err != nil {
		logger.Error(err, "Couldn't get key for PodDisruptionBudget", "podDisruptionBudget", klog.KObj(pdb))
		return
	}
	dc.recheckQueue.AddAfter(key, delay)
}

func (dc *DisruptionController) enqueueStalePodDisruptionCleanup(logger klog.Logger, pod *v1.Pod, d time.Duration) {
	key, err := controller.KeyFunc(pod)
	if err != nil {
		logger.Error(err, "Couldn't get key for Pod object", "pod", klog.KObj(pod))
		return
	}
	dc.stalePodDisruptionQueue.AddAfter(key, d)
	logger.V(4).Info("Enqueued pod to cleanup stale DisruptionTarget condition", "pod", klog.KObj(pod))
}

func (dc *DisruptionController) getPdbForPod(logger klog.Logger, pod *v1.Pod) *policy.PodDisruptionBudget {
	// GetPodPodDisruptionBudgets returns an error only if no
	// PodDisruptionBudgets are found.  We don't return that as an error to the
	// caller.
	pdbs, err := dc.pdbLister.GetPodPodDisruptionBudgets(pod)
	if err != nil {
		logger.V(4).Info("No PodDisruptionBudgets found for pod, PodDisruptionBudget controller will avoid syncing.", "pod", klog.KObj(pod))
		return nil
	}

	if len(pdbs) > 1 {
		msg := fmt.Sprintf("Pod %q/%q matches multiple PodDisruptionBudgets.  Chose %q arbitrarily.", pod.Namespace, pod.Name, pdbs[0].Name)
		logger.Info(msg)
		dc.recorder.Event(pod, v1.EventTypeWarning, "MultiplePodDisruptionBudgets", msg)
	}
	return pdbs[0]
}

// This function returns pods using the PodDisruptionBudget object.
// IMPORTANT NOTE : the returned pods should NOT be modified.
func (dc *DisruptionController) getPodsForPdb(pdb *policy.PodDisruptionBudget) ([]*v1.Pod, error) {
	sel, err := metav1.LabelSelectorAsSelector(pdb.Spec.Selector)
	if err != nil {
		return []*v1.Pod{}, err
	}
	pods, err := dc.podLister.Pods(pdb.Namespace).List(sel)
	if err != nil {
		return []*v1.Pod{}, err
	}
	return pods, nil
}

func (dc *DisruptionController) worker(ctx context.Context) {
	for dc.processNextWorkItem(ctx) {
	}
}

func (dc *DisruptionController) processNextWorkItem(ctx context.Context) bool {
	dKey, quit := dc.queue.Get()
	if quit {
		return false
	}
	defer dc.queue.Done(dKey)

	err := dc.sync(ctx, dKey)
	if err == nil {
		dc.queue.Forget(dKey)
		return true
	}

	utilruntime.HandleError(fmt.Errorf("Error syncing PodDisruptionBudget %v, requeuing: %w", dKey, err)) //nolint:stylecheck
	dc.queue.AddRateLimited(dKey)

	return true
}

func (dc *DisruptionController) recheckWorker() {
	for dc.processNextRecheckWorkItem() {
	}
}

func (dc *DisruptionController) processNextRecheckWorkItem() bool {
	dKey, quit := dc.recheckQueue.Get()
	if quit {
		return false
	}
	defer dc.recheckQueue.Done(dKey)
	dc.queue.AddRateLimited(dKey)
	return true
}

func (dc *DisruptionController) stalePodDisruptionWorker(ctx context.Context) {
	for dc.processNextStalePodDisruptionWorkItem(ctx) {
	}
}

func (dc *DisruptionController) processNextStalePodDisruptionWorkItem(ctx context.Context) bool {
	key, quit := dc.stalePodDisruptionQueue.Get()
	if quit {
		return false
	}
	defer dc.stalePodDisruptionQueue.Done(key)
	err := dc.syncStalePodDisruption(ctx, key)
	if err == nil {
		dc.stalePodDisruptionQueue.Forget(key)
		return true
	}
	utilruntime.HandleError(fmt.Errorf("error syncing Pod %v to clear DisruptionTarget condition, requeueing: %w", key, err))
	dc.stalePodDisruptionQueue.AddRateLimited(key)
	return true
}

func (dc *DisruptionController) sync(ctx context.Context, key string) error {
	logger := klog.FromContext(ctx)
	startTime := dc.clock.Now()
	defer func() {
		logger.V(4).Info("Finished syncing PodDisruptionBudget", "key", key, "duration", dc.clock.Since(startTime))
	}()

	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}
	pdb, err := dc.pdbLister.PodDisruptionBudgets(namespace).Get(name)
	if errors.IsNotFound(err) {
		logger.V(4).Info("podDisruptionBudget has been deleted", "key", key)
		return nil
	}
	if err != nil {
		return err
	}

	err = dc.trySync(ctx, pdb)
	// If the reason for failure was a conflict, then allow this PDB update to be
	// requeued without triggering the failSafe logic.
	if errors.IsConflict(err) {
		return err
	}
	if err != nil {
		logger.Error(err, "Failed to sync PDB", "podDisruptionBudget", klog.KRef(namespace, name))
		return dc.failSafe(ctx, pdb, err)
	}

	return nil
}

func (dc *DisruptionController) trySync(ctx context.Context, pdb *policy.PodDisruptionBudget) error {
	logger := klog.FromContext(ctx)
	pods, err := dc.getPodsForPdb(pdb)
	if err != nil {
		dc.recorder.Eventf(pdb, v1.EventTypeWarning, "NoPods", "Failed to get pods: %v", err)
		return err
	}
	if len(pods) == 0 {
		dc.recorder.Eventf(pdb, v1.EventTypeNormal, "NoPods", "No matching pods found")
	}

	expectedCount, desiredHealthy, unmanagedPods, err := dc.getExpectedPodCount(ctx, pdb, pods)
	if err != nil {
		dc.recorder.Eventf(pdb, v1.EventTypeWarning, "CalculateExpectedPodCountFailed", "Failed to calculate the number of expected pods: %v", err)
		return err
	}
	// We have unmanaged pods, instead of erroring and hotlooping in disruption controller, log and continue.
	if len(unmanagedPods) > 0 {
		logger.V(4).Info("Found unmanaged pods associated with this PDB", "pods", unmanagedPods)
		dc.recorder.Eventf(pdb, v1.EventTypeWarning, "UnmanagedPods", "Pods selected by this PodDisruptionBudget (selector: %v) were found "+
			"to be unmanaged. As a result, the status of the PDB cannot be calculated correctly, which may result in undefined behavior. "+
			"To account for these pods please set \".spec.minAvailable\" "+
			"field of the PDB to an integer value.", pdb.Spec.Selector)
	}

	currentTime := dc.clock.Now()
	disruptedPods, recheckTime := dc.buildDisruptedPodMap(logger, pods, pdb, currentTime)
	currentHealthy := countHealthyPods(pods, disruptedPods, currentTime)
	err = dc.updatePdbStatus(ctx, pdb, currentHealthy, desiredHealthy, expectedCount, disruptedPods)

	if err == nil && recheckTime != nil {
		// There is always at most one PDB waiting with a particular name in the queue,
		// and each PDB in the queue is associated with the lowest timestamp
		// that was supplied when a PDB with that name was added.
		dc.enqueuePdbForRecheck(logger, pdb, recheckTime.Sub(currentTime))
	}
	return err
}

func (dc *DisruptionController) syncStalePodDisruption(ctx context.Context, key string) error {
	logger := klog.FromContext(ctx)
	startTime := dc.clock.Now()
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}
	defer func() {
		logger.V(4).Info("Finished syncing Pod to clear DisruptionTarget condition", "pod", klog.KRef(namespace, name), "duration", dc.clock.Since(startTime))
	}()
	pod, err := dc.podLister.Pods(namespace).Get(name)
	if errors.IsNotFound(err) {
		logger.V(4).Info("Skipping clearing DisruptionTarget condition because pod was deleted", "pod", klog.KObj(pod))
		return nil
	}
	if err != nil {
		return err
	}

	hasCond, cleanAfter := dc.nonTerminatingPodHasStaleDisruptionCondition(pod)
	if !hasCond {
		return nil
	}
	if cleanAfter > 0 {
		dc.enqueueStalePodDisruptionCleanup(logger, pod, cleanAfter)
		return nil
	}

	newPod := pod.DeepCopy()
	updated := apipod.UpdatePodCondition(&newPod.Status, &v1.PodCondition{
		Type:   v1.DisruptionTarget,
		Status: v1.ConditionFalse,
	})
	if !updated {
		return nil
	}
	if _, err := dc.kubeClient.CoreV1().Pods(pod.Namespace).UpdateStatus(ctx, newPod, metav1.UpdateOptions{}); err != nil {
		return err
	}
	logger.V(2).Info("Reset stale DisruptionTarget condition to False", "pod", klog.KObj(pod))
	return nil
}

func (dc *DisruptionController) getExpectedPodCount(ctx context.Context, pdb *policy.PodDisruptionBudget, pods []*v1.Pod) (expectedCount, desiredHealthy int32, unmanagedPods []string, err error) {
	err = nil
	// TODO(davidopp): consider making the way expectedCount and rules about
	// permitted controller configurations (specifically, considering it an error
	// if a pod covered by a PDB has 0 controllers or > 1 controller) should be
	// handled the same way for integer and percentage minAvailable

	if pdb.Spec.MaxUnavailable != nil {
		expectedCount, unmanagedPods, err = dc.getExpectedScale(ctx, pdb, pods)
		if err != nil {
			return
		}
		var maxUnavailable int
		maxUnavailable, err = intstr.GetScaledValueFromIntOrPercent(pdb.Spec.MaxUnavailable, int(expectedCount), true)
		if err != nil {
			return
		}
		desiredHealthy = expectedCount - int32(maxUnavailable)
		if desiredHealthy < 0 {
			desiredHealthy = 0
		}
	} else if pdb.Spec.MinAvailable != nil {
		if pdb.Spec.MinAvailable.Type == intstr.Int {
			desiredHealthy = pdb.Spec.MinAvailable.IntVal
			expectedCount = int32(len(pods))
		} else if pdb.Spec.MinAvailable.Type == intstr.String {
			expectedCount, unmanagedPods, err = dc.getExpectedScale(ctx, pdb, pods)
			if err != nil {
				return
			}

			var minAvailable int
			minAvailable, err = intstr.GetScaledValueFromIntOrPercent(pdb.Spec.MinAvailable, int(expectedCount), true)
			if err != nil {
				return
			}
			desiredHealthy = int32(minAvailable)
		}
	}
	return
}

func (dc *DisruptionController) getExpectedScale(ctx context.Context, pdb *policy.PodDisruptionBudget, pods []*v1.Pod) (expectedCount int32, unmanagedPods []string, err error) {
	// When the user specifies a fraction of pods that must be available, we
	// use as the fraction's denominator
	// SUM_{all c in C} scale(c)
	// where C is the union of C_p1, C_p2, ..., C_pN
	// and each C_pi is the set of controllers controlling the pod pi

	// k8s only defines what will happens when 0 or 1 controllers control a
	// given pod.  We explicitly exclude the 0 controllers case here, and we
	// report an error if we find a pod with more than 1 controller.  Thus in
	// practice each C_pi is a set of exactly 1 controller.

	// A mapping from controllers to their scale.
	controllerScale := map[types.UID]int32{}

	// 1. Find the controller for each pod.

	// As of now, we allow PDBs to be applied to pods via selectors, so there
	// can be unmanaged pods(pods that don't have backing controllers) but still have PDBs associated.
	// Such pods are to be collected and PDB backing them should be enqueued instead of immediately throwing
	// a sync error. This ensures disruption controller is not frequently updating the status subresource and thus
	// preventing excessive and expensive writes to etcd.
	// With ControllerRef, a pod can only have 1 controller.
	for _, pod := range pods {
		controllerRef := metav1.GetControllerOf(pod)
		if controllerRef == nil {
			unmanagedPods = append(unmanagedPods, pod.Name)
			continue
		}

		// If we already know the scale of the controller there is no need to do anything.
		if _, found := controllerScale[controllerRef.UID]; found {
			continue
		}

		// Check all the supported controllers to find the desired scale.
		foundController := false
		for _, finder := range dc.finders() {
			var controllerNScale *controllerAndScale
			controllerNScale, err = finder(ctx, controllerRef, pod.Namespace)
			if err != nil {
				return
			}
			if controllerNScale != nil {
				controllerScale[controllerNScale.UID] = controllerNScale.scale
				foundController = true
				break
			}
		}
		if !foundController {
			err = fmt.Errorf("found no controllers for pod %q", pod.Name)
			return
		}
	}

	// 2. Add up all the controllers.
	expectedCount = 0
	for _, count := range controllerScale {
		expectedCount += count
	}

	return
}

func countHealthyPods(pods []*v1.Pod, disruptedPods map[string]metav1.Time, currentTime time.Time) (currentHealthy int32) {
	for _, pod := range pods {
		// Pod is being deleted.
		if pod.DeletionTimestamp != nil {
			continue
		}
		// Pod is expected to be deleted soon.
		if disruptionTime, found := disruptedPods[pod.Name]; found && disruptionTime.Time.Add(DeletionTimeout).After(currentTime) {
			continue
		}
		if apipod.IsPodReady(pod) {
			currentHealthy++
		}
	}

	return
}

// Builds new PodDisruption map, possibly removing items that refer to non-existing, already deleted
// or not-deleted at all items. Also returns an information when this check should be repeated.
func (dc *DisruptionController) buildDisruptedPodMap(logger klog.Logger, pods []*v1.Pod, pdb *policy.PodDisruptionBudget, currentTime time.Time) (map[string]metav1.Time, *time.Time) {
	disruptedPods := pdb.Status.DisruptedPods
	result := make(map[string]metav1.Time)
	var recheckTime *time.Time

	if disruptedPods == nil {
		return result, recheckTime
	}
	for _, pod := range pods {
		if pod.DeletionTimestamp != nil {
			// Already being deleted.
			continue
		}
		disruptionTime, found := disruptedPods[pod.Name]
		if !found {
			// Pod not on the list.
			continue
		}
		expectedDeletion := disruptionTime.Time.Add(DeletionTimeout)
		if expectedDeletion.Before(currentTime) {
			logger.V(1).Info("pod was expected to be deleted but it wasn't, updating PDB",
				"pod", klog.KObj(pod), "deletionTime", disruptionTime, "podDisruptionBudget", klog.KObj(pdb))
			dc.recorder.Eventf(pod, v1.EventTypeWarning, "NotDeleted", "Pod was expected by PDB %s/%s to be deleted but it wasn't",
				pdb.Namespace, pdb.Namespace)
		} else {
			if recheckTime == nil || expectedDeletion.Before(*recheckTime) {
				recheckTime = &expectedDeletion
			}
			result[pod.Name] = disruptionTime
		}
	}
	return result, recheckTime
}

// failSafe is an attempt to at least update the DisruptionsAllowed field to
// 0 if everything else has failed.  This is one place we
// implement the  "fail open" part of the design since if we manage to update
// this field correctly, we will prevent the /evict handler from approving an
// eviction when it may be unsafe to do so.
func (dc *DisruptionController) failSafe(ctx context.Context, pdb *policy.PodDisruptionBudget, err error) error {
	newPdb := pdb.DeepCopy()
	newPdb.Status.DisruptionsAllowed = 0

	if newPdb.Status.Conditions == nil {
		newPdb.Status.Conditions = make([]metav1.Condition, 0)
	}
	apimeta.SetStatusCondition(&newPdb.Status.Conditions, metav1.Condition{
		Type:               policy.DisruptionAllowedCondition,
		Status:             metav1.ConditionFalse,
		Reason:             policy.SyncFailedReason,
		Message:            err.Error(),
		ObservedGeneration: newPdb.Status.ObservedGeneration,
	})

	return dc.getUpdater()(ctx, newPdb)
}

func (dc *DisruptionController) updatePdbStatus(ctx context.Context, pdb *policy.PodDisruptionBudget, currentHealthy, desiredHealthy, expectedCount int32,
	disruptedPods map[string]metav1.Time) error {

	// We require expectedCount to be > 0 so that PDBs which currently match no
	// pods are in a safe state when their first pods appear but this controller
	// has not updated their status yet.  This isn't the only race, but it's a
	// common one that's easy to detect.
	disruptionsAllowed := currentHealthy - desiredHealthy
	if expectedCount <= 0 || disruptionsAllowed <= 0 {
		disruptionsAllowed = 0
	}

	if pdb.Status.CurrentHealthy == currentHealthy &&
		pdb.Status.DesiredHealthy == desiredHealthy &&
		pdb.Status.ExpectedPods == expectedCount &&
		pdb.Status.DisruptionsAllowed == disruptionsAllowed &&
		apiequality.Semantic.DeepEqual(pdb.Status.DisruptedPods, disruptedPods) &&
		pdb.Status.ObservedGeneration == pdb.Generation &&
		pdbhelper.ConditionsAreUpToDate(pdb) {
		return nil
	}

	newPdb := pdb.DeepCopy()
	newPdb.Status = policy.PodDisruptionBudgetStatus{
		CurrentHealthy:     currentHealthy,
		DesiredHealthy:     desiredHealthy,
		ExpectedPods:       expectedCount,
		DisruptionsAllowed: disruptionsAllowed,
		DisruptedPods:      disruptedPods,
		ObservedGeneration: pdb.Generation,
		Conditions:         newPdb.Status.Conditions,
	}

	pdbhelper.UpdateDisruptionAllowedCondition(newPdb)

	return dc.getUpdater()(ctx, newPdb)
}

func (dc *DisruptionController) writePdbStatus(ctx context.Context, pdb *policy.PodDisruptionBudget) error {
	// If this update fails, don't retry it. Allow the failure to get handled &
	// retried in `processNextWorkItem()`.
	_, err := dc.kubeClient.PolicyV1().PodDisruptionBudgets(pdb.Namespace).UpdateStatus(ctx, pdb, metav1.UpdateOptions{})
	return err
}

func (dc *DisruptionController) nonTerminatingPodHasStaleDisruptionCondition(pod *v1.Pod) (bool, time.Duration) {
	if pod.DeletionTimestamp != nil {
		return false, 0
	}
	_, cond := apipod.GetPodCondition(&pod.Status, v1.DisruptionTarget)
	// Pod disruption conditions added by kubelet are never considered stale because the condition might take
	// arbitrarily long before the pod is terminating (has deletion timestamp). Also, pod conditions present
	// on pods in terminal phase are not stale to avoid unnecessary status updates.
	if cond == nil || cond.Status != v1.ConditionTrue || cond.Reason == v1.PodReasonTerminationByKubelet || apipod.IsPodPhaseTerminal(pod.Status.Phase) {
		return false, 0
	}
	waitFor := dc.stalePodDisruptionTimeout - dc.clock.Since(cond.LastTransitionTime.Time)
	if waitFor < 0 {
		waitFor = 0
	}
	return true, waitFor
}
