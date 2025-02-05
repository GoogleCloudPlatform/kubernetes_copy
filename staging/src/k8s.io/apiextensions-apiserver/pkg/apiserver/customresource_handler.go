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

package apiserver

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"sigs.k8s.io/structured-merge-diff/v4/fieldpath"

	apiextensionshelpers "k8s.io/apiextensions-apiserver/pkg/apihelpers"
	apiextensionsinternal "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/conversion"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	structuraldefaulting "k8s.io/apiextensions-apiserver/pkg/apiserver/schema/defaulting"
	schemaobjectmeta "k8s.io/apiextensions-apiserver/pkg/apiserver/schema/objectmeta"
	structuralpruning "k8s.io/apiextensions-apiserver/pkg/apiserver/schema/pruning"
	apiservervalidation "k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	informers "k8s.io/apiextensions-apiserver/pkg/client/informers/externalversions/apiextensions/v1"
	listers "k8s.io/apiextensions-apiserver/pkg/client/listers/apiextensions/v1"
	"k8s.io/apiextensions-apiserver/pkg/controller/establish"
	"k8s.io/apiextensions-apiserver/pkg/controller/finalizer"
	"k8s.io/apiextensions-apiserver/pkg/controller/openapi/builder"
	"k8s.io/apiextensions-apiserver/pkg/crdserverscheme"
	"k8s.io/apiextensions-apiserver/pkg/registry/customresource"
	"k8s.io/apiextensions-apiserver/pkg/registry/customresource/tableconvertor"

	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/runtime/serializer/cbor"
	"k8s.io/apimachinery/pkg/runtime/serializer/json"
	"k8s.io/apimachinery/pkg/runtime/serializer/protobuf"
	"k8s.io/apimachinery/pkg/runtime/serializer/versioning"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/managedfields"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	utilwaitgroup "k8s.io/apimachinery/pkg/util/waitgroup"
	"k8s.io/apimachinery/pkg/version"
	"k8s.io/apiserver/pkg/admission"
	"k8s.io/apiserver/pkg/authorization/authorizer"
	"k8s.io/apiserver/pkg/endpoints/handlers"
	"k8s.io/apiserver/pkg/endpoints/handlers/responsewriters"
	"k8s.io/apiserver/pkg/endpoints/metrics"
	apirequest "k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/features"
	"k8s.io/apiserver/pkg/registry/generic"
	genericfilters "k8s.io/apiserver/pkg/server/filters"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/apiserver/pkg/util/webhook"
	"k8s.io/apiserver/pkg/warning"
	"k8s.io/client-go/scale"
	"k8s.io/client-go/scale/scheme/autoscalingv1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	"k8s.io/kube-openapi/pkg/spec3"
	"k8s.io/kube-openapi/pkg/validation/spec"
)

// crdHandler serves the `/apis` endpoint.
// This is registered as a filter so that it never collides with any explicitly registered endpoints
type crdHandler struct {
	versionDiscoveryHandler *versionDiscoveryHandler
	groupDiscoveryHandler   *groupDiscoveryHandler

	customStorageLock sync.Mutex
	// customStorage contains a crdStorageMap
	// atomic.Value has a very good read performance compared to sync.RWMutex
	// see https://gist.github.com/dim/152e6bf80e1384ea72e17ac717a5000a
	// which is suited for most read and rarely write cases
	customStorage atomic.Value

	crdLister listers.CustomResourceDefinitionLister

	delegate          http.Handler
	restOptionsGetter generic.RESTOptionsGetter
	admission         admission.Interface

	establishingController *establish.EstablishingController

	// MasterCount is used to implement sleep to improve
	// CRD establishing process for HA clusters.
	masterCount int

	converterFactory *conversion.CRConverterFactory

	// so that we can do create on update.
	authorizer authorizer.Authorizer

	// request timeout we should delay storage teardown for
	requestTimeout time.Duration

	// minRequestTimeout applies to CR's list/watch calls
	minRequestTimeout time.Duration

	// staticOpenAPISpec is used as a base for the schema of CR's for the
	// purpose of managing fields, it is how CR handlers get the structure
	// of TypeMeta and ObjectMeta
	staticOpenAPISpec map[string]*spec.Schema

	// The limit on the request size that would be accepted and decoded in a write request
	// 0 means no limit.
	maxRequestBodyBytes int64
}

// crdInfo stores enough information to serve the storage for the custom resource
type crdInfo struct {
	// spec and acceptedNames are used to compare against if a change is made on a CRD. We only update
	// the storage if one of these changes.
	spec          *apiextensionsv1.CustomResourceDefinitionSpec
	acceptedNames *apiextensionsv1.CustomResourceDefinitionNames

	// Deprecated per version
	deprecated map[string]bool

	// Warnings per version
	warnings map[string][]string

	// Storage per version
	storages map[string]customresource.CustomResourceStorage

	// Request scope per version
	requestScopes map[string]*handlers.RequestScope

	// Scale scope per version
	scaleRequestScopes map[string]*handlers.RequestScope

	// Status scope per version
	statusRequestScopes map[string]*handlers.RequestScope

	// storageVersion is the CRD version used when storing the object in etcd.
	storageVersion string

	waitGroup *utilwaitgroup.SafeWaitGroup
}

// crdStorageMap goes from customresourcedefinition to its storage
type crdStorageMap map[types.UID]*crdInfo

func NewCustomResourceDefinitionHandler(
	versionDiscoveryHandler *versionDiscoveryHandler,
	groupDiscoveryHandler *groupDiscoveryHandler,
	crdInformer informers.CustomResourceDefinitionInformer,
	delegate http.Handler,
	restOptionsGetter generic.RESTOptionsGetter,
	admission admission.Interface,
	establishingController *establish.EstablishingController,
	serviceResolver webhook.ServiceResolver,
	authResolverWrapper webhook.AuthenticationInfoResolverWrapper,
	masterCount int,
	authorizer authorizer.Authorizer,
	requestTimeout time.Duration,
	minRequestTimeout time.Duration,
	staticOpenAPISpec map[string]*spec.Schema,
	maxRequestBodyBytes int64) (*crdHandler, error) {
	ret := &crdHandler{
		versionDiscoveryHandler: versionDiscoveryHandler,
		groupDiscoveryHandler:   groupDiscoveryHandler,
		customStorage:           atomic.Value{},
		crdLister:               crdInformer.Lister(),
		delegate:                delegate,
		restOptionsGetter:       restOptionsGetter,
		admission:               admission,
		establishingController:  establishingController,
		masterCount:             masterCount,
		authorizer:              authorizer,
		requestTimeout:          requestTimeout,
		minRequestTimeout:       minRequestTimeout,
		staticOpenAPISpec:       staticOpenAPISpec,
		maxRequestBodyBytes:     maxRequestBodyBytes,
	}
	crdInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    ret.createCustomResourceDefinition,
		UpdateFunc: ret.updateCustomResourceDefinition,
		DeleteFunc: func(obj interface{}) {
			ret.removeDeadStorage()
		},
	})
	crConverterFactory, err := conversion.NewCRConverterFactory(serviceResolver, authResolverWrapper)
	if err != nil {
		return nil, err
	}
	ret.converterFactory = crConverterFactory

	ret.customStorage.Store(crdStorageMap{})

	return ret, nil
}

// watches are expected to handle storage disruption gracefully,
// both on the server-side (by terminating the watch connection)
// and on the client side (by restarting the watch)
var longRunningFilter = genericfilters.BasicLongRunningRequestCheck(sets.NewString("watch"), sets.NewString())

