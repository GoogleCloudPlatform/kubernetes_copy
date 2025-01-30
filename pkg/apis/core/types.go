/*
Copyright 2014 The Kubernetes Authors.

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

package core

import (
	"k8s.io/apimachinery/pkg/api/resource"
	metainternalversion "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
)

const (
	// NamespaceDefault means the object is in the default namespace which is applied when not specified by clients
	NamespaceDefault = "default"
	// NamespaceAll is the default argument to specify on a context when you want to list or filter resources across all namespaces
	NamespaceAll = ""
	// NamespaceNone is the argument for a context when there is no namespace.
	NamespaceNone = ""
	// NamespaceSystem is the system namespace where we place system components.
	NamespaceSystem = "kube-system"
	// NamespacePublic is the namespace where we place public info (ConfigMaps)
	NamespacePublic = "kube-public"
	// NamespaceNodeLease is the namespace where we place node lease objects (used for node heartbeats)
	NamespaceNodeLease = "kube-node-lease"
	// TerminationMessagePathDefault means the default path to capture the application termination message running in a container
	TerminationMessagePathDefault = "/dev/termination-log"
)

// Volume represents a named volume in a pod that may be accessed by any containers in the pod.
type Volume struct {
	// Required: This must be a DNS_LABEL.  Each volume in a pod must have
	// a unique name.
	Name string
	// The VolumeSource represents the location and type of a volume to mount.
	// This is optional for now. If not specified, the Volume is implied to be an EmptyDir.
	// This implied behavior is deprecated and will be removed in a future version.
	// +optional
	VolumeSource
}

// VolumeSource represents the source location of a volume to mount.
// Only one of its members may be specified.
type VolumeSource struct {
	// hostPath represents file or directory on the host machine that is
	// directly exposed to the container. This is generally used for system
	// agents or other privileged things that are allowed to see the host
	// machine. Most containers will NOT need this.
	// ---
	// TODO(jonesdl) We need to restrict who can use host directory mounts and who can/can not
	// mount host directories as read/write.
	// +optional
	HostPath *HostPathVolumeSource
	// emptyDir represents a temporary directory that shares a pod's lifetime.
	// +optional
	EmptyDir *EmptyDirVolumeSource
	// gcePersistentDisk represents a GCE Disk resource that is attached to a
	// kubelet's host machine and then exposed to the pod.
	// Deprecated: GCEPersistentDisk is deprecated. All operations for the in-tree
	// gcePersistentDisk type are redirected to the pd.csi.storage.gke.io CSI driver.
	// +optional
	GCEPersistentDisk *GCEPersistentDiskVolumeSource
	// awsElasticBlockStore represents an AWS EBS disk that is attached to a
	// kubelet's host machine and then exposed to the pod.
	// Deprecated: AWSElasticBlockStore is deprecated. All operations for the in-tree
	// awsElasticBlockStore type are redirected to the ebs.csi.aws.com CSI driver.
	// +optional
	AWSElasticBlockStore *AWSElasticBlockStoreVolumeSource
	// gitRepo represents a git repository at a particular revision.
	// Deprecated: GitRepo is deprecated. To provision a container with a git repo, mount an
	// EmptyDir into an InitContainer that clones the repo using git, then mount the EmptyDir
	// into the Pod's container.
	// +optional
	GitRepo *GitRepoVolumeSource
	// secret represents a secret that should populate this volume.
	// +optional
	Secret *SecretVolumeSource
	// nfs represents an NFS mount on the host that shares a pod's lifetime
	// +optional
	NFS *NFSVolumeSource
	// iscsi represents an ISCSI Disk resource that is attached to a
	// kubelet's host machine and then exposed to the pod.
	// +optional
	ISCSI *ISCSIVolumeSource
	// glusterfs represents a Glusterfs mount on the host that shares a pod's lifetime.
	// Deprecated: Glusterfs is deprecated and the in-tree glusterfs type is no longer supported.
	// +optional
	Glusterfs *GlusterfsVolumeSource
	// persistentVolumeClaim represents a reference to a PersistentVolumeClaim in the same namespace
	// +optional
	PersistentVolumeClaim *PersistentVolumeClaimVolumeSource
	// rdb represents a Rados Block Device mount on the host that shares a pod's lifetime.
	// Deprecated: RBD is deprecated and the in-tree rbd type is no longer supported.
	// +optional
	RBD *RBDVolumeSource

	// quobyte represents a Quobyte mount on the host that shares a pod's lifetime.
	// Deprecated: Quobyte is deprecated and the in-tree quobyte type is no longer supported.
	// +optional
	Quobyte *QuobyteVolumeSource

	// flexVolume represents a generic volume resource that is
	// provisioned/attached using an exec based plugin.
	// Deprecated: FlexVolume is deprecated. Consider using a CSIDriver instead.
	// +optional
	FlexVolume *FlexVolumeSource

	// cinder represents a cinder volume attached and mounted on kubelet's host machine.
	// Deprecated: Cinder is deprecated. All operations for the in-tree cinder type
	// are redirected to the cinder.csi.openstack.org CSI driver.
	// +optional
	Cinder *CinderVolumeSource

	// cephFS represents a Cephfs mount on the host that shares a pod's lifetime.
	// Deprecated: CephFS is deprecated and the in-tree cephfs type is no longer supported.
	// +optional
	CephFS *CephFSVolumeSource

	// flocker represents a Flocker volume attached to a kubelet's host machine. This depends on the Flocker control service being running.
	// Deprecated: Flocker is deprecated and the in-tree flocker type is no longer supported.
	// +optional
	Flocker *FlockerVolumeSource

	// downwardAPI represents metadata about the pod that should populate this volume
	// +optional
	DownwardAPI *DownwardAPIVolumeSource
	// fc represents a Fibre Channel resource that is attached to a kubelet's host machine and then exposed to the pod.
	// +optional
	FC *FCVolumeSource
	// azureFile represents an Azure File Service mount on the host and bind mount to the pod.
	// Deprecated: AzureFile is deprecated. All operations for the in-tree azureFile type
	// are redirected to the file.csi.azure.com CSI driver.
	// +optional
	AzureFile *AzureFileVolumeSource
	// ConfigMap represents a configMap that should populate this volume
	// +optional
	ConfigMap *ConfigMapVolumeSource
	// vsphereVolume represents a vSphere volume attached and mounted on kubelet's host machine.
	// Deprecated: VsphereVolume is deprecated. All operations for the in-tree vsphereVolume type
	// are redirected to the csi.vsphere.vmware.com CSI driver.
	// +optional
	VsphereVolume *VsphereVirtualDiskVolumeSource
	// azureDisk represents an Azure Data Disk mount on the host and bind mount to the pod.
	// Deprecated: AzureDisk is deprecated. All operations for the in-tree azureDisk type
	// are redirected to the disk.csi.azure.com CSI driver.
	// +optional
	AzureDisk *AzureDiskVolumeSource
	// photonPersistentDisk represents a PhotonController persistent disk attached and mounted on kubelets host machine.
	// Deprecated: PhotonPersistentDisk is deprecated and the in-tree photonPersistentDisk type is no longer supported.
	PhotonPersistentDisk *PhotonPersistentDiskVolumeSource
	// Items for all in one resources secrets, configmaps, and downward API
	Projected *ProjectedVolumeSource
	// portworxVolume represents a portworx volume attached and mounted on kubelets host machine.
	// Deprecated: PortworxVolume is deprecated. All operations for the in-tree portworxVolume type
	// are redirected to the pxd.portworx.com CSI driver when the CSIMigrationPortworx feature-gate
	// is on.
	// +optional
	PortworxVolume *PortworxVolumeSource
	// scaleIO represents a ScaleIO persistent volume attached and mounted on Kubernetes nodes.
	// Deprecated: ScaleIO is deprecated and the in-tree scaleIO type is no longer supported.
	// +optional
	ScaleIO *ScaleIOVolumeSource
	// storageOS represents a StorageOS volume that is attached to the kubelet's host machine and mounted into the pod.
	// Deprecated: StorageOS is deprecated and the in-tree storageos type is no longer supported.
	// +optional
	StorageOS *StorageOSVolumeSource
	// csi (Container Storage Interface) represents ephemeral storage that is handled by certain external CSI drivers.
	// +optional
	CSI *CSIVolumeSource
	// ephemeral represents a volume that is handled by a cluster storage driver.
	// The volume's lifecycle is tied to the pod that defines it - it will be created before the pod starts,
	// and deleted when the pod is removed.
	//
	// Use this if:
	// a) the volume is only needed while the pod runs,
	// b) features of normal volumes like restoring from snapshot or capacity
	//    tracking are needed,
	// c) the storage driver is specified through a storage class, and
	// d) the storage driver supports dynamic volume provisioning through
	//    a PersistentVolumeClaim (see EphemeralVolumeSource for more
	//    information on the connection between this volume type
	//    and PersistentVolumeClaim).
	//
	// Use PersistentVolumeClaim or one of the vendor-specific
	// APIs for volumes that persist for longer than the lifecycle
	// of an individual pod.
	//
	// Use CSI for light-weight local ephemeral volumes if the CSI driver is meant to
	// be used that way - see the documentation of the driver for
	// more information.
	//
	// A pod can use both types of ephemeral volumes and
	// persistent volumes at the same time.
	//
	// +optional
	Ephemeral *EphemeralVolumeSource
	// image represents an OCI object (a container image or artifact) pulled and mounted on the kubelet's host machine.
	// The volume is resolved at pod startup depending on which PullPolicy value is provided:
	//
	// - Always: the kubelet always attempts to pull the reference. Container creation will fail If the pull fails.
	// - Never: the kubelet never pulls the reference and only uses a local image or artifact. Container creation will fail if the reference isn't present.
	// - IfNotPresent: the kubelet pulls if the reference isn't already present on disk. Container creation will fail if the reference isn't present and the pull fails.
	//
	// The volume gets re-resolved if the pod gets deleted and recreated, which means that new remote content will become available on pod recreation.
	// A failure to resolve or pull the image during pod startup will block containers from starting and may add significant latency. Failures will be retried using normal volume backoff and will be reported on the pod reason and message.
	// The types of objects that may be mounted by this volume are defined by the container runtime implementation on a host machine and at minimum must include all valid types supported by the container image field.
	// The OCI object gets mounted in a single directory (spec.containers[*].volumeMounts.mountPath) by merging the manifest layers in the same way as for container images.
	// The volume will be mounted read-only (ro) and non-executable files (noexec).
	// Sub path mounts for containers are not supported (spec.containers[*].volumeMounts.subpath).
	// The field spec.securityContext.fsGroupChangePolicy has no effect on this volume type.
	// +featureGate=ImageVolume
	// +optional
	Image *ImageVolumeSource
}

// PersistentVolumeSource is similar to VolumeSource but meant for the administrator who creates PVs.
// Exactly one of its members must be set.
type PersistentVolumeSource struct {
	// gcePersistentDisk represents a GCE Disk resource that is attached to a
	// kubelet's host machine and then exposed to the pod. Provisioned by an admin.
	// Deprecated: GCEPersistentDisk is deprecated. All operations for the in-tree
	// gcePersistentDisk type are redirected to the pd.csi.storage.gke.io CSI driver.
	// +optional
	GCEPersistentDisk *GCEPersistentDiskVolumeSource
	// awsElasticBlockStore represents an AWS Disk resource that is attached to a
	// kubelet's host machine and then exposed to the pod.
	// Deprecated: AWSElasticBlockStore is deprecated. All operations for the in-tree
	// awsElasticBlockStore type are redirected to the ebs.csi.aws.com CSI driver.
	// +optional
	AWSElasticBlockStore *AWSElasticBlockStoreVolumeSource
	// hostPath represents a directory on the host.
	// Provisioned by a developer or tester.
	// This is useful for single-node development and testing only!
	// On-host storage is not supported in any way and WILL NOT WORK in a multi-node cluster.
	// +optional
	HostPath *HostPathVolumeSource
	// glusterfs represents a Glusterfs volume that is attached to a host and
	// exposed to the pod. Provisioned by an admin.
	// Deprecated: Glusterfs is deprecated and the in-tree glusterfs type is no longer supported.
	// +optional
	Glusterfs *GlusterfsPersistentVolumeSource
	// nfs represents an NFS mount on the host that shares a pod's lifetime
	// +optional
	NFS *NFSVolumeSource
	// rbd represents a Rados Block Device mount on the host that shares a pod's lifetime.
	// Deprecated: RBD is deprecated and the in-tree rbd type is no longer supported.
	// +optional
	RBD *RBDPersistentVolumeSource
	// quobyte represents a Quobyte mount on the host that shares a pod's lifetime.
	// Deprecated: Quobyte is deprecated and the in-tree quobyte type is no longer supported.
	// +optional
	Quobyte *QuobyteVolumeSource
	// iscsi represents an ISCSI resource that is attached to a
	// kubelet's host machine and then exposed to the pod.
	// +optional
	ISCSI *ISCSIPersistentVolumeSource
	// flexVolume represents a generic volume resource that is
	// provisioned/attached using an exec based plugin.
	// Deprecated: FlexVolume is deprecated. Consider using a CSIDriver instead.
	// +optional
	FlexVolume *FlexPersistentVolumeSource
	// cinder represents a cinder volume attached and mounted on kubelets host machine.
	// Deprecated: Cinder is deprecated. All operations for the in-tree cinder type
	// are redirected to the cinder.csi.openstack.org CSI driver.
	// +optional
	Cinder *CinderPersistentVolumeSource
	// cephFS represents a Ceph FS mount on the host that shares a pod's lifetime.
	// Deprecated: CephFS is deprecated and the in-tree cephfs type is no longer supported.
	// +optional
	CephFS *CephFSPersistentVolumeSource
	// fc represents a Fibre Channel resource that is attached to a kubelet's host machine and then exposed to the pod.
	// +optional
	FC *FCVolumeSource
	// flocker represents a Flocker volume attached to a kubelet's host machine and exposed to the pod for its usage. This depends on the Flocker control service being running.
	// Deprecated: Flocker is deprecated and the in-tree flocker type is no longer supported.
	// +optional
	Flocker *FlockerVolumeSource
	// azureFile represents an Azure File Service mount on the host and bind mount to the pod.
	// Deprecated: AzureFile is deprecated. All operations for the in-tree azureFile type
	// are redirected to the file.csi.azure.com CSI driver.
	// +optional
	AzureFile *AzureFilePersistentVolumeSource
	// vsphereVolume represents a vSphere volume attached and mounted on kubelets host machine.
	// Deprecated: VsphereVolume is deprecated. All operations for the in-tree vsphereVolume type
	// are redirected to the csi.vsphere.vmware.com CSI driver.
	// +optional
	VsphereVolume *VsphereVirtualDiskVolumeSource
	// azureDisk represents an Azure Data Disk mount on the host and bind mount to the pod.
	// Deprecated: AzureDisk is deprecated. All operations for the in-tree azureDisk type
	// are redirected to the disk.csi.azure.com CSI driver.
	// +optional
	AzureDisk *AzureDiskVolumeSource
	// photonPersistentDisk represents a PhotonController persistent disk attached and mounted on kubelets host machine.
	// Deprecated: PhotonPersistentDisk is deprecated and the in-tree photonPersistentDisk type is no longer supported.
	PhotonPersistentDisk *PhotonPersistentDiskVolumeSource
	// portworxVolume represents a portworx volume attached and mounted on kubelets host machine.
	// Deprecated: PortworxVolume is deprecated. All operations for the in-tree portworxVolume type
	// are redirected to the pxd.portworx.com CSI driver when the CSIMigrationPortworx feature-gate
	// is on.
	// +optional
	PortworxVolume *PortworxVolumeSource
	// scaleIO represents a ScaleIO persistent volume attached and mounted on Kubernetes nodes.
	// Deprecated: ScaleIO is deprecated and the in-tree scaleIO type is no longer supported.
	// +optional
	ScaleIO *ScaleIOPersistentVolumeSource
	// local represents directly-attached storage with node affinity
	// +optional
	Local *LocalVolumeSource
	// storageOS represents a StorageOS volume that is attached to the kubelet's host machine and mounted into the pod.
	// Deprecated: StorageOS is deprecated and the in-tree storageos type is no longer supported.
	// +optional
	StorageOS *StorageOSPersistentVolumeSource
	// csi represents storage that is handled by an external CSI driver.
	// +optional
	CSI *CSIPersistentVolumeSource
}

// PersistentVolumeClaimVolumeSource represents a reference to a PersistentVolumeClaim in the same namespace
type PersistentVolumeClaimVolumeSource struct {
	// ClaimName is the name of a PersistentVolumeClaim in the same namespace as the pod using this volume
	ClaimName string
	// Optional: Defaults to false (read/write).  ReadOnly here
	// will force the ReadOnly setting in VolumeMounts
	// +optional
	ReadOnly bool
}

const (
	// BetaStorageClassAnnotation represents the beta/previous StorageClass annotation.
	// It's deprecated and will be removed in a future release. (#51440)
	BetaStorageClassAnnotation = "volume.beta.kubernetes.io/storage-class"

	// MountOptionAnnotation defines mount option annotation used in PVs
	MountOptionAnnotation = "volume.beta.kubernetes.io/mount-options"
)

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// PersistentVolume struct captures the details of the implementation of PV storage
type PersistentVolume struct {
	metav1.TypeMeta
	// +optional
	metav1.ObjectMeta

	// Spec defines a persistent volume owned by the cluster
	// +optional
	Spec PersistentVolumeSpec

	// Status represents the current information about persistent volume.
	// +optional
	Status PersistentVolumeStatus
}

// PersistentVolumeSpec has most of the details required to define a persistent volume
type PersistentVolumeSpec struct {
	// Resources represents the actual resources of the volume
	Capacity ResourceList
	// Source represents the location and type of a volume to mount.
	PersistentVolumeSource
	// AccessModes contains all ways the volume can be mounted
	// +optional
	AccessModes []PersistentVolumeAccessMode
	// ClaimRef is part of a bi-directional binding between PersistentVolume and PersistentVolumeClaim.
	// ClaimRef is expected to be non-nil when bound.
	// claim.VolumeName is the authoritative bind between PV and PVC.
	// When set to non-nil value, PVC.Spec.Selector of the referenced PVC is
	// ignored, i.e. labels of this PV do not need to match PVC selector.
	// +optional
	ClaimRef *ObjectReference
	// Optional: what happens to a persistent volume when released from its claim.
	// +optional
	PersistentVolumeReclaimPolicy PersistentVolumeReclaimPolicy
	// Name of StorageClass to which this persistent volume belongs. Empty value
	// means that this volume does not belong to any StorageClass.
	// +optional
	StorageClassName string
	// A list of mount options, e.g. ["ro", "soft"]. Not validated - mount will
	// simply fail if one is invalid.
	// +optional
	MountOptions []string
	// volumeMode defines if a volume is intended to be used with a formatted filesystem
	// or to remain in raw block state. Value of Filesystem is implied when not included in spec.
	// +optional
	VolumeMode *PersistentVolumeMode
	// NodeAffinity defines constraints that limit what nodes this volume can be accessed from.
	// This field influences the scheduling of pods that use this volume.
	// +optional
	NodeAffinity *VolumeNodeAffinity
	// Name of VolumeAttributesClass to which this persistent volume belongs. Empty value
	// is not allowed. When this field is not set, it indicates that this volume does not belong to any
	// VolumeAttributesClass. This field is mutable and can be changed by the CSI driver
	// after a volume has been updated successfully to a new class.
	// For an unbound PersistentVolume, the volumeAttributesClassName will be matched with unbound
	// PersistentVolumeClaims during the binding process.
	// This is a beta field and requires enabling VolumeAttributesClass feature (off by default).
	// +featureGate=VolumeAttributesClass
	// +optional
	VolumeAttributesClassName *string
}

// VolumeNodeAffinity defines constraints that limit what nodes this volume can be accessed from.
type VolumeNodeAffinity struct {
	// Required specifies hard node constraints that must be met.
	Required *NodeSelector
}

// PersistentVolumeReclaimPolicy describes a policy for end-of-life maintenance of persistent volumes
type PersistentVolumeReclaimPolicy string

const (
	// PersistentVolumeReclaimRecycle means the volume will be recycled back into the pool of unbound persistent volumes on release from its claim.
	// The volume plugin must support Recycling.
	// DEPRECATED: The PersistentVolumeReclaimRecycle called Recycle is being deprecated. See announcement here: https://groups.google.com/forum/#!topic/kubernetes-dev/uexugCza84I
	PersistentVolumeReclaimRecycle PersistentVolumeReclaimPolicy = "Recycle"
	// PersistentVolumeReclaimDelete means the volume will be deleted from Kubernetes on release from its claim.
	// The volume plugin must support Deletion.
	PersistentVolumeReclaimDelete PersistentVolumeReclaimPolicy = "Delete"
	// PersistentVolumeReclaimRetain means the volume will be left in its current phase (Released) for manual reclamation by the administrator.
	// The default policy is Retain.
	PersistentVolumeReclaimRetain PersistentVolumeReclaimPolicy = "Retain"
)

// PersistentVolumeMode describes how a volume is intended to be consumed, either Block or Filesystem.
type PersistentVolumeMode string

const (
	// PersistentVolumeBlock means the volume will not be formatted with a filesystem and will remain a raw block device.
	PersistentVolumeBlock PersistentVolumeMode = "Block"
	// PersistentVolumeFilesystem means the volume will be or is formatted with a filesystem.
	PersistentVolumeFilesystem PersistentVolumeMode = "Filesystem"
)

// PersistentVolumeStatus represents the status of PV storage
type PersistentVolumeStatus struct {
	// Phase indicates if a volume is available, bound to a claim, or released by a claim
	// +optional
	Phase PersistentVolumePhase
	// A human-readable message indicating details about why the volume is in this state.
	// +optional
	Message string
	// Reason is a brief CamelCase string that describes any failure and is meant for machine parsing and tidy display in the CLI
	// +optional
	Reason string
	// LastPhaseTransitionTime is the time the phase transitioned from one to another
	// and automatically resets to current time everytime a volume phase transitions.
	// +optional
	LastPhaseTransitionTime *metav1.Time
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// PersistentVolumeList represents a list of PVs
type PersistentVolumeList struct {
	metav1.TypeMeta
	// +optional
	metav1.ListMeta
	Items []PersistentVolume
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// PersistentVolumeClaim is a user's request for and claim to a persistent volume
type PersistentVolumeClaim struct {
	metav1.TypeMeta
	// +optional
	metav1.ObjectMeta

	// Spec defines the volume requested by a pod author
	// +optional
	Spec PersistentVolumeClaimSpec

	// Status represents the current information about a claim
	// +optional
	Status PersistentVolumeClaimStatus
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// PersistentVolumeClaimList represents the list of PV claims
type PersistentVolumeClaimList struct {
	metav1.TypeMeta
	// +optional
	metav1.ListMeta
	Items []PersistentVolumeClaim
}

// PersistentVolumeClaimSpec describes the common attributes of storage devices
// and allows a Source for provider-specific attributes
type PersistentVolumeClaimSpec struct {
	// Contains the types of access modes required
	// +optional
	AccessModes []PersistentVolumeAccessMode
	// A label query over volumes to consider for binding. This selector is
	// ignored when VolumeName is set
	// +optional
	Selector *metav1.LabelSelector
	// Resources represents the minimum resources required
	// If RecoverVolumeExpansionFailure feature is enabled users are allowed to specify resource requirements
	// that are lower than previous value but must still be higher than capacity recorded in the
	// status field of the claim.
	// +optional
	Resources VolumeResourceRequirements
	// VolumeName is the binding reference to the PersistentVolume backing this
	// claim. When set to non-empty value Selector is not evaluated
	// +optional
	VolumeName string
	// Name of the StorageClass required by the claim.
	// More info: https://kubernetes.io/docs/concepts/storage/persistent-volumes/#class-1
	// +optional
	StorageClassName *string
	// volumeMode defines what type of volume is required by the claim.
	// Value of Filesystem is implied when not included in claim spec.
	// +optional
	VolumeMode *PersistentVolumeMode
	// This field can be used to specify either:
	// * An existing VolumeSnapshot object (snapshot.storage.k8s.io/VolumeSnapshot)
	// * An existing PVC (PersistentVolumeClaim)
	// If the provisioner or an external controller can support the specified data source,
	// it will create a new volume based on the contents of the specified data source.
	// When the AnyVolumeDataSource feature gate is enabled, dataSource contents will be copied to dataSourceRef,
	// and dataSourceRef contents will be copied to dataSource when dataSourceRef.namespace is not specified.
	// If the namespace is specified, then dataSourceRef will not be copied to dataSource.
	// +optional
	DataSource *TypedLocalObjectReference
	// Specifies the object from which to populate the volume with data, if a non-empty
	// volume is desired. This may be any object from a non-empty API group (non
	// core object) or a PersistentVolumeClaim object.
	// When this field is specified, volume binding will only succeed if the type of
	// the specified object matches some installed volume populator or dynamic
	// provisioner.
	// This field will replace the functionality of the dataSource field and as such
	// if both fields are non-empty, they must have the same value. For backwards
	// compatibility, when namespace isn't specified in dataSourceRef,
	// both fields (dataSource and dataSourceRef) will be set to the same
	// value automatically if one of them is empty and the other is non-empty.
	// When namespace is specified in dataSourceRef,
	// dataSource isn't set to the same value and must be empty.
	// There are three important differences between dataSource and dataSourceRef:
	// * While dataSource only allows two specific types of objects, dataSourceRef
	//   allows any non-core object, as well as PersistentVolumeClaim objects.
	// * While dataSource ignores disallowed values (dropping them), dataSourceRef
	//   preserves all values, and generates an error if a disallowed value is
	//   specified.
	// * While dataSource only allows local objects, dataSourceRef allows objects
	//   in any namespaces.
	// (Beta) Using this field requires the AnyVolumeDataSource feature gate to be enabled.
	// (Alpha) Using the namespace field of dataSourceRef requires the CrossNamespaceVolumeDataSource feature gate to be enabled.
	// +optional
	DataSourceRef *TypedObjectReference
	// volumeAttributesClassName may be used to set the VolumeAttributesClass used by this claim.
	// If specified, the CSI driver will create or update the volume with the attributes defined
	// in the corresponding VolumeAttributesClass. This has a different purpose than storageClassName,
	// it can be changed after the claim is created. An empty string value means that no VolumeAttributesClass
	// will be applied to the claim but it's not allowed to reset this field to empty string once it is set.
	// If unspecified and the PersistentVolumeClaim is unbound, the default VolumeAttributesClass
	// will be set by the persistentvolume controller if it exists.
	// If the resource referred to by volumeAttributesClass does not exist, this PersistentVolumeClaim will be
	// set to a Pending state, as reflected by the modifyVolumeStatus field, until such as a resource
	// exists.
	// More info: https://kubernetes.io/docs/concepts/storage/volume-attributes-classes/
	// (Beta) Using this field requires the VolumeAttributesClass feature gate to be enabled (off by default).
	// +featureGate=VolumeAttributesClass
	// +optional
	VolumeAttributesClassName *string
}

type TypedObjectReference struct {
	// APIGroup is the group for the resource being referenced.
	// If APIGroup is not specified, the specified Kind must be in the core API group.
	// For any other third-party types, APIGroup is required.
	// +optional
	APIGroup *string
	// Kind is the type of resource being referenced
	Kind string
	// Name is the name of resource being referenced
	Name string
	// Namespace is the namespace of resource being referenced
	// Note that when a namespace is specified, a gateway.networking.k8s.io/ReferenceGrant object is required in the referent namespace to allow that namespace's owner to accept the reference. See the ReferenceGrant documentation for details.
	// (Alpha) This field requires the CrossNamespaceVolumeDataSource feature gate to be enabled.
	// +featureGate=CrossNamespaceVolumeDataSource
	// +optional
	Namespace *string
}

// PersistentVolumeClaimConditionType defines the condition of PV claim.
// Valid values are:
//   - "Resizing", "FileSystemResizePending"
//
// If RecoverVolumeExpansionFailure feature gate is enabled, then following additional values can be expected:
//   - "ControllerResizeError", "NodeResizeError"
//
// If VolumeAttributesClass feature gate is enabled, then following additional values can be expected:
//   - "ModifyVolumeError", "ModifyingVolume"
type PersistentVolumeClaimConditionType string

// These are valid conditions of PVC
const (
	// An user trigger resize of pvc has been started
	PersistentVolumeClaimResizing PersistentVolumeClaimConditionType = "Resizing"
	// PersistentVolumeClaimFileSystemResizePending - controller resize is finished and a file system resize is pending on node
	PersistentVolumeClaimFileSystemResizePending PersistentVolumeClaimConditionType = "FileSystemResizePending"

	// PersistentVolumeClaimControllerResizeError indicates an error while resizing volume for size in the controller
	PersistentVolumeClaimControllerResizeError PersistentVolumeClaimConditionType = "ControllerResizeError"
	// PersistentVolumeClaimNodeResizeError indicates an error while resizing volume for size in the node.
	PersistentVolumeClaimNodeResizeError PersistentVolumeClaimConditionType = "NodeResizeError"

	// Applying the target VolumeAttributesClass encountered an error
	PersistentVolumeClaimVolumeModifyVolumeError PersistentVolumeClaimConditionType = "ModifyVolumeError"
	// Volume is being modified
	PersistentVolumeClaimVolumeModifyingVolume PersistentVolumeClaimConditionType = "ModifyingVolume"
)

// +enum
// When a controller receives persistentvolume claim update with ClaimResourceStatus for a resource
// that it does not recognizes, then it should ignore that update and let other controllers
// handle it.
type ClaimResourceStatus string

const (
	// State set when resize controller starts resizing the volume in control-plane
	PersistentVolumeClaimControllerResizeInProgress ClaimResourceStatus = "ControllerResizeInProgress"

	// State set when resize has failed in resize controller with a terminal unrecoverable error.
	// Transient errors such as timeout should not set this status and should leave allocatedResourceStatus
	// unmodified, so as resize controller can resume the volume expansion.
	PersistentVolumeClaimControllerResizeInfeasible ClaimResourceStatus = "ControllerResizeInfeasible"

	// State set when resize controller has finished resizing the volume but further resizing of volume
	// is needed on the node.
	PersistentVolumeClaimNodeResizePending ClaimResourceStatus = "NodeResizePending"
	// State set when kubelet starts resizing the volume.
	PersistentVolumeClaimNodeResizeInProgress ClaimResourceStatus = "NodeResizeInProgress"
	// State set when resizing has failed in kubelet with a terminal unrecoverable error. Transient errors
	// shouldn't set this status
	PersistentVolumeClaimNodeResizeInfeasible ClaimResourceStatus = "NodeResizeInfeasible"
)

// +enum
// New statuses can be added in the future. Consumers should check for unknown statuses and fail appropriately
type PersistentVolumeClaimModifyVolumeStatus string

const (
	// Pending indicates that the PersistentVolumeClaim cannot be modified due to unmet requirements, such as
	// the specified VolumeAttributesClass not existing
	PersistentVolumeClaimModifyVolumePending PersistentVolumeClaimModifyVolumeStatus = "Pending"
	// InProgress indicates that the volume is being modified
	PersistentVolumeClaimModifyVolumeInProgress PersistentVolumeClaimModifyVolumeStatus = "InProgress"
	// Infeasible indicates that the request has been rejected as invalid by the CSI driver. To
	// resolve the error, a valid VolumeAttributesClass needs to be specified
	PersistentVolumeClaimModifyVolumeInfeasible PersistentVolumeClaimModifyVolumeStatus = "Infeasible"
)

// ModifyVolumeStatus represents the status object of ControllerModifyVolume operation
type ModifyVolumeStatus struct {
	// targetVolumeAttributesClassName is the name of the VolumeAttributesClass the PVC currently being reconciled
	TargetVolumeAttributesClassName string
	// status is the status of the ControllerModifyVolume operation. It can be in any of following states:
	//  - Pending
	//    Pending indicates that the PersistentVolumeClaim cannot be modified due to unmet requirements, such as
	//    the specified VolumeAttributesClass not existing.
	//  - InProgress
	//    InProgress indicates that the volume is being modified.
	//  - Infeasible
	//   Infeasible indicates that the request has been rejected as invalid by the CSI driver. To
	// 	  resolve the error, a valid VolumeAttributesClass needs to be specified.
	// Note: New statuses can be added in the future. Consumers should check for unknown statuses and fail appropriately.
	Status PersistentVolumeClaimModifyVolumeStatus
}

// PersistentVolumeClaimCondition represents the current condition of PV claim
type PersistentVolumeClaimCondition struct {
	Type   PersistentVolumeClaimConditionType
	Status ConditionStatus
	// +optional
	LastProbeTime metav1.Time
	// +optional
	LastTransitionTime metav1.Time
	// +optional
	Reason string
	// +optional
	Message string
}

// PersistentVolumeClaimStatus represents the status of PV claim
type PersistentVolumeClaimStatus struct {
	// Phase represents the current phase of PersistentVolumeClaim
	// +optional
	Phase PersistentVolumeClaimPhase
	// AccessModes contains all ways the volume backing the PVC can be mounted
	// +optional
	AccessModes []PersistentVolumeAccessMode
	// Represents the actual resources of the underlying volume
	// +optional
	Capacity ResourceList
	// +optional
	Conditions []PersistentVolumeClaimCondition
	// AllocatedResources tracks the resources allocated to a PVC including its capacity.
	// Key names follow standard Kubernetes label syntax. Valid values are either:
	// 	* Un-prefixed keys:
	//		- storage - the capacity of the volume.
	//	* Custom resources must use implementation-defined prefixed names such as "example.com/my-custom-resource"
	// Apart from above values - keys that are unprefixed or have kubernetes.io prefix are considered
	// reserved and hence may not be used.
	//
	// Capacity reported here may be larger than the actual capacity when a volume expansion operation
	// is requested.
	// For storage quota, the larger value from allocatedResources and PVC.spec.resources is used.
	// If allocatedResources is not set, PVC.spec.resources alone is used for quota calculation.
	// If a volume expansion capacity request is lowered, allocatedResources is only
	// lowered if there are no expansion operations in progress and if the actual volume capacity
	// is equal or lower than the requested capacity.
	//
	// A controller that receives PVC update with previously unknown resourceName
	// should ignore the update for the purpose it was designed. For example - a controller that
	// only is responsible for resizing capacity of the volume, should ignore PVC updates that change other valid
	// resources associated with PVC.
	//
	// This is an alpha field and requires enabling RecoverVolumeExpansionFailure feature.
	// +featureGate=RecoverVolumeExpansionFailure
	// +optional
	AllocatedResources ResourceList
	// AllocatedResourceStatuses stores status of resource being resized for the given PVC.
	// Key names follow standard Kubernetes label syntax. Valid values are either:
	// 	* Un-prefixed keys:
	//		- storage - the capacity of the volume.
	//	* Custom resources must use implementation-defined prefixed names such as "example.com/my-custom-resource"
	// Apart from above values - keys that are unprefixed or have kubernetes.io prefix are considered
	// reserved and hence may not be used.
	//
	// ClaimResourceStatus can be in any of following states:
	//	- ControllerResizeInProgress:
	//		State set when resize controller starts resizing the volume in control-plane.
	// 	- ControllerResizeFailed:
	//		State set when resize has failed in resize controller with a terminal error.
	//	- NodeResizePending:
	//		State set when resize controller has finished resizing the volume but further resizing of
	//		volume is needed on the node.
	//	- NodeResizeInProgress:
	//		State set when kubelet starts resizing the volume.
	//	- NodeResizeFailed:
	//		State set when resizing has failed in kubelet with a terminal error. Transient errors don't set
	//		NodeResizeFailed.
	// For example: if expanding a PVC for more capacity - this field can be one of the following states:
	// 	- pvc.status.allocatedResourceStatus['storage'] = "ControllerResizeInProgress"
	//      - pvc.status.allocatedResourceStatus['storage'] = "ControllerResizeFailed"
	//      - pvc.status.allocatedResourceStatus['storage'] = "NodeResizePending"
	//      - pvc.status.allocatedResourceStatus['storage'] = "NodeResizeInProgress"
	//      - pvc.status.allocatedResourceStatus['storage'] = "NodeResizeFailed"
	// When this field is not set, it means that no resize operation is in progress for the given PVC.
	//
	// A controller that receives PVC update with previously unknown resourceName or ClaimResourceStatus
	// should ignore the update for the purpose it was designed. For example - a controller that
	// only is responsible for resizing capacity of the volume, should ignore PVC updates that change other valid
	// resources associated with PVC.
	//
	// This is an alpha field and requires enabling RecoverVolumeExpansionFailure feature.
	// +featureGate=RecoverVolumeExpansionFailure
	// +mapType=granular
	// +optional
	AllocatedResourceStatuses map[ResourceName]ClaimResourceStatus
	// currentVolumeAttributesClassName is the current name of the VolumeAttributesClass the PVC is using.
	// When unset, there is no VolumeAttributeClass applied to this PersistentVolumeClaim
	// This is a beta field and requires enabling VolumeAttributesClass feature (off by default).
	// +featureGate=VolumeAttributesClass
	// +optional
	CurrentVolumeAttributesClassName *string
	// ModifyVolumeStatus represents the status object of ControllerModifyVolume operation.
	// When this is unset, there is no ModifyVolume operation being attempted.
	// This is a beta field and requires enabling VolumeAttributesClass feature (off by default).
	// +featureGate=VolumeAttributesClass
	// +optional
	ModifyVolumeStatus *ModifyVolumeStatus
}

// PersistentVolumeAccessMode defines various access modes for PV.
type PersistentVolumeAccessMode string

// These are the valid values for PersistentVolumeAccessMode
const (
	// can be mounted read/write mode to exactly 1 host
	ReadWriteOnce PersistentVolumeAccessMode = "ReadWriteOnce"
	// can be mounted in read-only mode to many hosts
	ReadOnlyMany PersistentVolumeAccessMode = "ReadOnlyMany"
	// can be mounted in read/write mode to many hosts
	ReadWriteMany PersistentVolumeAccessMode = "ReadWriteMany"
	// can be mounted read/write mode to exactly 1 pod
	// cannot be used in combination with other access modes
	ReadWriteOncePod PersistentVolumeAccessMode = "ReadWriteOncePod"
)

// PersistentVolumePhase defines the phase in which a PV is
type PersistentVolumePhase string

// These are the valid values for PersistentVolumePhase
const (
	// used for PersistentVolumes that are not available
	VolumePending PersistentVolumePhase = "Pending"
	// used for PersistentVolumes that are not yet bound
	// Available volumes are held by the binder and matched to PersistentVolumeClaims
	VolumeAvailable PersistentVolumePhase = "Available"
	// used for PersistentVolumes that are bound
	VolumeBound PersistentVolumePhase = "Bound"
	// used for PersistentVolumes where the bound PersistentVolumeClaim was deleted
	// released volumes must be recycled before becoming available again
	// this phase is used by the persistent volume claim binder to signal to another process to reclaim the resource
	VolumeReleased PersistentVolumePhase = "Released"
	// used for PersistentVolumes that failed to be correctly recycled or deleted after being released from a claim
	VolumeFailed PersistentVolumePhase = "Failed"
)

// PersistentVolumeClaimPhase defines the phase of PV claim
type PersistentVolumeClaimPhase string

// These are the valid value for PersistentVolumeClaimPhase
const (
	// used for PersistentVolumeClaims that are not yet bound
	ClaimPending PersistentVolumeClaimPhase = "Pending"
	// used for PersistentVolumeClaims that are bound
	ClaimBound PersistentVolumeClaimPhase = "Bound"
	// used for PersistentVolumeClaims that lost their underlying
	// PersistentVolume. The claim was bound to a PersistentVolume and this
	// volume does not exist any longer and all data on it was lost.
	ClaimLost PersistentVolumeClaimPhase = "Lost"
)

// HostPathType defines the type of host path for PV
type HostPathType string

// These are the valid values for HostPathType
const (
	// For backwards compatible, leave it empty if unset
	HostPathUnset HostPathType = ""
	// If nothing exists at the given path, an empty directory will be created there
	// as needed with file mode 0755, having the same group and ownership with Kubelet.
	HostPathDirectoryOrCreate HostPathType = "DirectoryOrCreate"
	// A directory must exist at the given path
	HostPathDirectory HostPathType = "Directory"
	// If nothing exists at the given path, an empty file will be created there
	// as needed with file mode 0644, having the same group and ownership with Kubelet.
	HostPathFileOrCreate HostPathType = "FileOrCreate"
	// A file must exist at the given path
	HostPathFile HostPathType = "File"
	// A UNIX socket must exist at the given path
	HostPathSocket HostPathType = "Socket"
	// A character device must exist at the given path
	HostPathCharDev HostPathType = "CharDevice"
	// A block device must exist at the given path
	HostPathBlockDev HostPathType = "BlockDevice"
)

// HostPathVolumeSource represents a host path mapped into a pod.
// Host path volumes do not support ownership management or SELinux relabeling.
type HostPathVolumeSource struct {
	// If the path is a symlink, it will follow the link to the real path.
	Path string
	// Defaults to ""
	Type *HostPathType
}

// EmptyDirVolumeSource represents an empty directory for a pod.
// Empty directory volumes support ownership management and SELinux relabeling.
type EmptyDirVolumeSource struct {
	// TODO: Longer term we want to represent the selection of underlying
	// media more like a scheduling problem - user says what traits they
	// need, we give them a backing store that satisfies that.  For now
	// this will cover the most common needs.
	// Optional: what type of storage medium should back this directory.
	// The default is "" which means to use the node's default medium.
	// +optional
	Medium StorageMedium
	// Total amount of local storage required for this EmptyDir volume.
	// The size limit is also applicable for memory medium.
	// The maximum usage on memory medium EmptyDir would be the minimum value between
	// the SizeLimit specified here and the sum of memory limits of all containers in a pod.
	// The default is nil which means that the limit is undefined.
	// More info: https://kubernetes.io/docs/concepts/storage/volumes#emptydir
	// +optional
	SizeLimit *resource.Quantity
}

// StorageMedium defines ways that storage can be allocated to a volume.
type StorageMedium string

// These are the valid value for StorageMedium
const (
	StorageMediumDefault         StorageMedium = ""           // use whatever the default is for the node
	StorageMediumMemory          StorageMedium = "Memory"     // use memory (tmpfs)
	StorageMediumHugePages       StorageMedium = "HugePages"  // use hugepages
	StorageMediumHugePagesPrefix StorageMedium = "HugePages-" // prefix for full medium notation HugePages-<size>
)

// Protocol defines network protocols supported for things like container ports.
type Protocol string

const (
	// ProtocolTCP is the TCP protocol.
	ProtocolTCP Protocol = "TCP"
	// ProtocolUDP is the UDP protocol.
	ProtocolUDP Protocol = "UDP"
	// ProtocolSCTP is the SCTP protocol.
	ProtocolSCTP Protocol = "SCTP"
)

// GCEPersistentDiskVolumeSource represents a Persistent Disk resource in Google Compute Engine.
//
// A GCE PD must exist before mounting to a container. The disk must
// also be in the same GCE project and zone as the kubelet. A GCE PD
// can only be mounted as read/write once or read-only many times. GCE
// PDs support ownership management and SELinux relabeling.
type GCEPersistentDiskVolumeSource struct {
	// Unique name of the PD resource. Used to identify the disk in GCE
	PDName string
	// Filesystem type to mount.
	// Must be a filesystem type supported by the host operating system.
	// Ex. "ext4", "xfs", "ntfs". Implicitly inferred to be "ext4" if unspecified.
	// TODO: how do we prevent errors in the filesystem from compromising the machine
	// +optional
	FSType string
	// Optional: Partition on the disk to mount.
	// If omitted, kubelet will attempt to mount the device name.
	// Ex. For /dev/sda1, this field is "1", for /dev/sda, this field is 0 or empty.
	// +optional
	Partition int32
	// Optional: Defaults to false (read/write). ReadOnly here will force
	// the ReadOnly setting in VolumeMounts.
	// +optional
	ReadOnly bool
}

// ISCSIVolumeSource represents an ISCSI disk.
// ISCSI volumes can only be mounted as read/write once.
// ISCSI volumes support ownership management and SELinux relabeling.
type ISCSIVolumeSource struct {
	// Required: iSCSI target portal
	// the portal is either an IP or ip_addr:port if port is other than default (typically TCP ports 860 and 3260)
	// +optional
	TargetPortal string
	// Required:  target iSCSI Qualified Name
	// +optional
	IQN string
	// Required: iSCSI target lun number
	// +optional
	Lun int32
	// Optional: Defaults to 'default' (tcp). iSCSI interface name that uses an iSCSI transport.
	// +optional
	ISCSIInterface string
	// Filesystem type to mount.
	// Must be a filesystem type supported by the host operating system.
	// Ex. "ext4", "xfs", "ntfs". Implicitly inferred to be "ext4" if unspecified.
	// TODO: how do we prevent errors in the filesystem from compromising the machine
	// +optional
	FSType string
	// Optional: Defaults to false (read/write). ReadOnly here will force
	// the ReadOnly setting in VolumeMounts.
	// +optional
	ReadOnly bool
	// Optional: list of iSCSI target portal ips for high availability.
	// the portal is either an IP or ip_addr:port if port is other than default (typically TCP ports 860 and 3260)
	// +optional
	Portals []string
	// Optional: whether support iSCSI Discovery CHAP authentication
	// +optional
	DiscoveryCHAPAuth bool
	// Optional: whether support iSCSI Session CHAP authentication
	// +optional
	SessionCHAPAuth bool
	// Optional: CHAP secret for iSCSI target and initiator authentication.
	// The secret is used if either DiscoveryCHAPAuth or SessionCHAPAuth is true
	// +optional
	SecretRef *LocalObjectReference
	// Optional: Custom initiator name per volume.
	// If initiatorName is specified with iscsiInterface simultaneously, new iSCSI interface
	// <target portal>:<volume name> will be created for the connection.
	// +optional
	InitiatorName *string
}

// ISCSIPersistentVolumeSource represents an ISCSI disk.
// ISCSI volumes can only be mounted as read/write once.
// ISCSI volumes support ownership management and SELinux relabeling.
type ISCSIPersistentVolumeSource struct {
	// Required: iSCSI target portal
	// the portal is either an IP or ip_addr:port if port is other than default (typically TCP ports 860 and 3260)
	// +optional
	TargetPortal string
	// Required:  target iSCSI Qualified Name
	// +optional
	IQN string
	// Required: iSCSI target lun number
	// +optional
	Lun int32
	// Optional: Defaults to 'default' (tcp). iSCSI interface name that uses an iSCSI transport.
	// +optional
	ISCSIInterface string
	// Filesystem type to mount.
	// Must be a filesystem type supported by the host operating system.
	// Ex. "ext4", "xfs", "ntfs". Implicitly inferred to be "ext4" if unspecified.
	// TODO: how do we prevent errors in the filesystem from compromising the machine
	// +optional
	FSType string
	// Optional: Defaults to false (read/write). ReadOnly here will force
	// the ReadOnly setting in VolumeMounts.
	// +optional
	ReadOnly bool
	// Optional: list of iSCSI target portal ips for high availability.
	// the portal is either an IP or ip_addr:port if port is other than default (typically TCP ports 860 and 3260)
	// +optional
	Portals []string
	// Optional: whether support iSCSI Discovery CHAP authentication
	// +optional
	DiscoveryCHAPAuth bool
	// Optional: whether support iSCSI Session CHAP authentication
	// +optional
	SessionCHAPAuth bool
	// Optional: CHAP secret for iSCSI target and initiator authentication.
	// The secret is used if either DiscoveryCHAPAuth or SessionCHAPAuth is true
	// +optional
	SecretRef *SecretReference
	// Optional: Custom initiator name per volume.
	// If initiatorName is specified with iscsiInterface simultaneously, new iSCSI interface
	// <target portal>:<volume name> will be created for the connection.
	// +optional
	InitiatorName *string
}

// FCVolumeSource represents a Fibre Channel volume.
// Fibre Channel volumes can only be mounted as read/write once.
// Fibre Channel volumes support ownership management and SELinux relabeling.
type FCVolumeSource struct {
	// Optional: FC target worldwide names (WWNs)
	// +optional
	TargetWWNs []string
	// Optional: FC target lun number
	// +optional
	Lun *int32
	// Filesystem type to mount.
	// Must be a filesystem type supported by the host operating system.
	// Ex. "ext4", "xfs", "ntfs". Implicitly inferred to be "ext4" if unspecified.
	// TODO: how do we prevent errors in the filesystem from compromising the machine
	// +optional
	FSType string
	// Optional: Defaults to false (read/write). ReadOnly here will force
	// the ReadOnly setting in VolumeMounts.
	// +optional
	ReadOnly bool
	// Optional: FC volume World Wide Identifiers (WWIDs)
	// Either WWIDs or TargetWWNs and Lun must be set, but not both simultaneously.
	// +optional
	WWIDs []string
}

// FlexPersistentVolumeSource represents a generic persistent volume resource that is
// provisioned/attached using an exec based plugin.
type FlexPersistentVolumeSource struct {
	// Driver is the name of the driver to use for this volume.
	Driver string
	// Filesystem type to mount.
	// Must be a filesystem type supported by the host operating system.
	// Ex. "ext4", "xfs", "ntfs". The default filesystem depends on FlexVolume script.
	// +optional
	FSType string
	// Optional: SecretRef is reference to the secret object containing
	// sensitive information to pass to the plugin scripts. This may be
	// empty if no secret object is specified. If the secret object
	// contains more than one secret, all secrets are passed to the plugin
	// scripts.
	// +optional
	SecretRef *SecretReference
	// Optional: Defaults to false (read/write). ReadOnly here will force
	// the ReadOnly setting in VolumeMounts.
	// +optional
	ReadOnly bool
	// Optional: Extra driver options if any.
	// +optional
	Options map[string]string
}

// FlexVolumeSource represents a generic volume resource that is
// provisioned/attached using an exec based plugin.
type FlexVolumeSource struct {
	// Driver is the name of the driver to use for this volume.
	Driver string
	// Filesystem type to mount.
	// Must be a filesystem type supported by the host operating system.
	// Ex. "ext4", "xfs", "ntfs". The default filesystem depends on FlexVolume script.
	// +optional
	FSType string
	// Optional: SecretRef is reference to the secret object containing
	// sensitive information to pass to the plugin scripts. This may be
	// empty if no secret object is specified. If the secret object
	// contains more than one secret, all secrets are passed to the plugin
	// scripts.
	// +optional
	SecretRef *LocalObjectReference
	// Optional: Defaults to false (read/write). ReadOnly here will force
	// the ReadOnly setting in VolumeMounts.
	// +optional
	ReadOnly bool
	// Optional: Extra driver options if any.
	// +optional
	Options map[string]string
}

// AWSElasticBlockStoreVolumeSource represents a Persistent Disk resource in AWS.
//
// An AWS EBS disk must exist before mounting to a container. The disk
// must also be in the same AWS zone as the kubelet. An AWS EBS disk
// can only be mounted as read/write once. AWS EBS volumes support
// ownership management and SELinux relabeling.
type AWSElasticBlockStoreVolumeSource struct {
	// Unique id of the persistent disk resource. Used to identify the disk in AWS
	VolumeID string
	// Filesystem type to mount.
	// Must be a filesystem type supported by the host operating system.
	// Ex. "ext4", "xfs", "ntfs". Implicitly inferred to be "ext4" if unspecified.
	// TODO: how do we prevent errors in the filesystem from compromising the machine
	// +optional
	FSType string
	// Optional: Partition on the disk to mount.
	// If omitted, kubelet will attempt to mount the device name.
	// Ex. For /dev/sda1, this field is "1", for /dev/sda, this field is 0 or empty.
	// +optional
	Partition int32
	// Optional: Defaults to false (read/write). ReadOnly here will force
	// the ReadOnly setting in VolumeMounts.
	// +optional
	ReadOnly bool
}

// GitRepoVolumeSource represents a volume that is populated with the contents of a git repository.
// Git repo volumes do not support ownership management.
// Git repo volumes support SELinux relabeling.
//
// DEPRECATED: GitRepo is deprecated. To provision a container with a git repo, mount an
// EmptyDir into an InitContainer that clones the repo using git, then mount the EmptyDir
// into the Pod's container.
type GitRepoVolumeSource struct {
	// Repository URL
	Repository string
	// Commit hash, this is optional
	// +optional
	Revision string
	// Clone target, this is optional
	// Must not contain or start with '..'.  If '.' is supplied, the volume directory will be the
	// git repository.  Otherwise, if specified, the volume will contain the git repository in
	// the subdirectory with the given name.
	// +optional
	Directory string
	// TODO: Consider credentials here.
}

// SecretVolumeSource adapts a Secret into a volume.
//
// The contents of the target Secret's Data field will be presented in a volume
// as files using the keys in the Data field as the file names.
// Secret volumes support ownership management and SELinux relabeling.
type SecretVolumeSource struct {
	// Name of the secret in the pod's namespace to use.
	// +optional
	SecretName string
	// If unspecified, each key-value pair in the Data field of the referenced
	// Secret will be projected into the volume as a file whose name is the
	// key and content is the value. If specified, the listed keys will be
	// projected into the specified paths, and unlisted keys will not be
	// present. If a key is specified which is not present in the Secret,
	// the volume setup will error unless it is marked optional. Paths must be
	// relative and may not contain the '..' path or start with '..'.
	// +optional
	Items []KeyToPath
	// Mode bits to use on created files by default. Must be a value between
	// 0 and 0777.
	// Directories within the path are not affected by this setting.
	// This might be in conflict with other options that affect the file
	// mode, like fsGroup, and the result can be other mode bits set.
	// +optional
	DefaultMode *int32
	// Specify whether the Secret or its key must be defined
	// +optional
	Optional *bool
}

// SecretProjection adapts a secret into a projected volume.
//
// The contents of the target Secret's Data field will be presented in a
// projected volume as files using the keys in the Data field as the file names.
// Note that this is identical to a secret volume source without the default
// mode.
type SecretProjection struct {
	LocalObjectReference
	// If unspecified, each key-value pair in the Data field of the referenced
	// Secret will be projected into the volume as a file whose name is the
	// key and content is the value. If specified, the listed keys will be
	// projected into the specified paths, and unlisted keys will not be
	// present. If a key is specified which is not present in the Secret,
	// the volume setup will error unless it is marked optional. Paths must be
	// relative and may not contain the '..' path or start with '..'.
	// +optional
	Items []KeyToPath
	// Specify whether the Secret or its key must be defined
	// +optional
	Optional *bool
}

// NFSVolumeSource represents an NFS mount that lasts the lifetime of a pod.
// NFS volumes do not support ownership management or SELinux relabeling.
type NFSVolumeSource struct {
	// Server is the hostname or IP address of the NFS server
	Server string

	// Path is the exported NFS share
	Path string

	// Optional: Defaults to false (read/write). ReadOnly here will force
	// the NFS export to be mounted with read-only permissions
	// +optional
	ReadOnly bool
}

// QuobyteVolumeSource represents a Quobyte mount that lasts the lifetime of a pod.
// Quobyte volumes do not support ownership management or SELinux relabeling.
type QuobyteVolumeSource struct {
	// Registry represents a single or multiple Quobyte Registry services
	// specified as a string as host:port pair (multiple entries are separated with commas)
	// which acts as the central registry for volumes
	Registry string

	// Volume is a string that references an already created Quobyte volume by name.
	Volume string

	// Defaults to false (read/write). ReadOnly here will force
	// the Quobyte to be mounted with read-only permissions
	// +optional
	ReadOnly bool

	// User to map volume access to
	// Defaults to the root user
	// +optional
	User string

	// Group to map volume access to
	// Default is no group
	// +optional
	Group string

	// Tenant owning the given Quobyte volume in the Backend
	// Used with dynamically provisioned Quobyte volumes, value is set by the plugin
	// +optional
	Tenant string
}

// GlusterfsVolumeSource represents a Glusterfs mount that lasts the lifetime of a pod.
// Glusterfs volumes do not support ownership management or SELinux relabeling.
type GlusterfsVolumeSource struct {
	// Required: EndpointsName is the endpoint name that details Glusterfs topology
	EndpointsName string

	// Required: Path is the Glusterfs volume path
	Path string

	// Optional: Defaults to false (read/write). ReadOnly here will force
	// the Glusterfs to be mounted with read-only permissions
	// +optional
	ReadOnly bool
}

// GlusterfsPersistentVolumeSource represents a Glusterfs mount that lasts the lifetime of a pod.
// Glusterfs volumes do not support ownership management or SELinux relabeling.
type GlusterfsPersistentVolumeSource struct {
	// EndpointsName is the endpoint name that details Glusterfs topology.
	// More info: https://examples.k8s.io/volumes/glusterfs/README.md#create-a-pod
	EndpointsName string

	// Path is the Glusterfs volume path.
	// More info: https://examples.k8s.io/volumes/glusterfs/README.md#create-a-pod
	Path string

	// ReadOnly here will force the Glusterfs volume to be mounted with read-only permissions.
	// Defaults to false.
	// More info: https://examples.k8s.io/volumes/glusterfs/README.md#create-a-pod
	// +optional
	ReadOnly bool

	// EndpointsNamespace is the namespace that contains Glusterfs endpoint.
	// If this field is empty, the EndpointNamespace defaults to the same namespace as the bound PVC.
	// More info: https://examples.k8s.io/volumes/glusterfs/README.md#create-a-pod
	// +optional
	EndpointsNamespace *string
}

// RBDVolumeSource represents a Rados Block Device mount that lasts the lifetime of a pod.
// RBD volumes support ownership management and SELinux relabeling.
type RBDVolumeSource struct {
	// Required: CephMonitors is a collection of Ceph monitors
	CephMonitors []string
	// Required: RBDImage is the rados image name
	RBDImage string
	// Filesystem type to mount.
	// Must be a filesystem type supported by the host operating system.
	// Ex. "ext4", "xfs", "ntfs". Implicitly inferred to be "ext4" if unspecified.
	// TODO: how do we prevent errors in the filesystem from compromising the machine
	// +optional
	FSType string
	// Optional: RadosPool is the rados pool name,default is rbd
	// +optional
	RBDPool string
	// Optional: RBDUser is the rados user name, default is admin
	// +optional
	RadosUser string
	// Optional: Keyring is the path to key ring for RBDUser, default is /etc/ceph/keyring
	// +optional
	Keyring string
	// Optional: SecretRef is name of the authentication secret for RBDUser, default is nil.
	// +optional
	SecretRef *LocalObjectReference
	// Optional: Defaults to false (read/write). ReadOnly here will force
	// the ReadOnly setting in VolumeMounts.
	// +optional
	ReadOnly bool
}

// RBDPersistentVolumeSource represents a Rados Block Device mount that lasts the lifetime of a pod.
// RBD volumes support ownership management and SELinux relabeling.
type RBDPersistentVolumeSource struct {
	// Required: CephMonitors is a collection of Ceph monitors
	CephMonitors []string
	// Required: RBDImage is the rados image name
	RBDImage string
	// Filesystem type to mount.
	// Must be a filesystem type supported by the host operating system.
	// Ex. "ext4", "xfs", "ntfs". Implicitly inferred to be "ext4" if unspecified.
	// TODO: how do we prevent errors in the filesystem from compromising the machine
	// +optional
	FSType string
	// Optional: RadosPool is the rados pool name,default is rbd
	// +optional
	RBDPool string
	// Optional: RBDUser is the rados user name, default is admin
	// +optional
	RadosUser string
	// Optional: Keyring is the path to key ring for RBDUser, default is /etc/ceph/keyring
	// +optional
	Keyring string
	// Optional: SecretRef is reference to the authentication secret for User, default is empty.
	// +optional
	SecretRef *SecretReference
	// Optional: Defaults to false (read/write). ReadOnly here will force
	// the ReadOnly setting in VolumeMounts.
	// +optional
	ReadOnly bool
}

// CinderVolumeSource represents a cinder volume resource in Openstack. A Cinder volume
// must exist before mounting to a container. The volume must also be
// in the same region as the kubelet. Cinder volumes support ownership
// management and SELinux relabeling.
type CinderVolumeSource struct {
	// Unique id of the volume used to identify the cinder volume.
	VolumeID string
	// Filesystem type to mount.
	// Must be a filesystem type supported by the host operating system.
	// Ex. "ext4", "xfs", "ntfs". Implicitly inferred to be "ext4" if unspecified.
	// +optional
	FSType string
	// Optional: Defaults to false (read/write). ReadOnly here will force
	// the ReadOnly setting in VolumeMounts.
	// +optional
	ReadOnly bool
	// Optional: points to a secret object containing parameters used to connect
	// to OpenStack.
	// +optional
	SecretRef *LocalObjectReference
}

// CinderPersistentVolumeSource represents a cinder volume resource in Openstack. A Cinder volume
// must exist before mounting to a container. The volume must also be
// in the same region as the kubelet. Cinder volumes support ownership
// management and SELinux relabeling.
type CinderPersistentVolumeSource struct {
	// Unique id of the volume used to identify the cinder volume.
	VolumeID string
	// Filesystem type to mount.
	// Must be a filesystem type supported by the host operating system.
	// Ex. "ext4", "xfs", "ntfs". Implicitly inferred to be "ext4" if unspecified.
	// +optional
	FSType string
	// Optional: Defaults to false (read/write). ReadOnly here will force
	// the ReadOnly setting in VolumeMounts.
	// +optional
	ReadOnly bool
	// Optional: points to a secret object containing parameters used to connect
	// to OpenStack.
	// +optional
	SecretRef *SecretReference
}

// CephFSVolumeSource represents a Ceph Filesystem mount that lasts the lifetime of a pod
// Cephfs volumes do not support ownership management or SELinux relabeling.
type CephFSVolumeSource struct {
	// Required: Monitors is a collection of Ceph monitors
	Monitors []string
	// Optional: Used as the mounted root, rather than the full Ceph tree, default is /
	// +optional
	Path string
	// Optional: User is the rados user name, default is admin
	// +optional
	User string
	// Optional: SecretFile is the path to key ring for User, default is /etc/ceph/user.secret
	// +optional
	SecretFile string
	// Optional: SecretRef is reference to the authentication secret for User, default is empty.
	// +optional
	SecretRef *LocalObjectReference
	// Optional: Defaults to false (read/write). ReadOnly here will force
	// the ReadOnly setting in VolumeMounts.
	// +optional
	ReadOnly bool
}

// SecretReference represents a Secret Reference. It has enough information to retrieve secret
// in any namespace
type SecretReference struct {
	// Name is unique within a namespace to reference a secret resource.
	// +optional
	Name string
	// Namespace defines the space within which the secret name must be unique.
	// +optional
	Namespace string
}

// CephFSPersistentVolumeSource represents a Ceph Filesystem mount that lasts the lifetime of a pod
// Cephfs volumes do not support ownership management or SELinux relabeling.
type CephFSPersistentVolumeSource struct {
	// Required: Monitors is a collection of Ceph monitors
	Monitors []string
	// Optional: Used as the mounted root, rather than the full Ceph tree, default is /
	// +optional
	Path string
	// Optional: User is the rados user name, default is admin
	// +optional
	User string
	// Optional: SecretFile is the path to key ring for User, default is /etc/ceph/user.secret
	// +optional
	SecretFile string
	// Optional: SecretRef is reference to the authentication secret for User, default is empty.
	// +optional
	SecretRef *SecretReference
	// Optional: Defaults to false (read/write). ReadOnly here will force
	// the ReadOnly setting in VolumeMounts.
	// +optional
	ReadOnly bool
}

// FlockerVolumeSource represents a Flocker volume mounted by the Flocker agent.
// One and only one of datasetName and datasetUUID should be set.
// Flocker volumes do not support ownership management or SELinux relabeling.
type FlockerVolumeSource struct {
	// Name of the dataset stored as metadata -> name on the dataset for Flocker
	// should be considered as deprecated
	// +optional
	DatasetName string
	// UUID of the dataset. This is unique identifier of a Flocker dataset
	// +optional
	DatasetUUID string
}

// DownwardAPIVolumeSource represents a volume containing downward API info.
// Downward API volumes support ownership management and SELinux relabeling.
type DownwardAPIVolumeSource struct {
	// Items is a list of DownwardAPIVolume file
	// +optional
	Items []DownwardAPIVolumeFile
	// Mode bits to use on created files by default. Must be a value between
	// 0 and 0777.
	// Directories within the path are not affected by this setting.
	// This might be in conflict with other options that affect the file
	// mode, like fsGroup, and the result can be other mode bits set.
	// +optional
	DefaultMode *int32
}

// DownwardAPIVolumeFile represents a single file containing information from the downward API
type DownwardAPIVolumeFile struct {
	// Required: Path is  the relative path name of the file to be created. Must not be absolute or contain the '..' path. Must be utf-8 encoded. The first item of the relative path must not start with '..'
	Path string
	// Required: Selects a field of the pod: only annotations, labels, name, namespace and uid are supported.
	// +optional
	FieldRef *ObjectFieldSelector
	// Selects a resource of the container: only resources limits and requests
	// (limits.cpu, limits.memory, requests.cpu and requests.memory) are currently supported.
	// +optional
	ResourceFieldRef *ResourceFieldSelector
	// Optional: mode bits to use on this file, must be a value between 0
	// and 0777. If not specified, the volume defaultMode will be used.
	// This might be in conflict with other options that affect the file
	// mode, like fsGroup, and the result can be other mode bits set.
	// +optional
	Mode *int32
}

// DownwardAPIProjection represents downward API info for projecting into a projected volume.
// Note that this is identical to a downwardAPI volume source without the default
// mode.
type DownwardAPIProjection struct {
	// Items is a list of DownwardAPIVolume file
	// +optional
	Items []DownwardAPIVolumeFile
}

// AzureFileVolumeSource azureFile represents an Azure File Service mount on the host and bind mount to the pod.
type AzureFileVolumeSource struct {
	// the name of secret that contains Azure Storage Account Name and Key
	SecretName string
	// Share Name
	ShareName string
	// Defaults to false (read/write). ReadOnly here will force
	// the ReadOnly setting in VolumeMounts.
	// +optional
	ReadOnly bool
}

// AzureFilePersistentVolumeSource represents an Azure File Service mount on the host and bind mount to the pod.
type AzureFilePersistentVolumeSource struct {
	// the name of secret that contains Azure Storage Account Name and Key
	SecretName string
	// Share Name
	ShareName string
	// Defaults to false (read/write). ReadOnly here will force
	// the ReadOnly setting in VolumeMounts.
	// +optional
	ReadOnly bool
	// the namespace of the secret that contains Azure Storage Account Name and Key
	// default is the same as the Pod
	// +optional
	SecretNamespace *string
}

// VsphereVirtualDiskVolumeSource represents a vSphere volume resource.
type VsphereVirtualDiskVolumeSource struct {
	// Path that identifies vSphere volume vmdk
	VolumePath string
	// Filesystem type to mount.
	// Must be a filesystem type supported by the host operating system.
	// Ex. "ext4", "xfs", "ntfs". Implicitly inferred to be "ext4" if unspecified.
	// +optional
	FSType string
	// Storage Policy Based Management (SPBM) profile name.
	// +optional
	StoragePolicyName string
	// Storage Policy Based Management (SPBM) profile ID associated with the StoragePolicyName.
	// +optional
	StoragePolicyID string
}

// PhotonPersistentDiskVolumeSource represents a Photon Controller persistent disk resource.
type PhotonPersistentDiskVolumeSource struct {
	// ID that identifies Photon Controller persistent disk
	PdID string
	// Filesystem type to mount.
	// Must be a filesystem type supported by the host operating system.
	// Ex. "ext4", "xfs", "ntfs". Implicitly inferred to be "ext4" if unspecified.
	FSType string
}

// PortworxVolumeSource represents a Portworx volume resource.
type PortworxVolumeSource struct {
	// VolumeID uniquely identifies a Portworx volume
	VolumeID string
	// FSType represents the filesystem type to mount
	// Must be a filesystem type supported by the host operating system.
	// Ex. "ext4", "xfs". Implicitly inferred to be "ext4" if unspecified.
	// +optional
	FSType string
	// Defaults to false (read/write). ReadOnly here will force
	// the ReadOnly setting in VolumeMounts.
	// +optional
	ReadOnly bool
}

// AzureDataDiskCachingMode defines the caching mode for Azure data disk
type AzureDataDiskCachingMode string

// AzureDataDiskKind defines the kind of Azure data disk
type AzureDataDiskKind string

// Defines cache mode and kinds for Azure data disk
const (
	AzureDataDiskCachingNone      AzureDataDiskCachingMode = "None"
	AzureDataDiskCachingReadOnly  AzureDataDiskCachingMode = "ReadOnly"
	AzureDataDiskCachingReadWrite AzureDataDiskCachingMode = "ReadWrite"

	AzureSharedBlobDisk    AzureDataDiskKind = "Shared"
	AzureDedicatedBlobDisk AzureDataDiskKind = "Dedicated"
	AzureManagedDisk       AzureDataDiskKind = "Managed"
)

// AzureDiskVolumeSource represents an Azure Data Disk mount on the host and bind mount to the pod.
type AzureDiskVolumeSource struct {
	// The Name of the data disk in the blob storage
	DiskName string
	// The URI of the data disk in the blob storage
	DataDiskURI string
	// Host Caching mode: None, Read Only, Read Write.
	// +optional
	CachingMode *AzureDataDiskCachingMode
	// Filesystem type to mount.
	// Must be a filesystem type supported by the host operating system.
	// Ex. "ext4", "xfs", "ntfs". Implicitly inferred to be "ext4" if unspecified.
	// +optional
	FSType *string
	// Defaults to false (read/write). ReadOnly here will force
	// the ReadOnly setting in VolumeMounts.
	// +optional
	ReadOnly *bool
	// Expected values Shared: multiple blob disks per storage account  Dedicated: single blob disk per storage account  Managed: azure managed data disk (only in managed availability set). defaults to shared
	Kind *AzureDataDiskKind
}

// ScaleIOVolumeSource represents a persistent ScaleIO volume
type ScaleIOVolumeSource struct {
	// The host address of the ScaleIO API Gateway.
	Gateway string
	// The name of the storage system as configured in ScaleIO.
	System string
	// SecretRef references to the secret for ScaleIO user and other
	// sensitive information. If this is not provided, Login operation will fail.
	SecretRef *LocalObjectReference
	// Flag to enable/disable SSL communication with Gateway, default false
	// +optional
	SSLEnabled bool
	// The name of the ScaleIO Protection Domain for the configured storage.
	// +optional
	ProtectionDomain string
	// The ScaleIO Storage Pool associated with the protection domain.
	// +optional
	StoragePool string
	// Indicates whether the storage for a volume should be ThickProvisioned or ThinProvisioned.
	// Default is ThinProvisioned.
	// +optional
	StorageMode string
	// The name of a volume already created in the ScaleIO system
	// that is associated with this volume source.
	VolumeName string
	// Filesystem type to mount.
	// Must be a filesystem type supported by the host operating system.
	// Ex. "ext4", "xfs", "ntfs".
	// Default is "xfs".
	// +optional
	FSType string
	// Defaults to false (read/write). ReadOnly here will force
	// the ReadOnly setting in VolumeMounts.
	// +optional
	ReadOnly bool
}

// ScaleIOPersistentVolumeSource represents a persistent ScaleIO volume that can be defined
// by a an admin via a storage class, for instance.
type ScaleIOPersistentVolumeSource struct {
	// The host address of the ScaleIO API Gateway.
	Gateway string
	// The name of the storage system as configured in ScaleIO.
	System string
	// SecretRef references to the secret for ScaleIO user and other
	// sensitive information. If this is not provided, Login operation will fail.
	SecretRef *SecretReference
	// Flag to enable/disable SSL communication with Gateway, default false
	// +optional
	SSLEnabled bool
	// The name of the ScaleIO Protection Domain for the configured storage.
	// +optional
	ProtectionDomain string
	// The ScaleIO Storage Pool associated with the protection domain.
	// +optional
	StoragePool string
	// Indicates whether the storage for a volume should be ThickProvisioned or ThinProvisioned.
	// Default is ThinProvisioned.
	// +optional
	StorageMode string
	// The name of a volume created in the ScaleIO system
	// that is associated with this volume source.
	VolumeName string
	// Filesystem type to mount.
	// Must be a filesystem type supported by the host operating system.
	// Ex. "ext4", "xfs", "ntfs".
	// Default is "xfs".
	// +optional
	FSType string
	// Defaults to false (read/write). ReadOnly here will force
	// the ReadOnly setting in VolumeMounts.
	// +optional
	ReadOnly bool
}

// StorageOSVolumeSource represents a StorageOS persistent volume resource.
type StorageOSVolumeSource struct {
	// VolumeName is the human-readable name of the StorageOS volume.  Volume
	// names are only unique within a namespace.
	VolumeName string
	// VolumeNamespace specifies the scope of the volume within StorageOS.  If no
	// namespace is specified then the Pod's namespace will be used.  This allows the
	// Kubernetes name scoping to be mirrored within StorageOS for tighter integration.
	// Set VolumeName to any name to override the default behaviour.
	// Set to "default" if you are not using namespaces within StorageOS.
	// Namespaces that do not pre-exist within StorageOS will be created.
	// +optional
	VolumeNamespace string
	// Filesystem type to mount.
	// Must be a filesystem type supported by the host operating system.
	// Ex. "ext4", "xfs", "ntfs". Implicitly inferred to be "ext4" if unspecified.
	// +optional
	FSType string
	// Defaults to false (read/write). ReadOnly here will force
	// the ReadOnly setting in VolumeMounts.
	// +optional
	ReadOnly bool
	// SecretRef specifies the secret to use for obtaining the StorageOS API
	// credentials.  If not specified, default values will be attempted.
	// +optional
	SecretRef *LocalObjectReference
}

// StorageOSPersistentVolumeSource represents a StorageOS persistent volume resource.
type StorageOSPersistentVolumeSource struct {
	// VolumeName is the human-readable name of the StorageOS volume.  Volume
	// names are only unique within a namespace.
	VolumeName string
	// VolumeNamespace specifies the scope of the volume within StorageOS.  If no
	// namespace is specified then the Pod's namespace will be used.  This allows the
	// Kubernetes name scoping to be mirrored within StorageOS for tighter integration.
	// Set VolumeName to any name to override the default behaviour.
	// Set to "default" if you are not using namespaces within StorageOS.
	// Namespaces that do not pre-exist within StorageOS will be created.
	// +optional
	VolumeNamespace string
	// Filesystem type to mount.
	// Must be a filesystem type supported by the host operating system.
	// Ex. "ext4", "xfs", "ntfs". Implicitly inferred to be "ext4" if unspecified.
	// +optional
	FSType string
	// Defaults to false (read/write). ReadOnly here will force
	// the ReadOnly setting in VolumeMounts.
	// +optional
	ReadOnly bool
	// SecretRef specifies the secret to use for obtaining the StorageOS API
	// credentials.  If not specified, default values will be attempted.
	// +optional
	SecretRef *ObjectReference
}

// ConfigMapVolumeSource adapts a ConfigMap into a volume.
//
// The contents of the target ConfigMap's Data field will be presented in a
// volume as files using the keys in the Data field as the file names, unless
// the items element is populated with specific mappings of keys to paths.
// ConfigMap volumes support ownership management and SELinux relabeling.
type ConfigMapVolumeSource struct {
	LocalObjectReference
	// If unspecified, each key-value pair in the Data field of the referenced
	// ConfigMap will be projected into the volume as a file whose name is the
	// key and content is the value. If specified, the listed keys will be
	// projected into the specified paths, and unlisted keys will not be
	// present. If a key is specified which is not present in the ConfigMap,
	// the volume setup will error unless it is marked optional. Paths must be
	// relative and may not contain the '..' path or start with '..'.
	// +optional
	Items []KeyToPath
	// Mode bits to use on created files by default. Must be a value between
	// 0 and 0777.
	// Directories within the path are not affected by this setting.
	// This might be in conflict with other options that affect the file
	// mode, like fsGroup, and the result can be other mode bits set.
	// +optional
	DefaultMode *int32
	// Specify whether the ConfigMap or its keys must be defined
	// +optional
	Optional *bool
}

// ConfigMapProjection adapts a ConfigMap into a projected volume.
//
// The contents of the target ConfigMap's Data field will be presented in a
// projected volume as files using the keys in the Data field as the file names,
// unless the items element is populated with specific mappings of keys to paths.
// Note that this is identical to a configmap volume source without the default
// mode.
type ConfigMapProjection struct {
	LocalObjectReference
	// If unspecified, each key-value pair in the Data field of the referenced
	// ConfigMap will be projected into the volume as a file whose name is the
	// key and content is the value. If specified, the listed keys will be
	// projected into the specified paths, and unlisted keys will not be
	// present. If a key is specified which is not present in the ConfigMap,
	// the volume setup will error unless it is marked optional. Paths must be
	// relative and may not contain the '..' path or start with '..'.
	// +optional
	Items []KeyToPath
	// Specify whether the ConfigMap or its keys must be defined
	// +optional
	Optional *bool
}

// ServiceAccountTokenProjection represents a projected service account token
// volume. This projection can be used to insert a service account token into
// the pods runtime filesystem for use against APIs (Kubernetes API Server or
// otherwise).
type ServiceAccountTokenProjection struct {
	// Audience is the intended audience of the token. A recipient of a token
	// must identify itself with an identifier specified in the audience of the
	// token, and otherwise should reject the token. The audience defaults to the
	// identifier of the apiserver.
	Audience string
	// ExpirationSeconds is the requested duration of validity of the service
	// account token. As the token approaches expiration, the kubelet volume
	// plugin will proactively rotate the service account token. The kubelet will
	// start trying to rotate the token if the token is older than 80 percent of
	// its time to live or if the token is older than 24 hours.Defaults to 1 hour
	// and must be at least 10 minutes.
	ExpirationSeconds int64
	// Path is the path relative to the mount point of the file to project the
	// token into.
	Path string
}

// ClusterTrustBundleProjection allows a pod to access the
// `.spec.trustBundle` field of a ClusterTrustBundle object in an auto-updating
// file.
type ClusterTrustBundleProjection struct {
	// Select a single ClusterTrustBundle by object name.   Mutually-exclusive
	// with SignerName and LabelSelector.
	Name *string

	// Select all ClusterTrustBundles for this signer that match LabelSelector.
	// Mutually-exclusive with Name.
	SignerName *string

	// Select all ClusterTrustBundles that match this LabelSelecotr.
	// Mutually-exclusive with Name.
	LabelSelector *metav1.LabelSelector

	// Block pod startup if the selected ClusterTrustBundle(s) aren't available?
	Optional *bool

	// Relative path from the volume root to write the bundle.
	Path string
}

// ProjectedVolumeSource represents a projected volume source
type ProjectedVolumeSource struct {
	// list of volume projections
	Sources []VolumeProjection
	// Mode bits to use on created files by default. Must be a value between
	// 0 and 0777.
	// Directories within the path are not affected by this setting.
	// This might be in conflict with other options that affect the file
	// mode, like fsGroup, and the result can be other mode bits set.
	// +optional
	DefaultMode *int32
}

// VolumeProjection that may be projected along with other supported volume types
type VolumeProjection struct {
	// all types below are the supported types for projection into the same volume

	// information about the secret data to project
	Secret *SecretProjection
	// information about the downwardAPI data to project
	DownwardAPI *DownwardAPIProjection
	// information about the configMap data to project
	ConfigMap *ConfigMapProjection
	// information about the serviceAccountToken data to project
	ServiceAccountToken *ServiceAccountTokenProjection
	// information about the ClusterTrustBundle data to project
	ClusterTrustBundle *ClusterTrustBundleProjection
}

// KeyToPath maps a string key to a path within a volume.
type KeyToPath struct {
	// The key to project.
	Key string

	// The relative path of the file to map the key to.
	// May not be an absolute path.
	// May not contain the path element '..'.
	// May not start with the string '..'.
	Path string
	// Optional: mode bits to use on this file, should be a value between 0
	// and 0777. If not specified, the volume defaultMode will be used.
	// This might be in conflict with other options that affect the file
	// mode, like fsGroup, and the result can be other mode bits set.
	// +optional
	Mode *int32
}

// LocalVolumeSource represents directly-attached storage with node affinity
type LocalVolumeSource struct {
	// The full path to the volume on the node.
	// It can be either a directory or block device (disk, partition, ...).
	Path string

	// Filesystem type to mount.
	// It applies only when the Path is a block device.
	// Must be a filesystem type supported by the host operating system.
	// Ex. "ext4", "xfs", "ntfs". The default value is to auto-select a filesystem if unspecified.
	// +optional
	FSType *string
}

// CSIPersistentVolumeSource represents storage that is managed by an external CSI volume driver.
type CSIPersistentVolumeSource struct {
	// Driver is the name of the driver to use for this volume.
	// Required.
	Driver string

	// VolumeHandle is the unique volume name returned by the CSI volume
	// plugin’s CreateVolume to refer to the volume on all subsequent calls.
	// Required.
	VolumeHandle string

	// Optional: The value to pass to ControllerPublishVolumeRequest.
	// Defaults to false (read/write).
	// +optional
	ReadOnly bool

	// Filesystem type to mount.
	// Must be a filesystem type supported by the host operating system.
	// Ex. "ext4", "xfs", "ntfs".
	// +optional
	FSType string

	// Attributes of the volume to publish.
	// +optional
	VolumeAttributes map[string]string

	// ControllerPublishSecretRef is a reference to the secret object containing
	// sensitive information to pass to the CSI driver to complete the CSI
	// ControllerPublishVolume and ControllerUnpublishVolume calls.
	// This field is optional, and may be empty if no secret is required. If the
	// secret object contains more than one secret, all secrets are passed.
	// +optional
	ControllerPublishSecretRef *SecretReference

	// NodeStageSecretRef is a reference to the secret object containing sensitive
	// information to pass to the CSI driver to complete the CSI NodeStageVolume
	// and NodeStageVolume and NodeUnstageVolume calls.
	// This field is optional, and may be empty if no secret is required. If the
	// secret object contains more than one secret, all secrets are passed.
	// +optional
	NodeStageSecretRef *SecretReference

	// NodePublishSecretRef is a reference to the secret object containing
	// sensitive information to pass to the CSI driver to complete the CSI
	// NodePublishVolume and NodeUnpublishVolume calls.
	// This field is optional, and may be empty if no secret is required. If the
	// secret object contains more than one secret, all secrets are passed.
	// +optional
	NodePublishSecretRef *SecretReference

	// ControllerExpandSecretRef is a reference to the secret object containing
	// sensitive information to pass to the CSI driver to complete the CSI
	// ControllerExpandVolume call.
	// This field is optional, and may be empty if no secret is required. If the
	// secret object contains more than one secret, all secrets are passed.
	// +optional
	ControllerExpandSecretRef *SecretReference

	// NodeExpandSecretRef is a reference to the secret object containing
	// sensitive information to pass to the CSI driver to complete the CSI
	// NodeExpandVolume call.
	// This field is optional, may be omitted if no secret is required. If the
	// secret object contains more than one secret, all secrets are passed.
	// +optional
	NodeExpandSecretRef *SecretReference
}

// CSIVolumeSource represents a source location of a volume to mount, managed by an external CSI driver
type CSIVolumeSource struct {
	// Driver is the name of the CSI driver that handles this volume.
	// Consult with your admin for the correct name as registered in the cluster.
	// Required.
	Driver string

	// Specifies a read-only configuration for the volume.
	// Defaults to false (read/write).
	// +optional
	ReadOnly *bool

	// Filesystem type to mount. Ex. "ext4", "xfs", "ntfs".
	// If not provided, the empty value is passed to the associated CSI driver
	// which will determine the default filesystem to apply.
	// +optional
	FSType *string

	// VolumeAttributes stores driver-specific properties that are passed to the CSI
	// driver. Consult your driver's documentation for supported values.
	// +optional
	VolumeAttributes map[string]string

	// NodePublishSecretRef is a reference to the secret object containing
	// sensitive information to pass to the CSI driver to complete the CSI
	// NodePublishVolume and NodeUnpublishVolume calls.
	// This field is optional, and  may be empty if no secret is required. If the
	// secret object contains more than one secret, all secret references are passed.
	// +optional
	NodePublishSecretRef *LocalObjectReference
}

// EphemeralVolumeSource represents an ephemeral volume that is handled by a normal storage driver.
type EphemeralVolumeSource struct {
	// VolumeClaimTemplate will be used to create a stand-alone PVC to provision the volume.
	// The pod in which this EphemeralVolumeSource is embedded will be the
	// owner of the PVC, i.e. the PVC will be deleted together with the
	// pod.  The name of the PVC will be `<pod name>-<volume name>` where
	// `<volume name>` is the name from the `PodSpec.Volumes` array
	// entry. Pod validation will reject the pod if the concatenated name
	// is not valid for a PVC (for example, too long).
	//
	// An existing PVC with that name that is not owned by the pod
	// will *not* be used for the pod to avoid using an unrelated
	// volume by mistake. Starting the pod is then blocked until
	// the unrelated PVC is removed. If such a pre-created PVC is
	// meant to be used by the pod, the PVC has to updated with an
	// owner reference to the pod once the pod exists. Normally
	// this should not be necessary, but it may be useful when
	// manually reconstructing a broken cluster.
	//
	// This field is read-only and no changes will be made by Kubernetes
	// to the PVC after it has been created.
	//
	// Required, must not be nil.
	VolumeClaimTemplate *PersistentVolumeClaimTemplate
}

// PersistentVolumeClaimTemplate is used to produce
// PersistentVolumeClaim objects as part of an EphemeralVolumeSource.
type PersistentVolumeClaimTemplate struct {
	// ObjectMeta may contain labels and annotations that will be copied into the PVC
	// when creating it. No other fields are allowed and will be rejected during
	// validation.
	// +optional
	metav1.ObjectMeta

	// Spec for the PersistentVolumeClaim. The entire content is
	// copied unchanged into the PVC that gets created from this
	// template. The same fields as in a PersistentVolumeClaim
	// are also valid here.
	Spec PersistentVolumeClaimSpec
}

// ContainerPort represents a network port in a single container
type ContainerPort struct {
	// Optional: If specified, this must be an IANA_SVC_NAME  Each named port
	// in a pod must have a unique name.
	// +optional
	Name string
	// Optional: If specified, this must be a valid port number, 0 < x < 65536.
	// If HostNetwork is specified, this must match ContainerPort.
	// +optional
	HostPort int32
	// Required: This must be a valid port number, 0 < x < 65536.
	ContainerPort int32
	// Required: Supports "TCP", "UDP" and "SCTP"
	// +optional
	Protocol Protocol
	// Optional: What host IP to bind the external port to.
	// +optional
	HostIP string
}

// VolumeMount describes a mounting of a Volume within a container.
type VolumeMount struct {
	// Required: This must match the Name of a Volume [above].
	Name string
	// Optional: Defaults to false (read-write).
	// +optional
	ReadOnly bool
	// RecursiveReadOnly specifies whether read-only mounts should be handled
	// recursively.
	//
	// If ReadOnly is false, this field has no meaning and must be unspecified.
	//
	// If ReadOnly is true, and this field is set to Disabled, the mount is not made
	// recursively read-only.  If this field is set to IfPossible, the mount is made
	// recursively read-only, if it is supported by the container runtime.  If this
	// field is set to Enabled, the mount is made recursively read-only if it is
	// supported by the container runtime, otherwise the pod will not be started and
	// an error will be generated to indicate the reason.
	//
	// If this field is set to IfPossible or Enabled, MountPropagation must be set to
	// None (or be unspecified, which defaults to None).
	//
	// If this field is not specified, it is treated as an equivalent of Disabled.
	//
	// +featureGate=RecursiveReadOnlyMounts
	// +optional
	RecursiveReadOnly *RecursiveReadOnlyMode
	// Required. If the path is not an absolute path (e.g. some/path) it
	// will be prepended with the appropriate root prefix for the operating
	// system.  On Linux this is '/', on Windows this is 'C:\'.
	MountPath string
	// Path within the volume from which the container's volume should be mounted.
	// Defaults to "" (volume's root).
	// +optional
	SubPath string
	// mountPropagation determines how mounts are propagated from the host
	// to container and the other way around.
	// When not set, MountPropagationNone is used.
	// This field is beta in 1.10.
	// When RecursiveReadOnly is set to IfPossible or to Enabled, MountPropagation must be None or unspecified
	// (which defaults to None).
	// +optional
	MountPropagation *MountPropagationMode
	// Expanded path within the volume from which the container's volume should be mounted.
	// Behaves similarly to SubPath but environment variable references $(VAR_NAME) are expanded using the container's environment.
	// Defaults to "" (volume's root).
	// SubPathExpr and SubPath are mutually exclusive.
	// +optional
	SubPathExpr string
}

// MountPropagationMode describes mount propagation.
type MountPropagationMode string

const (
	// MountPropagationNone means that the volume in a container will
	// not receive new mounts from the host or other containers, and filesystems
	// mounted inside the container won't be propagated to the host or other
	// containers.
	// Note that this mode corresponds to "private" in Linux terminology.
	MountPropagationNone MountPropagationMode = "None"
	// MountPropagationHostToContainer means that the volume in a container will
	// receive new mounts from the host or other containers, but filesystems
	// mounted inside the container won't be propagated to the host or other
	// containers.
	// Note that this mode is recursively applied to all mounts in the volume
	// ("rslave" in Linux terminology).
	MountPropagationHostToContainer MountPropagationMode = "HostToContainer"
	// MountPropagationBidirectional means that the volume in a container will
	// receive new mounts from the host or other containers, and its own mounts
	// will be propagated from the container to the host or other containers.
	// Note that this mode is recursively applied to all mounts in the volume
	// ("rshared" in Linux terminology).
	MountPropagationBidirectional MountPropagationMode = "Bidirectional"
)

// RecursiveReadOnlyMode describes recursive-readonly mode.
type RecursiveReadOnlyMode string

const (
	// RecursiveReadOnlyDisabled disables recursive-readonly mode.
	RecursiveReadOnlyDisabled RecursiveReadOnlyMode = "Disabled"
	// RecursiveReadOnlyIfPossible enables recursive-readonly mode if possible.
	RecursiveReadOnlyIfPossible RecursiveReadOnlyMode = "IfPossible"
	// RecursiveReadOnlyEnabled enables recursive-readonly mode, or raise an error.
	RecursiveReadOnlyEnabled RecursiveReadOnlyMode = "Enabled"
)

// VolumeDevice describes a mapping of a raw block device within a container.
type VolumeDevice struct {
	// name must match the name of a persistentVolumeClaim in the pod
	Name string
	// devicePath is the path inside of the container that the device will be mapped to.
	DevicePath string
}

// EnvVar represents an environment variable present in a Container.
type EnvVar struct {
	// Required: Name of the environment variable.
	// When the RelaxedEnvironmentVariableValidation feature gate is disabled, this must consist of alphabetic characters,
	// digits, '_', '-', or '.', and must not start with a digit.
	// When the RelaxedEnvironmentVariableValidation feature gate is enabled,
	// this may contain any printable ASCII characters except '='.
	Name string
	// Optional: no more than one of the following may be specified.
	// Optional: Defaults to ""; variable references $(VAR_NAME) are expanded
	// using the previously defined environment variables in the container and
	// any service environment variables.  If a variable cannot be resolved,
	// the reference in the input string will be unchanged.  Double $$ are
	// reduced to a single $, which allows for escaping the $(VAR_NAME)
	// syntax: i.e. "$$(VAR_NAME)" will produce the string literal
	// "$(VAR_NAME)".  Escaped references will never be expanded,
	// regardless of whether the variable exists or not.
	// +optional
	Value string
	// Optional: Specifies a source the value of this var should come from.
	// +optional
	ValueFrom *EnvVarSource
}

// EnvVarSource represents a source for the value of an EnvVar.
// Only one of its fields may be set.
type EnvVarSource struct {
	// Selects a field of the pod: supports metadata.name, metadata.namespace, `metadata.labels['<KEY>']`, `metadata.annotations['<KEY>']`,
	// metadata.uid, spec.nodeName, spec.serviceAccountName, status.hostIP, status.podIP, status.podIPs.
	// +optional
	FieldRef *ObjectFieldSelector
	// Selects a resource of the container: only resources limits and requests
	// (limits.cpu, limits.memory, limits.ephemeral-storage, requests.cpu, requests.memory and requests.ephemeral-storage) are currently supported.
	// +optional
	ResourceFieldRef *ResourceFieldSelector
	// Selects a key of a ConfigMap.
	// +optional
	ConfigMapKeyRef *ConfigMapKeySelector
	// Selects a key of a secret in the pod's namespace.
	// +optional
	SecretKeyRef *SecretKeySelector
}

// ObjectFieldSelector selects an APIVersioned field of an object.
type ObjectFieldSelector struct {
	// Required: Version of the schema the FieldPath is written in terms of.
	// If no value is specified, it will be defaulted to the APIVersion of the
	// enclosing object.
	APIVersion string
	// Required: Path of the field to select in the specified API version
	FieldPath string
}

// ResourceFieldSelector represents container resources (cpu, memory) and their output format
type ResourceFieldSelector struct {
	// Container name: required for volumes, optional for env vars
	// +optional
	ContainerName string
	// Required: resource to select
	Resource string
	// Specifies the output format of the exposed resources, defaults to "1"
	// +optional
	Divisor resource.Quantity
}

// ConfigMapKeySelector selects a key from a ConfigMap.
type ConfigMapKeySelector struct {
	// The ConfigMap to select from.
	LocalObjectReference
	// The key to select.
	Key string
	// Specify whether the ConfigMap or its key must be defined
	// +optional
	Optional *bool
}

// SecretKeySelector selects a key of a Secret.
type SecretKeySelector struct {
	// The name of the secret in the pod's namespace to select from.
	LocalObjectReference
	// The key of the secret to select from.  Must be a valid secret key.
	Key string
	// Specify whether the Secret or its key must be defined
	// +optional
	Optional *bool
}

// EnvFromSource represents the source of a set of ConfigMaps or Secrets
type EnvFromSource struct {
	// Optional text to prepend to the name of each environment variable. Must be a C_IDENTIFIER.
	// +optional
	Prefix string
	// The ConfigMap to select from.
	// +optional
	ConfigMapRef *ConfigMapEnvSource
	// The Secret to select from.
	// +optional
	SecretRef *SecretEnvSource
}

// ConfigMapEnvSource selects a ConfigMap to populate the environment
// variables with.
//
// The contents of the target ConfigMap's Data field will represent the
// key-value pairs as environment variables.
type ConfigMapEnvSource struct {
	// The ConfigMap to select from.
	LocalObjectReference
	// Specify whether the ConfigMap must be defined
	// +optional
	Optional *bool
}

// SecretEnvSource selects a Secret to populate the environment
// variables with.
//
// The contents of the target Secret's Data field will represent the
// key-value pairs as environment variables.
type SecretEnvSource struct {
	// The Secret to select from.
	LocalObjectReference
	// Specify whether the Secret must be defined
	// +optional
	Optional *bool
}

// HTTPHeader describes a custom header to be used in HTTP probes
type HTTPHeader struct {
	// The header field name.
	// This will be canonicalized upon output, so case-variant names will be understood as the same header.
	Name string
	// The header field value
	Value string
}

// HTTPGetAction describes an action based on HTTP Get requests.
type HTTPGetAction struct {
	// Optional: Path to access on the HTTP server.
	// +optional
	Path string
	// Required: Name or number of the port to access on the container.
	// +optional
	Port intstr.IntOrString
	// Optional: Host name to connect to, defaults to the pod IP. You
	// probably want to set "Host" in httpHeaders instead.
	// +optional
	Host string
	// Optional: Scheme to use for connecting to the host, defaults to HTTP.
	// +optional
	Scheme URIScheme
	// Optional: Custom headers to set in the request. HTTP allows repeated headers.
	// +optional
	HTTPHeaders []HTTPHeader
}

// URIScheme identifies the scheme used for connection to a host for Get actions
type URIScheme string

const (
	// URISchemeHTTP means that the scheme used will be http://
	URISchemeHTTP URIScheme = "HTTP"
	// URISchemeHTTPS means that the scheme used will be https://
	URISchemeHTTPS URIScheme = "HTTPS"
)

// TCPSocketAction describes an action based on opening a socket
type TCPSocketAction struct {
	// Required: Port to connect to.
	// +optional
	Port intstr.IntOrString
	// Optional: Host name to connect to, defaults to the pod IP.
	// +optional
	Host string
}

// ExecAction describes a "run in container" action.
type ExecAction struct {
	// Command is the command line to execute inside the container, the working directory for the
	// command  is root ('/') in the container's filesystem.  The command is simply exec'd, it is
	// not run inside a shell, so traditional shell instructions ('|', etc) won't work.  To use
	// a shell, you need to explicitly call out to that shell.
	// +optional
	Command []string
}

// SleepAction describes a "sleep" action.
type SleepAction struct {
	// Seconds is the number of seconds to sleep.
	Seconds int64
}

// Probe describes a health check to be performed against a container to determine whether it is
// alive or ready to receive traffic.
type Probe struct {
	// The action taken to determine the health of a container
	ProbeHandler
	// Length of time before health checking is activated.  In seconds.
	// +optional
	InitialDelaySeconds int32
	// Length of time before health checking times out.  In seconds.
	// +optional
	TimeoutSeconds int32
	// How often (in seconds) to perform the probe.
	// +optional
	PeriodSeconds int32
	// Minimum consecutive successes for the probe to be considered successful after having failed.
	// Must be 1 for liveness and startup.
	// +optional
	SuccessThreshold int32
	// Minimum consecutive failures for the probe to be considered failed after having succeeded.
	// +optional
	FailureThreshold int32
	// Optional duration in seconds the pod needs to terminate gracefully upon probe failure.
	// The grace period is the duration in seconds after the processes running in the pod are sent
	// a termination signal and the time when the processes are forcibly halted with a kill signal.
	// Set this value longer than the expected cleanup time for your process.
	// If this value is nil, the pod's terminationGracePeriodSeconds will be used. Otherwise, this
	// value overrides the value provided by the pod spec.
	// Value must be non-negative integer. The value zero indicates stop immediately via
	// the kill signal (no opportunity to shut down).
	// This is a beta field and requires enabling ProbeTerminationGracePeriod feature gate.
	// +optional
	TerminationGracePeriodSeconds *int64
}

// PullPolicy describes a policy for if/when to pull a container image
type PullPolicy string

const (
	// PullAlways means that kubelet always attempts to pull the latest image.  Container will fail If the pull fails.
	PullAlways PullPolicy = "Always"
	// PullNever means that kubelet never pulls an image, but only uses a local image.  Container will fail if the image isn't present
	PullNever PullPolicy = "Never"
	// PullIfNotPresent means that kubelet pulls if the image isn't present on disk. Container will fail if the image isn't present and the pull fails.
	PullIfNotPresent PullPolicy = "IfNotPresent"
)

// ResourceResizeRestartPolicy specifies how to handle container resource resize.
type ResourceResizeRestartPolicy string

// These are the valid resource resize restart policy values:
const (
	// 'NotRequired' means Kubernetes will try to resize the container
	// without restarting it, if possible. Kubernetes may however choose to
	// restart the container if it is unable to actuate resize without a
	// restart. For e.g. the runtime doesn't support restart-free resizing.
	NotRequired ResourceResizeRestartPolicy = "NotRequired"
	// 'RestartContainer' means Kubernetes will resize the container in-place
	// by stopping and starting the container when new resources are applied.
	// This is needed for legacy applications. For e.g. java apps using the
	// -xmxN flag which are unable to use resized memory without restarting.
	RestartContainer ResourceResizeRestartPolicy = "RestartContainer"
)

// ContainerResizePolicy represents resource resize policy for the container.
type ContainerResizePolicy struct {
	// Name of the resource to which this resource resize policy applies.
	// Supported values: cpu, memory.
	ResourceName ResourceName
	// Restart policy to apply when specified resource is resized.
	// If not specified, it defaults to NotRequired.
	RestartPolicy ResourceResizeRestartPolicy
}

// PreemptionPolicy describes a policy for if/when to preempt a pod.
type PreemptionPolicy string

const (
	// PreemptLowerPriority means that pod can preempt other pods with lower priority.
	PreemptLowerPriority PreemptionPolicy = "PreemptLowerPriority"
	// PreemptNever means that pod never preempts other pods with lower priority.
	PreemptNever PreemptionPolicy = "Never"
)

// TerminationMessagePolicy describes how termination messages are retrieved from a container.
type TerminationMessagePolicy string

const (
	// TerminationMessageReadFile is the default behavior and will set the container status message to
	// the contents of the container's terminationMessagePath when the container exits.
	TerminationMessageReadFile TerminationMessagePolicy = "File"
	// TerminationMessageFallbackToLogsOnError will read the most recent contents of the container logs
	// for the container status message when the container exits with an error and the
	// terminationMessagePath has no contents.
	TerminationMessageFallbackToLogsOnError TerminationMessagePolicy = "FallbackToLogsOnError"
)

// Capability represent POSIX capabilities type
type Capability string

// Capabilities represent POSIX capabilities that can be added or removed to a running container.
type Capabilities struct {
	// Added capabilities
	// +optional
	Add []Capability
	// Removed capabilities
	// +optional
	Drop []Capability
}

// ResourceRequirements describes the compute resource requirements.
type ResourceRequirements struct {
	// Limits describes the maximum amount of compute resources allowed.
	// +optional
	Limits ResourceList
	// Requests describes the minimum amount of compute resources required.
	// If Request is omitted for a container, it defaults to Limits if that is explicitly specified,
	// otherwise to an implementation-defined value
	// +optional
	Requests ResourceList
	// Claims lists the names of resources, defined in spec.resourceClaims,
	// that are used by this container.
	//
	// This is an alpha field and requires enabling the
	// DynamicResourceAllocation feature gate.
	//
	// This field is immutable. It can only be set for containers.
	//
	// +featureGate=DynamicResourceAllocation
	// +optional
	Claims []ResourceClaim
}

// VolumeResourceRequirements describes the storage resource requirements for a volume.
type VolumeResourceRequirements struct {
	// Limits describes the maximum amount of compute resources allowed.
	// +optional
	Limits ResourceList
	// Requests describes the minimum amount of compute resources required.
	// If Request is omitted for a container, it defaults to Limits if that is explicitly specified,
	// otherwise to an implementation-defined value
	// +optional
	Requests ResourceList
}

// ResourceClaim references one entry in PodSpec.ResourceClaims.
type ResourceClaim struct {
	// Name must match the name of one entry in pod.spec.resourceClaims of
	// the Pod where this field is used. It makes that resource available
	// inside a container.
	Name string

	// Request is the name chosen for a request in the referenced claim.
	// If empty, everything from the claim is made available, otherwise
	// only the result of this request.
	//
	// +optional
	Request string
}

// Container represents a single container that is expected to be run on the host.
type Container struct {
	// Required: This must be a DNS_LABEL.  Each container in a pod must
	// have a unique name.
	Name string
	// Required.
	Image string
	// Optional: The container image's entrypoint is used if this is not provided; cannot be updated.
	// Variable references $(VAR_NAME) are expanded using the container's environment.  If a variable
	// cannot be resolved, the reference in the input string will be unchanged.  Double $$ are reduced
	// to a single $, which allows for escaping the $(VAR_NAME) syntax: i.e. "$$(VAR_NAME)" will
	// produce the string literal "$(VAR_NAME)".  Escaped references will never be expanded, regardless
	// of whether the variable exists or not.
	// +optional
	Command []string
	// Optional: The container image's cmd is used if this is not provided; cannot be updated.
	// Variable references $(VAR_NAME) are expanded using the container's environment.  If a variable
	// cannot be resolved, the reference in the input string will be unchanged.  Double $$ are reduced
	// to a single $, which allows for escaping the $(VAR_NAME) syntax: i.e. "$$(VAR_NAME)" will
	// produce the string literal "$(VAR_NAME)".  Escaped references will never be expanded, regardless
	// of whether the variable exists or not.
	// +optional
	Args []string
	// Optional: Defaults to the container runtime's default working directory.
	// +optional
	WorkingDir string
	// +optional
	Ports []ContainerPort
	// List of sources to populate environment variables in the container.
	// The keys defined within a source must be a C_IDENTIFIER. All invalid keys
	// will be reported as an event when the container is starting. When a key exists in multiple
	// sources, the value associated with the last source will take precedence.
	// Values defined by an Env with a duplicate key will take precedence.
	// Cannot be updated.
	// +optional
	EnvFrom []EnvFromSource
	// +optional
	Env []EnvVar
	// Compute resource requirements.
	// +optional
	Resources ResourceRequirements
	// Resources resize policy for the container.
	// +featureGate=InPlacePodVerticalScaling
	// +optional
	ResizePolicy []ContainerResizePolicy
	// RestartPolicy defines the restart behavior of individual containers in a pod.
	// This field may only be set for init containers, and the only allowed value is "Always".
	// For non-init containers or when this field is not specified,
	// the restart behavior is defined by the Pod's restart policy and the container type.
	// Setting the RestartPolicy as "Always" for the init container will have the following effect:
	// this init container will be continually restarted on
	// exit until all regular containers have terminated. Once all regular
	// containers have completed, all init containers with restartPolicy "Always"
	// will be shut down. This lifecycle differs from normal init containers and
	// is often referred to as a "sidecar" container. Although this init
	// container still starts in the init container sequence, it does not wait
	// for the container to complete before proceeding to the next init
	// container. Instead, the next init container starts immediately after this
	// init container is started, or after any startupProbe has successfully
	// completed.
	// +featureGate=SidecarContainers
	// +optional
	RestartPolicy *ContainerRestartPolicy
	// +optional
	VolumeMounts []VolumeMount
	// volumeDevices is the list of block devices to be used by the container.
	// +optional
	VolumeDevices []VolumeDevice
	// +optional
	LivenessProbe *Probe
	// +optional
	ReadinessProbe *Probe
	// +optional
	StartupProbe *Probe
	// +optional
	Lifecycle *Lifecycle
	// Required.
	// +optional
	TerminationMessagePath string
	// +optional
	TerminationMessagePolicy TerminationMessagePolicy
	// Required: Policy for pulling images for this container
	ImagePullPolicy PullPolicy
	// Optional: SecurityContext defines the security options the container should be run with.
	// If set, the fields of SecurityContext override the equivalent fields of PodSecurityContext.
	// +optional
	SecurityContext *SecurityContext

	// Variables for interactive containers, these have very specialized use-cases (e.g. debugging)
	// and shouldn't be used for general purpose containers.
	// +optional
	Stdin bool
	// +optional
	StdinOnce bool
	// +optional
	TTY bool
}

// ProbeHandler defines a specific action that should be taken in a probe.
// One and only one of the fields must be specified.
type ProbeHandler struct {
	// Exec specifies the action to take.
	// +optional
	Exec *ExecAction
	// HTTPGet specifies the http request to perform.
	// +optional
	HTTPGet *HTTPGetAction
	// TCPSocket specifies an action involving a TCP port.
	// +optional
	TCPSocket *TCPSocketAction

	// GRPC specifies an action involving a GRPC port.
	// +optional
	GRPC *GRPCAction
}

// LifecycleHandler defines a specific action that should be taken in a lifecycle
// hook. One and only one of the fields, except TCPSocket must be specified.
type LifecycleHandler struct {
	// Exec specifies the action to take.
	// +optional
	Exec *ExecAction
	// HTTPGet specifies the http request to perform.
	// +optional
	HTTPGet *HTTPGetAction
	// Deprecated. TCPSocket is NOT supported as a LifecycleHandler and kept
	// for the backward compatibility. There are no validation of this field and
	// lifecycle hooks will fail in runtime when tcp handler is specified.
	// +optional
	TCPSocket *TCPSocketAction
	// Sleep represents the duration that the container should sleep before being terminated.
	// +featureGate=PodLifecycleSleepAction
	// +optional
	Sleep *SleepAction
}

type GRPCAction struct {
	// Port number of the gRPC service.
	// Note: Number must be in the range 1 to 65535.
	Port int32

	// Service is the name of the service to place in the gRPC HealthCheckRequest
	// (see https://github.com/grpc/grpc/blob/master/doc/health-checking.md).
	//
	// If this is not specified, the default behavior is to probe the server's overall health status.
	// +optional
	Service *string
}

// Lifecycle describes actions that the management system should take in response to container lifecycle
// events.  For the PostStart and PreStop lifecycle handlers, management of the container blocks
// until the action is complete, unless the container process fails, in which case the handler is aborted.
type Lifecycle struct {
	// PostStart is called immediately after a container is created.  If the handler fails, the container
	// is terminated and restarted.
	// More info: https://kubernetes.io/docs/concepts/containers/container-lifecycle-hooks/#container-hooks
	// +optional
	PostStart *LifecycleHandler
	// PreStop is called immediately before a container is terminated due to an
	// API request or management event such as liveness/startup probe failure,
	// preemption, resource contention, etc. The handler is not called if the
	// container crashes or exits. The Pod's termination grace period countdown begins before the
	// PreStop hook is executed. Regardless of the outcome of the handler, the
	// container will eventually terminate within the Pod's termination grace
	// period (unless delayed by finalizers). Other management of the container blocks until the hook completes
	// or until the termination grace period is reached.
	// More info: https://kubernetes.io/docs/concepts/containers/container-lifecycle-hooks/#container-hooks
	// +optional
	PreStop *LifecycleHandler
}

// The below types are used by kube_client and api_server.

// ConditionStatus defines conditions of resources
type ConditionStatus string

// These are valid condition statuses. "ConditionTrue" means a resource is in the condition;
// "ConditionFalse" means a resource is not in the condition; "ConditionUnknown" means kubernetes
// can't decide if a resource is in the condition or not. In the future, we could add other
// intermediate conditions, e.g. ConditionDegraded.
const (
	ConditionTrue    ConditionStatus = "True"
	ConditionFalse   ConditionStatus = "False"
	ConditionUnknown ConditionStatus = "Unknown"
)

// ContainerStateWaiting represents the waiting state of a container
type ContainerStateWaiting struct {
	// A brief CamelCase string indicating details about why the container is in waiting state.
	// +optional
	Reason string
	// A human-readable message indicating details about why the container is in waiting state.
	// +optional
	Message string
}

// ContainerStateRunning represents the running state of a container
type ContainerStateRunning struct {
	// +optional
	StartedAt metav1.Time
}

// ContainerStateTerminated represents the terminated state of a container
type ContainerStateTerminated struct {
	ExitCode int32
	// +optional
	Signal int32
	// +optional
	Reason string
	// +optional
	Message string
	// +optional
	StartedAt metav1.Time
	// +optional
	FinishedAt metav1.Time
	// +optional
	ContainerID string
}

// ContainerState holds a possible state of container.
// Only one of its members may be specified.
// If none of them is specified, the default one is ContainerStateWaiting.
type ContainerState struct {
	// +optional
	Waiting *ContainerStateWaiting
	// +optional
	Running *ContainerStateRunning
	// +optional
	Terminated *ContainerStateTerminated
}

// ContainerStatus contains details for the current status of this container.
type ContainerStatus struct {
	// Name is a DNS_LABEL representing the unique name of the container.
	// Each container in a pod must have a unique name across all container types.
	// Cannot be updated.
	Name string
	// State holds details about the container's current condition.
	// +optional
	State ContainerState
	// LastTerminationState holds the last termination state of the container to
	// help debug container crashes and restarts. This field is not
	// populated if the container is still running and RestartCount is 0.
	// +optional
	LastTerminationState ContainerState
	// Ready specifies whether the container is currently passing its readiness check.
	// The value will change as readiness probes keep executing. If no readiness
	// probes are specified, this field defaults to true once the container is
	// fully started (see Started field).
	//
	// The value is typically used to determine whether a container is ready to
	// accept traffic.
	Ready bool
	// RestartCount holds the number of times the container has been restarted.
	// Kubelet makes an effort to always increment the value, but there
	// are cases when the state may be lost due to node restarts and then the value
	// may be reset to 0. The value is never negative.
	RestartCount int32
	// Image is the name of container image that the container is running.
	// The container image may not match the image used in the PodSpec,
	// as it may have been resolved by the runtime.
	// More info: https://kubernetes.io/docs/concepts/containers/images.
	Image string
	// ImageID is the image ID of the container's image. The image ID may not
	// match the image ID of the image used in the PodSpec, as it may have been
	// resolved by the runtime.
	ImageID string
	// ContainerID is the ID of the container in the format '<type>://<container_id>'.
	// Where type is a container runtime identifier, returned from Version call of CRI API
	// (for example "containerd").
	// +optional
	ContainerID string
	// Started indicates whether the container has finished its postStart lifecycle hook
	// and passed its startup probe.
	// Initialized as false, becomes true after startupProbe is considered
	// successful. Resets to false when the container is restarted, or if kubelet
	// loses state temporarily. In both cases, startup probes will run again.
	// Is always true when no startupProbe is defined and container is running and
	// has passed the postStart lifecycle hook. The null value must be treated the
	// same as false.
	// +optional
	Started *bool
	// AllocatedResources represents the compute resources allocated for this container by the
	// node. Kubelet sets this value to Container.Resources.Requests upon successful pod admission
	// and after successfully admitting desired pod resize.
	// +featureGate=InPlacePodVerticalScalingAllocatedStatus
	// +optional
	AllocatedResources ResourceList
	// Resources represents the compute resource requests and limits that have been successfully
	// enacted on the running container after it has been started or has been successfully resized.
	// +featureGate=InPlacePodVerticalScaling
	// +optional
	Resources *ResourceRequirements
	// Status of volume mounts.
	// +listType=atomic
	// +optional
	// +featureGate=RecursiveReadOnlyMounts
	VolumeMounts []VolumeMountStatus
	// User represents user identity information initially attached to the first process of the container
	// +featureGate=SupplementalGroupsPolicy
	// +optional
	User *ContainerUser
	// AllocatedResourcesStatus represents the status of various resources
	// allocated for this Pod.
	// +featureGate=ResourceHealthStatus
	// +optional
	AllocatedResourcesStatus []ResourceStatus
}

type ResourceStatus struct {
	// Name of the resource. Must be unique within the pod and in case of non-DRA resource, match one of the resources from the pod spec.
	// For DRA resources, the value must be "claim:<claim_name>/<request>".
	// When this status is reported about a container, the "claim_name" and "request" must match one of the claims of this container.
	// +required
	Name ResourceName
	// List of unique resources health. Each element in the list contains an unique resource ID and its health.
	// At a minimum, for the lifetime of a Pod, resource ID must uniquely identify the resource allocated to the Pod on the Node.
	// If other Pod on the same Node reports the status with the same resource ID, it must be the same resource they share.
	// See ResourceID type definition for a specific format it has in various use cases.
	// +listType=map
	// +listMapKey=resourceID
	Resources []ResourceHealth

	// allow to extend this struct in future with the overall health fields or things like Device Plugin version
}

// ResourceID is calculated based on the source of this resource health information.
// For DevicePlugin:
//
//	DeviceID, where DeviceID is how device plugin identifies the device. The same DeviceID can be found in PodResources API.
//
// DevicePlugin ID is usually a constant for the lifetime of a Node and typically can be used to uniquely identify the device on the node.
//
// For DRA:
//
//	<driver name>/<pool name>/<device name>: such a device can be looked up in the information published by that DRA driver to learn more about it. It is designed to be globally unique in a cluster.
type ResourceID string

type ResourceHealthStatus string

const (
	ResourceHealthStatusHealthy   ResourceHealthStatus = "Healthy"
	ResourceHealthStatusUnhealthy ResourceHealthStatus = "Unhealthy"
	ResourceHealthStatusUnknown   ResourceHealthStatus = "Unknown"
)

// ResourceHealth represents the health of a resource. It has the latest device health information.
// This is a part of KEP https://kep.k8s.io/4680.
type ResourceHealth struct {
	// ResourceID is the unique identifier of the resource. See the ResourceID type for more information.
	ResourceID ResourceID
	// Health of the resource.
	// can be one of:
	//  - Healthy: operates as normal
	//  - Unhealthy: reported unhealthy. We consider this a temporary health issue
	//               since we do not have a mechanism today to distinguish
	//               temporary and permanent issues.
	//  - Unknown: The status cannot be determined.
	//             For example, Device Plugin got unregistered and hasn't been re-registered since.
	//
	// In future we may want to introduce the PermanentlyUnhealthy Status.
	Health ResourceHealthStatus
}

// ContainerUser represents user identity information
type ContainerUser struct {
	// Linux holds user identity information initially attached to the first process of the containers in Linux.
	// Note that the actual running identity can be changed if the process has enough privilege to do so.
	// +optional
	Linux *LinuxContainerUser

	// Windows holds user identity information of the first process of the containers in Windows
	// This is just reserved for future use.
	// Windows *WindowsContainerUser
}

// LinuxContainerUser represents user identity information in Linux containers
type LinuxContainerUser struct {
	// UID is the primary uid initially attached to the first process in the container
	UID int64
	// GID is the primary gid initially attached to the first process in the container
	GID int64
	// SupplementalGroups are the supplemental groups initially attached to the first process in the container
	SupplementalGroups []int64
}

// PodPhase is a label for the condition of a pod at the current time.
type PodPhase string

// These are the valid statuses of pods.
const (
	// PodPending means the pod has been accepted by the system, but one or more of the containers
	// has not been started. This includes time before being bound to a node, as well as time spent
	// pulling images onto the host.
	PodPending PodPhase = "Pending"
	// PodRunning means the pod has been bound to a node and all of the containers have been started.
	// At least one container is still running or is in the process of being restarted.
	PodRunning PodPhase = "Running"
	// PodSucceeded means that all containers in the pod have voluntarily terminated
	// with a container exit code of 0, and the system is not going to restart any of these containers.
	PodSucceeded PodPhase = "Succeeded"
	// PodFailed means that all containers in the pod have terminated, and at least one container has
	// terminated in a failure (exited with a non-zero exit code or was stopped by the system).
	PodFailed PodPhase = "Failed"
	// PodUnknown means that for some reason the state of the pod could not be obtained, typically due
	// to an error in communicating with the host of the pod.
	// Deprecated in v1.21: It isn't being set since 2015 (74da3b14b0c0f658b3bb8d2def5094686d0e9095)
	PodUnknown PodPhase = "Unknown"
)

// PodConditionType defines the condition of pod
type PodConditionType string

// These are valid conditions of pod.
const (
	// PodScheduled represents status of the scheduling process for this pod.
	PodScheduled PodConditionType = "PodScheduled"
	// PodReady means the pod is able to service requests and should be added to the
	// load balancing pools of all matching services.
	PodReady PodConditionType = "Ready"
	// PodInitialized means that all init containers in the pod have started successfully.
	PodInitialized PodConditionType = "Initialized"
	// ContainersReady indicates whether all containers in the pod are ready.
	ContainersReady PodConditionType = "ContainersReady"
	// DisruptionTarget indicates the pod is about to be terminated due to a
	// disruption (such as preemption, eviction API or garbage-collection).
	DisruptionTarget PodConditionType = "DisruptionTarget"
)

// PodCondition represents pod's condition
type PodCondition struct {
	Type   PodConditionType
	Status ConditionStatus
	// +optional
	LastProbeTime metav1.Time
	// +optional
	LastTransitionTime metav1.Time
	// +optional
	Reason string
	// +optional
	Message string
}

// PodResizeStatus shows status of desired resize of a pod's containers.
type PodResizeStatus string

const (
	// Pod resources resize has been requested and will be evaluated by node.
	PodResizeStatusProposed PodResizeStatus = "Proposed"
	// Pod resources resize has been accepted by node and is being actuated.
	PodResizeStatusInProgress PodResizeStatus = "InProgress"
	// Node cannot resize the pod at this time and will keep retrying.
	PodResizeStatusDeferred PodResizeStatus = "Deferred"
	// Requested pod resize is not feasible and will not be re-evaluated.
	PodResizeStatusInfeasible PodResizeStatus = "Infeasible"
)

// VolumeMountStatus shows status of volume mounts.
type VolumeMountStatus struct {
	// Name corresponds to the name of the original VolumeMount.
	Name string
	// MountPath corresponds to the original VolumeMount.
	MountPath string
	// ReadOnly corresponds to the original VolumeMount.
	// +optional
	ReadOnly bool
	// RecursiveReadOnly must be set to Disabled, Enabled, or unspecified (for non-readonly mounts).
	// An IfPossible value in the original VolumeMount must be translated to Disabled or Enabled,
	// depending on the mount result.
	// +featureGate=RecursiveReadOnlyMounts
	// +optional
	RecursiveReadOnly *RecursiveReadOnlyMode
}

// RestartPolicy describes how the container should be restarted.
// Only one of the following restart policies may be specified.
// If none of the following policies is specified, the default one
// is RestartPolicyAlways.
type RestartPolicy string

// These are valid restart policies
const (
	RestartPolicyAlways    RestartPolicy = "Always"
	RestartPolicyOnFailure RestartPolicy = "OnFailure"
	RestartPolicyNever     RestartPolicy = "Never"
)

// ContainerRestartPolicy is the restart policy for a single container.
// This may only be set for init containers and only allowed value is "Always".
type ContainerRestartPolicy string

const (
	ContainerRestartPolicyAlways ContainerRestartPolicy = "Always"
)

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// PodList is a list of Pods.
type PodList struct {
	metav1.TypeMeta
	// +optional
	metav1.ListMeta

	Items []Pod
}

// DNSPolicy defines how a pod's DNS will be configured.
type DNSPolicy string

const (
	// DNSClusterFirstWithHostNet indicates that the pod should use cluster DNS
	// first, if it is available, then fall back on the default
	// (as determined by kubelet) DNS settings.
	DNSClusterFirstWithHostNet DNSPolicy = "ClusterFirstWithHostNet"

	// DNSClusterFirst indicates that the pod should use cluster DNS
	// first unless hostNetwork is true, if it is available, then
	// fall back on the default (as determined by kubelet) DNS settings.
	DNSClusterFirst DNSPolicy = "ClusterFirst"

	// DNSDefault indicates that the pod should use the default (as
	// determined by kubelet) DNS settings.
	DNSDefault DNSPolicy = "Default"

	// DNSNone indicates that the pod should use empty DNS settings. DNS
	// parameters such as nameservers and search paths should be defined via
	// DNSConfig.
	DNSNone DNSPolicy = "None"
)

// NodeSelector represents the union of the results of one or more label queries
// over a set of nodes; that is, it represents the OR of the selectors represented
// by the node selector terms.
type NodeSelector struct {
	// Required. A list of node selector terms. The terms are ORed.
	NodeSelectorTerms []NodeSelectorTerm
}

// NodeSelectorTerm represents expressions and fields required to select nodes.
// A null or empty node selector term matches no objects. The requirements of
// them are ANDed.
// The TopologySelectorTerm type implements a subset of the NodeSelectorTerm.
type NodeSelectorTerm struct {
	// A list of node selector requirements by node's labels.
	MatchExpressions []NodeSelectorRequirement
	// A list of node selector requirements by node's fields.
	MatchFields []NodeSelectorRequirement
}

// NodeSelectorRequirement is a selector that contains values, a key, and an operator
// that relates the key and values.
type NodeSelectorRequirement struct {
	// The label key that the selector applies to.
	Key string
	// Represents a key's relationship to a set of values.
	// Valid operators are In, NotIn, Exists, DoesNotExist. Gt, and Lt.
	Operator NodeSelectorOperator
	// An array of string values. If the operator is In or NotIn,
	// the values array must be non-empty. If the operator is Exists or DoesNotExist,
	// the values array must be empty. If the operator is Gt or Lt, the values
	// array must have a single element, which will be interpreted as an integer.
	// This array is replaced during a strategic merge patch.
	// +optional
	Values []string
}

// NodeSelectorOperator is the set of operators that can be used in
// a node selector requirement.
type NodeSelectorOperator string

// These are valid values of NodeSelectorOperator
const (
	NodeSelectorOpIn           NodeSelectorOperator = "In"
	NodeSelectorOpNotIn        NodeSelectorOperator = "NotIn"
	NodeSelectorOpExists       NodeSelectorOperator = "Exists"
	NodeSelectorOpDoesNotExist NodeSelectorOperator = "DoesNotExist"
	NodeSelectorOpGt           NodeSelectorOperator = "Gt"
	NodeSelectorOpLt           NodeSelectorOperator = "Lt"
)

// TopologySelectorTerm represents the result of label queries.
// A null or empty topology selector term matches no objects.
// The requirements of them are ANDed.
// It provides a subset of functionality as NodeSelectorTerm.
// This is an alpha feature and may change in the future.
type TopologySelectorTerm struct {
	// A list of topology selector requirements by labels.
	// +optional
	MatchLabelExpressions []TopologySelectorLabelRequirement
}

// TopologySelectorLabelRequirement is a selector that matches given label.
// This is an alpha feature and may change in the future.
type TopologySelectorLabelRequirement struct {
	// The label key that the selector applies to.
	Key string
	// An array of string values. One value must match the label to be selected.
	// Each entry in Values is ORed.
	Values []string
}

// Affinity is a group of affinity scheduling rules.
type Affinity struct {
	// Describes node affinity scheduling rules for the pod.
	// +optional
	NodeAffinity *NodeAffinity
	// Describes pod affinity scheduling rules (e.g. co-locate this pod in the same node, zone, etc. as some other pod(s)).
	// +optional
	PodAffinity *PodAffinity
	// Describes pod anti-affinity scheduling rules (e.g. avoid putting this pod in the same node, zone, etc. as some other pod(s)).
	// +optional
	PodAntiAffinity *PodAntiAffinity
}

// PodAffinity is a group of inter pod affinity scheduling rules.
type PodAffinity struct {
	// NOT YET IMPLEMENTED. TODO: Uncomment field once it is implemented.
	// If the affinity requirements specified by this field are not met at
	// scheduling time, the pod will not be scheduled onto the node.
	// If the affinity requirements specified by this field cease to be met
	// at some point during pod execution (e.g. due to a pod label update), the
	// system will try to eventually evict the pod from its node.
	// When there are multiple elements, the lists of nodes corresponding to each
	// podAffinityTerm are intersected, i.e. all terms must be satisfied.
	// +optional
	// RequiredDuringSchedulingRequiredDuringExecution []PodAffinityTerm

	// If the affinity requirements specified by this field are not met at
	// scheduling time, the pod will not be scheduled onto the node.
	// If the affinity requirements specified by this field cease to be met
	// at some point during pod execution (e.g. due to a pod label update), the
	// system may or may not try to eventually evict the pod from its node.
	// When there are multiple elements, the lists of nodes corresponding to each
	// podAffinityTerm are intersected, i.e. all terms must be satisfied.
	// +optional
	RequiredDuringSchedulingIgnoredDuringExecution []PodAffinityTerm
	// The scheduler will prefer to schedule pods to nodes that satisfy
	// the affinity expressions specified by this field, but it may choose
	// a node that violates one or more of the expressions. The node that is
	// most preferred is the one with the greatest sum of weights, i.e.
	// for each node that meets all of the scheduling requirements (resource
	// request, requiredDuringScheduling affinity expressions, etc.),
	// compute a sum by iterating through the elements of this field and adding
	// "weight" to the sum if the node has pods which matches the corresponding podAffinityTerm; the
	// node(s) with the highest sum are the most preferred.
	// +optional
	PreferredDuringSchedulingIgnoredDuringExecution []WeightedPodAffinityTerm
}

// PodAntiAffinity is a group of inter pod anti affinity scheduling rules.
type PodAntiAffinity struct {
	// NOT YET IMPLEMENTED. TODO: Uncomment field once it is implemented.
	// If the anti-affinity requirements specified by this field are not met at
	// scheduling time, the pod will not be scheduled onto the node.
	// If the anti-affinity requirements specified by this field cease to be met
	// at some point during pod execution (e.g. due to a pod label update), the
	// system will try to eventually evict the pod from its node.
	// When there are multiple elements, the lists of nodes corresponding to each
	// podAffinityTerm are intersected, i.e. all terms must be satisfied.
	// +optional
	// RequiredDuringSchedulingRequiredDuringExecution []PodAffinityTerm

	// If the anti-affinity requirements specified by this field are not met at
	// scheduling time, the pod will not be scheduled onto the node.
	// If the anti-affinity requirements specified by this field cease to be met
	// at some point during pod execution (e.g. due to a pod label update), the
	// system may or may not try to eventually evict the pod from its node.
	// When there are multiple elements, the lists of nodes corresponding to each
	// podAffinityTerm are intersected, i.e. all terms must be satisfied.
	// +optional
	RequiredDuringSchedulingIgnoredDuringExecution []PodAffinityTerm
	// The scheduler will prefer to schedule pods to nodes that satisfy
	// the anti-affinity expressions specified by this field, but it may choose
	// a node that violates one or more of the expressions. The node that is
	// most preferred is the one with the greatest sum of weights, i.e.
	// for each node that meets all of the scheduling requirements (resource
	// request, requiredDuringScheduling anti-affinity expressions, etc.),
	// compute a sum by iterating through the elements of this field and adding
	// "weight" to the sum if the node has pods which matches the corresponding podAffinityTerm; the
	// node(s) with the highest sum are the most preferred.
	// +optional
	PreferredDuringSchedulingIgnoredDuringExecution []WeightedPodAffinityTerm
}

// WeightedPodAffinityTerm represents the weights of all of the matched WeightedPodAffinityTerm
// fields are added per-node to find the most preferred node(s)
type WeightedPodAffinityTerm struct {
	// weight associated with matching the corresponding podAffinityTerm,
	// in the range 1-100.
	Weight int32
	// Required. A pod affinity term, associated with the corresponding weight.
	PodAffinityTerm PodAffinityTerm
}

// PodAffinityTerm defines a set of pods (namely those matching the labelSelector
// relative to the given namespace(s)) that this pod should be
// co-located (affinity) or not co-located (anti-affinity) with,
// where co-located is defined as running on a node whose value of
// the label with key <topologyKey> matches that of any node on which
// a pod of the set of pods is running.
type PodAffinityTerm struct {
	// A label query over a set of resources, in this case pods.
	// If it's null, this PodAffinityTerm matches with no Pods.
	// +optional
	LabelSelector *metav1.LabelSelector
	// namespaces specifies a static list of namespace names that the term applies to.
	// The term is applied to the union of the namespaces listed in this field
	// and the ones selected by namespaceSelector.
	// null or empty namespaces list and null namespaceSelector means "this pod's namespace".
	// +optional
	Namespaces []string
	// This pod should be co-located (affinity) or not co-located (anti-affinity) with the pods matching
	// the labelSelector in the specified namespaces, where co-located is defined as running on a node
	// whose value of the label with key topologyKey matches that of any node on which any of the
	// selected pods is running.
	// Empty topologyKey is not allowed.
	TopologyKey string
	// A label query over the set of namespaces that the term applies to.
	// The term is applied to the union of the namespaces selected by this field
	// and the ones listed in the namespaces field.
	// null selector and null or empty namespaces list means "this pod's namespace".
	// An empty selector ({}) matches all namespaces.
	// +optional
	NamespaceSelector *metav1.LabelSelector
	// MatchLabelKeys is a set of pod label keys to select which pods will
	// be taken into consideration. The keys are used to lookup values from the
	// incoming pod labels, those key-value labels are merged with `labelSelector` as `key in (value)`
	// to select the group of existing pods which pods will be taken into consideration
	// for the incoming pod's pod (anti) affinity. Keys that don't exist in the incoming
	// pod labels will be ignored. The default value is empty.
	// The same key is forbidden to exist in both matchLabelKeys and labelSelector.
	// Also, matchLabelKeys cannot be set when labelSelector isn't set.
	// This is a beta field and requires enabling MatchLabelKeysInPodAffinity feature gate (enabled by default).
	//
	// +listType=atomic
	// +optional
	MatchLabelKeys []string
	// MismatchLabelKeys is a set of pod label keys to select which pods will
	// be taken into consideration. The keys are used to lookup values from the
	// incoming pod labels, those key-value labels are merged with `labelSelector` as `key notin (value)`
	// to select the group of existing pods which pods will be taken into consideration
	// for the incoming pod's pod (anti) affinity. Keys that don't exist in the incoming
	// pod labels will be ignored. The default value is empty.
	// The same key is forbidden to exist in both mismatchLabelKeys and labelSelector.
	// Also, mismatchLabelKeys cannot be set when labelSelector isn't set.
	// This is a beta field and requires enabling MatchLabelKeysInPodAffinity feature gate (enabled by default).
	//
	// +listType=atomic
	// +optional
	MismatchLabelKeys []string
}

// NodeAffinity is a group of node affinity scheduling rules.
type NodeAffinity struct {
	// NOT YET IMPLEMENTED. TODO: Uncomment field once it is implemented.
	// If the affinity requirements specified by this field are not met at
	// scheduling time, the pod will not be scheduled onto the node.
	// If the affinity requirements specified by this field cease to be met
	// at some point during pod execution (e.g. due to an update), the system
	// will try to eventually evict the pod from its node.
	// +optional
	// RequiredDuringSchedulingRequiredDuringExecution *NodeSelector

	// If the affinity requirements specified by this field are not met at
	// scheduling time, the pod will not be scheduled onto the node.
	// If the affinity requirements specified by this field cease to be met
	// at some point during pod execution (e.g. due to an update), the system
	// may or may not try to eventually evict the pod from its node.
	// +optional
	RequiredDuringSchedulingIgnoredDuringExecution *NodeSelector
	// The scheduler will prefer to schedule pods to nodes that satisfy
	// the affinity expressions specified by this field, but it may choose
	// a node that violates one or more of the expressions. The node that is
	// most preferred is the one with the greatest sum of weights, i.e.
	// for each node that meets all of the scheduling requirements (resource
	// request, requiredDuringScheduling affinity expressions, etc.),
	// compute a sum by iterating through the elements of this field and adding
	// "weight" to the sum if the node matches the corresponding matchExpressions; the
	// node(s) with the highest sum are the most preferred.
	// +optional
	PreferredDuringSchedulingIgnoredDuringExecution []PreferredSchedulingTerm
}

// PreferredSchedulingTerm represents an empty preferred scheduling term matches all objects with implicit weight 0
// (i.e. it's a no-op). A null preferred scheduling term matches no objects (i.e. is also a no-op).
type PreferredSchedulingTerm struct {
	// Weight associated with matching the corresponding nodeSelectorTerm, in the range 1-100.
	Weight int32
	// A node selector term, associated with the corresponding weight.
	Preference NodeSelectorTerm
}

// Taint represents taint that can be applied to the node.
// The node this Taint is attached to has the "effect" on
// any pod that does not tolerate the Taint.
type Taint struct {
	// Required. The taint key to be applied to a node.
	Key string
	// Required. The taint value corresponding to the taint key.
	// +optional
	Value string
	// Required. The effect of the taint on pods
	// that do not tolerate the taint.
	// Valid effects are NoSchedule, PreferNoSchedule and NoExecute.
	Effect TaintEffect
	// TimeAdded represents the time at which the taint was added.
	// It is only written for NoExecute taints.
	// +optional
	TimeAdded *metav1.Time
}

// TaintEffect defines the effects of Taint
type TaintEffect string

// These are valid values for TaintEffect
const (
	// Do not allow new pods to schedule onto the node unless they tolerate the taint,
	// but allow all pods submitted to Kubelet without going through the scheduler
	// to start, and allow all already-running pods to continue running.
	// Enforced by the scheduler.
	TaintEffectNoSchedule TaintEffect = "NoSchedule"
	// Like TaintEffectNoSchedule, but the scheduler tries not to schedule
	// new pods onto the node, rather than prohibiting new pods from scheduling
	// onto the node entirely. Enforced by the scheduler.
	TaintEffectPreferNoSchedule TaintEffect = "PreferNoSchedule"
	// NOT YET IMPLEMENTED. TODO: Uncomment field once it is implemented.
	// Like TaintEffectNoSchedule, but additionally do not allow pods submitted to
	// Kubelet without going through the scheduler to start.
	// Enforced by Kubelet and the scheduler.
	// TaintEffectNoScheduleNoAdmit TaintEffect = "NoScheduleNoAdmit"

	// Evict any already-running pods that do not tolerate the taint.
	// Currently enforced by NodeController.
	TaintEffectNoExecute TaintEffect = "NoExecute"
)

// Toleration represents the toleration object that can be attached to a pod.
// The pod this Toleration is attached to tolerates any taint that matches
// the triple <key,value,effect> using the matching operator <operator>.
type Toleration struct {
	// Key is the taint key that the toleration applies to. Empty means match all taint keys.
	// If the key is empty, operator must be Exists; this combination means to match all values and all keys.
	// +optional
	Key string
	// Operator represents a key's relationship to the value.
	// Valid operators are Exists and Equal. Defaults to Equal.
	// Exists is equivalent to wildcard for value, so that a pod can
	// tolerate all taints of a particular category.
	// +optional
	Operator TolerationOperator
	// Value is the taint value the toleration matches to.
	// If the operator is Exists, the value should be empty, otherwise just a regular string.
	// +optional
	Value string
	// Effect indicates the taint effect to match. Empty means match all taint effects.
	// When specified, allowed values are NoSchedule, PreferNoSchedule and NoExecute.
	// +optional
	Effect TaintEffect
	// TolerationSeconds represents the period of time the toleration (which must be
	// of effect NoExecute, otherwise this field is ignored) tolerates the taint. By default,
	// it is not set, which means tolerate the taint forever (do not evict). Zero and
	// negative values will be treated as 0 (evict immediately) by the system.
	// +optional
	TolerationSeconds *int64
}

// TolerationOperator is the set of operators that can be used in a toleration.
type TolerationOperator string

// These are valid values for TolerationOperator
const (
	TolerationOpExists TolerationOperator = "Exists"
	TolerationOpEqual  TolerationOperator = "Equal"
)

// PodReadinessGate contains the reference to a pod condition
type PodReadinessGate struct {
	// ConditionType refers to a condition in the pod's condition list with matching type.
	ConditionType PodConditionType
}

// PodSpec is a description of a pod
type PodSpec struct {
	Volumes []Volume
	// List of initialization containers belonging to the pod.
	InitContainers []Container
	// List of containers belonging to the pod.
	Containers []Container
	// List of ephemeral containers run in this pod. Ephemeral containers may be run in an existing
	// pod to perform user-initiated actions such as debugging. This list cannot be specified when
	// creating a pod, and it cannot be modified by updating the pod spec. In order to add an
	// ephemeral container to an existing pod, use the pod's ephemeralcontainers subresource.
	// +optional
	EphemeralContainers []EphemeralContainer
	// +optional
	RestartPolicy RestartPolicy
	// Optional duration in seconds the pod needs to terminate gracefully. May be decreased in delete request.
	// Value must be non-negative integer. The value zero indicates stop immediately via the kill
	// signal (no opportunity to shut down).
	// If this value is nil, the default grace period will be used instead.
	// The grace period is the duration in seconds after the processes running in the pod are sent
	// a termination signal and the time when the processes are forcibly halted with a kill signal.
	// Set this value longer than the expected cleanup time for your process.
	// +optional
	TerminationGracePeriodSeconds *int64
	// Optional duration in seconds relative to the StartTime that the pod may be active on a node
	// before the system actively tries to terminate the pod; value must be positive integer
	// +optional
	ActiveDeadlineSeconds *int64
	// Set DNS policy for the pod.
	// Defaults to "ClusterFirst".
	// Valid values are 'ClusterFirstWithHostNet', 'ClusterFirst', 'Default' or 'None'.
	// DNS parameters given in DNSConfig will be merged with the policy selected with DNSPolicy.
	// To have DNS options set along with hostNetwork, you have to specify DNS policy
	// explicitly to 'ClusterFirstWithHostNet'.
	// +optional
	DNSPolicy DNSPolicy
	// NodeSelector is a selector which must be true for the pod to fit on a node
	// +optional
	NodeSelector map[string]string

	// ServiceAccountName is the name of the ServiceAccount to use to run this pod
	// The pod will be allowed to use secrets referenced by the ServiceAccount
	ServiceAccountName string
	// AutomountServiceAccountToken indicates whether a service account token should be automatically mounted.
	// +optional
	AutomountServiceAccountToken *bool

	// NodeName indicates in which node this pod is scheduled.
	// If empty, this pod is a candidate for scheduling by the scheduler defined in schedulerName.
	// Once this field is set, the kubelet for this node becomes responsible for the lifecycle of this pod.
	// This field should not be used to express a desire for the pod to be scheduled on a specific node.
	// https://kubernetes.io/docs/concepts/scheduling-eviction/assign-pod-node/#nodename
	// +optional
	NodeName string
	// SecurityContext holds pod-level security attributes and common container settings.
	// Optional: Defaults to empty.  See type description for default values of each field.
	// +optional
	SecurityContext *PodSecurityContext
	// ImagePullSecrets is an optional list of references to secrets in the same namespace to use for pulling any of the images used by this PodSpec.
	// If specified, these secrets will be passed to individual puller implementations for them to use.
	// +optional
	ImagePullSecrets []LocalObjectReference
	// Specifies the hostname of the Pod.
	// If not specified, the pod's hostname will be set to a system-defined value.
	// +optional
	Hostname string
	// If specified, the fully qualified Pod hostname will be "<hostname>.<subdomain>.<pod namespace>.svc.<cluster domain>".
	// If not specified, the pod will not have a domainname at all.
	// +optional
	Subdomain string
	// If true the pod's hostname will be configured as the pod's FQDN, rather than the leaf name (the default).
	// In Linux containers, this means setting the FQDN in the hostname field of the kernel (the nodename field of struct utsname).
	// In Windows containers, this means setting the registry value of hostname for the registry key HKEY_LOCAL_MACHINE\\SYSTEM\\CurrentControlSet\\Services\\Tcpip\\Parameters to FQDN.
	// If a pod does not have FQDN, this has no effect.
	// +optional
	SetHostnameAsFQDN *bool
	// If specified, the pod's scheduling constraints
	// +optional
	Affinity *Affinity
	// If specified, the pod will be dispatched by specified scheduler.
	// If not specified, the pod will be dispatched by default scheduler.
	// +optional
	SchedulerName string
	// If specified, the pod's tolerations.
	// +optional
	Tolerations []Toleration
	// HostAliases is an optional list of hosts and IPs that will be injected into the pod's hosts
	// file if specified.
	// +optional
	HostAliases []HostAlias
	// If specified, indicates the pod's priority. "system-node-critical" and
	// "system-cluster-critical" are two special keywords which indicate the
	// highest priorities with the former being the highest priority. Any other
	// name must be defined by creating a PriorityClass object with that name.
	// If not specified, the pod priority will be default or zero if there is no
	// default.
	// +optional
	PriorityClassName string
	// The priority value. Various system components use this field to find the
	// priority of the pod. When Priority Admission Controller is enabled, it
	// prevents users from setting this field. The admission controller populates
	// this field from PriorityClassName.
	// The higher the value, the higher the priority.
	// +optional
	Priority *int32
	// PreemptionPolicy is the Policy for preempting pods with lower priority.
	// One of Never, PreemptLowerPriority.
	// Defaults to PreemptLowerPriority if unset.
	// +optional
	PreemptionPolicy *PreemptionPolicy
	// Specifies the DNS parameters of a pod.
	// Parameters specified here will be merged to the generated DNS
	// configuration based on DNSPolicy.
	// +optional
	DNSConfig *PodDNSConfig
	// If specified, all readiness gates will be evaluated for pod readiness.
	// A pod is ready when all its containers are ready AND
	// all conditions specified in the readiness gates have status equal to "True"
	// More info: https://git.k8s.io/enhancements/keps/sig-network/580-pod-readiness-gates
	// +optional
	ReadinessGates []PodReadinessGate
	// RuntimeClassName refers to a RuntimeClass object in the node.k8s.io group, which should be used
	// to run this pod.  If no RuntimeClass resource matches the named class, the pod will not be run.
	// If unset or empty, the "legacy" RuntimeClass will be used, which is an implicit class with an
	// empty definition that uses the default runtime handler.
	// More info: https://git.k8s.io/enhancements/keps/sig-node/585-runtime-class
	// +optional
	RuntimeClassName *string
	// Overhead represents the resource overhead associated with running a pod for a given RuntimeClass.
	// This field will be autopopulated at admission time by the RuntimeClass admission controller. If
	// the RuntimeClass admission controller is enabled, overhead must not be set in Pod create requests.
	// The RuntimeClass admission controller will reject Pod create requests which have the overhead already
	// set. If RuntimeClass is configured and selected in the PodSpec, Overhead will be set to the value
	// defined in the corresponding RuntimeClass, otherwise it will remain unset and treated as zero.
	// More info: https://git.k8s.io/enhancements/keps/sig-node/688-pod-overhead
	// +optional
	Overhead ResourceList
	// EnableServiceLinks indicates whether information about services should be injected into pod's
	// environment variables, matching the syntax of Docker links.
	// If not specified, the default is true.
	// +optional
	EnableServiceLinks *bool
	// TopologySpreadConstraints describes how a group of pods ought to spread across topology
	// domains. Scheduler will schedule pods in a way which abides by the constraints.
	// All topologySpreadConstraints are ANDed.
	// +optional
	TopologySpreadConstraints []TopologySpreadConstraint
	// Specifies the OS of the containers in the pod.
	// Some pod and container fields are restricted if this is set.
	//
	// If the OS field is set to linux, the following fields must be unset:
	// - securityContext.windowsOptions
	//
	// If the OS field is set to windows, following fields must be unset:
	// - spec.hostPID
	// - spec.hostIPC
	// - spec.hostUsers
	// - spec.securityContext.appArmorProfile
	// - spec.securityContext.seLinuxOptions
	// - spec.securityContext.seccompProfile
	// - spec.securityContext.fsGroup
	// - spec.securityContext.fsGroupChangePolicy
	// - spec.securityContext.sysctls
	// - spec.shareProcessNamespace
	// - spec.securityContext.runAsUser
	// - spec.securityContext.runAsGroup
	// - spec.securityContext.supplementalGroups
	// - spec.securityContext.supplementalGroupsPolicy
	// - spec.containers[*].securityContext.appArmorProfile
	// - spec.containers[*].securityContext.seLinuxOptions
	// - spec.containers[*].securityContext.seccompProfile
	// - spec.containers[*].securityContext.capabilities
	// - spec.containers[*].securityContext.readOnlyRootFilesystem
	// - spec.containers[*].securityContext.privileged
	// - spec.containers[*].securityContext.allowPrivilegeEscalation
	// - spec.containers[*].securityContext.procMount
	// - spec.containers[*].securityContext.runAsUser
	// - spec.containers[*].securityContext.runAsGroup
	// +optional
	OS *PodOS

	// SchedulingGates is an opaque list of values that if specified will block scheduling the pod.
	// If schedulingGates is not empty, the pod will stay in the SchedulingGated state and the
	// scheduler will not attempt to schedule the pod.
	//
	// SchedulingGates can only be set at pod creation time, and be removed only afterwards.
	//
	// +optional
	SchedulingGates []PodSchedulingGate
	// ResourceClaims defines which ResourceClaims must be allocated
	// and reserved before the Pod is allowed to start. The resources
	// will be made available to those containers which consume them
	// by name.
	//
	// This is an alpha field and requires enabling the
	// DynamicResourceAllocation feature gate.
	//
	// This field is immutable.
	//
	// +featureGate=DynamicResourceAllocation
	// +optional
	ResourceClaims []PodResourceClaim
	// Resources is the total amount of CPU and Memory resources required by all
	// containers in the pod. It supports specifying Requests and Limits for
	// "cpu" and "memory" resource names only. ResourceClaims are not supported.
	//
	// This field enables fine-grained control over resource allocation for the
	// entire pod, allowing resource sharing among containers in a pod.
	// TODO: For beta graduation, expand this comment with a detailed explanation.
	//
	// This is an alpha field and requires enabling the PodLevelResources feature
	// gate.
	//
	// +featureGate=PodLevelResources
	// +optional
	Resources *ResourceRequirements
}

// PodResourceClaim references exactly one ResourceClaim through a ClaimSource.
// It adds a name to it that uniquely identifies the ResourceClaim inside the Pod.
// Containers that need access to the ResourceClaim reference it with this name.
type PodResourceClaim struct {
	// Name uniquely identifies this resource claim inside the pod.
	// This must be a DNS_LABEL.
	Name string

	// ResourceClaimName is the name of a ResourceClaim object in the same
	// namespace as this pod.
	//
	// Exactly one of ResourceClaimName and ResourceClaimTemplateName must
	// be set.
	ResourceClaimName *string

	// ResourceClaimTemplateName is the name of a ResourceClaimTemplate
	// object in the same namespace as this pod.
	//
	// The template will be used to create a new ResourceClaim, which will
	// be bound to this pod. When this pod is deleted, the ResourceClaim
	// will also be deleted. The pod name and resource name, along with a
	// generated component, will be used to form a unique name for the
	// ResourceClaim, which will be recorded in pod.status.resourceClaimStatuses.
	//
	// This field is immutable and no changes will be made to the
	// corresponding ResourceClaim by the control plane after creating the
	// ResourceClaim.
	//
	// Exactly one of ResourceClaimName and ResourceClaimTemplateName must
	// be set.
	ResourceClaimTemplateName *string
}

// PodResourceClaimStatus is stored in the PodStatus for each PodResourceClaim
// which references a ResourceClaimTemplate. It stores the generated name for
// the corresponding ResourceClaim.
type PodResourceClaimStatus struct {
	// Name uniquely identifies this resource claim inside the pod.
	// This must match the name of an entry in pod.spec.resourceClaims,
	// which implies that the string must be a DNS_LABEL.
	Name string

	// ResourceClaimName is the name of the ResourceClaim that was
	// generated for the Pod in the namespace of the Pod. If this is
	// unset, then generating a ResourceClaim was not necessary. The
	// pod.spec.resourceClaims entry can be ignored in this case.
	ResourceClaimName *string
}

// OSName is the set of OS'es that can be used in OS.
type OSName string

// These are valid values for OSName
const (
	Linux   OSName = "linux"
	Windows OSName = "windows"
)

// PodOS defines the OS parameters of a pod.
type PodOS struct {
	// Name is the name of the operating system. The currently supported values are linux and windows.
	// Additional value may be defined in future and can be one of:
	// https://github.com/opencontainers/runtime-spec/blob/master/config.md#platform-specific-configuration
	// Clients should expect to handle additional values and treat unrecognized values in this field as os: null
	Name OSName
}

// PodSchedulingGate is associated to a Pod to guard its scheduling.
type PodSchedulingGate struct {
	// Name of the scheduling gate.
	// Each scheduling gate must have a unique name field.
	Name string
}

// HostAlias holds the mapping between IP and hostnames that will be injected as an entry in the
// pod's hosts file.
type HostAlias struct {
	IP        string
	Hostnames []string
}

// Sysctl defines a kernel parameter to be set
type Sysctl struct {
	// Name of a property to set
	Name string
	// Value of a property to set
	Value string
}

// PodFSGroupChangePolicy holds policies that will be used for applying fsGroup to a volume
// when volume is mounted.
type PodFSGroupChangePolicy string

const (
	// FSGroupChangeOnRootMismatch indicates that volume's ownership and permissions will be changed
	// only when permission and ownership of root directory does not match with expected
	// permissions on the volume. This can help shorten the time it takes to change
	// ownership and permissions of a volume.
	FSGroupChangeOnRootMismatch PodFSGroupChangePolicy = "OnRootMismatch"
	// FSGroupChangeAlways indicates that volume's ownership and permissions
	// should always be changed whenever volume is mounted inside a Pod. This the default
	// behavior.
	FSGroupChangeAlways PodFSGroupChangePolicy = "Always"
)

// SupplementalGroupsPolicy defines how supplemental groups
// of the first container processes are calculated.
type SupplementalGroupsPolicy string

const (
	// SupplementalGroupsPolicyMerge means that the container's provided
	// SupplementalGroups and FsGroup (specified in SecurityContext) will be
	// merged with the primary user's groups as defined in the container image
	// (in /etc/group).
	SupplementalGroupsPolicyMerge SupplementalGroupsPolicy = "Merge"
	// SupplementalGroupsPolicyStrict means that the container's provided
	// SupplementalGroups and FsGroup (specified in SecurityContext) will be
	// used instead of any groups defined in the container image.
	SupplementalGroupsPolicyStrict SupplementalGroupsPolicy = "Strict"
)

// PodSELinuxChangePolicy defines how the container's SELinux label is applied to all volumes used by the Pod.
type PodSELinuxChangePolicy string

const (
	// Recursive relabeling of all Pod volumes by the container runtime.
	// This may be slow for large volumes, but allows mixing privileged and unprivileged Pods sharing the same volume on the same node.
	SELinuxChangePolicyRecursive PodSELinuxChangePolicy = "Recursive"
	// MountOption mounts all eligible Pod volumes with `-o context` mount option.
	// This requires all Pods that share the same volume to use the same SELinux label.
	// It is not possible to share the same volume among privileged and unprivileged Pods.
	// Eligible volumes are in-tree FibreChannel and iSCSI volumes, and all CSI volumes
	// whose CSI driver announces SELinux support by setting spec.seLinuxMount: true in their
	// CSIDriver instance. Other volumes are always re-labelled recursively.
	SELinuxChangePolicyMountOption PodSELinuxChangePolicy = "MountOption"
)

// PodSecurityContext holds pod-level security attributes and common container settings.
// Some fields are also present in container.securityContext.  Field values of
// container.securityContext take precedence over field values of PodSecurityContext.
type PodSecurityContext struct {
	// Use the host's network namespace.  If this option is set, the ports that will be
	// used must be specified.
	// Optional: Default to false
	// +k8s:conversion-gen=false
	// +optional
	HostNetwork bool
	// Use the host's pid namespace.
	// Optional: Default to false.
	// Note that this field cannot be set when spec.os.name is windows.
	// +k8s:conversion-gen=false
	// +optional
	HostPID bool
	// Use the host's ipc namespace.
	// Optional: Default to false.
	// Note that this field cannot be set when spec.os.name is windows.
	// +k8s:conversion-gen=false
	// +optional
	HostIPC bool
	// Share a single process namespace between all of the containers in a pod.
	// When this is set containers will be able to view and signal processes from other containers
	// in the same pod, and the first process in each container will not be assigned PID 1.
	// HostPID and ShareProcessNamespace cannot both be set.
	// Note that this field cannot be set when spec.os.name is windows.
	// Optional: Default to false.
	// +k8s:conversion-gen=false
	// +optional
	ShareProcessNamespace *bool
	// Use the host's user namespace.
	// Optional: Default to true.
	// If set to true or not present, the pod will be run in the host user namespace, useful
	// for when the pod needs a feature only available to the host user namespace, such as
	// loading a kernel module with CAP_SYS_MODULE.
	// When set to false, a new user namespace is created for the pod. Setting false is useful
	// for mitigating container breakout vulnerabilities even allowing users to run their
	// containers as root without actually having root privileges on the host.
	// Note that this field cannot be set when spec.os.name is windows.
	// +k8s:conversion-gen=false
	// +optional
	HostUsers *bool
	// The SELinux context to be applied to all containers.
	// If unspecified, the container runtime will allocate a random SELinux context for each
	// container.  May also be set in SecurityContext.  If set in
	// both SecurityContext and PodSecurityContext, the value specified in SecurityContext
	// takes precedence for that container.
	// Note that this field cannot be set when spec.os.name is windows.
	// +optional
	SELinuxOptions *SELinuxOptions
	// The Windows specific settings applied to all containers.
	// If unspecified, the options within a container's SecurityContext will be used.
	// If set in both SecurityContext and PodSecurityContext, the value specified in SecurityContext takes precedence.
	// Note that this field cannot be set when spec.os.name is linux.
	// +optional
	WindowsOptions *WindowsSecurityContextOptions
	// The UID to run the entrypoint of the container process.
	// Defaults to user specified in image metadata if unspecified.
	// May also be set in SecurityContext.  If set in both SecurityContext and
	// PodSecurityContext, the value specified in SecurityContext takes precedence
	// for that container.
	// Note that this field cannot be set when spec.os.name is windows.
	// +optional
	RunAsUser *int64
	// The GID to run the entrypoint of the container process.
	// Uses runtime default if unset.
	// May also be set in SecurityContext.  If set in both SecurityContext and
	// PodSecurityContext, the value specified in SecurityContext takes precedence
	// for that container.
	// Note that this field cannot be set when spec.os.name is windows.
	// +optional
	RunAsGroup *int64
	// Indicates that the container must run as a non-root user.
	// If true, the Kubelet will validate the image at runtime to ensure that it
	// does not run as UID 0 (root) and fail to start the container if it does.
	// If unset or false, no such validation will be performed.
	// May also be set in SecurityContext.  If set in both SecurityContext and
	// PodSecurityContext, the value specified in SecurityContext takes precedence
	// for that container.
	// +optional
	RunAsNonRoot *bool
	// A list of groups applied to the first process run in each container, in
	// addition to the container's primary GID and fsGroup (if specified).  If
	// the SupplementalGroupsPolicy feature is enabled, the
	// supplementalGroupsPolicy field determines whether these are in addition
	// to or instead of any group memberships defined in the container image.
	// If unspecified, no additional groups are added, though group memberships
	// defined in the container image may still be used, depending on the
	// supplementalGroupsPolicy field.
	// Note that this field cannot be set when spec.os.name is windows.
	// +optional
	SupplementalGroups []int64
	// Defines how supplemental groups of the first container processes are calculated.
	// Valid values are "Merge" and "Strict". If not specified, "Merge" is used.
	// (Alpha) Using the field requires the SupplementalGroupsPolicy feature gate to be enabled
	// and the container runtime must implement support for this feature.
	// Note that this field cannot be set when spec.os.name is windows.
	// TODO: update the default value to "Merge" when spec.os.name is not windows in v1.34
	// +featureGate=SupplementalGroupsPolicy
	// +optional
	SupplementalGroupsPolicy *SupplementalGroupsPolicy
	// A special supplemental group that applies to all containers in a pod.
	// Some volume types allow the Kubelet to change the ownership of that volume
	// to be owned by the pod:
	//
	// 1. The owning GID will be the FSGroup
	// 2. The setgid bit is set (new files created in the volume will be owned by FSGroup)
	// 3. The permission bits are OR'd with rw-rw----
	//
	// If unset, the Kubelet will not modify the ownership and permissions of any volume.
	// Note that this field cannot be set when spec.os.name is windows.
	// +optional
	FSGroup *int64
	// fsGroupChangePolicy defines behavior of changing ownership and permission of the volume
	// before being exposed inside Pod. This field will only apply to
	// volume types which support fsGroup based ownership(and permissions).
	// It will have no effect on ephemeral volume types such as: secret, configmaps
	// and emptydir.
	// Valid values are "OnRootMismatch" and "Always". If not specified, "Always" is used.
	// Note that this field cannot be set when spec.os.name is windows.
	// +optional
	FSGroupChangePolicy *PodFSGroupChangePolicy
	// Sysctls hold a list of namespaced sysctls used for the pod. Pods with unsupported
	// sysctls (by the container runtime) might fail to launch.
	// Note that this field cannot be set when spec.os.name is windows.
	// +optional
	Sysctls []Sysctl
	// The seccomp options to use by the containers in this pod.
	// Note that this field cannot be set when spec.os.name is windows.
	// +optional
	SeccompProfile *SeccompProfile
	// appArmorProfile is the AppArmor options to use by the containers in this pod.
	// Note that this field cannot be set when spec.os.name is windows.
	// +optional
	AppArmorProfile *AppArmorProfile
	// seLinuxChangePolicy defines how the container's SELinux label is applied to all volumes used by the Pod.
	// It has no effect on nodes that do not support SELinux or to volumes does not support SELinux.
	// Valid values are "MountOption" and "Recursive".
	//
	// "Recursive" means relabeling of all files on all Pod volumes by the container runtime.
	// This may be slow for large volumes, but allows mixing privileged and unprivileged Pods sharing the same volume on the same node.
	//
	// "MountOption" mounts all eligible Pod volumes with `-o context` mount option.
	// This requires all Pods that share the same volume to use the same SELinux label.
	// It is not possible to share the same volume among privileged and unprivileged Pods.
	// Eligible volumes are in-tree FibreChannel and iSCSI volumes, and all CSI volumes
	// whose CSI driver announces SELinux support by setting spec.seLinuxMount: true in their
	// CSIDriver instance. Other volumes are always re-labelled recursively.
	// "MountOption" value is allowed only when SELinuxMount feature gate is enabled.
	//
	// If not specified and SELinuxMount feature gate is enabled, "MountOption" is used.
	// If not specified and SELinuxMount feature gate is disabled, "MountOption" is used for ReadWriteOncePod volumes
	// and "Recursive" for all other volumes.
	//
	// This field affects only Pods that have SELinux label set, either in PodSecurityContext or in SecurityContext of all containers.
	//
	// All Pods that use the same volume should use the same seLinuxChangePolicy, otherwise some pods can get stuck in ContainerCreating state.
	// Note that this field cannot be set when spec.os.name is windows.
	// +featureGate=SELinuxChangePolicy
	// +optional
	SELinuxChangePolicy *PodSELinuxChangePolicy
}

// SeccompProfile defines a pod/container's seccomp profile settings.
// Only one profile source may be set.
// +union
type SeccompProfile struct {
	// +unionDiscriminator
	Type SeccompProfileType
	// Load a profile defined in static file on the node.
	// The profile must be preconfigured on the node to work.
	// LocalhostProfile cannot be an absolute nor a descending path.
	// +optional
	LocalhostProfile *string
}

// SeccompProfileType defines the supported seccomp profile types.
type SeccompProfileType string

const (
	// SeccompProfileTypeUnconfined is when no seccomp profile is applied (A.K.A. unconfined).
	SeccompProfileTypeUnconfined SeccompProfileType = "Unconfined"
	// SeccompProfileTypeRuntimeDefault represents the default container runtime seccomp profile.
	SeccompProfileTypeRuntimeDefault SeccompProfileType = "RuntimeDefault"
	// SeccompProfileTypeLocalhost represents custom made profiles stored on the node's disk.
	SeccompProfileTypeLocalhost SeccompProfileType = "Localhost"
)

// AppArmorProfile defines a pod or container's AppArmor settings.
// +union
type AppArmorProfile struct {
	// type indicates which kind of AppArmor profile will be applied.
	// Valid options are:
	//   Localhost - a profile pre-loaded on the node.
	//   RuntimeDefault - the container runtime's default profile.
	//   Unconfined - no AppArmor enforcement.
	// +unionDescriminator
	Type AppArmorProfileType

	// localhostProfile indicates a profile loaded on the node that should be used.
	// The profile must be preconfigured on the node to work.
	// Must match the loaded name of the profile.
	// Must be set if and only if type is "Localhost".
	// +optional
	LocalhostProfile *string
}

// +enum
type AppArmorProfileType string

const (
	// AppArmorProfileTypeUnconfined indicates that no AppArmor profile should be enforced.
	AppArmorProfileTypeUnconfined AppArmorProfileType = "Unconfined"
	// AppArmorProfileTypeRuntimeDefault indicates that the container runtime's default AppArmor
	// profile should be used.
	AppArmorProfileTypeRuntimeDefault AppArmorProfileType = "RuntimeDefault"
	// AppArmorProfileTypeLocalhost indicates that a profile pre-loaded on the node should be used.
	AppArmorProfileTypeLocalhost AppArmorProfileType = "Localhost"
)

// PodQOSClass defines the supported qos classes of Pods.
type PodQOSClass string

// These are valid values for PodQOSClass
const (
	// PodQOSGuaranteed is the Guaranteed qos class.
	PodQOSGuaranteed PodQOSClass = "Guaranteed"
	// PodQOSBurstable is the Burstable qos class.
	PodQOSBurstable PodQOSClass = "Burstable"
	// PodQOSBestEffort is the BestEffort qos class.
	PodQOSBestEffort PodQOSClass = "BestEffort"
)

// PodDNSConfig defines the DNS parameters of a pod in addition to
// those generated from DNSPolicy.
type PodDNSConfig struct {
	// A list of DNS name server IP addresses.
	// This will be appended to the base nameservers generated from DNSPolicy.
	// Duplicated nameservers will be removed.
	// +optional
	Nameservers []string
	// A list of DNS search domains for host-name lookup.
	// This will be appended to the base search paths generated from DNSPolicy.
	// Duplicated search paths will be removed.
	// +optional
	Searches []string
	// A list of DNS resolver options.
	// This will be merged with the base options generated from DNSPolicy.
	// Duplicated entries will be removed. Resolution options given in Options
	// will override those that appear in the base DNSPolicy.
	// +optional
	Options []PodDNSConfigOption
}

// PodDNSConfigOption defines DNS resolver options of a pod.
type PodDNSConfigOption struct {
	// Required.
	Name string
	// +optional
	Value *string
}

// PodIP represents a single IP address allocated to the pod.
type PodIP struct {
	// IP is the IP address assigned to the pod
	IP string
}

// HostIP represents a single IP address allocated to the host.
type HostIP struct {
	// IP is the IP address assigned to the host
	IP string
}

// EphemeralContainerCommon is a copy of all fields in Container to be inlined in
// EphemeralContainer. This separate type allows easy conversion from EphemeralContainer
// to Container and allows separate documentation for the fields of EphemeralContainer.
// When a new field is added to Container it must be added here as well.
type EphemeralContainerCommon struct {
	// Required: This must be a DNS_LABEL.  Each container in a pod must
	// have a unique name.
	Name string
	// Required.
	Image string
	// Optional: The container image's entrypoint is used if this is not provided; cannot be updated.
	// Variable references $(VAR_NAME) are expanded using the container's environment.  If a variable
	// cannot be resolved, the reference in the input string will be unchanged.  Double $$ are reduced
	// to a single $, which allows for escaping the $(VAR_NAME) syntax: i.e. "$$(VAR_NAME)" will
	// produce the string literal "$(VAR_NAME)".  Escaped references will never be expanded, regardless
	// of whether the variable exists or not.
	// +optional
	Command []string
	// Optional: The container image's cmd is used if this is not provided; cannot be updated.
	// Variable references $(VAR_NAME) are expanded using the container's environment.  If a variable
	// cannot be resolved, the reference in the input string will be unchanged.  Double $$ are reduced
	// to a single $, which allows for escaping the $(VAR_NAME) syntax: i.e. "$$(VAR_NAME)" will
	// produce the string literal "$(VAR_NAME)".  Escaped references will never be expanded, regardless
	// of whether the variable exists or not.
	// +optional
	Args []string
	// Optional: Defaults to the container runtime's default working directory.
	// +optional
	WorkingDir string
	// Ports are not allowed for ephemeral containers.
	// +optional
	Ports []ContainerPort
	// List of sources to populate environment variables in the container.
	// The keys defined within a source must be a C_IDENTIFIER. All invalid keys
	// will be reported as an event when the container is starting. When a key exists in multiple
	// sources, the value associated with the last source will take precedence.
	// Values defined by an Env with a duplicate key will take precedence.
	// Cannot be updated.
	// +optional
	EnvFrom []EnvFromSource
	// +optional
	Env []EnvVar
	// Resources are not allowed for ephemeral containers. Ephemeral containers use spare resources
	// already allocated to the pod.
	// +optional
	Resources ResourceRequirements
	// Resources resize policy for the container.
	// +featureGate=InPlacePodVerticalScaling
	// +optional
	ResizePolicy []ContainerResizePolicy
	// Restart policy for the container to manage the restart behavior of each
	// container within a pod.
	// This may only be set for init containers. You cannot set this field on
	// ephemeral containers.
	// +featureGate=SidecarContainers
	// +optional
	RestartPolicy *ContainerRestartPolicy
	// Pod volumes to mount into the container's filesystem. Subpath mounts are not allowed for ephemeral containers.
	// +optional
	VolumeMounts []VolumeMount
	// volumeDevices is the list of block devices to be used by the container.
	// +optional
	VolumeDevices []VolumeDevice
	// Probes are not allowed for ephemeral containers.
	// +optional
	LivenessProbe *Probe
	// Probes are not allowed for ephemeral containers.
	// +optional
	ReadinessProbe *Probe
	// Probes are not allowed for ephemeral containers.
	// +optional
	StartupProbe *Probe
	// Lifecycle is not allowed for ephemeral containers.
	// +optional
	Lifecycle *Lifecycle
	// Required.
	// +optional
	TerminationMessagePath string
	// +optional
	TerminationMessagePolicy TerminationMessagePolicy
	// Required: Policy for pulling images for this container
	ImagePullPolicy PullPolicy
	// Optional: SecurityContext defines the security options the ephemeral container should be run with.
	// If set, the fields of SecurityContext override the equivalent fields of PodSecurityContext.
	// +optional
	SecurityContext *SecurityContext

	// Variables for interactive containers, these have very specialized use-cases (e.g. debugging)
	// and shouldn't be used for general purpose containers.
	// +optional
	Stdin bool
	// +optional
	StdinOnce bool
	// +optional
	TTY bool
}

// EphemeralContainerCommon converts to Container. All fields must be kept in sync between
// these two types.
var _ = Container(EphemeralContainerCommon{})

// An EphemeralContainer is a temporary container that you may add to an existing Pod for
// user-initiated activities such as debugging. Ephemeral containers have no resource or
// scheduling guarantees, and they will not be restarted when they exit or when a Pod is
// removed or restarted. The kubelet may evict a Pod if an ephemeral container causes the
// Pod to exceed its resource allocation.
//
// To add an ephemeral container, use the ephemeralcontainers subresource of an existing
// Pod. Ephemeral containers may not be removed or restarted.
type EphemeralContainer struct {
	// Ephemeral containers have all of the fields of Container, plus additional fields
	// specific to ephemeral containers. Fields in common with Container are in the
	// following inlined struct so than an EphemeralContainer may easily be converted
	// to a Container.
	EphemeralContainerCommon

	// If set, the name of the container from PodSpec that this ephemeral container targets.
	// The ephemeral container will be run in the namespaces (IPC, PID, etc) of this container.
	// If not set then the ephemeral container uses the namespaces configured in the Pod spec.
	//
	// The container runtime must implement support for this feature. If the runtime does not
	// support namespace targeting then the result of setting this field is undefined.
	// +optional
	TargetContainerName string
}

// PodStatus represents information about the status of a pod. Status may trail the actual
// state of a system.
type PodStatus struct {
	// +optional
	Phase PodPhase
	// +optional
	Conditions []PodCondition
	// A human readable message indicating details about why the pod is in this state.
	// +optional
	Message string
	// A brief CamelCase message indicating details about why the pod is in this state. e.g. 'Evicted'
	// +optional
	Reason string
	// nominatedNodeName is set when this pod preempts other pods on the node, but it cannot be
	// scheduled right away as preemption victims receive their graceful termination periods.
	// This field does not guarantee that the pod will be scheduled on this node. Scheduler may decide
	// to place the pod elsewhere if other nodes become available sooner. Scheduler may also decide to
	// give the resources on this node to a higher priority pod that is created after preemption.
	// +optional
	NominatedNodeName string

	// HostIP holds the IP address of the host to which the pod is assigned. Empty if the pod has not started yet.
	// A pod can be assigned to a node that has a problem in kubelet which in turns mean that HostIP will
	// not be updated even if there is a node is assigned to pod
	// +optional
	HostIP string

	// HostIPs holds the IP addresses allocated to the host. If this field is specified, the first entry must
	// match the hostIP field. This list is empty if the pod has not started yet.
	// A pod can be assigned to a node that has a problem in kubelet which in turns means that HostIPs will
	// not be updated even if there is a node is assigned to this pod.
	// match the hostIP field. This list is empty if no IPs have been allocated yet.
	// +optional
	HostIPs []HostIP

	// PodIPs holds all of the known IP addresses allocated to the pod. Pods may be assigned AT MOST
	// one value for each of IPv4 and IPv6.
	// +optional
	PodIPs []PodIP

	// Date and time at which the object was acknowledged by the Kubelet.
	// This is before the Kubelet pulled the container image(s) for the pod.
	// +optional
	StartTime *metav1.Time
	// +optional
	QOSClass PodQOSClass

	// Statuses of init containers in this pod. The most recent successful non-restartable
	// init container will have ready = true, the most recently started container will have
	// startTime set.
	// Each init container in the pod should have at most one status in this list,
	// and all statuses should be for containers in the pod.
	// However this is not enforced.
	// If a status for a non-existent container is present in the list, or the list has duplicate names,
	// the behavior of various Kubernetes components is not defined and those statuses might be
	// ignored.
	// More info: https://kubernetes.io/docs/concepts/workloads/pods/pod-lifecycle/#pod-and-container-status
	InitContainerStatuses []ContainerStatus

	// Statuses of containers in this pod.
	// Each container in the pod should have at most one status in this list,
	// and all statuses should be for containers in the pod.
	// However this is not enforced.
	// If a status for a non-existent container is present in the list, or the list has duplicate names,
	// the behavior of various Kubernetes components is not defined and those statuses might be
	// ignored.
	// More info: https://kubernetes.io/docs/concepts/workloads/pods/pod-lifecycle#pod-and-container-status
	// +optional
	ContainerStatuses []ContainerStatus

	// Statuses for any ephemeral containers that have run in this pod.
	// Each ephemeral container in the pod should have at most one status in this list,
	// and all statuses should be for containers in the pod.
	// However this is not enforced.
	// If a status for a non-existent container is present in the list, or the list has duplicate names,
	// the behavior of various Kubernetes components is not defined and those statuses might be
	// ignored.
	// More info: https://kubernetes.io/docs/concepts/workloads/pods/pod-lifecycle#pod-and-container-status
	// +optional
	EphemeralContainerStatuses []ContainerStatus

	// Status of resources resize desired for pod's containers.
	// It is empty if no resources resize is pending.
	// Any changes to container resources will automatically set this to "Proposed"
	// +featureGate=InPlacePodVerticalScaling
	// +optional
	Resize PodResizeStatus

	// Status of resource claims.
	// +featureGate=DynamicResourceAllocation
	// +optional
	ResourceClaimStatuses []PodResourceClaimStatus
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// PodStatusResult is a wrapper for PodStatus returned by kubelet that can be encode/decoded
type PodStatusResult struct {
	metav1.TypeMeta
	// +optional
	metav1.ObjectMeta
	// Status represents the current information about a pod. This data may not be up
	// to date.
	// +optional
	Status PodStatus
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// Pod is a collection of containers, used as either input (create, update) or as output (list, get).
type Pod struct {
	metav1.TypeMeta
	// +optional
	metav1.ObjectMeta

	// Spec defines the behavior of a pod.
	// +optional
	Spec PodSpec

	// Status represents the current information about a pod. This data may not be up
	// to date.
	// +optional
	Status PodStatus
}

// PodTemplateSpec describes the data a pod should have when created from a template
type PodTemplateSpec struct {
	// Metadata of the pods created from this template.
	// +optional
	metav1.ObjectMeta

	// Spec defines the behavior of a pod.
	// +optional
	Spec PodSpec
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// PodTemplate describes a template for creating copies of a predefined pod.
type PodTemplate struct {
	metav1.TypeMeta
	// +optional
	metav1.ObjectMeta

	// Template defines the pods that will be created from this pod template
	// +optional
	Template PodTemplateSpec
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// PodTemplateList is a list of PodTemplates.
type PodTemplateList struct {
	metav1.TypeMeta
	// +optional
	metav1.ListMeta

	Items []PodTemplate
}

// ReplicationControllerSpec is the specification of a replication controller.
// As the internal representation of a replication controller, it may have either
// a TemplateRef or a Template set.
type ReplicationControllerSpec struct {
	// Replicas is the number of desired replicas.
	Replicas int32

	// Minimum number of seconds for which a newly created pod should be ready
	// without any of its container crashing, for it to be considered available.
	// Defaults to 0 (pod will be considered available as soon as it is ready)
	// +optional
	MinReadySeconds int32

	// Selector is a label query over pods that should match the Replicas count.
	Selector map[string]string

	// TemplateRef is a reference to an object that describes the pod that will be created if
	// insufficient replicas are detected. This reference is ignored if a Template is set.
	// Must be set before converting to a versioned API object
	// +optional
	// TemplateRef *ObjectReference

	// Template is the object that describes the pod that will be created if
	// insufficient replicas are detected. Internally, this takes precedence over a
	// TemplateRef.
	// The only allowed template.spec.restartPolicy value is "Always".
	// +optional
	Template *PodTemplateSpec
}

// ReplicationControllerStatus represents the current status of a replication
// controller.
type ReplicationControllerStatus struct {
	// Replicas is the number of actual replicas.
	Replicas int32

	// The number of pods that have labels matching the labels of the pod template of the replication controller.
	// +optional
	FullyLabeledReplicas int32

	// The number of ready replicas for this replication controller.
	// +optional
	ReadyReplicas int32

	// The number of available replicas (ready for at least minReadySeconds) for this replication controller.
	// +optional
	AvailableReplicas int32

	// ObservedGeneration is the most recent generation observed by the controller.
	// +optional
	ObservedGeneration int64

	// Represents the latest available observations of a replication controller's current state.
	// +optional
	Conditions []ReplicationControllerCondition
}

// ReplicationControllerConditionType defines the conditions of a replication controller.
type ReplicationControllerConditionType string

// These are valid conditions of a replication controller.
const (
	// ReplicationControllerReplicaFailure is added in a replication controller when one of its pods
	// fails to be created due to insufficient quota, limit ranges, pod security policy, node selectors,
	// etc. or deleted due to kubelet being down or finalizers are failing.
	ReplicationControllerReplicaFailure ReplicationControllerConditionType = "ReplicaFailure"
)

// ReplicationControllerCondition describes the state of a replication controller at a certain point.
type ReplicationControllerCondition struct {
	// Type of replication controller condition.
	Type ReplicationControllerConditionType
	// Status of the condition, one of True, False, Unknown.
	Status ConditionStatus
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

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// ReplicationController represents the configuration of a replication controller.
type ReplicationController struct {
	metav1.TypeMeta
	// +optional
	metav1.ObjectMeta

	// Spec defines the desired behavior of this replication controller.
	// +optional
	Spec ReplicationControllerSpec

	// Status is the current status of this replication controller. This data may be
	// out of date by some window of time.
	// +optional
	Status ReplicationControllerStatus
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// ReplicationControllerList is a collection of replication controllers.
type ReplicationControllerList struct {
	metav1.TypeMeta
	// +optional
	metav1.ListMeta

	Items []ReplicationController
}

const (
	// ClusterIPNone - do not assign a cluster IP
	// no proxying required and no environment variables should be created for pods
	ClusterIPNone = "None"
)

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// ServiceList holds a list of services.
type ServiceList struct {
	metav1.TypeMeta
	// +optional
	metav1.ListMeta

	Items []Service
}

// ServiceAffinity Type string
type ServiceAffinity string

const (
	// ServiceAffinityClientIP is the Client IP based.
	ServiceAffinityClientIP ServiceAffinity = "ClientIP"

	// ServiceAffinityNone - no session affinity.
	ServiceAffinityNone ServiceAffinity = "None"
)

const (
	// DefaultClientIPServiceAffinitySeconds is the default timeout seconds
	// of Client IP based session affinity - 3 hours.
	DefaultClientIPServiceAffinitySeconds int32 = 10800
	// MaxClientIPServiceAffinitySeconds is the max timeout seconds
	// of Client IP based session affinity - 1 day.
	MaxClientIPServiceAffinitySeconds int32 = 86400
)

// SessionAffinityConfig represents the configurations of session affinity.
type SessionAffinityConfig struct {
	// clientIP contains the configurations of Client IP based session affinity.
	// +optional
	ClientIP *ClientIPConfig
}

// ClientIPConfig represents the configurations of Client IP based session affinity.
type ClientIPConfig struct {
	// timeoutSeconds specifies the seconds of ClientIP type session sticky time.
	// The value must be >0 && <=86400(for 1 day) if ServiceAffinity == "ClientIP".
	// Default value is 10800(for 3 hours).
	// +optional
	TimeoutSeconds *int32
}

// ServiceType string describes ingress methods for a service
type ServiceType string

const (
	// ServiceTypeClusterIP means a service will only be accessible inside the
	// cluster, via the ClusterIP.
	ServiceTypeClusterIP ServiceType = "ClusterIP"

	// ServiceTypeNodePort means a service will be exposed on one port of
	// every node, in addition to 'ClusterIP' type.
	ServiceTypeNodePort ServiceType = "NodePort"

	// ServiceTypeLoadBalancer means a service will be exposed via an
	// external load balancer (if the cloud provider supports it), in addition
	// to 'NodePort' type.
	ServiceTypeLoadBalancer ServiceType = "LoadBalancer"

	// ServiceTypeExternalName means a service consists of only a reference to
	// an external name that kubedns or equivalent will return as a CNAME
	// record, with no exposing or proxying of any pods involved.
	ServiceTypeExternalName ServiceType = "ExternalName"
)

// ServiceInternalTrafficPolicy describes the endpoint-selection policy for
// traffic sent to the ClusterIP.
type ServiceInternalTrafficPolicy string

const (
	// ServiceInternalTrafficPolicyCluster routes traffic to all endpoints.
	ServiceInternalTrafficPolicyCluster ServiceInternalTrafficPolicy = "Cluster"

	// ServiceInternalTrafficPolicyLocal routes traffic only to endpoints on the same
	// node as the traffic was received on (dropping the traffic if there are no
	// local endpoints).
	ServiceInternalTrafficPolicyLocal ServiceInternalTrafficPolicy = "Local"
)

// ServiceExternalTrafficPolicy describes the endpoint-selection policy for
// traffic to external service entrypoints (NodePorts, ExternalIPs, and
// LoadBalancer IPs).
type ServiceExternalTrafficPolicy string

const (
	// ServiceExternalTrafficPolicyCluster routes traffic to all endpoints.
	ServiceExternalTrafficPolicyCluster ServiceExternalTrafficPolicy = "Cluster"

	// ServiceExternalTrafficPolicyLocal preserves the source IP of the traffic by
	// routing only to endpoints on the same node as the traffic was received on
	// (dropping the traffic if there are no local endpoints).
	ServiceExternalTrafficPolicyLocal ServiceExternalTrafficPolicy = "Local"
)

// These are valid values for the TrafficDistribution field of a Service.
const (
	// Indicates a preference for routing traffic to endpoints that are
	// topologically proximate to the client. The interpretation of "topologically
	// proximate" may vary across implementations and could encompass endpoints
	// within the same node, rack, zone, or even region. Setting this value gives
	// implementations permission to make different tradeoffs, e.g. optimizing for
	// proximity rather than equal distribution of load. Users should not set this
	// value if such tradeoffs are not acceptable.
	ServiceTrafficDistributionPreferClose = "PreferClose"
)

// These are the valid conditions of a service.
const (
	// LoadBalancerPortsError represents the condition of the requested ports
	// on the cloud load balancer instance.
	LoadBalancerPortsError = "LoadBalancerPortsError"
)

// ServiceStatus represents the current status of a service
type ServiceStatus struct {
	// LoadBalancer contains the current status of the load-balancer,
	// if one is present.
	// +optional
	LoadBalancer LoadBalancerStatus

	// Current service condition
	// +optional
	Conditions []metav1.Condition
}

// LoadBalancerStatus represents the status of a load-balancer
type LoadBalancerStatus struct {
	// Ingress is a list containing ingress points for the load-balancer;
	// traffic intended for the service should be sent to these ingress points.
	// +optional
	Ingress []LoadBalancerIngress
}

// LoadBalancerIngress represents the status of a load-balancer ingress point:
// traffic intended for the service should be sent to an ingress point.
type LoadBalancerIngress struct {
	// IP is set for load-balancer ingress points that are IP based
	// (typically GCE or OpenStack load-balancers)
	// +optional
	IP string

	// Hostname is set for load-balancer ingress points that are DNS based
	// (typically AWS load-balancers)
	// +optional
	Hostname string

	// IPMode specifies how the load-balancer IP behaves, and may only be specified when the ip field is specified.
	// Setting this to "VIP" indicates that traffic is delivered to the node with
	// the destination set to the load-balancer's IP and port.
	// Setting this to "Proxy" indicates that traffic is delivered to the node or pod with
	// the destination set to the node's IP and node port or the pod's IP and port.
	// Service implementations may use this information to adjust traffic routing.
	// +optional
	IPMode *LoadBalancerIPMode

	// Ports is a list of records of service ports
	// If used, every port defined in the service should have an entry in it
	// +optional
	Ports []PortStatus
}

// IPFamily represents the IP Family (IPv4 or IPv6). This type is used
// to express the family of an IP expressed by a type (e.g. service.spec.ipFamilies).
type IPFamily string

const (
	// IPv4Protocol indicates that this IP is IPv4 protocol
	IPv4Protocol IPFamily = "IPv4"
	// IPv6Protocol indicates that this IP is IPv6 protocol
	IPv6Protocol IPFamily = "IPv6"
)

// IPFamilyPolicy represents the dual-stack-ness requested or required by a Service
type IPFamilyPolicy string

const (
	// IPFamilyPolicySingleStack indicates that this service is required to have a single IPFamily.
	// The IPFamily assigned is based on the default IPFamily used by the cluster
	// or as identified by service.spec.ipFamilies field
	IPFamilyPolicySingleStack IPFamilyPolicy = "SingleStack"
	// IPFamilyPolicyPreferDualStack indicates that this service prefers dual-stack when
	// the cluster is configured for dual-stack. If the cluster is not configured
	// for dual-stack the service will be assigned a single IPFamily. If the IPFamily is not
	// set in service.spec.ipFamilies then the service will be assigned the default IPFamily
	// configured on the cluster
	IPFamilyPolicyPreferDualStack IPFamilyPolicy = "PreferDualStack"
	// IPFamilyPolicyRequireDualStack indicates that this service requires dual-stack. Using
	// IPFamilyPolicyRequireDualStack on a single stack cluster will result in validation errors. The
	// IPFamilies (and their order) assigned  to this service is based on service.spec.ipFamilies. If
	// service.spec.ipFamilies was not provided then it will be assigned according to how they are
	// configured on the cluster. If service.spec.ipFamilies has only one entry then the alternative
	// IPFamily will be added by apiserver
	IPFamilyPolicyRequireDualStack IPFamilyPolicy = "RequireDualStack"
)

// ServiceSpec describes the attributes that a user creates on a service
type ServiceSpec struct {
	// Type determines how the Service is exposed. Defaults to ClusterIP. Valid
	// options are ExternalName, ClusterIP, NodePort, and LoadBalancer.
	// "ExternalName" maps to the specified externalName.
	// "ClusterIP" allocates a cluster-internal IP address for load-balancing to
	// endpoints. Endpoints are determined by the selector or if that is not
	// specified, by manual construction of an Endpoints object. If clusterIP is
	// "None", no virtual IP is allocated and the endpoints are published as a
	// set of endpoints rather than a stable IP.
	// "NodePort" builds on ClusterIP and allocates a port on every node which
	// routes to the clusterIP.
	// "LoadBalancer" builds on NodePort and creates an
	// external load-balancer (if supported in the current cloud) which routes
	// to the clusterIP.
	// More info: https://kubernetes.io/docs/concepts/services-networking/service/
	// +optional
	Type ServiceType

	// Required: The list of ports that are exposed by this service.
	Ports []ServicePort

	// Route service traffic to pods with label keys and values matching this
	// selector. If empty or not present, the service is assumed to have an
	// external process managing its endpoints, which Kubernetes will not
	// modify. Only applies to types ClusterIP, NodePort, and LoadBalancer.
	// Ignored if type is ExternalName.
	// More info: https://kubernetes.io/docs/concepts/services-networking/service/
	Selector map[string]string

	// ClusterIP is the IP address of the service and is usually assigned
	// randomly by the master. If an address is specified manually and is not in
	// use by others, it will be allocated to the service; otherwise, creation
	// of the service will fail. This field can not be changed through updates.
	// Valid values are "None", empty string (""), or a valid IP address. "None"
	// can be specified for headless services when proxying is not required.
	// Only applies to types ClusterIP, NodePort, and LoadBalancer. Ignored if
	// type is ExternalName.
	// More info: https://kubernetes.io/docs/concepts/services-networking/service/#virtual-ips-and-service-proxies
	// +optional
	ClusterIP string

	// ClusterIPs identifies all the ClusterIPs assigned to this
	// service. ClusterIPs are assigned or reserved based on the values of
	// service.spec.ipFamilies. A maximum of two entries (dual-stack IPs) are
	// allowed in ClusterIPs. The IPFamily of each ClusterIP must match
	// values provided in service.spec.ipFamilies. Clients using ClusterIPs must
	// keep it in sync with ClusterIP (if provided) by having ClusterIP matching
	// first element of ClusterIPs.
	// +optional
	ClusterIPs []string

	// IPFamilies identifies all the IPFamilies assigned for this Service. If a value
	// was not provided for IPFamilies it will be defaulted based on the cluster
	// configuration and the value of service.spec.ipFamilyPolicy. A maximum of two
	// values (dual-stack IPFamilies) are allowed in IPFamilies. IPFamilies field is
	// conditionally mutable: it allows for adding or removing a secondary IPFamily,
	// but it does not allow changing the primary IPFamily of the service.
	// +optional
	IPFamilies []IPFamily

	// IPFamilyPolicy represents the dual-stack-ness requested or required by this
	// Service. If there is no value provided, then this Service will be considered
	// SingleStack (single IPFamily). Services can be SingleStack (single IPFamily),
	// PreferDualStack (two dual-stack IPFamilies on dual-stack clusters or single
	// IPFamily on single-stack clusters), or RequireDualStack (two dual-stack IPFamilies
	// on dual-stack configured clusters, otherwise fail). The IPFamilies and ClusterIPs assigned
	// to this service can be controlled by service.spec.ipFamilies and service.spec.clusterIPs
	// respectively.
	// +optional
	IPFamilyPolicy *IPFamilyPolicy

	// ExternalName is the external reference that kubedns or equivalent will
	// return as a CNAME record for this service. No proxying will be involved.
	// Must be a valid RFC-1123 hostname (https://tools.ietf.org/html/rfc1123)
	// and requires Type to be ExternalName.
	ExternalName string

	// ExternalIPs are used by external load balancers, or can be set by
	// users to handle external traffic that arrives at a node.
	// +optional
	ExternalIPs []string

	// Only applies to Service Type: LoadBalancer
	// LoadBalancer will get created with the IP specified in this field.
	// This feature depends on whether the underlying cloud-provider supports specifying
	// the loadBalancerIP when a load balancer is created.
	// This field will be ignored if the cloud-provider does not support the feature.
	// Deprecated: This field was under-specified and its meaning varies across implementations.
	// Using it is non-portable and it may not support dual-stack.
	// Users are encouraged to use implementation-specific annotations when available.
	// +optional
	LoadBalancerIP string

	// Optional: Supports "ClientIP" and "None".  Used to maintain session affinity.
	// +optional
	SessionAffinity ServiceAffinity

	// sessionAffinityConfig contains the configurations of session affinity.
	// +optional
	SessionAffinityConfig *SessionAffinityConfig

	// Optional: If specified and supported by the platform, this will restrict traffic through the cloud-provider
	// load-balancer will be restricted to the specified client IPs. This field will be ignored if the
	// cloud-provider does not support the feature."
	// +optional
	LoadBalancerSourceRanges []string

	// externalTrafficPolicy describes how nodes distribute service traffic they
	// receive on one of the Service's "externally-facing" addresses (NodePorts,
	// ExternalIPs, and LoadBalancer IPs). If set to "Local", the proxy will configure
	// the service in a way that assumes that external load balancers will take care
	// of balancing the service traffic between nodes, and so each node will deliver
	// traffic only to the node-local endpoints of the service, without masquerading
	// the client source IP. (Traffic mistakenly sent to a node with no endpoints will
	// be dropped.) The default value, "Cluster", uses the standard behavior of
	// routing to all endpoints evenly (possibly modified by topology and other
	// features). Note that traffic sent to an External IP or LoadBalancer IP from
	// within the cluster will always get "Cluster" semantics, but clients sending to
	// a NodePort from within the cluster may need to take traffic policy into account
	// when picking a node.
	// +optional
	ExternalTrafficPolicy ServiceExternalTrafficPolicy

	// healthCheckNodePort specifies the healthcheck nodePort for the service.
	// If not specified, HealthCheckNodePort is created by the service api
	// backend with the allocated nodePort. Will use user-specified nodePort value
	// if specified by the client. Only effects when Type is set to LoadBalancer
	// and ExternalTrafficPolicy is set to Local.
	// +optional
	HealthCheckNodePort int32

	// publishNotReadyAddresses indicates that any agent which deals with endpoints for this
	// Service should disregard any indications of ready/not-ready.
	// The primary use case for setting this field is for a StatefulSet's Headless Service to
	// propagate SRV DNS records for its Pods for the purpose of peer discovery.
	// The Kubernetes controllers that generate Endpoints and EndpointSlice resources for
	// Services interpret this to mean that all endpoints are considered "ready" even if the
	// Pods themselves are not. Agents which consume only Kubernetes generated endpoints
	// through the Endpoints or EndpointSlice resources can safely assume this behavior.
	// +optional
	PublishNotReadyAddresses bool

	// allocateLoadBalancerNodePorts defines if NodePorts will be automatically
	// allocated for services with type LoadBalancer.  Default is "true". It
	// may be set to "false" if the cluster load-balancer does not rely on
	// NodePorts.  If the caller requests specific NodePorts (by specifying a
	// value), those requests will be respected, regardless of this field.
	// This field may only be set for services with type LoadBalancer and will
	// be cleared if the type is changed to any other type.
	// +optional
	AllocateLoadBalancerNodePorts *bool

	// loadBalancerClass is the class of the load balancer implementation this Service belongs to.
	// If specified, the value of this field must be a label-style identifier, with an optional prefix,
	// e.g. "internal-vip" or "example.com/internal-vip". Unprefixed names are reserved for end-users.
	// This field can only be set when the Service type is 'LoadBalancer'. If not set, the default load
	// balancer implementation is used, today this is typically done through the cloud provider integration,
	// but should apply for any default implementation. If set, it is assumed that a load balancer
	// implementation is watching for Services with a matching class. Any default load balancer
	// implementation (e.g. cloud providers) should ignore Services that set this field.
	// This field can only be set when creating or updating a Service to type 'LoadBalancer'.
	// Once set, it can not be changed. This field will be wiped when a service is updated to a non 'LoadBalancer' type.
	// +optional
	LoadBalancerClass *string

	// InternalTrafficPolicy describes how nodes distribute service traffic they
	// receive on the ClusterIP. If set to "Local", the proxy will assume that pods
	// only want to talk to endpoints of the service on the same node as the pod,
	// dropping the traffic if there are no local endpoints. The default value,
	// "Cluster", uses the standard behavior of routing to all endpoints evenly
	// (possibly modified by topology and other features).
	// +optional
	InternalTrafficPolicy *ServiceInternalTrafficPolicy

	// TrafficDistribution offers a way to express preferences for how traffic is
	// distributed to Service endpoints. Implementations can use this field as a
	// hint, but are not required to guarantee strict adherence. If the field is
	// not set, the implementation will apply its default routing strategy. If set
	// to "PreferClose", implementations should prioritize endpoints that are
	// topologically close (e.g., same zone).
	// +optional
	TrafficDistribution *string
}

// ServicePort represents the port on which the service is exposed
type ServicePort struct {
	// Optional if only one ServicePort is defined on this service: The
	// name of this port within the service.  This must be a DNS_LABEL.
	// All ports within a ServiceSpec must have unique names.  This maps to
	// the 'Name' field in EndpointPort objects.
	Name string

	// The IP protocol for this port.  Supports "TCP", "UDP", and "SCTP".
	Protocol Protocol

	// The application protocol for this port.
	// This is used as a hint for implementations to offer richer behavior for protocols that they understand.
	// This field follows standard Kubernetes label syntax.
	// Valid values are either:
	//
	// * Un-prefixed protocol names - reserved for IANA standard service names (as per
	// RFC-6335 and https://www.iana.org/assignments/service-names).
	//
	// * Kubernetes-defined prefixed names:
	//   * 'kubernetes.io/h2c' - HTTP/2 prior knowledge over cleartext as described in https://www.rfc-editor.org/rfc/rfc9113.html#name-starting-http-2-with-prior-
	//   * 'kubernetes.io/ws'  - WebSocket over cleartext as described in https://www.rfc-editor.org/rfc/rfc6455
	//   * 'kubernetes.io/wss' - WebSocket over TLS as described in https://www.rfc-editor.org/rfc/rfc6455
	//
	// * Other protocols should use implementation-defined prefixed names such as
	// mycompany.com/my-custom-protocol.
	// +optional
	AppProtocol *string

	// The port that will be exposed on the service.
	Port int32

	// Optional: The target port on pods selected by this service.  If this
	// is a string, it will be looked up as a named port in the target
	// Pod's container ports.  If this is not specified, the value
	// of the 'port' field is used (an identity map).
	// This field is ignored for services with clusterIP=None, and should be
	// omitted or set equal to the 'port' field.
	TargetPort intstr.IntOrString

	// The port on each node on which this service is exposed.
	// Default is to auto-allocate a port if the ServiceType of this Service requires one.
	NodePort int32
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// Service is a named abstraction of software service (for example, mysql) consisting of local port
// (for example 3306) that the proxy listens on, and the selector that determines which pods
// will answer requests sent through the proxy.
type Service struct {
	metav1.TypeMeta
	// +optional
	metav1.ObjectMeta

	// Spec defines the behavior of a service.
	// +optional
	Spec ServiceSpec

	// Status represents the current status of a service.
	// +optional
	Status ServiceStatus
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// ServiceAccount binds together:
// * a name, understood by users, and perhaps by peripheral systems, for an identity
// * a principal that can be authenticated and authorized
// * a set of secrets
type ServiceAccount struct {
	metav1.TypeMeta
	// +optional
	metav1.ObjectMeta

	// Secrets is a list of the secrets in the same namespace that pods running using this ServiceAccount are allowed to use.
	// Pods are only limited to this list if this service account has a "kubernetes.io/enforce-mountable-secrets" annotation set to "true".
	// The "kubernetes.io/enforce-mountable-secrets" annotation is deprecated since v1.32.
	// Prefer separate namespaces to isolate access to mounted secrets.
	// This field should not be used to find auto-generated service account token secrets for use outside of pods.
	// Instead, tokens can be requested directly using the TokenRequest API, or service account token secrets can be manually created.
	Secrets []ObjectReference

	// ImagePullSecrets is a list of references to secrets in the same namespace to use for pulling any images
	// in pods that reference this ServiceAccount.  ImagePullSecrets are distinct from Secrets because Secrets
	// can be mounted in the pod, but ImagePullSecrets are only accessed by the kubelet.
	// +optional
	ImagePullSecrets []LocalObjectReference

	// AutomountServiceAccountToken indicates whether pods running as this service account should have an API token automatically mounted.
	// Can be overridden at the pod level.
	// +optional
	AutomountServiceAccountToken *bool
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// ServiceAccountList is a list of ServiceAccount objects
type ServiceAccountList struct {
	metav1.TypeMeta
	// +optional
	metav1.ListMeta

	Items []ServiceAccount
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// Endpoints is a collection of endpoints that implement the actual service.  Example:
//
//	 Name: "mysvc",
//	 Subsets: [
//	   {
//	     Addresses: [{"ip": "10.10.1.1"}, {"ip": "10.10.2.2"}],
//	     Ports: [{"name": "a", "port": 8675}, {"name": "b", "port": 309}]
//	   },
//	   {
//	     Addresses: [{"ip": "10.10.3.3"}],
//	     Ports: [{"name": "a", "port": 93}, {"name": "b", "port": 76}]
//	   },
//	]
type Endpoints struct {
	metav1.TypeMeta
	// +optional
	metav1.ObjectMeta

	// The set of all endpoints is the union of all subsets.
	Subsets []EndpointSubset
}

// EndpointSubset is a group of addresses with a common set of ports.  The
// expanded set of endpoints is the Cartesian product of Addresses x Ports.
// For example, given:
//
//	{
//	  Addresses: [{"ip": "10.10.1.1"}, {"ip": "10.10.2.2"}],
//	  Ports:     [{"name": "a", "port": 8675}, {"name": "b", "port": 309}]
//	}
//
// The resulting set of endpoints can be viewed as:
//
//	a: [ 10.10.1.1:8675, 10.10.2.2:8675 ],
//	b: [ 10.10.1.1:309, 10.10.2.2:309 ]
type EndpointSubset struct {
	Addresses         []EndpointAddress
	NotReadyAddresses []EndpointAddress
	Ports             []EndpointPort
}

// EndpointAddress is a tuple that describes single IP address.
type EndpointAddress struct {
	// The IP of this endpoint.
	// May not be loopback (127.0.0.0/8 or ::1), link-local (169.254.0.0/16 or fe80::/10),
	// or link-local multicast (224.0.0.0/24 or ff02::/16).
	IP string
	// Optional: Hostname of this endpoint
	// Meant to be used by DNS servers etc.
	// +optional
	Hostname string
	// Optional: Node hosting this endpoint. This can be used to determine endpoints local to a node.
	// +optional
	NodeName *string
	// Optional: The kubernetes object related to the entry point.
	TargetRef *ObjectReference
}

// EndpointPort is a tuple that describes a single port.
type EndpointPort struct {
	// The name of this port (corresponds to ServicePort.Name).  Optional
	// if only one port is defined.  Must be a DNS_LABEL.
	Name string

	// The port number.
	Port int32

	// The IP protocol for this port.
	Protocol Protocol

	// The application protocol for this port.
	// This is used as a hint for implementations to offer richer behavior for protocols that they understand.
	// This field follows standard Kubernetes label syntax.
	// Valid values are either:
	//
	// * Un-prefixed protocol names - reserved for IANA standard service names (as per
	// RFC-6335 and https://www.iana.org/assignments/service-names).
	//
	// * Kubernetes-defined prefixed names:
	//   * 'kubernetes.io/h2c' - HTTP/2 prior knowledge over cleartext as described in https://www.rfc-editor.org/rfc/rfc9113.html#name-starting-http-2-with-prior-
	//   * 'kubernetes.io/ws'  - WebSocket over cleartext as described in https://www.rfc-editor.org/rfc/rfc6455
	//   * 'kubernetes.io/wss' - WebSocket over TLS as described in https://www.rfc-editor.org/rfc/rfc6455
	//
	// * Other protocols should use implementation-defined prefixed names such as
	// mycompany.com/my-custom-protocol.
	// +optional
	AppProtocol *string
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// EndpointsList is a list of endpoints.
type EndpointsList struct {
	metav1.TypeMeta
	// +optional
	metav1.ListMeta

	Items []Endpoints
}

// NodeSpec describes the attributes that a node is created with.
type NodeSpec struct {
	// PodCIDRs represents the IP ranges assigned to the node for usage by Pods on that node. It may
	// contain AT MOST one value for each of IPv4 and IPv6.
	// Note: assigning IP ranges to nodes might need to be revisited when we support migratable IPs.
	// +optional
	PodCIDRs []string

	// ID of the node assigned by the cloud provider
	// Note: format is "<ProviderName>://<ProviderSpecificNodeID>"
	// +optional
	ProviderID string

	// Unschedulable controls node schedulability of new pods. By default node is schedulable.
	// +optional
	Unschedulable bool

	// If specified, the node's taints.
	// +optional
	Taints []Taint

	// Deprecated: Previously used to specify the source of the node's configuration for the DynamicKubeletConfig feature. This feature is removed.
	// +optional
	ConfigSource *NodeConfigSource

	// Deprecated. Not all kubelets will set this field. Remove field after 1.13.
	// see: https://issues.k8s.io/61966
	// +optional
	DoNotUseExternalID string
}

// Deprecated: NodeConfigSource specifies a source of node configuration. Exactly one subfield must be non-nil.
type NodeConfigSource struct {
	ConfigMap *ConfigMapNodeConfigSource
}

// Deprecated: ConfigMapNodeConfigSource represents the config map of a node
type ConfigMapNodeConfigSource struct {
	// Namespace is the metadata.namespace of the referenced ConfigMap.
	// This field is required in all cases.
	Namespace string

	// Name is the metadata.name of the referenced ConfigMap.
	// This field is required in all cases.
	Name string

	// UID is the metadata.UID of the referenced ConfigMap.
	// This field is forbidden in Node.Spec, and required in Node.Status.
	// +optional
	UID types.UID

	// ResourceVersion is the metadata.ResourceVersion of the referenced ConfigMap.
	// This field is forbidden in Node.Spec, and required in Node.Status.
	// +optional
	ResourceVersion string

	// KubeletConfigKey declares which key of the referenced ConfigMap corresponds to the KubeletConfiguration structure
	// This field is required in all cases.
	KubeletConfigKey string
}

// DaemonEndpoint contains information about a single Daemon endpoint.
type DaemonEndpoint struct {
	/*
		The port tag was not properly in quotes in earlier releases, so it must be
		uppercase for backwards compatibility (since it was falling back to var name of
		'Port').
	*/

	// Port number of the given endpoint.
	Port int32
}

