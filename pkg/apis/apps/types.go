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

package apps

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	api "k8s.io/kubernetes/pkg/apis/core"
)

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// StatefulSet represents a set of pods with consistent identities.
// Identities are defined as:
//   - Network: A single stable DNS and hostname.
//   - Storage: As many VolumeClaims as requested.
//
// The StatefulSet guarantees that a given network identity will always
// map to the same storage identity.
type StatefulSet struct {
	metav1.TypeMeta
	// +optional
	metav1.ObjectMeta

	// Spec defines the desired identities of pods in this set.
	// +optional
	Spec StatefulSetSpec

	// Status is the current status of Pods in this StatefulSet. This data
	// may be out of date by some window of time.
	// +optional
	Status StatefulSetStatus
}

// PodManagementPolicyType defines the policy for creating pods under a stateful set.
type PodManagementPolicyType string

const (
	// OrderedReadyPodManagement will create pods in strictly increasing order on
	// scale up and strictly decreasing order on scale down, progressing only when
	// the previous pod is ready or terminated. At most one pod will be changed
	// at any time.
	OrderedReadyPodManagement PodManagementPolicyType = "OrderedReady"
	// ParallelPodManagement will create and delete pods as soon as the stateful set
	// replica count is changed, and will not wait for pods to be ready or complete
	// termination.
	ParallelPodManagement PodManagementPolicyType = "Parallel"
)

// StatefulSetUpdateStrategy indicates the strategy that the StatefulSet
// controller will use to perform updates. It includes any additional parameters
// necessary to perform the update for the indicated strategy.
type StatefulSetUpdateStrategy struct {
	// Type indicates the type of the StatefulSetUpdateStrategy.
	Type StatefulSetUpdateStrategyType
	// RollingUpdate is used to communicate parameters when Type is RollingUpdateStatefulSetStrategyType.
	RollingUpdate *RollingUpdateStatefulSetStrategy
}

// StatefulSetUpdateStrategyType is a string enumeration type that enumerates
// all possible update strategies for the StatefulSet controller.
type StatefulSetUpdateStrategyType string

const (
	// RollingUpdateStatefulSetStrategyType indicates that update will be
	// applied to all Pods in the StatefulSet with respect to the StatefulSet
	// ordering constraints. When a scale operation is performed with this
	// strategy, new Pods will be created from the specification version indicated
	// by the StatefulSet's updateRevision.
	RollingUpdateStatefulSetStrategyType StatefulSetUpdateStrategyType = "RollingUpdate"
	// OnDeleteStatefulSetStrategyType triggers the legacy behavior. Version
	// tracking and ordered rolling restarts are disabled. Pods are recreated
	// from the StatefulSetSpec when they are manually deleted. When a scale
	// operation is performed with this strategy,specification version indicated
	// by the StatefulSet's currentRevision.
	OnDeleteStatefulSetStrategyType StatefulSetUpdateStrategyType = "OnDelete"
)

// RollingUpdateStatefulSetStrategy is used to communicate parameter for RollingUpdateStatefulSetStrategyType.
type RollingUpdateStatefulSetStrategy struct {
	// Partition indicates the ordinal at which the StatefulSet should be partitioned
	// for updates. During a rolling update, all pods from ordinal Replicas-1 to
	// Partition are updated. All pods from ordinal Partition-1 to 0 remain untouched.
	// This is helpful in being able to do a canary based deployment. The default value is 0.
	Partition int32
	// The maximum number of pods that can be unavailable during the update.
	// Value can be an absolute number (ex: 5) or a percentage of desired pods (ex: 10%).
	// Absolute number is calculated from percentage by rounding up. This can not be 0.
	// Defaults to 1. This field is alpha-level and is only honored by servers that enable the
	// MaxUnavailableStatefulSet feature. The field applies to all pods in the range 0 to
	// Replicas-1. That means if there is any unavailable pod in the range 0 to Replicas-1, it
	// will be counted towards MaxUnavailable.
	// +optional
	MaxUnavailable *intstr.IntOrString
}

// PersistentVolumeClaimRetentionPolicyType is a string enumeration of the policies that will determine
// when volumes from the VolumeClaimTemplates will be deleted when the controlling StatefulSet is
// deleted or scaled down.
type PersistentVolumeClaimRetentionPolicyType string

