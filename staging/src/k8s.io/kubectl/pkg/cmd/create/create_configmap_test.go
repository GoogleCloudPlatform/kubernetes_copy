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

package create

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

func TestCreateConfigMap(t *testing.T) {
	tests := map[string]struct {
		configMapName string
		configMapType string
		appendHash    bool
		immutable     bool
		fromLiteral   []string
		fromFile      []string
		fromEnvFile   []string
		setup         func(t *testing.T, configMapOptions *ConfigMapOptions) func()

		expected  *corev1.ConfigMap
		expectErr string
	}{
		"create_foo_configmap": {
			configMapName: "foo",
			expected: &corev1.ConfigMap{
				TypeMeta: metav1.TypeMeta{
					APIVersion: corev1.SchemeGroupVersion.String(),
					Kind:       "ConfigMap",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: "foo",
				},
				Data:       map[string]string{},
				BinaryData: map[string][]byte{},
			},
		},
		"create_foo_hash_configmap": {
			configMapName: "foo",
			appendHash:    true,
			expected: &corev1.ConfigMap{
				TypeMeta: metav1.TypeMeta{
					APIVersion: corev1.SchemeGroupVersion.String(),
					Kind:       "ConfigMap",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: "foo-867km9574f",
				},
				Data:       map[string]string{},
				BinaryData: map[string][]byte{},
			},
		},
		"create_foo_immutable_configmap": {
			configMapName: "foo",
			immutable:     true,
			expected: &corev1.ConfigMap{
				TypeMeta: metav1.TypeMeta{
					APIVersion: corev1.SchemeGroupVersion.String(),
					Kind:       "ConfigMap",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: "foo",
				},
				Data:       map[string]string{},
				BinaryData: map[string][]byte{},
				Immutable:  ptr.To[bool](true),
			},
		},
		"create_foo_type_configmap": {
			configMapName: "foo",
			configMapType: "my-type",
			expected: &corev1.ConfigMap{
				TypeMeta: metav1.TypeMeta{
					APIVersion: corev1.SchemeGroupVersion.String(),
					Kind:       "ConfigMap",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: "foo",
				},
				Data:       map[string]string{},
				BinaryData: map[string][]byte{},
			},
		},
		"create_foo_type_hash_configmap": {
			configMapName: "foo",
			configMapType: "my-type",
			appendHash:    true,
			expected: &corev1.ConfigMap{
				TypeMeta: metav1.TypeMeta{
					APIVersion: corev1.SchemeGroupVersion.String(),
					Kind:       "ConfigMap",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: "foo-867km9574f",
				},
				Data:       map[string]string{},
				BinaryData: map[string][]byte{},
			},
		},
		"create_foo_two_literal_configmap": {
			configMapName: "foo",
			fromLiteral:   []string{"key1=value1", "key2=value2"},
			expected: &corev1.ConfigMap{
				TypeMeta: metav1.TypeMeta{
					APIVersion: corev1.SchemeGroupVersion.String(),
					Kind:       "ConfigMap",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: "foo",
				},
				Data: map[string]string{
					"key1": "value1",
					"key2": "value2",
				},
				BinaryData: map[string][]byte{},
			},
		},
		"create_foo_two_literal_hash_configmap": {
			configMapName: "foo",
			fromLiteral:   []string{"key1=value1", "key2=value2"},
			appendHash:    true,
			expected: &corev1.ConfigMap{
				TypeMeta: metav1.TypeMeta{
					APIVersion: corev1.SchemeGroupVersion.String(),
					Kind:       "ConfigMap",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: "foo-gcb75dd9gb",
				},
				Data: map[string]string{
					"key1": "value1",
					"key2": "value2",
				},
				BinaryData: map[string][]byte{},
			},
		},
		"create_foo_key1_=value1_configmap": {
			configMapName: "foo",
			fromLiteral:   []string{"key1==value1"},
			expected: &corev1.ConfigMap{
				TypeMeta: metav1.TypeMeta{
					APIVersion: corev1.SchemeGroupVersion.String(),
					Kind:       "ConfigMap",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: "foo",
				},
				Data: map[string]string{
					"key1": "=value1",
				},
				BinaryData: map[string][]byte{},
			},
		},
		"create_foo_key1_=value1_hash_configmap": {
			configMapName: "foo",
			fromLiteral:   []string{"key1==value1"},
			appendHash:    true,
			expected: &corev1.ConfigMap{
				TypeMeta: metav1.TypeMeta{
					APIVersion: corev1.SchemeGroupVersion.String(),
					Kind:       "ConfigMap",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: "foo-bdgk9ttt7m",
				},
				Data: map[string]string{
					"key1": "=value1",
				},
				BinaryData: map[string][]byte{},
			},
		},
		"create_foo_from_file_foo1_foo2_configmap": {
			configMapName: "foo",
			setup:         setupBinaryFile([]byte{0x68, 0x65, 0x6c, 0x6c, 0x6f, 0x20, 0x77, 0x6f, 0x72, 0x6c, 0x64}),
			fromFile:      []string{"foo1", "foo2"},
			expected: &corev1.ConfigMap{
				TypeMeta: metav1.TypeMeta{
					APIVersion: corev1.SchemeGroupVersion.String(),
					Kind:       "ConfigMap",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: "foo",
				},
				Data: map[string]string{
					"foo1": "hello world",
					"foo2": "hello world",
				},
				BinaryData: map[string][]byte{},
			},
		},
		"create_foo_from_file_foo1_foo2_and_configmap": {
			configMapName: "foo",
			setup:         setupBinaryFile([]byte{0xff, 0xfd}),
			fromFile:      []string{"foo1", "foo2"},
			expected: &corev1.ConfigMap{
				TypeMeta: metav1.TypeMeta{
					APIVersion: corev1.SchemeGroupVersion.String(),
					Kind:       "ConfigMap",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: "foo",
				},
				Data: map[string]string{},
				BinaryData: map[string][]byte{
					"foo1": {0xff, 0xfd},
					"foo2": {0xff, 0xfd},
				},
			},
		},
		"create_valid_env_from_env_file_configmap": {
			configMapName: "valid_env",
			setup:         setupEnvFile([][]string{{"key1=value1", "#", "", "key2=value2"}}),
			fromEnvFile:   []string{"file.env"},
			expected: &corev1.ConfigMap{
				TypeMeta: metav1.TypeMeta{
					APIVersion: corev1.SchemeGroupVersion.String(),
					Kind:       "ConfigMap",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: "valid_env",
				},
				Data: map[string]string{
					"key1": "value1",
					"key2": "value2",
				},
				BinaryData: map[string][]byte{},
			},
		},
		"create_two_valid_env_from_env_file_configmap": {
			configMapName: "two_valid_env",
			setup:         setupEnvFile([][]string{{"key1=value1", "#", "", "key2=value2"}, {"key3=value3"}}),
			fromEnvFile:   []string{"file1.env", "file2.env"},
			expected: &corev1.ConfigMap{
				TypeMeta: metav1.TypeMeta{
					APIVersion: corev1.SchemeGroupVersion.String(),
					Kind:       "ConfigMap",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: "two_valid_env",
				},
				Data: map[string]string{
					"key1": "value1",
					"key2": "value2",
					"key3": "value3",
				},
				BinaryData: map[string][]byte{},
			},
		},
		"create_valid_env_from_env_file_hash_configmap": {
			configMapName: "valid_env",
			setup:         setupEnvFile([][]string{{"key1=value1", "#", "", "key2=value2"}}),
			fromEnvFile:   []string{"file.env"},
			appendHash:    true,
			expected: &corev1.ConfigMap{
				TypeMeta: metav1.TypeMeta{
					APIVersion: corev1.SchemeGroupVersion.String(),
					Kind:       "ConfigMap",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: "valid_env-2cgh8552ch",
				},
				Data: map[string]string{
					"key1": "value1",
					"key2": "value2",
				},
				BinaryData: map[string][]byte{},
			},
		},
		"create_two_valid_env_from_env_file_hash_configmap": {
			configMapName: "two_valid_env",
			setup:         setupEnvFile([][]string{{"key1=value1", "#", "", "key2=value2"}, {"key3=value3"}}),
			fromEnvFile:   []string{"file1.env", "file2.env"},
			appendHash:    true,
			expected: &corev1.ConfigMap{
				TypeMeta: metav1.TypeMeta{
					APIVersion: corev1.SchemeGroupVersion.String(),
					Kind:       "ConfigMap",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: "two_valid_env-2m5tm82522",
				},
				Data: map[string]string{
					"key1": "value1",
					"key2": "value2",
					"key3": "value3",
				},
				BinaryData: map[string][]byte{},
			},
		},
		"create_get_env_from_env_file_configmap": {
			configMapName: "get_env",
			setup: func() func(t *testing.T, configMapOptions *ConfigMapOptions) func() {
				t.Setenv("g_key1", "1")
				t.Setenv("g_key2", "2")
				return setupEnvFile([][]string{{"g_key1", "g_key2="}})
			}(),
			fromEnvFile: []string{"file.env"},
			expected: &corev1.ConfigMap{
				TypeMeta: metav1.TypeMeta{
					APIVersion: corev1.SchemeGroupVersion.String(),
					Kind:       "ConfigMap",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: "get_env",
				},
				Data: map[string]string{
					"g_key1": "1",
					"g_key2": "",
				},
				BinaryData: map[string][]byte{},
			},
		},
		"create_get_env_from_env_file_hash_configmap": {
			configMapName: "get_env",
			setup: func() func(t *testing.T, configMapOptions *ConfigMapOptions) func() {
				t.Setenv("g_key1", "1")
				t.Setenv("g_key2", "2")
				return setupEnvFile([][]string{{"g_key1", "g_key2="}})
			}(),
			fromEnvFile: []string{"file.env"},
			appendHash:  true,
			expected: &corev1.ConfigMap{
				TypeMeta: metav1.TypeMeta{
					APIVersion: corev1.SchemeGroupVersion.String(),
					Kind:       "ConfigMap",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: "get_env-54k882kkd2",
				},
				Data: map[string]string{
					"g_key1": "1",
					"g_key2": "",
				},
				BinaryData: map[string][]byte{},
			},
		},
		"create_value_with_space_from_env_file_configmap": {
			configMapName: "value_with_space",
			setup:         setupEnvFile([][]string{{"key1=  value1"}}),
			fromEnvFile:   []string{"file.env"},
			expected: &corev1.ConfigMap{
				TypeMeta: metav1.TypeMeta{
					APIVersion: corev1.SchemeGroupVersion.String(),
					Kind:       "ConfigMap",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: "value_with_space",
				},
				Data: map[string]string{
					"key1": "  value1",
				},
				BinaryData: map[string][]byte{},
			},
		},
		"create_value_with_space_from_env_file_hash_configmap": {
			configMapName: "valid_with_space",
			setup:         setupEnvFile([][]string{{"key1=  value1"}}),
			fromEnvFile:   []string{"file.env"},
			appendHash:    true,
			expected: &corev1.ConfigMap{
				TypeMeta: metav1.TypeMeta{
					APIVersion: corev1.SchemeGroupVersion.String(),
					Kind:       "ConfigMap",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: "valid_with_space-b4448m7gdm",
				},
				Data: map[string]string{
					"key1": "  value1",
				},
				BinaryData: map[string][]byte{},
			},
		},
		"create_invalid_configmap_filepath_contains_=": {
			configMapName: "foo",
			fromFile:      []string{"key1=/file=2"},
			expectErr:     `key names or file paths cannot contain '='`,
		},
		"create_invalid_configmap_filepath_key_contains_=": {
			configMapName: "foo",
			fromFile:      []string{"=key=/file1"},
			expectErr:     `key names or file paths cannot contain '='`,
		},
		"create_invalid_configmap_literal_key_contains_=": {
			configMapName: "foo",
			fromFile:      []string{"=key=value1"},
			expectErr:     `key names or file paths cannot contain '='`,
		},
		"create_invalid_configmap_duplicate_key1": {
			configMapName: "foo",
			fromLiteral:   []string{"key1=value1", "key1=value2"},
			expectErr:     `cannot add key "key1", another key by that name already exists in Data for ConfigMap "foo"`,
		},
		"create_invalid_configmap_no_file": {
			configMapName: "foo",
			fromFile:      []string{"key1=/file1"},
			expectErr:     `error reading /file1: no such file or directory`,
		},
		"create_invalid_configmap_invalid_literal": {
			configMapName: "foo",
			fromLiteral:   []string{"key1value1"},
			expectErr:     `invalid literal source key1value1, expected key=value`,
		},
		"create_invalid_configmap_invalid_filepath": {
			configMapName: "foo",
			fromFile:      []string{"key1==file1"},
			expectErr:     `key names or file paths cannot contain '='`,
		},
		"create_invalid_configmap_too_many_args": {
			configMapName: "too_many_args",
			fromFile:      []string{"key1=/file1"},
			fromEnvFile:   []string{"file.env"},
			expectErr:     `from-env-file cannot be combined with from-file or from-literal`,
		},
		"create_invalid_configmap_too_many_args_1": {
			configMapName: "too_many_args_1",
			fromLiteral:   []string{"key1=value1"},
			fromEnvFile:   []string{"file.env"},
			expectErr:     `from-env-file cannot be combined with from-file or from-literal`,
		},
	}

	// run all the tests
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			var configMap *corev1.ConfigMap = nil
			configMapOptions := ConfigMapOptions{
				Name:           test.configMapName,
				Type:           test.configMapType,
				AppendHash:     test.appendHash,
				Immutable:      test.immutable,
				FileSources:    test.fromFile,
				LiteralSources: test.fromLiteral,
				EnvFileSources: test.fromEnvFile,
			}

			if test.setup != nil {
				if teardown := test.setup(t, &configMapOptions); teardown != nil {
					defer teardown()
				}
			}
			err := configMapOptions.Validate()

			if err == nil {
				configMap, err = configMapOptions.createConfigMap()
			}
			if test.expectErr == "" {
				require.NoError(t, err)
				if !apiequality.Semantic.DeepEqual(configMap, test.expected) {
					t.Errorf("\nexpected:\n%#v\ngot:\n%#v", test.expected, configMap)
				}
			} else {
				require.Error(t, err)
				require.EqualError(t, err, test.expectErr)
			}
		})
	}
}

func setupEnvFile(lines [][]string) func(*testing.T, *ConfigMapOptions) func() {
	return func(t *testing.T, configMapOptions *ConfigMapOptions) func() {
		files := []*os.File{}
		filenames := configMapOptions.EnvFileSources
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
			configMapOptions.EnvFileSources[i] = f.Name()
		}
		return func() {
			for _, f := range files {
				os.Remove(f.Name())
			}
		}
	}
}

func setupBinaryFile(data []byte) func(*testing.T, *ConfigMapOptions) func() {
	return func(t *testing.T, configMapOptions *ConfigMapOptions) func() {
		tmp, _ := os.MkdirTemp("", "")
		files := configMapOptions.FileSources
		for i, file := range files {
			f := tmp + "/" + file
			os.WriteFile(f, data, 0644)
			configMapOptions.FileSources[i] = f
		}
		return func() {
			for _, file := range files {
				f := tmp + "/" + file
				os.Remove(f)
			}
		}
	}
}
