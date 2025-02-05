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

package v1

// FieldValueErrorReason is a machine-readable value providing more detail about why a field failed the validation.
// +enum
type FieldValueErrorReason string

const (
	// FieldValueRequired is used to report required values that are not
	// provided (e.g. empty strings, null values, or empty arrays).
	FieldValueRequired FieldValueErrorReason = "FieldValueRequired"
	// FieldValueDuplicate is used to report collisions of values that must be
	// unique (e.g. unique IDs).
	FieldValueDuplicate FieldValueErrorReason = "FieldValueDuplicate"
	// FieldValueInvalid is used to report malformed values (e.g. failed regex
	// match, too long, out of bounds).
	FieldValueInvalid FieldValueErrorReason = "FieldValueInvalid"
	// FieldValueForbidden is used to report valid (as per formatting rules)
	// values which would be accepted under some conditions, but which are not
	// permitted by the current conditions (such as security policy).
	FieldValueForbidden FieldValueErrorReason = "FieldValueForbidden"
)

// JSONSchemaProps is a JSON-Schema following Specification Draft 4 (http://json-schema.org/).
type JSONSchemaProps struct {
	ID          string        `json:"id,omitempty" protobuf:"bytes,1,opt,name=id"`
	Schema      JSONSchemaURL `json:"$schema,omitempty" protobuf:"bytes,2,opt,name=schema"`
	Ref         *string       `json:"$ref,omitempty" protobuf:"bytes,3,opt,name=ref"`
	Description string        `json:"description,omitempty" protobuf:"bytes,4,opt,name=description"`
	Type        string        `json:"type,omitempty" protobuf:"bytes,5,opt,name=type"`

	// format is an OpenAPI v3 format string. Unknown formats are ignored. The following formats are validated:
	//
	// - bsonobjectid: a bson object ID, i.e. a 24 characters hex string
	// - uri: an URI as parsed by Golang net/url.ParseRequestURI
	// - email: an email address as parsed by Golang net/mail.ParseAddress
	// - hostname: a valid representation for an Internet host name, as defined by RFC 1034, section 3.1 [RFC1034].
	// - ipv4: an IPv4 IP as parsed by Golang net.ParseIP
	// - ipv6: an IPv6 IP as parsed by Golang net.ParseIP
	// - cidr: a CIDR as parsed by Golang net.ParseCIDR
	// - mac: a MAC address as parsed by Golang net.ParseMAC
	// - uuid: an UUID that allows uppercase defined by the regex (?i)^[0-9a-f]{8}-?[0-9a-f]{4}-?[0-9a-f]{4}-?[0-9a-f]{4}-?[0-9a-f]{12}$
	// - uuid3: an UUID3 that allows uppercase defined by the regex (?i)^[0-9a-f]{8}-?[0-9a-f]{4}-?3[0-9a-f]{3}-?[0-9a-f]{4}-?[0-9a-f]{12}$
	// - uuid4: an UUID4 that allows uppercase defined by the regex (?i)^[0-9a-f]{8}-?[0-9a-f]{4}-?4[0-9a-f]{3}-?[89ab][0-9a-f]{3}-?[0-9a-f]{12}$
	// - uuid5: an UUID5 that allows uppercase defined by the regex (?i)^[0-9a-f]{8}-?[0-9a-f]{4}-?5[0-9a-f]{3}-?[89ab][0-9a-f]{3}-?[0-9a-f]{12}$
	// - isbn: an ISBN10 or ISBN13 number string like "0321751043" or "978-0321751041"
	// - isbn10: an ISBN10 number string like "0321751043"
	// - isbn13: an ISBN13 number string like "978-0321751041"
	// - creditcard: a credit card number defined by the regex ^(?:4[0-9]{12}(?:[0-9]{3})?|5[1-5][0-9]{14}|6(?:011|5[0-9][0-9])[0-9]{12}|3[47][0-9]{13}|3(?:0[0-5]|[68][0-9])[0-9]{11}|(?:2131|1800|35\\d{3})\\d{11})$ with any non digit characters mixed in
	// - ssn: a U.S. social security number following the regex ^\\d{3}[- ]?\\d{2}[- ]?\\d{4}$
	// - hexcolor: an hexadecimal color code like "#FFFFFF: following the regex ^#?([0-9a-fA-F]{3}|[0-9a-fA-F]{6})$
	// - rgbcolor: an RGB color code like rgb like "rgb(255,255,2559"
	// - byte: base64 encoded binary data
	// - password: any kind of string
	// - date: a date string like "2006-01-02" as defined by full-date in RFC3339
	// - duration: a duration string like "22 ns" as parsed by Golang time.ParseDuration or compatible with Scala duration format
	// - datetime: a date time string like "2014-12-15T19:30:20.000Z" as defined by date-time in RFC3339.
	Format string `json:"format,omitempty" protobuf:"bytes,6,opt,name=format"`

	Title string `json:"title,omitempty" protobuf:"bytes,7,opt,name=title"`
	// default is a default value for undefined object fields.
	// Defaulting is a beta feature under the CustomResourceDefaulting feature gate.
	// Defaulting requires spec.preserveUnknownFields to be false.
	Default          *JSON    `json:"default,omitempty" protobuf:"bytes,8,opt,name=default"`
	Maximum          *float64 `json:"maximum,omitempty" protobuf:"bytes,9,opt,name=maximum"`
	ExclusiveMaximum bool     `json:"exclusiveMaximum,omitempty" protobuf:"bytes,10,opt,name=exclusiveMaximum"`
	Minimum          *float64 `json:"minimum,omitempty" protobuf:"bytes,11,opt,name=minimum"`
	ExclusiveMinimum bool     `json:"exclusiveMinimum,omitempty" protobuf:"bytes,12,opt,name=exclusiveMinimum"`
	MaxLength        *int64   `json:"maxLength,omitempty" protobuf:"bytes,13,opt,name=maxLength"`
	MinLength        *int64   `json:"minLength,omitempty" protobuf:"bytes,14,opt,name=minLength"`
	Pattern          string   `json:"pattern,omitempty" protobuf:"bytes,15,opt,name=pattern"`
	MaxItems         *int64   `json:"maxItems,omitempty" protobuf:"bytes,16,opt,name=maxItems"`
	MinItems         *int64   `json:"minItems,omitempty" protobuf:"bytes,17,opt,name=minItems"`
	UniqueItems      bool     `json:"uniqueItems,omitempty" protobuf:"bytes,18,opt,name=uniqueItems"`
	MultipleOf       *float64 `json:"multipleOf,omitempty" protobuf:"bytes,19,opt,name=multipleOf"`
	// +listType=atomic
	Enum          []JSON `json:"enum,omitempty" protobuf:"bytes,20,rep,name=enum"`
	MaxProperties *int64 `json:"maxProperties,omitempty" protobuf:"bytes,21,opt,name=maxProperties"`
	MinProperties *int64 `json:"minProperties,omitempty" protobuf:"bytes,22,opt,name=minProperties"`
	// +listType=atomic
	Required []string                `json:"required,omitempty" protobuf:"bytes,23,rep,name=required"`
	Items    *JSONSchemaPropsOrArray `json:"items,omitempty" protobuf:"bytes,24,opt,name=items"`
	// +listType=atomic
	AllOf []JSONSchemaProps `json:"allOf,omitempty" protobuf:"bytes,25,rep,name=allOf"`
	// +listType=atomic
	OneOf []JSONSchemaProps `json:"oneOf,omitempty" protobuf:"bytes,26,rep,name=oneOf"`
	// +listType=atomic
	AnyOf                []JSONSchemaProps          `json:"anyOf,omitempty" protobuf:"bytes,27,rep,name=anyOf"`
	Not                  *JSONSchemaProps           `json:"not,omitempty" protobuf:"bytes,28,opt,name=not"`
	Properties           map[string]JSONSchemaProps `json:"properties,omitempty" protobuf:"bytes,29,rep,name=properties"`
	AdditionalProperties *JSONSchemaPropsOrBool     `json:"additionalProperties,omitempty" protobuf:"bytes,30,opt,name=additionalProperties"`
	PatternProperties    map[string]JSONSchemaProps `json:"patternProperties,omitempty" protobuf:"bytes,31,rep,name=patternProperties"`
	Dependencies         JSONSchemaDependencies     `json:"dependencies,omitempty" protobuf:"bytes,32,opt,name=dependencies"`
	AdditionalItems      *JSONSchemaPropsOrBool     `json:"additionalItems,omitempty" protobuf:"bytes,33,opt,name=additionalItems"`
	Definitions          JSONSchemaDefinitions      `json:"definitions,omitempty" protobuf:"bytes,34,opt,name=definitions"`
	ExternalDocs         *ExternalDocumentation     `json:"externalDocs,omitempty" protobuf:"bytes,35,opt,name=externalDocs"`
	Example              *JSON                      `json:"example,omitempty" protobuf:"bytes,36,opt,name=example"`
	Nullable             bool                       `json:"nullable,omitempty" protobuf:"bytes,37,opt,name=nullable"`

	// x-kubernetes-preserve-unknown-fields stops the API server
	// decoding step from pruning fields which are not specified
	// in the validation schema. This affects fields recursively,
	// but switches back to normal pruning behaviour if nested
	// properties or additionalProperties are specified in the schema.
	// This can either be true or undefined. False is forbidden.
	XPreserveUnknownFields *bool `json:"x-kubernetes-preserve-unknown-fields,omitempty" protobuf:"bytes,38,opt,name=xKubernetesPreserveUnknownFields"`

	// x-kubernetes-embedded-resource defines that the value is an
	// embedded Kubernetes runtime.Object, with TypeMeta and
	// ObjectMeta. The type must be object. It is allowed to further
	// restrict the embedded object. kind, apiVersion and metadata
	// are validated automatically. x-kubernetes-preserve-unknown-fields
	// is allowed to be true, but does not have to be if the object
	// is fully specified (up to kind, apiVersion, metadata).
	XEmbeddedResource bool `json:"x-kubernetes-embedded-resource,omitempty" protobuf:"bytes,39,opt,name=xKubernetesEmbeddedResource"`

	// x-kubernetes-int-or-string specifies that this value is
	// either an integer or a string. If this is true, an empty
	// type is allowed and type as child of anyOf is permitted
	// if following one of the following patterns:
	//
	// 1) anyOf:
	//    - type: integer
	//    - type: string
	// 2) allOf:
	//    - anyOf:
	//      - type: integer
	//      - type: string
	//    - ... zero or more
	XIntOrString bool `json:"x-kubernetes-int-or-string,omitempty" protobuf:"bytes,40,opt,name=xKubernetesIntOrString"`

	// x-kubernetes-list-map-keys annotates an array with the x-kubernetes-list-type `map` by specifying the keys used
	// as the index of the map.
	//
	// This tag MUST only be used on lists that have the "x-kubernetes-list-type"
	// extension set to "map". Also, the values specified for this attribute must
	// be a scalar typed field of the child structure (no nesting is supported).
	//
	// The properties specified must either be required or have a default value,
	// to ensure those properties are present for all list items.
	//
	// +optional
	// +listType=atomic
	XListMapKeys []string `json:"x-kubernetes-list-map-keys,omitempty" protobuf:"bytes,41,rep,name=xKubernetesListMapKeys"`

	// x-kubernetes-list-type annotates an array to further describe its topology.
	// This extension must only be used on lists and may have 3 possible values:
	//
	// 1) `atomic`: the list is treated as a single entity, like a scalar.
	//      Atomic lists will be entirely replaced when updated. This extension
	//      may be used on any type of list (struct, scalar, ...).
	// 2) `set`:
	//      Sets are lists that must not have multiple items with the same value. Each
	//      value must be a scalar, an object with x-kubernetes-map-type `atomic` or an
	//      array with x-kubernetes-list-type `atomic`.
	// 3) `map`:
	//      These lists are like maps in that their elements have a non-index key
	//      used to identify them. Order is preserved upon merge. The map tag
	//      must only be used on a list with elements of type object.
	// Defaults to atomic for arrays.
	// +optional
	XListType *string `json:"x-kubernetes-list-type,omitempty" protobuf:"bytes,42,opt,name=xKubernetesListType"`

	// x-kubernetes-map-type annotates an object to further describe its topology.
	// This extension must only be used when type is object and may have 2 possible values:
	//
	// 1) `granular`:
	//      These maps are actual maps (key-value pairs) and each fields are independent
	//      from each other (they can each be manipulated by separate actors). This is
	//      the default behaviour for all maps.
	// 2) `atomic`: the list is treated as a single entity, like a scalar.
	//      Atomic maps will be entirely replaced when updated.
	// +optional
	XMapType *string `json:"x-kubernetes-map-type,omitempty" protobuf:"bytes,43,opt,name=xKubernetesMapType"`

	// x-kubernetes-validations describes a list of validation rules written in the CEL expression language.
	// +patchMergeKey=rule
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=rule
	XValidations ValidationRules `json:"x-kubernetes-validations,omitempty" patchStrategy:"merge" patchMergeKey:"rule" protobuf:"bytes,44,rep,name=xKubernetesValidations"`
}

