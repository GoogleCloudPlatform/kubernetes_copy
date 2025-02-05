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

package pod

import (
	"strings"

	apiequality "k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	metavalidation "k8s.io/apimachinery/pkg/apis/meta/v1/validation"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/validation"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	api "k8s.io/kubernetes/pkg/apis/core"
	"k8s.io/kubernetes/pkg/apis/core/helper"
	apivalidation "k8s.io/kubernetes/pkg/apis/core/validation"
	"k8s.io/kubernetes/pkg/features"
)

// ContainerType signifies container type
type ContainerType int

const (
	// Containers is for normal containers
	Containers ContainerType = 1 << iota
	// InitContainers is for init containers
	InitContainers
	// EphemeralContainers is for ephemeral containers
	EphemeralContainers
)

// AllContainers specifies that all containers be visited
const AllContainers ContainerType = (InitContainers | Containers | EphemeralContainers)

// AllFeatureEnabledContainers returns a ContainerType mask which includes all container
// types except for the ones guarded by feature gate.
func AllFeatureEnabledContainers() ContainerType {
	return AllContainers
}

// ContainerVisitor is called with each container spec, and returns true
// if visiting should continue.
type ContainerVisitor func(container *api.Container, containerType ContainerType) (shouldContinue bool)

// VisitContainers invokes the visitor function with a pointer to every container
// spec in the given pod spec with type set in mask. If visitor returns false,
// visiting is short-circuited. VisitContainers returns true if visiting completes,
// false if visiting was short-circuited.
func VisitContainers(podSpec *api.PodSpec, mask ContainerType, visitor ContainerVisitor) bool {
	if mask&InitContainers != 0 {
		for i := range podSpec.InitContainers {
			if !visitor(&podSpec.InitContainers[i], InitContainers) {
				return false
			}
		}
	}
	if mask&Containers != 0 {
		for i := range podSpec.Containers {
			if !visitor(&podSpec.Containers[i], Containers) {
				return false
			}
		}
	}
	if mask&EphemeralContainers != 0 {
		for i := range podSpec.EphemeralContainers {
			if !visitor((*api.Container)(&podSpec.EphemeralContainers[i].EphemeralContainerCommon), EphemeralContainers) {
				return false
			}
		}
	}
	return true
}

// Visitor is called with each object name, and returns true if visiting should continue
type Visitor func(name string) (shouldContinue bool)

func skipEmptyNames(visitor Visitor) Visitor {
	return func(name string) bool {
		if len(name) == 0 {
			// continue visiting
			return true
		}
		// delegate to visitor
		return visitor(name)
	}
}

// VisitPodSecretNames invokes the visitor function with the name of every secret
// referenced by the pod spec. If visitor returns false, visiting is short-circuited.
// Transitive references (e.g. pod -> pvc -> pv -> secret) are not visited.
// Returns true if visiting completed, false if visiting was short-circuited.
func VisitPodSecretNames(pod *api.Pod, visitor Visitor, containerType ContainerType) bool {
	visitor = skipEmptyNames(visitor)
	for _, reference := range pod.Spec.ImagePullSecrets {
		if !visitor(reference.Name) {
			return false
		}
	}
	VisitContainers(&pod.Spec, containerType, func(c *api.Container, containerType ContainerType) bool {
		return visitContainerSecretNames(c, visitor)
	})
	var source *api.VolumeSource
	for i := range pod.Spec.Volumes {
		source = &pod.Spec.Volumes[i].VolumeSource
		switch {
		case source.AzureFile != nil:
			if len(source.AzureFile.SecretName) > 0 && !visitor(source.AzureFile.SecretName) {
				return false
			}
		case source.CephFS != nil:
			if source.CephFS.SecretRef != nil && !visitor(source.CephFS.SecretRef.Name) {
				return false
			}
		case source.Cinder != nil:
			if source.Cinder.SecretRef != nil && !visitor(source.Cinder.SecretRef.Name) {
				return false
			}
		case source.FlexVolume != nil:
			if source.FlexVolume.SecretRef != nil && !visitor(source.FlexVolume.SecretRef.Name) {
				return false
			}
		case source.Projected != nil:
			for j := range source.Projected.Sources {
				if source.Projected.Sources[j].Secret != nil {
					if !visitor(source.Projected.Sources[j].Secret.Name) {
						return false
					}
				}
			}
		case source.RBD != nil:
			if source.RBD.SecretRef != nil && !visitor(source.RBD.SecretRef.Name) {
				return false
			}
		case source.Secret != nil:
			if !visitor(source.Secret.SecretName) {
				return false
			}
		case source.ScaleIO != nil:
			if source.ScaleIO.SecretRef != nil && !visitor(source.ScaleIO.SecretRef.Name) {
				return false
			}
		case source.ISCSI != nil:
			if source.ISCSI.SecretRef != nil && !visitor(source.ISCSI.SecretRef.Name) {
				return false
			}
		case source.StorageOS != nil:
			if source.StorageOS.SecretRef != nil && !visitor(source.StorageOS.SecretRef.Name) {
				return false
			}
		case source.CSI != nil:
			if source.CSI.NodePublishSecretRef != nil && !visitor(source.CSI.NodePublishSecretRef.Name) {
				return false
			}
		}
	}
	return true
}

func visitContainerSecretNames(container *api.Container, visitor Visitor) bool {
	for _, env := range container.EnvFrom {
		if env.SecretRef != nil {
			if !visitor(env.SecretRef.Name) {
				return false
			}
		}
	}
	for _, envVar := range container.Env {
		if envVar.ValueFrom != nil && envVar.ValueFrom.SecretKeyRef != nil {
			if !visitor(envVar.ValueFrom.SecretKeyRef.Name) {
				return false
			}
		}
	}
	return true
}

// VisitPodConfigmapNames invokes the visitor function with the name of every configmap
// referenced by the pod spec. If visitor returns false, visiting is short-circuited.
// Transitive references (e.g. pod -> pvc -> pv -> secret) are not visited.
// Returns true if visiting completed, false if visiting was short-circuited.
func VisitPodConfigmapNames(pod *api.Pod, visitor Visitor, containerType ContainerType) bool {
	visitor = skipEmptyNames(visitor)
	VisitContainers(&pod.Spec, containerType, func(c *api.Container, containerType ContainerType) bool {
		return visitContainerConfigmapNames(c, visitor)
	})
	var source *api.VolumeSource
	for i := range pod.Spec.Volumes {
		source = &pod.Spec.Volumes[i].VolumeSource
		switch {
		case source.Projected != nil:
			for j := range source.Projected.Sources {
				if source.Projected.Sources[j].ConfigMap != nil {
					if !visitor(source.Projected.Sources[j].ConfigMap.Name) {
						return false
					}
				}
			}
		case source.ConfigMap != nil:
			if !visitor(source.ConfigMap.Name) {
				return false
			}
		}
	}
	return true
}

