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

package reconciler

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/record"
	registerapi "k8s.io/kubelet/pkg/apis/pluginregistration/v1"
	"k8s.io/kubernetes/pkg/kubelet/pluginmanager/cache"
	"k8s.io/kubernetes/pkg/kubelet/pluginmanager/operationexecutor"
	"k8s.io/kubernetes/pkg/kubelet/pluginmanager/pluginwatcher"
)

const (
	// reconcilerLoopSleepDuration is the amount of time the reconciler loop
	// waits between successive executions
	reconcilerLoopSleepDuration time.Duration = 1 * time.Nanosecond
)

var (
	socketDir         string
	supportedVersions = []string{"v1beta1", "v1beta2"}
)

func init() {
	d, err := os.MkdirTemp("", "reconciler_test")
	if err != nil {
		panic(fmt.Sprintf("Could not create a temp directory: %s", d))
	}
	socketDir = d
}

func cleanup(t *testing.T) {
	require.NoError(t, os.RemoveAll(socketDir))
	os.MkdirAll(socketDir, 0755)
}

func runReconciler(reconciler Reconciler) {
	go reconciler.Run(wait.NeverStop)
}

func waitForRegistration(
	t *testing.T,
	socketPath string,
	expectedUUID types.UID,
	asw cache.ActualStateOfWorld) {
	err := retryWithExponentialBackOff(
		time.Duration(500*time.Millisecond),
		func() (bool, error) {
			registeredPlugins := asw.GetRegisteredPlugins()
			for _, plugin := range registeredPlugins {
				if plugin.SocketPath == socketPath && plugin.UUID == expectedUUID {
					return true, nil
				}
			}
			return false, nil
		},
	)
	if err != nil {
		t.Fatalf("Timed out waiting for plugin to be registered:\n%s.", socketPath)
	}
}

