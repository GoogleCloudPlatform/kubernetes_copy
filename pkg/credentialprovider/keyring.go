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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/url"
	"path/filepath"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/util/sets"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/features"
)

// DockerKeyring tracks a set of docker registry credentials, maintaining a
// reverse index across the registry endpoints. A registry endpoint is made
// up of a host (e.g. registry.example.com), but it may also contain a path
// (e.g. registry.example.com/foo) This index is important for two reasons:
//   - registry endpoints may overlap, and when this happens we must find the
//     most specific match for a given image
//   - iterating a map does not yield predictable results
type DockerKeyring interface {
	Lookup(image string) ([]TrackedAuthConfig, bool)
}

// BasicDockerKeyring is a trivial map-backed implementation of DockerKeyring
type BasicDockerKeyring struct {
	index []string
	creds map[string][]TrackedAuthConfig
}

// providersDockerKeyring is an implementation of DockerKeyring that
// materializes its dockercfg based on a set of dockerConfigProviders.
type providersDockerKeyring struct {
	Providers []DockerConfigProvider
}

// TrackedAuthConfig wraps the AuthConfig and adds information about the source
// of the credentials.
type TrackedAuthConfig struct {
	AuthConfig
	AuthConfigHash string

	Source *CredentialSource
}

// NewTrackedAuthConfig initializes the TrackedAuthConfig structure by adding
// the source information to the supplied AuthConfig. It also counts a hash of the
// AuthConfig and keeps it in the returned structure.
//
// The supplied CredentialSource is only used when the "KubeletEnsureSecretPulledImages"
// is enabled, the same applies for counting the hash.
func NewTrackedAuthConfig(c *AuthConfig, src *CredentialSource) *TrackedAuthConfig {
	if c == nil {
		panic("cannot construct TrackedAuthConfig with a nil AuthConfig")
	}

	authConfig := &TrackedAuthConfig{
		AuthConfig: *c,
	}

	if utilfeature.DefaultFeatureGate.Enabled(features.KubeletEnsureSecretPulledImages) {
		authConfig.Source = src
		authConfig.AuthConfigHash = hashAuthConfig(c)
	}
	return authConfig
}

type CredentialSource struct {
	Secret SecretCoordinates
}

type SecretCoordinates struct {
	UID       string
	Namespace string
	Name      string
}

// AuthConfig contains authorization information for connecting to a Registry
// This type mirrors "github.com/docker/docker/api/types.AuthConfig"
type AuthConfig struct {
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	Auth     string `json:"auth,omitempty"`

	// Email is an optional value associated with the username.
	// This field is deprecated and will be removed in a later
	// version of docker.
	Email string `json:"email,omitempty"`

	ServerAddress string `json:"serveraddress,omitempty"`

	// IdentityToken is used to authenticate the user and get
	// an access token for the registry.
	IdentityToken string `json:"identitytoken,omitempty"`

	// RegistryToken is a bearer token to be sent to a registry
	RegistryToken string `json:"registrytoken,omitempty"`
}

// Add inserts the docker config `cfg` into the basic docker keyring. It attaches
// the `src` information that describes where the docker config `cfg` comes from.
// `src` is nil if the docker config is globally available on the node.
func (dk *BasicDockerKeyring) Add(src *CredentialSource, cfg DockerConfig) {
	if dk.index == nil {
		dk.index = make([]string, 0)
		dk.creds = make(map[string][]TrackedAuthConfig)
	}
	for loc, ident := range cfg {
		creds := AuthConfig{
			Username: ident.Username,
			Password: ident.Password,
			Email:    ident.Email,
		}

		value := loc
		if !strings.HasPrefix(value, "https://") && !strings.HasPrefix(value, "http://") {
			value = "https://" + value
		}
		parsed, err := url.Parse(value)
		if err != nil {
			klog.Errorf("Entry %q in dockercfg invalid (%v), ignoring", loc, err)
			continue
		}

		// The docker client allows exact matches:
		//    foo.bar.com/namespace
		// Or hostname matches:
		//    foo.bar.com
		// It also considers /v2/  and /v1/ equivalent to the hostname
		// See ResolveAuthConfig in docker/registry/auth.go.
		effectivePath := parsed.Path
		if strings.HasPrefix(effectivePath, "/v2/") || strings.HasPrefix(effectivePath, "/v1/") {
			effectivePath = effectivePath[3:]
		}
		var key string
		if (len(effectivePath) > 0) && (effectivePath != "/") {
			key = parsed.Host + effectivePath
		} else {
			key = parsed.Host
		}
		trackedCreds := NewTrackedAuthConfig(&creds, src)

		dk.creds[key] = append(dk.creds[key], *trackedCreds)
		dk.index = append(dk.index, key)
	}

	eliminateDupes := sets.NewString(dk.index...)
	dk.index = eliminateDupes.List()

	// Update the index used to identify which credentials to use for a given
	// image. The index is reverse-sorted so more specific paths are matched
	// first. For example, if for the given image "gcr.io/etcd-development/etcd",
	// credentials for "quay.io/coreos" should match before "quay.io".
	sort.Sort(sort.Reverse(sort.StringSlice(dk.index)))
}

const (
	defaultRegistryHost = "index.docker.io"
	defaultRegistryKey  = defaultRegistryHost + "/v1/"
)

