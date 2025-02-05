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

package validation

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apimachineryvalidation "k8s.io/apimachinery/pkg/api/validation"
	metav1validation "k8s.io/apimachinery/pkg/apis/meta/v1/validation"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apiserver/pkg/cel"
	"k8s.io/apiserver/pkg/cel/environment"
	dracel "k8s.io/dynamic-resource-allocation/cel"
	"k8s.io/dynamic-resource-allocation/structured"
	corevalidation "k8s.io/kubernetes/pkg/apis/core/validation"
	"k8s.io/kubernetes/pkg/apis/resource"
	netutils "k8s.io/utils/net"
)

var (
	// validateResourceDriverName reuses the validation of a CSI driver because
	// the allowed values are exactly the same.
	validateDriverName      = corevalidation.ValidateCSIDriverName
	validateDeviceName      = corevalidation.ValidateDNS1123Label
	validateDeviceClassName = corevalidation.ValidateDNS1123Subdomain
	validateRequestName     = corevalidation.ValidateDNS1123Label
)

func validatePoolName(name string, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	if name == "" {
		allErrs = append(allErrs, field.Required(fldPath, ""))
	} else {
		if len(name) > resource.PoolNameMaxLength {
			allErrs = append(allErrs, field.TooLong(fldPath, "" /*unused*/, resource.PoolNameMaxLength))
		}
		parts := strings.Split(name, "/")
		for _, part := range parts {
			allErrs = append(allErrs, corevalidation.ValidateDNS1123Subdomain(part, fldPath)...)
		}
	}
	return allErrs
}

// ValidateResourceClaim validates a ResourceClaim.
func ValidateResourceClaim(resourceClaim *resource.ResourceClaim) field.ErrorList {
	allErrs := corevalidation.ValidateObjectMeta(&resourceClaim.ObjectMeta, true, corevalidation.ValidateResourceClaimName, field.NewPath("metadata"))
	allErrs = append(allErrs, validateResourceClaimSpec(&resourceClaim.Spec, field.NewPath("spec"), false)...)
	return allErrs
}

// ValidateResourceClaimUpdate tests if an update to ResourceClaim is valid.
func ValidateResourceClaimUpdate(resourceClaim, oldClaim *resource.ResourceClaim) field.ErrorList {
	allErrs := corevalidation.ValidateObjectMetaUpdate(&resourceClaim.ObjectMeta, &oldClaim.ObjectMeta, field.NewPath("metadata"))
	allErrs = append(allErrs, apimachineryvalidation.ValidateImmutableField(resourceClaim.Spec, oldClaim.Spec, field.NewPath("spec"))...)
	// Because the spec is immutable, all CEL expressions in it must have been stored.
	// If the user tries an update, this is not true and checking is less strict, but
	// as there are errors, it doesn't matter.
	allErrs = append(allErrs, validateResourceClaimSpec(&resourceClaim.Spec, field.NewPath("spec"), true)...)
	return allErrs
}

// ValidateResourceClaimStatusUpdate tests if an update to the status of a ResourceClaim is valid.
func ValidateResourceClaimStatusUpdate(resourceClaim, oldClaim *resource.ResourceClaim) field.ErrorList {
	allErrs := corevalidation.ValidateObjectMetaUpdate(&resourceClaim.ObjectMeta, &oldClaim.ObjectMeta, field.NewPath("metadata"))
	requestNames := gatherRequestNames(&resourceClaim.Spec.Devices)
	allErrs = append(allErrs, validateResourceClaimStatusUpdate(&resourceClaim.Status, &oldClaim.Status, resourceClaim.DeletionTimestamp != nil, requestNames, field.NewPath("status"))...)
	return allErrs
}

func validateResourceClaimSpec(spec *resource.ResourceClaimSpec, fldPath *field.Path, stored bool) field.ErrorList {
	allErrs := field.ErrorList{}
	allErrs = append(allErrs, validateDeviceClaim(&spec.Devices, fldPath.Child("devices"), stored)...)
	return allErrs
}

func validateDeviceClaim(deviceClaim *resource.DeviceClaim, fldPath *field.Path, stored bool) field.ErrorList {
	allErrs := field.ErrorList{}
	requestNames := gatherRequestNames(deviceClaim)
	allErrs = append(allErrs, validateSet(deviceClaim.Requests, resource.DeviceRequestsMaxSize,
		func(request resource.DeviceRequest, fldPath *field.Path) field.ErrorList {
			return validateDeviceRequest(request, fldPath, stored)
		},
		func(request resource.DeviceRequest) (string, string) {
			return request.Name, "name"
		},
		fldPath.Child("requests"))...)
	allErrs = append(allErrs, validateSlice(deviceClaim.Constraints, resource.DeviceConstraintsMaxSize,
		func(constraint resource.DeviceConstraint, fldPath *field.Path) field.ErrorList {
			return validateDeviceConstraint(constraint, fldPath, requestNames)
		}, fldPath.Child("constraints"))...)
	allErrs = append(allErrs, validateSlice(deviceClaim.Config, resource.DeviceConfigMaxSize,
		func(config resource.DeviceClaimConfiguration, fldPath *field.Path) field.ErrorList {
			return validateDeviceClaimConfiguration(config, fldPath, requestNames, stored)
		}, fldPath.Child("config"))...)
	return allErrs
}

type requestNames map[string]sets.Set[string]

