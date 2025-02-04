/*
Copyright 2022 The Kubernetes Authors.

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

package dynamicresources

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/google/go-cmp/cmp" //nolint:depguard

	v1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1beta1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
	"k8s.io/component-helpers/scheduling/corev1/nodeaffinity"
	"k8s.io/dynamic-resource-allocation/cel"
	"k8s.io/dynamic-resource-allocation/resourceclaim"
	"k8s.io/dynamic-resource-allocation/structured"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/feature"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/names"
	schedutil "k8s.io/kubernetes/pkg/scheduler/util"
)

const (
	// Name is the name of the plugin used in Registry and configurations.
	Name = names.DynamicResources

	stateKey framework.StateKey = Name
)

// The state is initialized in PreFilter phase. Because we save the pointer in
// framework.CycleState, in the later phases we don't need to call Write method
// to update the value
type stateData struct {
	// A copy of all claims for the Pod (i.e. 1:1 match with
	// pod.Spec.ResourceClaims), initially with the status from the start
	// of the scheduling cycle. Each claim instance is read-only because it
	// might come from the informer cache. The instances get replaced when
	// the plugin itself successfully does an Update.
	//
	// Empty if the Pod has no claims.
	claims []*resourceapi.ResourceClaim

	// Allocator handles claims with structured parameters.
	allocator *structured.Allocator

	// mutex must be locked while accessing any of the fields below.
	mutex sync.Mutex

	// The indices of all claims that:
	// - are allocated
	// - use delayed allocation or the builtin controller
	// - were not available on at least one node
	//
	// Set in parallel during Filter, so write access there must be
	// protected by the mutex. Used by PostFilter.
	unavailableClaims sets.Set[int]

	informationsForClaim []informationForClaim

	// nodeAllocations caches the result of Filter for the nodes.
	nodeAllocations map[string][]resourceapi.AllocationResult
}

func (d *stateData) Clone() framework.StateData {
	return d
}

type informationForClaim struct {
	// Node selector based on the claim status if allocated.
	availableOnNodes *nodeaffinity.NodeSelector

	// Set by Reserved, published by PreBind.
	allocation *resourceapi.AllocationResult
}

// DynamicResources is a plugin that ensures that ResourceClaims are allocated.
type DynamicResources struct {
	enabled                   bool
	enableAdminAccess         bool
	enablePrioritizedList     bool
	enableSchedulingQueueHint bool

	fh         framework.Handle
	clientset  kubernetes.Interface
	celCache   *cel.Cache
	draManager framework.SharedDRAManager
}

// New initializes a new plugin and returns it.
func New(ctx context.Context, plArgs runtime.Object, fh framework.Handle, fts feature.Features) (framework.Plugin, error) {
	if !fts.EnableDynamicResourceAllocation {
		// Disabled, won't do anything.
		return &DynamicResources{}, nil
	}

	pl := &DynamicResources{
		enabled:                   true,
		enableAdminAccess:         fts.EnableDRAAdminAccess,
		enablePrioritizedList:     fts.EnableDRAPrioritizedList,
		enableSchedulingQueueHint: fts.EnableSchedulingQueueHint,

		fh:        fh,
		clientset: fh.ClientSet(),
		// This is a LRU cache for compiled CEL expressions. The most
		// recent 10 of them get reused across different scheduling
		// cycles.
		celCache:   cel.NewCache(10),
		draManager: fh.SharedDRAManager(),
	}

	return pl, nil
}

var _ framework.PreEnqueuePlugin = &DynamicResources{}
var _ framework.PreFilterPlugin = &DynamicResources{}
var _ framework.FilterPlugin = &DynamicResources{}
var _ framework.PostFilterPlugin = &DynamicResources{}
var _ framework.ReservePlugin = &DynamicResources{}
var _ framework.EnqueueExtensions = &DynamicResources{}
var _ framework.PreBindPlugin = &DynamicResources{}

// Name returns name of the plugin. It is used in logs, etc.
func (pl *DynamicResources) Name() string {
	return Name
}

// EventsToRegister returns the possible events that may make a Pod
// failed by this plugin schedulable.
func (pl *DynamicResources) EventsToRegister(_ context.Context) ([]framework.ClusterEventWithHint, error) {
	if !pl.enabled {
		return nil, nil
	}
	// A resource might depend on node labels for topology filtering.
	// A new or updated node may make pods schedulable.
	//
	// A note about UpdateNodeTaint event:
	// Ideally, it's supposed to register only Add | UpdateNodeLabel because UpdateNodeTaint will never change the result from this plugin.
	// But, we may miss Node/Add event due to preCheck, and we decided to register UpdateNodeTaint | UpdateNodeLabel for all plugins registering Node/Add.
	// See: https://github.com/kubernetes/kubernetes/issues/109437
	nodeActionType := framework.Add | framework.UpdateNodeLabel | framework.UpdateNodeTaint
	if pl.enableSchedulingQueueHint {
		// When QHint is enabled, the problematic preCheck is already removed, and we can remove UpdateNodeTaint.
		nodeActionType = framework.Add | framework.UpdateNodeLabel
	}

	events := []framework.ClusterEventWithHint{
		{Event: framework.ClusterEvent{Resource: framework.Node, ActionType: nodeActionType}},
		// Allocation is tracked in ResourceClaims, so any changes may make the pods schedulable.
		{Event: framework.ClusterEvent{Resource: framework.ResourceClaim, ActionType: framework.Add | framework.Update}, QueueingHintFn: pl.isSchedulableAfterClaimChange},
		// Adding the ResourceClaim name to the pod status makes pods waiting for their ResourceClaim schedulable.
		{Event: framework.ClusterEvent{Resource: framework.Pod, ActionType: framework.UpdatePodGeneratedResourceClaim}, QueueingHintFn: pl.isSchedulableAfterPodChange},
		// A pod might be waiting for a class to get created or modified.
		{Event: framework.ClusterEvent{Resource: framework.DeviceClass, ActionType: framework.Add | framework.Update}},
		// Adding or updating a ResourceSlice might make a pod schedulable because new resources became available.
		{Event: framework.ClusterEvent{Resource: framework.ResourceSlice, ActionType: framework.Add | framework.Update}},
	}

	return events, nil
}

// PreEnqueue checks if there are known reasons why a pod currently cannot be
// scheduled. When this fails, one of the registered events can trigger another
// attempt.
func (pl *DynamicResources) PreEnqueue(ctx context.Context, pod *v1.Pod) (status *framework.Status) {
	if !pl.enabled {
		return nil
	}

	if err := pl.foreachPodResourceClaim(pod, nil); err != nil {
		return statusUnschedulable(klog.FromContext(ctx), err.Error())
	}
	return nil
}

// isSchedulableAfterClaimChange is invoked for add and update claim events reported by
// an informer. It checks whether that change made a previously unschedulable
// pod schedulable. It errs on the side of letting a pod scheduling attempt
// happen. The delete claim event will not invoke it, so newObj will never be nil.
func (pl *DynamicResources) isSchedulableAfterClaimChange(logger klog.Logger, pod *v1.Pod, oldObj, newObj interface{}) (framework.QueueingHint, error) {
	originalClaim, modifiedClaim, err := schedutil.As[*resourceapi.ResourceClaim](oldObj, newObj)
	if err != nil {
		// Shouldn't happen.
		return framework.Queue, fmt.Errorf("unexpected object in isSchedulableAfterClaimChange: %w", err)
	}

	usesClaim := false
	if err := pl.foreachPodResourceClaim(pod, func(_ string, claim *resourceapi.ResourceClaim) {
		if claim.UID == modifiedClaim.UID {
			usesClaim = true
		}
	}); err != nil {
		// This is not an unexpected error: we know that
		// foreachPodResourceClaim only returns errors for "not
		// schedulable".
		if loggerV := logger.V(6); loggerV.Enabled() {
			owner := metav1.GetControllerOf(modifiedClaim)
			loggerV.Info("pod is not schedulable after resource claim change", "pod", klog.KObj(pod), "claim", klog.KObj(modifiedClaim), "claimOwner", owner, "reason", err.Error())
		}
		return framework.QueueSkip, nil
	}

	if originalClaim != nil &&
		originalClaim.Status.Allocation != nil &&
		modifiedClaim.Status.Allocation == nil {
		// A claim with structured parameters was deallocated. This might have made
		// resources available for other pods.
		logger.V(6).Info("claim with structured parameters got deallocated", "pod", klog.KObj(pod), "claim", klog.KObj(modifiedClaim))
		return framework.Queue, nil
	}

	if !usesClaim {
		// This was not the claim the pod was waiting for.
		logger.V(6).Info("unrelated claim got modified", "pod", klog.KObj(pod), "claim", klog.KObj(modifiedClaim))
		return framework.QueueSkip, nil
	}

	if originalClaim == nil {
		logger.V(5).Info("claim for pod got created", "pod", klog.KObj(pod), "claim", klog.KObj(modifiedClaim))
		return framework.Queue, nil
	}

	// Modifications may or may not be relevant. If the entire
	// status is as before, then something else must have changed
	// and we don't care. What happens in practice is that the
	// resource driver adds the finalizer.
	if apiequality.Semantic.DeepEqual(&originalClaim.Status, &modifiedClaim.Status) {
		if loggerV := logger.V(7); loggerV.Enabled() {
			// Log more information.
			loggerV.Info("claim for pod got modified where the pod doesn't care", "pod", klog.KObj(pod), "claim", klog.KObj(modifiedClaim), "diff", cmp.Diff(originalClaim, modifiedClaim))
		} else {
			logger.V(6).Info("claim for pod got modified where the pod doesn't care", "pod", klog.KObj(pod), "claim", klog.KObj(modifiedClaim))
		}
		return framework.QueueSkip, nil
	}

	logger.V(5).Info("status of claim for pod got updated", "pod", klog.KObj(pod), "claim", klog.KObj(modifiedClaim))
	return framework.Queue, nil
}

// isSchedulableAfterPodChange is invoked for update pod events reported by
// an informer. It checks whether that change adds the ResourceClaim(s) that the
// pod has been waiting for.
func (pl *DynamicResources) isSchedulableAfterPodChange(logger klog.Logger, pod *v1.Pod, oldObj, newObj interface{}) (framework.QueueingHint, error) {
	_, modifiedPod, err := schedutil.As[*v1.Pod](nil, newObj)
	if err != nil {
		// Shouldn't happen.
		return framework.Queue, fmt.Errorf("unexpected object in isSchedulableAfterClaimChange: %w", err)
	}

	if pod.UID != modifiedPod.UID {
		logger.V(7).Info("pod is not schedulable after change in other pod", "pod", klog.KObj(pod), "modifiedPod", klog.KObj(modifiedPod))
		return framework.QueueSkip, nil
	}

	if err := pl.foreachPodResourceClaim(modifiedPod, nil); err != nil {
		// This is not an unexpected error: we know that
		// foreachPodResourceClaim only returns errors for "not
		// schedulable".
		logger.V(6).Info("pod is not schedulable after being updated", "pod", klog.KObj(pod))
		return framework.QueueSkip, nil
	}

	logger.V(5).Info("pod got updated and is schedulable", "pod", klog.KObj(pod))
	return framework.Queue, nil
}

// podResourceClaims returns the ResourceClaims for all pod.Spec.PodResourceClaims.
func (pl *DynamicResources) podResourceClaims(pod *v1.Pod) ([]*resourceapi.ResourceClaim, error) {
	claims := make([]*resourceapi.ResourceClaim, 0, len(pod.Spec.ResourceClaims))
	if err := pl.foreachPodResourceClaim(pod, func(_ string, claim *resourceapi.ResourceClaim) {
		// We store the pointer as returned by the lister. The
		// assumption is that if a claim gets modified while our code
		// runs, the cache will store a new pointer, not mutate the
		// existing object that we point to here.
		claims = append(claims, claim)
	}); err != nil {
		return nil, err
	}
	return claims, nil
}

// foreachPodResourceClaim checks that each ResourceClaim for the pod exists.
// It calls an optional handler for those claims that it finds.
func (pl *DynamicResources) foreachPodResourceClaim(pod *v1.Pod, cb func(podResourceName string, claim *resourceapi.ResourceClaim)) error {
	for _, resource := range pod.Spec.ResourceClaims {
		claimName, mustCheckOwner, err := resourceclaim.Name(pod, &resource)
		if err != nil {
			return err
		}
		// The claim name might be nil if no underlying resource claim
		// was generated for the referenced claim. There are valid use
		// cases when this might happen, so we simply skip it.
		if claimName == nil {
			continue
		}
		claim, err := pl.draManager.ResourceClaims().Get(pod.Namespace, *claimName)
		if err != nil {
			return err
		}

		if claim.DeletionTimestamp != nil {
			return fmt.Errorf("resourceclaim %q is being deleted", claim.Name)
		}

		if mustCheckOwner {
			if err := resourceclaim.IsForPod(pod, claim); err != nil {
				return err
			}
		}
		if cb != nil {
			cb(resource.Name, claim)
		}
	}
	return nil
}

// PreFilter invoked at the prefilter extension point to check if pod has all
// immediate claims bound. UnschedulableAndUnresolvable is returned if
// the pod cannot be scheduled at the moment on any node.
func (pl *DynamicResources) PreFilter(ctx context.Context, state *framework.CycleState, pod *v1.Pod) (*framework.PreFilterResult, *framework.Status) {
	if !pl.enabled {
		return nil, framework.NewStatus(framework.Skip)
	}
	logger := klog.FromContext(ctx)

	// If the pod does not reference any claim, we don't need to do
	// anything for it. We just initialize an empty state to record that
	// observation for the other functions. This gets updated below
	// if we get that far.
	s := &stateData{}
	state.Write(stateKey, s)

	claims, err := pl.podResourceClaims(pod)
	if err != nil {
		return nil, statusUnschedulable(logger, err.Error())
	}
	logger.V(5).Info("pod resource claims", "pod", klog.KObj(pod), "resourceclaims", klog.KObjSlice(claims))

	// If the pod does not reference any claim,
	// DynamicResources Filter has nothing to do with the Pod.
	if len(claims) == 0 {
		return nil, framework.NewStatus(framework.Skip)
	}

	// All claims which the scheduler needs to allocate itself.
	allocateClaims := make([]*resourceapi.ResourceClaim, 0, len(claims))

	s.informationsForClaim = make([]informationForClaim, len(claims))
	for index, claim := range claims {
		if claim.Status.Allocation != nil &&
			!resourceclaim.CanBeReserved(claim) &&
			!resourceclaim.IsReservedForPod(pod, claim) {
			// Resource is in use. The pod has to wait.
			return nil, statusUnschedulable(logger, "resourceclaim in use", "pod", klog.KObj(pod), "resourceclaim", klog.KObj(claim))
		}

		if claim.Status.Allocation != nil {
			if claim.Status.Allocation.NodeSelector != nil {
				nodeSelector, err := nodeaffinity.NewNodeSelector(claim.Status.Allocation.NodeSelector)
				if err != nil {
					return nil, statusError(logger, err)
				}
				s.informationsForClaim[index].availableOnNodes = nodeSelector
			}
		} else {
			allocateClaims = append(allocateClaims, claim)

			// Allocation in flight? Better wait for that
			// to finish, see inFlightAllocations
			// documentation for details.
			if pl.draManager.ResourceClaims().ClaimHasPendingAllocation(claim.UID) {
				return nil, statusUnschedulable(logger, fmt.Sprintf("resource claim %s is in the process of being allocated", klog.KObj(claim)))
			}

			// Check all requests and device classes. If a class
			// does not exist, scheduling cannot proceed, no matter
			// how the claim is being allocated.
			//
			// When using a control plane controller, a class might
			// have a node filter. This is useful for trimming the
			// initial set of potential nodes before we ask the
			// driver(s) for information about the specific pod.
			for _, request := range claim.Spec.Devices.Requests {
				// The requirements differ depending on whether the request has a list of
				// alternative subrequests defined in the firstAvailable field.
				if len(request.FirstAvailable) == 0 {
					if status := pl.validateDeviceClass(logger, request.DeviceClassName, request.Name); status != nil {
						return nil, status
					}
				} else {
					for _, subRequest := range request.FirstAvailable {
						qualRequestName := strings.Join([]string{request.Name, subRequest.Name}, "/")
						if status := pl.validateDeviceClass(logger, subRequest.DeviceClassName, qualRequestName); status != nil {
							return nil, status
						}
					}
				}

			}
		}
	}

	if len(allocateClaims) > 0 {
		logger.V(5).Info("Preparing allocation with structured parameters", "pod", klog.KObj(pod), "resourceclaims", klog.KObjSlice(allocateClaims))

		// Doing this over and over again for each pod could be avoided
		// by setting the allocator up once and then keeping it up-to-date
		// as changes are observed.
		//
		// But that would cause problems for using the plugin in the
		// Cluster Autoscaler. If this step here turns out to be
		// expensive, we may have to maintain and update state more
		// persistently.
		//
		// Claims (and thus their devices) are treated as "allocated" if they are in the assume cache
		// or currently their allocation is in-flight. This does not change
		// during filtering, so we can determine that once.
		allAllocatedDevices, err := pl.draManager.ResourceClaims().ListAllAllocatedDevices()
		if err != nil {
			return nil, statusError(logger, err)
		}
		slices, err := pl.draManager.ResourceSlices().List()
		if err != nil {
			return nil, statusError(logger, err)
		}
		allocator, err := structured.NewAllocator(ctx, pl.enableAdminAccess, pl.enablePrioritizedList, allocateClaims, allAllocatedDevices, pl.draManager.DeviceClasses(), slices, pl.celCache)
		if err != nil {
			return nil, statusError(logger, err)
		}
		s.allocator = allocator
		s.nodeAllocations = make(map[string][]resourceapi.AllocationResult)
	}

	s.claims = claims
	return nil, nil
}

func (pl *DynamicResources) validateDeviceClass(logger klog.Logger, deviceClassName, requestName string) *framework.Status {
	if deviceClassName == "" {
		return statusError(logger, fmt.Errorf("request %s: unsupported request type", requestName))
	}

	_, err := pl.draManager.DeviceClasses().Get(deviceClassName)
	if err != nil {
		// If the class cannot be retrieved, allocation cannot proceed.
		if apierrors.IsNotFound(err) {
			// Here we mark the pod as "unschedulable", so it'll sleep in
			// the unscheduleable queue until a DeviceClass event occurs.
			return statusUnschedulable(logger, fmt.Sprintf("request %s: device class %s does not exist", requestName, deviceClassName))
		}
	}
	return nil
}

// PreFilterExtensions returns prefilter extensions, pod add and remove.
func (pl *DynamicResources) PreFilterExtensions() framework.PreFilterExtensions {
	return nil
}

func getStateData(cs *framework.CycleState) (*stateData, error) {
	state, err := cs.Read(stateKey)
	if err != nil {
		return nil, err
	}
	s, ok := state.(*stateData)
	if !ok {
		return nil, errors.New("unable to convert state into stateData")
	}
	return s, nil
}

// Filter invoked at the filter extension point.
// It evaluates if a pod can fit due to the resources it requests,
// for both allocated and unallocated claims.
//
// For claims that are bound, then it checks that the node affinity is
// satisfied by the given node.
//
// For claims that are unbound, it checks whether the claim might get allocated
// for the node.
func (pl *DynamicResources) Filter(ctx context.Context, cs *framework.CycleState, pod *v1.Pod, nodeInfo *framework.NodeInfo) *framework.Status {
	if !pl.enabled {
		return nil
	}
	state, err := getStateData(cs)
	if err != nil {
		return statusError(klog.FromContext(ctx), err)
	}
	if len(state.claims) == 0 {
		return nil
	}

	logger := klog.FromContext(ctx)
	node := nodeInfo.Node()

	var unavailableClaims []int
	for index, claim := range state.claims {
		logger.V(10).Info("filtering based on resource claims of the pod", "pod", klog.KObj(pod), "node", klog.KObj(node), "resourceclaim", klog.KObj(claim))

		// This node selector only gets set if the claim is allocated.
		if nodeSelector := state.informationsForClaim[index].availableOnNodes; nodeSelector != nil && !nodeSelector.Match(node) {
			logger.V(5).Info("allocation's node selector does not match", "pod", klog.KObj(pod), "node", klog.KObj(node), "resourceclaim", klog.KObj(claim))
			unavailableClaims = append(unavailableClaims, index)
		}
	}

	// Use allocator to check the node and cache the result in case that the node is picked.
	var allocations []resourceapi.AllocationResult
	if state.allocator != nil {
		allocCtx := ctx
		if loggerV := logger.V(5); loggerV.Enabled() {
			allocCtx = klog.NewContext(allocCtx, klog.LoggerWithValues(logger, "node", klog.KObj(node)))
		}

		a, err := state.allocator.Allocate(allocCtx, node)
		if err != nil {
			// This should only fail if there is something wrong with the claim or class.
			// Return an error to abort scheduling of it.
			//
			// This will cause retries. It would be slightly nicer to mark it as unschedulable
			// *and* abort scheduling. Then only cluster event for updating the claim or class
			// with the broken CEL expression would trigger rescheduling.
			//
			// But we cannot do both. As this shouldn't occur often, aborting like this is
			// better than the more complicated alternative (return Unschedulable here, remember
			// the error, then later raise it again later if needed).
			return statusError(logger, err, "pod", klog.KObj(pod), "node", klog.KObj(node), "resourceclaims", klog.KObjSlice(state.allocator.ClaimsToAllocate()))
		}
		// Check for exact length just to be sure. In practice this is all-or-nothing.
		if len(a) != len(state.allocator.ClaimsToAllocate()) {
			return statusUnschedulable(logger, "cannot allocate all claims", "pod", klog.KObj(pod), "node", klog.KObj(node), "resourceclaims", klog.KObjSlice(state.allocator.ClaimsToAllocate()))
		}
		// Reserve uses this information.
		allocations = a
	}

	// Store information in state while holding the mutex.
	if state.allocator != nil || len(unavailableClaims) > 0 {
		state.mutex.Lock()
		defer state.mutex.Unlock()
	}

	if len(unavailableClaims) > 0 {
		// Remember all unavailable claims. This might be observed
		// concurrently, so we have to lock the state before writing.

		if state.unavailableClaims == nil {
			state.unavailableClaims = sets.New[int]()
		}

		for _, index := range unavailableClaims {
			state.unavailableClaims.Insert(index)
		}
		return statusUnschedulable(logger, "resourceclaim not available on the node", "pod", klog.KObj(pod))
	}

	if state.allocator != nil {
		state.nodeAllocations[node.Name] = allocations
	}

	return nil
}

// PostFilter checks whether there are allocated claims that could get
// deallocated to help get the Pod schedulable. If yes, it picks one and
// requests its deallocation.  This only gets called when filtering found no
// suitable node.
func (pl *DynamicResources) PostFilter(ctx context.Context, cs *framework.CycleState, pod *v1.Pod, filteredNodeStatusMap framework.NodeToStatusReader) (*framework.PostFilterResult, *framework.Status) {
	if !pl.enabled {
		return nil, framework.NewStatus(framework.Unschedulable, "plugin disabled")
	}
	// If a Pod doesn't have any resource claims attached to it, there is no need for further processing.
	// Thus we provide a fast path for this case to avoid unnecessary computations.
	if len(pod.Spec.ResourceClaims) == 0 {
		return nil, framework.NewStatus(framework.Unschedulable)
	}
	logger := klog.FromContext(ctx)
	state, err := getStateData(cs)
	if err != nil {
		return nil, statusError(logger, err)
	}
	if len(state.claims) == 0 {
		return nil, framework.NewStatus(framework.Unschedulable, "no new claims to deallocate")
	}

	// Iterating over a map is random. This is intentional here, we want to
	// pick one claim randomly because there is no better heuristic.
	for index := range state.unavailableClaims {
		claim := state.claims[index]
		if len(claim.Status.ReservedFor) == 0 ||
			len(claim.Status.ReservedFor) == 1 && claim.Status.ReservedFor[0].UID == pod.UID {
			claim := claim.DeepCopy()
			claim.Status.ReservedFor = nil
			claim.Status.Allocation = nil
			logger.V(5).Info("Deallocation of ResourceClaim", "pod", klog.KObj(pod), "resourceclaim", klog.KObj(claim))
			if _, err := pl.clientset.ResourceV1beta1().ResourceClaims(claim.Namespace).UpdateStatus(ctx, claim, metav1.UpdateOptions{}); err != nil {
				return nil, statusError(logger, err)
			}
			return nil, framework.NewStatus(framework.Unschedulable, "deallocation of ResourceClaim completed")
		}
	}
	return nil, framework.NewStatus(framework.Unschedulable, "still not schedulable")
}

// Reserve reserves claims for the pod.
func (pl *DynamicResources) Reserve(ctx context.Context, cs *framework.CycleState, pod *v1.Pod, nodeName string) (status *framework.Status) {
	if !pl.enabled {
		return nil
	}
	state, err := getStateData(cs)
	if err != nil {
		return statusError(klog.FromContext(ctx), err)
	}
	if len(state.claims) == 0 {
		return nil
	}

	logger := klog.FromContext(ctx)

	numClaimsWithAllocator := 0
	for _, claim := range state.claims {
		if claim.Status.Allocation != nil {
			// Allocated, but perhaps not reserved yet. We checked in PreFilter that
			// the pod could reserve the claim. Instead of reserving here by
			// updating the ResourceClaim status, we assume that reserving
			// will work and only do it for real during binding. If it fails at
			// that time, some other pod was faster and we have to try again.
			continue
		}

		numClaimsWithAllocator++
	}

	if numClaimsWithAllocator == 0 {
		// Nothing left to do.
		return nil
	}

	// Prepare allocation of claims handled by the schedulder.
	if state.allocator != nil {
		// Entries in these two slices match each other.
		claimsToAllocate := state.allocator.ClaimsToAllocate()
		allocations, ok := state.nodeAllocations[nodeName]
		if !ok {
			// We checked before that the node is suitable. This shouldn't have failed,
			// so treat this as an error.
			return statusError(logger, errors.New("claim allocation not found for node"))
		}

		// Sanity check: do we have results for all pending claims?
		if len(allocations) != len(claimsToAllocate) ||
			len(allocations) != numClaimsWithAllocator {
			return statusError(logger, fmt.Errorf("internal error, have %d allocations, %d claims to allocate, want %d claims", len(allocations), len(claimsToAllocate), numClaimsWithAllocator))
		}

		for i, claim := range claimsToAllocate {
			index := slices.Index(state.claims, claim)
			if index < 0 {
				return statusError(logger, fmt.Errorf("internal error, claim %s with allocation not found", claim.Name))
			}
			allocation := &allocations[i]
			state.informationsForClaim[index].allocation = allocation

			// Strictly speaking, we don't need to store the full modified object.
			// The allocation would be enough. The full object is useful for
			// debugging, testing and the allocator, so let's make it realistic.
			claim = claim.DeepCopy()
			if !slices.Contains(claim.Finalizers, resourceapi.Finalizer) {
				claim.Finalizers = append(claim.Finalizers, resourceapi.Finalizer)
			}
			claim.Status.Allocation = allocation
			err := pl.draManager.ResourceClaims().SignalClaimPendingAllocation(claim.UID, claim)
			if err != nil {
				return statusError(logger, fmt.Errorf("internal error, couldn't signal allocation for claim %s", claim.Name))
			}
			logger.V(5).Info("Reserved resource in allocation result", "claim", klog.KObj(claim), "allocation", klog.Format(allocation))
		}
	}

	return nil
}

// Unreserve clears the ReservedFor field for all claims.
// It's idempotent, and does nothing if no state found for the given pod.
func (pl *DynamicResources) Unreserve(ctx context.Context, cs *framework.CycleState, pod *v1.Pod, nodeName string) {
	if !pl.enabled {
		return
	}
	state, err := getStateData(cs)
	if err != nil {
		return
	}
	if len(state.claims) == 0 {
		return
	}

	logger := klog.FromContext(ctx)

	for index, claim := range state.claims {
		// If allocation was in-flight, then it's not anymore and we need to revert the
		// claim object in the assume cache to what it was before.
		if deleted := pl.draManager.ResourceClaims().RemoveClaimPendingAllocation(state.claims[index].UID); deleted {
			pl.draManager.ResourceClaims().AssumedClaimRestore(claim.Namespace, claim.Name)
		}

		if claim.Status.Allocation != nil &&
			resourceclaim.IsReservedForPod(pod, claim) {
			// Remove pod from ReservedFor. A strategic-merge-patch is used
			// because that allows removing an individual entry without having
			// the latest slice.
			patch := fmt.Sprintf(`{"metadata": {"uid": %q}, "status": { "reservedFor": [ {"$patch": "delete", "uid": %q} ] }}`,
				claim.UID,
				pod.UID,
			)
			logger.V(5).Info("unreserve", "resourceclaim", klog.KObj(claim), "pod", klog.KObj(pod))
			claim, err := pl.clientset.ResourceV1beta1().ResourceClaims(claim.Namespace).Patch(ctx, claim.Name, types.StrategicMergePatchType, []byte(patch), metav1.PatchOptions{}, "status")
			if err != nil {
				// We will get here again when pod scheduling is retried.
				logger.Error(err, "unreserve", "resourceclaim", klog.KObj(claim))
			}
		}
	}
}

// PreBind gets called in a separate goroutine after it has been determined
// that the pod should get bound to this node. Because Reserve did not actually
// reserve claims, we need to do it now. For claims with the builtin controller,
// we also handle the allocation.
//
// If anything fails, we return an error and
// the pod will have to go into the backoff queue. The scheduler will call
// Unreserve as part of the error handling.
func (pl *DynamicResources) PreBind(ctx context.Context, cs *framework.CycleState, pod *v1.Pod, nodeName string) *framework.Status {
	if !pl.enabled {
		return nil
	}
	state, err := getStateData(cs)
	if err != nil {
		return statusError(klog.FromContext(ctx), err)
	}
	if len(state.claims) == 0 {
		return nil
	}

	logger := klog.FromContext(ctx)

	for index, claim := range state.claims {
		if !resourceclaim.IsReservedForPod(pod, claim) {
			claim, err := pl.bindClaim(ctx, state, index, pod, nodeName)
			if err != nil {
				return statusError(logger, err)
			}
			state.claims[index] = claim
		}
	}
	// If we get here, we know that reserving the claim for
	// the pod worked and we can proceed with binding it.
	return nil
}

// bindClaim gets called by PreBind for claim which is not reserved for the pod yet.
// It might not even be allocated. bindClaim then ensures that the allocation
// and reservation are recorded. This finishes the work started in Reserve.
func (pl *DynamicResources) bindClaim(ctx context.Context, state *stateData, index int, pod *v1.Pod, nodeName string) (patchedClaim *resourceapi.ResourceClaim, finalErr error) {
	logger := klog.FromContext(ctx)
	claim := state.claims[index].DeepCopy()
	allocation := state.informationsForClaim[index].allocation
	defer func() {
		if allocation != nil {
			// The scheduler was handling allocation. Now that has
			// completed, either successfully or with a failure.
			if finalErr == nil {
				// This can fail, but only for reasons that are okay (concurrent delete or update).
				// Shouldn't happen in this case.
				if err := pl.draManager.ResourceClaims().AssumeClaimAfterAPICall(claim); err != nil {
					logger.V(5).Info("Claim not stored in assume cache", "err", finalErr)
				}
			}
			pl.draManager.ResourceClaims().RemoveClaimPendingAllocation(claim.UID)
		}
	}()

	logger.V(5).Info("preparing claim status update", "claim", klog.KObj(state.claims[index]), "allocation", klog.Format(allocation))

	// We may run into a ResourceVersion conflict because there may be some
	// benign concurrent changes. In that case we get the latest claim and
	// try again.
	refreshClaim := false
	retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if refreshClaim {
			updatedClaim, err := pl.clientset.ResourceV1beta1().ResourceClaims(claim.Namespace).Get(ctx, claim.Name, metav1.GetOptions{})
			if err != nil {
				return fmt.Errorf("get updated claim %s after conflict: %w", klog.KObj(claim), err)
			}
			logger.V(5).Info("retrying update after conflict", "claim", klog.KObj(claim))
			claim = updatedClaim
		} else {
			// All future retries must get a new claim first.
			refreshClaim = true
		}

		if claim.DeletionTimestamp != nil {
			return fmt.Errorf("claim %s got deleted in the meantime", klog.KObj(claim))
		}

		// Do we need to store an allocation result from Reserve?
		if allocation != nil {
			if claim.Status.Allocation != nil {
				return fmt.Errorf("claim %s got allocated elsewhere in the meantime", klog.KObj(claim))
			}

			// The finalizer needs to be added in a normal update.
			// If we were interrupted in the past, it might already be set and we simply continue.
			if !slices.Contains(claim.Finalizers, resourceapi.Finalizer) {
				claim.Finalizers = append(claim.Finalizers, resourceapi.Finalizer)
				updatedClaim, err := pl.clientset.ResourceV1beta1().ResourceClaims(claim.Namespace).Update(ctx, claim, metav1.UpdateOptions{})
				if err != nil {
					return fmt.Errorf("add finalizer to claim %s: %w", klog.KObj(claim), err)
				}
				claim = updatedClaim
			}
			claim.Status.Allocation = allocation
		}

		// We can simply try to add the pod here without checking
		// preconditions. The apiserver will tell us with a
		// non-conflict error if this isn't possible.
		claim.Status.ReservedFor = append(claim.Status.ReservedFor, resourceapi.ResourceClaimConsumerReference{Resource: "pods", Name: pod.Name, UID: pod.UID})
		updatedClaim, err := pl.clientset.ResourceV1beta1().ResourceClaims(claim.Namespace).UpdateStatus(ctx, claim, metav1.UpdateOptions{})
		if err != nil {
			if allocation != nil {
				return fmt.Errorf("add allocation and reservation to claim %s: %w", klog.KObj(claim), err)
			}
			return fmt.Errorf("add reservation to claim %s: %w", klog.KObj(claim), err)
		}
		claim = updatedClaim
		return nil
	})

	if retryErr != nil {
		return nil, retryErr
	}

	logger.V(5).Info("reserved", "pod", klog.KObj(pod), "node", klog.ObjectRef{Name: nodeName}, "resourceclaim", klog.Format(claim))
	return claim, nil
}

// statusUnschedulable ensures that there is a log message associated with the
// line where the status originated.
func statusUnschedulable(logger klog.Logger, reason string, kv ...interface{}) *framework.Status {
	if loggerV := logger.V(5); loggerV.Enabled() {
		helper, loggerV := loggerV.WithCallStackHelper()
		helper()
		kv = append(kv, "reason", reason)
		// nolint: logcheck // warns because it cannot check key/values
		loggerV.Info("pod unschedulable", kv...)
	}
	return framework.NewStatus(framework.UnschedulableAndUnresolvable, reason)
}

// statusError ensures that there is a log message associated with the
// line where the error originated.
func statusError(logger klog.Logger, err error, kv ...interface{}) *framework.Status {
	if loggerV := logger.V(5); loggerV.Enabled() {
		helper, loggerV := loggerV.WithCallStackHelper()
		helper()
		// nolint: logcheck // warns because it cannot check key/values
		loggerV.Error(err, "dynamic resource plugin failed", kv...)
	}
	return framework.AsStatus(err)
}
