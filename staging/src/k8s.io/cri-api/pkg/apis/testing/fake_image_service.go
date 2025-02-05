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

package testing

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"

	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
)

// FakeImageService fakes the image service.
type FakeImageService struct {
	sync.Mutex

	FakeImageSize uint64
	Called        []string
	Errors        map[string][]error
	Images        map[string]*runtimeapi.Image
	Pinned        map[string]bool

	pulledImages []*pulledImage

	FakeFilesystemUsage          []*runtimeapi.FilesystemUsage
	FakeContainerFilesystemUsage []*runtimeapi.FilesystemUsage
}

// SetFakeImages sets the list of fake images for the FakeImageService.
func (r *FakeImageService) SetFakeImages(images []string) {
	r.Lock()
	defer r.Unlock()

	r.Images = make(map[string]*runtimeapi.Image)
	for _, image := range images {
		r.Images[image] = r.makeFakeImage(
			&runtimeapi.ImageSpec{
				Image:       image,
				Annotations: make(map[string]string)})
	}
}

// SetFakeImagesWithAnnotations sets the list of fake images for the FakeImageService with annotations.
func (r *FakeImageService) SetFakeImagesWithAnnotations(imageSpecs []*runtimeapi.ImageSpec) {
	r.Lock()
	defer r.Unlock()

	r.Images = make(map[string]*runtimeapi.Image)
	for _, imageSpec := range imageSpecs {
		r.Images[imageSpec.Image] = r.makeFakeImage(imageSpec)
	}
}

// SetFakeImageSize sets the image size for the FakeImageService.
func (r *FakeImageService) SetFakeImageSize(size uint64) {
	r.Lock()
	defer r.Unlock()

	r.FakeImageSize = size
}

// SetFakeImagePinned sets the image Pinned field for one image.
func (r *FakeImageService) SetFakeImagePinned(image string, pinned bool) {
	r.Lock()
	defer r.Unlock()

	if r.Pinned == nil {
		r.Pinned = make(map[string]bool)
	}
	r.Pinned[image] = pinned
}

// SetFakeFilesystemUsage sets the FilesystemUsage for FakeImageService.
func (r *FakeImageService) SetFakeFilesystemUsage(usage []*runtimeapi.FilesystemUsage) {
	r.Lock()
	defer r.Unlock()

	r.FakeFilesystemUsage = usage
}

// SetFakeFilesystemUsage sets the FilesystemUsage for FakeImageService.
func (r *FakeImageService) SetFakeContainerFilesystemUsage(usage []*runtimeapi.FilesystemUsage) {
	r.Lock()
	defer r.Unlock()

	r.FakeContainerFilesystemUsage = usage
}

// NewFakeImageService creates a new FakeImageService.
func NewFakeImageService() *FakeImageService {
	return &FakeImageService{
		Called: make([]string, 0),
		Errors: make(map[string][]error),
		Images: make(map[string]*runtimeapi.Image),
	}
}

func (r *FakeImageService) makeFakeImage(image *runtimeapi.ImageSpec) *runtimeapi.Image {
	return &runtimeapi.Image{
		Id:       image.Image,
		Size:     r.FakeImageSize,
		Spec:     image,
		RepoTags: []string{image.Image},
		Pinned:   r.Pinned[image.Image],
	}
}

// stringInSlice returns true if s is in list
func stringInSlice(s string, list []string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}

	return false
}

// InjectError sets the error message for the FakeImageService.
func (r *FakeImageService) InjectError(f string, err error) {
	r.Lock()
	defer r.Unlock()
	r.Errors[f] = append(r.Errors[f], err)
}

// caller of popError must grab a lock.
func (r *FakeImageService) popError(f string) error {
	if r.Errors == nil {
		return nil
	}
	errs := r.Errors[f]
	if len(errs) == 0 {
		return nil
	}
	err, errs := errs[0], errs[1:]
	r.Errors[f] = errs
	return err
}

// ListImages returns the list of images from FakeImageService or error if it was previously set.
func (r *FakeImageService) ListImages(_ context.Context, filter *runtimeapi.ImageFilter) ([]*runtimeapi.Image, error) {
	r.Lock()
	defer r.Unlock()

	r.Called = append(r.Called, "ListImages")
	if err := r.popError("ListImages"); err != nil {
		return nil, err
	}

	images := make([]*runtimeapi.Image, 0)
	for _, img := range r.Images {
		if filter != nil && filter.Image != nil {
			if !stringInSlice(filter.Image.Image, img.RepoTags) {
				continue
			}
		}

		images = append(images, img)
	}
	return images, nil
}

// ImageStatus returns the status of the image from the FakeImageService.
func (r *FakeImageService) ImageStatus(_ context.Context, image *runtimeapi.ImageSpec, verbose bool) (*runtimeapi.ImageStatusResponse, error) {
	r.Lock()
	defer r.Unlock()

	r.Called = append(r.Called, "ImageStatus")
	if err := r.popError("ImageStatus"); err != nil {
		return nil, err
	}

	return &runtimeapi.ImageStatusResponse{Image: r.Images[image.Image]}, nil
}

// PullImage emulate pulling the image from the FakeImageService.
func (r *FakeImageService) PullImage(_ context.Context, image *runtimeapi.ImageSpec, auth *runtimeapi.AuthConfig, podSandboxConfig *runtimeapi.PodSandboxConfig) (string, error) {
	r.Lock()
	defer r.Unlock()

	r.Called = append(r.Called, "PullImage")
	if err := r.popError("PullImage"); err != nil {
		return "", err
	}

	r.pulledImages = append(r.pulledImages, &pulledImage{imageSpec: image, authConfig: auth})
	// ImageID should be randomized for real container runtime, but here just use
	// image's name for easily making fake images.
	imageID := image.Image
	if _, ok := r.Images[imageID]; !ok {
		r.Images[imageID] = r.makeFakeImage(image)
	}

	return imageID, nil
}

// RemoveImage removes image from the FakeImageService.
func (r *FakeImageService) RemoveImage(_ context.Context, image *runtimeapi.ImageSpec) error {
	r.Lock()
	defer r.Unlock()

	r.Called = append(r.Called, "RemoveImage")
	if err := r.popError("RemoveImage"); err != nil {
		return err
	}

	// Remove the image
	delete(r.Images, image.Image)

	return nil
}

// ImageFsInfo returns information of the filesystem that is used to store images.
func (r *FakeImageService) ImageFsInfo(_ context.Context) (*runtimeapi.ImageFsInfoResponse, error) {
	r.Lock()
	defer r.Unlock()

	r.Called = append(r.Called, "ImageFsInfo")
	if err := r.popError("ImageFsInfo"); err != nil {
		return nil, err
	}

	return &runtimeapi.ImageFsInfoResponse{
		ImageFilesystems:     r.FakeFilesystemUsage,
		ContainerFilesystems: r.FakeContainerFilesystemUsage,
	}, nil
}

// AssertImagePulledWithAuth validates whether the image was pulled with auth and asserts if it wasn't.
func (r *FakeImageService) AssertImagePulledWithAuth(t *testing.T, image *runtimeapi.ImageSpec, auth *runtimeapi.AuthConfig, failMsg string) {
	r.Lock()
	defer r.Unlock()
	expected := &pulledImage{imageSpec: image, authConfig: auth}
	assert.Contains(t, r.pulledImages, expected, failMsg)
}

type pulledImage struct {
	imageSpec  *runtimeapi.ImageSpec
	authConfig *runtimeapi.AuthConfig
}
