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

package v1

import (
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/util/version"
	"k8s.io/component-base/featuregate"
)

func TestRegister(t *testing.T) {
	tests := []struct {
		name             string
		initialRegistry  map[string]logFormat
		frozen           bool
		nameToRegister   string
		formatToRegister logFormat
		wantErr          bool
		expectedErrMsg   string
		expectedRegistry map[string]logFormat
	}{
		{
			name:             "successful registration",
			initialRegistry:  map[string]logFormat{},
			frozen:           false,
			nameToRegister:   "json",
			formatToRegister: logFormat{factory: nil, versionedFeatures: []featuregate.VersionedFeature{{Version: version.MajorMinor(1, 24), Feature: LoggingBetaOptions}}},
			wantErr:          false,
			expectedRegistry: map[string]logFormat{"json": {factory: nil, versionedFeatures: []featuregate.VersionedFeature{{Version: version.MajorMinor(1, 24), Feature: LoggingBetaOptions}}}},
		},
		{
			name:             "registry is frozen",
			initialRegistry:  map[string]logFormat{},
			frozen:           true,
			nameToRegister:   "json",
			formatToRegister: logFormat{factory: nil, versionedFeatures: []featuregate.VersionedFeature{{Version: version.MajorMinor(1, 24), Feature: LoggingBetaOptions}}},
			wantErr:          true,
			expectedErrMsg:   "log format registry is frozen, unable to register log format json",
			expectedRegistry: map[string]logFormat{},
		},
		{
			name:             "log format already exists",
			initialRegistry:  map[string]logFormat{"json": {factory: nil, versionedFeatures: []featuregate.VersionedFeature{{Version: version.MajorMinor(1, 24), Feature: LoggingBetaOptions}}}},
			frozen:           false,
			nameToRegister:   "json",
			formatToRegister: logFormat{factory: nil, versionedFeatures: []featuregate.VersionedFeature{{Version: version.MajorMinor(1, 25), Feature: LoggingStableOptions}}},
			wantErr:          true,
			expectedErrMsg:   "log format: json already exists",
			expectedRegistry: map[string]logFormat{"json": {factory: nil, versionedFeatures: []featuregate.VersionedFeature{{Version: version.MajorMinor(1, 24), Feature: LoggingBetaOptions}}}},
		},
		{
			name:             "unsupported feature gate",
			initialRegistry:  map[string]logFormat{},
			frozen:           false,
			nameToRegister:   "json",
			formatToRegister: logFormat{factory: nil, versionedFeatures: []featuregate.VersionedFeature{{Version: version.MajorMinor(1, 24), Feature: "UnsupportedFeature"}}},
			wantErr:          true,
			expectedErrMsg:   "log format json: unsupported feature gate UnsupportedFeature",
			expectedRegistry: map[string]logFormat{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry := &logFormatRegistry{
				registry: tt.initialRegistry,
				frozen:   tt.frozen,
			}
			err := registry.register(tt.nameToRegister, tt.formatToRegister)
			if (err != nil) != tt.wantErr {
				t.Errorf("register() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if err != nil && err.Error() != tt.expectedErrMsg {
				t.Errorf("register() error = %v, expectedErrMsg %v", err, tt.expectedErrMsg)
			}
			if !tt.wantErr && !reflect.DeepEqual(registry.registry, tt.expectedRegistry) {
				t.Errorf("register() registry = %v, expectedRegistry %v", registry.registry, tt.expectedRegistry)
			}
		})
	}
}
