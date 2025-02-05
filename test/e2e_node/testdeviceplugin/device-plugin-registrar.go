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
	"net"

	"github.com/onsi/gomega"
	"google.golang.org/grpc"
	kubeletdevicepluginv1beta1 "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
	pluginregistrationapi "k8s.io/kubelet/pkg/apis/pluginregistration/v1"
	"k8s.io/kubernetes/test/e2e/framework"
)

// implement the kubelet plugin interface
type DevicePluginRegistrar struct {
	server        *grpc.Server
	listener      *net.Listener
	plugin        *DevicePlugin
	errorinjector func(string) error
}

func (d *DevicePluginRegistrar) GetInfo(context context.Context, req *pluginregistrationapi.InfoRequest) (*pluginregistrationapi.PluginInfo, error) {
	if d.errorinjector != nil {
		err := d.errorinjector("GetInfo")
		if err != nil {
			return nil, err
		}
	}

	pluginInfo := &pluginregistrationapi.PluginInfo{
		Name:              d.plugin.resourceName,
		Endpoint:          d.plugin.GetEndpoint(),
		Type:              pluginregistrationapi.DevicePlugin,
		SupportedVersions: []string{"v1beta1"},
	}

	return pluginInfo, nil
}

func (d *DevicePluginRegistrar) NotifyRegistrationStatus(ctx context.Context, status *pluginregistrationapi.RegistrationStatus) (*pluginregistrationapi.RegistrationStatusResponse, error) {
	framework.Logf("Received NotifyRegistrationStatus %v", status)
	return &pluginregistrationapi.RegistrationStatusResponse{}, nil
}

func (d *DevicePluginRegistrar) GetDevicePlugin() *DevicePlugin {
	return d.plugin
}

func NewDevicePluginRegistrar(resourceName, uniqueName string, devices []kubeletdevicepluginv1beta1.Device, pluginErrorInjector func(string) error, registrarErrorInjection func(string) error) (*DevicePluginRegistrar, error) {
	plugin, err := NewDevicePlugin(resourceName, uniqueName, false, devices, pluginErrorInjector)

	if err != nil {
		return nil, err
	}

	dpr := &DevicePluginRegistrar{
		plugin:        plugin,
		errorinjector: registrarErrorInjection,
	}

	// Implement the logic to register the device plugin with the kubelet
	// Create a new gRPC server
	dpr.server = grpc.NewServer()
	// Register the device plugin with the server
	pluginregistrationapi.RegisterRegistrationServer(dpr.server, dpr)
	// Create a listener on a specific port
	lis, err := net.Listen("unix", "/var/lib/kubelet/plugins_registry/tdp-registrar-"+uniqueName+".sock")
	dpr.listener = &lis
	if err != nil {
		return nil, err
	}
	// Start the gRPC server
	go func() {
		err := dpr.server.Serve(lis)
		framework.Logf("Device plugin registration server stopped: %v", err)
		gomega.Expect(err).To(gomega.Succeed())
	}()

	return dpr, nil
}

func (d *DevicePluginRegistrar) Stop() {
	framework.Logf("Stopping DevicePluginRegistrar")

	if d.server != nil {
		framework.Logf("Stopping DevicePluginRegistrar grpc server")
		d.server.Stop()
		d.server = nil
		(*d.listener).Close()
	}
	if d.plugin != nil {
		framework.Logf("Stopping plugin")
		d.plugin.Stop()
	}
}
