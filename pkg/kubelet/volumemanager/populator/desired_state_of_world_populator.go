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

/*
Package populator implements interfaces that monitor and keep the states of the
caches in sync with the "ground truth".
*/
package populator

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	utilfeature "k8s.io/apiserver/pkg/util/feature"
	internalapi "k8s.io/cri-api/pkg/apis"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/features"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/component-helpers/storage/ephemeral"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
	"k8s.io/kubernetes/pkg/kubelet/config"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	"k8s.io/kubernetes/pkg/kubelet/metrics"
	"k8s.io/kubernetes/pkg/kubelet/volumemanager/cache"
	"k8s.io/kubernetes/pkg/volume"
	"k8s.io/kubernetes/pkg/volume/csimigration"
	"k8s.io/kubernetes/pkg/volume/util"
	volumetypes "k8s.io/kubernetes/pkg/volume/util/types"
)

const (
	dswpEventChannelCapacity                    = 1000
	dswpEventMaxStreamRetries                   = 5
	dswpEventMinLoopBeforeIncreasingSleepPeriod = 3
	initialLoopSleepDuration                    = time.Duration(100) * time.Millisecond
)

// DesiredStateOfWorldPopulator periodically loops through the list of active
// pods and ensures that each one exists in the desired state of the world cache
// if it has volumes. It also verifies that the pods in the desired state of the
// world cache still exist, if not, it removes them.
type DesiredStateOfWorldPopulator interface {
	Run(ctx context.Context, sourcesReady config.SourcesReady)

	// ReprocessPod sets value for the specified pod in processedPods
	// to false, forcing it to be reprocessed. This is required to enable
	// remounting volumes on pod updates (volumes like Downward API volumes
	// depend on this behavior to ensure volume content is updated).
	ReprocessPod(podName volumetypes.UniquePodName)

	// HasAddedPods returns whether the populator has looped through the list
	// of active pods and added them to the desired state of the world cache,
	// at a time after sources are all ready, at least once. It does not
	// return true before sources are all ready because before then, there is
	// a chance many or all pods are missing from the list of active pods and
	// so few to none will have been added.
	HasAddedPods() bool
}

// PodStateProvider can determine if a pod is going to be terminated.
type PodStateProvider interface {
	ShouldPodContainersBeTerminating(types.UID) bool
	ShouldPodRuntimeBeRemoved(types.UID) bool
}

// PodManager is the subset of methods the manager needs to observe the actual state of the kubelet.
// See pkg/k8s.io/kubernetes/pkg/kubelet/pod.Manager for method godoc.
type PodManager interface {
	GetPodByUID(types.UID) (*v1.Pod, bool)
	GetPods() []*v1.Pod
}

// NewDesiredStateOfWorldPopulator returns a new instance of
// DesiredStateOfWorldPopulator.
//
// kubeClient - used to fetch PV and PVC objects from the API server
// loopSleepDuration - the amount of time the populator loop sleeps between
// successive executions
//
// podManager - the kubelet podManager that is the source of truth for the pods
// that exist on this host
//
// desiredStateOfWorld - the cache to populate
func NewDesiredStateOfWorldPopulator(
	kubeClient clientset.Interface,
	loopSleepDuration time.Duration,
	podManager PodManager,
	podStateProvider PodStateProvider,
	desiredStateOfWorld cache.DesiredStateOfWorld,
	actualStateOfWorld cache.ActualStateOfWorld,
	kubeContainerRuntime kubecontainer.Runtime,
	runtimeService internalapi.RuntimeService,
	csiMigratedPluginManager csimigration.PluginManager,
	intreeToCSITranslator csimigration.InTreeToCSITranslator,
	volumePluginMgr *volume.VolumePluginMgr) DesiredStateOfWorldPopulator {
	return &desiredStateOfWorldPopulator{
		kubeClient:             kubeClient,
		loopSleepDuration:      loopSleepDuration,
		loopSleepDurationLock:  sync.RWMutex{},
		watchProcessStoped:     true,
		watchProcessStopedLock: sync.RWMutex{},
		podManager:             podManager,
		podStateProvider:       podStateProvider,
		desiredStateOfWorld:    desiredStateOfWorld,
		actualStateOfWorld:     actualStateOfWorld,
		pods: processedPods{
			processedPods: make(map[volumetypes.UniquePodName]bool)},
		kubeContainerRuntime:     kubeContainerRuntime,
		runtimeService:           runtimeService,
		hasAddedPods:             false,
		hasAddedPodsLock:         sync.RWMutex{},
		csiMigratedPluginManager: csiMigratedPluginManager,
		intreeToCSITranslator:    intreeToCSITranslator,
		volumePluginMgr:          volumePluginMgr,
	}
}

