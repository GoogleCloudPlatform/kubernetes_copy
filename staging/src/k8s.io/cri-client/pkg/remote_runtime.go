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
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"k8s.io/component-base/logs/logreduction"
	tracing "k8s.io/component-base/tracing"
	internalapi "k8s.io/cri-api/pkg/apis"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
	"k8s.io/klog/v2"
	utilexec "k8s.io/utils/exec"

	"k8s.io/cri-client/pkg/internal"
	"k8s.io/cri-client/pkg/util"
)

// remoteRuntimeService is a gRPC implementation of internalapi.RuntimeService.
type remoteRuntimeService struct {
	timeout       time.Duration
	runtimeClient runtimeapi.RuntimeServiceClient
	// Cache last per-container error message to reduce log spam
	logReduction *logreduction.LogReduction
	logger       *klog.Logger
}

const (
	// How frequently to report identical errors
	identicalErrorDelay = 1 * time.Minute

	// connection parameters
	maxBackoffDelay      = 3 * time.Second
	baseBackoffDelay     = 100 * time.Millisecond
	minConnectionTimeout = 5 * time.Second
)

// CRIVersion is the type for valid Container Runtime Interface (CRI) API
// versions.
type CRIVersion string

var (
	// ErrContainerStatusNil indicates that the returned container status is nil.
	ErrContainerStatusNil = errors.New("container status is nil")

	// ErrCommandTimedOut indicates that the exec sync command timed.
	ErrCommandTimedOut = errors.New("command timed out")
)

const (
	// CRIVersionV1 references the v1 CRI API.
	CRIVersionV1 CRIVersion = "v1"
)

// NewRemoteRuntimeService creates a new internalapi.RuntimeService.
func NewRemoteRuntimeService(endpoint string, connectionTimeout time.Duration, tp trace.TracerProvider, logger *klog.Logger) (internalapi.RuntimeService, error) {
	internal.Log(logger, 3, "Connecting to runtime service", "endpoint", endpoint)
	addr, dialer, err := util.GetAddressAndDialer(endpoint)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), connectionTimeout)
	defer cancel()

	var dialOpts []grpc.DialOption
	dialOpts = append(dialOpts,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithAuthority("localhost"),
		grpc.WithContextDialer(dialer),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(maxMsgSize)))
	if tp != nil {
		tracingOpts := []otelgrpc.Option{
			otelgrpc.WithMessageEvents(otelgrpc.ReceivedEvents, otelgrpc.SentEvents),
			otelgrpc.WithPropagators(tracing.Propagators()),
			otelgrpc.WithTracerProvider(tp),
		}
		// Even if there is no TracerProvider, the otelgrpc still handles context propagation.
		// See https://github.com/open-telemetry/opentelemetry-go/tree/main/example/passthrough
		dialOpts = append(dialOpts,
			grpc.WithUnaryInterceptor(otelgrpc.UnaryClientInterceptor(tracingOpts...)),
			grpc.WithStreamInterceptor(otelgrpc.StreamClientInterceptor(tracingOpts...)))
	}

	connParams := grpc.ConnectParams{
		Backoff: backoff.DefaultConfig,
	}
	connParams.MinConnectTimeout = minConnectionTimeout
	connParams.Backoff.BaseDelay = baseBackoffDelay
	connParams.Backoff.MaxDelay = maxBackoffDelay
	dialOpts = append(dialOpts,
		grpc.WithConnectParams(connParams),
	)

	conn, err := grpc.DialContext(ctx, addr, dialOpts...)
	if err != nil {
		internal.LogErr(logger, err, "Connect remote runtime failed", "address", addr)
		return nil, err
	}

	service := &remoteRuntimeService{
		timeout:      connectionTimeout,
		logReduction: logreduction.NewLogReduction(identicalErrorDelay),
		logger:       logger,
	}

	if err := service.validateServiceConnection(ctx, conn, endpoint); err != nil {
		return nil, fmt.Errorf("validate service connection: %w", err)
	}

	return service, nil
}

func (r *remoteRuntimeService) log(level int, msg string, keyAndValues ...any) {
	internal.Log(r.logger, level, msg, keyAndValues...)
}

func (r *remoteRuntimeService) logErr(err error, msg string, keyAndValues ...any) {
	internal.LogErr(r.logger, err, msg, keyAndValues...)
}

