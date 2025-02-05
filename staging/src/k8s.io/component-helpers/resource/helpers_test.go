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

package resource

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestPodRequestsAndLimits(t *testing.T) {
	cases := []struct {
		pod              *v1.Pod
		cName            string
		expectedRequests v1.ResourceList
		expectedLimits   v1.ResourceList
	}{
		{
			cName:            "just-limit-no-overhead",
			pod:              getPod("foo", podResources{cpuLimit: "9"}),
			expectedRequests: v1.ResourceList{},
			expectedLimits: v1.ResourceList{
				v1.ResourceName(v1.ResourceCPU): resource.MustParse("9"),
			},
		},
		{
			cName: "just-overhead",
			pod:   getPod("foo", podResources{cpuOverhead: "5", memoryOverhead: "5"}),
			expectedRequests: v1.ResourceList{
				v1.ResourceName(v1.ResourceCPU):    resource.MustParse("5"),
				v1.ResourceName(v1.ResourceMemory): resource.MustParse("5"),
			},
			expectedLimits: v1.ResourceList{},
		},
		{
			cName: "req-and-overhead",
			pod:   getPod("foo", podResources{cpuRequest: "1", memoryRequest: "10", cpuOverhead: "5", memoryOverhead: "5"}),
			expectedRequests: v1.ResourceList{
				v1.ResourceName(v1.ResourceCPU):    resource.MustParse("6"),
				v1.ResourceName(v1.ResourceMemory): resource.MustParse("15"),
			},
			expectedLimits: v1.ResourceList{},
		},
		{
			cName: "all-req-lim-and-overhead",
			pod:   getPod("foo", podResources{cpuRequest: "1", cpuLimit: "2", memoryRequest: "10", memoryLimit: "12", cpuOverhead: "5", memoryOverhead: "5"}),
			expectedRequests: v1.ResourceList{
				v1.ResourceName(v1.ResourceCPU):    resource.MustParse("6"),
				v1.ResourceName(v1.ResourceMemory): resource.MustParse("15"),
			},
			expectedLimits: v1.ResourceList{
				v1.ResourceName(v1.ResourceCPU):    resource.MustParse("7"),
				v1.ResourceName(v1.ResourceMemory): resource.MustParse("17"),
			},
		},
		{
			cName: "req-some-lim-and-overhead",
			pod:   getPod("foo", podResources{cpuRequest: "1", cpuLimit: "2", memoryRequest: "10", cpuOverhead: "5", memoryOverhead: "5"}),
			expectedRequests: v1.ResourceList{
				v1.ResourceName(v1.ResourceCPU):    resource.MustParse("6"),
				v1.ResourceName(v1.ResourceMemory): resource.MustParse("15"),
			},
			expectedLimits: v1.ResourceList{
				v1.ResourceName(v1.ResourceCPU): resource.MustParse("7"),
			},
		},
	}
	for idx, tc := range cases {
		resRequests := PodRequests(tc.pod, PodResourcesOptions{})
		resLimits := PodLimits(tc.pod, PodResourcesOptions{})

		if !equality.Semantic.DeepEqual(tc.expectedRequests, resRequests) {
			t.Errorf("test case failure[%d]: %v, requests:\n expected:\t%v\ngot\t\t%v", idx, tc.cName, tc.expectedRequests, resRequests)
		}

		if !equality.Semantic.DeepEqual(tc.expectedLimits, resLimits) {
			t.Errorf("test case failure[%d]: %v, limits:\n expected:\t%v\ngot\t\t%v", idx, tc.cName, tc.expectedLimits, resLimits)
		}
	}
}

