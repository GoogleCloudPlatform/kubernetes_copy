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

package images

import (
	"context"
	"fmt"
	goruntime "runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	noopoteltrace "go.opentelemetry.io/otel/trace/noop"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/tools/record"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	statsapi "k8s.io/kubelet/pkg/apis/stats/v1alpha1"
	"k8s.io/kubernetes/pkg/features"
	"k8s.io/kubernetes/pkg/kubelet/container"
	containertest "k8s.io/kubernetes/pkg/kubelet/container/testing"
	stats "k8s.io/kubernetes/pkg/kubelet/server/stats"
	statstest "k8s.io/kubernetes/pkg/kubelet/server/stats/testing"
	testingclock "k8s.io/utils/clock/testing"
)

var zero time.Time
var sandboxImage = "registry.k8s.io/pause-amd64:latest"

func newRealImageGCManager(policy ImageGCPolicy, mockStatsProvider stats.Provider) (*realImageGCManager, *containertest.FakeRuntime) {
	fakeRuntime := &containertest.FakeRuntime{}
	return &realImageGCManager{
		runtime:       fakeRuntime,
		policy:        policy,
		imageRecords:  make(map[string]*imageRecord),
		statsProvider: mockStatsProvider,
		recorder:      &record.FakeRecorder{},
		tracer:        noopoteltrace.NewTracerProvider().Tracer(""),
	}, fakeRuntime
}

// Accessors used for thread-safe testing.
func (im *realImageGCManager) imageRecordsLen() int {
	im.imageRecordsLock.Lock()
	defer im.imageRecordsLock.Unlock()
	return len(im.imageRecords)
}
func (im *realImageGCManager) getImageRecord(name string) (*imageRecord, bool) {
	im.imageRecordsLock.Lock()
	defer im.imageRecordsLock.Unlock()
	v, ok := im.imageRecords[name]
	vCopy := *v
	return &vCopy, ok
}

func (im *realImageGCManager) getImageRecordWithRuntimeHandlerInImageCriAPIFeatureGate(name, runtimeHandler string) (*imageRecord, bool) {
	im.imageRecordsLock.Lock()
	defer im.imageRecordsLock.Unlock()
	imageKey := getImageTuple(name, runtimeHandler)
	v, ok := im.imageRecords[imageKey]
	vCopy := *v
	return &vCopy, ok
}

// Returns the id of the image with the given ID.
func imageID(id int) string {
	return fmt.Sprintf("image-%d", id)
}

// Returns the name of the image with the given ID.
func imageName(id int) string {
	return imageID(id) + "-name"
}

// Make an image with the specified ID.
func makeImage(id int, size int64) container.Image {
	return container.Image{
		ID:   imageID(id),
		Size: size,
	}
}

// Make an image with the specified ID.
func makeImageWithRuntimeHandler(id int, size int64, runtimeHandler string) container.Image {
	if runtimeHandler == "" {
		return container.Image{
			ID:   imageID(id),
			Size: size,
		}
	} else {
		return container.Image{
			ID:   imageID(id),
			Size: size,
			Spec: container.ImageSpec{
				RuntimeHandler: runtimeHandler,
			},
		}
	}
}

// Make a container with the specified ID. It will use the image with the same ID.
func makeContainer(id int) *container.Container {
	return &container.Container{
		ID:      container.ContainerID{Type: "test", ID: fmt.Sprintf("container-%d", id)},
		Image:   imageName(id),
		ImageID: imageID(id),
	}
}