// validateServiceConnection tries to connect to the remote runtime service by
// using the CRI v1 API version and fails if that's not possible.
func (r *remoteRuntimeService) validateServiceConnection(ctx context.Context, conn *grpc.ClientConn, endpoint string) error {
	r.log(4, "Validating the CRI v1 API runtime version")
	r.runtimeClient = runtimeapi.NewRuntimeServiceClient(conn)

	if _, err := r.runtimeClient.Version(ctx, &runtimeapi.VersionRequest{}); err != nil {
		return fmt.Errorf("validate CRI v1 runtime API for endpoint %q: %w", endpoint, err)
	}

	r.log(2, "Validated CRI v1 runtime API")
	return nil
}

// Version returns the runtime name, runtime version and runtime API version.
func (r *remoteRuntimeService) Version(ctx context.Context, apiVersion string) (*runtimeapi.VersionResponse, error) {
	r.log(10, "[RemoteRuntimeService] Version", "apiVersion", apiVersion, "timeout", r.timeout)

	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	return r.versionV1(ctx, apiVersion)
}

func (r *remoteRuntimeService) versionV1(ctx context.Context, apiVersion string) (*runtimeapi.VersionResponse, error) {
	typedVersion, err := r.runtimeClient.Version(ctx, &runtimeapi.VersionRequest{
		Version: apiVersion,
	})
	if err != nil {
		r.logErr(err, "Version from runtime service failed")
		return nil, err
	}

	r.log(10, "[RemoteRuntimeService] Version Response", "apiVersion", typedVersion)

	if typedVersion.Version == "" || typedVersion.RuntimeName == "" || typedVersion.RuntimeApiVersion == "" || typedVersion.RuntimeVersion == "" {
		return nil, fmt.Errorf("not all fields are set in VersionResponse (%q)", *typedVersion)
	}

	return typedVersion, err
}

// RunPodSandbox creates and starts a pod-level sandbox. Runtimes should ensure
// the sandbox is in ready state.
func (r *remoteRuntimeService) RunPodSandbox(ctx context.Context, config *runtimeapi.PodSandboxConfig, runtimeHandler string) (string, error) {
	// Use 2 times longer timeout for sandbox operation (4 mins by default)
	// TODO: Make the pod sandbox timeout configurable.
	timeout := r.timeout * 2

	r.log(10, "[RemoteRuntimeService] RunPodSandbox", "config", config, "runtimeHandler", runtimeHandler, "timeout", timeout)

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	resp, err := r.runtimeClient.RunPodSandbox(ctx, &runtimeapi.RunPodSandboxRequest{
		Config:         config,
		RuntimeHandler: runtimeHandler,
	})

	if err != nil {
		r.logErr(err, "RunPodSandbox from runtime service failed")
		return "", err
	}

	podSandboxID := resp.PodSandboxId

	if podSandboxID == "" {
		errorMessage := fmt.Sprintf("PodSandboxId is not set for sandbox %q", config.Metadata)
		err := errors.New(errorMessage)
		r.logErr(err, "RunPodSandbox failed")
		return "", err
	}

	r.log(10, "[RemoteRuntimeService] RunPodSandbox Response", "podSandboxID", podSandboxID)

	return podSandboxID, nil
}

// StopPodSandbox stops the sandbox. If there are any running containers in the
// sandbox, they should be forced to termination.
func (r *remoteRuntimeService) StopPodSandbox(ctx context.Context, podSandBoxID string) (err error) {
	r.log(10, "[RemoteRuntimeService] StopPodSandbox", "podSandboxID", podSandBoxID, "timeout", r.timeout)

	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	if _, err := r.runtimeClient.StopPodSandbox(ctx, &runtimeapi.StopPodSandboxRequest{
		PodSandboxId: podSandBoxID,
	}); err != nil {
		r.logErr(err, "StopPodSandbox from runtime service failed", "podSandboxID", podSandBoxID)
		return err
	}

	r.log(10, "[RemoteRuntimeService] StopPodSandbox Response", "podSandboxID", podSandBoxID)

	return nil
}

// RemovePodSandbox removes the sandbox. If there are any containers in the
// sandbox, they should be forcibly removed.
func (r *remoteRuntimeService) RemovePodSandbox(ctx context.Context, podSandBoxID string) (err error) {
	r.log(10, "[RemoteRuntimeService] RemovePodSandbox", "podSandboxID", podSandBoxID, "timeout", r.timeout)
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	if _, err := r.runtimeClient.RemovePodSandbox(ctx, &runtimeapi.RemovePodSandboxRequest{
		PodSandboxId: podSandBoxID,
	}); err != nil {
		r.logErr(err, "RemovePodSandbox from runtime service failed", "podSandboxID", podSandBoxID)
		return err
	}

	r.log(10, "[RemoteRuntimeService] RemovePodSandbox Response", "podSandboxID", podSandBoxID)

	return nil
}

