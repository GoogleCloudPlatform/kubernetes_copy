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
	"os"
	"path/filepath"
	"regexp"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	featuregatetesting "k8s.io/component-base/featuregate/testing"

	v1 "k8s.io/api/core/v1"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"

	"k8s.io/kubernetes/pkg/features"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	containertest "k8s.io/kubernetes/pkg/kubelet/container/testing"
	"k8s.io/kubernetes/pkg/kubelet/lifecycle"
)

// TestRemoveContainer tests removing the container and its corresponding container logs.
func TestRemoveContainer(t *testing.T) {
	ctx := context.Background()
	fakeRuntime, _, m, err := createTestRuntimeManager()
	require.NoError(t, err)
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			UID:       "12345678",
			Name:      "bar",
			Namespace: "new",
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name:            "foo",
					Image:           "busybox",
					ImagePullPolicy: v1.PullIfNotPresent,
				},
			},
		},
	}

	// Create fake sandbox and container
	_, fakeContainers := makeAndSetFakePod(t, m, fakeRuntime, pod)
	assert.Len(t, fakeContainers, 1)

	containerID := fakeContainers[0].Id
	fakeOS := m.osInterface.(*containertest.FakeOS)
	fakeOS.GlobFn = func(pattern, path string) bool {
		pattern = strings.Replace(pattern, "*", ".*", -1)
		pattern = strings.Replace(pattern, "\\", "\\\\", -1)
		return regexp.MustCompile(pattern).MatchString(path)
	}
	podLogsDirectory := "/var/log/pods"
	expectedContainerLogPath := filepath.Join(podLogsDirectory, "new_bar_12345678", "foo", "0.log")
	expectedContainerLogPathRotated := filepath.Join(podLogsDirectory, "new_bar_12345678", "foo", "0.log.20060102-150405")
	expectedContainerLogSymlink := legacyLogSymlink(containerID, "foo", "bar", "new")

	fakeOS.Create(expectedContainerLogPath)
	fakeOS.Create(expectedContainerLogPathRotated)

	err = m.removeContainer(ctx, containerID)
	assert.NoError(t, err)

	// Verify container log is removed.
	// We could not predict the order of `fakeOS.Removes`, so we use `assert.ElementsMatch` here.
	assert.ElementsMatch(t,
		[]string{expectedContainerLogSymlink, expectedContainerLogPath, expectedContainerLogPathRotated},
		fakeOS.Removes)
	// Verify container is removed
	assert.Contains(t, fakeRuntime.Called, "RemoveContainer")
	containers, err := fakeRuntime.ListContainers(ctx, &runtimeapi.ContainerFilter{Id: containerID})
	assert.NoError(t, err)
	assert.Empty(t, containers)
}

// TestKillContainer tests killing the container in a Pod.
func TestKillContainer(t *testing.T) {
	_, _, m, _ := createTestRuntimeManager()

	tests := []struct {
		caseName            string
		pod                 *v1.Pod
		containerID         kubecontainer.ContainerID
		containerName       string
		reason              string
		gracePeriodOverride int64
		succeed             bool
	}{
		{
			caseName: "Failed to find container in pods, expect to return error",
			pod: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{UID: "pod1_id", Name: "pod1", Namespace: "default"},
				Spec:       v1.PodSpec{Containers: []v1.Container{{Name: "empty_container"}}},
			},
			containerID:         kubecontainer.ContainerID{Type: "docker", ID: "not_exist_container_id"},
			containerName:       "not_exist_container",
			reason:              "unknown reason",
			gracePeriodOverride: 0,
			succeed:             false,
		},
	}

	for _, test := range tests {
		ctx := context.Background()
		err := m.killContainer(ctx, test.pod, test.containerID, test.containerName, test.reason, "", &test.gracePeriodOverride, nil)
		if test.succeed != (err == nil) {
			t.Errorf("%s: expected %v, got %v (%v)", test.caseName, test.succeed, (err == nil), err)
		}
	}
}