// NodeDaemonEndpoints lists ports opened by daemons running on the Node.
type NodeDaemonEndpoints struct {
	// Endpoint on which Kubelet is listening.
	// +optional
	KubeletEndpoint DaemonEndpoint
}

// NodeRuntimeHandlerFeatures is a set of features implemented by the runtime handler.
type NodeRuntimeHandlerFeatures struct {
	// RecursiveReadOnlyMounts is set to true if the runtime handler supports RecursiveReadOnlyMounts.
	// +featureGate=RecursiveReadOnlyMounts
	// +optional
	RecursiveReadOnlyMounts *bool
	// UserNamespaces is set to true if the runtime handler supports UserNamespaces, including for volumes.
	// +featureGate=UserNamespacesSupport
	// +optional
	UserNamespaces *bool
}

// NodeRuntimeHandler is a set of runtime handler information.
type NodeRuntimeHandler struct {
	// Runtime handler name.
	// Empty for the default runtime handler.
	// +optional
	Name string
	// Supported features.
	// +optional
	Features *NodeRuntimeHandlerFeatures
}

// NodeFeatures describes the set of features implemented by the CRI implementation.
// The features contained in the NodeFeatures should depend only on the cri implementation
// independent of runtime handlers.
type NodeFeatures struct {
	// SupplementalGroupsPolicy is set to true if the runtime supports SupplementalGroupsPolicy and ContainerUser.
	// +optional
	SupplementalGroupsPolicy *bool
}

