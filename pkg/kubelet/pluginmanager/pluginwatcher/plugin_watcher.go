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
	"fmt"
	"os"
	"strings"

	"github.com/fsnotify/fsnotify"
	"k8s.io/klog/v2"

	"k8s.io/kubernetes/pkg/kubelet/pluginmanager/cache"
	"k8s.io/kubernetes/pkg/kubelet/util"
	utilfs "k8s.io/kubernetes/pkg/util/filesystem"
)

// Watcher is the plugin watcher
type Watcher struct {
	path                string
	fs                  utilfs.Filesystem
	fsWatcher           *fsnotify.Watcher
	desiredStateOfWorld cache.DesiredStateOfWorld
	actualStateOfWorld  cache.ActualStateOfWorld
}

// NewWatcher provides a new watcher for socket registration
func NewWatcher(sockDir string, desiredStateOfWorld cache.DesiredStateOfWorld, actualStateOfWorld cache.ActualStateOfWorld) *Watcher {
	return &Watcher{
		path:                sockDir,
		fs:                  &utilfs.DefaultFs{},
		desiredStateOfWorld: desiredStateOfWorld,
		actualStateOfWorld:  actualStateOfWorld,
	}
}

// Start watches for the creation and deletion of plugin sockets at the path
func (w *Watcher) Start(stopCh <-chan struct{}) error {
	klog.V(2).InfoS("Plugin Watcher Start", "path", w.path)

	// Creating the directory to be watched if it doesn't exist yet,
	// and walks through the directory to discover the existing plugins.
	if err := w.init(); err != nil {
		return err
	}

	fsWatcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("failed to start plugin fsWatcher, err: %v", err)
	}
	w.fsWatcher = fsWatcher

	// Traverse plugin dir and add filesystem watchers before starting the plugin processing goroutine.
	if err := w.traversePluginDir(w.path); err != nil {
		klog.ErrorS(err, "Failed to traverse plugin socket path", "path", w.path)
	}

	go func(fsWatcher *fsnotify.Watcher) {
		for {
			select {
			case event := <-fsWatcher.Events:
				//TODO: Handle errors by taking corrective measures
				if event.Has(fsnotify.Create) {
					err := w.handleCreateEvent(event)
					if err != nil {
						klog.ErrorS(err, "Error when handling create event", "event", event)
					}
				} else if event.Has(fsnotify.Remove) {
					w.handleDeleteEvent(event)
				}
				continue
			case err := <-fsWatcher.Errors:
				if err != nil {
					klog.ErrorS(err, "FsWatcher received error")
				}
				continue
			case <-stopCh:
				w.fsWatcher.Close()
				return
			}
		}
	}(fsWatcher)

	return nil
}

func (w *Watcher) init() error {
	klog.V(4).InfoS("Ensuring Plugin directory", "path", w.path)

	if err := w.fs.MkdirAll(w.path, 0755); err != nil {
		return fmt.Errorf("error (re-)creating root %s: %v", w.path, err)
	}

	return nil
}

// Walks through the plugin directory discover any existing plugin sockets.
// Ignore all errors except root dir not being walkable
func (w *Watcher) traversePluginDir(dir string) error {
	// watch the new dir
	err := w.fsWatcher.Add(dir)
	if err != nil {
		return fmt.Errorf("failed to watch %s, err: %v", w.path, err)
	}
	// traverse existing children in the dir
	return w.fs.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			if path == dir {
				return fmt.Errorf("error accessing path: %s error: %v", path, err)
			}

			klog.ErrorS(err, "Error accessing path", "path", path)
			return nil
		}

		// do not call fsWatcher.Add twice on the root dir to avoid potential problems.
		if path == dir {
			return nil
		}

		mode := info.Mode()
		if mode.IsDir() {
			if err := w.fsWatcher.Add(path); err != nil {
				return fmt.Errorf("failed to watch %s, err: %v", path, err)
			}
		} else if isSocket, _ := util.IsUnixDomainSocket(path); isSocket {
			event := fsnotify.Event{
				Name: path,
				Op:   fsnotify.Create,
			}
			//TODO: Handle errors by taking corrective measures
			if err := w.handleCreateEvent(event); err != nil {
				klog.ErrorS(err, "Error when handling create", "event", event)
			}
		} else {
			klog.V(5).InfoS("Ignoring file", "path", path, "mode", mode)
		}

		return nil
	})
}

// Handle filesystem notify event.
// Files names:
// - MUST NOT start with a '.'
func (w *Watcher) handleCreateEvent(event fsnotify.Event) error {
	klog.V(6).InfoS("Handling create event", "event", event)

	fi, err := getStat(event)
	if err != nil {
		return fmt.Errorf("stat file %s failed: %v", event.Name, err)
	}

	if strings.HasPrefix(fi.Name(), ".") {
		klog.V(5).InfoS("Ignoring file (starts with '.')", "path", fi.Name())
		return nil
	}

	if !fi.IsDir() {
		isSocket, err := util.IsUnixDomainSocket(util.NormalizePath(event.Name))
		if err != nil {
			return fmt.Errorf("failed to determine if file: %s is a unix domain socket: %v", event.Name, err)
		}
		if !isSocket {
			klog.V(5).InfoS("Ignoring non socket file", "path", fi.Name())
			return nil
		}

		return w.handlePluginRegistration(event.Name)
	}

	return w.traversePluginDir(event.Name)
}

func (w *Watcher) handlePluginRegistration(socketPath string) error {
	socketPath = getSocketPath(socketPath)
	// Update desired state of world list of plugins
	// If the socket path does exist in the desired world cache, there's still
	// a possibility that it has been deleted and recreated again before it is
	// removed from the desired world cache, so we still need to call AddOrUpdatePlugin
	// in this case to update the timestamp
	klog.V(2).InfoS("Adding socket path or updating timestamp to desired state cache", "path", socketPath)
	err := w.desiredStateOfWorld.AddOrUpdatePlugin(socketPath)
	if err != nil {
		return fmt.Errorf("error adding socket path %s or updating timestamp to desired state cache: %v", socketPath, err)
	}
	return nil
}

func (w *Watcher) handleDeleteEvent(event fsnotify.Event) {
	klog.V(6).InfoS("Handling delete event", "event", event)

	socketPath := event.Name
	klog.V(2).InfoS("Removing socket path from desired state cache", "path", socketPath)
	w.desiredStateOfWorld.RemovePlugin(socketPath)
}
