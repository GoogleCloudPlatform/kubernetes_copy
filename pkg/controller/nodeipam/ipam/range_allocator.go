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

package ipam

import (
	"context"
	"fmt"
	"net"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"
	netutils "k8s.io/utils/net"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	informers "k8s.io/client-go/informers/core/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	v1core "k8s.io/client-go/kubernetes/typed/core/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	nodeutil "k8s.io/component-helpers/node/util"
	"k8s.io/kubernetes/pkg/controller/nodeipam/ipam/cidrset"
	controllerutil "k8s.io/kubernetes/pkg/controller/util/node"
)

type rangeAllocator struct {
	client clientset.Interface
	// cluster cidrs as passed in during controller creation
	clusterCIDRs []*net.IPNet
	// for each entry in clusterCIDRs we maintain a list of what is used and what is not
	cidrSets []*cidrset.CidrSet
	// nodeLister is able to list/get nodes and is populated by the shared informer passed to controller
	nodeLister corelisters.NodeLister
	// nodesSynced returns true if the node shared informer has been synced at least once.
	nodesSynced cache.InformerSynced
	broadcaster record.EventBroadcaster
	recorder    record.EventRecorder

	// queues are where incoming work is placed to de-dup and to allow "easy"
	// rate limited requeues on errors
	queue workqueue.RateLimitingInterface
}

var _ CIDRAllocator = &rangeAllocator{}

// NewCIDRRangeAllocator returns a CIDRAllocator to allocate CIDRs for node (one from each of clusterCIDRs)
// Caller must ensure subNetMaskSize is not less than cluster CIDR mask size.
// Caller must always pass in a list of existing nodes so the new allocator.
// Caller must ensure that ClusterCIDRs are semantically correct e.g (1 for non DualStack, 2 for DualStack etc..)
// can initialize its CIDR map. NodeList is only nil in testing.
func NewCIDRRangeAllocator(ctx context.Context, client clientset.Interface, nodeInformer informers.NodeInformer, allocatorParams CIDRAllocatorParams, nodeList *v1.NodeList) (CIDRAllocator, error) {
	logger := klog.FromContext(ctx)
	if client == nil {
		logger.Error(nil, "kubeClient is nil when starting CIDRRangeAllocator")
		klog.FlushAndExit(klog.ExitFlushTimeout, 1)
	}

	eventBroadcaster := record.NewBroadcaster(record.WithContext(ctx))
	recorder := eventBroadcaster.NewRecorder(scheme.Scheme, v1.EventSource{Component: "cidrAllocator"})

	// create a cidrSet for each cidr we operate on
	// cidrSet are mapped to clusterCIDR by index
	cidrSets := make([]*cidrset.CidrSet, len(allocatorParams.ClusterCIDRs))
	for idx, cidr := range allocatorParams.ClusterCIDRs {
		cidrSet, err := cidrset.NewCIDRSet(cidr, allocatorParams.NodeCIDRMaskSizes[idx])
		if err != nil {
			return nil, err
		}
		cidrSets[idx] = cidrSet
	}

	ra := &rangeAllocator{
		client:       client,
		clusterCIDRs: allocatorParams.ClusterCIDRs,
		cidrSets:     cidrSets,
		nodeLister:   nodeInformer.Lister(),
		nodesSynced:  nodeInformer.Informer().HasSynced,
		broadcaster:  eventBroadcaster,
		recorder:     recorder,
		queue:        workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "cidrallocator_node"),
	}

	if allocatorParams.ServiceCIDR != nil {
		ra.filterOutServiceRange(logger, allocatorParams.ServiceCIDR)
	} else {
		logger.Info("No Service CIDR provided. Skipping filtering out service addresses")
	}

	if allocatorParams.SecondaryServiceCIDR != nil {
		ra.filterOutServiceRange(logger, allocatorParams.SecondaryServiceCIDR)
	} else {
		logger.Info("No Secondary Service CIDR provided. Skipping filtering out secondary service addresses")
	}

	if nodeList != nil {
		for _, node := range nodeList.Items {
			if len(node.Spec.PodCIDRs) == 0 {
				logger.V(4).Info("Node has no CIDR, ignoring", "node", klog.KObj(&node))
				continue
			}
			logger.V(4).Info("Node has CIDR, occupying it in CIDR map", "node", klog.KObj(&node), "podCIDR", node.Spec.PodCIDR)
			if err := ra.occupyCIDRs(&node); err != nil {
				// This will happen if:
				// 1. We find garbage in the podCIDRs field. Retrying is useless.
				// 2. CIDR out of range: This means a node CIDR has changed.
				// This error will keep crashing controller-manager.
				return nil, err
			}
		}
	}

	nodeInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			key, err := cache.MetaNamespaceKeyFunc(obj)
			if err == nil {
				ra.queue.Add(key)
			}
		},
		UpdateFunc: func(old, new interface{}) {
			key, err := cache.MetaNamespaceKeyFunc(new)
			if err == nil {
				ra.queue.Add(key)
			}
		},
		DeleteFunc: func(obj interface{}) {
			// The informer cache no longer has the object, and since Node doesn't have a finalizer,
			// we don't see the Update with DeletionTimestamp != 0.
			node, ok := obj.(*v1.Node)
			if !ok {
				tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
				if !ok {
					utilruntime.HandleError(fmt.Errorf("unexpected object type: %v", obj))
					return
				}
				node, ok = tombstone.Obj.(*v1.Node)
				if !ok {
					utilruntime.HandleError(fmt.Errorf("unexpected object types: %v", obj))
					return
				}
			}
			if err := ra.ReleaseCIDR(logger, node); err != nil {
				utilruntime.HandleError(fmt.Errorf("error while processing CIDR Release: %w", err))

			}
		},
	})

	return ra, nil
}