// possiblyAcrossAllNamespacesVerbs contains those verbs which can be per-namespace and across all
// namespaces for namespaces resources. I.e. for these an empty namespace in the requestInfo is fine.
var possiblyAcrossAllNamespacesVerbs = sets.NewString("list", "watch")

func (r *crdHandler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	requestInfo, ok := apirequest.RequestInfoFrom(ctx)
	if !ok {
		responsewriters.ErrorNegotiated(
			apierrors.NewInternalError(fmt.Errorf("no RequestInfo found in the context")),
			Codecs, schema.GroupVersion{}, w, req,
		)
		return
	}
	if !requestInfo.IsResourceRequest {
		pathParts := splitPath(requestInfo.Path)
		// only match /apis/<group>/<version>
		// only registered under /apis
		if len(pathParts) == 3 {
			r.versionDiscoveryHandler.ServeHTTP(w, req)
			return
		}
		// only match /apis/<group>
		if len(pathParts) == 2 {
			r.groupDiscoveryHandler.ServeHTTP(w, req)
			return
		}

		r.delegate.ServeHTTP(w, req)
		return
	}

	crdName := requestInfo.Resource + "." + requestInfo.APIGroup
	crd, err := r.crdLister.Get(crdName)
	if apierrors.IsNotFound(err) {
		r.delegate.ServeHTTP(w, req)
		return
	}
	if err != nil {
		utilruntime.HandleError(err)
		responsewriters.ErrorNegotiated(
			apierrors.NewInternalError(fmt.Errorf("error resolving resource")),
			Codecs, schema.GroupVersion{Group: requestInfo.APIGroup, Version: requestInfo.APIVersion}, w, req,
		)
		return
	}

	// if the scope in the CRD and the scope in request differ (with exception of the verbs in possiblyAcrossAllNamespacesVerbs
	// for namespaced resources), pass request to the delegate, which is supposed to lead to a 404.
	namespacedCRD, namespacedReq := crd.Spec.Scope == apiextensionsv1.NamespaceScoped, len(requestInfo.Namespace) > 0
	if !namespacedCRD && namespacedReq {
		r.delegate.ServeHTTP(w, req)
		return
	}
	if namespacedCRD && !namespacedReq && !possiblyAcrossAllNamespacesVerbs.Has(requestInfo.Verb) {
		r.delegate.ServeHTTP(w, req)
		return
	}

	if !apiextensionshelpers.HasServedCRDVersion(crd, requestInfo.APIVersion) {
		r.delegate.ServeHTTP(w, req)
		return
	}

	// There is a small chance that a CRD is being served because NamesAccepted condition is true,
	// but it becomes "unserved" because another names update leads to a conflict
	// and EstablishingController wasn't fast enough to put the CRD into the Established condition.
	// We accept this as the problem is small and self-healing.
	if !apiextensionshelpers.IsCRDConditionTrue(crd, apiextensionsv1.NamesAccepted) &&
		!apiextensionshelpers.IsCRDConditionTrue(crd, apiextensionsv1.Established) {
		r.delegate.ServeHTTP(w, req)
		return
	}

	terminating := apiextensionshelpers.IsCRDConditionTrue(crd, apiextensionsv1.Terminating)

	crdInfo, err := r.getOrCreateServingInfoFor(crd.UID, crd.Name)
	if apierrors.IsNotFound(err) {
		r.delegate.ServeHTTP(w, req)
		return
	}
	if err != nil {
		utilruntime.HandleError(err)
		responsewriters.ErrorNegotiated(
			apierrors.NewInternalError(fmt.Errorf("error resolving resource")),
			Codecs, schema.GroupVersion{Group: requestInfo.APIGroup, Version: requestInfo.APIVersion}, w, req,
		)
		return
	}
	if !hasServedCRDVersion(crdInfo.spec, requestInfo.APIVersion) {
		r.delegate.ServeHTTP(w, req)
		return
	}

	deprecated := crdInfo.deprecated[requestInfo.APIVersion]
	for _, w := range crdInfo.warnings[requestInfo.APIVersion] {
		warning.AddWarning(req.Context(), "", w)
	}

	verb := strings.ToUpper(requestInfo.Verb)
	resource := requestInfo.Resource
	subresource := requestInfo.Subresource
	scope := metrics.CleanScope(requestInfo)
	supportedTypes := []string{
		string(types.JSONPatchType),
		string(types.MergePatchType),
		string(types.ApplyYAMLPatchType),
	}
	if utilfeature.DefaultFeatureGate.Enabled(features.CBORServingAndStorage) {
		supportedTypes = append(supportedTypes, string(types.ApplyCBORPatchType))
	}

	var handlerFunc http.HandlerFunc
	subresources, err := apiextensionshelpers.GetSubresourcesForVersion(crd, requestInfo.APIVersion)
	if err != nil {
		utilruntime.HandleError(err)
		responsewriters.ErrorNegotiated(
			apierrors.NewInternalError(fmt.Errorf("could not properly serve the subresource")),
			Codecs, schema.GroupVersion{Group: requestInfo.APIGroup, Version: requestInfo.APIVersion}, w, req,
		)
		return
	}
	switch {
	case subresource == "status" && subresources != nil && subresources.Status != nil:
		handlerFunc = r.serveStatus(w, req, requestInfo, crdInfo, terminating, supportedTypes)
	case subresource == "scale" && subresources != nil && subresources.Scale != nil:
		handlerFunc = r.serveScale(w, req, requestInfo, crdInfo, terminating, supportedTypes)
	case len(subresource) == 0:
		handlerFunc = r.serveResource(w, req, requestInfo, crdInfo, crd, terminating, supportedTypes)
	default:
		responsewriters.ErrorNegotiated(
			apierrors.NewNotFound(schema.GroupResource{Group: requestInfo.APIGroup, Resource: requestInfo.Resource}, requestInfo.Name),
			Codecs, schema.GroupVersion{Group: requestInfo.APIGroup, Version: requestInfo.APIVersion}, w, req,
		)
	}

	if handlerFunc != nil {
		handlerFunc = metrics.InstrumentHandlerFunc(verb, requestInfo.APIGroup, requestInfo.APIVersion, resource, subresource, scope, metrics.APIServerComponent, deprecated, "", handlerFunc)
		handler := genericfilters.WithWaitGroup(handlerFunc, longRunningFilter, crdInfo.waitGroup)
		handler.ServeHTTP(w, req)
		return
	}
}