func TestDetectImagesInitialDetect(t *testing.T) {
	ctx := context.Background()
	mockStatsProvider := statstest.NewMockProvider(t)

	manager, fakeRuntime := newRealImageGCManager(ImageGCPolicy{}, mockStatsProvider)
	fakeRuntime.ImageList = []container.Image{
		makeImage(0, 1024),
		makeImage(1, 2048),
		makeImage(2, 2048),
	}
	fakeRuntime.AllPodList = []*containertest.FakePod{
		{Pod: &container.Pod{
			Containers: []*container.Container{
				{
					ID:      container.ContainerID{Type: "test", ID: fmt.Sprintf("container-%d", 1)},
					ImageID: imageID(1),
					// The image filed is not set to simulate a no-name image
				},
				{
					ID:      container.ContainerID{Type: "test", ID: fmt.Sprintf("container-%d", 2)},
					Image:   imageName(2),
					ImageID: imageID(2),
				},
			},
		}},
	}

	startTime := time.Now().Add(-time.Millisecond)
	_, err := manager.detectImages(ctx, zero)
	assert := assert.New(t)
	require.NoError(t, err)
	assert.Equal(3, manager.imageRecordsLen())
	noContainer, ok := manager.getImageRecord(imageID(0))
	require.True(t, ok)
	assert.Equal(zero, noContainer.firstDetected)
	assert.Equal(zero, noContainer.lastUsed)
	withContainerUsingNoNameImage, ok := manager.getImageRecord(imageID(1))
	require.True(t, ok)
	assert.Equal(zero, withContainerUsingNoNameImage.firstDetected)
	assert.True(withContainerUsingNoNameImage.lastUsed.After(startTime))
	withContainer, ok := manager.getImageRecord(imageID(2))
	require.True(t, ok)
	assert.Equal(zero, withContainer.firstDetected)
	assert.True(withContainer.lastUsed.After(startTime))
}

func TestDetectImagesInitialDetectWithRuntimeHandlerInImageCriAPIFeatureGate(t *testing.T) {
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.RuntimeClassInImageCriAPI, true)
	testRuntimeHandler := "test-runtimeHandler"
	ctx := context.Background()
	mockStatsProvider := statstest.NewMockProvider(t)

	manager, fakeRuntime := newRealImageGCManager(ImageGCPolicy{}, mockStatsProvider)
	fakeRuntime.ImageList = []container.Image{
		makeImageWithRuntimeHandler(0, 1024, testRuntimeHandler),
		makeImageWithRuntimeHandler(1, 2048, testRuntimeHandler),
		makeImageWithRuntimeHandler(2, 2048, ""),
	}
	fakeRuntime.AllPodList = []*containertest.FakePod{
		{Pod: &container.Pod{
			Containers: []*container.Container{
				{
					ID:      container.ContainerID{Type: "test", ID: fmt.Sprintf("container-%d", 1)},
					ImageID: imageID(1),
					// The image field is not set to simulate a no-name image
					ImageRuntimeHandler: testRuntimeHandler,
				},
				{
					ID:      container.ContainerID{Type: "test", ID: fmt.Sprintf("container-%d", 2)},
					Image:   imageName(2),
					ImageID: imageID(2),
					// The runtime handler field is not set to simulate the case when
					// the feature gate "RuntimeHandlerInImageCriApi" is on and container runtime has not implemented
					// KEP 4216, which means that runtimeHandler string is not set in the
					// responses from the container runtime.
				},
			},
		}},
	}

	startTime := time.Now().Add(-time.Millisecond)
	_, err := manager.detectImages(ctx, zero)
	assert := assert.New(t)
	require.NoError(t, err)
	assert.Equal(3, manager.imageRecordsLen())
	noContainer, ok := manager.getImageRecordWithRuntimeHandlerInImageCriAPIFeatureGate(imageID(0), testRuntimeHandler)
	require.True(t, ok)
	assert.Equal(zero, noContainer.firstDetected)
	assert.Equal(testRuntimeHandler, noContainer.runtimeHandlerUsedToPullImage)
	assert.Equal(zero, noContainer.lastUsed)
	withContainerUsingNoNameImage, ok := manager.getImageRecordWithRuntimeHandlerInImageCriAPIFeatureGate(imageID(1), testRuntimeHandler)
	require.True(t, ok)
	assert.Equal(zero, withContainerUsingNoNameImage.firstDetected)
	assert.True(withContainerUsingNoNameImage.lastUsed.After(startTime))
	assert.Equal(testRuntimeHandler, withContainerUsingNoNameImage.runtimeHandlerUsedToPullImage)
	withContainer, ok := manager.getImageRecordWithRuntimeHandlerInImageCriAPIFeatureGate(imageID(2), "")
	require.True(t, ok)
	assert.Equal(zero, withContainer.firstDetected)
	assert.True(withContainer.lastUsed.After(startTime))
	assert.Equal("", withContainer.runtimeHandlerUsedToPullImage)
}

