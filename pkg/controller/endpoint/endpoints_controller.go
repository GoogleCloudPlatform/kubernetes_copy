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

package endpoint

import (
	"context"
	"fmt"
	"math"
	"time"

	v1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/conversion"
	"k8s.io/apimachinery/pkg/labels"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	coreinformers "k8s.io/client-go/informers/core/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	v1core "k8s.io/client-go/kubernetes/typed/core/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	endpointsliceutil "k8s.io/endpointslice/util"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/api/v1/endpoints"
	podutil "k8s.io/kubernetes/pkg/api/v1/pod"
	api "k8s.io/kubernetes/pkg/apis/core"
	helper "k8s.io/kubernetes/pkg/apis/core/v1/helper"
	"k8s.io/kubernetes/pkg/controller"
	utillabels "k8s.io/kubernetes/pkg/util/labels"
	utilnet "k8s.io/utils/net"
)

const (
	// maxRetries is the number of times a service will be retried before it is dropped out of the queue.
	// With the current rate-limiter in use (5ms*2^(maxRetries-1)) the following numbers represent the
	// sequence of delays between successive queuings of a service.
	//
	// 5ms, 10ms, 20ms, 40ms, 80ms, 160ms, 320ms, 640ms, 1.3s, 2.6s, 5.1s, 10.2s, 20.4s, 41s, 82s
	maxRetries = 15

	// maxCapacity represents the maximum number of addresses that should be
	// stored in an Endpoints resource. In a future release, this controller
	// may truncate endpoints exceeding this length.
	maxCapacity = 1000

	// truncated is a possible value for `endpoints.kubernetes.io/over-capacity` annotation on an
	// endpoint resource and indicates that the number of endpoints have been truncated to
	// maxCapacity
	truncated = "truncated"
)

// NewEndpointController returns a new *Controller.
func NewEndpointController(ctx context.Context, podInformer coreinformers.PodInformer, serviceInformer coreinformers.ServiceInformer,
	endpointsInformer coreinformers.EndpointsInformer, client clientset.Interface, endpointUpdatesBatchPeriod time.Duration) *Controller {
	broadcaster := record.NewBroadcaster(record.WithContext(ctx))
	recorder := broadcaster.NewRecorder(scheme.Scheme, v1.EventSource{Component: "endpoint-controller"})

	e := &Controller{
		client: client,
		queue: workqueue.NewTypedRateLimitingQueueWithConfig(
			workqueue.DefaultTypedControllerRateLimiter[string](),
			workqueue.TypedRateLimitingQueueConfig[string]{
				Name: "endpoint",
			},
		),
		workerLoopPeriod: time.Second,
	}

	serviceInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: e.onServiceUpdate,
		UpdateFunc: func(old, cur interface{}) {
			e.onServiceUpdate(cur)
		},
		DeleteFunc: e.onServiceDelete,
	})
	e.serviceLister = serviceInformer.Lister()
	e.servicesSynced = serviceInformer.Informer().HasSynced

	podInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    e.addPod,
		UpdateFunc: e.updatePod,
		DeleteFunc: e.deletePod,
	})
	e.podLister = podInformer.Lister()
	e.podsSynced = podInformer.Informer().HasSynced

	endpointsInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		DeleteFunc: e.onEndpointsDelete,
	})
	e.endpointsLister = endpointsInformer.Lister()
	e.endpointsSynced = endpointsInformer.Informer().HasSynced

	e.staleEndpointsTracker = newStaleEndpointsTracker()
	e.triggerTimeTracker = endpointsliceutil.NewTriggerTimeTracker()
	e.eventBroadcaster = broadcaster
	e.eventRecorder = recorder

	e.endpointUpdatesBatchPeriod = endpointUpdatesBatchPeriod

	return e
}