// TestToKubeContainerStatus tests the converting the CRI container status to
// the internal type (i.e., toKubeContainerStatus()) for containers in
// different states.
func TestToKubeContainerStatus(t *testing.T) {
	cid := &kubecontainer.ContainerID{Type: "testRuntime", ID: "dummyid"}
	meta := &runtimeapi.ContainerMetadata{Name: "cname", Attempt: 3}
	imageSpec := &runtimeapi.ImageSpec{Image: "fimage"}
	var (
		createdAt  int64 = 327
		startedAt  int64 = 999
		finishedAt int64 = 1278
	)

	for desc, test := range map[string]struct {
		input    *runtimeapi.ContainerStatus
		expected *kubecontainer.Status
	}{
		"created container": {
			input: &runtimeapi.ContainerStatus{
				Id:        cid.ID,
				Metadata:  meta,
				Image:     imageSpec,
				State:     runtimeapi.ContainerState_CONTAINER_CREATED,
				CreatedAt: createdAt,
			},
			expected: &kubecontainer.Status{
				ID:        *cid,
				Image:     imageSpec.Image,
				State:     kubecontainer.ContainerStateCreated,
				CreatedAt: time.Unix(0, createdAt),
			},
		},
		"running container": {
			input: &runtimeapi.ContainerStatus{
				Id:        cid.ID,
				Metadata:  meta,
				Image:     imageSpec,
				State:     runtimeapi.ContainerState_CONTAINER_RUNNING,
				CreatedAt: createdAt,
				StartedAt: startedAt,
			},
			expected: &kubecontainer.Status{
				ID:        *cid,
				Image:     imageSpec.Image,
				State:     kubecontainer.ContainerStateRunning,
				CreatedAt: time.Unix(0, createdAt),
				StartedAt: time.Unix(0, startedAt),
			},
		},
		"exited container": {
			input: &runtimeapi.ContainerStatus{
				Id:         cid.ID,
				Metadata:   meta,
				Image:      imageSpec,
				State:      runtimeapi.ContainerState_CONTAINER_EXITED,
				CreatedAt:  createdAt,
				StartedAt:  startedAt,
				FinishedAt: finishedAt,
				ExitCode:   int32(121),
				Reason:     "GotKilled",
				Message:    "The container was killed",
			},
			expected: &kubecontainer.Status{
				ID:         *cid,
				Image:      imageSpec.Image,
				State:      kubecontainer.ContainerStateExited,
				CreatedAt:  time.Unix(0, createdAt),
				StartedAt:  time.Unix(0, startedAt),
				FinishedAt: time.Unix(0, finishedAt),
				ExitCode:   121,
				Reason:     "GotKilled",
				Message:    "The container was killed",
			},
		},
		"unknown container": {
			input: &runtimeapi.ContainerStatus{
				Id:        cid.ID,
				Metadata:  meta,
				Image:     imageSpec,
				State:     runtimeapi.ContainerState_CONTAINER_UNKNOWN,
				CreatedAt: createdAt,
				StartedAt: startedAt,
			},
			expected: &kubecontainer.Status{
				ID:        *cid,
				Image:     imageSpec.Image,
				State:     kubecontainer.ContainerStateUnknown,
				CreatedAt: time.Unix(0, createdAt),
				StartedAt: time.Unix(0, startedAt),
			},
		},
	} {
		actual := toKubeContainerStatus(test.input, cid.Type)
		assert.Equal(t, test.expected, actual, desc)
	}
}

