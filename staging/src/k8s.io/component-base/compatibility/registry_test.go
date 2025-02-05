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
	"strings"
	"testing"

	"github.com/spf13/pflag"

	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/version"
	"k8s.io/component-base/featuregate"
)

const (
	testComponent = "test"
)

func TestEffectiveVersionRegistry(t *testing.T) {
	r := NewComponentGlobalsRegistry()
	ver1 := NewEffectiveVersionFromString("1.31", "", "")
	ver2 := NewEffectiveVersionFromString("1.28", "", "")

	if r.EffectiveVersionFor(testComponent) != nil {
		t.Fatalf("expected nil EffectiveVersion initially")
	}
	if err := r.Register(testComponent, ver1, nil); err != nil {
		t.Fatalf("expected no error to register new component, but got err: %v", err)
	}
	if !r.EffectiveVersionFor(testComponent).EqualTo(ver1) {
		t.Fatalf("expected EffectiveVersionFor to return the version registered")
	}
	// overwrite
	if err := r.Register(testComponent, ver2, nil); err == nil {
		t.Fatalf("expected error to register existing component when override is false")
	}
	if !r.EffectiveVersionFor(testComponent).EqualTo(ver1) {
		t.Fatalf("expected EffectiveVersionFor to return the version overridden")
	}
}