func (r requestNames) Has(s string) bool {
	segments := strings.Split(s, "/")
	// If there are more than one / in the string, we
	// know there can't be any match.
	if len(segments) > 2 {
		return false
	}
	// If the first segment doesn't have a match, we
	// don't need to check the other one.
	subRequestNames, found := r[segments[0]]
	if !found {
		return false
	}
	if len(segments) == 1 {
		return true
	}
	// If the first segment matched and we have another one,
	// check for a match for that too.
	return subRequestNames.Has(segments[1])
}

func gatherRequestNames(deviceClaim *resource.DeviceClaim) requestNames {
	requestNames := make(requestNames)
	for _, request := range deviceClaim.Requests {
		if len(request.FirstAvailable) == 0 {
			requestNames[request.Name] = nil
			continue
		}
		subRequestNames := sets.New[string]()
		for _, subRequest := range request.FirstAvailable {
			subRequestNames.Insert(subRequest.Name)
		}
		requestNames[request.Name] = subRequestNames
	}
	return requestNames
}

func gatherAllocatedDevices(allocationResult *resource.DeviceAllocationResult) sets.Set[structured.DeviceID] {
	allocatedDevices := sets.New[structured.DeviceID]()
	for _, result := range allocationResult.Results {
		deviceID := structured.MakeDeviceID(result.Driver, result.Pool, result.Device)
		allocatedDevices.Insert(deviceID)
	}
	return allocatedDevices
}

func validateDeviceRequest(request resource.DeviceRequest, fldPath *field.Path, stored bool) field.ErrorList {
	allErrs := validateRequestName(request.Name, fldPath.Child("name"))
	if len(request.FirstAvailable) > 0 {
		if request.DeviceClassName != "" {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("deviceClassName"), request.DeviceClassName, "must not be specified when firstAvailable is set"))
		}
		if request.Selectors != nil {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("selectors"), request.Selectors, "must not be specified when firstAvailable is set"))
		}
		if request.AllocationMode != "" {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("allocationMode"), request.AllocationMode, "must not be specified when firstAvailable is set"))
		}
		if request.Count != 0 {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("count"), request.Count, "must not be specified when firstAvailable is set"))
		}
		if request.AdminAccess != nil {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("adminAccess"), request.AdminAccess, "must not be specified when firstAvailable is set"))
		}
		allErrs = append(allErrs, validateSet(request.FirstAvailable, resource.FirstAvailableDeviceRequestMaxSize,
			func(subRequest resource.DeviceSubRequest, fldPath *field.Path) field.ErrorList {
				return validateDeviceSubRequest(subRequest, fldPath, stored)
			},
			func(subRequest resource.DeviceSubRequest) (string, string) {
				return subRequest.Name, "name"
			},
			fldPath.Child("firstAvailable"))...)
		return allErrs
	}
	allErrs = append(allErrs, validateDeviceClass(request.DeviceClassName, fldPath.Child("deviceClassName"))...)
	allErrs = append(allErrs, validateSelectorSlice(request.Selectors, fldPath.Child("selectors"), stored)...)
	allErrs = append(allErrs, validateDeviceAllocationMode(request.AllocationMode, request.Count, fldPath.Child("allocationMode"), fldPath.Child("count"))...)
	return allErrs
}

func validateDeviceSubRequest(subRequest resource.DeviceSubRequest, fldPath *field.Path, stored bool) field.ErrorList {
	allErrs := validateRequestName(subRequest.Name, fldPath.Child("name"))
	allErrs = append(allErrs, validateDeviceClass(subRequest.DeviceClassName, fldPath.Child("deviceClassName"))...)
	allErrs = append(allErrs, validateSelectorSlice(subRequest.Selectors, fldPath.Child("selectors"), stored)...)
	allErrs = append(allErrs, validateDeviceAllocationMode(subRequest.AllocationMode, subRequest.Count, fldPath.Child("allocationMode"), fldPath.Child("count"))...)
	return allErrs
}

func validateDeviceAllocationMode(deviceAllocationMode resource.DeviceAllocationMode, count int64, allocModeFldPath, countFldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	switch deviceAllocationMode {
	case resource.DeviceAllocationModeAll:
		if count != 0 {
			allErrs = append(allErrs, field.Invalid(countFldPath, count, fmt.Sprintf("must not be specified when allocationMode is '%s'", deviceAllocationMode)))
		}
	case resource.DeviceAllocationModeExactCount:
		if count <= 0 {
			allErrs = append(allErrs, field.Invalid(countFldPath, count, "must be greater than zero"))
		}
	default:
		allErrs = append(allErrs, field.NotSupported(allocModeFldPath, deviceAllocationMode, []resource.DeviceAllocationMode{resource.DeviceAllocationModeAll, resource.DeviceAllocationModeExactCount}))
	}
	return allErrs
}

func validateDeviceClass(deviceClass string, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	if deviceClass == "" {
		allErrs = append(allErrs, field.Required(fldPath, ""))
	} else {
		allErrs = append(allErrs, validateDeviceClassName(deviceClass, fldPath)...)
	}
	return allErrs
}

func validateSelectorSlice(selectors []resource.DeviceSelector, fldPath *field.Path, stored bool) field.ErrorList {
	return validateSlice(selectors, resource.DeviceSelectorsMaxSize,
		func(selector resource.DeviceSelector, fldPath *field.Path) field.ErrorList {
			return validateSelector(selector, fldPath, stored)
		},
		fldPath)
}

