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

package features

import (
	"fmt"
	"os"
	"strconv"
	"sync"
	"sync/atomic"

	"k8s.io/apimachinery/pkg/util/naming"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/klog/v2"
)

// internalPackages are packages that ignored when creating a name for featureGates. These packages are in the common
// call chains, so they'd be unhelpful as names.
var internalPackages = []string{"k8s.io/client-go/features/envvar.go"}

var _ Gates = &envVarFeatureGates{}

// newEnvVarFeatureGates creates a feature gate that allows for registration
// of features and checking if the features are enabled.
//
// On the first call to Enabled, the effective state of all known features is loaded from
// environment variables. The environment variable read for a given feature is formed by
// concatenating the prefix "KUBE_FEATURE_" with the feature's name.
//
// For example, if you have a feature named "MyFeature"
// setting an environmental variable "KUBE_FEATURE_MyFeature"
// will allow you to configure the state of that feature.
//
// Please note that environmental variables can only be set to the boolean value.
// Incorrect values will be ignored and logged.
//
// Features can also be set directly via the Set method.
// In that case, these features take precedence over
// features set via environmental variables.
func newEnvVarFeatureGates(features map[Feature]FeatureSpec) *envVarFeatureGates {
	known := map[Feature]FeatureSpec{}
	for name, spec := range features {
		known[name] = spec
	}

	fg := &envVarFeatureGates{
		callSiteName: naming.GetNameFromCallsite(internalPackages...),
		known:        known,
	}
	fg.enabledViaEnvVar.Store(map[Feature]bool{})
	fg.enabledViaSetMethod = map[Feature]bool{}

	return fg
}

// envVarFeatureGates implements Gates and allows for feature registration.
type envVarFeatureGates struct {
	// callSiteName holds the name of the file
	// that created this instance
	callSiteName string

	// readEnvVarsOnce guards reading environmental variables
	readEnvVarsOnce sync.Once

	// known holds known feature gates
	known map[Feature]FeatureSpec

	// enabledViaEnvVar holds a map[Feature]bool
	// with values explicitly set via env var
	enabledViaEnvVar atomic.Value

	// lockEnabledViaSetMethod protects enabledViaSetMethod
	lockEnabledViaSetMethod sync.RWMutex

	// enabledViaSetMethod holds values explicitly set
	// via Set method, features stored in this map take
	// precedence over features stored in enabledViaEnvVar
	enabledViaSetMethod map[Feature]bool

	// readEnvVars holds the boolean value which
	// indicates whether readEnvVarsOnce has been called.
	readEnvVars atomic.Bool
}

// Enabled returns true if the key is enabled. If the key is not known, this call will panic.
func (f *envVarFeatureGates) Enabled(key Feature) bool {
	if v, ok := f.wasFeatureEnabledViaSetMethod(key); ok {
		// ensue that the state of all known features
		// is loaded from environment variables
		// on the first call to Enabled method.
		if !f.hasAlreadyReadEnvVar() {
			_ = f.getEnabledMapFromEnvVar()
		}
		return v
	}
	if v, ok := f.getEnabledMapFromEnvVar()[key]; ok {
		return v
	}
	if v, ok := f.known[key]; ok {
		return v.Default
	}
	panic(fmt.Errorf("feature %q is not registered in FeatureGates %q", key, f.callSiteName))
}

// Set sets the given feature to the given value.
//
// Features set via this method take precedence over
// the features set via environment variables.
func (f *envVarFeatureGates) Set(featureName Feature, featureValue bool) error {
	feature, ok := f.known[featureName]
	if !ok {
		return fmt.Errorf("feature %q is not registered in FeatureGates %q", featureName, f.callSiteName)
	}
	if feature.LockToDefault && feature.Default != featureValue {
		return fmt.Errorf("cannot set feature gate %q to %v, feature is locked to %v", featureName, featureValue, feature.Default)
	}

	f.lockEnabledViaSetMethod.Lock()
	defer f.lockEnabledViaSetMethod.Unlock()
	f.enabledViaSetMethod[featureName] = featureValue

	return nil
}

// getEnabledMapFromEnvVar will fill the enabled map on the first call.
// This is the only time a known feature can be set to a value
// read from the corresponding environmental variable.
func (f *envVarFeatureGates) getEnabledMapFromEnvVar() map[Feature]bool {
	f.readEnvVarsOnce.Do(func() {
		// This code does not really support contextual logging. Making it do so has huge
		// implications for several call chains because the Enabled call then needs
		// a `*WithLogger` variant. This does not matter in Kubernetes itself because
		// all Kubernetes components replace the feature gate implementation used
		// by client-go, but it might matter elsewhere.
		logger := klog.Background()

		featureGatesState := map[Feature]bool{}
		for feature, featureSpec := range f.known {
			featureState, featureStateSet := os.LookupEnv(fmt.Sprintf("KUBE_FEATURE_%s", feature))
			if !featureStateSet {
				continue
			}
			boolVal, boolErr := strconv.ParseBool(featureState)
			switch {
			case boolErr != nil:
				utilruntime.HandleErrorWithLogger(logger, boolErr, "Could not set feature gate", "feature", feature, "desiredState", featureState)
			case featureSpec.LockToDefault:
				if boolVal != featureSpec.Default {
					utilruntime.HandleErrorWithLogger(logger, nil, "Could not set feature gate, feature is locked", "feature", feature, "desiredState", featureState, "lockedState", featureSpec.Default)
					break
				}
				featureGatesState[feature] = featureSpec.Default
			default:
				featureGatesState[feature] = boolVal
			}
		}
		f.enabledViaEnvVar.Store(featureGatesState)
		f.readEnvVars.Store(true)

		for feature, featureSpec := range f.known {
			if featureState, ok := featureGatesState[feature]; ok {
				logger.V(1).Info("Feature gate updated state", "feature", feature, "enabled", featureState)
				continue
			}
			logger.V(1).Info("Feature gate default state", "feature", feature, "enabled", featureSpec.Default)
		}
	})
	return f.enabledViaEnvVar.Load().(map[Feature]bool)
}

func (f *envVarFeatureGates) wasFeatureEnabledViaSetMethod(key Feature) (bool, bool) {
	f.lockEnabledViaSetMethod.RLock()
	defer f.lockEnabledViaSetMethod.RUnlock()

	value, found := f.enabledViaSetMethod[key]
	return value, found
}

func (f *envVarFeatureGates) hasAlreadyReadEnvVar() bool {
	return f.readEnvVars.Load()
}