func visitContainerConfigmapNames(container *api.Container, visitor Visitor) bool {
	for _, env := range container.EnvFrom {
		if env.ConfigMapRef != nil {
			if !visitor(env.ConfigMapRef.Name) {
				return false
			}
		}
	}
	for _, envVar := range container.Env {
		if envVar.ValueFrom != nil && envVar.ValueFrom.ConfigMapKeyRef != nil {
			if !visitor(envVar.ValueFrom.ConfigMapKeyRef.Name) {
				return false
			}
		}
	}
	return true
}

// IsPodReady returns true if a pod is ready; false otherwise.
func IsPodReady(pod *api.Pod) bool {
	return IsPodReadyConditionTrue(pod.Status)
}

// IsPodReadyConditionTrue returns true if a pod is ready; false otherwise.
func IsPodReadyConditionTrue(status api.PodStatus) bool {
	condition := GetPodReadyCondition(status)
	return condition != nil && condition.Status == api.ConditionTrue
}

// GetPodReadyCondition extracts the pod ready condition from the given status and returns that.
// Returns nil if the condition is not present.
func GetPodReadyCondition(status api.PodStatus) *api.PodCondition {
	_, condition := GetPodCondition(&status, api.PodReady)
	return condition
}

// GetPodCondition extracts the provided condition from the given status and returns that.
// Returns nil and -1 if the condition is not present, and the index of the located condition.
func GetPodCondition(status *api.PodStatus, conditionType api.PodConditionType) (int, *api.PodCondition) {
	if status == nil {
		return -1, nil
	}
	for i := range status.Conditions {
		if status.Conditions[i].Type == conditionType {
			return i, &status.Conditions[i]
		}
	}
	return -1, nil
}

// UpdatePodCondition updates existing pod condition or creates a new one. Sets LastTransitionTime to now if the
// status has changed.
// Returns true if pod condition has changed or has been added.
func UpdatePodCondition(status *api.PodStatus, condition *api.PodCondition) bool {
	condition.LastTransitionTime = metav1.Now()
	// Try to find this pod condition.
	conditionIndex, oldCondition := GetPodCondition(status, condition.Type)

	if oldCondition == nil {
		// We are adding new pod condition.
		status.Conditions = append(status.Conditions, *condition)
		return true
	}
	// We are updating an existing condition, so we need to check if it has changed.
	if condition.Status == oldCondition.Status {
		condition.LastTransitionTime = oldCondition.LastTransitionTime
	}

	isEqual := condition.Status == oldCondition.Status &&
		condition.Reason == oldCondition.Reason &&
		condition.Message == oldCondition.Message &&
		condition.LastProbeTime.Equal(&oldCondition.LastProbeTime) &&
		condition.LastTransitionTime.Equal(&oldCondition.LastTransitionTime)

	status.Conditions[conditionIndex] = *condition
	// Return true if one of the fields have changed.
	return !isEqual
}

func checkContainerUseIndivisibleHugePagesValues(container api.Container) bool {
	for resourceName, quantity := range container.Resources.Limits {
		if helper.IsHugePageResourceName(resourceName) {
			if !helper.IsHugePageResourceValueDivisible(resourceName, quantity) {
				return true
			}
		}
	}

	for resourceName, quantity := range container.Resources.Requests {
		if helper.IsHugePageResourceName(resourceName) {
			if !helper.IsHugePageResourceValueDivisible(resourceName, quantity) {
				return true
			}
		}
	}

	return false
}

// usesIndivisibleHugePagesValues returns true if the one of the containers uses non-integer multiple
// of huge page unit size
func usesIndivisibleHugePagesValues(podSpec *api.PodSpec) bool {
	foundIndivisibleHugePagesValue := false
	VisitContainers(podSpec, AllContainers, func(c *api.Container, containerType ContainerType) bool {
		if checkContainerUseIndivisibleHugePagesValues(*c) {
			foundIndivisibleHugePagesValue = true
		}
		return !foundIndivisibleHugePagesValue // continue visiting if we haven't seen an invalid value yet
	})

	if foundIndivisibleHugePagesValue {
		return true
	}

	for resourceName, quantity := range podSpec.Overhead {
		if helper.IsHugePageResourceName(resourceName) {
			if !helper.IsHugePageResourceValueDivisible(resourceName, quantity) {
				return true
			}
		}
	}

	return false
}

// hasInvalidTopologySpreadConstraintLabelSelector return true if spec.TopologySpreadConstraints have any entry with invalid labelSelector
func hasInvalidTopologySpreadConstraintLabelSelector(spec *api.PodSpec) bool {
	for _, constraint := range spec.TopologySpreadConstraints {
		errs := metavalidation.ValidateLabelSelector(constraint.LabelSelector, metavalidation.LabelSelectorValidationOptions{AllowInvalidLabelValueInSelector: false}, nil)
		if len(errs) != 0 {
			return true
		}
	}
	return false
}

// hasNonLocalProjectedTokenPath return true if spec.Volumes have any entry with non-local projected token path
func hasNonLocalProjectedTokenPath(spec *api.PodSpec) bool {
	for _, volume := range spec.Volumes {
		if volume.Projected != nil {
			for _, source := range volume.Projected.Sources {
				if source.ServiceAccountToken == nil {
					continue
				}
				errs := apivalidation.ValidateLocalNonReservedPath(source.ServiceAccountToken.Path, nil)
				if len(errs) != 0 {
					return true
				}
			}
		}
	}
	return false
}

