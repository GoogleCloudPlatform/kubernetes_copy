//go:build linux
// +build linux

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

package cm

import (
	"context"
	"fmt"
	"os"
	"path"
	"sync"
	"time"

	"github.com/opencontainers/runc/libcontainer/cgroups"
	"github.com/opencontainers/runc/libcontainer/cgroups/manager"
	"github.com/opencontainers/runc/libcontainer/configs"
	"k8s.io/klog/v2"
	"k8s.io/mount-utils"
	utilpath "k8s.io/utils/path"

	inuserns "github.com/moby/sys/userns"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apiserver/pkg/server/healthz"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/record"
	utilsysctl "k8s.io/component-helpers/node/util/sysctl"
	internalapi "k8s.io/cri-api/pkg/apis"
	pluginwatcherapi "k8s.io/kubelet/pkg/apis/pluginregistration/v1"
	podresourcesapi "k8s.io/kubelet/pkg/apis/podresources/v1"
	kubefeatures "k8s.io/kubernetes/pkg/features"
	"k8s.io/kubernetes/pkg/kubelet/cadvisor"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpumanager"
	"k8s.io/kubernetes/pkg/kubelet/cm/devicemanager"
	"k8s.io/kubernetes/pkg/kubelet/cm/dra"
	"k8s.io/kubernetes/pkg/kubelet/cm/memorymanager"
	memorymanagerstate "k8s.io/kubernetes/pkg/kubelet/cm/memorymanager/state"
	"k8s.io/kubernetes/pkg/kubelet/cm/resourceupdates"
	"k8s.io/kubernetes/pkg/kubelet/cm/topologymanager"
	cmutil "k8s.io/kubernetes/pkg/kubelet/cm/util"
	"k8s.io/kubernetes/pkg/kubelet/config"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	"k8s.io/kubernetes/pkg/kubelet/events"
	"k8s.io/kubernetes/pkg/kubelet/lifecycle"
	"k8s.io/kubernetes/pkg/kubelet/pluginmanager/cache"
	"k8s.io/kubernetes/pkg/kubelet/stats/pidlimit"
	"k8s.io/kubernetes/pkg/kubelet/status"
	"k8s.io/kubernetes/pkg/kubelet/util/swap"
	schedulerframework "k8s.io/kubernetes/pkg/scheduler/framework"
	"k8s.io/kubernetes/pkg/util/oom"
)

// A non-user container tracked by the Kubelet.
type systemContainer struct {
	// Absolute name of the container.
	name string

	// CPU limit in millicores.
	cpuMillicores int64

	// Function that ensures the state of the container.
	// m is the cgroup manager for the specified container.
	ensureStateFunc func(m cgroups.Manager) error

	// Manager for the cgroups of the external container.
	manager cgroups.Manager
}

func newSystemCgroups(containerName string) (*systemContainer, error) {
	manager, err := createManager(containerName)
	if err != nil {
		return nil, err
	}
	return &systemContainer{
		name:    containerName,
		manager: manager,
	}, nil
}

type containerManagerImpl struct {
	sync.RWMutex
	cadvisorInterface cadvisor.Interface
	mountUtil         mount.Interface
	NodeConfig
	status Status
	// External containers being managed.
	systemContainers []*systemContainer
	// Tasks that are run periodically
	periodicTasks []func()
	// Holds all the mounted cgroup subsystems
	subsystems *CgroupSubsystems
	nodeInfo   *v1.Node
	// Interface for cgroup management
	cgroupManager CgroupManager
	// Capacity of this node.
	capacity v1.ResourceList
	// Capacity of this node, including internal resources.
	internalCapacity v1.ResourceList
	// Absolute cgroupfs path to a cgroup that Kubelet needs to place all pods under.
	// This path include a top level container for enforcing Node Allocatable.
	cgroupRoot CgroupName
	// Event recorder interface.
	recorder record.EventRecorder
	// Interface for QoS cgroup management
	qosContainerManager QOSContainerManager
	// Interface for exporting and allocating devices reported by device plugins.
	deviceManager devicemanager.Manager
	// Interface for CPU affinity management.
	cpuManager cpumanager.Manager
	// Interface for memory affinity management.
	memoryManager memorymanager.Manager
	// Interface for Topology resource co-ordination
	topologyManager topologymanager.Manager
	// Interface for Dynamic Resource Allocation management.
	draManager dra.Manager
	// kubeClient is the interface to the Kubernetes API server. May be nil if the kubelet is running in standalone mode.
	kubeClient clientset.Interface
}

type features struct {
	cpuHardcapping bool
}

var _ ContainerManager = &containerManagerImpl{}

