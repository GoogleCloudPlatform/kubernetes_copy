/*
Copyright 2014 The Kubernetes Authors.

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

package storage

import (
	"context"
	"fmt"
	"net/http"
	"net/url"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apiserver/pkg/registry/generic"
	genericregistry "k8s.io/apiserver/pkg/registry/generic/registry"
	"k8s.io/apiserver/pkg/registry/rest"
	"k8s.io/apiserver/pkg/storage"
	storeerr "k8s.io/apiserver/pkg/storage/errors"
	"k8s.io/apiserver/pkg/util/dryrun"
	policyclient "k8s.io/client-go/kubernetes/typed/policy/v1"
	api "k8s.io/kubernetes/pkg/apis/core"
	"k8s.io/kubernetes/pkg/apis/core/validation"
	"k8s.io/kubernetes/pkg/kubelet/client"
	"k8s.io/kubernetes/pkg/printers"
	printersinternal "k8s.io/kubernetes/pkg/printers/internalversion"
	printerstorage "k8s.io/kubernetes/pkg/printers/storage"
	registrypod "k8s.io/kubernetes/pkg/registry/core/pod"
	podrest "k8s.io/kubernetes/pkg/registry/core/pod/rest"
	"sigs.k8s.io/structured-merge-diff/v4/fieldpath"
)

// PodStorage includes storage for pods and all sub resources
type PodStorage struct {
	Pod                 *REST
	Binding             *BindingREST
	LegacyBinding       *LegacyBindingREST
	Eviction            *EvictionREST
	Status              *StatusREST
	EphemeralContainers *EphemeralContainersREST
	Resize              *ResizeREST
	Log                 *podrest.LogREST
	Proxy               *podrest.ProxyREST
	Exec                *podrest.ExecREST
	Attach              *podrest.AttachREST
	PortForward         *podrest.PortForwardREST
}

// REST implements a RESTStorage for pods
type REST struct {
	*genericregistry.Store
	proxyTransport http.RoundTripper
}

// NewStorage returns a RESTStorage object that will work against pods.
func NewStorage(optsGetter generic.RESTOptionsGetter, k client.ConnectionInfoGetter, proxyTransport http.RoundTripper, podDisruptionBudgetClient policyclient.PodDisruptionBudgetsGetter) (PodStorage, error) {

	store := &genericregistry.Store{
		NewFunc:                   func() runtime.Object { return &api.Pod{} },
		NewListFunc:               func() runtime.Object { return &api.PodList{} },
		PredicateFunc:             registrypod.MatchPod,
		DefaultQualifiedResource:  api.Resource("pods"),
		SingularQualifiedResource: api.Resource("pod"),

		CreateStrategy:      registrypod.Strategy,
		UpdateStrategy:      registrypod.Strategy,
		DeleteStrategy:      registrypod.Strategy,
		ResetFieldsStrategy: registrypod.Strategy,
		ReturnDeletedObject: true,

		TableConvertor: printerstorage.TableConvertor{TableGenerator: printers.NewTableGenerator().With(printersinternal.AddHandlers)},
	}
	options := &generic.StoreOptions{
		RESTOptions: optsGetter,
		AttrFunc:    registrypod.GetAttrs,
		TriggerFunc: map[string]storage.IndexerFunc{"spec.nodeName": registrypod.NodeNameTriggerFunc},
		Indexers:    registrypod.Indexers(),
	}
	if err := store.CompleteWithOptions(options); err != nil {
		return PodStorage{}, err
	}

	statusStore := *store
	statusStore.UpdateStrategy = registrypod.StatusStrategy
	statusStore.ResetFieldsStrategy = registrypod.StatusStrategy
	ephemeralContainersStore := *store
	ephemeralContainersStore.UpdateStrategy = registrypod.EphemeralContainersStrategy
	resizeStore := *store
	resizeStore.UpdateStrategy = registrypod.ResizeStrategy

	bindingREST := &BindingREST{store: store}
	return PodStorage{
		Pod:                 &REST{store, proxyTransport},
		Binding:             &BindingREST{store: store},
		LegacyBinding:       &LegacyBindingREST{bindingREST},
		Eviction:            newEvictionStorage(&statusStore, podDisruptionBudgetClient),
		Status:              &StatusREST{store: &statusStore},
		EphemeralContainers: &EphemeralContainersREST{store: &ephemeralContainersStore},
		Resize:              &ResizeREST{store: &resizeStore},
		Log:                 &podrest.LogREST{Store: store, KubeletConn: k},
		Proxy:               &podrest.ProxyREST{Store: store, ProxyTransport: proxyTransport},
		Exec:                &podrest.ExecREST{Store: store, KubeletConn: k},
		Attach:              &podrest.AttachREST{Store: store, KubeletConn: k},
		PortForward:         &podrest.PortForwardREST{Store: store, KubeletConn: k},
	}, nil
}

// Implement Redirector.
var _ = rest.Redirector(&REST{})

// ResourceLocation returns a pods location from its HostIP
func (r *REST) ResourceLocation(ctx context.Context, name string) (*url.URL, http.RoundTripper, error) {
	return registrypod.ResourceLocation(ctx, r, r.proxyTransport, name)
}

// Implement ShortNamesProvider
var _ rest.ShortNamesProvider = &REST{}

// ShortNames implements the ShortNamesProvider interface. Returns a list of short names for a resource.
func (r *REST) ShortNames() []string {
	return []string{"po"}
}

// Implement CategoriesProvider
var _ rest.CategoriesProvider = &REST{}

// Categories implements the CategoriesProvider interface. Returns a list of categories a resource is part of.
func (r *REST) Categories() []string {
	return []string{"all"}
}

// BindingREST implements the REST endpoint for binding pods to nodes when etcd is in use.
type BindingREST struct {
	store *genericregistry.Store
}

// NamespaceScoped fulfill rest.Scoper
func (r *BindingREST) NamespaceScoped() bool {
	return r.store.NamespaceScoped()
}

// New creates a new binding resource
func (r *BindingREST) New() runtime.Object {
	return &api.Binding{}
}

// Destroy cleans up resources on shutdown.
func (r *BindingREST) Destroy() {
	// Given that underlying store is shared with REST,
	// we don't destroy it here explicitly.
}

var _ = rest.NamedCreater(&BindingREST{})
var _ = rest.SubresourceObjectMetaPreserver(&BindingREST{})

// Create ensures a pod is bound to a specific host.
func (r *BindingREST) Create(ctx context.Context, name string, obj runtime.Object, createValidation rest.ValidateObjectFunc, options *metav1.CreateOptions) (out runtime.Object, err error) {
	binding, ok := obj.(*api.Binding)
	if !ok {
		return nil, errors.NewBadRequest(fmt.Sprintf("not a Binding object: %#v", obj))
	}

	if name != binding.Name {
		return nil, errors.NewBadRequest("name in URL does not match name in Binding object")
	}

	// TODO: move me to a binding strategy
	if errs := validation.ValidatePodBinding(binding); len(errs) != 0 {
		return nil, errs.ToAggregate()
	}

	if createValidation != nil {
		if err := createValidation(ctx, binding.DeepCopyObject()); err != nil {
			return nil, err
		}
	}

	err = r.assignPod(ctx, binding.UID, binding.ResourceVersion, binding.Name, binding.Target.Name, binding.Annotations, dryrun.IsDryRun(options.DryRun))
	out = &metav1.Status{Status: metav1.StatusSuccess}
	return
}

// PreserveRequestObjectMetaSystemFieldsOnSubresourceCreate indicates to a
// handler that this endpoint requires the UID and ResourceVersion to use as
// preconditions. Other fields, such as timestamp, are ignored.
func (r *BindingREST) PreserveRequestObjectMetaSystemFieldsOnSubresourceCreate() bool {
	return true
}

// setPodHostAndAnnotations sets the given pod's host to 'machine' if and only if
// the pod is unassigned and merges the provided annotations with those of the pod.
// Returns the current state of the pod, or an error.
func (r *BindingREST) setPodHostAndAnnotations(ctx context.Context, podUID types.UID, podResourceVersion, podID, machine string, annotations map[string]string, dryRun bool) (finalPod *api.Pod, err error) {
	podKey, err := r.store.KeyFunc(ctx, podID)
	if err != nil {
		return nil, err
	}

	var preconditions *storage.Preconditions
	if podUID != "" || podResourceVersion != "" {
		preconditions = &storage.Preconditions{}
		if podUID != "" {
			preconditions.UID = &podUID
		}
		if podResourceVersion != "" {
			preconditions.ResourceVersion = &podResourceVersion
		}
	}

	err = r.store.Storage.GuaranteedUpdate(ctx, podKey, &api.Pod{}, false, preconditions, storage.SimpleUpdate(func(obj runtime.Object) (runtime.Object, error) {
		pod, ok := obj.(*api.Pod)
		if !ok {
			return nil, fmt.Errorf("unexpected object: %#v", obj)
		}
		if pod.DeletionTimestamp != nil {
			return nil, fmt.Errorf("pod %s is being deleted, cannot be assigned to a host", pod.Name)
		}
		if pod.Spec.NodeName != "" {
			return nil, fmt.Errorf("pod %v is already assigned to node %q", pod.Name, pod.Spec.NodeName)
		}
		// Reject binding to a scheduling un-ready Pod.
		if len(pod.Spec.SchedulingGates) != 0 {
			return nil, fmt.Errorf("pod %v has non-empty .spec.schedulingGates", pod.Name)
		}
		pod.Spec.NodeName = machine
		if pod.Annotations == nil {
			pod.Annotations = make(map[string]string)
		}
		for k, v := range annotations {
			pod.Annotations[k] = v
		}
		finalPod = pod
		return pod, nil
	}), dryRun, nil)
	return finalPod, err
}

// assignPod assigns the given pod to the given machine.
func (r *BindingREST) assignPod(ctx context.Context, podUID types.UID, podResourceVersion, podID string, machine string, annotations map[string]string, dryRun bool) (err error) {
	if _, err = r.setPodHostAndAnnotations(ctx, podUID, podResourceVersion, podID, machine, annotations, dryRun); err != nil {
		err = storeerr.InterpretGetError(err, api.Resource("pods"), podID)
		err = storeerr.InterpretUpdateError(err, api.Resource("pods"), podID)
		if _, ok := err.(*errors.StatusError); !ok {
			err = errors.NewConflict(api.Resource("pods/binding"), podID, err)
		}
	}
	return
}

var _ = rest.Creater(&LegacyBindingREST{})

// LegacyBindingREST implements the REST endpoint for binding pods to nodes when etcd is in use.
type LegacyBindingREST struct {
	bindingRest *BindingREST
}

// NamespaceScoped fulfill rest.Scoper
func (r *LegacyBindingREST) NamespaceScoped() bool {
	return r.bindingRest.NamespaceScoped()
}

// New creates a new binding resource
func (r *LegacyBindingREST) New() runtime.Object {
	return r.bindingRest.New()
}

// Destroy cleans up resources on shutdown.
func (r *LegacyBindingREST) Destroy() {
	// Given that underlying store is shared with REST,
	// we don't destroy it here explicitly.
}

// Create ensures a pod is bound to a specific host.
func (r *LegacyBindingREST) Create(ctx context.Context, obj runtime.Object, createValidation rest.ValidateObjectFunc, options *metav1.CreateOptions) (out runtime.Object, err error) {
	metadata, err := meta.Accessor(obj)
	if err != nil {
		return nil, errors.NewBadRequest(fmt.Sprintf("not a Binding object: %T", obj))
	}
	return r.bindingRest.Create(ctx, metadata.GetName(), obj, createValidation, options)
}

func (r *LegacyBindingREST) GetSingularName() string {
	return "binding"
}

// StatusREST implements the REST endpoint for changing the status of a pod.
type StatusREST struct {
	store *genericregistry.Store
}

// New creates a new pod resource
func (r *StatusREST) New() runtime.Object {
	return &api.Pod{}
}

// Destroy cleans up resources on shutdown.
func (r *StatusREST) Destroy() {
	// Given that underlying store is shared with REST,
	// we don't destroy it here explicitly.
}

// Get retrieves the object from the storage. It is required to support Patch.
func (r *StatusREST) Get(ctx context.Context, name string, options *metav1.GetOptions) (runtime.Object, error) {
	return r.store.Get(ctx, name, options)
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

func (r *StatusREST) ConvertToTable(ctx context.Context, object runtime.Object, tableOptions runtime.Object) (*metav1.Table, error) {
	return r.store.ConvertToTable(ctx, object, tableOptions)
}

// EphemeralContainersREST implements the REST endpoint for adding EphemeralContainers
type EphemeralContainersREST struct {
	store *genericregistry.Store
}

var _ = rest.Patcher(&EphemeralContainersREST{})

// Get retrieves the object from the storage. It is required to support Patch.
func (r *EphemeralContainersREST) Get(ctx context.Context, name string, options *metav1.GetOptions) (runtime.Object, error) {
	return r.store.Get(ctx, name, options)
}

// New creates a new pod resource
func (r *EphemeralContainersREST) New() runtime.Object {
	return &api.Pod{}
}

// Destroy cleans up resources on shutdown.
func (r *EphemeralContainersREST) Destroy() {
	// Given that underlying store is shared with REST,
	// we don't destroy it here explicitly.
}

// Update alters the EphemeralContainers field in PodSpec
func (r *EphemeralContainersREST) Update(ctx context.Context, name string, objInfo rest.UpdatedObjectInfo, createValidation rest.ValidateObjectFunc, updateValidation rest.ValidateObjectUpdateFunc, forceAllowCreate bool, options *metav1.UpdateOptions) (runtime.Object, bool, error) {
	// We are explicitly setting forceAllowCreate to false in the call to the underlying storage because
	// subresources should never allow create on update.
	return r.store.Update(ctx, name, objInfo, createValidation, updateValidation, false, options)
}

// ResizeREST implements the REST endpoint for resizing Pod containers.
type ResizeREST struct {
	store *genericregistry.Store
}

var _ = rest.Patcher(&ResizeREST{})

// Get retrieves the object from the storage. It is required to support Patch.
func (r *ResizeREST) Get(ctx context.Context, name string, options *metav1.GetOptions) (runtime.Object, error) {
	return r.store.Get(ctx, name, options)
}

// New creates a new pod resource
func (r *ResizeREST) New() runtime.Object {
	return &api.Pod{}
}

// Destroy cleans up resources on shutdown.
func (r *ResizeREST) Destroy() {
	// Given that underlying store is shared with REST,
	// we don't destroy it here explicitly.
}

// Update alters the resource fields in PodSpec
func (r *ResizeREST) Update(ctx context.Context, name string, objInfo rest.UpdatedObjectInfo, createValidation rest.ValidateObjectFunc, updateValidation rest.ValidateObjectUpdateFunc, forceAllowCreate bool, options *metav1.UpdateOptions) (runtime.Object, bool, error) {
	// We are explicitly setting forceAllowCreate to false in the call to the underlying storage because
	// subresources should never allow create on update.
	return r.store.Update(ctx, name, objInfo, createValidation, updateValidation, false, options)
}
