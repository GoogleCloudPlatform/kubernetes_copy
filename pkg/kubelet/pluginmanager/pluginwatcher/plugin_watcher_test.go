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
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"
	registerapi "k8s.io/kubelet/pkg/apis/pluginregistration/v1"
	"k8s.io/kubernetes/pkg/kubelet/pluginmanager/cache"
)

var (
	supportedVersions = []string{"v1beta1", "v1beta2"}
)

func init() {
	var logLevel string

	klog.InitFlags(flag.CommandLine)
	flag.Set("alsologtostderr", fmt.Sprintf("%t", true))
	flag.StringVar(&logLevel, "logLevel", "6", "test")
	flag.Lookup("v").Value.Set(logLevel)
}

func initTempDir(t *testing.T) string {
	// Creating a different directory. os.RemoveAll is not atomic enough;
	// os.MkdirAll can get into an "Access Denied" error on Windows.
	d, err := os.MkdirTemp("", "plugin_test")
	if err != nil {
		t.Fatalf("Could not create a temp directory %s: %v", d, err)
	}

	return d
}

func waitForRegistration(
	t *testing.T,
	socketPath string,
	dsw cache.DesiredStateOfWorld) {
	err := retryWithExponentialBackOff(
		time.Duration(500*time.Millisecond),
		func() (bool, error) {
			if dsw.PluginExists(socketPath) {
				return true, nil
			}
			return false, nil
		},
	)
	if err != nil {
		t.Fatalf("Timed out waiting for plugin to be added to desired state of world cache:\n%s.", socketPath)
	}
}

func waitForUnregistration(
	t *testing.T,
	socketPath string,
	dsw cache.DesiredStateOfWorld) {
	err := retryWithExponentialBackOff(
		time.Duration(500*time.Millisecond),
		func() (bool, error) {
			if !dsw.PluginExists(socketPath) {
				return true, nil
			}
			return false, nil
		},
	)

	if err != nil {
		t.Fatalf("Timed out waiting for plugin to be unregistered:\n%s.", socketPath)
	}
}

func retryWithExponentialBackOff(initialDuration time.Duration, fn wait.ConditionFunc) error {
	backoff := wait.Backoff{
		Duration: initialDuration,
		Factor:   3,
		Jitter:   0,
		Steps:    6,
	}
	return wait.ExponentialBackoff(backoff, fn)
}

func TestPluginRegistration(t *testing.T) {
	socketDir := initTempDir(t)
	defer os.RemoveAll(socketDir)

	dsw := cache.NewDesiredStateOfWorld()
	asw := cache.NewActualStateOfWorld()
	newWatcher(t, socketDir, dsw, asw, wait.NeverStop)

	for i := 0; i < 10; i++ {
		socketPath := filepath.Join(socketDir, fmt.Sprintf("plugin-%d.sock", i))
		pluginName := fmt.Sprintf("example-plugin-%d", i)

		p := NewTestExamplePlugin(pluginName, registerapi.DevicePlugin, socketPath, supportedVersions...)
		require.NoError(t, p.Serve("v1beta1", "v1beta2"))

		pluginInfo := GetPluginInfo(p)
		waitForRegistration(t, pluginInfo.SocketPath, dsw)

		// Check the desired state for plugins
		dswPlugins := dsw.GetPluginsToRegister()
		if len(dswPlugins) != 1 {
			t.Fatalf("TestPluginRegistration: desired state of world length should be 1 but it's %d", len(dswPlugins))
		}

		// Stop the plugin; the plugin should be removed from the desired state of world cache
		require.NoError(t, p.Stop())
		// The following doesn't work when running the unit tests locally: event.Op of plugin watcher won't pick up the delete event
		waitForUnregistration(t, pluginInfo.SocketPath, dsw)
		dswPlugins = dsw.GetPluginsToRegister()
		if len(dswPlugins) != 0 {
			t.Fatalf("TestPluginRegistration: desired state of world length should be 0 but it's %d", len(dswPlugins))
		}
	}
}

func TestPluginRegistrationSameName(t *testing.T) {
	socketDir := initTempDir(t)
	defer os.RemoveAll(socketDir)

	dsw := cache.NewDesiredStateOfWorld()
	asw := cache.NewActualStateOfWorld()
	newWatcher(t, socketDir, dsw, asw, wait.NeverStop)

	// Make 10 plugins with the same name and same type but different socket path;
	// all 10 should be in desired state of world cache
	pluginName := "dep-example-plugin"
	for i := 0; i < 10; i++ {
		socketPath := filepath.Join(socketDir, fmt.Sprintf("plugin-%d.sock", i))
		p := NewTestExamplePlugin(pluginName, registerapi.DevicePlugin, socketPath, supportedVersions...)
		require.NoError(t, p.Serve("v1beta1", "v1beta2"))

		pluginInfo := GetPluginInfo(p)
		waitForRegistration(t, pluginInfo.SocketPath, dsw)

		// Check the desired state for plugins
		dswPlugins := dsw.GetPluginsToRegister()
		if len(dswPlugins) != i+1 {
			t.Fatalf("TestPluginRegistrationSameName: desired state of world length should be %d but it's %d", i+1, len(dswPlugins))
		}
	}
}