// PodSandboxStatus returns the status of the PodSandbox.
func (r *remoteRuntimeService) PodSandboxStatus(ctx context.Context, podSandBoxID string, verbose bool) (*runtimeapi.PodSandboxStatusResponse, error) {
	r.log(10, "[RemoteRuntimeService] PodSandboxStatus", "podSandboxID", podSandBoxID, "timeout", r.timeout)
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	return r.podSandboxStatusV1(ctx, podSandBoxID, verbose)
}

func (r *remoteRuntimeService) podSandboxStatusV1(ctx context.Context, podSandBoxID string, verbose bool) (*runtimeapi.PodSandboxStatusResponse, error) {
	resp, err := r.runtimeClient.PodSandboxStatus(ctx, &runtimeapi.PodSandboxStatusRequest{
		PodSandboxId: podSandBoxID,
		Verbose:      verbose,
	})
	if err != nil {
		return nil, err
	}

	r.log(10, "[RemoteRuntimeService] PodSandboxStatus Response", "podSandboxID", podSandBoxID, "status", resp.Status)

	status := resp.Status
	if resp.Status != nil {
		if err := verifySandboxStatus(status); err != nil {
			return nil, err
		}
	}

	return resp, nil
}

// ListPodSandbox returns a list of PodSandboxes.
func (r *remoteRuntimeService) ListPodSandbox(ctx context.Context, filter *runtimeapi.PodSandboxFilter) ([]*runtimeapi.PodSandbox, error) {
	r.log(10, "[RemoteRuntimeService] ListPodSandbox", "filter", filter, "timeout", r.timeout)
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	return r.listPodSandboxV1(ctx, filter)
}

func (r *remoteRuntimeService) listPodSandboxV1(ctx context.Context, filter *runtimeapi.PodSandboxFilter) ([]*runtimeapi.PodSandbox, error) {
	resp, err := r.runtimeClient.ListPodSandbox(ctx, &runtimeapi.ListPodSandboxRequest{
		Filter: filter,
	})
	if err != nil {
		r.logErr(err, "ListPodSandbox with filter from runtime service failed", "filter", filter)
		return nil, err
	}

	r.log(10, "[RemoteRuntimeService] ListPodSandbox Response", "filter", filter, "items", resp.Items)

	return resp.Items, nil
}

// CreateContainer creates a new container in the specified PodSandbox.
func (r *remoteRuntimeService) CreateContainer(ctx context.Context, podSandBoxID string, config *runtimeapi.ContainerConfig, sandboxConfig *runtimeapi.PodSandboxConfig) (string, error) {
	r.log(10, "[RemoteRuntimeService] CreateContainer", "podSandboxID", podSandBoxID, "timeout", r.timeout)
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	return r.createContainerV1(ctx, podSandBoxID, config, sandboxConfig)
}

func (r *remoteRuntimeService) createContainerV1(ctx context.Context, podSandBoxID string, config *runtimeapi.ContainerConfig, sandboxConfig *runtimeapi.PodSandboxConfig) (string, error) {
	resp, err := r.runtimeClient.CreateContainer(ctx, &runtimeapi.CreateContainerRequest{
		PodSandboxId:  podSandBoxID,
		Config:        config,
		SandboxConfig: sandboxConfig,
	})
	if err != nil {
		r.logErr(err, "CreateContainer in sandbox from runtime service failed", "podSandboxID", podSandBoxID)
		return "", err
	}

	r.log(10, "[RemoteRuntimeService] CreateContainer", "podSandboxID", podSandBoxID, "containerID", resp.ContainerId)
	if resp.ContainerId == "" {
		errorMessage := fmt.Sprintf("ContainerId is not set for container %q", config.Metadata)
		err := errors.New(errorMessage)
		r.logErr(err, "CreateContainer failed")
		return "", err
	}

	return resp.ContainerId, nil
}

// StartContainer starts the container.
func (r *remoteRuntimeService) StartContainer(ctx context.Context, containerID string) (err error) {
	r.log(10, "[RemoteRuntimeService] StartContainer", "containerID", containerID, "timeout", r.timeout)
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	if _, err := r.runtimeClient.StartContainer(ctx, &runtimeapi.StartContainerRequest{
		ContainerId: containerID,
	}); err != nil {
		r.logErr(err, "StartContainer from runtime service failed", "containerID", containerID)
		return err
	}
	r.log(10, "[RemoteRuntimeService] StartContainer Response", "containerID", containerID)

	return nil
}