// NodeSystemInfo is a set of ids/uuids to uniquely identify the node.
type NodeSystemInfo struct {
	// MachineID reported by the node. For unique machine identification
	// in the cluster this field is preferred. Learn more from man(5)
	// machine-id: http://man7.org/linux/man-pages/man5/machine-id.5.html
	MachineID string
	// SystemUUID reported by the node. For unique machine identification
	// MachineID is preferred. This field is specific to Red Hat hosts
	// https://access.redhat.com/documentation/en-us/red_hat_subscription_management/1/html/rhsm/uuid
	SystemUUID string
	// Boot ID reported by the node.
	BootID string
	// Kernel Version reported by the node.
	KernelVersion string
	// OS Image reported by the node.
	OSImage string
	// ContainerRuntime Version reported by the node.
	ContainerRuntimeVersion string
	// Kubelet Version reported by the node.
	KubeletVersion string
	// Deprecated: KubeProxy Version reported by the node.
	KubeProxyVersion string
	// The Operating System reported by the node
	OperatingSystem string
	// The Architecture reported by the node
	Architecture string
	// Swap Info reported by the node.
	SwapInfo *SwapInfo
}

// SwapInfo represents swap memory information.
type SwapInfo struct {
	// Total amount of swap memory in bytes.
	// +optional
	Capacity *int64
}

