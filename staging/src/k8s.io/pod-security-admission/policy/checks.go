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

package policy

import (
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/pod-security-admission/api"
)

type Check struct {
	// ID is the unique ID of the check.
	ID CheckID
	// Level is the policy level this check belongs to.
	// Must be Baseline or Restricted.
	// Baseline checks are evaluated for baseline and restricted namespaces.
	// Restricted checks are only evaluated for restricted namespaces.
	Level api.Level
	// Versions contains one or more revisions of the check that apply to different versions.
	// If the check is not yet assigned to a version, this must be a single-item list with a MinimumVersion of "".
	// Otherwise, MinimumVersion of items must represent strictly increasing versions.
	Versions []VersionedCheck
}

type VersionedCheck struct {
	// MinimumVersion is the first policy version this check applies to.
	// If unset, this check is not yet assigned to a policy version.
	// If set, must not be "latest".
	MinimumVersion api.Version
	// CheckPod determines if the pod is allowed.
	CheckPod CheckPodFn
	// OverrideCheckIDs is an optional list of checks that should be skipped when this check is run.
	// Overrides may only be set on restricted checks, and may only override baseline checks.
	OverrideCheckIDs []CheckID
}

type CheckPodFn func(*metav1.ObjectMeta, *corev1.PodSpec) CheckResult

type CheckID string

// CheckResult contains the result of checking a pod and indicates whether the pod is allowed,
// and if not, why it was forbidden.
//
// Example output for (false, "host ports", "8080, 9090"):
//
//	When checking all pods in a namespace:
//	  disallowed by policy "baseline": host ports, privileged containers, non-default capabilities
//	When checking an individual pod:
//	  disallowed by policy "baseline": host ports (8080, 9090), privileged containers, non-default capabilities (CAP_NET_RAW)
type CheckResult struct {
	// Allowed indicates if the check allowed the pod.
	Allowed bool
	// ForbiddenReason must be set if Allowed is false.
	// ForbiddenReason should be as succinct as possible and is always output.
	// Examples:
	// - "host ports"
	// - "privileged containers"
	// - "non-default capabilities"
	ForbiddenReason string
	// ForbiddenDetail should only be set if Allowed is false, and is optional.
	// ForbiddenDetail can include specific values that were disallowed and is used when checking an individual object.
	// Examples:
	// - list specific invalid host ports: "8080, 9090"
	// - list specific invalid containers: "container1, container2"
	// - list specific non-default capabilities: "CAP_NET_RAW"
	ForbiddenDetail string
}

// AggregateCheckResult holds the aggregate result of running CheckPod across multiple checks.
type AggregateCheckResult struct {
	// Allowed indicates if all checks allowed the pod.
	Allowed bool
	// ForbiddenReasons is a slice of the forbidden reasons from all the forbidden checks. It should not include empty strings.
	// ForbiddenReasons and ForbiddenDetails must have the same number of elements, and the indexes are for the same check.
	ForbiddenReasons []string
	// ForbiddenDetails is a slice of the forbidden details from all the forbidden checks. It may include empty strings.
	// ForbiddenReasons and ForbiddenDetails must have the same number of elements, and the indexes are for the same check.
	ForbiddenDetails []string
}

// ForbiddenReason returns a comma-separated string of of the forbidden reasons.
// Example: host ports, privileged containers, non-default capabilities
func (a *AggregateCheckResult) ForbiddenReason() string {
	return strings.Join(a.ForbiddenReasons, ", ")
}

// ForbiddenDetail returns a detailed forbidden message, with non-empty details formatted in
// parentheses with the associated reason.
// Example: host ports (8080, 9090), privileged containers, non-default capabilities (NET_RAW)
func (a *AggregateCheckResult) ForbiddenDetail() string {
	var b strings.Builder
	for i := 0; i < len(a.ForbiddenReasons); i++ {
		b.WriteString(a.ForbiddenReasons[i])
		if a.ForbiddenDetails[i] != "" {
			b.WriteString(" (")
			b.WriteString(a.ForbiddenDetails[i])
			b.WriteString(")")
		}
		if i != len(a.ForbiddenReasons)-1 {
			b.WriteString(", ")
		}
	}
	return b.String()
}

// UnknownForbiddenReason is used as the placeholder forbidden reason for checks that incorrectly disallow without providing a reason.
const UnknownForbiddenReason = "unknown forbidden reason"

// AggregateCheckPod runs all the checks and aggregates the forbidden results into a single CheckResult.
// The aggregated reason is a comma-separated
func AggregateCheckResults(results []CheckResult) AggregateCheckResult {
	var (
		reasons []string
		details []string
	)
	for _, result := range results {
		if !result.Allowed {
			if len(result.ForbiddenReason) == 0 {
				reasons = append(reasons, UnknownForbiddenReason)
			} else {
				reasons = append(reasons, result.ForbiddenReason)
			}
			details = append(details, result.ForbiddenDetail)
		}
	}
	return AggregateCheckResult{
		Allowed:          len(reasons) == 0,
		ForbiddenReasons: reasons,
		ForbiddenDetails: details,
	}
}

var (
	defaultChecks      []func() Check
	experimentalChecks []func() Check
)

func addCheck(f func() Check) {
	// add to experimental or versioned list
	c := f()
	if len(c.Versions) == 1 && c.Versions[0].MinimumVersion == (api.Version{}) {
		experimentalChecks = append(experimentalChecks, f)
	} else {
		defaultChecks = append(defaultChecks, f)
	}
}

// DefaultChecks returns checks that are expected to be enabled by default.
// The results are mutually exclusive with ExperimentalChecks.
// It returns a new copy of checks on each invocation and is expected to be called once at setup time.
func DefaultChecks() []Check {
	retval := make([]Check, 0, len(defaultChecks))
	for _, f := range defaultChecks {
		retval = append(retval, f())
	}
	return retval
}

// ExperimentalChecks returns checks that have not yet been assigned to policy versions.
// The results are mutually exclusive with DefaultChecks.
// It returns a new copy of checks on each invocation and is expected to be called once at setup time.
func ExperimentalChecks() []Check {
	retval := make([]Check, 0, len(experimentalChecks))
	for _, f := range experimentalChecks {
		retval = append(retval, f())
	}
	return retval
}