// Controller manages selector-based service endpoints.
type Controller struct {
	client           clientset.Interface
	eventBroadcaster record.EventBroadcaster
	eventRecorder    record.EventRecorder

	// serviceLister is able to list/get services and is populated by the shared informer passed to
	// NewEndpointController.
	serviceLister corelisters.ServiceLister
	// servicesSynced returns true if the service shared informer has been synced at least once.
	// Added as a member to the struct to allow injection for testing.
	servicesSynced cache.InformerSynced

	// podLister is able to list/get pods and is populated by the shared informer passed to
	// NewEndpointController.
	podLister corelisters.PodLister
	// podsSynced returns true if the pod shared informer has been synced at least once.
	// Added as a member to the struct to allow injection for testing.
	podsSynced cache.InformerSynced

	// endpointsLister is able to list/get endpoints and is populated by the shared informer passed to
	// NewEndpointController.
	endpointsLister corelisters.EndpointsLister
	// endpointsSynced returns true if the endpoints shared informer has been synced at least once.
	// Added as a member to the struct to allow injection for testing.
	endpointsSynced cache.InformerSynced
	// staleEndpointsTracker can help determine if a cached Endpoints is out of date.
	staleEndpointsTracker *staleEndpointsTracker

	// Services that need to be updated. A channel is inappropriate here,
	// because it allows services with lots of pods to be serviced much
	// more often than services with few pods; it also would cause a
	// service that's inserted multiple times to be processed more than
	// necessary.
	queue workqueue.TypedRateLimitingInterface[string]

	// workerLoopPeriod is the time between worker runs. The workers process the queue of service and pod changes.
	workerLoopPeriod time.Duration

	// triggerTimeTracker is an util used to compute and export the EndpointsLastChangeTriggerTime
	// annotation.
	triggerTimeTracker *endpointsliceutil.TriggerTimeTracker

	endpointUpdatesBatchPeriod time.Duration
}

// Run will not return until stopCh is closed. workers determines how many
// endpoints will be handled in parallel.
func (e *Controller) Run(ctx context.Context, workers int) {
	defer utilruntime.HandleCrash()

	// Start events processing pipeline.
	e.eventBroadcaster.StartStructuredLogging(3)
	e.eventBroadcaster.StartRecordingToSink(&v1core.EventSinkImpl{Interface: e.client.CoreV1().Events("")})
	defer e.eventBroadcaster.Shutdown()

	defer e.queue.ShutDown()

	logger := klog.FromContext(ctx)
	logger.Info("Starting endpoint controller")
	defer logger.Info("Shutting down endpoint controller")

	if !cache.WaitForNamedCacheSync("endpoint", ctx.Done(), e.podsSynced, e.servicesSynced, e.endpointsSynced) {
		return
	}

	for i := 0; i < workers; i++ {
		go wait.UntilWithContext(ctx, e.worker, e.workerLoopPeriod)
	}

	go func() {
		defer utilruntime.HandleCrash()
		e.checkLeftoverEndpoints()
	}()

	<-ctx.Done()
}

// When a pod is added, figure out what services it will be a member of and
// enqueue them. obj must have *v1.Pod type.
func (e *Controller) addPod(obj interface{}) {
	pod := obj.(*v1.Pod)
	services, err := endpointsliceutil.GetPodServiceMemberships(e.serviceLister, pod)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("Unable to get pod %s/%s's service memberships: %v", pod.Namespace, pod.Name, err))
		return
	}
	for key := range services {
		e.queue.AddAfter(key, e.endpointUpdatesBatchPeriod)
	}
}

