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

package registry

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"sigs.k8s.io/structured-merge-diff/v4/fieldpath"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/validation"
	"k8s.io/apimachinery/pkg/api/validation/path"
	metainternalversion "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/apiserver/pkg/endpoints/handlers/fieldmanager"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/features"
	"k8s.io/apiserver/pkg/registry/generic"
	"k8s.io/apiserver/pkg/registry/rest"
	"k8s.io/apiserver/pkg/storage"
	storeerr "k8s.io/apiserver/pkg/storage/errors"
	"k8s.io/apiserver/pkg/storage/etcd3/metrics"
	"k8s.io/apiserver/pkg/util/dryrun"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	flowcontrolrequest "k8s.io/apiserver/pkg/util/flowcontrol/request"
	"k8s.io/client-go/tools/cache"

	"k8s.io/klog/v2"
)

// FinishFunc is a function returned by Begin hooks to complete an operation.
type FinishFunc func(ctx context.Context, success bool)

// AfterDeleteFunc is the type used for the Store.AfterDelete hook.
type AfterDeleteFunc func(obj runtime.Object, options *metav1.DeleteOptions)

// BeginCreateFunc is the type used for the Store.BeginCreate hook.
type BeginCreateFunc func(ctx context.Context, obj runtime.Object, options *metav1.CreateOptions) (FinishFunc, error)

// AfterCreateFunc is the type used for the Store.AfterCreate hook.
type AfterCreateFunc func(obj runtime.Object, options *metav1.CreateOptions)

// BeginUpdateFunc is the type used for the Store.BeginUpdate hook.
type BeginUpdateFunc func(ctx context.Context, obj, old runtime.Object, options *metav1.UpdateOptions) (FinishFunc, error)

// AfterUpdateFunc is the type used for the Store.AfterUpdate hook.
type AfterUpdateFunc func(obj runtime.Object, options *metav1.UpdateOptions)

// GenericStore interface can be used for type assertions when we need to access the underlying strategies.
type GenericStore interface {
	GetCreateStrategy() rest.RESTCreateStrategy
	GetUpdateStrategy() rest.RESTUpdateStrategy
	GetDeleteStrategy() rest.RESTDeleteStrategy
}

// Store implements k8s.io/apiserver/pkg/registry/rest.StandardStorage. It's
// intended to be embeddable and allows the consumer to implement any
// non-generic functions that are required. This object is intended to be
// copyable so that it can be used in different ways but share the same
// underlying behavior.
//
// All fields are required unless specified.
//
// The intended use of this type is embedding within a Kind specific
// RESTStorage implementation. This type provides CRUD semantics on a Kubelike
// resource, handling details like conflict detection with ResourceVersion and
// semantics. The RESTCreateStrategy, RESTUpdateStrategy, and
// RESTDeleteStrategy are generic across all backends, and encapsulate logic
// specific to the API.
//
// TODO: make the default exposed methods exactly match a generic RESTStorage
type Store struct {
	// NewFunc returns a new instance of the type this registry returns for a
	// GET of a single object, e.g.:
	//
	// curl GET /apis/group/version/namespaces/my-ns/myresource/name-of-object
	NewFunc func() runtime.Object

	// NewListFunc returns a new list of the type this registry; it is the
	// type returned when the resource is listed, e.g.:
	//
	// curl GET /apis/group/version/namespaces/my-ns/myresource
	NewListFunc func() runtime.Object

	// DefaultQualifiedResource is the pluralized name of the resource.
	// This field is used if there is no request info present in the context.
	// See qualifiedResourceFromContext for details.
	DefaultQualifiedResource schema.GroupResource

	// SingularQualifiedResource is the singular name of the resource.
	SingularQualifiedResource schema.GroupResource

	// KeyRootFunc returns the root etcd key for this resource; should not
	// include trailing "/".  This is used for operations that work on the
	// entire collection (listing and watching).
	//
	// KeyRootFunc and KeyFunc must be supplied together or not at all.
	KeyRootFunc func(ctx context.Context) string

	// KeyFunc returns the key for a specific object in the collection.
	// KeyFunc is called for Create/Update/Get/Delete. Note that 'namespace'
	// can be gotten from ctx.
	//
	// KeyFunc and KeyRootFunc must be supplied together or not at all.
	KeyFunc func(ctx context.Context, name string) (string, error)

	// ObjectNameFunc returns the name of an object or an error.
	ObjectNameFunc func(obj runtime.Object) (string, error)

	// TTLFunc returns the TTL (time to live) that objects should be persisted
	// with. The existing parameter is the current TTL or the default for this
	// operation. The update parameter indicates whether this is an operation
	// against an existing object.
	//
	// Objects that are persisted with a TTL are evicted once the TTL expires.
	TTLFunc func(obj runtime.Object, existing uint64, update bool) (uint64, error)

	// PredicateFunc returns a matcher corresponding to the provided labels
	// and fields. The SelectionPredicate returned should return true if the
	// object matches the given field and label selectors.
	PredicateFunc func(label labels.Selector, field fields.Selector) storage.SelectionPredicate

	// EnableGarbageCollection affects the handling of Update and Delete
	// requests. Enabling garbage collection allows finalizers to do work to
	// finalize this object before the store deletes it.
	//
	// If any store has garbage collection enabled, it must also be enabled in
	// the kube-controller-manager.
	EnableGarbageCollection bool

	// DeleteCollectionWorkers is the maximum number of workers in a single
	// DeleteCollection call. Delete requests for the items in a collection
	// are issued in parallel.
	DeleteCollectionWorkers int

	// Decorator is an optional exit hook on an object returned from the
	// underlying storage. The returned object could be an individual object
	// (e.g. Pod) or a list type (e.g. PodList). Decorator is intended for
	// integrations that are above storage and should only be used for
	// specific cases where storage of the value is not appropriate, since
	// they cannot be watched.
	Decorator func(runtime.Object)

	// CreateStrategy implements resource-specific behavior during creation.
	CreateStrategy rest.RESTCreateStrategy
	// BeginCreate is an optional hook that returns a "transaction-like"
	// commit/revert function which will be called at the end of the operation,
	// but before AfterCreate and Decorator, indicating via the argument
	// whether the operation succeeded.  If this returns an error, the function
	// is not called.  Almost nobody should use this hook.
	BeginCreate BeginCreateFunc
	// AfterCreate implements a further operation to run after a resource is
	// created and before it is decorated, optional.
	AfterCreate AfterCreateFunc

	// UpdateStrategy implements resource-specific behavior during updates.
	UpdateStrategy rest.RESTUpdateStrategy
	// BeginUpdate is an optional hook that returns a "transaction-like"
	// commit/revert function which will be called at the end of the operation,
	// but before AfterUpdate and Decorator, indicating via the argument
	// whether the operation succeeded.  If this returns an error, the function
	// is not called.  Almost nobody should use this hook.
	BeginUpdate BeginUpdateFunc
	// AfterUpdate implements a further operation to run after a resource is
	// updated and before it is decorated, optional.
	AfterUpdate AfterUpdateFunc

	// DeleteStrategy implements resource-specific behavior during deletion.
	DeleteStrategy rest.RESTDeleteStrategy
	// AfterDelete implements a further operation to run after a resource is
	// deleted and before it is decorated, optional.
	AfterDelete AfterDeleteFunc
	// ReturnDeletedObject determines whether the Store returns the object
	// that was deleted. Otherwise, return a generic success status response.
	ReturnDeletedObject bool
	// ShouldDeleteDuringUpdate is an optional function to determine whether
	// an update from existing to obj should result in a delete.
	// If specified, this is checked in addition to standard finalizer,
	// deletionTimestamp, and deletionGracePeriodSeconds checks.
	ShouldDeleteDuringUpdate func(ctx context.Context, key string, obj, existing runtime.Object) bool

	// TableConvertor is an optional interface for transforming items or lists
	// of items into tabular output. If unset, the default will be used.
	TableConvertor rest.TableConvertor

	// ResetFieldsStrategy provides the fields reset by the strategy that
	// should not be modified by the user.
	ResetFieldsStrategy rest.ResetFieldsStrategy

	// Storage is the interface for the underlying storage for the
	// resource. It is wrapped into a "DryRunnableStorage" that will
	// either pass-through or simply dry-run.
	Storage DryRunnableStorage
	// StorageVersioner outputs the <group/version/kind> an object will be
	// converted to before persisted in etcd, given a list of possible
	// kinds of the object.
	// If the StorageVersioner is nil, apiserver will leave the
	// storageVersionHash as empty in the discovery document.
	StorageVersioner runtime.GroupVersioner

	// ReadinessCheckFunc checks if the storage is ready for accepting requests.
	// The field is optional, if set needs to be thread-safe.
	ReadinessCheckFunc func() error

	// DestroyFunc cleans up clients used by the underlying Storage; optional.
	// If set, DestroyFunc has to be implemented in thread-safe way and
	// be prepared for being called more than once.
	DestroyFunc func()

	// corruptObjDeleter implements unsafe deletion flow to enable deletion
	// of corrupt object(s), it makes an attempt to perform a normal
	// deletion flow first, and if the normal deletion flow fails with a
	// corrupt object error then it proceeds with the unsafe deletion
	// of the object from the storage.
	// NOTE: it skips precondition checks, finalizer constraints, and any
	// after delete hook defined in 'AfterDelete' of the registry.
	// WARNING: This may break the cluster if the resource has
	// dependencies. Use when the cluster is broken, and there is no
	// other viable option to repair the cluster.
	corruptObjDeleter rest.GracefulDeleter
}

