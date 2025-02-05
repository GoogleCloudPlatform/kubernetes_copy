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

package job

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/kubernetes/pkg/apis/batch"
	"k8s.io/kubernetes/pkg/apis/core"
	"k8s.io/kubernetes/test/utils/ktesting"
	"k8s.io/utils/ptr"
)

// Define validSelector, a label selector for the job
// This is used to ensure that the jobs match a specific label set, necessary for managing
// job resources correctly in Kubernetes.
var (
	validSelector = &metav1.LabelSelector{
		MatchLabels: map[string]string{"a": "b"},
	}

	// validPodTemplate defines a valid pod template specification
	// This template includes basic metadata and specifications that define a Pod's behavior and resource requirements.
	// It's a template used in various test cases to simulate real-world job specifications.
	validPodTemplate = core.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Labels: validSelector.MatchLabels,
		},
		Spec: core.PodSpec{
			// RestartPolicy ensures that Pods are restarted on failure, which is typical for many batch jobs.
			RestartPolicy: core.RestartPolicyOnFailure,
			DNSPolicy:     core.DNSClusterFirst, // Standard DNS policy for Pods in Kubernetes clusters.
			Containers: []core.Container{
				{
					Name:  "abc", // Name of the container
					Image: "image", // The image to be used for the container
					// ImagePullPolicy specifies that the image should only be pulled if not present on the node.
					ImagePullPolicy:           "IfNotPresent",
					TerminationMessagePolicy: core.TerminationMessageReadFile, // Handle container termination status through file.
				},
			},
		},
	}
)

// TestWarningsForJobSpec tests different configurations of batch.JobSpec and their associated warnings.
// Each test case checks for potential warnings based on the provided JobSpec configuration.
func TestWarningsForJobSpec(t *testing.T) {
	// Define a set of test cases with different JobSpec configurations and the expected number of warnings.
	cases := map[string]struct {
		spec              *batch.JobSpec // The JobSpec under test
		wantWarningsCount int             // The expected number of warnings for this JobSpec
	}{
		// Test case: Valid NonIndexed job with no issues.
		"valid NonIndexed": {
			spec: &batch.JobSpec{
				CompletionMode: ptr.To(batch.NonIndexedCompletion), // Non-indexed completion mode.
				Template:       validPodTemplate,                   // Using the valid pod template.
			},
		},
		// Test case: NonIndexed job with very high completions and parallelism.
		"NondIndexed with high completions and parallelism": {
			spec: &batch.JobSpec{
				CompletionMode: ptr.To(batch.NonIndexedCompletion), // Non-indexed completion mode.
				Template:       validPodTemplate,                   // Using the valid pod template.
				// High Parallelism and Completions values to stress-test the job's configuration.
				Parallelism: ptr.To[int32](1_000_000_000),
				Completions: ptr.To[int32](1_000_000_000),
			},
		},
		// Test case: Invalid PodTemplate (e.g., ImagePullSecrets with an empty name).
		"invalid PodTemplate": {
			spec: &batch.JobSpec{
				CompletionMode: ptr.To(batch.NonIndexedCompletion),
				Template: core.PodTemplateSpec{
					// Invalid PodSpec with an empty ImagePullSecret name, which should trigger a warning.
					Spec: core.PodSpec{ImagePullSecrets: []core.LocalObjectReference{{Name: ""}}},
				},
			},
			wantWarningsCount: 1, // We expect one warning due to the invalid ImagePullSecret.
		},
		// Test case: Valid Indexed job with low completions and low parallelism.
		"valid Indexed low completions low parallelism": {
			spec: &batch.JobSpec{
				CompletionMode: ptr.To(batch.IndexedCompletion), // Indexed completion mode.
				Completions:    ptr.To[int32](10_000),           // Moderate completions.
				Parallelism:    ptr.To[int32](10_000),           // Moderate parallelism.
				Template:       validPodTemplate,                // Using the valid pod template.
			},
		},
		// Test case: Indexed job with high completions but low parallelism.
		"valid Indexed high completions low parallelism": {
			spec: &batch.JobSpec{
				CompletionMode: ptr.To(batch.IndexedCompletion),
				Completions:    ptr.To[int32](1000_000_000), // High number of completions.
				Parallelism:    ptr.To[int32](10_000),        // Lower parallelism.
				Template:       validPodTemplate,             // Using the valid pod template.
			},
		},
		// Test case: Indexed job with medium completions and medium parallelism.
		"valid Indexed medium completions medium parallelism": {
			spec: &batch.JobSpec{
				CompletionMode: ptr.To(batch.IndexedCompletion),
				Completions:    ptr.To[int32](100_000), // Medium completions.
				Parallelism:    ptr.To[int32](100_000), // Medium parallelism.
				Template:       validPodTemplate,       // Using the valid pod template.
			},
		},
		// Test case: Invalid Indexed job with too high completions and parallelism, which should trigger a warning.
		"invalid Indexed high completions high parallelism": {
			spec: &batch.JobSpec{
				CompletionMode: ptr.To(batch.IndexedCompletion),
				Completions:    ptr.To[int32](100_001), // Completions exceed the acceptable range.
				Parallelism:    ptr.To[int32](10_001),  // Parallelism exceeds the acceptable range.
				Template:       validPodTemplate,       // Using the valid pod template.
			},
			wantWarningsCount: 1, // We expect one warning due to invalid completions and parallelism.
		},
	}

	// Loop through each test case and execute it.
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			// Create a new testing context for each test case.
			_, ctx := ktesting.NewTestContext(t)

			// Call the function under test: WarningsForJobSpec.
			// This function checks for warnings based on the JobSpec configuration.
			warnings := WarningsForJobSpec(ctx, nil, tc.spec, nil)

			// Compare the actual number of warnings with the expected count.
			if len(warnings) != tc.wantWarningsCount {
				// If the counts don't match, report an error.
				t.Errorf("Got %d warnings, want %d.\nWarnings: %v", len(warnings), tc.wantWarningsCount, warnings)
			}
		})
	}
}
