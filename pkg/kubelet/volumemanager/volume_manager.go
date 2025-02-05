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

package volumemanager

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/klog/v2"
	"k8s.io/mount-utils"

	v1 "k8s.io/api/core/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/record"
	internalapi "k8s.io/cri-api/pkg/apis"
	csitrans "k8s.io/csi-translation-lib"
	"k8s.io/kubernetes/pkg/kubelet/config"
	"k8s.io/kubernetes/pkg/kubelet/container"
	"k8s.io/kubernetes/pkg/kubelet/volumemanager/cache"
	"k8s.io/kubernetes/pkg/kubelet/volumemanager/metrics"
	"k8s.io/kubernetes/pkg/kubelet/volumemanager/populator"
	"k8s.io/kubernetes/pkg/kubelet/volumemanager/reconciler"
	"k8s.io/kubernetes/pkg/volume"
	"k8s.io/kubernetes/pkg/volume/csimigration"
	"k8s.io/kubernetes/pkg/volume/util"
	"k8s.io/kubernetes/pkg/volume/util/hostutil"
	"k8s.io/kubernetes/pkg/volume/util/operationexecutor"
	"k8s.io/kubernetes/pkg/volume/util/types"
	"k8s.io/kubernetes/pkg/volume/util/volumepathhandler"
)

const (
	// reconcilerLoopSleepPeriod is the amount of time the reconciler loop waits
	// between successive executions
	reconcilerLoopSleepPeriod = 100 * time.Millisecond

	// desiredStateOfWorldPopulatorLoopSleepPeriod is the amount of time the
	// DesiredStateOfWorldPopulator loop waits between successive executions
	desiredStateOfWorldPopulatorLoopSleepPeriod = 100 * time.Millisecond

	// podAttachAndMountTimeout is the maximum amount of time the
	// WaitForAttachAndMount call will wait for all volumes in the specified pod
	// to be attached and mounted. Even though cloud operations can take several
	// minutes to complete, we set the timeout to 2 minutes because kubelet
	// will retry in the next sync iteration. This frees the associated
	// goroutine of the pod to process newer updates if needed (e.g., a delete
	// request to the pod).
	// Value is slightly offset from 2 minutes to make timeouts due to this
	// constant recognizable.
	podAttachAndMountTimeout = 2*time.Minute + 3*time.Second

	// podAttachAndMountRetryInterval is the amount of time the GetVolumesForPod
	// call waits before retrying
	podAttachAndMountRetryInterval = 300 * time.Millisecond

	// waitForAttachTimeout is the maximum amount of time a
	// operationexecutor.Mount call will wait for a volume to be attached.
	// Set to 10 minutes because we've seen attach operations take several
	// minutes to complete for some volume plugins in some cases. While this
	// operation is waiting it only blocks other operations on the same device,
	// other devices are not affected.
	waitForAttachTimeout = 10 * time.Minute
)

