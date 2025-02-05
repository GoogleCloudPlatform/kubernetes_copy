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
	"k8s.io/code-generator/cmd/defaulter-gen/output_tests/empty"
)

type IntTest struct {
	empty.TypeMeta
	// Test defaulting an int with feature gate
	// +default=5
	// +featureGate=WinDSR
	IntField int64
}

type MultipleGatesIntTest struct {
	empty.TypeMeta
	// Test defaulting an int with feature gate
	// +default=5
	// +featureGate=WinDSR
	// +featureGate=WinOverlay
	IntField int64
}

type StringTest struct {
	empty.TypeMeta
	// Test defaulting a string with feature gate
	// +default="foo"
	// +featureGate=WinDSR
	StringField string
}

type StringPtrTest struct {
	empty.TypeMeta
	// Test defaulting a pointer type with feature gate
	// +default="bar"
	// +featureGate=WinDSR
	StringPtrField *string
}

type NestedStructTest struct {
	empty.TypeMeta
	// Test nested default with feature gate
	// +default={"Value": "nested-default"}
	// +featureGate=WinDSR
	Nested *NestedStruct
}

type NestedStruct struct {
	Value string
}
