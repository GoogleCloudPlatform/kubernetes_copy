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

package credentialprovider

import (
	"os"
	"reflect"
	"sync"
	"time"

	"k8s.io/klog/v2"
)

// DockerConfigProvider is the interface that registered extensions implement
// to materialize 'dockercfg' credentials.
type DockerConfigProvider interface {
	// Enabled returns true if the config provider is enabled.
	// Implementations can be blocking - e.g. metadata server unavailable.
	Enabled() bool
	// Provide returns docker configuration.
	// Implementations can be blocking - e.g. metadata server unavailable.
	// The image is passed in as context in the event that the
	// implementation depends on information in the image name to return
	// credentials; implementations are safe to ignore the image.
	Provide(image string) DockerConfig
}

// A DockerConfigProvider that simply reads the .dockercfg file
type defaultDockerConfigProvider struct{}

// CachingDockerConfigProvider implements DockerConfigProvider by composing
// with another DockerConfigProvider and caching the DockerConfig it provides
// for a pre-specified lifetime.
type CachingDockerConfigProvider struct {
	Provider DockerConfigProvider
	Lifetime time.Duration

	// ShouldCache is an optional function that returns true if the specific config should be cached.
	// If nil, all configs are treated as cacheable.
	ShouldCache func(DockerConfig) bool

	// cache fields
	cacheDockerConfig DockerConfig
	expiration        time.Time
	mu                sync.Mutex
}

// Enabled implements dockerConfigProvider
func (d *defaultDockerConfigProvider) Enabled() bool {
	return true
}

// Provide implements dockerConfigProvider
func (d *defaultDockerConfigProvider) Provide(image string) DockerConfig {
	// Read the standard Docker credentials from .dockercfg
	if cfg, err := ReadDockerConfigFile(); err == nil {
		return cfg
	} else if !os.IsNotExist(err) {
		klog.V(2).Infof("Docker config file not found: %v", err)
	}
	return DockerConfig{}
}

// Enabled implements dockerConfigProvider
func (d *CachingDockerConfigProvider) Enabled() bool {
	return d.Provider.Enabled()
}

// Provide implements dockerConfigProvider
func (d *CachingDockerConfigProvider) Provide(image string) DockerConfig {
	d.mu.Lock()
	defer d.mu.Unlock()

	// If the cache hasn't expired, return our cache
	if time.Now().Before(d.expiration) {
		return d.cacheDockerConfig
	}

	klog.V(2).Infof("Refreshing cache for provider: %v", reflect.TypeOf(d.Provider).String())
	config := d.Provider.Provide(image)
	if d.ShouldCache == nil || d.ShouldCache(config) {
		d.cacheDockerConfig = config
		d.expiration = time.Now().Add(d.Lifetime)
	}
	return config
}

// NewDefaultDockerKeyring creates a DockerKeyring to use for resolving credentials,
// which returns the default credentials from the .dockercfg file.
func NewDefaultDockerKeyring() DockerKeyring {
	return &providersDockerKeyring{
		Providers: []DockerConfigProvider{
			&CachingDockerConfigProvider{
				Provider: &defaultDockerConfigProvider{},
				Lifetime: 5 * time.Minute,
			},
		},
	}
}