func podToEndpointAddressForService(svc *v1.Service, pod *v1.Pod) (*v1.EndpointAddress, error) {
	var endpointIP string
	ipFamily := v1.IPv4Protocol

	if len(svc.Spec.IPFamilies) > 0 {
		// controller is connected to an api-server that correctly sets IPFamilies
		ipFamily = svc.Spec.IPFamilies[0] // this works for headful and headless
	} else {
		// controller is connected to an api server that does not correctly
		// set IPFamilies (e.g. old api-server during an upgrade)
		// TODO (khenidak): remove by when the possibility of upgrading
		// from a cluster that does not support dual stack is nil
		if len(svc.Spec.ClusterIP) > 0 && svc.Spec.ClusterIP != v1.ClusterIPNone {
			// headful service. detect via service clusterIP
			if utilnet.IsIPv6String(svc.Spec.ClusterIP) {
				ipFamily = v1.IPv6Protocol
			}
		} else {
			// Since this is a headless service we use podIP to identify the family.
			// This assumes that status.PodIP is assigned correctly (follows pod cidr and
			// pod cidr list order is same as service cidr list order). The expectation is
			// this is *most probably* the case.

			// if the family was incorrectly identified then this will be corrected once the
			// upgrade is completed (controller connects to api-server that correctly defaults services)
			if utilnet.IsIPv6String(pod.Status.PodIP) {
				ipFamily = v1.IPv6Protocol
			}
		}
	}

	// find an ip that matches the family
	for _, podIP := range pod.Status.PodIPs {
		if (ipFamily == v1.IPv6Protocol) == utilnet.IsIPv6String(podIP.IP) {
			endpointIP = podIP.IP
			break
		}
	}

	if endpointIP == "" {
		return nil, fmt.Errorf("failed to find a matching endpoint for service %v", svc.Name)
	}

	return &v1.EndpointAddress{
		IP:       endpointIP,
		NodeName: &pod.Spec.NodeName,
		TargetRef: &v1.ObjectReference{
			Kind:      "Pod",
			Namespace: pod.ObjectMeta.Namespace,
			Name:      pod.ObjectMeta.Name,
			UID:       pod.ObjectMeta.UID,
		},
	}, nil
}

// When a pod is updated, figure out what services it used to be a member of
// and what services it will be a member of, and enqueue the union of these.
// old and cur must be *v1.Pod types.
func (e *Controller) updatePod(old, cur interface{}) {
	services := endpointsliceutil.GetServicesToUpdateOnPodChange(e.serviceLister, old, cur)
	for key := range services {
		e.queue.AddAfter(key, e.endpointUpdatesBatchPeriod)
	}
}

// When a pod is deleted, enqueue the services the pod used to be a member of.
// obj could be an *v1.Pod, or a DeletionFinalStateUnknown marker item.
func (e *Controller) deletePod(obj interface{}) {
	pod := endpointsliceutil.GetPodFromDeleteAction(obj)
	if pod != nil {
		e.addPod(pod)
	}
}

// onServiceUpdate updates the Service Selector in the cache and queues the Service for processing.
func (e *Controller) onServiceUpdate(obj interface{}) {
	key, err := controller.KeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("Couldn't get key for object %+v: %v", obj, err))
		return
	}
	e.queue.Add(key)
}

// onServiceDelete removes the Service Selector from the cache and queues the Service for processing.
func (e *Controller) onServiceDelete(obj interface{}) {
	key, err := controller.KeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("Couldn't get key for object %+v: %v", obj, err))
		return
	}
	e.queue.Add(key)
}

func (e *Controller) onEndpointsDelete(obj interface{}) {
	key, err := controller.KeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("Couldn't get key for object %+v: %v", obj, err))
		return
	}
	e.queue.Add(key)
}

// worker runs a worker thread that just dequeues items, processes them, and
// marks them done. You may run as many of these in parallel as you wish; the
// workqueue guarantees that they will not end up processing the same service
// at the same time.
func (e *Controller) worker(ctx context.Context) {
	for e.processNextWorkItem(ctx) {
	}
}

func (e *Controller) processNextWorkItem(ctx context.Context) bool {
	eKey, quit := e.queue.Get()
	if quit {
		return false
	}
	defer e.queue.Done(eKey)

	logger := klog.FromContext(ctx)
	err := e.syncService(ctx, eKey)
	e.handleErr(logger, err, eKey)

	return true
}

func (e *Controller) handleErr(logger klog.Logger, err error, key string) {
	if err == nil {
		e.queue.Forget(key)
		return
	}

	ns, name, keyErr := cache.SplitMetaNamespaceKey(key)
	if keyErr != nil {
		logger.Error(err, "Failed to split meta namespace cache key", "key", key)
	}

	if e.queue.NumRequeues(key) < maxRetries {
		logger.V(2).Info("Error syncing endpoints, retrying", "service", klog.KRef(ns, name), "err", err)
		e.queue.AddRateLimited(key)
		return
	}

	logger.Info("Dropping service out of the queue", "service", klog.KRef(ns, name), "err", err)
	e.queue.Forget(key)
	utilruntime.HandleError(err)
}