type desiredStateOfWorldPopulator struct {
	kubeClient               clientset.Interface
	loopSleepDuration        time.Duration
	loopSleepDurationLock    sync.RWMutex
	watchProcessStoped       bool
	watchProcessStopedLock   sync.RWMutex
	podManager               PodManager
	podStateProvider         PodStateProvider
	desiredStateOfWorld      cache.DesiredStateOfWorld
	actualStateOfWorld       cache.ActualStateOfWorld
	pods                     processedPods
	kubeContainerRuntime     kubecontainer.Runtime
	runtimeService           internalapi.RuntimeService
	hasAddedPods             bool
	hasAddedPodsLock         sync.RWMutex
	populatorLock            sync.RWMutex
	executionCounter         int
	csiMigratedPluginManager csimigration.PluginManager
	intreeToCSITranslator    csimigration.InTreeToCSITranslator
	volumePluginMgr          *volume.VolumePluginMgr
}

type processedPods struct {
	processedPods map[volumetypes.UniquePodName]bool
	sync.RWMutex
}

func (dswp *desiredStateOfWorldPopulator) Run(ctx context.Context, sourcesReady config.SourcesReady) {
	// Wait for the completion of a loop that started after sources are all ready, then set hasAddedPods accordingly
	logger := klog.FromContext(ctx)
	logger.Info("Desired state populator starts to run")
	_ = wait.PollUntilContextCancel(ctx, dswp.getLoopSleepDuration(), false, func(ctx context.Context) (bool, error) {
		done := sourcesReady.AllReady()
		dswp.populatorLoop(ctx)
		return done, nil
	})
	dswp.hasAddedPodsLock.Lock()
	if !dswp.hasAddedPods {
		logger.Info("Finished populating initial desired state of world")
		dswp.hasAddedPods = true
	}
	dswp.hasAddedPodsLock.Unlock()

	// initially the loopSleepDuration is set to 100ms (min period)
	// the only check needed is wether the FG is enabled
	if utilfeature.DefaultFeatureGate.Enabled(features.EventedDesiredStateOfWorldPopulator) {
		dswp.setWatchProcessStoped(false)
		go func() {
			_ = wait.PollUntilContextCancel(ctx, 0, false, func(ctx context.Context) (bool, error) {
				return dswp.watchAndProcessEvent(ctx), nil
			})
		}()
	}
	wait.DyanmicPeriodUntil(ctx, func() {
		dswp.populatorLoop(ctx)
		if utilfeature.DefaultFeatureGate.Enabled(features.EventedDesiredStateOfWorldPopulator) {
			if !dswp.isWatchProcessStoped() {
				dswp.increaseLoopSleepDuration(ctx)
			} else if dswp.getLoopSleepDuration() != initialLoopSleepDuration {
				// make sure after the watch func is stoped => reset the loopSleepDuration to the initial default value
				dswp.setLoopSleepDuration(time.Duration(100) * time.Millisecond)
			}
		}
	}, func() time.Duration {
		return dswp.getLoopSleepDuration()
	}, 0, true)
}

func (dswp *desiredStateOfWorldPopulator) increaseLoopSleepDuration(ctx context.Context) {
	if dswp.executionCounter > dswpEventMinLoopBeforeIncreasingSleepPeriod && dswp.getLoopSleepDuration() == initialLoopSleepDuration {
		dswp.executionCounter = 0
	}
	dswp.executionCounter++
	// gradually increase after the third execution
	// ( +100ms on each iteration) the sleep period to a maximum ( 1 second)
	if dswp.getLoopSleepDuration() < initialLoopSleepDuration*10 && dswp.executionCounter > dswpEventMinLoopBeforeIncreasingSleepPeriod {
		dswp.setLoopSleepDuration(dswp.getLoopSleepDuration() + time.Duration(100)*time.Millisecond)
	}
}

