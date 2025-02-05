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
	"time"

	internalapi "k8s.io/cri-api/pkg/apis"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
	"k8s.io/kubernetes/pkg/kubelet/metrics"
)

// instrumentedRuntimeService wraps the RuntimeService and records the operations
// and errors metrics.
type instrumentedRuntimeService struct {
	service internalapi.RuntimeService
}

// Creates an instrumented RuntimeInterface from an existing RuntimeService.
func newInstrumentedRuntimeService(service internalapi.RuntimeService) internalapi.RuntimeService {
	return &instrumentedRuntimeService{service: service}
}

// instrumentedImageManagerService wraps the ImageManagerService and records the operations
// and errors metrics.
type instrumentedImageManagerService struct {
	service internalapi.ImageManagerService
}

// Creates an instrumented ImageManagerService from an existing ImageManagerService.
func newInstrumentedImageManagerService(service internalapi.ImageManagerService) internalapi.ImageManagerService {
	return &instrumentedImageManagerService{service: service}
}

// recordOperation records the duration of the operation.
func recordOperation(operation string, start time.Time) {
	metrics.RuntimeOperations.WithLabelValues(operation).Inc()
	metrics.RuntimeOperationsDuration.WithLabelValues(operation).Observe(metrics.SinceInSeconds(start))
}

// recordError records error for metric if an error occurred.
func recordError(operation string, err error) {
	if err != nil {
		metrics.RuntimeOperationsErrors.WithLabelValues(operation).Inc()
	}
}

func (in instrumentedRuntimeService) Version(ctx context.Context, apiVersion string) (*runtimeapi.VersionResponse, error) {
	const operation = "version"
	defer recordOperation(operation, time.Now())

	out, err := in.service.Version(ctx, apiVersion)
	recordError(operation, err)
	return out, err
}

func (in instrumentedRuntimeService) Status(ctx context.Context, verbose bool) (*runtimeapi.StatusResponse, error) {
	const operation = "status"
	defer recordOperation(operation, time.Now())

	out, err := in.service.Status(ctx, verbose)
	recordError(operation, err)
	return out, err
}

func (in instrumentedRuntimeService) CreateContainer(ctx context.Context, podSandboxID string, config *runtimeapi.ContainerConfig, sandboxConfig *runtimeapi.PodSandboxConfig) (string, error) {
	const operation = "create_container"
	defer recordOperation(operation, time.Now())

	out, err := in.service.CreateContainer(ctx, podSandboxID, config, sandboxConfig)
	recordError(operation, err)
	return out, err
}

func (in instrumentedRuntimeService) StartContainer(ctx context.Context, containerID string) error {
	const operation = "start_container"
	defer recordOperation(operation, time.Now())

	err := in.service.StartContainer(ctx, containerID)
	recordError(operation, err)
	return err
}

func (in instrumentedRuntimeService) StopContainer(ctx context.Context, containerID string, timeout int64) error {
	const operation = "stop_container"
	defer recordOperation(operation, time.Now())

	err := in.service.StopContainer(ctx, containerID, timeout)
	recordError(operation, err)
	return err
}

func (in instrumentedRuntimeService) RemoveContainer(ctx context.Context, containerID string) error {
	const operation = "remove_container"
	defer recordOperation(operation, time.Now())

	err := in.service.RemoveContainer(ctx, containerID)
	recordError(operation, err)
	return err
}

func (in instrumentedRuntimeService) ListContainers(ctx context.Context, filter *runtimeapi.ContainerFilter) ([]*runtimeapi.Container, error) {
	const operation = "list_containers"
	defer recordOperation(operation, time.Now())

	out, err := in.service.ListContainers(ctx, filter)
	recordError(operation, err)
	return out, err
}

func (in instrumentedRuntimeService) ContainerStatus(ctx context.Context, containerID string, verbose bool) (*runtimeapi.ContainerStatusResponse, error) {
	const operation = "container_status"
	defer recordOperation(operation, time.Now())

	out, err := in.service.ContainerStatus(ctx, containerID, verbose)
	recordError(operation, err)
	return out, err
}

func (in instrumentedRuntimeService) UpdateContainerResources(ctx context.Context, containerID string, resources *runtimeapi.ContainerResources) error {
	const operation = "update_container"
	defer recordOperation(operation, time.Now())

	err := in.service.UpdateContainerResources(ctx, containerID, resources)
	recordError(operation, err)
	return err
}

func (in instrumentedRuntimeService) ReopenContainerLog(ctx context.Context, containerID string) error {
	const operation = "reopen_container_log"
	defer recordOperation(operation, time.Now())

	err := in.service.ReopenContainerLog(ctx, containerID)
	recordError(operation, err)
	return err
}

