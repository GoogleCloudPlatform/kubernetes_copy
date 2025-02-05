/*
Copyright 2021 The Kubernetes Authors.

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

package register

import (
	"k8s.io/apimachinery/pkg/util/version"
	"k8s.io/component-base/featuregate"
	logsapi "k8s.io/component-base/logs/api/v1"
	json "k8s.io/component-base/logs/json"
)

func init() {
	// JSON format is optional klog format
	if err := logsapi.RegisterLogFormat(logsapi.JSONLogFormat, json.Factory{}, logsapi.LoggingStableOptions,
		// TODO: when promoting JSON to GA, keep this entry for
		// three more releases because of version emulation,
		// then remove it.
		featuregate.VersionedFeature{Version: version.MajorMinor(1, 23), Feature: logsapi.LoggingBetaOptions},
	// Uncomment the following line to promote the feature to GA.
	// logsapi.VersionedFeature{Version: version.MajorMinor(1, 33), Feature: logsapi.LoggingStableOptions}
	); err != nil {
		panic(err)
	}
}