// NodeConfigStatus describes the status of the config assigned by Node.Spec.ConfigSource.
type NodeConfigStatus struct {
	// Assigned reports the checkpointed config the node will try to use.
	// When Node.Spec.ConfigSource is updated, the node checkpoints the associated
	// config payload to local disk, along with a record indicating intended
	// config. The node refers to this record to choose its config checkpoint, and
	// reports this record in Assigned. Assigned only updates in the status after
	// the record has been checkpointed to disk. When the Kubelet is restarted,
	// it tries to make the Assigned config the Active config by loading and
	// validating the checkpointed payload identified by Assigned.
	// +optional
	Assigned *NodeConfigSource
	// Active reports the checkpointed config the node is actively using.
	// Active will represent either the current version of the Assigned config,
	// or the current LastKnownGood config, depending on whether attempting to use the
	// Assigned config results in an error.
	// +optional
	Active *NodeConfigSource
	// LastKnownGood reports the checkpointed config the node will fall back to
	// when it encounters an error attempting to use the Assigned config.
	// The Assigned config becomes the LastKnownGood config when the node determines
	// that the Assigned config is stable and correct.
	// This is currently implemented as a 10-minute soak period starting when the local
	// record of Assigned config is updated. If the Assigned config is Active at the end
	// of this period, it becomes the LastKnownGood. Note that if Spec.ConfigSource is
	// reset to nil (use local defaults), the LastKnownGood is also immediately reset to nil,
	// because the local default config is always assumed good.
	// You should not make assumptions about the node's method of determining config stability
	// and correctness, as this may change or become configurable in the future.
	// +optional
	LastKnownGood *NodeConfigSource
	// Error describes any problems reconciling the Spec.ConfigSource to the Active config.
	// Errors may occur, for example, attempting to checkpoint Spec.ConfigSource to the local Assigned
	// record, attempting to checkpoint the payload associated with Spec.ConfigSource, attempting
	// to load or validate the Assigned config, etc.
	// Errors may occur at different points while syncing config. Earlier errors (e.g. download or
	// checkpointing errors) will not result in a rollback to LastKnownGood, and may resolve across
	// Kubelet retries. Later errors (e.g. loading or validating a checkpointed config) will result in
	// a rollback to LastKnownGood. In the latter case, it is usually possible to resolve the error
	// by fixing the config assigned in Spec.ConfigSource.
	// You can find additional information for debugging by searching the error message in the Kubelet log.
	// Error is a human-readable description of the error state; machines can check whether or not Error
	// is empty, but should not rely on the stability of the Error text across Kubelet versions.
	// +optional
	Error string
}

