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

package images

import (
	"context"
	"errors"

	v1 "k8s.io/api/core/v1"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
)

var (
	// ErrImagePullBackOff - Container image pull failed, kubelet is backing off image pull
	ErrImagePullBackOff = errors.New("ImagePullBackOff")

	// ErrImageInspect - Unable to inspect image
	ErrImageInspect = errors.New("ImageInspectError")

	// ErrImagePull - General image pull error
	ErrImagePull = errors.New("ErrImagePull")

	// ErrImageNeverPull - Required Image is absent on host and PullPolicy is NeverPullImage
	ErrImageNeverPull = errors.New("ErrImageNeverPull")

	// ErrInvalidImageName - Unable to parse the image name.
	ErrInvalidImageName = errors.New("InvalidImageName")
)

// ImageManager provides an interface to manage the lifecycle of images.
// Implementations of this interface are expected to deal with pulling (downloading),
// managing, and deleting container images.
// Implementations are expected to abstract the underlying runtimes.
// Implementations are expected to be thread safe.
type ImageManager interface {
	// EnsureImageExists ensures that image specified by `imgRef` exists.
	EnsureImageExists(ctx context.Context, objRef *v1.ObjectReference, pod *v1.Pod, imgRef string, pullSecrets []v1.Secret, podSandboxConfig *runtimeapi.PodSandboxConfig, podRuntimeHandler string, pullPolicy v1.PullPolicy) (string, string, error)

	// TODO(ronl): consolidating image managing and deleting operation in this interface
}