func validateSelector(selector resource.DeviceSelector, fldPath *field.Path, stored bool) field.ErrorList {
	var allErrs field.ErrorList
	if selector.CEL == nil {
		allErrs = append(allErrs, field.Required(fldPath.Child("cel"), ""))
	} else {
		allErrs = append(allErrs, validateCELSelector(*selector.CEL, fldPath.Child("cel"), stored)...)
	}
	return allErrs
}

func validateCELSelector(celSelector resource.CELDeviceSelector, fldPath *field.Path, stored bool) field.ErrorList {
	var allErrs field.ErrorList
	envType := environment.NewExpressions
	if stored {
		envType = environment.StoredExpressions
	}
	if len(celSelector.Expression) > resource.CELSelectorExpressionMaxLength {
		allErrs = append(allErrs, field.TooLong(fldPath.Child("expression"), "" /*unused*/, resource.CELSelectorExpressionMaxLength))
		// Don't bother compiling too long expressions.
		return allErrs
	}

	result := dracel.GetCompiler().CompileCELExpression(celSelector.Expression, dracel.Options{EnvType: &envType})
	if result.Error != nil {
		allErrs = append(allErrs, convertCELErrorToValidationError(fldPath.Child("expression"), celSelector.Expression, result.Error))
	} else if result.MaxCost > resource.CELSelectorExpressionMaxCost {
		allErrs = append(allErrs, field.Forbidden(fldPath.Child("expression"), "too complex, exceeds cost limit"))
	}

	return allErrs
}

func convertCELErrorToValidationError(fldPath *field.Path, expression string, err error) *field.Error {
	var celErr *cel.Error
	if errors.As(err, &celErr) {
		switch celErr.Type {
		case cel.ErrorTypeRequired:
			return field.Required(fldPath, celErr.Detail)
		case cel.ErrorTypeInvalid:
			return field.Invalid(fldPath, expression, celErr.Detail)
		case cel.ErrorTypeInternal:
			return field.InternalError(fldPath, celErr)
		}
	}
	return field.InternalError(fldPath, fmt.Errorf("unsupported error type: %w", err))
}

func validateDeviceConstraint(constraint resource.DeviceConstraint, fldPath *field.Path, requestNames requestNames) field.ErrorList {
	var allErrs field.ErrorList
	allErrs = append(allErrs, validateSet(constraint.Requests, resource.DeviceRequestsMaxSize,
		func(name string, fldPath *field.Path) field.ErrorList {
			return validateRequestNameRef(name, fldPath, requestNames)
		},
		stringKey, fldPath.Child("requests"))...)
	if constraint.MatchAttribute == nil {
		allErrs = append(allErrs, field.Required(fldPath.Child("matchAttribute"), ""))
	} else {
		allErrs = append(allErrs, validateFullyQualifiedName(*constraint.MatchAttribute, fldPath.Child("matchAttribute"))...)
	}
	return allErrs
}

func validateDeviceClaimConfiguration(config resource.DeviceClaimConfiguration, fldPath *field.Path, requestNames requestNames, stored bool) field.ErrorList {
	var allErrs field.ErrorList
	allErrs = append(allErrs, validateSet(config.Requests, resource.DeviceRequestsMaxSize,
		func(name string, fldPath *field.Path) field.ErrorList {
			return validateRequestNameRef(name, fldPath, requestNames)
		}, stringKey, fldPath.Child("requests"))...)
	allErrs = append(allErrs, validateDeviceConfiguration(config.DeviceConfiguration, fldPath, stored)...)
	return allErrs
}

func validateRequestNameRef(name string, fldPath *field.Path, requestNames requestNames) field.ErrorList {
	var allErrs field.ErrorList
	segments := strings.Split(name, "/")
	if len(segments) > 2 {
		allErrs = append(allErrs, field.Invalid(fldPath, name, "must be the name of a request in the claim or the name of a request and a subrequest separated by '/'"))
		return allErrs
	}

	for i := range segments {
		allErrs = append(allErrs, validateRequestName(segments[i], fldPath)...)
	}

	if !requestNames.Has(name) {
		allErrs = append(allErrs, field.Invalid(fldPath, name, "must be the name of a request in the claim or the name of a request and a subrequest separated by '/'"))
	}
	return allErrs
}

func validateDeviceConfiguration(config resource.DeviceConfiguration, fldPath *field.Path, stored bool) field.ErrorList {
	var allErrs field.ErrorList
	if config.Opaque == nil {
		allErrs = append(allErrs, field.Required(fldPath.Child("opaque"), ""))
	} else {
		allErrs = append(allErrs, validateOpaqueConfiguration(*config.Opaque, fldPath.Child("opaque"), stored)...)
	}
	return allErrs
}

func validateOpaqueConfiguration(config resource.OpaqueDeviceConfiguration, fldPath *field.Path, stored bool) field.ErrorList {
	var allErrs field.ErrorList
	allErrs = append(allErrs, validateDriverName(config.Driver, fldPath.Child("driver"))...)
	allErrs = append(allErrs, validateRawExtension(config.Parameters, fldPath.Child("parameters"), stored)...)
	return allErrs
}