// TestToKubeContainerStatusWithResources tests the converting the CRI container status to
// the internal type (i.e., toKubeContainerStatus()) for containers that returns Resources.
func TestToKubeContainerStatusWithResources(t *testing.T) {
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.InPlacePodVerticalScaling, true)
	cid := &kubecontainer.ContainerID{Type: "testRuntime", ID: "dummyid"}
	meta := &runtimeapi.ContainerMetadata{Name: "cname", Attempt: 3}
	imageSpec := &runtimeapi.ImageSpec{Image: "fimage"}
	var (
		createdAt int64 = 327
		startedAt int64 = 999
	)

	for desc, test := range map[string]struct {
		input         *runtimeapi.ContainerStatus
		expected      *kubecontainer.Status
		skipOnWindows bool
	}{
		"container reporting cpu and memory": {
			input: &runtimeapi.ContainerStatus{
				Id:        cid.ID,
				Metadata:  meta,
				Image:     imageSpec,
				State:     runtimeapi.ContainerState_CONTAINER_RUNNING,
				CreatedAt: createdAt,
				StartedAt: startedAt,
				Resources: func() *runtimeapi.ContainerResources {
					if goruntime.GOOS == "windows" {
						return &runtimeapi.ContainerResources{
							Windows: &runtimeapi.WindowsContainerResources{
								CpuMaximum:         2500,
								CpuCount:           1,
								MemoryLimitInBytes: 524288000,
							},
						}
					}
					return &runtimeapi.ContainerResources{
						Linux: &runtimeapi.LinuxContainerResources{
							CpuQuota:           25000,
							CpuPeriod:          100000,
							MemoryLimitInBytes: 524288000,
							OomScoreAdj:        -998,
						},
					}
				}(),
			},
			expected: &kubecontainer.Status{
				ID:        *cid,
				Image:     imageSpec.Image,
				State:     kubecontainer.ContainerStateRunning,
				CreatedAt: time.Unix(0, createdAt),
				StartedAt: time.Unix(0, startedAt),
				Resources: &kubecontainer.ContainerResources{
					CPULimit:    resource.NewMilliQuantity(250, resource.DecimalSI),
					MemoryLimit: resource.NewQuantity(524288000, resource.BinarySI),
				},
			},
			skipOnWindows: true,
		},
		"container reporting cpu only": {
			input: &runtimeapi.ContainerStatus{
				Id:        cid.ID,
				Metadata:  meta,
				Image:     imageSpec,
				State:     runtimeapi.ContainerState_CONTAINER_RUNNING,
				CreatedAt: createdAt,
				StartedAt: startedAt,
				Resources: func() *runtimeapi.ContainerResources {
					if goruntime.GOOS == "windows" {
						return &runtimeapi.ContainerResources{
							Windows: &runtimeapi.WindowsContainerResources{
								CpuMaximum: 2500,
								CpuCount:   2,
							},
						}
					}
					return &runtimeapi.ContainerResources{
						Linux: &runtimeapi.LinuxContainerResources{
							CpuQuota:  50000,
							CpuPeriod: 100000,
						},
					}
				}(),
			},
			expected: &kubecontainer.Status{
				ID:        *cid,
				Image:     imageSpec.Image,
				State:     kubecontainer.ContainerStateRunning,
				CreatedAt: time.Unix(0, createdAt),
				StartedAt: time.Unix(0, startedAt),
				Resources: &kubecontainer.ContainerResources{
					CPULimit: resource.NewMilliQuantity(500, resource.DecimalSI),
				},
			},
		},
		"container reporting memory only": {
			input: &runtimeapi.ContainerStatus{
				Id:        cid.ID,
				Metadata:  meta,
				Image:     imageSpec,
				State:     runtimeapi.ContainerState_CONTAINER_RUNNING,
				CreatedAt: createdAt,
				StartedAt: startedAt,
				Resources: &runtimeapi.ContainerResources{
					Linux: &runtimeapi.LinuxContainerResources{
						MemoryLimitInBytes: 524288000,
						OomScoreAdj:        -998,
					},
					Windows: &runtimeapi.WindowsContainerResources{
						MemoryLimitInBytes: 524288000,
					},
				},
			},
			expected: &kubecontainer.Status{
				ID:        *cid,
				Image:     imageSpec.Image,
				State:     kubecontainer.ContainerStateRunning,
				CreatedAt: time.Unix(0, createdAt),
				StartedAt: time.Unix(0, startedAt),
				Resources: &kubecontainer.ContainerResources{
					MemoryLimit: resource.NewQuantity(524288000, resource.BinarySI),
				},
			},
		},
	} {
		t.Run(desc, func(t *testing.T) {
			if test.skipOnWindows && goruntime.GOOS == "windows" {
				// TODO: remove skip once the failing test has been fixed.
				t.Skip("Skip failing test on Windows.")
			}
			actual := toKubeContainerStatus(test.input, cid.Type)
			assert.Equal(t, test.expected, actual, desc)
		})
	}
}