const (
	// RetainPersistentVolumeClaimRetentionPolicyType is the default
	// PersistentVolumeClaimRetentionPolicy and specifies that
	// PersistentVolumeClaims associated with StatefulSet VolumeClaimTemplates
	// will not be deleted.
	RetainPersistentVolumeClaimRetentionPolicyType PersistentVolumeClaimRetentionPolicyType = "Retain"
	// DeletePersistentVolumeClaimRetentionPolicyType specifies that
	// PersistentVolumeClaims associated with StatefulSet VolumeClaimTemplates
	// will be deleted in the scenario specified in
	// StatefulSetPersistentVolumeClaimPolicy.
	DeletePersistentVolumeClaimRetentionPolicyType PersistentVolumeClaimRetentionPolicyType = "Delete"
)

// StatefulSetPersistentVolumeClaimRetentionPolicy describes the policy used for PVCs
// created from the StatefulSet VolumeClaimTemplates.
type StatefulSetPersistentVolumeClaimRetentionPolicy struct {
	// WhenDeleted specifies what happens to PVCs created from StatefulSet
	// VolumeClaimTemplates when the StatefulSet is deleted. The default policy
	// of `Retain` causes PVCs to not be affected by StatefulSet deletion. The
	// `Delete` policy causes those PVCs to be deleted.
	WhenDeleted PersistentVolumeClaimRetentionPolicyType
	// WhenScaled specifies what happens to PVCs created from StatefulSet
	// VolumeClaimTemplates when the StatefulSet is scaled down. The default
	// policy of `Retain` causes PVCs to not be affected by a scaledown. The
	// `Delete` policy causes the associated PVCs for any excess pods above
	// the replica count to be deleted.
	WhenScaled PersistentVolumeClaimRetentionPolicyType
}

// StatefulSetOrdinals describes the policy used for replica ordinal assignment
// in this StatefulSet.
type StatefulSetOrdinals struct {
	// start is the number representing the first replica's index. It may be used
	// to number replicas from an alternate index (eg: 1-indexed) over the default
	// 0-indexed names, or to orchestrate progressive movement of replicas from
	// one StatefulSet to another.
	// If set, replica indices will be in the range:
	//   [.spec.ordinals.start, .spec.ordinals.start + .spec.replicas).
	// If unset, defaults to 0. Replica indices will be in the range:
	//   [0, .spec.replicas).
	// +optional
	Start int32
}

// A StatefulSetSpec is the specification of a StatefulSet.
type StatefulSetSpec struct {
	// Replicas is the desired number of replicas of the given Template.
	// These are replicas in the sense that they are instantiations of the
	// same Template, but individual replicas also have a consistent identity.
	// If unspecified, defaults to 1.
	// TODO: Consider a rename of this field.
	// +optional
	Replicas int32

	// Selector is a label query over pods that should match the replica count.
	// If empty, defaulted to labels on the pod template.
	// More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/labels/#label-selectors
	// +optional
	Selector *metav1.LabelSelector

	// Template is the object that describes the pod that will be created if
	// insufficient replicas are detected. Each pod stamped out by the StatefulSet
	// will fulfill this Template, but have a unique identity from the rest
	// of the StatefulSet. Each pod will be named with the format
	// <statefulsetname>-<podindex>. For example, a pod in a StatefulSet named
	// "web" with index number "3" would be named "web-3".
	// The only allowed template.spec.restartPolicy value is "Always".
	Template api.PodTemplateSpec

	// VolumeClaimTemplates is a list of claims that pods are allowed to reference.
	// The StatefulSet controller is responsible for mapping network identities to
	// claims in a way that maintains the identity of a pod. Every claim in
	// this list must have at least one matching (by name) volumeMount in one
	// container in the template. A claim in this list takes precedence over
	// any volumes in the template, with the same name.
	// TODO: Define the behavior if a claim already exists with the same name.
	// +optional
	VolumeClaimTemplates []api.PersistentVolumeClaim

	// ServiceName is the name of the service that governs this StatefulSet.
	// This service must exist before the StatefulSet, and is responsible for
	// the network identity of the set. Pods get DNS/hostnames that follow the
	// pattern: pod-specific-string.serviceName.default.svc.cluster.local
	// where "pod-specific-string" is managed by the StatefulSet controller.
	ServiceName string

	// PodManagementPolicy controls how pods are created during initial scale up,
	// when replacing pods on nodes, or when scaling down. The default policy is
	// `OrderedReady`, where pods are created in increasing order (pod-0, then
	// pod-1, etc) and the controller will wait until each pod is ready before
	// continuing. When scaling down, the pods are removed in the opposite order.
	// The alternative policy is `Parallel` which will create pods in parallel
	// to match the desired scale without waiting, and on scale down will delete
	// all pods at once.
	// +optional
	PodManagementPolicy PodManagementPolicyType

	// updateStrategy indicates the StatefulSetUpdateStrategy that will be
	// employed to update Pods in the StatefulSet when a revision is made to
	// Template.
	UpdateStrategy StatefulSetUpdateStrategy

	// revisionHistoryLimit is the maximum number of revisions that will
	// be maintained in the StatefulSet's revision history. The revision history
	// consists of all revisions not represented by a currently applied
	// StatefulSetSpec version. The default value is 10.
	RevisionHistoryLimit *int32

	// Minimum number of seconds for which a newly created pod should be ready
	// without any of its container crashing for it to be considered available.
	// Defaults to 0 (pod will be considered available as soon as it is ready)
	// +optional
	MinReadySeconds int32

	// PersistentVolumeClaimRetentionPolicy describes the policy used for PVCs created from
	// the StatefulSet VolumeClaimTemplates. This requires the
	// StatefulSetAutoDeletePVC feature gate to be enabled, which is beta and default on from 1.27.
	// +optional
	PersistentVolumeClaimRetentionPolicy *StatefulSetPersistentVolumeClaimRetentionPolicy

	// ordinals controls the numbering of replica indices in a StatefulSet. The
	// default ordinals behavior assigns a "0" index to the first replica and
	// increments the index by one for each additional replica requested.
	// +optional
	Ordinals *StatefulSetOrdinals
}

