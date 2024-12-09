/*
Copyright 2018 The Kubernetes Authors.

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

package dynamic

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/features"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/apply"
	"k8s.io/client-go/util/consistencydetector"
	"k8s.io/client-go/util/watchlist"
	"k8s.io/klog/v2"
)

type DynamicClient struct {
	client rest.Interface
}

var _ Interface = &DynamicClient{}

// ConfigFor returns a copy of the provided config with the
// appropriate dynamic client defaults set.
func ConfigFor(inConfig *rest.Config) *rest.Config {
	config := rest.CopyConfig(inConfig)

	config.ContentType = "application/json"
	config.AcceptContentTypes = "application/json"
	if features.FeatureGates().Enabled(features.ClientsAllowCBOR) {
		config.AcceptContentTypes = "application/json;q=0.9,application/cbor;q=1"
		if features.FeatureGates().Enabled(features.ClientsPreferCBOR) {
			config.ContentType = "application/cbor"
		}
	}

	config.NegotiatedSerializer = newBasicNegotiatedSerializer()
	if config.UserAgent == "" {
		config.UserAgent = rest.DefaultKubernetesUserAgent()
	}
	return config
}

// New creates a new DynamicClient for the given RESTClient.
func New(c rest.Interface) *DynamicClient {
	return &DynamicClient{client: c}
}

// NewForConfigOrDie creates a new DynamicClient for the given config and
// panics if there is an error in the config.
func NewForConfigOrDie(c *rest.Config) *DynamicClient {
	ret, err := NewForConfig(c)
	if err != nil {
		panic(err)
	}
	return ret
}

// NewForConfig creates a new dynamic client or returns an error.
// NewForConfig is equivalent to NewForConfigAndClient(c, httpClient),
// where httpClient was generated with rest.HTTPClientFor(c).
func NewForConfig(inConfig *rest.Config) (*DynamicClient, error) {
	config := ConfigFor(inConfig)

	httpClient, err := rest.HTTPClientFor(config)
	if err != nil {
		return nil, err
	}
	return NewForConfigAndClient(config, httpClient)
}

// NewForConfigAndClient creates a new dynamic client for the given config and http client.
// Note the http client provided takes precedence over the configured transport values.
func NewForConfigAndClient(inConfig *rest.Config, h *http.Client) (*DynamicClient, error) {
	config := ConfigFor(inConfig)
	config.GroupVersion = nil
	config.APIPath = "/if-you-see-this-search-for-the-break"

	restClient, err := rest.UnversionedRESTClientForConfigAndClient(config, h)
	if err != nil {
		return nil, err
	}
	return &DynamicClient{client: restClient}, nil
}

type dynamicResourceClient struct {
	client    *DynamicClient
	namespace string
	resource  schema.GroupVersionResource
}

func (c *DynamicClient) Resource(resource schema.GroupVersionResource) NamespaceableResourceInterface {
	return &dynamicResourceClient{client: c, resource: resource}
}

func (c *dynamicResourceClient) Namespace(ns string) ResourceInterface {
	ret := *c
	ret.namespace = ns
	return &ret
}

func (c *dynamicResourceClient) Create(ctx context.Context, obj *unstructured.Unstructured, opts metav1.CreateOptions, subresources ...string) (*unstructured.Unstructured, error) {
	name := ""
	if len(subresources) > 0 {
		accessor, err := meta.Accessor(obj)
		if err != nil {
			return nil, err
		}
		name = accessor.GetName()
		if len(name) == 0 {
			return nil, fmt.Errorf("name is required")
		}
	}
	if err := validateNamespaceWithOptionalName(c.namespace, name); err != nil {
		return nil, err
	}

	var out unstructured.Unstructured
	if err := c.client.client.
		Post().
		AbsPath(append(c.makeURLSegments(name), subresources...)...).
		Body(obj).
		SpecificallyVersionedParams(&opts, dynamicParameterCodec, versionV1).
		Do(ctx).Into(&out); err != nil {
		return nil, err
	}

	return &out, nil
}

func (c *dynamicResourceClient) Update(ctx context.Context, obj *unstructured.Unstructured, opts metav1.UpdateOptions, subresources ...string) (*unstructured.Unstructured, error) {
	accessor, err := meta.Accessor(obj)
	if err != nil {
		return nil, err
	}
	name := accessor.GetName()
	if len(name) == 0 {
		return nil, fmt.Errorf("name is required")
	}
	if err := validateNamespaceWithOptionalName(c.namespace, name); err != nil {
		return nil, err
	}

	var out unstructured.Unstructured
	if err := c.client.client.
		Put().
		AbsPath(append(c.makeURLSegments(name), subresources...)...).
		Body(obj).
		SpecificallyVersionedParams(&opts, dynamicParameterCodec, versionV1).
		Do(ctx).Into(&out); err != nil {
		return nil, err
	}

	return &out, nil
}

func (c *dynamicResourceClient) UpdateStatus(ctx context.Context, obj *unstructured.Unstructured, opts metav1.UpdateOptions) (*unstructured.Unstructured, error) {
	accessor, err := meta.Accessor(obj)
	if err != nil {
		return nil, err
	}
	name := accessor.GetName()
	if len(name) == 0 {
		return nil, fmt.Errorf("name is required")
	}
	if err := validateNamespaceWithOptionalName(c.namespace, name); err != nil {
		return nil, err
	}

	var out unstructured.Unstructured
	if err := c.client.client.
		Put().
		AbsPath(append(c.makeURLSegments(name), "status")...).
		Body(obj).
		SpecificallyVersionedParams(&opts, dynamicParameterCodec, versionV1).
		Do(ctx).Into(&out); err != nil {
		return nil, err
	}

	return &out, nil
}

func (c *dynamicResourceClient) Delete(ctx context.Context, name string, opts metav1.DeleteOptions, subresources ...string) error {
	if len(name) == 0 {
		return fmt.Errorf("name is required")
	}
	if err := validateNamespaceWithOptionalName(c.namespace, name); err != nil {
		return err
	}

	result := c.client.client.
		Delete().
		AbsPath(append(c.makeURLSegments(name), subresources...)...).
		Body(&opts).
		Do(ctx)
	return result.Error()
}

func (c *dynamicResourceClient) DeleteCollection(ctx context.Context, opts metav1.DeleteOptions, listOptions metav1.ListOptions) error {
	if err := validateNamespaceWithOptionalName(c.namespace); err != nil {
		return err
	}

	result := c.client.client.
		Delete().
		AbsPath(c.makeURLSegments("")...).
		Body(&opts).
		SpecificallyVersionedParams(&listOptions, dynamicParameterCodec, versionV1).
		Do(ctx)
	return result.Error()
}

func (c *dynamicResourceClient) Get(ctx context.Context, name string, opts metav1.GetOptions, subresources ...string) (*unstructured.Unstructured, error) {
	if len(name) == 0 {
		return nil, fmt.Errorf("name is required")
	}
	if err := validateNamespaceWithOptionalName(c.namespace, name); err != nil {
		return nil, err
	}
	var out unstructured.Unstructured
	if err := c.client.client.
		Get().
		AbsPath(append(c.makeURLSegments(name), subresources...)...).
		SpecificallyVersionedParams(&opts, dynamicParameterCodec, versionV1).
		Do(ctx).Into(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *dynamicResourceClient) List(ctx context.Context, opts metav1.ListOptions) (*unstructured.UnstructuredList, error) {
	if watchListOptions, hasWatchListOptionsPrepared, watchListOptionsErr := watchlist.PrepareWatchListOptionsFromListOptions(opts); watchListOptionsErr != nil {
		klog.FromContext(ctx).Info("Warning: failed preparing watchlist options, falling back to the standard LIST semantics", "resource", c.resource, "err", watchListOptionsErr)
	} else if hasWatchListOptionsPrepared {
		result, err := c.watchList(ctx, watchListOptions)
		if err == nil {
			consistencydetector.CheckWatchListFromCacheDataConsistencyIfRequested(ctx, fmt.Sprintf("watchlist request for %v", c.resource), c.list, opts, result)
			return result, nil
		}
		klog.FromContext(ctx).Info("Warning: the watchlist request ended with an error, falling back to the standard LIST semantics", "resource", c.resource, "err", err)
	}
	result, err := c.list(ctx, opts)
	if err == nil {
		consistencydetector.CheckListFromCacheDataConsistencyIfRequested(ctx, fmt.Sprintf("list request for %v", c.resource), c.list, opts, result)
	}
	return result, err
}

func (c *dynamicResourceClient) list(ctx context.Context, opts metav1.ListOptions) (*unstructured.UnstructuredList, error) {
	if err := validateNamespaceWithOptionalName(c.namespace); err != nil {
		return nil, err
	}
	var out unstructured.UnstructuredList
	if err := c.client.client.
		Get().
		AbsPath(c.makeURLSegments("")...).
		SpecificallyVersionedParams(&opts, dynamicParameterCodec, versionV1).
		Do(ctx).Into(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

// watchList establishes a watch stream with the server and returns an unstructured list.
func (c *dynamicResourceClient) watchList(ctx context.Context, opts metav1.ListOptions) (*unstructured.UnstructuredList, error) {
	if err := validateNamespaceWithOptionalName(c.namespace); err != nil {
		return nil, err
	}

	var timeout time.Duration
	if opts.TimeoutSeconds != nil {
		timeout = time.Duration(*opts.TimeoutSeconds) * time.Second
	}

	result := &unstructured.UnstructuredList{}
	err := c.client.client.Get().AbsPath(c.makeURLSegments("")...).
		SpecificallyVersionedParams(&opts, dynamicParameterCodec, versionV1).
		Timeout(timeout).
		WatchList(ctx).
		Into(result)

	return result, err
}

func (c *dynamicResourceClient) Watch(ctx context.Context, opts metav1.ListOptions) (watch.Interface, error) {
	opts.Watch = true
	if err := validateNamespaceWithOptionalName(c.namespace); err != nil {
		return nil, err
	}
	return c.client.client.Get().AbsPath(c.makeURLSegments("")...).
		SpecificallyVersionedParams(&opts, dynamicParameterCodec, versionV1).
		Watch(ctx)
}

func (c *dynamicResourceClient) Patch(ctx context.Context, name string, pt types.PatchType, data []byte, opts metav1.PatchOptions, subresources ...string) (*unstructured.Unstructured, error) {
	if len(name) == 0 {
		return nil, fmt.Errorf("name is required")
	}
	if err := validateNamespaceWithOptionalName(c.namespace, name); err != nil {
		return nil, err
	}
	var out unstructured.Unstructured
	if err := c.client.client.
		Patch(pt).
		AbsPath(append(c.makeURLSegments(name), subresources...)...).
		Body(data).
		SpecificallyVersionedParams(&opts, dynamicParameterCodec, versionV1).
		Do(ctx).Into(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *dynamicResourceClient) Apply(ctx context.Context, name string, obj *unstructured.Unstructured, opts metav1.ApplyOptions, subresources ...string) (*unstructured.Unstructured, error) {
	if len(name) == 0 {
		return nil, fmt.Errorf("name is required")
	}
	if err := validateNamespaceWithOptionalName(c.namespace, name); err != nil {
		return nil, err
	}
	accessor, err := meta.Accessor(obj)
	if err != nil {
		return nil, err
	}
	managedFields := accessor.GetManagedFields()
	if len(managedFields) > 0 {
		return nil, fmt.Errorf(`cannot apply an object with managed fields already set.
		Use the client-go/applyconfigurations "UnstructructuredExtractor" to obtain the unstructured ApplyConfiguration for the given field manager that you can use/modify here to apply`)
	}
	patchOpts := opts.ToPatchOptions()

	request, err := apply.NewRequest(c.client.client, obj.Object)
	if err != nil {
		return nil, err
	}

	var out unstructured.Unstructured
	if err := request.
		AbsPath(append(c.makeURLSegments(name), subresources...)...).
		SpecificallyVersionedParams(&patchOpts, dynamicParameterCodec, versionV1).
		Do(ctx).Into(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *dynamicResourceClient) ApplyStatus(ctx context.Context, name string, obj *unstructured.Unstructured, opts metav1.ApplyOptions) (*unstructured.Unstructured, error) {
	return c.Apply(ctx, name, obj, opts, "status")
}

func validateNamespaceWithOptionalName(namespace string, name ...string) error {
	if msgs := rest.IsValidPathSegmentName(namespace); len(msgs) != 0 {
		return fmt.Errorf("invalid namespace %q: %v", namespace, msgs)
	}
	if len(name) > 1 {
		panic("Invalid number of names")
	} else if len(name) == 1 {
		if msgs := rest.IsValidPathSegmentName(name[0]); len(msgs) != 0 {
			return fmt.Errorf("invalid resource name %q: %v", name[0], msgs)
		}
	}
	return nil
}

func (c *dynamicResourceClient) makeURLSegments(name string) []string {
	url := []string{}
	if len(c.resource.Group) == 0 {
		url = append(url, "api")
	} else {
		url = append(url, "apis", c.resource.Group)
	}
	url = append(url, c.resource.Version)

	if len(c.namespace) > 0 {
		url = append(url, "namespaces", c.namespace)
	}
	url = append(url, c.resource.Resource)

	if len(name) > 0 {
		url = append(url, name)
	}

	return url
}
