/*
Copyright 2015 The Kubernetes Authors.

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

package testing

import (
	"context"
	"io"
	"net/url"
	"reflect"
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/flowcontrol"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	"k8s.io/kubernetes/pkg/volume"
)

type TB interface {
	Errorf(format string, args ...any)
}

type FakePod struct {
	Pod       *kubecontainer.Pod
	NetnsPath string
}

// FakeRuntime is a fake container runtime for testing.
type FakeRuntime struct {
	sync.Mutex
	CalledFunctions   []string
	PodList           []*FakePod
	AllPodList        []*FakePod
	ImageList         []kubecontainer.Image
	ImageFsStats      []*runtimeapi.FilesystemUsage
	ContainerFsStats  []*runtimeapi.FilesystemUsage
	APIPodStatus      v1.PodStatus
	PodStatus         kubecontainer.PodStatus
	StartedPods       []string
	KilledPods        []string
	StartedContainers []string
	KilledContainers  []string
	RuntimeStatus     *kubecontainer.RuntimeStatus
	VersionInfo       string
	APIVersionInfo    string
	RuntimeType       string
	Err               error
	InspectErr        error
	StatusErr         error
	// If BlockImagePulls is true, then all PullImage() calls will be blocked until
	// UnblockImagePulls() is called. This is used to simulate image pull latency
	// from container runtime.
	BlockImagePulls      bool
	imagePullTokenBucket chan bool
	T                    TB
}

const FakeHost = "localhost:12345"

type FakeStreamingRuntime struct {
	*FakeRuntime
}

var _ kubecontainer.StreamingRuntime = &FakeStreamingRuntime{}

// FakeRuntime should implement Runtime.
var _ kubecontainer.Runtime = &FakeRuntime{}

type FakeVersion struct {
	Version string
}

func (fv *FakeVersion) String() string {
	return fv.Version
}

func (fv *FakeVersion) Compare(other string) (int, error) {
	result := 0
	if fv.Version > other {
		result = 1
	} else if fv.Version < other {
		result = -1
	}
	return result, nil
}

type podsGetter interface {
	GetPods(context.Context, bool) ([]*kubecontainer.Pod, error)
}

type FakeRuntimeCache struct {
	getter podsGetter
}

func NewFakeRuntimeCache(getter podsGetter) kubecontainer.RuntimeCache {
	return &FakeRuntimeCache{getter}
}

func (f *FakeRuntimeCache) GetPods(ctx context.Context) ([]*kubecontainer.Pod, error) {
	return f.getter.GetPods(ctx, false)
}

func (f *FakeRuntimeCache) ForceUpdateIfOlder(context.Context, time.Time) error {
	return nil
}

// UpdatePodCIDR fulfills the cri interface.
func (f *FakeRuntime) UpdatePodCIDR(_ context.Context, c string) error {
	return nil
}

func (f *FakeRuntime) assertList(expect []string, test []string) bool {
	if !reflect.DeepEqual(expect, test) {
		f.T.Errorf("AssertList: expected %#v, got %#v", expect, test)
		return false
	}
	return true
}

// AssertCalls test if the invoked functions are as expected.
func (f *FakeRuntime) AssertCalls(calls []string) bool {
	f.Lock()
	defer f.Unlock()
	return f.assertList(calls, f.CalledFunctions)
}

// AssertCallCounts checks if a certain call is called for a certain of numbers
func (f *FakeRuntime) AssertCallCounts(funcName string, expectedCount int) bool {
	f.Lock()
	defer f.Unlock()
	actualCount := 0
	for _, c := range f.CalledFunctions {
		if funcName == c {
			actualCount += 1
		}
	}
	if expectedCount != actualCount {
		f.T.Errorf("AssertCallCounts: expected %s to be called %d times, but was actually called %d times.", funcName, expectedCount, actualCount)
		return false
	}
	return true
}

func (f *FakeRuntime) AssertStartedPods(pods []string) bool {
	f.Lock()
	defer f.Unlock()
	return f.assertList(pods, f.StartedPods)
}

func (f *FakeRuntime) AssertKilledPods(pods []string) bool {
	f.Lock()
	defer f.Unlock()
	return f.assertList(pods, f.KilledPods)
}

func (f *FakeRuntime) AssertStartedContainers(containers []string) bool {
	f.Lock()
	defer f.Unlock()
	return f.assertList(containers, f.StartedContainers)
}

func (f *FakeRuntime) AssertKilledContainers(containers []string) bool {
	f.Lock()
	defer f.Unlock()
	return f.assertList(containers, f.KilledContainers)
}

func (f *FakeRuntime) Type() string {
	return f.RuntimeType
}

func (f *FakeRuntime) Version(_ context.Context) (kubecontainer.Version, error) {
	f.Lock()
	defer f.Unlock()

	f.CalledFunctions = append(f.CalledFunctions, "Version")
	return &FakeVersion{Version: f.VersionInfo}, f.Err
}

func (f *FakeRuntime) APIVersion() (kubecontainer.Version, error) {
	f.Lock()
	defer f.Unlock()

	f.CalledFunctions = append(f.CalledFunctions, "APIVersion")
	return &FakeVersion{Version: f.APIVersionInfo}, f.Err
}

func (f *FakeRuntime) Status(_ context.Context) (*kubecontainer.RuntimeStatus, error) {
	f.Lock()
	defer f.Unlock()

	f.CalledFunctions = append(f.CalledFunctions, "Status")
	return f.RuntimeStatus, f.StatusErr
}

func (f *FakeRuntime) GetPods(_ context.Context, all bool) ([]*kubecontainer.Pod, error) {
	f.Lock()
	defer f.Unlock()

	var pods []*kubecontainer.Pod

	f.CalledFunctions = append(f.CalledFunctions, "GetPods")
	if all {
		for _, fakePod := range f.AllPodList {
			pods = append(pods, fakePod.Pod)
		}
	} else {
		for _, fakePod := range f.PodList {
			pods = append(pods, fakePod.Pod)
		}
	}
	return pods, f.Err
}

func (f *FakeRuntime) SyncPod(_ context.Context, pod *v1.Pod, _ *kubecontainer.PodStatus, _ []v1.Secret, backOff *flowcontrol.Backoff) (result kubecontainer.PodSyncResult) {
	f.Lock()
	defer f.Unlock()

	f.CalledFunctions = append(f.CalledFunctions, "SyncPod")
	f.StartedPods = append(f.StartedPods, string(pod.UID))
	for _, c := range pod.Spec.Containers {
		f.StartedContainers = append(f.StartedContainers, c.Name)
	}
	// TODO(random-liu): Add SyncResult for starting and killing containers
	if f.Err != nil {
		result.Fail(f.Err)
	}
	return
}

func (f *FakeRuntime) KillPod(_ context.Context, pod *v1.Pod, runningPod kubecontainer.Pod, gracePeriodOverride *int64) error {
	f.Lock()
	defer f.Unlock()

	f.CalledFunctions = append(f.CalledFunctions, "KillPod")
	f.KilledPods = append(f.KilledPods, string(runningPod.ID))
	for _, c := range runningPod.Containers {
		f.KilledContainers = append(f.KilledContainers, c.Name)
	}
	return f.Err
}

func (f *FakeRuntime) RunContainerInPod(container v1.Container, pod *v1.Pod, volumeMap map[string]volume.VolumePlugin) error {
	f.Lock()
	defer f.Unlock()

	f.CalledFunctions = append(f.CalledFunctions, "RunContainerInPod")
	f.StartedContainers = append(f.StartedContainers, container.Name)

	pod.Spec.Containers = append(pod.Spec.Containers, container)
	for _, c := range pod.Spec.Containers {
		if c.Name == container.Name { // Container already in the pod.
			return f.Err
		}
	}
	pod.Spec.Containers = append(pod.Spec.Containers, container)
	return f.Err
}

func (f *FakeRuntime) KillContainerInPod(container v1.Container, pod *v1.Pod) error {
	f.Lock()
	defer f.Unlock()

	f.CalledFunctions = append(f.CalledFunctions, "KillContainerInPod")
	f.KilledContainers = append(f.KilledContainers, container.Name)
	return f.Err
}

func (f *FakeRuntime) GeneratePodStatus(event *runtimeapi.ContainerEventResponse) (*kubecontainer.PodStatus, error) {
	f.Lock()
	defer f.Unlock()

	f.CalledFunctions = append(f.CalledFunctions, "GeneratePodStatus")
	status := f.PodStatus
	return &status, f.Err
}

func (f *FakeRuntime) GetPodStatus(_ context.Context, uid types.UID, name, namespace string) (*kubecontainer.PodStatus, error) {
	f.Lock()
	defer f.Unlock()

	f.CalledFunctions = append(f.CalledFunctions, "GetPodStatus")
	status := f.PodStatus
	return &status, f.Err
}

func (f *FakeRuntime) GetContainerLogs(_ context.Context, pod *v1.Pod, containerID kubecontainer.ContainerID, logOptions *v1.PodLogOptions, stdout, stderr io.Writer) (err error) {
	f.Lock()
	defer f.Unlock()

	f.CalledFunctions = append(f.CalledFunctions, "GetContainerLogs")
	return f.Err
}

func (f *FakeRuntime) PullImage(ctx context.Context, image kubecontainer.ImageSpec, pullSecrets []v1.Secret, podSandboxConfig *runtimeapi.PodSandboxConfig, serviceAccountName string) (string, error) {
	f.Lock()
	f.CalledFunctions = append(f.CalledFunctions, "PullImage")
	if f.Err == nil {
		i := kubecontainer.Image{
			ID:   image.Image,
			Spec: image,
		}
		f.ImageList = append(f.ImageList, i)
	}

	if !f.BlockImagePulls {
		f.Unlock()
		return image.Image, f.Err
	}

	retErr := f.Err
	if f.imagePullTokenBucket == nil {
		f.imagePullTokenBucket = make(chan bool, 1)
	}
	// Unlock before waiting for UnblockImagePulls calls, to avoid deadlock.
	f.Unlock()
	select {
	case <-ctx.Done():
	case <-f.imagePullTokenBucket:
	}
	return image.Image, retErr
}

// UnblockImagePulls unblocks a certain number of image pulls, if BlockImagePulls is true.
func (f *FakeRuntime) UnblockImagePulls(count int) {
	if f.imagePullTokenBucket != nil {
		for i := 0; i < count; i++ {
			select {
			case f.imagePullTokenBucket <- true:
			default:
			}
		}
	}
}

func (f *FakeRuntime) GetImageRef(_ context.Context, image kubecontainer.ImageSpec) (string, error) {
	f.Lock()
	defer f.Unlock()

	f.CalledFunctions = append(f.CalledFunctions, "GetImageRef")
	for _, i := range f.ImageList {
		if i.ID == image.Image {
			return i.ID, nil
		}
	}
	return "", f.InspectErr
}

func (f *FakeRuntime) GetImageSize(_ context.Context, image kubecontainer.ImageSpec) (uint64, error) {
	f.Lock()
	defer f.Unlock()

	f.CalledFunctions = append(f.CalledFunctions, "GetImageSize")
	return 0, f.Err
}

func (f *FakeRuntime) ListImages(_ context.Context) ([]kubecontainer.Image, error) {
	f.Lock()
	defer f.Unlock()

	f.CalledFunctions = append(f.CalledFunctions, "ListImages")
	return snapshot(f.ImageList), f.Err
}

func snapshot(imageList []kubecontainer.Image) []kubecontainer.Image {
	result := make([]kubecontainer.Image, len(imageList))
	copy(result, imageList)
	return result
}

func (f *FakeRuntime) RemoveImage(_ context.Context, image kubecontainer.ImageSpec) error {
	f.Lock()
	defer f.Unlock()

	f.CalledFunctions = append(f.CalledFunctions, "RemoveImage")
	index := 0
	for i := range f.ImageList {
		if f.ImageList[i].ID == image.Image {
			index = i
			break
		}
	}
	f.ImageList = append(f.ImageList[:index], f.ImageList[index+1:]...)

	return f.Err
}

func (f *FakeRuntime) GarbageCollect(_ context.Context, gcPolicy kubecontainer.GCPolicy, ready bool, evictNonDeletedPods bool) error {
	f.Lock()
	defer f.Unlock()

	f.CalledFunctions = append(f.CalledFunctions, "GarbageCollect")
	return f.Err
}

func (f *FakeRuntime) DeleteContainer(_ context.Context, containerID kubecontainer.ContainerID) error {
	f.Lock()
	defer f.Unlock()

	f.CalledFunctions = append(f.CalledFunctions, "DeleteContainer")
	return f.Err
}

func (f *FakeRuntime) CheckpointContainer(_ context.Context, options *runtimeapi.CheckpointContainerRequest) error {
	f.Lock()
	defer f.Unlock()

	f.CalledFunctions = append(f.CalledFunctions, "CheckpointContainer")
	return f.Err
}

func (f *FakeRuntime) ListMetricDescriptors(_ context.Context) ([]*runtimeapi.MetricDescriptor, error) {
	f.Lock()
	defer f.Unlock()

	f.CalledFunctions = append(f.CalledFunctions, "ListMetricDescriptors")
	return nil, f.Err
}

func (f *FakeRuntime) ListPodSandboxMetrics(_ context.Context) ([]*runtimeapi.PodSandboxMetrics, error) {
	f.Lock()
	defer f.Unlock()

	f.CalledFunctions = append(f.CalledFunctions, "ListPodSandboxMetrics")
	return nil, f.Err
}

// SetContainerFsStats sets the containerFsStats for dependency injection.
func (f *FakeRuntime) SetContainerFsStats(val []*runtimeapi.FilesystemUsage) {
	f.ContainerFsStats = val
}

// SetImageFsStats sets the ImageFsStats for dependency injection.
func (f *FakeRuntime) SetImageFsStats(val []*runtimeapi.FilesystemUsage) {
	f.ImageFsStats = val
}

func (f *FakeRuntime) ImageStats(_ context.Context) (*kubecontainer.ImageStats, error) {
	f.Lock()
	defer f.Unlock()

	f.CalledFunctions = append(f.CalledFunctions, "ImageStats")
	return nil, f.Err
}

// ImageFsInfo returns a ImageFsInfoResponse given the DI injected values of ImageFsStats
// and ContainerFsStats.
func (f *FakeRuntime) ImageFsInfo(_ context.Context) (*runtimeapi.ImageFsInfoResponse, error) {
	f.Lock()
	defer f.Unlock()

	f.CalledFunctions = append(f.CalledFunctions, "ImageFsInfo")
	resp := &runtimeapi.ImageFsInfoResponse{
		ImageFilesystems:     f.ImageFsStats,
		ContainerFilesystems: f.ContainerFsStats,
	}
	return resp, f.Err
}

func (f *FakeStreamingRuntime) GetExec(_ context.Context, id kubecontainer.ContainerID, cmd []string, stdin, stdout, stderr, tty bool) (*url.URL, error) {
	f.Lock()
	defer f.Unlock()

	f.CalledFunctions = append(f.CalledFunctions, "GetExec")
	return &url.URL{Host: FakeHost}, f.Err
}

func (f *FakeStreamingRuntime) GetAttach(_ context.Context, id kubecontainer.ContainerID, stdin, stdout, stderr, tty bool) (*url.URL, error) {
	f.Lock()
	defer f.Unlock()

	f.CalledFunctions = append(f.CalledFunctions, "GetAttach")
	return &url.URL{Host: FakeHost}, f.Err
}

func (f *FakeStreamingRuntime) GetPortForward(_ context.Context, podName, podNamespace string, podUID types.UID, ports []int32) (*url.URL, error) {
	f.Lock()
	defer f.Unlock()

	f.CalledFunctions = append(f.CalledFunctions, "GetPortForward")
	return &url.URL{Host: FakeHost}, f.Err
}

type FakeContainerCommandRunner struct {
	// what to return
	Stdout string
	Err    error

	// actual values when invoked
	ContainerID kubecontainer.ContainerID
	Cmd         []string
}

var _ kubecontainer.CommandRunner = &FakeContainerCommandRunner{}

func (f *FakeContainerCommandRunner) RunInContainer(_ context.Context, containerID kubecontainer.ContainerID, cmd []string, timeout time.Duration) ([]byte, error) {
	// record invoked values
	f.ContainerID = containerID
	f.Cmd = cmd

	return []byte(f.Stdout), f.Err
}

func (f *FakeRuntime) GetContainerStatus(_ context.Context, _ kubecontainer.ContainerID) (status *kubecontainer.Status, err error) {
	f.Lock()
	defer f.Unlock()

	f.CalledFunctions = append(f.CalledFunctions, "GetContainerStatus")
	return nil, f.Err
}
