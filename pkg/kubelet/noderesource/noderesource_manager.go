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

package noderesource

import (
	"reflect"
	"time"

	cadvisorapi "github.com/google/cadvisor/info/v1"

	"k8s.io/apimachinery/pkg/util/wait"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/features"
	"k8s.io/kubernetes/pkg/kubelet/cadvisor"
	"k8s.io/kubernetes/pkg/kubelet/server"
)

type Manager interface {
	Start()
	MachineInfo() <-chan *cadvisorapi.MachineInfo
}

type manager struct {
	// server kubelet methods
	host server.HostInterface
	// cadvisor client
	cadvisor cadvisor.Interface
	// channel of MachineInfo
	machineInfo chan *cadvisorapi.MachineInfo
}

// managerStub is a fake node resource managerImpl.
type managerStub struct {
	// channel of MachineInfo
	machineInfo chan *cadvisorapi.MachineInfo
}

func NewNodeResourceManager(host server.HostInterface, cadvisor cadvisor.Interface) Manager {
	if !utilfeature.DefaultFeatureGate.Enabled(features.NodeResourceHotPlug) {
		return &managerStub{machineInfo: make(chan *cadvisorapi.MachineInfo)}
	}
	return &manager{
		host:        host,
		cadvisor:    cadvisor,
		machineInfo: make(chan *cadvisorapi.MachineInfo),
	}
}

func (m *manager) Start() {
	// starting to fetch machine info from cadvisor cache.
	klog.Info("Starting node resource manager")
	go wait.Forever(func() {
		klog.Info("Fetching machine info")
		machineInfo, err := m.cadvisor.MachineInfo()
		if err != nil {
			klog.ErrorS(err, "Error fetching machine info")
		} else {
			cachedMachineInfo, _ := m.host.GetCachedMachineInfo()
			// Avoid collector collects it as a timestamped metric
			// See PR #95210 and #97006 for more details.
			machineInfo.Timestamp = time.Time{}
			if !reflect.DeepEqual(cachedMachineInfo, machineInfo) {
				klog.InfoS("Observed change in machine info", "cachedMachineInfo", cachedMachineInfo,
					"machineInfo", machineInfo)
				m.machineInfo <- machineInfo
			}
		}
		// cadvisor updates its cache in `update_machine_info_interval` defaulted to 5 minutes.
	}, 5*time.Minute)
}

func (m *manager) MachineInfo() <-chan *cadvisorapi.MachineInfo {
	return m.machineInfo
}

func (m *managerStub) Start() {
	return
}

func (m *managerStub) MachineInfo() <-chan *cadvisorapi.MachineInfo {
	return m.machineInfo
}