func TestPodRequestsAndLimitsWithoutOverhead(t *testing.T) {
	cases := []struct {
		pod              *v1.Pod
		name             string
		expectedRequests v1.ResourceList
		expectedLimits   v1.ResourceList
	}{
		{
			name: "two container no overhead - should just be sum of containers",
			pod: &v1.Pod{
				Spec: v1.PodSpec{
					Containers: []v1.Container{
						{
							Name: "foobar",
							Resources: v1.ResourceRequirements{
								Requests: v1.ResourceList{
									v1.ResourceName(v1.ResourceCPU):    resource.MustParse("1"),
									v1.ResourceName(v1.ResourceMemory): resource.MustParse("5"),
								},
								Limits: v1.ResourceList{
									v1.ResourceName(v1.ResourceCPU):    resource.MustParse("2"),
									v1.ResourceName(v1.ResourceMemory): resource.MustParse("10"),
								},
							},
						},
						{
							Name: "foobar2",
							Resources: v1.ResourceRequirements{
								Requests: v1.ResourceList{
									v1.ResourceName(v1.ResourceCPU):    resource.MustParse("4"),
									v1.ResourceName(v1.ResourceMemory): resource.MustParse("12"),
								},
								Limits: v1.ResourceList{
									v1.ResourceName(v1.ResourceCPU):    resource.MustParse("8"),
									v1.ResourceName(v1.ResourceMemory): resource.MustParse("24"),
								},
							},
						},
					},
				},
			},
			expectedRequests: v1.ResourceList{
				v1.ResourceName(v1.ResourceCPU):    resource.MustParse("5"),
				v1.ResourceName(v1.ResourceMemory): resource.MustParse("17"),
			},
			expectedLimits: v1.ResourceList{
				v1.ResourceName(v1.ResourceCPU):    resource.MustParse("10"),
				v1.ResourceName(v1.ResourceMemory): resource.MustParse("34"),
			},
		},
		{
			name: "two container with overhead - shouldn't consider overhead",
			pod: &v1.Pod{
				Spec: v1.PodSpec{
					Overhead: v1.ResourceList{
						v1.ResourceName(v1.ResourceCPU):    resource.MustParse("3"),
						v1.ResourceName(v1.ResourceMemory): resource.MustParse("8"),
					},
					Containers: []v1.Container{
						{
							Name: "foobar",
							Resources: v1.ResourceRequirements{
								Requests: v1.ResourceList{
									v1.ResourceName(v1.ResourceCPU):    resource.MustParse("1"),
									v1.ResourceName(v1.ResourceMemory): resource.MustParse("5"),
								},
								Limits: v1.ResourceList{
									v1.ResourceName(v1.ResourceCPU):    resource.MustParse("2"),
									v1.ResourceName(v1.ResourceMemory): resource.MustParse("10"),
								},
							},
						},
						{
							Name: "foobar2",
							Resources: v1.ResourceRequirements{
								Requests: v1.ResourceList{
									v1.ResourceName(v1.ResourceCPU):    resource.MustParse("4"),
									v1.ResourceName(v1.ResourceMemory): resource.MustParse("12"),
								},
								Limits: v1.ResourceList{
									v1.ResourceName(v1.ResourceCPU):    resource.MustParse("8"),
									v1.ResourceName(v1.ResourceMemory): resource.MustParse("24"),
								},
							},
						},
					},
				},
			},
			expectedRequests: v1.ResourceList{
				v1.ResourceName(v1.ResourceCPU):    resource.MustParse("5"),
				v1.ResourceName(v1.ResourceMemory): resource.MustParse("17"),
			},
			expectedLimits: v1.ResourceList{
				v1.ResourceName(v1.ResourceCPU):    resource.MustParse("10"),
				v1.ResourceName(v1.ResourceMemory): resource.MustParse("34"),
			},
		},
		{
			name: "two container with overhead, massive init - should just be the largest init",
			pod: &v1.Pod{
				Spec: v1.PodSpec{
					Overhead: v1.ResourceList{
						v1.ResourceName(v1.ResourceCPU):    resource.MustParse("3"),
						v1.ResourceName(v1.ResourceMemory): resource.MustParse("8"),
					},
					Containers: []v1.Container{
						{
							Name: "foobar",
							Resources: v1.ResourceRequirements{
								Requests: v1.ResourceList{
									v1.ResourceName(v1.ResourceCPU):    resource.MustParse("1"),
									v1.ResourceName(v1.ResourceMemory): resource.MustParse("5"),
								},
								Limits: v1.ResourceList{
									v1.ResourceName(v1.ResourceCPU):    resource.MustParse("2"),
									v1.ResourceName(v1.ResourceMemory): resource.MustParse("10"),
								},
							},
						},
						{
							Name: "foobar2",
							Resources: v1.ResourceRequirements{
								Requests: v1.ResourceList{
									v1.ResourceName(v1.ResourceCPU):    resource.MustParse("4"),
									v1.ResourceName(v1.ResourceMemory): resource.MustParse("12"),
								},
								Limits: v1.ResourceList{
									v1.ResourceName(v1.ResourceCPU):    resource.MustParse("8"),
									v1.ResourceName(v1.ResourceMemory): resource.MustParse("24"),
								},
							},
						},
					},
					InitContainers: []v1.Container{
						{
							Name: "small-init",
							Resources: v1.ResourceRequirements{
								Requests: v1.ResourceList{
									v1.ResourceName(v1.ResourceCPU):    resource.MustParse("1"),
									v1.ResourceName(v1.ResourceMemory): resource.MustParse("5"),
								},
								Limits: v1.ResourceList{
									v1.ResourceName(v1.ResourceCPU):    resource.MustParse("1"),
									v1.ResourceName(v1.ResourceMemory): resource.MustParse("5"),
								},
							},
						},
						{
							Name: "big-init",
							Resources: v1.ResourceRequirements{
								Requests: v1.ResourceList{
									v1.ResourceName(v1.ResourceCPU):    resource.MustParse("40"),
									v1.ResourceName(v1.ResourceMemory): resource.MustParse("120"),
								},
								Limits: v1.ResourceList{
									v1.ResourceName(v1.ResourceCPU):    resource.MustParse("80"),
									v1.ResourceName(v1.ResourceMemory): resource.MustParse("240"),
								},
							},
						},
					},
				},
			},
			expectedRequests: v1.ResourceList{
				v1.ResourceName(v1.ResourceCPU):    resource.MustParse("40"),
				v1.ResourceName(v1.ResourceMemory): resource.MustParse("120"),
			},
			expectedLimits: v1.ResourceList{
				v1.ResourceName(v1.ResourceCPU):    resource.MustParse("80"),
				v1.ResourceName(v1.ResourceMemory): resource.MustParse("240"),
			},
		},
	}
	for idx, tc := range cases {
		resRequests := PodRequests(tc.pod, PodResourcesOptions{ExcludeOverhead: true})
		resLimits := PodLimits(tc.pod, PodResourcesOptions{ExcludeOverhead: true})

		if !equality.Semantic.DeepEqual(tc.expectedRequests, resRequests) {
			t.Errorf("test case failure[%d]: %v, requests:\n expected:\t%v\ngot\t\t%v", idx, tc.name, tc.expectedRequests, resRequests)
		}

		if !equality.Semantic.DeepEqual(tc.expectedLimits, resLimits) {
			t.Errorf("test case failure[%d]: %v, limits:\n expected:\t%v\ngot\t\t%v", idx, tc.name, tc.expectedLimits, resLimits)
		}
	}
}

