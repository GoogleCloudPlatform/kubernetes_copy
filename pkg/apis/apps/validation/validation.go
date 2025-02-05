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

package validation

import (
	"encoding/json"
	"fmt"
	"strconv"

	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apimachineryvalidation "k8s.io/apimachinery/pkg/api/validation"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	unversionedvalidation "k8s.io/apimachinery/pkg/apis/meta/v1/validation"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/kubernetes/pkg/apis/apps"
	api "k8s.io/kubernetes/pkg/apis/core"
	apivalidation "k8s.io/kubernetes/pkg/apis/core/validation"
)

// ValidateStatefulSetName can be used to check whether the given StatefulSet name is valid.
// Prefix indicates this name will be used as part of generation, in which case
// trailing dashes are allowed.
func ValidateStatefulSetName(name string, prefix bool) []string {
	// TODO: Validate that there's room for the suffix inserted by the pods.
	// Currently this is just "-index". In the future we may allow a user
	// specified list of suffixes and we need  to validate the longest one.
	return apimachineryvalidation.NameIsDNSLabel(name, prefix)
}

// ValidatePodTemplateSpecForStatefulSet validates the given template and ensures that it is in accordance with the desired selector.
func ValidatePodTemplateSpecForStatefulSet(template *api.PodTemplateSpec, selector labels.Selector, fldPath *field.Path, opts apivalidation.PodValidationOptions) field.ErrorList {
	allErrs := field.ErrorList{}
	if template == nil {
		allErrs = append(allErrs, field.Required(fldPath, ""))
	} else {
		if !selector.Empty() {
			// Verify that the StatefulSet selector matches the labels in template.
			labels := labels.Set(template.Labels)
			if !selector.Matches(labels) {
				allErrs = append(allErrs, field.Invalid(fldPath.Child("metadata", "labels"), template.Labels, "`selector` does not match template `labels`"))
			}
		}
		// TODO: Add validation for PodSpec, currently this will check volumes, which we know will
		// fail. We should really check that the union of the given volumes and volumeClaims match
		// volume mounts in the containers.
		// allErrs = append(allErrs, apivalidation.ValidatePodTemplateSpec(template, fldPath)...)
		allErrs = append(allErrs, unversionedvalidation.ValidateLabels(template.Labels, fldPath.Child("labels"))...)
		allErrs = append(allErrs, apivalidation.ValidateAnnotations(template.Annotations, fldPath.Child("annotations"))...)
		allErrs = append(allErrs, apivalidation.ValidatePodSpecificAnnotations(template.Annotations, &template.Spec, fldPath.Child("annotations"), opts)...)
	}
	return allErrs
}

func ValidatePersistentVolumeClaimRetentionPolicyType(policy apps.PersistentVolumeClaimRetentionPolicyType, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	switch policy {
	case apps.RetainPersistentVolumeClaimRetentionPolicyType:
	case apps.DeletePersistentVolumeClaimRetentionPolicyType:
	default:
		allErrs = append(allErrs, field.NotSupported(fldPath, policy, []string{string(apps.RetainPersistentVolumeClaimRetentionPolicyType), string(apps.DeletePersistentVolumeClaimRetentionPolicyType)}))
	}
	return allErrs
}

func ValidatePersistentVolumeClaimRetentionPolicy(policy *apps.StatefulSetPersistentVolumeClaimRetentionPolicy, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	if policy != nil {
		allErrs = append(allErrs, ValidatePersistentVolumeClaimRetentionPolicyType(policy.WhenDeleted, fldPath.Child("whenDeleted"))...)
		allErrs = append(allErrs, ValidatePersistentVolumeClaimRetentionPolicyType(policy.WhenScaled, fldPath.Child("whenScaled"))...)
	}
	return allErrs
}

// ValidateStatefulSetSpec tests if required fields in the StatefulSet spec are set.
func ValidateStatefulSetSpec(spec *apps.StatefulSetSpec, fldPath *field.Path, opts apivalidation.PodValidationOptions) field.ErrorList {
	allErrs := field.ErrorList{}

	switch spec.PodManagementPolicy {
	case "":
		allErrs = append(allErrs, field.Required(fldPath.Child("podManagementPolicy"), ""))
	case apps.OrderedReadyPodManagement, apps.ParallelPodManagement:
	default:
		allErrs = append(allErrs, field.Invalid(fldPath.Child("podManagementPolicy"), spec.PodManagementPolicy, fmt.Sprintf("must be '%s' or '%s'", apps.OrderedReadyPodManagement, apps.ParallelPodManagement)))
	}

	switch spec.UpdateStrategy.Type {
	case "":
		allErrs = append(allErrs, field.Required(fldPath.Child("updateStrategy"), ""))
	case apps.OnDeleteStatefulSetStrategyType:
		if spec.UpdateStrategy.RollingUpdate != nil {
			allErrs = append(
				allErrs,
				field.Invalid(
					fldPath.Child("updateStrategy").Child("rollingUpdate"),
					spec.UpdateStrategy.RollingUpdate,
					fmt.Sprintf("only allowed for updateStrategy '%s'", apps.RollingUpdateStatefulSetStrategyType)))
		}
	case apps.RollingUpdateStatefulSetStrategyType:
		if spec.UpdateStrategy.RollingUpdate != nil {
			allErrs = append(allErrs, validateRollingUpdateStatefulSet(spec.UpdateStrategy.RollingUpdate, fldPath.Child("updateStrategy", "rollingUpdate"))...)

		}
	default:
		allErrs = append(allErrs,
			field.Invalid(fldPath.Child("updateStrategy"), spec.UpdateStrategy,
				fmt.Sprintf("must be '%s' or '%s'",
					apps.RollingUpdateStatefulSetStrategyType,
					apps.OnDeleteStatefulSetStrategyType)))
	}

	allErrs = append(allErrs, ValidatePersistentVolumeClaimRetentionPolicy(spec.PersistentVolumeClaimRetentionPolicy, fldPath.Child("persistentVolumeClaimRetentionPolicy"))...)

	allErrs = append(allErrs, apivalidation.ValidateNonnegativeField(int64(spec.Replicas), fldPath.Child("replicas"))...)
	allErrs = append(allErrs, apivalidation.ValidateNonnegativeField(int64(spec.MinReadySeconds), fldPath.Child("minReadySeconds"))...)
	if spec.Ordinals != nil {
		replicaStartOrdinal := spec.Ordinals.Start
		allErrs = append(allErrs, apivalidation.ValidateNonnegativeField(int64(replicaStartOrdinal), fldPath.Child("ordinals.start"))...)
	}

	if spec.Selector == nil {
		allErrs = append(allErrs, field.Required(fldPath.Child("selector"), ""))
	} else {
		// validate selector strictly, spec.selector was always required to pass LabelSelectorAsSelector below
		allErrs = append(allErrs, unversionedvalidation.ValidateLabelSelector(spec.Selector, unversionedvalidation.LabelSelectorValidationOptions{AllowInvalidLabelValueInSelector: false}, fldPath.Child("selector"))...)
		if len(spec.Selector.MatchLabels)+len(spec.Selector.MatchExpressions) == 0 {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("selector"), spec.Selector, "empty selector is invalid for statefulset"))
		}
	}

	selector, err := metav1.LabelSelectorAsSelector(spec.Selector)
	if err != nil {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("selector"), spec.Selector, ""))
	} else {
		allErrs = append(allErrs, ValidatePodTemplateSpecForStatefulSet(&spec.Template, selector, fldPath.Child("template"), opts)...)
	}

	if spec.Template.Spec.RestartPolicy != api.RestartPolicyAlways {
		allErrs = append(allErrs, field.NotSupported(fldPath.Child("template", "spec", "restartPolicy"), spec.Template.Spec.RestartPolicy, []string{string(api.RestartPolicyAlways)}))
	}
	if spec.Template.Spec.ActiveDeadlineSeconds != nil {
		allErrs = append(allErrs, field.Forbidden(fldPath.Child("template", "spec", "activeDeadlineSeconds"), "activeDeadlineSeconds in StatefulSet is not Supported"))
	}

	return allErrs
}

