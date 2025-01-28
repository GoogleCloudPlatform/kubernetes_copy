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

package pluginmanager

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/record"
	registerapi "k8s.io/kubelet/pkg/apis/pluginregistration/v1"

	"k8s.io/kubernetes/pkg/kubelet/config"
	"k8s.io/kubernetes/pkg/kubelet/pluginmanager/pluginwatcher"
)

var (
	socketDir         string
	supportedVersions = []string{"v1beta1", "v1beta2"}
)

type fakePluginHandler struct {
	events []string
	sync.RWMutex
}

func newFakePluginHandler() *fakePluginHandler {
	return &fakePluginHandler{}
}

// ValidatePlugin is a fake method
func (f *fakePluginHandler) ValidatePlugin(pluginName string, endpoint string, versions []string) error {
	f.Lock()
	defer f.Unlock()
	f.events = append(f.events, "validate "+pluginName+" "+endpoint)
	return nil
}

// RegisterPlugin is a fake method
func (f *fakePluginHandler) RegisterPlugin(pluginName, endpoint string, versions []string, pluginClientTimeout *time.Duration) error {
	f.Lock()
	defer f.Unlock()
	f.events = append(f.events, "register "+pluginName+" "+endpoint)
	return nil
}

// DeRegisterPlugin is a fake method
func (f *fakePluginHandler) DeRegisterPlugin(pluginName, endpoint string) {
	f.Lock()
	defer f.Unlock()
	f.events = append(f.events, "deregister "+pluginName+" "+endpoint)
}

func (f *fakePluginHandler) Reset() {
	f.Lock()
	defer f.Unlock()
	f.events = nil
}

func init() {
	d, err := os.MkdirTemp("", "plugin_manager_test")
	if err != nil {
		panic(fmt.Sprintf("Could not create a temp directory: %s", d))
	}

	socketDir = d
}

func cleanup(t *testing.T) {
	require.NoError(t, os.RemoveAll(socketDir))
	os.MkdirAll(socketDir, 0755)
}

func waitForRegistration(t *testing.T, fakePluginHandler *fakePluginHandler, pluginName, endpoint string) {
	t.Helper()
	waitFor(t, fakePluginHandler,
		[]string{"validate " + pluginName + " " + endpoint, "register " + pluginName + " " + endpoint},
		"Timed out waiting for plugin to be added to actual state of world cache.",
	)
}

func waitForDeRegistration(t *testing.T, fakePluginHandler *fakePluginHandler, pluginName, endpoint string) {
	t.Helper()
	waitFor(t, fakePluginHandler,
		[]string{"deregister " + pluginName + " " + endpoint},
		"Timed out waiting for plugin to be removed from actual state of the world cache.",
	)
}

func waitFor(t *testing.T, fakePluginHandler *fakePluginHandler, expected []string, what string) {
	t.Helper()
	err := retryWithExponentialBackOff(
		100*time.Millisecond,
		func() (bool, error) {
			fakePluginHandler.Lock()
			defer fakePluginHandler.Unlock()
			if reflect.DeepEqual(fakePluginHandler.events, expected) {
				return true, nil
			}
			t.Logf("expected %#v, got %#v, will retry", expected, fakePluginHandler.events)
			return false, nil
		},
	)
	if err != nil {
		t.Fatal(what)
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

func TestPluginManager(t *testing.T) {
	defer cleanup(t)

	pluginManager := newTestPluginManager(socketDir)

	// Start the plugin manager
	stopChan := make(chan struct{})
	defer close(stopChan)
	go func() {
		sourcesReady := config.NewSourcesReady(func(_ sets.Set[string]) bool { return true })
		pluginManager.Run(sourcesReady, stopChan)
	}()

	// Add handler for device plugin
	fakeHandler := newFakePluginHandler()
	pluginManager.AddHandler(registerapi.DevicePlugin, fakeHandler)

	const maxDepth = 3
	// Make sure the plugin manager is aware of the socket in subdirectories
	for i := 0; i < maxDepth; i++ {
		fakeHandler.Reset()
		pluginDir := socketDir

		for j := 0; j < i; j++ {
			pluginDir = filepath.Join(pluginDir, strconv.Itoa(j))
		}
		require.NoError(t, os.MkdirAll(pluginDir, os.ModePerm))
		socketPath := filepath.Join(pluginDir, fmt.Sprintf("plugin-%d.sock", i))

		// Add a new plugin
		pluginName := fmt.Sprintf("example-plugin-%d", i)
		p := pluginwatcher.NewTestExamplePlugin(pluginName, registerapi.DevicePlugin, socketPath, supportedVersions...)
		require.NoError(t, p.Serve("v1beta1", "v1beta2"))

		// Verify that the plugin is registered
		waitForRegistration(t, fakeHandler, pluginName, socketPath)

		// And unregister.
		fakeHandler.Reset()
		require.NoError(t, p.Stop())
		waitForDeRegistration(t, fakeHandler, pluginName, socketPath)
	}
}

func newTestPluginManager(sockDir string) PluginManager {
	pm := NewPluginManager(
		sockDir,
		&record.FakeRecorder{},
	)
	return pm
}