// checks if the required cgroups subsystems are mounted.
// As of now, only 'cpu' and 'memory' are required.
// cpu quota is a soft requirement.
func validateSystemRequirements(mountUtil mount.Interface) (features, error) {
	const (
		cgroupMountType = "cgroup"
		localErr        = "system validation failed"
	)
	var (
		cpuMountPoint string
		f             features
	)
	mountPoints, err := mountUtil.List()
	if err != nil {
		return f, fmt.Errorf("%s - %v", localErr, err)
	}

	if cgroups.IsCgroup2UnifiedMode() {
		f.cpuHardcapping = true
		return f, nil
	}

	expectedCgroups := sets.New("cpu", "cpuacct", "cpuset", "memory")
	for _, mountPoint := range mountPoints {
		if mountPoint.Type == cgroupMountType {
			for _, opt := range mountPoint.Opts {
				if expectedCgroups.Has(opt) {
					expectedCgroups.Delete(opt)
				}
				if opt == "cpu" {
					cpuMountPoint = mountPoint.Path
				}
			}
		}
	}

	if expectedCgroups.Len() > 0 {
		return f, fmt.Errorf("%s - Following Cgroup subsystem not mounted: %v", localErr, sets.List(expectedCgroups))
	}

	// Check if cpu quota is available.
	// CPU cgroup is required and so it expected to be mounted at this point.
	periodExists, err := utilpath.Exists(utilpath.CheckFollowSymlink, path.Join(cpuMountPoint, "cpu.cfs_period_us"))
	if err != nil {
		klog.ErrorS(err, "Failed to detect if CPU cgroup cpu.cfs_period_us is available")
	}
	quotaExists, err := utilpath.Exists(utilpath.CheckFollowSymlink, path.Join(cpuMountPoint, "cpu.cfs_quota_us"))
	if err != nil {
		klog.ErrorS(err, "Failed to detect if CPU cgroup cpu.cfs_quota_us is available")
	}
	if quotaExists && periodExists {
		f.cpuHardcapping = true
	}
	return f, nil
}

// TODO(vmarmol): Add limits to the system containers.
// Takes the absolute name of the specified containers.
// Empty container name disables use of the specified container.
func NewContainerManager(mountUtil mount.Interface, cadvisorInterface cadvisor.Interface, nodeConfig NodeConfig, failSwapOn bool, recorder record.EventRecorder, kubeClient clientset.Interface) (ContainerManager, error) {
	subsystems, err := GetCgroupSubsystems()
	if err != nil {
		return nil, fmt.Errorf("failed to get mounted cgroup subsystems: %v", err)
	}

	isSwapOn, err := swap.IsSwapOn()
	if err != nil {
		return nil, fmt.Errorf("failed to determine if swap is on: %w", err)
	}

	if isSwapOn {
		if failSwapOn {
			return nil, fmt.Errorf("running with swap on is not supported, please disable swap or set --fail-swap-on flag to false")
		}

		if !swap.IsTmpfsNoswapOptionSupported(mountUtil, nodeConfig.KubeletRootDir) {
			nodeRef := nodeRefFromNode(string(nodeConfig.NodeName))
			recorder.Event(nodeRef, v1.EventTypeWarning, events.PossibleMemoryBackedVolumesOnDisk,
				"The tmpfs noswap option is not supported. Memory-backed volumes (e.g. secrets, emptyDirs, etc.) "+
					"might be swapped to disk and should no longer be considered secure.",
			)
		}
	}

	var internalCapacity = v1.ResourceList{}
	// It is safe to invoke `MachineInfo` on cAdvisor before logically initializing cAdvisor here because
	// machine info is computed and cached once as part of cAdvisor object creation.
	// But `RootFsInfo` and `ImagesFsInfo` are not available at this moment so they will be called later during manager starts
	machineInfo, err := cadvisorInterface.MachineInfo()
	if err != nil {
		return nil, err
	}
	capacity := cadvisor.CapacityFromMachineInfo(machineInfo)
	for k, v := range capacity {
		internalCapacity[k] = v
	}
	pidlimits, err := pidlimit.Stats()
	if err == nil && pidlimits != nil && pidlimits.MaxPID != nil {
		internalCapacity[pidlimit.PIDs] = *resource.NewQuantity(
			int64(*pidlimits.MaxPID),
			resource.DecimalSI)
	}

	// Turn CgroupRoot from a string (in cgroupfs path format) to internal CgroupName
	cgroupRoot := ParseCgroupfsToCgroupName(nodeConfig.CgroupRoot)
	cgroupManager := NewCgroupManager(subsystems, nodeConfig.CgroupDriver)
	nodeConfig.CgroupVersion = cgroupManager.Version()
	// Check if Cgroup-root actually exists on the node
	if nodeConfig.CgroupsPerQOS {
		// this does default to / when enabled, but this tests against regressions.
		if nodeConfig.CgroupRoot == "" {
			return nil, fmt.Errorf("invalid configuration: cgroups-per-qos was specified and cgroup-root was not specified. To enable the QoS cgroup hierarchy you need to specify a valid cgroup-root")
		}

		// we need to check that the cgroup root actually exists for each subsystem
		// of note, we always use the cgroupfs driver when performing this check since
		// the input is provided in that format.
		// this is important because we do not want any name conversion to occur.
		if err := cgroupManager.Validate(cgroupRoot); err != nil {
			return nil, fmt.Errorf("invalid configuration: %w", err)
		}
		klog.InfoS("Container manager verified user specified cgroup-root exists", "cgroupRoot", cgroupRoot)
		// Include the top level cgroup for enforcing node allocatable into cgroup-root.
		// This way, all sub modules can avoid having to understand the concept of node allocatable.
		cgroupRoot = NewCgroupName(cgroupRoot, defaultNodeAllocatableCgroupName)
	}
	klog.InfoS("Creating Container Manager object based on Node Config", "nodeConfig", nodeConfig)

	qosContainerManager, err := NewQOSContainerManager(subsystems, cgroupRoot, nodeConfig, cgroupManager)
	if err != nil {
		return nil, err
	}

	cm := &containerManagerImpl{
		cadvisorInterface:   cadvisorInterface,
		mountUtil:           mountUtil,
		NodeConfig:          nodeConfig,
		subsystems:          subsystems,
		cgroupManager:       cgroupManager,
		capacity:            capacity,
		internalCapacity:    internalCapacity,
		cgroupRoot:          cgroupRoot,
		recorder:            recorder,
		qosContainerManager: qosContainerManager,
	}

	cm.topologyManager, err = topologymanager.NewManager(
		machineInfo.Topology,
		nodeConfig.TopologyManagerPolicy,
		nodeConfig.TopologyManagerScope,
		nodeConfig.TopologyManagerPolicyOptions,
	)

	if err != nil {
		return nil, err
	}

	klog.InfoS("Creating device plugin manager")
	cm.deviceManager, err = devicemanager.NewManagerImpl(machineInfo.Topology, cm.topologyManager)
	if err != nil {
		return nil, err
	}
	cm.topologyManager.AddHintProvider(cm.deviceManager)

	// Initialize DRA manager
	if utilfeature.DefaultFeatureGate.Enabled(kubefeatures.DynamicResourceAllocation) {
		klog.InfoS("Creating Dynamic Resource Allocation (DRA) manager")
		cm.draManager, err = dra.NewManagerImpl(kubeClient, nodeConfig.KubeletRootDir, nodeConfig.NodeName)
		if err != nil {
			return nil, err
		}
	}
	cm.kubeClient = kubeClient

	// Initialize CPU manager
	cm.cpuManager, err = cpumanager.NewManager(
		nodeConfig.CPUManagerPolicy,
		nodeConfig.CPUManagerPolicyOptions,
		nodeConfig.CPUManagerReconcilePeriod,
		machineInfo,
		nodeConfig.NodeAllocatableConfig.ReservedSystemCPUs,
		cm.GetNodeAllocatableReservation(),
		nodeConfig.KubeletRootDir,
		cm.topologyManager,
	)
	if err != nil {
		klog.ErrorS(err, "Failed to initialize cpu manager")
		return nil, err
	}
	cm.topologyManager.AddHintProvider(cm.cpuManager)

	cm.memoryManager, err = memorymanager.NewManager(
		nodeConfig.MemoryManagerPolicy,
		machineInfo,
		cm.GetNodeAllocatableReservation(),
		nodeConfig.MemoryManagerReservedMemory,
		nodeConfig.KubeletRootDir,
		cm.topologyManager,
	)
	if err != nil {
		klog.ErrorS(err, "Failed to initialize memory manager")
		return nil, err
	}
	cm.topologyManager.AddHintProvider(cm.memoryManager)

	return cm, nil
}

