//go:build linux
// +build linux

/*
Copyright 2014 The Kubernetes Authors.

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

// Package app does all of the work necessary to configure and run a
// Kubernetes app process.
package app

import (
	"context"
	"errors"
	"fmt"
	goruntime "runtime"
	"time"

	"github.com/google/cadvisor/machine"
	"github.com/google/cadvisor/utils/sysfs"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	toolswatch "k8s.io/client-go/tools/watch"
	utilsysctl "k8s.io/component-helpers/node/util/sysctl"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/features"
	"k8s.io/kubernetes/pkg/proxy"
	proxyconfigapi "k8s.io/kubernetes/pkg/proxy/apis/config"
	"k8s.io/kubernetes/pkg/proxy/iptables"
	"k8s.io/kubernetes/pkg/proxy/ipvs"
	utilipset "k8s.io/kubernetes/pkg/proxy/ipvs/ipset"
	utilipvs "k8s.io/kubernetes/pkg/proxy/ipvs/util"
	"k8s.io/kubernetes/pkg/proxy/nftables"
	proxyutil "k8s.io/kubernetes/pkg/proxy/util"
	utiliptables "k8s.io/kubernetes/pkg/util/iptables"
	"k8s.io/utils/exec"
)

// timeoutForNodePodCIDR is the time to wait for allocators to assign a PodCIDR to the
// node after it is registered.
var timeoutForNodePodCIDR = 5 * time.Minute

// platformApplyDefaults is called after parsing command-line flags and/or reading the
// config file, to apply platform-specific default values to config.
func (o *Options) platformApplyDefaults(config *proxyconfigapi.KubeProxyConfiguration) {
	if config.Mode == "" {
		o.logger.Info("Using iptables proxy")
		config.Mode = proxyconfigapi.ProxyModeIPTables
	}

	if config.Mode == proxyconfigapi.ProxyModeNFTables && len(config.NodePortAddresses) == 0 {
		config.NodePortAddresses = []string{proxyconfigapi.NodePortAddressesPrimary}
	}

	if config.DetectLocalMode == "" {
		o.logger.V(4).Info("Defaulting detect-local-mode", "localModeClusterCIDR", string(proxyconfigapi.LocalModeClusterCIDR))
		config.DetectLocalMode = proxyconfigapi.LocalModeClusterCIDR
	}
	o.logger.V(2).Info("DetectLocalMode", "localMode", string(config.DetectLocalMode))
}

// platformSetup is called after setting up the ProxyServer, but before creating the
// Proxier. It should fill in any platform-specific fields and perform other
// platform-specific setup.
func (s *ProxyServer) platformSetup(ctx context.Context) error {
	logger := klog.FromContext(ctx)
	if s.Config.DetectLocalMode == proxyconfigapi.LocalModeNodeCIDR {
		logger.Info("Watching for node, awaiting podCIDR allocation", "hostname", s.Hostname)
		node, err := waitForPodCIDR(ctx, s.Client, s.Hostname)
		if err != nil {
			return err
		}
		s.podCIDRs = node.Spec.PodCIDRs
		logger.Info("NodeInfo", "podCIDRs", node.Spec.PodCIDRs)
	}

	ct := &realConntracker{}
	err := s.setupConntrack(ctx, ct)
	if err != nil {
		return err
	}

	return nil
}

// isIPTablesBased checks whether mode is based on iptables rather than nftables
func isIPTablesBased(mode proxyconfigapi.ProxyMode) bool {
	return mode == proxyconfigapi.ProxyModeIPTables || mode == proxyconfigapi.ProxyModeIPVS
}

// platformCheckSupported is called immediately before creating the Proxier, to check
// what IP families are supported (and whether the configuration is usable at all).
func (s *ProxyServer) platformCheckSupported(ctx context.Context) (ipv4Supported, ipv6Supported, dualStackSupported bool, err error) {
	logger := klog.FromContext(ctx)

	if isIPTablesBased(s.Config.Mode) {
		ipts := utiliptables.NewDualStack()
		ipv4Supported = ipts[v1.IPv4Protocol] != nil
		ipv6Supported = ipts[v1.IPv6Protocol] != nil

		if !ipv4Supported && !ipv6Supported {
			err = fmt.Errorf("iptables is not available on this host")
		} else if !ipv4Supported {
			logger.Info("No iptables support for family", "ipFamily", v1.IPv4Protocol)
		} else if !ipv6Supported {
			logger.Info("No iptables support for family", "ipFamily", v1.IPv6Protocol)
		}
	} else {
		// Assume support for both families.
		// FIXME: figure out how to check for kernel IPv6 support using nft
		ipv4Supported, ipv6Supported = true, true
	}

	// The Linux proxies can always support dual-stack if they can support both IPv4
	// and IPv6.
	dualStackSupported = ipv4Supported && ipv6Supported
	return
}

// createProxier creates the proxy.Provider
func (s *ProxyServer) createProxier(ctx context.Context, config *proxyconfigapi.KubeProxyConfiguration, dualStack, initOnly bool) (proxy.Provider, error) {
	logger := klog.FromContext(ctx)
	var proxier proxy.Provider
	var err error

	localDetectors := getLocalDetectors(logger, s.PrimaryIPFamily, config, s.podCIDRs)

	if config.Mode == proxyconfigapi.ProxyModeIPTables {
		logger.Info("Using iptables Proxier")
		ipts := utiliptables.NewDualStack()

		if dualStack {
			// TODO this has side effects that should only happen when Run() is invoked.
			proxier, err = iptables.NewDualStackProxier(
				ctx,
				ipts,
				utilsysctl.New(),
				exec.New(),
				config.SyncPeriod.Duration,
				config.MinSyncPeriod.Duration,
				config.Linux.MasqueradeAll,
				*config.IPTables.LocalhostNodePorts,
				int(*config.IPTables.MasqueradeBit),
				localDetectors,
				s.Hostname,
				s.NodeIPs,
				s.Recorder,
				s.HealthzServer,
				config.NodePortAddresses,
				initOnly,
			)
		} else {
			// Create a single-stack proxier if and only if the node does not support dual-stack (i.e, no iptables support).

			// TODO this has side effects that should only happen when Run() is invoked.
			proxier, err = iptables.NewProxier(
				ctx,
				s.PrimaryIPFamily,
				ipts[s.PrimaryIPFamily],
				utilsysctl.New(),
				exec.New(),
				config.SyncPeriod.Duration,
				config.MinSyncPeriod.Duration,
				config.Linux.MasqueradeAll,
				*config.IPTables.LocalhostNodePorts,
				int(*config.IPTables.MasqueradeBit),
				localDetectors[s.PrimaryIPFamily],
				s.Hostname,
				s.NodeIPs[s.PrimaryIPFamily],
				s.Recorder,
				s.HealthzServer,
				config.NodePortAddresses,
				initOnly,
			)
		}

		if err != nil {
			return nil, fmt.Errorf("unable to create proxier: %v", err)
		}
	} else if config.Mode == proxyconfigapi.ProxyModeIPVS {
		execer := exec.New()
		ipsetInterface := utilipset.New(execer)
		ipvsInterface := utilipvs.New()
		if err := ipvs.CanUseIPVSProxier(ctx, ipvsInterface, ipsetInterface, config.IPVS.Scheduler); err != nil {
			return nil, fmt.Errorf("can't use the IPVS proxier: %v", err)
		}
		ipts := utiliptables.NewDualStack()

		logger.Info("Using ipvs Proxier")
		if dualStack {
			proxier, err = ipvs.NewDualStackProxier(
				ctx,
				ipts,
				ipvsInterface,
				ipsetInterface,
				utilsysctl.New(),
				execer,
				config.SyncPeriod.Duration,
				config.MinSyncPeriod.Duration,
				config.IPVS.ExcludeCIDRs,
				config.IPVS.StrictARP,
				config.IPVS.TCPTimeout.Duration,
				config.IPVS.TCPFinTimeout.Duration,
				config.IPVS.UDPTimeout.Duration,
				config.Linux.MasqueradeAll,
				int(*config.IPTables.MasqueradeBit),
				localDetectors,
				s.Hostname,
				s.NodeIPs,
				s.Recorder,
				s.HealthzServer,
				config.IPVS.Scheduler,
				config.NodePortAddresses,
				initOnly,
			)
		} else {
			proxier, err = ipvs.NewProxier(
				ctx,
				s.PrimaryIPFamily,
				ipts[s.PrimaryIPFamily],
				ipvsInterface,
				ipsetInterface,
				utilsysctl.New(),
				execer,
				config.SyncPeriod.Duration,
				config.MinSyncPeriod.Duration,
				config.IPVS.ExcludeCIDRs,
				config.IPVS.StrictARP,
				config.IPVS.TCPTimeout.Duration,
				config.IPVS.TCPFinTimeout.Duration,
				config.IPVS.UDPTimeout.Duration,
				config.Linux.MasqueradeAll,
				int(*config.IPTables.MasqueradeBit),
				localDetectors[s.PrimaryIPFamily],
				s.Hostname,
				s.NodeIPs[s.PrimaryIPFamily],
				s.Recorder,
				s.HealthzServer,
				config.IPVS.Scheduler,
				config.NodePortAddresses,
				initOnly,
			)
		}
		if err != nil {
			return nil, fmt.Errorf("unable to create proxier: %v", err)
		}
	} else if config.Mode == proxyconfigapi.ProxyModeNFTables {
		logger.Info("Using nftables Proxier")

		if dualStack {
			// TODO this has side effects that should only happen when Run() is invoked.
			proxier, err = nftables.NewDualStackProxier(
				ctx,
				config.SyncPeriod.Duration,
				config.MinSyncPeriod.Duration,
				config.Linux.MasqueradeAll,
				int(*config.NFTables.MasqueradeBit),
				localDetectors,
				s.Hostname,
				s.NodeIPs,
				s.Recorder,
				s.HealthzServer,
				config.NodePortAddresses,
				initOnly,
			)
		} else {
			// Create a single-stack proxier if and only if the node does not support dual-stack
			// TODO this has side effects that should only happen when Run() is invoked.
			proxier, err = nftables.NewProxier(
				ctx,
				s.PrimaryIPFamily,
				config.SyncPeriod.Duration,
				config.MinSyncPeriod.Duration,
				config.Linux.MasqueradeAll,
				int(*config.NFTables.MasqueradeBit),
				localDetectors[s.PrimaryIPFamily],
				s.Hostname,
				s.NodeIPs[s.PrimaryIPFamily],
				s.Recorder,
				s.HealthzServer,
				config.NodePortAddresses,
				initOnly,
			)
		}

		if err != nil {
			return nil, fmt.Errorf("unable to create proxier: %v", err)
		}
	}

	return proxier, nil
}

func (s *ProxyServer) setupConntrack(ctx context.Context, ct Conntracker) error {
	max, err := getConntrackMax(ctx, s.Config.Linux.Conntrack)
	if err != nil {
		return err
	}
	if max > 0 {
		err := ct.SetMax(ctx, max)
		if err != nil {
			if err != errReadOnlySysFS {
				return err
			}
			// errReadOnlySysFS is caused by a known docker issue (https://github.com/docker/docker/issues/24000),
			// the only remediation we know is to restart the docker daemon.
			// Here we'll send an node event with specific reason and message, the
			// administrator should decide whether and how to handle this issue,
			// whether to drain the node and restart docker.  Occurs in other container runtimes
			// as well.
			// TODO(random-liu): Remove this when the docker bug is fixed.
			const message = "CRI error: /sys is read-only: " +
				"cannot modify conntrack limits, problems may arise later (If running Docker, see docker issue #24000)"
			s.Recorder.Eventf(s.NodeRef, nil, v1.EventTypeWarning, err.Error(), "StartKubeProxy", message)
		}
	}

	if s.Config.Linux.Conntrack.TCPEstablishedTimeout != nil && s.Config.Linux.Conntrack.TCPEstablishedTimeout.Duration > 0 {
		timeout := int(s.Config.Linux.Conntrack.TCPEstablishedTimeout.Duration / time.Second)
		if err := ct.SetTCPEstablishedTimeout(ctx, timeout); err != nil {
			return err
		}
	}

	if s.Config.Linux.Conntrack.TCPCloseWaitTimeout != nil && s.Config.Linux.Conntrack.TCPCloseWaitTimeout.Duration > 0 {
		timeout := int(s.Config.Linux.Conntrack.TCPCloseWaitTimeout.Duration / time.Second)
		if err := ct.SetTCPCloseWaitTimeout(ctx, timeout); err != nil {
			return err
		}
	}

	if s.Config.Linux.Conntrack.TCPBeLiberal {
		if err := ct.SetTCPBeLiberal(ctx, 1); err != nil {
			return err
		}
	}

	if s.Config.Linux.Conntrack.UDPTimeout.Duration > 0 {
		timeout := int(s.Config.Linux.Conntrack.UDPTimeout.Duration / time.Second)
		if err := ct.SetUDPTimeout(ctx, timeout); err != nil {
			return err
		}
	}

	if s.Config.Linux.Conntrack.UDPStreamTimeout.Duration > 0 {
		timeout := int(s.Config.Linux.Conntrack.UDPStreamTimeout.Duration / time.Second)
		if err := ct.SetUDPStreamTimeout(ctx, timeout); err != nil {
			return err
		}
	}

	return nil
}

func getConntrackMax(ctx context.Context, config proxyconfigapi.KubeProxyConntrackConfiguration) (int, error) {
	logger := klog.FromContext(ctx)
	if config.MaxPerCore != nil && *config.MaxPerCore > 0 {
		floor := 0
		if config.Min != nil {
			floor = int(*config.Min)
		}
		scaled := int(*config.MaxPerCore) * detectNumCPU()
		if scaled > floor {
			logger.V(3).Info("GetConntrackMax: using scaled conntrack-max-per-core")
			return scaled, nil
		}
		logger.V(3).Info("GetConntrackMax: using conntrack-min")
		return floor, nil
	}
	return 0, nil
}

func waitForPodCIDR(ctx context.Context, client clientset.Interface, nodeName string) (*v1.Node, error) {
	// since allocators can assign the podCIDR after the node registers, we do a watch here to wait
	// for podCIDR to be assigned, instead of assuming that the Get() on startup will have it.
	ctx, cancelFunc := context.WithTimeout(ctx, timeoutForNodePodCIDR)
	defer cancelFunc()

	fieldSelector := fields.OneTermEqualSelector("metadata.name", nodeName).String()
	lw := &cache.ListWatch{
		ListFunc: func(options metav1.ListOptions) (object runtime.Object, e error) {
			options.FieldSelector = fieldSelector
			return client.CoreV1().Nodes().List(ctx, options)
		},
		WatchFunc: func(options metav1.ListOptions) (i watch.Interface, e error) {
			options.FieldSelector = fieldSelector
			return client.CoreV1().Nodes().Watch(ctx, options)
		},
	}
	condition := func(event watch.Event) (bool, error) {
		// don't process delete events
		if event.Type != watch.Modified && event.Type != watch.Added {
			return false, nil
		}

		n, ok := event.Object.(*v1.Node)
		if !ok {
			return false, fmt.Errorf("event object not of type Node")
		}
		// don't consider the node if is going to be deleted and keep waiting
		if !n.DeletionTimestamp.IsZero() {
			return false, nil
		}
		return n.Spec.PodCIDR != "" && len(n.Spec.PodCIDRs) > 0, nil
	}

	evt, err := toolswatch.UntilWithSync(ctx, lw, &v1.Node{}, nil, condition)
	if err != nil {
		return nil, fmt.Errorf("timeout waiting for PodCIDR allocation to configure detect-local-mode %v: %v", proxyconfigapi.LocalModeNodeCIDR, err)
	}
	if n, ok := evt.Object.(*v1.Node); ok {
		return n, nil
	}
	return nil, fmt.Errorf("event object not of type node")
}

func detectNumCPU() int {
	// try get numCPU from /sys firstly due to a known issue (https://github.com/kubernetes/kubernetes/issues/99225)
	_, numCPU, err := machine.GetTopology(sysfs.NewRealSysFs())
	if err != nil || numCPU < 1 {
		return goruntime.NumCPU()
	}
	return numCPU
}

func getLocalDetectors(logger klog.Logger, primaryIPFamily v1.IPFamily, config *proxyconfigapi.KubeProxyConfiguration, nodePodCIDRs []string) map[v1.IPFamily]proxyutil.LocalTrafficDetector {
	localDetectors := map[v1.IPFamily]proxyutil.LocalTrafficDetector{
		v1.IPv4Protocol: proxyutil.NewNoOpLocalDetector(),
		v1.IPv6Protocol: proxyutil.NewNoOpLocalDetector(),
	}

	switch config.DetectLocalMode {
	case proxyconfigapi.LocalModeClusterCIDR:
		for family, cidrs := range proxyutil.MapCIDRsByIPFamily(config.DetectLocal.ClusterCIDRs) {
			localDetectors[family] = proxyutil.NewDetectLocalByCIDR(cidrs[0].String())
		}
		if !localDetectors[primaryIPFamily].IsImplemented() {
			logger.Info("Detect-local-mode set to ClusterCIDR, but no cluster CIDR specified for primary IP family", "ipFamily", primaryIPFamily, "clusterCIDRs", config.DetectLocal.ClusterCIDRs)
		}

	case proxyconfigapi.LocalModeNodeCIDR:
		for family, cidrs := range proxyutil.MapCIDRsByIPFamily(nodePodCIDRs) {
			localDetectors[family] = proxyutil.NewDetectLocalByCIDR(cidrs[0].String())
		}
		if !localDetectors[primaryIPFamily].IsImplemented() {
			logger.Info("Detect-local-mode set to NodeCIDR, but no PodCIDR defined at node for primary IP family", "ipFamily", primaryIPFamily, "podCIDRs", nodePodCIDRs)
		}

	case proxyconfigapi.LocalModeBridgeInterface:
		localDetector := proxyutil.NewDetectLocalByBridgeInterface(config.DetectLocal.BridgeInterface)
		localDetectors[v1.IPv4Protocol] = localDetector
		localDetectors[v1.IPv6Protocol] = localDetector

	case proxyconfigapi.LocalModeInterfaceNamePrefix:
		localDetector := proxyutil.NewDetectLocalByInterfaceNamePrefix(config.DetectLocal.InterfaceNamePrefix)
		localDetectors[v1.IPv4Protocol] = localDetector
		localDetectors[v1.IPv6Protocol] = localDetector

	default:
		logger.Info("Defaulting to no-op detect-local")
	}

	return localDetectors
}

// platformCleanup removes stale kube-proxy rules that can be safely removed. If
// cleanupAndExit is true, it will attempt to remove rules from all known kube-proxy
// modes. If it is false, it will only remove rules that are definitely not in use by the
// currently-configured mode.
func platformCleanup(ctx context.Context, mode proxyconfigapi.ProxyMode, cleanupAndExit bool) error {
	var encounteredError bool

	// Clean up iptables and ipvs rules if switching to nftables, or if cleanupAndExit
	if !isIPTablesBased(mode) || cleanupAndExit {
		ipts := utiliptables.NewDualStack()
		execer := exec.New()
		ipsetInterface := utilipset.New(execer)
		ipvsInterface := utilipvs.New()

		for _, ipt := range ipts {
			encounteredError = iptables.CleanupLeftovers(ctx, ipt) || encounteredError
			encounteredError = ipvs.CleanupLeftovers(ctx, ipvsInterface, ipt, ipsetInterface) || encounteredError
		}
	}

	if utilfeature.DefaultFeatureGate.Enabled(features.NFTablesProxyMode) {
		// Clean up nftables rules when switching to iptables or ipvs, or if cleanupAndExit
		if isIPTablesBased(mode) || cleanupAndExit {
			encounteredError = nftables.CleanupLeftovers(ctx) || encounteredError
		}
	}

	if encounteredError {
		return errors.New("encountered an error while tearing down rules")
	}
	return nil
}