// Subscribe to CRI event stream and fallback to 100ms loopSleepDuration
// when CRI runtime returns errors
func (dswp *desiredStateOfWorldPopulator) watchAndProcessEvent(ctx context.Context) bool {
	logger := klog.FromContext(ctx)
	logger.Info("Evented DSWP: watchAndProcessEvent starts")

	containerEventsResponseCh := make(chan *runtimeapi.ContainerEventResponse, dswpEventChannelCapacity)
	defer close(containerEventsResponseCh)

	go func() {
		numAttempts := 0
		for {
			if numAttempts >= dswpEventMaxStreamRetries {
				logger.Info("Evented DSWP: Disable subscription to CRI event")
				dswp.setLoopSleepDuration(initialLoopSleepDuration)
				logger.Info("Evented DSWP: watchAndProcessEvent exits")
				dswp.setWatchProcessStoped(true)
				break
			}

			// Get the container events from the runtime.
			err := dswp.runtimeService.GetContainerEvents(context.Background(), containerEventsResponseCh, func(runtimeapi.RuntimeService_GetContainerEventsClient) {
				metrics.EventedDSWPConn.Inc()
			})
			if err != nil {
				metrics.EventedDSWPConnErr.Inc()
				numAttempts++
				logger.Error(err, "Evented DSWP: Failed to get container events, number of attempts: ", numAttempts)
			}
		}
	}()

	dswp.processCRIEvents(ctx, containerEventsResponseCh)
	return true
}

// Check the CRI event and reset the interval to 100ms
// when the event.ContainerEventType in ('ContainerEventType_CONTAINER_DELETED_EVENT','ContainerEventType_CONTAINER_CREATED_EVENT')
func (dswp *desiredStateOfWorldPopulator) processCRIEvents(ctx context.Context, containerEventsResponseCh chan *runtimeapi.ContainerEventResponse) {
	logger := klog.FromContext(ctx)
	for event := range containerEventsResponseCh {
		if event.ContainerEventType == runtimeapi.ContainerEventType_CONTAINER_DELETED_EVENT || event.ContainerEventType == runtimeapi.ContainerEventType_CONTAINER_CREATED_EVENT {
			logger.Info("Evented DSWP: container created or deleted")
			dswp.setLoopSleepDuration(initialLoopSleepDuration)
			// force execution of populatorLoop function
			// => remediate a condition when multiple events received
			// as a consequence it will reset the timer befor executing
			// the populator loop.
			go dswp.populatorLoop(ctx)
		}
	}
}

func (dswp *desiredStateOfWorldPopulator) ReprocessPod(
	podName volumetypes.UniquePodName) {
	dswp.markPodProcessingFailed(podName)
}

func (dswp *desiredStateOfWorldPopulator) isWatchProcessStoped() bool {
	dswp.watchProcessStopedLock.RLock()
	defer dswp.watchProcessStopedLock.RUnlock()
	return dswp.watchProcessStoped
}

func (dswp *desiredStateOfWorldPopulator) setLoopSleepDuration(duration time.Duration) {
	dswp.loopSleepDurationLock.Lock()
	defer dswp.loopSleepDurationLock.Unlock()
	dswp.loopSleepDuration = duration
}

func (dswp *desiredStateOfWorldPopulator) getLoopSleepDuration() time.Duration {
	dswp.loopSleepDurationLock.RLock()
	defer dswp.loopSleepDurationLock.RUnlock()
	return dswp.loopSleepDuration
}

func (dswp *desiredStateOfWorldPopulator) setWatchProcessStoped(enable bool) {
	dswp.watchProcessStopedLock.Lock()
	defer dswp.watchProcessStopedLock.Unlock()
	dswp.watchProcessStoped = enable
}