func TestDetectImagesWithNewImage(t *testing.T) {
	ctx := context.Background()
	mockStatsProvider := statstest.NewMockProvider(t)

	// Just one image initially.
	manager, fakeRuntime := newRealImageGCManager(ImageGCPolicy{}, mockStatsProvider)
	fakeRuntime.ImageList = []container.Image{
		makeImage(0, 1024),
		makeImage(1, 2048),
	}
	fakeRuntime.AllPodList = []*containertest.FakePod{
		{Pod: &container.Pod{
			Containers: []*container.Container{
				makeContainer(1),
			},
		}},
	}

	_, err := manager.detectImages(ctx, zero)
	assert := assert.New(t)
	require.NoError(t, err)
	assert.Equal(2, manager.imageRecordsLen())

	// Add a new image.
	fakeRuntime.ImageList = []container.Image{
		makeImage(0, 1024),
		makeImage(1, 1024),
		makeImage(2, 1024),
	}

	detectedTime := zero.Add(time.Second)
	startTime := time.Now().Add(-time.Millisecond)
	_, err = manager.detectImages(ctx, detectedTime)
	require.NoError(t, err)
	assert.Equal(3, manager.imageRecordsLen())
	noContainer, ok := manager.getImageRecord(imageID(0))
	require.True(t, ok)
	assert.Equal(zero, noContainer.firstDetected)
	assert.Equal(zero, noContainer.lastUsed)
	assert.Equal("", noContainer.runtimeHandlerUsedToPullImage)
	withContainer, ok := manager.getImageRecord(imageID(1))
	require.True(t, ok)
	assert.Equal(zero, withContainer.firstDetected)
	assert.True(withContainer.lastUsed.After(startTime))
	assert.Equal("", noContainer.runtimeHandlerUsedToPullImage)
	newContainer, ok := manager.getImageRecord(imageID(2))
	require.True(t, ok)
	assert.Equal(detectedTime, newContainer.firstDetected)
	assert.Equal(zero, noContainer.lastUsed)
	assert.Equal("", noContainer.runtimeHandlerUsedToPullImage)
}

func TestDeleteUnusedImagesExemptSandboxImage(t *testing.T) {
	ctx := context.Background()
	mockStatsProvider := statstest.NewMockProvider(t)

	manager, fakeRuntime := newRealImageGCManager(ImageGCPolicy{}, mockStatsProvider)
	fakeRuntime.ImageList = []container.Image{
		{
			ID:     sandboxImage,
			Size:   1024,
			Pinned: true,
		},
	}

	err := manager.DeleteUnusedImages(ctx)
	assert := assert.New(t)
	assert.Len(fakeRuntime.ImageList, 1)
	require.NoError(t, err)
}

func TestDeletePinnedImage(t *testing.T) {
	ctx := context.Background()
	mockStatsProvider := statstest.NewMockProvider(t)

	manager, fakeRuntime := newRealImageGCManager(ImageGCPolicy{}, mockStatsProvider)
	fakeRuntime.ImageList = []container.Image{
		{
			ID:     sandboxImage,
			Size:   1024,
			Pinned: true,
		},
		{
			ID:   sandboxImage,
			Size: 1024,
		},
	}

	err := manager.DeleteUnusedImages(ctx)
	assert := assert.New(t)
	assert.Len(fakeRuntime.ImageList, 1)
	require.NoError(t, err)
}

func TestDoNotDeletePinnedImage(t *testing.T) {
	ctx := context.Background()
	mockStatsProvider := statstest.NewMockProvider(t)

	manager, fakeRuntime := newRealImageGCManager(ImageGCPolicy{}, mockStatsProvider)
	fakeRuntime.ImageList = []container.Image{
		{
			ID:     "1",
			Size:   1024,
			Pinned: true,
		},
		{
			ID:   "2",
			Size: 1024,
		},
	}

	assert := assert.New(t)
	getImagesAndFreeSpace(ctx, t, assert, manager, fakeRuntime, 4096, 1024, 1, time.Now())
}

func TestDeleteUnPinnedImage(t *testing.T) {
	ctx := context.Background()
	mockStatsProvider := statstest.NewMockProvider(t)

	manager, fakeRuntime := newRealImageGCManager(ImageGCPolicy{}, mockStatsProvider)
	fakeRuntime.ImageList = []container.Image{
		{
			ID:     "1",
			Size:   1024,
			Pinned: false,
		},
		{
			ID:   "2",
			Size: 1024,
		},
	}

	assert := assert.New(t)
	getImagesAndFreeSpace(ctx, t, assert, manager, fakeRuntime, 2048, 2048, 0, time.Now())
}

