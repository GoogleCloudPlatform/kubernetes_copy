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

package validation

import (
	"fmt"
	"reflect"
	"strings"
	"time"

	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apimachineryvalidation "k8s.io/apimachinery/pkg/api/validation"
	metav1validation "k8s.io/apimachinery/pkg/apis/meta/v1/validation"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/validation/field"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	api "k8s.io/kubernetes/pkg/apis/core"
	"k8s.io/kubernetes/pkg/apis/core/helper"
	apivalidation "k8s.io/kubernetes/pkg/apis/core/validation"
	"k8s.io/kubernetes/pkg/apis/storage"
	"k8s.io/kubernetes/pkg/features"
)

const (
	maxProvisionerParameterSize = 256 * (1 << 10) // 256 kB
	maxProvisionerParameterLen  = 512

	maxAttachedVolumeMetadataSize = 256 * (1 << 10) // 256 kB
	maxVolumeErrorMessageSize     = 1024

	csiNodeIDMaxLength       = 192
	csiNodeIDLongerMaxLength = 256
)

// CSINodeValidationOptions contains the validation options for validating CSINode
type CSINodeValidationOptions struct {
	AllowLongNodeID bool
}

// ValidateStorageClass validates a StorageClass.
func ValidateStorageClass(storageClass *storage.StorageClass) field.ErrorList {
	allErrs := apivalidation.ValidateObjectMeta(&storageClass.ObjectMeta, false, apivalidation.ValidateClassName, field.NewPath("metadata"))
	allErrs = append(allErrs, validateProvisioner(storageClass.Provisioner, field.NewPath("provisioner"))...)
	allErrs = append(allErrs, validateParameters(storageClass.Parameters, true, field.NewPath("parameters"))...)
	allErrs = append(allErrs, validateReclaimPolicy(storageClass.ReclaimPolicy, field.NewPath("reclaimPolicy"))...)
	allErrs = append(allErrs, validateVolumeBindingMode(storageClass.VolumeBindingMode, field.NewPath("volumeBindingMode"))...)
	allErrs = append(allErrs, validateAllowedTopologies(storageClass.AllowedTopologies, field.NewPath("allowedTopologies"))...)

	return allErrs
}

// ValidateStorageClassUpdate tests if an update to StorageClass is valid.
func ValidateStorageClassUpdate(storageClass, oldStorageClass *storage.StorageClass) field.ErrorList {
	allErrs := apivalidation.ValidateObjectMetaUpdate(&storageClass.ObjectMeta, &oldStorageClass.ObjectMeta, field.NewPath("metadata"))
	if !reflect.DeepEqual(oldStorageClass.Parameters, storageClass.Parameters) {
		allErrs = append(allErrs, field.Forbidden(field.NewPath("parameters"), "updates to parameters are forbidden."))
	}

	if storageClass.Provisioner != oldStorageClass.Provisioner {
		allErrs = append(allErrs, field.Forbidden(field.NewPath("provisioner"), "updates to provisioner are forbidden."))
	}

	if *storageClass.ReclaimPolicy != *oldStorageClass.ReclaimPolicy {
		allErrs = append(allErrs, field.Forbidden(field.NewPath("reclaimPolicy"), "updates to reclaimPolicy are forbidden."))
	}

	allErrs = append(allErrs, apivalidation.ValidateImmutableField(storageClass.VolumeBindingMode, oldStorageClass.VolumeBindingMode, field.NewPath("volumeBindingMode"))...)
	return allErrs
}

// validateProvisioner tests if provisioner is a valid qualified name.
func validateProvisioner(provisioner string, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if len(provisioner) == 0 {
		allErrs = append(allErrs, field.Required(fldPath, provisioner))
	}
	if len(provisioner) > 0 {
		allErrs = append(allErrs, apivalidation.ValidateQualifiedName(strings.ToLower(provisioner), fldPath)...)
	}
	return allErrs
}