func waitForUnregistration(
	t *testing.T,
	socketPath string,
	asw cache.ActualStateOfWorld) {
	err := retryWithExponentialBackOff(
		time.Duration(500*time.Millisecond),
		func() (bool, error) {
			registeredPlugins := asw.GetRegisteredPlugins()
			for _, plugin := range registeredPlugins {
				if plugin.SocketPath == socketPath {
					return false, nil
				}
			}
			return true, nil
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

type DummyImpl struct{}

func NewDummyImpl() *DummyImpl {
	return &DummyImpl{}
}

// ValidatePlugin is a dummy implementation
func (d *DummyImpl) ValidatePlugin(pluginName string, endpoint string, versions []string) error {
	return nil
}

// RegisterPlugin is a dummy implementation
func (d *DummyImpl) RegisterPlugin(pluginName string, endpoint string, versions []string, pluginClientTimeout *time.Duration) error {
	return nil
}

// DeRegisterPlugin is a dummy implementation
func (d *DummyImpl) DeRegisterPlugin(pluginName, endpoint string) {
}

// Calls Run()
// Verifies that asw and dsw have no plugins
func Test_Run_Positive_DoNothing(t *testing.T) {
	defer cleanup(t)

	dsw := cache.NewDesiredStateOfWorld()
	asw := cache.NewActualStateOfWorld()
	fakeRecorder := &record.FakeRecorder{}
	oex := operationexecutor.NewOperationExecutor(operationexecutor.NewOperationGenerator(
		fakeRecorder,
	))
	reconciler := NewReconciler(
		oex,
		reconcilerLoopSleepDuration,
		dsw,
		asw,
	)
	// Act
	runReconciler(reconciler)

	// Get dsw and asw plugins; they should both be empty
	if len(asw.GetRegisteredPlugins()) != 0 {
		t.Fatalf("Test_Run_Positive_DoNothing: actual state of world should be empty but it's not")
	}
	if len(dsw.GetPluginsToRegister()) != 0 {
		t.Fatalf("Test_Run_Positive_DoNothing: desired state of world should be empty but it's not")
	}
}

// Populates desiredStateOfWorld cache with one plugin.
// Calls Run()
// Verifies the actual state of world contains that plugin
func Test_Run_Positive_Register(t *testing.T) {
	defer cleanup(t)

	dsw := cache.NewDesiredStateOfWorld()
	asw := cache.NewActualStateOfWorld()
	di := NewDummyImpl()
	fakeRecorder := &record.FakeRecorder{}
	oex := operationexecutor.NewOperationExecutor(operationexecutor.NewOperationGenerator(
		fakeRecorder,
	))
	reconciler := NewReconciler(
		oex,
		reconcilerLoopSleepDuration,
		dsw,
		asw,
	)
	reconciler.AddHandler(registerapi.DevicePlugin, cache.PluginHandler(di))

	// Start the reconciler to fill ASW.
	stopChan := make(chan struct{})
	defer close(stopChan)
	go reconciler.Run(stopChan)
	socketPath := filepath.Join(socketDir, "plugin.sock")
	pluginName := fmt.Sprintf("example-plugin")
	p := pluginwatcher.NewTestExamplePlugin(pluginName, registerapi.DevicePlugin, socketPath, supportedVersions...)
	require.NoError(t, p.Serve("v1beta1", "v1beta2"))
	defer func() {
		require.NoError(t, p.Stop())
	}()
	dsw.AddOrUpdatePlugin(socketPath)
	plugins := dsw.GetPluginsToRegister()
	waitForRegistration(t, socketPath, plugins[0].UUID, asw)

	// Get asw plugins; it should contain the added plugin
	aswPlugins := asw.GetRegisteredPlugins()
	if len(aswPlugins) != 1 {
		t.Fatalf("Test_Run_Positive_Register: actual state of world length should be one but it's %d", len(aswPlugins))
	}
	if aswPlugins[0].SocketPath != socketPath {
		t.Fatalf("Test_Run_Positive_Register: expected\n%s\nin actual state of world, but got\n%v\n", socketPath, aswPlugins[0])
	}
}

// Populates desiredStateOfWorld cache with one plugin
// Calls Run()
// Verifies there is one plugin now in actual state of world.
// Deletes plugin from desired state of world.
// Verifies that plugin no longer exists in actual state of world.
func Test_Run_Positive_RegisterThenUnregister(t *testing.T) {
	defer cleanup(t)

	dsw := cache.NewDesiredStateOfWorld()
	asw := cache.NewActualStateOfWorld()
	di := NewDummyImpl()
	fakeRecorder := &record.FakeRecorder{}
	oex := operationexecutor.NewOperationExecutor(operationexecutor.NewOperationGenerator(
		fakeRecorder,
	))
	reconciler := NewReconciler(
		oex,
		reconcilerLoopSleepDuration,
		dsw,
		asw,
	)
	reconciler.AddHandler(registerapi.DevicePlugin, cache.PluginHandler(di))

	// Start the reconciler to fill ASW.
	stopChan := make(chan struct{})
	defer close(stopChan)
	go reconciler.Run(stopChan)

	socketPath := filepath.Join(socketDir, "plugin.sock")
	pluginName := fmt.Sprintf("example-plugin")
	p := pluginwatcher.NewTestExamplePlugin(pluginName, registerapi.DevicePlugin, socketPath, supportedVersions...)
	require.NoError(t, p.Serve("v1beta1", "v1beta2"))
	dsw.AddOrUpdatePlugin(socketPath)
	plugins := dsw.GetPluginsToRegister()
	waitForRegistration(t, socketPath, plugins[0].UUID, asw)

	// Get asw plugins; it should contain the added plugin
	aswPlugins := asw.GetRegisteredPlugins()
	if len(aswPlugins) != 1 {
		t.Fatalf("Test_Run_Positive_RegisterThenUnregister: actual state of world length should be one but it's %d", len(aswPlugins))
	}
	if aswPlugins[0].SocketPath != socketPath {
		t.Fatalf("Test_Run_Positive_RegisterThenUnregister: expected\n%s\nin actual state of world, but got\n%v\n", socketPath, aswPlugins[0])
	}

	dsw.RemovePlugin(socketPath)
	os.Remove(socketPath)
	waitForUnregistration(t, socketPath, asw)

	// Get asw plugins; it should no longer contain the added plugin
	aswPlugins = asw.GetRegisteredPlugins()
	if len(aswPlugins) != 0 {
		t.Fatalf("Test_Run_Positive_RegisterThenUnregister: actual state of world length should be zero but it's %d", len(aswPlugins))
	}
}

// Populates desiredStateOfWorld cache with one plugin
// Calls Run()
// Then update the timestamp of the plugin
// Verifies that the plugin is reregistered.
// Verifies the plugin with updated timestamp now in actual state of world.
func Test_Run_Positive_ReRegister(t *testing.T) {
	defer cleanup(t)

	dsw := cache.NewDesiredStateOfWorld()
	asw := cache.NewActualStateOfWorld()
	di := NewDummyImpl()
	fakeRecorder := &record.FakeRecorder{}
	oex := operationexecutor.NewOperationExecutor(operationexecutor.NewOperationGenerator(
		fakeRecorder,
	))
	reconciler := NewReconciler(
		oex,
		reconcilerLoopSleepDuration,
		dsw,
		asw,
	)
	reconciler.AddHandler(registerapi.DevicePlugin, cache.PluginHandler(di))

	// Start the reconciler to fill ASW.
	stopChan := make(chan struct{})
	defer close(stopChan)
	go reconciler.Run(stopChan)

	socketPath := filepath.Join(socketDir, "plugin2.sock")
	pluginName := fmt.Sprintf("example-plugin2")
	p := pluginwatcher.NewTestExamplePlugin(pluginName, registerapi.DevicePlugin, socketPath, supportedVersions...)
	require.NoError(t, p.Serve("v1beta1", "v1beta2"))
	dsw.AddOrUpdatePlugin(socketPath)
	plugins := dsw.GetPluginsToRegister()
	waitForRegistration(t, socketPath, plugins[0].UUID, asw)

	// Add the plugin again to update the timestamp
	dsw.AddOrUpdatePlugin(socketPath)
	// This should trigger a deregistration and a regitration
	// The process of unregistration and reregistration can happen so fast that
	// we are not able to catch it with waitForUnregistration, so here we are checking
	// the plugin has an updated timestamp.
	plugins = dsw.GetPluginsToRegister()
	waitForRegistration(t, socketPath, plugins[0].UUID, asw)

	// Get asw plugins; it should contain the added plugin
	aswPlugins := asw.GetRegisteredPlugins()
	if len(aswPlugins) != 1 {
		t.Fatalf("Test_Run_Positive_RegisterThenUnregister: actual state of world length should be one but it's %d", len(aswPlugins))
	}
	if aswPlugins[0].SocketPath != socketPath {
		t.Fatalf("Test_Run_Positive_RegisterThenUnregister: expected\n%s\nin actual state of world, but got\n%v\n", socketPath, aswPlugins[0])
	}
}
