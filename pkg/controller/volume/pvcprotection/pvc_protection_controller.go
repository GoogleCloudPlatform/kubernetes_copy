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

package pvcprotection

import (
	"context"
	"fmt"
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	coreinformers "k8s.io/client-go/informers/core/v1"
	clientset "k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/component-helpers/storage/ephemeral"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/controller/volume/common"
	"k8s.io/kubernetes/pkg/controller/volume/protectionutil"
	"k8s.io/kubernetes/pkg/util/slice"
	volumeutil "k8s.io/kubernetes/pkg/volume/util"
)

// Controller is controller that removes PVCProtectionFinalizer
// from PVCs that are used by no pods.

type LazyLivePodList struct {
	cache      []v1.Pod
	controller *Controller
}

func (ll *LazyLivePodList) getCache() []v1.Pod {
	return ll.cache
}

func (ll *LazyLivePodList) setCache(pods []v1.Pod) {
	ll.cache = pods
}

type pvcData struct {
	pvcKey  string
	pvcName string
}

type pvcProcessingStore struct {
	namespaceToPVCsMap map[string][]pvcData
	namespaceQueue     workqueue.TypedInterface[string]
	mu                 sync.Mutex
}

func NewPVCProcessingStore() *pvcProcessingStore {
	return &pvcProcessingStore{
		namespaceToPVCsMap: make(map[string][]pvcData),
		namespaceQueue:     workqueue.NewTyped[string](),
	}
}

func (m *pvcProcessingStore) addOrUpdate(namespace string, pvcKey, pvcName string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.namespaceToPVCsMap[namespace]; !exists {
		m.namespaceToPVCsMap[namespace] = make([]pvcData, 0)
		m.namespaceQueue.Add(namespace)
	}
	m.namespaceToPVCsMap[namespace] = append(m.namespaceToPVCsMap[namespace], pvcData{pvcKey: pvcKey, pvcName: pvcName})
}

// Returns a list of pvcs and the associated namespace to be processed downstream
func (m *pvcProcessingStore) flushNextPVCsByNamespace() ([]pvcData, string) {

	nextNamespace, quit := m.namespaceQueue.Get()
	if quit {
		return nil, nextNamespace
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	pvcs := m.namespaceToPVCsMap[nextNamespace]

	delete(m.namespaceToPVCsMap, nextNamespace)
	m.namespaceQueue.Done(nextNamespace)
	return pvcs, nextNamespace
}

type Controller struct {
	client clientset.Interface

	pvcLister       corelisters.PersistentVolumeClaimLister
	pvcListerSynced cache.InformerSynced

	podLister       corelisters.PodLister
	podListerSynced cache.InformerSynced
	podIndexer      cache.Indexer

	queue              workqueue.TypedRateLimitingInterface[string]
	pvcProcessingStore *pvcProcessingStore
}

// NewPVCProtectionController returns a new instance of PVCProtectionController.
func NewPVCProtectionController(logger klog.Logger, pvcInformer coreinformers.PersistentVolumeClaimInformer, podInformer coreinformers.PodInformer, cl clientset.Interface) (*Controller, error) {
	e := &Controller{
		client: cl,
		queue: workqueue.NewTypedRateLimitingQueueWithConfig(
			workqueue.DefaultTypedControllerRateLimiter[string](),
			workqueue.TypedRateLimitingQueueConfig[string]{Name: "pvcprotection"},
		),
		pvcProcessingStore: NewPVCProcessingStore(),
	}

	e.pvcLister = pvcInformer.Lister()
	e.pvcListerSynced = pvcInformer.Informer().HasSynced
	pvcInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			e.pvcAddedUpdated(logger, obj)
		},
		UpdateFunc: func(old, new interface{}) {
			e.pvcAddedUpdated(logger, new)
		},
	})

	e.podLister = podInformer.Lister()
	e.podListerSynced = podInformer.Informer().HasSynced
	e.podIndexer = podInformer.Informer().GetIndexer()
	if err := common.AddIndexerIfNotPresent(e.podIndexer, common.PodPVCIndex, common.PodPVCIndexFunc()); err != nil {
		return nil, fmt.Errorf("could not initialize pvc protection controller: %w", err)
	}
	podInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			e.podAddedDeletedUpdated(logger, nil, obj, false)
		},
		DeleteFunc: func(obj interface{}) {
			e.podAddedDeletedUpdated(logger, nil, obj, true)
		},
		UpdateFunc: func(old, new interface{}) {
			e.podAddedDeletedUpdated(logger, old, new, false)
		},
	})

	return e, nil
}