// validateParameters tests that keys are qualified names and that provisionerParameter are < 256kB.
func validateParameters(params map[string]string, allowEmpty bool, fldPath *field.Path) field.ErrorList {
	var totalSize int64
	allErrs := field.ErrorList{}

	if len(params) > maxProvisionerParameterLen {
		allErrs = append(allErrs, field.TooLong(fldPath, "" /*unused*/, maxProvisionerParameterLen))
		return allErrs
	}

	for k, v := range params {
		if len(k) < 1 {
			allErrs = append(allErrs, field.Invalid(fldPath, k, "field can not be empty."))
		}
		totalSize += (int64)(len(k)) + (int64)(len(v))
	}

	if totalSize > maxProvisionerParameterSize {
		allErrs = append(allErrs, field.TooLong(fldPath, "" /*unused*/, maxProvisionerParameterSize))
	}

	if !allowEmpty && len(params) == 0 {
		allErrs = append(allErrs, field.Required(fldPath, "must contain at least one key/value pair"))
	}
	return allErrs
}

var supportedReclaimPolicy = sets.NewString(string(api.PersistentVolumeReclaimDelete), string(api.PersistentVolumeReclaimRetain))

// validateReclaimPolicy tests that the reclaim policy is one of the supported. It is up to the volume plugin to reject
// provisioning for storage classes with impossible reclaim policies, e.g. EBS is not Recyclable
func validateReclaimPolicy(reclaimPolicy *api.PersistentVolumeReclaimPolicy, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if len(string(*reclaimPolicy)) > 0 {
		if !supportedReclaimPolicy.Has(string(*reclaimPolicy)) {
			allErrs = append(allErrs, field.NotSupported(fldPath, reclaimPolicy, supportedReclaimPolicy.List()))
		}
	}
	return allErrs
}

// ValidateVolumeAttachment validates a VolumeAttachment. This function is common for v1 and v1beta1 objects,
func ValidateVolumeAttachment(volumeAttachment *storage.VolumeAttachment) field.ErrorList {
	allErrs := apivalidation.ValidateObjectMeta(&volumeAttachment.ObjectMeta, false, apivalidation.ValidateClassName, field.NewPath("metadata"))
	allErrs = append(allErrs, validateVolumeAttachmentSpec(&volumeAttachment.Spec, field.NewPath("spec"))...)
	allErrs = append(allErrs, validateVolumeAttachmentStatus(&volumeAttachment.Status, field.NewPath("status"))...)
	return allErrs
}

// ValidateVolumeAttachmentV1 validates a v1/VolumeAttachment. It contains only extra checks missing in
// ValidateVolumeAttachment.
func ValidateVolumeAttachmentV1(volumeAttachment *storage.VolumeAttachment) field.ErrorList {
	allErrs := apivalidation.ValidateCSIDriverName(volumeAttachment.Spec.Attacher, field.NewPath("spec.attacher"))

	if volumeAttachment.Spec.Source.PersistentVolumeName != nil {
		pvName := *volumeAttachment.Spec.Source.PersistentVolumeName
		for _, msg := range apivalidation.ValidatePersistentVolumeName(pvName, false) {
			allErrs = append(allErrs, field.Invalid(field.NewPath("spec.source.persistentVolumeName"), pvName, msg))
		}
	}
	return allErrs
}

// ValidateVolumeAttachmentSpec tests that the specified VolumeAttachmentSpec
// has valid data.
func validateVolumeAttachmentSpec(
	spec *storage.VolumeAttachmentSpec, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	allErrs = append(allErrs, validateAttacher(spec.Attacher, fldPath.Child("attacher"))...)
	allErrs = append(allErrs, validateVolumeAttachmentSource(&spec.Source, fldPath.Child("source"))...)
	allErrs = append(allErrs, validateNodeName(spec.NodeName, fldPath.Child("nodeName"))...)
	return allErrs
}

// validateAttacher tests if attacher is a valid qualified name.
func validateAttacher(attacher string, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if len(attacher) == 0 {
		allErrs = append(allErrs, field.Required(fldPath, attacher))
	}
	return allErrs
}