func (dswp *desiredStateOfWorldPopulator) HasAddedPods() bool {
	dswp.hasAddedPodsLock.RLock()
	defer dswp.hasAddedPodsLock.RUnlock()
	return dswp.hasAddedPods
}

func (dswp *desiredStateOfWorldPopulator) populatorLoop(ctx context.Context) {
	if utilfeature.DefaultFeatureGate.Enabled(features.EventedDesiredStateOfWorldPopulator) {
		dswp.populatorLock.Lock()
		defer dswp.populatorLock.Unlock()
	}
	dswp.findAndAddNewPods(ctx)
	dswp.findAndRemoveDeletedPods()
}

// Iterate through all pods and add to desired state of world if they don't
// exist but should
func (dswp *desiredStateOfWorldPopulator) findAndAddNewPods(ctx context.Context) {
	for _, pod := range dswp.podManager.GetPods() {
		// Keep consistency of adding pod during reconstruction
		if dswp.hasAddedPods && dswp.podStateProvider.ShouldPodContainersBeTerminating(pod.UID) {
			// Do not (re)add volumes for pods that can't also be starting containers
			continue
		}

		if !dswp.hasAddedPods && dswp.podStateProvider.ShouldPodRuntimeBeRemoved(pod.UID) {
			// When kubelet restarts, we need to add pods to dsw if there is a possibility
			// that the container may still be running
			continue
		}

		dswp.processPodVolumes(ctx, pod)
	}
}

// Iterate through all pods in desired state of world, and remove if they no
// longer exist
func (dswp *desiredStateOfWorldPopulator) findAndRemoveDeletedPods() {
	podsFromCache := make(map[volumetypes.UniquePodName]struct{})
	for _, volumeToMount := range dswp.desiredStateOfWorld.GetVolumesToMount() {
		podsFromCache[volumetypes.UniquePodName(volumeToMount.Pod.UID)] = struct{}{}
		pod, podExists := dswp.podManager.GetPodByUID(volumeToMount.Pod.UID)
		if podExists {

			// check if the attachability has changed for this volume
			if volumeToMount.PluginIsAttachable {
				attachableVolumePlugin, err := dswp.volumePluginMgr.FindAttachablePluginBySpec(volumeToMount.VolumeSpec)
				// only this means the plugin is truly non-attachable
				if err == nil && attachableVolumePlugin == nil {
					// It is not possible right now for a CSI plugin to be both attachable and non-deviceMountable
					// So the uniqueVolumeName should remain the same after the attachability change
					dswp.desiredStateOfWorld.MarkVolumeAttachability(volumeToMount.VolumeName, false)
					klog.InfoS("Volume changes from attachable to non-attachable", "volumeName", volumeToMount.VolumeName)
					continue
				}
			}

			// Exclude known pods that we expect to be running
			if !dswp.podStateProvider.ShouldPodRuntimeBeRemoved(pod.UID) {
				continue
			}
		}

		// Once a pod has been deleted from kubelet pod manager, do not delete
		// it immediately from volume manager. Instead, check the kubelet
		// pod state provider to verify that all containers in the pod have been
		// terminated.
		if !dswp.podStateProvider.ShouldPodRuntimeBeRemoved(volumeToMount.Pod.UID) {
			klog.V(4).InfoS("Pod still has one or more containers in the non-exited state and will not be removed from desired state", "pod", klog.KObj(volumeToMount.Pod))
			continue
		}
		var volumeToMountSpecName string
		if volumeToMount.VolumeSpec != nil {
			volumeToMountSpecName = volumeToMount.VolumeSpec.Name()
		}
		removed := dswp.actualStateOfWorld.PodRemovedFromVolume(volumeToMount.PodName, volumeToMount.VolumeName)
		if removed && podExists {
			klog.V(4).InfoS("Actual state does not yet have volume mount information and pod still exists in pod manager, skip removing volume from desired state", "pod", klog.KObj(volumeToMount.Pod), "podUID", volumeToMount.Pod.UID, "volumeName", volumeToMountSpecName)
			continue
		}
		klog.V(4).InfoS("Removing volume from desired state", "pod", klog.KObj(volumeToMount.Pod), "podUID", volumeToMount.Pod.UID, "volumeName", volumeToMountSpecName)
		dswp.desiredStateOfWorld.DeletePodFromVolume(
			volumeToMount.PodName, volumeToMount.VolumeName)
		dswp.deleteProcessedPod(volumeToMount.PodName)
	}

	// Cleanup orphanded entries from processedPods
	dswp.pods.Lock()
	orphanedPods := make([]volumetypes.UniquePodName, 0, len(dswp.pods.processedPods))
	for k := range dswp.pods.processedPods {
		if _, ok := podsFromCache[k]; !ok {
			orphanedPods = append(orphanedPods, k)
		}
	}
	dswp.pods.Unlock()
	for _, orphanedPod := range orphanedPods {
		uid := types.UID(orphanedPod)
		_, podExists := dswp.podManager.GetPodByUID(uid)
		if !podExists && dswp.podStateProvider.ShouldPodRuntimeBeRemoved(uid) {
			dswp.deleteProcessedPod(orphanedPod)
		}
	}

	podsWithError := dswp.desiredStateOfWorld.GetPodsWithErrors()
	for _, podName := range podsWithError {
		if _, podExists := dswp.podManager.GetPodByUID(types.UID(podName)); !podExists {
			dswp.desiredStateOfWorld.PopPodErrors(podName)
		}
	}
}