func TestPluginReRegistration(t *testing.T) {
	socketDir := initTempDir(t)
	defer os.RemoveAll(socketDir)

	dsw := cache.NewDesiredStateOfWorld()
	asw := cache.NewActualStateOfWorld()
	newWatcher(t, socketDir, dsw, asw, wait.NeverStop)

	// Create a plugin first, we are then going to remove the plugin, update the plugin with a different name
	// and recreate it.
	socketPath := filepath.Join(socketDir, "plugin-reregistration.sock")
	pluginName := "reregister-plugin"
	p := NewTestExamplePlugin(pluginName, registerapi.DevicePlugin, socketPath, supportedVersions...)
	require.NoError(t, p.Serve("v1beta1", "v1beta2"))
	pluginInfo := GetPluginInfo(p)
	lastTimestamp := time.Now()
	waitForRegistration(t, pluginInfo.SocketPath, dsw)

	// Remove this plugin, then recreate it again with a different name for 10 times
	// The updated plugin should be in the desired state of world cache
	for i := 0; i < 10; i++ {
		// Stop the plugin; the plugin should be removed from the desired state of world cache
		// The plugin removal doesn't work when running the unit tests locally: event.Op of plugin watcher won't pick up the delete event
		require.NoError(t, p.Stop())
		waitForUnregistration(t, pluginInfo.SocketPath, dsw)

		// Add the plugin again
		pluginName := fmt.Sprintf("dep-example-plugin-%d", i)
		p := NewTestExamplePlugin(pluginName, registerapi.DevicePlugin, socketPath, supportedVersions...)
		require.NoError(t, p.Serve("v1beta1", "v1beta2"))
		waitForRegistration(t, pluginInfo.SocketPath, dsw)

		// Check the dsw cache. The updated plugin should be the only plugin in it
		dswPlugins := dsw.GetPluginsToRegister()
		if len(dswPlugins) != 1 {
			t.Fatalf("TestPluginReRegistration: desired state of world length should be 1 but it's %d", len(dswPlugins))
		}
		if !dswPlugins[0].Timestamp.After(lastTimestamp) {
			t.Fatalf("TestPluginReRegistration: for plugin %s timestamp of plugin is not updated", pluginName)
		}
		lastTimestamp = dswPlugins[0].Timestamp
	}
}

func TestPluginRegistrationAtKubeletStart(t *testing.T) {
	socketDir := initTempDir(t)
	defer os.RemoveAll(socketDir)

	plugins := make([]*examplePlugin, 10)

	for i := 0; i < len(plugins); i++ {
		socketPath := filepath.Join(socketDir, fmt.Sprintf("plugin-%d.sock", i))
		pluginName := fmt.Sprintf("example-plugin-%d", i)

		p := NewTestExamplePlugin(pluginName, registerapi.DevicePlugin, socketPath, supportedVersions...)
		require.NoError(t, p.Serve("v1beta1", "v1beta2"))
		defer func(p *examplePlugin) {
			require.NoError(t, p.Stop())
		}(p)

		plugins[i] = p
	}

	dsw := cache.NewDesiredStateOfWorld()
	asw := cache.NewActualStateOfWorld()
	newWatcher(t, socketDir, dsw, asw, wait.NeverStop)

	var wg sync.WaitGroup
	for i := 0; i < len(plugins); i++ {
		wg.Add(1)
		go func(p *examplePlugin) {
			defer wg.Done()

			pluginInfo := GetPluginInfo(p)
			// Validate that the plugin is in the desired state cache
			waitForRegistration(t, pluginInfo.SocketPath, dsw)
		}(plugins[i])
	}

	c := make(chan struct{})
	go func() {
		defer close(c)
		wg.Wait()
	}()

	select {
	case <-c:
		return
	case <-time.After(wait.ForeverTestTimeout):
		t.Fatalf("Timeout while waiting for the plugin registration status")
	}
}

func newWatcher(t *testing.T, socketDir string, desiredStateOfWorldCache cache.DesiredStateOfWorld, actualStateOfWorldCache cache.ActualStateOfWorld, stopCh <-chan struct{}) *Watcher {
	w := NewWatcher(socketDir, desiredStateOfWorldCache, actualStateOfWorldCache)
	require.NoError(t, w.Start(stopCh))

	return w
}
