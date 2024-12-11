// Copyright 2024 The etcd Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package featuregate is copied from k8s.io/component-base@v0.30.1 to avoid any potential circular dependency between k8s and etcd.
package featuregate

import (
	"flag"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/spf13/pflag"
	"go.uber.org/zap"
)

type Feature string

const (
	defaultFlagName = "feature-gates"

	// allAlphaGate is a global toggle for alpha features. Per-feature key
	// values override the default set by allAlphaGate. Examples:
	//   AllAlpha=false,NewFeature=true  will result in newFeature=true
	//   AllAlpha=true,NewFeature=false  will result in newFeature=false
	allAlphaGate Feature = "AllAlpha"

	// allBetaGate is a global toggle for beta features. Per-feature key
	// values override the default set by allBetaGate. Examples:
	//   AllBeta=false,NewFeature=true  will result in NewFeature=true
	//   AllBeta=true,NewFeature=false  will result in NewFeature=false
	allBetaGate Feature = "AllBeta"
)

var (
	// The generic features.
	defaultFeatures = map[Feature]FeatureSpec{
		allAlphaGate: {Default: false, PreRelease: Alpha},
		allBetaGate:  {Default: false, PreRelease: Beta},
	}

	// Special handling for a few gates.
	specialFeatures = map[Feature]func(known map[Feature]FeatureSpec, enabled map[Feature]bool, val bool){
		allAlphaGate: setUnsetAlphaGates,
		allBetaGate:  setUnsetBetaGates,
	}
)

type FeatureSpec struct {
	// Default is the default enablement state for the feature
	Default bool
	// LockToDefault indicates that the feature is locked to its default and cannot be changed
	LockToDefault bool
	// PreRelease indicates the maturity level of the feature
	PreRelease prerelease
}

type prerelease string

const (
	// Values for PreRelease.
	Alpha = prerelease("ALPHA")
	Beta  = prerelease("BETA")
	GA    = prerelease("")

	// Deprecated
	Deprecated = prerelease("DEPRECATED")
)

// FeatureGate indicates whether a given feature is enabled or not
type FeatureGate interface {
	// Enabled returns true if the key is enabled.
	Enabled(key Feature) bool
	// KnownFeatures returns a slice of strings describing the FeatureGate's known features.
	KnownFeatures() []string
	// DeepCopy returns a deep copy of the FeatureGate object, such that gates can be
	// set on the copy without mutating the original. This is useful for validating
	// config against potential feature gate changes before committing those changes.
	DeepCopy() MutableFeatureGate
}

// MutableFeatureGate parses and stores flag gates for known features from
// a string like feature1=true,feature2=false,...
type MutableFeatureGate interface {
	FeatureGate

	// AddFlag adds a flag for setting global feature gates to the specified FlagSet.
	AddFlag(fs *flag.FlagSet, flagName string)
	// Set parses and stores flag gates for known features
	// from a string like feature1=true,feature2=false,...
	Set(value string) error
	// SetFromMap stores flag gates for known features from a map[string]bool or returns an error
	SetFromMap(m map[string]bool) error
	// Add adds features to the featureGate.
	Add(features map[Feature]FeatureSpec) error
	// GetAll returns a copy of the map of known feature names to feature specs.
	GetAll() map[Feature]FeatureSpec
	// AddMetrics adds feature enablement metrics
	AddMetrics()
	// OverrideDefault sets a local override for the registered default value of a named
	// feature. If the feature has not been previously registered (e.g. by a call to Add), has a
	// locked default, or if the gate has already registered itself with a FlagSet, a non-nil
	// error is returned.
	//
	// When two or more components consume a common feature, one component can override its
	// default at runtime in order to adopt new defaults before or after the other
	// components. For example, a new feature can be evaluated with a limited blast radius by
	// overriding its default to true for a limited number of components without simultaneously
	// changing its default for all consuming components.
	OverrideDefault(name Feature, override bool) error
}

// featureGate implements FeatureGate as well as pflag.Value for flag parsing.
type featureGate struct {
	lg *zap.Logger

	featureGateName string

	special map[Feature]func(map[Feature]FeatureSpec, map[Feature]bool, bool)

	// lock guards writes to known, enabled, and reads/writes of closed
	lock sync.Mutex
	// known holds a map[Feature]FeatureSpec
	known atomic.Value
	// enabled holds a map[Feature]bool
	enabled atomic.Value
	// closed is set to true when AddFlag is called, and prevents subsequent calls to Add
	closed bool
}

