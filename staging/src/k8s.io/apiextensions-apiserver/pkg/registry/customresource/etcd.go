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

	autoscalingv1 "k8s.io/api/autoscaling/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/managedfields"
	"k8s.io/apiserver/pkg/registry/generic"
	genericregistry "k8s.io/apiserver/pkg/registry/generic/registry"
	"k8s.io/apiserver/pkg/registry/rest"
	"sigs.k8s.io/structured-merge-diff/v4/fieldpath"
)

// CustomResourceStorage includes dummy storage for CustomResources, and their Status and Scale subresources.
type CustomResourceStorage struct {
	CustomResource *REST
	Status         *StatusREST
	Scale          *ScaleREST
}

func NewStorage(resource schema.GroupResource, singularResource schema.GroupResource, kind, listKind schema.GroupVersionKind, strategy customResourceStrategy, optsGetter generic.RESTOptionsGetter, categories []string, tableConvertor rest.TableConvertor, replicasPathMapping managedfields.ResourcePathMappings) CustomResourceStorage {
	var storage CustomResourceStorage
	store := &genericregistry.Store{
		NewFunc: func() runtime.Object {
			// set the expected group/version/kind in the new object as a signal to the versioning decoder
			ret := &unstructured.Unstructured{}
			ret.SetGroupVersionKind(kind)
			return ret
		},
		NewListFunc: func() runtime.Object {
			// lists are never stored, only manufactured, so stomp in the right kind
			ret := &unstructured.UnstructuredList{}
			ret.SetGroupVersionKind(listKind)
			return ret
		},
		PredicateFunc:             strategy.MatchCustomResourceDefinitionStorage,
		DefaultQualifiedResource:  resource,
		SingularQualifiedResource: singularResource,

		CreateStrategy:      strategy,
		UpdateStrategy:      strategy,
		DeleteStrategy:      strategy,
		ResetFieldsStrategy: strategy,

		TableConvertor: tableConvertor,
	}
	options := &generic.StoreOptions{RESTOptions: optsGetter, AttrFunc: strategy.GetAttrs}
	if err := store.CompleteWithOptions(options); err != nil {
		panic(err) // TODO: Propagate error up
	}
	storage.CustomResource = &REST{store, categories}

	if strategy.status != nil {
		statusStore := *store
		statusStrategy := NewStatusStrategy(strategy)
		statusStore.UpdateStrategy = statusStrategy
		statusStore.ResetFieldsStrategy = statusStrategy
		storage.Status = &StatusREST{store: &statusStore}
	}

	if scale := strategy.scale; scale != nil {
		var labelSelectorPath string
		if scale.LabelSelectorPath != nil {
			labelSelectorPath = *scale.LabelSelectorPath
		}

		storage.Scale = &ScaleREST{
			store:               store,
			specReplicasPath:    scale.SpecReplicasPath,
			statusReplicasPath:  scale.StatusReplicasPath,
			labelSelectorPath:   labelSelectorPath,
			parentGV:            kind.GroupVersion(),
			replicasPathMapping: replicasPathMapping,
		}
	}

	return storage
}

// REST implements a RESTStorage for API services against etcd
type REST struct {
	*genericregistry.Store
	categories []string
}

// Implement CategoriesProvider
var _ rest.CategoriesProvider = &REST{}

// Categories implements the CategoriesProvider interface. Returns a list of categories a resource is part of.
func (r *REST) Categories() []string {
	return r.categories
}

// StatusREST implements the REST endpoint for changing the status of a CustomResource
type StatusREST struct {
	store *genericregistry.Store
}

var _ = rest.Patcher(&StatusREST{})

func (r *StatusREST) New() runtime.Object {
	return r.store.New()
}

// Get retrieves the object from the storage. It is required to support Patch.
func (r *StatusREST) Get(ctx context.Context, name string, options *metav1.GetOptions) (runtime.Object, error) {
	o, err := r.store.Get(ctx, name, options)
	if err != nil {
		return nil, err
	}
	return o, nil
}

// Update alters the status subset of an object.
func (r *StatusREST) Update(ctx context.Context, name string, objInfo rest.UpdatedObjectInfo, createValidation rest.ValidateObjectFunc, updateValidation rest.ValidateObjectUpdateFunc, forceAllowCreate bool, options *metav1.UpdateOptions) (runtime.Object, bool, error) {
	// We are explicitly setting forceAllowCreate to false in the call to the underlying storage because
	// subresources should never allow create on update.
	return r.store.Update(ctx, name, objInfo, createValidation, updateValidation, false, options)
}

