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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	cadvisorapi "github.com/google/cadvisor/info/v1"
	"go.opentelemetry.io/otel/trace"
	grpcstatus "google.golang.org/grpc/status"
	crierror "k8s.io/cri-api/pkg/errors"
	"k8s.io/klog/v2"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubetypes "k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	utilversion "k8s.io/apimachinery/pkg/util/version"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/tools/record"
	ref "k8s.io/client-go/tools/reference"
	"k8s.io/client-go/util/flowcontrol"
	"k8s.io/component-base/logs/logreduction"
	internalapi "k8s.io/cri-api/pkg/apis"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"

	"k8s.io/kubernetes/pkg/api/legacyscheme"
	podutil "k8s.io/kubernetes/pkg/api/v1/pod"
	"k8s.io/kubernetes/pkg/credentialprovider"
	"k8s.io/kubernetes/pkg/credentialprovider/plugin"
	"k8s.io/kubernetes/pkg/features"
	"k8s.io/kubernetes/pkg/kubelet/cm"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	"k8s.io/kubernetes/pkg/kubelet/events"
	"k8s.io/kubernetes/pkg/kubelet/images"
	runtimeutil "k8s.io/kubernetes/pkg/kubelet/kuberuntime/util"
	"k8s.io/kubernetes/pkg/kubelet/lifecycle"
	"k8s.io/kubernetes/pkg/kubelet/logs"
	"k8s.io/kubernetes/pkg/kubelet/metrics"
	"k8s.io/kubernetes/pkg/kubelet/pleg"
	proberesults "k8s.io/kubernetes/pkg/kubelet/prober/results"
	"k8s.io/kubernetes/pkg/kubelet/runtimeclass"
	"k8s.io/kubernetes/pkg/kubelet/sysctl"
	"k8s.io/kubernetes/pkg/kubelet/types"
	"k8s.io/kubernetes/pkg/kubelet/util/cache"
	"k8s.io/kubernetes/pkg/kubelet/util/format"
	sc "k8s.io/kubernetes/pkg/securitycontext"
)

const (
	// The api version of kubelet runtime api
	kubeRuntimeAPIVersion = "0.1.0"
	// A minimal shutdown window for avoiding unnecessary SIGKILLs
	minimumGracePeriodInSeconds = 2

	// The expiration time of version cache.
	versionCacheTTL = 60 * time.Second
	// How frequently to report identical errors
	identicalErrorDelay = 1 * time.Minute
	// OpenTelemetry instrumentation scope name
	instrumentationScope = "k8s.io/kubernetes/pkg/kubelet/kuberuntime"
)

var (
	// ErrVersionNotSupported is returned when the api version of runtime interface is not supported
	ErrVersionNotSupported = errors.New("runtime api version is not supported")
)

// podStateProvider can determine if none of the elements are necessary to retain (pod content)
// or if none of the runtime elements are necessary to retain (containers)
type podStateProvider interface {
	IsPodTerminationRequested(kubetypes.UID) bool
	ShouldPodContentBeRemoved(kubetypes.UID) bool
	ShouldPodRuntimeBeRemoved(kubetypes.UID) bool
}

type kubeGenericRuntimeManager struct {
	runtimeName string
	recorder    record.EventRecorder
	osInterface kubecontainer.OSInterface

	// machineInfo contains the machine information.
	machineInfo *cadvisorapi.MachineInfo

	// Container GC manager
	containerGC *containerGC

	// Keyring for pulling images
	keyring credentialprovider.DockerKeyring

	// Runner of lifecycle events.
	runner kubecontainer.HandlerRunner

	// RuntimeHelper that wraps kubelet to generate runtime container options.
	runtimeHelper kubecontainer.RuntimeHelper

	// Health check results.
	livenessManager  proberesults.Manager
	readinessManager proberesults.Manager
	startupManager   proberesults.Manager

	// If false, pass "memory.oom.group" to container cgroups when using cgroups v2 to cause processes
	// in those cgroups to be killed as a unit by the OOM killer.
	// It must be nil except for linux
	singleProcessOOMKill *bool

	// If true, enforce container cpu limits with CFS quota support
	cpuCFSQuota bool

	// CPUCFSQuotaPeriod sets the CPU CFS quota period value, cpu.cfs_period_us, defaults to 100ms
	cpuCFSQuotaPeriod metav1.Duration

	// wrapped image puller.
	imagePuller images.ImageManager

	// gRPC service clients
	runtimeService internalapi.RuntimeService
	imageService   internalapi.ImageManagerService

	// The version cache of runtime daemon.
	versionCache *cache.ObjectCache

	// The directory path for seccomp profiles.
	seccompProfileRoot string

	// Container management interface for pod container.
	containerManager cm.ContainerManager

	// Internal lifecycle event handlers for container resource management.
	internalLifecycle cm.InternalContainerLifecycle

	// Manage container logs.
	logManager logs.ContainerLogManager

	// Manage RuntimeClass resources.
	runtimeClassManager *runtimeclass.Manager

	// Cache last per-container error message to reduce log spam
	logReduction *logreduction.LogReduction

	// PodState provider instance
	podStateProvider podStateProvider

	// Use RuntimeDefault as the default seccomp profile for all workloads.
	seccompDefault bool

	// MemorySwapBehavior defines how swap is used
	memorySwapBehavior string

	//Function to get node allocatable resources
	getNodeAllocatable func() v1.ResourceList

	// Memory throttling factor for MemoryQoS
	memoryThrottlingFactor float64

	// Root directory used to store pod logs
	podLogsDirectory string
}

// KubeGenericRuntime is a interface contains interfaces for container runtime and command.
type KubeGenericRuntime interface {
	kubecontainer.Runtime
	kubecontainer.StreamingRuntime
	kubecontainer.CommandRunner
}

// NewKubeGenericRuntimeManager creates a new kubeGenericRuntimeManager
func NewKubeGenericRuntimeManager(
	recorder record.EventRecorder,
	livenessManager proberesults.Manager,
	readinessManager proberesults.Manager,
	startupManager proberesults.Manager,
	rootDirectory string,
	podLogsDirectory string,
	machineInfo *cadvisorapi.MachineInfo,
	podStateProvider podStateProvider,
	osInterface kubecontainer.OSInterface,
	runtimeHelper kubecontainer.RuntimeHelper,
	insecureContainerLifecycleHTTPClient types.HTTPDoer,
	imageBackOff *flowcontrol.Backoff,
	serializeImagePulls bool,
	maxParallelImagePulls *int32,
	imagePullQPS float32,
	imagePullBurst int,
	imageCredentialProviderConfigFile string,
	imageCredentialProviderBinDir string,
	singleProcessOOMKill *bool,
	cpuCFSQuota bool,
	cpuCFSQuotaPeriod metav1.Duration,
	runtimeService internalapi.RuntimeService,
	imageService internalapi.ImageManagerService,
	containerManager cm.ContainerManager,
	logManager logs.ContainerLogManager,
	runtimeClassManager *runtimeclass.Manager,
	seccompDefault bool,
	memorySwapBehavior string,
	getNodeAllocatable func() v1.ResourceList,
	memoryThrottlingFactor float64,
	podPullingTimeRecorder images.ImagePodPullingTimeRecorder,
	tracerProvider trace.TracerProvider,
) (KubeGenericRuntime, error) {
	ctx := context.Background()
	runtimeService = newInstrumentedRuntimeService(runtimeService)
	imageService = newInstrumentedImageManagerService(imageService)
	tracer := tracerProvider.Tracer(instrumentationScope)
	kubeRuntimeManager := &kubeGenericRuntimeManager{
		recorder:               recorder,
		singleProcessOOMKill:   singleProcessOOMKill,
		cpuCFSQuota:            cpuCFSQuota,
		cpuCFSQuotaPeriod:      cpuCFSQuotaPeriod,
		seccompProfileRoot:     filepath.Join(rootDirectory, "seccomp"),
		livenessManager:        livenessManager,
		readinessManager:       readinessManager,
		startupManager:         startupManager,
		machineInfo:            machineInfo,
		osInterface:            osInterface,
		runtimeHelper:          runtimeHelper,
		runtimeService:         runtimeService,
		imageService:           imageService,
		containerManager:       containerManager,
		internalLifecycle:      containerManager.InternalContainerLifecycle(),
		logManager:             logManager,
		runtimeClassManager:    runtimeClassManager,
		logReduction:           logreduction.NewLogReduction(identicalErrorDelay),
		seccompDefault:         seccompDefault,
		memorySwapBehavior:     memorySwapBehavior,
		getNodeAllocatable:     getNodeAllocatable,
		memoryThrottlingFactor: memoryThrottlingFactor,
		podLogsDirectory:       podLogsDirectory,
	}

	typedVersion, err := kubeRuntimeManager.getTypedVersion(ctx)
	if err != nil {
		klog.ErrorS(err, "Get runtime version failed")
		return nil, err
	}

	// Only matching kubeRuntimeAPIVersion is supported now
	// TODO: Runtime API machinery is under discussion at https://github.com/kubernetes/kubernetes/issues/28642
	if typedVersion.Version != kubeRuntimeAPIVersion {
		klog.ErrorS(err, "This runtime api version is not supported",
			"apiVersion", typedVersion.Version,
			"supportedAPIVersion", kubeRuntimeAPIVersion)
		return nil, ErrVersionNotSupported
	}

	kubeRuntimeManager.runtimeName = typedVersion.RuntimeName
	klog.InfoS("Container runtime initialized",
		"containerRuntime", typedVersion.RuntimeName,
		"version", typedVersion.RuntimeVersion,
		"apiVersion", typedVersion.RuntimeApiVersion)

	if imageCredentialProviderConfigFile != "" || imageCredentialProviderBinDir != "" {
		if err := plugin.RegisterCredentialProviderPlugins(imageCredentialProviderConfigFile, imageCredentialProviderBinDir); err != nil {
			klog.ErrorS(err, "Failed to register CRI auth plugins")
			os.Exit(1)
		}
	}
	kubeRuntimeManager.keyring = credentialprovider.NewDockerKeyring()

	kubeRuntimeManager.imagePuller = images.NewImageManager(
		kubecontainer.FilterEventRecorder(recorder),
		kubeRuntimeManager,
		imageBackOff,
		serializeImagePulls,
		maxParallelImagePulls,
		imagePullQPS,
		imagePullBurst,
		podPullingTimeRecorder)
	kubeRuntimeManager.runner = lifecycle.NewHandlerRunner(insecureContainerLifecycleHTTPClient, kubeRuntimeManager, kubeRuntimeManager, recorder)
	kubeRuntimeManager.containerGC = newContainerGC(runtimeService, podStateProvider, kubeRuntimeManager, tracer)
	kubeRuntimeManager.podStateProvider = podStateProvider

	kubeRuntimeManager.versionCache = cache.NewObjectCache(
		func() (interface{}, error) {
			return kubeRuntimeManager.getTypedVersion(ctx)
		},
		versionCacheTTL,
	)

	return kubeRuntimeManager, nil
}