// StopContainer stops a running container with a grace period (i.e., timeout).
func (r *remoteRuntimeService) StopContainer(ctx context.Context, containerID string, timeout int64) (err error) {
	r.log(10, "[RemoteRuntimeService] StopContainer", "containerID", containerID, "timeout", timeout)
	// Use timeout + default timeout (2 minutes) as timeout to leave extra time
	// for SIGKILL container and request latency.
	t := r.timeout + time.Duration(timeout)*time.Second
	ctx, cancel := context.WithTimeout(ctx, t)
	defer cancel()

	r.logReduction.ClearID(containerID)

	if _, err := r.runtimeClient.StopContainer(ctx, &runtimeapi.StopContainerRequest{
		ContainerId: containerID,
		Timeout:     timeout,
	}); err != nil {
		r.logErr(err, "StopContainer from runtime service failed", "containerID", containerID)
		return err
	}
	r.log(10, "[RemoteRuntimeService] StopContainer Response", "containerID", containerID)

	return nil
}

// RemoveContainer removes the container. If the container is running, the container
// should be forced to removal.
func (r *remoteRuntimeService) RemoveContainer(ctx context.Context, containerID string) (err error) {
	r.log(10, "[RemoteRuntimeService] RemoveContainer", "containerID", containerID, "timeout", r.timeout)
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	r.logReduction.ClearID(containerID)
	if _, err := r.runtimeClient.RemoveContainer(ctx, &runtimeapi.RemoveContainerRequest{
		ContainerId: containerID,
	}); err != nil {
		r.logErr(err, "RemoveContainer from runtime service failed", "containerID", containerID)
		return err
	}
	r.log(10, "[RemoteRuntimeService] RemoveContainer Response", "containerID", containerID)

	return nil
}

// ListContainers lists containers by filters.
func (r *remoteRuntimeService) ListContainers(ctx context.Context, filter *runtimeapi.ContainerFilter) ([]*runtimeapi.Container, error) {
	r.log(10, "[RemoteRuntimeService] ListContainers", "filter", filter, "timeout", r.timeout)
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	return r.listContainersV1(ctx, filter)
}

func (r *remoteRuntimeService) listContainersV1(ctx context.Context, filter *runtimeapi.ContainerFilter) ([]*runtimeapi.Container, error) {
	resp, err := r.runtimeClient.ListContainers(ctx, &runtimeapi.ListContainersRequest{
		Filter: filter,
	})
	if err != nil {
		r.logErr(err, "ListContainers with filter from runtime service failed", "filter", filter)
		return nil, err
	}
	r.log(10, "[RemoteRuntimeService] ListContainers Response", "filter", filter, "containers", resp.Containers)

	return resp.Containers, nil
}

// ContainerStatus returns the container status.
func (r *remoteRuntimeService) ContainerStatus(ctx context.Context, containerID string, verbose bool) (*runtimeapi.ContainerStatusResponse, error) {
	r.log(10, "[RemoteRuntimeService] ContainerStatus", "containerID", containerID, "timeout", r.timeout)
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	return r.containerStatusV1(ctx, containerID, verbose)
}

func (r *remoteRuntimeService) containerStatusV1(ctx context.Context, containerID string, verbose bool) (*runtimeapi.ContainerStatusResponse, error) {
	resp, err := r.runtimeClient.ContainerStatus(ctx, &runtimeapi.ContainerStatusRequest{
		ContainerId: containerID,
		Verbose:     verbose,
	})
	if err != nil {
		// Don't spam the log with endless messages about the same failure.
		if r.logReduction.ShouldMessageBePrinted(err.Error(), containerID) {
			r.logErr(err, "ContainerStatus from runtime service failed", "containerID", containerID)
		}
		return nil, err
	}
	r.logReduction.ClearID(containerID)
	r.log(10, "[RemoteRuntimeService] ContainerStatus Response", "containerID", containerID, "status", resp.Status)

	status := resp.Status
	if resp.Status != nil {
		if err := verifyContainerStatus(status); err != nil {
			r.logErr(err, "verify ContainerStatus failed", "containerID", containerID)
			return nil, err
		}
	}

	return resp, nil
}

// UpdateContainerResources updates a containers resource config
func (r *remoteRuntimeService) UpdateContainerResources(ctx context.Context, containerID string, resources *runtimeapi.ContainerResources) (err error) {
	r.log(10, "[RemoteRuntimeService] UpdateContainerResources", "containerID", containerID, "timeout", r.timeout)
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	if _, err := r.runtimeClient.UpdateContainerResources(ctx, &runtimeapi.UpdateContainerResourcesRequest{
		ContainerId: containerID,
		Linux:       resources.GetLinux(),
		Windows:     resources.GetWindows(),
	}); err != nil {
		r.logErr(err, "UpdateContainerResources from runtime service failed", "containerID", containerID)
		return err
	}
	r.log(10, "[RemoteRuntimeService] UpdateContainerResources Response", "containerID", containerID)

	return nil
}