func (r *crdHandler) serveResource(w http.ResponseWriter, req *http.Request, requestInfo *apirequest.RequestInfo, crdInfo *crdInfo, crd *apiextensionsv1.CustomResourceDefinition, terminating bool, supportedTypes []string) http.HandlerFunc {
	requestScope := crdInfo.requestScopes[requestInfo.APIVersion]
	storage := crdInfo.storages[requestInfo.APIVersion].CustomResource

	switch requestInfo.Verb {
	case "get":
		return handlers.GetResource(storage, requestScope)
	case "list":
		forceWatch := false
		return handlers.ListResource(storage, storage, requestScope, forceWatch, r.minRequestTimeout)
	case "watch":
		forceWatch := true
		return handlers.ListResource(storage, storage, requestScope, forceWatch, r.minRequestTimeout)
	case "create":
		// we want to track recently created CRDs so that in HA environments we don't have server A allow a create and server B
		// not have observed the established, so a followup get,update,delete results in a 404. We've observed about 800ms
		// delay in some CI environments.  Two seconds looks long enough and reasonably short for hot retriers.
		justCreated := time.Since(apiextensionshelpers.FindCRDCondition(crd, apiextensionsv1.Established).LastTransitionTime.Time) < 2*time.Second
		if justCreated {
			time.Sleep(2 * time.Second)
		}
		if terminating {
			err := apierrors.NewMethodNotSupported(schema.GroupResource{Group: requestInfo.APIGroup, Resource: requestInfo.Resource}, requestInfo.Verb)
			err.ErrStatus.Message = fmt.Sprintf("%v not allowed while custom resource definition is terminating", requestInfo.Verb)
			responsewriters.ErrorNegotiated(err, Codecs, schema.GroupVersion{Group: requestInfo.APIGroup, Version: requestInfo.APIVersion}, w, req)
			return nil
		}
		return handlers.CreateResource(storage, requestScope, r.admission)
	case "update":
		return handlers.UpdateResource(storage, requestScope, r.admission)
	case "patch":
		return handlers.PatchResource(storage, requestScope, r.admission, supportedTypes)
	case "delete":
		allowsOptions := true
		return handlers.DeleteResource(storage, allowsOptions, requestScope, r.admission)
	case "deletecollection":
		checkBody := true
		return handlers.DeleteCollection(storage, checkBody, requestScope, r.admission)
	default:
		responsewriters.ErrorNegotiated(
			apierrors.NewMethodNotSupported(schema.GroupResource{Group: requestInfo.APIGroup, Resource: requestInfo.Resource}, requestInfo.Verb),
			Codecs, schema.GroupVersion{Group: requestInfo.APIGroup, Version: requestInfo.APIVersion}, w, req,
		)
		return nil
	}
}

func (r *crdHandler) serveStatus(w http.ResponseWriter, req *http.Request, requestInfo *apirequest.RequestInfo, crdInfo *crdInfo, terminating bool, supportedTypes []string) http.HandlerFunc {
	requestScope := crdInfo.statusRequestScopes[requestInfo.APIVersion]
	storage := crdInfo.storages[requestInfo.APIVersion].Status

	switch requestInfo.Verb {
	case "get":
		return handlers.GetResource(storage, requestScope)
	case "update":
		return handlers.UpdateResource(storage, requestScope, r.admission)
	case "patch":
		return handlers.PatchResource(storage, requestScope, r.admission, supportedTypes)
	default:
		responsewriters.ErrorNegotiated(
			apierrors.NewMethodNotSupported(schema.GroupResource{Group: requestInfo.APIGroup, Resource: requestInfo.Resource}, requestInfo.Verb),
			Codecs, schema.GroupVersion{Group: requestInfo.APIGroup, Version: requestInfo.APIVersion}, w, req,
		)
		return nil
	}
}

func (r *crdHandler) serveScale(w http.ResponseWriter, req *http.Request, requestInfo *apirequest.RequestInfo, crdInfo *crdInfo, terminating bool, supportedTypes []string) http.HandlerFunc {
	requestScope := crdInfo.scaleRequestScopes[requestInfo.APIVersion]
	storage := crdInfo.storages[requestInfo.APIVersion].Scale

	switch requestInfo.Verb {
	case "get":
		return handlers.GetResource(storage, requestScope)
	case "update":
		return handlers.UpdateResource(storage, requestScope, r.admission)
	case "patch":
		return handlers.PatchResource(storage, requestScope, r.admission, supportedTypes)
	default:
		responsewriters.ErrorNegotiated(
			apierrors.NewMethodNotSupported(schema.GroupResource{Group: requestInfo.APIGroup, Resource: requestInfo.Resource}, requestInfo.Verb),
			Codecs, schema.GroupVersion{Group: requestInfo.APIGroup, Version: requestInfo.APIVersion}, w, req,
		)
		return nil
	}
}

// createCustomResourceDefinition removes potentially stale storage so it gets re-created
func (r *crdHandler) createCustomResourceDefinition(obj interface{}) {
	crd := obj.(*apiextensionsv1.CustomResourceDefinition)
	r.customStorageLock.Lock()
	defer r.customStorageLock.Unlock()
	// this could happen if the create event is merged from create-update events
	storageMap := r.customStorage.Load().(crdStorageMap)
	oldInfo, found := storageMap[crd.UID]
	if !found {
		return
	}
	if apiequality.Semantic.DeepEqual(&crd.Spec, oldInfo.spec) && apiequality.Semantic.DeepEqual(&crd.Status.AcceptedNames, oldInfo.acceptedNames) {
		klog.V(6).Infof("Ignoring customresourcedefinition %s create event because a storage with the same spec and accepted names exists",
			crd.Name)
		return
	}
	r.removeStorage_locked(crd.UID)
}

// updateCustomResourceDefinition removes potentially stale storage so it gets re-created
func (r *crdHandler) updateCustomResourceDefinition(oldObj, newObj interface{}) {
	oldCRD := oldObj.(*apiextensionsv1.CustomResourceDefinition)
	newCRD := newObj.(*apiextensionsv1.CustomResourceDefinition)

	r.customStorageLock.Lock()
	defer r.customStorageLock.Unlock()

	// Add CRD to the establishing controller queue.
	// For HA clusters, we want to prevent race conditions when changing status to Established,
	// so we want to be sure that CRD is Installing at least for 5 seconds before Establishing it.
	// TODO: find a real HA safe checkpointing mechanism instead of an arbitrary wait.
	if !apiextensionshelpers.IsCRDConditionTrue(newCRD, apiextensionsv1.Established) &&
		apiextensionshelpers.IsCRDConditionTrue(newCRD, apiextensionsv1.NamesAccepted) {
		if r.masterCount > 1 {
			r.establishingController.QueueCRD(newCRD.Name, 5*time.Second)
		} else {
			r.establishingController.QueueCRD(newCRD.Name, 0)
		}
	}

	if oldCRD.UID != newCRD.UID {
		r.removeStorage_locked(oldCRD.UID)
	}

	storageMap := r.customStorage.Load().(crdStorageMap)
	oldInfo, found := storageMap[newCRD.UID]
	if !found {
		return
	}
	if apiequality.Semantic.DeepEqual(&newCRD.Spec, oldInfo.spec) && apiequality.Semantic.DeepEqual(&newCRD.Status.AcceptedNames, oldInfo.acceptedNames) {
		klog.V(6).Infof("Ignoring customresourcedefinition %s update because neither spec, nor accepted names changed", oldCRD.Name)
		return
	}

	klog.V(4).Infof("Updating customresourcedefinition %s", newCRD.Name)
	r.removeStorage_locked(newCRD.UID)
}

// removeStorage_locked removes the cached storage with the given uid as key from the storage map. This function
// updates r.customStorage with the cleaned-up storageMap and tears down the old storage.
// NOTE: Caller MUST hold r.customStorageLock to write r.customStorage thread-safely.
func (r *crdHandler) removeStorage_locked(uid types.UID) {
	storageMap := r.customStorage.Load().(crdStorageMap)
	if oldInfo, ok := storageMap[uid]; ok {
		// Copy because we cannot write to storageMap without a race
		// as it is used without locking elsewhere.
		storageMap2 := storageMap.clone()

		// Remove from the CRD info map and store the map
		delete(storageMap2, uid)
		r.customStorage.Store(storageMap2)

		// Tear down the old storage
		go r.tearDown(oldInfo)
	}
}