// Type returns the type of the container runtime.
func (m *kubeGenericRuntimeManager) Type() string {
	return m.runtimeName
}

func newRuntimeVersion(version string) (*utilversion.Version, error) {
	if ver, err := utilversion.ParseSemantic(version); err == nil {
		return ver, err
	}
	return utilversion.ParseGeneric(version)
}

func (m *kubeGenericRuntimeManager) getTypedVersion(ctx context.Context) (*runtimeapi.VersionResponse, error) {
	typedVersion, err := m.runtimeService.Version(ctx, kubeRuntimeAPIVersion)
	if err != nil {
		return nil, fmt.Errorf("get remote runtime typed version failed: %v", err)
	}
	return typedVersion, nil
}

// Version returns the version information of the container runtime.
func (m *kubeGenericRuntimeManager) Version(ctx context.Context) (kubecontainer.Version, error) {
	typedVersion, err := m.getTypedVersion(ctx)
	if err != nil {
		return nil, err
	}

	return newRuntimeVersion(typedVersion.RuntimeVersion)
}

// APIVersion returns the cached API version information of the container
// runtime. Implementation is expected to update this cache periodically.
// This may be different from the runtime engine's version.
func (m *kubeGenericRuntimeManager) APIVersion() (kubecontainer.Version, error) {
	versionObject, err := m.versionCache.Get(m.machineInfo.MachineID)
	if err != nil {
		return nil, err
	}
	typedVersion := versionObject.(*runtimeapi.VersionResponse)

	return newRuntimeVersion(typedVersion.RuntimeApiVersion)
}

// Status returns the status of the runtime. An error is returned if the Status
// function itself fails, nil otherwise.
func (m *kubeGenericRuntimeManager) Status(ctx context.Context) (*kubecontainer.RuntimeStatus, error) {
	resp, err := m.runtimeService.Status(ctx, false)
	if err != nil {
		return nil, err
	}
	if resp.GetStatus() == nil {
		return nil, errors.New("runtime status is nil")
	}
	return toKubeRuntimeStatus(resp.GetStatus(), resp.GetRuntimeHandlers(), resp.GetFeatures()), nil
}

// GetPods returns a list of containers grouped by pods. The boolean parameter
// specifies whether the runtime returns all containers including those already
// exited and dead containers (used for garbage collection).
func (m *kubeGenericRuntimeManager) GetPods(ctx context.Context, all bool) ([]*kubecontainer.Pod, error) {
	pods := make(map[kubetypes.UID]*kubecontainer.Pod)
	sandboxes, err := m.getKubeletSandboxes(ctx, all)
	if err != nil {
		return nil, err
	}
	for i := range sandboxes {
		s := sandboxes[i]
		if s.Metadata == nil {
			klog.V(4).InfoS("Sandbox does not have metadata", "sandbox", s)
			continue
		}
		podUID := kubetypes.UID(s.Metadata.Uid)
		if _, ok := pods[podUID]; !ok {
			pods[podUID] = &kubecontainer.Pod{
				ID:        podUID,
				Name:      s.Metadata.Name,
				Namespace: s.Metadata.Namespace,
			}
		}
		p := pods[podUID]
		converted, err := m.sandboxToKubeContainer(s)
		if err != nil {
			klog.V(4).InfoS("Convert sandbox of pod failed", "runtimeName", m.runtimeName, "sandbox", s, "podUID", podUID, "err", err)
			continue
		}
		p.Sandboxes = append(p.Sandboxes, converted)
		p.CreatedAt = uint64(s.GetCreatedAt())
	}

	containers, err := m.getKubeletContainers(ctx, all)
	if err != nil {
		return nil, err
	}
	for i := range containers {
		c := containers[i]
		if c.Metadata == nil {
			klog.V(4).InfoS("Container does not have metadata", "container", c)
			continue
		}

		labelledInfo := getContainerInfoFromLabels(c.Labels)
		pod, found := pods[labelledInfo.PodUID]
		if !found {
			pod = &kubecontainer.Pod{
				ID:        labelledInfo.PodUID,
				Name:      labelledInfo.PodName,
				Namespace: labelledInfo.PodNamespace,
			}
			pods[labelledInfo.PodUID] = pod
		}

		converted, err := m.toKubeContainer(c)
		if err != nil {
			klog.V(4).InfoS("Convert container of pod failed", "runtimeName", m.runtimeName, "container", c, "podUID", labelledInfo.PodUID, "err", err)
			continue
		}

		pod.Containers = append(pod.Containers, converted)
	}

	// Convert map to list.
	var result []*kubecontainer.Pod
	for _, pod := range pods {
		result = append(result, pod)
	}

	// There are scenarios where multiple pods are running in parallel having
	// the same name, because one of them have not been fully terminated yet.
	// To avoid unexpected behavior on container name based search (for example
	// by calling *Kubelet.findContainer() without specifying a pod ID), we now
	// return the list of pods ordered by their creation time.
	sort.SliceStable(result, func(i, j int) bool {
		return result[i].CreatedAt > result[j].CreatedAt
	})
	klog.V(4).InfoS("Retrieved pods from runtime", "all", all)
	return result, nil
}

// containerKillReason explains what killed a given container
type containerKillReason string

const (
	reasonStartupProbe        containerKillReason = "StartupProbe"
	reasonLivenessProbe       containerKillReason = "LivenessProbe"
	reasonFailedPostStartHook containerKillReason = "FailedPostStartHook"
	reasonUnknown             containerKillReason = "Unknown"
)

// containerToKillInfo contains necessary information to kill a container.
type containerToKillInfo struct {
	// The spec of the container.
	container *v1.Container
	// The name of the container.
	name string
	// The message indicates why the container will be killed.
	message string
	// The reason is a clearer source of info on why a container will be killed
	// TODO: replace message with reason?
	reason containerKillReason
}

// containerResources holds the set of resources applicable to the running container
type containerResources struct {
	memoryLimit   int64
	memoryRequest int64
	cpuLimit      int64
	cpuRequest    int64
}

// containerToUpdateInfo contains necessary information to update a container's resources.
type containerToUpdateInfo struct {
	// The spec of the container.
	container *v1.Container
	// ID of the runtime container that needs resource update
	kubeContainerID kubecontainer.ContainerID
	// Desired resources for the running container
	desiredContainerResources containerResources
	// Most recently configured resources on the running container
	currentContainerResources *containerResources
}

// podActions keeps information what to do for a pod.
type podActions struct {
	// Stop all running (regular, init and ephemeral) containers and the sandbox for the pod.
	KillPod bool
	// Whether need to create a new sandbox. If needed to kill pod and create
	// a new pod sandbox, all init containers need to be purged (i.e., removed).
	CreateSandbox bool
	// The id of existing sandbox. It is used for starting containers in ContainersToStart.
	SandboxID string
	// The attempt number of creating sandboxes for the pod.
	Attempt uint32

	// The next init container to start.
	// TODO: Either this or InitContainersToStart will be used. Remove this
	// field once it is not needed.
	NextInitContainerToStart *v1.Container
	// InitContainersToStart keeps a list of indexes for the init containers to
	// start, where the index is the index of the specific init container in the
	// pod spec (pod.Spec.InitContainers).
	// NOTE: This is a field for SidecarContainers feature. Either this or
	// NextInitContainerToStart will be set.
	InitContainersToStart []int
	// ContainersToStart keeps a list of indexes for the containers to start,
	// where the index is the index of the specific container in the pod spec (
	// pod.Spec.Containers).
	ContainersToStart []int
	// ContainersToKill keeps a map of containers that need to be killed, note that
	// the key is the container ID of the container, while
	// the value contains necessary information to kill a container.
	ContainersToKill map[kubecontainer.ContainerID]containerToKillInfo
	// EphemeralContainersToStart is a list of indexes for the ephemeral containers to start,
	// where the index is the index of the specific container in pod.Spec.EphemeralContainers.
	EphemeralContainersToStart []int
	// ContainersToUpdate keeps a list of containers needing resource update.
	// Container resource update is applicable only for CPU and memory.
	ContainersToUpdate map[v1.ResourceName][]containerToUpdateInfo
	// UpdatePodResources is true if container(s) need resource update with restart
	UpdatePodResources bool
}