// Note: the rest.StandardStorage interface aggregates the common REST verbs
var _ rest.StandardStorage = &Store{}
var _ rest.StorageWithReadiness = &Store{}
var _ rest.TableConvertor = &Store{}
var _ GenericStore = &Store{}

var _ rest.SingularNameProvider = &Store{}

var _ rest.CorruptObjectDeleterProvider = &Store{}

const (
	OptimisticLockErrorMsg        = "the object has been modified; please apply your changes to the latest version and try again"
	resourceCountPollPeriodJitter = 1.2
)

// NamespaceKeyRootFunc is the default function for constructing storage paths
// to resource directories enforcing namespace rules.
func NamespaceKeyRootFunc(ctx context.Context, prefix string) string {
	key := prefix
	ns, ok := genericapirequest.NamespaceFrom(ctx)
	if ok && len(ns) > 0 {
		key = key + "/" + ns
	}
	return key
}

// NamespaceKeyFunc is the default function for constructing storage paths to
// a resource relative to the given prefix enforcing namespace rules. If the
// context does not contain a namespace, it errors.
func NamespaceKeyFunc(ctx context.Context, prefix string, name string) (string, error) {
	key := NamespaceKeyRootFunc(ctx, prefix)
	ns, ok := genericapirequest.NamespaceFrom(ctx)
	if !ok || len(ns) == 0 {
		return "", apierrors.NewBadRequest("Namespace parameter required.")
	}
	if len(name) == 0 {
		return "", apierrors.NewBadRequest("Name parameter required.")
	}
	if msgs := path.IsValidPathSegmentName(name); len(msgs) != 0 {
		return "", apierrors.NewBadRequest(fmt.Sprintf("Name parameter invalid: %q: %s", name, strings.Join(msgs, ";")))
	}
	key = key + "/" + name
	return key, nil
}

// NoNamespaceKeyFunc is the default function for constructing storage paths
// to a resource relative to the given prefix without a namespace.
func NoNamespaceKeyFunc(ctx context.Context, prefix string, name string) (string, error) {
	if len(name) == 0 {
		return "", apierrors.NewBadRequest("Name parameter required.")
	}
	if msgs := path.IsValidPathSegmentName(name); len(msgs) != 0 {
		return "", apierrors.NewBadRequest(fmt.Sprintf("Name parameter invalid: %q: %s", name, strings.Join(msgs, ";")))
	}
	key := prefix + "/" + name
	return key, nil
}

// New implements RESTStorage.New.
func (e *Store) New() runtime.Object {
	return e.NewFunc()
}

// ReadinessCheck checks if the storage is ready for accepting requests.
func (e *Store) ReadinessCheck() error {
	if e.ReadinessCheckFunc != nil {
		return e.ReadinessCheckFunc()
	}
	return nil
}

// Destroy cleans up its resources on shutdown.
func (e *Store) Destroy() {
	if e.DestroyFunc != nil {
		e.DestroyFunc()
	}
}

// NewList implements rest.Lister.
func (e *Store) NewList() runtime.Object {
	return e.NewListFunc()
}

// NamespaceScoped indicates whether the resource is namespaced
func (e *Store) NamespaceScoped() bool {
	if e.CreateStrategy != nil {
		return e.CreateStrategy.NamespaceScoped()
	}
	if e.UpdateStrategy != nil {
		return e.UpdateStrategy.NamespaceScoped()
	}

	panic("programmer error: no CRUD for resource, override NamespaceScoped too")
}

// GetCreateStrategy implements GenericStore.
func (e *Store) GetCreateStrategy() rest.RESTCreateStrategy {
	return e.CreateStrategy
}

// GetUpdateStrategy implements GenericStore.
func (e *Store) GetUpdateStrategy() rest.RESTUpdateStrategy {
	return e.UpdateStrategy
}

// GetDeleteStrategy implements GenericStore.
func (e *Store) GetDeleteStrategy() rest.RESTDeleteStrategy {
	return e.DeleteStrategy
}

// GetCorruptObjDeleter returns the unsafe corrupt object deleter
func (e *Store) GetCorruptObjDeleter() rest.GracefulDeleter {
	return e.corruptObjDeleter
}

// List returns a list of items matching labels and field according to the
// store's PredicateFunc.
func (e *Store) List(ctx context.Context, options *metainternalversion.ListOptions) (runtime.Object, error) {
	label := labels.Everything()
	if options != nil && options.LabelSelector != nil {
		label = options.LabelSelector
	}
	field := fields.Everything()
	if options != nil && options.FieldSelector != nil {
		field = options.FieldSelector
	}
	out, err := e.ListPredicate(ctx, e.PredicateFunc(label, field), options)
	if err != nil {
		return nil, err
	}
	if e.Decorator != nil {
		e.Decorator(out)
	}
	return out, nil
}

// ListPredicate returns a list of all the items matching the given
// SelectionPredicate.
func (e *Store) ListPredicate(ctx context.Context, p storage.SelectionPredicate, options *metainternalversion.ListOptions) (runtime.Object, error) {
	if options == nil {
		// By default we should serve the request from etcd.
		options = &metainternalversion.ListOptions{ResourceVersion: ""}
	}
	p.Limit = options.Limit
	p.Continue = options.Continue
	list := e.NewListFunc()
	qualifiedResource := e.qualifiedResourceFromContext(ctx)
	storageOpts := storage.ListOptions{
		ResourceVersion:      options.ResourceVersion,
		ResourceVersionMatch: options.ResourceVersionMatch,
		Predicate:            p,
		Recursive:            true,
	}

	// if we're not already namespace-scoped, see if the field selector narrows the scope of the watch
	if requestNamespace, _ := genericapirequest.NamespaceFrom(ctx); len(requestNamespace) == 0 {
		if selectorNamespace, ok := p.MatchesSingleNamespace(); ok {
			if len(validation.ValidateNamespaceName(selectorNamespace, false)) == 0 {
				ctx = genericapirequest.WithNamespace(ctx, selectorNamespace)
			}
		}
	}

	if name, ok := p.MatchesSingle(); ok {
		if key, err := e.KeyFunc(ctx, name); err == nil {
			storageOpts.Recursive = false
			err := e.Storage.GetList(ctx, key, storageOpts, list)
			return list, storeerr.InterpretListError(err, qualifiedResource)
		}
		// if we cannot extract a key based on the current context, the optimization is skipped
	}

	err := e.Storage.GetList(ctx, e.KeyRootFunc(ctx), storageOpts, list)
	return list, storeerr.InterpretListError(err, qualifiedResource)
}

// finishNothing is a do-nothing FinishFunc.
func finishNothing(context.Context, bool) {}

// maxNameGenerationCreateAttempts is the maximum number of
// times create will be attempted when generateName is used
// and create attempts fails due to name conflict errors.
// Each attempt uses a newly randomly generated name.
// 8 was selected as the max because it is sufficient to generate
// 1 million names per generateName prefix, with only a 0.1%
// probability of any generated name conflicting with existing names.
// Without retry, a 0.1% probability occurs at ~500
// generated names and a 50% probability occurs at ~4500
// generated names.
const maxNameGenerationCreateAttempts = 8