func (r *rangeAllocator) Run(ctx context.Context) {
	defer utilruntime.HandleCrash()

	// Start event processing pipeline.
	r.broadcaster.StartStructuredLogging(3)
	logger := klog.FromContext(ctx)
	logger.Info("Sending events to api server")
	r.broadcaster.StartRecordingToSink(&v1core.EventSinkImpl{Interface: r.client.CoreV1().Events("")})
	defer r.broadcaster.Shutdown()

	defer r.queue.ShutDown()

	logger.Info("Starting range CIDR allocator")
	defer logger.Info("Shutting down range CIDR allocator")

	if !cache.WaitForNamedCacheSync("cidrallocator", ctx.Done(), r.nodesSynced) {
		return
	}

	for i := 0; i < cidrUpdateWorkers; i++ {
		go wait.UntilWithContext(ctx, r.runWorker, time.Second)
	}

	<-ctx.Done()
}

// runWorker is a long-running function that will continually call the
// processNextWorkItem function in order to read and process a message on the
// queue.
func (r *rangeAllocator) runWorker(ctx context.Context) {
	for r.processNextNodeWorkItem(ctx) {
	}
}

// processNextWorkItem will read a single work item off the queue and
// attempt to process it, by calling the syncHandler.
func (r *rangeAllocator) processNextNodeWorkItem(ctx context.Context) bool {
	obj, shutdown := r.queue.Get()
	if shutdown {
		return false
	}

	// We wrap this block in a func so we can defer r.queue.Done.
	err := func(logger klog.Logger, obj interface{}) error {
		// We call Done here so the workNodeQueue knows we have finished
		// processing this item. We also must remember to call Forget if we
		// do not want this work item being re-queued. For example, we do
		// not call Forget if a transient error occurs, instead the item is
		// put back on the queue and attempted again after a back-off
		// period.
		defer r.queue.Done(obj)
		var key string
		var ok bool
		// We expect strings to come off the workNodeQueue. These are of the
		// form namespace/name. We do this as the delayed nature of the
		// workNodeQueue means the items in the informer cache may actually be
		// more up to date that when the item was initially put onto the
		// workNodeQueue.
		if key, ok = obj.(string); !ok {
			// As the item in the workNodeQueue is actually invalid, we call
			// Forget here else we'd go into a loop of attempting to
			// process a work item that is invalid.
			r.queue.Forget(obj)
			utilruntime.HandleError(fmt.Errorf("expected string in workNodeQueue but got %#v", obj))
			return nil
		}
		// Run the syncHandler, passing it the namespace/name string of the
		// Foo resource to be synced.
		if err := r.syncNode(ctx, key); err != nil {
			// Put the item back on the queue to handle any transient errors.
			r.queue.AddRateLimited(key)
			return fmt.Errorf("error syncing '%s': %s, requeuing", key, err.Error())
		}
		// Finally, if no error occurs we Forget this item so it does not
		// get queue again until another change happens.
		r.queue.Forget(obj)
		logger.Info("Successfully synced", "key", key)
		return nil
	}(klog.FromContext(ctx), obj)

	if err != nil {
		utilruntime.HandleError(err)
		return true
	}

	return true
}

