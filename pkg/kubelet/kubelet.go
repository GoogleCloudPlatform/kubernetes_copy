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

package kubelet

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	sysruntime "runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	cadvisorapi "github.com/google/cadvisor/info/v1"
	inuserns "github.com/moby/sys/userns"
	"github.com/opencontainers/selinux/go-selinux"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	semconv "go.opentelemetry.io/otel/semconv/v1.12.0"
	"go.opentelemetry.io/otel/trace"
	"k8s.io/client-go/informers"
	"k8s.io/mount-utils"

	v1qos "k8s.io/kubernetes/pkg/apis/core/v1/helper/qos"
	utilfs "k8s.io/kubernetes/pkg/util/filesystem"
	netutils "k8s.io/utils/net"
	"k8s.io/utils/ptr"

	v1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	clientset "k8s.io/client-go/kubernetes"
	v1core "k8s.io/client-go/kubernetes/typed/core/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/certificate"
	"k8s.io/client-go/util/flowcontrol"
	cloudprovider "k8s.io/cloud-provider"
	"k8s.io/component-helpers/apimachinery/lease"
	internalapi "k8s.io/cri-api/pkg/apis"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
	remote "k8s.io/cri-client/pkg"
	"k8s.io/klog/v2"
	pluginwatcherapi "k8s.io/kubelet/pkg/apis/pluginregistration/v1"
	statsapi "k8s.io/kubelet/pkg/apis/stats/v1alpha1"
	podutil "k8s.io/kubernetes/pkg/api/v1/pod"
	"k8s.io/kubernetes/pkg/api/v1/resource"
	"k8s.io/kubernetes/pkg/features"
	kubeletconfiginternal "k8s.io/kubernetes/pkg/kubelet/apis/config"
	"k8s.io/kubernetes/pkg/kubelet/apis/config/v1beta1"
	"k8s.io/kubernetes/pkg/kubelet/apis/podresources"
	"k8s.io/kubernetes/pkg/kubelet/cadvisor"
	kubeletcertificate "k8s.io/kubernetes/pkg/kubelet/certificate"
	"k8s.io/kubernetes/pkg/kubelet/cloudresource"
	"k8s.io/kubernetes/pkg/kubelet/clustertrustbundle"
	"k8s.io/kubernetes/pkg/kubelet/cm"
	"k8s.io/kubernetes/pkg/kubelet/cm/topologymanager"
	"k8s.io/kubernetes/pkg/kubelet/config"
	"k8s.io/kubernetes/pkg/kubelet/configmap"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	"k8s.io/kubernetes/pkg/kubelet/events"
	"k8s.io/kubernetes/pkg/kubelet/eviction"
	"k8s.io/kubernetes/pkg/kubelet/images"
	"k8s.io/kubernetes/pkg/kubelet/kuberuntime"
	"k8s.io/kubernetes/pkg/kubelet/lifecycle"
	"k8s.io/kubernetes/pkg/kubelet/logs"
	"k8s.io/kubernetes/pkg/kubelet/metrics"
	"k8s.io/kubernetes/pkg/kubelet/metrics/collectors"
	"k8s.io/kubernetes/pkg/kubelet/network/dns"
	"k8s.io/kubernetes/pkg/kubelet/nodeshutdown"
	oomwatcher "k8s.io/kubernetes/pkg/kubelet/oom"
	"k8s.io/kubernetes/pkg/kubelet/pleg"
	"k8s.io/kubernetes/pkg/kubelet/pluginmanager"
	plugincache "k8s.io/kubernetes/pkg/kubelet/pluginmanager/cache"
	kubepod "k8s.io/kubernetes/pkg/kubelet/pod"
	"k8s.io/kubernetes/pkg/kubelet/preemption"
	"k8s.io/kubernetes/pkg/kubelet/prober"
	proberesults "k8s.io/kubernetes/pkg/kubelet/prober/results"
	"k8s.io/kubernetes/pkg/kubelet/runtimeclass"
	"k8s.io/kubernetes/pkg/kubelet/secret"
	"k8s.io/kubernetes/pkg/kubelet/server"
	servermetrics "k8s.io/kubernetes/pkg/kubelet/server/metrics"
	serverstats "k8s.io/kubernetes/pkg/kubelet/server/stats"
	"k8s.io/kubernetes/pkg/kubelet/stats"
	"k8s.io/kubernetes/pkg/kubelet/status"
	"k8s.io/kubernetes/pkg/kubelet/sysctl"
	"k8s.io/kubernetes/pkg/kubelet/token"
	kubetypes "k8s.io/kubernetes/pkg/kubelet/types"
	"k8s.io/kubernetes/pkg/kubelet/userns"
	"k8s.io/kubernetes/pkg/kubelet/util"
	"k8s.io/kubernetes/pkg/kubelet/util/format"
	"k8s.io/kubernetes/pkg/kubelet/util/manager"
	"k8s.io/kubernetes/pkg/kubelet/util/queue"
	"k8s.io/kubernetes/pkg/kubelet/util/sliceutils"
	"k8s.io/kubernetes/pkg/kubelet/volumemanager"
	"k8s.io/kubernetes/pkg/kubelet/watchdog"
	httpprobe "k8s.io/kubernetes/pkg/probe/http"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/tainttoleration"
	"k8s.io/kubernetes/pkg/security/apparmor"
	"k8s.io/kubernetes/pkg/util/oom"
	"k8s.io/kubernetes/pkg/volume"
	"k8s.io/kubernetes/pkg/volume/csi"
	"k8s.io/kubernetes/pkg/volume/util/hostutil"
	"k8s.io/kubernetes/pkg/volume/util/subpath"
	"k8s.io/kubernetes/pkg/volume/util/volumepathhandler"
	"k8s.io/utils/clock"
)

const (
	// Max amount of time to wait for the container runtime to come up.
	maxWaitForContainerRuntime = 30 * time.Second

	// nodeStatusUpdateRetry specifies how many times kubelet retries when posting node status failed.
	nodeStatusUpdateRetry = 5

	// nodeReadyGracePeriod is the period to allow for before fast status update is
	// terminated and container runtime not being ready is logged without verbosity guard.
	nodeReadyGracePeriod = 120 * time.Second

	// DefaultContainerLogsDir is the location of container logs.
	DefaultContainerLogsDir = "/var/log/containers"

	// MaxContainerBackOff is the max backoff period for container restarts, exported for the e2e test
	MaxContainerBackOff = v1beta1.MaxContainerBackOff

	// MaxImageBackOff is the max backoff period for image pulls, exported for the e2e test
	MaxImageBackOff = 300 * time.Second

	// Period for performing global cleanup tasks.
	housekeepingPeriod = time.Second * 2

	// Duration at which housekeeping failed to satisfy the invariant that
	// housekeeping should be fast to avoid blocking pod config (while
	// housekeeping is running no new pods are started or deleted).
	housekeepingWarningDuration = time.Second * 1

	// Period after which the runtime cache expires - set to slightly longer than
	// the expected length between housekeeping periods, which explicitly refreshes
	// the cache.
	runtimeCacheRefreshPeriod = housekeepingPeriod + housekeepingWarningDuration

	// Period for performing eviction monitoring.
	// ensure this is kept in sync with internal cadvisor housekeeping.
	evictionMonitoringPeriod = time.Second * 10

	// The path in containers' filesystems where the hosts file is mounted.
	linuxEtcHostsPath   = "/etc/hosts"
	windowsEtcHostsPath = "C:\\Windows\\System32\\drivers\\etc\\hosts"

	// Capacity of the channel for receiving pod lifecycle events. This number
	// is a bit arbitrary and may be adjusted in the future.
	plegChannelCapacity = 1000

	// Generic PLEG relies on relisting for discovering container events.
	// A longer period means that kubelet will take longer to detect container
	// changes and to update pod status. On the other hand, a shorter period
	// will cause more frequent relisting (e.g., container runtime operations),
	// leading to higher cpu usage.
	// Note that even though we set the period to 1s, the relisting itself can
	// take more than 1s to finish if the container runtime responds slowly
	// and/or when there are many container changes in one cycle.
	genericPlegRelistPeriod    = time.Second * 1
	genericPlegRelistThreshold = time.Minute * 3

	// Generic PLEG relist period and threshold when used with Evented PLEG.
	eventedPlegRelistPeriod     = time.Second * 300
	eventedPlegRelistThreshold  = time.Minute * 10
	eventedPlegMaxStreamRetries = 5

	// backOffPeriod is the period to back off when pod syncing results in an
	// error.
	backOffPeriod = time.Second * 10

	// Initial period for the exponential backoff for container restarts.
	containerBackOffPeriod = time.Second * 10

	// Initial period for the exponential backoff for image pulls.
	imageBackOffPeriod = time.Second * 10

	// ContainerGCPeriod is the period for performing container garbage collection.
	ContainerGCPeriod = time.Minute
	// ImageGCPeriod is the period for performing image garbage collection.
	ImageGCPeriod = 5 * time.Minute

	// Minimum number of dead containers to keep in a pod
	minDeadContainerInPod = 1

	// nodeLeaseRenewIntervalFraction is the fraction of lease duration to renew the lease
	nodeLeaseRenewIntervalFraction = 0.25

	// instrumentationScope is the name of OpenTelemetry instrumentation scope
	instrumentationScope = "k8s.io/kubernetes/pkg/kubelet"
)

var (
	// ContainerLogsDir can be overwritten for testing usage
	ContainerLogsDir = DefaultContainerLogsDir
	etcHostsPath     = getContainerEtcHostsPath()

	admissionRejectionReasons = sets.New[string](
		lifecycle.AppArmorNotAdmittedReason,
		lifecycle.PodOSSelectorNodeLabelDoesNotMatch,
		lifecycle.PodOSNotSupported,
		lifecycle.InvalidNodeInfo,
		lifecycle.InitContainerRestartPolicyForbidden,
		lifecycle.UnexpectedAdmissionError,
		lifecycle.UnknownReason,
		lifecycle.UnexpectedPredicateFailureType,
		lifecycle.OutOfCPU,
		lifecycle.OutOfMemory,
		lifecycle.OutOfEphemeralStorage,
		lifecycle.OutOfPods,
		tainttoleration.ErrReasonNotMatch,
		eviction.Reason,
		sysctl.ForbiddenReason,
		topologymanager.ErrorTopologyAffinity,
		nodeshutdown.NodeShutdownNotAdmittedReason,
	)

	// This is exposed for unit tests.
	goos = sysruntime.GOOS
)

func getContainerEtcHostsPath() string {
	if goos == "windows" {
		return windowsEtcHostsPath
	}
	return linuxEtcHostsPath
}

// SyncHandler is an interface implemented by Kubelet, for testability
type SyncHandler interface {
	HandlePodAdditions(pods []*v1.Pod)
	HandlePodUpdates(pods []*v1.Pod)
	HandlePodRemoves(pods []*v1.Pod)
	HandlePodReconcile(pods []*v1.Pod)
	HandlePodSyncs(pods []*v1.Pod)
	HandlePodCleanups(ctx context.Context) error
}

// Option is a functional option type for Kubelet
type Option func(*Kubelet)

// Bootstrap is a bootstrapping interface for kubelet, targets the initialization protocol
type Bootstrap interface {
	GetConfiguration() kubeletconfiginternal.KubeletConfiguration
	BirthCry()
	StartGarbageCollection()
	ListenAndServe(kubeCfg *kubeletconfiginternal.KubeletConfiguration, tlsOptions *server.TLSOptions, auth server.AuthInterface, tp trace.TracerProvider)
	ListenAndServeReadOnly(address net.IP, port uint, tp trace.TracerProvider)
	ListenAndServePodResources()
	Run(<-chan kubetypes.PodUpdate)
}

// Dependencies is a bin for things we might consider "injected dependencies" -- objects constructed
// at runtime that are necessary for running the Kubelet. This is a temporary solution for grouping
// these objects while we figure out a more comprehensive dependency injection story for the Kubelet.
type Dependencies struct {
	Options []Option

	// Injected Dependencies
	Auth                      server.AuthInterface
	CAdvisorInterface         cadvisor.Interface
	Cloud                     cloudprovider.Interface
	ContainerManager          cm.ContainerManager
	EventClient               v1core.EventsGetter
	HeartbeatClient           clientset.Interface
	OnHeartbeatFailure        func()
	KubeClient                clientset.Interface
	Mounter                   mount.Interface
	HostUtil                  hostutil.HostUtils
	OOMAdjuster               *oom.OOMAdjuster
	OSInterface               kubecontainer.OSInterface
	PodConfig                 *config.PodConfig
	ProbeManager              prober.Manager
	Recorder                  record.EventRecorder
	Subpather                 subpath.Interface
	TracerProvider            trace.TracerProvider
	VolumePlugins             []volume.VolumePlugin
	DynamicPluginProber       volume.DynamicPluginProber
	TLSOptions                *server.TLSOptions
	RemoteRuntimeService      internalapi.RuntimeService
	RemoteImageService        internalapi.ImageManagerService
	PodStartupLatencyTracker  util.PodStartupLatencyTracker
	NodeStartupLatencyTracker util.NodeStartupLatencyTracker
	// remove it after cadvisor.UsingLegacyCadvisorStats dropped.
	useLegacyCadvisorStats bool
}

// makePodSourceConfig creates a config.PodConfig from the given
// KubeletConfiguration or returns an error.
func makePodSourceConfig(kubeCfg *kubeletconfiginternal.KubeletConfiguration, kubeDeps *Dependencies, nodeName types.NodeName, nodeHasSynced func() bool) (*config.PodConfig, error) {
	manifestURLHeader := make(http.Header)
	if len(kubeCfg.StaticPodURLHeader) > 0 {
		for k, v := range kubeCfg.StaticPodURLHeader {
			for i := range v {
				manifestURLHeader.Add(k, v[i])
			}
		}
	}

	// source of all configuration
	cfg := config.NewPodConfig(config.PodConfigNotificationIncremental, kubeDeps.Recorder, kubeDeps.PodStartupLatencyTracker)

	// TODO:  it needs to be replaced by a proper context in the future
	ctx := context.TODO()

	// define file config source
	if kubeCfg.StaticPodPath != "" {
		klog.InfoS("Adding static pod path", "path", kubeCfg.StaticPodPath)
		config.NewSourceFile(kubeCfg.StaticPodPath, nodeName, kubeCfg.FileCheckFrequency.Duration, cfg.Channel(ctx, kubetypes.FileSource))
	}

	// define url config source
	if kubeCfg.StaticPodURL != "" {
		klog.InfoS("Adding pod URL with HTTP header", "URL", kubeCfg.StaticPodURL, "header", manifestURLHeader)
		config.NewSourceURL(kubeCfg.StaticPodURL, manifestURLHeader, nodeName, kubeCfg.HTTPCheckFrequency.Duration, cfg.Channel(ctx, kubetypes.HTTPSource))
	}

	if kubeDeps.KubeClient != nil {
		klog.InfoS("Adding apiserver pod source")
		config.NewSourceApiserver(kubeDeps.KubeClient, nodeName, nodeHasSynced, cfg.Channel(ctx, kubetypes.ApiserverSource))
	}
	return cfg, nil
}

// PreInitRuntimeService will init runtime service before RunKubelet.
func PreInitRuntimeService(kubeCfg *kubeletconfiginternal.KubeletConfiguration, kubeDeps *Dependencies) error {
	remoteImageEndpoint := kubeCfg.ImageServiceEndpoint
	if remoteImageEndpoint == "" && kubeCfg.ContainerRuntimeEndpoint != "" {
		remoteImageEndpoint = kubeCfg.ContainerRuntimeEndpoint
	}
	var err error

	var tp trace.TracerProvider
	if utilfeature.DefaultFeatureGate.Enabled(features.KubeletTracing) {
		tp = kubeDeps.TracerProvider
	}

	logger := klog.Background()
	if kubeDeps.RemoteRuntimeService, err = remote.NewRemoteRuntimeService(kubeCfg.ContainerRuntimeEndpoint, kubeCfg.RuntimeRequestTimeout.Duration, tp, &logger); err != nil {
		return err
	}
	if kubeDeps.RemoteImageService, err = remote.NewRemoteImageService(remoteImageEndpoint, kubeCfg.RuntimeRequestTimeout.Duration, tp, &logger); err != nil {
		return err
	}

	kubeDeps.useLegacyCadvisorStats = cadvisor.UsingLegacyCadvisorStats(kubeCfg.ContainerRuntimeEndpoint)

	return nil
}