// ValidateStatefulSet validates a StatefulSet.
func ValidateStatefulSet(statefulSet *apps.StatefulSet, opts apivalidation.PodValidationOptions) field.ErrorList {
	allErrs := apivalidation.ValidateObjectMeta(&statefulSet.ObjectMeta, true, ValidateStatefulSetName, field.NewPath("metadata"))
	allErrs = append(allErrs, ValidateStatefulSetSpec(&statefulSet.Spec, field.NewPath("spec"), opts)...)
	return allErrs
}

// ValidateStatefulSetUpdate tests if required fields in the StatefulSet are set.
func ValidateStatefulSetUpdate(statefulSet, oldStatefulSet *apps.StatefulSet, opts apivalidation.PodValidationOptions) field.ErrorList {
	// First, validate that the new statefulset is valid.  Don't call
	// ValidateStatefulSet() because we don't want to revalidate the name on
	// update.  This is important here because we used to allow DNS subdomain
	// for name, but that can't actually create pods.  The only reasonable
	// thing to do it delete such an instance, but if there is a finalizer, it
	// would need to pass update validation.  Name can't change anyway.
	allErrs := apivalidation.ValidateObjectMetaUpdate(&statefulSet.ObjectMeta, &oldStatefulSet.ObjectMeta, field.NewPath("metadata"))
	allErrs = append(allErrs, ValidateStatefulSetSpec(&statefulSet.Spec, field.NewPath("spec"), opts)...)

	// statefulset updates aren't super common and general updates are likely to be touching spec, so we'll do this
	// deep copy right away.  This avoids mutating our inputs
	newStatefulSetClone := statefulSet.DeepCopy()
	newStatefulSetClone.Spec.Replicas = oldStatefulSet.Spec.Replicas                         // +k8s:verify-mutation:reason=clone
	newStatefulSetClone.Spec.Template = oldStatefulSet.Spec.Template                         // +k8s:verify-mutation:reason=clone
	newStatefulSetClone.Spec.UpdateStrategy = oldStatefulSet.Spec.UpdateStrategy             // +k8s:verify-mutation:reason=clone
	newStatefulSetClone.Spec.MinReadySeconds = oldStatefulSet.Spec.MinReadySeconds           // +k8s:verify-mutation:reason=clone
	newStatefulSetClone.Spec.Ordinals = oldStatefulSet.Spec.Ordinals                         // +k8s:verify-mutation:reason=clone
	newStatefulSetClone.Spec.RevisionHistoryLimit = oldStatefulSet.Spec.RevisionHistoryLimit // +k8s:verify-mutation:reason=clone

	newStatefulSetClone.Spec.PersistentVolumeClaimRetentionPolicy = oldStatefulSet.Spec.PersistentVolumeClaimRetentionPolicy // +k8s:verify-mutation:reason=clone
	if !apiequality.Semantic.DeepEqual(newStatefulSetClone.Spec, oldStatefulSet.Spec) {
		allErrs = append(allErrs, field.Forbidden(field.NewPath("spec"), "updates to statefulset spec for fields other than 'replicas', 'ordinals', 'template', 'updateStrategy', 'revisionHistoryLimit', 'persistentVolumeClaimRetentionPolicy' and 'minReadySeconds' are forbidden"))
	}

	return allErrs
}