// Create inserts a new item according to the unique key from the object.
// Note that registries may mutate the input object (e.g. in the strategy
// hooks).  Tests which call this might want to call DeepCopy if they expect to
// be able to examine the input and output objects for differences.
func (e *Store) Create(ctx context.Context, obj runtime.Object, createValidation rest.ValidateObjectFunc, options *metav1.CreateOptions) (runtime.Object, error) {
	if utilfeature.DefaultFeatureGate.Enabled(features.RetryGenerateName) && needsNameGeneration(obj) {
		return e.createWithGenerateNameRetry(ctx, obj, createValidation, options)
	}

	return e.create(ctx, obj, createValidation, options)
}

// needsNameGeneration returns true if the obj has a generateName but no name.
func needsNameGeneration(obj runtime.Object) bool {
	if objectMeta, err := meta.Accessor(obj); err == nil {
		if len(objectMeta.GetGenerateName()) > 0 && len(objectMeta.GetName()) == 0 {
			return true
		}
	}
	return false
}

// createWithGenerateNameRetry attempts to create obj up to maxNameGenerationCreateAttempts
// when create fails due to a name conflict error. Each attempt randomly generates a new
// name based on generateName.
func (e *Store) createWithGenerateNameRetry(ctx context.Context, obj runtime.Object, createValidation rest.ValidateObjectFunc, options *metav1.CreateOptions) (resultObj runtime.Object, err error) {
	for i := 0; i < maxNameGenerationCreateAttempts; i++ {
		resultObj, err = e.create(ctx, obj.DeepCopyObject(), createValidation, options)
		if err == nil || !apierrors.IsAlreadyExists(err) {
			return resultObj, err
		}
	}
	return resultObj, err
}

func (e *Store) create(ctx context.Context, obj runtime.Object, createValidation rest.ValidateObjectFunc, options *metav1.CreateOptions) (runtime.Object, error) {
	var finishCreate FinishFunc = finishNothing

	// Init metadata as early as possible.
	if objectMeta, err := meta.Accessor(obj); err != nil {
		return nil, err
	} else {
		rest.FillObjectMetaSystemFields(objectMeta)
		if len(objectMeta.GetGenerateName()) > 0 && len(objectMeta.GetName()) == 0 {
			objectMeta.SetName(e.CreateStrategy.GenerateName(objectMeta.GetGenerateName()))
		}
	}

	if e.BeginCreate != nil {
		fn, err := e.BeginCreate(ctx, obj, options)
		if err != nil {
			return nil, err
		}
		finishCreate = fn
		defer func() {
			finishCreate(ctx, false)
		}()
	}

	if err := rest.BeforeCreate(e.CreateStrategy, ctx, obj); err != nil {
		return nil, err
	}
	// at this point we have a fully formed object.  It is time to call the validators that the apiserver
	// handling chain wants to enforce.
	if createValidation != nil {
		if err := createValidation(ctx, obj.DeepCopyObject()); err != nil {
			return nil, err
		}
	}

	name, err := e.ObjectNameFunc(obj)
	if err != nil {
		return nil, err
	}
	key, err := e.KeyFunc(ctx, name)
	if err != nil {
		return nil, err
	}
	qualifiedResource := e.qualifiedResourceFromContext(ctx)
	ttl, err := e.calculateTTL(obj, 0, false)
	if err != nil {
		return nil, err
	}
	out := e.NewFunc()
	if err := e.Storage.Create(ctx, key, obj, out, ttl, dryrun.IsDryRun(options.DryRun)); err != nil {
		err = storeerr.InterpretCreateError(err, qualifiedResource, name)
		err = rest.CheckGeneratedNameError(ctx, e.CreateStrategy, err, obj)
		if !apierrors.IsAlreadyExists(err) {
			return nil, err
		}
		if errGet := e.Storage.Get(ctx, key, storage.GetOptions{}, out); errGet != nil {
			return nil, err
		}
		accessor, errGetAcc := meta.Accessor(out)
		if errGetAcc != nil {
			return nil, err
		}
		if accessor.GetDeletionTimestamp() != nil {
			msg := &err.(*apierrors.StatusError).ErrStatus.Message
			*msg = fmt.Sprintf("object is being deleted: %s", *msg)
		}
		return nil, err
	}
	// The operation has succeeded.  Call the finish function if there is one,
	// and then make sure the defer doesn't call it again.
	fn := finishCreate
	finishCreate = finishNothing
	fn(ctx, true)

	if e.AfterCreate != nil {
		e.AfterCreate(out, options)
	}
	if e.Decorator != nil {
		e.Decorator(out)
	}
	return out, nil
}

// ShouldDeleteDuringUpdate is the default function for
// checking if an object should be deleted during an update.
// It checks if the new object has no finalizers,
// the existing object's deletionTimestamp is set, and
// the existing object's deletionGracePeriodSeconds is 0 or nil
func ShouldDeleteDuringUpdate(ctx context.Context, key string, obj, existing runtime.Object) bool {
	newMeta, err := meta.Accessor(obj)
	if err != nil {
		utilruntime.HandleError(err)
		return false
	}
	oldMeta, err := meta.Accessor(existing)
	if err != nil {
		utilruntime.HandleError(err)
		return false
	}
	if len(newMeta.GetFinalizers()) > 0 {
		// don't delete with finalizers remaining in the new object
		return false
	}
	if oldMeta.GetDeletionTimestamp() == nil {
		// don't delete if the existing object hasn't had a delete request made
		return false
	}
	// delete if the existing object has no grace period or a grace period of 0
	return oldMeta.GetDeletionGracePeriodSeconds() == nil || *oldMeta.GetDeletionGracePeriodSeconds() == 0
}

// deleteWithoutFinalizers handles deleting an object ignoring its finalizer list.
// Used for objects that are either been finalized or have never initialized.
func (e *Store) deleteWithoutFinalizers(ctx context.Context, name, key string, obj runtime.Object, preconditions *storage.Preconditions, options *metav1.DeleteOptions) (runtime.Object, bool, error) {
	out := e.NewFunc()
	klog.V(6).InfoS("Going to delete object from registry, triggered by update", "object", klog.KRef(genericapirequest.NamespaceValue(ctx), name))
	// Using the rest.ValidateAllObjectFunc because the request is an UPDATE request and has already passed the admission for the UPDATE verb.
	if err := e.Storage.Delete(ctx, key, out, preconditions, rest.ValidateAllObjectFunc, dryrun.IsDryRun(options.DryRun), nil, storage.DeleteOptions{}); err != nil {
		// Deletion is racy, i.e., there could be multiple update
		// requests to remove all finalizers from the object, so we
		// ignore the NotFound error.
		if storage.IsNotFound(err) {
			_, err := e.finalizeDelete(ctx, obj, true, options)
			// clients are expecting an updated object if a PUT succeeded,
			// but finalizeDelete returns a metav1.Status, so return
			// the object in the request instead.
			return obj, false, err
		}
		return nil, false, storeerr.InterpretDeleteError(err, e.qualifiedResourceFromContext(ctx), name)
	}
	_, err := e.finalizeDelete(ctx, out, true, options)
	// clients are expecting an updated object if a PUT succeeded, but
	// finalizeDelete returns a metav1.Status, so return the object in
	// the request instead.
	return obj, false, err
}

