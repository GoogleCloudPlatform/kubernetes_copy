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

package compatibility

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/spf13/pflag"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/version"
	cliflag "k8s.io/component-base/cli/flag"
	"k8s.io/component-base/featuregate"
	"k8s.io/klog/v2"
)

const (
	// DefaultKubeComponent is the component name for k8s control plane components.
	DefaultKubeComponent = "kube"

	klogLevel = 2
)

type VersionMapping func(from *version.Version) *version.Version

// ComponentGlobals stores the global variables for a component for easy access, including feature gate and effective version.
type ComponentGlobals struct {
	effectiveVersion MutableEffectiveVersion
	featureGate      featuregate.MutableVersionedFeatureGate

	// emulationVersionMapping contains the mapping from the emulation version of this component
	// to the emulation version of another component.
	emulationVersionMapping map[string]VersionMapping
	// dependentEmulationVersion stores whether or not this component's EmulationVersion is dependent through mapping on another component.
	// If true, the emulation version cannot be set from the flag, or version mapping from another component.
	dependentEmulationVersion bool
	// minCompatibilityVersionMapping contains the mapping from the min compatibility version of this component
	// to the min compatibility version of another component.
	minCompatibilityVersionMapping map[string]VersionMapping
	// dependentMinCompatibilityVersion stores whether or not this component's MinCompatibilityVersion is dependent through mapping on another component
	// If true, the min compatibility version cannot be set from the flag, or version mapping from another component.
	dependentMinCompatibilityVersion bool
}

// ComponentGlobalsRegistry stores the global variables for different components for easy access, including feature gate and effective version of each component.
type ComponentGlobalsRegistry interface {
	// EffectiveVersionFor returns the EffectiveVersion registered under the component.
	// Returns nil if the component is not registered.
	EffectiveVersionFor(component string) EffectiveVersion
	// FeatureGateFor returns the FeatureGate registered under the component.
	// Returns nil if the component is not registered.
	FeatureGateFor(component string) featuregate.FeatureGate
	// Register registers the EffectiveVersion and FeatureGate for a component.
	// returns error if the component is already registered.
	Register(component string, effectiveVersion MutableEffectiveVersion, featureGate featuregate.MutableVersionedFeatureGate) error
	// ComponentGlobalsOrRegister would return the registered global variables for the component if it already exists in the registry.
	// Otherwise, the provided variables would be registered under the component, and the same variables would be returned.
	ComponentGlobalsOrRegister(component string, effectiveVersion MutableEffectiveVersion, featureGate featuregate.MutableVersionedFeatureGate) (MutableEffectiveVersion, featuregate.MutableVersionedFeatureGate)
	// AddFlags adds flags of "--emulated-version" and "--feature-gates"
	AddFlags(fs *pflag.FlagSet)
	// Set sets the flags for all global variables for all components registered.
	// A component's feature gate and effective version would not be updated until Set() is called.
	Set() error
	// SetFallback calls Set() if it has never been called.
	SetFallback() error
	// Validate calls the Validate() function for all the global variables for all components registered.
	Validate() []error
	// Reset removes all stored ComponentGlobals, configurations, and version mappings.
	Reset()
	// SetEmulationVersionMapping sets the mapping from the emulation version of one component
	// to the emulation version of another component.
	// Once set, the emulation version of the toComponent will be determined by the emulation version of the fromComponent,
	// and cannot be set from cmd flags anymore.
	// For a given component, its emulation version can only depend on one other component, no multiple dependency is allowed.
	SetEmulationVersionMapping(fromComponent, toComponent string, f VersionMapping) error
}

type componentGlobalsRegistry struct {
	componentGlobals map[string]*ComponentGlobals
	mutex            sync.RWMutex
	// emulationVersionConfig stores the list of component name to emulation version set from the flag.
	// When the `--emulated-version` flag is parsed, it would not take effect until Set() is called,
	// because the emulation version needs to be set before the feature gate is set.
	emulationVersionConfig []string
	// featureGatesConfig stores the map of component name to the list of feature gates set from the flag.
	// When the `--feature-gates` flag is parsed, it would not take effect until Set() is called,
	// because the emulation version needs to be set before the feature gate is set.
	featureGatesConfig map[string][]string
	// set stores if the Set() function for the registry is already called.
	set bool
}

