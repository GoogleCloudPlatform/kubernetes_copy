/*
Copyright 2022 The Kubernetes Authors.

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

package v1alpha3

import (
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
)

const (
	// Finalizer is the finalizer that gets set for claims
	// which were allocated through a builtin controller.
	// Reserved for use by Kubernetes, DRA driver controllers must
	// use their own finalizer.
	Finalizer = "resource.kubernetes.io/delete-protection"
)

// +genclient
// +genclient:nonNamespaced
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:prerelease-lifecycle-gen:introduced=1.31
// +k8s:prerelease-lifecycle-gen:replacement=resource.k8s.io,v1beta1,ResourceSlice

// ResourceSlice represents one or more resources in a pool of similar resources,
// managed by a common driver. A pool may span more than one ResourceSlice, and exactly how many
// ResourceSlices comprise a pool is determined by the driver.
//
// At the moment, the only supported resources are devices with attributes and capacities.
// Each device in a given pool, regardless of how many ResourceSlices, must have a unique name.
// The ResourceSlice in which a device gets published may change over time. The unique identifier
// for a device is the tuple <driver name>, <pool name>, <device name>.
//
// Whenever a driver needs to update a pool, it increments the pool.Spec.Pool.Generation number
// and updates all ResourceSlices with that new number and new resource definitions. A consumer
// must only use ResourceSlices with the highest generation number and ignore all others.
//
// When allocating all resources in a pool matching certain criteria or when
// looking for the best solution among several different alternatives, a
// consumer should check the number of ResourceSlices in a pool (included in
// each ResourceSlice) to determine whether its view of a pool is complete and
// if not, should wait until the driver has completed updating the pool.
//
// For resources that are not local to a node, the node name is not set. Instead,
// the driver may use a node selector to specify where the devices are available.
//
// This is an alpha type and requires enabling the DynamicResourceAllocation
// feature gate.
type ResourceSlice struct {
	metav1.TypeMeta `json:",inline"`
	// Standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	// Contains the information published by the driver.
	//
	// Changing the spec automatically increments the metadata.generation number.
	Spec ResourceSliceSpec `json:"spec" protobuf:"bytes,2,name=spec"`
}

const (
	// ResourceSliceSelectorNodeName can be used in a [metav1.ListOptions]
	// field selector to filter based on [ResourceSliceSpec.NodeName].
	ResourceSliceSelectorNodeName = "spec.nodeName"
	// ResourceSliceSelectorDriver can be used in a [metav1.ListOptions]
	// field selector to filter based on [ResourceSliceSpec.Driver].
	ResourceSliceSelectorDriver = "spec.driver"
)

// ResourceSliceSpec contains the information published by the driver in one ResourceSlice.
type ResourceSliceSpec struct {
	// Driver identifies the DRA driver providing the capacity information.
	// A field selector can be used to list only ResourceSlice
	// objects with a certain driver name.
	//
	// Must be a DNS subdomain and should end with a DNS domain owned by the
	// vendor of the driver. This field is immutable.
	//
	// +required
	Driver string `json:"driver" protobuf:"bytes,1,name=driver"`

	// Pool describes the pool that this ResourceSlice belongs to.
	//
	// +required
	Pool ResourcePool `json:"pool" protobuf:"bytes,2,name=pool"`

	// NodeName identifies the node which provides the resources in this pool.
	// A field selector can be used to list only ResourceSlice
	// objects belonging to a certain node.
	//
	// This field can be used to limit access from nodes to ResourceSlices with
	// the same node name. It also indicates to autoscalers that adding
	// new nodes of the same type as some old node might also make new
	// resources available.
	//
	// Exactly one of NodeName, NodeSelector and AllNodes must be set.
	// This field is immutable.
	//
	// +optional
	// +oneOf=NodeSelection
	NodeName string `json:"nodeName,omitempty" protobuf:"bytes,3,opt,name=nodeName"`

	// NodeSelector defines which nodes have access to the resources in the pool,
	// when that pool is not limited to a single node.
	//
	// Must use exactly one term.
	//
	// Exactly one of NodeName, NodeSelector and AllNodes must be set.
	//
	// +optional
	// +oneOf=NodeSelection
	NodeSelector *v1.NodeSelector `json:"nodeSelector,omitempty" protobuf:"bytes,4,opt,name=nodeSelector"`

	// AllNodes indicates that all nodes have access to the resources in the pool.
	//
	// Exactly one of NodeName, NodeSelector and AllNodes must be set.
	//
	// +optional
	// +oneOf=NodeSelection
	AllNodes bool `json:"allNodes,omitempty" protobuf:"bytes,5,opt,name=allNodes"`

	// Devices lists some or all of the devices in this pool.
	//
	// Must not have more than 128 entries.
	//
	// +optional
	// +listType=atomic
	Devices []Device `json:"devices" protobuf:"bytes,6,name=devices"`
}

// DriverNameMaxLength is the maximum valid length of a driver name in the
// ResourceSliceSpec and other places. It's the same as for CSI driver names.
const DriverNameMaxLength = 63

// ResourcePool describes the pool that ResourceSlices belong to.
type ResourcePool struct {
	// Name is used to identify the pool. For node-local devices, this
	// is often the node name, but this is not required.
	//
	// It must not be longer than 253 characters and must consist of one or more DNS sub-domains
	// separated by slashes. This field is immutable.
	//
	// +required
	Name string `json:"name" protobuf:"bytes,1,name=name"`

	// Generation tracks the change in a pool over time. Whenever a driver
	// changes something about one or more of the resources in a pool, it
	// must change the generation in all ResourceSlices which are part of
	// that pool. Consumers of ResourceSlices should only consider
	// resources from the pool with the highest generation number. The
	// generation may be reset by drivers, which should be fine for
	// consumers, assuming that all ResourceSlices in a pool are updated to
	// match or deleted.
	//
	// Combined with ResourceSliceCount, this mechanism enables consumers to
	// detect pools which are comprised of multiple ResourceSlices and are
	// in an incomplete state.
	//
	// +required
	Generation int64 `json:"generation" protobuf:"bytes,2,name=generation"`

	// ResourceSliceCount is the total number of ResourceSlices in the pool at this
	// generation number. Must be greater than zero.
	//
	// Consumers can use this to check whether they have seen all ResourceSlices
	// belonging to the same pool.
	//
	// +required
	ResourceSliceCount int64 `json:"resourceSliceCount" protobuf:"bytes,3,name=resourceSliceCount"`
}

const ResourceSliceMaxSharedCapacity = 128
const ResourceSliceMaxDevices = 128
const PoolNameMaxLength = validation.DNS1123SubdomainMaxLength // Same as for a single node name.

// Device represents one individual hardware instance that can be selected based
// on its attributes. Besides the name, exactly one field must be set.
type Device struct {
	// Name is unique identifier among all devices managed by
	// the driver in the pool. It must be a DNS label.
	//
	// +required
	Name string `json:"name" protobuf:"bytes,1,name=name"`

	// Basic defines one device instance.
	//
	// +optional
	// +oneOf=deviceType
	Basic *BasicDevice `json:"basic,omitempty" protobuf:"bytes,2,opt,name=basic"`
}

// BasicDevice defines one device instance.
type BasicDevice struct {
	// Attributes defines the set of attributes for this device.
	// The name of each attribute must be unique in that set.
	//
	// The maximum number of attributes and capacities combined is 32.
	//
	// +optional
	Attributes map[QualifiedName]DeviceAttribute `json:"attributes,omitempty" protobuf:"bytes,1,rep,name=attributes"`

	// Capacity defines the set of capacities for this device.
	// The name of each capacity must be unique in that set.
	//
	// The maximum number of attributes and capacities combined is 32.
	//
	// +optional
	Capacity map[QualifiedName]resource.Quantity `json:"capacity,omitempty" protobuf:"bytes,2,rep,name=capacity"`
}

// Limit for the sum of the number of entries in both attributes and capacity.
const ResourceSliceMaxAttributesAndCapacitiesPerDevice = 32

// QualifiedName is the name of a device attribute or capacity.
//
// Attributes and capacities are defined either by the owner of the specific
// driver (usually the vendor) or by some 3rd party (e.g. the Kubernetes
// project). Because they are sometimes compared across devices, a given name
// is expected to mean the same thing and have the same type on all devices.
//
// Names must be either a C identifier (e.g. "theName") or a DNS subdomain
// followed by a slash ("/") followed by a C identifier
// (e.g. "dra.example.com/theName"). Names which do not include the
// domain prefix are assumed to be part of the driver's domain. Attributes
// or capacities defined by 3rd parties must include the domain prefix.
//
// The maximum length for the DNS subdomain is 63 characters (same as
// for driver names) and the maximum length of the C identifier
// is 32.
type QualifiedName string

// FullyQualifiedName is a QualifiedName where the domain is set.
type FullyQualifiedName string

// DeviceMaxDomainLength is the maximum length of the domain prefix in a fully-qualified name.
const DeviceMaxDomainLength = 63

// DeviceMaxIDLength is the maximum length of the identifier in a device attribute or capacity name (`<domain>/<ID>`).
const DeviceMaxIDLength = 32

// DeviceAttribute must have exactly one field set.
type DeviceAttribute struct {
	// The Go field names below have a Value suffix to avoid a conflict between the
	// field "String" and the corresponding method. That method is required.
	// The Kubernetes API is defined without that suffix to keep it more natural.

	// IntValue is a number.
	//
	// +optional
	// +oneOf=ValueType
	IntValue *int64 `json:"int,omitempty" protobuf:"varint,2,opt,name=int"`

	// BoolValue is a true/false value.
	//
	// +optional
	// +oneOf=ValueType
	BoolValue *bool `json:"bool,omitempty" protobuf:"varint,3,opt,name=bool"`

	// StringValue is a string. Must not be longer than 64 characters.
	//
	// +optional
	// +oneOf=ValueType
	StringValue *string `json:"string,omitempty" protobuf:"bytes,4,opt,name=string"`

	// VersionValue is a semantic version according to semver.org spec 2.0.0.
	// Must not be longer than 64 characters.
	//
	// +optional
	// +oneOf=ValueType
	VersionValue *string `json:"version,omitempty" protobuf:"bytes,5,opt,name=version"`
}

// DeviceAttributeMaxValueLength is the maximum length of a string or version attribute value.
const DeviceAttributeMaxValueLength = 64

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:prerelease-lifecycle-gen:introduced=1.31
// +k8s:prerelease-lifecycle-gen:replacement=resource.k8s.io,v1beta1,ResourceSliceList

// ResourceSliceList is a collection of ResourceSlices.
type ResourceSliceList struct {
	metav1.TypeMeta `json:",inline"`
	// Standard list metadata
	// +optional
	metav1.ListMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	// Items is the list of resource ResourceSlices.
	Items []ResourceSlice `json:"items" protobuf:"bytes,2,rep,name=items"`
}

// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:prerelease-lifecycle-gen:introduced=1.31
// +k8s:prerelease-lifecycle-gen:replacement=resource.k8s.io,v1beta1,ResourceClaim

// ResourceClaim describes a request for access to resources in the cluster,
// for use by workloads. For example, if a workload needs an accelerator device
// with specific properties, this is how that request is expressed. The status
// stanza tracks whether this claim has been satisfied and what specific
// resources have been allocated.
//
// This is an alpha type and requires enabling the DynamicResourceAllocation
// feature gate.
type ResourceClaim struct {
	metav1.TypeMeta `json:",inline"`
	// Standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	// Spec describes what is being requested and how to configure it.
	// The spec is immutable.
	Spec ResourceClaimSpec `json:"spec" protobuf:"bytes,2,name=spec"`

	// Status describes whether the claim is ready to use and what has been allocated.
	// +optional
	Status ResourceClaimStatus `json:"status,omitempty" protobuf:"bytes,3,opt,name=status"`
}

// ResourceClaimSpec defines what is being requested in a ResourceClaim and how to configure it.
type ResourceClaimSpec struct {
	// Devices defines how to request devices.
	//
	// +optional
	Devices DeviceClaim `json:"devices" protobuf:"bytes,1,name=devices"`

	// Controller is tombstoned since Kubernetes 1.32 where
	// it got removed. May be reused once decoding v1alpha3 is no longer
	// supported.
	// Controller string `json:"controller,omitempty" protobuf:"bytes,2,opt,name=controller"`
}

// DeviceClaim defines how to request devices with a ResourceClaim.
type DeviceClaim struct {
	// Requests represent individual requests for distinct devices which
	// must all be satisfied. If empty, nothing needs to be allocated.
	//
	// +optional
	// +listType=atomic
	Requests []DeviceRequest `json:"requests" protobuf:"bytes,1,name=requests"`

	// These constraints must be satisfied by the set of devices that get
	// allocated for the claim.
	//
	// +optional
	// +listType=atomic
	Constraints []DeviceConstraint `json:"constraints,omitempty" protobuf:"bytes,2,opt,name=constraints"`

	// This field holds configuration for multiple potential drivers which
	// could satisfy requests in this claim. It is ignored while allocating
	// the claim.
	//
	// +optional
	// +listType=atomic
	Config []DeviceClaimConfiguration `json:"config,omitempty" protobuf:"bytes,3,opt,name=config"`

	// Potential future extension, ignored by older schedulers. This is
	// fine because scoring allows users to define a preference, without
	// making it a hard requirement.
	//
	// Score *SomeScoringStruct
}

const (
	DeviceRequestsMaxSize    = AllocationResultsMaxSize
	DeviceConstraintsMaxSize = 32
	DeviceConfigMaxSize      = 32
)

// DeviceRequest is a request for devices required for a claim.
// This is typically a request for a single resource like a device, but can
// also ask for several identical devices.
//
// A DeviceClassName is currently required. Clients must check that it is
// indeed set. It's absence indicates that something changed in a way that
// is not supported by the client yet, in which case it must refuse to
// handle the request.
type DeviceRequest struct {
	// Name can be used to reference this request in a pod.spec.containers[].resources.claims
	// entry and in a constraint of the claim.
	//
	// Must be a DNS label.
	//
	// +required
	Name string `json:"name" protobuf:"bytes,1,name=name"`

	// DeviceClassName references a specific DeviceClass, which can define
	// additional configuration and selectors to be inherited by this
	// request.
	//
	// A class is required. Which classes are available depends on the cluster.
	//
	// Administrators may use this to restrict which devices may get
	// requested by only installing classes with selectors for permitted
	// devices. If users are free to request anything without restrictions,
	// then administrators can create an empty DeviceClass for users
	// to reference.
	//
	// +required
	DeviceClassName string `json:"deviceClassName" protobuf:"bytes,2,name=deviceClassName"`

	// Selectors define criteria which must be satisfied by a specific
	// device in order for that device to be considered for this
	// request. All selectors must be satisfied for a device to be
	// considered.
	//
	// +optional
	// +listType=atomic
	Selectors []DeviceSelector `json:"selectors,omitempty" protobuf:"bytes,3,name=selectors"`

	// AllocationMode and its related fields define how devices are allocated
	// to satisfy this request. Supported values are:
	//
	// - ExactCount: This request is for a specific number of devices.
	//   This is the default. The exact number is provided in the
	//   count field.
	//
	// - All: This request is for all of the matching devices in a pool.
	//   Allocation will fail if some devices are already allocated,
	//   unless adminAccess is requested.
	//
	// If AlloctionMode is not specified, the default mode is ExactCount. If
	// the mode is ExactCount and count is not specified, the default count is
	// one. Any other requests must specify this field.
	//
	// More modes may get added in the future. Clients must refuse to handle
	// requests with unknown modes.
	//
	// +optional
	AllocationMode DeviceAllocationMode `json:"allocationMode,omitempty" protobuf:"bytes,4,opt,name=allocationMode"`

	// Count is used only when the count mode is "ExactCount". Must be greater than zero.
	// If AllocationMode is ExactCount and this field is not specified, the default is one.
	//
	// +optional
	// +oneOf=AllocationMode
	Count int64 `json:"count,omitempty" protobuf:"bytes,5,opt,name=count"`

	// AdminAccess indicates that this is a claim for administrative access
	// to the device(s). Claims with AdminAccess are expected to be used for
	// monitoring or other management services for a device.  They ignore
	// all ordinary claims to the device with respect to access modes and
	// any resource allocations.
	//
	// This is an alpha field and requires enabling the DRAAdminAccess
	// feature gate. Admin access is disabled if this field is unset or
	// set to false, otherwise it is enabled.
	//
	// +optional
	// +featureGate=DRAAdminAccess
	AdminAccess *bool `json:"adminAccess,omitempty" protobuf:"bytes,6,opt,name=adminAccess"`
}

const (
	DeviceSelectorsMaxSize = 32
)

type DeviceAllocationMode string

// Valid [DeviceRequest.CountMode] values.
const (
	DeviceAllocationModeExactCount = DeviceAllocationMode("ExactCount")
	DeviceAllocationModeAll        = DeviceAllocationMode("All")
)

// DeviceSelector must have exactly one field set.
type DeviceSelector struct {
	// CEL contains a CEL expression for selecting a device.
	//
	// +optional
	// +oneOf=SelectorType
	CEL *CELDeviceSelector `json:"cel,omitempty" protobuf:"bytes,1,opt,name=cel"`
}

// CELDeviceSelector contains a CEL expression for selecting a device.
type CELDeviceSelector struct {
	// Expression is a CEL expression which evaluates a single device. It
	// must evaluate to true when the device under consideration satisfies
	// the desired criteria, and false when it does not. Any other result
	// is an error and causes allocation of devices to abort.
	//
	// The expression's input is an object named "device", which carries
	// the following properties:
	//  - driver (string): the name of the driver which defines this device.
	//  - attributes (map[string]object): the device's attributes, grouped by prefix
	//    (e.g. device.attributes["dra.example.com"] evaluates to an object with all
	//    of the attributes which were prefixed by "dra.example.com".
	//  - capacity (map[string]object): the device's capacities, grouped by prefix.
	//
	// Example: Consider a device with driver="dra.example.com", which exposes
	// two attributes named "model" and "ext.example.com/family" and which
	// exposes one capacity named "modules". This input to this expression
	// would have the following fields:
	//
	//     device.driver
	//     device.attributes["dra.example.com"].model
	//     device.attributes["ext.example.com"].family
	//     device.capacity["dra.example.com"].modules
	//
	// The device.driver field can be used to check for a specific driver,
	// either as a high-level precondition (i.e. you only want to consider
	// devices from this driver) or as part of a multi-clause expression
	// that is meant to consider devices from different drivers.
	//
	// The value type of each attribute is defined by the device
	// definition, and users who write these expressions must consult the
	// documentation for their specific drivers. The value type of each
	// capacity is Quantity.
	//
	// If an unknown prefix is used as a lookup in either device.attributes
	// or device.capacity, an empty map will be returned. Any reference to
	// an unknown field will cause an evaluation error and allocation to
	// abort.
	//
	// A robust expression should check for the existence of attributes
	// before referencing them.
	//
	// For ease of use, the cel.bind() function is enabled, and can be used
	// to simplify expressions that access multiple attributes with the
	// same domain. For example:
	//
	//     cel.bind(dra, device.attributes["dra.example.com"], dra.someBool && dra.anotherBool)
	//
	// The length of the expression must be smaller or equal to 10 Ki. The
	// cost of evaluating it is also limited based on the estimated number
	// of logical steps.
	//
	// +required
	Expression string `json:"expression" protobuf:"bytes,1,name=expression"`
}

// CELSelectorExpressionMaxCost specifies the cost limit for a single CEL selector
// evaluation.
//
// There is no overall budget for selecting a device, so the actual time
// required for that is proportional to the number of CEL selectors and how
// often they need to be evaluated, which can vary depending on several factors
// (number of devices, cluster utilization, additional constraints).
//
// Validation against this limit and [CELSelectorExpressionMaxLength] happens
// only when setting an expression for the first time or when changing it. If
// the limits are changed in a future Kubernetes release, existing users are
// guaranteed that existing expressions will continue to be valid.
//
// However, the kube-scheduler also applies this cost limit at runtime, so it
// could happen that a valid expression fails at runtime after an up- or
// downgrade. This can also happen without version skew when the cost estimate
// underestimated the actual cost. That this might happen is the reason why
// kube-scheduler enforces the runtime limit instead of relying on validation.
//
// According to
// https://github.com/kubernetes/kubernetes/blob/4aeaf1e99e82da8334c0d6dddd848a194cd44b4f/staging/src/k8s.io/apiserver/pkg/apis/cel/config.go#L20-L22,
// this gives roughly 0.1 second for each expression evaluation.
// However, this depends on how fast the machine is.
const CELSelectorExpressionMaxCost = 1000000

// CELSelectorExpressionMaxLength is the maximum length of a CEL selector expression string.
const CELSelectorExpressionMaxLength = 10 * 1024

// DeviceConstraint must have exactly one field set besides Requests.
type DeviceConstraint struct {
	// Requests is a list of the one or more requests in this claim which
	// must co-satisfy this constraint. If a request is fulfilled by
	// multiple devices, then all of the devices must satisfy the
	// constraint. If this is not specified, this constraint applies to all
	// requests in this claim.
	//
	// +optional
	// +listType=atomic
	Requests []string `json:"requests,omitempty" protobuf:"bytes,1,opt,name=requests"`

	// MatchAttribute requires that all devices in question have this
	// attribute and that its type and value are the same across those
	// devices.
	//
	// For example, if you specified "dra.example.com/numa" (a hypothetical example!),
	// then only devices in the same NUMA node will be chosen. A device which
	// does not have that attribute will not be chosen. All devices should
	// use a value of the same type for this attribute because that is part of
	// its specification, but if one device doesn't, then it also will not be
	// chosen.
	//
	// Must include the domain qualifier.
	//
	// +optional
	// +oneOf=ConstraintType
	MatchAttribute *FullyQualifiedName `json:"matchAttribute,omitempty" protobuf:"bytes,2,opt,name=matchAttribute"`

	// Potential future extension, not part of the current design:
	// A CEL expression which compares different devices and returns
	// true if they match.
	//
	// Because it would be part of a one-of, old schedulers will not
	// accidentally ignore this additional, for them unknown match
	// criteria.
	//
	// MatchExpression string
}

// DeviceClaimConfiguration is used for configuration parameters in DeviceClaim.
type DeviceClaimConfiguration struct {
	// Requests lists the names of requests where the configuration applies.
	// If empty, it applies to all requests.
	//
	// +optional
	// +listType=atomic
	Requests []string `json:"requests,omitempty" protobuf:"bytes,1,opt,name=requests"`

	DeviceConfiguration `json:",inline" protobuf:"bytes,2,name=deviceConfiguration"`
}

// DeviceConfiguration must have exactly one field set. It gets embedded
// inline in some other structs which have other fields, so field names must
// not conflict with those.
type DeviceConfiguration struct {
	// Opaque provides driver-specific configuration parameters.
	//
	// +optional
	// +oneOf=ConfigurationType
	Opaque *OpaqueDeviceConfiguration `json:"opaque,omitempty" protobuf:"bytes,1,opt,name=opaque"`
}

// OpaqueDeviceConfiguration contains configuration parameters for a driver
// in a format defined by the driver vendor.
type OpaqueDeviceConfiguration struct {
	// Driver is used to determine which kubelet plugin needs
	// to be passed these configuration parameters.
	//
	// An admission policy provided by the driver developer could use this
	// to decide whether it needs to validate them.
	//
	// Must be a DNS subdomain and should end with a DNS domain owned by the
	// vendor of the driver.
	//
	// +required
	Driver string `json:"driver" protobuf:"bytes,1,name=driver"`

	// Parameters can contain arbitrary data. It is the responsibility of
	// the driver developer to handle validation and versioning. Typically this
	// includes self-identification and a version ("kind" + "apiVersion" for
	// Kubernetes types), with conversion between different versions.
	//
	// The length of the raw data must be smaller or equal to 10 Ki.
	//
	// +required
	Parameters runtime.RawExtension `json:"parameters" protobuf:"bytes,2,name=parameters"`
}

// OpaqueParametersMaxLength is the maximum length of the raw data in an
// [OpaqueDeviceConfiguration.Parameters] field.
const OpaqueParametersMaxLength = 10 * 1024

// ResourceClaimStatus tracks whether the resource has been allocated and what
// the result of that was.
type ResourceClaimStatus struct {
	// Allocation is set once the claim has been allocated successfully.
	//
	// +optional
	Allocation *AllocationResult `json:"allocation,omitempty" protobuf:"bytes,1,opt,name=allocation"`

	// ReservedFor indicates which entities are currently allowed to use
	// the claim. A Pod which references a ResourceClaim which is not
	// reserved for that Pod will not be started. A claim that is in
	// use or might be in use because it has been reserved must not get
	// deallocated.
	//
	// In a cluster with multiple scheduler instances, two pods might get
	// scheduled concurrently by different schedulers. When they reference
	// the same ResourceClaim which already has reached its maximum number
	// of consumers, only one pod can be scheduled.
	//
	// Both schedulers try to add their pod to the claim.status.reservedFor
	// field, but only the update that reaches the API server first gets
	// stored. The other one fails with an error and the scheduler
	// which issued it knows that it must put the pod back into the queue,
	// waiting for the ResourceClaim to become usable again.
	//
	// There can be at most 256 such reservations. This may get increased in
	// the future, but not reduced.
	//
	// +optional
	// +listType=map
	// +listMapKey=uid
	// +patchStrategy=merge
	// +patchMergeKey=uid
	ReservedFor []ResourceClaimConsumerReference `json:"reservedFor,omitempty" protobuf:"bytes,2,opt,name=reservedFor" patchStrategy:"merge" patchMergeKey:"uid"`

	// DeallocationRequested is tombstoned since Kubernetes 1.32 where
	// it got removed. May be reused once decoding v1alpha3 is no longer
	// supported.
	// DeallocationRequested bool `json:"deallocationRequested,omitempty" protobuf:"bytes,3,opt,name=deallocationRequested"`

	// Devices contains the status of each device allocated for this
	// claim, as reported by the driver. This can include driver-specific
	// information. Entries are owned by their respective drivers.
	//
	// +optional
	// +listType=map
	// +listMapKey=driver
	// +listMapKey=device
	// +listMapKey=pool
	// +featureGate=DRAResourceClaimDeviceStatus
	Devices []AllocatedDeviceStatus `json:"devices,omitempty" protobuf:"bytes,4,opt,name=devices"`
}

// ResourceClaimReservedForMaxSize is the maximum number of entries in
// claim.status.reservedFor.
const ResourceClaimReservedForMaxSize = 256

// ResourceClaimConsumerReference contains enough information to let you
// locate the consumer of a ResourceClaim. The user must be a resource in the same
// namespace as the ResourceClaim.
type ResourceClaimConsumerReference struct {
	// APIGroup is the group for the resource being referenced. It is
	// empty for the core API. This matches the group in the APIVersion
	// that is used when creating the resources.
	// +optional
	APIGroup string `json:"apiGroup,omitempty" protobuf:"bytes,1,opt,name=apiGroup"`
	// Resource is the type of resource being referenced, for example "pods".
	// +required
	Resource string `json:"resource" protobuf:"bytes,3,name=resource"`
	// Name is the name of resource being referenced.
	// +required
	Name string `json:"name" protobuf:"bytes,4,name=name"`
	// UID identifies exactly one incarnation of the resource.
	// +required
	UID types.UID `json:"uid" protobuf:"bytes,5,name=uid"`
}

// AllocationResult contains attributes of an allocated resource.
type AllocationResult struct {
	// Devices is the result of allocating devices.
	//
	// +optional
	Devices DeviceAllocationResult `json:"devices,omitempty" protobuf:"bytes,1,opt,name=devices"`

	// NodeSelector defines where the allocated resources are available. If
	// unset, they are available everywhere.
	//
	// +optional
	NodeSelector *v1.NodeSelector `json:"nodeSelector,omitempty" protobuf:"bytes,3,opt,name=nodeSelector"`

	// Controller is tombstoned since Kubernetes 1.32 where
	// it got removed. May be reused once decoding v1alpha3 is no longer
	// supported.
	// Controller string `json:"controller,omitempty" protobuf:"bytes,4,opt,name=controller"`
}

// DeviceAllocationResult is the result of allocating devices.
type DeviceAllocationResult struct {
	// Results lists all allocated devices.
	//
	// +optional
	// +listType=atomic
	Results []DeviceRequestAllocationResult `json:"results,omitempty" protobuf:"bytes,1,opt,name=results"`

	// This field is a combination of all the claim and class configuration parameters.
	// Drivers can distinguish between those based on a flag.
	//
	// This includes configuration parameters for drivers which have no allocated
	// devices in the result because it is up to the drivers which configuration
	// parameters they support. They can silently ignore unknown configuration
	// parameters.
	//
	// +optional
	// +listType=atomic
	Config []DeviceAllocationConfiguration `json:"config,omitempty" protobuf:"bytes,2,opt,name=config"`
}

// AllocationResultsMaxSize represents the maximum number of
// entries in allocation.devices.results.
const AllocationResultsMaxSize = 32

// DeviceRequestAllocationResult contains the allocation result for one request.
type DeviceRequestAllocationResult struct {
	// Request is the name of the request in the claim which caused this
	// device to be allocated. Multiple devices may have been allocated
	// per request.
	//
	// +required
	Request string `json:"request" protobuf:"bytes,1,name=request"`

	// Driver specifies the name of the DRA driver whose kubelet
	// plugin should be invoked to process the allocation once the claim is
	// needed on a node.
	//
	// Must be a DNS subdomain and should end with a DNS domain owned by the
	// vendor of the driver.
	//
	// +required
	Driver string `json:"driver" protobuf:"bytes,2,name=driver"`

	// This name together with the driver name and the device name field
	// identify which device was allocated (`<driver name>/<pool name>/<device name>`).
	//
	// Must not be longer than 253 characters and may contain one or more
	// DNS sub-domains separated by slashes.
	//
	// +required
	Pool string `json:"pool" protobuf:"bytes,3,name=pool"`

	// Device references one device instance via its name in the driver's
	// resource pool. It must be a DNS label.
	//
	// +required
	Device string `json:"device" protobuf:"bytes,4,name=device"`

	// AdminAccess indicates that this device was allocated for
	// administrative access. See the corresponding request field
	// for a definition of mode.
	//
	// This is an alpha field and requires enabling the DRAAdminAccess
	// feature gate. Admin access is disabled if this field is unset or
	// set to false, otherwise it is enabled.
	//
	// +optional
	// +featureGate=DRAAdminAccess
	AdminAccess *bool `json:"adminAccess" protobuf:"bytes,5,name=adminAccess"`
}

// DeviceAllocationConfiguration gets embedded in an AllocationResult.
type DeviceAllocationConfiguration struct {
	// Source records whether the configuration comes from a class and thus
	// is not something that a normal user would have been able to set
	// or from a claim.
	//
	// +required
	Source AllocationConfigSource `json:"source" protobuf:"bytes,1,name=source"`

	// Requests lists the names of requests where the configuration applies.
	// If empty, its applies to all requests.
	//
	// +optional
	// +listType=atomic
	Requests []string `json:"requests,omitempty" protobuf:"bytes,2,opt,name=requests"`

	DeviceConfiguration `json:",inline" protobuf:"bytes,3,name=deviceConfiguration"`
}

type AllocationConfigSource string

// Valid [DeviceAllocationConfiguration.Source] values.
const (
	AllocationConfigSourceClass = "FromClass"
	AllocationConfigSourceClaim = "FromClaim"
)

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:prerelease-lifecycle-gen:introduced=1.31
// +k8s:prerelease-lifecycle-gen:replacement=resource.k8s.io,v1beta1,ResourceClaimList

// ResourceClaimList is a collection of claims.
type ResourceClaimList struct {
	metav1.TypeMeta `json:",inline"`
	// Standard list metadata
	// +optional
	metav1.ListMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	// Items is the list of resource claims.
	Items []ResourceClaim `json:"items" protobuf:"bytes,2,rep,name=items"`
}

// +genclient
// +genclient:nonNamespaced
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:prerelease-lifecycle-gen:introduced=1.31
// +k8s:prerelease-lifecycle-gen:replacement=resource.k8s.io,v1beta1,DeviceClass

// DeviceClass is a vendor- or admin-provided resource that contains
// device configuration and selectors. It can be referenced in
// the device requests of a claim to apply these presets.
// Cluster scoped.
//
// This is an alpha type and requires enabling the DynamicResourceAllocation
// feature gate.
type DeviceClass struct {
	metav1.TypeMeta `json:",inline"`
	// Standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	// Spec defines what can be allocated and how to configure it.
	//
	// This is mutable. Consumers have to be prepared for classes changing
	// at any time, either because they get updated or replaced. Claim
	// allocations are done once based on whatever was set in classes at
	// the time of allocation.
	//
	// Changing the spec automatically increments the metadata.generation number.
	Spec DeviceClassSpec `json:"spec" protobuf:"bytes,2,name=spec"`
}

// DeviceClassSpec is used in a [DeviceClass] to define what can be allocated
// and how to configure it.
type DeviceClassSpec struct {
	// Each selector must be satisfied by a device which is claimed via this class.
	//
	// +optional
	// +listType=atomic
	Selectors []DeviceSelector `json:"selectors,omitempty" protobuf:"bytes,1,opt,name=selectors"`

	// Config defines configuration parameters that apply to each device that is claimed via this class.
	// Some classses may potentially be satisfied by multiple drivers, so each instance of a vendor
	// configuration applies to exactly one driver.
	//
	// They are passed to the driver, but are not considered while allocating the claim.
	//
	// +optional
	// +listType=atomic
	Config []DeviceClassConfiguration `json:"config,omitempty" protobuf:"bytes,2,opt,name=config"`

	// SuitableNodes is tombstoned since Kubernetes 1.32 where
	// it got removed. May be reused once decoding v1alpha3 is no longer
	// supported.
	// SuitableNodes *v1.NodeSelector `json:"suitableNodes,omitempty" protobuf:"bytes,3,opt,name=suitableNodes"`
}

// DeviceClassConfiguration is used in DeviceClass.
type DeviceClassConfiguration struct {
	DeviceConfiguration `json:",inline" protobuf:"bytes,1,opt,name=deviceConfiguration"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:prerelease-lifecycle-gen:introduced=1.31
// +k8s:prerelease-lifecycle-gen:replacement=resource.k8s.io,v1beta1,DeviceClassList

// DeviceClassList is a collection of classes.
type DeviceClassList struct {
	metav1.TypeMeta `json:",inline"`
	// Standard list metadata
	// +optional
	metav1.ListMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	// Items is the list of resource classes.
	Items []DeviceClass `json:"items" protobuf:"bytes,2,rep,name=items"`
}

// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:prerelease-lifecycle-gen:introduced=1.31
// +k8s:prerelease-lifecycle-gen:replacement=resource.k8s.io,v1beta1,ResourceClaimTemplate

// ResourceClaimTemplate is used to produce ResourceClaim objects.
//
// This is an alpha type and requires enabling the DynamicResourceAllocation
// feature gate.
type ResourceClaimTemplate struct {
	metav1.TypeMeta `json:",inline"`
	// Standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	// Describes the ResourceClaim that is to be generated.
	//
	// This field is immutable. A ResourceClaim will get created by the
	// control plane for a Pod when needed and then not get updated
	// anymore.
	Spec ResourceClaimTemplateSpec `json:"spec" protobuf:"bytes,2,name=spec"`
}

// ResourceClaimTemplateSpec contains the metadata and fields for a ResourceClaim.
type ResourceClaimTemplateSpec struct {
	// ObjectMeta may contain labels and annotations that will be copied into the ResourceClaim
	// when creating it. No other fields are allowed and will be rejected during
	// validation.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	// Spec for the ResourceClaim. The entire content is copied unchanged
	// into the ResourceClaim that gets created from this template. The
	// same fields as in a ResourceClaim are also valid here.
	Spec ResourceClaimSpec `json:"spec" protobuf:"bytes,2,name=spec"`
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:prerelease-lifecycle-gen:introduced=1.31
// +k8s:prerelease-lifecycle-gen:replacement=resource.k8s.io,v1beta1,ResourceClaimTemplateList

// ResourceClaimTemplateList is a collection of claim templates.
type ResourceClaimTemplateList struct {
	metav1.TypeMeta `json:",inline"`
	// Standard list metadata
	// +optional
	metav1.ListMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`

	// Items is the list of resource claim templates.
	Items []ResourceClaimTemplate `json:"items" protobuf:"bytes,2,rep,name=items"`
}

// AllocatedDeviceStatus contains the status of an allocated device, if the
// driver chooses to report it. This may include driver-specific information.
type AllocatedDeviceStatus struct {
	// Driver specifies the name of the DRA driver whose kubelet
	// plugin should be invoked to process the allocation once the claim is
	// needed on a node.
	//
	// Must be a DNS subdomain and should end with a DNS domain owned by the
	// vendor of the driver.
	//
	// +required
	Driver string `json:"driver" protobuf:"bytes,1,rep,name=driver"`

	// This name together with the driver name and the device name field
	// identify which device was allocated (`<driver name>/<pool name>/<device name>`).
	//
	// Must not be longer than 253 characters and may contain one or more
	// DNS sub-domains separated by slashes.
	//
	// +required
	Pool string `json:"pool" protobuf:"bytes,2,rep,name=pool"`

	// Device references one device instance via its name in the driver's
	// resource pool. It must be a DNS label.
	//
	// +required
	Device string `json:"device" protobuf:"bytes,3,rep,name=device"`

	// Conditions contains the latest observation of the device's state.
	// If the device has been configured according to the class and claim
	// config references, the `Ready` condition should be True.
	//
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions" protobuf:"bytes,4,opt,name=conditions"`

	// Data contains arbitrary driver-specific data.
	//
	// The length of the raw data must be smaller or equal to 10 Ki.
	//
	// +optional
	Data runtime.RawExtension `json:"data,omitempty" protobuf:"bytes,5,opt,name=data"`

	// NetworkData contains network-related information specific to the device.
	//
	// +optional
	NetworkData *NetworkDeviceData `json:"networkData,omitempty" protobuf:"bytes,6,opt,name=networkData"`
}

// NetworkDeviceData provides network-related details for the allocated device.
// This information may be filled by drivers or other components to configure
// or identify the device within a network context.
type NetworkDeviceData struct {
	// InterfaceName specifies the name of the network interface associated with
	// the allocated device. This might be the name of a physical or virtual
	// network interface being configured in the pod.
	//
	// Must not be longer than 256 characters.
	//
	// +optional
	InterfaceName string `json:"interfaceName,omitempty" protobuf:"bytes,1,opt,name=interfaceName"`

	// IPs lists the network addresses assigned to the device's network interface.
	// This can include both IPv4 and IPv6 addresses.
	// The IPs are in the CIDR notation, which includes both the address and the
	// associated subnet mask.
	// e.g.: "192.0.2.5/24" for IPv4 and "2001:db8::5/64" for IPv6.
	//
	// +optional
	// +listType=atomic
	IPs []string `json:"ips,omitempty" protobuf:"bytes,2,opt,name=ips"`

	// HardwareAddress represents the hardware address (e.g. MAC Address) of the device's network interface.
	//
	// Must not be longer than 128 characters.
	//
	// +optional
	HardwareAddress string `json:"hardwareAddress,omitempty" protobuf:"bytes,3,opt,name=hardwareAddress"`
}
