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
	"encoding/json"
	"strings"

	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apiextensions-apiserver/pkg/features"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apiserver/pkg/cel/common"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	openapierrors "k8s.io/kube-openapi/pkg/validation/errors"
	"k8s.io/kube-openapi/pkg/validation/spec"
	"k8s.io/kube-openapi/pkg/validation/strfmt"
	"k8s.io/kube-openapi/pkg/validation/validate"
)

type SchemaValidator interface {
	SchemaCreateValidator
	ValidateUpdate(new, old interface{}, options ...ValidationOption) *validate.Result
}

type SchemaCreateValidator interface {
	Validate(value interface{}, options ...ValidationOption) *validate.Result
}

type ValidationOptions struct {
	// Whether errors from unchanged portions of the schema should be ratcheted
	// This field is ignored for Validate
	Ratcheting bool

	// Correlation between old and new arguments.
	// If set, this is expected to be the correlation between the `new` and
	// `old` arguments to ValidateUpdate, and values for `new` and `old` will
	// be taken from the correlation.
	//
	// This field is ignored for Validate
	//
	// Used for ratcheting, but left as a separate field since it may be used
	// for other purposes in the future.
	CorrelatedObject *common.CorrelatedObject
}

type ValidationOption func(*ValidationOptions)

func NewValidationOptions(opts ...ValidationOption) ValidationOptions {
	options := ValidationOptions{}
	for _, opt := range opts {
		opt(&options)
	}
	return options
}

func WithRatcheting(correlation *common.CorrelatedObject) ValidationOption {
	return func(options *ValidationOptions) {
		options.Ratcheting = true
		options.CorrelatedObject = correlation
	}
}

// basicSchemaValidator wraps a kube-openapi SchemaCreateValidator to
// support ValidateUpdate. It implements ValidateUpdate by simply validating
// the new value via kube-openapi, ignoring the old value
type basicSchemaValidator struct {
	*validate.SchemaValidator
}

func (s basicSchemaValidator) Validate(new interface{}, options ...ValidationOption) *validate.Result {
	return s.SchemaValidator.Validate(new)
}

func (s basicSchemaValidator) ValidateUpdate(new, old interface{}, options ...ValidationOption) *validate.Result {
	return s.Validate(new, options...)
}

// NewSchemaValidator creates an openapi schema validator for the given CRD validation.
//
// If feature `CRDValidationRatcheting` is disabled, this returns validator which
// validates all `Update`s and `Create`s as a `Create` - without considering old value.
//
// If feature `CRDValidationRatcheting` is enabled - the validator returned
// will support ratcheting unchanged correlatable fields across an update.
func NewSchemaValidator(customResourceValidation *apiextensions.JSONSchemaProps) (SchemaValidator, *spec.Schema, error) {
	// Convert CRD schema to openapi schema
	openapiSchema := &spec.Schema{}
	if customResourceValidation != nil {
		// TODO: replace with NewStructural(...).ToGoOpenAPI
		if err := ConvertJSONSchemaPropsWithPostProcess(customResourceValidation, openapiSchema, StripUnsupportedFormatsPostProcess); err != nil {
			return nil, nil, err
		}
	}
	return NewSchemaValidatorFromOpenAPI(openapiSchema), openapiSchema, nil
}

func NewSchemaValidatorFromOpenAPI(openapiSchema *spec.Schema) SchemaValidator {
	if utilfeature.DefaultFeatureGate.Enabled(features.CRDValidationRatcheting) {
		return NewRatchetingSchemaValidator(openapiSchema, nil, "", strfmt.Default)
	}
	return basicSchemaValidator{validate.NewSchemaValidator(openapiSchema, nil, "", strfmt.Default)}

}

// ValidateCustomResourceUpdate validates the transition of Custom Resource from
// `old` to `new` against the schema in the CustomResourceDefinition.
// Both customResource and old represent a JSON data structures.
//
// If feature `CRDValidationRatcheting` is disabled, this behaves identically to
// ValidateCustomResource(customResource).
func ValidateCustomResourceUpdate(fldPath *field.Path, customResource, old interface{}, validator SchemaValidator, options ...ValidationOption) field.ErrorList {
	// Additional feature gate check for sanity
	if !utilfeature.DefaultFeatureGate.Enabled(features.CRDValidationRatcheting) {
		return ValidateCustomResource(nil, customResource, validator)
	} else if validator == nil {
		return nil
	}

	result := validator.ValidateUpdate(customResource, old, options...)
	if result.IsValid() {
		return nil
	}

	return kubeOpenAPIResultToFieldErrors(fldPath, result)
}