func (in instrumentedRuntimeService) ExecSync(ctx context.Context, containerID string, cmd []string, timeout time.Duration) ([]byte, []byte, error) {
	const operation = "exec_sync"
	defer recordOperation(operation, time.Now())

	stdout, stderr, err := in.service.ExecSync(ctx, containerID, cmd, timeout)
	recordError(operation, err)
	return stdout, stderr, err
}

func (in instrumentedRuntimeService) Exec(ctx context.Context, req *runtimeapi.ExecRequest) (*runtimeapi.ExecResponse, error) {
	const operation = "exec"
	defer recordOperation(operation, time.Now())

	resp, err := in.service.Exec(ctx, req)
	recordError(operation, err)
	return resp, err
}

func (in instrumentedRuntimeService) Attach(ctx context.Context, req *runtimeapi.AttachRequest) (*runtimeapi.AttachResponse, error) {
	const operation = "attach"
	defer recordOperation(operation, time.Now())

	resp, err := in.service.Attach(ctx, req)
	recordError(operation, err)
	return resp, err
}

func (in instrumentedRuntimeService) RunPodSandbox(ctx context.Context, config *runtimeapi.PodSandboxConfig, runtimeHandler string) (string, error) {
	const operation = "run_podsandbox"
	startTime := time.Now()
	defer recordOperation(operation, startTime)
	defer metrics.RunPodSandboxDuration.WithLabelValues(runtimeHandler).Observe(metrics.SinceInSeconds(startTime))

	out, err := in.service.RunPodSandbox(ctx, config, runtimeHandler)
	recordError(operation, err)
	if err != nil {
		metrics.RunPodSandboxErrors.WithLabelValues(runtimeHandler).Inc()
	}
	return out, err
}

func (in instrumentedRuntimeService) StopPodSandbox(ctx context.Context, podSandboxID string) error {
	const operation = "stop_podsandbox"
	defer recordOperation(operation, time.Now())

	err := in.service.StopPodSandbox(ctx, podSandboxID)
	recordError(operation, err)
	return err
}

func (in instrumentedRuntimeService) RemovePodSandbox(ctx context.Context, podSandboxID string) error {
	const operation = "remove_podsandbox"
	defer recordOperation(operation, time.Now())

	err := in.service.RemovePodSandbox(ctx, podSandboxID)
	recordError(operation, err)
	return err
}

func (in instrumentedRuntimeService) PodSandboxStatus(ctx context.Context, podSandboxID string, verbose bool) (*runtimeapi.PodSandboxStatusResponse, error) {
	const operation = "podsandbox_status"
	defer recordOperation(operation, time.Now())

	out, err := in.service.PodSandboxStatus(ctx, podSandboxID, verbose)
	recordError(operation, err)
	return out, err
}

func (in instrumentedRuntimeService) ListPodSandbox(ctx context.Context, filter *runtimeapi.PodSandboxFilter) ([]*runtimeapi.PodSandbox, error) {
	const operation = "list_podsandbox"
	defer recordOperation(operation, time.Now())

	out, err := in.service.ListPodSandbox(ctx, filter)
	recordError(operation, err)
	return out, err
}

func (in instrumentedRuntimeService) ContainerStats(ctx context.Context, containerID string) (*runtimeapi.ContainerStats, error) {
	const operation = "container_stats"
	defer recordOperation(operation, time.Now())

	out, err := in.service.ContainerStats(ctx, containerID)
	recordError(operation, err)
	return out, err
}

func (in instrumentedRuntimeService) ListContainerStats(ctx context.Context, filter *runtimeapi.ContainerStatsFilter) ([]*runtimeapi.ContainerStats, error) {
	const operation = "list_container_stats"
	defer recordOperation(operation, time.Now())

	out, err := in.service.ListContainerStats(ctx, filter)
	recordError(operation, err)
	return out, err
}

func (in instrumentedRuntimeService) PodSandboxStats(ctx context.Context, podSandboxID string) (*runtimeapi.PodSandboxStats, error) {
	const operation = "podsandbox_stats"
	defer recordOperation(operation, time.Now())

	out, err := in.service.PodSandboxStats(ctx, podSandboxID)
	recordError(operation, err)
	return out, err
}

func (in instrumentedRuntimeService) ListPodSandboxStats(ctx context.Context, filter *runtimeapi.PodSandboxStatsFilter) ([]*runtimeapi.PodSandboxStats, error) {
	const operation = "list_podsandbox_stats"
	defer recordOperation(operation, time.Now())

	out, err := in.service.ListPodSandboxStats(ctx, filter)
	recordError(operation, err)
	return out, err
}