// removeDeadStorage removes REST storage that isn't being used
func (r *crdHandler) removeDeadStorage() {
	allCustomResourceDefinitions, err := r.crdLister.List(labels.Everything())
	if err != nil {
		utilruntime.HandleError(err)
		return
	}

	r.customStorageLock.Lock()
	defer r.customStorageLock.Unlock()

	storageMap := r.customStorage.Load().(crdStorageMap)
	// Copy because we cannot write to storageMap without a race
	storageMap2 := make(crdStorageMap)
	for _, crd := range allCustomResourceDefinitions {
		if _, ok := storageMap[crd.UID]; ok {
			storageMap2[crd.UID] = storageMap[crd.UID]
		}
	}
	r.customStorage.Store(storageMap2)

	for uid, crdInfo := range storageMap {
		if _, ok := storageMap2[uid]; !ok {
			klog.V(4).Infof("Removing dead CRD storage for %s/%s", crdInfo.spec.Group, crdInfo.spec.Names.Kind)
			go r.tearDown(crdInfo)
		}
	}
}

// Wait up to a minute for requests to drain, then tear down storage
func (r *crdHandler) tearDown(oldInfo *crdInfo) {
	requestsDrained := make(chan struct{})
	go func() {
		defer close(requestsDrained)
		// Allow time for in-flight requests with a handle to the old info to register themselves
		time.Sleep(time.Second)
		// Wait for in-flight requests to drain
		oldInfo.waitGroup.Wait()
	}()

	select {
	case <-time.After(r.requestTimeout * 2):
		klog.Warningf("timeout waiting for requests to drain for %s/%s, tearing down storage", oldInfo.spec.Group, oldInfo.spec.Names.Kind)
	case <-requestsDrained:
	}

	for _, storage := range oldInfo.storages {
		// destroy only the main storage. Those for the subresources share cacher and etcd clients.
		storage.CustomResource.DestroyFunc()
	}
}

// Destroy shuts down storage layer for all registered CRDs.
// It should be called as a last step of the shutdown sequence.
func (r *crdHandler) destroy() {
	r.customStorageLock.Lock()
	defer r.customStorageLock.Unlock()

	storageMap := r.customStorage.Load().(crdStorageMap)
	for _, crdInfo := range storageMap {
		for _, storage := range crdInfo.storages {
			// DestroyFunc have to be implemented in idempotent way,
			// so the potential race with r.tearDown() (being called
			// from a goroutine) is safe.
			storage.CustomResource.DestroyFunc()
		}
	}
}

// GetCustomResourceListerCollectionDeleter returns the ListerCollectionDeleter of
// the given crd.
func (r *crdHandler) GetCustomResourceListerCollectionDeleter(crd *apiextensionsv1.CustomResourceDefinition) (finalizer.ListerCollectionDeleter, error) {
	info, err := r.getOrCreateServingInfoFor(crd.UID, crd.Name)
	if err != nil {
		return nil, err
	}
	return info.storages[info.storageVersion].CustomResource, nil
}

