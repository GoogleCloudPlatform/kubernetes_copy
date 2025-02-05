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

package v1beta1

import (
	admissionregistrationv1beta1 "k8s.io/api/admissionregistration/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	types "k8s.io/apimachinery/pkg/types"
	managedfields "k8s.io/apimachinery/pkg/util/managedfields"
	internal "k8s.io/client-go/applyconfigurations/internal"
	v1 "k8s.io/client-go/applyconfigurations/meta/v1"
)

// ValidatingAdmissionPolicyApplyConfiguration represents a declarative configuration of the ValidatingAdmissionPolicy type for use
// with apply.
type ValidatingAdmissionPolicyApplyConfiguration struct {
	v1.TypeMetaApplyConfiguration    `json:",inline"`
	*v1.ObjectMetaApplyConfiguration `json:"metadata,omitempty"`
	Spec                             *ValidatingAdmissionPolicySpecApplyConfiguration   `json:"spec,omitempty"`
	Status                           *ValidatingAdmissionPolicyStatusApplyConfiguration `json:"status,omitempty"`
}

// ValidatingAdmissionPolicy constructs a declarative configuration of the ValidatingAdmissionPolicy type for use with
// apply.
func ValidatingAdmissionPolicy(name string) *ValidatingAdmissionPolicyApplyConfiguration {
	b := &ValidatingAdmissionPolicyApplyConfiguration{}
	b.WithName(name)
	b.WithKind("ValidatingAdmissionPolicy")
	b.WithAPIVersion("admissionregistration.k8s.io/v1beta1")
	return b
}

// ExtractValidatingAdmissionPolicy extracts the applied configuration owned by fieldManager from
// validatingAdmissionPolicy. If no managedFields are found in validatingAdmissionPolicy for fieldManager, a
// ValidatingAdmissionPolicyApplyConfiguration is returned with only the Name, Namespace (if applicable),
// APIVersion and Kind populated. It is possible that no managed fields were found for because other
// field managers have taken ownership of all the fields previously owned by fieldManager, or because
// the fieldManager never owned fields any fields.
// validatingAdmissionPolicy must be a unmodified ValidatingAdmissionPolicy API object that was retrieved from the Kubernetes API.
// ExtractValidatingAdmissionPolicy provides a way to perform a extract/modify-in-place/apply workflow.
// Note that an extracted apply configuration will contain fewer fields than what the fieldManager previously
// applied if another fieldManager has updated or force applied any of the previously applied fields.
// Experimental!
func ExtractValidatingAdmissionPolicy(validatingAdmissionPolicy *admissionregistrationv1beta1.ValidatingAdmissionPolicy, fieldManager string) (*ValidatingAdmissionPolicyApplyConfiguration, error) {
	return extractValidatingAdmissionPolicy(validatingAdmissionPolicy, fieldManager, "")
}

// ExtractValidatingAdmissionPolicyStatus is the same as ExtractValidatingAdmissionPolicy except
// that it extracts the status subresource applied configuration.
// Experimental!
func ExtractValidatingAdmissionPolicyStatus(validatingAdmissionPolicy *admissionregistrationv1beta1.ValidatingAdmissionPolicy, fieldManager string) (*ValidatingAdmissionPolicyApplyConfiguration, error) {
	return extractValidatingAdmissionPolicy(validatingAdmissionPolicy, fieldManager, "status")
}

func extractValidatingAdmissionPolicy(validatingAdmissionPolicy *admissionregistrationv1beta1.ValidatingAdmissionPolicy, fieldManager string, subresource string) (*ValidatingAdmissionPolicyApplyConfiguration, error) {
	b := &ValidatingAdmissionPolicyApplyConfiguration{}
	err := managedfields.ExtractInto(validatingAdmissionPolicy, internal.Parser().Type("io.k8s.api.admissionregistration.v1beta1.ValidatingAdmissionPolicy"), fieldManager, b, subresource)
	if err != nil {
		return nil, err
	}
	b.WithName(validatingAdmissionPolicy.Name)

	b.WithKind("ValidatingAdmissionPolicy")
	b.WithAPIVersion("admissionregistration.k8s.io/v1beta1")
	return b, nil
}
func (b ValidatingAdmissionPolicyApplyConfiguration) IsApplyConfiguration() bool {
	return true
}

// WithKind sets the Kind field in the declarative configuration to the given value
// and returns the receiver, so that objects can be built by chaining "With" function invocations.
// If called multiple times, the Kind field is set to the value of the last call.
func (b *ValidatingAdmissionPolicyApplyConfiguration) WithKind(value string) *ValidatingAdmissionPolicyApplyConfiguration {
	b.TypeMetaApplyConfiguration.Kind = &value
	return b
}