// ValidationRules describes a list of validation rules written in the CEL expression language.
type ValidationRules []ValidationRule

// ValidationRule describes a validation rule written in the CEL expression language.
type ValidationRule struct {
	// Rule represents the expression which will be evaluated by CEL.
	// ref: https://github.com/google/cel-spec
	// The Rule is scoped to the location of the x-kubernetes-validations extension in the schema.
	// The `self` variable in the CEL expression is bound to the scoped value.
	// Example:
	// - Rule scoped to the root of a resource with a status subresource: {"rule": "self.status.actual <= self.spec.maxDesired"}
	//
	// If the Rule is scoped to an object with properties, the accessible properties of the object are field selectable
	// via `self.field` and field presence can be checked via `has(self.field)`. Null valued fields are treated as
	// absent fields in CEL expressions.
	// If the Rule is scoped to an object with additionalProperties (i.e. a map) the value of the map
	// are accessible via `self[mapKey]`, map containment can be checked via `mapKey in self` and all entries of the map
	// are accessible via CEL macros and functions such as `self.all(...)`.
	// If the Rule is scoped to an array, the elements of the array are accessible via `self[i]` and also by macros and
	// functions.
	// If the Rule is scoped to a scalar, `self` is bound to the scalar value.
	// Examples:
	// - Rule scoped to a map of objects: {"rule": "self.components['Widget'].priority < 10"}
	// - Rule scoped to a list of integers: {"rule": "self.values.all(value, value >= 0 && value < 100)"}
	// - Rule scoped to a string value: {"rule": "self.startsWith('kube')"}
	//
	// The `apiVersion`, `kind`, `metadata.name` and `metadata.generateName` are always accessible from the root of the
	// object and from any x-kubernetes-embedded-resource annotated objects. No other metadata properties are accessible.
	//
	// Unknown data preserved in custom resources via x-kubernetes-preserve-unknown-fields is not accessible in CEL
	// expressions. This includes:
	// - Unknown field values that are preserved by object schemas with x-kubernetes-preserve-unknown-fields.
	// - Object properties where the property schema is of an "unknown type". An "unknown type" is recursively defined as:
	//   - A schema with no type and x-kubernetes-preserve-unknown-fields set to true
	//   - An array where the items schema is of an "unknown type"
	//   - An object where the additionalProperties schema is of an "unknown type"
	//
	// Only property names of the form `[a-zA-Z_.-/][a-zA-Z0-9_.-/]*` are accessible.
	// Accessible property names are escaped according to the following rules when accessed in the expression:
	// - '__' escapes to '__underscores__'
	// - '.' escapes to '__dot__'
	// - '-' escapes to '__dash__'
	// - '/' escapes to '__slash__'
	// - Property names that exactly match a CEL RESERVED keyword escape to '__{keyword}__'. The keywords are:
	//	  "true", "false", "null", "in", "as", "break", "const", "continue", "else", "for", "function", "if",
	//	  "import", "let", "loop", "package", "namespace", "return".
	// Examples:
	//   - Rule accessing a property named "namespace": {"rule": "self.__namespace__ > 0"}
	//   - Rule accessing a property named "x-prop": {"rule": "self.x__dash__prop > 0"}
	//   - Rule accessing a property named "redact__d": {"rule": "self.redact__underscores__d > 0"}
	//
	// Equality on arrays with x-kubernetes-list-type of 'set' or 'map' ignores element order, i.e. [1, 2] == [2, 1].
	// Concatenation on arrays with x-kubernetes-list-type use the semantics of the list type:
	//   - 'set': `X + Y` performs a union where the array positions of all elements in `X` are preserved and
	//     non-intersecting elements in `Y` are appended, retaining their partial order.
	//   - 'map': `X + Y` performs a merge where the array positions of all keys in `X` are preserved but the values
	//     are overwritten by values in `Y` when the key sets of `X` and `Y` intersect. Elements in `Y` with
	//     non-intersecting keys are appended, retaining their partial order.
	//
	// If `rule` makes use of the `oldSelf` variable it is implicitly a
	// `transition rule`.
	//
	// By default, the `oldSelf` variable is the same type as `self`.
	// When `optionalOldSelf` is true, the `oldSelf` variable is a CEL optional
	//  variable whose value() is the same type as `self`.
	// See the documentation for the `optionalOldSelf` field for details.
	//
	// Transition rules by default are applied only on UPDATE requests and are
	// skipped if an old value could not be found. You can opt a transition
	// rule into unconditional evaluation by setting `optionalOldSelf` to true.
	//
	Rule string `json:"rule" protobuf:"bytes,1,opt,name=rule"`
	// Message represents the message displayed when validation fails. The message is required if the Rule contains
	// line breaks. The message must not contain line breaks.
	// If unset, the message is "failed rule: {Rule}".
	// e.g. "must be a URL with the host matching spec.host"
	Message string `json:"message,omitempty" protobuf:"bytes,2,opt,name=message"`
	// MessageExpression declares a CEL expression that evaluates to the validation failure message that is returned when this rule fails.
	// Since messageExpression is used as a failure message, it must evaluate to a string.
	// If both message and messageExpression are present on a rule, then messageExpression will be used if validation
	// fails. If messageExpression results in a runtime error, the runtime error is logged, and the validation failure message is produced
	// as if the messageExpression field were unset. If messageExpression evaluates to an empty string, a string with only spaces, or a string
	// that contains line breaks, then the validation failure message will also be produced as if the messageExpression field were unset, and
	// the fact that messageExpression produced an empty string/string with only spaces/string with line breaks will be logged.
	// messageExpression has access to all the same variables as the rule; the only difference is the return type.
	// Example:
	// "x must be less than max ("+string(self.max)+")"
	// +optional
	MessageExpression string `json:"messageExpression,omitempty" protobuf:"bytes,3,opt,name=messageExpression"`
	// reason provides a machine-readable validation failure reason that is returned to the caller when a request fails this validation rule.
	// The HTTP status code returned to the caller will match the reason of the reason of the first failed validation rule.
	// The currently supported reasons are: "FieldValueInvalid", "FieldValueForbidden", "FieldValueRequired", "FieldValueDuplicate".
	// If not set, default to use "FieldValueInvalid".
	// All future added reasons must be accepted by clients when reading this value and unknown reasons should be treated as FieldValueInvalid.
	// +optional
	Reason *FieldValueErrorReason `json:"reason,omitempty" protobuf:"bytes,4,opt,name=reason"`
	// fieldPath represents the field path returned when the validation fails.
	// It must be a relative JSON path (i.e. with array notation) scoped to the location of this x-kubernetes-validations extension in the schema and refer to an existing field.
	// e.g. when validation checks if a specific attribute `foo` under a map `testMap`, the fieldPath could be set to `.testMap.foo`
	// If the validation checks two lists must have unique attributes, the fieldPath could be set to either of the list: e.g. `.testList`
	// It does not support list numeric index.
	// It supports child operation to refer to an existing field currently. Refer to [JSONPath support in Kubernetes](https://kubernetes.io/docs/reference/kubectl/jsonpath/) for more info.
	// Numeric index of array is not supported.
	// For field name which contains special characters, use `['specialName']` to refer the field name.
	// e.g. for attribute `foo.34$` appears in a list `testList`, the fieldPath could be set to `.testList['foo.34$']`
	// +optional
	FieldPath string `json:"fieldPath,omitempty" protobuf:"bytes,5,opt,name=fieldPath"`

	// optionalOldSelf is used to opt a transition rule into evaluation
	// even when the object is first created, or if the old object is
	// missing the value.
	//
	// When enabled `oldSelf` will be a CEL optional whose value will be
	// `None` if there is no old value, or when the object is initially created.
	//
	// You may check for presence of oldSelf using `oldSelf.hasValue()` and
	// unwrap it after checking using `oldSelf.value()`. Check the CEL
	// documentation for Optional types for more information:
	// https://pkg.go.dev/github.com/google/cel-go/cel#OptionalTypes
	//
	// May not be set unless `oldSelf` is used in `rule`.
	//
	// +featureGate=CRDValidationRatcheting
	// +optional
	OptionalOldSelf *bool `json:"optionalOldSelf,omitempty" protobuf:"bytes,6,opt,name=optionalOldSelf"`
}

