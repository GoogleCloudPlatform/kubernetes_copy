//go:build linux

/*
Copyright 2024 The Kubernetes Authors.

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

package kubelet

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	v1 "k8s.io/api/core/v1"
	"k8s.io/kubernetes/pkg/kubelet/cm"
	"k8s.io/kubernetes/pkg/kubelet/cm/util"
	"k8s.io/kubernetes/pkg/kubelet/events"
	"k8s.io/kubernetes/pkg/kubelet/metrics"
)

func (kl *Kubelet) cgroupVersionCheck() error {
	cgroupVersion := kl.containerManager.GetNodeConfig().CgroupVersion
	metrics.CgroupVersion.Set(float64(cgroupVersion))
	switch cgroupVersion {
	case 1:
		kl.recorder.Eventf(kl.nodeRef, nil, v1.EventTypeWarning, events.CgroupV1, "WarningCgroupv1", cm.CgroupV1MaintenanceModeWarning)
		return errors.New(cm.CgroupV1MaintenanceModeWarning)
	case 2:
		cpustat := filepath.Join(util.CgroupRoot, "cpu.stat")
		if _, err := os.Stat(cpustat); os.IsNotExist(err) {
			// if `/sys/fs/cgroup/cpu.stat` does not exist, log a warning
			return errors.New(cm.CgroupV2KernelWarning)
		}
	default:
		return fmt.Errorf("unsupported cgroup version: %d", cgroupVersion)
	}
	return nil
}
