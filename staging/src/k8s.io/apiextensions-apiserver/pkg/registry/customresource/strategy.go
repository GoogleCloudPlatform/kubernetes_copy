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

package customresource

import (
	"context"
	"fmt"
	"strings"

	"sigs.k8s.io/structured-merge-diff/v4/fieldpath"

	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	v1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel/model"
	structurallisttype "k8s.io/apiextensions-apiserver/pkg/apiserver/schema/listtype"
	schemaobjectmeta "k8s.io/apiextensions-apiserver/pkg/apiserver/schema/objectmeta"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	apiextensionsfeatures "k8s.io/apiextensions-apiserver/pkg/features"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/validation/field"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
	"k8s.io/apiserver/pkg/cel/common"
	"k8s.io/apiserver/pkg/registry/generic"
	apiserverstorage "k8s.io/apiserver/pkg/storage"
	"k8s.io/apiserver/pkg/storage/names"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/util/jsonpath"
)

// customResourceStrategy implements behavior for CustomResources for a single
// version
type customResourceStrategy struct {
	runtime.ObjectTyper
	names.NameGenerator

	namespaceScoped    bool
	validator          customResourceValidator
	structuralSchema   *structuralschema.Structural
	celValidator       *cel.Validator
	status             *apiextensions.CustomResourceSubresourceStatus
	scale              *apiextensions.CustomResourceSubresourceScale
	kind               schema.GroupVersionKind
	selectableFieldSet []selectableField
}

type selectableField struct {
	name      string
	fieldPath *jsonpath.JSONPath
	err       error
}

func NewStrategy(typer runtime.ObjectTyper, namespaceScoped bool, kind schema.GroupVersionKind, schemaValidator, statusSchemaValidator validation.SchemaValidator, structuralSchema *structuralschema.Structural, status *apiextensions.CustomResourceSubresourceStatus, scale *apiextensions.CustomResourceSubresourceScale, selectableFields []v1.SelectableField) customResourceStrategy {
	var celValidator *cel.Validator
	celValidator = cel.NewValidator(structuralSchema, true, celconfig.PerCallLimit) // CEL programs are compiled and cached here

	strategy := customResourceStrategy{
		ObjectTyper:     typer,
		NameGenerator:   names.SimpleNameGenerator,
		namespaceScoped: namespaceScoped,
		status:          status,
		scale:           scale,
		validator: customResourceValidator{
			namespaceScoped:       namespaceScoped,
			kind:                  kind,
			schemaValidator:       schemaValidator,
			statusSchemaValidator: statusSchemaValidator,
		},
		structuralSchema: structuralSchema,
		celValidator:     celValidator,
		kind:             kind,
	}
	if utilfeature.DefaultFeatureGate.Enabled(apiextensionsfeatures.CustomResourceFieldSelectors) {
		strategy.selectableFieldSet = prepareSelectableFields(selectableFields)
	}
	return strategy
}

func prepareSelectableFields(selectableFields []v1.SelectableField) []selectableField {
	result := make([]selectableField, len(selectableFields))
	for i, sf := range selectableFields {
		name := strings.TrimPrefix(sf.JSONPath, ".")

		parser := jsonpath.New("selectableField")
		parser.AllowMissingKeys(true)
		err := parser.Parse("{" + sf.JSONPath + "}")
		if err == nil {
			result[i] = selectableField{
				name:      name,
				fieldPath: parser,
			}
		} else {
			result[i] = selectableField{
				name: name,
				err:  err,
			}
		}
	}

	return result
}

func (a customResourceStrategy) NamespaceScoped() bool {
	return a.namespaceScoped
}

// GetResetFields returns the set of fields that get reset by the strategy
// and should not be modified by the user.
func (a customResourceStrategy) GetResetFields() map[fieldpath.APIVersion]*fieldpath.Set {
	fields := map[fieldpath.APIVersion]*fieldpath.Set{}

	if a.status != nil {
		fields[fieldpath.APIVersion(a.kind.GroupVersion().String())] = fieldpath.NewSet(
			fieldpath.MakePathOrDie("status"),
		)
	}

	return fields
}

// PrepareForCreate clears the status of a CustomResource before creation.
func (a customResourceStrategy) PrepareForCreate(ctx context.Context, obj runtime.Object) {
	if a.status != nil {
		customResourceObject := obj.(*unstructured.Unstructured)
		customResource := customResourceObject.UnstructuredContent()

		// create cannot set status
		delete(customResource, "status")
	}

	accessor, _ := meta.Accessor(obj)
	accessor.SetGeneration(1)
}

