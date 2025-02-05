//go:build linux
// +build linux

/*
Copyright 2020 The Kubernetes Authors.

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

// utils_linux_test.go is named with _test.go because the functions in this file use global variables from test files,
// and this file only provides helper functions for the e2e module.
package e2enode

import (
	"fmt"

	libcontainercgroups "github.com/opencontainers/runc/libcontainer/cgroups"
)

// IsCgroup2UnifiedMode returns whether we are running in cgroup v2 unified mode.
func IsCgroup2UnifiedMode() bool {
	return libcontainercgroups.IsCgroup2UnifiedMode()
}

// addCRIProxyInjector registers an injector function for the CRIProxy.
func addCRIProxyInjector(injector func(apiName string) error) error {
	if e2eCriProxy == nil {
		return fmt.Errorf("failed to add injector because the CRI Proxy is undefined")
	}
	e2eCriProxy.AddInjector(injector)
	return nil
}

// resetCRIProxyInjector resets all injector functions for the CRIProxy.
func resetCRIProxyInjector() error {
	if e2eCriProxy == nil {
		return fmt.Errorf("failed to reset injector because the CRI Proxy is undefined")
	}
	e2eCriProxy.ResetInjectors()
	return nil
}