// NodeStatus is information about the current status of a node.
type NodeStatus struct {
	// Capacity represents the total resources of a node.
	// +optional
	Capacity ResourceList
	// Allocatable represents the resources of a node that are available for scheduling.
	// +optional
	Allocatable ResourceList
	// NodePhase is the current lifecycle phase of the node.
	// +optional
	Phase NodePhase
	// Conditions is an array of current node conditions.
	// +optional
	Conditions []NodeCondition
	// Queried from cloud provider, if available.
	// +optional
	Addresses []NodeAddress
	// Endpoints of daemons running on the Node.
	// +optional
	DaemonEndpoints NodeDaemonEndpoints
	// Set of ids/uuids to uniquely identify the node.
	// +optional
	NodeInfo NodeSystemInfo
	// List of container images on this node
	// +optional
	Images []ContainerImage
	// List of attachable volumes in use (mounted) by the node.
	// +optional
	VolumesInUse []UniqueVolumeName
	// List of volumes that are attached to the node.
	// +optional
	VolumesAttached []AttachedVolume
	// Status of the config assigned to the node via the dynamic Kubelet config feature.
	// +optional
	Config *NodeConfigStatus
	// The available runtime handlers.
	// +featureGate=RecursiveReadOnlyMounts
	// +featureGate=UserNamespacesSupport
	// +optional
	RuntimeHandlers []NodeRuntimeHandler
	// Features describes the set of features implemented by the CRI implementation.
	// +featureGate=SupplementalGroupsPolicy
	// +optional
	Features *NodeFeatures
}