// validateSource tests if the source is valid for VolumeAttachment.
func validateVolumeAttachmentSource(source *storage.VolumeAttachmentSource, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	switch {
	case source.InlineVolumeSpec == nil && source.PersistentVolumeName == nil:
		allErrs = append(allErrs, field.Required(fldPath, "must specify exactly one of inlineVolumeSpec and persistentVolumeName"))
	case source.InlineVolumeSpec != nil && source.PersistentVolumeName != nil:
		allErrs = append(allErrs, field.Forbidden(fldPath, "must specify exactly one of inlineVolumeSpec and persistentVolumeName"))
	case source.PersistentVolumeName != nil:
		if len(*source.PersistentVolumeName) == 0 {
			// Invalid err
			allErrs = append(allErrs, field.Required(fldPath.Child("persistentVolumeName"), "must specify non empty persistentVolumeName"))
		}
	case source.InlineVolumeSpec != nil:
		opts := apivalidation.PersistentVolumeSpecValidationOptions{}
		allErrs = append(allErrs, apivalidation.ValidatePersistentVolumeSpec(source.InlineVolumeSpec, "", true, fldPath.Child("inlineVolumeSpec"), opts)...)
	}
	return allErrs
}

// validateNodeName tests if the nodeName is valid for VolumeAttachment.
func validateNodeName(nodeName string, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	for _, msg := range apivalidation.ValidateNodeName(nodeName, false /* prefix */) {
		allErrs = append(allErrs, field.Invalid(fldPath, nodeName, msg))
	}
	return allErrs
}

// validaVolumeAttachmentStatus tests if volumeAttachmentStatus is valid.
func validateVolumeAttachmentStatus(status *storage.VolumeAttachmentStatus, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	allErrs = append(allErrs, validateAttachmentMetadata(status.AttachmentMetadata, fldPath.Child("attachmentMetadata"))...)
	allErrs = append(allErrs, validateVolumeError(status.AttachError, fldPath.Child("attachError"))...)
	allErrs = append(allErrs, validateVolumeError(status.DetachError, fldPath.Child("detachError"))...)
	return allErrs
}

func validateAttachmentMetadata(metadata map[string]string, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}

	var size int64
	for k, v := range metadata {
		size += (int64)(len(k)) + (int64)(len(v))
	}
	if size > maxAttachedVolumeMetadataSize {
		allErrs = append(allErrs, field.TooLong(fldPath, "" /*unused*/, maxAttachedVolumeMetadataSize))
	}
	return allErrs
}

func validateVolumeError(e *storage.VolumeError, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}

	if e == nil {
		return allErrs
	}
	if len(e.Message) > maxVolumeErrorMessageSize {
		allErrs = append(allErrs, field.TooLong(fldPath.Child("message"), "" /*unused*/, maxAttachedVolumeMetadataSize))
	}
	return allErrs
}

// ValidateVolumeAttachmentUpdate validates a VolumeAttachment.
func ValidateVolumeAttachmentUpdate(new, old *storage.VolumeAttachment) field.ErrorList {
	allErrs := ValidateVolumeAttachment(new)

	// Spec is read-only
	// If this ever relaxes in the future, make sure to increment the Generation number in PrepareForUpdate
	if !apiequality.Semantic.DeepEqual(old.Spec, new.Spec) {
		allErrs = append(allErrs, field.Invalid(field.NewPath("spec"), new.Spec, "field is immutable"))
	}
	return allErrs
}

var supportedVolumeBindingModes = sets.NewString(string(storage.VolumeBindingImmediate), string(storage.VolumeBindingWaitForFirstConsumer))

// validateVolumeBindingMode tests that VolumeBindingMode specifies valid values.
func validateVolumeBindingMode(mode *storage.VolumeBindingMode, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if mode == nil {
		allErrs = append(allErrs, field.Required(fldPath, ""))
	} else if !supportedVolumeBindingModes.Has(string(*mode)) {
		allErrs = append(allErrs, field.NotSupported(fldPath, mode, supportedVolumeBindingModes.List()))
	}

	return allErrs
}

