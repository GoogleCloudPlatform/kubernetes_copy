/*
Copyright 2016 The Kubernetes Authors.

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

package sysctl

import (
	"context"
	goruntime "runtime"

	"k8s.io/apimachinery/pkg/util/version"
	"k8s.io/klog/v2"
	utilkernel "k8s.io/kubernetes/pkg/util/kernel"
)

type sysctl struct {
	// the name of sysctl
	name string
	// the minimum kernel version where the sysctl is available
	kernel string
}

var safeSysctls = []sysctl{
	{
		name: "kernel.shm_rmid_forced",
	}, {
		name: "net.ipv4.ip_local_port_range",
	}, {
		name: "net.ipv4.tcp_syncookies",
	}, {
		name: "net.ipv4.ping_group_range",
	}, {
		name: "net.ipv4.ip_unprivileged_port_start",
	}, {
		name:   "net.ipv4.ip_local_reserved_ports",
		kernel: utilkernel.IPLocalReservedPortsNamespacedKernelVersion,
	}, {
		name:   "net.ipv4.tcp_keepalive_time",
		kernel: utilkernel.TCPKeepAliveTimeNamespacedKernelVersion,
	}, {
		name:   "net.ipv4.tcp_fin_timeout",
		kernel: utilkernel.TCPFinTimeoutNamespacedKernelVersion,
	},
	{
		name:   "net.ipv4.tcp_keepalive_intvl",
		kernel: utilkernel.TCPKeepAliveIntervalNamespacedKernelVersion,
	},
	{
		name:   "net.ipv4.tcp_keepalive_probes",
		kernel: utilkernel.TCPKeepAliveProbesNamespacedKernelVersion,
	},
	{
		name:   "net.ipv4.tcp_rmem",
		kernel: utilkernel.TCPReceiveMemoryNamespacedKernelVersion,
	},
	{
		name:   "net.ipv4.tcp_wmem",
		kernel: utilkernel.TCPTransmitMemoryNamespacedKernelVersion,
	},
}

// SafeSysctlAllowlist returns the allowlist of safe sysctls and safe sysctl patterns (ending in *).
//
// A sysctl is called safe iff
// - it is namespaced in the container or the pod
// - it is isolated, i.e. has no influence on any other pod on the same node.
func SafeSysctlAllowlist(ctx context.Context) []string {
	if goruntime.GOOS != "linux" {
		return nil
	}

	return getSafeSysctlAllowlist(ctx, utilkernel.GetVersion)
}

func getSafeSysctlAllowlist(ctx context.Context, getVersion func() (*version.Version, error)) []string {
	logger := klog.FromContext(ctx)
	kernelVersion, err := getVersion()
	if err != nil {
		logger.Error(err, "failed to get kernel version, unable to determine which sysctls are available")
	}
	var safeSysctlAllowlist []string
	for _, sc := range safeSysctls {
		if sc.kernel == "" {
			safeSysctlAllowlist = append(safeSysctlAllowlist, sc.name)
			continue
		}

		if kernelVersion != nil && kernelVersion.AtLeast(version.MustParseGeneric(sc.kernel)) {
			safeSysctlAllowlist = append(safeSysctlAllowlist, sc.name)
		} else {
			logger.Info("kernel version is too old, dropping the sysctl from safe sysctl list", "kernelVersion", kernelVersion, "sysctl", sc.name)
		}
	}
	return safeSysctlAllowlist
}

// safeAllowList()
// There is a good chance that we have a different pod OS from host OS, in such cases, we should let container
// runtime deal with such errors instead of breaking hard. An example of it Linux Containers on Windows.
func safeAllowedList() []string {
	var safeSysctlAllowlist []string
	for _, sc := range safeSysctls {
		safeSysctlAllowlist = append(safeSysctlAllowlist, sc.name)
	}
	return safeSysctlAllowlist
}
