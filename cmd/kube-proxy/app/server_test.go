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

package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientsetfake "k8s.io/client-go/kubernetes/fake"
	kubeproxyconfig "k8s.io/kubernetes/pkg/proxy/apis/config"
	"k8s.io/kubernetes/test/utils/ktesting"
	netutils "k8s.io/utils/net"
)

type fakeProxyServerLongRun struct{}

// Run runs the specified ProxyServer.
func (s *fakeProxyServerLongRun) Run(ctx context.Context) error {
	for {
		time.Sleep(2 * time.Second)
	}
}

// CleanupAndExit runs in the specified ProxyServer.
func (s *fakeProxyServerLongRun) CleanupAndExit() error {
	return nil
}

type fakeProxyServerError struct{}

// Run runs the specified ProxyServer.
func (s *fakeProxyServerError) Run(ctx context.Context) error {
	for {
		time.Sleep(2 * time.Second)
		return fmt.Errorf("mocking error from ProxyServer.Run()")
	}
}

// CleanupAndExit runs in the specified ProxyServer.
func (s *fakeProxyServerError) CleanupAndExit() error {
	return errors.New("mocking error from ProxyServer.CleanupAndExit()")
}

func makeNodeWithAddress(name, primaryIP string) *v1.Node {
	node := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Status: v1.NodeStatus{
			Addresses: []v1.NodeAddress{},
		},
	}

	if primaryIP != "" {
		node.Status.Addresses = append(node.Status.Addresses,
			v1.NodeAddress{Type: v1.NodeInternalIP, Address: primaryIP},
		)
	}

	return node
}

// Test that getNodeIPs retries on failure
func Test_getNodeIPs(t *testing.T) {
	var chans [3]chan error

	client := clientsetfake.NewSimpleClientset(
		// node1 initially has no IP address.
		makeNodeWithAddress("node1", ""),

		// node2 initially has an invalid IP address.
		makeNodeWithAddress("node2", "invalid-ip"),

		// node3 initially does not exist.
	)

	for i := range chans {
		chans[i] = make(chan error)
		ch := chans[i]
		nodeName := fmt.Sprintf("node%d", i+1)
		expectIP := fmt.Sprintf("192.168.0.%d", i+1)
		go func() {
			_, ctx := ktesting.NewTestContext(t)
			ips := getNodeIPs(ctx, client, nodeName)
			if len(ips) == 0 {
				ch <- fmt.Errorf("expected IP %s for %s but got nil", expectIP, nodeName)
			} else if ips[0].String() != expectIP {
				ch <- fmt.Errorf("expected IP %s for %s but got %s", expectIP, nodeName, ips[0].String())
			} else if len(ips) != 1 {
				ch <- fmt.Errorf("expected IP %s for %s but got multiple IPs", expectIP, nodeName)
			}
			close(ch)
		}()
	}

	// Give the goroutines time to fetch the bad/non-existent nodes, then fix them.
	time.Sleep(1200 * time.Millisecond)

	_, _ = client.CoreV1().Nodes().UpdateStatus(context.TODO(),
		makeNodeWithAddress("node1", "192.168.0.1"),
		metav1.UpdateOptions{},
	)
	_, _ = client.CoreV1().Nodes().UpdateStatus(context.TODO(),
		makeNodeWithAddress("node2", "192.168.0.2"),
		metav1.UpdateOptions{},
	)
	_, _ = client.CoreV1().Nodes().Create(context.TODO(),
		makeNodeWithAddress("node3", "192.168.0.3"),
		metav1.CreateOptions{},
	)

	// Ensure each getNodeIP completed as expected
	for i := range chans {
		err := <-chans[i]
		if err != nil {
			t.Error(err.Error())
		}
	}
}

