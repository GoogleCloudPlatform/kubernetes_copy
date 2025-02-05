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

package featuregate

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/version"
	"k8s.io/component-base/metrics/legacyregistry"
	featuremetrics "k8s.io/component-base/metrics/prometheus/feature"
	"k8s.io/component-base/metrics/testutil"
)

func TestFeatureGateFlag(t *testing.T) {
	// gates for testing
	const testAlphaGate Feature = "TestAlpha"
	const testBetaGate Feature = "TestBeta"
	const testDeprecatedGate Feature = "TestDeprecated"
	const testLockedFalseGate Feature = "TestLockedFalse"

	tests := []struct {
		arg        string
		expect     map[Feature]bool
		parseError string
	}{
		{
			arg: "",
			expect: map[Feature]bool{
				allAlphaGate:        false,
				allBetaGate:         false,
				testAlphaGate:       false,
				testBetaGate:        false,
				testDeprecatedGate:  false,
				testLockedFalseGate: false,
			},
		},
		{
			arg: "TestDeprecated=true",
			expect: map[Feature]bool{
				allAlphaGate:        false,
				allBetaGate:         false,
				testAlphaGate:       false,
				testBetaGate:        false,
				testDeprecatedGate:  true,
				testLockedFalseGate: false,
			},
		},
		{
			arg: "TestLockedFalse=true",
			expect: map[Feature]bool{
				allAlphaGate:        false,
				allBetaGate:         false,
				testAlphaGate:       false,
				testBetaGate:        false,
				testLockedFalseGate: false,
			},
			parseError: "cannot set feature gate TestLockedFalse to true, feature is locked to false",
		},
		{
			arg: "fooBarBaz=true",
			expect: map[Feature]bool{
				allAlphaGate:        false,
				allBetaGate:         false,
				testAlphaGate:       false,
				testBetaGate:        false,
				testLockedFalseGate: false,
			},
			parseError: "unrecognized feature gate: fooBarBaz",
		},
		{
			arg: "AllAlpha=false",
			expect: map[Feature]bool{
				allAlphaGate:        false,
				allBetaGate:         false,
				testAlphaGate:       false,
				testBetaGate:        false,
				testLockedFalseGate: false,
			},
		},
		{
			arg: "AllAlpha=true",
			expect: map[Feature]bool{
				allAlphaGate:        true,
				allBetaGate:         false,
				testAlphaGate:       true,
				testBetaGate:        false,
				testLockedFalseGate: false,
			},
		},
		{
			arg: "AllAlpha=banana",
			expect: map[Feature]bool{
				allAlphaGate:        false,
				allBetaGate:         false,
				testAlphaGate:       false,
				testBetaGate:        false,
				testLockedFalseGate: false,
			},
			parseError: "invalid value of AllAlpha",
		},
		{
			arg: "AllAlpha=false,TestAlpha=true",
			expect: map[Feature]bool{
				allAlphaGate:        false,
				allBetaGate:         false,
				testAlphaGate:       true,
				testBetaGate:        false,
				testLockedFalseGate: false,
			},
		},
		{
			arg: "TestAlpha=true,AllAlpha=false",
			expect: map[Feature]bool{
				allAlphaGate:        false,
				allBetaGate:         false,
				testAlphaGate:       true,
				testBetaGate:        false,
				testLockedFalseGate: false,
			},
		},
		{
			arg: "AllAlpha=true,TestAlpha=false",
			expect: map[Feature]bool{
				allAlphaGate:        true,
				allBetaGate:         false,
				testAlphaGate:       false,
				testBetaGate:        false,
				testLockedFalseGate: false,
			},
		},
		{
			arg: "TestAlpha=false,AllAlpha=true",
			expect: map[Feature]bool{
				allAlphaGate:        true,
				allBetaGate:         false,
				testAlphaGate:       false,
				testBetaGate:        false,
				testLockedFalseGate: false,
			},
		},
		{
			arg: "TestBeta=true,AllAlpha=false",
			expect: map[Feature]bool{
				allAlphaGate:        false,
				allBetaGate:         false,
				testAlphaGate:       false,
				testBetaGate:        true,
				testLockedFalseGate: false,
			},
		},

		{
			arg: "AllBeta=false",
			expect: map[Feature]bool{
				allAlphaGate:  false,
				allBetaGate:   false,
				testAlphaGate: false,
				testBetaGate:  false,
			},
		},
		{
			arg: "AllBeta=true",
			expect: map[Feature]bool{
				allAlphaGate:        false,
				allBetaGate:         true,
				testAlphaGate:       false,
				testBetaGate:        true,
				testLockedFalseGate: false,
			},
		},
		{
			arg: "AllBeta=banana",
			expect: map[Feature]bool{
				allAlphaGate:        false,
				allBetaGate:         false,
				testAlphaGate:       false,
				testBetaGate:        false,
				testLockedFalseGate: false,
			},
			parseError: "invalid value of AllBeta",
		},
		{
			arg: "AllBeta=false,TestBeta=true",
			expect: map[Feature]bool{
				allAlphaGate:        false,
				allBetaGate:         false,
				testAlphaGate:       false,
				testBetaGate:        true,
				testLockedFalseGate: false,
			},
		},
		{
			arg: "TestBeta=true,AllBeta=false",
			expect: map[Feature]bool{
				allAlphaGate:        false,
				allBetaGate:         false,
				testAlphaGate:       false,
				testBetaGate:        true,
				testLockedFalseGate: false,
			},
		},
		{
			arg: "AllBeta=true,TestBeta=false",
			expect: map[Feature]bool{
				allAlphaGate:        false,
				allBetaGate:         true,
				testAlphaGate:       false,
				testBetaGate:        false,
				testLockedFalseGate: false,
			},
		},
		{
			arg: "TestBeta=false,AllBeta=true",
			expect: map[Feature]bool{
				allAlphaGate:        false,
				allBetaGate:         true,
				testAlphaGate:       false,
				testBetaGate:        false,
				testLockedFalseGate: false,
			},
		},
		{
			arg: "TestAlpha=true,AllBeta=false",
			expect: map[Feature]bool{
				allAlphaGate:        false,
				allBetaGate:         false,
				testAlphaGate:       true,
				testBetaGate:        false,
				testLockedFalseGate: false,
			},
		},
	}
	for i, test := range tests {
		t.Run(test.arg, func(t *testing.T) {
			fs := pflag.NewFlagSet("testfeaturegateflag", pflag.ContinueOnError)
			f := NewFeatureGate()
			err := f.Add(map[Feature]FeatureSpec{
				testAlphaGate:       {Default: false, PreRelease: Alpha},
				testBetaGate:        {Default: false, PreRelease: Beta},
				testDeprecatedGate:  {Default: false, PreRelease: Deprecated},
				testLockedFalseGate: {Default: false, PreRelease: GA, LockToDefault: true},
			})
			require.NoError(t, err)
			f.AddFlag(fs)
			err = fs.Parse([]string{fmt.Sprintf("--%s=%s", flagName, test.arg)})
			if test.parseError != "" {
				if !strings.Contains(err.Error(), test.parseError) {
					t.Errorf("%d: Parse() Expected %v, Got %v", i, test.parseError, err)
				}
			} else if err != nil {
				t.Errorf("%d: Parse() Expected nil, Got %v", i, err)
			}
			for k, v := range test.expect {
				if actual := f.enabled.Load().(map[Feature]bool)[k]; actual != v {
					t.Errorf("%d: expected %s=%v, Got %v", i, k, v, actual)
				}
			}
		})
	}
}

func TestFeatureGateOverride(t *testing.T) {
	const testAlphaGate Feature = "TestAlpha"
	const testBetaGate Feature = "TestBeta"

	// Don't parse the flag, assert defaults are used.
	var f *featureGate = NewFeatureGate()
	err := f.Add(map[Feature]FeatureSpec{
		testAlphaGate: {Default: false, PreRelease: Alpha},
		testBetaGate:  {Default: false, PreRelease: Beta},
	})
	require.NoError(t, err)

	f.Set("TestAlpha=true,TestBeta=true")
	if errs := f.Validate(); len(errs) > 0 {
		t.Fatalf("Validate() Expected no error, Got %v", errs)
	}
	if f.Enabled(testAlphaGate) != true {
		t.Errorf("Expected true")
	}
	if f.Enabled(testBetaGate) != true {
		t.Errorf("Expected true")
	}

	f.Set("TestAlpha=false")
	if errs := f.Validate(); len(errs) > 0 {
		t.Fatalf("Validate() Expected no error, Got %v", errs)
	}
	if f.Enabled(testAlphaGate) != false {
		t.Errorf("Expected false")
	}
	if f.Enabled(testBetaGate) != true {
		t.Errorf("Expected true")
	}
}