// ExecSync executes a command in the container, and returns the stdout output.
// If command exits with a non-zero exit code, an error is returned.
func (r *remoteRuntimeService) ExecSync(ctx context.Context, containerID string, cmd []string, timeout time.Duration) (stdout []byte, stderr []byte, err error) {
	r.log(10, "[RemoteRuntimeService] ExecSync", "containerID", containerID, "timeout", timeout)
	// Do not set timeout when timeout is 0.
	var cancel context.CancelFunc
	if timeout != 0 {
		// Use timeout + default timeout (2 minutes) as timeout to leave some time for
		// the runtime to do cleanup.
		ctx, cancel = context.WithTimeout(ctx, r.timeout+timeout)
	} else {
		ctx, cancel = context.WithCancel(ctx)
	}
	defer cancel()

	return r.execSyncV1(ctx, containerID, cmd, timeout)
}

func (r *remoteRuntimeService) execSyncV1(ctx context.Context, containerID string, cmd []string, timeout time.Duration) (stdout []byte, stderr []byte, err error) {
	timeoutSeconds := int64(timeout.Seconds())
	req := &runtimeapi.ExecSyncRequest{
		ContainerId: containerID,
		Cmd:         cmd,
		Timeout:     timeoutSeconds,
	}
	resp, err := r.runtimeClient.ExecSync(ctx, req)
	if err != nil {
		r.logErr(err, "ExecSync cmd from runtime service failed", "containerID", containerID, "cmd", cmd)

		// interpret DeadlineExceeded gRPC errors as timedout errors
		if status.Code(err) == codes.DeadlineExceeded {
			err = fmt.Errorf("%w: %q timed out after %s", ErrCommandTimedOut, strings.Join(cmd, " "), timeout)
		}

		return nil, nil, err
	}

	r.log(10, "[RemoteRuntimeService] ExecSync Response", "containerID", containerID, "exitCode", resp.ExitCode)
	err = nil
	if resp.ExitCode != 0 {
		err = utilexec.CodeExitError{
			Err:  fmt.Errorf("command '%s' exited with %d: %s", strings.Join(cmd, " "), resp.ExitCode, resp.Stderr),
			Code: int(resp.ExitCode),
		}
	}

	return resp.Stdout, resp.Stderr, err
}

// Exec prepares a streaming endpoint to execute a command in the container, and returns the address.
func (r *remoteRuntimeService) Exec(ctx context.Context, req *runtimeapi.ExecRequest) (*runtimeapi.ExecResponse, error) {
	r.log(10, "[RemoteRuntimeService] Exec", "timeout", r.timeout)
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	return r.execV1(ctx, req)
}

func (r *remoteRuntimeService) execV1(ctx context.Context, req *runtimeapi.ExecRequest) (*runtimeapi.ExecResponse, error) {
	resp, err := r.runtimeClient.Exec(ctx, req)
	if err != nil {
		r.logErr(err, "Exec cmd from runtime service failed", "containerID", req.ContainerId, "cmd", req.Cmd)
		return nil, err
	}
	r.log(10, "[RemoteRuntimeService] Exec Response")

	if resp.Url == "" {
		errorMessage := "URL is not set"
		err := errors.New(errorMessage)
		r.logErr(err, "Exec failed")
		return nil, err
	}

	return resp, nil
}

// Attach prepares a streaming endpoint to attach to a running container, and returns the address.
func (r *remoteRuntimeService) Attach(ctx context.Context, req *runtimeapi.AttachRequest) (*runtimeapi.AttachResponse, error) {
	r.log(10, "[RemoteRuntimeService] Attach", "containerID", req.ContainerId, "timeout", r.timeout)
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	return r.attachV1(ctx, req)
}

func (r *remoteRuntimeService) attachV1(ctx context.Context, req *runtimeapi.AttachRequest) (*runtimeapi.AttachResponse, error) {
	resp, err := r.runtimeClient.Attach(ctx, req)
	if err != nil {
		r.logErr(err, "Attach container from runtime service failed", "containerID", req.ContainerId)
		return nil, err
	}
	r.log(10, "[RemoteRuntimeService] Attach Response", "containerID", req.ContainerId)

	if resp.Url == "" {
		errorMessage := "URL is not set"
		err := errors.New(errorMessage)
		r.logErr(err, "Attach failed")
		return nil, err
	}
	return resp, nil
}

