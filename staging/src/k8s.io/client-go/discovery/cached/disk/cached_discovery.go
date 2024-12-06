/*
Copyright 2016 The Kubernetes Authors.

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

package disk

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	openapi_v2 "github.com/google/gnostic-models/openapiv2"
	"k8s.io/klog/v2"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/version"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/openapi"
	cachedopenapi "k8s.io/client-go/openapi/cached"
	restclient "k8s.io/client-go/rest"
)

// CachedDiscoveryClient implements the functions that discovery server-supported API groups,
// versions and resources.
type CachedDiscoveryClient struct {
	delegate discovery.DiscoveryInterfaceWithContext

	// cacheDirectory is the directory where discovery docs are held.  It must be unique per host:port combination to work well.
	cacheDirectory string

	// ttl is how long the cache should be considered valid
	ttl time.Duration

	// mutex protects the variables below
	mutex sync.Mutex

	// ourFiles are all filenames of cache files created by this process
	ourFiles map[string]struct{}
	// invalidated is true if all cache files should be ignored that are not ours (e.g. after Invalidate() was called)
	invalidated bool
	// fresh is true if all used cache files were ours
	fresh bool

	// caching openapi v3 client which wraps the delegate's client
	openapiClient *cachedopenapi.Client
}

var (
	_ discovery.CachedDiscoveryInterface            = &CachedDiscoveryClient{}
	_ discovery.CachedDiscoveryInterfaceWithContext = &CachedDiscoveryClient{}
)

// ServerResourcesForGroupVersion returns the supported resources for a group and version.
//
// Deprecated: use ServerResourcesForGroupVersionWithContext instead.
func (d *CachedDiscoveryClient) ServerResourcesForGroupVersion(groupVersion string) (*metav1.APIResourceList, error) {
	return d.ServerResourcesForGroupVersionWithContext(context.Background(), groupVersion)
}

// ServerResourcesForGroupVersionWithContext returns the supported resources for a group and version.
func (d *CachedDiscoveryClient) ServerResourcesForGroupVersionWithContext(ctx context.Context, groupVersion string) (*metav1.APIResourceList, error) {
	filename := filepath.Join(d.cacheDirectory, groupVersion, "serverresources.json")
	cachedBytes, err := d.getCachedFile(filename)
	// don't fail on errors, we either don't have a file or won't be able to run the cached check. Either way we can fallback.
	if err == nil {
		cachedResources := &metav1.APIResourceList{}
		if err := runtime.DecodeInto(scheme.Codecs.UniversalDecoder(), cachedBytes, cachedResources); err == nil {
			klog.FromContext(ctx).V(10).Info("Returning cached discovery info", "fileName", filename)
			return cachedResources, nil
		}
	}

	liveResources, err := d.delegate.ServerResourcesForGroupVersionWithContext(ctx, groupVersion)
	if err != nil {
		klog.FromContext(ctx).V(3).Info("Skipped caching discovery info due to error", "err", err)
		return liveResources, err
	}
	if liveResources == nil || len(liveResources.APIResources) == 0 {
		klog.FromContext(ctx).V(3).Info("skipped caching discovery info, no resources found")
		return liveResources, err
	}

	if err := d.writeCachedFile(filename, liveResources); err != nil {
		klog.FromContext(ctx).V(1).Info("Failed to write cache", "fileName", filename, "err", err)
	}

	return liveResources, nil
}

// ServerGroupsAndResources returns the supported groups and resources for all groups and versions.
//
// Deprecated: use ServerGroupsAndResourcesWithContext instead.
func (d *CachedDiscoveryClient) ServerGroupsAndResources() ([]*metav1.APIGroup, []*metav1.APIResourceList, error) {
	return d.ServerGroupsAndResourcesWithContext(context.Background())
}

// ServerGroupsAndResourcesWithContext returns the supported groups and resources for all groups and versions.
func (d *CachedDiscoveryClient) ServerGroupsAndResourcesWithContext(ctx context.Context) ([]*metav1.APIGroup, []*metav1.APIResourceList, error) {
	return discovery.ServerGroupsAndResourcesWithContext(ctx, d)
}

// ServerGroups returns the supported groups, with information like supported versions and the
// preferred version.
//
// Deprecated: use ServerGroupsWithContext instead.
func (d *CachedDiscoveryClient) ServerGroups() (*metav1.APIGroupList, error) {
	return d.ServerGroupsWithContext(context.Background())
}

// ServerGroupsWithContext returns the supported groups, with information like supported versions and the
// preferred version.
func (d *CachedDiscoveryClient) ServerGroupsWithContext(ctx context.Context) (*metav1.APIGroupList, error) {
	filename := filepath.Join(d.cacheDirectory, "servergroups.json")
	cachedBytes, err := d.getCachedFile(filename)
	// don't fail on errors, we either don't have a file or won't be able to run the cached check. Either way we can fallback.
	if err == nil {
		cachedGroups := &metav1.APIGroupList{}
		if err := runtime.DecodeInto(scheme.Codecs.UniversalDecoder(), cachedBytes, cachedGroups); err == nil {
			klog.FromContext(ctx).V(10).Info("Returning cached discovery info", "fileName", filename)
			return cachedGroups, nil
		}
	}

	liveGroups, err := d.delegate.ServerGroupsWithContext(ctx)
	if err != nil {
		klog.FromContext(ctx).V(3).Info("Skipped caching discovery info due to error", "err", err)
		return liveGroups, err
	}
	if liveGroups == nil || len(liveGroups.Groups) == 0 {
		klog.FromContext(ctx).V(3).Info("Skipped caching discovery info, no groups found")
		return liveGroups, err
	}

	if err := d.writeCachedFile(filename, liveGroups); err != nil {
		klog.FromContext(ctx).V(1).Info("Failed to write cache", "fileName", filename, "err", err)
	}

	return liveGroups, nil
}

func (d *CachedDiscoveryClient) getCachedFile(filename string) ([]byte, error) {
	// after invalidation ignore cache files not created by this process
	d.mutex.Lock()
	_, ourFile := d.ourFiles[filename]
	if d.invalidated && !ourFile {
		d.mutex.Unlock()
		return nil, errors.New("cache invalidated")
	}
	d.mutex.Unlock()

	file, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	fileInfo, err := file.Stat()
	if err != nil {
		return nil, err
	}

	if time.Now().After(fileInfo.ModTime().Add(d.ttl)) {
		return nil, errors.New("cache expired")
	}

	// the cache is present and its valid.  Try to read and use it.
	cachedBytes, err := io.ReadAll(file)
	if err != nil {
		return nil, err
	}

	d.mutex.Lock()
	defer d.mutex.Unlock()
	d.fresh = d.fresh && ourFile

	return cachedBytes, nil
}

func (d *CachedDiscoveryClient) writeCachedFile(filename string, obj runtime.Object) error {
	if err := os.MkdirAll(filepath.Dir(filename), 0750); err != nil {
		return err
	}

	bytes, err := runtime.Encode(scheme.Codecs.LegacyCodec(), obj)
	if err != nil {
		return err
	}

	f, err := os.CreateTemp(filepath.Dir(filename), filepath.Base(filename)+".")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = f.Write(bytes)
	if err != nil {
		return err
	}

	err = os.Chmod(f.Name(), 0660)
	if err != nil {
		return err
	}

	name := f.Name()
	err = f.Close()
	if err != nil {
		return err
	}

	// atomic rename
	d.mutex.Lock()
	defer d.mutex.Unlock()
	err = os.Rename(name, filename)
	if err == nil {
		d.ourFiles[filename] = struct{}{}
	}
	return err
}

// RESTClient returns a RESTClient that is used to communicate with API server
// by this client implementation.
func (d *CachedDiscoveryClient) RESTClient() restclient.Interface {
	return d.delegate.RESTClient()
}

// ServerPreferredResources returns the supported resources with the version preferred by the
// server.
//
// Deprecated: use ServerPreferredResourcesWithContext instead.
func (d *CachedDiscoveryClient) ServerPreferredResources() ([]*metav1.APIResourceList, error) {
	return d.ServerPreferredResourcesWithContext(context.Background())
}

// ServerPreferredResourcesWithContext returns the supported resources with the version preferred by the
// server.
func (d *CachedDiscoveryClient) ServerPreferredResourcesWithContext(ctx context.Context) ([]*metav1.APIResourceList, error) {
	return discovery.ServerPreferredResourcesWithContext(ctx, d)
}

// ServerPreferredNamespacedResources returns the supported namespaced resources with the
// version preferred by the server.
//
// Deprecated: use ServerPreferredNamespacedResourcesWithContext instead.
func (d *CachedDiscoveryClient) ServerPreferredNamespacedResources() ([]*metav1.APIResourceList, error) {
	return d.ServerPreferredNamespacedResourcesWithContext(context.Background())
}

// ServerPreferredNamespacedResourcesWithContext returns the supported namespaced resources with the
// version preferred by the server.
func (d *CachedDiscoveryClient) ServerPreferredNamespacedResourcesWithContext(ctx context.Context) ([]*metav1.APIResourceList, error) {
	return discovery.ServerPreferredNamespacedResourcesWithContext(ctx, d)
}

// ServerVersion retrieves and parses the server's version (git version).
//
// Deprecated: use ServerVersionWithContext instead.
func (d *CachedDiscoveryClient) ServerVersion() (*version.Info, error) {
	return d.ServerVersionWithContext(context.Background())
}

// ServerVersionWithContext retrieves and parses the server's version (git version).
func (d *CachedDiscoveryClient) ServerVersionWithContext(ctx context.Context) (*version.Info, error) {
	return d.delegate.ServerVersionWithContext(ctx)
}

// OpenAPISchema retrieves and parses the swagger API schema the server supports.
//
// Deprecated: use OpenAPISchemaWithContext instead.
func (d *CachedDiscoveryClient) OpenAPISchema() (*openapi_v2.Document, error) {
	return d.OpenAPISchemaWithContext(context.Background())
}

// OpenAPISchemaWithContext retrieves and parses the swagger API schema the server supports.
func (d *CachedDiscoveryClient) OpenAPISchemaWithContext(ctx context.Context) (*openapi_v2.Document, error) {
	return d.delegate.OpenAPISchemaWithContext(ctx)
}

// OpenAPIV3 retrieves and parses the OpenAPIV3 specs exposed by the server
func (d *CachedDiscoveryClient) OpenAPIV3() openapi.Client {
	return d.openAPIV3(context.Background())
}

// OpenAPIV3WithContext retrieves and parses the OpenAPIV3 specs exposed by the server
func (d *CachedDiscoveryClient) OpenAPIV3WithContext(ctx context.Context) openapi.ClientWithContext {
	return d.openAPIV3(ctx)
}

func (d *CachedDiscoveryClient) openAPIV3(ctx context.Context) *cachedopenapi.Client {
	// Must take lock since Invalidate call may modify openapiClient
	d.mutex.Lock()
	defer d.mutex.Unlock()

	if d.openapiClient == nil {
		// Delegate is discovery client created with special HTTP client which
		// respects E-Tag cache responses to serve cache from disk.
		d.openapiClient = cachedopenapi.NewClientWithContext(d.delegate.OpenAPIV3WithContext(ctx))
	}

	return d.openapiClient
}

// Fresh is supposed to tell the caller whether or not to retry if the cache
// fails to find something (false = retry, true = no need to retry).
//
// Deprecated: use FreshWithContext instead.
func (d *CachedDiscoveryClient) Fresh() bool {
	return d.FreshWithContext(context.Background())
}

// FreshWithContext is supposed to tell the caller whether or not to retry if the cache
// fails to find something (false = retry, true = no need to retry).
func (d *CachedDiscoveryClient) FreshWithContext(ctx context.Context) bool {
	d.mutex.Lock()
	defer d.mutex.Unlock()

	return d.fresh
}

// Invalidate enforces that no cached data is used in the future that is older than the current time.
//
// Deprecated: use InvalidateWithContext instead.
func (d *CachedDiscoveryClient) Invalidate() {
	d.InvalidateWithContext(context.Background())
}

// InvalidateWithContext enforces that no cached data is used in the future that is older than the current time.
func (d *CachedDiscoveryClient) InvalidateWithContext(ctx context.Context) {
	d.mutex.Lock()
	defer d.mutex.Unlock()

	d.ourFiles = map[string]struct{}{}
	d.fresh = true
	d.invalidated = true
	d.openapiClient = nil
	ad, ok := d.delegate.(discovery.CachedDiscoveryInterfaceWithContext)
	if !ok {
		if ad2, ok2 := d.delegate.(discovery.CachedDiscoveryInterface); ok2 {
			ad = discovery.ToCachedDiscoveryInterfaceWithContext(ad2)
			ok = true
		}
	}
	if ok {
		ad.InvalidateWithContext(ctx)
	}
}

// WithLegacy returns current cached discovery client;
// current client does not support legacy-only discovery.
func (d *CachedDiscoveryClient) WithLegacy() discovery.DiscoveryInterface {
	return d
}

// WithLegacyWithContext returns current cached discovery client;
// current client does not support legacy-only discovery.
func (d *CachedDiscoveryClient) WithLegacyWithContext(ctx context.Context) discovery.DiscoveryInterfaceWithContext {
	return d
}

// NewCachedDiscoveryClientForConfig creates a new DiscoveryClient for the given config, and wraps
// the created client in a CachedDiscoveryClient. The provided configuration is updated with a
// custom transport that understands cache responses.
// We receive two distinct cache directories for now, in order to preserve old behavior
// which makes use of the --cache-dir flag value for storing cache data from the CacheRoundTripper,
// and makes use of the hardcoded destination (~/.kube/cache/discovery/...) for storing
// CachedDiscoveryClient cache data. If httpCacheDir is empty, the restconfig's transport will not
// be updated with a roundtripper that understands cache responses.
// If discoveryCacheDir is empty, cached server resource data will be looked up in the current directory.
func NewCachedDiscoveryClientForConfig(config *restclient.Config, discoveryCacheDir, httpCacheDir string, ttl time.Duration) (*CachedDiscoveryClient, error) {
	if len(httpCacheDir) > 0 {
		// update the given restconfig with a custom roundtripper that
		// understands how to handle cache responses.
		config = restclient.CopyConfig(config)
		config.Wrap(func(rt http.RoundTripper) http.RoundTripper {
			return newCacheRoundTripper(httpCacheDir, rt)
		})
	}

	discoveryClient, err := discovery.NewDiscoveryClientForConfig(config)
	if err != nil {
		return nil, err
	}

	// The delegate caches the discovery groups and resources (memcache). "ServerGroups",
	// which usually only returns (and caches) the groups, can now store the resources as
	// well if the server supports the newer aggregated discovery format.
	return newCachedDiscoveryClient(memory.NewMemCacheClientWithContext(discoveryClient), discoveryCacheDir, ttl), nil
}

// NewCachedDiscoveryClient creates a new DiscoveryClient.  cacheDirectory is the directory where discovery docs are held.  It must be unique per host:port combination to work well.
func newCachedDiscoveryClient(delegate discovery.DiscoveryInterfaceWithContext, cacheDirectory string, ttl time.Duration) *CachedDiscoveryClient {
	return &CachedDiscoveryClient{
		delegate:       delegate,
		cacheDirectory: cacheDirectory,
		ttl:            ttl,
		ourFiles:       map[string]struct{}{},
		fresh:          true,
	}
}