// Run runs the controller goroutines.
func (c *Controller) Run(ctx context.Context, workers int) {
	defer utilruntime.HandleCrash()
	defer c.queue.ShutDown()
	defer c.pvcProcessingStore.namespaceQueue.ShutDown()

	logger := klog.FromContext(ctx)
	logger.Info("Starting PVC protection controller")
	defer logger.Info("Shutting down PVC protection controller")

	if !cache.WaitForNamedCacheSync("PVC protection", ctx.Done(), c.pvcListerSynced, c.podListerSynced) {
		return
	}

	go wait.UntilWithContext(ctx, c.runMainWorker, time.Second)
	for i := 0; i < workers; i++ {
		go wait.UntilWithContext(ctx, c.runProcessNamespaceWorker, time.Second)
	}

	<-ctx.Done()
}

// Main worker batch-pulls PVC items off informer's work queue and populates namespace queue and namespace-PVCs map
func (c *Controller) runMainWorker(ctx context.Context) {
	for c.processNextWorkItem() {
	}
}

// Consumer worker pulls items off namespace queue and processes associated PVCs
func (c *Controller) runProcessNamespaceWorker(ctx context.Context) {
	for c.processPVCsByNamespace(ctx) {
	}
}

func (c *Controller) processNextWorkItem() bool {
	pvcKey, quit := c.queue.Get()
	if quit {
		return false
	}

	pvcNamespace, pvcName, err := cache.SplitMetaNamespaceKey(pvcKey)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("error parsing PVC key %q: %w", pvcKey, err))
		return true
	}

	c.pvcProcessingStore.addOrUpdate(pvcNamespace, pvcKey, pvcName)
	return true
}

func (c *Controller) processPVCsByNamespace(ctx context.Context) bool {
	pvcList, namespace := c.pvcProcessingStore.flushNextPVCsByNamespace()
	if pvcList == nil {
		return false
	}

	lazyLivePodList := &LazyLivePodList{controller: c}
	for _, item := range pvcList {
		pvcKey, pvcName := item.pvcKey, item.pvcName
		err := c.processPVC(ctx, namespace, pvcName, lazyLivePodList)
		if err == nil {
			c.queue.Forget(pvcKey)
		} else {
			c.queue.AddRateLimited(pvcKey)
			utilruntime.HandleError(fmt.Errorf("PVC %v/%v failed with: %w", pvcName, namespace, err))
		}
		c.queue.Done(pvcKey)
	}
	return true
}

