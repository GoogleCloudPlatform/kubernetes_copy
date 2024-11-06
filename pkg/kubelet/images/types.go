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
	"time"

	v1 "k8s.io/api/core/v1"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
	kubeletconfiginternal "k8s.io/kubernetes/pkg/kubelet/apis/config"
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

// ImagePullManager keeps the state of images that were pulled and which are
// currently still being pulled.
// It should keep an internal state of images currently being pulled by the kubelet
// in order to determine whether to destroy a "pulling" record should an image
// pull fail.
type ImagePullManager interface {
	// RecordPullIntent records an intent to pull an image and should be called
	// before a pull of the image occurs.
	//
	// RecordPullIntent() should be called before every image pull. Each call of
	// RecordPullIntent() must match exactly one call of RecordImagePulled()/RecordImagePullFailed().
	//
	// `image` is the content of the pod's container `image` field.
	RecordPullIntent(image string) error
	// RecordImagePulled writes a record of an image being successfully pulled
	// with ImagePullCredentials.
	//
	// `credentials` must not be nil and must contain either exactly one Kubernetes
	// Secret coordinates in the `.KubernetesSecrets` slice or set `.NodePodsAccessible`
	// to `true`.
	//
	// `image` is the content of the pod's container `image` field.
	RecordImagePulled(image, imageRef string, credentials *kubeletconfiginternal.ImagePullCredentials)
	// RecordImagePullFailed should be called if an image failed to pull.
	//
	// Internally, it lowers its reference counter for the given image. If the
	// counter reaches zero, the pull intent record for the image is removed.
	//
	// `image` is the content of the pod's container `image` field.
	RecordImagePullFailed(image string)
	// MustAttemptImagePull evaluates the policy for the image specified in
	// `image` and if the policy demands verification, it checks the internal
	// cache to see if there's a record of pulling the image with the presented
	// set of credentials or if the image can be accessed by any of the node's pods.
	//
	// Returns true if the policy demands verification and no record of the pull
	// was found in the cache.
	//
	// `image` is the content of the pod's container `image` field.
	MustAttemptImagePull(image, imageRef string, credentials []kubeletconfiginternal.ImagePullSecret) bool
	// PruneUnknownRecords deletes all of the cache ImagePulledRecords for each of the images
	// whose imageRef does not appear in the `imageList` iff such an record was last updated
	// _before_ the `until` timestamp.
	//
	// This method is only expected to be called by the kubelet's image garbage collector.
	// `until` is a timestamp created _before_ the `imageList` was requested from the CRI.
	PruneUnknownRecords(imageList []string, until time.Time)
}

// PullRecordsAccessor allows unified access to ImagePullIntents/ImagePulledRecords
// irregardless of the backing database implementation
type PullRecordsAccessor interface {
	// ListImagePullIntents lists all the ImagePullIntents in the database.
	// ImagePullIntents that cannot be decoded will not appear in the list.
	// Returns nil and an error if there was a problem reading from the database.
	ListImagePullIntents() ([]*kubeletconfiginternal.ImagePullIntent, error)
	// ImagePullIntentExists returns whether a valid ImagePullIntent is present
	// for the given image.
	ImagePullIntentExists(image string) (bool, error)
	// WriteImagePullIntent writes a an intent record for the image into the database
	WriteImagePullIntent(image string) error
	// DeleteImagePullIntent removes an `image` intent record from the database
	DeleteImagePullIntent(image string) error

	// ListImagePulledRecords lists the database ImagePulledRecords.
	// Records that cannot be decoded will be ignored.
	// Returns an error if there was a problem reading from the database.
	ListImagePulledRecords() ([]*kubeletconfiginternal.ImagePulledRecord, error)
	// GetImagePulledRecord fetches an ImagePulledRecord for the given `imageRef`.
	// If a file for the `imageRef` is present but the contents cannot be decoded,
	// it returns a exists=true with err equal to the decoding error.
	GetImagePulledRecord(imageRef string) (record *kubeletconfiginternal.ImagePulledRecord, exists bool, err error)
	// WriteImagePulledRecord writes an ImagePulledRecord into the database.
	WriteImagePulledRecord(record *kubeletconfiginternal.ImagePulledRecord) error
	// DeleteImagePulledRecord removes an ImagePulledRecord for `imageRef` from the
	// database.
	DeleteImagePulledRecord(imageRef string) error
}