func TestAllPinnedImages(t *testing.T) {
	ctx := context.Background()
	mockStatsProvider := statstest.NewMockProvider(t)

	manager, fakeRuntime := newRealImageGCManager(ImageGCPolicy{}, mockStatsProvider)
	fakeRuntime.ImageList = []container.Image{
		{
			ID:     "1",
			Size:   1024,
			Pinned: true,
		},
		{
			ID:     "2",
			Size:   1024,
			Pinned: true,
		},
	}

	assert := assert.New(t)
	getImagesAndFreeSpace(ctx, t, assert, manager, fakeRuntime, 2048, 0, 2, time.Now())
}

func TestDetectImagesContainerStopped(t *testing.T) {
	ctx := context.Background()
	mockStatsProvider := statstest.NewMockProvider(t)

	manager, fakeRuntime := newRealImageGCManager(ImageGCPolicy{}, mockStatsProvider)
	fakeRuntime.ImageList = []container.Image{
		makeImage(0, 1024),
		makeImage(1, 2048),
	}
	fakeRuntime.AllPodList = []*containertest.FakePod{
		{Pod: &container.Pod{
			Containers: []*container.Container{
				makeContainer(1),
			},
		}},
	}

	_, err := manager.detectImages(ctx, zero)
	assert := assert.New(t)
	require.NoError(t, err)
	assert.Equal(2, manager.imageRecordsLen())
	withContainer, ok := manager.getImageRecord(imageID(1))
	require.True(t, ok)

	// Simulate container being stopped.
	fakeRuntime.AllPodList = []*containertest.FakePod{}
	_, err = manager.detectImages(ctx, time.Now())
	require.NoError(t, err)
	assert.Equal(2, manager.imageRecordsLen())
	container1, ok := manager.getImageRecord(imageID(0))
	require.True(t, ok)
	assert.Equal(zero, container1.firstDetected)
	assert.Equal(zero, container1.lastUsed)
	container2, ok := manager.getImageRecord(imageID(1))
	require.True(t, ok)
	assert.Equal(zero, container2.firstDetected)
	assert.True(container2.lastUsed.Equal(withContainer.lastUsed))
}

func TestDetectImagesWithRemovedImages(t *testing.T) {
	ctx := context.Background()
	mockStatsProvider := statstest.NewMockProvider(t)

	manager, fakeRuntime := newRealImageGCManager(ImageGCPolicy{}, mockStatsProvider)
	fakeRuntime.ImageList = []container.Image{
		makeImage(0, 1024),
		makeImage(1, 2048),
	}
	fakeRuntime.AllPodList = []*containertest.FakePod{
		{Pod: &container.Pod{
			Containers: []*container.Container{
				makeContainer(1),
			},
		}},
	}

	_, err := manager.detectImages(ctx, zero)
	assert := assert.New(t)
	require.NoError(t, err)
	assert.Equal(2, manager.imageRecordsLen())

	// Simulate both images being removed.
	fakeRuntime.ImageList = []container.Image{}
	_, err = manager.detectImages(ctx, time.Now())
	require.NoError(t, err)
	assert.Equal(0, manager.imageRecordsLen())
}

func TestFreeSpaceImagesInUseContainersAreIgnored(t *testing.T) {
	ctx := context.Background()
	mockStatsProvider := statstest.NewMockProvider(t)

	manager, fakeRuntime := newRealImageGCManager(ImageGCPolicy{}, mockStatsProvider)
	fakeRuntime.ImageList = []container.Image{
		makeImage(0, 1024),
		makeImage(1, 2048),
	}
	fakeRuntime.AllPodList = []*containertest.FakePod{
		{Pod: &container.Pod{
			Containers: []*container.Container{
				makeContainer(1),
			},
		}},
	}

	assert := assert.New(t)
	getImagesAndFreeSpace(ctx, t, assert, manager, fakeRuntime, 2048, 1024, 1, time.Now())
}

