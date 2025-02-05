/*
Copyright The Kubernetes Authors.

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

// Code generated by applyconfiguration-gen. DO NOT EDIT.

package v1

import (
	corev1 "k8s.io/api/core/v1"
)

// ContainerResizePolicyApplyConfiguration represents a declarative configuration of the ContainerResizePolicy type for use
// with apply. Note: The `resizePolicy` field is not supported for ephemeral containers.
type ContainerResizePolicyApplyConfiguration struct {
	ResourceName  *corev1.ResourceName                `json:"resourceName,omitempty"`
	RestartPolicy *corev1.ResourceResizeRestartPolicy `json:"restartPolicy,omitempty"`
}

// ContainerResizePolicyApplyConfiguration constructs a declarative configuration of the ContainerResizePolicy type for use with
// apply. Note: The `resizePolicy` field is not supported for ephemeral containers.
func ContainerResizePolicy() *ContainerResizePolicyApplyConfiguration {
	return &ContainerResizePolicyApplyConfiguration{}
}

// WithResourceName sets the ResourceName field in the declarative configuration to the given value
// and returns the receiver, so that objects can be built by chaining "With" function invocations.
// If called multiple times, the ResourceName field is set to the value of the last call.
func (b *ContainerResizePolicyApplyConfiguration) WithResourceName(value corev1.ResourceName) *ContainerResizePolicyApplyConfiguration {
	b.ResourceName = &value
	return b
}

// WithRestartPolicy sets the RestartPolicy field in the declarative configuration to the given value
// and returns the receiver, so that objects can be built by chaining "With" function invocations.
// If called multiple times, the RestartPolicy field is set to the value of the last call.
// Note: The `resizePolicy` field is not supported for ephemeral containers.
func (b *ContainerResizePolicyApplyConfiguration) WithRestartPolicy(value corev1.ResourceResizeRestartPolicy) *ContainerResizePolicyApplyConfiguration {
	b.RestartPolicy = &value
	return b
}