func NewComponentGlobalsRegistry() *componentGlobalsRegistry {
	return &componentGlobalsRegistry{
		componentGlobals:       make(map[string]*ComponentGlobals),
		emulationVersionConfig: nil,
		featureGatesConfig:     nil,
	}
}

func (r *componentGlobalsRegistry) Reset() {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	r.componentGlobals = make(map[string]*ComponentGlobals)
	r.emulationVersionConfig = nil
	r.featureGatesConfig = nil
	r.set = false
}

func (r *componentGlobalsRegistry) EffectiveVersionFor(component string) EffectiveVersion {
	r.mutex.RLock()
	defer r.mutex.RUnlock()
	globals, ok := r.componentGlobals[component]
	if !ok {
		return nil
	}
	return globals.effectiveVersion
}

func (r *componentGlobalsRegistry) FeatureGateFor(component string) featuregate.FeatureGate {
	r.mutex.RLock()
	defer r.mutex.RUnlock()
	globals, ok := r.componentGlobals[component]
	if !ok {
		return nil
	}
	return globals.featureGate
}

func (r *componentGlobalsRegistry) unsafeRegister(component string, effectiveVersion MutableEffectiveVersion, featureGate featuregate.MutableVersionedFeatureGate) error {
	if _, ok := r.componentGlobals[component]; ok {
		return fmt.Errorf("component globals of %s already registered", component)
	}
	if featureGate != nil {
		if err := featureGate.SetEmulationVersion(effectiveVersion.EmulationVersion()); err != nil {
			return err
		}
	}
	c := ComponentGlobals{
		effectiveVersion:               effectiveVersion,
		featureGate:                    featureGate,
		emulationVersionMapping:        make(map[string]VersionMapping),
		minCompatibilityVersionMapping: make(map[string]VersionMapping),
	}
	r.componentGlobals[component] = &c
	return nil
}

func (r *componentGlobalsRegistry) Register(component string, effectiveVersion MutableEffectiveVersion, featureGate featuregate.MutableVersionedFeatureGate) error {
	if effectiveVersion == nil {
		return fmt.Errorf("cannot register nil effectiveVersion")
	}
	r.mutex.Lock()
	defer r.mutex.Unlock()
	return r.unsafeRegister(component, effectiveVersion, featureGate)
}

func (r *componentGlobalsRegistry) ComponentGlobalsOrRegister(component string, effectiveVersion MutableEffectiveVersion, featureGate featuregate.MutableVersionedFeatureGate) (MutableEffectiveVersion, featuregate.MutableVersionedFeatureGate) {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	globals, ok := r.componentGlobals[component]
	if ok {
		return globals.effectiveVersion, globals.featureGate
	}
	utilruntime.Must(r.unsafeRegister(component, effectiveVersion, featureGate))
	return effectiveVersion, featureGate
}

func (r *componentGlobalsRegistry) unsafeKnownFeatures() []string {
	var known []string
	for component, globals := range r.componentGlobals {
		if globals.featureGate == nil {
			continue
		}
		for _, f := range globals.featureGate.KnownFeatures() {
			known = append(known, component+":"+f)
		}
	}
	sort.Strings(known)
	return known
}

func (r *componentGlobalsRegistry) unsafeVersionFlagOptions(isEmulation bool) []string {
	var vs []string
	for component, globals := range r.componentGlobals {
		if isEmulation {
			if globals.dependentEmulationVersion {
				continue
			}
			vs = append(vs, fmt.Sprintf("%s=%s", component, globals.effectiveVersion.AllowedEmulationVersionRange()))
		} else {
			if globals.dependentMinCompatibilityVersion {
				continue
			}
			vs = append(vs, fmt.Sprintf("%s=%s", component, globals.effectiveVersion.AllowedMinCompatibilityVersionRange()))
		}
	}
	sort.Strings(vs)
	return vs
}