// VolumeManager runs a set of asynchronous loops that figure out which volumes
// need to be attached/mounted/unmounted/detached based on the pods scheduled on
// this node and makes it so.
type VolumeManager interface {
	// Starts the volume manager and all the asynchronous loops that it controls
	Run(ctx context.Context, sourcesReady config.SourcesReady)

	// WaitForAttachAndMount processes the volumes referenced in the specified
	// pod and blocks until they are all attached and mounted (reflected in
	// actual state of the world).
	// An error is returned if all volumes are not attached and mounted within
	// the duration defined in podAttachAndMountTimeout.
	WaitForAttachAndMount(ctx context.Context, pod *v1.Pod) error

	// WaitForUnmount processes the volumes referenced in the specified
	// pod and blocks until they are all unmounted (reflected in the actual
	// state of the world).
	// An error is returned if all volumes are not unmounted within
	// the duration defined in podAttachAndMountTimeout.
	WaitForUnmount(ctx context.Context, pod *v1.Pod) error

	// WaitForAllPodsUnmount is a version of WaitForUnmount that blocks and
	// waits until all the volumes belonging to all the pods are unmounted.
	// An error is returned if there's at least one Pod with volumes not unmounted
	// within the duration defined in podAttachAndMountTimeout.
	WaitForAllPodsUnmount(ctx context.Context, pods []*v1.Pod) error

	// GetMountedVolumesForPod returns a VolumeMap containing the volumes
	// referenced by the specified pod that are successfully attached and
	// mounted. The key in the map is the OuterVolumeSpecName (i.e.
	// pod.Spec.Volumes[x].Name). It returns an empty VolumeMap if pod has no
	// volumes.
	GetMountedVolumesForPod(podName types.UniquePodName) container.VolumeMap

	// GetPossiblyMountedVolumesForPod returns a VolumeMap containing the volumes
	// referenced by the specified pod that are either successfully attached
	// and mounted or are "uncertain", i.e. a volume plugin may be mounting
	// them right now. The key in the map is the OuterVolumeSpecName (i.e.
	// pod.Spec.Volumes[x].Name). It returns an empty VolumeMap if pod has no
	// volumes.
	GetPossiblyMountedVolumesForPod(podName types.UniquePodName) container.VolumeMap

	// GetExtraSupplementalGroupsForPod returns a list of the extra
	// supplemental groups for the Pod. These extra supplemental groups come
	// from annotations on persistent volumes that the pod depends on.
	GetExtraSupplementalGroupsForPod(pod *v1.Pod) []int64

	// GetVolumesInUse returns a list of all volumes that implement the volume.Attacher
	// interface and are currently in use according to the actual and desired
	// state of the world caches. A volume is considered "in use" as soon as it
	// is added to the desired state of world, indicating it *should* be
	// attached to this node and remains "in use" until it is removed from both
	// the desired state of the world and the actual state of the world, or it
	// has been unmounted (as indicated in actual state of world).
	GetVolumesInUse() []v1.UniqueVolumeName

	// ReconcilerStatesHasBeenSynced returns true only after the actual states in reconciler
	// has been synced at least once after kubelet starts so that it is safe to update mounted
	// volume list retrieved from actual state.
	ReconcilerStatesHasBeenSynced() bool

	// VolumeIsAttached returns true if the given volume is attached to this
	// node.
	VolumeIsAttached(volumeName v1.UniqueVolumeName) bool

	// Marks the specified volume as having successfully been reported as "in
	// use" in the nodes's volume status.
	MarkVolumesAsReportedInUse(volumesReportedAsInUse []v1.UniqueVolumeName)
}

// podStateProvider can determine if a pod is going to be terminated
type PodStateProvider interface {
	ShouldPodContainersBeTerminating(k8stypes.UID) bool
	ShouldPodRuntimeBeRemoved(k8stypes.UID) bool
}

// PodManager is the subset of methods the manager needs to observe the actual state of the kubelet.
// See pkg/k8s.io/kubernetes/pkg/kubelet/pod.Manager for method godoc.
type PodManager interface {
	GetPodByUID(k8stypes.UID) (*v1.Pod, bool)
	GetPods() []*v1.Pod
}