// GetValidationOptionsFromPodSpecAndMeta returns validation options based on pod specs and metadata
func GetValidationOptionsFromPodSpecAndMeta(podSpec, oldPodSpec *api.PodSpec, podMeta, oldPodMeta *metav1.ObjectMeta) apivalidation.PodValidationOptions {
	// default pod validation options based on feature gate
	opts := apivalidation.PodValidationOptions{
		AllowInvalidPodDeletionCost: !utilfeature.DefaultFeatureGate.Enabled(features.PodDeletionCost),
		// Do not allow pod spec to use non-integer multiple of huge page unit size default
		AllowIndivisibleHugePagesValues:                   false,
		AllowInvalidLabelValueInSelector:                  false,
		AllowInvalidTopologySpreadConstraintLabelSelector: false,
		AllowNamespacedSysctlsForHostNetAndHostIPC:        false,
		AllowNonLocalProjectedTokenPath:                   false,
		AllowPodLifecycleSleepActionZeroValue:             utilfeature.DefaultFeatureGate.Enabled(features.PodLifecycleSleepActionAllowZero),
		PodLevelResourcesEnabled:                          utilfeature.DefaultFeatureGate.Enabled(features.PodLevelResources),
		AllowInvalidLabelValueInRequiredNodeAffinity:      false,
		AllowSidecarResizePolicy:                          utilfeature.DefaultFeatureGate.Enabled(features.InPlacePodVerticalScaling),
	}

	// If old spec uses relaxed validation or enabled the RelaxedEnvironmentVariableValidation feature gate,
	// we must allow it
	opts.AllowRelaxedEnvironmentVariableValidation = useRelaxedEnvironmentVariableValidation(podSpec, oldPodSpec)
	opts.AllowRelaxedDNSSearchValidation = useRelaxedDNSSearchValidation(oldPodSpec)

	opts.AllowOnlyRecursiveSELinuxChangePolicy = useOnlyRecursiveSELinuxChangePolicy(oldPodSpec)

	if oldPodSpec != nil {
		// if old spec used non-integer multiple of huge page unit size, we must allow it
		opts.AllowIndivisibleHugePagesValues = usesIndivisibleHugePagesValues(oldPodSpec)

		opts.AllowInvalidLabelValueInSelector = hasInvalidLabelValueInAffinitySelector(oldPodSpec)
		opts.AllowInvalidLabelValueInRequiredNodeAffinity = hasInvalidLabelValueInRequiredNodeAffinity(oldPodSpec)
		// if old spec has invalid labelSelector in topologySpreadConstraint, we must allow it
		opts.AllowInvalidTopologySpreadConstraintLabelSelector = hasInvalidTopologySpreadConstraintLabelSelector(oldPodSpec)
		// if old spec has an invalid projected token volume path, we must allow it
		opts.AllowNonLocalProjectedTokenPath = hasNonLocalProjectedTokenPath(oldPodSpec)

		// if old spec has invalid sysctl with hostNet or hostIPC, we must allow it when update
		if oldPodSpec.SecurityContext != nil && len(oldPodSpec.SecurityContext.Sysctls) != 0 {
			for _, s := range oldPodSpec.SecurityContext.Sysctls {
				err := apivalidation.ValidateHostSysctl(s.Name, oldPodSpec.SecurityContext, nil)
				if err != nil {
					opts.AllowNamespacedSysctlsForHostNetAndHostIPC = true
					break
				}
			}
		}

		opts.AllowPodLifecycleSleepActionZeroValue = opts.AllowPodLifecycleSleepActionZeroValue || podLifecycleSleepActionZeroValueInUse(podSpec)
		// If oldPod has resize policy set on the restartable init container, we must allow it
		opts.AllowSidecarResizePolicy = opts.AllowSidecarResizePolicy || hasRestartableInitContainerResizePolicy(oldPodSpec)
	}
	if oldPodMeta != nil && !opts.AllowInvalidPodDeletionCost {
		// This is an update, so validate only if the existing object was valid.
		_, err := helper.GetDeletionCostFromPodAnnotations(oldPodMeta.Annotations)
		opts.AllowInvalidPodDeletionCost = err != nil
	}

	return opts
}

func useRelaxedEnvironmentVariableValidation(podSpec, oldPodSpec *api.PodSpec) bool {
	if utilfeature.DefaultFeatureGate.Enabled(features.RelaxedEnvironmentVariableValidation) {
		return true
	}

	var oldPodEnvVarNames, podEnvVarNames sets.Set[string]
	if oldPodSpec != nil {
		oldPodEnvVarNames = gatherPodEnvVarNames(oldPodSpec)
	}

	if podSpec != nil {
		podEnvVarNames = gatherPodEnvVarNames(podSpec)
	}

	for env := range podEnvVarNames {
		if relaxedEnvVarUsed(env, oldPodEnvVarNames) {
			return true
		}
	}

	return false
}

func useRelaxedDNSSearchValidation(oldPodSpec *api.PodSpec) bool {
	// Return true early if feature gate is enabled
	if utilfeature.DefaultFeatureGate.Enabled(features.RelaxedDNSSearchValidation) {
		return true
	}

	// Return false early if there is no DNSConfig or Searches.
	if oldPodSpec == nil || oldPodSpec.DNSConfig == nil || oldPodSpec.DNSConfig.Searches == nil {
		return false
	}

	return hasDotOrUnderscore(oldPodSpec.DNSConfig.Searches)
}

// Helper function to check if any domain is a dot or contains an underscore.
func hasDotOrUnderscore(searches []string) bool {
	for _, domain := range searches {
		if domain == "." || strings.Contains(domain, "_") {
			return true
		}
	}
	return false
}

func gatherPodEnvVarNames(podSpec *api.PodSpec) sets.Set[string] {
	podEnvVarNames := sets.Set[string]{}

	for _, c := range podSpec.Containers {
		for _, env := range c.Env {
			podEnvVarNames.Insert(env.Name)
		}

		for _, env := range c.EnvFrom {
			podEnvVarNames.Insert(env.Prefix)
		}
	}

	for _, c := range podSpec.InitContainers {
		for _, env := range c.Env {
			podEnvVarNames.Insert(env.Name)
		}

		for _, env := range c.EnvFrom {
			podEnvVarNames.Insert(env.Prefix)
		}
	}

	for _, c := range podSpec.EphemeralContainers {
		for _, env := range c.Env {
			podEnvVarNames.Insert(env.Name)
		}

		for _, env := range c.EnvFrom {
			podEnvVarNames.Insert(env.Prefix)
		}
	}

	return podEnvVarNames
}

func relaxedEnvVarUsed(name string, oldPodEnvVarNames sets.Set[string]) bool {
	// A length of 0 means this is not an update request,
	// or the old pod does not exist in the env.
	// We will let the feature gate decide whether to use relaxed rules.
	if oldPodEnvVarNames.Len() == 0 {
		return false
	}

	if len(validation.IsEnvVarName(name)) == 0 || len(validation.IsRelaxedEnvVarName(name)) != 0 {
		// It's either a valid name by strict rules or an invalid name under relaxed rules.
		// Either way, we'll use strict rules to validate.
		return false
	}

	// The name in question failed strict rules but passed relaxed rules.
	if oldPodEnvVarNames.Has(name) {
		// This relaxed-rules name was already in use.
		return true
	}

	return false
}

// GetValidationOptionsFromPodTemplate will return pod validation options for specified template.
func GetValidationOptionsFromPodTemplate(podTemplate, oldPodTemplate *api.PodTemplateSpec) apivalidation.PodValidationOptions {
	var newPodSpec, oldPodSpec *api.PodSpec
	var newPodMeta, oldPodMeta *metav1.ObjectMeta
	// we have to be careful about nil pointers here
	// replication controller in particular is prone to passing nil
	if podTemplate != nil {
		newPodSpec = &podTemplate.Spec
		newPodMeta = &podTemplate.ObjectMeta
	}
	if oldPodTemplate != nil {
		oldPodSpec = &oldPodTemplate.Spec
		oldPodMeta = &oldPodTemplate.ObjectMeta
	}
	return GetValidationOptionsFromPodSpecAndMeta(newPodSpec, oldPodSpec, newPodMeta, oldPodMeta)
}

