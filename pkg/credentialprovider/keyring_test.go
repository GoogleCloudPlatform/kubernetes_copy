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
	"encoding/base64"
	"fmt"
	"reflect"
	"testing"

	utilfeature "k8s.io/apiserver/pkg/util/feature"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	"k8s.io/kubernetes/pkg/features"
)

func TestURLsMatch(t *testing.T) {
	tests := []struct {
		globURL       string
		targetURL     string
		matchExpected bool
	}{
		// match when there is no path component
		{
			globURL:       "*.kubernetes.io",
			targetURL:     "prefix.kubernetes.io",
			matchExpected: true,
		},
		{
			globURL:       "prefix.*.io",
			targetURL:     "prefix.kubernetes.io",
			matchExpected: true,
		},
		{
			globURL:       "prefix.kubernetes.*",
			targetURL:     "prefix.kubernetes.io",
			matchExpected: true,
		},
		{
			globURL:       "*-good.kubernetes.io",
			targetURL:     "prefix-good.kubernetes.io",
			matchExpected: true,
		},
		// match with path components
		{
			globURL:       "*.kubernetes.io/blah",
			targetURL:     "prefix.kubernetes.io/blah",
			matchExpected: true,
		},
		{
			globURL:       "prefix.*.io/foo",
			targetURL:     "prefix.kubernetes.io/foo/bar",
			matchExpected: true,
		},
		// match with path components and ports
		{
			globURL:       "*.kubernetes.io:1111/blah",
			targetURL:     "prefix.kubernetes.io:1111/blah",
			matchExpected: true,
		},
		{
			globURL:       "prefix.*.io:1111/foo",
			targetURL:     "prefix.kubernetes.io:1111/foo/bar",
			matchExpected: true,
		},
		// no match when number of parts mismatch
		{
			globURL:       "*.kubernetes.io",
			targetURL:     "kubernetes.io",
			matchExpected: false,
		},
		{
			globURL:       "*.*.kubernetes.io",
			targetURL:     "prefix.kubernetes.io",
			matchExpected: false,
		},
		{
			globURL:       "*.*.kubernetes.io",
			targetURL:     "kubernetes.io",
			matchExpected: false,
		},
		{
			globURL:       "*kubernetes.io",
			targetURL:     "a.kubernetes.io",
			matchExpected: false,
		},
		// match when number of parts match
		{
			globURL:       "*kubernetes.io",
			targetURL:     "kubernetes.io",
			matchExpected: true,
		},
		{
			globURL:       "*.*.*.kubernetes.io",
			targetURL:     "a.b.c.kubernetes.io",
			matchExpected: true,
		},
		// no match when some parts mismatch
		{
			globURL:       "kubernetes.io",
			targetURL:     "kubernetes.com",
			matchExpected: false,
		},
		{
			globURL:       "k*.io",
			targetURL:     "quay.io",
			matchExpected: false,
		},
		// no match when ports mismatch
		{
			globURL:       "*.kubernetes.io:1234/blah",
			targetURL:     "prefix.kubernetes.io:1111/blah",
			matchExpected: false,
		},
		{
			globURL:       "prefix.*.io/foo",
			targetURL:     "prefix.kubernetes.io:1111/foo/bar",
			matchExpected: false,
		},
	}
	for _, test := range tests {
		matched, _ := URLsMatchStr(test.globURL, test.targetURL)
		if matched != test.matchExpected {
			t.Errorf("Expected match result of %s and %s to be %t, but was %t",
				test.globURL, test.targetURL, test.matchExpected, matched)
		}
	}
}