// NewVolumeManager returns a new concrete instance implementing the
// VolumeManager interface.
//
// kubeClient - kubeClient is the kube API client used by DesiredStateOfWorldPopulator
// to communicate with the API server to fetch PV and PVC objects
//
// volumePluginMgr - the volume plugin manager used to access volume plugins.
// Must be pre-initialized.
func NewVolumeManager(
	controllerAttachDetachEnabled bool,
	nodeName k8stypes.NodeName,
	podManager PodManager,
	podStateProvider PodStateProvider,
	kubeClient clientset.Interface,
	volumePluginMgr *volume.VolumePluginMgr,
	kubeContainerRuntime container.Runtime,
	runtimeService internalapi.RuntimeService,
	mounter mount.Interface,
	hostutil hostutil.HostUtils,
	kubeletPodsDir string,
	recorder record.EventRecorder,
	blockVolumePathHandler volumepathhandler.BlockVolumePathHandler) VolumeManager {

	seLinuxTranslator := util.NewSELinuxLabelTranslator()
	vm := &volumeManager{
		kubeClient:          kubeClient,
		volumePluginMgr:     volumePluginMgr,
		desiredStateOfWorld: cache.NewDesiredStateOfWorld(volumePluginMgr, seLinuxTranslator),
		actualStateOfWorld:  cache.NewActualStateOfWorld(nodeName, volumePluginMgr),
		operationExecutor: operationexecutor.NewOperationExecutor(operationexecutor.NewOperationGenerator(
			kubeClient,
			volumePluginMgr,
			recorder,
			blockVolumePathHandler)),
	}

	intreeToCSITranslator := csitrans.New()
	csiMigratedPluginManager := csimigration.NewPluginManager(intreeToCSITranslator, utilfeature.DefaultFeatureGate)

	vm.intreeToCSITranslator = intreeToCSITranslator
	vm.csiMigratedPluginManager = csiMigratedPluginManager
	vm.desiredStateOfWorldPopulator = populator.NewDesiredStateOfWorldPopulator(
		kubeClient,
		desiredStateOfWorldPopulatorLoopSleepPeriod,
		podManager,
		podStateProvider,
		vm.desiredStateOfWorld,
		vm.actualStateOfWorld,
		kubeContainerRuntime,
		runtimeService,
		csiMigratedPluginManager,
		intreeToCSITranslator,
		volumePluginMgr)
	vm.reconciler = reconciler.NewReconciler(
		kubeClient,
		controllerAttachDetachEnabled,
		reconcilerLoopSleepPeriod,
		waitForAttachTimeout,
		nodeName,
		vm.desiredStateOfWorld,
		vm.actualStateOfWorld,
		vm.desiredStateOfWorldPopulator.HasAddedPods,
		vm.operationExecutor,
		mounter,
		hostutil,
		volumePluginMgr,
		kubeletPodsDir)

	return vm
}

// volumeManager implements the VolumeManager interface
type volumeManager struct {
	// kubeClient is the kube API client used by DesiredStateOfWorldPopulator to
	// communicate with the API server to fetch PV and PVC objects
	kubeClient clientset.Interface

	// volumePluginMgr is the volume plugin manager used to access volume
	// plugins. It must be pre-initialized.
	volumePluginMgr *volume.VolumePluginMgr

	// desiredStateOfWorld is a data structure containing the desired state of
	// the world according to the volume manager: i.e. what volumes should be
	// attached and which pods are referencing the volumes).
	// The data structure is populated by the desired state of the world
	// populator using the kubelet pod manager.
	desiredStateOfWorld cache.DesiredStateOfWorld

	// actualStateOfWorld is a data structure containing the actual state of
	// the world according to the manager: i.e. which volumes are attached to
	// this node and what pods the volumes are mounted to.
	// The data structure is populated upon successful completion of attach,
	// detach, mount, and unmount actions triggered by the reconciler.
	actualStateOfWorld cache.ActualStateOfWorld

	// operationExecutor is used to start asynchronous attach, detach, mount,
	// and unmount operations.
	operationExecutor operationexecutor.OperationExecutor

	// reconciler runs an asynchronous periodic loop to reconcile the
	// desiredStateOfWorld with the actualStateOfWorld by triggering attach,
	// detach, mount, and unmount operations using the operationExecutor.
	reconciler reconciler.Reconciler

	// desiredStateOfWorldPopulator runs an asynchronous periodic loop to
	// populate the desiredStateOfWorld using the kubelet PodManager.
	desiredStateOfWorldPopulator populator.DesiredStateOfWorldPopulator

	// csiMigratedPluginManager keeps track of CSI migration status of plugins
	csiMigratedPluginManager csimigration.PluginManager

	// intreeToCSITranslator translates in-tree volume specs to CSI
	intreeToCSITranslator csimigration.InTreeToCSITranslator
}

func (vm *volumeManager) Run(ctx context.Context, sourcesReady config.SourcesReady) {
	defer runtime.HandleCrash()

	if vm.kubeClient != nil {
		// start informer for CSIDriver
		go vm.volumePluginMgr.Run(ctx.Done())
	}

	go vm.desiredStateOfWorldPopulator.Run(ctx, sourcesReady)
	klog.V(2).InfoS("The desired_state_of_world populator starts")

	klog.InfoS("Starting Kubelet Volume Manager")
	go vm.reconciler.Run(ctx.Done())

	metrics.Register(vm.actualStateOfWorld, vm.desiredStateOfWorld, vm.volumePluginMgr)

	<-ctx.Done()
	klog.InfoS("Shutting down Kubelet Volume Manager")
}

