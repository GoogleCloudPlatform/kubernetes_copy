/*
Copyright 2017 The Kubernetes Authors.

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

package devicemanager

import (
	"context"
	"fmt"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
	plugin "k8s.io/kubernetes/pkg/kubelet/cm/devicemanager/plugin/v1beta1"
)

// endpoint maps to a single registered device plugin. It is responsible
// for managing gRPC communications with the device plugin and caching
// device states reported by the device plugin.
type endpoint interface {
	getPreferredAllocation(available, mustInclude []string, size int) (*pluginapi.PreferredAllocationResponse, error)
	allocate(devs []string) (*pluginapi.AllocateResponse, error)
	preStartContainer(devs []string) (*pluginapi.PreStartContainerResponse, error)
	setStopTime(t time.Time)
	isStopped() bool
	stopGracePeriodExpired() bool
}

type endpointImpl struct {
	mutex          sync.Mutex
	resourceName   string
	api            pluginapi.DevicePluginClient
	stopTime       time.Time
	client         plugin.Client // for testing only
	requestTimeout metav1.Duration
}

// newEndpointImpl creates a new endpoint for the given resourceName.
// This is to be used during normal device plugin registration.
func newEndpointImpl(p plugin.DevicePlugin) *endpointImpl {
	return &endpointImpl{
		api:            p.API(),
		resourceName:   p.Resource(),
		requestTimeout: metav1.Duration{Duration: 2 * time.Minute},
	}
}

// newStoppedEndpointImpl creates a new endpoint for the given resourceName with stopTime set.
// This is to be used during Kubelet restart, before the actual device plugin re-registers.
func newStoppedEndpointImpl(resourceName string) *endpointImpl {
	return &endpointImpl{
		resourceName: resourceName,
		stopTime:     time.Now(),
	}
}

func (e *endpointImpl) isStopped() bool {
	e.mutex.Lock()
	defer e.mutex.Unlock()
	return !e.stopTime.IsZero()
}

func (e *endpointImpl) stopGracePeriodExpired() bool {
	e.mutex.Lock()
	defer e.mutex.Unlock()
	return !e.stopTime.IsZero() && time.Since(e.stopTime) > endpointStopGracePeriod
}

func (e *endpointImpl) setStopTime(t time.Time) {
	e.mutex.Lock()
	defer e.mutex.Unlock()
	e.stopTime = t
}

// getPreferredAllocation issues GetPreferredAllocation gRPC call to the device plugin.
func (e *endpointImpl) getPreferredAllocation(available, mustInclude []string, size int) (*pluginapi.PreferredAllocationResponse, error) {
	if e.isStopped() {
		return nil, fmt.Errorf(errEndpointStopped, e)
	}
	return e.api.GetPreferredAllocation(context.Background(), &pluginapi.PreferredAllocationRequest{
		ContainerRequests: []*pluginapi.ContainerPreferredAllocationRequest{
			{
				AvailableDeviceIDs:   available,
				MustIncludeDeviceIDs: mustInclude,
				AllocationSize:       int32(size),
			},
		},
	})
}

// allocate issues Allocate gRPC call to the device plugin.
func (e *endpointImpl) allocate(devs []string) (*pluginapi.AllocateResponse, error) {
	if e.isStopped() {
		return nil, fmt.Errorf(errEndpointStopped, e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), e.requestTimeout.Duration)
	defer cancel()
	return e.api.Allocate(ctx, &pluginapi.AllocateRequest{
		ContainerRequests: []*pluginapi.ContainerAllocateRequest{
			{DevicesIDs: devs},
		},
	})
}

// preStartContainer issues PreStartContainer gRPC call to the device plugin.
func (e *endpointImpl) preStartContainer(devs []string) (*pluginapi.PreStartContainerResponse, error) {
	if e.isStopped() {
		return nil, fmt.Errorf(errEndpointStopped, e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), pluginapi.KubeletPreStartContainerRPCTimeoutInSecs*time.Second)
	defer cancel()
	return e.api.PreStartContainer(ctx, &pluginapi.PreStartContainerRequest{
		DevicesIDs: devs,
	})
}