// ValidateStatefulSetStatus validates a StatefulSetStatus.
func ValidateStatefulSetStatus(status *apps.StatefulSetStatus, fieldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}

	allErrs = append(allErrs, apivalidation.ValidateNonnegativeField(int64(status.Replicas), fieldPath.Child("replicas"))...)
	allErrs = append(allErrs, apivalidation.ValidateNonnegativeField(int64(status.ReadyReplicas), fieldPath.Child("readyReplicas"))...)
	allErrs = append(allErrs, apivalidation.ValidateNonnegativeField(int64(status.CurrentReplicas), fieldPath.Child("currentReplicas"))...)
	allErrs = append(allErrs, apivalidation.ValidateNonnegativeField(int64(status.UpdatedReplicas), fieldPath.Child("updatedReplicas"))...)
	allErrs = append(allErrs, apivalidation.ValidateNonnegativeField(int64(status.AvailableReplicas), fieldPath.Child("availableReplicas"))...)
	if status.ObservedGeneration != nil {
		allErrs = append(allErrs, apivalidation.ValidateNonnegativeField(int64(*status.ObservedGeneration), fieldPath.Child("observedGeneration"))...)
	}
	if status.CollisionCount != nil {
		allErrs = append(allErrs, apivalidation.ValidateNonnegativeField(int64(*status.CollisionCount), fieldPath.Child("collisionCount"))...)
	}

	msg := "cannot be greater than status.replicas"
	if status.ReadyReplicas > status.Replicas {
		allErrs = append(allErrs, field.Invalid(fieldPath.Child("readyReplicas"), status.ReadyReplicas, msg))
	}
	if status.CurrentReplicas > status.Replicas {
		allErrs = append(allErrs, field.Invalid(fieldPath.Child("currentReplicas"), status.CurrentReplicas, msg))
	}
	if status.UpdatedReplicas > status.Replicas {
		allErrs = append(allErrs, field.Invalid(fieldPath.Child("updatedReplicas"), status.UpdatedReplicas, msg))
	}
	if status.AvailableReplicas > status.Replicas {
		allErrs = append(allErrs, field.Invalid(fieldPath.Child("availableReplicas"), status.AvailableReplicas, msg))
	}
	if status.AvailableReplicas > status.ReadyReplicas {
		allErrs = append(allErrs, field.Invalid(fieldPath.Child("availableReplicas"), status.AvailableReplicas, "cannot be greater than status.readyReplicas"))
	}
	return allErrs
}

// ValidateStatefulSetStatusUpdate tests if required fields in the StatefulSet are set.
func ValidateStatefulSetStatusUpdate(statefulSet, oldStatefulSet *apps.StatefulSet) field.ErrorList {
	allErrs := field.ErrorList{}
	allErrs = append(allErrs, ValidateStatefulSetStatus(&statefulSet.Status, field.NewPath("status"))...)
	allErrs = append(allErrs, apivalidation.ValidateObjectMetaUpdate(&statefulSet.ObjectMeta, &oldStatefulSet.ObjectMeta, field.NewPath("metadata"))...)
	// TODO: Validate status.
	if apivalidation.IsDecremented(statefulSet.Status.CollisionCount, oldStatefulSet.Status.CollisionCount) {
		value := int32(0)
		if statefulSet.Status.CollisionCount != nil {
			value = *statefulSet.Status.CollisionCount
		}
		allErrs = append(allErrs, field.Invalid(field.NewPath("status").Child("collisionCount"), value, "cannot be decremented"))
	}
	return allErrs
}

// ValidateControllerRevisionName can be used to check whether the given ControllerRevision name is valid.
// Prefix indicates this name will be used as part of generation, in which case
// trailing dashes are allowed.
var ValidateControllerRevisionName = apimachineryvalidation.NameIsDNSSubdomain

// validateControllerRevision collects errors for the fields of state and returns those errors as an ErrorList. If the
// returned list is empty, state is valid. Validation is performed to ensure that state is a valid ObjectMeta, its name
// is valid, and that it doesn't exceed the MaxControllerRevisionSize.
func validateControllerRevision(revision *apps.ControllerRevision) field.ErrorList {
	errs := field.ErrorList{}
	errs = append(errs, apivalidation.ValidateObjectMeta(&revision.ObjectMeta, true, ValidateControllerRevisionName, field.NewPath("metadata"))...)
	errs = append(errs, apivalidation.ValidateNonnegativeField(revision.Revision, field.NewPath("revision"))...)
	return errs
}

func ValidateControllerRevisionCreate(revision *apps.ControllerRevision) field.ErrorList {
	errs := field.ErrorList{}
	errs = append(errs, validateControllerRevision(revision)...)

	var v any
	if revision.Data.Raw == nil {
		errs = append(errs, field.Required(field.NewPath("data"), "data is mandatory"))
	} else if err := json.Unmarshal(revision.Data.Raw, &v); err != nil {
		errs = append(errs, field.Invalid(field.NewPath("data"), "<value omitted>", fmt.Sprintf("error parsing data: %v", err.Error())))
	} else if v == nil {
		errs = append(errs, field.Required(field.NewPath("data"), "data is mandatory"))
	} else if _, isObject := v.(map[string]any); !isObject {
		errs = append(errs, field.Required(field.NewPath("data"), "data must be a valid JSON object"))
	}

	return errs
}

// ValidateControllerRevisionUpdate collects errors pertaining to the mutation of an ControllerRevision Object. If the
// returned ErrorList is empty the update operation is valid. Any mutation to the ControllerRevision's Data or Revision
// is considered to be invalid.
func ValidateControllerRevisionUpdate(newHistory, oldHistory *apps.ControllerRevision) field.ErrorList {
	errs := field.ErrorList{}

	errs = append(errs, apivalidation.ValidateObjectMetaUpdate(&newHistory.ObjectMeta, &oldHistory.ObjectMeta, field.NewPath("metadata"))...)
	errs = append(errs, validateControllerRevision(newHistory)...)
	errs = append(errs, apivalidation.ValidateImmutableField(newHistory.Data, oldHistory.Data, field.NewPath("data"))...)
	return errs
}

// ValidateDaemonSet tests if required fields in the DaemonSet are set.
func ValidateDaemonSet(ds *apps.DaemonSet, opts apivalidation.PodValidationOptions) field.ErrorList {
	allErrs := apivalidation.ValidateObjectMeta(&ds.ObjectMeta, true, ValidateDaemonSetName, field.NewPath("metadata"))
	allErrs = append(allErrs, ValidateDaemonSetSpec(&ds.Spec, field.NewPath("spec"), opts)...)
	return allErrs
}

