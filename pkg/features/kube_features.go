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

package features

import (
	"k8s.io/apimachinery/pkg/util/runtime"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	clientfeatures "k8s.io/client-go/features"
	"k8s.io/component-base/featuregate"
	zpagesfeatures "k8s.io/component-base/zpages/features"
)

const (
	// Every feature gate should add method here following this template:
	//
	// // owner: @username
	// // kep: https://kep.k8s.io/NNN
	// MyFeature featuregate.Feature = "MyFeature"
	//
	// Feature gates should be listed in alphabetical, case-sensitive
	// (upper before any lower case character) order. This reduces the risk
	// of code conflicts because changes are more likely to be scattered
	// across the file.

	// owner: @aojea
	// Deprecated: v1.31
	//
	// Allow kubelet to request a certificate without any Node IP available, only
	// with DNS names.
	AllowDNSOnlyNodeCSR featuregate.Feature = "AllowDNSOnlyNodeCSR"

	// owner: @HirazawaUi
	// Deprecated: v1.32
	//
	// Allow spec.terminationGracePeriodSeconds to be overridden by MaxPodGracePeriodSeconds in soft evictions.
	AllowOverwriteTerminationGracePeriodSeconds featuregate.Feature = "AllowOverwriteTerminationGracePeriodSeconds"

	// owner: @thockin
	// Deprecated: v1.29
	//
	// Enables Service.status.ingress.loadBanace to be set on
	// services of types other than LoadBalancer.
	AllowServiceLBStatusOnNonLB featuregate.Feature = "AllowServiceLBStatusOnNonLB"

	// owner: @bswartz
	//
	// Enables usage of any object for volume data source in PVCs
	AnyVolumeDataSource featuregate.Feature = "AnyVolumeDataSource"

	// owner: @tallclair
	AppArmor featuregate.Feature = "AppArmor"

	// owner: @tallclair
	AppArmorFields featuregate.Feature = "AppArmorFields"

	// owner: @liggitt
	// kep:
	//
	// Make the Node authorizer use fine-grained selector authorization.
	// Requires AuthorizeWithSelectors to be enabled.
	AuthorizeNodeWithSelectors featuregate.Feature = "AuthorizeNodeWithSelectors"

	// owner: @ahmedtd
	//
	// Enable ClusterTrustBundle objects and Kubelet integration.
	ClusterTrustBundle featuregate.Feature = "ClusterTrustBundle"

	// owner: @ahmedtd
	//
	// Enable ClusterTrustBundle Kubelet projected volumes.  Depends on ClusterTrustBundle.
	ClusterTrustBundleProjection featuregate.Feature = "ClusterTrustBundleProjection"

	// owner: @szuecs
	//
	// Enable nodes to change CPUCFSQuotaPeriod
	CPUCFSQuotaPeriod featuregate.Feature = "CustomCPUCFSQuotaPeriod"

	// owner: @ConnorDoyle, @fromanirh (only for GA graduation)
	//
	// Alternative container-level CPU affinity policies.
	CPUManager featuregate.Feature = "CPUManager"

	// owner: @fromanirh
	// beta: see below.
	//
	// Allow fine-tuning of cpumanager policies, experimental, alpha-quality options
	// Per https://groups.google.com/g/kubernetes-sig-architecture/c/Nxsc7pfe5rw/m/vF2djJh0BAAJ
	// We want to avoid a proliferation of feature gates. This feature gate:
	// - will guard *a group* of cpumanager options whose quality level is alpha.
	// - will never graduate to beta or stable.
	// See https://groups.google.com/g/kubernetes-sig-architecture/c/Nxsc7pfe5rw/m/vF2djJh0BAAJ
	// for details about the removal of this feature gate.
	CPUManagerPolicyAlphaOptions featuregate.Feature = "CPUManagerPolicyAlphaOptions"

	// owner: @fromanirh
	// beta: see below.
	//
	// Allow fine-tuning of cpumanager policies, experimental, beta-quality options
	// Per https://groups.google.com/g/kubernetes-sig-architecture/c/Nxsc7pfe5rw/m/vF2djJh0BAAJ
	// We want to avoid a proliferation of feature gates. This feature gate:
	// - will guard *a group* of cpumanager options whose quality level is beta.
	// - is thus *introduced* as beta
	// - will never graduate to stable.
	// See https://groups.google.com/g/kubernetes-sig-architecture/c/Nxsc7pfe5rw/m/vF2djJh0BAAJ
	// for details about the removal of this feature gate.
	CPUManagerPolicyBetaOptions featuregate.Feature = "CPUManagerPolicyBetaOptions"

	// owner: @fromanirh
	//
	// Allow the usage of options to fine-tune the cpumanager policies.
	CPUManagerPolicyOptions featuregate.Feature = "CPUManagerPolicyOptions"

	// owner: @jefftree
	// kep: https://kep.k8s.io/4355
	//
	// Enables coordinated leader election in the API server
	CoordinatedLeaderElection featuregate.Feature = "CoordinatedLeaderElection"

	// owner: @trierra
	// kep:  http://kep.k8s.io/2589
	//
	// Enables the Portworx in-tree driver to Portworx migration feature.
	CSIMigrationPortworx featuregate.Feature = "CSIMigrationPortworx"

	// owner: @fengzixu
	//
	// Enables kubelet to detect CSI volume condition and send the event of the abnormal volume to the corresponding pod that is using it.
	CSIVolumeHealth featuregate.Feature = "CSIVolumeHealth"

	// owner: @adrianreber
	// kep: https://kep.k8s.io/2008
	//
	// Enables container Checkpoint support in the kubelet
	ContainerCheckpoint featuregate.Feature = "ContainerCheckpoint"

	// owner: @helayoty
	// kep: https://kep.k8s.io/4026
	//
	// Set the scheduled time as an annotation in the job.
	CronJobsScheduledAnnotation featuregate.Feature = "CronJobsScheduledAnnotation"

	// owner: @ttakahashi21 @mkimuram
	// kep: https://kep.k8s.io/3294
	//
	// Enable usage of Provision of PVCs from snapshots in other namespaces
	CrossNamespaceVolumeDataSource featuregate.Feature = "CrossNamespaceVolumeDataSource"

	// owner: @elezar
	// kep: http://kep.k8s.io/4009
	//
	// Add support for CDI Device IDs in the Device Plugin API.
	DevicePluginCDIDevices featuregate.Feature = "DevicePluginCDIDevices"

	// owner: @aojea
	//
	// The apiservers with the MultiCIDRServiceAllocator feature enable, in order to support live migration from the old bitmap ClusterIP
	// allocators to the new IPAddress allocators introduced by the MultiCIDRServiceAllocator feature, performs a dual-write on
	// both allocators. This feature gate disables the dual write on the new Cluster IP allocators.
	DisableAllocatorDualWrite featuregate.Feature = "DisableAllocatorDualWrite"

	// owner: @andrewsykim
	//
	// Disable any functionality in kube-apiserver, kube-controller-manager and kubelet related to the `--cloud-provider` component flag.
	DisableCloudProviders featuregate.Feature = "DisableCloudProviders"

	// owner: @andrewsykim
	//
	// Disable in-tree functionality in kubelet to authenticate to cloud provider container registries for image pull credentials.
	DisableKubeletCloudCredentialProviders featuregate.Feature = "DisableKubeletCloudCredentialProviders"

	// owner: @micahhausler
	// Deprecated: v1.31
	//
	// Setting AllowInsecureKubeletCertificateSigningRequests to true disables node admission validation of CSRs
	// for kubelet signers where CN=system:node:$nodeName.
	// Remove in v1.33
	AllowInsecureKubeletCertificateSigningRequests featuregate.Feature = "AllowInsecureKubeletCertificateSigningRequests"

	// owner: @hoskeri
	// Deprecated: v1.32
	//
	// Restores previous behavior where Kubelet fails self registration if node create returns 403 Forbidden.
	// Remove in v1.34
	KubeletRegistrationGetOnExistsOnly featuregate.Feature = "KubeletRegistrationGetOnExistsOnly"

	// owner: @HirazawaUi
	// kep: http://kep.k8s.io/4004
	// Deprecated: v1.31 (default off)
	//
	// DisableNodeKubeProxyVersion disable the status.nodeInfo.kubeProxyVersion field of v1.Node
	DisableNodeKubeProxyVersion featuregate.Feature = "DisableNodeKubeProxyVersion"

	// owner: @pohly
	// kep: http://kep.k8s.io/4381
	//
	// Enables support for requesting admin access in a ResourceClaim.
	// Admin access is granted even if a device is already in use and,
	// depending on the DRA driver, may enable additional permissions
	// when a container uses the allocated device.
	//
	// This feature gate is currently defined in KEP #4381. The intent
	// is to move it into a separate KEP.
	DRAAdminAccess featuregate.Feature = "DRAAdminAccess"

	// owner: @pohly
	// kep: http://kep.k8s.io/4381
	//
	// Enables support for resources with custom parameters and a lifecycle
	// that is independent of a Pod. Resource allocation is done by the scheduler
	// based on "structured parameters".
	DynamicResourceAllocation featuregate.Feature = "DynamicResourceAllocation"

	// owner: @LionelJouin
	// kep: http://kep.k8s.io/4817
	// alpha: v1.32
	//
	// Enables support the ResourceClaim.status.devices field and for setting this
	// status from DRA drivers.
	DRAResourceClaimDeviceStatus featuregate.Feature = "DRAResourceClaimDeviceStatus"

	// owner: @lauralorenz
	// kep: https://kep.k8s.io/4603
	// owner: @lauralorenz
	// kep: https://kep.k8s.io/4603
	//
	// Enables support for configurable per-node backoff maximums for restarting
	// containers (aka containers in CrashLoopBackOff)
	KubeletCrashLoopBackOffMax featuregate.Feature = "KubeletCrashLoopBackOffMax"

	// owner: @harche
	// kep: http://kep.k8s.io/3386
	//
	// Allows using event-driven PLEG (pod lifecycle event generator) through kubelet
	// which avoids frequent relisting of containers which helps optimize performance.
	EventedPLEG featuregate.Feature = "EventedPLEG"

	// owner: @andrewsykim @SergeyKanzhelev
	//
	// Ensure kubelet respects exec probe timeouts. Feature gate exists in-case existing workloads
	// may depend on old behavior where exec probe timeouts were ignored.
	// Lock to default and remove after v1.22 based on user feedback that should be reflected in KEP #1972 update
	ExecProbeTimeout featuregate.Feature = "ExecProbeTimeout"

	// owner: @bobbypage
	// Adds support for kubelet to detect node shutdown and gracefully terminate pods prior to the node being shutdown.
	GracefulNodeShutdown featuregate.Feature = "GracefulNodeShutdown"

	// owner: @wzshiming
	// Make the kubelet use shutdown configuration based on pod priority values for graceful shutdown.
	GracefulNodeShutdownBasedOnPodPriority featuregate.Feature = "GracefulNodeShutdownBasedOnPodPriority"

	// owner: @dxist
	//
	// Enables support of HPA scaling to zero pods when an object or custom metric is configured.
	HPAScaleToZero featuregate.Feature = "HPAScaleToZero"

	// owner: @deepakkinni @xing-yang
	// kep: https://kep.k8s.io/2644
	//
	// Honor Persistent Volume Reclaim Policy when it is "Delete" irrespective of PV-PVC
	// deletion ordering.
	HonorPVReclaimPolicy featuregate.Feature = "HonorPVReclaimPolicy"

	// owner: @vinaykul,@tallclair
	// kep: http://kep.k8s.io/1287
	//
	// Enables In-Place Pod Vertical Scaling
	InPlacePodVerticalScaling featuregate.Feature = "InPlacePodVerticalScaling"

	// owner: @tallclair
	// kep: http://kep.k8s.io/1287
	//
	// Enables the AllocatedResources field in container status. This feature requires
	// InPlacePodVerticalScaling also be enabled.
	InPlacePodVerticalScalingAllocatedStatus featuregate.Feature = "InPlacePodVerticalScalingAllocatedStatus"

	// owner: @tallclair @esotsal
	// alpha: v1.32
	//
	// Allow resource resize for containers in Guaranteed pods with integer CPU requests ( default false ).
	// Applies only in nodes with InPlacePodVerticalScaling and CPU Manager features enabled, and
	// CPU Manager Static Policy option set.
	InPlacePodVerticalScalingExclusiveCPUs featuregate.Feature = "InPlacePodVerticalScalingExclusiveCPUs"

	// owner: @trierra
	//
	// Disables the Portworx in-tree driver.
	InTreePluginPortworxUnregister featuregate.Feature = "InTreePluginPortworxUnregister"

	// owner: @mimowo
	// kep: https://kep.k8s.io/3850
	//
	// Allows users to specify counting of failed pods per index.
	JobBackoffLimitPerIndex featuregate.Feature = "JobBackoffLimitPerIndex"

	// owner: @mimowo
	// kep: https://kep.k8s.io/4368
	// alpha: v1.30
	// beta: v1.32
	//
	// Allows to delegate reconciliation of a Job object to an external controller.
	JobManagedBy featuregate.Feature = "JobManagedBy"

	// owner: @mimowo
	// kep: https://kep.k8s.io/3329
	//
	// Allow users to specify handling of pod failures based on container exit codes
	// and pod conditions.
	JobPodFailurePolicy featuregate.Feature = "JobPodFailurePolicy"

	// owner: @kannon92
	// kep : https://kep.k8s.io/3939
	//
	// Allow users to specify recreating pods of a job only when
	// pods have fully terminated.
	JobPodReplacementPolicy featuregate.Feature = "JobPodReplacementPolicy"

	// owner: @tenzen-y
	// kep: https://kep.k8s.io/3998
	//
	// Allow users to specify when a Job can be declared as succeeded
	// based on the set of succeeded pods.
	JobSuccessPolicy featuregate.Feature = "JobSuccessPolicy"

	// owner: @marquiz
	// kep: http://kep.k8s.io/4033
	//
	// Enable detection of the kubelet cgroup driver configuration option from
	// the CRI.  The CRI runtime also needs to support this feature in which
	// case the kubelet will ignore the cgroupDriver (--cgroup-driver)
	// configuration option. If runtime doesn't support it, the kubelet will
	// fallback to using it's cgroupDriver option.
	KubeletCgroupDriverFromCRI featuregate.Feature = "KubeletCgroupDriverFromCRI"

	// owner: @vinayakankugoyal
	// kep: http://kep.k8s.io/2862
	//
	// Enable fine-grained kubelet API authorization for webhook based
	// authorization.
	KubeletFineGrainedAuthz featuregate.Feature = "KubeletFineGrainedAuthz"

	// owner: @AkihiroSuda
	//
	// Enables support for running kubelet in a user namespace.
	// The user namespace has to be created before running kubelet.
	// All the node components such as CRI need to be running in the same user namespace.
	KubeletInUserNamespace featuregate.Feature = "KubeletInUserNamespace"

	// owner: @moshe010
	//
	// Enable POD resources API to return resources allocated by Dynamic Resource Allocation
	KubeletPodResourcesDynamicResources featuregate.Feature = "KubeletPodResourcesDynamicResources"

	// owner: @moshe010
	//
	// Enable POD resources API with Get method
	KubeletPodResourcesGet featuregate.Feature = "KubeletPodResourcesGet"

	// owner: @kannon92
	// kep: https://kep.k8s.io/4191
	//
	// The split image filesystem feature enables kubelet to perform garbage collection
	// of images (read-only layers) and/or containers (writeable layers) deployed on
	// separate filesystems.
	KubeletSeparateDiskGC featuregate.Feature = "KubeletSeparateDiskGC"

	// owner: @sallyom
	// kep: https://kep.k8s.io/2832
	//
	// Add support for distributed tracing in the kubelet
	KubeletTracing featuregate.Feature = "KubeletTracing"

	// owner: @alexanderConstantinescu
	// kep: http://kep.k8s.io/3836
	//
	// Implement connection draining for terminating nodes for
	// `externalTrafficPolicy: Cluster` services.
	KubeProxyDrainingTerminatingNodes featuregate.Feature = "KubeProxyDrainingTerminatingNodes"

	// owner: @RobertKrawitz
	//
	// Allow use of filesystems for ephemeral storage monitoring.
	// Only applies if LocalStorageCapacityIsolation is set.
	// Relies on UserNamespacesSupport feature, and thus should follow it when setting defaults.
	LocalStorageCapacityIsolationFSQuotaMonitoring featuregate.Feature = "LocalStorageCapacityIsolationFSQuotaMonitoring"

	// owner: @damemi
	//
	// Enables scaling down replicas via logarithmic comparison of creation/ready timestamps
	LogarithmicScaleDown featuregate.Feature = "LogarithmicScaleDown"

	// owner: @sanposhiho
	// kep: https://kep.k8s.io/3633
	//
	// Enables the MatchLabelKeys and MismatchLabelKeys in PodAffinity and PodAntiAffinity.
	MatchLabelKeysInPodAffinity featuregate.Feature = "MatchLabelKeysInPodAffinity"

	// owner: @denkensk
	// kep: https://kep.k8s.io/3243
	//
	// Enable MatchLabelKeys in PodTopologySpread.
	MatchLabelKeysInPodTopologySpread featuregate.Feature = "MatchLabelKeysInPodTopologySpread"

	// owner: @krmayankk
	//
	// Enables maxUnavailable for StatefulSet
	MaxUnavailableStatefulSet featuregate.Feature = "MaxUnavailableStatefulSet"

	// owner: @cynepco3hahue(alukiano) @cezaryzukowski @k-wiatrzyk, @Tal-or (only for GA graduation)
	//
	// Allows setting memory affinity for a container based on NUMA topology
	MemoryManager featuregate.Feature = "MemoryManager"

	// owner: @xiaoxubeii
	// kep: https://kep.k8s.io/2570
	//
	// Enables kubelet to support memory QoS with cgroups v2.
	MemoryQoS featuregate.Feature = "MemoryQoS"

	// owner: @aojea
	// kep: https://kep.k8s.io/1880
	//
	// Enables the dynamic configuration of Service IP ranges
	MultiCIDRServiceAllocator featuregate.Feature = "MultiCIDRServiceAllocator"

	// owner: @danwinship
	// kep: https://kep.k8s.io/3866
	//
	// Allows running kube-proxy with `--mode nftables`.
	NFTablesProxyMode featuregate.Feature = "NFTablesProxyMode"

	// owner: @aravindhp @LorbusChris
	// kep: http://kep.k8s.io/2271
	//
	// Enables querying logs of node services using the /logs endpoint. Enabling this feature has security implications.
	// The recommendation is to enable it on a need basis for debugging purposes and disabling otherwise.
	NodeLogQuery featuregate.Feature = "NodeLogQuery"

	// owner: @iholder101 @kannon92
	// kep: https://kep.k8s.io/2400

	// Permits kubelet to run with swap enabled.
	NodeSwap featuregate.Feature = "NodeSwap"

	// owner: @RomanBednar
	// kep: https://kep.k8s.io/3762
	//
	// Adds a new field to persistent volumes which holds a timestamp of when the volume last transitioned its phase.
	PersistentVolumeLastPhaseTransitionTime featuregate.Feature = "PersistentVolumeLastPhaseTransitionTime"

	// owner: @haircommander
	// kep: https://kep.k8s.io/2364
	//
	// Configures the Kubelet to use the CRI to populate pod and container stats, instead of supplimenting with stats from cAdvisor.
	// Requires the CRI implementation supports supplying the required stats.
	PodAndContainerStatsFromCRI featuregate.Feature = "PodAndContainerStatsFromCRI"

	// owner: @ahg-g
	//
	// Enables controlling pod ranking on replicaset scale-down.
	PodDeletionCost featuregate.Feature = "PodDeletionCost"

	// owner: @mimowo
	// kep: https://kep.k8s.io/3329
	//
	// Enables support for appending a dedicated pod condition indicating that
	// the pod is being deleted due to a disruption.
	PodDisruptionConditions featuregate.Feature = "PodDisruptionConditions"

	// owner: @danielvegamyhre
	// kep: https://kep.k8s.io/4017
	//
	// Set pod completion index as a pod label for Indexed Jobs.
	PodIndexLabel featuregate.Feature = "PodIndexLabel"

	// owner: @knight42
	// kep: https://kep.k8s.io/3288
	// alpha: v1.32
	//
	// Enables only stdout or stderr of the container to be retrievd.
	PodLogsQuerySplitStreams featuregate.Feature = "PodLogsQuerySplitStreams"

	// owner: @ddebroy, @kannon92
	//
	// Enables reporting of PodReadyToStartContainersCondition condition in pod status after pod
	// sandbox creation and network configuration completes successfully
	PodReadyToStartContainersCondition featuregate.Feature = "PodReadyToStartContainersCondition"

	// owner: @AxeZhan
	// kep: http://kep.k8s.io/3960
	//
	// Enables SleepAction in container lifecycle hooks
	PodLifecycleSleepAction featuregate.Feature = "PodLifecycleSleepAction"

	// owner: @sreeram-venkitesh
	// kep: http://kep.k8s.io/4818
	//
	// Allows zero value for sleep duration in SleepAction in container lifecycle hooks
	PodLifecycleSleepActionAllowZero featuregate.Feature = "PodLifecycleSleepActionAllowZero"

	// owner: @Huang-Wei
	// kep: https://kep.k8s.io/3521
	//
	// Enable users to specify when a Pod is ready for scheduling.
	PodSchedulingReadiness featuregate.Feature = "PodSchedulingReadiness"

	// owner: @seans3
	// kep: http://kep.k8s.io/4006
	//
	// Enables PortForward to be proxied with a websocket client
	PortForwardWebsockets featuregate.Feature = "PortForwardWebsockets"

	// owner: @jessfraz
	//
	// Enables control over ProcMountType for containers.
	ProcMountType featuregate.Feature = "ProcMountType"

	// owner: @sjenning
	//
	// Allows resource reservations at the QoS level preventing pods at lower QoS levels from
	// bursting into resources requested at higher QoS levels (memory only for now)
	QOSReserved featuregate.Feature = "QOSReserved"

	// owner: @gnufied
	// kep: https://kep.k8s.io/1790
	// beta - v1.32
	//
	// Allow users to recover from volume expansion failure
	RecoverVolumeExpansionFailure featuregate.Feature = "RecoverVolumeExpansionFailure"

	// owner: @AkihiroSuda
	// kep: https://kep.k8s.io/3857
	//
	// Allows recursive read-only mounts.
	RecursiveReadOnlyMounts featuregate.Feature = "RecursiveReadOnlyMounts"

	// owner: @adrianmoisey
	// kep: https://kep.k8s.io/4427
	//
	// Relaxed DNS search string validation.
	RelaxedDNSSearchValidation featuregate.Feature = "RelaxedDNSSearchValidation"

	// owner: @HirazawaUi
	// kep: https://kep.k8s.io/4369
	//
	// Allow almost all printable ASCII characters in environment variables
	RelaxedEnvironmentVariableValidation featuregate.Feature = "RelaxedEnvironmentVariableValidation"

	// owner: @zhangweikop
	//
	// Enable kubelet tls server to update certificate if the specified certificate files are changed.
	// This feature is useful when specifying tlsCertFile & tlsPrivateKeyFile in kubelet Configuration.
	// No effect for other cases such as using serverTLSbootstap.
	ReloadKubeletServerCertificateFile featuregate.Feature = "ReloadKubeletServerCertificateFile"

	// owner: @SergeyKanzhelev
	// kep: https://kep.k8s.io/4680
	//
	// Adds the AllocatedResourcesStatus to the container status.
	ResourceHealthStatus featuregate.Feature = "ResourceHealthStatus"

	// owner: @mikedanese
	//
	// Gets a server certificate for the kubelet from the Certificate Signing
	// Request API instead of generating one self signed and auto rotates the
	// certificate as expiration approaches.
	RotateKubeletServerCertificate featuregate.Feature = "RotateKubeletServerCertificate"

	// owner: @kiashok
	// kep: https://kep.k8s.io/4216
	//
	// Adds support to pull images based on the runtime class specified.
	RuntimeClassInImageCriAPI featuregate.Feature = "RuntimeClassInImageCriApi"

	// owner: @danielvegamyhre
	// kep: https://kep.k8s.io/2413
	//
	// Allows mutating spec.completions for Indexed job when done in tandem with
	// spec.parallelism. Specifically, spec.completions is mutable iff spec.completions
	// equals to spec.parallelism before and after the update.
	ElasticIndexedJob featuregate.Feature = "ElasticIndexedJob"

	// owner: @sanposhiho
	// kep: http://kep.k8s.io/4247
	//
	// Enables the scheduler's enhancement called QueueingHints,
	// which benefits to reduce the useless requeueing.
	SchedulerQueueingHints featuregate.Feature = "SchedulerQueueingHints"

	// owner: @sanposhiho
	// kep: http://kep.k8s.io/4832
	// alpha: v1.32
	//
	// Running some expensive operation within the scheduler's preemption asynchronously,
	// which improves the scheduling latency when the preemption involves in.
	SchedulerAsyncPreemption featuregate.Feature = "SchedulerAsyncPreemption"

	// owner: @atosatto @yuanchen8911
	// kep: http://kep.k8s.io/3902
	//
	// Decouples Taint Eviction Controller, performing taint-based Pod eviction, from Node Lifecycle Controller.
	SeparateTaintEvictionController featuregate.Feature = "SeparateTaintEvictionController"

	// owner: @aramase
	// kep: https://kep.k8s.io/4412
	//
	// ServiceAccountNodeAudienceRestriction is used to restrict the audience for which the
	// kubelet can request a service account token for.
	ServiceAccountNodeAudienceRestriction featuregate.Feature = "ServiceAccountNodeAudienceRestriction"

	// owner: @munnerz
	// kep: http://kep.k8s.io/4193
	//
	// Controls whether JTIs (UUIDs) are embedded into generated service account tokens, and whether these JTIs are
	// recorded into the audit log for future requests made by these tokens.
	ServiceAccountTokenJTI featuregate.Feature = "ServiceAccountTokenJTI"

	// owner: @munnerz
	// kep: http://kep.k8s.io/4193
	//
	// Controls whether the apiserver supports binding service account tokens to Node objects.
	ServiceAccountTokenNodeBinding featuregate.Feature = "ServiceAccountTokenNodeBinding"

	// owner: @munnerz
	// kep: http://kep.k8s.io/4193
	//
	// Controls whether the apiserver will validate Node claims in service account tokens.
	ServiceAccountTokenNodeBindingValidation featuregate.Feature = "ServiceAccountTokenNodeBindingValidation"

	// owner: @munnerz
	// kep: http://kep.k8s.io/4193
	//
	// Controls whether the apiserver embeds the node name and uid for the associated node when issuing
	// service account tokens bound to Pod objects.
	ServiceAccountTokenPodNodeInfo featuregate.Feature = "ServiceAccountTokenPodNodeInfo"

	// owner: @gauravkghildiyal @robscott
	// kep: https://kep.k8s.io/4444
	//
	// Enables trafficDistribution field on Services.
	ServiceTrafficDistribution featuregate.Feature = "ServiceTrafficDistribution"

	// owner: @gjkim42 @SergeyKanzhelev @matthyx @tzneal
	// kep: http://kep.k8s.io/753
	//
	// Introduces sidecar containers, a new type of init container that starts
	// before other containers but remains running for the full duration of the
	// pod's lifecycle and will not block pod termination.
	SidecarContainers featuregate.Feature = "SidecarContainers"

	// owner: @derekwaynecarr
	//
	// Enables kubelet support to size memory backed volumes
	// This is a kubelet only feature gate.
	// Code can be removed in 1.35 without any consideration for emulated versions.
	SizeMemoryBackedVolumes featuregate.Feature = "SizeMemoryBackedVolumes"

	// owner: @mattcary
	//
	// Enables policies controlling deletion of PVCs created by a StatefulSet.
	StatefulSetAutoDeletePVC featuregate.Feature = "StatefulSetAutoDeletePVC"

	// owner: @psch
	//
	// Enables a StatefulSet to start from an arbitrary non zero ordinal
	StatefulSetStartOrdinal featuregate.Feature = "StatefulSetStartOrdinal"

	// owner: @ahutsunshine
	//
	// Allows namespace indexer for namespace scope resources in apiserver cache to accelerate list operations.
	StorageNamespaceIndex featuregate.Feature = "StorageNamespaceIndex"

	// owner: @nilekhc
	// kep: https://kep.k8s.io/4192

	// Enables support for the StorageVersionMigrator controller.
	StorageVersionMigrator featuregate.Feature = "StorageVersionMigrator"

	// owner: @robscott
	// kep: https://kep.k8s.io/2433
	//
	// Enables topology aware hints for EndpointSlices
	TopologyAwareHints featuregate.Feature = "TopologyAwareHints"

	// owner: @PiotrProkop
	// kep: https://kep.k8s.io/3545
	//
	// Allow fine-tuning of topology manager policies with alpha options.
	// This feature gate:
	// - will guard *a group* of topology manager options whose quality level is alpha.
	// - will never graduate to beta or stable.
	TopologyManagerPolicyAlphaOptions featuregate.Feature = "TopologyManagerPolicyAlphaOptions"

	// owner: @PiotrProkop
	// kep: https://kep.k8s.io/3545
	//
	// Allow fine-tuning of topology manager policies with beta options.
	// This feature gate:
	// - will guard *a group* of topology manager options whose quality level is beta.
	// - is thus *introduced* as beta
	// - will never graduate to stable.
	TopologyManagerPolicyBetaOptions featuregate.Feature = "TopologyManagerPolicyBetaOptions"

	// owner: @PiotrProkop
	// kep: https://kep.k8s.io/3545
	//
	// Allow the usage of options to fine-tune the topology manager policies.
	TopologyManagerPolicyOptions featuregate.Feature = "TopologyManagerPolicyOptions"

	// owner: @seans3
	// kep: http://kep.k8s.io/4006
	//
	// Enables StreamTranslator proxy to handle WebSockets upgrade requests for the
	// version of the RemoteCommand subprotocol that supports the "close" signal.
	TranslateStreamCloseWebsocketRequests featuregate.Feature = "TranslateStreamCloseWebsocketRequests"

	// owner: @richabanker
	//
	// Proxies client to an apiserver capable of serving the request in the event of version skew.
	UnknownVersionInteroperabilityProxy featuregate.Feature = "UnknownVersionInteroperabilityProxy"

	// owner: @rata, @giuseppe
	// kep: https://kep.k8s.io/127
	//
	// Enables user namespace support for stateless pods.
	UserNamespacesSupport featuregate.Feature = "UserNamespacesSupport"

	// owner: @mattcarry, @sunnylovestiramisu
	// kep: https://kep.k8s.io/3751
	//
	// Enables user specified volume attributes for persistent volumes, like iops and throughput.
	VolumeAttributesClass featuregate.Feature = "VolumeAttributesClass"

	// owner: @cofyc
	VolumeCapacityPriority featuregate.Feature = "VolumeCapacityPriority"

	// owner: @ksubrmnn
	//
	// Allows kube-proxy to create DSR loadbalancers for Windows
	WinDSR featuregate.Feature = "WinDSR"

	// owner: @zylxjtu
	// kep: https://kep.k8s.io/4802
	// alpha: v1.32
	//
	// Enables support for graceful shutdown windows node.
	WindowsGracefulNodeShutdown featuregate.Feature = "WindowsGracefulNodeShutdown"

	// owner: @ksubrmnn
	//
	// Allows kube-proxy to run in Overlay mode for Windows
	WinOverlay featuregate.Feature = "WinOverlay"

	// owner: @jsturtevant
	// kep: https://kep.k8s.io/4888
	//
	// Add CPU and Memory Affinity support to Windows nodes with CPUManager, MemoryManager and Topology manager
	WindowsCPUAndMemoryAffinity featuregate.Feature = "WindowsCPUAndMemoryAffinity"

	// owner: @marosset
	// kep: https://kep.k8s.io/3503
	//
	// Enables support for joining Windows containers to a hosts' network namespace.
	WindowsHostNetwork featuregate.Feature = "WindowsHostNetwork"

	// owner: @kerthcet
	// kep: https://kep.k8s.io/3094
	//
	// Allow users to specify whether to take nodeAffinity/nodeTaint into consideration when
	// calculating pod topology spread skew.
	NodeInclusionPolicyInPodTopologySpread featuregate.Feature = "NodeInclusionPolicyInPodTopologySpread"

	// owner: @jsafrane
	// kep: https://kep.k8s.io/1710
	// Speed up container startup by mounting volumes with the correct SELinux label
	// instead of changing each file on the volumes recursively.
	// Initial implementation focused on ReadWriteOncePod volumes.
	SELinuxMountReadWriteOncePod featuregate.Feature = "SELinuxMountReadWriteOncePod"

	// owner: @Sh4d1,@RyanAoh,@rikatz
	// kep: http://kep.k8s.io/1860
	// LoadBalancerIPMode enables the IPMode field in the LoadBalancerIngress status of a Service
	LoadBalancerIPMode featuregate.Feature = "LoadBalancerIPMode"

	// owner: @haircommander
	// kep: http://kep.k8s.io/4210
	// ImageMaximumGCAge enables the Kubelet configuration field of the same name, allowing an admin
	// to specify the age after which an image will be garbage collected.
	ImageMaximumGCAge featuregate.Feature = "ImageMaximumGCAge"

	// owner: @saschagrunert
	//
	// Enables user namespace support for Pod Security Standards. Enabling this
	// feature will modify all Pod Security Standard rules to allow setting:
	// spec[.*].securityContext.[runAsNonRoot,runAsUser]
	// This feature gate should only be enabled if all nodes in the cluster
	// support the user namespace feature and have it enabled. The feature gate
	// will not graduate or be enabled by default in future Kubernetes
	// releases.
	UserNamespacesPodSecurityStandards featuregate.Feature = "UserNamespacesPodSecurityStandards"

	// owner: @jsafrane
	// kep: https://kep.k8s.io/1710
	// Speed up container startup by mounting volumes with the correct SELinux label
	// instead of changing each file on the volumes recursively.
	SELinuxMount featuregate.Feature = "SELinuxMount"

	// owner: @everpeace
	// kep: https://kep.k8s.io/3619
	//
	// Enable SupplementalGroupsPolicy feature in PodSecurityContext
	SupplementalGroupsPolicy featuregate.Feature = "SupplementalGroupsPolicy"

	// owner: @saschagrunert
	// kep: https://kep.k8s.io/4639
	//
	// Enables the image volume source.
	ImageVolume featuregate.Feature = "ImageVolume"

	// owner: @zhifei92
	// beta: v1.32
	//
	// Enables the systemd watchdog for the kubelet. When enabled, the kubelet will
	// periodically notify the systemd watchdog to indicate that it is still alive.
	// This can help prevent the system from restarting the kubelet if it becomes
	// unresponsive. The feature gate is enabled by default, but should only be used
	// if the system supports the systemd watchdog feature and has it configured properly.
	SystemdWatchdog = featuregate.Feature("SystemdWatchdog")

	// owner: @jsafrane
	// kep: https://kep.k8s.io/1710
	// alpha: v1.32
	//
	// Speed up container startup by mounting volumes with the correct SELinux label
	// instead of changing each file on the volumes recursively.
	// Enables the SELinuxChangePolicy field in PodSecurityContext before SELinuxMount featgure gate is enabled.
	SELinuxChangePolicy featuregate.Feature = "SELinuxChangePolicy"

	// owner: @HarshalNeelkamal
	// alpha: v1.32
	//
	// Enables external service account JWT signing and key management.
	// If enabled, it allows passing --service-account-signing-endpoint flag to configure external signer.
	ExternalServiceAccountTokenSigner featuregate.Feature = "ExternalServiceAccountTokenSigner"

	// owner: @ndixita
	// key: https://kep.k8s.io/2837
	// alpha: 1.32
	//
	// Enables specifying resources at pod-level.
	PodLevelResources featuregate.Feature = "PodLevelResources"
)