// processPodVolumes processes the volumes in the given pod and adds them to the
// desired state of the world.
func (dswp *desiredStateOfWorldPopulator) processPodVolumes(ctx context.Context, pod *v1.Pod) {
	if pod == nil {
		return
	}

	logger := klog.FromContext(ctx)
	uniquePodName := util.GetUniquePodName(pod)
	if dswp.podPreviouslyProcessed(uniquePodName) {
		return
	}

	allVolumesAdded := true
	collectSELinuxOptions := utilfeature.DefaultFeatureGate.Enabled(features.SELinuxMountReadWriteOncePod)
	mounts, devices, seLinuxContainerContexts := util.GetPodVolumeNames(pod, collectSELinuxOptions)

	// Process volume spec for each volume defined in pod
	for _, podVolume := range pod.Spec.Volumes {
		if !mounts.Has(podVolume.Name) && !devices.Has(podVolume.Name) {
			// Volume is not used in the pod, ignore it.
			logger.V(4).Info("Skipping unused volume", "pod", klog.KObj(pod), "volumeName", podVolume.Name)
			continue
		}

		pvc, volumeSpec, volumeGIDValue, err :=
			dswp.createVolumeSpec(logger, podVolume, pod, mounts, devices)
		if err != nil {
			logger.Error(err, "Error processing volume", "pod", klog.KObj(pod), "volumeName", podVolume.Name)
			dswp.desiredStateOfWorld.AddErrorToPod(uniquePodName, err.Error())
			allVolumesAdded = false
			continue
		}

		// Add volume to desired state of world
		uniqueVolumeName, err := dswp.desiredStateOfWorld.AddPodToVolume(
			uniquePodName, pod, volumeSpec, podVolume.Name, volumeGIDValue, seLinuxContainerContexts[podVolume.Name])
		if err != nil {
			logger.Error(err, "Failed to add volume to desiredStateOfWorld", "pod", klog.KObj(pod), "volumeName", podVolume.Name, "volumeSpecName", volumeSpec.Name())
			dswp.desiredStateOfWorld.AddErrorToPod(uniquePodName, err.Error())
			allVolumesAdded = false
			continue
		}

		logger.V(4).Info("Added volume to desired state", "pod", klog.KObj(pod), "volumeName", podVolume.Name, "volumeSpecName", volumeSpec.Name())
		dswp.checkVolumeFSResize(pod, podVolume, pvc, volumeSpec, uniqueVolumeName)
	}

	// some of the volume additions may have failed, should not mark this pod as fully processed
	if allVolumesAdded {
		dswp.markPodProcessed(uniquePodName)
		// New pod has been synced. Re-mount all volumes that need it
		// (e.g. DownwardAPI)
		dswp.actualStateOfWorld.MarkRemountRequired(uniquePodName)
		// Remove any stored errors for the pod, everything went well in this processPodVolumes
		dswp.desiredStateOfWorld.PopPodErrors(uniquePodName)
	} else if dswp.podHasBeenSeenOnce(uniquePodName) {
		// For the Pod which has been processed at least once, even though some volumes
		// may not have been reprocessed successfully this round, we still mark it as processed to avoid
		// processing it at a very high frequency. The pod will be reprocessed when volume manager calls
		// ReprocessPod() which is triggered by SyncPod.
		dswp.markPodProcessed(uniquePodName)
	}

}