// StatefulSetStatus represents the current state of a StatefulSet.
type StatefulSetStatus struct {
	// observedGeneration is the most recent generation observed for this StatefulSet. It corresponds to the
	// StatefulSet's generation, which is updated on mutation by the API Server.
	// +optional
	ObservedGeneration *int64

	// replicas is the number of Pods created by the StatefulSet controller.
	Replicas int32

	// readyReplicas is the number of Pods created by the StatefulSet controller that have a Ready Condition.
	ReadyReplicas int32

	// currentReplicas is the number of Pods created by the StatefulSet controller from the StatefulSet version
	// indicated by currentRevision.
	CurrentReplicas int32

	// updatedReplicas is the number of Pods created by the StatefulSet controller from the StatefulSet version
	// indicated by updateRevision.
	UpdatedReplicas int32

	// currentRevision, if not empty, indicates the version of the StatefulSet used to generate Pods in the
	// sequence [0,currentReplicas).
	CurrentRevision string

	// updateRevision, if not empty, indicates the version of the StatefulSet used to generate Pods in the sequence
	// [replicas-updatedReplicas,replicas)
	UpdateRevision string

	// collisionCount is the count of hash collisions for the StatefulSet. The StatefulSet controller
	// uses this field as a collision avoidance mechanism when it needs to create the name for the
	// newest ControllerRevision.
	// +optional
	CollisionCount *int32

	// Represents the latest available observations of a statefulset's current state.
	Conditions []StatefulSetCondition

	// Total number of available pods (ready for at least minReadySeconds) targeted by this statefulset.
	// +optional
	AvailableReplicas int32
}

// StatefulSetConditionType describes the condition types of StatefulSets.
type StatefulSetConditionType string

// TODO: Add valid condition types for Statefulsets.

