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

	"github.com/stretchr/testify/mock"
	api "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

// MockRegistrationHandler is a mock implementation of the RegistrationHandler interface.
type MockRegistrationHandler struct {
	mock.Mock
}

func (m *MockRegistrationHandler) CleanupPluginDirectory(socketDir string) error {
	m.Called(socketDir) // Keep this to track calls in future checks
	return nil
}

// MockClientHandler mocks the ClientHandler interface
type MockClientHandler struct {
	mock.Mock
}

func (m *MockClientHandler) PluginConnected(string, DevicePlugin) error {
	return nil
}

func (m *MockClientHandler) PluginDisconnected(string) {

}

func (m *MockClientHandler) PluginListAndWatchReceiver(string, *api.ListAndWatchResponse) {

}

func TestServerStartRetries(t *testing.T) {
	testCases := []struct {
		name          string
		failAttempts  int
		expectSuccess bool
	}{
		{
			name:          "Succeeds on first attempt",
			failAttempts:  0,
			expectSuccess: true,
		},
		{
			name:          "Succeeds after 2 retries",
			failAttempts:  2,
			expectSuccess: true,
		},
		{
			name:          "Succeeds after 6 retries",
			failAttempts:  6,
			expectSuccess: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// ... setup code ...

			// Create a temporary directory for the socket
			socketDir, err := os.MkdirTemp("", "device-plugin-test")
			if err != nil {
				t.Fatalf("Failed to create temp dir: %v", err)
			}
			defer os.RemoveAll(socketDir)
			socketName := "test.sock"
			socketPath := filepath.Join(socketDir, socketName)

			// Create mock handlers
			rh := &MockRegistrationHandler{}
			ch := &MockClientHandler{}
			rh.On("CleanupPluginDirectory", mock.Anything).Return(nil)

			// Counter to keep track of attempts
			var attemptCount int

			// Define a custom listen function to simulate failures
			customListenFunc := func(network, address string) (net.Listener, error) {
				attemptCount++
				if attemptCount <= tc.failAttempts {
					return nil, fmt.Errorf("simulated listen failure on attempt %d", attemptCount)
				}
				// Use the actual net.Listen after failing the specified times
				return net.Listen(network, address)
			}

			// Create the server with custom options
			s, err := NewServer(
				socketPath,
				rh,
				ch,
				WithListenFunc(customListenFunc),
			)
			if err != nil {
				t.Fatalf("Failed to create server: %v", err)
			}

			// Start the server in a goroutine
			errCh := make(chan error)
			go func() {
				err := s.Start()
				errCh <- err
			}()

			// Wait for the server to start or fail
			timeout := time.After(15 * time.Second)
			ticker := time.NewTicker(100 * time.Millisecond)
			defer ticker.Stop()

			var conn net.Conn
			var attempt int
			for {
				select {
				case err := <-errCh:
					if err != nil {
						if tc.expectSuccess {
							t.Fatalf("Server failed to start: %v", err)
						} else {
							t.Logf("Server failed to start as expected: %v", err)
						}
					} else {
						if !tc.expectSuccess {
							t.Errorf("Expected server to fail, but it started successfully")
						} else {
							t.Log("Server started successfully")
						}
					}
					goto TestDone
				case <-timeout:
					t.Fatalf("Timed out waiting for server to start")
				case <-ticker.C:
					if tc.expectSuccess {
						attempt++
						conn, err = net.Dial("unix", socketPath)
						if err == nil {
							conn.Close()
							t.Log("Successfully connected to the server socket")
							goto TestDone
						} else {
							if attempt%10 == 0 {
								t.Logf("Failed to connect to server socket after %d attempts: %v", attempt, err)
							}
						}
					}
				}
			}

		TestDone:
			// Clean up
			if err := s.Stop(); err != nil {
				t.Errorf("Failed to stop server: %v", err)
			}
		})
	}
}