// NewPodContainerManager is a factory method returns a PodContainerManager object
// If qosCgroups are enabled then it returns the general pod container manager
// otherwise it returns a no-op manager which essentially does nothing
func (cm *containerManagerImpl) NewPodContainerManager() PodContainerManager {
	if cm.NodeConfig.CgroupsPerQOS {
		return &podContainerManagerImpl{
			qosContainersInfo: cm.GetQOSContainersInfo(),
			subsystems:        cm.subsystems,
			cgroupManager:     cm.cgroupManager,
			podPidsLimit:      cm.PodPidsLimit,
			enforceCPULimits:  cm.EnforceCPULimits,
			// cpuCFSQuotaPeriod is in microseconds. NodeConfig.CPUCFSQuotaPeriod is time.Duration (measured in nano seconds).
			// Convert (cm.CPUCFSQuotaPeriod) [nanoseconds] / time.Microsecond (1000) to get cpuCFSQuotaPeriod in microseconds.
			cpuCFSQuotaPeriod:   uint64(cm.CPUCFSQuotaPeriod / time.Microsecond),
			podContainerManager: cm,
		}
	}
	return &podContainerManagerNoop{
		cgroupRoot: cm.cgroupRoot,
	}
}

func (cm *containerManagerImpl) PodHasExclusiveCPUs(pod *v1.Pod) bool {
	return podHasExclusiveCPUs(cm.cpuManager, pod)
}

func (cm *containerManagerImpl) ContainerHasExclusiveCPUs(pod *v1.Pod, container *v1.Container) bool {
	return containerHasExclusiveCPUs(cm.cpuManager, pod, container)
}

func (cm *containerManagerImpl) InternalContainerLifecycle() InternalContainerLifecycle {
	return &internalContainerLifecycleImpl{cm.cpuManager, cm.memoryManager, cm.topologyManager}
}

// Create a cgroup container manager.
func createManager(containerName string) (cgroups.Manager, error) {
	cg := &configs.Cgroup{
		Parent: "/",
		Name:   containerName,
		Resources: &configs.Resources{
			SkipDevices: true,
		},
		Systemd: false,
	}

	return manager.New(cg)
}

type KernelTunableBehavior string

const (
	KernelTunableWarn   KernelTunableBehavior = "warn"
	KernelTunableError  KernelTunableBehavior = "error"
	KernelTunableModify KernelTunableBehavior = "modify"
)

