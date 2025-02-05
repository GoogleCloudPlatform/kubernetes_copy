/*
Copyright 2024 The Kubernetes Authors.

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

package statusz

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/util/version"
	"k8s.io/component-base/compatibility"
	utilversion "k8s.io/component-base/version"
)

func TestBinaryVersion(t *testing.T) {
	tests := []struct {
		name                    string
		setFakeEffectiveVersion bool
		fakeVersion             string
		wantBinaryVersion       *version.Version
	}{
		{
			name:                    "binaryVersion with effective version",
			wantBinaryVersion:       version.MustParseSemantic("v1.2.3"),
			setFakeEffectiveVersion: true,
			fakeVersion:             "1.2.3",
		},
		{
			name:              "binaryVersion without effective version",
			wantBinaryVersion: version.MustParse(utilversion.Get().String()),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry := &registry{}
			if tt.setFakeEffectiveVersion {
				verKube := compatibility.NewEffectiveVersionFromString(tt.fakeVersion, "", "")
				registry.effectiveVersion = verKube
			}

			got := registry.binaryVersion()
			assert.Equal(t, tt.wantBinaryVersion, got)
		})
	}
}

func TestEmulationVersion(t *testing.T) {
	tests := []struct {
		name                    string
		setFakeEffectiveVersion bool
		fakeEmulVer             string
		wantEmul                *version.Version
	}{
		{
			name:                    "emulationVersion with effective version",
			fakeEmulVer:             "2.3.4",
			setFakeEffectiveVersion: true,
			wantEmul:                version.MustParseSemantic("2.3.4"),
		},
		{
			name:     "emulationVersion without effective version",
			wantEmul: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry := &registry{}
			if tt.setFakeEffectiveVersion {
				verKube := compatibility.NewEffectiveVersionFromString("0.0.0", "", "")
				verKube.SetEmulationVersion(version.MustParse(tt.fakeEmulVer))
				registry.effectiveVersion = verKube
			}

			got := registry.emulationVersion()
			if tt.wantEmul != nil && got != nil {
				assert.Equal(t, tt.wantEmul.Major(), got.Major())
				assert.Equal(t, tt.wantEmul.Minor(), got.Minor())
			} else {
				assert.Equal(t, tt.wantEmul, got)
			}
		})
	}
}
