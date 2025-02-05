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
	"net/url"

	"k8s.io/kube-openapi/pkg/handler3"
)

const ContentTypeOpenAPIV3PB = "application/com.github.proto-openapi.spec.v3@v1.0+protobuf"

// Deprecated: use GroupVersionWithContext instead.
type GroupVersion interface {
	Schema(contentType string) ([]byte, error)

	// ServerRelativeURL. Returns the path and parameters used to fetch the schema.
	// You should use the Schema method to fetch it, but this value can be used
	// to key the current version of the schema in a cache since it contains a
	// hash string which changes upon schema update.
	ServerRelativeURL() string
}

type GroupVersionWithContext interface {
	SchemaWithContext(ctx context.Context, contentType string) ([]byte, error)

	// ServerRelativeURL. Returns the path and parameters used to fetch the schema.
	// You should use the Schema method to fetch it, but this value can be used
	// to key the current version of the schema in a cache since it contains a
	// hash string which changes upon schema update.
	ServerRelativeURL() string
}

func ToGroupVersionWithContext(gv GroupVersion) GroupVersionWithContext {
	if gv == nil {
		return nil
	}
	if gv, ok := gv.(GroupVersionWithContext); ok {
		return gv
	}
	return &groupVersionWrapper{
		GroupVersion: gv,
	}
}

type groupVersionWrapper struct {
	GroupVersion
}

func (gv *groupVersionWrapper) SchemaWithContext(ctx context.Context, contentType string) ([]byte, error) {
	return gv.Schema(contentType)
}

type groupversion struct {
	client          *client
	item            handler3.OpenAPIV3DiscoveryGroupVersion
	useClientPrefix bool
}

var (
	_ GroupVersion            = &groupversion{}
	_ GroupVersionWithContext = &groupversion{}
)

func newGroupVersion(client *client, item handler3.OpenAPIV3DiscoveryGroupVersion, useClientPrefix bool) *groupversion {
	return &groupversion{client: client, item: item, useClientPrefix: useClientPrefix}
}

func (g *groupversion) Schema(contentType string) ([]byte, error) {
	return g.SchemaWithContext(context.Background(), contentType)
}

func (g *groupversion) SchemaWithContext(ctx context.Context, contentType string) ([]byte, error) {
	if !g.useClientPrefix {
		return g.client.restClient.Get().
			RequestURI(g.item.ServerRelativeURL).
			SetHeader("Accept", contentType).
			Do(ctx).
			Raw()
	}

	locator, err := url.Parse(g.item.ServerRelativeURL)
	if err != nil {
		return nil, err
	}

	path := g.client.restClient.Get().
		AbsPath(locator.Path).
		SetHeader("Accept", contentType)

	// Other than root endpoints(openapiv3/apis), resources have hash query parameter to support etags.
	// However, absPath does not support handling query parameters internally,
	// so that hash query parameter is added manually
	for k, value := range locator.Query() {
		for _, v := range value {
			path.Param(k, v)
		}
	}

	return path.Do(ctx).Raw()
}

// URL used for fetching the schema. The URL includes a hash and can be used
// to key the current version of the schema in a cache.
func (g *groupversion) ServerRelativeURL() string {
	return g.item.ServerRelativeURL
}
