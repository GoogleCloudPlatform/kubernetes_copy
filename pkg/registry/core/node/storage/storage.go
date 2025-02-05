/*
Copyright 2015 The Kubernetes Authors.

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

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/registry/generic"
	genericregistry "k8s.io/apiserver/pkg/registry/generic/registry"
	"k8s.io/apiserver/pkg/registry/rest"
	api "k8s.io/kubernetes/pkg/apis/core"
	k8s_api_v1 "k8s.io/kubernetes/pkg/apis/core/v1"
	"k8s.io/kubernetes/pkg/kubelet/client"
	"k8s.io/kubernetes/pkg/printers"
	printersinternal "k8s.io/kubernetes/pkg/printers/internalversion"
	printerstorage "k8s.io/kubernetes/pkg/printers/storage"
	"k8s.io/kubernetes/pkg/registry/core/node"
	noderest "k8s.io/kubernetes/pkg/registry/core/node/rest"
	"sigs.k8s.io/structured-merge-diff/v4/fieldpath"
)

// NodeStorage includes storage for nodes and all sub resources.
type NodeStorage struct {
	Node   *REST
	Status *StatusREST
	Proxy  *noderest.ProxyREST

	KubeletConnectionInfo client.ConnectionInfoGetter
}

// REST implements a RESTStorage for nodes.
type REST struct {
	*genericregistry.Store
	connection     client.ConnectionInfoGetter
	proxyTransport http.RoundTripper
}

// StatusREST implements the REST endpoint for changing the status of a node.
type StatusREST struct {
	store *genericregistry.Store
}

// New creates a new Node object.
func (r *StatusREST) New() runtime.Object {
	return &api.Node{}
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

// NewStorage returns a NodeStorage object that will work against nodes.
func NewStorage(optsGetter generic.RESTOptionsGetter, kubeletClientConfig client.KubeletClientConfig, proxyTransport http.RoundTripper) (*NodeStorage, error) {
	store := &genericregistry.Store{
		NewFunc:                   func() runtime.Object { return &api.Node{} },
		NewListFunc:               func() runtime.Object { return &api.NodeList{} },
		PredicateFunc:             node.MatchNode,
		DefaultQualifiedResource:  api.Resource("nodes"),
		SingularQualifiedResource: api.Resource("node"),

		CreateStrategy:      node.Strategy,
		UpdateStrategy:      node.Strategy,
		DeleteStrategy:      node.Strategy,
		ResetFieldsStrategy: node.Strategy,

		TableConvertor: printerstorage.TableConvertor{TableGenerator: printers.NewTableGenerator().With(printersinternal.AddHandlers)},
	}
	options := &generic.StoreOptions{
		RESTOptions: optsGetter,
		AttrFunc:    node.GetAttrs,
	}
	if err := store.CompleteWithOptions(options); err != nil {
		return nil, err
	}

	statusStore := *store
	statusStore.UpdateStrategy = node.StatusStrategy
	statusStore.ResetFieldsStrategy = node.StatusStrategy

	// Set up REST handlers
	nodeREST := &REST{Store: store, proxyTransport: proxyTransport}
	statusREST := &StatusREST{store: &statusStore}
	proxyREST := &noderest.ProxyREST{Store: store, ProxyTransport: proxyTransport}

	// Build a NodeGetter that looks up nodes using the REST handler
	nodeGetter := client.NodeGetterFunc(func(ctx context.Context, nodeName string, options metav1.GetOptions) (*v1.Node, error) {
		// If the request is for a different resource, we need to temporarily overwrite the request resource to nodes
		if requestInfo, found := genericapirequest.RequestInfoFrom(ctx); found {
			if requestInfo.Resource != store.DefaultQualifiedResource.Resource || requestInfo.APIGroup != store.DefaultQualifiedResource.Group {
				defer func(group, resource string) {
					requestInfo.APIGroup, requestInfo.Resource = group, resource
					genericapirequest.WithRequestInfo(ctx, requestInfo)
				}(requestInfo.APIGroup, requestInfo.Resource)

				requestInfo.APIGroup, requestInfo.Resource = store.DefaultQualifiedResource.Group, store.DefaultQualifiedResource.Resource
				genericapirequest.WithRequestInfo(ctx, requestInfo)
			}
		}
		obj, err := nodeREST.Get(ctx, nodeName, &options)
		if err != nil {
			return nil, err
		}
		node, ok := obj.(*api.Node)
		if !ok {
			return nil, fmt.Errorf("unexpected type %T", obj)
		}
		// TODO: Remove the conversion. Consider only return the NodeAddresses
		externalNode := &v1.Node{}
		err = k8s_api_v1.Convert_core_Node_To_v1_Node(node, externalNode, nil)
		if err != nil {
			return nil, fmt.Errorf("failed to convert to v1.Node: %v", err)
		}
		return externalNode, nil
	})
	connectionInfoGetter, err := client.NewNodeConnectionInfoGetter(nodeGetter, kubeletClientConfig)
	if err != nil {
		return nil, err
	}
	nodeREST.connection = connectionInfoGetter
	proxyREST.Connection = connectionInfoGetter

	return &NodeStorage{
		Node:                  nodeREST,
		Status:                statusREST,
		Proxy:                 proxyREST,
		KubeletConnectionInfo: connectionInfoGetter,
	}, nil
}

// Implement Redirector.
var _ = rest.Redirector(&REST{})

// ResourceLocation returns a URL to which one can send traffic for the specified node.
func (r *REST) ResourceLocation(ctx context.Context, id string) (*url.URL, http.RoundTripper, error) {
	return node.ResourceLocation(r, r.connection, r.proxyTransport, ctx, id)
}

// ShortNames implements the ShortNamesProvider interface. Returns a list of short names for a resource.
func (r *REST) ShortNames() []string {
	return []string{"no"}
}
