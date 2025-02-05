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

package validation

import (
	"encoding/json"
	"fmt"
	"math"
	"net"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/google/go-cmp/cmp" //nolint:depguard
	netutils "k8s.io/utils/net"

	v1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/resource"
	apimachineryvalidation "k8s.io/apimachinery/pkg/api/validation"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	unversionedvalidation "k8s.io/apimachinery/pkg/apis/meta/v1/validation"
	"k8s.io/apimachinery/pkg/conversion"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	utilsysctl "k8s.io/component-helpers/node/util/sysctl"
	resourcehelper "k8s.io/component-helpers/resource"
	schedulinghelper "k8s.io/component-helpers/scheduling/corev1"
	kubeletapis "k8s.io/kubelet/pkg/apis"

	apiservice "k8s.io/kubernetes/pkg/api/service"
	"k8s.io/kubernetes/pkg/apis/core"
	"k8s.io/kubernetes/pkg/apis/core/helper"
	"k8s.io/kubernetes/pkg/apis/core/helper/qos"
	podshelper "k8s.io/kubernetes/pkg/apis/core/pods"
	corev1 "k8s.io/kubernetes/pkg/apis/core/v1"
	"k8s.io/kubernetes/pkg/capabilities"
	"k8s.io/kubernetes/pkg/features"
	"k8s.io/kubernetes/pkg/fieldpath"
)

const isNegativeErrorMsg string = apimachineryvalidation.IsNegativeErrorMsg
const isInvalidQuotaResource string = `must be a standard resource for quota`
const fieldImmutableErrorMsg string = apimachineryvalidation.FieldImmutableErrorMsg
const isNotIntegerErrorMsg string = `must be an integer`
const isNotPositiveErrorMsg string = `must be greater than zero`

var pdPartitionErrorMsg string = validation.InclusiveRangeError(1, 255)
var fileModeErrorMsg = "must be a number between 0 and 0777 (octal), both inclusive"

// BannedOwners is a black list of object that are not allowed to be owners.
var BannedOwners = apimachineryvalidation.BannedOwners

var iscsiInitiatorIqnRegex = regexp.MustCompile(`iqn\.\d{4}-\d{2}\.([[:alnum:]-.]+)(:[^,;*&$|\s]+)$`)
var iscsiInitiatorEuiRegex = regexp.MustCompile(`^eui.[[:alnum:]]{16}$`)
var iscsiInitiatorNaaRegex = regexp.MustCompile(`^naa.[[:alnum:]]{32}$`)

var allowedEphemeralContainerFields = map[string]bool{
	"Name":                     true,
	"Image":                    true,
	"Command":                  true,
	"Args":                     true,
	"WorkingDir":               true,
	"Ports":                    false,
	"EnvFrom":                  true,
	"Env":                      true,
	"Resources":                false,
	"VolumeMounts":             true,
	"VolumeDevices":            true,
	"LivenessProbe":            false,
	"ReadinessProbe":           false,
	"StartupProbe":             false,
	"Lifecycle":                false,
	"TerminationMessagePath":   true,
	"TerminationMessagePolicy": true,
	"ImagePullPolicy":          true,
	"SecurityContext":          true,
	"Stdin":                    true,
	"StdinOnce":                true,
	"TTY":                      true,
}

// validOS stores the set of valid OSes within pod spec.
// The valid values currently are linux, windows.
// In future, they can be expanded to values from
// https://github.com/opencontainers/runtime-spec/blob/master/config.md#platform-specific-configuration
var validOS = sets.New(core.Linux, core.Windows)

// ValidateHasLabel requires that metav1.ObjectMeta has a Label with key and expectedValue
func ValidateHasLabel(meta metav1.ObjectMeta, fldPath *field.Path, key, expectedValue string) field.ErrorList {
	allErrs := field.ErrorList{}
	actualValue, found := meta.Labels[key]
	if !found {
		allErrs = append(allErrs, field.Required(fldPath.Child("labels").Key(key),
			fmt.Sprintf("must be '%s'", expectedValue)))
		return allErrs
	}
	if actualValue != expectedValue {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("labels").Key(key), meta.Labels,
			fmt.Sprintf("must be '%s'", expectedValue)))
	}
	return allErrs
}

// ValidateAnnotations validates that a set of annotations are correctly defined.
func ValidateAnnotations(annotations map[string]string, fldPath *field.Path) field.ErrorList {
	return apimachineryvalidation.ValidateAnnotations(annotations, fldPath)
}

func ValidateDNS1123Label(value string, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	for _, msg := range validation.IsDNS1123Label(value) {
		allErrs = append(allErrs, field.Invalid(fldPath, value, msg))
	}
	return allErrs
}

// ValidateQualifiedName validates if name is what Kubernetes calls a "qualified name".
func ValidateQualifiedName(value string, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	for _, msg := range validation.IsQualifiedName(value) {
		allErrs = append(allErrs, field.Invalid(fldPath, value, msg))
	}
	return allErrs
}

// ValidateDNS1123SubdomainWithUnderScore validates that a name is a proper DNS subdomain but allows for an underscore in the string
func ValidateDNS1123SubdomainWithUnderScore(value string, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	for _, msg := range validation.IsDNS1123SubdomainWithUnderscore(value) {
		allErrs = append(allErrs, field.Invalid(fldPath, value, msg))
	}
	return allErrs
}

// ValidateDNS1123Subdomain validates that a name is a proper DNS subdomain.
func ValidateDNS1123Subdomain(value string, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	for _, msg := range validation.IsDNS1123Subdomain(value) {
		allErrs = append(allErrs, field.Invalid(fldPath, value, msg))
	}
	return allErrs
}

func ValidatePodSpecificAnnotations(annotations map[string]string, spec *core.PodSpec, fldPath *field.Path, opts PodValidationOptions) field.ErrorList {
	allErrs := field.ErrorList{}

	if value, isMirror := annotations[core.MirrorPodAnnotationKey]; isMirror {
		if len(spec.NodeName) == 0 {
			allErrs = append(allErrs, field.Invalid(fldPath.Key(core.MirrorPodAnnotationKey), value, "must set spec.nodeName if mirror pod annotation is set"))
		}
	}

	if annotations[core.TolerationsAnnotationKey] != "" {
		allErrs = append(allErrs, ValidateTolerationsInPodAnnotations(annotations, fldPath)...)
	}

	if !opts.AllowInvalidPodDeletionCost {
		if _, err := helper.GetDeletionCostFromPodAnnotations(annotations); err != nil {
			allErrs = append(allErrs, field.Invalid(fldPath.Key(core.PodDeletionCost), annotations[core.PodDeletionCost], "must be a 32bit integer"))
		}
	}

	allErrs = append(allErrs, ValidateSeccompPodAnnotations(annotations, fldPath)...)
	allErrs = append(allErrs, ValidateAppArmorPodAnnotations(annotations, spec, fldPath)...)

	return allErrs
}

// ValidateTolerationsInPodAnnotations tests that the serialized tolerations in Pod.Annotations has valid data
func ValidateTolerationsInPodAnnotations(annotations map[string]string, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}

	tolerations, err := helper.GetTolerationsFromPodAnnotations(annotations)
	if err != nil {
		allErrs = append(allErrs, field.Invalid(fldPath, core.TolerationsAnnotationKey, err.Error()))
		return allErrs
	}

	if len(tolerations) > 0 {
		allErrs = append(allErrs, ValidateTolerations(tolerations, fldPath.Child(core.TolerationsAnnotationKey))...)
	}

	return allErrs
}

func ValidatePodSpecificAnnotationUpdates(newPod, oldPod *core.Pod, fldPath *field.Path, opts PodValidationOptions) field.ErrorList {
	allErrs := field.ErrorList{}
	newAnnotations := newPod.Annotations
	oldAnnotations := oldPod.Annotations
	for k, oldVal := range oldAnnotations {
		if newVal, exists := newAnnotations[k]; exists && newVal == oldVal {
			continue // No change.
		}
		if strings.HasPrefix(k, v1.DeprecatedAppArmorBetaContainerAnnotationKeyPrefix) {
			allErrs = append(allErrs, field.Forbidden(fldPath.Key(k), "may not remove or update AppArmor annotations"))
		}
		if k == core.MirrorPodAnnotationKey {
			allErrs = append(allErrs, field.Forbidden(fldPath.Key(k), "may not remove or update mirror pod annotation"))
		}
	}
	// Check for additions
	for k := range newAnnotations {
		if _, ok := oldAnnotations[k]; ok {
			continue // No change.
		}
		if strings.HasPrefix(k, v1.DeprecatedAppArmorBetaContainerAnnotationKeyPrefix) {
			allErrs = append(allErrs, field.Forbidden(fldPath.Key(k), "may not add AppArmor annotations"))
		}
		if k == core.MirrorPodAnnotationKey {
			allErrs = append(allErrs, field.Forbidden(fldPath.Key(k), "may not add mirror pod annotation"))
		}
	}
	allErrs = append(allErrs, ValidatePodSpecificAnnotations(newAnnotations, &newPod.Spec, fldPath, opts)...)
	return allErrs
}

func ValidateEndpointsSpecificAnnotations(annotations map[string]string, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	return allErrs
}

// ValidateNameFunc validates that the provided name is valid for a given resource type.
// Not all resources have the same validation rules for names. Prefix is true
// if the name will have a value appended to it.  If the name is not valid,
// this returns a list of descriptions of individual characteristics of the
// value that were not valid.  Otherwise this returns an empty list or nil.
type ValidateNameFunc apimachineryvalidation.ValidateNameFunc

// ValidatePodName can be used to check whether the given pod name is valid.
// Prefix indicates this name will be used as part of generation, in which case
// trailing dashes are allowed.
var ValidatePodName = apimachineryvalidation.NameIsDNSSubdomain

// ValidateReplicationControllerName can be used to check whether the given replication
// controller name is valid.
// Prefix indicates this name will be used as part of generation, in which case
// trailing dashes are allowed.
var ValidateReplicationControllerName = apimachineryvalidation.NameIsDNSSubdomain

// ValidateServiceName can be used to check whether the given service name is valid.
// Prefix indicates this name will be used as part of generation, in which case
// trailing dashes are allowed.
var ValidateServiceName = apimachineryvalidation.NameIsDNS1035Label

// ValidateNodeName can be used to check whether the given node name is valid.
// Prefix indicates this name will be used as part of generation, in which case
// trailing dashes are allowed.
var ValidateNodeName = apimachineryvalidation.NameIsDNSSubdomain

// ValidateNamespaceName can be used to check whether the given namespace name is valid.
// Prefix indicates this name will be used as part of generation, in which case
// trailing dashes are allowed.
var ValidateNamespaceName = apimachineryvalidation.ValidateNamespaceName

// ValidateLimitRangeName can be used to check whether the given limit range name is valid.
// Prefix indicates this name will be used as part of generation, in which case
// trailing dashes are allowed.
var ValidateLimitRangeName = apimachineryvalidation.NameIsDNSSubdomain

// ValidateResourceQuotaName can be used to check whether the given
// resource quota name is valid.
// Prefix indicates this name will be used as part of generation, in which case
// trailing dashes are allowed.
var ValidateResourceQuotaName = apimachineryvalidation.NameIsDNSSubdomain

// ValidateSecretName can be used to check whether the given secret name is valid.
// Prefix indicates this name will be used as part of generation, in which case
// trailing dashes are allowed.
var ValidateSecretName = apimachineryvalidation.NameIsDNSSubdomain

// ValidateServiceAccountName can be used to check whether the given service account name is valid.
// Prefix indicates this name will be used as part of generation, in which case
// trailing dashes are allowed.
var ValidateServiceAccountName = apimachineryvalidation.ValidateServiceAccountName

// ValidateEndpointsName can be used to check whether the given endpoints name is valid.
// Prefix indicates this name will be used as part of generation, in which case
// trailing dashes are allowed.
var ValidateEndpointsName = apimachineryvalidation.NameIsDNSSubdomain

// ValidateClassName can be used to check whether the given class name is valid.
// It is defined here to avoid import cycle between pkg/apis/storage/validation
// (where it should be) and this file.
var ValidateClassName = apimachineryvalidation.NameIsDNSSubdomain

// ValidatePriorityClassName can be used to check whether the given priority
// class name is valid.
var ValidatePriorityClassName = apimachineryvalidation.NameIsDNSSubdomain

// ValidateResourceClaimName can be used to check whether the given
// name for a ResourceClaim is valid.
var ValidateResourceClaimName = apimachineryvalidation.NameIsDNSSubdomain

// ValidateResourceClaimTemplateName can be used to check whether the given
// name for a ResourceClaimTemplate is valid.
var ValidateResourceClaimTemplateName = apimachineryvalidation.NameIsDNSSubdomain

// ValidateRuntimeClassName can be used to check whether the given RuntimeClass name is valid.
// Prefix indicates this name will be used as part of generation, in which case
// trailing dashes are allowed.
func ValidateRuntimeClassName(name string, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	for _, msg := range apimachineryvalidation.NameIsDNSSubdomain(name, false) {
		allErrs = append(allErrs, field.Invalid(fldPath, name, msg))
	}
	return allErrs
}

// validateOverhead can be used to check whether the given Overhead is valid.
func validateOverhead(overhead core.ResourceList, fldPath *field.Path, opts PodValidationOptions) field.ErrorList {
	// reuse the ResourceRequirements validation logic
	return ValidateContainerResourceRequirements(&core.ResourceRequirements{Limits: overhead}, nil, fldPath, opts)
}

// Validates that given value is not negative.
func ValidateNonnegativeField(value int64, fldPath *field.Path) field.ErrorList {
	return apimachineryvalidation.ValidateNonnegativeField(value, fldPath)
}

// Validates that a Quantity is not negative
func ValidateNonnegativeQuantity(value resource.Quantity, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if value.Cmp(resource.Quantity{}) < 0 {
		allErrs = append(allErrs, field.Invalid(fldPath, value.String(), isNegativeErrorMsg))
	}
	return allErrs
}

// Validates that a Quantity is positive
func ValidatePositiveQuantityValue(value resource.Quantity, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if value.Cmp(resource.Quantity{}) <= 0 {
		allErrs = append(allErrs, field.Invalid(fldPath, value.String(), isNotPositiveErrorMsg))
	}
	return allErrs
}

func ValidateImmutableField(newVal, oldVal interface{}, fldPath *field.Path) field.ErrorList {
	return apimachineryvalidation.ValidateImmutableField(newVal, oldVal, fldPath)
}

func ValidateImmutableAnnotation(newVal string, oldVal string, annotation string, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}

	if oldVal != newVal {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("annotations", annotation), newVal, fieldImmutableErrorMsg))
	}
	return allErrs
}

// ValidateObjectMeta validates an object's metadata on creation. It expects that name generation has already
// been performed.
// It doesn't return an error for rootscoped resources with namespace, because namespace should already be cleared before.
// TODO: Remove calls to this method scattered in validations of specific resources, e.g., ValidatePodUpdate.
func ValidateObjectMeta(meta *metav1.ObjectMeta, requiresNamespace bool, nameFn ValidateNameFunc, fldPath *field.Path) field.ErrorList {
	allErrs := apimachineryvalidation.ValidateObjectMeta(meta, requiresNamespace, apimachineryvalidation.ValidateNameFunc(nameFn), fldPath)
	// run additional checks for the finalizer name
	for i := range meta.Finalizers {
		allErrs = append(allErrs, validateKubeFinalizerName(string(meta.Finalizers[i]), fldPath.Child("finalizers").Index(i))...)
	}
	return allErrs
}

// ValidateObjectMetaUpdate validates an object's metadata when updated
func ValidateObjectMetaUpdate(newMeta, oldMeta *metav1.ObjectMeta, fldPath *field.Path) field.ErrorList {
	allErrs := apimachineryvalidation.ValidateObjectMetaUpdate(newMeta, oldMeta, fldPath)
	// run additional checks for the finalizer name
	for i := range newMeta.Finalizers {
		allErrs = append(allErrs, validateKubeFinalizerName(string(newMeta.Finalizers[i]), fldPath.Child("finalizers").Index(i))...)
	}

	return allErrs
}

func ValidateVolumes(volumes []core.Volume, podMeta *metav1.ObjectMeta, fldPath *field.Path, opts PodValidationOptions) (map[string]core.VolumeSource, field.ErrorList) {
	allErrs := field.ErrorList{}

	allNames := sets.Set[string]{}
	allCreatedPVCs := sets.Set[string]{}
	// Determine which PVCs will be created for this pod. We need
	// the exact name of the pod for this. Without it, this sanity
	// check has to be skipped.
	if podMeta != nil && podMeta.Name != "" {
		for _, vol := range volumes {
			if vol.VolumeSource.Ephemeral != nil {
				allCreatedPVCs.Insert(podMeta.Name + "-" + vol.Name)
			}
		}
	}
	vols := make(map[string]core.VolumeSource)
	for i, vol := range volumes {
		idxPath := fldPath.Index(i)
		namePath := idxPath.Child("name")
		el := validateVolumeSource(&vol.VolumeSource, idxPath, vol.Name, podMeta, opts)
		if len(vol.Name) == 0 {
			el = append(el, field.Required(namePath, ""))
		} else {
			el = append(el, ValidateDNS1123Label(vol.Name, namePath)...)
		}
		if allNames.Has(vol.Name) {
			el = append(el, field.Duplicate(namePath, vol.Name))
		}
		if len(el) == 0 {
			allNames.Insert(vol.Name)
			vols[vol.Name] = vol.VolumeSource
		} else {
			allErrs = append(allErrs, el...)
		}
		// A PersistentVolumeClaimSource should not reference a created PVC. That doesn't
		// make sense.
		if vol.PersistentVolumeClaim != nil && allCreatedPVCs.Has(vol.PersistentVolumeClaim.ClaimName) {
			allErrs = append(allErrs, field.Invalid(idxPath.Child("persistentVolumeClaim").Child("claimName"), vol.PersistentVolumeClaim.ClaimName,
				"must not reference a PVC that gets created for an ephemeral volume"))
		}
	}

	return vols, allErrs
}

func IsMatchedVolume(name string, volumes map[string]core.VolumeSource) bool {
	if _, ok := volumes[name]; ok {
		return true
	}
	return false
}

// isMatched checks whether the volume with the given name is used by a
// container and if so, if it involves a PVC.
func isMatchedDevice(name string, volumes map[string]core.VolumeSource) (isMatched bool, isPVC bool) {
	if source, ok := volumes[name]; ok {
		if source.PersistentVolumeClaim != nil ||
			source.Ephemeral != nil {
			return true, true
		}
		return true, false
	}
	return false, false
}

func mountNameAlreadyExists(name string, devices map[string]string) bool {
	if _, ok := devices[name]; ok {
		return true
	}
	return false
}

func mountPathAlreadyExists(mountPath string, devices map[string]string) bool {
	for _, devPath := range devices {
		if mountPath == devPath {
			return true
		}
	}

	return false
}

func deviceNameAlreadyExists(name string, mounts map[string]string) bool {
	if _, ok := mounts[name]; ok {
		return true
	}
	return false
}

func devicePathAlreadyExists(devicePath string, mounts map[string]string) bool {
	for _, mountPath := range mounts {
		if mountPath == devicePath {
			return true
		}
	}

	return false
}

func validateVolumeSource(source *core.VolumeSource, fldPath *field.Path, volName string, podMeta *metav1.ObjectMeta, opts PodValidationOptions) field.ErrorList {
	numVolumes := 0
	allErrs := field.ErrorList{}
	if source.EmptyDir != nil {
		numVolumes++
		if source.EmptyDir.SizeLimit != nil && source.EmptyDir.SizeLimit.Cmp(resource.Quantity{}) < 0 {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("emptyDir").Child("sizeLimit"), "SizeLimit field must be a valid resource quantity"))
		}
	}
	if source.HostPath != nil {
		if numVolumes > 0 {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("hostPath"), "may not specify more than 1 volume type"))
		} else {
			numVolumes++
			allErrs = append(allErrs, validateHostPathVolumeSource(source.HostPath, fldPath.Child("hostPath"))...)
		}
	}
	if source.GitRepo != nil {
		if numVolumes > 0 {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("gitRepo"), "may not specify more than 1 volume type"))
		} else {
			numVolumes++
			allErrs = append(allErrs, validateGitRepoVolumeSource(source.GitRepo, fldPath.Child("gitRepo"))...)
		}
	}
	if source.GCEPersistentDisk != nil {
		if numVolumes > 0 {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("gcePersistentDisk"), "may not specify more than 1 volume type"))
		} else {
			numVolumes++
			allErrs = append(allErrs, validateGCEPersistentDiskVolumeSource(source.GCEPersistentDisk, fldPath.Child("persistentDisk"))...)
		}
	}
	if source.AWSElasticBlockStore != nil {
		if numVolumes > 0 {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("awsElasticBlockStore"), "may not specify more than 1 volume type"))
		} else {
			numVolumes++
			allErrs = append(allErrs, validateAWSElasticBlockStoreVolumeSource(source.AWSElasticBlockStore, fldPath.Child("awsElasticBlockStore"))...)
		}
	}
	if source.Secret != nil {
		if numVolumes > 0 {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("secret"), "may not specify more than 1 volume type"))
		} else {
			numVolumes++
			allErrs = append(allErrs, validateSecretVolumeSource(source.Secret, fldPath.Child("secret"))...)
		}
	}
	if source.NFS != nil {
		if numVolumes > 0 {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("nfs"), "may not specify more than 1 volume type"))
		} else {
			numVolumes++
			allErrs = append(allErrs, validateNFSVolumeSource(source.NFS, fldPath.Child("nfs"))...)
		}
	}
	if source.ISCSI != nil {
		if numVolumes > 0 {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("iscsi"), "may not specify more than 1 volume type"))
		} else {
			numVolumes++
			allErrs = append(allErrs, validateISCSIVolumeSource(source.ISCSI, fldPath.Child("iscsi"))...)
		}
		if source.ISCSI.InitiatorName != nil && len(volName+":"+source.ISCSI.TargetPortal) > 64 {
			tooLongErr := "Total length of <volume name>:<iscsi.targetPortal> must be under 64 characters if iscsi.initiatorName is specified."
			allErrs = append(allErrs, field.Invalid(fldPath.Child("name"), volName, tooLongErr))
		}
	}
	if source.Glusterfs != nil {
		if numVolumes > 0 {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("glusterfs"), "may not specify more than 1 volume type"))
		} else {
			numVolumes++
			allErrs = append(allErrs, validateGlusterfsVolumeSource(source.Glusterfs, fldPath.Child("glusterfs"))...)
		}
	}
	if source.Flocker != nil {
		if numVolumes > 0 {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("flocker"), "may not specify more than 1 volume type"))
		} else {
			numVolumes++
			allErrs = append(allErrs, validateFlockerVolumeSource(source.Flocker, fldPath.Child("flocker"))...)
		}
	}
	if source.PersistentVolumeClaim != nil {
		if numVolumes > 0 {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("persistentVolumeClaim"), "may not specify more than 1 volume type"))
		} else {
			numVolumes++
			allErrs = append(allErrs, validatePersistentClaimVolumeSource(source.PersistentVolumeClaim, fldPath.Child("persistentVolumeClaim"))...)
		}
	}
	if source.RBD != nil {
		if numVolumes > 0 {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("rbd"), "may not specify more than 1 volume type"))
		} else {
			numVolumes++
			allErrs = append(allErrs, validateRBDVolumeSource(source.RBD, fldPath.Child("rbd"))...)
		}
	}
	if source.Cinder != nil {
		if numVolumes > 0 {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("cinder"), "may not specify more than 1 volume type"))
		} else {
			numVolumes++
			allErrs = append(allErrs, validateCinderVolumeSource(source.Cinder, fldPath.Child("cinder"))...)
		}
	}
	if source.CephFS != nil {
		if numVolumes > 0 {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("cephFS"), "may not specify more than 1 volume type"))
		} else {
			numVolumes++
			allErrs = append(allErrs, validateCephFSVolumeSource(source.CephFS, fldPath.Child("cephfs"))...)
		}
	}
	if source.Quobyte != nil {
		if numVolumes > 0 {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("quobyte"), "may not specify more than 1 volume type"))
		} else {
			numVolumes++
			allErrs = append(allErrs, validateQuobyteVolumeSource(source.Quobyte, fldPath.Child("quobyte"))...)
		}
	}
	if source.DownwardAPI != nil {
		if numVolumes > 0 {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("downwarAPI"), "may not specify more than 1 volume type"))
		} else {
			numVolumes++
			allErrs = append(allErrs, validateDownwardAPIVolumeSource(source.DownwardAPI, fldPath.Child("downwardAPI"), opts)...)
		}
	}
	if source.FC != nil {
		if numVolumes > 0 {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("fc"), "may not specify more than 1 volume type"))
		} else {
			numVolumes++
			allErrs = append(allErrs, validateFCVolumeSource(source.FC, fldPath.Child("fc"))...)
		}
	}
	if source.FlexVolume != nil {
		if numVolumes > 0 {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("flexVolume"), "may not specify more than 1 volume type"))
		} else {
			numVolumes++
			allErrs = append(allErrs, validateFlexVolumeSource(source.FlexVolume, fldPath.Child("flexVolume"))...)
		}
	}
	if source.ConfigMap != nil {
		if numVolumes > 0 {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("configMap"), "may not specify more than 1 volume type"))
		} else {
			numVolumes++
			allErrs = append(allErrs, validateConfigMapVolumeSource(source.ConfigMap, fldPath.Child("configMap"))...)
		}
	}

	if source.AzureFile != nil {
		if numVolumes > 0 {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("azureFile"), "may not specify more than 1 volume type"))
		} else {
			numVolumes++
			allErrs = append(allErrs, validateAzureFile(source.AzureFile, fldPath.Child("azureFile"))...)
		}
	}

	if source.VsphereVolume != nil {
		if numVolumes > 0 {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("vsphereVolume"), "may not specify more than 1 volume type"))
		} else {
			numVolumes++
			allErrs = append(allErrs, validateVsphereVolumeSource(source.VsphereVolume, fldPath.Child("vsphereVolume"))...)
		}
	}
	if source.PhotonPersistentDisk != nil {
		if numVolumes > 0 {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("photonPersistentDisk"), "may not specify more than 1 volume type"))
		} else {
			numVolumes++
			allErrs = append(allErrs, validatePhotonPersistentDiskVolumeSource(source.PhotonPersistentDisk, fldPath.Child("photonPersistentDisk"))...)
		}
	}
	if source.PortworxVolume != nil {
		if numVolumes > 0 {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("portworxVolume"), "may not specify more than 1 volume type"))
		} else {
			numVolumes++
			allErrs = append(allErrs, validatePortworxVolumeSource(source.PortworxVolume, fldPath.Child("portworxVolume"))...)
		}
	}
	if source.AzureDisk != nil {
		if numVolumes > 0 {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("azureDisk"), "may not specify more than 1 volume type"))
		} else {
			numVolumes++
			allErrs = append(allErrs, validateAzureDisk(source.AzureDisk, fldPath.Child("azureDisk"))...)
		}
	}
	if source.StorageOS != nil {
		if numVolumes > 0 {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("storageos"), "may not specify more than 1 volume type"))
		} else {
			numVolumes++
			allErrs = append(allErrs, validateStorageOSVolumeSource(source.StorageOS, fldPath.Child("storageos"))...)
		}
	}
	if source.Projected != nil {
		if numVolumes > 0 {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("projected"), "may not specify more than 1 volume type"))
		} else {
			numVolumes++
			allErrs = append(allErrs, validateProjectedVolumeSource(source.Projected, fldPath.Child("projected"), opts)...)
		}
	}
	if source.ScaleIO != nil {
		if numVolumes > 0 {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("scaleIO"), "may not specify more than 1 volume type"))
		} else {
			numVolumes++
			allErrs = append(allErrs, validateScaleIOVolumeSource(source.ScaleIO, fldPath.Child("scaleIO"))...)
		}
	}
	if source.CSI != nil {
		if numVolumes > 0 {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("csi"), "may not specify more than 1 volume type"))
		} else {
			numVolumes++
			allErrs = append(allErrs, validateCSIVolumeSource(source.CSI, fldPath.Child("csi"))...)
		}
	}
	if source.Ephemeral != nil {
		if numVolumes > 0 {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("ephemeral"), "may not specify more than 1 volume type"))
		} else {
			numVolumes++
			allErrs = append(allErrs, validateEphemeralVolumeSource(source.Ephemeral, fldPath.Child("ephemeral"))...)
			// Check the expected name for the PVC. This gets skipped if information is missing,
			// because that already gets flagged as a problem elsewhere. For example,
			// ValidateObjectMeta as called by validatePodMetadataAndSpec checks that the name is set.
			if podMeta != nil && podMeta.Name != "" && volName != "" {
				pvcName := podMeta.Name + "-" + volName
				for _, msg := range ValidatePersistentVolumeName(pvcName, false) {
					allErrs = append(allErrs, field.Invalid(fldPath.Child("name"), volName, fmt.Sprintf("PVC name %q: %v", pvcName, msg)))
				}
			}
		}
	}
	if source.Image != nil {
		if numVolumes > 0 {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("image"), "may not specify more than 1 volume type"))
		} else {
			numVolumes++
			allErrs = append(allErrs, validateImageVolumeSource(source.Image, fldPath.Child("image"), opts)...)
		}
	}

	if numVolumes == 0 {
		allErrs = append(allErrs, field.Required(fldPath, "must specify a volume type"))
	}

	return allErrs
}

func validateHostPathVolumeSource(hostPath *core.HostPathVolumeSource, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if len(hostPath.Path) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("path"), ""))
		return allErrs
	}

	allErrs = append(allErrs, validatePathNoBacksteps(hostPath.Path, fldPath.Child("path"))...)
	allErrs = append(allErrs, validateHostPathType(hostPath.Type, fldPath.Child("type"))...)
	return allErrs
}

func validateGitRepoVolumeSource(gitRepo *core.GitRepoVolumeSource, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if len(gitRepo.Repository) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("repository"), ""))
	}

	pathErrs := validateLocalDescendingPath(gitRepo.Directory, fldPath.Child("directory"))
	allErrs = append(allErrs, pathErrs...)
	return allErrs
}

func validateISCSIVolumeSource(iscsi *core.ISCSIVolumeSource, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if len(iscsi.TargetPortal) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("targetPortal"), ""))
	}
	if len(iscsi.IQN) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("iqn"), ""))
	} else {
		if !strings.HasPrefix(iscsi.IQN, "iqn") && !strings.HasPrefix(iscsi.IQN, "eui") && !strings.HasPrefix(iscsi.IQN, "naa") {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("iqn"), iscsi.IQN, "must be valid format starting with iqn, eui, or naa"))
		} else if strings.HasPrefix(iscsi.IQN, "iqn") && !iscsiInitiatorIqnRegex.MatchString(iscsi.IQN) {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("iqn"), iscsi.IQN, "must be valid format"))
		} else if strings.HasPrefix(iscsi.IQN, "eui") && !iscsiInitiatorEuiRegex.MatchString(iscsi.IQN) {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("iqn"), iscsi.IQN, "must be valid format"))
		} else if strings.HasPrefix(iscsi.IQN, "naa") && !iscsiInitiatorNaaRegex.MatchString(iscsi.IQN) {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("iqn"), iscsi.IQN, "must be valid format"))
		}
	}
	if iscsi.Lun < 0 || iscsi.Lun > 255 {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("lun"), iscsi.Lun, validation.InclusiveRangeError(0, 255)))
	}
	if (iscsi.DiscoveryCHAPAuth || iscsi.SessionCHAPAuth) && iscsi.SecretRef == nil {
		allErrs = append(allErrs, field.Required(fldPath.Child("secretRef"), ""))
	}
	if iscsi.InitiatorName != nil {
		initiator := *iscsi.InitiatorName
		if !strings.HasPrefix(initiator, "iqn") && !strings.HasPrefix(initiator, "eui") && !strings.HasPrefix(initiator, "naa") {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("initiatorname"), initiator, "must be valid format starting with iqn, eui, or naa"))
		}
		if strings.HasPrefix(initiator, "iqn") && !iscsiInitiatorIqnRegex.MatchString(initiator) {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("initiatorname"), initiator, "must be valid format"))
		} else if strings.HasPrefix(initiator, "eui") && !iscsiInitiatorEuiRegex.MatchString(initiator) {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("initiatorname"), initiator, "must be valid format"))
		} else if strings.HasPrefix(initiator, "naa") && !iscsiInitiatorNaaRegex.MatchString(initiator) {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("initiatorname"), initiator, "must be valid format"))
		}
	}
	return allErrs
}

func validateISCSIPersistentVolumeSource(iscsi *core.ISCSIPersistentVolumeSource, pvName string, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if len(iscsi.TargetPortal) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("targetPortal"), ""))
	}
	if iscsi.InitiatorName != nil && len(pvName+":"+iscsi.TargetPortal) > 64 {
		tooLongErr := "Total length of <volume name>:<iscsi.targetPortal> must be under 64 characters if iscsi.initiatorName is specified."
		allErrs = append(allErrs, field.Invalid(fldPath.Child("targetportal"), iscsi.TargetPortal, tooLongErr))
	}
	if len(iscsi.IQN) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("iqn"), ""))
	} else {
		if !strings.HasPrefix(iscsi.IQN, "iqn") && !strings.HasPrefix(iscsi.IQN, "eui") && !strings.HasPrefix(iscsi.IQN, "naa") {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("iqn"), iscsi.IQN, "must be valid format"))
		} else if strings.HasPrefix(iscsi.IQN, "iqn") && !iscsiInitiatorIqnRegex.MatchString(iscsi.IQN) {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("iqn"), iscsi.IQN, "must be valid format"))
		} else if strings.HasPrefix(iscsi.IQN, "eui") && !iscsiInitiatorEuiRegex.MatchString(iscsi.IQN) {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("iqn"), iscsi.IQN, "must be valid format"))
		} else if strings.HasPrefix(iscsi.IQN, "naa") && !iscsiInitiatorNaaRegex.MatchString(iscsi.IQN) {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("iqn"), iscsi.IQN, "must be valid format"))
		}
	}
	if iscsi.Lun < 0 || iscsi.Lun > 255 {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("lun"), iscsi.Lun, validation.InclusiveRangeError(0, 255)))
	}
	if (iscsi.DiscoveryCHAPAuth || iscsi.SessionCHAPAuth) && iscsi.SecretRef == nil {
		allErrs = append(allErrs, field.Required(fldPath.Child("secretRef"), ""))
	}
	if iscsi.SecretRef != nil {
		if len(iscsi.SecretRef.Name) == 0 {
			allErrs = append(allErrs, field.Required(fldPath.Child("secretRef", "name"), ""))
		}
	}
	if iscsi.InitiatorName != nil {
		initiator := *iscsi.InitiatorName
		if !strings.HasPrefix(initiator, "iqn") && !strings.HasPrefix(initiator, "eui") && !strings.HasPrefix(initiator, "naa") {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("initiatorname"), initiator, "must be valid format"))
		}
		if strings.HasPrefix(initiator, "iqn") && !iscsiInitiatorIqnRegex.MatchString(initiator) {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("initiatorname"), initiator, "must be valid format"))
		} else if strings.HasPrefix(initiator, "eui") && !iscsiInitiatorEuiRegex.MatchString(initiator) {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("initiatorname"), initiator, "must be valid format"))
		} else if strings.HasPrefix(initiator, "naa") && !iscsiInitiatorNaaRegex.MatchString(initiator) {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("initiatorname"), initiator, "must be valid format"))
		}
	}
	return allErrs
}

func validateFCVolumeSource(fc *core.FCVolumeSource, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if len(fc.TargetWWNs) < 1 && len(fc.WWIDs) < 1 {
		allErrs = append(allErrs, field.Required(fldPath.Child("targetWWNs"), "must specify either targetWWNs or wwids, but not both"))
	}

	if len(fc.TargetWWNs) != 0 && len(fc.WWIDs) != 0 {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("targetWWNs"), fc.TargetWWNs, "targetWWNs and wwids can not be specified simultaneously"))
	}

	if len(fc.TargetWWNs) != 0 {
		if fc.Lun == nil {
			allErrs = append(allErrs, field.Required(fldPath.Child("lun"), "lun is required if targetWWNs is specified"))
		} else {
			if *fc.Lun < 0 || *fc.Lun > 255 {
				allErrs = append(allErrs, field.Invalid(fldPath.Child("lun"), fc.Lun, validation.InclusiveRangeError(0, 255)))
			}
		}
	}
	return allErrs
}

func validateGCEPersistentDiskVolumeSource(pd *core.GCEPersistentDiskVolumeSource, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if len(pd.PDName) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("pdName"), ""))
	}
	if pd.Partition < 0 || pd.Partition > 255 {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("partition"), pd.Partition, pdPartitionErrorMsg))
	}
	return allErrs
}

func validateAWSElasticBlockStoreVolumeSource(PD *core.AWSElasticBlockStoreVolumeSource, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if len(PD.VolumeID) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("volumeID"), ""))
	}
	if PD.Partition < 0 || PD.Partition > 255 {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("partition"), PD.Partition, pdPartitionErrorMsg))
	}
	return allErrs
}

func validateSecretVolumeSource(secretSource *core.SecretVolumeSource, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if len(secretSource.SecretName) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("secretName"), ""))
	}

	secretMode := secretSource.DefaultMode
	if secretMode != nil && (*secretMode > 0777 || *secretMode < 0) {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("defaultMode"), *secretMode, fileModeErrorMsg))
	}

	itemsPath := fldPath.Child("items")
	for i, kp := range secretSource.Items {
		itemPath := itemsPath.Index(i)
		allErrs = append(allErrs, validateKeyToPath(&kp, itemPath)...)
	}
	return allErrs
}

func validateConfigMapVolumeSource(configMapSource *core.ConfigMapVolumeSource, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if len(configMapSource.Name) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("name"), ""))
	}

	configMapMode := configMapSource.DefaultMode
	if configMapMode != nil && (*configMapMode > 0777 || *configMapMode < 0) {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("defaultMode"), *configMapMode, fileModeErrorMsg))
	}

	itemsPath := fldPath.Child("items")
	for i, kp := range configMapSource.Items {
		itemPath := itemsPath.Index(i)
		allErrs = append(allErrs, validateKeyToPath(&kp, itemPath)...)
	}
	return allErrs
}

func validateKeyToPath(kp *core.KeyToPath, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if len(kp.Key) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("key"), ""))
	}
	if len(kp.Path) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("path"), ""))
	}
	allErrs = append(allErrs, ValidateLocalNonReservedPath(kp.Path, fldPath.Child("path"))...)
	if kp.Mode != nil && (*kp.Mode > 0777 || *kp.Mode < 0) {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("mode"), *kp.Mode, fileModeErrorMsg))
	}

	return allErrs
}

func validatePersistentClaimVolumeSource(claim *core.PersistentVolumeClaimVolumeSource, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if len(claim.ClaimName) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("claimName"), ""))
	}
	return allErrs
}

func validateNFSVolumeSource(nfs *core.NFSVolumeSource, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if len(nfs.Server) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("server"), ""))
	}
	if len(nfs.Path) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("path"), ""))
	}
	if !path.IsAbs(nfs.Path) {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("path"), nfs.Path, "must be an absolute path"))
	}
	return allErrs
}

func validateQuobyteVolumeSource(quobyte *core.QuobyteVolumeSource, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if len(quobyte.Registry) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("registry"), "must be a host:port pair or multiple pairs separated by commas"))
	} else if len(quobyte.Tenant) >= 65 {
		allErrs = append(allErrs, field.Required(fldPath.Child("tenant"), "must be a UUID and may not exceed a length of 64 characters"))
	} else {
		for _, hostPortPair := range strings.Split(quobyte.Registry, ",") {
			if _, _, err := net.SplitHostPort(hostPortPair); err != nil {
				allErrs = append(allErrs, field.Invalid(fldPath.Child("registry"), quobyte.Registry, "must be a host:port pair or multiple pairs separated by commas"))
			}
		}
	}

	if len(quobyte.Volume) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("volume"), ""))
	}
	return allErrs
}

func validateGlusterfsVolumeSource(glusterfs *core.GlusterfsVolumeSource, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if len(glusterfs.EndpointsName) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("endpoints"), ""))
	}
	if len(glusterfs.Path) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("path"), ""))
	}
	return allErrs
}
func validateGlusterfsPersistentVolumeSource(glusterfs *core.GlusterfsPersistentVolumeSource, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if len(glusterfs.EndpointsName) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("endpoints"), ""))
	}
	if len(glusterfs.Path) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("path"), ""))
	}
	if glusterfs.EndpointsNamespace != nil {
		endpointNs := glusterfs.EndpointsNamespace
		if *endpointNs == "" {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("endpointsNamespace"), *endpointNs, "if the endpointnamespace is set, it must be a valid namespace name"))
		} else {
			for _, msg := range ValidateNamespaceName(*endpointNs, false) {
				allErrs = append(allErrs, field.Invalid(fldPath.Child("endpointsNamespace"), *endpointNs, msg))
			}
		}
	}
	return allErrs
}

func validateFlockerVolumeSource(flocker *core.FlockerVolumeSource, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if len(flocker.DatasetName) == 0 && len(flocker.DatasetUUID) == 0 {
		// TODO: consider adding a RequiredOneOf() error for this and similar cases
		allErrs = append(allErrs, field.Required(fldPath, "one of datasetName and datasetUUID is required"))
	}
	if len(flocker.DatasetName) != 0 && len(flocker.DatasetUUID) != 0 {
		allErrs = append(allErrs, field.Invalid(fldPath, "resource", "datasetName and datasetUUID can not be specified simultaneously"))
	}
	if strings.Contains(flocker.DatasetName, "/") {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("datasetName"), flocker.DatasetName, "must not contain '/'"))
	}
	return allErrs
}

var validVolumeDownwardAPIFieldPathExpressions = sets.New(
	"metadata.name",
	"metadata.namespace",
	"metadata.labels",
	"metadata.annotations",
	"metadata.uid")

func validateDownwardAPIVolumeFile(file *core.DownwardAPIVolumeFile, fldPath *field.Path, opts PodValidationOptions) field.ErrorList {
	allErrs := field.ErrorList{}
	if len(file.Path) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("path"), ""))
	}
	allErrs = append(allErrs, ValidateLocalNonReservedPath(file.Path, fldPath.Child("path"))...)
	if file.FieldRef != nil {
		allErrs = append(allErrs, validateObjectFieldSelector(file.FieldRef, &validVolumeDownwardAPIFieldPathExpressions, fldPath.Child("fieldRef"))...)
		if file.ResourceFieldRef != nil {
			allErrs = append(allErrs, field.Invalid(fldPath, "resource", "fieldRef and resourceFieldRef can not be specified simultaneously"))
		}
	} else if file.ResourceFieldRef != nil {
		localValidContainerResourceFieldPathPrefixes := validContainerResourceFieldPathPrefixesWithDownwardAPIHugePages
		allErrs = append(allErrs, validateContainerResourceFieldSelector(file.ResourceFieldRef, &validContainerResourceFieldPathExpressions, &localValidContainerResourceFieldPathPrefixes, fldPath.Child("resourceFieldRef"), true)...)
	} else {
		allErrs = append(allErrs, field.Required(fldPath, "one of fieldRef and resourceFieldRef is required"))
	}
	if file.Mode != nil && (*file.Mode > 0777 || *file.Mode < 0) {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("mode"), *file.Mode, fileModeErrorMsg))
	}

	return allErrs
}

func validateDownwardAPIVolumeSource(downwardAPIVolume *core.DownwardAPIVolumeSource, fldPath *field.Path, opts PodValidationOptions) field.ErrorList {
	allErrs := field.ErrorList{}

	downwardAPIMode := downwardAPIVolume.DefaultMode
	if downwardAPIMode != nil && (*downwardAPIMode > 0777 || *downwardAPIMode < 0) {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("defaultMode"), *downwardAPIMode, fileModeErrorMsg))
	}

	for _, file := range downwardAPIVolume.Items {
		allErrs = append(allErrs, validateDownwardAPIVolumeFile(&file, fldPath, opts)...)
	}
	return allErrs
}

func validateProjectionSources(projection *core.ProjectedVolumeSource, projectionMode *int32, fldPath *field.Path, opts PodValidationOptions) field.ErrorList {
	allErrs := field.ErrorList{}
	allPaths := sets.Set[string]{}

	for i, source := range projection.Sources {
		numSources := 0
		srcPath := fldPath.Child("sources").Index(i)
		if projPath := srcPath.Child("secret"); source.Secret != nil {
			numSources++
			if len(source.Secret.Name) == 0 {
				allErrs = append(allErrs, field.Required(projPath.Child("name"), ""))
			}
			itemsPath := projPath.Child("items")
			for i, kp := range source.Secret.Items {
				itemPath := itemsPath.Index(i)
				allErrs = append(allErrs, validateKeyToPath(&kp, itemPath)...)
				if len(kp.Path) > 0 {
					curPath := kp.Path
					if !allPaths.Has(curPath) {
						allPaths.Insert(curPath)
					} else {
						allErrs = append(allErrs, field.Invalid(fldPath, source.Secret.Name, "conflicting duplicate paths"))
					}
				}
			}
		}
		if projPath := srcPath.Child("configMap"); source.ConfigMap != nil {
			numSources++
			if len(source.ConfigMap.Name) == 0 {
				allErrs = append(allErrs, field.Required(projPath.Child("name"), ""))
			}
			itemsPath := projPath.Child("items")
			for i, kp := range source.ConfigMap.Items {
				itemPath := itemsPath.Index(i)
				allErrs = append(allErrs, validateKeyToPath(&kp, itemPath)...)
				if len(kp.Path) > 0 {
					curPath := kp.Path
					if !allPaths.Has(curPath) {
						allPaths.Insert(curPath)
					} else {
						allErrs = append(allErrs, field.Invalid(fldPath, source.ConfigMap.Name, "conflicting duplicate paths"))
					}
				}
			}
		}
		if projPath := srcPath.Child("downwardAPI"); source.DownwardAPI != nil {
			numSources++
			for _, file := range source.DownwardAPI.Items {
				allErrs = append(allErrs, validateDownwardAPIVolumeFile(&file, projPath, opts)...)
				if len(file.Path) > 0 {
					curPath := file.Path
					if !allPaths.Has(curPath) {
						allPaths.Insert(curPath)
					} else {
						allErrs = append(allErrs, field.Invalid(fldPath, curPath, "conflicting duplicate paths"))
					}
				}
			}
		}
		if projPath := srcPath.Child("serviceAccountToken"); source.ServiceAccountToken != nil {
			numSources++
			if source.ServiceAccountToken.ExpirationSeconds < 10*60 {
				allErrs = append(allErrs, field.Invalid(projPath.Child("expirationSeconds"), source.ServiceAccountToken.ExpirationSeconds, "may not specify a duration less than 10 minutes"))
			}
			if source.ServiceAccountToken.ExpirationSeconds > 1<<32 {
				allErrs = append(allErrs, field.Invalid(projPath.Child("expirationSeconds"), source.ServiceAccountToken.ExpirationSeconds, "may not specify a duration larger than 2^32 seconds"))
			}
			if source.ServiceAccountToken.Path == "" {
				allErrs = append(allErrs, field.Required(fldPath.Child("path"), ""))
			} else if !opts.AllowNonLocalProjectedTokenPath {
				allErrs = append(allErrs, ValidateLocalNonReservedPath(source.ServiceAccountToken.Path, fldPath.Child("path"))...)
			}
		}
		if projPath := srcPath.Child("clusterTrustBundle"); source.ClusterTrustBundle != nil {
			numSources++

			usingName := source.ClusterTrustBundle.Name != nil
			usingSignerName := source.ClusterTrustBundle.SignerName != nil

			switch {
			case usingName && usingSignerName:
				allErrs = append(allErrs, field.Invalid(projPath, source.ClusterTrustBundle, "only one of name and signerName may be used"))
			case usingName:
				if *source.ClusterTrustBundle.Name == "" {
					allErrs = append(allErrs, field.Required(projPath.Child("name"), "must be a valid object name"))
				}

				name := *source.ClusterTrustBundle.Name
				if signerName, ok := extractSignerNameFromClusterTrustBundleName(name); ok {
					validationFunc := ValidateClusterTrustBundleName(signerName)
					errMsgs := validationFunc(name, false)
					for _, msg := range errMsgs {
						allErrs = append(allErrs, field.Invalid(projPath.Child("name"), name, fmt.Sprintf("not a valid clustertrustbundlename: %v", msg)))
					}
				} else {
					validationFunc := ValidateClusterTrustBundleName("")
					errMsgs := validationFunc(name, false)
					for _, msg := range errMsgs {
						allErrs = append(allErrs, field.Invalid(projPath.Child("name"), name, fmt.Sprintf("not a valid clustertrustbundlename: %v", msg)))
					}
				}

				if source.ClusterTrustBundle.LabelSelector != nil {
					allErrs = append(allErrs, field.Invalid(projPath.Child("labelSelector"), source.ClusterTrustBundle.LabelSelector, "labelSelector must be unset if name is specified"))
				}
			case usingSignerName:
				if *source.ClusterTrustBundle.SignerName == "" {
					allErrs = append(allErrs, field.Required(projPath.Child("signerName"), "must be a valid signer name"))
				}

				allErrs = append(allErrs, ValidateSignerName(projPath.Child("signerName"), *source.ClusterTrustBundle.SignerName)...)

				labelSelectorErrs := unversionedvalidation.ValidateLabelSelector(
					source.ClusterTrustBundle.LabelSelector,
					unversionedvalidation.LabelSelectorValidationOptions{AllowInvalidLabelValueInSelector: false},
					projPath.Child("labelSelector"),
				)
				allErrs = append(allErrs, labelSelectorErrs...)

			default:
				allErrs = append(allErrs, field.Required(projPath, "either name or signerName must be specified"))
			}

			if source.ClusterTrustBundle.Path == "" {
				allErrs = append(allErrs, field.Required(projPath.Child("path"), ""))
			}

			allErrs = append(allErrs, ValidateLocalNonReservedPath(source.ClusterTrustBundle.Path, projPath.Child("path"))...)

			curPath := source.ClusterTrustBundle.Path
			if !allPaths.Has(curPath) {
				allPaths.Insert(curPath)
			} else {
				allErrs = append(allErrs, field.Invalid(fldPath, curPath, "conflicting duplicate paths"))
			}
		}
		if numSources > 1 {
			allErrs = append(allErrs, field.Forbidden(srcPath, "may not specify more than 1 volume type per source"))
		}
	}
	return allErrs
}

func validateProjectedVolumeSource(projection *core.ProjectedVolumeSource, fldPath *field.Path, opts PodValidationOptions) field.ErrorList {
	allErrs := field.ErrorList{}

	projectionMode := projection.DefaultMode
	if projectionMode != nil && (*projectionMode > 0777 || *projectionMode < 0) {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("defaultMode"), *projectionMode, fileModeErrorMsg))
	}

	allErrs = append(allErrs, validateProjectionSources(projection, projectionMode, fldPath, opts)...)
	return allErrs
}

var supportedHostPathTypes = sets.New(
	core.HostPathUnset,
	core.HostPathDirectoryOrCreate,
	core.HostPathDirectory,
	core.HostPathFileOrCreate,
	core.HostPathFile,
	core.HostPathSocket,
	core.HostPathCharDev,
	core.HostPathBlockDev)

func validateHostPathType(hostPathType *core.HostPathType, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}

	if hostPathType != nil && !supportedHostPathTypes.Has(*hostPathType) {
		allErrs = append(allErrs, field.NotSupported(fldPath, hostPathType, sets.List(supportedHostPathTypes)))
	}

	return allErrs
}

// This validate will make sure targetPath:
// 1. is not abs path
// 2. does not have any element which is ".."
func validateLocalDescendingPath(targetPath string, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if path.IsAbs(targetPath) {
		allErrs = append(allErrs, field.Invalid(fldPath, targetPath, "must be a relative path"))
	}

	allErrs = append(allErrs, validatePathNoBacksteps(targetPath, fldPath)...)

	return allErrs
}

// validatePathNoBacksteps makes sure the targetPath does not have any `..` path elements when split
//
// This assumes the OS of the apiserver and the nodes are the same. The same check should be done
// on the node to ensure there are no backsteps.
func validatePathNoBacksteps(targetPath string, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	parts := strings.Split(filepath.ToSlash(targetPath), "/")
	for _, item := range parts {
		if item == ".." {
			allErrs = append(allErrs, field.Invalid(fldPath, targetPath, "must not contain '..'"))
			break // even for `../../..`, one error is sufficient to make the point
		}
	}
	return allErrs
}

// validateMountPropagation verifies that MountPropagation field is valid and
// allowed for given container.
func validateMountPropagation(mountPropagation *core.MountPropagationMode, container *core.Container, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}

	if mountPropagation == nil {
		return allErrs
	}

	supportedMountPropagations := sets.New(
		core.MountPropagationBidirectional,
		core.MountPropagationHostToContainer,
		core.MountPropagationNone)

	if !supportedMountPropagations.Has(*mountPropagation) {
		allErrs = append(allErrs, field.NotSupported(fldPath, *mountPropagation, sets.List(supportedMountPropagations)))
	}

	if container == nil {
		// The container is not available yet.
		// Stop validation now, Pod validation will refuse final
		// Pods with Bidirectional propagation in non-privileged containers.
		return allErrs
	}

	privileged := container.SecurityContext != nil && container.SecurityContext.Privileged != nil && *container.SecurityContext.Privileged
	if *mountPropagation == core.MountPropagationBidirectional && !privileged {
		allErrs = append(allErrs, field.Forbidden(fldPath, "Bidirectional mount propagation is available only to privileged containers"))
	}
	return allErrs
}

// validateMountRecursiveReadOnly validates RecursiveReadOnly mounts.
func validateMountRecursiveReadOnly(mount core.VolumeMount, fldPath *field.Path) field.ErrorList {
	if mount.RecursiveReadOnly == nil {
		return nil
	}
	allErrs := field.ErrorList{}
	switch *mount.RecursiveReadOnly {
	case core.RecursiveReadOnlyDisabled:
		// NOP
	case core.RecursiveReadOnlyEnabled, core.RecursiveReadOnlyIfPossible:
		if !mount.ReadOnly {
			allErrs = append(allErrs, field.Forbidden(fldPath, "may only be specified when readOnly is true"))
		}
		if mount.MountPropagation != nil && *mount.MountPropagation != core.MountPropagationNone {
			allErrs = append(allErrs, field.Forbidden(fldPath, "may only be specified when mountPropagation is None or not specified"))
		}
	default:
		supportedRRO := sets.New(
			core.RecursiveReadOnlyDisabled,
			core.RecursiveReadOnlyIfPossible,
			core.RecursiveReadOnlyEnabled)
		allErrs = append(allErrs, field.NotSupported(fldPath, *mount.RecursiveReadOnly, sets.List(supportedRRO)))
	}
	return allErrs
}

// ValidateLocalNonReservedPath makes sure targetPath:
// 1. is not abs path
// 2. does not contain any '..' elements
// 3. does not start with '..'
func ValidateLocalNonReservedPath(targetPath string, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	allErrs = append(allErrs, validateLocalDescendingPath(targetPath, fldPath)...)
	// Don't report this error if the check for .. elements already caught it.
	if strings.HasPrefix(targetPath, "..") && !strings.HasPrefix(targetPath, "../") {
		allErrs = append(allErrs, field.Invalid(fldPath, targetPath, "must not start with '..'"))
	}
	return allErrs
}

func validateRBDVolumeSource(rbd *core.RBDVolumeSource, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if len(rbd.CephMonitors) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("monitors"), ""))
	}
	if len(rbd.RBDImage) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("image"), ""))
	}
	return allErrs
}

func validateRBDPersistentVolumeSource(rbd *core.RBDPersistentVolumeSource, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if len(rbd.CephMonitors) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("monitors"), ""))
	}
	if len(rbd.RBDImage) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("image"), ""))
	}
	return allErrs
}

func validateCinderVolumeSource(cd *core.CinderVolumeSource, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if len(cd.VolumeID) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("volumeID"), ""))
	}
	if cd.SecretRef != nil {
		if len(cd.SecretRef.Name) == 0 {
			allErrs = append(allErrs, field.Required(fldPath.Child("secretRef", "name"), ""))
		}
	}
	return allErrs
}

func validateCinderPersistentVolumeSource(cd *core.CinderPersistentVolumeSource, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if len(cd.VolumeID) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("volumeID"), ""))
	}
	if cd.SecretRef != nil {
		if len(cd.SecretRef.Name) == 0 {
			allErrs = append(allErrs, field.Required(fldPath.Child("secretRef", "name"), ""))
		}
		if len(cd.SecretRef.Namespace) == 0 {
			allErrs = append(allErrs, field.Required(fldPath.Child("secretRef", "namespace"), ""))
		}
	}
	return allErrs
}

func validateCephFSVolumeSource(cephfs *core.CephFSVolumeSource, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if len(cephfs.Monitors) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("monitors"), ""))
	}
	return allErrs
}

func validateCephFSPersistentVolumeSource(cephfs *core.CephFSPersistentVolumeSource, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if len(cephfs.Monitors) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("monitors"), ""))
	}
	return allErrs
}

func validateFlexVolumeSource(fv *core.FlexVolumeSource, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if len(fv.Driver) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("driver"), ""))
	}

	// Make sure user-specified options don't use kubernetes namespaces
	for k := range fv.Options {
		namespace := k
		if parts := strings.SplitN(k, "/", 2); len(parts) == 2 {
			namespace = parts[0]
		}
		normalized := "." + strings.ToLower(namespace)
		if strings.HasSuffix(normalized, ".kubernetes.io") || strings.HasSuffix(normalized, ".k8s.io") {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("options").Key(k), k, "kubernetes.io and k8s.io namespaces are reserved"))
		}
	}

	return allErrs
}

func validateFlexPersistentVolumeSource(fv *core.FlexPersistentVolumeSource, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if len(fv.Driver) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("driver"), ""))
	}

	// Make sure user-specified options don't use kubernetes namespaces
	for k := range fv.Options {
		namespace := k
		if parts := strings.SplitN(k, "/", 2); len(parts) == 2 {
			namespace = parts[0]
		}
		normalized := "." + strings.ToLower(namespace)
		if strings.HasSuffix(normalized, ".kubernetes.io") || strings.HasSuffix(normalized, ".k8s.io") {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("options").Key(k), k, "kubernetes.io and k8s.io namespaces are reserved"))
		}
	}

	return allErrs
}

func validateAzureFile(azure *core.AzureFileVolumeSource, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if azure.SecretName == "" {
		allErrs = append(allErrs, field.Required(fldPath.Child("secretName"), ""))
	}
	if azure.ShareName == "" {
		allErrs = append(allErrs, field.Required(fldPath.Child("shareName"), ""))
	}
	return allErrs
}

func validateAzureFilePV(azure *core.AzureFilePersistentVolumeSource, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if azure.SecretName == "" {
		allErrs = append(allErrs, field.Required(fldPath.Child("secretName"), ""))
	}
	if azure.ShareName == "" {
		allErrs = append(allErrs, field.Required(fldPath.Child("shareName"), ""))
	}
	if azure.SecretNamespace != nil {
		if len(*azure.SecretNamespace) == 0 {
			allErrs = append(allErrs, field.Required(fldPath.Child("secretNamespace"), ""))
		}
	}
	return allErrs
}

func validateAzureDisk(azure *core.AzureDiskVolumeSource, fldPath *field.Path) field.ErrorList {
	var supportedCachingModes = sets.New(
		core.AzureDataDiskCachingNone,
		core.AzureDataDiskCachingReadOnly,
		core.AzureDataDiskCachingReadWrite)

	var supportedDiskKinds = sets.New(
		core.AzureSharedBlobDisk,
		core.AzureDedicatedBlobDisk,
		core.AzureManagedDisk)

	diskURISupportedManaged := []string{"/subscriptions/{sub-id}/resourcegroups/{group-name}/providers/microsoft.compute/disks/{disk-id}"}
	diskURISupportedblob := []string{"https://{account-name}.blob.core.windows.net/{container-name}/{disk-name}.vhd"}

	allErrs := field.ErrorList{}
	if azure.DiskName == "" {
		allErrs = append(allErrs, field.Required(fldPath.Child("diskName"), ""))
	}

	if azure.DataDiskURI == "" {
		allErrs = append(allErrs, field.Required(fldPath.Child("diskURI"), ""))
	}

	if azure.CachingMode != nil && !supportedCachingModes.Has(*azure.CachingMode) {
		allErrs = append(allErrs, field.NotSupported(fldPath.Child("cachingMode"), *azure.CachingMode, sets.List(supportedCachingModes)))
	}

	if azure.Kind != nil && !supportedDiskKinds.Has(*azure.Kind) {
		allErrs = append(allErrs, field.NotSupported(fldPath.Child("kind"), *azure.Kind, sets.List(supportedDiskKinds)))
	}

	// validate that DiskUri is the correct format
	if azure.Kind != nil && *azure.Kind == core.AzureManagedDisk && strings.Index(azure.DataDiskURI, "/subscriptions/") != 0 {
		allErrs = append(allErrs, field.NotSupported(fldPath.Child("diskURI"), azure.DataDiskURI, diskURISupportedManaged))
	}

	if azure.Kind != nil && *azure.Kind != core.AzureManagedDisk && strings.Index(azure.DataDiskURI, "https://") != 0 {
		allErrs = append(allErrs, field.NotSupported(fldPath.Child("diskURI"), azure.DataDiskURI, diskURISupportedblob))
	}

	return allErrs
}

func validateVsphereVolumeSource(cd *core.VsphereVirtualDiskVolumeSource, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if len(cd.VolumePath) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("volumePath"), ""))
	}
	return allErrs
}

func validatePhotonPersistentDiskVolumeSource(cd *core.PhotonPersistentDiskVolumeSource, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if len(cd.PdID) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("pdID"), ""))
	}
	return allErrs
}

func validatePortworxVolumeSource(pwx *core.PortworxVolumeSource, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if len(pwx.VolumeID) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("volumeID"), ""))
	}
	return allErrs
}

func validateScaleIOVolumeSource(sio *core.ScaleIOVolumeSource, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if sio.Gateway == "" {
		allErrs = append(allErrs, field.Required(fldPath.Child("gateway"), ""))
	}
	if sio.System == "" {
		allErrs = append(allErrs, field.Required(fldPath.Child("system"), ""))
	}
	if sio.VolumeName == "" {
		allErrs = append(allErrs, field.Required(fldPath.Child("volumeName"), ""))
	}
	return allErrs
}

func validateScaleIOPersistentVolumeSource(sio *core.ScaleIOPersistentVolumeSource, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if sio.Gateway == "" {
		allErrs = append(allErrs, field.Required(fldPath.Child("gateway"), ""))
	}
	if sio.System == "" {
		allErrs = append(allErrs, field.Required(fldPath.Child("system"), ""))
	}
	if sio.VolumeName == "" {
		allErrs = append(allErrs, field.Required(fldPath.Child("volumeName"), ""))
	}
	return allErrs
}

func validateLocalVolumeSource(ls *core.LocalVolumeSource, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if ls.Path == "" {
		allErrs = append(allErrs, field.Required(fldPath.Child("path"), ""))
		return allErrs
	}

	allErrs = append(allErrs, validatePathNoBacksteps(ls.Path, fldPath.Child("path"))...)
	return allErrs
}

func validateStorageOSVolumeSource(storageos *core.StorageOSVolumeSource, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if len(storageos.VolumeName) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("volumeName"), ""))
	} else {
		allErrs = append(allErrs, ValidateDNS1123Label(storageos.VolumeName, fldPath.Child("volumeName"))...)
	}
	if len(storageos.VolumeNamespace) > 0 {
		allErrs = append(allErrs, ValidateDNS1123Label(storageos.VolumeNamespace, fldPath.Child("volumeNamespace"))...)
	}
	if storageos.SecretRef != nil {
		if len(storageos.SecretRef.Name) == 0 {
			allErrs = append(allErrs, field.Required(fldPath.Child("secretRef", "name"), ""))
		}
	}
	return allErrs
}

func validateStorageOSPersistentVolumeSource(storageos *core.StorageOSPersistentVolumeSource, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if len(storageos.VolumeName) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("volumeName"), ""))
	} else {
		allErrs = append(allErrs, ValidateDNS1123Label(storageos.VolumeName, fldPath.Child("volumeName"))...)
	}
	if len(storageos.VolumeNamespace) > 0 {
		allErrs = append(allErrs, ValidateDNS1123Label(storageos.VolumeNamespace, fldPath.Child("volumeNamespace"))...)
	}
	if storageos.SecretRef != nil {
		if len(storageos.SecretRef.Name) == 0 {
			allErrs = append(allErrs, field.Required(fldPath.Child("secretRef", "name"), ""))
		}
		if len(storageos.SecretRef.Namespace) == 0 {
			allErrs = append(allErrs, field.Required(fldPath.Child("secretRef", "namespace"), ""))
		}
	}
	return allErrs
}

// validatePVSecretReference check whether provided SecretReference object is valid in terms of secret name and namespace.

func validatePVSecretReference(secretRef *core.SecretReference, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	if len(secretRef.Name) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("name"), ""))
	} else {
		allErrs = append(allErrs, ValidateDNS1123Subdomain(secretRef.Name, fldPath.Child("name"))...)
	}

	if len(secretRef.Namespace) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("namespace"), ""))
	} else {
		allErrs = append(allErrs, ValidateDNS1123Label(secretRef.Namespace, fldPath.Child("namespace"))...)
	}
	return allErrs
}

func ValidateCSIDriverName(driverName string, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}

	if len(driverName) == 0 {
		allErrs = append(allErrs, field.Required(fldPath, ""))
	}

	if len(driverName) > 63 {
		allErrs = append(allErrs, field.TooLong(fldPath, "" /*unused*/, 63))
	}

	for _, msg := range validation.IsDNS1123Subdomain(strings.ToLower(driverName)) {
		allErrs = append(allErrs, field.Invalid(fldPath, driverName, msg))
	}

	return allErrs
}

func validateCSIPersistentVolumeSource(csi *core.CSIPersistentVolumeSource, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}

	allErrs = append(allErrs, ValidateCSIDriverName(csi.Driver, fldPath.Child("driver"))...)

	if len(csi.VolumeHandle) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("volumeHandle"), ""))
	}
	if csi.ControllerPublishSecretRef != nil {
		allErrs = append(allErrs, validatePVSecretReference(csi.ControllerPublishSecretRef, fldPath.Child("controllerPublishSecretRef"))...)
	}
	if csi.ControllerExpandSecretRef != nil {
		allErrs = append(allErrs, validatePVSecretReference(csi.ControllerExpandSecretRef, fldPath.Child("controllerExpandSecretRef"))...)
	}
	if csi.NodePublishSecretRef != nil {
		allErrs = append(allErrs, validatePVSecretReference(csi.NodePublishSecretRef, fldPath.Child("nodePublishSecretRef"))...)
	}
	if csi.NodeExpandSecretRef != nil {
		allErrs = append(allErrs, validatePVSecretReference(csi.NodeExpandSecretRef, fldPath.Child("nodeExpandSecretRef"))...)
	}
	return allErrs
}

func validateCSIVolumeSource(csi *core.CSIVolumeSource, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	allErrs = append(allErrs, ValidateCSIDriverName(csi.Driver, fldPath.Child("driver"))...)

	if csi.NodePublishSecretRef != nil {
		if len(csi.NodePublishSecretRef.Name) == 0 {
			allErrs = append(allErrs, field.Required(fldPath.Child("nodePublishSecretRef", "name"), ""))
		} else {
			for _, msg := range ValidateSecretName(csi.NodePublishSecretRef.Name, false) {
				allErrs = append(allErrs, field.Invalid(fldPath.Child("name"), csi.NodePublishSecretRef.Name, msg))
			}
		}
	}

	return allErrs
}

func validateEphemeralVolumeSource(ephemeral *core.EphemeralVolumeSource, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if ephemeral.VolumeClaimTemplate == nil {
		allErrs = append(allErrs, field.Required(fldPath.Child("volumeClaimTemplate"), ""))
	} else {
		opts := ValidationOptionsForPersistentVolumeClaimTemplate(ephemeral.VolumeClaimTemplate, nil)
		allErrs = append(allErrs, ValidatePersistentVolumeClaimTemplate(ephemeral.VolumeClaimTemplate, fldPath.Child("volumeClaimTemplate"), opts)...)
	}
	return allErrs
}

// ValidatePersistentVolumeClaimTemplate verifies that the embedded object meta and spec are valid.
// Checking of the object data is very minimal because only labels and annotations are used.
func ValidatePersistentVolumeClaimTemplate(claimTemplate *core.PersistentVolumeClaimTemplate, fldPath *field.Path, opts PersistentVolumeClaimSpecValidationOptions) field.ErrorList {
	allErrs := ValidateTemplateObjectMeta(&claimTemplate.ObjectMeta, fldPath.Child("metadata"))
	allErrs = append(allErrs, ValidatePersistentVolumeClaimSpec(&claimTemplate.Spec, fldPath.Child("spec"), opts)...)
	return allErrs
}

func ValidateTemplateObjectMeta(objMeta *metav1.ObjectMeta, fldPath *field.Path) field.ErrorList {
	allErrs := apimachineryvalidation.ValidateAnnotations(objMeta.Annotations, fldPath.Child("annotations"))
	allErrs = append(allErrs, unversionedvalidation.ValidateLabels(objMeta.Labels, fldPath.Child("labels"))...)
	// All other fields are not supported and thus must not be set
	// to avoid confusion.  We could reject individual fields,
	// but then adding a new one to ObjectMeta wouldn't be checked
	// unless this code gets updated. Instead, we ensure that
	// only allowed fields are set via reflection.
	allErrs = append(allErrs, validateFieldAllowList(*objMeta, allowedTemplateObjectMetaFields, "cannot be set", fldPath)...)
	return allErrs
}

var allowedTemplateObjectMetaFields = map[string]bool{
	"Annotations": true,
	"Labels":      true,
}

// PersistentVolumeSpecValidationOptions contains the different settings for PeristentVolume validation
type PersistentVolumeSpecValidationOptions struct {
	// Allow users to modify the class of volume attributes
	EnableVolumeAttributesClass bool
	// Allow invalid label-value in RequiredNodeSelector
	AllowInvalidLabelValueInRequiredNodeAffinity bool
}

// ValidatePersistentVolumeName checks that a name is appropriate for a
// PersistentVolumeName object.
var ValidatePersistentVolumeName = apimachineryvalidation.NameIsDNSSubdomain

var supportedAccessModes = sets.New(
	core.ReadWriteOnce,
	core.ReadOnlyMany,
	core.ReadWriteMany,
	core.ReadWriteOncePod)

var supportedReclaimPolicy = sets.New(
	core.PersistentVolumeReclaimDelete,
	core.PersistentVolumeReclaimRecycle,
	core.PersistentVolumeReclaimRetain)

var supportedVolumeModes = sets.New(core.PersistentVolumeBlock, core.PersistentVolumeFilesystem)

func ValidationOptionsForPersistentVolume(pv, oldPv *core.PersistentVolume) PersistentVolumeSpecValidationOptions {
	opts := PersistentVolumeSpecValidationOptions{
		EnableVolumeAttributesClass:                  utilfeature.DefaultMutableFeatureGate.Enabled(features.VolumeAttributesClass),
		AllowInvalidLabelValueInRequiredNodeAffinity: false,
	}
	if oldPv != nil && oldPv.Spec.VolumeAttributesClassName != nil {
		opts.EnableVolumeAttributesClass = true
	}
	if oldPv != nil && oldPv.Spec.NodeAffinity != nil &&
		oldPv.Spec.NodeAffinity.Required != nil {
		terms := oldPv.Spec.NodeAffinity.Required.NodeSelectorTerms
		opts.AllowInvalidLabelValueInRequiredNodeAffinity = helper.HasInvalidLabelValueInNodeSelectorTerms(terms)
	}
	return opts
}

func ValidatePersistentVolumeSpec(pvSpec *core.PersistentVolumeSpec, pvName string, validateInlinePersistentVolumeSpec bool, fldPath *field.Path, opts PersistentVolumeSpecValidationOptions) field.ErrorList {
	allErrs := field.ErrorList{}

	if validateInlinePersistentVolumeSpec {
		if pvSpec.ClaimRef != nil {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("claimRef"), "may not be specified in the context of inline volumes"))
		}
		if len(pvSpec.Capacity) != 0 {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("capacity"), "may not be specified in the context of inline volumes"))
		}
		if pvSpec.CSI == nil {
			allErrs = append(allErrs, field.Required(fldPath.Child("csi"), "has to be specified in the context of inline volumes"))
		}
	}

	if len(pvSpec.AccessModes) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("accessModes"), ""))
	}

	foundReadWriteOncePod, foundNonReadWriteOncePod := false, false
	for _, mode := range pvSpec.AccessModes {
		if !supportedAccessModes.Has(mode) {
			allErrs = append(allErrs, field.NotSupported(fldPath.Child("accessModes"), mode, sets.List(supportedAccessModes)))
		}

		if mode == core.ReadWriteOncePod {
			foundReadWriteOncePod = true
		} else if supportedAccessModes.Has(mode) {
			foundNonReadWriteOncePod = true
		}
	}
	if foundReadWriteOncePod && foundNonReadWriteOncePod {
		allErrs = append(allErrs, field.Forbidden(fldPath.Child("accessModes"), "may not use ReadWriteOncePod with other access modes"))
	}

	if !validateInlinePersistentVolumeSpec {
		if len(pvSpec.Capacity) == 0 {
			allErrs = append(allErrs, field.Required(fldPath.Child("capacity"), ""))
		}

		if _, ok := pvSpec.Capacity[core.ResourceStorage]; !ok || len(pvSpec.Capacity) > 1 {
			allErrs = append(allErrs, field.NotSupported(fldPath.Child("capacity"), pvSpec.Capacity, []core.ResourceName{core.ResourceStorage}))
		}
		capPath := fldPath.Child("capacity")
		for r, qty := range pvSpec.Capacity {
			allErrs = append(allErrs, validateBasicResource(qty, capPath.Key(string(r)))...)
			allErrs = append(allErrs, ValidatePositiveQuantityValue(qty, capPath.Key(string(r)))...)
		}
	}

	if len(pvSpec.PersistentVolumeReclaimPolicy) > 0 {
		if validateInlinePersistentVolumeSpec {
			if pvSpec.PersistentVolumeReclaimPolicy != core.PersistentVolumeReclaimRetain {
				allErrs = append(allErrs, field.Forbidden(fldPath.Child("persistentVolumeReclaimPolicy"), "may only be "+string(core.PersistentVolumeReclaimRetain)+" in the context of inline volumes"))
			}
		} else {
			if !supportedReclaimPolicy.Has(pvSpec.PersistentVolumeReclaimPolicy) {
				allErrs = append(allErrs, field.NotSupported(fldPath.Child("persistentVolumeReclaimPolicy"), pvSpec.PersistentVolumeReclaimPolicy, sets.List(supportedReclaimPolicy)))
			}
		}
	}

	var nodeAffinitySpecified bool
	var errs field.ErrorList
	if pvSpec.NodeAffinity != nil {
		if validateInlinePersistentVolumeSpec {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("nodeAffinity"), "may not be specified in the context of inline volumes"))
		} else {
			nodeAffinitySpecified, errs = validateVolumeNodeAffinity(pvSpec.NodeAffinity, opts, fldPath.Child("nodeAffinity"))
			allErrs = append(allErrs, errs...)
		}
	}
	numVolumes := 0
	if pvSpec.HostPath != nil {
		if numVolumes > 0 {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("hostPath"), "may not specify more than 1 volume type"))
		} else {
			numVolumes++
			allErrs = append(allErrs, validateHostPathVolumeSource(pvSpec.HostPath, fldPath.Child("hostPath"))...)
		}
	}
	if pvSpec.GCEPersistentDisk != nil {
		if numVolumes > 0 {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("gcePersistentDisk"), "may not specify more than 1 volume type"))
		} else {
			numVolumes++
			allErrs = append(allErrs, validateGCEPersistentDiskVolumeSource(pvSpec.GCEPersistentDisk, fldPath.Child("persistentDisk"))...)
		}
	}
	if pvSpec.AWSElasticBlockStore != nil {
		if numVolumes > 0 {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("awsElasticBlockStore"), "may not specify more than 1 volume type"))
		} else {
			numVolumes++
			allErrs = append(allErrs, validateAWSElasticBlockStoreVolumeSource(pvSpec.AWSElasticBlockStore, fldPath.Child("awsElasticBlockStore"))...)
		}
	}
	if pvSpec.Glusterfs != nil {
		if numVolumes > 0 {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("glusterfs"), "may not specify more than 1 volume type"))
		} else {
			numVolumes++
			allErrs = append(allErrs, validateGlusterfsPersistentVolumeSource(pvSpec.Glusterfs, fldPath.Child("glusterfs"))...)
		}
	}
	if pvSpec.Flocker != nil {
		if numVolumes > 0 {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("flocker"), "may not specify more than 1 volume type"))
		} else {
			numVolumes++
			allErrs = append(allErrs, validateFlockerVolumeSource(pvSpec.Flocker, fldPath.Child("flocker"))...)
		}
	}
	if pvSpec.NFS != nil {
		if numVolumes > 0 {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("nfs"), "may not specify more than 1 volume type"))
		} else {
			numVolumes++
			allErrs = append(allErrs, validateNFSVolumeSource(pvSpec.NFS, fldPath.Child("nfs"))...)
		}
	}
	if pvSpec.RBD != nil {
		if numVolumes > 0 {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("rbd"), "may not specify more than 1 volume type"))
		} else {
			numVolumes++
			allErrs = append(allErrs, validateRBDPersistentVolumeSource(pvSpec.RBD, fldPath.Child("rbd"))...)
		}
	}
	if pvSpec.Quobyte != nil {
		if numVolumes > 0 {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("quobyte"), "may not specify more than 1 volume type"))
		} else {
			numVolumes++
			allErrs = append(allErrs, validateQuobyteVolumeSource(pvSpec.Quobyte, fldPath.Child("quobyte"))...)
		}
	}
	if pvSpec.CephFS != nil {
		if numVolumes > 0 {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("cephFS"), "may not specify more than 1 volume type"))
		} else {
			numVolumes++
			allErrs = append(allErrs, validateCephFSPersistentVolumeSource(pvSpec.CephFS, fldPath.Child("cephfs"))...)
		}
	}
	if pvSpec.ISCSI != nil {
		if numVolumes > 0 {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("iscsi"), "may not specify more than 1 volume type"))
		} else {
			numVolumes++
			allErrs = append(allErrs, validateISCSIPersistentVolumeSource(pvSpec.ISCSI, pvName, fldPath.Child("iscsi"))...)
		}
	}
	if pvSpec.Cinder != nil {
		if numVolumes > 0 {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("cinder"), "may not specify more than 1 volume type"))
		} else {
			numVolumes++
			allErrs = append(allErrs, validateCinderPersistentVolumeSource(pvSpec.Cinder, fldPath.Child("cinder"))...)
		}
	}
	if pvSpec.FC != nil {
		if numVolumes > 0 {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("fc"), "may not specify more than 1 volume type"))
		} else {
			numVolumes++
			allErrs = append(allErrs, validateFCVolumeSource(pvSpec.FC, fldPath.Child("fc"))...)
		}
	}
	if pvSpec.FlexVolume != nil {
		numVolumes++
		allErrs = append(allErrs, validateFlexPersistentVolumeSource(pvSpec.FlexVolume, fldPath.Child("flexVolume"))...)
	}
	if pvSpec.AzureFile != nil {
		if numVolumes > 0 {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("azureFile"), "may not specify more than 1 volume type"))

		} else {
			numVolumes++
			allErrs = append(allErrs, validateAzureFilePV(pvSpec.AzureFile, fldPath.Child("azureFile"))...)
		}
	}

	if pvSpec.VsphereVolume != nil {
		if numVolumes > 0 {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("vsphereVolume"), "may not specify more than 1 volume type"))
		} else {
			numVolumes++
			allErrs = append(allErrs, validateVsphereVolumeSource(pvSpec.VsphereVolume, fldPath.Child("vsphereVolume"))...)
		}
	}
	if pvSpec.PhotonPersistentDisk != nil {
		if numVolumes > 0 {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("photonPersistentDisk"), "may not specify more than 1 volume type"))
		} else {
			numVolumes++
			allErrs = append(allErrs, validatePhotonPersistentDiskVolumeSource(pvSpec.PhotonPersistentDisk, fldPath.Child("photonPersistentDisk"))...)
		}
	}
	if pvSpec.PortworxVolume != nil {
		if numVolumes > 0 {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("portworxVolume"), "may not specify more than 1 volume type"))
		} else {
			numVolumes++
			allErrs = append(allErrs, validatePortworxVolumeSource(pvSpec.PortworxVolume, fldPath.Child("portworxVolume"))...)
		}
	}
	if pvSpec.AzureDisk != nil {
		if numVolumes > 0 {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("azureDisk"), "may not specify more than 1 volume type"))
		} else {
			numVolumes++
			allErrs = append(allErrs, validateAzureDisk(pvSpec.AzureDisk, fldPath.Child("azureDisk"))...)
		}
	}
	if pvSpec.ScaleIO != nil {
		if numVolumes > 0 {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("scaleIO"), "may not specify more than 1 volume type"))
		} else {
			numVolumes++
			allErrs = append(allErrs, validateScaleIOPersistentVolumeSource(pvSpec.ScaleIO, fldPath.Child("scaleIO"))...)
		}
	}
	if pvSpec.Local != nil {
		if numVolumes > 0 {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("local"), "may not specify more than 1 volume type"))
		} else {
			numVolumes++
			allErrs = append(allErrs, validateLocalVolumeSource(pvSpec.Local, fldPath.Child("local"))...)
			// NodeAffinity is required
			if !nodeAffinitySpecified {
				allErrs = append(allErrs, field.Required(fldPath.Child("nodeAffinity"), "Local volume requires node affinity"))
			}
		}
	}
	if pvSpec.StorageOS != nil {
		if numVolumes > 0 {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("storageos"), "may not specify more than 1 volume type"))
		} else {
			numVolumes++
			allErrs = append(allErrs, validateStorageOSPersistentVolumeSource(pvSpec.StorageOS, fldPath.Child("storageos"))...)
		}
	}

	if pvSpec.CSI != nil {
		if numVolumes > 0 {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("csi"), "may not specify more than 1 volume type"))
		} else {
			numVolumes++
			allErrs = append(allErrs, validateCSIPersistentVolumeSource(pvSpec.CSI, fldPath.Child("csi"))...)
		}
	}

	if numVolumes == 0 {
		allErrs = append(allErrs, field.Required(fldPath, "must specify a volume type"))
	}

	// do not allow hostPath mounts of '/' to have a 'recycle' reclaim policy
	if pvSpec.HostPath != nil && path.Clean(pvSpec.HostPath.Path) == "/" && pvSpec.PersistentVolumeReclaimPolicy == core.PersistentVolumeReclaimRecycle {
		allErrs = append(allErrs, field.Forbidden(fldPath.Child("persistentVolumeReclaimPolicy"), "may not be 'recycle' for a hostPath mount of '/'"))
	}

	if len(pvSpec.StorageClassName) > 0 {
		if validateInlinePersistentVolumeSpec {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("storageClassName"), "may not be specified in the context of inline volumes"))
		} else {
			for _, msg := range ValidateClassName(pvSpec.StorageClassName, false) {
				allErrs = append(allErrs, field.Invalid(fldPath.Child("storageClassName"), pvSpec.StorageClassName, msg))
			}
		}
	}
	if pvSpec.VolumeMode != nil {
		if validateInlinePersistentVolumeSpec {
			if *pvSpec.VolumeMode != core.PersistentVolumeFilesystem {
				allErrs = append(allErrs, field.Forbidden(fldPath.Child("volumeMode"), "may not specify volumeMode other than "+string(core.PersistentVolumeFilesystem)+" in the context of inline volumes"))
			}
		} else {
			if !supportedVolumeModes.Has(*pvSpec.VolumeMode) {
				allErrs = append(allErrs, field.NotSupported(fldPath.Child("volumeMode"), *pvSpec.VolumeMode, sets.List(supportedVolumeModes)))
			}
		}
	}
	if pvSpec.VolumeAttributesClassName != nil && opts.EnableVolumeAttributesClass {
		if len(*pvSpec.VolumeAttributesClassName) == 0 {
			allErrs = append(allErrs, field.Required(fldPath.Child("volumeAttributesClassName"), "an empty string is disallowed"))
		} else {
			for _, msg := range ValidateClassName(*pvSpec.VolumeAttributesClassName, false) {
				allErrs = append(allErrs, field.Invalid(fldPath.Child("volumeAttributesClassName"), *pvSpec.VolumeAttributesClassName, msg))
			}
		}
		if pvSpec.CSI == nil {
			allErrs = append(allErrs, field.Required(fldPath.Child("csi"), "has to be specified when using volumeAttributesClassName"))
		}
	}
	return allErrs
}

func ValidatePersistentVolume(pv *core.PersistentVolume, opts PersistentVolumeSpecValidationOptions) field.ErrorList {
	metaPath := field.NewPath("metadata")
	allErrs := ValidateObjectMeta(&pv.ObjectMeta, false, ValidatePersistentVolumeName, metaPath)
	allErrs = append(allErrs, ValidatePersistentVolumeSpec(&pv.Spec, pv.ObjectMeta.Name, false, field.NewPath("spec"), opts)...)
	return allErrs
}

// ValidatePersistentVolumeUpdate tests to see if the update is legal for an end user to make.
// newPv is updated with fields that cannot be changed.
func ValidatePersistentVolumeUpdate(newPv, oldPv *core.PersistentVolume, opts PersistentVolumeSpecValidationOptions) field.ErrorList {
	allErrs := ValidatePersistentVolume(newPv, opts)

	// if oldPV does not have ControllerExpandSecretRef then allow it to be set
	if (oldPv.Spec.CSI != nil && oldPv.Spec.CSI.ControllerExpandSecretRef == nil) &&
		(newPv.Spec.CSI != nil && newPv.Spec.CSI.ControllerExpandSecretRef != nil) {
		newPv = newPv.DeepCopy()
		newPv.Spec.CSI.ControllerExpandSecretRef = nil
	}

	// PersistentVolumeSource should be immutable after creation.
	if !apiequality.Semantic.DeepEqual(newPv.Spec.PersistentVolumeSource, oldPv.Spec.PersistentVolumeSource) {
		pvcSourceDiff := cmp.Diff(oldPv.Spec.PersistentVolumeSource, newPv.Spec.PersistentVolumeSource)
		allErrs = append(allErrs, field.Forbidden(field.NewPath("spec", "persistentvolumesource"), fmt.Sprintf("spec.persistentvolumesource is immutable after creation\n%v", pvcSourceDiff)))
	}
	allErrs = append(allErrs, ValidateImmutableField(newPv.Spec.VolumeMode, oldPv.Spec.VolumeMode, field.NewPath("volumeMode"))...)

	// Allow setting NodeAffinity if oldPv NodeAffinity was not set
	if oldPv.Spec.NodeAffinity != nil {
		allErrs = append(allErrs, validatePvNodeAffinity(newPv.Spec.NodeAffinity, oldPv.Spec.NodeAffinity, field.NewPath("nodeAffinity"))...)
	}

	if !apiequality.Semantic.DeepEqual(oldPv.Spec.VolumeAttributesClassName, newPv.Spec.VolumeAttributesClassName) {
		if !utilfeature.DefaultFeatureGate.Enabled(features.VolumeAttributesClass) {
			allErrs = append(allErrs, field.Forbidden(field.NewPath("spec", "volumeAttributesClassName"), "update is forbidden when the VolumeAttributesClass feature gate is disabled"))
		}
		if opts.EnableVolumeAttributesClass {
			if oldPv.Spec.VolumeAttributesClassName != nil && newPv.Spec.VolumeAttributesClassName == nil {
				allErrs = append(allErrs, field.Forbidden(field.NewPath("spec", "volumeAttributesClassName"), "update from non-nil value to nil is forbidden"))
			}
		}
	}

	return allErrs
}

// ValidatePersistentVolumeStatusUpdate tests to see if the status update is legal for an end user to make.
func ValidatePersistentVolumeStatusUpdate(newPv, oldPv *core.PersistentVolume) field.ErrorList {
	allErrs := ValidateObjectMetaUpdate(&newPv.ObjectMeta, &oldPv.ObjectMeta, field.NewPath("metadata"))
	if len(newPv.ResourceVersion) == 0 {
		allErrs = append(allErrs, field.Required(field.NewPath("resourceVersion"), ""))
	}
	return allErrs
}

type PersistentVolumeClaimSpecValidationOptions struct {
	// Allow users to recover from previously failing expansion operation
	EnableRecoverFromExpansionFailure bool
	// Allow to validate the label value of the label selector
	AllowInvalidLabelValueInSelector bool
	// Allow to validate the API group of the data source and data source reference
	AllowInvalidAPIGroupInDataSourceOrRef bool
	// Allow users to modify the class of volume attributes
	EnableVolumeAttributesClass bool
}

func ValidationOptionsForPersistentVolumeClaim(pvc, oldPvc *core.PersistentVolumeClaim) PersistentVolumeClaimSpecValidationOptions {
	opts := PersistentVolumeClaimSpecValidationOptions{
		EnableRecoverFromExpansionFailure: utilfeature.DefaultFeatureGate.Enabled(features.RecoverVolumeExpansionFailure),
		AllowInvalidLabelValueInSelector:  false,
		EnableVolumeAttributesClass:       utilfeature.DefaultFeatureGate.Enabled(features.VolumeAttributesClass),
	}
	if oldPvc == nil {
		// If there's no old PVC, use the options based solely on feature enablement
		return opts
	}

	// If the old object had an invalid API group in the data source or data source reference, continue to allow it in the new object
	opts.AllowInvalidAPIGroupInDataSourceOrRef = allowInvalidAPIGroupInDataSourceOrRef(&oldPvc.Spec)

	if oldPvc.Spec.VolumeAttributesClassName != nil {
		// If the old object had a volume attributes class, continue to validate it in the new object.
		opts.EnableVolumeAttributesClass = true
	}

	labelSelectorValidationOpts := unversionedvalidation.LabelSelectorValidationOptions{
		AllowInvalidLabelValueInSelector: opts.AllowInvalidLabelValueInSelector,
	}
	if len(unversionedvalidation.ValidateLabelSelector(oldPvc.Spec.Selector, labelSelectorValidationOpts, nil)) > 0 {
		// If the old object had an invalid label selector, continue to allow it in the new object
		opts.AllowInvalidLabelValueInSelector = true
	}

	if helper.ClaimContainsAllocatedResources(oldPvc) ||
		helper.ClaimContainsAllocatedResourceStatus(oldPvc) {
		opts.EnableRecoverFromExpansionFailure = true
	}
	return opts
}

func ValidationOptionsForPersistentVolumeClaimTemplate(claimTemplate, oldClaimTemplate *core.PersistentVolumeClaimTemplate) PersistentVolumeClaimSpecValidationOptions {
	opts := PersistentVolumeClaimSpecValidationOptions{
		AllowInvalidLabelValueInSelector: false,
		EnableVolumeAttributesClass:      utilfeature.DefaultFeatureGate.Enabled(features.VolumeAttributesClass),
	}
	if oldClaimTemplate == nil {
		// If there's no old PVC template, use the options based solely on feature enablement
		return opts
	}
	labelSelectorValidationOpts := unversionedvalidation.LabelSelectorValidationOptions{
		AllowInvalidLabelValueInSelector: opts.AllowInvalidLabelValueInSelector,
	}
	if len(unversionedvalidation.ValidateLabelSelector(oldClaimTemplate.Spec.Selector, labelSelectorValidationOpts, nil)) > 0 {
		// If the old object had an invalid label selector, continue to allow it in the new object
		opts.AllowInvalidLabelValueInSelector = true
	}
	return opts
}

// allowInvalidAPIGroupInDataSourceOrRef returns true if the spec contains a data source or data source reference with an API group
func allowInvalidAPIGroupInDataSourceOrRef(spec *core.PersistentVolumeClaimSpec) bool {
	if spec.DataSource != nil && spec.DataSource.APIGroup != nil {
		return true
	}
	if spec.DataSourceRef != nil && spec.DataSourceRef.APIGroup != nil {
		return true
	}
	return false
}

// ValidatePersistentVolumeClaim validates a PersistentVolumeClaim
func ValidatePersistentVolumeClaim(pvc *core.PersistentVolumeClaim, opts PersistentVolumeClaimSpecValidationOptions) field.ErrorList {
	allErrs := ValidateObjectMeta(&pvc.ObjectMeta, true, ValidatePersistentVolumeName, field.NewPath("metadata"))
	allErrs = append(allErrs, ValidatePersistentVolumeClaimSpec(&pvc.Spec, field.NewPath("spec"), opts)...)
	return allErrs
}

// validateDataSource validates a DataSource/DataSourceRef in a PersistentVolumeClaimSpec
func validateDataSource(dataSource *core.TypedLocalObjectReference, fldPath *field.Path, allowInvalidAPIGroupInDataSourceOrRef bool) field.ErrorList {
	allErrs := field.ErrorList{}

	if len(dataSource.Name) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("name"), ""))
	}
	if len(dataSource.Kind) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("kind"), ""))
	}
	apiGroup := ""
	if dataSource.APIGroup != nil {
		apiGroup = *dataSource.APIGroup
	}
	if len(apiGroup) == 0 && dataSource.Kind != "PersistentVolumeClaim" {
		allErrs = append(allErrs, field.Invalid(fldPath, dataSource.Kind, "must be 'PersistentVolumeClaim' when referencing the default apiGroup"))
	}
	if len(apiGroup) > 0 && !allowInvalidAPIGroupInDataSourceOrRef {
		for _, errString := range validation.IsDNS1123Subdomain(apiGroup) {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("apiGroup"), apiGroup, errString))
		}
	}

	return allErrs
}

// validateDataSourceRef validates a DataSourceRef in a PersistentVolumeClaimSpec
func validateDataSourceRef(dataSourceRef *core.TypedObjectReference, fldPath *field.Path, allowInvalidAPIGroupInDataSourceOrRef bool) field.ErrorList {
	allErrs := field.ErrorList{}

	if len(dataSourceRef.Name) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("name"), ""))
	}
	if len(dataSourceRef.Kind) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("kind"), ""))
	}
	apiGroup := ""
	if dataSourceRef.APIGroup != nil {
		apiGroup = *dataSourceRef.APIGroup
	}
	if len(apiGroup) == 0 && dataSourceRef.Kind != "PersistentVolumeClaim" {
		allErrs = append(allErrs, field.Invalid(fldPath, dataSourceRef.Kind, "must be 'PersistentVolumeClaim' when referencing the default apiGroup"))
	}
	if len(apiGroup) > 0 && !allowInvalidAPIGroupInDataSourceOrRef {
		for _, errString := range validation.IsDNS1123Subdomain(apiGroup) {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("apiGroup"), apiGroup, errString))
		}
	}

	if dataSourceRef.Namespace != nil && len(*dataSourceRef.Namespace) > 0 {
		for _, msg := range ValidateNameFunc(ValidateNamespaceName)(*dataSourceRef.Namespace, false) {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("namespace"), *dataSourceRef.Namespace, msg))
		}
	}

	return allErrs
}

// ValidatePersistentVolumeClaimSpec validates a PersistentVolumeClaimSpec
func ValidatePersistentVolumeClaimSpec(spec *core.PersistentVolumeClaimSpec, fldPath *field.Path, opts PersistentVolumeClaimSpecValidationOptions) field.ErrorList {
	allErrs := field.ErrorList{}
	if len(spec.AccessModes) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("accessModes"), "at least 1 access mode is required"))
	}
	if spec.Selector != nil {
		labelSelectorValidationOpts := unversionedvalidation.LabelSelectorValidationOptions{
			AllowInvalidLabelValueInSelector: opts.AllowInvalidLabelValueInSelector,
		}
		allErrs = append(allErrs, unversionedvalidation.ValidateLabelSelector(spec.Selector, labelSelectorValidationOpts, fldPath.Child("selector"))...)
	}

	foundReadWriteOncePod, foundNonReadWriteOncePod := false, false
	for _, mode := range spec.AccessModes {
		if !supportedAccessModes.Has(mode) {
			allErrs = append(allErrs, field.NotSupported(fldPath.Child("accessModes"), mode, sets.List(supportedAccessModes)))
		}

		if mode == core.ReadWriteOncePod {
			foundReadWriteOncePod = true
		} else if supportedAccessModes.Has(mode) {
			foundNonReadWriteOncePod = true
		}
	}
	if foundReadWriteOncePod && foundNonReadWriteOncePod {
		allErrs = append(allErrs, field.Forbidden(fldPath.Child("accessModes"), "may not use ReadWriteOncePod with other access modes"))
	}

	storageValue, ok := spec.Resources.Requests[core.ResourceStorage]
	if !ok {
		allErrs = append(allErrs, field.Required(fldPath.Child("resources").Key(string(core.ResourceStorage)), ""))
	} else if errs := ValidatePositiveQuantityValue(storageValue, fldPath.Child("resources").Key(string(core.ResourceStorage))); len(errs) > 0 {
		allErrs = append(allErrs, errs...)
	} else {
		allErrs = append(allErrs, ValidateResourceQuantityValue(core.ResourceStorage, storageValue, fldPath.Child("resources").Key(string(core.ResourceStorage)))...)
	}

	if spec.StorageClassName != nil && len(*spec.StorageClassName) > 0 {
		for _, msg := range ValidateClassName(*spec.StorageClassName, false) {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("storageClassName"), *spec.StorageClassName, msg))
		}
	}
	if spec.VolumeMode != nil && !supportedVolumeModes.Has(*spec.VolumeMode) {
		allErrs = append(allErrs, field.NotSupported(fldPath.Child("volumeMode"), *spec.VolumeMode, sets.List(supportedVolumeModes)))
	}

	if spec.DataSource != nil {
		allErrs = append(allErrs, validateDataSource(spec.DataSource, fldPath.Child("dataSource"), opts.AllowInvalidAPIGroupInDataSourceOrRef)...)
	}
	if spec.DataSourceRef != nil {
		allErrs = append(allErrs, validateDataSourceRef(spec.DataSourceRef, fldPath.Child("dataSourceRef"), opts.AllowInvalidAPIGroupInDataSourceOrRef)...)
	}
	if spec.DataSourceRef != nil && spec.DataSourceRef.Namespace != nil && len(*spec.DataSourceRef.Namespace) > 0 {
		if spec.DataSource != nil {
			allErrs = append(allErrs, field.Invalid(fldPath, fldPath.Child("dataSource"),
				"may not be specified when dataSourceRef.namespace is specified"))
		}
	} else if spec.DataSource != nil && spec.DataSourceRef != nil {
		if !isDataSourceEqualDataSourceRef(spec.DataSource, spec.DataSourceRef) {
			allErrs = append(allErrs, field.Invalid(fldPath, fldPath.Child("dataSource"),
				"must match dataSourceRef"))
		}
	}
	if spec.VolumeAttributesClassName != nil && len(*spec.VolumeAttributesClassName) > 0 && opts.EnableVolumeAttributesClass {
		for _, msg := range ValidateClassName(*spec.VolumeAttributesClassName, false) {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("volumeAttributesClassName"), *spec.VolumeAttributesClassName, msg))
		}
	}

	return allErrs
}

func isDataSourceEqualDataSourceRef(dataSource *core.TypedLocalObjectReference, dataSourceRef *core.TypedObjectReference) bool {
	return reflect.DeepEqual(dataSource.APIGroup, dataSourceRef.APIGroup) && dataSource.Kind == dataSourceRef.Kind && dataSource.Name == dataSourceRef.Name
}

// ValidatePersistentVolumeClaimUpdate validates an update to a PersistentVolumeClaim
func ValidatePersistentVolumeClaimUpdate(newPvc, oldPvc *core.PersistentVolumeClaim, opts PersistentVolumeClaimSpecValidationOptions) field.ErrorList {
	allErrs := ValidateObjectMetaUpdate(&newPvc.ObjectMeta, &oldPvc.ObjectMeta, field.NewPath("metadata"))
	allErrs = append(allErrs, ValidatePersistentVolumeClaim(newPvc, opts)...)
	newPvcClone := newPvc.DeepCopy()
	oldPvcClone := oldPvc.DeepCopy()

	// PVController needs to update PVC.Spec w/ VolumeName.
	// Claims are immutable in order to enforce quota, range limits, etc. without gaming the system.
	if len(oldPvc.Spec.VolumeName) == 0 {
		// volumeName changes are allowed once.
		oldPvcClone.Spec.VolumeName = newPvcClone.Spec.VolumeName // +k8s:verify-mutation:reason=clone
	}

	if validateStorageClassUpgradeFromAnnotation(oldPvcClone.Annotations, newPvcClone.Annotations,
		oldPvcClone.Spec.StorageClassName, newPvcClone.Spec.StorageClassName) {
		newPvcClone.Spec.StorageClassName = nil
		metav1.SetMetaDataAnnotation(&newPvcClone.ObjectMeta, core.BetaStorageClassAnnotation, oldPvcClone.Annotations[core.BetaStorageClassAnnotation])
	} else {
		// storageclass annotation should be immutable after creation
		// TODO: remove Beta when no longer needed
		allErrs = append(allErrs, ValidateImmutableAnnotation(newPvc.ObjectMeta.Annotations[v1.BetaStorageClassAnnotation], oldPvc.ObjectMeta.Annotations[v1.BetaStorageClassAnnotation], v1.BetaStorageClassAnnotation, field.NewPath("metadata"))...)

		// If update from annotation to attribute failed we can attempt try to validate update from nil value.
		if validateStorageClassUpgradeFromNil(oldPvc.Annotations, oldPvc.Spec.StorageClassName, newPvc.Spec.StorageClassName, opts) {
			newPvcClone.Spec.StorageClassName = oldPvcClone.Spec.StorageClassName // +k8s:verify-mutation:reason=clone
		}
		// TODO: add a specific error with a hint that storage class name can not be changed
		// (instead of letting spec comparison below return generic field forbidden error)
	}

	// lets make sure storage values are same.
	if newPvc.Status.Phase == core.ClaimBound && newPvcClone.Spec.Resources.Requests != nil {
		newPvcClone.Spec.Resources.Requests["storage"] = oldPvc.Spec.Resources.Requests["storage"] // +k8s:verify-mutation:reason=clone
	}
	// lets make sure volume attributes class name is same.
	if newPvc.Status.Phase == core.ClaimBound && newPvcClone.Spec.VolumeAttributesClassName != nil {
		newPvcClone.Spec.VolumeAttributesClassName = oldPvcClone.Spec.VolumeAttributesClassName // +k8s:verify-mutation:reason=clone
	}

	oldSize := oldPvc.Spec.Resources.Requests["storage"]
	newSize := newPvc.Spec.Resources.Requests["storage"]
	statusSize := oldPvc.Status.Capacity["storage"]

	if !apiequality.Semantic.DeepEqual(newPvcClone.Spec, oldPvcClone.Spec) {
		specDiff := cmp.Diff(oldPvcClone.Spec, newPvcClone.Spec)
		allErrs = append(allErrs, field.Forbidden(field.NewPath("spec"), fmt.Sprintf("spec is immutable after creation except resources.requests and volumeAttributesClassName for bound claims\n%v", specDiff)))
	}
	if newSize.Cmp(oldSize) < 0 {
		if !opts.EnableRecoverFromExpansionFailure {
			allErrs = append(allErrs, field.Forbidden(field.NewPath("spec", "resources", "requests", "storage"), "field can not be less than previous value"))
		} else {
			// This validation permits reducing pvc requested size up to capacity recorded in pvc.status
			// so that users can recover from volume expansion failure, but Kubernetes does not actually
			// support volume shrinking
			if newSize.Cmp(statusSize) <= 0 {
				allErrs = append(allErrs, field.Forbidden(field.NewPath("spec", "resources", "requests", "storage"), "field can not be less than status.capacity"))
			}
		}
	}

	allErrs = append(allErrs, ValidateImmutableField(newPvc.Spec.VolumeMode, oldPvc.Spec.VolumeMode, field.NewPath("volumeMode"))...)

	if !apiequality.Semantic.DeepEqual(oldPvc.Spec.VolumeAttributesClassName, newPvc.Spec.VolumeAttributesClassName) {
		if !utilfeature.DefaultFeatureGate.Enabled(features.VolumeAttributesClass) {
			allErrs = append(allErrs, field.Forbidden(field.NewPath("spec", "volumeAttributesClassName"), "update is forbidden when the VolumeAttributesClass feature gate is disabled"))
		}
		if opts.EnableVolumeAttributesClass {
			if oldPvc.Spec.VolumeAttributesClassName != nil {
				if newPvc.Spec.VolumeAttributesClassName == nil {
					allErrs = append(allErrs, field.Forbidden(field.NewPath("spec", "volumeAttributesClassName"), "update from non-nil value to nil is forbidden"))
				} else if len(*newPvc.Spec.VolumeAttributesClassName) == 0 {
					allErrs = append(allErrs, field.Forbidden(field.NewPath("spec", "volumeAttributesClassName"), "update from non-nil value to an empty string is forbidden"))
				}
			}
		}
	}

	return allErrs
}

// Provide an upgrade path from PVC with storage class specified in beta
// annotation to storage class specified in attribute. We allow update of
// StorageClassName only if following four conditions are met at the same time:
// 1. The old pvc's StorageClassAnnotation is set
// 2. The old pvc's StorageClassName is not set
// 3. The new pvc's StorageClassName is set and equal to the old value in annotation
// 4. If the new pvc's StorageClassAnnotation is set,it must be equal to the old pv/pvc's StorageClassAnnotation
func validateStorageClassUpgradeFromAnnotation(oldAnnotations, newAnnotations map[string]string, oldScName, newScName *string) bool {
	oldSc, oldAnnotationExist := oldAnnotations[core.BetaStorageClassAnnotation]
	newScInAnnotation, newAnnotationExist := newAnnotations[core.BetaStorageClassAnnotation]
	return oldAnnotationExist /* condition 1 */ &&
		oldScName == nil /* condition 2*/ &&
		(newScName != nil && *newScName == oldSc) /* condition 3 */ &&
		(!newAnnotationExist || newScInAnnotation == oldSc) /* condition 4 */
}

// Provide an upgrade path from PVC with nil storage class. We allow update of
// StorageClassName only if following four conditions are met at the same time:
// 1. The new pvc's StorageClassName is not nil
// 2. The old pvc's StorageClassName is nil
// 3. The old pvc either does not have beta annotation set, or the beta annotation matches new pvc's StorageClassName
func validateStorageClassUpgradeFromNil(oldAnnotations map[string]string, oldScName, newScName *string, opts PersistentVolumeClaimSpecValidationOptions) bool {
	oldAnnotation, oldAnnotationExist := oldAnnotations[core.BetaStorageClassAnnotation]
	return newScName != nil /* condition 1 */ &&
		oldScName == nil /* condition 2 */ &&
		(!oldAnnotationExist || *newScName == oldAnnotation) /* condition 3 */
}

func validatePersistentVolumeClaimResourceKey(value string, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	for _, msg := range validation.IsQualifiedName(value) {
		allErrs = append(allErrs, field.Invalid(fldPath, value, msg))
	}
	if len(allErrs) != 0 {
		return allErrs
	}
	// For native resource names such as - either unprefixed names or with kubernetes.io prefix,
	// only allowed value is storage
	if helper.IsNativeResource(core.ResourceName(value)) {
		if core.ResourceName(value) != core.ResourceStorage {
			return append(allErrs, field.NotSupported(fldPath, value, []core.ResourceName{core.ResourceStorage}))
		}
	}
	return allErrs
}

var resizeStatusSet = sets.New(core.PersistentVolumeClaimControllerResizeInProgress,
	core.PersistentVolumeClaimControllerResizeInfeasible,
	core.PersistentVolumeClaimNodeResizePending,
	core.PersistentVolumeClaimNodeResizeInProgress,
	core.PersistentVolumeClaimNodeResizeInfeasible)

// ValidatePersistentVolumeClaimStatusUpdate validates an update to status of a PersistentVolumeClaim
func ValidatePersistentVolumeClaimStatusUpdate(newPvc, oldPvc *core.PersistentVolumeClaim, validationOpts PersistentVolumeClaimSpecValidationOptions) field.ErrorList {
	allErrs := ValidateObjectMetaUpdate(&newPvc.ObjectMeta, &oldPvc.ObjectMeta, field.NewPath("metadata"))
	if len(newPvc.ResourceVersion) == 0 {
		allErrs = append(allErrs, field.Required(field.NewPath("resourceVersion"), ""))
	}
	if len(newPvc.Spec.AccessModes) == 0 {
		allErrs = append(allErrs, field.Required(field.NewPath("Spec", "accessModes"), ""))
	}

	capPath := field.NewPath("status", "capacity")
	for r, qty := range newPvc.Status.Capacity {
		allErrs = append(allErrs, validateBasicResource(qty, capPath.Key(string(r)))...)
	}
	if validationOpts.EnableRecoverFromExpansionFailure {
		resizeStatusPath := field.NewPath("status", "allocatedResourceStatus")
		if newPvc.Status.AllocatedResourceStatuses != nil {
			resizeStatus := newPvc.Status.AllocatedResourceStatuses
			for k, v := range resizeStatus {
				if errs := validatePersistentVolumeClaimResourceKey(k.String(), resizeStatusPath); len(errs) > 0 {
					allErrs = append(allErrs, errs...)
				}
				if !resizeStatusSet.Has(v) {
					allErrs = append(allErrs, field.NotSupported(resizeStatusPath, k, sets.List(resizeStatusSet)))
					continue
				}
			}
		}
		allocPath := field.NewPath("status", "allocatedResources")
		for r, qty := range newPvc.Status.AllocatedResources {
			if errs := validatePersistentVolumeClaimResourceKey(r.String(), allocPath); len(errs) > 0 {
				allErrs = append(allErrs, errs...)
				continue
			}

			if errs := validateBasicResource(qty, allocPath.Key(string(r))); len(errs) > 0 {
				allErrs = append(allErrs, errs...)
			} else {
				allErrs = append(allErrs, ValidateResourceQuantityValue(core.ResourceStorage, qty, allocPath.Key(string(r)))...)
			}
		}
	}
	return allErrs
}

var supportedPortProtocols = sets.New(
	core.ProtocolTCP,
	core.ProtocolUDP,
	core.ProtocolSCTP)

func validateContainerPorts(ports []core.ContainerPort, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}

	allNames := sets.Set[string]{}
	for i, port := range ports {
		idxPath := fldPath.Index(i)
		if len(port.Name) > 0 {
			if msgs := validation.IsValidPortName(port.Name); len(msgs) != 0 {
				for i = range msgs {
					allErrs = append(allErrs, field.Invalid(idxPath.Child("name"), port.Name, msgs[i]))
				}
			} else if allNames.Has(port.Name) {
				allErrs = append(allErrs, field.Duplicate(idxPath.Child("name"), port.Name))
			} else {
				allNames.Insert(port.Name)
			}
		}
		if port.ContainerPort == 0 {
			allErrs = append(allErrs, field.Required(idxPath.Child("containerPort"), ""))
		} else {
			for _, msg := range validation.IsValidPortNum(int(port.ContainerPort)) {
				allErrs = append(allErrs, field.Invalid(idxPath.Child("containerPort"), port.ContainerPort, msg))
			}
		}
		if port.HostPort != 0 {
			for _, msg := range validation.IsValidPortNum(int(port.HostPort)) {
				allErrs = append(allErrs, field.Invalid(idxPath.Child("hostPort"), port.HostPort, msg))
			}
		}
		if len(port.Protocol) == 0 {
			allErrs = append(allErrs, field.Required(idxPath.Child("protocol"), ""))
		} else if !supportedPortProtocols.Has(port.Protocol) {
			allErrs = append(allErrs, field.NotSupported(idxPath.Child("protocol"), port.Protocol, sets.List(supportedPortProtocols)))
		}
	}
	return allErrs
}

// ValidateEnv validates env vars
func ValidateEnv(vars []core.EnvVar, fldPath *field.Path, opts PodValidationOptions) field.ErrorList {
	allErrs := field.ErrorList{}

	for i, ev := range vars {
		idxPath := fldPath.Index(i)
		if len(ev.Name) == 0 {
			allErrs = append(allErrs, field.Required(idxPath.Child("name"), ""))
		} else {
			if opts.AllowRelaxedEnvironmentVariableValidation {
				for _, msg := range validation.IsRelaxedEnvVarName(ev.Name) {
					allErrs = append(allErrs, field.Invalid(idxPath.Child("name"), ev.Name, msg))
				}
			} else {
				for _, msg := range validation.IsEnvVarName(ev.Name) {
					allErrs = append(allErrs, field.Invalid(idxPath.Child("name"), ev.Name, msg))
				}
			}
		}
		allErrs = append(allErrs, validateEnvVarValueFrom(ev, idxPath.Child("valueFrom"), opts)...)
	}
	return allErrs
}

var validEnvDownwardAPIFieldPathExpressions = sets.New(
	"metadata.name",
	"metadata.namespace",
	"metadata.uid",
	"spec.nodeName",
	"spec.serviceAccountName",
	"status.hostIP",
	"status.hostIPs",
	"status.podIP",
	"status.podIPs",
)

var validContainerResourceFieldPathExpressions = sets.New(
	"limits.cpu",
	"limits.memory",
	"limits.ephemeral-storage",
	"requests.cpu",
	"requests.memory",
	"requests.ephemeral-storage",
)

var validContainerResourceFieldPathPrefixesWithDownwardAPIHugePages = sets.New(hugepagesRequestsPrefixDownwardAPI, hugepagesLimitsPrefixDownwardAPI)

const hugepagesRequestsPrefixDownwardAPI string = `requests.hugepages-`
const hugepagesLimitsPrefixDownwardAPI string = `limits.hugepages-`

func validateEnvVarValueFrom(ev core.EnvVar, fldPath *field.Path, opts PodValidationOptions) field.ErrorList {
	allErrs := field.ErrorList{}

	if ev.ValueFrom == nil {
		return allErrs
	}

	numSources := 0

	if ev.ValueFrom.FieldRef != nil {
		numSources++
		allErrs = append(allErrs, validateObjectFieldSelector(ev.ValueFrom.FieldRef, &validEnvDownwardAPIFieldPathExpressions, fldPath.Child("fieldRef"))...)
	}
	if ev.ValueFrom.ResourceFieldRef != nil {
		numSources++
		localValidContainerResourceFieldPathPrefixes := validContainerResourceFieldPathPrefixesWithDownwardAPIHugePages
		allErrs = append(allErrs, validateContainerResourceFieldSelector(ev.ValueFrom.ResourceFieldRef, &validContainerResourceFieldPathExpressions, &localValidContainerResourceFieldPathPrefixes, fldPath.Child("resourceFieldRef"), false)...)
	}
	if ev.ValueFrom.ConfigMapKeyRef != nil {
		numSources++
		allErrs = append(allErrs, validateConfigMapKeySelector(ev.ValueFrom.ConfigMapKeyRef, fldPath.Child("configMapKeyRef"))...)
	}
	if ev.ValueFrom.SecretKeyRef != nil {
		numSources++
		allErrs = append(allErrs, validateSecretKeySelector(ev.ValueFrom.SecretKeyRef, fldPath.Child("secretKeyRef"))...)
	}

	if numSources == 0 {
		allErrs = append(allErrs, field.Invalid(fldPath, "", "must specify one of: `fieldRef`, `resourceFieldRef`, `configMapKeyRef` or `secretKeyRef`"))
	} else if len(ev.Value) != 0 {
		if numSources != 0 {
			allErrs = append(allErrs, field.Invalid(fldPath, "", "may not be specified when `value` is not empty"))
		}
	} else if numSources > 1 {
		allErrs = append(allErrs, field.Invalid(fldPath, "", "may not have more than one field specified at a time"))
	}

	return allErrs
}

func validateObjectFieldSelector(fs *core.ObjectFieldSelector, expressions *sets.Set[string], fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}

	if len(fs.APIVersion) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("apiVersion"), ""))
		return allErrs
	}
	if len(fs.FieldPath) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("fieldPath"), ""))
		return allErrs
	}

	internalFieldPath, _, err := podshelper.ConvertDownwardAPIFieldLabel(fs.APIVersion, fs.FieldPath, "")
	if err != nil {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("fieldPath"), fs.FieldPath, fmt.Sprintf("error converting fieldPath: %v", err)))
		return allErrs
	}

	if path, subscript, ok := fieldpath.SplitMaybeSubscriptedPath(internalFieldPath); ok {
		switch path {
		case "metadata.annotations":
			allErrs = append(allErrs, ValidateQualifiedName(strings.ToLower(subscript), fldPath)...)
		case "metadata.labels":
			allErrs = append(allErrs, ValidateQualifiedName(subscript, fldPath)...)
		default:
			allErrs = append(allErrs, field.Invalid(fldPath, path, "does not support subscript"))
		}
	} else if !expressions.Has(path) {
		allErrs = append(allErrs, field.NotSupported(fldPath.Child("fieldPath"), path, sets.List(*expressions)))
		return allErrs
	}

	return allErrs
}

func validateContainerResourceFieldSelector(fs *core.ResourceFieldSelector, expressions *sets.Set[string], prefixes *sets.Set[string], fldPath *field.Path, volume bool) field.ErrorList {
	allErrs := field.ErrorList{}

	if volume && len(fs.ContainerName) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("containerName"), ""))
	} else if len(fs.Resource) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("resource"), ""))
	} else if !expressions.Has(fs.Resource) {
		// check if the prefix is present
		foundPrefix := false
		if prefixes != nil {
			for _, prefix := range sets.List(*prefixes) {
				if strings.HasPrefix(fs.Resource, prefix) {
					foundPrefix = true
				}
			}
		}
		if !foundPrefix {
			allErrs = append(allErrs, field.NotSupported(fldPath.Child("resource"), fs.Resource, sets.List(*expressions)))
		}
	}
	allErrs = append(allErrs, validateContainerResourceDivisor(fs.Resource, fs.Divisor, fldPath)...)
	return allErrs
}

func ValidateEnvFrom(vars []core.EnvFromSource, fldPath *field.Path, opts PodValidationOptions) field.ErrorList {
	allErrs := field.ErrorList{}
	for i, ev := range vars {
		idxPath := fldPath.Index(i)
		if len(ev.Prefix) > 0 {
			if opts.AllowRelaxedEnvironmentVariableValidation {
				for _, msg := range validation.IsRelaxedEnvVarName(ev.Prefix) {
					allErrs = append(allErrs, field.Invalid(idxPath.Child("prefix"), ev.Prefix, msg))
				}
			} else {
				for _, msg := range validation.IsEnvVarName(ev.Prefix) {
					allErrs = append(allErrs, field.Invalid(idxPath.Child("prefix"), ev.Prefix, msg))
				}
			}
		}

		numSources := 0
		if ev.ConfigMapRef != nil {
			numSources++
			allErrs = append(allErrs, validateConfigMapEnvSource(ev.ConfigMapRef, idxPath.Child("configMapRef"))...)
		}
		if ev.SecretRef != nil {
			numSources++
			allErrs = append(allErrs, validateSecretEnvSource(ev.SecretRef, idxPath.Child("secretRef"))...)
		}

		if numSources == 0 {
			allErrs = append(allErrs, field.Invalid(fldPath, "", "must specify one of: `configMapRef` or `secretRef`"))
		} else if numSources > 1 {
			allErrs = append(allErrs, field.Invalid(fldPath, "", "may not have more than one field specified at a time"))
		}
	}
	return allErrs
}

func validateConfigMapEnvSource(configMapSource *core.ConfigMapEnvSource, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if len(configMapSource.Name) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("name"), ""))
	} else {
		for _, msg := range ValidateConfigMapName(configMapSource.Name, true) {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("name"), configMapSource.Name, msg))
		}
	}
	return allErrs
}

func validateSecretEnvSource(secretSource *core.SecretEnvSource, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if len(secretSource.Name) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("name"), ""))
	} else {
		for _, msg := range ValidateSecretName(secretSource.Name, true) {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("name"), secretSource.Name, msg))
		}
	}
	return allErrs
}

var validContainerResourceDivisorForCPU = sets.New("1m", "1")
var validContainerResourceDivisorForMemory = sets.New(
	"1",
	"1k", "1M", "1G", "1T", "1P", "1E",
	"1Ki", "1Mi", "1Gi", "1Ti", "1Pi", "1Ei")
var validContainerResourceDivisorForHugePages = sets.New(
	"1",
	"1k", "1M", "1G", "1T", "1P", "1E",
	"1Ki", "1Mi", "1Gi", "1Ti", "1Pi", "1Ei")
var validContainerResourceDivisorForEphemeralStorage = sets.New(
	"1",
	"1k", "1M", "1G", "1T", "1P", "1E",
	"1Ki", "1Mi", "1Gi", "1Ti", "1Pi", "1Ei")

func validateContainerResourceDivisor(rName string, divisor resource.Quantity, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	unsetDivisor := resource.Quantity{}
	if unsetDivisor.Cmp(divisor) == 0 {
		return allErrs
	}
	switch rName {
	case "limits.cpu", "requests.cpu":
		if !validContainerResourceDivisorForCPU.Has(divisor.String()) {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("divisor"), rName, "only divisor's values 1m and 1 are supported with the cpu resource"))
		}
	case "limits.memory", "requests.memory":
		if !validContainerResourceDivisorForMemory.Has(divisor.String()) {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("divisor"), rName, "only divisor's values 1, 1k, 1M, 1G, 1T, 1P, 1E, 1Ki, 1Mi, 1Gi, 1Ti, 1Pi, 1Ei are supported with the memory resource"))
		}
	case "limits.ephemeral-storage", "requests.ephemeral-storage":
		if !validContainerResourceDivisorForEphemeralStorage.Has(divisor.String()) {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("divisor"), rName, "only divisor's values 1, 1k, 1M, 1G, 1T, 1P, 1E, 1Ki, 1Mi, 1Gi, 1Ti, 1Pi, 1Ei are supported with the local ephemeral storage resource"))
		}
	}
	if strings.HasPrefix(rName, hugepagesRequestsPrefixDownwardAPI) || strings.HasPrefix(rName, hugepagesLimitsPrefixDownwardAPI) {
		if !validContainerResourceDivisorForHugePages.Has(divisor.String()) {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("divisor"), rName, "only divisor's values 1, 1k, 1M, 1G, 1T, 1P, 1E, 1Ki, 1Mi, 1Gi, 1Ti, 1Pi, 1Ei are supported with the hugepages resource"))
		}
	}
	return allErrs
}

func validateConfigMapKeySelector(s *core.ConfigMapKeySelector, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}

	nameFn := ValidateNameFunc(ValidateSecretName)
	for _, msg := range nameFn(s.Name, false) {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("name"), s.Name, msg))
	}
	if len(s.Key) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("key"), ""))
	} else {
		for _, msg := range validation.IsConfigMapKey(s.Key) {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("key"), s.Key, msg))
		}
	}

	return allErrs
}

func validateSecretKeySelector(s *core.SecretKeySelector, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}

	nameFn := ValidateNameFunc(ValidateSecretName)
	for _, msg := range nameFn(s.Name, false) {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("name"), s.Name, msg))
	}
	if len(s.Key) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("key"), ""))
	} else {
		for _, msg := range validation.IsConfigMapKey(s.Key) {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("key"), s.Key, msg))
		}
	}

	return allErrs
}

func GetVolumeMountMap(mounts []core.VolumeMount) map[string]string {
	volmounts := make(map[string]string)

	for _, mnt := range mounts {
		volmounts[mnt.Name] = mnt.MountPath
	}

	return volmounts
}

func GetVolumeDeviceMap(devices []core.VolumeDevice) map[string]string {
	volDevices := make(map[string]string)

	for _, dev := range devices {
		volDevices[dev.Name] = dev.DevicePath
	}

	return volDevices
}

func ValidateVolumeMounts(mounts []core.VolumeMount, voldevices map[string]string, volumes map[string]core.VolumeSource, container *core.Container, fldPath *field.Path, opts PodValidationOptions) field.ErrorList {
	allErrs := field.ErrorList{}
	mountpoints := sets.New[string]()

	for i, mnt := range mounts {
		idxPath := fldPath.Index(i)
		if len(mnt.Name) == 0 {
			allErrs = append(allErrs, field.Required(idxPath.Child("name"), ""))
		}
		if !IsMatchedVolume(mnt.Name, volumes) {
			allErrs = append(allErrs, field.NotFound(idxPath.Child("name"), mnt.Name))
		}
		if len(mnt.MountPath) == 0 {
			allErrs = append(allErrs, field.Required(idxPath.Child("mountPath"), ""))
		}
		if mountpoints.Has(mnt.MountPath) {
			allErrs = append(allErrs, field.Invalid(idxPath.Child("mountPath"), mnt.MountPath, "must be unique"))
		}
		mountpoints.Insert(mnt.MountPath)

		// check for overlap with VolumeDevice
		if mountNameAlreadyExists(mnt.Name, voldevices) {
			allErrs = append(allErrs, field.Invalid(idxPath.Child("name"), mnt.Name, "must not already exist in volumeDevices"))
		}
		if mountPathAlreadyExists(mnt.MountPath, voldevices) {
			allErrs = append(allErrs, field.Invalid(idxPath.Child("mountPath"), mnt.MountPath, "must not already exist as a path in volumeDevices"))
		}

		// Disallow subPath/subPathExpr for image volumes
		if v, ok := volumes[mnt.Name]; ok && v.Image != nil {
			if len(mnt.SubPath) != 0 {
				allErrs = append(allErrs, field.Invalid(idxPath.Child("subPath"), mnt.SubPath, "not allowed in image volume sources"))
			}
			if len(mnt.SubPathExpr) != 0 {
				allErrs = append(allErrs, field.Invalid(idxPath.Child("subPathExpr"), mnt.SubPathExpr, "not allowed in image volume sources"))
			}
		}

		if len(mnt.SubPath) > 0 {
			allErrs = append(allErrs, validateLocalDescendingPath(mnt.SubPath, fldPath.Child("subPath"))...)
		}

		if len(mnt.SubPathExpr) > 0 {
			if len(mnt.SubPath) > 0 {
				allErrs = append(allErrs, field.Invalid(idxPath.Child("subPathExpr"), mnt.SubPathExpr, "subPathExpr and subPath are mutually exclusive"))
			}

			allErrs = append(allErrs, validateLocalDescendingPath(mnt.SubPathExpr, fldPath.Child("subPathExpr"))...)
		}

		if mnt.MountPropagation != nil {
			allErrs = append(allErrs, validateMountPropagation(mnt.MountPropagation, container, fldPath.Child("mountPropagation"))...)
		}
		allErrs = append(allErrs, validateMountRecursiveReadOnly(mnt, fldPath.Child("recursiveReadOnly"))...)
	}
	return allErrs
}

func ValidateVolumeDevices(devices []core.VolumeDevice, volmounts map[string]string, volumes map[string]core.VolumeSource, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	devicepath := sets.New[string]()
	devicename := sets.New[string]()

	for i, dev := range devices {
		idxPath := fldPath.Index(i)
		devName := dev.Name
		devPath := dev.DevicePath
		didMatch, isPVC := isMatchedDevice(devName, volumes)
		if len(devName) == 0 {
			allErrs = append(allErrs, field.Required(idxPath.Child("name"), ""))
		}
		if devicename.Has(devName) {
			allErrs = append(allErrs, field.Invalid(idxPath.Child("name"), devName, "must be unique"))
		}
		// Must be based on PersistentVolumeClaim (PVC reference or generic ephemeral inline volume)
		if didMatch && !isPVC {
			allErrs = append(allErrs, field.Invalid(idxPath.Child("name"), devName, "can only use volume source type of PersistentVolumeClaim or Ephemeral for block mode"))
		}
		if !didMatch {
			allErrs = append(allErrs, field.NotFound(idxPath.Child("name"), devName))
		}
		if len(devPath) == 0 {
			allErrs = append(allErrs, field.Required(idxPath.Child("devicePath"), ""))
		}
		if devicepath.Has(devPath) {
			allErrs = append(allErrs, field.Invalid(idxPath.Child("devicePath"), devPath, "must be unique"))
		}
		if len(devPath) > 0 && len(validatePathNoBacksteps(devPath, fldPath.Child("devicePath"))) > 0 {
			allErrs = append(allErrs, field.Invalid(idxPath.Child("devicePath"), devPath, "can not contain backsteps ('..')"))
		} else {
			devicepath.Insert(devPath)
		}
		// check for overlap with VolumeMount
		if deviceNameAlreadyExists(devName, volmounts) {
			allErrs = append(allErrs, field.Invalid(idxPath.Child("name"), devName, "must not already exist in volumeMounts"))
		}
		if devicePathAlreadyExists(devPath, volmounts) {
			allErrs = append(allErrs, field.Invalid(idxPath.Child("devicePath"), devPath, "must not already exist as a path in volumeMounts"))
		}
		if len(devName) > 0 {
			devicename.Insert(devName)
		}
	}
	return allErrs
}

func validatePodResourceClaims(podMeta *metav1.ObjectMeta, claims []core.PodResourceClaim, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	podClaimNames := sets.New[string]()
	for i, claim := range claims {
		allErrs = append(allErrs, validatePodResourceClaim(podMeta, claim, &podClaimNames, fldPath.Index(i))...)
	}
	return allErrs
}

// gatherPodResourceClaimNames returns a set of all non-empty
// PodResourceClaim.Name values. Validation that those names are valid is
// handled by validatePodResourceClaims.
func gatherPodResourceClaimNames(claims []core.PodResourceClaim) sets.Set[string] {
	podClaimNames := sets.Set[string]{}
	for _, claim := range claims {
		if claim.Name != "" {
			podClaimNames.Insert(claim.Name)
		}
	}
	return podClaimNames
}

func validatePodResourceClaim(podMeta *metav1.ObjectMeta, claim core.PodResourceClaim, podClaimNames *sets.Set[string], fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	if claim.Name == "" {
		allErrs = append(allErrs, field.Required(fldPath.Child("name"), ""))
	} else if podClaimNames.Has(claim.Name) {
		allErrs = append(allErrs, field.Duplicate(fldPath.Child("name"), claim.Name))
	} else {
		allErrs = append(allErrs, ValidateDNS1123Label(claim.Name, fldPath.Child("name"))...)
		podClaimNames.Insert(claim.Name)
	}
	if claim.ResourceClaimName != nil && claim.ResourceClaimTemplateName != nil {
		allErrs = append(allErrs, field.Invalid(fldPath, claim, "at most one of `resourceClaimName` or `resourceClaimTemplateName` may be specified"))
	}
	if claim.ResourceClaimName == nil && claim.ResourceClaimTemplateName == nil {
		allErrs = append(allErrs, field.Invalid(fldPath, claim, "must specify one of: `resourceClaimName`, `resourceClaimTemplateName`"))
	}
	if claim.ResourceClaimName != nil {
		for _, detail := range ValidateResourceClaimName(*claim.ResourceClaimName, false) {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("resourceClaimName"), *claim.ResourceClaimName, detail))
		}
	}
	if claim.ResourceClaimTemplateName != nil {
		for _, detail := range ValidateResourceClaimTemplateName(*claim.ResourceClaimTemplateName, false) {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("resourceClaimTemplateName"), *claim.ResourceClaimTemplateName, detail))
		}
	}
	return allErrs
}

func validateLivenessProbe(probe *core.Probe, gracePeriod *int64, fldPath *field.Path, opts PodValidationOptions) field.ErrorList {
	allErrs := field.ErrorList{}

	if probe == nil {
		return allErrs
	}
	allErrs = append(allErrs, validateProbe(probe, gracePeriod, fldPath, opts)...)
	if probe.SuccessThreshold != 1 {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("successThreshold"), probe.SuccessThreshold, "must be 1"))
	}
	return allErrs
}

func validateReadinessProbe(probe *core.Probe, gracePeriod *int64, fldPath *field.Path, opts PodValidationOptions) field.ErrorList {
	allErrs := field.ErrorList{}

	if probe == nil {
		return allErrs
	}
	allErrs = append(allErrs, validateProbe(probe, gracePeriod, fldPath, opts)...)
	if probe.TerminationGracePeriodSeconds != nil {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("terminationGracePeriodSeconds"), probe.TerminationGracePeriodSeconds, "must not be set for readinessProbes"))
	}
	return allErrs
}

func validateStartupProbe(probe *core.Probe, gracePeriod *int64, fldPath *field.Path, opts PodValidationOptions) field.ErrorList {
	allErrs := field.ErrorList{}

	if probe == nil {
		return allErrs
	}
	allErrs = append(allErrs, validateProbe(probe, gracePeriod, fldPath, opts)...)
	if probe.SuccessThreshold != 1 {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("successThreshold"), probe.SuccessThreshold, "must be 1"))
	}
	return allErrs
}

func validateProbe(probe *core.Probe, gracePeriod *int64, fldPath *field.Path, opts PodValidationOptions) field.ErrorList {
	allErrs := field.ErrorList{}

	if probe == nil {
		return allErrs
	}
	allErrs = append(allErrs, validateHandler(handlerFromProbe(&probe.ProbeHandler), gracePeriod, fldPath, opts)...)

	allErrs = append(allErrs, ValidateNonnegativeField(int64(probe.InitialDelaySeconds), fldPath.Child("initialDelaySeconds"))...)
	allErrs = append(allErrs, ValidateNonnegativeField(int64(probe.TimeoutSeconds), fldPath.Child("timeoutSeconds"))...)
	allErrs = append(allErrs, ValidateNonnegativeField(int64(probe.PeriodSeconds), fldPath.Child("periodSeconds"))...)
	allErrs = append(allErrs, ValidateNonnegativeField(int64(probe.SuccessThreshold), fldPath.Child("successThreshold"))...)
	allErrs = append(allErrs, ValidateNonnegativeField(int64(probe.FailureThreshold), fldPath.Child("failureThreshold"))...)
	if probe.TerminationGracePeriodSeconds != nil && *probe.TerminationGracePeriodSeconds <= 0 {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("terminationGracePeriodSeconds"), *probe.TerminationGracePeriodSeconds, "must be greater than 0"))
	}
	return allErrs
}

func validateInitContainerRestartPolicy(restartPolicy *core.ContainerRestartPolicy, fldPath *field.Path) field.ErrorList {
	var allErrors field.ErrorList

	if restartPolicy == nil {
		return allErrors
	}
	switch *restartPolicy {
	case core.ContainerRestartPolicyAlways:
		break
	default:
		validValues := []core.ContainerRestartPolicy{core.ContainerRestartPolicyAlways}
		allErrors = append(allErrors, field.NotSupported(fldPath, *restartPolicy, validValues))
	}

	return allErrors
}

type commonHandler struct {
	Exec      *core.ExecAction
	HTTPGet   *core.HTTPGetAction
	TCPSocket *core.TCPSocketAction
	GRPC      *core.GRPCAction
	Sleep     *core.SleepAction
}

func handlerFromProbe(ph *core.ProbeHandler) commonHandler {
	return commonHandler{
		Exec:      ph.Exec,
		HTTPGet:   ph.HTTPGet,
		TCPSocket: ph.TCPSocket,
		GRPC:      ph.GRPC,
	}
}

func handlerFromLifecycle(lh *core.LifecycleHandler) commonHandler {
	return commonHandler{
		Exec:      lh.Exec,
		HTTPGet:   lh.HTTPGet,
		TCPSocket: lh.TCPSocket,
		Sleep:     lh.Sleep,
	}
}

func validateSleepAction(sleep *core.SleepAction, gracePeriod *int64, fldPath *field.Path, opts PodValidationOptions) field.ErrorList {
	allErrors := field.ErrorList{}
	// We allow gracePeriod to be nil here because the pod in which this SleepAction
	// is defined might have an invalid grace period defined, and we don't want to
	// flag another error here when the real problem will already be flagged.
	if opts.AllowPodLifecycleSleepActionZeroValue {
		if gracePeriod != nil && (sleep.Seconds < 0 || sleep.Seconds > *gracePeriod) {
			invalidStr := fmt.Sprintf("must be non-negative and less than terminationGracePeriodSeconds (%d)", *gracePeriod)
			allErrors = append(allErrors, field.Invalid(fldPath, sleep.Seconds, invalidStr))
		}
	} else {
		if gracePeriod != nil && (sleep.Seconds <= 0 || sleep.Seconds > *gracePeriod) {
			invalidStr := fmt.Sprintf("must be greater than 0 and less than terminationGracePeriodSeconds (%d). Enable AllowPodLifecycleSleepActionZeroValue feature gate for zero sleep.", *gracePeriod)
			allErrors = append(allErrors, field.Invalid(fldPath, sleep.Seconds, invalidStr))
		}
	}
	return allErrors
}

func validateClientIPAffinityConfig(config *core.SessionAffinityConfig, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if config == nil {
		allErrs = append(allErrs, field.Required(fldPath, fmt.Sprintf("when session affinity type is %s", core.ServiceAffinityClientIP)))
		return allErrs
	}
	if config.ClientIP == nil {
		allErrs = append(allErrs, field.Required(fldPath.Child("clientIP"), fmt.Sprintf("when session affinity type is %s", core.ServiceAffinityClientIP)))
		return allErrs
	}
	if config.ClientIP.TimeoutSeconds == nil {
		allErrs = append(allErrs, field.Required(fldPath.Child("clientIP").Child("timeoutSeconds"), fmt.Sprintf("when session affinity type is %s", core.ServiceAffinityClientIP)))
		return allErrs
	}
	allErrs = append(allErrs, validateAffinityTimeout(config.ClientIP.TimeoutSeconds, fldPath.Child("clientIP").Child("timeoutSeconds"))...)

	return allErrs
}

func validateAffinityTimeout(timeout *int32, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if *timeout <= 0 || *timeout > core.MaxClientIPServiceAffinitySeconds {
		allErrs = append(allErrs, field.Invalid(fldPath, timeout, fmt.Sprintf("must be greater than 0 and less than %d", core.MaxClientIPServiceAffinitySeconds)))
	}
	return allErrs
}

// AccumulateUniqueHostPorts extracts each HostPort of each Container,
// accumulating the results and returning an error if any ports conflict.
func AccumulateUniqueHostPorts(containers []core.Container, accumulator *sets.Set[string], fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}

	for ci, ctr := range containers {
		idxPath := fldPath.Index(ci)
		portsPath := idxPath.Child("ports")
		for pi := range ctr.Ports {
			idxPath := portsPath.Index(pi)
			port := ctr.Ports[pi].HostPort
			if port == 0 {
				continue
			}
			str := fmt.Sprintf("%s/%s/%d", ctr.Ports[pi].Protocol, ctr.Ports[pi].HostIP, port)
			if accumulator.Has(str) {
				allErrs = append(allErrs, field.Duplicate(idxPath.Child("hostPort"), str))
			} else {
				accumulator.Insert(str)
			}
		}
	}
	return allErrs
}

// checkHostPortConflicts checks for colliding Port.HostPort values across
// a slice of containers.
func checkHostPortConflicts(containers []core.Container, fldPath *field.Path) field.ErrorList {
	allPorts := sets.Set[string]{}
	return AccumulateUniqueHostPorts(containers, &allPorts, fldPath)
}

func validateExecAction(exec *core.ExecAction, fldPath *field.Path) field.ErrorList {
	allErrors := field.ErrorList{}
	if len(exec.Command) == 0 {
		allErrors = append(allErrors, field.Required(fldPath.Child("command"), ""))
	}
	return allErrors
}

var supportedHTTPSchemes = sets.New(core.URISchemeHTTP, core.URISchemeHTTPS)

func validateHTTPGetAction(http *core.HTTPGetAction, fldPath *field.Path) field.ErrorList {
	allErrors := field.ErrorList{}
	if len(http.Path) == 0 {
		allErrors = append(allErrors, field.Required(fldPath.Child("path"), ""))
	}
	allErrors = append(allErrors, ValidatePortNumOrName(http.Port, fldPath.Child("port"))...)
	if !supportedHTTPSchemes.Has(http.Scheme) {
		allErrors = append(allErrors, field.NotSupported(fldPath.Child("scheme"), http.Scheme, sets.List(supportedHTTPSchemes)))
	}
	for _, header := range http.HTTPHeaders {
		for _, msg := range validation.IsHTTPHeaderName(header.Name) {
			allErrors = append(allErrors, field.Invalid(fldPath.Child("httpHeaders"), header.Name, msg))
		}
	}
	return allErrors
}

func ValidatePortNumOrName(port intstr.IntOrString, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if port.Type == intstr.Int {
		for _, msg := range validation.IsValidPortNum(port.IntValue()) {
			allErrs = append(allErrs, field.Invalid(fldPath, port.IntValue(), msg))
		}
	} else if port.Type == intstr.String {
		for _, msg := range validation.IsValidPortName(port.StrVal) {
			allErrs = append(allErrs, field.Invalid(fldPath, port.StrVal, msg))
		}
	} else {
		allErrs = append(allErrs, field.InternalError(fldPath, fmt.Errorf("unknown type: %v", port.Type)))
	}
	return allErrs
}

func validateTCPSocketAction(tcp *core.TCPSocketAction, fldPath *field.Path) field.ErrorList {
	return ValidatePortNumOrName(tcp.Port, fldPath.Child("port"))
}
func validateGRPCAction(grpc *core.GRPCAction, fldPath *field.Path) field.ErrorList {
	return ValidatePortNumOrName(intstr.FromInt32(grpc.Port), fldPath.Child("port"))
}
func validateHandler(handler commonHandler, gracePeriod *int64, fldPath *field.Path, opts PodValidationOptions) field.ErrorList {
	numHandlers := 0
	allErrors := field.ErrorList{}
	if handler.Exec != nil {
		if numHandlers > 0 {
			allErrors = append(allErrors, field.Forbidden(fldPath.Child("exec"), "may not specify more than 1 handler type"))
		} else {
			numHandlers++
			allErrors = append(allErrors, validateExecAction(handler.Exec, fldPath.Child("exec"))...)
		}
	}
	if handler.HTTPGet != nil {
		if numHandlers > 0 {
			allErrors = append(allErrors, field.Forbidden(fldPath.Child("httpGet"), "may not specify more than 1 handler type"))
		} else {
			numHandlers++
			allErrors = append(allErrors, validateHTTPGetAction(handler.HTTPGet, fldPath.Child("httpGet"))...)
		}
	}
	if handler.TCPSocket != nil {
		if numHandlers > 0 {
			allErrors = append(allErrors, field.Forbidden(fldPath.Child("tcpSocket"), "may not specify more than 1 handler type"))
		} else {
			numHandlers++
			allErrors = append(allErrors, validateTCPSocketAction(handler.TCPSocket, fldPath.Child("tcpSocket"))...)
		}
	}
	if handler.GRPC != nil {
		if numHandlers > 0 {
			allErrors = append(allErrors, field.Forbidden(fldPath.Child("grpc"), "may not specify more than 1 handler type"))
		} else {
			numHandlers++
			allErrors = append(allErrors, validateGRPCAction(handler.GRPC, fldPath.Child("grpc"))...)
		}
	}
	if handler.Sleep != nil {
		if numHandlers > 0 {
			allErrors = append(allErrors, field.Forbidden(fldPath.Child("sleep"), "may not specify more than 1 handler type"))
		} else {
			numHandlers++
			allErrors = append(allErrors, validateSleepAction(handler.Sleep, gracePeriod, fldPath.Child("sleep"), opts)...)
		}
	}
	if numHandlers == 0 {
		allErrors = append(allErrors, field.Required(fldPath, "must specify a handler type"))
	}
	return allErrors
}

func validateLifecycle(lifecycle *core.Lifecycle, gracePeriod *int64, fldPath *field.Path, opts PodValidationOptions) field.ErrorList {
	allErrs := field.ErrorList{}
	if lifecycle.PostStart != nil {
		allErrs = append(allErrs, validateHandler(handlerFromLifecycle(lifecycle.PostStart), gracePeriod, fldPath.Child("postStart"), opts)...)
	}
	if lifecycle.PreStop != nil {
		allErrs = append(allErrs, validateHandler(handlerFromLifecycle(lifecycle.PreStop), gracePeriod, fldPath.Child("preStop"), opts)...)
	}
	return allErrs
}

var supportedPullPolicies = sets.New(
	core.PullAlways,
	core.PullIfNotPresent,
	core.PullNever)

func validatePullPolicy(policy core.PullPolicy, fldPath *field.Path) field.ErrorList {
	allErrors := field.ErrorList{}

	switch policy {
	case core.PullAlways, core.PullIfNotPresent, core.PullNever:
		break
	case "":
		allErrors = append(allErrors, field.Required(fldPath, ""))
	default:
		allErrors = append(allErrors, field.NotSupported(fldPath, policy, sets.List(supportedPullPolicies)))
	}

	return allErrors
}

var supportedResizeResources = sets.New(core.ResourceCPU, core.ResourceMemory)
var supportedResizePolicies = sets.New(core.NotRequired, core.RestartContainer)

func validateResizePolicy(policyList []core.ContainerResizePolicy, fldPath *field.Path, podRestartPolicy *core.RestartPolicy) field.ErrorList {
	allErrors := field.ErrorList{}

	// validate that resource name is not repeated, supported resource names and policy values are specified
	resources := make(map[core.ResourceName]bool)
	for i, p := range policyList {
		if _, found := resources[p.ResourceName]; found {
			allErrors = append(allErrors, field.Duplicate(fldPath.Index(i), p.ResourceName))
		}
		resources[p.ResourceName] = true
		switch p.ResourceName {
		case core.ResourceCPU, core.ResourceMemory:
		case "":
			allErrors = append(allErrors, field.Required(fldPath, ""))
		default:
			allErrors = append(allErrors, field.NotSupported(fldPath, p.ResourceName, sets.List(supportedResizeResources)))
		}
		switch p.RestartPolicy {
		case core.NotRequired, core.RestartContainer:
		case "":
			allErrors = append(allErrors, field.Required(fldPath, ""))
		default:
			allErrors = append(allErrors, field.NotSupported(fldPath, p.RestartPolicy, sets.List(supportedResizePolicies)))
		}

		if *podRestartPolicy == core.RestartPolicyNever && p.RestartPolicy != core.NotRequired {
			allErrors = append(allErrors, field.Invalid(fldPath, p.RestartPolicy, "must be 'NotRequired' when `restartPolicy` is 'Never'"))
		}
	}
	return allErrors
}

// validateEphemeralContainers is called by pod spec and template validation to validate the list of ephemeral containers.
// Note that this is called for pod template even though ephemeral containers aren't allowed in pod templates.
func validateEphemeralContainers(ephemeralContainers []core.EphemeralContainer, containers, initContainers []core.Container, volumes map[string]core.VolumeSource, podClaimNames sets.Set[string], fldPath *field.Path, opts PodValidationOptions, podRestartPolicy *core.RestartPolicy, hostUsers bool) field.ErrorList {
	var allErrs field.ErrorList

	if len(ephemeralContainers) == 0 {
		return allErrs
	}

	otherNames, allNames := sets.Set[string]{}, sets.Set[string]{}
	for _, c := range containers {
		otherNames.Insert(c.Name)
		allNames.Insert(c.Name)
	}
	for _, c := range initContainers {
		otherNames.Insert(c.Name)
		allNames.Insert(c.Name)
	}

	for i, ec := range ephemeralContainers {
		idxPath := fldPath.Index(i)

		c := (*core.Container)(&ec.EphemeralContainerCommon)
		allErrs = append(allErrs, validateContainerCommon(c, volumes, podClaimNames, idxPath, opts, podRestartPolicy, hostUsers)...)
		// Ephemeral containers don't need looser constraints for pod templates, so it's convenient to apply both validations
		// here where we've already converted EphemeralContainerCommon to Container.
		allErrs = append(allErrs, validateContainerOnlyForPod(c, idxPath)...)

		// Ephemeral containers must have a name unique across all container types.
		if allNames.Has(ec.Name) {
			allErrs = append(allErrs, field.Duplicate(idxPath.Child("name"), ec.Name))
		} else {
			allNames.Insert(ec.Name)
		}

		// The target container name must exist and be non-ephemeral.
		if ec.TargetContainerName != "" && !otherNames.Has(ec.TargetContainerName) {
			allErrs = append(allErrs, field.NotFound(idxPath.Child("targetContainerName"), ec.TargetContainerName))
		}

		// Ephemeral containers should not be relied upon for fundamental pod services, so fields such as
		// Lifecycle, probes, resources and ports should be disallowed. This is implemented as a list
		// of allowed fields so that new fields will be given consideration prior to inclusion in ephemeral containers.
		allErrs = append(allErrs, validateFieldAllowList(ec.EphemeralContainerCommon, allowedEphemeralContainerFields, "cannot be set for an Ephemeral Container", idxPath)...)

		// VolumeMount subpaths have the potential to leak resources since they're implemented with bind mounts
		// that aren't cleaned up until the pod exits. Since they also imply that the container is being used
		// as part of the workload, they're disallowed entirely.
		for i, vm := range ec.VolumeMounts {
			if vm.SubPath != "" {
				allErrs = append(allErrs, field.Forbidden(idxPath.Child("volumeMounts").Index(i).Child("subPath"), "cannot be set for an Ephemeral Container"))
			}
			if vm.SubPathExpr != "" {
				allErrs = append(allErrs, field.Forbidden(idxPath.Child("volumeMounts").Index(i).Child("subPathExpr"), "cannot be set for an Ephemeral Container"))
			}
		}
	}

	return allErrs
}

// ValidateFieldAcceptList checks that only allowed fields are set.
// The value must be a struct (not a pointer to a struct!).
func validateFieldAllowList(value interface{}, allowedFields map[string]bool, errorText string, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList

	reflectType, reflectValue := reflect.TypeOf(value), reflect.ValueOf(value)
	for i := 0; i < reflectType.NumField(); i++ {
		f := reflectType.Field(i)
		if allowedFields[f.Name] {
			continue
		}

		// Compare the value of this field to its zero value to determine if it has been set
		if !reflect.DeepEqual(reflectValue.Field(i).Interface(), reflect.Zero(f.Type).Interface()) {
			r, n := utf8.DecodeRuneInString(f.Name)
			lcName := string(unicode.ToLower(r)) + f.Name[n:]
			allErrs = append(allErrs, field.Forbidden(fldPath.Child(lcName), errorText))
		}
	}

	return allErrs
}

// validateInitContainers is called by pod spec and template validation to validate the list of init containers
func validateInitContainers(containers []core.Container, regularContainers []core.Container, volumes map[string]core.VolumeSource, podClaimNames sets.Set[string], gracePeriod *int64, fldPath *field.Path, opts PodValidationOptions, podRestartPolicy *core.RestartPolicy, hostUsers bool) field.ErrorList {
	var allErrs field.ErrorList

	allNames := sets.Set[string]{}
	for _, ctr := range regularContainers {
		allNames.Insert(ctr.Name)
	}
	for i, ctr := range containers {
		idxPath := fldPath.Index(i)

		// Apply the validation common to all container types
		allErrs = append(allErrs, validateContainerCommon(&ctr, volumes, podClaimNames, idxPath, opts, podRestartPolicy, hostUsers)...)

		restartAlways := false
		// Apply the validation specific to init containers
		if ctr.RestartPolicy != nil {
			allErrs = append(allErrs, validateInitContainerRestartPolicy(ctr.RestartPolicy, idxPath.Child("restartPolicy"))...)
			restartAlways = *ctr.RestartPolicy == core.ContainerRestartPolicyAlways
		}

		// Names must be unique within regular and init containers. Collisions with ephemeral containers
		// will be detected by validateEphemeralContainers().
		if allNames.Has(ctr.Name) {
			allErrs = append(allErrs, field.Duplicate(idxPath.Child("name"), ctr.Name))
		} else if len(ctr.Name) > 0 {
			allNames.Insert(ctr.Name)
		}

		// Check for port conflicts in init containers individually since init containers run one-by-one.
		allErrs = append(allErrs, checkHostPortConflicts([]core.Container{ctr}, fldPath)...)

		switch {
		case restartAlways:
			if ctr.Lifecycle != nil {
				allErrs = append(allErrs, validateLifecycle(ctr.Lifecycle, gracePeriod, idxPath.Child("lifecycle"), opts)...)
			}
			allErrs = append(allErrs, validateLivenessProbe(ctr.LivenessProbe, gracePeriod, idxPath.Child("livenessProbe"), opts)...)
			allErrs = append(allErrs, validateReadinessProbe(ctr.ReadinessProbe, gracePeriod, idxPath.Child("readinessProbe"), opts)...)
			allErrs = append(allErrs, validateStartupProbe(ctr.StartupProbe, gracePeriod, idxPath.Child("startupProbe"), opts)...)

		default:
			// These fields are disallowed for init containers.
			if ctr.Lifecycle != nil {
				allErrs = append(allErrs, field.Forbidden(idxPath.Child("lifecycle"), "may not be set for init containers without restartPolicy=Always"))
			}
			if ctr.LivenessProbe != nil {
				allErrs = append(allErrs, field.Forbidden(idxPath.Child("livenessProbe"), "may not be set for init containers without restartPolicy=Always"))
			}
			if ctr.ReadinessProbe != nil {
				allErrs = append(allErrs, field.Forbidden(idxPath.Child("readinessProbe"), "may not be set for init containers without restartPolicy=Always"))
			}
			if ctr.StartupProbe != nil {
				allErrs = append(allErrs, field.Forbidden(idxPath.Child("startupProbe"), "may not be set for init containers without restartPolicy=Always"))
			}
		}

		if !opts.AllowSidecarResizePolicy && len(ctr.ResizePolicy) > 0 {
			allErrs = append(allErrs, field.Invalid(idxPath.Child("resizePolicy"), ctr.ResizePolicy, "must not be set for init containers"))
		}
	}

	return allErrs
}

// validateContainerCommon applies validation common to all container types. It's called by regular, init, and ephemeral
// container list validation to require a properly formatted name, image, etc.
func validateContainerCommon(ctr *core.Container, volumes map[string]core.VolumeSource, podClaimNames sets.Set[string], path *field.Path, opts PodValidationOptions, podRestartPolicy *core.RestartPolicy, hostUsers bool) field.ErrorList {
	var allErrs field.ErrorList

	namePath := path.Child("name")
	if len(ctr.Name) == 0 {
		allErrs = append(allErrs, field.Required(namePath, ""))
	} else {
		allErrs = append(allErrs, ValidateDNS1123Label(ctr.Name, namePath)...)
	}

	// TODO: do not validate leading and trailing whitespace to preserve backward compatibility.
	// for example: https://github.com/openshift/origin/issues/14659 image = " " is special token in pod template
	// others may have done similar
	if len(ctr.Image) == 0 {
		allErrs = append(allErrs, field.Required(path.Child("image"), ""))
	}

	switch ctr.TerminationMessagePolicy {
	case core.TerminationMessageReadFile, core.TerminationMessageFallbackToLogsOnError:
	case "":
		allErrs = append(allErrs, field.Required(path.Child("terminationMessagePolicy"), ""))
	default:
		supported := []core.TerminationMessagePolicy{
			core.TerminationMessageReadFile,
			core.TerminationMessageFallbackToLogsOnError,
		}
		allErrs = append(allErrs, field.NotSupported(path.Child("terminationMessagePolicy"), ctr.TerminationMessagePolicy, supported))
	}

	volMounts := GetVolumeMountMap(ctr.VolumeMounts)
	volDevices := GetVolumeDeviceMap(ctr.VolumeDevices)
	allErrs = append(allErrs, validateContainerPorts(ctr.Ports, path.Child("ports"))...)
	allErrs = append(allErrs, ValidateEnv(ctr.Env, path.Child("env"), opts)...)
	allErrs = append(allErrs, ValidateEnvFrom(ctr.EnvFrom, path.Child("envFrom"), opts)...)
	allErrs = append(allErrs, ValidateVolumeMounts(ctr.VolumeMounts, volDevices, volumes, ctr, path.Child("volumeMounts"), opts)...)
	allErrs = append(allErrs, ValidateVolumeDevices(ctr.VolumeDevices, volMounts, volumes, path.Child("volumeDevices"))...)
	allErrs = append(allErrs, validatePullPolicy(ctr.ImagePullPolicy, path.Child("imagePullPolicy"))...)
	allErrs = append(allErrs, ValidateContainerResourceRequirements(&ctr.Resources, podClaimNames, path.Child("resources"), opts)...)
	allErrs = append(allErrs, validateResizePolicy(ctr.ResizePolicy, path.Child("resizePolicy"), podRestartPolicy)...)
	allErrs = append(allErrs, ValidateSecurityContext(ctr.SecurityContext, path.Child("securityContext"), hostUsers)...)
	return allErrs
}

func validateHostUsers(spec *core.PodSpec, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}

	// Only make the following checks if hostUsers is false (otherwise, the container uses the
	// same userns as the host, and so there isn't anything to check).
	if spec.SecurityContext == nil || spec.SecurityContext.HostUsers == nil || *spec.SecurityContext.HostUsers {
		return allErrs
	}

	// We decided to restrict the usage of userns with other host namespaces:
	// 	https://github.com/kubernetes/kubernetes/pull/111090#discussion_r935994282
	// The tl;dr is: you can easily run into permission issues that seem unexpected, we don't
	// know of any good use case and we can always enable them later.

	// Note we already validated above spec.SecurityContext is not nil.
	if spec.SecurityContext.HostNetwork {
		allErrs = append(allErrs, field.Forbidden(fldPath.Child("hostNetwork"), "when `pod.Spec.HostUsers` is false"))
	}
	if spec.SecurityContext.HostPID {
		allErrs = append(allErrs, field.Forbidden(fldPath.Child("HostPID"), "when `pod.Spec.HostUsers` is false"))
	}
	if spec.SecurityContext.HostIPC {
		allErrs = append(allErrs, field.Forbidden(fldPath.Child("HostIPC"), "when `pod.Spec.HostUsers` is false"))
	}

	return allErrs
}

// validateContainers is called by pod spec and template validation to validate the list of regular containers.
func validateContainers(containers []core.Container, volumes map[string]core.VolumeSource, podClaimNames sets.Set[string], gracePeriod *int64, fldPath *field.Path, opts PodValidationOptions, podRestartPolicy *core.RestartPolicy, hostUsers bool) field.ErrorList {
	allErrs := field.ErrorList{}

	if len(containers) == 0 {
		return append(allErrs, field.Required(fldPath, ""))
	}

	allNames := sets.Set[string]{}
	for i, ctr := range containers {
		path := fldPath.Index(i)

		// Apply validation common to all containers
		allErrs = append(allErrs, validateContainerCommon(&ctr, volumes, podClaimNames, path, opts, podRestartPolicy, hostUsers)...)

		// Container names must be unique within the list of regular containers.
		// Collisions with init or ephemeral container names will be detected by the init or ephemeral
		// container validation to prevent duplicate error messages.
		if allNames.Has(ctr.Name) {
			allErrs = append(allErrs, field.Duplicate(path.Child("name"), ctr.Name))
		} else {
			allNames.Insert(ctr.Name)
		}

		// These fields are allowed for regular containers and restartable init
		// containers.
		// Regular init container and ephemeral container validation will return
		// field.Forbidden() for these paths.
		if ctr.Lifecycle != nil {
			allErrs = append(allErrs, validateLifecycle(ctr.Lifecycle, gracePeriod, path.Child("lifecycle"), opts)...)
		}
		allErrs = append(allErrs, validateLivenessProbe(ctr.LivenessProbe, gracePeriod, path.Child("livenessProbe"), opts)...)
		allErrs = append(allErrs, validateReadinessProbe(ctr.ReadinessProbe, gracePeriod, path.Child("readinessProbe"), opts)...)
		allErrs = append(allErrs, validateStartupProbe(ctr.StartupProbe, gracePeriod, path.Child("startupProbe"), opts)...)

		// These fields are disallowed for regular containers
		if ctr.RestartPolicy != nil {
			allErrs = append(allErrs, field.Forbidden(path.Child("restartPolicy"), "may not be set for non-init containers"))
		}
	}

	// Port conflicts are checked across all containers
	allErrs = append(allErrs, checkHostPortConflicts(containers, fldPath)...)

	return allErrs
}

func validateRestartPolicy(restartPolicy *core.RestartPolicy, fldPath *field.Path) field.ErrorList {
	allErrors := field.ErrorList{}
	switch *restartPolicy {
	case core.RestartPolicyAlways, core.RestartPolicyOnFailure, core.RestartPolicyNever:
		break
	case "":
		allErrors = append(allErrors, field.Required(fldPath, ""))
	default:
		validValues := []core.RestartPolicy{core.RestartPolicyAlways, core.RestartPolicyOnFailure, core.RestartPolicyNever}
		allErrors = append(allErrors, field.NotSupported(fldPath, *restartPolicy, validValues))
	}

	return allErrors
}

func ValidatePreemptionPolicy(preemptionPolicy *core.PreemptionPolicy, fldPath *field.Path) field.ErrorList {
	allErrors := field.ErrorList{}
	switch *preemptionPolicy {
	case core.PreemptLowerPriority, core.PreemptNever:
	case "":
		allErrors = append(allErrors, field.Required(fldPath, ""))
	default:
		validValues := []core.PreemptionPolicy{core.PreemptLowerPriority, core.PreemptNever}
		allErrors = append(allErrors, field.NotSupported(fldPath, preemptionPolicy, validValues))
	}
	return allErrors
}

func validateDNSPolicy(dnsPolicy *core.DNSPolicy, fldPath *field.Path) field.ErrorList {
	allErrors := field.ErrorList{}
	switch *dnsPolicy {
	case core.DNSClusterFirstWithHostNet, core.DNSClusterFirst, core.DNSDefault, core.DNSNone:
	case "":
		allErrors = append(allErrors, field.Required(fldPath, ""))
	default:
		validValues := []core.DNSPolicy{core.DNSClusterFirstWithHostNet, core.DNSClusterFirst, core.DNSDefault, core.DNSNone}
		allErrors = append(allErrors, field.NotSupported(fldPath, dnsPolicy, validValues))
	}
	return allErrors
}

var validFSGroupChangePolicies = sets.New(core.FSGroupChangeOnRootMismatch, core.FSGroupChangeAlways)

func validateFSGroupChangePolicy(fsGroupPolicy *core.PodFSGroupChangePolicy, fldPath *field.Path) field.ErrorList {
	allErrors := field.ErrorList{}
	if !validFSGroupChangePolicies.Has(*fsGroupPolicy) {
		allErrors = append(allErrors, field.NotSupported(fldPath, fsGroupPolicy, sets.List(validFSGroupChangePolicies)))
	}
	return allErrors
}

const (
	// Limits on various DNS parameters. These are derived from
	// restrictions in Linux libc name resolution handling.
	// Max number of DNS name servers.
	MaxDNSNameservers = 3
	// Max number of domains in the search path list.
	MaxDNSSearchPaths = 32
	// Max number of characters in the search path.
	MaxDNSSearchListChars = 2048
)

func validateReadinessGates(readinessGates []core.PodReadinessGate, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	for i, value := range readinessGates {
		allErrs = append(allErrs, ValidateQualifiedName(string(value.ConditionType), fldPath.Index(i).Child("conditionType"))...)
	}
	return allErrs
}

func validateSchedulingGates(schedulingGates []core.PodSchedulingGate, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	// There should be no duplicates in the list of scheduling gates.
	seen := sets.Set[string]{}
	for i, schedulingGate := range schedulingGates {
		allErrs = append(allErrs, ValidateQualifiedName(schedulingGate.Name, fldPath.Index(i))...)
		if seen.Has(schedulingGate.Name) {
			allErrs = append(allErrs, field.Duplicate(fldPath.Index(i), schedulingGate.Name))
		}
		seen.Insert(schedulingGate.Name)
	}
	return allErrs
}

func validatePodDNSConfig(dnsConfig *core.PodDNSConfig, dnsPolicy *core.DNSPolicy, fldPath *field.Path, opts PodValidationOptions) field.ErrorList {
	allErrs := field.ErrorList{}

	// Validate DNSNone case. Must provide at least one DNS name server.
	if dnsPolicy != nil && *dnsPolicy == core.DNSNone {
		if dnsConfig == nil {
			return append(allErrs, field.Required(fldPath, fmt.Sprintf("must provide `dnsConfig` when `dnsPolicy` is %s", core.DNSNone)))
		}
		if len(dnsConfig.Nameservers) == 0 {
			return append(allErrs, field.Required(fldPath.Child("nameservers"), fmt.Sprintf("must provide at least one DNS nameserver when `dnsPolicy` is %s", core.DNSNone)))
		}
	}

	if dnsConfig != nil {
		// Validate nameservers.
		if len(dnsConfig.Nameservers) > MaxDNSNameservers {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("nameservers"), dnsConfig.Nameservers, fmt.Sprintf("must not have more than %v nameservers", MaxDNSNameservers)))
		}
		for i, ns := range dnsConfig.Nameservers {
			allErrs = append(allErrs, validation.IsValidIP(fldPath.Child("nameservers").Index(i), ns)...)
		}
		// Validate searches.
		if len(dnsConfig.Searches) > MaxDNSSearchPaths {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("searches"), dnsConfig.Searches, fmt.Sprintf("must not have more than %v search paths", MaxDNSSearchPaths)))
		}
		// Include the space between search paths.
		if len(strings.Join(dnsConfig.Searches, " ")) > MaxDNSSearchListChars {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("searches"), dnsConfig.Searches, fmt.Sprintf("must not have more than %v characters (including spaces) in the search list", MaxDNSSearchListChars)))
		}

		for i, search := range dnsConfig.Searches {
			if opts.AllowRelaxedDNSSearchValidation {
				if search != "." {
					search = strings.TrimSuffix(search, ".")
					allErrs = append(allErrs, ValidateDNS1123SubdomainWithUnderScore(search, fldPath.Child("searches").Index(i))...)
				}
			} else {
				search = strings.TrimSuffix(search, ".")
				allErrs = append(allErrs, ValidateDNS1123Subdomain(search, fldPath.Child("searches").Index(i))...)
			}

		}
		// Validate options.
		for i, option := range dnsConfig.Options {
			if len(option.Name) == 0 {
				allErrs = append(allErrs, field.Required(fldPath.Child("options").Index(i), "must not be empty"))
			}
		}
	}
	return allErrs
}

// validatePodHostNetworkDeps checks fields which depend on whether HostNetwork is
// true or not.  It should be called on all PodSpecs, but opts can change what
// is enforce.  E.g. opts.ResourceIsPod should only be set when called in the
// context of a Pod, and not on PodSpecs which are embedded in other resources
// (e.g. Deployments).
func validatePodHostNetworkDeps(spec *core.PodSpec, fldPath *field.Path, opts PodValidationOptions) field.ErrorList {
	// For <reasons> we keep `.HostNetwork` in .SecurityContext on the internal
	// version of Pod.
	hostNetwork := false
	if spec.SecurityContext != nil {
		hostNetwork = spec.SecurityContext.HostNetwork
	}

	allErrors := field.ErrorList{}

	if hostNetwork {
		fldPath := fldPath.Child("containers")
		for i, container := range spec.Containers {
			portsPath := fldPath.Index(i).Child("ports")
			for i, port := range container.Ports {
				idxPath := portsPath.Index(i)
				// At this point, we know that HostNetwork is true. If this
				// PodSpec is in a Pod (opts.ResourceIsPod), then HostPort must
				// be the same value as ContainerPort. If this PodSpec is in
				// some other resource (e.g. Deployment) we allow 0 (i.e.
				// unspecified) because it will be defaulted when the Pod is
				// ultimately created, but we do not allow any other values.
				if hp, cp := port.HostPort, port.ContainerPort; (opts.ResourceIsPod || hp != 0) && hp != cp {
					allErrors = append(allErrors, field.Invalid(idxPath.Child("hostPort"), port.HostPort, "must match `containerPort` when `hostNetwork` is true"))
				}
			}
		}
	}
	return allErrors
}

// validateImagePullSecrets checks to make sure the pull secrets are well
// formed.  Right now, we only expect name to be set (it's the only field).  If
// this ever changes and someone decides to set those fields, we'd like to
// know.
func validateImagePullSecrets(imagePullSecrets []core.LocalObjectReference, fldPath *field.Path) field.ErrorList {
	allErrors := field.ErrorList{}
	for i, currPullSecret := range imagePullSecrets {
		idxPath := fldPath.Index(i)
		strippedRef := core.LocalObjectReference{Name: currPullSecret.Name}
		if !reflect.DeepEqual(strippedRef, currPullSecret) {
			allErrors = append(allErrors, field.Invalid(idxPath, currPullSecret, "only name may be set"))
		}
	}
	return allErrors
}

// validateAffinity checks if given affinities are valid
func validateAffinity(affinity *core.Affinity, opts PodValidationOptions, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}

	if affinity != nil {
		if affinity.NodeAffinity != nil {
			allErrs = append(allErrs, validateNodeAffinity(affinity.NodeAffinity, opts, fldPath.Child("nodeAffinity"))...)
		}
		if affinity.PodAffinity != nil {
			allErrs = append(allErrs, validatePodAffinity(affinity.PodAffinity, opts.AllowInvalidLabelValueInSelector, fldPath.Child("podAffinity"))...)
		}
		if affinity.PodAntiAffinity != nil {
			allErrs = append(allErrs, validatePodAntiAffinity(affinity.PodAntiAffinity, opts.AllowInvalidLabelValueInSelector, fldPath.Child("podAntiAffinity"))...)
		}
	}

	return allErrs
}

func validateTaintEffect(effect *core.TaintEffect, allowEmpty bool, fldPath *field.Path) field.ErrorList {
	if !allowEmpty && len(*effect) == 0 {
		return field.ErrorList{field.Required(fldPath, "")}
	}

	allErrors := field.ErrorList{}
	switch *effect {
	// TODO: Replace next line with subsequent commented-out line when implement TaintEffectNoScheduleNoAdmit.
	case core.TaintEffectNoSchedule, core.TaintEffectPreferNoSchedule, core.TaintEffectNoExecute:
		// case core.TaintEffectNoSchedule, core.TaintEffectPreferNoSchedule, core.TaintEffectNoScheduleNoAdmit, core.TaintEffectNoExecute:
	default:
		validValues := []core.TaintEffect{
			core.TaintEffectNoSchedule,
			core.TaintEffectPreferNoSchedule,
			core.TaintEffectNoExecute,
			// TODO: Uncomment this block when implement TaintEffectNoScheduleNoAdmit.
			// core.TaintEffectNoScheduleNoAdmit,
		}
		allErrors = append(allErrors, field.NotSupported(fldPath, *effect, validValues))
	}
	return allErrors
}

// validateOnlyAddedTolerations validates updated pod tolerations.
func validateOnlyAddedTolerations(newTolerations []core.Toleration, oldTolerations []core.Toleration, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	for _, old := range oldTolerations {
		found := false
		oldTolerationClone := old.DeepCopy()
		for _, newToleration := range newTolerations {
			// assign to our clone before doing a deep equal so we can allow tolerationseconds to change.
			oldTolerationClone.TolerationSeconds = newToleration.TolerationSeconds // +k8s:verify-mutation:reason=clone
			if reflect.DeepEqual(*oldTolerationClone, newToleration) {
				found = true
				break
			}
		}
		if !found {
			allErrs = append(allErrs, field.Forbidden(fldPath, "existing toleration can not be modified except its tolerationSeconds"))
			return allErrs
		}
	}

	allErrs = append(allErrs, ValidateTolerations(newTolerations, fldPath)...)
	return allErrs
}

func validateOnlyDeletedSchedulingGates(newGates, oldGates []core.PodSchedulingGate, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if len(newGates) == 0 {
		return allErrs
	}

	additionalGates := make(map[string]int)
	for i, newGate := range newGates {
		additionalGates[newGate.Name] = i
	}

	for _, oldGate := range oldGates {
		delete(additionalGates, oldGate.Name)
	}

	for gate, i := range additionalGates {
		allErrs = append(allErrs, field.Forbidden(fldPath.Index(i).Child("name"), fmt.Sprintf("only deletion is allowed, but found new scheduling gate '%s'", gate)))
	}

	return allErrs
}

func ValidateHostAliases(hostAliases []core.HostAlias, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	for i, hostAlias := range hostAliases {
		allErrs = append(allErrs, validation.IsValidIP(fldPath.Index(i).Child("ip"), hostAlias.IP)...)
		for j, hostname := range hostAlias.Hostnames {
			allErrs = append(allErrs, ValidateDNS1123Subdomain(hostname, fldPath.Index(i).Child("hostnames").Index(j))...)
		}
	}
	return allErrs
}

// ValidateTolerations tests if given tolerations have valid data.
func ValidateTolerations(tolerations []core.Toleration, fldPath *field.Path) field.ErrorList {
	allErrors := field.ErrorList{}
	for i, toleration := range tolerations {
		idxPath := fldPath.Index(i)
		// validate the toleration key
		if len(toleration.Key) > 0 {
			allErrors = append(allErrors, unversionedvalidation.ValidateLabelName(toleration.Key, idxPath.Child("key"))...)
		}

		// empty toleration key with Exists operator and empty value means match all taints
		if len(toleration.Key) == 0 && toleration.Operator != core.TolerationOpExists {
			allErrors = append(allErrors, field.Invalid(idxPath.Child("operator"), toleration.Operator,
				"operator must be Exists when `key` is empty, which means \"match all values and all keys\""))
		}

		if toleration.TolerationSeconds != nil && toleration.Effect != core.TaintEffectNoExecute {
			allErrors = append(allErrors, field.Invalid(idxPath.Child("effect"), toleration.Effect,
				"effect must be 'NoExecute' when `tolerationSeconds` is set"))
		}

		// validate toleration operator and value
		switch toleration.Operator {
		// empty operator means Equal
		case core.TolerationOpEqual, "":
			if errs := validation.IsValidLabelValue(toleration.Value); len(errs) != 0 {
				allErrors = append(allErrors, field.Invalid(idxPath.Child("operator"), toleration.Value, strings.Join(errs, ";")))
			}
		case core.TolerationOpExists:
			if len(toleration.Value) > 0 {
				allErrors = append(allErrors, field.Invalid(idxPath.Child("operator"), toleration.Value, "value must be empty when `operator` is 'Exists'"))
			}
		default:
			validValues := []core.TolerationOperator{core.TolerationOpEqual, core.TolerationOpExists}
			allErrors = append(allErrors, field.NotSupported(idxPath.Child("operator"), toleration.Operator, validValues))
		}

		// validate toleration effect, empty toleration effect means match all taint effects
		if len(toleration.Effect) > 0 {
			allErrors = append(allErrors, validateTaintEffect(&toleration.Effect, true, idxPath.Child("effect"))...)
		}
	}
	return allErrors
}

// validateContainersOnlyForPod does additional validation for containers on a pod versus a pod template
// it only does additive validation of fields not covered in validateContainers and is not called for
// ephemeral containers which require a conversion to core.Container.
func validateContainersOnlyForPod(containers []core.Container, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	for i, ctr := range containers {
		allErrs = append(allErrs, validateContainerOnlyForPod(&ctr, fldPath.Index(i))...)
	}
	return allErrs
}

// validateContainerOnlyForPod does pod-only (i.e. not pod template) validation for a single container.
// This is called by validateContainersOnlyForPod and validateEphemeralContainers directly.
func validateContainerOnlyForPod(ctr *core.Container, path *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if len(ctr.Image) != len(strings.TrimSpace(ctr.Image)) {
		allErrs = append(allErrs, field.Invalid(path.Child("image"), ctr.Image, "must not have leading or trailing whitespace"))
	}
	return allErrs
}

// PodValidationOptions contains the different settings for pod validation
type PodValidationOptions struct {
	// Allow invalid pod-deletion-cost annotation value for backward compatibility.
	AllowInvalidPodDeletionCost bool
	// Allow invalid label-value in LabelSelector
	AllowInvalidLabelValueInSelector bool
	// Allow pod spec to use non-integer multiple of huge page unit size
	AllowIndivisibleHugePagesValues bool
	// Allow invalid topologySpreadConstraint labelSelector for backward compatibility
	AllowInvalidTopologySpreadConstraintLabelSelector bool
	// Allow projected token volumes with non-local paths
	AllowNonLocalProjectedTokenPath bool
	// Allow namespaced sysctls in hostNet and hostIPC pods
	AllowNamespacedSysctlsForHostNetAndHostIPC bool
	// The top-level resource being validated is a Pod, not just a PodSpec
	// embedded in some other resource.
	ResourceIsPod bool
	// Allow relaxed validation of environment variable names
	AllowRelaxedEnvironmentVariableValidation bool
	// Allow the use of a relaxed DNS search
	AllowRelaxedDNSSearchValidation bool
	// Allows zero value for Pod Lifecycle Sleep Action
	AllowPodLifecycleSleepActionZeroValue bool
	// Allow only Recursive value of SELinuxChangePolicy.
	AllowOnlyRecursiveSELinuxChangePolicy bool
	// Indicates whether PodLevelResources feature is enabled or disabled.
	PodLevelResourcesEnabled bool
	// Allow sidecar containers resize policy for backward compatibility
	AllowSidecarResizePolicy bool
	// Allow invalid label-value in RequiredNodeSelector
	AllowInvalidLabelValueInRequiredNodeAffinity bool
}

// validatePodMetadataAndSpec tests if required fields in the pod.metadata and pod.spec are set,
// and is called by ValidatePodCreate and ValidatePodUpdate.
func validatePodMetadataAndSpec(pod *core.Pod, opts PodValidationOptions) field.ErrorList {
	metaPath := field.NewPath("metadata")
	specPath := field.NewPath("spec")

	allErrs := ValidateObjectMeta(&pod.ObjectMeta, true, ValidatePodName, metaPath)
	allErrs = append(allErrs, ValidatePodSpecificAnnotations(pod.ObjectMeta.Annotations, &pod.Spec, metaPath.Child("annotations"), opts)...)
	allErrs = append(allErrs, ValidatePodSpec(&pod.Spec, &pod.ObjectMeta, specPath, opts)...)

	// we do additional validation only pertinent for pods and not pod templates
	// this was done to preserve backwards compatibility

	if pod.Spec.ServiceAccountName == "" {
		for vi, volume := range pod.Spec.Volumes {
			path := specPath.Child("volumes").Index(vi).Child("projected")
			if volume.Projected != nil {
				for si, source := range volume.Projected.Sources {
					saPath := path.Child("sources").Index(si).Child("serviceAccountToken")
					if source.ServiceAccountToken != nil {
						allErrs = append(allErrs, field.Forbidden(saPath, "must not be specified when serviceAccountName is not set"))
					}
				}
			}
		}
	}

	allErrs = append(allErrs, validateContainersOnlyForPod(pod.Spec.Containers, specPath.Child("containers"))...)
	allErrs = append(allErrs, validateContainersOnlyForPod(pod.Spec.InitContainers, specPath.Child("initContainers"))...)
	// validateContainersOnlyForPod() is checked for ephemeral containers by validateEphemeralContainers()

	return allErrs
}

// validatePodIPs validates IPs in pod status
func validatePodIPs(pod *core.Pod) field.ErrorList {
	allErrs := field.ErrorList{}

	podIPsField := field.NewPath("status", "podIPs")

	// all PodIPs must be valid IPs
	for i, podIP := range pod.Status.PodIPs {
		allErrs = append(allErrs, validation.IsValidIP(podIPsField.Index(i), podIP.IP)...)
	}

	// if we have more than one Pod.PodIP then
	// - validate for dual stack
	// - validate for duplication
	if len(pod.Status.PodIPs) > 1 {
		podIPs := make([]string, 0, len(pod.Status.PodIPs))
		for _, podIP := range pod.Status.PodIPs {
			podIPs = append(podIPs, podIP.IP)
		}

		dualStack, err := netutils.IsDualStackIPStrings(podIPs)
		if err != nil {
			allErrs = append(allErrs, field.InternalError(podIPsField, fmt.Errorf("failed to check for dual stack with error:%v", err)))
		}

		// We only support one from each IP family (i.e. max two IPs in this list).
		if !dualStack || len(podIPs) > 2 {
			allErrs = append(allErrs, field.Invalid(podIPsField, pod.Status.PodIPs, "may specify no more than one IP for each IP family"))
		}

		// There should be no duplicates in list of Pod.PodIPs
		seen := sets.Set[string]{} // := make(map[string]int)
		for i, podIP := range pod.Status.PodIPs {
			if seen.Has(podIP.IP) {
				allErrs = append(allErrs, field.Duplicate(podIPsField.Index(i), podIP))
			}
			seen.Insert(podIP.IP)
		}
	}

	return allErrs
}

// validateHostIPs validates IPs in pod status
func validateHostIPs(pod *core.Pod) field.ErrorList {
	allErrs := field.ErrorList{}

	if len(pod.Status.HostIPs) == 0 {
		return allErrs
	}

	hostIPsField := field.NewPath("status", "hostIPs")

	// hostIP must be equal to hostIPs[0].IP
	if pod.Status.HostIP != pod.Status.HostIPs[0].IP {
		allErrs = append(allErrs, field.Invalid(hostIPsField.Index(0).Child("ip"), pod.Status.HostIPs[0].IP, "must be equal to `hostIP`"))
	}

	// all HostPs must be valid IPs
	for i, hostIP := range pod.Status.HostIPs {
		allErrs = append(allErrs, validation.IsValidIP(hostIPsField.Index(i), hostIP.IP)...)
	}

	// if we have more than one Pod.HostIP then
	// - validate for dual stack
	// - validate for duplication
	if len(pod.Status.HostIPs) > 1 {
		seen := sets.Set[string]{}
		hostIPs := make([]string, 0, len(pod.Status.HostIPs))

		// There should be no duplicates in list of Pod.HostIPs
		for i, hostIP := range pod.Status.HostIPs {
			hostIPs = append(hostIPs, hostIP.IP)
			if seen.Has(hostIP.IP) {
				allErrs = append(allErrs, field.Duplicate(hostIPsField.Index(i), hostIP))
			}
			seen.Insert(hostIP.IP)
		}

		dualStack, err := netutils.IsDualStackIPStrings(hostIPs)
		if err != nil {
			allErrs = append(allErrs, field.InternalError(hostIPsField, fmt.Errorf("failed to check for dual stack with error:%v", err)))
		}

		// We only support one from each IP family (i.e. max two IPs in this list).
		if !dualStack || len(hostIPs) > 2 {
			allErrs = append(allErrs, field.Invalid(hostIPsField, pod.Status.HostIPs, "may specify no more than one IP for each IP family"))
		}
	}

	return allErrs
}

// ValidatePodSpec tests that the specified PodSpec has valid data.
// This includes checking formatting and uniqueness.  It also canonicalizes the
// structure by setting default values and implementing any backwards-compatibility
// tricks.
// The pod metadata is needed to validate generic ephemeral volumes. It is optional
// and should be left empty unless the spec is from a real pod object.
func ValidatePodSpec(spec *core.PodSpec, podMeta *metav1.ObjectMeta, fldPath *field.Path, opts PodValidationOptions) field.ErrorList {
	allErrs := field.ErrorList{}

	if spec.TerminationGracePeriodSeconds == nil {
		allErrs = append(allErrs, field.Required(fldPath.Child("terminationGracePeriodSeconds"), ""))
	}
	gracePeriod := spec.TerminationGracePeriodSeconds

	// The default for hostUsers is true, so a spec with no SecurityContext or no HostUsers field will be true.
	// If the default ever changes, this condition will need to be changed.
	hostUsers := spec.SecurityContext == nil || spec.SecurityContext.HostUsers == nil || *spec.SecurityContext.HostUsers

	vols, vErrs := ValidateVolumes(spec.Volumes, podMeta, fldPath.Child("volumes"), opts)
	allErrs = append(allErrs, vErrs...)
	podClaimNames := gatherPodResourceClaimNames(spec.ResourceClaims)
	allErrs = append(allErrs, validatePodResourceClaims(podMeta, spec.ResourceClaims, fldPath.Child("resourceClaims"))...)
	allErrs = append(allErrs, validateContainers(spec.Containers, vols, podClaimNames, gracePeriod, fldPath.Child("containers"), opts, &spec.RestartPolicy, hostUsers)...)
	allErrs = append(allErrs, validateInitContainers(spec.InitContainers, spec.Containers, vols, podClaimNames, gracePeriod, fldPath.Child("initContainers"), opts, &spec.RestartPolicy, hostUsers)...)
	allErrs = append(allErrs, validateEphemeralContainers(spec.EphemeralContainers, spec.Containers, spec.InitContainers, vols, podClaimNames, fldPath.Child("ephemeralContainers"), opts, &spec.RestartPolicy, hostUsers)...)

	if opts.PodLevelResourcesEnabled {
		allErrs = append(allErrs, validatePodResources(spec, podClaimNames, fldPath.Child("resources"), opts)...)
	}

	allErrs = append(allErrs, validatePodHostNetworkDeps(spec, fldPath, opts)...)
	allErrs = append(allErrs, validateRestartPolicy(&spec.RestartPolicy, fldPath.Child("restartPolicy"))...)
	allErrs = append(allErrs, validateDNSPolicy(&spec.DNSPolicy, fldPath.Child("dnsPolicy"))...)
	allErrs = append(allErrs, unversionedvalidation.ValidateLabels(spec.NodeSelector, fldPath.Child("nodeSelector"))...)
	allErrs = append(allErrs, validatePodSpecSecurityContext(spec.SecurityContext, spec, fldPath, fldPath.Child("securityContext"), opts)...)
	allErrs = append(allErrs, validateImagePullSecrets(spec.ImagePullSecrets, fldPath.Child("imagePullSecrets"))...)
	allErrs = append(allErrs, validateAffinity(spec.Affinity, opts, fldPath.Child("affinity"))...)
	allErrs = append(allErrs, validatePodDNSConfig(spec.DNSConfig, &spec.DNSPolicy, fldPath.Child("dnsConfig"), opts)...)
	allErrs = append(allErrs, validateReadinessGates(spec.ReadinessGates, fldPath.Child("readinessGates"))...)
	allErrs = append(allErrs, validateSchedulingGates(spec.SchedulingGates, fldPath.Child("schedulingGates"))...)
	allErrs = append(allErrs, validateTopologySpreadConstraints(spec.TopologySpreadConstraints, fldPath.Child("topologySpreadConstraints"), opts)...)
	allErrs = append(allErrs, validateWindowsHostProcessPod(spec, fldPath)...)
	allErrs = append(allErrs, validateHostUsers(spec, fldPath)...)
	if len(spec.ServiceAccountName) > 0 {
		for _, msg := range ValidateServiceAccountName(spec.ServiceAccountName, false) {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("serviceAccountName"), spec.ServiceAccountName, msg))
		}
	}

	if len(spec.NodeName) > 0 {
		for _, msg := range ValidateNodeName(spec.NodeName, false) {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("nodeName"), spec.NodeName, msg))
		}
	}

	if spec.ActiveDeadlineSeconds != nil {
		value := *spec.ActiveDeadlineSeconds
		if value < 1 || value > math.MaxInt32 {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("activeDeadlineSeconds"), value, validation.InclusiveRangeError(1, math.MaxInt32)))
		}
	}

	if len(spec.Hostname) > 0 {
		allErrs = append(allErrs, ValidateDNS1123Label(spec.Hostname, fldPath.Child("hostname"))...)
	}

	if len(spec.Subdomain) > 0 {
		allErrs = append(allErrs, ValidateDNS1123Label(spec.Subdomain, fldPath.Child("subdomain"))...)
	}

	if len(spec.Tolerations) > 0 {
		allErrs = append(allErrs, ValidateTolerations(spec.Tolerations, fldPath.Child("tolerations"))...)
	}

	if len(spec.HostAliases) > 0 {
		allErrs = append(allErrs, ValidateHostAliases(spec.HostAliases, fldPath.Child("hostAliases"))...)
	}

	if len(spec.PriorityClassName) > 0 {
		for _, msg := range ValidatePriorityClassName(spec.PriorityClassName, false) {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("priorityClassName"), spec.PriorityClassName, msg))
		}
	}

	if spec.RuntimeClassName != nil {
		allErrs = append(allErrs, ValidateRuntimeClassName(*spec.RuntimeClassName, fldPath.Child("runtimeClassName"))...)
	}

	if spec.PreemptionPolicy != nil {
		allErrs = append(allErrs, ValidatePreemptionPolicy(spec.PreemptionPolicy, fldPath.Child("preemptionPolicy"))...)
	}

	if spec.Overhead != nil {
		allErrs = append(allErrs, validateOverhead(spec.Overhead, fldPath.Child("overhead"), opts)...)
	}

	if spec.OS != nil {
		osErrs := validateOS(spec, fldPath.Child("os"), opts)
		switch {
		case len(osErrs) > 0:
			allErrs = append(allErrs, osErrs...)
		case spec.OS.Name == core.Linux:
			allErrs = append(allErrs, validateLinux(spec, fldPath)...)
		case spec.OS.Name == core.Windows:
			allErrs = append(allErrs, validateWindows(spec, fldPath)...)
		}
	}
	return allErrs
}

func validatePodResources(spec *core.PodSpec, podClaimNames sets.Set[string], fldPath *field.Path, opts PodValidationOptions) field.ErrorList {
	if spec.Resources == nil {
		return nil
	}

	allErrs := field.ErrorList{}

	if spec.Resources.Claims != nil {
		allErrs = append(allErrs, field.Forbidden(fldPath.Child("claims"), "claims cannot be set for Resources at pod-level"))
	}

	// validatePodResourceRequirements checks if resource names and quantities are
	// valid, and requests are less than limits.
	allErrs = append(allErrs, validatePodResourceRequirements(spec.Resources, podClaimNames, fldPath, opts)...)
	allErrs = append(allErrs, validatePodResourceConsistency(spec, fldPath)...)
	return allErrs
}

// validatePodResourceConsistency checks if aggregate container-level requests are
// less than or equal to pod-level requests, and individual container-level limits
// are less than or equal to pod-level limits.
func validatePodResourceConsistency(spec *core.PodSpec, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}

	// Convert the *core.PodSpec to *v1.PodSpec to satisfy the call to
	// resourcehelper.PodRequests method, in the subsequent lines,
	// which requires a *v1.Pod object (containing a *v1.PodSpec).
	v1PodSpec := &v1.PodSpec{}
	// TODO(ndixita): Convert_core_PodSpec_To_v1_PodSpec is risky. Add a copy of
	// AggregateContainerRequests against internal core.Pod type for beta release of
	// PodLevelResources feature.
	if err := corev1.Convert_core_PodSpec_To_v1_PodSpec(spec, v1PodSpec, nil); err != nil {
		allErrs = append(allErrs, field.InternalError(fldPath, fmt.Errorf("invalid %q: %v", fldPath, err.Error())))
	}

	reqPath := fldPath.Child("requests")
	// resourcehelper.AggregateContainerRequests method requires a Pod object to
	// calculate the total requests requirements of a pod. Hence a Pod object using
	// v1PodSpec i.e. (&v1.Pod{Spec: *v1PodSpec}, is created on the fly, and passed
	// to the AggregateContainerRequests method to facilitate proper resource
	// calculation without modifying AggregateContainerRequests method.
	aggrContainerReqs := resourcehelper.AggregateContainerRequests(&v1.Pod{Spec: *v1PodSpec}, resourcehelper.PodResourcesOptions{})

	// Pod-level requests must be >= aggregate requests of all containers in a pod.
	for resourceName, ctrReqs := range aggrContainerReqs {
		key := resourceName.String()
		podSpecRequests := spec.Resources.Requests[core.ResourceName(key)]

		fldPath := reqPath.Key(key)
		if ctrReqs.Cmp(podSpecRequests) > 0 {
			allErrs = append(allErrs, field.Invalid(fldPath, podSpecRequests.String(), fmt.Sprintf("must be greater than or equal to aggregate container requests of %s", ctrReqs.String())))
		}
	}

	// Individual Container limits must be <= Pod-level limits.
	for i, ctr := range spec.Containers {
		for resourceName, ctrLimit := range ctr.Resources.Limits {
			podSpecLimits, exists := spec.Resources.Limits[core.ResourceName(resourceName.String())]
			if !exists {
				continue
			}

			if ctrLimit.Cmp(podSpecLimits) > 0 {
				fldPath := fldPath.Child("containers").Index(i).Key(resourceName.String()).Child("limits")
				allErrs = append(allErrs, field.Invalid(fldPath, ctrLimit.String(), fmt.Sprintf("must be less than or equal to pod limits of %s", podSpecLimits.String())))
			}
		}
	}
	return allErrs
}

func validateLinux(spec *core.PodSpec, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	securityContext := spec.SecurityContext
	if securityContext != nil && securityContext.WindowsOptions != nil {
		allErrs = append(allErrs, field.Forbidden(fldPath.Child("securityContext").Child("windowsOptions"), "windows options cannot be set for a linux pod"))
	}
	podshelper.VisitContainersWithPath(spec, fldPath, func(c *core.Container, cFldPath *field.Path) bool {
		sc := c.SecurityContext
		if sc != nil && sc.WindowsOptions != nil {
			fldPath := cFldPath.Child("securityContext")
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("windowsOptions"), "windows options cannot be set for a linux pod"))
		}
		return true
	})
	return allErrs
}

func validateWindows(spec *core.PodSpec, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	securityContext := spec.SecurityContext
	// validate Pod SecurityContext
	if securityContext != nil {
		if securityContext.AppArmorProfile != nil {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("securityContext").Child("appArmorProfile"), "cannot be set for a windows pod"))
		}
		if securityContext.SELinuxOptions != nil {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("securityContext").Child("seLinuxOptions"), "cannot be set for a windows pod"))
		}
		if securityContext.HostUsers != nil {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("hostUsers"), "cannot be set for a windows pod"))
		}
		if securityContext.HostPID {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("hostPID"), "cannot be set for a windows pod"))
		}
		if securityContext.HostIPC {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("hostIPC"), "cannot be set for a windows pod"))
		}
		if securityContext.SeccompProfile != nil {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("securityContext").Child("seccompProfile"), "cannot be set for a windows pod"))
		}
		if securityContext.FSGroup != nil {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("securityContext").Child("fsGroup"), "cannot be set for a windows pod"))
		}
		if securityContext.FSGroupChangePolicy != nil {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("securityContext").Child("fsGroupChangePolicy"), "cannot be set for a windows pod"))
		}
		if len(securityContext.Sysctls) > 0 {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("securityContext").Child("sysctls"), "cannot be set for a windows pod"))
		}
		if securityContext.ShareProcessNamespace != nil {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("shareProcessNamespace"), "cannot be set for a windows pod"))
		}
		if securityContext.RunAsUser != nil {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("securityContext").Child("runAsUser"), "cannot be set for a windows pod"))
		}
		if securityContext.RunAsGroup != nil {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("securityContext").Child("runAsGroup"), "cannot be set for a windows pod"))
		}
		if securityContext.SupplementalGroups != nil {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("securityContext").Child("supplementalGroups"), "cannot be set for a windows pod"))
		}
		if securityContext.SupplementalGroupsPolicy != nil {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("securityContext").Child("supplementalGroupsPolicy"), "cannot be set for a windows pod"))
		}
		if securityContext.SELinuxChangePolicy != nil {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("securityContext").Child("seLinuxChangePolicy"), "cannot be set for a windows pod"))
		}
	}
	podshelper.VisitContainersWithPath(spec, fldPath, func(c *core.Container, cFldPath *field.Path) bool {
		// validate container security context
		sc := c.SecurityContext
		// OS based podSecurityContext validation
		// There is some naming overlap between Windows and Linux Security Contexts but all the Windows Specific options
		// are set via securityContext.WindowsOptions which we validate below
		// TODO: Think if we need to relax this restriction or some of the restrictions
		if sc != nil {
			fldPath := cFldPath.Child("securityContext")
			if sc.AppArmorProfile != nil {
				allErrs = append(allErrs, field.Forbidden(fldPath.Child("appArmorProfile"), "cannot be set for a windows pod"))
			}
			if sc.SELinuxOptions != nil {
				allErrs = append(allErrs, field.Forbidden(fldPath.Child("seLinuxOptions"), "cannot be set for a windows pod"))
			}
			if sc.SeccompProfile != nil {
				allErrs = append(allErrs, field.Forbidden(fldPath.Child("seccompProfile"), "cannot be set for a windows pod"))
			}
			if sc.Capabilities != nil {
				allErrs = append(allErrs, field.Forbidden(fldPath.Child("capabilities"), "cannot be set for a windows pod"))
			}
			if sc.ReadOnlyRootFilesystem != nil {
				allErrs = append(allErrs, field.Forbidden(fldPath.Child("readOnlyRootFilesystem"), "cannot be set for a windows pod"))
			}
			if sc.Privileged != nil {
				allErrs = append(allErrs, field.Forbidden(fldPath.Child("privileged"), "cannot be set for a windows pod"))
			}
			if sc.AllowPrivilegeEscalation != nil {
				allErrs = append(allErrs, field.Forbidden(fldPath.Child("allowPrivilegeEscalation"), "cannot be set for a windows pod"))
			}
			if sc.ProcMount != nil {
				allErrs = append(allErrs, field.Forbidden(fldPath.Child("procMount"), "cannot be set for a windows pod"))
			}
			if sc.RunAsUser != nil {
				allErrs = append(allErrs, field.Forbidden(fldPath.Child("runAsUser"), "cannot be set for a windows pod"))
			}
			if sc.RunAsGroup != nil {
				allErrs = append(allErrs, field.Forbidden(fldPath.Child("runAsGroup"), "cannot be set for a windows pod"))
			}
		}
		return true
	})
	return allErrs
}

// ValidateNodeSelectorRequirement tests that the specified NodeSelectorRequirement fields has valid data
func ValidateNodeSelectorRequirement(rq core.NodeSelectorRequirement, allowInvalidLabelValueInRequiredNodeAffinity bool, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	switch rq.Operator {
	case core.NodeSelectorOpIn, core.NodeSelectorOpNotIn:
		if len(rq.Values) == 0 {
			allErrs = append(allErrs, field.Required(fldPath.Child("values"), "must be specified when `operator` is 'In' or 'NotIn'"))
		}
	case core.NodeSelectorOpExists, core.NodeSelectorOpDoesNotExist:
		if len(rq.Values) > 0 {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("values"), "may not be specified when `operator` is 'Exists' or 'DoesNotExist'"))
		}

	case core.NodeSelectorOpGt, core.NodeSelectorOpLt:
		if len(rq.Values) != 1 {
			allErrs = append(allErrs, field.Required(fldPath.Child("values"), "must be specified single value when `operator` is 'Lt' or 'Gt'"))
		}
	default:
		allErrs = append(allErrs, field.Invalid(fldPath.Child("operator"), rq.Operator, "not a valid selector operator"))
	}
	allErrs = append(allErrs, unversionedvalidation.ValidateLabelName(rq.Key, fldPath.Child("key"))...)
	if !allowInvalidLabelValueInRequiredNodeAffinity {
		path := fldPath.Child("values")
		for valueIndex, value := range rq.Values {
			for _, msg := range validation.IsValidLabelValue(value) {
				allErrs = append(allErrs, field.Invalid(path.Index(valueIndex), value, msg))
			}
		}
	}
	return allErrs
}

var nodeFieldSelectorValidators = map[string]func(string, bool) []string{
	metav1.ObjectNameField: ValidateNodeName,
}

// ValidateNodeFieldSelectorRequirement tests that the specified NodeSelectorRequirement fields has valid data
func ValidateNodeFieldSelectorRequirement(req core.NodeSelectorRequirement, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}

	switch req.Operator {
	case core.NodeSelectorOpIn, core.NodeSelectorOpNotIn:
		if len(req.Values) != 1 {
			allErrs = append(allErrs, field.Required(fldPath.Child("values"),
				"must be only one value when `operator` is 'In' or 'NotIn' for node field selector"))
		}
	default:
		allErrs = append(allErrs, field.Invalid(fldPath.Child("operator"), req.Operator, "not a valid selector operator"))
	}

	if vf, found := nodeFieldSelectorValidators[req.Key]; !found {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("key"), req.Key, "not a valid field selector key"))
	} else {
		for i, v := range req.Values {
			for _, msg := range vf(v, false) {
				allErrs = append(allErrs, field.Invalid(fldPath.Child("values").Index(i), v, msg))
			}
		}
	}

	return allErrs
}

// ValidateNodeSelectorTerm tests that the specified node selector term has valid data
func ValidateNodeSelectorTerm(term core.NodeSelectorTerm, allowInvalidLabelValueInRequiredNodeAffinity bool, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}

	for j, req := range term.MatchExpressions {
		allErrs = append(allErrs, ValidateNodeSelectorRequirement(req, allowInvalidLabelValueInRequiredNodeAffinity, fldPath.Child("matchExpressions").Index(j))...)
	}

	for j, req := range term.MatchFields {
		allErrs = append(allErrs, ValidateNodeFieldSelectorRequirement(req, fldPath.Child("matchFields").Index(j))...)
	}

	return allErrs
}

// ValidateNodeSelector tests that the specified nodeSelector fields has valid data
func ValidateNodeSelector(nodeSelector *core.NodeSelector, allowInvalidLabelValueInRequiredNodeAffinity bool, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}

	termFldPath := fldPath.Child("nodeSelectorTerms")
	if len(nodeSelector.NodeSelectorTerms) == 0 {
		return append(allErrs, field.Required(termFldPath, "must have at least one node selector term"))
	}

	for i, term := range nodeSelector.NodeSelectorTerms {
		allErrs = append(allErrs, ValidateNodeSelectorTerm(term, allowInvalidLabelValueInRequiredNodeAffinity, termFldPath.Index(i))...)
	}

	return allErrs
}

// validateTopologySelectorLabelRequirement tests that the specified TopologySelectorLabelRequirement fields has valid data,
// and constructs a set containing all of its Values.
func validateTopologySelectorLabelRequirement(rq core.TopologySelectorLabelRequirement, fldPath *field.Path) (sets.Set[string], field.ErrorList) {
	allErrs := field.ErrorList{}
	valueSet := make(sets.Set[string])
	valuesPath := fldPath.Child("values")
	if len(rq.Values) == 0 {
		allErrs = append(allErrs, field.Required(valuesPath, ""))
	}

	// Validate set property of Values field
	for i, value := range rq.Values {
		if valueSet.Has(value) {
			allErrs = append(allErrs, field.Duplicate(valuesPath.Index(i), value))
		}
		valueSet.Insert(value)
	}

	allErrs = append(allErrs, unversionedvalidation.ValidateLabelName(rq.Key, fldPath.Child("key"))...)

	return valueSet, allErrs
}

// ValidateTopologySelectorTerm tests that the specified topology selector term has valid data,
// and constructs a map representing the term in raw form.
func ValidateTopologySelectorTerm(term core.TopologySelectorTerm, fldPath *field.Path) (map[string]sets.Set[string], field.ErrorList) {
	allErrs := field.ErrorList{}
	exprMap := make(map[string]sets.Set[string])
	exprPath := fldPath.Child("matchLabelExpressions")

	// Allow empty MatchLabelExpressions, in case this field becomes optional in the future.
	for i, req := range term.MatchLabelExpressions {
		idxPath := exprPath.Index(i)
		valueSet, exprErrs := validateTopologySelectorLabelRequirement(req, idxPath)
		allErrs = append(allErrs, exprErrs...)

		// Validate no duplicate keys exist.
		if _, exists := exprMap[req.Key]; exists {
			allErrs = append(allErrs, field.Duplicate(idxPath.Child("key"), req.Key))
		}
		exprMap[req.Key] = valueSet
	}

	return exprMap, allErrs
}

// ValidateAvoidPodsInNodeAnnotations tests that the serialized AvoidPods in Node.Annotations has valid data
func ValidateAvoidPodsInNodeAnnotations(annotations map[string]string, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}

	v1Avoids, err := schedulinghelper.GetAvoidPodsFromNodeAnnotations(annotations)
	if err != nil {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("AvoidPods"), core.PreferAvoidPodsAnnotationKey, err.Error()))
		return allErrs
	}
	var avoids core.AvoidPods
	if err := corev1.Convert_v1_AvoidPods_To_core_AvoidPods(&v1Avoids, &avoids, nil); err != nil {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("AvoidPods"), core.PreferAvoidPodsAnnotationKey, err.Error()))
		return allErrs
	}

	if len(avoids.PreferAvoidPods) != 0 {
		for i, pa := range avoids.PreferAvoidPods {
			idxPath := fldPath.Child(core.PreferAvoidPodsAnnotationKey).Index(i)
			allErrs = append(allErrs, validatePreferAvoidPodsEntry(pa, idxPath)...)
		}
	}

	return allErrs
}

// validatePreferAvoidPodsEntry tests if given PreferAvoidPodsEntry has valid data.
func validatePreferAvoidPodsEntry(avoidPodEntry core.PreferAvoidPodsEntry, fldPath *field.Path) field.ErrorList {
	allErrors := field.ErrorList{}
	if avoidPodEntry.PodSignature.PodController == nil {
		allErrors = append(allErrors, field.Required(fldPath.Child("PodSignature"), ""))
	} else {
		if !*(avoidPodEntry.PodSignature.PodController.Controller) {
			allErrors = append(allErrors,
				field.Invalid(fldPath.Child("PodSignature").Child("PodController").Child("Controller"),
					*(avoidPodEntry.PodSignature.PodController.Controller), "must point to a controller"))
		}
	}
	return allErrors
}

// ValidatePreferredSchedulingTerms tests that the specified SoftNodeAffinity fields has valid data
func ValidatePreferredSchedulingTerms(terms []core.PreferredSchedulingTerm, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}

	for i, term := range terms {
		if term.Weight <= 0 || term.Weight > 100 {
			allErrs = append(allErrs, field.Invalid(fldPath.Index(i).Child("weight"), term.Weight, "must be in the range 1-100"))
		}

		// we always allow invalid label-value for preferred affinity
		// as they can success when cluster has only one node
		allErrs = append(allErrs, ValidateNodeSelectorTerm(term.Preference, true, fldPath.Index(i).Child("preference"))...)
	}
	return allErrs
}

// validatePodAffinityTerm tests that the specified podAffinityTerm fields have valid data
func validatePodAffinityTerm(podAffinityTerm core.PodAffinityTerm, allowInvalidLabelValueInSelector bool, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}

	allErrs = append(allErrs, ValidatePodAffinityTermSelector(podAffinityTerm, allowInvalidLabelValueInSelector, fldPath)...)
	for _, name := range podAffinityTerm.Namespaces {
		for _, msg := range ValidateNamespaceName(name, false) {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("namespace"), name, msg))
		}
	}
	allErrs = append(allErrs, validateMatchLabelKeysAndMismatchLabelKeys(fldPath, podAffinityTerm.MatchLabelKeys, podAffinityTerm.MismatchLabelKeys, podAffinityTerm.LabelSelector)...)
	if len(podAffinityTerm.TopologyKey) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("topologyKey"), "can not be empty"))
	}
	return append(allErrs, unversionedvalidation.ValidateLabelName(podAffinityTerm.TopologyKey, fldPath.Child("topologyKey"))...)
}

// validatePodAffinityTerms tests that the specified podAffinityTerms fields have valid data
func validatePodAffinityTerms(podAffinityTerms []core.PodAffinityTerm, allowInvalidLabelValueInSelector bool, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	for i, podAffinityTerm := range podAffinityTerms {
		allErrs = append(allErrs, validatePodAffinityTerm(podAffinityTerm, allowInvalidLabelValueInSelector, fldPath.Index(i))...)
	}
	return allErrs
}

// validateWeightedPodAffinityTerms tests that the specified weightedPodAffinityTerms fields have valid data
func validateWeightedPodAffinityTerms(weightedPodAffinityTerms []core.WeightedPodAffinityTerm, allowInvalidLabelValueInSelector bool, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	for j, weightedTerm := range weightedPodAffinityTerms {
		if weightedTerm.Weight <= 0 || weightedTerm.Weight > 100 {
			allErrs = append(allErrs, field.Invalid(fldPath.Index(j).Child("weight"), weightedTerm.Weight, "must be in the range 1-100"))
		}
		allErrs = append(allErrs, validatePodAffinityTerm(weightedTerm.PodAffinityTerm, allowInvalidLabelValueInSelector, fldPath.Index(j).Child("podAffinityTerm"))...)
	}
	return allErrs
}

// validatePodAntiAffinity tests that the specified podAntiAffinity fields have valid data
func validatePodAntiAffinity(podAntiAffinity *core.PodAntiAffinity, allowInvalidLabelValueInSelector bool, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	// TODO:Uncomment below code once RequiredDuringSchedulingRequiredDuringExecution is implemented.
	// if podAntiAffinity.RequiredDuringSchedulingRequiredDuringExecution != nil {
	//	allErrs = append(allErrs, validatePodAffinityTerms(podAntiAffinity.RequiredDuringSchedulingRequiredDuringExecution, false,
	//		fldPath.Child("requiredDuringSchedulingRequiredDuringExecution"))...)
	// }
	if podAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution != nil {
		allErrs = append(allErrs, validatePodAffinityTerms(podAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution, allowInvalidLabelValueInSelector,
			fldPath.Child("requiredDuringSchedulingIgnoredDuringExecution"))...)
	}
	if podAntiAffinity.PreferredDuringSchedulingIgnoredDuringExecution != nil {
		allErrs = append(allErrs, validateWeightedPodAffinityTerms(podAntiAffinity.PreferredDuringSchedulingIgnoredDuringExecution, allowInvalidLabelValueInSelector,
			fldPath.Child("preferredDuringSchedulingIgnoredDuringExecution"))...)
	}
	return allErrs
}

// validateNodeAffinity tests that the specified nodeAffinity fields have valid data
func validateNodeAffinity(na *core.NodeAffinity, opts PodValidationOptions, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	// TODO: Uncomment the next three lines once RequiredDuringSchedulingRequiredDuringExecution is implemented.
	// if na.RequiredDuringSchedulingRequiredDuringExecution != nil {
	//	allErrs = append(allErrs, ValidateNodeSelector(na.RequiredDuringSchedulingRequiredDuringExecution, fldPath.Child("requiredDuringSchedulingRequiredDuringExecution"))...)
	// }
	if na.RequiredDuringSchedulingIgnoredDuringExecution != nil {
		allErrs = append(allErrs, ValidateNodeSelector(na.RequiredDuringSchedulingIgnoredDuringExecution, opts.AllowInvalidLabelValueInRequiredNodeAffinity, fldPath.Child("requiredDuringSchedulingIgnoredDuringExecution"))...)
	}
	if len(na.PreferredDuringSchedulingIgnoredDuringExecution) > 0 {
		allErrs = append(allErrs, ValidatePreferredSchedulingTerms(na.PreferredDuringSchedulingIgnoredDuringExecution, fldPath.Child("preferredDuringSchedulingIgnoredDuringExecution"))...)
	}
	return allErrs
}

// validatePodAffinity tests that the specified podAffinity fields have valid data
func validatePodAffinity(podAffinity *core.PodAffinity, allowInvalidLabelValueInSelector bool, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	// TODO:Uncomment below code once RequiredDuringSchedulingRequiredDuringExecution is implemented.
	// if podAffinity.RequiredDuringSchedulingRequiredDuringExecution != nil {
	//	allErrs = append(allErrs, validatePodAffinityTerms(podAffinity.RequiredDuringSchedulingRequiredDuringExecution, false,
	//		fldPath.Child("requiredDuringSchedulingRequiredDuringExecution"))...)
	// }
	if podAffinity.RequiredDuringSchedulingIgnoredDuringExecution != nil {
		allErrs = append(allErrs, validatePodAffinityTerms(podAffinity.RequiredDuringSchedulingIgnoredDuringExecution, allowInvalidLabelValueInSelector,
			fldPath.Child("requiredDuringSchedulingIgnoredDuringExecution"))...)
	}
	if podAffinity.PreferredDuringSchedulingIgnoredDuringExecution != nil {
		allErrs = append(allErrs, validateWeightedPodAffinityTerms(podAffinity.PreferredDuringSchedulingIgnoredDuringExecution, allowInvalidLabelValueInSelector,
			fldPath.Child("preferredDuringSchedulingIgnoredDuringExecution"))...)
	}
	return allErrs
}

func validateSeccompProfileField(sp *core.SeccompProfile, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if sp == nil {
		return allErrs
	}

	if err := validateSeccompProfileType(fldPath.Child("type"), sp.Type); err != nil {
		allErrs = append(allErrs, err)
	}

	if sp.Type == core.SeccompProfileTypeLocalhost {
		if sp.LocalhostProfile == nil {
			allErrs = append(allErrs, field.Required(fldPath.Child("localhostProfile"), "must be set when seccomp type is Localhost"))
		} else {
			allErrs = append(allErrs, validateLocalDescendingPath(*sp.LocalhostProfile, fldPath.Child("localhostProfile"))...)
		}
	} else {
		if sp.LocalhostProfile != nil {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("localhostProfile"), sp, "can only be set when seccomp type is Localhost"))
		}
	}

	return allErrs
}

func ValidateSeccompProfile(p string, fldPath *field.Path) field.ErrorList {
	if p == core.SeccompProfileRuntimeDefault || p == core.DeprecatedSeccompProfileDockerDefault {
		return nil
	}
	if p == v1.SeccompProfileNameUnconfined {
		return nil
	}
	if strings.HasPrefix(p, v1.SeccompLocalhostProfileNamePrefix) {
		return validateLocalDescendingPath(strings.TrimPrefix(p, v1.SeccompLocalhostProfileNamePrefix), fldPath)
	}
	return field.ErrorList{field.Invalid(fldPath, p, "must be a valid seccomp profile")}
}

func ValidateSeccompPodAnnotations(annotations map[string]string, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if p, exists := annotations[core.SeccompPodAnnotationKey]; exists {
		allErrs = append(allErrs, ValidateSeccompProfile(p, fldPath.Child(core.SeccompPodAnnotationKey))...)
	}
	for k, p := range annotations {
		if strings.HasPrefix(k, core.SeccompContainerAnnotationKeyPrefix) {
			allErrs = append(allErrs, ValidateSeccompProfile(p, fldPath.Child(k))...)
		}
	}

	return allErrs
}

// ValidateSeccompProfileType tests that the argument is a valid SeccompProfileType.
func validateSeccompProfileType(fldPath *field.Path, seccompProfileType core.SeccompProfileType) *field.Error {
	switch seccompProfileType {
	case core.SeccompProfileTypeLocalhost, core.SeccompProfileTypeRuntimeDefault, core.SeccompProfileTypeUnconfined:
		return nil
	case "":
		return field.Required(fldPath, "type is required when seccompProfile is set")
	default:
		return field.NotSupported(fldPath, seccompProfileType, []core.SeccompProfileType{core.SeccompProfileTypeLocalhost, core.SeccompProfileTypeRuntimeDefault, core.SeccompProfileTypeUnconfined})
	}
}

func ValidateAppArmorProfileField(profile *core.AppArmorProfile, fldPath *field.Path) field.ErrorList {
	if profile == nil {
		return nil
	}

	allErrs := field.ErrorList{}

	switch profile.Type {
	case core.AppArmorProfileTypeLocalhost:
		if profile.LocalhostProfile == nil {
			allErrs = append(allErrs, field.Required(fldPath.Child("localhostProfile"), "must be set when AppArmor type is Localhost"))
		} else {
			localhostProfile := strings.TrimSpace(*profile.LocalhostProfile)
			if localhostProfile != *profile.LocalhostProfile {
				allErrs = append(allErrs, field.Invalid(fldPath.Child("localhostProfile"), *profile.LocalhostProfile, "must not be padded with whitespace"))
			} else if localhostProfile == "" {
				allErrs = append(allErrs, field.Required(fldPath.Child("localhostProfile"), "must be set when AppArmor type is Localhost"))
			}

			const maxLocalhostProfileLength = 4095 // PATH_MAX - 1
			if len(*profile.LocalhostProfile) > maxLocalhostProfileLength {
				allErrs = append(allErrs, field.TooLong(fldPath.Child("localhostProfile"), "" /*unused*/, maxLocalhostProfileLength))
			}
		}

	case core.AppArmorProfileTypeRuntimeDefault, core.AppArmorProfileTypeUnconfined:
		if profile.LocalhostProfile != nil {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("localhostProfile"), profile.LocalhostProfile, "can only be set when AppArmor type is Localhost"))
		}

	case "":
		allErrs = append(allErrs, field.Required(fldPath.Child("type"), "type is required when appArmorProfile is set"))

	default:
		allErrs = append(allErrs, field.NotSupported(fldPath.Child("type"), profile.Type,
			[]core.AppArmorProfileType{core.AppArmorProfileTypeLocalhost, core.AppArmorProfileTypeRuntimeDefault, core.AppArmorProfileTypeUnconfined}))
	}

	return allErrs

}

func ValidateAppArmorPodAnnotations(annotations map[string]string, spec *core.PodSpec, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	for k, p := range annotations {
		if !strings.HasPrefix(k, v1.DeprecatedAppArmorBetaContainerAnnotationKeyPrefix) {
			continue
		}
		containerName := strings.TrimPrefix(k, v1.DeprecatedAppArmorBetaContainerAnnotationKeyPrefix)
		if !podSpecHasContainer(spec, containerName) {
			allErrs = append(allErrs, field.Invalid(fldPath.Key(k), containerName, "container not found"))
		}

		if err := ValidateAppArmorProfileFormat(p); err != nil {
			allErrs = append(allErrs, field.Invalid(fldPath.Key(k), p, err.Error()))
		}
	}

	return allErrs
}

func ValidateAppArmorProfileFormat(profile string) error {
	if profile == "" || profile == v1.DeprecatedAppArmorBetaProfileRuntimeDefault || profile == v1.DeprecatedAppArmorBetaProfileNameUnconfined {
		return nil
	}
	if !strings.HasPrefix(profile, v1.DeprecatedAppArmorBetaProfileNamePrefix) {
		return fmt.Errorf("invalid AppArmor profile name: %q", profile)
	}
	return nil
}

// validateAppArmorAnnotationsAndFieldsMatchOnCreate validates that AppArmor fields and annotations are consistent.
func validateAppArmorAnnotationsAndFieldsMatchOnCreate(objectMeta metav1.ObjectMeta, podSpec *core.PodSpec, specPath *field.Path) field.ErrorList {
	if podSpec.OS != nil && podSpec.OS.Name == core.Windows {
		// Skip consistency check for windows pods.
		return nil
	}

	allErrs := field.ErrorList{}

	var podProfile *core.AppArmorProfile
	if podSpec.SecurityContext != nil {
		podProfile = podSpec.SecurityContext.AppArmorProfile
	}
	podshelper.VisitContainersWithPath(podSpec, specPath, func(c *core.Container, cFldPath *field.Path) bool {
		containerProfile := podProfile
		if c.SecurityContext != nil && c.SecurityContext.AppArmorProfile != nil {
			containerProfile = c.SecurityContext.AppArmorProfile
		}

		if containerProfile == nil {
			return true
		}

		key := core.DeprecatedAppArmorAnnotationKeyPrefix + c.Name
		if annotation, found := objectMeta.Annotations[key]; found {
			apparmorPath := cFldPath.Child("securityContext").Child("appArmorProfile")

			switch containerProfile.Type {
			case core.AppArmorProfileTypeUnconfined:
				if annotation != core.DeprecatedAppArmorAnnotationValueUnconfined {
					allErrs = append(allErrs, field.Forbidden(apparmorPath.Child("type"), "apparmor type in annotation and field must match"))
				}

			case core.AppArmorProfileTypeRuntimeDefault:
				if annotation != core.DeprecatedAppArmorAnnotationValueRuntimeDefault {
					allErrs = append(allErrs, field.Forbidden(apparmorPath.Child("type"), "apparmor type in annotation and field must match"))
				}

			case core.AppArmorProfileTypeLocalhost:
				if !strings.HasPrefix(annotation, core.DeprecatedAppArmorAnnotationValueLocalhostPrefix) {
					allErrs = append(allErrs, field.Forbidden(apparmorPath.Child("type"), "apparmor type in annotation and field must match"))
				} else if containerProfile.LocalhostProfile == nil || strings.TrimPrefix(annotation, core.DeprecatedAppArmorAnnotationValueLocalhostPrefix) != *containerProfile.LocalhostProfile {
					allErrs = append(allErrs, field.Forbidden(apparmorPath.Child("localhostProfile"), "apparmor profile in annotation and field must match"))
				}
			}
		}
		return true
	})

	return allErrs
}

func podSpecHasContainer(spec *core.PodSpec, containerName string) bool {
	var hasContainer bool
	podshelper.VisitContainersWithPath(spec, field.NewPath("spec"), func(c *core.Container, _ *field.Path) bool {
		if c.Name == containerName {
			hasContainer = true
			return false
		}
		return true
	})
	return hasContainer
}

const (
	// a sysctl segment regex, concatenated with dots to form a sysctl name
	SysctlSegmentFmt string = "[a-z0-9]([-_a-z0-9]*[a-z0-9])?"

	// a sysctl name regex with slash allowed
	SysctlContainSlashFmt string = "(" + SysctlSegmentFmt + "[\\./])*" + SysctlSegmentFmt

	// the maximal length of a sysctl name
	SysctlMaxLength int = 253
)

var sysctlContainSlashRegexp = regexp.MustCompile("^" + SysctlContainSlashFmt + "$")

// IsValidSysctlName checks that the given string is a valid sysctl name,
// i.e. matches SysctlContainSlashFmt.
// More info:
//
//	https://man7.org/linux/man-pages/man8/sysctl.8.html
//	https://man7.org/linux/man-pages/man5/sysctl.d.5.html
func IsValidSysctlName(name string) bool {
	if len(name) > SysctlMaxLength {
		return false
	}
	return sysctlContainSlashRegexp.MatchString(name)
}

func validateSysctls(securityContext *core.PodSecurityContext, fldPath *field.Path, opts PodValidationOptions) field.ErrorList {
	allErrs := field.ErrorList{}
	names := make(map[string]struct{})
	for i, s := range securityContext.Sysctls {
		if len(s.Name) == 0 {
			allErrs = append(allErrs, field.Required(fldPath.Index(i).Child("name"), ""))
		} else if !IsValidSysctlName(s.Name) {
			allErrs = append(allErrs, field.Invalid(fldPath.Index(i).Child("name"), s.Name, fmt.Sprintf("must have at most %d characters and match regex %s", SysctlMaxLength, sysctlContainSlashRegexp)))
		} else if _, ok := names[s.Name]; ok {
			allErrs = append(allErrs, field.Duplicate(fldPath.Index(i).Child("name"), s.Name))
		}
		if !opts.AllowNamespacedSysctlsForHostNetAndHostIPC {
			err := ValidateHostSysctl(s.Name, securityContext, fldPath.Index(i).Child("name"))
			if err != nil {
				allErrs = append(allErrs, err)
			}
		}
		names[s.Name] = struct{}{}
	}
	return allErrs
}

// ValidateHostSysctl will return error if namespaced sysctls is applied to pod sharing the respective namespaces with the host.
func ValidateHostSysctl(sysctl string, securityContext *core.PodSecurityContext, fldPath *field.Path) *field.Error {
	ns, _, _ := utilsysctl.GetNamespace(sysctl)
	switch {
	case securityContext.HostNetwork && ns == utilsysctl.NetNamespace:
		return field.Invalid(fldPath, sysctl, "may not be specified when 'hostNetwork' is true")
	case securityContext.HostIPC && ns == utilsysctl.IPCNamespace:
		return field.Invalid(fldPath, sysctl, "may not be specified when 'hostIPC' is true")
	}
	return nil
}

var validSELinuxChangePolicies = sets.New(core.SELinuxChangePolicyRecursive, core.SELinuxChangePolicyMountOption)

func validateSELinuxChangePolicy(seLinuxChangePolicy *core.PodSELinuxChangePolicy, fldPath *field.Path, opts PodValidationOptions) field.ErrorList {
	if seLinuxChangePolicy == nil {
		return nil
	}

	allErrs := field.ErrorList{}

	if opts.AllowOnlyRecursiveSELinuxChangePolicy {
		if *seLinuxChangePolicy != core.SELinuxChangePolicyRecursive {
			allErrs = append(allErrs, field.NotSupported(fldPath, *seLinuxChangePolicy, []core.PodSELinuxChangePolicy{core.SELinuxChangePolicyRecursive}))
		}
	} else {
		// Allow any valid SELinuxChangePolicy value.
		if !validSELinuxChangePolicies.Has(*seLinuxChangePolicy) {
			allErrs = append(allErrs, field.NotSupported(fldPath, *seLinuxChangePolicy, sets.List(validSELinuxChangePolicies)))
		}
	}
	return allErrs
}

// validatePodSpecSecurityContext verifies the SecurityContext of a PodSpec,
// whether that is defined in a Pod or in an embedded PodSpec (e.g. a
// Deployment's pod template).
func validatePodSpecSecurityContext(securityContext *core.PodSecurityContext, spec *core.PodSpec, specPath, fldPath *field.Path, opts PodValidationOptions) field.ErrorList {
	allErrs := field.ErrorList{}

	if securityContext != nil {
		if securityContext.FSGroup != nil {
			for _, msg := range validation.IsValidGroupID(*securityContext.FSGroup) {
				allErrs = append(allErrs, field.Invalid(fldPath.Child("fsGroup"), *(securityContext.FSGroup), msg))
			}
		}
		if securityContext.RunAsUser != nil {
			for _, msg := range validation.IsValidUserID(*securityContext.RunAsUser) {
				allErrs = append(allErrs, field.Invalid(fldPath.Child("runAsUser"), *(securityContext.RunAsUser), msg))
			}
		}
		if securityContext.RunAsGroup != nil {
			for _, msg := range validation.IsValidGroupID(*securityContext.RunAsGroup) {
				allErrs = append(allErrs, field.Invalid(fldPath.Child("runAsGroup"), *(securityContext.RunAsGroup), msg))
			}
		}
		for g, gid := range securityContext.SupplementalGroups {
			for _, msg := range validation.IsValidGroupID(gid) {
				allErrs = append(allErrs, field.Invalid(fldPath.Child("supplementalGroups").Index(g), gid, msg))
			}
		}
		if securityContext.ShareProcessNamespace != nil && securityContext.HostPID && *securityContext.ShareProcessNamespace {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("shareProcessNamespace"), *securityContext.ShareProcessNamespace, "ShareProcessNamespace and HostPID cannot both be enabled"))
		}

		if len(securityContext.Sysctls) != 0 {
			allErrs = append(allErrs, validateSysctls(securityContext, fldPath.Child("sysctls"), opts)...)
		}

		if securityContext.FSGroupChangePolicy != nil {
			allErrs = append(allErrs, validateFSGroupChangePolicy(securityContext.FSGroupChangePolicy, fldPath.Child("fsGroupChangePolicy"))...)
		}

		allErrs = append(allErrs, validateSeccompProfileField(securityContext.SeccompProfile, fldPath.Child("seccompProfile"))...)
		allErrs = append(allErrs, validateWindowsSecurityContextOptions(securityContext.WindowsOptions, fldPath.Child("windowsOptions"))...)
		allErrs = append(allErrs, ValidateAppArmorProfileField(securityContext.AppArmorProfile, fldPath.Child("appArmorProfile"))...)

		if securityContext.SupplementalGroupsPolicy != nil {
			allErrs = append(allErrs, validateSupplementalGroupsPolicy(securityContext.SupplementalGroupsPolicy, fldPath.Child("supplementalGroupsPolicy"))...)
		}

		if securityContext.SELinuxChangePolicy != nil {
			allErrs = append(allErrs, validateSELinuxChangePolicy(securityContext.SELinuxChangePolicy, fldPath.Child("seLinuxChangePolicy"), opts)...)
		}
	}

	return allErrs
}

func ValidateContainerUpdates(newContainers, oldContainers []core.Container, fldPath *field.Path) (allErrs field.ErrorList, stop bool) {
	allErrs = field.ErrorList{}
	if len(newContainers) != len(oldContainers) {
		// TODO: Pinpoint the specific container that causes the invalid error after we have strategic merge diff
		allErrs = append(allErrs, field.Forbidden(fldPath, "pod updates may not add or remove containers"))
		return allErrs, true
	}

	// validate updated container images
	for i, ctr := range newContainers {
		if len(ctr.Image) == 0 {
			allErrs = append(allErrs, field.Required(fldPath.Index(i).Child("image"), ""))
		}
		// this is only called from ValidatePodUpdate so its safe to check leading/trailing whitespace.
		if len(strings.TrimSpace(ctr.Image)) != len(ctr.Image) {
			allErrs = append(allErrs, field.Invalid(fldPath.Index(i).Child("image"), ctr.Image, "must not have leading or trailing whitespace"))
		}
	}
	return allErrs, false
}

// ValidatePodCreate validates a pod in the context of its initial create
func ValidatePodCreate(pod *core.Pod, opts PodValidationOptions) field.ErrorList {
	allErrs := validatePodMetadataAndSpec(pod, opts)

	fldPath := field.NewPath("spec")
	// EphemeralContainers can only be set on update using the ephemeralcontainers subresource
	if len(pod.Spec.EphemeralContainers) > 0 {
		allErrs = append(allErrs, field.Forbidden(fldPath.Child("ephemeralContainers"), "cannot be set on create"))
	}
	// A Pod cannot be assigned a Node if there are remaining scheduling gates.
	if pod.Spec.NodeName != "" && len(pod.Spec.SchedulingGates) != 0 {
		allErrs = append(allErrs, field.Forbidden(fldPath.Child("nodeName"), "cannot be set until all schedulingGates have been cleared"))
	}
	allErrs = append(allErrs, validateSeccompAnnotationsAndFields(pod.ObjectMeta, &pod.Spec, fldPath)...)
	allErrs = append(allErrs, validateAppArmorAnnotationsAndFieldsMatchOnCreate(pod.ObjectMeta, &pod.Spec, fldPath)...)

	return allErrs
}

// validateSeccompAnnotationsAndFields iterates through all containers and ensure that when both seccompProfile and seccomp annotations exist they match.
func validateSeccompAnnotationsAndFields(objectMeta metav1.ObjectMeta, podSpec *core.PodSpec, specPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}

	if podSpec.SecurityContext != nil && podSpec.SecurityContext.SeccompProfile != nil {
		// If both seccomp annotations and fields are specified, the values must match.
		if annotation, found := objectMeta.Annotations[v1.SeccompPodAnnotationKey]; found {
			seccompPath := specPath.Child("securityContext").Child("seccompProfile")
			err := validateSeccompAnnotationsAndFieldsMatch(annotation, podSpec.SecurityContext.SeccompProfile, seccompPath)
			if err != nil {
				allErrs = append(allErrs, err)
			}
		}
	}

	podshelper.VisitContainersWithPath(podSpec, specPath, func(c *core.Container, cFldPath *field.Path) bool {
		var field *core.SeccompProfile
		if c.SecurityContext != nil {
			field = c.SecurityContext.SeccompProfile
		}

		if field == nil {
			return true
		}

		key := v1.SeccompContainerAnnotationKeyPrefix + c.Name
		if annotation, found := objectMeta.Annotations[key]; found {
			seccompPath := cFldPath.Child("securityContext").Child("seccompProfile")
			err := validateSeccompAnnotationsAndFieldsMatch(annotation, field, seccompPath)
			if err != nil {
				allErrs = append(allErrs, err)
			}
		}
		return true
	})

	return allErrs
}

func validateSeccompAnnotationsAndFieldsMatch(annotationValue string, seccompField *core.SeccompProfile, fldPath *field.Path) *field.Error {
	if seccompField == nil {
		return nil
	}

	switch seccompField.Type {
	case core.SeccompProfileTypeUnconfined:
		if annotationValue != v1.SeccompProfileNameUnconfined {
			return field.Forbidden(fldPath.Child("type"), "seccomp type in annotation and field must match")
		}

	case core.SeccompProfileTypeRuntimeDefault:
		if annotationValue != v1.SeccompProfileRuntimeDefault && annotationValue != v1.DeprecatedSeccompProfileDockerDefault {
			return field.Forbidden(fldPath.Child("type"), "seccomp type in annotation and field must match")
		}

	case core.SeccompProfileTypeLocalhost:
		if !strings.HasPrefix(annotationValue, v1.SeccompLocalhostProfileNamePrefix) {
			return field.Forbidden(fldPath.Child("type"), "seccomp type in annotation and field must match")
		} else if seccompField.LocalhostProfile == nil || strings.TrimPrefix(annotationValue, v1.SeccompLocalhostProfileNamePrefix) != *seccompField.LocalhostProfile {
			return field.Forbidden(fldPath.Child("localhostProfile"), "seccomp profile in annotation and field must match")
		}
	}

	return nil
}

var updatablePodSpecFields = []string{
	"`spec.containers[*].image`",
	"`spec.initContainers[*].image`",
	"`spec.activeDeadlineSeconds`",
	"`spec.tolerations` (only additions to existing tolerations)",
	"`spec.terminationGracePeriodSeconds` (allow it to be set to 1 if it was previously negative)",
}

// ValidatePodUpdate tests to see if the update is legal for an end user to make. newPod is updated with fields
// that cannot be changed.
func ValidatePodUpdate(newPod, oldPod *core.Pod, opts PodValidationOptions) field.ErrorList {
	fldPath := field.NewPath("metadata")
	allErrs := ValidateObjectMetaUpdate(&newPod.ObjectMeta, &oldPod.ObjectMeta, fldPath)
	allErrs = append(allErrs, validatePodMetadataAndSpec(newPod, opts)...)
	allErrs = append(allErrs, ValidatePodSpecificAnnotationUpdates(newPod, oldPod, fldPath.Child("annotations"), opts)...)
	specPath := field.NewPath("spec")

	// validate updateable fields:
	// 1.  spec.containers[*].image
	// 2.  spec.initContainers[*].image
	// 3.  spec.activeDeadlineSeconds
	// 4.  spec.terminationGracePeriodSeconds
	// 5.  spec.schedulingGates

	containerErrs, stop := ValidateContainerUpdates(newPod.Spec.Containers, oldPod.Spec.Containers, specPath.Child("containers"))
	allErrs = append(allErrs, containerErrs...)
	if stop {
		return allErrs
	}
	containerErrs, stop = ValidateContainerUpdates(newPod.Spec.InitContainers, oldPod.Spec.InitContainers, specPath.Child("initContainers"))
	allErrs = append(allErrs, containerErrs...)
	if stop {
		return allErrs
	}

	// validate updated spec.activeDeadlineSeconds.  two types of updates are allowed:
	// 1.  from nil to a positive value
	// 2.  from a positive value to a lesser, non-negative value
	if newPod.Spec.ActiveDeadlineSeconds != nil {
		newActiveDeadlineSeconds := *newPod.Spec.ActiveDeadlineSeconds
		if newActiveDeadlineSeconds < 0 || newActiveDeadlineSeconds > math.MaxInt32 {
			allErrs = append(allErrs, field.Invalid(specPath.Child("activeDeadlineSeconds"), newActiveDeadlineSeconds, validation.InclusiveRangeError(0, math.MaxInt32)))
			return allErrs
		}
		if oldPod.Spec.ActiveDeadlineSeconds != nil {
			oldActiveDeadlineSeconds := *oldPod.Spec.ActiveDeadlineSeconds
			if oldActiveDeadlineSeconds < newActiveDeadlineSeconds {
				allErrs = append(allErrs, field.Invalid(specPath.Child("activeDeadlineSeconds"), newActiveDeadlineSeconds, "must be less than or equal to previous value"))
				return allErrs
			}
		}
	} else if oldPod.Spec.ActiveDeadlineSeconds != nil {
		allErrs = append(allErrs, field.Invalid(specPath.Child("activeDeadlineSeconds"), newPod.Spec.ActiveDeadlineSeconds, "must not update from a positive integer to nil value"))
	}

	// Allow only additions to tolerations updates.
	allErrs = append(allErrs, validateOnlyAddedTolerations(newPod.Spec.Tolerations, oldPod.Spec.Tolerations, specPath.Child("tolerations"))...)

	// Allow only deletions to schedulingGates updates.
	allErrs = append(allErrs, validateOnlyDeletedSchedulingGates(newPod.Spec.SchedulingGates, oldPod.Spec.SchedulingGates, specPath.Child("schedulingGates"))...)

	// the last thing to check is pod spec equality.  If the pod specs are equal, then we can simply return the errors we have
	// so far and save the cost of a deep copy.
	if apiequality.Semantic.DeepEqual(newPod.Spec, oldPod.Spec) {
		return allErrs
	}

	// handle updateable fields by munging those fields prior to deep equal comparison.
	mungedPodSpec := *newPod.Spec.DeepCopy()
	// munge spec.containers[*].image
	var newContainers []core.Container
	for ix, container := range mungedPodSpec.Containers {
		container.Image = oldPod.Spec.Containers[ix].Image // +k8s:verify-mutation:reason=clone
		newContainers = append(newContainers, container)
	}
	mungedPodSpec.Containers = newContainers
	// munge spec.initContainers[*].image
	var newInitContainers []core.Container
	for ix, container := range mungedPodSpec.InitContainers {
		container.Image = oldPod.Spec.InitContainers[ix].Image // +k8s:verify-mutation:reason=clone
		newInitContainers = append(newInitContainers, container)
	}
	mungedPodSpec.InitContainers = newInitContainers
	// munge spec.activeDeadlineSeconds
	mungedPodSpec.ActiveDeadlineSeconds = nil
	if oldPod.Spec.ActiveDeadlineSeconds != nil {
		activeDeadlineSeconds := *oldPod.Spec.ActiveDeadlineSeconds
		mungedPodSpec.ActiveDeadlineSeconds = &activeDeadlineSeconds
	}
	// munge spec.schedulingGates
	mungedPodSpec.SchedulingGates = oldPod.Spec.SchedulingGates // +k8s:verify-mutation:reason=clone
	// tolerations are checked before the deep copy, so munge those too
	mungedPodSpec.Tolerations = oldPod.Spec.Tolerations // +k8s:verify-mutation:reason=clone

	// Relax validation of immutable fields to allow it to be set to 1 if it was previously negative.
	if oldPod.Spec.TerminationGracePeriodSeconds != nil && *oldPod.Spec.TerminationGracePeriodSeconds < 0 &&
		mungedPodSpec.TerminationGracePeriodSeconds != nil && *mungedPodSpec.TerminationGracePeriodSeconds == 1 {
		mungedPodSpec.TerminationGracePeriodSeconds = oldPod.Spec.TerminationGracePeriodSeconds // +k8s:verify-mutation:reason=clone
	}

	// Handle validations specific to gated pods.
	podIsGated := len(oldPod.Spec.SchedulingGates) > 0
	if podIsGated {
		// Additions to spec.nodeSelector are allowed (no deletions or mutations) for gated pods.
		if !apiequality.Semantic.DeepEqual(mungedPodSpec.NodeSelector, oldPod.Spec.NodeSelector) {
			allErrs = append(allErrs, validateNodeSelectorMutation(specPath.Child("nodeSelector"), mungedPodSpec.NodeSelector, oldPod.Spec.NodeSelector)...)
			mungedPodSpec.NodeSelector = oldPod.Spec.NodeSelector // +k8s:verify-mutation:reason=clone
		}

		// Validate node affinity mutations.
		var oldNodeAffinity *core.NodeAffinity
		if oldPod.Spec.Affinity != nil {
			oldNodeAffinity = oldPod.Spec.Affinity.NodeAffinity // +k8s:verify-mutation:reason=clone
		}

		var mungedNodeAffinity *core.NodeAffinity
		if mungedPodSpec.Affinity != nil {
			mungedNodeAffinity = mungedPodSpec.Affinity.NodeAffinity // +k8s:verify-mutation:reason=clone
		}

		if !apiequality.Semantic.DeepEqual(oldNodeAffinity, mungedNodeAffinity) {
			allErrs = append(allErrs, validateNodeAffinityMutation(specPath.Child("affinity").Child("nodeAffinity"), mungedNodeAffinity, oldNodeAffinity)...)
			switch {
			case mungedPodSpec.Affinity == nil && oldNodeAffinity == nil:
				// already effectively nil, no change needed
			case mungedPodSpec.Affinity == nil && oldNodeAffinity != nil:
				mungedPodSpec.Affinity = &core.Affinity{NodeAffinity: oldNodeAffinity} // +k8s:verify-mutation:reason=clone
			case mungedPodSpec.Affinity != nil && oldPod.Spec.Affinity == nil &&
				mungedPodSpec.Affinity.PodAntiAffinity == nil && mungedPodSpec.Affinity.PodAffinity == nil:
				// We ensure no other fields are being changed, but the NodeAffinity. If that's the case, and the
				// old pod's affinity is nil, we set the mungedPodSpec's affinity to nil.
				mungedPodSpec.Affinity = nil // +k8s:verify-mutation:reason=clone
			default:
				// The node affinity is being updated and the old pod Affinity is not nil.
				// We set the mungedPodSpec's node affinity to the old pod's node affinity.
				mungedPodSpec.Affinity.NodeAffinity = oldNodeAffinity // +k8s:verify-mutation:reason=clone
			}
		}

		// Note: Unlike NodeAffinity and NodeSelector, we cannot make PodAffinity/PodAntiAffinity mutable due to the presence of the matchLabelKeys/mismatchLabelKeys feature.
		// Those features automatically generate the matchExpressions in labelSelector for PodAffinity/PodAntiAffinity when the Pod is created.
		// When we make them mutable, we need to make sure things like how to handle/validate matchLabelKeys,
		// and what if the fieldManager/A sets matchexpressions and fieldManager/B sets matchLabelKeys later. (could it lead the understandable conflict, etc)
	}

	if !apiequality.Semantic.DeepEqual(mungedPodSpec, oldPod.Spec) {
		// This diff isn't perfect, but it's a helluva lot better an "I'm not going to tell you what the difference is".
		// TODO: Pinpoint the specific field that causes the invalid error after we have strategic merge diff
		specDiff := cmp.Diff(oldPod.Spec, mungedPodSpec)
		errs := field.Forbidden(specPath, fmt.Sprintf("pod updates may not change fields other than %s\n%v", strings.Join(updatablePodSpecFields, ","), specDiff))
		allErrs = append(allErrs, errs)
	}
	return allErrs
}

// ValidateContainerStateTransition test to if any illegal container state transitions are being attempted
func ValidateContainerStateTransition(newStatuses, oldStatuses []core.ContainerStatus, fldpath *field.Path, restartPolicy core.RestartPolicy) field.ErrorList {
	allErrs := field.ErrorList{}
	// If we should always restart, containers are allowed to leave the terminated state
	if restartPolicy == core.RestartPolicyAlways {
		return allErrs
	}
	for i, oldStatus := range oldStatuses {
		// Skip any container that is not terminated
		if oldStatus.State.Terminated == nil {
			continue
		}
		// Skip any container that failed but is allowed to restart
		if oldStatus.State.Terminated.ExitCode != 0 && restartPolicy == core.RestartPolicyOnFailure {
			continue
		}
		for _, newStatus := range newStatuses {
			if oldStatus.Name == newStatus.Name && newStatus.State.Terminated == nil {
				allErrs = append(allErrs, field.Forbidden(fldpath.Index(i).Child("state"), "may not be transitioned to non-terminated state"))
			}
		}
	}
	return allErrs
}

// ValidateInitContainerStateTransition test to if any illegal init container state transitions are being attempted
func ValidateInitContainerStateTransition(newStatuses, oldStatuses []core.ContainerStatus, fldpath *field.Path, podSpec *core.PodSpec) field.ErrorList {
	allErrs := field.ErrorList{}
	// If we should always restart, containers are allowed to leave the terminated state
	if podSpec.RestartPolicy == core.RestartPolicyAlways {
		return allErrs
	}
	for i, oldStatus := range oldStatuses {
		// Skip any container that is not terminated
		if oldStatus.State.Terminated == nil {
			continue
		}
		// Skip any container that failed but is allowed to restart
		if oldStatus.State.Terminated.ExitCode != 0 && podSpec.RestartPolicy == core.RestartPolicyOnFailure {
			continue
		}

		// Skip any restartable init container that is allowed to restart
		isRestartableInitCtr := false
		for _, c := range podSpec.InitContainers {
			if oldStatus.Name == c.Name {
				if isRestartableInitContainer(&c) {
					isRestartableInitCtr = true
				}
				break
			}
		}
		if isRestartableInitCtr {
			continue
		}

		for _, newStatus := range newStatuses {
			if oldStatus.Name == newStatus.Name && newStatus.State.Terminated == nil {
				allErrs = append(allErrs, field.Forbidden(fldpath.Index(i).Child("state"), "may not be transitioned to non-terminated state"))
			}
		}
	}
	return allErrs
}

// ValidatePodStatusUpdate checks for changes to status that shouldn't occur in normal operation.
func ValidatePodStatusUpdate(newPod, oldPod *core.Pod, opts PodValidationOptions) field.ErrorList {
	fldPath := field.NewPath("metadata")
	allErrs := ValidateObjectMetaUpdate(&newPod.ObjectMeta, &oldPod.ObjectMeta, fldPath)
	allErrs = append(allErrs, ValidatePodSpecificAnnotationUpdates(newPod, oldPod, fldPath.Child("annotations"), opts)...)
	allErrs = append(allErrs, validatePodConditions(newPod.Status.Conditions, fldPath.Child("conditions"))...)

	fldPath = field.NewPath("status")
	if newPod.Spec.NodeName != oldPod.Spec.NodeName {
		allErrs = append(allErrs, field.Forbidden(fldPath.Child("nodeName"), "may not be changed directly"))
	}

	if newPod.Status.NominatedNodeName != oldPod.Status.NominatedNodeName && len(newPod.Status.NominatedNodeName) > 0 {
		for _, msg := range ValidateNodeName(newPod.Status.NominatedNodeName, false) {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("nominatedNodeName"), newPod.Status.NominatedNodeName, msg))
		}
	}

	// Pod QoS is immutable
	allErrs = append(allErrs, ValidateImmutableField(newPod.Status.QOSClass, oldPod.Status.QOSClass, fldPath.Child("qosClass"))...)

	// Note: there is no check that ContainerStatuses, InitContainerStatuses, and EphemeralContainerStatuses doesn't have duplicate conatainer names
	// or statuses of containers that are not defined in the pod spec. Changing this may lead to a breaking changes. So consumers of those fields
	// must account for unexpected data. Kubelet will never report statuses like this.
	//
	// If pod should not restart, make sure the status update does not transition
	// any terminated containers to a non-terminated state.
	allErrs = append(allErrs, ValidateContainerStateTransition(newPod.Status.ContainerStatuses, oldPod.Status.ContainerStatuses, fldPath.Child("containerStatuses"), oldPod.Spec.RestartPolicy)...)
	allErrs = append(allErrs, ValidateInitContainerStateTransition(newPod.Status.InitContainerStatuses, oldPod.Status.InitContainerStatuses, fldPath.Child("initContainerStatuses"), &oldPod.Spec)...)
	// The kubelet will never restart ephemeral containers, so treat them like they have an implicit RestartPolicyNever.
	allErrs = append(allErrs, ValidateContainerStateTransition(newPod.Status.EphemeralContainerStatuses, oldPod.Status.EphemeralContainerStatuses, fldPath.Child("ephemeralContainerStatuses"), core.RestartPolicyNever)...)
	allErrs = append(allErrs, validatePodResourceClaimStatuses(newPod.Status.ResourceClaimStatuses, newPod.Spec.ResourceClaims, fldPath.Child("resourceClaimStatuses"))...)

	if newIPErrs := validatePodIPs(newPod); len(newIPErrs) > 0 {
		allErrs = append(allErrs, newIPErrs...)
	}

	if newIPErrs := validateHostIPs(newPod); len(newIPErrs) > 0 {
		allErrs = append(allErrs, newIPErrs...)
	}

	allErrs = append(allErrs, validateContainerStatusUsers(newPod.Status.ContainerStatuses, fldPath.Child("containerStatuses"), newPod.Spec.OS)...)
	allErrs = append(allErrs, validateContainerStatusUsers(newPod.Status.InitContainerStatuses, fldPath.Child("initContainerStatuses"), newPod.Spec.OS)...)
	allErrs = append(allErrs, validateContainerStatusUsers(newPod.Status.EphemeralContainerStatuses, fldPath.Child("ephemeralContainerStatuses"), newPod.Spec.OS)...)

	allErrs = append(allErrs, validateContainerStatusAllocatedResourcesStatus(newPod.Status.ContainerStatuses, fldPath.Child("containerStatuses"), newPod.Spec.Containers)...)
	allErrs = append(allErrs, validateContainerStatusAllocatedResourcesStatus(newPod.Status.InitContainerStatuses, fldPath.Child("initContainerStatuses"), newPod.Spec.InitContainers)...)
	// ephemeral containers are not allowed to have resources allocated
	allErrs = append(allErrs, validateContainerStatusNoAllocatedResourcesStatus(newPod.Status.EphemeralContainerStatuses, fldPath.Child("ephemeralContainerStatuses"))...)

	return allErrs
}

// validatePodConditions tests if the custom pod conditions are valid.
func validatePodConditions(conditions []core.PodCondition, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	systemConditions := sets.New(
		core.PodScheduled,
		core.PodReady,
		core.PodInitialized)
	for i, condition := range conditions {
		if systemConditions.Has(condition.Type) {
			continue
		}
		allErrs = append(allErrs, ValidateQualifiedName(string(condition.Type), fldPath.Index(i).Child("Type"))...)
	}
	return allErrs
}

// validatePodResourceClaimStatuses validates the ResourceClaimStatuses slice in a pod status.
func validatePodResourceClaimStatuses(statuses []core.PodResourceClaimStatus, podClaims []core.PodResourceClaim, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList

	claimNames := sets.New[string]()
	for i, status := range statuses {
		idxPath := fldPath.Index(i)
		// There's no need to check the content of the name. If it matches an entry,
		// then it is valid, otherwise we reject it here.
		if !havePodClaim(podClaims, status.Name) {
			allErrs = append(allErrs, field.Invalid(idxPath.Child("name"), status.Name, "must match the name of an entry in `spec.resourceClaims`"))
		}
		if claimNames.Has(status.Name) {
			allErrs = append(allErrs, field.Duplicate(idxPath.Child("name"), status.Name))
		} else {
			claimNames.Insert(status.Name)
		}
		if status.ResourceClaimName != nil {
			for _, detail := range ValidateResourceClaimName(*status.ResourceClaimName, false) {
				allErrs = append(allErrs, field.Invalid(idxPath.Child("name"), status.ResourceClaimName, detail))
			}
		}
	}

	return allErrs
}

func havePodClaim(podClaims []core.PodResourceClaim, name string) bool {
	for _, podClaim := range podClaims {
		if podClaim.Name == name {
			return true
		}
	}
	return false
}

// ValidatePodEphemeralContainersUpdate tests that a user update to EphemeralContainers is valid.
// newPod and oldPod must only differ in their EphemeralContainers.
func ValidatePodEphemeralContainersUpdate(newPod, oldPod *core.Pod, opts PodValidationOptions) field.ErrorList {
	// Part 1: Validate newPod's spec and updates to metadata
	fldPath := field.NewPath("metadata")
	allErrs := ValidateObjectMetaUpdate(&newPod.ObjectMeta, &oldPod.ObjectMeta, fldPath)
	allErrs = append(allErrs, validatePodMetadataAndSpec(newPod, opts)...)
	allErrs = append(allErrs, ValidatePodSpecificAnnotationUpdates(newPod, oldPod, fldPath.Child("annotations"), opts)...)

	// static pods don't support ephemeral containers #113935
	if _, ok := oldPod.Annotations[core.MirrorPodAnnotationKey]; ok {
		return field.ErrorList{field.Forbidden(field.NewPath(""), "static pods do not support ephemeral containers")}
	}

	// Part 2: Validate that the changes between oldPod.Spec.EphemeralContainers and
	// newPod.Spec.EphemeralContainers are allowed.
	//
	// Existing EphemeralContainers may not be changed. Order isn't preserved by patch, so check each individually.
	newContainerIndex := make(map[string]*core.EphemeralContainer)
	specPath := field.NewPath("spec").Child("ephemeralContainers")
	for i := range newPod.Spec.EphemeralContainers {
		newContainerIndex[newPod.Spec.EphemeralContainers[i].Name] = &newPod.Spec.EphemeralContainers[i]
	}
	for _, old := range oldPod.Spec.EphemeralContainers {
		if new, ok := newContainerIndex[old.Name]; !ok {
			allErrs = append(allErrs, field.Forbidden(specPath, fmt.Sprintf("existing ephemeral containers %q may not be removed\n", old.Name)))
		} else if !apiequality.Semantic.DeepEqual(old, *new) {
			specDiff := cmp.Diff(old, *new)
			allErrs = append(allErrs, field.Forbidden(specPath, fmt.Sprintf("existing ephemeral containers %q may not be changed\n%v", old.Name, specDiff)))
		}
	}

	return allErrs
}

// ValidatePodResize tests that a user update to pod container resources is valid.
// newPod and oldPod must only differ in their Containers[*].Resources and
// Containers[*].ResizePolicy field.
func ValidatePodResize(newPod, oldPod *core.Pod, opts PodValidationOptions) field.ErrorList {
	// Part 1: Validate newPod's spec and updates to metadata
	fldPath := field.NewPath("metadata")
	allErrs := ValidateImmutableField(&newPod.ObjectMeta, &oldPod.ObjectMeta, fldPath)
	allErrs = append(allErrs, validatePodMetadataAndSpec(newPod, opts)...)

	// pods with pod-level resources cannot be resized
	isPodLevelResourcesSet := func(pod *core.Pod) bool {
		return pod.Spec.Resources != nil &&
			(len(pod.Spec.Resources.Requests)+len(pod.Spec.Resources.Limits) > 0)
	}

	if isPodLevelResourcesSet(oldPod) || isPodLevelResourcesSet(newPod) {
		return field.ErrorList{field.Forbidden(field.NewPath(""), "pods with pod-level resources cannot be resized")}
	}

	// static pods cannot be resized.
	if _, ok := oldPod.Annotations[core.MirrorPodAnnotationKey]; ok {
		return field.ErrorList{field.Forbidden(field.NewPath(""), "static pods cannot be resized")}
	}

	// windows pods are not supported.
	if oldPod.Spec.OS != nil && oldPod.Spec.OS.Name == core.Windows {
		return field.ErrorList{field.Forbidden(field.NewPath(""), "windows pods cannot be resized")}
	}

	// Part 2: Validate that the changes between oldPod.Spec.Containers[].Resources and
	// newPod.Spec.Containers[].Resources are allowed. Also validate that the changes between oldPod.Spec.InitContainers[].Resources and
	// newPod.Spec.InitContainers[].Resources are allowed.
	specPath := field.NewPath("spec")
	if qos.GetPodQOS(oldPod) != qos.ComputePodQOS(newPod) {
		allErrs = append(allErrs, field.Invalid(specPath, newPod.Status.QOSClass, "Pod QOS Class may not change as a result of resizing"))
	}

	if !isPodResizeRequestSupported(*oldPod) {
		allErrs = append(allErrs, field.Forbidden(specPath, "Pod running on node without support for resize"))
	}

	// Do not allow removing resource requests/limits on resize.
	if utilfeature.DefaultFeatureGate.Enabled(features.SidecarContainers) {
		for ix, ctr := range oldPod.Spec.InitContainers {
			if !isRestartableInitContainer(&ctr) {
				continue
			}
			if resourcesRemoved(newPod.Spec.InitContainers[ix].Resources.Requests, ctr.Resources.Requests) {
				allErrs = append(allErrs, field.Forbidden(specPath.Child("initContainers").Index(ix).Child("resources").Child("requests"), "resource requests cannot be removed"))
			}
			if resourcesRemoved(newPod.Spec.InitContainers[ix].Resources.Limits, ctr.Resources.Limits) {
				allErrs = append(allErrs, field.Forbidden(specPath.Child("initContainers").Index(ix).Child("resources").Child("limits"), "resource limits cannot be removed"))
			}
		}
	}
	for ix, ctr := range oldPod.Spec.Containers {
		if resourcesRemoved(newPod.Spec.Containers[ix].Resources.Requests, ctr.Resources.Requests) {
			allErrs = append(allErrs, field.Forbidden(specPath.Child("containers").Index(ix).Child("resources").Child("requests"), "resource requests cannot be removed"))
		}
		if resourcesRemoved(newPod.Spec.Containers[ix].Resources.Limits, ctr.Resources.Limits) {
			allErrs = append(allErrs, field.Forbidden(specPath.Child("containers").Index(ix).Child("resources").Child("limits"), "resource limits cannot be removed"))
		}
	}

	// Ensure that only CPU and memory resources are mutable for regular containers.
	originalCPUMemPodSpec := *newPod.Spec.DeepCopy()
	var newContainers []core.Container
	for ix, container := range originalCPUMemPodSpec.Containers {
		dropCPUMemoryResourcesFromContainer(&container, &oldPod.Spec.Containers[ix])
		if !apiequality.Semantic.DeepEqual(container, oldPod.Spec.Containers[ix]) {
			// This likely means that the user has made changes to resources other than CPU and memory for regular container.
			errs := field.Forbidden(specPath, "only cpu and memory resources are mutable")
			allErrs = append(allErrs, errs)
		}
		newContainers = append(newContainers, container)
	}
	originalCPUMemPodSpec.Containers = newContainers

	// Ensure that only CPU and memory resources are mutable for restartable init containers.
	// Also ensure that resources are immutable for non-restartable init containers.
	var newInitContainers []core.Container
	if utilfeature.DefaultFeatureGate.Enabled(features.SidecarContainers) {
		for ix, container := range originalCPUMemPodSpec.InitContainers {
			if isRestartableInitContainer(&container) { // restartable init container
				dropCPUMemoryResourcesFromContainer(&container, &oldPod.Spec.InitContainers[ix])
				if !apiequality.Semantic.DeepEqual(container, oldPod.Spec.InitContainers[ix]) {
					// This likely means that the user has made changes to resources other than CPU and memory for sidecar container.
					errs := field.Forbidden(specPath, "only cpu and memory resources for sidecar containers are mutable")
					allErrs = append(allErrs, errs)
				}
			} else if !apiequality.Semantic.DeepEqual(container, oldPod.Spec.InitContainers[ix]) { // non-restartable init container
				// This likely means that the user has modified resources of non-sidecar init container.
				errs := field.Forbidden(specPath, "resources for non-sidecar init containers are immutable")
				allErrs = append(allErrs, errs)
			}
			newInitContainers = append(newInitContainers, container)
		}
		originalCPUMemPodSpec.InitContainers = newInitContainers
	}

	if len(allErrs) > 0 {
		return allErrs
	}

	if !apiequality.Semantic.DeepEqual(originalCPUMemPodSpec, oldPod.Spec) {
		// This likely means that the user has made changes to resources other than CPU and Memory.
		errs := field.Forbidden(specPath, "only cpu and memory resources are mutable")
		allErrs = append(allErrs, errs)
	}
	return allErrs
}

// dropCPUMemoryResourcesFromContainer deletes the cpu and memory resources from the container, and copies them from the old pod container resources if present.
func dropCPUMemoryResourcesFromContainer(container *core.Container, oldPodSpecContainer *core.Container) {
	dropCPUMemoryUpdates := func(resourceList, oldResourceList core.ResourceList) core.ResourceList {
		if oldResourceList == nil {
			return nil
		}
		var mungedResourceList core.ResourceList
		if resourceList == nil {
			mungedResourceList = make(core.ResourceList)
		} else {
			mungedResourceList = resourceList.DeepCopy()
		}
		delete(mungedResourceList, core.ResourceCPU)
		delete(mungedResourceList, core.ResourceMemory)
		if cpu, found := oldResourceList[core.ResourceCPU]; found {
			mungedResourceList[core.ResourceCPU] = cpu
		}
		if mem, found := oldResourceList[core.ResourceMemory]; found {
			mungedResourceList[core.ResourceMemory] = mem
		}
		return mungedResourceList
	}
	lim := dropCPUMemoryUpdates(container.Resources.Limits, oldPodSpecContainer.Resources.Limits)
	req := dropCPUMemoryUpdates(container.Resources.Requests, oldPodSpecContainer.Resources.Requests)
	container.Resources = core.ResourceRequirements{Limits: lim, Requests: req}
	container.ResizePolicy = oldPodSpecContainer.ResizePolicy // +k8s:verify-mutation:reason=clone
}

// isPodResizeRequestSupported checks whether the pod is running on a node with InPlacePodVerticalScaling enabled.
func isPodResizeRequestSupported(pod core.Pod) bool {
	// TODO: Remove this after GA+3 releases of InPlacePodVerticalScaling
	// This code handles the version skew as described in the KEP.
	// For handling version skew we're only allowing to update the Pod's Resources
	// if the Pod already has Pod.Status.ContainerStatuses[i].Resources. This means
	// that the apiserver would only allow updates to Pods running on Nodes with
	// the InPlacePodVerticalScaling feature gate enabled.
	for _, c := range pod.Status.ContainerStatuses {
		if c.State.Running != nil {
			return c.Resources != nil
		}
	}
	// No running containers. We cannot tell whether the node supports resize at this point, so we assume it does.
	return true
}

func resourcesRemoved(resourceList, oldResourceList core.ResourceList) bool {
	if len(oldResourceList) > len(resourceList) {
		return true
	}
	for name := range oldResourceList {
		if _, ok := resourceList[name]; !ok {
			return true
		}
	}

	return false
}

// ValidatePodBinding tests if required fields in the pod binding are legal.
func ValidatePodBinding(binding *core.Binding) field.ErrorList {
	allErrs := field.ErrorList{}

	if len(binding.Target.Kind) != 0 && binding.Target.Kind != "Node" {
		// TODO: When validation becomes versioned, this gets more complicated.
		allErrs = append(allErrs, field.NotSupported(field.NewPath("target", "kind"), binding.Target.Kind, []string{"Node", "<empty>"}))
	}
	if len(binding.Target.Name) == 0 {
		// TODO: When validation becomes versioned, this gets more complicated.
		allErrs = append(allErrs, field.Required(field.NewPath("target", "name"), ""))
	}

	return allErrs
}

// ValidatePodTemplate tests if required fields in the pod template are set.
func ValidatePodTemplate(pod *core.PodTemplate, opts PodValidationOptions) field.ErrorList {
	allErrs := ValidateObjectMeta(&pod.ObjectMeta, true, ValidatePodName, field.NewPath("metadata"))
	allErrs = append(allErrs, ValidatePodTemplateSpec(&pod.Template, field.NewPath("template"), opts)...)
	return allErrs
}

// ValidatePodTemplateUpdate tests to see if the update is legal for an end user to make. newPod is updated with fields
// that cannot be changed.
func ValidatePodTemplateUpdate(newPod, oldPod *core.PodTemplate, opts PodValidationOptions) field.ErrorList {
	allErrs := ValidateObjectMetaUpdate(&newPod.ObjectMeta, &oldPod.ObjectMeta, field.NewPath("metadata"))
	allErrs = append(allErrs, ValidatePodTemplateSpec(&newPod.Template, field.NewPath("template"), opts)...)
	return allErrs
}

var supportedSessionAffinityType = sets.New(core.ServiceAffinityClientIP, core.ServiceAffinityNone)
var supportedServiceType = sets.New(core.ServiceTypeClusterIP, core.ServiceTypeNodePort,
	core.ServiceTypeLoadBalancer, core.ServiceTypeExternalName)

var supportedServiceInternalTrafficPolicy = sets.New(core.ServiceInternalTrafficPolicyCluster, core.ServiceInternalTrafficPolicyLocal)

var supportedServiceIPFamily = sets.New(core.IPv4Protocol, core.IPv6Protocol)
var supportedServiceIPFamilyPolicy = sets.New(
	core.IPFamilyPolicySingleStack,
	core.IPFamilyPolicyPreferDualStack,
	core.IPFamilyPolicyRequireDualStack)

// ValidateService tests if required fields/annotations of a Service are valid.
func ValidateService(service *core.Service) field.ErrorList {
	metaPath := field.NewPath("metadata")
	allErrs := ValidateObjectMeta(&service.ObjectMeta, true, ValidateServiceName, metaPath)

	topologyHintsVal, topologyHintsSet := service.Annotations[core.DeprecatedAnnotationTopologyAwareHints]
	topologyModeVal, topologyModeSet := service.Annotations[core.AnnotationTopologyMode]

	if topologyModeSet && topologyHintsSet && topologyModeVal != topologyHintsVal {
		message := fmt.Sprintf("must match annotations[%s] when both are specified", core.DeprecatedAnnotationTopologyAwareHints)
		allErrs = append(allErrs, field.Invalid(metaPath.Child("annotations").Key(core.AnnotationTopologyMode), topologyModeVal, message))
	}

	specPath := field.NewPath("spec")

	if len(service.Spec.Ports) == 0 && !isHeadlessService(service) && service.Spec.Type != core.ServiceTypeExternalName {
		allErrs = append(allErrs, field.Required(specPath.Child("ports"), ""))
	}
	switch service.Spec.Type {
	case core.ServiceTypeLoadBalancer:
		if isHeadlessService(service) {
			allErrs = append(allErrs, field.Invalid(specPath.Child("clusterIPs").Index(0), service.Spec.ClusterIPs[0], "may not be set to 'None' for LoadBalancer services"))
		}
	case core.ServiceTypeNodePort:
		if isHeadlessService(service) {
			allErrs = append(allErrs, field.Invalid(specPath.Child("clusterIPs").Index(0), service.Spec.ClusterIPs[0], "may not be set to 'None' for NodePort services"))
		}
	case core.ServiceTypeExternalName:
		// must have  len(.spec.ClusterIPs) == 0 // note: strategy sets ClusterIPs based on ClusterIP
		if len(service.Spec.ClusterIPs) > 0 {
			allErrs = append(allErrs, field.Forbidden(specPath.Child("clusterIPs"), "may not be set for ExternalName services"))
		}

		// must have nil families and nil policy
		if len(service.Spec.IPFamilies) > 0 {
			allErrs = append(allErrs, field.Forbidden(specPath.Child("ipFamilies"), "may not be set for ExternalName services"))
		}
		if service.Spec.IPFamilyPolicy != nil {
			allErrs = append(allErrs, field.Forbidden(specPath.Child("ipFamilyPolicy"), "may not be set for ExternalName services"))
		}

		// The value (a CNAME) may have a trailing dot to denote it as fully qualified
		cname := strings.TrimSuffix(service.Spec.ExternalName, ".")
		if len(cname) > 0 {
			allErrs = append(allErrs, ValidateDNS1123Subdomain(cname, specPath.Child("externalName"))...)
		} else {
			allErrs = append(allErrs, field.Required(specPath.Child("externalName"), ""))
		}
	}

	allPortNames := sets.Set[string]{}
	portsPath := specPath.Child("ports")
	for i := range service.Spec.Ports {
		portPath := portsPath.Index(i)
		allErrs = append(allErrs, validateServicePort(&service.Spec.Ports[i], len(service.Spec.Ports) > 1, isHeadlessService(service), &allPortNames, portPath)...)
	}

	if service.Spec.Selector != nil {
		allErrs = append(allErrs, unversionedvalidation.ValidateLabels(service.Spec.Selector, specPath.Child("selector"))...)
	}

	if len(service.Spec.SessionAffinity) == 0 {
		allErrs = append(allErrs, field.Required(specPath.Child("sessionAffinity"), ""))
	} else if !supportedSessionAffinityType.Has(service.Spec.SessionAffinity) {
		allErrs = append(allErrs, field.NotSupported(specPath.Child("sessionAffinity"), service.Spec.SessionAffinity, sets.List(supportedSessionAffinityType)))
	}

	if service.Spec.SessionAffinity == core.ServiceAffinityClientIP {
		allErrs = append(allErrs, validateClientIPAffinityConfig(service.Spec.SessionAffinityConfig, specPath.Child("sessionAffinityConfig"))...)
	} else if service.Spec.SessionAffinity == core.ServiceAffinityNone {
		if service.Spec.SessionAffinityConfig != nil {
			allErrs = append(allErrs, field.Forbidden(specPath.Child("sessionAffinityConfig"), fmt.Sprintf("must not be set when session affinity is %s", core.ServiceAffinityNone)))
		}
	}

	// dualstack <-> ClusterIPs <-> ipfamilies
	allErrs = append(allErrs, ValidateServiceClusterIPsRelatedFields(service)...)

	ipPath := specPath.Child("externalIPs")
	for i, ip := range service.Spec.ExternalIPs {
		idxPath := ipPath.Index(i)
		if errs := validation.IsValidIP(idxPath, ip); len(errs) != 0 {
			allErrs = append(allErrs, errs...)
		} else {
			allErrs = append(allErrs, ValidateNonSpecialIP(ip, idxPath)...)
		}
	}

	if len(service.Spec.Type) == 0 {
		allErrs = append(allErrs, field.Required(specPath.Child("type"), ""))
	} else if !supportedServiceType.Has(service.Spec.Type) {
		allErrs = append(allErrs, field.NotSupported(specPath.Child("type"), service.Spec.Type, sets.List(supportedServiceType)))
	}

	if service.Spec.Type == core.ServiceTypeClusterIP {
		portsPath := specPath.Child("ports")
		for i := range service.Spec.Ports {
			portPath := portsPath.Index(i)
			if service.Spec.Ports[i].NodePort != 0 {
				allErrs = append(allErrs, field.Forbidden(portPath.Child("nodePort"), "may not be used when `type` is 'ClusterIP'"))
			}
		}
	}

	// Check for duplicate NodePorts, considering (protocol,port) pairs
	portsPath = specPath.Child("ports")
	nodePorts := make(map[core.ServicePort]bool)
	for i := range service.Spec.Ports {
		port := &service.Spec.Ports[i]
		if port.NodePort == 0 {
			continue
		}
		portPath := portsPath.Index(i)
		var key core.ServicePort
		key.Protocol = port.Protocol
		key.NodePort = port.NodePort
		_, found := nodePorts[key]
		if found {
			allErrs = append(allErrs, field.Duplicate(portPath.Child("nodePort"), port.NodePort))
		}
		nodePorts[key] = true
	}

	// Check for duplicate Ports, considering (protocol,port) pairs
	portsPath = specPath.Child("ports")
	ports := make(map[core.ServicePort]bool)
	for i, port := range service.Spec.Ports {
		portPath := portsPath.Index(i)
		key := core.ServicePort{Protocol: port.Protocol, Port: port.Port}
		_, found := ports[key]
		if found {
			allErrs = append(allErrs, field.Duplicate(portPath, key))
		}
		ports[key] = true
	}

	// Validate SourceRanges field or annotation.
	if len(service.Spec.LoadBalancerSourceRanges) > 0 {
		fieldPath := specPath.Child("LoadBalancerSourceRanges")

		if service.Spec.Type != core.ServiceTypeLoadBalancer {
			allErrs = append(allErrs, field.Forbidden(fieldPath, "may only be used when `type` is 'LoadBalancer'"))
		}
		for idx, value := range service.Spec.LoadBalancerSourceRanges {
			// Note: due to a historical accident around transition from the
			// annotation value, these values are allowed to be space-padded.
			value = strings.TrimSpace(value)
			allErrs = append(allErrs, validation.IsValidCIDR(fieldPath.Index(idx), value)...)
		}
	} else if val, annotationSet := service.Annotations[core.AnnotationLoadBalancerSourceRangesKey]; annotationSet {
		fieldPath := field.NewPath("metadata", "annotations").Key(core.AnnotationLoadBalancerSourceRangesKey)
		if service.Spec.Type != core.ServiceTypeLoadBalancer {
			allErrs = append(allErrs, field.Forbidden(fieldPath, "may only be used when `type` is 'LoadBalancer'"))
		}

		val = strings.TrimSpace(val)
		if val != "" {
			cidrs := strings.Split(val, ",")
			for _, value := range cidrs {
				value = strings.TrimSpace(value)
				allErrs = append(allErrs, validation.IsValidCIDR(fieldPath, value)...)
			}
		}
	}

	if service.Spec.AllocateLoadBalancerNodePorts != nil && service.Spec.Type != core.ServiceTypeLoadBalancer {
		allErrs = append(allErrs, field.Forbidden(specPath.Child("allocateLoadBalancerNodePorts"), "may only be used when `type` is 'LoadBalancer'"))
	}

	if service.Spec.Type == core.ServiceTypeLoadBalancer && service.Spec.AllocateLoadBalancerNodePorts == nil {
		allErrs = append(allErrs, field.Required(field.NewPath("allocateLoadBalancerNodePorts"), ""))
	}

	// validate LoadBalancerClass field
	allErrs = append(allErrs, validateLoadBalancerClassField(nil, service)...)

	// external traffic policy fields
	allErrs = append(allErrs, validateServiceExternalTrafficPolicy(service)...)

	// internal traffic policy field
	allErrs = append(allErrs, validateServiceInternalTrafficFieldsValue(service)...)

	// traffic distribution field
	allErrs = append(allErrs, validateServiceTrafficDistribution(service)...)

	return allErrs
}

func validateServicePort(sp *core.ServicePort, requireName, isHeadlessService bool, allNames *sets.Set[string], fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}

	if requireName && len(sp.Name) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("name"), ""))
	} else if len(sp.Name) != 0 {
		allErrs = append(allErrs, ValidateDNS1123Label(sp.Name, fldPath.Child("name"))...)
		if allNames.Has(sp.Name) {
			allErrs = append(allErrs, field.Duplicate(fldPath.Child("name"), sp.Name))
		} else {
			allNames.Insert(sp.Name)
		}
	}

	for _, msg := range validation.IsValidPortNum(int(sp.Port)) {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("port"), sp.Port, msg))
	}

	if len(sp.Protocol) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("protocol"), ""))
	} else if !supportedPortProtocols.Has(sp.Protocol) {
		allErrs = append(allErrs, field.NotSupported(fldPath.Child("protocol"), sp.Protocol, sets.List(supportedPortProtocols)))
	}

	allErrs = append(allErrs, ValidatePortNumOrName(sp.TargetPort, fldPath.Child("targetPort"))...)

	if sp.AppProtocol != nil {
		allErrs = append(allErrs, ValidateQualifiedName(*sp.AppProtocol, fldPath.Child("appProtocol"))...)
	}

	// in the v1 API, targetPorts on headless services were tolerated.
	// once we have version-specific validation, we can reject this on newer API versions, but until then, we have to tolerate it for compatibility.
	//
	// if isHeadlessService {
	// 	if sp.TargetPort.Type == intstr.String || (sp.TargetPort.Type == intstr.Int && sp.Port != sp.TargetPort.IntValue()) {
	// 		allErrs = append(allErrs, field.Invalid(fldPath.Child("targetPort"), sp.TargetPort, "must be equal to the value of 'port' when clusterIP = None"))
	// 	}
	// }

	return allErrs
}

var validExternalTrafficPolicies = sets.New(core.ServiceExternalTrafficPolicyCluster, core.ServiceExternalTrafficPolicyLocal)

func validateServiceExternalTrafficPolicy(service *core.Service) field.ErrorList {
	allErrs := field.ErrorList{}

	fldPath := field.NewPath("spec")

	if !apiservice.ExternallyAccessible(service) {
		if service.Spec.ExternalTrafficPolicy != "" {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("externalTrafficPolicy"), service.Spec.ExternalTrafficPolicy,
				"may only be set for externally-accessible services"))
		}
	} else {
		if service.Spec.ExternalTrafficPolicy == "" {
			allErrs = append(allErrs, field.Required(fldPath.Child("externalTrafficPolicy"), ""))
		} else if !validExternalTrafficPolicies.Has(service.Spec.ExternalTrafficPolicy) {
			allErrs = append(allErrs, field.NotSupported(fldPath.Child("externalTrafficPolicy"),
				service.Spec.ExternalTrafficPolicy, sets.List(validExternalTrafficPolicies)))
		}
	}

	if !apiservice.NeedsHealthCheck(service) {
		if service.Spec.HealthCheckNodePort != 0 {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("healthCheckNodePort"), service.Spec.HealthCheckNodePort,
				"may only be set when `type` is 'LoadBalancer' and `externalTrafficPolicy` is 'Local'"))
		}
	} else {
		if service.Spec.HealthCheckNodePort == 0 {
			allErrs = append(allErrs, field.Required(fldPath.Child("healthCheckNodePort"), ""))
		} else {
			for _, msg := range validation.IsValidPortNum(int(service.Spec.HealthCheckNodePort)) {
				allErrs = append(allErrs, field.Invalid(fldPath.Child("healthCheckNodePort"), service.Spec.HealthCheckNodePort, msg))
			}
		}
	}

	return allErrs
}

func validateServiceExternalTrafficFieldsUpdate(before, after *core.Service) field.ErrorList {
	allErrs := field.ErrorList{}

	if apiservice.NeedsHealthCheck(before) && apiservice.NeedsHealthCheck(after) {
		if after.Spec.HealthCheckNodePort != before.Spec.HealthCheckNodePort {
			allErrs = append(allErrs, field.Forbidden(field.NewPath("spec", "healthCheckNodePort"), "field is immutable"))
		}
	}

	return allErrs
}

// validateServiceInternalTrafficFieldsValue validates InternalTraffic related
// spec have legal value.
func validateServiceInternalTrafficFieldsValue(service *core.Service) field.ErrorList {
	allErrs := field.ErrorList{}

	if service.Spec.InternalTrafficPolicy == nil {
		// We do not forbid internalTrafficPolicy on other Service types because of historical reasons.
		// We did not check that before it went beta and we don't want to invalidate existing stored objects.
		if service.Spec.Type == core.ServiceTypeNodePort ||
			service.Spec.Type == core.ServiceTypeLoadBalancer || service.Spec.Type == core.ServiceTypeClusterIP {
			allErrs = append(allErrs, field.Required(field.NewPath("spec").Child("internalTrafficPolicy"), ""))
		}
	}

	if service.Spec.InternalTrafficPolicy != nil && !supportedServiceInternalTrafficPolicy.Has(*service.Spec.InternalTrafficPolicy) {
		allErrs = append(allErrs, field.NotSupported(field.NewPath("spec").Child("internalTrafficPolicy"), *service.Spec.InternalTrafficPolicy, sets.List(supportedServiceInternalTrafficPolicy)))
	}

	return allErrs
}

// validateServiceTrafficDistribution validates the values for the
// trafficDistribution field.
func validateServiceTrafficDistribution(service *core.Service) field.ErrorList {
	allErrs := field.ErrorList{}

	if service.Spec.TrafficDistribution == nil {
		return allErrs
	}

	if *service.Spec.TrafficDistribution != v1.ServiceTrafficDistributionPreferClose {
		allErrs = append(allErrs, field.NotSupported(field.NewPath("spec").Child("trafficDistribution"), *service.Spec.TrafficDistribution, []string{v1.ServiceTrafficDistributionPreferClose}))
	}

	return allErrs
}

// ValidateServiceCreate validates Services as they are created.
func ValidateServiceCreate(service *core.Service) field.ErrorList {
	return ValidateService(service)
}

// ValidateServiceUpdate tests if required fields in the service are set during an update
func ValidateServiceUpdate(service, oldService *core.Service) field.ErrorList {
	allErrs := ValidateObjectMetaUpdate(&service.ObjectMeta, &oldService.ObjectMeta, field.NewPath("metadata"))

	// User can upgrade (add another clusterIP or ipFamily)
	//      can downgrade (remove secondary clusterIP or ipFamily)
	// but *CAN NOT* change primary/secondary clusterIP || ipFamily *UNLESS*
	// they are changing from/to/ON ExternalName

	upgradeDowngradeClusterIPsErrs := validateUpgradeDowngradeClusterIPs(oldService, service)
	allErrs = append(allErrs, upgradeDowngradeClusterIPsErrs...)

	upgradeDowngradeIPFamiliesErrs := validateUpgradeDowngradeIPFamilies(oldService, service)
	allErrs = append(allErrs, upgradeDowngradeIPFamiliesErrs...)

	upgradeDowngradeLoadBalancerClassErrs := validateLoadBalancerClassField(oldService, service)
	allErrs = append(allErrs, upgradeDowngradeLoadBalancerClassErrs...)

	allErrs = append(allErrs, validateServiceExternalTrafficFieldsUpdate(oldService, service)...)

	return append(allErrs, ValidateService(service)...)
}

// ValidateServiceStatusUpdate tests if required fields in the Service are set when updating status.
func ValidateServiceStatusUpdate(service, oldService *core.Service) field.ErrorList {
	allErrs := ValidateObjectMetaUpdate(&service.ObjectMeta, &oldService.ObjectMeta, field.NewPath("metadata"))
	allErrs = append(allErrs, ValidateLoadBalancerStatus(&service.Status.LoadBalancer, field.NewPath("status", "loadBalancer"), &service.Spec)...)
	return allErrs
}

// ValidateReplicationController tests if required fields in the replication controller are set.
func ValidateReplicationController(controller *core.ReplicationController, opts PodValidationOptions) field.ErrorList {
	allErrs := ValidateObjectMeta(&controller.ObjectMeta, true, ValidateReplicationControllerName, field.NewPath("metadata"))
	allErrs = append(allErrs, ValidateReplicationControllerSpec(&controller.Spec, nil, field.NewPath("spec"), opts)...)
	return allErrs
}

// ValidateReplicationControllerUpdate tests if required fields in the replication controller are set.
func ValidateReplicationControllerUpdate(controller, oldController *core.ReplicationController, opts PodValidationOptions) field.ErrorList {
	allErrs := ValidateObjectMetaUpdate(&controller.ObjectMeta, &oldController.ObjectMeta, field.NewPath("metadata"))
	allErrs = append(allErrs, ValidateReplicationControllerSpec(&controller.Spec, &oldController.Spec, field.NewPath("spec"), opts)...)
	return allErrs
}

// ValidateReplicationControllerStatusUpdate tests if required fields in the replication controller are set.
func ValidateReplicationControllerStatusUpdate(controller, oldController *core.ReplicationController) field.ErrorList {
	allErrs := ValidateObjectMetaUpdate(&controller.ObjectMeta, &oldController.ObjectMeta, field.NewPath("metadata"))
	allErrs = append(allErrs, ValidateReplicationControllerStatus(controller.Status, field.NewPath("status"))...)
	return allErrs
}

func ValidateReplicationControllerStatus(status core.ReplicationControllerStatus, statusPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	allErrs = append(allErrs, ValidateNonnegativeField(int64(status.Replicas), statusPath.Child("replicas"))...)
	allErrs = append(allErrs, ValidateNonnegativeField(int64(status.FullyLabeledReplicas), statusPath.Child("fullyLabeledReplicas"))...)
	allErrs = append(allErrs, ValidateNonnegativeField(int64(status.ReadyReplicas), statusPath.Child("readyReplicas"))...)
	allErrs = append(allErrs, ValidateNonnegativeField(int64(status.AvailableReplicas), statusPath.Child("availableReplicas"))...)
	allErrs = append(allErrs, ValidateNonnegativeField(int64(status.ObservedGeneration), statusPath.Child("observedGeneration"))...)
	msg := "cannot be greater than status.replicas"
	if status.FullyLabeledReplicas > status.Replicas {
		allErrs = append(allErrs, field.Invalid(statusPath.Child("fullyLabeledReplicas"), status.FullyLabeledReplicas, msg))
	}
	if status.ReadyReplicas > status.Replicas {
		allErrs = append(allErrs, field.Invalid(statusPath.Child("readyReplicas"), status.ReadyReplicas, msg))
	}
	if status.AvailableReplicas > status.Replicas {
		allErrs = append(allErrs, field.Invalid(statusPath.Child("availableReplicas"), status.AvailableReplicas, msg))
	}
	if status.AvailableReplicas > status.ReadyReplicas {
		allErrs = append(allErrs, field.Invalid(statusPath.Child("availableReplicas"), status.AvailableReplicas, "cannot be greater than readyReplicas"))
	}
	return allErrs
}

// Validates that the given selector is non-empty.
func ValidateNonEmptySelector(selectorMap map[string]string, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	selector := labels.Set(selectorMap).AsSelector()
	if selector.Empty() {
		allErrs = append(allErrs, field.Required(fldPath, ""))
	}
	return allErrs
}

// Validates the given template and ensures that it is in accordance with the desired selector and replicas.
func ValidatePodTemplateSpecForRC(template *core.PodTemplateSpec, selectorMap map[string]string, replicas int32, fldPath *field.Path, opts PodValidationOptions) field.ErrorList {
	allErrs := field.ErrorList{}
	if template == nil {
		allErrs = append(allErrs, field.Required(fldPath, ""))
	} else {
		selector := labels.Set(selectorMap).AsSelector()
		if !selector.Empty() {
			// Verify that the RC selector matches the labels in template.
			labels := labels.Set(template.Labels)
			if !selector.Matches(labels) {
				allErrs = append(allErrs, field.Invalid(fldPath.Child("metadata", "labels"), template.Labels, "`selector` does not match template `labels`"))
			}
		}
		allErrs = append(allErrs, ValidatePodTemplateSpec(template, fldPath, opts)...)
		// RestartPolicy has already been first-order validated as per ValidatePodTemplateSpec().
		if template.Spec.RestartPolicy != core.RestartPolicyAlways {
			allErrs = append(allErrs, field.NotSupported(fldPath.Child("spec", "restartPolicy"), template.Spec.RestartPolicy, []core.RestartPolicy{core.RestartPolicyAlways}))
		}
		if template.Spec.ActiveDeadlineSeconds != nil {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("spec", "activeDeadlineSeconds"), "activeDeadlineSeconds in ReplicationController is not Supported"))
		}
	}
	return allErrs
}

// ValidateReplicationControllerSpec tests if required fields in the replication controller spec are set.
func ValidateReplicationControllerSpec(spec, oldSpec *core.ReplicationControllerSpec, fldPath *field.Path, opts PodValidationOptions) field.ErrorList {
	allErrs := field.ErrorList{}
	allErrs = append(allErrs, ValidateNonnegativeField(int64(spec.MinReadySeconds), fldPath.Child("minReadySeconds"))...)
	allErrs = append(allErrs, ValidateNonEmptySelector(spec.Selector, fldPath.Child("selector"))...)
	allErrs = append(allErrs, ValidateNonnegativeField(int64(spec.Replicas), fldPath.Child("replicas"))...)
	allErrs = append(allErrs, ValidatePodTemplateSpecForRC(spec.Template, spec.Selector, spec.Replicas, fldPath.Child("template"), opts)...)
	return allErrs
}

// ValidatePodTemplateSpec validates the spec of a pod template
func ValidatePodTemplateSpec(spec *core.PodTemplateSpec, fldPath *field.Path, opts PodValidationOptions) field.ErrorList {
	allErrs := field.ErrorList{}
	allErrs = append(allErrs, unversionedvalidation.ValidateLabels(spec.Labels, fldPath.Child("labels"))...)
	allErrs = append(allErrs, ValidateAnnotations(spec.Annotations, fldPath.Child("annotations"))...)
	allErrs = append(allErrs, ValidatePodSpecificAnnotations(spec.Annotations, &spec.Spec, fldPath.Child("annotations"), opts)...)
	allErrs = append(allErrs, ValidatePodSpec(&spec.Spec, nil, fldPath.Child("spec"), opts)...)
	allErrs = append(allErrs, validateSeccompAnnotationsAndFields(spec.ObjectMeta, &spec.Spec, fldPath.Child("spec"))...)
	allErrs = append(allErrs, validateAppArmorAnnotationsAndFieldsMatchOnCreate(spec.ObjectMeta, &spec.Spec, fldPath.Child("spec"))...)

	if len(spec.Spec.EphemeralContainers) > 0 {
		allErrs = append(allErrs, field.Forbidden(fldPath.Child("spec", "ephemeralContainers"), "ephemeral containers not allowed in pod template"))
	}

	return allErrs
}

// ValidateTaintsInNodeAnnotations tests that the serialized taints in Node.Annotations has valid data
func ValidateTaintsInNodeAnnotations(annotations map[string]string, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}

	taints, err := helper.GetTaintsFromNodeAnnotations(annotations)
	if err != nil {
		allErrs = append(allErrs, field.Invalid(fldPath, core.TaintsAnnotationKey, err.Error()))
		return allErrs
	}

	if len(taints) > 0 {
		allErrs = append(allErrs, validateNodeTaints(taints, fldPath.Child(core.TaintsAnnotationKey))...)
	}

	return allErrs
}

// validateNodeTaints tests if given taints have valid data.
func validateNodeTaints(taints []core.Taint, fldPath *field.Path) field.ErrorList {
	allErrors := field.ErrorList{}

	uniqueTaints := map[core.TaintEffect]sets.Set[string]{}

	for i, currTaint := range taints {
		idxPath := fldPath.Index(i)
		// validate the taint key
		allErrors = append(allErrors, unversionedvalidation.ValidateLabelName(currTaint.Key, idxPath.Child("key"))...)
		// validate the taint value
		if errs := validation.IsValidLabelValue(currTaint.Value); len(errs) != 0 {
			allErrors = append(allErrors, field.Invalid(idxPath.Child("value"), currTaint.Value, strings.Join(errs, ";")))
		}
		// validate the taint effect
		allErrors = append(allErrors, validateTaintEffect(&currTaint.Effect, false, idxPath.Child("effect"))...)

		// validate if taint is unique by <key, effect>
		if len(uniqueTaints[currTaint.Effect]) > 0 && uniqueTaints[currTaint.Effect].Has(currTaint.Key) {
			duplicatedError := field.Duplicate(idxPath, currTaint)
			duplicatedError.Detail = "taints must be unique by key and effect pair"
			allErrors = append(allErrors, duplicatedError)
			continue
		}

		// add taint to existingTaints for uniqueness check
		if len(uniqueTaints[currTaint.Effect]) == 0 {
			uniqueTaints[currTaint.Effect] = sets.Set[string]{}
		}
		uniqueTaints[currTaint.Effect].Insert(currTaint.Key)
	}
	return allErrors
}

func ValidateNodeSpecificAnnotations(annotations map[string]string, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}

	if annotations[core.TaintsAnnotationKey] != "" {
		allErrs = append(allErrs, ValidateTaintsInNodeAnnotations(annotations, fldPath)...)
	}

	if annotations[core.PreferAvoidPodsAnnotationKey] != "" {
		allErrs = append(allErrs, ValidateAvoidPodsInNodeAnnotations(annotations, fldPath)...)
	}
	return allErrs
}

// ValidateNode tests if required fields in the node are set.
func ValidateNode(node *core.Node) field.ErrorList {
	fldPath := field.NewPath("metadata")
	allErrs := ValidateObjectMeta(&node.ObjectMeta, false, ValidateNodeName, fldPath)
	allErrs = append(allErrs, ValidateNodeSpecificAnnotations(node.ObjectMeta.Annotations, fldPath.Child("annotations"))...)
	if len(node.Spec.Taints) > 0 {
		allErrs = append(allErrs, validateNodeTaints(node.Spec.Taints, fldPath.Child("taints"))...)
	}

	// Only validate spec.
	// All status fields are optional and can be updated later.
	// That said, if specified, we need to ensure they are valid.
	allErrs = append(allErrs, ValidateNodeResources(node)...)

	// validate PodCIDRS only if we need to
	if len(node.Spec.PodCIDRs) > 0 {
		podCIDRsField := field.NewPath("spec", "podCIDRs")

		// all PodCIDRs should be valid ones
		for idx, value := range node.Spec.PodCIDRs {
			allErrs = append(allErrs, validation.IsValidCIDR(podCIDRsField.Index(idx), value)...)
		}

		// if more than PodCIDR then
		// - validate for dual stack
		// - validate for duplication
		if len(node.Spec.PodCIDRs) > 1 {
			dualStack, err := netutils.IsDualStackCIDRStrings(node.Spec.PodCIDRs)
			if err != nil {
				allErrs = append(allErrs, field.InternalError(podCIDRsField, fmt.Errorf("invalid PodCIDRs. failed to check with dual stack with error:%v", err)))
			}
			if !dualStack || len(node.Spec.PodCIDRs) > 2 {
				allErrs = append(allErrs, field.Invalid(podCIDRsField, node.Spec.PodCIDRs, "may specify no more than one CIDR for each IP family"))
			}

			// PodCIDRs must not contain duplicates
			seen := sets.Set[string]{}
			for i, value := range node.Spec.PodCIDRs {
				if seen.Has(value) {
					allErrs = append(allErrs, field.Duplicate(podCIDRsField.Index(i), value))
				}
				seen.Insert(value)
			}
		}
	}

	return allErrs
}

// ValidateNodeResources is used to make sure a node has valid capacity and allocatable values.
func ValidateNodeResources(node *core.Node) field.ErrorList {
	allErrs := field.ErrorList{}

	// Validate resource quantities in capacity.
	for k, v := range node.Status.Capacity {
		resPath := field.NewPath("status", "capacity", string(k))
		allErrs = append(allErrs, ValidateResourceQuantityValue(k, v, resPath)...)
	}

	// Validate resource quantities in allocatable.
	for k, v := range node.Status.Allocatable {
		resPath := field.NewPath("status", "allocatable", string(k))
		allErrs = append(allErrs, ValidateResourceQuantityValue(k, v, resPath)...)
	}
	return allErrs
}

// ValidateNodeUpdate tests to make sure a node update can be applied.  Modifies oldNode.
func ValidateNodeUpdate(node, oldNode *core.Node) field.ErrorList {
	fldPath := field.NewPath("metadata")
	allErrs := ValidateObjectMetaUpdate(&node.ObjectMeta, &oldNode.ObjectMeta, fldPath)
	allErrs = append(allErrs, ValidateNodeSpecificAnnotations(node.ObjectMeta.Annotations, fldPath.Child("annotations"))...)

	// TODO: Enable the code once we have better core object.status update model. Currently,
	// anyone can update node status.
	// if !apiequality.Semantic.DeepEqual(node.Status, core.NodeStatus{}) {
	// 	allErrs = append(allErrs, field.Invalid("status", node.Status, "must be empty"))
	// }

	allErrs = append(allErrs, ValidateNodeResources(node)...)

	// Validate no duplicate addresses in node status.
	addresses := make(map[core.NodeAddress]bool)
	for i, address := range node.Status.Addresses {
		if _, ok := addresses[address]; ok {
			allErrs = append(allErrs, field.Duplicate(field.NewPath("status", "addresses").Index(i), address))
		}
		addresses[address] = true
	}

	// Allow the controller manager to assign a CIDR to a node if it doesn't have one.
	if len(oldNode.Spec.PodCIDRs) > 0 {
		// compare the entire slice
		if len(oldNode.Spec.PodCIDRs) != len(node.Spec.PodCIDRs) {
			allErrs = append(allErrs, field.Forbidden(field.NewPath("spec", "podCIDRs"), "node updates may not change podCIDR except from \"\" to valid"))
		} else {
			for idx, value := range oldNode.Spec.PodCIDRs {
				if value != node.Spec.PodCIDRs[idx] {
					allErrs = append(allErrs, field.Forbidden(field.NewPath("spec", "podCIDRs"), "node updates may not change podCIDR except from \"\" to valid"))
				}
			}
		}
	}

	// Allow controller manager updating provider ID when not set
	if len(oldNode.Spec.ProviderID) > 0 && oldNode.Spec.ProviderID != node.Spec.ProviderID {
		allErrs = append(allErrs, field.Forbidden(field.NewPath("spec", "providerID"), "node updates may not change providerID except from \"\" to valid"))
	}

	if node.Spec.ConfigSource != nil {
		allErrs = append(allErrs, validateNodeConfigSourceSpec(node.Spec.ConfigSource, field.NewPath("spec", "configSource"))...)
	}
	if node.Status.Config != nil {
		allErrs = append(allErrs, validateNodeConfigStatus(node.Status.Config, field.NewPath("status", "config"))...)
	}

	// update taints
	if len(node.Spec.Taints) > 0 {
		allErrs = append(allErrs, validateNodeTaints(node.Spec.Taints, fldPath.Child("taints"))...)
	}

	if node.Spec.DoNotUseExternalID != oldNode.Spec.DoNotUseExternalID {
		allErrs = append(allErrs, field.Forbidden(field.NewPath("spec", "externalID"), "may not be updated"))
	}

	// status and metadata are allowed change (barring restrictions above), so separately test spec field.
	// spec only has a few fields, so check the ones we don't allow changing
	//  1. PodCIDRs - immutable after first set - checked above
	//  2. ProviderID - immutable after first set - checked above
	//  3. Unschedulable - allowed to change
	//  4. Taints - allowed to change
	//  5. ConfigSource - allowed to change (and checked above)
	//  6. DoNotUseExternalID - immutable - checked above

	return allErrs
}

// validation specific to Node.Spec.ConfigSource
// The field ConfigSource is deprecated and will not be used. The validation is kept in place
// for the backward compatibility
func validateNodeConfigSourceSpec(source *core.NodeConfigSource, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	count := int(0)
	if source.ConfigMap != nil {
		count++
		allErrs = append(allErrs, validateConfigMapNodeConfigSourceSpec(source.ConfigMap, fldPath.Child("configMap"))...)
	}
	// add more subfields here in the future as they are added to NodeConfigSource

	// exactly one reference subfield must be non-nil
	if count != 1 {
		allErrs = append(allErrs, field.Invalid(fldPath, source, "exactly one reference subfield must be non-nil"))
	}
	return allErrs
}

// validation specific to Node.Spec.ConfigSource.ConfigMap
// The field ConfigSource is deprecated and will not be used. The validation is kept in place
// for the backward compatibility
func validateConfigMapNodeConfigSourceSpec(source *core.ConfigMapNodeConfigSource, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	// uid and resourceVersion must not be set in spec
	if string(source.UID) != "" {
		allErrs = append(allErrs, field.Forbidden(fldPath.Child("uid"), "uid must not be set in spec"))
	}
	if source.ResourceVersion != "" {
		allErrs = append(allErrs, field.Forbidden(fldPath.Child("resourceVersion"), "resourceVersion must not be set in spec"))
	}
	return append(allErrs, validateConfigMapNodeConfigSource(source, fldPath)...)
}

// validation specififc to Node.Status.Config
func validateNodeConfigStatus(status *core.NodeConfigStatus, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if status.Assigned != nil {
		allErrs = append(allErrs, validateNodeConfigSourceStatus(status.Assigned, fldPath.Child("assigned"))...)
	}
	if status.Active != nil {
		allErrs = append(allErrs, validateNodeConfigSourceStatus(status.Active, fldPath.Child("active"))...)
	}
	if status.LastKnownGood != nil {
		allErrs = append(allErrs, validateNodeConfigSourceStatus(status.LastKnownGood, fldPath.Child("lastKnownGood"))...)
	}
	return allErrs
}

// validation specific to Node.Status.Config.(Active|Assigned|LastKnownGood)
func validateNodeConfigSourceStatus(source *core.NodeConfigSource, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	count := int(0)
	if source.ConfigMap != nil {
		count++
		allErrs = append(allErrs, validateConfigMapNodeConfigSourceStatus(source.ConfigMap, fldPath.Child("configMap"))...)
	}
	// add more subfields here in the future as they are added to NodeConfigSource

	// exactly one reference subfield must be non-nil
	if count != 1 {
		allErrs = append(allErrs, field.Invalid(fldPath, source, "exactly one reference subfield must be non-nil"))
	}
	return allErrs
}

// validation specific to Node.Status.Config.(Active|Assigned|LastKnownGood).ConfigMap
func validateConfigMapNodeConfigSourceStatus(source *core.ConfigMapNodeConfigSource, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	// uid and resourceVersion must be set in status
	if string(source.UID) == "" {
		allErrs = append(allErrs, field.Required(fldPath.Child("uid"), "uid must be set in status"))
	}
	if source.ResourceVersion == "" {
		allErrs = append(allErrs, field.Required(fldPath.Child("resourceVersion"), "resourceVersion must be set in status"))
	}
	return append(allErrs, validateConfigMapNodeConfigSource(source, fldPath)...)
}

// common validation
func validateConfigMapNodeConfigSource(source *core.ConfigMapNodeConfigSource, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	// validate target configmap namespace
	if source.Namespace == "" {
		allErrs = append(allErrs, field.Required(fldPath.Child("namespace"), "namespace must be set"))
	} else {
		for _, msg := range ValidateNameFunc(ValidateNamespaceName)(source.Namespace, false) {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("namespace"), source.Namespace, msg))
		}
	}
	// validate target configmap name
	if source.Name == "" {
		allErrs = append(allErrs, field.Required(fldPath.Child("name"), "name must be set"))
	} else {
		for _, msg := range ValidateNameFunc(ValidateConfigMapName)(source.Name, false) {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("name"), source.Name, msg))
		}
	}
	// validate kubeletConfigKey against rules for configMap key names
	if source.KubeletConfigKey == "" {
		allErrs = append(allErrs, field.Required(fldPath.Child("kubeletConfigKey"), "kubeletConfigKey must be set"))
	} else {
		for _, msg := range validation.IsConfigMapKey(source.KubeletConfigKey) {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("kubeletConfigKey"), source.KubeletConfigKey, msg))
		}
	}
	return allErrs
}

// Validate compute resource typename.
// Refer to docs/design/resources.md for more details.
func validateResourceName(value core.ResourceName, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	for _, msg := range validation.IsQualifiedName(string(value)) {
		allErrs = append(allErrs, field.Invalid(fldPath, value, msg))
	}
	if len(allErrs) != 0 {
		return allErrs
	}

	if len(strings.Split(string(value), "/")) == 1 {
		if !helper.IsStandardResourceName(value) {
			return append(allErrs, field.Invalid(fldPath, value, "must be a standard resource type or fully qualified"))
		}
	}

	return allErrs
}

// Validate container resource name
// Refer to docs/design/resources.md for more details.
func validateContainerResourceName(value core.ResourceName, fldPath *field.Path) field.ErrorList {
	allErrs := validateResourceName(value, fldPath)

	if len(strings.Split(string(value), "/")) == 1 {
		if !helper.IsStandardContainerResourceName(value) {
			return append(allErrs, field.Invalid(fldPath, value, "must be a standard resource for containers"))
		}
	} else if !helper.IsNativeResource(value) {
		if !helper.IsExtendedResourceName(value) {
			return append(allErrs, field.Invalid(fldPath, value, "doesn't follow extended resource name standard"))
		}
	}
	return allErrs
}

// validatePodResourceName verifies that:
// 1. The resource name is a valid compute resource name for pod-level specification.
// 2. The resource is supported by the PodLevelResources feature.
func validatePodResourceName(resourceName core.ResourceName, fldPath *field.Path) field.ErrorList {
	allErrs := validateResourceName(resourceName, fldPath)
	if len(allErrs) != 0 {
		return allErrs
	}

	if !resourcehelper.IsSupportedPodLevelResource(v1.ResourceName(resourceName)) {
		return append(allErrs, field.NotSupported(fldPath, resourceName, sets.List(resourcehelper.SupportedPodLevelResources())))
	}

	return allErrs
}

// Validate resource names that can go in a resource quota
// Refer to docs/design/resources.md for more details.
func ValidateResourceQuotaResourceName(value core.ResourceName, fldPath *field.Path) field.ErrorList {
	allErrs := validateResourceName(value, fldPath)

	if len(strings.Split(string(value), "/")) == 1 {
		if !helper.IsStandardQuotaResourceName(value) {
			return append(allErrs, field.Invalid(fldPath, value, isInvalidQuotaResource))
		}
	}
	return allErrs
}

// Validate limit range types
func validateLimitRangeTypeName(value core.LimitType, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	for _, msg := range validation.IsQualifiedName(string(value)) {
		allErrs = append(allErrs, field.Invalid(fldPath, value, msg))
	}
	if len(allErrs) != 0 {
		return allErrs
	}

	if len(strings.Split(string(value), "/")) == 1 {
		if !helper.IsStandardLimitRangeType(value) {
			return append(allErrs, field.Invalid(fldPath, value, "must be a standard limit type or fully qualified"))
		}
	}

	return allErrs
}

// Validate limit range resource name
// limit types (other than Pod/Container) could contain storage not just cpu or memory
func validateLimitRangeResourceName(limitType core.LimitType, value core.ResourceName, fldPath *field.Path) field.ErrorList {
	switch limitType {
	case core.LimitTypePod, core.LimitTypeContainer:
		return validateContainerResourceName(value, fldPath)
	default:
		return validateResourceName(value, fldPath)
	}
}

// ValidateLimitRange tests if required fields in the LimitRange are set.
func ValidateLimitRange(limitRange *core.LimitRange) field.ErrorList {
	allErrs := ValidateObjectMeta(&limitRange.ObjectMeta, true, ValidateLimitRangeName, field.NewPath("metadata"))

	// ensure resource names are properly qualified per docs/design/resources.md
	limitTypeSet := map[core.LimitType]bool{}
	fldPath := field.NewPath("spec", "limits")
	for i := range limitRange.Spec.Limits {
		idxPath := fldPath.Index(i)
		limit := &limitRange.Spec.Limits[i]
		allErrs = append(allErrs, validateLimitRangeTypeName(limit.Type, idxPath.Child("type"))...)

		_, found := limitTypeSet[limit.Type]
		if found {
			allErrs = append(allErrs, field.Duplicate(idxPath.Child("type"), limit.Type))
		}
		limitTypeSet[limit.Type] = true

		keys := sets.Set[string]{}
		min := map[string]resource.Quantity{}
		max := map[string]resource.Quantity{}
		defaults := map[string]resource.Quantity{}
		defaultRequests := map[string]resource.Quantity{}
		maxLimitRequestRatios := map[string]resource.Quantity{}

		for k, q := range limit.Max {
			allErrs = append(allErrs, validateLimitRangeResourceName(limit.Type, k, idxPath.Child("max").Key(string(k)))...)
			keys.Insert(string(k))
			max[string(k)] = q
		}
		for k, q := range limit.Min {
			allErrs = append(allErrs, validateLimitRangeResourceName(limit.Type, k, idxPath.Child("min").Key(string(k)))...)
			keys.Insert(string(k))
			min[string(k)] = q
		}

		if limit.Type == core.LimitTypePod {
			if len(limit.Default) > 0 {
				allErrs = append(allErrs, field.Forbidden(idxPath.Child("default"), "may not be specified when `type` is 'Pod'"))
			}
			if len(limit.DefaultRequest) > 0 {
				allErrs = append(allErrs, field.Forbidden(idxPath.Child("defaultRequest"), "may not be specified when `type` is 'Pod'"))
			}
		} else {
			for k, q := range limit.Default {
				allErrs = append(allErrs, validateLimitRangeResourceName(limit.Type, k, idxPath.Child("default").Key(string(k)))...)
				keys.Insert(string(k))
				defaults[string(k)] = q
			}
			for k, q := range limit.DefaultRequest {
				allErrs = append(allErrs, validateLimitRangeResourceName(limit.Type, k, idxPath.Child("defaultRequest").Key(string(k)))...)
				keys.Insert(string(k))
				defaultRequests[string(k)] = q
			}
		}

		if limit.Type == core.LimitTypePersistentVolumeClaim {
			_, minQuantityFound := limit.Min[core.ResourceStorage]
			_, maxQuantityFound := limit.Max[core.ResourceStorage]
			if !minQuantityFound && !maxQuantityFound {
				allErrs = append(allErrs, field.Required(idxPath.Child("limits"), "either minimum or maximum storage value is required, but neither was provided"))
			}
		}

		for k, q := range limit.MaxLimitRequestRatio {
			allErrs = append(allErrs, validateLimitRangeResourceName(limit.Type, k, idxPath.Child("maxLimitRequestRatio").Key(string(k)))...)
			keys.Insert(string(k))
			maxLimitRequestRatios[string(k)] = q
		}

		for k := range keys {
			minQuantity, minQuantityFound := min[k]
			maxQuantity, maxQuantityFound := max[k]
			defaultQuantity, defaultQuantityFound := defaults[k]
			defaultRequestQuantity, defaultRequestQuantityFound := defaultRequests[k]
			maxRatio, maxRatioFound := maxLimitRequestRatios[k]

			if minQuantityFound && maxQuantityFound && minQuantity.Cmp(maxQuantity) > 0 {
				allErrs = append(allErrs, field.Invalid(idxPath.Child("min").Key(string(k)), minQuantity, fmt.Sprintf("min value %s is greater than max value %s", minQuantity.String(), maxQuantity.String())))
			}

			if defaultRequestQuantityFound && minQuantityFound && minQuantity.Cmp(defaultRequestQuantity) > 0 {
				allErrs = append(allErrs, field.Invalid(idxPath.Child("defaultRequest").Key(string(k)), defaultRequestQuantity, fmt.Sprintf("min value %s is greater than default request value %s", minQuantity.String(), defaultRequestQuantity.String())))
			}

			if defaultRequestQuantityFound && maxQuantityFound && defaultRequestQuantity.Cmp(maxQuantity) > 0 {
				allErrs = append(allErrs, field.Invalid(idxPath.Child("defaultRequest").Key(string(k)), defaultRequestQuantity, fmt.Sprintf("default request value %s is greater than max value %s", defaultRequestQuantity.String(), maxQuantity.String())))
			}

			if defaultRequestQuantityFound && defaultQuantityFound && defaultRequestQuantity.Cmp(defaultQuantity) > 0 {
				allErrs = append(allErrs, field.Invalid(idxPath.Child("defaultRequest").Key(string(k)), defaultRequestQuantity, fmt.Sprintf("default request value %s is greater than default limit value %s", defaultRequestQuantity.String(), defaultQuantity.String())))
			}

			if defaultQuantityFound && minQuantityFound && minQuantity.Cmp(defaultQuantity) > 0 {
				allErrs = append(allErrs, field.Invalid(idxPath.Child("default").Key(string(k)), minQuantity, fmt.Sprintf("min value %s is greater than default value %s", minQuantity.String(), defaultQuantity.String())))
			}

			if defaultQuantityFound && maxQuantityFound && defaultQuantity.Cmp(maxQuantity) > 0 {
				allErrs = append(allErrs, field.Invalid(idxPath.Child("default").Key(string(k)), maxQuantity, fmt.Sprintf("default value %s is greater than max value %s", defaultQuantity.String(), maxQuantity.String())))
			}
			if maxRatioFound && maxRatio.Cmp(*resource.NewQuantity(1, resource.DecimalSI)) < 0 {
				allErrs = append(allErrs, field.Invalid(idxPath.Child("maxLimitRequestRatio").Key(string(k)), maxRatio, fmt.Sprintf("ratio %s is less than 1", maxRatio.String())))
			}
			if maxRatioFound && minQuantityFound && maxQuantityFound {
				maxRatioValue := float64(maxRatio.Value())
				minQuantityValue := minQuantity.Value()
				maxQuantityValue := maxQuantity.Value()
				if maxRatio.Value() < resource.MaxMilliValue && minQuantityValue < resource.MaxMilliValue && maxQuantityValue < resource.MaxMilliValue {
					maxRatioValue = float64(maxRatio.MilliValue()) / 1000
					minQuantityValue = minQuantity.MilliValue()
					maxQuantityValue = maxQuantity.MilliValue()
				}
				maxRatioLimit := float64(maxQuantityValue) / float64(minQuantityValue)
				if maxRatioValue > maxRatioLimit {
					allErrs = append(allErrs, field.Invalid(idxPath.Child("maxLimitRequestRatio").Key(string(k)), maxRatio, fmt.Sprintf("ratio %s is greater than max/min = %f", maxRatio.String(), maxRatioLimit)))
				}
			}

			// for GPU, hugepages and other resources that are not allowed to overcommit,
			// the default value and defaultRequest value must match if both are specified
			if !helper.IsOvercommitAllowed(core.ResourceName(k)) && defaultQuantityFound && defaultRequestQuantityFound && defaultQuantity.Cmp(defaultRequestQuantity) != 0 {
				allErrs = append(allErrs, field.Invalid(idxPath.Child("defaultRequest").Key(string(k)), defaultRequestQuantity, fmt.Sprintf("default value %s must equal to defaultRequest value %s in %s", defaultQuantity.String(), defaultRequestQuantity.String(), k)))
			}
		}
	}

	return allErrs
}

// ValidateServiceAccount tests if required fields in the ServiceAccount are set.
func ValidateServiceAccount(serviceAccount *core.ServiceAccount) field.ErrorList {
	allErrs := ValidateObjectMeta(&serviceAccount.ObjectMeta, true, ValidateServiceAccountName, field.NewPath("metadata"))
	return allErrs
}

// ValidateServiceAccountUpdate tests if required fields in the ServiceAccount are set.
func ValidateServiceAccountUpdate(newServiceAccount, oldServiceAccount *core.ServiceAccount) field.ErrorList {
	allErrs := ValidateObjectMetaUpdate(&newServiceAccount.ObjectMeta, &oldServiceAccount.ObjectMeta, field.NewPath("metadata"))
	allErrs = append(allErrs, ValidateServiceAccount(newServiceAccount)...)
	return allErrs
}

// ValidateSecret tests if required fields in the Secret are set.
func ValidateSecret(secret *core.Secret) field.ErrorList {
	allErrs := ValidateObjectMeta(&secret.ObjectMeta, true, ValidateSecretName, field.NewPath("metadata"))

	dataPath := field.NewPath("data")
	totalSize := 0
	for key, value := range secret.Data {
		for _, msg := range validation.IsConfigMapKey(key) {
			allErrs = append(allErrs, field.Invalid(dataPath.Key(key), key, msg))
		}
		totalSize += len(value)
	}
	if totalSize > core.MaxSecretSize {
		allErrs = append(allErrs, field.TooLong(dataPath, "" /*unused*/, core.MaxSecretSize))
	}

	switch secret.Type {
	case core.SecretTypeServiceAccountToken:
		// Only require Annotations[kubernetes.io/service-account.name]
		// Additional fields (like Annotations[kubernetes.io/service-account.uid] and Data[token]) might be contributed later by a controller loop
		if value := secret.Annotations[core.ServiceAccountNameKey]; len(value) == 0 {
			allErrs = append(allErrs, field.Required(field.NewPath("metadata", "annotations").Key(core.ServiceAccountNameKey), ""))
		}
	case core.SecretTypeOpaque, "":
	// no-op
	case core.SecretTypeDockercfg:
		dockercfgBytes, exists := secret.Data[core.DockerConfigKey]
		if !exists {
			allErrs = append(allErrs, field.Required(dataPath.Key(core.DockerConfigKey), ""))
			break
		}

		// make sure that the content is well-formed json.
		if err := json.Unmarshal(dockercfgBytes, &map[string]interface{}{}); err != nil {
			allErrs = append(allErrs, field.Invalid(dataPath.Key(core.DockerConfigKey), "<secret contents redacted>", err.Error()))
		}
	case core.SecretTypeDockerConfigJSON:
		dockerConfigJSONBytes, exists := secret.Data[core.DockerConfigJSONKey]
		if !exists {
			allErrs = append(allErrs, field.Required(dataPath.Key(core.DockerConfigJSONKey), ""))
			break
		}

		// make sure that the content is well-formed json.
		if err := json.Unmarshal(dockerConfigJSONBytes, &map[string]interface{}{}); err != nil {
			allErrs = append(allErrs, field.Invalid(dataPath.Key(core.DockerConfigJSONKey), "<secret contents redacted>", err.Error()))
		}
	case core.SecretTypeBasicAuth:
		_, usernameFieldExists := secret.Data[core.BasicAuthUsernameKey]
		_, passwordFieldExists := secret.Data[core.BasicAuthPasswordKey]

		// username or password might be empty, but the field must be present
		if !usernameFieldExists && !passwordFieldExists {
			allErrs = append(allErrs, field.Required(dataPath.Key(core.BasicAuthUsernameKey), ""))
			allErrs = append(allErrs, field.Required(dataPath.Key(core.BasicAuthPasswordKey), ""))
			break
		}
	case core.SecretTypeSSHAuth:
		if len(secret.Data[core.SSHAuthPrivateKey]) == 0 {
			allErrs = append(allErrs, field.Required(dataPath.Key(core.SSHAuthPrivateKey), ""))
			break
		}

	case core.SecretTypeTLS:
		if _, exists := secret.Data[core.TLSCertKey]; !exists {
			allErrs = append(allErrs, field.Required(dataPath.Key(core.TLSCertKey), ""))
		}
		if _, exists := secret.Data[core.TLSPrivateKeyKey]; !exists {
			allErrs = append(allErrs, field.Required(dataPath.Key(core.TLSPrivateKeyKey), ""))
		}
	default:
		// no-op
	}

	return allErrs
}

// ValidateSecretUpdate tests if required fields in the Secret are set.
func ValidateSecretUpdate(newSecret, oldSecret *core.Secret) field.ErrorList {
	allErrs := ValidateObjectMetaUpdate(&newSecret.ObjectMeta, &oldSecret.ObjectMeta, field.NewPath("metadata"))

	allErrs = append(allErrs, ValidateImmutableField(newSecret.Type, oldSecret.Type, field.NewPath("type"))...)
	if oldSecret.Immutable != nil && *oldSecret.Immutable {
		if newSecret.Immutable == nil || !*newSecret.Immutable {
			allErrs = append(allErrs, field.Forbidden(field.NewPath("immutable"), "field is immutable when `immutable` is set"))
		}
		if !reflect.DeepEqual(newSecret.Data, oldSecret.Data) {
			allErrs = append(allErrs, field.Forbidden(field.NewPath("data"), "field is immutable when `immutable` is set"))
		}
		// We don't validate StringData, as it was already converted back to Data
		// before validation is happening.
	}

	allErrs = append(allErrs, ValidateSecret(newSecret)...)
	return allErrs
}

// ValidateConfigMapName can be used to check whether the given ConfigMap name is valid.
// Prefix indicates this name will be used as part of generation, in which case
// trailing dashes are allowed.
var ValidateConfigMapName = apimachineryvalidation.NameIsDNSSubdomain

// ValidateConfigMap tests whether required fields in the ConfigMap are set.
func ValidateConfigMap(cfg *core.ConfigMap) field.ErrorList {
	allErrs := field.ErrorList{}
	allErrs = append(allErrs, ValidateObjectMeta(&cfg.ObjectMeta, true, ValidateConfigMapName, field.NewPath("metadata"))...)

	totalSize := 0

	for key, value := range cfg.Data {
		for _, msg := range validation.IsConfigMapKey(key) {
			allErrs = append(allErrs, field.Invalid(field.NewPath("data").Key(key), key, msg))
		}
		// check if we have a duplicate key in the other bag
		if _, isValue := cfg.BinaryData[key]; isValue {
			msg := "duplicate of key present in binaryData"
			allErrs = append(allErrs, field.Invalid(field.NewPath("data").Key(key), key, msg))
		}
		totalSize += len(value)
	}
	for key, value := range cfg.BinaryData {
		for _, msg := range validation.IsConfigMapKey(key) {
			allErrs = append(allErrs, field.Invalid(field.NewPath("binaryData").Key(key), key, msg))
		}
		totalSize += len(value)
	}
	if totalSize > core.MaxSecretSize {
		// pass back "" to indicate that the error refers to the whole object.
		allErrs = append(allErrs, field.TooLong(field.NewPath(""), "" /*unused*/, core.MaxSecretSize))
	}

	return allErrs
}

// ValidateConfigMapUpdate tests if required fields in the ConfigMap are set.
func ValidateConfigMapUpdate(newCfg, oldCfg *core.ConfigMap) field.ErrorList {
	allErrs := field.ErrorList{}
	allErrs = append(allErrs, ValidateObjectMetaUpdate(&newCfg.ObjectMeta, &oldCfg.ObjectMeta, field.NewPath("metadata"))...)

	if oldCfg.Immutable != nil && *oldCfg.Immutable {
		if newCfg.Immutable == nil || !*newCfg.Immutable {
			allErrs = append(allErrs, field.Forbidden(field.NewPath("immutable"), "field is immutable when `immutable` is set"))
		}
		if !reflect.DeepEqual(newCfg.Data, oldCfg.Data) {
			allErrs = append(allErrs, field.Forbidden(field.NewPath("data"), "field is immutable when `immutable` is set"))
		}
		if !reflect.DeepEqual(newCfg.BinaryData, oldCfg.BinaryData) {
			allErrs = append(allErrs, field.Forbidden(field.NewPath("binaryData"), "field is immutable when `immutable` is set"))
		}
	}

	allErrs = append(allErrs, ValidateConfigMap(newCfg)...)
	return allErrs
}

func validateBasicResource(quantity resource.Quantity, fldPath *field.Path) field.ErrorList {
	if quantity.Value() < 0 {
		return field.ErrorList{field.Invalid(fldPath, quantity.Value(), "must be a valid resource quantity")}
	}
	return field.ErrorList{}
}

func validatePodResourceRequirements(requirements *core.ResourceRequirements, podClaimNames sets.Set[string], fldPath *field.Path, opts PodValidationOptions) field.ErrorList {
	return validateResourceRequirements(requirements, validatePodResourceName, podClaimNames, fldPath, opts)
}

func ValidateContainerResourceRequirements(requirements *core.ResourceRequirements, podClaimNames sets.Set[string], fldPath *field.Path, opts PodValidationOptions) field.ErrorList {
	return validateResourceRequirements(requirements, validateContainerResourceName, podClaimNames, fldPath, opts)
}

// Validates resource requirement spec.
func validateResourceRequirements(requirements *core.ResourceRequirements, resourceNameFn func(core.ResourceName, *field.Path) field.ErrorList, podClaimNames sets.Set[string], fldPath *field.Path, opts PodValidationOptions) field.ErrorList {
	allErrs := field.ErrorList{}
	limPath := fldPath.Child("limits")
	reqPath := fldPath.Child("requests")
	limContainsCPUOrMemory := false
	reqContainsCPUOrMemory := false
	limContainsHugePages := false
	reqContainsHugePages := false
	supportedQoSComputeResources := sets.New(core.ResourceCPU, core.ResourceMemory)
	for resourceName, quantity := range requirements.Limits {

		fldPath := limPath.Key(string(resourceName))
		// Validate resource name.
		allErrs = append(allErrs, resourceNameFn(resourceName, fldPath)...)

		// Validate resource quantity.
		allErrs = append(allErrs, ValidateResourceQuantityValue(resourceName, quantity, fldPath)...)

		if helper.IsHugePageResourceName(resourceName) {
			limContainsHugePages = true
			if err := validateResourceQuantityHugePageValue(resourceName, quantity, opts); err != nil {
				allErrs = append(allErrs, field.Invalid(fldPath, quantity.String(), err.Error()))
			}
		}

		if supportedQoSComputeResources.Has(resourceName) {
			limContainsCPUOrMemory = true
		}
	}
	for resourceName, quantity := range requirements.Requests {
		fldPath := reqPath.Key(string(resourceName))
		// Validate resource name.
		allErrs = append(allErrs, resourceNameFn(resourceName, fldPath)...)

		// Validate resource quantity.
		allErrs = append(allErrs, ValidateResourceQuantityValue(resourceName, quantity, fldPath)...)

		// Check that request <= limit.
		limitQuantity, exists := requirements.Limits[resourceName]
		if exists {
			// For non overcommitable resources, not only requests can't exceed limits, they also can't be lower, i.e. must be equal.
			if quantity.Cmp(limitQuantity) != 0 && !helper.IsOvercommitAllowed(resourceName) {
				allErrs = append(allErrs, field.Invalid(reqPath, quantity.String(), fmt.Sprintf("must be equal to %s limit of %s", resourceName, limitQuantity.String())))
			} else if quantity.Cmp(limitQuantity) > 0 {
				allErrs = append(allErrs, field.Invalid(reqPath, quantity.String(), fmt.Sprintf("must be less than or equal to %s limit of %s", resourceName, limitQuantity.String())))
			}
		} else if !helper.IsOvercommitAllowed(resourceName) {
			allErrs = append(allErrs, field.Required(limPath, "Limit must be set for non overcommitable resources"))
		}
		if helper.IsHugePageResourceName(resourceName) {
			reqContainsHugePages = true
			if err := validateResourceQuantityHugePageValue(resourceName, quantity, opts); err != nil {
				allErrs = append(allErrs, field.Invalid(fldPath, quantity.String(), err.Error()))
			}
		}
		if supportedQoSComputeResources.Has(resourceName) {
			reqContainsCPUOrMemory = true
		}

	}
	if !limContainsCPUOrMemory && !reqContainsCPUOrMemory && (reqContainsHugePages || limContainsHugePages) {
		allErrs = append(allErrs, field.Forbidden(fldPath, "HugePages require cpu or memory"))
	}

	allErrs = append(allErrs, validateResourceClaimNames(requirements.Claims, podClaimNames, fldPath.Child("claims"))...)

	return allErrs
}

// validateResourceClaimNames checks that the names in
// ResourceRequirements.Claims have a corresponding entry in
// PodSpec.ResourceClaims.
func validateResourceClaimNames(claims []core.ResourceClaim, podClaimNames sets.Set[string], fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	names := sets.Set[string]{}
	for i, claim := range claims {
		name := claim.Name
		if name == "" {
			allErrs = append(allErrs, field.Required(fldPath.Index(i), ""))
		} else {
			if names.Has(name) {
				// All requests of that claim already referenced.
				allErrs = append(allErrs, field.Duplicate(fldPath.Index(i), name))
			} else {
				key := name
				if claim.Request != "" {
					allErrs = append(allErrs, ValidateDNS1123Label(claim.Request, fldPath.Index(i).Child("request"))...)
					key += "/" + claim.Request
				}
				if names.Has(key) {
					// The exact same entry was already referenced.
					allErrs = append(allErrs, field.Duplicate(fldPath.Index(i), key))
				} else if claim.Request == "" {
					// When referencing a claim, there's an
					// overlap when previously some request
					// in the claim was referenced. This
					// cannot be checked with a map lookup,
					// we need to iterate.
					for key := range names {
						index := strings.Index(key, "/")
						if index < 0 {
							continue
						}
						if key[0:index] == name {
							allErrs = append(allErrs, field.Duplicate(fldPath.Index(i), name))
						}
					}
				}

				names.Insert(key)
			}
			if !podClaimNames.Has(name) {
				// field.NotFound doesn't accept an
				// explanation. Adding one here is more
				// user-friendly.
				error := field.NotFound(fldPath.Index(i), name)
				error.Detail = "must be one of the names in pod.spec.resourceClaims"
				if len(podClaimNames) == 0 {
					error.Detail += " which is empty"
				} else {
					error.Detail += ": " + strings.Join(sets.List(podClaimNames), ", ")
				}
				allErrs = append(allErrs, error)
			}
		}
	}
	return allErrs
}

func validateResourceQuantityHugePageValue(name core.ResourceName, quantity resource.Quantity, opts PodValidationOptions) error {
	if !helper.IsHugePageResourceName(name) {
		return nil
	}

	if !opts.AllowIndivisibleHugePagesValues && !helper.IsHugePageResourceValueDivisible(name, quantity) {
		return fmt.Errorf("%s is not positive integer multiple of %s", quantity.String(), name)
	}

	return nil
}

// validateResourceQuotaScopes ensures that each enumerated hard resource constraint is valid for set of scopes
func validateResourceQuotaScopes(resourceQuotaSpec *core.ResourceQuotaSpec, fld *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if len(resourceQuotaSpec.Scopes) == 0 {
		return allErrs
	}
	hardLimits := sets.New[core.ResourceName]()
	for k := range resourceQuotaSpec.Hard {
		hardLimits.Insert(k)
	}
	fldPath := fld.Child("scopes")
	scopeSet := sets.New[core.ResourceQuotaScope]()
	for _, scope := range resourceQuotaSpec.Scopes {
		if !helper.IsStandardResourceQuotaScope(scope) {
			allErrs = append(allErrs, field.Invalid(fldPath, resourceQuotaSpec.Scopes, "unsupported scope"))
		}
		for _, k := range sets.List(hardLimits) {
			if helper.IsStandardQuotaResourceName(k) && !helper.IsResourceQuotaScopeValidForResource(scope, k) {
				allErrs = append(allErrs, field.Invalid(fldPath, resourceQuotaSpec.Scopes, "unsupported scope applied to resource"))
			}
		}
		scopeSet.Insert(scope)
	}
	invalidScopePairs := []sets.Set[core.ResourceQuotaScope]{
		sets.New(core.ResourceQuotaScopeBestEffort, core.ResourceQuotaScopeNotBestEffort),
		sets.New(core.ResourceQuotaScopeTerminating, core.ResourceQuotaScopeNotTerminating),
	}
	for _, invalidScopePair := range invalidScopePairs {
		if scopeSet.HasAll(sets.List(invalidScopePair)...) {
			allErrs = append(allErrs, field.Invalid(fldPath, resourceQuotaSpec.Scopes, "conflicting scopes"))
		}
	}
	return allErrs
}

// validateScopedResourceSelectorRequirement tests that the match expressions has valid data
func validateScopedResourceSelectorRequirement(resourceQuotaSpec *core.ResourceQuotaSpec, fld *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	hardLimits := sets.New[core.ResourceName]()
	for k := range resourceQuotaSpec.Hard {
		hardLimits.Insert(k)
	}
	fldPath := fld.Child("matchExpressions")
	scopeSet := sets.New[core.ResourceQuotaScope]()
	for _, req := range resourceQuotaSpec.ScopeSelector.MatchExpressions {
		if !helper.IsStandardResourceQuotaScope(req.ScopeName) {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("scopeName"), req.ScopeName, "unsupported scope"))
		}
		for _, k := range sets.List(hardLimits) {
			if helper.IsStandardQuotaResourceName(k) && !helper.IsResourceQuotaScopeValidForResource(req.ScopeName, k) {
				allErrs = append(allErrs, field.Invalid(fldPath, resourceQuotaSpec.ScopeSelector, "unsupported scope applied to resource"))
			}
		}
		switch req.ScopeName {
		case core.ResourceQuotaScopeBestEffort, core.ResourceQuotaScopeNotBestEffort, core.ResourceQuotaScopeTerminating, core.ResourceQuotaScopeNotTerminating, core.ResourceQuotaScopeCrossNamespacePodAffinity:
			if req.Operator != core.ScopeSelectorOpExists {
				allErrs = append(allErrs, field.Invalid(fldPath.Child("operator"), req.Operator,
					"must be 'Exists' when scope is any of ResourceQuotaScopeTerminating, ResourceQuotaScopeNotTerminating, ResourceQuotaScopeBestEffort, ResourceQuotaScopeNotBestEffort or ResourceQuotaScopeCrossNamespacePodAffinity"))
			}
		}

		switch req.Operator {
		case core.ScopeSelectorOpIn, core.ScopeSelectorOpNotIn:
			if len(req.Values) == 0 {
				allErrs = append(allErrs, field.Required(fldPath.Child("values"),
					"must be at least one value when `operator` is 'In' or 'NotIn' for scope selector"))
			}
		case core.ScopeSelectorOpExists, core.ScopeSelectorOpDoesNotExist:
			if len(req.Values) != 0 {
				allErrs = append(allErrs, field.Invalid(fldPath.Child("values"), req.Values,
					"must be no value when `operator` is 'Exist' or 'DoesNotExist' for scope selector"))
			}
		default:
			allErrs = append(allErrs, field.Invalid(fldPath.Child("operator"), req.Operator, "not a valid selector operator"))
		}
		scopeSet.Insert(req.ScopeName)
	}
	invalidScopePairs := []sets.Set[core.ResourceQuotaScope]{
		sets.New(core.ResourceQuotaScopeBestEffort, core.ResourceQuotaScopeNotBestEffort),
		sets.New(core.ResourceQuotaScopeTerminating, core.ResourceQuotaScopeNotTerminating),
	}
	for _, invalidScopePair := range invalidScopePairs {
		if scopeSet.HasAll(sets.List(invalidScopePair)...) {
			allErrs = append(allErrs, field.Invalid(fldPath, resourceQuotaSpec.Scopes, "conflicting scopes"))
		}
	}

	return allErrs
}

// validateScopeSelector tests that the specified scope selector has valid data
func validateScopeSelector(resourceQuotaSpec *core.ResourceQuotaSpec, fld *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if resourceQuotaSpec.ScopeSelector == nil {
		return allErrs
	}
	allErrs = append(allErrs, validateScopedResourceSelectorRequirement(resourceQuotaSpec, fld.Child("scopeSelector"))...)
	return allErrs
}

// ValidateResourceQuota tests if required fields in the ResourceQuota are set.
func ValidateResourceQuota(resourceQuota *core.ResourceQuota) field.ErrorList {
	allErrs := ValidateObjectMeta(&resourceQuota.ObjectMeta, true, ValidateResourceQuotaName, field.NewPath("metadata"))

	allErrs = append(allErrs, ValidateResourceQuotaSpec(&resourceQuota.Spec, field.NewPath("spec"))...)
	allErrs = append(allErrs, ValidateResourceQuotaStatus(&resourceQuota.Status, field.NewPath("status"))...)

	return allErrs
}

func ValidateResourceQuotaStatus(status *core.ResourceQuotaStatus, fld *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}

	fldPath := fld.Child("hard")
	for k, v := range status.Hard {
		resPath := fldPath.Key(string(k))
		allErrs = append(allErrs, ValidateResourceQuotaResourceName(k, resPath)...)
		allErrs = append(allErrs, ValidateResourceQuantityValue(k, v, resPath)...)
	}
	fldPath = fld.Child("used")
	for k, v := range status.Used {
		resPath := fldPath.Key(string(k))
		allErrs = append(allErrs, ValidateResourceQuotaResourceName(k, resPath)...)
		allErrs = append(allErrs, ValidateResourceQuantityValue(k, v, resPath)...)
	}

	return allErrs
}

func ValidateResourceQuotaSpec(resourceQuotaSpec *core.ResourceQuotaSpec, fld *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}

	fldPath := fld.Child("hard")
	for k, v := range resourceQuotaSpec.Hard {
		resPath := fldPath.Key(string(k))
		allErrs = append(allErrs, ValidateResourceQuotaResourceName(k, resPath)...)
		allErrs = append(allErrs, ValidateResourceQuantityValue(k, v, resPath)...)
	}

	allErrs = append(allErrs, validateResourceQuotaScopes(resourceQuotaSpec, fld)...)
	allErrs = append(allErrs, validateScopeSelector(resourceQuotaSpec, fld)...)

	return allErrs
}

// ValidateResourceQuantityValue enforces that specified quantity is valid for specified resource
func ValidateResourceQuantityValue(resource core.ResourceName, value resource.Quantity, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	allErrs = append(allErrs, ValidateNonnegativeQuantity(value, fldPath)...)
	if helper.IsIntegerResourceName(resource) {
		if value.MilliValue()%int64(1000) != int64(0) {
			allErrs = append(allErrs, field.Invalid(fldPath, value, isNotIntegerErrorMsg))
		}
	}
	return allErrs
}

// ValidateResourceQuotaUpdate tests to see if the update is legal for an end user to make.
func ValidateResourceQuotaUpdate(newResourceQuota, oldResourceQuota *core.ResourceQuota) field.ErrorList {
	allErrs := ValidateObjectMetaUpdate(&newResourceQuota.ObjectMeta, &oldResourceQuota.ObjectMeta, field.NewPath("metadata"))
	allErrs = append(allErrs, ValidateResourceQuotaSpec(&newResourceQuota.Spec, field.NewPath("spec"))...)

	// ensure scopes cannot change, and that resources are still valid for scope
	fldPath := field.NewPath("spec", "scopes")
	oldScopes := sets.New[string]()
	newScopes := sets.New[string]()
	for _, scope := range newResourceQuota.Spec.Scopes {
		newScopes.Insert(string(scope))
	}
	for _, scope := range oldResourceQuota.Spec.Scopes {
		oldScopes.Insert(string(scope))
	}
	if !oldScopes.Equal(newScopes) {
		allErrs = append(allErrs, field.Invalid(fldPath, newResourceQuota.Spec.Scopes, fieldImmutableErrorMsg))
	}

	return allErrs
}

// ValidateResourceQuotaStatusUpdate tests to see if the status update is legal for an end user to make.
func ValidateResourceQuotaStatusUpdate(newResourceQuota, oldResourceQuota *core.ResourceQuota) field.ErrorList {
	allErrs := ValidateObjectMetaUpdate(&newResourceQuota.ObjectMeta, &oldResourceQuota.ObjectMeta, field.NewPath("metadata"))
	if len(newResourceQuota.ResourceVersion) == 0 {
		allErrs = append(allErrs, field.Required(field.NewPath("resourceVersion"), ""))
	}
	fldPath := field.NewPath("status", "hard")
	for k, v := range newResourceQuota.Status.Hard {
		resPath := fldPath.Key(string(k))
		allErrs = append(allErrs, ValidateResourceQuotaResourceName(k, resPath)...)
		allErrs = append(allErrs, ValidateResourceQuantityValue(k, v, resPath)...)
	}
	fldPath = field.NewPath("status", "used")
	for k, v := range newResourceQuota.Status.Used {
		resPath := fldPath.Key(string(k))
		allErrs = append(allErrs, ValidateResourceQuotaResourceName(k, resPath)...)
		allErrs = append(allErrs, ValidateResourceQuantityValue(k, v, resPath)...)
	}
	return allErrs
}

// ValidateNamespace tests if required fields are set.
func ValidateNamespace(namespace *core.Namespace) field.ErrorList {
	allErrs := ValidateObjectMeta(&namespace.ObjectMeta, false, ValidateNamespaceName, field.NewPath("metadata"))
	for i := range namespace.Spec.Finalizers {
		allErrs = append(allErrs, validateFinalizerName(string(namespace.Spec.Finalizers[i]), field.NewPath("spec", "finalizers"))...)
	}
	return allErrs
}

// Validate finalizer names
func validateFinalizerName(stringValue string, fldPath *field.Path) field.ErrorList {
	allErrs := apimachineryvalidation.ValidateFinalizerName(stringValue, fldPath)
	allErrs = append(allErrs, validateKubeFinalizerName(stringValue, fldPath)...)
	return allErrs
}

// validateKubeFinalizerName checks for "standard" names of legacy finalizer
func validateKubeFinalizerName(stringValue string, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if len(strings.Split(stringValue, "/")) == 1 {
		if !helper.IsStandardFinalizerName(stringValue) {
			return append(allErrs, field.Invalid(fldPath, stringValue, "name is neither a standard finalizer name nor is it fully qualified"))
		}
	}

	return allErrs
}

// ValidateNamespaceUpdate tests to make sure a namespace update can be applied.
func ValidateNamespaceUpdate(newNamespace *core.Namespace, oldNamespace *core.Namespace) field.ErrorList {
	allErrs := ValidateObjectMetaUpdate(&newNamespace.ObjectMeta, &oldNamespace.ObjectMeta, field.NewPath("metadata"))
	return allErrs
}

// ValidateNamespaceStatusUpdate tests to see if the update is legal for an end user to make.
func ValidateNamespaceStatusUpdate(newNamespace, oldNamespace *core.Namespace) field.ErrorList {
	allErrs := ValidateObjectMetaUpdate(&newNamespace.ObjectMeta, &oldNamespace.ObjectMeta, field.NewPath("metadata"))
	if newNamespace.DeletionTimestamp.IsZero() {
		if newNamespace.Status.Phase != core.NamespaceActive {
			allErrs = append(allErrs, field.Invalid(field.NewPath("status", "Phase"), newNamespace.Status.Phase, "may only be 'Active' if `deletionTimestamp` is empty"))
		}
	} else {
		if newNamespace.Status.Phase != core.NamespaceTerminating {
			allErrs = append(allErrs, field.Invalid(field.NewPath("status", "Phase"), newNamespace.Status.Phase, "may only be 'Terminating' if `deletionTimestamp` is not empty"))
		}
	}
	return allErrs
}

// ValidateNamespaceFinalizeUpdate tests to see if the update is legal for an end user to make.
func ValidateNamespaceFinalizeUpdate(newNamespace, oldNamespace *core.Namespace) field.ErrorList {
	allErrs := ValidateObjectMetaUpdate(&newNamespace.ObjectMeta, &oldNamespace.ObjectMeta, field.NewPath("metadata"))

	fldPath := field.NewPath("spec", "finalizers")
	for i := range newNamespace.Spec.Finalizers {
		idxPath := fldPath.Index(i)
		allErrs = append(allErrs, validateFinalizerName(string(newNamespace.Spec.Finalizers[i]), idxPath)...)
	}
	return allErrs
}

// ValidateEndpoints validates Endpoints on create and update.
func ValidateEndpoints(endpoints *core.Endpoints) field.ErrorList {
	allErrs := ValidateObjectMeta(&endpoints.ObjectMeta, true, ValidateEndpointsName, field.NewPath("metadata"))
	allErrs = append(allErrs, ValidateEndpointsSpecificAnnotations(endpoints.Annotations, field.NewPath("annotations"))...)
	allErrs = append(allErrs, validateEndpointSubsets(endpoints.Subsets, field.NewPath("subsets"))...)
	return allErrs
}

// ValidateEndpointsCreate validates Endpoints on create.
func ValidateEndpointsCreate(endpoints *core.Endpoints) field.ErrorList {
	return ValidateEndpoints(endpoints)
}

// ValidateEndpointsUpdate validates Endpoints on update. NodeName changes are
// allowed during update to accommodate the case where nodeIP or PodCIDR is
// reused. An existing endpoint ip will have a different nodeName if this
// happens.
func ValidateEndpointsUpdate(newEndpoints, oldEndpoints *core.Endpoints) field.ErrorList {
	allErrs := ValidateObjectMetaUpdate(&newEndpoints.ObjectMeta, &oldEndpoints.ObjectMeta, field.NewPath("metadata"))
	allErrs = append(allErrs, ValidateEndpoints(newEndpoints)...)
	return allErrs
}

func validateEndpointSubsets(subsets []core.EndpointSubset, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	for i := range subsets {
		ss := &subsets[i]
		idxPath := fldPath.Index(i)

		// EndpointSubsets must include endpoint address. For headless service, we allow its endpoints not to have ports.
		if len(ss.Addresses) == 0 && len(ss.NotReadyAddresses) == 0 {
			// TODO: consider adding a RequiredOneOf() error for this and similar cases
			allErrs = append(allErrs, field.Required(idxPath, "must specify `addresses` or `notReadyAddresses`"))
		}
		for addr := range ss.Addresses {
			allErrs = append(allErrs, validateEndpointAddress(&ss.Addresses[addr], idxPath.Child("addresses").Index(addr))...)
		}
		for addr := range ss.NotReadyAddresses {
			allErrs = append(allErrs, validateEndpointAddress(&ss.NotReadyAddresses[addr], idxPath.Child("notReadyAddresses").Index(addr))...)
		}
		for port := range ss.Ports {
			allErrs = append(allErrs, validateEndpointPort(&ss.Ports[port], len(ss.Ports) > 1, idxPath.Child("ports").Index(port))...)
		}
	}

	return allErrs
}

func validateEndpointAddress(address *core.EndpointAddress, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	allErrs = append(allErrs, validation.IsValidIP(fldPath.Child("ip"), address.IP)...)
	if len(address.Hostname) > 0 {
		allErrs = append(allErrs, ValidateDNS1123Label(address.Hostname, fldPath.Child("hostname"))...)
	}
	// During endpoint update, verify that NodeName is a DNS subdomain and transition rules allow the update
	if address.NodeName != nil {
		for _, msg := range ValidateNodeName(*address.NodeName, false) {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("nodeName"), *address.NodeName, msg))
		}
	}
	allErrs = append(allErrs, ValidateNonSpecialIP(address.IP, fldPath.Child("ip"))...)
	return allErrs
}

// ValidateNonSpecialIP is used to validate Endpoints, EndpointSlices, and
// external IPs. Specifically, this disallows unspecified and loopback addresses
// are nonsensical and link-local addresses tend to be used for node-centric
// purposes (e.g. metadata service).
//
// IPv6 references
// - https://www.iana.org/assignments/iana-ipv6-special-registry/iana-ipv6-special-registry.xhtml
// - https://www.iana.org/assignments/ipv6-multicast-addresses/ipv6-multicast-addresses.xhtml
func ValidateNonSpecialIP(ipAddress string, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	ip := netutils.ParseIPSloppy(ipAddress)
	if ip == nil {
		allErrs = append(allErrs, field.Invalid(fldPath, ipAddress, "must be a valid IP address"))
		return allErrs
	}
	if ip.IsUnspecified() {
		allErrs = append(allErrs, field.Invalid(fldPath, ipAddress, fmt.Sprintf("may not be unspecified (%v)", ipAddress)))
	}
	if ip.IsLoopback() {
		allErrs = append(allErrs, field.Invalid(fldPath, ipAddress, "may not be in the loopback range (127.0.0.0/8, ::1/128)"))
	}
	if ip.IsLinkLocalUnicast() {
		allErrs = append(allErrs, field.Invalid(fldPath, ipAddress, "may not be in the link-local range (169.254.0.0/16, fe80::/10)"))
	}
	if ip.IsLinkLocalMulticast() {
		allErrs = append(allErrs, field.Invalid(fldPath, ipAddress, "may not be in the link-local multicast range (224.0.0.0/24, ff02::/10)"))
	}
	return allErrs
}

func validateEndpointPort(port *core.EndpointPort, requireName bool, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if requireName && len(port.Name) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("name"), ""))
	} else if len(port.Name) != 0 {
		allErrs = append(allErrs, ValidateDNS1123Label(port.Name, fldPath.Child("name"))...)
	}
	for _, msg := range validation.IsValidPortNum(int(port.Port)) {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("port"), port.Port, msg))
	}
	if len(port.Protocol) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("protocol"), ""))
	} else if !supportedPortProtocols.Has(port.Protocol) {
		allErrs = append(allErrs, field.NotSupported(fldPath.Child("protocol"), port.Protocol, sets.List(supportedPortProtocols)))
	}
	if port.AppProtocol != nil {
		allErrs = append(allErrs, ValidateQualifiedName(*port.AppProtocol, fldPath.Child("appProtocol"))...)
	}
	return allErrs
}

// ValidateSecurityContext ensures the security context contains valid settings
func ValidateSecurityContext(sc *core.SecurityContext, fldPath *field.Path, hostUsers bool) field.ErrorList {
	allErrs := field.ErrorList{}
	// this should only be true for testing since SecurityContext is defaulted by the core
	if sc == nil {
		return allErrs
	}

	if sc.Privileged != nil {
		if *sc.Privileged && !capabilities.Get().AllowPrivileged {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("privileged"), "disallowed by cluster policy"))
		}
	}

	if sc.RunAsUser != nil {
		for _, msg := range validation.IsValidUserID(*sc.RunAsUser) {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("runAsUser"), *sc.RunAsUser, msg))
		}
	}

	if sc.RunAsGroup != nil {
		for _, msg := range validation.IsValidGroupID(*sc.RunAsGroup) {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("runAsGroup"), *sc.RunAsGroup, msg))
		}
	}

	if sc.ProcMount != nil {
		if err := ValidateProcMountType(fldPath.Child("procMount"), *sc.ProcMount); err != nil {
			allErrs = append(allErrs, err)
		}
		if hostUsers && *sc.ProcMount == core.UnmaskedProcMount {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("procMount"), sc.ProcMount, "`hostUsers` must be false to use `Unmasked`"))
		}

	}
	allErrs = append(allErrs, validateSeccompProfileField(sc.SeccompProfile, fldPath.Child("seccompProfile"))...)
	if sc.AllowPrivilegeEscalation != nil && !*sc.AllowPrivilegeEscalation {
		if sc.Privileged != nil && *sc.Privileged {
			allErrs = append(allErrs, field.Invalid(fldPath, sc, "cannot set `allowPrivilegeEscalation` to false and `privileged` to true"))
		}

		if sc.Capabilities != nil {
			for _, cap := range sc.Capabilities.Add {
				if string(cap) == "CAP_SYS_ADMIN" {
					allErrs = append(allErrs, field.Invalid(fldPath, sc, "cannot set `allowPrivilegeEscalation` to false and `capabilities.Add` CAP_SYS_ADMIN"))
				}
			}
		}
	}

	allErrs = append(allErrs, validateWindowsSecurityContextOptions(sc.WindowsOptions, fldPath.Child("windowsOptions"))...)
	allErrs = append(allErrs, ValidateAppArmorProfileField(sc.AppArmorProfile, fldPath.Child("appArmorProfile"))...)

	return allErrs
}

// maxGMSACredentialSpecLength is the max length, in bytes, for the actual contents
// of a GMSA cred spec. In general, those shouldn't be more than a few hundred bytes,
// so we want to give plenty of room here while still providing an upper bound.
// The runAsUserName field will be used to execute the given container's entrypoint, and
// it can be formatted as "DOMAIN/USER", where the DOMAIN is optional, maxRunAsUserNameDomainLength
// is the max character length for the user's DOMAIN, and maxRunAsUserNameUserLength
// is the max character length for the USER itself. Both the DOMAIN and USER have their
// own restrictions, and more information about them can be found here:
// https://support.microsoft.com/en-us/help/909264/naming-conventions-in-active-directory-for-computers-domains-sites-and
// https://docs.microsoft.com/en-us/previous-versions/windows/it-pro/windows-2000-server/bb726984(v=technet.10)
const (
	maxGMSACredentialSpecLengthInKiB = 64
	maxGMSACredentialSpecLength      = maxGMSACredentialSpecLengthInKiB * 1024
	maxRunAsUserNameDomainLength     = 256
	maxRunAsUserNameUserLength       = 104
)

var (
	// control characters are not permitted in the runAsUserName field.
	ctrlRegex = regexp.MustCompile(`[[:cntrl:]]+`)

	// a valid NetBios Domain name cannot start with a dot, has at least 1 character,
	// at most 15 characters, and it cannot the characters: \ / : * ? " < > |
	validNetBiosRegex = regexp.MustCompile(`^[^\\/:\*\?"<>|\.][^\\/:\*\?"<>|]{0,14}$`)

	// a valid DNS name contains only alphanumeric characters, dots, and dashes.
	dnsLabelFormat                 = `[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?`
	dnsSubdomainFormat             = fmt.Sprintf(`^%s(?:\.%s)*$`, dnsLabelFormat, dnsLabelFormat)
	validWindowsUserDomainDNSRegex = regexp.MustCompile(dnsSubdomainFormat)

	// a username is invalid if it contains the characters: " / \ [ ] : ; | = , + * ? < > @
	// or it contains only dots or spaces.
	invalidUserNameCharsRegex      = regexp.MustCompile(`["/\\:;|=,\+\*\?<>@\[\]]`)
	invalidUserNameDotsSpacesRegex = regexp.MustCompile(`^[\. ]+$`)
)

func validateWindowsSecurityContextOptions(windowsOptions *core.WindowsSecurityContextOptions, fieldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}

	if windowsOptions == nil {
		return allErrs
	}

	if windowsOptions.GMSACredentialSpecName != nil {
		// gmsaCredentialSpecName must be the name of a custom resource
		for _, msg := range validation.IsDNS1123Subdomain(*windowsOptions.GMSACredentialSpecName) {
			allErrs = append(allErrs, field.Invalid(fieldPath.Child("gmsaCredentialSpecName"), windowsOptions.GMSACredentialSpecName, msg))
		}
	}

	if windowsOptions.GMSACredentialSpec != nil {
		if l := len(*windowsOptions.GMSACredentialSpec); l == 0 {
			allErrs = append(allErrs, field.Invalid(fieldPath.Child("gmsaCredentialSpec"), windowsOptions.GMSACredentialSpec, "gmsaCredentialSpec cannot be an empty string"))
		} else if l > maxGMSACredentialSpecLength {
			errMsg := fmt.Sprintf("gmsaCredentialSpec size must be under %d KiB", maxGMSACredentialSpecLengthInKiB)
			allErrs = append(allErrs, field.Invalid(fieldPath.Child("gmsaCredentialSpec"), windowsOptions.GMSACredentialSpec, errMsg))
		}
	}

	if windowsOptions.RunAsUserName != nil {
		if l := len(*windowsOptions.RunAsUserName); l == 0 {
			allErrs = append(allErrs, field.Invalid(fieldPath.Child("runAsUserName"), windowsOptions.RunAsUserName, "runAsUserName cannot be an empty string"))
		} else if ctrlRegex.MatchString(*windowsOptions.RunAsUserName) {
			errMsg := "runAsUserName cannot contain control characters"
			allErrs = append(allErrs, field.Invalid(fieldPath.Child("runAsUserName"), windowsOptions.RunAsUserName, errMsg))
		} else if parts := strings.Split(*windowsOptions.RunAsUserName, "\\"); len(parts) > 2 {
			errMsg := "runAsUserName cannot contain more than one backslash"
			allErrs = append(allErrs, field.Invalid(fieldPath.Child("runAsUserName"), windowsOptions.RunAsUserName, errMsg))
		} else {
			var (
				hasDomain = false
				domain    = ""
				user      string
			)
			if len(parts) == 1 {
				user = parts[0]
			} else {
				hasDomain = true
				domain = parts[0]
				user = parts[1]
			}

			if len(domain) >= maxRunAsUserNameDomainLength {
				errMsg := fmt.Sprintf("runAsUserName's Domain length must be under %d characters", maxRunAsUserNameDomainLength)
				allErrs = append(allErrs, field.Invalid(fieldPath.Child("runAsUserName"), windowsOptions.RunAsUserName, errMsg))
			}

			if hasDomain && !(validNetBiosRegex.MatchString(domain) || validWindowsUserDomainDNSRegex.MatchString(domain)) {
				errMsg := "runAsUserName's Domain doesn't match the NetBios nor the DNS format"
				allErrs = append(allErrs, field.Invalid(fieldPath.Child("runAsUserName"), windowsOptions.RunAsUserName, errMsg))
			}

			if l := len(user); l == 0 {
				errMsg := "runAsUserName's User cannot be empty"
				allErrs = append(allErrs, field.Invalid(fieldPath.Child("runAsUserName"), windowsOptions.RunAsUserName, errMsg))
			} else if l > maxRunAsUserNameUserLength {
				errMsg := fmt.Sprintf("runAsUserName's User length must not be longer than %d characters", maxRunAsUserNameUserLength)
				allErrs = append(allErrs, field.Invalid(fieldPath.Child("runAsUserName"), windowsOptions.RunAsUserName, errMsg))
			}

			if invalidUserNameDotsSpacesRegex.MatchString(user) {
				errMsg := `runAsUserName's User cannot contain only periods or spaces`
				allErrs = append(allErrs, field.Invalid(fieldPath.Child("runAsUserName"), windowsOptions.RunAsUserName, errMsg))
			}

			if invalidUserNameCharsRegex.MatchString(user) {
				errMsg := `runAsUserName's User cannot contain the following characters: "/\:;|=,+*?<>@[]`
				allErrs = append(allErrs, field.Invalid(fieldPath.Child("runAsUserName"), windowsOptions.RunAsUserName, errMsg))
			}
		}
	}

	return allErrs
}

func validateWindowsHostProcessPod(podSpec *core.PodSpec, fieldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}

	// Keep track of container and hostProcess container count for validate
	containerCount := 0
	hostProcessContainerCount := 0

	var podHostProcess *bool
	if podSpec.SecurityContext != nil && podSpec.SecurityContext.WindowsOptions != nil {
		podHostProcess = podSpec.SecurityContext.WindowsOptions.HostProcess
	}

	hostNetwork := false
	if podSpec.SecurityContext != nil {
		hostNetwork = podSpec.SecurityContext.HostNetwork
	}

	podshelper.VisitContainersWithPath(podSpec, fieldPath, func(c *core.Container, cFieldPath *field.Path) bool {
		containerCount++

		var containerHostProcess *bool = nil
		if c.SecurityContext != nil && c.SecurityContext.WindowsOptions != nil {
			containerHostProcess = c.SecurityContext.WindowsOptions.HostProcess
		}

		if podHostProcess != nil && containerHostProcess != nil && *podHostProcess != *containerHostProcess {
			errMsg := fmt.Sprintf("pod hostProcess value must be identical if both are specified, was %v", *podHostProcess)
			allErrs = append(allErrs, field.Invalid(cFieldPath.Child("securityContext", "windowsOptions", "hostProcess"), *containerHostProcess, errMsg))
		}

		switch {
		case containerHostProcess != nil && *containerHostProcess:
			// Container explicitly sets hostProcess=true
			hostProcessContainerCount++
		case containerHostProcess == nil && podHostProcess != nil && *podHostProcess:
			// Container inherits hostProcess=true from pod settings
			hostProcessContainerCount++
		}

		return true
	})

	if hostProcessContainerCount > 0 {
		// At present, if a Windows Pods contains any HostProcess containers than all containers must be
		// HostProcess containers (explicitly set or inherited).
		if hostProcessContainerCount != containerCount {
			errMsg := "If pod contains any hostProcess containers then all containers must be HostProcess containers"
			allErrs = append(allErrs, field.Invalid(fieldPath, "", errMsg))
		}

		// At present Windows Pods which contain HostProcess containers must also set HostNetwork.
		if !hostNetwork {
			errMsg := "hostNetwork must be true if pod contains any hostProcess containers"
			allErrs = append(allErrs, field.Invalid(fieldPath.Child("hostNetwork"), hostNetwork, errMsg))
		}

		if !capabilities.Get().AllowPrivileged {
			errMsg := "hostProcess containers are disallowed by cluster policy"
			allErrs = append(allErrs, field.Forbidden(fieldPath, errMsg))
		}
	}

	return allErrs
}

// validateOS validates the OS field within pod spec
func validateOS(podSpec *core.PodSpec, fldPath *field.Path, opts PodValidationOptions) field.ErrorList {
	allErrs := field.ErrorList{}
	os := podSpec.OS
	if os == nil {
		return allErrs
	}
	if len(os.Name) == 0 {
		return append(allErrs, field.Required(fldPath.Child("name"), "cannot be empty"))
	}
	if !validOS.Has(os.Name) {
		allErrs = append(allErrs, field.NotSupported(fldPath, os.Name, sets.List(validOS)))
	}
	return allErrs
}

var validLogStreams = sets.New[string](
	core.LogStreamStdout,
	core.LogStreamStderr,
	core.LogStreamAll,
)

func ValidatePodLogOptions(opts *core.PodLogOptions, allowStreamSelection bool) field.ErrorList {
	allErrs := field.ErrorList{}
	if opts.TailLines != nil && *opts.TailLines < 0 {
		allErrs = append(allErrs, field.Invalid(field.NewPath("tailLines"), *opts.TailLines, isNegativeErrorMsg))
	}
	if opts.LimitBytes != nil && *opts.LimitBytes < 1 {
		allErrs = append(allErrs, field.Invalid(field.NewPath("limitBytes"), *opts.LimitBytes, "must be greater than 0"))
	}
	switch {
	case opts.SinceSeconds != nil && opts.SinceTime != nil:
		allErrs = append(allErrs, field.Forbidden(field.NewPath(""), "at most one of `sinceTime` or `sinceSeconds` may be specified"))
	case opts.SinceSeconds != nil:
		if *opts.SinceSeconds < 1 {
			allErrs = append(allErrs, field.Invalid(field.NewPath("sinceSeconds"), *opts.SinceSeconds, "must be greater than 0"))
		}
	}
	if allowStreamSelection {
		if opts.Stream == nil {
			allErrs = append(allErrs, field.Required(field.NewPath("stream"), "must be specified"))
		} else {
			if !validLogStreams.Has(*opts.Stream) {
				allErrs = append(allErrs, field.NotSupported(field.NewPath("stream"), *opts.Stream, validLogStreams.UnsortedList()))
			}
			if *opts.Stream != core.LogStreamAll && opts.TailLines != nil {
				allErrs = append(allErrs, field.Forbidden(field.NewPath(""), "`tailLines` and specific `stream` are mutually exclusive for now"))
			}
		}
	} else if opts.Stream != nil {
		allErrs = append(allErrs, field.Forbidden(field.NewPath("stream"), "may not be specified"))
	}
	return allErrs
}

var (
	supportedLoadBalancerIPMode = sets.New(core.LoadBalancerIPModeVIP, core.LoadBalancerIPModeProxy)
)

// ValidateLoadBalancerStatus validates required fields on a LoadBalancerStatus
func ValidateLoadBalancerStatus(status *core.LoadBalancerStatus, fldPath *field.Path, spec *core.ServiceSpec) field.ErrorList {
	allErrs := field.ErrorList{}
	ingrPath := fldPath.Child("ingress")
	if !utilfeature.DefaultFeatureGate.Enabled(features.AllowServiceLBStatusOnNonLB) && spec.Type != core.ServiceTypeLoadBalancer && len(status.Ingress) != 0 {
		allErrs = append(allErrs, field.Forbidden(ingrPath, "may only be used when `spec.type` is 'LoadBalancer'"))
	} else {
		for i, ingress := range status.Ingress {
			idxPath := ingrPath.Index(i)
			if len(ingress.IP) > 0 {
				allErrs = append(allErrs, validation.IsValidIP(idxPath.Child("ip"), ingress.IP)...)
			}

			if utilfeature.DefaultFeatureGate.Enabled(features.LoadBalancerIPMode) && ingress.IPMode == nil {
				if len(ingress.IP) > 0 {
					allErrs = append(allErrs, field.Required(idxPath.Child("ipMode"), "must be specified when `ip` is set"))
				}
			} else if ingress.IPMode != nil && len(ingress.IP) == 0 {
				allErrs = append(allErrs, field.Forbidden(idxPath.Child("ipMode"), "may not be specified when `ip` is not set"))
			} else if ingress.IPMode != nil && !supportedLoadBalancerIPMode.Has(*ingress.IPMode) {
				allErrs = append(allErrs, field.NotSupported(idxPath.Child("ipMode"), ingress.IPMode, sets.List(supportedLoadBalancerIPMode)))
			}

			if len(ingress.Hostname) > 0 {
				for _, msg := range validation.IsDNS1123Subdomain(ingress.Hostname) {
					allErrs = append(allErrs, field.Invalid(idxPath.Child("hostname"), ingress.Hostname, msg))
				}
				if isIP := (netutils.ParseIPSloppy(ingress.Hostname) != nil); isIP {
					allErrs = append(allErrs, field.Invalid(idxPath.Child("hostname"), ingress.Hostname, "must be a DNS name, not an IP address"))
				}
			}
		}
	}
	return allErrs
}

// validateVolumeNodeAffinity tests that the PersistentVolume.NodeAffinity has valid data
// returns:
// - true if volumeNodeAffinity is set
// - errorList if there are validation errors
func validateVolumeNodeAffinity(nodeAffinity *core.VolumeNodeAffinity, opts PersistentVolumeSpecValidationOptions, fldPath *field.Path) (bool, field.ErrorList) {
	allErrs := field.ErrorList{}

	if nodeAffinity == nil {
		return false, allErrs
	}

	if nodeAffinity.Required != nil {
		allErrs = append(allErrs, ValidateNodeSelector(nodeAffinity.Required, opts.AllowInvalidLabelValueInRequiredNodeAffinity, fldPath.Child("required"))...)
	} else {
		allErrs = append(allErrs, field.Required(fldPath.Child("required"), "must specify required node constraints"))
	}

	return true, allErrs
}

func IsDecremented(update, old *int32) bool {
	if update == nil && old != nil {
		return true
	}
	if update == nil || old == nil {
		return false
	}
	return *update < *old
}

// ValidateProcMountType tests that the argument is a valid ProcMountType.
func ValidateProcMountType(fldPath *field.Path, procMountType core.ProcMountType) *field.Error {
	switch procMountType {
	case core.DefaultProcMount, core.UnmaskedProcMount:
		return nil
	default:
		return field.NotSupported(fldPath, procMountType, []core.ProcMountType{core.DefaultProcMount, core.UnmaskedProcMount})
	}
}

var (
	supportedScheduleActions = sets.New(core.DoNotSchedule, core.ScheduleAnyway)
)

// validateTopologySpreadConstraints validates given TopologySpreadConstraints.
func validateTopologySpreadConstraints(constraints []core.TopologySpreadConstraint, fldPath *field.Path, opts PodValidationOptions) field.ErrorList {
	allErrs := field.ErrorList{}

	for i, constraint := range constraints {
		subFldPath := fldPath.Index(i)
		if err := ValidateMaxSkew(subFldPath.Child("maxSkew"), constraint.MaxSkew); err != nil {
			allErrs = append(allErrs, err)
		}
		if err := ValidateTopologyKey(subFldPath.Child("topologyKey"), constraint.TopologyKey); err != nil {
			allErrs = append(allErrs, err)
		}
		if err := ValidateWhenUnsatisfiable(subFldPath.Child("whenUnsatisfiable"), constraint.WhenUnsatisfiable); err != nil {
			allErrs = append(allErrs, err)
		}
		// tuple {topologyKey, whenUnsatisfiable} denotes one kind of spread constraint
		if err := ValidateSpreadConstraintNotRepeat(subFldPath.Child("{topologyKey, whenUnsatisfiable}"), constraint, constraints[i+1:]); err != nil {
			allErrs = append(allErrs, err)
		}
		allErrs = append(allErrs, validateMinDomains(subFldPath.Child("minDomains"), constraint.MinDomains, constraint.WhenUnsatisfiable)...)
		if err := validateNodeInclusionPolicy(subFldPath.Child("nodeAffinityPolicy"), constraint.NodeAffinityPolicy); err != nil {
			allErrs = append(allErrs, err)
		}
		if err := validateNodeInclusionPolicy(subFldPath.Child("nodeTaintsPolicy"), constraint.NodeTaintsPolicy); err != nil {
			allErrs = append(allErrs, err)
		}
		allErrs = append(allErrs, validateMatchLabelKeysInTopologySpread(subFldPath.Child("matchLabelKeys"), constraint.MatchLabelKeys, constraint.LabelSelector)...)
		if !opts.AllowInvalidTopologySpreadConstraintLabelSelector {
			allErrs = append(allErrs, unversionedvalidation.ValidateLabelSelector(constraint.LabelSelector, unversionedvalidation.LabelSelectorValidationOptions{AllowInvalidLabelValueInSelector: false}, subFldPath.Child("labelSelector"))...)
		}
	}

	return allErrs
}

// ValidateMaxSkew tests that the argument is a valid MaxSkew.
func ValidateMaxSkew(fldPath *field.Path, maxSkew int32) *field.Error {
	if maxSkew <= 0 {
		return field.Invalid(fldPath, maxSkew, isNotPositiveErrorMsg)
	}
	return nil
}

// validateMinDomains tests that the argument is a valid MinDomains.
func validateMinDomains(fldPath *field.Path, minDomains *int32, action core.UnsatisfiableConstraintAction) field.ErrorList {
	if minDomains == nil {
		return nil
	}
	var allErrs field.ErrorList
	if *minDomains <= 0 {
		allErrs = append(allErrs, field.Invalid(fldPath, minDomains, isNotPositiveErrorMsg))
	}
	// When MinDomains is non-nil, whenUnsatisfiable must be DoNotSchedule.
	if action != core.DoNotSchedule {
		allErrs = append(allErrs, field.Invalid(fldPath, minDomains, fmt.Sprintf("can only use minDomains if whenUnsatisfiable=%s, not %s", core.DoNotSchedule, action)))
	}
	return allErrs
}

// ValidateTopologyKey tests that the argument is a valid TopologyKey.
func ValidateTopologyKey(fldPath *field.Path, topologyKey string) *field.Error {
	if len(topologyKey) == 0 {
		return field.Required(fldPath, "can not be empty")
	}
	return nil
}

// ValidateWhenUnsatisfiable tests that the argument is a valid UnsatisfiableConstraintAction.
func ValidateWhenUnsatisfiable(fldPath *field.Path, action core.UnsatisfiableConstraintAction) *field.Error {
	if !supportedScheduleActions.Has(action) {
		return field.NotSupported(fldPath, action, sets.List(supportedScheduleActions))
	}
	return nil
}

// ValidateSpreadConstraintNotRepeat tests that if `constraint` duplicates with `existingConstraintPairs`
// on TopologyKey and WhenUnsatisfiable fields.
func ValidateSpreadConstraintNotRepeat(fldPath *field.Path, constraint core.TopologySpreadConstraint, restingConstraints []core.TopologySpreadConstraint) *field.Error {
	for _, restingConstraint := range restingConstraints {
		if constraint.TopologyKey == restingConstraint.TopologyKey &&
			constraint.WhenUnsatisfiable == restingConstraint.WhenUnsatisfiable {
			return field.Duplicate(fldPath, fmt.Sprintf("{%v, %v}", constraint.TopologyKey, constraint.WhenUnsatisfiable))
		}
	}
	return nil
}

var (
	supportedPodTopologySpreadNodePolicies = sets.New(core.NodeInclusionPolicyIgnore, core.NodeInclusionPolicyHonor)
)

// validateNodeAffinityPolicy tests that the argument is a valid NodeInclusionPolicy.
func validateNodeInclusionPolicy(fldPath *field.Path, policy *core.NodeInclusionPolicy) *field.Error {
	if policy == nil {
		return nil
	}

	if !supportedPodTopologySpreadNodePolicies.Has(*policy) {
		return field.NotSupported(fldPath, policy, sets.List(supportedPodTopologySpreadNodePolicies))
	}
	return nil
}

// validateMatchLabelKeysAndMismatchLabelKeys checks if both matchLabelKeys and mismatchLabelKeys are valid.
// - validate that all matchLabelKeys and mismatchLabelKeys are valid label names.
// - validate that the user doens't specify the same key in both matchLabelKeys and labelSelector.
// - validate that any matchLabelKeys are not duplicated with mismatchLabelKeys.
func validateMatchLabelKeysAndMismatchLabelKeys(fldPath *field.Path, matchLabelKeys, mismatchLabelKeys []string, labelSelector *metav1.LabelSelector) field.ErrorList {
	var allErrs field.ErrorList
	// 1. validate that all matchLabelKeys and mismatchLabelKeys are valid label names.
	allErrs = append(allErrs, validateLabelKeys(fldPath.Child("matchLabelKeys"), matchLabelKeys, labelSelector)...)
	allErrs = append(allErrs, validateLabelKeys(fldPath.Child("mismatchLabelKeys"), mismatchLabelKeys, labelSelector)...)

	// 2. validate that the user doens't specify the same key in both matchLabelKeys and labelSelector.
	// It doesn't make sense to have the labelselector with the key specified in matchLabelKeys
	// because the matchLabelKeys will be `In` labelSelector which matches with only one value in the key
	// and we cannot make any further filtering with that key.
	// On the other hand, we may want to have labelSelector with the key specified in mismatchLabelKeys.
	// because the mismatchLabelKeys will be `NotIn` labelSelector
	// and we may want to filter Pods further with other labelSelector with that key.

	// labelKeysMap is keyed by label key and valued by the index of label key in labelKeys.
	if labelSelector != nil {
		labelKeysMap := map[string]int{}
		for i, key := range matchLabelKeys {
			labelKeysMap[key] = i
		}
		labelSelectorKeys := sets.New[string]()
		for key := range labelSelector.MatchLabels {
			labelSelectorKeys.Insert(key)
		}
		for _, matchExpression := range labelSelector.MatchExpressions {
			key := matchExpression.Key
			if i, ok := labelKeysMap[key]; ok && labelSelectorKeys.Has(key) {
				// Before validateLabelKeysWithSelector is called, the labelSelector has already got the selector created from matchLabelKeys.
				// Here, we found the duplicate key in labelSelector and the key is specified in labelKeys.
				// Meaning that the same key is specified in both labelSelector and matchLabelKeys/mismatchLabelKeys.
				allErrs = append(allErrs, field.Invalid(fldPath.Index(i), key, "exists in both matchLabelKeys and labelSelector"))
			}

			labelSelectorKeys.Insert(key)
		}
	}

	// 3. validate that any matchLabelKeys are not duplicated with mismatchLabelKeys.
	mismatchLabelKeysSet := sets.New(mismatchLabelKeys...)
	for i, k := range matchLabelKeys {
		if mismatchLabelKeysSet.Has(k) {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("matchLabelKeys").Index(i), k, "exists in both matchLabelKeys and mismatchLabelKeys"))
		}
	}

	return allErrs
}

// validateMatchLabelKeysInTopologySpread tests that the elements are a valid label name and are not already included in labelSelector.
func validateMatchLabelKeysInTopologySpread(fldPath *field.Path, matchLabelKeys []string, labelSelector *metav1.LabelSelector) field.ErrorList {
	if len(matchLabelKeys) == 0 {
		return nil
	}

	var allErrs field.ErrorList
	labelSelectorKeys := sets.Set[string]{}

	if labelSelector != nil {
		for key := range labelSelector.MatchLabels {
			labelSelectorKeys.Insert(key)
		}
		for _, matchExpression := range labelSelector.MatchExpressions {
			labelSelectorKeys.Insert(matchExpression.Key)
		}
	} else {
		allErrs = append(allErrs, field.Forbidden(fldPath, "must not be specified when labelSelector is not set"))
	}

	for i, key := range matchLabelKeys {
		allErrs = append(allErrs, unversionedvalidation.ValidateLabelName(key, fldPath.Index(i))...)
		if labelSelectorKeys.Has(key) {
			allErrs = append(allErrs, field.Invalid(fldPath.Index(i), key, "exists in both matchLabelKeys and labelSelector"))
		}
	}

	return allErrs
}

// validateLabelKeys tests that the label keys are a valid label name.
// It's intended to be used for matchLabelKeys or mismatchLabelKeys.
func validateLabelKeys(fldPath *field.Path, labelKeys []string, labelSelector *metav1.LabelSelector) field.ErrorList {
	if len(labelKeys) == 0 {
		return nil
	}

	if labelSelector == nil {
		return field.ErrorList{field.Forbidden(fldPath, "must not be specified when labelSelector is not set")}
	}

	var allErrs field.ErrorList
	for i, key := range labelKeys {
		allErrs = append(allErrs, unversionedvalidation.ValidateLabelName(key, fldPath.Index(i))...)
	}

	return allErrs
}

// ValidateServiceClusterIPsRelatedFields validates .spec.ClusterIPs,,
// .spec.IPFamilies, .spec.ipFamilyPolicy.  This is exported because it is used
// during IP init and allocation.
func ValidateServiceClusterIPsRelatedFields(service *core.Service) field.ErrorList {
	// ClusterIP, ClusterIPs, IPFamilyPolicy and IPFamilies are validated prior (all must be unset) for ExternalName service
	if service.Spec.Type == core.ServiceTypeExternalName {
		return field.ErrorList{}
	}

	allErrs := field.ErrorList{}
	hasInvalidIPs := false

	specPath := field.NewPath("spec")
	clusterIPsField := specPath.Child("clusterIPs")
	ipFamiliesField := specPath.Child("ipFamilies")
	ipFamilyPolicyField := specPath.Child("ipFamilyPolicy")

	// Make sure ClusterIP and ClusterIPs are synced.  For most cases users can
	// just manage one or the other and we'll handle the rest (see PrepareFor*
	// in strategy).
	if len(service.Spec.ClusterIP) != 0 {
		// If ClusterIP is set, ClusterIPs[0] must match.
		if len(service.Spec.ClusterIPs) == 0 {
			allErrs = append(allErrs, field.Required(clusterIPsField, ""))
		} else if service.Spec.ClusterIPs[0] != service.Spec.ClusterIP {
			allErrs = append(allErrs, field.Invalid(clusterIPsField, service.Spec.ClusterIPs, "first value must match `clusterIP`"))
		}
	} else { // ClusterIP == ""
		// If ClusterIP is not set, ClusterIPs must also be unset.
		if len(service.Spec.ClusterIPs) != 0 {
			allErrs = append(allErrs, field.Invalid(clusterIPsField, service.Spec.ClusterIPs, "must be empty when `clusterIP` is not specified"))
		}
	}

	// ipfamilies stand alone validation
	// must be either IPv4 or IPv6
	seen := sets.Set[core.IPFamily]{}
	for i, ipFamily := range service.Spec.IPFamilies {
		if !supportedServiceIPFamily.Has(ipFamily) {
			allErrs = append(allErrs, field.NotSupported(ipFamiliesField.Index(i), ipFamily, sets.List(supportedServiceIPFamily)))
		}
		// no duplicate check also ensures that ipfamilies is dualstacked, in any order
		if seen.Has(ipFamily) {
			allErrs = append(allErrs, field.Duplicate(ipFamiliesField.Index(i), ipFamily))
		}
		seen.Insert(ipFamily)
	}

	// IPFamilyPolicy stand alone validation
	// note: nil is ok, defaulted in alloc check registry/core/service/*
	if service.Spec.IPFamilyPolicy != nil {
		// must have a supported value
		if !supportedServiceIPFamilyPolicy.Has(*(service.Spec.IPFamilyPolicy)) {
			allErrs = append(allErrs, field.NotSupported(ipFamilyPolicyField, service.Spec.IPFamilyPolicy, sets.List(supportedServiceIPFamilyPolicy)))
		}
	}

	// clusterIPs stand alone validation
	// valid ips with None and empty string handling
	// duplication check is done as part of DualStackvalidation below
	for i, clusterIP := range service.Spec.ClusterIPs {
		// valid at first location only. if and only if len(clusterIPs) == 1
		if i == 0 && clusterIP == core.ClusterIPNone {
			if len(service.Spec.ClusterIPs) > 1 {
				hasInvalidIPs = true
				allErrs = append(allErrs, field.Invalid(clusterIPsField, service.Spec.ClusterIPs, "'None' must be the first and only value"))
			}
			continue
		}

		// is it valid ip?
		errorMessages := validation.IsValidIP(clusterIPsField.Index(i), clusterIP)
		hasInvalidIPs = (len(errorMessages) != 0) || hasInvalidIPs
		allErrs = append(allErrs, errorMessages...)
	}

	// max two
	if len(service.Spec.ClusterIPs) > 2 {
		allErrs = append(allErrs, field.Invalid(clusterIPsField, service.Spec.ClusterIPs, "may only hold up to 2 values"))
	}

	// at this stage if there is an invalid ip or misplaced none/empty string
	// it will skew the error messages (bad index || dualstackness of already bad ips). so we
	// stop here if there are errors in clusterIPs validation
	if hasInvalidIPs {
		return allErrs
	}

	// must be dual stacked ips if they are more than one ip
	if len(service.Spec.ClusterIPs) > 1 /* meaning: it does not have a None or empty string */ {
		dualStack, err := netutils.IsDualStackIPStrings(service.Spec.ClusterIPs)
		if err != nil { // though we check for that earlier. safe > sorry
			allErrs = append(allErrs, field.InternalError(clusterIPsField, fmt.Errorf("failed to check for dual stack with error:%v", err)))
		}

		// We only support one from each IP family (i.e. max two IPs in this list).
		if !dualStack {
			allErrs = append(allErrs, field.Invalid(clusterIPsField, service.Spec.ClusterIPs, "may specify no more than one IP for each IP family"))
		}
	}

	// match clusterIPs to their families, if they were provided
	if !isHeadlessService(service) && len(service.Spec.ClusterIPs) > 0 && len(service.Spec.IPFamilies) > 0 {
		for i, ip := range service.Spec.ClusterIPs {
			if i > (len(service.Spec.IPFamilies) - 1) {
				break // no more families to check
			}

			// 4=>6
			if service.Spec.IPFamilies[i] == core.IPv4Protocol && netutils.IsIPv6String(ip) {
				allErrs = append(allErrs, field.Invalid(clusterIPsField.Index(i), ip, fmt.Sprintf("expected an IPv4 value as indicated by `ipFamilies[%v]`", i)))
			}
			// 6=>4
			if service.Spec.IPFamilies[i] == core.IPv6Protocol && !netutils.IsIPv6String(ip) {
				allErrs = append(allErrs, field.Invalid(clusterIPsField.Index(i), ip, fmt.Sprintf("expected an IPv6 value as indicated by `ipFamilies[%v]`", i)))
			}
		}
	}

	return allErrs
}

// specific validation for clusterIPs in cases of user upgrading or downgrading to/from dualstack
func validateUpgradeDowngradeClusterIPs(oldService, service *core.Service) field.ErrorList {
	allErrs := make(field.ErrorList, 0)

	// bail out early for ExternalName
	if service.Spec.Type == core.ServiceTypeExternalName || oldService.Spec.Type == core.ServiceTypeExternalName {
		return allErrs
	}
	newIsHeadless := isHeadlessService(service)
	oldIsHeadless := isHeadlessService(oldService)

	if oldIsHeadless && newIsHeadless {
		return allErrs
	}

	switch {
	// no change in ClusterIP lengths
	// compare each
	case len(oldService.Spec.ClusterIPs) == len(service.Spec.ClusterIPs):
		for i, ip := range oldService.Spec.ClusterIPs {
			if ip != service.Spec.ClusterIPs[i] {
				allErrs = append(allErrs, field.Invalid(field.NewPath("spec", "clusterIPs").Index(i), service.Spec.ClusterIPs, "may not change once set"))
			}
		}

	// something has been released (downgraded)
	case len(oldService.Spec.ClusterIPs) > len(service.Spec.ClusterIPs):
		// primary ClusterIP has been released
		if len(service.Spec.ClusterIPs) == 0 {
			allErrs = append(allErrs, field.Invalid(field.NewPath("spec", "clusterIPs").Index(0), service.Spec.ClusterIPs, "primary clusterIP can not be unset"))
		}

		// test if primary clusterIP has changed
		if len(oldService.Spec.ClusterIPs) > 0 &&
			len(service.Spec.ClusterIPs) > 0 &&
			service.Spec.ClusterIPs[0] != oldService.Spec.ClusterIPs[0] {
			allErrs = append(allErrs, field.Invalid(field.NewPath("spec", "clusterIPs").Index(0), service.Spec.ClusterIPs, "may not change once set"))
		}

		// test if secondary ClusterIP has been released. has this service been downgraded correctly?
		// user *must* set IPFamilyPolicy == SingleStack
		if len(service.Spec.ClusterIPs) == 1 {
			if service.Spec.IPFamilyPolicy == nil || *(service.Spec.IPFamilyPolicy) != core.IPFamilyPolicySingleStack {
				allErrs = append(allErrs, field.Invalid(field.NewPath("spec", "ipFamilyPolicy"), service.Spec.IPFamilyPolicy, "must be set to 'SingleStack' when releasing the secondary clusterIP"))
			}
		}
	case len(oldService.Spec.ClusterIPs) < len(service.Spec.ClusterIPs):
		// something has been added (upgraded)
		// test if primary clusterIP has changed
		if len(oldService.Spec.ClusterIPs) > 0 &&
			service.Spec.ClusterIPs[0] != oldService.Spec.ClusterIPs[0] {
			allErrs = append(allErrs, field.Invalid(field.NewPath("spec", "clusterIPs").Index(0), service.Spec.ClusterIPs, "may not change once set"))
		}
		// we don't check for Policy == RequireDualStack here since, Validation/Creation func takes care of it
	}
	return allErrs
}

// specific validation for ipFamilies in cases of user upgrading or downgrading to/from dualstack
func validateUpgradeDowngradeIPFamilies(oldService, service *core.Service) field.ErrorList {
	allErrs := make(field.ErrorList, 0)
	// bail out early for ExternalName
	if service.Spec.Type == core.ServiceTypeExternalName || oldService.Spec.Type == core.ServiceTypeExternalName {
		return allErrs
	}

	oldIsHeadless := isHeadlessService(oldService)
	newIsHeadless := isHeadlessService(service)

	// if changed to/from headless, then bail out
	if newIsHeadless != oldIsHeadless {
		return allErrs
	}
	// headless can change families
	if newIsHeadless {
		return allErrs
	}

	switch {
	case len(oldService.Spec.IPFamilies) == len(service.Spec.IPFamilies):
		// no change in ClusterIP lengths
		// compare each

		for i, ip := range oldService.Spec.IPFamilies {
			if ip != service.Spec.IPFamilies[i] {
				allErrs = append(allErrs, field.Invalid(field.NewPath("spec", "ipFamilies").Index(0), service.Spec.IPFamilies, "may not change once set"))
			}
		}

	case len(oldService.Spec.IPFamilies) > len(service.Spec.IPFamilies):
		// something has been released (downgraded)

		// test if primary ipfamily has been released
		if len(service.Spec.ClusterIPs) == 0 {
			allErrs = append(allErrs, field.Invalid(field.NewPath("spec", "ipFamilies").Index(0), service.Spec.IPFamilies, "primary ipFamily can not be unset"))
		}

		// test if primary ipFamily has changed
		if len(service.Spec.IPFamilies) > 0 &&
			service.Spec.IPFamilies[0] != oldService.Spec.IPFamilies[0] {
			allErrs = append(allErrs, field.Invalid(field.NewPath("spec", "ipFamilies").Index(0), service.Spec.ClusterIPs, "may not change once set"))
		}

		// test if secondary IPFamily has been released. has this service been downgraded correctly?
		// user *must* set IPFamilyPolicy == SingleStack
		if len(service.Spec.IPFamilies) == 1 {
			if service.Spec.IPFamilyPolicy == nil || *(service.Spec.IPFamilyPolicy) != core.IPFamilyPolicySingleStack {
				allErrs = append(allErrs, field.Invalid(field.NewPath("spec", "ipFamilyPolicy"), service.Spec.IPFamilyPolicy, "must be set to 'SingleStack' when releasing the secondary ipFamily"))
			}
		}
	case len(oldService.Spec.IPFamilies) < len(service.Spec.IPFamilies):
		// something has been added (upgraded)

		// test if primary ipFamily has changed
		if len(oldService.Spec.IPFamilies) > 0 &&
			len(service.Spec.IPFamilies) > 0 &&
			service.Spec.IPFamilies[0] != oldService.Spec.IPFamilies[0] {
			allErrs = append(allErrs, field.Invalid(field.NewPath("spec", "ipFamilies").Index(0), service.Spec.ClusterIPs, "may not change once set"))
		}
		// we don't check for Policy == RequireDualStack here since, Validation/Creation func takes care of it
	}
	return allErrs
}

func isHeadlessService(service *core.Service) bool {
	return service != nil &&
		len(service.Spec.ClusterIPs) == 1 &&
		service.Spec.ClusterIPs[0] == core.ClusterIPNone
}

// validateLoadBalancerClassField validation for loadBalancerClass
func validateLoadBalancerClassField(oldService, service *core.Service) field.ErrorList {
	allErrs := make(field.ErrorList, 0)
	if oldService != nil {
		// validate update op
		if isTypeLoadBalancer(oldService) && isTypeLoadBalancer(service) {
			// old and new are both LoadBalancer
			if !sameLoadBalancerClass(oldService, service) {
				// can't change loadBalancerClass
				allErrs = append(allErrs, field.Invalid(field.NewPath("spec", "loadBalancerClass"), service.Spec.LoadBalancerClass, "may not change once set"))
			}
		}
	}

	if isTypeLoadBalancer(service) {
		// check LoadBalancerClass format
		if service.Spec.LoadBalancerClass != nil {
			allErrs = append(allErrs, ValidateQualifiedName(*service.Spec.LoadBalancerClass, field.NewPath("spec", "loadBalancerClass"))...)
		}
	} else {
		// check if LoadBalancerClass set for non LoadBalancer type of service
		if service.Spec.LoadBalancerClass != nil {
			allErrs = append(allErrs, field.Forbidden(field.NewPath("spec", "loadBalancerClass"), "may only be used when `type` is 'LoadBalancer'"))
		}
	}
	return allErrs
}

// isTypeLoadBalancer tests service type is loadBalancer or not
func isTypeLoadBalancer(service *core.Service) bool {
	return service.Spec.Type == core.ServiceTypeLoadBalancer
}

// sameLoadBalancerClass check two services have the same loadBalancerClass or not
func sameLoadBalancerClass(oldService, service *core.Service) bool {
	if oldService.Spec.LoadBalancerClass == nil && service.Spec.LoadBalancerClass == nil {
		return true
	}
	if oldService.Spec.LoadBalancerClass == nil || service.Spec.LoadBalancerClass == nil {
		return false
	}
	return *oldService.Spec.LoadBalancerClass == *service.Spec.LoadBalancerClass
}

func ValidatePodAffinityTermSelector(podAffinityTerm core.PodAffinityTerm, allowInvalidLabelValueInSelector bool, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	labelSelectorValidationOptions := unversionedvalidation.LabelSelectorValidationOptions{AllowInvalidLabelValueInSelector: allowInvalidLabelValueInSelector}
	allErrs = append(allErrs, unversionedvalidation.ValidateLabelSelector(podAffinityTerm.LabelSelector, labelSelectorValidationOptions, fldPath.Child("labelSelector"))...)
	allErrs = append(allErrs, unversionedvalidation.ValidateLabelSelector(podAffinityTerm.NamespaceSelector, labelSelectorValidationOptions, fldPath.Child("namespaceSelector"))...)
	return allErrs
}

var betaToGALabel = map[string]string{
	v1.LabelFailureDomainBetaZone:   v1.LabelTopologyZone,
	v1.LabelFailureDomainBetaRegion: v1.LabelTopologyRegion,
	kubeletapis.LabelOS:             v1.LabelOSStable,
	kubeletapis.LabelArch:           v1.LabelArchStable,
	v1.LabelInstanceType:            v1.LabelInstanceTypeStable,
}

var (
	maskNodeSelectorLabelChangeEqualities     conversion.Equalities
	initMaskNodeSelectorLabelChangeEqualities sync.Once
)

func getMaskNodeSelectorLabelChangeEqualities() conversion.Equalities {
	initMaskNodeSelectorLabelChangeEqualities.Do(func() {
		var eqs = apiequality.Semantic.Copy()
		err := eqs.AddFunc(
			func(newReq, oldReq core.NodeSelectorRequirement) bool {
				// allow newReq to change to a GA key
				if oldReq.Key != newReq.Key && betaToGALabel[oldReq.Key] == newReq.Key {
					oldReq.Key = newReq.Key // +k8s:verify-mutation:reason=clone
				}
				return apiequality.Semantic.DeepEqual(newReq, oldReq)
			},
		)
		if err != nil {
			panic(fmt.Errorf("failed to instantiate semantic equalities: %w", err))
		}
		maskNodeSelectorLabelChangeEqualities = eqs
	})
	return maskNodeSelectorLabelChangeEqualities
}

func validatePvNodeAffinity(newPvNodeAffinity, oldPvNodeAffinity *core.VolumeNodeAffinity, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	if !getMaskNodeSelectorLabelChangeEqualities().DeepEqual(newPvNodeAffinity, oldPvNodeAffinity) {
		allErrs = append(allErrs, field.Invalid(fldPath, newPvNodeAffinity, fieldImmutableErrorMsg+", except for updating from beta label to GA"))
	}
	return allErrs
}

func validateNodeSelectorMutation(fldPath *field.Path, newNodeSelector, oldNodeSelector map[string]string) field.ErrorList {
	var allErrs field.ErrorList

	// Validate no existing node selectors were deleted or mutated.
	for k, v1 := range oldNodeSelector {
		if v2, ok := newNodeSelector[k]; !ok || v1 != v2 {
			allErrs = append(allErrs, field.Invalid(fldPath, newNodeSelector, "only additions to spec.nodeSelector are allowed (no mutations or deletions)"))
			return allErrs
		}
	}
	return allErrs
}

func validateNodeAffinityMutation(nodeAffinityPath *field.Path, newNodeAffinity, oldNodeAffinity *core.NodeAffinity) field.ErrorList {
	var allErrs field.ErrorList
	// If old node affinity was nil, anything can be set.
	if oldNodeAffinity == nil || oldNodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		return allErrs
	}

	oldTerms := oldNodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	var newTerms []core.NodeSelectorTerm
	if newNodeAffinity != nil && newNodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution != nil {
		newTerms = newNodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	}

	// If there are no old terms, we can set the new terms to anything.
	// If there are old terms, we cannot add any new ones.
	if len(oldTerms) > 0 && len(oldTerms) != len(newTerms) {
		return append(allErrs, field.Invalid(nodeAffinityPath.Child("requiredDuringSchedulingIgnoredDuringExecution").Child("nodeSelectorTerms"), newTerms, "no additions/deletions to non-empty NodeSelectorTerms list are allowed"))
	}

	// For requiredDuringSchedulingIgnoredDuringExecution, if old NodeSelectorTerms
	// was empty, anything can be set. If non-empty, only additions of NodeSelectorRequirements
	// to matchExpressions or fieldExpressions are allowed.
	for i := range oldTerms {
		if !validateNodeSelectorTermHasOnlyAdditions(newTerms[i], oldTerms[i]) {
			allErrs = append(allErrs, field.Invalid(nodeAffinityPath.Child("requiredDuringSchedulingIgnoredDuringExecution").Child("nodeSelectorTerms").Index(i), newTerms[i], "only additions are allowed (no mutations or deletions)"))
		}
	}
	return allErrs
}

func validateNodeSelectorTermHasOnlyAdditions(newTerm, oldTerm core.NodeSelectorTerm) bool {
	if len(oldTerm.MatchExpressions) == 0 && len(oldTerm.MatchFields) == 0 {
		if len(newTerm.MatchExpressions) > 0 || len(newTerm.MatchFields) > 0 {
			return false
		}
	}

	// Validate MatchExpressions only has additions (no deletions or mutations)
	if l := len(oldTerm.MatchExpressions); l > 0 {
		if len(newTerm.MatchExpressions) < l {
			return false
		}
		if !apiequality.Semantic.DeepEqual(newTerm.MatchExpressions[:l], oldTerm.MatchExpressions) {
			return false
		}
	}
	// Validate MatchFields only has additions (no deletions or mutations)
	if l := len(oldTerm.MatchFields); l > 0 {
		if len(newTerm.MatchFields) < l {
			return false
		}
		if !apiequality.Semantic.DeepEqual(newTerm.MatchFields[:l], oldTerm.MatchFields) {
			return false
		}
	}
	return true
}

var validSupplementalGroupsPolicies = sets.New(core.SupplementalGroupsPolicyMerge, core.SupplementalGroupsPolicyStrict)

func validateSupplementalGroupsPolicy(supplementalGroupsPolicy *core.SupplementalGroupsPolicy, fldPath *field.Path) field.ErrorList {
	allErrors := field.ErrorList{}
	if !validSupplementalGroupsPolicies.Has(*supplementalGroupsPolicy) {
		allErrors = append(allErrors, field.NotSupported(fldPath, supplementalGroupsPolicy, sets.List(validSupplementalGroupsPolicies)))
	}
	return allErrors
}

func validateContainerStatusUsers(containerStatuses []core.ContainerStatus, fldPath *field.Path, podOS *core.PodOS) field.ErrorList {
	allErrors := field.ErrorList{}
	osName := core.Linux
	if podOS != nil {
		osName = podOS.Name
	}
	for i, containerStatus := range containerStatuses {
		if containerStatus.User == nil {
			continue
		}
		containerUser := containerStatus.User
		switch osName {
		case core.Windows:
			if containerUser.Linux != nil {
				allErrors = append(allErrors, field.Forbidden(fldPath.Index(i).Child("linux"), "cannot be set for a windows pod"))
			}
		case core.Linux:
			allErrors = append(allErrors, validateLinuxContainerUser(containerUser.Linux, fldPath.Index(i).Child("linux"))...)
		}
	}
	return allErrors
}

func validateContainerStatusNoAllocatedResourcesStatus(containerStatuses []core.ContainerStatus, fldPath *field.Path) field.ErrorList {
	allErrors := field.ErrorList{}

	for i, containerStatus := range containerStatuses {
		if len(containerStatus.AllocatedResourcesStatus) == 0 {
			continue
		}
		allErrors = append(allErrors, field.Forbidden(fldPath.Index(i).Child("allocatedResourcesStatus"), "must not be specified in container status"))
	}

	return allErrors
}

// validateContainerStatusAllocatedResourcesStatus iterate the allocated resources health and validate:
// - resourceName matches one of resources in container's resource requirements
// - resourceID is not empty and unique
func validateContainerStatusAllocatedResourcesStatus(containerStatuses []core.ContainerStatus, fldPath *field.Path, containers []core.Container) field.ErrorList {
	allErrors := field.ErrorList{}

	for i, containerStatus := range containerStatuses {
		if containerStatus.AllocatedResourcesStatus == nil {
			continue
		}

		allocatedResources := containerStatus.AllocatedResourcesStatus
		for j, allocatedResource := range allocatedResources {
			var container core.Container
			containerFound := false
			// get container by name
			for _, c := range containers {
				if c.Name == containerStatus.Name {
					containerFound = true
					container = c
					break
				}
			}

			// ignore missing container, see https://github.com/kubernetes/kubernetes/issues/124915
			if containerFound {
				found := false
				var errorStr string

				if strings.HasPrefix(string(allocatedResource.Name), "claim:") {
					// assume it is a claim name

					errorStr = "must match one of the container's resource claims in a format 'claim:<claimName>/<request>' or 'claim:<claimName>' if request is empty"

					for _, c := range container.Resources.Claims {
						name := "claim:" + c.Name
						if c.Request != "" {
							name += "/" + c.Request
						}

						if name == string(allocatedResource.Name) {
							found = true
							break
						}
					}

				} else {
					// assume it is a resource name

					errorStr = "must match one of the container's resource requests"

					for resourceName := range container.Resources.Requests {
						if resourceName == allocatedResource.Name {
							found = true
							break
						}
					}
				}
				if !found {
					allErrors = append(allErrors, field.Invalid(fldPath.Index(i).Child("allocatedResourcesStatus").Index(j).Child("name"), allocatedResource.Name, errorStr))
				}
			}

			uniqueResources := sets.New[core.ResourceID]()
			// check resource IDs are unique
			for k, r := range allocatedResource.Resources {

				var supportedResourceHealthValues = sets.New(
					core.ResourceHealthStatusHealthy,
					core.ResourceHealthStatusUnhealthy,
					core.ResourceHealthStatusUnknown)

				if !supportedResourceHealthValues.Has(r.Health) {
					allErrors = append(allErrors, field.NotSupported(fldPath.Index(i).Child("allocatedResourcesStatus").Index(j).Child("resources").Index(k).Child("health"), r.Health, sets.List(supportedResourceHealthValues)))
				}

				if uniqueResources.Has(r.ResourceID) {
					allErrors = append(allErrors, field.Duplicate(fldPath.Index(i).Child("allocatedResourcesStatus").Index(j).Child("resources").Index(k).Child("resourceID"), r.ResourceID))
				} else {
					uniqueResources.Insert(r.ResourceID)
				}
			}
		}
	}

	return allErrors
}

func validateLinuxContainerUser(linuxContainerUser *core.LinuxContainerUser, fldPath *field.Path) field.ErrorList {
	allErrors := field.ErrorList{}
	if linuxContainerUser == nil {
		return allErrors
	}
	for _, msg := range validation.IsValidUserID(linuxContainerUser.UID) {
		allErrors = append(allErrors, field.Invalid(fldPath.Child("uid"), linuxContainerUser.UID, msg))
	}

	for _, msg := range validation.IsValidGroupID(linuxContainerUser.GID) {
		allErrors = append(allErrors, field.Invalid(fldPath.Child("gid"), linuxContainerUser.GID, msg))
	}
	for g, gid := range linuxContainerUser.SupplementalGroups {
		for _, msg := range validation.IsValidGroupID(gid) {
			allErrors = append(allErrors, field.Invalid(fldPath.Child("supplementalGroups").Index(g), gid, msg))
		}
	}
	return allErrors
}

func validateImageVolumeSource(imageVolume *core.ImageVolumeSource, fldPath *field.Path, opts PodValidationOptions) field.ErrorList {
	allErrs := field.ErrorList{}
	if opts.ResourceIsPod && len(imageVolume.Reference) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("reference"), ""))
	}
	allErrs = append(allErrs, validatePullPolicy(imageVolume.PullPolicy, fldPath.Child("pullPolicy"))...)
	return allErrs
}

// isRestartableInitContainer returns true if the container has ContainerRestartPolicyAlways.
func isRestartableInitContainer(initContainer *core.Container) bool {
	if initContainer == nil || initContainer.RestartPolicy == nil {
		return false
	}
	return *initContainer.RestartPolicy == core.ContainerRestartPolicyAlways
}