// ValidateDaemonSetUpdate tests if required fields in the DaemonSet are set.
func ValidateDaemonSetUpdate(ds, oldDS *apps.DaemonSet, opts apivalidation.PodValidationOptions) field.ErrorList {
	allErrs := apivalidation.ValidateObjectMetaUpdate(&ds.ObjectMeta, &oldDS.ObjectMeta, field.NewPath("metadata"))
	allErrs = append(allErrs, ValidateDaemonSetSpecUpdate(&ds.Spec, &oldDS.Spec, field.NewPath("spec"))...)
	allErrs = append(allErrs, ValidateDaemonSetSpec(&ds.Spec, field.NewPath("spec"), opts)...)
	return allErrs
}

// ValidateDaemonSetSpecUpdate tests if an update to a DaemonSetSpec is valid.
func ValidateDaemonSetSpecUpdate(newSpec, oldSpec *apps.DaemonSetSpec, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}

	// TemplateGeneration shouldn't be decremented
	if newSpec.TemplateGeneration < oldSpec.TemplateGeneration {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("templateGeneration"), newSpec.TemplateGeneration, "must not be decremented"))
	}

	// TemplateGeneration should be increased when and only when template is changed
	templateUpdated := !apiequality.Semantic.DeepEqual(newSpec.Template, oldSpec.Template)
	if newSpec.TemplateGeneration == oldSpec.TemplateGeneration && templateUpdated {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("templateGeneration"), newSpec.TemplateGeneration, "must be incremented upon template update"))
	} else if newSpec.TemplateGeneration > oldSpec.TemplateGeneration && !templateUpdated {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("templateGeneration"), newSpec.TemplateGeneration, "must not be incremented without template update"))
	}

	return allErrs
}

// validateDaemonSetStatus validates a DaemonSetStatus
func validateDaemonSetStatus(status *apps.DaemonSetStatus, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	allErrs = append(allErrs, apivalidation.ValidateNonnegativeField(int64(status.CurrentNumberScheduled), fldPath.Child("currentNumberScheduled"))...)
	allErrs = append(allErrs, apivalidation.ValidateNonnegativeField(int64(status.NumberMisscheduled), fldPath.Child("numberMisscheduled"))...)
	allErrs = append(allErrs, apivalidation.ValidateNonnegativeField(int64(status.DesiredNumberScheduled), fldPath.Child("desiredNumberScheduled"))...)
	allErrs = append(allErrs, apivalidation.ValidateNonnegativeField(int64(status.NumberReady), fldPath.Child("numberReady"))...)
	allErrs = append(allErrs, apivalidation.ValidateNonnegativeField(status.ObservedGeneration, fldPath.Child("observedGeneration"))...)
	allErrs = append(allErrs, apivalidation.ValidateNonnegativeField(int64(status.UpdatedNumberScheduled), fldPath.Child("updatedNumberScheduled"))...)
	allErrs = append(allErrs, apivalidation.ValidateNonnegativeField(int64(status.NumberAvailable), fldPath.Child("numberAvailable"))...)
	allErrs = append(allErrs, apivalidation.ValidateNonnegativeField(int64(status.NumberUnavailable), fldPath.Child("numberUnavailable"))...)
	if status.CollisionCount != nil {
		allErrs = append(allErrs, apivalidation.ValidateNonnegativeField(int64(*status.CollisionCount), fldPath.Child("collisionCount"))...)
	}
	return allErrs
}

// ValidateDaemonSetStatusUpdate tests if required fields in the DaemonSet Status section
func ValidateDaemonSetStatusUpdate(ds, oldDS *apps.DaemonSet) field.ErrorList {
	allErrs := apivalidation.ValidateObjectMetaUpdate(&ds.ObjectMeta, &oldDS.ObjectMeta, field.NewPath("metadata"))
	allErrs = append(allErrs, validateDaemonSetStatus(&ds.Status, field.NewPath("status"))...)
	if apivalidation.IsDecremented(ds.Status.CollisionCount, oldDS.Status.CollisionCount) {
		value := int32(0)
		if ds.Status.CollisionCount != nil {
			value = *ds.Status.CollisionCount
		}
		allErrs = append(allErrs, field.Invalid(field.NewPath("status").Child("collisionCount"), value, "cannot be decremented"))
	}
	return allErrs
}

// ValidateDaemonSetSpec tests if required fields in the DaemonSetSpec are set.
func ValidateDaemonSetSpec(spec *apps.DaemonSetSpec, fldPath *field.Path, opts apivalidation.PodValidationOptions) field.ErrorList {
	allErrs := field.ErrorList{}
	labelSelectorValidationOpts := unversionedvalidation.LabelSelectorValidationOptions{AllowInvalidLabelValueInSelector: opts.AllowInvalidLabelValueInSelector}

	allErrs = append(allErrs, unversionedvalidation.ValidateLabelSelector(spec.Selector, labelSelectorValidationOpts, fldPath.Child("selector"))...)

	selector, err := metav1.LabelSelectorAsSelector(spec.Selector)
	if err == nil && !selector.Matches(labels.Set(spec.Template.Labels)) {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("template", "metadata", "labels"), spec.Template.Labels, "`selector` does not match template `labels`"))
	}
	if spec.Selector != nil && len(spec.Selector.MatchLabels)+len(spec.Selector.MatchExpressions) == 0 {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("selector"), spec.Selector, "empty selector is invalid for daemonset"))
	}

	allErrs = append(allErrs, apivalidation.ValidatePodTemplateSpec(&spec.Template, fldPath.Child("template"), opts)...)
	// RestartPolicy has already been first-order validated as per ValidatePodTemplateSpec().
	if spec.Template.Spec.RestartPolicy != api.RestartPolicyAlways {
		allErrs = append(allErrs, field.NotSupported(fldPath.Child("template", "spec", "restartPolicy"), spec.Template.Spec.RestartPolicy, []string{string(api.RestartPolicyAlways)}))
	}
	if spec.Template.Spec.ActiveDeadlineSeconds != nil {
		allErrs = append(allErrs, field.Forbidden(fldPath.Child("template", "spec", "activeDeadlineSeconds"), "activeDeadlineSeconds in DaemonSet is not Supported"))
	}
	allErrs = append(allErrs, apivalidation.ValidateNonnegativeField(int64(spec.MinReadySeconds), fldPath.Child("minReadySeconds"))...)
	allErrs = append(allErrs, apivalidation.ValidateNonnegativeField(int64(spec.TemplateGeneration), fldPath.Child("templateGeneration"))...)

	allErrs = append(allErrs, ValidateDaemonSetUpdateStrategy(&spec.UpdateStrategy, fldPath.Child("updateStrategy"))...)
	if spec.RevisionHistoryLimit != nil {
		// zero is a valid RevisionHistoryLimit
		allErrs = append(allErrs, apivalidation.ValidateNonnegativeField(int64(*spec.RevisionHistoryLimit), fldPath.Child("revisionHistoryLimit"))...)
	}
	return allErrs
}