func TestPodResourceRequests(t *testing.T) {
	restartAlways := v1.ContainerRestartPolicyAlways
	testCases := []struct {
		description           string
		options               PodResourcesOptions
		overhead              v1.ResourceList
		podResizeStatus       v1.PodResizeStatus
		initContainers        []v1.Container
		initContainerStatuses []v1.ContainerStatus
		containers            []v1.Container
		containerStatus       []v1.ContainerStatus
		expectedRequests      v1.ResourceList
	}{
		{
			description: "nil options, larger init container",
			expectedRequests: v1.ResourceList{
				v1.ResourceCPU: resource.MustParse("4"),
			},
			initContainers: []v1.Container{
				{
					Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{
							v1.ResourceCPU: resource.MustParse("4"),
						},
					},
				},
			},
			containers: []v1.Container{
				{
					Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{
							v1.ResourceCPU: resource.MustParse("1"),
						},
					},
				},
			},
		},
		{
			description: "nil options, larger containers",
			expectedRequests: v1.ResourceList{
				v1.ResourceCPU: resource.MustParse("5"),
			},
			initContainers: []v1.Container{
				{
					Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{
							v1.ResourceCPU: resource.MustParse("2"),
						},
					},
				},
			},
			containers: []v1.Container{
				{
					Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{
							v1.ResourceCPU: resource.MustParse("2"),
						},
					},
				},
				{
					Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{
							v1.ResourceCPU: resource.MustParse("3"),
						},
					},
				},
			},
		},
		{
			description: "pod overhead excluded",
			expectedRequests: v1.ResourceList{
				v1.ResourceCPU: resource.MustParse("5"),
			},
			options: PodResourcesOptions{
				ExcludeOverhead: true,
			},
			overhead: v1.ResourceList{
				v1.ResourceCPU: resource.MustParse("1"),
			},
			initContainers: []v1.Container{
				{
					Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{
							v1.ResourceCPU: resource.MustParse("2"),
						},
					},
				},
			},
			containers: []v1.Container{
				{
					Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{
							v1.ResourceCPU: resource.MustParse("2"),
						},
					},
				},
				{
					Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{
							v1.ResourceCPU: resource.MustParse("3"),
						},
					},
				},
			},
		},
		{
			description: "pod overhead included",
			expectedRequests: v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse("6"),
				v1.ResourceMemory: resource.MustParse("1Gi"),
			},
			overhead: v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse("1"),
				v1.ResourceMemory: resource.MustParse("1Gi"),
			},
			initContainers: []v1.Container{
				{
					Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{
							v1.ResourceCPU: resource.MustParse("2"),
						},
						Limits: v1.ResourceList{
							v1.ResourceCPU: resource.MustParse("2"),
						},
					},
				},
			},
			containers: []v1.Container{
				{
					Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{
							v1.ResourceCPU: resource.MustParse("2"),
						},
					},
				},
				{
					Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{
							v1.ResourceCPU: resource.MustParse("3"),
						},
					},
				},
			},
		},
		{
			description: "resized, infeasible",
			expectedRequests: v1.ResourceList{
				v1.ResourceCPU: resource.MustParse("2"),
			},
			podResizeStatus: v1.PodResizeStatusInfeasible,
			options:         PodResourcesOptions{UseStatusResources: true},
			containers: []v1.Container{
				{
					Name: "container-1",
					Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{
							v1.ResourceCPU: resource.MustParse("4"),
						},
					},
				},
			},
			containerStatus: []v1.ContainerStatus{
				{
					Name: "container-1",
					Resources: &v1.ResourceRequirements{
						Requests: v1.ResourceList{
							v1.ResourceCPU: resource.MustParse("2"),
						},
					},
				},
			},
		},
		{
			description: "resized, no resize status",
			expectedRequests: v1.ResourceList{
				v1.ResourceCPU: resource.MustParse("4"),
			},
			options: PodResourcesOptions{UseStatusResources: true},
			containers: []v1.Container{
				{
					Name: "container-1",
					Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{
							v1.ResourceCPU: resource.MustParse("4"),
						},
					},
				},
			},
			containerStatus: []v1.ContainerStatus{
				{
					Name: "container-1",
					Resources: &v1.ResourceRequirements{
						Requests: v1.ResourceList{
							v1.ResourceCPU: resource.MustParse("2"),
						},
					},
				},
			},
		},
		{
			description: "resized, infeasible, but don't use status",
			expectedRequests: v1.ResourceList{
				v1.ResourceCPU: resource.MustParse("4"),
			},
			podResizeStatus: v1.PodResizeStatusInfeasible,
			options:         PodResourcesOptions{UseStatusResources: false},
			containers: []v1.Container{
				{
					Name: "container-1",
					Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{
							v1.ResourceCPU: resource.MustParse("4"),
						},
					},
				},
			},
			containerStatus: []v1.ContainerStatus{
				{
					Name: "container-1",
					Resources: &v1.ResourceRequirements{
						Requests: v1.ResourceList{
							v1.ResourceCPU: resource.MustParse("2"),
						},
					},
				},
			},
		},
		{
			description: "resized, restartable init container, infeasible",
			expectedRequests: v1.ResourceList{
				v1.ResourceCPU: resource.MustParse("2"),
			},
			podResizeStatus: v1.PodResizeStatusInfeasible,
			options:         PodResourcesOptions{UseStatusResources: true},
			initContainers: []v1.Container{
				{
					Name:          "restartable-init-1",
					RestartPolicy: &restartAlways,
					Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{
							v1.ResourceCPU: resource.MustParse("4"),
						},
					},
				},
			},
			initContainerStatuses: []v1.ContainerStatus{
				{
					Name: "restartable-init-1",
					Resources: &v1.ResourceRequirements{
						Requests: v1.ResourceList{
							v1.ResourceCPU: resource.MustParse("2"),
						},
					},
				},
			},
		},
		{
			description: "resized, restartable init container, no resize status",
			expectedRequests: v1.ResourceList{
				v1.ResourceCPU: resource.MustParse("4"),
			},
			options: PodResourcesOptions{UseStatusResources: true},
			initContainers: []v1.Container{
				{
					Name:          "restartable-init-1",
					RestartPolicy: &restartAlways,
					Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{
							v1.ResourceCPU: resource.MustParse("4"),
						},
					},
				},
			},
			initContainerStatuses: []v1.ContainerStatus{
				{
					Name: "restartable-init-1",
					Resources: &v1.ResourceRequirements{
						Requests: v1.ResourceList{
							v1.ResourceCPU: resource.MustParse("2"),
						},
					},
				},
			},
		},
		{
			description: "resized, restartable init container, infeasible, but don't use status",
			expectedRequests: v1.ResourceList{
				v1.ResourceCPU: resource.MustParse("4"),
			},
			podResizeStatus: v1.PodResizeStatusInfeasible,
			options:         PodResourcesOptions{UseStatusResources: false},
			initContainers: []v1.Container{
				{
					Name:          "restartable-init-1",
					RestartPolicy: &restartAlways,
					Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{
							v1.ResourceCPU: resource.MustParse("4"),
						},
					},
				},
			},
			initContainerStatuses: []v1.ContainerStatus{
				{
					Name: "restartable-init-1",
					Resources: &v1.ResourceRequirements{
						Requests: v1.ResourceList{
							v1.ResourceCPU: resource.MustParse("2"),
						},
					},
				},
			},
		},
		{
			description: "restartable init container",
			expectedRequests: v1.ResourceList{
				// restartable init + regular container
				v1.ResourceCPU: resource.MustParse("2"),
			},
			initContainers: []v1.Container{
				{
					Name:          "restartable-init-1",
					RestartPolicy: &restartAlways,
					Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{
							v1.ResourceCPU: resource.MustParse("1"),
						},
					},
				},
			},
			containers: []v1.Container{
				{
					Name: "container-1",
					Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{
							v1.ResourceCPU: resource.MustParse("1"),
						},
					},
				},
			},
		},
		{
			description: "multiple restartable init containers",
			expectedRequests: v1.ResourceList{
				// max(5, restartable init containers(3+2+1) + regular(1)) = 7
				v1.ResourceCPU: resource.MustParse("7"),
			},
			initContainers: []v1.Container{
				{
					Name: "init-1",
					Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{
							v1.ResourceCPU: resource.MustParse("5"),
						},
					},
				},
				{
					Name:          "restartable-init-1",
					RestartPolicy: &restartAlways,
					Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{
							v1.ResourceCPU: resource.MustParse("1"),
						},
					},
				},
				{
					Name:          "restartable-init-2",
					RestartPolicy: &restartAlways,
					Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{
							v1.ResourceCPU: resource.MustParse("2"),
						},
					},
				},
				{
					Name:          "restartable-init-3",
					RestartPolicy: &restartAlways,
					Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{
							v1.ResourceCPU: resource.MustParse("3"),
						},
					},
				},
			},
			containers: []v1.Container{
				{
					Name: "container-1",
					Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{
							v1.ResourceCPU: resource.MustParse("1"),
						},
					},
				},
			},
		},
		{
			description: "multiple restartable and regular init containers",
			expectedRequests: v1.ResourceList{
				// init-2 requires 5 + the previously running restartable init
				// containers(1+2) = 8, the restartable init container that starts
				// after it doesn't count
				v1.ResourceCPU: resource.MustParse("8"),
			},
			initContainers: []v1.Container{
				{
					Name: "init-1",
					Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{
							v1.ResourceCPU: resource.MustParse("5"),
						},
					},
				},
				{
					Name:          "restartable-init-1",
					RestartPolicy: &restartAlways,
					Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{
							v1.ResourceCPU: resource.MustParse("1"),
						},
					},
				},
				{
					Name:          "restartable-init-2",
					RestartPolicy: &restartAlways,
					Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{
							v1.ResourceCPU: resource.MustParse("2"),
						},
					},
				},
				{
					Name: "init-2",
					Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{
							v1.ResourceCPU: resource.MustParse("5"),
						},
					},
				},
				{
					Name:          "restartable-init-3",
					RestartPolicy: &restartAlways,
					Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{
							v1.ResourceCPU: resource.MustParse("3"),
						},
					},
				},
			},
			containers: []v1.Container{
				{
					Name: "container-1",
					Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{
							v1.ResourceCPU: resource.MustParse("1"),
						},
					},
				},
			},
		},
		{
			description: "restartable-init, init and regular",
			expectedRequests: v1.ResourceList{
				v1.ResourceCPU: resource.MustParse("210"),
			},
			initContainers: []v1.Container{
				{
					Name:          "restartable-init-1",
					RestartPolicy: &restartAlways,
					Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{
							v1.ResourceCPU: resource.MustParse("10"),
						},
					},
				},
				{
					Name: "init-1",
					Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{
							v1.ResourceCPU: resource.MustParse("200"),
						},
					},
				},
			},
			containers: []v1.Container{
				{
					Name: "container-1",
					Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{
							v1.ResourceCPU: resource.MustParse("100"),
						},
					},
				},
			},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.description, func(t *testing.T) {
			p := &v1.Pod{
				Spec: v1.PodSpec{
					Containers:     tc.containers,
					InitContainers: tc.initContainers,
					Overhead:       tc.overhead,
				},
				Status: v1.PodStatus{
					ContainerStatuses:     tc.containerStatus,
					InitContainerStatuses: tc.initContainerStatuses,
					Resize:                tc.podResizeStatus,
				},
			}
			request := PodRequests(p, tc.options)
			if diff := cmp.Diff(request, tc.expectedRequests); diff != "" {
				t.Errorf("got=%v, want=%v, diff=%s", request, tc.expectedRequests, diff)
			}
		})
	}
}

