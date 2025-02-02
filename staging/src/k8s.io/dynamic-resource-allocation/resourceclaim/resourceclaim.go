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

// Package resourceclaim provides code that supports the usual pattern
// for accessing the ResourceClaim that is referenced by a PodResourceClaim:
//
// - determine the ResourceClaim name that corresponds to the PodResourceClaim
// - retrieve the ResourceClaim
// - verify that the ResourceClaim is owned by the pod if generated from a template
// - use the ResourceClaim
package resourceclaim

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	v1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var (
	// ErrAPIUnsupported is wrapped by the actual errors returned by Name and
	// indicates that none of the required fields are set.
	ErrAPIUnsupported = errors.New("none of the supported fields are set")

	// ErrClaimNotFound is wrapped by the actual errors returned by Name and
	// indicates that the claim has not been created yet.
	ErrClaimNotFound = errors.New("ResourceClaim not created yet")
)

// Name returns the name of the ResourceClaim object that gets referenced by or
// created for the PodResourceClaim. Three different results are possible:
//
//   - An error is returned when some field is not set as expected (either the
//     input is invalid or the API got extended and the library and the client
//     using it need to be updated) or the claim hasn't been created yet.
//
//     The error includes pod and pod claim name and the unexpected field and
//     is derived from one of the pre-defined errors in this package.
//
//   - A nil string pointer and no error when the ResourceClaim intentionally
//     didn't get created and the PodResourceClaim can be ignored.
//
//   - A pointer to the name and no error when the ResourceClaim got created.
//     In this case the boolean determines whether IsForPod must be called
//     after retrieving the ResourceClaim and before using it.
//
// Determining the name depends on Kubernetes >= 1.28.
func Name(pod *v1.Pod, podClaim *v1.PodResourceClaim) (name *string, mustCheckOwner bool, err error) {
	switch {
	case podClaim.ResourceClaimName != nil:
		return podClaim.ResourceClaimName, false, nil
	case podClaim.ResourceClaimTemplateName != nil:
		for _, status := range pod.Status.ResourceClaimStatuses {
			if status.Name == podClaim.Name {
				return status.ResourceClaimName, true, nil
			}
		}
		return nil, false, fmt.Errorf(`pod "%s/%s": %w`, pod.Namespace, pod.Name, ErrClaimNotFound)
	default:
		return nil, false, fmt.Errorf(`pod "%s/%s", spec.resourceClaim %q: %w`, pod.Namespace, pod.Name, podClaim.Name, ErrAPIUnsupported)
	}
}

// IsForPod checks that the ResourceClaim is the one that
// was created for the Pod. It returns an error that is informative
// enough to be returned by the caller without adding further details
// about the Pod or ResourceClaim.
func IsForPod(pod *v1.Pod, claim *resourceapi.ResourceClaim) error {
	// Checking the namespaces is just a precaution. The caller should
	// never pass in a ResourceClaim that isn't from the same namespace as the
	// Pod.
	if claim.Namespace != pod.Namespace || !metav1.IsControlledBy(claim, pod) {
		return fmt.Errorf("ResourceClaim %s/%s was not created for pod %s/%s (pod is not owner)", claim.Namespace, claim.Name, pod.Namespace, pod.Name)
	}
	return nil
}

// IsReservedForPod checks whether a claim lists the pod as one of the objects
// that the claim was reserved for.
func IsReservedForPod(pod *v1.Pod, claim *resourceapi.ResourceClaim) bool {
	for _, reserved := range claim.Status.ReservedFor {
		if reserved.UID == pod.UID {
			return true
		}
	}
	return false
}

// CanBeReserved checks whether the claim could be reserved for another object.
func CanBeReserved(claim *resourceapi.ResourceClaim) bool {
	// Currently no restrictions on sharing...
	return true
}

// BaseRequestRef returns the request name if the reference is to a top-level
// request and the name of the parent request if the reference is to a subrequest.
func BaseRequestRef(requestRef string) string {
	segments := strings.Split(requestRef, "/")
	return segments[0]
}

// ConfigForResult returns the configs that are applicable to device
// allocated for the provided result.
func ConfigForResult(deviceConfigurations []resourceapi.DeviceAllocationConfiguration, result resourceapi.DeviceRequestAllocationResult, driver string) []resourceapi.DeviceAllocationConfiguration {
	var configs []resourceapi.DeviceAllocationConfiguration
	for _, deviceConfiguration := range deviceConfigurations {
		if deviceConfiguration.Opaque != nil &&
			deviceConfiguration.Opaque.Driver == driver &&
			isMatch(deviceConfiguration.Requests, result.Request) {
			configs = append(configs, deviceConfiguration)
		}
	}
	return configs
}

func isMatch(requests []string, requestRef string) bool {
	if len(requests) == 0 {
		return true
	}

	if slices.Contains(requests, requestRef) {
		return true
	}

	baseRequestRef := BaseRequestRef(requestRef)
	return slices.Contains(requests, baseRequestRef)
}
