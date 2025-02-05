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

	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
	"k8s.io/klog/v2"
	crededentialprovider "k8s.io/kubernetes/pkg/credentialprovider"
	"k8s.io/kubernetes/pkg/features"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
)

// PullImage pulls an image from the network to local storage using the supplied
// secrets if necessary.
func (m *kubeGenericRuntimeManager) PullImage(ctx context.Context, image kubecontainer.ImageSpec, credentials []crededentialprovider.TrackedAuthConfig, podSandboxConfig *runtimeapi.PodSandboxConfig) (string, *crededentialprovider.TrackedAuthConfig, error) {
	img := image.Image
	imgSpec := toRuntimeAPIImageSpec(image)

	if len(credentials) == 0 {
		klog.V(3).InfoS("Pulling image without credentials", "image", img)

		imageRef, err := m.imageService.PullImage(ctx, imgSpec, nil, podSandboxConfig)
		if err != nil {
			klog.ErrorS(err, "Failed to pull image", "image", img)
			return "", nil, err
		}

		return imageRef, nil, nil
	}

	var pullErrs []error
	for _, currentCreds := range credentials {
		auth := &runtimeapi.AuthConfig{
			Username:      currentCreds.Username,
			Password:      currentCreds.Password,
			Auth:          currentCreds.Auth,
			ServerAddress: currentCreds.ServerAddress,
			IdentityToken: currentCreds.IdentityToken,
			RegistryToken: currentCreds.RegistryToken,
		}

		imageRef, err := m.imageService.PullImage(ctx, imgSpec, auth, podSandboxConfig)
		// If there was no error, return success
		if err == nil {
			return imageRef, &currentCreds, nil
		}

		pullErrs = append(pullErrs, err)
	}

	return "", nil, utilerrors.NewAggregate(pullErrs)
}

// GetImageRef gets the ID of the image which has already been in
// the local storage. It returns ("", nil) if the image isn't in the local storage.
func (m *kubeGenericRuntimeManager) GetImageRef(ctx context.Context, image kubecontainer.ImageSpec) (string, error) {
	resp, err := m.imageService.ImageStatus(ctx, toRuntimeAPIImageSpec(image), false)
	if err != nil {
		klog.ErrorS(err, "Failed to get image status", "image", image.Image)
		return "", err
	}
	if resp.Image == nil {
		return "", nil
	}
	return resp.Image.Id, nil
}

func (m *kubeGenericRuntimeManager) GetImageSize(ctx context.Context, image kubecontainer.ImageSpec) (uint64, error) {
	resp, err := m.imageService.ImageStatus(ctx, toRuntimeAPIImageSpec(image), false)
	if err != nil {
		klog.ErrorS(err, "Failed to get image status", "image", image.Image)
		return 0, err
	}
	if resp.Image == nil {
		return 0, nil
	}
	return resp.Image.Size_, nil
}

// ListImages gets all images currently on the machine.
func (m *kubeGenericRuntimeManager) ListImages(ctx context.Context) ([]kubecontainer.Image, error) {
	var images []kubecontainer.Image

	allImages, err := m.imageService.ListImages(ctx, nil)
	if err != nil {
		klog.ErrorS(err, "Failed to list images")
		return nil, err
	}

	for _, img := range allImages {
		// Container runtimes may choose not to implement changes needed for KEP 4216. If
		// the changes are not implemented by a container runtime, the exisiting behavior
		// of not populating the runtimeHandler CRI field in ImageSpec struct is preserved.
		// Therefore, when RuntimeClassInImageCriAPI feature gate is set, check to see if this
		// field is empty and log a warning message.
		if utilfeature.DefaultFeatureGate.Enabled(features.RuntimeClassInImageCriAPI) {
			if img.Spec == nil || (img.Spec != nil && img.Spec.RuntimeHandler == "") {
				klog.V(2).InfoS("WARNING: RuntimeHandler is empty", "ImageID", img.Id)
			}
		}

		images = append(images, kubecontainer.Image{
			ID:          img.Id,
			Size:        int64(img.Size_),
			RepoTags:    img.RepoTags,
			RepoDigests: img.RepoDigests,
			Spec:        toKubeContainerImageSpec(img),
			Pinned:      img.Pinned,
		})
	}

	return images, nil
}

// RemoveImage removes the specified image.
func (m *kubeGenericRuntimeManager) RemoveImage(ctx context.Context, image kubecontainer.ImageSpec) error {
	err := m.imageService.RemoveImage(ctx, &runtimeapi.ImageSpec{Image: image.Image})
	if err != nil {
		klog.ErrorS(err, "Failed to remove image", "image", image.Image)
		return err
	}

	return nil
}

// ImageStats returns the statistics of the image.
// Notice that current logic doesn't really work for images which share layers (e.g. docker image),
// this is a known issue, and we'll address this by getting imagefs stats directly from CRI.
// TODO: Get imagefs stats directly from CRI.
func (m *kubeGenericRuntimeManager) ImageStats(ctx context.Context) (*kubecontainer.ImageStats, error) {
	allImages, err := m.imageService.ListImages(ctx, nil)
	if err != nil {
		klog.ErrorS(err, "Failed to list images")
		return nil, err
	}
	stats := &kubecontainer.ImageStats{}
	for _, img := range allImages {
		stats.TotalStorageBytes += img.Size_
	}
	return stats, nil
}

func (m *kubeGenericRuntimeManager) ImageFsInfo(ctx context.Context) (*runtimeapi.ImageFsInfoResponse, error) {
	allImages, err := m.imageService.ImageFsInfo(ctx)
	if err != nil {
		klog.ErrorS(err, "Failed to get image filesystem")
		return nil, err
	}
	return allImages, nil
}