func (p podActions) String() string {
	return fmt.Sprintf("KillPod: %t, CreateSandbox: %t, UpdatePodResources: %t, Attempt: %d, InitContainersToStart: %v, ContainersToStart: %v, EphemeralContainersToStart: %v,ContainersToUpdate: %v, ContainersToKill: %v",
		p.KillPod, p.CreateSandbox, p.UpdatePodResources, p.Attempt, p.InitContainersToStart, p.ContainersToStart, p.EphemeralContainersToStart, p.ContainersToUpdate, p.ContainersToKill)
}

// containerChanged will determine whether the container has changed based on the fields that will affect the running of the container.
// Currently, there are only `image` and `name` fields.
// we don't need to consider the pod UID here, because we find the containerStatus through the pod UID.
// If the pod UID changes, we will not be able to find the containerStatus to compare against.
func containerChanged(container *v1.Container, containerStatus *kubecontainer.Status) (uint64, uint64, bool) {
	expectedHash := kubecontainer.HashContainer(container)
	return expectedHash, containerStatus.Hash, containerStatus.Hash != expectedHash
}

func shouldRestartOnFailure(pod *v1.Pod) bool {
	return pod.Spec.RestartPolicy != v1.RestartPolicyNever
}

func containerSucceeded(c *v1.Container, podStatus *kubecontainer.PodStatus) bool {
	cStatus := podStatus.FindContainerStatusByName(c.Name)
	if cStatus == nil {
		return false
	}
	// Container has exited, with an exit code of 0.
	return cStatus.State == kubecontainer.ContainerStateExited && cStatus.ExitCode == 0
}

func IsInPlacePodVerticalScalingAllowed(pod *v1.Pod) bool {
	if !utilfeature.DefaultFeatureGate.Enabled(features.InPlacePodVerticalScaling) {
		return false
	}
	if types.IsStaticPod(pod) {
		return false
	}
	return true
}

// computePodResizeAction determines the actions required (if any) to resize the given container.
// Returns whether to keep (true) or restart (false) the container.
// TODO(vibansal): Make this function to be agnostic to whether it is dealing with a restartable init container or not (i.e. remove the argument `isRestartableInitContainer`).
func (m *kubeGenericRuntimeManager) computePodResizeAction(pod *v1.Pod, containerIdx int, isRestartableInitContainer bool, kubeContainerStatus *kubecontainer.Status, changes *podActions) (keepContainer bool) {
	var container v1.Container
	if isRestartableInitContainer {
		container = pod.Spec.InitContainers[containerIdx]
	} else {
		container = pod.Spec.Containers[containerIdx]
	}

	// Determine if the *running* container needs resource update by comparing v1.Spec.Resources (desired)
	// with v1.Status.Resources / runtime.Status.Resources (last known actual).
	// Proceed only when kubelet has accepted the resize a.k.a v1.Spec.Resources.Requests == v1.Status.AllocatedResources.
	// Skip if runtime containerID doesn't match pod.Status containerID (container is restarting)
	if kubeContainerStatus.State != kubecontainer.ContainerStateRunning {
		return true
	}

	if kubeContainerStatus.Resources == nil {
		// Not enough information to actuate a resize.
		klog.V(4).InfoS("Missing runtime resource information for container", "pod", klog.KObj(pod), "container", container.Name)
		return true
	}

	desiredResources := containerResources{
		memoryLimit:   container.Resources.Limits.Memory().Value(),
		memoryRequest: container.Resources.Requests.Memory().Value(),
		cpuLimit:      container.Resources.Limits.Cpu().MilliValue(),
		cpuRequest:    container.Resources.Requests.Cpu().MilliValue(),
	}

	currentResources := containerResources{
		// memoryRequest isn't set by the runtime, so default it to the desired.
		memoryRequest: desiredResources.memoryRequest,
	}
	if kubeContainerStatus.Resources.MemoryLimit != nil {
		currentResources.memoryLimit = kubeContainerStatus.Resources.MemoryLimit.Value()
	}
	if kubeContainerStatus.Resources.CPULimit != nil {
		currentResources.cpuLimit = kubeContainerStatus.Resources.CPULimit.MilliValue()
	}
	if kubeContainerStatus.Resources.CPURequest != nil {
		currentResources.cpuRequest = kubeContainerStatus.Resources.CPURequest.MilliValue()
	}
	// Note: cgroup doesn't support memory request today, so we don't compare that. If canAdmitPod called  during
	// handlePodResourcesResize finds 'fit', then desiredMemoryRequest == currentMemoryRequest.

	// Special case for minimum CPU request
	if desiredResources.cpuRequest <= cm.MinShares && currentResources.cpuRequest <= cm.MinShares {
		// If both desired & current CPU requests are at or below MinShares,
		// then consider these equal.
		desiredResources.cpuRequest = currentResources.cpuRequest
	}
	// Special case for minimum CPU limit
	if desiredResources.cpuLimit <= cm.MinMilliCPULimit && currentResources.cpuLimit <= cm.MinMilliCPULimit {
		// If both desired & current CPU limit are at or below the minimum effective limit,
		// then consider these equal.
		desiredResources.cpuLimit = currentResources.cpuLimit
	}
	if currentResources == desiredResources {
		// No resize required.
		return true
	}

	determineContainerResize := func(rName v1.ResourceName, specValue, statusValue int64) (resize, restart bool) {
		if specValue == statusValue {
			return false, false
		}
		for _, policy := range container.ResizePolicy {
			if policy.ResourceName == rName {
				return true, policy.RestartPolicy == v1.RestartContainer
			}
		}
		// If a resource policy isn't set, the implicit default is NotRequired.
		return true, false
	}
	markContainerForUpdate := func(rName v1.ResourceName, specValue, statusValue int64) {
		cUpdateInfo := containerToUpdateInfo{
			container:                 &container,
			kubeContainerID:           kubeContainerStatus.ID,
			desiredContainerResources: desiredResources,
			currentContainerResources: &currentResources,
		}
		// Order the container updates such that resource decreases are applied before increases
		switch {
		case specValue > statusValue: // append
			changes.ContainersToUpdate[rName] = append(changes.ContainersToUpdate[rName], cUpdateInfo)
		case specValue < statusValue: // prepend
			changes.ContainersToUpdate[rName] = append(changes.ContainersToUpdate[rName], containerToUpdateInfo{})
			copy(changes.ContainersToUpdate[rName][1:], changes.ContainersToUpdate[rName])
			changes.ContainersToUpdate[rName][0] = cUpdateInfo
		}
	}
	resizeMemLim, restartMemLim := determineContainerResize(v1.ResourceMemory, desiredResources.memoryLimit, currentResources.memoryLimit)
	resizeCPULim, restartCPULim := determineContainerResize(v1.ResourceCPU, desiredResources.cpuLimit, currentResources.cpuLimit)
	resizeCPUReq, restartCPUReq := determineContainerResize(v1.ResourceCPU, desiredResources.cpuRequest, currentResources.cpuRequest)
	if restartCPULim || restartCPUReq || restartMemLim {
		// resize policy requires this container to restart
		changes.ContainersToKill[kubeContainerStatus.ID] = containerToKillInfo{
			name:      kubeContainerStatus.Name,
			container: &container,
			message:   fmt.Sprintf("Container %s resize requires restart", container.Name),
		}
		if isRestartableInitContainer {
			changes.InitContainersToStart = append(changes.InitContainersToStart, containerIdx)
		} else {
			changes.ContainersToStart = append(changes.ContainersToStart, containerIdx)
		}
		changes.UpdatePodResources = true
		return false
	} else {
		if resizeMemLim {
			markContainerForUpdate(v1.ResourceMemory, desiredResources.memoryLimit, currentResources.memoryLimit)
		}
		if resizeCPULim {
			markContainerForUpdate(v1.ResourceCPU, desiredResources.cpuLimit, currentResources.cpuLimit)
		} else if resizeCPUReq {
			markContainerForUpdate(v1.ResourceCPU, desiredResources.cpuRequest, currentResources.cpuRequest)
		}
	}
	return true
}