// PrepareForUpdate clears fields that are not allowed to be set by end users on update.
func (a customResourceStrategy) PrepareForUpdate(ctx context.Context, obj, old runtime.Object) {
	newCustomResourceObject := obj.(*unstructured.Unstructured)
	oldCustomResourceObject := old.(*unstructured.Unstructured)

	newCustomResource := newCustomResourceObject.UnstructuredContent()
	oldCustomResource := oldCustomResourceObject.UnstructuredContent()

	// If the /status subresource endpoint is installed, update is not allowed to set status.
	if a.status != nil {
		_, ok1 := newCustomResource["status"]
		_, ok2 := oldCustomResource["status"]
		switch {
		case ok2:
			newCustomResource["status"] = oldCustomResource["status"]
		case ok1:
			delete(newCustomResource, "status")
		}
	}

	// except for the changes to `metadata`, any other changes
	// cause the generation to increment.
	newCopyContent := copyNonMetadata(newCustomResource)
	oldCopyContent := copyNonMetadata(oldCustomResource)
	if !apiequality.Semantic.DeepEqual(newCopyContent, oldCopyContent) {
		oldAccessor, _ := meta.Accessor(oldCustomResourceObject)
		newAccessor, _ := meta.Accessor(newCustomResourceObject)
		newAccessor.SetGeneration(oldAccessor.GetGeneration() + 1)
	}
}

func copyNonMetadata(original map[string]interface{}) map[string]interface{} {
	ret := make(map[string]interface{})
	for key, val := range original {
		if key == "metadata" {
			continue
		}
		ret[key] = val
	}
	return ret
}

// Validate validates a new CustomResource.
func (a customResourceStrategy) Validate(ctx context.Context, obj runtime.Object) field.ErrorList {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return field.ErrorList{field.Invalid(field.NewPath(""), u, fmt.Sprintf("has type %T. Must be a pointer to an Unstructured type", obj))}
	}

	var errs field.ErrorList
	errs = append(errs, a.validator.Validate(ctx, u, a.scale)...)

	// validate embedded resources
	errs = append(errs, schemaobjectmeta.Validate(nil, u.Object, a.structuralSchema, false)...)

	// validate x-kubernetes-list-type "map" and "set" invariant
	errs = append(errs, structurallisttype.ValidateListSetsAndMaps(nil, a.structuralSchema, u.Object)...)

	// validate x-kubernetes-validations rules
	if celValidator := a.celValidator; celValidator != nil {
		if has, err := hasBlockingErr(errs); has {
			errs = append(errs, err)
		} else {
			err, _ := celValidator.Validate(ctx, nil, a.structuralSchema, u.Object, nil, celconfig.RuntimeCELCostBudget)
			errs = append(errs, err...)
		}
	}

	return errs
}

// WarningsOnCreate returns warnings for the creation of the given object.
func (a customResourceStrategy) WarningsOnCreate(ctx context.Context, obj runtime.Object) []string {
	return generateWarningsFromObj(obj, nil)
}

func generateWarningsFromObj(obj, old runtime.Object) []string {
	var allWarnings []string
	fldPath := field.NewPath("metadata", "finalizers")
	newObjAccessor, err := meta.Accessor(obj)
	if err != nil {
		return allWarnings
	}

	newAdded := sets.NewString(newObjAccessor.GetFinalizers()...)
	if old != nil {
		oldObjAccessor, err := meta.Accessor(old)
		if err != nil {
			return allWarnings
		}
		newAdded = newAdded.Difference(sets.NewString(oldObjAccessor.GetFinalizers()...))
	}

	for _, finalizer := range newAdded.List() {
		allWarnings = append(allWarnings, validateKubeFinalizerName(finalizer, fldPath)...)
	}

	return allWarnings
}

// Canonicalize normalizes the object after validation.
func (customResourceStrategy) Canonicalize(obj runtime.Object) {
}

// AllowCreateOnUpdate is false for CustomResources; this means a POST is
// needed to create one.
func (customResourceStrategy) AllowCreateOnUpdate() bool {
	return false
}

// AllowUnconditionalUpdate is the default update policy for CustomResource objects.
func (customResourceStrategy) AllowUnconditionalUpdate() bool {
	return false
}

// ValidateUpdate is the default update validation for an end user updating status.
func (a customResourceStrategy) ValidateUpdate(ctx context.Context, obj, old runtime.Object) field.ErrorList {
	uNew, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return field.ErrorList{field.Invalid(field.NewPath(""), obj, fmt.Sprintf("has type %T. Must be a pointer to an Unstructured type", obj))}
	}
	uOld, ok := old.(*unstructured.Unstructured)
	if !ok {
		return field.ErrorList{field.Invalid(field.NewPath(""), old, fmt.Sprintf("has type %T. Must be a pointer to an Unstructured type", old))}
	}

	var options []validation.ValidationOption
	var celOptions []cel.Option
	var correlatedObject *common.CorrelatedObject
	if utilfeature.DefaultFeatureGate.Enabled(apiextensionsfeatures.CRDValidationRatcheting) {
		correlatedObject = common.NewCorrelatedObject(uNew.Object, uOld.Object, &model.Structural{Structural: a.structuralSchema})
		options = append(options, validation.WithRatcheting(correlatedObject))
		celOptions = append(celOptions, cel.WithRatcheting(correlatedObject))
	}

	var errs field.ErrorList
	errs = append(errs, a.validator.ValidateUpdate(ctx, uNew, uOld, a.scale, options...)...)

	// Checks the embedded objects. We don't make a difference between update and create for those.
	errs = append(errs, schemaobjectmeta.Validate(nil, uNew.Object, a.structuralSchema, false)...)

	// ratcheting validation of x-kubernetes-list-type value map and set
	if oldErrs := structurallisttype.ValidateListSetsAndMaps(nil, a.structuralSchema, uOld.Object); len(oldErrs) == 0 {
		errs = append(errs, structurallisttype.ValidateListSetsAndMaps(nil, a.structuralSchema, uNew.Object)...)
	}

	// validate x-kubernetes-validations rules
	if celValidator := a.celValidator; celValidator != nil {
		if has, err := hasBlockingErr(errs); has {
			errs = append(errs, err)
		} else {
			err, _ := celValidator.Validate(ctx, nil, a.structuralSchema, uNew.Object, uOld.Object, celconfig.RuntimeCELCostBudget, celOptions...)
			errs = append(errs, err...)
		}
	}

	// No-op if not attached to context
	if utilfeature.DefaultFeatureGate.Enabled(apiextensionsfeatures.CRDValidationRatcheting) {
		validation.Metrics.ObserveRatchetingTime(*correlatedObject.Duration)
	}
	return errs
}