func TestDockerKeyringForGlob(t *testing.T) {
	tests := []struct {
		globURL   string
		targetURL string
	}{
		{
			globURL:   "https://hello.kubernetes.io",
			targetURL: "hello.kubernetes.io",
		},
		{
			globURL:   "https://*.docker.io",
			targetURL: "prefix.docker.io",
		},
		{
			globURL:   "https://prefix.*.io",
			targetURL: "prefix.docker.io",
		},
		{
			globURL:   "https://prefix.docker.*",
			targetURL: "prefix.docker.io",
		},
		{
			globURL:   "https://*.docker.io/path",
			targetURL: "prefix.docker.io/path",
		},
		{
			globURL:   "https://prefix.*.io/path",
			targetURL: "prefix.docker.io/path/subpath",
		},
		{
			globURL:   "https://prefix.docker.*/path",
			targetURL: "prefix.docker.io/path",
		},
		{
			globURL:   "https://*.docker.io:8888",
			targetURL: "prefix.docker.io:8888",
		},
		{
			globURL:   "https://prefix.*.io:8888",
			targetURL: "prefix.docker.io:8888",
		},
		{
			globURL:   "https://prefix.docker.*:8888",
			targetURL: "prefix.docker.io:8888",
		},
		{
			globURL:   "https://*.docker.io/path:1111",
			targetURL: "prefix.docker.io/path:1111",
		},
		{
			globURL:   "https://*.docker.io/v1/",
			targetURL: "prefix.docker.io/path:1111",
		},
		{
			globURL:   "https://*.docker.io/v2/",
			targetURL: "prefix.docker.io/path:1111",
		},
		{
			globURL:   "https://prefix.docker.*/path:1111",
			targetURL: "prefix.docker.io/path:1111",
		},
		{
			globURL:   "prefix.docker.io:1111",
			targetURL: "prefix.docker.io:1111/path",
		},
		{
			globURL:   "*.docker.io:1111",
			targetURL: "prefix.docker.io:1111/path",
		},
	}
	for i, test := range tests {
		email := "foo@bar.baz"
		username := "foo"
		password := "bar" // Fake value for testing.
		auth := base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("%s:%s", username, password)))
		sampleDockerConfig := fmt.Sprintf(`{
   "%s": {
     "email": %q,
     "auth": %q
   }
}`, test.globURL, email, auth)

		keyring := &BasicDockerKeyring{}
		if cfg, err := ReadDockerConfigFileFromBytes([]byte(sampleDockerConfig)); err != nil {
			t.Errorf("Error processing json blob %q, %v", sampleDockerConfig, err)
		} else {
			keyring.Add(nil, cfg)
		}

		creds, ok := keyring.Lookup(test.targetURL + "/foo/bar")
		if !ok {
			t.Errorf("%d: Didn't find expected URL: %s", i, test.targetURL)
			continue
		}
		val := creds[0]

		if username != val.Username {
			t.Errorf("Unexpected username value, want: %s, got: %s", username, val.Username)
		}
		if password != val.Password {
			t.Errorf("Unexpected password value, want: %s, got: %s", password, val.Password)
		}
		if email != val.Email {
			t.Errorf("Unexpected email value, want: %s, got: %s", email, val.Email)
		}
	}
}

func TestKeyringMiss(t *testing.T) {
	tests := []struct {
		globURL   string
		lookupURL string
	}{
		{
			globURL:   "https://hello.kubernetes.io",
			lookupURL: "world.mesos.org/foo/bar",
		},
		{
			globURL:   "https://*.docker.com",
			lookupURL: "prefix.docker.io",
		},
		{
			globURL:   "https://suffix.*.io",
			lookupURL: "prefix.docker.io",
		},
		{
			globURL:   "https://prefix.docker.c*",
			lookupURL: "prefix.docker.io",
		},
		{
			globURL:   "https://prefix.*.io/path:1111",
			lookupURL: "prefix.docker.io/path/subpath:1111",
		},
		{
			globURL:   "suffix.*.io",
			lookupURL: "prefix.docker.io",
		},
	}
	for _, test := range tests {
		email := "foo@bar.baz"
		username := "foo"
		password := "bar" // Fake value for testing.
		auth := base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("%s:%s", username, password)))
		sampleDockerConfig := fmt.Sprintf(`{
   "%s": {
     "email": %q,
     "auth": %q
   }
}`, test.globURL, email, auth)

		keyring := &BasicDockerKeyring{}
		if cfg, err := ReadDockerConfigFileFromBytes([]byte(sampleDockerConfig)); err != nil {
			t.Errorf("Error processing json blob %q, %v", sampleDockerConfig, err)
		} else {
			keyring.Add(nil, cfg)
		}

		_, ok := keyring.Lookup(test.lookupURL + "/foo/bar")
		if ok {
			t.Errorf("Expected not to find URL %s, but found", test.lookupURL)
		}
	}

}

