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
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"
	api "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

// fakeRegistrationHandler is a fake implementation of the RegistrationHandler interface.
type fakeRegistrationHandler struct{}

func (f *fakeRegistrationHandler) CleanupPluginDirectory(socketDir string) error {
	return nil
}

// fakeClientHandler is a fake implementation of the ClientHandler interface.
type fakeClientHandler struct{}

func (f *fakeClientHandler) PluginConnected(pluginName string, plugin DevicePlugin) error {
	return nil
}

func (f *fakeClientHandler) PluginDisconnected(pluginName string) {

}

func (f *fakeClientHandler) PluginListAndWatchReceiver(pluginName string, response *api.ListAndWatchResponse) {

}

// mockGRPCServer is a mock gRPC server for testing.
type mockGRPCServer struct {
	grpc         *grpc.Server
	serveError   error
	failureCount int
}

func (m *mockGRPCServer) Serve(lis net.Listener) error {
	serveErrCh := make(chan error, 1)
	go func() {
		err := m.grpc.Serve(lis)
		serveErrCh <- err
		close(serveErrCh)
	}()
	if m.serveError != nil && m.failureCount > 0 {
		// Simulate a gRPC serve failure by closing the listen socket.
		fmt.Println("close called")
		_ = lis.Close()
	}
	err := <-serveErrCh
	if err != nil {
		m.failureCount--
	}
	return err
}

func (m *mockGRPCServer) RegisterService(sd *grpc.ServiceDesc, ss any) {
	m.grpc.RegisterService(sd, ss)
}

func (m *mockGRPCServer) GracefulStop() {
	m.grpc.GracefulStop()
}

func (m *mockGRPCServer) GetServiceInfo() map[string]grpc.ServiceInfo {
	return m.grpc.GetServiceInfo()
}

func (m *mockGRPCServer) Stop() {
	m.grpc.Stop()
}

func TestServeWithRetry(t *testing.T) {
	tests := []struct {
		name          string
		failureCount  int
		serveError    error
		expectSuccess bool
	}{
		{
			name:          "Serve succeeds",
			failureCount:  0,
			serveError:    nil,
			expectSuccess: true,
		},
		{
			name:          "Serve succeeds after retrying twice.",
			failureCount:  2,
			serveError:    fmt.Errorf("mock serve error"),
			expectSuccess: true,
		},
		{
			name:          "Serve fails",
			failureCount:  maxServeFails,
			serveError:    fmt.Errorf("mock serve error"),
			expectSuccess: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create a temporary directory for the socket
			socketDir, err := os.MkdirTemp("", "device-plugin-test")
			if err != nil {
				t.Fatalf("Failed to create temp dir: %v", err)
			}
			defer func(path string) {
				_ = os.RemoveAll(path)
			}(socketDir)
			socketName := "test.sock"
			socketPath := filepath.Join(socketDir, socketName)

			rh := &fakeRegistrationHandler{}
			ch := &fakeClientHandler{}
			grpcServer := &mockGRPCServer{
				grpc:         grpc.NewServer(),
				serveError:   tt.serveError,
				failureCount: tt.failureCount,
			}
			s, err := NewServer(
				socketPath,
				rh,
				ch,
				WithGRPCServer(grpcServer.grpc, grpcServer),
			)
			if err != nil {
				t.Fatalf("Failed to create server: %v", err)
			}

			_ = s.Start()
			defer func(s Server) {
				_ = s.Stop()
			}(s)

			// Wait for a moment to ensure the health check is effective.
			time.Sleep(maxServeFails * retryInterval)

			isHealthy := false
			if err := s.Check(nil); err == nil {
				isHealthy = true
			}

			if isHealthy != tt.expectSuccess {
				t.Errorf("Health check result mismatch: expected %v, got %v", tt.expectSuccess, isHealthy)
			}
		})
	}
}