func TestToKubeContainerStatusWithUser(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("Updating Pod Container User is not supported on Windows.")
	}

	cid := &kubecontainer.ContainerID{Type: "testRuntime", ID: "dummyid"}
	meta := &runtimeapi.ContainerMetadata{Name: "cname", Attempt: 3}
	imageSpec := &runtimeapi.ImageSpec{Image: "fimage"}
	var (
		createdAt int64 = 327
		startedAt int64 = 999
	)

	for desc, test := range map[string]struct {
		input          *runtimeapi.ContainerUser
		expected       *kubecontainer.ContainerUser
		featureEnabled bool
	}{
		"non nil user, SupplementalGroupsPolicy is disabled": {
			input: &runtimeapi.ContainerUser{
				Linux: &runtimeapi.LinuxContainerUser{
					Uid:                0,
					Gid:                0,
					SupplementalGroups: []int64{10},
				},
			},
			expected:       nil,
			featureEnabled: false,
		},
		"empty user, SupplementalGroupsPolicy is disabled": {
			input:          &runtimeapi.ContainerUser{},
			expected:       nil,
			featureEnabled: false,
		},
		"nil user, SupplementalGroupsPolicy is disabled": {
			input:          nil,
			expected:       nil,
			featureEnabled: false,
		},
		"non nil user, SupplementalGroupsPolicy is enabled": {
			input: &runtimeapi.ContainerUser{
				Linux: &runtimeapi.LinuxContainerUser{
					Uid:                0,
					Gid:                0,
					SupplementalGroups: []int64{10},
				},
			},
			expected: &kubecontainer.ContainerUser{
				Linux: &kubecontainer.LinuxContainerUser{
					UID:                0,
					GID:                0,
					SupplementalGroups: []int64{10},
				},
			},
			featureEnabled: true,
		},
		"empty user, SupplementalGroupsPolicy is enabled": {
			input:          &runtimeapi.ContainerUser{},
			expected:       &kubecontainer.ContainerUser{},
			featureEnabled: true,
		},
		"nil user, SupplementalGroupsPolicy is enabled": {
			input:          nil,
			expected:       nil,
			featureEnabled: true,
		},
	} {
		t.Run(desc, func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.SupplementalGroupsPolicy, test.featureEnabled)
			cStatus := &runtimeapi.ContainerStatus{
				Id:        cid.ID,
				Metadata:  meta,
				Image:     imageSpec,
				State:     runtimeapi.ContainerState_CONTAINER_RUNNING,
				CreatedAt: createdAt,
				StartedAt: startedAt,
				User:      test.input,
			}
			actual := toKubeContainerStatus(cStatus, cid.Type)
			assert.EqualValues(t, test.expected, actual.User, desc)
		})
	}
}

