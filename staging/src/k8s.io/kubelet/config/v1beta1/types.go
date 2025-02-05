/*
Copyright 2017 The Kubernetes Authors.

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

package v1beta1

import (
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	logsapi "k8s.io/component-base/logs/api/v1"
	tracingapi "k8s.io/component-base/tracing/api/v1"
	v1alphav1 "k8s.io/kubelet/config/v1alpha1"
)

// HairpinMode denotes how the kubelet should configure networking to handle
// hairpin packets.
type HairpinMode string

// Enum settings for different ways to handle hairpin packets.
const (
	// Set the hairpin flag on the veth of containers in the respective
	// container runtime.
	HairpinVeth = "hairpin-veth"
	// Make the container bridge promiscuous. This will force it to accept
	// hairpin packets, even if the flag isn't set on ports of the bridge.
	PromiscuousBridge = "promiscuous-bridge"
	// Neither of the above. If the kubelet is started in this hairpin mode
	// and kube-proxy is running in iptables mode, hairpin packets will be
	// dropped by the container bridge.
	HairpinNone = "none"
)

// ResourceChangeDetectionStrategy denotes a mode in which internal
// managers (secret, configmap) are discovering object changes.
type ResourceChangeDetectionStrategy string

// Enum settings for different strategies of kubelet managers.
const (
	// GetChangeDetectionStrategy is a mode in which kubelet fetches
	// necessary objects directly from apiserver.
	GetChangeDetectionStrategy ResourceChangeDetectionStrategy = "Get"
	// TTLCacheChangeDetectionStrategy is a mode in which kubelet uses
	// ttl cache for object directly fetched from apiserver.
	TTLCacheChangeDetectionStrategy ResourceChangeDetectionStrategy = "Cache"
	// WatchChangeDetectionStrategy is a mode in which kubelet uses
	// watches to observe changes to objects that are in its interest.
	WatchChangeDetectionStrategy ResourceChangeDetectionStrategy = "Watch"
	// RestrictedTopologyManagerPolicy is a mode in which kubelet only allows
	// pods with optimal NUMA node alignment for requested resources
	RestrictedTopologyManagerPolicy = "restricted"
	// BestEffortTopologyManagerPolicy is a mode in which kubelet will favour
	// pods with NUMA alignment of CPU and device resources.
	BestEffortTopologyManagerPolicy = "best-effort"
	// NoneTopologyManagerPolicy is a mode in which kubelet has no knowledge
	// of NUMA alignment of a pod's CPU and device resources.
	NoneTopologyManagerPolicy = "none"
	// SingleNumaNodeTopologyManagerPolicy is a mode in which kubelet only allows
	// pods with a single NUMA alignment of CPU and device resources.
	SingleNumaNodeTopologyManagerPolicy = "single-numa-node"
	// ContainerTopologyManagerScope represents that
	// topology policy is applied on a per-container basis.
	ContainerTopologyManagerScope = "container"
	// PodTopologyManagerScope represents that
	// topology policy is applied on a per-pod basis.
	PodTopologyManagerScope = "pod"
	// NoneMemoryManagerPolicy is a memory manager none policy, under the none policy
	// the memory manager will not pin containers memory of guaranteed pods
	NoneMemoryManagerPolicy = "None"
	// StaticMemoryManagerPolicy is a memory manager static policy, under the static policy
	// the memory manager will try to pin containers memory of guaranteed pods to the smallest
	// possible sub-set of NUMA nodes
	StaticMemoryManagerPolicy = "Static"
)

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// KubeletConfiguration contains the configuration for the Kubelet
type KubeletConfiguration struct {
	metav1.TypeMeta `json:",inline"`

	// enableServer enables Kubelet's secured server.
	// Note: Kubelet's insecure port is controlled by the readOnlyPort option.
	// Default: true
	EnableServer *bool `json:"enableServer,omitempty"`
	// staticPodPath is the path to the directory containing local (static) pods to
	// run, or the path to a single static pod file.
	// Default: ""
	// +optional
	StaticPodPath string `json:"staticPodPath,omitempty"`
	// podLogsDir is a custom root directory path kubelet will use to place pod's log files.
	// Default: "/var/log/pods/"
	// Note: it is not recommended to use the temp folder as a log directory as it may cause
	// unexpected behavior in many places.
	// +optional
	PodLogsDir string `json:"podLogsDir,omitempty"`
	// syncFrequency is the max period between synchronizing running
	// containers and config.
	// Default: "1m"
	// +optional
	SyncFrequency metav1.Duration `json:"syncFrequency,omitempty"`
	// fileCheckFrequency is the duration between checking config files for
	// new data.
	// Default: "20s"
	// +optional
	FileCheckFrequency metav1.Duration `json:"fileCheckFrequency,omitempty"`
	// httpCheckFrequency is the duration between checking http for new data.
	// Default: "20s"
	// +optional
	HTTPCheckFrequency metav1.Duration `json:"httpCheckFrequency,omitempty"`
	// staticPodURL is the URL for accessing static pods to run.
	// Default: ""
	// +optional
	StaticPodURL string `json:"staticPodURL,omitempty"`
	// staticPodURLHeader is a map of slices with HTTP headers to use when accessing the podURL.
	// Default: nil
	// +optional
	StaticPodURLHeader map[string][]string `json:"staticPodURLHeader,omitempty"`
	// address is the IP address for the Kubelet to serve on (set to 0.0.0.0
	// for all interfaces).
	// Default: "0.0.0.0"
	// +optional
	Address string `json:"address,omitempty"`
	// port is the port for the Kubelet to serve on.
	// The port number must be between 1 and 65535, inclusive.
	// Default: 10250
	// +optional
	Port int32 `json:"port,omitempty"`
	// readOnlyPort is the read-only port for the Kubelet to serve on with
	// no authentication/authorization.
	// The port number must be between 1 and 65535, inclusive.
	// Setting this field to 0 disables the read-only service.
	// Default: 0 (disabled)
	// +optional
	ReadOnlyPort int32 `json:"readOnlyPort,omitempty"`
	// tlsCertFile is the file containing x509 Certificate for HTTPS. (CA cert,
	// if any, concatenated after server cert). If tlsCertFile and
	// tlsPrivateKeyFile are not provided, a self-signed certificate
	// and key are generated for the public address and saved to the directory
	// passed to the Kubelet's --cert-dir flag.
	// Default: ""
	// +optional
	TLSCertFile string `json:"tlsCertFile,omitempty"`
	// tlsPrivateKeyFile is the file containing x509 private key matching tlsCertFile.
	// Default: ""
	// +optional
	TLSPrivateKeyFile string `json:"tlsPrivateKeyFile,omitempty"`
	// tlsCipherSuites is the list of allowed cipher suites for the server.
	// Note that TLS 1.3 ciphersuites are not configurable.
	// Values are from tls package constants (https://golang.org/pkg/crypto/tls/#pkg-constants).
	// Default: nil
	// +optional
	TLSCipherSuites []string `json:"tlsCipherSuites,omitempty"`
	// tlsMinVersion is the minimum TLS version supported.
	// Values are from tls package constants (https://golang.org/pkg/crypto/tls/#pkg-constants).
	// Default: ""
	// +optional
	TLSMinVersion string `json:"tlsMinVersion,omitempty"`
	// rotateCertificates enables client certificate rotation. The Kubelet will request a
	// new certificate from the certificates.k8s.io API. This requires an approver to approve the
	// certificate signing requests.
	// Default: false
	// +optional
	RotateCertificates bool `json:"rotateCertificates,omitempty"`
	// serverTLSBootstrap enables server certificate bootstrap. Instead of self
	// signing a serving certificate, the Kubelet will request a certificate from
	// the 'certificates.k8s.io' API. This requires an approver to approve the
	// certificate signing requests (CSR). The RotateKubeletServerCertificate feature
	// must be enabled when setting this field.
	// Default: false
	// +optional
	ServerTLSBootstrap bool `json:"serverTLSBootstrap,omitempty"`
	// authentication specifies how requests to the Kubelet's server are authenticated.
	// Defaults:
	//   anonymous:
	//     enabled: false
	//   webhook:
	//     enabled: true
	//     cacheTTL: "2m"
	// +optional
	Authentication KubeletAuthentication `json:"authentication"`
	// authorization specifies how requests to the Kubelet's server are authorized.
	// Defaults:
	//   mode: Webhook
	//   webhook:
	//     cacheAuthorizedTTL: "5m"
	//     cacheUnauthorizedTTL: "30s"
	// +optional
	Authorization KubeletAuthorization `json:"authorization"`
	// registryPullQPS is the limit of registry pulls per second.
	// The value must not be a negative number.
	// Setting it to 0 means no limit.
	// Default: 5
	// +optional
	RegistryPullQPS *int32 `json:"registryPullQPS,omitempty"`
	// registryBurst is the maximum size of bursty pulls, temporarily allows
	// pulls to burst to this number, while still not exceeding registryPullQPS.
	// The value must not be a negative number.
	// Only used if registryPullQPS is greater than 0.
	// Default: 10
	// +optional
	RegistryBurst int32 `json:"registryBurst,omitempty"`
	// imagePullCredentialsVerificationPolicy determines how credentials should be
	// verified when pod requests an image that is already present on the node:
	//   - NeverVerify
	//       - anyone on a node can use any image present on the node
	//   - NeverVerifyPreloadedImages
	//       - images that were pulled to the node by something else than the kubelet
	//         can be used without reverifying pull credentials
	//   - NeverVerifyAllowlistedImages
	//       - like "NeverVerifyPreloadedImages" but only node images from
	//         `preloadedImagesVerificationAllowlist` don't require reverification
	//   - AlwaysVerify
	//       - all images require credential reverification
	// +optional
	ImagePullCredentialsVerificationPolicy v1alphav1.ImagePullCredentialsVerificationPolicy `json:"imagePullCredentialsVerificationPolicy,omitempty"`
	// preloadedImagesVerificationAllowlist specifies a list of images that are
	// exempted from credential reverification for the "NeverVerifyAllowlistedImages"
	// `imagePullCredentialsVerificationPolicy`.
	// The list accepts a full path segment wildcard suffix "/*".
	// Only use image specs without an image tag or digest.
	// +optional
	PreloadedImagesVerificationAllowlist []string `json:"preloadedImagesVerificationAllowlist,omitempty"`
	// eventRecordQPS is the maximum event creations per second. If 0, there
	// is no limit enforced. The value cannot be a negative number.
	// Default: 50
	// +optional
	EventRecordQPS *int32 `json:"eventRecordQPS,omitempty"`
	// eventBurst is the maximum size of a burst of event creations, temporarily
	// allows event creations to burst to this number, while still not exceeding
	// eventRecordQPS. This field canot be a negative number and it is only used
	// when eventRecordQPS > 0.
	// Default: 100
	// +optional
	EventBurst int32 `json:"eventBurst,omitempty"`
	// enableDebuggingHandlers enables server endpoints for log access
	// and local running of containers and commands, including the exec,
	// attach, logs, and portforward features.
	// Default: true
	// +optional
	EnableDebuggingHandlers *bool `json:"enableDebuggingHandlers,omitempty"`
	// enableContentionProfiling enables block profiling, if enableDebuggingHandlers is true.
	// Default: false
	// +optional
	EnableContentionProfiling bool `json:"enableContentionProfiling,omitempty"`
	// healthzPort is the port of the localhost healthz endpoint (set to 0 to disable).
	// A valid number is between 1 and 65535.
	// Default: 10248
	// +optional
	HealthzPort *int32 `json:"healthzPort,omitempty"`
	// healthzBindAddress is the IP address for the healthz server to serve on.
	// Default: "127.0.0.1"
	// +optional
	HealthzBindAddress string `json:"healthzBindAddress,omitempty"`
	// oomScoreAdj is The oom-score-adj value for kubelet process. Values
	// must be within the range [-1000, 1000].
	// Default: -999
	// +optional
	OOMScoreAdj *int32 `json:"oomScoreAdj,omitempty"`
	// clusterDomain is the DNS domain for this cluster. If set, kubelet will
	// configure all containers to search this domain in addition to the
	// host's search domains.
	// Default: ""
	// +optional
	ClusterDomain string `json:"clusterDomain,omitempty"`
	// clusterDNS is a list of IP addresses for the cluster DNS server. If set,
	// kubelet will configure all containers to use this for DNS resolution
	// instead of the host's DNS servers.
	// Default: nil
	// +optional
	ClusterDNS []string `json:"clusterDNS,omitempty"`
	// streamingConnectionIdleTimeout is the maximum time a streaming connection
	// can be idle before the connection is automatically closed.
	// Default: "4h"
	// +optional
	StreamingConnectionIdleTimeout metav1.Duration `json:"streamingConnectionIdleTimeout,omitempty"`
	// nodeStatusUpdateFrequency is the frequency that kubelet computes node
	// status. If node lease feature is not enabled, it is also the frequency that
	// kubelet posts node status to master.
	// Note: When node lease feature is not enabled, be cautious when changing the
	// constant, it must work with nodeMonitorGracePeriod in nodecontroller.
	// Default: "10s"
	// +optional
	NodeStatusUpdateFrequency metav1.Duration `json:"nodeStatusUpdateFrequency,omitempty"`
	// nodeStatusReportFrequency is the frequency that kubelet posts node
	// status to master if node status does not change. Kubelet will ignore this
	// frequency and post node status immediately if any change is detected. It is
	// only used when node lease feature is enabled. nodeStatusReportFrequency's
	// default value is 5m. But if nodeStatusUpdateFrequency is set explicitly,
	// nodeStatusReportFrequency's default value will be set to
	// nodeStatusUpdateFrequency for backward compatibility.
	// Default: "5m"
	// +optional
	NodeStatusReportFrequency metav1.Duration `json:"nodeStatusReportFrequency,omitempty"`
	// nodeLeaseDurationSeconds is the duration the Kubelet will set on its corresponding Lease.
	// NodeLease provides an indicator of node health by having the Kubelet create and
	// periodically renew a lease, named after the node, in the kube-node-lease namespace.
	// If the lease expires, the node can be considered unhealthy.
	// The lease is currently renewed every 10s, per KEP-0009. In the future, the lease renewal
	// interval may be set based on the lease duration.
	// The field value must be greater than 0.
	// Default: 40
	// +optional
	NodeLeaseDurationSeconds int32 `json:"nodeLeaseDurationSeconds,omitempty"`
	// imageMinimumGCAge is the minimum age for an unused image before it is
	// garbage collected.
	// Default: "2m"
	// +optional
	ImageMinimumGCAge metav1.Duration `json:"imageMinimumGCAge,omitempty"`
	// imageMaximumGCAge is the maximum age an image can be unused before it is garbage collected.
	// The default of this field is "0s", which disables this field--meaning images won't be garbage
	// collected based on being unused for too long.
	// Default: "0s" (disabled)
	// +optional
	ImageMaximumGCAge metav1.Duration `json:"imageMaximumGCAge,omitempty"`
	// imageGCHighThresholdPercent is the percent of disk usage after which
	// image garbage collection is always run. The percent is calculated by
	// dividing this field value by 100, so this field must be between 0 and
	// 100, inclusive. When specified, the value must be greater than
	// imageGCLowThresholdPercent.
	// Default: 85
	// +optional
	ImageGCHighThresholdPercent *int32 `json:"imageGCHighThresholdPercent,omitempty"`
	// imageGCLowThresholdPercent is the percent of disk usage before which
	// image garbage collection is never run. Lowest disk usage to garbage
	// collect to. The percent is calculated by dividing this field value by 100,
	// so the field value must be between 0 and 100, inclusive. When specified, the
	// value must be less than imageGCHighThresholdPercent.
	// Default: 80
	// +optional
	ImageGCLowThresholdPercent *int32 `json:"imageGCLowThresholdPercent,omitempty"`
	// volumeStatsAggPeriod is the frequency for calculating and caching volume
	// disk usage for all pods.
	// Default: "1m"
	// +optional
	VolumeStatsAggPeriod metav1.Duration `json:"volumeStatsAggPeriod,omitempty"`
	// kubeletCgroups is the absolute name of cgroups to isolate the kubelet in
	// Default: ""
	// +optional
	KubeletCgroups string `json:"kubeletCgroups,omitempty"`
	// systemCgroups is absolute name of cgroups in which to place
	// all non-kernel processes that are not already in a container. Empty
	// for no container. Rolling back the flag requires a reboot.
	// The cgroupRoot must be specified if this field is not empty.
	// Default: ""
	// +optional
	SystemCgroups string `json:"systemCgroups,omitempty"`
	// cgroupRoot is the root cgroup to use for pods. This is handled by the
	// container runtime on a best effort basis.
	// +optional
	CgroupRoot string `json:"cgroupRoot,omitempty"`
	// cgroupsPerQOS enable QoS based CGroup hierarchy: top level CGroups for QoS classes
	// and all Burstable and BestEffort Pods are brought up under their specific top level
	// QoS CGroup.
	// Default: true
	// +optional
	CgroupsPerQOS *bool `json:"cgroupsPerQOS,omitempty"`
	// cgroupDriver is the driver kubelet uses to manipulate CGroups on the host (cgroupfs
	// or systemd).
	// Default: "cgroupfs"
	// +optional
	CgroupDriver string `json:"cgroupDriver,omitempty"`
	// cpuManagerPolicy is the name of the policy to use.
	// Requires the CPUManager feature gate to be enabled.
	// Default: "None"
	// +optional
	CPUManagerPolicy string `json:"cpuManagerPolicy,omitempty"`
	// singleProcessOOMKill, if true, will prevent the `memory.oom.group` flag from being set for container
	// cgroups in cgroups v2. This causes processes in the container to be OOM killed individually instead of as
	// a group. It means that if true, the behavior aligns with the behavior of cgroups v1.
	// The default value is determined automatically when you don't specify.
	// On non-linux such as windows, only null / absent is allowed.
	// On cgroup v1 linux, only null / absent and true are allowed.
	// On cgroup v2 linux, null / absent, true and false are allowed. The default value is false.
	// +optional
	SingleProcessOOMKill *bool `json:"singleProcessOOMKill,omitempty"`
	// cpuManagerPolicyOptions is a set of key=value which 	allows to set extra options
	// to fine tune the behaviour of the cpu manager policies.
	// Requires  both the "CPUManager" and "CPUManagerPolicyOptions" feature gates to be enabled.
	// Default: nil
	// +optional
	CPUManagerPolicyOptions map[string]string `json:"cpuManagerPolicyOptions,omitempty"`
	// cpuManagerReconcilePeriod is the reconciliation period for the CPU Manager.
	// Requires the CPUManager feature gate to be enabled.
	// Default: "10s"
	// +optional
	CPUManagerReconcilePeriod metav1.Duration `json:"cpuManagerReconcilePeriod,omitempty"`
	// memoryManagerPolicy is the name of the policy to use by memory manager.
	// Requires the MemoryManager feature gate to be enabled.
	// Default: "none"
	// +optional
	MemoryManagerPolicy string `json:"memoryManagerPolicy,omitempty"`
	// topologyManagerPolicy is the name of the topology manager policy to use.
	// Valid values include:
	//
	// - `restricted`: kubelet only allows pods with optimal NUMA node alignment for
	//   requested resources;
	// - `best-effort`: kubelet will favor pods with NUMA alignment of CPU and device
	//   resources;
	// - `none`: kubelet has no knowledge of NUMA alignment of a pod's CPU and device resources.
	// - `single-numa-node`: kubelet only allows pods with a single NUMA alignment
	//   of CPU and device resources.
	//
	// Default: "none"
	// +optional
	TopologyManagerPolicy string `json:"topologyManagerPolicy,omitempty"`
	// topologyManagerScope represents the scope of topology hint generation
	// that topology manager requests and hint providers generate. Valid values include:
	//
	// - `container`: topology policy is applied on a per-container basis.
	// - `pod`: topology policy is applied on a per-pod basis.
	//
	// Default: "container"
	// +optional
	TopologyManagerScope string `json:"topologyManagerScope,omitempty"`
	// TopologyManagerPolicyOptions is a set of key=value which allows to set extra options
	// to fine tune the behaviour of the topology manager policies.
	// Requires  both the "TopologyManager" and "TopologyManagerPolicyOptions" feature gates to be enabled.
	// Default: nil
	// +optional
	TopologyManagerPolicyOptions map[string]string `json:"topologyManagerPolicyOptions,omitempty"`
	// qosReserved is a set of resource name to percentage pairs that specify
	// the minimum percentage of a resource reserved for exclusive use by the
	// guaranteed QoS tier.
	// Currently supported resources: "memory"
	// Requires the QOSReserved feature gate to be enabled.
	// Default: nil
	// +optional
	QOSReserved map[string]string `json:"qosReserved,omitempty"`
	// runtimeRequestTimeout is the timeout for all runtime requests except long running
	// requests - pull, logs, exec and attach.
	// Default: "2m"
	// +optional
	RuntimeRequestTimeout metav1.Duration `json:"runtimeRequestTimeout,omitempty"`
	// hairpinMode specifies how the Kubelet should configure the container
	// bridge for hairpin packets.
	// Setting this flag allows endpoints in a Service to loadbalance back to
	// themselves if they should try to access their own Service. Values:
	//
	// - "promiscuous-bridge": make the container bridge promiscuous.
	// - "hairpin-veth":       set the hairpin flag on container veth interfaces.
	// - "none":               do nothing.
	//
	// Generally, one must set `--hairpin-mode=hairpin-veth to` achieve hairpin NAT,
	// because promiscuous-bridge assumes the existence of a container bridge named cbr0.
	// Default: "promiscuous-bridge"
	// +optional
	HairpinMode string `json:"hairpinMode,omitempty"`
	// maxPods is the maximum number of Pods that can run on this Kubelet.
	// The value must be a non-negative integer.
	// Default: 110
	// +optional
	MaxPods int32 `json:"maxPods,omitempty"`
	// podCIDR is the CIDR to use for pod IP addresses, only used in standalone mode.
	// In cluster mode, this is obtained from the control plane.
	// Default: ""
	// +optional
	PodCIDR string `json:"podCIDR,omitempty"`
	// podPidsLimit is the maximum number of PIDs in any pod.
	// Default: -1
	// +optional
	PodPidsLimit *int64 `json:"podPidsLimit,omitempty"`
	// resolvConf is the resolver configuration file used as the basis
	// for the container DNS resolution configuration.
	// If set to the empty string, will override the default and effectively disable DNS lookups.
	// Default: "/etc/resolv.conf"
	// +optional
	ResolverConfig *string `json:"resolvConf,omitempty"`
	// runOnce causes the Kubelet to check the API server once for pods,
	// run those in addition to the pods specified by static pod files, and exit.
	// Default: false
	// +optional
	RunOnce bool `json:"runOnce,omitempty"`
	// cpuCFSQuota enables CPU CFS quota enforcement for containers that
	// specify CPU limits.
	// Default: true
	// +optional
	CPUCFSQuota *bool `json:"cpuCFSQuota,omitempty"`
	// cpuCFSQuotaPeriod is the CPU CFS quota period value, `cpu.cfs_period_us`.
	// The value must be between 1 ms and 1 second, inclusive.
	// Requires the CustomCPUCFSQuotaPeriod feature gate to be enabled.
	// Default: "100ms"
	// +optional
	CPUCFSQuotaPeriod *metav1.Duration `json:"cpuCFSQuotaPeriod,omitempty"`
	// nodeStatusMaxImages caps the number of images reported in Node.status.images.
	// The value must be greater than -2.
	// Note: If -1 is specified, no cap will be applied. If 0 is specified, no image is returned.
	// Default: 50
	// +optional
	NodeStatusMaxImages *int32 `json:"nodeStatusMaxImages,omitempty"`
	// maxOpenFiles is Number of files that can be opened by Kubelet process.
	// The value must be a non-negative number.
	// Default: 1000000
	// +optional
	MaxOpenFiles int64 `json:"maxOpenFiles,omitempty"`
	// contentType is contentType of requests sent to apiserver.
	// Default: "application/vnd.kubernetes.protobuf"
	// +optional
	ContentType string `json:"contentType,omitempty"`
	// kubeAPIQPS is the QPS to use while talking with kubernetes apiserver.
	// Default: 50
	// +optional
	KubeAPIQPS *int32 `json:"kubeAPIQPS,omitempty"`
	// kubeAPIBurst is the burst to allow while talking with kubernetes API server.
	// This field cannot be a negative number.
	// Default: 100
	// +optional
	KubeAPIBurst int32 `json:"kubeAPIBurst,omitempty"`
	// serializeImagePulls when enabled, tells the Kubelet to pull images one
	// at a time. We recommend *not* changing the default value on nodes that
	// run docker daemon with version  < 1.9 or an Aufs storage backend.
	// Issue #10959 has more details.
	// Default: true
	// +optional
	SerializeImagePulls *bool `json:"serializeImagePulls,omitempty"`
	// MaxParallelImagePulls sets the maximum number of image pulls in parallel.
	// This field cannot be set if SerializeImagePulls is true.
	// Setting it to nil means no limit.
	// Default: nil
	// +optional
	MaxParallelImagePulls *int32 `json:"maxParallelImagePulls,omitempty"`
	// evictionHard is a map of signal names to quantities that defines hard eviction
	// thresholds. For example: `{"memory.available": "300Mi"}`.
	// To explicitly disable, pass a 0% or 100% threshold on an arbitrary resource.
	// Default:
	//   memory.available:  "100Mi"
	//   nodefs.available:  "10%"
	//   nodefs.inodesFree: "5%"
	//   imagefs.available: "15%"
	// +optional
	EvictionHard map[string]string `json:"evictionHard,omitempty"`
	// evictionSoft is a map of signal names to quantities that defines soft eviction thresholds.
	// For example: `{"memory.available": "300Mi"}`.
	// Default: nil
	// +optional
	EvictionSoft map[string]string `json:"evictionSoft,omitempty"`
	// evictionSoftGracePeriod is a map of signal names to quantities that defines grace
	// periods for each soft eviction signal. For example: `{"memory.available": "30s"}`.
	// Default: nil
	// +optional
	EvictionSoftGracePeriod map[string]string `json:"evictionSoftGracePeriod,omitempty"`
	// evictionPressureTransitionPeriod is the duration for which the kubelet has to wait
	// before transitioning out of an eviction pressure condition.
	// A duration of 0s will be converted to the default value of 5m
	// Default: "5m"
	// +optional
	EvictionPressureTransitionPeriod metav1.Duration `json:"evictionPressureTransitionPeriod,omitempty"`
	// evictionMaxPodGracePeriod is the maximum allowed grace period (in seconds) to use
	// when terminating pods in response to a soft eviction threshold being met. This value
	// effectively caps the Pod's terminationGracePeriodSeconds value during soft evictions.
	// Default: 0
	// +optional
	EvictionMaxPodGracePeriod int32 `json:"evictionMaxPodGracePeriod,omitempty"`
	// evictionMinimumReclaim is a map of signal names to quantities that defines minimum reclaims,
	// which describe the minimum amount of a given resource the kubelet will reclaim when
	// performing a pod eviction while that resource is under pressure.
	// For example: `{"imagefs.available": "2Gi"}`.
	// Default: nil
	// +optional
	EvictionMinimumReclaim map[string]string `json:"evictionMinimumReclaim,omitempty"`
	// podsPerCore is the maximum number of pods per core. Cannot exceed maxPods.
	// The value must be a non-negative integer.
	// If 0, there is no limit on the number of Pods.
	// Default: 0
	// +optional
	PodsPerCore int32 `json:"podsPerCore,omitempty"`
	// enableControllerAttachDetach enables the Attach/Detach controller to
	// manage attachment/detachment of volumes scheduled to this node, and
	// disables kubelet from executing any attach/detach operations.
	// Note: attaching/detaching CSI volumes is not supported by the kubelet,
	// so this option needs to be true for that use case.
	// Default: true
	// +optional
	EnableControllerAttachDetach *bool `json:"enableControllerAttachDetach,omitempty"`
	// protectKernelDefaults, if true, causes the Kubelet to error if kernel
	// flags are not as it expects. Otherwise the Kubelet will attempt to modify
	// kernel flags to match its expectation.
	// Default: false
	// +optional
	ProtectKernelDefaults bool `json:"protectKernelDefaults,omitempty"`
	// makeIPTablesUtilChains, if true, causes the Kubelet to create the
	// KUBE-IPTABLES-HINT chain in iptables as a hint to other components about the
	// configuration of iptables on the system.
	// Default: true
	// +optional
	MakeIPTablesUtilChains *bool `json:"makeIPTablesUtilChains,omitempty"`
	// iptablesMasqueradeBit formerly controlled the creation of the KUBE-MARK-MASQ
	// chain.
	// Deprecated: no longer has any effect.
	// Default: 14
	// +optional
	IPTablesMasqueradeBit *int32 `json:"iptablesMasqueradeBit,omitempty"`
	// iptablesDropBit formerly controlled the creation of the KUBE-MARK-DROP chain.
	// Deprecated: no longer has any effect.
	// Default: 15
	// +optional
	IPTablesDropBit *int32 `json:"iptablesDropBit,omitempty"`
	// featureGates is a map of feature names to bools that enable or disable experimental
	// features. This field modifies piecemeal the built-in default values from
	// "k8s.io/kubernetes/pkg/features/kube_features.go".
	// Default: nil
	// +optional
	FeatureGates map[string]bool `json:"featureGates,omitempty"`
	// failSwapOn tells the Kubelet to fail to start if swap is enabled on the node.
	// Default: true
	// +optional
	FailSwapOn *bool `json:"failSwapOn,omitempty"`
	// memorySwap configures swap memory available to container workloads.
	// +featureGate=NodeSwap
	// +optional
	MemorySwap MemorySwapConfiguration `json:"memorySwap,omitempty"`
	// containerLogMaxSize is a quantity defining the maximum size of the container log
	// file before it is rotated. For example: "5Mi" or "256Ki".
	// Default: "10Mi"
	// +optional
	ContainerLogMaxSize string `json:"containerLogMaxSize,omitempty"`
	// containerLogMaxFiles specifies the maximum number of container log files that can
	// be present for a container.
	// Default: 5
	// +optional
	ContainerLogMaxFiles *int32 `json:"containerLogMaxFiles,omitempty"`

	// ContainerLogMaxWorkers specifies the maximum number of concurrent workers to spawn
	// for performing the log rotate operations. Set this count to 1 for disabling the
	// concurrent log rotation workflows
	// Default: 1
	// +optional
	ContainerLogMaxWorkers *int32 `json:"containerLogMaxWorkers,omitempty"`

	// ContainerLogMonitorInterval specifies the duration at which the container logs are monitored
	// for performing the log rotate operation. This defaults to 10 * time.Seconds. But can be
	// customized to a smaller value based on the log generation rate and the size required to be
	// rotated against
	// Default: 10s
	// +optional
	ContainerLogMonitorInterval *metav1.Duration `json:"containerLogMonitorInterval,omitempty"`
	// configMapAndSecretChangeDetectionStrategy is a mode in which ConfigMap and Secret
	// managers are running. Valid values include:
	//
	// - `Get`: kubelet fetches necessary objects directly from the API server;
	// - `Cache`: kubelet uses TTL cache for object fetched from the API server;
	// - `Watch`: kubelet uses watches to observe changes to objects that are in its interest.
	//
	// Default: "Watch"
	// +optional
	ConfigMapAndSecretChangeDetectionStrategy ResourceChangeDetectionStrategy `json:"configMapAndSecretChangeDetectionStrategy,omitempty"`

	/* the following fields are meant for Node Allocatable */

	// systemReserved is a set of ResourceName=ResourceQuantity (e.g. cpu=200m,memory=150G)
	// pairs that describe resources reserved for non-kubernetes components.
	// Currently only cpu and memory are supported.
	// See https://kubernetes.io/docs/tasks/administer-cluster/reserve-compute-resources for more detail.
	// Default: nil
	// +optional
	SystemReserved map[string]string `json:"systemReserved,omitempty"`
	// kubeReserved is a set of ResourceName=ResourceQuantity (e.g. cpu=200m,memory=150G) pairs
	// that describe resources reserved for kubernetes system components.
	// Currently cpu, memory and local storage for root file system are supported.
	// See https://kubernetes.io/docs/tasks/administer-cluster/reserve-compute-resources
	// for more details.
	// Default: nil
	// +optional
	KubeReserved map[string]string `json:"kubeReserved,omitempty"`
	// The reservedSystemCPUs option specifies the CPU list reserved for the host
	// level system threads and kubernetes related threads. This provide a "static"
	// CPU list rather than the "dynamic" list by systemReserved and kubeReserved.
	// This option does not support systemReservedCgroup or kubeReservedCgroup.
	ReservedSystemCPUs string `json:"reservedSystemCPUs,omitempty"`
	// showHiddenMetricsForVersion is the previous version for which you want to show
	// hidden metrics.
	// Only the previous minor version is meaningful, other values will not be allowed.
	// The format is `<major>.<minor>`, e.g.: `1.16`.
	// The purpose of this format is make sure you have the opportunity to notice
	// if the next release hides additional metrics, rather than being surprised
	// when they are permanently removed in the release after that.
	// Default: ""
	// +optional
	ShowHiddenMetricsForVersion string `json:"showHiddenMetricsForVersion,omitempty"`
	// systemReservedCgroup helps the kubelet identify absolute name of top level CGroup used
	// to enforce `systemReserved` compute resource reservation for OS system daemons.
	// Refer to [Node Allocatable](https://kubernetes.io/docs/tasks/administer-cluster/reserve-compute-resources/#node-allocatable)
	// doc for more information.
	// Default: ""
	// +optional
	SystemReservedCgroup string `json:"systemReservedCgroup,omitempty"`
	// kubeReservedCgroup helps the kubelet identify absolute name of top level CGroup used
	// to enforce `KubeReserved` compute resource reservation for Kubernetes node system daemons.
	// Refer to [Node Allocatable](https://kubernetes.io/docs/tasks/administer-cluster/reserve-compute-resources/#node-allocatable)
	// doc for more information.
	// Default: ""
	// +optional
	KubeReservedCgroup string `json:"kubeReservedCgroup,omitempty"`
	// This flag specifies the various Node Allocatable enforcements that Kubelet needs to perform.
	// This flag accepts a list of options. Acceptable options are `none`, `pods`,
	// `system-reserved` and `kube-reserved`.
	// If `none` is specified, no other options may be specified.
	// When `system-reserved` is in the list, systemReservedCgroup must be specified.
	// When `kube-reserved` is in the list, kubeReservedCgroup must be specified.
	// This field is supported only when `cgroupsPerQOS` is set to true.
	// Refer to [Node Allocatable](https://kubernetes.io/docs/tasks/administer-cluster/reserve-compute-resources/#node-allocatable)
	// for more information.
	// Default: ["pods"]
	// +optional
	EnforceNodeAllocatable []string `json:"enforceNodeAllocatable,omitempty"`
	// A comma separated whitelist of unsafe sysctls or sysctl patterns (ending in `*`).
	// Unsafe sysctl groups are `kernel.shm*`, `kernel.msg*`, `kernel.sem`, `fs.mqueue.*`,
	// and `net.*`. For example: "`kernel.msg*,net.ipv4.route.min_pmtu`"
	// Default: []
	// +optional
	AllowedUnsafeSysctls []string `json:"allowedUnsafeSysctls,omitempty"`
	// volumePluginDir is the full path of the directory in which to search
	// for additional third party volume plugins.
	// Default: "/usr/libexec/kubernetes/kubelet-plugins/volume/exec/"
	// +optional
	VolumePluginDir string `json:"volumePluginDir,omitempty"`
	// providerID, if set, sets the unique ID of the instance that an external
	// provider (i.e. cloudprovider) can use to identify a specific node.
	// Default: ""
	// +optional
	ProviderID string `json:"providerID,omitempty"`
	// kernelMemcgNotification, if set, instructs the kubelet to integrate with the
	// kernel memcg notification for determining if memory eviction thresholds are
	// exceeded rather than polling.
	// Default: false
	// +optional
	KernelMemcgNotification bool `json:"kernelMemcgNotification,omitempty"`
	// logging specifies the options of logging.
	// Refer to [Logs Options](https://github.com/kubernetes/component-base/blob/master/logs/options.go)
	// for more information.
	// Default:
	//   Format: text
	// + optional
	Logging logsapi.LoggingConfiguration `json:"logging,omitempty"`
	// enableSystemLogHandler enables system logs via web interface host:port/logs/
	// Default: true
	// +optional
	EnableSystemLogHandler *bool `json:"enableSystemLogHandler,omitempty"`
	// enableSystemLogQuery enables the node log query feature on the /logs endpoint.
	// EnableSystemLogHandler has to be enabled in addition for this feature to work.
	// Enabling this feature has security implications. The recommendation is to enable it on a need basis for debugging
	// purposes and disabling otherwise.
	// Default: false
	// +featureGate=NodeLogQuery
	// +optional
	EnableSystemLogQuery *bool `json:"enableSystemLogQuery,omitempty"`
	// shutdownGracePeriod specifies the total duration that the node should delay the
	// shutdown and total grace period for pod termination during a node shutdown.
	// Default: "0s"
	// +featureGate=GracefulNodeShutdown
	// +optional
	ShutdownGracePeriod metav1.Duration `json:"shutdownGracePeriod,omitempty"`
	// shutdownGracePeriodCriticalPods specifies the duration used to terminate critical
	// pods during a node shutdown. This should be less than shutdownGracePeriod.
	// For example, if shutdownGracePeriod=30s, and shutdownGracePeriodCriticalPods=10s,
	// during a node shutdown the first 20 seconds would be reserved for gracefully
	// terminating normal pods, and the last 10 seconds would be reserved for terminating
	// critical pods.
	// Default: "0s"
	// +featureGate=GracefulNodeShutdown
	// +optional
	ShutdownGracePeriodCriticalPods metav1.Duration `json:"shutdownGracePeriodCriticalPods,omitempty"`
	// shutdownGracePeriodByPodPriority specifies the shutdown grace period for Pods based
	// on their associated priority class value.
	// When a shutdown request is received, the Kubelet will initiate shutdown on all pods
	// running on the node with a grace period that depends on the priority of the pod,
	// and then wait for all pods to exit.
	// Each entry in the array represents the graceful shutdown time a pod with a priority
	// class value that lies in the range of that value and the next higher entry in the
	// list when the node is shutting down.
	// For example, to allow critical pods 10s to shutdown, priority>=10000 pods 20s to
	// shutdown, and all remaining pods 30s to shutdown.
	//
	// shutdownGracePeriodByPodPriority:
	//   - priority: 2000000000
	//     shutdownGracePeriodSeconds: 10
	//   - priority: 10000
	//     shutdownGracePeriodSeconds: 20
	//   - priority: 0
	//     shutdownGracePeriodSeconds: 30
	//
	// The time the Kubelet will wait before exiting will at most be the maximum of all
	// shutdownGracePeriodSeconds for each priority class range represented on the node.
	// When all pods have exited or reached their grace periods, the Kubelet will release
	// the shutdown inhibit lock.
	// Requires the GracefulNodeShutdown feature gate to be enabled.
	// This configuration must be empty if either ShutdownGracePeriod or ShutdownGracePeriodCriticalPods is set.
	// Default: nil
	// +featureGate=GracefulNodeShutdownBasedOnPodPriority
	// +optional
	ShutdownGracePeriodByPodPriority []ShutdownGracePeriodByPodPriority `json:"shutdownGracePeriodByPodPriority,omitempty"`
	// CrashLoopBackOff contains config to modify node-level parameters for
	// container restart behavior
	// +featureGate=KubeletCrashLoopBackOffMax
	// +optional
	CrashLoopBackOff CrashLoopBackOffConfig `json:"crashLoopBackOff,omitempty"`
	// reservedMemory specifies a comma-separated list of memory reservations for NUMA nodes.
	// The parameter makes sense only in the context of the memory manager feature.
	// The memory manager will not allocate reserved memory for container workloads.
	// For example, if you have a NUMA0 with 10Gi of memory and the reservedMemory was
	// specified to reserve 1Gi of memory at NUMA0, the memory manager will assume that
	// only 9Gi is available for allocation.
	// You can specify a different amount of NUMA node and memory types.
	// You can omit this parameter at all, but you should be aware that the amount of
	// reserved memory from all NUMA nodes should be equal to the amount of memory specified
	// by the [node allocatable](https://kubernetes.io/docs/tasks/administer-cluster/reserve-compute-resources/#node-allocatable).
	// If at least one node allocatable parameter has a non-zero value, you will need
	// to specify at least one NUMA node.
	// Also, avoid specifying:
	//
	// 1. Duplicates, the same NUMA node, and memory type, but with a different value.
	// 2. zero limits for any memory type.
	// 3. NUMAs nodes IDs that do not exist under the machine.
	// 4. memory types except for memory and hugepages-<size>
	//
	// Default: nil
	// +optional
	ReservedMemory []MemoryReservation `json:"reservedMemory,omitempty"`
	// enableProfilingHandler enables profiling via web interface host:port/debug/pprof/
	// Default: true
	// +optional
	EnableProfilingHandler *bool `json:"enableProfilingHandler,omitempty"`
	// enableDebugFlagsHandler enables flags endpoint via web interface host:port/debug/flags/v
	// Default: true
	// +optional
	EnableDebugFlagsHandler *bool `json:"enableDebugFlagsHandler,omitempty"`
	// SeccompDefault enables the use of `RuntimeDefault` as the default seccomp profile for all workloads.
	// Default: false
	// +optional
	SeccompDefault *bool `json:"seccompDefault,omitempty"`
	// MemoryThrottlingFactor specifies the factor multiplied by the memory limit or node allocatable memory
	// when setting the cgroupv2 memory.high value to enforce MemoryQoS.
	// Decreasing this factor will set lower high limit for container cgroups and put heavier reclaim pressure
	// while increasing will put less reclaim pressure.
	// See https://kep.k8s.io/2570 for more details.
	// Default: 0.9
	// +featureGate=MemoryQoS
	// +optional
	MemoryThrottlingFactor *float64 `json:"memoryThrottlingFactor,omitempty"`
	// registerWithTaints are an array of taints to add to a node object when
	// the kubelet registers itself. This only takes effect when registerNode
	// is true and upon the initial registration of the node.
	// Default: nil
	// +optional
	RegisterWithTaints []v1.Taint `json:"registerWithTaints,omitempty"`
	// registerNode enables automatic registration with the apiserver.
	// Default: true
	// +optional
	RegisterNode *bool `json:"registerNode,omitempty"`
	// Tracing specifies the versioned configuration for OpenTelemetry tracing clients.
	// See https://kep.k8s.io/2832 for more details.
	// Default: nil
	// +featureGate=KubeletTracing
	// +optional
	Tracing *tracingapi.TracingConfiguration `json:"tracing,omitempty"`

	// LocalStorageCapacityIsolation enables local ephemeral storage isolation feature. The default setting is true.
	// This feature allows users to set request/limit for container's ephemeral storage and manage it in a similar way
	// as cpu and memory. It also allows setting sizeLimit for emptyDir volume, which will trigger pod eviction if disk
	// usage from the volume exceeds the limit.
	// This feature depends on the capability of detecting correct root file system disk usage. For certain systems,
	// such as kind rootless, if this capability cannot be supported, the feature LocalStorageCapacityIsolation should be
	// disabled. Once disabled, user should not set request/limit for container's ephemeral storage, or sizeLimit for emptyDir.
	// Default: true
	// +optional
	LocalStorageCapacityIsolation *bool `json:"localStorageCapacityIsolation,omitempty"`

	// ContainerRuntimeEndpoint is the endpoint of container runtime.
	// Unix Domain Sockets are supported on Linux, while npipe and tcp endpoints are supported on Windows.
	// Examples:'unix:///path/to/runtime.sock', 'npipe:////./pipe/runtime'
	ContainerRuntimeEndpoint string `json:"containerRuntimeEndpoint"`

	// ImageServiceEndpoint is the endpoint of container image service.
	// Unix Domain Socket are supported on Linux, while npipe and tcp endpoints are supported on Windows.
	// Examples:'unix:///path/to/runtime.sock', 'npipe:////./pipe/runtime'.
	// If not specified, the value in containerRuntimeEndpoint is used.
	// +optional
	ImageServiceEndpoint string `json:"imageServiceEndpoint,omitempty"`

	// FailCgroupV1 prevents the kubelet from starting on hosts
	// that use cgroup v1. By default, this is set to 'false', meaning
	// the kubelet is allowed to start on cgroup v1 hosts unless this
	// option is explicitly enabled.
	// Default: false
	// +optional
	FailCgroupV1 *bool `json:"failCgroupV1,omitempty"`
}