// UniqueVolumeName defines the name of attached volume
type UniqueVolumeName string

// AttachedVolume describes a volume attached to a node
type AttachedVolume struct {
	// Name of the attached volume
	Name UniqueVolumeName

	// DevicePath represents the device path where the volume should be available
	DevicePath string
}

// AvoidPods describes pods that should avoid this node. This is the value for a
// Node annotation with key scheduler.alpha.kubernetes.io/preferAvoidPods and
// will eventually become a field of NodeStatus.
type AvoidPods struct {
	// Bounded-sized list of signatures of pods that should avoid this node, sorted
	// in timestamp order from oldest to newest. Size of the slice is unspecified.
	// +optional
	PreferAvoidPods []PreferAvoidPodsEntry
}

// PreferAvoidPodsEntry describes a class of pods that should avoid this node.
type PreferAvoidPodsEntry struct {
	// The class of pods.
	PodSignature PodSignature
	// Time at which this entry was added to the list.
	// +optional
	EvictionTime metav1.Time
	// (brief) reason why this entry was added to the list.
	// +optional
	Reason string
	// Human readable message indicating why this entry was added to the list.
	// +optional
	Message string
}

// PodSignature describes the class of pods that should avoid this node.
// Exactly one field should be set.
type PodSignature struct {
	// Reference to controller whose pods should avoid this node.
	// +optional
	PodController *metav1.OwnerReference
}