func setUnsetAlphaGates(known map[Feature]FeatureSpec, enabled map[Feature]bool, val bool) {
	for k, v := range known {
		if v.PreRelease == Alpha {
			if _, found := enabled[k]; !found {
				enabled[k] = val
			}
		}
	}
}

func setUnsetBetaGates(known map[Feature]FeatureSpec, enabled map[Feature]bool, val bool) {
	for k, v := range known {
		if v.PreRelease == Beta {
			if _, found := enabled[k]; !found {
				enabled[k] = val
			}
		}
	}
}

// Set, String, and Type implement pflag.Value
var _ pflag.Value = &featureGate{}

func New(name string, lg *zap.Logger) *featureGate {
	if lg == nil {
		lg = zap.NewNop()
	}
	known := map[Feature]FeatureSpec{}
	for k, v := range defaultFeatures {
		known[k] = v
	}

	f := &featureGate{
		lg:              lg,
		featureGateName: name,
		special:         specialFeatures,
	}
	f.known.Store(known)
	f.enabled.Store(map[Feature]bool{})

	return f
}

// Set parses a string of the form "key1=value1,key2=value2,..." into a
// map[string]bool of known keys or returns an error.
func (f *featureGate) Set(value string) error {
	m := make(map[string]bool)
	for _, s := range strings.Split(value, ",") {
		if len(s) == 0 {
			continue
		}
		arr := strings.SplitN(s, "=", 2)
		k := strings.TrimSpace(arr[0])
		if len(arr) != 2 {
			return fmt.Errorf("missing bool value for %s", k)
		}
		v := strings.TrimSpace(arr[1])
		boolValue, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("invalid value of %s=%s, err: %w", k, v, err)
		}
		m[k] = boolValue
	}
	return f.SetFromMap(m)
}

// SetFromMap stores flag gates for known features from a map[string]bool or returns an error
func (f *featureGate) SetFromMap(m map[string]bool) error {
	f.lock.Lock()
	defer f.lock.Unlock()

	// Copy existing state
	known := map[Feature]FeatureSpec{}
	for k, v := range f.known.Load().(map[Feature]FeatureSpec) {
		known[k] = v
	}
	enabled := map[Feature]bool{}
	for k, v := range f.enabled.Load().(map[Feature]bool) {
		enabled[k] = v
	}

	for k, v := range m {
		k := Feature(k)
		featureSpec, ok := known[k]
		if !ok {
			return fmt.Errorf("unrecognized feature gate: %s", k)
		}
		if featureSpec.LockToDefault && featureSpec.Default != v {
			return fmt.Errorf("cannot set feature gate %v to %v, feature is locked to %v", k, v, featureSpec.Default)
		}
		enabled[k] = v
		// Handle "special" features like "all alpha gates"
		if fn, found := f.special[k]; found {
			fn(known, enabled, v)
		}

		if featureSpec.PreRelease == Deprecated {
			f.lg.Warn(fmt.Sprintf("Setting deprecated feature gate %s=%t. It will be removed in a future release.", k, v))
		} else if featureSpec.PreRelease == GA {
			f.lg.Warn(fmt.Sprintf("Setting GA feature gate %s=%t. It will be removed in a future release.", k, v))
		}
	}

	// Persist changes
	f.known.Store(known)
	f.enabled.Store(enabled)

	f.lg.Info(fmt.Sprintf("feature gates: %v", f.enabled))
	return nil
}

// String returns a string containing all enabled feature gates, formatted as "key1=value1,key2=value2,...".
func (f *featureGate) String() string {
	pairs := []string{}
	for k, v := range f.enabled.Load().(map[Feature]bool) {
		pairs = append(pairs, fmt.Sprintf("%s=%t", k, v))
	}
	sort.Strings(pairs)
	return strings.Join(pairs, ",")
}

func (f *featureGate) Type() string {
	return "mapStringBool"
}

// Add adds features to the featureGate.
func (f *featureGate) Add(features map[Feature]FeatureSpec) error {
	f.lock.Lock()
	defer f.lock.Unlock()

	if f.closed {
		return fmt.Errorf("cannot add a feature gate after adding it to the flag set")
	}

	// Copy existing state
	known := map[Feature]FeatureSpec{}
	for k, v := range f.known.Load().(map[Feature]FeatureSpec) {
		known[k] = v
	}

	for name, spec := range features {
		if existingSpec, found := known[name]; found {
			if existingSpec == spec {
				continue
			}
			return fmt.Errorf("feature gate %q with different spec already exists: %v", name, existingSpec)
		}

		known[name] = spec
	}

	// Persist updated state
	f.known.Store(known)

	return nil
}