func TestDeleteUnusedImagesRemoveAllUnusedImages(t *testing.T) {
	ctx := context.Background()
	mockStatsProvider := statstest.NewMockProvider(t)

	manager, fakeRuntime := newRealImageGCManager(ImageGCPolicy{}, mockStatsProvider)
	fakeRuntime.ImageList = []container.Image{
		makeImage(0, 1024),
		makeImage(1, 2048),
		makeImage(2, 2048),
	}
	fakeRuntime.AllPodList = []*containertest.FakePod{
		{Pod: &container.Pod{
			Containers: []*container.Container{
				makeContainer(2),
			},
		}},
	}

	err := manager.DeleteUnusedImages(ctx)
	assert := assert.New(t)
	require.NoError(t, err)
	assert.Len(fakeRuntime.ImageList, 1)
}

func TestDeleteUnusedImagesLimitByImageLiveTime(t *testing.T) {
	ctx := context.Background()
	mockStatsProvider := statstest.NewMockProvider(t)

	manager, fakeRuntime := newRealImageGCManager(ImageGCPolicy{
		MinAge: time.Second * 3, // set minAge to 3 seconds,
	}, mockStatsProvider)
	fakeRuntime.ImageList = []container.Image{
		makeImage(0, 1024),
		makeImage(1, 2048),
		makeImage(2, 2048),
	}
	fakeRuntime.AllPodList = []*containertest.FakePod{
		{Pod: &container.Pod{
			Containers: []*container.Container{
				makeContainer(2),
			},
		}},
	}
	// start to detect images
	manager.Start()
	// try to delete images, but images are not old enough,so no image will be deleted
	err := manager.DeleteUnusedImages(ctx)
	assert := assert.New(t)
	require.NoError(t, err)
	assert.Len(fakeRuntime.ImageList, 3)
	// sleep 3 seconds, then images will be old enough to be deleted
	time.Sleep(time.Second * 3)
	err = manager.DeleteUnusedImages(ctx)
	require.NoError(t, err)
	assert.Len(fakeRuntime.ImageList, 1)
}

func TestFreeSpaceRemoveByLeastRecentlyUsed(t *testing.T) {
	ctx := context.Background()
	mockStatsProvider := statstest.NewMockProvider(t)

	manager, fakeRuntime := newRealImageGCManager(ImageGCPolicy{}, mockStatsProvider)
	fakeRuntime.ImageList = []container.Image{
		makeImage(0, 1024),
		makeImage(1, 2048),
	}
	fakeRuntime.AllPodList = []*containertest.FakePod{
		{Pod: &container.Pod{
			Containers: []*container.Container{
				makeContainer(0),
				makeContainer(1),
			},
		}},
	}

	// Make 1 be more recently used than 0.
	_, err := manager.detectImages(ctx, zero)
	require.NoError(t, err)
	fakeRuntime.AllPodList = []*containertest.FakePod{
		{Pod: &container.Pod{
			Containers: []*container.Container{
				makeContainer(1),
			},
		}},
	}
	// manager.detectImages uses time.Now() to update the image's lastUsed field.
	// On Windows, consecutive time.Now() calls can return the same timestamp, which would mean
	// that the second image is NOT newer than the first one.
	// time.Sleep will result in the timestamp to be updated as well.
	if goruntime.GOOS == "windows" {
		time.Sleep(time.Millisecond)
	}
	_, err = manager.detectImages(ctx, time.Now())
	require.NoError(t, err)
	fakeRuntime.AllPodList = []*containertest.FakePod{
		{Pod: &container.Pod{
			Containers: []*container.Container{},
		}},
	}
	_, err = manager.detectImages(ctx, time.Now())
	require.NoError(t, err)
	require.Equal(t, 2, manager.imageRecordsLen())

	// We're setting the delete time one minute in the future, so the time the image
	// was first detected and the delete time are different.
	assert := assert.New(t)
	getImagesAndFreeSpace(ctx, t, assert, manager, fakeRuntime, 1024, 1024, 1, time.Now().Add(time.Minute))
}

