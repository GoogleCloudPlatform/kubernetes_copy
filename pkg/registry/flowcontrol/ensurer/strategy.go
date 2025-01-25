/*
Copyright 2021 The Kubernetes Authors.

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

package ensurer

import (
	"context"
	"fmt"
	"strconv"

	"github.com/google/go-cmp/cmp"
	flowcontrolv1 "k8s.io/api/flowcontrol/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
)

const (
	fieldManager = "api-priority-and-fairness-config-producer-v1"
)

// EnsureStrategy provides a maintenance strategy for APF configuration objects.
// We have two types of strategy, corresponding to the two types of config objetcs:
//
//   - mandatory: the mandatory configurationWrapper objects are about ensuring that the P&F
//     system itself won't crash; we have to be sure there's 'catch-all' place for
//     everything to go. Any changes made by the cluster operators to these
//     configurationWrapper objects will be stomped by the apiserver.
//
//   - suggested: additional configurationWrapper objects for initial behavior.
//     the cluster operators have an option to edit or delete these configurationWrapper objects.
type EnsureStrategy[ObjectType configurationObjectType] interface {
	// Name of the strategy, for now we have two: 'mandatory' and 'suggested'.
	// This comes handy in logging.
	Name() string

	// ReviseIfNeeded accepts a pair of the current and the bootstrap configuration, determines
	// whether an update is necessary, and returns a (revised if appropriate) copy of the object.
	// current is the existing in-cluster configuration object.
	// bootstrap is the configuration the kube-apiserver maintains in-memory.
	//
	// revised: the new object represents the new configuration to be stored in-cluster.
	// ok: true if auto update is required, otherwise false
	// err: err is set when the function runs into an error and can not
	//      determine if auto update is needed.
	ReviseIfNeeded(objectOps objectLocalOps[ObjectType], current, bootstrap ObjectType) (revised ObjectType, ok bool, err error)
}

// objectLocalOps is the needed operations on an individual configurationObject
type objectLocalOps[ObjectType configurationObject] interface {
	DeepCopy(ObjectType) ObjectType

	// replaceSpec returns a deep copy of `into` except that the spec is a deep copy of `from`
	ReplaceSpec(into, from ObjectType) ObjectType

	// SpecEqualish says whether applying defaulting to `expected`
	// makes its spec more or less equal (as appropriate for the
	// object at hand) that of `actual`.
	SpecEqualish(expected, actual ObjectType) bool
}

// ObjectOps is the needed operations, both as a receiver from a server and server-independent, on configurationObjects
type ObjectOps[ObjectType configurationObject] interface {
	client[ObjectType]
	cache[ObjectType]
	objectLocalOps[ObjectType]
}

// Client is the needed fragment of the typed generated client stubs for the given object type
type client[ObjectType configurationObject] interface {
	Create(ctx context.Context, obj ObjectType, opts metav1.CreateOptions) (ObjectType, error)
	Update(ctx context.Context, obj ObjectType, opts metav1.UpdateOptions) (ObjectType, error)
	Delete(ctx context.Context, name string, opts metav1.DeleteOptions) error
}

// cache is the needed fragment of the typed generated access ("lister") to an informer's local cache
type cache[ObjectType configurationObject] interface {
	List(labels.Selector) ([]ObjectType, error)
	Get(name string) (ObjectType, error)
}

// configurationObject is the relevant interfaces that each API object type implements
type configurationObject interface {
	metav1.Object
	runtime.Object
}

// configurationObjectType adds the type constraint `comparable` and is thus
// only usable as a type constraint.
type configurationObjectType interface {
	comparable
	configurationObject
}

type objectOps[ObjectType configurationObjectType] struct {
	client[ObjectType]
	cache[ObjectType]
	deepCopy     func(ObjectType) ObjectType
	replaceSpec  func(ObjectType, ObjectType) ObjectType
	specEqualish func(expected, actual ObjectType) bool
}

func NewObjectOps[ObjectType configurationObjectType](client client[ObjectType], cache cache[ObjectType],
	deepCopy func(ObjectType) ObjectType,
	replaceSpec func(ObjectType, ObjectType) ObjectType,
	specEqualish func(expected, actual ObjectType) bool,
) ObjectOps[ObjectType] {
	return objectOps[ObjectType]{client: client,
		cache:        cache,
		deepCopy:     deepCopy,
		replaceSpec:  replaceSpec,
		specEqualish: specEqualish}
}

func (oo objectOps[ObjectType]) DeepCopy(obj ObjectType) ObjectType { return oo.deepCopy(obj) }

func (oo objectOps[ObjectType]) ReplaceSpec(into, from ObjectType) ObjectType {
	return oo.replaceSpec(into, from)
}

func (oo objectOps[ObjectType]) SpecEqualish(expected, actual ObjectType) bool {
	return oo.specEqualish(expected, actual)
}

// NewSuggestedEnsureStrategy returns an EnsureStrategy for suggested config objects
func NewSuggestedEnsureStrategy[ObjectType configurationObjectType]() EnsureStrategy[ObjectType] {
	return &strategy[ObjectType]{
		alwaysAutoUpdateSpec: false,
		name:                 "suggested",
	}
}

// NewMandatoryEnsureStrategy returns an EnsureStrategy for mandatory config objects
func NewMandatoryEnsureStrategy[ObjectType configurationObjectType]() EnsureStrategy[ObjectType] {
	return &strategy[ObjectType]{
		alwaysAutoUpdateSpec: true,
		name:                 "mandatory",
	}
}

// auto-update strategy for the configuration objects
type strategy[ObjectType configurationObjectType] struct {
	alwaysAutoUpdateSpec bool
	name                 string
}

func (s *strategy[ObjectType]) Name() string {
	return s.name
}

func (s *strategy[ObjectType]) ReviseIfNeeded(objectOps objectLocalOps[ObjectType], current, bootstrap ObjectType) (ObjectType, bool, error) {
	var zero ObjectType
	if current == zero {
		return zero, false, nil
	}

	autoUpdateSpec := s.alwaysAutoUpdateSpec
	if !autoUpdateSpec {
		autoUpdateSpec = shouldUpdateSpec(current)
	}
	updateAnnotation := shouldUpdateAnnotation(current, autoUpdateSpec)

	specChanged := autoUpdateSpec && !objectOps.SpecEqualish(bootstrap, current)

	if !(updateAnnotation || specChanged) {
		// the annotation key is up to date and the spec has not changed, no update is necessary
		return zero, false, nil
	}

	var revised ObjectType
	if specChanged {
		revised = objectOps.ReplaceSpec(current, bootstrap)
	} else {
		revised = objectOps.DeepCopy(current)
	}
	if updateAnnotation {
		setAutoUpdateAnnotation(revised, autoUpdateSpec)
	}

	return revised, true, nil
}

// shouldUpdateSpec inspects the auto-update annotation key and generation field to determine
// whether the config object should be auto-updated.
func shouldUpdateSpec(accessor metav1.Object) bool {
	value := accessor.GetAnnotations()[flowcontrolv1.AutoUpdateAnnotationKey]
	if autoUpdate, err := strconv.ParseBool(value); err == nil {
		return autoUpdate
	}

	// We are here because of either a or b:
	// a. the annotation key is missing.
	// b. the annotation key is present but the value does not represent a boolean.
	// In either case, if the operator hasn't changed the spec, we can safely auto update.
	// Please note that we can't protect the changes made by the operator in the following scenario:
	// - The operator deletes and recreates the same object with a variant spec (generation resets to 1).
	if accessor.GetGeneration() == 1 {
		return true
	}
	return false
}

// shouldUpdateAnnotation determines whether the current value of the auto-update annotation
// key matches the desired value.
func shouldUpdateAnnotation(accessor metav1.Object, desired bool) bool {
	if value, ok := accessor.GetAnnotations()[flowcontrolv1.AutoUpdateAnnotationKey]; ok {
		if current, err := strconv.ParseBool(value); err == nil && current == desired {
			return false
		}
	}

	return true
}

// setAutoUpdateAnnotation sets the auto-update annotation key to the specified value.
func setAutoUpdateAnnotation(accessor metav1.Object, autoUpdate bool) {
	if accessor.GetAnnotations() == nil {
		accessor.SetAnnotations(map[string]string{})
	}

	accessor.GetAnnotations()[flowcontrolv1.AutoUpdateAnnotationKey] = strconv.FormatBool(autoUpdate)
}

// EnsureConfigurations applies the given maintenance strategy to the given objects.
// At the first error, if any, it stops and returns that error.
func EnsureConfigurations[ObjectType configurationObjectType](ctx context.Context, ops ObjectOps[ObjectType], boots []ObjectType, strategy EnsureStrategy[ObjectType]) error {
	for _, bo := range boots {
		err := EnsureConfiguration(ctx, ops, bo, strategy)
		if err != nil {
			return err
		}
	}
	return nil
}

// EnsureConfiguration applies the given maintenance strategy to the given object.
func EnsureConfiguration[ObjectType configurationObjectType](ctx context.Context, ops ObjectOps[ObjectType], bootstrap ObjectType, strategy EnsureStrategy[ObjectType]) error {
	name := bootstrap.GetName()
	configurationType := strategy.Name()

	logger := klog.FromContext(ctx).WithValues("type", configurationType, "name", bootstrap.GetName())

	var current ObjectType
	var err error
	for {
		current, err = ops.Get(name)
		if err == nil {
			break
		}
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("failed to retrieve %T type=%s name=%q error=%w", bootstrap, configurationType, name, err)
		}

		// we always re-create a missing configuration object
		if _, err = ops.Create(ctx, ops.DeepCopy(bootstrap), metav1.CreateOptions{FieldManager: fieldManager}); err == nil {
			logger.V(2).Info(fmt.Sprintf("Successfully created %T", bootstrap))
			return nil
		}

		if !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("cannot create %T type=%s name=%q error=%w", bootstrap, configurationType, name, err)
		}
		logger.V(5).Info(fmt.Sprintf("Something created the %T concurrently", bootstrap))
	}

	newObject, update, err := strategy.ReviseIfNeeded(ops, current, bootstrap)
	if err != nil {
		return fmt.Errorf("failed to determine whether auto-update is required for %T type=%s name=%q error=%w", bootstrap, configurationType, name, err)
	}
	if !update {
		return nil
	}

	if updated, err := ops.Update(ctx, newObject, metav1.UpdateOptions{FieldManager: fieldManager}); err == nil {
		if loggerV := logger.V(3); loggerV.Enabled() {
			loggerV.Info(fmt.Sprintf("Updated the %T", bootstrap), "diff", cmp.Diff(current, updated))
		} else {
			logger.V(2).Info(fmt.Sprintf("Updated the %T", bootstrap), "old", current, "updated", updated)
		}
		return nil
	} else if apierrors.IsConflict(err) {
		logger.V(2).Info(fmt.Sprintf("Something updated the %T concurrently, I will check its spec later", bootstrap))
		return nil
	}

	return fmt.Errorf("failed to update the %T, will retry later type=%s name=%q error=%w", bootstrap, configurationType, name, err)
}

// RemoveUnwantedObjects attempts to delete the configuration objects
// that exist, are annotated `apf.kubernetes.io/autoupdate-spec=true`, and do not
// have a name in the given set.  A refusal due to concurrent update is logged
// and not considered an error; the object will be reconsidered later.
func RemoveUnwantedObjects[ObjectType configurationObjectType](ctx context.Context, objectOps ObjectOps[ObjectType], boots []ObjectType) error {
	logger := klog.FromContext(ctx)

	current, err := objectOps.List(labels.Everything())
	if err != nil {
		return err
	}
	wantedNames := namesOfBootstrapObjects(boots)
	for _, object := range current {
		name := object.GetName()
		if wantedNames.Has(name) {
			continue
		}
		var value string
		var ok, autoUpdate bool
		var err error
		if value, ok = object.GetAnnotations()[flowcontrolv1.AutoUpdateAnnotationKey]; !ok {
			// the configuration object does not have the annotation key,
			// it's probably a user defined configuration object,
			// so we can skip it.
			logger.V(5).Info("Skipping deletion of APF object with no "+flowcontrolv1.AutoUpdateAnnotationKey+" annotation", "name", name)
			continue
		}
		autoUpdate, err = strconv.ParseBool(value)
		if err != nil {
			// Log this because it is not an expected situation.
			logger.V(4).Info("Skipping deletion of APF object with malformed "+flowcontrolv1.AutoUpdateAnnotationKey+" annotation", "name", name, "annotationValue", value, "parseError", err)
			continue
		}
		if !autoUpdate {
			logger.V(5).Info("Skipping deletion of APF object with "+flowcontrolv1.AutoUpdateAnnotationKey+"=false annotation", "name", name)
			continue
		}
		// TODO: expectedResourceVersion := object.GetResourceVersion()
		err = objectOps.Delete(ctx, object.GetName(), metav1.DeleteOptions{ /* TODO: expectedResourceVersion */ })
		if err == nil {
			logger.V(2).Info("Successfully deleted the unwanted object", "kind", object.GetObjectKind().GroupVersionKind().Kind, "name", name)
			continue
		}
		if apierrors.IsNotFound(err) {
			logger.V(5).Info("Unwanted APF object was concurrently deleted", "name", name)
		} else {
			return fmt.Errorf("failed to delete unwatned APF object %q - %w", name, err)
		}
	}
	return nil
}

func namesOfBootstrapObjects[ObjectType configurationObjectType](bos []ObjectType) sets.String {
	names := sets.NewString()
	for _, bo := range bos {
		names.Insert(bo.GetName())
	}
	return names
}