func (c *Controller) processPVC(ctx context.Context, pvcNamespace, pvcName string, lazyLivePodList *LazyLivePodList) error {
	logger := klog.FromContext(ctx)
	logger.V(4).Info("Processing PVC", "PVC", klog.KRef(pvcNamespace, pvcName))
	startTime := time.Now()
	defer func() {
		logger.V(4).Info("Finished processing PVC", "PVC", klog.KRef(pvcNamespace, pvcName), "duration", time.Since(startTime))
	}()

	pvc, err := c.pvcLister.PersistentVolumeClaims(pvcNamespace).Get(pvcName)
	if apierrors.IsNotFound(err) {
		logger.V(4).Info("PVC not found, ignoring", "PVC", klog.KRef(pvcNamespace, pvcName))
		return nil
	}
	if err != nil {
		return err
	}

	if protectionutil.IsDeletionCandidate(pvc, volumeutil.PVCProtectionFinalizer) {
		// PVC should be deleted. Check if it's used and remove finalizer if
		// it's not.
		isUsed, err := c.isBeingUsed(ctx, pvc, lazyLivePodList)
		if err != nil {
			return err
		}
		if !isUsed {
			return c.removeFinalizer(ctx, pvc)
		}
		logger.V(2).Info("Keeping PVC because it is being used", "PVC", klog.KObj(pvc))
	}

	if protectionutil.NeedToAddFinalizer(pvc, volumeutil.PVCProtectionFinalizer) {
		// PVC is not being deleted -> it should have the finalizer. The
		// finalizer should be added by admission plugin, this is just to add
		// the finalizer to old PVCs that were created before the admission
		// plugin was enabled.
		return c.addFinalizer(ctx, pvc)
	}
	return nil
}

func (c *Controller) addFinalizer(ctx context.Context, pvc *v1.PersistentVolumeClaim) error {
	claimClone := pvc.DeepCopy()
	claimClone.ObjectMeta.Finalizers = append(claimClone.ObjectMeta.Finalizers, volumeutil.PVCProtectionFinalizer)
	_, err := c.client.CoreV1().PersistentVolumeClaims(claimClone.Namespace).Update(ctx, claimClone, metav1.UpdateOptions{})
	logger := klog.FromContext(ctx)
	if err != nil {
		logger.Error(err, "Error adding protection finalizer to PVC", "PVC", klog.KObj(pvc))
		return err
	}
	logger.V(3).Info("Added protection finalizer to PVC", "PVC", klog.KObj(pvc))
	return nil
}

func (c *Controller) removeFinalizer(ctx context.Context, pvc *v1.PersistentVolumeClaim) error {
	claimClone := pvc.DeepCopy()
	claimClone.ObjectMeta.Finalizers = slice.RemoveString(claimClone.ObjectMeta.Finalizers, volumeutil.PVCProtectionFinalizer, nil)
	_, err := c.client.CoreV1().PersistentVolumeClaims(claimClone.Namespace).Update(ctx, claimClone, metav1.UpdateOptions{})
	logger := klog.FromContext(ctx)
	if err != nil {
		logger.Error(err, "Error removing protection finalizer from PVC", "PVC", klog.KObj(pvc))
		return err
	}
	logger.V(3).Info("Removed protection finalizer from PVC", "PVC", klog.KObj(pvc))
	return nil
}

func (c *Controller) isBeingUsed(ctx context.Context, pvc *v1.PersistentVolumeClaim, lazyLivePodList *LazyLivePodList) (bool, error) {
	// Look for a Pod using pvc in the Informer's cache. If one is found the
	// correct decision to keep pvc is taken without doing an expensive live
	// list.
	logger := klog.FromContext(ctx)
	if inUse, err := c.askInformer(logger, pvc); err != nil {
		// No need to return because a live list will follow.
		logger.Error(err, "")
	} else if inUse {
		return true, nil
	}

	// Even if no Pod using pvc was found in the Informer's cache it doesn't
	// mean such a Pod doesn't exist: it might just not be in the cache yet. To
	// be 100% confident that it is safe to delete pvc make sure no Pod is using
	// it among those returned by a "lazy" live list.

	// Use a "lazy" live pod list: lazyLivePodList caches the first successful live pod list response,
	// so for a large number of PVC deletions in a short duration, subsequent requests can use the cached pod list
	// instead of issuing a lot of API requests. The cache is refreshed for each run of processNextWorkItem().
	return c.askAPIServer(ctx, pvc, lazyLivePodList)
}

