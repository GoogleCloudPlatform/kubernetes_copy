/*
Copyright 2018 The Kubernetes Authors.

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

package pluginwatcher

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	"google.golang.org/grpc"
	"k8s.io/klog/v2"

	registerapi "k8s.io/kubelet/pkg/apis/pluginregistration/v1"
	"k8s.io/kubernetes/pkg/kubelet/pluginmanager/cache"
	v1beta1 "k8s.io/kubernetes/pkg/kubelet/pluginmanager/pluginwatcher/example_plugin_apis/v1beta1"
	v1beta2 "k8s.io/kubernetes/pkg/kubelet/pluginmanager/pluginwatcher/example_plugin_apis/v1beta2"
)

// examplePlugin is a sample plugin to work with plugin watcher
type examplePlugin struct {
	grpcServer         *grpc.Server
	wg                 sync.WaitGroup
	registrationStatus chan registerapi.RegistrationStatus // for testing
	endpoint           string                              // for testing
	pluginName         string
	pluginType         string
	versions           []string
	unlinkSocket       bool
	getInfoLock        sync.Mutex
}

type pluginServiceV1Beta1 struct {
	server *examplePlugin
}

func (s *pluginServiceV1Beta1) GetExampleInfo(ctx context.Context, rqt *v1beta1.ExampleRequest) (*v1beta1.ExampleResponse, error) {
	klog.InfoS("GetExampleInfo v1beta1field", "field", rqt.V1Beta1Field)
	return &v1beta1.ExampleResponse{}, nil
}

func (s *pluginServiceV1Beta1) RegisterService() {
	v1beta1.RegisterExampleServer(s.server.grpcServer, s)
}

type pluginServiceV1Beta2 struct {
	server *examplePlugin
}

func (s *pluginServiceV1Beta2) GetExampleInfo(ctx context.Context, rqt *v1beta2.ExampleRequest) (*v1beta2.ExampleResponse, error) {
	klog.InfoS("GetExampleInfo v1beta2_field", "field", rqt.V1Beta2Field)
	return &v1beta2.ExampleResponse{}, nil
}

func (s *pluginServiceV1Beta2) RegisterService() {
	v1beta2.RegisterExampleServer(s.server.grpcServer, s)
}

// NewExamplePlugin returns an initialized examplePlugin instance
func NewExamplePlugin() *examplePlugin {
	return &examplePlugin{}
}

// NewTestExamplePlugin returns an initialized examplePlugin instance for testing
func NewTestExamplePlugin(pluginName string, pluginType string, endpoint string, advertisedVersions ...string) *examplePlugin {
	return &examplePlugin{
		pluginName:         pluginName,
		pluginType:         pluginType,
		endpoint:           endpoint,
		versions:           advertisedVersions,
		registrationStatus: make(chan registerapi.RegistrationStatus),
		unlinkSocket:       true, // delete unix socket file on close
	}
}

// GetPluginInfo returns a PluginInfo object
func GetPluginInfo(plugin *examplePlugin) cache.PluginInfo {
	return cache.PluginInfo{
		SocketPath: plugin.endpoint,
	}
}

// GetInfo is the RPC invoked by plugin watcher
func (e *examplePlugin) GetInfo(ctx context.Context, req *registerapi.InfoRequest) (*registerapi.PluginInfo, error) {
	e.getInfoLock.Lock()
	defer e.getInfoLock.Unlock()
	return &registerapi.PluginInfo{
		Type:              e.pluginType,
		Name:              e.pluginName,
		Endpoint:          e.endpoint,
		SupportedVersions: e.versions,
	}, nil
}

func (e *examplePlugin) NotifyRegistrationStatus(ctx context.Context, status *registerapi.RegistrationStatus) (*registerapi.RegistrationStatusResponse, error) {
	klog.InfoS("Notify registration status", "status", status)

	if e.registrationStatus != nil {
		e.registrationStatus <- *status
	}

	return &registerapi.RegistrationStatusResponse{}, nil
}

// Serve starts a pluginwatcher server and one or more of the plugin services
func (e *examplePlugin) Serve(services ...string) error {
	klog.InfoS("Starting example server", "endpoint", e.endpoint)
	lis, err := net.Listen("unix", e.endpoint)
	if err != nil {
		return err
	}

	lis.(*net.UnixListener).SetUnlinkOnClose(e.unlinkSocket)

	klog.InfoS("Example server started", "endpoint", e.endpoint)
	e.grpcServer = grpc.NewServer()

	// Registers kubelet plugin watcher api.
	registerapi.RegisterRegistrationServer(e.grpcServer, e)

	for _, service := range services {
		switch service {
		case "v1beta1":
			v1beta1 := &pluginServiceV1Beta1{server: e}
			v1beta1.RegisterService()
		case "v1beta2":
			v1beta2 := &pluginServiceV1Beta2{server: e}
			v1beta2.RegisterService()
		default:
			return fmt.Errorf("unsupported service: '%s'", service)
		}
	}

	// Starts service
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		// Blocking call to accept incoming connections.
		if err := e.grpcServer.Serve(lis); err != nil {
			klog.ErrorS(err, "Example server stopped serving")
		}
	}()

	return nil
}

func (e *examplePlugin) Stop() error {
	klog.InfoS("Stopping example server", "endpoint", e.endpoint)

	e.grpcServer.Stop()
	c := make(chan struct{})
	go func() {
		defer close(c)
		e.wg.Wait()
	}()

	select {
	case <-c:
		break
	case <-time.After(time.Second):
		return errors.New("timed out on waiting for stop completion")
	}

	if e.unlinkSocket {
		// NOTE: Unix socket gets unlinked by the net.UnixListener.close()
		// so this is not necessary, but we do it anyway to be explicit.
		if err := os.Remove(e.endpoint); err != nil && !os.IsNotExist(err) {
			klog.ErrorS(err, "Failed to remove endpoint", "endpoint", e.endpoint)
			return err
		}
	}

	klog.InfoS("Example server stopped", "endpoint", e.endpoint)

	return nil
}

func (e *examplePlugin) SetUnlinkSocket(unlinkSocket bool) {
	e.unlinkSocket = unlinkSocket
}

// StuckGetInfo locks getInfoLock to simulate a stuck GetInfo call.
func (e *examplePlugin) StuckGetInfo() {
	e.getInfoLock.Lock()
}

// UnstuckGetInfo unlocks getInfoLock to unstuck GetInfo call.
func (e *examplePlugin) UnstuckGetInfo() {
	e.getInfoLock.Unlock()
}