func TestKeyringMissWithDockerHubCredentials(t *testing.T) {
	url := defaultRegistryKey
	email := "foo@bar.baz"
	username := "foo"
	password := "bar" // Fake value for testing.
	auth := base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("%s:%s", username, password)))
	sampleDockerConfig := fmt.Sprintf(`{
   "https://%s": {
     "email": %q,
     "auth": %q
   }
}`, url, email, auth)

	keyring := &BasicDockerKeyring{}
	if cfg, err := ReadDockerConfigFileFromBytes([]byte(sampleDockerConfig)); err != nil {
		t.Errorf("Error processing json blob %q, %v", sampleDockerConfig, err)
	} else {
		keyring.Add(nil, cfg)
	}

	val, ok := keyring.Lookup("world.mesos.org/foo/bar")
	if ok {
		t.Errorf("Found unexpected credential: %+v", val)
	}
}

func TestKeyringHitWithUnqualifiedDockerHub(t *testing.T) {
	url := defaultRegistryKey
	email := "foo@bar.baz"
	username := "foo"
	password := "bar" // Fake value for testing.
	auth := base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("%s:%s", username, password)))
	sampleDockerConfig := fmt.Sprintf(`{
   "https://%s": {
     "email": %q,
     "auth": %q
   }
}`, url, email, auth)

	keyring := &BasicDockerKeyring{}
	if cfg, err := ReadDockerConfigFileFromBytes([]byte(sampleDockerConfig)); err != nil {
		t.Errorf("Error processing json blob %q, %v", sampleDockerConfig, err)
	} else {
		keyring.Add(nil, cfg)
	}

	creds, ok := keyring.Lookup("google/docker-registry")
	if !ok {
		t.Errorf("Didn't find expected URL: %s", url)
		return
	}
	if len(creds) > 1 {
		t.Errorf("Got more hits than expected: %v", creds)
	}
	val := creds[0]

	if username != val.Username {
		t.Errorf("Unexpected username value, want: %s, got: %s", username, val.Username)
	}
	if password != val.Password {
		t.Errorf("Unexpected password value, want: %s, got: %s", password, val.Password)
	}
	if email != val.Email {
		t.Errorf("Unexpected email value, want: %s, got: %s", email, val.Email)
	}
}

func TestKeyringHitWithUnqualifiedLibraryDockerHub(t *testing.T) {
	url := defaultRegistryKey
	email := "foo@bar.baz"
	username := "foo"
	password := "bar" // Fake value for testing.
	auth := base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("%s:%s", username, password)))
	sampleDockerConfig := fmt.Sprintf(`{
   "https://%s": {
     "email": %q,
     "auth": %q
   }
}`, url, email, auth)

	keyring := &BasicDockerKeyring{}
	if cfg, err := ReadDockerConfigFileFromBytes([]byte(sampleDockerConfig)); err != nil {
		t.Errorf("Error processing json blob %q, %v", sampleDockerConfig, err)
	} else {
		keyring.Add(nil, cfg)
	}

	creds, ok := keyring.Lookup("jenkins")
	if !ok {
		t.Errorf("Didn't find expected URL: %s", url)
		return
	}
	if len(creds) > 1 {
		t.Errorf("Got more hits than expected: %v", creds)
	}
	val := creds[0]

	if username != val.Username {
		t.Errorf("Unexpected username value, want: %s, got: %s", username, val.Username)
	}
	if password != val.Password {
		t.Errorf("Unexpected password value, want: %s, got: %s", password, val.Password)
	}
	if email != val.Email {
		t.Errorf("Unexpected email value, want: %s, got: %s", email, val.Email)
	}
}