func TestFreeSpaceTiesBrokenByDetectedTime(t *testing.T) {
	ctx := context.Background()
	mockStatsProvider := statstest.NewMockProvider(t)

	manager, fakeRuntime := newRealImageGCManager(ImageGCPolicy{}, mockStatsProvider)
	fakeRuntime.ImageList = []container.Image{
		makeImage(0, 1024),
	}
	fakeRuntime.AllPodList = []*containertest.FakePod{
		{Pod: &container.Pod{
			Containers: []*container.Container{
				makeContainer(0),
			},
		}},
	}

	// Make 1 more recently detected but used at the same time as 0.
	_, err := manager.detectImages(ctx, zero)
	require.NoError(t, err)
	fakeRuntime.ImageList = []container.Image{
		makeImage(0, 1024),
		makeImage(1, 2048),
	}
	_, err = manager.detectImages(ctx, time.Now())
	require.NoError(t, err)
	fakeRuntime.AllPodList = []*containertest.FakePod{}
	_, err = manager.detectImages(ctx, time.Now())
	require.NoError(t, err)
	require.Equal(t, 2, manager.imageRecordsLen())

	assert := assert.New(t)
	getImagesAndFreeSpace(ctx, t, assert, manager, fakeRuntime, 1024, 2048, 1, time.Now())
}

func TestGarbageCollectBelowLowThreshold(t *testing.T) {
	ctx := context.Background()
	policy := ImageGCPolicy{
		HighThresholdPercent: 90,
		LowThresholdPercent:  80,
	}
	mockStatsProvider := statstest.NewMockProvider(t)
	manager, _ := newRealImageGCManager(policy, mockStatsProvider)

	// Expect 40% usage.
	imageStats := &statsapi.FsStats{
		AvailableBytes: uint64Ptr(600),
		CapacityBytes:  uint64Ptr(1000),
	}
	mockStatsProvider.EXPECT().ImageFsStats(mock.Anything).Return(imageStats, imageStats, nil)

	assert.NoError(t, manager.GarbageCollect(ctx, time.Now()))
}

func TestGarbageCollectCadvisorFailure(t *testing.T) {
	ctx := context.Background()
	policy := ImageGCPolicy{
		HighThresholdPercent: 90,
		LowThresholdPercent:  80,
	}
	mockStatsProvider := statstest.NewMockProvider(t)
	manager, _ := newRealImageGCManager(policy, mockStatsProvider)

	mockStatsProvider.EXPECT().ImageFsStats(mock.Anything).Return(&statsapi.FsStats{}, &statsapi.FsStats{}, fmt.Errorf("error"))
	assert.Error(t, manager.GarbageCollect(ctx, time.Now()))
}

func TestGarbageCollectBelowSuccess(t *testing.T) {
	ctx := context.Background()
	policy := ImageGCPolicy{
		HighThresholdPercent: 90,
		LowThresholdPercent:  80,
	}

	mockStatsProvider := statstest.NewMockProvider(t)
	manager, fakeRuntime := newRealImageGCManager(policy, mockStatsProvider)

	// Expect 95% usage and most of it gets freed.
	imageFs := &statsapi.FsStats{
		AvailableBytes: uint64Ptr(50),
		CapacityBytes:  uint64Ptr(1000),
	}
	mockStatsProvider.EXPECT().ImageFsStats(mock.Anything).Return(imageFs, imageFs, nil)
	fakeRuntime.ImageList = []container.Image{
		makeImage(0, 450),
	}

	assert.NoError(t, manager.GarbageCollect(ctx, time.Now()))
}

func TestGarbageCollectNotEnoughFreed(t *testing.T) {
	ctx := context.Background()
	policy := ImageGCPolicy{
		HighThresholdPercent: 90,
		LowThresholdPercent:  80,
	}
	mockStatsProvider := statstest.NewMockProvider(t)
	manager, fakeRuntime := newRealImageGCManager(policy, mockStatsProvider)

	// Expect 95% usage and little of it gets freed.
	imageFs := &statsapi.FsStats{
		AvailableBytes: uint64Ptr(50),
		CapacityBytes:  uint64Ptr(1000),
	}
	mockStatsProvider.EXPECT().ImageFsStats(mock.Anything).Return(imageFs, imageFs, nil)
	fakeRuntime.ImageList = []container.Image{
		makeImage(0, 50),
	}

	assert.Error(t, manager.GarbageCollect(ctx, time.Now()))
}

