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

package featuregate

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/component-base/featuregate"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	"k8s.io/kubernetes/pkg/features"
)

func TestFeatureGatedDefaultingInt(t *testing.T) {
	testcases := []struct {
		name        string
		featureGate featuregate.Feature
		enabled     bool
		in          IntTest
		out         IntTest
	}{
		{
			name:        "int - enabled",
			featureGate: features.WinDSR,
			enabled:     true,
			in:          IntTest{},
			out:         IntTest{IntField: 5},
		},
		{
			name:        "int - disabled",
			featureGate: features.WinDSR,
			enabled:     false,
			in:          IntTest{},
			out:         IntTest{},
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, tc.featureGate, tc.enabled)

			SetObjectDefaults_IntTest(&tc.in)
			if diff := cmp.Diff(tc.out, tc.in); diff != "" {
				t.Errorf("mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestFeatureGatedDefaultingMultipleGatesInt(t *testing.T) {
	testcases := []struct {
		name           string
		featureGateMap map[featuregate.Feature]bool
		in             MultipleGatesIntTest
		out            MultipleGatesIntTest
	}{
		{
			name: "multiple gates int - one of two gates enabled",
			featureGateMap: map[featuregate.Feature]bool{
				features.WinDSR:     true,
				features.WinOverlay: false,
			},
			in:  MultipleGatesIntTest{},
			out: MultipleGatesIntTest{},
		},
		{
			name: "multiple gates int - both gates enabled",
			featureGateMap: map[featuregate.Feature]bool{
				features.WinDSR:     true,
				features.WinOverlay: true,
			},
			in:  MultipleGatesIntTest{},
			out: MultipleGatesIntTest{IntField: 5},
		},
		{
			name: "multiple gates int - all gates disabled",
			featureGateMap: map[featuregate.Feature]bool{
				features.WinDSR:     false,
				features.WinOverlay: false,
			},
			in:  MultipleGatesIntTest{},
			out: MultipleGatesIntTest{},
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			for gate, enabled := range tc.featureGateMap {
				featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, gate, enabled)
			}

			SetObjectDefaults_MultipleGatesIntTest(&tc.in)
			if diff := cmp.Diff(tc.out, tc.in); diff != "" {
				t.Errorf("mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestFeatureGatedDefaultingString(t *testing.T) {
	testcases := []struct {
		name        string
		featureGate featuregate.Feature
		enabled     bool
		in          StringTest
		out         StringTest
	}{
		{
			name:        "string - enabled",
			featureGate: features.WinDSR,
			enabled:     true,
			in:          StringTest{},
			out:         StringTest{StringField: "foo"},
		},
		{
			name:        "string - disabled",
			featureGate: features.WinDSR,
			enabled:     false,
			in:          StringTest{},
			out:         StringTest{},
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, tc.featureGate, tc.enabled)

			SetObjectDefaults_StringTest(&tc.in)
			if diff := cmp.Diff(tc.out, tc.in); diff != "" {
				t.Errorf("mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func stringPointer(s string) *string {
	return &s
}

func TestFeatureGatedDefaultingStringPtr(t *testing.T) {
	testcases := []struct {
		name        string
		featureGate featuregate.Feature
		enabled     bool
		in          StringPtrTest
		out         StringPtrTest
	}{
		{
			name:        "string pointer - enabled",
			featureGate: features.WinDSR,
			enabled:     true,
			in:          StringPtrTest{},
			out:         StringPtrTest{StringPtrField: stringPointer("bar")},
		},
		{
			name:        "string pointer - disabled",
			featureGate: features.WinDSR,
			enabled:     false,
			in:          StringPtrTest{},
			out:         StringPtrTest{},
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, tc.featureGate, tc.enabled)

			SetObjectDefaults_StringPtrTest(&tc.in)
			if diff := cmp.Diff(tc.out, tc.in); diff != "" {
				t.Errorf("mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestFeatureGatedDefaultingNestedStruct(t *testing.T) {
	testcases := []struct {
		name        string
		featureGate featuregate.Feature
		enabled     bool
		in          NestedStructTest
		out         NestedStructTest
	}{
		{
			name:        "nested - enabled",
			featureGate: features.WinDSR,
			enabled:     true,
			in:          NestedStructTest{},
			out:         NestedStructTest{Nested: &NestedStruct{Value: "nested-default"}},
		},
		{
			name:        "nested - disabled",
			featureGate: features.WinDSR,
			enabled:     false,
			in:          NestedStructTest{},
			out:         NestedStructTest{},
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, tc.featureGate, tc.enabled)

			SetObjectDefaults_NestedStructTest(&tc.in)
			if diff := cmp.Diff(tc.out, tc.in); diff != "" {
				t.Errorf("mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