func (vm *volumeManager) GetMountedVolumesForPod(podName types.UniquePodName) container.VolumeMap {
	podVolumes := make(container.VolumeMap)
	for _, mountedVolume := range vm.actualStateOfWorld.GetMountedVolumesForPod(podName) {
		podVolumes[mountedVolume.OuterVolumeSpecName] = container.VolumeInfo{
			Mounter:             mountedVolume.Mounter,
			BlockVolumeMapper:   mountedVolume.BlockVolumeMapper,
			ReadOnly:            mountedVolume.VolumeSpec.ReadOnly,
			InnerVolumeSpecName: mountedVolume.InnerVolumeSpecName,
		}
	}
	return podVolumes
}

func (vm *volumeManager) GetPossiblyMountedVolumesForPod(podName types.UniquePodName) container.VolumeMap {
	podVolumes := make(container.VolumeMap)
	for _, mountedVolume := range vm.actualStateOfWorld.GetPossiblyMountedVolumesForPod(podName) {
		podVolumes[mountedVolume.OuterVolumeSpecName] = container.VolumeInfo{
			Mounter:             mountedVolume.Mounter,
			BlockVolumeMapper:   mountedVolume.BlockVolumeMapper,
			ReadOnly:            mountedVolume.VolumeSpec.ReadOnly,
			InnerVolumeSpecName: mountedVolume.InnerVolumeSpecName,
		}
	}
	return podVolumes
}

func (vm *volumeManager) GetExtraSupplementalGroupsForPod(pod *v1.Pod) []int64 {
	podName := util.GetUniquePodName(pod)
	supplementalGroups := sets.New[string]()

	for _, mountedVolume := range vm.actualStateOfWorld.GetMountedVolumesForPod(podName) {
		if mountedVolume.VolumeGIDValue != "" {
			supplementalGroups.Insert(mountedVolume.VolumeGIDValue)
		}
	}

	result := make([]int64, 0, supplementalGroups.Len())
	for _, group := range supplementalGroups.UnsortedList() {
		iGroup, extra := getExtraSupplementalGID(group, pod)
		if !extra {
			continue
		}

		result = append(result, int64(iGroup))
	}

	return result
}

func (vm *volumeManager) GetVolumesInUse() []v1.UniqueVolumeName {
	// Report volumes in desired state of world and actual state of world so
	// that volumes are marked in use as soon as the decision is made that the
	// volume *should* be attached to this node until it is safely unmounted.
	desiredVolumes := vm.desiredStateOfWorld.GetVolumesToMount()
	allAttachedVolumes := vm.actualStateOfWorld.GetAttachedVolumes()
	volumesToReportInUse := make([]v1.UniqueVolumeName, 0, len(desiredVolumes)+len(allAttachedVolumes))

	for _, volume := range desiredVolumes {
		if volume.PluginIsAttachable {
			volumesToReportInUse = append(volumesToReportInUse, volume.VolumeName)
		}
	}

	for _, volume := range allAttachedVolumes {
		if volume.PluginIsAttachable {
			volumesToReportInUse = append(volumesToReportInUse, volume.VolumeName)
		}
	}

	slices.Sort(volumesToReportInUse)
	return slices.Compact(volumesToReportInUse)
}

func (vm *volumeManager) ReconcilerStatesHasBeenSynced() bool {
	return vm.reconciler.StatesHasBeenSynced()
}

func (vm *volumeManager) VolumeIsAttached(
	volumeName v1.UniqueVolumeName) bool {
	return vm.actualStateOfWorld.VolumeExists(volumeName)
}

func (vm *volumeManager) MarkVolumesAsReportedInUse(
	volumesReportedAsInUse []v1.UniqueVolumeName) {
	vm.desiredStateOfWorld.MarkVolumesReportedInUse(volumesReportedAsInUse)
}