// getOrCreateServingInfoFor gets the CRD serving info for the given CRD UID if the key exists in the storage map.
// Otherwise the function fetches the up-to-date CRD using the given CRD name and creates CRD serving info.
func (r *crdHandler) getOrCreateServingInfoFor(uid types.UID, name string) (*crdInfo, error) {
	storageMap := r.customStorage.Load().(crdStorageMap)
	if ret, ok := storageMap[uid]; ok {
		return ret, nil
	}

	r.customStorageLock.Lock()
	defer r.customStorageLock.Unlock()

	// Get the up-to-date CRD when we have the lock, to avoid racing with updateCustomResourceDefinition.
	// If updateCustomResourceDefinition sees an update and happens later, the storage will be deleted and
	// we will re-create the updated storage on demand. If updateCustomResourceDefinition happens before,
	// we make sure that we observe the same up-to-date CRD.
	crd, err := r.crdLister.Get(name)
	if err != nil {
		return nil, err
	}
	storageMap = r.customStorage.Load().(crdStorageMap)
	if ret, ok := storageMap[crd.UID]; ok {
		return ret, nil
	}

	storageVersion, err := apiextensionshelpers.GetCRDStorageVersion(crd)
	if err != nil {
		return nil, err
	}

	// Scope/Storages per version.
	requestScopes := map[string]*handlers.RequestScope{}
	storages := map[string]customresource.CustomResourceStorage{}
	statusScopes := map[string]*handlers.RequestScope{}
	scaleScopes := map[string]*handlers.RequestScope{}
	deprecated := map[string]bool{}
	warnings := map[string][]string{}

	equivalentResourceRegistry := runtime.NewEquivalentResourceRegistry()

	structuralSchemas := map[string]*structuralschema.Structural{}
	for _, v := range crd.Spec.Versions {
		val, err := apiextensionshelpers.GetSchemaForVersion(crd, v.Name)
		if err != nil {
			utilruntime.HandleError(err)
			return nil, fmt.Errorf("the server could not properly serve the CR schema")
		}
		if val == nil {
			continue
		}
		internalValidation := &apiextensionsinternal.CustomResourceValidation{}
		if err := apiextensionsv1.Convert_v1_CustomResourceValidation_To_apiextensions_CustomResourceValidation(val, internalValidation, nil); err != nil {
			return nil, fmt.Errorf("failed converting CRD validation to internal version: %v", err)
		}
		s, err := structuralschema.NewStructural(internalValidation.OpenAPIV3Schema)
		if !crd.Spec.PreserveUnknownFields && err != nil {
			// This should never happen. If it does, it is a programming error.
			utilruntime.HandleError(fmt.Errorf("failed to convert schema to structural: %v", err))
			return nil, fmt.Errorf("the server could not properly serve the CR schema") // validation should avoid this
		}

		if !crd.Spec.PreserveUnknownFields {
			// we don't own s completely, e.g. defaults are not deep-copied. So better make a copy here.
			s = s.DeepCopy()

			if err := structuraldefaulting.PruneDefaults(s); err != nil {
				// This should never happen. If it does, it is a programming error.
				utilruntime.HandleError(fmt.Errorf("failed to prune defaults: %v", err))
				return nil, fmt.Errorf("the server could not properly serve the CR schema") // validation should avoid this
			}
		}
		structuralSchemas[v.Name] = s
	}

	openAPIModels, err := buildOpenAPIModelsForApply(r.staticOpenAPISpec, crd)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("error building openapi models for %s: %v", crd.Name, err))
		openAPIModels = nil
	}

	var typeConverter managedfields.TypeConverter = managedfields.NewDeducedTypeConverter()
	if len(openAPIModels) > 0 {
		typeConverter, err = managedfields.NewTypeConverter(openAPIModels, crd.Spec.PreserveUnknownFields)
		if err != nil {
			return nil, err
		}
	}

	safeConverter, unsafeConverter, err := r.converterFactory.NewConverter(crd)
	if err != nil {
		return nil, err
	}

	// Create replicasPathInCustomResource
	replicasPathInCustomResource := managedfields.ResourcePathMappings{}
	for _, v := range crd.Spec.Versions {
		subresources, err := apiextensionshelpers.GetSubresourcesForVersion(crd, v.Name)
		if err != nil {
			utilruntime.HandleError(err)
			return nil, fmt.Errorf("the server could not properly serve the CR subresources")
		}
		if subresources == nil || subresources.Scale == nil {
			replicasPathInCustomResource[schema.GroupVersion{Group: crd.Spec.Group, Version: v.Name}.String()] = nil
			continue
		}
		path := fieldpath.Path{}
		splitReplicasPath := strings.Split(strings.TrimPrefix(subresources.Scale.SpecReplicasPath, "."), ".")
		for _, element := range splitReplicasPath {
			s := element
			path = append(path, fieldpath.PathElement{FieldName: &s})
		}
		replicasPathInCustomResource[schema.GroupVersion{Group: crd.Spec.Group, Version: v.Name}.String()] = path
	}

	for _, v := range crd.Spec.Versions {
		// In addition to Unstructured objects (Custom Resources), we also may sometimes need to
		// decode unversioned Options objects, so we delegate to parameterScheme for such types.
		parameterScheme := runtime.NewScheme()
		parameterScheme.AddUnversionedTypes(schema.GroupVersion{Group: crd.Spec.Group, Version: v.Name},
			&metav1.ListOptions{},
			&metav1.GetOptions{},
			&metav1.DeleteOptions{},
		)
		parameterCodec := runtime.NewParameterCodec(parameterScheme)

		resource := schema.GroupVersionResource{Group: crd.Spec.Group, Version: v.Name, Resource: crd.Status.AcceptedNames.Plural}
		if len(resource.Resource) == 0 {
			utilruntime.HandleError(fmt.Errorf("CustomResourceDefinition %s has unexpected empty status.acceptedNames.plural", crd.Name))
			return nil, fmt.Errorf("the server could not properly serve the resource")
		}
		singularResource := schema.GroupVersionResource{Group: crd.Spec.Group, Version: v.Name, Resource: crd.Status.AcceptedNames.Singular}
		if len(singularResource.Resource) == 0 {
			utilruntime.HandleError(fmt.Errorf("CustomResourceDefinition %s has unexpected empty status.acceptedNames.singular", crd.Name))
			return nil, fmt.Errorf("the server could not properly serve the resource")
		}
		kind := schema.GroupVersionKind{Group: crd.Spec.Group, Version: v.Name, Kind: crd.Status.AcceptedNames.Kind}
		if len(kind.Kind) == 0 {
			utilruntime.HandleError(fmt.Errorf("CustomResourceDefinition %s has unexpected empty status.acceptedNames.kind", crd.Name))
			return nil, fmt.Errorf("the server could not properly serve the kind")
		}
		equivalentResourceRegistry.RegisterKindFor(resource, "", kind)

		typer := newUnstructuredObjectTyper(parameterScheme)
		creator := unstructuredCreator{}

		validationSchema, err := apiextensionshelpers.GetSchemaForVersion(crd, v.Name)
		if err != nil {
			utilruntime.HandleError(err)
			return nil, fmt.Errorf("the server could not properly serve the CR schema")
		}
		var internalSchemaProps *apiextensionsinternal.JSONSchemaProps
		var internalValidationSchema *apiextensionsinternal.CustomResourceValidation
		if validationSchema != nil {
			internalValidationSchema = &apiextensionsinternal.CustomResourceValidation{}
			if err := apiextensionsv1.Convert_v1_CustomResourceValidation_To_apiextensions_CustomResourceValidation(validationSchema, internalValidationSchema, nil); err != nil {
				return nil, fmt.Errorf("failed to convert CRD validation to internal version: %v", err)
			}
			internalSchemaProps = internalValidationSchema.OpenAPIV3Schema
		}
		validator, _, err := apiservervalidation.NewSchemaValidator(internalSchemaProps)
		if err != nil {
			return nil, err
		}

		var statusSpec *apiextensionsinternal.CustomResourceSubresourceStatus
		var statusValidator apiservervalidation.SchemaValidator
		subresources, err := apiextensionshelpers.GetSubresourcesForVersion(crd, v.Name)
		if err != nil {
			utilruntime.HandleError(err)
			return nil, fmt.Errorf("the server could not properly serve the CR subresources")
		}
		if subresources != nil && subresources.Status != nil {
			equivalentResourceRegistry.RegisterKindFor(resource, "status", kind)
			statusSpec = &apiextensionsinternal.CustomResourceSubresourceStatus{}
			if err := apiextensionsv1.Convert_v1_CustomResourceSubresourceStatus_To_apiextensions_CustomResourceSubresourceStatus(subresources.Status, statusSpec, nil); err != nil {
				return nil, fmt.Errorf("failed converting CRD status subresource to internal version: %v", err)
			}
			// for the status subresource, validate only against the status schema
			if internalValidationSchema != nil && internalValidationSchema.OpenAPIV3Schema != nil && internalValidationSchema.OpenAPIV3Schema.Properties != nil {
				if statusSchema, ok := internalValidationSchema.OpenAPIV3Schema.Properties["status"]; ok {
					statusValidator, _, err = apiservervalidation.NewSchemaValidator(&statusSchema)
					if err != nil {
						return nil, err
					}
				}
			}
		}

		var scaleSpec *apiextensionsinternal.CustomResourceSubresourceScale
		if subresources != nil && subresources.Scale != nil {
			equivalentResourceRegistry.RegisterKindFor(resource, "scale", autoscalingv1.SchemeGroupVersion.WithKind("Scale"))
			scaleSpec = &apiextensionsinternal.CustomResourceSubresourceScale{}
			if err := apiextensionsv1.Convert_v1_CustomResourceSubresourceScale_To_apiextensions_CustomResourceSubresourceScale(subresources.Scale, scaleSpec, nil); err != nil {
				return nil, fmt.Errorf("failed converting CRD status subresource to internal version: %v", err)
			}
		}

		columns, err := getColumnsForVersion(crd, v.Name)
		if err != nil {
			utilruntime.HandleError(err)
			return nil, fmt.Errorf("the server could not properly serve the CR columns")
		}
		table, err := tableconvertor.New(columns)
		if err != nil {
			klog.V(2).Infof("The CRD for %v has an invalid printer specification, falling back to default printing: %v", kind, err)
		}

		listKind := schema.GroupVersionKind{Group: crd.Spec.Group, Version: v.Name, Kind: crd.Status.AcceptedNames.ListKind}
		if len(listKind.Kind) == 0 {
			utilruntime.HandleError(fmt.Errorf("CustomResourceDefinition %s has unexpected empty status.acceptedNames.listKind", crd.Name))
			return nil, fmt.Errorf("the server could not properly serve the list kind")
		}

		storages[v.Name] = customresource.NewStorage(
			resource.GroupResource(),
			singularResource.GroupResource(),
			kind,
			listKind,
			customresource.NewStrategy(
				typer,
				crd.Spec.Scope == apiextensionsv1.NamespaceScoped,
				kind,
				validator,
				statusValidator,
				structuralSchemas[v.Name],
				statusSpec,
				scaleSpec,
				v.SelectableFields,
			),
			crdConversionRESTOptionsGetter{
				RESTOptionsGetter:     r.restOptionsGetter,
				converter:             safeConverter,
				decoderVersion:        schema.GroupVersion{Group: crd.Spec.Group, Version: v.Name},
				encoderVersion:        schema.GroupVersion{Group: crd.Spec.Group, Version: storageVersion},
				structuralSchemas:     structuralSchemas,
				structuralSchemaGK:    kind.GroupKind(),
				preserveUnknownFields: crd.Spec.PreserveUnknownFields,
			},
			crd.Status.AcceptedNames.Categories,
			table,
			replicasPathInCustomResource,
		)

		clusterScoped := crd.Spec.Scope == apiextensionsv1.ClusterScoped

		// CRDs explicitly do not support protobuf, but some objects returned by the API server do
		streamingCollections := utilfeature.DefaultFeatureGate.Enabled(features.StreamingCollectionEncodingToJSON)
		negotiatedSerializer := unstructuredNegotiatedSerializer{
			typer:                 typer,
			creator:               creator,
			converter:             safeConverter,
			structuralSchemas:     structuralSchemas,
			structuralSchemaGK:    kind.GroupKind(),
			preserveUnknownFields: crd.Spec.PreserveUnknownFields,
			supportedMediaTypes: []runtime.SerializerInfo{
				{
					MediaType:        "application/json",
					MediaTypeType:    "application",
					MediaTypeSubType: "json",
					EncodesAsText:    true,
					Serializer:       json.NewSerializerWithOptions(json.DefaultMetaFactory, creator, typer, json.SerializerOptions{StreamingCollectionsEncoding: streamingCollections}),
					PrettySerializer: json.NewSerializerWithOptions(json.DefaultMetaFactory, creator, typer, json.SerializerOptions{Pretty: true}),
					StrictSerializer: json.NewSerializerWithOptions(json.DefaultMetaFactory, creator, typer, json.SerializerOptions{
						Strict:                       true,
						StreamingCollectionsEncoding: streamingCollections,
					}),
					StreamSerializer: &runtime.StreamSerializerInfo{
						EncodesAsText: true,
						Serializer:    json.NewSerializerWithOptions(json.DefaultMetaFactory, creator, typer, json.SerializerOptions{}),
						Framer:        json.Framer,
					},
				},
				{
					MediaType:        "application/yaml",
					MediaTypeType:    "application",
					MediaTypeSubType: "yaml",
					EncodesAsText:    true,
					Serializer:       json.NewSerializerWithOptions(json.DefaultMetaFactory, creator, typer, json.SerializerOptions{Yaml: true}),
					StrictSerializer: json.NewSerializerWithOptions(json.DefaultMetaFactory, creator, typer, json.SerializerOptions{
						Yaml:   true,
						Strict: true,
					}),
				},
				{
					MediaType:        "application/vnd.kubernetes.protobuf",
					MediaTypeType:    "application",
					MediaTypeSubType: "vnd.kubernetes.protobuf",
					Serializer:       protobuf.NewSerializer(creator, typer),
					StreamSerializer: &runtime.StreamSerializerInfo{
						Serializer: protobuf.NewRawSerializer(creator, typer),
						Framer:     protobuf.LengthDelimitedFramer,
					},
				},
			},
		}

		if utilfeature.DefaultFeatureGate.Enabled(features.CBORServingAndStorage) {
			negotiatedSerializer.supportedMediaTypes = append(negotiatedSerializer.supportedMediaTypes, cbor.NewSerializerInfo(creator, typer))
		}

		var standardSerializers []runtime.SerializerInfo
		for _, s := range negotiatedSerializer.SupportedMediaTypes() {
			if s.MediaType == runtime.ContentTypeProtobuf {
				continue
			}
			standardSerializers = append(standardSerializers, s)
		}

		reqScope := handlers.RequestScope{
			Namer: handlers.ContextBasedNaming{
				Namer:         meta.NewAccessor(),
				ClusterScoped: clusterScoped,
			},
			Serializer:          negotiatedSerializer,
			ParameterCodec:      parameterCodec,
			StandardSerializers: standardSerializers,

			Creater:         creator,
			Convertor:       safeConverter,
			Defaulter:       unstructuredDefaulter{parameterScheme, structuralSchemas, kind.GroupKind()},
			Typer:           typer,
			UnsafeConvertor: unsafeConverter,

			EquivalentResourceMapper: equivalentResourceRegistry,

			Resource: schema.GroupVersionResource{Group: crd.Spec.Group, Version: v.Name, Resource: crd.Status.AcceptedNames.Plural},
			Kind:     kind,

			// a handler for a specific group-version of a custom resource uses that version as the in-memory representation
			HubGroupVersion: kind.GroupVersion(),

			MetaGroupVersion: metav1.SchemeGroupVersion,

			TableConvertor: storages[v.Name].CustomResource,

			Authorizer: r.authorizer,

			MaxRequestBodyBytes: r.maxRequestBodyBytes,
		}

		resetFields := storages[v.Name].CustomResource.GetResetFields()
		reqScope, err = scopeWithFieldManager(
			typeConverter,
			reqScope,
			resetFields,
			"",
		)
		if err != nil {
			return nil, err
		}
		requestScopes[v.Name] = &reqScope

		scaleColumns, err := getScaleColumnsForVersion(crd, v.Name)
		if err != nil {
			return nil, fmt.Errorf("the server could not properly serve the CR scale subresource columns %w", err)
		}
		scaleTable, _ := tableconvertor.New(scaleColumns)

		// override scale subresource values
		// shallow copy
		scaleScope := *requestScopes[v.Name]
		scaleConverter := scale.NewScaleConverter()
		scaleScope.Subresource = "scale"
		var opts []serializer.CodecFactoryOptionsMutator
		if utilfeature.DefaultFeatureGate.Enabled(features.CBORServingAndStorage) {
			opts = append(opts, serializer.WithSerializer(cbor.NewSerializerInfo))
		}
		if utilfeature.DefaultFeatureGate.Enabled(features.StreamingCollectionEncodingToJSON) {
			opts = append(opts, serializer.WithStreamingCollectionEncodingToJSON())
		}
		scaleScope.Serializer = serializer.NewCodecFactory(scaleConverter.Scheme(), opts...)
		scaleScope.Kind = autoscalingv1.SchemeGroupVersion.WithKind("Scale")
		scaleScope.Namer = handlers.ContextBasedNaming{
			Namer:         meta.NewAccessor(),
			ClusterScoped: clusterScoped,
		}
		scaleScope.TableConvertor = scaleTable

		if subresources != nil && subresources.Scale != nil {
			scaleScope, err = scopeWithFieldManager(
				typeConverter,
				scaleScope,
				nil,
				"scale",
			)
			if err != nil {
				return nil, err
			}
		}

		scaleScopes[v.Name] = &scaleScope

		// override status subresource values
		// shallow copy
		statusScope := *requestScopes[v.Name]
		statusScope.Subresource = "status"
		statusScope.Namer = handlers.ContextBasedNaming{
			Namer:         meta.NewAccessor(),
			ClusterScoped: clusterScoped,
		}

		if subresources != nil && subresources.Status != nil {
			resetFields := storages[v.Name].Status.GetResetFields()
			statusScope, err = scopeWithFieldManager(
				typeConverter,
				statusScope,
				resetFields,
				"status",
			)
			if err != nil {
				return nil, err
			}
		}

		statusScopes[v.Name] = &statusScope

		if v.Deprecated {
			deprecated[v.Name] = true
			if v.DeprecationWarning != nil {
				warnings[v.Name] = append(warnings[v.Name], *v.DeprecationWarning)
			} else {
				warnings[v.Name] = append(warnings[v.Name], defaultDeprecationWarning(v.Name, crd.Spec))
			}
		}
	}

	ret := &crdInfo{
		spec:                &crd.Spec,
		acceptedNames:       &crd.Status.AcceptedNames,
		storages:            storages,
		requestScopes:       requestScopes,
		scaleRequestScopes:  scaleScopes,
		statusRequestScopes: statusScopes,
		deprecated:          deprecated,
		warnings:            warnings,
		storageVersion:      storageVersion,
		waitGroup:           &utilwaitgroup.SafeWaitGroup{},
	}

	// Copy because we cannot write to storageMap without a race
	// as it is used without locking elsewhere.
	storageMap2 := storageMap.clone()

	storageMap2[crd.UID] = ret
	r.customStorage.Store(storageMap2)

	return ret, nil
}