func TestGarbageCollectImageNotOldEnough(t *testing.T) {
	ctx := context.Background()
	policy := ImageGCPolicy{
		HighThresholdPercent: 90,
		LowThresholdPercent:  80,
		MinAge:               time.Minute * 1,
	}
	fakeRuntime := &containertest.FakeRuntime{}
	mockStatsProvider := statstest.NewMockProvider(t)
	manager := &realImageGCManager{
		runtime:       fakeRuntime,
		policy:        policy,
		imageRecords:  make(map[string]*imageRecord),
		statsProvider: mockStatsProvider,
		recorder:      &record.FakeRecorder{},
	}

	fakeRuntime.ImageList = []container.Image{
		makeImage(0, 1024),
		makeImage(1, 2048),
	}
	// 1 image is in use, and another one is not old enough
	fakeRuntime.AllPodList = []*containertest.FakePod{
		{Pod: &container.Pod{
			Containers: []*container.Container{
				makeContainer(1),
			},
		}},
	}

	fakeClock := testingclock.NewFakeClock(time.Now())
	t.Log(fakeClock.Now())
	_, err := manager.detectImages(ctx, fakeClock.Now())
	require.NoError(t, err)
	require.Equal(t, 2, manager.imageRecordsLen())
	// no space freed since one image is in used, and another one is not old enough
	assert := assert.New(t)
	getImagesAndFreeSpace(ctx, t, assert, manager, fakeRuntime, 1024, 0, 2, fakeClock.Now())

	// move clock by minAge duration, then 1 image will be garbage collected
	fakeClock.Step(policy.MinAge)
	getImagesAndFreeSpace(ctx, t, assert, manager, fakeRuntime, 1024, 1024, 1, fakeClock.Now())
}

func getImagesAndFreeSpace(ctx context.Context, t *testing.T, assert *assert.Assertions, im *realImageGCManager, fakeRuntime *containertest.FakeRuntime, spaceToFree, expectedSpaceFreed int64, imagesLen int, freeTime time.Time) {
	images, err := im.imagesInEvictionOrder(ctx, freeTime)
	require.NoError(t, err)
	spaceFreed, err := im.freeSpace(ctx, spaceToFree, freeTime, images)
	require.NoError(t, err)
	assert.EqualValues(expectedSpaceFreed, spaceFreed)
	assert.Len(fakeRuntime.ImageList, imagesLen)
}

func TestGarbageCollectImageTooOld(t *testing.T) {
	ctx := context.Background()
	policy := ImageGCPolicy{
		HighThresholdPercent: 90,
		LowThresholdPercent:  80,
		MinAge:               0,
		MaxAge:               time.Minute * 1,
	}
	fakeRuntime := &containertest.FakeRuntime{}
	mockStatsProvider := statstest.NewMockProvider(t)
	manager := &realImageGCManager{
		runtime:       fakeRuntime,
		policy:        policy,
		imageRecords:  make(map[string]*imageRecord),
		statsProvider: mockStatsProvider,
		recorder:      &record.FakeRecorder{},
	}

	fakeRuntime.ImageList = []container.Image{
		makeImage(0, 1024),
		makeImage(1, 2048),
	}
	// 1 image is in use, and another one is not old enough
	fakeRuntime.AllPodList = []*containertest.FakePod{
		{Pod: &container.Pod{
			Containers: []*container.Container{
				makeContainer(1),
			},
		}},
	}

	fakeClock := testingclock.NewFakeClock(time.Now())
	t.Log(fakeClock.Now())
	images, err := manager.imagesInEvictionOrder(ctx, fakeClock.Now())
	require.NoError(t, err)
	require.Len(t, images, 1)
	// Simulate pod having just used this image, but having been GC'd
	images[0].lastUsed = fakeClock.Now()

	// First GC round should not GC remaining image, as it was used too recently.
	assert := assert.New(t)
	oldStartTime := fakeClock.Now()
	images, err = manager.freeOldImages(ctx, images, oldStartTime, oldStartTime)
	require.NoError(t, err)
	assert.Len(images, 1)
	assert.Len(fakeRuntime.ImageList, 2)

	// move clock by a millisecond past maxAge duration, then 1 image will be garbage collected
	fakeClock.Step(policy.MaxAge + 1)
	images, err = manager.freeOldImages(ctx, images, fakeClock.Now(), oldStartTime)
	require.NoError(t, err)
	assert.Empty(images)
	assert.Len(fakeRuntime.ImageList, 1)
}