// NewMainKubelet instantiates a new Kubelet object along with all the required internal modules.
// No initialization of Kubelet and its modules should happen here.
func NewMainKubelet(kubeCfg *kubeletconfiginternal.KubeletConfiguration,
	kubeDeps *Dependencies,
	crOptions *config.ContainerRuntimeOptions,
	hostname string,
	hostnameOverridden bool,
	nodeName types.NodeName,
	nodeIPs []net.IP,
	providerID string,
	cloudProvider string,
	certDirectory string,
	rootDirectory string,
	podLogsDirectory string,
	imageCredentialProviderConfigFile string,
	imageCredentialProviderBinDir string,
	registerNode bool,
	registerWithTaints []v1.Taint,
	allowedUnsafeSysctls []string,
	experimentalMounterPath string,
	kernelMemcgNotification bool,
	experimentalNodeAllocatableIgnoreEvictionThreshold bool,
	minimumGCAge metav1.Duration,
	maxPerPodContainerCount int32,
	maxContainerCount int32,
	registerSchedulable bool,
	nodeLabels map[string]string,
	nodeStatusMaxImages int32,
	seccompDefault bool,
) (*Kubelet, error) {
	ctx := context.Background()
	logger := klog.TODO()

	if rootDirectory == "" {
		return nil, fmt.Errorf("invalid root directory %q", rootDirectory)
	}
	if podLogsDirectory == "" {
		return nil, errors.New("pod logs root directory is empty")
	}
	if kubeCfg.SyncFrequency.Duration <= 0 {
		return nil, fmt.Errorf("invalid sync frequency %d", kubeCfg.SyncFrequency.Duration)
	}

	if !cloudprovider.IsExternal(cloudProvider) && len(cloudProvider) != 0 {
		cloudprovider.DisableWarningForProvider(cloudProvider)
		return nil, fmt.Errorf("cloud provider %q was specified, but built-in cloud providers are disabled. Please set --cloud-provider=external and migrate to an external cloud provider", cloudProvider)
	}

	var nodeHasSynced cache.InformerSynced
	var nodeLister corelisters.NodeLister

	// If kubeClient == nil, we are running in standalone mode (i.e. no API servers)
	// If not nil, we are running as part of a cluster and should sync w/API
	if kubeDeps.KubeClient != nil {
		kubeInformers := informers.NewSharedInformerFactoryWithOptions(kubeDeps.KubeClient, 0, informers.WithTweakListOptions(func(options *metav1.ListOptions) {
			options.FieldSelector = fields.Set{metav1.ObjectNameField: string(nodeName)}.String()
		}))
		nodeLister = kubeInformers.Core().V1().Nodes().Lister()
		nodeHasSynced = func() bool {
			return kubeInformers.Core().V1().Nodes().Informer().HasSynced()
		}
		kubeInformers.Start(wait.NeverStop)
		klog.InfoS("Attempting to sync node with API server")
	} else {
		// we don't have a client to sync!
		nodeIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
		nodeLister = corelisters.NewNodeLister(nodeIndexer)
		nodeHasSynced = func() bool { return true }
		klog.InfoS("Kubelet is running in standalone mode, will skip API server sync")
	}

	if kubeDeps.PodConfig == nil {
		var err error
		kubeDeps.PodConfig, err = makePodSourceConfig(kubeCfg, kubeDeps, nodeName, nodeHasSynced)
		if err != nil {
			return nil, err
		}
	}

	containerGCPolicy := kubecontainer.GCPolicy{
		MinAge:             minimumGCAge.Duration,
		MaxPerPodContainer: int(maxPerPodContainerCount),
		MaxContainers:      int(maxContainerCount),
	}

	daemonEndpoints := &v1.NodeDaemonEndpoints{
		KubeletEndpoint: v1.DaemonEndpoint{Port: kubeCfg.Port},
	}

	imageGCPolicy := images.ImageGCPolicy{
		MinAge:               kubeCfg.ImageMinimumGCAge.Duration,
		HighThresholdPercent: int(kubeCfg.ImageGCHighThresholdPercent),
		LowThresholdPercent:  int(kubeCfg.ImageGCLowThresholdPercent),
	}

	if utilfeature.DefaultFeatureGate.Enabled(features.ImageMaximumGCAge) {
		imageGCPolicy.MaxAge = kubeCfg.ImageMaximumGCAge.Duration
	} else if kubeCfg.ImageMaximumGCAge.Duration != 0 {
		klog.InfoS("ImageMaximumGCAge flag enabled, but corresponding feature gate is not enabled. Ignoring flag.")
	}

	enforceNodeAllocatable := kubeCfg.EnforceNodeAllocatable
	if experimentalNodeAllocatableIgnoreEvictionThreshold {
		// Do not provide kubeCfg.EnforceNodeAllocatable to eviction threshold parsing if we are not enforcing Evictions
		enforceNodeAllocatable = []string{}
	}
	thresholds, err := eviction.ParseThresholdConfig(enforceNodeAllocatable, kubeCfg.EvictionHard, kubeCfg.EvictionSoft, kubeCfg.EvictionSoftGracePeriod, kubeCfg.EvictionMinimumReclaim)
	if err != nil {
		return nil, err
	}
	evictionConfig := eviction.Config{
		PressureTransitionPeriod: kubeCfg.EvictionPressureTransitionPeriod.Duration,
		MaxPodGracePeriodSeconds: int64(kubeCfg.EvictionMaxPodGracePeriod),
		Thresholds:               thresholds,
		KernelMemcgNotification:  kernelMemcgNotification,
		PodCgroupRoot:            kubeDeps.ContainerManager.GetPodCgroupRoot(),
	}

	var serviceLister corelisters.ServiceLister
	var serviceHasSynced cache.InformerSynced
	if kubeDeps.KubeClient != nil {
		// don't watch headless services, they are not needed since this informer is only used to create the environment variables for pods.
		// See https://issues.k8s.io/122394
		kubeInformers := informers.NewSharedInformerFactoryWithOptions(kubeDeps.KubeClient, 0, informers.WithTweakListOptions(func(options *metav1.ListOptions) {
			options.FieldSelector = fields.OneTermNotEqualSelector("spec.clusterIP", v1.ClusterIPNone).String()
		}))
		serviceLister = kubeInformers.Core().V1().Services().Lister()
		serviceHasSynced = kubeInformers.Core().V1().Services().Informer().HasSynced
		kubeInformers.Start(wait.NeverStop)
	} else {
		serviceIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
		serviceLister = corelisters.NewServiceLister(serviceIndexer)
		serviceHasSynced = func() bool { return true }
	}

	// construct a node reference used for events
	nodeRef := &v1.ObjectReference{
		Kind:      "Node",
		Name:      string(nodeName),
		UID:       types.UID(nodeName),
		Namespace: "",
	}

	oomWatcher, err := oomwatcher.NewWatcher(kubeDeps.Recorder)
	if err != nil {
		if inuserns.RunningInUserNS() {
			if utilfeature.DefaultFeatureGate.Enabled(features.KubeletInUserNamespace) {
				// oomwatcher.NewWatcher returns "open /dev/kmsg: operation not permitted" error,
				// when running in a user namespace with sysctl value `kernel.dmesg_restrict=1`.
				klog.V(2).InfoS("Failed to create an oomWatcher (running in UserNS, ignoring)", "err", err)
				oomWatcher = nil
			} else {
				klog.ErrorS(err, "Failed to create an oomWatcher (running in UserNS, Hint: enable KubeletInUserNamespace feature flag to ignore the error)")
				return nil, err
			}
		} else {
			return nil, err
		}
	}

	clusterDNS := make([]net.IP, 0, len(kubeCfg.ClusterDNS))
	for _, ipEntry := range kubeCfg.ClusterDNS {
		ip := netutils.ParseIPSloppy(ipEntry)
		if ip == nil {
			klog.InfoS("Invalid clusterDNS IP", "IP", ipEntry)
		} else {
			clusterDNS = append(clusterDNS, ip)
		}
	}

	// A TLS transport is needed to make HTTPS-based container lifecycle requests,
	// but we do not have the information necessary to do TLS verification.
	//
	// This client must not be modified to include credentials, because it is
	// critical that credentials not leak from the client to arbitrary hosts.
	insecureContainerLifecycleHTTPClient := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
		CheckRedirect: httpprobe.RedirectChecker(false),
	}

	tracer := kubeDeps.TracerProvider.Tracer(instrumentationScope)

	klet := &Kubelet{
		hostname:                       hostname,
		hostnameOverridden:             hostnameOverridden,
		nodeName:                       nodeName,
		kubeClient:                     kubeDeps.KubeClient,
		heartbeatClient:                kubeDeps.HeartbeatClient,
		onRepeatedHeartbeatFailure:     kubeDeps.OnHeartbeatFailure,
		rootDirectory:                  filepath.Clean(rootDirectory),
		podLogsDirectory:               podLogsDirectory,
		resyncInterval:                 kubeCfg.SyncFrequency.Duration,
		sourcesReady:                   config.NewSourcesReady(kubeDeps.PodConfig.SeenAllSources),
		registerNode:                   registerNode,
		registerWithTaints:             registerWithTaints,
		registerSchedulable:            registerSchedulable,
		dnsConfigurer:                  dns.NewConfigurer(kubeDeps.Recorder, nodeRef, nodeIPs, clusterDNS, kubeCfg.ClusterDomain, kubeCfg.ResolverConfig),
		serviceLister:                  serviceLister,
		serviceHasSynced:               serviceHasSynced,
		nodeLister:                     nodeLister,
		nodeHasSynced:                  nodeHasSynced,
		streamingConnectionIdleTimeout: kubeCfg.StreamingConnectionIdleTimeout.Duration,
		recorder:                       kubeDeps.Recorder,
		cadvisor:                       kubeDeps.CAdvisorInterface,
		cloud:                          kubeDeps.Cloud,
		externalCloudProvider:          cloudprovider.IsExternal(cloudProvider),
		providerID:                     providerID,
		nodeRef:                        nodeRef,
		nodeLabels:                     nodeLabels,
		nodeStatusUpdateFrequency:      kubeCfg.NodeStatusUpdateFrequency.Duration,
		nodeStatusReportFrequency:      kubeCfg.NodeStatusReportFrequency.Duration,
		os:                             kubeDeps.OSInterface,
		oomWatcher:                     oomWatcher,
		cgroupsPerQOS:                  kubeCfg.CgroupsPerQOS,
		cgroupRoot:                     kubeCfg.CgroupRoot,
		mounter:                        kubeDeps.Mounter,
		hostutil:                       kubeDeps.HostUtil,
		subpather:                      kubeDeps.Subpather,
		maxPods:                        int(kubeCfg.MaxPods),
		podsPerCore:                    int(kubeCfg.PodsPerCore),
		syncLoopMonitor:                atomic.Value{},
		daemonEndpoints:                daemonEndpoints,
		containerManager:               kubeDeps.ContainerManager,
		nodeIPs:                        nodeIPs,
		nodeIPValidator:                validateNodeIP,
		clock:                          clock.RealClock{},
		enableControllerAttachDetach:   kubeCfg.EnableControllerAttachDetach,
		makeIPTablesUtilChains:         kubeCfg.MakeIPTablesUtilChains,
		nodeStatusMaxImages:            nodeStatusMaxImages,
		tracer:                         tracer,
		nodeStartupLatencyTracker:      kubeDeps.NodeStartupLatencyTracker,
	}

	if klet.cloud != nil {
		klet.cloudResourceSyncManager = cloudresource.NewSyncManager(klet.cloud, nodeName, klet.nodeStatusUpdateFrequency)
	}

	var secretManager secret.Manager
	var configMapManager configmap.Manager
	if klet.kubeClient != nil {
		switch kubeCfg.ConfigMapAndSecretChangeDetectionStrategy {
		case kubeletconfiginternal.WatchChangeDetectionStrategy:
			secretManager = secret.NewWatchingSecretManager(klet.kubeClient, klet.resyncInterval)
			configMapManager = configmap.NewWatchingConfigMapManager(klet.kubeClient, klet.resyncInterval)
		case kubeletconfiginternal.TTLCacheChangeDetectionStrategy:
			secretManager = secret.NewCachingSecretManager(
				klet.kubeClient, manager.GetObjectTTLFromNodeFunc(klet.GetNode))
			configMapManager = configmap.NewCachingConfigMapManager(
				klet.kubeClient, manager.GetObjectTTLFromNodeFunc(klet.GetNode))
		case kubeletconfiginternal.GetChangeDetectionStrategy:
			secretManager = secret.NewSimpleSecretManager(klet.kubeClient)
			configMapManager = configmap.NewSimpleConfigMapManager(klet.kubeClient)
		default:
			return nil, fmt.Errorf("unknown configmap and secret manager mode: %v", kubeCfg.ConfigMapAndSecretChangeDetectionStrategy)
		}

		klet.secretManager = secretManager
		klet.configMapManager = configMapManager
	}

	machineInfo, err := klet.cadvisor.MachineInfo()
	if err != nil {
		return nil, err
	}
	// Avoid collector collects it as a timestamped metric
	// See PR #95210 and #97006 for more details.
	machineInfo.Timestamp = time.Time{}
	klet.setCachedMachineInfo(machineInfo)

	imageBackOff := flowcontrol.NewBackOff(imageBackOffPeriod, MaxImageBackOff)

	klet.livenessManager = proberesults.NewManager()
	klet.readinessManager = proberesults.NewManager()
	klet.startupManager = proberesults.NewManager()
	klet.podCache = kubecontainer.NewCache()

	klet.mirrorPodClient = kubepod.NewBasicMirrorClient(klet.kubeClient, string(nodeName), nodeLister)
	klet.podManager = kubepod.NewBasicPodManager()

	klet.statusManager = status.NewManager(klet.kubeClient, klet.podManager, klet, kubeDeps.PodStartupLatencyTracker, klet.getRootDir())

	klet.resourceAnalyzer = serverstats.NewResourceAnalyzer(klet, kubeCfg.VolumeStatsAggPeriod.Duration, kubeDeps.Recorder)

	klet.runtimeService = kubeDeps.RemoteRuntimeService

	if kubeDeps.KubeClient != nil {
		klet.runtimeClassManager = runtimeclass.NewManager(kubeDeps.KubeClient)
	}

	// setup containerLogManager for CRI container runtime
	containerLogManager, err := logs.NewContainerLogManager(
		klet.runtimeService,
		kubeDeps.OSInterface,
		kubeCfg.ContainerLogMaxSize,
		int(kubeCfg.ContainerLogMaxFiles),
		int(kubeCfg.ContainerLogMaxWorkers),
		kubeCfg.ContainerLogMonitorInterval,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize container log manager: %v", err)
	}
	klet.containerLogManager = containerLogManager

	klet.reasonCache = NewReasonCache()
	klet.workQueue = queue.NewBasicWorkQueue(klet.clock)
	klet.podWorkers = newPodWorkers(
		klet,
		kubeDeps.Recorder,
		klet.workQueue,
		klet.resyncInterval,
		backOffPeriod,
		klet.podCache,
	)

	var singleProcessOOMKill *bool
	if sysruntime.GOOS == "linux" {
		if !util.IsCgroup2UnifiedMode() {
			// This is a default behavior for cgroups v1.
			singleProcessOOMKill = ptr.To(true)
		} else {
			if kubeCfg.SingleProcessOOMKill == nil {
				singleProcessOOMKill = ptr.To(false)
			} else {
				singleProcessOOMKill = kubeCfg.SingleProcessOOMKill
			}
		}
	}

	runtime, err := kuberuntime.NewKubeGenericRuntimeManager(
		kubecontainer.FilterEventRecorder(kubeDeps.Recorder),
		klet.livenessManager,
		klet.readinessManager,
		klet.startupManager,
		rootDirectory,
		podLogsDirectory,
		machineInfo,
		klet.podWorkers,
		kubeDeps.OSInterface,
		klet,
		insecureContainerLifecycleHTTPClient,
		imageBackOff,
		kubeCfg.SerializeImagePulls,
		kubeCfg.MaxParallelImagePulls,
		float32(kubeCfg.RegistryPullQPS),
		int(kubeCfg.RegistryBurst),
		imageCredentialProviderConfigFile,
		imageCredentialProviderBinDir,
		singleProcessOOMKill,
		kubeCfg.CPUCFSQuota,
		kubeCfg.CPUCFSQuotaPeriod,
		kubeDeps.RemoteRuntimeService,
		kubeDeps.RemoteImageService,
		kubeDeps.ContainerManager,
		klet.containerLogManager,
		klet.runtimeClassManager,
		seccompDefault,
		kubeCfg.MemorySwap.SwapBehavior,
		kubeDeps.ContainerManager.GetNodeAllocatableAbsolute,
		*kubeCfg.MemoryThrottlingFactor,
		kubeDeps.PodStartupLatencyTracker,
		kubeDeps.TracerProvider,
	)
	if err != nil {
		return nil, err
	}
	klet.containerRuntime = runtime
	klet.streamingRuntime = runtime
	klet.runner = runtime

	runtimeCache, err := kubecontainer.NewRuntimeCache(klet.containerRuntime, runtimeCacheRefreshPeriod)
	if err != nil {
		return nil, err
	}
	klet.runtimeCache = runtimeCache

	// common provider to get host file system usage associated with a pod managed by kubelet
	hostStatsProvider := stats.NewHostStatsProvider(kubecontainer.RealOS{}, func(podUID types.UID) string {
		return getEtcHostsPath(klet.getPodDir(podUID))
	}, podLogsDirectory)
	if kubeDeps.useLegacyCadvisorStats {
		klet.StatsProvider = stats.NewCadvisorStatsProvider(
			klet.cadvisor,
			klet.resourceAnalyzer,
			klet.podManager,
			klet.runtimeCache,
			klet.containerRuntime,
			klet.statusManager,
			hostStatsProvider)
	} else {
		klet.StatsProvider = stats.NewCRIStatsProvider(
			klet.cadvisor,
			klet.resourceAnalyzer,
			klet.podManager,
			klet.runtimeCache,
			kubeDeps.RemoteRuntimeService,
			kubeDeps.RemoteImageService,
			hostStatsProvider,
			utilfeature.DefaultFeatureGate.Enabled(features.PodAndContainerStatsFromCRI))
	}

	eventChannel := make(chan *pleg.PodLifecycleEvent, plegChannelCapacity)

	if utilfeature.DefaultFeatureGate.Enabled(features.EventedPLEG) {
		// adjust Generic PLEG relisting period and threshold to higher value when Evented PLEG is turned on
		genericRelistDuration := &pleg.RelistDuration{
			RelistPeriod:    eventedPlegRelistPeriod,
			RelistThreshold: eventedPlegRelistThreshold,
		}
		klet.pleg = pleg.NewGenericPLEG(logger, klet.containerRuntime, eventChannel, genericRelistDuration, klet.podCache, clock.RealClock{})
		// In case Evented PLEG has to fall back on Generic PLEG due to an error,
		// Evented PLEG should be able to reset the Generic PLEG relisting duration
		// to the default value.
		eventedRelistDuration := &pleg.RelistDuration{
			RelistPeriod:    genericPlegRelistPeriod,
			RelistThreshold: genericPlegRelistThreshold,
		}
		klet.eventedPleg, err = pleg.NewEventedPLEG(logger, klet.containerRuntime, klet.runtimeService, eventChannel,
			klet.podCache, klet.pleg, eventedPlegMaxStreamRetries, eventedRelistDuration, clock.RealClock{})
		if err != nil {
			return nil, err
		}
	} else {
		genericRelistDuration := &pleg.RelistDuration{
			RelistPeriod:    genericPlegRelistPeriod,
			RelistThreshold: genericPlegRelistThreshold,
		}
		klet.pleg = pleg.NewGenericPLEG(logger, klet.containerRuntime, eventChannel, genericRelistDuration, klet.podCache, clock.RealClock{})
	}

	klet.runtimeState = newRuntimeState(maxWaitForContainerRuntime)
	klet.runtimeState.addHealthCheck("PLEG", klet.pleg.Healthy)
	if utilfeature.DefaultFeatureGate.Enabled(features.EventedPLEG) {
		klet.runtimeState.addHealthCheck("EventedPLEG", klet.eventedPleg.Healthy)
	}
	if _, err := klet.updatePodCIDR(ctx, kubeCfg.PodCIDR); err != nil {
		klog.ErrorS(err, "Pod CIDR update failed")
	}

	// setup containerGC
	containerGC, err := kubecontainer.NewContainerGC(klet.containerRuntime, containerGCPolicy, klet.sourcesReady)
	if err != nil {
		return nil, err
	}
	klet.containerGC = containerGC
	klet.containerDeletor = newPodContainerDeletor(klet.containerRuntime, max(containerGCPolicy.MaxPerPodContainer, minDeadContainerInPod))

	// setup imageManager
	imageManager, err := images.NewImageGCManager(klet.containerRuntime, klet.StatsProvider, kubeDeps.Recorder, nodeRef, imageGCPolicy, kubeDeps.TracerProvider)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize image manager: %v", err)
	}
	klet.imageManager = imageManager

	if kubeDeps.TLSOptions != nil {
		if kubeCfg.ServerTLSBootstrap && utilfeature.DefaultFeatureGate.Enabled(features.RotateKubeletServerCertificate) {
			klet.serverCertificateManager, err = kubeletcertificate.NewKubeletServerCertificateManager(klet.kubeClient, kubeCfg, klet.nodeName, klet.getLastObservedNodeAddresses, certDirectory)
			if err != nil {
				return nil, fmt.Errorf("failed to initialize certificate manager: %w", err)
			}

		} else if kubeDeps.TLSOptions.CertFile != "" && kubeDeps.TLSOptions.KeyFile != "" && utilfeature.DefaultFeatureGate.Enabled(features.ReloadKubeletServerCertificateFile) {
			klet.serverCertificateManager, err = kubeletcertificate.NewKubeletServerCertificateDynamicFileManager(kubeDeps.TLSOptions.CertFile, kubeDeps.TLSOptions.KeyFile)
			if err != nil {
				return nil, fmt.Errorf("failed to initialize file based certificate manager: %w", err)
			}
		}

		if klet.serverCertificateManager != nil {
			kubeDeps.TLSOptions.Config.GetCertificate = func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
				cert := klet.serverCertificateManager.Current()
				if cert == nil {
					return nil, fmt.Errorf("no serving certificate available for the kubelet")
				}
				return cert, nil
			}
		}
	}

	if kubeDeps.ProbeManager != nil {
		klet.probeManager = kubeDeps.ProbeManager
	} else {
		klet.probeManager = prober.NewManager(
			klet.statusManager,
			klet.livenessManager,
			klet.readinessManager,
			klet.startupManager,
			klet.runner,
			kubeDeps.Recorder)
	}

	tokenManager := token.NewManager(kubeDeps.KubeClient)

	var clusterTrustBundleManager clustertrustbundle.Manager
	if kubeDeps.KubeClient != nil && utilfeature.DefaultFeatureGate.Enabled(features.ClusterTrustBundleProjection) {
		kubeInformers := informers.NewSharedInformerFactoryWithOptions(kubeDeps.KubeClient, 0)
		clusterTrustBundleManager, err = clustertrustbundle.NewInformerManager(ctx, kubeInformers.Certificates().V1alpha1().ClusterTrustBundles(), 2*int(kubeCfg.MaxPods), 5*time.Minute)
		if err != nil {
			return nil, fmt.Errorf("while starting informer-based ClusterTrustBundle manager: %w", err)
		}
		kubeInformers.Start(wait.NeverStop)
		klog.InfoS("Started ClusterTrustBundle informer")
	} else {
		// In static kubelet mode, use a no-op manager.
		clusterTrustBundleManager = &clustertrustbundle.NoopManager{}
		klog.InfoS("Not starting ClusterTrustBundle informer because we are in static kubelet mode")
	}

	// NewInitializedVolumePluginMgr initializes some storageErrors on the Kubelet runtimeState (in csi_plugin.go init)
	// which affects node ready status. This function must be called before Kubelet is initialized so that the Node
	// ReadyState is accurate with the storage state.
	klet.volumePluginMgr, err =
		NewInitializedVolumePluginMgr(klet, secretManager, configMapManager, tokenManager, clusterTrustBundleManager, kubeDeps.VolumePlugins, kubeDeps.DynamicPluginProber)
	if err != nil {
		return nil, err
	}
	klet.pluginManager = pluginmanager.NewPluginManager(
		klet.getPluginsRegistrationDir(), /* sockDir */
		kubeDeps.Recorder,
	)

	// If the experimentalMounterPathFlag is set, we do not want to
	// check node capabilities since the mount path is not the default
	if len(experimentalMounterPath) != 0 {
		// Replace the nameserver in containerized-mounter's rootfs/etc/resolv.conf with kubelet.ClusterDNS
		// so that service name could be resolved
		klet.dnsConfigurer.SetupDNSinContainerizedMounter(experimentalMounterPath)
	}

	// setup volumeManager
	klet.volumeManager = volumemanager.NewVolumeManager(
		kubeCfg.EnableControllerAttachDetach,
		nodeName,
		klet.podManager,
		klet.podWorkers,
		klet.kubeClient,
		klet.volumePluginMgr,
		klet.containerRuntime,
		kubeDeps.Mounter,
		kubeDeps.HostUtil,
		klet.getPodsDir(),
		kubeDeps.Recorder,
		volumepathhandler.NewBlockVolumePathHandler())

	boMax := MaxContainerBackOff
	base := containerBackOffPeriod
	if utilfeature.DefaultFeatureGate.Enabled(features.KubeletCrashLoopBackOffMax) {
		boMax = kubeCfg.CrashLoopBackOff.MaxContainerRestartPeriod.Duration
		if boMax < containerBackOffPeriod {
			base = boMax
		}
	}
	klet.backOff = flowcontrol.NewBackOff(base, boMax)
	klet.backOff.HasExpiredFunc = func(eventTime time.Time, lastUpdate time.Time, maxDuration time.Duration) bool {
		return eventTime.Sub(lastUpdate) > 600*time.Second
	}

	// setup eviction manager
	evictionManager, evictionAdmitHandler := eviction.NewManager(klet.resourceAnalyzer, evictionConfig,
		killPodNow(klet.podWorkers, kubeDeps.Recorder), klet.imageManager, klet.containerGC, kubeDeps.Recorder, nodeRef, klet.clock, kubeCfg.LocalStorageCapacityIsolation)

	klet.evictionManager = evictionManager
	klet.admitHandlers.AddPodAdmitHandler(evictionAdmitHandler)

	// Safe, allowed sysctls can always be used as unsafe sysctls in the spec.
	// Hence, we concatenate those two lists.
	safeAndUnsafeSysctls := append(sysctl.SafeSysctlAllowlist(ctx), allowedUnsafeSysctls...)
	sysctlsAllowlist, err := sysctl.NewAllowlist(safeAndUnsafeSysctls)
	if err != nil {
		return nil, err
	}
	klet.admitHandlers.AddPodAdmitHandler(sysctlsAllowlist)

	// enable active deadline handler
	activeDeadlineHandler, err := newActiveDeadlineHandler(klet.statusManager, kubeDeps.Recorder, klet.clock)
	if err != nil {
		return nil, err
	}
	klet.AddPodSyncLoopHandler(activeDeadlineHandler)
	klet.AddPodSyncHandler(activeDeadlineHandler)

	klet.admitHandlers.AddPodAdmitHandler(klet.containerManager.GetAllocateResourcesPodAdmitHandler())

	criticalPodAdmissionHandler := preemption.NewCriticalPodAdmissionHandler(klet.GetActivePods, killPodNow(klet.podWorkers, kubeDeps.Recorder), kubeDeps.Recorder)
	klet.admitHandlers.AddPodAdmitHandler(lifecycle.NewPredicateAdmitHandler(klet.getNodeAnyWay, criticalPodAdmissionHandler, klet.containerManager.UpdatePluginResources))
	// apply functional Option's
	for _, opt := range kubeDeps.Options {
		opt(klet)
	}

	if goos == "linux" {
		// AppArmor is a Linux kernel security module and it does not support other operating systems.
		klet.appArmorValidator = apparmor.NewValidator()
		klet.admitHandlers.AddPodAdmitHandler(lifecycle.NewAppArmorAdmitHandler(klet.appArmorValidator))
	}

	leaseDuration := time.Duration(kubeCfg.NodeLeaseDurationSeconds) * time.Second
	renewInterval := time.Duration(float64(leaseDuration) * nodeLeaseRenewIntervalFraction)
	klet.nodeLeaseController = lease.NewController(
		klet.clock,
		klet.heartbeatClient,
		string(klet.nodeName),
		kubeCfg.NodeLeaseDurationSeconds,
		klet.onRepeatedHeartbeatFailure,
		renewInterval,
		string(klet.nodeName),
		v1.NamespaceNodeLease,
		util.SetNodeOwnerFunc(klet.heartbeatClient, string(klet.nodeName)))

	// setup node shutdown manager
	shutdownManager := nodeshutdown.NewManager(&nodeshutdown.Config{
		Logger:                           logger,
		ProbeManager:                     klet.probeManager,
		VolumeManager:                    klet.volumeManager,
		Recorder:                         kubeDeps.Recorder,
		NodeRef:                          nodeRef,
		GetPodsFunc:                      klet.GetActivePods,
		KillPodFunc:                      killPodNow(klet.podWorkers, kubeDeps.Recorder),
		SyncNodeStatusFunc:               klet.syncNodeStatus,
		ShutdownGracePeriodRequested:     kubeCfg.ShutdownGracePeriod.Duration,
		ShutdownGracePeriodCriticalPods:  kubeCfg.ShutdownGracePeriodCriticalPods.Duration,
		ShutdownGracePeriodByPodPriority: kubeCfg.ShutdownGracePeriodByPodPriority,
		StateDirectory:                   rootDirectory,
	})
	klet.shutdownManager = shutdownManager
	klet.usernsManager, err = userns.MakeUserNsManager(klet)
	if err != nil {
		return nil, fmt.Errorf("create user namespace manager: %w", err)
	}
	klet.admitHandlers.AddPodAdmitHandler(shutdownManager)

	// Finally, put the most recent version of the config on the Kubelet, so
	// people can see how it was configured.
	klet.kubeletConfiguration = *kubeCfg

	// Generating the status funcs should be the last thing we do,
	// since this relies on the rest of the Kubelet having been constructed.
	klet.setNodeStatusFuncs = klet.defaultNodeStatusFuncs()

	if utilfeature.DefaultFeatureGate.Enabled(features.SystemdWatchdog) {
		// NewHealthChecker returns an error indicating that the watchdog is configured but the configuration is incorrect,
		// the kubelet will not be started.
		checkers := klet.containerManager.GetHealthCheckers()
		klet.healthChecker, err = watchdog.NewHealthChecker(klet, watchdog.WithExtendedCheckers(checkers))
		if err != nil {
			return nil, fmt.Errorf("create health checker: %w", err)
		}
	}
	return klet, nil
}

