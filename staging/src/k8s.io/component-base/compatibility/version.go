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
	"fmt"
	"sync/atomic"

	"k8s.io/apimachinery/pkg/util/version"
	baseversion "k8s.io/component-base/version"
)

// EffectiveVersion stores all the version information of a component.
type EffectiveVersion interface {
	// BinaryVersion is the binary version of a component. Tied to a particular binary release.
	BinaryVersion() *version.Version
	// EmulationVersion is the version a component emulate its capabilities (APIs, features, ...) of.
	// If EmulationVersion is set to be different from BinaryVersion, the component will emulate the behavior of this version instead of the underlying binary version.
	EmulationVersion() *version.Version
	// MinCompatibilityVersion is the minimum version a component is compatible with (in terms of storage versions, validation rules, ...).
	MinCompatibilityVersion() *version.Version
	EqualTo(other EffectiveVersion) bool
	String() string
	Validate() []error
	// AllowedEmulationVersionRange returns the string of the allowed range of emulation version.
	AllowedEmulationVersionRange() string
	// AllowedMinCompatibilityVersionRange returns the string of the allowed range of min compatibility version.
	AllowedMinCompatibilityVersionRange() string
}

type MutableEffectiveVersion interface {
	EffectiveVersion
	SetEmulationVersion(emulationVersion *version.Version)
	SetMinCompatibilityVersion(minCompatibilityVersion *version.Version)
}

type effectiveVersion struct {
	// When true, BinaryVersion() returns the current binary version
	useDefaultBuildBinaryVersion atomic.Bool
	// Holds the last binary version stored in Set()
	binaryVersion atomic.Pointer[version.Version]
	// If the emulationVersion is set by the users, it could only contain major and minor versions.
	// In tests, emulationVersion could be the same as the binary version, or set directly,
	// which can have "alpha" as pre-release to continue serving expired apis while we clean up the test.
	emulationVersion atomic.Pointer[version.Version]
	// minCompatibilityVersion could only contain major and minor versions.
	minCompatibilityVersion atomic.Pointer[version.Version]
	// emulationVersionFloor is the minimum emulationVersion allowed. No limit if nil.
	emulationVersionFloor *version.Version
	// minCompatibilityVersionFloor is the minimum minCompatibilityVersionFloor allowed. No limit if nil.
	minCompatibilityVersionFloor *version.Version
}

func (m *effectiveVersion) BinaryVersion() *version.Version {
	if m.useDefaultBuildBinaryVersion.Load() {
		return defaultBuildBinaryVersion()
	}
	return m.binaryVersion.Load()
}

func (m *effectiveVersion) EmulationVersion() *version.Version {
	ver := m.emulationVersion.Load()
	if ver != nil {
		// Emulation version can have "alpha" as pre-release to continue serving expired apis while we clean up the test.
		// The pre-release should not be accessible to the users.
		return ver.WithPreRelease(m.BinaryVersion().PreRelease())
	}
	return ver
}

func (m *effectiveVersion) MinCompatibilityVersion() *version.Version {
	return m.minCompatibilityVersion.Load()
}

func (m *effectiveVersion) EqualTo(other EffectiveVersion) bool {
	return m.BinaryVersion().EqualTo(other.BinaryVersion()) && m.EmulationVersion().EqualTo(other.EmulationVersion()) && m.MinCompatibilityVersion().EqualTo(other.MinCompatibilityVersion())
}

func (m *effectiveVersion) String() string {
	if m == nil {
		return "<nil>"
	}
	return fmt.Sprintf("{BinaryVersion: %s, EmulationVersion: %s, MinCompatibilityVersion: %s}",
		m.BinaryVersion().String(), m.EmulationVersion().String(), m.MinCompatibilityVersion().String())
}

func majorMinor(ver *version.Version) *version.Version {
	if ver == nil {
		return ver
	}
	return version.MajorMinor(ver.Major(), ver.Minor())
}

func (m *effectiveVersion) SetEmulationVersion(emulationVersion *version.Version) {
	m.emulationVersion.Store(majorMinor(emulationVersion))
	// set the default minCompatibilityVersion to be emulationVersion - 1 if possible
	minCompatibilityVersion := majorMinor(emulationVersion.SubtractMinor(1))
	if minCompatibilityVersion.LessThan(m.minCompatibilityVersionFloor) {
		minCompatibilityVersion = m.minCompatibilityVersionFloor
	}
	m.minCompatibilityVersion.Store(minCompatibilityVersion)
}

// SetMinCompatibilityVersion should be called after SetEmulationVersion
func (m *effectiveVersion) SetMinCompatibilityVersion(minCompatibilityVersion *version.Version) {
	m.minCompatibilityVersion.Store(majorMinor(minCompatibilityVersion))
}

