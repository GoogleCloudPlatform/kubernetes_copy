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

package volumerestrictions

import (
	"context"
	"fmt"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"
	v1helper "k8s.io/kubernetes/pkg/apis/core/v1/helper"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/feature"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/names"
	"k8s.io/kubernetes/pkg/scheduler/util"
)

// VolumeRestrictions is a plugin that checks volume restrictions.
type VolumeRestrictions struct {
	pvcLister                 corelisters.PersistentVolumeClaimLister
	sharedLister              framework.SharedLister
	enableSchedulingQueueHint bool
}

var _ framework.PreFilterPlugin = &VolumeRestrictions{}
var _ framework.FilterPlugin = &VolumeRestrictions{}
var _ framework.EnqueueExtensions = &VolumeRestrictions{}
var _ framework.StateData = &preFilterState{}

const (
	// Name is the name of the plugin used in the plugin registry and configurations.
	Name = names.VolumeRestrictions
	// preFilterStateKey is the key in CycleState to VolumeRestrictions pre-computed data for Filtering.
	// Using the name of the plugin will likely help us avoid collisions with other plugins.
	preFilterStateKey = "PreFilter" + Name

	// ErrReasonDiskConflict is used for NoDiskConflict predicate error.
	ErrReasonDiskConflict = "node(s) had no available disk"
	// ErrReasonReadWriteOncePodConflict is used when a PVC with ReadWriteOncePod access mode is already in-use by another pod.
	ErrReasonReadWriteOncePodConflict = "node(s) unavailable due to PersistentVolumeClaim with ReadWriteOncePod access mode already in-use by another pod"
)

// preFilterState computed at PreFilter and used at Filter.
type preFilterState struct {
	// Names of the pod's volumes using the ReadWriteOncePod access mode.
	readWriteOncePodPVCs sets.Set[string]
	// The number of references to these ReadWriteOncePod volumes by scheduled pods.
	conflictingPVCRefCount int
}

func (s *preFilterState) updateWithPod(podInfo *framework.PodInfo, multiplier int) {
	s.conflictingPVCRefCount += multiplier * s.conflictingPVCRefCountForPod(podInfo)
}

func (s *preFilterState) conflictingPVCRefCountForPod(podInfo *framework.PodInfo) int {
	conflicts := 0
	for _, volume := range podInfo.Pod.Spec.Volumes {
		if volume.PersistentVolumeClaim == nil {
			continue
		}
		if s.readWriteOncePodPVCs.Has(volume.PersistentVolumeClaim.ClaimName) {
			conflicts += 1
		}
	}
	return conflicts
}

// Clone the prefilter state.
func (s *preFilterState) Clone() framework.StateData {
	if s == nil {
		return nil
	}
	return &preFilterState{
		readWriteOncePodPVCs:   s.readWriteOncePodPVCs,
		conflictingPVCRefCount: s.conflictingPVCRefCount,
	}
}

// Name returns name of the plugin. It is used in logs, etc.
func (pl *VolumeRestrictions) Name() string {
	return Name
}

func isVolumeConflict(volume *v1.Volume, pod *v1.Pod) bool {
	for _, existingVolume := range pod.Spec.Volumes {
		// Same GCE disk mounted by multiple pods conflicts unless all pods mount it read-only.
		if volume.GCEPersistentDisk != nil && existingVolume.GCEPersistentDisk != nil {
			disk, existingDisk := volume.GCEPersistentDisk, existingVolume.GCEPersistentDisk
			if disk.PDName == existingDisk.PDName && !(disk.ReadOnly && existingDisk.ReadOnly) {
				return true
			}
		}

		if volume.AWSElasticBlockStore != nil && existingVolume.AWSElasticBlockStore != nil {
			if volume.AWSElasticBlockStore.VolumeID == existingVolume.AWSElasticBlockStore.VolumeID {
				return true
			}
		}

		if volume.ISCSI != nil && existingVolume.ISCSI != nil {
			iqn := volume.ISCSI.IQN
			eiqn := existingVolume.ISCSI.IQN
			// two ISCSI volumes are same, if they share the same iqn. As iscsi volumes are of type
			// RWO or ROX, we could permit only one RW mount. Same iscsi volume mounted by multiple Pods
			// conflict unless all other pods mount as read only.
			if iqn == eiqn && !(volume.ISCSI.ReadOnly && existingVolume.ISCSI.ReadOnly) {
				return true
			}
		}

		if volume.RBD != nil && existingVolume.RBD != nil {
			mon, pool, image := volume.RBD.CephMonitors, volume.RBD.RBDPool, volume.RBD.RBDImage
			emon, epool, eimage := existingVolume.RBD.CephMonitors, existingVolume.RBD.RBDPool, existingVolume.RBD.RBDImage
			// two RBDs images are the same if they share the same Ceph monitor, are in the same RADOS Pool, and have the same image name
			// only one read-write mount is permitted for the same RBD image.
			// same RBD image mounted by multiple Pods conflicts unless all Pods mount the image read-only
			if haveOverlap(mon, emon) && pool == epool && image == eimage && !(volume.RBD.ReadOnly && existingVolume.RBD.ReadOnly) {
				return true
			}
		}
	}

	return false
}