func (c *Controller) askInformer(logger klog.Logger, pvc *v1.PersistentVolumeClaim) (bool, error) {
	logger.V(4).Info("Looking for Pods using PVC in the Informer's cache", "PVC", klog.KObj(pvc))

	// The indexer is used to find pods which might use the PVC.
	objs, err := c.podIndexer.ByIndex(common.PodPVCIndex, fmt.Sprintf("%s/%s", pvc.Namespace, pvc.Name))
	if err != nil {
		return false, fmt.Errorf("cache-based list of pods failed while processing %s/%s: %s", pvc.Namespace, pvc.Name, err.Error())
	}
	for _, obj := range objs {
		pod, ok := obj.(*v1.Pod)
		if !ok {
			continue
		}

		// We still need to look at each volume: that's redundant for volume.PersistentVolumeClaim,
		// but for volume.Ephemeral we need to be sure that this particular PVC is the one
		// created for the ephemeral volume.
		if c.podUsesPVC(logger, pod, pvc) {
			return true, nil
		}
	}

	logger.V(4).Info("No Pod using PVC was found in the Informer's cache", "PVC", klog.KObj(pvc))
	return false, nil
}

func (c *Controller) askAPIServer(ctx context.Context, pvc *v1.PersistentVolumeClaim, lazyLivePodList *LazyLivePodList) (bool, error) {
	logger := klog.FromContext(ctx)
	logger.V(4).Info("Looking for Pods using PVC", "PVC", klog.KObj(pvc))
	if lazyLivePodList.getCache() == nil {
		logger.V(4).Info("Live listing Pods in namespace", "namespace", pvc.Namespace)
		podsList, err := c.client.CoreV1().Pods(pvc.Namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			return false, fmt.Errorf("live list of pods failed: %s", err.Error())
		}

		if podsList.Items == nil {
			lazyLivePodList.setCache(make([]v1.Pod, 0))
		} else {
			lazyLivePodList.setCache(podsList.Items)
		}
	}

	for _, pod := range lazyLivePodList.getCache() {
		if c.podUsesPVC(logger, &pod, pvc) {
			return true, nil
		}
	}

	logger.V(2).Info("PVC is unused", "PVC", klog.KObj(pvc))
	return false, nil
}

func (c *Controller) podUsesPVC(logger klog.Logger, pod *v1.Pod, pvc *v1.PersistentVolumeClaim) bool {
	// Check whether pvc is used by pod only if pod is scheduled, because
	// kubelet sees pods after they have been scheduled and it won't allow
	// starting a pod referencing a PVC with a non-nil deletionTimestamp.
	if pod.Spec.NodeName != "" {
		for _, volume := range pod.Spec.Volumes {
			if volume.PersistentVolumeClaim != nil && volume.PersistentVolumeClaim.ClaimName == pvc.Name && !podIsFinished(pod) ||
				!podIsShutDown(pod) && volume.Ephemeral != nil && ephemeral.VolumeClaimName(pod, &volume) == pvc.Name && ephemeral.VolumeIsForPod(pod, pvc) == nil {
				logger.V(2).Info("Pod uses PVC", "pod", klog.KObj(pod), "PVC", klog.KObj(pvc))
				return true
			}
		}
	}
	return false
}

func podIsFinished(pod *v1.Pod) bool {
	return pod.Status.Phase == v1.PodFailed || pod.Status.Phase == v1.PodSucceeded
}