type serviceLister interface {
	List(labels.Selector) ([]*v1.Service, error)
}

// Kubelet is the main kubelet implementation.
type Kubelet struct {
	kubeletConfiguration kubeletconfiginternal.KubeletConfiguration

	// hostname is the hostname the kubelet detected or was given via flag/config
	hostname string
	// hostnameOverridden indicates the hostname was overridden via flag/config
	hostnameOverridden bool

	nodeName        types.NodeName
	runtimeCache    kubecontainer.RuntimeCache
	kubeClient      clientset.Interface
	heartbeatClient clientset.Interface
	// mirrorPodClient is used to create and delete mirror pods in the API for static
	// pods.
	mirrorPodClient kubepod.MirrorClient

	rootDirectory    string
	podLogsDirectory string

	lastObservedNodeAddressesMux sync.RWMutex
	lastObservedNodeAddresses    []v1.NodeAddress

	// onRepeatedHeartbeatFailure is called when a heartbeat operation fails more than once. optional.
	onRepeatedHeartbeatFailure func()

	// podManager stores the desired set of admitted pods and mirror pods that the kubelet should be
	// running. The actual set of running pods is stored on the podWorkers. The manager is populated
	// by the kubelet config loops which abstracts receiving configuration from many different sources
	// (api for regular pods, local filesystem or http for static pods). The manager may be consulted
	// by other components that need to see the set of desired pods. Note that not all desired pods are
	// running, and not all running pods are in the podManager - for instance, force deleting a pod
	// from the apiserver will remove it from the podManager, but the pod may still be terminating and
	// tracked by the podWorkers. Components that need to know the actual consumed resources of the
	// node or are driven by podWorkers and the sync*Pod methods (status, volume, stats) should also
	// consult the podWorkers when reconciling.
	//
	// TODO: review all kubelet components that need the actual set of pods (vs the desired set)
	// and update them to use podWorkers instead of podManager. This may introduce latency in some
	// methods, but avoids race conditions and correctly accounts for terminating pods that have
	// been force deleted or static pods that have been updated.
	// https://github.com/kubernetes/kubernetes/issues/116970
	podManager kubepod.Manager

	// podWorkers is responsible for driving the lifecycle state machine of each pod. The worker is
	// notified of config changes, updates, periodic reconciliation, container runtime updates, and
	// evictions of all desired pods and will invoke reconciliation methods per pod in separate
	// goroutines. The podWorkers are authoritative in the kubelet for what pods are actually being
	// run and their current state:
	//
	// * syncing: pod should be running (syncPod)
	// * terminating: pod should be stopped (syncTerminatingPod)
	// * terminated: pod should have all resources cleaned up (syncTerminatedPod)
	//
	// and invoke the handler methods that correspond to each state. Components within the
	// kubelet that need to know the phase of the pod in order to correctly set up or tear down
	// resources must consult the podWorkers.
	//
	// Once a pod has been accepted by the pod workers, no other pod with that same UID (and
	// name+namespace, for static pods) will be started until the first pod has fully terminated
	// and been cleaned up by SyncKnownPods. This means a pod may be desired (in API), admitted
	// (in pod manager), and requested (by invoking UpdatePod) but not start for an arbitrarily
	// long interval because a prior pod is still terminating.
	//
	// As an event-driven (by UpdatePod) controller, the podWorkers must periodically be resynced
	// by the kubelet invoking SyncKnownPods with the desired state (admitted pods in podManager).
	// Since the podManager may be unaware of some running pods due to force deletion, the
	// podWorkers are responsible for triggering a sync of pods that are no longer desired but
	// must still run to completion.
	podWorkers PodWorkers

	// evictionManager observes the state of the node for situations that could impact node stability
	// and evicts pods (sets to phase Failed with reason Evicted) to reduce resource pressure. The
	// eviction manager acts on the actual state of the node and considers the podWorker to be
	// authoritative.
	evictionManager eviction.Manager

	// probeManager tracks the set of running pods and ensures any user-defined periodic checks are
	// run to introspect the state of each pod.  The probe manager acts on the actual state of the node
	// and is notified of pods by the podWorker. The probe manager is the authoritative source of the
	// most recent probe status and is responsible for notifying the status manager, which
	// synthesizes them into the overall pod status.
	probeManager prober.Manager

	// secretManager caches the set of secrets used by running pods on this node. The podWorkers
	// notify the secretManager when pods are started and terminated, and the secretManager must
	// then keep the needed secrets up-to-date as they change.
	secretManager secret.Manager

	// configMapManager caches the set of config maps used by running pods on this node. The
	// podWorkers notify the configMapManager when pods are started and terminated, and the
	// configMapManager must then keep the needed config maps up-to-date as they change.
	configMapManager configmap.Manager

	// volumeManager observes the set of running pods and is responsible for attaching, mounting,
	// unmounting, and detaching as those pods move through their lifecycle. It periodically
	// synchronizes the set of known volumes to the set of actually desired volumes and cleans up
	// any orphaned volumes. The volume manager considers the podWorker to be authoritative for
	// which pods are running.
	volumeManager volumemanager.VolumeManager

	// statusManager receives updated pod status updates from the podWorker and updates the API
	// status of those pods to match. The statusManager is authoritative for the synthesized
	// status of the pod from the kubelet's perspective (other components own the individual
	// elements of status) and should be consulted by components in preference to assembling
	// that status themselves. Note that the status manager is downstream of the pod worker
	// and components that need to check whether a pod is still running should instead directly
	// consult the pod worker.
	statusManager status.Manager

	// resyncInterval is the interval between periodic full reconciliations of
	// pods on this node.
	resyncInterval time.Duration

	// sourcesReady records the sources seen by the kubelet, it is thread-safe.
	sourcesReady config.SourcesReady

	// Optional, defaults to /logs/ from /var/log
	logServer http.Handler
	// Optional, defaults to simple Docker implementation
	runner kubecontainer.CommandRunner

	// cAdvisor used for container information.
	cadvisor cadvisor.Interface

	// Set to true to have the node register itself with the apiserver.
	registerNode bool
	// List of taints to add to a node object when the kubelet registers itself.
	registerWithTaints []v1.Taint
	// Set to true to have the node register itself as schedulable.
	registerSchedulable bool
	// for internal book keeping; access only from within registerWithApiserver
	registrationCompleted bool

	// dnsConfigurer is used for setting up DNS resolver configuration when launching pods.
	dnsConfigurer *dns.Configurer

	// serviceLister knows how to list services
	serviceLister serviceLister
	// serviceHasSynced indicates whether services have been sync'd at least once.
	// Check this before trusting a response from the lister.
	serviceHasSynced cache.InformerSynced
	// nodeLister knows how to list nodes
	nodeLister corelisters.NodeLister
	// nodeHasSynced indicates whether nodes have been sync'd at least once.
	// Check this before trusting a response from the node lister.
	nodeHasSynced cache.InformerSynced
	// a list of node labels to register
	nodeLabels map[string]string

	// Last timestamp when runtime responded on ping.
	// Mutex is used to protect this value.
	runtimeState *runtimeState

	// Volume plugins.
	volumePluginMgr *volume.VolumePluginMgr

	// Manages container health check results.
	livenessManager  proberesults.Manager
	readinessManager proberesults.Manager
	startupManager   proberesults.Manager

	// How long to keep idle streaming command execution/port forwarding
	// connections open before terminating them
	streamingConnectionIdleTimeout time.Duration

	// The EventRecorder to use
	recorder record.EventRecorder

	// Policy for handling garbage collection of dead containers.
	containerGC kubecontainer.GC

	// Manager for image garbage collection.
	imageManager images.ImageGCManager

	// Manager for container logs.
	containerLogManager logs.ContainerLogManager

	// Cached MachineInfo returned by cadvisor.
	machineInfoLock sync.RWMutex
	machineInfo     *cadvisorapi.MachineInfo

	// Handles certificate rotations.
	serverCertificateManager certificate.Manager

	// Cloud provider interface.
	cloud cloudprovider.Interface
	// Handles requests to cloud provider with timeout
	cloudResourceSyncManager cloudresource.SyncManager

	// Indicates that the node initialization happens in an external cloud controller
	externalCloudProvider bool
	// Reference to this node.
	nodeRef *v1.ObjectReference

	// Container runtime.
	containerRuntime kubecontainer.Runtime

	// Streaming runtime handles container streaming.
	streamingRuntime kubecontainer.StreamingRuntime

	// Container runtime service (needed by container runtime Start()).
	runtimeService internalapi.RuntimeService

	// reasonCache caches the failure reason of the last creation of all containers, which is
	// used for generating ContainerStatus.
	reasonCache *ReasonCache

	// containerRuntimeReadyExpected indicates whether container runtime being ready is expected
	// so errors are logged without verbosity guard, to avoid excessive error logs at node startup.
	// It's false during the node initialization period of nodeReadyGracePeriod, and after that
	// it's set to true by fastStatusUpdateOnce when it exits.
	containerRuntimeReadyExpected bool

	// nodeStatusUpdateFrequency specifies how often kubelet computes node status. If node lease
	// feature is not enabled, it is also the frequency that kubelet posts node status to master.
	// In that case, be cautious when changing the constant, it must work with nodeMonitorGracePeriod
	// in nodecontroller. There are several constraints:
	// 1. nodeMonitorGracePeriod must be N times more than nodeStatusUpdateFrequency, where
	//    N means number of retries allowed for kubelet to post node status. It is pointless
	//    to make nodeMonitorGracePeriod be less than nodeStatusUpdateFrequency, since there
	//    will only be fresh values from Kubelet at an interval of nodeStatusUpdateFrequency.
	//    The constant must be less than podEvictionTimeout.
	// 2. nodeStatusUpdateFrequency needs to be large enough for kubelet to generate node
	//    status. Kubelet may fail to update node status reliably if the value is too small,
	//    as it takes time to gather all necessary node information.
	nodeStatusUpdateFrequency time.Duration

	// nodeStatusReportFrequency is the frequency that kubelet posts node
	// status to master. It is only used when node lease feature is enabled.
	nodeStatusReportFrequency time.Duration

	// delayAfterNodeStatusChange is the one-time random duration that we add to the next node status report interval
	// every time when there's an actual node status change. But all future node status update that is not caused by
	// real status change will stick with nodeStatusReportFrequency. The random duration is a uniform distribution over
	// [-0.5*nodeStatusReportFrequency, 0.5*nodeStatusReportFrequency]
	delayAfterNodeStatusChange time.Duration

	// lastStatusReportTime is the time when node status was last reported.
	lastStatusReportTime time.Time

	// syncNodeStatusMux is a lock on updating the node status, because this path is not thread-safe.
	// This lock is used by Kubelet.syncNodeStatus and Kubelet.fastNodeStatusUpdate functions and shouldn't be used anywhere else.
	syncNodeStatusMux sync.Mutex

	// updatePodCIDRMux is a lock on updating pod CIDR, because this path is not thread-safe.
	// This lock is used by Kubelet.updatePodCIDR function and shouldn't be used anywhere else.
	updatePodCIDRMux sync.Mutex

	// updateRuntimeMux is a lock on updating runtime, because this path is not thread-safe.
	// This lock is used by Kubelet.updateRuntimeUp, Kubelet.fastNodeStatusUpdate and
	// Kubelet.HandlerSupportsUserNamespaces functions and shouldn't be used anywhere else.
	updateRuntimeMux sync.Mutex

	// nodeLeaseController claims and renews the node lease for this Kubelet
	nodeLeaseController lease.Controller

	// pleg observes the state of the container runtime and notifies the kubelet of changes to containers, which
	// notifies the podWorkers to reconcile the state of the pod (for instance, if a container dies and needs to
	// be restarted).
	pleg pleg.PodLifecycleEventGenerator

	// eventedPleg supplements the pleg to deliver edge-driven container changes with low-latency.
	eventedPleg pleg.PodLifecycleEventGenerator

	// Store kubecontainer.PodStatus for all pods.
	podCache kubecontainer.Cache

	// os is a facade for various syscalls that need to be mocked during testing.
	os kubecontainer.OSInterface

	// Watcher of out of memory events.
	oomWatcher oomwatcher.Watcher

	// Monitor resource usage
	resourceAnalyzer serverstats.ResourceAnalyzer

	// Whether or not we should have the QOS cgroup hierarchy for resource management
	cgroupsPerQOS bool

	// If non-empty, pass this to the container runtime as the root cgroup.
	cgroupRoot string

	// Mounter to use for volumes.
	mounter mount.Interface

	// hostutil to interact with filesystems
	hostutil hostutil.HostUtils

	// subpather to execute subpath actions
	subpather subpath.Interface

	// Manager of non-Runtime containers.
	containerManager cm.ContainerManager

	// Maximum Number of Pods which can be run by this Kubelet
	maxPods int

	// Monitor Kubelet's sync loop
	syncLoopMonitor atomic.Value

	// Container restart Backoff
	backOff *flowcontrol.Backoff

	// Information about the ports which are opened by daemons on Node running this Kubelet server.
	daemonEndpoints *v1.NodeDaemonEndpoints

	// A queue used to trigger pod workers.
	workQueue queue.WorkQueue

	// oneTimeInitializer is used to initialize modules that are dependent on the runtime to be up.
	oneTimeInitializer sync.Once

	// If set, use this IP address or addresses for the node
	nodeIPs []net.IP

	// use this function to validate the kubelet nodeIP
	nodeIPValidator func(net.IP) error

	// If non-nil, this is a unique identifier for the node in an external database, eg. cloudprovider
	providerID string

	// clock is an interface that provides time related functionality in a way that makes it
	// easy to test the code.
	clock clock.WithTicker

	// handlers called during the tryUpdateNodeStatus cycle
	setNodeStatusFuncs []func(context.Context, *v1.Node) error

	lastNodeUnschedulableLock sync.Mutex
	// maintains Node.Spec.Unschedulable value from previous run of tryUpdateNodeStatus()
	lastNodeUnschedulable bool

	// the list of handlers to call during pod admission.
	admitHandlers lifecycle.PodAdmitHandlers

	// the list of handlers to call during pod sync loop.
	lifecycle.PodSyncLoopHandlers

	// the list of handlers to call during pod sync.
	lifecycle.PodSyncHandlers

	// the number of allowed pods per core
	podsPerCore int

	// enableControllerAttachDetach indicates the Attach/Detach controller
	// should manage attachment/detachment of volumes scheduled to this node,
	// and disable kubelet from executing any attach/detach operations
	enableControllerAttachDetach bool

	// trigger deleting containers in a pod
	containerDeletor *podContainerDeletor

	// config iptables util rules
	makeIPTablesUtilChains bool

	// The AppArmor validator for checking whether AppArmor is supported.
	appArmorValidator apparmor.Validator

	// StatsProvider provides the node and the container stats.
	StatsProvider *stats.Provider

	// pluginmanager runs a set of asynchronous loops that figure out which
	// plugins need to be registered/unregistered based on this node and makes it so.
	pluginManager pluginmanager.PluginManager

	// This flag sets a maximum number of images to report in the node status.
	nodeStatusMaxImages int32

	// Handles RuntimeClass objects for the Kubelet.
	runtimeClassManager *runtimeclass.Manager

	// Handles node shutdown events for the Node.
	shutdownManager nodeshutdown.Manager

	// Manage user namespaces
	usernsManager *userns.UsernsManager

	// Mutex to serialize new pod admission and existing pod resizing
	podResizeMutex sync.Mutex

	// OpenTelemetry Tracer
	tracer trace.Tracer

	// Track node startup latencies
	nodeStartupLatencyTracker util.NodeStartupLatencyTracker

	// Health check kubelet
	healthChecker watchdog.HealthChecker
}