// ValidateRollingUpdateDaemonSet validates a given RollingUpdateDaemonSet.
func ValidateRollingUpdateDaemonSet(rollingUpdate *apps.RollingUpdateDaemonSet, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList

	// Validate both fields are positive ints or have a percentage value
	allErrs = append(allErrs, ValidatePositiveIntOrPercent(rollingUpdate.MaxUnavailable, fldPath.Child("maxUnavailable"))...)
	allErrs = append(allErrs, ValidatePositiveIntOrPercent(rollingUpdate.MaxSurge, fldPath.Child("maxSurge"))...)

	// Validate that MaxUnavailable and MaxSurge are not more than 100%.
	allErrs = append(allErrs, IsNotMoreThan100Percent(rollingUpdate.MaxUnavailable, fldPath.Child("maxUnavailable"))...)
	allErrs = append(allErrs, IsNotMoreThan100Percent(rollingUpdate.MaxSurge, fldPath.Child("maxSurge"))...)

	// Validate exactly one of MaxSurge or MaxUnavailable is non-zero
	hasUnavailable := getIntOrPercentValue(rollingUpdate.MaxUnavailable) != 0
	hasSurge := getIntOrPercentValue(rollingUpdate.MaxSurge) != 0
	switch {
	case hasUnavailable && hasSurge:
		allErrs = append(allErrs, field.Invalid(fldPath.Child("maxSurge"), rollingUpdate.MaxSurge, "may not be set when maxUnavailable is non-zero"))
	case !hasUnavailable && !hasSurge:
		allErrs = append(allErrs, field.Required(fldPath.Child("maxUnavailable"), "cannot be 0 when maxSurge is 0"))
	}

	return allErrs
}

// validateRollingUpdateStatefulSet validates a given RollingUpdateStatefulSet.
func validateRollingUpdateStatefulSet(rollingUpdate *apps.RollingUpdateStatefulSetStrategy, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	fldPathMaxUn := fldPath.Child("maxUnavailable")
	allErrs = append(allErrs,
		apivalidation.ValidateNonnegativeField(
			int64(rollingUpdate.Partition),
			fldPath.Child("partition"))...)
	if rollingUpdate.MaxUnavailable != nil {
		allErrs = append(allErrs, ValidatePositiveIntOrPercent(*rollingUpdate.MaxUnavailable, fldPathMaxUn)...)
		if getIntOrPercentValue(*rollingUpdate.MaxUnavailable) == 0 {
			// MaxUnavailable cannot be 0.
			allErrs = append(allErrs, field.Invalid(fldPathMaxUn, *rollingUpdate.MaxUnavailable, "cannot be 0"))
		}
		// Validate that MaxUnavailable is not more than 100%.
		allErrs = append(allErrs, IsNotMoreThan100Percent(*rollingUpdate.MaxUnavailable, fldPathMaxUn)...)
	}
	return allErrs
}

// ValidateDaemonSetUpdateStrategy validates a given DaemonSetUpdateStrategy.
func ValidateDaemonSetUpdateStrategy(strategy *apps.DaemonSetUpdateStrategy, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	switch strategy.Type {
	case apps.OnDeleteDaemonSetStrategyType:
	case apps.RollingUpdateDaemonSetStrategyType:
		// Make sure RollingUpdate field isn't nil.
		if strategy.RollingUpdate == nil {
			allErrs = append(allErrs, field.Required(fldPath.Child("rollingUpdate"), ""))
			return allErrs
		}
		allErrs = append(allErrs, ValidateRollingUpdateDaemonSet(strategy.RollingUpdate, fldPath.Child("rollingUpdate"))...)
	default:
		validValues := []string{string(apps.RollingUpdateDaemonSetStrategyType), string(apps.OnDeleteDaemonSetStrategyType)}
		allErrs = append(allErrs, field.NotSupported(fldPath, strategy, validValues))
	}
	return allErrs
}

// ValidateDaemonSetName can be used to check whether the given daemon set name is valid.
// Prefix indicates this name will be used as part of generation, in which case
// trailing dashes are allowed.
var ValidateDaemonSetName = apimachineryvalidation.NameIsDNSSubdomain

// ValidateDeploymentName validates that the given name can be used as a deployment name.
var ValidateDeploymentName = apimachineryvalidation.NameIsDNSSubdomain

// ValidatePositiveIntOrPercent tests if a given value is a valid int or
// percentage.
func ValidatePositiveIntOrPercent(intOrPercent intstr.IntOrString, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	switch intOrPercent.Type {
	case intstr.String:
		for _, msg := range validation.IsValidPercent(intOrPercent.StrVal) {
			allErrs = append(allErrs, field.Invalid(fldPath, intOrPercent, msg))
		}
	case intstr.Int:
		allErrs = append(allErrs, apivalidation.ValidateNonnegativeField(int64(intOrPercent.IntValue()), fldPath)...)
	default:
		allErrs = append(allErrs, field.Invalid(fldPath, intOrPercent, "must be an integer or percentage (e.g '5%%')"))
	}
	return allErrs
}

