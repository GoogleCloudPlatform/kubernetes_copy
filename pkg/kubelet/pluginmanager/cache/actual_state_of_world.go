/*
Copyright 2019 The Kubernetes Authors.

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

/*
Package cache implements data structures used by the kubelet plugin manager to
keep track of registered plugins.
*/
package cache

import (
	"fmt"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
)

// ActualStateOfWorld defines a set of thread-safe operations for the kubelet
// plugin manager's actual state of the world cache.
// This cache contains a map of socket file path to plugin information of
// all plugins attached to this node.
type ActualStateOfWorld interface {

	// GetRegisteredPlugins generates and returns a list of plugins
	// that are successfully registered plugins in the current actual state of world.
	GetRegisteredPlugins() []PluginInfo

	// AddPlugin add the given plugin in the cache.
	// An error will be returned if socketPath of the PluginInfo object is empty.
	// Note that this is different from desired world cache's AddOrUpdatePlugin
	// because for the actual state of world cache, there won't be a scenario where
	// we need to update an existing plugin if the timestamps don't match. This is
	// because the plugin should have been unregistered in the reconciler and therefore
	// removed from the actual state of world cache first before adding it back into
	// the actual state of world cache again with the new timestamp
	AddPlugin(pluginInfo PluginInfo) error

	// RemovePlugin deletes the plugin with the given socket path from the actual
	// state of world.
	// If a plugin does not exist with the given socket path, this is a no-op.
	RemovePlugin(socketPath string)

	// PluginExistsWithCorrectTimestamp checks if the given plugin exists in the current actual
	// state of world cache with the correct timestamp.
	// Deprecated: please use `PluginExistsWithCorrectUUID` instead as it provides a better
	// cross-platform support
	PluginExistsWithCorrectTimestamp(pluginInfo PluginInfo) bool

	// PluginExistsWithCorrectUUID checks if the given plugin exists in the current actual
	// state of world cache with the correct UUID
	PluginExistsWithCorrectUUID(pluginInfo PluginInfo) bool
}

// NewActualStateOfWorld returns a new instance of ActualStateOfWorld
func NewActualStateOfWorld() ActualStateOfWorld {
	return &actualStateOfWorld{
		socketFileToInfo: make(map[string]PluginInfo),
	}
}

type actualStateOfWorld struct {

	// socketFileToInfo is a map containing the set of successfully registered plugins
	// The keys are plugin socket file paths. The values are PluginInfo objects
	socketFileToInfo map[string]PluginInfo
	sync.RWMutex
}

var _ ActualStateOfWorld = &actualStateOfWorld{}

// PluginInfo holds information of a plugin
type PluginInfo struct {
	SocketPath string
	Timestamp  time.Time
	UUID       types.UID
	Handler    PluginHandler
	Name       string
	Endpoint   string
}

func (asw *actualStateOfWorld) AddPlugin(pluginInfo PluginInfo) error {
	asw.Lock()
	defer asw.Unlock()

	if pluginInfo.SocketPath == "" {
		return fmt.Errorf("socket path is empty")
	}
	if _, ok := asw.socketFileToInfo[pluginInfo.SocketPath]; ok {
		klog.V(2).InfoS("Plugin exists in actual state cache", "path", pluginInfo.SocketPath)
	}
	asw.socketFileToInfo[pluginInfo.SocketPath] = pluginInfo
	return nil
}

func (asw *actualStateOfWorld) RemovePlugin(socketPath string) {
	asw.Lock()
	defer asw.Unlock()

	delete(asw.socketFileToInfo, socketPath)
}

func (asw *actualStateOfWorld) GetRegisteredPlugins() []PluginInfo {
	asw.RLock()
	defer asw.RUnlock()

	currentPlugins := []PluginInfo{}
	for _, pluginInfo := range asw.socketFileToInfo {
		currentPlugins = append(currentPlugins, pluginInfo)
	}
	return currentPlugins
}

func (asw *actualStateOfWorld) PluginExistsWithCorrectTimestamp(pluginInfo PluginInfo) bool {
	asw.RLock()
	defer asw.RUnlock()

	// We need to check both if the socket file path exists, and the timestamp
	// matches the given plugin (from the desired state cache) timestamp
	actualStatePlugin, exists := asw.socketFileToInfo[pluginInfo.SocketPath]
	return exists && (actualStatePlugin.Timestamp == pluginInfo.Timestamp)
}

func (asw *actualStateOfWorld) PluginExistsWithCorrectUUID(pluginInfo PluginInfo) bool {
	asw.RLock()
	defer asw.RUnlock()

	// We need to check both if the socket file path exists, and the UUID
	// matches the given plugin (from the desired state cache) UUID
	actualStatePlugin, exists := asw.socketFileToInfo[pluginInfo.SocketPath]
	return exists && (actualStatePlugin.UUID == pluginInfo.UUID)
}