func testLifeCycleHook(t *testing.T, testPod *v1.Pod, testContainer *v1.Container) {

	// Setup
	fakeRuntime, _, m, _ := createTestRuntimeManager()

	gracePeriod := int64(30)
	cID := kubecontainer.ContainerID{
		Type: "docker",
		ID:   "foo",
	}

	cmdPostStart := &v1.Lifecycle{
		PostStart: &v1.LifecycleHandler{
			Exec: &v1.ExecAction{
				Command: []string{"PostStartCMD"},
			},
		},
	}

	httpLifeCycle := &v1.Lifecycle{
		PreStop: &v1.LifecycleHandler{
			HTTPGet: &v1.HTTPGetAction{
				Host: "testHost.com",
				Path: "/GracefulExit",
			},
		},
	}

	cmdLifeCycle := &v1.Lifecycle{
		PreStop: &v1.LifecycleHandler{
			Exec: &v1.ExecAction{
				Command: []string{"PreStopCMD"},
			},
		},
	}

	fakeRunner := &containertest.FakeContainerCommandRunner{}
	fakeHTTP := &fakeHTTP{}
	fakePodStatusProvider := podStatusProviderFunc(func(uid types.UID, name, namespace string) (*kubecontainer.PodStatus, error) {
		return &kubecontainer.PodStatus{
			ID:        uid,
			Name:      name,
			Namespace: namespace,
			IPs: []string{
				"127.0.0.1",
			},
		}, nil
	})

	lcHanlder := lifecycle.NewHandlerRunner(
		fakeHTTP,
		fakeRunner,
		fakePodStatusProvider,
		nil)

	m.runner = lcHanlder

	// Configured and works as expected
	t.Run("PreStop-CMDExec", func(t *testing.T) {
		ctx := context.Background()
		testContainer.Lifecycle = cmdLifeCycle
		_ = m.killContainer(ctx, testPod, cID, "foo", "testKill", "", &gracePeriod, nil)
		if fakeRunner.Cmd[0] != cmdLifeCycle.PreStop.Exec.Command[0] {
			t.Errorf("CMD Prestop hook was not invoked")
		}
	})

	// Configured and working HTTP hook
	t.Run("PreStop-HTTPGet", func(t *testing.T) {
		t.Run("consistent", func(t *testing.T) {
			ctx := context.Background()
			defer func() { fakeHTTP.req = nil }()
			httpLifeCycle.PreStop.HTTPGet.Port = intstr.FromInt32(80)
			testContainer.Lifecycle = httpLifeCycle
			_ = m.killContainer(ctx, testPod, cID, "foo", "testKill", "", &gracePeriod, nil)
			if fakeHTTP.req == nil || !strings.Contains(fakeHTTP.req.URL.String(), httpLifeCycle.PreStop.HTTPGet.Host) {
				t.Errorf("HTTP Prestop hook was not invoked")
			}
		})
	})

	// When there is no time to run PreStopHook
	t.Run("PreStop-NoTimeToRun", func(t *testing.T) {
		ctx := context.Background()
		gracePeriodLocal := int64(0)

		testPod.DeletionGracePeriodSeconds = &gracePeriodLocal
		testPod.Spec.TerminationGracePeriodSeconds = &gracePeriodLocal

		_ = m.killContainer(ctx, testPod, cID, "foo", "testKill", "", &gracePeriodLocal, nil)
		if fakeHTTP.req != nil {
			t.Errorf("HTTP Prestop hook Should not execute when gracePeriod is 0")
		}
	})

	// Post Start script
	t.Run("PostStart-CmdExe", func(t *testing.T) {
		ctx := context.Background()
		// Fake all the things you need before trying to create a container
		fakeSandBox, _ := makeAndSetFakePod(t, m, fakeRuntime, testPod)
		fakeSandBoxConfig, _ := m.generatePodSandboxConfig(testPod, 0)
		testContainer.Lifecycle = cmdPostStart
		fakePodStatus := &kubecontainer.PodStatus{
			ContainerStatuses: []*kubecontainer.Status{
				{
					ID: kubecontainer.ContainerID{
						Type: "docker",
						ID:   testContainer.Name,
					},
					Name:      testContainer.Name,
					State:     kubecontainer.ContainerStateCreated,
					CreatedAt: time.Unix(0, time.Now().Unix()),
				},
			},
		}

		// Now try to create a container, which should in turn invoke PostStart Hook
		_, err := m.startContainer(ctx, fakeSandBox.Id, fakeSandBoxConfig, containerStartSpec(testContainer), testPod, fakePodStatus, nil, "", []string{}, nil)
		if err != nil {
			t.Errorf("startContainer error =%v", err)
		}
		if fakeRunner.Cmd[0] != cmdPostStart.PostStart.Exec.Command[0] {
			t.Errorf("CMD PostStart hook was not invoked")
		}
	})
}

func TestLifeCycleHook(t *testing.T) {
	testPod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "bar",
			Namespace: "default",
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name:            "foo",
					Image:           "busybox",
					ImagePullPolicy: v1.PullIfNotPresent,
					Command:         []string{"testCommand"},
					WorkingDir:      "testWorkingDir",
				},
			},
		},
	}

	testLifeCycleHook(t, testPod, &testPod.Spec.Containers[0])
}