// setupKernelTunables validates kernel tunable flags are set as expected
// depending upon the specified option, it will either warn, error, or modify the kernel tunable flags
func setupKernelTunables(option KernelTunableBehavior) error {
	desiredState := map[string]int{
		utilsysctl.VMOvercommitMemory: utilsysctl.VMOvercommitMemoryAlways,
		utilsysctl.VMPanicOnOOM:       utilsysctl.VMPanicOnOOMInvokeOOMKiller,
		utilsysctl.KernelPanic:        utilsysctl.KernelPanicRebootTimeout,
		utilsysctl.KernelPanicOnOops:  utilsysctl.KernelPanicOnOopsAlways,
		utilsysctl.RootMaxKeys:        utilsysctl.RootMaxKeysSetting,
		utilsysctl.RootMaxBytes:       utilsysctl.RootMaxBytesSetting,
	}

	sysctl := utilsysctl.New()

	errList := []error{}
	for flag, expectedValue := range desiredState {
		val, err := sysctl.GetSysctl(flag)
		if err != nil {
			errList = append(errList, err)
			continue
		}
		if val == expectedValue {
			continue
		}

		switch option {
		case KernelTunableError:
			errList = append(errList, fmt.Errorf("invalid kernel flag: %v, expected value: %v, actual value: %v", flag, expectedValue, val))
		case KernelTunableWarn:
			klog.V(2).InfoS("Invalid kernel flag", "flag", flag, "expectedValue", expectedValue, "actualValue", val)
		case KernelTunableModify:
			klog.V(2).InfoS("Updating kernel flag", "flag", flag, "expectedValue", expectedValue, "actualValue", val)
			err = sysctl.SetSysctl(flag, expectedValue)
			if err != nil {
				if inuserns.RunningInUserNS() {
					if utilfeature.DefaultFeatureGate.Enabled(kubefeatures.KubeletInUserNamespace) {
						klog.V(2).InfoS("Updating kernel flag failed (running in UserNS, ignoring)", "flag", flag, "err", err)
						continue
					}
					klog.ErrorS(err, "Updating kernel flag failed (Hint: enable KubeletInUserNamespace feature flag to ignore the error)", "flag", flag)
				}
				errList = append(errList, err)
			}
		}
	}
	return utilerrors.NewAggregate(errList)
}

func (cm *containerManagerImpl) setupNode(activePods ActivePodsFunc) error {
	f, err := validateSystemRequirements(cm.mountUtil)
	if err != nil {
		return err
	}
	if !f.cpuHardcapping {
		cm.status.SoftRequirements = fmt.Errorf("CPU hardcapping unsupported")
	}
	b := KernelTunableModify
	if cm.GetNodeConfig().ProtectKernelDefaults {
		b = KernelTunableError
	}
	if err := setupKernelTunables(b); err != nil {
		return err
	}

	// Setup top level qos containers only if CgroupsPerQOS flag is specified as true
	if cm.NodeConfig.CgroupsPerQOS {
		if err := cm.createNodeAllocatableCgroups(); err != nil {
			return err
		}
		err = cm.qosContainerManager.Start(cm.GetNodeAllocatableAbsolute, activePods)
		if err != nil {
			return fmt.Errorf("failed to initialize top level QOS containers: %v", err)
		}
	}

	// Enforce Node Allocatable (if required)
	if err := cm.enforceNodeAllocatableCgroups(); err != nil {
		return err
	}

	systemContainers := []*systemContainer{}

	if cm.SystemCgroupsName != "" {
		if cm.SystemCgroupsName == "/" {
			return fmt.Errorf("system container cannot be root (\"/\")")
		}
		cont, err := newSystemCgroups(cm.SystemCgroupsName)
		if err != nil {
			return err
		}
		cont.ensureStateFunc = func(manager cgroups.Manager) error {
			return ensureSystemCgroups("/", manager)
		}
		systemContainers = append(systemContainers, cont)
	}

	if cm.KubeletCgroupsName != "" {
		cont, err := newSystemCgroups(cm.KubeletCgroupsName)
		if err != nil {
			return err
		}

		cont.ensureStateFunc = func(_ cgroups.Manager) error {
			return ensureProcessInContainerWithOOMScore(os.Getpid(), int(cm.KubeletOOMScoreAdj), cont.manager)
		}
		systemContainers = append(systemContainers, cont)
	} else {
		cm.periodicTasks = append(cm.periodicTasks, func() {
			if err := ensureProcessInContainerWithOOMScore(os.Getpid(), int(cm.KubeletOOMScoreAdj), nil); err != nil {
				klog.ErrorS(err, "Failed to ensure process in container with oom score")
				return
			}
			cont, err := getContainer(os.Getpid())
			if err != nil {
				klog.ErrorS(err, "Failed to find cgroups of kubelet")
				return
			}
			cm.Lock()
			defer cm.Unlock()

			cm.KubeletCgroupsName = cont
		})
	}

	cm.systemContainers = systemContainers
	return nil
}

func (cm *containerManagerImpl) GetNodeConfig() NodeConfig {
	cm.RLock()
	defer cm.RUnlock()
	return cm.NodeConfig
}

