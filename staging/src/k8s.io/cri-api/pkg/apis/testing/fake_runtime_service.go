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
	"fmt"
	"reflect"
	"sync"
	"time"

	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
)

var (
	// FakeVersion is a version of a fake runtime.
	FakeVersion = "0.1.0"

	// FakeRuntimeName is the name of the fake runtime.
	FakeRuntimeName = "fakeRuntime"

	// FakePodSandboxIPs is an IP address of the fake runtime.
	FakePodSandboxIPs = []string{"192.168.192.168"}
)

// FakePodSandbox is the fake implementation of runtimeapi.PodSandboxStatus.
type FakePodSandbox struct {
	// PodSandboxStatus contains the runtime information for a sandbox.
	runtimeapi.PodSandboxStatus
	// RuntimeHandler is the runtime handler that was issued with the RunPodSandbox request.
	RuntimeHandler string
}

// FakeContainer is a fake container.
type FakeContainer struct {
	// ContainerStatus contains the runtime information for a container.
	runtimeapi.ContainerStatus

	// LinuxResources contains the resources specific to linux containers.
	LinuxResources *runtimeapi.LinuxContainerResources

	// the sandbox id of this container
	SandboxID string
}

// FakeRuntimeService is a fake runetime service.
type FakeRuntimeService struct {
	sync.Mutex

	Called []string
	Errors map[string][]error

	FakeStatus             *runtimeapi.RuntimeStatus
	Containers             map[string]*FakeContainer
	Sandboxes              map[string]*FakePodSandbox
	FakeContainerStats     map[string]*runtimeapi.ContainerStats
	FakePodSandboxStats    map[string]*runtimeapi.PodSandboxStats
	FakePodSandboxMetrics  map[string]*runtimeapi.PodSandboxMetrics
	FakeMetricDescriptors  map[string]*runtimeapi.MetricDescriptor
	FakeContainerMetrics   map[string]*runtimeapi.ContainerMetrics
	FakeLinuxConfiguration *runtimeapi.LinuxRuntimeConfiguration

	ErrorOnSandboxCreate bool
}

// GetContainerID returns the unique container ID from the FakeRuntimeService.
func (r *FakeRuntimeService) GetContainerID(sandboxID, name string, attempt uint32) (string, error) {
	r.Lock()
	defer r.Unlock()

	for id, c := range r.Containers {
		if c.SandboxID == sandboxID && c.Metadata.Name == name && c.Metadata.Attempt == attempt {
			return id, nil
		}
	}
	return "", fmt.Errorf("container (name, attempt, sandboxID)=(%q, %d, %q) not found", name, attempt, sandboxID)
}

// SetFakeSandboxes sets the fake sandboxes for the FakeRuntimeService.
func (r *FakeRuntimeService) SetFakeSandboxes(sandboxes []*FakePodSandbox) {
	r.Lock()
	defer r.Unlock()

	r.Sandboxes = make(map[string]*FakePodSandbox)
	for _, sandbox := range sandboxes {
		sandboxID := sandbox.Id
		r.Sandboxes[sandboxID] = sandbox
	}
}

// SetFakeContainers sets fake containers for the FakeRuntimeService.
func (r *FakeRuntimeService) SetFakeContainers(containers []*FakeContainer) {
	r.Lock()
	defer r.Unlock()

	r.Containers = make(map[string]*FakeContainer)
	for _, c := range containers {
		containerID := c.Id
		r.Containers[containerID] = c
	}

}

// AssertCalls validates whether specified calls were made to the FakeRuntimeService.
func (r *FakeRuntimeService) AssertCalls(calls []string) error {
	r.Lock()
	defer r.Unlock()

	if !reflect.DeepEqual(calls, r.Called) {
		return fmt.Errorf("expected %#v, got %#v", calls, r.Called)
	}
	return nil
}

// GetCalls returns the list of calls made to the FakeRuntimeService.
func (r *FakeRuntimeService) GetCalls() []string {
	r.Lock()
	defer r.Unlock()
	return append([]string{}, r.Called...)
}

