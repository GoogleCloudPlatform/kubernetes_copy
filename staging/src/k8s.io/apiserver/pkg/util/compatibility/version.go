/*
Copyright 2025 The Kubernetes Authors.

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

package compatibility

import (
	"k8s.io/apimachinery/pkg/util/version"
	basecompatibility "k8s.io/component-base/compatibility"
	baseversion "k8s.io/component-base/version"
)

// minimumKubeEmulationVersion is the first release emulation version is introduced,
// so the emulation version cannot go lower than that.
var minimumKubeEmulationVersion *version.Version = version.MajorMinor(1, 31)

// DefaultBuildEffectiveVersion returns the MutableEffectiveVersion based on the
// current build information.
func DefaultBuildEffectiveVersion() basecompatibility.MutableEffectiveVersion {
	binaryVersion := defaultBuildBinaryVersion()
	useDefaultBuildBinaryVersion := true
	// fall back to the hard coded kube version only when the git tag is not available for local unit tests.
	if binaryVersion.Major() == 0 && binaryVersion.Minor() == 0 {
		useDefaultBuildBinaryVersion = false
		binaryVersion = version.MustParse(baseversion.DefaultKubeBinaryVersion)
	}
	versionFloor := kubeEffectiveVersionFloors(binaryVersion)
	return basecompatibility.NewEffectiveVersion(binaryVersion, useDefaultBuildBinaryVersion, versionFloor, versionFloor)
}

func kubeEffectiveVersionFloors(binaryVersion *version.Version) *version.Version {
	// both emulationVersion and minCompatibilityVersion can be set to binaryVersion - 3
	versionFloor := binaryVersion.WithPatch(0).SubtractMinor(3)
	if versionFloor.LessThan(minimumKubeEmulationVersion) {
		versionFloor = minimumKubeEmulationVersion
	}
	return versionFloor
}

// DefaultKubeEffectiveVersionForTest returns the MutableEffectiveVersion based on the
// latest K8s release hardcoded in DefaultKubeBinaryVersion.
// DefaultKubeBinaryVersion is hard coded because defaultBuildBinaryVersion would return 0.0 when test is run without a git tag.
// We do not enforce the N-3..N emulation version range in tests so that the tests would not automatically fail when there is a version bump.
// Only used in tests.
func DefaultKubeEffectiveVersionForTest() basecompatibility.MutableEffectiveVersion {
	binaryVersion := version.MustParse(baseversion.DefaultKubeBinaryVersion).WithInfo(baseversion.Get())
	return basecompatibility.NewEffectiveVersion(binaryVersion, false, version.MustParse("0.0"), version.MustParse("0.0"))
}

func defaultBuildBinaryVersion() *version.Version {
	verInfo := baseversion.Get()
	return version.MustParse(verInfo.String()).WithInfo(verInfo)
}