// DropDisabledTemplateFields removes disabled fields from the pod template metadata and spec.
// This should be called from PrepareForCreate/PrepareForUpdate for all resources containing a PodTemplateSpec
func DropDisabledTemplateFields(podTemplate, oldPodTemplate *api.PodTemplateSpec) {
	var (
		podSpec           *api.PodSpec
		podAnnotations    map[string]string
		oldPodSpec        *api.PodSpec
		oldPodAnnotations map[string]string
	)
	if podTemplate != nil {
		podSpec = &podTemplate.Spec
		podAnnotations = podTemplate.Annotations
	}
	if oldPodTemplate != nil {
		oldPodSpec = &oldPodTemplate.Spec
		oldPodAnnotations = oldPodTemplate.Annotations
	}
	dropDisabledFields(podSpec, podAnnotations, oldPodSpec, oldPodAnnotations)
}

// DropDisabledPodFields removes disabled fields from the pod metadata and spec.
// This should be called from PrepareForCreate/PrepareForUpdate for all resources containing a Pod
func DropDisabledPodFields(pod, oldPod *api.Pod) {
	var (
		podSpec           *api.PodSpec
		podStatus         *api.PodStatus
		podAnnotations    map[string]string
		oldPodSpec        *api.PodSpec
		oldPodStatus      *api.PodStatus
		oldPodAnnotations map[string]string
	)
	if pod != nil {
		podSpec = &pod.Spec
		podStatus = &pod.Status
		podAnnotations = pod.Annotations
	}
	if oldPod != nil {
		oldPodSpec = &oldPod.Spec
		oldPodStatus = &oldPod.Status
		oldPodAnnotations = oldPod.Annotations
	}
	dropDisabledFields(podSpec, podAnnotations, oldPodSpec, oldPodAnnotations)
	dropDisabledPodStatusFields(podStatus, oldPodStatus, podSpec, oldPodSpec)
}

// dropDisabledFields removes disabled fields from the pod metadata and spec.
func dropDisabledFields(
	podSpec *api.PodSpec, podAnnotations map[string]string,
	oldPodSpec *api.PodSpec, oldPodAnnotations map[string]string,
) {
	// the new spec must always be non-nil
	if podSpec == nil {
		podSpec = &api.PodSpec{}
	}

	// If the feature is disabled and not in use, drop the hostUsers field.
	if !utilfeature.DefaultFeatureGate.Enabled(features.UserNamespacesSupport) && !hostUsersInUse(oldPodSpec) {
		// Drop the field in podSpec only if SecurityContext is not nil.
		// If it is nil, there is no need to set hostUsers=nil (it will be nil too).
		if podSpec.SecurityContext != nil {
			podSpec.SecurityContext.HostUsers = nil
		}
	}

	// If the feature is disabled and not in use, drop the SupplementalGroupsPolicy field.
	if !utilfeature.DefaultFeatureGate.Enabled(features.SupplementalGroupsPolicy) && !supplementalGroupsPolicyInUse(oldPodSpec) {
		// Drop the field in podSpec only if SecurityContext is not nil.
		// If it is nil, there is no need to set supplementalGroupsPolicy=nil (it will be nil too).
		if podSpec.SecurityContext != nil {
			podSpec.SecurityContext.SupplementalGroupsPolicy = nil
		}
	}

	dropDisabledPodLevelResources(podSpec, oldPodSpec)
	dropDisabledProcMountField(podSpec, oldPodSpec)

	dropDisabledNodeInclusionPolicyFields(podSpec, oldPodSpec)
	dropDisabledMatchLabelKeysFieldInTopologySpread(podSpec, oldPodSpec)
	dropDisabledMatchLabelKeysFieldInPodAffinity(podSpec, oldPodSpec)
	dropDisabledDynamicResourceAllocationFields(podSpec, oldPodSpec)
	dropDisabledClusterTrustBundleProjection(podSpec, oldPodSpec)

	if !utilfeature.DefaultFeatureGate.Enabled(features.InPlacePodVerticalScaling) && !inPlacePodVerticalScalingInUse(oldPodSpec) {
		// Drop ResizePolicy fields. Don't drop updates to Resources field as template.spec.resources
		// field is mutable for certain controllers. Let ValidatePodUpdate handle it.
		for i := range podSpec.Containers {
			podSpec.Containers[i].ResizePolicy = nil
		}
		for i := range podSpec.InitContainers {
			podSpec.InitContainers[i].ResizePolicy = nil
		}
		for i := range podSpec.EphemeralContainers {
			podSpec.EphemeralContainers[i].ResizePolicy = nil
		}
	}

	if !utilfeature.DefaultFeatureGate.Enabled(features.SidecarContainers) && !restartableInitContainersInUse(oldPodSpec) {
		// Drop the RestartPolicy field of init containers.
		for i := range podSpec.InitContainers {
			podSpec.InitContainers[i].RestartPolicy = nil
		}
		// For other types of containers, validateContainers will handle them.
	}

	if !utilfeature.DefaultFeatureGate.Enabled(features.RecursiveReadOnlyMounts) && !rroInUse(oldPodSpec) {
		for i := range podSpec.Containers {
			for j := range podSpec.Containers[i].VolumeMounts {
				podSpec.Containers[i].VolumeMounts[j].RecursiveReadOnly = nil
			}
		}
		for i := range podSpec.InitContainers {
			for j := range podSpec.InitContainers[i].VolumeMounts {
				podSpec.InitContainers[i].VolumeMounts[j].RecursiveReadOnly = nil
			}
		}
		for i := range podSpec.EphemeralContainers {
			for j := range podSpec.EphemeralContainers[i].VolumeMounts {
				podSpec.EphemeralContainers[i].VolumeMounts[j].RecursiveReadOnly = nil
			}
		}
	}

	dropPodLifecycleSleepAction(podSpec, oldPodSpec)
	dropImageVolumes(podSpec, oldPodSpec)
	dropSELinuxChangePolicy(podSpec, oldPodSpec)
}

func dropDisabledPodLevelResources(podSpec, oldPodSpec *api.PodSpec) {
	// If the feature is disabled and not in use, drop Resources at the pod-level
	// from PodSpec.
	if !utilfeature.DefaultFeatureGate.Enabled(features.PodLevelResources) && !podLevelResourcesInUse(oldPodSpec) {
		podSpec.Resources = nil
	}
}