func Test_detectNodeIPs(t *testing.T) {
	cases := []struct {
		name           string
		rawNodeIPs     []net.IP
		nodeIPOverride []string
		ipFamilyPolicy kubeproxyconfig.IPFamilyPolicy
		expectedFamily v1.IPFamily
		expectedIPv4   string
		expectedIPv6   string
	}{
		{
			name:           "NodeIPOverride IPv4 unicast address and no Node object",
			rawNodeIPs:     nil,
			nodeIPOverride: []string{"10.0.0.1"},
			ipFamilyPolicy: kubeproxyconfig.IPFamilyPolicyPreferDualStack,
			expectedFamily: v1.IPv4Protocol,
			expectedIPv4:   "10.0.0.1",
			expectedIPv6:   "::1",
		},
		{
			name:           "NodeIPOverride IPv4 unicast address, no Node object and SingleStack IPFamilyPolicy",
			rawNodeIPs:     nil,
			nodeIPOverride: []string{"10.0.0.1"},
			ipFamilyPolicy: kubeproxyconfig.IPFamilyPolicySingleStack,
			expectedFamily: v1.IPv4Protocol,
			expectedIPv4:   "10.0.0.1",
		},
		{
			name:           "NodeIPOverride IPv6 unicast address and no Node object",
			rawNodeIPs:     nil,
			nodeIPOverride: []string{"fd00:4321::2"},
			ipFamilyPolicy: kubeproxyconfig.IPFamilyPolicyPreferDualStack,
			expectedFamily: v1.IPv6Protocol,
			expectedIPv4:   "127.0.0.1",
			expectedIPv6:   "fd00:4321::2",
		},
		{
			name:           "NodeIPOverride IPv6 unicast address, no Node object and SingleStack IPFamilyPolicy",
			rawNodeIPs:     nil,
			nodeIPOverride: []string{"fd00:4321::2"},
			ipFamilyPolicy: kubeproxyconfig.IPFamilyPolicySingleStack,
			expectedFamily: v1.IPv6Protocol,
			expectedIPv6:   "fd00:4321::2",
		},
		{
			name:           "No Valid IP found and no NodeIPOverride",
			rawNodeIPs:     nil,
			nodeIPOverride: []string{""},
			expectedFamily: v1.IPv4Protocol,
			ipFamilyPolicy: kubeproxyconfig.IPFamilyPolicyPreferDualStack,
			expectedIPv4:   "127.0.0.1",
			expectedIPv6:   "::1",
		},
		{
			name:           "No Valid IP found and unspecified NodeIPOverride",
			rawNodeIPs:     nil,
			nodeIPOverride: []string{"0.0.0.0"},
			expectedFamily: v1.IPv4Protocol,
			ipFamilyPolicy: kubeproxyconfig.IPFamilyPolicyPreferDualStack,
			expectedIPv4:   "127.0.0.1",
			expectedIPv6:   "::1",
		},
		{
			name:           "NodeIPOverride 0.0.0.0 and node with IPv4 InternalIP set",
			rawNodeIPs:     []net.IP{netutils.ParseIPSloppy("192.168.1.1")},
			nodeIPOverride: []string{"0.0.0.0"},
			expectedFamily: v1.IPv4Protocol,
			ipFamilyPolicy: kubeproxyconfig.IPFamilyPolicyPreferDualStack,
			expectedIPv4:   "192.168.1.1",
			expectedIPv6:   "::1",
		},
		{
			name:           "NodeIPOverride :: and node with IPv4 InternalIP set",
			rawNodeIPs:     []net.IP{netutils.ParseIPSloppy("192.168.1.1")},
			nodeIPOverride: []string{"::"},
			expectedFamily: v1.IPv4Protocol,
			ipFamilyPolicy: kubeproxyconfig.IPFamilyPolicyPreferDualStack,
			expectedIPv4:   "192.168.1.1",
			expectedIPv6:   "::1",
		},
		{
			name:           "NodeIPOverride 0.0.0.0 and node with IPv6 InternalIP set",
			rawNodeIPs:     []net.IP{netutils.ParseIPSloppy("fd00:1234::1")},
			nodeIPOverride: []string{"0.0.0.0"},
			expectedFamily: v1.IPv6Protocol,
			ipFamilyPolicy: kubeproxyconfig.IPFamilyPolicyPreferDualStack,
			expectedIPv4:   "127.0.0.1",
			expectedIPv6:   "fd00:1234::1",
		},
		{
			name:           "NodeIPOverride :: and node with IPv6 InternalIP set",
			rawNodeIPs:     []net.IP{netutils.ParseIPSloppy("fd00:1234::1")},
			nodeIPOverride: []string{"::"},
			expectedFamily: v1.IPv6Protocol,
			ipFamilyPolicy: kubeproxyconfig.IPFamilyPolicyPreferDualStack,
			expectedIPv4:   "127.0.0.1",
			expectedIPv6:   "fd00:1234::1",
		},
		{
			name: "Dual stack, primary IPv4",
			rawNodeIPs: []net.IP{
				netutils.ParseIPSloppy("90.90.90.90"),
				netutils.ParseIPSloppy("2001:db8::2"),
			},
			nodeIPOverride: []string{"::"},
			expectedFamily: v1.IPv4Protocol,
			ipFamilyPolicy: kubeproxyconfig.IPFamilyPolicyPreferDualStack,
			expectedIPv4:   "90.90.90.90",
			expectedIPv6:   "2001:db8::2",
		},
		{
			name: "Dual stack, primary IPv4 and RequiredDualStack IPFamilyPolicy",
			rawNodeIPs: []net.IP{
				netutils.ParseIPSloppy("90.90.90.90"),
				netutils.ParseIPSloppy("2001:db8::2"),
			},
			nodeIPOverride: []string{"::"},
			expectedFamily: v1.IPv4Protocol,
			ipFamilyPolicy: kubeproxyconfig.IPFamilyPolicyRequireDualStack,
			expectedIPv4:   "90.90.90.90",
			expectedIPv6:   "2001:db8::2",
		},
		{
			name: "Dual stack, primary IPv6",
			rawNodeIPs: []net.IP{
				netutils.ParseIPSloppy("2001:db8::2"),
				netutils.ParseIPSloppy("90.90.90.90"),
			},
			nodeIPOverride: []string{"0.0.0.0"},
			expectedFamily: v1.IPv6Protocol,
			ipFamilyPolicy: kubeproxyconfig.IPFamilyPolicyPreferDualStack,
			expectedIPv4:   "90.90.90.90",
			expectedIPv6:   "2001:db8::2",
		},
		{
			name: "Dual stack, primary IPv6 and RequiredDualStack IPFamilyPolicy",
			rawNodeIPs: []net.IP{
				netutils.ParseIPSloppy("2001:db8::2"),
				netutils.ParseIPSloppy("90.90.90.90"),
			},
			nodeIPOverride: []string{"0.0.0.0"},
			expectedFamily: v1.IPv6Protocol,
			ipFamilyPolicy: kubeproxyconfig.IPFamilyPolicyPreferDualStack,
			expectedIPv4:   "90.90.90.90",
			expectedIPv6:   "2001:db8::2",
		},
		{
			name: "Dual stack, override IPv4",
			rawNodeIPs: []net.IP{
				netutils.ParseIPSloppy("2001:db8::2"),
				netutils.ParseIPSloppy("90.90.90.90"),
			},
			nodeIPOverride: []string{"80.80.80.80"},
			expectedFamily: v1.IPv4Protocol,
			ipFamilyPolicy: kubeproxyconfig.IPFamilyPolicyPreferDualStack,
			expectedIPv4:   "80.80.80.80",
			expectedIPv6:   "2001:db8::2",
		},
		{
			name: "Dual stack, override IPv6",
			rawNodeIPs: []net.IP{
				netutils.ParseIPSloppy("90.90.90.90"),
				netutils.ParseIPSloppy("2001:db8::2"),
			},
			nodeIPOverride: []string{"2001:db8::555"},
			expectedFamily: v1.IPv6Protocol,
			ipFamilyPolicy: kubeproxyconfig.IPFamilyPolicyPreferDualStack,
			expectedIPv4:   "90.90.90.90",
			expectedIPv6:   "2001:db8::555",
		},
		{
			name: "Dual stack, override primary family, IPv4",
			rawNodeIPs: []net.IP{
				netutils.ParseIPSloppy("2001:db8::2"),
				netutils.ParseIPSloppy("90.90.90.90"),
			},
			nodeIPOverride: []string{"127.0.0.1"},
			expectedFamily: v1.IPv4Protocol,
			ipFamilyPolicy: kubeproxyconfig.IPFamilyPolicyPreferDualStack,
			expectedIPv4:   "127.0.0.1",
			expectedIPv6:   "2001:db8::2",
		},
		{
			name: "Dual stack, override primary family, IPv6",
			rawNodeIPs: []net.IP{
				netutils.ParseIPSloppy("90.90.90.90"),
				netutils.ParseIPSloppy("2001:db8::2"),
			},
			nodeIPOverride: []string{"::1"},
			expectedFamily: v1.IPv6Protocol,
			ipFamilyPolicy: kubeproxyconfig.IPFamilyPolicyPreferDualStack,
			expectedIPv4:   "90.90.90.90",
			expectedIPv6:   "::1",
		},
		{
			name: "Dual stack, override with unspecified addresses, IPv6",
			rawNodeIPs: []net.IP{
				netutils.ParseIPSloppy("2001:db8::2"),
				netutils.ParseIPSloppy("90.90.90.90"),
			},
			nodeIPOverride: []string{"0.0.0.0", "::"},
			expectedFamily: v1.IPv6Protocol,
			ipFamilyPolicy: kubeproxyconfig.IPFamilyPolicyPreferDualStack,
			expectedIPv4:   "90.90.90.90",
			expectedIPv6:   "2001:db8::2",
		},
		{
			name: "Dual stack, override with specified addresses, IPv4",
			rawNodeIPs: []net.IP{
				netutils.ParseIPSloppy("2001:db8::2"),
				netutils.ParseIPSloppy("90.90.90.90"),
			},
			nodeIPOverride: []string{"80.80.80.80", "2001:bd8::3"},
			expectedFamily: v1.IPv4Protocol,
			ipFamilyPolicy: kubeproxyconfig.IPFamilyPolicyPreferDualStack,
			expectedIPv4:   "80.80.80.80",
			expectedIPv6:   "2001:bd8::3",
		},
		{
			name: "Dual stack, override with unspecified addresses, IPv4",
			rawNodeIPs: []net.IP{
				netutils.ParseIPSloppy("90.90.90.90"),
				netutils.ParseIPSloppy("2001:db8::2"),
			},
			nodeIPOverride: []string{"::", "0.0.0.0"},
			expectedFamily: v1.IPv4Protocol,
			ipFamilyPolicy: kubeproxyconfig.IPFamilyPolicyPreferDualStack,
			expectedIPv4:   "90.90.90.90",
			expectedIPv6:   "2001:db8::2",
		},
		{
			name: "Dual stack, override with specified addresses, IPv6",
			rawNodeIPs: []net.IP{
				netutils.ParseIPSloppy("90.90.90.90"),
				netutils.ParseIPSloppy("2001:db8::2"),
			},
			nodeIPOverride: []string{"2001:bd8::3", "80.80.80.80"},
			expectedFamily: v1.IPv6Protocol,
			ipFamilyPolicy: kubeproxyconfig.IPFamilyPolicyPreferDualStack,
			expectedIPv4:   "80.80.80.80",
			expectedIPv6:   "2001:bd8::3",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, ctx := ktesting.NewTestContext(t)
			primaryFamily, ips := detectNodeIPs(ctx, c.rawNodeIPs, c.nodeIPOverride, c.ipFamilyPolicy)
			if primaryFamily != c.expectedFamily {
				t.Errorf("Expected family %q got %q", c.expectedFamily, primaryFamily)
			}

			var ipString string
			if ip, ok := ips[v1.IPv4Protocol]; ok {
				ipString = ip.String()
			}
			if ipString != c.expectedIPv4 {
				t.Errorf("Expected IPv4 %q got %q", c.expectedIPv4, ips[v1.IPv4Protocol].String())
			}

			ipString = ""
			if ip, ok := ips[v1.IPv6Protocol]; ok {
				ipString = ip.String()
			}
			if ipString != c.expectedIPv6 {
				t.Errorf("Expected IPv6 %q got %q", c.expectedIPv6, ips[v1.IPv6Protocol].String())
			}
		})
	}
}