func (f *featureGate) OverrideDefault(name Feature, override bool) error {
	f.lock.Lock()
	defer f.lock.Unlock()

	if f.closed {
		return fmt.Errorf("cannot override default for feature %q: gates already added to a flag set", name)
	}

	known := map[Feature]FeatureSpec{}
	for name, spec := range f.known.Load().(map[Feature]FeatureSpec) {
		known[name] = spec
	}

	spec, ok := known[name]
	switch {
	case !ok:
		return fmt.Errorf("cannot override default: feature %q is not registered", name)
	case spec.LockToDefault:
		return fmt.Errorf("cannot override default: feature %q default is locked to %t", name, spec.Default)
	case spec.PreRelease == Deprecated:
		f.lg.Warn(fmt.Sprintf("Overriding default of deprecated feature gate %s=%t. It will be removed in a future release.", name, override))
	case spec.PreRelease == GA:
		f.lg.Warn(fmt.Sprintf("Overriding default of GA feature gate %s=%t. It will be removed in a future release.", name, override))
	}

	spec.Default = override
	known[name] = spec
	f.known.Store(known)

	return nil
}

// GetAll returns a copy of the map of known feature names to feature specs.
func (f *featureGate) GetAll() map[Feature]FeatureSpec {
	retval := map[Feature]FeatureSpec{}
	for k, v := range f.known.Load().(map[Feature]FeatureSpec) {
		retval[k] = v
	}
	return retval
}

// Enabled returns true if the key is enabled.  If the key is not known, this call will panic.
func (f *featureGate) Enabled(key Feature) bool {
	if v, ok := f.enabled.Load().(map[Feature]bool)[key]; ok {
		return v
	}
	if v, ok := f.known.Load().(map[Feature]FeatureSpec)[key]; ok {
		return v.Default
	}

	panic(fmt.Errorf("feature %q is not registered in FeatureGate %q", key, f.featureGateName))
}

// AddFlag adds a flag for setting global feature gates to the specified FlagSet.
func (f *featureGate) AddFlag(fs *flag.FlagSet, flagName string) {
	if flagName == "" {
		flagName = defaultFlagName
	}
	f.lock.Lock()
	// TODO(mtaufen): Shouldn't we just close it on the first Set/SetFromMap instead?
	// Not all components expose a feature gates flag using this AddFlag method, and
	// in the future, all components will completely stop exposing a feature gates flag,
	// in favor of componentconfig.
	f.closed = true
	f.lock.Unlock()

	known := f.KnownFeatures()
	fs.Var(f, flagName, ""+
		"A set of key=value pairs that describe feature gates for alpha/experimental features. "+
		"Options are:\n"+strings.Join(known, "\n"))
}

func (f *featureGate) AddMetrics() {
	// TODO(henrybear327): implement this.
}

// KnownFeatures returns a slice of strings describing the FeatureGate's known features.
// Deprecated and GA features are hidden from the list.
func (f *featureGate) KnownFeatures() []string {
	var known []string
	for k, v := range f.known.Load().(map[Feature]FeatureSpec) {
		if v.PreRelease == GA || v.PreRelease == Deprecated {
			continue
		}
		known = append(known, fmt.Sprintf("%s=true|false (%s - default=%t)", k, v.PreRelease, v.Default))
	}
	sort.Strings(known)
	return known
}

// DeepCopy returns a deep copy of the FeatureGate object, such that gates can be
// set on the copy without mutating the original. This is useful for validating
// config against potential feature gate changes before committing those changes.
func (f *featureGate) DeepCopy() MutableFeatureGate {
	// Copy existing state.
	known := map[Feature]FeatureSpec{}
	for k, v := range f.known.Load().(map[Feature]FeatureSpec) {
		known[k] = v
	}
	enabled := map[Feature]bool{}
	for k, v := range f.enabled.Load().(map[Feature]bool) {
		enabled[k] = v
	}

	// Construct a new featureGate around the copied state.
	// Note that specialFeatures is treated as immutable by convention,
	// and we maintain the value of f.closed across the copy.
	fg := &featureGate{
		special: specialFeatures,
		closed:  f.closed,
	}

	fg.known.Store(known)
	fg.enabled.Store(enabled)

	return fg
}