// checkVolumeFSResize records desired PVC size for a volume mounted by the pod.
// It is used for comparison with actual size(coming from pvc.Status.Capacity) and calling
// volume expansion on the node if needed.
func (dswp *desiredStateOfWorldPopulator) checkVolumeFSResize(
	pod *v1.Pod,
	podVolume v1.Volume,
	pvc *v1.PersistentVolumeClaim,
	volumeSpec *volume.Spec,
	uniqueVolumeName v1.UniqueVolumeName) {

	// if a volumeSpec does not have PV or has InlineVolumeSpecForCSIMigration set or pvc is nil
	// we can't resize the volume and hence resizing should be skipped.
	if volumeSpec.PersistentVolume == nil || volumeSpec.InlineVolumeSpecForCSIMigration || pvc == nil {
		// Only PVC supports resize operation.
		return
	}

	// volumeSpec.ReadOnly is the value that determines if volume could be formatted when being mounted.
	// This is the same flag that determines filesystem resizing behaviour for offline resizing and hence
	// we should use it here. This value comes from Pod.spec.volumes.persistentVolumeClaim.readOnly.
	if volumeSpec.ReadOnly {
		// This volume is used as read only by this pod, we don't perform resize for read only volumes.
		klog.V(5).InfoS("Skip file system resize check for the volume, as the volume is mounted as readonly", "pod", klog.KObj(pod), "volumeName", podVolume.Name)
		return
	}

	pvCap := volumeSpec.PersistentVolume.Spec.Capacity.Storage().DeepCopy()
	pvcStatusCap := pvc.Status.Capacity.Storage().DeepCopy()
	dswp.desiredStateOfWorld.UpdatePersistentVolumeSize(uniqueVolumeName, pvCap)
	klog.V(5).InfoS("NodeExpandVolume updating size", "actualSize", pvcStatusCap.String(), "desiredSize", pvCap.String(), "volumeName", uniqueVolumeName)
	// in case the actualStateOfWorld was rebuild after kubelet restart ensure that claimSize is set to accurate value
	dswp.actualStateOfWorld.InitializeClaimSize(klog.TODO(), uniqueVolumeName, pvcStatusCap)
}

// podPreviouslyProcessed returns true if the volumes for this pod have already
// been processed/reprocessed by the populator. Otherwise, the volumes for this pod need to
// be reprocessed.
func (dswp *desiredStateOfWorldPopulator) podPreviouslyProcessed(
	podName volumetypes.UniquePodName) bool {
	dswp.pods.RLock()
	defer dswp.pods.RUnlock()

	return dswp.pods.processedPods[podName]
}

// markPodProcessingFailed marks the specified pod from processedPods as false to indicate that it failed processing
func (dswp *desiredStateOfWorldPopulator) markPodProcessingFailed(
	podName volumetypes.UniquePodName) {
	dswp.pods.Lock()
	dswp.pods.processedPods[podName] = false
	dswp.pods.Unlock()
}

// podHasBeenSeenOnce returns true if the pod has been seen by the popoulator
// at least once.
func (dswp *desiredStateOfWorldPopulator) podHasBeenSeenOnce(
	podName volumetypes.UniquePodName) bool {
	dswp.pods.RLock()
	_, exist := dswp.pods.processedPods[podName]
	dswp.pods.RUnlock()
	return exist
}