func TestFeatureGateFlagDefaults(t *testing.T) {
	// gates for testing
	const testAlphaGate Feature = "TestAlpha"
	const testBetaGate Feature = "TestBeta"

	// Don't parse the flag, assert defaults are used.
	var f *featureGate = NewFeatureGate()
	err := f.Add(map[Feature]FeatureSpec{
		testAlphaGate: {Default: false, PreRelease: Alpha},
		testBetaGate:  {Default: true, PreRelease: Beta},
	})
	require.NoError(t, err)

	if f.Enabled(testAlphaGate) != false {
		t.Errorf("Expected false")
	}
	if f.Enabled(testBetaGate) != true {
		t.Errorf("Expected true")
	}
}

func TestFeatureGateKnownFeatures(t *testing.T) {
	// gates for testing
	const (
		testAlphaGate      Feature = "TestAlpha"
		testBetaGate       Feature = "TestBeta"
		testGAGate         Feature = "TestGA"
		testDeprecatedGate Feature = "TestDeprecated"
	)

	// Don't parse the flag, assert defaults are used.
	var f *featureGate = NewFeatureGate()
	err := f.Add(map[Feature]FeatureSpec{
		testAlphaGate:      {Default: false, PreRelease: Alpha},
		testBetaGate:       {Default: true, PreRelease: Beta},
		testGAGate:         {Default: true, PreRelease: GA},
		testDeprecatedGate: {Default: false, PreRelease: Deprecated},
	})
	require.NoError(t, err)
	known := strings.Join(f.KnownFeatures(), " ")

	assert.Contains(t, known, testAlphaGate)
	assert.Contains(t, known, testBetaGate)
	assert.NotContains(t, known, testGAGate)
	assert.NotContains(t, known, testDeprecatedGate)
}

func TestFeatureGateSetFromMap(t *testing.T) {
	// gates for testing
	const testAlphaGate Feature = "TestAlpha"
	const testBetaGate Feature = "TestBeta"
	const testLockedTrueGate Feature = "TestLockedTrue"
	const testLockedFalseGate Feature = "TestLockedFalse"

	tests := []struct {
		name        string
		setmap      map[string]bool
		expect      map[Feature]bool
		setmapError string
	}{
		{
			name: "set TestAlpha and TestBeta true",
			setmap: map[string]bool{
				"TestAlpha": true,
				"TestBeta":  true,
			},
			expect: map[Feature]bool{
				testAlphaGate: true,
				testBetaGate:  true,
			},
		},
		{
			name: "set TestBeta true",
			setmap: map[string]bool{
				"TestBeta": true,
			},
			expect: map[Feature]bool{
				testAlphaGate: false,
				testBetaGate:  true,
			},
		},
		{
			name: "set TestAlpha false",
			setmap: map[string]bool{
				"TestAlpha": false,
			},
			expect: map[Feature]bool{
				testAlphaGate: false,
				testBetaGate:  false,
			},
		},
		{
			name: "set TestInvaild true",
			setmap: map[string]bool{
				"TestInvaild": true,
			},
			expect: map[Feature]bool{
				testAlphaGate: false,
				testBetaGate:  false,
			},
			setmapError: "unrecognized feature gate:",
		},
		{
			name: "set locked gates",
			setmap: map[string]bool{
				"TestLockedTrue":  true,
				"TestLockedFalse": false,
			},
			expect: map[Feature]bool{
				testAlphaGate: false,
				testBetaGate:  false,
			},
		},
		{
			name: "set locked gates",
			setmap: map[string]bool{
				"TestLockedTrue": false,
			},
			expect: map[Feature]bool{
				testAlphaGate: false,
				testBetaGate:  false,
			},
			setmapError: "cannot set feature gate TestLockedTrue to false, feature is locked to true",
		},
		{
			name: "set locked gates",
			setmap: map[string]bool{
				"TestLockedFalse": true,
			},
			expect: map[Feature]bool{
				testAlphaGate: false,
				testBetaGate:  false,
			},
			setmapError: "cannot set feature gate TestLockedFalse to true, feature is locked to false",
		},
	}
	for i, test := range tests {
		t.Run(fmt.Sprintf("SetFromMap %s", test.name), func(t *testing.T) {
			f := NewFeatureGate()
			err := f.Add(map[Feature]FeatureSpec{
				testAlphaGate:       {Default: false, PreRelease: Alpha},
				testBetaGate:        {Default: false, PreRelease: Beta},
				testLockedTrueGate:  {Default: true, PreRelease: GA, LockToDefault: true},
				testLockedFalseGate: {Default: false, PreRelease: GA, LockToDefault: true},
			})
			require.NoError(t, err)
			err = f.SetFromMap(test.setmap)
			if test.setmapError != "" {
				if err == nil {
					t.Errorf("expected error, got none")
				} else if !strings.Contains(err.Error(), test.setmapError) {
					t.Errorf("%d: SetFromMap(%#v) Expected err:%v, Got err:%v", i, test.setmap, test.setmapError, err)
				}
			} else if err != nil {
				t.Errorf("%d: SetFromMap(%#v) Expected success, Got err:%v", i, test.setmap, err)
			}
			for k, v := range test.expect {
				if actual := f.Enabled(k); actual != v {
					t.Errorf("%d: SetFromMap(%#v) Expected %s=%v, Got %s=%v", i, test.setmap, k, v, k, actual)
				}
			}
		})
	}
}

func TestFeatureGateMetrics(t *testing.T) {
	// gates for testing
	featuremetrics.ResetFeatureInfoMetric()
	const testAlphaGate Feature = "TestAlpha"
	const testBetaGate Feature = "TestBeta"
	const testAlphaEnabled Feature = "TestAlphaEnabled"
	const testBetaDisabled Feature = "TestBetaDisabled"
	testedMetrics := []string{"kubernetes_feature_enabled"}
	expectedOutput := `
		# HELP kubernetes_feature_enabled [BETA] This metric records the data about the stage and enablement of a k8s feature.
        # TYPE kubernetes_feature_enabled gauge
        kubernetes_feature_enabled{name="TestAlpha",stage="ALPHA"} 0
        kubernetes_feature_enabled{name="TestBeta",stage="BETA"} 1
		kubernetes_feature_enabled{name="TestAlphaEnabled",stage="ALPHA"} 1
        kubernetes_feature_enabled{name="AllAlpha",stage="ALPHA"} 0
        kubernetes_feature_enabled{name="AllBeta",stage="BETA"} 0
		kubernetes_feature_enabled{name="TestBetaDisabled",stage="ALPHA"} 0
`

	f := NewFeatureGate()
	fMap := map[Feature]FeatureSpec{
		testAlphaGate:    {Default: false, PreRelease: Alpha},
		testAlphaEnabled: {Default: false, PreRelease: Alpha},
		testBetaGate:     {Default: true, PreRelease: Beta},
		testBetaDisabled: {Default: true, PreRelease: Alpha},
	}
	require.NoError(t, f.Add(fMap))
	require.NoError(t, f.SetFromMap(map[string]bool{"TestAlphaEnabled": true, "TestBetaDisabled": false}))
	f.AddMetrics()
	if err := testutil.GatherAndCompare(legacyregistry.DefaultGatherer, strings.NewReader(expectedOutput), testedMetrics...); err != nil {
		t.Fatal(err)
	}
}

func TestFeatureGateString(t *testing.T) {
	// gates for testing
	const testAlphaGate Feature = "TestAlpha"
	const testBetaGate Feature = "TestBeta"
	const testGAGate Feature = "TestGA"

	featuremap := map[Feature]FeatureSpec{
		testGAGate:    {Default: true, PreRelease: GA},
		testAlphaGate: {Default: false, PreRelease: Alpha},
		testBetaGate:  {Default: true, PreRelease: Beta},
	}

	tests := []struct {
		setmap map[string]bool
		expect string
	}{
		{
			setmap: map[string]bool{
				"TestAlpha": false,
			},
			expect: "TestAlpha=false",
		},
		{
			setmap: map[string]bool{
				"TestAlpha": false,
				"TestBeta":  true,
			},
			expect: "TestAlpha=false,TestBeta=true",
		},
		{
			setmap: map[string]bool{
				"TestGA":    true,
				"TestAlpha": false,
				"TestBeta":  true,
			},
			expect: "TestAlpha=false,TestBeta=true,TestGA=true",
		},
	}
	for i, test := range tests {
		t.Run(fmt.Sprintf("SetFromMap %s", test.expect), func(t *testing.T) {
			f := NewFeatureGate()
			require.NoError(t, f.Add(featuremap))
			require.NoError(t, f.SetFromMap(test.setmap))
			result := f.String()
			if result != test.expect {
				t.Errorf("%d: SetFromMap(%#v) Expected %s, Got %s", i, test.setmap, test.expect, result)
			}
		})
	}
}

