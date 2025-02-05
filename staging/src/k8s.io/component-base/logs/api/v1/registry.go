/*
Copyright 2020 The Kubernetes Authors.

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

package v1

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/go-logr/logr"

	"k8s.io/apimachinery/pkg/util/version"
	"k8s.io/component-base/featuregate"
)

var logRegistry = newLogFormatRegistry()

// logFormatRegistry stores factories for all supported logging formats.
type logFormatRegistry struct {
	mutex    sync.Mutex
	registry map[string]logFormat
	frozen   bool
}

type logFormat struct {
	factory LogFormatFactory
	// Stability by version.
	// Guaranteed to have at least one entry.
	versionedFeatures []featuregate.VersionedFeature
}

// +k8s:deepcopy-gen=false

// RuntimeControl provides operations that aren't available through the normal
// Logger or LogSink API.
type RuntimeControl struct {
	// Flush ensures that all in-memory data is written.
	// May be nil.
	Flush func()

	// SetVerbosityLevel changes the level for all Logger instances
	// derived from the initial one. May be nil.
	//
	// The parameter is intentionally a plain uint32 instead of
	// VerbosityLevel to enable implementations that don't need to import
	// the API (helps avoid circular dependencies).
	SetVerbosityLevel func(v uint32) error
}

// LogFormatFactory provides support for a certain additional,
// non-default log format.
type LogFormatFactory interface {
	// Create returns a logger with the requested configuration.
	Create(c LoggingConfiguration, o LoggingOptions) (logr.Logger, RuntimeControl)
}

// RegisterLogFormat registers support for a new logging format. This must be called
// before using any of the methods in LoggingConfiguration. The feature must
// be one of those defined in this package (typically LoggingAlphaOptions,
// LoggingBetaOptions or LoggingStableOptions). It represents the stability of
// the logging format in the current Kubernetes release.
//
// Optionally, information about stability in previous Kubernetes releases
// may be provided with additional parameters. This enables compatibility
// version support (https://github.com/kubernetes/enhancements/issues/4330).
//
// Example:
// RegisterLogFormat("json", ..., LoggingStableOptions,
// VersionedFeature{Version: *version.MajorMinor(1, 19), Feature: LoggingAlphaOptions},
// VersionedFeature{Version: *version.MajorMinor(1, 23), Feature: LoggingBetaOptions})
func RegisterLogFormat(name string, factory LogFormatFactory, feature featuregate.Feature, versionedFeatures ...featuregate.VersionedFeature) error {

	if len(versionedFeatures) == 0 {
		versionedFeatures = append(versionedFeatures, featuregate.VersionedFeature{
			Version: version.MajorMinor(0, 0),
			Feature: feature,
		})
	}

	format := logFormat{
		factory:           factory,
		versionedFeatures: versionedFeatures,
	}

	return logRegistry.register(name, format)
}

func newLogFormatRegistry() *logFormatRegistry {
	registry := &logFormatRegistry{
		registry: make(map[string]logFormat),
		frozen:   false,
	}
	_ = registry.register(DefaultLogFormat, logFormat{factory: textFactory{}, versionedFeatures: []featuregate.VersionedFeature{
		{
			Version: version.MajorMinor(0, 0),
			Feature: LoggingStableOptions,
		},
	}})
	return registry
}

// register adds a new log format. It's an error to modify an existing one.
func (lfr *logFormatRegistry) register(name string, format logFormat) error {
	lfr.mutex.Lock()
	defer lfr.mutex.Unlock()
	if lfr.frozen {
		return fmt.Errorf("log format registry is frozen, unable to register log format %s", name)
	}
	if _, ok := lfr.registry[name]; ok {
		return fmt.Errorf("log format: %s already exists", name)
	}
	for _, f := range format.versionedFeatures {
		if _, ok := featureGates()[f.Feature]; !ok && f.Feature != LoggingStableOptions {
			return fmt.Errorf("log format %s: unsupported feature gate %s", name, f.Feature)
		}
	}

	lfr.registry[name] = format
	return nil
}

// get specified log format factory
func (lfr *logFormatRegistry) get(name string) (*logFormat, error) {
	lfr.mutex.Lock()
	defer lfr.mutex.Unlock()
	format, ok := lfr.registry[name]
	if !ok {
		return nil, fmt.Errorf("log format: %s does not exists", name)
	}
	return &format, nil
}

// list names of registered log formats, including feature gates (sorted)
func (lfr *logFormatRegistry) list() string {
	lfr.mutex.Lock()
	defer lfr.mutex.Unlock()
	formats := make([]string, 0, len(lfr.registry))
	for name, format := range lfr.registry {
		item := fmt.Sprintf(`"%s"`, name)
		for _, f := range format.versionedFeatures {
			fullItem := fmt.Sprintf("%s (gated by %s since %s)", item, f.Feature, f.Version)
			formats = append(formats, fullItem)
		}

	}
	sort.Strings(formats)
	return strings.Join(formats, ", ")
}

// freeze prevents further modifications of the registered log formats.
func (lfr *logFormatRegistry) freeze() {
	lfr.mutex.Lock()
	defer lfr.mutex.Unlock()
	lfr.frozen = true
}
