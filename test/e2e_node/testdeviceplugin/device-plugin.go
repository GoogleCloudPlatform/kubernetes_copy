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

package testdeviceplugin

import (
	"context"
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	kubeletdevicepluginv1beta1 "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
	"k8s.io/kubernetes/test/e2e/framework"
)

type DevicePlugin struct {
	server       *grpc.Server
	listener     *net.Listener
	uniqueName   string
	resourceName string

	devicePluginEndpoint string

	devices     []kubeletdevicepluginv1beta1.Device
	devicesSync sync.Mutex

	devicesUpdateCh chan struct{}

	calls     []string
	callsSync sync.Mutex

	errorInjector func(string) error
}

func NewDevicePlugin(resourceName, uniqueName string, defaultEndpoint bool, devices []kubeletdevicepluginv1beta1.Device, errorInjector func(string) error) (*DevicePlugin, error) {
	var dp = &DevicePlugin{
		calls:           []string{},
		devicesUpdateCh: make(chan struct{}),
		errorInjector:   errorInjector,
		resourceName:    resourceName,
	}

	dp.devicesSync.Lock()
	dp.devices = devices
	dp.devicesSync.Unlock()

	dp.devicePluginEndpoint = fmt.Sprintf("%s-%s.sock", "test-device-plugin", uniqueName)
	if defaultEndpoint {
		dp.devicePluginEndpoint = kubeletdevicepluginv1beta1.DevicePluginPath + dp.devicePluginEndpoint
	} else {
		dp.devicePluginEndpoint = "/var/lib/kubelet/" + dp.devicePluginEndpoint
	}

	dp.uniqueName = uniqueName

	// Implement the logic to register the device plugin with the kubelet
	// Create a new gRPC server
	dp.server = grpc.NewServer()
	// Register the device plugin with the server
	kubeletdevicepluginv1beta1.RegisterDevicePluginServer(dp.server, dp)
	// Create a listener on a specific port
	lis, err := net.Listen("unix", dp.devicePluginEndpoint)
	dp.listener = &lis
	if err != nil {
		return nil, err
	}
	// Start the gRPC server
	go func() {
		err := dp.server.Serve(lis)
		framework.Logf("Device plugin server stopped: %v", err)
		gomega.Expect(err).To(gomega.Succeed())
	}()

	return dp, nil
}

func (dp *DevicePlugin) GetEndpoint() string {
	return dp.devicePluginEndpoint
}

func (dp *DevicePlugin) GetDevicePluginOptions(context.Context, *kubeletdevicepluginv1beta1.Empty) (*kubeletdevicepluginv1beta1.DevicePluginOptions, error) {
	framework.Logf("GetDevicePluginOptions called")

	// lock the mutex and add to a list of calls
	dp.callsSync.Lock()
	dp.calls = append(dp.calls, "GetDevicePluginOptions")
	dp.callsSync.Unlock()

	if dp.errorInjector != nil {
		return &kubeletdevicepluginv1beta1.DevicePluginOptions{}, dp.errorInjector("GetDevicePluginOptions")
	}
	return &kubeletdevicepluginv1beta1.DevicePluginOptions{}, nil
}

func (dp *DevicePlugin) sendDevices(stream kubeletdevicepluginv1beta1.DevicePlugin_ListAndWatchServer) error {
	resp := new(kubeletdevicepluginv1beta1.ListAndWatchResponse)

	dp.devicesSync.Lock()
	for _, d := range dp.devices {
		resp.Devices = append(resp.Devices, &d)
	}
	dp.devicesSync.Unlock()

	return stream.Send(resp)
}

