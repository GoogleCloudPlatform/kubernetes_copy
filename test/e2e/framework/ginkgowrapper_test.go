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

package framework_test

import (
	"fmt"
	"testing"

	"k8s.io/kubernetes/test/e2e/framework"
	"k8s.io/kubernetes/test/e2e/framework/internal/unittests/features"
)

func TestTagsEqual(t *testing.T) {
	testcases := []struct {
		a, b        interface{}
		expectEqual bool
	}{
		{1, 2, false},
		{2, 2, false},
		{framework.WithSlow(), 2, false},
		{framework.WithSlow(), framework.WithSerial(), false},
		{framework.WithSerial(), framework.WithSlow(), false},
		{framework.WithSlow(), framework.WithSlow(), true},
		{framework.WithSerial(), framework.WithSerial(), true},
		{framework.WithLabel("hello"), framework.WithLabel("world"), false},
		{framework.WithLabel("hello"), framework.WithLabel("hello"), true},
		{framework.WithFeatureGate(features.Test), framework.WithLabel("Test"), false},
		{framework.WithFeatureGate(features.Test), framework.WithFeatureGate(features.Test), true},
	}

	for _, tc := range testcases {
		t.Run(fmt.Sprintf("%v=%v", tc.a, tc.b), func(t *testing.T) {
			actualEqual := framework.TagsEqual(tc.a, tc.b)
			if actualEqual != tc.expectEqual {
				t.Errorf("expected %v, got %v", tc.expectEqual, actualEqual)
			}
		})
	}
}

var (
	failCalled               bool
	failMessage              string
	deferCleanupCalled       bool
	originalFail             = framework.Fail
	originalDeferCleanup     = framework.DeferCleanup
	originalIsFailed         = framework.IsFailed
	originalCleanupOnFailure bool
)

// mockFail replaces framework.Fail to prevent ginkgo panic.
func mockFail(message string, _ ...int) {
	failCalled = true
	failMessage = message
}

// mockDeferCleanupAlways replaces framework.DeferCleanup to prevent ginkgo cleanup usage outside of ginkgo context.
// It always calls passed cleanup function.
func mockDeferCleanupAlways(args ...interface{}) {
	deferCleanupCalled = true
	if len(args) > 0 {
		if fn, ok := args[0].(func()); ok {
			fn()
		}
	}
}

// mockDeferCleanupAlways replaces framework.DeferCleanup to prevent ginkgo cleanup usage outside of ginkgo context.
// It follows the logic expected in framework.DeferConditionalCleanup.
func mockDeferCleanupOnFailure(args ...interface{}) {
	if framework.TestContext.CleanupOnFailure || !framework.IsFailed() {
		deferCleanupCalled = true
		if len(args) > 0 {
			if fn, ok := args[0].(func()); ok {
				fn()
			}
		}
	}
}

// mockIsFailedAlways replaces framework.IsFailed to prevent ginkgo usage outside of ginkgo context.
// It always returns true.
func mockIsFailedAlways() bool {
	return true
}

func resetFrameworkState() {
	failCalled = false
	failMessage = ""
	deferCleanupCalled = false
	framework.Fail = originalFail
	framework.DeferCleanup = originalDeferCleanup
	framework.TestContext.CleanupOnFailure = originalCleanupOnFailure
	framework.IsFailed = originalIsFailed
}

func TestDeferConditionalCleanup(t *testing.T) {
	originalCleanupOnFailure = framework.TestContext.CleanupOnFailure
	framework.Fail = mockFail
	framework.DeferCleanup = mockDeferCleanupAlways
	defer resetFrameworkState()

	tests := []struct {
		name          string
		args          []interface{}
		setup         func()
		expectFail    bool
		expectMessage string
		expectCleanup bool
	}{
		{
			name:          "calls Fail when no arguments provided",
			args:          nil,
			expectFail:    true,
			expectMessage: "DeferConditionalCleanup called with no arguments",
		},
		{
			name:          "calls Fail when first argument is not a function",
			args:          []interface{}{"not-a-function"},
			expectFail:    true,
			expectMessage: "The first argument to DeferConditionalCleanup should be a function",
		},
		{
			name: "executes cleanup function with arguments",
			args: []interface{}{
				func(arg string) {
					t.Logf("arg is %q", arg)
				},
				"test-arg",
			},
			expectCleanup: true,
		},
		{
			name: "executes cleanup function without arguments",
			args: []interface{}{
				func() {
					t.Log("called without arguments")
				},
			},
			expectCleanup: true,
		},
		{
			name: "executes cleanup when it's wrapped into a function with different count of arguments",
			args: []interface{}{
				framework.IgnoreNotFound(func(arg1 string, arg2 int) {
					t.Logf("arg1 is %q and arg2 is %d", arg1, arg2)
				}),
				"test-arg1", 42,
			},
			expectCleanup: true,
		},
		{
			name: "skips cleanup when test fails and CleanupOnFailure is false",
			args: []interface{}{
				func() {
					t.Error("cleanup should not run")
				},
			},
			setup: func() {
				framework.TestContext.CleanupOnFailure = false
				framework.IsFailed = mockIsFailedAlways
				framework.DeferCleanup = mockDeferCleanupOnFailure
			},
			expectCleanup: false,
		},
		{
			name: "runs cleanup when test fails and CleanupOnFailure is true",
			args: []interface{}{
				func() {
					t.Log("cleanup ran successfully")
				},
			},
			setup: func() {
				framework.TestContext.CleanupOnFailure = true
				framework.IsFailed = mockIsFailedAlways
			},
			expectCleanup: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			failCalled = false
			failMessage = ""
			deferCleanupCalled = false
			if tt.setup != nil {
				tt.setup()
			}

			framework.DeferConditionalCleanup(tt.args...)

			if tt.expectFail {
				if !failCalled {
					t.Errorf("expected Fail to be called, but it wasn't")
				}
				if failMessage != tt.expectMessage {
					t.Errorf("expected Fail message %q, but got %q", tt.expectMessage, failMessage)
				}
			} else if failCalled {
				t.Errorf("didn't expect Fail to be called, but it was with message: %q", failMessage)
			}

			if tt.expectCleanup {
				if !deferCleanupCalled {
					t.Errorf("expected DeferCleanup to be called, but it wasn't")
				}
			} else if deferCleanupCalled {
				t.Errorf("expected DeferCleanup to be skipped, but it was called")
			}
		})
	}
}