type KubeletAuthorizationMode string

const (
	// KubeletAuthorizationModeAlwaysAllow authorizes all authenticated requests
	KubeletAuthorizationModeAlwaysAllow KubeletAuthorizationMode = "AlwaysAllow"
	// KubeletAuthorizationModeWebhook uses the SubjectAccessReview API to determine authorization
	KubeletAuthorizationModeWebhook KubeletAuthorizationMode = "Webhook"
)

type KubeletAuthorization struct {
	// mode is the authorization mode to apply to requests to the kubelet server.
	// Valid values are `AlwaysAllow` and `Webhook`.
	// Webhook mode uses the SubjectAccessReview API to determine authorization.
	// +optional
	Mode KubeletAuthorizationMode `json:"mode,omitempty"`

	// webhook contains settings related to Webhook authorization.
	// +optional
	Webhook KubeletWebhookAuthorization `json:"webhook"`
}

type KubeletWebhookAuthorization struct {
	// cacheAuthorizedTTL is the duration to cache 'authorized' responses from the
	// webhook authorizer.
	// +optional
	CacheAuthorizedTTL metav1.Duration `json:"cacheAuthorizedTTL,omitempty"`
	// cacheUnauthorizedTTL is the duration to cache 'unauthorized' responses from
	// the webhook authorizer.
	// +optional
	CacheUnauthorizedTTL metav1.Duration `json:"cacheUnauthorizedTTL,omitempty"`
}