// WarningsOnUpdate returns warnings for the given update.
func (a customResourceStrategy) WarningsOnUpdate(ctx context.Context, obj, old runtime.Object) []string {
	return generateWarningsFromObj(obj, old)
}

// GetAttrs returns labels and fields of a given object for filtering purposes.
func (a customResourceStrategy) GetAttrs(obj runtime.Object) (labels.Set, fields.Set, error) {
	accessor, err := meta.Accessor(obj)
	if err != nil {
		return nil, nil, err
	}
	sFields, err := a.selectableFields(obj, accessor)
	if err != nil {
		return nil, nil, err
	}
	return accessor.GetLabels(), sFields, nil
}

// selectableFields returns a field set that can be used for filter selection.
// This includes metadata.name, metadata.namespace and all custom selectable fields.
func (a customResourceStrategy) selectableFields(obj runtime.Object, objectMeta metav1.Object) (fields.Set, error) {
	objectMetaFields := objectMetaFieldsSet(objectMeta, a.namespaceScoped)
	var selectableFieldsSet fields.Set

	if utilfeature.DefaultFeatureGate.Enabled(apiextensionsfeatures.CustomResourceFieldSelectors) && len(a.selectableFieldSet) > 0 {
		us, ok := obj.(runtime.Unstructured)
		if !ok {
			return nil, fmt.Errorf("unexpected error casting a custom resource to unstructured")
		}
		uc := us.UnstructuredContent()

		selectableFieldsSet = fields.Set{}
		for _, sf := range a.selectableFieldSet {
			if sf.err != nil {
				return nil, fmt.Errorf("unexpected error parsing jsonPath: %w", sf.err)
			}
			results, err := sf.fieldPath.FindResults(uc)
			if err != nil {
				return nil, fmt.Errorf("unexpected error finding value with jsonPath: %w", err)
			}
			var value any

			if len(results) > 0 && len(results[0]) > 0 {
				if len(results) > 1 || len(results[0]) > 1 {
					return nil, fmt.Errorf("unexpectedly received more than one JSON path result")
				}
				value = results[0][0].Interface()
			}

			if value != nil {
				selectableFieldsSet[sf.name] = fmt.Sprint(value)
			} else {
				selectableFieldsSet[sf.name] = ""
			}
		}
	}
	return generic.MergeFieldsSets(objectMetaFields, selectableFieldsSet), nil
}

// objectMetaFieldsSet returns a fields that represent the ObjectMeta.
func objectMetaFieldsSet(objectMeta metav1.Object, namespaceScoped bool) fields.Set {
	if namespaceScoped {
		return fields.Set{
			"metadata.name":      objectMeta.GetName(),
			"metadata.namespace": objectMeta.GetNamespace(),
		}
	}
	return fields.Set{
		"metadata.name": objectMeta.GetName(),
	}
}

// MatchCustomResourceDefinitionStorage is the filter used by the generic etcd backend to route
// watch events from etcd to clients of the apiserver only interested in specific
// labels/fields.
func (a customResourceStrategy) MatchCustomResourceDefinitionStorage(label labels.Selector, field fields.Selector) apiserverstorage.SelectionPredicate {
	return apiserverstorage.SelectionPredicate{
		Label:    label,
		Field:    field,
		GetAttrs: a.GetAttrs,
	}
}

// OpenAPIv3 type/maxLength/maxItems/MaxProperties/required/enum violation/wrong type field validation failures are viewed as blocking err for CEL validation
func hasBlockingErr(errs field.ErrorList) (bool, *field.Error) {
	for _, err := range errs {
		if err.Type == field.ErrorTypeNotSupported || err.Type == field.ErrorTypeRequired || err.Type == field.ErrorTypeTooLong || err.Type == field.ErrorTypeTooMany || err.Type == field.ErrorTypeTypeInvalid {
			berr := *err
			berr.Detail = "some validation rules were not checked because the object was invalid; correct the existing errors to complete validation"
			return true, &berr
		}
	}
	return false, nil
}