// podIsShutDown returns true if kubelet is done with the pod or
// it was force-deleted.
func podIsShutDown(pod *v1.Pod) bool {
	// A pod that has a deletionTimestamp and a zero
	// deletionGracePeriodSeconds
	// a) has been processed by kubelet and was set up for deletion
	//    by the apiserver:
	//    - canBeDeleted has verified that volumes were unpublished
	//      https://github.com/kubernetes/kubernetes/blob/5404b5a28a2114299608bab00e4292960dd864a0/pkg/kubelet/kubelet_pods.go#L980
	//    - deletionGracePeriodSeconds was set via a delete
	//      with zero GracePeriodSeconds
	//      https://github.com/kubernetes/kubernetes/blob/5404b5a28a2114299608bab00e4292960dd864a0/pkg/kubelet/status/status_manager.go#L580-L592
	// or
	// b) was force-deleted.
	//
	// It's now just waiting for garbage collection. We could wait
	// for it to actually get removed, but that may be blocked by
	// finalizers for the pod and thus get delayed.
	//
	// Worse, it is possible that there is a cyclic dependency
	// (pod finalizer waits for PVC to get removed, PVC protection
	// controller waits for pod to get removed).  By considering
	// the PVC unused in this case, we allow the PVC to get
	// removed and break such a cycle.
	//
	// Therefore it is better to proceed with PVC removal,
	// which is safe (case a) and/or desirable (case b).
	return pod.DeletionTimestamp != nil && pod.DeletionGracePeriodSeconds != nil && *pod.DeletionGracePeriodSeconds == 0
}

// pvcAddedUpdated reacts to pvc added/updated events
func (c *Controller) pvcAddedUpdated(logger klog.Logger, obj interface{}) {
	pvc, ok := obj.(*v1.PersistentVolumeClaim)
	if !ok {
		utilruntime.HandleError(fmt.Errorf("PVC informer returned non-PVC object: %#v", obj))
		return
	}
	key, err := cache.MetaNamespaceKeyFunc(pvc)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for Persistent Volume Claim %#v: %v", pvc, err))
		return
	}
	logger.V(4).Info("Got event on PVC", "pvc", klog.KObj(pvc))

	if protectionutil.NeedToAddFinalizer(pvc, volumeutil.PVCProtectionFinalizer) || protectionutil.IsDeletionCandidate(pvc, volumeutil.PVCProtectionFinalizer) {
		c.queue.Add(key)
	}
}

// podAddedDeletedUpdated reacts to Pod events
func (c *Controller) podAddedDeletedUpdated(logger klog.Logger, old, new interface{}, deleted bool) {
	if pod := c.parsePod(new); pod != nil {
		c.enqueuePVCs(logger, pod, deleted)

		// An update notification might mask the deletion of a pod X and the
		// following creation of a pod Y with the same namespaced name as X. If
		// that's the case X needs to be processed as well to handle the case
		// where it is blocking deletion of a PVC not referenced by Y, otherwise
		// such PVC will never be deleted.
		if oldPod := c.parsePod(old); oldPod != nil && oldPod.UID != pod.UID {
			c.enqueuePVCs(logger, oldPod, true)
		}
	}
}

func (*Controller) parsePod(obj interface{}) *v1.Pod {
	if obj == nil {
		return nil
	}
	pod, ok := obj.(*v1.Pod)
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("couldn't get object from tombstone %#v", obj))
			return nil
		}
		pod, ok = tombstone.Obj.(*v1.Pod)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("tombstone contained object that is not a Pod %#v", obj))
			return nil
		}
	}
	return pod
}

func (c *Controller) enqueuePVCs(logger klog.Logger, pod *v1.Pod, deleted bool) {
	// Filter out pods that can't help us to remove a finalizer on PVC
	if !deleted && !volumeutil.IsPodTerminated(pod, pod.Status) && pod.Spec.NodeName != "" {
		return
	}

	logger.V(4).Info("Enqueuing PVCs for Pod", "pod", klog.KObj(pod), "podUID", pod.UID)

	// Enqueue all PVCs that the pod uses
	for _, volume := range pod.Spec.Volumes {
		switch {
		case volume.PersistentVolumeClaim != nil:
			c.queue.Add(pod.Namespace + "/" + volume.PersistentVolumeClaim.ClaimName)
		case volume.Ephemeral != nil:
			c.queue.Add(pod.Namespace + "/" + ephemeral.VolumeClaimName(pod, &volume))
		}
	}
}