func validateResourceClaimStatusUpdate(status, oldStatus *resource.ResourceClaimStatus, claimDeleted bool, requestNames requestNames, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	allErrs = append(allErrs, validateSet(status.ReservedFor, resource.ResourceClaimReservedForMaxSize,
		validateResourceClaimUserReference,
		func(consumer resource.ResourceClaimConsumerReference) (types.UID, string) { return consumer.UID, "uid" },
		fldPath.Child("reservedFor"))...)

	var allocatedDevices sets.Set[structured.DeviceID]
	if status.Allocation != nil {
		allocatedDevices = gatherAllocatedDevices(&status.Allocation.Devices)
	}
	allErrs = append(allErrs, validateSet(status.Devices, -1,
		func(device resource.AllocatedDeviceStatus, fldPath *field.Path) field.ErrorList {
			return validateDeviceStatus(device, fldPath, allocatedDevices)
		},
		func(device resource.AllocatedDeviceStatus) (structured.DeviceID, string) {
			return structured.MakeDeviceID(device.Driver, device.Pool, device.Device), "deviceID"
		},
		fldPath.Child("devices"))...)

	// Now check for invariants that must be valid for a ResourceClaim.
	if len(status.ReservedFor) > 0 {
		if status.Allocation == nil {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("reservedFor"), "may not be specified when `allocated` is not set"))
		} else {
			// Items may be removed from ReservedFor while the claim is meant to be deallocated,
			// but not added.
			if claimDeleted {
				oldSet := sets.New(oldStatus.ReservedFor...)
				newSet := sets.New(status.ReservedFor...)
				newItems := newSet.Difference(oldSet)
				if len(newItems) > 0 {
					allErrs = append(allErrs, field.Forbidden(fldPath.Child("reservedFor"), "new entries may not be added while `deallocationRequested` or `deletionTimestamp` are set"))
				}
			}
		}
	}

	// Updates to a populated status.Allocation are not allowed.
	// Unmodified fields don't need to be validated again and,
	// in this particular case, must not be validated again because
	// validation for new results is tighter than it was before.
	if oldStatus.Allocation != nil && status.Allocation != nil {
		allErrs = append(allErrs, apimachineryvalidation.ValidateImmutableField(status.Allocation, oldStatus.Allocation, fldPath.Child("allocation"))...)
	} else if status.Allocation != nil {
		allErrs = append(allErrs, validateAllocationResult(status.Allocation, fldPath.Child("allocation"), requestNames, false)...)
	}

	return allErrs
}

func validateResourceClaimUserReference(ref resource.ResourceClaimConsumerReference, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	if ref.Resource == "" {
		allErrs = append(allErrs, field.Required(fldPath.Child("resource"), ""))
	}
	if ref.Name == "" {
		allErrs = append(allErrs, field.Required(fldPath.Child("name"), ""))
	}
	if ref.UID == "" {
		allErrs = append(allErrs, field.Required(fldPath.Child("uid"), ""))
	}
	return allErrs
}

// validateAllocationResult enforces constraints for *new* results, which in at
// least one case (admin access) are more strict than before. Therefore it
// may not be called to re-validate results which were stored earlier.
func validateAllocationResult(allocation *resource.AllocationResult, fldPath *field.Path, requestNames requestNames, stored bool) field.ErrorList {
	var allErrs field.ErrorList
	allErrs = append(allErrs, validateDeviceAllocationResult(allocation.Devices, fldPath.Child("devices"), requestNames, stored)...)
	if allocation.NodeSelector != nil {
		allErrs = append(allErrs, corevalidation.ValidateNodeSelector(allocation.NodeSelector, false, fldPath.Child("nodeSelector"))...)
	}
	return allErrs
}

func validateDeviceAllocationResult(allocation resource.DeviceAllocationResult, fldPath *field.Path, requestNames requestNames, stored bool) field.ErrorList {
	var allErrs field.ErrorList
	allErrs = append(allErrs, validateSlice(allocation.Results, resource.AllocationResultsMaxSize,
		func(result resource.DeviceRequestAllocationResult, fldPath *field.Path) field.ErrorList {
			return validateDeviceRequestAllocationResult(result, fldPath, requestNames)
		}, fldPath.Child("results"))...)
	allErrs = append(allErrs, validateSlice(allocation.Config, 2*resource.DeviceConfigMaxSize, /* class + claim */
		func(config resource.DeviceAllocationConfiguration, fldPath *field.Path) field.ErrorList {
			return validateDeviceAllocationConfiguration(config, fldPath, requestNames, stored)
		}, fldPath.Child("config"))...)

	return allErrs
}

func validateDeviceRequestAllocationResult(result resource.DeviceRequestAllocationResult, fldPath *field.Path, requestNames requestNames) field.ErrorList {
	var allErrs field.ErrorList
	allErrs = append(allErrs, validateRequestNameRef(result.Request, fldPath.Child("request"), requestNames)...)
	allErrs = append(allErrs, validateDriverName(result.Driver, fldPath.Child("driver"))...)
	allErrs = append(allErrs, validatePoolName(result.Pool, fldPath.Child("pool"))...)
	allErrs = append(allErrs, validateDeviceName(result.Device, fldPath.Child("device"))...)
	return allErrs
}

func validateDeviceAllocationConfiguration(config resource.DeviceAllocationConfiguration, fldPath *field.Path, requestNames requestNames, stored bool) field.ErrorList {
	var allErrs field.ErrorList
	allErrs = append(allErrs, validateAllocationConfigSource(config.Source, fldPath.Child("source"))...)
	allErrs = append(allErrs, validateSet(config.Requests, resource.DeviceRequestsMaxSize,
		func(name string, fldPath *field.Path) field.ErrorList {
			return validateRequestNameRef(name, fldPath, requestNames)
		}, stringKey, fldPath.Child("requests"))...)
	allErrs = append(allErrs, validateDeviceConfiguration(config.DeviceConfiguration, fldPath, stored)...)
	return allErrs
}

