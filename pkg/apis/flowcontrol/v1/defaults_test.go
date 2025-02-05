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
	"reflect"
	"testing"

	"github.com/google/go-cmp/cmp"

	flowcontrolv1 "k8s.io/api/flowcontrol/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
)

func TestDefaultWithPriorityLevelConfiguration(t *testing.T) {
	tests := []struct {
		name     string
		original runtime.Object
		expected runtime.Object
	}{
		{
			name: "Defaulting for Exempt",
			original: &flowcontrolv1.PriorityLevelConfiguration{
				Spec: flowcontrolv1.PriorityLevelConfigurationSpec{
					Type:   flowcontrolv1.PriorityLevelEnablementExempt,
					Exempt: &flowcontrolv1.ExemptPriorityLevelConfiguration{},
				},
			},
			expected: &flowcontrolv1.PriorityLevelConfiguration{
				Spec: flowcontrolv1.PriorityLevelConfigurationSpec{
					Type: flowcontrolv1.PriorityLevelEnablementExempt,
					Exempt: &flowcontrolv1.ExemptPriorityLevelConfiguration{
						NominalConcurrencyShares: ptr.To(int32(0)),
						LendablePercent:          ptr.To(int32(0)),
					},
				},
			},
		},
		{
			name: "LendablePercent is not specified in Limited, should default to zero",
			original: &flowcontrolv1.PriorityLevelConfiguration{
				Spec: flowcontrolv1.PriorityLevelConfigurationSpec{
					Type: flowcontrolv1.PriorityLevelEnablementLimited,
					Limited: &flowcontrolv1.LimitedPriorityLevelConfiguration{
						NominalConcurrencyShares: ptr.To(int32(5)),
						LimitResponse: flowcontrolv1.LimitResponse{
							Type: flowcontrolv1.LimitResponseTypeReject,
						},
						Weight: ptr.To(int32(42)),
					},
				},
			},
			expected: &flowcontrolv1.PriorityLevelConfiguration{
				Spec: flowcontrolv1.PriorityLevelConfigurationSpec{
					Type: flowcontrolv1.PriorityLevelEnablementLimited,
					Limited: &flowcontrolv1.LimitedPriorityLevelConfiguration{
						NominalConcurrencyShares: ptr.To(int32(5)),
						LendablePercent:          ptr.To(int32(0)),
						LimitResponse: flowcontrolv1.LimitResponse{
							Type: flowcontrolv1.LimitResponseTypeReject,
						},
						Weight: ptr.To(int32(42)),
					},
				},
			},
		},
		{
			name: "Weight is not specified in Limited, should get default value",
			original: &flowcontrolv1.PriorityLevelConfiguration{
				Spec: flowcontrolv1.PriorityLevelConfigurationSpec{
					Type: flowcontrolv1.PriorityLevelEnablementLimited,
					Limited: &flowcontrolv1.LimitedPriorityLevelConfiguration{
						NominalConcurrencyShares: ptr.To(int32(5)),
						LimitResponse: flowcontrolv1.LimitResponse{
							Type: flowcontrolv1.LimitResponseTypeReject,
						},
					},
				},
			},
			expected: &flowcontrolv1.PriorityLevelConfiguration{
				Spec: flowcontrolv1.PriorityLevelConfigurationSpec{
					Type: flowcontrolv1.PriorityLevelEnablementLimited,
					Limited: &flowcontrolv1.LimitedPriorityLevelConfiguration{
						NominalConcurrencyShares: ptr.To(int32(5)),
						LendablePercent:          ptr.To(int32(0)),
						LimitResponse: flowcontrolv1.LimitResponse{
							Type: flowcontrolv1.LimitResponseTypeReject,
						},
						Weight: ptr.To(LimitedPriorityLevelConfigurationDefaultWeight),
					},
				},
			},
		},
		{
			name: "NominalConcurrencyShares is not specified in Limited, should default to 30",
			original: &flowcontrolv1.PriorityLevelConfiguration{
				Spec: flowcontrolv1.PriorityLevelConfigurationSpec{
					Type: flowcontrolv1.PriorityLevelEnablementLimited,
					Limited: &flowcontrolv1.LimitedPriorityLevelConfiguration{
						NominalConcurrencyShares: nil,
						LimitResponse: flowcontrolv1.LimitResponse{
							Type: flowcontrolv1.LimitResponseTypeReject,
						},
						Weight: ptr.To(int32(42)),
					},
				},
			},
			expected: &flowcontrolv1.PriorityLevelConfiguration{
				Spec: flowcontrolv1.PriorityLevelConfigurationSpec{
					Type: flowcontrolv1.PriorityLevelEnablementLimited,
					Limited: &flowcontrolv1.LimitedPriorityLevelConfiguration{
						NominalConcurrencyShares: ptr.To(int32(PriorityLevelConfigurationDefaultNominalConcurrencyShares)),
						LendablePercent:          ptr.To(int32(0)),
						LimitResponse: flowcontrolv1.LimitResponse{
							Type: flowcontrolv1.LimitResponseTypeReject,
						},
						Weight: ptr.To(int32(42)),
					},
				},
			},
		},
		{
			name: "NominalConcurrencyShares is set to zero in Limited, no defaulting should be applied",
			original: &flowcontrolv1.PriorityLevelConfiguration{
				Spec: flowcontrolv1.PriorityLevelConfigurationSpec{
					Type: flowcontrolv1.PriorityLevelEnablementLimited,
					Limited: &flowcontrolv1.LimitedPriorityLevelConfiguration{
						NominalConcurrencyShares: ptr.To(int32(0)),
						LimitResponse: flowcontrolv1.LimitResponse{
							Type: flowcontrolv1.LimitResponseTypeReject,
						},
						Weight: ptr.To(int32(42)),
					},
				},
			},
			expected: &flowcontrolv1.PriorityLevelConfiguration{
				Spec: flowcontrolv1.PriorityLevelConfigurationSpec{
					Type: flowcontrolv1.PriorityLevelEnablementLimited,
					Limited: &flowcontrolv1.LimitedPriorityLevelConfiguration{
						NominalConcurrencyShares: ptr.To(int32(0)),
						LendablePercent:          ptr.To(int32(0)),
						LimitResponse: flowcontrolv1.LimitResponse{
							Type: flowcontrolv1.LimitResponseTypeReject,
						},
						Weight: ptr.To(int32(42)),
					},
				},
			},
		},
	}

	scheme := runtime.NewScheme()
	if err := AddToScheme(scheme); err != nil {
		t.Fatalf("Failed to add to scheme: %v", err)
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			original := test.original
			expected := test.expected

			scheme.Default(original)
			if !reflect.DeepEqual(expected, original) {
				t.Errorf("Expected defaulting to work - diff: %s", cmp.Diff(expected, original))
			}
		})
	}
}