func getPercentValue(intOrStringValue intstr.IntOrString) (int, bool) {
	if intOrStringValue.Type != intstr.String {
		return 0, false
	}
	if len(validation.IsValidPercent(intOrStringValue.StrVal)) != 0 {
		return 0, false
	}
	value, _ := strconv.Atoi(intOrStringValue.StrVal[:len(intOrStringValue.StrVal)-1])
	return value, true
}

func getIntOrPercentValue(intOrStringValue intstr.IntOrString) int {
	value, isPercent := getPercentValue(intOrStringValue)
	if isPercent {
		return value
	}
	return intOrStringValue.IntValue()
}

// IsNotMoreThan100Percent tests is a value can be represented as a percentage
// and if this value is not more than 100%.
func IsNotMoreThan100Percent(intOrStringValue intstr.IntOrString, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	value, isPercent := getPercentValue(intOrStringValue)
	if !isPercent || value <= 100 {
		return nil
	}
	allErrs = append(allErrs, field.Invalid(fldPath, intOrStringValue, "must not be greater than 100%"))
	return allErrs
}

// ValidateRollingUpdateDeployment validates a given RollingUpdateDeployment.
func ValidateRollingUpdateDeployment(rollingUpdate *apps.RollingUpdateDeployment, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	allErrs = append(allErrs, ValidatePositiveIntOrPercent(rollingUpdate.MaxUnavailable, fldPath.Child("maxUnavailable"))...)
	allErrs = append(allErrs, ValidatePositiveIntOrPercent(rollingUpdate.MaxSurge, fldPath.Child("maxSurge"))...)
	if getIntOrPercentValue(rollingUpdate.MaxUnavailable) == 0 && getIntOrPercentValue(rollingUpdate.MaxSurge) == 0 {
		// Both MaxSurge and MaxUnavailable cannot be zero.
		allErrs = append(allErrs, field.Invalid(fldPath.Child("maxUnavailable"), rollingUpdate.MaxUnavailable, "may not be 0 when `maxSurge` is 0"))
	}
	// Validate that MaxUnavailable is not more than 100%.
	allErrs = append(allErrs, IsNotMoreThan100Percent(rollingUpdate.MaxUnavailable, fldPath.Child("maxUnavailable"))...)
	return allErrs
}

// ValidateDeploymentStrategy validates given DeploymentStrategy.
func ValidateDeploymentStrategy(strategy *apps.DeploymentStrategy, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	switch strategy.Type {
	case apps.RecreateDeploymentStrategyType:
		if strategy.RollingUpdate != nil {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("rollingUpdate"), "may not be specified when strategy `type` is '"+string(apps.RecreateDeploymentStrategyType+"'")))
		}
	case apps.RollingUpdateDeploymentStrategyType:
		// This should never happen since it's set and checked in defaults.go
		if strategy.RollingUpdate == nil {
			allErrs = append(allErrs, field.Required(fldPath.Child("rollingUpdate"), "this should be defaulted and never be nil"))
		} else {
			allErrs = append(allErrs, ValidateRollingUpdateDeployment(strategy.RollingUpdate, fldPath.Child("rollingUpdate"))...)
		}
	default:
		validValues := []string{string(apps.RecreateDeploymentStrategyType), string(apps.RollingUpdateDeploymentStrategyType)}
		allErrs = append(allErrs, field.NotSupported(fldPath, strategy, validValues))
	}
	return allErrs
}

var supportedDeploymentPodReplacementPolicies = sets.NewString(
	string(apps.TerminationStarted),
	string(apps.TerminationComplete),
)

// ValidateRollback validates given RollbackConfig.
func ValidateRollback(rollback *apps.RollbackConfig, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	v := rollback.Revision
	allErrs = append(allErrs, apivalidation.ValidateNonnegativeField(int64(v), fldPath.Child("version"))...)
	return allErrs
}

// ValidateDeploymentSpec validates given deployment spec.
func ValidateDeploymentSpec(spec, oldSpec *apps.DeploymentSpec, fldPath *field.Path, opts apivalidation.PodValidationOptions) field.ErrorList {
	allErrs := field.ErrorList{}
	allErrs = append(allErrs, apivalidation.ValidateNonnegativeField(int64(spec.Replicas), fldPath.Child("replicas"))...)

	if spec.Selector == nil {
		allErrs = append(allErrs, field.Required(fldPath.Child("selector"), ""))
	} else {
		// validate selector strictly, spec.selector was always required to pass LabelSelectorAsSelector below
		allErrs = append(allErrs, unversionedvalidation.ValidateLabelSelector(spec.Selector, unversionedvalidation.LabelSelectorValidationOptions{AllowInvalidLabelValueInSelector: false}, fldPath.Child("selector"))...)
		if len(spec.Selector.MatchLabels)+len(spec.Selector.MatchExpressions) == 0 {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("selector"), spec.Selector, "empty selector is invalid for deployment"))
		}
	}

	selector, err := metav1.LabelSelectorAsSelector(spec.Selector)
	if err != nil {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("selector"), spec.Selector, "invalid label selector"))
	} else {
		allErrs = append(allErrs, ValidatePodTemplateSpecForReplicaSet(&spec.Template, selector, spec.Replicas, fldPath.Child("template"), opts)...)
	}

	allErrs = append(allErrs, ValidateDeploymentStrategy(&spec.Strategy, fldPath.Child("strategy"))...)
	allErrs = append(allErrs, apivalidation.ValidateNonnegativeField(int64(spec.MinReadySeconds), fldPath.Child("minReadySeconds"))...)
	if spec.RevisionHistoryLimit != nil {
		// zero is a valid RevisionHistoryLimit
		allErrs = append(allErrs, apivalidation.ValidateNonnegativeField(int64(*spec.RevisionHistoryLimit), fldPath.Child("revisionHistoryLimit"))...)
	}
	if spec.RollbackTo != nil {
		allErrs = append(allErrs, ValidateRollback(spec.RollbackTo, fldPath.Child("rollback"))...)
	}
	if spec.ProgressDeadlineSeconds != nil {
		allErrs = append(allErrs, apivalidation.ValidateNonnegativeField(int64(*spec.ProgressDeadlineSeconds), fldPath.Child("progressDeadlineSeconds"))...)
		if *spec.ProgressDeadlineSeconds <= spec.MinReadySeconds {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("progressDeadlineSeconds"), spec.ProgressDeadlineSeconds, "must be greater than minReadySeconds"))
		}
	}
	if spec.PodReplacementPolicy != nil && !supportedDeploymentPodReplacementPolicies.Has(string(*spec.PodReplacementPolicy)) {
		allErrs = append(allErrs, field.NotSupported(fldPath.Child("podReplacementPolicy"), *spec.PodReplacementPolicy, supportedDeploymentPodReplacementPolicies.List()))
	}
	return allErrs
}