func validateAllocationConfigSource(source resource.AllocationConfigSource, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	switch source {
	case "":
		allErrs = append(allErrs, field.Required(fldPath, ""))
	case resource.AllocationConfigSourceClaim, resource.AllocationConfigSourceClass:
	default:
		allErrs = append(allErrs, field.NotSupported(fldPath, source, []resource.AllocationConfigSource{resource.AllocationConfigSourceClaim, resource.AllocationConfigSourceClass}))
	}
	return allErrs
}

// ValidateClass validates a DeviceClass.
func ValidateDeviceClass(class *resource.DeviceClass) field.ErrorList {
	allErrs := corevalidation.ValidateObjectMeta(&class.ObjectMeta, false, corevalidation.ValidateClassName, field.NewPath("metadata"))
	allErrs = append(allErrs, validateDeviceClassSpec(&class.Spec, nil, field.NewPath("spec"))...)
	return allErrs
}

// ValidateClassUpdate tests if an update to DeviceClass is valid.
func ValidateDeviceClassUpdate(class, oldClass *resource.DeviceClass) field.ErrorList {
	allErrs := corevalidation.ValidateObjectMetaUpdate(&class.ObjectMeta, &oldClass.ObjectMeta, field.NewPath("metadata"))
	allErrs = append(allErrs, validateDeviceClassSpec(&class.Spec, &oldClass.Spec, field.NewPath("spec"))...)
	return allErrs
}

func validateDeviceClassSpec(spec, oldSpec *resource.DeviceClassSpec, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	// If the selectors are exactly as before, we treat the CEL expressions as "stored".
	// Any change, including merely reordering selectors, triggers validation as new
	// expressions.
	stored := false
	if oldSpec != nil {
		stored = apiequality.Semantic.DeepEqual(spec.Selectors, oldSpec.Selectors)
	}
	allErrs = append(allErrs, validateSlice(spec.Selectors, resource.DeviceSelectorsMaxSize,
		func(selector resource.DeviceSelector, fldPath *field.Path) field.ErrorList {
			return validateSelector(selector, fldPath, stored)
		},
		fldPath.Child("selectors"))...)
	// Same logic as above for configs.
	if oldSpec != nil {
		stored = apiequality.Semantic.DeepEqual(spec.Config, oldSpec.Config)
	}
	allErrs = append(allErrs, validateSlice(spec.Config, resource.DeviceConfigMaxSize,
		func(config resource.DeviceClassConfiguration, fldPath *field.Path) field.ErrorList {
			return validateDeviceClassConfiguration(config, fldPath, stored)
		},
		fldPath.Child("config"))...)
	return allErrs
}

func validateDeviceClassConfiguration(config resource.DeviceClassConfiguration, fldPath *field.Path, stored bool) field.ErrorList {
	return validateDeviceConfiguration(config.DeviceConfiguration, fldPath, stored)
}

// ValidateResourceClaimTemplate validates a ResourceClaimTemplate.
func ValidateResourceClaimTemplate(template *resource.ResourceClaimTemplate) field.ErrorList {
	allErrs := corevalidation.ValidateObjectMeta(&template.ObjectMeta, true, corevalidation.ValidateResourceClaimTemplateName, field.NewPath("metadata"))
	allErrs = append(allErrs, validateResourceClaimTemplateSpec(&template.Spec, field.NewPath("spec"), false)...)
	return allErrs
}

func validateResourceClaimTemplateSpec(spec *resource.ResourceClaimTemplateSpec, fldPath *field.Path, stored bool) field.ErrorList {
	allErrs := corevalidation.ValidateTemplateObjectMeta(&spec.ObjectMeta, fldPath.Child("metadata"))
	allErrs = append(allErrs, validateResourceClaimSpec(&spec.Spec, fldPath.Child("spec"), stored)...)
	return allErrs
}

// ValidateResourceClaimTemplateUpdate tests if an update to template is valid.
func ValidateResourceClaimTemplateUpdate(template, oldTemplate *resource.ResourceClaimTemplate) field.ErrorList {
	allErrs := corevalidation.ValidateObjectMetaUpdate(&template.ObjectMeta, &oldTemplate.ObjectMeta, field.NewPath("metadata"))
	allErrs = append(allErrs, apimachineryvalidation.ValidateImmutableField(template.Spec, oldTemplate.Spec, field.NewPath("spec"))...)
	// Because the spec is immutable, all CEL expressions in it must have been stored.
	// If the user tries an update, this is not true and checking is less strict, but
	// as there are errors, it doesn't matter.
	allErrs = append(allErrs, validateResourceClaimTemplateSpec(&template.Spec, field.NewPath("spec"), true)...)
	return allErrs
}

func validateNodeName(name string, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	for _, msg := range corevalidation.ValidateNodeName(name, false) {
		allErrs = append(allErrs, field.Invalid(fldPath, name, msg))
	}
	return allErrs
}

// ValidateResourceSlice tests if a ResourceSlice object is valid.
func ValidateResourceSlice(slice *resource.ResourceSlice) field.ErrorList {
	allErrs := corevalidation.ValidateObjectMeta(&slice.ObjectMeta, false, apimachineryvalidation.NameIsDNSSubdomain, field.NewPath("metadata"))
	allErrs = append(allErrs, validateResourceSliceSpec(&slice.Spec, nil, field.NewPath("spec"))...)
	return allErrs
}