func (in instrumentedRuntimeService) PortForward(ctx context.Context, req *runtimeapi.PortForwardRequest) (*runtimeapi.PortForwardResponse, error) {
	const operation = "port_forward"
	defer recordOperation(operation, time.Now())

	resp, err := in.service.PortForward(ctx, req)
	recordError(operation, err)
	return resp, err
}

func (in instrumentedRuntimeService) UpdatePodSandboxResources(ctx context.Context, req *runtimeapi.UpdatePodSandboxResourcesRequest) (*runtimeapi.UpdatePodSandboxResourcesResponse, error) {
	const operation = "update_podsandbox"
	defer recordOperation(operation, time.Now())

	resp, err := in.service.UpdatePodSandboxResources(ctx, req)
	recordError(operation, err)
	return resp, err
}

func (in instrumentedRuntimeService) UpdateRuntimeConfig(ctx context.Context, runtimeConfig *runtimeapi.RuntimeConfig) error {
	const operation = "update_runtime_config"
	defer recordOperation(operation, time.Now())

	err := in.service.UpdateRuntimeConfig(ctx, runtimeConfig)
	recordError(operation, err)
	return err
}

func (in instrumentedImageManagerService) ListImages(ctx context.Context, filter *runtimeapi.ImageFilter) ([]*runtimeapi.Image, error) {
	const operation = "list_images"
	defer recordOperation(operation, time.Now())

	out, err := in.service.ListImages(ctx, filter)
	recordError(operation, err)
	return out, err
}

func (in instrumentedImageManagerService) ImageStatus(ctx context.Context, image *runtimeapi.ImageSpec, verbose bool) (*runtimeapi.ImageStatusResponse, error) {
	const operation = "image_status"
	defer recordOperation(operation, time.Now())

	out, err := in.service.ImageStatus(ctx, image, verbose)
	recordError(operation, err)
	return out, err
}

func (in instrumentedImageManagerService) PullImage(ctx context.Context, image *runtimeapi.ImageSpec, auth *runtimeapi.AuthConfig, podSandboxConfig *runtimeapi.PodSandboxConfig) (string, error) {
	const operation = "pull_image"
	defer recordOperation(operation, time.Now())

	imageRef, err := in.service.PullImage(ctx, image, auth, podSandboxConfig)
	recordError(operation, err)
	return imageRef, err
}

func (in instrumentedImageManagerService) RemoveImage(ctx context.Context, image *runtimeapi.ImageSpec) error {
	const operation = "remove_image"
	defer recordOperation(operation, time.Now())

	err := in.service.RemoveImage(ctx, image)
	recordError(operation, err)
	return err
}

func (in instrumentedImageManagerService) ImageFsInfo(ctx context.Context) (*runtimeapi.ImageFsInfoResponse, error) {
	const operation = "image_fs_info"
	defer recordOperation(operation, time.Now())

	fsInfo, err := in.service.ImageFsInfo(ctx)
	recordError(operation, err)
	return fsInfo, nil
}

func (in instrumentedRuntimeService) CheckpointContainer(ctx context.Context, options *runtimeapi.CheckpointContainerRequest) error {
	const operation = "checkpoint_container"
	defer recordOperation(operation, time.Now())

	err := in.service.CheckpointContainer(ctx, options)
	recordError(operation, err)
	return err
}

func (in instrumentedRuntimeService) GetContainerEvents(ctx context.Context, containerEventsCh chan *runtimeapi.ContainerEventResponse, connectionEstablishedCallback func(runtimeapi.RuntimeService_GetContainerEventsClient)) error {
	const operation = "get_container_events"
	defer recordOperation(operation, time.Now())

	err := in.service.GetContainerEvents(ctx, containerEventsCh, connectionEstablishedCallback)
	recordError(operation, err)
	return err
}

func (in instrumentedRuntimeService) ListMetricDescriptors(ctx context.Context) ([]*runtimeapi.MetricDescriptor, error) {
	const operation = "list_metric_descriptors"
	defer recordOperation(operation, time.Now())

	out, err := in.service.ListMetricDescriptors(ctx)
	recordError(operation, err)
	return out, err
}

func (in instrumentedRuntimeService) ListPodSandboxMetrics(ctx context.Context) ([]*runtimeapi.PodSandboxMetrics, error) {
	const operation = "list_podsandbox_metrics"
	defer recordOperation(operation, time.Now())

	out, err := in.service.ListPodSandboxMetrics(ctx)
	recordError(operation, err)
	return out, err
}

func (in instrumentedRuntimeService) RuntimeConfig(ctx context.Context) (*runtimeapi.RuntimeConfigResponse, error) {
	const operation = "runtime_config"
	defer recordOperation(operation, time.Now())

	out, err := in.service.RuntimeConfig(ctx)
	recordError(operation, err)
	return out, err
}