// GetResetFields implements rest.ResetFieldsStrategy
func (r *StatusREST) GetResetFields() map[fieldpath.APIVersion]*fieldpath.Set {
	return r.store.GetResetFields()
}

type ScaleREST struct {
	store               *genericregistry.Store
	specReplicasPath    string
	statusReplicasPath  string
	labelSelectorPath   string
	parentGV            schema.GroupVersion
	replicasPathMapping managedfields.ResourcePathMappings
}

// ScaleREST implements Patcher
var _ = rest.Patcher(&ScaleREST{})
var _ = rest.GroupVersionKindProvider(&ScaleREST{})

func (r *ScaleREST) GroupVersionKind(containingGV schema.GroupVersion) schema.GroupVersionKind {
	return autoscalingv1.SchemeGroupVersion.WithKind("Scale")
}

// New creates a new Scale object
func (r *ScaleREST) New() runtime.Object {
	return &autoscalingv1.Scale{}
}

func (r *ScaleREST) Get(ctx context.Context, name string, options *metav1.GetOptions) (runtime.Object, error) {
	obj, err := r.store.Get(ctx, name, options)
	if err != nil {
		return nil, err
	}
	cr := obj.(*unstructured.Unstructured)

	scaleObject, replicasFound, err := scaleFromCustomResource(cr, r.specReplicasPath, r.statusReplicasPath, r.labelSelectorPath)
	if err != nil {
		return nil, err
	}
	if !replicasFound {
		return nil, apierrors.NewInternalError(fmt.Errorf("the spec replicas field %q does not exist", r.specReplicasPath))
	}
	return scaleObject, err
}

func (r *ScaleREST) Update(ctx context.Context, name string, objInfo rest.UpdatedObjectInfo, createValidation rest.ValidateObjectFunc, updateValidation rest.ValidateObjectUpdateFunc, forceAllowCreate bool, options *metav1.UpdateOptions) (runtime.Object, bool, error) {
	scaleObjInfo := &scaleUpdatedObjectInfo{
		reqObjInfo:          objInfo,
		specReplicasPath:    r.specReplicasPath,
		labelSelectorPath:   r.labelSelectorPath,
		statusReplicasPath:  r.statusReplicasPath,
		parentGV:            r.parentGV,
		replicasPathMapping: r.replicasPathMapping,
	}

	obj, _, err := r.store.Update(
		ctx,
		name,
		scaleObjInfo,
		toScaleCreateValidation(createValidation, r.specReplicasPath, r.statusReplicasPath, r.labelSelectorPath),
		toScaleUpdateValidation(updateValidation, r.specReplicasPath, r.statusReplicasPath, r.labelSelectorPath),
		false,
		options,
	)
	if err != nil {
		return nil, false, err
	}
	cr := obj.(*unstructured.Unstructured)

	newScale, _, err := scaleFromCustomResource(cr, r.specReplicasPath, r.statusReplicasPath, r.labelSelectorPath)
	if err != nil {
		return nil, false, apierrors.NewBadRequest(err.Error())
	}

	return newScale, false, err
}

func toScaleCreateValidation(f rest.ValidateObjectFunc, specReplicasPath, statusReplicasPath, labelSelectorPath string) rest.ValidateObjectFunc {
	return func(ctx context.Context, obj runtime.Object) error {
		scale, _, err := scaleFromCustomResource(obj.(*unstructured.Unstructured), specReplicasPath, statusReplicasPath, labelSelectorPath)
		if err != nil {
			return err
		}
		return f(ctx, scale)
	}
}

func toScaleUpdateValidation(f rest.ValidateObjectUpdateFunc, specReplicasPath, statusReplicasPath, labelSelectorPath string) rest.ValidateObjectUpdateFunc {
	return func(ctx context.Context, obj, old runtime.Object) error {
		newScale, _, err := scaleFromCustomResource(obj.(*unstructured.Unstructured), specReplicasPath, statusReplicasPath, labelSelectorPath)
		if err != nil {
			return err
		}
		oldScale, _, err := scaleFromCustomResource(old.(*unstructured.Unstructured), specReplicasPath, statusReplicasPath, labelSelectorPath)
		if err != nil {
			return err
		}
		return f(ctx, newScale, oldScale)
	}
}

// Split the path per period, ignoring the leading period.
func splitReplicasPath(replicasPath string) []string {
	return strings.Split(strings.TrimPrefix(replicasPath, "."), ".")
}