// haveOverlap searches two arrays and returns true if they have at least one common element; returns false otherwise.
func haveOverlap(a1, a2 []string) bool {
	if len(a1) > len(a2) {
		a1, a2 = a2, a1
	}
	m := sets.New(a1...)
	for _, val := range a2 {
		if _, ok := m[val]; ok {
			return true
		}
	}

	return false
}

// return true if there are conflict checking targets.
func needsRestrictionsCheck(v v1.Volume) bool {
	return v.GCEPersistentDisk != nil || v.AWSElasticBlockStore != nil || v.RBD != nil || v.ISCSI != nil
}

// PreFilter computes and stores cycleState containing details for enforcing ReadWriteOncePod.
func (pl *VolumeRestrictions) PreFilter(ctx context.Context, cycleState *framework.CycleState, pod *v1.Pod) (*framework.PreFilterResult, *framework.Status) {
	needsCheck := false
	for i := range pod.Spec.Volumes {
		if needsRestrictionsCheck(pod.Spec.Volumes[i]) {
			needsCheck = true
			break
		}
	}

	pvcs, err := pl.readWriteOncePodPVCsForPod(ctx, pod)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, framework.NewStatus(framework.UnschedulableAndUnresolvable, err.Error())
		}
		return nil, framework.AsStatus(err)
	}

	s, err := pl.calPreFilterState(ctx, pod, pvcs)
	if err != nil {
		return nil, framework.AsStatus(err)
	}

	if !needsCheck && s.conflictingPVCRefCount == 0 {
		return nil, framework.NewStatus(framework.Skip)
	}
	cycleState.Write(preFilterStateKey, s)
	return nil, nil
}

// AddPod from pre-computed data in cycleState.
func (pl *VolumeRestrictions) AddPod(ctx context.Context, cycleState *framework.CycleState, podToSchedule *v1.Pod, podInfoToAdd *framework.PodInfo, nodeInfo *framework.NodeInfo) *framework.Status {
	state, err := getPreFilterState(cycleState)
	if err != nil {
		return framework.AsStatus(err)
	}
	state.updateWithPod(podInfoToAdd, 1)
	return nil
}

// RemovePod from pre-computed data in cycleState.
func (pl *VolumeRestrictions) RemovePod(ctx context.Context, cycleState *framework.CycleState, podToSchedule *v1.Pod, podInfoToRemove *framework.PodInfo, nodeInfo *framework.NodeInfo) *framework.Status {
	state, err := getPreFilterState(cycleState)
	if err != nil {
		return framework.AsStatus(err)
	}
	state.updateWithPod(podInfoToRemove, -1)
	return nil
}

func getPreFilterState(cycleState *framework.CycleState) (*preFilterState, error) {
	c, err := cycleState.Read(preFilterStateKey)
	if err != nil {
		// preFilterState doesn't exist, likely PreFilter wasn't invoked.
		return nil, fmt.Errorf("cannot read %q from cycleState", preFilterStateKey)
	}

	s, ok := c.(*preFilterState)
	if !ok {
		return nil, fmt.Errorf("%+v convert to volumerestrictions.state error", c)
	}
	return s, nil
}

// calPreFilterState computes preFilterState describing which PVCs use ReadWriteOncePod
// and which pods in the cluster are in conflict.
func (pl *VolumeRestrictions) calPreFilterState(ctx context.Context, pod *v1.Pod, pvcs sets.Set[string]) (*preFilterState, error) {
	conflictingPVCRefCount := 0
	for pvc := range pvcs {
		key := framework.GetNamespacedName(pod.Namespace, pvc)
		if pl.sharedLister.StorageInfos().IsPVCUsedByPods(key) {
			// There can only be at most one pod using the ReadWriteOncePod PVC.
			conflictingPVCRefCount += 1
		}
	}
	return &preFilterState{
		readWriteOncePodPVCs:   pvcs,
		conflictingPVCRefCount: conflictingPVCRefCount,
	}, nil
}