func (r *rangeAllocator) syncNode(ctx context.Context, key string) error {
	logger := klog.FromContext(ctx)
	startTime := time.Now()
	defer func() {
		logger.V(4).Info("Finished syncing Node request", "node", key, "elapsed", time.Since(startTime))
	}()

	node, err := r.nodeLister.Get(key)
	if apierrors.IsNotFound(err) {
		logger.V(3).Info("node has been deleted", "node", key)
		// TODO: obtain the node object information to call ReleaseCIDR from here
		// and retry if there is an error.
		return nil
	}
	if err != nil {
		return err
	}
	// Check the DeletionTimestamp to determine if object is under deletion.
	if !node.DeletionTimestamp.IsZero() {
		logger.V(3).Info("node is being deleted", "node", key)
		return nil
	}
	return r.AllocateOrOccupyCIDR(ctx, node)
}

// marks node.PodCIDRs[...] as used in allocator's tracked cidrSet
func (r *rangeAllocator) occupyCIDRs(node *v1.Node) error {
	if len(node.Spec.PodCIDRs) == 0 {
		return nil
	}
	for idx, cidr := range node.Spec.PodCIDRs {
		_, podCIDR, err := netutils.ParseCIDRSloppy(cidr)
		if err != nil {
			return fmt.Errorf("failed to parse node %s, CIDR %s", node.Name, node.Spec.PodCIDR)
		}
		// If node has a pre allocate cidr that does not exist in our cidrs.
		// This will happen if cluster went from dualstack(multi cidrs) to non-dualstack
		// then we have now way of locking it
		if idx >= len(r.cidrSets) {
			return fmt.Errorf("node:%s has an allocated cidr: %v at index:%v that does not exist in cluster cidrs configuration", node.Name, cidr, idx)
		}

		if err := r.cidrSets[idx].Occupy(podCIDR); err != nil {
			return fmt.Errorf("failed to mark cidr[%v] at idx [%v] as occupied for node: %v: %v", podCIDR, idx, node.Name, err)
		}
	}
	return nil
}

// WARNING: If you're adding any return calls or defer any more work from this
// function you have to make sure to update nodesInProcessing properly with the
// disposition of the node when the work is done.
func (r *rangeAllocator) AllocateOrOccupyCIDR(ctx context.Context, node *v1.Node) error {
	if node == nil {
		return nil
	}

	if len(node.Spec.PodCIDRs) > 0 {
		return r.occupyCIDRs(node)
	}

	logger := klog.FromContext(ctx)
	allocatedCIDRs := make([]*net.IPNet, len(r.cidrSets))

	for idx := range r.cidrSets {
		podCIDR, err := r.cidrSets[idx].AllocateNext()
		if err != nil {
			controllerutil.RecordNodeStatusChange(logger, r.recorder, node, "CIDRNotAvailable")
			return fmt.Errorf("failed to allocate cidr from cluster cidr at idx:%v: %v", idx, err)
		}
		allocatedCIDRs[idx] = podCIDR
	}

	// queue the assignment
	logger.V(4).Info("Putting node with CIDR into the work queue", "node", klog.KObj(node), "CIDRs", allocatedCIDRs)
	return r.updateCIDRsAllocation(ctx, node.Name, allocatedCIDRs)
}

// ReleaseCIDR marks node.podCIDRs[...] as unused in our tracked cidrSets
func (r *rangeAllocator) ReleaseCIDR(logger klog.Logger, node *v1.Node) error {
	if node == nil || len(node.Spec.PodCIDRs) == 0 {
		return nil
	}

	for idx, cidr := range node.Spec.PodCIDRs {
		_, podCIDR, err := netutils.ParseCIDRSloppy(cidr)
		if err != nil {
			return fmt.Errorf("failed to parse CIDR %s on Node %v: %v", cidr, node.Name, err)
		}

		// If node has a pre allocate cidr that does not exist in our cidrs.
		// This will happen if cluster went from dualstack(multi cidrs) to non-dualstack
		// then we have now way of locking it
		if idx >= len(r.cidrSets) {
			return fmt.Errorf("node:%s has an allocated cidr: %v at index:%v that does not exist in cluster cidrs configuration", node.Name, cidr, idx)
		}

		logger.V(4).Info("Release CIDR for node", "CIDR", cidr, "node", klog.KObj(node))
		if err = r.cidrSets[idx].Release(podCIDR); err != nil {
			return fmt.Errorf("error when releasing CIDR %v: %v", cidr, err)
		}
	}
	return nil
}

