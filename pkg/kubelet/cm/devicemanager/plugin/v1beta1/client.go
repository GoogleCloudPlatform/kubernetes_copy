/*
Copyright 2022 The Kubernetes Authors.

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

package v1beta1

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"k8s.io/klog/v2"
	api "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

// DevicePlugin interface provides methods for accessing Device Plugin resources, API and unix socket.
type DevicePlugin interface {
	API() api.DevicePluginClient
	Resource() string
	SocketPath() string
}

// Client interface provides methods for establishing/closing gRPC connection and running the device plugin gRPC client.
type Client interface {
	Connect() error
	Run()
	Disconnect() error
}

type client struct {
	mutex    sync.Mutex
	resource string
	socket   string
	grpc     *grpc.ClientConn
	handler  ClientHandler
	client   api.DevicePluginClient
}

// NewPluginClient returns an initialized device plugin client.
func NewPluginClient(r string, socketPath string, h ClientHandler) Client {
	return &client{
		resource: r,
		socket:   socketPath,
		handler:  h,
	}
}

// Connect is for establishing a gRPC connection between device manager and device plugin.
func (c *client) Connect() error {
	client, conn, err := dial(c.socket)
	if err != nil {
		klog.ErrorS(err, "Unable to connect to device plugin client with socket path", "path", c.socket)
		return err
	}
	c.mutex.Lock()
	c.grpc = conn
	c.client = client
	c.mutex.Unlock()
	return c.handler.PluginConnected(c.resource, c)
}

// Run is for running the device plugin gRPC client.
func (c *client) Run() {
	stream, err := c.client.ListAndWatch(context.Background(), &api.Empty{})
	if err != nil {
		klog.ErrorS(err, "ListAndWatch ended unexpectedly for device plugin", "resource", c.resource)
		return
	}

	for {
		response, err := stream.Recv()
		if err != nil {
			klog.ErrorS(err, "ListAndWatch ended unexpectedly for device plugin", "resource", c.resource)
			return
		}
		klog.V(2).InfoS("State pushed for device plugin", "resource", c.resource, "resourceCapacity", len(response.Devices))
		c.handler.PluginListAndWatchReceiver(c.resource, response)
	}
}

// Disconnect is for closing gRPC connection between device manager and device plugin.
func (c *client) Disconnect() error {
	c.mutex.Lock()
	if c.grpc != nil {
		if err := c.grpc.Close(); err != nil {
			klog.V(2).ErrorS(err, "Failed to close grcp connection", "resource", c.Resource())
		}
		c.grpc = nil
	}
	c.mutex.Unlock()
	c.handler.PluginDisconnected(c.resource)

	klog.V(2).InfoS("device plugin disconnected", "resource", c.resource)
	return nil
}

func (c *client) Resource() string {
	return c.resource
}

func (c *client) API() api.DevicePluginClient {
	return c.client
}

func (c *client) SocketPath() string {
	return c.socket
}

// dial establishes the gRPC communication with the registered device plugin. https://godoc.org/google.golang.org/grpc#Dial
func dial(unixSocketPath string) (api.DevicePluginClient, *grpc.ClientConn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	c, err := grpc.DialContext(ctx, unixSocketPath,
		grpc.WithAuthority("localhost"),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
		grpc.WithContextDialer(func(ctx context.Context, addr string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", addr)
		}),
	)

	if err != nil {
		return nil, nil, fmt.Errorf(errFailedToDialDevicePlugin+" %v", err)
	}

	return api.NewDevicePluginClient(c), c, nil
}