func TestKeyringHitWithQualifiedDockerHub(t *testing.T) {
	url := defaultRegistryKey
	email := "foo@bar.baz"
	username := "foo"
	password := "bar" // Fake value for testing.
	auth := base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("%s:%s", username, password)))
	sampleDockerConfig := fmt.Sprintf(`{
   "https://%s": {
     "email": %q,
     "auth": %q
   }
}`, url, email, auth)

	keyring := &BasicDockerKeyring{}
	if cfg, err := ReadDockerConfigFileFromBytes([]byte(sampleDockerConfig)); err != nil {
		t.Errorf("Error processing json blob %q, %v", sampleDockerConfig, err)
	} else {
		keyring.Add(nil, cfg)
	}

	creds, ok := keyring.Lookup(url + "/google/docker-registry")
	if !ok {
		t.Errorf("Didn't find expected URL: %s", url)
		return
	}
	if len(creds) > 2 {
		t.Errorf("Got more hits than expected: %v", creds)
	}
	val := creds[0]

	if username != val.Username {
		t.Errorf("Unexpected username value, want: %s, got: %s", username, val.Username)
	}
	if password != val.Password {
		t.Errorf("Unexpected password value, want: %s, got: %s", password, val.Password)
	}
	if email != val.Email {
		t.Errorf("Unexpected email value, want: %s, got: %s", email, val.Email)
	}
}

func TestIsDefaultRegistryMatch(t *testing.T) {
	samples := []map[bool]string{
		{true: "foo/bar"},
		{true: "docker.io/foo/bar"},
		{true: "index.docker.io/foo/bar"},
		{true: "foo"},
		{false: ""},
		{false: "registry.tld/foo/bar"},
		{false: "registry:5000/foo/bar"},
		{false: "myhostdocker.io/foo/bar"},
	}
	for _, sample := range samples {
		for expected, imageName := range sample {
			if got := isDefaultRegistryMatch(imageName); got != expected {
				t.Errorf("Expected '%s' to be %t, got %t", imageName, expected, got)
			}
		}
	}
}

func TestProvidersDockerKeyring(t *testing.T) {
	provider := &testProvider{
		Count: 0,
	}
	keyring := &providersDockerKeyring{
		Providers: []DockerConfigProvider{
			provider,
		},
	}

	if provider.Count != 0 {
		t.Errorf("Unexpected number of Provide calls: %v", provider.Count)
	}
	keyring.Lookup("foo")
	if provider.Count != 1 {
		t.Errorf("Unexpected number of Provide calls: %v", provider.Count)
	}
	keyring.Lookup("foo")
	if provider.Count != 2 {
		t.Errorf("Unexpected number of Provide calls: %v", provider.Count)
	}
	keyring.Lookup("foo")
	if provider.Count != 3 {
		t.Errorf("Unexpected number of Provide calls: %v", provider.Count)
	}
}