func TestFeatureGateOverrideDefault(t *testing.T) {
	t.Run("overrides take effect", func(t *testing.T) {
		f := NewFeatureGate()
		if err := f.Add(map[Feature]FeatureSpec{
			"TestFeature1": {Default: true},
			"TestFeature2": {Default: false},
		}); err != nil {
			t.Fatal(err)
		}
		require.NoError(t, f.OverrideDefault("TestFeature1", false))
		require.NoError(t, f.OverrideDefault("TestFeature2", true))
		if f.Enabled("TestFeature1") {
			t.Error("expected TestFeature1 to have effective default of false")
		}
		if !f.Enabled("TestFeature2") {
			t.Error("expected TestFeature2 to have effective default of true")
		}
	})

	t.Run("overrides are preserved across deep copies", func(t *testing.T) {
		f := NewFeatureGate()
		require.NoError(t, f.Add(map[Feature]FeatureSpec{"TestFeature": {Default: false}}))
		require.NoError(t, f.OverrideDefault("TestFeature", true))
		fcopy := f.DeepCopy()
		if !fcopy.Enabled("TestFeature") {
			t.Error("default override was not preserved by deep copy")
		}
	})

	t.Run("overrides are preserved across CopyKnownFeatures", func(t *testing.T) {
		f := NewFeatureGate()
		require.NoError(t, f.Add(map[Feature]FeatureSpec{"TestFeature": {Default: false}}))
		require.NoError(t, f.OverrideDefault("TestFeature", true))
		fcopy := f.DeepCopyAndReset()
		if !f.Enabled("TestFeature") {
			t.Error("TestFeature should be enabled by override")
		}
		if !fcopy.Enabled("TestFeature") {
			t.Error("default override was not preserved by CopyKnownFeatures")
		}
	})

	t.Run("overrides are not passed over after CopyKnownFeatures", func(t *testing.T) {
		f := NewFeatureGate()
		require.NoError(t, f.Add(map[Feature]FeatureSpec{"TestFeature": {Default: false}}))
		fcopy := f.DeepCopyAndReset()
		require.NoError(t, f.OverrideDefault("TestFeature", true))
		if !f.Enabled("TestFeature") {
			t.Error("TestFeature should be enabled by override")
		}
		if fcopy.Enabled("TestFeature") {
			t.Error("default override should not be passed over after CopyKnownFeatures")
		}
	})

	t.Run("reflected in known features", func(t *testing.T) {
		f := NewFeatureGate()
		if err := f.Add(map[Feature]FeatureSpec{"TestFeature": {
			Default:    false,
			PreRelease: Alpha,
		}}); err != nil {
			t.Fatal(err)
		}
		require.NoError(t, f.OverrideDefault("TestFeature", true))
		var found bool
		for _, s := range f.KnownFeatures() {
			if !strings.Contains(s, "TestFeature") {
				continue
			}
			found = true
			if !strings.Contains(s, "default=true") {
				t.Errorf("expected override of default to be reflected in known feature description %q", s)
			}
		}
		if !found {
			t.Error("found no entry for TestFeature in known features")
		}
	})

	t.Run("may not change default for specs with locked defaults", func(t *testing.T) {
		f := NewFeatureGate()
		if err := f.Add(map[Feature]FeatureSpec{
			"LockedFeature": {
				Default:       true,
				LockToDefault: true,
			},
		}); err != nil {
			t.Fatal(err)
		}
		if f.OverrideDefault("LockedFeature", false) == nil {
			t.Error("expected error when attempting to override the default for a feature with a locked default")
		}
		if f.OverrideDefault("LockedFeature", true) == nil {
			t.Error("expected error when attempting to override the default for a feature with a locked default")
		}
	})

	t.Run("does not supersede explicitly-set value", func(t *testing.T) {
		f := NewFeatureGate()
		require.NoError(t, f.Add(map[Feature]FeatureSpec{"TestFeature": {Default: true}}))
		require.NoError(t, f.OverrideDefault("TestFeature", false))
		require.NoError(t, f.SetFromMap(map[string]bool{"TestFeature": true}))
		if !f.Enabled("TestFeature") {
			t.Error("expected feature to be effectively enabled despite default override")
		}
	})

	t.Run("prevents re-registration of feature spec after overriding default", func(t *testing.T) {
		f := NewFeatureGate()
		if err := f.Add(map[Feature]FeatureSpec{
			"TestFeature": {
				Default:    true,
				PreRelease: Alpha,
			},
		}); err != nil {
			t.Fatal(err)
		}
		require.NoError(t, f.OverrideDefault("TestFeature", false))
		if err := f.Add(map[Feature]FeatureSpec{
			"TestFeature": {
				Default:    true,
				PreRelease: Alpha,
			},
		}); err == nil {
			t.Error("expected re-registration to return a non-nil error after overriding its default")
		}
	})

	t.Run("does not allow override for an unknown feature", func(t *testing.T) {
		f := NewFeatureGate()
		if err := f.OverrideDefault("TestFeature", true); err == nil {
			t.Error("expected an error to be returned in attempt to override default for unregistered feature")
		}
	})

	t.Run("returns error if already added to flag set", func(t *testing.T) {
		f := NewFeatureGate()
		fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
		f.AddFlag(fs)

		if err := f.OverrideDefault("TestFeature", true); err == nil {
			t.Error("expected a non-nil error to be returned")
		}
	})
}