func (e *Controller) syncService(ctx context.Context, key string) error {
	startTime := time.Now()
	logger := klog.FromContext(ctx)
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}
	defer func() {
		logger.V(4).Info("Finished syncing service endpoints", "service", klog.KRef(namespace, name), "startTime", time.Since(startTime))
	}()

	service, err := e.serviceLister.Services(namespace).Get(name)
	if err != nil {
		if !errors.IsNotFound(err) {
			return err
		}

		// Delete the corresponding endpoint, as the service has been deleted.
		// TODO: Please note that this will delete an endpoint when a
		// service is deleted. However, if we're down at the time when
		// the service is deleted, we will miss that deletion, so this
		// doesn't completely solve the problem. See #6877.
		err = e.client.CoreV1().Endpoints(namespace).Delete(ctx, name, metav1.DeleteOptions{})
		if err != nil && !errors.IsNotFound(err) {
			return err
		}
		e.triggerTimeTracker.DeleteService(namespace, name)
		e.staleEndpointsTracker.Delete(namespace, name)
		return nil
	}

	if service.Spec.Type == v1.ServiceTypeExternalName {
		// services with Type ExternalName receive no endpoints from this controller;
		// Ref: https://issues.k8s.io/105986
		return nil
	}

	if service.Spec.Selector == nil {
		// services without a selector receive no endpoints from this controller;
		// these services will receive the endpoints that are created out-of-band via the REST API.
		return nil
	}

	logger.V(5).Info("About to update endpoints for service", "service", klog.KRef(namespace, name))
	pods, err := e.podLister.Pods(service.Namespace).List(labels.Set(service.Spec.Selector).AsSelectorPreValidated())
	if err != nil {
		// Since we're getting stuff from a local cache, it is
		// basically impossible to get this error.
		return err
	}

	// We call ComputeEndpointLastChangeTriggerTime here to make sure that the
	// state of the trigger time tracker gets updated even if the sync turns out
	// to be no-op and we don't update the endpoints object.
	endpointsLastChangeTriggerTime := e.triggerTimeTracker.
		ComputeEndpointLastChangeTriggerTime(namespace, service, pods)

	subsets := []v1.EndpointSubset{}
	var totalReadyEps int
	var totalNotReadyEps int

	for _, pod := range pods {
		if !endpointsliceutil.ShouldPodBeInEndpoints(pod, service.Spec.PublishNotReadyAddresses) {
			logger.V(5).Info("Pod is not included on endpoints for Service", "pod", klog.KObj(pod), "service", klog.KObj(service))
			continue
		}

		ep, err := podToEndpointAddressForService(service, pod)
		if err != nil {
			// this will happen, if the cluster runs with some nodes configured as dual stack and some as not
			// such as the case of an upgrade..
			logger.V(2).Info("Failed to find endpoint for service with ClusterIP on pod with error", "service", klog.KObj(service), "clusterIP", service.Spec.ClusterIP, "pod", klog.KObj(pod), "error", err)
			continue
		}

		epa := *ep
		if endpointsliceutil.ShouldSetHostname(pod, service) {
			epa.Hostname = pod.Spec.Hostname
		}

		// Allow headless service not to have ports.
		if len(service.Spec.Ports) == 0 {
			if service.Spec.ClusterIP == api.ClusterIPNone {
				subsets, totalReadyEps, totalNotReadyEps = addEndpointSubset(logger, subsets, pod, epa, nil, service.Spec.PublishNotReadyAddresses)
				// No need to repack subsets for headless service without ports.
			}
		} else {
			for i := range service.Spec.Ports {
				servicePort := &service.Spec.Ports[i]
				portNum, err := podutil.FindPort(pod, servicePort)
				if err != nil {
					logger.V(4).Info("Failed to find port for service", "service", klog.KObj(service), "error", err)
					continue
				}
				epp := endpointPortFromServicePort(servicePort, portNum)

				var readyEps, notReadyEps int
				subsets, readyEps, notReadyEps = addEndpointSubset(logger, subsets, pod, epa, epp, service.Spec.PublishNotReadyAddresses)
				totalReadyEps = totalReadyEps + readyEps
				totalNotReadyEps = totalNotReadyEps + notReadyEps
			}
		}
	}
	subsets = endpoints.RepackSubsets(subsets)

	// See if there's actually an update here.
	currentEndpoints, err := e.endpointsLister.Endpoints(service.Namespace).Get(service.Name)
	if err != nil {
		if !errors.IsNotFound(err) {
			return err
		}
		currentEndpoints = &v1.Endpoints{
			ObjectMeta: metav1.ObjectMeta{
				Name:   service.Name,
				Labels: service.Labels,
			},
		}
	} else if e.staleEndpointsTracker.IsStale(currentEndpoints) {
		return fmt.Errorf("endpoints informer cache is out of date, resource version %s already processed for endpoints %s", currentEndpoints.ResourceVersion, key)
	}

	createEndpoints := len(currentEndpoints.ResourceVersion) == 0

	// Compare the sorted subsets and labels
	// Remove the HeadlessService label from the endpoints if it exists,
	// as this won't be set on the service itself
	// and will cause a false negative in this diff check.
	// But first check if it has that label to avoid expensive copies.
	compareLabels := currentEndpoints.Labels
	if _, ok := currentEndpoints.Labels[v1.IsHeadlessService]; ok {
		compareLabels = utillabels.CloneAndRemoveLabel(currentEndpoints.Labels, v1.IsHeadlessService)
	}
	// When comparing the subsets, we ignore the difference in ResourceVersion of Pod to avoid unnecessary Endpoints
	// updates caused by Pod updates that we don't care, e.g. annotation update.
	if !createEndpoints &&
		endpointSubsetsEqualIgnoreResourceVersion(currentEndpoints.Subsets, subsets) &&
		apiequality.Semantic.DeepEqual(compareLabels, service.Labels) &&
		capacityAnnotationSetCorrectly(currentEndpoints.Annotations, currentEndpoints.Subsets) {
		logger.V(5).Info("endpoints are equal, skipping update", "service", klog.KObj(service))
		return nil
	}
	newEndpoints := currentEndpoints.DeepCopy()
	newEndpoints.Subsets = subsets
	newEndpoints.Labels = service.Labels
	if newEndpoints.Annotations == nil {
		newEndpoints.Annotations = make(map[string]string)
	}

	if !endpointsLastChangeTriggerTime.IsZero() {
		newEndpoints.Annotations[v1.EndpointsLastChangeTriggerTime] =
			endpointsLastChangeTriggerTime.UTC().Format(time.RFC3339Nano)
	} else { // No new trigger time, clear the annotation.
		delete(newEndpoints.Annotations, v1.EndpointsLastChangeTriggerTime)
	}

	if truncateEndpoints(newEndpoints) {
		newEndpoints.Annotations[v1.EndpointsOverCapacity] = truncated
	} else {
		delete(newEndpoints.Annotations, v1.EndpointsOverCapacity)
	}

	if newEndpoints.Labels == nil {
		newEndpoints.Labels = make(map[string]string)
	}

	if !helper.IsServiceIPSet(service) {
		newEndpoints.Labels = utillabels.CloneAndAddLabel(newEndpoints.Labels, v1.IsHeadlessService, "")
	} else {
		newEndpoints.Labels = utillabels.CloneAndRemoveLabel(newEndpoints.Labels, v1.IsHeadlessService)
	}

	logger.V(4).Info("Update endpoints", "service", klog.KObj(service), "readyEndpoints", totalReadyEps, "notreadyEndpoints", totalNotReadyEps)
	var updatedEndpoints *v1.Endpoints
	if createEndpoints {
		// No previous endpoints, create them
		_, err = e.client.CoreV1().Endpoints(service.Namespace).Create(ctx, newEndpoints, metav1.CreateOptions{})
	} else {
		// Pre-existing
		updatedEndpoints, err = e.client.CoreV1().Endpoints(service.Namespace).Update(ctx, newEndpoints, metav1.UpdateOptions{})
	}
	if err != nil {
		if createEndpoints && errors.IsForbidden(err) {
			// A request is forbidden primarily for two reasons:
			// 1. namespace is terminating, endpoint creation is not allowed by default.
			// 2. policy is misconfigured, in which case no service would function anywhere.
			// Given the frequency of 1, we log at a lower level.
			logger.V(5).Info("Forbidden from creating endpoints", "error", err)

			// If the namespace is terminating, creates will continue to fail. Simply drop the item.
			if errors.HasStatusCause(err, v1.NamespaceTerminatingCause) {
				return nil
			}
		}

		if createEndpoints {
			e.eventRecorder.Eventf(newEndpoints, v1.EventTypeWarning, "FailedToCreateEndpoint", "Failed to create endpoint for service %v/%v: %v", service.Namespace, service.Name, err)
		} else {
			e.eventRecorder.Eventf(newEndpoints, v1.EventTypeWarning, "FailedToUpdateEndpoint", "Failed to update endpoint %v/%v: %v", service.Namespace, service.Name, err)
		}

		return err
	}
	// If the current endpoints is updated we track the old resource version, so
	// if we obtain this resource version again from the lister we know is outdated
	// and we need to retry later to wait for the informer cache to be up-to-date.
	// there are some operations (webhooks, truncated endpoints, ...) that can potentially cause endpoints updates became noop
	// and return the same resourceVersion.
	// Ref: https://issues.k8s.io/127370 , https://issues.k8s.io/126578
	if updatedEndpoints != nil && updatedEndpoints.ResourceVersion != currentEndpoints.ResourceVersion {
		e.staleEndpointsTracker.Stale(currentEndpoints)
	}
	return nil
}

