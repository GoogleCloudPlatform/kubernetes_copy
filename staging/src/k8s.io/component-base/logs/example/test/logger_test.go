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

// Package logger_test demonstrates how to write unit tests with per-test
// output. Per-test output only works for code which supports contextual
// logging.
package logger_test

import (
	"flag"
	"os"
	"testing"

	"k8s.io/component-base/logs/example"
	"k8s.io/klog/v2/ktesting"
	// This import could be used to add command line flags for per-test
	// output with a default verbosity of 5. This example instead
	// uses a TestMain where the default verbosity gets lowered.
	// "k8s.io/klog/v2/ktesting/init"
)

func TestLogger(t *testing.T) {
	// Produce some output in two different tests which run in parallel.
	for _, testcase := range []string{"abc", "xyz"} {
		t.Run(testcase, func(t *testing.T) {
			t.Parallel()
			_ /* logger */, ctx := ktesting.NewTestContext(t)
			example.Run(ctx)
		})
	}
}

func TestMain(m *testing.M) {
	// Run with verbosity 2, this is the default log level in production.
	// The name of the flags also could be customized here. The defaults
	// are -testing.v and -testing.vmodule.
	ktesting.DefaultConfig = ktesting.NewConfig(ktesting.Verbosity(2))
	ktesting.DefaultConfig.AddFlags(flag.CommandLine)
	flag.Parse()
	os.Exit(m.Run())
}
