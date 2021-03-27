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

package nodeipam

import (
	"context"
	"fmt"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	coreinformers "k8s.io/client-go/informers/core/v1"
	clientset "k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/events"
	cloudprovider "k8s.io/cloud-provider"
	controllersmetrics "k8s.io/component-base/metrics/prometheus/controllers"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/controller/nodeipam/ipam"
	"net"
)

// ipamController is an interface abstracting an interface for
// legacy mode.
type ipamController interface {
	Run(ctx context.Context)
}

// Controller is the controller that manages node ipam state.
type Controller struct {
	allocatorType ipam.CIDRAllocatorType

	cloud                cloudprovider.Interface
	clusterCIDRs         []*net.IPNet
	serviceCIDR          *net.IPNet
	secondaryServiceCIDR *net.IPNet
	kubeClient           clientset.Interface
	eventBroadcaster     events.EventBroadcaster

	nodeLister         corelisters.NodeLister
	nodeInformerSynced cache.InformerSynced

	legacyIPAM    ipamController
	cidrAllocator ipam.CIDRAllocator
}

// NewNodeIpamController returns a new node IP Address Management controller to
// sync instances from cloudprovider.
// This method returns an error if it is unable to initialize the CIDR bitmap with
// podCIDRs it has already allocated to nodes. Since we don't allow podCIDR changes
// currently, this should be handled as a fatal error.
func NewNodeIpamController(
	ctx context.Context,
	nodeInformer coreinformers.NodeInformer,
	cloud cloudprovider.Interface,
	kubeClient clientset.Interface,
	clusterCIDRs []*net.IPNet,
	serviceCIDR *net.IPNet,
	secondaryServiceCIDR *net.IPNet,
	nodeCIDRMaskSizes []int,
	allocatorType ipam.CIDRAllocatorType) (*Controller, error) {

	if kubeClient == nil {
		return nil, fmt.Errorf("kubeClient is nil when starting Controller")
	}

	eventBroadcaster := events.NewBroadcaster(&events.EventSinkImpl{Interface: kubeClient.EventsV1()})

	// Cloud CIDR allocator does not rely on clusterCIDR or nodeCIDRMaskSize for allocation.
	if allocatorType != ipam.CloudAllocatorType {
		if len(clusterCIDRs) == 0 {
			return nil, fmt.Errorf("Controller: Must specify --cluster-cidr if --allocate-node-cidrs is set")
		}

		for idx, cidr := range clusterCIDRs {
			mask := cidr.Mask
			if maskSize, _ := mask.Size(); maskSize > nodeCIDRMaskSizes[idx] {
				return nil, fmt.Errorf("Controller: Invalid --cluster-cidr, mask size of cluster CIDR must be less than or equal to --node-cidr-mask-size configured for CIDR family")
			}
		}
	}

	ic := &Controller{
		cloud:                cloud,
		kubeClient:           kubeClient,
		eventBroadcaster:     eventBroadcaster,
		clusterCIDRs:         clusterCIDRs,
		serviceCIDR:          serviceCIDR,
		secondaryServiceCIDR: secondaryServiceCIDR,
		allocatorType:        allocatorType,
	}

	// TODO: Abstract this check into a generic controller manager should run method.
	if ic.allocatorType == ipam.IPAMFromClusterAllocatorType || ic.allocatorType == ipam.IPAMFromCloudAllocatorType {
		var err error
		ic.legacyIPAM, err = createLegacyIPAM(ctx, ic, nodeInformer, cloud, kubeClient, clusterCIDRs, serviceCIDR, nodeCIDRMaskSizes)
		if err != nil {
			return nil, err
		}
	} else {
		var err error

		allocatorParams := ipam.CIDRAllocatorParams{
			ClusterCIDRs:         clusterCIDRs,
			ServiceCIDR:          ic.serviceCIDR,
			SecondaryServiceCIDR: ic.secondaryServiceCIDR,
			NodeCIDRMaskSizes:    nodeCIDRMaskSizes,
		}

		ic.cidrAllocator, err = ipam.New(ctx, kubeClient, cloud, nodeInformer, ic.allocatorType, allocatorParams)
		if err != nil {
			return nil, err
		}
	}

	ic.nodeLister = nodeInformer.Lister()
	ic.nodeInformerSynced = nodeInformer.Informer().HasSynced

	return ic, nil
}

// Run starts an asynchronous loop that monitors the status of cluster nodes.
func (nc *Controller) Run(ctx context.Context) {
	defer utilruntime.HandleCrash()

	// Start events processing pipeline.
	nc.eventBroadcaster.StartStructuredLogging(3)
	nc.eventBroadcaster.StartRecordingToSink(ctx.Done())
	defer nc.eventBroadcaster.Shutdown()
	klog.FromContext(ctx).Info("Starting ipam controller")
	defer klog.FromContext(ctx).Info("Shutting down ipam controller")

	if !cache.WaitForNamedCacheSync("node", ctx.Done(), nc.nodeInformerSynced) {
		return
	}

	if nc.allocatorType == ipam.IPAMFromClusterAllocatorType || nc.allocatorType == ipam.IPAMFromCloudAllocatorType {
		go nc.legacyIPAM.Run(ctx)
	} else {
		go nc.cidrAllocator.Run(ctx)
	}

	<-ctx.Done()
}

// RunWithMetrics is a wrapper for Run that also tracks starting and stopping of the nodeipam controller with additional metric
func (nc *Controller) RunWithMetrics(ctx context.Context, controllerManagerMetrics *controllersmetrics.ControllerManagerMetrics) {
	controllerManagerMetrics.ControllerStarted("nodeipam")
	defer controllerManagerMetrics.ControllerStopped("nodeipam")
	nc.Run(ctx)
}
