/*
Copyright 2022 The Kubernetes Authors.

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

package metrics

import (
	"context"
	"strings"
	"testing"
	"time"

	"k8s.io/component-base/metrics/legacyregistry"
	"k8s.io/component-base/metrics/testutil"
)

type metricsObserver func()

func TestNoUtils(t *testing.T) {

	metrics := []string{
		"apiserver_validating_admission_policy_check_total",
		"apiserver_validating_admission_policy_check_duration_seconds",
		"apiserver_validating_admission_policy_active_policies",
		"apiserver_validating_admission_policy_active_bindings",
		"apiserver_validating_admission_policy_active_bindings_enforcement",
	}

	testCases := []struct {
		desc     string
		want     string
		observer metricsObserver
	}{
		{
			desc: "observe policy admission",
			want: `
			# HELP apiserver_validating_admission_policy_active_policies [ALPHA] Validation admission policy active policies
			# TYPE apiserver_validating_admission_policy_active_policies gauge
			apiserver_validating_admission_policy_active_policies 0
			# HELP apiserver_validating_admission_policy_check_duration_seconds [BETA] Validation admission latency for individual validation expressions in seconds, labeled by policy and further including binding and enforcement action taken, result type, namespace and name of the associated parameter resource, and index of the result.
			# TYPE apiserver_validating_admission_policy_check_duration_seconds histogram
			apiserver_validating_admission_policy_check_duration_seconds_bucket{enforcement_action="allow",error_type="invalid_error",param_name="param_name",param_namespace="param_namespace",policy="policy.example.com",policy_binding="binding.example.com",result_index="0",result_type="validation",le="5e-07"} 0
			apiserver_validating_admission_policy_check_duration_seconds_bucket{enforcement_action="allow",error_type="invalid_error",param_name="param_name",param_namespace="param_namespace",policy="policy.example.com",policy_binding="binding.example.com",result_index="0",result_type="validation",le="0.001"} 0
			apiserver_validating_admission_policy_check_duration_seconds_bucket{enforcement_action="allow",error_type="invalid_error",param_name="param_name",param_namespace="param_namespace",policy="policy.example.com",policy_binding="binding.example.com",result_index="0",result_type="validation",le="0.01"} 0
			apiserver_validating_admission_policy_check_duration_seconds_bucket{enforcement_action="allow",error_type="invalid_error",param_name="param_name",param_namespace="param_namespace",policy="policy.example.com",policy_binding="binding.example.com",result_index="0",result_type="validation",le="0.1"} 0
			apiserver_validating_admission_policy_check_duration_seconds_bucket{enforcement_action="allow",error_type="invalid_error",param_name="param_name",param_namespace="param_namespace",policy="policy.example.com",policy_binding="binding.example.com",result_index="0",result_type="validation",le="1"} 0
			apiserver_validating_admission_policy_check_duration_seconds_bucket{enforcement_action="allow",error_type="invalid_error",param_name="param_name",param_namespace="param_namespace",policy="policy.example.com",policy_binding="binding.example.com",result_index="0",result_type="validation",le="+Inf"} 1
			apiserver_validating_admission_policy_check_duration_seconds_sum{enforcement_action="allow",error_type="invalid_error",param_name="param_name",param_namespace="param_namespace",policy="policy.example.com",policy_binding="binding.example.com",result_index="0",result_type="validation"} 10
			apiserver_validating_admission_policy_check_duration_seconds_count{enforcement_action="allow",error_type="invalid_error",param_name="param_name",param_namespace="param_namespace",policy="policy.example.com",policy_binding="binding.example.com",result_index="0",result_type="validation"} 1
			# HELP apiserver_validating_admission_policy_check_total [BETA] Validation admission policy check total, labeled by policy and further identified by binding, enforcement action taken, result type, namespace and name of the associated parameter resource, and index of the result.
			# TYPE apiserver_validating_admission_policy_check_total counter
			apiserver_validating_admission_policy_check_total{enforcement_action="allow",error_type="invalid_error",param_name="param_name",param_namespace="param_namespace",policy="policy.example.com",policy_binding="binding.example.com",result_index="0",result_type="validation"} 1
			`,
			observer: func() {
				Metrics.ObserveAdmission(context.TODO(), time.Duration(10)*time.Second, "policy.example.com", "binding.example.com", ValidatingInvalidError,
					"validation", "param_namespace", "param_name", 0)
			},
		},
		{
			desc: "observe policy rejection",
			want: `
			# HELP apiserver_validating_admission_policy_active_policies [ALPHA] Validation admission policy active policies
			# TYPE apiserver_validating_admission_policy_active_policies gauge
			apiserver_validating_admission_policy_active_policies 0
			# HELP apiserver_validating_admission_policy_check_duration_seconds [BETA] Validation admission latency for individual validation expressions in seconds, labeled by policy and further including binding and enforcement action taken, result type, namespace and name of the associated parameter resource, and index of the result.
			# TYPE apiserver_validating_admission_policy_check_duration_seconds histogram
			apiserver_validating_admission_policy_check_duration_seconds_bucket{enforcement_action="deny",error_type="invalid_error",param_name="param_name",param_namespace="param_namespace",policy="policy.example.com",policy_binding="binding.example.com",result_index="0",result_type="validation",le="5e-07"} 0
			apiserver_validating_admission_policy_check_duration_seconds_bucket{enforcement_action="deny",error_type="invalid_error",param_name="param_name",param_namespace="param_namespace",policy="policy.example.com",policy_binding="binding.example.com",result_index="0",result_type="validation",le="0.001"} 0
			apiserver_validating_admission_policy_check_duration_seconds_bucket{enforcement_action="deny",error_type="invalid_error",param_name="param_name",param_namespace="param_namespace",policy="policy.example.com",policy_binding="binding.example.com",result_index="0",result_type="validation",le="0.01"} 0
			apiserver_validating_admission_policy_check_duration_seconds_bucket{enforcement_action="deny",error_type="invalid_error",param_name="param_name",param_namespace="param_namespace",policy="policy.example.com",policy_binding="binding.example.com",result_index="0",result_type="validation",le="0.1"} 0
			apiserver_validating_admission_policy_check_duration_seconds_bucket{enforcement_action="deny",error_type="invalid_error",param_name="param_name",param_namespace="param_namespace",policy="policy.example.com",policy_binding="binding.example.com",result_index="0",result_type="validation",le="1"} 0
			apiserver_validating_admission_policy_check_duration_seconds_bucket{enforcement_action="deny",error_type="invalid_error",param_name="param_name",param_namespace="param_namespace",policy="policy.example.com",policy_binding="binding.example.com",result_index="0",result_type="validation",le="+Inf"} 1
			apiserver_validating_admission_policy_check_duration_seconds_sum{enforcement_action="deny",error_type="invalid_error",param_name="param_name",param_namespace="param_namespace",policy="policy.example.com",policy_binding="binding.example.com",result_index="0",result_type="validation"} 10
			apiserver_validating_admission_policy_check_duration_seconds_count{enforcement_action="deny",error_type="invalid_error",param_name="param_name",param_namespace="param_namespace",policy="policy.example.com",policy_binding="binding.example.com",result_index="0",result_type="validation"} 1
			# HELP apiserver_validating_admission_policy_check_total [BETA] Validation admission policy check total, labeled by policy and further identified by binding, enforcement action taken, result type, namespace and name of the associated parameter resource, and index of the result.
			# TYPE apiserver_validating_admission_policy_check_total counter
			apiserver_validating_admission_policy_check_total{enforcement_action="deny",error_type="invalid_error",param_name="param_name",param_namespace="param_namespace",policy="policy.example.com",policy_binding="binding.example.com",result_index="0",result_type="validation"} 1
			`,
			observer: func() {
				Metrics.ObserveRejection(context.TODO(), time.Duration(10)*time.Second, "policy.example.com", "binding.example.com", ValidatingInvalidError,
					"validation", "param_namespace", "param_name", 0)
			},
		},
		{
			desc: "observe active bindings",
			want: `
            # HELP apiserver_validating_admission_policy_active_bindings_enforcement [ALPHA] Validation admission policy active bindings, labeled by enforcement action.
            # TYPE apiserver_validating_admission_policy_active_bindings_enforcement gauge
            apiserver_validating_admission_policy_active_bindings_enforcement{enforcement_action="allow"} 5
            apiserver_validating_admission_policy_active_bindings_enforcement{enforcement_action="deny"} 11
            apiserver_validating_admission_policy_active_bindings_enforcement{enforcement_action="warn"} 7
            # HELP apiserver_validating_admission_policy_active_policies [ALPHA] Validation admission policy active policies
            # TYPE apiserver_validating_admission_policy_active_policies gauge
            apiserver_validating_admission_policy_active_policies 13
			`,
			observer: func() {
				Metrics.ObserveActiveBindings(13, 5, 7, 11)
			},
		},
	}

	Metrics.Register()
	Metrics.Reset()

	for _, tt := range testCases {
		t.Run(tt.desc, func(t *testing.T) {
			defer Metrics.Reset()
			tt.observer()
			if err := testutil.GatherAndCompare(legacyregistry.DefaultGatherer, strings.NewReader(tt.want), metrics...); err != nil {
				t.Fatal(err)
			}
		})
	}
}
