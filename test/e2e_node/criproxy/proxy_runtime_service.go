/*
Copyright 2024 The Kubernetes Authors.

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

package criproxy

import (
	"context"
	"errors"
	"fmt"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	internalapi "k8s.io/cri-api/pkg/apis"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
	"k8s.io/cri-client/pkg/util"
	"k8s.io/kubernetes/test/e2e/framework"
	utilexec "k8s.io/utils/exec"
)

const (
	Version                   = "Version"
	RunPodSandbox             = "RunPodSandbox"
	StopPodSandbox            = "StopPodSandbox"
	RemovePodSandbox          = "RemovePodSandbox"
	PodSandboxStatus          = "PodSandboxStatus"
	ListPodSandbox            = "ListPodSandbox"
	CreateContainer           = "CreateContainer"
	StartContainer            = "StartContainer"
	StopContainer             = "StopContainer"
	RemoveContainer           = "RemoveContainer"
	ListContainers            = "ListContainers"
	ContainerStatus           = "ContainerStatus"
	UpdateContainerResources  = "UpdateContainerResources"
	ReopenContainerLog        = "ReopenContainerLog"
	ExecSync                  = "ExecSync"
	Exec                      = "Exec"
	Attach                    = "Attach"
	PortForward               = "PortForward"
	ContainerStats            = "ContainerStats"
	ListContainerStats        = "ListContainerStats"
	PodSandboxStats           = "PodSandboxStats"
	ListPodSandboxStats       = "ListPodSandboxStats"
	UpdateRuntimeConfig       = "UpdateRuntimeConfig"
	Status                    = "Status"
	CheckpointContainer       = "CheckpointContainer"
	GetContainerEvents        = "GetContainerEvents"
	ListMetricDescriptors     = "ListMetricDescriptors"
	ListPodSandboxMetrics     = "ListPodSandboxMetrics"
	RuntimeConfig             = "RuntimeConfig"
	UpdatePodSandboxResources = "UpdatePodSandboxResources"
)

// AddInjector inject the error or delay to the next call to the RuntimeService.
func (p *RemoteRuntime) AddInjector(injector func(string) error) {
	p.injectors = append(p.injectors, injector)
}

// ResetInjectors resets all registered injectors.
func (p *RemoteRuntime) ResetInjectors() {
	p.injectors = []func(string) error{}
}

func (p *RemoteRuntime) runInjectors(apiName string) error {
	for _, injector := range p.injectors {
		if err := injector(apiName); err != nil {
			return err
		}
	}
	return nil
}

// RemoteRuntime represents a proxy for remote container runtime.
type RemoteRuntime struct {
	server         *grpc.Server
	injectors      []func(string) error
	runtimeService internalapi.RuntimeService
	imageService   internalapi.ImageManagerService
}

// NewRemoteRuntimeProxy creates a new RemoteRuntime.
func NewRemoteRuntimeProxy(runtimeService internalapi.RuntimeService, imageService internalapi.ImageManagerService) *RemoteRuntime {
	p := &RemoteRuntime{
		server:         grpc.NewServer(),
		runtimeService: runtimeService,
		imageService:   imageService,
	}
	runtimeapi.RegisterRuntimeServiceServer(p.server, p)
	runtimeapi.RegisterImageServiceServer(p.server, p)

	return p
}

// Start starts the remote runtime proxy.
func (p *RemoteRuntime) Start(endpoint string) error {
	l, err := util.CreateListener(endpoint)
	if err != nil {
		return fmt.Errorf("failed to listen on %q: %w", endpoint, err)
	}

	go func() {
		if err := p.server.Serve(l); err != nil {
			framework.Failf("Failed to start cri proxy : %v", err)
		}
	}()
	return nil
}

// Stop stops the fake remote runtime proxy.
func (p *RemoteRuntime) Stop() {
	p.server.Stop()
}

// Version returns the runtime name, runtime version, and runtime API version.
func (p *RemoteRuntime) Version(ctx context.Context, req *runtimeapi.VersionRequest) (*runtimeapi.VersionResponse, error) {
	if err := p.runInjectors(Version); err != nil {
		return nil, err
	}
	return p.runtimeService.Version(ctx, req.Version)
}

// RunPodSandbox creates and starts a pod-level sandbox. Runtimes must ensure
// the sandbox is in the ready state on success.
func (p *RemoteRuntime) RunPodSandbox(ctx context.Context, req *runtimeapi.RunPodSandboxRequest) (*runtimeapi.RunPodSandboxResponse, error) {
	if err := p.runInjectors(RunPodSandbox); err != nil {
		return nil, err
	}

	sandboxID, err := p.runtimeService.RunPodSandbox(ctx, req.Config, req.RuntimeHandler)
	if err != nil {
		return nil, err
	}
	return &runtimeapi.RunPodSandboxResponse{PodSandboxId: sandboxID}, nil
}

// StopPodSandbox stops any running process that is part of the sandbox and
// reclaims network resources (e.g., IP addresses) allocated to the sandbox.
// If there are any running containers in the sandbox, they must be forcibly
// terminated.
func (p *RemoteRuntime) StopPodSandbox(ctx context.Context, req *runtimeapi.StopPodSandboxRequest) (*runtimeapi.StopPodSandboxResponse, error) {
	if err := p.runInjectors(StopPodSandbox); err != nil {
		return nil, err
	}

	err := p.runtimeService.StopPodSandbox(ctx, req.PodSandboxId)
	if err != nil {
		return nil, err
	}
	return &runtimeapi.StopPodSandboxResponse{}, nil
}

// RemovePodSandbox removes the sandbox. If there are any running containers
// in the sandbox, they must be forcibly terminated and removed.
// This call is idempotent, and must not return an error if the sandbox has
// already been removed.
func (p *RemoteRuntime) RemovePodSandbox(ctx context.Context, req *runtimeapi.RemovePodSandboxRequest) (*runtimeapi.RemovePodSandboxResponse, error) {
	if err := p.runInjectors(RemovePodSandbox); err != nil {
		return nil, err
	}
	err := p.runtimeService.RemovePodSandbox(ctx, req.PodSandboxId)
	if err != nil {
		return nil, err
	}

	return &runtimeapi.RemovePodSandboxResponse{}, nil
}

// PodSandboxStatus returns the status of the PodSandbox. If the PodSandbox is not
// present, returns an error.
func (p *RemoteRuntime) PodSandboxStatus(ctx context.Context, req *runtimeapi.PodSandboxStatusRequest) (*runtimeapi.PodSandboxStatusResponse, error) {
	if err := p.runInjectors(PodSandboxStatus); err != nil {
		return nil, err
	}

	resp, err := p.runtimeService.PodSandboxStatus(ctx, req.PodSandboxId, false)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// ListPodSandbox returns a list of PodSandboxes.
func (p *RemoteRuntime) ListPodSandbox(ctx context.Context, req *runtimeapi.ListPodSandboxRequest) (*runtimeapi.ListPodSandboxResponse, error) {
	if err := p.runInjectors(ListPodSandbox); err != nil {
		return nil, err
	}

	items, err := p.runtimeService.ListPodSandbox(ctx, req.Filter)
	if err != nil {
		return nil, err
	}
	return &runtimeapi.ListPodSandboxResponse{Items: items}, nil
}

// CreateContainer creates a new container in specified PodSandbox
func (p *RemoteRuntime) CreateContainer(ctx context.Context, req *runtimeapi.CreateContainerRequest) (*runtimeapi.CreateContainerResponse, error) {
	if err := p.runInjectors(CreateContainer); err != nil {
		return nil, err
	}

	containerID, err := p.runtimeService.CreateContainer(ctx, req.PodSandboxId, req.Config, req.SandboxConfig)
	if err != nil {
		return nil, err
	}
	return &runtimeapi.CreateContainerResponse{ContainerId: containerID}, nil
}

// StartContainer starts the container.
func (p *RemoteRuntime) StartContainer(ctx context.Context, req *runtimeapi.StartContainerRequest) (*runtimeapi.StartContainerResponse, error) {
	if err := p.runInjectors(StartContainer); err != nil {
		return nil, err
	}

	err := p.runtimeService.StartContainer(ctx, req.ContainerId)
	if err != nil {
		return nil, err
	}
	return &runtimeapi.StartContainerResponse{}, nil
}

// StopContainer stops a running container with a grace period (i.e., timeout).
// This call is idempotent, and must not return an error if the container has
// already been stopped.
func (p *RemoteRuntime) StopContainer(ctx context.Context, req *runtimeapi.StopContainerRequest) (*runtimeapi.StopContainerResponse, error) {
	if err := p.runInjectors(StopContainer); err != nil {
		return nil, err
	}

	err := p.runtimeService.StopContainer(ctx, req.ContainerId, req.Timeout)
	if err != nil {
		return nil, err
	}
	return &runtimeapi.StopContainerResponse{}, nil
}

// RemoveContainer removes the container. If the container is running, the
// container must be forcibly removed.
// This call is idempotent, and must not return an error if the container has
// already been removed.
func (p *RemoteRuntime) RemoveContainer(ctx context.Context, req *runtimeapi.RemoveContainerRequest) (*runtimeapi.RemoveContainerResponse, error) {
	if err := p.runInjectors(RemoveContainer); err != nil {
		return nil, err
	}

	err := p.runtimeService.RemoveContainer(ctx, req.ContainerId)
	if err != nil {
		return nil, err
	}
	return &runtimeapi.RemoveContainerResponse{}, nil
}

// ListContainers lists all containers by filters.
func (p *RemoteRuntime) ListContainers(ctx context.Context, req *runtimeapi.ListContainersRequest) (*runtimeapi.ListContainersResponse, error) {
	if err := p.runInjectors(ListContainers); err != nil {
		return nil, err
	}

	items, err := p.runtimeService.ListContainers(ctx, req.Filter)
	if err != nil {
		return nil, err
	}
	return &runtimeapi.ListContainersResponse{Containers: items}, nil
}

// ContainerStatus returns status of the container. If the container is not
// present, returns an error.
func (p *RemoteRuntime) ContainerStatus(ctx context.Context, req *runtimeapi.ContainerStatusRequest) (*runtimeapi.ContainerStatusResponse, error) {
	if err := p.runInjectors(ContainerStatus); err != nil {
		return nil, err
	}

	resp, err := p.runtimeService.ContainerStatus(ctx, req.ContainerId, false)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// ExecSync runs a command in a container synchronously.
func (p *RemoteRuntime) ExecSync(ctx context.Context, req *runtimeapi.ExecSyncRequest) (*runtimeapi.ExecSyncResponse, error) {
	if err := p.runInjectors(ExecSync); err != nil {
		return nil, err
	}

	var exitCode int32
	stdout, stderr, err := p.runtimeService.ExecSync(ctx, req.ContainerId, req.Cmd, time.Duration(req.Timeout)*time.Second)
	if err != nil {
		var exitError utilexec.ExitError
		ok := errors.As(err, &exitError)
		if !ok {
			return nil, err
		}
		exitCode = int32(exitError.ExitStatus())
	}
	return &runtimeapi.ExecSyncResponse{
		Stdout:   stdout,
		Stderr:   stderr,
		ExitCode: exitCode,
	}, nil
}

// Exec prepares a streaming endpoint to execute a command in the container.
func (p *RemoteRuntime) Exec(ctx context.Context, req *runtimeapi.ExecRequest) (*runtimeapi.ExecResponse, error) {
	if err := p.runInjectors(Exec); err != nil {
		return nil, err
	}

	return p.runtimeService.Exec(ctx, req)
}

// Attach prepares a streaming endpoint to attach to a running container.
func (p *RemoteRuntime) Attach(ctx context.Context, req *runtimeapi.AttachRequest) (*runtimeapi.AttachResponse, error) {
	if err := p.runInjectors(Attach); err != nil {
		return nil, err
	}

	return p.runtimeService.Attach(ctx, req)
}

// PortForward prepares a streaming endpoint to forward ports from a PodSandbox.
func (p *RemoteRuntime) PortForward(ctx context.Context, req *runtimeapi.PortForwardRequest) (*runtimeapi.PortForwardResponse, error) {
	if err := p.runInjectors(PortForward); err != nil {
		return nil, err
	}

	return p.runtimeService.PortForward(ctx, req)
}

// ContainerStats returns stats of the container. If the container does not
// exist, the call returns an error.
func (p *RemoteRuntime) ContainerStats(ctx context.Context, req *runtimeapi.ContainerStatsRequest) (*runtimeapi.ContainerStatsResponse, error) {
	if err := p.runInjectors(ContainerStats); err != nil {
		return nil, err
	}

	stats, err := p.runtimeService.ContainerStats(ctx, req.ContainerId)
	if err != nil {
		return nil, err
	}
	return &runtimeapi.ContainerStatsResponse{Stats: stats}, nil
}

// ListContainerStats returns stats of all running containers.
func (p *RemoteRuntime) ListContainerStats(ctx context.Context, req *runtimeapi.ListContainerStatsRequest) (*runtimeapi.ListContainerStatsResponse, error) {
	if err := p.runInjectors(ListContainerStats); err != nil {
		return nil, err
	}

	stats, err := p.runtimeService.ListContainerStats(ctx, req.Filter)
	if err != nil {
		return nil, err
	}
	return &runtimeapi.ListContainerStatsResponse{Stats: stats}, nil
}

// PodSandboxStats returns stats of the pod. If the pod does not
// exist, the call returns an error.
func (p *RemoteRuntime) PodSandboxStats(ctx context.Context, req *runtimeapi.PodSandboxStatsRequest) (*runtimeapi.PodSandboxStatsResponse, error) {
	if err := p.runInjectors(PodSandboxStats); err != nil {
		return nil, err
	}

	stats, err := p.runtimeService.PodSandboxStats(ctx, req.PodSandboxId)
	if err != nil {
		return nil, err
	}
	return &runtimeapi.PodSandboxStatsResponse{Stats: stats}, nil
}

// ListPodSandboxStats returns stats of all running pods.
func (p *RemoteRuntime) ListPodSandboxStats(ctx context.Context, req *runtimeapi.ListPodSandboxStatsRequest) (*runtimeapi.ListPodSandboxStatsResponse, error) {
	if err := p.runInjectors(ListPodSandboxStats); err != nil {
		return nil, err
	}

	stats, err := p.runtimeService.ListPodSandboxStats(ctx, req.Filter)
	if err != nil {
		return nil, err
	}
	return &runtimeapi.ListPodSandboxStatsResponse{Stats: stats}, nil
}

// UpdateRuntimeConfig updates the runtime configuration based on the given request.
func (p *RemoteRuntime) UpdateRuntimeConfig(ctx context.Context, req *runtimeapi.UpdateRuntimeConfigRequest) (*runtimeapi.UpdateRuntimeConfigResponse, error) {
	if err := p.runInjectors(UpdateRuntimeConfig); err != nil {
		return nil, err
	}

	err := p.runtimeService.UpdateRuntimeConfig(ctx, req.RuntimeConfig)
	if err != nil {
		return nil, err
	}
	return &runtimeapi.UpdateRuntimeConfigResponse{}, nil
}

// UpdatePodSandboxResources synchronously updates the PodSandboxConfig.
func (p *RemoteRuntime) UpdatePodSandboxResources(ctx context.Context, req *runtimeapi.UpdatePodSandboxResourcesRequest) (*runtimeapi.UpdatePodSandboxResourcesResponse, error) {
	if err := p.runInjectors(UpdatePodSandboxResources); err != nil {
		return nil, err
	}

	return p.runtimeService.UpdatePodSandboxResources(ctx, req)
}

// Status returns the status of the runtime.
func (p *RemoteRuntime) Status(ctx context.Context, req *runtimeapi.StatusRequest) (*runtimeapi.StatusResponse, error) {
	if err := p.runInjectors(Status); err != nil {
		return nil, err
	}

	resp, err := p.runtimeService.Status(ctx, false)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// UpdateContainerResources updates ContainerConfig of the container.
func (p *RemoteRuntime) UpdateContainerResources(ctx context.Context, req *runtimeapi.UpdateContainerResourcesRequest) (*runtimeapi.UpdateContainerResourcesResponse, error) {
	if err := p.runInjectors(UpdateContainerResources); err != nil {
		return nil, err
	}

	err := p.runtimeService.UpdateContainerResources(ctx, req.ContainerId, &runtimeapi.ContainerResources{Linux: req.Linux})
	if err != nil {
		return nil, err
	}
	return &runtimeapi.UpdateContainerResourcesResponse{}, nil
}

// ReopenContainerLog reopens the container log file.
func (p *RemoteRuntime) ReopenContainerLog(ctx context.Context, req *runtimeapi.ReopenContainerLogRequest) (*runtimeapi.ReopenContainerLogResponse, error) {
	if err := p.runInjectors(ReopenContainerLog); err != nil {
		return nil, err
	}

	err := p.runtimeService.ReopenContainerLog(ctx, req.ContainerId)
	if err != nil {
		return nil, err
	}
	return &runtimeapi.ReopenContainerLogResponse{}, nil
}

// CheckpointContainer checkpoints the given container.
func (p *RemoteRuntime) CheckpointContainer(ctx context.Context, req *runtimeapi.CheckpointContainerRequest) (*runtimeapi.CheckpointContainerResponse, error) {
	if err := p.runInjectors(CheckpointContainer); err != nil {
		return nil, err
	}

	err := p.runtimeService.CheckpointContainer(ctx, req)
	if err != nil {
		return nil, err
	}
	return &runtimeapi.CheckpointContainerResponse{}, nil
}

func (p *RemoteRuntime) GetContainerEvents(req *runtimeapi.GetEventsRequest, ces runtimeapi.RuntimeService_GetContainerEventsServer) error {
	if err := p.runInjectors(GetContainerEvents); err != nil {
		return err
	}

	// Capacity of the channel for receiving pod lifecycle events. This number
	// is a bit arbitrary and may be adjusted in the future.
	plegChannelCapacity := 1000
	containerEventsResponseCh := make(chan *runtimeapi.ContainerEventResponse, plegChannelCapacity)
	defer close(containerEventsResponseCh)

	if err := p.runtimeService.GetContainerEvents(context.Background(), containerEventsResponseCh, nil); err != nil {
		return err
	}

	for event := range containerEventsResponseCh {
		if err := ces.Send(event); err != nil {
			return status.Errorf(codes.Unknown, "Failed to send event: %v", err)
		}
	}

	return nil
}

// ListMetricDescriptors gets the descriptors for the metrics that will be returned in ListPodSandboxMetrics.
func (p *RemoteRuntime) ListMetricDescriptors(ctx context.Context, req *runtimeapi.ListMetricDescriptorsRequest) (*runtimeapi.ListMetricDescriptorsResponse, error) {
	if err := p.runInjectors(ListMetricDescriptors); err != nil {
		return nil, err
	}

	descs, err := p.runtimeService.ListMetricDescriptors(ctx)
	if err != nil {
		return nil, err
	}
	return &runtimeapi.ListMetricDescriptorsResponse{Descriptors: descs}, nil
}

// ListPodSandboxMetrics retrieves the metrics for all pod sandboxes.
func (p *RemoteRuntime) ListPodSandboxMetrics(ctx context.Context, req *runtimeapi.ListPodSandboxMetricsRequest) (*runtimeapi.ListPodSandboxMetricsResponse, error) {
	if err := p.runInjectors(ListPodSandboxMetrics); err != nil {
		return nil, err
	}

	podMetrics, err := p.runtimeService.ListPodSandboxMetrics(ctx)
	if err != nil {
		return nil, err
	}
	return &runtimeapi.ListPodSandboxMetricsResponse{PodMetrics: podMetrics}, nil
}

// RuntimeConfig returns the configuration information of the runtime.
func (p *RemoteRuntime) RuntimeConfig(ctx context.Context, req *runtimeapi.RuntimeConfigRequest) (*runtimeapi.RuntimeConfigResponse, error) {
	if err := p.runInjectors(RuntimeConfig); err != nil {
		return nil, err
	}

	resp, err := p.runtimeService.RuntimeConfig(ctx)
	if err != nil {
		return nil, err
	}
	return resp, nil
}