func TestVersionedFeatureGateFlag(t *testing.T) {
	// gates for testing
	const testGAGate Feature = "TestGA"
	const testAlphaGate Feature = "TestAlpha"
	const testBetaGate Feature = "TestBeta"
	const testLockedFalseGate Feature = "TestLockedFalse"
	const testAlphaGateNoVersion Feature = "TestAlphaNoVersion"
	const testBetaGateNoVersion Feature = "TestBetaNoVersion"

	tests := []struct {
		arg        string
		expect     map[Feature]bool
		parseError string
	}{
		{
			arg: "",
			expect: map[Feature]bool{
				testGAGate:             false,
				allAlphaGate:           false,
				allBetaGate:            false,
				testAlphaGate:          false,
				testBetaGate:           false,
				testLockedFalseGate:    false,
				testAlphaGateNoVersion: false,
				testBetaGateNoVersion:  false,
			},
		},
		{
			arg: "TestLockedFalse=true",
			expect: map[Feature]bool{
				allAlphaGate:           false,
				allBetaGate:            false,
				testAlphaGate:          false,
				testBetaGate:           false,
				testLockedFalseGate:    true,
				testAlphaGateNoVersion: false,
				testBetaGateNoVersion:  false,
			},
		},
		{
			arg: "fooBarBaz=true",
			expect: map[Feature]bool{
				allAlphaGate:           false,
				allBetaGate:            false,
				testGAGate:             false,
				testAlphaGate:          false,
				testBetaGate:           false,
				testLockedFalseGate:    false,
				testAlphaGateNoVersion: false,
				testBetaGateNoVersion:  false,
			},
			parseError: "unrecognized feature gate: fooBarBaz",
		},
		{
			arg: "AllAlpha=false",
			expect: map[Feature]bool{
				allAlphaGate:           false,
				allBetaGate:            false,
				testGAGate:             false,
				testAlphaGate:          false,
				testBetaGate:           false,
				testLockedFalseGate:    false,
				testAlphaGateNoVersion: false,
				testBetaGateNoVersion:  false,
			},
		},
		{
			arg: "AllAlpha=true",
			expect: map[Feature]bool{
				allAlphaGate:           true,
				allBetaGate:            false,
				testAlphaGate:          false,
				testGAGate:             false,
				testBetaGate:           true,
				testLockedFalseGate:    false,
				testAlphaGateNoVersion: true,
				testBetaGateNoVersion:  false,
			},
		},
		{
			arg: "AllAlpha=banana",
			expect: map[Feature]bool{
				allAlphaGate:           false,
				allBetaGate:            false,
				testGAGate:             false,
				testAlphaGate:          false,
				testBetaGate:           false,
				testLockedFalseGate:    false,
				testAlphaGateNoVersion: false,
				testBetaGateNoVersion:  false,
			},
			parseError: "invalid value of AllAlpha",
		},
		{
			arg: "AllAlpha=false,TestAlpha=true,TestAlphaNoVersion=true",
			expect: map[Feature]bool{
				allAlphaGate:           false,
				allBetaGate:            false,
				testGAGate:             false,
				testAlphaGate:          false,
				testBetaGate:           false,
				testLockedFalseGate:    false,
				testAlphaGateNoVersion: true,
				testBetaGateNoVersion:  false,
			},
			parseError: "cannot set feature gate TestAlpha to true, feature is PreAlpha at emulated version 1.28",
		},
		{
			arg: "AllAlpha=false,TestAlphaNoVersion=true",
			expect: map[Feature]bool{
				allAlphaGate:           false,
				allBetaGate:            false,
				testGAGate:             false,
				testAlphaGate:          false,
				testBetaGate:           false,
				testLockedFalseGate:    false,
				testAlphaGateNoVersion: true,
				testBetaGateNoVersion:  false,
			},
		},
		{
			arg: "TestAlpha=true,TestAlphaNoVersion=true,AllAlpha=false",
			expect: map[Feature]bool{
				allAlphaGate:           false,
				allBetaGate:            false,
				testGAGate:             false,
				testAlphaGate:          false,
				testBetaGate:           false,
				testLockedFalseGate:    false,
				testAlphaGateNoVersion: true,
				testBetaGateNoVersion:  false,
			},
			parseError: "cannot set feature gate TestAlpha to true, feature is PreAlpha at emulated version 1.28",
		},
		{
			arg: "AllAlpha=true,TestAlpha=false,TestAlphaNoVersion=false",
			expect: map[Feature]bool{
				allAlphaGate:           true,
				allBetaGate:            false,
				testGAGate:             false,
				testAlphaGate:          false,
				testBetaGate:           true,
				testLockedFalseGate:    false,
				testAlphaGateNoVersion: false,
				testBetaGateNoVersion:  false,
			},
			parseError: "cannot set feature gate TestAlpha to false, feature is PreAlpha at emulated version 1.28",
		},
		{
			arg: "AllAlpha=true,TestAlphaNoVersion=false",
			expect: map[Feature]bool{
				allAlphaGate:           true,
				allBetaGate:            false,
				testGAGate:             false,
				testAlphaGate:          false,
				testBetaGate:           true,
				testLockedFalseGate:    false,
				testAlphaGateNoVersion: false,
				testBetaGateNoVersion:  false,
			},
		},
		{
			arg: "TestAlpha=false,TestAlphaNoVersion=false,AllAlpha=true",
			expect: map[Feature]bool{
				allAlphaGate:           true,
				allBetaGate:            false,
				testGAGate:             false,
				testAlphaGate:          false,
				testBetaGate:           true,
				testLockedFalseGate:    false,
				testAlphaGateNoVersion: false,
				testBetaGateNoVersion:  false,
			},
			parseError: "cannot set feature gate TestAlpha to false, feature is PreAlpha at emulated version 1.28",
		},
		{
			arg: "TestBeta=true,TestBetaNoVersion=true,TestGA=true,AllAlpha=false",
			expect: map[Feature]bool{
				allAlphaGate:           false,
				allBetaGate:            false,
				testGAGate:             true,
				testAlphaGate:          false,
				testBetaGate:           true,
				testLockedFalseGate:    false,
				testAlphaGateNoVersion: false,
				testBetaGateNoVersion:  true,
			},
		},

		{
			arg: "AllBeta=false",
			expect: map[Feature]bool{
				allAlphaGate:           false,
				allBetaGate:            false,
				testGAGate:             false,
				testAlphaGate:          false,
				testBetaGate:           false,
				testLockedFalseGate:    false,
				testAlphaGateNoVersion: false,
				testBetaGateNoVersion:  false,
			},
		},
		{
			arg: "AllBeta=true",
			expect: map[Feature]bool{
				allAlphaGate:           false,
				allBetaGate:            true,
				testGAGate:             true,
				testAlphaGate:          false,
				testBetaGate:           false,
				testLockedFalseGate:    false,
				testAlphaGateNoVersion: false,
				testBetaGateNoVersion:  true,
			},
		},
		{
			arg: "AllBeta=banana",
			expect: map[Feature]bool{
				allAlphaGate:           false,
				allBetaGate:            false,
				testGAGate:             false,
				testAlphaGate:          false,
				testBetaGate:           false,
				testAlphaGateNoVersion: false,
				testBetaGateNoVersion:  false,
			},
			parseError: "invalid value of AllBeta",
		},
		{
			arg: "AllBeta=false,TestBeta=true,TestBetaNoVersion=true,TestGA=true",
			expect: map[Feature]bool{
				allAlphaGate:           false,
				allBetaGate:            false,
				testGAGate:             true,
				testAlphaGate:          false,
				testBetaGate:           true,
				testLockedFalseGate:    false,
				testAlphaGateNoVersion: false,
				testBetaGateNoVersion:  true,
			},
		},
		{
			arg: "TestBeta=true,TestBetaNoVersion=true,AllBeta=false",
			expect: map[Feature]bool{
				allAlphaGate:           false,
				allBetaGate:            false,
				testGAGate:             false,
				testAlphaGate:          false,
				testBetaGate:           true,
				testLockedFalseGate:    false,
				testAlphaGateNoVersion: false,
				testBetaGateNoVersion:  true,
			},
		},
		{
			arg: "AllBeta=true,TestBetaNoVersion=false,TestBeta=false,TestGA=false",
			expect: map[Feature]bool{
				allAlphaGate:           false,
				allBetaGate:            true,
				testGAGate:             false,
				testAlphaGate:          false,
				testBetaGate:           false,
				testLockedFalseGate:    false,
				testAlphaGateNoVersion: false,
				testBetaGateNoVersion:  false,
			},
		},
		{
			arg: "TestBeta=false,TestBetaNoVersion=false,AllBeta=true",
			expect: map[Feature]bool{
				allAlphaGate:           false,
				allBetaGate:            true,
				testGAGate:             true,
				testAlphaGate:          false,
				testBetaGate:           false,
				testLockedFalseGate:    false,
				testAlphaGateNoVersion: false,
				testBetaGateNoVersion:  false,
			},
		},
		{
			arg: "TestAlpha=true,AllBeta=false",
			expect: map[Feature]bool{
				allAlphaGate:           false,
				allBetaGate:            false,
				testGAGate:             false,
				testAlphaGate:          true,
				testBetaGate:           false,
				testLockedFalseGate:    false,
				testAlphaGateNoVersion: false,
				testBetaGateNoVersion:  false,
			},
			parseError: "cannot set feature gate TestAlpha to true, feature is PreAlpha at emulated version 1.28",
		},
	}
	for i, test := range tests {
		t.Run(test.arg, func(t *testing.T) {
			fs := pflag.NewFlagSet("testfeaturegateflag", pflag.ContinueOnError)
			f := NewVersionedFeatureGate(version.MustParse("1.29"))
			if err := f.SetEmulationVersion(version.MustParse("1.28")); err != nil {
				t.Fatalf("failed to SetEmulationVersion: %v", err)
			}
			err := f.AddVersioned(map[Feature]VersionedSpecs{
				testGAGate: {
					{Version: version.MustParse("1.27"), Default: false, PreRelease: Alpha},
					{Version: version.MustParse("1.28"), Default: false, PreRelease: Beta},
					{Version: version.MustParse("1.29"), Default: true, PreRelease: GA},
				},
				testAlphaGate: {
					{Version: version.MustParse("1.29"), Default: false, PreRelease: Alpha},
				},
				testBetaGate: {
					{Version: version.MustParse("1.28"), Default: false, PreRelease: Alpha},
					{Version: version.MustParse("1.29"), Default: false, PreRelease: Beta},
				},
				testLockedFalseGate: {
					{Version: version.MustParse("1.28"), Default: false, PreRelease: GA},
					{Version: version.MustParse("1.29"), Default: false, PreRelease: GA, LockToDefault: true},
				},
			})
			require.NoError(t, err)
			err = f.Add(map[Feature]FeatureSpec{
				testAlphaGateNoVersion: {Default: false, PreRelease: Alpha},
				testBetaGateNoVersion:  {Default: false, PreRelease: Beta},
			})
			require.NoError(t, err)
			f.AddFlag(fs)

			var errs []error
			err = fs.Parse([]string{fmt.Sprintf("--%s=%s", flagName, test.arg)})
			if err != nil {
				errs = append(errs, err)
			}
			err = utilerrors.NewAggregate(errs)
			if test.parseError != "" {
				if !strings.Contains(err.Error(), test.parseError) {
					t.Errorf("%d: Parse() Expected %v, Got %v", i, test.parseError, err)
				}
				return
			} else if err != nil {
				t.Errorf("%d: Parse() Expected nil, Got %v", i, err)
			}
			for k, v := range test.expect {
				if actual := f.enabled.Load().(map[Feature]bool)[k]; actual != v {
					t.Errorf("%d: expected %s=%v, Got %v", i, k, v, actual)
				}
			}
		})
	}
}

func TestVersionedFeatureGateOverride(t *testing.T) {
	const testAlphaGate Feature = "TestAlpha"
	const testBetaGate Feature = "TestBeta"

	// Don't parse the flag, assert defaults are used.
	f := NewVersionedFeatureGate(version.MustParse("1.29"))

	err := f.AddVersioned(map[Feature]VersionedSpecs{
		testAlphaGate: {
			{Version: version.MustParse("1.29"), Default: false, PreRelease: Alpha},
		},
		testBetaGate: {
			{Version: version.MustParse("1.28"), Default: false, PreRelease: Alpha},
			{Version: version.MustParse("1.29"), Default: false, PreRelease: Beta},
		},
	})
	require.NoError(t, err)
	if f.Enabled(testAlphaGate) != false {
		t.Errorf("Expected false")
	}
	if f.Enabled(testBetaGate) != false {
		t.Errorf("Expected false")
	}
	if errs := f.Validate(); len(errs) > 0 {
		t.Errorf("Expected no errors when emulation version is equal to binary version.")
	}

	require.NoError(t, f.Set("TestAlpha=true,TestBeta=true"))
	if f.Enabled(testAlphaGate) != true {
		t.Errorf("Expected false")
	}
	if f.Enabled(testBetaGate) != true {
		t.Errorf("Expected true")
	}

	require.NoError(t, f.Set("TestAlpha=false"))
	if f.Enabled(testAlphaGate) != false {
		t.Errorf("Expected false")
	}
	if f.Enabled(testBetaGate) != true {
		t.Errorf("Expected true")
	}
	if errs := f.Validate(); len(errs) > 0 {
		t.Errorf("Expected no errors when emulation version is equal to binary version.")
	}

	if err := f.SetEmulationVersion(version.MustParse("1.28")); err == nil {
		t.Errorf("Expected errors when emulation version is 1.28.")
	}
}