func scopeWithFieldManager(typeConverter managedfields.TypeConverter, reqScope handlers.RequestScope, resetFields map[fieldpath.APIVersion]*fieldpath.Set, subresource string) (handlers.RequestScope, error) {
	fieldManager, err := managedfields.NewDefaultCRDFieldManager(
		typeConverter,
		reqScope.Convertor,
		reqScope.Defaulter,
		reqScope.Creater,
		reqScope.Kind,
		reqScope.HubGroupVersion,
		subresource,
		fieldpath.NewExcludeFilterSetMap(resetFields),
	)
	if err != nil {
		return handlers.RequestScope{}, err
	}
	reqScope.FieldManager = fieldManager
	return reqScope, nil
}

func defaultDeprecationWarning(deprecatedVersion string, crd apiextensionsv1.CustomResourceDefinitionSpec) string {
	msg := fmt.Sprintf("%s/%s %s is deprecated", crd.Group, deprecatedVersion, crd.Names.Kind)

	var servedNonDeprecatedVersions []string
	for _, v := range crd.Versions {
		if v.Served && !v.Deprecated && version.CompareKubeAwareVersionStrings(deprecatedVersion, v.Name) < 0 {
			servedNonDeprecatedVersions = append(servedNonDeprecatedVersions, v.Name)
		}
	}
	if len(servedNonDeprecatedVersions) == 0 {
		return msg
	}
	sort.Slice(servedNonDeprecatedVersions, func(i, j int) bool {
		return version.CompareKubeAwareVersionStrings(servedNonDeprecatedVersions[i], servedNonDeprecatedVersions[j]) > 0
	})
	msg += fmt.Sprintf("; use %s/%s %s", crd.Group, servedNonDeprecatedVersions[0], crd.Names.Kind)
	return msg
}