// ValidateResourceSlice tests if a ResourceSlice update is valid.
func ValidateResourceSliceUpdate(resourceSlice, oldResourceSlice *resource.ResourceSlice) field.ErrorList {
	allErrs := corevalidation.ValidateObjectMetaUpdate(&resourceSlice.ObjectMeta, &oldResourceSlice.ObjectMeta, field.NewPath("metadata"))
	allErrs = append(allErrs, validateResourceSliceSpec(&resourceSlice.Spec, &oldResourceSlice.Spec, field.NewPath("spec"))...)
	return allErrs
}

func validateResourceSliceSpec(spec, oldSpec *resource.ResourceSliceSpec, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	allErrs = append(allErrs, validateDriverName(spec.Driver, fldPath.Child("driver"))...)
	allErrs = append(allErrs, validateResourcePool(spec.Pool, fldPath.Child("pool"))...)
	if oldSpec != nil {
		allErrs = append(allErrs, apimachineryvalidation.ValidateImmutableField(spec.Pool.Name, oldSpec.Pool.Name, fldPath.Child("pool", "name"))...)
		allErrs = append(allErrs, apimachineryvalidation.ValidateImmutableField(spec.Driver, oldSpec.Driver, fldPath.Child("driver"))...)
		allErrs = append(allErrs, apimachineryvalidation.ValidateImmutableField(spec.NodeName, oldSpec.NodeName, fldPath.Child("nodeName"))...)
	}

	numNodeSelectionFields := 0
	if spec.NodeName != "" {
		numNodeSelectionFields++
		allErrs = append(allErrs, validateNodeName(spec.NodeName, fldPath.Child("nodeName"))...)
	}
	if spec.NodeSelector != nil {
		numNodeSelectionFields++
		allErrs = append(allErrs, corevalidation.ValidateNodeSelector(spec.NodeSelector, false, fldPath.Child("nodeSelector"))...)
		if len(spec.NodeSelector.NodeSelectorTerms) != 1 {
			// This additional constraint simplifies merging of different selectors
			// when devices are allocated from different slices.
			allErrs = append(allErrs, field.Invalid(fldPath.Child("nodeSelector", "nodeSelectorTerms"), spec.NodeSelector.NodeSelectorTerms, "must have exactly one node selector term"))
		}
	}
	if spec.AllNodes {
		numNodeSelectionFields++
	}
	switch numNodeSelectionFields {
	case 0:
		allErrs = append(allErrs, field.Required(fldPath, "exactly one of `nodeName`, `nodeSelector`, or `allNodes` is required"))
	case 1:
	default:
		allErrs = append(allErrs, field.Invalid(fldPath, nil, "exactly one of `nodeName`, `nodeSelector`, or `allNodes` is required"))
	}

	allErrs = append(allErrs, validateSet(spec.Devices, resource.ResourceSliceMaxDevices, validateDevice,
		func(device resource.Device) (string, string) {
			return device.Name, "name"
		}, fldPath.Child("devices"))...)

	return allErrs
}

func validateResourcePool(pool resource.ResourcePool, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	allErrs = append(allErrs, validatePoolName(pool.Name, fldPath.Child("name"))...)
	if pool.ResourceSliceCount <= 0 {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("resourceSliceCount"), pool.ResourceSliceCount, "must be greater than zero"))
	}
	if pool.Generation < 0 {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("generation"), pool.Generation, "must be greater than or equal to zero"))
	}
	return allErrs
}

func validateDevice(device resource.Device, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	allErrs = append(allErrs, validateDeviceName(device.Name, fldPath.Child("name"))...)
	if device.Basic == nil {
		allErrs = append(allErrs, field.Required(fldPath.Child("basic"), ""))
	} else {
		allErrs = append(allErrs, validateBasicDevice(*device.Basic, fldPath.Child("basic"))...)
	}
	return allErrs
}

func validateBasicDevice(device resource.BasicDevice, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	// Warn about exceeding the maximum length only once. If any individual
	// field is too large, then so is the combination.
	maxKeyLen := resource.DeviceMaxDomainLength + 1 + resource.DeviceMaxIDLength
	allErrs = append(allErrs, validateMap(device.Attributes, -1, maxKeyLen, validateQualifiedName, validateDeviceAttribute, fldPath.Child("attributes"))...)
	allErrs = append(allErrs, validateMap(device.Capacity, -1, maxKeyLen, validateQualifiedName, validateDeviceCapacity, fldPath.Child("capacity"))...)
	if combinedLen, max := len(device.Attributes)+len(device.Capacity), resource.ResourceSliceMaxAttributesAndCapacitiesPerDevice; combinedLen > max {
		allErrs = append(allErrs, field.Invalid(fldPath, combinedLen, fmt.Sprintf("the total number of attributes and capacities must not exceed %d", max)))
	}
	return allErrs
}

var (
	numericIdentifier = `(0|[1-9]\d*)`

	preReleaseIdentifier = `(0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*)`

	buildIdentifier = `[0-9a-zA-Z-]+`

	semverRe = regexp.MustCompile(`^` +

		// dot-separated version segments (e.g. 1.2.3)
		numericIdentifier + `\.` + numericIdentifier + `\.` + numericIdentifier +

		// optional dot-separated prerelease segments (e.g. -alpha.PRERELEASE.1)
		`(-` + preReleaseIdentifier + `(\.` + preReleaseIdentifier + `)*)?` +

		// optional dot-separated build identifier segments (e.g. +build.id.20240305)
		`(\+` + buildIdentifier + `(\.` + buildIdentifier + `)*)?` +

		`$`)
)