func (m *effectiveVersion) AllowedEmulationVersionRange() string {
	binaryVersion := m.BinaryVersion()
	if binaryVersion == nil {
		return ""
	}

	// Consider patch version to be 0.
	binaryVersion = version.MajorMinor(binaryVersion.Major(), binaryVersion.Minor())

	floor := m.emulationVersionFloor
	if floor == nil {
		floor = version.MajorMinor(0, 0)
	}

	return fmt.Sprintf("%s..%s (default=%s)", floor.String(), binaryVersion.String(), m.EmulationVersion().String())
}

func (m *effectiveVersion) AllowedMinCompatibilityVersionRange() string {
	binaryVersion := m.BinaryVersion()
	if binaryVersion == nil {
		return ""
	}

	// Consider patch version to be 0.
	binaryVersion = version.MajorMinor(binaryVersion.Major(), binaryVersion.Minor())

	floor := m.minCompatibilityVersionFloor
	if floor == nil {
		floor = version.MajorMinor(0, 0)
	}

	return fmt.Sprintf("%s..%s (default=%s)", floor.String(), binaryVersion.String(), m.MinCompatibilityVersion().String())
}

func (m *effectiveVersion) Validate() []error {
	var errs []error
	// Validate only checks the major and minor versions.
	binaryVersion := m.BinaryVersion().WithPatch(0)
	emulationVersion := m.emulationVersion.Load()
	minCompatibilityVersion := m.minCompatibilityVersion.Load()
	// emulationVersion can only be between emulationVersionFloor and binaryVersion
	if emulationVersion.GreaterThan(binaryVersion) || emulationVersion.LessThan(m.emulationVersionFloor) {
		errs = append(errs, fmt.Errorf("emulation version %s is not between [%s, %s]", emulationVersion.String(), m.emulationVersionFloor.String(), binaryVersion.String()))
	}
	// minCompatibilityVersion can only be between minCompatibilityVersionFloor and emulationVersion
	if minCompatibilityVersion.GreaterThan(emulationVersion) || minCompatibilityVersion.LessThan(m.minCompatibilityVersionFloor) {
		errs = append(errs, fmt.Errorf("minCompatibilityVersion version %s is not between [%s, %s]", minCompatibilityVersion.String(), m.minCompatibilityVersionFloor.String(), emulationVersion.String()))
	}
	return errs
}

// NewEffectiveVersion creates a MutableEffectiveVersion from the binaryVersion.
// If useDefaultBuildBinaryVersion is true, the call of BinaryVersion() will always return the current binary version.
// NewEffectiveVersion(binaryVersion, true) should only be used if the binary version is dynamic.
// Otherwise, use NewEffectiveVersion(binaryVersion, false) or NewEffectiveVersionFromString.
func NewEffectiveVersion(binaryVersion *version.Version, useDefaultBuildBinaryVersion bool, emulationVersionFloor, minCompatibilityVersionFloor *version.Version) MutableEffectiveVersion {
	effective := &effectiveVersion{
		emulationVersionFloor:        emulationVersionFloor,
		minCompatibilityVersionFloor: minCompatibilityVersionFloor,
	}
	compatVersion := binaryVersion.SubtractMinor(1)
	effective.binaryVersion.Store(binaryVersion)
	effective.useDefaultBuildBinaryVersion.Store(useDefaultBuildBinaryVersion)
	effective.SetEmulationVersion(binaryVersion)
	effective.SetMinCompatibilityVersion(compatVersion)
	return effective
}

// NewEffectiveVersionFromString creates a MutableEffectiveVersion from the binaryVersion string.
func NewEffectiveVersionFromString(binaryVer, emulationVerFloor, minCompatibilityVerFloor string) MutableEffectiveVersion {
	if binaryVer == "" {
		return &effectiveVersion{}
	}
	binaryVersion := version.MustParse(binaryVer)
	emulationVersionFloor := version.MajorMinor(0, 0)
	if emulationVerFloor != "" {
		emulationVersionFloor = version.MustParse(emulationVerFloor)
	}
	minCompatibilityVersionFloor := version.MajorMinor(0, 0)
	if minCompatibilityVerFloor != "" {
		minCompatibilityVersionFloor = version.MustParse(minCompatibilityVerFloor)
	}
	return NewEffectiveVersion(binaryVersion, false, emulationVersionFloor, minCompatibilityVersionFloor)
}

func defaultBuildBinaryVersion() *version.Version {
	verInfo := baseversion.Get()
	return version.MustParse(verInfo.String()).WithInfo(verInfo)
}
