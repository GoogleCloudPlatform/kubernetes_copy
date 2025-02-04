/*
Copyright 2018 The Kubernetes Authors.

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

package options

import (
	"fmt"
	"strings"
	"testing"
	"time"

	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/version"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	basecompatibility "k8s.io/component-base/compatibility"
	netutils "k8s.io/utils/net"
)

func TestServerRunOptionsValidate(t *testing.T) {
	defaultComponentGlobalsRegistry := basecompatibility.NewComponentGlobalsRegistry()
	testRegistry := basecompatibility.NewComponentGlobalsRegistry()
	featureGate := utilfeature.DefaultFeatureGate.DeepCopy()
	effectiveVersion := basecompatibility.NewEffectiveVersionFromString("1.35", "1.32", "1.32")
	effectiveVersion.SetEmulationVersion(version.MajorMinor(1, 31))
	testComponent := "test"
	utilruntime.Must(testRegistry.Register(testComponent, effectiveVersion, featureGate))

	testCases := []struct {
		name        string
		testOptions *ServerRunOptions
		expectErr   string
	}{
		{
			name: "Test when MaxRequestsInFlight is negative value",
			testOptions: &ServerRunOptions{
				AdvertiseAddress:            netutils.ParseIPSloppy("192.168.10.10"),
				CorsAllowedOriginList:       []string{"10.10.10.100", "10.10.10.200"},
				MaxRequestsInFlight:         -400,
				MaxMutatingRequestsInFlight: 200,
				RequestTimeout:              time.Duration(2) * time.Minute,
				MinRequestTimeout:           1800,
				JSONPatchMaxCopyBytes:       10 * 1024 * 1024,
				MaxRequestBodyBytes:         10 * 1024 * 1024,
				ComponentGlobalsRegistry:    defaultComponentGlobalsRegistry,
			},
			expectErr: "--max-requests-inflight can not be negative value",
		},
		{
			name: "Test when MaxMutatingRequestsInFlight is negative value",
			testOptions: &ServerRunOptions{
				AdvertiseAddress:            netutils.ParseIPSloppy("192.168.10.10"),
				CorsAllowedOriginList:       []string{"10.10.10.100", "10.10.10.200"},
				MaxRequestsInFlight:         400,
				MaxMutatingRequestsInFlight: -200,
				RequestTimeout:              time.Duration(2) * time.Minute,
				MinRequestTimeout:           1800,
				JSONPatchMaxCopyBytes:       10 * 1024 * 1024,
				MaxRequestBodyBytes:         10 * 1024 * 1024,
				ComponentGlobalsRegistry:    defaultComponentGlobalsRegistry,
			},
			expectErr: "--max-mutating-requests-inflight can not be negative value",
		},
		{
			name: "Test when RequestTimeout is negative value",
			testOptions: &ServerRunOptions{
				AdvertiseAddress:            netutils.ParseIPSloppy("192.168.10.10"),
				CorsAllowedOriginList:       []string{"10.10.10.100", "10.10.10.200"},
				MaxRequestsInFlight:         400,
				MaxMutatingRequestsInFlight: 200,
				RequestTimeout:              -time.Duration(2) * time.Minute,
				MinRequestTimeout:           1800,
				JSONPatchMaxCopyBytes:       10 * 1024 * 1024,
				MaxRequestBodyBytes:         10 * 1024 * 1024,
				ComponentGlobalsRegistry:    defaultComponentGlobalsRegistry,
			},
			expectErr: "--request-timeout can not be negative value",
		},
		{
			name: "Test when MinRequestTimeout is negative value",
			testOptions: &ServerRunOptions{
				AdvertiseAddress:            netutils.ParseIPSloppy("192.168.10.10"),
				CorsAllowedOriginList:       []string{"10.10.10.100", "10.10.10.200"},
				MaxRequestsInFlight:         400,
				MaxMutatingRequestsInFlight: 200,
				RequestTimeout:              time.Duration(2) * time.Minute,
				MinRequestTimeout:           -1800,
				JSONPatchMaxCopyBytes:       10 * 1024 * 1024,
				MaxRequestBodyBytes:         10 * 1024 * 1024,
				ComponentGlobalsRegistry:    defaultComponentGlobalsRegistry,
			},
			expectErr: "--min-request-timeout can not be negative value",
		},
		{
			name: "Test when JSONPatchMaxCopyBytes is negative value",
			testOptions: &ServerRunOptions{
				AdvertiseAddress:            netutils.ParseIPSloppy("192.168.10.10"),
				CorsAllowedOriginList:       []string{"10.10.10.100", "10.10.10.200"},
				MaxRequestsInFlight:         400,
				MaxMutatingRequestsInFlight: 200,
				RequestTimeout:              time.Duration(2) * time.Minute,
				MinRequestTimeout:           1800,
				JSONPatchMaxCopyBytes:       -10 * 1024 * 1024,
				MaxRequestBodyBytes:         10 * 1024 * 1024,
				ComponentGlobalsRegistry:    defaultComponentGlobalsRegistry,
			},
			expectErr: "ServerRunOptions.JSONPatchMaxCopyBytes can not be negative value",
		},
		{
			name: "Test when MaxRequestBodyBytes is negative value",
			testOptions: &ServerRunOptions{
				AdvertiseAddress:            netutils.ParseIPSloppy("192.168.10.10"),
				CorsAllowedOriginList:       []string{"10.10.10.100", "10.10.10.200"},
				MaxRequestsInFlight:         400,
				MaxMutatingRequestsInFlight: 200,
				RequestTimeout:              time.Duration(2) * time.Minute,
				MinRequestTimeout:           1800,
				JSONPatchMaxCopyBytes:       10 * 1024 * 1024,
				MaxRequestBodyBytes:         -10 * 1024 * 1024,
				ComponentGlobalsRegistry:    defaultComponentGlobalsRegistry,
			},
			expectErr: "ServerRunOptions.MaxRequestBodyBytes can not be negative value",
		},
		{
			name: "Test when LivezGracePeriod is negative value",
			testOptions: &ServerRunOptions{
				AdvertiseAddress:            netutils.ParseIPSloppy("192.168.10.10"),
				CorsAllowedOriginList:       []string{"10.10.10.100", "10.10.10.200"},
				MaxRequestsInFlight:         400,
				MaxMutatingRequestsInFlight: 200,
				RequestTimeout:              time.Duration(2) * time.Minute,
				MinRequestTimeout:           1800,
				JSONPatchMaxCopyBytes:       10 * 1024 * 1024,
				MaxRequestBodyBytes:         10 * 1024 * 1024,
				LivezGracePeriod:            -time.Second,
				ComponentGlobalsRegistry:    defaultComponentGlobalsRegistry,
			},
			expectErr: "--livez-grace-period can not be a negative value",
		},
		{
			name: "Test when MinimalShutdownDuration is negative value",
			testOptions: &ServerRunOptions{
				AdvertiseAddress:            netutils.ParseIPSloppy("192.168.10.10"),
				CorsAllowedOriginList:       []string{"10.10.10.100", "10.10.10.200"},
				MaxRequestsInFlight:         400,
				MaxMutatingRequestsInFlight: 200,
				RequestTimeout:              time.Duration(2) * time.Minute,
				MinRequestTimeout:           1800,
				JSONPatchMaxCopyBytes:       10 * 1024 * 1024,
				MaxRequestBodyBytes:         10 * 1024 * 1024,
				ShutdownDelayDuration:       -time.Second,
				ComponentGlobalsRegistry:    defaultComponentGlobalsRegistry,
			},
			expectErr: "--shutdown-delay-duration can not be negative value",
		},
		{
			name: "Test when HSTSHeaders is valid",
			testOptions: &ServerRunOptions{
				AdvertiseAddress:            netutils.ParseIPSloppy("192.168.10.10"),
				CorsAllowedOriginList:       []string{"10.10.10.100", "10.10.10.200"},
				HSTSDirectives:              []string{"fakevalue", "includeSubDomains", "preload"},
				MaxRequestsInFlight:         400,
				MaxMutatingRequestsInFlight: 200,
				RequestTimeout:              time.Duration(2) * time.Minute,
				MinRequestTimeout:           1800,
				JSONPatchMaxCopyBytes:       10 * 1024 * 1024,
				MaxRequestBodyBytes:         10 * 1024 * 1024,
				ComponentGlobalsRegistry:    defaultComponentGlobalsRegistry,
			},
			expectErr: "--strict-transport-security-directives invalid, allowed values: max-age=expireTime, includeSubDomains, preload. see https://tools.ietf.org/html/rfc6797#section-6.1 for more information",
		},
		{
			name: "Test when emulation version is invalid",
			testOptions: &ServerRunOptions{
				AdvertiseAddress:            netutils.ParseIPSloppy("192.168.10.10"),
				CorsAllowedOriginList:       []string{"^10.10.10.100$", "^10.10.10.200$"},
				HSTSDirectives:              []string{"max-age=31536000", "includeSubDomains", "preload"},
				MaxRequestsInFlight:         400,
				MaxMutatingRequestsInFlight: 200,
				RequestTimeout:              time.Duration(2) * time.Minute,
				MinRequestTimeout:           1800,
				JSONPatchMaxCopyBytes:       10 * 1024 * 1024,
				MaxRequestBodyBytes:         10 * 1024 * 1024,
				ComponentName:               testComponent,
				ComponentGlobalsRegistry:    testRegistry,
			},
			expectErr: "emulation version 1.31 is not between [1.32, 1.35.0]",
		},
		{
			name: "Test when ServerRunOptions is valid",
			testOptions: &ServerRunOptions{
				AdvertiseAddress:            netutils.ParseIPSloppy("192.168.10.10"),
				CorsAllowedOriginList:       []string{"^10.10.10.100$", "^10.10.10.200$"},
				HSTSDirectives:              []string{"max-age=31536000", "includeSubDomains", "preload"},
				MaxRequestsInFlight:         400,
				MaxMutatingRequestsInFlight: 200,
				RequestTimeout:              time.Duration(2) * time.Minute,
				MinRequestTimeout:           1800,
				JSONPatchMaxCopyBytes:       10 * 1024 * 1024,
				MaxRequestBodyBytes:         10 * 1024 * 1024,
				ComponentGlobalsRegistry:    defaultComponentGlobalsRegistry,
			},
		},
	}

	for _, testcase := range testCases {
		t.Run(testcase.name, func(t *testing.T) {
			errs := testcase.testOptions.Validate()
			if len(testcase.expectErr) != 0 && !strings.Contains(utilerrors.NewAggregate(errs).Error(), testcase.expectErr) {
				t.Errorf("got err: %v, expected err: %s", errs, testcase.expectErr)
			}

			if len(testcase.expectErr) == 0 && len(errs) != 0 {
				t.Errorf("got err: %s, expected err nil", errs)
			}
		})
	}
}

func TestValidateCorsAllowedOriginList(t *testing.T) {
	tests := []struct {
		regexp           [][]string
		errShouldContain string
	}{
		{
			regexp: [][]string{
				{}, // empty list, the cluster operator wants to disable CORS
				{`^http://foo.com$`},
				{`^http://foo.com`}, // valid, because we relaxed the validation
				{`://foo.com$`},
				{`//foo.com$`},
				{`^http://foo.com(:|$)`},
				{`://foo.com(:|$)`},
				{`//foo.com(:|$)`},
				{`(^foo.com$)`},
				{`^http://foo.com$`, `//bar.com(:|$)`},
			},
			errShouldContain: "",
		},
		{
			// empty string, indicates that the cluster operator
			// specified --cors-allowed-origins=""
			regexp: [][]string{
				{`^http://foo.com$`, ``},
			},
			errShouldContain: "empty value in --cors-allowed-origins",
		},
		{
			regexp: [][]string{
				{`^foo.com`},
				{`//foo.com`},
				{`foo.com$`},
				{`foo.com(:|$)`},
			},
			errShouldContain: "regular expression does not pin to start/end of host in the origin header",
		},
		{
			regexp: [][]string{
				{`^http://foo.com$`, `^foo.com`}, // one good followed by a bad one
			},
			errShouldContain: "regular expression does not pin to start/end of host in the origin header",
		},
	}

	for _, test := range tests {
		for _, regexp := range test.regexp {
			t.Run(fmt.Sprintf("regexp/%s", regexp), func(t *testing.T) {
				options := NewServerRunOptions()
				if errs := options.Validate(); len(errs) != 0 {
					t.Fatalf("wrong test setup: %#v", errs)
				}

				options.CorsAllowedOriginList = regexp
				errsGot := options.Validate()
				switch {
				case len(test.errShouldContain) == 0:
					if len(errsGot) != 0 {
						t.Errorf("expected no error, but got: %v", errsGot)
					}
				default:
					if len(errsGot) == 0 ||
						!strings.Contains(utilerrors.NewAggregate(errsGot).Error(), test.errShouldContain) {
						t.Errorf("expected error to contain: %s, but got: %v", test.errShouldContain, errsGot)
					}
				}
			})
		}
	}
}

func TestServerRunOptionsWithShutdownWatchTerminationGracePeriod(t *testing.T) {
	tests := []struct {
		name             string
		optionsFn        func() *ServerRunOptions
		errShouldContain string
	}{
		{
			name: "default should be valid",
			optionsFn: func() *ServerRunOptions {
				return NewServerRunOptions()
			},
		},
		{
			name: "negative not allowed",
			optionsFn: func() *ServerRunOptions {
				o := NewServerRunOptions()
				o.ShutdownWatchTerminationGracePeriod = -time.Second
				return o
			},
			errShouldContain: "shutdown-watch-termination-grace-period, if provided, can not be a negative value",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			options := test.optionsFn()
			errsGot := options.Validate()
			switch {
			case len(test.errShouldContain) == 0:
				if len(errsGot) != 0 {
					t.Errorf("expected no error, but got: %v", errsGot)
				}
			default:
				if len(errsGot) == 0 ||
					!strings.Contains(utilerrors.NewAggregate(errsGot).Error(), test.errShouldContain) {
					t.Errorf("expected error to contain: %s, but got: %v", test.errShouldContain, errsGot)
				}
			}
		})
	}

	t.Run("default should be zero", func(t *testing.T) {
		options := NewServerRunOptions()
		if options.ShutdownWatchTerminationGracePeriod != time.Duration(0) {
			t.Errorf("expected default of ShutdownWatchTerminationGracePeriod to be zero, but got: %s", options.ShutdownWatchTerminationGracePeriod)
		}
	})
}
