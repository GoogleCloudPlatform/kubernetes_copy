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
	"bytes"
	"testing"

	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/component-base/featuregate"
	logsapi "k8s.io/component-base/logs/api/v1"
	"k8s.io/klog/v2"
)

func TestJSONFlag(t *testing.T) {
	c := logsapi.NewLoggingConfiguration()
	fs := pflag.NewFlagSet("addflagstest", pflag.ContinueOnError)
	output := bytes.Buffer{}
	logsapi.AddFlags(c, fs)
	fs.SetOutput(&output)
	fs.PrintDefaults()
	wantSubstring := `Permitted formats: "json" (gated by LoggingBetaOptions since 1.23), "text"`
	// Uncomment the following when promoting JSON to GA
	// wantSubstring := `Permitted formats: "json" (gated by LoggingBetaOptions since 1.23), "json" (gated by LoggingStableOptions since 1.33), "text"`
	if !assert.Contains(t, output.String(), wantSubstring) {
		t.Errorf("JSON logging format flag is not available. expect to contain %q, got %q", wantSubstring, output.String())
	}
}

func TestJSONFormatRegister(t *testing.T) {
	config := logsapi.NewLoggingConfiguration()
	klogr := klog.Background()
	defaultGate := featuregate.NewFeatureGate()
	err := logsapi.AddFeatureGates(defaultGate)
	require.NoError(t, err)
	allEnabled := defaultGate.DeepCopy()
	allDisabled := defaultGate.DeepCopy()
	for feature := range defaultGate.GetAll() {
		if err := allEnabled.SetFromMap(map[string]bool{string(feature): true}); err != nil {
			panic(err)
		}
		if err := allDisabled.SetFromMap(map[string]bool{string(feature): false}); err != nil {
			panic(err)
		}
	}
	testcases := []struct {
		name              string
		args              []string
		contextualLogging bool
		featureGate       featuregate.FeatureGate
		want              *logsapi.LoggingConfiguration
		errs              field.ErrorList
	}{
		{
			name: "JSON log format, default gates",
			args: []string{"--logging-format=json"},
			want: func() *logsapi.LoggingConfiguration {
				c := config.DeepCopy()
				c.Format = logsapi.JSONLogFormat
				return c
			}(),
		},
		{
			name:        "JSON log format, disabled gates",
			args:        []string{"--logging-format=json"},
			featureGate: allDisabled,
			want: func() *logsapi.LoggingConfiguration {
				c := config.DeepCopy()
				c.Format = logsapi.JSONLogFormat
				return c
			}(),
			errs: field.ErrorList{&field.Error{
				Type:     "FieldValueForbidden",
				Field:    "format",
				BadValue: "",
				Detail:   "Log format json is disabled, see LoggingBetaOptions feature",
			}},
		},
		{
			name:        "JSON log format, enabled gates",
			args:        []string{"--logging-format=json"},
			featureGate: allEnabled,
			want: func() *logsapi.LoggingConfiguration {
				c := config.DeepCopy()
				c.Format = logsapi.JSONLogFormat
				return c
			}(),
		},
		{
			name: "JSON log format",
			args: []string{"--logging-format=json"},
			want: func() *logsapi.LoggingConfiguration {
				c := config.DeepCopy()
				c.Format = logsapi.JSONLogFormat
				return c
			}(),
		},
		{
			name:              "JSON direct",
			args:              []string{"--logging-format=json"},
			contextualLogging: true,
			want: func() *logsapi.LoggingConfiguration {
				c := config.DeepCopy()
				c.Format = logsapi.JSONLogFormat
				return c
			}(),
		},
		{
			name: "Unsupported log format",
			args: []string{"--logging-format=test"},
			want: func() *logsapi.LoggingConfiguration {
				c := config.DeepCopy()
				c.Format = "test"
				return c
			}(),
			errs: field.ErrorList{&field.Error{
				Type:     "FieldValueInvalid",
				Field:    "format",
				BadValue: "test",
				Detail:   "Unsupported log format",
			}},
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			state := klog.CaptureState()
			defer state.Restore()
			c := logsapi.NewLoggingConfiguration()
			fs := pflag.NewFlagSet("addflagstest", pflag.ContinueOnError)
			logsapi.AddFlags(c, fs)
			fs.Parse(tc.args)
			if !assert.Equal(t, tc.want, c) {
				t.Errorf("Wrong Validate() result for %q. expect %v, got %v", tc.name, tc.want, c)
			}
			featureGate := tc.featureGate
			if featureGate == nil {
				featureGate = defaultGate
			}
			mutable := featureGate.DeepCopy()
			err := mutable.SetFromMap(map[string]bool{string(logsapi.ContextualLogging): tc.contextualLogging})
			require.NoError(t, err)
			featureGate = mutable
			defer func() {
				if err := logsapi.ResetForTest(featureGate); err != nil {
					t.Errorf("Unexpected error while resetting the logging configuration: %v", err)
				}
			}()
			errs := logsapi.ValidateAndApply(c, featureGate)
			if !assert.ElementsMatch(t, tc.errs, errs) {
				t.Errorf("Wrong Validate() result for %q.\n expect:\t%+v\n got:\t%+v", tc.name, tc.errs, errs)

			}
			currentLogger := klog.Background()
			isKlogr := currentLogger == klogr
			if tc.contextualLogging && isKlogr {
				t.Errorf("Expected to get zapr as logger, got: %T", currentLogger)
			}
			if !tc.contextualLogging && !isKlogr {
				t.Errorf("Expected to get klogr as logger, got: %T", currentLogger)
			}
		})
	}
}
