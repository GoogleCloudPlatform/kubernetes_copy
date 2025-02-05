/*
Copyright 2019 The Kubernetes Authors.

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

package runtime

import (
	"context"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/uuid"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

func TestDecodeInto(t *testing.T) {
	type PluginFooConfig struct {
		FooTest string `json:"fooTest,omitempty"`
	}
	tests := []struct {
		name     string
		args     *runtime.Unknown
		expected PluginFooConfig
	}{
		{
			name: "test decode for JSON config",
			args: &runtime.Unknown{
				ContentType: runtime.ContentTypeJSON,
				Raw: []byte(`{
					"fooTest": "test decode"
				}`),
			},
			expected: PluginFooConfig{
				FooTest: "test decode",
			},
		},
		{
			name: "test decode for YAML config",
			args: &runtime.Unknown{
				ContentType: runtime.ContentTypeYAML,
				Raw:         []byte(`fooTest: "test decode"`),
			},
			expected: PluginFooConfig{
				FooTest: "test decode",
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var pluginFooConf PluginFooConfig
			if err := DecodeInto(test.args, &pluginFooConf); err != nil {
				t.Errorf("DecodeInto(): failed to decode args %+v: %v", test.args, err)
			}
			if diff := cmp.Diff(test.expected, pluginFooConf); diff != "" {
				t.Errorf("DecodeInto(): failed to decode plugin config (-want,+got):\n%s", diff)
			}
		})
	}
}

// isRegistryEqual compares two registries for equality. This function is used in place of
// reflect.DeepEqual() and cmp() as they don't compare function values.
func isRegistryEqual(registryX, registryY Registry) bool {
	for name, pluginFactory := range registryY {
		if val, ok := registryX[name]; ok {
			p1, _ := pluginFactory(nil, nil, nil)
			p2, _ := val(nil, nil, nil)
			if p1.Name() != p2.Name() {
				// pluginFactory functions are not the same.
				return false
			}
		} else {
			// registryY contains an entry that is not present in registryX
			return false
		}
	}

	for name := range registryX {
		if _, ok := registryY[name]; !ok {
			// registryX contains an entry that is not present in registryY
			return false
		}
	}

	return true
}

type mockNoopPlugin struct {
	uuid string
}

func (p *mockNoopPlugin) Name() string {
	return p.uuid
}

func NewMockNoopPluginFactory() PluginFactory {
	uuid := uuid.New().String()
	return func(_ context.Context, _ runtime.Object, _ framework.Handle) (framework.Plugin, error) {
		return &mockNoopPlugin{uuid}, nil
	}
}

func TestMerge(t *testing.T) {
	m1 := NewMockNoopPluginFactory()
	m2 := NewMockNoopPluginFactory()
	tests := []struct {
		name            string
		primaryRegistry Registry
		registryToMerge Registry
		expected        Registry
		shouldError     bool
	}{
		{
			name: "valid Merge",
			primaryRegistry: Registry{
				"pluginFactory1": m1,
			},
			registryToMerge: Registry{
				"pluginFactory2": m2,
			},
			expected: Registry{
				"pluginFactory1": m1,
				"pluginFactory2": m2,
			},
			shouldError: false,
		},
		{
			name: "Merge duplicate factories",
			primaryRegistry: Registry{
				"pluginFactory1": m1,
			},
			registryToMerge: Registry{
				"pluginFactory1": m2,
			},
			expected: Registry{
				"pluginFactory1": m1,
			},
			shouldError: true,
		},
	}

	for _, scenario := range tests {
		t.Run(scenario.name, func(t *testing.T) {
			err := scenario.primaryRegistry.Merge(scenario.registryToMerge)

			if (err == nil) == scenario.shouldError {
				t.Errorf("Merge() shouldError is: %v, however err is: %v.", scenario.shouldError, err)
				return
			}

			if !isRegistryEqual(scenario.expected, scenario.primaryRegistry) {
				t.Errorf("Merge(). Expected %v. Got %v instead.", scenario.expected, scenario.primaryRegistry)
			}
		})
	}
}

func TestRegister(t *testing.T) {
	m1 := NewMockNoopPluginFactory()
	m2 := NewMockNoopPluginFactory()
	tests := []struct {
		name              string
		registry          Registry
		nameToRegister    string
		factoryToRegister PluginFactory
		expected          Registry
		shouldError       bool
	}{
		{
			name:              "valid Register",
			registry:          Registry{},
			nameToRegister:    "pluginFactory1",
			factoryToRegister: m1,
			expected: Registry{
				"pluginFactory1": m1,
			},
			shouldError: false,
		},
		{
			name: "Register duplicate factories",
			registry: Registry{
				"pluginFactory1": m1,
			},
			nameToRegister:    "pluginFactory1",
			factoryToRegister: m2,
			expected: Registry{
				"pluginFactory1": m1,
			},
			shouldError: true,
		},
	}

	for _, scenario := range tests {
		t.Run(scenario.name, func(t *testing.T) {
			err := scenario.registry.Register(scenario.nameToRegister, scenario.factoryToRegister)

			if (err == nil) == scenario.shouldError {
				t.Errorf("Register() shouldError is: %v however err is: %v.", scenario.shouldError, err)
				return
			}

			if !isRegistryEqual(scenario.expected, scenario.registry) {
				t.Errorf("Register(). Expected %v. Got %v instead.", scenario.expected, scenario.registry)
			}
		})
	}
}

func TestUnregister(t *testing.T) {
	m1 := NewMockNoopPluginFactory()
	m2 := NewMockNoopPluginFactory()
	tests := []struct {
		name             string
		registry         Registry
		nameToUnregister string
		expected         Registry
		shouldError      bool
	}{
		{
			name: "valid Unregister",
			registry: Registry{
				"pluginFactory1": m1,
				"pluginFactory2": m2,
			},
			nameToUnregister: "pluginFactory1",
			expected: Registry{
				"pluginFactory2": m2,
			},
			shouldError: false,
		},
		{
			name:             "Unregister non-existent plugin factory",
			registry:         Registry{},
			nameToUnregister: "pluginFactory1",
			expected:         Registry{},
			shouldError:      true,
		},
	}

	for _, scenario := range tests {
		t.Run(scenario.name, func(t *testing.T) {
			err := scenario.registry.Unregister(scenario.nameToUnregister)

			if (err == nil) == scenario.shouldError {
				t.Errorf("Unregister() shouldError is: %v however err is: %v.", scenario.shouldError, err)
				return
			}

			if !isRegistryEqual(scenario.expected, scenario.registry) {
				t.Errorf("Unregister(). Expected %v. Got %v instead.", scenario.expected, scenario.registry)
			}
		})
	}
}