func (m *kubeGenericRuntimeManager) doPodResizeAction(pod *v1.Pod, podContainerChanges podActions, result *kubecontainer.PodSyncResult) {
	pcm := m.containerManager.NewPodContainerManager()
	//TODO(vinaykul,InPlacePodVerticalScaling): Figure out best way to get enforceMemoryQoS value (parameter #4 below) in platform-agnostic way
	podResources := cm.ResourceConfigForPod(pod, m.cpuCFSQuota, uint64((m.cpuCFSQuotaPeriod.Duration)/time.Microsecond), false)
	if podResources == nil {
		klog.ErrorS(nil, "Unable to get resource configuration", "pod", pod.Name)
		result.Fail(fmt.Errorf("unable to get resource configuration processing resize for pod %s", pod.Name))
		return
	}
	setPodCgroupConfig := func(rName v1.ResourceName, setLimitValue bool) error {
		var err error
		switch rName {
		case v1.ResourceCPU:
			podCPUResources := &cm.ResourceConfig{}
			if setLimitValue {
				podCPUResources.CPUPeriod = podResources.CPUPeriod
				podCPUResources.CPUQuota = podResources.CPUQuota
			} else {
				podCPUResources.CPUShares = podResources.CPUShares
			}
			err = pcm.SetPodCgroupConfig(pod, podCPUResources)
		case v1.ResourceMemory:
			if !setLimitValue {
				// Memory requests aren't written to cgroups.
				return nil
			}
			podMemoryResources := &cm.ResourceConfig{
				Memory: podResources.Memory,
			}
			err = pcm.SetPodCgroupConfig(pod, podMemoryResources)
		}
		if err != nil {
			klog.ErrorS(err, "Failed to set cgroup config", "resource", rName, "pod", pod.Name)
		}
		return err
	}
	// Memory and CPU are updated separately because memory resizes may be ordered differently than CPU resizes.
	// If resize results in net pod resource increase, set pod cgroup config before resizing containers.
	// If resize results in net pod resource decrease, set pod cgroup config after resizing containers.
	// If an error occurs at any point, abort. Let future syncpod iterations retry the unfinished stuff.
	resizeContainers := func(rName v1.ResourceName, currPodCgLimValue, newPodCgLimValue, currPodCgReqValue, newPodCgReqValue int64) error {
		var err error
		// At upsizing, limits should expand prior to requests in order to keep "requests <= limits".
		if newPodCgLimValue > currPodCgLimValue {
			if err = setPodCgroupConfig(rName, true); err != nil {
				return err
			}
		}
		if newPodCgReqValue > currPodCgReqValue {
			if err = setPodCgroupConfig(rName, false); err != nil {
				return err
			}
		}
		if len(podContainerChanges.ContainersToUpdate[rName]) > 0 {
			if err = m.updatePodContainerResources(pod, rName, podContainerChanges.ContainersToUpdate[rName]); err != nil {
				klog.ErrorS(err, "updatePodContainerResources failed", "pod", format.Pod(pod), "resource", rName)
				return err
			}
		}
		// At downsizing, requests should shrink prior to limits in order to keep "requests <= limits".
		if newPodCgReqValue < currPodCgReqValue {
			if err = setPodCgroupConfig(rName, false); err != nil {
				return err
			}
		}
		if newPodCgLimValue < currPodCgLimValue {
			if err = setPodCgroupConfig(rName, true); err != nil {
				return err
			}
		}
		return err
	}
	if len(podContainerChanges.ContainersToUpdate[v1.ResourceMemory]) > 0 || podContainerChanges.UpdatePodResources {
		currentPodMemoryConfig, err := pcm.GetPodCgroupConfig(pod, v1.ResourceMemory)
		if err != nil {
			klog.ErrorS(err, "GetPodCgroupConfig for memory failed", "pod", pod.Name)
			result.Fail(err)
			return
		}
		if podResources.Memory != nil {
			currentPodMemoryUsage, err := pcm.GetPodCgroupMemoryUsage(pod)
			if err != nil {
				klog.ErrorS(err, "GetPodCgroupMemoryUsage failed", "pod", pod.Name)
				result.Fail(err)
				return
			}
			if currentPodMemoryUsage >= uint64(*podResources.Memory) {
				klog.ErrorS(nil, "Aborting attempt to set pod memory limit less than current memory usage", "pod", pod.Name)
				result.Fail(fmt.Errorf("aborting attempt to set pod memory limit less than current memory usage for pod %s", pod.Name))
				return
			}
		} else {
			// Default pod memory limit to the current memory limit if unset to prevent it from updating.
			// TODO(#128675): This does not support removing limits.
			podResources.Memory = currentPodMemoryConfig.Memory
		}
		if errResize := resizeContainers(v1.ResourceMemory, int64(*currentPodMemoryConfig.Memory), *podResources.Memory, 0, 0); errResize != nil {
			result.Fail(errResize)
			return
		}
	}
	if len(podContainerChanges.ContainersToUpdate[v1.ResourceCPU]) > 0 || podContainerChanges.UpdatePodResources {
		if podResources.CPUShares == nil {
			// This shouldn't happen: ResourceConfigForPod always returns a non-nil value for CPUShares.
			klog.ErrorS(nil, "podResources.CPUShares is nil", "pod", pod.Name)
			result.Fail(fmt.Errorf("podResources.CPUShares is nil for pod %s", pod.Name))
			return
		}
		currentPodCpuConfig, err := pcm.GetPodCgroupConfig(pod, v1.ResourceCPU)
		if err != nil {
			klog.ErrorS(err, "GetPodCgroupConfig for CPU failed", "pod", pod.Name)
			result.Fail(err)
			return
		}

		// Default pod CPUQuota to the current CPUQuota if no limit is set to prevent the pod limit
		// from updating.
		// TODO(#128675): This does not support removing limits.
		if podResources.CPUQuota == nil {
			podResources.CPUQuota = currentPodCpuConfig.CPUQuota
		}
		if errResize := resizeContainers(v1.ResourceCPU, *currentPodCpuConfig.CPUQuota, *podResources.CPUQuota,
			int64(*currentPodCpuConfig.CPUShares), int64(*podResources.CPUShares)); errResize != nil {
			result.Fail(errResize)
			return
		}
	}
}

func (m *kubeGenericRuntimeManager) updatePodContainerResources(pod *v1.Pod, resourceName v1.ResourceName, containersToUpdate []containerToUpdateInfo) error {
	klog.V(5).InfoS("Updating container resources", "pod", klog.KObj(pod))

	for _, cInfo := range containersToUpdate {
		container := cInfo.container.DeepCopy()
		// If updating memory limit, use most recently configured CPU request and limit values.
		// If updating CPU request and limit, use most recently configured memory request and limit values.
		switch resourceName {
		case v1.ResourceMemory:
			container.Resources.Limits = v1.ResourceList{
				v1.ResourceCPU:    *resource.NewMilliQuantity(cInfo.currentContainerResources.cpuLimit, resource.DecimalSI),
				v1.ResourceMemory: *resource.NewQuantity(cInfo.desiredContainerResources.memoryLimit, resource.BinarySI),
			}
			container.Resources.Requests = v1.ResourceList{
				v1.ResourceCPU:    *resource.NewMilliQuantity(cInfo.currentContainerResources.cpuRequest, resource.DecimalSI),
				v1.ResourceMemory: *resource.NewQuantity(cInfo.desiredContainerResources.memoryRequest, resource.BinarySI),
			}
		case v1.ResourceCPU:
			container.Resources.Limits = v1.ResourceList{
				v1.ResourceCPU:    *resource.NewMilliQuantity(cInfo.desiredContainerResources.cpuLimit, resource.DecimalSI),
				v1.ResourceMemory: *resource.NewQuantity(cInfo.currentContainerResources.memoryLimit, resource.BinarySI),
			}
			container.Resources.Requests = v1.ResourceList{
				v1.ResourceCPU:    *resource.NewMilliQuantity(cInfo.desiredContainerResources.cpuRequest, resource.DecimalSI),
				v1.ResourceMemory: *resource.NewQuantity(cInfo.currentContainerResources.memoryRequest, resource.BinarySI),
			}
		}
		if err := m.updateContainerResources(pod, container, cInfo.kubeContainerID); err != nil {
			// Log error and abort as container updates need to succeed in the order determined by computePodResizeAction.
			// The recovery path is for SyncPod to keep retrying at later times until it succeeds.
			klog.ErrorS(err, "updateContainerResources failed", "container", container.Name, "cID", cInfo.kubeContainerID,
				"pod", format.Pod(pod), "resourceName", resourceName)
			return err
		}
		resizeKey := fmt.Sprintf("%s:resize:%s", container.Name, resourceName)

		// Watch (poll) the container for the expected resources update. Stop watching once the resources
		// match the desired values.
		resizeCondition := pleg.RunningContainerWatchCondition(container.Name, func(status *kubecontainer.Status) bool {
			if status.Resources == nil {
				return false
			}
			switch resourceName {
			case v1.ResourceMemory:
				actualLimit := nonNilQuantity(status.Resources.MemoryLimit)
				return actualLimit.Equal(*container.Resources.Limits.Memory())
			case v1.ResourceCPU:
				actualLimit := nonNilQuantity(status.Resources.CPULimit)
				actualRequest := nonNilQuantity(status.Resources.CPURequest)
				desiredLimit := container.Resources.Limits.Cpu()
				desiredRequest := container.Resources.Requests.Cpu()
				// Consider limits equal if both are at or below the effective minimum limit.
				equalLimits := actualLimit.Equal(*desiredLimit) || (actualLimit.MilliValue() <= cm.MinMilliCPULimit &&
					desiredLimit.MilliValue() <= cm.MinMilliCPULimit)
				// Consider requests equal if both are at or below MinShares.
				equalRequests := actualRequest.Equal(*desiredRequest) || (actualRequest.MilliValue() <= cm.MinShares &&
					desiredRequest.MilliValue() <= cm.MinShares)
				return equalLimits && equalRequests
			default:
				return true // Shouldn't happen.
			}
		})
		m.runtimeHelper.SetPodWatchCondition(pod.UID, resizeKey, resizeCondition)

		// If UpdateContainerResources is error-free, it means desired values for 'resourceName' was accepted by runtime.
		// So we update currentContainerResources for 'resourceName', which is our view of most recently configured resources.
		// Note: We can't rely on GetPodStatus as runtime may lag in actuating the resource values it just accepted.
		switch resourceName {
		case v1.ResourceMemory:
			cInfo.currentContainerResources.memoryLimit = cInfo.desiredContainerResources.memoryLimit
			cInfo.currentContainerResources.memoryRequest = cInfo.desiredContainerResources.memoryRequest
		case v1.ResourceCPU:
			cInfo.currentContainerResources.cpuLimit = cInfo.desiredContainerResources.cpuLimit
			cInfo.currentContainerResources.cpuRequest = cInfo.desiredContainerResources.cpuRequest
		}
	}
	return nil
}

// nonNilQuantity returns a non-nil quantity. If the input is non-nil, it is returned. Otherwise a
// pointer to the zero value is returned.
func nonNilQuantity(q *resource.Quantity) *resource.Quantity {
	if q != nil {
		return q
	}
	return &resource.Quantity{}
}

