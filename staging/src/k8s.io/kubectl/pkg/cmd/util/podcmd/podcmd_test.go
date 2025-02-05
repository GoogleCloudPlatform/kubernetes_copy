package podcmd

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestFindContainerByName(t *testing.T) {
	containerName := "test-container"
	initContainerName := "test-init-container"
	ephemeralContainerName := "test-ephemeral-container"

	pod := &v1.Pod{
		Spec: v1.PodSpec{
			Containers:     []v1.Container{{Name: containerName}},
			InitContainers: []v1.Container{{Name: initContainerName}},
			EphemeralContainers: []v1.EphemeralContainer{
				{EphemeralContainerCommon: v1.EphemeralContainerCommon{Name: ephemeralContainerName}},
			},
		},
	}

	tests := []struct {
		name              string
		input             string
		expectedContainer *v1.Container
		expectedString    string
	}{
		{
			name:              "find container",
			input:             containerName,
			expectedContainer: &v1.Container{Name: containerName},
			expectedString:    fmt.Sprintf("spec.containers{%s}", containerName),
		},
		{
			name:              "find init container",
			input:             initContainerName,
			expectedContainer: &v1.Container{Name: initContainerName},
			expectedString:    fmt.Sprintf("spec.initContainers{%s}", initContainerName),
		},
		{
			name:              "find ephemeral container",
			input:             ephemeralContainerName,
			expectedContainer: &v1.Container{Name: ephemeralContainerName},
			expectedString:    fmt.Sprintf("spec.ephemeralContainers{%s}", ephemeralContainerName),
		},
		{
			name:              "can't find container",
			input:             "non-existent-container",
			expectedContainer: nil,
			expectedString:    "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			container, expectedString := FindContainerByName(pod, tt.input)
			assert.Equal(t, tt.expectedContainer, container)
			assert.Equal(t, tt.expectedString, expectedString)
		})
	}
}

func TestFindOrDefaultContainerByName(t *testing.T) {
	podName := "test-pod"
	containerName := "test-container"
	nonExistentName := "non-existent-container"

	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: podName,
		},
		Spec: v1.PodSpec{Containers: []v1.Container{{Name: containerName}}},
	}

	noContainerPod := &v1.Pod{
		Spec: v1.PodSpec{Containers: []v1.Container{}},
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: "test-namespace",
		},
	}

	hasAnnotationPod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: podName,
			Annotations: map[string]string{
				DefaultContainerAnnotationName: containerName,
			},
		},
		Spec: v1.PodSpec{Containers: []v1.Container{{Name: containerName}}},
	}

	hasManyContainerPod := &v1.Pod{
		Spec: v1.PodSpec{
			Containers:     []v1.Container{{Name: containerName}},
			InitContainers: []v1.Container{{Name: "test-init-container"}},
			EphemeralContainers: []v1.EphemeralContainer{
				{EphemeralContainerCommon: v1.EphemeralContainerCommon{Name: "test-ephemeral-container"}},
			},
		},
	}

	tests := []struct {
		testName      string
		pod           *v1.Pod
		name          string
		expected      *v1.Container
		quiet         bool
		expectedWarn  string
		expectedError string
	}{
		{
			testName:      "find existing container",
			pod:           pod,
			name:          containerName,
			quiet:         true,
			expected:      &v1.Container{Name: containerName},
			expectedWarn:  "",
			expectedError: "",
		},
		{
			testName:      "can't find container",
			pod:           pod,
			name:          nonExistentName,
			quiet:         true,
			expected:      nil,
			expectedWarn:  "",
			expectedError: "container non-existent-container not found in pod test-pod",
		},
		{
			testName:      "have any container",
			pod:           noContainerPod,
			name:          "",
			quiet:         true,
			expected:      nil,
			expectedWarn:  "",
			expectedError: "pod test-namespace/test-pod does not have any containers",
		},
		{
			testName:      "read the default container the annotation as per",
			pod:           hasAnnotationPod,
			name:          "",
			quiet:         true,
			expected:      &v1.Container{Name: containerName},
			expectedWarn:  "",
			expectedError: "",
		},
		{
			testName:      "find default cotainer with not quiet",
			pod:           hasManyContainerPod,
			name:          "",
			quiet:         false,
			expected:      &v1.Container{Name: containerName},
			expectedWarn:  "Defaulted container \"test-container\" out of: test-container, test-ephemeral-container (ephem), test-init-container (init)\n",
			expectedError: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.testName, func(t *testing.T) {
			var buf bytes.Buffer
			container, err := FindOrDefaultContainerByName(tt.pod, tt.name, tt.quiet, &buf)
			if len(tt.expectedError) != 0 {
				require.Error(t, err)
				assert.EqualError(t, err, tt.expectedError)
			} else {
				require.NoError(t, err)
				require.Equal(t, tt.expected, container)
				require.Equal(t, tt.expectedWarn, buf.String())
			}
		})
	}
}

func TestAllContainerNames(t *testing.T) {
	tests := []struct {
		name     string
		pod      *v1.Pod
		expected string
	}{
		{
			name: "has any containers",
			pod: &v1.Pod{
				Spec: v1.PodSpec{},
			},
			expected: "",
		},
		{
			name: "has one container of each type",
			pod: &v1.Pod{
				Spec: v1.PodSpec{
					Containers: []v1.Container{
						{Name: "container1"},
					},
					InitContainers: []v1.Container{
						{Name: "initcontainer1"},
					},
					EphemeralContainers: []v1.EphemeralContainer{
						{EphemeralContainerCommon: v1.EphemeralContainerCommon{Name: "ephemeralcontainer1"}},
					},
				},
			},
			expected: "container1, ephemeralcontainer1 (ephem), initcontainer1 (init)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := AllContainerNames(tt.pod)
			assert.Equal(t, tt.expected, result)
		})
	}
}