func (r *componentGlobalsRegistry) AddFlags(fs *pflag.FlagSet) {
	if r == nil {
		return
	}
	r.mutex.Lock()
	defer r.mutex.Unlock()
	for _, globals := range r.componentGlobals {
		if globals.featureGate != nil {
			globals.featureGate.Close()
		}
	}
	if r.emulationVersionConfig != nil || r.featureGatesConfig != nil {
		klog.Warning("calling componentGlobalsRegistry.AddFlags more than once, the registry will be set by the latest flags")
	}
	r.emulationVersionConfig = []string{}
	r.featureGatesConfig = make(map[string][]string)

	fs.StringSliceVar(&r.emulationVersionConfig, "emulated-version", r.emulationVersionConfig, ""+
		"The versions different components emulate their capabilities (APIs, features, ...) of.\n"+
		"If set, the component will emulate the behavior of this version instead of the underlying binary version.\n"+
		"Version format could only be major.minor, for example: '--emulated-version=wardle=1.2,kube=1.31'. Options are:\n"+strings.Join(r.unsafeVersionFlagOptions(true), "\n")+
		"If the component is not specified, defaults to \"kube\"")

	fs.Var(cliflag.NewColonSeparatedMultimapStringStringAllowDefaultEmptyKey(&r.featureGatesConfig), "feature-gates", "Comma-separated list of component:key=value pairs that describe feature gates for alpha/experimental features of different components.\n"+
		"If the component is not specified, defaults to \"kube\". This flag can be repeatedly invoked. For example: --feature-gates 'wardle:featureA=true,wardle:featureB=false' --feature-gates 'kube:featureC=true'"+
		"Options are:\n"+strings.Join(r.unsafeKnownFeatures(), "\n"))
}

type componentVersion struct {
	component string
	ver       *version.Version
}

// getFullEmulationVersionConfig expands the given version config with version registered version mapping,
// and returns the map of component to Version.
func (r *componentGlobalsRegistry) getFullEmulationVersionConfig(
	versionConfigMap map[string]*version.Version) (map[string]*version.Version, error) {
	result := map[string]*version.Version{}
	setQueue := []componentVersion{}
	for comp, ver := range versionConfigMap {
		if _, ok := r.componentGlobals[comp]; !ok {
			return result, fmt.Errorf("component not registered: %s", comp)
		}
		klog.V(klogLevel).Infof("setting version %s=%s", comp, ver.String())
		setQueue = append(setQueue, componentVersion{comp, ver})
	}
	for len(setQueue) > 0 {
		cv := setQueue[0]
		if _, visited := result[cv.component]; visited {
			return result, fmt.Errorf("setting version of %s more than once, probably version mapping loop", cv.component)
		}
		setQueue = setQueue[1:]
		result[cv.component] = cv.ver
		for toComp, f := range r.componentGlobals[cv.component].emulationVersionMapping {
			toVer := f(cv.ver)
			if toVer == nil {
				return result, fmt.Errorf("got nil version from mapping of %s=%s to component:%s", cv.component, cv.ver.String(), toComp)
			}
			klog.V(klogLevel).Infof("setting version %s=%s from version mapping of %s=%s", toComp, toVer.String(), cv.component, cv.ver.String())
			setQueue = append(setQueue, componentVersion{toComp, toVer})
		}
	}
	return result, nil
}

func toVersionMap(versionConfig []string) (map[string]*version.Version, error) {
	m := map[string]*version.Version{}
	for _, compVer := range versionConfig {
		// default to "kube" of component is not specified
		k := "kube"
		v := compVer
		if strings.Contains(compVer, "=") {
			arr := strings.SplitN(compVer, "=", 2)
			if len(arr) != 2 {
				return m, fmt.Errorf("malformed pair, expect string=string")
			}
			k = strings.TrimSpace(arr[0])
			v = strings.TrimSpace(arr[1])
		}
		ver, err := version.Parse(v)
		if err != nil {
			return m, err
		}
		if ver.Patch() != 0 {
			return m, fmt.Errorf("patch version not allowed, got: %s=%s", k, ver.String())
		}
		if existingVer, ok := m[k]; ok {
			return m, fmt.Errorf("duplicate version flag, %s=%s and %s=%s", k, existingVer.String(), k, ver.String())
		}
		m[k] = ver
	}
	return m, nil
}

func (r *componentGlobalsRegistry) SetFallback() error {
	r.mutex.Lock()
	set := r.set
	r.mutex.Unlock()
	if set {
		return nil
	}
	klog.Warning("setting componentGlobalsRegistry in SetFallback. We recommend calling componentGlobalsRegistry.Set()" +
		" right after parsing flags to avoid using feature gates before their final values are set by the flags.")
	return r.Set()
}

