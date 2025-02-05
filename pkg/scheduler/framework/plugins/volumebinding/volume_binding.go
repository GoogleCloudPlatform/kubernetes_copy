/*
Copyright 2019 The Kubernetes Authors.

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

package volumebinding

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	corelisters "k8s.io/client-go/listers/core/v1"
	storagelisters "k8s.io/client-go/listers/storage/v1"
	"k8s.io/component-helpers/storage/ephemeral"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/apis/config"
	"k8s.io/kubernetes/pkg/scheduler/apis/config/validation"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/feature"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/helper"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/names"
	"k8s.io/kubernetes/pkg/scheduler/util"
)

const (
	stateKey framework.StateKey = Name

	maxUtilization = 100
)

// the state is initialized in PreFilter phase. because we save the pointer in
// framework.CycleState, in the later phases we don't need to call Write method
// to update the value
type stateData struct {
	allBound bool
	// podVolumesByNode holds the pod's volume information found in the Filter
	// phase for each node
	// it's initialized in the PreFilter phase
	podVolumesByNode map[string]*PodVolumes
	podVolumeClaims  *PodVolumeClaims
	// hasStaticBindings declares whether the pod contains one or more StaticBinding.
	// If not, vloumeBinding will skip score extension point.
	hasStaticBindings bool
	sync.Mutex
}

func (d *stateData) Clone() framework.StateData {
	return d
}

// VolumeBinding is a plugin that binds pod volumes in scheduling.
// In the Filter phase, pod binding cache is created for the pod and used in
// Reserve and PreBind phases.
type VolumeBinding struct {
	Binder      SchedulerVolumeBinder
	PVCLister   corelisters.PersistentVolumeClaimLister
	classLister storagelisters.StorageClassLister
	scorer      volumeCapacityScorer
	fts         feature.Features
}

var _ framework.PreFilterPlugin = &VolumeBinding{}
var _ framework.FilterPlugin = &VolumeBinding{}
var _ framework.ReservePlugin = &VolumeBinding{}
var _ framework.PreBindPlugin = &VolumeBinding{}
var _ framework.PreScorePlugin = &VolumeBinding{}
var _ framework.ScorePlugin = &VolumeBinding{}
var _ framework.EnqueueExtensions = &VolumeBinding{}

// Name is the name of the plugin used in Registry and configurations.
const Name = names.VolumeBinding

// Name returns name of the plugin. It is used in logs, etc.
func (pl *VolumeBinding) Name() string {
	return Name
}

// EventsToRegister returns the possible events that may make a Pod
// failed by this plugin schedulable.
func (pl *VolumeBinding) EventsToRegister(_ context.Context) ([]framework.ClusterEventWithHint, error) {
	// Pods may fail to find available PVs because the node labels do not
	// match the storage class's allowed topologies or PV's node affinity.
	// A new or updated node may make pods schedulable.
	//
	// A note about UpdateNodeTaint event:
	// Ideally, it's supposed to register only Add | UpdateNodeLabel because UpdateNodeTaint will never change the result from this plugin.
	// But, we may miss Node/Add event due to preCheck, and we decided to register UpdateNodeTaint | UpdateNodeLabel for all plugins registering Node/Add.
	// See: https://github.com/kubernetes/kubernetes/issues/109437
	nodeActionType := framework.Add | framework.UpdateNodeLabel | framework.UpdateNodeTaint
	if pl.fts.EnableSchedulingQueueHint {
		// When scheduling queue hint is enabled, we don't use the problematic preCheck and don't need to register UpdateNodeTaint event.
		nodeActionType = framework.Add | framework.UpdateNodeLabel
	}
	events := []framework.ClusterEventWithHint{
		// Pods may fail because of missing or mis-configured storage class
		// (e.g., allowedTopologies, volumeBindingMode), and hence may become
		// schedulable upon StorageClass Add or Update events.
		{Event: framework.ClusterEvent{Resource: framework.StorageClass, ActionType: framework.Add | framework.Update}, QueueingHintFn: pl.isSchedulableAfterStorageClassChange},

		// We bind PVCs with PVs, so any changes may make the pods schedulable.
		{Event: framework.ClusterEvent{Resource: framework.PersistentVolumeClaim, ActionType: framework.Add | framework.Update}, QueueingHintFn: pl.isSchedulableAfterPersistentVolumeClaimChange},
		{Event: framework.ClusterEvent{Resource: framework.PersistentVolume, ActionType: framework.Add | framework.Update}},

		{Event: framework.ClusterEvent{Resource: framework.Node, ActionType: nodeActionType}},

		// We rely on CSI node to translate in-tree PV to CSI.
		// TODO: kube-schduler will unregister the CSINode events once all the volume plugins has completed their CSI migration.
		{Event: framework.ClusterEvent{Resource: framework.CSINode, ActionType: framework.Add | framework.Update}, QueueingHintFn: pl.isSchedulableAfterCSINodeChange},

		// When CSIStorageCapacity is enabled, pods may become schedulable
		// on CSI driver & storage capacity changes.
		{Event: framework.ClusterEvent{Resource: framework.CSIDriver, ActionType: framework.Update}, QueueingHintFn: pl.isSchedulableAfterCSIDriverChange},
		{Event: framework.ClusterEvent{Resource: framework.CSIStorageCapacity, ActionType: framework.Add | framework.Update}, QueueingHintFn: pl.isSchedulableAfterCSIStorageCapacityChange},
	}
	return events, nil
}

func (pl *VolumeBinding) isSchedulableAfterCSINodeChange(logger klog.Logger, pod *v1.Pod, oldObj, newObj interface{}) (framework.QueueingHint, error) {
	if oldObj == nil {
		logger.V(5).Info("CSINode creation could make the pod schedulable")
		return framework.Queue, nil
	}
	oldCSINode, modifiedCSINode, err := util.As[*storagev1.CSINode](oldObj, newObj)
	if err != nil {
		return framework.Queue, err
	}

	logger = klog.LoggerWithValues(
		logger,
		"Pod", klog.KObj(pod),
		"CSINode", klog.KObj(modifiedCSINode),
	)

	if oldCSINode.ObjectMeta.Annotations[v1.MigratedPluginsAnnotationKey] != modifiedCSINode.ObjectMeta.Annotations[v1.MigratedPluginsAnnotationKey] {
		logger.V(5).Info("CSINode's migrated plugins annotation is updated and that may make the pod schedulable")
		return framework.Queue, nil
	}

	logger.V(5).Info("CISNode was created or updated but it doesn't make this pod schedulable")
	return framework.QueueSkip, nil
}

func (pl *VolumeBinding) isSchedulableAfterPersistentVolumeClaimChange(logger klog.Logger, pod *v1.Pod, oldObj, newObj interface{}) (framework.QueueingHint, error) {
	_, newPVC, err := util.As[*v1.PersistentVolumeClaim](oldObj, newObj)
	if err != nil {
		return framework.Queue, err
	}

	logger = klog.LoggerWithValues(
		logger,
		"Pod", klog.KObj(pod),
		"PersistentVolumeClaim", klog.KObj(newPVC),
	)

	if pod.Namespace != newPVC.Namespace {
		logger.V(5).Info("PersistentVolumeClaim was created or updated, but it doesn't make this pod schedulable because the PVC belongs to a different namespace")
		return framework.QueueSkip, nil
	}

	for _, vol := range pod.Spec.Volumes {
		var pvcName string
		switch {
		case vol.PersistentVolumeClaim != nil:
			pvcName = vol.PersistentVolumeClaim.ClaimName
		case vol.Ephemeral != nil:
			pvcName = ephemeral.VolumeClaimName(pod, &vol)
		default:
			continue
		}

		if pvcName == newPVC.Name {
			// Return Queue because, in this case,
			// all PVC creations and almost all PVC updates could make the Pod schedulable.
			logger.V(5).Info("PersistentVolumeClaim the pod requires was created or updated, potentially making the target Pod schedulable")
			return framework.Queue, nil
		}
	}

	logger.V(5).Info("PersistentVolumeClaim was created or updated, but it doesn't make this pod schedulable")
	return framework.QueueSkip, nil
}

// isSchedulableAfterStorageClassChange checks whether an StorageClass event might make a Pod schedulable or not.
// Any StorageClass addition and a StorageClass update to allowedTopologies
// might make a Pod schedulable.
// Note that an update to volume binding mode is not allowed and we don't have to consider while examining the update event.
func (pl *VolumeBinding) isSchedulableAfterStorageClassChange(logger klog.Logger, pod *v1.Pod, oldObj, newObj interface{}) (framework.QueueingHint, error) {
	oldSC, newSC, err := util.As[*storagev1.StorageClass](oldObj, newObj)
	if err != nil {
		return framework.Queue, err
	}

	logger = klog.LoggerWithValues(
		logger,
		"Pod", klog.KObj(pod),
		"StorageClass", klog.KObj(newSC),
	)

	if oldSC == nil {
		// No further filtering can be made for a creation event,
		// and we just always return Queue.
		logger.V(5).Info("A new StorageClass was created, which could make a Pod schedulable")
		return framework.Queue, nil
	}

	if !apiequality.Semantic.DeepEqual(newSC.AllowedTopologies, oldSC.AllowedTopologies) {
		logger.V(5).Info("StorageClass got an update in AllowedTopologies", "AllowedTopologies", newSC.AllowedTopologies)
		return framework.Queue, nil
	}

	logger.V(5).Info("StorageClass was updated, but it doesn't make this pod schedulable")
	return framework.QueueSkip, nil
}

// isSchedulableAfterCSIStorageCapacityChange checks whether a CSIStorageCapacity event
// might make a Pod schedulable or not.
// Any CSIStorageCapacity addition and a CSIStorageCapacity update to volume limit
// (calculated based on capacity and maximumVolumeSize) might make a Pod schedulable.
// Note that an update to nodeTopology and storageClassName is not allowed and
// we don't have to consider while examining the update event.
func (pl *VolumeBinding) isSchedulableAfterCSIStorageCapacityChange(logger klog.Logger, pod *v1.Pod, oldObj, newObj interface{}) (framework.QueueingHint, error) {
	oldCap, newCap, err := util.As[*storagev1.CSIStorageCapacity](oldObj, newObj)
	if err != nil {
		return framework.Queue, err
	}

	if oldCap == nil {
		logger.V(5).Info(
			"A new CSIStorageCapacity was created, which could make a Pod schedulable",
			"Pod", klog.KObj(pod),
			"CSIStorageCapacity", klog.KObj(newCap),
		)
		return framework.Queue, nil
	}

	oldLimit := volumeLimit(oldCap)
	newLimit := volumeLimit(newCap)

	logger = klog.LoggerWithValues(
		logger,
		"Pod", klog.KObj(pod),
		"CSIStorageCapacity", klog.KObj(newCap),
		"volumeLimit(new)", newLimit,
		"volumeLimit(old)", oldLimit,
	)

	if newLimit != nil && (oldLimit == nil || newLimit.Value() > oldLimit.Value()) {
		logger.V(5).Info("VolumeLimit was increased, which could make a Pod schedulable")
		return framework.Queue, nil
	}

	logger.V(5).Info("CSIStorageCapacity was updated, but it doesn't make this pod schedulable")
	return framework.QueueSkip, nil
}

func (pl *VolumeBinding) isSchedulableAfterCSIDriverChange(logger klog.Logger, pod *v1.Pod, oldObj, newObj interface{}) (framework.QueueingHint, error) {
	originalCSIDriver, modifiedCSIDriver, err := util.As[*storagev1.CSIDriver](oldObj, newObj)
	if err != nil {
		return framework.Queue, err
	}

	logger = klog.LoggerWithValues(
		logger,
		"Pod", klog.KObj(pod),
		"CSIDriver", klog.KObj(modifiedCSIDriver),
	)

	for _, vol := range pod.Spec.Volumes {
		if vol.CSI == nil || vol.CSI.Driver != modifiedCSIDriver.Name {
			continue
		}
		if (originalCSIDriver.Spec.StorageCapacity != nil && *originalCSIDriver.Spec.StorageCapacity) &&
			(modifiedCSIDriver.Spec.StorageCapacity == nil || !*modifiedCSIDriver.Spec.StorageCapacity) {
			logger.V(5).Info("CSIDriver was updated and storage capacity got disabled, which may make the pod schedulable")
			return framework.Queue, nil
		}
	}

	logger.V(5).Info("CSIDriver was created or updated but it doesn't make this pod schedulable")
	return framework.QueueSkip, nil
}

// podHasPVCs returns 2 values:
// - the first one to denote if the given "pod" has any PVC defined.
// - the second one to return any error if the requested PVC is illegal.
func (pl *VolumeBinding) podHasPVCs(pod *v1.Pod) (bool, error) {
	hasPVC := false
	for _, vol := range pod.Spec.Volumes {
		var pvcName string
		isEphemeral := false
		switch {
		case vol.PersistentVolumeClaim != nil:
			pvcName = vol.PersistentVolumeClaim.ClaimName
		case vol.Ephemeral != nil:
			pvcName = ephemeral.VolumeClaimName(pod, &vol)
			isEphemeral = true
		default:
			// Volume is not using a PVC, ignore
			continue
		}
		hasPVC = true
		pvc, err := pl.PVCLister.PersistentVolumeClaims(pod.Namespace).Get(pvcName)
		if err != nil {
			// The error usually has already enough context ("persistentvolumeclaim "myclaim" not found"),
			// but we can do better for generic ephemeral inline volumes where that situation
			// is normal directly after creating a pod.
			if isEphemeral && apierrors.IsNotFound(err) {
				err = fmt.Errorf("waiting for ephemeral volume controller to create the persistentvolumeclaim %q", pvcName)
			}
			return hasPVC, err
		}

		if pvc.Status.Phase == v1.ClaimLost {
			return hasPVC, fmt.Errorf("persistentvolumeclaim %q bound to non-existent persistentvolume %q", pvc.Name, pvc.Spec.VolumeName)
		}

		if pvc.DeletionTimestamp != nil {
			return hasPVC, fmt.Errorf("persistentvolumeclaim %q is being deleted", pvc.Name)
		}

		if isEphemeral {
			if err := ephemeral.VolumeIsForPod(pod, pvc); err != nil {
				return hasPVC, err
			}
		}
	}
	return hasPVC, nil
}

// PreFilter invoked at the prefilter extension point to check if pod has all
// immediate PVCs bound. If not all immediate PVCs are bound, an
// UnschedulableAndUnresolvable is returned.
func (pl *VolumeBinding) PreFilter(ctx context.Context, state *framework.CycleState, pod *v1.Pod) (*framework.PreFilterResult, *framework.Status) {
	logger := klog.FromContext(ctx)
	// If pod does not reference any PVC, we don't need to do anything.
	if hasPVC, err := pl.podHasPVCs(pod); err != nil {
		return nil, framework.NewStatus(framework.UnschedulableAndUnresolvable, err.Error())
	} else if !hasPVC {
		state.Write(stateKey, &stateData{})
		return nil, framework.NewStatus(framework.Skip)
	}
	podVolumeClaims, err := pl.Binder.GetPodVolumeClaims(logger, pod)
	if err != nil {
		return nil, framework.AsStatus(err)
	}
	if len(podVolumeClaims.unboundClaimsImmediate) > 0 {
		// Return UnschedulableAndUnresolvable error if immediate claims are
		// not bound. Pod will be moved to active/backoff queues once these
		// claims are bound by PV controller.
		status := framework.NewStatus(framework.UnschedulableAndUnresolvable)
		status.AppendReason("pod has unbound immediate PersistentVolumeClaims")
		return nil, status
	}
	state.Write(stateKey, &stateData{
		podVolumesByNode: make(map[string]*PodVolumes),
		podVolumeClaims: &PodVolumeClaims{
			boundClaims:                podVolumeClaims.boundClaims,
			unboundClaimsDelayBinding:  podVolumeClaims.unboundClaimsDelayBinding,
			unboundVolumesDelayBinding: podVolumeClaims.unboundVolumesDelayBinding,
		},
	})
	return nil, nil
}

// PreFilterExtensions returns prefilter extensions, pod add and remove.
func (pl *VolumeBinding) PreFilterExtensions() framework.PreFilterExtensions {
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
// It evaluates if a pod can fit due to the volumes it requests,
// for both bound and unbound PVCs.
//
// For PVCs that are bound, then it checks that the corresponding PV's node affinity is
// satisfied by the given node.
//
// For PVCs that are unbound, it tries to find available PVs that can satisfy the PVC requirements
// and that the PV node affinity is satisfied by the given node.
//
// If storage capacity tracking is enabled, then enough space has to be available
// for the node and volumes that still need to be created.
//
// The predicate returns true if all bound PVCs have compatible PVs with the node, and if all unbound
// PVCs can be matched with an available and node-compatible PV.
func (pl *VolumeBinding) Filter(ctx context.Context, cs *framework.CycleState, pod *v1.Pod, nodeInfo *framework.NodeInfo) *framework.Status {
	logger := klog.FromContext(ctx)
	node := nodeInfo.Node()

	state, err := getStateData(cs)
	if err != nil {
		return framework.AsStatus(err)
	}

	podVolumes, reasons, err := pl.Binder.FindPodVolumes(logger, pod, state.podVolumeClaims, node)

	if err != nil {
		return framework.AsStatus(err)
	}

	if len(reasons) > 0 {
		status := framework.NewStatus(framework.UnschedulableAndUnresolvable)
		for _, reason := range reasons {
			status.AppendReason(string(reason))
		}
		return status
	}

	// multiple goroutines call `Filter` on different nodes simultaneously and the `CycleState` may be duplicated, so we must use a local lock here
	state.Lock()
	state.podVolumesByNode[node.Name] = podVolumes
	state.hasStaticBindings = state.hasStaticBindings || (podVolumes != nil && len(podVolumes.StaticBindings) > 0)
	state.Unlock()
	return nil
}

// PreScore invoked at the preScore extension point. It checks whether volumeBinding can skip Score
func (pl *VolumeBinding) PreScore(ctx context.Context, cs *framework.CycleState, pod *v1.Pod, nodes []*framework.NodeInfo) *framework.Status {
	if pl.scorer == nil {
		return framework.NewStatus(framework.Skip)
	}
	state, err := getStateData(cs)
	if err != nil {
		return framework.AsStatus(err)
	}
	if state.hasStaticBindings || pl.fts.EnableStorageCapacityScoring {
		return nil
	}
	return framework.NewStatus(framework.Skip)
}

// Score invoked at the score extension point.
func (pl *VolumeBinding) Score(ctx context.Context, cs *framework.CycleState, pod *v1.Pod, nodeName string) (int64, *framework.Status) {
	if pl.scorer == nil {
		return 0, nil
	}
	state, err := getStateData(cs)
	if err != nil {
		return 0, framework.AsStatus(err)
	}
	podVolumes, ok := state.podVolumesByNode[nodeName]
	if !ok {
		return 0, nil
	}

	classResources := make(classResourceMap)
	if len(podVolumes.StaticBindings) != 0 || !pl.fts.EnableStorageCapacityScoring {
		// group static binding volumes by storage class
		for _, staticBinding := range podVolumes.StaticBindings {
			class := staticBinding.StorageClassName()
			storageResource := staticBinding.StorageResource()
			if _, ok := classResources[class]; !ok {
				classResources[class] = &StorageResource{
					Requested: 0,
					Capacity:  0,
				}
			}
			classResources[class].Requested += storageResource.Requested
			classResources[class].Capacity += storageResource.Capacity
		}
	} else {
		// group dynamic binding volumes by storage class
		for _, provision := range podVolumes.DynamicProvisions {
			if provision.Capacity == nil {
				continue
			}
			class := *provision.PVC.Spec.StorageClassName
			if _, ok := classResources[class]; !ok {
				classResources[class] = &StorageResource{
					Requested: 0,
					Capacity:  provision.Capacity.Capacity.Value(),
				}
			} else {
				classResources[class].Capacity = provision.Capacity.Capacity.Value()
			}
			requestedQty := provision.PVC.Spec.Resources.Requests[v1.ResourceName(v1.ResourceStorage)]
			classResources[class].Requested += requestedQty.Value()
		}
	}

	return pl.scorer(classResources), nil
}

// ScoreExtensions of the Score plugin.
func (pl *VolumeBinding) ScoreExtensions() framework.ScoreExtensions {
	return nil
}

// Reserve reserves volumes of pod and saves binding status in cycle state.
func (pl *VolumeBinding) Reserve(ctx context.Context, cs *framework.CycleState, pod *v1.Pod, nodeName string) *framework.Status {
	state, err := getStateData(cs)
	if err != nil {
		return framework.AsStatus(err)
	}
	// we don't need to hold the lock as only one node will be reserved for the given pod
	podVolumes, ok := state.podVolumesByNode[nodeName]
	if ok {
		allBound, err := pl.Binder.AssumePodVolumes(klog.FromContext(ctx), pod, nodeName, podVolumes)
		if err != nil {
			return framework.AsStatus(err)
		}
		state.allBound = allBound
	} else {
		// may not exist if the pod does not reference any PVC
		state.allBound = true
	}
	return nil
}

// PreBind will make the API update with the assumed bindings and wait until
// the PV controller has completely finished the binding operation.
//
// If binding errors, times out or gets undone, then an error will be returned to
// retry scheduling.
func (pl *VolumeBinding) PreBind(ctx context.Context, cs *framework.CycleState, pod *v1.Pod, nodeName string) *framework.Status {
	s, err := getStateData(cs)
	if err != nil {
		return framework.AsStatus(err)
	}
	if s.allBound {
		// no need to bind volumes
		return nil
	}
	// we don't need to hold the lock as only one node will be pre-bound for the given pod
	podVolumes, ok := s.podVolumesByNode[nodeName]
	if !ok {
		return framework.AsStatus(fmt.Errorf("no pod volumes found for node %q", nodeName))
	}
	logger := klog.FromContext(ctx)
	logger.V(5).Info("Trying to bind volumes for pod", "pod", klog.KObj(pod))
	err = pl.Binder.BindPodVolumes(ctx, pod, podVolumes)
	if err != nil {
		logger.V(5).Info("Failed to bind volumes for pod", "pod", klog.KObj(pod), "err", err)
		return framework.AsStatus(err)
	}
	logger.V(5).Info("Success binding volumes for pod", "pod", klog.KObj(pod))
	return nil
}

// Unreserve clears assumed PV and PVC cache.
// It's idempotent, and does nothing if no cache found for the given pod.
func (pl *VolumeBinding) Unreserve(ctx context.Context, cs *framework.CycleState, pod *v1.Pod, nodeName string) {
	s, err := getStateData(cs)
	if err != nil {
		return
	}
	// we don't need to hold the lock as only one node may be unreserved
	podVolumes, ok := s.podVolumesByNode[nodeName]
	if !ok {
		return
	}
	pl.Binder.RevertAssumedPodVolumes(podVolumes)
}

// New initializes a new plugin and returns it.
func New(ctx context.Context, plArgs runtime.Object, fh framework.Handle, fts feature.Features) (framework.Plugin, error) {
	args, ok := plArgs.(*config.VolumeBindingArgs)
	if !ok {
		return nil, fmt.Errorf("want args to be of type VolumeBindingArgs, got %T", plArgs)
	}
	if err := validation.ValidateVolumeBindingArgsWithOptions(nil, args, validation.VolumeBindingArgsValidationOptions{
		AllowVolumeCapacityPriority: fts.EnableVolumeCapacityPriority,
	}); err != nil {
		return nil, err
	}
	podInformer := fh.SharedInformerFactory().Core().V1().Pods()
	nodeInformer := fh.SharedInformerFactory().Core().V1().Nodes()
	pvcInformer := fh.SharedInformerFactory().Core().V1().PersistentVolumeClaims()
	pvInformer := fh.SharedInformerFactory().Core().V1().PersistentVolumes()
	storageClassInformer := fh.SharedInformerFactory().Storage().V1().StorageClasses()
	csiNodeInformer := fh.SharedInformerFactory().Storage().V1().CSINodes()
	capacityCheck := CapacityCheck{
		CSIDriverInformer:          fh.SharedInformerFactory().Storage().V1().CSIDrivers(),
		CSIStorageCapacityInformer: fh.SharedInformerFactory().Storage().V1().CSIStorageCapacities(),
	}
	binder := NewVolumeBinder(klog.FromContext(ctx), fh.ClientSet(), podInformer, nodeInformer, csiNodeInformer, pvcInformer, pvInformer, storageClassInformer, capacityCheck, time.Duration(args.BindTimeoutSeconds)*time.Second)

	// build score function
	var scorer volumeCapacityScorer
	if fts.EnableVolumeCapacityPriority {
		shape := make(helper.FunctionShape, 0, len(args.Shape))
		for _, point := range args.Shape {
			shape = append(shape, helper.FunctionShapePoint{
				Utilization: int64(point.Utilization),
				Score:       int64(point.Score) * (framework.MaxNodeScore / config.MaxCustomPriorityScore),
			})
		}
		scorer = buildScorerFunction(shape)
	}
	return &VolumeBinding{
		Binder:      binder,
		PVCLister:   pvcInformer.Lister(),
		classLister: storageClassInformer.Lister(),
		scorer:      scorer,
		fts:         fts,
	}, nil
}