func (vm *volumeManager) WaitForAttachAndMount(ctx context.Context, pod *v1.Pod) error {
	if pod == nil {
		return nil
	}

	expectedVolumes := getExpectedVolumes(pod)
	if len(expectedVolumes) == 0 {
		// No volumes to verify
		return nil
	}

	klog.V(3).InfoS("Waiting for volumes to attach and mount for pod", "pod", klog.KObj(pod))
	uniquePodName := util.GetUniquePodName(pod)

	// Some pods expect to have Setup called over and over again to update.
	// Remount plugins for which this is true. (Atomically updating volumes,
	// like Downward API, depend on this to update the contents of the volume).
	vm.desiredStateOfWorldPopulator.ReprocessPod(uniquePodName)

	err := wait.PollUntilContextTimeout(
		ctx,
		podAttachAndMountRetryInterval,
		podAttachAndMountTimeout,
		true,
		vm.verifyVolumesMountedFunc(uniquePodName, expectedVolumes))

	if err != nil {
		unmountedVolumes :=
			vm.getUnmountedVolumes(uniquePodName, expectedVolumes)
		// Also get unattached volumes and volumes not in dsw for error message
		unattachedVolumes :=
			vm.getUnattachedVolumes(uniquePodName)
		volumesNotInDSW :=
			vm.getVolumesNotInDSW(uniquePodName, expectedVolumes)

		if len(unmountedVolumes) == 0 {
			return nil
		}

		return fmt.Errorf(
			"unmounted volumes=%v, unattached volumes=%v, failed to process volumes=%v: %w",
			unmountedVolumes,
			unattachedVolumes,
			volumesNotInDSW,
			err)
	}

	klog.V(3).InfoS("All volumes are attached and mounted for pod", "pod", klog.KObj(pod))
	return nil
}

func (vm *volumeManager) WaitForUnmount(ctx context.Context, pod *v1.Pod) error {
	if pod == nil {
		return nil
	}

	klog.V(3).InfoS("Waiting for volumes to unmount for pod", "pod", klog.KObj(pod))
	uniquePodName := util.GetUniquePodName(pod)

	vm.desiredStateOfWorldPopulator.ReprocessPod(uniquePodName)

	err := wait.PollUntilContextTimeout(
		ctx,
		podAttachAndMountRetryInterval,
		podAttachAndMountTimeout,
		true,
		vm.verifyVolumesUnmountedFunc(uniquePodName))

	if err != nil {
		var mountedVolumes []string
		for _, v := range vm.actualStateOfWorld.GetMountedVolumesForPod(uniquePodName) {
			mountedVolumes = append(mountedVolumes, v.OuterVolumeSpecName)
		}
		if len(mountedVolumes) == 0 {
			return nil
		}

		slices.Sort(mountedVolumes)
		return fmt.Errorf(
			"mounted volumes=%v: %w",
			mountedVolumes,
			err)
	}

	klog.V(3).InfoS("All volumes are unmounted for pod", "pod", klog.KObj(pod))
	return nil
}

func (vm *volumeManager) WaitForAllPodsUnmount(ctx context.Context, pods []*v1.Pod) error {
	var (
		errors []error
		wg     sync.WaitGroup
	)
	wg.Add(len(pods))
	for _, pod := range pods {
		go func(pod *v1.Pod) {
			defer wg.Done()
			if err := vm.WaitForUnmount(ctx, pod); err != nil {
				errors = append(errors, err)
			}
		}(pod)
	}
	wg.Wait()
	return utilerrors.NewAggregate(errors)
}

func (vm *volumeManager) getVolumesNotInDSW(uniquePodName types.UniquePodName, expectedVolumes []string) []string {
	volumesNotInDSW := sets.New(expectedVolumes...)

	for _, volumeToMount := range vm.desiredStateOfWorld.GetVolumesToMount() {
		if volumeToMount.PodName == uniquePodName {
			volumesNotInDSW.Delete(volumeToMount.OuterVolumeSpecName)
		}
	}

	return sets.List(volumesNotInDSW)
}