// validateAllowedTopology tests that AllowedTopologies specifies valid values.
func validateAllowedTopologies(topologies []api.TopologySelectorTerm, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}

	if len(topologies) == 0 {
		return allErrs
	}

	rawTopologies := make([]map[string]sets.Set[string], 0, len(topologies))
	for i, term := range topologies {
		idxPath := fldPath.Index(i)
		exprMap, termErrs := apivalidation.ValidateTopologySelectorTerm(term, fldPath.Index(i))
		allErrs = append(allErrs, termErrs...)

		// TODO (verult) consider improving runtime
		for _, t := range rawTopologies {
			if helper.Semantic.DeepEqual(exprMap, t) {
				allErrs = append(allErrs, field.Duplicate(idxPath.Child("matchLabelExpressions"), ""))
			}
		}

		rawTopologies = append(rawTopologies, exprMap)
	}

	return allErrs
}

// ValidateCSINode validates a CSINode.
func ValidateCSINode(csiNode *storage.CSINode, validationOpts CSINodeValidationOptions) field.ErrorList {
	allErrs := apivalidation.ValidateObjectMeta(&csiNode.ObjectMeta, false, apivalidation.ValidateNodeName, field.NewPath("metadata"))
	allErrs = append(allErrs, validateCSINodeSpec(&csiNode.Spec, field.NewPath("spec"), validationOpts)...)
	return allErrs
}

// ValidateCSINodeUpdate validates a CSINode.
func ValidateCSINodeUpdate(new, old *storage.CSINode, validationOpts CSINodeValidationOptions) field.ErrorList {
	allErrs := ValidateCSINode(new, validationOpts)

	// Validate modifying fields inside an existing CSINodeDriver entry is not allowed
	for _, oldDriver := range old.Spec.Drivers {
		for _, newDriver := range new.Spec.Drivers {
			if oldDriver.Name == newDriver.Name {
				if !apiequality.Semantic.DeepEqual(oldDriver, newDriver) {
					allErrs = append(allErrs, field.Invalid(field.NewPath("CSINodeDriver"), newDriver, "field is immutable"))
				}
			}
		}
	}

	return allErrs
}

// ValidateCSINodeSpec tests that the specified CSINodeSpec has valid data.
func validateCSINodeSpec(
	spec *storage.CSINodeSpec, fldPath *field.Path, validationOpts CSINodeValidationOptions) field.ErrorList {
	allErrs := field.ErrorList{}
	allErrs = append(allErrs, validateCSINodeDrivers(spec.Drivers, fldPath.Child("drivers"), validationOpts)...)
	return allErrs
}

// ValidateCSINodeDrivers tests that the specified CSINodeDrivers have valid data.
func validateCSINodeDrivers(drivers []storage.CSINodeDriver, fldPath *field.Path, validationOpts CSINodeValidationOptions) field.ErrorList {
	allErrs := field.ErrorList{}
	driverNamesInSpecs := sets.New[string]()
	for i, driver := range drivers {
		idxPath := fldPath.Index(i)
		allErrs = append(allErrs, validateCSINodeDriver(driver, driverNamesInSpecs, idxPath, validationOpts)...)
	}

	return allErrs
}

// validateCSINodeDriverNodeID tests if Name in CSINodeDriver is a valid node id.
func validateCSINodeDriverNodeID(nodeID string, fldPath *field.Path, validationOpts CSINodeValidationOptions) field.ErrorList {
	allErrs := field.ErrorList{}

	// nodeID is always required
	if len(nodeID) == 0 {
		allErrs = append(allErrs, field.Required(fldPath, nodeID))
	}
	maxLength := csiNodeIDMaxLength
	if validationOpts.AllowLongNodeID {
		maxLength = csiNodeIDLongerMaxLength
	}
	if len(nodeID) > maxLength {
		allErrs = append(allErrs, field.Invalid(fldPath, nodeID, fmt.Sprintf("must be %d characters or less", maxLength)))
	}
	return allErrs
}

// validateCSINodeDriverAllocatable tests if Allocatable in CSINodeDriver has valid volume limits.
func validateCSINodeDriverAllocatable(a *storage.VolumeNodeResources, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}

	if a == nil || a.Count == nil {
		return allErrs
	}

	allErrs = append(allErrs, apivalidation.ValidateNonnegativeField(int64(*a.Count), fldPath.Child("count"))...)
	return allErrs
}