func (dp *DevicePlugin) ListAndWatch(empty *kubeletdevicepluginv1beta1.Empty, stream kubeletdevicepluginv1beta1.DevicePlugin_ListAndWatchServer) error {
	framework.Logf("ListAndWatch called")
	dp.callsSync.Lock()
	dp.calls = append(dp.calls, "ListAndWatch")
	dp.callsSync.Unlock()

	if dp.errorInjector != nil {
		if err := dp.errorInjector("ListAndWatch"); err != nil {
			return err
		}
	}

	if err := dp.sendDevices(stream); err != nil {
		return err
	}

	// when the devices are updated, send the new devices to the kubelet
	// also start a timer and every second call into the dp.errorInjector
	// to simulate a device plugin failure
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-dp.devicesUpdateCh:
			if err := dp.sendDevices(stream); err != nil {
				return err
			}
		case <-ticker.C:
			if dp.errorInjector != nil {
				if err := dp.errorInjector("ListAndWatch"); err != nil {
					return err
				}
			}
		}
	}
}

func (dp *DevicePlugin) Allocate(ctx context.Context, request *kubeletdevicepluginv1beta1.AllocateRequest) (*kubeletdevicepluginv1beta1.AllocateResponse, error) {
	result := new(kubeletdevicepluginv1beta1.AllocateResponse)

	dp.callsSync.Lock()
	dp.calls = append(dp.calls, "Allocate")
	dp.callsSync.Unlock()

	for _, r := range request.ContainerRequests {
		response := &kubeletdevicepluginv1beta1.ContainerAllocateResponse{}
		for _, id := range r.DevicesIDs {
			fpath, err := os.CreateTemp("/tmp", fmt.Sprintf("%s-%s", dp.uniqueName, id))
			gomega.Expect(err).To(gomega.Succeed())

			response.Mounts = append(response.Mounts, &kubeletdevicepluginv1beta1.Mount{
				ContainerPath: fpath.Name(),
				HostPath:      fpath.Name(),
			})
		}
		result.ContainerResponses = append(result.ContainerResponses, response)
	}

	return result, nil
}

func (dp *DevicePlugin) PreStartContainer(ctx context.Context, request *kubeletdevicepluginv1beta1.PreStartContainerRequest) (*kubeletdevicepluginv1beta1.PreStartContainerResponse, error) {
	return nil, nil
}

func (dp *DevicePlugin) GetPreferredAllocation(ctx context.Context, request *kubeletdevicepluginv1beta1.PreferredAllocationRequest) (*kubeletdevicepluginv1beta1.PreferredAllocationResponse, error) {
	return nil, nil
}

func (dp *DevicePlugin) RegisterDevicePlugin(ctx context.Context) error {
	ginkgo.GinkgoHelper()

	// Create a connection to the kubelet
	conn, err := grpc.NewClient("unix://"+kubeletdevicepluginv1beta1.KubeletSocket,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return err
	}
	defer func() {
		err := conn.Close()
		gomega.Expect(err).To(gomega.Succeed())
	}()

	// Create a client for the kubelet
	client := kubeletdevicepluginv1beta1.NewRegistrationClient(conn)

	// Register the device plugin with the kubelet
	_, err = client.Register(ctx, &kubeletdevicepluginv1beta1.RegisterRequest{
		Version:      kubeletdevicepluginv1beta1.Version,
		Endpoint:     dp.GetEndpoint(),
		ResourceName: dp.resourceName,
	})
	if err != nil {
		return err
	}
	return nil
}

func (dp *DevicePlugin) Stop() {
	framework.Logf("Stopping device plugin")
	if dp.server != nil {
		dp.server.Stop()
		dp.server = nil
		(*dp.listener).Close()
	}
}

func (dp *DevicePlugin) WasCalled(method string) bool {
	// lock mutex and then search if the method was called
	dp.callsSync.Lock()
	defer dp.callsSync.Unlock()
	for _, call := range dp.calls {
		if call == method {
			return true
		}
	}
	return false
}

func (dp *DevicePlugin) Calls() []string {
	// lock the mutex and return the calls
	dp.callsSync.Lock()
	defer dp.callsSync.Unlock()
	// return a copy of the calls
	calls := make([]string, len(dp.calls))
	copy(calls, dp.calls)
	return calls
}

func (dp *DevicePlugin) UpdateDevices(devices []kubeletdevicepluginv1beta1.Device) {
	// lock the mutex and update the devices
	dp.devicesSync.Lock()
	defer dp.devicesSync.Unlock()
	dp.devices = devices
	dp.devicesUpdateCh <- struct{}{}
}
