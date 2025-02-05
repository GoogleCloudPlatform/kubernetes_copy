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
	"reflect"
	"regexp"
	"sort"

	"k8s.io/apimachinery/pkg/util/validation/field"
)

var intOrStringAnyOf = []NestedValueValidation{
	{ForbiddenGenerics: Generic{
		Type: "integer",
	}},
	{ForbiddenGenerics: Generic{
		Type: "string",
	}},
}

type level int

const (
	rootLevel level = iota
	itemLevel
	fieldLevel
)

type ValidationOptions struct {
	// AllowNestedAdditionalProperties allows additionalProperties to be specified in
	// nested contexts (allOf, anyOf, oneOf, not).
	AllowNestedAdditionalProperties bool

	// AllowNestedXValidations allows x-kubernetes-validations to be specified in
	// nested contexts (allOf, anyOf, oneOf, not).
	AllowNestedXValidations bool

	// AllowValidationPropertiesWithAdditionalProperties allows
	// value validations on specific properties that are structurally
	// defined by additionalProperties. i.e. additionalProperties in main structural
	// schema, but properties within an allOf.
	AllowValidationPropertiesWithAdditionalProperties bool
}

// ValidateStructural checks that s is a structural schema with the invariants:
//
// * structurality: both `ForbiddenGenerics` and `ForbiddenExtensions` only have zero values, with the two exceptions for IntOrString.
// * RawExtension: for every schema with `x-kubernetes-embedded-resource: true`, `x-kubernetes-preserve-unknown-fields: true` and `type: object` are set
// * IntOrString: for `x-kubernetes-int-or-string: true` either `type` is empty under `anyOf` and `allOf` or the schema structure is one of these:
//
//  1. anyOf:
//     - type: integer
//     - type: string
//  2. allOf:
//     - anyOf:
//     - type: integer
//     - type: string
//     - ... zero or more
//
// * every specified field or array in s is also specified outside of value validation.
// * metadata at the root can only restrict the name and generateName, and not be specified at all in nested contexts.
// * additionalProperties at the root is not allowed.
func ValidateStructural(fldPath *field.Path, s *Structural) field.ErrorList {
	return ValidateStructuralWithOptions(fldPath, s, ValidationOptions{
		// This would widen the schema for CRD if set to true, so first few releases will still
		// not admit any. But it can still be used by libraries and
		// declarative validation for native types
		AllowNestedAdditionalProperties:                   false,
		AllowNestedXValidations:                           false,
		AllowValidationPropertiesWithAdditionalProperties: false,
	})
}

func ValidateStructuralWithOptions(fldPath *field.Path, s *Structural, opts ValidationOptions) field.ErrorList {
	allErrs := field.ErrorList{}

	allErrs = append(allErrs, validateStructuralInvariants(s, rootLevel, fldPath, opts)...)
	allErrs = append(allErrs, validateStructuralCompleteness(s, fldPath, opts)...)

	// sort error messages. Otherwise, the errors slice will change every time due to
	// maps in the types and randomized iteration.
	sort.Slice(allErrs, func(i, j int) bool {
		return allErrs[i].Error() < allErrs[j].Error()
	})

	return allErrs
}