// ListPodStats is delegated to StatsProvider, which implements stats.Provider interface
func (kl *Kubelet) ListPodStats(ctx context.Context) ([]statsapi.PodStats, error) {
	return kl.StatsProvider.ListPodStats(ctx)
}

// ListPodCPUAndMemoryStats is delegated to StatsProvider, which implements stats.Provider interface
func (kl *Kubelet) ListPodCPUAndMemoryStats(ctx context.Context) ([]statsapi.PodStats, error) {
	return kl.StatsProvider.ListPodCPUAndMemoryStats(ctx)
}

// ListPodStatsAndUpdateCPUNanoCoreUsage is delegated to StatsProvider, which implements stats.Provider interface
func (kl *Kubelet) ListPodStatsAndUpdateCPUNanoCoreUsage(ctx context.Context) ([]statsapi.PodStats, error) {
	return kl.StatsProvider.ListPodStatsAndUpdateCPUNanoCoreUsage(ctx)
}

// ImageFsStats is delegated to StatsProvider, which implements stats.Provider interface
func (kl *Kubelet) ImageFsStats(ctx context.Context) (*statsapi.FsStats, *statsapi.FsStats, error) {
	return kl.StatsProvider.ImageFsStats(ctx)
}

// GetCgroupStats is delegated to StatsProvider, which implements stats.Provider interface
func (kl *Kubelet) GetCgroupStats(cgroupName string, updateStats bool) (*statsapi.ContainerStats, *statsapi.NetworkStats, error) {
	return kl.StatsProvider.GetCgroupStats(cgroupName, updateStats)
}

// GetCgroupCPUAndMemoryStats is delegated to StatsProvider, which implements stats.Provider interface
func (kl *Kubelet) GetCgroupCPUAndMemoryStats(cgroupName string, updateStats bool) (*statsapi.ContainerStats, error) {
	return kl.StatsProvider.GetCgroupCPUAndMemoryStats(cgroupName, updateStats)
}

// RootFsStats is delegated to StatsProvider, which implements stats.Provider interface
func (kl *Kubelet) RootFsStats() (*statsapi.FsStats, error) {
	return kl.StatsProvider.RootFsStats()
}

// RlimitStats is delegated to StatsProvider, which implements stats.Provider interface
func (kl *Kubelet) RlimitStats() (*statsapi.RlimitStats, error) {
	return kl.StatsProvider.RlimitStats()
}

// setupDataDirs creates:
// 1.  the root directory
// 2.  the pods directory
// 3.  the plugins directory
// 4.  the pod-resources directory
// 5.  the checkpoint directory
// 6.  the pod logs root directory
func (kl *Kubelet) setupDataDirs() error {
	if cleanedRoot := filepath.Clean(kl.rootDirectory); cleanedRoot != kl.rootDirectory {
		return fmt.Errorf("rootDirectory not in canonical form: expected %s, was %s", cleanedRoot, kl.rootDirectory)
	}
	pluginRegistrationDir := kl.getPluginsRegistrationDir()
	pluginsDir := kl.getPluginsDir()
	if err := os.MkdirAll(kl.getRootDir(), 0750); err != nil {
		return fmt.Errorf("error creating root directory: %v", err)
	}
	if err := utilfs.MkdirAll(kl.getPodLogsDir(), 0750); err != nil {
		return fmt.Errorf("error creating pod logs root directory %q: %w", kl.getPodLogsDir(), err)
	}
	if err := kl.hostutil.MakeRShared(kl.getRootDir()); err != nil {
		return fmt.Errorf("error configuring root directory: %v", err)
	}
	if err := os.MkdirAll(kl.getPodsDir(), 0750); err != nil {
		return fmt.Errorf("error creating pods directory: %v", err)
	}
	if err := utilfs.MkdirAll(kl.getPluginsDir(), 0750); err != nil {
		return fmt.Errorf("error creating plugins directory: %v", err)
	}
	if err := utilfs.MkdirAll(kl.getPluginsRegistrationDir(), 0750); err != nil {
		return fmt.Errorf("error creating plugins registry directory: %v", err)
	}
	if err := os.MkdirAll(kl.getPodResourcesDir(), 0750); err != nil {
		return fmt.Errorf("error creating podresources directory: %v", err)
	}
	if utilfeature.DefaultFeatureGate.Enabled(features.ContainerCheckpoint) {
		if err := utilfs.MkdirAll(kl.getCheckpointsDir(), 0700); err != nil {
			return fmt.Errorf("error creating checkpoint directory: %v", err)
		}
	}
	if selinux.GetEnabled() {
		err := selinux.SetFileLabel(pluginRegistrationDir, config.KubeletPluginsDirSELinuxLabel)
		if err != nil {
			klog.InfoS("Unprivileged containerized plugins might not work, could not set selinux context on plugin registration dir", "path", pluginRegistrationDir, "err", err)
		}
		err = selinux.SetFileLabel(pluginsDir, config.KubeletPluginsDirSELinuxLabel)
		if err != nil {
			klog.InfoS("Unprivileged containerized plugins might not work, could not set selinux context on plugins dir", "path", pluginsDir, "err", err)
		}
	}
	return nil
}

// StartGarbageCollection starts garbage collection threads.
func (kl *Kubelet) StartGarbageCollection() {
	loggedContainerGCFailure := false
	go wait.Until(func() {
		ctx := context.Background()
		if err := kl.containerGC.GarbageCollect(ctx); err != nil {
			klog.ErrorS(err, "Container garbage collection failed")
			kl.recorder.Eventf(kl.nodeRef, v1.EventTypeWarning, events.ContainerGCFailed, err.Error())
			loggedContainerGCFailure = true
		} else {
			var vLevel klog.Level = 4
			if loggedContainerGCFailure {
				vLevel = 1
				loggedContainerGCFailure = false
			}

			klog.V(vLevel).InfoS("Container garbage collection succeeded")
		}
	}, ContainerGCPeriod, wait.NeverStop)

	// when the high threshold is set to 100, and the max age is 0 (or the max age feature is disabled)
	// stub the image GC manager
	if kl.kubeletConfiguration.ImageGCHighThresholdPercent == 100 &&
		(!utilfeature.DefaultFeatureGate.Enabled(features.ImageMaximumGCAge) || kl.kubeletConfiguration.ImageMaximumGCAge.Duration == 0) {
		klog.V(2).InfoS("ImageGCHighThresholdPercent is set 100 and ImageMaximumGCAge is 0, Disable image GC")
		return
	}

	prevImageGCFailed := false
	beganGC := time.Now()
	go wait.Until(func() {
		ctx := context.Background()
		if err := kl.imageManager.GarbageCollect(ctx, beganGC); err != nil {
			if prevImageGCFailed {
				klog.ErrorS(err, "Image garbage collection failed multiple times in a row")
				// Only create an event for repeated failures
				kl.recorder.Eventf(kl.nodeRef, v1.EventTypeWarning, events.ImageGCFailed, err.Error())
			} else {
				klog.ErrorS(err, "Image garbage collection failed once. Stats initialization may not have completed yet")
			}
			prevImageGCFailed = true
		} else {
			var vLevel klog.Level = 4
			if prevImageGCFailed {
				vLevel = 1
				prevImageGCFailed = false
			}

			klog.V(vLevel).InfoS("Image garbage collection succeeded")
		}
	}, ImageGCPeriod, wait.NeverStop)
}

// initializeModules will initialize internal modules that do not require the container runtime to be up.
// Note that the modules here must not depend on modules that are not initialized here.
func (kl *Kubelet) initializeModules(ctx context.Context) error {
	// Prometheus metrics.
	metrics.Register(
		collectors.NewVolumeStatsCollector(kl),
		collectors.NewLogMetricsCollector(kl.StatsProvider.ListPodStats),
	)
	metrics.SetNodeName(kl.nodeName)
	servermetrics.Register()

	// Setup filesystem directories.
	if err := kl.setupDataDirs(); err != nil {
		return err
	}

	// If the container logs directory does not exist, create it.
	if _, err := os.Stat(ContainerLogsDir); err != nil {
		if err := kl.os.MkdirAll(ContainerLogsDir, 0755); err != nil {
			return fmt.Errorf("failed to create directory %q: %v", ContainerLogsDir, err)
		}
	}

	if goos == "windows" {
		// On Windows we should not allow other users to read the logs directory
		// to avoid allowing non-root containers from reading the logs of other containers.
		if err := utilfs.Chmod(ContainerLogsDir, 0750); err != nil {
			return fmt.Errorf("failed to set permissions on directory %q: %w", ContainerLogsDir, err)
		}
	}

	// Start the image manager.
	kl.imageManager.Start()

	// Start the certificate manager if it was enabled.
	if kl.serverCertificateManager != nil {
		kl.serverCertificateManager.Start()
	}

	// Start out of memory watcher.
	if kl.oomWatcher != nil {
		if err := kl.oomWatcher.Start(ctx, kl.nodeRef); err != nil {
			return fmt.Errorf("failed to start OOM watcher: %w", err)
		}
	}

	// Start resource analyzer
	kl.resourceAnalyzer.Start()

	return nil
}