func testRegistry(t *testing.T) *componentGlobalsRegistry {
	r := NewComponentGlobalsRegistry()
	verKube := NewEffectiveVersionFromString("1.31", "1.31", "1.30")
	fgKube := featuregate.NewVersionedFeatureGate(version.MustParse("0.0"))
	err := fgKube.AddVersioned(map[featuregate.Feature]featuregate.VersionedSpecs{
		"kubeA": {
			{Version: version.MustParse("1.27"), Default: false, PreRelease: featuregate.Alpha},
			{Version: version.MustParse("1.28"), Default: false, PreRelease: featuregate.Beta},
			{Version: version.MustParse("1.31"), Default: true, LockToDefault: true, PreRelease: featuregate.GA},
		},
		"kubeB": {
			{Version: version.MustParse("1.30"), Default: false, PreRelease: featuregate.Alpha},
		},
		"commonC": {
			{Version: version.MustParse("1.27"), Default: false, PreRelease: featuregate.Alpha},
			{Version: version.MustParse("1.29"), Default: true, PreRelease: featuregate.Beta},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	verTest := NewEffectiveVersionFromString("2.8", "2.8", "2.7")
	fgTest := featuregate.NewVersionedFeatureGate(version.MustParse("0.0"))
	err = fgTest.AddVersioned(map[featuregate.Feature]featuregate.VersionedSpecs{
		"testA": {
			{Version: version.MustParse("2.7"), Default: false, PreRelease: featuregate.Alpha},
			{Version: version.MustParse("2.8"), Default: false, PreRelease: featuregate.Beta},
			{Version: version.MustParse("2.10"), Default: true, PreRelease: featuregate.GA},
		},
		"testB": {
			{Version: version.MustParse("2.9"), Default: false, PreRelease: featuregate.Alpha},
		},
		"commonC": {
			{Version: version.MustParse("2.7"), Default: false, PreRelease: featuregate.Alpha},
			{Version: version.MustParse("2.9"), Default: true, PreRelease: featuregate.Beta},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	utilruntime.Must(r.Register(DefaultKubeComponent, verKube, fgKube))
	utilruntime.Must(r.Register(testComponent, verTest, fgTest))
	return r
}

func TestVersionFlagOptions(t *testing.T) {
	r := testRegistry(t)
	emuVers := strings.Join(r.unsafeVersionFlagOptions(true), "\n")
	expectedEmuVers := "kube=1.31..1.31 (default=1.31)\ntest=2.8..2.8 (default=2.8)"
	if emuVers != expectedEmuVers {
		t.Errorf("wanted emulation version flag options to be: %s, got %s", expectedEmuVers, emuVers)
	}
	minCompVers := strings.Join(r.unsafeVersionFlagOptions(false), "\n")
	expectedMinCompVers := "kube=1.30..1.31 (default=1.30)\ntest=2.7..2.8 (default=2.7)"
	if minCompVers != expectedMinCompVers {
		t.Errorf("wanted min compatibility version flag options to be: %s, got %s", expectedMinCompVers, minCompVers)
	}
}

func TestVersionFlagOptionsWithMapping(t *testing.T) {
	r := testRegistry(t)
	utilruntime.Must(r.SetEmulationVersionMapping(testComponent, DefaultKubeComponent,
		func(from *version.Version) *version.Version { return version.MajorMinor(1, from.Minor()+23) }))
	emuVers := strings.Join(r.unsafeVersionFlagOptions(true), "\n")
	expectedEmuVers := "test=2.8..2.8 (default=2.8)"
	if emuVers != expectedEmuVers {
		t.Errorf("wanted emulation version flag options to be: %s, got %s", expectedEmuVers, emuVers)
	}
	minCompVers := strings.Join(r.unsafeVersionFlagOptions(false), "\n")
	expectedMinCompVers := "kube=1.30..1.31 (default=1.30)\ntest=2.7..2.8 (default=2.7)"
	if minCompVers != expectedMinCompVers {
		t.Errorf("wanted min compatibility version flag options to be: %s, got %s", expectedMinCompVers, minCompVers)
	}
}

func TestVersionedFeatureGateFlags(t *testing.T) {
	r := testRegistry(t)
	known := strings.Join(r.unsafeKnownFeatures(), "\n")
	expectedKnown := "kube:AllAlpha=true|false (ALPHA - default=false)\n" +
		"kube:AllBeta=true|false (BETA - default=false)\n" +
		"kube:commonC=true|false (BETA - default=true)\n" +
		"kube:kubeB=true|false (ALPHA - default=false)\n" +
		"test:AllAlpha=true|false (ALPHA - default=false)\n" +
		"test:AllBeta=true|false (BETA - default=false)\n" +
		"test:commonC=true|false (ALPHA - default=false)\n" +
		"test:testA=true|false (BETA - default=false)"
	if known != expectedKnown {
		t.Errorf("wanted min compatibility version flag options to be:\n%s, got:\n%s", expectedKnown, known)
	}
}

func TestFlags(t *testing.T) {
	tests := []struct {
		name                         string
		flags                        []string
		parseError                   string
		expectedKubeEmulationVersion string
		expectedTestEmulationVersion string
		expectedKubeFeatureValues    map[featuregate.Feature]bool
		expectedTestFeatureValues    map[featuregate.Feature]bool
	}{
		{
			name:                         "setting kube emulation version",
			flags:                        []string{"--emulated-version=kube=1.30"},
			expectedKubeEmulationVersion: "1.30",
		},
		{
			name: "setting kube emulation version twice",
			flags: []string{
				"--emulated-version=kube=1.30",
				"--emulated-version=kube=1.32",
			},
			parseError: "duplicate version flag, kube=1.30 and kube=1.32",
		},
		{
			name:                         "prefix v ok",
			flags:                        []string{"--emulated-version=kube=v1.30"},
			expectedKubeEmulationVersion: "1.30",
		},
		{
			name:       "patch version not ok",
			flags:      []string{"--emulated-version=kube=1.30.2"},
			parseError: "patch version not allowed, got: kube=1.30.2",
		},
		{
			name:                         "setting test emulation version",
			flags:                        []string{"--emulated-version=test=2.7"},
			expectedKubeEmulationVersion: "1.31",
			expectedTestEmulationVersion: "2.7",
		},
		{
			name:                         "version missing component default to kube",
			flags:                        []string{"--emulated-version=1.30"},
			expectedKubeEmulationVersion: "1.30",
		},
		{
			name:       "version missing component default to kube with duplicate",
			flags:      []string{"--emulated-version=1.30", "--emulated-version=kube=1.30"},
			parseError: "duplicate version flag, kube=1.30 and kube=1.30",
		},
		{
			name:       "version unregistered component",
			flags:      []string{"--emulated-version=test3=1.31"},
			parseError: "component not registered: test3",
		},
		{
			name:       "invalid version",
			flags:      []string{"--emulated-version=test=1.foo"},
			parseError: "illegal version string \"1.foo\"",
		},
		{
			name: "setting test feature flag",
			flags: []string{
				"--emulated-version=test=2.7",
				"--feature-gates=test:testA=true",
			},
			expectedKubeEmulationVersion: "1.31",
			expectedTestEmulationVersion: "2.7",
			expectedKubeFeatureValues:    map[featuregate.Feature]bool{"kubeA": true, "kubeB": false, "commonC": true},
			expectedTestFeatureValues:    map[featuregate.Feature]bool{"testA": true, "testB": false, "commonC": false},
		},
		{
			name: "setting future test feature flag",
			flags: []string{
				"--emulated-version=test=2.7",
				"--feature-gates=test:testA=true,test:testB=true",
			},
			parseError: "cannot set feature gate testB to true, feature is PreAlpha at emulated version 2.7",
		},
		{
			name: "setting kube feature flag",
			flags: []string{
				"--emulated-version=test=2.7",
				"--emulated-version=kube=1.30",
				"--feature-gates=kubeB=false,test:commonC=true",
				"--feature-gates=commonC=false,kubeB=true",
			},
			expectedKubeEmulationVersion: "1.30",
			expectedTestEmulationVersion: "2.7",
			expectedKubeFeatureValues:    map[featuregate.Feature]bool{"kubeA": false, "kubeB": true, "commonC": false},
			expectedTestFeatureValues:    map[featuregate.Feature]bool{"testA": false, "testB": false, "commonC": true},
		},
		{
			name: "setting kube feature flag with different prefix",
			flags: []string{
				"--emulated-version=test=2.7",
				"--emulated-version=kube=1.30",
				"--feature-gates=kube:kubeB=false,test:commonC=true",
				"--feature-gates=commonC=false,kubeB=true",
			},
			parseError: "set kube feature gates with default empty prefix or kube: prefix consistently, do not mix use",
		},
		{
			name: "setting locked kube feature flag",
			flags: []string{
				"--emulated-version=test=2.7",
				"--feature-gates=kubeA=false",
			},
			parseError: "cannot set feature gate kubeA to false, feature is locked to true",
		},
		{
			name: "setting unknown test feature flag",
			flags: []string{
				"--emulated-version=test=2.7",
				"--feature-gates=test:testD=true",
			},
			parseError: "unrecognized feature gate: testD",
		},
		{
			name: "setting unknown component feature flag",
			flags: []string{
				"--emulated-version=test=2.7",
				"--feature-gates=test3:commonC=true",
			},
			parseError: "component not registered: test3",
		},
	}
	for i, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fs := pflag.NewFlagSet("testflag", pflag.ContinueOnError)
			r := testRegistry(t)
			r.AddFlags(fs)
			err := fs.Parse(test.flags)
			if err == nil {
				err = r.Set()
			}
			if test.parseError != "" {
				if err == nil || !strings.Contains(err.Error(), test.parseError) {
					t.Fatalf("%d: Parse() expected: %v, got: %v", i, test.parseError, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("%d: Parse() expected: nil, got: %v", i, err)
			}
			if len(test.expectedKubeEmulationVersion) > 0 {
				assertVersionEqualTo(t, r.EffectiveVersionFor(DefaultKubeComponent).EmulationVersion(), test.expectedKubeEmulationVersion)
			}
			if len(test.expectedTestEmulationVersion) > 0 {
				assertVersionEqualTo(t, r.EffectiveVersionFor(testComponent).EmulationVersion(), test.expectedTestEmulationVersion)
			}
			for f, v := range test.expectedKubeFeatureValues {
				if r.FeatureGateFor(DefaultKubeComponent).Enabled(f) != v {
					t.Errorf("%d: expected kube feature Enabled(%s)=%v", i, f, v)
				}
			}
			for f, v := range test.expectedTestFeatureValues {
				if r.FeatureGateFor(testComponent).Enabled(f) != v {
					t.Errorf("%d: expected test feature Enabled(%s)=%v", i, f, v)
				}
			}
		})
	}
}

func TestVersionMapping(t *testing.T) {
	r := NewComponentGlobalsRegistry()
	ver1 := NewEffectiveVersionFromString("0.58", "", "")
	ver2 := NewEffectiveVersionFromString("1.28", "", "")
	ver3 := NewEffectiveVersionFromString("2.10", "", "")

	utilruntime.Must(r.Register("test1", ver1, nil))
	utilruntime.Must(r.Register("test2", ver2, nil))
	utilruntime.Must(r.Register("test3", ver3, nil))

	assertVersionEqualTo(t, r.EffectiveVersionFor("test1").EmulationVersion(), "0.58")
	assertVersionEqualTo(t, r.EffectiveVersionFor("test2").EmulationVersion(), "1.28")
	assertVersionEqualTo(t, r.EffectiveVersionFor("test3").EmulationVersion(), "2.10")

	utilruntime.Must(r.SetEmulationVersionMapping("test2", "test3",
		func(from *version.Version) *version.Version {
			return version.MajorMinor(from.Major()+1, from.Minor()-19)
		}))
	utilruntime.Must(r.SetEmulationVersionMapping("test1", "test2",
		func(from *version.Version) *version.Version {
			return version.MajorMinor(from.Major()+1, from.Minor()-28)
		}))
	assertVersionEqualTo(t, r.EffectiveVersionFor("test1").EmulationVersion(), "0.58")
	assertVersionEqualTo(t, r.EffectiveVersionFor("test2").EmulationVersion(), "1.30")
	assertVersionEqualTo(t, r.EffectiveVersionFor("test3").EmulationVersion(), "2.11")

	fs := pflag.NewFlagSet("testflag", pflag.ContinueOnError)
	r.AddFlags(fs)

	if err := fs.Parse([]string{fmt.Sprintf("--emulated-version=%s", "test1=0.56")}); err != nil {
		t.Fatal(err)
		return
	}
	if err := r.Set(); err != nil {
		t.Fatal(err)
		return
	}
	assertVersionEqualTo(t, r.EffectiveVersionFor("test1").EmulationVersion(), "0.56")
	assertVersionEqualTo(t, r.EffectiveVersionFor("test2").EmulationVersion(), "1.28")
	assertVersionEqualTo(t, r.EffectiveVersionFor("test3").EmulationVersion(), "2.09")
}

func TestVersionMappingWithMultipleDependency(t *testing.T) {
	r := NewComponentGlobalsRegistry()
	ver1 := NewEffectiveVersionFromString("0.58", "", "")
	ver2 := NewEffectiveVersionFromString("1.28", "", "")
	ver3 := NewEffectiveVersionFromString("2.10", "", "")

	utilruntime.Must(r.Register("test1", ver1, nil))
	utilruntime.Must(r.Register("test2", ver2, nil))
	utilruntime.Must(r.Register("test3", ver3, nil))

	assertVersionEqualTo(t, r.EffectiveVersionFor("test1").EmulationVersion(), "0.58")
	assertVersionEqualTo(t, r.EffectiveVersionFor("test2").EmulationVersion(), "1.28")
	assertVersionEqualTo(t, r.EffectiveVersionFor("test3").EmulationVersion(), "2.10")

	utilruntime.Must(r.SetEmulationVersionMapping("test1", "test2",
		func(from *version.Version) *version.Version {
			return version.MajorMinor(from.Major()+1, from.Minor()-28)
		}))
	err := r.SetEmulationVersionMapping("test3", "test2",
		func(from *version.Version) *version.Version {
			return version.MajorMinor(from.Major()-1, from.Minor()+19)
		})
	if err == nil {
		t.Errorf("expect error when setting 2nd mapping to test2")
	}
}

func TestVersionMappingWithCyclicDependency(t *testing.T) {
	r := NewComponentGlobalsRegistry()
	ver1 := NewEffectiveVersionFromString("0.58", "", "")
	ver2 := NewEffectiveVersionFromString("1.28", "", "")
	ver3 := NewEffectiveVersionFromString("2.10", "", "")

	utilruntime.Must(r.Register("test1", ver1, nil))
	utilruntime.Must(r.Register("test2", ver2, nil))
	utilruntime.Must(r.Register("test3", ver3, nil))

	assertVersionEqualTo(t, r.EffectiveVersionFor("test1").EmulationVersion(), "0.58")
	assertVersionEqualTo(t, r.EffectiveVersionFor("test2").EmulationVersion(), "1.28")
	assertVersionEqualTo(t, r.EffectiveVersionFor("test3").EmulationVersion(), "2.10")

	utilruntime.Must(r.SetEmulationVersionMapping("test1", "test2",
		func(from *version.Version) *version.Version {
			return version.MajorMinor(from.Major()+1, from.Minor()-28)
		}))
	utilruntime.Must(r.SetEmulationVersionMapping("test2", "test3",
		func(from *version.Version) *version.Version {
			return version.MajorMinor(from.Major()+1, from.Minor()-19)
		}))
	err := r.SetEmulationVersionMapping("test3", "test1",
		func(from *version.Version) *version.Version {
			return version.MajorMinor(from.Major()-2, from.Minor()+48)
		})
	if err == nil {
		t.Errorf("expect cyclic version mapping error")
	}
}

func assertVersionEqualTo(t *testing.T, ver *version.Version, expectedVer string) {
	if ver.EqualTo(version.MustParse(expectedVer)) {
		return
	}
	t.Errorf("expected: %s, got %s", expectedVer, ver.String())
}