func dropPodLifecycleSleepAction(podSpec, oldPodSpec *api.PodSpec) {
	if utilfeature.DefaultFeatureGate.Enabled(features.PodLifecycleSleepAction) || podLifecycleSleepActionInUse(oldPodSpec) {
		return
	}

	adjustLifecycle := func(lifecycle *api.Lifecycle) {
		if lifecycle.PreStop != nil && lifecycle.PreStop.Sleep != nil {
			lifecycle.PreStop.Sleep = nil
			if lifecycle.PreStop.Exec == nil && lifecycle.PreStop.HTTPGet == nil && lifecycle.PreStop.TCPSocket == nil {
				lifecycle.PreStop = nil
			}
		}
		if lifecycle.PostStart != nil && lifecycle.PostStart.Sleep != nil {
			lifecycle.PostStart.Sleep = nil
			if lifecycle.PostStart.Exec == nil && lifecycle.PostStart.HTTPGet == nil && lifecycle.PostStart.TCPSocket == nil {
				lifecycle.PostStart = nil
			}
		}
	}

	for i := range podSpec.Containers {
		if podSpec.Containers[i].Lifecycle == nil {
			continue
		}
		adjustLifecycle(podSpec.Containers[i].Lifecycle)
		if podSpec.Containers[i].Lifecycle.PreStop == nil && podSpec.Containers[i].Lifecycle.PostStart == nil {
			podSpec.Containers[i].Lifecycle = nil
		}
	}

	for i := range podSpec.InitContainers {
		if podSpec.InitContainers[i].Lifecycle == nil {
			continue
		}
		adjustLifecycle(podSpec.InitContainers[i].Lifecycle)
		if podSpec.InitContainers[i].Lifecycle.PreStop == nil && podSpec.InitContainers[i].Lifecycle.PostStart == nil {
			podSpec.InitContainers[i].Lifecycle = nil
		}
	}

	for i := range podSpec.EphemeralContainers {
		if podSpec.EphemeralContainers[i].Lifecycle == nil {
			continue
		}
		adjustLifecycle(podSpec.EphemeralContainers[i].Lifecycle)
		if podSpec.EphemeralContainers[i].Lifecycle.PreStop == nil && podSpec.EphemeralContainers[i].Lifecycle.PostStart == nil {
			podSpec.EphemeralContainers[i].Lifecycle = nil
		}
	}
}

func podLifecycleSleepActionInUse(podSpec *api.PodSpec) bool {
	if podSpec == nil {
		return false
	}
	var inUse bool
	VisitContainers(podSpec, AllContainers, func(c *api.Container, containerType ContainerType) bool {
		if c.Lifecycle == nil {
			return true
		}
		if c.Lifecycle.PreStop != nil && c.Lifecycle.PreStop.Sleep != nil {
			inUse = true
			return false
		}
		if c.Lifecycle.PostStart != nil && c.Lifecycle.PostStart.Sleep != nil {
			inUse = true
			return false
		}
		return true
	})
	return inUse
}

func podLifecycleSleepActionZeroValueInUse(podSpec *api.PodSpec) bool {
	if podSpec == nil {
		return false
	}
	var inUse bool
	VisitContainers(podSpec, AllContainers, func(c *api.Container, containerType ContainerType) bool {
		if c.Lifecycle == nil {
			return true
		}
		if c.Lifecycle.PreStop != nil && c.Lifecycle.PreStop.Sleep != nil && c.Lifecycle.PreStop.Sleep.Seconds == 0 {
			inUse = true
			return false
		}
		if c.Lifecycle.PostStart != nil && c.Lifecycle.PostStart.Sleep != nil && c.Lifecycle.PreStop.Sleep.Seconds == 0 {
			inUse = true
			return false
		}
		return true
	})
	return inUse
}

// dropDisabledPodStatusFields removes disabled fields from the pod status
func dropDisabledPodStatusFields(podStatus, oldPodStatus *api.PodStatus, podSpec, oldPodSpec *api.PodSpec) {
	// the new status is always be non-nil
	if podStatus == nil {
		podStatus = &api.PodStatus{}
	}

	if !utilfeature.DefaultFeatureGate.Enabled(features.InPlacePodVerticalScaling) && !inPlacePodVerticalScalingInUse(oldPodSpec) {
		// Drop Resize and Resources fields
		dropResourcesField := func(csl []api.ContainerStatus) {
			for i := range csl {
				csl[i].Resources = nil
			}
		}
		dropResourcesField(podStatus.ContainerStatuses)
		dropResourcesField(podStatus.InitContainerStatuses)
		dropResourcesField(podStatus.EphemeralContainerStatuses)
		podStatus.Resize = ""
	}
	if !utilfeature.DefaultFeatureGate.Enabled(features.InPlacePodVerticalScaling) ||
		!utilfeature.DefaultFeatureGate.Enabled(features.InPlacePodVerticalScalingAllocatedStatus) {
		// Drop AllocatedResources field
		dropAllocatedResourcesField := func(csl []api.ContainerStatus) {
			for i := range csl {
				csl[i].AllocatedResources = nil
			}
		}
		dropAllocatedResourcesField(podStatus.ContainerStatuses)
		dropAllocatedResourcesField(podStatus.InitContainerStatuses)
		dropAllocatedResourcesField(podStatus.EphemeralContainerStatuses)
	}

	if !utilfeature.DefaultFeatureGate.Enabled(features.DynamicResourceAllocation) && !dynamicResourceAllocationInUse(oldPodSpec) {
		podStatus.ResourceClaimStatuses = nil
	}

	if !utilfeature.DefaultFeatureGate.Enabled(features.RecursiveReadOnlyMounts) && !rroInUse(oldPodSpec) {
		for i := range podStatus.ContainerStatuses {
			podStatus.ContainerStatuses[i].VolumeMounts = nil
		}
		for i := range podStatus.InitContainerStatuses {
			podStatus.InitContainerStatuses[i].VolumeMounts = nil
		}
		for i := range podStatus.EphemeralContainerStatuses {
			podStatus.EphemeralContainerStatuses[i].VolumeMounts = nil
		}
	}

	if !utilfeature.DefaultFeatureGate.Enabled(features.ResourceHealthStatus) {
		setAllocatedResourcesStatusToNil := func(csl []api.ContainerStatus) {
			for i := range csl {
				csl[i].AllocatedResourcesStatus = nil
			}
		}
		setAllocatedResourcesStatusToNil(podStatus.ContainerStatuses)
		setAllocatedResourcesStatusToNil(podStatus.InitContainerStatuses)
		setAllocatedResourcesStatusToNil(podStatus.EphemeralContainerStatuses)
	}

	// drop ContainerStatus.User field to empty (disable SupplementalGroupsPolicy)
	if !utilfeature.DefaultFeatureGate.Enabled(features.SupplementalGroupsPolicy) && !supplementalGroupsPolicyInUse(oldPodSpec) {
		dropUserField := func(csl []api.ContainerStatus) {
			for i := range csl {
				csl[i].User = nil
			}
		}
		dropUserField(podStatus.InitContainerStatuses)
		dropUserField(podStatus.ContainerStatuses)
		dropUserField(podStatus.EphemeralContainerStatuses)
	}
}