func TestPodResourceRequestsReuse(t *testing.T) {
	expectedRequests := v1.ResourceList{
		v1.ResourceCPU: resource.MustParse("1"),
	}
	p := &v1.Pod{
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Resources: v1.ResourceRequirements{
						Requests: expectedRequests,
					},
				},
			},
		},
	}

	opts := PodResourcesOptions{
		Reuse: v1.ResourceList{
			v1.ResourceCPU: resource.MustParse("25"),
		},
	}
	requests := PodRequests(p, opts)

	if diff := cmp.Diff(requests, expectedRequests); diff != "" {
		t.Errorf("got=%v, want=%v, diff=%s", requests, expectedRequests, diff)
	}

	// should re-use the maps we passed in
	if diff := cmp.Diff(opts.Reuse, expectedRequests); diff != "" {
		t.Errorf("got=%v, want=%v, diff=%s", requests, expectedRequests, diff)
	}
}

func TestPodResourceLimits(t *testing.T) {
	restartAlways := v1.ContainerRestartPolicyAlways
	testCases := []struct {
		description           string
		options               PodResourcesOptions
		overhead              v1.ResourceList
		initContainers        []v1.Container
		initContainerStatuses []v1.ContainerStatus
		containers            []v1.Container
		containerStatuses     []v1.ContainerStatus
		expectedLimits        v1.ResourceList
	}{
		{
			description: "nil options, larger init container",
			expectedLimits: v1.ResourceList{
				v1.ResourceCPU: resource.MustParse("4"),
			},
			initContainers: []v1.Container{
				{
					Resources: v1.ResourceRequirements{
						Limits: v1.ResourceList{
							v1.ResourceCPU: resource.MustParse("4"),
						},
					},
				},
			},
			containers: []v1.Container{
				{
					Resources: v1.ResourceRequirements{
						Limits: v1.ResourceList{
							v1.ResourceCPU: resource.MustParse("1"),
						},
					},
				},
			},
		},
		{
			description: "nil options, larger containers",
			expectedLimits: v1.ResourceList{
				v1.ResourceCPU: resource.MustParse("5"),
			},
			initContainers: []v1.Container{
				{
					Resources: v1.ResourceRequirements{
						Limits: v1.ResourceList{
							v1.ResourceCPU: resource.MustParse("2"),
						},
					},
				},
			},
			containers: []v1.Container{
				{
					Resources: v1.ResourceRequirements{
						Limits: v1.ResourceList{
							v1.ResourceCPU: resource.MustParse("2"),
						},
					},
				},
				{
					Resources: v1.ResourceRequirements{
						Limits: v1.ResourceList{
							v1.ResourceCPU: resource.MustParse("3"),
						},
					},
				},
			},
		},
		{
			description: "pod overhead excluded",
			expectedLimits: v1.ResourceList{
				v1.ResourceCPU: resource.MustParse("5"),
			},
			options: PodResourcesOptions{
				ExcludeOverhead: true,
			},
			overhead: v1.ResourceList{
				v1.ResourceCPU: resource.MustParse("1"),
			},
			initContainers: []v1.Container{
				{
					Resources: v1.ResourceRequirements{
						Limits: v1.ResourceList{
							v1.ResourceCPU: resource.MustParse("2"),
						},
					},
				},
			},
			containers: []v1.Container{
				{
					Resources: v1.ResourceRequirements{
						Limits: v1.ResourceList{
							v1.ResourceCPU: resource.MustParse("2"),
						},
					},
				},
				{
					Resources: v1.ResourceRequirements{
						Limits: v1.ResourceList{
							v1.ResourceCPU: resource.MustParse("3"),
						},
					},
				},
			},
		},
		{
			description: "pod overhead included",
			overhead: v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse("1"),
				v1.ResourceMemory: resource.MustParse("1Gi"),
			},
			expectedLimits: v1.ResourceList{
				v1.ResourceCPU: resource.MustParse("6"),
				// overhead is only added to non-zero limits, so there will be no expected memory limit
			},
			initContainers: []v1.Container{
				{
					Resources: v1.ResourceRequirements{
						Limits: v1.ResourceList{
							v1.ResourceCPU: resource.MustParse("2"),
						},
					},
				},
			},
			containers: []v1.Container{
				{
					Resources: v1.ResourceRequirements{
						Limits: v1.ResourceList{
							v1.ResourceCPU: resource.MustParse("2"),
						},
					},
				},
				{
					Resources: v1.ResourceRequirements{
						Limits: v1.ResourceList{
							v1.ResourceCPU: resource.MustParse("3"),
						},
					},
				},
			},
		},
		{
			description:    "no limited containers should result in no limits for the pod",
			expectedLimits: v1.ResourceList{},
			initContainers: []v1.Container{},
			containers: []v1.Container{
				{
					// Unlimited container
				},
			},
		},
		{
			description: "one limited and one unlimited container should result in the limited container's limits for the pod",
			expectedLimits: v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse("2"),
				v1.ResourceMemory: resource.MustParse("2Gi"),
			},
			initContainers: []v1.Container{},
			containers: []v1.Container{
				{
					Resources: v1.ResourceRequirements{
						Limits: v1.ResourceList{
							v1.ResourceCPU:    resource.MustParse("2"),
							v1.ResourceMemory: resource.MustParse("2Gi"),
						},
					},
				},
				{
					// Unlimited container
				},
			},
		},
		{
			description: "one limited and one unlimited init container should result in the limited init container's limits for the pod",
			expectedLimits: v1.ResourceList{
				v1.ResourceCPU:    resource.MustParse("2"),
				v1.ResourceMemory: resource.MustParse("2Gi"),
			},
			initContainers: []v1.Container{
				{
					Resources: v1.ResourceRequirements{
						Limits: v1.ResourceList{
							v1.ResourceCPU:    resource.MustParse("2"),
							v1.ResourceMemory: resource.MustParse("2Gi"),
						},
					},
				},
				{
					// Unlimited init container
				},
			},
			containers: []v1.Container{
				{
					Resources: v1.ResourceRequirements{
						Limits: v1.ResourceList{
							v1.ResourceCPU:    resource.MustParse("1"),
							v1.ResourceMemory: resource.MustParse("1Gi"),
						},
					},
				},
			},
		},
		{
			description: "restartable init container",
			expectedLimits: v1.ResourceList{
				// restartable init + regular container
				v1.ResourceCPU: resource.MustParse("2"),
			},
			initContainers: []v1.Container{
				{
					Name:          "restartable-init-1",
					RestartPolicy: &restartAlways,
					Resources: v1.ResourceRequirements{
						Limits: v1.ResourceList{
							v1.ResourceCPU: resource.MustParse("1"),
						},
					},
				},
			},
			containers: []v1.Container{
				{
					Name: "container-1",
					Resources: v1.ResourceRequirements{
						Limits: v1.ResourceList{
							v1.ResourceCPU: resource.MustParse("1"),
						},
					},
				},
			},
		},
		{
			description: "multiple restartable init containers",
			expectedLimits: v1.ResourceList{
				// max(5, restartable init containers(3+2+1) + regular(1)) = 7
				v1.ResourceCPU: resource.MustParse("7"),
			},
			initContainers: []v1.Container{
				{
					Name: "init-1",
					Resources: v1.ResourceRequirements{
						Limits: v1.ResourceList{
							v1.ResourceCPU: resource.MustParse("5"),
						},
					},
				},
				{
					Name:          "restartable-init-1",
					RestartPolicy: &restartAlways,
					Resources: v1.ResourceRequirements{
						Limits: v1.ResourceList{
							v1.ResourceCPU: resource.MustParse("1"),
						},
					},
				},
				{
					Name:          "restartable-init-2",
					RestartPolicy: &restartAlways,
					Resources: v1.ResourceRequirements{
						Limits: v1.ResourceList{
							v1.ResourceCPU: resource.MustParse("2"),
						},
					},
				},
				{
					Name:          "restartable-init-3",
					RestartPolicy: &restartAlways,
					Resources: v1.ResourceRequirements{
						Limits: v1.ResourceList{
							v1.ResourceCPU: resource.MustParse("3"),
						},
					},
				},
			},
			containers: []v1.Container{
				{
					Name: "container-1",
					Resources: v1.ResourceRequirements{
						Limits: v1.ResourceList{
							v1.ResourceCPU: resource.MustParse("1"),
						},
					},
				},
			},
		},
		{
			description: "multiple restartable and regular init containers",
			expectedLimits: v1.ResourceList{
				// init-2 requires 5 + the previously running restartable init
				// containers(1+2) = 8, the restartable init container that starts
				// after it doesn't count
				v1.ResourceCPU: resource.MustParse("8"),
			},
			initContainers: []v1.Container{
				{
					Name: "init-1",
					Resources: v1.ResourceRequirements{
						Limits: v1.ResourceList{
							v1.ResourceCPU: resource.MustParse("5"),
						},
					},
				},
				{
					Name:          "restartable-init-1",
					RestartPolicy: &restartAlways,
					Resources: v1.ResourceRequirements{
						Limits: v1.ResourceList{
							v1.ResourceCPU: resource.MustParse("1"),
						},
					},
				},
				{
					Name:          "restartable-init-2",
					RestartPolicy: &restartAlways,
					Resources: v1.ResourceRequirements{
						Limits: v1.ResourceList{
							v1.ResourceCPU: resource.MustParse("2"),
						},
					},
				},
				{
					Name: "init-2",
					Resources: v1.ResourceRequirements{
						Limits: v1.ResourceList{
							v1.ResourceCPU: resource.MustParse("5"),
						},
					},
				},
				{
					Name:          "restartable-init-3",
					RestartPolicy: &restartAlways,
					Resources: v1.ResourceRequirements{
						Limits: v1.ResourceList{
							v1.ResourceCPU: resource.MustParse("3"),
						},
					},
				},
			},
			containers: []v1.Container{
				{
					Name: "container-1",
					Resources: v1.ResourceRequirements{
						Limits: v1.ResourceList{
							v1.ResourceCPU: resource.MustParse("1"),
						},
					},
				},
			},
		},
		{
			description: "restartable-init, init and regular",
			expectedLimits: v1.ResourceList{
				v1.ResourceCPU: resource.MustParse("210"),
			},
			initContainers: []v1.Container{
				{
					Name:          "restartable-init-1",
					RestartPolicy: &restartAlways,
					Resources: v1.ResourceRequirements{
						Limits: v1.ResourceList{
							v1.ResourceCPU: resource.MustParse("10"),
						},
					},
				},
				{
					Name: "init-1",
					Resources: v1.ResourceRequirements{
						Limits: v1.ResourceList{
							v1.ResourceCPU: resource.MustParse("200"),
						},
					},
				},
			},
			containers: []v1.Container{
				{
					Name: "container-1",
					Resources: v1.ResourceRequirements{
						Limits: v1.ResourceList{
							v1.ResourceCPU: resource.MustParse("100"),
						},
					},
				},
			},
		},
		{
			description: "pod scaled up",
			expectedLimits: v1.ResourceList{
				v1.ResourceMemory: resource.MustParse("2Gi"),
			},
			options: PodResourcesOptions{UseStatusResources: true},
			containers: []v1.Container{
				{
					Name: "container-1",
					Resources: v1.ResourceRequirements{
						Limits: v1.ResourceList{
							v1.ResourceMemory: resource.MustParse("2Gi"),
						},
					},
				},
			},
			containerStatuses: []v1.ContainerStatus{
				{
					Name: "container-1",
					Resources: &v1.ResourceRequirements{
						Limits: v1.ResourceList{
							v1.ResourceMemory: resource.MustParse("1Gi"),
						},
					},
				},
			},
		},
		{
			description: "pod scaled down",
			expectedLimits: v1.ResourceList{
				v1.ResourceMemory: resource.MustParse("2Gi"),
			},
			options: PodResourcesOptions{UseStatusResources: true},
			containers: []v1.Container{
				{
					Name: "container-1",
					Resources: v1.ResourceRequirements{
						Limits: v1.ResourceList{
							v1.ResourceMemory: resource.MustParse("1Gi"),
						},
					},
				},
			},
			containerStatuses: []v1.ContainerStatus{
				{
					Name: "container-1",
					Resources: &v1.ResourceRequirements{
						Limits: v1.ResourceList{
							v1.ResourceMemory: resource.MustParse("2Gi"),
						},
					},
				},
			},
		},
		{
			description: "pod scaled down, don't use status",
			expectedLimits: v1.ResourceList{
				v1.ResourceMemory: resource.MustParse("1Gi"),
			},
			options: PodResourcesOptions{UseStatusResources: false},
			containers: []v1.Container{
				{
					Name: "container-1",
					Resources: v1.ResourceRequirements{
						Limits: v1.ResourceList{
							v1.ResourceMemory: resource.MustParse("1Gi"),
						},
					},
				},
			},
			containerStatuses: []v1.ContainerStatus{
				{
					Name: "container-1",
					Resources: &v1.ResourceRequirements{
						Limits: v1.ResourceList{
							v1.ResourceMemory: resource.MustParse("2Gi"),
						},
					},
				},
			},
		},
		{
			description: "pod scaled up with restartable init containers",
			expectedLimits: v1.ResourceList{
				v1.ResourceMemory: resource.MustParse("2Gi"),
			},
			options: PodResourcesOptions{UseStatusResources: true},
			initContainers: []v1.Container{
				{
					Name:          "restartable-init-1",
					RestartPolicy: &restartAlways,
					Resources: v1.ResourceRequirements{
						Limits: v1.ResourceList{
							v1.ResourceMemory: resource.MustParse("2Gi"),
						},
					},
				},
			},
			initContainerStatuses: []v1.ContainerStatus{
				{
					Name: "restartable-init-1",
					Resources: &v1.ResourceRequirements{
						Limits: v1.ResourceList{
							v1.ResourceMemory: resource.MustParse("1Gi"),
						},
					},
				},
			},
		},
		{
			description: "pod scaled down with restartable init containers",
			expectedLimits: v1.ResourceList{
				v1.ResourceMemory: resource.MustParse("2Gi"),
			},
			options: PodResourcesOptions{UseStatusResources: true},
			initContainers: []v1.Container{
				{
					Name:          "restartable-init-1",
					RestartPolicy: &restartAlways,
					Resources: v1.ResourceRequirements{
						Limits: v1.ResourceList{
							v1.ResourceMemory: resource.MustParse("1Gi"),
						},
					},
				},
			},
			initContainerStatuses: []v1.ContainerStatus{
				{
					Name: "restartable-init-1",
					Resources: &v1.ResourceRequirements{
						Limits: v1.ResourceList{
							v1.ResourceMemory: resource.MustParse("2Gi"),
						},
					},
				},
			},
		},
		{
			description: "pod scaled down with restartable init containers, don't use status",
			expectedLimits: v1.ResourceList{
				v1.ResourceMemory: resource.MustParse("1Gi"),
			},
			options: PodResourcesOptions{UseStatusResources: false},
			initContainers: []v1.Container{
				{
					Name:          "restartable-init-1",
					RestartPolicy: &restartAlways,
					Resources: v1.ResourceRequirements{
						Limits: v1.ResourceList{
							v1.ResourceMemory: resource.MustParse("1Gi"),
						},
					},
				},
			},
			initContainerStatuses: []v1.ContainerStatus{
				{
					Name: "restartable-init-1",
					Resources: &v1.ResourceRequirements{
						Limits: v1.ResourceList{
							v1.ResourceMemory: resource.MustParse("2Gi"),
						},
					},
				},
			},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.description, func(t *testing.T) {
			p := &v1.Pod{
				Spec: v1.PodSpec{
					Containers:     tc.containers,
					InitContainers: tc.initContainers,
					Overhead:       tc.overhead,
				},
				Status: v1.PodStatus{
					ContainerStatuses:     tc.containerStatuses,
					InitContainerStatuses: tc.initContainerStatuses,
				},
			}
			limits := PodLimits(p, tc.options)
			if diff := cmp.Diff(limits, tc.expectedLimits); diff != "" {
				t.Errorf("got=%v, want=%v, diff=%s", limits, tc.expectedLimits, diff)
			}
		})
	}
}

