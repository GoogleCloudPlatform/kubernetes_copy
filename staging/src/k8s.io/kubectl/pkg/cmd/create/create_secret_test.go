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

package create

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestCreateSecretObject(t *testing.T) {
	secretObject := newSecretObj("foo", "foo-namespace", corev1.SecretTypeDockerConfigJson)
	expectedSecretObject := &corev1.Secret{
		TypeMeta: metav1.TypeMeta{
			APIVersion: corev1.SchemeGroupVersion.String(),
			Kind:       "Secret",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "foo",
			Namespace: "foo-namespace",
		},
		Type: corev1.SecretTypeDockerConfigJson,
		Data: map[string][]byte{},
	}
	t.Run("Creating a Secret Object", func(t *testing.T) {
		if !apiequality.Semantic.DeepEqual(secretObject, expectedSecretObject) {
			t.Errorf("expected:\n%#v\ngot:\n%#v", secretObject, expectedSecretObject)
		}
	})
}

func TestCreateSecretGeneric(t *testing.T) {
	tests := map[string]struct {
		secretName        string
		secretType        string
		fromLiteral       []string
		fromBase64Literal []string
		fromFile          []string
		fromEnvFile       []string
		appendHash        bool
		setup             func(t *testing.T, secretGenericOptions *CreateSecretOptions) func()

		expected  *corev1.Secret
		expectErr string
	}{
		"create_secret_foo": {
			secretName: "foo",
			expected: &corev1.Secret{
				TypeMeta: metav1.TypeMeta{
					APIVersion: corev1.SchemeGroupVersion.String(),
					Kind:       "Secret",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: "foo",
				},
				Data: map[string][]byte{},
			},
		},
		"create_secret_foo_hash": {
			secretName: "foo",
			appendHash: true,
			expected: &corev1.Secret{
				TypeMeta: metav1.TypeMeta{
					APIVersion: corev1.SchemeGroupVersion.String(),
					Kind:       "Secret",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: "foo-949tdgdkgg",
				},
				Data: map[string][]byte{},
			},
		},
		"create_secret_foo_type": {
			secretName: "foo",
			secretType: "my-type",
			expected: &corev1.Secret{
				TypeMeta: metav1.TypeMeta{
					APIVersion: corev1.SchemeGroupVersion.String(),
					Kind:       "Secret",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: "foo",
				},
				Data: map[string][]byte{},
				Type: "my-type",
			},
		},
		"create_secret_foo_type_hash": {
			secretName: "foo",
			secretType: "my-type",
			appendHash: true,
			expected: &corev1.Secret{
				TypeMeta: metav1.TypeMeta{
					APIVersion: corev1.SchemeGroupVersion.String(),
					Kind:       "Secret",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: "foo-dg474f9t76",
				},
				Data: map[string][]byte{},
				Type: "my-type",
			},
		},
		"create_secret_foo_two_literal": {
			secretName:  "foo",
			fromLiteral: []string{"key1=value1", "key2=value2"},
			expected: &corev1.Secret{
				TypeMeta: metav1.TypeMeta{
					APIVersion: corev1.SchemeGroupVersion.String(),
					Kind:       "Secret",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: "foo",
				},
				Data: map[string][]byte{
					"key1": []byte("value1"),
					"key2": []byte("value2"),
				},
			},
		},
		"create_secret_foo_two_literal_hash": {
			secretName:  "foo",
			fromLiteral: []string{"key1=value1", "key2=value2"},
			appendHash:  true,
			expected: &corev1.Secret{
				TypeMeta: metav1.TypeMeta{
					APIVersion: corev1.SchemeGroupVersion.String(),
					Kind:       "Secret",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: "foo-tf72c228m4",
				},
				Data: map[string][]byte{
					"key1": []byte("value1"),
					"key2": []byte("value2"),
				},
			},
		},
		"create_secret_foo_two_base64_literal": {
			secretName:        "foo",
			fromBase64Literal: []string{"key1=dmFsdWUx", "key2=dmFsdWUy"},
			expected: &corev1.Secret{
				TypeMeta: metav1.TypeMeta{
					APIVersion: corev1.SchemeGroupVersion.String(),
					Kind:       "Secret",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: "foo",
				},
				Data: map[string][]byte{
					"key1": []byte("value1"),
					"key2": []byte("value2"),
				},
			},
		},
		"create_secret_foo_two_base64_literal_hash": {
			secretName:        "foo",
			fromBase64Literal: []string{"key1=dmFsdWUx", "key2=dmFsdWUy"},
			appendHash:        true,
			expected: &corev1.Secret{
				TypeMeta: metav1.TypeMeta{
					APIVersion: corev1.SchemeGroupVersion.String(),
					Kind:       "Secret",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: "foo-tf72c228m4",
				},
				Data: map[string][]byte{
					"key1": []byte("value1"),
					"key2": []byte("value2"),
				},
			},
		},
		"create_secret_foo_key1_=value1": {
			secretName:  "foo",
			fromLiteral: []string{"key1==value1"},
			expected: &corev1.Secret{
				TypeMeta: metav1.TypeMeta{
					APIVersion: corev1.SchemeGroupVersion.String(),
					Kind:       "Secret",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: "foo",
				},
				Data: map[string][]byte{
					"key1": []byte("=value1"),
				},
			},
		},
		"create_secret_foo_base64_literal_key1_value1": {
			secretName:        "foo",
			fromBase64Literal: []string{"key1=dmFsdWUx"},
			expected: &corev1.Secret{
				TypeMeta: metav1.TypeMeta{
					APIVersion: corev1.SchemeGroupVersion.String(),
					Kind:       "Secret",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: "foo",
				},
				Data: map[string][]byte{
					"key1": []byte("value1"),
				},
			},
		},
		"create_secret_foo_key1_=value1_hash": {
			secretName:  "foo",
			fromLiteral: []string{"key1==value1"},
			appendHash:  true,
			expected: &corev1.Secret{
				TypeMeta: metav1.TypeMeta{
					APIVersion: corev1.SchemeGroupVersion.String(),
					Kind:       "Secret",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: "foo-fdcc8tkhh5",
				},
				Data: map[string][]byte{
					"key1": []byte("=value1"),
				},
			},
		},
		"create_secret_foo_from_file_foo1_foo2_secret": {
			secretName: "foo",
			setup:      setupSecretBinaryFile([]byte{0x68, 0x65, 0x6c, 0x6c, 0x6f, 0x20, 0x77, 0x6f, 0x72, 0x6c, 0x64}),
			fromFile:   []string{"foo1", "foo2"},
			expected: &corev1.Secret{
				TypeMeta: metav1.TypeMeta{
					APIVersion: corev1.SchemeGroupVersion.String(),
					Kind:       "Secret",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: "foo",
				},
				Data: map[string][]byte{
					"foo1": []byte("hello world"),
					"foo2": []byte("hello world"),
				},
			},
		},
		"create_secret_foo_from_file_foo1_foo2_hash": {
			secretName: "foo",
			setup:      setupSecretBinaryFile([]byte{0x68, 0x65, 0x6c, 0x6c, 0x6f, 0x20, 0x77, 0x6f, 0x72, 0x6c, 0x64}),
			fromFile:   []string{"foo1", "foo2"},
			appendHash: true,
			expected: &corev1.Secret{
				TypeMeta: metav1.TypeMeta{
					APIVersion: corev1.SchemeGroupVersion.String(),
					Kind:       "Secret",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: "foo-hbkh2cdb57",
				},
				Data: map[string][]byte{
					"foo1": []byte("hello world"),
					"foo2": []byte("hello world"),
				},
			},
		},
		"create_secret_foo_from_file_foo1_foo2_and": {
			secretName: "foo",
			setup:      setupSecretBinaryFile([]byte{0xff, 0xfd}),
			fromFile:   []string{"foo1", "foo2"},
			expected: &corev1.Secret{
				TypeMeta: metav1.TypeMeta{
					APIVersion: corev1.SchemeGroupVersion.String(),
					Kind:       "Secret",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: "foo",
				},
				Data: map[string][]byte{
					"foo1": {0xff, 0xfd},
					"foo2": {0xff, 0xfd},
				},
			},
		},
		"create_secret_foo_from_file_foo1_foo2_and_hash": {
			secretName: "foo",
			setup:      setupSecretBinaryFile([]byte{0xff, 0xfd}),
			fromFile:   []string{"foo1", "foo2"},
			appendHash: true,
			expected: &corev1.Secret{
				TypeMeta: metav1.TypeMeta{
					APIVersion: corev1.SchemeGroupVersion.String(),
					Kind:       "Secret",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: "foo-mkhg4ktk4d",
				},
				Data: map[string][]byte{
					"foo1": {0xff, 0xfd},
					"foo2": {0xff, 0xfd},
				},
			},
		},
		"create_secret_valid_env_from_env_file": {
			secretName:  "valid_env",
			setup:       setupSecretEnvFile([][]string{{"key1=value1", "#", "", "key2=value2"}}),
			fromEnvFile: []string{"file.env"},
			expected: &corev1.Secret{
				TypeMeta: metav1.TypeMeta{
					APIVersion: corev1.SchemeGroupVersion.String(),
					Kind:       "Secret",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: "valid_env",
				},
				Data: map[string][]byte{
					"key1": []byte("value1"),
					"key2": []byte("value2"),
				},
			},
		},
		"create_secret_valid_env_from_env_file_hash": {
			secretName:  "valid_env",
			setup:       setupSecretEnvFile([][]string{{"key1=value1", "#", "", "key2=value2"}}),
			fromEnvFile: []string{"file.env"},
			appendHash:  true,
			expected: &corev1.Secret{
				TypeMeta: metav1.TypeMeta{
					APIVersion: corev1.SchemeGroupVersion.String(),
					Kind:       "Secret",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: "valid_env-bkb2m2965h",
				},
				Data: map[string][]byte{
					"key1": []byte("value1"),
					"key2": []byte("value2"),
				},
			},
		},
		"create_two_secret_valid_env_from_env_file": {
			secretName:  "two_valid_env",
			setup:       setupSecretEnvFile([][]string{{"key1=value1", "#", "", "key2=value2"}, {"key3=value3"}}),
			fromEnvFile: []string{"file1.env", "file2.env"},
			expected: &corev1.Secret{
				TypeMeta: metav1.TypeMeta{
					APIVersion: corev1.SchemeGroupVersion.String(),
					Kind:       "Secret",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: "two_valid_env",
				},
				Data: map[string][]byte{
					"key1": []byte("value1"),
					"key2": []byte("value2"),
					"key3": []byte("value3"),
				},
			},
		},
		"create_two_secret_valid_env_from_env_file_hash": {
			secretName:  "two_valid_env",
			setup:       setupSecretEnvFile([][]string{{"key1=value1", "#", "", "key2=value2"}, {"key3=value3"}}),
			fromEnvFile: []string{"file1.env", "file2.env"},
			appendHash:  true,
			expected: &corev1.Secret{
				TypeMeta: metav1.TypeMeta{
					APIVersion: corev1.SchemeGroupVersion.String(),
					Kind:       "Secret",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: "two_valid_env-gd56gct5cf",
				},
				Data: map[string][]byte{
					"key1": []byte("value1"),
					"key2": []byte("value2"),
					"key3": []byte("value3"),
				},
			},
		},
		"create_secret_get_env_from_env_file": {
			secretName: "get_env",
			setup: func() func(t *testing.T, secretGenericOptions *CreateSecretOptions) func() {
				t.Setenv("g_key1", "1")
				t.Setenv("g_key2", "2")
				return setupSecretEnvFile([][]string{{"g_key1", "g_key2="}})
			}(),
			fromEnvFile: []string{"file.env"},
			expected: &corev1.Secret{
				TypeMeta: metav1.TypeMeta{
					APIVersion: corev1.SchemeGroupVersion.String(),
					Kind:       "Secret",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: "get_env",
				},
				Data: map[string][]byte{
					"g_key1": []byte("1"),
					"g_key2": []byte(""),
				},
			},
		},
		"create_secret_get_env_from_env_file_hash": {
			secretName: "get_env",
			setup: func() func(t *testing.T, secretGenericOptions *CreateSecretOptions) func() {
				t.Setenv("g_key1", "1")
				t.Setenv("g_key2", "2")
				return setupSecretEnvFile([][]string{{"g_key1", "g_key2="}})
			}(),
			fromEnvFile: []string{"file.env"},
			appendHash:  true,
			expected: &corev1.Secret{
				TypeMeta: metav1.TypeMeta{
					APIVersion: corev1.SchemeGroupVersion.String(),
					Kind:       "Secret",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: "get_env-68mt8f2kkt",
				},
				Data: map[string][]byte{
					"g_key1": []byte("1"),
					"g_key2": []byte(""),
				},
			},
		},
		"create_secret_value_with_space_from_env_file": {
			secretName:  "value_with_space",
			setup:       setupSecretEnvFile([][]string{{"   key1=  value1"}}),
			fromEnvFile: []string{"file.env"},
			expected: &corev1.Secret{
				TypeMeta: metav1.TypeMeta{
					APIVersion: corev1.SchemeGroupVersion.String(),
					Kind:       "Secret",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: "value_with_space",
				},
				Data: map[string][]byte{
					"key1": []byte("  value1"),
				},
			},
		},
		"create_secret_value_with_space_from_env_file_hash": {
			secretName:  "valid_with_space",
			setup:       setupSecretEnvFile([][]string{{"   key1=  value1"}}),
			fromEnvFile: []string{"file.env"},
			appendHash:  true,
			expected: &corev1.Secret{
				TypeMeta: metav1.TypeMeta{
					APIVersion: corev1.SchemeGroupVersion.String(),
					Kind:       "Secret",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: "valid_with_space-bhkb4gfck6",
				},
				Data: map[string][]byte{
					"key1": []byte("  value1"),
				},
			},
		},
		"create_invalid_secret_filepath_contains_=": {
			secretName: "foo",
			fromFile:   []string{"key1=/file=2"},
			expectErr:  `key names or file paths cannot contain '='`,
		},
		"create_invalid_secret_filepath_key_contains_=": {
			secretName: "foo",
			fromFile:   []string{"=key=/file1"},
			expectErr:  `key names or file paths cannot contain '='`,
		},
		"create_invalid_secret_literal_key_contains_=": {
			secretName:  "foo",
			fromLiteral: []string{"=key=value1"},
			expectErr:   `invalid literal source =key=value1, expected key=value`,
		},
		"create_invalid_secret_literal_key_with_invalid_character": {
			secretName:  "foo",
			fromLiteral: []string{"key#1=value1"},
			expectErr:   `"key#1" is not valid key name for a Secret a valid config key must consist of alphanumeric characters, '-', '_' or '.' (e.g. 'key.name',  or 'KEY_NAME',  or 'key-name', regex used for validation is '[-._a-zA-Z0-9]+')`,
		},
		"create_invalid_secret_base64_literal_key_contains_=": {
			secretName:        "foo",
			fromBase64Literal: []string{"=key=value1"},
			expectErr:         `invalid literal source =key=value1, expected key=value`,
		},
		"create_invalid_secret_base64_literal_key_with_invalid_character": {
			secretName:        "foo",
			fromBase64Literal: []string{"key#1=value1"},
			expectErr:         `base64 string decoding error`,
		},
		"create_invalid_secret_env_key_contains_#": {
			secretName:  "invalid_key",
			setup:       setupSecretEnvFile([][]string{{"key#1=value1"}}),
			fromEnvFile: []string{"file.env"},
			expectErr:   `"key#1" is not a valid key name: a valid environment variable name must consist of alphabetic characters, digits, '_', '-', or '.', and must not start with a digit (e.g. 'my.env-name',  or 'MY_ENV.NAME',  or 'MyEnvName1', regex used for validation is '[-._a-zA-Z][-._a-zA-Z0-9]*')`,
		},
		"create_invalid_secret_env_key_start_with_digit": {
			secretName:  "invalid_key",
			setup:       setupSecretEnvFile([][]string{{"1key=value1"}}),
			fromEnvFile: []string{"file.env"},
			expectErr:   `"1key" is not a valid key name: a valid environment variable name must consist of alphabetic characters, digits, '_', '-', or '.', and must not start with a digit (e.g. 'my.env-name',  or 'MY_ENV.NAME',  or 'MyEnvName1', regex used for validation is '[-._a-zA-Z][-._a-zA-Z0-9]*')`,
		},
		"create_invalid_secret_env_key_with_invalid_character": {
			secretName:  "invalid_key",
			setup:       setupSecretEnvFile([][]string{{"key@=value1"}}),
			fromEnvFile: []string{"file.env"},
			expectErr:   `"key@" is not a valid key name: a valid environment variable name must consist of alphabetic characters, digits, '_', '-', or '.', and must not start with a digit (e.g. 'my.env-name',  or 'MY_ENV.NAME',  or 'MyEnvName1', regex used for validation is '[-._a-zA-Z][-._a-zA-Z0-9]*')`,
		},
		"create_invalid_secret_duplicate_key1": {
			secretName:  "foo",
			fromLiteral: []string{"key1=value1", "key1=value2"},
			expectErr:   `cannot add key key1, another key by that name already exists`,
		},
		"create_invalid_secret_no_file": {
			secretName: "foo",
			fromFile:   []string{"key1=/file1"},
			expectErr:  `error reading /file1: no such file or directory`,
		},
		"create_invalid_secret_invalid_literal": {
			secretName:  "foo",
			fromLiteral: []string{"key1value1"},
			expectErr:   `invalid literal source key1value1, expected key=value`,
		},
		"create_invalid_secret_invalid_base64_literal": {
			secretName:        "foo",
			fromBase64Literal: []string{"key1value1"},
			expectErr:         `invalid literal source key1value1, expected key=value`,
		},
		"create_invalid_secret_invalid_filepath": {
			secretName: "foo",
			fromFile:   []string{"key1==file1"},
			expectErr:  `key names or file paths cannot contain '='`,
		},
		"create_invalid_secret_no_name": {
			expectErr: `name must be specified`,
		},
		"create_invalid_secret_too_many_args": {
			secretName:  "too_many_args",
			fromFile:    []string{"key1=/file1"},
			fromEnvFile: []string{"foo"},
			expectErr:   `from-env-file cannot be combined with from-file, from-literal or from-base64-literal`,
		},
		"create_invalid_secret_too_many_args_1": {
			secretName:  "too_many_args_1",
			fromLiteral: []string{"key1=value1"},
			fromEnvFile: []string{"foo"},
			expectErr:   `from-env-file cannot be combined with from-file, from-literal or from-base64-literal`,
		},
		"create_invalid_secret_too_many_args_2": {
			secretName:        "too_many_args_2",
			fromBase64Literal: []string{"key1=value1"},
			fromEnvFile:       []string{"foo"},
			expectErr:         `from-env-file cannot be combined with from-file, from-literal or from-base64-literal`,
		},
		"create_invalid_secret_too_many_args_3": {
			secretName:  "too_many_args_3",
			fromFile:    []string{"key0=/file1"},
			fromLiteral: []string{"key1=value1"},
			fromEnvFile: []string{"foo"},
			expectErr:   `from-env-file cannot be combined with from-file, from-literal or from-base64-literal`,
		},
		"create_invalid_secret_too_many_args_4": {
			secretName:        "too_many_args_4",
			fromFile:          []string{"key0=/file1"},
			fromBase64Literal: []string{"key1=value1"},
			fromEnvFile:       []string{"foo"},
			expectErr:         `from-env-file cannot be combined with from-file, from-literal or from-base64-literal`,
		},
		"create_invalid_secret_too_many_args_5": {
			secretName:        "too_many_args_5",
			fromLiteral:       []string{"key1=value1"},
			fromBase64Literal: []string{"key2=value2"},
			fromEnvFile:       []string{"foo"},
			expectErr:         `from-env-file cannot be combined with from-file, from-literal or from-base64-literal`,
		},
		"create_invalid_secret_too_many_args_6": {
			secretName:        "too_many_args_6",
			fromFile:          []string{"key0=/file1"},
			fromLiteral:       []string{"key1=value1"},
			fromBase64Literal: []string{"key2=value2"},
			fromEnvFile:       []string{"foo"},
			expectErr:         `from-env-file cannot be combined with from-file, from-literal or from-base64-literal`,
		},
	}

	// run all the tests
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			var secret *corev1.Secret = nil
			secretOptions := CreateSecretOptions{
				Name:           test.secretName,
				Type:           test.secretType,
				AppendHash:     test.appendHash,
				FileSources:    test.fromFile,
				LiteralSources: test.fromLiteral,
				EncodedSources: test.fromBase64Literal,
				EnvFileSources: test.fromEnvFile,
			}
			if test.setup != nil {
				if teardown := test.setup(t, &secretOptions); teardown != nil {
					defer teardown()
				}
			}
			err := secretOptions.Validate()
			if err == nil {
				secret, err = secretOptions.createSecret()
			}

			if test.expectErr == "" {
				require.NoError(t, err)
				if !apiequality.Semantic.DeepEqual(secret, test.expected) {
					t.Errorf("\nexpected:\n%#v\ngot:\n%#v", test.expected, secret)
				}
			} else {
				require.Error(t, err)
				require.EqualError(t, err, test.expectErr)
			}
		})
	}
}

func setupSecretEnvFile(lines [][]string) func(*testing.T, *CreateSecretOptions) func() {
	return func(t *testing.T, secretOptions *CreateSecretOptions) func() {
		files := []*os.File{}
		filenames := secretOptions.EnvFileSources
		for _, filename := range filenames {
			file, err := os.CreateTemp("", filename)
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			files = append(files, file)
		}
		for i, f := range files {
			for _, l := range lines[i] {
				f.WriteString(l)
				f.WriteString("\r\n")
			}
			f.Close()
			secretOptions.EnvFileSources[i] = f.Name()
		}
		return func() {
			for _, f := range files {
				os.Remove(f.Name())
			}
		}
	}
}

func setupSecretBinaryFile(data []byte) func(*testing.T, *CreateSecretOptions) func() {
	return func(t *testing.T, secretOptions *CreateSecretOptions) func() {
		tmp, _ := os.MkdirTemp("", "")
		files := secretOptions.FileSources
		for i, file := range files {
			f := tmp + "/" + file
			os.WriteFile(f, data, 0644)
			secretOptions.FileSources[i] = f
		}
		return func() {
			for _, file := range files {
				f := tmp + "/" + file
				os.RemoveAll(f)
			}
		}
	}
}
