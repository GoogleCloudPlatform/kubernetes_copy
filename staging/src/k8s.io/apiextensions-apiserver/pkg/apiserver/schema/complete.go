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

package schema

import (
	"fmt"

	"k8s.io/apimachinery/pkg/util/validation/field"
)

// validateStructuralCompleteness checks that all value validations in s have
// a structural counterpart so that every value validation applies to a value
// with a known schema:
// - validations for specific properties must have that property (or additionalProperties under an option) structurally defined
// - additionalProperties validations must have additionalProperties defined in the structural portion of the schema corresponding to that node
// - Items validations must also have a corresponding items structurally
//
// The "structural" portion of the schema refers to all nodes in the
// schema traversable without following any NestedValueValidations.
func validateStructuralCompleteness(s *Structural, fldPath *field.Path, opts ValidationOptions) field.ErrorList {
	if s == nil {
		return nil
	}

	return validateValueValidationCompleteness(s.ValueValidation, s, fldPath, fldPath, opts)
}

func validateValueValidationCompleteness(v *ValueValidation, s *Structural, sPath, vPath *field.Path, opts ValidationOptions) field.ErrorList {
	if v == nil {
		return nil
	}
	if s == nil {
		return field.ErrorList{field.Required(sPath, fmt.Sprintf("because it is defined in %s", vPath.String()))}
	}

	allErrs := field.ErrorList{}

	allErrs = append(allErrs, validateNestedValueValidationCompleteness(v.Not, s, sPath, vPath.Child("not"), opts)...)
	for i := range v.AllOf {
		allErrs = append(allErrs, validateNestedValueValidationCompleteness(&v.AllOf[i], s, sPath, vPath.Child("allOf").Index(i), opts)...)
	}
	for i := range v.AnyOf {
		allErrs = append(allErrs, validateNestedValueValidationCompleteness(&v.AnyOf[i], s, sPath, vPath.Child("anyOf").Index(i), opts)...)
	}
	for i := range v.OneOf {
		allErrs = append(allErrs, validateNestedValueValidationCompleteness(&v.OneOf[i], s, sPath, vPath.Child("oneOf").Index(i), opts)...)
	}

	return allErrs
}

func validateNestedValueValidationCompleteness(v *NestedValueValidation, s *Structural, sPath, vPath *field.Path, opts ValidationOptions) field.ErrorList {
	if v == nil {
		return nil
	}
	if s == nil {
		return field.ErrorList{field.Required(sPath, fmt.Sprintf("because it is defined in %s", vPath.String()))}
	}

	allErrs := field.ErrorList{}

	allErrs = append(allErrs, validateValueValidationCompleteness(&v.ValueValidation, s, sPath, vPath, opts)...)
	allErrs = append(allErrs, validateNestedValueValidationCompleteness(v.Items, s.Items, sPath.Child("items"), vPath.Child("items"), opts)...)

	var sAdditionalPropertiesSchema *Structural
	if s.AdditionalProperties != nil {
		sAdditionalPropertiesSchema = s.AdditionalProperties.Structural
	}

	for k, vFld := range v.Properties {
		if sFld, ok := s.Properties[k]; !ok {
			if sAdditionalPropertiesSchema == nil || !opts.AllowValidationPropertiesWithAdditionalProperties {
				allErrs = append(allErrs, field.Required(sPath.Child("properties").Key(k), fmt.Sprintf("because it is defined in %s", vPath.Child("properties").Key(k))))
			} else {
				// Allow validations on specific properties if there exists an
				// additionalProperties structural schema specified instead of
				// direct properties
				// NOTE: This does not allow `additionalProperties: true` structural
				// schema to be combined with specific property validations.
				allErrs = append(allErrs, validateNestedValueValidationCompleteness(&vFld, sAdditionalPropertiesSchema, sPath.Child("additionalProperties"), vPath.Child("properties").Key(k), opts)...)
			}
		} else {
			allErrs = append(allErrs, validateNestedValueValidationCompleteness(&vFld, &sFld, sPath.Child("properties").Key(k), vPath.Child("properties").Key(k), opts)...)
		}
	}

	if v.AdditionalProperties != nil && opts.AllowNestedAdditionalProperties {
		allErrs = append(allErrs, validateNestedValueValidationCompleteness(v.AdditionalProperties, sAdditionalPropertiesSchema, sPath.Child("additionalProperties"), vPath.Child("additionalProperties"), opts)...)
	}

	return allErrs
}