func init() {
	runtime.Must(utilfeature.DefaultMutableFeatureGate.Add(defaultKubernetesFeatureGates))
	runtime.Must(utilfeature.DefaultMutableFeatureGate.AddVersioned(defaultVersionedKubernetesFeatureGates))
	runtime.Must(zpagesfeatures.AddFeatureGates(utilfeature.DefaultMutableFeatureGate))

	// Register all client-go features with kube's feature gate instance and make all client-go
	// feature checks use kube's instance. The effect is that for kube binaries, client-go
	// features are wired to the existing --feature-gates flag just as all other features
	// are. Further, client-go features automatically support the existing mechanisms for
	// feature enablement metrics and test overrides.
	ca := &clientAdapter{utilfeature.DefaultMutableFeatureGate}
	runtime.Must(clientfeatures.AddFeaturesToExistingFeatureGates(ca))
	clientfeatures.ReplaceFeatureGates(ca)
}

// defaultKubernetesFeatureGates consists of legacy unversioned Kubernetes-specific feature keys.
// Please do not add to this file and use pkg/features/versioned_kube_features.go instead.
//
// Entries are separated from each other with blank lines to avoid sweeping gofmt changes
// when adding or removing one entry.
var defaultKubernetesFeatureGates = map[featuregate.Feature]featuregate.FeatureSpec{}
