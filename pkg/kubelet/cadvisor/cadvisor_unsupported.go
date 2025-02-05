//go:build !linux && !windows
// +build !linux,!windows

/*
Copyright 2015 The Kubernetes Authors.

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

package cadvisor

import (
	"context"
	"errors"

	cadvisorapi "github.com/google/cadvisor/info/v1"
	cadvisorapiv2 "github.com/google/cadvisor/info/v2"
)

type cadvisorUnsupported struct {
}

var _ Interface = new(cadvisorUnsupported)
var MaxHouseKeepingInterval int = 0

// New creates a new cAdvisor Interface for unsupported systems.
func New(imageFsInfoProvider ImageFsInfoProvider, rootPath string, cgroupsRoots []string, usingLegacyStats, localStorageCapacityIsolation bool) (Interface, error) {
	return &cadvisorUnsupported{}, nil
}

var errUnsupported = errors.New("cAdvisor is unsupported in this build")

func (cu *cadvisorUnsupported) Start() error {
	return errUnsupported
}

func (cu *cadvisorUnsupported) ContainerInfoV2(name string, options cadvisorapiv2.RequestOptions) (map[string]cadvisorapiv2.ContainerInfo, error) {
	return nil, errUnsupported
}

func (cu *cadvisorUnsupported) GetRequestedContainersInfo(containerName string, options cadvisorapiv2.RequestOptions) (map[string]*cadvisorapi.ContainerInfo, error) {
	return nil, errUnsupported
}

func (cu *cadvisorUnsupported) MachineInfo() (*cadvisorapi.MachineInfo, error) {
	return nil, errUnsupported
}

func (cu *cadvisorUnsupported) VersionInfo() (*cadvisorapi.VersionInfo, error) {
	return nil, errUnsupported
}

func (cu *cadvisorUnsupported) ImagesFsInfo(context.Context) (cadvisorapiv2.FsInfo, error) {
	return cadvisorapiv2.FsInfo{}, errUnsupported
}

func (cu *cadvisorUnsupported) RootFsInfo() (cadvisorapiv2.FsInfo, error) {
	return cadvisorapiv2.FsInfo{}, errUnsupported
}

func (cu *cadvisorUnsupported) ContainerFsInfo(context.Context) (cadvisorapiv2.FsInfo, error) {
	return cadvisorapiv2.FsInfo{}, errUnsupported
}

func (cu *cadvisorUnsupported) GetDirFsInfo(path string) (cadvisorapiv2.FsInfo, error) {
	return cadvisorapiv2.FsInfo{}, nil
}
