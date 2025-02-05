/*
Copyright 2019 The Kubernetes Authors.

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

package drain

import (
	"context"
	"fmt"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	daemonSetFatal      = "DaemonSet-managed Pods (use --ignore-daemonsets to ignore)"
	daemonSetWarning    = "ignoring DaemonSet-managed Pods"
	localStorageFatal   = "Pods with local storage (use --delete-emptydir-data to override)"
	localStorageWarning = "deleting Pods with local storage"
	unmanagedFatal      = "cannot delete Pods that declare no controller (use --force to override)"
	unmanagedWarning    = "deleting Pods that declare no controller"
)

// PodDelete informs filtering logic whether a pod should be deleted or not
type PodDelete struct {
	Pod    corev1.Pod
	Status PodDeleteStatus
}

// PodDeleteList is a wrapper around []PodDelete
type PodDeleteList struct {
	items []PodDelete
}

// Pods returns a list of all pods marked for deletion after filtering.
func (l *PodDeleteList) Pods() []corev1.Pod {
	pods := []corev1.Pod{}
	for _, i := range l.items {
		if i.Status.Delete {
			pods = append(pods, i.Pod)
		}
	}
	return pods
}

// Warnings returns all warning messages concatenated into a string.
func (l *PodDeleteList) Warnings() string {
	ps := make(map[string][]string)
	for _, i := range l.items {
		if i.Status.Reason == PodDeleteStatusTypeWarning {
			ps[i.Status.Message] = append(ps[i.Status.Message], fmt.Sprintf("%s/%s", i.Pod.Namespace, i.Pod.Name))
		}
	}

	msgs := []string{}
	for key, pods := range ps {
		msgs = append(msgs, fmt.Sprintf("%s: %s", key, strings.Join(pods, ", ")))
	}
	return strings.Join(msgs, "; ")
}

func (l *PodDeleteList) errors() []error {
	failedPods := make(map[string][]string)
	for _, i := range l.items {
		if i.Status.Reason == PodDeleteStatusTypeError {
			msg := i.Status.Message
			if msg == "" {
				msg = "unexpected error"
			}
			failedPods[msg] = append(failedPods[msg], fmt.Sprintf("%s/%s", i.Pod.Namespace, i.Pod.Name))
		}
	}
	errs := make([]error, 0, len(failedPods))
	for msg, pods := range failedPods {
		errs = append(errs, fmt.Errorf("cannot delete %s: %s", msg, strings.Join(pods, ", ")))
	}
	return errs
}

// PodDeleteStatus informs filters if a pod should be deleted
type PodDeleteStatus struct {
	Delete  bool
	Reason  string
	Message string
}

// PodFilter takes a pod and returns a PodDeleteStatus
type PodFilter func(corev1.Pod) PodDeleteStatus

const (
	// PodDeleteStatusTypeOkay is "Okay"
	PodDeleteStatusTypeOkay = "Okay"
	// PodDeleteStatusTypeSkip is "Skip"
	PodDeleteStatusTypeSkip = "Skip"
	// PodDeleteStatusTypeWarning is "Warning"
	PodDeleteStatusTypeWarning = "Warning"
	// PodDeleteStatusTypeError is "Error"
	PodDeleteStatusTypeError = "Error"
)

// MakePodDeleteStatusOkay is a helper method to return the corresponding PodDeleteStatus
func MakePodDeleteStatusOkay() PodDeleteStatus {
	return PodDeleteStatus{
		Delete: true,
		Reason: PodDeleteStatusTypeOkay,
	}
}

// MakePodDeleteStatusSkip is a helper method to return the corresponding PodDeleteStatus
func MakePodDeleteStatusSkip() PodDeleteStatus {
	return PodDeleteStatus{
		Delete: false,
		Reason: PodDeleteStatusTypeSkip,
	}
}

// MakePodDeleteStatusWithWarning is a helper method to return the corresponding PodDeleteStatus
func MakePodDeleteStatusWithWarning(delete bool, message string) PodDeleteStatus {
	return PodDeleteStatus{
		Delete:  delete,
		Reason:  PodDeleteStatusTypeWarning,
		Message: message,
	}
}

// MakePodDeleteStatusWithError is a helper method to return the corresponding PodDeleteStatus
func MakePodDeleteStatusWithError(message string) PodDeleteStatus {
	return PodDeleteStatus{
		Delete:  false,
		Reason:  PodDeleteStatusTypeError,
		Message: message,
	}
}

// The filters are applied in a specific order, only the last filter's
// message will be retained if there are any warnings.
func (d *Helper) makeFilters() []PodFilter {
	baseFilters := []PodFilter{
		d.skipDeletedFilter,
		d.daemonSetFilter,
		d.mirrorPodFilter,
		d.localStorageFilter,
		d.unreplicatedFilter,
	}
	return append(baseFilters, d.AdditionalFilters...)
}

func hasLocalStorage(pod corev1.Pod) bool {
	for _, volume := range pod.Spec.Volumes {
		if volume.EmptyDir != nil {
			return true
		}
	}

	return false
}

func isControllerRefDaemonSet(workloadRef *metav1.OwnerReference) bool {
	// find if workloadRef is daemonSet
	daemonSetAPIVersion, daemonSetKind := appsv1.SchemeGroupVersion.WithKind("DaemonSet").ToAPIVersionAndKind()
	return workloadRef.Kind == daemonSetKind && workloadRef.APIVersion == daemonSetAPIVersion
}

func (d *Helper) daemonSetFilter(pod corev1.Pod) PodDeleteStatus {
	// Note that we return false in cases where the pod is DaemonSet managed,
	// regardless of flags.
	//
	// The exception is for pods that are orphaned (the referencing
	// management resource - including DaemonSet - is not found).
	// Such pods will be deleted if --force is used.
	controllerRef := metav1.GetControllerOf(&pod)
	if controllerRef == nil || !isControllerRefDaemonSet(controllerRef) {
		return MakePodDeleteStatusOkay()
	}
	// Any finished pod can be removed.
	if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
		return MakePodDeleteStatusOkay()
	}

	if _, err := d.Client.AppsV1().DaemonSets(pod.Namespace).Get(context.TODO(), controllerRef.Name, metav1.GetOptions{}); err != nil {
		// remove orphaned pods with a warning if --force is used
		if apierrors.IsNotFound(err) && d.Force {
			return MakePodDeleteStatusWithWarning(true, err.Error())
		}

		return MakePodDeleteStatusWithError(err.Error())
	}

	if !d.IgnoreAllDaemonSets {
		return MakePodDeleteStatusWithError(daemonSetFatal)
	}

	return MakePodDeleteStatusWithWarning(false, daemonSetWarning)
}

func (d *Helper) mirrorPodFilter(pod corev1.Pod) PodDeleteStatus {
	if _, found := pod.ObjectMeta.Annotations[corev1.MirrorPodAnnotationKey]; found {
		return MakePodDeleteStatusSkip()
	}
	return MakePodDeleteStatusOkay()
}

func (d *Helper) localStorageFilter(pod corev1.Pod) PodDeleteStatus {
	if !hasLocalStorage(pod) {
		return MakePodDeleteStatusOkay()
	}
	// Any finished pod can be removed.
	if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
		return MakePodDeleteStatusOkay()
	}
	if !d.DeleteEmptyDirData {
		return MakePodDeleteStatusWithError(localStorageFatal)
	}

	// TODO: this warning gets dropped by subsequent filters;
	// consider accounting for multiple warning conditions or at least
	// preserving the last warning message.
	return MakePodDeleteStatusWithWarning(true, localStorageWarning)
}

func (d *Helper) unreplicatedFilter(pod corev1.Pod) PodDeleteStatus {
	// any finished pod can be removed
	if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
		return MakePodDeleteStatusOkay()
	}

	controllerRef := metav1.GetControllerOf(&pod)
	if controllerRef != nil {
		return MakePodDeleteStatusOkay()
	}
	if d.Force {
		return MakePodDeleteStatusWithWarning(true, unmanagedWarning)
	}
	return MakePodDeleteStatusWithError(unmanagedFatal)
}

func shouldSkipPod(pod corev1.Pod, skipDeletedTimeoutSeconds int) bool {
	return skipDeletedTimeoutSeconds > 0 &&
		!pod.ObjectMeta.DeletionTimestamp.IsZero() &&
		int(time.Now().Sub(pod.ObjectMeta.GetDeletionTimestamp().Time).Seconds()) > skipDeletedTimeoutSeconds
}

func (d *Helper) skipDeletedFilter(pod corev1.Pod) PodDeleteStatus {
	if shouldSkipPod(pod, d.SkipWaitForDeleteTimeoutSeconds) {
		return MakePodDeleteStatusSkip()
	}
	return MakePodDeleteStatusOkay()
}