func TestVersionedFeatureGateFlagDefaults(t *testing.T) {
	// gates for testing
	const testGAGate Feature = "TestGA"
	const testAlphaGate Feature = "TestAlpha"
	const testBetaGate Feature = "TestBeta"

	// Don't parse the flag, assert defaults are used.
	f := NewVersionedFeatureGate(version.MustParse("1.29"))
	require.NoError(t, f.SetEmulationVersion(version.MustParse("1.28")))

	err := f.AddVersioned(map[Feature]VersionedSpecs{
		testGAGate: {
			{Version: version.MustParse("1.25"), Default: true, PreRelease: Alpha},
			{Version: version.MustParse("1.27"), Default: true, PreRelease: Beta},
			{Version: version.MustParse("1.29"), Default: true, PreRelease: GA},
		},
		testAlphaGate: {
			{Version: version.MustParse("1.29"), Default: false, PreRelease: Alpha},
		},
		testBetaGate: {
			{Version: version.MustParse("1.26"), Default: false, PreRelease: Alpha},
			{Version: version.MustParse("1.28"), Default: false, PreRelease: Beta},
			{Version: version.MustParse("1.29"), Default: true, PreRelease: Beta},
		},
	})
	require.NoError(t, err)

	if f.Enabled(testAlphaGate) != false {
		t.Errorf("Expected false")
	}
	if fs, _ := f.featureSpec(testAlphaGate); fs.PreRelease != PreAlpha || fs.Version.String() != "0.0" {
		t.Errorf("Expected (PreAlpha, 0.0)")
	}
	if f.Enabled(testBetaGate) != false {
		t.Errorf("Expected false")
	}
	if fs, _ := f.featureSpec(testBetaGate); fs.PreRelease != Beta || fs.Version.String() != "1.28" {
		t.Errorf("Expected (Beta, 1.28)")
	}
	if f.Enabled(testGAGate) != true {
		t.Errorf("Expected true")
	}
	if fs, _ := f.featureSpec(testGAGate); fs.PreRelease != Beta || fs.Version.String() != "1.27" {
		t.Errorf("Expected (Beta, 1.27)")
	}
	if _, err := f.featureSpec("NonExist"); err == nil {
		t.Errorf("Expected Error")
	}
	allFeatures := f.GetAll()
	expectedAllFeatures := []Feature{testGAGate, testBetaGate, allAlphaGate, allBetaGate}
	if len(allFeatures) != 4 {
		t.Errorf("Expected 4 features from GetAll(), got %d", len(allFeatures))
	}
	for _, feature := range expectedAllFeatures {
		if _, ok := allFeatures[feature]; !ok {
			t.Errorf("Expected feature %s to be in GetAll()", feature)
		}
	}
}

func TestVersionedFeatureGateKnownFeatures(t *testing.T) {
	// gates for testing
	const (
		testPreAlphaGate            Feature = "TestPreAlpha"
		testAlphaGate               Feature = "TestAlpha"
		testBetaGate                Feature = "TestBeta"
		testGAGate                  Feature = "TestGA"
		testDeprecatedGate          Feature = "TestDeprecated"
		testGAGateNoVersion         Feature = "TestGANoVersion"
		testAlphaGateNoVersion      Feature = "TestAlphaNoVersion"
		testBetaGateNoVersion       Feature = "TestBetaNoVersion"
		testDeprecatedGateNoVersion Feature = "TestDeprecatedNoVersion"
	)

	// Don't parse the flag, assert defaults are used.
	f := NewVersionedFeatureGate(version.MustParse("1.29"))
	require.NoError(t, f.SetEmulationVersion(version.MustParse("1.28")))
	err := f.AddVersioned(map[Feature]VersionedSpecs{
		testGAGate: {
			{Version: version.MustParse("1.27"), Default: false, PreRelease: Beta},
			{Version: version.MustParse("1.28"), Default: true, PreRelease: GA},
		},
		testPreAlphaGate: {
			{Version: version.MustParse("1.29"), Default: false, PreRelease: Alpha},
		},
		testAlphaGate: {
			{Version: version.MustParse("1.28"), Default: false, PreRelease: Alpha},
		},
		testBetaGate: {
			{Version: version.MustParse("1.28"), Default: false, PreRelease: Beta},
		},
		testDeprecatedGate: {
			{Version: version.MustParse("1.26"), Default: false, PreRelease: Alpha},
			{Version: version.MustParse("1.28"), Default: true, PreRelease: Deprecated},
		},
	})
	require.NoError(t, err)
	err = f.Add(map[Feature]FeatureSpec{
		testAlphaGateNoVersion:      {Default: false, PreRelease: Alpha},
		testBetaGateNoVersion:       {Default: false, PreRelease: Beta},
		testGAGateNoVersion:         {Default: false, PreRelease: GA},
		testDeprecatedGateNoVersion: {Default: false, PreRelease: Deprecated},
	})
	require.NoError(t, err)

	known := strings.Join(f.KnownFeatures(), " ")

	assert.NotContains(t, known, testPreAlphaGate)
	assert.Contains(t, known, testAlphaGate)
	assert.Contains(t, known, testBetaGate)
	assert.NotContains(t, known, testGAGate)
	assert.NotContains(t, known, testDeprecatedGate)
	assert.Contains(t, known, testAlphaGateNoVersion)
	assert.Contains(t, known, testBetaGateNoVersion)
	assert.NotContains(t, known, testGAGateNoVersion)
	assert.NotContains(t, known, testDeprecatedGateNoVersion)
}

func TestVersionedFeatureGateMetrics(t *testing.T) {
	// gates for testing
	featuremetrics.ResetFeatureInfoMetric()
	const testAlphaGate Feature = "TestAlpha"
	const testBetaGate Feature = "TestBeta"
	const testAlphaEnabled Feature = "TestAlphaEnabled"
	const testBetaDisabled Feature = "TestBetaDisabled"
	testedMetrics := []string{"kubernetes_feature_enabled"}
	expectedOutput := `
		# HELP kubernetes_feature_enabled [BETA] This metric records the data about the stage and enablement of a k8s feature.
        # TYPE kubernetes_feature_enabled gauge
        kubernetes_feature_enabled{name="TestAlpha",stage="ALPHA"} 0
        kubernetes_feature_enabled{name="TestBeta",stage="BETA"} 1
		kubernetes_feature_enabled{name="TestAlphaEnabled",stage="ALPHA"} 1
        kubernetes_feature_enabled{name="AllAlpha",stage="ALPHA"} 0
        kubernetes_feature_enabled{name="AllBeta",stage="BETA"} 0
		kubernetes_feature_enabled{name="TestBetaDisabled",stage="BETA"} 0
`

	f := NewVersionedFeatureGate(version.MustParse("1.29"))
	require.NoError(t, f.SetEmulationVersion(version.MustParse("1.28")))
	err := f.AddVersioned(map[Feature]VersionedSpecs{
		testAlphaGate: {
			{Version: version.MustParse("1.28"), Default: false, PreRelease: Alpha},
			{Version: version.MustParse("1.29"), Default: true, PreRelease: Beta},
		},
		testAlphaEnabled: {
			{Version: version.MustParse("1.28"), Default: false, PreRelease: Alpha},
			{Version: version.MustParse("1.29"), Default: true, PreRelease: Beta},
		},
		testBetaGate: {
			{Version: version.MustParse("1.27"), Default: false, PreRelease: Alpha},
			{Version: version.MustParse("1.28"), Default: true, PreRelease: Beta},
		},
		testBetaDisabled: {
			{Version: version.MustParse("1.27"), Default: false, PreRelease: Alpha},
			{Version: version.MustParse("1.28"), Default: true, PreRelease: Beta},
		},
	})
	require.NoError(t, err)

	require.NoError(t, f.SetFromMap(map[string]bool{"TestAlphaEnabled": true, "TestBetaDisabled": false}))
	f.AddMetrics()
	if err := testutil.GatherAndCompare(legacyregistry.DefaultGatherer, strings.NewReader(expectedOutput), testedMetrics...); err != nil {
		t.Fatal(err)
	}
}