type unstructuredNegotiatedSerializer struct {
	typer     runtime.ObjectTyper
	creator   runtime.ObjectCreater
	converter runtime.ObjectConvertor

	structuralSchemas     map[string]*structuralschema.Structural // by version
	structuralSchemaGK    schema.GroupKind
	preserveUnknownFields bool

	supportedMediaTypes []runtime.SerializerInfo
}

func (s unstructuredNegotiatedSerializer) SupportedMediaTypes() []runtime.SerializerInfo {
	return s.supportedMediaTypes
}

func (s unstructuredNegotiatedSerializer) EncoderForVersion(encoder runtime.Encoder, gv runtime.GroupVersioner) runtime.Encoder {
	return versioning.NewCodec(encoder, nil, s.converter, Scheme, Scheme, Scheme, gv, nil, "crdNegotiatedSerializer")
}

func (s unstructuredNegotiatedSerializer) DecoderToVersion(decoder runtime.Decoder, gv runtime.GroupVersioner) runtime.Decoder {
	returnUnknownFieldPaths := false
	if serializer, ok := decoder.(*json.Serializer); ok {
		returnUnknownFieldPaths = serializer.IsStrict()
	}
	d := schemaCoercingDecoder{delegate: decoder, validator: unstructuredSchemaCoercer{structuralSchemas: s.structuralSchemas, structuralSchemaGK: s.structuralSchemaGK, preserveUnknownFields: s.preserveUnknownFields, returnUnknownFieldPaths: returnUnknownFieldPaths}}
	return versioning.NewCodec(nil, d, runtime.UnsafeObjectConvertor(Scheme), Scheme, Scheme, unstructuredDefaulter{
		delegate:           Scheme,
		structuralSchemas:  s.structuralSchemas,
		structuralSchemaGK: s.structuralSchemaGK,
	}, nil, gv, "unstructuredNegotiatedSerializer")
}

type UnstructuredObjectTyper struct {
	Delegate          runtime.ObjectTyper
	UnstructuredTyper runtime.ObjectTyper
}

func newUnstructuredObjectTyper(Delegate runtime.ObjectTyper) UnstructuredObjectTyper {
	return UnstructuredObjectTyper{
		Delegate:          Delegate,
		UnstructuredTyper: crdserverscheme.NewUnstructuredObjectTyper(),
	}
}

func (t UnstructuredObjectTyper) ObjectKinds(obj runtime.Object) ([]schema.GroupVersionKind, bool, error) {
	// Delegate for things other than Unstructured.
	if _, ok := obj.(runtime.Unstructured); !ok {
		return t.Delegate.ObjectKinds(obj)
	}
	return t.UnstructuredTyper.ObjectKinds(obj)
}

func (t UnstructuredObjectTyper) Recognizes(gvk schema.GroupVersionKind) bool {
	return t.Delegate.Recognizes(gvk) || t.UnstructuredTyper.Recognizes(gvk)
}

type unstructuredCreator struct{}

func (c unstructuredCreator) New(kind schema.GroupVersionKind) (runtime.Object, error) {
	ret := &unstructured.Unstructured{}
	ret.SetGroupVersionKind(kind)
	return ret, nil
}

type unstructuredDefaulter struct {
	delegate           runtime.ObjectDefaulter
	structuralSchemas  map[string]*structuralschema.Structural // by version
	structuralSchemaGK schema.GroupKind
}

func (d unstructuredDefaulter) Default(in runtime.Object) {
	// Delegate for things other than Unstructured, and other GKs
	u, ok := in.(runtime.Unstructured)
	if !ok || u.GetObjectKind().GroupVersionKind().GroupKind() != d.structuralSchemaGK {
		d.delegate.Default(in)
		return
	}

	structuraldefaulting.Default(u.UnstructuredContent(), d.structuralSchemas[u.GetObjectKind().GroupVersionKind().Version])
}