// GetPodCgroupRoot returns the literal cgroupfs value for the cgroup containing all pods.
func (cm *containerManagerImpl) GetPodCgroupRoot() string {
	return cm.cgroupManager.Name(cm.cgroupRoot)
}

func (cm *containerManagerImpl) GetMountedSubsystems() *CgroupSubsystems {
	return cm.subsystems
}

func (cm *containerManagerImpl) GetQOSContainersInfo() QOSContainersInfo {
	return cm.qosContainerManager.GetQOSContainersInfo()
}

func (cm *containerManagerImpl) UpdateQOSCgroups() error {
	return cm.qosContainerManager.UpdateCgroups()
}

func (cm *containerManagerImpl) Status() Status {
	cm.RLock()
	defer cm.RUnlock()
	return cm.status
}

func (cm *containerManagerImpl) Start(ctx context.Context, node *v1.Node,
	activePods ActivePodsFunc,
	getNode GetNodeFunc,
	sourcesReady config.SourcesReady,
	podStatusProvider status.PodStatusProvider,
	runtimeService internalapi.RuntimeService,
	localStorageCapacityIsolation bool) error {

	containerMap, containerRunningSet := buildContainerMapAndRunningSetFromRuntime(ctx, runtimeService)

	// Initialize DRA manager
	if utilfeature.DefaultFeatureGate.Enabled(kubefeatures.DynamicResourceAllocation) {
		err := cm.draManager.Start(ctx, dra.ActivePodsFunc(activePods), dra.GetNodeFunc(getNode), sourcesReady)
		if err != nil {
			return fmt.Errorf("start dra manager error: %w", err)
		}
	}

	// Initialize CPU manager
	err := cm.cpuManager.Start(cpumanager.ActivePodsFunc(activePods), sourcesReady, podStatusProvider, runtimeService, containerMap.Clone())
	if err != nil {
		return fmt.Errorf("start cpu manager error: %w", err)
	}

	// Initialize memory manager
	err = cm.memoryManager.Start(memorymanager.ActivePodsFunc(activePods), sourcesReady, podStatusProvider, runtimeService, containerMap.Clone())
	if err != nil {
		return fmt.Errorf("start memory manager error: %w", err)
	}

	// cache the node Info including resource capacity and
	// allocatable of the node
	cm.nodeInfo = node

	if localStorageCapacityIsolation {
		rootfs, err := cm.cadvisorInterface.RootFsInfo()
		if err != nil {
			return fmt.Errorf("failed to get rootfs info: %v", err)
		}
		for rName, rCap := range cadvisor.EphemeralStorageCapacityFromFsInfo(rootfs) {
			cm.capacity[rName] = rCap
		}
	}

	// Ensure that node allocatable configuration is valid.
	if err := cm.validateNodeAllocatable(); err != nil {
		return err
	}

	// Setup the node
	if err := cm.setupNode(activePods); err != nil {
		return err
	}

	// Don't run a background thread if there are no ensureStateFuncs.
	hasEnsureStateFuncs := false
	for _, cont := range cm.systemContainers {
		if cont.ensureStateFunc != nil {
			hasEnsureStateFuncs = true
			break
		}
	}
	if hasEnsureStateFuncs {
		// Run ensure state functions every minute.
		go wait.Until(func() {
			for _, cont := range cm.systemContainers {
				if cont.ensureStateFunc != nil {
					if err := cont.ensureStateFunc(cont.manager); err != nil {
						klog.InfoS("Failed to ensure state", "containerName", cont.name, "err", err)
					}
				}
			}
		}, time.Minute, wait.NeverStop)

	}

	if len(cm.periodicTasks) > 0 {
		go wait.Until(func() {
			for _, task := range cm.periodicTasks {
				if task != nil {
					task()
				}
			}
		}, 5*time.Minute, wait.NeverStop)
	}

	// Starts device manager.
	if err := cm.deviceManager.Start(devicemanager.ActivePodsFunc(activePods), sourcesReady, containerMap.Clone(), containerRunningSet); err != nil {
		return err
	}

	return nil
}

func (cm *containerManagerImpl) GetPluginRegistrationHandlers() map[string]cache.PluginHandler {
	res := map[string]cache.PluginHandler{
		pluginwatcherapi.DevicePlugin: cm.deviceManager.GetWatcherHandler(),
	}

	if utilfeature.DefaultFeatureGate.Enabled(kubefeatures.DynamicResourceAllocation) {
		res[pluginwatcherapi.DRAPlugin] = cm.draManager.GetWatcherHandler()
	}

	return res
}

func (cm *containerManagerImpl) GetHealthCheckers() []healthz.HealthChecker {
	return []healthz.HealthChecker{cm.deviceManager.GetHealthChecker()}
}

