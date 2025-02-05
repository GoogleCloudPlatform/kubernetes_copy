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

package kuberuntime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"go.opentelemetry.io/otel/trace"
	"k8s.io/apimachinery/pkg/types"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	internalapi "k8s.io/cri-api/pkg/apis"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
	"k8s.io/klog/v2"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
)

// containerGC is the manager of garbage collection.
type containerGC struct {
	client           internalapi.RuntimeService
	manager          *kubeGenericRuntimeManager
	podStateProvider podStateProvider
	tracer           trace.Tracer
}

// NewContainerGC creates a new containerGC.
func newContainerGC(client internalapi.RuntimeService, podStateProvider podStateProvider, manager *kubeGenericRuntimeManager, tracer trace.Tracer) *containerGC {
	return &containerGC{
		client:           client,
		manager:          manager,
		podStateProvider: podStateProvider,
		tracer:           tracer,
	}
}

// containerGCInfo is the internal information kept for containers being considered for GC.
type containerGCInfo struct {
	// The ID of the container.
	id string
	// The name of the container.
	name string
	// Creation time for the container.
	createTime time.Time
	// If true, the container is in unknown state. Garbage collector should try
	// to stop containers before removal.
	unknown bool
}

// sandboxGCInfo is the internal information kept for sandboxes being considered for GC.
type sandboxGCInfo struct {
	// The ID of the sandbox.
	id string
	// Creation time for the sandbox.
	createTime time.Time
	// If true, the sandbox is ready or still has containers.
	active bool
}

// evictUnit is considered for eviction as units of (UID, container name) pair.
type evictUnit struct {
	// UID of the pod.
	uid types.UID
	// Name of the container in the pod.
	name string
}

type containersByEvictUnit map[evictUnit][]containerGCInfo
type sandboxesByPodUID map[types.UID][]sandboxGCInfo

// NumContainers returns the number of containers in this map.
func (cu containersByEvictUnit) NumContainers() int {
	num := 0
	for key := range cu {
		num += len(cu[key])
	}
	return num
}

// NumEvictUnits returns the number of pod in this map.
func (cu containersByEvictUnit) NumEvictUnits() int {
	return len(cu)
}

// Newest first.
type byCreated []containerGCInfo

func (a byCreated) Len() int           { return len(a) }
func (a byCreated) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }
func (a byCreated) Less(i, j int) bool { return a[i].createTime.After(a[j].createTime) }

// Newest first.
type sandboxByCreated []sandboxGCInfo

func (a sandboxByCreated) Len() int           { return len(a) }
func (a sandboxByCreated) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }
func (a sandboxByCreated) Less(i, j int) bool { return a[i].createTime.After(a[j].createTime) }

// retainLatestContainerPerEvictUnit ensures only the most recent container is kept per evictUnit,
// removing all older containers.
func (cgc *containerGC) retainLatestContainerPerEvictUnit(ctx context.Context, evictUnits containersByEvictUnit) {
	for key := range evictUnits {
		toRemove := len(evictUnits[key]) - 1

		if toRemove > 0 {
			evictUnits[key] = cgc.removeOldestN(ctx, evictUnits[key], toRemove)
		}
	}
}

// removeOldestN removes the oldest toRemove containers and returns the resulting slice.
func (cgc *containerGC) removeOldestN(ctx context.Context, containers []containerGCInfo, toRemove int) []containerGCInfo {
	// Remove from oldest to newest (last to first).
	numToKeep := len(containers) - toRemove
	if numToKeep > 0 {
		sort.Sort(byCreated(containers))
	}
	for i := len(containers) - 1; i >= numToKeep; i-- {
		if containers[i].unknown {
			// Containers in known state could be running, we should try
			// to stop it before removal.
			id := kubecontainer.ContainerID{
				Type: cgc.manager.runtimeName,
				ID:   containers[i].id,
			}
			message := "Container is in unknown state, try killing it before removal"
			if err := cgc.manager.killContainer(ctx, nil, id, containers[i].name, message, reasonUnknown, nil, nil); err != nil {
				klog.ErrorS(err, "Failed to stop container", "containerID", containers[i].id)
				continue
			}
		}
		if err := cgc.manager.removeContainer(ctx, containers[i].id); err != nil {
			klog.ErrorS(err, "Failed to remove container", "containerID", containers[i].id)
		}
	}

	// Assume we removed the containers so that we're not too aggressive.
	return containers[:numToKeep]
}

// removeOldestNSandboxes removes the oldest inactive toRemove sandboxes and
// returns the resulting slice.
func (cgc *containerGC) removeOldestNSandboxes(ctx context.Context, sandboxes []sandboxGCInfo, toRemove int) {
	numToKeep := len(sandboxes) - toRemove
	if numToKeep > 0 {
		sort.Sort(sandboxByCreated(sandboxes))
	}
	// Remove from oldest to newest (last to first).
	for i := len(sandboxes) - 1; i >= numToKeep; i-- {
		if !sandboxes[i].active {
			cgc.removeSandbox(ctx, sandboxes[i].id)
		}
	}
}