// clone returns a clone of the provided crdStorageMap.
// The clone is a shallow copy of the map.
func (in crdStorageMap) clone() crdStorageMap {
	if in == nil {
		return nil
	}
	out := make(crdStorageMap, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

// crdConversionRESTOptionsGetter overrides the codec with one using the
// provided custom converter and custom encoder and decoder version.
type crdConversionRESTOptionsGetter struct {
	generic.RESTOptionsGetter
	converter             runtime.ObjectConvertor
	encoderVersion        schema.GroupVersion
	decoderVersion        schema.GroupVersion
	structuralSchemas     map[string]*structuralschema.Structural // by version
	structuralSchemaGK    schema.GroupKind
	preserveUnknownFields bool
}

func (t crdConversionRESTOptionsGetter) GetRESTOptions(resource schema.GroupResource, example runtime.Object) (generic.RESTOptions, error) {
	// Explicitly ignore example, we override storageconfig below
	ret, err := t.RESTOptionsGetter.GetRESTOptions(resource, nil)
	if err == nil {
		d := schemaCoercingDecoder{delegate: ret.StorageConfig.Codec, validator: unstructuredSchemaCoercer{
			// drop invalid fields while decoding old CRs (before we haven't had any ObjectMeta validation)
			dropInvalidMetadata:   true,
			repairGeneration:      true,
			structuralSchemas:     t.structuralSchemas,
			structuralSchemaGK:    t.structuralSchemaGK,
			preserveUnknownFields: t.preserveUnknownFields,
		}}
		c := schemaCoercingConverter{delegate: t.converter, validator: unstructuredSchemaCoercer{
			structuralSchemas:     t.structuralSchemas,
			structuralSchemaGK:    t.structuralSchemaGK,
			preserveUnknownFields: t.preserveUnknownFields,
		}}
		ret.StorageConfig.Codec = versioning.NewCodec(
			ret.StorageConfig.Codec,
			d,
			c,
			&unstructuredCreator{},
			crdserverscheme.NewUnstructuredObjectTyper(),
			&unstructuredDefaulter{
				delegate:           Scheme,
				structuralSchemaGK: t.structuralSchemaGK,
				structuralSchemas:  t.structuralSchemas,
			},
			t.encoderVersion,
			t.decoderVersion,
			"crdRESTOptions",
		)
	}
	return ret, err
}

// schemaCoercingDecoder calls the delegate decoder, and then applies the Unstructured schema validator
// to coerce the schema.
type schemaCoercingDecoder struct {
	delegate  runtime.Decoder
	validator unstructuredSchemaCoercer
}

var _ runtime.Decoder = schemaCoercingDecoder{}

func (d schemaCoercingDecoder) Decode(data []byte, defaults *schema.GroupVersionKind, into runtime.Object) (runtime.Object, *schema.GroupVersionKind, error) {
	var decodingStrictErrs []error
	obj, gvk, err := d.delegate.Decode(data, defaults, into)
	if err != nil {
		decodeStrictErr, ok := runtime.AsStrictDecodingError(err)
		if !ok || obj == nil {
			return nil, gvk, err
		}
		decodingStrictErrs = decodeStrictErr.Errors()
	}
	var unknownFields []string
	if u, ok := obj.(*unstructured.Unstructured); ok {
		unknownFields, err = d.validator.apply(u)
		if err != nil {
			return nil, gvk, err
		}
	}
	if d.validator.returnUnknownFieldPaths && (len(decodingStrictErrs) > 0 || len(unknownFields) > 0) {
		for _, unknownField := range unknownFields {
			decodingStrictErrs = append(decodingStrictErrs, fmt.Errorf(`unknown field "%s"`, unknownField))
		}
		return obj, gvk, runtime.NewStrictDecodingError(decodingStrictErrs)
	}

	return obj, gvk, nil
}

// schemaCoercingConverter calls the delegate converter and applies the Unstructured validator to
// coerce the schema.
type schemaCoercingConverter struct {
	delegate  runtime.ObjectConvertor
	validator unstructuredSchemaCoercer
}

var _ runtime.ObjectConvertor = schemaCoercingConverter{}

func (v schemaCoercingConverter) Convert(in, out, context interface{}) error {
	if err := v.delegate.Convert(in, out, context); err != nil {
		return err
	}

	if u, ok := out.(*unstructured.Unstructured); ok {
		if _, err := v.validator.apply(u); err != nil {
			return err
		}
	}

	return nil
}

func (v schemaCoercingConverter) ConvertToVersion(in runtime.Object, gv runtime.GroupVersioner) (runtime.Object, error) {
	out, err := v.delegate.ConvertToVersion(in, gv)
	if err != nil {
		return nil, err
	}

	if u, ok := out.(*unstructured.Unstructured); ok {
		if _, err := v.validator.apply(u); err != nil {
			return nil, err
		}
	}

	return out, nil
}

func (v schemaCoercingConverter) ConvertFieldLabel(gvk schema.GroupVersionKind, label, value string) (string, string, error) {
	return v.delegate.ConvertFieldLabel(gvk, label, value)
}

// unstructuredSchemaCoercer adds to unstructured unmarshalling what json.Unmarshal does
// in addition for native types when decoding into Golang structs:
//
// - validating and pruning ObjectMeta
// - generic pruning of unknown fields following a structural schema
// - removal of non-defaulted non-nullable null map values.
type unstructuredSchemaCoercer struct {
	dropInvalidMetadata bool
	repairGeneration    bool

	structuralSchemas       map[string]*structuralschema.Structural
	structuralSchemaGK      schema.GroupKind
	preserveUnknownFields   bool
	returnUnknownFieldPaths bool
}

func (v *unstructuredSchemaCoercer) apply(u *unstructured.Unstructured) (unknownFieldPaths []string, err error) {
	// save implicit meta fields that don't have to be specified in the validation spec
	kind, foundKind, err := unstructured.NestedString(u.UnstructuredContent(), "kind")
	if err != nil {
		return nil, err
	}
	apiVersion, foundApiVersion, err := unstructured.NestedString(u.UnstructuredContent(), "apiVersion")
	if err != nil {
		return nil, err
	}
	objectMeta, foundObjectMeta, metaUnknownFields, err := schemaobjectmeta.GetObjectMetaWithOptions(u.Object, schemaobjectmeta.ObjectMetaOptions{
		DropMalformedFields:     v.dropInvalidMetadata,
		ReturnUnknownFieldPaths: v.returnUnknownFieldPaths,
	})
	if err != nil {
		return nil, err
	}
	unknownFieldPaths = append(unknownFieldPaths, metaUnknownFields...)

	// compare group and kind because also other object like DeleteCollection options pass through here
	gv, err := schema.ParseGroupVersion(apiVersion)
	if err != nil {
		return nil, err
	}

	if gv.Group == v.structuralSchemaGK.Group && kind == v.structuralSchemaGK.Kind {
		if !v.preserveUnknownFields {
			// TODO: switch over pruning and coercing at the root to schemaobjectmeta.Coerce too
			pruneOpts := structuralschema.UnknownFieldPathOptions{}
			if v.returnUnknownFieldPaths {
				pruneOpts.TrackUnknownFieldPaths = true
			}
			unknownFieldPaths = append(unknownFieldPaths, structuralpruning.PruneWithOptions(u.Object, v.structuralSchemas[gv.Version], true, pruneOpts)...)
			structuraldefaulting.PruneNonNullableNullsWithoutDefaults(u.Object, v.structuralSchemas[gv.Version])
		}

		err, paths := schemaobjectmeta.CoerceWithOptions(nil, u.Object, v.structuralSchemas[gv.Version], false, schemaobjectmeta.CoerceOptions{
			DropInvalidFields:       v.dropInvalidMetadata,
			ReturnUnknownFieldPaths: v.returnUnknownFieldPaths,
		})
		if err != nil {
			return nil, err
		}
		unknownFieldPaths = append(unknownFieldPaths, paths...)

		// fixup missing generation in very old CRs
		if v.repairGeneration && objectMeta.Generation == 0 {
			objectMeta.Generation = 1
		}
	}

	// restore meta fields, starting clean
	if foundKind {
		u.SetKind(kind)
	}
	if foundApiVersion {
		u.SetAPIVersion(apiVersion)
	}
	if foundObjectMeta {
		if err := schemaobjectmeta.SetObjectMeta(u.Object, objectMeta); err != nil {
			return nil, err
		}
	}

	return unknownFieldPaths, nil
}

// hasServedCRDVersion returns true if the given version is in the list of CRD's versions and the Served flag is set.
func hasServedCRDVersion(spec *apiextensionsv1.CustomResourceDefinitionSpec, version string) bool {
	for _, v := range spec.Versions {
		if v.Name == version {
			return v.Served
		}
	}
	return false
}

// buildOpenAPIModelsForApply constructs openapi models from any validation schemas specified in the custom resource,
// and merges it with the models defined in the static OpenAPI spec.
// Returns nil models ifthe static spec is nil, or an error is encountered.
func buildOpenAPIModelsForApply(staticOpenAPISpec map[string]*spec.Schema, crd *apiextensionsv1.CustomResourceDefinition) (map[string]*spec.Schema, error) {
	if staticOpenAPISpec == nil {
		return nil, nil
	}

	// Convert static spec to V3 format to be able to merge
	staticSpecV3 := &spec3.OpenAPI{
		Version: "3.0.0",
		Info: &spec.Info{
			InfoProps: spec.InfoProps{
				Title:   "Kubernetes CRD Swagger",
				Version: "v0.1.0",
			},
		},
		Components: &spec3.Components{
			Schemas: staticOpenAPISpec,
		},
	}

	specs := []*spec3.OpenAPI{staticSpecV3}
	for _, v := range crd.Spec.Versions {
		// Defaults are not pruned here, but before being served.
		// See flag description in builder.go for flag usage
		s, err := builder.BuildOpenAPIV3(crd, v.Name, builder.Options{})
		if err != nil {
			return nil, err
		}
		specs = append(specs, s)
	}

	mergedOpenAPI, err := builder.MergeSpecsV3(specs...)
	if err != nil {
		return nil, err
	}
	return mergedOpenAPI.Components.Schemas, nil
}