func (pl *VolumeRestrictions) readWriteOncePodPVCsForPod(ctx context.Context, pod *v1.Pod) (sets.Set[string], error) {
	pvcs := sets.New[string]()
	for _, volume := range pod.Spec.Volumes {
		if volume.PersistentVolumeClaim == nil {
			continue
		}

		pvc, err := pl.pvcLister.PersistentVolumeClaims(pod.Namespace).Get(volume.PersistentVolumeClaim.ClaimName)
		if err != nil {
			return nil, err
		}

		if !v1helper.ContainsAccessMode(pvc.Spec.AccessModes, v1.ReadWriteOncePod) {
			continue
		}
		pvcs.Insert(pvc.Name)
	}
	return pvcs, nil
}

// Checks if scheduling the pod onto this node would cause any conflicts with
// existing volumes.
func satisfyVolumeConflicts(pod *v1.Pod, nodeInfo *framework.NodeInfo) bool {
	for i := range pod.Spec.Volumes {
		v := pod.Spec.Volumes[i]
		if !needsRestrictionsCheck(v) {
			continue
		}
		for _, ev := range nodeInfo.Pods {
			if isVolumeConflict(&v, ev.Pod) {
				return false
			}
		}
	}
	return true
}

// Checks if scheduling the pod would cause any ReadWriteOncePod PVC access mode conflicts.
func satisfyReadWriteOncePod(ctx context.Context, state *preFilterState) *framework.Status {
	if state == nil {
		return nil
	}
	if state.conflictingPVCRefCount > 0 {
		return framework.NewStatus(framework.Unschedulable, ErrReasonReadWriteOncePodConflict)
	}
	return nil
}

// PreFilterExtensions returns prefilter extensions, pod add and remove.
func (pl *VolumeRestrictions) PreFilterExtensions() framework.PreFilterExtensions {
	return pl
}

// Filter invoked at the filter extension point.
// It evaluates if a pod can fit due to the volumes it requests, and those that
// are already mounted. If there is already a volume mounted on that node, another pod that uses the same volume
// can't be scheduled there.
// This is GCE, Amazon EBS, ISCSI and Ceph RBD specific for now:
// - GCE PD allows multiple mounts as long as they're all read-only
// - AWS EBS forbids any two pods mounting the same volume ID
// - Ceph RBD forbids if any two pods share at least same monitor, and match pool and image, and the image is read-only
// - ISCSI forbids if any two pods share at least same IQN and ISCSI volume is read-only
// If the pod uses PVCs with the ReadWriteOncePod access mode, it evaluates if
// these PVCs are already in-use and if preemption will help.
func (pl *VolumeRestrictions) Filter(ctx context.Context, cycleState *framework.CycleState, pod *v1.Pod, nodeInfo *framework.NodeInfo) *framework.Status {
	if !satisfyVolumeConflicts(pod, nodeInfo) {
		return framework.NewStatus(framework.Unschedulable, ErrReasonDiskConflict)
	}
	state, err := getPreFilterState(cycleState)
	if err != nil {
		return framework.AsStatus(err)
	}
	return satisfyReadWriteOncePod(ctx, state)
}

// EventsToRegister returns the possible events that may make a Pod
// failed by this plugin schedulable.
func (pl *VolumeRestrictions) EventsToRegister(_ context.Context) ([]framework.ClusterEventWithHint, error) {
	// A note about UpdateNodeTaint/UpdateNodeLabel event:
	// Ideally, it's supposed to register only Add because any Node update event will never change the result from this plugin.
	// But, we may miss Node/Add event due to preCheck, and we decided to register UpdateNodeTaint | UpdateNodeLabel for all plugins registering Node/Add.
	// See: https://github.com/kubernetes/kubernetes/issues/109437
	nodeActionType := framework.Add | framework.UpdateNodeTaint | framework.UpdateNodeLabel
	if pl.enableSchedulingQueueHint {
		// preCheck is not used when QHint is enabled, and hence Update event isn't necessary.
		nodeActionType = framework.Add
	}

	return []framework.ClusterEventWithHint{
		// Pods may fail to schedule because of volumes conflicting with other pods on same node.
		// Once running pods are deleted and volumes have been released, the unschedulable pod will be schedulable.
		// Due to immutable fields `spec.volumes`, pod update events are ignored.
		{Event: framework.ClusterEvent{Resource: framework.Pod, ActionType: framework.Delete}, QueueingHintFn: pl.isSchedulableAfterPodDeleted},
		// A new Node may make a pod schedulable.
		// We intentionally don't set QueueingHint since all Node/Add events could make Pods schedulable.
		{Event: framework.ClusterEvent{Resource: framework.Node, ActionType: nodeActionType}},
		// Pods may fail to schedule because the PVC it uses has not yet been created.
		// This PVC is required to exist to check its access modes.
		{Event: framework.ClusterEvent{Resource: framework.PersistentVolumeClaim, ActionType: framework.Add},
			QueueingHintFn: pl.isSchedulableAfterPersistentVolumeClaimAdded},
	}, nil
}