// Update performs an atomic update and set of the object. Returns the result of the update
// or an error. If the registry allows create-on-update, the create flow will be executed.
// A bool is returned along with the object and any errors, to indicate object creation.
func (e *Store) Update(ctx context.Context, name string, objInfo rest.UpdatedObjectInfo, createValidation rest.ValidateObjectFunc, updateValidation rest.ValidateObjectUpdateFunc, forceAllowCreate bool, options *metav1.UpdateOptions) (runtime.Object, bool, error) {
	key, err := e.KeyFunc(ctx, name)
	if err != nil {
		return nil, false, err
	}

	var (
		creatingObj runtime.Object
		creating    = false
	)

	qualifiedResource := e.qualifiedResourceFromContext(ctx)
	storagePreconditions := &storage.Preconditions{}
	if preconditions := objInfo.Preconditions(); preconditions != nil {
		storagePreconditions.UID = preconditions.UID
		storagePreconditions.ResourceVersion = preconditions.ResourceVersion
	}

	out := e.NewFunc()
	// deleteObj is only used in case a deletion is carried out
	var deleteObj runtime.Object
	err = e.Storage.GuaranteedUpdate(ctx, key, out, true, storagePreconditions, func(existing runtime.Object, res storage.ResponseMeta) (runtime.Object, *uint64, error) {
		existingResourceVersion, err := e.Storage.Versioner().ObjectResourceVersion(existing)
		if err != nil {
			return nil, nil, err
		}
		if existingResourceVersion == 0 {
			if !e.UpdateStrategy.AllowCreateOnUpdate() && !forceAllowCreate {
				return nil, nil, apierrors.NewNotFound(qualifiedResource, name)
			}
		}

		// Given the existing object, get the new object
		obj, err := objInfo.UpdatedObject(ctx, existing)
		if err != nil {
			return nil, nil, err
		}

		// If AllowUnconditionalUpdate() is true and the object specified by
		// the user does not have a resource version, then we populate it with
		// the latest version. Else, we check that the version specified by
		// the user matches the version of latest storage object.
		newResourceVersion, err := e.Storage.Versioner().ObjectResourceVersion(obj)
		if err != nil {
			return nil, nil, err
		}
		doUnconditionalUpdate := newResourceVersion == 0 && e.UpdateStrategy.AllowUnconditionalUpdate()

		if existingResourceVersion == 0 {
			// Init metadata as early as possible.
			if objectMeta, err := meta.Accessor(obj); err != nil {
				return nil, nil, err
			} else {
				rest.FillObjectMetaSystemFields(objectMeta)
			}

			var finishCreate FinishFunc = finishNothing

			if e.BeginCreate != nil {
				fn, err := e.BeginCreate(ctx, obj, newCreateOptionsFromUpdateOptions(options))
				if err != nil {
					return nil, nil, err
				}
				finishCreate = fn
				defer func() {
					finishCreate(ctx, false)
				}()
			}

			creating = true
			creatingObj = obj
			if err := rest.BeforeCreate(e.CreateStrategy, ctx, obj); err != nil {
				return nil, nil, err
			}
			// at this point we have a fully formed object.  It is time to call the validators that the apiserver
			// handling chain wants to enforce.
			if createValidation != nil {
				if err := createValidation(ctx, obj.DeepCopyObject()); err != nil {
					return nil, nil, err
				}
			}
			ttl, err := e.calculateTTL(obj, 0, false)
			if err != nil {
				return nil, nil, err
			}

			// The operation has succeeded.  Call the finish function if there is one,
			// and then make sure the defer doesn't call it again.
			fn := finishCreate
			finishCreate = finishNothing
			fn(ctx, true)

			return obj, &ttl, nil
		}

		creating = false
		creatingObj = nil
		if doUnconditionalUpdate {
			// Update the object's resource version to match the latest
			// storage object's resource version.
			err = e.Storage.Versioner().UpdateObject(obj, res.ResourceVersion)
			if err != nil {
				return nil, nil, err
			}
		} else {
			// Check if the object's resource version matches the latest
			// resource version.
			if newResourceVersion == 0 {
				// TODO: The Invalid error should have a field for Resource.
				// After that field is added, we should fill the Resource and
				// leave the Kind field empty. See the discussion in #18526.
				qualifiedKind := schema.GroupKind{Group: qualifiedResource.Group, Kind: qualifiedResource.Resource}
				fieldErrList := field.ErrorList{field.Invalid(field.NewPath("metadata").Child("resourceVersion"), newResourceVersion, "must be specified for an update")}
				return nil, nil, apierrors.NewInvalid(qualifiedKind, name, fieldErrList)
			}
			if newResourceVersion != existingResourceVersion {
				return nil, nil, apierrors.NewConflict(qualifiedResource, name, fmt.Errorf(OptimisticLockErrorMsg))
			}
		}

		var finishUpdate FinishFunc = finishNothing

		if e.BeginUpdate != nil {
			fn, err := e.BeginUpdate(ctx, obj, existing, options)
			if err != nil {
				return nil, nil, err
			}
			finishUpdate = fn
			defer func() {
				finishUpdate(ctx, false)
			}()
		}

		if err := rest.BeforeUpdate(e.UpdateStrategy, ctx, obj, existing); err != nil {
			return nil, nil, err
		}

		// Ignore changes that only affect managed fields timestamps.
		// FieldManager can't know about changes like normalized fields, defaulted
		// fields and other mutations.
		obj, err = fieldmanager.IgnoreManagedFieldsTimestampsTransformer(ctx, obj, existing)
		if err != nil {
			return nil, nil, err
		}

		// at this point we have a fully formed object.  It is time to call the validators that the apiserver
		// handling chain wants to enforce.
		if updateValidation != nil {
			if err := updateValidation(ctx, obj.DeepCopyObject(), existing.DeepCopyObject()); err != nil {
				return nil, nil, err
			}
		}
		// Check the default delete-during-update conditions, and store-specific conditions if provided
		if ShouldDeleteDuringUpdate(ctx, key, obj, existing) &&
			(e.ShouldDeleteDuringUpdate == nil || e.ShouldDeleteDuringUpdate(ctx, key, obj, existing)) {
			deleteObj = obj
			return nil, nil, errEmptiedFinalizers
		}
		ttl, err := e.calculateTTL(obj, res.TTL, true)
		if err != nil {
			return nil, nil, err
		}

		// The operation has succeeded.  Call the finish function if there is one,
		// and then make sure the defer doesn't call it again.
		fn := finishUpdate
		finishUpdate = finishNothing
		fn(ctx, true)

		if int64(ttl) != res.TTL {
			return obj, &ttl, nil
		}
		return obj, nil, nil
	}, dryrun.IsDryRun(options.DryRun), nil)

	if err != nil {
		// delete the object
		if err == errEmptiedFinalizers {
			return e.deleteWithoutFinalizers(ctx, name, key, deleteObj, storagePreconditions, newDeleteOptionsFromUpdateOptions(options))
		}
		if creating {
			err = storeerr.InterpretCreateError(err, qualifiedResource, name)
			err = rest.CheckGeneratedNameError(ctx, e.CreateStrategy, err, creatingObj)
		} else {
			err = storeerr.InterpretUpdateError(err, qualifiedResource, name)
		}
		return nil, false, err
	}

	if creating {
		if e.AfterCreate != nil {
			e.AfterCreate(out, newCreateOptionsFromUpdateOptions(options))
		}
	} else {
		if e.AfterUpdate != nil {
			e.AfterUpdate(out, options)
		}
	}
	if e.Decorator != nil {
		e.Decorator(out)
	}
	return out, creating, nil
}

// This is a helper to convert UpdateOptions to CreateOptions for the
// create-on-update path.
func newCreateOptionsFromUpdateOptions(in *metav1.UpdateOptions) *metav1.CreateOptions {
	co := &metav1.CreateOptions{
		DryRun:          in.DryRun,
		FieldManager:    in.FieldManager,
		FieldValidation: in.FieldValidation,
	}
	co.TypeMeta.SetGroupVersionKind(metav1.SchemeGroupVersion.WithKind("CreateOptions"))
	return co
}

// This is a helper to convert UpdateOptions to DeleteOptions for the
// delete-on-update path.
func newDeleteOptionsFromUpdateOptions(in *metav1.UpdateOptions) *metav1.DeleteOptions {
	do := &metav1.DeleteOptions{
		DryRun: in.DryRun,
	}
	do.TypeMeta.SetGroupVersionKind(metav1.SchemeGroupVersion.WithKind("DeleteOptions"))
	return do
}

// Get retrieves the item from storage.
func (e *Store) Get(ctx context.Context, name string, options *metav1.GetOptions) (runtime.Object, error) {
	obj := e.NewFunc()
	key, err := e.KeyFunc(ctx, name)
	if err != nil {
		return nil, err
	}
	if err := e.Storage.Get(ctx, key, storage.GetOptions{ResourceVersion: options.ResourceVersion}, obj); err != nil {
		return nil, storeerr.InterpretGetError(err, e.qualifiedResourceFromContext(ctx), name)
	}
	if e.Decorator != nil {
		e.Decorator(obj)
	}
	return obj, nil
}

