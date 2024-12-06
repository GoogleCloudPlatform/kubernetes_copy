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

package openapi

import (
	"context"
	"encoding/json"
	"strings"

	"k8s.io/client-go/rest"
	"k8s.io/kube-openapi/pkg/handler3"
)

// Deprecated: use ClientWithContext instead.
type Client interface {
	Paths() (map[string]GroupVersion, error)
}

type ClientWithContext interface {
	PathsWithContext(ctx context.Context) (map[string]GroupVersionWithContext, error)
}

func ToClientWithContext(c Client) ClientWithContext {
	if c == nil {
		return nil
	}
	if c, ok := c.(ClientWithContext); ok {
		return c
	}
	return &clientWrapper{
		delegate: c,
	}
}

type clientWrapper struct {
	delegate Client
}

func (c *clientWrapper) PathsWithContext(ctx context.Context) (map[string]GroupVersionWithContext, error) {
	resultWithoutContext, err := c.delegate.Paths()
	result := make(map[string]GroupVersionWithContext, len(resultWithoutContext))
	for key, entry := range resultWithoutContext {
		result[key] = ToGroupVersionWithContext(entry)
	}
	return result, err
}

type client struct {
	// URL includes the `hash` query param to take advantage of cache busting
	restClient rest.Interface
}

// Deprecated: use NewClientWithContext instead.
func NewClient(restClient rest.Interface) Client {
	return newClient(restClient)
}

func NewClientWithContext(restClient rest.Interface) ClientWithContext {
	return newClient(restClient)
}

func newClient(restClient rest.Interface) *client {
	return &client{
		restClient: restClient,
	}
}

// Deprecated: use PathsWithContext instead.
func (c *client) Paths() (map[string]GroupVersion, error) {
	resultWithContext, err := c.PathsWithContext(context.Background())
	result := make(map[string]GroupVersion, len(resultWithContext))
	for key, entry := range resultWithContext {
		// We know that this is a *groupversion which implements GroupVersion.
		result[key] = entry.(GroupVersion)
	}
	return result, err
}

func (c *client) PathsWithContext(ctx context.Context) (map[string]GroupVersionWithContext, error) {
	data, err := c.restClient.Get().
		AbsPath("/openapi/v3").
		Do(ctx).
		Raw()

	if err != nil {
		return nil, err
	}

	discoMap := &handler3.OpenAPIV3Discovery{}
	err = json.Unmarshal(data, discoMap)
	if err != nil {
		return nil, err
	}

	// Create GroupVersions for each element of the result
	result := make(map[string]GroupVersionWithContext, len(discoMap.Paths))
	for k, v := range discoMap.Paths {
		// If the server returned a URL rooted at /openapi/v3, preserve any additional client-side prefix.
		// If the server returned a URL not rooted at /openapi/v3, treat it as an actual server-relative URL.
		// See https://github.com/kubernetes/kubernetes/issues/117463 for details
		useClientPrefix := strings.HasPrefix(v.ServerRelativeURL, "/openapi/v3")
		result[k] = newGroupVersion(c, v, useClientPrefix)
	}
	return result, nil
}