// StatefulSetCondition describes the state of a statefulset at a certain point.
type StatefulSetCondition struct {
	// Type of statefulset condition.
	Type StatefulSetConditionType
	// Status of the condition, one of True, False, Unknown.
	Status api.ConditionStatus
	// The last time this condition was updated.
	LastTransitionTime metav1.Time
	// The reason for the condition's last transition.
	Reason string
	// A human readable message indicating details about the transition.
	Message string
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// StatefulSetList is a collection of StatefulSets.
type StatefulSetList struct {
	metav1.TypeMeta
	// +optional
	metav1.ListMeta
	Items []StatefulSet
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// ControllerRevision implements an immutable snapshot of state data. Clients
// are responsible for serializing and deserializing the objects that contain
// their internal state.
// Once a ControllerRevision has been successfully created, it can not be updated.
// The API Server will fail validation of all requests that attempt to mutate
// the Data field. ControllerRevisions may, however, be deleted.
type ControllerRevision struct {
	metav1.TypeMeta
	// Standard object's metadata.
	// More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#metadata
	// +optional
	metav1.ObjectMeta

	// Data is the Object representing the state.
	Data runtime.RawExtension

	// Revision indicates the revision of the state represented by Data.
	Revision int64
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// ControllerRevisionList is a resource containing a list of ControllerRevision objects.
type ControllerRevisionList struct {
	metav1.TypeMeta
	// +optional
	metav1.ListMeta

	// Items is the list of ControllerRevision objects.
	Items []ControllerRevision
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// Deployment provides declarative updates for Pods and ReplicaSets.
type Deployment struct {
	metav1.TypeMeta
	// +optional
	metav1.ObjectMeta

	// Specification of the desired behavior of the Deployment.
	// +optional
	Spec DeploymentSpec

	// Most recently observed status of the Deployment.
	// +optional
	Status DeploymentStatus
}

// DeploymentSpec specifies the state of a Deployment.
type DeploymentSpec struct {
	// Number of desired pods.
	Replicas int32

	// Label selector for pods. Existing ReplicaSets whose pods are
	// selected by this will be the ones affected by this deployment.
	// +optional
	Selector *metav1.LabelSelector

	// Template describes the pods that will be created.
	// The only allowed template.spec.restartPolicy value is "Always".
	Template api.PodTemplateSpec

	// The deployment strategy to use to replace existing pods with new ones.
	// +optional
	Strategy DeploymentStrategy

	// Minimum number of seconds for which a newly created pod should be ready
	// without any of its container crashing, for it to be considered available.
	// Defaults to 0 (pod will be considered available as soon as it is ready)
	// +optional
	MinReadySeconds int32

	// The number of old ReplicaSets to retain to allow rollback.
	// This is a pointer to distinguish between explicit zero and not specified.
	// This is set to the max value of int32 (i.e. 2147483647) by default, which means
	// "retaining all old ReplicaSets".
	// +optional
	RevisionHistoryLimit *int32

	// Indicates that the deployment is paused and will not be processed by the
	// deployment controller.
	// +optional
	Paused bool

	// DEPRECATED.
	// The config this deployment is rolling back to. Will be cleared after rollback is done.
	// +optional
	RollbackTo *RollbackConfig

	// The maximum time in seconds for a deployment to make progress before it
	// is considered to be failed. The deployment controller will continue to
	// process failed deployments and a condition with a ProgressDeadlineExceeded
	// reason will be surfaced in the deployment status. Note that progress will
	// not be estimated during the time a deployment is paused. This is set to
	// the max value of int32 (i.e. 2147483647) by default, which means "no deadline".
	// +optional
	ProgressDeadlineSeconds *int32

	// podReplacementPolicy specifies when to create replacement Pods.
	// Possible values are:
	// - TerminationStarted policy creates replacement Pods when the old Pods start
	//   terminating (have a non-null .metadata.deletionTimestamp). The total number
	//   of Deployment Pods can be greater than specified by the Deployment's
	//   .spec.replicas and the DeploymentStrategy.
	// - TerminationComplete policy creates replacement Pods only when the old Pods
	//   are fully terminated (reach Succeeded or Failed phase). The old Pods are
	//   subsequently removed. The total number of the Deployment Pods is
	//   limited by the Deployment's .spec.replicas and the DeploymentStrategy.
	//   This policy will also delay declaring the deployment as complete until all
	//   of its terminating replicas have been fully terminated.
	//
	// The default behavior when the policy is not specified depends on the DeploymentStrategy:
	// - Recreate strategy uses TerminationComplete behavior when recreating the deployment,
	//   but uses TerminationStarted when scaling the deployment.
	// - RollingUpdate strategy uses TerminationStarted behavior for both rolling out and
	//   scaling the deployments.
	//
	// This is an alpha field. Enable DeploymentPodReplacementPolicy to be able to
	// use this field.
	// +optional
	PodReplacementPolicy *DeploymentPodReplacementPolicy
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// DeploymentRollback stores the information required to rollback a deployment.
// DEPRECATED.
type DeploymentRollback struct {
	metav1.TypeMeta
	// Required: This must match the Name of a deployment.
	Name string
	// The annotations to be updated to a deployment
	// +optional
	UpdatedAnnotations map[string]string
	// The config of this deployment rollback.
	RollbackTo RollbackConfig
}

// RollbackConfig specifies the state of a revision to roll back to.
// DEPRECATED.
type RollbackConfig struct {
	// The revision to rollback to. If set to 0, rollback to the last revision.
	// +optional
	Revision int64
}

const (
	// DefaultDeploymentUniqueLabelKey is the default key of the selector that is added
	// to existing RCs (and label key that is added to its pods) to prevent the existing RCs
	// to select new pods (and old pods being select by new RC).
	DefaultDeploymentUniqueLabelKey string = "pod-template-hash"
)

// DeploymentStrategy stores information about the strategy and rolling-update
// behavior of a deployment.
type DeploymentStrategy struct {
	// Type of deployment. Can be "Recreate" or "RollingUpdate". Default is RollingUpdate.
	// +optional
	Type DeploymentStrategyType

	// Rolling update config params. Present only if DeploymentStrategyType =
	// RollingUpdate.
	//---
	// TODO: Update this to follow our convention for oneOf, whatever we decide it
	// to be.
	// +optional
	RollingUpdate *RollingUpdateDeployment
}

// DeploymentStrategyType defines strategies with a deployment.
type DeploymentStrategyType string

const (
	// RecreateDeploymentStrategyType - kill all existing pods before creating new ones.
	RecreateDeploymentStrategyType DeploymentStrategyType = "Recreate"

	// RollingUpdateDeploymentStrategyType - Replace the old RCs by new one using rolling update i.e gradually scale down the old RCs and scale up the new one.
	RollingUpdateDeploymentStrategyType DeploymentStrategyType = "RollingUpdate"
)

// RollingUpdateDeployment is the spec to control the desired behavior of rolling update.
type RollingUpdateDeployment struct {
	// The maximum number of pods that can be unavailable during the update.
	// Value can be an absolute number (ex: 5) or a percentage of total pods at the start of update (ex: 10%).
	// Absolute number is calculated from percentage by rounding down.
	// This can not be 0 if MaxSurge is 0.
	// By default, a fixed value of 1 is used.
	// Example: when this is set to 30%, the old RC can be scaled down by 30%
	// immediately when the rolling update starts. Once new pods are ready, old RC
	// can be scaled down further, followed by scaling up the new RC, ensuring
	// that at least 70% of original number of pods are available at all times
	// during the update.
	// +optional
	MaxUnavailable intstr.IntOrString

	// The maximum number of pods that can be scheduled above the original number of
	// pods.
	// Value can be an absolute number (ex: 5) or a percentage of total pods at
	// the start of the update (ex: 10%). This can not be 0 if MaxUnavailable is 0.
	// Absolute number is calculated from percentage by rounding up.
	// By default, a value of 1 is used.
	// Example: when this is set to 30%, the new RC can be scaled up by 30%
	// immediately when the rolling update starts. Once old pods have been killed,
	// new RC can be scaled up further, ensuring that total number of pods running
	// at any time during the update is at most 130% of original pods.
	// +optional
	MaxSurge intstr.IntOrString
}

// DeploymentPodReplacementPolicy specifies the policy for creating Deployment Pod replacements.
// Default is a mixed behavior depending on the DeploymentStrategy
// +enum
type DeploymentPodReplacementPolicy string

const (
	// TerminationStarted policy creates replacement Pods when the old Pods start
	// terminating (have a non-null .metadata.deletionTimestamp). The total number
	// of Deployment Pods can be greater than specified by the Deployment's
	// .spec.replicas and the DeploymentStrategy.
	TerminationStarted DeploymentPodReplacementPolicy = "TerminationStarted"
	// TerminationComplete policy creates replacement Pods only when the old Pods
	// are fully terminated (reach Succeeded or Failed phase). The old Pods are
	// subsequently removed. The total number of the Deployment Pods is
	// limited by the Deployment's .spec.replicas and the DeploymentStrategy.
	//
	// This policy will also delay declaring the deployment as complete until all
	// of its terminating replicas have been fully terminated.
	TerminationComplete DeploymentPodReplacementPolicy = "TerminationComplete"
)

// DeploymentStatus holds information about the observed status of a deployment.
type DeploymentStatus struct {
	// The generation observed by the deployment controller.
	// +optional
	ObservedGeneration int64

	// Total number of non-terminating pods targeted by this deployment (their labels match the selector).
	// +optional
	Replicas int32

	// Total number of non-terminating pods targeted by this deployment that have the desired template spec.
	// +optional
	UpdatedReplicas int32

	// Total number of non-terminating pods targeted by this Deployment with a Ready Condition.
	// +optional
	ReadyReplicas int32

	// Total number of available non-terminating pods (ready for at least minReadySeconds) targeted by this deployment.
	// +optional
	AvailableReplicas int32

	// Total number of unavailable pods targeted by this deployment. This is the total number of
	// pods that are still required for the deployment to have 100% available capacity. They may
	// either be pods that are running but not yet available or pods that still have not been created.
	// +optional
	UnavailableReplicas int32

	// Total number of terminating pods targeted by this deployment. Terminating pods have a non-null
	// .metadata.deletionTimestamp and have not yet reached the Failed or Succeeded .status.phase.
	//
	// This is an alpha field. Enable DeploymentPodReplacementPolicy to be able to use this field.
	// +optional
	TerminatingReplicas *int32

	// Represents the latest available observations of a deployment's current state.
	Conditions []DeploymentCondition

	// Count of hash collisions for the Deployment. The Deployment controller uses this
	// field as a collision avoidance mechanism when it needs to create the name for the
	// newest ReplicaSet.
	// +optional
	CollisionCount *int32
}

// DeploymentConditionType defines conditions of a deployment.
type DeploymentConditionType string

// These are valid conditions of a deployment.
const (
	// Available means the deployment is available, ie. at least the minimum available
	// replicas required are up and running for at least minReadySeconds.
	DeploymentAvailable DeploymentConditionType = "Available"
	// Progressing means the deployment is progressing. Progress for a deployment is
	// considered when a new replica set is created or adopted, and when new pods scale
	// up or old pods scale down. Progress is not estimated for paused deployments or
	// when progressDeadlineSeconds is not specified.
	DeploymentProgressing DeploymentConditionType = "Progressing"
	// ReplicaFailure is added in a deployment when one of its pods fails to be created
	// or deleted.
	DeploymentReplicaFailure DeploymentConditionType = "ReplicaFailure"
)

// DeploymentCondition describes the state of a deployment at a certain point.
type DeploymentCondition struct {
	// Type of deployment condition.
	Type DeploymentConditionType
	// Status of the condition, one of True, False, Unknown.
	Status api.ConditionStatus
	// The last time this condition was updated.
	LastUpdateTime metav1.Time
	// Last time the condition transitioned from one status to another.
	LastTransitionTime metav1.Time
	// The reason for the condition's last transition.
	Reason string
	// A human readable message indicating details about the transition.
	Message string
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// DeploymentList defines multiple deployments.
type DeploymentList struct {
	metav1.TypeMeta
	// +optional
	metav1.ListMeta

	// Items is the list of deployments.
	Items []Deployment
}

// DaemonSetUpdateStrategy defines a strategy to update a daemon set.
type DaemonSetUpdateStrategy struct {
	// Type of daemon set update. Can be "RollingUpdate" or "OnDelete".
	// +optional
	Type DaemonSetUpdateStrategyType

	// Rolling update config params. Present only if type = "RollingUpdate".
	//---
	// TODO: Update this to follow our convention for oneOf, whatever we decide it
	// to be. Same as Deployment `strategy.rollingUpdate`.
	// See https://github.com/kubernetes/kubernetes/issues/35345
	// +optional
	RollingUpdate *RollingUpdateDaemonSet
}

// DaemonSetUpdateStrategyType is a strategy according to which a daemon set
// gets updated.
type DaemonSetUpdateStrategyType string

const (
	// RollingUpdateDaemonSetStrategyType - Replace the old daemons by new ones using rolling update i.e replace them on each node one after the other.
	RollingUpdateDaemonSetStrategyType DaemonSetUpdateStrategyType = "RollingUpdate"

	// OnDeleteDaemonSetStrategyType - Replace the old daemons only when it's killed
	OnDeleteDaemonSetStrategyType DaemonSetUpdateStrategyType = "OnDelete"
)

// RollingUpdateDaemonSet is the spec to control the desired behavior of daemon set rolling update.
type RollingUpdateDaemonSet struct {
	// The maximum number of DaemonSet pods that can be unavailable during the
	// update. Value can be an absolute number (ex: 5) or a percentage of total
	// number of DaemonSet pods at the start of the update (ex: 10%). Absolute
	// number is calculated from percentage by rounding up.
	// This cannot be 0 if MaxSurge is 0
	// Default value is 1.
	// Example: when this is set to 30%, at most 30% of the total number of nodes
	// that should be running the daemon pod (i.e. status.desiredNumberScheduled)
	// can have their pods stopped for an update at any given time. The update
	// starts by stopping at most 30% of those DaemonSet pods and then brings
	// up new DaemonSet pods in their place. Once the new pods are available,
	// it then proceeds onto other DaemonSet pods, thus ensuring that at least
	// 70% of original number of DaemonSet pods are available at all times during
	// the update.
	// +optional
	MaxUnavailable intstr.IntOrString

	// The maximum number of nodes with an existing available DaemonSet pod that
	// can have an updated DaemonSet pod during during an update.
	// Value can be an absolute number (ex: 5) or a percentage of desired pods (ex: 10%).
	// This can not be 0 if MaxUnavailable is 0.
	// Absolute number is calculated from percentage by rounding up to a minimum of 1.
	// Default value is 0.
	// Example: when this is set to 30%, at most 30% of the total number of nodes
	// that should be running the daemon pod (i.e. status.desiredNumberScheduled)
	// can have their a new pod created before the old pod is marked as deleted.
	// The update starts by launching new pods on 30% of nodes. Once an updated
	// pod is available (Ready for at least minReadySeconds) the old DaemonSet pod
	// on that node is marked deleted. If the old pod becomes unavailable for any
	// reason (Ready transitions to false, is evicted, or is drained) an updated
	// pod is immediately created on that node without considering surge limits.
	// Allowing surge implies the possibility that the resources consumed by the
	// daemonset on any given node can double if the readiness check fails, and
	// so resource intensive daemonsets should take into account that they may
	// cause evictions during disruption.
	// +optional
	MaxSurge intstr.IntOrString
}

// DaemonSetSpec is the specification of a daemon set.
type DaemonSetSpec struct {
	// A label query over pods that are managed by the daemon set.
	// Must match in order to be controlled.
	// If empty, defaulted to labels on Pod template.
	// More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/labels/#label-selectors
	// +optional
	Selector *metav1.LabelSelector

	// An object that describes the pod that will be created.
	// The DaemonSet will create exactly one copy of this pod on every node
	// that matches the template's node selector (or on every node if no node
	// selector is specified).
	// The only allowed template.spec.restartPolicy value is "Always".
	// More info: https://kubernetes.io/docs/concepts/workloads/controllers/replicationcontroller#pod-template
	Template api.PodTemplateSpec

	// An update strategy to replace existing DaemonSet pods with new pods.
	// +optional
	UpdateStrategy DaemonSetUpdateStrategy

	// The minimum number of seconds for which a newly created DaemonSet pod should
	// be ready without any of its container crashing, for it to be considered
	// available. Defaults to 0 (pod will be considered available as soon as it
	// is ready).
	// +optional
	MinReadySeconds int32

	// DEPRECATED.
	// A sequence number representing a specific generation of the template.
	// Populated by the system. It can be set only during the creation.
	// +optional
	TemplateGeneration int64

	// The number of old history to retain to allow rollback.
	// This is a pointer to distinguish between explicit zero and not specified.
	// Defaults to 10.
	// +optional
	RevisionHistoryLimit *int32
}

// DaemonSetStatus represents the current status of a daemon set.
type DaemonSetStatus struct {
	// The number of nodes that are running at least 1
	// daemon pod and are supposed to run the daemon pod.
	CurrentNumberScheduled int32

	// The number of nodes that are running the daemon pod, but are
	// not supposed to run the daemon pod.
	NumberMisscheduled int32

	// The total number of nodes that should be running the daemon
	// pod (including nodes correctly running the daemon pod).
	DesiredNumberScheduled int32

	// The number of nodes that should be running the daemon pod and have one
	// or more of the daemon pod running and ready.
	NumberReady int32

	// The most recent generation observed by the daemon set controller.
	// +optional
	ObservedGeneration int64

	// The total number of nodes that are running updated daemon pod
	// +optional
	UpdatedNumberScheduled int32

	// The number of nodes that should be running the
	// daemon pod and have one or more of the daemon pod running and
	// available (ready for at least spec.minReadySeconds)
	// +optional
	NumberAvailable int32

	// The number of nodes that should be running the
	// daemon pod and have none of the daemon pod running and available
	// (ready for at least spec.minReadySeconds)
	// +optional
	NumberUnavailable int32

	// Count of hash collisions for the DaemonSet. The DaemonSet controller
	// uses this field as a collision avoidance mechanism when it needs to
	// create the name for the newest ControllerRevision.
	// +optional
	CollisionCount *int32

	// Represents the latest available observations of a DaemonSet's current state.
	Conditions []DaemonSetCondition
}

// DaemonSetConditionType defines a daemon set condition.
type DaemonSetConditionType string

// TODO: Add valid condition types of a DaemonSet.

// DaemonSetCondition describes the state of a DaemonSet at a certain point.
type DaemonSetCondition struct {
	// Type of DaemonSet condition.
	Type DaemonSetConditionType
	// Status of the condition, one of True, False, Unknown.
	Status api.ConditionStatus
	// Last time the condition transitioned from one status to another.
	LastTransitionTime metav1.Time
	// The reason for the condition's last transition.
	Reason string
	// A human readable message indicating details about the transition.
	Message string
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// DaemonSet represents the configuration of a daemon set.
type DaemonSet struct {
	metav1.TypeMeta
	// Standard object's metadata.
	// More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#metadata
	// +optional
	metav1.ObjectMeta

	// The desired behavior of this daemon set.
	// More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#spec-and-status
	// +optional
	Spec DaemonSetSpec

	// The current status of this daemon set. This data may be
	// out of date by some window of time.
	// Populated by the system.
	// Read-only.
	// More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#spec-and-status
	// +optional
	Status DaemonSetStatus
}

const (
	// DaemonSetTemplateGenerationKey is the key of the labels that is added
	// to daemon set pods to distinguish between old and new pod templates
	// during DaemonSet template update.
	// DEPRECATED: DefaultDaemonSetUniqueLabelKey is used instead.
	DaemonSetTemplateGenerationKey string = "pod-template-generation"
)

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// DaemonSetList is a collection of daemon sets.
type DaemonSetList struct {
	metav1.TypeMeta
	// Standard list metadata.
	// More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#metadata
	// +optional
	metav1.ListMeta

	// A list of daemon sets.
	Items []DaemonSet
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// ReplicaSet ensures that a specified number of pod replicas are running at any given time.
type ReplicaSet struct {
	metav1.TypeMeta
	// +optional
	metav1.ObjectMeta

	// Spec defines the desired behavior of this ReplicaSet.
	// +optional
	Spec ReplicaSetSpec

	// Status is the current status of this ReplicaSet. This data may be
	// out of date by some window of time.
	// +optional
	Status ReplicaSetStatus
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// ReplicaSetList is a collection of ReplicaSets.
type ReplicaSetList struct {
	metav1.TypeMeta
	// +optional
	metav1.ListMeta

	Items []ReplicaSet
}

// ReplicaSetSpec is the specification of a ReplicaSet.
// As the internal representation of a ReplicaSet, it must have
// a Template set.
type ReplicaSetSpec struct {
	// Replicas is the number of desired replicas.
	Replicas int32

	// Minimum number of seconds for which a newly created pod should be ready
	// without any of its container crashing, for it to be considered available.
	// Defaults to 0 (pod will be considered available as soon as it is ready)
	// +optional
	MinReadySeconds int32

	// Selector is a label query over pods that should match the replica count.
	// Must match in order to be controlled.
	// If empty, defaulted to labels on pod template.
	// More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/labels/#label-selectors
	// +optional
	Selector *metav1.LabelSelector

	// Template is the object that describes the pod that will be created if
	// insufficient replicas are detected.
	// The only allowed template.spec.restartPolicy value is "Always".
	// +optional
	Template api.PodTemplateSpec
}

// ReplicaSetStatus represents the current status of a ReplicaSet.
type ReplicaSetStatus struct {
	// Replicas is the most recently observed number of non-terminating pods.
	// More info: https://kubernetes.io/docs/concepts/workloads/controllers/replicaset
	Replicas int32

	// The number of non-terminating pods that have labels matching the labels of the pod template of the replicaset.
	// +optional
	FullyLabeledReplicas int32

	// The number of non-terminating pods targeted by this ReplicaSet with a Ready Condition.
	// +optional
	ReadyReplicas int32

	// The number of available non-terminating pods (ready for at least minReadySeconds) for this replica set.
	// +optional
	AvailableReplicas int32

	// The number of terminating pods for this replica set. Terminating pods have a non-null .metadata.deletionTimestamp
	// and have not yet reached the Failed or Succeeded .status.phase.
	//
	// This is an alpha field. Enable DeploymentPodReplacementPolicy to be able to use this field.
	// +optional
	TerminatingReplicas *int32

	// ObservedGeneration reflects the generation of the most recently observed ReplicaSet.
	// +optional
	ObservedGeneration int64

	// Represents the latest available observations of a replica set's current state.
	// +optional
	Conditions []ReplicaSetCondition
}

// ReplicaSetConditionType is a condition of a replica set.
type ReplicaSetConditionType string

// These are valid conditions of a replica set.
const (
	// ReplicaSetReplicaFailure is added in a replica set when one of its pods fails to be created
	// due to insufficient quota, limit ranges, pod security policy, node selectors, etc. or deleted
	// due to kubelet being down or finalizers are failing.
	ReplicaSetReplicaFailure ReplicaSetConditionType = "ReplicaFailure"
)

// ReplicaSetCondition describes the state of a replica set at a certain point.
type ReplicaSetCondition struct {
	// Type of replica set condition.
	Type ReplicaSetConditionType
	// Status of the condition, one of True, False, Unknown.
	Status api.ConditionStatus
	// The last time the condition transitioned from one status to another.
	// +optional
	LastTransitionTime metav1.Time
	// The reason for the condition's last transition.
	// +optional
	Reason string
	// A human readable message indicating details about the transition.
	// +optional
	Message string
}
