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

package cri

import (
	"context"
	"time"

	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
)

// RuntimeVersioner contains methods for runtime name, version and API version.
type RuntimeVersioner interface {
	// Version returns the runtime name, runtime version and runtime API version
	Version(ctx context.Context, apiVersion string) (*runtimeapi.VersionResponse, error)
}

// ContainerManager contains methods to manipulate containers managed by a
// container runtime. The methods are thread-safe.
type ContainerManager interface {
	// CreateContainer creates a new container in specified PodSandbox.
	CreateContainer(ctx context.Context, podSandboxID string, config *runtimeapi.ContainerConfig, sandboxConfig *runtimeapi.PodSandboxConfig) (string, error)
	// StartContainer starts the container.
	StartContainer(ctx context.Context, containerID string) error
	// StopContainer stops a running container with a grace period (i.e., timeout).
	StopContainer(ctx context.Context, containerID string, timeout int64) error
	// RemoveContainer removes the container.
	RemoveContainer(ctx context.Context, containerID string) error
	// ListContainers lists all containers by filters.
	ListContainers(ctx context.Context, filter *runtimeapi.ContainerFilter) ([]*runtimeapi.Container, error)
	// ContainerStatus returns the status of the container.
	ContainerStatus(ctx context.Context, containerID string, verbose bool) (*runtimeapi.ContainerStatusResponse, error)
	// UpdateContainerResources updates ContainerConfig of the container synchronously.
	// If runtime fails to transactionally update the requested resources, an error is returned.
	UpdateContainerResources(ctx context.Context, containerID string, resources *runtimeapi.ContainerResources) error
	// ExecSync executes a command in the container, and returns the stdout output.
	// If command exits with a non-zero exit code, an error is returned.
	ExecSync(ctx context.Context, containerID string, cmd []string, timeout time.Duration) (stdout []byte, stderr []byte, err error)
	// Exec prepares a streaming endpoint to execute a command in the container, and returns the address.
	Exec(ctx context.Context, request *runtimeapi.ExecRequest) (*runtimeapi.ExecResponse, error)
	// Attach prepares a streaming endpoint to attach to a running container, and returns the address.
	Attach(ctx context.Context, req *runtimeapi.AttachRequest) (*runtimeapi.AttachResponse, error)
	// ReopenContainerLog asks runtime to reopen the stdout/stderr log file
	// for the container. If it returns error, new container log file MUST NOT
	// be created.
	ReopenContainerLog(ctx context.Context, ContainerID string) error
	// CheckpointContainer checkpoints a container
	CheckpointContainer(ctx context.Context, options *runtimeapi.CheckpointContainerRequest) error
	// GetContainerEvents gets container events from the CRI runtime
	GetContainerEvents(ctx context.Context, containerEventsCh chan *runtimeapi.ContainerEventResponse, connectionEstablishedCallback func(runtimeapi.RuntimeService_GetContainerEventsClient)) error
}

// PodSandboxManager contains methods for operating on PodSandboxes. The methods
// are thread-safe.
type PodSandboxManager interface {
	// RunPodSandbox creates and starts a pod-level sandbox. Runtimes should ensure
	// the sandbox is in ready state.
	RunPodSandbox(ctx context.Context, config *runtimeapi.PodSandboxConfig, runtimeHandler string) (string, error)
	// StopPodSandbox stops the sandbox. If there are any running containers in the
	// sandbox, they should be force terminated.
	StopPodSandbox(ctx context.Context, podSandboxID string) error
	// RemovePodSandbox removes the sandbox. If there are running containers in the
	// sandbox, they should be forcibly removed.
	RemovePodSandbox(ctx context.Context, podSandboxID string) error
	// PodSandboxStatus returns the Status of the PodSandbox.
	PodSandboxStatus(ctx context.Context, podSandboxID string, verbose bool) (*runtimeapi.PodSandboxStatusResponse, error)
	// ListPodSandbox returns a list of Sandbox.
	ListPodSandbox(ctx context.Context, filter *runtimeapi.PodSandboxFilter) ([]*runtimeapi.PodSandbox, error)
	// PortForward prepares a streaming endpoint to forward ports from a PodSandbox, and returns the address.
	PortForward(ctx context.Context, request *runtimeapi.PortForwardRequest) (*runtimeapi.PortForwardResponse, error)
	// UpdatePodSandboxResources synchronously updates the PodSandboxConfig with
	// the pod-level resource configuration. This method is called _after_ the
	// Kubelet reconfigures the pod-level cgroups.
	// This request is treated as best effort, and failure will not block the
	// Kubelet with proceeding with a resize.
	UpdatePodSandboxResources(ctx context.Context, request *runtimeapi.UpdatePodSandboxResourcesRequest) (*runtimeapi.UpdatePodSandboxResourcesResponse, error)
}