// dropDisabledDynamicResourceAllocationFields removes pod claim references from
// container specs and pod-level resource claims unless they are already used
// by the old pod spec.
func dropDisabledDynamicResourceAllocationFields(podSpec, oldPodSpec *api.PodSpec) {
	if !utilfeature.DefaultFeatureGate.Enabled(features.DynamicResourceAllocation) && !dynamicResourceAllocationInUse(oldPodSpec) {
		dropResourceClaimRequests(podSpec.Containers)
		dropResourceClaimRequests(podSpec.InitContainers)
		dropEphemeralResourceClaimRequests(podSpec.EphemeralContainers)
		podSpec.ResourceClaims = nil
	}
}

func dynamicResourceAllocationInUse(podSpec *api.PodSpec) bool {
	if podSpec == nil {
		return false
	}

	// We only need to check this field because the containers cannot have
	// resource requirements entries for claims without a corresponding
	// entry at the pod spec level.
	return len(podSpec.ResourceClaims) > 0
}

func dropResourceClaimRequests(containers []api.Container) {
	for i := range containers {
		containers[i].Resources.Claims = nil
	}
}

func dropEphemeralResourceClaimRequests(containers []api.EphemeralContainer) {
	for i := range containers {
		containers[i].Resources.Claims = nil
	}
}

// dropDisabledProcMountField removes disabled fields from PodSpec related
// to ProcMount only if it is not already used by the old spec
func dropDisabledProcMountField(podSpec, oldPodSpec *api.PodSpec) {
	if !utilfeature.DefaultFeatureGate.Enabled(features.ProcMountType) && !procMountInUse(oldPodSpec) {
		defaultProcMount := api.DefaultProcMount
		VisitContainers(podSpec, AllContainers, func(c *api.Container, containerType ContainerType) bool {
			if c.SecurityContext != nil && c.SecurityContext.ProcMount != nil {
				// The ProcMount field was improperly forced to non-nil in 1.12.
				// If the feature is disabled, and the existing object is not using any non-default values, and the ProcMount field is present in the incoming object, force to the default value.
				// Note: we cannot force the field to nil when the feature is disabled because it causes a diff against previously persisted data.
				c.SecurityContext.ProcMount = &defaultProcMount
			}
			return true
		})
	}
}

// dropDisabledNodeInclusionPolicyFields removes disabled fields from PodSpec related
// to NodeInclusionPolicy only if it is not used by the old spec.
func dropDisabledNodeInclusionPolicyFields(podSpec, oldPodSpec *api.PodSpec) {
	if !utilfeature.DefaultFeatureGate.Enabled(features.NodeInclusionPolicyInPodTopologySpread) && podSpec != nil {
		if !nodeTaintsPolicyInUse(oldPodSpec) {
			for i := range podSpec.TopologySpreadConstraints {
				podSpec.TopologySpreadConstraints[i].NodeTaintsPolicy = nil
			}
		}
		if !nodeAffinityPolicyInUse(oldPodSpec) {
			for i := range podSpec.TopologySpreadConstraints {
				podSpec.TopologySpreadConstraints[i].NodeAffinityPolicy = nil
			}
		}
	}
}

// dropDisabledMatchLabelKeysFieldInPodAffinity removes disabled fields from PodSpec related
// to MatchLabelKeys in required/preferred PodAffinity/PodAntiAffinity only if it is not already used by the old spec.
func dropDisabledMatchLabelKeysFieldInPodAffinity(podSpec, oldPodSpec *api.PodSpec) {
	if podSpec == nil || podSpec.Affinity == nil || utilfeature.DefaultFeatureGate.Enabled(features.MatchLabelKeysInPodAffinity) || matchLabelKeysFieldInPodAffinityInUse(oldPodSpec) {
		return
	}

	if affinity := podSpec.Affinity.PodAffinity; affinity != nil {
		dropMatchLabelKeysFieldInPodAffnityTerm(affinity.RequiredDuringSchedulingIgnoredDuringExecution)
		dropMatchLabelKeysFieldInWeightedPodAffnityTerm(affinity.PreferredDuringSchedulingIgnoredDuringExecution)
	}
	if antiaffinity := podSpec.Affinity.PodAntiAffinity; antiaffinity != nil {
		dropMatchLabelKeysFieldInPodAffnityTerm(antiaffinity.RequiredDuringSchedulingIgnoredDuringExecution)
		dropMatchLabelKeysFieldInWeightedPodAffnityTerm(antiaffinity.PreferredDuringSchedulingIgnoredDuringExecution)
	}
}

// dropDisabledMatchLabelKeysFieldInTopologySpread removes disabled fields from PodSpec related
// to MatchLabelKeys in TopologySpread only if it is not already used by the old spec.
func dropDisabledMatchLabelKeysFieldInTopologySpread(podSpec, oldPodSpec *api.PodSpec) {
	if !utilfeature.DefaultFeatureGate.Enabled(features.MatchLabelKeysInPodTopologySpread) && !matchLabelKeysInTopologySpreadInUse(oldPodSpec) {
		for i := range podSpec.TopologySpreadConstraints {
			podSpec.TopologySpreadConstraints[i].MatchLabelKeys = nil
		}
	}
}

// dropMatchLabelKeysFieldInWeightedPodAffnityTerm removes MatchLabelKeys and MismatchLabelKeys fields from WeightedPodAffinityTerm
func dropMatchLabelKeysFieldInWeightedPodAffnityTerm(terms []api.WeightedPodAffinityTerm) {
	for i := range terms {
		terms[i].PodAffinityTerm.MatchLabelKeys = nil
		terms[i].PodAffinityTerm.MismatchLabelKeys = nil
	}
}

// dropMatchLabelKeysFieldInPodAffnityTerm removes MatchLabelKeys and MismatchLabelKeys fields from PodAffinityTerm
func dropMatchLabelKeysFieldInPodAffnityTerm(terms []api.PodAffinityTerm) {
	for i := range terms {
		terms[i].MatchLabelKeys = nil
		terms[i].MismatchLabelKeys = nil
	}
}

// matchLabelKeysFieldInPodAffinityInUse returns true if given affinityTerms have MatchLabelKeys field set.
func matchLabelKeysFieldInPodAffinityInUse(podSpec *api.PodSpec) bool {
	if podSpec == nil || podSpec.Affinity == nil {
		return false
	}

	if affinity := podSpec.Affinity.PodAffinity; affinity != nil {
		for _, c := range affinity.RequiredDuringSchedulingIgnoredDuringExecution {
			if len(c.MatchLabelKeys) > 0 || len(c.MismatchLabelKeys) > 0 {
				return true
			}
		}

		for _, c := range affinity.PreferredDuringSchedulingIgnoredDuringExecution {
			if len(c.PodAffinityTerm.MatchLabelKeys) > 0 || len(c.PodAffinityTerm.MismatchLabelKeys) > 0 {
				return true
			}
		}
	}

	if antiAffinity := podSpec.Affinity.PodAntiAffinity; antiAffinity != nil {
		for _, c := range antiAffinity.RequiredDuringSchedulingIgnoredDuringExecution {
			if len(c.MatchLabelKeys) > 0 || len(c.MismatchLabelKeys) > 0 {
				return true
			}
		}

		for _, c := range antiAffinity.PreferredDuringSchedulingIgnoredDuringExecution {
			if len(c.PodAffinityTerm.MatchLabelKeys) > 0 || len(c.PodAffinityTerm.MismatchLabelKeys) > 0 {
				return true
			}
		}
	}

	return false
}