// isDefaultRegistryMatch determines whether the given image will
// pull from the default registry (DockerHub) based on the
// characteristics of its name.
func isDefaultRegistryMatch(image string) bool {
	parts := strings.SplitN(image, "/", 2)

	if len(parts[0]) == 0 {
		return false
	}

	if len(parts) == 1 {
		// e.g. library/ubuntu
		return true
	}

	if parts[0] == "docker.io" || parts[0] == "index.docker.io" {
		// resolve docker.io/image and index.docker.io/image as default registry
		return true
	}

	// From: http://blog.docker.com/2013/07/how-to-use-your-own-registry/
	// Docker looks for either a “.” (domain separator) or “:” (port separator)
	// to learn that the first part of the repository name is a location and not
	// a user name.
	return !strings.ContainsAny(parts[0], ".:")
}

// ParseSchemelessURL parses a schemeless url and returns a url.URL
// url.Parse require a scheme, but ours don't have schemes.  Adding a
// scheme to make url.Parse happy, then clear out the resulting scheme.
func ParseSchemelessURL(schemelessURL string) (*url.URL, error) {
	parsed, err := url.Parse("https://" + schemelessURL)
	if err != nil {
		return nil, err
	}
	// clear out the resulting scheme
	parsed.Scheme = ""
	return parsed, nil
}

// SplitURL splits the host name into parts, as well as the port
func SplitURL(url *url.URL) (parts []string, port string) {
	host, port, err := net.SplitHostPort(url.Host)
	if err != nil {
		// could not parse port
		host, port = url.Host, ""
	}
	return strings.Split(host, "."), port
}

// URLsMatchStr is wrapper for URLsMatch, operating on strings instead of URLs.
func URLsMatchStr(glob string, target string) (bool, error) {
	globURL, err := ParseSchemelessURL(glob)
	if err != nil {
		return false, err
	}
	targetURL, err := ParseSchemelessURL(target)
	if err != nil {
		return false, err
	}
	return URLsMatch(globURL, targetURL)
}

// URLsMatch checks whether the given target url matches the glob url, which may have
// glob wild cards in the host name.
//
// Examples:
//
//	globURL=*.docker.io, targetURL=blah.docker.io => match
//	globURL=*.docker.io, targetURL=not.right.io   => no match
//
// Note that we don't support wildcards in ports and paths yet.
func URLsMatch(globURL *url.URL, targetURL *url.URL) (bool, error) {
	globURLParts, globPort := SplitURL(globURL)
	targetURLParts, targetPort := SplitURL(targetURL)
	if globPort != targetPort {
		// port doesn't match
		return false, nil
	}
	if len(globURLParts) != len(targetURLParts) {
		// host name does not have the same number of parts
		return false, nil
	}
	if !strings.HasPrefix(targetURL.Path, globURL.Path) {
		// the path of the credential must be a prefix
		return false, nil
	}
	for k, globURLPart := range globURLParts {
		targetURLPart := targetURLParts[k]
		matched, err := filepath.Match(globURLPart, targetURLPart)
		if err != nil {
			return false, err
		}
		if !matched {
			// glob mismatch for some part
			return false, nil
		}
	}
	// everything matches
	return true, nil
}

// Lookup implements the DockerKeyring method for fetching credentials based on image name.
// Multiple credentials may be returned if there are multiple potentially valid credentials
// available.  This allows for rotation.
func (dk *BasicDockerKeyring) Lookup(image string) ([]TrackedAuthConfig, bool) {
	// range over the index as iterating over a map does not provide a predictable ordering
	ret := []TrackedAuthConfig{}
	for _, k := range dk.index {
		// both k and image are schemeless URLs because even though schemes are allowed
		// in the credential configurations, we remove them in Add.
		if matched, _ := URLsMatchStr(k, image); matched {
			ret = append(ret, dk.creds[k]...)
		}
	}

	if len(ret) > 0 {
		return ret, true
	}

	// Use credentials for the default registry if provided, and appropriate
	if isDefaultRegistryMatch(image) {
		if auth, ok := dk.creds[defaultRegistryHost]; ok {
			return auth, true
		}
	}

	return []TrackedAuthConfig{}, false
}

// Lookup implements the DockerKeyring method for fetching credentials
// based on image name.
func (dk *providersDockerKeyring) Lookup(image string) ([]TrackedAuthConfig, bool) {
	keyring := &BasicDockerKeyring{}

	for _, p := range dk.Providers {
		// TODO: the source should probably change once we depend on service accounts (KEP-4412).
		//       Perhaps `Provide()` should return the source modified to accommodate this?
		keyring.Add(nil, p.Provide(image))
	}

	return keyring.Lookup(image)
}

// FakeKeyring a fake config credentials
type FakeKeyring struct {
	auth []TrackedAuthConfig
	ok   bool
}

// Lookup implements the DockerKeyring method for fetching credentials based on image name
// return fake auth and ok
func (f *FakeKeyring) Lookup(image string) ([]TrackedAuthConfig, bool) {
	return f.auth, f.ok
}

// UnionDockerKeyring delegates to a set of keyrings.
type UnionDockerKeyring []DockerKeyring

// Lookup implements the DockerKeyring method for fetching credentials based on image name.
// return each credentials
func (k UnionDockerKeyring) Lookup(image string) ([]TrackedAuthConfig, bool) {
	authConfigs := []TrackedAuthConfig{}
	for _, subKeyring := range k {
		if subKeyring == nil {
			continue
		}

		currAuthResults, _ := subKeyring.Lookup(image)
		authConfigs = append(authConfigs, currAuthResults...)
	}

	return authConfigs, (len(authConfigs) > 0)
}

func hashAuthConfig(creds *AuthConfig) string {
	credBytes, err := json.Marshal(creds)
	if err != nil {
		return ""
	}

	hash := sha256.New()
	hash.Write([]byte(credBytes))
	return hex.EncodeToString(hash.Sum(nil))
}