// ValidateDeploymentStatus validates given deployment status.
func ValidateDeploymentStatus(status *apps.DeploymentStatus, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	allErrs = append(allErrs, apivalidation.ValidateNonnegativeField(status.ObservedGeneration, fldPath.Child("observedGeneration"))...)
	allErrs = append(allErrs, apivalidation.ValidateNonnegativeField(int64(status.Replicas), fldPath.Child("replicas"))...)
	allErrs = append(allErrs, apivalidation.ValidateNonnegativeField(int64(status.UpdatedReplicas), fldPath.Child("updatedReplicas"))...)
	allErrs = append(allErrs, apivalidation.ValidateNonnegativeField(int64(status.ReadyReplicas), fldPath.Child("readyReplicas"))...)
	allErrs = append(allErrs, apivalidation.ValidateNonnegativeField(int64(status.AvailableReplicas), fldPath.Child("availableReplicas"))...)
	allErrs = append(allErrs, apivalidation.ValidateNonnegativeField(int64(status.UnavailableReplicas), fldPath.Child("unavailableReplicas"))...)
	if status.TerminatingReplicas != nil {
		allErrs = append(allErrs, apivalidation.ValidateNonnegativeField(int64(*status.TerminatingReplicas), fldPath.Child("terminatingReplicas"))...)
	}
	if status.CollisionCount != nil {
		allErrs = append(allErrs, apivalidation.ValidateNonnegativeField(int64(*status.CollisionCount), fldPath.Child("collisionCount"))...)
	}
	msg := "cannot be greater than status.replicas"
	if status.UpdatedReplicas > status.Replicas {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("updatedReplicas"), status.UpdatedReplicas, msg))
	}
	if status.ReadyReplicas > status.Replicas {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("readyReplicas"), status.ReadyReplicas, msg))
	}
	if status.AvailableReplicas > status.Replicas {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("availableReplicas"), status.AvailableReplicas, msg))
	}
	if status.AvailableReplicas > status.ReadyReplicas {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("availableReplicas"), status.AvailableReplicas, "cannot be greater than readyReplicas"))
	}
	return allErrs
}

// ValidateDeploymentUpdate tests if an update to a Deployment is valid.
func ValidateDeploymentUpdate(update, old *apps.Deployment, opts apivalidation.PodValidationOptions) field.ErrorList {
	allErrs := apivalidation.ValidateObjectMetaUpdate(&update.ObjectMeta, &old.ObjectMeta, field.NewPath("metadata"))
	allErrs = append(allErrs, ValidateDeploymentSpec(&update.Spec, &old.Spec, field.NewPath("spec"), opts)...)
	return allErrs
}

// ValidateDeploymentStatusUpdate tests if a an update to a Deployment status
// is valid.
func ValidateDeploymentStatusUpdate(update, old *apps.Deployment) field.ErrorList {
	allErrs := apivalidation.ValidateObjectMetaUpdate(&update.ObjectMeta, &old.ObjectMeta, field.NewPath("metadata"))
	fldPath := field.NewPath("status")
	allErrs = append(allErrs, ValidateDeploymentStatus(&update.Status, fldPath)...)
	if apivalidation.IsDecremented(update.Status.CollisionCount, old.Status.CollisionCount) {
		value := int32(0)
		if update.Status.CollisionCount != nil {
			value = *update.Status.CollisionCount
		}
		allErrs = append(allErrs, field.Invalid(fldPath.Child("collisionCount"), value, "cannot be decremented"))
	}
	return allErrs
}

// ValidateDeployment validates a given Deployment.
func ValidateDeployment(obj *apps.Deployment, opts apivalidation.PodValidationOptions) field.ErrorList {
	allErrs := apivalidation.ValidateObjectMeta(&obj.ObjectMeta, true, ValidateDeploymentName, field.NewPath("metadata"))
	allErrs = append(allErrs, ValidateDeploymentSpec(&obj.Spec, nil, field.NewPath("spec"), opts)...)
	return allErrs
}

// ValidateDeploymentRollback validates a given DeploymentRollback.
func ValidateDeploymentRollback(obj *apps.DeploymentRollback) field.ErrorList {
	allErrs := apivalidation.ValidateAnnotations(obj.UpdatedAnnotations, field.NewPath("updatedAnnotations"))
	if len(obj.Name) == 0 {
		allErrs = append(allErrs, field.Required(field.NewPath("name"), "name is required"))
	}
	allErrs = append(allErrs, ValidateRollback(&obj.RollbackTo, field.NewPath("rollback"))...)
	return allErrs
}

// ValidateReplicaSetName can be used to check whether the given ReplicaSet
// name is valid.
// Prefix indicates this name will be used as part of generation, in which case
// trailing dashes are allowed.
var ValidateReplicaSetName = apimachineryvalidation.NameIsDNSSubdomain

// ValidateReplicaSet tests if required fields in the ReplicaSet are set.
func ValidateReplicaSet(rs *apps.ReplicaSet, opts apivalidation.PodValidationOptions) field.ErrorList {
	allErrs := apivalidation.ValidateObjectMeta(&rs.ObjectMeta, true, ValidateReplicaSetName, field.NewPath("metadata"))
	allErrs = append(allErrs, ValidateReplicaSetSpec(&rs.Spec, nil, field.NewPath("spec"), opts)...)
	return allErrs
}