// qualifiedResourceFromContext attempts to retrieve a GroupResource from the context's request info.
// If the context has no request info, DefaultQualifiedResource is used.
func (e *Store) qualifiedResourceFromContext(ctx context.Context) schema.GroupResource {
	if info, ok := genericapirequest.RequestInfoFrom(ctx); ok {
		return schema.GroupResource{Group: info.APIGroup, Resource: info.Resource}
	}
	// some implementations access storage directly and thus the context has no RequestInfo
	return e.DefaultQualifiedResource
}

var (
	errAlreadyDeleting   = fmt.Errorf("abort delete")
	errDeleteNow         = fmt.Errorf("delete now")
	errEmptiedFinalizers = fmt.Errorf("emptied finalizers")
)

// shouldOrphanDependents returns true if the finalizer for orphaning should be set
// updated for FinalizerOrphanDependents. In the order of highest to lowest
// priority, there are three factors affect whether to add/remove the
// FinalizerOrphanDependents: options, existing finalizers of the object,
// and e.DeleteStrategy.DefaultGarbageCollectionPolicy.
func shouldOrphanDependents(ctx context.Context, e *Store, accessor metav1.Object, options *metav1.DeleteOptions) bool {
	// Get default GC policy from this REST object type
	gcStrategy, ok := e.DeleteStrategy.(rest.GarbageCollectionDeleteStrategy)
	var defaultGCPolicy rest.GarbageCollectionPolicy
	if ok {
		defaultGCPolicy = gcStrategy.DefaultGarbageCollectionPolicy(ctx)
	}

	if defaultGCPolicy == rest.Unsupported {
		// return  false to indicate that we should NOT orphan
		return false
	}

	// An explicit policy was set at deletion time, that overrides everything
	//nolint:staticcheck // SA1019 backwards compatibility
	if options != nil && options.OrphanDependents != nil {
		//nolint:staticcheck // SA1019 backwards compatibility
		return *options.OrphanDependents
	}
	if options != nil && options.PropagationPolicy != nil {
		switch *options.PropagationPolicy {
		case metav1.DeletePropagationOrphan:
			return true
		case metav1.DeletePropagationBackground, metav1.DeletePropagationForeground:
			return false
		}
	}

	// If a finalizer is set in the object, it overrides the default
	// validation should make sure the two cases won't be true at the same time.
	finalizers := accessor.GetFinalizers()
	for _, f := range finalizers {
		switch f {
		case metav1.FinalizerOrphanDependents:
			return true
		case metav1.FinalizerDeleteDependents:
			return false
		}
	}

	// Get default orphan policy from this REST object type if it exists
	return defaultGCPolicy == rest.OrphanDependents
}

// shouldDeleteDependents returns true if the finalizer for foreground deletion should be set
// updated for FinalizerDeleteDependents. In the order of highest to lowest
// priority, there are three factors affect whether to add/remove the
// FinalizerDeleteDependents: options, existing finalizers of the object, and
// e.DeleteStrategy.DefaultGarbageCollectionPolicy.
func shouldDeleteDependents(ctx context.Context, e *Store, accessor metav1.Object, options *metav1.DeleteOptions) bool {
	// Get default GC policy from this REST object type
	if gcStrategy, ok := e.DeleteStrategy.(rest.GarbageCollectionDeleteStrategy); ok && gcStrategy.DefaultGarbageCollectionPolicy(ctx) == rest.Unsupported {
		// return false to indicate that we should NOT delete in foreground
		return false
	}

	// If an explicit policy was set at deletion time, that overrides both
	//nolint:staticcheck // SA1019 backwards compatibility
	if options != nil && options.OrphanDependents != nil {
		return false
	}
	if options != nil && options.PropagationPolicy != nil {
		switch *options.PropagationPolicy {
		case metav1.DeletePropagationForeground:
			return true
		case metav1.DeletePropagationBackground, metav1.DeletePropagationOrphan:
			return false
		}
	}

	// If a finalizer is set in the object, it overrides the default
	// validation has made sure the two cases won't be true at the same time.
	finalizers := accessor.GetFinalizers()
	for _, f := range finalizers {
		switch f {
		case metav1.FinalizerDeleteDependents:
			return true
		case metav1.FinalizerOrphanDependents:
			return false
		}
	}

	return false
}

// deletionFinalizersForGarbageCollection analyzes the object and delete options
// to determine whether the object is in need of finalization by the garbage
// collector. If so, returns the set of deletion finalizers to apply and a bool
// indicating whether the finalizer list has changed and is in need of updating.
//
// The finalizers returned are intended to be handled by the garbage collector.
// If garbage collection is disabled for the store, this function returns false
// to ensure finalizers aren't set which will never be cleared.
func deletionFinalizersForGarbageCollection(ctx context.Context, e *Store, accessor metav1.Object, options *metav1.DeleteOptions) (bool, []string) {
	if !e.EnableGarbageCollection {
		return false, []string{}
	}
	shouldOrphan := shouldOrphanDependents(ctx, e, accessor, options)
	shouldDeleteDependentInForeground := shouldDeleteDependents(ctx, e, accessor, options)
	newFinalizers := []string{}

	// first remove both finalizers, add them back if needed.
	for _, f := range accessor.GetFinalizers() {
		if f == metav1.FinalizerOrphanDependents || f == metav1.FinalizerDeleteDependents {
			continue
		}
		newFinalizers = append(newFinalizers, f)
	}

	if shouldOrphan {
		newFinalizers = append(newFinalizers, metav1.FinalizerOrphanDependents)
	}
	if shouldDeleteDependentInForeground {
		newFinalizers = append(newFinalizers, metav1.FinalizerDeleteDependents)
	}

	oldFinalizerSet := sets.NewString(accessor.GetFinalizers()...)
	newFinalizersSet := sets.NewString(newFinalizers...)
	if oldFinalizerSet.Equal(newFinalizersSet) {
		return false, accessor.GetFinalizers()
	}
	return true, newFinalizers
}

// markAsDeleting sets the obj's DeletionGracePeriodSeconds to 0, and sets the
// DeletionTimestamp to "now" if there is no existing deletionTimestamp or if the existing
// deletionTimestamp is further in future. Finalizers are watching for such updates and will
// finalize the object if their IDs are present in the object's Finalizers list.
func markAsDeleting(obj runtime.Object, now time.Time) (err error) {
	objectMeta, kerr := meta.Accessor(obj)
	if kerr != nil {
		return kerr
	}
	// This handles Generation bump for resources that don't support graceful
	// deletion. For resources that support graceful deletion is handle in
	// pkg/api/rest/delete.go
	if objectMeta.GetDeletionTimestamp() == nil && objectMeta.GetGeneration() > 0 {
		objectMeta.SetGeneration(objectMeta.GetGeneration() + 1)
	}
	existingDeletionTimestamp := objectMeta.GetDeletionTimestamp()
	if existingDeletionTimestamp == nil || existingDeletionTimestamp.After(now) {
		metaNow := metav1.NewTime(now)
		objectMeta.SetDeletionTimestamp(&metaNow)
	}
	var zero int64 = 0
	objectMeta.SetDeletionGracePeriodSeconds(&zero)
	return nil
}