// checkLeftoverEndpoints lists all currently existing endpoints and adds their
// service to the queue. This will detect endpoints that exist with no
// corresponding service; these endpoints need to be deleted. We only need to
// do this once on startup, because in steady-state these are detected (but
// some stragglers could have been left behind if the endpoint controller
// reboots).
func (e *Controller) checkLeftoverEndpoints() {
	list, err := e.endpointsLister.List(labels.Everything())
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("Unable to list endpoints (%v); orphaned endpoints will not be cleaned up. (They're pretty harmless, but you can restart this component if you want another attempt made.)", err))
		return
	}
	for _, ep := range list {
		if _, ok := ep.Annotations[resourcelock.LeaderElectionRecordAnnotationKey]; ok {
			// when there are multiple controller-manager instances,
			// we observe that it will delete leader-election endpoints after 5min
			// and cause re-election
			// so skip the delete here
			// as leader-election only have endpoints without service
			continue
		}
		key, err := controller.KeyFunc(ep)
		if err != nil {
			utilruntime.HandleError(fmt.Errorf("Unable to get key for endpoint %#v", ep))
			continue
		}
		e.queue.Add(key)
	}
}

// addEndpointSubset add the endpoints addresses and ports to the EndpointSubset.
// The addresses are added to the corresponding field, ready or not ready, depending
// on the pod status and the Service PublishNotReadyAddresses field value.
// The pod passed to this function must have already been filtered through ShouldPodBeInEndpoints.
func addEndpointSubset(logger klog.Logger, subsets []v1.EndpointSubset, pod *v1.Pod, epa v1.EndpointAddress,
	epp *v1.EndpointPort, tolerateUnreadyEndpoints bool) ([]v1.EndpointSubset, int, int) {
	var readyEps int
	var notReadyEps int
	ports := []v1.EndpointPort{}
	if epp != nil {
		ports = append(ports, *epp)
	}
	if tolerateUnreadyEndpoints || podutil.IsPodReady(pod) {
		subsets = append(subsets, v1.EndpointSubset{
			Addresses: []v1.EndpointAddress{epa},
			Ports:     ports,
		})
		readyEps++
	} else { // if it is not a ready address it has to be not ready
		logger.V(5).Info("Pod is out of service", "pod", klog.KObj(pod))
		subsets = append(subsets, v1.EndpointSubset{
			NotReadyAddresses: []v1.EndpointAddress{epa},
			Ports:             ports,
		})
		notReadyEps++
	}
	return subsets, readyEps, notReadyEps
}

