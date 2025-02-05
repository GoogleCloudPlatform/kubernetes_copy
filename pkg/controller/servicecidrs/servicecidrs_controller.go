/*
Copyright 2023 The Kubernetes Authors.

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

package servicecidrs

import (
	"context"
	"encoding/json"
	"net/netip"
	"time"

	v1 "k8s.io/api/core/v1"
	networkingapiv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	metav1apply "k8s.io/client-go/applyconfigurations/meta/v1"
	networkingapiv1apply "k8s.io/client-go/applyconfigurations/networking/v1"
	networkinginformers "k8s.io/client-go/informers/networking/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	v1core "k8s.io/client-go/kubernetes/typed/core/v1"
	networkinglisters "k8s.io/client-go/listers/networking/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/api/servicecidr"
	"k8s.io/kubernetes/pkg/registry/core/service/ipallocator"
	netutils "k8s.io/utils/net"
)

const (
	// maxRetries is the max number of times a service object will be retried before it is dropped out of the queue.
	// With the current rate-limiter in use (5ms*2^(maxRetries-1)) the following numbers represent the
	// sequence of delays between successive queuings of a service.
	//
	// 5ms, 10ms, 20ms, 40ms, 80ms, 160ms, 320ms, 640ms, 1.3s, 2.6s, 5.1s, 10.2s, 20.4s, 41s, 82s
	maxRetries     = 15
	controllerName = "service-cidr-controller"

	ServiceCIDRProtectionFinalizer = "networking.k8s.io/service-cidr-finalizer"

	// deletionGracePeriod is the time in seconds to wait to remove the finalizer from a ServiceCIDR to ensure the
	// deletion information has been propagated to the apiserver allocators to avoid allocating any IP address
	// before we complete delete the ServiceCIDR
	deletionGracePeriod = 10 * time.Second
)

// NewController returns a new *Controller.
func NewController(
	ctx context.Context,
	serviceCIDRInformer networkinginformers.ServiceCIDRInformer,
	ipAddressInformer networkinginformers.IPAddressInformer,
	client clientset.Interface,
) *Controller {
	broadcaster := record.NewBroadcaster(record.WithContext(ctx))
	recorder := broadcaster.NewRecorder(scheme.Scheme, v1.EventSource{Component: controllerName})
	c := &Controller{
		client: client,
		queue: workqueue.NewTypedRateLimitingQueueWithConfig(
			workqueue.DefaultTypedControllerRateLimiter[string](),
			workqueue.TypedRateLimitingQueueConfig[string]{Name: "ipaddresses"},
		),
		workerLoopPeriod: time.Second,
	}

	_, _ = serviceCIDRInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.addServiceCIDR,
		UpdateFunc: c.updateServiceCIDR,
		DeleteFunc: c.deleteServiceCIDR,
	})
	c.serviceCIDRLister = serviceCIDRInformer.Lister()
	c.serviceCIDRsSynced = serviceCIDRInformer.Informer().HasSynced

	_, _ = ipAddressInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.addIPAddress,
		DeleteFunc: c.deleteIPAddress,
	})

	c.ipAddressLister = ipAddressInformer.Lister()
	c.ipAddressSynced = ipAddressInformer.Informer().HasSynced

	c.eventBroadcaster = broadcaster
	c.eventRecorder = recorder

	return c
}

// Controller manages selector-based service ipAddress.
type Controller struct {
	client           clientset.Interface
	eventBroadcaster record.EventBroadcaster
	eventRecorder    record.EventRecorder

	serviceCIDRLister  networkinglisters.ServiceCIDRLister
	serviceCIDRsSynced cache.InformerSynced

	ipAddressLister networkinglisters.IPAddressLister
	ipAddressSynced cache.InformerSynced

	queue workqueue.TypedRateLimitingInterface[string]

	// workerLoopPeriod is the time between worker runs. The workers process the queue of service and ipRange changes.
	workerLoopPeriod time.Duration
}

// Run will not return until stopCh is closed.
func (c *Controller) Run(ctx context.Context, workers int) {
	defer utilruntime.HandleCrash()
	defer c.queue.ShutDown()

	c.eventBroadcaster.StartStructuredLogging(3)
	c.eventBroadcaster.StartRecordingToSink(&v1core.EventSinkImpl{Interface: c.client.CoreV1().Events("")})
	defer c.eventBroadcaster.Shutdown()

	logger := klog.FromContext(ctx)

	logger.Info("Starting", "controller", controllerName)
	defer logger.Info("Shutting down", "controller", controllerName)

	if !cache.WaitForNamedCacheSync(controllerName, ctx.Done(), c.serviceCIDRsSynced, c.ipAddressSynced) {
		return
	}

	for i := 0; i < workers; i++ {
		go wait.UntilWithContext(ctx, c.worker, c.workerLoopPeriod)
	}
	<-ctx.Done()
}

func (c *Controller) addServiceCIDR(obj interface{}) {
	cidr, ok := obj.(*networkingapiv1.ServiceCIDR)
	if !ok {
		return
	}
	c.queue.Add(cidr.Name)
	for _, key := range c.overlappingServiceCIDRs(cidr) {
		c.queue.Add(key)
	}
}

func (c *Controller) updateServiceCIDR(oldObj, obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err == nil {
		c.queue.Add(key)
	}
}

// deleteServiceCIDR
func (c *Controller) deleteServiceCIDR(obj interface{}) {
	key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
	if err == nil {
		c.queue.Add(key)
	}
}

// addIPAddress may block a ServiceCIDR deletion
func (c *Controller) addIPAddress(obj interface{}) {
	ip, ok := obj.(*networkingapiv1.IPAddress)
	if !ok {
		return
	}

	for _, cidr := range c.containingServiceCIDRs(ip) {
		c.queue.Add(cidr)
	}
}

// deleteIPAddress may unblock a ServiceCIDR deletion
func (c *Controller) deleteIPAddress(obj interface{}) {
	ip, ok := obj.(*networkingapiv1.IPAddress)
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			return
		}
		ip, ok = tombstone.Obj.(*networkingapiv1.IPAddress)
		if !ok {
			return
		}
	}

	for _, cidr := range c.containingServiceCIDRs(ip) {
		c.queue.Add(cidr)
	}
}

// overlappingServiceCIDRs, given a ServiceCIDR return the ServiceCIDRs that contain or are contained,
// this is required because adding or removing a CIDR will require to recompute the
// state of each ServiceCIDR to check if can be unblocked on deletion.
func (c *Controller) overlappingServiceCIDRs(serviceCIDR *networkingapiv1.ServiceCIDR) []string {
	result := sets.New[string]()
	for _, cidr := range serviceCIDR.Spec.CIDRs {
		if prefix, err := netip.ParsePrefix(cidr); err == nil { // if is empty err will not be nil
			serviceCIDRs := servicecidr.OverlapsPrefix(c.serviceCIDRLister, prefix)
			for _, v := range serviceCIDRs {
				result.Insert(v.Name)
			}
		}
	}

	return result.UnsortedList()
}

// containingServiceCIDRs, given an IPAddress return the ServiceCIDRs that contains the IP,
// as it may block or be blocking the deletion of the ServiceCIDRs that contain it.
func (c *Controller) containingServiceCIDRs(ip *networkingapiv1.IPAddress) []string {
	// only process IPs managed by the kube-apiserver
	managedBy, ok := ip.Labels[networkingapiv1.LabelManagedBy]
	if !ok || managedBy != ipallocator.ControllerName {
		return []string{}
	}

	address, err := netip.ParseAddr(ip.Name)
	if err != nil {
		// This should not happen, the IPAddress object validates
		// the name is a valid IPAddress
		return []string{}
	}

	result := sets.New[string]()
	serviceCIDRs := servicecidr.ContainsAddress(c.serviceCIDRLister, address)
	for _, v := range serviceCIDRs {
		result.Insert(v.Name)
	}

	return result.UnsortedList()
}

func (c *Controller) worker(ctx context.Context) {
	for c.processNext(ctx) {
	}
}

func (c *Controller) processNext(ctx context.Context) bool {
	key, quit := c.queue.Get()
	if quit {
		return false
	}
	defer c.queue.Done(key)

	err := c.sync(ctx, key)
	if err == nil {
		c.queue.Forget(key)
		return true
	}
	logger := klog.FromContext(ctx)
	if c.queue.NumRequeues(key) < maxRetries {
		logger.V(2).Info("Error syncing ServiceCIDR, retrying", "ServiceCIDR", key, "err", err)
		c.queue.AddRateLimited(key)
	} else {
		logger.Info("Dropping ServiceCIDR out of the queue", "ServiceCIDR", key, "err", err)
		c.queue.Forget(key)
		utilruntime.HandleError(err)
	}
	return true
}

func (c *Controller) sync(ctx context.Context, key string) error {
	logger := klog.FromContext(ctx)
	startTime := time.Now()
	defer func() {
		logger.V(4).Info("Finished syncing ServiceCIDR)", "ServiceCIDR", key, "elapsed", time.Since(startTime))
	}()

	logger.V(4).Info("syncing ServiceCIDR", "ServiceCIDR", key)
	cidr, err := c.serviceCIDRLister.Get(key)
	if err != nil {
		if apierrors.IsNotFound(err) {
			logger.V(4).Info("ServiceCIDR no longer exist", "ServiceCIDR", key)
			return nil
		}
		return err
	}

	// Deleting ....
	if !cidr.GetDeletionTimestamp().IsZero() {
		// check if the existing ServiceCIDR can be deleted before removing the finalizer
		ok, err := c.canDeleteCIDR(ctx, cidr)
		if err != nil {
			return err
		}
		if !ok {
			// update the status to indicate why the ServiceCIDR can not be deleted,
			// it will be reevaluated by an event on any ServiceCIDR or IPAddress related object
			// that may remove this condition.
			svcApplyStatus := networkingapiv1apply.ServiceCIDRStatus().WithConditions(
				metav1apply.Condition().
					WithType(networkingapiv1.ServiceCIDRConditionReady).
					WithStatus(metav1.ConditionFalse).
					WithReason(networkingapiv1.ServiceCIDRReasonTerminating).
					WithMessage("There are still IPAddresses referencing the ServiceCIDR, please remove them or create a new ServiceCIDR").
					WithLastTransitionTime(metav1.Now()))
			svcApply := networkingapiv1apply.ServiceCIDR(cidr.Name).WithStatus(svcApplyStatus)
			_, err = c.client.NetworkingV1().ServiceCIDRs().ApplyStatus(ctx, svcApply, metav1.ApplyOptions{FieldManager: controllerName, Force: true})
			return err
		}
		// If there are no IPAddress depending on this ServiceCIDR is safe to remove it,
		// however, there can be a race when the allocators still consider the ServiceCIDR
		// ready and allocate a new IPAddress from them, to avoid that, we wait during a
		// a grace period to be sure the deletion change has been propagated to the allocators
		// and no new IPAddress is going to be allocated.
		timeUntilDeleted := deletionGracePeriod - time.Since(cidr.GetDeletionTimestamp().Time)
		if timeUntilDeleted > 0 {
			c.queue.AddAfter(key, timeUntilDeleted)
			return nil
		}
		return c.removeServiceCIDRFinalizerIfNeeded(ctx, cidr)
	}

	// Created or Updated, the ServiceCIDR must have a finalizer.
	err = c.addServiceCIDRFinalizerIfNeeded(ctx, cidr)
	if err != nil {
		return err
	}

	// Set Ready condition to True.
	svcApplyStatus := networkingapiv1apply.ServiceCIDRStatus().WithConditions(
		metav1apply.Condition().
			WithType(networkingapiv1.ServiceCIDRConditionReady).
			WithStatus(metav1.ConditionTrue).
			WithMessage("Kubernetes Service CIDR is ready").
			WithLastTransitionTime(metav1.Now()))
	svcApply := networkingapiv1apply.ServiceCIDR(cidr.Name).WithStatus(svcApplyStatus)
	if _, err := c.client.NetworkingV1().ServiceCIDRs().ApplyStatus(ctx, svcApply, metav1.ApplyOptions{FieldManager: controllerName, Force: true}); err != nil {
		logger.Info("error updating default ServiceCIDR status", "error", err)
		c.eventRecorder.Eventf(cidr, v1.EventTypeWarning, "KubernetesServiceCIDRError", "The ServiceCIDR Status can not be set to Ready=True")
		return err
	}

	return nil
}

// canDeleteCIDR checks that the ServiceCIDR can be safely deleted and not leave orphan IPAddresses
func (c *Controller) canDeleteCIDR(ctx context.Context, serviceCIDR *networkingapiv1.ServiceCIDR) (bool, error) {
	logger := klog.FromContext(ctx)
	// Check if there is a subnet that already contains the ServiceCIDR that is going to be deleted.
	hasParent := true
	for _, cidr := range serviceCIDR.Spec.CIDRs {
		// Find if there is a larger subnet that contains the existing one,
		// or there is another ServiceCIDR with the same subnet.
		if prefix, err := netip.ParsePrefix(cidr); err == nil {
			serviceCIDRs := servicecidr.ContainsPrefix(c.serviceCIDRLister, prefix)
			if len(serviceCIDRs) == 0 ||
				len(serviceCIDRs) == 1 && serviceCIDRs[0].Name == serviceCIDR.Name {
				hasParent = false
			}
		}
	}

	// All the existing IP addresses will be contained on the parent ServiceCIDRs,
	// it is safe to delete, remove the finalizer.
	if hasParent {
		logger.Info("Deleting ServiceCIDR contained in other ServiceCIDR", "ServiceCIDR", serviceCIDR.String())
		return true, nil
	}

	// TODO: optimize this
	// Since current ServiceCIDR does not have another ServiceCIDR containing it,
	// verify there are no existing IPAddresses referencing it that will be orphan.
	for _, cidr := range serviceCIDR.Spec.CIDRs {
		// get all the IPv4 addresses
		ipLabelSelector := labels.Set(map[string]string{
			networkingapiv1.LabelIPAddressFamily: string(convertToV1IPFamily(netutils.IPFamilyOfCIDRString(cidr))),
			networkingapiv1.LabelManagedBy:       ipallocator.ControllerName,
		}).AsSelectorPreValidated()
		ips, err := c.ipAddressLister.List(ipLabelSelector)
		if err != nil {
			return false, err
		}
		for _, ip := range ips {
			// if the longest prefix match is the ServiceCIDR to be deleted
			// and is the only existing one, at least one IPAddress will be
			// orphan, block the ServiceCIDR deletion.
			address, err := netip.ParseAddr(ip.Name)
			if err != nil {
				// the IPAddress object validates that the name is a valid IPAddress
				logger.Info("[SHOULD NOT HAPPEN] unexpected error parsing IPAddress", "IPAddress", ip.Name, "error", err)
				continue
			}
			// find all ServiceCIDRs containing this IP
			serviceCIDRs := servicecidr.ContainsAddress(c.serviceCIDRLister, address)
			if len(serviceCIDRs) == 1 && serviceCIDRs[0].Name == serviceCIDR.Name {
				logger.Info("Deleting ServiceCIDR blocked by IP address", "IPAddress", address.String())
				return false, nil
			}
		}
	}

	// There are no IPAddresses that depend on the existing ServiceCIDR, so
	// it is safe to delete, remove finalizer.
	logger.Info("Deleting ServiceCIDR no longer have orphan IPs", "ServiceCIDR", serviceCIDR.String())
	return true, nil
}

func (c *Controller) addServiceCIDRFinalizerIfNeeded(ctx context.Context, cidr *networkingapiv1.ServiceCIDR) error {
	for _, f := range cidr.GetFinalizers() {
		if f == ServiceCIDRProtectionFinalizer {
			return nil
		}
	}

	patch := map[string]interface{}{
		"metadata": map[string]interface{}{
			"finalizers": []string{ServiceCIDRProtectionFinalizer},
		},
	}
	patchBytes, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	_, err = c.client.NetworkingV1().ServiceCIDRs().Patch(ctx, cidr.Name, types.StrategicMergePatchType, patchBytes, metav1.PatchOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	klog.FromContext(ctx).V(4).Info("Added protection finalizer to ServiceCIDR", "ServiceCIDR", cidr.Name)
	return nil

}

func (c *Controller) removeServiceCIDRFinalizerIfNeeded(ctx context.Context, cidr *networkingapiv1.ServiceCIDR) error {
	found := false
	for _, f := range cidr.GetFinalizers() {
		if f == ServiceCIDRProtectionFinalizer {
			found = true
			break
		}
	}
	if !found {
		return nil
	}
	patch := map[string]interface{}{
		"metadata": map[string]interface{}{
			"$deleteFromPrimitiveList/finalizers": []string{ServiceCIDRProtectionFinalizer},
		},
	}
	patchBytes, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	_, err = c.client.NetworkingV1().ServiceCIDRs().Patch(ctx, cidr.Name, types.StrategicMergePatchType, patchBytes, metav1.PatchOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	klog.FromContext(ctx).V(4).Info("Removed protection finalizer from ServiceCIDRs", "ServiceCIDR", cidr.Name)
	return nil
}

// Convert netutils.IPFamily to v1.IPFamily
// TODO: consolidate helpers
// copied from pkg/proxy/util/utils.go
func convertToV1IPFamily(ipFamily netutils.IPFamily) v1.IPFamily {
	switch ipFamily {
	case netutils.IPv4:
		return v1.IPv4Protocol
	case netutils.IPv6:
		return v1.IPv6Protocol
	}

	return v1.IPFamilyUnknown
}