func TestVersionedFeatureGateOverrideDefault(t *testing.T) {
	t.Run("overrides take effect", func(t *testing.T) {
		f := NewVersionedFeatureGate(version.MustParse("1.29"))
		require.NoError(t, f.SetEmulationVersion(version.MustParse("1.28")))
		if err := f.AddVersioned(map[Feature]VersionedSpecs{
			"TestFeature1": {
				{Version: version.MustParse("1.28"), Default: true},
			},
			"TestFeature2": {
				{Version: version.MustParse("1.26"), Default: false},
				{Version: version.MustParse("1.29"), Default: true},
			},
		}); err != nil {
			t.Fatal(err)
		}
		require.NoError(t, f.OverrideDefault("TestFeature1", false))
		require.NoError(t, f.OverrideDefault("TestFeature2", true))
		if f.Enabled("TestFeature1") {
			t.Error("expected TestFeature1 to have effective default of false")
		}
		if !f.Enabled("TestFeature2") {
			t.Error("expected TestFeature2 to have effective default of true")
		}
	})

	t.Run("overrides at specific version take effect", func(t *testing.T) {
		f := NewVersionedFeatureGate(version.MustParse("1.29"))
		require.NoError(t, f.SetEmulationVersion(version.MustParse("1.28")))
		if err := f.AddVersioned(map[Feature]VersionedSpecs{
			"TestFeature1": {
				{Version: version.MustParse("1.28"), Default: true},
			},
			"TestFeature2": {
				{Version: version.MustParse("1.26"), Default: false},
				{Version: version.MustParse("1.29"), Default: false},
			},
		}); err != nil {
			t.Fatal(err)
		}
		if f.OverrideDefaultAtVersion("TestFeature1", false, version.MustParse("1.27")) == nil {
			t.Error("expected error when attempting to override the default for a feature not available at given version")
		}
		require.NoError(t, f.OverrideDefaultAtVersion("TestFeature2", true, version.MustParse("1.27")))
		if !f.Enabled("TestFeature1") {
			t.Error("expected TestFeature1 to have effective default of true")
		}
		if !f.Enabled("TestFeature2") {
			t.Error("expected TestFeature2 to have effective default of true")
		}
		require.NoError(t, f.SetEmulationVersion(version.MustParse("1.29")))
		if !f.Enabled("TestFeature1") {
			t.Error("expected TestFeature1 to have effective default of true")
		}
		if f.Enabled("TestFeature2") {
			t.Error("expected TestFeature2 to have effective default of false")
		}
		require.NoError(t, f.SetEmulationVersion(version.MustParse("1.26")))
		if f.Enabled("TestFeature1") {
			t.Error("expected TestFeature1 to have effective default of false")
		}
		if !f.Enabled("TestFeature2") {
			t.Error("expected TestFeature2 to have effective default of true")
		}
	})

	t.Run("overrides are preserved across deep copies", func(t *testing.T) {
		f := NewVersionedFeatureGate(version.MustParse("1.29"))
		require.NoError(t, f.SetEmulationVersion(version.MustParse("1.28")))
		if err := f.AddVersioned(map[Feature]VersionedSpecs{
			"TestFeature": {
				{Version: version.MustParse("1.28"), Default: false},
				{Version: version.MustParse("1.29"), Default: true},
			},
		}); err != nil {
			t.Fatal(err)
		}
		require.NoError(t, f.OverrideDefault("TestFeature", true))
		fcopy := f.DeepCopy()
		if !fcopy.Enabled("TestFeature") {
			t.Error("default override was not preserved by deep copy")
		}
	})

	t.Run("overrides are not passed over after deep copies", func(t *testing.T) {
		f := NewVersionedFeatureGate(version.MustParse("1.29"))
		require.NoError(t, f.SetEmulationVersion(version.MustParse("1.28")))
		if err := f.AddVersioned(map[Feature]VersionedSpecs{
			"TestFeature": {
				{Version: version.MustParse("1.28"), Default: false},
				{Version: version.MustParse("1.29"), Default: true},
			},
		}); err != nil {
			t.Fatal(err)
		}
		assert.False(t, f.Enabled("TestFeature"))

		fcopy := f.DeepCopy()
		require.NoError(t, f.OverrideDefault("TestFeature", true))
		require.NoError(t, f.OverrideDefaultAtVersion("TestFeature", false, version.MustParse("1.29")))
		assert.True(t, f.Enabled("TestFeature"))
		assert.False(t, fcopy.Enabled("TestFeature"))

		require.NoError(t, f.SetEmulationVersion(version.MustParse("1.29")))
		assert.False(t, f.Enabled("TestFeature"))
		assert.False(t, fcopy.Enabled("TestFeature"))

		require.NoError(t, fcopy.SetEmulationVersion(version.MustParse("1.29")))
		assert.False(t, f.Enabled("TestFeature"))
		assert.True(t, fcopy.Enabled("TestFeature"))
	})

	t.Run("reflected in known features", func(t *testing.T) {
		f := NewVersionedFeatureGate(version.MustParse("1.29"))
		require.NoError(t, f.SetEmulationVersion(version.MustParse("1.28")))
		if err := f.AddVersioned(map[Feature]VersionedSpecs{
			"TestFeature": {
				{Version: version.MustParse("1.28"), Default: false, PreRelease: Alpha},
				{Version: version.MustParse("1.29"), Default: true, PreRelease: Beta},
			},
		}); err != nil {
			t.Fatal(err)
		}
		require.NoError(t, f.OverrideDefault("TestFeature", true))
		var found bool
		for _, s := range f.KnownFeatures() {
			if !strings.Contains(s, "TestFeature") {
				continue
			}
			found = true
			if !strings.Contains(s, "default=true") {
				t.Errorf("expected override of default to be reflected in known feature description %q", s)
			}
		}
		if !found {
			t.Error("found no entry for TestFeature in known features")
		}
	})

	t.Run("may not change default for specs with locked defaults", func(t *testing.T) {
		f := NewVersionedFeatureGate(version.MustParse("1.29"))
		require.NoError(t, f.SetEmulationVersion(version.MustParse("1.28")))
		if err := f.AddVersioned(map[Feature]VersionedSpecs{
			"LockedFeature": {
				{Version: version.MustParse("1.28"), Default: true, LockToDefault: true},
			},
		}); err != nil {
			t.Fatal(err)
		}
		if f.OverrideDefault("LockedFeature", false) == nil {
			t.Error("expected error when attempting to override the default for a feature with a locked default")
		}
		if f.OverrideDefault("LockedFeature", true) == nil {
			t.Error("expected error when attempting to override the default for a feature with a locked default")
		}
	})

	t.Run("can change default for specs without locked defaults for emulation version", func(t *testing.T) {
		f := NewVersionedFeatureGate(version.MustParse("1.29"))
		require.NoError(t, f.SetEmulationVersion(version.MustParse("1.28")))
		if err := f.AddVersioned(map[Feature]VersionedSpecs{
			"LockedFeature": {
				{Version: version.MustParse("1.28"), Default: true},
				{Version: version.MustParse("1.29"), Default: true, LockToDefault: true},
			},
		}); err != nil {
			t.Fatal(err)
		}
		require.NoError(t, f.OverrideDefault("LockedFeature", false))
		if f.Enabled("LockedFeature") {
			t.Error("expected LockedFeature to have effective default of false")
		}
	})

	t.Run("does not supersede explicitly-set value", func(t *testing.T) {
		f := NewVersionedFeatureGate(version.MustParse("1.29"))
		require.NoError(t, f.SetEmulationVersion(version.MustParse("1.28")))
		if err := f.AddVersioned(map[Feature]VersionedSpecs{
			"TestFeature": {
				{Version: version.MustParse("1.28"), Default: true},
			},
		}); err != nil {
			t.Fatal(err)
		}
		require.NoError(t, f.OverrideDefault("TestFeature", false))
		require.NoError(t, f.SetFromMap(map[string]bool{"TestFeature": true}))
		if !f.Enabled("TestFeature") {
			t.Error("expected feature to be effectively enabled despite default override")
		}
	})

	t.Run("prevents re-registration of feature spec after overriding default", func(t *testing.T) {
		f := NewVersionedFeatureGate(version.MustParse("1.29"))
		require.NoError(t, f.SetEmulationVersion(version.MustParse("1.28")))
		if err := f.AddVersioned(map[Feature]VersionedSpecs{
			"TestFeature": {
				{Version: version.MustParse("1.28"), Default: true, PreRelease: Alpha},
			},
		}); err != nil {
			t.Fatal(err)
		}
		require.NoError(t, f.OverrideDefault("TestFeature", false))
		if err := f.Add(map[Feature]FeatureSpec{
			"TestFeature": {
				Default:    true,
				PreRelease: Alpha,
			},
		}); err == nil {
			t.Error("expected re-registration to return a non-nil error after overriding its default")
		}
	})

	t.Run("does not allow override for a feature added after emulation version", func(t *testing.T) {
		f := NewVersionedFeatureGate(version.MustParse("1.29"))
		require.NoError(t, f.SetEmulationVersion(version.MustParse("1.28")))
		if err := f.AddVersioned(map[Feature]VersionedSpecs{
			"TestFeature": {
				{Version: version.MustParse("1.29"), Default: false},
			},
		}); err != nil {
			t.Fatal(err)
		}
		if err := f.OverrideDefault("TestFeature", true); err == nil {
			t.Error("expected an error to be returned in attempt to override default for a feature added after emulation version")
		}
	})

	t.Run("does not allow override for an unknown feature", func(t *testing.T) {
		f := NewVersionedFeatureGate(version.MustParse("1.29"))
		require.NoError(t, f.SetEmulationVersion(version.MustParse("1.28")))
		if err := f.OverrideDefault("TestFeature", true); err == nil {
			t.Error("expected an error to be returned in attempt to override default for unregistered feature")
		}
	})

	t.Run("returns error if already added to flag set", func(t *testing.T) {
		f := NewVersionedFeatureGate(version.MustParse("1.29"))
		require.NoError(t, f.SetEmulationVersion(version.MustParse("1.28")))
		fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
		f.AddFlag(fs)

		if err := f.OverrideDefault("TestFeature", true); err == nil {
			t.Error("expected a non-nil error to be returned")
		}
	})
}

