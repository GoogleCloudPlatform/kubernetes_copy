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
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	basecompatibility "k8s.io/component-base/compatibility"
)

// DefaultComponentGlobalsRegistry is the global var to store the effective versions and feature gates for all components for easy access.
// Example usage:
// // register the component effective version and feature gate first
// wardleEffectiveVersion := basecompatibility.NewEffectiveVersion("1.2")
// wardleFeatureGate := featuregate.NewFeatureGate()
// utilruntime.Must(compatibility.DefaultComponentGlobalsRegistry.Register(apiserver.WardleComponentName, wardleEffectiveVersion, wardleFeatureGate, false))
//
//	cmd := &cobra.Command{
//	 ...
//		// call DefaultComponentGlobalsRegistry.Set() in PersistentPreRunE to ensure the feature gates are set based on emulation version right after parsing the flags.
//		PersistentPreRunE: func(*cobra.Command, []string) error {
//			if err := compatibility.DefaultComponentGlobalsRegistry.Set(); err != nil {
//				return err
//			}
//	 ...
//		},
//		RunE: func(c *cobra.Command, args []string) error {
//			// call compatibility.DefaultComponentGlobalsRegistry.Validate() somewhere
//		},
//	}
//
// flags := cmd.Flags()
// // add flags
// compatibility.DefaultComponentGlobalsRegistry.AddFlags(flags)
var DefaultComponentGlobalsRegistry basecompatibility.ComponentGlobalsRegistry = basecompatibility.NewComponentGlobalsRegistry()

func init() {
	utilruntime.Must(DefaultComponentGlobalsRegistry.Register(basecompatibility.DefaultKubeComponent, DefaultBuildEffectiveVersion(), utilfeature.DefaultMutableFeatureGate))
}