// Marks all CIDRs with subNetMaskSize that belongs to serviceCIDR as used across all cidrs
// so that they won't be assignable.
func (r *rangeAllocator) filterOutServiceRange(logger klog.Logger, serviceCIDR *net.IPNet) {
	// Checks if service CIDR has a nonempty intersection with cluster
	// CIDR. It is the case if either clusterCIDR contains serviceCIDR with
	// clusterCIDR's Mask applied (this means that clusterCIDR contains
	// serviceCIDR) or vice versa (which means that serviceCIDR contains
	// clusterCIDR).
	for idx, cidr := range r.clusterCIDRs {
		// if they don't overlap then ignore the filtering
		if !cidr.Contains(serviceCIDR.IP.Mask(cidr.Mask)) && !serviceCIDR.Contains(cidr.IP.Mask(serviceCIDR.Mask)) {
			continue
		}

		// at this point, len(cidrSet) == len(clusterCidr)
		if err := r.cidrSets[idx].Occupy(serviceCIDR); err != nil {
			logger.Error(err, "Error filtering out service cidr out cluster cidr", "CIDR", cidr, "index", idx, "serviceCIDR", serviceCIDR)
		}
	}
}

// updateCIDRsAllocation assigns CIDR to Node and sends an update to the API server.
func (r *rangeAllocator) updateCIDRsAllocation(ctx context.Context, nodeName string, allocatedCIDRs []*net.IPNet) error {
	var err error
	var node *v1.Node
	logger := klog.FromContext(ctx)
	cidrsString := ipnetToStringList(allocatedCIDRs)
	node, err = r.nodeLister.Get(nodeName)
	if err != nil {
		logger.Error(err, "Failed while getting node for updating Node.Spec.PodCIDRs", "node", klog.KRef("", nodeName))
		return err
	}

	// if cidr list matches the proposed.
	// then we possibly updated this node
	// and just failed to ack the success.
	if len(node.Spec.PodCIDRs) == len(allocatedCIDRs) {
		match := true
		for idx, cidr := range cidrsString {
			if node.Spec.PodCIDRs[idx] != cidr {
				match = false
				break
			}
		}
		if match {
			logger.V(4).Info("Node already has allocated CIDR. It matches the proposed one", "node", klog.KObj(node), "CIDRs", allocatedCIDRs)
			return nil
		}
	}

	// node has cidrs, release the reserved
	if len(node.Spec.PodCIDRs) != 0 {
		logger.Error(nil, "Node already has a CIDR allocated. Releasing the new one", "node", klog.KObj(node), "podCIDRs", node.Spec.PodCIDRs)
		for idx, cidr := range allocatedCIDRs {
			if releaseErr := r.cidrSets[idx].Release(cidr); releaseErr != nil {
				logger.Error(releaseErr, "Error when releasing CIDR", "index", idx, "CIDR", cidr)
			}
		}
		return nil
	}

	// If we reached here, it means that the node has no CIDR currently assigned. So we set it.
	for i := 0; i < cidrUpdateRetries; i++ {
		if err = nodeutil.PatchNodeCIDRs(ctx, r.client, types.NodeName(node.Name), cidrsString); err == nil {
			logger.Info("Set node PodCIDR", "node", klog.KObj(node), "podCIDRs", cidrsString)
			return nil
		}
	}
	// failed release back to the pool
	logger.Error(err, "Failed to update node PodCIDR after multiple attempts", "node", klog.KObj(node), "podCIDRs", cidrsString)
	controllerutil.RecordNodeStatusChange(logger, r.recorder, node, "CIDRAssignmentFailed")
	// We accept the fact that we may leak CIDRs here. This is safer than releasing
	// them in case when we don't know if request went through.
	// NodeController restart will return all falsely allocated CIDRs to the pool.
	if !apierrors.IsServerTimeout(err) {
		logger.Error(err, "CIDR assignment for node failed. Releasing allocated CIDR", "node", klog.KObj(node))
		for idx, cidr := range allocatedCIDRs {
			if releaseErr := r.cidrSets[idx].Release(cidr); releaseErr != nil {
				logger.Error(releaseErr, "Error releasing allocated CIDR for node", "node", klog.KObj(node))
			}
		}
	}
	return err
}