// matchLabelKeysInTopologySpreadInUse returns true if the pod spec is non-nil
// and has MatchLabelKeys field set in TopologySpreadConstraints.
func matchLabelKeysInTopologySpreadInUse(podSpec *api.PodSpec) bool {
	if podSpec == nil {
		return false
	}

	for _, c := range podSpec.TopologySpreadConstraints {
		if len(c.MatchLabelKeys) > 0 {
			return true
		}
	}
	return false
}

// nodeAffinityPolicyInUse returns true if the pod spec is non-nil and has NodeAffinityPolicy field set
// in TopologySpreadConstraints
func nodeAffinityPolicyInUse(podSpec *api.PodSpec) bool {
	if podSpec == nil {
		return false
	}
	for _, c := range podSpec.TopologySpreadConstraints {
		if c.NodeAffinityPolicy != nil {
			return true
		}
	}
	return false
}

// nodeTaintsPolicyInUse returns true if the pod spec is non-nil and has NodeTaintsPolicy field set
// in TopologySpreadConstraints
func nodeTaintsPolicyInUse(podSpec *api.PodSpec) bool {
	if podSpec == nil {
		return false
	}
	for _, c := range podSpec.TopologySpreadConstraints {
		if c.NodeTaintsPolicy != nil {
			return true
		}
	}
	return false
}

// hostUsersInUse returns true if the pod spec has spec.hostUsers field set.
func hostUsersInUse(podSpec *api.PodSpec) bool {
	if podSpec != nil && podSpec.SecurityContext != nil && podSpec.SecurityContext.HostUsers != nil {
		return true
	}

	return false
}

func supplementalGroupsPolicyInUse(podSpec *api.PodSpec) bool {
	if podSpec != nil && podSpec.SecurityContext != nil && podSpec.SecurityContext.SupplementalGroupsPolicy != nil {
		return true
	}

	return false
}

// podLevelResourcesInUse returns true if pod-spec is non-nil and Resources field at
// pod-level has non-empty Requests or Limits.
func podLevelResourcesInUse(podSpec *api.PodSpec) bool {
	if podSpec == nil {
		return false
	}

	if podSpec.Resources == nil {
		return false
	}

	if len(podSpec.Resources.Requests) > 0 {
		return true
	}

	if len(podSpec.Resources.Limits) > 0 {
		return true
	}

	return false
}

// inPlacePodVerticalScalingInUse returns true if pod spec is non-nil and ResizePolicy is set
func inPlacePodVerticalScalingInUse(podSpec *api.PodSpec) bool {
	if podSpec == nil {
		return false
	}
	var inUse bool
	containersMask := Containers | InitContainers
	VisitContainers(podSpec, containersMask, func(c *api.Container, containerType ContainerType) bool {
		if len(c.ResizePolicy) > 0 {
			inUse = true
			return false
		}
		return true
	})
	return inUse
}

// procMountInUse returns true if the pod spec is non-nil and has a SecurityContext's ProcMount field set to a non-default value
func procMountInUse(podSpec *api.PodSpec) bool {
	if podSpec == nil {
		return false
	}

	var inUse bool
	VisitContainers(podSpec, AllContainers, func(c *api.Container, containerType ContainerType) bool {
		if c.SecurityContext == nil || c.SecurityContext.ProcMount == nil {
			return true
		}
		if *c.SecurityContext.ProcMount != api.DefaultProcMount {
			inUse = true
			return false
		}
		return true
	})

	return inUse
}

// appArmorAnnotationsInUse returns true if the pod has apparmor annotations
func appArmorAnnotationsInUse(podAnnotations map[string]string) bool {
	for k := range podAnnotations {
		if strings.HasPrefix(k, api.DeprecatedAppArmorAnnotationKeyPrefix) {
			return true
		}
	}
	return false
}

// appArmorFieldsInUse returns true if the pod has apparmor fields set
func appArmorFieldsInUse(podSpec *api.PodSpec) bool {
	if podSpec == nil {
		return false
	}
	if podSpec.SecurityContext != nil && podSpec.SecurityContext.AppArmorProfile != nil {
		return true
	}
	hasAppArmorContainer := false
	VisitContainers(podSpec, AllContainers, func(c *api.Container, _ ContainerType) bool {
		if c.SecurityContext != nil && c.SecurityContext.AppArmorProfile != nil {
			hasAppArmorContainer = true
			return false
		}
		return true
	})
	return hasAppArmorContainer
}

// restartableInitContainersInUse returns true if the pod spec is non-nil and
// it has any init container with ContainerRestartPolicyAlways.
func restartableInitContainersInUse(podSpec *api.PodSpec) bool {
	if podSpec == nil {
		return false
	}
	var inUse bool
	VisitContainers(podSpec, InitContainers, func(c *api.Container, containerType ContainerType) bool {
		if c.RestartPolicy != nil && *c.RestartPolicy == api.ContainerRestartPolicyAlways {
			inUse = true
			return false
		}
		return true
	})
	return inUse
}

func clusterTrustBundleProjectionInUse(podSpec *api.PodSpec) bool {
	if podSpec == nil {
		return false
	}
	for _, v := range podSpec.Volumes {
		if v.Projected == nil {
			continue
		}

		for _, s := range v.Projected.Sources {
			if s.ClusterTrustBundle != nil {
				return true
			}
		}
	}

	return false
}

func rroInUse(podSpec *api.PodSpec) bool {
	if podSpec == nil {
		return false
	}
	var inUse bool
	VisitContainers(podSpec, AllContainers, func(c *api.Container, _ ContainerType) bool {
		for _, f := range c.VolumeMounts {
			if f.RecursiveReadOnly != nil {
				inUse = true
				return false
			}
		}
		return true
	})
	return inUse
}

func dropDisabledClusterTrustBundleProjection(podSpec, oldPodSpec *api.PodSpec) {
	if utilfeature.DefaultFeatureGate.Enabled(features.ClusterTrustBundleProjection) {
		return
	}
	if podSpec == nil {
		return
	}

	// If the pod was already using it, it can keep using it.
	if clusterTrustBundleProjectionInUse(oldPodSpec) {
		return
	}

	for i := range podSpec.Volumes {
		if podSpec.Volumes[i].Projected == nil {
			continue
		}

		for j := range podSpec.Volumes[i].Projected.Sources {
			podSpec.Volumes[i].Projected.Sources[j].ClusterTrustBundle = nil
		}
	}
}