// computePodActions checks whether the pod spec has changed and returns the changes if true.
func (m *kubeGenericRuntimeManager) computePodActions(ctx context.Context, pod *v1.Pod, podStatus *kubecontainer.PodStatus) podActions {
	klog.V(5).InfoS("Syncing Pod", "pod", klog.KObj(pod))

	createPodSandbox, attempt, sandboxID := runtimeutil.PodSandboxChanged(pod, podStatus)
	changes := podActions{
		KillPod:           createPodSandbox,
		CreateSandbox:     createPodSandbox,
		SandboxID:         sandboxID,
		Attempt:           attempt,
		ContainersToStart: []int{},
		ContainersToKill:  make(map[kubecontainer.ContainerID]containerToKillInfo),
	}

	handleRestartableInitContainers := types.HasRestartableInitContainer(pod)

	// If we need to (re-)create the pod sandbox, everything will need to be
	// killed and recreated, and init containers should be purged.
	if createPodSandbox {
		if !shouldRestartOnFailure(pod) && attempt != 0 && len(podStatus.ContainerStatuses) != 0 {
			// Should not restart the pod, just return.
			// we should not create a sandbox, and just kill the pod if it is already done.
			// if all containers are done and should not be started, there is no need to create a new sandbox.
			// this stops confusing logs on pods whose containers all have exit codes, but we recreate a sandbox before terminating it.
			//
			// If ContainerStatuses is empty, we assume that we've never
			// successfully created any containers. In this case, we should
			// retry creating the sandbox.
			changes.CreateSandbox = false
			return changes
		}

		// Get the containers to start, excluding the ones that succeeded if RestartPolicy is OnFailure.
		var containersToStart []int
		for idx, c := range pod.Spec.Containers {
			if pod.Spec.RestartPolicy == v1.RestartPolicyOnFailure && containerSucceeded(&c, podStatus) {
				continue
			}
			containersToStart = append(containersToStart, idx)
		}

		// We should not create a sandbox, and just kill the pod if initialization
		// is done and there is no container to start.
		if len(containersToStart) == 0 {
			hasInitialized := false
			// TODO: Remove this code path as logically it is the subset of the next
			// code path.
			if !handleRestartableInitContainers {
				_, _, hasInitialized = findNextInitContainerToRun(pod, podStatus)
			} else {
				// If there is any regular container, it means all init containers have
				// been initialized.
				hasInitialized = hasAnyRegularContainerCreated(pod, podStatus)
			}

			if hasInitialized {
				changes.CreateSandbox = false
				return changes
			}
		}

		// If we are creating a pod sandbox, we should restart from the initial
		// state.
		if len(pod.Spec.InitContainers) != 0 {
			// Pod has init containers, return the first one.
			// TODO: Remove this code path as logically it is the subset of the next
			// code path.
			if !handleRestartableInitContainers {
				changes.NextInitContainerToStart = &pod.Spec.InitContainers[0]
			} else {
				changes.InitContainersToStart = []int{0}
			}

			return changes
		}
		changes.ContainersToStart = containersToStart
		return changes
	}

	// Ephemeral containers may be started even if initialization is not yet complete.
	for i := range pod.Spec.EphemeralContainers {
		c := (*v1.Container)(&pod.Spec.EphemeralContainers[i].EphemeralContainerCommon)

		// Ephemeral Containers are never restarted
		if podStatus.FindContainerStatusByName(c.Name) == nil {
			changes.EphemeralContainersToStart = append(changes.EphemeralContainersToStart, i)
		}
	}

	if IsInPlacePodVerticalScalingAllowed(pod) {
		changes.ContainersToUpdate = make(map[v1.ResourceName][]containerToUpdateInfo)
	}

	// Check initialization progress.
	// TODO: Remove this code path as logically it is the subset of the next
	// code path.
	if !handleRestartableInitContainers {
		initLastStatus, next, done := findNextInitContainerToRun(pod, podStatus)
		if !done {
			if next != nil {
				initFailed := initLastStatus != nil && isInitContainerFailed(initLastStatus)
				if initFailed && !shouldRestartOnFailure(pod) {
					changes.KillPod = true
				} else {
					// Always try to stop containers in unknown state first.
					if initLastStatus != nil && initLastStatus.State == kubecontainer.ContainerStateUnknown {
						changes.ContainersToKill[initLastStatus.ID] = containerToKillInfo{
							name:      next.Name,
							container: next,
							message: fmt.Sprintf("Init container is in %q state, try killing it before restart",
								initLastStatus.State),
							reason: reasonUnknown,
						}
					}
					changes.NextInitContainerToStart = next
				}
			}
			// Initialization failed or still in progress. Skip inspecting non-init
			// containers.
			return changes
		}
	} else {
		hasInitialized := m.computeInitContainerActions(pod, podStatus, &changes)
		if changes.KillPod || !hasInitialized {
			// Initialization failed or still in progress. Skip inspecting non-init
			// containers.
			return changes
		}
	}

	// Number of running containers to keep.
	keepCount := 0
	// check the status of containers.
	for idx, container := range pod.Spec.Containers {
		containerStatus := podStatus.FindContainerStatusByName(container.Name)

		// Call internal container post-stop lifecycle hook for any non-running container so that any
		// allocated cpus are released immediately. If the container is restarted, cpus will be re-allocated
		// to it.
		if containerStatus != nil && containerStatus.State != kubecontainer.ContainerStateRunning {
			if err := m.internalLifecycle.PostStopContainer(containerStatus.ID.ID); err != nil {
				klog.ErrorS(err, "Internal container post-stop lifecycle hook failed for container in pod with error",
					"containerName", container.Name, "pod", klog.KObj(pod))
			}
		}

		// If container does not exist, or is not running, check whether we
		// need to restart it.
		if containerStatus == nil || containerStatus.State != kubecontainer.ContainerStateRunning {
			if kubecontainer.ShouldContainerBeRestarted(&container, pod, podStatus) {
				klog.V(3).InfoS("Container of pod is not in the desired state and shall be started", "containerName", container.Name, "pod", klog.KObj(pod))
				changes.ContainersToStart = append(changes.ContainersToStart, idx)
				if containerStatus != nil && containerStatus.State == kubecontainer.ContainerStateUnknown {
					// If container is in unknown state, we don't know whether it
					// is actually running or not, always try killing it before
					// restart to avoid having 2 running instances of the same container.
					changes.ContainersToKill[containerStatus.ID] = containerToKillInfo{
						name:      containerStatus.Name,
						container: &pod.Spec.Containers[idx],
						message: fmt.Sprintf("Container is in %q state, try killing it before restart",
							containerStatus.State),
						reason: reasonUnknown,
					}
				}
			}
			continue
		}
		// The container is running, but kill the container if any of the following condition is met.
		var message string
		var reason containerKillReason
		restart := shouldRestartOnFailure(pod)
		if _, _, changed := containerChanged(&container, containerStatus); changed {
			message = fmt.Sprintf("Container %s definition changed", container.Name)
			// Restart regardless of the restart policy because the container
			// spec changed.
			restart = true
		} else if liveness, found := m.livenessManager.Get(containerStatus.ID); found && liveness == proberesults.Failure {
			// If the container failed the liveness probe, we should kill it.
			message = fmt.Sprintf("Container %s failed liveness probe", container.Name)
			reason = reasonLivenessProbe
		} else if startup, found := m.startupManager.Get(containerStatus.ID); found && startup == proberesults.Failure {
			// If the container failed the startup probe, we should kill it.
			message = fmt.Sprintf("Container %s failed startup probe", container.Name)
			reason = reasonStartupProbe
		} else if IsInPlacePodVerticalScalingAllowed(pod) && !m.computePodResizeAction(pod, idx, false, containerStatus, &changes) {
			// computePodResizeAction updates 'changes' if resize policy requires restarting this container
			continue
		} else {
			// Keep the container.
			keepCount++
			continue
		}

		// We need to kill the container, but if we also want to restart the
		// container afterwards, make the intent clear in the message. Also do
		// not kill the entire pod since we expect container to be running eventually.
		if restart {
			message = fmt.Sprintf("%s, will be restarted", message)
			changes.ContainersToStart = append(changes.ContainersToStart, idx)
		}

		changes.ContainersToKill[containerStatus.ID] = containerToKillInfo{
			name:      containerStatus.Name,
			container: &pod.Spec.Containers[idx],
			message:   message,
			reason:    reason,
		}
		klog.V(2).InfoS("Message for Container of pod", "containerName", container.Name, "containerStatusID", containerStatus.ID, "pod", klog.KObj(pod), "containerMessage", message)
	}

	if keepCount == 0 && len(changes.ContainersToStart) == 0 {
		changes.KillPod = true
		// TODO: Remove this code path as logically it is the subset of the next
		// code path.
		if handleRestartableInitContainers {
			// To prevent the restartable init containers to keep pod alive, we should
			// not restart them.
			changes.InitContainersToStart = nil
		}
	}

	return changes
}