// PortForward prepares a streaming endpoint to forward ports from a PodSandbox, and returns the address.
func (r *remoteRuntimeService) PortForward(ctx context.Context, req *runtimeapi.PortForwardRequest) (*runtimeapi.PortForwardResponse, error) {
	r.log(10, "[RemoteRuntimeService] PortForward", "podSandboxID", req.PodSandboxId, "port", req.Port, "timeout", r.timeout)
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	return r.portForwardV1(ctx, req)
}

func (r *remoteRuntimeService) portForwardV1(ctx context.Context, req *runtimeapi.PortForwardRequest) (*runtimeapi.PortForwardResponse, error) {
	resp, err := r.runtimeClient.PortForward(ctx, req)
	if err != nil {
		r.logErr(err, "PortForward from runtime service failed", "podSandboxID", req.PodSandboxId)
		return nil, err
	}
	r.log(10, "[RemoteRuntimeService] PortForward Response", "podSandboxID", req.PodSandboxId)

	if resp.Url == "" {
		errorMessage := "URL is not set"
		err := errors.New(errorMessage)
		r.logErr(err, "PortForward failed")
		return nil, err
	}

	return resp, nil
}

// UpdatePodSandboxResources synchronously updates the PodSandboxConfig with
// the pod-level resource configuration.
func (r *remoteRuntimeService) UpdatePodSandboxResources(ctx context.Context, req *runtimeapi.UpdatePodSandboxResourcesRequest) (*runtimeapi.UpdatePodSandboxResourcesResponse, error) {
	r.log(10, "[RemoteRuntimeService] UpdatePodSandboxResources", "PodSandboxId", req.PodSandboxId, "timeout", r.timeout)
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	resp, err := r.runtimeClient.UpdatePodSandboxResources(ctx, req)
	if err != nil {
		r.logErr(err, "UpdatePodSandboxResources from runtime service failed", "podSandboxID", req.PodSandboxId)
		return nil, err
	}
	r.log(10, "[RemoteRuntimeService] UpdatePodSandboxResources Response", "podSandboxID", req.PodSandboxId)

	return resp, nil
}

// UpdateRuntimeConfig updates the config of a runtime service. The only
// update payload currently supported is the pod CIDR assigned to a node,
// and the runtime service just proxies it down to the network plugin.
func (r *remoteRuntimeService) UpdateRuntimeConfig(ctx context.Context, runtimeConfig *runtimeapi.RuntimeConfig) (err error) {
	r.log(10, "[RemoteRuntimeService] UpdateRuntimeConfig", "runtimeConfig", runtimeConfig, "timeout", r.timeout)
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	// Response doesn't contain anything of interest. This translates to an
	// Event notification to the network plugin, which can't fail, so we're
	// really looking to surface destination unreachable.
	if _, err := r.runtimeClient.UpdateRuntimeConfig(ctx, &runtimeapi.UpdateRuntimeConfigRequest{
		RuntimeConfig: runtimeConfig,
	}); err != nil {
		return err
	}
	r.log(10, "[RemoteRuntimeService] UpdateRuntimeConfig Response", "runtimeConfig", runtimeConfig)

	return nil
}

// Status returns the status of the runtime.
func (r *remoteRuntimeService) Status(ctx context.Context, verbose bool) (*runtimeapi.StatusResponse, error) {
	r.log(10, "[RemoteRuntimeService] Status", "timeout", r.timeout)
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	return r.statusV1(ctx, verbose)
}

func (r *remoteRuntimeService) statusV1(ctx context.Context, verbose bool) (*runtimeapi.StatusResponse, error) {
	resp, err := r.runtimeClient.Status(ctx, &runtimeapi.StatusRequest{
		Verbose: verbose,
	})
	if err != nil {
		r.logErr(err, "Status from runtime service failed")
		return nil, err
	}

	r.log(10, "[RemoteRuntimeService] Status Response", "status", resp.Status)

	if resp.Status == nil || len(resp.Status.Conditions) < 2 {
		errorMessage := "RuntimeReady or NetworkReady condition are not set"
		err := errors.New(errorMessage)
		r.logErr(err, "Status failed")
		return nil, err
	}

	return resp, nil
}

// ContainerStats returns the stats of the container.
func (r *remoteRuntimeService) ContainerStats(ctx context.Context, containerID string) (*runtimeapi.ContainerStats, error) {
	r.log(10, "[RemoteRuntimeService] ContainerStats", "containerID", containerID, "timeout", r.timeout)
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	return r.containerStatsV1(ctx, containerID)
}

