/*
Copyright 2016 The Kubernetes Authors.

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
	"github.com/stretchr/testify/assert"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
	containertest "k8s.io/kubernetes/pkg/kubelet/container/testing"
	"os"
	"path/filepath"
	"testing"
)

func TestSandboxGC(t *testing.T) {
	fakeRuntime, _, m, err := createTestRuntimeManager()
	assert.NoError(t, err)

	podStateProvider := m.containerGC.podStateProvider.(*fakePodStateProvider)
	makeGCSandbox := func(pod *v1.Pod, attempt uint32, state runtimeapi.PodSandboxState, hasRunningContainers, isTerminating bool, createdAt int64) sandboxTemplate {
		return sandboxTemplate{
			pod:         pod,
			state:       state,
			attempt:     attempt,
			createdAt:   createdAt,
			running:     hasRunningContainers,
			terminating: isTerminating,
		}
	}

	pods := []*v1.Pod{
		makeTestPod("foo1", "new", "1234", []v1.Container{
			makeTestContainer("bar1", "busybox"),
			makeTestContainer("bar2", "busybox"),
		}),
		makeTestPod("foo2", "new", "5678", []v1.Container{
			makeTestContainer("bar3", "busybox"),
		}),
		makeTestPod("deleted", "new", "9012", []v1.Container{
			makeTestContainer("bar4", "busybox"),
		}),
	}

	for _, test := range []struct {
		description          string              // description of the test case
		sandboxes            []sandboxTemplate   // templates of sandboxes
		containers           []containerTemplate // templates of containers
		remain               []int               // template indexes of remaining sandboxes
		evictTerminatingPods bool
	}{
		{
			description: "notready sandboxes without containers for deleted pods should be garbage collected.",
			sandboxes: []sandboxTemplate{
				makeGCSandbox(pods[2], 0, runtimeapi.PodSandboxState_SANDBOX_NOTREADY, false, false, 0),
			},
			containers:           []containerTemplate{},
			remain:               []int{},
			evictTerminatingPods: false,
		},
		{
			description: "ready sandboxes without containers for deleted pods should not be garbage collected.",
			sandboxes: []sandboxTemplate{
				makeGCSandbox(pods[2], 0, runtimeapi.PodSandboxState_SANDBOX_READY, false, false, 0),
			},
			containers:           []containerTemplate{},
			remain:               []int{0},
			evictTerminatingPods: false,
		},
		{
			description: "sandboxes for existing pods should not be garbage collected.",
			sandboxes: []sandboxTemplate{
				makeGCSandbox(pods[0], 0, runtimeapi.PodSandboxState_SANDBOX_READY, true, false, 0),
				makeGCSandbox(pods[1], 0, runtimeapi.PodSandboxState_SANDBOX_NOTREADY, true, false, 0),
			},
			containers:           []containerTemplate{},
			remain:               []int{0, 1},
			evictTerminatingPods: false,
		},
		{
			description: "older exited sandboxes without containers for existing pods should be garbage collected if there are more than one exited sandboxes.",
			sandboxes: []sandboxTemplate{
				makeGCSandbox(pods[0], 1, runtimeapi.PodSandboxState_SANDBOX_NOTREADY, true, false, 1),
				makeGCSandbox(pods[0], 0, runtimeapi.PodSandboxState_SANDBOX_NOTREADY, true, false, 0),
			},
			containers:           []containerTemplate{},
			remain:               []int{0},
			evictTerminatingPods: false,
		},
		{
			description: "older exited sandboxes with containers for existing pods should not be garbage collected even if there are more than one exited sandboxes.",
			sandboxes: []sandboxTemplate{
				makeGCSandbox(pods[0], 1, runtimeapi.PodSandboxState_SANDBOX_NOTREADY, true, false, 1),
				makeGCSandbox(pods[0], 0, runtimeapi.PodSandboxState_SANDBOX_NOTREADY, true, false, 0),
			},
			containers: []containerTemplate{
				{pod: pods[0], container: &pods[0].Spec.Containers[0], sandboxAttempt: 0, state: runtimeapi.ContainerState_CONTAINER_EXITED},
			},
			remain:               []int{0, 1},
			evictTerminatingPods: false,
		},
		{
			description: "non-running sandboxes for existing pods should be garbage collected if evictTerminatingPods is set.",
			sandboxes: []sandboxTemplate{
				makeGCSandbox(pods[0], 0, runtimeapi.PodSandboxState_SANDBOX_READY, true, true, 0),
				makeGCSandbox(pods[1], 0, runtimeapi.PodSandboxState_SANDBOX_NOTREADY, true, true, 0),
			},
			containers:           []containerTemplate{},
			remain:               []int{0},
			evictTerminatingPods: true,
		},
		{
			description: "sandbox with containers should not be garbage collected.",
			sandboxes: []sandboxTemplate{
				makeGCSandbox(pods[0], 0, runtimeapi.PodSandboxState_SANDBOX_NOTREADY, false, false, 0),
			},
			containers: []containerTemplate{
				{pod: pods[0], container: &pods[0].Spec.Containers[0], state: runtimeapi.ContainerState_CONTAINER_EXITED},
			},
			remain:               []int{0},
			evictTerminatingPods: false,
		},
		{
			description: "multiple sandboxes should be handled properly.",
			sandboxes: []sandboxTemplate{
				// running sandbox.
				makeGCSandbox(pods[0], 1, runtimeapi.PodSandboxState_SANDBOX_READY, true, false, 1),
				// exited sandbox without containers.
				makeGCSandbox(pods[0], 0, runtimeapi.PodSandboxState_SANDBOX_NOTREADY, true, false, 0),
				// exited sandbox with containers.
				makeGCSandbox(pods[1], 1, runtimeapi.PodSandboxState_SANDBOX_NOTREADY, true, false, 1),
				// exited sandbox without containers.
				makeGCSandbox(pods[1], 0, runtimeapi.PodSandboxState_SANDBOX_NOTREADY, true, false, 0),
				// exited sandbox without containers for deleted pods.
				makeGCSandbox(pods[2], 0, runtimeapi.PodSandboxState_SANDBOX_NOTREADY, false, true, 0),
			},
			containers: []containerTemplate{
				{pod: pods[1], container: &pods[1].Spec.Containers[0], sandboxAttempt: 1, state: runtimeapi.ContainerState_CONTAINER_EXITED},
			},
			remain:               []int{0, 2},
			evictTerminatingPods: false,
		},
	} {
		t.Run(test.description, func(t *testing.T) {
			ctx := context.Background()
			podStateProvider.removed = make(map[types.UID]struct{})
			podStateProvider.terminated = make(map[types.UID]struct{})
			fakeSandboxes := makeFakePodSandboxes(t, m, test.sandboxes)
			fakeContainers := makeFakeContainers(t, m, test.containers)
			for _, s := range test.sandboxes {
				if !s.running && s.pod.Name == "deleted" {
					podStateProvider.removed[s.pod.UID] = struct{}{}
				}
				if s.terminating {
					podStateProvider.terminated[s.pod.UID] = struct{}{}
				}
			}
			fakeRuntime.SetFakeSandboxes(fakeSandboxes)
			fakeRuntime.SetFakeContainers(fakeContainers)

			err := m.containerGC.evictSandboxes(ctx, test.evictTerminatingPods)
			assert.NoError(t, err)
			realRemain, err := fakeRuntime.ListPodSandbox(ctx, nil)
			assert.NoError(t, err)
			assert.Len(t, realRemain, len(test.remain))
			for _, remain := range test.remain {
				resp, err := fakeRuntime.PodSandboxStatus(ctx, fakeSandboxes[remain].Id, false)
				assert.NoError(t, err)
				assert.Equal(t, &fakeSandboxes[remain].PodSandboxStatus, resp.Status)
			}
		})
	}
}

func makeGCContainer(podName, containerName string, attempt int, createdAt int64, state runtimeapi.ContainerState) containerTemplate {
	container := makeTestContainer(containerName, "test-image")
	pod := makeTestPod(podName, "test-ns", podName, []v1.Container{container})
	return containerTemplate{
		pod:       pod,
		container: &container,
		attempt:   attempt,
		createdAt: createdAt,
		state:     state,
	}
}

func TestContainerGC(t *testing.T) {
	fakeRuntime, _, m, err := createTestRuntimeManager()
	assert.NoError(t, err)

	podStateProvider := m.containerGC.podStateProvider.(*fakePodStateProvider)

	for _, test := range []struct {
		description          string              // description of the test case
		containers           []containerTemplate // templates of containers
		remain               []int               // template indexes of remaining containers
		evictTerminatingPods bool
		allSourcesReady      bool
	}{
		{
			description: "at least a non-running container is retained",
			containers: []containerTemplate{
				makeGCContainer("foo", "bar", 0, 0, runtimeapi.ContainerState_CONTAINER_EXITED),
			},
			remain:               []int{0},
			evictTerminatingPods: false,
			allSourcesReady:      true,
		},
		{
			description: "only the most recently created container will not be removed When there are no running containers within a pod.",
			containers: []containerTemplate{
				makeGCContainer("foo", "bar", 4, 4, runtimeapi.ContainerState_CONTAINER_EXITED),
				makeGCContainer("foo", "bar", 3, 3, runtimeapi.ContainerState_CONTAINER_EXITED),
				makeGCContainer("foo", "bar", 2, 2, runtimeapi.ContainerState_CONTAINER_EXITED),
				makeGCContainer("foo", "bar", 1, 1, runtimeapi.ContainerState_CONTAINER_EXITED),
				makeGCContainer("foo", "bar", 0, 0, runtimeapi.ContainerState_CONTAINER_EXITED),
			},
			remain:               []int{0},
			evictTerminatingPods: false,
			allSourcesReady:      true,
		},
		{
			description: "retain one running and one non-running container.",
			containers: []containerTemplate{
				makeGCContainer("foo", "bar", 2, 2, runtimeapi.ContainerState_CONTAINER_EXITED),
				makeGCContainer("foo", "bar", 1, 1, runtimeapi.ContainerState_CONTAINER_EXITED),
				makeGCContainer("foo", "bar", 0, 0, runtimeapi.ContainerState_CONTAINER_RUNNING),
			},
			remain:               []int{0, 2},
			evictTerminatingPods: false,
			allSourcesReady:      true,
		},
		{
			description: "gc policy should apply per (UID, container) pair",
			containers: []containerTemplate{
				makeGCContainer("foo", "bar", 2, 2, runtimeapi.ContainerState_CONTAINER_EXITED),
				makeGCContainer("foo", "bar", 1, 1, runtimeapi.ContainerState_CONTAINER_EXITED),
				makeGCContainer("foo", "bar", 0, 0, runtimeapi.ContainerState_CONTAINER_EXITED),
				makeGCContainer("foo1", "baz", 2, 2, runtimeapi.ContainerState_CONTAINER_EXITED),
				makeGCContainer("foo1", "baz", 1, 1, runtimeapi.ContainerState_CONTAINER_EXITED),
				makeGCContainer("foo1", "baz", 0, 0, runtimeapi.ContainerState_CONTAINER_EXITED),
				makeGCContainer("foo2", "bar", 2, 2, runtimeapi.ContainerState_CONTAINER_EXITED),
				makeGCContainer("foo2", "bar", 1, 1, runtimeapi.ContainerState_CONTAINER_EXITED),
				makeGCContainer("foo2", "bar", 0, 0, runtimeapi.ContainerState_CONTAINER_EXITED),
			},
			remain:               []int{0, 3, 6},
			evictTerminatingPods: false,
			allSourcesReady:      true,
		},
		{
			description: "all non-running containers should be removed when evictTerminatingPods is set",
			containers: []containerTemplate{
				makeGCContainer("foo", "bar", 2, 2, runtimeapi.ContainerState_CONTAINER_EXITED),
				makeGCContainer("foo", "bar", 1, 1, runtimeapi.ContainerState_CONTAINER_EXITED),
				makeGCContainer("foo1", "bar1", 2, 2, runtimeapi.ContainerState_CONTAINER_EXITED),
				makeGCContainer("foo1", "bar1", 1, 1, runtimeapi.ContainerState_CONTAINER_EXITED),
				makeGCContainer("running", "bar2", 1, 1, runtimeapi.ContainerState_CONTAINER_EXITED),
				makeGCContainer("foo3", "bar3", 0, 0, runtimeapi.ContainerState_CONTAINER_RUNNING),
			},
			remain:               []int{4, 5},
			evictTerminatingPods: true,
			allSourcesReady:      true,
		},
		{
			description: "containers for deleted pods may not be removed if allSourcesReady is set false ",
			containers: []containerTemplate{
				makeGCContainer("deleted", "bar1", 0, 0, runtimeapi.ContainerState_CONTAINER_EXITED),
			},
			remain:               []int{0},
			evictTerminatingPods: true,
			allSourcesReady:      false,
		},
	} {
		t.Run(test.description, func(t *testing.T) {
			ctx := context.Background()
			podStateProvider.removed = make(map[types.UID]struct{})
			podStateProvider.terminated = make(map[types.UID]struct{})
			fakeContainers := makeFakeContainers(t, m, test.containers)
			for _, s := range test.containers {
				if s.pod.Name == "deleted" {
					podStateProvider.removed[s.pod.UID] = struct{}{}
				}
				if s.pod.Name != "running" {
					podStateProvider.terminated[s.pod.UID] = struct{}{}
				}
			}
			fakeRuntime.SetFakeContainers(fakeContainers)

			err := m.containerGC.evictContainers(ctx, test.allSourcesReady, test.evictTerminatingPods)
			assert.NoError(t, err)
			realRemain, err := fakeRuntime.ListContainers(ctx, nil)
			assert.NoError(t, err)
			assert.Len(t, realRemain, len(test.remain))
			for _, remain := range test.remain {
				resp, err := fakeRuntime.ContainerStatus(ctx, fakeContainers[remain].Id, false)
				assert.NoError(t, err)
				assert.Equal(t, &fakeContainers[remain].ContainerStatus, resp.Status)
			}
		})
	}
}

// Notice that legacy container symlink is not tested since it may be deprecated soon.
func TestPodLogDirectoryGC(t *testing.T) {
	ctx := context.Background()
	_, _, m, err := createTestRuntimeManager()
	assert.NoError(t, err)
	fakeOS := m.osInterface.(*containertest.FakeOS)
	podStateProvider := m.containerGC.podStateProvider.(*fakePodStateProvider)

	// pod log directories without corresponding pods should be removed.
	files := []string{"123", "456", "789", "012", "name_namespace_321", "name_namespace_654"}
	podLogsDirectory := "/var/log/pods"
	removed := []string{
		filepath.Join(podLogsDirectory, "789"),
		filepath.Join(podLogsDirectory, "012"),
		filepath.Join(podLogsDirectory, "name_namespace_654"),
	}
	podStateProvider.removed["012"] = struct{}{}
	podStateProvider.removed["789"] = struct{}{}
	podStateProvider.removed["654"] = struct{}{}

	fakeOS.ReadDirFn = func(string) ([]os.DirEntry, error) {
		var dirEntries []os.DirEntry
		for _, file := range files {
			mockDE := containertest.NewMockDirEntry(t)
			mockDE.EXPECT().Name().Return(file)
			dirEntries = append(dirEntries, mockDE)
		}
		return dirEntries, nil
	}

	// allSourcesReady == true, pod log directories without corresponding pod should be removed.
	err = m.containerGC.evictPodLogsDirectories(ctx, true)
	assert.NoError(t, err)
	assert.Equal(t, removed, fakeOS.Removes)

	// allSourcesReady == false, pod log directories should not be removed.
	fakeOS.Removes = []string{}
	err = m.containerGC.evictPodLogsDirectories(ctx, false)
	assert.NoError(t, err)
	assert.Empty(t, fakeOS.Removes)
}

func TestUnknownStateContainerGC(t *testing.T) {
	ctx := context.Background()
	fakeRuntime, _, m, err := createTestRuntimeManager()
	assert.NoError(t, err)

	fakeContainers := makeFakeContainers(t, m, []containerTemplate{
		makeGCContainer("foo", "bar", 1, 1, runtimeapi.ContainerState_CONTAINER_UNKNOWN),
		makeGCContainer("foo", "bar", 0, 0, runtimeapi.ContainerState_CONTAINER_UNKNOWN),
	})
	fakeRuntime.SetFakeContainers(fakeContainers)

	err = m.containerGC.evictContainers(ctx, true, false)
	assert.NoError(t, err)

	assert.Contains(t, fakeRuntime.GetCalls(), "StopContainer", "RemoveContainer",
		"container in unknown state should be stopped before being removed")

	remain, err := fakeRuntime.ListContainers(ctx, nil)
	assert.NoError(t, err)
	assert.Len(t, remain, 1)
}