// SyncPod syncs the running pod into the desired pod by executing following steps:
//
//  1. Compute sandbox and container changes.
//  2. Kill pod sandbox if necessary.
//  3. Kill any containers that should not be running.
//  4. Create sandbox if necessary.
//  5. Create ephemeral containers.
//  6. Create init containers.
//  7. Resize running containers (if InPlacePodVerticalScaling==true)
//  8. Create normal containers.
func (m *kubeGenericRuntimeManager) SyncPod(ctx context.Context, pod *v1.Pod, podStatus *kubecontainer.PodStatus, pullSecrets []v1.Secret, backOff *flowcontrol.Backoff) (result kubecontainer.PodSyncResult) {
	// Step 1: Compute sandbox and container changes.
	podContainerChanges := m.computePodActions(ctx, pod, podStatus)
	klog.V(3).InfoS("computePodActions got for pod", "podActions", podContainerChanges, "pod", klog.KObj(pod))
	if podContainerChanges.CreateSandbox {
		ref, err := ref.GetReference(legacyscheme.Scheme, pod)
		if err != nil {
			klog.ErrorS(err, "Couldn't make a ref to pod", "pod", klog.KObj(pod))
		}
		if podContainerChanges.SandboxID != "" {
			m.recorder.Eventf(ref, v1.EventTypeNormal, events.SandboxChanged, "Pod sandbox changed, it will be killed and re-created.")
		} else {
			klog.V(4).InfoS("SyncPod received new pod, will create a sandbox for it", "pod", klog.KObj(pod))
		}
	}

	// Step 2: Kill the pod if the sandbox has changed.
	if podContainerChanges.KillPod {
		if podContainerChanges.CreateSandbox {
			klog.V(4).InfoS("Stopping PodSandbox for pod, will start new one", "pod", klog.KObj(pod))
		} else {
			klog.V(4).InfoS("Stopping PodSandbox for pod, because all other containers are dead", "pod", klog.KObj(pod))
		}

		killResult := m.killPodWithSyncResult(ctx, pod, kubecontainer.ConvertPodStatusToRunningPod(m.runtimeName, podStatus), nil)
		result.AddPodSyncResult(killResult)
		if killResult.Error() != nil {
			klog.ErrorS(killResult.Error(), "killPodWithSyncResult failed")
			return
		}

		if podContainerChanges.CreateSandbox {
			m.purgeInitContainers(ctx, pod, podStatus)
		}
	} else {
		// Step 3: kill any running containers in this pod which are not to keep.
		for containerID, containerInfo := range podContainerChanges.ContainersToKill {
			klog.V(3).InfoS("Killing unwanted container for pod", "containerName", containerInfo.name, "containerID", containerID, "pod", klog.KObj(pod))
			killContainerResult := kubecontainer.NewSyncResult(kubecontainer.KillContainer, containerInfo.name)
			result.AddSyncResult(killContainerResult)
			if err := m.killContainer(ctx, pod, containerID, containerInfo.name, containerInfo.message, containerInfo.reason, nil, nil); err != nil {
				killContainerResult.Fail(kubecontainer.ErrKillContainer, err.Error())
				klog.ErrorS(err, "killContainer for pod failed", "containerName", containerInfo.name, "containerID", containerID, "pod", klog.KObj(pod))
				return
			}
		}
	}

	// Keep terminated init containers fairly aggressively controlled
	// This is an optimization because container removals are typically handled
	// by container garbage collector.
	m.pruneInitContainersBeforeStart(ctx, pod, podStatus)

	// We pass the value of the PRIMARY podIP and list of podIPs down to
	// generatePodSandboxConfig and generateContainerConfig, which in turn
	// passes it to various other functions, in order to facilitate functionality
	// that requires this value (hosts file and downward API) and avoid races determining
	// the pod IP in cases where a container requires restart but the
	// podIP isn't in the status manager yet. The list of podIPs is used to
	// generate the hosts file.
	//
	// We default to the IPs in the passed-in pod status, and overwrite them if the
	// sandbox needs to be (re)started.
	var podIPs []string
	if podStatus != nil {
		podIPs = podStatus.IPs
	}

	// Step 4: Create a sandbox for the pod if necessary.
	podSandboxID := podContainerChanges.SandboxID
	if podContainerChanges.CreateSandbox {
		var msg string
		var err error

		klog.V(4).InfoS("Creating PodSandbox for pod", "pod", klog.KObj(pod))
		metrics.StartedPodsTotal.Inc()
		createSandboxResult := kubecontainer.NewSyncResult(kubecontainer.CreatePodSandbox, format.Pod(pod))
		result.AddSyncResult(createSandboxResult)

		// ConvertPodSysctlsVariableToDotsSeparator converts sysctl variable
		// in the Pod.Spec.SecurityContext.Sysctls slice into a dot as a separator.
		// runc uses the dot as the separator to verify whether the sysctl variable
		// is correct in a separate namespace, so when using the slash as the sysctl
		// variable separator, runc returns an error: "sysctl is not in a separate kernel namespace"
		// and the podSandBox cannot be successfully created. Therefore, before calling runc,
		// we need to convert the sysctl variable, the dot is used as a separator to separate the kernel namespace.
		// When runc supports slash as sysctl separator, this function can no longer be used.
		sysctl.ConvertPodSysctlsVariableToDotsSeparator(pod.Spec.SecurityContext)

		// Prepare resources allocated by the Dynammic Resource Allocation feature for the pod
		if utilfeature.DefaultFeatureGate.Enabled(features.DynamicResourceAllocation) {
			if err := m.runtimeHelper.PrepareDynamicResources(ctx, pod); err != nil {
				ref, referr := ref.GetReference(legacyscheme.Scheme, pod)
				if referr != nil {
					klog.ErrorS(referr, "Couldn't make a ref to pod", "pod", klog.KObj(pod))
					return
				}
				m.recorder.Eventf(ref, v1.EventTypeWarning, events.FailedPrepareDynamicResources, "Failed to prepare dynamic resources: %v", err)
				klog.ErrorS(err, "Failed to prepare dynamic resources", "pod", klog.KObj(pod))
				return
			}
		}

		podSandboxID, msg, err = m.createPodSandbox(ctx, pod, podContainerChanges.Attempt)
		if err != nil {
			// createPodSandbox can return an error from CNI, CSI,
			// or CRI if the Pod has been deleted while the POD is
			// being created. If the pod has been deleted then it's
			// not a real error.
			//
			// SyncPod can still be running when we get here, which
			// means the PodWorker has not acked the deletion.
			if m.podStateProvider.IsPodTerminationRequested(pod.UID) {
				klog.V(4).InfoS("Pod was deleted and sandbox failed to be created", "pod", klog.KObj(pod), "podUID", pod.UID)
				return
			}
			metrics.StartedPodsErrorsTotal.Inc()
			createSandboxResult.Fail(kubecontainer.ErrCreatePodSandbox, msg)
			klog.ErrorS(err, "CreatePodSandbox for pod failed", "pod", klog.KObj(pod))
			ref, referr := ref.GetReference(legacyscheme.Scheme, pod)
			if referr != nil {
				klog.ErrorS(referr, "Couldn't make a ref to pod", "pod", klog.KObj(pod))
			}
			m.recorder.Eventf(ref, v1.EventTypeWarning, events.FailedCreatePodSandBox, "Failed to create pod sandbox: %v", err)
			return
		}
		klog.V(4).InfoS("Created PodSandbox for pod", "podSandboxID", podSandboxID, "pod", klog.KObj(pod))

		resp, err := m.runtimeService.PodSandboxStatus(ctx, podSandboxID, false)
		if err != nil {
			ref, referr := ref.GetReference(legacyscheme.Scheme, pod)
			if referr != nil {
				klog.ErrorS(referr, "Couldn't make a ref to pod", "pod", klog.KObj(pod))
			}
			m.recorder.Eventf(ref, v1.EventTypeWarning, events.FailedStatusPodSandBox, "Unable to get pod sandbox status: %v", err)
			klog.ErrorS(err, "Failed to get pod sandbox status; Skipping pod", "pod", klog.KObj(pod))
			result.Fail(err)
			return
		}
		if resp.GetStatus() == nil {
			result.Fail(errors.New("pod sandbox status is nil"))
			return
		}

		// If we ever allow updating a pod from non-host-network to
		// host-network, we may use a stale IP.
		if !kubecontainer.IsHostNetworkPod(pod) {
			// Overwrite the podIPs passed in the pod status, since we just started the pod sandbox.
			podIPs = m.determinePodSandboxIPs(pod.Namespace, pod.Name, resp.GetStatus())
			klog.V(4).InfoS("Determined the ip for pod after sandbox changed", "IPs", podIPs, "pod", klog.KObj(pod))
		}
	}

	// the start containers routines depend on pod ip(as in primary pod ip)
	// instead of trying to figure out if we have 0 < len(podIPs)
	// everytime, we short circuit it here
	podIP := ""
	if len(podIPs) != 0 {
		podIP = podIPs[0]
	}

	// Get podSandboxConfig for containers to start.
	configPodSandboxResult := kubecontainer.NewSyncResult(kubecontainer.ConfigPodSandbox, podSandboxID)
	result.AddSyncResult(configPodSandboxResult)
	podSandboxConfig, err := m.generatePodSandboxConfig(pod, podContainerChanges.Attempt)
	if err != nil {
		message := fmt.Sprintf("GeneratePodSandboxConfig for pod %q failed: %v", format.Pod(pod), err)
		klog.ErrorS(err, "GeneratePodSandboxConfig for pod failed", "pod", klog.KObj(pod))
		configPodSandboxResult.Fail(kubecontainer.ErrConfigPodSandbox, message)
		return
	}

	imageVolumePullResults, err := m.getImageVolumes(ctx, pod, podSandboxConfig, pullSecrets)
	if err != nil {
		klog.ErrorS(err, "Get image volumes for pod failed", "pod", klog.KObj(pod))
		configPodSandboxResult.Fail(kubecontainer.ErrConfigPodSandbox, err.Error())
		return
	}

	// Helper containing boilerplate common to starting all types of containers.
	// typeName is a description used to describe this type of container in log messages,
	// currently: "container", "init container" or "ephemeral container"
	// metricLabel is the label used to describe this type of container in monitoring metrics.
	// currently: "container", "init_container" or "ephemeral_container"
	start := func(ctx context.Context, typeName, metricLabel string, spec *startSpec) error {
		startContainerResult := kubecontainer.NewSyncResult(kubecontainer.StartContainer, spec.container.Name)
		result.AddSyncResult(startContainerResult)

		isInBackOff, msg, err := m.doBackOff(pod, spec.container, podStatus, backOff)
		if isInBackOff {
			startContainerResult.Fail(err, msg)
			klog.V(4).InfoS("Backing Off restarting container in pod", "containerType", typeName, "container", spec.container, "pod", klog.KObj(pod))
			return err
		}

		metrics.StartedContainersTotal.WithLabelValues(metricLabel).Inc()
		if sc.HasWindowsHostProcessRequest(pod, spec.container) {
			metrics.StartedHostProcessContainersTotal.WithLabelValues(metricLabel).Inc()
		}
		klog.V(4).InfoS("Creating container in pod", "containerType", typeName, "container", spec.container, "pod", klog.KObj(pod))

		// We fail late here to populate the "ErrImagePull" and "ImagePullBackOff" correctly to the end user.
		imageVolumes, err := m.toKubeContainerImageVolumes(imageVolumePullResults, spec.container, pod, startContainerResult)
		if err != nil {
			return err
		}

		// NOTE (aramase) podIPs are populated for single stack and dual stack clusters. Send only podIPs.
		if msg, err := m.startContainer(ctx, podSandboxID, podSandboxConfig, spec, pod, podStatus, pullSecrets, podIP, podIPs, imageVolumes); err != nil {
			// startContainer() returns well-defined error codes that have reasonable cardinality for metrics and are
			// useful to cluster administrators to distinguish "server errors" from "user errors".
			metrics.StartedContainersErrorsTotal.WithLabelValues(metricLabel, err.Error()).Inc()
			if sc.HasWindowsHostProcessRequest(pod, spec.container) {
				metrics.StartedHostProcessContainersErrorsTotal.WithLabelValues(metricLabel, err.Error()).Inc()
			}
			startContainerResult.Fail(err, msg)
			// known errors that are logged in other places are logged at higher levels here to avoid
			// repetitive log spam
			switch {
			case err == images.ErrImagePullBackOff:
				klog.V(3).InfoS("Container start failed in pod", "containerType", typeName, "container", spec.container, "pod", klog.KObj(pod), "containerMessage", msg, "err", err)
			default:
				utilruntime.HandleError(fmt.Errorf("%v %+v start failed in pod %v: %v: %s", typeName, spec.container, format.Pod(pod), err, msg))
			}
			return err
		}

		return nil
	}

	// Step 5: start ephemeral containers
	// These are started "prior" to init containers to allow running ephemeral containers even when there
	// are errors starting an init container. In practice init containers will start first since ephemeral
	// containers cannot be specified on pod creation.
	for _, idx := range podContainerChanges.EphemeralContainersToStart {
		start(ctx, "ephemeral container", metrics.EphemeralContainer, ephemeralContainerStartSpec(&pod.Spec.EphemeralContainers[idx]))
	}

	// TODO: Remove this code path as logically it is the subset of the next
	// code path.
	if !types.HasRestartableInitContainer(pod) {
		// Step 6: start the init container.
		if container := podContainerChanges.NextInitContainerToStart; container != nil {
			// Start the next init container.
			if err := start(ctx, "init container", metrics.InitContainer, containerStartSpec(container)); err != nil {
				return
			}

			// Successfully started the container; clear the entry in the failure
			klog.V(4).InfoS("Completed init container for pod", "containerName", container.Name, "pod", klog.KObj(pod))
		}
	} else {
		// Step 6: start init containers.
		for _, idx := range podContainerChanges.InitContainersToStart {
			container := &pod.Spec.InitContainers[idx]
			// Start the next init container.
			if err := start(ctx, "init container", metrics.InitContainer, containerStartSpec(container)); err != nil {
				if podutil.IsRestartableInitContainer(container) {
					klog.V(4).InfoS("Failed to start the restartable init container for the pod, skipping", "initContainerName", container.Name, "pod", klog.KObj(pod))
					continue
				}
				klog.V(4).InfoS("Failed to initialize the pod, as the init container failed to start, aborting", "initContainerName", container.Name, "pod", klog.KObj(pod))
				return
			}

			// Successfully started the container; clear the entry in the failure
			klog.V(4).InfoS("Completed init container for pod", "containerName", container.Name, "pod", klog.KObj(pod))
		}
	}

	// Step 7: For containers in podContainerChanges.ContainersToUpdate[CPU,Memory] list, invoke UpdateContainerResources
	if IsInPlacePodVerticalScalingAllowed(pod) {
		if len(podContainerChanges.ContainersToUpdate) > 0 || podContainerChanges.UpdatePodResources {
			m.doPodResizeAction(pod, podContainerChanges, &result)
		}
	}

	// Step 8: start containers in podContainerChanges.ContainersToStart.
	for _, idx := range podContainerChanges.ContainersToStart {
		start(ctx, "container", metrics.Container, containerStartSpec(&pod.Spec.Containers[idx]))
	}

	return
}

