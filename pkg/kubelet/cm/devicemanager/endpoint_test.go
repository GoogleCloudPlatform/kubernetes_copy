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
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
	plugin "k8s.io/kubernetes/pkg/kubelet/cm/devicemanager/plugin/v1beta1"
)

// monitorCallback is the function called when a device's health state changes,
// or new devices are reported, or old devices are deleted.
// Updated contains the most recent state of the Device.
type monitorCallback func(resourceName string, devices []pluginapi.Device)

func newMockPluginManager() *mockPluginManager {
	return &mockPluginManager{
		func(string) error { return nil },
		func(string, plugin.DevicePlugin) error { return nil },
		func(string) {},
		func(string, *pluginapi.ListAndWatchResponse) {},
	}
}

type mockPluginManager struct {
	cleanupPluginDirectory     func(string) error
	pluginConnected            func(string, plugin.DevicePlugin) error
	pluginDisconnected         func(string)
	pluginListAndWatchReceiver func(string, *pluginapi.ListAndWatchResponse)
}

func (m *mockPluginManager) CleanupPluginDirectory(r string) error {
	return m.cleanupPluginDirectory(r)
}

func (m *mockPluginManager) PluginConnected(r string, p plugin.DevicePlugin) error {
	return m.pluginConnected(r, p)
}

func (m *mockPluginManager) PluginDisconnected(r string) {
	m.pluginDisconnected(r)
}

func (m *mockPluginManager) PluginListAndWatchReceiver(r string, lr *pluginapi.ListAndWatchResponse) {
	m.pluginListAndWatchReceiver(r, lr)
}

func esocketName() string {
	return fmt.Sprintf("mock%d.sock", time.Now().UnixNano())
}

func TestNewEndpoint(t *testing.T) {
	socket := filepath.Join(os.TempDir(), esocketName())

	devs := []*pluginapi.Device{
		{ID: "ADeviceId", Health: pluginapi.Healthy},
	}

	esetup(t, devs, socket, "mock", func(n string, d []pluginapi.Device) {})
}

func TestRun(t *testing.T) {
	socket := filepath.Join(os.TempDir(), esocketName())

	devs := []*pluginapi.Device{
		{ID: "ADeviceId", Health: pluginapi.Healthy},
		{ID: "AnotherDeviceId", Health: pluginapi.Healthy},
		{ID: "AThirdDeviceId", Health: pluginapi.Unhealthy},
	}

	updated := []*pluginapi.Device{
		{ID: "ADeviceId", Health: pluginapi.Unhealthy},
		{ID: "AThirdDeviceId", Health: pluginapi.Healthy},
		{ID: "AFourthDeviceId", Health: pluginapi.Healthy},
	}

	callbackCount := 0
	callbackChan := make(chan int)
	callback := func(n string, devices []pluginapi.Device) {
		// Should be called twice:
		// one for plugin registration, one for plugin update.
		if callbackCount > 2 {
			t.FailNow()
		}

		// Check plugin registration
		if callbackCount == 0 {
			require.Len(t, devices, 3)
			require.Equal(t, devices[0].ID, devs[0].ID)
			require.Equal(t, devices[1].ID, devs[1].ID)
			require.Equal(t, devices[2].ID, devs[2].ID)
			require.Equal(t, devices[0].Health, devs[0].Health)
			require.Equal(t, devices[1].Health, devs[1].Health)
			require.Equal(t, devices[2].Health, devs[2].Health)
		}

		// Check plugin update
		if callbackCount == 1 {
			require.Len(t, devices, 3)
			require.Equal(t, devices[0].ID, updated[0].ID)
			require.Equal(t, devices[1].ID, updated[1].ID)
			require.Equal(t, devices[2].ID, updated[2].ID)
			require.Equal(t, devices[0].Health, updated[0].Health)
			require.Equal(t, devices[1].Health, updated[1].Health)
			require.Equal(t, devices[2].Health, updated[2].Health)
		}

		callbackCount++
		callbackChan <- callbackCount
	}

	p, e := esetup(t, devs, socket, "mock", callback)

	go e.client.Run()
	// Wait for the first callback to be issued.
	<-callbackChan

	p.Update(updated)

	// Wait for the second callback to be issued.
	<-callbackChan

	require.Equal(t, 2, callbackCount)
}