// updateForGracefulDeletionAndFinalizers updates the given object for
// graceful deletion and finalization by setting the deletion timestamp and
// grace period seconds (graceful deletion) and updating the list of
// finalizers (finalization); it returns:
//
//  1. an error
//  2. a boolean indicating that the object was not found, but it should be
//     ignored
//  3. a boolean indicating that the object's grace period is exhausted and it
//     should be deleted immediately
//  4. a new output object with the state that was updated
//  5. a copy of the last existing state of the object
func (e *Store) updateForGracefulDeletionAndFinalizers(ctx context.Context, name, key string, options *metav1.DeleteOptions, preconditions storage.Preconditions, deleteValidation rest.ValidateObjectFunc, in runtime.Object) (err error, ignoreNotFound, deleteImmediately bool, out, lastExisting runtime.Object) {
	lastGraceful := int64(0)
	var pendingFinalizers bool
	out = e.NewFunc()
	err = e.Storage.GuaranteedUpdate(
		ctx,
		key,
		out,
		false, /* ignoreNotFound */
		&preconditions,
		storage.SimpleUpdate(func(existing runtime.Object) (runtime.Object, error) {
			if err := deleteValidation(ctx, existing); err != nil {
				return nil, err
			}
			graceful, pendingGraceful, err := rest.BeforeDelete(e.DeleteStrategy, ctx, existing, options)
			if err != nil {
				return nil, err
			}
			if pendingGraceful {
				return nil, errAlreadyDeleting
			}

			// Add/remove the orphan finalizer as the options dictates.
			// Note that this occurs after checking pendingGraceful, so
			// finalizers cannot be updated via DeleteOptions if deletion has
			// started.
			existingAccessor, err := meta.Accessor(existing)
			if err != nil {
				return nil, err
			}
			needsUpdate, newFinalizers := deletionFinalizersForGarbageCollection(ctx, e, existingAccessor, options)
			if needsUpdate {
				existingAccessor.SetFinalizers(newFinalizers)
			}

			pendingFinalizers = len(existingAccessor.GetFinalizers()) != 0
			if !graceful {
				// set the DeleteGracePeriods to 0 if the object has pendingFinalizers but not supporting graceful deletion
				if pendingFinalizers {
					klog.V(6).InfoS("Object has pending finalizers, so the registry is going to update its status to deleting",
						"object", klog.KRef(genericapirequest.NamespaceValue(ctx), name), "gracePeriod", time.Second*0)
					err = markAsDeleting(existing, time.Now())
					if err != nil {
						return nil, err
					}
					return existing, nil
				}
				return nil, errDeleteNow
			}
			lastGraceful = *options.GracePeriodSeconds
			lastExisting = existing
			return existing, nil
		}),
		dryrun.IsDryRun(options.DryRun),
		nil,
	)
	switch err {
	case nil:
		// If there are pending finalizers, we never delete the object immediately.
		if pendingFinalizers {
			return nil, false, false, out, lastExisting
		}
		if lastGraceful > 0 {
			return nil, false, false, out, lastExisting
		}
		// If we are here, the registry supports grace period mechanism and
		// we are intentionally delete gracelessly. In this case, we may
		// enter a race with other k8s components. If other component wins
		// the race, the object will not be found, and we should tolerate
		// the NotFound error. See
		// https://github.com/kubernetes/kubernetes/issues/19403 for
		// details.
		return nil, true, true, out, lastExisting
	case errDeleteNow:
		// we've updated the object to have a zero grace period, or it's already at 0, so
		// we should fall through and truly delete the object.
		return nil, false, true, out, lastExisting
	case errAlreadyDeleting:
		out, err = e.finalizeDelete(ctx, in, true, options)
		return err, false, false, out, lastExisting
	default:
		return storeerr.InterpretUpdateError(err, e.qualifiedResourceFromContext(ctx), name), false, false, out, lastExisting
	}
}

// Delete removes the item from storage.
// options can be mutated by rest.BeforeDelete due to a graceful deletion strategy.
func (e *Store) Delete(ctx context.Context, name string, deleteValidation rest.ValidateObjectFunc, options *metav1.DeleteOptions) (runtime.Object, bool, error) {
	key, err := e.KeyFunc(ctx, name)
	if err != nil {
		return nil, false, err
	}
	obj := e.NewFunc()
	qualifiedResource := e.qualifiedResourceFromContext(ctx)
	if err = e.Storage.Get(ctx, key, storage.GetOptions{}, obj); err != nil {
		return nil, false, storeerr.InterpretDeleteError(err, qualifiedResource, name)
	}

	// support older consumers of delete by treating "nil" as delete immediately
	if options == nil {
		options = metav1.NewDeleteOptions(0)
	}
	var preconditions storage.Preconditions
	if options.Preconditions != nil {
		preconditions.UID = options.Preconditions.UID
		preconditions.ResourceVersion = options.Preconditions.ResourceVersion
	}
	graceful, pendingGraceful, err := rest.BeforeDelete(e.DeleteStrategy, ctx, obj, options)
	if err != nil {
		return nil, false, err
	}
	// this means finalizers cannot be updated via DeleteOptions if a deletion is already pending
	if pendingGraceful {
		out, err := e.finalizeDelete(ctx, obj, false, options)
		return out, false, err
	}
	// check if obj has pending finalizers
	accessor, err := meta.Accessor(obj)
	if err != nil {
		return nil, false, apierrors.NewInternalError(err)
	}
	pendingFinalizers := len(accessor.GetFinalizers()) != 0
	var ignoreNotFound bool
	var deleteImmediately bool = true
	var lastExisting, out runtime.Object

	// Handle combinations of graceful deletion and finalization by issuing
	// the correct updates.
	shouldUpdateFinalizers, _ := deletionFinalizersForGarbageCollection(ctx, e, accessor, options)
	// TODO: remove the check, because we support no-op updates now.
	if graceful || pendingFinalizers || shouldUpdateFinalizers {
		err, ignoreNotFound, deleteImmediately, out, lastExisting = e.updateForGracefulDeletionAndFinalizers(ctx, name, key, options, preconditions, deleteValidation, obj)
		// Update the preconditions.ResourceVersion if set since we updated the object.
		if err == nil && deleteImmediately && preconditions.ResourceVersion != nil {
			accessor, err = meta.Accessor(out)
			if err != nil {
				return out, false, apierrors.NewInternalError(err)
			}
			resourceVersion := accessor.GetResourceVersion()
			preconditions.ResourceVersion = &resourceVersion
		}
	}

	// !deleteImmediately covers all cases where err != nil. We keep both to be future-proof.
	if !deleteImmediately || err != nil {
		return out, false, err
	}

	// Going further in this function is not useful when we are
	// performing a dry-run request. Worse, it will actually
	// override "out" with the version of the object in database
	// that doesn't have the finalizer and deletiontimestamp set
	// (because the update above was dry-run too). If we already
	// have that version available, let's just return it now,
	// otherwise, we can call dry-run delete that will get us the
	// latest version of the object.
	if dryrun.IsDryRun(options.DryRun) && out != nil {
		return out, true, nil
	}

	// delete immediately, or no graceful deletion supported
	klog.V(6).InfoS("Going to delete object from registry", "object", klog.KRef(genericapirequest.NamespaceValue(ctx), name))
	out = e.NewFunc()
	if err := e.Storage.Delete(ctx, key, out, &preconditions, storage.ValidateObjectFunc(deleteValidation), dryrun.IsDryRun(options.DryRun), nil, storage.DeleteOptions{}); err != nil {
		// Please refer to the place where we set ignoreNotFound for the reason
		// why we ignore the NotFound error .
		if storage.IsNotFound(err) && ignoreNotFound && lastExisting != nil {
			// The lastExisting object may not be the last state of the object
			// before its deletion, but it's the best approximation.
			out, err := e.finalizeDelete(ctx, lastExisting, true, options)
			return out, true, err
		}
		return nil, false, storeerr.InterpretDeleteError(err, qualifiedResource, name)
	}
	out, err = e.finalizeDelete(ctx, out, true, options)
	return out, true, err
}

// DeleteReturnsDeletedObject implements the rest.MayReturnFullObjectDeleter interface
func (e *Store) DeleteReturnsDeletedObject() bool {
	return e.ReturnDeletedObject
}

// deleteCollectionPageSize is the size of the page used when
// listing objects from storage during DeleteCollection calls.
// It's a variable to make allow overwriting in tests.
var deleteCollectionPageSize = int64(10000)