// ValidateCustomResource validates the Custom Resource against the schema in the CustomResourceDefinition.
// CustomResource is a JSON data structure.
func ValidateCustomResource(fldPath *field.Path, customResource interface{}, validator SchemaCreateValidator, options ...ValidationOption) field.ErrorList {
	if validator == nil {
		return nil
	}

	result := validator.Validate(customResource, options...)
	if result.IsValid() {
		return nil
	}

	return kubeOpenAPIResultToFieldErrors(fldPath, result)
}

func kubeOpenAPIResultToFieldErrors(fldPath *field.Path, result *validate.Result) field.ErrorList {
	var allErrs field.ErrorList
	for _, err := range result.Errors {
		switch err := err.(type) {

		case *openapierrors.Validation:
			errPath := fldPath
			if len(err.Name) > 0 && err.Name != "." {
				errPath = errPath.Child(strings.TrimPrefix(err.Name, "."))
			}

			switch err.Code() {
			case openapierrors.RequiredFailCode:
				allErrs = append(allErrs, field.Required(errPath, ""))

			case openapierrors.EnumFailCode:
				values := []string{}
				for _, allowedValue := range err.Values {
					if s, ok := allowedValue.(string); ok {
						values = append(values, s)
					} else {
						allowedJSON, _ := json.Marshal(allowedValue)
						values = append(values, string(allowedJSON))
					}
				}
				allErrs = append(allErrs, field.NotSupported(errPath, err.Value, values))

			case openapierrors.TooLongFailCode:
				max := int64(-1)
				if i, ok := err.Valid.(int64); ok {
					max = i
				}
				allErrs = append(allErrs, field.TooLong(errPath, "" /*unused*/, int(max)))

			case openapierrors.MaxItemsFailCode:
				actual := int64(-1)
				if i, ok := err.Value.(int64); ok {
					actual = i
				}
				max := int64(-1)
				if i, ok := err.Valid.(int64); ok {
					max = i
				}
				allErrs = append(allErrs, field.TooMany(errPath, int(actual), int(max)))

			case openapierrors.TooManyPropertiesCode:
				actual := int64(-1)
				if i, ok := err.Value.(int64); ok {
					actual = i
				}
				max := int64(-1)
				if i, ok := err.Valid.(int64); ok {
					max = i
				}
				allErrs = append(allErrs, field.TooMany(errPath, int(actual), int(max)))

			case openapierrors.InvalidTypeCode:
				value := interface{}("")
				if err.Value != nil {
					value = err.Value
				}
				allErrs = append(allErrs, field.TypeInvalid(errPath, value, err.Error()))

			default:
				value := interface{}("")
				if err.Value != nil {
					value = err.Value
				}
				allErrs = append(allErrs, field.Invalid(errPath, value, err.Error()))
			}

		default:
			allErrs = append(allErrs, field.Invalid(fldPath, "", err.Error()))
		}
	}
	return allErrs
}

// ConvertJSONSchemaProps converts the schema from apiextensions.JSONSchemaProps to go-openapi/spec.Schema.
func ConvertJSONSchemaProps(in *apiextensions.JSONSchemaProps, out *spec.Schema) error {
	return ConvertJSONSchemaPropsWithPostProcess(in, out, nil)
}

// PostProcessFunc post-processes one node of a spec.Schema.
type PostProcessFunc func(*spec.Schema) error