func endpointPortFromServicePort(servicePort *v1.ServicePort, portNum int) *v1.EndpointPort {
	return &v1.EndpointPort{
		Name:        servicePort.Name,
		Port:        int32(portNum),
		Protocol:    servicePort.Protocol,
		AppProtocol: servicePort.AppProtocol,
	}
}

// capacityAnnotationSetCorrectly returns false if number of endpoints is greater than maxCapacity or
// returns true if underCapacity and the annotation is not set.
func capacityAnnotationSetCorrectly(annotations map[string]string, subsets []v1.EndpointSubset) bool {
	numEndpoints := 0
	for _, subset := range subsets {
		numEndpoints += len(subset.Addresses) + len(subset.NotReadyAddresses)
	}
	if numEndpoints > maxCapacity {
		// If subsets are over capacity, they must be truncated so consider
		// the annotation as not set correctly
		return false
	}
	_, ok := annotations[v1.EndpointsOverCapacity]
	return !ok
}

// truncateEndpoints by best effort will distribute the endpoints over the subsets based on the proportion
// of endpoints per subset and will prioritize Ready Endpoints over NotReady Endpoints.
func truncateEndpoints(endpoints *v1.Endpoints) bool {
	totalReady := 0
	totalNotReady := 0
	for _, subset := range endpoints.Subsets {
		totalReady += len(subset.Addresses)
		totalNotReady += len(subset.NotReadyAddresses)
	}

	if totalReady+totalNotReady <= maxCapacity {
		return false
	}

	truncateReady := false
	max := maxCapacity - totalReady
	numTotal := totalNotReady
	if totalReady > maxCapacity {
		truncateReady = true
		max = maxCapacity
		numTotal = totalReady
	}
	canBeAdded := max

	for i := range endpoints.Subsets {
		subset := endpoints.Subsets[i]
		numInSubset := len(subset.Addresses)
		if !truncateReady {
			numInSubset = len(subset.NotReadyAddresses)
		}

		// The number of endpoints per subset will be based on the proportion of endpoints
		// in this subset versus the total number of endpoints. The proportion of endpoints
		// will be rounded up which most likely will lead to the last subset having less
		// endpoints than the expected proportion.
		toBeAdded := int(math.Ceil((float64(numInSubset) / float64(numTotal)) * float64(max)))
		// If there is not enough endpoints for the last subset, ensure only the number up
		// to the capacity are added
		if toBeAdded > canBeAdded {
			toBeAdded = canBeAdded
		}

		if truncateReady {
			// Truncate ready Addresses to allocated proportion and truncate all not ready
			// addresses
			subset.Addresses = addressSubset(subset.Addresses, toBeAdded)
			subset.NotReadyAddresses = []v1.EndpointAddress{}
			canBeAdded -= len(subset.Addresses)
		} else {
			// Only truncate the not ready addresses
			subset.NotReadyAddresses = addressSubset(subset.NotReadyAddresses, toBeAdded)
			canBeAdded -= len(subset.NotReadyAddresses)
		}
		endpoints.Subsets[i] = subset
	}
	return true
}

// addressSubset takes a list of addresses and returns a subset if the length is greater
// than the maxNum. If less than the maxNum, the entire list is returned.
func addressSubset(addresses []v1.EndpointAddress, maxNum int) []v1.EndpointAddress {
	if len(addresses) <= maxNum {
		return addresses
	}
	return addresses[0:maxNum]
}

// semanticIgnoreResourceVersion does semantic deep equality checks for objects
// but excludes ResourceVersion of ObjectReference. They are used when comparing
// endpoints in Endpoints and EndpointSlice objects to avoid unnecessary updates
// caused by Pod resourceVersion change.
var semanticIgnoreResourceVersion = conversion.EqualitiesOrDie(
	func(a, b v1.ObjectReference) bool {
		a.ResourceVersion = ""
		b.ResourceVersion = ""
		return a == b
	},
)

// endpointSubsetsEqualIgnoreResourceVersion returns true if EndpointSubsets
// have equal attributes but excludes ResourceVersion of Pod.
func endpointSubsetsEqualIgnoreResourceVersion(subsets1, subsets2 []v1.EndpointSubset) bool {
	return semanticIgnoreResourceVersion.DeepEqual(subsets1, subsets2)
}