func (r *componentGlobalsRegistry) Set() error {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	r.set = true
	emulationVersionConfigMap, err := toVersionMap(r.emulationVersionConfig)
	if err != nil {
		return err
	}
	for comp := range emulationVersionConfigMap {
		if _, ok := r.componentGlobals[comp]; !ok {
			return fmt.Errorf("component not registered: %s", comp)
		}
		// only components without any dependencies can be set from the flag.
		if r.componentGlobals[comp].dependentEmulationVersion {
			return fmt.Errorf("EmulationVersion of %s is set by mapping, cannot set it by flag", comp)
		}
	}
	if emulationVersions, err := r.getFullEmulationVersionConfig(emulationVersionConfigMap); err != nil {
		return err
	} else {
		for comp, ver := range emulationVersions {
			r.componentGlobals[comp].effectiveVersion.SetEmulationVersion(ver)
		}
	}
	// Set feature gate emulation version before setting feature gate flag values.
	for comp, globals := range r.componentGlobals {
		if globals.featureGate == nil {
			continue
		}
		klog.V(klogLevel).Infof("setting %s:feature gate emulation version to %s", comp, globals.effectiveVersion.EmulationVersion().String())
		if err := globals.featureGate.SetEmulationVersion(globals.effectiveVersion.EmulationVersion()); err != nil {
			return err
		}
	}
	for comp, fg := range r.featureGatesConfig {
		if comp == "" {
			if _, ok := r.featureGatesConfig[DefaultKubeComponent]; ok {
				return fmt.Errorf("set kube feature gates with default empty prefix or kube: prefix consistently, do not mix use")
			}
			comp = DefaultKubeComponent
		}
		if _, ok := r.componentGlobals[comp]; !ok {
			return fmt.Errorf("component not registered: %s", comp)
		}
		featureGate := r.componentGlobals[comp].featureGate
		if featureGate == nil {
			return fmt.Errorf("component featureGate not registered: %s", comp)
		}
		flagVal := strings.Join(fg, ",")
		klog.V(klogLevel).Infof("setting %s:feature-gates=%s", comp, flagVal)
		if err := featureGate.Set(flagVal); err != nil {
			return err
		}
	}
	return nil
}

func (r *componentGlobalsRegistry) Validate() []error {
	var errs []error
	r.mutex.Lock()
	defer r.mutex.Unlock()
	for _, globals := range r.componentGlobals {
		errs = append(errs, globals.effectiveVersion.Validate()...)
		if globals.featureGate != nil {
			errs = append(errs, globals.featureGate.Validate()...)
		}
	}
	return errs
}

func (r *componentGlobalsRegistry) SetEmulationVersionMapping(fromComponent, toComponent string, f VersionMapping) error {
	if f == nil {
		return nil
	}
	klog.V(klogLevel).Infof("setting EmulationVersion mapping from %s to %s", fromComponent, toComponent)
	r.mutex.Lock()
	defer r.mutex.Unlock()
	if _, ok := r.componentGlobals[fromComponent]; !ok {
		return fmt.Errorf("component not registered: %s", fromComponent)
	}
	if _, ok := r.componentGlobals[toComponent]; !ok {
		return fmt.Errorf("component not registered: %s", toComponent)
	}
	// check multiple dependency
	if r.componentGlobals[toComponent].dependentEmulationVersion {
		return fmt.Errorf("mapping of %s already exists from another component", toComponent)
	}
	r.componentGlobals[toComponent].dependentEmulationVersion = true

	versionMapping := r.componentGlobals[fromComponent].emulationVersionMapping
	if _, ok := versionMapping[toComponent]; ok {
		return fmt.Errorf("EmulationVersion from %s to %s already exists", fromComponent, toComponent)
	}
	versionMapping[toComponent] = f
	klog.V(klogLevel).Infof("setting the default EmulationVersion of %s based on mapping from the default EmulationVersion of %s", fromComponent, toComponent)
	defaultFromVersion := r.componentGlobals[fromComponent].effectiveVersion.EmulationVersion()
	emulationVersions, err := r.getFullEmulationVersionConfig(map[string]*version.Version{fromComponent: defaultFromVersion})
	if err != nil {
		return err
	}
	for comp, ver := range emulationVersions {
		r.componentGlobals[comp].effectiveVersion.SetEmulationVersion(ver)
	}
	return nil
}