func TestFeatureSpecAtEmulationVersion(t *testing.T) {
	specs := VersionedSpecs{
		{Version: version.MustParse("1.25"), Default: false, PreRelease: Alpha},
		{Version: version.MustParse("1.28"), Default: false, PreRelease: Beta},
		{Version: version.MustParse("1.29"), Default: true, PreRelease: GA},
	}
	sort.Sort(specs)
	tests := []struct {
		cVersion string
		expect   FeatureSpec
	}{
		{
			cVersion: "1.30",
			expect:   FeatureSpec{Version: version.MustParse("1.29"), Default: true, PreRelease: GA},
		},
		{
			cVersion: "1.29",
			expect:   FeatureSpec{Version: version.MustParse("1.29"), Default: true, PreRelease: GA},
		},
		{
			cVersion: "1.28",
			expect:   FeatureSpec{Version: version.MustParse("1.28"), Default: false, PreRelease: Beta},
		},
		{
			cVersion: "1.27",
			expect:   FeatureSpec{Version: version.MustParse("1.25"), Default: false, PreRelease: Alpha},
		},
		{
			cVersion: "1.25",
			expect:   FeatureSpec{Version: version.MustParse("1.25"), Default: false, PreRelease: Alpha},
		},
		{
			cVersion: "1.24",
			expect:   FeatureSpec{Version: version.MajorMinor(0, 0), Default: false, PreRelease: PreAlpha},
		},
	}
	for i, test := range tests {
		t.Run(fmt.Sprintf("featureSpecAtEmulationVersion for emulationVersion %s", test.cVersion), func(t *testing.T) {
			result := featureSpecAtEmulationVersion(specs, version.MustParse(test.cVersion))
			if !reflect.DeepEqual(*result, test.expect) {
				t.Errorf("%d: featureSpecAtEmulationVersion(, %s) Expected %v, Got %v", i, test.cVersion, test.expect, result)
			}
		})
	}
}

func TestCopyKnownFeatures(t *testing.T) {
	f := NewFeatureGate()
	require.NoError(t, f.Add(map[Feature]FeatureSpec{"FeatureA": {Default: false}, "FeatureB": {Default: false}}))
	require.NoError(t, f.Set("FeatureA=true"))
	require.NoError(t, f.OverrideDefault("FeatureB", true))
	fcopy := f.DeepCopyAndReset()
	require.NoError(t, f.Add(map[Feature]FeatureSpec{"FeatureC": {Default: false}}))

	assert.True(t, f.Enabled("FeatureA"))
	assert.True(t, f.Enabled("FeatureB"))
	assert.False(t, f.Enabled("FeatureC"))

	assert.False(t, fcopy.Enabled("FeatureA"))
	assert.True(t, fcopy.Enabled("FeatureB"))

	require.NoError(t, fcopy.Set("FeatureB=false"))
	assert.True(t, f.Enabled("FeatureB"))
	assert.False(t, fcopy.Enabled("FeatureB"))
	if err := fcopy.Set("FeatureC=true"); err == nil {
		t.Error("expected FeatureC not registered in the copied feature gate")
	}
}

func TestExplicitlySet(t *testing.T) {
	// gates for testing
	const testAlphaGate Feature = "TestAlpha"
	const testBetaGate Feature = "TestBeta"

	tests := []struct {
		arg                   string
		expectedFeatureValue  map[Feature]bool
		expectedExplicitlySet map[Feature]bool
	}{
		{
			arg: "",
			expectedFeatureValue: map[Feature]bool{
				allAlphaGate:  false,
				allBetaGate:   false,
				testAlphaGate: false,
				testBetaGate:  false,
			},
			expectedExplicitlySet: map[Feature]bool{
				allAlphaGate:  false,
				allBetaGate:   false,
				testAlphaGate: false,
				testBetaGate:  false,
			},
		},
		{
			arg: "AllAlpha=true,TestBeta=false",
			expectedFeatureValue: map[Feature]bool{
				allAlphaGate:  true,
				allBetaGate:   false,
				testAlphaGate: true,
				testBetaGate:  false,
			},
			expectedExplicitlySet: map[Feature]bool{
				allAlphaGate:  true,
				allBetaGate:   false,
				testAlphaGate: false,
				testBetaGate:  true,
			},
		},
		{
			arg: "AllAlpha=true,AllBeta=false",
			expectedFeatureValue: map[Feature]bool{
				allAlphaGate:  true,
				allBetaGate:   false,
				testAlphaGate: true,
				testBetaGate:  false,
			},
			expectedExplicitlySet: map[Feature]bool{
				allAlphaGate:  true,
				allBetaGate:   true,
				testAlphaGate: false,
				testBetaGate:  false,
			},
		},
	}
	for i, test := range tests {
		t.Run(test.arg, func(t *testing.T) {
			fs := pflag.NewFlagSet("testfeaturegateflag", pflag.ContinueOnError)
			f := NewVersionedFeatureGate(version.MustParse("1.29"))
			err := f.AddVersioned(map[Feature]VersionedSpecs{
				testAlphaGate: {
					{Version: version.MustParse("1.29"), Default: false, PreRelease: Alpha},
				},
				testBetaGate: {
					{Version: version.MustParse("1.28"), Default: false, PreRelease: Alpha},
					{Version: version.MustParse("1.29"), Default: false, PreRelease: Beta},
				},
			})
			require.NoError(t, err)
			f.AddFlag(fs)

			var errs []error
			err = fs.Parse([]string{fmt.Sprintf("--%s=%s", flagName, test.arg)})
			if err != nil {
				errs = append(errs, err)
			}
			err = utilerrors.NewAggregate(errs)
			require.NoError(t, err)
			for k, v := range test.expectedFeatureValue {
				if actual := f.Enabled(k); actual != v {
					t.Errorf("%d: expected %s=%v, Got %v", i, k, v, actual)
				}
			}
			for k, v := range test.expectedExplicitlySet {
				if actual := f.ExplicitlySet(k); actual != v {
					t.Errorf("%d: expected ExplicitlySet(%s)=%v, Got %v", i, k, v, actual)
				}
			}
		})
	}
}

func TestResetFeatureValueToDefault(t *testing.T) {
	// gates for testing
	const testAlphaGate Feature = "TestAlpha"
	const testBetaGate Feature = "TestBeta"

	f := NewVersionedFeatureGate(version.MustParse("1.29"))
	err := f.AddVersioned(map[Feature]VersionedSpecs{
		testAlphaGate: {
			{Version: version.MustParse("1.29"), Default: false, PreRelease: Alpha},
		},
		testBetaGate: {
			{Version: version.MustParse("1.28"), Default: false, PreRelease: Alpha},
			{Version: version.MustParse("1.29"), Default: true, PreRelease: Beta},
		},
	})
	require.NoError(t, err)

	fs := pflag.NewFlagSet("testfeaturegateflag", pflag.ContinueOnError)
	assert.False(t, f.Enabled("AllAlpha"))
	assert.False(t, f.Enabled("AllBeta"))
	assert.False(t, f.Enabled("TestAlpha"))
	assert.True(t, f.Enabled("TestBeta"))

	f.AddFlag(fs)
	var errs []error
	err = fs.Parse([]string{fmt.Sprintf("--%s=%s", flagName, "AllAlpha=true,TestBeta=false")})
	if err != nil {
		errs = append(errs, err)
	}
	err = utilerrors.NewAggregate(errs)
	require.NoError(t, err)
	assert.True(t, f.Enabled("AllAlpha"))
	assert.False(t, f.Enabled("AllBeta"))
	assert.True(t, f.Enabled("TestAlpha"))
	assert.False(t, f.Enabled("TestBeta"))

	require.NoError(t, f.ResetFeatureValueToDefault("AllAlpha"))
	assert.False(t, f.Enabled("AllAlpha"))
	assert.False(t, f.Enabled("AllBeta"))
	assert.True(t, f.Enabled("TestAlpha"))
	assert.False(t, f.Enabled("TestBeta"))

	require.NoError(t, f.ResetFeatureValueToDefault("TestBeta"))
	assert.False(t, f.Enabled("AllAlpha"))
	assert.False(t, f.Enabled("AllBeta"))
	assert.True(t, f.Enabled("TestAlpha"))
	assert.True(t, f.Enabled("TestBeta"))

	require.NoError(t, f.SetEmulationVersion(version.MustParse("1.28")))
	assert.False(t, f.Enabled("AllAlpha"))
	assert.False(t, f.Enabled("AllBeta"))
	assert.False(t, f.Enabled("TestAlpha"))
	assert.False(t, f.Enabled("TestBeta"))
}

