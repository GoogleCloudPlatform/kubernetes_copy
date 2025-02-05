/*
Copyright 2023 The Kubernetes Authors.

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

package environment

import (
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/checker"
	"github.com/google/cel-go/ext"
	"github.com/google/cel-go/interpreter"
	"golang.org/x/sync/singleflight"

	"k8s.io/apimachinery/pkg/util/version"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
	"k8s.io/apiserver/pkg/cel/library"
	genericfeatures "k8s.io/apiserver/pkg/features"
	"k8s.io/apiserver/pkg/util/compatibility"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	basecompatibility "k8s.io/component-base/compatibility"
)

// DefaultCompatibilityVersion returns a default compatibility version for use with EnvSet
// that guarantees compatibility with CEL features/libraries/parameters understood by
// the api server min compatibility version
//
// This default will be set to no more than the current Kubernetes major.minor version.
//
// Note that a default version number less than n-1 the current Kubernetes major.minor version
// indicates a wider range of version compatibility than strictly required for rollback.
// A wide range of compatibility is desirable because it means that CEL expressions are portable
// across a wider range of Kubernetes versions.
// A default version number equal to the current Kubernetes major.minor version
// indicates fast forward CEL features that can be used when rollback is no longer needed.
func DefaultCompatibilityVersion() *version.Version {
	effectiveVer := compatibility.DefaultComponentGlobalsRegistry.EffectiveVersionFor(basecompatibility.DefaultKubeComponent)
	if effectiveVer == nil {
		effectiveVer = compatibility.DefaultBuildEffectiveVersion()
	}
	return effectiveVer.MinCompatibilityVersion()
}

var baseOpts = append(baseOptsWithoutStrictCost, StrictCostOpt)

var baseOptsWithoutStrictCost = []VersionedOptions{
	{
		// CEL epoch was actually 1.23, but we artificially set it to 1.0 because these
		// options should always be present.
		IntroducedVersion: version.MajorMinor(1, 0),
		EnvOptions: []cel.EnvOption{
			cel.HomogeneousAggregateLiterals(),
			// Validate function declarations once during base env initialization,
			// so they don't need to be evaluated each time a CEL rule is compiled.
			// This is a relatively expensive operation.
			cel.EagerlyValidateDeclarations(true),
			cel.DefaultUTCTimeZone(true),

			UnversionedLib(library.URLs),
			UnversionedLib(library.Regex),
			UnversionedLib(library.Lists),

			// cel-go v0.17.7 change the cost of has() from 0 to 1, but also provided the CostEstimatorOptions option to preserve the old behavior, so we enabled it at the same time we bumped our cel version to v0.17.7.
			// Since it is a regression fix, we apply it uniformly to all code use v0.17.7.
			cel.CostEstimatorOptions(checker.PresenceTestHasCost(false)),
		},
		ProgramOptions: []cel.ProgramOption{
			cel.EvalOptions(cel.OptOptimize, cel.OptTrackCost),
			cel.CostLimit(celconfig.PerCallLimit),

			// cel-go v0.17.7 change the cost of has() from 0 to 1, but also provided the CostEstimatorOptions option to preserve the old behavior, so we enabled it at the same time we bumped our cel version to v0.17.7.
			// Since it is a regression fix, we apply it uniformly to all code use v0.17.7.
			cel.CostTrackerOptions(interpreter.PresenceTestHasCost(false)),
		},
	},
	{
		IntroducedVersion: version.MajorMinor(1, 27),
		EnvOptions: []cel.EnvOption{
			UnversionedLib(library.Authz),
		},
	},
	{
		IntroducedVersion: version.MajorMinor(1, 28),
		EnvOptions: []cel.EnvOption{
			cel.CrossTypeNumericComparisons(true),
			cel.OptionalTypes(),
			UnversionedLib(library.Quantity),
		},
	},
	// add the new validator in 1.29
	{
		IntroducedVersion: version.MajorMinor(1, 29),
		EnvOptions: []cel.EnvOption{
			cel.ASTValidators(
				cel.ValidateDurationLiterals(),
				cel.ValidateTimestampLiterals(),
				cel.ValidateRegexLiterals(),
				cel.ValidateHomogeneousAggregateLiterals(),
			),
		},
	},
	// String library
	{
		IntroducedVersion: version.MajorMinor(1, 0),
		RemovedVersion:    version.MajorMinor(1, 29),
		EnvOptions: []cel.EnvOption{
			ext.Strings(ext.StringsVersion(0)),
		},
	},
	{
		IntroducedVersion: version.MajorMinor(1, 29),
		EnvOptions: []cel.EnvOption{
			ext.Strings(ext.StringsVersion(2)),
		},
	},
	// Set library
	{
		IntroducedVersion: version.MajorMinor(1, 29),
		EnvOptions: []cel.EnvOption{
			ext.Sets(),
		},
	},
	{
		IntroducedVersion: version.MajorMinor(1, 30),
		EnvOptions: []cel.EnvOption{
			UnversionedLib(library.IP),
			UnversionedLib(library.CIDR),
		},
	},
	// Format Library
	{
		IntroducedVersion: version.MajorMinor(1, 31),
		EnvOptions: []cel.EnvOption{
			UnversionedLib(library.Format),
		},
	},
	// Authz selectors
	{
		IntroducedVersion: version.MajorMinor(1, 31),
		FeatureEnabled: func() bool {
			enabled := utilfeature.DefaultFeatureGate.Enabled(genericfeatures.AuthorizeWithSelectors)
			authzSelectorsLibraryInit.Do(func() {
				// Record the first time feature enablement was checked for this library.
				// This is checked from integration tests to ensure no cached cel envs
				// are constructed before feature enablement is effectively set.
				authzSelectorsLibraryEnabled.Store(enabled)
				// Uncomment to debug where the first initialization is coming from if needed.
				// debug.PrintStack()
			})
			return enabled
		},
		EnvOptions: []cel.EnvOption{
			UnversionedLib(library.AuthzSelectors),
		},
	},
	// Two variable comprehensions
	{
		IntroducedVersion: version.MajorMinor(1, 32),
		EnvOptions: []cel.EnvOption{
			ext.TwoVarComprehensions(),
		},
	},
}

var (
	authzSelectorsLibraryInit    sync.Once
	authzSelectorsLibraryEnabled atomic.Value
)

// AuthzSelectorsLibraryEnabled returns whether the AuthzSelectors library was enabled when it was constructed.
// If it has not been contructed yet, this returns `false, false`.
// This is solely for the benefit of the integration tests making sure feature gates get correctly parsed before AuthzSelector ever has to check for enablement.
func AuthzSelectorsLibraryEnabled() (enabled, constructed bool) {
	enabled, constructed = authzSelectorsLibraryEnabled.Load().(bool)
	return
}

var StrictCostOpt = VersionedOptions{
	// This is to configure the cost calculation for extended libraries
	IntroducedVersion: version.MajorMinor(1, 0),
	ProgramOptions: []cel.ProgramOption{
		cel.CostTracking(&library.CostEstimator{}),
	},
}

// cacheBaseEnvs controls whether calls to MustBaseEnvSet are cached.
// Defaults to true, may be disabled by calling DisableBaseEnvSetCachingForTests.
var cacheBaseEnvs = true

// DisableBaseEnvSetCachingForTests clears and disables base env caching.
// This is only intended for unit tests exercising MustBaseEnvSet directly with different enablement options.
// It does not clear other initialization paths that may cache results of calling MustBaseEnvSet.
func DisableBaseEnvSetCachingForTests() {
	cacheBaseEnvs = false
	baseEnvs.Clear()
	baseEnvsWithOption.Clear()
}

// MustBaseEnvSet returns the common CEL base environments for Kubernetes for Version, or panics
// if the version is nil, or does not have major and minor components.
//
// The returned environment contains function libraries, language settings, optimizations and
// runtime cost limits appropriate CEL as it is used in Kubernetes.
//
// The returned environment contains no CEL variable definitions or custom type declarations and
// should be extended to construct environments with the appropriate variable definitions,
// type declarations and any other needed configuration.
// strictCost is used to determine whether to enforce strict cost calculation for CEL expressions.
func MustBaseEnvSet(ver *version.Version, strictCost bool) *EnvSet {
	if ver == nil {
		panic("version must be non-nil")
	}
	if len(ver.Components()) < 2 {
		panic(fmt.Sprintf("version must contain an major and minor component, but got: %s", ver.String()))
	}
	key := strconv.FormatUint(uint64(ver.Major()), 10) + "." + strconv.FormatUint(uint64(ver.Minor()), 10)
	var entry interface{}
	if strictCost {
		if entry, ok := baseEnvs.Load(key); ok {
			return entry.(*EnvSet)
		}
		entry, _, _ = baseEnvsSingleflight.Do(key, func() (interface{}, error) {
			entry := mustNewEnvSet(ver, baseOpts)
			if cacheBaseEnvs {
				baseEnvs.Store(key, entry)
			}
			return entry, nil
		})
	} else {
		if entry, ok := baseEnvsWithOption.Load(key); ok {
			return entry.(*EnvSet)
		}
		entry, _, _ = baseEnvsWithOptionSingleflight.Do(key, func() (interface{}, error) {
			entry := mustNewEnvSet(ver, baseOptsWithoutStrictCost)
			if cacheBaseEnvs {
				baseEnvsWithOption.Store(key, entry)
			}
			return entry, nil
		})
	}

	return entry.(*EnvSet)
}

var (
	baseEnvs                       = sync.Map{}
	baseEnvsWithOption             = sync.Map{}
	baseEnvsSingleflight           = &singleflight.Group{}
	baseEnvsWithOptionSingleflight = &singleflight.Group{}
)

// UnversionedLib wraps library initialization calls like ext.Sets() or library.IP()
// to force compilation errors if the call evolves to include a varadic variable option.
//
// This provides automatic detection of a problem that is hard to catch in review--
// If a CEL library used in Kubernetes is unversioned and then become versioned, and we
// fail to set a desired version, the libraries defaults to the latest version, changing
// CEL environment without controlled rollout, bypassing the entire purpose of the base
// environment.
//
// If usages of this function fail to compile: add version=1 argument to all call sites
// that fail compilation while removing the UnversionedLib wrapper. Next, review
// the changes in the library present in higher versions and, if needed, use VersionedOptions to
// the base environment to roll out to a newer version safely.
func UnversionedLib(initializer func() cel.EnvOption) cel.EnvOption {
	return initializer()
}