// validateCSINodeDriver tests if CSINodeDriver has valid entries
func validateCSINodeDriver(driver storage.CSINodeDriver, driverNamesInSpecs sets.Set[string], fldPath *field.Path,
	validationOpts CSINodeValidationOptions) field.ErrorList {
	allErrs := field.ErrorList{}

	allErrs = append(allErrs, apivalidation.ValidateCSIDriverName(driver.Name, fldPath.Child("name"))...)
	allErrs = append(allErrs, validateCSINodeDriverNodeID(driver.NodeID, fldPath.Child("nodeID"), validationOpts)...)
	allErrs = append(allErrs, validateCSINodeDriverAllocatable(driver.Allocatable, fldPath.Child("allocatable"))...)

	// check for duplicate entries for the same driver in specs
	if driverNamesInSpecs.Has(driver.Name) {
		allErrs = append(allErrs, field.Duplicate(fldPath.Child("name"), driver.Name))
	}
	driverNamesInSpecs.Insert(driver.Name)
	topoKeys := sets.New[string]()
	for _, key := range driver.TopologyKeys {
		if len(key) == 0 {
			allErrs = append(allErrs, field.Required(fldPath, key))
		}

		if topoKeys.Has(key) {
			allErrs = append(allErrs, field.Duplicate(fldPath, key))
		}
		topoKeys.Insert(key)

		allErrs = append(allErrs, apivalidation.ValidateQualifiedName(key, fldPath)...)
	}

	return allErrs
}

// ValidateCSIDriverName checks that a name is appropriate for a
// CSIDriver object.
var ValidateCSIDriverName = apimachineryvalidation.NameIsDNSSubdomain

// ValidateCSIDriver validates a CSIDriver.
func ValidateCSIDriver(csiDriver *storage.CSIDriver) field.ErrorList {
	allErrs := apivalidation.ValidateObjectMeta(&csiDriver.ObjectMeta, false, ValidateCSIDriverName, field.NewPath("metadata"))

	allErrs = append(allErrs, validateCSIDriverSpec(&csiDriver.Spec, field.NewPath("spec"))...)
	return allErrs
}

// ValidateCSIDriverUpdate validates a CSIDriver.
func ValidateCSIDriverUpdate(new, old *storage.CSIDriver) field.ErrorList {
	allErrs := apivalidation.ValidateObjectMetaUpdate(&new.ObjectMeta, &old.ObjectMeta, field.NewPath("metadata"))
	allErrs = append(allErrs, validateCSIDriverSpec(&new.Spec, field.NewPath("spec"))...)

	// immutable fields should not be mutated.
	allErrs = append(allErrs, apimachineryvalidation.ValidateImmutableField(new.Spec.AttachRequired, old.Spec.AttachRequired, field.NewPath("spec", "attachedRequired"))...)
	allErrs = append(allErrs, apimachineryvalidation.ValidateImmutableField(new.Spec.VolumeLifecycleModes, old.Spec.VolumeLifecycleModes, field.NewPath("spec", "volumeLifecycleModes"))...)

	return allErrs
}

// ValidateCSIDriverSpec tests that the specified CSIDriverSpec
// has valid data.
func validateCSIDriverSpec(
	spec *storage.CSIDriverSpec, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	allErrs = append(allErrs, validateAttachRequired(spec.AttachRequired, fldPath.Child("attachedRequired"))...)
	allErrs = append(allErrs, validatePodInfoOnMount(spec.PodInfoOnMount, fldPath.Child("podInfoOnMount"))...)
	allErrs = append(allErrs, validateStorageCapacity(spec.StorageCapacity, fldPath.Child("storageCapacity"))...)
	allErrs = append(allErrs, validateFSGroupPolicy(spec.FSGroupPolicy, fldPath.Child("fsGroupPolicy"))...)
	allErrs = append(allErrs, validateTokenRequests(spec.TokenRequests, fldPath.Child("tokenRequests"))...)
	allErrs = append(allErrs, validateVolumeLifecycleModes(spec.VolumeLifecycleModes, fldPath.Child("volumeLifecycleModes"))...)
	allErrs = append(allErrs, validateSELinuxMount(spec.SELinuxMount, fldPath.Child("seLinuxMount"))...)
	return allErrs
}