// ContainerImage describe a container image
type ContainerImage struct {
	// Names by which this image is known.
	// +optional
	Names []string
	// The size of the image in bytes.
	// +optional
	SizeBytes int64
}

// NodePhase defines the phase in which a node is in
type NodePhase string

// These are the valid phases of node.
const (
	// NodePending means the node has been created/added by the system, but not configured.
	NodePending NodePhase = "Pending"
	// NodeRunning means the node has been configured and has Kubernetes components running.
	NodeRunning NodePhase = "Running"
	// NodeTerminated means the node has been removed from the cluster.
	NodeTerminated NodePhase = "Terminated"
)

// NodeConditionType defines node's condition
type NodeConditionType string

// These are valid but not exhaustive conditions of node. A cloud provider may set a condition not listed here.
// Relevant events contain "NodeReady", "NodeNotReady", "NodeSchedulable", and "NodeNotSchedulable".
const (
	// NodeReady means kubelet is healthy and ready to accept pods.
	NodeReady NodeConditionType = "Ready"
	// NodeMemoryPressure means the kubelet is under pressure due to insufficient available memory.
	NodeMemoryPressure NodeConditionType = "MemoryPressure"
	// NodeDiskPressure means the kubelet is under pressure due to insufficient available disk.
	NodeDiskPressure NodeConditionType = "DiskPressure"
	// NodeNetworkUnavailable means that network for the node is not correctly configured.
	NodeNetworkUnavailable NodeConditionType = "NetworkUnavailable"
)

// NodeCondition represents the node's condition
type NodeCondition struct {
	Type   NodeConditionType
	Status ConditionStatus
	// +optional
	LastHeartbeatTime metav1.Time
	// +optional
	LastTransitionTime metav1.Time
	// +optional
	Reason string
	// +optional
	Message string
}

// NodeAddressType defines the node's address type
type NodeAddressType string

// These are valid values of node address type
const (
	// NodeHostName identifies a name of the node. Although every node can be assumed
	// to have a NodeAddress of this type, its exact syntax and semantics are not
	// defined, and are not consistent between different clusters.
	NodeHostName NodeAddressType = "Hostname"

	// NodeInternalIP identifies an IP address which is assigned to one of the node's
	// network interfaces. Every node should have at least one address of this type.
	//
	// An internal IP is normally expected to be reachable from every other node, but
	// may not be visible to hosts outside the cluster. By default it is assumed that
	// kube-apiserver can reach node internal IPs, though it is possible to configure
	// clusters where this is not the case.
	//
	// NodeInternalIP is the default type of node IP, and does not necessarily imply
	// that the IP is ONLY reachable internally. If a node has multiple internal IPs,
	// no specific semantics are assigned to the additional IPs.
	NodeInternalIP NodeAddressType = "InternalIP"

	// NodeExternalIP identifies an IP address which is, in some way, intended to be
	// more usable from outside the cluster then an internal IP, though no specific
	// semantics are defined. It may be a globally routable IP, though it is not
	// required to be.
	//
	// External IPs may be assigned directly to an interface on the node, like a
	// NodeInternalIP, or alternatively, packets sent to the external IP may be NAT'ed
	// to an internal node IP rather than being delivered directly (making the IP less
	// efficient for node-to-node traffic than a NodeInternalIP).
	NodeExternalIP NodeAddressType = "ExternalIP"

	// NodeInternalDNS identifies a DNS name which resolves to an IP address which has
	// the characteristics of a NodeInternalIP. The IP it resolves to may or may not
	// be a listed NodeInternalIP address.
	NodeInternalDNS NodeAddressType = "InternalDNS"

	// NodeExternalDNS identifies a DNS name which resolves to an IP address which has
	// the characteristics of a NodeExternalIP. The IP it resolves to may or may not
	// be a listed NodeExternalIP address.
	NodeExternalDNS NodeAddressType = "ExternalDNS"
)

// NodeAddress represents node's address
type NodeAddress struct {
	Type    NodeAddressType
	Address string
}

// ResourceName is the name identifying various resources in a ResourceList.
type ResourceName string

// Resource names must be not more than 63 characters, consisting of upper- or lower-case alphanumeric characters,
// with the -, _, and . characters allowed anywhere, except the first or last character.
// The default convention, matching that for annotations, is to use lower-case names, with dashes, rather than
// camel case, separating compound words.
// Fully-qualified resource typenames are constructed from a DNS-style subdomain, followed by a slash `/` and a name.
const (
	// CPU, in cores. (500m = .5 cores)
	ResourceCPU ResourceName = "cpu"
	// Memory, in bytes. (500Gi = 500GiB = 500 * 1024 * 1024 * 1024)
	ResourceMemory ResourceName = "memory"
	// Volume size, in bytes (e,g. 5Gi = 5GiB = 5 * 1024 * 1024 * 1024)
	ResourceStorage ResourceName = "storage"
	// Local ephemeral storage, in bytes. (500Gi = 500GiB = 500 * 1024 * 1024 * 1024)
	ResourceEphemeralStorage ResourceName = "ephemeral-storage"
)

const (
	// ResourceDefaultNamespacePrefix is the default namespace prefix.
	ResourceDefaultNamespacePrefix = "kubernetes.io/"
	// ResourceHugePagesPrefix is the name prefix for huge page resources (alpha).
	ResourceHugePagesPrefix = "hugepages-"
	// ResourceAttachableVolumesPrefix is the name prefix for storage resource limits
	ResourceAttachableVolumesPrefix = "attachable-volumes-"
)