// getUnattachedVolumes returns a list of the volumes that are expected to be attached but
// are not currently attached to the node
func (vm *volumeManager) getUnattachedVolumes(uniquePodName types.UniquePodName) []string {
	unattachedVolumes := []string{}
	for _, volumeToMount := range vm.desiredStateOfWorld.GetVolumesToMount() {
		if volumeToMount.PodName == uniquePodName &&
			volumeToMount.PluginIsAttachable &&
			!vm.actualStateOfWorld.VolumeExists(volumeToMount.VolumeName) {
			unattachedVolumes = append(unattachedVolumes, volumeToMount.OuterVolumeSpecName)
		}
	}
	slices.Sort(unattachedVolumes)

	return unattachedVolumes
}

// verifyVolumesMountedFunc returns a method that returns true when all expected
// volumes are mounted.
func (vm *volumeManager) verifyVolumesMountedFunc(podName types.UniquePodName, expectedVolumes []string) wait.ConditionWithContextFunc {
	return func(_ context.Context) (done bool, err error) {
		if errs := vm.desiredStateOfWorld.PopPodErrors(podName); len(errs) > 0 {
			return true, errors.New(strings.Join(errs, "; "))
		}
		for _, expectedVolume := range expectedVolumes {
			_, found := vm.actualStateOfWorld.GetMountedVolumeForPodByOuterVolumeSpecName(podName, expectedVolume)
			if !found {
				return false, nil
			}
		}
		return true, nil
	}
}

// verifyVolumesUnmountedFunc returns a method that is true when there are no mounted volumes for this
// pod.
func (vm *volumeManager) verifyVolumesUnmountedFunc(podName types.UniquePodName) wait.ConditionWithContextFunc {
	return func(_ context.Context) (done bool, err error) {
		if errs := vm.desiredStateOfWorld.PopPodErrors(podName); len(errs) > 0 {
			return true, errors.New(strings.Join(errs, "; "))
		}
		return !vm.actualStateOfWorld.PodHasMountedVolumes(podName), nil
	}
}

// getUnmountedVolumes fetches the current list of mounted volumes from
// the actual state of the world, and uses it to process the list of
// expectedVolumes. It returns a list of unmounted volumes.
// The list also includes volume that may be mounted in uncertain state.
func (vm *volumeManager) getUnmountedVolumes(podName types.UniquePodName, expectedVolumes []string) []string {
	mountedVolumes := sets.New[string]()
	for _, mountedVolume := range vm.actualStateOfWorld.GetMountedVolumesForPod(podName) {
		mountedVolumes.Insert(mountedVolume.OuterVolumeSpecName)
	}
	return filterUnmountedVolumes(mountedVolumes, expectedVolumes)
}

// filterUnmountedVolumes adds each element of expectedVolumes that is not in
// mountedVolumes to a list of unmountedVolumes and returns it.
func filterUnmountedVolumes(mountedVolumes sets.Set[string], expectedVolumes []string) []string {
	unmountedVolumes := []string{}
	for _, expectedVolume := range expectedVolumes {
		if !mountedVolumes.Has(expectedVolume) {
			unmountedVolumes = append(unmountedVolumes, expectedVolume)
		}
	}
	slices.Sort(unmountedVolumes)

	return unmountedVolumes
}

// getExpectedVolumes returns a list of volumes that must be mounted in order to
// consider the volume setup step for this pod satisfied.
func getExpectedVolumes(pod *v1.Pod) []string {
	mounts, devices, _ := util.GetPodVolumeNames(pod, false /* collectSELinuxOptions */)
	return mounts.Union(devices).UnsortedList()
}

// getExtraSupplementalGID returns the value of an extra supplemental GID as
// defined by an annotation on a volume and a boolean indicating whether the
// volume defined a GID that the pod doesn't already request.
func getExtraSupplementalGID(volumeGIDValue string, pod *v1.Pod) (int64, bool) {
	if volumeGIDValue == "" {
		return 0, false
	}

	gid, err := strconv.ParseInt(volumeGIDValue, 10, 64)
	if err != nil {
		return 0, false
	}

	if pod.Spec.SecurityContext != nil {
		for _, existingGID := range pod.Spec.SecurityContext.SupplementalGroups {
			if gid == int64(existingGID) {
				return 0, false
			}
		}
	}

	return gid, true
}
