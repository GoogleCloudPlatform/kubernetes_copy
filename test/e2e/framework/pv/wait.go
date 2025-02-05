/*
Copyright 2024 The Kubernetes Authors.

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

package pv

import (
	"context"
	"fmt"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"
	"k8s.io/kubernetes/test/utils/format"
	"k8s.io/utils/ptr"
)

// WaitForPersistentVolumeClaimModified waits the given timeout duration for the specified claim to become bound with the
// desired volume attributes class.
// Returns an error if timeout occurs first.
func WaitForPersistentVolumeClaimModified(ctx context.Context, c clientset.Interface, claim *v1.PersistentVolumeClaim, timeout time.Duration) error {
	desiredClass := ptr.Deref(claim.Spec.VolumeAttributesClassName, "")

	var match = func(claim *v1.PersistentVolumeClaim) bool {
		for _, condition := range claim.Status.Conditions {
			// conditions that indicate the claim is being modified
			// or has an error when modifying the volume
			if condition.Type == v1.PersistentVolumeClaimVolumeModifyVolumeError ||
				condition.Type == v1.PersistentVolumeClaimVolumeModifyingVolume {
				return false
			}
		}

		// check if claim is bound with the desired volume attributes class
		currentClass := ptr.Deref(claim.Status.CurrentVolumeAttributesClassName, "")
		return claim.Status.Phase == v1.ClaimBound &&
			desiredClass == currentClass && claim.Status.ModifyVolumeStatus == nil
	}

	if match(claim) {
		return nil
	}

	return framework.Gomega().
		Eventually(ctx, framework.GetObject(c.CoreV1().PersistentVolumeClaims(claim.Namespace).Get, claim.Name, metav1.GetOptions{})).
		WithTimeout(timeout).
		Should(framework.MakeMatcher(func(claim *v1.PersistentVolumeClaim) (func() string, error) {
			if match(claim) {
				return nil, nil
			}

			return func() string {
				return fmt.Sprintf("expected claim's status to be modified with the given VolumeAttirbutesClass %s, got instead:\n%s", desiredClass, format.Object(claim, 1))
			}, nil
		}))
}

// WaitForPersistentVolumeClaimModificationFailure waits the given timeout duration for the specified claim to have
// failed to bind to the invalid volume attributes class.
// Returns an error if timeout occurs first.
func WaitForPersistentVolumeClaimModificationFailure(ctx context.Context, c clientset.Interface, claim *v1.PersistentVolumeClaim, timeout time.Duration) error {
	desiredClass := ptr.Deref(claim.Spec.VolumeAttributesClassName, "")

	var match = func(claim *v1.PersistentVolumeClaim) bool {
		for _, condition := range claim.Status.Conditions {
			if condition.Type != v1.PersistentVolumeClaimVolumeModifyVolumeError {
				return false
			}
		}

		// check if claim's current volume attributes class is NOT desired one, and has appropriate ModifyVolumeStatus
		currentClass := ptr.Deref(claim.Status.CurrentVolumeAttributesClassName, "")
		return claim.Status.Phase == v1.ClaimBound &&
			desiredClass != currentClass && claim.Status.ModifyVolumeStatus != nil &&
			(claim.Status.ModifyVolumeStatus.Status == v1.PersistentVolumeClaimModifyVolumeInProgress ||
				claim.Status.ModifyVolumeStatus.Status == v1.PersistentVolumeClaimModifyVolumeInfeasible)
	}

	if match(claim) {
		return nil
	}

	return framework.Gomega().
		Eventually(ctx, framework.GetObject(c.CoreV1().PersistentVolumeClaims(claim.Namespace).Get, claim.Name, metav1.GetOptions{})).
		WithTimeout(timeout).
		Should(framework.MakeMatcher(func(claim *v1.PersistentVolumeClaim) (func() string, error) {
			if match(claim) {
				return nil, nil
			}

			return func() string {
				return fmt.Sprintf("expected claim's status to NOT be modified with the given VolumeAttirbutesClass %s, got instead:\n%s", desiredClass, format.Object(claim, 1))
			}, nil
		}))
}
