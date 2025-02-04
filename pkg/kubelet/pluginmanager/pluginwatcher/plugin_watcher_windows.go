//go:build windows
// +build windows

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

package pluginwatcher

import (
	"github.com/fsnotify/fsnotify"
	"k8s.io/kubernetes/pkg/kubelet/util"
	"os"
)

func getStat(event fsnotify.Event) (os.FileInfo, error) {
	fi, err := os.Stat(event.Name)
	// TODO: This is a workaround for Windows 20H2 issue for os.Stat(). Please see
	// microsoft/Windows-Containers#97 for details.
	// Once the issue is resolved, the following os.Lstat() is not needed.
	if err != nil {
		fi, err = os.Lstat(event.Name)
	}

	return fi, err
}

var getSocketPath = util.NormalizePath