type KubeletAuthentication struct {
	// x509 contains settings related to x509 client certificate authentication.
	// +optional
	X509 KubeletX509Authentication `json:"x509"`
	// webhook contains settings related to webhook bearer token authentication.
	// +optional
	Webhook KubeletWebhookAuthentication `json:"webhook"`
	// anonymous contains settings related to anonymous authentication.
	// +optional
	Anonymous KubeletAnonymousAuthentication `json:"anonymous"`
}

type KubeletX509Authentication struct {
	// clientCAFile is the path to a PEM-encoded certificate bundle. If set, any request
	// presenting a client certificate signed by one of the authorities in the bundle
	// is authenticated with a username corresponding to the CommonName,
	// and groups corresponding to the Organization in the client certificate.
	// +optional
	ClientCAFile string `json:"clientCAFile,omitempty"`
}

type KubeletWebhookAuthentication struct {
	// enabled allows bearer token authentication backed by the
	// tokenreviews.authentication.k8s.io API.
	// +optional
	Enabled *bool `json:"enabled,omitempty"`
	// cacheTTL enables caching of authentication results
	// +optional
	CacheTTL metav1.Duration `json:"cacheTTL,omitempty"`
}

type KubeletAnonymousAuthentication struct {
	// enabled allows anonymous requests to the kubelet server.
	// Requests that are not rejected by another authentication method are treated as
	// anonymous requests.
	// Anonymous requests have a username of `system:anonymous`, and a group name of
	// `system:unauthenticated`.
	// +optional
	Enabled *bool `json:"enabled,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// SerializedNodeConfigSource allows us to serialize v1.NodeConfigSource.
// This type is used internally by the Kubelet for tracking checkpointed dynamic configs.
// It exists in the kubeletconfig API group because it is classified as a versioned input to the Kubelet.
type SerializedNodeConfigSource struct {
	metav1.TypeMeta `json:",inline"`
	// source is the source that we are serializing.
	// +optional
	Source v1.NodeConfigSource `json:"source,omitempty" protobuf:"bytes,1,opt,name=source"`
}

// MemoryReservation specifies the memory reservation of different types for each NUMA node
type MemoryReservation struct {
	NumaNode int32           `json:"numaNode"`
	Limits   v1.ResourceList `json:"limits"`
}

// ShutdownGracePeriodByPodPriority specifies the shutdown grace period for Pods based on their associated priority class value
type ShutdownGracePeriodByPodPriority struct {
	// priority is the priority value associated with the shutdown grace period
	Priority int32 `json:"priority"`
	// shutdownGracePeriodSeconds is the shutdown grace period in seconds
	ShutdownGracePeriodSeconds int64 `json:"shutdownGracePeriodSeconds"`
}

type MemorySwapConfiguration struct {
	// swapBehavior configures swap memory available to container workloads. May be one of
	// "", "NoSwap": workloads can not use swap, default option.
	// "LimitedSwap": workload swap usage is limited. The swap limit is proportionate to the container's memory request.
	// +featureGate=NodeSwap
	// +optional
	SwapBehavior string `json:"swapBehavior,omitempty"`
}

type CrashLoopBackOffConfig struct {
	// maxContainerRestartPeriod is the maximum duration the backoff delay can accrue
	// to for container restarts, minimum 1 second, maximum 300 seconds. If not set,
	// defaults to the internal crashloopbackoff maximum (300s).
	// +featureGate=KubeletCrashLoopBackOffMax
	// +optional
	MaxContainerRestartPeriod *metav1.Duration `json:"maxContainerRestartPeriod,omitempty"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// CredentialProviderConfig is the configuration containing information about
// each exec credential provider. Kubelet reads this configuration from disk and enables
// each provider as specified by the CredentialProvider type.
type CredentialProviderConfig struct {
	metav1.TypeMeta `json:",inline"`

	// providers is a list of credential provider plugins that will be enabled by the kubelet.
	// Multiple providers may match against a single image, in which case credentials
	// from all providers will be returned to the kubelet. If multiple providers are called
	// for a single image, the results are combined. If providers return overlapping
	// auth keys, the value from the provider earlier in this list is attempted first.
	Providers []CredentialProvider `json:"providers"`
}

// CredentialProvider represents an exec plugin to be invoked by the kubelet. The plugin is only
// invoked when an image being pulled matches the images handled by the plugin (see matchImages).
type CredentialProvider struct {
	// name is the required name of the credential provider. It must match the name of the
	// provider executable as seen by the kubelet. The executable must be in the kubelet's
	// bin directory (set by the --image-credential-provider-bin-dir flag).
	// Required to be unique across all providers.
	Name string `json:"name"`

	// matchImages is a required list of strings used to match against images in order to
	// determine if this provider should be invoked. If one of the strings matches the
	// requested image from the kubelet, the plugin will be invoked and given a chance
	// to provide credentials. Images are expected to contain the registry domain
	// and URL path.
	//
	// Each entry in matchImages is a pattern which can optionally contain a port and a path.
	// Globs can be used in the domain, but not in the port or the path. Globs are supported
	// as subdomains like '*.k8s.io' or 'k8s.*.io', and top-level-domains such as 'k8s.*'.
	// Matching partial subdomains like 'app*.k8s.io' is also supported. Each glob can only match
	// a single subdomain segment, so *.io does not match *.k8s.io.
	//
	// A match exists between an image and a matchImage when all of the below are true:
	// - Both contain the same number of domain parts and each part matches.
	// - The URL path of an imageMatch must be a prefix of the target image URL path.
	// - If the imageMatch contains a port, then the port must match in the image as well.
	//
	// Example values of matchImages:
	//   - 123456789.dkr.ecr.us-east-1.amazonaws.com
	//   - *.azurecr.io
	//   - gcr.io
	//   - *.*.registry.io
	//   - registry.io:8080/path
	MatchImages []string `json:"matchImages"`

	// defaultCacheDuration is the default duration the plugin will cache credentials in-memory
	// if a cache duration is not provided in the plugin response. This field is required.
	DefaultCacheDuration *metav1.Duration `json:"defaultCacheDuration"`

	// Required input version of the exec CredentialProviderRequest. The returned CredentialProviderResponse
	// MUST use the same encoding version as the input. Current supported values are:
	// - credentialprovider.kubelet.k8s.io/v1beta1
	APIVersion string `json:"apiVersion"`

	// Arguments to pass to the command when executing it.
	// +optional
	Args []string `json:"args,omitempty"`

	// Env defines additional environment variables to expose to the process. These
	// are unioned with the host's environment, as well as variables client-go uses
	// to pass argument to the plugin.
	// +optional
	Env []ExecEnvVar `json:"env,omitempty"`
}

// ExecEnvVar is used for setting environment variables when executing an exec-based
// credential plugin.
type ExecEnvVar struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}