// WithAPIVersion sets the APIVersion field in the declarative configuration to the given value
// and returns the receiver, so that objects can be built by chaining "With" function invocations.
// If called multiple times, the APIVersion field is set to the value of the last call.
func (b *ValidatingAdmissionPolicyApplyConfiguration) WithAPIVersion(value string) *ValidatingAdmissionPolicyApplyConfiguration {
	b.TypeMetaApplyConfiguration.APIVersion = &value
	return b
}

// WithName sets the Name field in the declarative configuration to the given value
// and returns the receiver, so that objects can be built by chaining "With" function invocations.
// If called multiple times, the Name field is set to the value of the last call.
func (b *ValidatingAdmissionPolicyApplyConfiguration) WithName(value string) *ValidatingAdmissionPolicyApplyConfiguration {
	b.ensureObjectMetaApplyConfigurationExists()
	b.ObjectMetaApplyConfiguration.Name = &value
	return b
}

// WithGenerateName sets the GenerateName field in the declarative configuration to the given value
// and returns the receiver, so that objects can be built by chaining "With" function invocations.
// If called multiple times, the GenerateName field is set to the value of the last call.
func (b *ValidatingAdmissionPolicyApplyConfiguration) WithGenerateName(value string) *ValidatingAdmissionPolicyApplyConfiguration {
	b.ensureObjectMetaApplyConfigurationExists()
	b.ObjectMetaApplyConfiguration.GenerateName = &value
	return b
}

// WithNamespace sets the Namespace field in the declarative configuration to the given value
// and returns the receiver, so that objects can be built by chaining "With" function invocations.
// If called multiple times, the Namespace field is set to the value of the last call.
func (b *ValidatingAdmissionPolicyApplyConfiguration) WithNamespace(value string) *ValidatingAdmissionPolicyApplyConfiguration {
	b.ensureObjectMetaApplyConfigurationExists()
	b.ObjectMetaApplyConfiguration.Namespace = &value
	return b
}

// WithUID sets the UID field in the declarative configuration to the given value
// and returns the receiver, so that objects can be built by chaining "With" function invocations.
// If called multiple times, the UID field is set to the value of the last call.
func (b *ValidatingAdmissionPolicyApplyConfiguration) WithUID(value types.UID) *ValidatingAdmissionPolicyApplyConfiguration {
	b.ensureObjectMetaApplyConfigurationExists()
	b.ObjectMetaApplyConfiguration.UID = &value
	return b
}

// WithResourceVersion sets the ResourceVersion field in the declarative configuration to the given value
// and returns the receiver, so that objects can be built by chaining "With" function invocations.
// If called multiple times, the ResourceVersion field is set to the value of the last call.
func (b *ValidatingAdmissionPolicyApplyConfiguration) WithResourceVersion(value string) *ValidatingAdmissionPolicyApplyConfiguration {
	b.ensureObjectMetaApplyConfigurationExists()
	b.ObjectMetaApplyConfiguration.ResourceVersion = &value
	return b
}

// WithGeneration sets the Generation field in the declarative configuration to the given value
// and returns the receiver, so that objects can be built by chaining "With" function invocations.
// If called multiple times, the Generation field is set to the value of the last call.
func (b *ValidatingAdmissionPolicyApplyConfiguration) WithGeneration(value int64) *ValidatingAdmissionPolicyApplyConfiguration {
	b.ensureObjectMetaApplyConfigurationExists()
	b.ObjectMetaApplyConfiguration.Generation = &value
	return b
}

// WithCreationTimestamp sets the CreationTimestamp field in the declarative configuration to the given value
// and returns the receiver, so that objects can be built by chaining "With" function invocations.
// If called multiple times, the CreationTimestamp field is set to the value of the last call.
func (b *ValidatingAdmissionPolicyApplyConfiguration) WithCreationTimestamp(value metav1.Time) *ValidatingAdmissionPolicyApplyConfiguration {
	b.ensureObjectMetaApplyConfigurationExists()
	b.ObjectMetaApplyConfiguration.CreationTimestamp = &value
	return b
}

// WithDeletionTimestamp sets the DeletionTimestamp field in the declarative configuration to the given value
// and returns the receiver, so that objects can be built by chaining "With" function invocations.
// If called multiple times, the DeletionTimestamp field is set to the value of the last call.
func (b *ValidatingAdmissionPolicyApplyConfiguration) WithDeletionTimestamp(value metav1.Time) *ValidatingAdmissionPolicyApplyConfiguration {
	b.ensureObjectMetaApplyConfigurationExists()
	b.ObjectMetaApplyConfiguration.DeletionTimestamp = &value
	return b
}

// WithDeletionGracePeriodSeconds sets the DeletionGracePeriodSeconds field in the declarative configuration to the given value
// and returns the receiver, so that objects can be built by chaining "With" function invocations.
// If called multiple times, the DeletionGracePeriodSeconds field is set to the value of the last call.
func (b *ValidatingAdmissionPolicyApplyConfiguration) WithDeletionGracePeriodSeconds(value int64) *ValidatingAdmissionPolicyApplyConfiguration {
	b.ensureObjectMetaApplyConfigurationExists()
	b.ObjectMetaApplyConfiguration.DeletionGracePeriodSeconds = &value
	return b
}