// DeleteCollection removes all items returned by List with a given ListOptions from storage.
//
// DeleteCollection is currently NOT atomic. It can happen that only subset of objects
// will be deleted from storage, and then an error will be returned.
// In case of success, the list of deleted objects will be returned.
func (e *Store) DeleteCollection(ctx context.Context, deleteValidation rest.ValidateObjectFunc, options *metav1.DeleteOptions, listOptions *metainternalversion.ListOptions) (runtime.Object, error) {
	if listOptions == nil {
		listOptions = &metainternalversion.ListOptions{}
	} else {
		listOptions = listOptions.DeepCopy()
	}

	var items []runtime.Object

	workersNumber := e.DeleteCollectionWorkers
	if workersNumber < 1 {
		workersNumber = 1
	}
	wg := sync.WaitGroup{}
	// Ensure that chanSize is not too high (to avoid wasted work) but
	// at the same time high enough to start listing before we process
	// the whole page.
	chanSize := 2 * workersNumber
	if chanSize < 256 {
		chanSize = 256
	}
	toProcess := make(chan runtime.Object, chanSize)
	errs := make(chan error, workersNumber+1)
	workersExited := make(chan struct{})

	wg.Add(workersNumber)
	for i := 0; i < workersNumber; i++ {
		go func() {
			// panics don't cross goroutine boundaries
			defer utilruntime.HandleCrash(func(panicReason interface{}) {
				errs <- fmt.Errorf("DeleteCollection goroutine panicked: %v", panicReason)
			})
			defer wg.Done()

			for item := range toProcess {
				accessor, err := meta.Accessor(item)
				if err != nil {
					errs <- err
					return
				}
				// DeepCopy the deletion options because individual graceful deleters communicate changes via a mutating
				// function in the delete strategy called in the delete method.  While that is always ugly, it works
				// when making a single call.  When making multiple calls via delete collection, the mutation applied to
				// pod/A can change the option ultimately used for pod/B.
				if _, _, err := e.Delete(ctx, accessor.GetName(), deleteValidation, options.DeepCopy()); err != nil && !apierrors.IsNotFound(err) {
					klog.V(4).InfoS("Delete object in DeleteCollection failed", "object", klog.KObj(accessor), "err", err)
					errs <- err
					return
				}
			}
		}()
	}
	// In case of all workers exit, notify distributor.
	go func() {
		defer utilruntime.HandleCrash(func(panicReason interface{}) {
			errs <- fmt.Errorf("DeleteCollection workers closer panicked: %v", panicReason)
		})
		wg.Wait()
		close(workersExited)
	}()

	hasLimit := listOptions.Limit > 0
	if listOptions.Limit == 0 {
		listOptions.Limit = deleteCollectionPageSize
	}

	// Paginate the list request and throw all items into workers.
	listObj, err := func() (runtime.Object, error) {
		defer close(toProcess)

		processedItems := 0
		var originalList runtime.Object
		for {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			default:
			}

			listObj, err := e.List(ctx, listOptions)
			if err != nil {
				return nil, err
			}

			newItems, err := meta.ExtractList(listObj)
			if err != nil {
				return nil, err
			}
			items = append(items, newItems...)

			for i := 0; i < len(newItems); i++ {
				select {
				case toProcess <- newItems[i]:
				case <-workersExited:
					klog.V(4).InfoS("workers already exited, and there are some items waiting to be processed", "queued/finished", i, "total", processedItems+len(newItems))
					// Try to propagate an error from the workers if possible.
					select {
					case err := <-errs:
						return nil, err
					default:
						return nil, fmt.Errorf("all DeleteCollection workers exited")
					}
				}
			}
			processedItems += len(newItems)

			// If the original request was setting the limit, finish after running it.
			if hasLimit {
				return listObj, nil
			}

			if originalList == nil {
				originalList = listObj
				meta.SetList(originalList, nil)
			}

			// If there are no more items, return the list.
			m, err := meta.ListAccessor(listObj)
			if err != nil {
				return nil, err
			}
			if len(m.GetContinue()) == 0 {
				meta.SetList(originalList, items)
				return originalList, nil
			}

			// Set up the next loop.
			listOptions.Continue = m.GetContinue()
			listOptions.ResourceVersion = ""
			listOptions.ResourceVersionMatch = ""
		}
	}()
	if err != nil {
		return nil, err
	}

	// Wait for all workers to exit.
	<-workersExited

	select {
	case err := <-errs:
		return nil, err
	default:
		return listObj, nil
	}
}

// finalizeDelete runs the Store's AfterDelete hook if runHooks is set and
// returns the decorated deleted object if appropriate.
func (e *Store) finalizeDelete(ctx context.Context, obj runtime.Object, runHooks bool, options *metav1.DeleteOptions) (runtime.Object, error) {
	if runHooks && e.AfterDelete != nil {
		e.AfterDelete(obj, options)
	}
	if e.ReturnDeletedObject {
		if e.Decorator != nil {
			e.Decorator(obj)
		}
		return obj, nil
	}
	// Return information about the deleted object, which enables clients to
	// verify that the object was actually deleted and not waiting for finalizers.
	accessor, err := meta.Accessor(obj)
	if err != nil {
		return nil, err
	}
	qualifiedResource := e.qualifiedResourceFromContext(ctx)
	details := &metav1.StatusDetails{
		Name:  accessor.GetName(),
		Group: qualifiedResource.Group,
		Kind:  qualifiedResource.Resource, // Yes we set Kind field to resource.
		UID:   accessor.GetUID(),
	}
	status := &metav1.Status{Status: metav1.StatusSuccess, Details: details}
	return status, nil
}

// Watch makes a matcher for the given label and field, and calls
// WatchPredicate. If possible, you should customize PredicateFunc to produce
// a matcher that matches by key. SelectionPredicate does this for you
// automatically.
func (e *Store) Watch(ctx context.Context, options *metainternalversion.ListOptions) (watch.Interface, error) {
	label := labels.Everything()
	if options != nil && options.LabelSelector != nil {
		label = options.LabelSelector
	}
	field := fields.Everything()
	if options != nil && options.FieldSelector != nil {
		field = options.FieldSelector
	}
	predicate := e.PredicateFunc(label, field)

	resourceVersion := ""
	if options != nil {
		resourceVersion = options.ResourceVersion
		predicate.AllowWatchBookmarks = options.AllowWatchBookmarks
	}
	return e.WatchPredicate(ctx, predicate, resourceVersion, options.SendInitialEvents)
}

// WatchPredicate starts a watch for the items that matches.
func (e *Store) WatchPredicate(ctx context.Context, p storage.SelectionPredicate, resourceVersion string, sendInitialEvents *bool) (watch.Interface, error) {
	storageOpts := storage.ListOptions{ResourceVersion: resourceVersion, Predicate: p, Recursive: true, SendInitialEvents: sendInitialEvents}

	// if we're not already namespace-scoped, see if the field selector narrows the scope of the watch
	if requestNamespace, _ := genericapirequest.NamespaceFrom(ctx); len(requestNamespace) == 0 {
		if selectorNamespace, ok := p.MatchesSingleNamespace(); ok {
			if len(validation.ValidateNamespaceName(selectorNamespace, false)) == 0 {
				ctx = genericapirequest.WithNamespace(ctx, selectorNamespace)
			}
		}
	}

	key := e.KeyRootFunc(ctx)
	if name, ok := p.MatchesSingle(); ok {
		if k, err := e.KeyFunc(ctx, name); err == nil {
			key = k
			storageOpts.Recursive = false
		}
		// if we cannot extract a key based on the current context, the
		// optimization is skipped
	}

	w, err := e.Storage.Watch(ctx, key, storageOpts)
	if err != nil {
		return nil, err
	}
	if e.Decorator != nil {
		return newDecoratedWatcher(ctx, w, e.Decorator), nil
	}
	return w, nil
}

// calculateTTL is a helper for retrieving the updated TTL for an object or
// returning an error if the TTL cannot be calculated. The defaultTTL is
// changed to 1 if less than zero. Zero means no TTL, not expire immediately.
func (e *Store) calculateTTL(obj runtime.Object, defaultTTL int64, update bool) (ttl uint64, err error) {
	// TODO: validate this is assertion is still valid.

	// etcd may return a negative TTL for a node if the expiration has not
	// occurred due to server lag - we will ensure that the value is at least
	// set.
	if defaultTTL < 0 {
		defaultTTL = 1
	}
	ttl = uint64(defaultTTL)
	if e.TTLFunc != nil {
		ttl, err = e.TTLFunc(obj, ttl, update)
	}
	return ttl, err
}