// TODO: move the GetResources logic to PodContainerManager.
func (cm *containerManagerImpl) GetResources(ctx context.Context, pod *v1.Pod, container *v1.Container) (*kubecontainer.RunContainerOptions, error) {
	logger := klog.FromContext(ctx)
	opts := &kubecontainer.RunContainerOptions{}
	if utilfeature.DefaultFeatureGate.Enabled(kubefeatures.DynamicResourceAllocation) {
		resOpts, err := cm.draManager.GetResources(pod, container)
		if err != nil {
			return nil, err
		}
		logger.V(5).Info("Determined CDI devices for pod", "pod", klog.KObj(pod), "cdiDevices", resOpts.CDIDevices)
		opts.CDIDevices = append(opts.CDIDevices, resOpts.CDIDevices...)
	}
	// Allocate should already be called during predicateAdmitHandler.Admit(),
	// just try to fetch device runtime information from cached state here
	devOpts, err := cm.deviceManager.GetDeviceRunContainerOptions(pod, container)
	if err != nil {
		return nil, err
	} else if devOpts == nil {
		return opts, nil
	}
	opts.Devices = append(opts.Devices, devOpts.Devices...)
	opts.Mounts = append(opts.Mounts, devOpts.Mounts...)
	opts.Envs = append(opts.Envs, devOpts.Envs...)
	opts.Annotations = append(opts.Annotations, devOpts.Annotations...)
	opts.CDIDevices = append(opts.CDIDevices, devOpts.CDIDevices...)
	return opts, nil
}

func (cm *containerManagerImpl) UpdatePluginResources(node *schedulerframework.NodeInfo, attrs *lifecycle.PodAdmitAttributes) error {
	return cm.deviceManager.UpdatePluginResources(node, attrs)
}

func (cm *containerManagerImpl) GetAllocateResourcesPodAdmitHandler() lifecycle.PodAdmitHandler {
	return cm.topologyManager
}

func (cm *containerManagerImpl) SystemCgroupsLimit() v1.ResourceList {
	cpuLimit := int64(0)

	// Sum up resources of all external containers.
	for _, cont := range cm.systemContainers {
		cpuLimit += cont.cpuMillicores
	}

	return v1.ResourceList{
		v1.ResourceCPU: *resource.NewMilliQuantity(
			cpuLimit,
			resource.DecimalSI),
	}
}

func isProcessRunningInHost(pid int) (bool, error) {
	// Get init pid namespace.
	initPidNs, err := os.Readlink("/proc/1/ns/pid")
	if err != nil {
		return false, fmt.Errorf("failed to find pid namespace of init process")
	}
	klog.V(10).InfoS("Found init PID namespace", "namespace", initPidNs)
	processPidNs, err := os.Readlink(fmt.Sprintf("/proc/%d/ns/pid", pid))
	if err != nil {
		return false, fmt.Errorf("failed to find pid namespace of process %q", pid)
	}
	klog.V(10).InfoS("Process info", "pid", pid, "namespace", processPidNs)
	return initPidNs == processPidNs, nil
}

func ensureProcessInContainerWithOOMScore(pid int, oomScoreAdj int, manager cgroups.Manager) error {
	if runningInHost, err := isProcessRunningInHost(pid); err != nil {
		// Err on the side of caution. Avoid moving the docker daemon unless we are able to identify its context.
		return err
	} else if !runningInHost {
		// Process is running inside a container. Don't touch that.
		klog.V(2).InfoS("PID is not running in the host namespace", "pid", pid)
		return nil
	}

	var errs []error
	if manager != nil {
		cont, err := getContainer(pid)
		if err != nil {
			errs = append(errs, fmt.Errorf("failed to find container of PID %d: %v", pid, err))
		}

		name := ""
		cgroups, err := manager.GetCgroups()
		if err != nil {
			errs = append(errs, fmt.Errorf("failed to get cgroups for %d: %v", pid, err))
		} else {
			name = cgroups.Name
		}

		if cont != name {
			err = manager.Apply(pid)
			if err != nil {
				errs = append(errs, fmt.Errorf("failed to move PID %d (in %q) to %q: %v", pid, cont, name, err))
			}
		}
	}

	// Also apply oom-score-adj to processes
	oomAdjuster := oom.NewOOMAdjuster()
	klog.V(5).InfoS("Attempting to apply oom_score_adj to process", "oomScoreAdj", oomScoreAdj, "pid", pid)
	if err := oomAdjuster.ApplyOOMScoreAdj(pid, oomScoreAdj); err != nil {
		klog.V(3).InfoS("Failed to apply oom_score_adj to process", "oomScoreAdj", oomScoreAdj, "pid", pid, "err", err)
		errs = append(errs, fmt.Errorf("failed to apply oom score %d to PID %d: %v", oomScoreAdj, pid, err))
	}
	return utilerrors.NewAggregate(errs)
}

