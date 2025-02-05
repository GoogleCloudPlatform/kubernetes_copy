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

package v1beta1

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
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

// Server interface provides methods for Device plugin registration server.
type Server interface {
	cache.PluginHandler
	healthz.HealthChecker
	Start() error
	Stop() error
	SocketPath() string
}

type server struct {
	socketName string
	socketDir  string
	mutex      sync.Mutex
	wg         sync.WaitGroup
	grpc       *grpc.Server
	rhandler   RegistrationHandler
	chandler   ClientHandler
	clients    map[string]Client

	// isStarted indicates whether the service has started successfully.
	isStarted bool
	ctx        context.Context
	cancel     context.CancelFunc
	listenFunc func(network, address string) (net.Listener, error)
	serveFunc  func(s *grpc.Server, lis net.Listener) error
}

type notifyListener struct {
	net.Listener
	readyCh chan struct{}
	once    sync.Once
}

func (nl *notifyListener) Accept() (net.Conn, error) {
	// Signal that the server is ready upon the first Accept call
	nl.once.Do(func() {
		close(nl.readyCh)
	})
	return nl.Listener.Accept()
}

type ServerOption func(*server)

// NewServer returns an initialized device plugin registration server.
func NewServer(socketPath string, rh RegistrationHandler, ch ClientHandler, opts ...ServerOption) (Server, error) {
	if socketPath == "" || !filepath.IsAbs(socketPath) {
		return nil, fmt.Errorf(errBadSocket+" %s", socketPath)
	}

	dir, name := filepath.Split(socketPath)

	klog.V(2).InfoS("Creating device plugin registration server", "version", api.Version, "socket", socketPath)
	s := &server{
		socketName: name,
		socketDir:  dir,
		rhandler:   rh,
		chandler:   ch,
		clients:    make(map[string]Client),
		listenFunc: net.Listen,
		serveFunc:  func(srv *grpc.Server, lis net.Listener) error { return srv.Serve(lis) },
	}

	// Apply all the options passed
	for _, opt := range opts {
		opt(s)
	}

	return s, nil
}

func WithListenFunc(listenFunc func(network, address string) (net.Listener, error)) ServerOption {
	return func(s *server) {
		s.listenFunc = listenFunc
	}
}

func WithServeFunc(serveFunc func(s *grpc.Server, lis net.Listener) error) ServerOption {
	return func(s *server) {
		s.serveFunc = serveFunc
	}
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

	s.wg.Add(1)
	s.ctx, s.cancel = context.WithCancel(context.Background())

	// Create a channel to receive the start error
	startErrCh := make(chan error, 1)

	go func(ctx context.Context) {
		defer s.wg.Done()
		s.setHealthy()
		err := s.createAndServe()
		startErrCh <- err
	}(s.ctx)

	err := <-startErrCh
	return err
}

func (s *server) createAndServe() error {
	var retries int
	for {
		klog.Infof("Attempt %d to create and serve gRPC server", retries+1)

		if err := s.cleanupSocket(); err != nil {
			klog.ErrorS(err, "Failed to remove existing socket, retrying...")
			time.Sleep(100 * time.Millisecond)
			continue
		}

		ln, err := s.listenFunc("unix", s.SocketPath())
		if err != nil {
			s.setUnhealthy()
			klog.ErrorS(err, "Failed to listen on socket, retrying...")
			time.Sleep(100 * time.Millisecond)
			continue
		}

		s.grpc = grpc.NewServer()
		api.RegisterRegistrationServer(s.grpc, s)

		serveErrCh := make(chan error, 1)
		readyCh := make(chan struct{})

		nl := &notifyListener{Listener: ln, readyCh: readyCh}

		// Start the gRPC server in a new goroutine
		go func() {
			err := s.serveFunc(s.grpc, nl)
			serveErrCh <- err
		}()

		// Wait for the server to be ready, encounter an error, or timeout
		select {
		case <-readyCh:
			// Server is ready
			klog.InfoS("Server is ready to accept connections")
			return nil
		case err := <-serveErrCh:
			// Serve exited with an error before becoming ready
			klog.ErrorS(err, "Error serving device plugin gRPC server, retrying...")
			s.grpc.Stop()
			nl.Close()
			time.Sleep(100 * time.Millisecond)
			continue
		case <-time.After(5 * time.Second):
			// Timeout waiting for server to be ready
			klog.ErrorS(fmt.Errorf("timeout"), "Server did not become ready, retrying...")
			s.grpc.Stop()
			nl.Close()
			continue
		}
	}
}

func (s *server) cleanupSocket() error {
	if _, err := os.Stat(s.SocketPath()); err == nil {
		if err := os.Remove(s.SocketPath()); err != nil {
			return err
		}
	}
	return nil
}

func (s *server) Stop() error {
	// Cancel the context to stop the goroutine
	if s.cancel != nil {
		s.cancel()
	}

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

	s.grpc.Stop()
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
	if s.isStarted {
		return nil
	}
	return fmt.Errorf("device plugin registration gRPC server failed and no device plugins can register")
}

// setHealthy sets the health status of the gRPC server.
func (s *server) setHealthy() {
	s.isStarted = true
}

// setUnhealthy sets the health status of the gRPC server to unhealthy.
func (s *server) setUnhealthy() {
	s.isStarted = false
}
