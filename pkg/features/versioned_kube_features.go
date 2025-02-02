/*
Copyright 2024 The Kubernetes Authors.

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
	apiextensionsfeatures "k8s.io/apiextensions-apiserver/pkg/features"
	"k8s.io/apimachinery/pkg/util/version"
	genericfeatures "k8s.io/apiserver/pkg/features"
	"k8s.io/component-base/featuregate"
	zpagesfeatures "k8s.io/component-base/zpages/features"
	kcmfeatures "k8s.io/controller-manager/pkg/features"
)

// defaultVersionedKubernetesFeatureGates consists of all known Kubernetes-specific feature keys with VersionedSpecs.
// To add a new feature, define a key for it in pkg/features/kube_features.go and add it here. The features will be
// available throughout Kubernetes binaries.
// For features available via specific kubernetes components like apiserver,
// cloud-controller-manager, etc find the respective kube_features.go file
// (eg:staging/src/apiserver/pkg/features/kube_features.go), define the versioned
// feature gate there, and reference it in this file.
// To support n-3 compatibility version, features may only be removed 3 releases after graduation.
//
// Entries are alphabetized.
var defaultVersionedKubernetesFeatureGates = map[featuregate.Feature]featuregate.VersionedSpecs{
	AllowDNSOnlyNodeCSR: {
		{Version: version.MustParse("1.0"), Default: true, PreRelease: featuregate.GA},
		{Version: version.MustParse("1.31"), Default: false, PreRelease: featuregate.Deprecated},
	},

	AllowInsecureKubeletCertificateSigningRequests: {
		{Version: version.MustParse("1.0"), Default: true, PreRelease: featuregate.GA},
		{Version: version.MustParse("1.31"), Default: false, PreRelease: featuregate.Deprecated},
	},

	AllowOverwriteTerminationGracePeriodSeconds: {
		{Version: version.MustParse("1.0"), Default: true, PreRelease: featuregate.GA},
		{Version: version.MustParse("1.32"), Default: false, PreRelease: featuregate.Deprecated},
	},

	AllowServiceLBStatusOnNonLB: {
		{Version: version.MustParse("1.0"), Default: true, PreRelease: featuregate.GA},
		{Version: version.MustParse("1.29"), Default: false, PreRelease: featuregate.Deprecated},
		{Version: version.MustParse("1.32"), Default: false, PreRelease: featuregate.Deprecated, LockToDefault: true}, // remove in 1.35
	},

	AnyVolumeDataSource: {
		{Version: version.MustParse("1.18"), Default: false, PreRelease: featuregate.Alpha},
		{Version: version.MustParse("1.24"), Default: true, PreRelease: featuregate.Beta},
	},

	AppArmor: {
		{Version: version.MustParse("1.4"), Default: true, PreRelease: featuregate.Beta},
		{Version: version.MustParse("1.31"), Default: true, PreRelease: featuregate.GA, LockToDefault: true}, // remove in 1.33
	},

	AppArmorFields: {
		{Version: version.MustParse("1.30"), Default: true, PreRelease: featuregate.Beta},
		{Version: version.MustParse("1.31"), Default: true, PreRelease: featuregate.GA, LockToDefault: true}, // remove in 1.33
	},

	AuthorizeNodeWithSelectors: {
		{Version: version.MustParse("1.31"), Default: false, PreRelease: featuregate.Alpha},
		{Version: version.MustParse("1.32"), Default: true, PreRelease: featuregate.Beta},
	},

	kcmfeatures.CloudControllerManagerWebhook: {
		{Version: version.MustParse("1.27"), Default: false, PreRelease: featuregate.Alpha},
	},

	ClusterTrustBundle: {
		{Version: version.MustParse("1.27"), Default: false, PreRelease: featuregate.Alpha},
	},

	ClusterTrustBundleProjection: {
		{Version: version.MustParse("1.29"), Default: false, PreRelease: featuregate.Alpha},
	},

	ContainerCheckpoint: {
		{Version: version.MustParse("1.25"), Default: false, PreRelease: featuregate.Alpha},
		{Version: version.MustParse("1.30"), Default: true, PreRelease: featuregate.Beta},
	},

	CPUCFSQuotaPeriod: {
		{Version: version.MustParse("1.12"), Default: false, PreRelease: featuregate.Alpha},
	},

	CPUManager: {
		{Version: version.MustParse("1.8"), Default: false, PreRelease: featuregate.Alpha},
		{Version: version.MustParse("1.10"), Default: true, PreRelease: featuregate.Beta},
		{Version: version.MustParse("1.26"), Default: true, PreRelease: featuregate.GA, LockToDefault: true}, // GA in 1.26
	},

	CPUManagerPolicyAlphaOptions: {
		{Version: version.MustParse("1.23"), Default: false, PreRelease: featuregate.Alpha},
	},

	CPUManagerPolicyBetaOptions: {
		{Version: version.MustParse("1.23"), Default: true, PreRelease: featuregate.Beta},
	},

	CPUManagerPolicyOptions: {
		{Version: version.MustParse("1.22"), Default: false, PreRelease: featuregate.Alpha},
		{Version: version.MustParse("1.23"), Default: true, PreRelease: featuregate.Beta},
	},

	CronJobsScheduledAnnotation: {
		{Version: version.MustParse("1.28"), Default: true, PreRelease: featuregate.Beta},
		{Version: version.MustParse("1.32"), Default: true, PreRelease: featuregate.GA, LockToDefault: true}, // remove in 1.35
	},

	// inherited features from apiextensions-apiserver, relisted here to get a conflict if it is changed
	// unintentionally on either side:
	apiextensionsfeatures.CRDValidationRatcheting: {
		{Version: version.MustParse("1.28"), Default: false, PreRelease: featuregate.Alpha},
		{Version: version.MustParse("1.30"), Default: true, PreRelease: featuregate.Beta},
	},

	CrossNamespaceVolumeDataSource: {
		{Version: version.MustParse("1.26"), Default: false, PreRelease: featuregate.Alpha},
	},

	CSIMigrationPortworx: {
		{Version: version.MustParse("1.23"), Default: false, PreRelease: featuregate.Alpha},
		{Version: version.MustParse("1.25"), Default: false, PreRelease: featuregate.Beta},
		{Version: version.MustParse("1.31"), Default: true, PreRelease: featuregate.Beta}, // On by default (requires Portworx CSI driver)
	},

	CSIVolumeHealth: {
		{Version: version.MustParse("1.21"), Default: false, PreRelease: featuregate.Alpha},
	},

	// inherited features from apiextensions-apiserver, relisted here to get a conflict if it is changed
	// unintentionally on either side:
	apiextensionsfeatures.CustomResourceFieldSelectors: {
		{Version: version.MustParse("1.30"), Default: false, PreRelease: featuregate.Alpha},
		{Version: version.MustParse("1.31"), Default: true, PreRelease: featuregate.Beta},
		{Version: version.MustParse("1.32"), Default: true, LockToDefault: true, PreRelease: featuregate.GA},
	},

	DevicePluginCDIDevices: {
		{Version: version.MustParse("1.28"), Default: false, PreRelease: featuregate.Alpha},
		{Version: version.MustParse("1.29"), Default: true, PreRelease: featuregate.Beta},
		{Version: version.MustParse("1.31"), Default: true, PreRelease: featuregate.GA, LockToDefault: true}, // remove in 1.33
	},

	DisableAllocatorDualWrite: {
		{Version: version.MustParse("1.31"), Default: false, PreRelease: featuregate.Alpha}, // remove after MultiCIDRServiceAllocator is GA
	},

	DisableCloudProviders: {
		{Version: version.MustParse("1.22"), Default: false, PreRelease: featuregate.Alpha},
		{Version: version.MustParse("1.29"), Default: true, PreRelease: featuregate.Beta},
		{Version: version.MustParse("1.31"), Default: true, PreRelease: featuregate.GA, LockToDefault: true},
	},

	DisableKubeletCloudCredentialProviders: {
		{Version: version.MustParse("1.23"), Default: false, PreRelease: featuregate.Alpha},
		{Version: version.MustParse("1.29"), Default: true, PreRelease: featuregate.Beta},
		{Version: version.MustParse("1.31"), Default: true, PreRelease: featuregate.GA, LockToDefault: true},
	},

	DisableNodeKubeProxyVersion: {
		{Version: version.MustParse("1.29"), Default: false, PreRelease: featuregate.Alpha},
		{Version: version.MustParse("1.31"), Default: false, PreRelease: featuregate.Deprecated},
	},

	DRAAdminAccess: {
		{Version: version.MustParse("1.32"), Default: false, PreRelease: featuregate.Alpha},
	},

	DynamicResourceAllocation: {
		{Version: version.MustParse("1.26"), Default: false, PreRelease: featuregate.Alpha},
		{Version: version.MustParse("1.32"), Default: false, PreRelease: featuregate.Beta},
	},

	DRAResourceClaimDeviceStatus: {
		{Version: version.MustParse("1.32"), Default: false, PreRelease: featuregate.Alpha},
	},

	KubeletCrashLoopBackOffMax: {
		{Version: version.MustParse("1.32"), Default: false, PreRelease: featuregate.Alpha},
	},

	ElasticIndexedJob: {
		{Version: version.MustParse("1.27"), Default: true, PreRelease: featuregate.Beta},
		{Version: version.MustParse("1.31"), Default: true, PreRelease: featuregate.GA, LockToDefault: true}, // GA in 1.31, remove in 1.32
	},

	EventedPLEG: {
		{Version: version.MustParse("1.26"), Default: false, PreRelease: featuregate.Alpha},
	},

	ExecProbeTimeout: {
		{Version: version.MustParse("1.20"), Default: true, PreRelease: featuregate.GA}, // lock to default and remove after v1.22 based on KEP #1972 update
	},

	ExternalServiceAccountTokenSigner: {
		{Version: version.MustParse("1.32"), Default: false, PreRelease: featuregate.Alpha},
	},

	genericfeatures.AdmissionWebhookMatchConditions: {
		{Version: version.MustParse("1.27"), Default: false, PreRelease: featuregate.Alpha},
		{Version: version.MustParse("1.28"), Default: true, PreRelease: featuregate.Beta},
		{Version: version.MustParse("1.30"), Default: true, PreRelease: featuregate.GA, LockToDefault: true},
	},

	genericfeatures.AggregatedDiscoveryEndpoint: {
		{Version: version.MustParse("1.26"), Default: false, PreRelease: featuregate.Alpha},
		{Version: version.MustParse("1.27"), Default: true, PreRelease: featuregate.Beta},
		{Version: version.MustParse("1.30"), Default: true, PreRelease: featuregate.GA, LockToDefault: true},
	},

	genericfeatures.AllowParsingUserUIDFromCertAuth: {
		{Version: version.MustParse("1.33"), Default: true, PreRelease: featuregate.Beta},
	},

	genericfeatures.AllowUnsafeMalformedObjectDeletion: {
		{Version: version.MustParse("1.32"), Default: false, PreRelease: featuregate.Alpha},
	},

	genericfeatures.AnonymousAuthConfigurableEndpoints: {
		{Version: version.MustParse("1.31"), Default: false, PreRelease: featuregate.Alpha},
		{Version: version.MustParse("1.32"), Default: true, PreRelease: featuregate.Beta},
	},

	genericfeatures.APIListChunking: {
		{Version: version.MustParse("1.8"), Default: false, PreRelease: featuregate.Alpha},
		{Version: version.MustParse("1.9"), Default: true, PreRelease: featuregate.Beta},
		{Version: version.MustParse("1.29"), Default: true, PreRelease: featuregate.GA, LockToDefault: true},
	},

	genericfeatures.APIResponseCompression: {
		{Version: version.MustParse("1.8"), Default: false, PreRelease: featuregate.Alpha},
		{Version: version.MustParse("1.16"), Default: true, PreRelease: featuregate.Beta},
	},

	genericfeatures.APIServerIdentity: {
		{Version: version.MustParse("1.20"), Default: false, PreRelease: featuregate.Alpha},
		{Version: version.MustParse("1.26"), Default: true, PreRelease: featuregate.Beta},
	},

	genericfeatures.APIServerTracing: {
		{Version: version.MustParse("1.22"), Default: false, PreRelease: featuregate.Alpha},
		{Version: version.MustParse("1.27"), Default: true, PreRelease: featuregate.Beta},
	},

	genericfeatures.APIServingWithRoutine: {
		{Version: version.MustParse("1.30"), Default: false, PreRelease: featuregate.Alpha},
	},

	genericfeatures.AuthorizeWithSelectors: {
		{Version: version.MustParse("1.31"), Default: false, PreRelease: featuregate.Alpha},
		{Version: version.MustParse("1.32"), Default: true, PreRelease: featuregate.Beta},
	},

	genericfeatures.BtreeWatchCache: {
		{Version: version.MustParse("1.32"), Default: true, PreRelease: featuregate.Beta},
	},

	genericfeatures.CBORServingAndStorage: {
		{Version: version.MustParse("1.32"), Default: false, PreRelease: featuregate.Alpha},
	},

	genericfeatures.ConcurrentWatchObjectDecode: {
		{Version: version.MustParse("1.31"), Default: false, PreRelease: featuregate.Beta},
	},

	genericfeatures.ConsistentListFromCache: {
		{Version: version.MustParse("1.28"), Default: false, PreRelease: featuregate.Alpha},
		{Version: version.MustParse("1.31"), Default: true, PreRelease: featuregate.Beta},
	},

	genericfeatures.CoordinatedLeaderElection: {
		{Version: version.MustParse("1.31"), Default: false, PreRelease: featuregate.Alpha},
	},

	genericfeatures.EfficientWatchResumption: {
		{Version: version.MustParse("1.20"), Default: false, PreRelease: featuregate.Alpha},
		{Version: version.MustParse("1.21"), Default: true, PreRelease: featuregate.Beta},
		{Version: version.MustParse("1.24"), Default: true, PreRelease: featuregate.GA, LockToDefault: true},
	},

	genericfeatures.KMSv1: {
		{Version: version.MustParse("1.0"), Default: true, PreRelease: featuregate.GA},
		{Version: version.MustParse("1.28"), Default: true, PreRelease: featuregate.Deprecated},
		{Version: version.MustParse("1.29"), Default: false, PreRelease: featuregate.Deprecated},
	},

	genericfeatures.MutatingAdmissionPolicy: {
		{Version: version.MustParse("1.32"), Default: false, PreRelease: featuregate.Alpha},
	},

	genericfeatures.OpenAPIEnums: {
		{Version: version.MustParse("1.23"), Default: false, PreRelease: featuregate.Alpha},
		{Version: version.MustParse("1.24"), Default: true, PreRelease: featuregate.Beta},
	},

	genericfeatures.RemainingItemCount: {
		{Version: version.MustParse("1.15"), Default: false, PreRelease: featuregate.Alpha},
		{Version: version.MustParse("1.16"), Default: true, PreRelease: featuregate.Beta},
		{Version: version.MustParse("1.29"), Default: true, PreRelease: featuregate.GA, LockToDefault: true},
	},

	genericfeatures.RemoteRequestHeaderUID: {
		{Version: version.MustParse("1.32"), Default: false, PreRelease: featuregate.Alpha},
	},

	genericfeatures.ResilientWatchCacheInitialization: {
		{Version: version.MustParse("1.31"), Default: true, PreRelease: featuregate.Beta},
	},

	genericfeatures.RetryGenerateName: {
		{Version: version.MustParse("1.30"), Default: false, PreRelease: featuregate.Alpha},
		{Version: version.MustParse("1.31"), Default: true, PreRelease: featuregate.Beta},
		{Version: version.MustParse("1.32"), Default: true, LockToDefault: true, PreRelease: featuregate.GA},
	},

	genericfeatures.SeparateCacheWatchRPC: {
		{Version: version.MustParse("1.28"), Default: true, PreRelease: featuregate.Beta},
	},

	genericfeatures.StorageVersionAPI: {
		{Version: version.MustParse("1.20"), Default: false, PreRelease: featuregate.Alpha},
	},

	genericfeatures.StorageVersionHash: {
		{Version: version.MustParse("1.14"), Default: false, PreRelease: featuregate.Alpha},
		{Version: version.MustParse("1.15"), Default: true, PreRelease: featuregate.Beta},
	},

	genericfeatures.StrictCostEnforcementForVAP: {
		{Version: version.MustParse("1.30"), Default: false, PreRelease: featuregate.Beta},
		{Version: version.MustParse("1.32"), Default: true, PreRelease: featuregate.GA, LockToDefault: true},
	},

	genericfeatures.StrictCostEnforcementForWebhooks: {
		{Version: version.MustParse("1.30"), Default: false, PreRelease: featuregate.Beta},
		{Version: version.MustParse("1.32"), Default: true, PreRelease: featuregate.GA, LockToDefault: true},
	},

	genericfeatures.StructuredAuthenticationConfiguration: {
		{Version: version.MustParse("1.29"), Default: false, PreRelease: featuregate.Alpha},
		{Version: version.MustParse("1.30"), Default: true, PreRelease: featuregate.Beta},
	},

	genericfeatures.StructuredAuthorizationConfiguration: {
		{Version: version.MustParse("1.29"), Default: false, PreRelease: featuregate.Alpha},
		{Version: version.MustParse("1.30"), Default: true, PreRelease: featuregate.Beta},
		{Version: version.MustParse("1.32"), Default: true, PreRelease: featuregate.GA, LockToDefault: true},
	},

	genericfeatures.UnauthenticatedHTTP2DOSMitigation: {
		{Version: version.MustParse("1.25"), Default: false, PreRelease: featuregate.Beta},
		{Version: version.MustParse("1.29"), Default: true, PreRelease: featuregate.Beta},
	},

	genericfeatures.WatchCacheInitializationPostStartHook: {
		{Version: version.MustParse("1.31"), Default: false, PreRelease: featuregate.Beta},
	},

	genericfeatures.WatchFromStorageWithoutResourceVersion: {
		{Version: version.MustParse("1.27"), Default: false, PreRelease: featuregate.Beta},
	},

	genericfeatures.WatchList: {
		{Version: version.MustParse("1.27"), Default: false, PreRelease: featuregate.Alpha},
		{Version: version.MustParse("1.32"), Default: true, PreRelease: featuregate.Beta},
	},

	GracefulNodeShutdown: {
		{Version: version.MustParse("1.20"), Default: false, PreRelease: featuregate.Alpha},
		{Version: version.MustParse("1.21"), Default: true, PreRelease: featuregate.Beta},
	},

	GracefulNodeShutdownBasedOnPodPriority: {
		{Version: version.MustParse("1.23"), Default: false, PreRelease: featuregate.Alpha},
		{Version: version.MustParse("1.24"), Default: true, PreRelease: featuregate.Beta},
	},

	HonorPVReclaimPolicy: {
		{Version: version.MustParse("1.23"), Default: false, PreRelease: featuregate.Alpha},
		{Version: version.MustParse("1.31"), Default: true, PreRelease: featuregate.Beta},
	},

	HPAScaleToZero: {
		{Version: version.MustParse("1.16"), Default: false, PreRelease: featuregate.Alpha},
	},

	ImageMaximumGCAge: {
		{Version: version.MustParse("1.29"), Default: false, PreRelease: featuregate.Alpha},
		{Version: version.MustParse("1.30"), Default: true, PreRelease: featuregate.Beta},
	},

	ImageVolume: {
		{Version: version.MustParse("1.31"), Default: false, PreRelease: featuregate.Alpha},
	},

	InPlacePodVerticalScaling: {
		{Version: version.MustParse("1.27"), Default: false, PreRelease: featuregate.Alpha},
	},

	InPlacePodVerticalScalingAllocatedStatus: {
		{Version: version.MustParse("1.32"), Default: false, PreRelease: featuregate.Alpha},
	},

	InPlacePodVerticalScalingExclusiveCPUs: {
		{Version: version.MustParse("1.32"), Default: false, PreRelease: featuregate.Alpha},
	},

	InTreePluginPortworxUnregister: {
		{Version: version.MustParse("1.23"), Default: false, PreRelease: featuregate.Alpha},
	},

	JobBackoffLimitPerIndex: {
		{Version: version.MustParse("1.28"), Default: false, PreRelease: featuregate.Alpha},
		{Version: version.MustParse("1.29"), Default: true, PreRelease: featuregate.Beta},
	},

	JobManagedBy: {
		{Version: version.MustParse("1.30"), Default: false, PreRelease: featuregate.Alpha},
		{Version: version.MustParse("1.32"), Default: true, PreRelease: featuregate.Beta},
	},

	JobPodFailurePolicy: {
		{Version: version.MustParse("1.25"), Default: false, PreRelease: featuregate.Alpha},
		{Version: version.MustParse("1.26"), Default: true, PreRelease: featuregate.Beta},
		{Version: version.MustParse("1.31"), Default: true, PreRelease: featuregate.GA, LockToDefault: true}, // remove in 1.33
	},

	JobPodReplacementPolicy: {
		{Version: version.MustParse("1.28"), Default: false, PreRelease: featuregate.Alpha},
		{Version: version.MustParse("1.29"), Default: true, PreRelease: featuregate.Beta},
	},

	JobSuccessPolicy: {
		{Version: version.MustParse("1.30"), Default: false, PreRelease: featuregate.Alpha},
		{Version: version.MustParse("1.31"), Default: true, PreRelease: featuregate.Beta},
	},

	KubeletCgroupDriverFromCRI: {
		{Version: version.MustParse("1.28"), Default: false, PreRelease: featuregate.Alpha},
		{Version: version.MustParse("1.31"), Default: true, PreRelease: featuregate.Beta},
	},

	KubeletFineGrainedAuthz: {
		{Version: version.MustParse("1.32"), Default: false, PreRelease: featuregate.Alpha},
	},

	KubeletInUserNamespace: {
		{Version: version.MustParse("1.22"), Default: false, PreRelease: featuregate.Alpha},
	},

	KubeletPodResourcesDynamicResources: {
		{Version: version.MustParse("1.27"), Default: false, PreRelease: featuregate.Alpha},
	},

	KubeletPodResourcesGet: {
		{Version: version.MustParse("1.27"), Default: false, PreRelease: featuregate.Alpha},
	},

	KubeletRegistrationGetOnExistsOnly: {
		{Version: version.MustParse("1.0"), Default: true, PreRelease: featuregate.GA},
		{Version: version.MustParse("1.32"), Default: false, PreRelease: featuregate.Deprecated},
	},

	KubeletSeparateDiskGC: {
		{Version: version.MustParse("1.29"), Default: false, PreRelease: featuregate.Alpha},
		{Version: version.MustParse("1.31"), Default: true, PreRelease: featuregate.Beta},
	},

	KubeletTracing: {
		{Version: version.MustParse("1.25"), Default: false, PreRelease: featuregate.Alpha},
		{Version: version.MustParse("1.27"), Default: true, PreRelease: featuregate.Beta},
	},

	KubeProxyDrainingTerminatingNodes: {
		{Version: version.MustParse("1.28"), Default: false, PreRelease: featuregate.Alpha},
		{Version: version.MustParse("1.30"), Default: true, PreRelease: featuregate.Beta},
		{Version: version.MustParse("1.31"), Default: true, PreRelease: featuregate.GA, LockToDefault: true}, // GA in 1.31; remove in 1.33
	},

	LoadBalancerIPMode: {
		{Version: version.MustParse("1.29"), Default: false, PreRelease: featuregate.Alpha},
		{Version: version.MustParse("1.30"), Default: true, PreRelease: featuregate.Beta},
		{Version: version.MustParse("1.32"), Default: true, PreRelease: featuregate.GA, LockToDefault: true},
	},

	LocalStorageCapacityIsolationFSQuotaMonitoring: {
		{Version: version.MustParse("1.15"), Default: false, PreRelease: featuregate.Alpha},
		{Version: version.MustParse("1.31"), Default: false, PreRelease: featuregate.Beta},
	},

	LogarithmicScaleDown: {
		{Version: version.MustParse("1.21"), Default: false, PreRelease: featuregate.Alpha},
		{Version: version.MustParse("1.22"), Default: true, PreRelease: featuregate.Beta},
		{Version: version.MustParse("1.31"), Default: true, PreRelease: featuregate.GA, LockToDefault: true},
	},

	MatchLabelKeysInPodAffinity: {
		{Version: version.MustParse("1.29"), Default: false, PreRelease: featuregate.Alpha},
		{Version: version.MustParse("1.31"), Default: true, PreRelease: featuregate.Beta},
	},

	MatchLabelKeysInPodTopologySpread: {
		{Version: version.MustParse("1.25"), Default: false, PreRelease: featuregate.Alpha},
		{Version: version.MustParse("1.27"), Default: true, PreRelease: featuregate.Beta},
	},

	MaxUnavailableStatefulSet: {
		{Version: version.MustParse("1.24"), Default: false, PreRelease: featuregate.Alpha},
	},

	MemoryManager: {
		{Version: version.MustParse("1.21"), Default: false, PreRelease: featuregate.Alpha},
		{Version: version.MustParse("1.22"), Default: true, PreRelease: featuregate.Beta},
		{Version: version.MustParse("1.32"), Default: true, PreRelease: featuregate.GA, LockToDefault: true},
	},

	MemoryQoS: {
		{Version: version.MustParse("1.22"), Default: false, PreRelease: featuregate.Alpha},
	},

	MultiCIDRServiceAllocator: {
		{Version: version.MustParse("1.27"), Default: false, PreRelease: featuregate.Alpha},
		{Version: version.MustParse("1.31"), Default: false, PreRelease: featuregate.Beta},
	},

	NFTablesProxyMode: {
		{Version: version.MustParse("1.29"), Default: false, PreRelease: featuregate.Alpha},
		{Version: version.MustParse("1.31"), Default: true, PreRelease: featuregate.Beta},
	},

	NodeInclusionPolicyInPodTopologySpread: {
		{Version: version.MustParse("1.25"), Default: false, PreRelease: featuregate.Alpha},
		{Version: version.MustParse("1.26"), Default: true, PreRelease: featuregate.Beta},
	},

	NodeLogQuery: {
		{Version: version.MustParse("1.27"), Default: false, PreRelease: featuregate.Alpha},
		{Version: version.MustParse("1.30"), Default: false, PreRelease: featuregate.Beta},
	},

	NodeSwap: {
		{Version: version.MustParse("1.22"), Default: false, PreRelease: featuregate.Alpha},
		{Version: version.MustParse("1.28"), Default: false, PreRelease: featuregate.Beta},
		{Version: version.MustParse("1.30"), Default: true, PreRelease: featuregate.Beta},
	},

	PersistentVolumeLastPhaseTransitionTime: {
		{Version: version.MustParse("1.28"), Default: false, PreRelease: featuregate.Alpha},
		{Version: version.MustParse("1.29"), Default: true, PreRelease: featuregate.Beta},
		{Version: version.MustParse("1.31"), Default: true, PreRelease: featuregate.GA, LockToDefault: true}, // remove in 1.33
	},

	PodAndContainerStatsFromCRI: {
		{Version: version.MustParse("1.23"), Default: false, PreRelease: featuregate.Alpha},
	},

	PodDeletionCost: {
		{Version: version.MustParse("1.21"), Default: false, PreRelease: featuregate.Alpha},
		{Version: version.MustParse("1.22"), Default: true, PreRelease: featuregate.Beta},
	},

	PodDisruptionConditions: {
		{Version: version.MustParse("1.25"), Default: false, PreRelease: featuregate.Alpha},
		{Version: version.MustParse("1.26"), Default: true, PreRelease: featuregate.Beta},
		{Version: version.MustParse("1.31"), Default: true, PreRelease: featuregate.GA, LockToDefault: true}, // remove in 1.33
	},

	PodIndexLabel: {
		{Version: version.MustParse("1.28"), Default: true, PreRelease: featuregate.Beta},
		{Version: version.MustParse("1.32"), Default: true, PreRelease: featuregate.GA, LockToDefault: true}, // remove in 1.35
	},

	PodLevelResources: {
		{Version: version.MustParse("1.32"), Default: false, PreRelease: featuregate.Alpha},
	},

	PodLifecycleSleepAction: {
		{Version: version.MustParse("1.29"), Default: false, PreRelease: featuregate.Alpha},
		{Version: version.MustParse("1.30"), Default: true, PreRelease: featuregate.Beta},
	},
	PodReadyToStartContainersCondition: {
		{Version: version.MustParse("1.28"), Default: false, PreRelease: featuregate.Alpha},
		{Version: version.MustParse("1.29"), Default: true, PreRelease: featuregate.Beta},
	},
	PodLifecycleSleepActionAllowZero: {
		{Version: version.MustParse("1.32"), Default: false, PreRelease: featuregate.Alpha},
	},
	PodSchedulingReadiness: {
		{Version: version.MustParse("1.26"), Default: false, PreRelease: featuregate.Alpha},
		{Version: version.MustParse("1.27"), Default: true, PreRelease: featuregate.Beta},
		{Version: version.MustParse("1.30"), Default: true, PreRelease: featuregate.GA, LockToDefault: true}, // GA in 1.30; remove in 1.32
	},

	PortForwardWebsockets: {
		{Version: version.MustParse("1.30"), Default: false, PreRelease: featuregate.Alpha},
		{Version: version.MustParse("1.31"), Default: true, PreRelease: featuregate.Beta},
	},

	ProcMountType: {
		{Version: version.MustParse("1.12"), Default: false, PreRelease: featuregate.Alpha},
		{Version: version.MustParse("1.31"), Default: false, PreRelease: featuregate.Beta},
	},

	QOSReserved: {
		{Version: version.MustParse("1.11"), Default: false, PreRelease: featuregate.Alpha},
	},

	RecoverVolumeExpansionFailure: {
		{Version: version.MustParse("1.23"), Default: false, PreRelease: featuregate.Alpha},
		{Version: version.MustParse("1.32"), Default: true, PreRelease: featuregate.Beta},
	},
	RecursiveReadOnlyMounts: {
		{Version: version.MustParse("1.30"), Default: false, PreRelease: featuregate.Alpha},
		{Version: version.MustParse("1.31"), Default: true, PreRelease: featuregate.Beta},
	},

	RelaxedDNSSearchValidation: {
		{Version: version.MustParse("1.32"), Default: false, PreRelease: featuregate.Alpha},
	},

	RelaxedEnvironmentVariableValidation: {
		{Version: version.MustParse("1.30"), Default: false, PreRelease: featuregate.Alpha},
		{Version: version.MustParse("1.32"), Default: true, PreRelease: featuregate.Beta},
	},

	ReloadKubeletServerCertificateFile: {
		{Version: version.MustParse("1.31"), Default: true, PreRelease: featuregate.Beta},
	},

	ResourceHealthStatus: {
		{Version: version.MustParse("1.31"), Default: false, PreRelease: featuregate.Alpha},
	},

	RotateKubeletServerCertificate: {
		{Version: version.MustParse("1.7"), Default: false, PreRelease: featuregate.Alpha},
		{Version: version.MustParse("1.12"), Default: true, PreRelease: featuregate.Beta},
	},

	RuntimeClassInImageCriAPI: {
		{Version: version.MustParse("1.29"), Default: false, PreRelease: featuregate.Alpha},
	},

	SchedulerAsyncPreemption: {
		{Version: version.MustParse("1.32"), Default: false, PreRelease: featuregate.Alpha},
	},

	SchedulerQueueingHints: {
		{Version: version.MustParse("1.28"), Default: false, PreRelease: featuregate.Beta},
		{Version: version.MustParse("1.32"), Default: true, PreRelease: featuregate.Beta},
	},

	SELinuxChangePolicy: {
		{Version: version.MustParse("1.32"), Default: false, PreRelease: featuregate.Alpha},
	},

	SELinuxMount: {
		{Version: version.MustParse("1.30"), Default: false, PreRelease: featuregate.Alpha},
	},

	SELinuxMountReadWriteOncePod: {
		{Version: version.MustParse("1.25"), Default: false, PreRelease: featuregate.Alpha},
		{Version: version.MustParse("1.27"), Default: false, PreRelease: featuregate.Beta},
		{Version: version.MustParse("1.28"), Default: true, PreRelease: featuregate.Beta},
	},

	SeparateTaintEvictionController: {
		{Version: version.MustParse("1.29"), Default: true, PreRelease: featuregate.Beta},
	},

	StorageNamespaceIndex: {
		{Version: version.MustParse("1.30"), Default: true, PreRelease: featuregate.Beta},
	},

	ServiceAccountNodeAudienceRestriction: {
		{Version: version.MustParse("1.32"), Default: true, PreRelease: featuregate.Beta},
	},

	ServiceAccountTokenJTI: {
		{Version: version.MustParse("1.29"), Default: false, PreRelease: featuregate.Alpha},
		{Version: version.MustParse("1.30"), Default: true, PreRelease: featuregate.Beta},
		{Version: version.MustParse("1.32"), Default: true, PreRelease: featuregate.GA, LockToDefault: true},
	},

	ServiceAccountTokenNodeBinding: {
		{Version: version.MustParse("1.29"), Default: false, PreRelease: featuregate.Alpha},
		{Version: version.MustParse("1.31"), Default: true, PreRelease: featuregate.Beta},
		{Version: version.MustParse("1.33"), Default: true, PreRelease: featuregate.GA, LockToDefault: true},
	},

	ServiceAccountTokenNodeBindingValidation: {
		{Version: version.MustParse("1.29"), Default: false, PreRelease: featuregate.Alpha},
		{Version: version.MustParse("1.30"), Default: true, PreRelease: featuregate.Beta},
		{Version: version.MustParse("1.32"), Default: true, PreRelease: featuregate.GA, LockToDefault: true},
	},

	ServiceAccountTokenPodNodeInfo: {
		{Version: version.MustParse("1.29"), Default: false, PreRelease: featuregate.Alpha},
		{Version: version.MustParse("1.30"), Default: true, PreRelease: featuregate.Beta},
		{Version: version.MustParse("1.32"), Default: true, PreRelease: featuregate.GA, LockToDefault: true},
	},

	ServiceTrafficDistribution: {
		{Version: version.MustParse("1.30"), Default: false, PreRelease: featuregate.Alpha},
		{Version: version.MustParse("1.31"), Default: true, PreRelease: featuregate.Beta},
	},

	SidecarContainers: {
		{Version: version.MustParse("1.28"), Default: false, PreRelease: featuregate.Alpha},
		{Version: version.MustParse("1.29"), Default: true, PreRelease: featuregate.Beta},
	},

	SizeMemoryBackedVolumes: {
		{Version: version.MustParse("1.20"), Default: false, PreRelease: featuregate.Alpha},
		{Version: version.MustParse("1.22"), Default: true, PreRelease: featuregate.Beta},
		{Version: version.MustParse("1.32"), Default: true, LockToDefault: true, PreRelease: featuregate.GA},
	},

	PodLogsQuerySplitStreams: {
		{Version: version.MustParse("1.32"), Default: false, PreRelease: featuregate.Alpha},
	},

	StatefulSetAutoDeletePVC: {
		{Version: version.MustParse("1.23"), Default: false, PreRelease: featuregate.Alpha},
		{Version: version.MustParse("1.27"), Default: true, PreRelease: featuregate.Beta},
		{Version: version.MustParse("1.32"), Default: true, PreRelease: featuregate.GA, LockToDefault: true}, // GA in 1.32, remove in 1.35
	},

	StatefulSetStartOrdinal: {
		{Version: version.MustParse("1.26"), Default: false, PreRelease: featuregate.Alpha},
		{Version: version.MustParse("1.27"), Default: true, PreRelease: featuregate.Beta},
		{Version: version.MustParse("1.31"), Default: true, PreRelease: featuregate.GA, LockToDefault: true}, // GA in 1.31, remove in 1.33
	},

	StorageVersionMigrator: {
		{Version: version.MustParse("1.30"), Default: false, PreRelease: featuregate.Alpha},
	},

	SupplementalGroupsPolicy: {
		{Version: version.MustParse("1.31"), Default: false, PreRelease: featuregate.Alpha},
	},

	SystemdWatchdog: {
		{Version: version.MustParse("1.32"), Default: true, PreRelease: featuregate.Beta},
	},

	TopologyAwareHints: {
		{Version: version.MustParse("1.21"), Default: false, PreRelease: featuregate.Alpha},
		{Version: version.MustParse("1.23"), Default: false, PreRelease: featuregate.Beta},
		{Version: version.MustParse("1.24"), Default: true, PreRelease: featuregate.Beta},
	},

	TopologyManagerPolicyAlphaOptions: {
		{Version: version.MustParse("1.26"), Default: false, PreRelease: featuregate.Alpha},
	},

	TopologyManagerPolicyBetaOptions: {
		{Version: version.MustParse("1.26"), Default: false, PreRelease: featuregate.Beta},
		{Version: version.MustParse("1.28"), Default: true, PreRelease: featuregate.Beta},
	},

	TopologyManagerPolicyOptions: {
		{Version: version.MustParse("1.26"), Default: false, PreRelease: featuregate.Alpha},
		{Version: version.MustParse("1.28"), Default: true, PreRelease: featuregate.Beta},
		{Version: version.MustParse("1.32"), Default: true, PreRelease: featuregate.GA},
	},

	TranslateStreamCloseWebsocketRequests: {
		{Version: version.MustParse("1.29"), Default: false, PreRelease: featuregate.Alpha},
		{Version: version.MustParse("1.30"), Default: true, PreRelease: featuregate.Beta},
	},

	UnknownVersionInteroperabilityProxy: {
		{Version: version.MustParse("1.28"), Default: false, PreRelease: featuregate.Alpha},
	},

	UserNamespacesPodSecurityStandards: {
		{Version: version.MustParse("1.29"), Default: false, PreRelease: featuregate.Alpha},
	},

	UserNamespacesSupport: {
		{Version: version.MustParse("1.25"), Default: false, PreRelease: featuregate.Alpha},
		{Version: version.MustParse("1.30"), Default: false, PreRelease: featuregate.Beta},
	},

	VolumeAttributesClass: {
		{Version: version.MustParse("1.29"), Default: false, PreRelease: featuregate.Alpha},
		{Version: version.MustParse("1.31"), Default: false, PreRelease: featuregate.Beta},
	},

	VolumeCapacityPriority: {
		{Version: version.MustParse("1.21"), Default: false, PreRelease: featuregate.Alpha},
	},

	WinDSR: {
		{Version: version.MustParse("1.14"), Default: false, PreRelease: featuregate.Alpha},
	},
	WindowsGracefulNodeShutdown: {
		{Version: version.MustParse("1.32"), Default: false, PreRelease: featuregate.Alpha},
	},
	WinOverlay: {
		{Version: version.MustParse("1.14"), Default: false, PreRelease: featuregate.Alpha},
		{Version: version.MustParse("1.20"), Default: true, PreRelease: featuregate.Beta},
	},

	WindowsCPUAndMemoryAffinity: {
		{Version: version.MustParse("1.32"), Default: false, PreRelease: featuregate.Alpha},
	},

	WindowsHostNetwork: {
		{Version: version.MustParse("1.26"), Default: true, PreRelease: featuregate.Alpha},
	},

	zpagesfeatures.ComponentFlagz: {
		{Version: version.MustParse("1.32"), Default: false, PreRelease: featuregate.Alpha},
	},

	zpagesfeatures.ComponentStatusz: {
		{Version: version.MustParse("1.32"), Default: false, PreRelease: featuregate.Alpha},
	},
}