// ResourceList is a set of (resource name, quantity) pairs.
type ResourceList map[ResourceName]resource.Quantity

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// Node is a worker node in Kubernetes
// The name of the node according to etcd is in ObjectMeta.Name.
type Node struct {
	metav1.TypeMeta
	// +optional
	metav1.ObjectMeta

	// Spec defines the behavior of a node.
	// +optional
	Spec NodeSpec

	// Status describes the current status of a Node
	// +optional
	Status NodeStatus
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// NodeList is a list of nodes.
type NodeList struct {
	metav1.TypeMeta
	// +optional
	metav1.ListMeta

	Items []Node
}

// NamespaceSpec describes the attributes on a Namespace
type NamespaceSpec struct {
	// Finalizers is an opaque list of values that must be empty to permanently remove object from storage
	Finalizers []FinalizerName
}

// FinalizerName is the name identifying a finalizer during namespace lifecycle.
type FinalizerName string

// These are internal finalizer values to Kubernetes, must be qualified name unless defined here or
// in metav1.
const (
	FinalizerKubernetes FinalizerName = "kubernetes"
)

// NamespaceStatus is information about the current status of a Namespace.
type NamespaceStatus struct {
	// Phase is the current lifecycle phase of the namespace.
	// +optional
	Phase NamespacePhase
	// +optional
	Conditions []NamespaceCondition
}

// NamespacePhase defines the phase in which the namespace is
type NamespacePhase string

// These are the valid phases of a namespace.
const (
	// NamespaceActive means the namespace is available for use in the system
	NamespaceActive NamespacePhase = "Active"
	// NamespaceTerminating means the namespace is undergoing graceful termination
	NamespaceTerminating NamespacePhase = "Terminating"
)

// NamespaceConditionType defines constants reporting on status during namespace lifetime and deletion progress
type NamespaceConditionType string

// These are valid conditions of a namespace.
const (
	NamespaceDeletionDiscoveryFailure NamespaceConditionType = "NamespaceDeletionDiscoveryFailure"
	NamespaceDeletionContentFailure   NamespaceConditionType = "NamespaceDeletionContentFailure"
	NamespaceDeletionGVParsingFailure NamespaceConditionType = "NamespaceDeletionGroupVersionParsingFailure"
)

// NamespaceCondition contains details about state of namespace.
type NamespaceCondition struct {
	// Type of namespace controller condition.
	Type NamespaceConditionType
	// Status of the condition, one of True, False, Unknown.
	Status ConditionStatus
	// +optional
	LastTransitionTime metav1.Time
	// +optional
	Reason string
	// +optional
	Message string
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// Namespace provides a scope for Names.
// Use of multiple namespaces is optional
type Namespace struct {
	metav1.TypeMeta
	// +optional
	metav1.ObjectMeta

	// Spec defines the behavior of the Namespace.
	// +optional
	Spec NamespaceSpec

	// Status describes the current status of a Namespace
	// +optional
	Status NamespaceStatus
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// NamespaceList is a list of Namespaces.
type NamespaceList struct {
	metav1.TypeMeta
	// +optional
	metav1.ListMeta

	Items []Namespace
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// Binding ties one object to another; for example, a pod is bound to a node by a scheduler.
type Binding struct {
	metav1.TypeMeta
	// ObjectMeta describes the object that is being bound.
	// +optional
	metav1.ObjectMeta

	// Target is the object to bind to.
	Target ObjectReference
}

// Preconditions must be fulfilled before an operation (update, delete, etc.) is carried out.
type Preconditions struct {
	// Specifies the target UID.
	// +optional
	UID *types.UID
}

const (
	// LogStreamStdout is the stream type for stdout.
	LogStreamStdout = "Stdout"
	// LogStreamStderr is the stream type for stderr.
	LogStreamStderr = "Stderr"
	// LogStreamAll represents the combined stdout and stderr.
	LogStreamAll = "All"
)

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// PodLogOptions is the query options for a Pod's logs REST call
type PodLogOptions struct {
	metav1.TypeMeta

	// Container for which to return logs
	Container string
	// If true, follow the logs for the pod
	Follow bool
	// If true, return previous terminated container logs
	Previous bool
	// A relative time in seconds before the current time from which to show logs. If this value
	// precedes the time a pod was started, only logs since the pod start will be returned.
	// If this value is in the future, no logs will be returned.
	// Only one of sinceSeconds or sinceTime may be specified.
	SinceSeconds *int64
	// An RFC3339 timestamp from which to show logs. If this value
	// precedes the time a pod was started, only logs since the pod start will be returned.
	// If this value is in the future, no logs will be returned.
	// Only one of sinceSeconds or sinceTime may be specified.
	SinceTime *metav1.Time
	// If true, add an RFC 3339 timestamp with 9 digits of fractional seconds at the beginning of every line
	// of log output.
	Timestamps bool
	// If set, the number of lines from the end of the logs to show. If not specified,
	// logs are shown from the creation of the container or sinceSeconds or sinceTime.
	// Note that when "TailLines" is specified, "Stream" can only be set to nil or "All".
	TailLines *int64
	// If set, the number of bytes to read from the server before terminating the
	// log output. This may not display a complete final line of logging, and may return
	// slightly more or slightly less than the specified limit.
	LimitBytes *int64

	// insecureSkipTLSVerifyBackend indicates that the apiserver should not confirm the validity of the
	// serving certificate of the backend it is connecting to.  This will make the HTTPS connection between the apiserver
	// and the backend insecure. This means the apiserver cannot verify the log data it is receiving came from the real
	// kubelet.  If the kubelet is configured to verify the apiserver's TLS credentials, it does not mean the
	// connection to the real kubelet is vulnerable to a man in the middle attack (e.g. an attacker could not intercept
	// the actual log data coming from the real kubelet).
	// +optional
	InsecureSkipTLSVerifyBackend bool

	// Specify which container log stream to return to the client.
	// Acceptable values are "All", "Stdout" and "Stderr". If not specified, "All" is used, and both stdout and stderr
	// are returned interleaved.
	// Note that when "TailLines" is specified, "Stream" can only be set to nil or "All".
	// +featureGate=PodLogsQuerySplitStreams
	// +optional
	Stream *string
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// PodAttachOptions is the query options to a Pod's remote attach call
// TODO: merge w/ PodExecOptions below for stdin, stdout, etc
type PodAttachOptions struct {
	metav1.TypeMeta

	// Stdin if true indicates that stdin is to be redirected for the attach call
	// +optional
	Stdin bool

	// Stdout if true indicates that stdout is to be redirected for the attach call
	// +optional
	Stdout bool

	// Stderr if true indicates that stderr is to be redirected for the attach call
	// +optional
	Stderr bool

	// TTY if true indicates that a tty will be allocated for the attach call
	// +optional
	TTY bool

	// Container to attach to.
	// +optional
	Container string
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// PodExecOptions is the query options to a Pod's remote exec call
type PodExecOptions struct {
	metav1.TypeMeta

	// Stdin if true indicates that stdin is to be redirected for the exec call
	Stdin bool

	// Stdout if true indicates that stdout is to be redirected for the exec call
	Stdout bool

	// Stderr if true indicates that stderr is to be redirected for the exec call
	Stderr bool

	// TTY if true indicates that a tty will be allocated for the exec call
	TTY bool

	// Container in which to execute the command.
	Container string

	// Command is the remote command to execute; argv array; not executed within a shell.
	Command []string
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// PodPortForwardOptions is the query options to a Pod's port forward call
type PodPortForwardOptions struct {
	metav1.TypeMeta

	// The list of ports to forward
	// +optional
	Ports []int32
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// PodProxyOptions is the query options to a Pod's proxy call
type PodProxyOptions struct {
	metav1.TypeMeta

	// Path is the URL path to use for the current proxy request
	Path string
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// NodeProxyOptions is the query options to a Node's proxy call
type NodeProxyOptions struct {
	metav1.TypeMeta

	// Path is the URL path to use for the current proxy request
	Path string
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// ServiceProxyOptions is the query options to a Service's proxy call.
type ServiceProxyOptions struct {
	metav1.TypeMeta

	// Path is the part of URLs that include service endpoints, suffixes,
	// and parameters to use for the current proxy request to service.
	// For example, the whole request URL is
	// http://localhost/api/v1/namespaces/kube-system/services/elasticsearch-logging/_search?q=user:kimchy.
	// Path is _search?q=user:kimchy.
	Path string
}

// ObjectReference contains enough information to let you inspect or modify the referred object.
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type ObjectReference struct {
	// +optional
	Kind string
	// +optional
	Namespace string
	// +optional
	Name string
	// +optional
	UID types.UID
	// +optional
	APIVersion string
	// +optional
	ResourceVersion string

	// Optional. If referring to a piece of an object instead of an entire object, this string
	// should contain information to identify the sub-object. For example, if the object
	// reference is to a container within a pod, this would take on a value like:
	// "spec.containers{name}" (where "name" refers to the name of the container that triggered
	// the event) or if no container name is specified "spec.containers[2]" (container with
	// index 2 in this pod). This syntax is chosen only to have some well-defined way of
	// referencing a part of an object.
	// TODO: this design is not final and this field is subject to change in the future.
	// +optional
	FieldPath string
}

// LocalObjectReference contains enough information to let you locate the referenced object inside the same namespace.
type LocalObjectReference struct {
	// TODO: Add other useful fields.  apiVersion, kind, uid?
	Name string
}

// TypedLocalObjectReference contains enough information to let you locate the typed referenced object inside the same namespace.
type TypedLocalObjectReference struct {
	// APIGroup is the group for the resource being referenced.
	// If APIGroup is not specified, the specified Kind must be in the core API group.
	// For any other third-party types, APIGroup is required.
	// +optional
	APIGroup *string
	// Kind is the type of resource being referenced
	Kind string
	// Name is the name of resource being referenced
	Name string
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// SerializedReference represents a serialized object reference
type SerializedReference struct {
	metav1.TypeMeta
	// +optional
	Reference ObjectReference
}

// EventSource represents the source from which an event is generated
type EventSource struct {
	// Component from which the event is generated.
	// +optional
	Component string
	// Node name on which the event is generated.
	// +optional
	Host string
}

// Valid values for event types (new types could be added in future)
const (
	// Information only and will not cause any problems
	EventTypeNormal string = "Normal"
	// These events are to warn that something might go wrong
	EventTypeWarning string = "Warning"
)

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// Event is a report of an event somewhere in the cluster.  Events
// have a limited retention time and triggers and messages may evolve
// with time.  Event consumers should not rely on the timing of an event
// with a given Reason reflecting a consistent underlying trigger, or the
// continued existence of events with that Reason.  Events should be
// treated as informative, best-effort, supplemental data.
// TODO: Decide whether to store these separately or with the object they apply to.
type Event struct {
	metav1.TypeMeta

	metav1.ObjectMeta

	// The object that this event is about. Mapped to events.Event.regarding
	InvolvedObject ObjectReference

	// Optional; this should be a short, machine understandable string that gives the reason
	// for this event being generated. For example, if the event is reporting that a container
	// can't start, the Reason might be "ImageNotFound".
	// TODO: provide exact specification for format.
	// +optional
	Reason string

	// Optional. A human-readable description of the status of this operation.
	// TODO: decide on maximum length. Mapped to events.Event.note
	// +optional
	Message string

	// Optional. The component reporting this event. Should be a short machine understandable string.
	// +optional
	Source EventSource

	// The time at which the event was first recorded. (Time of server receipt is in TypeMeta.)
	// +optional
	FirstTimestamp metav1.Time

	// The time at which the most recent occurrence of this event was recorded.
	// +optional
	LastTimestamp metav1.Time

	// The number of times this event has occurred.
	// +optional
	Count int32

	// Type of this event (Normal, Warning), new types could be added in the future.
	// +optional
	Type string

	// Time when this Event was first observed.
	// +optional
	EventTime metav1.MicroTime

	// Data about the Event series this event represents or nil if it's a singleton Event.
	// +optional
	Series *EventSeries

	// What action was taken/failed regarding to the Regarding object.
	// +optional
	Action string

	// Optional secondary object for more complex actions.
	// +optional
	Related *ObjectReference

	// Name of the controller that emitted this Event, e.g. `kubernetes.io/kubelet`.
	// +optional
	ReportingController string

	// ID of the controller instance, e.g. `kubelet-xyzf`.
	// +optional
	ReportingInstance string
}

// EventSeries represents a series ov events
type EventSeries struct {
	// Number of occurrences in this series up to the last heartbeat time
	Count int32
	// Time of the last occurrence observed
	LastObservedTime metav1.MicroTime
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// EventList is a list of events.
type EventList struct {
	metav1.TypeMeta
	// +optional
	metav1.ListMeta

	Items []Event
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// List holds a list of objects, which may not be known by the server.
type List metainternalversion.List

// LimitType defines a type of object that is limited
type LimitType string

const (
	// LimitTypePod defines limit that applies to all pods in a namespace
	LimitTypePod LimitType = "Pod"
	// LimitTypeContainer defines limit that applies to all containers in a namespace
	LimitTypeContainer LimitType = "Container"
	// LimitTypePersistentVolumeClaim defines limit that applies to all persistent volume claims in a namespace
	LimitTypePersistentVolumeClaim LimitType = "PersistentVolumeClaim"
)

// LimitRangeItem defines a min/max usage limit for any resource that matches on kind
type LimitRangeItem struct {
	// Type of resource that this limit applies to
	// +optional
	Type LimitType
	// Max usage constraints on this kind by resource name
	// +optional
	Max ResourceList
	// Min usage constraints on this kind by resource name
	// +optional
	Min ResourceList
	// Default resource requirement limit value by resource name.
	// +optional
	Default ResourceList
	// DefaultRequest resource requirement request value by resource name.
	// +optional
	DefaultRequest ResourceList
	// MaxLimitRequestRatio represents the max burst value for the named resource
	// +optional
	MaxLimitRequestRatio ResourceList
}

// LimitRangeSpec defines a min/max usage limit for resources that match on kind
type LimitRangeSpec struct {
	// Limits is the list of LimitRangeItem objects that are enforced
	Limits []LimitRangeItem
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// LimitRange sets resource usage limits for each kind of resource in a Namespace
type LimitRange struct {
	metav1.TypeMeta
	// +optional
	metav1.ObjectMeta

	// Spec defines the limits enforced
	// +optional
	Spec LimitRangeSpec
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// LimitRangeList is a list of LimitRange items.
type LimitRangeList struct {
	metav1.TypeMeta
	// +optional
	metav1.ListMeta

	// Items is a list of LimitRange objects
	Items []LimitRange
}

// The following identify resource constants for Kubernetes object types
const (
	// Pods, number
	ResourcePods ResourceName = "pods"
	// Services, number
	ResourceServices ResourceName = "services"
	// ReplicationControllers, number
	ResourceReplicationControllers ResourceName = "replicationcontrollers"
	// ResourceQuotas, number
	ResourceQuotas ResourceName = "resourcequotas"
	// ResourceSecrets, number
	ResourceSecrets ResourceName = "secrets"
	// ResourceConfigMaps, number
	ResourceConfigMaps ResourceName = "configmaps"
	// ResourcePersistentVolumeClaims, number
	ResourcePersistentVolumeClaims ResourceName = "persistentvolumeclaims"
	// ResourceServicesNodePorts, number
	ResourceServicesNodePorts ResourceName = "services.nodeports"
	// ResourceServicesLoadBalancers, number
	ResourceServicesLoadBalancers ResourceName = "services.loadbalancers"
	// CPU request, in cores. (500m = .5 cores)
	ResourceRequestsCPU ResourceName = "requests.cpu"
	// Memory request, in bytes. (500Gi = 500GiB = 500 * 1024 * 1024 * 1024)
	ResourceRequestsMemory ResourceName = "requests.memory"
	// Storage request, in bytes
	ResourceRequestsStorage ResourceName = "requests.storage"
	// Local ephemeral storage request, in bytes. (500Gi = 500GiB = 500 * 1024 * 1024 * 1024)
	ResourceRequestsEphemeralStorage ResourceName = "requests.ephemeral-storage"
	// CPU limit, in cores. (500m = .5 cores)
	ResourceLimitsCPU ResourceName = "limits.cpu"
	// Memory limit, in bytes. (500Gi = 500GiB = 500 * 1024 * 1024 * 1024)
	ResourceLimitsMemory ResourceName = "limits.memory"
	// Local ephemeral storage limit, in bytes. (500Gi = 500GiB = 500 * 1024 * 1024 * 1024)
	ResourceLimitsEphemeralStorage ResourceName = "limits.ephemeral-storage"
	// resource.k8s.io devices requested with a certain DeviceClass, number
	ResourceClaimsPerClass string = ".deviceclass.resource.k8s.io/devices"
)

// The following identify resource prefix for Kubernetes object types
const (
	// HugePages request, in bytes. (500Gi = 500GiB = 500 * 1024 * 1024 * 1024)
	// As burst is not supported for HugePages, we would only quota its request, and ignore the limit.
	ResourceRequestsHugePagesPrefix = "requests.hugepages-"
	// Default resource requests prefix
	DefaultResourceRequestsPrefix = "requests."
)

// ResourceQuotaScope defines a filter that must match each object tracked by a quota
type ResourceQuotaScope string

// These are valid values for resource quota spec
const (
	// Match all pod objects where spec.activeDeadlineSeconds >=0
	ResourceQuotaScopeTerminating ResourceQuotaScope = "Terminating"
	// Match all pod objects where spec.activeDeadlineSeconds is nil
	ResourceQuotaScopeNotTerminating ResourceQuotaScope = "NotTerminating"
	// Match all pod objects that have best effort quality of service
	ResourceQuotaScopeBestEffort ResourceQuotaScope = "BestEffort"
	// Match all pod objects that do not have best effort quality of service
	ResourceQuotaScopeNotBestEffort ResourceQuotaScope = "NotBestEffort"
	// Match all pod objects that have priority class mentioned
	ResourceQuotaScopePriorityClass ResourceQuotaScope = "PriorityClass"
	// Match all pod objects that have cross-namespace pod (anti)affinity mentioned
	ResourceQuotaScopeCrossNamespacePodAffinity ResourceQuotaScope = "CrossNamespacePodAffinity"
)

// ResourceQuotaSpec defines the desired hard limits to enforce for Quota
type ResourceQuotaSpec struct {
	// Hard is the set of desired hard limits for each named resource
	// +optional
	Hard ResourceList
	// A collection of filters that must match each object tracked by a quota.
	// If not specified, the quota matches all objects.
	// +optional
	Scopes []ResourceQuotaScope
	// ScopeSelector is also a collection of filters like Scopes that must match each object tracked by a quota
	// but expressed using ScopeSelectorOperator in combination with possible values.
	// +optional
	ScopeSelector *ScopeSelector
}

// ScopeSelector represents the AND of the selectors represented
// by the scoped-resource selector terms.
type ScopeSelector struct {
	// A list of scope selector requirements by scope of the resources.
	// +optional
	MatchExpressions []ScopedResourceSelectorRequirement
}

// ScopedResourceSelectorRequirement is a selector that contains values, a scope name, and an operator
// that relates the scope name and values.
type ScopedResourceSelectorRequirement struct {
	// The name of the scope that the selector applies to.
	ScopeName ResourceQuotaScope
	// Represents a scope's relationship to a set of values.
	// Valid operators are In, NotIn, Exists, DoesNotExist.
	Operator ScopeSelectorOperator
	// An array of string values. If the operator is In or NotIn,
	// the values array must be non-empty. If the operator is Exists or DoesNotExist,
	// the values array must be empty.
	// This array is replaced during a strategic merge patch.
	// +optional
	Values []string
}

// ScopeSelectorOperator is the set of operators that can be used in
// a scope selector requirement.
type ScopeSelectorOperator string

// These are the valid values for ScopeSelectorOperator
const (
	ScopeSelectorOpIn           ScopeSelectorOperator = "In"
	ScopeSelectorOpNotIn        ScopeSelectorOperator = "NotIn"
	ScopeSelectorOpExists       ScopeSelectorOperator = "Exists"
	ScopeSelectorOpDoesNotExist ScopeSelectorOperator = "DoesNotExist"
)

// ResourceQuotaStatus defines the enforced hard limits and observed use
type ResourceQuotaStatus struct {
	// Hard is the set of enforced hard limits for each named resource
	// +optional
	Hard ResourceList
	// Used is the current observed total usage of the resource in the namespace
	// +optional
	Used ResourceList
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// ResourceQuota sets aggregate quota restrictions enforced per namespace
type ResourceQuota struct {
	metav1.TypeMeta
	// +optional
	metav1.ObjectMeta

	// Spec defines the desired quota
	// +optional
	Spec ResourceQuotaSpec

	// Status defines the actual enforced quota and its current usage
	// +optional
	Status ResourceQuotaStatus
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// ResourceQuotaList is a list of ResourceQuota items
type ResourceQuotaList struct {
	metav1.TypeMeta
	// +optional
	metav1.ListMeta

	// Items is a list of ResourceQuota objects
	Items []ResourceQuota
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// Secret holds secret data of a certain type.  The total bytes of the values in
// the Data field must be less than MaxSecretSize bytes.
type Secret struct {
	metav1.TypeMeta
	// +optional
	metav1.ObjectMeta

	// Immutable field, if set, ensures that data stored in the Secret cannot
	// be updated (only object metadata can be modified).
	// +optional
	Immutable *bool

	// Data contains the secret data. Each key must consist of alphanumeric
	// characters, '-', '_' or '.'. The serialized form of the secret data is a
	// base64 encoded string, representing the arbitrary (possibly non-string)
	// data value here.
	// +optional
	Data map[string][]byte `datapolicy:"password,security-key,token"`

	// Used to facilitate programmatic handling of secret data.
	// More info: https://kubernetes.io/docs/concepts/configuration/secret/#secret-types
	// +optional
	Type SecretType
}

// MaxSecretSize represents the max secret size.
const MaxSecretSize = 1 * 1024 * 1024

// SecretType defines the types of secrets
type SecretType string

// These are the valid values for SecretType
const (
	// SecretTypeOpaque is the default; arbitrary user-defined data
	SecretTypeOpaque SecretType = "Opaque"

	// SecretTypeServiceAccountToken contains a token that identifies a service account to the API
	//
	// Required fields:
	// - Secret.Annotations["kubernetes.io/service-account.name"] - the name of the ServiceAccount the token identifies
	// - Secret.Annotations["kubernetes.io/service-account.uid"] - the UID of the ServiceAccount the token identifies
	// - Secret.Data["token"] - a token that identifies the service account to the API
	SecretTypeServiceAccountToken SecretType = "kubernetes.io/service-account-token"

	// ServiceAccountNameKey is the key of the required annotation for SecretTypeServiceAccountToken secrets
	ServiceAccountNameKey = "kubernetes.io/service-account.name"
	// ServiceAccountUIDKey is the key of the required annotation for SecretTypeServiceAccountToken secrets
	ServiceAccountUIDKey = "kubernetes.io/service-account.uid"
	// ServiceAccountTokenKey is the key of the required data for SecretTypeServiceAccountToken secrets
	ServiceAccountTokenKey = "token"
	// ServiceAccountKubeconfigKey is the key of the optional kubeconfig data for SecretTypeServiceAccountToken secrets
	ServiceAccountKubeconfigKey = "kubernetes.kubeconfig"
	// ServiceAccountRootCAKey is the key of the optional root certificate authority for SecretTypeServiceAccountToken secrets
	ServiceAccountRootCAKey = "ca.crt"
	// ServiceAccountNamespaceKey is the key of the optional namespace to use as the default for namespaced API calls
	ServiceAccountNamespaceKey = "namespace"

	// SecretTypeDockercfg contains a dockercfg file that follows the same format rules as ~/.dockercfg
	//
	// Required fields:
	// - Secret.Data[".dockercfg"] - a serialized ~/.dockercfg file
	SecretTypeDockercfg SecretType = "kubernetes.io/dockercfg"

	// DockerConfigKey is the key of the required data for SecretTypeDockercfg secrets
	DockerConfigKey = ".dockercfg"

	// SecretTypeDockerConfigJSON contains a dockercfg file that follows the same format rules as ~/.docker/config.json
	//
	// Required fields:
	// - Secret.Data[".dockerconfigjson"] - a serialized ~/.docker/config.json file
	SecretTypeDockerConfigJSON SecretType = "kubernetes.io/dockerconfigjson"

	// DockerConfigJSONKey is the key of the required data for SecretTypeDockerConfigJson secrets
	DockerConfigJSONKey = ".dockerconfigjson"

	// SecretTypeBasicAuth contains data needed for basic authentication.
	//
	// Required at least one of fields:
	// - Secret.Data["username"] - username used for authentication
	// - Secret.Data["password"] - password or token needed for authentication
	SecretTypeBasicAuth SecretType = "kubernetes.io/basic-auth"

	// BasicAuthUsernameKey is the key of the username for SecretTypeBasicAuth secrets
	BasicAuthUsernameKey = "username"
	// BasicAuthPasswordKey is the key of the password or token for SecretTypeBasicAuth secrets
	BasicAuthPasswordKey = "password"

	// SecretTypeSSHAuth contains data needed for SSH authentication.
	//
	// Required field:
	// - Secret.Data["ssh-privatekey"] - private SSH key needed for authentication
	SecretTypeSSHAuth SecretType = "kubernetes.io/ssh-auth"

	// SSHAuthPrivateKey is the key of the required SSH private key for SecretTypeSSHAuth secrets
	SSHAuthPrivateKey = "ssh-privatekey"

	// SecretTypeTLS contains information about a TLS client or server secret. It
	// is primarily used with TLS termination of the Ingress resource, but may be
	// used in other types.
	//
	// Required fields:
	// - Secret.Data["tls.key"] - TLS private key.
	//   Secret.Data["tls.crt"] - TLS certificate.
	// TODO: Consider supporting different formats, specifying CA/destinationCA.
	SecretTypeTLS SecretType = "kubernetes.io/tls"

	// TLSCertKey is the key for tls certificates in a TLS secret.
	TLSCertKey = "tls.crt"
	// TLSPrivateKeyKey is the key for the private key field in a TLS secret.
	TLSPrivateKeyKey = "tls.key"
	// SecretTypeBootstrapToken is used during the automated bootstrap process (first
	// implemented by kubeadm). It stores tokens that are used to sign well known
	// ConfigMaps. They are used for authn.
	SecretTypeBootstrapToken SecretType = "bootstrap.kubernetes.io/token"
)

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// SecretList represents the list of secrets
type SecretList struct {
	metav1.TypeMeta
	// +optional
	metav1.ListMeta

	Items []Secret
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// ConfigMap holds configuration data for components or applications to consume.
type ConfigMap struct {
	metav1.TypeMeta
	// +optional
	metav1.ObjectMeta

	// Immutable field, if set, ensures that data stored in the ConfigMap cannot
	// be updated (only object metadata can be modified).
	// +optional
	Immutable *bool

	// Data contains the configuration data.
	// Each key must consist of alphanumeric characters, '-', '_' or '.'.
	// Values with non-UTF-8 byte sequences must use the BinaryData field.
	// The keys stored in Data must not overlap with the keys in
	// the BinaryData field, this is enforced during validation process.
	// +optional
	Data map[string]string

	// BinaryData contains the binary data.
	// Each key must consist of alphanumeric characters, '-', '_' or '.'.
	// BinaryData can contain byte sequences that are not in the UTF-8 range.
	// The keys stored in BinaryData must not overlap with the ones in
	// the Data field, this is enforced during validation process.
	// Using this field will require 1.10+ apiserver and
	// kubelet.
	// +optional
	BinaryData map[string][]byte
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// ConfigMapList is a resource containing a list of ConfigMap objects.
type ConfigMapList struct {
	metav1.TypeMeta
	// +optional
	metav1.ListMeta

	// Items is the list of ConfigMaps.
	Items []ConfigMap
}

// These constants are for remote command execution and port forwarding and are
// used by both the client side and server side components.
//
// This is probably not the ideal place for them, but it didn't seem worth it
// to create pkg/exec and pkg/portforward just to contain a single file with
// constants in it.  Suggestions for more appropriate alternatives are
// definitely welcome!
const (
	// Enable stdin for remote command execution
	ExecStdinParam = "input"
	// Enable stdout for remote command execution
	ExecStdoutParam = "output"
	// Enable stderr for remote command execution
	ExecStderrParam = "error"
	// Enable TTY for remote command execution
	ExecTTYParam = "tty"
	// Command to run for remote command execution
	ExecCommandParam = "command"

	// Name of header that specifies stream type
	StreamType = "streamType"
	// Value for streamType header for stdin stream
	StreamTypeStdin = "stdin"
	// Value for streamType header for stdout stream
	StreamTypeStdout = "stdout"
	// Value for streamType header for stderr stream
	StreamTypeStderr = "stderr"
	// Value for streamType header for data stream
	StreamTypeData = "data"
	// Value for streamType header for error stream
	StreamTypeError = "error"
	// Value for streamType header for terminal resize stream
	StreamTypeResize = "resize"

	// Name of header that specifies the port being forwarded
	PortHeader = "port"
	// Name of header that specifies a request ID used to associate the error
	// and data streams for a single forwarded connection
	PortForwardRequestIDHeader = "requestID"
)

// ComponentConditionType defines type and constants for component health validation.
type ComponentConditionType string

// These are the valid conditions for the component.
const (
	ComponentHealthy ComponentConditionType = "Healthy"
)

// ComponentCondition represents the condition of a component
type ComponentCondition struct {
	Type   ComponentConditionType
	Status ConditionStatus
	// +optional
	Message string
	// +optional
	Error string
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// ComponentStatus (and ComponentStatusList) holds the cluster validation info.
// Deprecated: This API is deprecated in v1.19+
type ComponentStatus struct {
	metav1.TypeMeta
	// +optional
	metav1.ObjectMeta

	// +optional
	Conditions []ComponentCondition
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// ComponentStatusList represents the list of component statuses
// Deprecated: This API is deprecated in v1.19+
type ComponentStatusList struct {
	metav1.TypeMeta
	// +optional
	metav1.ListMeta

	Items []ComponentStatus
}

// SecurityContext holds security configuration that will be applied to a container.
// Some fields are present in both SecurityContext and PodSecurityContext.  When both
// are set, the values in SecurityContext take precedence.
type SecurityContext struct {
	// The capabilities to add/drop when running containers.
	// Defaults to the default set of capabilities granted by the container runtime.
	// Note that this field cannot be set when spec.os.name is windows.
	// +optional
	Capabilities *Capabilities
	// Run container in privileged mode.
	// Processes in privileged containers are essentially equivalent to root on the host.
	// Defaults to false.
	// Note that this field cannot be set when spec.os.name is windows.
	// +optional
	Privileged *bool
	// The SELinux context to be applied to the container.
	// If unspecified, the container runtime will allocate a random SELinux context for each
	// container.  May also be set in PodSecurityContext.  If set in both SecurityContext and
	// PodSecurityContext, the value specified in SecurityContext takes precedence.
	// Note that this field cannot be set when spec.os.name is windows.
	// +optional
	SELinuxOptions *SELinuxOptions
	// The Windows specific settings applied to all containers.
	// If unspecified, the options from the PodSecurityContext will be used.
	// If set in both SecurityContext and PodSecurityContext, the value specified in SecurityContext takes precedence.
	// Note that this field cannot be set when spec.os.name is linux.
	// +optional
	WindowsOptions *WindowsSecurityContextOptions
	// The UID to run the entrypoint of the container process.
	// Defaults to user specified in image metadata if unspecified.
	// May also be set in PodSecurityContext.  If set in both SecurityContext and
	// PodSecurityContext, the value specified in SecurityContext takes precedence.
	// Note that this field cannot be set when spec.os.name is windows.
	// +optional
	RunAsUser *int64
	// The GID to run the entrypoint of the container process.
	// Uses runtime default if unset.
	// May also be set in PodSecurityContext.  If set in both SecurityContext and
	// PodSecurityContext, the value specified in SecurityContext takes precedence.
	// Note that this field cannot be set when spec.os.name is windows.
	// +optional
	RunAsGroup *int64
	// Indicates that the container must run as a non-root user.
	// If true, the Kubelet will validate the image at runtime to ensure that it
	// does not run as UID 0 (root) and fail to start the container if it does.
	// If unset or false, no such validation will be performed.
	// May also be set in PodSecurityContext.  If set in both SecurityContext and
	// PodSecurityContext, the value specified in SecurityContext takes precedence.
	// +optional
	RunAsNonRoot *bool
	// The read-only root filesystem allows you to restrict the locations that an application can write
	// files to, ensuring the persistent data can only be written to mounts.
	// Note that this field cannot be set when spec.os.name is windows.
	// +optional
	ReadOnlyRootFilesystem *bool
	// AllowPrivilegeEscalation controls whether a process can gain more
	// privileges than its parent process. This bool directly controls if
	// the no_new_privs flag will be set on the container process.
	// Note that this field cannot be set when spec.os.name is windows.
	// +optional
	AllowPrivilegeEscalation *bool
	// ProcMount denotes the type of proc mount to use for the containers.
	// The default value is Default which uses the container runtime defaults for
	// readonly paths and masked paths.
	// Note that this field cannot be set when spec.os.name is windows.
	// +optional
	ProcMount *ProcMountType
	// The seccomp options to use by this container. If seccomp options are
	// provided at both the pod & container level, the container options
	// override the pod options.
	// Note that this field cannot be set when spec.os.name is windows.
	// +optional
	SeccompProfile *SeccompProfile
	// appArmorProfile is the AppArmor options to use by this container. If set, this profile
	// overrides the pod's appArmorProfile.
	// Note that this field cannot be set when spec.os.name is windows.
	// +optional
	AppArmorProfile *AppArmorProfile
}

// ProcMountType defines the type of proc mount
type ProcMountType string

const (
	// DefaultProcMount uses the container runtime defaults for readonly and masked
	// paths for /proc.  Most container runtimes mask certain paths in /proc to avoid
	// accidental security exposure of special devices or information.
	DefaultProcMount ProcMountType = "Default"

	// UnmaskedProcMount bypasses the default masking behavior of the container
	// runtime and ensures the newly created /proc the container stays intact with
	// no modifications.
	UnmaskedProcMount ProcMountType = "Unmasked"
)

// SELinuxOptions are the labels to be applied to the container.
type SELinuxOptions struct {
	// SELinux user label
	// +optional
	User string
	// SELinux role label
	// +optional
	Role string
	// SELinux type label
	// +optional
	Type string
	// SELinux level label.
	// +optional
	Level string
}

// WindowsSecurityContextOptions contain Windows-specific options and credentials.
type WindowsSecurityContextOptions struct {
	// GMSACredentialSpecName is the name of the GMSA credential spec to use.
	// +optional
	GMSACredentialSpecName *string

	// GMSACredentialSpec is where the GMSA admission webhook
	// (https://github.com/kubernetes-sigs/windows-gmsa) inlines the contents of the
	// GMSA credential spec named by the GMSACredentialSpecName field.
	// +optional
	GMSACredentialSpec *string

	// The UserName in Windows to run the entrypoint of the container process.
	// Defaults to the user specified in image metadata if unspecified.
	// May also be set in PodSecurityContext. If set in both SecurityContext and
	// PodSecurityContext, the value specified in SecurityContext takes precedence.
	// +optional
	RunAsUserName *string

	// HostProcess determines if a container should be run as a 'Host Process' container.
	// All of a Pod's containers must have the same effective HostProcess value
	// (it is not allowed to have a mix of HostProcess containers and non-HostProcess containers).
	// In addition, if HostProcess is true then HostNetwork must also be set to true.
	// +optional
	HostProcess *bool
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// RangeAllocation is an opaque API object (not exposed to end users) that can be persisted to record
// the global allocation state of the cluster. The schema of Range and Data generic, in that Range
// should be a string representation of the inputs to a range (for instance, for IP allocation it
// might be a CIDR) and Data is an opaque blob understood by an allocator which is typically a
// binary range.  Consumers should use annotations to record additional information (schema version,
// data encoding hints). A range allocation should *ALWAYS* be recreatable at any time by observation
// of the cluster, thus the object is less strongly typed than most.
type RangeAllocation struct {
	metav1.TypeMeta
	// +optional
	metav1.ObjectMeta
	// A string representing a unique label for a range of resources, such as a CIDR "10.0.0.0/8" or
	// port range "10000-30000". Range is not strongly schema'd here. The Range is expected to define
	// a start and end unless there is an implicit end.
	Range string
	// A byte array representing the serialized state of a range allocation. Additional clarifiers on
	// the type or format of data should be represented with annotations. For IP allocations, this is
	// represented as a bit array starting at the base IP of the CIDR in Range, with each bit representing
	// a single allocated address (the fifth bit on CIDR 10.0.0.0/8 is 10.0.0.4).
	Data []byte
}

const (
	// DefaultHardPodAffinitySymmetricWeight is the weight of implicit PreferredDuringScheduling affinity rule.
	//
	// RequiredDuringScheduling affinity is not symmetric, but there is an implicit PreferredDuringScheduling affinity rule
	// corresponding to every RequiredDuringScheduling affinity rule.
	// When the --hard-pod-affinity-weight scheduler flag is not specified,
	// DefaultHardPodAffinityWeight defines the weight of the implicit PreferredDuringScheduling affinity rule.
	DefaultHardPodAffinitySymmetricWeight int32 = 1
)

// UnsatisfiableConstraintAction defines the actions that can be taken for an
// unsatisfiable constraint.
type UnsatisfiableConstraintAction string

const (
	// DoNotSchedule instructs the scheduler not to schedule the pod
	// when constraints are not satisfied.
	DoNotSchedule UnsatisfiableConstraintAction = "DoNotSchedule"
	// ScheduleAnyway instructs the scheduler to schedule the pod
	// even if constraints are not satisfied.
	ScheduleAnyway UnsatisfiableConstraintAction = "ScheduleAnyway"
)

// NodeInclusionPolicy defines the type of node inclusion policy
// +enum
type NodeInclusionPolicy string

const (
	// NodeInclusionPolicyIgnore means ignore this scheduling directive when calculating pod topology spread skew.
	NodeInclusionPolicyIgnore NodeInclusionPolicy = "Ignore"
	// NodeInclusionPolicyHonor means use this scheduling directive when calculating pod topology spread skew.
	NodeInclusionPolicyHonor NodeInclusionPolicy = "Honor"
)

// TopologySpreadConstraint specifies how to spread matching pods among the given topology.
type TopologySpreadConstraint struct {
	// MaxSkew describes the degree to which pods may be unevenly distributed.
	// When `whenUnsatisfiable=DoNotSchedule`, it is the maximum permitted difference
	// between the number of matching pods in the target topology and the global minimum.
	// The global minimum is the minimum number of matching pods in an eligible domain
	// or zero if the number of eligible domains is less than MinDomains.
	// For example, in a 3-zone cluster, MaxSkew is set to 1, and pods with the same
	// labelSelector spread as 2/2/1:
	// In this case, the global minimum is 1.
	// +-------+-------+-------+
	// | zone1 | zone2 | zone3 |
	// +-------+-------+-------+
	// |  P P  |  P P  |   P   |
	// +-------+-------+-------+
	// - if MaxSkew is 1, incoming pod can only be scheduled to zone3 to become 2/2/2;
	// scheduling it onto zone1(zone2) would make the ActualSkew(3-1) on zone1(zone2)
	// violate MaxSkew(1).
	// - if MaxSkew is 2, incoming pod can be scheduled onto any zone.
	// When `whenUnsatisfiable=ScheduleAnyway`, it is used to give higher precedence
	// to topologies that satisfy it.
	// It's a required field. Default value is 1 and 0 is not allowed.
	MaxSkew int32
	// TopologyKey is the key of node labels. Nodes that have a label with this key
	// and identical values are considered to be in the same topology.
	// We consider each <key, value> as a "bucket", and try to put balanced number
	// of pods into each bucket.
	// We define a domain as a particular instance of a topology.
	// Also, we define an eligible domain as a domain whose nodes meet the requirements of
	// nodeAffinityPolicy and nodeTaintsPolicy.
	// e.g. If TopologyKey is "kubernetes.io/hostname", each Node is a domain of that topology.
	// And, if TopologyKey is "topology.kubernetes.io/zone", each zone is a domain of that topology.
	// It's a required field.
	TopologyKey string
	// WhenUnsatisfiable indicates how to deal with a pod if it doesn't satisfy
	// the spread constraint.
	// - DoNotSchedule (default) tells the scheduler not to schedule it.
	// - ScheduleAnyway tells the scheduler to schedule the pod in any location,
	//   but giving higher precedence to topologies that would help reduce the
	//   skew.
	// A constraint is considered "Unsatisfiable" for an incoming pod
	// if and only if every possible node assignment for that pod would violate
	// "MaxSkew" on some topology.
	// For example, in a 3-zone cluster, MaxSkew is set to 1, and pods with the same
	// labelSelector spread as 3/1/1:
	// +-------+-------+-------+
	// | zone1 | zone2 | zone3 |
	// +-------+-------+-------+
	// | P P P |   P   |   P   |
	// +-------+-------+-------+
	// If WhenUnsatisfiable is set to DoNotSchedule, incoming pod can only be scheduled
	// to zone2(zone3) to become 3/2/1(3/1/2) as ActualSkew(2-1) on zone2(zone3) satisfies
	// MaxSkew(1). In other words, the cluster can still be imbalanced, but scheduler
	// won't make it *more* imbalanced.
	// It's a required field.
	WhenUnsatisfiable UnsatisfiableConstraintAction
	// LabelSelector is used to find matching pods.
	// Pods that match this label selector are counted to determine the number of pods
	// in their corresponding topology domain.
	// +optional
	LabelSelector *metav1.LabelSelector
	// MinDomains indicates a minimum number of eligible domains.
	// When the number of eligible domains with matching topology keys is less than minDomains,
	// Pod Topology Spread treats "global minimum" as 0, and then the calculation of Skew is performed.
	// And when the number of eligible domains with matching topology keys equals or greater than minDomains,
	// this value has no effect on scheduling.
	// As a result, when the number of eligible domains is less than minDomains,
	// scheduler won't schedule more than maxSkew Pods to those domains.
	// If value is nil, the constraint behaves as if MinDomains is equal to 1.
	// Valid values are integers greater than 0.
	// When value is not nil, WhenUnsatisfiable must be DoNotSchedule.
	//
	// For example, in a 3-zone cluster, MaxSkew is set to 2, MinDomains is set to 5 and pods with the same
	// labelSelector spread as 2/2/2:
	// +-------+-------+-------+
	// | zone1 | zone2 | zone3 |
	// +-------+-------+-------+
	// |  P P  |  P P  |  P P  |
	// +-------+-------+-------+
	// The number of domains is less than 5(MinDomains), so "global minimum" is treated as 0.
	// In this situation, new pod with the same labelSelector cannot be scheduled,
	// because computed skew will be 3(3 - 0) if new Pod is scheduled to any of the three zones,
	// it will violate MaxSkew.
	// +optional
	MinDomains *int32
	// NodeAffinityPolicy indicates how we will treat Pod's nodeAffinity/nodeSelector
	// when calculating pod topology spread skew. Options are:
	// - Honor: only nodes matching nodeAffinity/nodeSelector are included in the calculations.
	// - Ignore: nodeAffinity/nodeSelector are ignored. All nodes are included in the calculations.
	//
	// If this value is nil, the behavior is equivalent to the Honor policy.
	// This is a beta-level feature default enabled by the NodeInclusionPolicyInPodTopologySpread feature flag.
	// +optional
	NodeAffinityPolicy *NodeInclusionPolicy
	// NodeTaintsPolicy indicates how we will treat node taints when calculating
	// pod topology spread skew. Options are:
	// - Honor: nodes without taints, along with tainted nodes for which the incoming pod
	// has a toleration, are included.
	// - Ignore: node taints are ignored. All nodes are included.
	//
	// If this value is nil, the behavior is equivalent to the Ignore policy.
	// This is a beta-level feature default enabled by the NodeInclusionPolicyInPodTopologySpread feature flag.
	// +optional
	NodeTaintsPolicy *NodeInclusionPolicy
	// MatchLabelKeys is a set of pod label keys to select the pods over which
	// spreading will be calculated. The keys are used to lookup values from the
	// incoming pod labels, those key-value labels are ANDed with labelSelector
	// to select the group of existing pods over which spreading will be calculated
	// for the incoming pod. The same key is forbidden to exist in both MatchLabelKeys and LabelSelector.
	// MatchLabelKeys cannot be set when LabelSelector isn't set.
	// Keys that don't exist in the incoming pod labels will
	// be ignored. A null or empty list means only match against labelSelector.
	//
	// This is a beta field and requires the MatchLabelKeysInPodTopologySpread feature gate to be enabled (enabled by default).
	// +listType=atomic
	// +optional
	MatchLabelKeys []string
}

// These are the built-in errors for PortStatus.
const (
	// MixedProtocolNotSupported error in PortStatus means that the cloud provider
	// can't ensure the port on the load balancer because mixed values of protocols
	// on the same LoadBalancer type of Service are not supported by the cloud provider.
	MixedProtocolNotSupported = "MixedProtocolNotSupported"
)

// PortStatus represents the error condition of a service port
type PortStatus struct {
	// Port is the port number of the service port of which status is recorded here
	Port int32
	// Protocol is the protocol of the service port of which status is recorded here
	Protocol Protocol
	// Error is to record the problem with the service port
	// The format of the error shall comply with the following rules:
	// - built-in error values shall be specified in this file and those shall use
	//   CamelCase names
	// - cloud provider specific error values must have names that comply with the
	//   format foo.example.com/CamelCase.
	// ---
	// The regex it matches is (dns1123SubdomainFmt/)?(qualifiedNameFmt)
	// +optional
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^([a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*/)?(([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9])$`
	// +kubebuilder:validation:MaxLength=316
	Error *string
}

// LoadBalancerIPMode represents the mode of the LoadBalancer ingress IP
type LoadBalancerIPMode string

const (
	// LoadBalancerIPModeVIP indicates that traffic is delivered to the node with
	// the destination set to the load-balancer's IP and port.
	LoadBalancerIPModeVIP LoadBalancerIPMode = "VIP"
	// LoadBalancerIPModeProxy indicates that traffic is delivered to the node or pod with
	// the destination set to the node's IP and port or the pod's IP and port.
	LoadBalancerIPModeProxy LoadBalancerIPMode = "Proxy"
)

// ImageVolumeSource represents a image volume resource.
type ImageVolumeSource struct {
	// Required: Image or artifact reference to be used.
	// Behaves in the same way as pod.spec.containers[*].image.
	// Pull secrets will be assembled in the same way as for the container image by looking up node credentials, SA image pull secrets, and pod spec image pull secrets.
	// More info: https://kubernetes.io/docs/concepts/containers/images
	// This field is optional to allow higher level config management to default or override
	// container images in workload controllers like Deployments and StatefulSets.
	// +optional
	Reference string

	// Policy for pulling OCI objects. Possible values are:
	// Always: the kubelet always attempts to pull the reference. Container creation will fail If the pull fails.
	// Never: the kubelet never pulls the reference and only uses a local image or artifact. Container creation will fail if the reference isn't present.
	// IfNotPresent: the kubelet pulls if the reference isn't already present on disk. Container creation will fail if the reference isn't present and the pull fails.
	// Defaults to Always if :latest tag is specified, or IfNotPresent otherwise.
	// +optional
	PullPolicy PullPolicy
}
