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
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/opencontainers/selinux/go-selinux"
	"google.golang.org/grpc"

	core "k8s.io/api/core/v1"
	"k8s.io/apiserver/pkg/server/healthz"
	"k8s.io/klog/v2"
	api "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
	v1helper "k8s.io/kubernetes/pkg/apis/core/v1/helper"
	"k8s.io/kubernetes/pkg/kubelet/config"
	"k8s.io/kubernetes/pkg/kubelet/metrics"
	"k8s.io/kubernetes/pkg/kubelet/pluginmanager/cache"
)

const (
	retryInterval = 1 * time.Second
	maxServeFails = 5
)

// Server interface provides methods for Device plugin registration server.
type Server interface {
	cache.PluginHandler
	healthz.HealthChecker
	Start() error
	Stop() error
	SocketPath() string
}

// GRPCServer is an interface that abstracts the functionality of a gRPC server.
// This interface is implemented by *grpc.Server, but can also be implemented by
// mock objects for testing purposes.
type GRPCServer interface {
	RegisterService(*grpc.ServiceDesc, any)
	Serve(net.Listener) error
	Stop()
	GracefulStop()
	GetServiceInfo() map[string]grpc.ServiceInfo
}

// grpcServerAdapter is an adapter that implements the GRPCServer interface by
// wrapping a *grpc.Server. This allows us to use the real gRPC server in
// production while using mock implementations for testing.
type grpcServerAdapter struct {
	*grpc.Server
}

func NewGRPCServerAdapter(server *grpc.Server) GRPCServer {
	return &grpcServerAdapter{Server: server}
}

func (a *grpcServerAdapter) RegisterService(sd *grpc.ServiceDesc, ss any) {
	a.Server.RegisterService(sd, ss)
}

func (a *grpcServerAdapter) Serve(lis net.Listener) error {
	return a.Server.Serve(lis)
}

func (a *grpcServerAdapter) Stop() {
	a.Server.Stop()
}

func (a *grpcServerAdapter) GracefulStop() {
	a.Server.GracefulStop()
}

func (a *grpcServerAdapter) GetServiceInfo() map[string]grpc.ServiceInfo {
	return a.Server.GetServiceInfo()
}

type server struct {
	socketName string
	socketDir  string
	mutex      sync.Mutex
	wg         sync.WaitGroup
	ctx        context.Context
	cancel     context.CancelFunc
	grpc       *grpc.Server
	grpcServer GRPCServer
	rhandler   RegistrationHandler
	chandler   ClientHandler
	clients    map[string]Client

	// isStarted indicates whether the service has started successfully.
	//
	isStarted atomic.Bool
	// exceptionMonitor is used to count the number of retry attempts after a gRPC serve failure.
	exceptionMonitor atomic.Int32
}

type Option func(*server)

func WithGRPCServer(grpc *grpc.Server, grpcServer GRPCServer) Option {
	return func(s *server) {
		s.grpcServer = grpcServer
		s.grpc = grpc
	}
}

// NewServer returns an initialized device plugin registration server.
func NewServer(socketPath string, rh RegistrationHandler, ch ClientHandler, opts ...Option) (Server, error) {
	if socketPath == "" || !filepath.IsAbs(socketPath) {
		return nil, fmt.Errorf(errBadSocket+" %s", socketPath)
	}

	dir, name := filepath.Split(socketPath)

	klog.V(2).InfoS("Creating device plugin registration server", "version", api.Version, "socket", socketPath)
	defaultGrpc := grpc.NewServer([]grpc.ServerOption{}...)
	s := &server{
		socketName: name,
		socketDir:  dir,
		rhandler:   rh,
		chandler:   ch,
		clients:    make(map[string]Client),
		grpcServer: NewGRPCServerAdapter(defaultGrpc),
		grpc:       defaultGrpc,
	}

	for _, opt := range opts {
		opt(s)
	}

	return s, nil
}

func (s *server) Start() error {
	klog.V(2).InfoS("Starting device plugin registration server")

	if err := os.MkdirAll(s.socketDir, 0750); err != nil {
		klog.ErrorS(err, "Failed to create the device plugin socket directory", "directory", s.socketDir)
		return err
	}

	if selinux.GetEnabled() {
		if err := selinux.SetFileLabel(s.socketDir, config.KubeletPluginsDirSELinuxLabel); err != nil {
			klog.InfoS("Unprivileged containerized plugins might not work. Could not set selinux context on socket dir", "path", s.socketDir, "err", err)
		}
	}

	// For now, we leave cleanup of the *entire* directory up to the Handler
	// (even though we should in theory be able to just wipe the whole directory)
	// because the Handler stores its checkpoint file (amongst others) in here.
	if err := s.rhandler.CleanupPluginDirectory(s.socketDir); err != nil {
		klog.ErrorS(err, "Failed to cleanup the device plugin directory", "directory", s.socketDir)
		return err
	}

	api.RegisterRegistrationServer(s.grpc, s)
	s.ctx, s.cancel = context.WithCancel(context.Background())
	s.wg.Add(1)
	go func(ctx context.Context) {
		defer s.wg.Done()
		s.serveWithRetry(ctx)
	}(s.ctx)

	return nil
}