// ValidateReplicaSetUpdate tests if required fields in the ReplicaSet are set.
func ValidateReplicaSetUpdate(rs, oldRs *apps.ReplicaSet, opts apivalidation.PodValidationOptions) field.ErrorList {
	allErrs := field.ErrorList{}
	allErrs = append(allErrs, apivalidation.ValidateObjectMetaUpdate(&rs.ObjectMeta, &oldRs.ObjectMeta, field.NewPath("metadata"))...)
	allErrs = append(allErrs, ValidateReplicaSetSpec(&rs.Spec, &oldRs.Spec, field.NewPath("spec"), opts)...)
	return allErrs
}

// ValidateReplicaSetStatusUpdate tests if required fields in the ReplicaSet are set.
func ValidateReplicaSetStatusUpdate(rs, oldRs *apps.ReplicaSet) field.ErrorList {
	allErrs := field.ErrorList{}
	allErrs = append(allErrs, apivalidation.ValidateObjectMetaUpdate(&rs.ObjectMeta, &oldRs.ObjectMeta, field.NewPath("metadata"))...)
	allErrs = append(allErrs, ValidateReplicaSetStatus(rs.Status, field.NewPath("status"))...)
	return allErrs
}

// ValidateReplicaSetStatus validates a given ReplicaSetStatus.
func ValidateReplicaSetStatus(status apps.ReplicaSetStatus, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	allErrs = append(allErrs, apivalidation.ValidateNonnegativeField(int64(status.Replicas), fldPath.Child("replicas"))...)
	allErrs = append(allErrs, apivalidation.ValidateNonnegativeField(int64(status.FullyLabeledReplicas), fldPath.Child("fullyLabeledReplicas"))...)
	allErrs = append(allErrs, apivalidation.ValidateNonnegativeField(int64(status.ReadyReplicas), fldPath.Child("readyReplicas"))...)
	allErrs = append(allErrs, apivalidation.ValidateNonnegativeField(int64(status.AvailableReplicas), fldPath.Child("availableReplicas"))...)
	allErrs = append(allErrs, apivalidation.ValidateNonnegativeField(int64(status.ObservedGeneration), fldPath.Child("observedGeneration"))...)
	if status.TerminatingReplicas != nil {
		allErrs = append(allErrs, apivalidation.ValidateNonnegativeField(int64(*status.TerminatingReplicas), fldPath.Child("terminatingReplicas"))...)
	}
	msg := "cannot be greater than status.replicas"
	if status.FullyLabeledReplicas > status.Replicas {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("fullyLabeledReplicas"), status.FullyLabeledReplicas, msg))
	}
	if status.ReadyReplicas > status.Replicas {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("readyReplicas"), status.ReadyReplicas, msg))
	}
	if status.AvailableReplicas > status.Replicas {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("availableReplicas"), status.AvailableReplicas, msg))
	}
	if status.AvailableReplicas > status.ReadyReplicas {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("availableReplicas"), status.AvailableReplicas, "cannot be greater than readyReplicas"))
	}
	return allErrs
}

// ValidateReplicaSetSpec tests if required fields in the ReplicaSet spec are set.
func ValidateReplicaSetSpec(spec, oldSpec *apps.ReplicaSetSpec, fldPath *field.Path, opts apivalidation.PodValidationOptions) field.ErrorList {
	allErrs := field.ErrorList{}

	allErrs = append(allErrs, apivalidation.ValidateNonnegativeField(int64(spec.Replicas), fldPath.Child("replicas"))...)
	allErrs = append(allErrs, apivalidation.ValidateNonnegativeField(int64(spec.MinReadySeconds), fldPath.Child("minReadySeconds"))...)

	if spec.Selector == nil {
		allErrs = append(allErrs, field.Required(fldPath.Child("selector"), ""))
	} else {
		// validate selector strictly, spec.selector was always required to pass LabelSelectorAsSelector below
		allErrs = append(allErrs, unversionedvalidation.ValidateLabelSelector(spec.Selector, unversionedvalidation.LabelSelectorValidationOptions{AllowInvalidLabelValueInSelector: false}, fldPath.Child("selector"))...)
		if len(spec.Selector.MatchLabels)+len(spec.Selector.MatchExpressions) == 0 {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("selector"), spec.Selector, "empty selector is invalid for deployment"))
		}
	}

	selector, err := metav1.LabelSelectorAsSelector(spec.Selector)
	if err != nil {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("selector"), spec.Selector, "invalid label selector"))
	} else {
		allErrs = append(allErrs, ValidatePodTemplateSpecForReplicaSet(&spec.Template, selector, spec.Replicas, fldPath.Child("template"), opts)...)
	}
	return allErrs
}

// ValidatePodTemplateSpecForReplicaSet validates the given template and ensures that it is in accordance with the desired selector and replicas.
func ValidatePodTemplateSpecForReplicaSet(template *api.PodTemplateSpec, selector labels.Selector, replicas int32, fldPath *field.Path, opts apivalidation.PodValidationOptions) field.ErrorList {
	allErrs := field.ErrorList{}
	if template == nil {
		allErrs = append(allErrs, field.Required(fldPath, ""))
	} else {
		if !selector.Empty() {
			// Verify that the ReplicaSet selector matches the labels in template.
			labels := labels.Set(template.Labels)
			if !selector.Matches(labels) {
				allErrs = append(allErrs, field.Invalid(fldPath.Child("metadata", "labels"), template.Labels, "`selector` does not match template `labels`"))
			}
		}
		allErrs = append(allErrs, apivalidation.ValidatePodTemplateSpec(template, fldPath, opts)...)
		// RestartPolicy has already been first-order validated as per ValidatePodTemplateSpec().
		if template.Spec.RestartPolicy != api.RestartPolicyAlways {
			allErrs = append(allErrs, field.NotSupported(fldPath.Child("spec", "restartPolicy"), template.Spec.RestartPolicy, []string{string(api.RestartPolicyAlways)}))
		}
		if template.Spec.ActiveDeadlineSeconds != nil {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("spec", "activeDeadlineSeconds"), "activeDeadlineSeconds in ReplicaSet is not Supported"))
		}
	}
	return allErrs
}