func TestGarbageCollectImageMaxAgeDisabled(t *testing.T) {
	ctx := context.Background()
	policy := ImageGCPolicy{
		HighThresholdPercent: 90,
		LowThresholdPercent:  80,
		MinAge:               0,
		MaxAge:               0,
	}
	fakeRuntime := &containertest.FakeRuntime{}
	mockStatsProvider := statstest.NewMockProvider(t)
	manager := &realImageGCManager{
		runtime:       fakeRuntime,
		policy:        policy,
		imageRecords:  make(map[string]*imageRecord),
		statsProvider: mockStatsProvider,
		recorder:      &record.FakeRecorder{},
	}

	assert := assert.New(t)
	fakeRuntime.ImageList = []container.Image{
		makeImage(0, 1024),
		makeImage(1, 2048),
	}
	assert.Len(fakeRuntime.ImageList, 2)
	// 1 image is in use, and another one is not old enough
	fakeRuntime.AllPodList = []*containertest.FakePod{
		{Pod: &container.Pod{
			Containers: []*container.Container{
				makeContainer(1),
			},
		}},
	}

	fakeClock := testingclock.NewFakeClock(time.Now())
	t.Log(fakeClock.Now())
	images, err := manager.imagesInEvictionOrder(ctx, fakeClock.Now())
	require.NoError(t, err)
	require.Len(t, images, 1)
	assert.Len(fakeRuntime.ImageList, 2)

	oldStartTime := fakeClock.Now()

	// First GC round should not GC remaining image, as it was used too recently.
	images, err = manager.freeOldImages(ctx, images, oldStartTime, oldStartTime)
	require.NoError(t, err)
	assert.Len(images, 1)
	assert.Len(fakeRuntime.ImageList, 2)

	// Move clock by a lot, and the images should continue to not be garbage collected
	// See https://stackoverflow.com/questions/25065055/what-is-the-maximum-time-time-in-go
	fakeClock.SetTime(time.Unix(1<<63-62135596801, 999999999))
	images, err = manager.freeOldImages(ctx, images, fakeClock.Now(), oldStartTime)
	require.NoError(t, err)
	assert.Len(images, 1)
	assert.Len(fakeRuntime.ImageList, 2)
}

func TestValidateImageGCPolicy(t *testing.T) {
	testCases := []struct {
		name          string
		imageGCPolicy ImageGCPolicy
		expectErr     string
	}{
		{
			name: "Test for LowThresholdPercent < HighThresholdPercent",
			imageGCPolicy: ImageGCPolicy{
				HighThresholdPercent: 2,
				LowThresholdPercent:  1,
			},
		},
		{
			name: "Test for HighThresholdPercent < 0,",
			imageGCPolicy: ImageGCPolicy{
				HighThresholdPercent: -1,
			},
			expectErr: "invalid HighThresholdPercent -1, must be in range [0-100]",
		},
		{
			name: "Test for HighThresholdPercent > 100",
			imageGCPolicy: ImageGCPolicy{
				HighThresholdPercent: 101,
			},
			expectErr: "invalid HighThresholdPercent 101, must be in range [0-100]",
		},
		{
			name: "Test for LowThresholdPercent < 0",
			imageGCPolicy: ImageGCPolicy{
				LowThresholdPercent: -1,
			},
			expectErr: "invalid LowThresholdPercent -1, must be in range [0-100]",
		},
		{
			name: "Test for LowThresholdPercent > 100",
			imageGCPolicy: ImageGCPolicy{
				LowThresholdPercent: 101,
			},
			expectErr: "invalid LowThresholdPercent 101, must be in range [0-100]",
		},
		{
			name: "Test for LowThresholdPercent > HighThresholdPercent",
			imageGCPolicy: ImageGCPolicy{
				HighThresholdPercent: 1,
				LowThresholdPercent:  2,
			},
			expectErr: "LowThresholdPercent 2 can not be higher than HighThresholdPercent 1",
		},
	}

	for _, tc := range testCases {
		if _, err := NewImageGCManager(nil, nil, nil, nil, tc.imageGCPolicy, noopoteltrace.NewTracerProvider()); err != nil {
			if err.Error() != tc.expectErr {
				t.Errorf("[%s:]Expected err:%v, but got:%v", tc.name, tc.expectErr, err.Error())
			}
		}
	}
}

func uint64Ptr(i uint64) *uint64 {
	return &i
}