func (r *remoteRuntimeService) containerStatsV1(ctx context.Context, containerID string) (*runtimeapi.ContainerStats, error) {
	resp, err := r.runtimeClient.ContainerStats(ctx, &runtimeapi.ContainerStatsRequest{
		ContainerId: containerID,
	})
	if err != nil {
		if r.logReduction.ShouldMessageBePrinted(err.Error(), containerID) {
			r.logErr(err, "ContainerStats from runtime service failed", "containerID", containerID)
		}
		return nil, err
	}
	r.logReduction.ClearID(containerID)
	r.log(10, "[RemoteRuntimeService] ContainerStats Response", "containerID", containerID, "stats", resp.GetStats())

	return resp.GetStats(), nil
}

// ListContainerStats returns the list of ContainerStats given the filter.
func (r *remoteRuntimeService) ListContainerStats(ctx context.Context, filter *runtimeapi.ContainerStatsFilter) ([]*runtimeapi.ContainerStats, error) {
	r.log(10, "[RemoteRuntimeService] ListContainerStats", "filter", filter)
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	return r.listContainerStatsV1(ctx, filter)
}

func (r *remoteRuntimeService) listContainerStatsV1(ctx context.Context, filter *runtimeapi.ContainerStatsFilter) ([]*runtimeapi.ContainerStats, error) {
	resp, err := r.runtimeClient.ListContainerStats(ctx, &runtimeapi.ListContainerStatsRequest{
		Filter: filter,
	})
	if err != nil {
		r.logErr(err, "ListContainerStats with filter from runtime service failed", "filter", filter)
		return nil, err
	}
	r.log(10, "[RemoteRuntimeService] ListContainerStats Response", "filter", filter, "stats", resp.GetStats())

	return resp.GetStats(), nil
}

// PodSandboxStats returns the stats of the pod.
func (r *remoteRuntimeService) PodSandboxStats(ctx context.Context, podSandboxID string) (*runtimeapi.PodSandboxStats, error) {
	r.log(10, "[RemoteRuntimeService] PodSandboxStats", "podSandboxID", podSandboxID, "timeout", r.timeout)
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	return r.podSandboxStatsV1(ctx, podSandboxID)
}

func (r *remoteRuntimeService) podSandboxStatsV1(ctx context.Context, podSandboxID string) (*runtimeapi.PodSandboxStats, error) {
	resp, err := r.runtimeClient.PodSandboxStats(ctx, &runtimeapi.PodSandboxStatsRequest{
		PodSandboxId: podSandboxID,
	})
	if err != nil {
		if r.logReduction.ShouldMessageBePrinted(err.Error(), podSandboxID) {
			r.logErr(err, "PodSandbox from runtime service failed", "podSandboxID", podSandboxID)
		}
		return nil, err
	}
	r.logReduction.ClearID(podSandboxID)
	r.log(10, "[RemoteRuntimeService] PodSandbox Response", "podSandboxID", podSandboxID, "stats", resp.GetStats())

	return resp.GetStats(), nil
}

// ListPodSandboxStats returns the list of pod sandbox stats given the filter
func (r *remoteRuntimeService) ListPodSandboxStats(ctx context.Context, filter *runtimeapi.PodSandboxStatsFilter) ([]*runtimeapi.PodSandboxStats, error) {
	r.log(10, "[RemoteRuntimeService] ListPodSandboxStats", "filter", filter)
	// Set timeout, because runtimes are able to cache disk stats results
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	return r.listPodSandboxStatsV1(ctx, filter)
}

func (r *remoteRuntimeService) listPodSandboxStatsV1(ctx context.Context, filter *runtimeapi.PodSandboxStatsFilter) ([]*runtimeapi.PodSandboxStats, error) {
	resp, err := r.runtimeClient.ListPodSandboxStats(ctx, &runtimeapi.ListPodSandboxStatsRequest{
		Filter: filter,
	})
	if err != nil {
		r.logErr(err, "ListPodSandboxStats with filter from runtime service failed", "filter", filter)
		return nil, err
	}
	r.log(10, "[RemoteRuntimeService] ListPodSandboxStats Response", "filter", filter, "stats", resp.GetStats())

	return resp.GetStats(), nil
}

// ReopenContainerLog reopens the container log file.
func (r *remoteRuntimeService) ReopenContainerLog(ctx context.Context, containerID string) (err error) {
	r.log(10, "[RemoteRuntimeService] ReopenContainerLog", "containerID", containerID, "timeout", r.timeout)
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	if _, err := r.runtimeClient.ReopenContainerLog(ctx, &runtimeapi.ReopenContainerLogRequest{ContainerId: containerID}); err != nil {
		r.logErr(err, "ReopenContainerLog from runtime service failed", "containerID", containerID)
		return err
	}

	r.log(10, "[RemoteRuntimeService] ReopenContainerLog Response", "containerID", containerID)
	return nil
}