func Test_checkBadConfig(t *testing.T) {
	cases := []struct {
		name  string
		proxy *ProxyServer
		err   bool
	}{
		{
			name: "single-stack NodePortAddresses with single-stack config",
			proxy: &ProxyServer{
				Config: &kubeproxyconfig.KubeProxyConfiguration{
					DetectLocal: kubeproxyconfig.DetectLocalConfiguration{
						ClusterCIDRs: []string{"10.0.0.0/8"},
					},
					NodePortAddresses: []string{"192.168.0.0/24"},
				},
				PrimaryIPFamily: v1.IPv4Protocol,
			},
			err: false,
		},
		{
			name: "dual-stack NodePortAddresses with dual-stack config",
			proxy: &ProxyServer{
				Config: &kubeproxyconfig.KubeProxyConfiguration{
					DetectLocal: kubeproxyconfig.DetectLocalConfiguration{
						ClusterCIDRs: []string{"10.0.0.0/8", "fd09::/64"},
					},
					NodePortAddresses: []string{"192.168.0.0/24", "fd03::/64"},
				},
				PrimaryIPFamily: v1.IPv4Protocol,
			},
			err: false,
		},
		{
			name: "empty NodePortAddresses",
			proxy: &ProxyServer{
				Config: &kubeproxyconfig.KubeProxyConfiguration{
					NodePortAddresses: []string{},
				},
				PrimaryIPFamily: v1.IPv4Protocol,
			},
			err: true,
		},
		{
			name: "single-stack NodePortAddresses with dual-stack config",
			proxy: &ProxyServer{
				Config: &kubeproxyconfig.KubeProxyConfiguration{
					DetectLocal: kubeproxyconfig.DetectLocalConfiguration{
						ClusterCIDRs: []string{"10.0.0.0/8", "fd09::/64"},
					},
					NodePortAddresses: []string{"192.168.0.0/24"},
				},
				PrimaryIPFamily: v1.IPv4Protocol,
			},
			err: true,
		},
		{
			name: "wrong-single-stack NodePortAddresses",
			proxy: &ProxyServer{
				Config: &kubeproxyconfig.KubeProxyConfiguration{
					DetectLocal: kubeproxyconfig.DetectLocalConfiguration{
						ClusterCIDRs: []string{"fd09::/64"},
					},
					NodePortAddresses: []string{"192.168.0.0/24"},
				},
				PrimaryIPFamily: v1.IPv6Protocol,
			},
			err: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := checkBadConfig(c.proxy)
			if err != nil && !c.err {
				t.Errorf("unexpected error: %v", err)
			} else if err == nil && c.err {
				t.Errorf("unexpected lack of error")
			}
		})
	}
}

