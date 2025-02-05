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

type Client struct {
	delegate openapi.ClientWithContext

	once   sync.Once
	result map[string]openapi.GroupVersionWithContext
	err    error
}

var (
	_ openapi.Client            = &Client{}
	_ openapi.ClientWithContext = &Client{}
)

// Deprecated: use NewClientWithContext instead.
func NewClient(other openapi.Client) openapi.Client {
	return newClient(openapi.ToClientWithContext(other))
}

func NewClientWithContext(other openapi.ClientWithContext) *Client {
	return newClient(other)
}

func newClient(other openapi.ClientWithContext) *Client {
	return &Client{
		delegate: other,
	}
}

// Deprecated: use PathsWithContext instead.
func (c *Client) Paths() (map[string]openapi.GroupVersion, error) {
	// For efficiency reasons in the *WithContext case this is a map to
	// openapi.GroupVersionWithContext. But we know that all entries
	// also implement openapi.GroupVersion.
	resultWithContext, err := c.PathsWithContext(context.Background())
	result := make(map[string]openapi.GroupVersion, len(resultWithContext))
	for key, entry := range resultWithContext {
		result[key] = entry.(openapi.GroupVersion)
	}
	return result, err
}

func (c *Client) PathsWithContext(ctx context.Context) (map[string]openapi.GroupVersionWithContext, error) {
	c.once.Do(func() {
		uncached, err := c.delegate.PathsWithContext(ctx)
		if err != nil {
			c.err = err
			return
		}

		result := make(map[string]openapi.GroupVersionWithContext, len(uncached))
		for k, v := range uncached {
			result[k] = newGroupVersion(v)
		}
		c.result = result
	})
	return c.result, c.err
}