func TestLifeCycleHookForRestartableInitContainer(t *testing.T) {
	testPod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "bar",
			Namespace: "default",
		},
		Spec: v1.PodSpec{
			InitContainers: []v1.Container{
				{
					Name:            "foo",
					Image:           "busybox",
					ImagePullPolicy: v1.PullIfNotPresent,
					Command:         []string{"testCommand"},
					WorkingDir:      "testWorkingDir",
					RestartPolicy:   &containerRestartPolicyAlways,
				},
			},
		},
	}

	testLifeCycleHook(t, testPod, &testPod.Spec.InitContainers[0])
}

func TestStartSpec(t *testing.T) {
	podStatus := &kubecontainer.PodStatus{
		ContainerStatuses: []*kubecontainer.Status{
			{
				ID: kubecontainer.ContainerID{
					Type: "docker",
					ID:   "docker-something-something",
				},
				Name: "target",
			},
		},
	}

	for _, tc := range []struct {
		name string
		spec *startSpec
		want *kubecontainer.ContainerID
	}{
		{
			"Regular Container",
			containerStartSpec(&v1.Container{
				Name: "test",
			}),
			nil,
		},
		{
			"Ephemeral Container w/o Target",
			ephemeralContainerStartSpec(&v1.EphemeralContainer{
				EphemeralContainerCommon: v1.EphemeralContainerCommon{
					Name: "test",
				},
			}),
			nil,
		},
		{
			"Ephemeral Container w/ Target",
			ephemeralContainerStartSpec(&v1.EphemeralContainer{
				EphemeralContainerCommon: v1.EphemeralContainerCommon{
					Name: "test",
				},
				TargetContainerName: "target",
			}),
			&kubecontainer.ContainerID{
				Type: "docker",
				ID:   "docker-something-something",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, err := tc.spec.getTargetID(podStatus); err != nil {
				t.Fatalf("%v: getTargetID got unexpected error: %v", t.Name(), err)
			} else if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("%v: getTargetID got unexpected result. diff:\n%v", t.Name(), diff)
			}
		})
	}
}

func TestRestartCountByLogDir(t *testing.T) {
	for _, tc := range []struct {
		filenames    []string
		restartCount int
	}{
		{
			filenames:    []string{"0.log.rotated-log"},
			restartCount: 1,
		},
		{
			filenames:    []string{"0.log"},
			restartCount: 1,
		},
		{
			filenames:    []string{"0.log", "1.log", "2.log"},
			restartCount: 3,
		},
		{
			filenames:    []string{"0.log.rotated", "1.log", "2.log"},
			restartCount: 3,
		},
		{
			filenames:    []string{"5.log.rotated", "6.log.rotated"},
			restartCount: 7,
		},
		{
			filenames:    []string{"5.log.rotated", "6.log", "7.log"},
			restartCount: 8,
		},
		// no restart count log files
		{
			filenames:    []string{},
			restartCount: 0,
		},
		{
			filenames:    []string{"a.log.rotated", "b.log.rotated", "12log.rotated"},
			restartCount: 0,
		},
		// log extension twice
		{
			filenames:    []string{"145.log.log.rotated"},
			restartCount: 146,
		},
		// too big of the integer
		{
			filenames:    []string{"92233720368547758089223372036854775808.log.rotated"},
			restartCount: 0,
		},
		// mix of log files
		{
			filenames:    []string{"9223372036854775808.log.rotated", "23.log", "23a.log", "1aaa.log.rotated", "2.log", "3.log.rotated"},
			restartCount: 24,
		},
		// prefixed
		{
			filenames:    []string{"rotated.23.log"},
			restartCount: 0,
		},
		{
			filenames:    []string{"mylog42.log"},
			restartCount: 0,
		},
		{
			filenames:    []string{"-42.log"},
			restartCount: 0,
		},
		// same restart count multiple times
		{
			filenames:    []string{"6.log", "6.log.rotated", "6.log.rotated.rotated"},
			restartCount: 7,
		},
	} {
		tempDirPath, err := os.MkdirTemp("", "test-restart-count-")
		assert.NoError(t, err, "create tempdir error")
		defer os.RemoveAll(tempDirPath)
		for _, filename := range tc.filenames {
			err = os.WriteFile(filepath.Join(tempDirPath, filename), []byte("a log line"), 0600)
			assert.NoError(t, err, "could not write log file")
		}
		count, err := calcRestartCountByLogDir(tempDirPath)
		if assert.NoError(t, err) {
			assert.Equal(t, count, tc.restartCount, "count %v should equal restartCount %v", count, tc.restartCount)
		}
	}
}