// removeSandbox removes the sandbox by sandboxID.
func (cgc *containerGC) removeSandbox(ctx context.Context, sandboxID string) {
	klog.V(4).InfoS("Removing sandbox", "sandboxID", sandboxID)
	// In normal cases, kubelet should've already called StopPodSandbox before
	// GC kicks in. To guard against the rare cases where this is not true, try
	// stopping the sandbox before removing it.
	if err := cgc.client.StopPodSandbox(ctx, sandboxID); err != nil {
		klog.ErrorS(err, "Failed to stop sandbox before removing", "sandboxID", sandboxID)
		return
	}
	if err := cgc.client.RemovePodSandbox(ctx, sandboxID); err != nil {
		klog.ErrorS(err, "Failed to remove sandbox", "sandboxID", sandboxID)
	}
}

// evictableContainers gets all containers that are evictable. Evictable containers are: not running.
func (cgc *containerGC) evictableContainers(ctx context.Context) (containersByEvictUnit, error) {
	containers, err := cgc.manager.getKubeletContainers(ctx, true)
	if err != nil {
		return containersByEvictUnit{}, err
	}

	evictUnits := make(containersByEvictUnit)
	for _, container := range containers {
		// Prune out running containers.
		if container.State == runtimeapi.ContainerState_CONTAINER_RUNNING {
			continue
		}

		labeledInfo := getContainerInfoFromLabels(container.Labels)
		containerInfo := containerGCInfo{
			id:         container.Id,
			name:       container.Metadata.Name,
			createTime: time.Unix(0, container.CreatedAt),
			unknown:    container.State == runtimeapi.ContainerState_CONTAINER_UNKNOWN,
		}
		key := evictUnit{
			uid:  labeledInfo.PodUID,
			name: containerInfo.name,
		}
		evictUnits[key] = append(evictUnits[key], containerInfo)
	}

	return evictUnits, nil
}

// evict all containers that are evictable
func (cgc *containerGC) evictContainers(ctx context.Context, allSourcesReady bool, evictNonDeletedPods bool) error {
	// Separate containers by evict units.
	evictUnits, err := cgc.evictableContainers(ctx)
	if err != nil {
		return err
	}

	// Remove deleted pod containers if all sources are ready.
	if allSourcesReady {
		for key, unit := range evictUnits {
			if cgc.podStateProvider.ShouldPodContentBeRemoved(key.uid) || (evictNonDeletedPods && cgc.podStateProvider.ShouldPodRuntimeBeRemoved(key.uid)) {
				cgc.removeOldestN(ctx, unit, len(unit)) // Remove all.
				delete(evictUnits, key)
			}
		}
	}

	// only the most recent container is kept per evictUnit
	cgc.retainLatestContainerPerEvictUnit(ctx, evictUnits)

	return nil
}

// evictSandboxes remove all evictable sandboxes. An evictable sandbox must
// meet the following requirements:
//  1. not in ready state
//  2. contains no containers.
//  3. belong to a non-existent (i.e., already removed) pod, or is not the
//     most recently created sandbox for the pod.
func (cgc *containerGC) evictSandboxes(ctx context.Context, evictNonDeletedPods bool) error {
	containers, err := cgc.manager.getKubeletContainers(ctx, true)
	if err != nil {
		return err
	}

	sandboxes, err := cgc.manager.getKubeletSandboxes(ctx, true)
	if err != nil {
		return err
	}

	// collect all the PodSandboxId of container
	sandboxIDs := sets.New[string]()
	for _, container := range containers {
		sandboxIDs.Insert(container.PodSandboxId)
	}

	sandboxesByPod := make(sandboxesByPodUID, len(sandboxes))
	for _, sandbox := range sandboxes {
		podUID := types.UID(sandbox.Metadata.Uid)
		sandboxInfo := sandboxGCInfo{
			id:         sandbox.Id,
			createTime: time.Unix(0, sandbox.CreatedAt),
		}

		// Set ready sandboxes and sandboxes that still have containers to be active.
		if sandbox.State == runtimeapi.PodSandboxState_SANDBOX_READY || sandboxIDs.Has(sandbox.Id) {
			sandboxInfo.active = true
		}

		sandboxesByPod[podUID] = append(sandboxesByPod[podUID], sandboxInfo)
	}

	for podUID, sandboxes := range sandboxesByPod {
		if cgc.podStateProvider.ShouldPodContentBeRemoved(podUID) || (evictNonDeletedPods && cgc.podStateProvider.ShouldPodRuntimeBeRemoved(podUID)) {
			// Remove all evictable sandboxes if the pod has been removed.
			// Note that the latest dead sandbox is also removed if there is
			// already an active one.
			cgc.removeOldestNSandboxes(ctx, sandboxes, len(sandboxes))
		} else {
			// Keep latest one if the pod still exists.
			cgc.removeOldestNSandboxes(ctx, sandboxes, len(sandboxes)-1)
		}
	}
	return nil
}

