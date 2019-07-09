// +build windows

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

package app

import (
	"k8s.io/kubernetes/pkg/windows/service"

	"sigs.k8s.io/legacyflag/pkg/legacyflag"
)

const (
	serviceName = "kube-proxy"
)

func initForOS(windowsService bool) error {
	if windowsService {
		return service.InitService(serviceName)
	}
	return nil
}

func (o *Options) addOSFlags(fs *legacyflag.FlagSet) {
	fs.BoolVar("windows-service", o.WindowsService, "Enable Windows Service Control Manager API integration").Set(&o.WindowsService)
	fs.StringVar("source-vip", o.config.Winkernel.SourceVip, "The IP address of the source VIP for non-DSR.").Set(&o.config.Winkernel.SourceVip)
	fs.StringVar("network-name", o.config.Winkernel.NetworkName, "The name of the cluster network.").Set(&o.config.Winkernel.NetworkName)
	fs.BoolVar("enable-dsr", o.config.Winkernel.EnableDSR, "If true make kube-proxy apply DSR policies for service VIP").Set(&o.config.Winkernel.EnableDSR)
}
