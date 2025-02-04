/*
Copyright 2014 The Kubernetes Authors.

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

package types

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/kubernetes/pkg/apis/scheduling"
)

var (
	systemPriority      = scheduling.SystemCriticalPriority
	systemPriorityUpper = systemPriority + 1000
)

// getTestPod generates a new instance of an empty test Pod
func getTestPod(annotations map[string]string, podPriority *int32, priorityClassName string) *v1.Pod {
	pod := v1.Pod{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Pod",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "foo",
			Namespace: "default",
		},
	}
	// Set pod Priority in Spec if exists
	if podPriority != nil {
		pod.Spec = v1.PodSpec{
			Priority: podPriority,
		}
	}
	pod.Spec.PriorityClassName = priorityClassName
	// Set annotations if exists
	if annotations != nil {
		pod.Annotations = annotations
	}
	return &pod
}

func configSourceAnnotation(source string) map[string]string {
	return map[string]string{ConfigSourceAnnotationKey: source}
}

func configMirrorAnnotation() map[string]string {
	return map[string]string{ConfigMirrorAnnotationKey: "true"}
}

func TestGetValidatedSources(t *testing.T) {
	tests := []struct {
		name        string
		sources     []string
		errExpected bool
		sourcesLen  int
	}{
		{
			name:        "empty source",
			sources:     []string{""},
			errExpected: false,
			sourcesLen:  0,
		},
		{
			name:        "file and apiserver source",
			sources:     []string{FileSource, ApiserverSource},
			errExpected: false,
			sourcesLen:  2,
		},
		{
			name:        "all source",
			sources:     []string{AllSource},
			errExpected: false,
			sourcesLen:  3,
		},
		{
			name:        "unknown source",
			sources:     []string{"unknown"},
			errExpected: true,
			sourcesLen:  0,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			sources, err := GetValidatedSources(test.sources)
			if test.errExpected {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
			assert.Len(t, sources, test.sourcesLen)
		})
	}
}

func TestGetPodSource(t *testing.T) {
	tests := []struct {
		name        string
		pod         *v1.Pod
		expected    string
		errExpected bool
	}{
		{
			name:        "cannot get pod source",
			pod:         getTestPod(nil, nil, ""),
			expected:    "",
			errExpected: true,
		},
		{
			name:        "valid annotation returns the source",
			pod:         getTestPod(configSourceAnnotation("host-ipc-sources"), nil, ""),
			expected:    "host-ipc-sources",
			errExpected: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source, err := GetPodSource(test.pod)
			if test.errExpected {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
			assert.Equal(t, test.expected, source)
		})
	}
}

func TestString(t *testing.T) {
	tests := []struct {
		sp       SyncPodType
		expected string
	}{
		{
			sp:       SyncPodCreate,
			expected: "create",
		},
		{
			sp:       SyncPodUpdate,
			expected: "update",
		},
		{
			sp:       SyncPodSync,
			expected: "sync",
		},
		{
			sp:       SyncPodKill,
			expected: "kill",
		},
		{
			sp:       50,
			expected: "unknown",
		},
	}
	for _, test := range tests {
		t.Run(test.expected, func(t *testing.T) {
			syncPodString := test.sp.String()
			assert.Equal(t, test.expected, syncPodString)
		})
	}
}

func TestIsMirrorPod(t *testing.T) {
	tests := []struct {
		name     string
		pod      *v1.Pod
		expected bool
	}{
		{
			name:     "mirror pod",
			pod:      getTestPod(configMirrorAnnotation(), nil, ""),
			expected: true,
		},
		{
			name:     "not a mirror pod",
			pod:      getTestPod(nil, nil, ""),
			expected: false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			isMirrorPod := IsMirrorPod(test.pod)
			assert.Equal(t, test.expected, isMirrorPod)
		})
	}
}

func TestIsStaticPod(t *testing.T) {
	tests := []struct {
		name     string
		pod      *v1.Pod
		expected bool
	}{
		{
			name:     "static pod with file source",
			pod:      getTestPod(configSourceAnnotation(FileSource), nil, ""),
			expected: true,
		},
		{
			name:     "static pod with http source",
			pod:      getTestPod(configSourceAnnotation(HTTPSource), nil, ""),
			expected: true,
		},
		{
			name:     "static pod with api server source",
			pod:      getTestPod(configSourceAnnotation(ApiserverSource), nil, ""),
			expected: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			isStaticPod := IsStaticPod(test.pod)
			assert.Equal(t, test.expected, isStaticPod)
		})
	}
}

func TestIsCriticalPod(t *testing.T) {
	tests := []struct {
		name     string
		pod      *v1.Pod
		expected bool
	}{
		{
			name:     "critical pod with file source",
			pod:      getTestPod(configSourceAnnotation(FileSource), nil, ""),
			expected: true,
		},
		{
			name:     "critical pod with mirror annotation",
			pod:      getTestPod(configMirrorAnnotation(), nil, ""),
			expected: true,
		},
		{
			name:     "critical pod using system priority",
			pod:      getTestPod(nil, &systemPriority, ""),
			expected: true,
		},
		{
			name:     "critical pod using greater than system priority",
			pod:      getTestPod(nil, &systemPriorityUpper, ""),
			expected: true,
		},
		{
			name:     "not a critical pod with api server annotation",
			pod:      getTestPod(configSourceAnnotation(ApiserverSource), nil, ""),
			expected: false,
		},
		{
			name:     "not critical if not static, mirror or without a priority",
			pod:      getTestPod(nil, nil, ""),
			expected: false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			isCriticalPod := IsCriticalPod(test.pod)
			assert.Equal(t, test.expected, isCriticalPod)
		})
	}
}

func TestPreemptible(t *testing.T) {
	tests := []struct {
		name      string
		preemptor *v1.Pod
		preemptee *v1.Pod
		expected  bool
	}{
		{
			name:      "a critical preemptor pod preempts a non critical pod",
			preemptor: getTestPod(configSourceAnnotation(FileSource), nil, ""),
			preemptee: getTestPod(nil, nil, ""),
			expected:  true,
		},
		{
			name:      "a preemptor pod with higher priority preempts a critical pod",
			preemptor: getTestPod(configSourceAnnotation(FileSource), &systemPriorityUpper, ""),
			preemptee: getTestPod(configSourceAnnotation(FileSource), &systemPriority, ""),
			expected:  true,
		},
		{
			name:      "a not critical pod with higher priority preempts a critical pod",
			preemptor: getTestPod(configSourceAnnotation(ApiserverSource), &systemPriorityUpper, ""),
			preemptee: getTestPod(configSourceAnnotation(FileSource), &systemPriority, ""),
			expected:  true,
		},
		{
			name:      "a critical pod with less priority do not preempts a critical pod",
			preemptor: getTestPod(configSourceAnnotation(FileSource), &systemPriority, ""),
			preemptee: getTestPod(configSourceAnnotation(FileSource), &systemPriorityUpper, ""),
			expected:  false,
		},
		{
			name:      "a critical pod without priority do not preempts a critical pod without priority",
			preemptor: getTestPod(configSourceAnnotation(FileSource), nil, ""),
			preemptee: getTestPod(configSourceAnnotation(FileSource), nil, ""),
			expected:  false,
		},
		{
			name:      "a critical pod with priority do not preempts a critical pod with the same priority",
			preemptor: getTestPod(configSourceAnnotation(FileSource), &systemPriority, ""),
			preemptee: getTestPod(configSourceAnnotation(FileSource), &systemPriority, ""),
			expected:  false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			isPreemtable := Preemptible(test.preemptor, test.preemptee)
			assert.Equal(t, test.expected, isPreemtable)
		})
	}
}

func TestIsCriticalPodBasedOnPriority(t *testing.T) {
	tests := []struct {
		priority int32
		name     string
		expected bool
	}{
		{
			name:     "a system critical pod",
			priority: systemPriority,
			expected: true,
		},
		{
			name:     "a non system critical pod",
			priority: scheduling.HighestUserDefinablePriority,
			expected: false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			actual := IsCriticalPodBasedOnPriority(test.priority)
			if actual != test.expected {
				t.Errorf("IsCriticalPodBased on priority should have returned %v for test %v but got %v", test.expected, test.name, actual)
			}
		})
	}
}

func TestIsNodeCriticalPod(t *testing.T) {
	tests := []struct {
		name     string
		pod      *v1.Pod
		expected bool
	}{
		{
			name:     "critical pod with file source and systemNodeCritical",
			pod:      getTestPod(configSourceAnnotation(FileSource), nil, scheduling.SystemNodeCritical),
			expected: true,
		},
		{
			name:     "critical pod with mirror annotation and systemNodeCritical",
			pod:      getTestPod(configMirrorAnnotation(), nil, scheduling.SystemNodeCritical),
			expected: true,
		},
		{
			name:     "critical pod using system priority and systemNodeCritical",
			pod:      getTestPod(nil, &systemPriority, scheduling.SystemNodeCritical),
			expected: true,
		},
		{
			name:     "critical pod using greater than system priority and systemNodeCritical",
			pod:      getTestPod(nil, &systemPriorityUpper, scheduling.SystemNodeCritical),
			expected: true,
		},
		{
			name:     "not a critical pod with api server annotation and systemNodeCritical",
			pod:      getTestPod(configSourceAnnotation(ApiserverSource), nil, scheduling.SystemNodeCritical),
			expected: false,
		},
		{
			name:     "not critical if not static, mirror or without a priority and systemNodeCritical",
			pod:      getTestPod(nil, nil, scheduling.SystemNodeCritical),
			expected: false,
		},
		{
			name:     "not critical if not static, mirror or without a priority",
			pod:      getTestPod(nil, nil, ""),
			expected: false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			isNodeCriticalPod := IsNodeCriticalPod(test.pod)
			require.Equal(t, test.expected, isNodeCriticalPod)
		})
	}
}