// scaleFromCustomResource returns a scale subresource for a customresource and a bool signalling whether
// the specReplicas value was found.
func scaleFromCustomResource(cr *unstructured.Unstructured, specReplicasPath, statusReplicasPath, labelSelectorPath string) (*autoscalingv1.Scale, bool, error) {
	specReplicas, foundSpecReplicas, err := unstructured.NestedInt64(cr.UnstructuredContent(), splitReplicasPath(specReplicasPath)...)
	if err != nil {
		return nil, false, err
	} else if !foundSpecReplicas {
		specReplicas = 0
	}

	statusReplicas, found, err := unstructured.NestedInt64(cr.UnstructuredContent(), splitReplicasPath(statusReplicasPath)...)
	if err != nil {
		return nil, false, err
	} else if !found {
		statusReplicas = 0
	}

	var labelSelector string
	if len(labelSelectorPath) > 0 {
		labelSelector, _, err = unstructured.NestedString(cr.UnstructuredContent(), splitReplicasPath(labelSelectorPath)...)
		if err != nil {
			return nil, false, err
		}
	}

	scale := &autoscalingv1.Scale{
		// Populate apiVersion and kind so conversion recognizes we are already in the desired GVK and doesn't try to convert
		TypeMeta: metav1.TypeMeta{
			APIVersion: "autoscaling/v1",
			Kind:       "Scale",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:              cr.GetName(),
			Namespace:         cr.GetNamespace(),
			UID:               cr.GetUID(),
			ResourceVersion:   cr.GetResourceVersion(),
			CreationTimestamp: cr.GetCreationTimestamp(),
		},
		Spec: autoscalingv1.ScaleSpec{
			Replicas: int32(specReplicas),
		},
		Status: autoscalingv1.ScaleStatus{
			Replicas: int32(statusReplicas),
			Selector: labelSelector,
		},
	}

	return scale, foundSpecReplicas, nil
}

type scaleUpdatedObjectInfo struct {
	reqObjInfo          rest.UpdatedObjectInfo
	specReplicasPath    string
	statusReplicasPath  string
	labelSelectorPath   string
	parentGV            schema.GroupVersion
	replicasPathMapping managedfields.ResourcePathMappings
}

func (i *scaleUpdatedObjectInfo) Preconditions() *metav1.Preconditions {
	return i.reqObjInfo.Preconditions()
}

func (i *scaleUpdatedObjectInfo) UpdatedObject(ctx context.Context, oldObj runtime.Object) (runtime.Object, error) {
	cr := oldObj.DeepCopyObject().(*unstructured.Unstructured)
	const invalidSpecReplicas = -2147483648 // smallest int32

	managedFieldsHandler := managedfields.NewScaleHandler(
		cr.GetManagedFields(),
		i.parentGV,
		i.replicasPathMapping,
	)

	oldScale, replicasFound, err := scaleFromCustomResource(cr, i.specReplicasPath, i.statusReplicasPath, i.labelSelectorPath)
	if err != nil {
		return nil, err
	}
	if !replicasFound {
		oldScale.Spec.Replicas = invalidSpecReplicas // signal that this was not set before
	}

	scaleManagedFields, err := managedFieldsHandler.ToSubresource()
	if err != nil {
		return nil, err
	}
	oldScale.ManagedFields = scaleManagedFields

	obj, err := i.reqObjInfo.UpdatedObject(ctx, oldScale)
	if err != nil {
		return nil, err
	}
	if obj == nil {
		return nil, apierrors.NewBadRequest(fmt.Sprintf("nil update passed to Scale"))
	}

	scale, ok := obj.(*autoscalingv1.Scale)
	if !ok {
		return nil, apierrors.NewBadRequest(fmt.Sprintf("wrong object passed to Scale update: %v", obj))
	}

	if scale.Spec.Replicas == invalidSpecReplicas {
		return nil, apierrors.NewBadRequest(fmt.Sprintf("the spec replicas field %q cannot be empty", i.specReplicasPath))
	}

	if err := unstructured.SetNestedField(cr.Object, int64(scale.Spec.Replicas), splitReplicasPath(i.specReplicasPath)...); err != nil {
		return nil, err
	}
	if len(scale.ResourceVersion) != 0 {
		// The client provided a resourceVersion precondition.
		// Set that precondition and return any conflict errors to the client.
		cr.SetResourceVersion(scale.ResourceVersion)
	}

	updatedEntries, err := managedFieldsHandler.ToParent(scale.ManagedFields)
	if err != nil {
		return nil, err
	}
	cr.SetManagedFields(updatedEntries)

	return cr, nil
}