func validateDeviceAttribute(attribute resource.DeviceAttribute, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	numFields := 0
	if attribute.BoolValue != nil {
		numFields++
	}
	if attribute.IntValue != nil {
		numFields++
	}
	if attribute.StringValue != nil {
		if len(*attribute.StringValue) > resource.DeviceAttributeMaxValueLength {
			allErrs = append(allErrs, field.TooLong(fldPath.Child("string"), "" /*unused*/, resource.DeviceAttributeMaxValueLength))
		}
		numFields++
	}
	if attribute.VersionValue != nil {
		numFields++
		if !semverRe.MatchString(*attribute.VersionValue) {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("version"), *attribute.VersionValue, "must be a string compatible with semver.org spec 2.0.0"))
		}
		if len(*attribute.VersionValue) > resource.DeviceAttributeMaxValueLength {
			allErrs = append(allErrs, field.TooLong(fldPath.Child("version"), "" /*unused*/, resource.DeviceAttributeMaxValueLength))
		}
	}

	switch numFields {
	case 0:
		allErrs = append(allErrs, field.Required(fldPath, "exactly one value must be specified"))
	case 1:
		// Okay.
	default:
		allErrs = append(allErrs, field.Invalid(fldPath, attribute, "exactly one value must be specified"))
	}
	return allErrs
}

func validateDeviceCapacity(capacity resource.DeviceCapacity, fldPath *field.Path) field.ErrorList {
	// Any parsed quantity is valid.
	return nil
}

func validateQualifiedName(name resource.QualifiedName, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	if name == "" {
		allErrs = append(allErrs, field.Required(fldPath, "name required"))
		return allErrs
	}

	parts := strings.Split(string(name), "/")
	switch len(parts) {
	case 1:
		allErrs = append(allErrs, validateCIdentifier(parts[0], fldPath)...)
	case 2:
		if len(parts[0]) == 0 {
			allErrs = append(allErrs, field.Required(fldPath, "the domain must not be empty"))
		} else {
			allErrs = append(allErrs, validateDriverName(parts[0], fldPath)...)
		}
		if len(parts[1]) == 0 {
			allErrs = append(allErrs, field.Required(fldPath, "the name must not be empty"))
		} else {
			allErrs = append(allErrs, validateCIdentifier(parts[1], fldPath)...)
		}
	}
	return allErrs
}

func validateFullyQualifiedName(name resource.FullyQualifiedName, fldPath *field.Path) field.ErrorList {
	allErrs := validateQualifiedName(resource.QualifiedName(name), fldPath)
	// validateQualifiedName checks that the name isn't empty and both parts are valid.
	// What we need to enforce here is that there really is a domain.
	if name != "" && !strings.Contains(string(name), "/") {
		allErrs = append(allErrs, field.Invalid(fldPath, name, "must include a domain"))
	}
	return allErrs
}

func validateCIdentifier(id string, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	if len(id) > resource.DeviceMaxIDLength {
		allErrs = append(allErrs, field.TooLong(fldPath, "" /*unused*/, resource.DeviceMaxIDLength))
	}
	for _, msg := range validation.IsCIdentifier(id) {
		allErrs = append(allErrs, field.TypeInvalid(fldPath, id, msg))
	}
	return allErrs
}

// validateSlice ensures that a slice does not exceed a certain maximum size
// and that all entries are valid.
// A negative maxSize disables the length check.
func validateSlice[T any](slice []T, maxSize int, validateItem func(T, *field.Path) field.ErrorList, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	for i, item := range slice {
		idxPath := fldPath.Index(i)
		allErrs = append(allErrs, validateItem(item, idxPath)...)
	}
	if maxSize >= 0 && len(slice) > maxSize {
		// Dumping the entire field into the error message is likely to be too long,
		// in particular when it is already beyond the maximum size. Instead this
		// just shows the number of entries.
		allErrs = append(allErrs, field.TooMany(fldPath, len(slice), maxSize))
	}
	return allErrs
}

// validateSet ensures that a slice contains no duplicates, does not
// exceed a certain maximum size and that all entries are valid.
func validateSet[T any, K comparable](slice []T, maxSize int, validateItem func(item T, fldPath *field.Path) field.ErrorList, itemKey func(T) (K, string), fldPath *field.Path) field.ErrorList {
	allErrs := validateSlice(slice, maxSize, validateItem, fldPath)
	allItems := sets.New[K]()
	for i, item := range slice {
		idxPath := fldPath.Index(i)
		key, fieldName := itemKey(item)
		childPath := idxPath
		if fieldName != "" {
			childPath = childPath.Child(fieldName)
		}
		if allItems.Has(key) {
			allErrs = append(allErrs, field.Duplicate(childPath, key))
		} else {
			allItems.Insert(key)
		}
	}
	return allErrs
}

// stringKey uses the item itself as a key for validateSet.
func stringKey(item string) (string, string) {
	return item, ""
}