func TestIsPodLevelResourcesSet(t *testing.T) {
	testCases := []struct {
		name         string
		podResources *v1.ResourceRequirements
		expected     bool
	}{
		{
			name:     "nil resources struct",
			expected: false,
		},
		{
			name:         "empty resources struct",
			podResources: &v1.ResourceRequirements{},
			expected:     false,
		},
		{
			name: "only unsupported resource requests set",
			podResources: &v1.ResourceRequirements{
				Requests: v1.ResourceList{v1.ResourceEphemeralStorage: resource.MustParse("1Mi")},
			},
			expected: false,
		},
		{
			name: "only unsupported resource limits set",
			podResources: &v1.ResourceRequirements{
				Limits: v1.ResourceList{v1.ResourceEphemeralStorage: resource.MustParse("1Mi")},
			},
			expected: false,
		},
		{
			name: "unsupported and suported resources requests set",
			podResources: &v1.ResourceRequirements{
				Requests: v1.ResourceList{
					v1.ResourceEphemeralStorage: resource.MustParse("1Mi"),
					v1.ResourceCPU:              resource.MustParse("1m"),
				},
			},
			expected: true,
		},
		{
			name: "unsupported and suported resources limits set",
			podResources: &v1.ResourceRequirements{
				Limits: v1.ResourceList{
					v1.ResourceEphemeralStorage: resource.MustParse("1Mi"),
					v1.ResourceCPU:              resource.MustParse("1m"),
				},
			},
			expected: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			testPod := &v1.Pod{Spec: v1.PodSpec{Resources: tc.podResources}}
			if got := IsPodLevelResourcesSet(testPod); got != tc.expected {
				t.Errorf("got=%t, want=%t", got, tc.expected)
			}
		})
	}

}