// markPodProcessed records that the volumes for the specified pod have been
// processed by the populator
func (dswp *desiredStateOfWorldPopulator) markPodProcessed(
	podName volumetypes.UniquePodName) {
	dswp.pods.Lock()
	defer dswp.pods.Unlock()

	dswp.pods.processedPods[podName] = true
}

// deleteProcessedPod removes the specified pod from processedPods
func (dswp *desiredStateOfWorldPopulator) deleteProcessedPod(
	podName volumetypes.UniquePodName) {
	dswp.pods.Lock()
	defer dswp.pods.Unlock()

	delete(dswp.pods.processedPods, podName)
}

// createVolumeSpec creates and returns a mutable volume.Spec object for the
// specified volume. It dereference any PVC to get PV objects, if needed.
// Returns an error if unable to obtain the volume at this time.
func (dswp *desiredStateOfWorldPopulator) createVolumeSpec(
	logger klog.Logger, podVolume v1.Volume, pod *v1.Pod, mounts, devices sets.Set[string]) (*v1.PersistentVolumeClaim, *volume.Spec, string, error) {
	pvcSource := podVolume.VolumeSource.PersistentVolumeClaim
	isEphemeral := pvcSource == nil && podVolume.VolumeSource.Ephemeral != nil
	if isEphemeral {
		// Generic ephemeral inline volumes are handled the
		// same way as a PVC reference. The only additional
		// constraint (checked below) is that the PVC must be
		// owned by the pod.
		pvcSource = &v1.PersistentVolumeClaimVolumeSource{
			ClaimName: ephemeral.VolumeClaimName(pod, &podVolume),
		}
	}
	if pvcSource != nil {
		logger.V(5).Info("Found PVC", "PVC", klog.KRef(pod.Namespace, pvcSource.ClaimName))
		// If podVolume is a PVC, fetch the real PV behind the claim
		pvc, err := dswp.getPVCExtractPV(
			pod.Namespace, pvcSource.ClaimName)
		if err != nil {
			return nil, nil, "", fmt.Errorf(
				"error processing PVC %s/%s: %v",
				pod.Namespace,
				pvcSource.ClaimName,
				err)
		}
		if isEphemeral {
			if err := ephemeral.VolumeIsForPod(pod, pvc); err != nil {
				return nil, nil, "", err
			}
		}
		pvName, pvcUID := pvc.Spec.VolumeName, pvc.UID
		logger.V(5).Info("Found bound PV for PVC", "PVC", klog.KRef(pod.Namespace, pvcSource.ClaimName), "PVCUID", pvcUID, "PVName", pvName)
		// Fetch actual PV object
		volumeSpec, volumeGIDValue, err :=
			dswp.getPVSpec(pvName, pvcSource.ReadOnly, pvcUID)
		if err != nil {
			return nil, nil, "", fmt.Errorf(
				"error processing PVC %s/%s: %v",
				pod.Namespace,
				pvcSource.ClaimName,
				err)
		}
		logger.V(5).Info("Extracted volumeSpec from bound PV and PVC", "PVC", klog.KRef(pod.Namespace, pvcSource.ClaimName), "PVCUID", pvcUID, "PVName", pvName, "volumeSpecName", volumeSpec.Name())
		migratable, err := dswp.csiMigratedPluginManager.IsMigratable(volumeSpec)
		if err != nil {
			return nil, nil, "", err
		}
		if migratable {
			volumeSpec, err = csimigration.TranslateInTreeSpecToCSI(logger, volumeSpec, pod.Namespace, dswp.intreeToCSITranslator)
			if err != nil {
				return nil, nil, "", err
			}
		}

		volumeMode, err := util.GetVolumeMode(volumeSpec)
		if err != nil {
			return nil, nil, "", err
		}
		// Error if a container has volumeMounts but the volumeMode of PVC isn't Filesystem.
		if mounts.Has(podVolume.Name) && volumeMode != v1.PersistentVolumeFilesystem {
			return nil, nil, "", fmt.Errorf(
				"volume %s has volumeMode %s, but is specified in volumeMounts",
				podVolume.Name,
				volumeMode)
		}
		// Error if a container has volumeDevices but the volumeMode of PVC isn't Block
		if devices.Has(podVolume.Name) && volumeMode != v1.PersistentVolumeBlock {
			return nil, nil, "", fmt.Errorf(
				"volume %s has volumeMode %s, but is specified in volumeDevices",
				podVolume.Name,
				volumeMode)
		}
		return pvc, volumeSpec, volumeGIDValue, nil
	}

	// Do not return the original volume object, since the source could mutate it
	clonedPodVolume := podVolume.DeepCopy()

	spec := volume.NewSpecFromVolume(clonedPodVolume)
	migratable, err := dswp.csiMigratedPluginManager.IsMigratable(spec)
	if err != nil {
		return nil, nil, "", err
	}
	if migratable {
		spec, err = csimigration.TranslateInTreeSpecToCSI(logger, spec, pod.Namespace, dswp.intreeToCSITranslator)
		if err != nil {
			return nil, nil, "", err
		}
	}
	return nil, spec, "", nil
}