// imageVolumePulls are the pull results for each image volume name.
type imageVolumePulls = map[string]imageVolumePullResult

// imageVolumePullResult is a pull result for a single image volume.
// If spec is nil, then err and msg should be set.
// If err is nil, then spec should be set.
type imageVolumePullResult struct {
	spec runtimeapi.ImageSpec
	err  error
	msg  string
}

func (m *kubeGenericRuntimeManager) toKubeContainerImageVolumes(imageVolumePullResults imageVolumePulls, container *v1.Container, pod *v1.Pod, syncResult *kubecontainer.SyncResult) (kubecontainer.ImageVolumes, error) {
	if len(imageVolumePullResults) == 0 {
		return nil, nil
	}

	imageVolumes := kubecontainer.ImageVolumes{}
	var (
		lastErr error
		lastMsg string
	)
	for _, v := range container.VolumeMounts {
		res, ok := imageVolumePullResults[v.Name]
		if !ok {
			continue
		}

		if res.err != nil {
			s, _ := grpcstatus.FromError(res.err)
			m.recordContainerEvent(pod, container, "", v1.EventTypeWarning, events.FailedToCreateContainer, "Error: %v", s.Message())
			lastErr = res.err
			lastMsg = res.msg
			continue
		}

		imageVolumes[v.Name] = &res.spec
	}

	if lastErr != nil {
		syncResult.Fail(lastErr, lastMsg)
		return nil, lastErr
	}

	return imageVolumes, nil
}

func (m *kubeGenericRuntimeManager) getImageVolumes(ctx context.Context, pod *v1.Pod, podSandboxConfig *runtimeapi.PodSandboxConfig, pullSecrets []v1.Secret) (imageVolumePulls, error) {
	if !utilfeature.DefaultFeatureGate.Enabled(features.ImageVolume) {
		return nil, nil
	}

	podRuntimeHandler, err := m.getPodRuntimeHandler(pod)
	if err != nil {
		klog.ErrorS(err, "Failed to get pod runtime handler", "pod", klog.KObj(pod))
		return nil, err
	}

	res := make(imageVolumePulls)
	for _, volume := range pod.Spec.Volumes {
		if volume.Image == nil {
			continue
		}

		objectRef, _ := ref.GetReference(legacyscheme.Scheme, pod) // objectRef can be nil, no error check required
		ref, msg, err := m.imagePuller.EnsureImageExists(
			ctx, objectRef, pod, volume.Image.Reference, pullSecrets, podSandboxConfig, podRuntimeHandler, volume.Image.PullPolicy,
		)
		if err != nil {
			klog.ErrorS(err, "Failed to ensure image", "pod", klog.KObj(pod))
			res[volume.Name] = imageVolumePullResult{err: err, msg: msg}
			continue
		}

		klog.V(4).InfoS("Pulled image", "ref", ref, "pod", klog.KObj(pod))
		res[volume.Name] = imageVolumePullResult{spec: runtimeapi.ImageSpec{
			Image:              ref,
			UserSpecifiedImage: volume.Image.Reference,
			RuntimeHandler:     podRuntimeHandler,
			Annotations:        pod.Annotations,
		}}
	}

	return res, nil
}

// If a container is still in backoff, the function will return a brief backoff error and
// a detailed error message.
func (m *kubeGenericRuntimeManager) doBackOff(pod *v1.Pod, container *v1.Container, podStatus *kubecontainer.PodStatus, backOff *flowcontrol.Backoff) (bool, string, error) {
	var cStatus *kubecontainer.Status
	for _, c := range podStatus.ContainerStatuses {
		if c.Name == container.Name && c.State == kubecontainer.ContainerStateExited {
			cStatus = c
			break
		}
	}

	if cStatus == nil {
		return false, "", nil
	}

	klog.V(3).InfoS("Checking backoff for container in pod", "containerName", container.Name, "pod", klog.KObj(pod))
	// Use the finished time of the latest exited container as the start point to calculate whether to do back-off.
	ts := cStatus.FinishedAt
	// backOff requires a unique key to identify the container.
	key := GetStableKey(pod, container)
	if backOff.IsInBackOffSince(key, ts) {
		if containerRef, err := kubecontainer.GenerateContainerRef(pod, container); err == nil {
			m.recorder.Eventf(containerRef, v1.EventTypeWarning, events.BackOffStartContainer,
				fmt.Sprintf("Back-off restarting failed container %s in pod %s", container.Name, format.Pod(pod)))
		}
		err := fmt.Errorf("back-off %s restarting failed container=%s pod=%s", backOff.Get(key), container.Name, format.Pod(pod))
		klog.V(3).InfoS("Back-off restarting failed container", "err", err.Error())
		return true, err.Error(), kubecontainer.ErrCrashLoopBackOff
	}

	backOff.Next(key, ts)
	return false, "", nil
}