// validateMap validates keys, items and the maximum length of a map.
// A negative maxSize disables the length check.
//
// Keys larger than truncateKeyLen get truncated in the middle. A very
// small limit gets increased because it is okay to include more details.
// This is not used for validation of keys, which has to be done by
// the callback function.
func validateMap[K ~string, T any](m map[K]T, maxSize, truncateKeyLen int, validateKey func(K, *field.Path) field.ErrorList, validateItem func(T, *field.Path) field.ErrorList, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	if maxSize >= 0 && len(m) > maxSize {
		allErrs = append(allErrs, field.TooMany(fldPath, len(m), maxSize))
	}
	for key, item := range m {
		keyPath := fldPath.Key(truncateIfTooLong(string(key), truncateKeyLen))
		allErrs = append(allErrs, validateKey(key, keyPath)...)
		allErrs = append(allErrs, validateItem(item, keyPath)...)
	}
	return allErrs
}

func truncateIfTooLong(str string, maxLen int) string {
	// The caller was overly restrictive. Increase the length to something reasonable
	// (https://github.com/kubernetes/kubernetes/pull/127511#discussion_r1826206362).
	if maxLen < 16 {
		maxLen = 16
	}
	if len(str) <= maxLen {
		return str
	}
	ellipsis := "..."
	remaining := maxLen - len(ellipsis)
	return str[0:(remaining+1)/2] + ellipsis + str[len(str)-remaining/2:]
}

func validateDeviceStatus(device resource.AllocatedDeviceStatus, fldPath *field.Path, allocatedDevices sets.Set[structured.DeviceID]) field.ErrorList {
	var allErrs field.ErrorList
	allErrs = append(allErrs, validateDriverName(device.Driver, fldPath.Child("driver"))...)
	allErrs = append(allErrs, validatePoolName(device.Pool, fldPath.Child("pool"))...)
	allErrs = append(allErrs, validateDeviceName(device.Device, fldPath.Child("device"))...)
	deviceID := structured.MakeDeviceID(device.Driver, device.Pool, device.Device)
	if !allocatedDevices.Has(deviceID) {
		allErrs = append(allErrs, field.Invalid(fldPath, deviceID, "must be an allocated device in the claim"))
	}
	if len(device.Conditions) > maxConditions {
		allErrs = append(allErrs, field.TooMany(fldPath.Child("conditions"), len(device.Conditions), maxConditions))
	}
	allErrs = append(allErrs, metav1validation.ValidateConditions(device.Conditions, fldPath.Child("conditions"))...)
	if len(device.Data.Raw) > 0 { // Data is an optional field.
		allErrs = append(allErrs, validateRawExtension(device.Data, fldPath.Child("data"), false)...)
	}
	allErrs = append(allErrs, validateNetworkDeviceData(device.NetworkData, fldPath.Child("networkData"))...)
	return allErrs
}

// validateRawExtension validates RawExtension as in https://github.com/kubernetes/kubernetes/pull/125549/
func validateRawExtension(rawExtension runtime.RawExtension, fldPath *field.Path, stored bool) field.ErrorList {
	var allErrs field.ErrorList
	var v any
	if len(rawExtension.Raw) == 0 {
		allErrs = append(allErrs, field.Required(fldPath, ""))
	} else if !stored && len(rawExtension.Raw) > resource.OpaqueParametersMaxLength {
		// Don't even bother with parsing when too large.
		// Only applies on create. Existing parameters are grand-fathered in
		// because the limit was introduced in 1.32. This also means that it
		// can be changed in the future.
		allErrs = append(allErrs, field.TooLong(fldPath, "" /* unused */, resource.OpaqueParametersMaxLength))
	} else if err := json.Unmarshal(rawExtension.Raw, &v); err != nil {
		allErrs = append(allErrs, field.Invalid(fldPath, "<value omitted>", fmt.Sprintf("error parsing data as JSON: %v", err.Error())))
	} else if v == nil {
		allErrs = append(allErrs, field.Required(fldPath, ""))
	} else if _, isObject := v.(map[string]any); !isObject {
		allErrs = append(allErrs, field.Invalid(fldPath, "<value omitted>", "parameters must be a valid JSON object"))
	}
	return allErrs
}

const maxConditions int = 8
const maxIPs int = 16
const interfaceNameMaxLength int = 256
const hardwareAddressMaxLength int = 128

func validateNetworkDeviceData(networkDeviceData *resource.NetworkDeviceData, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	if networkDeviceData == nil {
		return allErrs
	}

	if len(networkDeviceData.InterfaceName) > interfaceNameMaxLength {
		allErrs = append(allErrs, field.TooLong(fldPath.Child("interfaceName"), "" /* unused */, interfaceNameMaxLength))
	}

	if len(networkDeviceData.HardwareAddress) > hardwareAddressMaxLength {
		allErrs = append(allErrs, field.TooLong(fldPath.Child("hardwareAddress"), "" /* unused */, hardwareAddressMaxLength))
	}

	allErrs = append(allErrs, validateSet(networkDeviceData.IPs, maxIPs,
		func(address string, fldPath *field.Path) field.ErrorList {
			return validation.IsValidCIDR(fldPath, address)
		},
		func(address string) (string, string) {
			// reformat CIDR to handle different ways IPs can be written
			// (e.g. 2001:db8::1/64 == 2001:0db8::1/64)
			ip, ipNet, err := netutils.ParseCIDRSloppy(address)
			if err != nil {
				return "", "" // will fail at IsValidCIDR
			}
			maskSize, _ := ipNet.Mask.Size()
			return fmt.Sprintf("%s/%d", ip.String(), maskSize), ""
		},
		fldPath.Child("ips"))...)
	return allErrs
}
