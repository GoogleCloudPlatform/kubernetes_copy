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

package kuberuntime

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/flowcontrol"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
	apitest "k8s.io/cri-api/pkg/apis/testing"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	"k8s.io/utils/ptr"
)

func stringSliceContains(slice []string, s string) bool {
	for _, v := range slice {
		if v == s {
			return true
		}
	}
	return false
}

func TestSyncTerminatingPod(t *testing.T) {
	testCases := []struct {
		desc  string
		pod   *v1.Pod
		setup func(*testing.T, *apitest.FakeRuntimeService)

		extraTest func(*testing.T, *apitest.FakeRuntimeService)

		expectedNumStoppedContainers int
		expectedTerminationOrder     []string
	}{
		{
			desc: "pod with 2 containers and 1 ephemeral container",
			pod: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					UID:       "12345678",
					Name:      "foo",
					Namespace: "new",
				},
				Spec: v1.PodSpec{
					Containers: []v1.Container{
						{
							Name:  "foo1",
							Image: "busybox",
						},
						{
							Name:  "foo2",
							Image: "busybox",
						},
					},
					EphemeralContainers: []v1.EphemeralContainer{
						{
							EphemeralContainerCommon: v1.EphemeralContainerCommon{
								Name:  "debug",
								Image: "busybox",
							},
						},
					},
					TerminationGracePeriodSeconds: ptr.To(int64(30)),
				},
			},
			expectedNumStoppedContainers: 3,
		},
		{
			desc: "pod with 1 containers and 3 restartable init containers",
			pod: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					UID:       "12345678",
					Name:      "foo",
					Namespace: "new",
				},
				Spec: v1.PodSpec{
					InitContainers: []v1.Container{
						{
							Name:          "restartable-init1",
							Image:         "busybox",
							RestartPolicy: ptr.To(v1.ContainerRestartPolicyAlways),
						},
						{
							Name:          "restartable-init2",
							Image:         "busybox",
							RestartPolicy: ptr.To(v1.ContainerRestartPolicyAlways),
						},
						{
							Name:          "restartable-init3",
							Image:         "busybox",
							RestartPolicy: ptr.To(v1.ContainerRestartPolicyAlways),
						},
					},
					Containers: []v1.Container{
						{
							Name:  "foo1",
							Image: "busybox",
						},
					},
					TerminationGracePeriodSeconds: ptr.To(int64(30)),
				},
			},
			expectedNumStoppedContainers: 4,
			expectedTerminationOrder: []string{
				"foo1",
				"restartable-init3",
				"restartable-init2",
				"restartable-init1",
			},
		},
		{
			desc: "pod with a slowly terminating container and 2 restartable init containers",
			pod: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					UID:       "12345678",
					Name:      "foo",
					Namespace: "new",
				},
				Spec: v1.PodSpec{
					InitContainers: []v1.Container{
						{
							Name:            "restartable-init1",
							Image:           "busybox",
							RestartPolicy:   ptr.To(v1.ContainerRestartPolicyAlways),
							ImagePullPolicy: v1.PullIfNotPresent,
						},
						{
							Name:          "restartable-init2",
							Image:         "busybox",
							RestartPolicy: ptr.To(v1.ContainerRestartPolicyAlways),
						},
					},
					Containers: []v1.Container{
						{
							Name:  "foo1",
							Image: "busybox",
						},
					},
					TerminationGracePeriodSeconds: ptr.To(int64(30)),
				},
			},
			setup: func(t *testing.T, fakeRuntime *apitest.FakeRuntimeService) {
				// assume that the regular container takes 5 seconds to stop
				for _, c := range fakeRuntime.Containers {
					if c.Metadata.Name == "foo1" {
						c.TerminationDuration = 3 * time.Second
					}
				}
				t.Logf("A restartable init container %q is crashed for some reason", "restartable-init1")
				for _, c := range fakeRuntime.Containers {
					if c.Metadata.Name == "restartable-init1" {
						c.State = runtimeapi.ContainerState_CONTAINER_EXITED
					}
				}
			},
			extraTest: func(t *testing.T, fakeRuntime *apitest.FakeRuntimeService) {
				t.Logf("Check the restartable init containers restart during the pod termination due to the slow termination of the main container")
				assert.True(t, stringSliceContains(fakeRuntime.Called, "CreateContainer"))
				assert.True(t, stringSliceContains(fakeRuntime.Called, "StartContainer"))
			},
			expectedNumStoppedContainers: 4,
			expectedTerminationOrder: []string{
				"restartable-init1",
				"foo1",
				"restartable-init2",
				"restartable-init1",
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.desc, func(t *testing.T) {
			ctx := context.Background()
			fakeRuntime, _, m, err := createTestRuntimeManager()
			require.NoError(t, err)

			// Set fake sandbox and fake containers to fakeRuntime.
			fakeSandbox, _ := makeAndSetFakePod(t, m, fakeRuntime, tc.pod)

			if tc.setup != nil {
				fakeRuntime.Lock()
				tc.setup(t, fakeRuntime)
				fakeRuntime.Unlock()
			}

			backoff := flowcontrol.NewBackOff(time.Second, time.Minute)

			t.Log("Waiting for the pod to be stopped")
			for {
				// Convert the fakeContainers to kubecontainer.Container
				fakeRuntime.Lock()
				containers := make([]*kubecontainer.Status, 0, len(fakeRuntime.Containers))
				for id, c := range fakeRuntime.Containers {
					containers = append(containers, &kubecontainer.Status{
						ID: kubecontainer.ContainerID{
							ID: id,
						},
						Name:  c.Metadata.Name,
						State: toKubeContainerState(c.State),
						// convert int64 to time.Time
						CreatedAt:  time.Unix(0, c.CreatedAt),
						FinishedAt: time.Unix(0, c.FinishedAt),
						Image:      c.Image.Image,
						ImageRef:   c.ImageRef,
					})
				}
				fakeRuntime.Unlock()

				sort.Slice(containers, func(i, j int) bool {
					// Sort containers by creation time in descending order
					return containers[i].CreatedAt.After(containers[j].CreatedAt)
				})

				podStatus := &kubecontainer.PodStatus{
					ID:                tc.pod.UID,
					Name:              tc.pod.Name,
					Namespace:         tc.pod.Namespace,
					ContainerStatuses: containers,
					SandboxStatuses: []*runtimeapi.PodSandboxStatus{
						&fakeSandbox.PodSandboxStatus,
					},
				}

				stopped, err := m.SyncTerminatingPod(ctx, tc.pod, podStatus, nil, []v1.Secret{}, backoff, true)
				require.NoError(t, err)
				if stopped {
					break
				}
				time.Sleep(100 * time.Millisecond)
			}

			t.Log("Check all containers and sandboxes are stopped")
			assert.Len(t, fakeRuntime.Containers, tc.expectedNumStoppedContainers)
			assert.Len(t, fakeRuntime.Sandboxes, 1)
			for _, sandbox := range fakeRuntime.Sandboxes {
				assert.Equal(t, runtimeapi.PodSandboxState_SANDBOX_NOTREADY, sandbox.State)
			}
			for _, c := range fakeRuntime.Containers {
				assert.Equal(t, runtimeapi.ContainerState_CONTAINER_EXITED, c.State, "unexpected container state, container: %v", c.Metadata.Name)
			}

			t.Log("Sort containers by finishedAt to check termination order")
			sortedContainers := make([]*apitest.FakeContainer, 0, len(fakeRuntime.Containers))
			for _, c := range fakeRuntime.Containers {
				sortedContainers = append(sortedContainers, c)
			}
			sort.Slice(sortedContainers, func(i, j int) bool {
				return sortedContainers[i].FinishedAt < sortedContainers[j].FinishedAt
			})
			for i, c := range sortedContainers {
				t.Logf("container %d: %s, finishedAt: %v", i, c.Metadata.Name, c.FinishedAt)
			}

			t.Log("Check all containers terminated in the right order")
			if len(tc.expectedTerminationOrder) != 0 {
				assert.Equal(t, len(tc.expectedTerminationOrder), len(sortedContainers))
				for i, c := range sortedContainers {
					assert.Equal(t, tc.expectedTerminationOrder[i], c.Metadata.Name, "unexpected container termination order: index: %d", i)
				}
			}

			if tc.extraTest != nil {
				tc.extraTest(t, fakeRuntime)
			}
		})
	}
}