// isSchedulableAfterPersistentVolumeClaimAdded is invoked whenever a PersistentVolumeClaim added or changed, It checks whether
// that change made a previously unschedulable pod schedulable.
func (pl *VolumeRestrictions) isSchedulableAfterPersistentVolumeClaimAdded(logger klog.Logger, pod *v1.Pod, oldObj, newObj interface{}) (framework.QueueingHint, error) {
	_, newPersistentVolumeClaim, err := util.As[*v1.PersistentVolumeClaim](oldObj, newObj)
	if err != nil {
		return framework.Queue, fmt.Errorf("unexpected objects in isSchedulableAfterPersistentVolumeClaimChange: %w", err)
	}

	if newPersistentVolumeClaim.Namespace != pod.Namespace {
		return framework.QueueSkip, nil
	}

	for _, volume := range pod.Spec.Volumes {
		if volume.PersistentVolumeClaim == nil {
			continue
		}

		if volume.PersistentVolumeClaim.ClaimName == newPersistentVolumeClaim.Name {
			logger.V(5).Info("PVC that is referred from the pod was created, which might make this pod schedulable", "pod", klog.KObj(pod), "PVC", klog.KObj(newPersistentVolumeClaim))
			return framework.Queue, nil
		}
	}
	logger.V(5).Info("PVC irrelevant to the Pod was created, which doesn't make this pod schedulable", "pod", klog.KObj(pod), "PVC", klog.KObj(newPersistentVolumeClaim))
	return framework.QueueSkip, nil
}

// isSchedulableAfterPodDeleted is invoked whenever a pod deleted,
// It checks whether the deleted pod will conflict with volumes of other pods on the same node
func (pl *VolumeRestrictions) isSchedulableAfterPodDeleted(logger klog.Logger, pod *v1.Pod, oldObj, newObj interface{}) (framework.QueueingHint, error) {
	deletedPod, _, err := util.As[*v1.Pod](oldObj, newObj)
	if err != nil {
		return framework.Queue, fmt.Errorf("unexpected objects in isSchedulableAfterPodDeleted: %w", err)
	}

	if deletedPod.Namespace != pod.Namespace {
		return framework.QueueSkip, nil
	}

	nodeInfo := framework.NewNodeInfo(deletedPod)
	if !satisfyVolumeConflicts(pod, nodeInfo) {
		logger.V(5).Info("Pod with the volume that the target pod requires was deleted, which might make this pod schedulable", "pod", klog.KObj(pod), "deletedPod", klog.KObj(deletedPod))
		return framework.Queue, nil
	}

	// Return Queue if a deleted pod uses the same PVC since the pod may be unschedulable due to the ReadWriteOncePod access mode of the PVC.
	//
	// For now, we don't actually fetch PVC and check the access mode because that operation could be expensive.
	// Once the observability around QHint is established,
	// we may want to do that depending on how much the operation would impact the QHint latency negatively.
	// https://github.com/kubernetes/kubernetes/issues/124566
	claims := sets.New[string]()
	for _, volume := range pod.Spec.Volumes {
		if volume.PersistentVolumeClaim != nil {
			claims.Insert(volume.PersistentVolumeClaim.ClaimName)
		}
	}
	for _, volume := range deletedPod.Spec.Volumes {
		if volume.PersistentVolumeClaim != nil && claims.Has(volume.PersistentVolumeClaim.ClaimName) {
			logger.V(5).Info("Pod with the same PVC that the target pod requires was deleted, which might make this pod schedulable", "pod", klog.KObj(pod), "deletedPod", klog.KObj(deletedPod))
			return framework.Queue, nil
		}
	}

	logger.V(5).Info("An irrelevant Pod was deleted, which doesn't make this pod schedulable", "pod", klog.KObj(pod), "deletedPod", klog.KObj(deletedPod))
	return framework.QueueSkip, nil
}

// New initializes a new plugin and returns it.
func New(_ context.Context, _ runtime.Object, handle framework.Handle, fts feature.Features) (framework.Plugin, error) {
	informerFactory := handle.SharedInformerFactory()
	pvcLister := informerFactory.Core().V1().PersistentVolumeClaims().Lister()
	sharedLister := handle.SnapshotSharedLister()

	return &VolumeRestrictions{
		pvcLister:                 pvcLister,
		sharedLister:              sharedLister,
		enableSchedulingQueueHint: fts.EnableSchedulingQueueHint,
	}, nil
}
