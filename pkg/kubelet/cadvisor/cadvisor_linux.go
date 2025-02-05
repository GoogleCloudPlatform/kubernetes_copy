//go:build linux
// +build linux

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
	"flag"
	"fmt"
	"net/http"
	"os"
	"path"
	"time"

	// Register supported container handlers.
	_ "github.com/google/cadvisor/container/containerd/install"
	_ "github.com/google/cadvisor/container/crio/install"
	_ "github.com/google/cadvisor/container/systemd/install"

	"github.com/google/cadvisor/cache/memory"
	cadvisormetrics "github.com/google/cadvisor/container"
	cadvisorapi "github.com/google/cadvisor/info/v1"
	cadvisorapiv2 "github.com/google/cadvisor/info/v2"
	"github.com/google/cadvisor/manager"
	"github.com/google/cadvisor/utils/sysfs"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
)

type cadvisorClient struct {
	imageFsInfoProvider ImageFsInfoProvider
	rootPath            string
	manager.Manager
}

var _ Interface = new(cadvisorClient)
var MaxHouseKeepingInterval int = 0

// TODO(vmarmol): Make configurable.
// The amount of time for which to keep stats in memory.
const statsCacheDuration = 2 * time.Minute
const maxHousekeepingInterval = 15 * time.Second
const defaultHousekeepingInterval = 10 * time.Second
const allowDynamicHousekeeping = true

func init() {
	// Override cAdvisor flag defaults.
	flagOverrides := map[string]string{
		// Override the default cAdvisor housekeeping interval.
		"housekeeping_interval": defaultHousekeepingInterval.String(),
		// Disable event storage by default.
		"event_storage_event_limit": "default=0",
		"event_storage_age_limit":   "default=0",
	}
	for name, defaultValue := range flagOverrides {
		if f := flag.Lookup(name); f != nil {
			f.DefValue = defaultValue
			f.Value.Set(defaultValue)
		} else {
			ctx := context.Background()
			klog.FromContext(ctx).Error(nil, "Expected cAdvisor flag not found", "flag", name)
		}
	}
}

// New creates a new cAdvisor Interface for linux systems.
func New(imageFsInfoProvider ImageFsInfoProvider, rootPath string, cgroupRoots []string, usingLegacyStats, localStorageCapacityIsolation bool) (Interface, error) {
	sysFs := sysfs.NewRealSysFs()

	includedMetrics := cadvisormetrics.MetricSet{
		cadvisormetrics.CpuUsageMetrics:     struct{}{},
		cadvisormetrics.MemoryUsageMetrics:  struct{}{},
		cadvisormetrics.CpuLoadMetrics:      struct{}{},
		cadvisormetrics.DiskIOMetrics:       struct{}{},
		cadvisormetrics.NetworkUsageMetrics: struct{}{},
		cadvisormetrics.AppMetrics:          struct{}{},
		cadvisormetrics.ProcessMetrics:      struct{}{},
		cadvisormetrics.OOMMetrics:          struct{}{},
	}

	if usingLegacyStats || localStorageCapacityIsolation {
		includedMetrics[cadvisormetrics.DiskUsageMetrics] = struct{}{}
	}

	duration := maxHousekeepingInterval
	if MaxHouseKeepingInterval != 0 {
		duration = MaxHouseKeepingInterval * time.Second
	}
	housekeepingConfig := manager.HousekeepingConfig{
		Interval:     &duration,
		AllowDynamic: ptr.To(allowDynamicHousekeeping),
	}

	// Create the cAdvisor container manager.
	m, err := manager.New(memory.New(statsCacheDuration, nil), sysFs, housekeepingConfig, includedMetrics, http.DefaultClient, cgroupRoots, nil /* containerEnvMetadataWhiteList */, "" /* perfEventsFile */, time.Duration(0) /*resctrlInterval*/)
	if err != nil {
		return nil, err
	}

	if _, err := os.Stat(rootPath); err != nil {
		if os.IsNotExist(err) {
			if err := os.MkdirAll(path.Clean(rootPath), 0750); err != nil {
				return nil, fmt.Errorf("error creating root directory %q: %v", rootPath, err)
			}
		} else {
			return nil, fmt.Errorf("failed to Stat %q: %v", rootPath, err)
		}
	}

	return &cadvisorClient{
		imageFsInfoProvider: imageFsInfoProvider,
		rootPath:            rootPath,
		Manager:             m,
	}, nil
}

func (cc *cadvisorClient) Start() error {
	return cc.Manager.Start()
}

func (cc *cadvisorClient) ContainerInfoV2(name string, options cadvisorapiv2.RequestOptions) (map[string]cadvisorapiv2.ContainerInfo, error) {
	return cc.GetContainerInfoV2(name, options)
}

func (cc *cadvisorClient) VersionInfo() (*cadvisorapi.VersionInfo, error) {
	return cc.GetVersionInfo()
}

func (cc *cadvisorClient) MachineInfo() (*cadvisorapi.MachineInfo, error) {
	return cc.GetMachineInfo()
}

func (cc *cadvisorClient) ImagesFsInfo(ctx context.Context) (cadvisorapiv2.FsInfo, error) {
	label, err := cc.imageFsInfoProvider.ImageFsInfoLabel()
	if err != nil {
		return cadvisorapiv2.FsInfo{}, err
	}
	return cc.getFsInfo(ctx, label)
}

func (cc *cadvisorClient) RootFsInfo() (cadvisorapiv2.FsInfo, error) {
	return cc.GetDirFsInfo(cc.rootPath)
}

func (cc *cadvisorClient) getFsInfo(ctx context.Context, label string) (cadvisorapiv2.FsInfo, error) {
	res, err := cc.GetFsInfo(label)
	if err != nil {
		return cadvisorapiv2.FsInfo{}, err
	}
	if len(res) == 0 {
		return cadvisorapiv2.FsInfo{}, fmt.Errorf("failed to find information for the filesystem labeled %q", label)
	}
	// TODO(vmarmol): Handle this better when a label has more than one image filesystem.
	if len(res) > 1 {
		klog.FromContext(ctx).Info("More than one filesystem labeled. Only using the first one", "label", label, "fileSystem", res)
	}

	return res[0], nil
}

func (cc *cadvisorClient) ContainerFsInfo(ctx context.Context) (cadvisorapiv2.FsInfo, error) {
	label, err := cc.imageFsInfoProvider.ContainerFsInfoLabel()
	if err != nil {
		return cadvisorapiv2.FsInfo{}, err
	}
	return cc.getFsInfo(ctx, label)
}
