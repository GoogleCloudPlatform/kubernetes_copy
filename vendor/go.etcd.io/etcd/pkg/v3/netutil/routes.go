// Copyright 2016 The etcd Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build !linux

package netutil

import (
	"fmt"
	"runtime"
)

// GetDefaultHost fetches the a resolvable name that corresponds
// to the machine's default routable interface
func GetDefaultHost() (string, error) {
	return "", fmt.Errorf("default host not supported on %s_%s", runtime.GOOS, runtime.GOARCH)
}

// GetDefaultInterfaces fetches the device name of default routable interface.
func GetDefaultInterfaces() (map[string]uint8, error) {
	return nil, fmt.Errorf("default host not supported on %s_%s", runtime.GOOS, runtime.GOARCH)
}
