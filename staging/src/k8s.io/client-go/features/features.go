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
	"context"
	"sync/atomic"

	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
)

// NOTE: types Feature, FeatureSpec, prerelease (and its values)
// were duplicated from the component-base repository
//
// for more information please refer to https://docs.google.com/document/d/1g9BGCRw-7ucUxO6OtCWbb3lfzUGA_uU9178wLdXAIfs

const (
	// Values for PreRelease.
	Alpha = prerelease("ALPHA")
	Beta  = prerelease("BETA")
	GA    = prerelease("")

	// Deprecated
	Deprecated = prerelease("DEPRECATED")
)

type prerelease string

type Feature string

type FeatureSpec struct {
	// Default is the default enablement state for the feature
	Default bool
	// LockToDefault indicates that the feature is locked to its default and cannot be changed
	LockToDefault bool
	// PreRelease indicates the maturity level of the feature
	PreRelease prerelease
}

// Gates indicates whether a given feature is enabled or not.
type Gates interface {
	// Enabled returns true if the key is enabled.
	Enabled(key Feature) bool
}

// Registry represents an external feature gates registry.
type Registry interface {
	// Add adds existing feature gates to the provided registry.
	//
	// As of today, this method is used by AddFeaturesToExistingFeatureGates and
	// ReplaceFeatureGates to take control of the features exposed by this library.
	Add(map[Feature]FeatureSpec) error
}

// FeatureGates returns the feature gates exposed by this library.
//
// By default, only the default features gates will be returned.
// The default implementation allows controlling the features
// via environmental variables.
// For example, if you have a feature named "MyFeature"
// setting an environmental variable "KUBE_FEATURE_MyFeature"
// will allow you to configure the state of that feature.
//
// Please note that the actual set of the feature gates
// might be overwritten by calling ReplaceFeatureGates method.
func FeatureGates() Gates {
	return featureGates.Load().(*featureGatesWrapper).Gates
}

// AddFeaturesToExistingFeatureGates adds the default feature gates to the provided registry.
// Usually this function is combined with ReplaceFeatureGates to take control of the
// features exposed by this library.
func AddFeaturesToExistingFeatureGates(registry Registry) error {
	return registry.Add(defaultKubernetesFeatureGates)
}

// ReplaceFeatureGates overwrites the default implementation of the feature gates
// used by this library.
//
// Useful for binaries that would like to have full control of the features
// exposed by this library, such as allowing consumers of a binary
// to interact with the features via a command line flag.
//
// For example:
//
//	// first, register client-go's features to your registry.
//	clientgofeaturegate.AddFeaturesToExistingFeatureGates(utilfeature.DefaultMutableFeatureGate)
//	// then replace client-go's feature gates implementation with your implementation
//	clientgofeaturegate.ReplaceFeatureGates(utilfeature.DefaultMutableFeatureGate)
func ReplaceFeatureGates(newFeatureGates Gates) {
	if replaceFeatureGatesWithWarningIndicator(newFeatureGates) {
		// TODO (?): A new API would be needed where callers pass in a context or logger.
		// Probably not worth it.
		utilruntime.HandleErrorWithContext(context.TODO(), nil, "The default feature gates implementation has already been used and now it's being overwritten. This might lead to unexpected behaviour. Check your initialization order.")
	}
}

func replaceFeatureGatesWithWarningIndicator(newFeatureGates Gates) bool {
	shouldProduceWarning := false

	if defaultFeatureGates, ok := FeatureGates().(*envVarFeatureGates); ok {
		if defaultFeatureGates.hasAlreadyReadEnvVar() {
			shouldProduceWarning = true
		}
	}
	wrappedFeatureGates := &featureGatesWrapper{newFeatureGates}
	featureGates.Store(wrappedFeatureGates)

	return shouldProduceWarning
}

func init() {
	envVarGates := newEnvVarFeatureGates(defaultKubernetesFeatureGates)

	wrappedFeatureGates := &featureGatesWrapper{envVarGates}
	featureGates.Store(wrappedFeatureGates)
}

// featureGatesWrapper a thin wrapper to satisfy featureGates variable (atomic.Value).
// That is, all calls to Store for a given Value must use values of the same concrete type.
type featureGatesWrapper struct {
	Gates
}

var (
	// featureGates is a shared global FeatureGates.
	//
	// Top-level commands/options setup that needs to modify this feature gates
	// should use AddFeaturesToExistingFeatureGates followed by ReplaceFeatureGates.
	featureGates = &atomic.Value{}
)
