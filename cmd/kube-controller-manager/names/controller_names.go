/*
Copyright 2023 The Kubernetes Authors.

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

package names

// Canonical controller names
//
// NAMING CONVENTIONS
// 1. naming should be consistent across the controllers
// 2. use of shortcuts should be avoided, unless they are well-known non-Kubernetes shortcuts
// 3. Kubernetes' resources should be written together without a hyphen ("-")
//
// CHANGE POLICY
// The controller names should be treated as IDs.
// They can only be changed if absolutely necessary. For example if an inappropriate name was chosen in the past, or if the scope of the controller changes.
// When a name is changed, the old name should be aliased in app.ControllerDescriptor#GetAliases, while preserving all old aliases.
// This is done to achieve backwards compatibility
//
// USE CASES
// The following places should use the controller name constants, when:
//  1. defining a new app.ControllerDescriptor so it can be used in app.NewControllerDescriptors or app.KnownControllers:
//  2. used anywhere inside the controller itself:
//     2.1. logging should use a canonical controller name when referencing a controller. Each controller gets instantiated with a context where the controller name is already included via WithName.
//     2.2. [TODO] emitted events should have an EventSource.Component set to the controller name (usually when initializing an EventRecorder)
//     2.3. [TODO] registering ControllerManagerMetrics with ControllerStarted and ControllerStopped
//     2.4. [TODO] calling WaitForNamedCacheSync
//  3. defining controller options for "--help" command or generated documentation
//     3.1. controller name should be used to create a pflag.FlagSet when registering controller options (the name is rendered in a controller flag group header) in options.KubeControllerManagerOptions
//     3.2. when defined flag's help mentions a controller name
//  4. defining a new service account for a new controller (old controllers may have inconsistent service accounts to stay backwards compatible)
const (
	ServiceAccountTokenController                      = "serviceaccount-token-controller"
	EndpointsController                                = "endpoints-controller"
	EndpointSliceController                            = "endpointslice-controller"
	EndpointSliceMirroringController                   = "endpointslice-mirroring-controller"
	ReplicationControllerController                    = "replicationcontroller-controller"
	PodGarbageCollectorController                      = "pod-garbage-collector-controller"
	ResourceQuotaController                            = "resourcequota-controller"
	NamespaceController                                = "namespace-controller"
	ServiceAccountController                           = "serviceaccount-controller"
	GarbageCollectorController                         = "garbage-collector-controller"
	DaemonSetController                                = "daemonset-controller"
	JobController                                      = "job-controller"
	DeploymentController                               = "deployment-controller"
	ReplicaSetController                               = "replicaset-controller"
	HorizontalPodAutoscalerController                  = "horizontal-pod-autoscaler-controller"
	DisruptionController                               = "disruption-controller"
	StatefulSetController                              = "statefulset-controller"
	CronJobController                                  = "cronjob-controller"
	CertificateSigningRequestSigningController         = "certificatesigningrequest-signing-controller"
	CertificateSigningRequestApprovingController       = "certificatesigningrequest-approving-controller"
	CertificateSigningRequestCleanerController         = "certificatesigningrequest-cleaner-controller"
	TTLController                                      = "ttl-controller"
	BootstrapSignerController                          = "bootstrap-signer-controller"
	TokenCleanerController                             = "token-cleaner-controller"
	NodeIpamController                                 = "node-ipam-controller"
	NodeLifecycleController                            = "node-lifecycle-controller"
	TaintEvictionController                            = "taint-eviction-controller"
	PersistentVolumeBinderController                   = "persistentvolume-binder-controller"
	PersistentVolumeAttachDetachController             = "persistentvolume-attach-detach-controller"
	PersistentVolumeExpanderController                 = "persistentvolume-expander-controller"
	ClusterRoleAggregationController                   = "clusterrole-aggregation-controller"
	PersistentVolumeClaimProtectionController          = "persistentvolumeclaim-protection-controller"
	PersistentVolumeProtectionController               = "persistentvolume-protection-controller"
	TTLAfterFinishedController                         = "ttl-after-finished-controller"
	RootCACertificatePublisherController               = "root-ca-certificate-publisher-controller"
	KubeAPIServerClusterTrustBundlePublisherController = "kube-apiserver-serving-clustertrustbundle-publisher-controller"
	EphemeralVolumeController                          = "ephemeral-volume-controller"
	StorageVersionGarbageCollectorController           = "storageversion-garbage-collector-controller"
	ResourceClaimController                            = "resourceclaim-controller"
	LegacyServiceAccountTokenCleanerController         = "legacy-serviceaccount-token-cleaner-controller"
	ValidatingAdmissionPolicyStatusController          = "validatingadmissionpolicy-status-controller"
	VolumeAttributesClassProtectionController          = "volumeattributesclass-protection-controller"
	ServiceCIDRController                              = "service-cidr-controller"
	StorageVersionMigratorController                   = "storage-version-migrator-controller"
	SELinuxWarningController                           = "selinux-warning-controller"
)
