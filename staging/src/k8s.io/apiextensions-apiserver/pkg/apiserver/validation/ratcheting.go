/*
Copyright 2023 The Kubernetes Authors.

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
	"reflect"

	"k8s.io/apiserver/pkg/cel/common"
	celopenapi "k8s.io/apiserver/pkg/cel/openapi"
	"k8s.io/kube-openapi/pkg/validation/spec"
	"k8s.io/kube-openapi/pkg/validation/strfmt"
	"k8s.io/kube-openapi/pkg/validation/validate"
)

// schemaArgs are the arguments to constructor for OpenAPI schema validator,
// NewSchemaValidator
type schemaArgs struct {
	schema       *spec.Schema
	root         interface{}
	path         string
	knownFormats strfmt.Registry
	options      []validate.Option
}

// RatchetingSchemaValidator wraps kube-openapis SchemaValidator to provide a
// ValidateUpdate function which allows ratcheting
type RatchetingSchemaValidator struct {
	schemaArgs
}

func NewRatchetingSchemaValidator(schema *spec.Schema, rootSchema interface{}, root string, formats strfmt.Registry, options ...validate.Option) *RatchetingSchemaValidator {
	return &RatchetingSchemaValidator{
		schemaArgs: schemaArgs{
			schema:       schema,
			root:         rootSchema,
			path:         root,
			knownFormats: formats,
			options:      options,
		},
	}
}

func (r *RatchetingSchemaValidator) Validate(new interface{}, options ...ValidationOption) *validate.Result {
	sv := validate.NewSchemaValidator(r.schema, r.root, r.path, r.knownFormats, r.options...)
	return sv.Validate(new)
}

func (r *RatchetingSchemaValidator) ValidateUpdate(new, old interface{}, options ...ValidationOption) *validate.Result {
	opts := NewValidationOptions(options...)

	if !opts.Ratcheting {
		sv := validate.NewSchemaValidator(r.schema, r.root, r.path, r.knownFormats, r.options...)
		return sv.Validate(new)
	}

	correlation := opts.CorrelatedObject
	if correlation == nil {
		correlation = common.NewCorrelatedObject(new, old, &celopenapi.Schema{Schema: r.schema})
	}

	return newRatchetingValueValidator(
		correlation,
		r.schemaArgs,
	).Validate(new)
}

// ratchetingValueValidator represents an invocation of SchemaValidator.ValidateUpdate
// for specific arguments for `old` and `new`
//
// It follows the openapi SchemaValidator down its traversal of the new value
// by injecting validate.Option into each recursive invocation.
//
// A ratchetingValueValidator will be constructed and added to the tree for
// each explored sub-index and sub-property during validation.
//
// It's main job is to keep the old/new values correlated as the traversal
// continues, and postprocess errors according to our ratcheting policy.
//
// ratchetingValueValidator is not thread safe.
type ratchetingValueValidator struct {
	// schemaArgs provides the arguments to use in the temporary SchemaValidator
	// that is created during a call to Validate.
	schemaArgs
	correlation *common.CorrelatedObject
}

func newRatchetingValueValidator(correlation *common.CorrelatedObject, args schemaArgs) *ratchetingValueValidator {
	return &ratchetingValueValidator{
		schemaArgs:  args,
		correlation: correlation,
	}
}

// getValidateOption provides a kube-openapi validate.Option for SchemaValidator
// that injects a ratchetingValueValidator to be used for all subkeys and subindices
func (r *ratchetingValueValidator) getValidateOption() validate.Option {
	return func(svo *validate.SchemaValidatorOptions) {
		svo.NewValidatorForField = r.SubPropertyValidator
		svo.NewValidatorForIndex = r.SubIndexValidator
	}
}

// Validate validates the update from r.oldValue to r.value
//
// During evaluation, a temporary tree of ratchetingValueValidator is built for all
// traversed field paths. It is necessary to build the tree to take advantage of
// DeepEqual checks performed by lower levels of the object during validation without
// greatly modifying `kube-openapi`'s implementation.
//
// The tree, and all cache storage/scratch space for the validation of a single
// call to `Validate` is thrown away at the end of the top-level call
// to `Validate`.
//
// `Validate` will create a node in the tree to for each of the explored children.
// The node's main purpose is to store a lazily computed DeepEqual check between
// the oldValue and the currently passed value. If the check is performed, it
// will be stored in the node to be re-used by a parent node during a DeepEqual
// comparison, if necessary.
//
// This call has a side-effect of populating it's `children` variable with
// the explored nodes of the object tree.
func (r *ratchetingValueValidator) Validate(new interface{}) *validate.Result {
	opts := append([]validate.Option{
		r.getValidateOption(),
	}, r.options...)

	s := validate.NewSchemaValidator(r.schema, r.root, r.path, r.knownFormats, opts...)

	res := s.Validate(r.correlation.Value)

	if res.IsValid() {
		return res
	}

	// Current ratcheting rule is to ratchet errors if DeepEqual(old, new) is true.
	if r.correlation.CachedDeepEqual() {
		newRes := &validate.Result{}
		newRes.MergeAsWarnings(res)
		return newRes
	}

	return res
}

// SubPropertyValidator overrides the standard validator constructor for sub-properties by
// returning our special ratcheting variant.
//
// If we can correlate an old value, we return a ratcheting validator to
// use for the child.
//
// If the old value cannot be correlated, then default validation is used.
func (r *ratchetingValueValidator) SubPropertyValidator(field string, schema *spec.Schema, rootSchema interface{}, root string, formats strfmt.Registry, options ...validate.Option) validate.ValueValidator {
	childNode := r.correlation.Key(field)
	if childNode == nil || (r.path == "" && isTypeMetaField(field)) {
		// Defer to default validation if we cannot correlate the old value
		// or if we are validating the root object and the field is a metadata
		// field.
		//
		// We cannot ratchet changes to the APIVersion field since they aren't visible.
		// (both old and new are converted to the same type)
		return validate.NewSchemaValidator(schema, rootSchema, root, formats, options...)
	}

	return newRatchetingValueValidator(childNode, schemaArgs{
		schema:       schema,
		root:         rootSchema,
		path:         root,
		knownFormats: formats,
		options:      options,
	})
}

// SubIndexValidator overrides the standard validator constructor for sub-indices by
// returning our special ratcheting variant.
//
// If we can correlate an old value, we return a ratcheting validator to
// use for the child.
//
// If the old value cannot be correlated, then default validation is used.
func (r *ratchetingValueValidator) SubIndexValidator(index int, schema *spec.Schema, rootSchema interface{}, root string, formats strfmt.Registry, options ...validate.Option) validate.ValueValidator {
	childNode := r.correlation.Index(index)
	if childNode == nil {
		return validate.NewSchemaValidator(schema, rootSchema, root, formats, options...)
	}

	return newRatchetingValueValidator(childNode, schemaArgs{
		schema:       schema,
		root:         rootSchema,
		path:         root,
		knownFormats: formats,
		options:      options,
	})
}

var _ validate.ValueValidator = (&ratchetingValueValidator{})

func (r ratchetingValueValidator) SetPath(path string) {
	// Do nothing
	// Unused by kube-openapi
}

func (r ratchetingValueValidator) Applies(source interface{}, valueKind reflect.Kind) bool {
	return true
}

func isTypeMetaField(path string) bool {
	return path == "kind" || path == "apiVersion"
}