// validateAttachRequired tests if attachRequired is set for CSIDriver.
func validateAttachRequired(attachRequired *bool, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if attachRequired == nil {
		allErrs = append(allErrs, field.Required(fldPath, ""))
	}

	return allErrs
}

// validatePodInfoOnMount tests if podInfoOnMount is set for CSIDriver.
func validatePodInfoOnMount(podInfoOnMount *bool, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if podInfoOnMount == nil {
		allErrs = append(allErrs, field.Required(fldPath, ""))
	}

	return allErrs
}

// validateStorageCapacity tests if storageCapacity is set for CSIDriver.
func validateStorageCapacity(storageCapacity *bool, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if storageCapacity == nil {
		allErrs = append(allErrs, field.Required(fldPath, ""))
	}

	return allErrs
}

var supportedFSGroupPolicy = sets.NewString(string(storage.ReadWriteOnceWithFSTypeFSGroupPolicy), string(storage.FileFSGroupPolicy), string(storage.NoneFSGroupPolicy))

// validateFSGroupPolicy tests if FSGroupPolicy contains an appropriate value.
func validateFSGroupPolicy(fsGroupPolicy *storage.FSGroupPolicy, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if fsGroupPolicy == nil {
		// This is not a required field, so if nothing is provided simply return
		return allErrs
	}

	if !supportedFSGroupPolicy.Has(string(*fsGroupPolicy)) {
		allErrs = append(allErrs, field.NotSupported(fldPath, fsGroupPolicy, supportedFSGroupPolicy.List()))
	}

	return allErrs
}

// validateTokenRequests tests if the Audience in each TokenRequest are different.
// Besides, at most one TokenRequest can ignore Audience.
func validateTokenRequests(tokenRequests []storage.TokenRequest, fldPath *field.Path) field.ErrorList {
	const min = 10 * time.Minute
	allErrs := field.ErrorList{}
	audiences := make(map[string]bool)
	for i, tokenRequest := range tokenRequests {
		path := fldPath.Index(i)
		audience := tokenRequest.Audience
		if _, ok := audiences[audience]; ok {
			allErrs = append(allErrs, field.Duplicate(path.Child("audience"), audience))
			continue
		}
		audiences[audience] = true

		if tokenRequest.ExpirationSeconds == nil {
			continue
		}
		if *tokenRequest.ExpirationSeconds < int64(min.Seconds()) {
			allErrs = append(allErrs, field.Invalid(path.Child("expirationSeconds"), *tokenRequest.ExpirationSeconds, "may not specify a duration less than 10 minutes"))
		}
		if *tokenRequest.ExpirationSeconds > 1<<32 {
			allErrs = append(allErrs, field.Invalid(path.Child("expirationSeconds"), *tokenRequest.ExpirationSeconds, "may not specify a duration larger than 2^32 seconds"))
		}
	}

	return allErrs
}

// validateVolumeLifecycleModes tests if mode has one of the allowed values.
func validateVolumeLifecycleModes(modes []storage.VolumeLifecycleMode, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	for _, mode := range modes {
		switch mode {
		case storage.VolumeLifecyclePersistent, storage.VolumeLifecycleEphemeral:
		default:
			allErrs = append(allErrs, field.NotSupported(fldPath, mode,
				[]string{
					string(storage.VolumeLifecyclePersistent),
					string(storage.VolumeLifecycleEphemeral),
				}))
		}
	}

	return allErrs
}

// validateSELinuxMount tests if seLinuxMount is set for CSIDriver.
func validateSELinuxMount(seLinuxMount *bool, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if seLinuxMount == nil && utilfeature.DefaultFeatureGate.Enabled(features.SELinuxMountReadWriteOncePod) {
		allErrs = append(allErrs, field.Required(fldPath, ""))
	}

	return allErrs
}

// ValidateStorageCapacityName checks that a name is appropriate for a
// CSIStorageCapacity object.
var ValidateStorageCapacityName = apimachineryvalidation.NameIsDNSSubdomain

