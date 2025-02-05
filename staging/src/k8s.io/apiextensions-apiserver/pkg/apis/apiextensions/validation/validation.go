/*
Copyright 2017 The Kubernetes Authors.

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

package validation

import (
	"context"
	"fmt"
	"math"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	celgo "github.com/google/cel-go/cel"

	"k8s.io/apiextensions-apiserver/pkg/apihelpers"
	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsv1beta1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1beta1"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	structuraldefaulting "k8s.io/apiextensions-apiserver/pkg/apiserver/schema/defaulting"
	apiservervalidation "k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	genericvalidation "k8s.io/apimachinery/pkg/api/validation"
	"k8s.io/apimachinery/pkg/util/sets"
	utilvalidation "k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
	apiservercel "k8s.io/apiserver/pkg/cel"
	"k8s.io/apiserver/pkg/cel/environment"
	"k8s.io/apiserver/pkg/util/webhook"
)

var (
	printerColumnDatatypes                = sets.NewString("integer", "number", "string", "boolean", "date")
	customResourceColumnDefinitionFormats = sets.NewString("int32", "int64", "float", "double", "byte", "date", "date-time", "password")
	openapiV3Types                        = sets.NewString("string", "number", "integer", "boolean", "array", "object")
)

const (
	// StaticEstimatedCostLimit represents the largest-allowed static CEL cost on a per-expression basis.
	StaticEstimatedCostLimit = 10000000
	// StaticEstimatedCRDCostLimit represents the largest-allowed total cost for the x-kubernetes-validations rules of a CRD.
	StaticEstimatedCRDCostLimit = 100000000

	MaxSelectableFields = 8
)

var supportedValidationReason = sets.NewString(
	string(apiextensions.FieldValueRequired),
	string(apiextensions.FieldValueForbidden),
	string(apiextensions.FieldValueInvalid),
	string(apiextensions.FieldValueDuplicate),
)

// ValidateCustomResourceDefinition statically validates
// context is passed for supporting context cancellation during cel validation when validating defaults
func ValidateCustomResourceDefinition(ctx context.Context, obj *apiextensions.CustomResourceDefinition) field.ErrorList {
	nameValidationFn := func(name string, prefix bool) []string {
		ret := genericvalidation.NameIsDNSSubdomain(name, prefix)
		requiredName := obj.Spec.Names.Plural + "." + obj.Spec.Group
		if name != requiredName {
			ret = append(ret, fmt.Sprintf(`must be spec.names.plural+"."+spec.group`))
		}
		return ret
	}

	opts := validationOptions{
		allowDefaults:                            true,
		requireRecognizedConversionReviewVersion: true,
		requireImmutableNames:                    false,
		requireOpenAPISchema:                     true,
		requireValidPropertyType:                 true,
		requireStructuralSchema:                  true,
		requirePrunedDefaults:                    true,
		requireAtomicSetType:                     true,
		requireMapListKeysMapSetValidation:       true,
		// strictCost is always true to enforce cost limits.
		celEnvironmentSet: environment.MustBaseEnvSet(environment.DefaultCompatibilityVersion(), true),
		// allowInvalidCABundle is set to true since the CRD is not established yet.
		allowInvalidCABundle: true,
	}

	allErrs := genericvalidation.ValidateObjectMeta(&obj.ObjectMeta, false, nameValidationFn, field.NewPath("metadata"))
	allErrs = append(allErrs, validateCustomResourceDefinitionSpec(ctx, &obj.Spec, opts, field.NewPath("spec"))...)
	allErrs = append(allErrs, ValidateCustomResourceDefinitionStatus(&obj.Status, field.NewPath("status"))...)
	allErrs = append(allErrs, ValidateCustomResourceDefinitionStoredVersions(obj.Status.StoredVersions, obj.Spec.Versions, field.NewPath("status").Child("storedVersions"))...)
	allErrs = append(allErrs, validateAPIApproval(obj, nil)...)
	allErrs = append(allErrs, validatePreserveUnknownFields(obj, nil)...)
	return allErrs
}

// validationOptions groups several validation options, to avoid passing multiple bool parameters to methods
type validationOptions struct {
	// allowDefaults permits the validation schema to contain default attributes
	allowDefaults bool
	// disallowDefaultsReason gives a reason as to why allowDefaults is false (for better user feedback)
	disallowDefaultsReason string
	// requireRecognizedConversionReviewVersion requires accepted webhook conversion versions to contain a recognized version
	requireRecognizedConversionReviewVersion bool
	// requireImmutableNames disables changing spec.names
	requireImmutableNames bool
	// requireOpenAPISchema requires an openapi V3 schema be specified
	requireOpenAPISchema bool
	// requireValidPropertyType requires property types specified in the validation schema to be valid openapi v3 types
	requireValidPropertyType bool
	// requireStructuralSchema indicates that any schemas present must be structural
	requireStructuralSchema bool
	// requirePrunedDefaults indicates that defaults must be pruned
	requirePrunedDefaults bool
	// requireAtomicSetType indicates that the items type for a x-kubernetes-list-type=set list must be atomic.
	requireAtomicSetType bool
	// requireMapListKeysMapSetValidation indicates that:
	// 1. For x-kubernetes-list-type=map list, key fields are not nullable, and are required or have a default
	// 2. For x-kubernetes-list-type=map or x-kubernetes-list-type=set list, the whole item must not be nullable.
	requireMapListKeysMapSetValidation bool
	// preexistingExpressions tracks which CEL expressions existed in an object before an update. May be nil for create.
	preexistingExpressions preexistingExpressions
	// versionsWithUnchangedSchemas tracks schemas of which versions are unchanged when updating a CRD.
	// Does not apply to creation or deletion.
	// Some checks use this to avoid rejecting previously accepted versions due to a control plane upgrade/downgrade.
	versionsWithUnchangedSchemas sets.Set[string]
	// suppressPerExpressionCost indicates whether CEL per-expression cost limit should be suppressed.
	// It will be automatically set during Versions validation if the version is in versionsWithUnchangedSchemas.
	suppressPerExpressionCost bool

	celEnvironmentSet *environment.EnvSet
	// allowInvalidCABundle allows an invalid conversion webhook CABundle on update only if the existing CABundle is invalid.
	// An invalid CABundle is also permitted on create and before a CRD is in an Established=True condition.
	allowInvalidCABundle bool
}

type preexistingExpressions struct {
	rules              sets.Set[string]
	messageExpressions sets.Set[string]
}

func (pe preexistingExpressions) RuleEnv(envSet *environment.EnvSet, expression string) *celgo.Env {
	if pe.rules.Has(expression) {
		return envSet.StoredExpressionsEnv()
	}
	return envSet.NewExpressionsEnv()
}

func (pe preexistingExpressions) MessageExpressionEnv(envSet *environment.EnvSet, expression string) *celgo.Env {
	if pe.messageExpressions.Has(expression) {
		return envSet.StoredExpressionsEnv()
	}
	return envSet.NewExpressionsEnv()
}

func findPreexistingExpressions(spec *apiextensions.CustomResourceDefinitionSpec) preexistingExpressions {
	expressions := preexistingExpressions{rules: sets.New[string](), messageExpressions: sets.New[string]()}
	if spec.Validation != nil && spec.Validation.OpenAPIV3Schema != nil {
		findPreexistingExpressionsInSchema(spec.Validation.OpenAPIV3Schema, expressions)
	}
	for _, v := range spec.Versions {
		if v.Schema != nil && v.Schema.OpenAPIV3Schema != nil {
			findPreexistingExpressionsInSchema(v.Schema.OpenAPIV3Schema, expressions)
		}
	}
	return expressions
}

func findPreexistingExpressionsInSchema(schema *apiextensions.JSONSchemaProps, expressions preexistingExpressions) {
	SchemaHas(schema, func(s *apiextensions.JSONSchemaProps) bool {
		for _, v := range s.XValidations {
			expressions.rules.Insert(v.Rule)
			if len(v.MessageExpression) > 0 {
				expressions.messageExpressions.Insert(v.MessageExpression)
			}
		}
		return false
	})
}

// findVersionsWithUnchangedSchemas finds each version that is in the new CRD object and differs from that of the old CRD object.
// It returns a set of the names of mutated versions.
// This function does not check for duplicated versions, top-level version not in versions, or coexistence of
// top-level and per-version schemas, as further validations will check for these problems.
func findVersionsWithUnchangedSchemas(obj, oldObject *apiextensions.CustomResourceDefinition) sets.Set[string] {
	versionsWithUnchangedSchemas := sets.New[string]()
	for _, version := range obj.Spec.Versions {
		newSchema, err := apiextensions.GetSchemaForVersion(obj, version.Name)
		if err != nil {
			continue
		}
		oldSchema, err := apiextensions.GetSchemaForVersion(oldObject, version.Name)
		if err != nil {
			continue
		}
		if apiequality.Semantic.DeepEqual(newSchema, oldSchema) {
			versionsWithUnchangedSchemas.Insert(version.Name)
		}
	}
	return versionsWithUnchangedSchemas
}

// suppressExpressionCostForUnchangedSchema returns a copy of opts with suppressPerExpressionCost set to true if
// the specified version's schema is unchanged.
func suppressExpressionCostForUnchangedSchema(opts validationOptions, version string) validationOptions {
	if opts.versionsWithUnchangedSchemas.Has(version) {
		opts.suppressPerExpressionCost = true
	}
	return opts
}

// ValidateCustomResourceDefinitionUpdate statically validates
// context is passed for supporting context cancellation during cel validation when validating defaults
func ValidateCustomResourceDefinitionUpdate(ctx context.Context, obj, oldObj *apiextensions.CustomResourceDefinition) field.ErrorList {
	opts := validationOptions{
		allowDefaults:                            true,
		requireRecognizedConversionReviewVersion: oldObj.Spec.Conversion == nil || hasValidConversionReviewVersionOrEmpty(oldObj.Spec.Conversion.ConversionReviewVersions),
		requireImmutableNames:                    apiextensions.IsCRDConditionTrue(oldObj, apiextensions.Established),
		requireOpenAPISchema:                     requireOpenAPISchema(&oldObj.Spec),
		requireValidPropertyType:                 requireValidPropertyType(&oldObj.Spec),
		requireStructuralSchema:                  requireStructuralSchema(&oldObj.Spec),
		requirePrunedDefaults:                    requirePrunedDefaults(&oldObj.Spec),
		requireAtomicSetType:                     requireAtomicSetType(&oldObj.Spec),
		requireMapListKeysMapSetValidation:       requireMapListKeysMapSetValidation(&oldObj.Spec),
		preexistingExpressions:                   findPreexistingExpressions(&oldObj.Spec),
		versionsWithUnchangedSchemas:             findVersionsWithUnchangedSchemas(obj, oldObj),
		// strictCost is always true to enforce cost limits.
		celEnvironmentSet:    environment.MustBaseEnvSet(environment.DefaultCompatibilityVersion(), true),
		allowInvalidCABundle: allowInvalidCABundle(oldObj),
	}
	return validateCustomResourceDefinitionUpdate(ctx, obj, oldObj, opts)
}

func validateCustomResourceDefinitionUpdate(ctx context.Context, obj, oldObj *apiextensions.CustomResourceDefinition, opts validationOptions) field.ErrorList {
	allErrs := genericvalidation.ValidateObjectMetaUpdate(&obj.ObjectMeta, &oldObj.ObjectMeta, field.NewPath("metadata"))
	allErrs = append(allErrs, validateCustomResourceDefinitionSpecUpdate(ctx, &obj.Spec, &oldObj.Spec, opts, field.NewPath("spec"))...)
	allErrs = append(allErrs, ValidateCustomResourceDefinitionStatus(&obj.Status, field.NewPath("status"))...)
	allErrs = append(allErrs, ValidateCustomResourceDefinitionStoredVersions(obj.Status.StoredVersions, obj.Spec.Versions, field.NewPath("status").Child("storedVersions"))...)
	allErrs = append(allErrs, validateAPIApproval(obj, oldObj)...)
	allErrs = append(allErrs, validatePreserveUnknownFields(obj, oldObj)...)
	return allErrs
}

// ValidateCustomResourceDefinitionStoredVersions statically validates
func ValidateCustomResourceDefinitionStoredVersions(storedVersions []string, versions []apiextensions.CustomResourceDefinitionVersion, fldPath *field.Path) field.ErrorList {
	if len(storedVersions) == 0 {
		return field.ErrorList{field.Invalid(fldPath, storedVersions, "must have at least one stored version")}
	}
	allErrs := field.ErrorList{}
	storedVersionsMap := map[string]int{}
	for i, v := range storedVersions {
		storedVersionsMap[v] = i
	}
	for _, v := range versions {
		_, ok := storedVersionsMap[v.Name]
		if v.Storage && !ok {
			allErrs = append(allErrs, field.Invalid(fldPath, storedVersions, "must have the storage version "+v.Name))
		}
		if ok {
			delete(storedVersionsMap, v.Name)
		}
	}

	for v, i := range storedVersionsMap {
		allErrs = append(allErrs, field.Invalid(fldPath.Index(i), v, fmt.Sprintf("missing from spec.versions; %[1]s was previously a storage version, and must remain in spec.versions until a storage migration ensures no data remains persisted in %[1]s and removes %[1]s from status.storedVersions", v)))
	}

	return allErrs
}

// ValidateUpdateCustomResourceDefinitionStatus statically validates
func ValidateUpdateCustomResourceDefinitionStatus(obj, oldObj *apiextensions.CustomResourceDefinition) field.ErrorList {
	allErrs := genericvalidation.ValidateObjectMetaUpdate(&obj.ObjectMeta, &oldObj.ObjectMeta, field.NewPath("metadata"))
	allErrs = append(allErrs, ValidateCustomResourceDefinitionStatus(&obj.Status, field.NewPath("status"))...)
	return allErrs
}

// validateCustomResourceDefinitionVersion statically validates.
// context is passed for supporting context cancellation during cel validation when validating defaults
func validateCustomResourceDefinitionVersion(ctx context.Context, version *apiextensions.CustomResourceDefinitionVersion, fldPath *field.Path, statusEnabled bool, opts validationOptions) field.ErrorList {
	allErrs := field.ErrorList{}
	for _, err := range validateDeprecationWarning(version.Deprecated, version.DeprecationWarning) {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("deprecationWarning"), version.DeprecationWarning, err))
	}
	opts = suppressExpressionCostForUnchangedSchema(opts, version.Name)
	allErrs = append(allErrs, validateCustomResourceDefinitionValidation(ctx, version.Schema, statusEnabled, opts, fldPath.Child("schema"))...)
	allErrs = append(allErrs, ValidateCustomResourceDefinitionSubresources(version.Subresources, fldPath.Child("subresources"))...)
	for i := range version.AdditionalPrinterColumns {
		allErrs = append(allErrs, ValidateCustomResourceColumnDefinition(&version.AdditionalPrinterColumns[i], fldPath.Child("additionalPrinterColumns").Index(i))...)
	}

	if len(version.SelectableFields) > 0 {
		if version.Schema == nil || version.Schema.OpenAPIV3Schema == nil {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("selectableFields"), "", "selectableFields may only be set when version.schema.openAPIV3Schema is not included"))
		} else {
			schema, err := structuralschema.NewStructural(version.Schema.OpenAPIV3Schema)
			if err != nil {
				allErrs = append(allErrs, field.Invalid(fldPath.Child("schema.openAPIV3Schema"), "", err.Error()))
			}
			allErrs = append(allErrs, ValidateCustomResourceSelectableFields(version.SelectableFields, schema, fldPath.Child("selectableFields"))...)
		}
	}
	return allErrs
}

func validateDeprecationWarning(deprecated bool, deprecationWarning *string) []string {
	if !deprecated && deprecationWarning != nil {
		return []string{"can only be set for deprecated versions"}
	}
	if deprecationWarning == nil {
		return nil
	}
	var errors []string
	if len(*deprecationWarning) > 256 {
		errors = append(errors, "must be <= 256 characters long")
	}
	if len(*deprecationWarning) == 0 {
		errors = append(errors, "must not be an empty string")
	}
	for i, r := range *deprecationWarning {
		if !unicode.IsPrint(r) {
			errors = append(errors, fmt.Sprintf("must only contain printable UTF-8 characters; non-printable character found at index %d", i))
			break
		}
		if unicode.IsControl(r) {
			errors = append(errors, fmt.Sprintf("must only contain printable UTF-8 characters; control character found at index %d", i))
			break
		}
	}
	if !utf8.ValidString(*deprecationWarning) {
		errors = append(errors, "must only contain printable UTF-8 characters")
	}
	return errors
}

// context is passed for supporting context cancellation during cel validation when validating defaults
func validateCustomResourceDefinitionSpec(ctx context.Context, spec *apiextensions.CustomResourceDefinitionSpec, opts validationOptions, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}

	if len(spec.Group) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("group"), ""))
	} else if errs := utilvalidation.IsDNS1123Subdomain(spec.Group); len(errs) > 0 {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("group"), spec.Group, strings.Join(errs, ",")))
	} else if len(strings.Split(spec.Group, ".")) < 2 {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("group"), spec.Group, "should be a domain with at least one dot"))
	}

	allErrs = append(allErrs, validateEnumStrings(fldPath.Child("scope"), string(spec.Scope), []string{string(apiextensions.ClusterScoped), string(apiextensions.NamespaceScoped)}, true)...)

	// enabling pruning requires structural schemas
	if spec.PreserveUnknownFields == nil || *spec.PreserveUnknownFields == false {
		opts.requireStructuralSchema = true
	}

	if opts.requireOpenAPISchema {
		// check that either a global schema or versioned schemas are set in all versions
		if spec.Validation == nil || spec.Validation.OpenAPIV3Schema == nil {
			for i, v := range spec.Versions {
				if v.Schema == nil || v.Schema.OpenAPIV3Schema == nil {
					allErrs = append(allErrs, field.Required(fldPath.Child("versions").Index(i).Child("schema").Child("openAPIV3Schema"), "schemas are required"))
				}
			}
		}
	} else if spec.PreserveUnknownFields == nil || *spec.PreserveUnknownFields == false {
		// check that either a global schema or versioned schemas are set in served versions
		if spec.Validation == nil || spec.Validation.OpenAPIV3Schema == nil {
			for i, v := range spec.Versions {
				schemaPath := fldPath.Child("versions").Index(i).Child("schema", "openAPIV3Schema")
				if v.Served && (v.Schema == nil || v.Schema.OpenAPIV3Schema == nil) {
					allErrs = append(allErrs, field.Required(schemaPath, "because otherwise all fields are pruned"))
				}
			}
		}
	}
	if opts.allowDefaults && specHasDefaults(spec) {
		opts.requireStructuralSchema = true
		if spec.PreserveUnknownFields == nil || *spec.PreserveUnknownFields {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("preserveUnknownFields"), true, "must be false in order to use defaults in the schema"))
		}
	}
	if specHasKubernetesExtensions(spec) {
		opts.requireStructuralSchema = true
	}

	storageFlagCount := 0
	versionsMap := map[string]bool{}
	uniqueNames := true
	for i, version := range spec.Versions {
		if version.Storage {
			storageFlagCount++
		}
		if versionsMap[version.Name] {
			uniqueNames = false
		} else {
			versionsMap[version.Name] = true
		}
		if errs := utilvalidation.IsDNS1035Label(version.Name); len(errs) > 0 {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("versions").Index(i).Child("name"), spec.Versions[i].Name, strings.Join(errs, ",")))
		}
		subresources := getSubresourcesForVersion(spec, version.Name)
		allErrs = append(allErrs, validateCustomResourceDefinitionVersion(ctx, &version, fldPath.Child("versions").Index(i), hasStatusEnabled(subresources), opts)...)
	}

	// The top-level and per-version fields are mutual exclusive
	if spec.Validation != nil && hasPerVersionSchema(spec.Versions) {
		allErrs = append(allErrs, field.Forbidden(fldPath.Child("validation"), "top-level and per-version schemas are mutually exclusive"))
	}
	if spec.Subresources != nil && hasPerVersionSubresources(spec.Versions) {
		allErrs = append(allErrs, field.Forbidden(fldPath.Child("subresources"), "top-level and per-version subresources are mutually exclusive"))
	}
	if len(spec.AdditionalPrinterColumns) > 0 && hasPerVersionColumns(spec.Versions) {
		allErrs = append(allErrs, field.Forbidden(fldPath.Child("additionalPrinterColumns"), "top-level and per-version additionalPrinterColumns are mutually exclusive"))
	}

	// Per-version fields may not all be set to identical values (top-level field should be used instead)
	if hasIdenticalPerVersionSchema(spec.Versions) {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("versions"), spec.Versions, "per-version schemas may not all be set to identical values (top-level validation should be used instead)"))
	}
	if hasIdenticalPerVersionSubresources(spec.Versions) {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("versions"), spec.Versions, "per-version subresources may not all be set to identical values (top-level subresources should be used instead)"))
	}
	if hasIdenticalPerVersionColumns(spec.Versions) {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("versions"), spec.Versions, "per-version additionalPrinterColumns may not all be set to identical values (top-level additionalPrinterColumns should be used instead)"))
	}

	if !uniqueNames {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("versions"), spec.Versions, "must contain unique version names"))
	}
	if storageFlagCount != 1 {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("versions"), spec.Versions, "must have exactly one version marked as storage version"))
	}
	if len(spec.Version) != 0 {
		if errs := utilvalidation.IsDNS1035Label(spec.Version); len(errs) > 0 {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("version"), spec.Version, strings.Join(errs, ",")))
		}
		if len(spec.Versions) >= 1 && spec.Versions[0].Name != spec.Version {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("version"), spec.Version, "must match the first version in spec.versions"))
		}
	}

	// in addition to the basic name restrictions, some names are required for spec, but not for status
	if len(spec.Names.Plural) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("names", "plural"), ""))
	}
	if len(spec.Names.Singular) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("names", "singular"), ""))
	}
	if len(spec.Names.Kind) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("names", "kind"), ""))
	}
	if len(spec.Names.ListKind) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("names", "listKind"), ""))
	}

	allErrs = append(allErrs, ValidateCustomResourceDefinitionNames(&spec.Names, fldPath.Child("names"))...)
	allErrs = append(allErrs, validateCustomResourceDefinitionValidation(ctx, spec.Validation, hasAnyStatusEnabled(spec), suppressExpressionCostForUnchangedSchema(opts, spec.Version), fldPath.Child("validation"))...)
	allErrs = append(allErrs, ValidateCustomResourceDefinitionSubresources(spec.Subresources, fldPath.Child("subresources"))...)

	for i := range spec.AdditionalPrinterColumns {
		if errs := ValidateCustomResourceColumnDefinition(&spec.AdditionalPrinterColumns[i], fldPath.Child("additionalPrinterColumns").Index(i)); len(errs) > 0 {
			allErrs = append(allErrs, errs...)
		}
	}

	if len(spec.SelectableFields) > 0 {
		if spec.Validation == nil {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("selectableFields"), "", "selectableFields may only be set when validations.schema is included"))
		} else {
			schema, err := structuralschema.NewStructural(spec.Validation.OpenAPIV3Schema)
			if err != nil {
				allErrs = append(allErrs, field.Invalid(fldPath.Child("schema.openAPIV3Schema"), "", err.Error()))
			}

			allErrs = append(allErrs, ValidateCustomResourceSelectableFields(spec.SelectableFields, schema, fldPath.Child("selectableFields"))...)
		}
	}

	if (spec.Conversion != nil && spec.Conversion.Strategy != apiextensions.NoneConverter) && (spec.PreserveUnknownFields == nil || *spec.PreserveUnknownFields) {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("conversion").Child("strategy"), spec.Conversion.Strategy, "must be None if spec.preserveUnknownFields is true"))
	}
	allErrs = append(allErrs, validateCustomResourceConversion(spec.Conversion, opts.requireRecognizedConversionReviewVersion, fldPath.Child("conversion"), opts)...)

	return allErrs
}

func validateEnumStrings(fldPath *field.Path, value string, accepted []string, required bool) field.ErrorList {
	if value == "" {
		if required {
			return field.ErrorList{field.Required(fldPath, "")}
		}
		return field.ErrorList{}
	}
	for _, a := range accepted {
		if a == value {
			return field.ErrorList{}
		}
	}
	return field.ErrorList{field.NotSupported(fldPath, value, accepted)}
}

// AcceptedConversionReviewVersions contains the list of ConversionReview versions the *prior* version of the API server understands.
// 1.15: server understands v1beta1; accepted versions are ["v1beta1"]
// 1.16: server understands v1, v1beta1; accepted versions are ["v1beta1"]
// 1.17+: server understands v1, v1beta1; accepted versions are ["v1","v1beta1"]
var acceptedConversionReviewVersions = sets.NewString(apiextensionsv1.SchemeGroupVersion.Version, apiextensionsv1beta1.SchemeGroupVersion.Version)

func isAcceptedConversionReviewVersion(v string) bool {
	return acceptedConversionReviewVersions.Has(v)
}

func validateConversionReviewVersions(versions []string, requireRecognizedVersion bool, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if len(versions) < 1 {
		allErrs = append(allErrs, field.Required(fldPath, ""))
	} else {
		seen := map[string]bool{}
		hasAcceptedVersion := false
		for i, v := range versions {
			if seen[v] {
				allErrs = append(allErrs, field.Invalid(fldPath.Index(i), v, "duplicate version"))
				continue
			}
			seen[v] = true
			for _, errString := range utilvalidation.IsDNS1035Label(v) {
				allErrs = append(allErrs, field.Invalid(fldPath.Index(i), v, errString))
			}
			if isAcceptedConversionReviewVersion(v) {
				hasAcceptedVersion = true
			}
		}
		if requireRecognizedVersion && !hasAcceptedVersion {
			allErrs = append(allErrs, field.Invalid(
				fldPath, versions,
				fmt.Sprintf("must include at least one of %v",
					strings.Join(acceptedConversionReviewVersions.List(), ", "))))
		}
	}
	return allErrs
}

// Allows invalid CA Bundle to be specified only if the existing CABundle is invalid
// or if the CRD is not established yet.
func allowInvalidCABundle(oldCRD *apiextensions.CustomResourceDefinition) bool {
	if !apiextensions.IsCRDConditionTrue(oldCRD, apiextensions.Established) {
		return true
	}
	oldConversion := oldCRD.Spec.Conversion
	if oldConversion == nil || oldConversion.WebhookClientConfig == nil ||
		len(oldConversion.WebhookClientConfig.CABundle) == 0 {
		return false
	}
	return len(webhook.ValidateCABundle(field.NewPath("caBundle"), oldConversion.WebhookClientConfig.CABundle)) > 0
}

// hasValidConversionReviewVersion return true if there is a valid version or if the list is empty.
func hasValidConversionReviewVersionOrEmpty(versions []string) bool {
	if len(versions) < 1 {
		return true
	}
	for _, v := range versions {
		if isAcceptedConversionReviewVersion(v) {
			return true
		}
	}
	return false
}

func validateCustomResourceConversion(conversion *apiextensions.CustomResourceConversion, requireRecognizedVersion bool, fldPath *field.Path, opts validationOptions) field.ErrorList {
	allErrs := field.ErrorList{}
	if conversion == nil {
		return allErrs
	}
	allErrs = append(allErrs, validateEnumStrings(fldPath.Child("strategy"), string(conversion.Strategy), []string{string(apiextensions.NoneConverter), string(apiextensions.WebhookConverter)}, true)...)
	if conversion.Strategy == apiextensions.WebhookConverter {
		if conversion.WebhookClientConfig == nil {
			allErrs = append(allErrs, field.Required(fldPath.Child("webhookClientConfig"), "required when strategy is set to Webhook"))
		} else {
			cc := conversion.WebhookClientConfig
			switch {
			case (cc.URL == nil) == (cc.Service == nil):
				allErrs = append(allErrs, field.Required(fldPath.Child("webhookClientConfig"), "exactly one of url or service is required"))
			case cc.URL != nil:
				allErrs = append(allErrs, webhook.ValidateWebhookURL(fldPath.Child("webhookClientConfig").Child("url"), *cc.URL, true)...)
			case cc.Service != nil:
				allErrs = append(allErrs, webhook.ValidateWebhookService(fldPath.Child("webhookClientConfig").Child("service"), cc.Service.Name, cc.Service.Namespace, cc.Service.Path, cc.Service.Port)...)
			}
			if len(cc.CABundle) > 0 && !opts.allowInvalidCABundle {
				allErrs = append(allErrs, webhook.ValidateCABundle(fldPath.Child("webhookClientConfig").Child("caBundle"), cc.CABundle)...)
			}
		}
		allErrs = append(allErrs, validateConversionReviewVersions(conversion.ConversionReviewVersions, requireRecognizedVersion, fldPath.Child("conversionReviewVersions"))...)
	} else {
		if conversion.WebhookClientConfig != nil {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("webhookClientConfig"), "should not be set when strategy is not set to Webhook"))
		}
		if len(conversion.ConversionReviewVersions) > 0 {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("conversionReviewVersions"), "should not be set when strategy is not set to Webhook"))
		}
	}
	return allErrs
}

// validateCustomResourceDefinitionSpecUpdate statically validates
// context is passed for supporting context cancellation during cel validation when validating defaults
func validateCustomResourceDefinitionSpecUpdate(ctx context.Context, spec, oldSpec *apiextensions.CustomResourceDefinitionSpec, opts validationOptions, fldPath *field.Path) field.ErrorList {
	allErrs := validateCustomResourceDefinitionSpec(ctx, spec, opts, fldPath)

	if opts.requireImmutableNames {
		// these effect the storage and cannot be changed therefore
		allErrs = append(allErrs, genericvalidation.ValidateImmutableField(spec.Scope, oldSpec.Scope, fldPath.Child("scope"))...)
		allErrs = append(allErrs, genericvalidation.ValidateImmutableField(spec.Names.Kind, oldSpec.Names.Kind, fldPath.Child("names", "kind"))...)
	}

	// these affects the resource name, which is always immutable, so this can't be updated.
	allErrs = append(allErrs, genericvalidation.ValidateImmutableField(spec.Group, oldSpec.Group, fldPath.Child("group"))...)
	allErrs = append(allErrs, genericvalidation.ValidateImmutableField(spec.Names.Plural, oldSpec.Names.Plural, fldPath.Child("names", "plural"))...)

	return allErrs
}

// getSubresourcesForVersion returns the subresources for given version in given CRD spec.
// NOTE That this function assumes version always exist since it's used by the validation process
// that iterates through the existing versions.
func getSubresourcesForVersion(crd *apiextensions.CustomResourceDefinitionSpec, version string) *apiextensions.CustomResourceSubresources {
	if !hasPerVersionSubresources(crd.Versions) {
		return crd.Subresources
	}
	for _, v := range crd.Versions {
		if version == v.Name {
			return v.Subresources
		}
	}
	return nil
}

// hasAnyStatusEnabled returns true if given CRD spec has at least one Status Subresource set
// among the top-level and per-version Subresources.
func hasAnyStatusEnabled(crd *apiextensions.CustomResourceDefinitionSpec) bool {
	if hasStatusEnabled(crd.Subresources) {
		return true
	}
	for _, v := range crd.Versions {
		if hasStatusEnabled(v.Subresources) {
			return true
		}
	}
	return false
}

// hasStatusEnabled returns true if given CRD Subresources has non-nil Status set.
func hasStatusEnabled(subresources *apiextensions.CustomResourceSubresources) bool {
	if subresources != nil && subresources.Status != nil {
		return true
	}
	return false
}

// hasPerVersionSchema returns true if a CRD uses per-version schema.
func hasPerVersionSchema(versions []apiextensions.CustomResourceDefinitionVersion) bool {
	for _, v := range versions {
		if v.Schema != nil {
			return true
		}
	}
	return false
}

// hasPerVersionSubresources returns true if a CRD uses per-version subresources.
func hasPerVersionSubresources(versions []apiextensions.CustomResourceDefinitionVersion) bool {
	for _, v := range versions {
		if v.Subresources != nil {
			return true
		}
	}
	return false
}

// hasPerVersionColumns returns true if a CRD uses per-version columns.
func hasPerVersionColumns(versions []apiextensions.CustomResourceDefinitionVersion) bool {
	for _, v := range versions {
		if len(v.AdditionalPrinterColumns) > 0 {
			return true
		}
	}
	return false
}

// hasIdenticalPerVersionSchema returns true if a CRD sets identical non-nil values
// to all per-version schemas
func hasIdenticalPerVersionSchema(versions []apiextensions.CustomResourceDefinitionVersion) bool {
	if len(versions) == 0 {
		return false
	}
	value := versions[0].Schema
	for _, v := range versions {
		if v.Schema == nil || !apiequality.Semantic.DeepEqual(v.Schema, value) {
			return false
		}
	}
	return true
}

// hasIdenticalPerVersionSubresources returns true if a CRD sets identical non-nil values
// to all per-version subresources
func hasIdenticalPerVersionSubresources(versions []apiextensions.CustomResourceDefinitionVersion) bool {
	if len(versions) == 0 {
		return false
	}
	value := versions[0].Subresources
	for _, v := range versions {
		if v.Subresources == nil || !apiequality.Semantic.DeepEqual(v.Subresources, value) {
			return false
		}
	}
	return true
}

// hasIdenticalPerVersionColumns returns true if a CRD sets identical non-nil values
// to all per-version columns
func hasIdenticalPerVersionColumns(versions []apiextensions.CustomResourceDefinitionVersion) bool {
	if len(versions) == 0 {
		return false
	}
	value := versions[0].AdditionalPrinterColumns
	for _, v := range versions {
		if len(v.AdditionalPrinterColumns) == 0 || !apiequality.Semantic.DeepEqual(v.AdditionalPrinterColumns, value) {
			return false
		}
	}
	return true
}

// ValidateCustomResourceDefinitionStatus statically validates
func ValidateCustomResourceDefinitionStatus(status *apiextensions.CustomResourceDefinitionStatus, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	allErrs = append(allErrs, ValidateCustomResourceDefinitionNames(&status.AcceptedNames, fldPath.Child("acceptedNames"))...)
	return allErrs
}

// ValidateCustomResourceDefinitionNames statically validates
func ValidateCustomResourceDefinitionNames(names *apiextensions.CustomResourceDefinitionNames, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if errs := utilvalidation.IsDNS1035Label(names.Plural); len(names.Plural) > 0 && len(errs) > 0 {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("plural"), names.Plural, strings.Join(errs, ",")))
	}
	if errs := utilvalidation.IsDNS1035Label(names.Singular); len(names.Singular) > 0 && len(errs) > 0 {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("singular"), names.Singular, strings.Join(errs, ",")))
	}
	if errs := utilvalidation.IsDNS1035Label(strings.ToLower(names.Kind)); len(names.Kind) > 0 && len(errs) > 0 {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("kind"), names.Kind, "may have mixed case, but should otherwise match: "+strings.Join(errs, ",")))
	}
	if errs := utilvalidation.IsDNS1035Label(strings.ToLower(names.ListKind)); len(names.ListKind) > 0 && len(errs) > 0 {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("listKind"), names.ListKind, "may have mixed case, but should otherwise match: "+strings.Join(errs, ",")))
	}

	for i, shortName := range names.ShortNames {
		if errs := utilvalidation.IsDNS1035Label(shortName); len(errs) > 0 {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("shortNames").Index(i), shortName, strings.Join(errs, ",")))
		}
	}

	// kind and listKind may not be the same or parsing become ambiguous
	if len(names.Kind) > 0 && names.Kind == names.ListKind {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("listKind"), names.ListKind, "kind and listKind may not be the same"))
	}

	for i, category := range names.Categories {
		if errs := utilvalidation.IsDNS1035Label(category); len(errs) > 0 {
			allErrs = append(allErrs, field.Invalid(fldPath.Child("categories").Index(i), category, strings.Join(errs, ",")))
		}
	}

	return allErrs
}

// ValidateCustomResourceColumnDefinition statically validates a printer column.
func ValidateCustomResourceColumnDefinition(col *apiextensions.CustomResourceColumnDefinition, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}

	if len(col.Name) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("name"), ""))
	}

	if len(col.Type) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("type"), fmt.Sprintf("must be one of %s", strings.Join(printerColumnDatatypes.List(), ","))))
	} else if !printerColumnDatatypes.Has(col.Type) {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("type"), col.Type, fmt.Sprintf("must be one of %s", strings.Join(printerColumnDatatypes.List(), ","))))
	}

	if len(col.Format) > 0 && !customResourceColumnDefinitionFormats.Has(col.Format) {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("format"), col.Format, fmt.Sprintf("must be one of %s", strings.Join(customResourceColumnDefinitionFormats.List(), ","))))
	}

	if len(col.JSONPath) == 0 {
		allErrs = append(allErrs, field.Required(fldPath.Child("JSONPath"), ""))
	} else if errs := validateSimpleJSONPath(col.JSONPath, fldPath.Child("JSONPath")); len(errs) > 0 {
		allErrs = append(allErrs, errs...)
	}

	return allErrs
}

func ValidateCustomResourceSelectableFields(selectableFields []apiextensions.SelectableField, schema *structuralschema.Structural, fldPath *field.Path) (allErrs field.ErrorList) {
	uniqueSelectableFields := sets.New[string]()
	for i, selectableField := range selectableFields {
		indexFldPath := fldPath.Index(i)
		if len(selectableField.JSONPath) == 0 {
			allErrs = append(allErrs, field.Required(indexFldPath.Child("jsonPath"), ""))
			continue
		}
		// Leverage the field path validation originally built for use with CEL features
		path, foundSchema, err := cel.ValidFieldPath(selectableField.JSONPath, schema, cel.WithFieldPathAllowArrayNotation(false))
		if err != nil {
			allErrs = append(allErrs, field.Invalid(indexFldPath.Child("jsonPath"), selectableField.JSONPath, fmt.Sprintf("is an invalid path: %v", err)))
			continue
		}
		if path.Root().String() == "metadata" {
			allErrs = append(allErrs, field.Invalid(indexFldPath.Child("jsonPath"), selectableField.JSONPath, "must not point to fields in metadata"))
		}
		if !allowedSelectableFieldSchema(foundSchema) {
			allErrs = append(allErrs, field.Invalid(indexFldPath.Child("jsonPath"), selectableField.JSONPath, "must point to a field of type string, boolean or integer. Enum string fields and strings with formats are allowed."))
		}
		if uniqueSelectableFields.Has(path.String()) {
			allErrs = append(allErrs, field.Duplicate(indexFldPath.Child("jsonPath"), selectableField.JSONPath))
		} else {
			uniqueSelectableFields.Insert(path.String())
		}
	}
	uniqueSelectableFieldCount := uniqueSelectableFields.Len()
	if uniqueSelectableFieldCount > MaxSelectableFields {
		allErrs = append(allErrs, field.TooMany(fldPath, uniqueSelectableFieldCount, MaxSelectableFields))
	}
	return allErrs
}

func allowedSelectableFieldSchema(schema *structuralschema.Structural) bool {
	if schema == nil {
		return false
	}
	switch schema.Type {
	case "string", "boolean", "integer":
		return true
	default:
		return false
	}
}

// specStandardValidator applies validations for different OpenAPI specification versions.
type specStandardValidator interface {
	validate(spec *apiextensions.JSONSchemaProps, fldPath *field.Path) field.ErrorList
	withForbiddenDefaults(reason string) specStandardValidator

	// insideResourceMeta returns true when validating either TypeMeta or ObjectMeta, from an embedded resource or on the top-level.
	insideResourceMeta() bool
	withInsideResourceMeta() specStandardValidator

	// forbidOldSelfValidations returns the path to the first ancestor of the visited path that can't be safely correlated between two revisions of an object, or nil if there is no such path
	forbidOldSelfValidations() *field.Path
	withForbidOldSelfValidations(path *field.Path) specStandardValidator
}

// validateCustomResourceDefinitionValidation statically validates
// context is passed for supporting context cancellation during cel validation when validating defaults
func validateCustomResourceDefinitionValidation(ctx context.Context, customResourceValidation *apiextensions.CustomResourceValidation, statusSubresourceEnabled bool, opts validationOptions, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}

	if customResourceValidation == nil {
		return allErrs
	}

	if schema := customResourceValidation.OpenAPIV3Schema; schema != nil {
		// if the status subresource is enabled, only certain fields are allowed inside the root schema.
		// these fields are chosen such that, if status is extracted as properties["status"], it's validation is not lost.
		if statusSubresourceEnabled {
			v := reflect.ValueOf(schema).Elem()
			for i := 0; i < v.NumField(); i++ {
				// skip zero values
				if value := v.Field(i).Interface(); reflect.DeepEqual(value, reflect.Zero(reflect.TypeOf(value)).Interface()) {
					continue
				}

				fieldName := v.Type().Field(i).Name

				// only "object" type is valid at root of the schema since validation schema for status is extracted as properties["status"]
				if fieldName == "Type" {
					if schema.Type != "object" {
						allErrs = append(allErrs, field.Invalid(fldPath.Child("openAPIV3Schema.type"), schema.Type, fmt.Sprintf(`only "object" is allowed as the type at the root of the schema if the status subresource is enabled`)))
						break
					}
					continue
				}

				if !allowedAtRootSchema(fieldName) {
					allErrs = append(allErrs, field.Invalid(fldPath.Child("openAPIV3Schema"), *schema, fmt.Sprintf(`only %v fields are allowed at the root of the schema if the status subresource is enabled`, allowedFieldsAtRootSchema)))
					break
				}
			}
		}

		if schema.Nullable {
			allErrs = append(allErrs, field.Forbidden(fldPath.Child("openAPIV3Schema.nullable"), fmt.Sprintf(`nullable cannot be true at the root`)))
		}

		openAPIV3Schema := &specStandardValidatorV3{
			allowDefaults:            opts.allowDefaults,
			disallowDefaultsReason:   opts.disallowDefaultsReason,
			requireValidPropertyType: opts.requireValidPropertyType,
		}

		var celContext *CELSchemaContext
		var structuralSchemaInitErrs field.ErrorList
		if opts.requireStructuralSchema {
			if ss, err := structuralschema.NewStructural(schema); err != nil {
				// These validation errors overlap with  OpenAPISchema validation errors so we keep track of them
				// separately and only show them if OpenAPISchema validation does not report any errors.
				structuralSchemaInitErrs = append(structuralSchemaInitErrs, field.Invalid(fldPath.Child("openAPIV3Schema"), "", err.Error()))
			} else if validationErrors := structuralschema.ValidateStructural(fldPath.Child("openAPIV3Schema"), ss); len(validationErrors) > 0 {
				allErrs = append(allErrs, validationErrors...)
			} else if validationErrors, err := structuraldefaulting.ValidateDefaults(ctx, fldPath.Child("openAPIV3Schema"), ss, true, opts.requirePrunedDefaults); err != nil {
				// this should never happen
				allErrs = append(allErrs, field.Invalid(fldPath.Child("openAPIV3Schema"), "", err.Error()))
			} else if len(validationErrors) > 0 {
				allErrs = append(allErrs, validationErrors...)
			} else {
				// Only initialize CEL rule validation context if the structural schemas are valid.
				// A nil CELSchemaContext indicates that no CEL validation should be attempted.
				celContext = RootCELContext(schema)
			}
		}
		allErrs = append(allErrs, ValidateCustomResourceDefinitionOpenAPISchema(schema, fldPath.Child("openAPIV3Schema"), openAPIV3Schema, true, &opts, celContext).AllErrors()...)

		if len(allErrs) == 0 && len(structuralSchemaInitErrs) > 0 {
			// Structural schema initialization errors overlap with OpenAPISchema validation errors so we only show them
			// if there are no OpenAPISchema validation errors.
			allErrs = append(allErrs, structuralSchemaInitErrs...)
		}

		if celContext != nil && celContext.TotalCost != nil {
			if celContext.TotalCost.Total > StaticEstimatedCRDCostLimit {
				for _, expensive := range celContext.TotalCost.MostExpensive {
					costErrorMsg := fmt.Sprintf("contributed to estimated rule cost total exceeding cost limit for entire OpenAPIv3 schema")
					allErrs = append(allErrs, field.Forbidden(expensive.Path, costErrorMsg))
				}

				costErrorMsg := getCostErrorMessage("x-kubernetes-validations estimated rule cost total for entire OpenAPIv3 schema", celContext.TotalCost.Total, StaticEstimatedCRDCostLimit)
				allErrs = append(allErrs, field.Forbidden(fldPath.Child("openAPIV3Schema"), costErrorMsg))
			}
		}
	}

	// if validation passed otherwise, make sure we can actually construct a schema validator from this custom resource validation.
	if len(allErrs) == 0 {
		if _, _, err := apiservervalidation.NewSchemaValidator(customResourceValidation.OpenAPIV3Schema); err != nil {
			allErrs = append(allErrs, field.Invalid(fldPath, "", fmt.Sprintf("error building validator: %v", err)))
		}
	}
	return allErrs
}

var metaFields = sets.NewString("metadata", "kind", "apiVersion")

// OpenAPISchemaErrorList tracks all validation errors reported ValidateCustomResourceDefinitionOpenAPISchema
// with CEL related errors kept separate from schema related errors.
type OpenAPISchemaErrorList struct {
	SchemaErrors field.ErrorList
	CELErrors    field.ErrorList
}

// AppendErrors appends all errors in the provided list with the errors of this list.
func (o *OpenAPISchemaErrorList) AppendErrors(list *OpenAPISchemaErrorList) {
	if o == nil || list == nil {
		return
	}
	o.SchemaErrors = append(o.SchemaErrors, list.SchemaErrors...)
	o.CELErrors = append(o.CELErrors, list.CELErrors...)
}

// AllErrors returns a list containing both schema and CEL errors.
func (o *OpenAPISchemaErrorList) AllErrors() field.ErrorList {
	if o == nil {
		return field.ErrorList{}
	}
	return append(o.SchemaErrors, o.CELErrors...)
}

// ValidateCustomResourceDefinitionOpenAPISchema statically validates
func ValidateCustomResourceDefinitionOpenAPISchema(schema *apiextensions.JSONSchemaProps, fldPath *field.Path, ssv specStandardValidator, isRoot bool, opts *validationOptions, celContext *CELSchemaContext) *OpenAPISchemaErrorList {
	allErrs := &OpenAPISchemaErrorList{SchemaErrors: field.ErrorList{}, CELErrors: field.ErrorList{}}

	if schema == nil {
		return allErrs
	}
	allErrs.SchemaErrors = append(allErrs.SchemaErrors, ssv.validate(schema, fldPath)...)

	if schema.UniqueItems {
		allErrs.SchemaErrors = append(allErrs.SchemaErrors, field.Forbidden(fldPath.Child("uniqueItems"), "uniqueItems cannot be set to true since the runtime complexity becomes quadratic"))
	}

	// additionalProperties and properties are mutual exclusive because otherwise they
	// contradict Kubernetes' API convention to ignore unknown fields.
	//
	// In other words:
	// - properties are for structs,
	// - additionalProperties are for map[string]interface{}
	//
	// Note: when patternProperties is added to OpenAPI some day, this will have to be
	//       restricted like additionalProperties.
	if schema.AdditionalProperties != nil {
		if len(schema.Properties) != 0 {
			if !schema.AdditionalProperties.Allows || schema.AdditionalProperties.Schema != nil {
				allErrs.SchemaErrors = append(allErrs.SchemaErrors, field.Forbidden(fldPath.Child("additionalProperties"), "additionalProperties and properties are mutual exclusive"))
			}
		}
		// Note: we forbid additionalProperties at resource root, both embedded and top-level.
		//       But further inside, additionalProperties is possible, e.g. for labels or annotations.
		subSsv := ssv
		if ssv.insideResourceMeta() {
			// we have to forbid defaults inside additionalProperties because pruning without actual value is ambiguous
			subSsv = ssv.withForbiddenDefaults("inside additionalProperties applying to object metadata")
		}
		allErrs.AppendErrors(ValidateCustomResourceDefinitionOpenAPISchema(schema.AdditionalProperties.Schema, fldPath.Child("additionalProperties"), subSsv, false, opts, celContext.ChildAdditionalPropertiesContext(schema.AdditionalProperties.Schema)))
	}

	if len(schema.Properties) != 0 {
		for property, jsonSchema := range schema.Properties {
			subSsv := ssv

			if !cel.MapIsCorrelatable(schema.XMapType) {
				subSsv = subSsv.withForbidOldSelfValidations(fldPath)
			}

			if (isRoot || schema.XEmbeddedResource) && metaFields.Has(property) {
				// we recurse into the schema that applies to ObjectMeta.
				subSsv = subSsv.withInsideResourceMeta()
				if isRoot {
					subSsv = subSsv.withForbiddenDefaults(fmt.Sprintf("in top-level %s", property))
				}
			}
			propertySchema := jsonSchema
			allErrs.AppendErrors(ValidateCustomResourceDefinitionOpenAPISchema(&propertySchema, fldPath.Child("properties").Key(property), subSsv, false, opts, celContext.ChildPropertyContext(&propertySchema, property)))
		}
	}

	allErrs.AppendErrors(ValidateCustomResourceDefinitionOpenAPISchema(schema.Not, fldPath.Child("not"), ssv, false, opts, nil))

	if len(schema.AllOf) != 0 {
		for i, jsonSchema := range schema.AllOf {
			allOfSchema := jsonSchema
			allErrs.AppendErrors(ValidateCustomResourceDefinitionOpenAPISchema(&allOfSchema, fldPath.Child("allOf").Index(i), ssv, false, opts, nil))
		}
	}

	if len(schema.OneOf) != 0 {
		for i, jsonSchema := range schema.OneOf {
			oneOfSchema := jsonSchema
			allErrs.AppendErrors(ValidateCustomResourceDefinitionOpenAPISchema(&oneOfSchema, fldPath.Child("oneOf").Index(i), ssv, false, opts, nil))
		}
	}

	if len(schema.AnyOf) != 0 {
		for i, jsonSchema := range schema.AnyOf {
			anyOfSchema := jsonSchema
			allErrs.AppendErrors(ValidateCustomResourceDefinitionOpenAPISchema(&anyOfSchema, fldPath.Child("anyOf").Index(i), ssv, false, opts, nil))
		}
	}

	if len(schema.Definitions) != 0 {
		for definition, jsonSchema := range schema.Definitions {
			definitionSchema := jsonSchema
			allErrs.AppendErrors(ValidateCustomResourceDefinitionOpenAPISchema(&definitionSchema, fldPath.Child("definitions").Key(definition), ssv, false, opts, nil))
		}
	}

	if schema.Items != nil {
		subSsv := ssv

		// we can only correlate old/new items for "map" and "set" lists, and correlation of
		// "set" elements by identity is not supported for cel (x-kubernetes-validations)
		// rules. an unset list type defaults to "atomic".
		if schema.XListType == nil || *schema.XListType != "map" {
			subSsv = subSsv.withForbidOldSelfValidations(fldPath)
		}

		allErrs.AppendErrors(ValidateCustomResourceDefinitionOpenAPISchema(schema.Items.Schema, fldPath.Child("items"), subSsv, false, opts, celContext.ChildItemsContext(schema.Items.Schema)))
		if len(schema.Items.JSONSchemas) != 0 {
			for i, jsonSchema := range schema.Items.JSONSchemas {
				itemsSchema := jsonSchema
				allErrs.AppendErrors(ValidateCustomResourceDefinitionOpenAPISchema(&itemsSchema, fldPath.Child("items").Index(i), subSsv, false, opts, celContext.ChildItemsContext(&itemsSchema)))
			}
		}
	}

	if schema.Dependencies != nil {
		for dependency, jsonSchemaPropsOrStringArray := range schema.Dependencies {
			allErrs.AppendErrors(ValidateCustomResourceDefinitionOpenAPISchema(jsonSchemaPropsOrStringArray.Schema, fldPath.Child("dependencies").Key(dependency), ssv, false, opts, nil))
		}
	}

	if schema.XPreserveUnknownFields != nil && !*schema.XPreserveUnknownFields {
		allErrs.SchemaErrors = append(allErrs.SchemaErrors, field.Invalid(fldPath.Child("x-kubernetes-preserve-unknown-fields"), *schema.XPreserveUnknownFields, "must be true or undefined"))
	}

	if schema.XMapType != nil && schema.Type != "object" {
		if len(schema.Type) == 0 {
			allErrs.SchemaErrors = append(allErrs.SchemaErrors, field.Required(fldPath.Child("type"), "must be object if x-kubernetes-map-type is specified"))
		} else {
			allErrs.SchemaErrors = append(allErrs.SchemaErrors, field.Invalid(fldPath.Child("type"), schema.Type, "must be object if x-kubernetes-map-type is specified"))
		}
	}

	if schema.XMapType != nil && *schema.XMapType != "atomic" && *schema.XMapType != "granular" {
		allErrs.SchemaErrors = append(allErrs.SchemaErrors, field.NotSupported(fldPath.Child("x-kubernetes-map-type"), *schema.XMapType, []string{"atomic", "granular"}))
	}

	if schema.XListType != nil && schema.Type != "array" {
		if len(schema.Type) == 0 {
			allErrs.SchemaErrors = append(allErrs.SchemaErrors, field.Required(fldPath.Child("type"), "must be array if x-kubernetes-list-type is specified"))
		} else {
			allErrs.SchemaErrors = append(allErrs.SchemaErrors, field.Invalid(fldPath.Child("type"), schema.Type, "must be array if x-kubernetes-list-type is specified"))
		}
	} else if opts.requireAtomicSetType && schema.XListType != nil && *schema.XListType == "set" && schema.Items != nil && schema.Items.Schema != nil { // by structural schema items are present
		is := schema.Items.Schema
		switch is.Type {
		case "array":
			if is.XListType != nil && *is.XListType != "atomic" { // atomic is the implicit default behaviour if unset, hence != atomic is wrong
				allErrs.SchemaErrors = append(allErrs.SchemaErrors, field.Invalid(fldPath.Child("items").Child("x-kubernetes-list-type"), is.XListType, "must be atomic as item of a list with x-kubernetes-list-type=set"))
			}
		case "object":
			if is.XMapType == nil || *is.XMapType != "atomic" { // granular is the implicit default behaviour if unset, hence nil and != atomic are wrong
				allErrs.SchemaErrors = append(allErrs.SchemaErrors, field.Invalid(fldPath.Child("items").Child("x-kubernetes-map-type"), is.XListType, "must be atomic as item of a list with x-kubernetes-list-type=set"))
			}
		}
	}

	if schema.XListType != nil && *schema.XListType != "atomic" && *schema.XListType != "set" && *schema.XListType != "map" {
		allErrs.SchemaErrors = append(allErrs.SchemaErrors, field.NotSupported(fldPath.Child("x-kubernetes-list-type"), *schema.XListType, []string{"atomic", "set", "map"}))
	}

	if len(schema.XListMapKeys) > 0 {
		if schema.XListType == nil {
			allErrs.SchemaErrors = append(allErrs.SchemaErrors, field.Required(fldPath.Child("x-kubernetes-list-type"), "must be map if x-kubernetes-list-map-keys is non-empty"))
		} else if *schema.XListType != "map" {
			allErrs.SchemaErrors = append(allErrs.SchemaErrors, field.Invalid(fldPath.Child("x-kubernetes-list-type"), *schema.XListType, "must be map if x-kubernetes-list-map-keys is non-empty"))
		}
	}

	if schema.XListType != nil && *schema.XListType == "map" {
		if len(schema.XListMapKeys) == 0 {
			allErrs.SchemaErrors = append(allErrs.SchemaErrors, field.Required(fldPath.Child("x-kubernetes-list-map-keys"), "must not be empty if x-kubernetes-list-type is map"))
		}

		if schema.Items == nil {
			allErrs.SchemaErrors = append(allErrs.SchemaErrors, field.Required(fldPath.Child("items"), "must have a schema if x-kubernetes-list-type is map"))
		}

		if schema.Items != nil && schema.Items.Schema == nil {
			allErrs.SchemaErrors = append(allErrs.SchemaErrors, field.Invalid(fldPath.Child("items"), schema.Items, "must only have a single schema if x-kubernetes-list-type is map"))
		}

		if schema.Items != nil && schema.Items.Schema != nil && schema.Items.Schema.Type != "object" {
			allErrs.SchemaErrors = append(allErrs.SchemaErrors, field.Invalid(fldPath.Child("items").Child("type"), schema.Items.Schema.Type, "must be object if parent array's x-kubernetes-list-type is map"))
		}

		if schema.Items != nil && schema.Items.Schema != nil && schema.Items.Schema.Type == "object" {
			keys := map[string]struct{}{}
			for _, k := range schema.XListMapKeys {
				if s, ok := schema.Items.Schema.Properties[k]; ok {
					if s.Type == "array" || s.Type == "object" {
						allErrs.SchemaErrors = append(allErrs.SchemaErrors, field.Invalid(fldPath.Child("items").Child("properties").Key(k).Child("type"), schema.Items.Schema.Type, "must be a scalar type if parent array's x-kubernetes-list-type is map"))
					}
				} else {
					allErrs.SchemaErrors = append(allErrs.SchemaErrors, field.Invalid(fldPath.Child("x-kubernetes-list-map-keys"), schema.XListMapKeys, "entries must all be names of item properties"))
				}
				if _, ok := keys[k]; ok {
					allErrs.SchemaErrors = append(allErrs.SchemaErrors, field.Invalid(fldPath.Child("x-kubernetes-list-map-keys"), schema.XListMapKeys, "must not contain duplicate entries"))
				}
				keys[k] = struct{}{}
			}
		}
	}

	if opts.requireMapListKeysMapSetValidation {
		allErrs.SchemaErrors = append(allErrs.SchemaErrors, validateMapListKeysMapSet(schema, fldPath)...)
	}
	if len(schema.XValidations) > 0 {
		for i, rule := range schema.XValidations {
			trimmedRule := strings.TrimSpace(rule.Rule)
			trimmedMsg := strings.TrimSpace(rule.Message)
			trimmedMsgExpr := strings.TrimSpace(rule.MessageExpression)
			if len(trimmedRule) == 0 {
				allErrs.SchemaErrors = append(allErrs.SchemaErrors, field.Required(fldPath.Child("x-kubernetes-validations").Index(i).Child("rule"), "rule is not specified"))
			} else if len(rule.Message) > 0 && len(trimmedMsg) == 0 {
				allErrs.SchemaErrors = append(allErrs.SchemaErrors, field.Invalid(fldPath.Child("x-kubernetes-validations").Index(i).Child("message"), rule.Message, "message must be non-empty if specified"))
			} else if hasNewlines(trimmedMsg) {
				allErrs.SchemaErrors = append(allErrs.SchemaErrors, field.Invalid(fldPath.Child("x-kubernetes-validations").Index(i).Child("message"), rule.Message, "message must not contain line breaks"))
			} else if hasNewlines(trimmedRule) && len(trimmedMsg) == 0 {
				allErrs.SchemaErrors = append(allErrs.SchemaErrors, field.Required(fldPath.Child("x-kubernetes-validations").Index(i).Child("message"), "message must be specified if rule contains line breaks"))
			}
			if len(rule.MessageExpression) > 0 && len(trimmedMsgExpr) == 0 {
				allErrs.SchemaErrors = append(allErrs.SchemaErrors, field.Required(fldPath.Child("x-kubernetes-validations").Index(i).Child("messageExpression"), "messageExpression must be non-empty if specified"))
			}
			if rule.Reason != nil && !supportedValidationReason.Has(string(*rule.Reason)) {
				allErrs.SchemaErrors = append(allErrs.SchemaErrors, field.NotSupported(fldPath.Child("x-kubernetes-validations").Index(i).Child("reason"), *rule.Reason, supportedValidationReason.List()))
			}
			trimmedFieldPath := strings.TrimSpace(rule.FieldPath)
			if len(rule.FieldPath) > 0 && len(trimmedFieldPath) == 0 {
				allErrs.SchemaErrors = append(allErrs.SchemaErrors, field.Invalid(fldPath.Child("x-kubernetes-validations").Index(i).Child("fieldPath"), rule.FieldPath, "fieldPath must be non-empty if specified"))
			}
			if hasNewlines(rule.FieldPath) {
				allErrs.SchemaErrors = append(allErrs.SchemaErrors, field.Invalid(fldPath.Child("x-kubernetes-validations").Index(i).Child("fieldPath"), rule.FieldPath, "fieldPath must not contain line breaks"))
			}
			if len(rule.FieldPath) > 0 {
				if !pathValid(schema, rule.FieldPath) {
					allErrs.SchemaErrors = append(allErrs.SchemaErrors, field.Invalid(fldPath.Child("x-kubernetes-validations").Index(i).Child("fieldPath"), rule.FieldPath, "fieldPath must be a valid path"))
				}

			}
		}

		// If any schema related validation errors have been found at this level or deeper, skip CEL expression validation.
		// Invalid OpenAPISchemas are not always possible to convert into valid CEL DeclTypes, and can lead to CEL
		// validation error messages that are not actionable (will go away once the schema errors are resolved) and that
		// are difficult for CEL expression authors to understand.
		if len(allErrs.SchemaErrors) == 0 && celContext != nil {
			typeInfo, err := celContext.TypeInfo()
			if err != nil {
				allErrs.CELErrors = append(allErrs.CELErrors, field.InternalError(fldPath.Child("x-kubernetes-validations"), fmt.Errorf("internal error: failed to construct type information for x-kubernetes-validations rules: %s", err)))
			} else if typeInfo == nil {
				allErrs.CELErrors = append(allErrs.CELErrors, field.InternalError(fldPath.Child("x-kubernetes-validations"), fmt.Errorf("internal error: failed to retrieve type information for x-kubernetes-validations")))
			} else {
				compResults, err := cel.Compile(typeInfo.Schema, typeInfo.DeclType, celconfig.PerCallLimit, opts.celEnvironmentSet, opts.preexistingExpressions)
				if err != nil {
					allErrs.CELErrors = append(allErrs.CELErrors, field.InternalError(fldPath.Child("x-kubernetes-validations"), err))
				} else {
					for i, cr := range compResults {
						expressionCost := getExpressionCost(cr, celContext)
						if !opts.suppressPerExpressionCost && expressionCost > StaticEstimatedCostLimit {
							costErrorMsg := getCostErrorMessage("estimated rule cost", expressionCost, StaticEstimatedCostLimit)
							allErrs.CELErrors = append(allErrs.CELErrors, field.Forbidden(fldPath.Child("x-kubernetes-validations").Index(i).Child("rule"), costErrorMsg))
						}
						if celContext.TotalCost != nil {
							celContext.TotalCost.ObserveExpressionCost(fldPath.Child("x-kubernetes-validations").Index(i).Child("rule"), expressionCost)
						}
						if cr.Error != nil {
							if cr.Error.Type == apiservercel.ErrorTypeRequired {
								allErrs.CELErrors = append(allErrs.CELErrors, field.Required(fldPath.Child("x-kubernetes-validations").Index(i).Child("rule"), cr.Error.Detail))
							} else {
								allErrs.CELErrors = append(allErrs.CELErrors, field.Invalid(fldPath.Child("x-kubernetes-validations").Index(i).Child("rule"), schema.XValidations[i], cr.Error.Detail))
							}
						}
						if cr.MessageExpressionError != nil {
							allErrs.CELErrors = append(allErrs.CELErrors, field.Invalid(fldPath.Child("x-kubernetes-validations").Index(i).Child("messageExpression"), schema.XValidations[i], cr.MessageExpressionError.Detail))
						} else {
							if cr.MessageExpression != nil {
								if !opts.suppressPerExpressionCost && cr.MessageExpressionMaxCost > StaticEstimatedCostLimit {
									costErrorMsg := getCostErrorMessage("estimated messageExpression cost", cr.MessageExpressionMaxCost, StaticEstimatedCostLimit)
									allErrs.CELErrors = append(allErrs.CELErrors, field.Forbidden(fldPath.Child("x-kubernetes-validations").Index(i).Child("messageExpression"), costErrorMsg))
								}
								if celContext.TotalCost != nil {
									celContext.TotalCost.ObserveExpressionCost(fldPath.Child("x-kubernetes-validations").Index(i).Child("messageExpression"), cr.MessageExpressionMaxCost)
								}
							}
						}
						if cr.UsesOldSelf {
							if uncorrelatablePath := ssv.forbidOldSelfValidations(); uncorrelatablePath != nil {
								allErrs.CELErrors = append(allErrs.CELErrors, field.Invalid(fldPath.Child("x-kubernetes-validations").Index(i).Child("rule"), schema.XValidations[i].Rule, fmt.Sprintf("oldSelf cannot be used on the uncorrelatable portion of the schema within %v", uncorrelatablePath)))
							}
						} else if schema.XValidations[i].OptionalOldSelf != nil {
							allErrs.CELErrors = append(allErrs.CELErrors, field.Invalid(fldPath.Child("x-kubernetes-validations").Index(i).Child("optionalOldSelf"), *schema.XValidations[i].OptionalOldSelf, "may not be set if oldSelf is not used in rule"))
						}
					}
				}
			}
		}
	}

	return allErrs
}

func pathValid(schema *apiextensions.JSONSchemaProps, path string) bool {
	// To avoid duplicated code and better maintain, using ValidaFieldPath func to check if the path is valid
	if ss, err := structuralschema.NewStructural(schema); err == nil {
		_, _, err := cel.ValidFieldPath(path, ss)
		return err == nil
	}
	return true
}

// multiplyWithOverflowGuard returns the product of baseCost and cardinality unless that product
// would exceed math.MaxUint, in which case math.MaxUint is returned.
func multiplyWithOverflowGuard(baseCost, cardinality uint64) uint64 {
	if baseCost == 0 {
		// an empty rule can return 0, so guard for that here
		return 0
	} else if math.MaxUint/baseCost < cardinality {
		return math.MaxUint
	}
	return baseCost * cardinality
}

func getExpressionCost(cr cel.CompilationResult, cardinalityCost *CELSchemaContext) uint64 {
	if cardinalityCost.MaxCardinality != unbounded {
		return multiplyWithOverflowGuard(cr.MaxCost, *cardinalityCost.MaxCardinality)
	}
	return multiplyWithOverflowGuard(cr.MaxCost, cr.MaxCardinality)
}

func getCostErrorMessage(costName string, expressionCost, costLimit uint64) string {
	exceedFactor := float64(expressionCost) / float64(costLimit)
	var factor string
	if exceedFactor > 100.0 {
		// if exceedFactor is greater than 2 orders of magnitude, the rule is likely O(n^2) or worse
		// and will probably never validate without some set limits
		// also in such cases the cost estimation is generally large enough to not add any value
		factor = fmt.Sprintf("more than 100x")
	} else if exceedFactor < 1.5 {
		factor = fmt.Sprintf("%fx", exceedFactor) // avoid reporting "exceeds budge by a factor of 1.0x"
	} else {
		factor = fmt.Sprintf("%.1fx", exceedFactor)
	}
	return fmt.Sprintf("%s exceeds budget by factor of %s (try simplifying the rule, or adding maxItems, maxProperties, and maxLength where arrays, maps, and strings are declared)", costName, factor)
}

var newlineMatcher = regexp.MustCompile(`[\n\r]+`) // valid newline chars in CEL grammar
func hasNewlines(s string) bool {
	return newlineMatcher.MatchString(s)
}

func validateMapListKeysMapSet(schema *apiextensions.JSONSchemaProps, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}

	if schema.Items == nil || schema.Items.Schema == nil {
		return nil
	}
	if schema.XListType == nil {
		return nil
	}
	if *schema.XListType != "set" && *schema.XListType != "map" {
		return nil
	}

	// set and map list items cannot be nullable
	if schema.Items.Schema.Nullable {
		allErrs = append(allErrs, field.Forbidden(fldPath.Child("items").Child("nullable"), "cannot be nullable when x-kubernetes-list-type is "+*schema.XListType))
	}

	switch *schema.XListType {
	case "map":
		// ensure all map keys are required or have a default
		isRequired := make(map[string]bool, len(schema.Items.Schema.Required))
		for _, required := range schema.Items.Schema.Required {
			isRequired[required] = true
		}

		for _, k := range schema.XListMapKeys {
			obj, ok := schema.Items.Schema.Properties[k]
			if !ok {
				// we validate that all XListMapKeys are existing properties in ValidateCustomResourceDefinitionOpenAPISchema, so skipping here is ok
				continue
			}

			if isRequired[k] == false && obj.Default == nil {
				allErrs = append(allErrs, field.Required(fldPath.Child("items").Child("properties").Key(k).Child("default"), "this property is in x-kubernetes-list-map-keys, so it must have a default or be a required property"))
			}

			if obj.Nullable {
				allErrs = append(allErrs, field.Forbidden(fldPath.Child("items").Child("properties").Key(k).Child("nullable"), "this property is in x-kubernetes-list-map-keys, so it cannot be nullable"))
			}
		}
	case "set":
		// no other set-specific validation
	}

	return allErrs
}

type specStandardValidatorV3 struct {
	allowDefaults                       bool
	disallowDefaultsReason              string
	isInsideResourceMeta                bool
	requireValidPropertyType            bool
	uncorrelatableOldSelfValidationPath *field.Path
}

func (v *specStandardValidatorV3) withForbiddenDefaults(reason string) specStandardValidator {
	clone := *v
	clone.disallowDefaultsReason = reason
	clone.allowDefaults = false
	return &clone
}

func (v *specStandardValidatorV3) withInsideResourceMeta() specStandardValidator {
	clone := *v
	clone.isInsideResourceMeta = true
	return &clone
}

func (v *specStandardValidatorV3) insideResourceMeta() bool {
	return v.isInsideResourceMeta
}

func (v *specStandardValidatorV3) withForbidOldSelfValidations(path *field.Path) specStandardValidator {
	if v.uncorrelatableOldSelfValidationPath != nil {
		// oldSelf validations are already forbidden. preserve the highest-level path
		// causing oldSelf validations to be forbidden
		return v
	}
	clone := *v
	clone.uncorrelatableOldSelfValidationPath = path
	return &clone
}

func (v *specStandardValidatorV3) forbidOldSelfValidations() *field.Path {
	return v.uncorrelatableOldSelfValidationPath
}

// validate validates against OpenAPI Schema v3.
func (v *specStandardValidatorV3) validate(schema *apiextensions.JSONSchemaProps, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}

	if schema == nil {
		return allErrs
	}

	//
	// WARNING: if anything new is allowed below, NewStructural must be adapted to support it.
	//

	if v.requireValidPropertyType && len(schema.Type) > 0 && !openapiV3Types.Has(schema.Type) {
		allErrs = append(allErrs, field.NotSupported(fldPath.Child("type"), schema.Type, openapiV3Types.List()))
	}

	if schema.Default != nil && !v.allowDefaults {
		detail := "must not be set"
		if len(v.disallowDefaultsReason) > 0 {
			detail += " " + v.disallowDefaultsReason
		}
		allErrs = append(allErrs, field.Forbidden(fldPath.Child("default"), detail))
	}

	if schema.ID != "" {
		allErrs = append(allErrs, field.Forbidden(fldPath.Child("id"), "id is not supported"))
	}

	if schema.AdditionalItems != nil {
		allErrs = append(allErrs, field.Forbidden(fldPath.Child("additionalItems"), "additionalItems is not supported"))
	}

	if len(schema.PatternProperties) != 0 {
		allErrs = append(allErrs, field.Forbidden(fldPath.Child("patternProperties"), "patternProperties is not supported"))
	}

	if len(schema.Definitions) != 0 {
		allErrs = append(allErrs, field.Forbidden(fldPath.Child("definitions"), "definitions is not supported"))
	}

	if schema.Dependencies != nil {
		allErrs = append(allErrs, field.Forbidden(fldPath.Child("dependencies"), "dependencies is not supported"))
	}

	if schema.Ref != nil {
		allErrs = append(allErrs, field.Forbidden(fldPath.Child("$ref"), "$ref is not supported"))
	}

	if schema.Type == "null" {
		allErrs = append(allErrs, field.Forbidden(fldPath.Child("type"), "type cannot be set to null, use nullable as an alternative"))
	}

	if schema.Items != nil && len(schema.Items.JSONSchemas) != 0 {
		allErrs = append(allErrs, field.Forbidden(fldPath.Child("items"), "items must be a schema object and not an array"))
	}

	if v.isInsideResourceMeta && schema.XEmbeddedResource {
		allErrs = append(allErrs, field.Forbidden(fldPath.Child("x-kubernetes-embedded-resource"), "must not be used inside of resource meta"))
	}

	return allErrs
}

// ValidateCustomResourceDefinitionSubresources statically validates
func ValidateCustomResourceDefinitionSubresources(subresources *apiextensions.CustomResourceSubresources, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}

	if subresources == nil {
		return allErrs
	}

	if subresources.Scale != nil {
		if len(subresources.Scale.SpecReplicasPath) == 0 {
			allErrs = append(allErrs, field.Required(fldPath.Child("scale.specReplicasPath"), ""))
		} else {
			// should be constrained json path under .spec
			if errs := validateSimpleJSONPath(subresources.Scale.SpecReplicasPath, fldPath.Child("scale.specReplicasPath")); len(errs) > 0 {
				allErrs = append(allErrs, errs...)
			} else if !strings.HasPrefix(subresources.Scale.SpecReplicasPath, ".spec.") {
				allErrs = append(allErrs, field.Invalid(fldPath.Child("scale.specReplicasPath"), subresources.Scale.SpecReplicasPath, "should be a json path under .spec"))
			}
		}

		if len(subresources.Scale.StatusReplicasPath) == 0 {
			allErrs = append(allErrs, field.Required(fldPath.Child("scale.statusReplicasPath"), ""))
		} else {
			// should be constrained json path under .status
			if errs := validateSimpleJSONPath(subresources.Scale.StatusReplicasPath, fldPath.Child("scale.statusReplicasPath")); len(errs) > 0 {
				allErrs = append(allErrs, errs...)
			} else if !strings.HasPrefix(subresources.Scale.StatusReplicasPath, ".status.") {
				allErrs = append(allErrs, field.Invalid(fldPath.Child("scale.statusReplicasPath"), subresources.Scale.StatusReplicasPath, "should be a json path under .status"))
			}
		}

		// if labelSelectorPath is present, it should be a constrained json path under .status
		if subresources.Scale.LabelSelectorPath != nil && len(*subresources.Scale.LabelSelectorPath) > 0 {
			if errs := validateSimpleJSONPath(*subresources.Scale.LabelSelectorPath, fldPath.Child("scale.labelSelectorPath")); len(errs) > 0 {
				allErrs = append(allErrs, errs...)
			} else if !strings.HasPrefix(*subresources.Scale.LabelSelectorPath, ".spec.") && !strings.HasPrefix(*subresources.Scale.LabelSelectorPath, ".status.") {
				allErrs = append(allErrs, field.Invalid(fldPath.Child("scale.labelSelectorPath"), subresources.Scale.LabelSelectorPath, "should be a json path under either .spec or .status"))
			}
		}
	}

	return allErrs
}

func validateSimpleJSONPath(s string, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}

	switch {
	case len(s) == 0:
		allErrs = append(allErrs, field.Invalid(fldPath, s, "must not be empty"))
	case s[0] != '.':
		allErrs = append(allErrs, field.Invalid(fldPath, s, "must be a simple json path starting with ."))
	case s != ".":
		if cs := strings.Split(s[1:], "."); len(cs) < 1 {
			allErrs = append(allErrs, field.Invalid(fldPath, s, "must be a json path in the dot notation"))
		}
	}

	return allErrs
}

var allowedFieldsAtRootSchema = []string{"Description", "Type", "Format", "Title", "Maximum", "ExclusiveMaximum", "Minimum", "ExclusiveMinimum", "MaxLength", "MinLength", "Pattern", "MaxItems", "MinItems", "UniqueItems", "MultipleOf", "Required", "Items", "Properties", "ExternalDocs", "Example", "XPreserveUnknownFields", "XValidations"}

func allowedAtRootSchema(field string) bool {
	for _, v := range allowedFieldsAtRootSchema {
		if field == v {
			return true
		}
	}
	return false
}

// requireOpenAPISchema returns true if the request group version requires a schema
func requireOpenAPISchema(oldCRDSpec *apiextensions.CustomResourceDefinitionSpec) bool {
	if oldCRDSpec != nil && !allVersionsSpecifyOpenAPISchema(oldCRDSpec) {
		// don't tighten validation on existing persisted data
		return false
	}
	return true
}
func allVersionsSpecifyOpenAPISchema(spec *apiextensions.CustomResourceDefinitionSpec) bool {
	if spec.Validation != nil && spec.Validation.OpenAPIV3Schema != nil {
		return true
	}
	for _, v := range spec.Versions {
		if v.Schema == nil || v.Schema.OpenAPIV3Schema == nil {
			return false
		}
	}
	return true
}

func specHasDefaults(spec *apiextensions.CustomResourceDefinitionSpec) bool {
	return HasSchemaWith(spec, schemaHasDefaults)
}

func schemaHasDefaults(s *apiextensions.JSONSchemaProps) bool {
	return SchemaHas(s, func(s *apiextensions.JSONSchemaProps) bool {
		return s.Default != nil
	})
}

func HasSchemaWith(spec *apiextensions.CustomResourceDefinitionSpec, pred func(s *apiextensions.JSONSchemaProps) bool) bool {
	if spec.Validation != nil && spec.Validation.OpenAPIV3Schema != nil && pred(spec.Validation.OpenAPIV3Schema) {
		return true
	}
	for _, v := range spec.Versions {
		if v.Schema != nil && v.Schema.OpenAPIV3Schema != nil && pred(v.Schema.OpenAPIV3Schema) {
			return true
		}
	}
	return false
}

var schemaPool = sync.Pool{
	New: func() any {
		return new(apiextensions.JSONSchemaProps)
	},
}

func schemaHasRecurse(s *apiextensions.JSONSchemaProps, pred func(s *apiextensions.JSONSchemaProps) bool) bool {
	if s == nil {
		return false
	}
	schema := schemaPool.Get().(*apiextensions.JSONSchemaProps)
	defer schemaPool.Put(schema)
	*schema = *s
	return SchemaHas(schema, pred)
}

// SchemaHas recursively traverses the Schema and calls the `pred`
// predicate to see if the schema contains specific values.
//
// The predicate MUST NOT keep a copy of the json schema NOR modify the
// schema.
func SchemaHas(s *apiextensions.JSONSchemaProps, pred func(s *apiextensions.JSONSchemaProps) bool) bool {
	if s == nil {
		return false
	}

	if pred(s) {
		return true
	}

	if s.Items != nil {
		if s.Items != nil && schemaHasRecurse(s.Items.Schema, pred) {
			return true
		}
		for i := range s.Items.JSONSchemas {
			if schemaHasRecurse(&s.Items.JSONSchemas[i], pred) {
				return true
			}
		}
	}
	for i := range s.AllOf {
		if schemaHasRecurse(&s.AllOf[i], pred) {
			return true
		}
	}
	for i := range s.AnyOf {
		if schemaHasRecurse(&s.AnyOf[i], pred) {
			return true
		}
	}
	for i := range s.OneOf {
		if schemaHasRecurse(&s.OneOf[i], pred) {
			return true
		}
	}
	if schemaHasRecurse(s.Not, pred) {
		return true
	}
	for _, s := range s.Properties {
		if schemaHasRecurse(&s, pred) {
			return true
		}
	}
	if s.AdditionalProperties != nil {
		if schemaHasRecurse(s.AdditionalProperties.Schema, pred) {
			return true
		}
	}
	for _, s := range s.PatternProperties {
		if schemaHasRecurse(&s, pred) {
			return true
		}
	}
	if s.AdditionalItems != nil {
		if schemaHasRecurse(s.AdditionalItems.Schema, pred) {
			return true
		}
	}
	for _, s := range s.Definitions {
		if schemaHasRecurse(&s, pred) {
			return true
		}
	}
	for _, d := range s.Dependencies {
		if schemaHasRecurse(d.Schema, pred) {
			return true
		}
	}

	return false
}

func specHasKubernetesExtensions(spec *apiextensions.CustomResourceDefinitionSpec) bool {
	if spec.Validation != nil && schemaHasKubernetesExtensions(spec.Validation.OpenAPIV3Schema) {
		return true
	}
	for _, v := range spec.Versions {
		if v.Schema != nil && schemaHasKubernetesExtensions(v.Schema.OpenAPIV3Schema) {
			return true
		}
	}
	return false
}

func schemaHasKubernetesExtensions(s *apiextensions.JSONSchemaProps) bool {
	return SchemaHas(s, func(s *apiextensions.JSONSchemaProps) bool {
		return s.XEmbeddedResource || s.XPreserveUnknownFields != nil || s.XIntOrString || len(s.XListMapKeys) > 0 || s.XListType != nil || len(s.XValidations) > 0
	})
}

// requireStructuralSchema returns true if schemas specified must be structural
func requireStructuralSchema(oldCRDSpec *apiextensions.CustomResourceDefinitionSpec) bool {
	if oldCRDSpec != nil && specHasNonStructuralSchema(oldCRDSpec) {
		// don't tighten validation on existing persisted data
		return false
	}
	return true
}

func specHasNonStructuralSchema(spec *apiextensions.CustomResourceDefinitionSpec) bool {
	if spec.Validation != nil && schemaIsNonStructural(spec.Validation.OpenAPIV3Schema) {
		return true
	}
	for _, v := range spec.Versions {
		if v.Schema != nil && schemaIsNonStructural(v.Schema.OpenAPIV3Schema) {
			return true
		}
	}
	return false
}
func schemaIsNonStructural(schema *apiextensions.JSONSchemaProps) bool {
	if schema == nil {
		return false
	}
	ss, err := structuralschema.NewStructural(schema)
	if err != nil {
		return true
	}
	return len(structuralschema.ValidateStructural(nil, ss)) > 0
}

// requirePrunedDefaults returns false if there are any unpruned default in oldCRDSpec, and true otherwise.
func requirePrunedDefaults(oldCRDSpec *apiextensions.CustomResourceDefinitionSpec) bool {
	if oldCRDSpec.Validation != nil {
		if has, err := schemaHasUnprunedDefaults(oldCRDSpec.Validation.OpenAPIV3Schema); err == nil && has {
			return false
		}
	}
	for _, v := range oldCRDSpec.Versions {
		if v.Schema == nil {
			continue
		}
		if has, err := schemaHasUnprunedDefaults(v.Schema.OpenAPIV3Schema); err == nil && has {
			return false
		}
	}
	return true
}
func schemaHasUnprunedDefaults(schema *apiextensions.JSONSchemaProps) (bool, error) {
	if schema == nil || !schemaHasDefaults(schema) {
		return false, nil
	}
	ss, err := structuralschema.NewStructural(schema)
	if err != nil {
		return false, err
	}
	if errs := structuralschema.ValidateStructural(nil, ss); len(errs) > 0 {
		return false, errs.ToAggregate()
	}
	pruned := ss.DeepCopy()
	if err := structuraldefaulting.PruneDefaults(pruned); err != nil {
		return false, err
	}
	return !reflect.DeepEqual(ss, pruned), nil
}

// requireAtomicSetType returns true if the old CRD spec as at least one x-kubernetes-list-type=set with non-atomic items type.
func requireAtomicSetType(oldCRDSpec *apiextensions.CustomResourceDefinitionSpec) bool {
	return !HasSchemaWith(oldCRDSpec, hasNonAtomicSetType)
}

// hasNonAtomicSetType recurses over the schema and returns whether any list of type "set" as non-atomic item types.
func hasNonAtomicSetType(schema *apiextensions.JSONSchemaProps) bool {
	return SchemaHas(schema, func(schema *apiextensions.JSONSchemaProps) bool {
		if schema.XListType != nil && *schema.XListType == "set" && schema.Items != nil && schema.Items.Schema != nil { // we don't support schema.Items.JSONSchemas
			is := schema.Items.Schema
			switch is.Type {
			case "array":
				return is.XListType != nil && *is.XListType != "atomic" // atomic is the implicit default behaviour if unset, hence != atomic is wrong
			case "object":
				return is.XMapType == nil || *is.XMapType != "atomic" // granular is the implicit default behaviour if unset, hence nil and != atomic are wrong
			default:
				return false // scalar types are always atomic
			}
		}
		return false
	})
}

func requireMapListKeysMapSetValidation(oldCRDSpec *apiextensions.CustomResourceDefinitionSpec) bool {
	return !HasSchemaWith(oldCRDSpec, hasInvalidMapListKeysMapSet)
}

func hasInvalidMapListKeysMapSet(schema *apiextensions.JSONSchemaProps) bool {
	return SchemaHas(schema, func(schema *apiextensions.JSONSchemaProps) bool {
		return len(validateMapListKeysMapSet(schema, field.NewPath(""))) > 0
	})
}

// requireValidPropertyType returns true if valid openapi v3 types should be required for the given API version
func requireValidPropertyType(oldCRDSpec *apiextensions.CustomResourceDefinitionSpec) bool {
	if oldCRDSpec != nil && specHasInvalidTypes(oldCRDSpec) {
		// don't tighten validation on existing persisted data
		return false
	}
	return true
}

// validateAPIApproval returns a list of errors if the API approval annotation isn't valid
func validateAPIApproval(newCRD, oldCRD *apiextensions.CustomResourceDefinition) field.ErrorList {
	// check to see if we need confirm API approval for kube group.
	if !apihelpers.IsProtectedCommunityGroup(newCRD.Spec.Group) {
		// no-op for non-protected groups
		return nil
	}

	// default to a state that allows missing values to continue to be missing
	var oldApprovalState *apihelpers.APIApprovalState
	if oldCRD != nil {
		t, _ := apihelpers.GetAPIApprovalState(oldCRD.Annotations)
		oldApprovalState = &t // +k8s:verify-mutation:reason=clone
	}
	newApprovalState, reason := apihelpers.GetAPIApprovalState(newCRD.Annotations)

	// if the approval state hasn't changed, never fail on approval validation
	// this is allowed so that a v1 client that is simply updating spec and not mutating this value doesn't get rejected.  Imagine a controller controlling a CRD spec.
	if oldApprovalState != nil && *oldApprovalState == newApprovalState {
		return nil
	}

	// in v1, we require valid approval strings
	switch newApprovalState {
	case apihelpers.APIApprovalInvalid:
		return field.ErrorList{field.Invalid(field.NewPath("metadata", "annotations").Key(apiextensionsv1beta1.KubeAPIApprovedAnnotation), newCRD.Annotations[apiextensionsv1beta1.KubeAPIApprovedAnnotation], reason)}
	case apihelpers.APIApprovalMissing:
		return field.ErrorList{field.Required(field.NewPath("metadata", "annotations").Key(apiextensionsv1beta1.KubeAPIApprovedAnnotation), reason)}
	case apihelpers.APIApproved, apihelpers.APIApprovalBypassed:
		// success
		return nil
	default:
		return field.ErrorList{field.Invalid(field.NewPath("metadata", "annotations").Key(apiextensionsv1beta1.KubeAPIApprovedAnnotation), newCRD.Annotations[apiextensionsv1beta1.KubeAPIApprovedAnnotation], reason)}
	}
}

func validatePreserveUnknownFields(crd, oldCRD *apiextensions.CustomResourceDefinition) field.ErrorList {
	if oldCRD != nil && oldCRD.Spec.PreserveUnknownFields != nil && *oldCRD.Spec.PreserveUnknownFields {
		// no-op for compatibility with existing data
		return nil
	}

	var errs field.ErrorList
	if crd != nil && crd.Spec.PreserveUnknownFields != nil && *crd.Spec.PreserveUnknownFields {
		// disallow changing spec.preserveUnknownFields=false to spec.preserveUnknownFields=true
		errs = append(errs, field.Invalid(field.NewPath("spec").Child("preserveUnknownFields"), crd.Spec.PreserveUnknownFields, "cannot set to true, set x-kubernetes-preserve-unknown-fields to true in spec.versions[*].schema instead"))
	}
	return errs
}

func specHasInvalidTypes(spec *apiextensions.CustomResourceDefinitionSpec) bool {
	if spec.Validation != nil && SchemaHasInvalidTypes(spec.Validation.OpenAPIV3Schema) {
		return true
	}
	for _, v := range spec.Versions {
		if v.Schema != nil && SchemaHasInvalidTypes(v.Schema.OpenAPIV3Schema) {
			return true
		}
	}
	return false
}

// SchemaHasInvalidTypes returns true if it contains invalid offending openapi-v3 specification.
func SchemaHasInvalidTypes(s *apiextensions.JSONSchemaProps) bool {
	return SchemaHas(s, func(s *apiextensions.JSONSchemaProps) bool {
		return len(s.Type) > 0 && !openapiV3Types.Has(s.Type)
	})
}
