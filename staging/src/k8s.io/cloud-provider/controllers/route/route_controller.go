/*
Copyright 2015 The Kubernetes Authors.

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

package route

import (
	"context"
	"fmt"
	"net"
	"reflect"
	"sync"
	"time"

	"k8s.io/klog/v2"
	netutils "k8s.io/utils/net"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	coreinformers "k8s.io/client-go/informers/core/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	v1core "k8s.io/client-go/kubernetes/typed/core/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	clientretry "k8s.io/client-go/util/retry"
	cloudprovider "k8s.io/cloud-provider"
	controllersmetrics "k8s.io/component-base/metrics/prometheus/controllers"
	nodeutil "k8s.io/component-helpers/node/util"
)

const (
	// Maximal number of concurrent route operation API calls.
	// TODO: This should be per-provider.
	maxConcurrentRouteOperations int = 200
)

var updateNetworkConditionBackoff = wait.Backoff{
	Steps:    5, // Maximum number of retries.
	Duration: 100 * time.Millisecond,
	Jitter:   1.0,
}

type RouteController struct {
	routes           cloudprovider.Routes
	kubeClient       clientset.Interface
	clusterName      string
	clusterCIDRs     []*net.IPNet
	nodeLister       corelisters.NodeLister
	nodeListerSynced cache.InformerSynced
	broadcaster      record.EventBroadcaster
	recorder         record.EventRecorder
}

func New(routes cloudprovider.Routes, kubeClient clientset.Interface, nodeInformer coreinformers.NodeInformer, clusterName string, clusterCIDRs []*net.IPNet) *RouteController {
	if len(clusterCIDRs) == 0 {
		klog.Fatal("RouteController: Must specify clusterCIDR.")
	}

	rc := &RouteController{
		routes:           routes,
		kubeClient:       kubeClient,
		clusterName:      clusterName,
		clusterCIDRs:     clusterCIDRs,
		nodeLister:       nodeInformer.Lister(),
		nodeListerSynced: nodeInformer.Informer().HasSynced,
	}

	return rc
}

func (rc *RouteController) Run(ctx context.Context, syncPeriod time.Duration, controllerManagerMetrics *controllersmetrics.ControllerManagerMetrics) {
	defer utilruntime.HandleCrash()

	rc.broadcaster = record.NewBroadcaster(record.WithContext(ctx))
	rc.recorder = rc.broadcaster.NewRecorder(scheme.Scheme, v1.EventSource{Component: "route_controller"})

	// Start event processing pipeline.
	if rc.broadcaster != nil {
		rc.broadcaster.StartStructuredLogging(0)
		rc.broadcaster.StartRecordingToSink(&v1core.EventSinkImpl{Interface: rc.kubeClient.CoreV1().Events("")})
		defer rc.broadcaster.Shutdown()
	}

	klog.Info("Starting route controller")
	defer klog.Info("Shutting down route controller")
	controllerManagerMetrics.ControllerStarted("route")
	defer controllerManagerMetrics.ControllerStopped("route")

	if !cache.WaitForNamedCacheSync("route", ctx.Done(), rc.nodeListerSynced) {
		return
	}

	// TODO: If we do just the full Resync every 5 minutes (default value)
	// that means that we may wait up to 5 minutes before even starting
	// creating a route for it. This is bad.
	// We should have a watch on node and if we observe a new node (with CIDR?)
	// trigger reconciliation for that node.
	go wait.NonSlidingUntil(func() {
		if err := rc.reconcileNodeRoutes(ctx); err != nil {
			klog.Errorf("Couldn't reconcile node routes: %v", err)
		}
	}, syncPeriod, ctx.Done())

	<-ctx.Done()
}

func (rc *RouteController) reconcileNodeRoutes(ctx context.Context) error {
	routeList, err := rc.routes.ListRoutes(ctx, rc.clusterName)
	if err != nil {
		return fmt.Errorf("error listing routes: %v", err)
	}
	nodes, err := rc.nodeLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("error listing nodes: %v", err)
	}
	return rc.reconcile(ctx, nodes, routeList)
}

type routeAction string

var (
	keep   routeAction = "keep"
	add    routeAction = "add"
	remove routeAction = "remove"
	update routeAction = "update"
)

type routeNode struct {
	name            types.NodeName
	addrs           []v1.NodeAddress
	routes          []*cloudprovider.Route
	cidrWithActions *map[string]routeAction
}

func (rc *RouteController) reconcile(ctx context.Context, nodes []*v1.Node, routes []*cloudprovider.Route) error {
	var l sync.Mutex
	// routeMap includes info about a target Node and its addresses, routes and a map between Pod CIDRs and actions.
	// If action is add/remove, the route will be added/removed.
	// If action is keep, the route will not be touched.
	// If action is update, the route will be deleted and then added.
	routeMap := make(map[types.NodeName]routeNode)

	// Put current routes into routeMap.
	for _, route := range routes {
		if route.TargetNode == "" {
			continue
		}
		rn, ok := routeMap[route.TargetNode]
		if !ok {
			rn = routeNode{
				name:            route.TargetNode,
				addrs:           []v1.NodeAddress{},
				routes:          []*cloudprovider.Route{},
				cidrWithActions: &map[string]routeAction{},
			}
		} else if rn.routes == nil {
			rn.routes = []*cloudprovider.Route{}
		}
		rn.routes = append(rn.routes, route)
		routeMap[route.TargetNode] = rn
	}

	wg := sync.WaitGroup{}
	rateLimiter := make(chan struct{}, maxConcurrentRouteOperations)
	// searches existing routes by node for a matching route

	// Check Nodes and their Pod CIDRs. Then put expected route actions into nodePodCIDRActionMap.
	// Add addresses of Nodes into routeMap.
	for _, node := range nodes {
		// Skip if the node hasn't been assigned a CIDR yet.
		if len(node.Spec.PodCIDRs) == 0 {
			continue
		}
		nodeName := types.NodeName(node.Name)
		l.Lock()
		rn, ok := routeMap[nodeName]
		if !ok {
			rn = routeNode{
				name:            nodeName,
				addrs:           []v1.NodeAddress{},
				routes:          []*cloudprovider.Route{},
				cidrWithActions: &map[string]routeAction{},
			}
		}
		rn.addrs = node.Status.Addresses
		routeMap[nodeName] = rn
		l.Unlock()
		// for every node, for every cidr
		for _, podCIDR := range node.Spec.PodCIDRs {
			// we add it to our nodeCIDRs map here because if we don't consider Node addresses change,
			// add and delete go routines run simultaneously.
			l.Lock()
			action := getRouteAction(rn.routes, podCIDR, nodeName, node.Status.Addresses)
			(*routeMap[nodeName].cidrWithActions)[podCIDR] = action
			l.Unlock()
			klog.Infof("action for Node %q with CIDR %q: %q", nodeName, podCIDR, action)
		}
	}

	// searches our bag of node -> cidrs for a match
	// If the action doesn't exist, action is remove or update, then the route should be deleted.
	shouldDeleteRoute := func(nodeName types.NodeName, cidr string) bool {
		l.Lock()
		defer l.Unlock()

		cidrWithActions := routeMap[nodeName].cidrWithActions
		if cidrWithActions == nil {
			return true
		}
		action, exist := (*cidrWithActions)[cidr]
		if !exist || action == remove || action == update {
			klog.Infof("route should be deleted, spec: exist: %v, action: %q, Node %q, CIDR %q", exist, action, nodeName, cidr)
			return true
		}
		return false
	}

	// remove routes that are not in use or need to be updated.
	for _, route := range routes {
		if !rc.isResponsibleForRoute(route) {
			continue
		}
		// Check if this route is a blackhole, or applies to a node we know about & CIDR status is created.
		if route.Blackhole || shouldDeleteRoute(route.TargetNode, route.DestinationCIDR) {
			wg.Add(1)
			// Delete the route.
			go func(route *cloudprovider.Route, startTime time.Time) {
				defer wg.Done()
				// respect the rate limiter
				rateLimiter <- struct{}{}
				klog.Infof("Deleting route %s %s", route.Name, route.DestinationCIDR)
				if err := rc.routes.DeleteRoute(ctx, rc.clusterName, route); err != nil {
					klog.Errorf("Could not delete route %s %s after %v: %v", route.Name, route.DestinationCIDR, time.Since(startTime), err)
				} else {
					klog.Infof("Deleted route %s %s after %v", route.Name, route.DestinationCIDR, time.Since(startTime))
				}
				<-rateLimiter
			}(route, time.Now())
		}
	}
	// https://github.com/kubernetes/kubernetes/issues/98359
	// When routesUpdated is true, Route addition and deletion cannot run simultaneously because if action is update,
	// the same route may be added and deleted.
	if len(routes) != 0 && routes[0].EnableNodeAddresses {
		wg.Wait()
	}

	// Now create new routes or update existing ones.
	for _, node := range nodes {
		// Skip if the node hasn't been assigned a CIDR yet.
		if len(node.Spec.PodCIDRs) == 0 {
			continue
		}
		nodeName := types.NodeName(node.Name)

		// for every node, for every cidr
		for _, podCIDR := range node.Spec.PodCIDRs {
			l.Lock()
			action := (*routeMap[nodeName].cidrWithActions)[podCIDR]
			l.Unlock()
			if action == keep || action == remove {
				continue
			}
			// if we are here, then a route needs to be created for this node
			route := &cloudprovider.Route{
				TargetNode:          nodeName,
				TargetNodeAddresses: node.Status.Addresses,
				DestinationCIDR:     podCIDR,
			}
			klog.Infof("route spec to be created: %v", route)
			// cloud providers that:
			// - depend on nameHint
			// - trying to support dual stack
			// will have to carefully generate new route names that allow node->(multi cidr)
			nameHint := string(node.UID)
			wg.Add(1)
			go func(nodeName types.NodeName, nameHint string, route *cloudprovider.Route) {
				defer wg.Done()
				err := clientretry.RetryOnConflict(updateNetworkConditionBackoff, func() error {
					startTime := time.Now()
					// Ensure that we don't have more than maxConcurrentRouteOperations
					// CreateRoute calls in flight.
					rateLimiter <- struct{}{}
					klog.Infof("Creating route for node %s %s with hint %s, throttled %v", nodeName, route.DestinationCIDR, nameHint, time.Since(startTime))
					err := rc.routes.CreateRoute(ctx, rc.clusterName, nameHint, route)
					<-rateLimiter
					if err != nil {
						msg := fmt.Sprintf("Could not create route %s %s for node %s after %v: %v", nameHint, route.DestinationCIDR, nodeName, time.Since(startTime), err)
						if rc.recorder != nil {
							rc.recorder.Eventf(
								&v1.ObjectReference{
									Kind:      "Node",
									Name:      string(nodeName),
									UID:       types.UID(nodeName),
									Namespace: "",
								}, v1.EventTypeWarning, "FailedToCreateRoute", msg)
							klog.V(4).Info(msg)
							return err
						}
					}
					l.Lock()
					// Mark the route action as done (keep)
					(*routeMap[nodeName].cidrWithActions)[route.DestinationCIDR] = keep
					l.Unlock()
					klog.Infof("Created route for node %s %s with hint %s after %v", nodeName, route.DestinationCIDR, nameHint, time.Since(startTime))
					return nil
				})
				if err != nil {
					klog.Errorf("Could not create route %s %s for node %s: %v", nameHint, route.DestinationCIDR, nodeName, err)
				}
			}(nodeName, nameHint, route)
		}
	}
	wg.Wait()

	// after all route actions have been done (or not), we start updating
	// all nodes' statuses with the outcome
	for _, node := range nodes {
		actions := routeMap[types.NodeName(node.Name)].cidrWithActions
		if actions == nil {
			continue
		}

		wg.Add(1)
		if len(*actions) == 0 {
			go func(n *v1.Node) {
				defer wg.Done()
				klog.Infof("node %v has no routes assigned to it. NodeNetworkUnavailable will be set to true", n.Name)
				if err := rc.updateNetworkingCondition(n, false); err != nil {
					klog.Errorf("failed to update networking condition when no actions: %v", err)
				}
			}(node)
			continue
		}

		// check if all route actions were done. if so, then it should be ready
		allRoutesCreated := true
		for _, action := range *actions {
			if action == add || action == update {
				allRoutesCreated = false
				break
			}
		}
		go func(n *v1.Node) {
			defer wg.Done()
			if err := rc.updateNetworkingCondition(n, allRoutesCreated); err != nil {
				klog.Errorf("failed to update networking condition: %v", err)
			}
		}(node)
	}
	wg.Wait()
	return nil
}

func (rc *RouteController) updateNetworkingCondition(node *v1.Node, routesCreated bool) error {
	_, condition := nodeutil.GetNodeCondition(&(node.Status), v1.NodeNetworkUnavailable)
	if routesCreated && condition != nil && condition.Status == v1.ConditionFalse {
		klog.V(2).Infof("set node %v with NodeNetworkUnavailable=false was canceled because it is already set", node.Name)
		return nil
	}

	if !routesCreated && condition != nil && condition.Status == v1.ConditionTrue {
		klog.V(2).Infof("set node %v with NodeNetworkUnavailable=true was canceled because it is already set", node.Name)
		return nil
	}

	klog.Infof("Patching node status %v with %v previous condition was:%+v", node.Name, routesCreated, condition)

	// either condition is not there, or has a value != to what we need
	// start setting it
	err := clientretry.RetryOnConflict(updateNetworkConditionBackoff, func() error {
		var err error
		// Patch could also fail, even though the chance is very slim. So we still do
		// patch in the retry loop.
		currentTime := metav1.Now()
		if routesCreated {
			err = nodeutil.SetNodeCondition(rc.kubeClient, types.NodeName(node.Name), v1.NodeCondition{
				Type:               v1.NodeNetworkUnavailable,
				Status:             v1.ConditionFalse,
				Reason:             "RouteCreated",
				Message:            "RouteController created a route",
				LastTransitionTime: currentTime,
			})
		} else {
			err = nodeutil.SetNodeCondition(rc.kubeClient, types.NodeName(node.Name), v1.NodeCondition{
				Type:               v1.NodeNetworkUnavailable,
				Status:             v1.ConditionTrue,
				Reason:             "NoRouteCreated",
				Message:            "RouteController failed to create a route",
				LastTransitionTime: currentTime,
			})
		}
		if err != nil {
			klog.V(4).Infof("Error updating node %s, retrying: %v", types.NodeName(node.Name), err)
		}
		return err
	})

	if err != nil {
		klog.Errorf("Error updating node %s: %v", node.Name, err)
	}

	return err
}

func (rc *RouteController) isResponsibleForRoute(route *cloudprovider.Route) bool {
	_, cidr, err := netutils.ParseCIDRSloppy(route.DestinationCIDR)
	if err != nil {
		klog.Errorf("Ignoring route %s, unparsable CIDR: %v", route.Name, err)
		return false
	}
	// Not responsible if this route's CIDR is not within our clusterCIDR
	lastIP := make([]byte, len(cidr.IP))
	for i := range lastIP {
		lastIP[i] = cidr.IP[i] | ^cidr.Mask[i]
	}

	// check across all cluster cidrs
	for _, clusterCIDR := range rc.clusterCIDRs {
		if clusterCIDR.Contains(cidr.IP) || clusterCIDR.Contains(lastIP) {
			return true
		}
	}
	return false
}

// getRouteAction returns an action according to if there's a route matches a specific cidr and target Node addresses.
func getRouteAction(routes []*cloudprovider.Route, cidr string, nodeName types.NodeName, realNodeAddrs []v1.NodeAddress) routeAction {
	for _, route := range routes {
		if route.DestinationCIDR == cidr {
			if route.GatewayAddress != "" {
				if hasAddressValue(realNodeAddrs, route.GatewayAddress) {
					return keep
				}
			} else if !route.EnableNodeAddresses || equalNodeAddrs(realNodeAddrs, route.TargetNodeAddresses) {
				return keep
			}
			klog.Infof("Node addresses have changed to %v", realNodeAddrs)
			return update
		}
	}
	return add
}

func equalNodeAddrs(addrs0 []v1.NodeAddress, addrs1 []v1.NodeAddress) bool {
	if len(addrs0) != len(addrs1) {
		return false
	}
	for _, ip0 := range addrs0 {
		found := false
		for _, ip1 := range addrs1 {
			if reflect.DeepEqual(ip0, ip1) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func hasAddressValue(nodeAddresses []v1.NodeAddress, routeGatewayAddress string) bool {
	for _, value := range nodeAddresses {
		if value.Address == routeGatewayAddress {
			return true
		}
	}

	return false
}