// getPVCExtractPV fetches the PVC object with the given namespace and name from
// the API server, checks whether PVC is being deleted, extracts the name of the PV
// it is pointing to and returns it.
// An error is returned if the PVC object's phase is not "Bound".
func (dswp *desiredStateOfWorldPopulator) getPVCExtractPV(
	namespace string, claimName string) (*v1.PersistentVolumeClaim, error) {
	pvc, err :=
		dswp.kubeClient.CoreV1().PersistentVolumeClaims(namespace).Get(context.TODO(), claimName, metav1.GetOptions{})
	if err != nil || pvc == nil {
		return nil, fmt.Errorf("failed to fetch PVC from API server: %v", err)
	}

	// Pods that uses a PVC that is being deleted must not be started.
	//
	// In case an old kubelet is running without this check or some kubelets
	// have this feature disabled, the worst that can happen is that such
	// pod is scheduled. This was the default behavior in 1.8 and earlier
	// and users should not be that surprised.
	// It should happen only in very rare case when scheduler schedules
	// a pod and user deletes a PVC that's used by it at the same time.
	if pvc.ObjectMeta.DeletionTimestamp != nil {
		return nil, errors.New("PVC is being deleted")
	}

	if pvc.Status.Phase != v1.ClaimBound {
		return nil, errors.New("PVC is not bound")
	}
	if pvc.Spec.VolumeName == "" {
		return nil, errors.New("PVC has empty pvc.Spec.VolumeName")
	}

	return pvc, nil
}

// getPVSpec fetches the PV object with the given name from the API server
// and returns a volume.Spec representing it.
// An error is returned if the call to fetch the PV object fails.
func (dswp *desiredStateOfWorldPopulator) getPVSpec(
	name string,
	pvcReadOnly bool,
	expectedClaimUID types.UID) (*volume.Spec, string, error) {
	pv, err := dswp.kubeClient.CoreV1().PersistentVolumes().Get(context.TODO(), name, metav1.GetOptions{})
	if err != nil || pv == nil {
		return nil, "", fmt.Errorf(
			"failed to fetch PV %s from API server: %v", name, err)
	}

	if pv.Spec.ClaimRef == nil {
		return nil, "", fmt.Errorf(
			"found PV object %s but it has a nil pv.Spec.ClaimRef indicating it is not yet bound to the claim",
			name)
	}

	if pv.Spec.ClaimRef.UID != expectedClaimUID {
		return nil, "", fmt.Errorf(
			"found PV object %s but its pv.Spec.ClaimRef.UID %s does not point to claim.UID %s",
			name,
			pv.Spec.ClaimRef.UID,
			expectedClaimUID)
	}

	volumeGIDValue := getPVVolumeGidAnnotationValue(pv)
	return volume.NewSpecFromPersistentVolume(pv, pvcReadOnly), volumeGIDValue, nil
}

func getPVVolumeGidAnnotationValue(pv *v1.PersistentVolume) string {
	if volumeGid, ok := pv.Annotations[util.VolumeGidAnnotationKey]; ok {
		return volumeGid
	}

	return ""
}