// ContainerStatsManager contains methods for retrieving the container
// statistics.
type ContainerStatsManager interface {
	// ContainerStats returns stats of the container. If the container does not
	// exist, the call returns an error.
	ContainerStats(ctx context.Context, containerID string) (*runtimeapi.ContainerStats, error)
	// ListContainerStats returns stats of all running containers.
	ListContainerStats(ctx context.Context, filter *runtimeapi.ContainerStatsFilter) ([]*runtimeapi.ContainerStats, error)
	// PodSandboxStats returns stats of the pod. If the pod does not
	// exist, the call returns an error.
	PodSandboxStats(ctx context.Context, podSandboxID string) (*runtimeapi.PodSandboxStats, error)
	// ListPodSandboxStats returns stats of all running pods.
	ListPodSandboxStats(ctx context.Context, filter *runtimeapi.PodSandboxStatsFilter) ([]*runtimeapi.PodSandboxStats, error)
	// ListMetricDescriptors gets the descriptors for the metrics that will be returned in ListPodSandboxMetrics.
	ListMetricDescriptors(ctx context.Context) ([]*runtimeapi.MetricDescriptor, error)
	// ListPodSandboxMetrics returns metrics of all running pods.
	ListPodSandboxMetrics(ctx context.Context) ([]*runtimeapi.PodSandboxMetrics, error)
}

// RuntimeService interface should be implemented by a container runtime.
// The methods should be thread-safe.
type RuntimeService interface {
	RuntimeVersioner
	ContainerManager
	PodSandboxManager
	ContainerStatsManager

	// UpdateRuntimeConfig updates runtime configuration if specified
	UpdateRuntimeConfig(ctx context.Context, runtimeConfig *runtimeapi.RuntimeConfig) error
	// Status returns the status of the runtime.
	Status(ctx context.Context, verbose bool) (*runtimeapi.StatusResponse, error)
	// RuntimeConfig returns the configuration information of the runtime.
	RuntimeConfig(ctx context.Context) (*runtimeapi.RuntimeConfigResponse, error)
}

// ImageManagerService interface should be implemented by a container image
// manager.
// The methods should be thread-safe.
type ImageManagerService interface {
	// ListImages lists the existing images.
	ListImages(ctx context.Context, filter *runtimeapi.ImageFilter) ([]*runtimeapi.Image, error)
	// ImageStatus returns the status of the image.
	ImageStatus(ctx context.Context, image *runtimeapi.ImageSpec, verbose bool) (*runtimeapi.ImageStatusResponse, error)
	// PullImage pulls an image with the authentication config.
	PullImage(ctx context.Context, image *runtimeapi.ImageSpec, auth *runtimeapi.AuthConfig, podSandboxConfig *runtimeapi.PodSandboxConfig) (string, error)
	// RemoveImage removes the image.
	RemoveImage(ctx context.Context, image *runtimeapi.ImageSpec) error
	// ImageFsInfo returns information of the filesystem(s) used to store the read-only layers and the writeable layer.
	ImageFsInfo(ctx context.Context) (*runtimeapi.ImageFsInfoResponse, error)
}