// CheckpointContainer triggers a checkpoint of the given CheckpointContainerRequest
func (r *remoteRuntimeService) CheckpointContainer(ctx context.Context, options *runtimeapi.CheckpointContainerRequest) error {
	r.log(10,
		"[RemoteRuntimeService] CheckpointContainer",
		"options",
		options,
	)
	if options == nil {
		return errors.New("CheckpointContainer requires non-nil CheckpointRestoreOptions parameter")
	}
	if options.Timeout < 0 {
		return errors.New("CheckpointContainer requires the timeout value to be > 0")
	}

	ctx, cancel := func(ctx context.Context) (context.Context, context.CancelFunc) {
		defaultTimeout := int64(r.timeout / time.Second)
		if options.Timeout > defaultTimeout {
			// The user requested a specific timeout, let's use that if it
			// is larger than the CRI default.
			return context.WithTimeout(ctx, time.Duration(options.Timeout)*time.Second)
		}
		// If the user requested a timeout less than the
		// CRI default, let's use the CRI default.
		options.Timeout = defaultTimeout
		return context.WithTimeout(ctx, r.timeout)
	}(ctx)
	defer cancel()

	_, err := r.runtimeClient.CheckpointContainer(
		ctx,
		options,
	)

	if err != nil {
		r.logErr(
			err,
			"CheckpointContainer from runtime service failed",
			"containerID",
			options.ContainerId,
		)
		return err
	}
	r.log(10,
		"[RemoteRuntimeService] CheckpointContainer Response",
		"containerID",
		options.ContainerId,
	)

	return nil
}

func (r *remoteRuntimeService) GetContainerEvents(ctx context.Context, containerEventsCh chan *runtimeapi.ContainerEventResponse, connectionEstablishedCallback func(runtimeapi.RuntimeService_GetContainerEventsClient)) error {
	containerEventsStreamingClient, err := r.runtimeClient.GetContainerEvents(ctx, &runtimeapi.GetEventsRequest{})
	if err != nil {
		r.logErr(err, "GetContainerEvents failed to get streaming client")
		return err
	}

	if connectionEstablishedCallback != nil {
		// The connection is successfully established and we have a streaming client ready for use.
		connectionEstablishedCallback(containerEventsStreamingClient)
	}

	for {
		resp, err := containerEventsStreamingClient.Recv()
		if err == io.EOF {
			r.logErr(err, "container events stream is closed")
			return err
		}
		if err != nil {
			r.logErr(err, "failed to receive streaming container event")
			return err
		}
		if resp != nil {
			containerEventsCh <- resp
			r.log(4, "container event received", "resp", resp)
		}
	}
}

// ListMetricDescriptors gets the descriptors for the metrics that will be returned in ListPodSandboxMetrics.
func (r *remoteRuntimeService) ListMetricDescriptors(ctx context.Context) ([]*runtimeapi.MetricDescriptor, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	resp, err := r.runtimeClient.ListMetricDescriptors(ctx, &runtimeapi.ListMetricDescriptorsRequest{})
	if err != nil {
		r.logErr(err, "ListMetricDescriptors from runtime service failed")
		return nil, err
	}
	r.log(10, "[RemoteRuntimeService] ListMetricDescriptors Response", "stats", resp.GetDescriptors())

	return resp.GetDescriptors(), nil
}

// ListPodSandboxMetrics retrieves the metrics for all pod sandboxes.
func (r *remoteRuntimeService) ListPodSandboxMetrics(ctx context.Context) ([]*runtimeapi.PodSandboxMetrics, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	resp, err := r.runtimeClient.ListPodSandboxMetrics(ctx, &runtimeapi.ListPodSandboxMetricsRequest{})
	if err != nil {
		r.logErr(err, "ListPodSandboxMetrics from runtime service failed")
		return nil, err
	}
	r.log(10, "[RemoteRuntimeService] ListPodSandboxMetrics Response", "stats", resp.GetPodMetrics())

	return resp.GetPodMetrics(), nil
}

// RuntimeConfig returns the configuration information of the runtime.
func (r *remoteRuntimeService) RuntimeConfig(ctx context.Context) (*runtimeapi.RuntimeConfigResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	resp, err := r.runtimeClient.RuntimeConfig(ctx, &runtimeapi.RuntimeConfigRequest{})
	if err != nil {
		r.logErr(err, "RuntimeConfig from runtime service failed")
		return nil, err
	}
	r.log(10, "[RemoteRuntimeService] RuntimeConfigResponse", "linuxConfig", resp.GetLinux())

	return resp, nil
}