// initializeRuntimeDependentModules will initialize internal modules that require the container runtime to be up.
func (kl *Kubelet) initializeRuntimeDependentModules() {
	if err := kl.cadvisor.Start(); err != nil {
		// Fail kubelet and rely on the babysitter to retry starting kubelet.
		klog.ErrorS(err, "Failed to start cAdvisor")
		os.Exit(1)
	}

	// trigger on-demand stats collection once so that we have capacity information for ephemeral storage.
	// ignore any errors, since if stats collection is not successful, the container manager will fail to start below.
	kl.StatsProvider.GetCgroupStats("/", true)
	// Start container manager.
	node, err := kl.getNodeAnyWay()
	if err != nil {
		// Fail kubelet and rely on the babysitter to retry starting kubelet.
		klog.ErrorS(err, "Kubelet failed to get node info")
		os.Exit(1)
	}
	// containerManager must start after cAdvisor because it needs filesystem capacity information
	if err := kl.containerManager.Start(context.TODO(), node, kl.GetActivePods, kl.getNodeAnyWay, kl.sourcesReady, kl.statusManager, kl.runtimeService, kl.supportLocalStorageCapacityIsolation()); err != nil {
		// Fail kubelet and rely on the babysitter to retry starting kubelet.
		klog.ErrorS(err, "Failed to start ContainerManager")
		os.Exit(1)
	}
	// eviction manager must start after cadvisor because it needs to know if the container runtime has a dedicated imagefs
	// Eviction decisions are based on the allocated (rather than desired) pod resources.
	kl.evictionManager.Start(kl.StatsProvider, kl.getAllocatedPods, kl.PodIsFinished, evictionMonitoringPeriod)

	// container log manager must start after container runtime is up to retrieve information from container runtime
	// and inform container to reopen log file after log rotation.
	kl.containerLogManager.Start()
	// Adding Registration Callback function for CSI Driver
	kl.pluginManager.AddHandler(pluginwatcherapi.CSIPlugin, plugincache.PluginHandler(csi.PluginHandler))
	// Adding Registration Callback function for DRA Plugin and Device Plugin
	for name, handler := range kl.containerManager.GetPluginRegistrationHandlers() {
		kl.pluginManager.AddHandler(name, handler)
	}

	// Start the plugin manager
	klog.V(4).InfoS("Starting plugin manager")
	go kl.pluginManager.Run(kl.sourcesReady, wait.NeverStop)

	err = kl.shutdownManager.Start()
	if err != nil {
		// The shutdown manager is not critical for kubelet, so log failure, but don't block Kubelet startup if there was a failure starting it.
		klog.ErrorS(err, "Failed to start node shutdown manager")
	}
}

// Run starts the kubelet reacting to config updates
func (kl *Kubelet) Run(updates <-chan kubetypes.PodUpdate) {
	ctx := context.Background()
	if kl.logServer == nil {
		file := http.FileServer(http.Dir(nodeLogDir))
		if utilfeature.DefaultFeatureGate.Enabled(features.NodeLogQuery) && kl.kubeletConfiguration.EnableSystemLogQuery {
			kl.logServer = http.StripPrefix("/logs/", http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if nlq, errs := newNodeLogQuery(req.URL.Query()); len(errs) > 0 {
					http.Error(w, errs.ToAggregate().Error(), http.StatusBadRequest)
					return
				} else if nlq != nil {
					if req.URL.Path != "/" && req.URL.Path != "" {
						http.Error(w, "path not allowed in query mode", http.StatusNotAcceptable)
						return
					}
					if errs := nlq.validate(); len(errs) > 0 {
						http.Error(w, errs.ToAggregate().Error(), http.StatusNotAcceptable)
						return
					}
					// Validation ensures that the request does not query services and files at the same time
					if len(nlq.Services) > 0 {
						journal.ServeHTTP(w, req)
						return
					}
					// Validation ensures that the request does not explicitly query multiple files at the same time
					if len(nlq.Files) == 1 {
						// Account for the \ being used on Windows clients
						req.URL.Path = filepath.ToSlash(nlq.Files[0])
					}
				}
				// Fall back in case the caller is directly trying to query a file
				// Example: kubectl get --raw /api/v1/nodes/$name/proxy/logs/foo.log
				file.ServeHTTP(w, req)
			}))
		} else {
			kl.logServer = http.StripPrefix("/logs/", file)
		}
	}
	if kl.kubeClient == nil {
		klog.InfoS("No API server defined - no node status update will be sent")
	}

	// Start the cloud provider sync manager
	if kl.cloudResourceSyncManager != nil {
		go kl.cloudResourceSyncManager.Run(wait.NeverStop)
	}

	if err := kl.initializeModules(ctx); err != nil {
		kl.recorder.Eventf(kl.nodeRef, v1.EventTypeWarning, events.KubeletSetupFailed, err.Error())
		klog.ErrorS(err, "Failed to initialize internal modules")
		os.Exit(1)
	}

	if err := kl.cgroupVersionCheck(); err != nil {
		klog.V(2).InfoS("Warning: cgroup check", "error", err)
	}

	// Start volume manager
	go kl.volumeManager.Run(ctx, kl.sourcesReady)

	if kl.kubeClient != nil {
		// Start two go-routines to update the status.
		//
		// The first will report to the apiserver every nodeStatusUpdateFrequency and is aimed to provide regular status intervals,
		// while the second is used to provide a more timely status update during initialization and runs an one-shot update to the apiserver
		// once the node becomes ready, then exits afterwards.
		//
		// Introduce some small jittering to ensure that over time the requests won't start
		// accumulating at approximately the same time from the set of nodes due to priority and
		// fairness effect.
		go func() {
			// Call updateRuntimeUp once before syncNodeStatus to make sure kubelet had already checked runtime state
			// otherwise when restart kubelet, syncNodeStatus will report node notReady in first report period
			kl.updateRuntimeUp()
			wait.JitterUntil(kl.syncNodeStatus, kl.nodeStatusUpdateFrequency, 0.04, true, wait.NeverStop)
		}()

		go kl.fastStatusUpdateOnce()

		// start syncing lease
		go kl.nodeLeaseController.Run(context.Background())

		// Mirror pods for static pods may not be created immediately during node startup
		// due to node registration or informer sync delays. They will be created eventually
		//  when static pods are resynced (every 1-1.5 minutes).
		// To ensure kube-scheduler is aware of static pod resource usage faster,
		// mirror pods are created as soon as the node registers.
		go kl.fastStaticPodsRegistration(ctx)
	}
	go wait.Until(kl.updateRuntimeUp, 5*time.Second, wait.NeverStop)

	// Set up iptables util rules
	if kl.makeIPTablesUtilChains {
		kl.initNetworkUtil()
	}

	// Start component sync loops.
	kl.statusManager.Start()

	// Start syncing RuntimeClasses if enabled.
	if kl.runtimeClassManager != nil {
		kl.runtimeClassManager.Start(wait.NeverStop)
	}

	// Start the pod lifecycle event generator.
	kl.pleg.Start()

	// Start eventedPLEG only if EventedPLEG feature gate is enabled.
	if utilfeature.DefaultFeatureGate.Enabled(features.EventedPLEG) {
		kl.eventedPleg.Start()
	}

	if utilfeature.DefaultFeatureGate.Enabled(features.SystemdWatchdog) {
		kl.healthChecker.Start()
	}

	kl.syncLoop(ctx, updates, kl)
}

// SyncPod is the transaction script for the sync of a single pod (setting up)
// a pod. This method is reentrant and expected to converge a pod towards the
// desired state of the spec. The reverse (teardown) is handled in
// SyncTerminatingPod and SyncTerminatedPod. If SyncPod exits without error,
// then the pod runtime state is in sync with the desired configuration state
// (pod is running). If SyncPod exits with a transient error, the next
// invocation of SyncPod is expected to make progress towards reaching the
// desired state. SyncPod exits with isTerminal when the pod was detected to
// have reached a terminal lifecycle phase due to container exits (for
// RestartNever or RestartOnFailure) and the next method invoked will be
// SyncTerminatingPod. If the pod terminates for any other reason, SyncPod
// will receive a context cancellation and should exit as soon as possible.
//
// Arguments:
//
// updateType - whether this is a create (first time) or an update, should
// only be used for metrics since this method must be reentrant
//
// pod - the pod that is being set up
//
// mirrorPod - the mirror pod known to the kubelet for this pod, if any
//
// podStatus - the most recent pod status observed for this pod which can
// be used to determine the set of actions that should be taken during
// this loop of SyncPod
//
// The workflow is:
//   - If the pod is being created, record pod worker start latency
//   - Call generateAPIPodStatus to prepare an v1.PodStatus for the pod
//   - If the pod is being seen as running for the first time, record pod
//     start latency
//   - Update the status of the pod in the status manager
//   - Stop the pod's containers if it should not be running due to soft
//     admission
//   - Ensure any background tracking for a runnable pod is started
//   - Create a mirror pod if the pod is a static pod, and does not
//     already have a mirror pod
//   - Create the data directories for the pod if they do not exist
//   - Wait for volumes to attach/mount
//   - Fetch the pull secrets for the pod
//   - Call the container runtime's SyncPod callback
//   - Update the traffic shaping for the pod's ingress and egress limits
//
// If any step of this workflow errors, the error is returned, and is repeated
// on the next SyncPod call.
//
// This operation writes all events that are dispatched in order to provide
// the most accurate information possible about an error situation to aid debugging.
// Callers should not write an event if this operation returns an error.
func (kl *Kubelet) SyncPod(ctx context.Context, updateType kubetypes.SyncPodType, pod, mirrorPod *v1.Pod, podStatus *kubecontainer.PodStatus) (isTerminal bool, err error) {
	ctx, otelSpan := kl.tracer.Start(ctx, "syncPod", trace.WithAttributes(
		semconv.K8SPodUIDKey.String(string(pod.UID)),
		attribute.String("k8s.pod", klog.KObj(pod).String()),
		semconv.K8SPodNameKey.String(pod.Name),
		attribute.String("k8s.pod.update_type", updateType.String()),
		semconv.K8SNamespaceNameKey.String(pod.Namespace),
	))
	klog.V(4).InfoS("SyncPod enter", "pod", klog.KObj(pod), "podUID", pod.UID)
	defer func() {
		if err != nil {
			otelSpan.RecordError(err)
			otelSpan.SetStatus(codes.Error, err.Error())
		}
		klog.V(4).InfoS("SyncPod exit", "pod", klog.KObj(pod), "podUID", pod.UID, "isTerminal", isTerminal)
		otelSpan.End()
	}()

	// Latency measurements for the main workflow are relative to the
	// first time the pod was seen by kubelet.
	var firstSeenTime time.Time
	if firstSeenTimeStr, ok := pod.Annotations[kubetypes.ConfigFirstSeenAnnotationKey]; ok {
		firstSeenTime = kubetypes.ConvertToTimestamp(firstSeenTimeStr).Get()
	}

	// Record pod worker start latency if being created
	// TODO: make pod workers record their own latencies
	if updateType == kubetypes.SyncPodCreate {
		if !firstSeenTime.IsZero() {
			// This is the first time we are syncing the pod. Record the latency
			// since kubelet first saw the pod if firstSeenTime is set.
			metrics.PodWorkerStartDuration.Observe(metrics.SinceInSeconds(firstSeenTime))
		} else {
			klog.V(3).InfoS("First seen time not recorded for pod",
				"podUID", pod.UID,
				"pod", klog.KObj(pod))
		}
	}

	// handlePodResourcesResize updates the pod to use the allocated resources. This should come
	// before the main business logic of SyncPod, so that a consistent view of the pod is used
	// across the sync loop.
	if kuberuntime.IsInPlacePodVerticalScalingAllowed(pod) {
		// Handle pod resize here instead of doing it in HandlePodUpdates because
		// this conveniently retries any Deferred resize requests
		// TODO(vinaykul,InPlacePodVerticalScaling): Investigate doing this in HandlePodUpdates + periodic SyncLoop scan
		//     See: https://github.com/kubernetes/kubernetes/pull/102884#discussion_r663160060
		pod, err = kl.handlePodResourcesResize(pod, podStatus)
		if err != nil {
			return false, err
		}
	}

	// Generate final API pod status with pod and status manager status
	apiPodStatus := kl.generateAPIPodStatus(pod, podStatus, false)
	// The pod IP may be changed in generateAPIPodStatus if the pod is using host network. (See #24576)
	// TODO(random-liu): After writing pod spec into container labels, check whether pod is using host network, and
	// set pod IP to hostIP directly in runtime.GetPodStatus
	podStatus.IPs = make([]string, 0, len(apiPodStatus.PodIPs))
	for _, ipInfo := range apiPodStatus.PodIPs {
		podStatus.IPs = append(podStatus.IPs, ipInfo.IP)
	}
	if len(podStatus.IPs) == 0 && len(apiPodStatus.PodIP) > 0 {
		podStatus.IPs = []string{apiPodStatus.PodIP}
	}

	// If the pod is terminal, we don't need to continue to setup the pod
	if apiPodStatus.Phase == v1.PodSucceeded || apiPodStatus.Phase == v1.PodFailed {
		kl.statusManager.SetPodStatus(pod, apiPodStatus)
		isTerminal = true
		return isTerminal, nil
	}

	// Record the time it takes for the pod to become running
	// since kubelet first saw the pod if firstSeenTime is set.
	existingStatus, ok := kl.statusManager.GetPodStatus(pod.UID)
	if !ok || existingStatus.Phase == v1.PodPending && apiPodStatus.Phase == v1.PodRunning &&
		!firstSeenTime.IsZero() {
		metrics.PodStartDuration.Observe(metrics.SinceInSeconds(firstSeenTime))
	}

	kl.statusManager.SetPodStatus(pod, apiPodStatus)

	// If the network plugin is not ready, only start the pod if it uses the host network
	if err := kl.runtimeState.networkErrors(); err != nil && !kubecontainer.IsHostNetworkPod(pod) {
		kl.recorder.Eventf(pod, v1.EventTypeWarning, events.NetworkNotReady, "%s: %v", NetworkNotReadyErrorMsg, err)
		return false, fmt.Errorf("%s: %v", NetworkNotReadyErrorMsg, err)
	}

	// ensure the kubelet knows about referenced secrets or configmaps used by the pod
	if !kl.podWorkers.IsPodTerminationRequested(pod.UID) {
		if kl.secretManager != nil {
			kl.secretManager.RegisterPod(pod)
		}
		if kl.configMapManager != nil {
			kl.configMapManager.RegisterPod(pod)
		}
	}

	// Create Cgroups for the pod and apply resource parameters
	// to them if cgroups-per-qos flag is enabled.
	pcm := kl.containerManager.NewPodContainerManager()
	// If pod has already been terminated then we need not create
	// or update the pod's cgroup
	// TODO: once context cancellation is added this check can be removed
	if !kl.podWorkers.IsPodTerminationRequested(pod.UID) {
		// When the kubelet is restarted with the cgroups-per-qos
		// flag enabled, all the pod's running containers
		// should be killed intermittently and brought back up
		// under the qos cgroup hierarchy.
		// Check if this is the pod's first sync
		firstSync := true
		for _, containerStatus := range apiPodStatus.ContainerStatuses {
			if containerStatus.State.Running != nil {
				firstSync = false
				break
			}
		}
		// Don't kill containers in pod if pod's cgroups already
		// exists or the pod is running for the first time
		podKilled := false
		if !pcm.Exists(pod) && !firstSync {
			p := kubecontainer.ConvertPodStatusToRunningPod(kl.getRuntime().Type(), podStatus)
			if err := kl.killPod(ctx, pod, p, nil); err == nil {
				podKilled = true
			} else {
				if wait.Interrupted(err) {
					return false, nil
				}
				klog.ErrorS(err, "KillPod failed", "pod", klog.KObj(pod), "podStatus", podStatus)
			}
		}
		// Create and Update pod's Cgroups
		// Don't create cgroups for run once pod if it was killed above
		// The current policy is not to restart the run once pods when
		// the kubelet is restarted with the new flag as run once pods are
		// expected to run only once and if the kubelet is restarted then
		// they are not expected to run again.
		// We don't create and apply updates to cgroup if its a run once pod and was killed above
		if !(podKilled && pod.Spec.RestartPolicy == v1.RestartPolicyNever) {
			if !pcm.Exists(pod) {
				if err := kl.containerManager.UpdateQOSCgroups(); err != nil {
					klog.V(2).InfoS("Failed to update QoS cgroups while syncing pod", "pod", klog.KObj(pod), "err", err)
				}
				if err := pcm.EnsureExists(pod); err != nil {
					kl.recorder.Eventf(pod, v1.EventTypeWarning, events.FailedToCreatePodContainer, "unable to ensure pod container exists: %v", err)
					return false, fmt.Errorf("failed to ensure that the pod: %v cgroups exist and are correctly applied: %v", pod.UID, err)
				}
			}
		}
	}

	// Create Mirror Pod for Static Pod if it doesn't already exist
	kl.tryReconcileMirrorPods(pod, mirrorPod)

	// Make data directories for the pod
	if err := kl.makePodDataDirs(pod); err != nil {
		kl.recorder.Eventf(pod, v1.EventTypeWarning, events.FailedToMakePodDataDirectories, "error making pod data directories: %v", err)
		klog.ErrorS(err, "Unable to make pod data directories for pod", "pod", klog.KObj(pod))
		return false, err
	}

	// Wait for volumes to attach/mount
	if err := kl.volumeManager.WaitForAttachAndMount(ctx, pod); err != nil {
		if !wait.Interrupted(err) {
			kl.recorder.Eventf(pod, v1.EventTypeWarning, events.FailedMountVolume, "Unable to attach or mount volumes: %v", err)
			klog.ErrorS(err, "Unable to attach or mount volumes for pod; skipping pod", "pod", klog.KObj(pod))
		}
		return false, err
	}

	// Fetch the pull secrets for the pod
	pullSecrets := kl.getPullSecretsForPod(pod)

	// Ensure the pod is being probed
	kl.probeManager.AddPod(pod)

	// TODO(#113606): use cancellation from the incoming context parameter, which comes from the pod worker.
	// Currently, using cancellation from that context causes test failures. To remove this WithoutCancel,
	// any wait.Interrupted errors need to be filtered from result and bypass the reasonCache - cancelling
	// the context for SyncPod is a known and deliberate error, not a generic error.
	// Use WithoutCancel instead of a new context.TODO() to propagate trace context
	// Call the container runtime's SyncPod callback
	sctx := context.WithoutCancel(ctx)
	result := kl.containerRuntime.SyncPod(sctx, pod, podStatus, pullSecrets, kl.backOff)
	kl.reasonCache.Update(pod.UID, result)
	if err := result.Error(); err != nil {
		// Do not return error if the only failures were pods in backoff
		for _, r := range result.SyncResults {
			if r.Error != kubecontainer.ErrCrashLoopBackOff && r.Error != images.ErrImagePullBackOff {
				// Do not record an event here, as we keep all event logging for sync pod failures
				// local to container runtime, so we get better errors.
				return false, err
			}
		}

		return false, nil
	}

	return false, nil
}