func TestDockerKeyringLookup(t *testing.T) {
	// turn on the ensure secret pulled images feature to get the hashes with the creds
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.KubeletEnsureSecretPulledImages, true)
	ada := AuthConfig{
		Username: "ada",
		Password: "smash", // Fake value for testing.
		Email:    "ada@example.com",
	}
	adaHash := "353258b53f5e9a57b059eab3f05312fc35bbeb874f08ce101e7bf0bf46977423"

	grace := AuthConfig{
		Username: "grace",
		Password: "squash", // Fake value for testing.
		Email:    "grace@example.com",
	}
	graceHash := "f949b3837a1eb733a951b6aeda0b3327c09ec50c917de9ca35818e8fbf567e29"

	dk := &BasicDockerKeyring{}
	dk.Add(nil, DockerConfig{
		"bar.example.com/pong": DockerConfigEntry{
			Username: grace.Username,
			Password: grace.Password,
			Email:    grace.Email,
		},
		"bar.example.com": DockerConfigEntry{
			Username: ada.Username,
			Password: ada.Password,
			Email:    ada.Email,
		},
	})

	tests := []struct {
		image string
		match []TrackedAuthConfig
		ok    bool
	}{
		// direct match
		{"bar.example.com", []TrackedAuthConfig{{AuthConfig: ada, AuthConfigHash: adaHash}}, true},

		// direct match deeper than other possible matches
		{"bar.example.com/pong", []TrackedAuthConfig{{AuthConfig: grace, AuthConfigHash: graceHash}, {AuthConfig: ada, AuthConfigHash: adaHash}}, true},

		// no direct match, deeper path ignored
		{"bar.example.com/ping", []TrackedAuthConfig{{AuthConfig: ada, AuthConfigHash: adaHash}}, true},

		// match first part of path token
		{"bar.example.com/pongz", []TrackedAuthConfig{{AuthConfig: grace, AuthConfigHash: graceHash}, {AuthConfig: ada, AuthConfigHash: adaHash}}, true},

		// match regardless of sub-path
		{"bar.example.com/pong/pang", []TrackedAuthConfig{{AuthConfig: grace, AuthConfigHash: graceHash}, {AuthConfig: ada, AuthConfigHash: adaHash}}, true},

		// no host match
		{"example.com", []TrackedAuthConfig{}, false},
		{"foo.example.com", []TrackedAuthConfig{}, false},
	}

	for i, tt := range tests {
		match, ok := dk.Lookup(tt.image)
		if tt.ok != ok {
			t.Errorf("case %d: expected ok=%t, got %t", i, tt.ok, ok)
		}

		if !reflect.DeepEqual(tt.match, match) {
			t.Errorf("case %d: expected match=%#v, got %#v", i, tt.match, match)
		}
	}
}

// This validates that dockercfg entries with a scheme and url path are properly matched
// by images that only match the hostname.
// NOTE: the above covers the case of a more specific match trumping just hostname.
func TestIssue3797(t *testing.T) {
	// turn on the ensure secret pulled images feature to get the hashes with the creds
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.KubeletEnsureSecretPulledImages, true)
	rex := AuthConfig{
		Username: "rex",
		Password: "tiny arms", // Fake value for testing.
		Email:    "rex@example.com",
	}
	rexHash := "899748fec74c8dd761845fca727f4249b05be275ff24026676fcd4351f656363"

	dk := &BasicDockerKeyring{}
	dk.Add(nil, DockerConfig{
		"https://quay.io/v1/": DockerConfigEntry{
			Username: rex.Username,
			Password: rex.Password,
			Email:    rex.Email,
		},
	})

	tests := []struct {
		image string
		match []TrackedAuthConfig
		ok    bool
	}{
		// direct match
		{"quay.io", []TrackedAuthConfig{{AuthConfig: rex, AuthConfigHash: rexHash}}, true},

		// partial matches
		{"quay.io/foo", []TrackedAuthConfig{{AuthConfig: rex, AuthConfigHash: rexHash}}, true},
		{"quay.io/foo/bar", []TrackedAuthConfig{{AuthConfig: rex, AuthConfigHash: rexHash}}, true},
	}

	for i, tt := range tests {
		match, ok := dk.Lookup(tt.image)
		if tt.ok != ok {
			t.Errorf("case %d: expected ok=%t, got %t", i, tt.ok, ok)
		}

		if !reflect.DeepEqual(tt.match, match) {
			t.Errorf("case %d: expected match=%#v, got %#v", i, tt.match, match)
		}
	}
}
