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
	"time"

	"k8s.io/apimachinery/pkg/util/wait"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
	"k8s.io/kubernetes/pkg/credentialprovider"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
)

type pullResult struct {
	imageRef        string
	imageSize       uint64
	err             error
	pullDuration    time.Duration
	credentialsUsed *credentialprovider.TrackedAuthConfig
}

type imagePuller interface {
	pullImage(context.Context, kubecontainer.ImageSpec, []credentialprovider.TrackedAuthConfig, chan<- pullResult, *runtimeapi.PodSandboxConfig)
}

var _, _ imagePuller = &parallelImagePuller{}, &serialImagePuller{}

type parallelImagePuller struct {
	imageService kubecontainer.ImageService
	tokens       chan struct{}
}

func newParallelImagePuller(imageService kubecontainer.ImageService, maxParallelImagePulls *int32) imagePuller {
	if maxParallelImagePulls == nil || *maxParallelImagePulls < 1 {
		return &parallelImagePuller{imageService, nil}
	}
	return &parallelImagePuller{imageService, make(chan struct{}, *maxParallelImagePulls)}
}

func (pip *parallelImagePuller) pullImage(ctx context.Context, spec kubecontainer.ImageSpec, credentials []credentialprovider.TrackedAuthConfig, pullChan chan<- pullResult, podSandboxConfig *runtimeapi.PodSandboxConfig) {
	go func() {
		if pip.tokens != nil {
			pip.tokens <- struct{}{}
			defer func() { <-pip.tokens }()
		}
		startTime := time.Now()
		imageRef, creds, err := pip.imageService.PullImage(ctx, spec, credentials, podSandboxConfig)
		var size uint64
		if err == nil && imageRef != "" {
			// Getting the image size with best effort, ignoring the error.
			size, _ = pip.imageService.GetImageSize(ctx, spec)
		}
		pullChan <- pullResult{
			imageRef:        imageRef,
			imageSize:       size,
			err:             err,
			pullDuration:    time.Since(startTime),
			credentialsUsed: creds,
		}
	}()
}

// Maximum number of image pull requests than can be queued.
const maxImagePullRequests = 10

type serialImagePuller struct {
	imageService kubecontainer.ImageService
	pullRequests chan *imagePullRequest
}

func newSerialImagePuller(imageService kubecontainer.ImageService) imagePuller {
	imagePuller := &serialImagePuller{imageService, make(chan *imagePullRequest, maxImagePullRequests)}
	go wait.Until(imagePuller.processImagePullRequests, time.Second, wait.NeverStop)
	return imagePuller
}

type imagePullRequest struct {
	ctx              context.Context
	spec             kubecontainer.ImageSpec
	credentials      []credentialprovider.TrackedAuthConfig
	pullChan         chan<- pullResult
	podSandboxConfig *runtimeapi.PodSandboxConfig
}

func (sip *serialImagePuller) pullImage(ctx context.Context, spec kubecontainer.ImageSpec, credentials []credentialprovider.TrackedAuthConfig, pullChan chan<- pullResult, podSandboxConfig *runtimeapi.PodSandboxConfig) {
	sip.pullRequests <- &imagePullRequest{
		ctx:              ctx,
		spec:             spec,
		credentials:      credentials,
		pullChan:         pullChan,
		podSandboxConfig: podSandboxConfig,
	}
}

func (sip *serialImagePuller) processImagePullRequests() {
	for pullRequest := range sip.pullRequests {
		startTime := time.Now()
		imageRef, creds, err := sip.imageService.PullImage(pullRequest.ctx, pullRequest.spec, pullRequest.credentials, pullRequest.podSandboxConfig)
		var size uint64
		if err == nil && imageRef != "" {
			// Getting the image size with best effort, ignoring the error.
			size, _ = sip.imageService.GetImageSize(pullRequest.ctx, pullRequest.spec)
		}
		pullRequest.pullChan <- pullResult{
			imageRef:  imageRef,
			imageSize: size,
			err:       err,
			// Note: pullDuration includes getting the image size.
			pullDuration:    time.Since(startTime),
			credentialsUsed: creds,
		}
	}
}