// KillPod kills all the containers of a pod. Pod may be nil, running pod must not be.
// gracePeriodOverride if specified allows the caller to override the pod default grace period.
// only hard kill paths are allowed to specify a gracePeriodOverride in the kubelet in order to not corrupt user data.
// it is useful when doing SIGKILL for hard eviction scenarios, or max grace period during soft eviction scenarios.
func (m *kubeGenericRuntimeManager) KillPod(ctx context.Context, pod *v1.Pod, runningPod kubecontainer.Pod, gracePeriodOverride *int64) error {
	err := m.killPodWithSyncResult(ctx, pod, runningPod, gracePeriodOverride)
	return err.Error()
}

// killPodWithSyncResult kills a runningPod and returns SyncResult.
// Note: The pod passed in could be *nil* when kubelet restarted.
func (m *kubeGenericRuntimeManager) killPodWithSyncResult(ctx context.Context, pod *v1.Pod, runningPod kubecontainer.Pod, gracePeriodOverride *int64) (result kubecontainer.PodSyncResult) {
	killContainerResults := m.killContainersWithSyncResult(ctx, pod, runningPod, gracePeriodOverride)
	for _, containerResult := range killContainerResults {
		result.AddSyncResult(containerResult)
	}

	// stop sandbox, the sandbox will be removed in GarbageCollect
	killSandboxResult := kubecontainer.NewSyncResult(kubecontainer.KillPodSandbox, runningPod.ID)
	result.AddSyncResult(killSandboxResult)
	// Stop all sandboxes belongs to same pod
	for _, podSandbox := range runningPod.Sandboxes {
		if err := m.runtimeService.StopPodSandbox(ctx, podSandbox.ID.ID); err != nil && !crierror.IsNotFound(err) {
			killSandboxResult.Fail(kubecontainer.ErrKillPodSandbox, err.Error())
			klog.ErrorS(nil, "Failed to stop sandbox", "podSandboxID", podSandbox.ID)
		}
	}

	return
}

func (m *kubeGenericRuntimeManager) GeneratePodStatus(event *runtimeapi.ContainerEventResponse) (*kubecontainer.PodStatus, error) {
	podIPs := m.determinePodSandboxIPs(event.PodSandboxStatus.Metadata.Namespace, event.PodSandboxStatus.Metadata.Name, event.PodSandboxStatus)

	kubeContainerStatuses := []*kubecontainer.Status{}
	for _, status := range event.ContainersStatuses {
		kubeContainerStatuses = append(kubeContainerStatuses, m.convertToKubeContainerStatus(status))
	}

	sort.Sort(containerStatusByCreated(kubeContainerStatuses))

	return &kubecontainer.PodStatus{
		ID:                kubetypes.UID(event.PodSandboxStatus.Metadata.Uid),
		Name:              event.PodSandboxStatus.Metadata.Name,
		Namespace:         event.PodSandboxStatus.Metadata.Namespace,
		IPs:               podIPs,
		SandboxStatuses:   []*runtimeapi.PodSandboxStatus{event.PodSandboxStatus},
		ContainerStatuses: kubeContainerStatuses,
	}, nil
}

// GetPodStatus retrieves the status of the pod, including the
// information of all containers in the pod that are visible in Runtime.
func (m *kubeGenericRuntimeManager) GetPodStatus(ctx context.Context, uid kubetypes.UID, name, namespace string) (*kubecontainer.PodStatus, error) {
	// Now we retain restart count of container as a container label. Each time a container
	// restarts, pod will read the restart count from the registered dead container, increment
	// it to get the new restart count, and then add a label with the new restart count on
	// the newly started container.
	// However, there are some limitations of this method:
	//	1. When all dead containers were garbage collected, the container status could
	//	not get the historical value and would be *inaccurate*. Fortunately, the chance
	//	is really slim.
	//	2. When working with old version containers which have no restart count label,
	//	we can only assume their restart count is 0.
	// Anyhow, we only promised "best-effort" restart count reporting, we can just ignore
	// these limitations now.
	// TODO: move this comment to SyncPod.
	podSandboxIDs, err := m.getSandboxIDByPodUID(ctx, uid, nil)
	if err != nil {
		return nil, err
	}

	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			UID:       uid,
		},
	}

	podFullName := format.Pod(pod)

	klog.V(4).InfoS("getSandboxIDByPodUID got sandbox IDs for pod", "podSandboxID", podSandboxIDs, "pod", klog.KObj(pod))

	sandboxStatuses := []*runtimeapi.PodSandboxStatus{}
	containerStatuses := []*kubecontainer.Status{}
	timestamp := time.Now()

	podIPs := []string{}
	for idx, podSandboxID := range podSandboxIDs {
		resp, err := m.runtimeService.PodSandboxStatus(ctx, podSandboxID, false)
		// Between List (getSandboxIDByPodUID) and check (PodSandboxStatus) another thread might remove a container, and that is normal.
		// The previous call (getSandboxIDByPodUID) never fails due to a pod sandbox not existing.
		// Therefore, this method should not either, but instead act as if the previous call failed,
		// which means the error should be ignored.
		if crierror.IsNotFound(err) {
			continue
		}
		if err != nil {
			klog.ErrorS(err, "PodSandboxStatus of sandbox for pod", "podSandboxID", podSandboxID, "pod", klog.KObj(pod))
			return nil, err
		}
		if resp.GetStatus() == nil {
			return nil, errors.New("pod sandbox status is nil")

		}
		sandboxStatuses = append(sandboxStatuses, resp.Status)
		// Only get pod IP from latest sandbox
		if idx == 0 && resp.Status.State == runtimeapi.PodSandboxState_SANDBOX_READY {
			podIPs = m.determinePodSandboxIPs(namespace, name, resp.Status)
		}

		if idx == 0 && utilfeature.DefaultFeatureGate.Enabled(features.EventedPLEG) {
			if resp.Timestamp == 0 {
				// If the Evented PLEG is enabled in the kubelet, but not in the runtime
				// then the pod status we get will not have the timestamp set.
				// e.g. CI job 'pull-kubernetes-e2e-gce-alpha-features' will runs with
				// features gate enabled, which includes Evented PLEG, but uses the
				// runtime without Evented PLEG support.
				klog.V(4).InfoS("Runtime does not set pod status timestamp", "pod", klog.KObj(pod))
				containerStatuses, err = m.getPodContainerStatuses(ctx, uid, name, namespace)
				if err != nil {
					if m.logReduction.ShouldMessageBePrinted(err.Error(), podFullName) {
						klog.ErrorS(err, "getPodContainerStatuses for pod failed", "pod", klog.KObj(pod))
					}
					return nil, err
				}
			} else {
				// Get the statuses of all containers visible to the pod and
				// timestamp from sandboxStatus.
				timestamp = time.Unix(0, resp.Timestamp)
				for _, cs := range resp.ContainersStatuses {
					cStatus := m.convertToKubeContainerStatus(cs)
					containerStatuses = append(containerStatuses, cStatus)
				}
			}
		}
	}

	if !utilfeature.DefaultFeatureGate.Enabled(features.EventedPLEG) {
		// Get statuses of all containers visible in the pod.
		containerStatuses, err = m.getPodContainerStatuses(ctx, uid, name, namespace)
		if err != nil {
			if m.logReduction.ShouldMessageBePrinted(err.Error(), podFullName) {
				klog.ErrorS(err, "getPodContainerStatuses for pod failed", "pod", klog.KObj(pod))
			}
			return nil, err
		}
	}

	m.logReduction.ClearID(podFullName)
	return &kubecontainer.PodStatus{
		ID:                uid,
		Name:              name,
		Namespace:         namespace,
		IPs:               podIPs,
		SandboxStatuses:   sandboxStatuses,
		ContainerStatuses: containerStatuses,
		TimeStamp:         timestamp,
	}, nil
}

func (m *kubeGenericRuntimeManager) GetContainerStatus(ctx context.Context, id kubecontainer.ContainerID) (*kubecontainer.Status, error) {
	resp, err := m.runtimeService.ContainerStatus(ctx, id.ID, false)
	if err != nil {
		return nil, fmt.Errorf("runtime container status: %w", err)
	}
	return m.convertToKubeContainerStatus(resp.GetStatus()), nil
}

// GarbageCollect removes dead containers using the specified container gc policy.
func (m *kubeGenericRuntimeManager) GarbageCollect(ctx context.Context, gcPolicy kubecontainer.GCPolicy, allSourcesReady bool, evictNonDeletedPods bool) error {
	return m.containerGC.GarbageCollect(ctx, gcPolicy, allSourcesReady, evictNonDeletedPods)
}

// UpdatePodCIDR is just a passthrough method to update the runtimeConfig of the shim
// with the podCIDR supplied by the kubelet.
func (m *kubeGenericRuntimeManager) UpdatePodCIDR(ctx context.Context, podCIDR string) error {
	// TODO(#35531): do we really want to write a method on this manager for each
	// field of the config?
	klog.InfoS("Updating runtime config through cri with podcidr", "CIDR", podCIDR)
	return m.runtimeService.UpdateRuntimeConfig(ctx,
		&runtimeapi.RuntimeConfig{
			NetworkConfig: &runtimeapi.NetworkConfig{
				PodCidr: podCIDR,
			},
		})
}

func (m *kubeGenericRuntimeManager) CheckpointContainer(ctx context.Context, options *runtimeapi.CheckpointContainerRequest) error {
	return m.runtimeService.CheckpointContainer(ctx, options)
}

func (m *kubeGenericRuntimeManager) ListMetricDescriptors(ctx context.Context) ([]*runtimeapi.MetricDescriptor, error) {
	return m.runtimeService.ListMetricDescriptors(ctx)
}

func (m *kubeGenericRuntimeManager) ListPodSandboxMetrics(ctx context.Context) ([]*runtimeapi.PodSandboxMetrics, error) {
	return m.runtimeService.ListPodSandboxMetrics(ctx)
}