func hasInvalidLabelValueInAffinitySelector(spec *api.PodSpec) bool {
	if spec.Affinity != nil {
		if spec.Affinity.PodAffinity != nil {
			for _, term := range spec.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution {
				allErrs := apivalidation.ValidatePodAffinityTermSelector(term, false, nil)
				if len(allErrs) != 0 {
					return true
				}
			}
			for _, term := range spec.Affinity.PodAffinity.PreferredDuringSchedulingIgnoredDuringExecution {
				allErrs := apivalidation.ValidatePodAffinityTermSelector(term.PodAffinityTerm, false, nil)
				if len(allErrs) != 0 {
					return true
				}
			}
		}
		if spec.Affinity.PodAntiAffinity != nil {
			for _, term := range spec.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution {
				allErrs := apivalidation.ValidatePodAffinityTermSelector(term, false, nil)
				if len(allErrs) != 0 {
					return true
				}
			}
			for _, term := range spec.Affinity.PodAntiAffinity.PreferredDuringSchedulingIgnoredDuringExecution {
				allErrs := apivalidation.ValidatePodAffinityTermSelector(term.PodAffinityTerm, false, nil)
				if len(allErrs) != 0 {
					return true
				}
			}
		}
	}
	return false
}

// IsRestartableInitContainer returns true if the container has ContainerRestartPolicyAlways.
// This function is not checking if the container passed to it is indeed an init container.
// It is just checking if the container restart policy has been set to always.
func IsRestartableInitContainer(initContainer *api.Container) bool {
	if initContainer == nil || initContainer.RestartPolicy == nil {
		return false
	}
	return *initContainer.RestartPolicy == api.ContainerRestartPolicyAlways
}

func hasInvalidLabelValueInRequiredNodeAffinity(spec *api.PodSpec) bool {
	if spec == nil ||
		spec.Affinity == nil ||
		spec.Affinity.NodeAffinity == nil ||
		spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		return false
	}
	return helper.HasInvalidLabelValueInNodeSelectorTerms(spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms)
}

func MarkPodProposedForResize(oldPod, newPod *api.Pod) {
	if len(newPod.Spec.Containers) != len(oldPod.Spec.Containers) {
		// Update is invalid: ignore changes and let validation handle it
		return
	}

	if utilfeature.DefaultFeatureGate.Enabled(features.SidecarContainers) && len(newPod.Spec.InitContainers) != len(oldPod.Spec.InitContainers) {
		return
	}

	for i, c := range newPod.Spec.Containers {
		if c.Name != oldPod.Spec.Containers[i].Name {
			return // Update is invalid (container mismatch): let validation handle it.
		}
		if apiequality.Semantic.DeepEqual(oldPod.Spec.Containers[i].Resources, c.Resources) {
			continue
		}
		newPod.Status.Resize = api.PodResizeStatusProposed
		return
	}

	if utilfeature.DefaultFeatureGate.Enabled(features.SidecarContainers) {
		for i, c := range newPod.Spec.InitContainers {
			if IsRestartableInitContainer(&c) {
				if c.Name != oldPod.Spec.InitContainers[i].Name {
					return // Update is invalid (container mismatch): let validation handle it.
				}
				if apiequality.Semantic.DeepEqual(oldPod.Spec.InitContainers[i].Resources, c.Resources) {
					continue
				}
				newPod.Status.Resize = api.PodResizeStatusProposed
				return
			}
		}
	}
}

// KEP: https://kep.k8s.io/4639
func dropImageVolumes(podSpec, oldPodSpec *api.PodSpec) {
	if utilfeature.DefaultFeatureGate.Enabled(features.ImageVolume) || imageVolumesInUse(oldPodSpec) {
		return
	}

	imageVolumeNames := sets.New[string]()
	var newVolumes []api.Volume
	for _, v := range podSpec.Volumes {
		if v.Image != nil {
			imageVolumeNames.Insert(v.Name)
			continue
		}
		newVolumes = append(newVolumes, v)
	}
	podSpec.Volumes = newVolumes

	dropVolumeMounts := func(givenMounts []api.VolumeMount) (newVolumeMounts []api.VolumeMount) {
		for _, m := range givenMounts {
			if !imageVolumeNames.Has(m.Name) {
				newVolumeMounts = append(newVolumeMounts, m)
			}
		}
		return newVolumeMounts
	}

	for i, c := range podSpec.Containers {
		podSpec.Containers[i].VolumeMounts = dropVolumeMounts(c.VolumeMounts)
	}

	for i, c := range podSpec.InitContainers {
		podSpec.InitContainers[i].VolumeMounts = dropVolumeMounts(c.VolumeMounts)
	}

	for i, c := range podSpec.EphemeralContainers {
		podSpec.EphemeralContainers[i].VolumeMounts = dropVolumeMounts(c.VolumeMounts)
	}
}

func imageVolumesInUse(podSpec *api.PodSpec) bool {
	if podSpec == nil {
		return false
	}

	for _, v := range podSpec.Volumes {
		if v.Image != nil {
			return true
		}
	}

	return false
}

func dropSELinuxChangePolicy(podSpec, oldPodSpec *api.PodSpec) {
	if utilfeature.DefaultFeatureGate.Enabled(features.SELinuxChangePolicy) || seLinuxChangePolicyInUse(oldPodSpec) {
		return
	}
	if podSpec == nil || podSpec.SecurityContext == nil {
		return
	}
	podSpec.SecurityContext.SELinuxChangePolicy = nil
}

func seLinuxChangePolicyInUse(podSpec *api.PodSpec) bool {
	if podSpec == nil || podSpec.SecurityContext == nil {
		return false
	}
	return podSpec.SecurityContext.SELinuxChangePolicy != nil
}

func useOnlyRecursiveSELinuxChangePolicy(oldPodSpec *api.PodSpec) bool {
	if utilfeature.DefaultFeatureGate.Enabled(features.SELinuxMount) {
		// All policies are allowed
		return false
	}

	if seLinuxChangePolicyInUse(oldPodSpec) {
		// The old pod spec has *any* policy: we need to keep that object update-able.
		return false
	}
	// No feature gate + no value in the old object -> only Recursive is allowed
	return true
}

// hasRestartableInitContainerResizePolicy returns true if the pod spec is non-nil and
// it has any init container with ContainerRestartPolicyAlways and non-nil ResizePolicy.
func hasRestartableInitContainerResizePolicy(podSpec *api.PodSpec) bool {
	if podSpec == nil {
		return false
	}
	for _, c := range podSpec.InitContainers {
		if IsRestartableInitContainer(&c) && len(c.ResizePolicy) > 0 {
			return true
		}
	}
	return false
}