func TestKillContainerGracePeriod(t *testing.T) {

	shortGracePeriod := int64(10)
	mediumGracePeriod := int64(30)
	longGracePeriod := int64(60)

	tests := []struct {
		name                string
		pod                 *v1.Pod
		reason              containerKillReason
		expectedGracePeriod int64
	}{
		{
			name: "default termination grace period",
			pod: &v1.Pod{
				Spec: v1.PodSpec{Containers: []v1.Container{{Name: "foo"}}},
			},
			reason:              reasonUnknown,
			expectedGracePeriod: int64(2),
		},
		{
			name: "use pod termination grace period",
			pod: &v1.Pod{
				Spec: v1.PodSpec{
					Containers:                    []v1.Container{{Name: "foo"}},
					TerminationGracePeriodSeconds: &longGracePeriod,
				},
			},
			reason:              reasonUnknown,
			expectedGracePeriod: longGracePeriod,
		},
		{
			name: "liveness probe overrides pod termination grace period",
			pod: &v1.Pod{
				Spec: v1.PodSpec{
					Containers: []v1.Container{{
						Name: "foo", LivenessProbe: &v1.Probe{TerminationGracePeriodSeconds: &shortGracePeriod},
					}},
					TerminationGracePeriodSeconds: &longGracePeriod,
				},
			},
			reason:              reasonLivenessProbe,
			expectedGracePeriod: shortGracePeriod,
		},
		{
			name: "startup probe overrides pod termination grace period",
			pod: &v1.Pod{
				Spec: v1.PodSpec{
					Containers: []v1.Container{{
						Name: "foo", StartupProbe: &v1.Probe{TerminationGracePeriodSeconds: &shortGracePeriod},
					}},
					TerminationGracePeriodSeconds: &longGracePeriod,
				},
			},
			reason:              reasonStartupProbe,
			expectedGracePeriod: shortGracePeriod,
		},
		{
			name: "startup probe overrides pod termination grace period, probe period > pod period",
			pod: &v1.Pod{
				Spec: v1.PodSpec{
					Containers: []v1.Container{{
						Name: "foo", StartupProbe: &v1.Probe{TerminationGracePeriodSeconds: &longGracePeriod},
					}},
					TerminationGracePeriodSeconds: &shortGracePeriod,
				},
			},
			reason:              reasonStartupProbe,
			expectedGracePeriod: longGracePeriod,
		},
		{
			name: "liveness probe overrides pod termination grace period, probe period > pod period",
			pod: &v1.Pod{
				Spec: v1.PodSpec{
					Containers: []v1.Container{{
						Name: "foo", LivenessProbe: &v1.Probe{TerminationGracePeriodSeconds: &longGracePeriod},
					}},
					TerminationGracePeriodSeconds: &shortGracePeriod,
				},
			},
			reason:              reasonLivenessProbe,
			expectedGracePeriod: longGracePeriod,
		},
		{
			name: "non-liveness probe failure, use pod termination grace period",
			pod: &v1.Pod{
				Spec: v1.PodSpec{
					Containers: []v1.Container{{
						Name: "foo", LivenessProbe: &v1.Probe{TerminationGracePeriodSeconds: &shortGracePeriod},
					}},
					TerminationGracePeriodSeconds: &longGracePeriod,
				},
			},
			reason:              reasonUnknown,
			expectedGracePeriod: longGracePeriod,
		},
		{
			name: "non-startup probe failure, use pod termination grace period",
			pod: &v1.Pod{
				Spec: v1.PodSpec{
					Containers: []v1.Container{{
						Name: "foo", StartupProbe: &v1.Probe{TerminationGracePeriodSeconds: &shortGracePeriod},
					}},
					TerminationGracePeriodSeconds: &longGracePeriod,
				},
			},
			reason:              reasonUnknown,
			expectedGracePeriod: longGracePeriod,
		},
		{
			name: "all three grace periods set, use pod termination grace period",
			pod: &v1.Pod{
				Spec: v1.PodSpec{
					Containers: []v1.Container{{
						Name:          "foo",
						StartupProbe:  &v1.Probe{TerminationGracePeriodSeconds: &shortGracePeriod},
						LivenessProbe: &v1.Probe{TerminationGracePeriodSeconds: &mediumGracePeriod},
					}},
					TerminationGracePeriodSeconds: &longGracePeriod,
				},
			},
			reason:              reasonUnknown,
			expectedGracePeriod: longGracePeriod,
		},
		{
			name: "all three grace periods set, use startup termination grace period",
			pod: &v1.Pod{
				Spec: v1.PodSpec{
					Containers: []v1.Container{{
						Name:          "foo",
						StartupProbe:  &v1.Probe{TerminationGracePeriodSeconds: &shortGracePeriod},
						LivenessProbe: &v1.Probe{TerminationGracePeriodSeconds: &mediumGracePeriod},
					}},
					TerminationGracePeriodSeconds: &longGracePeriod,
				},
			},
			reason:              reasonStartupProbe,
			expectedGracePeriod: shortGracePeriod,
		},
		{
			name: "all three grace periods set, use liveness termination grace period",
			pod: &v1.Pod{
				Spec: v1.PodSpec{
					Containers: []v1.Container{{
						Name:          "foo",
						StartupProbe:  &v1.Probe{TerminationGracePeriodSeconds: &shortGracePeriod},
						LivenessProbe: &v1.Probe{TerminationGracePeriodSeconds: &mediumGracePeriod},
					}},
					TerminationGracePeriodSeconds: &longGracePeriod,
				},
			},
			reason:              reasonLivenessProbe,
			expectedGracePeriod: mediumGracePeriod,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			actualGracePeriod := setTerminationGracePeriod(test.pod, &test.pod.Spec.Containers[0], "", kubecontainer.ContainerID{}, test.reason)
			require.Equal(t, test.expectedGracePeriod, actualGracePeriod)
		})
	}
}