// JSON represents any valid JSON value.
// These types are supported: bool, int64, float64, string, []interface{}, map[string]interface{} and nil.
type JSON struct {
	Raw []byte `json:"-" protobuf:"bytes,1,opt,name=raw"`
}

// OpenAPISchemaType is used by the kube-openapi generator when constructing
// the OpenAPI spec of this type.
//
// See: https://github.com/kubernetes/kube-openapi/tree/master/pkg/generators
func (JSON) OpenAPISchemaType() []string {
	return nil
}

// OpenAPIV3OneOfTypes is used by the kube-openapi generator when constructing
// the OpenAPI spec of this type.
//
// See: https://github.com/kubernetes/kube-openapi/tree/master/pkg/generators
func (JSON) OpenAPIV3OneOfTypes() []string {
	return []string{
		"string",
		"number",
		"integer",
		"boolean",
		"array",
		"object",
	}
}

// OpenAPISchemaFormat is used by the kube-openapi generator when constructing
// the OpenAPI spec of this type.
func (_ JSON) OpenAPISchemaFormat() string { return "" }

// JSONSchemaURL represents a schema url.
type JSONSchemaURL string

// JSONSchemaPropsOrArray represents a value that can either be a JSONSchemaProps
// or an array of JSONSchemaProps. Mainly here for serialization purposes.
type JSONSchemaPropsOrArray struct {
	Schema *JSONSchemaProps `protobuf:"bytes,1,opt,name=schema"`
	// +listType=atomic
	JSONSchemas []JSONSchemaProps `protobuf:"bytes,2,rep,name=jSONSchemas"`
}