// getContainer returns the cgroup associated with the specified pid.
// It enforces a unified hierarchy for memory and cpu cgroups.
// On systemd environments, it uses the name=systemd cgroup for the specified pid.
func getContainer(pid int) (string, error) {
	cgs, err := cgroups.ParseCgroupFile(fmt.Sprintf("/proc/%d/cgroup", pid))
	if err != nil {
		return "", err
	}

	if cgroups.IsCgroup2UnifiedMode() {
		c, found := cgs[""]
		if !found {
			return "", cgroups.NewNotFoundError("unified")
		}
		return c, nil
	}

	cpu, found := cgs["cpu"]
	if !found {
		return "", cgroups.NewNotFoundError("cpu")
	}
	memory, found := cgs["memory"]
	if !found {
		return "", cgroups.NewNotFoundError("memory")
	}

	// since we use this container for accounting, we need to ensure its a unified hierarchy.
	if cpu != memory {
		return "", fmt.Errorf("cpu and memory cgroup hierarchy not unified.  cpu: %s, memory: %s", cpu, memory)
	}

	// on systemd, every pid is in a unified cgroup hierarchy (name=systemd as seen in systemd-cgls)
	// cpu and memory accounting is off by default, users may choose to enable it per unit or globally.
	// users could enable CPU and memory accounting globally via /etc/systemd/system.conf (DefaultCPUAccounting=true DefaultMemoryAccounting=true).
	// users could also enable CPU and memory accounting per unit via CPUAccounting=true and MemoryAccounting=true
	// we only warn if accounting is not enabled for CPU or memory so as to not break local development flows where kubelet is launched in a terminal.
	// for example, the cgroup for the user session will be something like /user.slice/user-X.slice/session-X.scope, but the cpu and memory
	// cgroup will be the closest ancestor where accounting is performed (most likely /) on systems that launch docker containers.
	// as a result, on those systems, you will not get cpu or memory accounting statistics for kubelet.
	// in addition, you would not get memory or cpu accounting for the runtime unless accounting was enabled on its unit (or globally).
	if systemd, found := cgs["name=systemd"]; found {
		if systemd != cpu {
			klog.InfoS("CPUAccounting not enabled for process", "pid", pid)
		}
		if systemd != memory {
			klog.InfoS("MemoryAccounting not enabled for process", "pid", pid)
		}
		return systemd, nil
	}

	return cpu, nil
}

// Ensures the system container is created and all non-kernel threads and process 1
// without a container are moved to it.
//
// The reason of leaving kernel threads at root cgroup is that we don't want to tie the
// execution of these threads with to-be defined /system quota and create priority inversions.
func ensureSystemCgroups(rootCgroupPath string, manager cgroups.Manager) error {
	// Move non-kernel PIDs to the system container.
	// Only keep errors on latest attempt.
	var finalErr error
	for i := 0; i <= 10; i++ {
		allPids, err := cmutil.GetPids(rootCgroupPath)
		if err != nil {
			finalErr = fmt.Errorf("failed to list PIDs for root: %v", err)
			continue
		}

		// Remove kernel pids and other protected PIDs (pid 1, PIDs already in system & kubelet containers)
		pids := make([]int, 0, len(allPids))
		for _, pid := range allPids {
			if pid == 1 || isKernelPid(pid) {
				continue
			}

			pids = append(pids, pid)
		}

		// Check if we have moved all the non-kernel PIDs.
		if len(pids) == 0 {
			return nil
		}

		klog.V(3).InfoS("Moving non-kernel processes", "pids", pids)
		for _, pid := range pids {
			err := manager.Apply(pid)
			if err != nil {
				name := ""
				cgroups, err := manager.GetCgroups()
				if err == nil {
					name = cgroups.Name
				}

				finalErr = fmt.Errorf("failed to move PID %d into the system container %q: %v", pid, name, err)
			}
		}

	}

	return finalErr
}

// Determines whether the specified PID is a kernel PID.
func isKernelPid(pid int) bool {
	// Kernel threads have no associated executable.
	_, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
	return err != nil && os.IsNotExist(err)
}

// GetCapacity returns node capacity data for "cpu", "memory", "ephemeral-storage", and "huge-pages*"
// At present this method is only invoked when introspecting ephemeral storage
func (cm *containerManagerImpl) GetCapacity(localStorageCapacityIsolation bool) v1.ResourceList {
	if localStorageCapacityIsolation {
		// We store allocatable ephemeral-storage in the capacity property once we Start() the container manager
		if _, ok := cm.capacity[v1.ResourceEphemeralStorage]; !ok {
			// If we haven't yet stored the capacity for ephemeral-storage, we can try to fetch it directly from cAdvisor,
			if cm.cadvisorInterface != nil {
				rootfs, err := cm.cadvisorInterface.RootFsInfo()
				if err != nil {
					klog.ErrorS(err, "Unable to get rootfs data from cAdvisor interface")
					// If the rootfsinfo retrieval from cAdvisor fails for any reason, fallback to returning the capacity property with no ephemeral storage data
					return cm.capacity
				}
				// We don't want to mutate cm.capacity here so we'll manually construct a v1.ResourceList from it,
				// and add ephemeral-storage
				capacityWithEphemeralStorage := v1.ResourceList{}
				for rName, rQuant := range cm.capacity {
					capacityWithEphemeralStorage[rName] = rQuant
				}
				capacityWithEphemeralStorage[v1.ResourceEphemeralStorage] = cadvisor.EphemeralStorageCapacityFromFsInfo(rootfs)[v1.ResourceEphemeralStorage]
				return capacityWithEphemeralStorage
			}
		}
	}
	return cm.capacity
}

func (cm *containerManagerImpl) GetDevicePluginResourceCapacity() (v1.ResourceList, v1.ResourceList, []string) {
	return cm.deviceManager.GetCapacity()
}

func (cm *containerManagerImpl) GetDevices(podUID, containerName string) []*podresourcesapi.ContainerDevices {
	return containerDevicesFromResourceDeviceInstances(cm.deviceManager.GetDevices(podUID, containerName))
}

func (cm *containerManagerImpl) GetAllocatableDevices() []*podresourcesapi.ContainerDevices {
	return containerDevicesFromResourceDeviceInstances(cm.deviceManager.GetAllocatableDevices())
}