// CompleteWithOptions updates the store with the provided options and
// defaults common fields.
func (e *Store) CompleteWithOptions(options *generic.StoreOptions) error {
	if e.DefaultQualifiedResource.Empty() {
		return fmt.Errorf("store %#v must have a non-empty qualified resource", e)
	}
	if e.SingularQualifiedResource.Empty() {
		return fmt.Errorf("store %#v must have a non-empty singular qualified resource", e)
	}
	if e.DefaultQualifiedResource.Group != e.SingularQualifiedResource.Group {
		return fmt.Errorf("store for %#v, singular and plural qualified resource's group name's must match", e)
	}
	if e.NewFunc == nil {
		return fmt.Errorf("store for %s must have NewFunc set", e.DefaultQualifiedResource.String())
	}
	if e.NewListFunc == nil {
		return fmt.Errorf("store for %s must have NewListFunc set", e.DefaultQualifiedResource.String())
	}
	if (e.KeyRootFunc == nil) != (e.KeyFunc == nil) {
		return fmt.Errorf("store for %s must set both KeyRootFunc and KeyFunc or neither", e.DefaultQualifiedResource.String())
	}

	if e.TableConvertor == nil {
		return fmt.Errorf("store for %s must set TableConvertor; rest.NewDefaultTableConvertor(e.DefaultQualifiedResource) can be used to output just name/creation time", e.DefaultQualifiedResource.String())
	}

	var isNamespaced bool
	switch {
	case e.CreateStrategy != nil:
		isNamespaced = e.CreateStrategy.NamespaceScoped()
	case e.UpdateStrategy != nil:
		isNamespaced = e.UpdateStrategy.NamespaceScoped()
	default:
		return fmt.Errorf("store for %s must have CreateStrategy or UpdateStrategy set", e.DefaultQualifiedResource.String())
	}

	if e.DeleteStrategy == nil {
		return fmt.Errorf("store for %s must have DeleteStrategy set", e.DefaultQualifiedResource.String())
	}

	if options.RESTOptions == nil {
		return fmt.Errorf("options for %s must have RESTOptions set", e.DefaultQualifiedResource.String())
	}

	attrFunc := options.AttrFunc
	if attrFunc == nil {
		if isNamespaced {
			attrFunc = storage.DefaultNamespaceScopedAttr
		} else {
			attrFunc = storage.DefaultClusterScopedAttr
		}
	}
	if e.PredicateFunc == nil {
		e.PredicateFunc = func(label labels.Selector, field fields.Selector) storage.SelectionPredicate {
			return storage.SelectionPredicate{
				Label:    label,
				Field:    field,
				GetAttrs: attrFunc,
			}
		}
	}

	err := validateIndexers(options.Indexers)
	if err != nil {
		return err
	}

	opts, err := options.RESTOptions.GetRESTOptions(e.DefaultQualifiedResource, e.NewFunc())
	if err != nil {
		return err
	}

	// ResourcePrefix must come from the underlying factory
	prefix := opts.ResourcePrefix
	if !strings.HasPrefix(prefix, "/") {
		prefix = "/" + prefix
	}
	if prefix == "/" {
		return fmt.Errorf("store for %s has an invalid prefix %q", e.DefaultQualifiedResource.String(), opts.ResourcePrefix)
	}

	// Set the default behavior for storage key generation
	if e.KeyRootFunc == nil && e.KeyFunc == nil {
		if isNamespaced {
			e.KeyRootFunc = func(ctx context.Context) string {
				return NamespaceKeyRootFunc(ctx, prefix)
			}
			e.KeyFunc = func(ctx context.Context, name string) (string, error) {
				return NamespaceKeyFunc(ctx, prefix, name)
			}
		} else {
			e.KeyRootFunc = func(ctx context.Context) string {
				return prefix
			}
			e.KeyFunc = func(ctx context.Context, name string) (string, error) {
				return NoNamespaceKeyFunc(ctx, prefix, name)
			}
		}
	}

	// We adapt the store's keyFunc so that we can use it with the StorageDecorator
	// without making any assumptions about where objects are stored in etcd
	keyFunc := func(obj runtime.Object) (string, error) {
		accessor, err := meta.Accessor(obj)
		if err != nil {
			return "", err
		}

		if isNamespaced {
			return e.KeyFunc(genericapirequest.WithNamespace(genericapirequest.NewContext(), accessor.GetNamespace()), accessor.GetName())
		}

		return e.KeyFunc(genericapirequest.NewContext(), accessor.GetName())
	}

	if e.DeleteCollectionWorkers == 0 {
		e.DeleteCollectionWorkers = opts.DeleteCollectionWorkers
	}

	e.EnableGarbageCollection = opts.EnableGarbageCollection

	if e.ObjectNameFunc == nil {
		e.ObjectNameFunc = func(obj runtime.Object) (string, error) {
			accessor, err := meta.Accessor(obj)
			if err != nil {
				return "", err
			}
			return accessor.GetName(), nil
		}
	}

	if e.Storage.Storage == nil {
		e.Storage.Codec = opts.StorageConfig.Codec
		var err error
		e.Storage.Storage, e.DestroyFunc, err = opts.Decorator(
			opts.StorageConfig,
			prefix,
			keyFunc,
			e.NewFunc,
			e.NewListFunc,
			attrFunc,
			options.TriggerFunc,
			options.Indexers,
		)
		if err != nil {
			return err
		}
		e.StorageVersioner = opts.StorageConfig.EncodeVersioner

		if opts.CountMetricPollPeriod > 0 {
			stopFunc := e.startObservingCount(opts.CountMetricPollPeriod, opts.StorageObjectCountTracker)
			previousDestroy := e.DestroyFunc
			var once sync.Once
			e.DestroyFunc = func() {
				once.Do(func() {
					stopFunc()
					if previousDestroy != nil {
						previousDestroy()
					}
				})
			}
		}
	}
	if e.Storage.Storage != nil {
		e.ReadinessCheckFunc = e.Storage.Storage.ReadinessCheck
	}

	if utilfeature.DefaultFeatureGate.Enabled(features.AllowUnsafeMalformedObjectDeletion) {
		e.corruptObjDeleter = NewCorruptObjectDeleter(e)
	}

	return nil
}

// startObservingCount starts monitoring given prefix and periodically updating metrics. It returns a function to stop collection.
func (e *Store) startObservingCount(period time.Duration, objectCountTracker flowcontrolrequest.StorageObjectCountTracker) func() {
	prefix := e.KeyRootFunc(genericapirequest.NewContext())
	resourceName := e.DefaultQualifiedResource.String()
	klog.V(2).InfoS("Monitoring resource count at path", "resource", resourceName, "path", "<storage-prefix>/"+prefix)
	stopCh := make(chan struct{})
	go wait.JitterUntil(func() {
		count, err := e.Storage.Count(prefix)
		if err != nil {
			klog.V(5).InfoS("Failed to update storage count metric", "err", err)
			count = -1
		}

		metrics.UpdateObjectCount(resourceName, count)
		if objectCountTracker != nil {
			objectCountTracker.Set(resourceName, count)
		}
	}, period, resourceCountPollPeriodJitter, true, stopCh)
	return func() { close(stopCh) }
}

func (e *Store) ConvertToTable(ctx context.Context, object runtime.Object, tableOptions runtime.Object) (*metav1.Table, error) {
	if e.TableConvertor != nil {
		return e.TableConvertor.ConvertToTable(ctx, object, tableOptions)
	}
	return rest.NewDefaultTableConvertor(e.DefaultQualifiedResource).ConvertToTable(ctx, object, tableOptions)
}

func (e *Store) StorageVersion() runtime.GroupVersioner {
	return e.StorageVersioner
}

// GetResetFields implements rest.ResetFieldsStrategy
func (e *Store) GetResetFields() map[fieldpath.APIVersion]*fieldpath.Set {
	if e.ResetFieldsStrategy == nil {
		return nil
	}
	return e.ResetFieldsStrategy.GetResetFields()
}

func (e *Store) GetSingularName() string {
	return e.SingularQualifiedResource.Resource
}

// validateIndexers will check the prefix of indexers.
func validateIndexers(indexers *cache.Indexers) error {
	if indexers == nil {
		return nil
	}
	for indexName := range *indexers {
		if len(indexName) <= 2 || (indexName[:2] != "l:" && indexName[:2] != "f:") {
			return fmt.Errorf("index must prefix with \"l:\" or \"f:\"")
		}
	}
	return nil
}