// InjectError inject the error to the next call to the FakeRuntimeService.
func (r *FakeRuntimeService) InjectError(f string, err error) {
	r.Lock()
	defer r.Unlock()
	r.Errors[f] = append(r.Errors[f], err)
}

// caller of popError must grab a lock.
func (r *FakeRuntimeService) popError(f string) error {
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

// NewFakeRuntimeService creates a new FakeRuntimeService.
func NewFakeRuntimeService() *FakeRuntimeService {
	return &FakeRuntimeService{
		Called:                make([]string, 0),
		Errors:                make(map[string][]error),
		Containers:            make(map[string]*FakeContainer),
		Sandboxes:             make(map[string]*FakePodSandbox),
		FakeContainerStats:    make(map[string]*runtimeapi.ContainerStats),
		FakePodSandboxStats:   make(map[string]*runtimeapi.PodSandboxStats),
		FakePodSandboxMetrics: make(map[string]*runtimeapi.PodSandboxMetrics),
		FakeContainerMetrics:  make(map[string]*runtimeapi.ContainerMetrics),
	}
}

// Version returns version information from the FakeRuntimeService.
func (r *FakeRuntimeService) Version(_ context.Context, apiVersion string) (*runtimeapi.VersionResponse, error) {
	r.Lock()
	defer r.Unlock()

	r.Called = append(r.Called, "Version")
	if err := r.popError("Version"); err != nil {
		return nil, err
	}

	return &runtimeapi.VersionResponse{
		Version:           FakeVersion,
		RuntimeName:       FakeRuntimeName,
		RuntimeVersion:    FakeVersion,
		RuntimeApiVersion: FakeVersion,
	}, nil
}

// Status returns runtime status of the FakeRuntimeService.
func (r *FakeRuntimeService) Status(_ context.Context, verbose bool) (*runtimeapi.StatusResponse, error) {
	r.Lock()
	defer r.Unlock()

	r.Called = append(r.Called, "Status")
	if err := r.popError("Status"); err != nil {
		return nil, err
	}

	return &runtimeapi.StatusResponse{Status: r.FakeStatus}, nil
}

// RunPodSandbox emulates the run of the pod sandbox in the FakeRuntimeService.
func (r *FakeRuntimeService) RunPodSandbox(_ context.Context, config *runtimeapi.PodSandboxConfig, runtimeHandler string) (string, error) {
	r.Lock()
	defer r.Unlock()

	r.Called = append(r.Called, "RunPodSandbox")
	if err := r.popError("RunPodSandbox"); err != nil {
		return "", err
	}

	if r.ErrorOnSandboxCreate {
		return "", fmt.Errorf("error on sandbox create")
	}

	// PodSandboxID should be randomized for real container runtime, but here just use
	// fixed name from BuildSandboxName() for easily making fake sandboxes.
	podSandboxID := BuildSandboxName(config.Metadata)
	createdAt := time.Now().UnixNano()
	r.Sandboxes[podSandboxID] = &FakePodSandbox{
		PodSandboxStatus: runtimeapi.PodSandboxStatus{
			Id:        podSandboxID,
			Metadata:  config.Metadata,
			State:     runtimeapi.PodSandboxState_SANDBOX_READY,
			CreatedAt: createdAt,
			Network: &runtimeapi.PodSandboxNetworkStatus{
				Ip: FakePodSandboxIPs[0],
			},
			// Without setting sandboxStatus's Linux.Namespaces.Options, kubeGenericRuntimeManager's podSandboxChanged will consider it as network
			// namespace changed and always recreate sandbox which causes pod creation failed.
			// Ref `sandboxStatus.GetLinux().GetNamespaces().GetOptions().GetNetwork() != networkNamespaceForPod(pod)` in podSandboxChanged function.
			Linux: &runtimeapi.LinuxPodSandboxStatus{
				Namespaces: &runtimeapi.Namespace{
					Options: config.GetLinux().GetSecurityContext().GetNamespaceOptions(),
				},
			},
			Labels:         config.Labels,
			Annotations:    config.Annotations,
			RuntimeHandler: runtimeHandler,
		},
		RuntimeHandler: runtimeHandler,
	}
	// assign additional IPs
	additionalIPs := FakePodSandboxIPs[1:]
	additionalPodIPs := make([]*runtimeapi.PodIP, 0, len(additionalIPs))
	for _, ip := range additionalIPs {
		additionalPodIPs = append(additionalPodIPs, &runtimeapi.PodIP{
			Ip: ip,
		})
	}
	r.Sandboxes[podSandboxID].PodSandboxStatus.Network.AdditionalIps = additionalPodIPs
	return podSandboxID, nil
}

// StopPodSandbox emulates the stop of pod sandbox in the FakeRuntimeService.
func (r *FakeRuntimeService) StopPodSandbox(_ context.Context, podSandboxID string) error {
	r.Lock()
	defer r.Unlock()

	r.Called = append(r.Called, "StopPodSandbox")
	if err := r.popError("StopPodSandbox"); err != nil {
		return err
	}

	if s, ok := r.Sandboxes[podSandboxID]; ok {
		s.State = runtimeapi.PodSandboxState_SANDBOX_NOTREADY
	} else {
		return fmt.Errorf("pod sandbox %s not found", podSandboxID)
	}

	return nil
}

// RemovePodSandbox emulates removal of the pod sadbox in the FakeRuntimeService.
func (r *FakeRuntimeService) RemovePodSandbox(_ context.Context, podSandboxID string) error {
	r.Lock()
	defer r.Unlock()

	r.Called = append(r.Called, "RemovePodSandbox")
	if err := r.popError("RemovePodSandbox"); err != nil {
		return err
	}

	// Remove the pod sandbox
	delete(r.Sandboxes, podSandboxID)

	return nil
}

// PodSandboxStatus returns pod sandbox status from the FakeRuntimeService.
func (r *FakeRuntimeService) PodSandboxStatus(_ context.Context, podSandboxID string, verbose bool) (*runtimeapi.PodSandboxStatusResponse, error) {
	r.Lock()
	defer r.Unlock()

	r.Called = append(r.Called, "PodSandboxStatus")
	if err := r.popError("PodSandboxStatus"); err != nil {
		return nil, err
	}

	s, ok := r.Sandboxes[podSandboxID]
	if !ok {
		return nil, fmt.Errorf("pod sandbox %q not found", podSandboxID)
	}

	status := s.PodSandboxStatus
	return &runtimeapi.PodSandboxStatusResponse{Status: &status}, nil
}

// ListPodSandbox returns the list of pod sandboxes in the FakeRuntimeService.
func (r *FakeRuntimeService) ListPodSandbox(_ context.Context, filter *runtimeapi.PodSandboxFilter) ([]*runtimeapi.PodSandbox, error) {
	r.Lock()
	defer r.Unlock()

	r.Called = append(r.Called, "ListPodSandbox")
	if err := r.popError("ListPodSandbox"); err != nil {
		return nil, err
	}

	result := make([]*runtimeapi.PodSandbox, 0)
	for id, s := range r.Sandboxes {
		if filter != nil {
			if filter.Id != "" && filter.Id != id {
				continue
			}
			if filter.State != nil && filter.GetState().State != s.State {
				continue
			}
			if filter.LabelSelector != nil && !filterInLabels(filter.LabelSelector, s.GetLabels()) {
				continue
			}
		}

		result = append(result, &runtimeapi.PodSandbox{
			Id:             s.Id,
			Metadata:       s.Metadata,
			State:          s.State,
			CreatedAt:      s.CreatedAt,
			Labels:         s.Labels,
			Annotations:    s.Annotations,
			RuntimeHandler: s.RuntimeHandler,
		})
	}

	return result, nil
}

// PortForward emulates the set up of port forward in the FakeRuntimeService.
func (r *FakeRuntimeService) PortForward(context.Context, *runtimeapi.PortForwardRequest) (*runtimeapi.PortForwardResponse, error) {
	r.Lock()
	defer r.Unlock()

	r.Called = append(r.Called, "PortForward")
	if err := r.popError("PortForward"); err != nil {
		return nil, err
	}

	return &runtimeapi.PortForwardResponse{}, nil
}

// CreateContainer emulates container creation in the FakeRuntimeService.
func (r *FakeRuntimeService) CreateContainer(_ context.Context, podSandboxID string, config *runtimeapi.ContainerConfig, sandboxConfig *runtimeapi.PodSandboxConfig) (string, error) {
	r.Lock()
	defer r.Unlock()

	r.Called = append(r.Called, "CreateContainer")
	if err := r.popError("CreateContainer"); err != nil {
		return "", err
	}

	// ContainerID should be randomized for real container runtime, but here just use
	// fixed BuildContainerName() for easily making fake containers.
	containerID := BuildContainerName(config.Metadata, podSandboxID)
	createdAt := time.Now().UnixNano()
	createdState := runtimeapi.ContainerState_CONTAINER_CREATED
	imageRef := config.Image.Image
	r.Containers[containerID] = &FakeContainer{
		ContainerStatus: runtimeapi.ContainerStatus{
			Id:          containerID,
			Metadata:    config.Metadata,
			Image:       config.Image,
			ImageRef:    imageRef,
			CreatedAt:   createdAt,
			State:       createdState,
			Labels:      config.Labels,
			Annotations: config.Annotations,
		},
		SandboxID:      podSandboxID,
		LinuxResources: config.GetLinux().GetResources(),
	}

	return containerID, nil
}

// StartContainer emulates start of a container in the FakeRuntimeService.
func (r *FakeRuntimeService) StartContainer(_ context.Context, containerID string) error {
	r.Lock()
	defer r.Unlock()

	r.Called = append(r.Called, "StartContainer")
	if err := r.popError("StartContainer"); err != nil {
		return err
	}

	c, ok := r.Containers[containerID]
	if !ok {
		return fmt.Errorf("container %s not found", containerID)
	}

	// Set container to running.
	c.State = runtimeapi.ContainerState_CONTAINER_RUNNING
	c.StartedAt = time.Now().UnixNano()

	return nil
}

// StopContainer emulates stop of a container in the FakeRuntimeService.
func (r *FakeRuntimeService) StopContainer(_ context.Context, containerID string, timeout int64) error {
	r.Lock()
	defer r.Unlock()

	r.Called = append(r.Called, "StopContainer")
	if err := r.popError("StopContainer"); err != nil {
		return err
	}

	c, ok := r.Containers[containerID]
	if !ok {
		return fmt.Errorf("container %q not found", containerID)
	}

	// Set container to exited state.
	finishedAt := time.Now().UnixNano()
	exitedState := runtimeapi.ContainerState_CONTAINER_EXITED
	c.State = exitedState
	c.FinishedAt = finishedAt

	return nil
}

// RemoveContainer emulates remove of a container in the FakeRuntimeService.
func (r *FakeRuntimeService) RemoveContainer(_ context.Context, containerID string) error {
	r.Lock()
	defer r.Unlock()

	r.Called = append(r.Called, "RemoveContainer")
	if err := r.popError("RemoveContainer"); err != nil {
		return err
	}

	// Remove the container
	delete(r.Containers, containerID)

	return nil
}

// ListContainers returns the list of containers in the FakeRuntimeService.
func (r *FakeRuntimeService) ListContainers(_ context.Context, filter *runtimeapi.ContainerFilter) ([]*runtimeapi.Container, error) {
	r.Lock()
	defer r.Unlock()

	r.Called = append(r.Called, "ListContainers")
	if err := r.popError("ListContainers"); err != nil {
		return nil, err
	}

	result := make([]*runtimeapi.Container, 0)
	for _, s := range r.Containers {
		if filter != nil {
			if filter.Id != "" && filter.Id != s.Id {
				continue
			}
			if filter.PodSandboxId != "" && filter.PodSandboxId != s.SandboxID {
				continue
			}
			if filter.State != nil && filter.GetState().State != s.State {
				continue
			}
			if filter.LabelSelector != nil && !filterInLabels(filter.LabelSelector, s.GetLabels()) {
				continue
			}
		}

		result = append(result, &runtimeapi.Container{
			Id:           s.Id,
			CreatedAt:    s.CreatedAt,
			PodSandboxId: s.SandboxID,
			Metadata:     s.Metadata,
			State:        s.State,
			Image:        s.Image,
			ImageRef:     s.ImageRef,
			Labels:       s.Labels,
			Annotations:  s.Annotations,
		})
	}

	return result, nil
}

// ContainerStatus returns the container status given the container ID in FakeRuntimeService.
func (r *FakeRuntimeService) ContainerStatus(_ context.Context, containerID string, verbose bool) (*runtimeapi.ContainerStatusResponse, error) {
	r.Lock()
	defer r.Unlock()

	r.Called = append(r.Called, "ContainerStatus")
	if err := r.popError("ContainerStatus"); err != nil {
		return nil, err
	}

	c, ok := r.Containers[containerID]
	if !ok {
		return nil, fmt.Errorf("container %q not found", containerID)
	}

	status := c.ContainerStatus
	return &runtimeapi.ContainerStatusResponse{Status: &status}, nil
}

// UpdateContainerResources returns the container resource in the FakeRuntimeService.
func (r *FakeRuntimeService) UpdateContainerResources(context.Context, string, *runtimeapi.ContainerResources) error {
	r.Lock()
	defer r.Unlock()

	r.Called = append(r.Called, "UpdateContainerResources")
	return r.popError("UpdateContainerResources")
}

// ExecSync emulates the sync execution of a command in a container in the FakeRuntimeService.
func (r *FakeRuntimeService) ExecSync(_ context.Context, containerID string, cmd []string, timeout time.Duration) (stdout []byte, stderr []byte, err error) {
	r.Lock()
	defer r.Unlock()

	r.Called = append(r.Called, "ExecSync")
	err = r.popError("ExecSync")
	return
}

// Exec emulates the execution of a command in a container in the FakeRuntimeService.
func (r *FakeRuntimeService) Exec(context.Context, *runtimeapi.ExecRequest) (*runtimeapi.ExecResponse, error) {
	r.Lock()
	defer r.Unlock()

	r.Called = append(r.Called, "Exec")
	if err := r.popError("Exec"); err != nil {
		return nil, err
	}

	return &runtimeapi.ExecResponse{}, nil
}

// Attach emulates the attach request in the FakeRuntimeService.
func (r *FakeRuntimeService) Attach(_ context.Context, req *runtimeapi.AttachRequest) (*runtimeapi.AttachResponse, error) {
	r.Lock()
	defer r.Unlock()

	r.Called = append(r.Called, "Attach")
	if err := r.popError("Attach"); err != nil {
		return nil, err
	}

	return &runtimeapi.AttachResponse{}, nil
}

// UpdateRuntimeConfig emulates the update of a runtime config for the FakeRuntimeService.
func (r *FakeRuntimeService) UpdateRuntimeConfig(_ context.Context, runtimeCOnfig *runtimeapi.RuntimeConfig) error {
	r.Lock()
	defer r.Unlock()

	r.Called = append(r.Called, "UpdateRuntimeConfig")
	return r.popError("UpdateRuntimeConfig")
}

// SetFakeContainerStats sets the fake container stats in the FakeRuntimeService.
func (r *FakeRuntimeService) SetFakeContainerStats(containerStats []*runtimeapi.ContainerStats) {
	r.Lock()
	defer r.Unlock()

	r.FakeContainerStats = make(map[string]*runtimeapi.ContainerStats)
	for _, s := range containerStats {
		r.FakeContainerStats[s.Attributes.Id] = s
	}
}

// ContainerStats returns the container stats in the FakeRuntimeService.
func (r *FakeRuntimeService) ContainerStats(_ context.Context, containerID string) (*runtimeapi.ContainerStats, error) {
	r.Lock()
	defer r.Unlock()

	r.Called = append(r.Called, "ContainerStats")
	if err := r.popError("ContainerStats"); err != nil {
		return nil, err
	}

	s, found := r.FakeContainerStats[containerID]
	if !found {
		return nil, fmt.Errorf("no stats for container %q", containerID)
	}
	return s, nil
}

// ListContainerStats returns the list of all container stats given the filter in the FakeRuntimeService.
func (r *FakeRuntimeService) ListContainerStats(_ context.Context, filter *runtimeapi.ContainerStatsFilter) ([]*runtimeapi.ContainerStats, error) {
	r.Lock()
	defer r.Unlock()

	r.Called = append(r.Called, "ListContainerStats")
	if err := r.popError("ListContainerStats"); err != nil {
		return nil, err
	}

	var result []*runtimeapi.ContainerStats
	for _, c := range r.Containers {
		if filter != nil {
			if filter.Id != "" && filter.Id != c.Id {
				continue
			}
			if filter.PodSandboxId != "" && filter.PodSandboxId != c.SandboxID {
				continue
			}
			if filter.LabelSelector != nil && !filterInLabels(filter.LabelSelector, c.GetLabels()) {
				continue
			}
		}
		s, found := r.FakeContainerStats[c.Id]
		if !found {
			continue
		}
		result = append(result, s)
	}

	return result, nil
}

// SetFakePodSandboxStats sets the fake pod sandbox stats in the FakeRuntimeService.
func (r *FakeRuntimeService) SetFakePodSandboxStats(podStats []*runtimeapi.PodSandboxStats) {
	r.Lock()
	defer r.Unlock()

	r.FakePodSandboxStats = make(map[string]*runtimeapi.PodSandboxStats)
	for _, s := range podStats {
		r.FakePodSandboxStats[s.Attributes.Id] = s
	}
}

// PodSandboxStats returns the sandbox stats in the FakeRuntimeService.
func (r *FakeRuntimeService) PodSandboxStats(_ context.Context, podSandboxID string) (*runtimeapi.PodSandboxStats, error) {
	r.Lock()
	defer r.Unlock()

	r.Called = append(r.Called, "PodSandboxStats")
	if err := r.popError("PodSandboxStats"); err != nil {
		return nil, err
	}

	s, found := r.FakePodSandboxStats[podSandboxID]
	if !found {
		return nil, fmt.Errorf("no stats for pod sandbox %q", podSandboxID)
	}
	return s, nil
}

// ListPodSandboxStats returns the list of all pod sandbox stats given the filter in the FakeRuntimeService.
func (r *FakeRuntimeService) ListPodSandboxStats(_ context.Context, filter *runtimeapi.PodSandboxStatsFilter) ([]*runtimeapi.PodSandboxStats, error) {
	r.Lock()
	defer r.Unlock()

	r.Called = append(r.Called, "ListPodSandboxStats")
	if err := r.popError("ListPodSandboxStats"); err != nil {
		return nil, err
	}

	var result []*runtimeapi.PodSandboxStats
	for _, sb := range r.Sandboxes {
		if filter != nil {
			if filter.Id != "" && filter.Id != sb.Id {
				continue
			}
			if filter.LabelSelector != nil && !filterInLabels(filter.LabelSelector, sb.GetLabels()) {
				continue
			}
		}
		s, found := r.FakePodSandboxStats[sb.Id]
		if !found {
			continue
		}
		result = append(result, s)
	}

	return result, nil
}

// ReopenContainerLog emulates call to the reopen container log in the FakeRuntimeService.
func (r *FakeRuntimeService) ReopenContainerLog(_ context.Context, containerID string) error {
	r.Lock()
	defer r.Unlock()

	r.Called = append(r.Called, "ReopenContainerLog")

	if err := r.popError("ReopenContainerLog"); err != nil {
		return err
	}

	return nil
}

// CheckpointContainer emulates call to checkpoint a container in the FakeRuntimeService.
func (r *FakeRuntimeService) CheckpointContainer(_ context.Context, options *runtimeapi.CheckpointContainerRequest) error {
	r.Lock()
	defer r.Unlock()

	r.Called = append(r.Called, "CheckpointContainer")

	if err := r.popError("CheckpointContainer"); err != nil {
		return err
	}

	return nil
}

func (f *FakeRuntimeService) GetContainerEvents(ctx context.Context, containerEventsCh chan *runtimeapi.ContainerEventResponse, connectionEstablishedCallback func(runtimeapi.RuntimeService_GetContainerEventsClient)) error {
	return nil
}

// SetFakeMetricDescriptors sets the fake metrics descriptors in the FakeRuntimeService.
func (r *FakeRuntimeService) SetFakeMetricDescriptors(descs []*runtimeapi.MetricDescriptor) {
	r.Lock()
	defer r.Unlock()

	r.FakeMetricDescriptors = make(map[string]*runtimeapi.MetricDescriptor)
	for _, d := range descs {
		r.FakeMetricDescriptors[d.Name] = d
	}
}

// ListMetricDescriptors gets the descriptors for the metrics that will be returned in ListPodSandboxMetrics.
func (r *FakeRuntimeService) ListMetricDescriptors(_ context.Context) ([]*runtimeapi.MetricDescriptor, error) {
	r.Lock()
	defer r.Unlock()

	r.Called = append(r.Called, "ListMetricDescriptors")
	if err := r.popError("ListMetricDescriptors"); err != nil {
		return nil, err
	}

	descs := make([]*runtimeapi.MetricDescriptor, 0, len(r.FakeMetricDescriptors))
	for _, d := range r.FakeMetricDescriptors {
		descs = append(descs, d)
	}

	return descs, nil
}

// SetFakePodSandboxMetrics sets the fake pod sandbox metrics in the FakeRuntimeService.
func (r *FakeRuntimeService) SetFakePodSandboxMetrics(podStats []*runtimeapi.PodSandboxMetrics) {
	r.Lock()
	defer r.Unlock()

	r.FakePodSandboxMetrics = make(map[string]*runtimeapi.PodSandboxMetrics)
	for _, s := range podStats {
		r.FakePodSandboxMetrics[s.PodSandboxId] = s
	}
}

// ListPodSandboxMetrics returns the list of all pod sandbox metrics in the FakeRuntimeService.
func (r *FakeRuntimeService) ListPodSandboxMetrics(_ context.Context) ([]*runtimeapi.PodSandboxMetrics, error) {
	r.Lock()
	defer r.Unlock()

	r.Called = append(r.Called, "ListPodSandboxMetrics")
	if err := r.popError("ListPodSandboxMetrics"); err != nil {
		return nil, err
	}

	var result []*runtimeapi.PodSandboxMetrics
	for _, sb := range r.Sandboxes {
		s, found := r.FakePodSandboxMetrics[sb.Id]
		if !found {
			continue
		}
		result = append(result, s)
	}

	return result, nil
}

// RuntimeConfig returns runtime configuration of the FakeRuntimeService.
func (r *FakeRuntimeService) RuntimeConfig(_ context.Context) (*runtimeapi.RuntimeConfigResponse, error) {
	r.Lock()
	defer r.Unlock()

	r.Called = append(r.Called, "RuntimeConfig")
	if err := r.popError("RuntimeConfig"); err != nil {
		return nil, err
	}

	return &runtimeapi.RuntimeConfigResponse{Linux: r.FakeLinuxConfiguration}, nil
}

// UpdatePodSandboxResources returns the container resource in the FakeRuntimeService.
func (r *FakeRuntimeService) UpdatePodSandboxResources(context.Context, *runtimeapi.UpdatePodSandboxResourcesRequest) (*runtimeapi.UpdatePodSandboxResourcesResponse, error) {
	r.Lock()
	defer r.Unlock()

	r.Called = append(r.Called, "UpdatePodSandboxResources")
	if err := r.popError("UpdatePodSandboxResources"); err != nil {
		return nil, err
	}

	return &runtimeapi.UpdatePodSandboxResourcesResponse{}, nil
}