// ConvertJSONSchemaPropsWithPostProcess converts the schema from apiextensions.JSONSchemaProps to go-openapi/spec.Schema
// and run a post process step on each JSONSchemaProps node. postProcess is never called for nil schemas.
func ConvertJSONSchemaPropsWithPostProcess(in *apiextensions.JSONSchemaProps, out *spec.Schema, postProcess PostProcessFunc) error {
	if in == nil {
		return nil
	}

	out.ID = in.ID
	out.Schema = spec.SchemaURL(in.Schema)
	out.Description = in.Description
	if in.Type != "" {
		out.Type = spec.StringOrArray([]string{in.Type})
	}
	if in.XIntOrString {
		out.VendorExtensible.AddExtension("x-kubernetes-int-or-string", true)
		out.Type = spec.StringOrArray{"integer", "string"}
	}
	out.Nullable = in.Nullable
	out.Format = in.Format
	out.Title = in.Title
	out.Maximum = in.Maximum
	out.ExclusiveMaximum = in.ExclusiveMaximum
	out.Minimum = in.Minimum
	out.ExclusiveMinimum = in.ExclusiveMinimum
	out.MaxLength = in.MaxLength
	out.MinLength = in.MinLength
	out.Pattern = in.Pattern
	out.MaxItems = in.MaxItems
	out.MinItems = in.MinItems
	out.UniqueItems = in.UniqueItems
	out.MultipleOf = in.MultipleOf
	out.MaxProperties = in.MaxProperties
	out.MinProperties = in.MinProperties
	out.Required = in.Required

	if in.Default != nil {
		out.Default = *(in.Default)
	}
	if in.Example != nil {
		out.Example = *(in.Example)
	}

	if in.Enum != nil {
		out.Enum = make([]interface{}, len(in.Enum))
		for k, v := range in.Enum {
			out.Enum[k] = v
		}
	}

	if err := convertSliceOfJSONSchemaProps(&in.AllOf, &out.AllOf, postProcess); err != nil {
		return err
	}
	if err := convertSliceOfJSONSchemaProps(&in.OneOf, &out.OneOf, postProcess); err != nil {
		return err
	}
	if err := convertSliceOfJSONSchemaProps(&in.AnyOf, &out.AnyOf, postProcess); err != nil {
		return err
	}

	if in.Not != nil {
		in, out := &in.Not, &out.Not
		*out = new(spec.Schema)
		if err := ConvertJSONSchemaPropsWithPostProcess(*in, *out, postProcess); err != nil {
			return err
		}
	}

	var err error
	out.Properties, err = convertMapOfJSONSchemaProps(in.Properties, postProcess)
	if err != nil {
		return err
	}

	out.PatternProperties, err = convertMapOfJSONSchemaProps(in.PatternProperties, postProcess)
	if err != nil {
		return err
	}

	out.Definitions, err = convertMapOfJSONSchemaProps(in.Definitions, postProcess)
	if err != nil {
		return err
	}

	if in.Ref != nil {
		out.Ref, err = spec.NewRef(*in.Ref)
		if err != nil {
			return err
		}
	}

	if in.AdditionalProperties != nil {
		in, out := &in.AdditionalProperties, &out.AdditionalProperties
		*out = new(spec.SchemaOrBool)
		if err := convertJSONSchemaPropsorBool(*in, *out, postProcess); err != nil {
			return err
		}
	}

	if in.AdditionalItems != nil {
		in, out := &in.AdditionalItems, &out.AdditionalItems
		*out = new(spec.SchemaOrBool)
		if err := convertJSONSchemaPropsorBool(*in, *out, postProcess); err != nil {
			return err
		}
	}

	if in.Items != nil {
		in, out := &in.Items, &out.Items
		*out = new(spec.SchemaOrArray)
		if err := convertJSONSchemaPropsOrArray(*in, *out, postProcess); err != nil {
			return err
		}
	}

	if in.Dependencies != nil {
		in, out := &in.Dependencies, &out.Dependencies
		*out = make(spec.Dependencies, len(*in))
		for key, val := range *in {
			newVal := new(spec.SchemaOrStringArray)
			if err := convertJSONSchemaPropsOrStringArray(&val, newVal, postProcess); err != nil {
				return err
			}
			(*out)[key] = *newVal
		}
	}

	if in.ExternalDocs != nil {
		out.ExternalDocs = &spec.ExternalDocumentation{}
		out.ExternalDocs.Description = in.ExternalDocs.Description
		out.ExternalDocs.URL = in.ExternalDocs.URL
	}

	if postProcess != nil {
		if err := postProcess(out); err != nil {
			return err
		}
	}

	if in.XPreserveUnknownFields != nil {
		out.VendorExtensible.AddExtension("x-kubernetes-preserve-unknown-fields", *in.XPreserveUnknownFields)
	}
	if in.XEmbeddedResource {
		out.VendorExtensible.AddExtension("x-kubernetes-embedded-resource", true)
	}
	if len(in.XListMapKeys) != 0 {
		out.VendorExtensible.AddExtension("x-kubernetes-list-map-keys", convertSliceToInterfaceSlice(in.XListMapKeys))
	}
	if in.XListType != nil {
		out.VendorExtensible.AddExtension("x-kubernetes-list-type", *in.XListType)
	}
	if in.XMapType != nil {
		out.VendorExtensible.AddExtension("x-kubernetes-map-type", *in.XMapType)
	}
	if len(in.XValidations) != 0 {
		var serializationValidationRules apiextensionsv1.ValidationRules
		if err := apiextensionsv1.Convert_apiextensions_ValidationRules_To_v1_ValidationRules(&in.XValidations, &serializationValidationRules, nil); err != nil {
			return err
		}
		out.VendorExtensible.AddExtension("x-kubernetes-validations", convertSliceToInterfaceSlice(serializationValidationRules))
	}
	return nil
}