// WithLabels puts the entries into the Labels field in the declarative configuration
// and returns the receiver, so that objects can be build by chaining "With" function invocations.
// If called multiple times, the entries provided by each call will be put on the Labels field,
// overwriting an existing map entries in Labels field with the same key.
func (b *ValidatingAdmissionPolicyApplyConfiguration) WithLabels(entries map[string]string) *ValidatingAdmissionPolicyApplyConfiguration {
	b.ensureObjectMetaApplyConfigurationExists()
	if b.ObjectMetaApplyConfiguration.Labels == nil && len(entries) > 0 {
		b.ObjectMetaApplyConfiguration.Labels = make(map[string]string, len(entries))
	}
	for k, v := range entries {
		b.ObjectMetaApplyConfiguration.Labels[k] = v
	}
	return b
}

// WithAnnotations puts the entries into the Annotations field in the declarative configuration
// and returns the receiver, so that objects can be build by chaining "With" function invocations.
// If called multiple times, the entries provided by each call will be put on the Annotations field,
// overwriting an existing map entries in Annotations field with the same key.
func (b *ValidatingAdmissionPolicyApplyConfiguration) WithAnnotations(entries map[string]string) *ValidatingAdmissionPolicyApplyConfiguration {
	b.ensureObjectMetaApplyConfigurationExists()
	if b.ObjectMetaApplyConfiguration.Annotations == nil && len(entries) > 0 {
		b.ObjectMetaApplyConfiguration.Annotations = make(map[string]string, len(entries))
	}
	for k, v := range entries {
		b.ObjectMetaApplyConfiguration.Annotations[k] = v
	}
	return b
}

// WithOwnerReferences adds the given value to the OwnerReferences field in the declarative configuration
// and returns the receiver, so that objects can be build by chaining "With" function invocations.
// If called multiple times, values provided by each call will be appended to the OwnerReferences field.
func (b *ValidatingAdmissionPolicyApplyConfiguration) WithOwnerReferences(values ...*v1.OwnerReferenceApplyConfiguration) *ValidatingAdmissionPolicyApplyConfiguration {
	b.ensureObjectMetaApplyConfigurationExists()
	for i := range values {
		if values[i] == nil {
			panic("nil value passed to WithOwnerReferences")
		}
		b.ObjectMetaApplyConfiguration.OwnerReferences = append(b.ObjectMetaApplyConfiguration.OwnerReferences, *values[i])
	}
	return b
}

// WithFinalizers adds the given value to the Finalizers field in the declarative configuration
// and returns the receiver, so that objects can be build by chaining "With" function invocations.
// If called multiple times, values provided by each call will be appended to the Finalizers field.
func (b *ValidatingAdmissionPolicyApplyConfiguration) WithFinalizers(values ...string) *ValidatingAdmissionPolicyApplyConfiguration {
	b.ensureObjectMetaApplyConfigurationExists()
	for i := range values {
		b.ObjectMetaApplyConfiguration.Finalizers = append(b.ObjectMetaApplyConfiguration.Finalizers, values[i])
	}
	return b
}

func (b *ValidatingAdmissionPolicyApplyConfiguration) ensureObjectMetaApplyConfigurationExists() {
	if b.ObjectMetaApplyConfiguration == nil {
		b.ObjectMetaApplyConfiguration = &v1.ObjectMetaApplyConfiguration{}
	}
}

// WithSpec sets the Spec field in the declarative configuration to the given value
// and returns the receiver, so that objects can be built by chaining "With" function invocations.
// If called multiple times, the Spec field is set to the value of the last call.
func (b *ValidatingAdmissionPolicyApplyConfiguration) WithSpec(value *ValidatingAdmissionPolicySpecApplyConfiguration) *ValidatingAdmissionPolicyApplyConfiguration {
	b.Spec = value
	return b
}

// WithStatus sets the Status field in the declarative configuration to the given value
// and returns the receiver, so that objects can be built by chaining "With" function invocations.
// If called multiple times, the Status field is set to the value of the last call.
func (b *ValidatingAdmissionPolicyApplyConfiguration) WithStatus(value *ValidatingAdmissionPolicyStatusApplyConfiguration) *ValidatingAdmissionPolicyApplyConfiguration {
	b.Status = value
	return b
}

// GetName retrieves the value of the Name field in the declarative configuration.
func (b *ValidatingAdmissionPolicyApplyConfiguration) GetName() *string {
	b.ensureObjectMetaApplyConfigurationExists()
	return b.ObjectMetaApplyConfiguration.Name
}