func Test_checkBadIPConfig(t *testing.T) {
	cases := []struct {
		name    string
		proxy   *ProxyServer
		ssErr   bool
		ssFatal bool
		dsErr   bool
		dsFatal bool
	}{
		{
			name: "empty config",
			proxy: &ProxyServer{
				Config:          &kubeproxyconfig.KubeProxyConfiguration{},
				PrimaryIPFamily: v1.IPv4Protocol,
			},
			ssErr: false,
			dsErr: false,
		},

		{
			name: "ok single-stack clusterCIDR",
			proxy: &ProxyServer{
				Config: &kubeproxyconfig.KubeProxyConfiguration{
					DetectLocal: kubeproxyconfig.DetectLocalConfiguration{
						ClusterCIDRs: []string{"10.0.0.0/8"},
					},
				},
				PrimaryIPFamily: v1.IPv4Protocol,
			},
			ssErr: false,
			dsErr: false,
		},
		{
			name: "ok dual-stack clusterCIDR",
			proxy: &ProxyServer{
				Config: &kubeproxyconfig.KubeProxyConfiguration{
					DetectLocal: kubeproxyconfig.DetectLocalConfiguration{
						ClusterCIDRs: []string{"10.0.0.0/8", "fd01:2345::/64"},
					},
				},
				PrimaryIPFamily: v1.IPv4Protocol,
			},
			ssErr: false,
			dsErr: false,
		},
		{
			name: "ok reversed dual-stack clusterCIDR",
			proxy: &ProxyServer{
				Config: &kubeproxyconfig.KubeProxyConfiguration{
					DetectLocal: kubeproxyconfig.DetectLocalConfiguration{
						ClusterCIDRs: []string{"fd01:2345::/64", "10.0.0.0/8"},
					},
				},
				PrimaryIPFamily: v1.IPv4Protocol,
			},
			ssErr: false,
			dsErr: false,
		},
		{
			name: "wrong-family clusterCIDR",
			proxy: &ProxyServer{
				Config: &kubeproxyconfig.KubeProxyConfiguration{
					DetectLocal: kubeproxyconfig.DetectLocalConfiguration{
						ClusterCIDRs: []string{"fd01:2345::/64"},
					},
				},
				PrimaryIPFamily: v1.IPv4Protocol,
			},
			ssErr:   true,
			ssFatal: false,
			dsErr:   true,
			dsFatal: false,
		},
		{
			name: "wrong-family clusterCIDR when using ClusterCIDR LocalDetector",
			proxy: &ProxyServer{
				Config: &kubeproxyconfig.KubeProxyConfiguration{
					DetectLocal: kubeproxyconfig.DetectLocalConfiguration{
						ClusterCIDRs: []string{"fd01:2345::/64"},
					},
					DetectLocalMode: kubeproxyconfig.LocalModeClusterCIDR,
				},
				PrimaryIPFamily: v1.IPv4Protocol,
			},
			ssErr:   true,
			ssFatal: true,
			dsErr:   true,
			dsFatal: false,
		},

		{
			name: "ok single-stack node.spec.podCIDRs",
			proxy: &ProxyServer{
				Config: &kubeproxyconfig.KubeProxyConfiguration{
					DetectLocalMode: kubeproxyconfig.LocalModeNodeCIDR,
				},
				PrimaryIPFamily: v1.IPv4Protocol,
				podCIDRs:        []string{"10.0.0.0/8"},
			},
			ssErr: false,
			dsErr: false,
		},
		{
			name: "ok dual-stack node.spec.podCIDRs",
			proxy: &ProxyServer{
				Config: &kubeproxyconfig.KubeProxyConfiguration{
					DetectLocalMode: kubeproxyconfig.LocalModeNodeCIDR,
				},
				PrimaryIPFamily: v1.IPv4Protocol,
				podCIDRs:        []string{"10.0.0.0/8", "fd01:2345::/64"},
			},
			ssErr: false,
			dsErr: false,
		},
		{
			name: "ok reversed dual-stack node.spec.podCIDRs",
			proxy: &ProxyServer{
				Config: &kubeproxyconfig.KubeProxyConfiguration{
					DetectLocalMode: kubeproxyconfig.LocalModeNodeCIDR,
				},
				PrimaryIPFamily: v1.IPv4Protocol,
				podCIDRs:        []string{"fd01:2345::/64", "10.0.0.0/8"},
			},
			ssErr: false,
			dsErr: false,
		},
		{
			name: "wrong-family node.spec.podCIDRs",
			proxy: &ProxyServer{
				Config: &kubeproxyconfig.KubeProxyConfiguration{
					DetectLocalMode: kubeproxyconfig.LocalModeNodeCIDR,
				},
				PrimaryIPFamily: v1.IPv4Protocol,
				podCIDRs:        []string{"fd01:2345::/64"},
			},
			ssErr:   true,
			ssFatal: true,
			dsErr:   true,
			dsFatal: true,
		},

		{
			name: "ok winkernel.sourceVip",
			proxy: &ProxyServer{
				Config: &kubeproxyconfig.KubeProxyConfiguration{
					Winkernel: kubeproxyconfig.KubeProxyWinkernelConfiguration{
						SourceVip: "10.0.0.1",
					},
				},
				PrimaryIPFamily: v1.IPv4Protocol,
			},
			ssErr: false,
			dsErr: false,
		},
		{
			name: "wrong family winkernel.sourceVip",
			proxy: &ProxyServer{
				Config: &kubeproxyconfig.KubeProxyConfiguration{
					Winkernel: kubeproxyconfig.KubeProxyWinkernelConfiguration{
						SourceVip: "fd01:2345::1",
					},
				},
				PrimaryIPFamily: v1.IPv4Protocol,
			},
			ssErr:   true,
			ssFatal: false,
			dsErr:   true,
			dsFatal: false,
		},

		{
			name: "ok IPv4 metricsBindAddress",
			proxy: &ProxyServer{
				Config: &kubeproxyconfig.KubeProxyConfiguration{
					MetricsBindAddress: "10.0.0.1:9999",
				},
				PrimaryIPFamily: v1.IPv4Protocol,
			},
			ssErr: false,
			dsErr: false,
		},
		{
			name: "ok IPv6 metricsBindAddress",
			proxy: &ProxyServer{
				Config: &kubeproxyconfig.KubeProxyConfiguration{
					MetricsBindAddress: "[fd01:2345::1]:9999",
				},
				PrimaryIPFamily: v1.IPv6Protocol,
			},
			ssErr: false,
			dsErr: false,
		},
		{
			name: "ok unspecified wrong-family metricsBindAddress",
			proxy: &ProxyServer{
				Config: &kubeproxyconfig.KubeProxyConfiguration{
					MetricsBindAddress: "0.0.0.0:9999",
				},
				PrimaryIPFamily: v1.IPv6Protocol,
			},
			ssErr: false,
			dsErr: false,
		},
		{
			name: "wrong family metricsBindAddress",
			proxy: &ProxyServer{
				Config: &kubeproxyconfig.KubeProxyConfiguration{
					MetricsBindAddress: "10.0.0.1:9999",
				},
				PrimaryIPFamily: v1.IPv6Protocol,
			},
			ssErr:   true,
			ssFatal: false,
			dsErr:   false,
		},

		{
			name: "ok ipvs.excludeCIDRs",
			proxy: &ProxyServer{
				Config: &kubeproxyconfig.KubeProxyConfiguration{
					IPVS: kubeproxyconfig.KubeProxyIPVSConfiguration{
						ExcludeCIDRs: []string{"10.0.0.0/8"},
					},
				},
				PrimaryIPFamily: v1.IPv4Protocol,
			},
			ssErr: false,
			dsErr: false,
		},
		{
			name: "wrong family ipvs.excludeCIDRs",
			proxy: &ProxyServer{
				Config: &kubeproxyconfig.KubeProxyConfiguration{
					IPVS: kubeproxyconfig.KubeProxyIPVSConfiguration{
						ExcludeCIDRs: []string{"10.0.0.0/8", "192.168.0.0/24"},
					},
				},
				PrimaryIPFamily: v1.IPv6Protocol,
			},
			ssErr:   true,
			ssFatal: false,
			dsErr:   false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err, fatal := checkBadIPConfig(c.proxy, false)
			if err != nil && !c.ssErr {
				t.Errorf("unexpected error in single-stack case: %v", err)
			} else if err == nil && c.ssErr {
				t.Errorf("unexpected lack of error in single-stack case")
			} else if fatal != c.ssFatal {
				t.Errorf("expected fatal=%v, got %v", c.ssFatal, fatal)
			}

			err, fatal = checkBadIPConfig(c.proxy, true)
			if err != nil && !c.dsErr {
				t.Errorf("unexpected error in dual-stack case: %v", err)
			} else if err == nil && c.dsErr {
				t.Errorf("unexpected lack of error in dual-stack case")
			} else if fatal != c.dsFatal {
				t.Errorf("expected fatal=%v, got %v", c.dsFatal, fatal)
			}
		})
	}
}
