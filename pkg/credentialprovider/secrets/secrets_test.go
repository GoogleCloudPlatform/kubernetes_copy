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

package secrets

import (
	"reflect"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	"k8s.io/kubernetes/pkg/credentialprovider"
	"k8s.io/kubernetes/pkg/features"
)

// FakeKeyring a fake config credentials
type FakeKeyring struct {
	auth []credentialprovider.TrackedAuthConfig
	ok   bool
}

// Lookup implements the DockerKeyring method for fetching credentials based on image name
// return fake auth and ok
func (f *FakeKeyring) Lookup(image string) ([]credentialprovider.TrackedAuthConfig, bool) {
	return f.auth, f.ok
}

func Test_MakeDockerKeyring(t *testing.T) {
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.KubeletEnsureSecretPulledImages, true)

	testcases := []struct {
		name                string
		image               string
		defaultKeyring      credentialprovider.DockerKeyring
		pullSecrets         []v1.Secret
		expectedAuthConfigs []credentialprovider.TrackedAuthConfig
		found               bool
	}{
		{
			name:  "with .dockerconfigjson and auth field",
			image: "test.registry.io",
			pullSecrets: []v1.Secret{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "s1", Namespace: "ns1", UID: "uid1"},
					Type: v1.SecretTypeDockerConfigJson,
					Data: map[string][]byte{
						v1.DockerConfigJsonKey: []byte(`{"auths": {"test.registry.io": {"auth": "dXNlcjpwYXNzd29yZA=="}}}`),
					},
				},
			},
			expectedAuthConfigs: []credentialprovider.TrackedAuthConfig{
				{
					Source: &credentialprovider.CredentialSource{
						Secret: credentialprovider.SecretCoordinates{
							Name: "s1", Namespace: "ns1", UID: "uid1"},
					},
					AuthConfig: credentialprovider.AuthConfig{
						Username: "user",
						Password: "password",
					},
					AuthConfigHash: "a55436fc140d516560d072c5fe8700385ce9f41629abf65c1edcbcb39fac691d",
				},
			},
			found: true,
		},
		{
			name:  "with .dockerconfig and auth field",
			image: "test.registry.io",
			pullSecrets: []v1.Secret{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "s1", Namespace: "ns1", UID: "uid1"},
					Type: v1.SecretTypeDockercfg,
					Data: map[string][]byte{
						v1.DockerConfigKey: []byte(`{"test.registry.io": {"auth": "dXNlcjpwYXNzd29yZA=="}}`),
					},
				},
			},
			expectedAuthConfigs: []credentialprovider.TrackedAuthConfig{
				{
					Source: &credentialprovider.CredentialSource{
						Secret: credentialprovider.SecretCoordinates{
							Name: "s1", Namespace: "ns1", UID: "uid1"},
					},
					AuthConfig: credentialprovider.AuthConfig{
						Username: "user",
						Password: "password",
					},
					AuthConfigHash: "a55436fc140d516560d072c5fe8700385ce9f41629abf65c1edcbcb39fac691d",
				},
			},
			found: true,
		},
		{
			name:  "with .dockerconfigjson and username/password fields",
			image: "test.registry.io",
			pullSecrets: []v1.Secret{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "s1", Namespace: "ns1", UID: "uid1"},
					Type: v1.SecretTypeDockerConfigJson,
					Data: map[string][]byte{
						v1.DockerConfigJsonKey: []byte(`{"auths": {"test.registry.io": {"username": "user", "password": "password"}}}`),
					},
				},
			},
			expectedAuthConfigs: []credentialprovider.TrackedAuthConfig{
				{
					Source: &credentialprovider.CredentialSource{
						Secret: credentialprovider.SecretCoordinates{
							Name: "s1", Namespace: "ns1", UID: "uid1"},
					},
					AuthConfig: credentialprovider.AuthConfig{
						Username: "user",
						Password: "password",
					},
					AuthConfigHash: "a55436fc140d516560d072c5fe8700385ce9f41629abf65c1edcbcb39fac691d",
				},
			},
			found: true,
		},
		{
			name:  "with .dockerconfig and username/password fields",
			image: "test.registry.io",
			pullSecrets: []v1.Secret{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "s1", Namespace: "ns1", UID: "uid1"},
					Type: v1.SecretTypeDockercfg,
					Data: map[string][]byte{
						v1.DockerConfigKey: []byte(`{"test.registry.io": {"username": "user", "password": "password"}}`),
					},
				},
			},
			expectedAuthConfigs: []credentialprovider.TrackedAuthConfig{
				{
					Source: &credentialprovider.CredentialSource{
						Secret: credentialprovider.SecretCoordinates{
							Name: "s1", Namespace: "ns1", UID: "uid1"},
					},
					AuthConfig: credentialprovider.AuthConfig{
						Username: "user",
						Password: "password",
					},
					AuthConfigHash: "a55436fc140d516560d072c5fe8700385ce9f41629abf65c1edcbcb39fac691d",
				},
			},
			found: true,
		},
		{
			name:           "with .dockerconfigjson but with wrong Secret Type",
			image:          "test.registry.io",
			defaultKeyring: &FakeKeyring{},
			pullSecrets: []v1.Secret{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "s1", Namespace: "ns1", UID: "uid1"},
					Type: v1.SecretTypeDockercfg,
					Data: map[string][]byte{
						v1.DockerConfigJsonKey: []byte(`{"auths": {"test.registry.io": {"auth": "dXNlcjpwYXNzd29yZA=="}}}`),
					},
				},
			},
			found: false,
		},
		{
			name:           "with .dockerconfig but with wrong Secret Type",
			image:          "test.registry.io",
			defaultKeyring: &FakeKeyring{},
			pullSecrets: []v1.Secret{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "s1", Namespace: "ns1", UID: "uid1"},
					Type: v1.SecretTypeDockerConfigJson,
					Data: map[string][]byte{
						v1.DockerConfigKey: []byte(`{"test.registry.io": {"auth": "dXNlcjpwYXNzd29yZA=="}}`),
					},
				},
			},
			found: false,
		},
		{
			name:  "with not matcing .dockerconfigjson and default keyring",
			image: "test.registry.io",
			defaultKeyring: &FakeKeyring{
				auth: []credentialprovider.TrackedAuthConfig{
					{
						AuthConfig: credentialprovider.AuthConfig{
							Username: "default-user",
							Password: "default-password",
						},
					},
				},
			},
			pullSecrets: []v1.Secret{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "s1", Namespace: "ns1", UID: "uid1"},
					Type: v1.SecretTypeDockerConfigJson,
					Data: map[string][]byte{
						v1.DockerConfigJsonKey: []byte(`{"auths": {"foobar.io": {"auth": "dXNlcjpwYXNzd29yZA=="}}}`),
					},
				},
			},
			expectedAuthConfigs: []credentialprovider.TrackedAuthConfig{
				{
					AuthConfig: credentialprovider.AuthConfig{
						Username: "default-user",
						Password: "default-password",
					},
					AuthConfigHash: "",
				},
			},
			found: true,
		},
		{
			name:  "with not matching .dockerconfig and default keyring",
			image: "test.registry.io",
			defaultKeyring: &FakeKeyring{
				auth: []credentialprovider.TrackedAuthConfig{
					{
						AuthConfig: credentialprovider.AuthConfig{
							Username: "default-user",
							Password: "default-password",
						},
					},
				},
			},
			pullSecrets: []v1.Secret{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "s1", Namespace: "ns1", UID: "uid1"},
					Type: v1.SecretTypeDockercfg,
					Data: map[string][]byte{
						v1.DockerConfigKey: []byte(`{"foobar.io": {"auth": "dXNlcjpwYXNzd29yZA=="}}`),
					},
				},
			},
			expectedAuthConfigs: []credentialprovider.TrackedAuthConfig{
				{
					AuthConfig: credentialprovider.AuthConfig{
						Username: "default-user",
						Password: "default-password",
					},
					AuthConfigHash: "",
				},
			},
			found: true,
		},
		{
			name:  "with no pull secrets but has default keyring",
			image: "test.registry.io",
			defaultKeyring: &FakeKeyring{
				auth: []credentialprovider.TrackedAuthConfig{
					{
						AuthConfig: credentialprovider.AuthConfig{
							Username: "default-user",
							Password: "default-password",
						},
					},
				},
			},
			pullSecrets: []v1.Secret{},
			expectedAuthConfigs: []credentialprovider.TrackedAuthConfig{
				{
					AuthConfig: credentialprovider.AuthConfig{
						Username: "default-user",
						Password: "default-password",
					},
					AuthConfigHash: "",
				},
			},
			found: false,
		},
		{
			name:  "with pull secrets and has default keyring",
			image: "test.registry.io",
			defaultKeyring: &FakeKeyring{
				auth: []credentialprovider.TrackedAuthConfig{
					{
						AuthConfig: credentialprovider.AuthConfig{
							Username: "default-user",
							Password: "default-password",
						},
					},
				},
			},
			pullSecrets: []v1.Secret{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "s1", Namespace: "ns1", UID: "uid1"},
					Type: v1.SecretTypeDockerConfigJson,
					Data: map[string][]byte{
						v1.DockerConfigJsonKey: []byte(`{"auths": {"test.registry.io": {"auth": "dXNlcjpwYXNzd29yZA=="}}}`),
					},
				},
			},
			expectedAuthConfigs: []credentialprovider.TrackedAuthConfig{
				{
					Source: &credentialprovider.CredentialSource{
						Secret: credentialprovider.SecretCoordinates{
							Name: "s1", Namespace: "ns1", UID: "uid1"},
					},
					AuthConfig: credentialprovider.AuthConfig{
						Username: "user",
						Password: "password",
					},
					AuthConfigHash: "a55436fc140d516560d072c5fe8700385ce9f41629abf65c1edcbcb39fac691d",
				},
				{
					AuthConfig: credentialprovider.AuthConfig{
						Username: "default-user",
						Password: "default-password",
					},
					AuthConfigHash: "",
				},
			},
			found: true,
		},
	}

	for _, testcase := range testcases {
		t.Run(testcase.name, func(t *testing.T) {
			keyring, err := MakeDockerKeyring(testcase.pullSecrets, testcase.defaultKeyring)
			if err != nil {
				t.Fatalf("error creating secret-based docker keyring: %v", err)
			}

			authConfigs, found := keyring.Lookup(testcase.image)
			if found != testcase.found {
				t.Logf("actual lookup status: %v", found)
				t.Logf("expected lookup status: %v", testcase.found)
				t.Errorf("unexpected lookup for image: %s", testcase.image)
			}

			if !reflect.DeepEqual(authConfigs, testcase.expectedAuthConfigs) { // TODO: we may need better comparison as the result is unordered
				t.Logf("actual auth configs: %#v", authConfigs)
				t.Logf("expected auth configs: %#v", testcase.expectedAuthConfigs)
				t.Error("auth configs did not match")
			}
		})
	}
}