// SyncTerminatingPod is expected to terminate all running containers in a pod. Once this method
// returns without error, the pod is considered to be terminated and it will be safe to clean up any
// pod state that is tied to the lifetime of running containers. The next method invoked will be
// SyncTerminatedPod. This method is expected to return with the grace period provided and the
// provided context may be cancelled if the duration is exceeded. The method may also be interrupted
// with a context cancellation if the grace period is shortened by the user or the kubelet (such as
// during eviction). This method is not guaranteed to be called if a pod is force deleted from the
// configuration and the kubelet is restarted - SyncTerminatingRuntimePod handles those orphaned
// pods.
func (kl *Kubelet) SyncTerminatingPod(_ context.Context, pod *v1.Pod, podStatus *kubecontainer.PodStatus, gracePeriod *int64, podStatusFn func(*v1.PodStatus)) error {
	// TODO(#113606): connect this with the incoming context parameter, which comes from the pod worker.
	// Currently, using that context causes test failures.
	ctx, otelSpan := kl.tracer.Start(context.Background(), "syncTerminatingPod", trace.WithAttributes(
		semconv.K8SPodUIDKey.String(string(pod.UID)),
		attribute.String("k8s.pod", klog.KObj(pod).String()),
		semconv.K8SPodNameKey.String(pod.Name),
		semconv.K8SNamespaceNameKey.String(pod.Namespace),
	))
	defer otelSpan.End()
	klog.V(4).InfoS("SyncTerminatingPod enter", "pod", klog.KObj(pod), "podUID", pod.UID)
	defer klog.V(4).InfoS("SyncTerminatingPod exit", "pod", klog.KObj(pod), "podUID", pod.UID)

	apiPodStatus := kl.generateAPIPodStatus(pod, podStatus, false)
	if podStatusFn != nil {
		podStatusFn(&apiPodStatus)
	}
	kl.statusManager.SetPodStatus(pod, apiPodStatus)

	if gracePeriod != nil {
		klog.V(4).InfoS("Pod terminating with grace period", "pod", klog.KObj(pod), "podUID", pod.UID, "gracePeriod", *gracePeriod)
	} else {
		klog.V(4).InfoS("Pod terminating with grace period", "pod", klog.KObj(pod), "podUID", pod.UID, "gracePeriod", nil)
	}

	kl.probeManager.StopLivenessAndStartup(pod)

	p := kubecontainer.ConvertPodStatusToRunningPod(kl.getRuntime().Type(), podStatus)
	if err := kl.killPod(ctx, pod, p, gracePeriod); err != nil {
		kl.recorder.Eventf(pod, v1.EventTypeWarning, events.FailedToKillPod, "error killing pod: %v", err)
		// there was an error killing the pod, so we return that error directly
		utilruntime.HandleError(err)
		return err
	}

	// Once the containers are stopped, we can stop probing for liveness and readiness.
	// TODO: once a pod is terminal, certain probes (liveness exec) could be stopped immediately after
	//   the detection of a container shutdown or (for readiness) after the first failure. Tracked as
	//   https://github.com/kubernetes/kubernetes/issues/107894 although may not be worth optimizing.
	kl.probeManager.RemovePod(pod)

	// Guard against consistency issues in KillPod implementations by checking that there are no
	// running containers. This method is invoked infrequently so this is effectively free and can
	// catch race conditions introduced by callers updating pod status out of order.
	// TODO: have KillPod return the terminal status of stopped containers and write that into the
	//  cache immediately
	stoppedPodStatus, err := kl.containerRuntime.GetPodStatus(ctx, pod.UID, pod.Name, pod.Namespace)
	if err != nil {
		klog.ErrorS(err, "Unable to read pod status prior to final pod termination", "pod", klog.KObj(pod), "podUID", pod.UID)
		return err
	}
	preserveDataFromBeforeStopping(stoppedPodStatus, podStatus)
	var runningContainers []string
	type container struct {
		Name       string
		State      string
		ExitCode   int
		FinishedAt string
	}
	var containers []container
	klogV := klog.V(4)
	klogVEnabled := klogV.Enabled()
	for _, s := range stoppedPodStatus.ContainerStatuses {
		if s.State == kubecontainer.ContainerStateRunning {
			runningContainers = append(runningContainers, s.ID.String())
		}
		if klogVEnabled {
			containers = append(containers, container{Name: s.Name, State: string(s.State), ExitCode: s.ExitCode, FinishedAt: s.FinishedAt.UTC().Format(time.RFC3339Nano)})
		}
	}
	if klogVEnabled {
		sort.Slice(containers, func(i, j int) bool { return containers[i].Name < containers[j].Name })
		klog.V(4).InfoS("Post-termination container state", "pod", klog.KObj(pod), "podUID", pod.UID, "containers", containers)
	}
	if len(runningContainers) > 0 {
		return fmt.Errorf("detected running containers after a successful KillPod, CRI violation: %v", runningContainers)
	}

	// NOTE: resources must be unprepared AFTER all containers have stopped
	// and BEFORE the pod status is changed on the API server
	// to avoid race conditions with the resource deallocation code in kubernetes core.
	if utilfeature.DefaultFeatureGate.Enabled(features.DynamicResourceAllocation) {
		if err := kl.UnprepareDynamicResources(ctx, pod); err != nil {
			return err
		}
	}

	// Compute and update the status in cache once the pods are no longer running.
	// The computation is done here to ensure the pod status used for it contains
	// information about the container end states (including exit codes) - when
	// SyncTerminatedPod is called the containers may already be removed.
	apiPodStatus = kl.generateAPIPodStatus(pod, stoppedPodStatus, true)
	kl.statusManager.SetPodStatus(pod, apiPodStatus)

	// we have successfully stopped all containers, the pod is terminating, our status is "done"
	klog.V(4).InfoS("Pod termination stopped all running containers", "pod", klog.KObj(pod), "podUID", pod.UID)

	return nil
}

// preserveDataFromBeforeStopping preserves data, like IPs, which are expected
// to be sent to the API server after termination, but are no longer returned by
// containerRuntime.GetPodStatus for a stopped pod.
// Note that Kubelet restart, after the pod is stopped, may still cause losing
// track of the data.
func preserveDataFromBeforeStopping(stoppedPodStatus, podStatus *kubecontainer.PodStatus) {
	stoppedPodStatus.IPs = podStatus.IPs
}

// SyncTerminatingRuntimePod is expected to terminate running containers in a pod that we have no
// configuration for. Once this method returns without error, any remaining local state can be safely
// cleaned up by background processes in each subsystem. Unlike syncTerminatingPod, we lack
// knowledge of the full pod spec and so cannot perform lifecycle related operations, only ensure
// that the remnant of the running pod is terminated and allow garbage collection to proceed. We do
// not update the status of the pod because with the source of configuration removed, we have no
// place to send that status.
func (kl *Kubelet) SyncTerminatingRuntimePod(_ context.Context, runningPod *kubecontainer.Pod) error {
	// TODO(#113606): connect this with the incoming context parameter, which comes from the pod worker.
	// Currently, using that context causes test failures.
	ctx := context.Background()
	pod := runningPod.ToAPIPod()
	klog.V(4).InfoS("SyncTerminatingRuntimePod enter", "pod", klog.KObj(pod), "podUID", pod.UID)
	defer klog.V(4).InfoS("SyncTerminatingRuntimePod exit", "pod", klog.KObj(pod), "podUID", pod.UID)

	// we kill the pod directly since we have lost all other information about the pod.
	klog.V(4).InfoS("Orphaned running pod terminating without grace period", "pod", klog.KObj(pod), "podUID", pod.UID)
	// TODO: this should probably be zero, to bypass any waiting (needs fixes in container runtime)
	gracePeriod := int64(1)
	if err := kl.killPod(ctx, pod, *runningPod, &gracePeriod); err != nil {
		kl.recorder.Eventf(pod, v1.EventTypeWarning, events.FailedToKillPod, "error killing pod: %v", err)
		// there was an error killing the pod, so we return that error directly
		utilruntime.HandleError(err)
		return err
	}
	klog.V(4).InfoS("Pod termination stopped all running orphaned containers", "pod", klog.KObj(pod), "podUID", pod.UID)
	return nil
}

// SyncTerminatedPod cleans up a pod that has terminated (has no running containers).
// The invocations in this call are expected to tear down all pod resources.
// When this method exits the pod is expected to be ready for cleanup. This method
// reduces the latency of pod cleanup but is not guaranteed to get called in all scenarios.
//
// Because the kubelet has no local store of information, all actions in this method that modify
// on-disk state must be reentrant and be garbage collected by HandlePodCleanups or a separate loop.
// This typically occurs when a pod is force deleted from configuration (local disk or API) and the
// kubelet restarts in the middle of the action.
func (kl *Kubelet) SyncTerminatedPod(ctx context.Context, pod *v1.Pod, podStatus *kubecontainer.PodStatus) error {
	ctx, otelSpan := kl.tracer.Start(ctx, "syncTerminatedPod", trace.WithAttributes(
		semconv.K8SPodUIDKey.String(string(pod.UID)),
		attribute.String("k8s.pod", klog.KObj(pod).String()),
		semconv.K8SPodNameKey.String(pod.Name),
		semconv.K8SNamespaceNameKey.String(pod.Namespace),
	))
	defer otelSpan.End()
	klog.V(4).InfoS("SyncTerminatedPod enter", "pod", klog.KObj(pod), "podUID", pod.UID)
	defer klog.V(4).InfoS("SyncTerminatedPod exit", "pod", klog.KObj(pod), "podUID", pod.UID)

	// generate the final status of the pod
	// TODO: should we simply fold this into TerminatePod? that would give a single pod update
	apiPodStatus := kl.generateAPIPodStatus(pod, podStatus, true)

	kl.statusManager.SetPodStatus(pod, apiPodStatus)

	// volumes are unmounted after the pod worker reports ShouldPodRuntimeBeRemoved (which is satisfied
	// before syncTerminatedPod is invoked)
	if err := kl.volumeManager.WaitForUnmount(ctx, pod); err != nil {
		return err
	}
	klog.V(4).InfoS("Pod termination unmounted volumes", "pod", klog.KObj(pod), "podUID", pod.UID)

	// This waiting loop relies on the background cleanup which starts after pod workers respond
	// true for ShouldPodRuntimeBeRemoved, which happens after `SyncTerminatingPod` is completed.
	if err := wait.PollUntilContextCancel(ctx, 100*time.Millisecond, true, func(ctx context.Context) (bool, error) {
		volumesExist := kl.podVolumesExist(pod.UID)
		if volumesExist {
			klog.V(3).InfoS("Pod is terminated, but some volumes have not been cleaned up", "pod", klog.KObj(pod), "podUID", pod.UID)
		}
		return !volumesExist, nil
	}); err != nil {
		return err
	}
	klog.V(3).InfoS("Pod termination cleaned up volume paths", "pod", klog.KObj(pod), "podUID", pod.UID)

	// After volume unmount is complete, let the secret and configmap managers know we're done with this pod
	if kl.secretManager != nil {
		kl.secretManager.UnregisterPod(pod)
	}
	if kl.configMapManager != nil {
		kl.configMapManager.UnregisterPod(pod)
	}

	// Note: we leave pod containers to be reclaimed in the background since dockershim requires the
	// container for retrieving logs and we want to make sure logs are available until the pod is
	// physically deleted.

	// remove any cgroups in the hierarchy for pods that are no longer running.
	if kl.cgroupsPerQOS {
		pcm := kl.containerManager.NewPodContainerManager()
		name, _ := pcm.GetPodContainerName(pod)
		if err := pcm.Destroy(name); err != nil {
			return err
		}
		klog.V(4).InfoS("Pod termination removed cgroups", "pod", klog.KObj(pod), "podUID", pod.UID)
	}

	kl.usernsManager.Release(pod.UID)

	// mark the final pod status
	kl.statusManager.TerminatePod(pod)
	klog.V(4).InfoS("Pod is terminated and will need no more status updates", "pod", klog.KObj(pod), "podUID", pod.UID)

	return nil
}

// Get pods which should be resynchronized. Currently, the following pod should be resynchronized:
//   - pod whose work is ready.
//   - internal modules that request sync of a pod.
//
// This method does not return orphaned pods (those known only to the pod worker that may have
// been deleted from configuration). Those pods are synced by HandlePodCleanups as a consequence
// of driving the state machine to completion.
//
// TODO: Consider synchronizing all pods which have not recently been acted on to be resilient
// to bugs that might prevent updates from being delivered (such as the previous bug with
// orphaned pods). Instead of asking the work queue for pending work, consider asking the
// PodWorker which pods should be synced.
func (kl *Kubelet) getPodsToSync() []*v1.Pod {
	allPods := kl.podManager.GetPods()
	podUIDs := kl.workQueue.GetWork()
	podUIDSet := sets.New[string]()
	for _, podUID := range podUIDs {
		podUIDSet.Insert(string(podUID))
	}
	var podsToSync []*v1.Pod
	for _, pod := range allPods {
		if podUIDSet.Has(string(pod.UID)) {
			// The work of the pod is ready
			podsToSync = append(podsToSync, pod)
			continue
		}
		for _, podSyncLoopHandler := range kl.PodSyncLoopHandlers {
			if podSyncLoopHandler.ShouldSync(pod) {
				podsToSync = append(podsToSync, pod)
				break
			}
		}
	}
	return podsToSync
}

// deletePod deletes the pod from the internal state of the kubelet by:
// 1.  stopping the associated pod worker asynchronously
// 2.  signaling to kill the pod by sending on the podKillingCh channel
//
// deletePod returns an error if not all sources are ready or the pod is not
// found in the runtime cache.
func (kl *Kubelet) deletePod(pod *v1.Pod) error {
	if pod == nil {
		return fmt.Errorf("deletePod does not allow nil pod")
	}
	if !kl.sourcesReady.AllReady() {
		// If the sources aren't ready, skip deletion, as we may accidentally delete pods
		// for sources that haven't reported yet.
		return fmt.Errorf("skipping delete because sources aren't ready yet")
	}
	klog.V(3).InfoS("Pod has been deleted and must be killed", "pod", klog.KObj(pod), "podUID", pod.UID)
	kl.podWorkers.UpdatePod(UpdatePodOptions{
		Pod:        pod,
		UpdateType: kubetypes.SyncPodKill,
	})
	// We leave the volume/directory cleanup to the periodic cleanup routine.
	return nil
}

// rejectPod records an event about the pod with the given reason and message,
// and updates the pod to the failed phase in the status manager.
func (kl *Kubelet) rejectPod(pod *v1.Pod, reason, message string) {
	kl.recorder.Eventf(pod, v1.EventTypeWarning, reason, message)
	kl.statusManager.SetPodStatus(pod, v1.PodStatus{
		QOSClass: v1qos.GetPodQOS(pod), // keep it as is
		Phase:    v1.PodFailed,
		Reason:   reason,
		Message:  "Pod was rejected: " + message})
}

// canAdmitPod determines if a pod can be admitted, and gives a reason if it
// cannot. "pod" is new pod, while "pods" are all admitted pods
// The function returns a boolean value indicating whether the pod
// can be admitted, a brief single-word reason and a message explaining why
// the pod cannot be admitted.
// allocatedPods should represent the pods that have already been admitted, along with their
// admitted (allocated) resources.
func (kl *Kubelet) canAdmitPod(allocatedPods []*v1.Pod, pod *v1.Pod) (bool, string, string) {
	// the kubelet will invoke each pod admit handler in sequence
	// if any handler rejects, the pod is rejected.
	// TODO: move out of disk check into a pod admitter
	// TODO: out of resource eviction should have a pod admitter call-out
	attrs := &lifecycle.PodAdmitAttributes{Pod: pod, OtherPods: allocatedPods}
	for _, podAdmitHandler := range kl.admitHandlers {
		if result := podAdmitHandler.Admit(attrs); !result.Admit {
			klog.InfoS("Pod admission denied", "podUID", attrs.Pod.UID, "pod", klog.KObj(attrs.Pod), "reason", result.Reason, "message", result.Message)

			return false, result.Reason, result.Message
		}
	}

	return true, "", ""
}

func recordAdmissionRejection(reason string) {
	// It is possible that the "reason" label can have high cardinality.
	// To avoid this metric from exploding, we create an allowlist of known
	// reasons, and only record reasons from this list. Use "Other" reason
	// for the rest.
	if admissionRejectionReasons.Has(reason) {
		metrics.AdmissionRejectionsTotal.WithLabelValues(reason).Inc()
	} else if strings.HasPrefix(reason, lifecycle.InsufficientResourcePrefix) {
		// non-extended resources (like cpu, memory, ephemeral-storage, pods)
		// are already included in admissionRejectionReasons.
		metrics.AdmissionRejectionsTotal.WithLabelValues("OutOfExtendedResources").Inc()
	} else {
		metrics.AdmissionRejectionsTotal.WithLabelValues("Other").Inc()
	}
}

// syncLoop is the main loop for processing changes. It watches for changes from
// three channels (file, apiserver, and http) and creates a union of them. For
// any new change seen, will run a sync against desired state and running state. If
// no changes are seen to the configuration, will synchronize the last known desired
// state every sync-frequency seconds. Never returns.
func (kl *Kubelet) syncLoop(ctx context.Context, updates <-chan kubetypes.PodUpdate, handler SyncHandler) {
	klog.InfoS("Starting kubelet main sync loop")
	// The syncTicker wakes up kubelet to checks if there are any pod workers
	// that need to be sync'd. A one-second period is sufficient because the
	// sync interval is defaulted to 10s.
	syncTicker := time.NewTicker(time.Second)
	defer syncTicker.Stop()
	housekeepingTicker := time.NewTicker(housekeepingPeriod)
	defer housekeepingTicker.Stop()
	plegCh := kl.pleg.Watch()
	const (
		base   = 100 * time.Millisecond
		max    = 5 * time.Second
		factor = 2
	)
	duration := base
	// Responsible for checking limits in resolv.conf
	// The limits do not have anything to do with individual pods
	// Since this is called in syncLoop, we don't need to call it anywhere else
	if kl.dnsConfigurer != nil && kl.dnsConfigurer.ResolverConfig != "" {
		kl.dnsConfigurer.CheckLimitsForResolvConf()
	}

	for {
		if err := kl.runtimeState.runtimeErrors(); err != nil {
			klog.ErrorS(err, "Skipping pod synchronization")
			// exponential backoff
			time.Sleep(duration)
			duration = time.Duration(math.Min(float64(max), factor*float64(duration)))
			continue
		}
		// reset backoff if we have a success
		duration = base

		kl.syncLoopMonitor.Store(kl.clock.Now())
		if !kl.syncLoopIteration(ctx, updates, handler, syncTicker.C, housekeepingTicker.C, plegCh) {
			break
		}
		kl.syncLoopMonitor.Store(kl.clock.Now())
	}
}