// TestUpdateContainerResources tests updating a container in a Pod.
func TestUpdateContainerResources(t *testing.T) {
	fakeRuntime, _, m, errCreate := createTestRuntimeManager()
	require.NoError(t, errCreate)
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			UID:       "12345678",
			Name:      "bar",
			Namespace: "new",
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name:            "foo",
					Image:           "busybox",
					ImagePullPolicy: v1.PullIfNotPresent,
				},
			},
		},
	}

	// Create fake sandbox and container
	_, fakeContainers := makeAndSetFakePod(t, m, fakeRuntime, pod)
	assert.Len(t, fakeContainers, 1)

	ctx := context.Background()
	cStatus, err := m.getPodContainerStatuses(ctx, pod.UID, pod.Name, pod.Namespace)
	assert.NoError(t, err)
	containerID := cStatus[0].ID

	err = m.updateContainerResources(pod, &pod.Spec.Containers[0], containerID)
	assert.NoError(t, err)

	// Verify container is updated
	assert.Contains(t, fakeRuntime.Called, "UpdateContainerResources")
}

// TestUpdatePodSandboxResources tests updating a podSandBox resources.
func TestUpdatePodSandboxResources(t *testing.T) {
	fakeRuntime, _, m, errCreate := createTestRuntimeManager()
	require.NoError(t, errCreate)
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			UID:       "12345678",
			Name:      "bar",
			Namespace: "new",
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name:            "foo",
					Image:           "busybox",
					ImagePullPolicy: v1.PullIfNotPresent,
				},
			},
		},
	}

	// Create fake sandbox and container
	fakeSandbox, fakeContainers := makeAndSetFakePod(t, m, fakeRuntime, pod)
	assert.Len(t, fakeContainers, 1)

	ctx := context.Background()
	_, err := m.getPodContainerStatuses(ctx, pod.UID, pod.Name, pod.Namespace)
	require.NoError(t, err)

	err = m.updatePodSandboxResources(fakeSandbox.Id, pod)
	require.NoError(t, err)

	// Verify sandbox is updated
	assert.Contains(t, fakeRuntime.Called, "UpdatePodSandboxResources")
}
