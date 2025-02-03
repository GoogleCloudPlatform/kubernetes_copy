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

package cache

import (
	"reflect"
	goruntime "runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/uuid"
)

// Calls AddPlugin() to add a plugin
// Verifies newly added plugin exists in GetRegisteredPlugins()
// Verifies PluginExistsWithCorrectUUID returns true for the plugin
// Verifies PluginExistsWithCorrectTimestamp returns true for the plugin (excluded on Windows)
func Test_ASW_AddPlugin_Positive_NewPlugin(t *testing.T) {
	pluginInfo := PluginInfo{
		SocketPath: "/var/lib/kubelet/device-plugins/test-plugin.sock",
		Timestamp:  time.Now(),
		UUID:       uuid.NewUUID(),
		Handler:    nil,
		Name:       "test",
	}
	asw := NewActualStateOfWorld()
	err := asw.AddPlugin(pluginInfo)
	// Assert
	if err != nil {
		t.Fatalf("AddPlugin failed. Expected: <no error> Actual: <%v>", err)
	}

	// Get registered plugins and check the newly added plugin is there
	aswPlugins := asw.GetRegisteredPlugins()
	if len(aswPlugins) != 1 {
		t.Fatalf("Actual state of world length should be one but it's %d", len(aswPlugins))
	}
	if !reflect.DeepEqual(aswPlugins[0], pluginInfo) {
		t.Fatalf("Expected\n%#v\nin actual state of world, but got\n%#v\n", pluginInfo, aswPlugins[0])
	}

	// Check PluginExistsWithCorrectUUID returns true
	if !asw.PluginExistsWithCorrectUUID(pluginInfo) {
		t.Fatalf("PluginExistsWithCorrectUUID returns false for plugin that should be registered")
	}

	// Check PluginExistsWithCorrectTimestamp returns true
	// Skipped on Windows. Time measurements are not as fine-grained on Windows and can lead to
	// 2 consecutive time.Now() calls to be return identical timestamps.
	if goruntime.GOOS != "windows" && !asw.PluginExistsWithCorrectTimestamp(pluginInfo) {
		t.Fatalf("PluginExistsWithCorrectTimestamp returns false for plugin that should be registered")
	}
}

// Calls AddPlugin() to add an empty string for socket path
// Verifies the plugin does not exist in GetRegisteredPlugins()
// Verifies PluginExistsWithCorrectUUID returns false
// Verifies PluginExistsWithCorrectTimestamp returns false (excluded on Windows)
func Test_ASW_AddPlugin_Negative_EmptySocketPath(t *testing.T) {
	asw := NewActualStateOfWorld()
	pluginInfo := PluginInfo{
		SocketPath: "",
		Timestamp:  time.Now(),
		UUID:       uuid.NewUUID(),
		Handler:    nil,
		Name:       "test",
	}
	err := asw.AddPlugin(pluginInfo)
	require.EqualError(t, err, "socket path is empty")

	// Get registered plugins and check the newly added plugin is there
	aswPlugins := asw.GetRegisteredPlugins()
	if len(aswPlugins) != 0 {
		t.Fatalf("Actual state of world length should be zero but it's %d", len(aswPlugins))
	}

	// Check PluginExistsWithCorrectUUID returns false
	if asw.PluginExistsWithCorrectUUID(pluginInfo) {
		t.Fatalf("PluginExistsWithCorrectUUID returns true for plugin that's not registered")
	}

	// Check PluginExistsWithCorrectTimestamp returns false
	// Skipped on Windows. Time measurements are not as fine-grained on Windows and can lead to
	// 2 consecutive time.Now() calls to be return identical timestamps.
	if goruntime.GOOS != "windows" && asw.PluginExistsWithCorrectTimestamp(pluginInfo) {
		t.Fatalf("PluginExistsWithCorrectTimestamp returns true for plugin that's not registered")
	}
}

// Calls RemovePlugin() to remove a plugin
// Verifies newly removed plugin no longer exists in GetRegisteredPlugins()
// Verifies PluginExistsWithCorrectUUID returns false
// Verifies PluginExistsWithCorrectTimestamp returns false (excluded on Windows)
func Test_ASW_RemovePlugin_Positive(t *testing.T) {
	// First, add a plugin
	asw := NewActualStateOfWorld()
	pluginInfo := PluginInfo{
		SocketPath: "/var/lib/kubelet/device-plugins/test-plugin.sock",
		Timestamp:  time.Now(),
		UUID:       uuid.NewUUID(),
		Handler:    nil,
		Name:       "test",
	}
	err := asw.AddPlugin(pluginInfo)
	// Assert
	if err != nil {
		t.Fatalf("AddPlugin failed. Expected: <no error> Actual: <%v>", err)
	}

	// Try removing this plugin
	asw.RemovePlugin(pluginInfo.SocketPath)

	// Get registered plugins and check the newly added plugin is not there
	aswPlugins := asw.GetRegisteredPlugins()
	if len(aswPlugins) != 0 {
		t.Fatalf("Actual state of world length should be zero but it's %d", len(aswPlugins))
	}

	// Check PluginExistsWithCorrectUUID returns false
	if asw.PluginExistsWithCorrectUUID(pluginInfo) {
		t.Fatalf("PluginExistsWithCorrectUUID returns true for the removed plugin")
	}

	// Check PluginExistsWithCorrectTimestamp returns false
	// Skipped on Windows. Time measurements are not as fine-grained on Windows and can lead to
	// 2 consecutive time.Now() calls to be return identical timestamps.
	if goruntime.GOOS != "windows" && asw.PluginExistsWithCorrectTimestamp(pluginInfo) {
		t.Fatalf("PluginExistsWithCorrectTimestamp returns true for the removed plugin")
	}
}

// Verifies PluginExistsWithCorrectUUID returns false for an existing
// plugin with the wrong UUID
func Test_ASW_PluginExistsWithCorrectUUID_Negative_WrongUUID(t *testing.T) {
	// First, add a plugin
	asw := NewActualStateOfWorld()
	pluginInfo := PluginInfo{
		SocketPath: "/var/lib/kubelet/device-plugins/test-plugin.sock",
		Timestamp:  time.Now(),
		UUID:       uuid.NewUUID(),
		Handler:    nil,
		Name:       "test",
	}
	err := asw.AddPlugin(pluginInfo)
	// Assert
	if err != nil {
		t.Fatalf("AddPlugin failed. Expected: <no error> Actual: <%v>", err)
	}

	newerPlugin := PluginInfo{
		SocketPath: "/var/lib/kubelet/device-plugins/test-plugin.sock",
		Timestamp:  time.Now(),
		UUID:       uuid.NewUUID(),
	}
	// Check PluginExistsWithCorrectUUID returns false
	if asw.PluginExistsWithCorrectUUID(newerPlugin) {
		t.Fatalf("PluginExistsWithCorrectUUID returns true for a plugin with a different UUID")
	}
}