func TestPodLevelResourceRequests(t *testing.T) {
	restartAlways := v1.ContainerRestartPolicyAlways
	testCases := []struct {
		name             string
		opts             PodResourcesOptions
		podResources     v1.ResourceRequirements
		overhead         v1.ResourceList
		initContainers   []v1.Container
		containers       []v1.Container
		expectedRequests v1.ResourceList
	}{
		{
			name:             "nil",
			expectedRequests: v1.ResourceList{},
		},
		{
			name:             "pod level memory resource with SkipPodLevelResources true",
			podResources:     v1.ResourceRequirements{Requests: v1.ResourceList{v1.ResourceMemory: resource.MustParse("2Mi")}},
			opts:             PodResourcesOptions{SkipPodLevelResources: true},
			expectedRequests: v1.ResourceList{},
		},
		{
			name:             "pod level memory resource with SkipPodLevelResources false",
			podResources:     v1.ResourceRequirements{Requests: v1.ResourceList{v1.ResourceMemory: resource.MustParse("2Mi")}},
			opts:             PodResourcesOptions{SkipPodLevelResources: false},
			expectedRequests: v1.ResourceList{v1.ResourceMemory: resource.MustParse("2Mi")},
		},
		{
			name:         "pod level memory and container level cpu resources with SkipPodLevelResources false",
			podResources: v1.ResourceRequirements{Requests: v1.ResourceList{v1.ResourceMemory: resource.MustParse("2Mi")}},
			containers: []v1.Container{
				{
					Resources: v1.ResourceRequirements{Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("2m")}},
				},
			},
			opts:             PodResourcesOptions{SkipPodLevelResources: false},
			expectedRequests: v1.ResourceList{v1.ResourceMemory: resource.MustParse("2Mi"), v1.ResourceCPU: resource.MustParse("2m")},
		},
		{
			name:         "pod level unsupported resources set at both pod-level and container-level with SkipPodLevelResources false",
			podResources: v1.ResourceRequirements{Requests: v1.ResourceList{v1.ResourceStorage: resource.MustParse("2Mi")}},
			containers: []v1.Container{
				{
					Resources: v1.ResourceRequirements{Requests: v1.ResourceList{v1.ResourceStorage: resource.MustParse("3Mi")}},
				},
			},
			opts:             PodResourcesOptions{SkipPodLevelResources: false},
			expectedRequests: v1.ResourceList{v1.ResourceStorage: resource.MustParse("3Mi")},
		},
		{
			name:         "pod level unsupported resources set at pod-level with SkipPodLevelResources false",
			podResources: v1.ResourceRequirements{Requests: v1.ResourceList{v1.ResourceStorage: resource.MustParse("2Mi")}},
			containers: []v1.Container{
				{
					Resources: v1.ResourceRequirements{Requests: v1.ResourceList{v1.ResourceMemory: resource.MustParse("3Mi")}},
				},
			},
			opts:             PodResourcesOptions{SkipPodLevelResources: false},
			expectedRequests: v1.ResourceList{v1.ResourceMemory: resource.MustParse("3Mi")},
		},
		{
			name: "only container level resources set with SkipPodLevelResources false",
			containers: []v1.Container{
				{
					Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{
							v1.ResourceMemory: resource.MustParse("3Mi"),
							v1.ResourceCPU:    resource.MustParse("2m"),
						},
					},
				},
			},
			opts:             PodResourcesOptions{SkipPodLevelResources: false},
			expectedRequests: v1.ResourceList{v1.ResourceMemory: resource.MustParse("3Mi"), v1.ResourceCPU: resource.MustParse("2m")},
		},
		{
			name: "both container-level and pod-level resources set with SkipPodLevelResources false",
			podResources: v1.ResourceRequirements{
				Requests: v1.ResourceList{
					v1.ResourceMemory: resource.MustParse("6Mi"),
					v1.ResourceCPU:    resource.MustParse("8m"),
				},
			},
			containers: []v1.Container{
				{
					Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{
							v1.ResourceMemory: resource.MustParse("3Mi"),
							v1.ResourceCPU:    resource.MustParse("2m"),
						},
					},
				},
			},
			opts:             PodResourcesOptions{SkipPodLevelResources: false},
			expectedRequests: v1.ResourceList{v1.ResourceMemory: resource.MustParse("6Mi"), v1.ResourceCPU: resource.MustParse("8m")},
		},
		{
			name: "container-level resources and init container set with SkipPodLevelResources false",
			containers: []v1.Container{
				{
					Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{
							v1.ResourceMemory: resource.MustParse("3Mi"),
							v1.ResourceCPU:    resource.MustParse("2m"),
						},
					},
				},
			},
			initContainers: []v1.Container{
				{
					Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{
							v1.ResourceMemory: resource.MustParse("5Mi"),
							v1.ResourceCPU:    resource.MustParse("4m"),
						},
					},
				},
			},
			opts:             PodResourcesOptions{SkipPodLevelResources: false},
			expectedRequests: v1.ResourceList{v1.ResourceMemory: resource.MustParse("5Mi"), v1.ResourceCPU: resource.MustParse("4m")},
		},
		{
			name: "container-level resources and init container set with SkipPodLevelResources false",
			containers: []v1.Container{
				{
					Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{
							v1.ResourceMemory: resource.MustParse("3Mi"),
							v1.ResourceCPU:    resource.MustParse("2m"),
						},
					},
				},
			},
			initContainers: []v1.Container{
				{
					Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{
							v1.ResourceMemory: resource.MustParse("5Mi"),
							v1.ResourceCPU:    resource.MustParse("4m"),
						},
					},
				},
			},
			opts:             PodResourcesOptions{SkipPodLevelResources: true},
			expectedRequests: v1.ResourceList{v1.ResourceMemory: resource.MustParse("5Mi"), v1.ResourceCPU: resource.MustParse("4m")},
		},
		{
			name: "container-level resources and sidecar container set with SkipPodLevelResources false",
			containers: []v1.Container{
				{
					Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{
							v1.ResourceMemory: resource.MustParse("3Mi"),
							v1.ResourceCPU:    resource.MustParse("2m"),
						},
					},
				},
			},
			initContainers: []v1.Container{
				{
					Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{
							v1.ResourceMemory: resource.MustParse("5Mi"),
							v1.ResourceCPU:    resource.MustParse("4m"),
						},
					},
					RestartPolicy: &restartAlways,
				},
			},
			opts:             PodResourcesOptions{SkipPodLevelResources: false},
			expectedRequests: v1.ResourceList{v1.ResourceMemory: resource.MustParse("8Mi"), v1.ResourceCPU: resource.MustParse("6m")},
		},
		{
			name: "container-level resources, init and sidecar container set with SkipPodLevelResources false",
			containers: []v1.Container{
				{
					Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{
							v1.ResourceMemory: resource.MustParse("3Mi"),
							v1.ResourceCPU:    resource.MustParse("2m"),
						},
					},
				},
			},
			initContainers: []v1.Container{
				{
					Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{
							v1.ResourceMemory: resource.MustParse("5Mi"),
							v1.ResourceCPU:    resource.MustParse("4m"),
						},
					},
					RestartPolicy: &restartAlways,
				},
				{
					Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{
							v1.ResourceMemory: resource.MustParse("6Mi"),
							v1.ResourceCPU:    resource.MustParse("8m"),
						},
					},
				},
			},
			opts:             PodResourcesOptions{SkipPodLevelResources: false},
			expectedRequests: v1.ResourceList{v1.ResourceMemory: resource.MustParse("11Mi"), v1.ResourceCPU: resource.MustParse("12m")},
		},
		{
			name: "pod-level resources, container-level resources, init and sidecar container set with SkipPodLevelResources false",
			podResources: v1.ResourceRequirements{
				Requests: v1.ResourceList{
					v1.ResourceMemory: resource.MustParse("15Mi"),
					v1.ResourceCPU:    resource.MustParse("18m"),
				},
			},
			containers: []v1.Container{
				{
					Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{
							v1.ResourceMemory: resource.MustParse("3Mi"),
							v1.ResourceCPU:    resource.MustParse("2m"),
						},
					},
				},
			},
			initContainers: []v1.Container{
				{
					Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{
							v1.ResourceMemory: resource.MustParse("5Mi"),
							v1.ResourceCPU:    resource.MustParse("4m"),
						},
					},
					RestartPolicy: &restartAlways,
				},
				{
					Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{
							v1.ResourceMemory: resource.MustParse("6Mi"),
							v1.ResourceCPU:    resource.MustParse("8m"),
						},
					},
				},
			},
			opts:             PodResourcesOptions{SkipPodLevelResources: false},
			expectedRequests: v1.ResourceList{v1.ResourceMemory: resource.MustParse("15Mi"), v1.ResourceCPU: resource.MustParse("18m")},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			podReqs := PodRequests(getPodLevelResourcesPod(tc.podResources, tc.overhead, tc.containers, tc.initContainers), tc.opts)
			if diff := cmp.Diff(podReqs, tc.expectedRequests); diff != "" {
				t.Errorf("got=%v, want=%v, diff=%s", podReqs, tc.expectedRequests, diff)
			}
		})
	}
}