// serveWithRetry serves the gRPC server with retry.
func (s *server) serveWithRetry(ctx context.Context) {
	for {
		s.setHealthy()
		serveErrCh := make(chan error, 1)
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			err := s.serve()
			serveErrCh <- err
			close(serveErrCh)
			s.setUnhealthy()
		}()

		select {
		case <-ctx.Done():
			klog.InfoS("Context done, stopping server")
			return
		case err := <-serveErrCh:
			if err == nil {
				// Serve completes normally, no retry needed
				return
			}
			time.Sleep(retryInterval)
			klog.ErrorS(err, "Failed to serve device plugin registration grpc server, retrying...")
			continue
		}
	}
}

func (s *server) serve() error {
	ln, err := s.listen()
	if err != nil {
		klog.ErrorS(err, "Failed to listen to socket while starting device plugin registry", "socket", s.socketDir)
		return err
	}
	if err := s.grpcServer.Serve(ln); err != nil {
		klog.ErrorS(err, "Error while serving device plugin registration grpc server")
		return err
	}
	return nil
}

func (s *server) listen() (net.Listener, error) {
	if err := s.cleanupSocket(); err != nil {
		return nil, fmt.Errorf("failed to remove existing socket: %w", err)
	}
	return net.Listen("unix", s.SocketPath())
}

func (s *server) isFileExist() bool {
	if _, err := os.Stat(s.SocketPath()); err != nil {
		if os.IsNotExist(err) {
			return false
		}
		klog.V(2).InfoS("Failed to get socket file", "error", err)
	}
	return true
}

func (s *server) cleanupSocket() error {
	if !s.isFileExist() {
		return nil
	}
	if err := os.Remove(s.SocketPath()); err != nil {
		return err
	}
	return nil
}

func (s *server) Stop() error {
	s.visitClients(func(r string, c Client) {
		if err := s.disconnectClient(r, c); err != nil {
			klog.InfoS("Error disconnecting device plugin client", "resourceName", r, "err", err)
		}
	})

	s.mutex.Lock()
	defer s.mutex.Unlock()

	if s.grpc == nil {
		return nil
	}

	s.cancel()
	s.grpcServer.Stop()
	s.wg.Wait()
	s.grpc = nil
	// During kubelet termination, we do not need the registration server,
	// and we consider the kubelet to be healthy even when it is down.
	s.setHealthy()

	return nil
}

func (s *server) SocketPath() string {
	return filepath.Join(s.socketDir, s.socketName)
}

func (s *server) Register(ctx context.Context, r *api.RegisterRequest) (*api.Empty, error) {
	klog.InfoS("Got registration request from device plugin with resource", "resourceName", r.ResourceName)
	metrics.DevicePluginRegistrationCount.WithLabelValues(r.ResourceName).Inc()

	if !s.isVersionCompatibleWithPlugin(r.Version) {
		err := fmt.Errorf(errUnsupportedVersion, r.Version, api.SupportedVersions)
		klog.InfoS("Bad registration request from device plugin with resource", "resourceName", r.ResourceName, "err", err)
		return &api.Empty{}, err
	}

	if !v1helper.IsExtendedResourceName(core.ResourceName(r.ResourceName)) {
		err := fmt.Errorf(errInvalidResourceName, r.ResourceName)
		klog.InfoS("Bad registration request from device plugin", "err", err)
		return &api.Empty{}, err
	}

	if err := s.connectClient(r.ResourceName, filepath.Join(s.socketDir, r.Endpoint)); err != nil {
		klog.InfoS("Error connecting to device plugin client", "err", err)
		return &api.Empty{}, err
	}

	return &api.Empty{}, nil
}

func (s *server) isVersionCompatibleWithPlugin(versions ...string) bool {
	// TODO(vikasc): Currently this is fine as we only have a single supported version. When we do need to support
	// multiple versions in the future, we may need to extend this function to return a supported version.
	// E.g., say kubelet supports v1beta1 and v1beta2, and we get v1alpha1 and v1beta1 from a device plugin,
	// this function should return v1beta1
	for _, version := range versions {
		for _, supportedVersion := range api.SupportedVersions {
			if version == supportedVersion {
				return true
			}
		}
	}
	return false
}

func (s *server) visitClients(visit func(r string, c Client)) {
	s.mutex.Lock()
	for r, c := range s.clients {
		s.mutex.Unlock()
		visit(r, c)
		s.mutex.Lock()
	}
	s.mutex.Unlock()
}

func (s *server) Name() string {
	return "device-plugin"
}

func (s *server) Check(_ *http.Request) error {
	currentFails := s.exceptionMonitor.Load()
	if !s.isStarted.Load() && currentFails >= maxServeFails {
		// Health check failure requires both conditions to be met: isStarted is false and
		// the number of failed retry attempts exceeds maxServeFails.
		// After obtaining a failed health check result, the exception counter exceptionMonitor should be initialized to 0.
		// The purpose of this is to ensure that the number of retry attempts after a gRPC serve failure reaches maxServeFails.
		s.exceptionMonitor.Store(0)
		return fmt.Errorf("device plugin registration gRPC server failed and no device plugins can register")
	}
	return nil
}

// setHealthy sets the health status of the gRPC server.
func (s *server) setHealthy() {
	s.isStarted.Store(true)
}

// setUnhealthy sets the health status of the gRPC server to unhealthy.
func (s *server) setUnhealthy() {
	s.isStarted.Store(false)
	s.exceptionMonitor.Add(1)
}