func TestAddVersioned(t *testing.T) {
	// gates for testing
	const testAGate Feature = "TestA"
	const testBGate Feature = "TestB"
	tests := []struct {
		name        string
		expectError bool
		features    map[Feature]VersionedSpecs
	}{
		{
			name: "normal progression",
			features: map[Feature]VersionedSpecs{
				testAGate: {
					{Version: version.MustParse("1.29"), Default: false, PreRelease: Alpha},
					{Version: version.MustParse("1.30"), Default: false, PreRelease: Beta},
					{Version: version.MustParse("1.31"), Default: true, PreRelease: Beta},
					{Version: version.MustParse("1.32"), Default: true, PreRelease: GA},
					{Version: version.MustParse("1.33"), Default: false, PreRelease: Deprecated},
				},
				testBGate: {
					{Version: version.MustParse("1.27"), Default: false, PreRelease: Alpha},
				},
			},
		},
		{
			name:        "conflicting specs",
			expectError: true,
			features: map[Feature]VersionedSpecs{
				testBGate: {
					{Version: version.MustParse("1.28"), Default: false, PreRelease: Alpha},
				},
			},
		},
		{
			name:        "deprecated feature with no prior state",
			expectError: true,
			features: map[Feature]VersionedSpecs{
				testAGate: {
					{Version: version.MustParse("1.29"), Default: false, PreRelease: Deprecated},
				},
			},
		},
		{
			name: "deprecated feature with prior state",
			features: map[Feature]VersionedSpecs{
				testAGate: {
					{Version: version.MustParse("1.0"), Default: true, PreRelease: GA},
					{Version: version.MustParse("1.29"), Default: false, PreRelease: Deprecated},
				},
			},
		},
		{
			name:        "duplicate version",
			expectError: true,
			features: map[Feature]VersionedSpecs{
				testAGate: {
					{Version: version.MustParse("1.29"), Default: false, PreRelease: Alpha},
					{Version: version.MustParse("1.29"), Default: true, PreRelease: Beta},
				},
			},
		},
		{
			name:        "decreasing version",
			expectError: true,
			features: map[Feature]VersionedSpecs{
				testAGate: {
					{Version: version.MustParse("1.30"), Default: false, PreRelease: Beta},
					{Version: version.MustParse("1.29"), Default: true, PreRelease: Beta},
				},
			},
		},
		{
			name:        "Beta to Alpha",
			expectError: true,
			features: map[Feature]VersionedSpecs{
				testAGate: {
					{Version: version.MustParse("1.29"), Default: true, PreRelease: Beta},
					{Version: version.MustParse("1.30"), Default: false, PreRelease: Alpha},
				},
			},
		},
		{
			name:        "GA to Alpha",
			expectError: true,
			features: map[Feature]VersionedSpecs{
				testAGate: {
					{Version: version.MustParse("1.29"), Default: true, PreRelease: GA},
					{Version: version.MustParse("1.30"), Default: false, PreRelease: Alpha},
				},
			},
		},
		{
			name:        "GA to Beta",
			expectError: true,
			features: map[Feature]VersionedSpecs{
				testAGate: {
					{Version: version.MustParse("1.29"), Default: true, PreRelease: GA},
					{Version: version.MustParse("1.30"), Default: true, PreRelease: Beta},
				},
			},
		},
		{
			name:        "Alpha to Deprecated to Beta",
			expectError: true,
			features: map[Feature]VersionedSpecs{
				testAGate: {
					{Version: version.MustParse("1.29"), Default: false, PreRelease: Alpha},
					{Version: version.MustParse("1.30"), Default: false, PreRelease: Deprecated},
					{Version: version.MustParse("1.31"), Default: true, PreRelease: Beta},
				},
			},
		},
		{
			name: "Deprecated to Deprecated",
			features: map[Feature]VersionedSpecs{
				testAGate: {
					{Version: version.MustParse("1.29"), Default: false, PreRelease: Alpha},
					{Version: version.MustParse("1.30"), Default: true, PreRelease: Deprecated},
					{Version: version.MustParse("1.31"), Default: false, PreRelease: Deprecated},
				},
			},
		},
		{
			name: "always Deprecated",
			features: map[Feature]VersionedSpecs{
				testAGate: {
					{Version: version.MustParse("1.0"), Default: false, PreRelease: Deprecated},
				},
			},
		},
		{
			name:        "patch version",
			expectError: true,
			features: map[Feature]VersionedSpecs{
				testAGate: {
					{Version: version.MustParse("1.29.1"), Default: false, PreRelease: Alpha},
					{Version: version.MustParse("1.30"), Default: false, PreRelease: Beta},
				},
			},
		},
	}
	for _, test := range tests {
		t.Run(fmt.Sprintf("AddVersioned-%s", test.name), func(t *testing.T) {
			f := NewFeatureGate()
			err := f.AddVersioned(map[Feature]VersionedSpecs{
				testBGate: {
					{Version: version.MustParse("1.27"), Default: false, PreRelease: Alpha},
				},
			})
			require.NoError(t, err)
			err = f.AddVersioned(test.features)
			if err != nil && !test.expectError {
				t.Errorf("expected no errors, error found %+v", err)
			}

			if err == nil && test.expectError {
				t.Errorf("expected errors, no errors found")
			}
		})
	}
}

func TestFeatureForVersion(t *testing.T) {
	defaultFeature := Feature("default")
	betaFeature := Feature("beta")
	alphaFeature := Feature("alpha")
	stableFeature := Feature("stable")
	tests := []struct {
		name              string
		versionedFeatures []VersionedFeature
		emulatedVersion   string // format: "1.XX"
		want              Feature
	}{
		{
			name:              "empty versioned features",
			versionedFeatures: nil,
			emulatedVersion:   "1.25",
			want:              defaultFeature,
		},
		{
			name: "single version",
			versionedFeatures: []VersionedFeature{
				{Version: version.MajorMinor(1, 24), Feature: betaFeature},
			},
			emulatedVersion: "1.25",
			want:            betaFeature,
		},
		{
			name: "multiple versions - exact match",
			versionedFeatures: []VersionedFeature{
				{Version: version.MajorMinor(1, 23), Feature: alphaFeature},
				{Version: version.MajorMinor(1, 24), Feature: betaFeature},
				{Version: version.MajorMinor(1, 25), Feature: stableFeature},
			},
			emulatedVersion: "1.24",
			want:            betaFeature,
		},
		{
			name: "between versions",
			versionedFeatures: []VersionedFeature{
				{Version: version.MajorMinor(1, 23), Feature: alphaFeature},
				{Version: version.MajorMinor(1, 25), Feature: betaFeature},
			},
			emulatedVersion: "1.24",
			want:            alphaFeature,
		},
		{
			name: "before all versions",
			versionedFeatures: []VersionedFeature{
				{Version: version.MajorMinor(1, 24), Feature: betaFeature},
				{Version: version.MajorMinor(1, 25), Feature: stableFeature},
			},
			emulatedVersion: "1.23",
			want:            defaultFeature,
		},
		{
			name: "after all versions",
			versionedFeatures: []VersionedFeature{
				{Version: version.MajorMinor(1, 23), Feature: alphaFeature},
				{Version: version.MajorMinor(1, 24), Feature: betaFeature},
			},
			emulatedVersion: "1.25",
			want:            betaFeature,
		},
		{
			name: "unsorted versions",
			versionedFeatures: []VersionedFeature{
				{Version: version.MajorMinor(1, 25), Feature: stableFeature},
				{Version: version.MajorMinor(1, 23), Feature: alphaFeature},
				{Version: version.MajorMinor(1, 24), Feature: betaFeature},
			},
			emulatedVersion: "1.24",
			want:            betaFeature,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			emuVer := version.MustParse(tt.emulatedVersion)
			got := FeatureForVersion(emuVer, tt.versionedFeatures, defaultFeature)
			if got != tt.want {
				t.Errorf("FeatureForVersion() = %v, want %v", got, tt.want)
			}
		})
	}
}
