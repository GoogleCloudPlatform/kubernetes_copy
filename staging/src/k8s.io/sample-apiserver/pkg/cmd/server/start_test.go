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

package server

import (
	"testing"

	"k8s.io/apimachinery/pkg/util/version"
	"k8s.io/apiserver/pkg/util/compatibility"

	"github.com/stretchr/testify/assert"
)

func TestWardleEmulationVersionToKubeEmulationVersion(t *testing.T) {
	defaultKubeEffectiveVersion := compatibility.DefaultKubeEffectiveVersionForTest()

	testCases := []struct {
		desc                     string
		wardleEmulationVer       *version.Version
		expectedKubeEmulationVer *version.Version
	}{
		{
			desc:                     "same version as than kube binary",
			wardleEmulationVer:       version.MajorMinor(1, 2),
			expectedKubeEmulationVer: defaultKubeEffectiveVersion.BinaryVersion(),
		},
		{
			desc:                     "1 version lower than kube binary",
			wardleEmulationVer:       version.MajorMinor(1, 1),
			expectedKubeEmulationVer: defaultKubeEffectiveVersion.BinaryVersion().OffsetMinor(-1),
		},
		{
			desc:                     "2 versions lower than kube binary",
			wardleEmulationVer:       version.MajorMinor(1, 0),
			expectedKubeEmulationVer: defaultKubeEffectiveVersion.BinaryVersion().OffsetMinor(-2),
		},
		{
			desc:                     "capped at kube binary",
			wardleEmulationVer:       version.MajorMinor(1, 3),
			expectedKubeEmulationVer: defaultKubeEffectiveVersion.BinaryVersion(),
		},
		{
			desc:               "no mapping",
			wardleEmulationVer: version.MajorMinor(2, 10),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.desc, func(t *testing.T) {
			mappedKubeEmulationVer := WardleVersionToKubeVersion(tc.wardleEmulationVer)
			assert.True(t, mappedKubeEmulationVer.EqualTo(tc.expectedKubeEmulationVer))
		})
	}
}
