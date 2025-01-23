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
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
const AllContainers ContainerType = InitContainers | Containers | EphemeralContainers

// AllFeatureEnabledContainers returns a ContainerType mask which includes all container
// types except for the ones guarded by feature gate.
func AllFeatureEnabledContainers() ContainerType {
	return AllContainers
}

// ContainerVisitor is called with each container spec, and returns true
// if visiting should continue.
type ContainerVisitor func(container *v1.Container, containerType ContainerType) (shouldContinue bool)

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

// VisitContainers invokes the visitor function with a pointer to every container
// spec in the given pod spec with type set in mask. If visitor returns false,
// visiting is short-circuited. VisitContainers returns true if visiting completes,
// false if visiting was short-circuited.
func VisitContainers(podSpec *v1.PodSpec, mask ContainerType, visitor ContainerVisitor) bool {
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
			if !visitor((*v1.Container)(&podSpec.EphemeralContainers[i].EphemeralContainerCommon), EphemeralContainers) {
				return false
			}
		}
	}
	return true
}

// VisitPodSecretNames invokes the visitor function with the name of every secret
// referenced by the pod spec. If visitor returns false, visiting is short-circuited.
// Transitive references (e.g. pod -> pvc -> pv -> secret) are not visited.
// Returns true if visiting completed, false if visiting was short-circuited.
func VisitPodSecretNames(pod *v1.Pod, visitor Visitor) bool {
	visitor = skipEmptyNames(visitor)
	for _, reference := range pod.Spec.ImagePullSecrets {
		if !visitor(reference.Name) {
			return false
		}
	}
	VisitContainers(&pod.Spec, AllContainers, func(c *v1.Container, containerType ContainerType) bool {
		return visitContainerSecretNames(c, visitor)
	})
	var source *v1.VolumeSource

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

// visitContainerSecretNames returns true unless the visitor returned false when invoked with a secret reference
func visitContainerSecretNames(container *v1.Container, visitor Visitor) bool {
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
func VisitPodConfigmapNames(pod *v1.Pod, visitor Visitor) bool {
	visitor = skipEmptyNames(visitor)
	VisitContainers(&pod.Spec, AllContainers, func(c *v1.Container, containerType ContainerType) bool {
		return visitContainerConfigmapNames(c, visitor)
	})
	var source *v1.VolumeSource
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

// visitContainerConfigmapNames returns true unless the visitor returned false when invoked with a configmap reference
func visitContainerConfigmapNames(container *v1.Container, visitor Visitor) bool {
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

// GetContainerStatus extracts the status of container "name" from "statuses".
// It returns true if "name" exists, else returns false.
func GetContainerStatus(statuses []v1.ContainerStatus, name string) (v1.ContainerStatus, bool) {
	for i := range statuses {
		if statuses[i].Name == name {
			return statuses[i], true
		}
	}
	return v1.ContainerStatus{}, false
}

// GetExistingContainerStatus extracts the status of container "name" from "statuses",
// It also returns if "name" exists.
func GetExistingContainerStatus(statuses []v1.ContainerStatus, name string) v1.ContainerStatus {
	status, _ := GetContainerStatus(statuses, name)
	return status
}

// GetIndexOfContainerStatus gets the index of status of container "name" from "statuses",
// It returns (index, true) if "name" exists, else returns (0, false).
func GetIndexOfContainerStatus(statuses []v1.ContainerStatus, name string) (int, bool) {
	for i := range statuses {
		if statuses[i].Name == name {
			return i, true
		}
	}
	return 0, false
}

// IsPodAvailable returns true if a pod is available; false otherwise.
// Precondition for an available pod is that it must be ready. On top
// of that, there are two cases when a pod can be considered available:
// 1. minReadySeconds == 0, or
// 2. LastTransitionTime (is set) + minReadySeconds < current time
func IsPodAvailable(pod *v1.Pod, minReadySeconds int32, now metav1.Time) bool {
	if !IsPodReady(pod) {
		return false
	}

	c := GetPodReadyCondition(pod.Status)
	minReadySecondsDuration := time.Duration(minReadySeconds) * time.Second
	if minReadySeconds == 0 || (!c.LastTransitionTime.IsZero() && c.LastTransitionTime.Add(minReadySecondsDuration).Before(now.Time)) {
		return true
	}
	return false
}

// IsPodReady returns true if a pod is ready; false otherwise.
func IsPodReady(pod *v1.Pod) bool {
	return IsPodReadyConditionTrue(pod.Status)
}

// IsPodTerminal returns true if a pod is terminal, all containers are stopped and cannot ever regress.
func IsPodTerminal(pod *v1.Pod) bool {
	return IsPodPhaseTerminal(pod.Status.Phase)
}

// IsPodPhaseTerminal returns true if the pod's phase is terminal.
func IsPodPhaseTerminal(phase v1.PodPhase) bool {
	return phase == v1.PodFailed || phase == v1.PodSucceeded
}

// IsPodReadyConditionTrue returns true if a pod is ready; false otherwise.
func IsPodReadyConditionTrue(status v1.PodStatus) bool {
	condition := GetPodReadyCondition(status)
	return condition != nil && condition.Status == v1.ConditionTrue
}

// IsContainersReadyConditionTrue returns true if a pod is ready; false otherwise.
func IsContainersReadyConditionTrue(status v1.PodStatus) bool {
	condition := GetContainersReadyCondition(status)
	return condition != nil && condition.Status == v1.ConditionTrue
}

// GetPodReadyCondition extracts the pod ready condition from the given status and returns that.
// Returns nil if the condition is not present.
func GetPodReadyCondition(status v1.PodStatus) *v1.PodCondition {
	_, condition := GetPodCondition(&status, v1.PodReady)
	return condition
}

// GetContainersReadyCondition extracts the containers ready condition from the given status and returns that.
// Returns nil if the condition is not present.
func GetContainersReadyCondition(status v1.PodStatus) *v1.PodCondition {
	_, condition := GetPodCondition(&status, v1.ContainersReady)
	return condition
}

// GetPodCondition extracts the provided condition from the given status and returns that.
// Returns nil and -1 if the condition is not present, and the index of the located condition.
func GetPodCondition(status *v1.PodStatus, conditionType v1.PodConditionType) (int, *v1.PodCondition) {
	if status == nil {
		return -1, nil
	}
	return GetPodConditionFromList(status.Conditions, conditionType)
}

// GetPodConditionFromList extracts the provided condition from the given list of condition and
// returns the index of the condition and the condition. Returns -1 and nil if the condition is not present.
func GetPodConditionFromList(conditions []v1.PodCondition, conditionType v1.PodConditionType) (int, *v1.PodCondition) {
	if conditions == nil {
		return -1, nil
	}
	for i := range conditions {
		if conditions[i].Type == conditionType {
			return i, &conditions[i]
		}
	}
	return -1, nil
}

// UpdatePodCondition updates existing pod condition or creates a new one. Sets LastTransitionTime to now if the
// status has changed.
// Returns true if pod condition has changed or has been added.
func UpdatePodCondition(status *v1.PodStatus, condition *v1.PodCondition) bool {
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

// IsRestartableInitContainer returns true if the container has ContainerRestartPolicyAlways.
// This function is not checking if the container passed to it is indeed an init container.
// It is just checking if the container restart policy has been set to always.
func IsRestartableInitContainer(initContainer *v1.Container) bool {
	if initContainer == nil || initContainer.RestartPolicy == nil {
		return false
	}
	return *initContainer.RestartPolicy == v1.ContainerRestartPolicyAlways
}
