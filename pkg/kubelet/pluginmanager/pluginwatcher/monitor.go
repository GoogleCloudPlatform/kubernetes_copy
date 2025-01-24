/*
Copyright 2025 The Kubernetes Authors.

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
	"sync"
	"time"

	"google.golang.org/grpc"
	"k8s.io/klog/v2"
	registerapi "k8s.io/kubelet/pkg/apis/pluginregistration/v1"
	"k8s.io/kubernetes/pkg/kubelet/pluginmanager/cache"
)

const (
	// dialTimeout is the timeout duration for dialing a plugin socket.
	dialTimeout time.Duration = 20 * time.Second

	// getInfoTimeout is the timeout duration for GetInfo call.
	getInfoTimeout time.Duration = 5 * time.Second

	// pluginListInterval is the duration between getting list of registered plugins from ASW.
	pluginListInterval time.Duration = 10 * time.Second

	// retryInterval is the interval duration between connection checks.
	connectionCheckInterval time.Duration = 3 * time.Second

	// maxFailures is the maximum number of allowed connection monitor failures.
	// If the number of consecutive failures exceeds this value, the connection is considered dead.
	maxFailures uint = 2
)

// pluginConnectionMonitor is a struct to monitor GRPC connections between Kubelet and plugins
type pluginConnectionMonitor struct {
	// A sync.Map to store monitored connections
	connMap sync.Map

	// A channel to stop the monitor
	stopCh chan struct{}
	once   sync.Once

	dsw cache.DesiredStateOfWorld
	asw cache.ActualStateOfWorld
}

// newPluginConnectionMonitor creates a new plugin connection monitor.
func newPluginConnectionMonitor(dsw cache.DesiredStateOfWorld, asw cache.ActualStateOfWorld) *pluginConnectionMonitor {
	return &pluginConnectionMonitor{
		connMap: sync.Map{},
		stopCh:  make(chan struct{}),
		dsw:     dsw,
		asw:     asw,
	}
}

// start starts the plugin connection monitor.
func (m *pluginConnectionMonitor) start() {
	go func() {
		for {
			select {
			case <-m.stopCh:
				klog.InfoS("Stopping plugin connection monitor")
				return
			case <-time.After(pluginListInterval):
				for _, plugin := range m.asw.GetRegisteredPlugins() {
					if m.dsw.PluginExists(plugin.SocketPath) {
						if _, exists := m.connMap.Load(plugin.SocketPath); !exists {
							m.connMap.Store(plugin.SocketPath, nil)
							go m.monitorPluginConnection(plugin)
						}
					}
				}
			}
		}
	}()
}

// monitorPluginConnection monitors GRPC connection to the plugin
// by periodically sending GetInfo RPC to the plugin.
func (m *pluginConnectionMonitor) monitorPluginConnection(plugin cache.PluginInfo) {
	socket := plugin.SocketPath
	klog.InfoS("Start monitoring plugin connection", "plugin", plugin.Name, "socket", socket)

	var client registerapi.RegistrationClient
	var conn *grpc.ClientConn
	var err error

	defer func() {
		if conn != nil {
			if err := conn.Close(); err != nil {
				klog.ErrorS(err, "Failed to close connection", "plugin", plugin.Name, "socket", socket)
			}
		}
	}()

	consecutiveFailures := uint(0)
	for {
		select {
		case <-m.stopCh:
			klog.InfoS("Stop monitoring plugin connection", "plugin", plugin.Name, "socket", socket)
			return
		case <-time.After(connectionCheckInterval):
			client, conn, err = getOrEstablishConnection(client, conn, plugin)
			if err == nil {
				m.connMap.Store(socket, conn)
				if err = checkConnection(client, plugin); err == nil {
					consecutiveFailures = 0
					continue
				}
			}

			// Exit after reaching the failure threshold
			consecutiveFailures++
			if consecutiveFailures >= maxFailures {
				klog.ErrorS(err, "Failure threshold reached, remove plugin from dsw and stop monitoring", "plugin", plugin.Name, "socket", socket)
				m.connMap.Delete(socket)
				m.dsw.RemovePlugin(socket)
				return
			}

		}
	}
}

// getOrEstablishConnection ensures a connection is established and returns the client and connection.
func getOrEstablishConnection(
	client registerapi.RegistrationClient,
	conn *grpc.ClientConn,
	plugin cache.PluginInfo,
) (registerapi.RegistrationClient, *grpc.ClientConn, error) {
	// If connection is already established, return it
	if conn != nil {
		return client, conn, nil
	}

	socket := plugin.SocketPath
	newClient, newConn, err := dial(socket, dialTimeout)
	if err != nil {
		klog.ErrorS(err, "Failed to establish connection", "plugin", plugin.Name, "socket", socket)
		return nil, nil, err
	}

	klog.InfoS("Connection established successfully", "plugin", plugin.Name, "socket", socket)
	return newClient, newConn, nil
}

// checkConnection checks the connection to the plugin by sending GetInfo RPC.
func checkConnection(client registerapi.RegistrationClient, plugin cache.PluginInfo) error {
	ctx, cancel := context.WithTimeout(context.Background(), getInfoTimeout)
	defer cancel()

	_, err := client.GetInfo(ctx, &registerapi.InfoRequest{})
	if err != nil {
		klog.ErrorS(err, "Failed to get plugin info", "plugin", plugin.Name, "socket", plugin.SocketPath)
		return err
	}
	return nil
}

// stop closes the stop channel, which results in
// stoping the plugin connection monitor and all running
// monitorPluginConnection goroutines created by it.
func (m *pluginConnectionMonitor) stop() {
	m.once.Do(func() {
		close(m.stopCh)
	})
}