// syncLoopIteration reads from various channels and dispatches pods to the
// given handler.
//
// Arguments:
// 1.  configCh:       a channel to read config events from
// 2.  handler:        the SyncHandler to dispatch pods to
// 3.  syncCh:         a channel to read periodic sync events from
// 4.  housekeepingCh: a channel to read housekeeping events from
// 5.  plegCh:         a channel to read PLEG updates from
//
// Events are also read from the kubelet liveness manager's update channel.
//
// The workflow is to read from one of the channels, handle that event, and
// update the timestamp in the sync loop monitor.
//
// Here is an appropriate place to note that despite the syntactical
// similarity to the switch statement, the case statements in a select are
// evaluated in a pseudorandom order if there are multiple channels ready to
// read from when the select is evaluated.  In other words, case statements
// are evaluated in random order, and you can not assume that the case
// statements evaluate in order if multiple channels have events.
//
// With that in mind, in truly no particular order, the different channels
// are handled as follows:
//
//   - configCh: dispatch the pods for the config change to the appropriate
//     handler callback for the event type
//   - plegCh: update the runtime cache; sync pod
//   - syncCh: sync all pods waiting for sync
//   - housekeepingCh: trigger cleanup of pods
//   - health manager: sync pods that have failed or in which one or more
//     containers have failed health checks
func (kl *Kubelet) syncLoopIteration(ctx context.Context, configCh <-chan kubetypes.PodUpdate, handler SyncHandler,
	syncCh <-chan time.Time, housekeepingCh <-chan time.Time, plegCh <-chan *pleg.PodLifecycleEvent) bool {
	select {
	case u, open := <-configCh:
		// Update from a config source; dispatch it to the right handler
		// callback.
		if !open {
			klog.ErrorS(nil, "Update channel is closed, exiting the sync loop")
			return false
		}

		switch u.Op {
		case kubetypes.ADD:
			klog.V(2).InfoS("SyncLoop ADD", "source", u.Source, "pods", klog.KObjSlice(u.Pods))
			// After restarting, kubelet will get all existing pods through
			// ADD as if they are new pods. These pods will then go through the
			// admission process and *may* be rejected. This can be resolved
			// once we have checkpointing.
			handler.HandlePodAdditions(u.Pods)
		case kubetypes.UPDATE:
			klog.V(2).InfoS("SyncLoop UPDATE", "source", u.Source, "pods", klog.KObjSlice(u.Pods))
			handler.HandlePodUpdates(u.Pods)
		case kubetypes.REMOVE:
			klog.V(2).InfoS("SyncLoop REMOVE", "source", u.Source, "pods", klog.KObjSlice(u.Pods))
			handler.HandlePodRemoves(u.Pods)
		case kubetypes.RECONCILE:
			klog.V(4).InfoS("SyncLoop RECONCILE", "source", u.Source, "pods", klog.KObjSlice(u.Pods))
			handler.HandlePodReconcile(u.Pods)
		case kubetypes.DELETE:
			klog.V(2).InfoS("SyncLoop DELETE", "source", u.Source, "pods", klog.KObjSlice(u.Pods))
			// DELETE is treated as a UPDATE because of graceful deletion.
			handler.HandlePodUpdates(u.Pods)
		case kubetypes.SET:
			// TODO: Do we want to support this?
			klog.ErrorS(nil, "Kubelet does not support snapshot update")
		default:
			klog.ErrorS(nil, "Invalid operation type received", "operation", u.Op)
		}

		kl.sourcesReady.AddSource(u.Source)

	case e := <-plegCh:
		if isSyncPodWorthy(e) {
			// PLEG event for a pod; sync it.
			if pod, ok := kl.podManager.GetPodByUID(e.ID); ok {
				klog.V(2).InfoS("SyncLoop (PLEG): event for pod", "pod", klog.KObj(pod), "event", e)
				handler.HandlePodSyncs([]*v1.Pod{pod})
			} else {
				// If the pod no longer exists, ignore the event.
				klog.V(4).InfoS("SyncLoop (PLEG): pod does not exist, ignore irrelevant event", "event", e)
			}
		}

		if e.Type == pleg.ContainerDied {
			if containerID, ok := e.Data.(string); ok {
				kl.cleanUpContainersInPod(e.ID, containerID)
			}
		}
	case <-syncCh:
		// Sync pods waiting for sync
		podsToSync := kl.getPodsToSync()
		if len(podsToSync) == 0 {
			break
		}
		klog.V(4).InfoS("SyncLoop (SYNC) pods", "total", len(podsToSync), "pods", klog.KObjSlice(podsToSync))
		handler.HandlePodSyncs(podsToSync)
	case update := <-kl.livenessManager.Updates():
		if update.Result == proberesults.Failure {
			handleProbeSync(kl, update, handler, "liveness", "unhealthy")
		}
	case update := <-kl.readinessManager.Updates():
		ready := update.Result == proberesults.Success
		kl.statusManager.SetContainerReadiness(update.PodUID, update.ContainerID, ready)

		status := "not ready"
		if ready {
			status = "ready"
		}
		handleProbeSync(kl, update, handler, "readiness", status)
	case update := <-kl.startupManager.Updates():
		started := update.Result == proberesults.Success
		kl.statusManager.SetContainerStartup(update.PodUID, update.ContainerID, started)

		status := "unhealthy"
		if started {
			status = "started"
		}
		handleProbeSync(kl, update, handler, "startup", status)
	case update := <-kl.containerManager.Updates():
		pods := []*v1.Pod{}
		for _, p := range update.PodUIDs {
			if pod, ok := kl.podManager.GetPodByUID(types.UID(p)); ok {
				klog.V(3).InfoS("SyncLoop (containermanager): event for pod", "pod", klog.KObj(pod), "event", update)
				pods = append(pods, pod)
			} else {
				// If the pod no longer exists, ignore the event.
				klog.V(4).InfoS("SyncLoop (containermanager): pod does not exist, ignore devices updates", "event", update)
			}
		}
		if len(pods) > 0 {
			// Updating the pod by syncing it again
			// We do not apply the optimization by updating the status directly, but can do it later
			handler.HandlePodSyncs(pods)
		}

	case <-housekeepingCh:
		if !kl.sourcesReady.AllReady() {
			// If the sources aren't ready or volume manager has not yet synced the states,
			// skip housekeeping, as we may accidentally delete pods from unready sources.
			klog.V(4).InfoS("SyncLoop (housekeeping, skipped): sources aren't ready yet")
		} else {
			start := time.Now()
			klog.V(4).InfoS("SyncLoop (housekeeping)")
			if err := handler.HandlePodCleanups(ctx); err != nil {
				klog.ErrorS(err, "Failed cleaning pods")
			}
			duration := time.Since(start)
			if duration > housekeepingWarningDuration {
				klog.ErrorS(fmt.Errorf("housekeeping took too long"), "Housekeeping took longer than expected", "expected", housekeepingWarningDuration, "actual", duration.Round(time.Millisecond))
			}
			klog.V(4).InfoS("SyncLoop (housekeeping) end", "duration", duration.Round(time.Millisecond))
		}
	}
	return true
}

func handleProbeSync(kl *Kubelet, update proberesults.Update, handler SyncHandler, probe, status string) {
	// We should not use the pod from manager, because it is never updated after initialization.
	pod, ok := kl.podManager.GetPodByUID(update.PodUID)
	if !ok {
		// If the pod no longer exists, ignore the update.
		klog.V(4).InfoS("SyncLoop (probe): ignore irrelevant update", "probe", probe, "status", status, "update", update)
		return
	}
	klog.V(1).InfoS("SyncLoop (probe)", "probe", probe, "status", status, "pod", klog.KObj(pod))
	handler.HandlePodSyncs([]*v1.Pod{pod})
}

// HandlePodAdditions is the callback in SyncHandler for pods being added from
// a config source.
func (kl *Kubelet) HandlePodAdditions(pods []*v1.Pod) {
	start := kl.clock.Now()
	sort.Sort(sliceutils.PodsByCreationTime(pods))
	if utilfeature.DefaultFeatureGate.Enabled(features.InPlacePodVerticalScaling) {
		kl.podResizeMutex.Lock()
		defer kl.podResizeMutex.Unlock()
	}
	for _, pod := range pods {
		// Always add the pod to the pod manager. Kubelet relies on the pod
		// manager as the source of truth for the desired state. If a pod does
		// not exist in the pod manager, it means that it has been deleted in
		// the apiserver and no action (other than cleanup) is required.
		kl.podManager.AddPod(pod)

		pod, mirrorPod, wasMirror := kl.podManager.GetPodAndMirrorPod(pod)
		if wasMirror {
			if pod == nil {
				klog.V(2).InfoS("Unable to find pod for mirror pod, skipping", "mirrorPod", klog.KObj(mirrorPod), "mirrorPodUID", mirrorPod.UID)
				continue
			}
			kl.podWorkers.UpdatePod(UpdatePodOptions{
				Pod:        pod,
				MirrorPod:  mirrorPod,
				UpdateType: kubetypes.SyncPodUpdate,
				StartTime:  start,
			})
			continue
		}

		// Only go through the admission process if the pod is not requested
		// for termination by another part of the kubelet. If the pod is already
		// using resources (previously admitted), the pod worker is going to be
		// shutting it down. If the pod hasn't started yet, we know that when
		// the pod worker is invoked it will also avoid setting up the pod, so
		// we simply avoid doing any work.
		// We also do not try to admit the pod that is already in terminated state.
		if !kl.podWorkers.IsPodTerminationRequested(pod.UID) && !podutil.IsPodPhaseTerminal(pod.Status.Phase) {
			// We failed pods that we rejected, so allocatedPods include all admitted
			// pods that are alive.
			allocatedPods := kl.getAllocatedPods()
			// Filter out the pod being evaluated.
			allocatedPods = slices.DeleteFunc(allocatedPods, func(p *v1.Pod) bool { return p.UID == pod.UID })

			if utilfeature.DefaultFeatureGate.Enabled(features.InPlacePodVerticalScaling) {
				// To handle kubelet restarts, test pod admissibility using AllocatedResources values
				// (for cpu & memory) from checkpoint store. If found, that is the source of truth.
				allocatedPod, _ := kl.statusManager.UpdatePodFromAllocation(pod)

				// Check if we can admit the pod; if not, reject it.
				if ok, reason, message := kl.canAdmitPod(allocatedPods, allocatedPod); !ok {
					kl.rejectPod(pod, reason, message)
					// We avoid recording the metric in canAdmitPod because it's called
					// repeatedly during a resize, which would inflate the metric.
					// Instead, we record the metric here in HandlePodAdditions for new pods
					// and capture resize events separately.
					recordAdmissionRejection(reason)
					continue
				}
				// For new pod, checkpoint the resource values at which the Pod has been admitted
				if err := kl.statusManager.SetPodAllocation(allocatedPod); err != nil {
					//TODO(vinaykul,InPlacePodVerticalScaling): Can we recover from this in some way? Investigate
					klog.ErrorS(err, "SetPodAllocation failed", "pod", klog.KObj(pod))
				}
			} else {
				// Check if we can admit the pod; if not, reject it.
				if ok, reason, message := kl.canAdmitPod(allocatedPods, pod); !ok {
					kl.rejectPod(pod, reason, message)
					// We avoid recording the metric in canAdmitPod because it's called
					// repeatedly during a resize, which would inflate the metric.
					// Instead, we record the metric here in HandlePodAdditions for new pods
					// and capture resize events separately.
					recordAdmissionRejection(reason)
					continue
				}
			}
		}
		kl.podWorkers.UpdatePod(UpdatePodOptions{
			Pod:        pod,
			MirrorPod:  mirrorPod,
			UpdateType: kubetypes.SyncPodCreate,
			StartTime:  start,
		})
	}
}

// HandlePodUpdates is the callback in the SyncHandler interface for pods
// being updated from a config source.
func (kl *Kubelet) HandlePodUpdates(pods []*v1.Pod) {
	start := kl.clock.Now()
	for _, pod := range pods {
		kl.podManager.UpdatePod(pod)

		pod, mirrorPod, wasMirror := kl.podManager.GetPodAndMirrorPod(pod)
		if wasMirror {
			if pod == nil {
				klog.V(2).InfoS("Unable to find pod for mirror pod, skipping", "mirrorPod", klog.KObj(mirrorPod), "mirrorPodUID", mirrorPod.UID)
				continue
			}
		}

		kl.podWorkers.UpdatePod(UpdatePodOptions{
			Pod:        pod,
			MirrorPod:  mirrorPod,
			UpdateType: kubetypes.SyncPodUpdate,
			StartTime:  start,
		})
	}
}

// HandlePodRemoves is the callback in the SyncHandler interface for pods
// being removed from a config source.
func (kl *Kubelet) HandlePodRemoves(pods []*v1.Pod) {
	start := kl.clock.Now()
	for _, pod := range pods {
		kl.podManager.RemovePod(pod)

		pod, mirrorPod, wasMirror := kl.podManager.GetPodAndMirrorPod(pod)
		if wasMirror {
			if pod == nil {
				klog.V(2).InfoS("Unable to find pod for mirror pod, skipping", "mirrorPod", klog.KObj(mirrorPod), "mirrorPodUID", mirrorPod.UID)
				continue
			}
			kl.podWorkers.UpdatePod(UpdatePodOptions{
				Pod:        pod,
				MirrorPod:  mirrorPod,
				UpdateType: kubetypes.SyncPodUpdate,
				StartTime:  start,
			})
			continue
		}

		// Deletion is allowed to fail because the periodic cleanup routine
		// will trigger deletion again.
		if err := kl.deletePod(pod); err != nil {
			klog.V(2).InfoS("Failed to delete pod", "pod", klog.KObj(pod), "err", err)
		}
	}
}

// HandlePodReconcile is the callback in the SyncHandler interface for pods
// that should be reconciled. Pods are reconciled when only the status of the
// pod is updated in the API.
func (kl *Kubelet) HandlePodReconcile(pods []*v1.Pod) {
	start := kl.clock.Now()
	for _, pod := range pods {
		// Update the pod in pod manager, status manager will do periodically reconcile according
		// to the pod manager.
		kl.podManager.UpdatePod(pod)

		pod, mirrorPod, wasMirror := kl.podManager.GetPodAndMirrorPod(pod)
		if wasMirror {
			if pod == nil {
				klog.V(2).InfoS("Unable to find pod for mirror pod, skipping", "mirrorPod", klog.KObj(mirrorPod), "mirrorPodUID", mirrorPod.UID)
				continue
			}
			// Static pods should be reconciled the same way as regular pods
		}

		// TODO: reconcile being calculated in the config manager is questionable, and avoiding
		// extra syncs may no longer be necessary. Reevaluate whether Reconcile and Sync can be
		// merged (after resolving the next two TODOs).

		// Reconcile Pod "Ready" condition if necessary. Trigger sync pod for reconciliation.
		// TODO: this should be unnecessary today - determine what is the cause for this to
		// be different than Sync, or if there is a better place for it. For instance, we have
		// needsReconcile in kubelet/config, here, and in status_manager.
		if status.NeedToReconcilePodReadiness(pod) {
			kl.podWorkers.UpdatePod(UpdatePodOptions{
				Pod:        pod,
				MirrorPod:  mirrorPod,
				UpdateType: kubetypes.SyncPodSync,
				StartTime:  start,
			})
		}

		// After an evicted pod is synced, all dead containers in the pod can be removed.
		// TODO: this is questionable - status read is async and during eviction we already
		// expect to not have some container info. The pod worker knows whether a pod has
		// been evicted, so if this is about minimizing the time to react to an eviction we
		// can do better. If it's about preserving pod status info we can also do better.
		if eviction.PodIsEvicted(pod.Status) {
			if podStatus, err := kl.podCache.Get(pod.UID); err == nil {
				kl.containerDeletor.deleteContainersInPod("", podStatus, true)
			}
		}
	}
}

// HandlePodSyncs is the callback in the syncHandler interface for pods
// that should be dispatched to pod workers for sync.
func (kl *Kubelet) HandlePodSyncs(pods []*v1.Pod) {
	start := kl.clock.Now()
	for _, pod := range pods {
		pod, mirrorPod, wasMirror := kl.podManager.GetPodAndMirrorPod(pod)
		if wasMirror {
			if pod == nil {
				klog.V(2).InfoS("Unable to find pod for mirror pod, skipping", "mirrorPod", klog.KObj(mirrorPod), "mirrorPodUID", mirrorPod.UID)
				continue
			}
			// Syncing a mirror pod is a programmer error since the intent of sync is to
			// batch notify all pending work. We should make it impossible to double sync,
			// but for now log a programmer error to prevent accidental introduction.
			klog.V(3).InfoS("Programmer error, HandlePodSyncs does not expect to receive mirror pods", "podUID", pod.UID, "mirrorPodUID", mirrorPod.UID)
			continue
		}
		kl.podWorkers.UpdatePod(UpdatePodOptions{
			Pod:        pod,
			MirrorPod:  mirrorPod,
			UpdateType: kubetypes.SyncPodSync,
			StartTime:  start,
		})
	}
}