func TestAggregateContainerRequestsAndLimits(t *testing.T) {
	restartAlways := v1.ContainerRestartPolicyAlways
	cases := []struct {
		containers       []v1.Container
		initContainers   []v1.Container
		name             string
		expectedRequests v1.ResourceList
		expectedLimits   v1.ResourceList
	}{
		{
			name: "one container with limits",
			containers: []v1.Container{
				{
					Resources: v1.ResourceRequirements{
						Limits: v1.ResourceList{v1.ResourceName(v1.ResourceCPU): resource.MustParse("9")},
					},
				},
			},
			expectedRequests: v1.ResourceList{},
			expectedLimits: v1.ResourceList{
				v1.ResourceName(v1.ResourceCPU): resource.MustParse("9"),
			},
		},
		{
			name: "two containers with limits",
			containers: []v1.Container{
				{
					Resources: v1.ResourceRequirements{
						Limits: v1.ResourceList{v1.ResourceName(v1.ResourceCPU): resource.MustParse("9")},
					},
				},
				{
					Resources: v1.ResourceRequirements{
						Limits: v1.ResourceList{v1.ResourceName(v1.ResourceCPU): resource.MustParse("9")},
					},
				},
			},
			expectedRequests: v1.ResourceList{},
			expectedLimits: v1.ResourceList{
				v1.ResourceName(v1.ResourceCPU): resource.MustParse("18"),
			},
		},
		{
			name: "one container with requests",
			containers: []v1.Container{
				{
					Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{v1.ResourceName(v1.ResourceCPU): resource.MustParse("9")},
					},
				},
			},
			expectedRequests: v1.ResourceList{
				v1.ResourceName(v1.ResourceCPU): resource.MustParse("9"),
			},
			expectedLimits: v1.ResourceList{},
		},
		{
			name: "two containers with requests",
			containers: []v1.Container{
				{
					Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{v1.ResourceName(v1.ResourceCPU): resource.MustParse("9")},
					},
				},
				{
					Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{v1.ResourceName(v1.ResourceCPU): resource.MustParse("9")},
					},
				},
			},
			expectedRequests: v1.ResourceList{
				v1.ResourceName(v1.ResourceCPU): resource.MustParse("18"),
			},
			expectedLimits: v1.ResourceList{},
		},
		{
			name: "regular and init containers with requests",
			containers: []v1.Container{
				{
					Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{v1.ResourceName(v1.ResourceCPU): resource.MustParse("9")},
					},
				},
			},
			initContainers: []v1.Container{
				{
					Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{v1.ResourceName(v1.ResourceCPU): resource.MustParse("9")},
					},
				},
			},
			expectedRequests: v1.ResourceList{
				v1.ResourceName(v1.ResourceCPU): resource.MustParse("9"),
			},
			expectedLimits: v1.ResourceList{},
		},
		{
			name: "regular, init and sidecar containers with requests",
			containers: []v1.Container{
				{
					Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{v1.ResourceName(v1.ResourceCPU): resource.MustParse("9")},
					},
				},
			},
			initContainers: []v1.Container{
				{
					Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{v1.ResourceName(v1.ResourceCPU): resource.MustParse("8")},
					},
					RestartPolicy: &restartAlways,
				},
				{
					Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{v1.ResourceName(v1.ResourceCPU): resource.MustParse("6")},
					},
				},
			},
			expectedRequests: v1.ResourceList{
				v1.ResourceName(v1.ResourceCPU): resource.MustParse("17"),
			},
			expectedLimits: v1.ResourceList{},
		},
		{
			name: "regular and init containers with limits",
			containers: []v1.Container{
				{
					Resources: v1.ResourceRequirements{
						Limits: v1.ResourceList{v1.ResourceName(v1.ResourceCPU): resource.MustParse("9")},
					},
				},
			},
			initContainers: []v1.Container{
				{
					Resources: v1.ResourceRequirements{
						Limits: v1.ResourceList{v1.ResourceName(v1.ResourceCPU): resource.MustParse("9")},
					},
				},
			},
			expectedRequests: v1.ResourceList{},
			expectedLimits: v1.ResourceList{
				v1.ResourceName(v1.ResourceCPU): resource.MustParse("9"),
			},
		},
		{
			name: "regular, init and sidecar containers with limits",
			containers: []v1.Container{
				{
					Resources: v1.ResourceRequirements{
						Limits: v1.ResourceList{v1.ResourceName(v1.ResourceCPU): resource.MustParse("9")},
					},
				},
			},
			initContainers: []v1.Container{
				{
					Resources: v1.ResourceRequirements{
						Limits: v1.ResourceList{v1.ResourceName(v1.ResourceCPU): resource.MustParse("8")},
					},
					RestartPolicy: &restartAlways,
				},
				{
					Resources: v1.ResourceRequirements{
						Limits: v1.ResourceList{v1.ResourceName(v1.ResourceCPU): resource.MustParse("6")},
					},
				},
			},
			expectedRequests: v1.ResourceList{},
			expectedLimits: v1.ResourceList{
				v1.ResourceName(v1.ResourceCPU): resource.MustParse("17"),
			},
		},
	}

	for idx, tc := range cases {
		testPod := &v1.Pod{Spec: v1.PodSpec{Containers: tc.containers, InitContainers: tc.initContainers}}
		resRequests := AggregateContainerRequests(testPod, PodResourcesOptions{})
		resLimits := AggregateContainerLimits(testPod, PodResourcesOptions{})

		if !equality.Semantic.DeepEqual(tc.expectedRequests, resRequests) {
			t.Errorf("test case failure[%d]: %v, requests:\n expected:\t%v\ngot\t\t%v", idx, tc.name, tc.expectedRequests, resRequests)
		}

		if !equality.Semantic.DeepEqual(tc.expectedLimits, resLimits) {
			t.Errorf("test case failure[%d]: %v, limits:\n expected:\t%v\ngot\t\t%v", idx, tc.name, tc.expectedLimits, resLimits)
		}
	}
}