// validateStructuralInvariants checks the invariants of a structural schema.
func validateStructuralInvariants(s *Structural, lvl level, fldPath *field.Path, opts ValidationOptions) field.ErrorList {
	if s == nil {
		return nil
	}

	allErrs := field.ErrorList{}

	if s.Type == "array" && s.Items == nil {
		allErrs = append(allErrs, field.Required(fldPath.Child("items"), "must be specified"))
	}
	allErrs = append(allErrs, validateStructuralInvariants(s.Items, itemLevel, fldPath.Child("items"), opts)...)

	for k, v := range s.Properties {
		allErrs = append(allErrs, validateStructuralInvariants(&v, fieldLevel, fldPath.Child("properties").Key(k), opts)...)
	}

	if s.AdditionalProperties != nil {
		if lvl == rootLevel {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("additionalProperties"), "must not be used at the root"))
		}
		if s.AdditionalProperties.Structural != nil {
			allErrs = append(allErrs, validateStructuralInvariants(s.AdditionalProperties.Structural, fieldLevel, fldPath.Child("additionalProperties"), opts)...)
		}
	}

	allErrs = append(allErrs, validateGeneric(&s.Generic, lvl, fldPath)...)
	allErrs = append(allErrs, validateExtensions(&s.Extensions, fldPath)...)

	// detect the two IntOrString exceptions:
	// 1) anyOf:
	//    - type: integer
	//    - type: string
	// 2) allOf:
	//    - anyOf:
	//      - type: integer
	//      - type: string
	//    - ... zero or more
	skipAnyOf := isIntOrStringAnyOfPattern(s)
	skipFirstAllOfAnyOf := isIntOrStringAllOfPattern(s)

	allErrs = append(allErrs, validateValueValidation(s.ValueValidation, skipAnyOf, skipFirstAllOfAnyOf, lvl, fldPath, opts)...)

	checkMetadata := (lvl == rootLevel) || s.XEmbeddedResource

	if s.XEmbeddedResource && s.Type != "object" {
		if len(s.Type) == 0 {
			allErrs = append(allErrs, field.Required(fldPath.Child("type"), "must be object if x-kubernetes-embedded-resource is true"))
		} else {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("type"), s.Type, "must be object if x-kubernetes-embedded-resource is true"))
		}
	} else if len(s.Type) == 0 && !s.Extensions.XIntOrString && !s.Extensions.XPreserveUnknownFields {
		switch lvl {
		case rootLevel:
			allErrs = append(allErrs, field.Required(fldPath.Child("type"), "must not be empty at the root"))
		case itemLevel:
			allErrs = append(allErrs, field.Required(fldPath.Child("type"), "must not be empty for specified array items"))
		case fieldLevel:
			allErrs = append(allErrs, field.Required(fldPath.Child("type"), "must not be empty for specified object fields"))
		}
	}
	if s.XEmbeddedResource && s.AdditionalProperties != nil {
		allErrs = append(allErrs, field.Forbidden(fldPath.Child("additionalProperties"), "must not be used if x-kubernetes-embedded-resource is set"))
	}

	if lvl == rootLevel && len(s.Type) > 0 && s.Type != "object" {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("type"), s.Type, "must be object at the root"))
	}

	// restrict metadata schemas to name and generateName only
	if kind, found := s.Properties["kind"]; found && checkMetadata {
		if kind.Type != "string" {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("properties").Key("kind").Child("type"), kind.Type, "must be string"))
		}
	}
	if apiVersion, found := s.Properties["apiVersion"]; found && checkMetadata {
		if apiVersion.Type != "string" {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("properties").Key("apiVersion").Child("type"), apiVersion.Type, "must be string"))
		}
	}

	if metadata, found := s.Properties["metadata"]; found {
		allErrs = append(allErrs, validateStructuralMetadataInvariants(&metadata, checkMetadata, lvl, fldPath.Child("properties").Key("metadata"))...)
	}

	if s.XEmbeddedResource && !s.XPreserveUnknownFields && len(s.Properties) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("properties"), "must not be empty if x-kubernetes-embedded-resource is true without x-kubernetes-preserve-unknown-fields"))
	}

	return allErrs
}

func validateStructuralMetadataInvariants(s *Structural, checkMetadata bool, lvl level, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}

	if checkMetadata && s.Type != "object" {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("type"), s.Type, "must be object"))
	}

	if lvl == rootLevel {
		// metadata is a shallow copy. We can mutate it.
		_, foundName := s.Properties["name"]
		_, foundGenerateName := s.Properties["generateName"]
		if foundName && foundGenerateName && len(s.Properties) == 2 {
			s.Properties = nil
		} else if (foundName || foundGenerateName) && len(s.Properties) == 1 {
			s.Properties = nil
		}
		s.Type = ""
		s.Default.Object = nil // this is checked in API validation (and also tested)
		if s.ValueValidation == nil {
			s.ValueValidation = &ValueValidation{}
		}
		if !reflect.DeepEqual(*s, Structural{ValueValidation: &ValueValidation{}}) {
			// TODO: this is actually a field.Invalid error, but we cannot do JSON serialization of metadata here to get a proper message
			allErrs = append(allErrs, field.Forbidden(fldPath, "must not specify anything other than name and generateName, but metadata is implicitly specified"))
		}
	}

	return allErrs
}

