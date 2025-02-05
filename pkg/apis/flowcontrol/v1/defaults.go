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

package v1

import (
	v1 "k8s.io/api/flowcontrol/v1"
	"k8s.io/utils/ptr"
)

// Default settings for flow-schema
const (
	FlowSchemaDefaultMatchingPrecedence int32 = 1000
)

// Default settings for priority-level-configuration
const (
	PriorityLevelConfigurationDefaultHandSize                 int32 = 8
	PriorityLevelConfigurationDefaultQueues                   int32 = 64
	PriorityLevelConfigurationDefaultQueueLengthLimit         int32 = 50
	PriorityLevelConfigurationDefaultNominalConcurrencyShares int32 = 30
	LimitedPriorityLevelConfigurationDefaultWeight            int32 = 30
)

// SetDefaults_FlowSchema sets default values for flow schema
func SetDefaults_FlowSchemaSpec(spec *v1.FlowSchemaSpec) {
	if spec.MatchingPrecedence == 0 {
		spec.MatchingPrecedence = FlowSchemaDefaultMatchingPrecedence
	}
}

func SetDefaults_ExemptPriorityLevelConfiguration(eplc *v1.ExemptPriorityLevelConfiguration) {
	if eplc.NominalConcurrencyShares == nil {
		eplc.NominalConcurrencyShares = new(int32)
		*eplc.NominalConcurrencyShares = 0
	}
	if eplc.LendablePercent == nil {
		eplc.LendablePercent = new(int32)
		*eplc.LendablePercent = 0
	}
}

func SetDefaults_LimitedPriorityLevelConfiguration(lplc *v1.LimitedPriorityLevelConfiguration) {
	if lplc.NominalConcurrencyShares == nil {
		lplc.NominalConcurrencyShares = ptr.To(PriorityLevelConfigurationDefaultNominalConcurrencyShares)
	}
	if lplc.LendablePercent == nil {
		lplc.LendablePercent = new(int32)
		*lplc.LendablePercent = 0
	}
	if lplc.Weight == nil {
		lplc.Weight = ptr.To(LimitedPriorityLevelConfigurationDefaultWeight)
	}
}

// SetDefaults_FlowSchema sets default values for flow schema
func SetDefaults_QueuingConfiguration(cfg *v1.QueuingConfiguration) {
	if cfg.HandSize == 0 {
		cfg.HandSize = PriorityLevelConfigurationDefaultHandSize
	}
	if cfg.Queues == 0 {
		cfg.Queues = PriorityLevelConfigurationDefaultQueues
	}
	if cfg.QueueLengthLimit == 0 {
		cfg.QueueLengthLimit = PriorityLevelConfigurationDefaultQueueLengthLimit
	}
}