type podResources struct {
	cpuRequest, cpuLimit, memoryRequest, memoryLimit, cpuOverhead, memoryOverhead string
}

func getPodLevelResourcesPod(podResources v1.ResourceRequirements, overhead v1.ResourceList, containers, initContainers []v1.Container) *v1.Pod {
	return &v1.Pod{
		Spec: v1.PodSpec{
			Resources:      &podResources,
			Containers:     containers,
			InitContainers: initContainers,
			Overhead:       overhead,
		},
	}
}

// TODO(ndixita): refactor to re-use getPodResourcesPod()
func getPod(cname string, resources podResources) *v1.Pod {
	r := v1.ResourceRequirements{
		Limits:   make(v1.ResourceList),
		Requests: make(v1.ResourceList),
	}

	overhead := make(v1.ResourceList)

	if resources.cpuLimit != "" {
		r.Limits[v1.ResourceCPU] = resource.MustParse(resources.cpuLimit)
	}
	if resources.memoryLimit != "" {
		r.Limits[v1.ResourceMemory] = resource.MustParse(resources.memoryLimit)
	}
	if resources.cpuRequest != "" {
		r.Requests[v1.ResourceCPU] = resource.MustParse(resources.cpuRequest)
	}
	if resources.memoryRequest != "" {
		r.Requests[v1.ResourceMemory] = resource.MustParse(resources.memoryRequest)
	}
	if resources.cpuOverhead != "" {
		overhead[v1.ResourceCPU] = resource.MustParse(resources.cpuOverhead)
	}
	if resources.memoryOverhead != "" {
		overhead[v1.ResourceMemory] = resource.MustParse(resources.memoryOverhead)
	}

	return &v1.Pod{
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name:      cname,
					Resources: r,
				},
			},
			InitContainers: []v1.Container{
				{
					Name:      "init-" + cname,
					Resources: r,
				},
			},
			Overhead: overhead,
		},
	}
}