// OpenAPISchemaType is used by the kube-openapi generator when constructing
// the OpenAPI spec of this type.
//
// See: https://github.com/kubernetes/kube-openapi/tree/master/pkg/generators
func (_ JSONSchemaPropsOrArray) OpenAPISchemaType() []string {
	// TODO: return actual types when anyOf is supported
	return nil
}

// OpenAPISchemaFormat is used by the kube-openapi generator when constructing
// the OpenAPI spec of this type.
func (_ JSONSchemaPropsOrArray) OpenAPISchemaFormat() string { return "" }

// JSONSchemaPropsOrBool represents JSONSchemaProps or a boolean value.
// Defaults to true for the boolean property.
type JSONSchemaPropsOrBool struct {
	Allows bool             `protobuf:"varint,1,opt,name=allows"`
	Schema *JSONSchemaProps `protobuf:"bytes,2,opt,name=schema"`
}

// OpenAPISchemaType is used by the kube-openapi generator when constructing
// the OpenAPI spec of this type.
//
// See: https://github.com/kubernetes/kube-openapi/tree/master/pkg/generators
func (_ JSONSchemaPropsOrBool) OpenAPISchemaType() []string {
	// TODO: return actual types when anyOf is supported
	return nil
}

// OpenAPISchemaFormat is used by the kube-openapi generator when constructing
// the OpenAPI spec of this type.
func (_ JSONSchemaPropsOrBool) OpenAPISchemaFormat() string { return "" }