// canResizePod determines if the requested resize is currently feasible.
// pod should hold the desired (pre-allocated) spec.
// Returns true if the resize can proceed.
func (kl *Kubelet) canResizePod(pod *v1.Pod) (bool, v1.PodResizeStatus, string) {
	if goos == "windows" {
		return false, v1.PodResizeStatusInfeasible, "Resizing Windows pods is not supported"
	}

	if v1qos.GetPodQOS(pod) == v1.PodQOSGuaranteed && !utilfeature.DefaultFeatureGate.Enabled(features.InPlacePodVerticalScalingExclusiveCPUs) {
		if utilfeature.DefaultFeatureGate.Enabled(features.CPUManager) {
			if kl.containerManager.GetNodeConfig().CPUManagerPolicy == "static" {
				msg := "Resize is infeasible for Guaranteed Pods alongside CPU Manager static policy"
				klog.V(3).InfoS(msg, "pod", format.Pod(pod))
				return false, v1.PodResizeStatusInfeasible, msg
			}
		}
		if utilfeature.DefaultFeatureGate.Enabled(features.MemoryManager) {
			if kl.containerManager.GetNodeConfig().MemoryManagerPolicy == "static" {
				msg := "Resize is infeasible for Guaranteed Pods alongside Memory Manager static policy"
				klog.V(3).InfoS(msg, "pod", format.Pod(pod))
				return false, v1.PodResizeStatusInfeasible, msg
			}
		}
	}

	node, err := kl.getNodeAnyWay()
	if err != nil {
		klog.ErrorS(err, "getNodeAnyway function failed")
		return false, "", ""
	}
	cpuAvailable := node.Status.Allocatable.Cpu().MilliValue()
	memAvailable := node.Status.Allocatable.Memory().Value()
	cpuRequests := resource.GetResourceRequest(pod, v1.ResourceCPU)
	memRequests := resource.GetResourceRequest(pod, v1.ResourceMemory)
	if cpuRequests > cpuAvailable || memRequests > memAvailable {
		var msg string
		if memRequests > memAvailable {
			msg = fmt.Sprintf("memory, requested: %d, capacity: %d", memRequests, memAvailable)
		} else {
			msg = fmt.Sprintf("cpu, requested: %d, capacity: %d", cpuRequests, cpuAvailable)
		}
		msg = "Node didn't have enough capacity: " + msg
		klog.V(3).InfoS(msg, "pod", klog.KObj(pod))
		return false, v1.PodResizeStatusInfeasible, msg
	}

	// Treat the existing pod needing resize as a new pod with desired resources seeking admit.
	// If desired resources don't fit, pod continues to run with currently allocated resources.
	allocatedPods := kl.getAllocatedPods()
	allocatedPods = slices.DeleteFunc(allocatedPods, func(p *v1.Pod) bool { return p.UID == pod.UID })

	if ok, failReason, failMessage := kl.canAdmitPod(allocatedPods, pod); !ok {
		// Log reason and return. Let the next sync iteration retry the resize
		klog.V(3).InfoS("Resize cannot be accommodated", "pod", klog.KObj(pod), "reason", failReason, "message", failMessage)
		return false, v1.PodResizeStatusDeferred, failMessage
	}

	return true, v1.PodResizeStatusInProgress, ""
}

// handlePodResourcesResize returns the "allocated pod", which should be used for all resource
// calculations after this function is called. It also updates the cached ResizeStatus according to
// the allocation decision and pod status.
func (kl *Kubelet) handlePodResourcesResize(pod *v1.Pod, podStatus *kubecontainer.PodStatus) (*v1.Pod, error) {
	allocatedPod, updated := kl.statusManager.UpdatePodFromAllocation(pod)
	if !updated {
		// Desired resources == allocated resources. Check whether a resize is in progress.
		resizeInProgress := !allocatedResourcesMatchStatus(allocatedPod, podStatus)
		if resizeInProgress {
			// If a resize is in progress, make sure the cache has the correct state in case the Kubelet restarted.
			kl.statusManager.SetPodResizeStatus(pod.UID, v1.PodResizeStatusInProgress)
		} else {
			// (Desired == Allocated == Actual) => clear the resize status.
			kl.statusManager.SetPodResizeStatus(pod.UID, "")
		}
		// Pod allocation does not need to be updated.
		return allocatedPod, nil
	}

	kl.podResizeMutex.Lock()
	defer kl.podResizeMutex.Unlock()
	// Desired resources != allocated resources. Can we update the allocation to the desired resources?
	fit, resizeStatus, resizeMsg := kl.canResizePod(pod)
	if fit {
		// Update pod resource allocation checkpoint
		if err := kl.statusManager.SetPodAllocation(pod); err != nil {
			return nil, err
		}
		for i, container := range pod.Spec.Containers {
			if !apiequality.Semantic.DeepEqual(container.Resources, allocatedPod.Spec.Containers[i].Resources) {
				key := kuberuntime.GetStableKey(pod, &container)
				kl.backOff.Reset(key)
			}
		}
		for i, container := range pod.Spec.InitContainers {
			if podutil.IsRestartableInitContainer(&container) {
				if !apiequality.Semantic.DeepEqual(container.Resources, allocatedPod.Spec.InitContainers[i].Resources) {
					key := kuberuntime.GetStableKey(pod, &container)
					kl.backOff.Reset(key)
				}
			}
		}
		allocatedPod = pod

		// Special case when the updated allocation matches the actual resources. This can occur
		// when reverting a resize that hasn't been actuated, or when making an equivalent change
		// (such as CPU requests below MinShares). This is an optimization to clear the resize
		// status immediately, rather than waiting for the next SyncPod iteration.
		if allocatedResourcesMatchStatus(allocatedPod, podStatus) {
			// In this case, consider the resize complete.
			kl.statusManager.SetPodResizeStatus(pod.UID, "")
			return allocatedPod, nil
		}
	}
	if resizeStatus != "" {
		kl.statusManager.SetPodResizeStatus(pod.UID, resizeStatus)
		if resizeMsg != "" {
			switch resizeStatus {
			case v1.PodResizeStatusDeferred:
				kl.recorder.Eventf(pod, v1.EventTypeWarning, events.ResizeDeferred, resizeMsg)
			case v1.PodResizeStatusInfeasible:
				kl.recorder.Eventf(pod, v1.EventTypeWarning, events.ResizeInfeasible, resizeMsg)
			}
		}
	}
	return allocatedPod, nil
}

// LatestLoopEntryTime returns the last time in the sync loop monitor.
func (kl *Kubelet) LatestLoopEntryTime() time.Time {
	val := kl.syncLoopMonitor.Load()
	if val == nil {
		return time.Time{}
	}
	return val.(time.Time)
}

// SyncLoopHealthCheck checks if kubelet's sync loop that updates containers is working.
func (kl *Kubelet) SyncLoopHealthCheck(req *http.Request) error {
	duration := kl.resyncInterval * 2
	minDuration := time.Minute * 5
	if duration < minDuration {
		duration = minDuration
	}
	enterLoopTime := kl.LatestLoopEntryTime()
	if !enterLoopTime.IsZero() && time.Now().After(enterLoopTime.Add(duration)) {
		return fmt.Errorf("sync Loop took longer than expected")
	}
	return nil
}

// updateRuntimeUp calls the container runtime status callback, initializing
// the runtime dependent modules when the container runtime first comes up,
// and returns an error if the status check fails.  If the status check is OK,
// update the container runtime uptime in the kubelet runtimeState.
func (kl *Kubelet) updateRuntimeUp() {
	kl.updateRuntimeMux.Lock()
	defer kl.updateRuntimeMux.Unlock()
	ctx := context.Background()

	s, err := kl.containerRuntime.Status(ctx)
	if err != nil {
		klog.ErrorS(err, "Container runtime sanity check failed")
		return
	}
	if s == nil {
		klog.ErrorS(nil, "Container runtime status is nil")
		return
	}
	// Periodically log the whole runtime status for debugging.
	klog.V(4).InfoS("Container runtime status", "status", s)
	klogErrorS := klog.ErrorS
	if !kl.containerRuntimeReadyExpected {
		klogErrorS = klog.V(4).ErrorS
	}
	networkReady := s.GetRuntimeCondition(kubecontainer.NetworkReady)
	if networkReady == nil || !networkReady.Status {
		klogErrorS(nil, "Container runtime network not ready", "networkReady", networkReady)
		kl.runtimeState.setNetworkState(fmt.Errorf("container runtime network not ready: %v", networkReady))
	} else {
		// Set nil if the container runtime network is ready.
		kl.runtimeState.setNetworkState(nil)
	}
	// information in RuntimeReady condition will be propagated to NodeReady condition.
	runtimeReady := s.GetRuntimeCondition(kubecontainer.RuntimeReady)
	// If RuntimeReady is not set or is false, report an error.
	if runtimeReady == nil || !runtimeReady.Status {
		klogErrorS(nil, "Container runtime not ready", "runtimeReady", runtimeReady)
		kl.runtimeState.setRuntimeState(fmt.Errorf("container runtime not ready: %v", runtimeReady))
		return
	}

	kl.runtimeState.setRuntimeState(nil)
	kl.runtimeState.setRuntimeHandlers(s.Handlers)
	kl.runtimeState.setRuntimeFeatures(s.Features)
	kl.oneTimeInitializer.Do(kl.initializeRuntimeDependentModules)
	kl.runtimeState.setRuntimeSync(kl.clock.Now())
}

// GetConfiguration returns the KubeletConfiguration used to configure the kubelet.
func (kl *Kubelet) GetConfiguration() kubeletconfiginternal.KubeletConfiguration {
	return kl.kubeletConfiguration
}

// BirthCry sends an event that the kubelet has started up.
func (kl *Kubelet) BirthCry() {
	// Make an event that kubelet restarted.
	kl.recorder.Eventf(kl.nodeRef, v1.EventTypeNormal, events.StartingKubelet, "Starting kubelet.")
}

// ListenAndServe runs the kubelet HTTP server.
func (kl *Kubelet) ListenAndServe(kubeCfg *kubeletconfiginternal.KubeletConfiguration, tlsOptions *server.TLSOptions,
	auth server.AuthInterface, tp trace.TracerProvider) {
	server.ListenAndServeKubeletServer(kl, kl.resourceAnalyzer, kl.containerManager.GetHealthCheckers(), kubeCfg, tlsOptions, auth, tp)
}

// ListenAndServeReadOnly runs the kubelet HTTP server in read-only mode.
func (kl *Kubelet) ListenAndServeReadOnly(address net.IP, port uint, tp trace.TracerProvider) {
	server.ListenAndServeKubeletReadOnlyServer(kl, kl.resourceAnalyzer, kl.containerManager.GetHealthCheckers(), address, port, tp)
}

// ListenAndServePodResources runs the kubelet podresources grpc service
func (kl *Kubelet) ListenAndServePodResources() {
	endpoint, err := util.LocalEndpoint(kl.getPodResourcesDir(), podresources.Socket)
	if err != nil {
		klog.V(2).InfoS("Failed to get local endpoint for PodResources endpoint", "err", err)
		return
	}

	providers := podresources.PodResourcesProviders{
		Pods:             kl.podManager,
		Devices:          kl.containerManager,
		Cpus:             kl.containerManager,
		Memory:           kl.containerManager,
		DynamicResources: kl.containerManager,
	}

	server.ListenAndServePodResources(endpoint, providers)
}

// Delete the eligible dead container instances in a pod. Depending on the configuration, the latest dead containers may be kept around.
func (kl *Kubelet) cleanUpContainersInPod(podID types.UID, exitedContainerID string) {
	if podStatus, err := kl.podCache.Get(podID); err == nil {
		// When an evicted or deleted pod has already synced, all containers can be removed.
		removeAll := kl.podWorkers.ShouldPodContentBeRemoved(podID)
		kl.containerDeletor.deleteContainersInPod(exitedContainerID, podStatus, removeAll)
	}
}

// fastStatusUpdateOnce starts a loop that checks if the current state of kubelet + container runtime
// would be able to turn the node ready, and sync the ready state to the apiserver as soon as possible.
// Function returns after the node status update after such event, or when the node is already ready.
// Function is executed only during Kubelet start which improves latency to ready node by updating
// kubelet state, runtime status and node statuses ASAP.
func (kl *Kubelet) fastStatusUpdateOnce() {
	ctx := context.Background()
	start := kl.clock.Now()
	stopCh := make(chan struct{})

	// Keep trying to make fast node status update until either timeout is reached or an update is successful.
	wait.Until(func() {
		// fastNodeStatusUpdate returns true when it succeeds or when the grace period has expired
		// (status was not updated within nodeReadyGracePeriod and the second argument below gets true),
		// then we close the channel and abort the loop.
		if kl.fastNodeStatusUpdate(ctx, kl.clock.Since(start) >= nodeReadyGracePeriod) {
			close(stopCh)
		}
	}, 100*time.Millisecond, stopCh)
}

// CheckpointContainer tries to checkpoint a container. The parameters are used to
// look up the specified container. If the container specified by the given parameters
// cannot be found an error is returned. If the container is found the container
// engine will be asked to checkpoint the given container into the kubelet's default
// checkpoint directory.
func (kl *Kubelet) CheckpointContainer(
	ctx context.Context,
	podUID types.UID,
	podFullName,
	containerName string,
	options *runtimeapi.CheckpointContainerRequest,
) error {
	container, err := kl.findContainer(ctx, podFullName, podUID, containerName)
	if err != nil {
		return err
	}
	if container == nil {
		return fmt.Errorf("container %v not found", containerName)
	}

	options.Location = filepath.Join(
		kl.getCheckpointsDir(),
		fmt.Sprintf(
			"checkpoint-%s-%s-%s.tar",
			podFullName,
			containerName,
			time.Now().Format(time.RFC3339),
		),
	)

	options.ContainerId = string(container.ID.ID)

	if err := kl.containerRuntime.CheckpointContainer(ctx, options); err != nil {
		return err
	}

	return nil
}

// ListMetricDescriptors gets the descriptors for the metrics that will be returned in ListPodSandboxMetrics.
func (kl *Kubelet) ListMetricDescriptors(ctx context.Context) ([]*runtimeapi.MetricDescriptor, error) {
	return kl.containerRuntime.ListMetricDescriptors(ctx)
}

// ListPodSandboxMetrics retrieves the metrics for all pod sandboxes.
func (kl *Kubelet) ListPodSandboxMetrics(ctx context.Context) ([]*runtimeapi.PodSandboxMetrics, error) {
	return kl.containerRuntime.ListPodSandboxMetrics(ctx)
}

func (kl *Kubelet) supportLocalStorageCapacityIsolation() bool {
	return kl.GetConfiguration().LocalStorageCapacityIsolation
}

// isSyncPodWorthy filters out events that are not worthy of pod syncing
func isSyncPodWorthy(event *pleg.PodLifecycleEvent) bool {
	// ContainerRemoved doesn't affect pod state
	return event.Type != pleg.ContainerRemoved
}

// PrepareDynamicResources calls the container Manager PrepareDynamicResources API
// This method implements the RuntimeHelper interface
func (kl *Kubelet) PrepareDynamicResources(ctx context.Context, pod *v1.Pod) error {
	return kl.containerManager.PrepareDynamicResources(ctx, pod)
}

// UnprepareDynamicResources calls the container Manager UnprepareDynamicResources API
// This method implements the RuntimeHelper interface
func (kl *Kubelet) UnprepareDynamicResources(ctx context.Context, pod *v1.Pod) error {
	return kl.containerManager.UnprepareDynamicResources(ctx, pod)
}

// Ensure Mirror Pod for Static Pod exists and matches the current pod definition.
// The function logs and ignores any errors.
func (kl *Kubelet) tryReconcileMirrorPods(staticPod, mirrorPod *v1.Pod) {
	if !kubetypes.IsStaticPod(staticPod) {
		return
	}
	deleted := false
	if mirrorPod != nil {
		if mirrorPod.DeletionTimestamp != nil || !kubepod.IsMirrorPodOf(mirrorPod, staticPod) {
			// The mirror pod is semantically different from the static pod. Remove
			// it. The mirror pod will get recreated later.
			klog.InfoS("Trying to delete pod", "pod", klog.KObj(mirrorPod), "podUID", mirrorPod.ObjectMeta.UID)
			podFullName := kubecontainer.GetPodFullName(staticPod)
			if ok, err := kl.mirrorPodClient.DeleteMirrorPod(podFullName, &mirrorPod.ObjectMeta.UID); err != nil {
				klog.ErrorS(err, "Failed deleting mirror pod", "pod", klog.KObj(mirrorPod))
			} else if ok {
				deleted = ok
				klog.InfoS("Deleted mirror pod as it didn't match the static Pod", "pod", klog.KObj(mirrorPod))
			}
		}
	}
	if mirrorPod == nil || deleted {
		node, err := kl.GetNode()
		if err != nil {
			klog.ErrorS(err, "No need to create a mirror pod, since failed to get node info from the cluster", "node", klog.KRef("", string(kl.nodeName)))
		} else if node.DeletionTimestamp != nil {
			klog.InfoS("No need to create a mirror pod, since node has been removed from the cluster", "node", klog.KRef("", string(kl.nodeName)))
		} else {
			klog.InfoS("Creating a mirror pod for static pod", "pod", klog.KObj(staticPod))
			if err := kl.mirrorPodClient.CreateMirrorPod(staticPod); err != nil {
				klog.ErrorS(err, "Failed creating a mirror pod", "pod", klog.KObj(staticPod))
			}
		}
	}
}

// Ensure Mirror Pod for Static Pod exists as soon as node is registered.
func (kl *Kubelet) fastStaticPodsRegistration(ctx context.Context) {
	if err := wait.PollUntilContextCancel(ctx, 100*time.Millisecond, true, func(ctx context.Context) (bool, error) {
		_, err := kl.GetNode()
		if err == nil {
			return true, nil
		}

		klog.V(4).ErrorS(err, "Unable to register mirror pod because node is not registered yet", "node", klog.KRef("", string(kl.nodeName)))
		return false, nil
	}); err != nil {
		klog.V(4).ErrorS(err, "Failed to wait until node is registered", "node", klog.KRef("", string(kl.nodeName)))
	}

	staticPodToMirrorPodMap := kl.podManager.GetStaticPodToMirrorPodMap()
	for staticPod, mirrorPod := range staticPodToMirrorPodMap {
		kl.tryReconcileMirrorPods(staticPod, mirrorPod)
	}
}

func (kl *Kubelet) SetPodWatchCondition(podUID types.UID, conditionKey string, condition pleg.WatchCondition) {
	kl.pleg.SetPodWatchCondition(podUID, conditionKey, condition)
}
