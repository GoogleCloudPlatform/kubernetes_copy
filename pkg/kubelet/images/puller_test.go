/*
Copyright 2025 The Kubernetes Authors.

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
	"sync/atomic"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
	"k8s.io/kubernetes/pkg/kubelet/container"
)

// FakeImageService is a fake implementation of the ImageService interface that returns empty values.
type FakeImageService struct {
	pullImageFn func(ctx context.Context, image container.ImageSpec, secrets []v1.Secret, podSandboxConfig *runtimeapi.PodSandboxConfig) (string, error)
}

// NewFakeImageService creates a new FakeImageService.
func NewFakeImageService() *FakeImageService {
	return &FakeImageService{}
}

// PullImage mocks pulling an image.
func (f *FakeImageService) PullImage(ctx context.Context, image container.ImageSpec, secrets []v1.Secret, podSandboxConfig *runtimeapi.PodSandboxConfig) (string, error) {
	if f.pullImageFn != nil {
		return f.pullImageFn(ctx, image, secrets, podSandboxConfig)
	}
	return image.Image, nil
}

// GetImageRef mocks getting an image reference.
func (f *FakeImageService) GetImageRef(ctx context.Context, image container.ImageSpec) (string, error) {
	return "", nil
}

// ListImages mocks listing images.
func (f *FakeImageService) ListImages(ctx context.Context) ([]container.Image, error) {
	return nil, nil
}

// RemoveImage mocks removing an image.
func (f *FakeImageService) RemoveImage(ctx context.Context, image container.ImageSpec) error {
	return nil
}

// ImageStats mocks returning image statistics.
func (f *FakeImageService) ImageStats(ctx context.Context) (*container.ImageStats, error) {
	return nil, nil
}

// ImageFsInfo mocks returning image filesystem information.
func (f *FakeImageService) ImageFsInfo(ctx context.Context) (*runtimeapi.ImageFsInfoResponse, error) {
	return nil, nil
}

// GetImageSize mocks returning the size of an image.
func (f *FakeImageService) GetImageSize(ctx context.Context, image container.ImageSpec) (uint64, error) {
	return 0, nil
}

func Test_newParallelImagePuller(t *testing.T) {
	maxImagePullRequests := int32(2)
	totalImages := 10
	var counter atomic.Int32
	imagePullCh := make(chan struct{})
	pullCh := make(chan pullResult)
	imageService := NewFakeImageService()

	imageService.pullImageFn = func(ctx context.Context, image container.ImageSpec, secrets []v1.Secret, podSandboxConfig *runtimeapi.PodSandboxConfig) (string, error) {
		counter.Add(1)
		<-imagePullCh
		return image.Image, nil
	}
	puller := newParallelImagePuller(imageService, &maxImagePullRequests)

	for i := 0; i < totalImages; i++ {
		puller.pullImage(context.Background(), container.ImageSpec{Image: fmt.Sprintf("image-id-%d", i)}, nil, pullCh, nil)
	}
	// wait for goroutines to finish.
	// this is required because the pullImage() method spawns a goroutine internally
	// and we can not sync the test, 100 ms is more than enough to guarantee the
	// test does not flake.
	time.Sleep(100 * time.Millisecond)
	select {
	case <-pullCh:
		t.Fatalf("unexpected image pull")
	default:
	}

	if counter.Load() != maxImagePullRequests {
		t.Fatalf("expected %d pulls in parallel, got %d", maxImagePullRequests, counter.Load())
	}
	close(imagePullCh)

	for i := 0; i < totalImages; i++ {
		<-pullCh
	}

	if counter.Load() != int32(totalImages) {
		t.Fatalf("expected %d total pulls, got %d", totalImages, counter.Load())
	}
}