func convertSliceToInterfaceSlice[T any](in []T) []interface{} {
	var res []interface{}
	for _, v := range in {
		res = append(res, v)
	}
	return res
}

func convertSliceOfJSONSchemaProps(in *[]apiextensions.JSONSchemaProps, out *[]spec.Schema, postProcess PostProcessFunc) error {
	if in != nil {
		for _, jsonSchemaProps := range *in {
			schema := spec.Schema{}
			if err := ConvertJSONSchemaPropsWithPostProcess(&jsonSchemaProps, &schema, postProcess); err != nil {
				return err
			}
			*out = append(*out, schema)
		}
	}
	return nil
}

func convertMapOfJSONSchemaProps(in map[string]apiextensions.JSONSchemaProps, postProcess PostProcessFunc) (map[string]spec.Schema, error) {
	if in == nil {
		return nil, nil
	}

	out := make(map[string]spec.Schema)
	for k, jsonSchemaProps := range in {
		schema := spec.Schema{}
		if err := ConvertJSONSchemaPropsWithPostProcess(&jsonSchemaProps, &schema, postProcess); err != nil {
			return nil, err
		}
		out[k] = schema
	}
	return out, nil
}

func convertJSONSchemaPropsOrArray(in *apiextensions.JSONSchemaPropsOrArray, out *spec.SchemaOrArray, postProcess PostProcessFunc) error {
	if in.Schema != nil {
		in, out := &in.Schema, &out.Schema
		*out = new(spec.Schema)
		if err := ConvertJSONSchemaPropsWithPostProcess(*in, *out, postProcess); err != nil {
			return err
		}
	}
	if in.JSONSchemas != nil {
		in, out := &in.JSONSchemas, &out.Schemas
		*out = make([]spec.Schema, len(*in))
		for i := range *in {
			if err := ConvertJSONSchemaPropsWithPostProcess(&(*in)[i], &(*out)[i], postProcess); err != nil {
				return err
			}
		}
	}
	return nil
}

func convertJSONSchemaPropsorBool(in *apiextensions.JSONSchemaPropsOrBool, out *spec.SchemaOrBool, postProcess PostProcessFunc) error {
	out.Allows = in.Allows
	if in.Schema != nil {
		in, out := &in.Schema, &out.Schema
		*out = new(spec.Schema)
		if err := ConvertJSONSchemaPropsWithPostProcess(*in, *out, postProcess); err != nil {
			return err
		}
	}
	return nil
}

func convertJSONSchemaPropsOrStringArray(in *apiextensions.JSONSchemaPropsOrStringArray, out *spec.SchemaOrStringArray, postProcess PostProcessFunc) error {
	out.Property = in.Property
	if in.Schema != nil {
		in, out := &in.Schema, &out.Schema
		*out = new(spec.Schema)
		if err := ConvertJSONSchemaPropsWithPostProcess(*in, *out, postProcess); err != nil {
			return err
		}
	}
	return nil
}
