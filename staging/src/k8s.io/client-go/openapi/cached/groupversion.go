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

package cached

import (
	"context"
	"sync"

	"k8s.io/client-go/openapi"
)

type groupversion struct {
	delegate openapi.GroupVersionWithContext

	lock sync.Mutex
	docs map[string]docInfo
}

var (
	_ openapi.GroupVersion            = &groupversion{}
	_ openapi.GroupVersionWithContext = &groupversion{}
)

type docInfo struct {
	data []byte
	err  error
}

func newGroupVersion(delegate openapi.GroupVersionWithContext) *groupversion {
	return &groupversion{
		delegate: delegate,
	}
}

// Deprecated: use SchemaWithContext instead.
func (g *groupversion) Schema(contentType string) ([]byte, error) {
	return g.SchemaWithContext(context.Background(), contentType)
}

func (g *groupversion) SchemaWithContext(ctx context.Context, contentType string) ([]byte, error) {
	g.lock.Lock()
	defer g.lock.Unlock()

	cachedInfo, ok := g.docs[contentType]
	if !ok {
		if g.docs == nil {
			g.docs = make(map[string]docInfo)
		}

		cachedInfo.data, cachedInfo.err = g.delegate.SchemaWithContext(ctx, contentType)
		g.docs[contentType] = cachedInfo
	}

	return cachedInfo.data, cachedInfo.err
}

func (c *groupversion) ServerRelativeURL() string {
	return c.delegate.ServerRelativeURL()
}
