//go:build linux
// +build linux

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

package kubelet

import (
	"time"

	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"
	utiliptables "k8s.io/kubernetes/pkg/util/iptables"
)

const (
	// KubeIPTablesHintChain is the chain whose existence in either iptables-legacy
	// or iptables-nft indicates which version of iptables the system is using
	KubeIPTablesHintChain utiliptables.Chain = "KUBE-IPTABLES-HINT"

	// KubeFirewallChain is kubernetes firewall rules
	KubeFirewallChain utiliptables.Chain = "KUBE-FIREWALL"
)

func (kl *Kubelet) initNetworkUtil() {
	iptClients := []utiliptables.Interface{
		utiliptables.New(utiliptables.ProtocolIPv4),
		utiliptables.New(utiliptables.ProtocolIPv6),
	}

	for i := range iptClients {
		iptClient := iptClients[i]
		if kl.syncIPTablesRules(iptClient) {
			klog.InfoS("Initialized iptables rules.", "protocol", iptClient.Protocol())
			go iptClient.Monitor(
				utiliptables.Chain("KUBE-KUBELET-CANARY"),
				[]utiliptables.Table{utiliptables.TableMangle, utiliptables.TableNAT, utiliptables.TableFilter},
				func() { kl.syncIPTablesRules(iptClient) },
				1*time.Minute, wait.NeverStop,
			)
		} else {
			klog.InfoS("Failed to initialize iptables rules; some functionality may be missing.", "protocol", iptClient.Protocol())
		}
	}
}

// syncIPTablesRules ensures the KUBE-IPTABLES-HINT chain exists, and the martian packet
// protection rule is installed.
func (kl *Kubelet) syncIPTablesRules(iptClient utiliptables.Interface) bool {
	// Create hint chain so other components can see whether we are using iptables-legacy
	// or iptables-nft.
	if _, err := iptClient.EnsureChain(utiliptables.TableMangle, KubeIPTablesHintChain); err != nil {
		klog.ErrorS(err, "Failed to ensure that iptables hint chain exists")
		return false
	}

	if !iptClient.IsIPv6() { // ipv6 doesn't have this issue
		// Set up the KUBE-FIREWALL chain and martian packet protection rule.
		// (See below.)

		// NOTE: kube-proxy (in iptables mode) creates an identical copy of this
		// rule. If you want to change this rule in the future, you MUST do so in
		// a way that will interoperate correctly with skewed versions of the rule
		// created by kube-proxy.

		if _, err := iptClient.EnsureChain(utiliptables.TableFilter, KubeFirewallChain); err != nil {
			klog.ErrorS(err, "Failed to ensure that filter table KUBE-FIREWALL chain exists")
			return false
		}

		if _, err := iptClient.EnsureRule(utiliptables.Prepend, utiliptables.TableFilter, utiliptables.ChainOutput, "-j", string(KubeFirewallChain)); err != nil {
			klog.ErrorS(err, "Failed to ensure that OUTPUT chain jumps to KUBE-FIREWALL")
			return false
		}
		if _, err := iptClient.EnsureRule(utiliptables.Prepend, utiliptables.TableFilter, utiliptables.ChainInput, "-j", string(KubeFirewallChain)); err != nil {
			klog.ErrorS(err, "Failed to ensure that INPUT chain jumps to KUBE-FIREWALL")
			return false
		}

		// Kube-proxy's use of `route_localnet` to enable NodePorts on localhost
		// creates a security hole (https://issue.k8s.io/90259) which this
		// iptables rule mitigates. This rule should have been added to
		// kube-proxy, but it mistakenly ended up in kubelet instead, and we are
		// keeping it in kubelet for now in case other third-party components
		// depend on it.
		if _, err := iptClient.EnsureRule(utiliptables.Append, utiliptables.TableFilter, KubeFirewallChain,
			"-m", "comment", "--comment", "block incoming localnet connections",
			"--dst", "127.0.0.0/8",
			"!", "--src", "127.0.0.0/8",
			"-m", "conntrack",
			"!", "--ctstate", "RELATED,ESTABLISHED,DNAT",
			"-j", "DROP"); err != nil {
			klog.ErrorS(err, "Failed to ensure rule to drop invalid localhost packets in filter table KUBE-FIREWALL chain")
			return false
		}
	}

	return true
}