// evictPodLogsDirectories evicts all evictable pod logs directories. Pod logs directories
// are evictable if there are no corresponding pods.
func (cgc *containerGC) evictPodLogsDirectories(ctx context.Context, allSourcesReady bool) error {
	osInterface := cgc.manager.osInterface
	podLogsDirectory := cgc.manager.podLogsDirectory
	if allSourcesReady {
		// Only remove pod logs directories when all sources are ready.
		dirs, err := osInterface.ReadDir(podLogsDirectory)
		if err != nil {
			return fmt.Errorf("failed to read podLogsDirectory %q: %w", podLogsDirectory, err)
		}
		for _, dir := range dirs {
			name := dir.Name()
			podUID := parsePodUIDFromLogsDirectory(name)
			if !cgc.podStateProvider.ShouldPodContentBeRemoved(podUID) {
				continue
			}
			klog.V(4).InfoS("Removing pod logs", "podUID", podUID)
			err := osInterface.RemoveAll(filepath.Join(podLogsDirectory, name))
			if err != nil {
				klog.ErrorS(err, "Failed to remove pod logs directory", "path", name)
			}
		}
	}

	// Remove dead container log symlinks.
	// TODO(random-liu): Remove this after cluster logging supports CRI container log path.
	logSymlinks, _ := osInterface.Glob(filepath.Join(legacyContainerLogsDir, fmt.Sprintf("*.%s", legacyLogSuffix)))
	for _, logSymlink := range logSymlinks {
		if _, err := osInterface.Stat(logSymlink); os.IsNotExist(err) {
			if containerID, err := getContainerIDFromLegacyLogSymlink(logSymlink); err == nil {
				resp, err := cgc.manager.runtimeService.ContainerStatus(ctx, containerID, false)
				if err != nil {
					// TODO: we should handle container not found (i.e. container was deleted) case differently
					// once https://github.com/kubernetes/kubernetes/issues/63336 is resolved
					klog.InfoS("Error getting ContainerStatus for containerID", "containerID", containerID, "err", err)
				} else {
					status := resp.GetStatus()
					if status == nil {
						klog.V(4).InfoS("Container status is nil")
						continue
					}
					if status.State != runtimeapi.ContainerState_CONTAINER_EXITED {
						// Here is how container log rotation works (see containerLogManager#rotateLatestLog):
						//
						// 1. rename current log to rotated log file whose filename contains current timestamp (fmt.Sprintf("%s.%s", log, timestamp))
						// 2. reopen the container log
						// 3. if #2 fails, rename rotated log file back to container log
						//
						// There is small but indeterministic amount of time during which log file doesn't exist (between steps #1 and #2, between #1 and #3).
						// Hence the symlink may be deemed unhealthy during that period.
						// See https://github.com/kubernetes/kubernetes/issues/52172
						//
						// We only remove unhealthy symlink for dead containers
						klog.V(5).InfoS("Container is still running, not removing symlink", "containerID", containerID, "path", logSymlink)
						continue
					}
				}
			} else {
				klog.V(4).InfoS("Unable to obtain container ID", "err", err)
			}
			err := osInterface.Remove(logSymlink)
			if err != nil {
				klog.ErrorS(err, "Failed to remove container log dead symlink", "path", logSymlink)
			} else {
				klog.V(4).InfoS("Removed symlink", "path", logSymlink)
			}
		}
	}
	return nil
}

// GarbageCollect removes dead containers using the specified container gc policy.
// Note that gc policy is not applied to sandboxes. Sandboxes are only removed when they are
// not ready and containing no containers.
//
// GarbageCollect consists of the following steps:
// * gets evictable containers which are not active and created more than gcPolicy.MinAge ago.
// * removes oldest dead containers for each pod by enforcing gcPolicy.MaxPerPodContainer.
// * removes oldest dead containers by enforcing gcPolicy.MaxContainers.
// * gets evictable sandboxes which are not ready and contains no containers.
// * removes evictable sandboxes.
func (cgc *containerGC) GarbageCollect(ctx context.Context, allSourcesReady bool, evictNonDeletedPods bool) error {
	ctx, otelSpan := cgc.tracer.Start(ctx, "Containers/GarbageCollect")
	defer otelSpan.End()
	errors := []error{}
	// Remove evictable containers
	if err := cgc.evictContainers(ctx, allSourcesReady, evictNonDeletedPods); err != nil {
		errors = append(errors, err)
	}

	// Remove sandboxes with zero containers
	if err := cgc.evictSandboxes(ctx, evictNonDeletedPods); err != nil {
		errors = append(errors, err)
	}

	// Remove pod sandbox log directory
	if err := cgc.evictPodLogsDirectories(ctx, allSourcesReady); err != nil {
		errors = append(errors, err)
	}
	return utilerrors.NewAggregate(errors)
}