func (cm *containerManagerImpl) GetCPUs(podUID, containerName string) []int64 {
	if cm.cpuManager != nil {
		return int64Slice(cm.cpuManager.GetExclusiveCPUs(podUID, containerName).UnsortedList())
	}
	return []int64{}
}

func (cm *containerManagerImpl) GetAllocatableCPUs() []int64 {
	if cm.cpuManager != nil {
		return int64Slice(cm.cpuManager.GetAllocatableCPUs().UnsortedList())
	}
	return []int64{}
}

func (cm *containerManagerImpl) GetMemory(podUID, containerName string) []*podresourcesapi.ContainerMemory {
	if cm.memoryManager == nil {
		return []*podresourcesapi.ContainerMemory{}
	}

	return containerMemoryFromBlock(cm.memoryManager.GetMemory(podUID, containerName))
}

func (cm *containerManagerImpl) GetAllocatableMemory() []*podresourcesapi.ContainerMemory {
	if cm.memoryManager == nil {
		return []*podresourcesapi.ContainerMemory{}
	}

	return containerMemoryFromBlock(cm.memoryManager.GetAllocatableMemory())
}

func (cm *containerManagerImpl) GetDynamicResources(pod *v1.Pod, container *v1.Container) []*podresourcesapi.DynamicResource {
	if !utilfeature.DefaultFeatureGate.Enabled(kubefeatures.DynamicResourceAllocation) {
		return []*podresourcesapi.DynamicResource{}
	}

	var containerDynamicResources []*podresourcesapi.DynamicResource
	containerClaimInfos, err := cm.draManager.GetContainerClaimInfos(pod, container)
	if err != nil {
		klog.ErrorS(err, "Unable to get container claim info state")
		return []*podresourcesapi.DynamicResource{}
	}
	for _, containerClaimInfo := range containerClaimInfos {
		var claimResources []*podresourcesapi.ClaimResource
		for driverName, driverState := range containerClaimInfo.DriverState {
			var cdiDevices []*podresourcesapi.CDIDevice
			for _, device := range driverState.Devices {
				for _, cdiDeviceID := range device.CDIDeviceIDs {
					cdiDevices = append(cdiDevices, &podresourcesapi.CDIDevice{Name: cdiDeviceID})
				}
				resources := &podresourcesapi.ClaimResource{
					CDIDevices: cdiDevices,
					DriverName: driverName,
					PoolName:   device.PoolName,
					DeviceName: device.DeviceName,
				}
				claimResources = append(claimResources, resources)
			}
		}
		containerDynamicResource := podresourcesapi.DynamicResource{
			ClaimName:      containerClaimInfo.ClaimName,
			ClaimNamespace: containerClaimInfo.Namespace,
			ClaimResources: claimResources,
		}
		containerDynamicResources = append(containerDynamicResources, &containerDynamicResource)
	}
	return containerDynamicResources
}

func (cm *containerManagerImpl) ShouldResetExtendedResourceCapacity() bool {
	return cm.deviceManager.ShouldResetExtendedResourceCapacity()
}

func (cm *containerManagerImpl) UpdateAllocatedDevices() {
	cm.deviceManager.UpdateAllocatedDevices()
}

func containerMemoryFromBlock(blocks []memorymanagerstate.Block) []*podresourcesapi.ContainerMemory {
	var containerMemories []*podresourcesapi.ContainerMemory

	for _, b := range blocks {
		containerMemory := podresourcesapi.ContainerMemory{
			MemoryType: string(b.Type),
			Size_:      b.Size,
			Topology: &podresourcesapi.TopologyInfo{
				Nodes: []*podresourcesapi.NUMANode{},
			},
		}

		for _, numaNodeID := range b.NUMAAffinity {
			containerMemory.Topology.Nodes = append(containerMemory.Topology.Nodes, &podresourcesapi.NUMANode{ID: int64(numaNodeID)})
		}

		containerMemories = append(containerMemories, &containerMemory)
	}

	return containerMemories
}

func (cm *containerManagerImpl) PrepareDynamicResources(ctx context.Context, pod *v1.Pod) error {
	return cm.draManager.PrepareResources(ctx, pod)
}

func (cm *containerManagerImpl) UnprepareDynamicResources(ctx context.Context, pod *v1.Pod) error {
	return cm.draManager.UnprepareResources(ctx, pod)
}

func (cm *containerManagerImpl) PodMightNeedToUnprepareResources(UID types.UID) bool {
	return cm.draManager.PodMightNeedToUnprepareResources(UID)
}

func (cm *containerManagerImpl) UpdateAllocatedResourcesStatus(pod *v1.Pod, status *v1.PodStatus) {

	// For now we only support Device Plugin
	cm.deviceManager.UpdateAllocatedResourcesStatus(pod, status)

	// TODO(SergeyKanzhelev, https://kep.k8s.io/4680): add support for DRA resources which is planned for the next iteration of a KEP.
}

func (cm *containerManagerImpl) Updates() <-chan resourceupdates.Update {
	// TODO(SergeyKanzhelev, https://kep.k8s.io/4680): add support for DRA resources, for now only use device plugin updates. DRA support is planned for the next iteration of a KEP.
	return cm.deviceManager.Updates()
}