func isIntOrStringAnyOfPattern(s *Structural) bool {
	if s == nil || s.ValueValidation == nil {
		return false
	}
	return len(s.ValueValidation.AnyOf) == 2 && reflect.DeepEqual(s.ValueValidation.AnyOf, intOrStringAnyOf)
}

func isIntOrStringAllOfPattern(s *Structural) bool {
	if s == nil || s.ValueValidation == nil {
		return false
	}
	return len(s.ValueValidation.AllOf) >= 1 && len(s.ValueValidation.AllOf[0].AnyOf) == 2 && reflect.DeepEqual(s.ValueValidation.AllOf[0].AnyOf, intOrStringAnyOf)
}

// validateGeneric checks the generic fields of a structural schema.
func validateGeneric(g *Generic, lvl level, fldPath *field.Path) field.ErrorList {
	if g == nil {
		return nil
	}

	return nil
}

// validateExtensions checks Kubernetes vendor extensions of a structural schema.
func validateExtensions(x *Extensions, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}

	if x.XIntOrString && x.XPreserveUnknownFields {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("x-kubernetes-preserve-unknown-fields"), x.XPreserveUnknownFields, "must be false if x-kubernetes-int-or-string is true"))
	}
	if x.XIntOrString && x.XEmbeddedResource {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("x-kubernetes-embedded-resource"), x.XEmbeddedResource, "must be false if x-kubernetes-int-or-string is true"))
	}

	return allErrs
}

// validateValueValidation checks the value validation in a structural schema.
func validateValueValidation(v *ValueValidation, skipAnyOf, skipFirstAllOfAnyOf bool, lvl level, fldPath *field.Path, opts ValidationOptions) field.ErrorList {
	if v == nil {
		return nil
	}

	allErrs := field.ErrorList{}

	// We still unconditionally forbid XValidations in quantifiers, the only
	// quantifier that is allowed to have right now is AllOf. This is due to
	// implementation constraints - the SchemaValidator would need to become
	// aware of CEL to properly implement the other quantifiers.
	optsWithCELDisabled := opts
	optsWithCELDisabled.AllowNestedXValidations = false

	if !skipAnyOf {
		for i := range v.AnyOf {
			allErrs = append(allErrs, validateNestedValueValidation(&v.AnyOf[i], false, false, lvl, fldPath.Child("anyOf").Index(i), optsWithCELDisabled)...)
		}
	}

	for i := range v.AllOf {
		skipAnyOf := false
		if skipFirstAllOfAnyOf && i == 0 {
			skipAnyOf = true
		}
		allErrs = append(allErrs, validateNestedValueValidation(&v.AllOf[i], skipAnyOf, false, lvl, fldPath.Child("allOf").Index(i), opts)...)
	}

	for i := range v.OneOf {
		allErrs = append(allErrs, validateNestedValueValidation(&v.OneOf[i], false, false, lvl, fldPath.Child("oneOf").Index(i), optsWithCELDisabled)...)
	}

	allErrs = append(allErrs, validateNestedValueValidation(v.Not, false, false, lvl, fldPath.Child("not"), optsWithCELDisabled)...)

	if len(v.Pattern) > 0 {
		if _, err := regexp.Compile(v.Pattern); err != nil {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("pattern"), v.Pattern, fmt.Sprintf("must be a valid regular expression, but isn't: %v", err)))
		}
	}

	return allErrs
}