type CSIStorageCapacityValidateOptions struct {
	AllowInvalidLabelValueInSelector bool
}

// ValidateCSIStorageCapacity validates a CSIStorageCapacity.
func ValidateCSIStorageCapacity(capacity *storage.CSIStorageCapacity, opts CSIStorageCapacityValidateOptions) field.ErrorList {
	allErrs := apivalidation.ValidateObjectMeta(&capacity.ObjectMeta, true, ValidateStorageCapacityName, field.NewPath("metadata"))
	labelSelectorValidationOptions := metav1validation.LabelSelectorValidationOptions{AllowInvalidLabelValueInSelector: opts.AllowInvalidLabelValueInSelector}
	allErrs = append(allErrs, metav1validation.ValidateLabelSelector(capacity.NodeTopology, labelSelectorValidationOptions, field.NewPath("nodeTopology"))...)
	for _, msg := range apivalidation.ValidateClassName(capacity.StorageClassName, false) {
		allErrs = append(allErrs, field.Invalid(field.NewPath("storageClassName"), capacity.StorageClassName, msg))
	}
	if capacity.Capacity != nil {
		allErrs = append(allErrs, apivalidation.ValidateNonnegativeQuantity(*capacity.Capacity, field.NewPath("capacity"))...)
	}
	return allErrs
}

// ValidateCSIStorageCapacityUpdate tests if an update to CSIStorageCapacity is valid.
func ValidateCSIStorageCapacityUpdate(capacity, oldCapacity *storage.CSIStorageCapacity) field.ErrorList {
	allErrs := apivalidation.ValidateObjectMetaUpdate(&capacity.ObjectMeta, &oldCapacity.ObjectMeta, field.NewPath("metadata"))

	// Input fields for CSI GetCapacity are immutable.
	// If this ever relaxes in the future, make sure to increment the Generation number in PrepareForUpdate
	if !apiequality.Semantic.DeepEqual(capacity.NodeTopology, oldCapacity.NodeTopology) {
		allErrs = append(allErrs, field.Invalid(field.NewPath("nodeTopology"), capacity.NodeTopology, "field is immutable"))
	}
	if capacity.StorageClassName != oldCapacity.StorageClassName {
		allErrs = append(allErrs, field.Invalid(field.NewPath("storageClassName"), capacity.StorageClassName, "field is immutable"))
	}

	return allErrs
}

// ValidateVolumeAttributesClass validates a VolumeAttributesClass.
func ValidateVolumeAttributesClass(volumeAttributesClass *storage.VolumeAttributesClass) field.ErrorList {
	allErrs := apivalidation.ValidateObjectMeta(&volumeAttributesClass.ObjectMeta, false, apivalidation.ValidateClassName, field.NewPath("metadata"))
	allErrs = append(allErrs, validateProvisioner(volumeAttributesClass.DriverName, field.NewPath("driverName"))...)
	allErrs = append(allErrs, validateParameters(volumeAttributesClass.Parameters, false, field.NewPath("parameters"))...)
	return allErrs
}

// ValidateVolumeAttributesClassUpdate tests if an update to VolumeAttributesClass is valid.
func ValidateVolumeAttributesClassUpdate(volumeAttributesClass, oldVolumeAttributesClass *storage.VolumeAttributesClass) field.ErrorList {
	allErrs := apivalidation.ValidateObjectMetaUpdate(&volumeAttributesClass.ObjectMeta, &oldVolumeAttributesClass.ObjectMeta, field.NewPath("metadata"))
	if volumeAttributesClass.DriverName != oldVolumeAttributesClass.DriverName {
		allErrs = append(allErrs, field.Forbidden(field.NewPath("driverName"), "updates to driverName are forbidden."))
	}
	if !reflect.DeepEqual(oldVolumeAttributesClass.Parameters, volumeAttributesClass.Parameters) {
		allErrs = append(allErrs, field.Forbidden(field.NewPath("parameters"), "updates to parameters are forbidden."))
	}
	allErrs = append(allErrs, ValidateVolumeAttributesClass(volumeAttributesClass)...)
	return allErrs
}
