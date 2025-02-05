/*
Copyright 2020 The Kubernetes Authors.

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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ImagePullCredentialsVerificationPolicy is an enum for the policy that is enforced
// when pod is requesting an image that appears on the system
type ImagePullCredentialsVerificationPolicy string

const (
	// NeverVerify will never require credential verification for images that
	// already exist on the node
	NeverVerify ImagePullCredentialsVerificationPolicy = "NeverVerify"
	// NeverVerifyPreloadedImages does not require credential verification for images
	// pulled outside the kubelet process
	NeverVerifyPreloadedImages ImagePullCredentialsVerificationPolicy = "NeverVerifyPreloadedImages"
	// NeverVerifyAllowlistedImages does not require credential verification for
	// a list of images that were pulled outside the kubelet process
	NeverVerifyAllowlistedImages ImagePullCredentialsVerificationPolicy = "NeverVerifyAllowlistedImages"
	// AlwaysVerify requires credential verification for accessing any image on the
	// node irregardless how it was pulled
	AlwaysVerify ImagePullCredentialsVerificationPolicy = "AlwaysVerify"
)

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// CredentialProviderConfig is the configuration containing information about
// each exec credential provider. Kubelet reads this configuration from disk and enables
// each provider as specified by the CredentialProvider type.
type CredentialProviderConfig struct {
	metav1.TypeMeta `json:",inline"`

	// providers is a list of credential provider plugins that will be enabled by the kubelet.
	// Multiple providers may match against a single image, in which case credentials
	// from all providers will be returned to the kubelet. If multiple providers are called
	// for a single image, the results are combined. If providers return overlapping
	// auth keys, the value from the provider earlier in this list is attempted first.
	Providers []CredentialProvider `json:"providers"`
}

// CredentialProvider represents an exec plugin to be invoked by the kubelet. The plugin is only
// invoked when an image being pulled matches the images handled by the plugin (see matchImages).
type CredentialProvider struct {
	// name is the required name of the credential provider. It must match the name of the
	// provider executable as seen by the kubelet. The executable must be in the kubelet's
	// bin directory (set by the --image-credential-provider-bin-dir flag).
	// Required to be unique across all providers.
	Name string `json:"name"`

	// matchImages is a required list of strings used to match against images in order to
	// determine if this provider should be invoked. If one of the strings matches the
	// requested image from the kubelet, the plugin will be invoked and given a chance
	// to provide credentials. Images are expected to contain the registry domain
	// and URL path.
	//
	// Each entry in matchImages is a pattern which can optionally contain a port and a path.
	// Globs can be used in the domain, but not in the port or the path. Globs are supported
	// as subdomains like `*.k8s.io` or `k8s.*.io`, and top-level-domains such as `k8s.*`.
	// Matching partial subdomains like `app*.k8s.io` is also supported. Each glob can only match
	// a single subdomain segment, so `*.io` does not match `*.k8s.io`.
	//
	// A match exists between an image and a matchImage when all of the below are true:
	// - Both contain the same number of domain parts and each part matches.
	// - The URL path of an imageMatch must be a prefix of the target image URL path.
	// - If the imageMatch contains a port, then the port must match in the image as well.
	//
	// Example values of matchImages:
	//   - `123456789.dkr.ecr.us-east-1.amazonaws.com`
	//   - `*.azurecr.io`
	//   - `gcr.io`
	//   - `*.*.registry.io`
	//   - `registry.io:8080/path`
	MatchImages []string `json:"matchImages"`

	// defaultCacheDuration is the default duration the plugin will cache credentials in-memory
	// if a cache duration is not provided in the plugin response. This field is required.
	DefaultCacheDuration *metav1.Duration `json:"defaultCacheDuration"`

	// Required input version of the exec CredentialProviderRequest. The returned CredentialProviderResponse
	// MUST use the same encoding version as the input. Current supported values are:
	// - credentialprovider.kubelet.k8s.io/v1alpha1
	APIVersion string `json:"apiVersion"`

	// Arguments to pass to the command when executing it.
	// +optional
	Args []string `json:"args,omitempty"`

	// Env defines additional environment variables to expose to the process. These
	// are unioned with the host's environment, as well as variables client-go uses
	// to pass argument to the plugin.
	// +optional
	Env []ExecEnvVar `json:"env,omitempty"`
}

// ExecEnvVar is used for setting environment variables when executing an exec-based
// credential plugin.
type ExecEnvVar struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// ImagePullIntent is a record of the kubelet attempting to pull an image.
//
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type ImagePullIntent struct {
	metav1.TypeMeta `json:",inline"`

	// Image is the image spec from a Container's `image` field.
	// The filename is a SHA-256 hash of this value. This is to avoid filename-unsafe
	// characters like ':' and '/'.
	Image string `json:"image"`
}

// ImagePullRecord is a record of an image that was pulled by the kubelet.
//
// If there are no records in the `kubernetesSecrets` field and both `nodeWideCredentials`
// and `anonymous` are `false`, credentials must be re-checked the next time an
// image represented by this record is being requested.
//
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type ImagePulledRecord struct {
	metav1.TypeMeta `json:",inline"`

	// LastUpdatedTime is the time of the last update to this record
	LastUpdatedTime metav1.Time `json:"lastUpdatedTime"`

	// ImageRef is a reference to the image represented by this file as received
	// from the CRI.
	// The filename is a SHA-256 hash of this value. This is to avoid filename-unsafe
	// characters like ':' and '/'.
	ImageRef string `json:"imageRef"`

	// CredentialMapping maps `image` to the set of credentials that it was
	// previously pulled with.
	// `image` in this case is the content of a pod's container `image` field that's
	// got its tag/digest removed.
	//
	// Example:
	//   Container requests the `hello-world:latest@sha256:91fb4b041da273d5a3273b6d587d62d518300a6ad268b28628f74997b93171b2` image:
	//     "credentialMapping": {
	//       "hello-world": { "nodePodsAccessible": true }
	//     }
	CredentialMapping map[string]ImagePullCredentials `json:"credentialMapping,omitempty"`
}

// ImagePullCredentials describe credentials that can be used to pull an image.
type ImagePullCredentials struct {
	// KuberneteSecretCoordinates is an index of coordinates of all the kubernetes
	// secrets that were used to pull the image.
	// +optional
	KubernetesSecrets []ImagePullSecret `json:"kubernetesSecrets"`

	// NodePodsAccessible is a flag denoting the pull credentials are accessible
	// by all the pods on the node, or that no credentials are needed for the pull.
	//
	// If true, it is mutually exclusive with the `kubernetesSecrets` field.
	// +optional
	NodePodsAccessible bool `json:"nodePodsAccessible,omitempty"`
}

// ImagePullSecret is a representation of a Kubernetes secret object coordinates along
// with a credential hash of the pull secret credentials this object contains.
type ImagePullSecret struct {
	UID       string `json:"uid"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`

	// CredentialHash is a SHA-256 retrieved by hashing the image pull credentials
	// content of the secret specified by the UID/Namespace/Name coordinates.
	CredentialHash string `json:"credentialHash"`
}