func TestAllocate(t *testing.T) {
	socket := filepath.Join(os.TempDir(), esocketName())
	devs := []*pluginapi.Device{
		{ID: "ADeviceId", Health: pluginapi.Healthy},
	}
	callbackCount := 0
	callbackChan := make(chan int)
	p, e := esetup(t, devs, socket, "mock", func(n string, d []pluginapi.Device) {
		callbackCount++
		callbackChan <- callbackCount
	})

	resp := new(pluginapi.AllocateResponse)
	contResp := new(pluginapi.ContainerAllocateResponse)
	contResp.Devices = append(contResp.Devices, &pluginapi.DeviceSpec{
		ContainerPath: "/dev/aaa",
		HostPath:      "/dev/aaa",
		Permissions:   "mrw",
	})

	contResp.Devices = append(contResp.Devices, &pluginapi.DeviceSpec{
		ContainerPath: "/dev/bbb",
		HostPath:      "/dev/bbb",
		Permissions:   "mrw",
	})

	contResp.Mounts = append(contResp.Mounts, &pluginapi.Mount{
		ContainerPath: "/container_dir1/file1",
		HostPath:      "host_dir1/file1",
		ReadOnly:      true,
	})

	resp.ContainerResponses = append(resp.ContainerResponses, contResp)

	p.SetAllocFunc(func(r *pluginapi.AllocateRequest, devs map[string]pluginapi.Device) (*pluginapi.AllocateResponse, error) {
		return resp, nil
	})

	go e.client.Run()
	// Wait for the callback to be issued.
	select {
	case <-callbackChan:
		break
	case <-time.After(time.Second):
		t.FailNow()
	}

	respOut, err := e.allocate([]string{"ADeviceId"})
	require.NoError(t, err)
	require.Equal(t, resp, respOut)
}

func TestGetPreferredAllocation(t *testing.T) {
	socket := filepath.Join(os.TempDir(), esocketName())
	callbackCount := 0
	callbackChan := make(chan int)
	p, e := esetup(t, []*pluginapi.Device{}, socket, "mock", func(n string, d []pluginapi.Device) {
		callbackCount++
		callbackChan <- callbackCount
	})

	resp := &pluginapi.PreferredAllocationResponse{
		ContainerResponses: []*pluginapi.ContainerPreferredAllocationResponse{
			{DeviceIDs: []string{"device0", "device1", "device2"}},
		},
	}

	p.SetGetPreferredAllocFunc(func(r *pluginapi.PreferredAllocationRequest, devs map[string]pluginapi.Device) (*pluginapi.PreferredAllocationResponse, error) {
		return resp, nil
	})

	go e.client.Run()
	// Wait for the callback to be issued.
	select {
	case <-callbackChan:
		break
	case <-time.After(time.Second):
		t.FailNow()
	}

	respOut, err := e.getPreferredAllocation([]string{}, []string{}, -1)
	require.NoError(t, err)
	require.Equal(t, resp, respOut)
}

func esetup(t *testing.T, devs []*pluginapi.Device, socket, resourceName string, callback monitorCallback) (*plugin.Stub, *endpointImpl) {
	m := newMockPluginManager()

	m.pluginListAndWatchReceiver = func(r string, resp *pluginapi.ListAndWatchResponse) {
		var newDevs []pluginapi.Device
		for _, d := range resp.Devices {
			newDevs = append(newDevs, *d)
		}
		callback(resourceName, newDevs)
	}

	var dp plugin.DevicePlugin
	var wg sync.WaitGroup
	wg.Add(1)
	m.pluginConnected = func(r string, c plugin.DevicePlugin) error {
		dp = c
		wg.Done()
		return nil
	}

	p := plugin.NewDevicePluginStub(devs, socket, resourceName, false, false)
	err := p.Start()
	require.NoError(t, err)
	t.Cleanup(func() {
		err := p.Stop()
		require.NoError(t, err)
	})

	c := plugin.NewPluginClient(resourceName, socket, m)
	err = c.Connect()
	require.NoError(t, err)

	wg.Wait()

	e := newEndpointImpl(dp)
	e.client = c
	t.Cleanup(func() {
		err := e.client.Disconnect()
		require.NoError(t, err)
	})

	m.pluginDisconnected = func(r string) {
		e.setStopTime(time.Now())
	}

	return p, e
}
