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

package limitranger

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apiserver/pkg/admission"
)

// LimitRangerActions is an interface defining actions to be carried over ranges to identify and manipulate their limits
type LimitRangerActions interface {
	// MutateLimit is a pluggable function to set limits on the object.
	MutateLimit(limitRange *corev1.LimitRange, kind string, obj runtime.Object) error
	// ValidateLimits is a pluggable function to enforce limits on the object.
	ValidateLimit(limitRange *corev1.LimitRange, kind string, obj runtime.Object) error
	// SupportsAttributes is a pluggable function to allow overriding what resources the limitranger
	// supports.
	SupportsAttributes(attr admission.Attributes) bool
	// SupportsLimit is a pluggable function to allow ignoring limits that should not be applied
	// for any reason.
	SupportsLimit(limitRange *corev1.LimitRange) bool
}