// JSONSchemaDependencies represent a dependencies property.
type JSONSchemaDependencies map[string]JSONSchemaPropsOrStringArray

// JSONSchemaPropsOrStringArray represents a JSONSchemaProps or a string array.
type JSONSchemaPropsOrStringArray struct {
	Schema *JSONSchemaProps `protobuf:"bytes,1,opt,name=schema"`
	// +listType=atomic
	Property []string `protobuf:"bytes,2,rep,name=property"`
}

// OpenAPISchemaType is used by the kube-openapi generator when constructing
// the OpenAPI spec of this type.
//
// See: https://github.com/kubernetes/kube-openapi/tree/master/pkg/generators
func (_ JSONSchemaPropsOrStringArray) OpenAPISchemaType() []string {
	// TODO: return actual types when anyOf is supported
	return nil
}

// OpenAPISchemaFormat is used by the kube-openapi generator when constructing
// the OpenAPI spec of this type.
func (_ JSONSchemaPropsOrStringArray) OpenAPISchemaFormat() string { return "" }

// JSONSchemaDefinitions contains the models explicitly defined in this spec.
type JSONSchemaDefinitions map[string]JSONSchemaProps

// ExternalDocumentation allows referencing an external resource for extended documentation.
type ExternalDocumentation struct {
	Description string `json:"description,omitempty" protobuf:"bytes,1,opt,name=description"`
	URL         string `json:"url,omitempty" protobuf:"bytes,2,opt,name=url"`
}
