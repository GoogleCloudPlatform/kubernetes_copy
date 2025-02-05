/*
Copyright 2021 The Kubernetes Authors.

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

package create

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	cmdtesting "k8s.io/kubectl/pkg/cmd/testing"
	"k8s.io/utils/ptr"
)

func TestCreateSecretDockerRegistry(t *testing.T) {
	username, password, email, server := "test-user", "test-password", "test-user@example.org", "https://index.docker.io/v1/"
	secretData, err := handleDockerCfgJSONContent(username, password, email, server)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	secretDataNoEmail, err := handleDockerCfgJSONContent(username, password, "", server)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	tests := map[string]struct {
		dockerRegistrySecretName string
		dockerUsername           string
		dockerEmail              string
		dockerPassword           string
		dockerServer             string
		appendHash               bool
		immutable                bool
		expected                 *corev1.Secret
		expectErr                bool
	}{
		"create_secret_docker_registry_with_email": {
			dockerRegistrySecretName: "foo",
			dockerUsername:           username,
			dockerPassword:           password,
			dockerEmail:              email,
			dockerServer:             server,
			expected: &corev1.Secret{
				TypeMeta: metav1.TypeMeta{
					APIVersion: corev1.SchemeGroupVersion.String(),
					Kind:       "Secret",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: "foo",
				},
				Type: corev1.SecretTypeDockerConfigJson,
				Data: map[string][]byte{
					corev1.DockerConfigJsonKey: secretData,
				},
			},
			expectErr: false,
		},
		"create_secret_docker_registry_with_email_hash": {
			dockerRegistrySecretName: "foo",
			dockerUsername:           username,
			dockerPassword:           password,
			dockerEmail:              email,
			dockerServer:             server,
			appendHash:               true,
			expected: &corev1.Secret{
				TypeMeta: metav1.TypeMeta{
					APIVersion: corev1.SchemeGroupVersion.String(),
					Kind:       "Secret",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: "foo-548cm7fgdh",
				},
				Type: corev1.SecretTypeDockerConfigJson,
				Data: map[string][]byte{
					corev1.DockerConfigJsonKey: secretData,
				},
			},
			expectErr: false,
		},
		"create_secret_docker_registry_without_email": {
			dockerRegistrySecretName: "foo",
			dockerUsername:           username,
			dockerPassword:           password,
			dockerEmail:              "",
			dockerServer:             server,
			expected: &corev1.Secret{
				TypeMeta: metav1.TypeMeta{
					APIVersion: corev1.SchemeGroupVersion.String(),
					Kind:       "Secret",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: "foo",
				},
				Type: corev1.SecretTypeDockerConfigJson,
				Data: map[string][]byte{
					corev1.DockerConfigJsonKey: secretDataNoEmail,
				},
			},
			expectErr: false,
		},
		"create_secret_docker_registry_without_email_hash": {
			dockerRegistrySecretName: "foo",
			dockerUsername:           username,
			dockerPassword:           password,
			dockerEmail:              "",
			dockerServer:             server,
			appendHash:               true,
			immutable:                true,
			expected: &corev1.Secret{
				TypeMeta: metav1.TypeMeta{
					APIVersion: corev1.SchemeGroupVersion.String(),
					Kind:       "Secret",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: "foo-bff5bt4f92",
				},
				Type: corev1.SecretTypeDockerConfigJson,
				Data: map[string][]byte{
					corev1.DockerConfigJsonKey: secretDataNoEmail,
				},
				Immutable: ptr.To[bool](true),
			},
			expectErr: false,
		},
		"create_invalid_secret_docker_registry_without_username": {
			dockerRegistrySecretName: "foo",
			dockerPassword:           password,
			dockerEmail:              "",
			dockerServer:             server,
			expectErr:                true,
		},
		"create_invalid_secret_docker_registry_without_password": {
			dockerRegistrySecretName: "foo",
			dockerUsername:           username,
			dockerEmail:              "",
			dockerServer:             server,
			expectErr:                true,
		},
		"create_invalid_secret_docker_registry_without_server": {
			dockerRegistrySecretName: "foo",
			dockerUsername:           username,
			dockerPassword:           password,
			dockerEmail:              "",
			expectErr:                true,
		},
	}

	// Run all the tests
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			var secretDockerRegistry *corev1.Secret = nil
			secretDockerRegistryOptions := CreateSecretDockerRegistryOptions{
				Name:       test.dockerRegistrySecretName,
				Username:   test.dockerUsername,
				Email:      test.dockerEmail,
				Password:   test.dockerPassword,
				Server:     test.dockerServer,
				AppendHash: test.appendHash,
				Immutable:  test.immutable,
			}
			err := secretDockerRegistryOptions.Validate()
			if err == nil {
				secretDockerRegistry, err = secretDockerRegistryOptions.createSecretDockerRegistry()
			}

			if !test.expectErr && err != nil {
				t.Errorf("test %s, unexpected error: %v", name, err)
			}
			if test.expectErr && err == nil {
				t.Errorf("test %s was expecting an error but no error occurred", name)
			}
			if !apiequality.Semantic.DeepEqual(secretDockerRegistry, test.expected) {
				t.Errorf("test %s\n expected:\n%#v\ngot:\n%#v", name, test.expected, secretDockerRegistry)
			}
		})
	}
}

func TestCreateSecretDockerRegistryFromFile(t *testing.T) {
	username, password, email, server := "test-user", "test-password", "test-user@example.org", "https://index.docker.io/v1/"
	secretData, err := handleDockerCfgJSONContent(username, password, email, server)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	secret := &corev1.Secret{
		TypeMeta: metav1.TypeMeta{
			APIVersion: corev1.SchemeGroupVersion.String(),
			Kind:       "Secret",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: "foo",
		},
		Type: corev1.SecretTypeDockerConfigJson,
		Data: map[string][]byte{
			corev1.DockerConfigJsonKey: secretData,
		},
	}

	tests := map[string]struct {
		withKey  bool
		expected *corev1.Secret
	}{
		"create_secret_docker_registry_from_file_with_keyname": {
			withKey:  true,
			expected: secret,
		},
		"create_secret_docker_registry_from_file_without_keyname": {
			withKey:  false,
			expected: secret,
		},
	}

	// Run all the tests
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			tmp, _ := os.MkdirTemp("", "input")
			defer func() {
				err := os.RemoveAll(tmp)
				if err != nil {
					t.Fatalf("Failed to teardown: %s", err)
				}
			}()
			dockerCfgFile := tmp + "/dockerconfig.json"
			err := os.WriteFile(dockerCfgFile, secretData, 0644)
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}

			tf := cmdtesting.NewTestFactory()
			defer tf.Cleanup()
			ioStreams, _, out, _ := genericiooptions.NewTestIOStreams()
			cmd := NewCmdCreateSecretDockerRegistry(tf, ioStreams)
			args := []string{"foo", "--dry-run=client", "-ojson"}
			if test.withKey {
				args = append(args, fmt.Sprintf("--from-file=%s=%s", corev1.DockerConfigJsonKey, dockerCfgFile))
			} else {
				args = append(args, fmt.Sprintf("--from-file=%s", dockerCfgFile))
			}
			cmd.SetArgs(args)
			err = cmd.Execute()
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			got := &corev1.Secret{}
			err = json.Unmarshal(out.Bytes(), got)
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if !apiequality.Semantic.DeepEqual(got, test.expected) {
				t.Errorf("test %s\n expected:\n%#v\ngot:\n%#v", name, test.expected, got)
			}
		})
	}
}
