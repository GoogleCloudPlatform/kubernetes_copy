/*
Copyright 2023 The Kubernetes Authors.

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

package plugin

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"google.golang.org/grpc"
	drapbv1alpha4 "k8s.io/kubelet/pkg/apis/dra/v1alpha4"
	drapbv1beta1 "k8s.io/kubelet/pkg/apis/dra/v1beta1"
	"k8s.io/kubernetes/test/utils/ktesting"
)

type fakeGRPCServer struct {
}

var _ drapbv1beta1.DRAPluginServer = &fakeGRPCServer{}

func (f *fakeGRPCServer) NodePrepareResources(ctx context.Context, in *drapbv1beta1.NodePrepareResourcesRequest) (*drapbv1beta1.NodePrepareResourcesResponse, error) {
	return &drapbv1beta1.NodePrepareResourcesResponse{Claims: map[string]*drapbv1beta1.NodePrepareResourceResponse{"claim-uid": {
		Devices: []*drapbv1beta1.Device{
			{
				RequestNames: []string{"test-request"},
				CDIDeviceIDs: []string{"test-cdi-id"},
			},
		},
	}}}, nil
}

func (f *fakeGRPCServer) NodeUnprepareResources(ctx context.Context, in *drapbv1beta1.NodeUnprepareResourcesRequest) (*drapbv1beta1.NodeUnprepareResourcesResponse, error) {

	return &drapbv1beta1.NodeUnprepareResourcesResponse{}, nil
}

type tearDown func()

func setupFakeGRPCServer(service string) (string, tearDown, error) {
	p, err := os.MkdirTemp("", "dra_plugin")
	if err != nil {
		return "", nil, err
	}

	closeCh := make(chan struct{})
	addr := filepath.Join(p, "server.sock")
	teardown := func() {
		close(closeCh)
		if err := os.RemoveAll(addr); err != nil {
			panic(err)
		}
	}

	listener, err := net.Listen("unix", addr)
	if err != nil {
		teardown()
		return "", nil, err
	}

	s := grpc.NewServer()
	fakeGRPCServer := &fakeGRPCServer{}
	switch service {
	case drapbv1beta1.DRAPluginService:
		drapbv1beta1.RegisterDRAPluginServer(s, fakeGRPCServer)
	case drapbv1alpha4.NodeService:
		drapbv1alpha4.RegisterNodeServer(s, drapbv1alpha4.V1Beta1ServerWrapper{DRAPluginServer: fakeGRPCServer})
	default:
		return "", nil, fmt.Errorf("unsupported gRPC service: %s", service)
	}

	go func() {
		go func() {
			if err := s.Serve(listener); err != nil {
				panic(err)
			}
		}()
		<-closeCh
		s.GracefulStop()
	}()

	return addr, teardown, nil
}

func TestGRPCConnIsReused(t *testing.T) {
	tCtx := ktesting.Init(t)
	service := drapbv1beta1.DRAPluginService
	addr, teardown, err := setupFakeGRPCServer(service)
	if err != nil {
		t.Fatal(err)
	}
	defer teardown()

	reusedConns := make(map[*grpc.ClientConn]int)
	wg := sync.WaitGroup{}
	m := sync.Mutex{}

	pluginName := "dummy-plugin"
	p := &Plugin{
		name:              pluginName,
		backgroundCtx:     tCtx,
		endpoint:          addr,
		chosenService:     service,
		clientCallTimeout: defaultClientCallTimeout,
	}

	conn, err := p.getOrCreateGRPCConn()
	defer func() {
		err := conn.Close()
		if err != nil {
			t.Error(err)
		}
	}()
	if err != nil {
		t.Fatal(err)
	}

	// ensure the plugin we are using is registered
	draPlugins.add(p)
	defer draPlugins.remove(pluginName, addr)

	// we call `NodePrepareResource` 2 times and check whether a new connection is created or the same is reused
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			client, err := NewDRAPluginClient(pluginName)
			if err != nil {
				t.Error(err)
				return
			}

			req := &drapbv1beta1.NodePrepareResourcesRequest{
				Claims: []*drapbv1beta1.Claim{
					{
						Namespace: "dummy-namespace",
						UID:       "dummy-uid",
						Name:      "dummy-claim",
					},
				},
			}

			_, err = client.NodePrepareResources(tCtx, req)
			assert.NoError(t, err)

			client.mutex.Lock()
			conn := client.conn
			client.mutex.Unlock()

			m.Lock()
			defer m.Unlock()
			reusedConns[conn]++
		}()
	}

	wg.Wait()
	// We should have only one entry otherwise it means another gRPC connection has been created
	if len(reusedConns) != 1 {
		t.Errorf("expected length to be 1 but got %d", len(reusedConns))
	}
	if counter, ok := reusedConns[conn]; ok && counter != 2 {
		t.Errorf("expected counter to be 2 but got %d", counter)
	}
}

func TestNewDRAPluginClient(t *testing.T) {
	for _, test := range []struct {
		description string
		setup       func(string) tearDown
		pluginName  string
		shouldError bool
	}{
		{
			description: "plugin name is empty",
			setup: func(_ string) tearDown {
				return func() {}
			},
			pluginName:  "",
			shouldError: true,
		},
		{
			description: "plugin name not found in the list",
			setup: func(_ string) tearDown {
				return func() {}
			},
			pluginName:  "plugin-name-not-found-in-the-list",
			shouldError: true,
		},
		{
			description: "plugin exists",
			setup: func(name string) tearDown {
				draPlugins.add(&Plugin{name: name})
				return func() {
					draPlugins.remove(name, "")
				}
			},
			pluginName: "dummy-plugin",
		},
	} {
		t.Run(test.description, func(t *testing.T) {
			teardown := test.setup(test.pluginName)
			defer teardown()

			client, err := NewDRAPluginClient(test.pluginName)
			if test.shouldError {
				assert.Nil(t, client)
				assert.Error(t, err)
			} else {
				assert.NotNil(t, client)
				assert.NoError(t, err)
			}
		})
	}
}

func TestGRPCMethods(t *testing.T) {
	for _, test := range []struct {
		description   string
		serverSetup   func(string) (string, tearDown, error)
		service       string
		chosenService string
		expectError   string
	}{
		{
			description:   "v1alpha4",
			serverSetup:   setupFakeGRPCServer,
			service:       drapbv1alpha4.NodeService,
			chosenService: drapbv1alpha4.NodeService,
		},
		{
			description:   "v1beta1",
			serverSetup:   setupFakeGRPCServer,
			service:       drapbv1beta1.DRAPluginService,
			chosenService: drapbv1beta1.DRAPluginService,
		},
		{
			// In practice, such a mismatch between plugin and kubelet should not happen.
			description:   "mismatch",
			serverSetup:   setupFakeGRPCServer,
			service:       drapbv1beta1.DRAPluginService,
			chosenService: drapbv1alpha4.NodeService,
			expectError:   "unknown service v1alpha3.Node",
		},
		{
			// In practice, kubelet wouldn't choose an invalid service.
			description:   "internal-error",
			serverSetup:   setupFakeGRPCServer,
			service:       drapbv1beta1.DRAPluginService,
			chosenService: "some-other-service",
			expectError:   "unsupported chosen service",
		},
	} {
		t.Run(test.description, func(t *testing.T) {
			tCtx := ktesting.Init(t)
			addr, teardown, err := setupFakeGRPCServer(test.service)
			if err != nil {
				t.Fatal(err)
			}
			defer teardown()

			pluginName := "dummy-plugin"
			p := &Plugin{
				name:              pluginName,
				backgroundCtx:     tCtx,
				endpoint:          addr,
				chosenService:     test.chosenService,
				clientCallTimeout: defaultClientCallTimeout,
			}

			conn, err := p.getOrCreateGRPCConn()
			defer func() {
				err := conn.Close()
				if err != nil {
					t.Error(err)
				}
			}()
			if err != nil {
				t.Fatal(err)
			}

			draPlugins.add(p)
			defer draPlugins.remove(pluginName, addr)

			client, err := NewDRAPluginClient(pluginName)
			if err != nil {
				t.Fatal(err)
			}

			_, err = client.NodePrepareResources(tCtx, &drapbv1beta1.NodePrepareResourcesRequest{})
			assertError(t, test.expectError, err)

			_, err = client.NodeUnprepareResources(tCtx, &drapbv1beta1.NodeUnprepareResourcesRequest{})
			assertError(t, test.expectError, err)
		})
	}
}

func assertError(t *testing.T, expectError string, err error) {
	t.Helper()
	switch {
	case err != nil && expectError == "":
		t.Errorf("Expected no error, got: %v", err)
	case err == nil && expectError != "":
		t.Errorf("Expected error %q, got none", expectError)
	case err != nil && !strings.Contains(err.Error(), expectError):
		t.Errorf("Expected error %q, got: %v", expectError, err)
	}
}
