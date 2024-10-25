/*
Copyright 2022 The Kubernetes Authors.

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

package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// CredentialProviderRequest includes the image that the kubelet requires authentication for.
// Kubelet will pass this request object to the plugin via stdin. In general, plugins should
// prefer responding with the same apiVersion they were sent.
type CredentialProviderRequest struct {
	metav1.TypeMeta `json:",inline"`

	// image is the container image that is being pulled as part of the
	// credential provider plugin request. Plugins may optionally parse the image
	// to extract any information required to fetch credentials.
	Image string `json:"image"`

	// serviceAccountToken is the service account token bound to the pod for which
	// the image is being pulled. This token is only sent to the plugin if the
	// tokenAttributes.serviceAccountTokenAudience field is configured in the kubelet's credential
	// provider configuration.
	ServiceAccountToken string `json:"serviceAccountToken,omitempty" datapolicy:"token"`

	// serviceAccountAnnotations is a map of annotations on the service account bound to the
	// pod for which the image is being pulled. The list of annotations in the service account
	// that need to be passed to the plugin is configured in the kubelet's credential provider
	// configuration.
	ServiceAccountAnnotations map[string]string `json:"serviceAccountAnnotations,omitempty"`
}

type PluginCacheKeyType string

const (
	// ImagePluginCacheKeyType means the kubelet will cache credentials on a per-image basis,
	// using the image passed from the kubelet directly as the cache key. This includes
	// the registry domain, port (if specified), and path but does not include tags or SHAs.
	ImagePluginCacheKeyType PluginCacheKeyType = "Image"
	// RegistryPluginCacheKeyType means the kubelet will cache credentials on a per-registry basis.
	// The cache key will be based on the registry domain and port (if present) parsed from the requested image.
	RegistryPluginCacheKeyType PluginCacheKeyType = "Registry"
	// GlobalPluginCacheKeyType means the kubelet will cache credentials for all images that
	// match for a given plugin. This cache key should only be returned by plugins that do not use
	// the image input at all.
	GlobalPluginCacheKeyType PluginCacheKeyType = "Global"
)

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// CredentialProviderResponse holds credentials that the kubelet should use for the specified
// image provided in the original request. Kubelet will read the response from the plugin via stdout.
// This response should be set to the same apiVersion as CredentialProviderRequest.
type CredentialProviderResponse struct {
	metav1.TypeMeta `json:",inline"`

	// cacheKeyType indiciates the type of caching key to use based on the image provided
	// in the request. There are three valid values for the cache key type: Image, Registry, and
	// Global. If an invalid value is specified, the response will NOT be used by the kubelet.
	CacheKeyType PluginCacheKeyType `json:"cacheKeyType"`

	// cacheDuration indicates the duration the provided credentials should be cached for.
	// The kubelet will use this field to set the in-memory cache duration for credentials
	// in the AuthConfig. If null, the kubelet will use defaultCacheDuration provided in
	// CredentialProviderConfig. If set to 0, the kubelet will not cache the provided AuthConfig.
	// +optional
	CacheDuration *metav1.Duration `json:"cacheDuration,omitempty"`

	// auth is a map containing authentication information passed into the kubelet.
	// Each key is a match image string (more on this below). The corresponding authConfig value
	// should be valid for all images that match against this key. A plugin should set
	// this field to null if no valid credentials can be returned for the requested image.
	//
	// Each key in the map is a pattern which can optionally contain a port and a path.
	// Globs can be used in the domain, but not in the port or the path. Globs are supported
	// as subdomains like '*.k8s.io' or 'k8s.*.io', and top-level-domains such as 'k8s.*'.
	// Matching partial subdomains like 'app*.k8s.io' is also supported. Each glob can only match
	// a single subdomain segment, so *.io does not match *.k8s.io.
	//
	// The kubelet will match images against the key when all of the below are true:
	// - Both contain the same number of domain parts and each part matches.
	// - The URL path of an imageMatch must be a prefix of the target image URL path.
	// - If the imageMatch contains a port, then the port must match in the image as well.
	//
	// When multiple keys are returned, the kubelet will traverse all keys in reverse order so that:
	// - longer keys come before shorter keys with the same prefix
	// - non-wildcard keys come before wildcard keys with the same prefix.
	//
	// For any given match, the kubelet will attempt an image pull with the provided credentials,
	// stopping after the first successfully authenticated pull.
	//
	// Example keys:
	//   - 123456789.dkr.ecr.us-east-1.amazonaws.com
	//   - *.azurecr.io
	//   - gcr.io
	//   - *.*.registry.io
	//   - registry.io:8080/path
	// +optional
	Auth map[string]AuthConfig `json:"auth,omitempty"`
}

// AuthConfig contains authentication information for a container registry.
// Only username/password based authentication is supported today, but more authentication
// mechanisms may be added in the future.
type AuthConfig struct {
	// username is the username used for authenticating to the container registry
	// An empty username is valid.
	Username string `json:"username"`

	// password is the password used for authenticating to the container registry
	// An empty password is valid.
	Password string `json:"password"`
}
