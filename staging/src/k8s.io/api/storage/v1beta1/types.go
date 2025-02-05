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

package v1beta1

import (
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +genclient
// +genclient:nonNamespaced
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:prerelease-lifecycle-gen:introduced=1.4
// +k8s:prerelease-lifecycle-gen:deprecated=1.19
// +k8s:prerelease-lifecycle-gen:replacement=storage.k8s.io,v1,StorageClass

// StorageClass describes the parameters for a class of storage for
// which PersistentVolumes can be dynamically provisioned.
//
// StorageClasses are non-namespaced; the name of the storage class
// according to etcd is in ObjectMeta.Name.
type StorageClass struct {
	metav1.TypeMeta `json:",inline"`

	// Standard object's metadata.
	// More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	// provisioner indicates the type of the provisioner.
	Provisioner string `json:"provisioner" protobuf:"bytes,2,opt,name=provisioner"`

	// parameters holds the parameters for the provisioner that should
	// create volumes of this storage class.
	// +optional
	Parameters map[string]string `json:"parameters,omitempty" protobuf:"bytes,3,rep,name=parameters"`

	// reclaimPolicy controls the reclaimPolicy for dynamically provisioned PersistentVolumes of this storage class.
	// Defaults to Delete.
	// +optional
	ReclaimPolicy *v1.PersistentVolumeReclaimPolicy `json:"reclaimPolicy,omitempty" protobuf:"bytes,4,opt,name=reclaimPolicy,casttype=k8s.io/api/core/v1.PersistentVolumeReclaimPolicy"`

	// mountOptions controls the mountOptions for dynamically provisioned PersistentVolumes of this storage class.
	// e.g. ["ro", "soft"]. Not validated -
	// mount of the PVs will simply fail if one is invalid.
	// +optional
	// +listType=atomic
	MountOptions []string `json:"mountOptions,omitempty" protobuf:"bytes,5,opt,name=mountOptions"`

	// allowVolumeExpansion shows whether the storage class allow volume expand
	// +optional
	AllowVolumeExpansion *bool `json:"allowVolumeExpansion,omitempty" protobuf:"varint,6,opt,name=allowVolumeExpansion"`

	// volumeBindingMode indicates how PersistentVolumeClaims should be
	// provisioned and bound.  When unset, VolumeBindingImmediate is used.
	// +optional
	VolumeBindingMode *VolumeBindingMode `json:"volumeBindingMode,omitempty" protobuf:"bytes,7,opt,name=volumeBindingMode"`

	// allowedTopologies restrict the node topologies where volumes can be dynamically provisioned.
	// Each volume plugin defines its own supported topology specifications.
	// An empty TopologySelectorTerm list means there is no topology restriction.
	// +optional
	// +listType=atomic
	AllowedTopologies []v1.TopologySelectorTerm `json:"allowedTopologies,omitempty" protobuf:"bytes,8,rep,name=allowedTopologies"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:prerelease-lifecycle-gen:introduced=1.4
// +k8s:prerelease-lifecycle-gen:deprecated=1.19
// +k8s:prerelease-lifecycle-gen:replacement=storage.k8s.io,v1,StorageClassList

// StorageClassList is a collection of storage classes.
type StorageClassList struct {
	metav1.TypeMeta `json:",inline"`

	// Standard list metadata
	// More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#metadata
	// +optional
	metav1.ListMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	// items is the list of StorageClasses
	Items []StorageClass `json:"items" protobuf:"bytes,2,rep,name=items"`
}

// VolumeBindingMode indicates how PersistentVolumeClaims should be bound.
type VolumeBindingMode string

const (
	// VolumeBindingImmediate indicates that PersistentVolumeClaims should be
	// immediately provisioned and bound.  This is the default mode.
	VolumeBindingImmediate VolumeBindingMode = "Immediate"

	// VolumeBindingWaitForFirstConsumer indicates that PersistentVolumeClaims
	// should not be provisioned and bound until the first Pod is created that
	// references the PeristentVolumeClaim.  The volume provisioning and
	// binding will occur during Pod scheduing.
	VolumeBindingWaitForFirstConsumer VolumeBindingMode = "WaitForFirstConsumer"
)

// +genclient
// +genclient:nonNamespaced
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:prerelease-lifecycle-gen:introduced=1.10
// +k8s:prerelease-lifecycle-gen:deprecated=1.19
// +k8s:prerelease-lifecycle-gen:replacement=storage.k8s.io,v1,VolumeAttachment

// VolumeAttachment captures the intent to attach or detach the specified volume
// to/from the specified node.
//
// VolumeAttachment objects are non-namespaced.
type VolumeAttachment struct {
	metav1.TypeMeta `json:",inline"`

	// Standard object metadata.
	// More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	// spec represents specification of the desired attach/detach volume behavior.
	// Populated by the Kubernetes system.
	Spec VolumeAttachmentSpec `json:"spec" protobuf:"bytes,2,opt,name=spec"`

	// status represents status of the VolumeAttachment request.
	// Populated by the entity completing the attach or detach
	// operation, i.e. the external-attacher.
	// +optional
	Status VolumeAttachmentStatus `json:"status,omitempty" protobuf:"bytes,3,opt,name=status"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:prerelease-lifecycle-gen:introduced=1.10
// +k8s:prerelease-lifecycle-gen:deprecated=1.19
// +k8s:prerelease-lifecycle-gen:replacement=storage.k8s.io,v1,VolumeAttachmentList

// VolumeAttachmentList is a collection of VolumeAttachment objects.
type VolumeAttachmentList struct {
	metav1.TypeMeta `json:",inline"`

	// Standard list metadata
	// More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#metadata
	// +optional
	metav1.ListMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	// items is the list of VolumeAttachments
	Items []VolumeAttachment `json:"items" protobuf:"bytes,2,rep,name=items"`
}

// VolumeAttachmentSpec is the specification of a VolumeAttachment request.
type VolumeAttachmentSpec struct {
	// attacher indicates the name of the volume driver that MUST handle this
	// request. This is the name returned by GetPluginName().
	Attacher string `json:"attacher" protobuf:"bytes,1,opt,name=attacher"`

	// source represents the volume that should be attached.
	Source VolumeAttachmentSource `json:"source" protobuf:"bytes,2,opt,name=source"`

	// nodeName represents the node that the volume should be attached to.
	NodeName string `json:"nodeName" protobuf:"bytes,3,opt,name=nodeName"`
}

// VolumeAttachmentSource represents a volume that should be attached.
// Right now only PersistentVolumes can be attached via external attacher,
// in the future we may allow also inline volumes in pods.
// Exactly one member can be set.
type VolumeAttachmentSource struct {
	// persistentVolumeName represents the name of the persistent volume to attach.
	// +optional
	PersistentVolumeName *string `json:"persistentVolumeName,omitempty" protobuf:"bytes,1,opt,name=persistentVolumeName"`

	// inlineVolumeSpec contains all the information necessary to attach
	// a persistent volume defined by a pod's inline VolumeSource. This field
	// is populated only for the CSIMigration feature. It contains
	// translated fields from a pod's inline VolumeSource to a
	// PersistentVolumeSpec. This field is beta-level and is only
	// honored by servers that enabled the CSIMigration feature.
	// +optional
	InlineVolumeSpec *v1.PersistentVolumeSpec `json:"inlineVolumeSpec,omitempty" protobuf:"bytes,2,opt,name=inlineVolumeSpec"`
}

// VolumeAttachmentStatus is the status of a VolumeAttachment request.
type VolumeAttachmentStatus struct {
	// attached indicates the volume is successfully attached.
	// This field must only be set by the entity completing the attach
	// operation, i.e. the external-attacher.
	Attached bool `json:"attached" protobuf:"varint,1,opt,name=attached"`

	// attachmentMetadata is populated with any
	// information returned by the attach operation, upon successful attach, that must be passed
	// into subsequent WaitForAttach or Mount calls.
	// This field must only be set by the entity completing the attach
	// operation, i.e. the external-attacher.
	// +optional
	AttachmentMetadata map[string]string `json:"attachmentMetadata,omitempty" protobuf:"bytes,2,rep,name=attachmentMetadata"`

	// attachError represents the last error encountered during attach operation, if any.
	// This field must only be set by the entity completing the attach
	// operation, i.e. the external-attacher.
	// +optional
	AttachError *VolumeError `json:"attachError,omitempty" protobuf:"bytes,3,opt,name=attachError,casttype=VolumeError"`

	// detachError represents the last error encountered during detach operation, if any.
	// This field must only be set by the entity completing the detach
	// operation, i.e. the external-attacher.
	// +optional
	DetachError *VolumeError `json:"detachError,omitempty" protobuf:"bytes,4,opt,name=detachError,casttype=VolumeError"`
}

// VolumeError captures an error encountered during a volume operation.
type VolumeError struct {
	// time represents the time the error was encountered.
	// +optional
	Time metav1.Time `json:"time,omitempty" protobuf:"bytes,1,opt,name=time"`

	// message represents the error encountered during Attach or Detach operation.
	// This string may be logged, so it should not contain sensitive
	// information.
	// +optional
	Message string `json:"message,omitempty" protobuf:"bytes,2,opt,name=message"`
}

// +genclient
// +genclient:nonNamespaced
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:prerelease-lifecycle-gen:introduced=1.14
// +k8s:prerelease-lifecycle-gen:deprecated=1.19
// +k8s:prerelease-lifecycle-gen:replacement=storage.k8s.io,v1,CSIDriver

// CSIDriver captures information about a Container Storage Interface (CSI)
// volume driver deployed on the cluster.
// CSI drivers do not need to create the CSIDriver object directly. Instead they may use the
// cluster-driver-registrar sidecar container. When deployed with a CSI driver it automatically
// creates a CSIDriver object representing the driver.
// Kubernetes attach detach controller uses this object to determine whether attach is required.
// Kubelet uses this object to determine whether pod information needs to be passed on mount.
// CSIDriver objects are non-namespaced.
type CSIDriver struct {
	metav1.TypeMeta `json:",inline"`

	// Standard object metadata.
	// metadata.Name indicates the name of the CSI driver that this object
	// refers to; it MUST be the same name returned by the CSI GetPluginName()
	// call for that driver.
	// The driver name must be 63 characters or less, beginning and ending with
	// an alphanumeric character ([a-z0-9A-Z]) with dashes (-), dots (.), and
	// alphanumerics between.
	// More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#metadata
	metav1.ObjectMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	// spec represents the specification of the CSI Driver.
	Spec CSIDriverSpec `json:"spec" protobuf:"bytes,2,opt,name=spec"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:prerelease-lifecycle-gen:introduced=1.14
// +k8s:prerelease-lifecycle-gen:deprecated=1.19
// +k8s:prerelease-lifecycle-gen:replacement=storage.k8s.io,v1,CSIDriverList

// CSIDriverList is a collection of CSIDriver objects.
type CSIDriverList struct {
	metav1.TypeMeta `json:",inline"`

	// Standard list metadata
	// More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#metadata
	// +optional
	metav1.ListMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	// items is the list of CSIDriver
	Items []CSIDriver `json:"items" protobuf:"bytes,2,rep,name=items"`
}

// CSIDriverSpec is the specification of a CSIDriver.
type CSIDriverSpec struct {
	// attachRequired indicates this CSI volume driver requires an attach
	// operation (because it implements the CSI ControllerPublishVolume()
	// method), and that the Kubernetes attach detach controller should call
	// the attach volume interface which checks the volumeattachment status
	// and waits until the volume is attached before proceeding to mounting.
	// The CSI external-attacher coordinates with CSI volume driver and updates
	// the volumeattachment status when the attach operation is complete.
	// If the CSIDriverRegistry feature gate is enabled and the value is
	// specified to false, the attach operation will be skipped.
	// Otherwise the attach operation will be called.
	//
	// This field is immutable.
	//
	// +optional
	AttachRequired *bool `json:"attachRequired,omitempty" protobuf:"varint,1,opt,name=attachRequired"`

	// podInfoOnMount indicates this CSI volume driver requires additional pod information (like podName, podUID, etc.)
	// during mount operations, if set to true.
	// If set to false, pod information will not be passed on mount.
	// Default is false.
	//
	// The CSI driver specifies podInfoOnMount as part of driver deployment.
	// If true, Kubelet will pass pod information as VolumeContext in the CSI NodePublishVolume() calls.
	// The CSI driver is responsible for parsing and validating the information passed in as VolumeContext.
	//
	// The following VolumeContext will be passed if podInfoOnMount is set to true.
	// This list might grow, but the prefix will be used.
	// "csi.storage.k8s.io/pod.name": pod.Name
	// "csi.storage.k8s.io/pod.namespace": pod.Namespace
	// "csi.storage.k8s.io/pod.uid": string(pod.UID)
	// "csi.storage.k8s.io/ephemeral": "true" if the volume is an ephemeral inline volume
	//                                 defined by a CSIVolumeSource, otherwise "false"
	//
	// "csi.storage.k8s.io/ephemeral" is a new feature in Kubernetes 1.16. It is only
	// required for drivers which support both the "Persistent" and "Ephemeral" VolumeLifecycleMode.
	// Other drivers can leave pod info disabled and/or ignore this field.
	// As Kubernetes 1.15 doesn't support this field, drivers can only support one mode when
	// deployed on such a cluster and the deployment determines which mode that is, for example
	// via a command line parameter of the driver.
	//
	// This field is immutable.
	//
	// +optional
	PodInfoOnMount *bool `json:"podInfoOnMount,omitempty" protobuf:"bytes,2,opt,name=podInfoOnMount"`

	// volumeLifecycleModes defines what kind of volumes this CSI volume driver supports.
	// The default if the list is empty is "Persistent", which is the usage defined by the
	// CSI specification and implemented in Kubernetes via the usual PV/PVC mechanism.
	//
	// The other mode is "Ephemeral". In this mode, volumes are defined inline inside the pod spec
	// with CSIVolumeSource and their lifecycle is tied to the lifecycle of that pod.
	// A driver has to be aware of this because it is only going to get a NodePublishVolume call for such a volume.
	//
	// For more information about implementing this mode, see
	// https://kubernetes-csi.github.io/docs/ephemeral-local-volumes.html
	// A driver can support one or more of these modes and
	// more modes may be added in the future.
	//
	// This field is immutable.
	//
	// +optional
	// +listType=atomic
	VolumeLifecycleModes []VolumeLifecycleMode `json:"volumeLifecycleModes,omitempty" protobuf:"bytes,3,opt,name=volumeLifecycleModes"`

	// storageCapacity indicates that the CSI volume driver wants pod scheduling to consider the storage
	// capacity that the driver deployment will report by creating
	// CSIStorageCapacity objects with capacity information, if set to true.
	//
	// The check can be enabled immediately when deploying a driver.
	// In that case, provisioning new volumes with late binding
	// will pause until the driver deployment has published
	// some suitable CSIStorageCapacity object.
	//
	// Alternatively, the driver can be deployed with the field
	// unset or false and it can be flipped later when storage
	// capacity information has been published.
	//
	// This field was immutable in Kubernetes <= 1.22 and now is mutable.
	//
	// +optional
	StorageCapacity *bool `json:"storageCapacity,omitempty" protobuf:"bytes,4,opt,name=storageCapacity"`

	// fsGroupPolicy defines if the underlying volume supports changing ownership and
	// permission of the volume before being mounted.
	// Refer to the specific FSGroupPolicy values for additional details.
	//
	// This field is immutable.
	//
	// Defaults to ReadWriteOnceWithFSType, which will examine each volume
	// to determine if Kubernetes should modify ownership and permissions of the volume.
	// With the default policy the defined fsGroup will only be applied
	// if a fstype is defined and the volume's access mode contains ReadWriteOnce.
	//
	// +optional
	FSGroupPolicy *FSGroupPolicy `json:"fsGroupPolicy,omitempty" protobuf:"bytes,5,opt,name=fsGroupPolicy"`

	// tokenRequests indicates the CSI driver needs pods' service account
	// tokens it is mounting volume for to do necessary authentication. Kubelet
	// will pass the tokens in VolumeContext in the CSI NodePublishVolume calls.
	// The CSI driver should parse and validate the following VolumeContext:
	// "csi.storage.k8s.io/serviceAccount.tokens": {
	//   "<audience>": {
	//     "token": <token>,
	//     "expirationTimestamp": <expiration timestamp in RFC3339>,
	//   },
	//   ...
	// }
	//
	// Note: Audience in each TokenRequest should be different and at
	// most one token is empty string. To receive a new token after expiry,
	// RequiresRepublish can be used to trigger NodePublishVolume periodically.
	//
	// +optional
	// +listType=atomic
	TokenRequests []TokenRequest `json:"tokenRequests,omitempty" protobuf:"bytes,6,opt,name=tokenRequests"`

	// requiresRepublish indicates the CSI driver wants `NodePublishVolume`
	// being periodically called to reflect any possible change in the mounted
	// volume. This field defaults to false.
	//
	// Note: After a successful initial NodePublishVolume call, subsequent calls
	// to NodePublishVolume should only update the contents of the volume. New
	// mount points will not be seen by a running container.
	//
	// +optional
	RequiresRepublish *bool `json:"requiresRepublish,omitempty" protobuf:"varint,7,opt,name=requiresRepublish"`

	// seLinuxMount specifies if the CSI driver supports "-o context"
	// mount option.
	//
	// When "true", the CSI driver must ensure that all volumes provided by this CSI
	// driver can be mounted separately with different `-o context` options. This is
	// typical for storage backends that provide volumes as filesystems on block
	// devices or as independent shared volumes.
	// Kubernetes will call NodeStage / NodePublish with "-o context=xyz" mount
	// option when mounting a ReadWriteOncePod volume used in Pod that has
	// explicitly set SELinux context. In the future, it may be expanded to other
	// volume AccessModes. In any case, Kubernetes will ensure that the volume is
	// mounted only with a single SELinux context.
	//
	// When "false", Kubernetes won't pass any special SELinux mount options to the driver.
	// This is typical for volumes that represent subdirectories of a bigger shared filesystem.
	//
	// Default is "false".
	//
	// +featureGate=SELinuxMountReadWriteOncePod
	// +optional
	SELinuxMount *bool `json:"seLinuxMount,omitempty" protobuf:"varint,8,opt,name=seLinuxMount"`
}

// FSGroupPolicy specifies if a CSI Driver supports modifying
// volume ownership and permissions of the volume to be mounted.
// More modes may be added in the future.
type FSGroupPolicy string

const (
	// ReadWriteOnceWithFSTypeFSGroupPolicy indicates that each volume will be examined
	// to determine if the volume ownership and permissions
	// should be modified. If a fstype is defined and the volume's access mode
	// contains ReadWriteOnce, then the defined fsGroup will be applied.
	// This is the default behavior if no other FSGroupPolicy is defined.
	ReadWriteOnceWithFSTypeFSGroupPolicy FSGroupPolicy = "ReadWriteOnceWithFSType"

	// FileFSGroupPolicy indicates that CSI driver supports volume ownership
	// and permission change via fsGroup, and Kubernetes will change the permissions
	// and ownership of every file in the volume to match the user requested fsGroup in
	// the pod's SecurityPolicy regardless of fstype or access mode.
	// Use this mode if Kubernetes should modify the permissions and ownership
	// of the volume.
	FileFSGroupPolicy FSGroupPolicy = "File"

	// None indicates that volumes will be mounted without performing
	// any ownership or permission modifications, as the CSIDriver does not support
	// these operations.
	NoneFSGroupPolicy FSGroupPolicy = "None"
)

// VolumeLifecycleMode is an enumeration of possible usage modes for a volume
// provided by a CSI driver. More modes may be added in the future.
type VolumeLifecycleMode string

// TokenRequest contains parameters of a service account token.
type TokenRequest struct {
	// audience is the intended audience of the token in "TokenRequestSpec".
	// It will default to the audiences of kube apiserver.
	Audience string `json:"audience" protobuf:"bytes,1,opt,name=audience"`

	// expirationSeconds is the duration of validity of the token in "TokenRequestSpec".
	// It has the same default value of "ExpirationSeconds" in "TokenRequestSpec"
	//
	// +optional
	ExpirationSeconds *int64 `json:"expirationSeconds,omitempty" protobuf:"varint,2,opt,name=expirationSeconds"`
}

const (
	// VolumeLifecyclePersistent explicitly confirms that the driver implements
	// the full CSI spec. It is the default when CSIDriverSpec.VolumeLifecycleModes is not
	// set. Such volumes are managed in Kubernetes via the persistent volume
	// claim mechanism and have a lifecycle that is independent of the pods which
	// use them.
	VolumeLifecyclePersistent VolumeLifecycleMode = "Persistent"

	// VolumeLifecycleEphemeral indicates that the driver can be used for
	// ephemeral inline volumes. Such volumes are specified inside the pod
	// spec with a CSIVolumeSource and, as far as Kubernetes is concerned, have
	// a lifecycle that is tied to the lifecycle of the pod. For example, such
	// a volume might contain data that gets created specifically for that pod,
	// like secrets.
	// But how the volume actually gets created and managed is entirely up to
	// the driver. It might also use reference counting to share the same volume
	// instance among different pods if the CSIVolumeSource of those pods is
	// identical.
	VolumeLifecycleEphemeral VolumeLifecycleMode = "Ephemeral"
)

// +genclient
// +genclient:nonNamespaced
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:prerelease-lifecycle-gen:introduced=1.14
// +k8s:prerelease-lifecycle-gen:deprecated=1.17
// +k8s:prerelease-lifecycle-gen:removed=1.22
// +k8s:prerelease-lifecycle-gen:replacement=storage.k8s.io,v1,CSINode

// DEPRECATED - This group version of CSINode is deprecated by storage/v1/CSINode.
// See the release notes for more information.
// CSINode holds information about all CSI drivers installed on a node.
// CSI drivers do not need to create the CSINode object directly. As long as
// they use the node-driver-registrar sidecar container, the kubelet will
// automatically populate the CSINode object for the CSI driver as part of
// kubelet plugin registration.
// CSINode has the same name as a node. If the object is missing, it means either
// there are no CSI Drivers available on the node, or the Kubelet version is low
// enough that it doesn't create this object.
// CSINode has an OwnerReference that points to the corresponding node object.
type CSINode struct {
	metav1.TypeMeta `json:",inline"`

	// metadata.name must be the Kubernetes node name.
	metav1.ObjectMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	// spec is the specification of CSINode
	Spec CSINodeSpec `json:"spec" protobuf:"bytes,2,opt,name=spec"`
}

// CSINodeSpec holds information about the specification of all CSI drivers installed on a node
type CSINodeSpec struct {
	// drivers is a list of information of all CSI Drivers existing on a node.
	// If all drivers in the list are uninstalled, this can become empty.
	// +patchMergeKey=name
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=name
	Drivers []CSINodeDriver `json:"drivers" patchStrategy:"merge" patchMergeKey:"name" protobuf:"bytes,1,rep,name=drivers"`
}

// CSINodeDriver holds information about the specification of one CSI driver installed on a node
type CSINodeDriver struct {
	// name represents the name of the CSI driver that this object refers to.
	// This MUST be the same name returned by the CSI GetPluginName() call for
	// that driver.
	Name string `json:"name" protobuf:"bytes,1,opt,name=name"`

	// nodeID of the node from the driver point of view.
	// This field enables Kubernetes to communicate with storage systems that do
	// not share the same nomenclature for nodes. For example, Kubernetes may
	// refer to a given node as "node1", but the storage system may refer to
	// the same node as "nodeA". When Kubernetes issues a command to the storage
	// system to attach a volume to a specific node, it can use this field to
	// refer to the node name using the ID that the storage system will
	// understand, e.g. "nodeA" instead of "node1". This field is required.
	NodeID string `json:"nodeID" protobuf:"bytes,2,opt,name=nodeID"`

	// topologyKeys is the list of keys supported by the driver.
	// When a driver is initialized on a cluster, it provides a set of topology
	// keys that it understands (e.g. "company.com/zone", "company.com/region").
	// When a driver is initialized on a node, it provides the same topology keys
	// along with values. Kubelet will expose these topology keys as labels
	// on its own node object.
	// When Kubernetes does topology aware provisioning, it can use this list to
	// determine which labels it should retrieve from the node object and pass
	// back to the driver.
	// It is possible for different nodes to use different topology keys.
	// This can be empty if driver does not support topology.
	// +optional
	// +listType=atomic
	TopologyKeys []string `json:"topologyKeys" protobuf:"bytes,3,rep,name=topologyKeys"`

	// allocatable represents the volume resources of a node that are available for scheduling.
	// +optional
	Allocatable *VolumeNodeResources `json:"allocatable,omitempty" protobuf:"bytes,4,opt,name=allocatable"`
}

// VolumeNodeResources is a set of resource limits for scheduling of volumes.
type VolumeNodeResources struct {
	// count indicates the maximum number of unique volumes managed by the CSI driver that can be used on a node.
	// A volume that is both attached and mounted on a node is considered to be used once, not twice.
	// The same rule applies for a unique volume that is shared among multiple pods on the same node.
	// If this field is nil, then the supported number of volumes on this node is unbounded.
	// +optional
	Count *int32 `json:"count,omitempty" protobuf:"varint,1,opt,name=count"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:prerelease-lifecycle-gen:introduced=1.14
// +k8s:prerelease-lifecycle-gen:deprecated=1.17
// +k8s:prerelease-lifecycle-gen:removed=1.22
// +k8s:prerelease-lifecycle-gen:replacement=storage.k8s.io,v1,CSINode

// CSINodeList is a collection of CSINode objects.
type CSINodeList struct {
	metav1.TypeMeta `json:",inline"`

	// Standard list metadata
	// More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#metadata
	// +optional
	metav1.ListMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	// items is the list of CSINode
	Items []CSINode `json:"items" protobuf:"bytes,2,rep,name=items"`
}

// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:prerelease-lifecycle-gen:introduced=1.21
// +k8s:prerelease-lifecycle-gen:deprecated=1.24
// +k8s:prerelease-lifecycle-gen:replacement=storage.k8s.io,v1,CSIStorageCapacity

// CSIStorageCapacity stores the result of one CSI GetCapacity call.
// For a given StorageClass, this describes the available capacity in a
// particular topology segment.  This can be used when considering where to
// instantiate new PersistentVolumes.
//
// For example this can express things like:
// - StorageClass "standard" has "1234 GiB" available in "topology.kubernetes.io/zone=us-east1"
// - StorageClass "localssd" has "10 GiB" available in "kubernetes.io/hostname=knode-abc123"
//
// The following three cases all imply that no capacity is available for
// a certain combination:
// - no object exists with suitable topology and storage class name
// - such an object exists, but the capacity is unset
// - such an object exists, but the capacity is zero
//
// The producer of these objects can decide which approach is more suitable.
//
// They are consumed by the kube-scheduler when a CSI driver opts into
// capacity-aware scheduling with CSIDriverSpec.StorageCapacity. The scheduler
// compares the MaximumVolumeSize against the requested size of pending volumes
// to filter out unsuitable nodes. If MaximumVolumeSize is unset, it falls back
// to a comparison against the less precise Capacity. If that is also unset,
// the scheduler assumes that capacity is insufficient and tries some other
// node.
type CSIStorageCapacity struct {
	metav1.TypeMeta `json:",inline"`

	// Standard object's metadata. The name has no particular meaning. It must be
	// be a DNS subdomain (dots allowed, 253 characters). To ensure that
	// there are no conflicts with other CSI drivers on the cluster, the recommendation
	// is to use csisc-<uuid>, a generated name, or a reverse-domain name which ends
	// with the unique CSI driver name.
	//
	// Objects are namespaced.
	//
	// More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	// nodeTopology defines which nodes have access to the storage
	// for which capacity was reported. If not set, the storage is
	// not accessible from any node in the cluster. If empty, the
	// storage is accessible from all nodes. This field is
	// immutable.
	//
	// +optional
	NodeTopology *metav1.LabelSelector `json:"nodeTopology,omitempty" protobuf:"bytes,2,opt,name=nodeTopology"`

	// storageClassName represents the name of the StorageClass that the reported capacity applies to.
	// It must meet the same requirements as the name of a StorageClass
	// object (non-empty, DNS subdomain). If that object no longer exists,
	// the CSIStorageCapacity object is obsolete and should be removed by its
	// creator.
	// This field is immutable.
	StorageClassName string `json:"storageClassName" protobuf:"bytes,3,name=storageClassName"`

	// capacity is the value reported by the CSI driver in its GetCapacityResponse
	// for a GetCapacityRequest with topology and parameters that match the
	// previous fields.
	//
	// The semantic is currently (CSI spec 1.2) defined as:
	// The available capacity, in bytes, of the storage that can be used
	// to provision volumes. If not set, that information is currently
	// unavailable.
	//
	// +optional
	Capacity *resource.Quantity `json:"capacity,omitempty" protobuf:"bytes,4,opt,name=capacity"`

	// maximumVolumeSize is the value reported by the CSI driver in its GetCapacityResponse
	// for a GetCapacityRequest with topology and parameters that match the
	// previous fields.
	//
	// This is defined since CSI spec 1.4.0 as the largest size
	// that may be used in a
	// CreateVolumeRequest.capacity_range.required_bytes field to
	// create a volume with the same parameters as those in
	// GetCapacityRequest. The corresponding value in the Kubernetes
	// API is ResourceRequirements.Requests in a volume claim.
	//
	// +optional
	MaximumVolumeSize *resource.Quantity `json:"maximumVolumeSize,omitempty" protobuf:"bytes,5,opt,name=maximumVolumeSize"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:prerelease-lifecycle-gen:introduced=1.21
// +k8s:prerelease-lifecycle-gen:deprecated=1.24
// +k8s:prerelease-lifecycle-gen:replacement=storage.k8s.io,v1,CSIStorageCapacityList

// CSIStorageCapacityList is a collection of CSIStorageCapacity objects.
type CSIStorageCapacityList struct {
	metav1.TypeMeta `json:",inline"`

	// Standard list metadata
	// More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#metadata
	// +optional
	metav1.ListMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	// items is the list of CSIStorageCapacity objects.
	Items []CSIStorageCapacity `json:"items" protobuf:"bytes,2,rep,name=items"`
}

// +genclient
// +genclient:nonNamespaced
// +k8s:prerelease-lifecycle-gen:introduced=1.31
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// VolumeAttributesClass represents a specification of mutable volume attributes
// defined by the CSI driver. The class can be specified during dynamic provisioning
// of PersistentVolumeClaims, and changed in the PersistentVolumeClaim spec after provisioning.
type VolumeAttributesClass struct {
	metav1.TypeMeta `json:",inline"`

	// Standard object's metadata.
	// More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	// Name of the CSI driver
	// This field is immutable.
	DriverName string `json:"driverName" protobuf:"bytes,2,opt,name=driverName"`

	// parameters hold volume attributes defined by the CSI driver. These values
	// are opaque to the Kubernetes and are passed directly to the CSI driver.
	// The underlying storage provider supports changing these attributes on an
	// existing volume, however the parameters field itself is immutable. To
	// invoke a volume update, a new VolumeAttributesClass should be created with
	// new parameters, and the PersistentVolumeClaim should be updated to reference
	// the new VolumeAttributesClass.
	//
	// This field is required and must contain at least one key/value pair.
	// The keys cannot be empty, and the maximum number of parameters is 512, with
	// a cumulative max size of 256K. If the CSI driver rejects invalid parameters,
	// the target PersistentVolumeClaim will be set to an "Infeasible" state in the
	// modifyVolumeStatus field.
	Parameters map[string]string `json:"parameters,omitempty" protobuf:"bytes,3,rep,name=parameters"`
}

// +k8s:prerelease-lifecycle-gen:introduced=1.31
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// VolumeAttributesClassList is a collection of VolumeAttributesClass objects.
type VolumeAttributesClassList struct {
	metav1.TypeMeta `json:",inline"`

	// Standard list metadata
	// More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#metadata
	// +optional
	metav1.ListMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	// items is the list of VolumeAttributesClass objects.
	Items []VolumeAttributesClass `json:"items" protobuf:"bytes,2,rep,name=items"`
}