// validateNestedValueValidation checks the nested value validation under a logic junctor in a structural schema.
func validateNestedValueValidation(v *NestedValueValidation, skipAnyOf, skipAllOfAnyOf bool, lvl level, fldPath *field.Path, opts ValidationOptions) field.ErrorList {
	if v == nil {
		return nil
	}

	allErrs := field.ErrorList{}

	allErrs = append(allErrs, validateValueValidation(&v.ValueValidation, skipAnyOf, skipAllOfAnyOf, lvl, fldPath, opts)...)
	allErrs = append(allErrs, validateNestedValueValidation(v.Items, false, false, lvl, fldPath.Child("items"), opts)...)

	for k, fld := range v.Properties {
		allErrs = append(allErrs, validateNestedValueValidation(&fld, false, false, fieldLevel, fldPath.Child("properties").Key(k), opts)...)
	}

	if len(v.ForbiddenGenerics.Type) > 0 {
		allErrs = append(allErrs, field.Forbidden(fldPath.Child("type"), "must be empty to be structural"))
	}
	if v.AdditionalProperties != nil && !opts.AllowNestedAdditionalProperties {
		allErrs = append(allErrs, field.Forbidden(fldPath.Child("additionalProperties"), "must be undefined to be structural"))
	} else {
		allErrs = append(allErrs, validateNestedValueValidation(v.AdditionalProperties, false, false, lvl, fldPath.Child("additionalProperties"), opts)...)
	}
	if v.ForbiddenGenerics.Default.Object != nil {
		allErrs = append(allErrs, field.Forbidden(fldPath.Child("default"), "must be undefined to be structural"))
	}
	if len(v.ForbiddenGenerics.Title) > 0 {
		allErrs = append(allErrs, field.Forbidden(fldPath.Child("title"), "must be empty to be structural"))
	}
	if len(v.ForbiddenGenerics.Description) > 0 {
		allErrs = append(allErrs, field.Forbidden(fldPath.Child("description"), "must be empty to be structural"))
	}
	if v.ForbiddenGenerics.Nullable {
		allErrs = append(allErrs, field.Forbidden(fldPath.Child("nullable"), "must be false to be structural"))
	}

	if v.ForbiddenExtensions.XPreserveUnknownFields {
		allErrs = append(allErrs, field.Forbidden(fldPath.Child("x-kubernetes-preserve-unknown-fields"), "must be false to be structural"))
	}
	if v.ForbiddenExtensions.XEmbeddedResource {
		allErrs = append(allErrs, field.Forbidden(fldPath.Child("x-kubernetes-embedded-resource"), "must be false to be structural"))
	}
	if v.ForbiddenExtensions.XIntOrString {
		allErrs = append(allErrs, field.Forbidden(fldPath.Child("x-kubernetes-int-or-string"), "must be false to be structural"))
	}
	if len(v.ForbiddenExtensions.XListMapKeys) > 0 {
		allErrs = append(allErrs, field.Forbidden(fldPath.Child("x-kubernetes-list-map-keys"), "must be empty to be structural"))
	}
	if v.ForbiddenExtensions.XListType != nil {
		allErrs = append(allErrs, field.Forbidden(fldPath.Child("x-kubernetes-list-type"), "must be undefined to be structural"))
	}
	if v.ForbiddenExtensions.XMapType != nil {
		allErrs = append(allErrs, field.Forbidden(fldPath.Child("x-kubernetes-map-type"), "must be undefined to be structural"))
	}
	if len(v.ValidationExtensions.XValidations) > 0 && !opts.AllowNestedXValidations {
		allErrs = append(allErrs, field.Forbidden(fldPath.Child("x-kubernetes-validations"), "must be empty to be structural"))
	}

	// forbid reasoning about metadata because it can lead to metadata restriction we don't want
	if _, found := v.Properties["metadata"]; found {
		allErrs = append(allErrs, field.Forbidden(fldPath.Child("properties").Key("metadata"), "must not be specified in a nested context"))
	}

	return allErrs
}
