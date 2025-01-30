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

package nodestatus

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
	"testing"
	"time"

	cadvisorapiv1 "github.com/google/cadvisor/info/v1"
	"github.com/google/go-cmp/cmp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	v1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/apimachinery/pkg/util/uuid"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	cloudprovider "k8s.io/cloud-provider"
	fakecloud "k8s.io/cloud-provider/fake"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	"k8s.io/component-base/version"
	"k8s.io/kubernetes/pkg/features"
	"k8s.io/kubernetes/pkg/kubelet/cm"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	kubecontainertest "k8s.io/kubernetes/pkg/kubelet/container/testing"
	"k8s.io/kubernetes/pkg/kubelet/events"
	"k8s.io/kubernetes/pkg/kubelet/util/sliceutils"
	"k8s.io/kubernetes/pkg/volume"
	volumetest "k8s.io/kubernetes/pkg/volume/testing"
	netutils "k8s.io/utils/net"
)

const (
	testKubeletHostname = "hostname"
)

// TODO(mtaufen): below is ported from the old kubelet_node_status_test.go code, potentially add more test coverage for NodeAddress setter in future
func TestNodeAddress(t *testing.T) {
	type cloudProviderType int
	const (
		cloudProviderLegacy cloudProviderType = iota
		cloudProviderExternal
		cloudProviderNone
	)
	existingNodeAddress := v1.NodeAddress{Address: "10.1.1.2"}
	cases := []struct {
		name                           string
		hostnameOverride               bool
		nodeIP                         net.IP
		secondaryNodeIP                net.IP
		cloudProviderType              cloudProviderType
		nodeAddresses                  []v1.NodeAddress
		expectedAddresses              []v1.NodeAddress
		existingAnnotations            map[string]string
		expectedAnnotations            map[string]string
		shouldError                    bool
		shouldSetNodeAddressBeforeTest bool
	}{
		{
			name:   "A single InternalIP",
			nodeIP: netutils.ParseIPSloppy("10.1.1.1"),
			nodeAddresses: []v1.NodeAddress{
				{Type: v1.NodeInternalIP, Address: "10.1.1.1"},
				{Type: v1.NodeHostName, Address: testKubeletHostname},
			},
			expectedAddresses: []v1.NodeAddress{
				{Type: v1.NodeInternalIP, Address: "10.1.1.1"},
				{Type: v1.NodeHostName, Address: testKubeletHostname},
			},
			shouldError: false,
		},
		{
			name:   "NodeIP is external",
			nodeIP: netutils.ParseIPSloppy("55.55.55.55"),
			nodeAddresses: []v1.NodeAddress{
				{Type: v1.NodeInternalIP, Address: "10.1.1.1"},
				{Type: v1.NodeExternalIP, Address: "55.55.55.55"},
				{Type: v1.NodeHostName, Address: testKubeletHostname},
			},
			expectedAddresses: []v1.NodeAddress{
				{Type: v1.NodeExternalIP, Address: "55.55.55.55"},
				{Type: v1.NodeInternalIP, Address: "10.1.1.1"},
				{Type: v1.NodeHostName, Address: testKubeletHostname},
			},
			shouldError: false,
		},
		{
			// Accommodating #45201 and #49202
			name:   "InternalIP and ExternalIP are the same",
			nodeIP: netutils.ParseIPSloppy("55.55.55.55"),
			nodeAddresses: []v1.NodeAddress{
				{Type: v1.NodeInternalIP, Address: "44.44.44.44"},
				{Type: v1.NodeExternalIP, Address: "44.44.44.44"},
				{Type: v1.NodeInternalIP, Address: "55.55.55.55"},
				{Type: v1.NodeExternalIP, Address: "55.55.55.55"},
				{Type: v1.NodeHostName, Address: testKubeletHostname},
			},
			expectedAddresses: []v1.NodeAddress{
				{Type: v1.NodeInternalIP, Address: "55.55.55.55"},
				{Type: v1.NodeExternalIP, Address: "55.55.55.55"},
				{Type: v1.NodeHostName, Address: testKubeletHostname},
			},
			shouldError: false,
		},
		{
			name:   "An Internal/ExternalIP, an Internal/ExternalDNS",
			nodeIP: netutils.ParseIPSloppy("10.1.1.1"),
			nodeAddresses: []v1.NodeAddress{
				{Type: v1.NodeInternalIP, Address: "10.1.1.1"},
				{Type: v1.NodeExternalIP, Address: "55.55.55.55"},
				{Type: v1.NodeInternalDNS, Address: "ip-10-1-1-1.us-west-2.compute.internal"},
				{Type: v1.NodeExternalDNS, Address: "ec2-55-55-55-55.us-west-2.compute.amazonaws.com"},
				{Type: v1.NodeHostName, Address: testKubeletHostname},
			},
			expectedAddresses: []v1.NodeAddress{
				{Type: v1.NodeInternalIP, Address: "10.1.1.1"},
				{Type: v1.NodeExternalIP, Address: "55.55.55.55"},
				{Type: v1.NodeInternalDNS, Address: "ip-10-1-1-1.us-west-2.compute.internal"},
				{Type: v1.NodeExternalDNS, Address: "ec2-55-55-55-55.us-west-2.compute.amazonaws.com"},
				{Type: v1.NodeHostName, Address: testKubeletHostname},
			},
			shouldError: false,
		},
		{
			name:   "An Internal with multiple internal IPs",
			nodeIP: netutils.ParseIPSloppy("10.1.1.1"),
			nodeAddresses: []v1.NodeAddress{
				{Type: v1.NodeInternalIP, Address: "10.1.1.1"},
				{Type: v1.NodeInternalIP, Address: "10.2.2.2"},
				{Type: v1.NodeInternalIP, Address: "10.3.3.3"},
				{Type: v1.NodeExternalIP, Address: "55.55.55.55"},
				{Type: v1.NodeHostName, Address: testKubeletHostname},
			},
			expectedAddresses: []v1.NodeAddress{
				{Type: v1.NodeInternalIP, Address: "10.1.1.1"},
				{Type: v1.NodeExternalIP, Address: "55.55.55.55"},
				{Type: v1.NodeHostName, Address: testKubeletHostname},
			},
			shouldError: false,
		},
		{
			name:   "An InternalIP that isn't valid: should error",
			nodeIP: netutils.ParseIPSloppy("10.2.2.2"),
			nodeAddresses: []v1.NodeAddress{
				{Type: v1.NodeInternalIP, Address: "10.1.1.1"},
				{Type: v1.NodeExternalIP, Address: "55.55.55.55"},
				{Type: v1.NodeHostName, Address: testKubeletHostname},
			},
			expectedAddresses: nil,
			shouldError:       true,
		},
		{
			name:          "no cloud reported hostnames",
			nodeAddresses: []v1.NodeAddress{},
			expectedAddresses: []v1.NodeAddress{
				{Type: v1.NodeHostName, Address: testKubeletHostname}, // detected hostname is auto-added in the absence of cloud-reported hostnames
			},
			shouldError: false,
		},
		{
			name: "cloud reports hostname, no override",
			nodeAddresses: []v1.NodeAddress{
				{Type: v1.NodeInternalIP, Address: "10.1.1.1"},
				{Type: v1.NodeExternalIP, Address: "55.55.55.55"},
				{Type: v1.NodeHostName, Address: "cloud-host"},
			},
			expectedAddresses: []v1.NodeAddress{
				{Type: v1.NodeInternalIP, Address: "10.1.1.1"},
				{Type: v1.NodeExternalIP, Address: "55.55.55.55"},
				{Type: v1.NodeHostName, Address: "cloud-host"}, // cloud-reported hostname wins over detected hostname
			},
			shouldError: false,
		},
		{
			name:   "cloud reports hostname, nodeIP is set, no override",
			nodeIP: netutils.ParseIPSloppy("10.1.1.1"),
			nodeAddresses: []v1.NodeAddress{
				{Type: v1.NodeInternalIP, Address: "10.1.1.1"},
				{Type: v1.NodeExternalIP, Address: "55.55.55.55"},
				{Type: v1.NodeHostName, Address: "cloud-host"},
			},
			expectedAddresses: []v1.NodeAddress{
				{Type: v1.NodeInternalIP, Address: "10.1.1.1"},
				{Type: v1.NodeExternalIP, Address: "55.55.55.55"},
				{Type: v1.NodeHostName, Address: "cloud-host"}, // cloud-reported hostname wins over detected hostname
			},
			shouldError: false,
		},
		{
			name: "cloud reports hostname, overridden",
			nodeAddresses: []v1.NodeAddress{
				{Type: v1.NodeInternalIP, Address: "10.1.1.1"},
				{Type: v1.NodeHostName, Address: "cloud-host"},
				{Type: v1.NodeExternalIP, Address: "55.55.55.55"},
			},
			expectedAddresses: []v1.NodeAddress{
				{Type: v1.NodeInternalIP, Address: "10.1.1.1"},
				{Type: v1.NodeHostName, Address: testKubeletHostname}, // hostname-override wins over cloud-reported hostname
				{Type: v1.NodeExternalIP, Address: "55.55.55.55"},
			},
			hostnameOverride: true,
			shouldError:      false,
		},
		{
			name:              "cloud provider is external and nodeIP specified",
			nodeIP:            netutils.ParseIPSloppy("10.0.0.1"),
			nodeAddresses:     []v1.NodeAddress{},
			cloudProviderType: cloudProviderExternal,
			expectedAddresses: []v1.NodeAddress{
				{Type: v1.NodeInternalIP, Address: "10.0.0.1"},
				{Type: v1.NodeHostName, Address: testKubeletHostname},
			},
			shouldError: false,
		},
		{
			name:              "cloud provider is external and nodeIP unspecified",
			nodeIP:            netutils.ParseIPSloppy("::"),
			nodeAddresses:     []v1.NodeAddress{},
			cloudProviderType: cloudProviderExternal,
			expectedAddresses: []v1.NodeAddress{
				{Type: v1.NodeHostName, Address: testKubeletHostname},
			},
			shouldError: false,
		},
		{
			name:              "cloud provider is external and no nodeIP",
			nodeAddresses:     []v1.NodeAddress{},
			cloudProviderType: cloudProviderExternal,
			expectedAddresses: []v1.NodeAddress{
				{Type: v1.NodeHostName, Address: testKubeletHostname},
			},
			shouldError: false,
		},
		{
			name: "cloud doesn't report hostname, no override, detected hostname mismatch",
			nodeAddresses: []v1.NodeAddress{
				{Type: v1.NodeInternalIP, Address: "10.1.1.1"},
				{Type: v1.NodeExternalIP, Address: "55.55.55.55"},
			},
			expectedAddresses: []v1.NodeAddress{
				{Type: v1.NodeInternalIP, Address: "10.1.1.1"},
				{Type: v1.NodeExternalIP, Address: "55.55.55.55"},
				// detected hostname is not auto-added if it doesn't match any cloud-reported addresses
			},
			shouldError: false,
		},
		{
			name: "cloud doesn't report hostname, no override, detected hostname match",
			nodeAddresses: []v1.NodeAddress{
				{Type: v1.NodeInternalIP, Address: "10.1.1.1"},
				{Type: v1.NodeExternalIP, Address: "55.55.55.55"},
				{Type: v1.NodeExternalDNS, Address: testKubeletHostname}, // cloud-reported address value matches detected hostname
			},
			expectedAddresses: []v1.NodeAddress{
				{Type: v1.NodeInternalIP, Address: "10.1.1.1"},
				{Type: v1.NodeExternalIP, Address: "55.55.55.55"},
				{Type: v1.NodeExternalDNS, Address: testKubeletHostname},
				{Type: v1.NodeHostName, Address: testKubeletHostname}, // detected hostname gets auto-added
			},
			shouldError: false,
		},
		{
			name:   "cloud doesn't report hostname, nodeIP is set, no override, detected hostname match",
			nodeIP: netutils.ParseIPSloppy("10.1.1.1"),
			nodeAddresses: []v1.NodeAddress{
				{Type: v1.NodeInternalIP, Address: "10.1.1.1"},
				{Type: v1.NodeExternalIP, Address: "55.55.55.55"},
				{Type: v1.NodeExternalDNS, Address: testKubeletHostname}, // cloud-reported address value matches detected hostname
			},
			expectedAddresses: []v1.NodeAddress{
				{Type: v1.NodeInternalIP, Address: "10.1.1.1"},
				{Type: v1.NodeExternalIP, Address: "55.55.55.55"},
				{Type: v1.NodeExternalDNS, Address: testKubeletHostname},
				{Type: v1.NodeHostName, Address: testKubeletHostname}, // detected hostname gets auto-added
			},
			shouldError: false,
		},
		{
			name:   "cloud doesn't report hostname, nodeIP is set, no override, detected hostname match with same type as nodeIP",
			nodeIP: netutils.ParseIPSloppy("10.1.1.1"),
			nodeAddresses: []v1.NodeAddress{
				{Type: v1.NodeInternalIP, Address: "10.1.1.1"},
				{Type: v1.NodeInternalIP, Address: testKubeletHostname}, // cloud-reported address value matches detected hostname
				{Type: v1.NodeExternalIP, Address: "55.55.55.55"},
			},
			expectedAddresses: []v1.NodeAddress{
				{Type: v1.NodeInternalIP, Address: "10.1.1.1"},
				{Type: v1.NodeExternalIP, Address: "55.55.55.55"},
				{Type: v1.NodeHostName, Address: testKubeletHostname}, // detected hostname gets auto-added
			},
			shouldError: false,
		},
		{
			name: "cloud doesn't report hostname, hostname override, hostname mismatch",
			nodeAddresses: []v1.NodeAddress{
				{Type: v1.NodeInternalIP, Address: "10.1.1.1"},
				{Type: v1.NodeExternalIP, Address: "55.55.55.55"},
			},
			expectedAddresses: []v1.NodeAddress{
				{Type: v1.NodeInternalIP, Address: "10.1.1.1"},
				{Type: v1.NodeExternalIP, Address: "55.55.55.55"},
				{Type: v1.NodeHostName, Address: testKubeletHostname}, // overridden hostname gets auto-added
			},
			hostnameOverride: true,
			shouldError:      false,
		},
		{
			name:   "Dual-stack cloud, with nodeIP, different IPv6 formats",
			nodeIP: netutils.ParseIPSloppy("2600:1f14:1d4:d101::ba3d"),
			nodeAddresses: []v1.NodeAddress{
				{Type: v1.NodeInternalIP, Address: "10.1.1.1"},
				{Type: v1.NodeInternalIP, Address: "2600:1f14:1d4:d101:0:0:0:ba3d"},
				{Type: v1.NodeHostName, Address: testKubeletHostname},
			},
			expectedAddresses: []v1.NodeAddress{
				{Type: v1.NodeInternalIP, Address: "2600:1f14:1d4:d101:0:0:0:ba3d"},
				{Type: v1.NodeHostName, Address: testKubeletHostname},
			},
			shouldError: false,
		},
		{
			name: "Dual-stack cloud, IPv4 first, no nodeIP",
			nodeAddresses: []v1.NodeAddress{
				{Type: v1.NodeInternalIP, Address: "10.1.1.1"},
				{Type: v1.NodeInternalIP, Address: "fc01:1234::5678"},
				{Type: v1.NodeHostName, Address: testKubeletHostname},
			},
			expectedAddresses: []v1.NodeAddress{
				{Type: v1.NodeInternalIP, Address: "10.1.1.1"},
				{Type: v1.NodeInternalIP, Address: "fc01:1234::5678"},
				{Type: v1.NodeHostName, Address: testKubeletHostname},
			},
			shouldError: false,
		},
		{
			name: "Dual-stack cloud, IPv6 first, no nodeIP",
			nodeAddresses: []v1.NodeAddress{
				{Type: v1.NodeInternalIP, Address: "fc01:1234::5678"},
				{Type: v1.NodeInternalIP, Address: "10.1.1.1"},
				{Type: v1.NodeHostName, Address: testKubeletHostname},
			},
			expectedAddresses: []v1.NodeAddress{
				{Type: v1.NodeInternalIP, Address: "fc01:1234::5678"},
				{Type: v1.NodeInternalIP, Address: "10.1.1.1"},
				{Type: v1.NodeHostName, Address: testKubeletHostname},
			},
			shouldError: false,
		},
		{
			name:   "Dual-stack cloud, IPv4 first, request IPv4",
			nodeIP: netutils.ParseIPSloppy("0.0.0.0"),
			nodeAddresses: []v1.NodeAddress{
				{Type: v1.NodeInternalIP, Address: "10.1.1.1"},
				{Type: v1.NodeInternalIP, Address: "fc01:1234::5678"},
				{Type: v1.NodeHostName, Address: testKubeletHostname},
			},
			expectedAddresses: []v1.NodeAddress{
				{Type: v1.NodeInternalIP, Address: "10.1.1.1"},
				{Type: v1.NodeHostName, Address: testKubeletHostname},
				{Type: v1.NodeInternalIP, Address: "fc01:1234::5678"},
			},
			shouldError: false,
		},
		{
			name:   "Dual-stack cloud, IPv6 first, request IPv4",
			nodeIP: netutils.ParseIPSloppy("0.0.0.0"),
			nodeAddresses: []v1.NodeAddress{
				{Type: v1.NodeInternalIP, Address: "fc01:1234::5678"},
				{Type: v1.NodeInternalIP, Address: "10.1.1.1"},
				{Type: v1.NodeHostName, Address: testKubeletHostname},
			},
			expectedAddresses: []v1.NodeAddress{
				{Type: v1.NodeInternalIP, Address: "10.1.1.1"},
				{Type: v1.NodeHostName, Address: testKubeletHostname},
				{Type: v1.NodeInternalIP, Address: "fc01:1234::5678"},
			},
			shouldError: false,
		},
		{
			name:   "Dual-stack cloud, IPv4 first, request IPv6",
			nodeIP: netutils.ParseIPSloppy("::"),
			nodeAddresses: []v1.NodeAddress{
				{Type: v1.NodeInternalIP, Address: "10.1.1.1"},
				{Type: v1.NodeInternalIP, Address: "fc01:1234::5678"},
				{Type: v1.NodeHostName, Address: testKubeletHostname},
			},
			expectedAddresses: []v1.NodeAddress{
				{Type: v1.NodeInternalIP, Address: "fc01:1234::5678"},
				{Type: v1.NodeHostName, Address: testKubeletHostname},
				{Type: v1.NodeInternalIP, Address: "10.1.1.1"},
			},
			shouldError: false,
		},
		{
			name:   "Dual-stack cloud, IPv6 first, request IPv6",
			nodeIP: netutils.ParseIPSloppy("::"),
			nodeAddresses: []v1.NodeAddress{
				{Type: v1.NodeInternalIP, Address: "fc01:1234::5678"},
				{Type: v1.NodeInternalIP, Address: "10.1.1.1"},
				{Type: v1.NodeHostName, Address: testKubeletHostname},
			},
			expectedAddresses: []v1.NodeAddress{
				{Type: v1.NodeInternalIP, Address: "fc01:1234::5678"},
				{Type: v1.NodeHostName, Address: testKubeletHostname},
				{Type: v1.NodeInternalIP, Address: "10.1.1.1"},
			},
			shouldError: false,
		},
		{
			name:              "Legacy cloud provider gets nodeIP annotation",
			nodeIP:            netutils.ParseIPSloppy("10.1.1.1"),
			cloudProviderType: cloudProviderLegacy,
			nodeAddresses: []v1.NodeAddress{
				{Type: v1.NodeInternalIP, Address: "10.1.1.1"},
				{Type: v1.NodeHostName, Address: testKubeletHostname},
			},
			expectedAddresses: []v1.NodeAddress{
				{Type: v1.NodeInternalIP, Address: "10.1.1.1"},
				{Type: v1.NodeHostName, Address: testKubeletHostname},
			},
			expectedAnnotations: map[string]string{
				"alpha.kubernetes.io/provided-node-ip": "10.1.1.1",
			},
			shouldError: false,
		},
		{
			name:              "External cloud provider gets nodeIP annotation",
			nodeIP:            netutils.ParseIPSloppy("10.1.1.1"),
			cloudProviderType: cloudProviderExternal,
			nodeAddresses: []v1.NodeAddress{
				{Type: v1.NodeInternalIP, Address: "10.1.1.1"},
				{Type: v1.NodeHostName, Address: testKubeletHostname},
			},
			expectedAddresses: []v1.NodeAddress{
				{Type: v1.NodeInternalIP, Address: "10.1.1.1"},
				{Type: v1.NodeHostName, Address: testKubeletHostname},
			},
			expectedAnnotations: map[string]string{
				"alpha.kubernetes.io/provided-node-ip": "10.1.1.1",
			},
			shouldError: false,
		},
		{
			name:                           "External cloud provider, node address is already set",
			nodeIP:                         netutils.ParseIPSloppy("10.1.1.1"),
			cloudProviderType:              cloudProviderExternal,
			nodeAddresses:                  []v1.NodeAddress{existingNodeAddress},
			expectedAddresses:              []v1.NodeAddress{existingNodeAddress},
			shouldError:                    true,
			shouldSetNodeAddressBeforeTest: true,
		},
		{
			name:              "No cloud provider does not get nodeIP annotation",
			nodeIP:            netutils.ParseIPSloppy("10.1.1.1"),
			cloudProviderType: cloudProviderNone,
			nodeAddresses: []v1.NodeAddress{
				{Type: v1.NodeInternalIP, Address: "10.1.1.1"},
				{Type: v1.NodeHostName, Address: testKubeletHostname},
			},
			expectedAddresses: []v1.NodeAddress{
				{Type: v1.NodeInternalIP, Address: "10.1.1.1"},
				{Type: v1.NodeHostName, Address: testKubeletHostname},
			},
			expectedAnnotations: map[string]string{},
			shouldError:         false,
		},
		{
			name:              "Stale nodeIP annotation is removed when not using cloud provider",
			nodeIP:            netutils.ParseIPSloppy("10.1.1.1"),
			cloudProviderType: cloudProviderNone,
			nodeAddresses: []v1.NodeAddress{
				{Type: v1.NodeInternalIP, Address: "10.1.1.1"},
				{Type: v1.NodeHostName, Address: testKubeletHostname},
			},
			expectedAddresses: []v1.NodeAddress{
				{Type: v1.NodeInternalIP, Address: "10.1.1.1"},
				{Type: v1.NodeHostName, Address: testKubeletHostname},
			},
			existingAnnotations: map[string]string{
				"alpha.kubernetes.io/provided-node-ip": "10.1.1.3",
			},
			expectedAnnotations: map[string]string{},
			shouldError:         false,
		},
		{
			name:              "Stale nodeIP annotation is removed when using cloud provider but no --node-ip",
			nodeIP:            nil,
			cloudProviderType: cloudProviderLegacy,
			nodeAddresses: []v1.NodeAddress{
				{Type: v1.NodeInternalIP, Address: "10.1.1.1"},
				{Type: v1.NodeHostName, Address: testKubeletHostname},
			},
			expectedAddresses: []v1.NodeAddress{
				{Type: v1.NodeInternalIP, Address: "10.1.1.1"},
				{Type: v1.NodeHostName, Address: testKubeletHostname},
			},
			existingAnnotations: map[string]string{
				"alpha.kubernetes.io/provided-node-ip": "10.1.1.1",
			},
			expectedAnnotations: map[string]string{},
			shouldError:         false,
		},
		{
			name:              "Incorrect nodeIP annotation is fixed",
			nodeIP:            netutils.ParseIPSloppy("10.1.1.1"),
			cloudProviderType: cloudProviderExternal,
			nodeAddresses: []v1.NodeAddress{
				{Type: v1.NodeInternalIP, Address: "10.1.1.1"},
				{Type: v1.NodeHostName, Address: testKubeletHostname},
			},
			expectedAddresses: []v1.NodeAddress{
				{Type: v1.NodeInternalIP, Address: "10.1.1.1"},
				{Type: v1.NodeHostName, Address: testKubeletHostname},
			},
			existingAnnotations: map[string]string{
				"alpha.kubernetes.io/provided-node-ip": "10.1.1.3",
			},
			expectedAnnotations: map[string]string{
				"alpha.kubernetes.io/provided-node-ip": "10.1.1.1",
			},
			shouldError: false,
		},
		{
			// We don't have to test "legacy cloud provider with dual-stack
			// IPs" etc because we won't have gotten this far with an invalid
			// config like that.
			name:              "Dual-stack cloud, with dual-stack nodeIPs",
			nodeIP:            netutils.ParseIPSloppy("2600:1f14:1d4:d101::ba3d"),
			secondaryNodeIP:   netutils.ParseIPSloppy("10.1.1.2"),
			cloudProviderType: cloudProviderExternal,
			nodeAddresses: []v1.NodeAddress{
				{Type: v1.NodeInternalIP, Address: "10.1.1.1"},
				{Type: v1.NodeInternalIP, Address: "10.1.1.2"},
				{Type: v1.NodeInternalIP, Address: "2600:1f14:1d4:d101::ba3d"},
				{Type: v1.NodeHostName, Address: testKubeletHostname},
			},
			expectedAddresses: []v1.NodeAddress{
				{Type: v1.NodeInternalIP, Address: "2600:1f14:1d4:d101::ba3d"},
				{Type: v1.NodeInternalIP, Address: "10.1.1.2"},
				{Type: v1.NodeHostName, Address: testKubeletHostname},
			},
			expectedAnnotations: map[string]string{
				"alpha.kubernetes.io/provided-node-ip": "2600:1f14:1d4:d101::ba3d,10.1.1.2",
			},
			shouldError: false,
		},
		{
			name:              "Upgrade to cloud dual-stack nodeIPs",
			nodeIP:            netutils.ParseIPSloppy("10.1.1.1"),
			secondaryNodeIP:   netutils.ParseIPSloppy("2600:1f14:1d4:d101::ba3d"),
			cloudProviderType: cloudProviderExternal,
			nodeAddresses: []v1.NodeAddress{
				{Type: v1.NodeInternalIP, Address: "10.1.1.1"},
				{Type: v1.NodeInternalIP, Address: "2600:1f14:1d4:d101::ba3d"},
				{Type: v1.NodeHostName, Address: testKubeletHostname},
			},
			expectedAddresses: []v1.NodeAddress{
				{Type: v1.NodeInternalIP, Address: "10.1.1.1"},
				{Type: v1.NodeInternalIP, Address: "2600:1f14:1d4:d101::ba3d"},
				{Type: v1.NodeHostName, Address: testKubeletHostname},
			},
			existingAnnotations: map[string]string{
				"alpha.kubernetes.io/provided-node-ip": "10.1.1.1",
			},
			expectedAnnotations: map[string]string{
				"alpha.kubernetes.io/provided-node-ip": "10.1.1.1,2600:1f14:1d4:d101::ba3d",
			},
			shouldError: false,
		},
		{
			name:              "Downgrade from cloud dual-stack nodeIPs",
			nodeIP:            netutils.ParseIPSloppy("10.1.1.1"),
			cloudProviderType: cloudProviderExternal,
			nodeAddresses: []v1.NodeAddress{
				{Type: v1.NodeInternalIP, Address: "10.1.1.1"},
				{Type: v1.NodeInternalIP, Address: "2600:1f14:1d4:d101::ba3d"},
				{Type: v1.NodeHostName, Address: testKubeletHostname},
			},
			expectedAddresses: []v1.NodeAddress{
				{Type: v1.NodeInternalIP, Address: "10.1.1.1"},
				{Type: v1.NodeHostName, Address: testKubeletHostname},
			},
			existingAnnotations: map[string]string{
				"alpha.kubernetes.io/provided-node-ip": "10.1.1.1,2600:1f14:1d4:d101::ba3d",
			},
			expectedAnnotations: map[string]string{
				"alpha.kubernetes.io/provided-node-ip": "10.1.1.1",
			},
			shouldError: false,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			ctx := context.Background()
			// testCase setup
			existingNode := &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        testKubeletHostname,
					Annotations: testCase.existingAnnotations,
				},
				Spec: v1.NodeSpec{},
				Status: v1.NodeStatus{
					Addresses: []v1.NodeAddress{},
				},
			}

			if testCase.shouldSetNodeAddressBeforeTest {
				existingNode.Status.Addresses = append(existingNode.Status.Addresses, existingNodeAddress)
			}

			nodeIPValidator := func(nodeIP net.IP) error {
				return nil
			}
			hostname := testKubeletHostname

			nodeAddressesFunc := func() ([]v1.NodeAddress, error) {
				return testCase.nodeAddresses, nil
			}

			// cloud provider is expected to be nil if external provider is set or there is no cloud provider
			var cloud cloudprovider.Interface
			if testCase.cloudProviderType == cloudProviderLegacy {
				cloud = &fakecloud.Cloud{
					Addresses: testCase.nodeAddresses,
					Err:       nil,
				}
			}

			nodeIPs := []net.IP{testCase.nodeIP}
			if testCase.secondaryNodeIP != nil {
				nodeIPs = append(nodeIPs, testCase.secondaryNodeIP)
			}

			// construct setter
			setter := NodeAddress(nodeIPs,
				nodeIPValidator,
				hostname,
				testCase.hostnameOverride,
				testCase.cloudProviderType == cloudProviderExternal,
				cloud,
				nodeAddressesFunc)

			// call setter on existing node
			err := setter(ctx, existingNode)
			if err != nil && !testCase.shouldError {
				t.Fatalf("unexpected error: %v", err)
			} else if err != nil && testCase.shouldError {
				// expected an error, and got one, so just return early here
				return
			}

			assert.True(t, apiequality.Semantic.DeepEqual(testCase.expectedAddresses, existingNode.Status.Addresses),
				"Diff: %s", cmp.Diff(testCase.expectedAddresses, existingNode.Status.Addresses))
			if testCase.expectedAnnotations != nil {
				assert.True(t, apiequality.Semantic.DeepEqual(testCase.expectedAnnotations, existingNode.Annotations),
					"Diff: %s", cmp.Diff(testCase.expectedAnnotations, existingNode.Annotations))
			}
		})
	}
}

// We can't test failure or autodetection cases here because the relevant code isn't mockable
func TestNodeAddress_NoCloudProvider(t *testing.T) {
	cases := []struct {
		name              string
		nodeIPs           []net.IP
		expectedAddresses []v1.NodeAddress
		shouldError       bool
	}{
		{
			name:    "Single --node-ip",
			nodeIPs: []net.IP{netutils.ParseIPSloppy("10.1.1.1")},
			expectedAddresses: []v1.NodeAddress{
				{Type: v1.NodeInternalIP, Address: "10.1.1.1"},
				{Type: v1.NodeHostName, Address: testKubeletHostname},
			},
		},
		{
			name:        "Invalid single --node-ip (using loopback)",
			nodeIPs:     []net.IP{netutils.ParseIPSloppy("127.0.0.1")},
			shouldError: true,
		},
		{
			name:    "Dual --node-ips",
			nodeIPs: []net.IP{netutils.ParseIPSloppy("10.1.1.1"), netutils.ParseIPSloppy("fd01::1234")},
			expectedAddresses: []v1.NodeAddress{
				{Type: v1.NodeInternalIP, Address: "10.1.1.1"},
				{Type: v1.NodeInternalIP, Address: "fd01::1234"},
				{Type: v1.NodeHostName, Address: testKubeletHostname},
			},
		},
		{
			name:        "Dual --node-ips but with invalid secondary IP (using multicast IP)",
			nodeIPs:     []net.IP{netutils.ParseIPSloppy("10.1.1.1"), netutils.ParseIPSloppy("224.0.0.0")},
			shouldError: true,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			ctx := context.Background()
			// testCase setup
			existingNode := &v1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: testKubeletHostname, Annotations: make(map[string]string)},
				Spec:       v1.NodeSpec{},
				Status: v1.NodeStatus{
					Addresses: []v1.NodeAddress{},
				},
			}

			nodeIPValidator := func(nodeIP net.IP) error {
				if nodeIP.IsLoopback() {
					return fmt.Errorf("nodeIP can't be loopback address")
				} else if nodeIP.IsMulticast() {
					return fmt.Errorf("nodeIP can't be a multicast address")
				}
				return nil
			}
			nodeAddressesFunc := func() ([]v1.NodeAddress, error) {
				return nil, fmt.Errorf("not reached")
			}

			// construct setter
			setter := NodeAddress(testCase.nodeIPs,
				nodeIPValidator,
				testKubeletHostname,
				false, // hostnameOverridden
				false, // externalCloudProvider
				nil,   // cloud
				nodeAddressesFunc)

			// call setter on existing node
			err := setter(ctx, existingNode)
			if testCase.shouldError && err == nil {
				t.Fatal("expected error but no error returned")
			}
			if err != nil && !testCase.shouldError {
				t.Fatalf("unexpected error: %v", err)
			}

			assert.True(t, apiequality.Semantic.DeepEqual(testCase.expectedAddresses, existingNode.Status.Addresses),
				"Diff: %s", cmp.Diff(testCase.expectedAddresses, existingNode.Status.Addresses))
		})
	}
}

func TestMachineInfo(t *testing.T) {
	const nodeName = "test-node"

	type dprc struct {
		capacity    v1.ResourceList
		allocatable v1.ResourceList
		inactive    []string
	}

	cases := []struct {
		desc                                 string
		node                                 *v1.Node
		maxPods                              int
		podsPerCore                          int
		machineInfo                          *cadvisorapiv1.MachineInfo
		machineInfoError                     error
		capacity                             v1.ResourceList
		devicePluginResourceCapacity         dprc
		nodeAllocatableReservation           v1.ResourceList
		expectNode                           *v1.Node
		expectEvents                         []testEvent
		disableLocalStorageCapacityIsolation bool
	}{
		{
			desc:    "machine identifiers, basic capacity and allocatable",
			node:    &v1.Node{},
			maxPods: 110,
			machineInfo: &cadvisorapiv1.MachineInfo{
				MachineID:      "MachineID",
				SystemUUID:     "SystemUUID",
				NumCores:       2,
				MemoryCapacity: 1024,
			},
			expectNode: &v1.Node{
				Status: v1.NodeStatus{
					NodeInfo: v1.NodeSystemInfo{
						MachineID:  "MachineID",
						SystemUUID: "SystemUUID",
					},
					Capacity: v1.ResourceList{
						v1.ResourceCPU:    *resource.NewMilliQuantity(2000, resource.DecimalSI),
						v1.ResourceMemory: *resource.NewQuantity(1024, resource.BinarySI),
						v1.ResourcePods:   *resource.NewQuantity(110, resource.DecimalSI),
					},
					Allocatable: v1.ResourceList{
						v1.ResourceCPU:    *resource.NewMilliQuantity(2000, resource.DecimalSI),
						v1.ResourceMemory: *resource.NewQuantity(1024, resource.BinarySI),
						v1.ResourcePods:   *resource.NewQuantity(110, resource.DecimalSI),
					},
				},
			},
		},
		{
			desc:        "podsPerCore greater than zero, but less than maxPods/cores",
			node:        &v1.Node{},
			maxPods:     10,
			podsPerCore: 4,
			machineInfo: &cadvisorapiv1.MachineInfo{
				NumCores:       2,
				MemoryCapacity: 1024,
			},
			expectNode: &v1.Node{
				Status: v1.NodeStatus{
					Capacity: v1.ResourceList{
						v1.ResourceCPU:    *resource.NewMilliQuantity(2000, resource.DecimalSI),
						v1.ResourceMemory: *resource.NewQuantity(1024, resource.BinarySI),
						v1.ResourcePods:   *resource.NewQuantity(8, resource.DecimalSI),
					},
					Allocatable: v1.ResourceList{
						v1.ResourceCPU:    *resource.NewMilliQuantity(2000, resource.DecimalSI),
						v1.ResourceMemory: *resource.NewQuantity(1024, resource.BinarySI),
						v1.ResourcePods:   *resource.NewQuantity(8, resource.DecimalSI),
					},
				},
			},
		},
		{
			desc:        "podsPerCore greater than maxPods/cores",
			node:        &v1.Node{},
			maxPods:     10,
			podsPerCore: 6,
			machineInfo: &cadvisorapiv1.MachineInfo{
				NumCores:       2,
				MemoryCapacity: 1024,
			},
			expectNode: &v1.Node{
				Status: v1.NodeStatus{
					Capacity: v1.ResourceList{
						v1.ResourceCPU:    *resource.NewMilliQuantity(2000, resource.DecimalSI),
						v1.ResourceMemory: *resource.NewQuantity(1024, resource.BinarySI),
						v1.ResourcePods:   *resource.NewQuantity(10, resource.DecimalSI),
					},
					Allocatable: v1.ResourceList{
						v1.ResourceCPU:    *resource.NewMilliQuantity(2000, resource.DecimalSI),
						v1.ResourceMemory: *resource.NewQuantity(1024, resource.BinarySI),
						v1.ResourcePods:   *resource.NewQuantity(10, resource.DecimalSI),
					},
				},
			},
		},
		{
			desc:    "allocatable should equal capacity minus reservations",
			node:    &v1.Node{},
			maxPods: 110,
			machineInfo: &cadvisorapiv1.MachineInfo{
				NumCores:       2,
				MemoryCapacity: 1024,
			},
			nodeAllocatableReservation: v1.ResourceList{
				// reserve 1 unit for each resource
				v1.ResourceCPU:              *resource.NewMilliQuantity(1, resource.DecimalSI),
				v1.ResourceMemory:           *resource.NewQuantity(1, resource.BinarySI),
				v1.ResourcePods:             *resource.NewQuantity(1, resource.DecimalSI),
				v1.ResourceEphemeralStorage: *resource.NewQuantity(1, resource.BinarySI),
			},
			expectNode: &v1.Node{
				Status: v1.NodeStatus{
					Capacity: v1.ResourceList{
						v1.ResourceCPU:    *resource.NewMilliQuantity(2000, resource.DecimalSI),
						v1.ResourceMemory: *resource.NewQuantity(1024, resource.BinarySI),
						v1.ResourcePods:   *resource.NewQuantity(110, resource.DecimalSI),
					},
					Allocatable: v1.ResourceList{
						v1.ResourceCPU:    *resource.NewMilliQuantity(1999, resource.DecimalSI),
						v1.ResourceMemory: *resource.NewQuantity(1023, resource.BinarySI),
						v1.ResourcePods:   *resource.NewQuantity(109, resource.DecimalSI),
					},
				},
			},
		},
		{
			desc: "allocatable memory does not double-count hugepages reservations",
			node: &v1.Node{
				Status: v1.NodeStatus{
					Capacity: v1.ResourceList{
						// it's impossible on any real system to reserve 1 byte,
						// but we just need to test that the setter does the math
						v1.ResourceHugePagesPrefix + "test": *resource.NewQuantity(1, resource.BinarySI),
					},
				},
			},
			maxPods: 110,
			machineInfo: &cadvisorapiv1.MachineInfo{
				NumCores:       2,
				MemoryCapacity: 1024,
			},
			expectNode: &v1.Node{
				Status: v1.NodeStatus{
					Capacity: v1.ResourceList{
						v1.ResourceCPU:                      *resource.NewMilliQuantity(2000, resource.DecimalSI),
						v1.ResourceMemory:                   *resource.NewQuantity(1024, resource.BinarySI),
						v1.ResourceHugePagesPrefix + "test": *resource.NewQuantity(1, resource.BinarySI),
						v1.ResourcePods:                     *resource.NewQuantity(110, resource.DecimalSI),
					},
					Allocatable: v1.ResourceList{
						v1.ResourceCPU: *resource.NewMilliQuantity(2000, resource.DecimalSI),
						// memory has 1-unit difference for hugepages reservation
						v1.ResourceMemory:                   *resource.NewQuantity(1023, resource.BinarySI),
						v1.ResourceHugePagesPrefix + "test": *resource.NewQuantity(1, resource.BinarySI),
						v1.ResourcePods:                     *resource.NewQuantity(110, resource.DecimalSI),
					},
				},
			},
		},
		{
			desc: "negative capacity resources should be set to 0 in allocatable",
			node: &v1.Node{
				Status: v1.NodeStatus{
					Capacity: v1.ResourceList{
						"negative-resource": *resource.NewQuantity(-1, resource.BinarySI),
					},
				},
			},
			maxPods: 110,
			machineInfo: &cadvisorapiv1.MachineInfo{
				NumCores:       2,
				MemoryCapacity: 1024,
			},
			expectNode: &v1.Node{
				Status: v1.NodeStatus{
					Capacity: v1.ResourceList{
						v1.ResourceCPU:      *resource.NewMilliQuantity(2000, resource.DecimalSI),
						v1.ResourceMemory:   *resource.NewQuantity(1024, resource.BinarySI),
						v1.ResourcePods:     *resource.NewQuantity(110, resource.DecimalSI),
						"negative-resource": *resource.NewQuantity(-1, resource.BinarySI),
					},
					Allocatable: v1.ResourceList{
						v1.ResourceCPU:      *resource.NewMilliQuantity(2000, resource.DecimalSI),
						v1.ResourceMemory:   *resource.NewQuantity(1024, resource.BinarySI),
						v1.ResourcePods:     *resource.NewQuantity(110, resource.DecimalSI),
						"negative-resource": *resource.NewQuantity(0, resource.BinarySI),
					},
				},
			},
		},
		{
			desc: "hugepages reservation greater than node memory capacity should result in memory capacity set to 0",
			node: &v1.Node{
				Status: v1.NodeStatus{
					Capacity: v1.ResourceList{
						v1.ResourceHugePagesPrefix + "test": *resource.NewQuantity(1025, resource.BinarySI),
					},
				},
			},
			maxPods: 110,
			machineInfo: &cadvisorapiv1.MachineInfo{
				NumCores:       2,
				MemoryCapacity: 1024,
			},
			expectNode: &v1.Node{
				Status: v1.NodeStatus{
					Capacity: v1.ResourceList{
						v1.ResourceCPU:                      *resource.NewMilliQuantity(2000, resource.DecimalSI),
						v1.ResourceMemory:                   *resource.NewQuantity(1024, resource.BinarySI),
						v1.ResourcePods:                     *resource.NewQuantity(110, resource.DecimalSI),
						v1.ResourceHugePagesPrefix + "test": *resource.NewQuantity(1025, resource.BinarySI),
					},
					Allocatable: v1.ResourceList{
						v1.ResourceCPU:                      *resource.NewMilliQuantity(2000, resource.DecimalSI),
						v1.ResourceMemory:                   *resource.NewQuantity(0, resource.BinarySI),
						v1.ResourcePods:                     *resource.NewQuantity(110, resource.DecimalSI),
						v1.ResourceHugePagesPrefix + "test": *resource.NewQuantity(1025, resource.BinarySI),
					},
				},
			},
		},
		{
			desc:    "ephemeral storage is reflected in capacity and allocatable",
			node:    &v1.Node{},
			maxPods: 110,
			machineInfo: &cadvisorapiv1.MachineInfo{
				NumCores:       2,
				MemoryCapacity: 1024,
			},
			capacity: v1.ResourceList{
				v1.ResourceEphemeralStorage: *resource.NewQuantity(5000, resource.BinarySI),
			},
			expectNode: &v1.Node{
				Status: v1.NodeStatus{
					Capacity: v1.ResourceList{
						v1.ResourceCPU:              *resource.NewMilliQuantity(2000, resource.DecimalSI),
						v1.ResourceMemory:           *resource.NewQuantity(1024, resource.BinarySI),
						v1.ResourcePods:             *resource.NewQuantity(110, resource.DecimalSI),
						v1.ResourceEphemeralStorage: *resource.NewQuantity(5000, resource.BinarySI),
					},
					Allocatable: v1.ResourceList{
						v1.ResourceCPU:              *resource.NewMilliQuantity(2000, resource.DecimalSI),
						v1.ResourceMemory:           *resource.NewQuantity(1024, resource.BinarySI),
						v1.ResourcePods:             *resource.NewQuantity(110, resource.DecimalSI),
						v1.ResourceEphemeralStorage: *resource.NewQuantity(5000, resource.BinarySI),
					},
				},
			},
		},
		{
			desc:    "ephemeral storage is not reflected in capacity and allocatable because localStorageCapacityIsolation is disabled",
			node:    &v1.Node{},
			maxPods: 110,
			machineInfo: &cadvisorapiv1.MachineInfo{
				NumCores:       2,
				MemoryCapacity: 1024,
			},
			capacity: v1.ResourceList{
				v1.ResourceEphemeralStorage: *resource.NewQuantity(5000, resource.BinarySI),
			},
			expectNode: &v1.Node{
				Status: v1.NodeStatus{
					Capacity: v1.ResourceList{
						v1.ResourceCPU:              *resource.NewMilliQuantity(2000, resource.DecimalSI),
						v1.ResourceMemory:           *resource.NewQuantity(1024, resource.BinarySI),
						v1.ResourcePods:             *resource.NewQuantity(110, resource.DecimalSI),
						v1.ResourceEphemeralStorage: *resource.NewQuantity(5000, resource.BinarySI),
					},
					Allocatable: v1.ResourceList{
						v1.ResourceCPU:              *resource.NewMilliQuantity(2000, resource.DecimalSI),
						v1.ResourceMemory:           *resource.NewQuantity(1024, resource.BinarySI),
						v1.ResourcePods:             *resource.NewQuantity(110, resource.DecimalSI),
						v1.ResourceEphemeralStorage: *resource.NewQuantity(5000, resource.BinarySI),
					},
				},
			},
			disableLocalStorageCapacityIsolation: true,
		},
		{
			desc:    "device plugin resources are reflected in capacity and allocatable",
			node:    &v1.Node{},
			maxPods: 110,
			machineInfo: &cadvisorapiv1.MachineInfo{
				NumCores:       2,
				MemoryCapacity: 1024,
			},
			devicePluginResourceCapacity: dprc{
				capacity: v1.ResourceList{
					"device-plugin": *resource.NewQuantity(1, resource.BinarySI),
				},
				allocatable: v1.ResourceList{
					"device-plugin": *resource.NewQuantity(1, resource.BinarySI),
				},
			},
			expectNode: &v1.Node{
				Status: v1.NodeStatus{
					Capacity: v1.ResourceList{
						v1.ResourceCPU:    *resource.NewMilliQuantity(2000, resource.DecimalSI),
						v1.ResourceMemory: *resource.NewQuantity(1024, resource.BinarySI),
						v1.ResourcePods:   *resource.NewQuantity(110, resource.DecimalSI),
						"device-plugin":   *resource.NewQuantity(1, resource.BinarySI),
					},
					Allocatable: v1.ResourceList{
						v1.ResourceCPU:    *resource.NewMilliQuantity(2000, resource.DecimalSI),
						v1.ResourceMemory: *resource.NewQuantity(1024, resource.BinarySI),
						v1.ResourcePods:   *resource.NewQuantity(110, resource.DecimalSI),
						"device-plugin":   *resource.NewQuantity(1, resource.BinarySI),
					},
				},
			},
		},
		{
			desc: "inactive device plugin resources should have their capacity set to 0",
			node: &v1.Node{
				Status: v1.NodeStatus{
					Capacity: v1.ResourceList{
						"inactive": *resource.NewQuantity(1, resource.BinarySI),
					},
				},
			},
			maxPods: 110,
			machineInfo: &cadvisorapiv1.MachineInfo{
				NumCores:       2,
				MemoryCapacity: 1024,
			},
			devicePluginResourceCapacity: dprc{
				inactive: []string{"inactive"},
			},
			expectNode: &v1.Node{
				Status: v1.NodeStatus{
					Capacity: v1.ResourceList{
						v1.ResourceCPU:    *resource.NewMilliQuantity(2000, resource.DecimalSI),
						v1.ResourceMemory: *resource.NewQuantity(1024, resource.BinarySI),
						v1.ResourcePods:   *resource.NewQuantity(110, resource.DecimalSI),
						"inactive":        *resource.NewQuantity(0, resource.BinarySI),
					},
					Allocatable: v1.ResourceList{
						v1.ResourceCPU:    *resource.NewMilliQuantity(2000, resource.DecimalSI),
						v1.ResourceMemory: *resource.NewQuantity(1024, resource.BinarySI),
						v1.ResourcePods:   *resource.NewQuantity(110, resource.DecimalSI),
						"inactive":        *resource.NewQuantity(0, resource.BinarySI),
					},
				},
			},
		},
		{
			desc: "extended resources not present in capacity are removed from allocatable",
			node: &v1.Node{
				Status: v1.NodeStatus{
					Allocatable: v1.ResourceList{
						"example.com/extended": *resource.NewQuantity(1, resource.BinarySI),
					},
				},
			},
			maxPods: 110,
			machineInfo: &cadvisorapiv1.MachineInfo{
				NumCores:       2,
				MemoryCapacity: 1024,
			},
			expectNode: &v1.Node{
				Status: v1.NodeStatus{
					Capacity: v1.ResourceList{
						v1.ResourceCPU:    *resource.NewMilliQuantity(2000, resource.DecimalSI),
						v1.ResourceMemory: *resource.NewQuantity(1024, resource.BinarySI),
						v1.ResourcePods:   *resource.NewQuantity(110, resource.DecimalSI),
					},
					Allocatable: v1.ResourceList{
						v1.ResourceCPU:    *resource.NewMilliQuantity(2000, resource.DecimalSI),
						v1.ResourceMemory: *resource.NewQuantity(1024, resource.BinarySI),
						v1.ResourcePods:   *resource.NewQuantity(110, resource.DecimalSI),
					},
				},
			},
		},
		{
			desc:    "on failure to get machine info, allocatable and capacity for memory and cpu are set to 0, pods to maxPods",
			node:    &v1.Node{},
			maxPods: 110,
			// podsPerCore is not accounted for when getting machine info fails
			podsPerCore:      1,
			machineInfoError: fmt.Errorf("foo"),
			expectNode: &v1.Node{
				Status: v1.NodeStatus{
					Capacity: v1.ResourceList{
						v1.ResourceCPU:    *resource.NewMilliQuantity(0, resource.DecimalSI),
						v1.ResourceMemory: resource.MustParse("0Gi"),
						v1.ResourcePods:   *resource.NewQuantity(110, resource.DecimalSI),
					},
					Allocatable: v1.ResourceList{
						v1.ResourceCPU:    *resource.NewMilliQuantity(0, resource.DecimalSI),
						v1.ResourceMemory: resource.MustParse("0Gi"),
						v1.ResourcePods:   *resource.NewQuantity(110, resource.DecimalSI),
					},
				},
			},
		},
		{
			desc: "node reboot event is recorded",
			node: &v1.Node{
				Status: v1.NodeStatus{
					NodeInfo: v1.NodeSystemInfo{
						BootID: "foo",
					},
				},
			},
			maxPods: 110,
			machineInfo: &cadvisorapiv1.MachineInfo{
				BootID:         "bar",
				NumCores:       2,
				MemoryCapacity: 1024,
			},
			expectNode: &v1.Node{
				Status: v1.NodeStatus{
					NodeInfo: v1.NodeSystemInfo{
						BootID: "bar",
					},
					Capacity: v1.ResourceList{
						v1.ResourceCPU:    *resource.NewMilliQuantity(2000, resource.DecimalSI),
						v1.ResourceMemory: *resource.NewQuantity(1024, resource.BinarySI),
						v1.ResourcePods:   *resource.NewQuantity(110, resource.DecimalSI),
					},
					Allocatable: v1.ResourceList{
						v1.ResourceCPU:    *resource.NewMilliQuantity(2000, resource.DecimalSI),
						v1.ResourceMemory: *resource.NewQuantity(1024, resource.BinarySI),
						v1.ResourcePods:   *resource.NewQuantity(110, resource.DecimalSI),
					},
				},
			},
			expectEvents: []testEvent{
				{
					eventType: v1.EventTypeWarning,
					event:     events.NodeRebooted,
					message:   fmt.Sprintf("Node %s has been rebooted, boot id: %s", nodeName, "bar"),
				},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			ctx := context.Background()
			machineInfoFunc := func() (*cadvisorapiv1.MachineInfo, error) {
				return tc.machineInfo, tc.machineInfoError
			}
			capacityFunc := func(localStorageCapacityIsolation bool) v1.ResourceList {
				return tc.capacity
			}
			devicePluginResourceCapacityFunc := func() (v1.ResourceList, v1.ResourceList, []string) {
				c := tc.devicePluginResourceCapacity
				return c.capacity, c.allocatable, c.inactive
			}
			nodeAllocatableReservationFunc := func() v1.ResourceList {
				return tc.nodeAllocatableReservation
			}

			events := []testEvent{}
			recordEventFunc := func(eventType, event, message string) {
				events = append(events, testEvent{
					eventType: eventType,
					event:     event,
					message:   message,
				})
			}
			// construct setter
			setter := MachineInfo(nodeName, tc.maxPods, tc.podsPerCore, machineInfoFunc, capacityFunc,
				devicePluginResourceCapacityFunc, nodeAllocatableReservationFunc, recordEventFunc, tc.disableLocalStorageCapacityIsolation)
			// call setter on node
			if err := setter(ctx, tc.node); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			// check expected node
			assert.True(t, apiequality.Semantic.DeepEqual(tc.expectNode, tc.node),
				"Diff: %s", cmp.Diff(tc.expectNode, tc.node))
			// check expected events
			require.Equal(t, len(tc.expectEvents), len(events))
			for i := range tc.expectEvents {
				assert.Equal(t, tc.expectEvents[i], events[i])
			}
		})
	}

}

func TestVersionInfo(t *testing.T) {
	cases := []struct {
		desc                string
		node                *v1.Node
		versionInfo         *cadvisorapiv1.VersionInfo
		versionInfoError    error
		runtimeType         string
		runtimeVersion      kubecontainer.Version
		runtimeVersionError error
		expectNode          *v1.Node
		expectError         error
		kubeProxyVersion    bool
	}{
		{
			desc: "versions set in node info",
			node: &v1.Node{},
			versionInfo: &cadvisorapiv1.VersionInfo{
				KernelVersion:      "KernelVersion",
				ContainerOsVersion: "ContainerOSVersion",
			},
			runtimeType: "RuntimeType",
			runtimeVersion: &kubecontainertest.FakeVersion{
				Version: "RuntimeVersion",
			},
			expectNode: &v1.Node{
				Status: v1.NodeStatus{
					NodeInfo: v1.NodeSystemInfo{
						KernelVersion:           "KernelVersion",
						OSImage:                 "ContainerOSVersion",
						ContainerRuntimeVersion: "RuntimeType://RuntimeVersion",
						KubeletVersion:          version.Get().String(),
						KubeProxyVersion:        version.Get().String(),
					},
				},
			},
			kubeProxyVersion: true,
		},
		{
			desc:             "error getting version info",
			node:             &v1.Node{},
			versionInfoError: fmt.Errorf("foo"),
			expectNode:       &v1.Node{},
			expectError:      fmt.Errorf("error getting version info: foo"),
			kubeProxyVersion: true,
		},
		{
			desc:                "error getting runtime version results in Unknown runtime",
			node:                &v1.Node{},
			versionInfo:         &cadvisorapiv1.VersionInfo{},
			runtimeType:         "RuntimeType",
			runtimeVersionError: fmt.Errorf("foo"),
			expectNode: &v1.Node{
				Status: v1.NodeStatus{
					NodeInfo: v1.NodeSystemInfo{
						ContainerRuntimeVersion: "RuntimeType://Unknown",
						KubeletVersion:          version.Get().String(),
						KubeProxyVersion:        version.Get().String(),
					},
				},
			},
			kubeProxyVersion: true,
		},
		{
			desc: "DisableNodeKubeProxyVersion FeatureGate enable, versions set in node info",
			node: &v1.Node{},
			versionInfo: &cadvisorapiv1.VersionInfo{
				KernelVersion:      "KernelVersion",
				ContainerOsVersion: "ContainerOSVersion",
			},
			runtimeType: "RuntimeType",
			runtimeVersion: &kubecontainertest.FakeVersion{
				Version: "RuntimeVersion",
			},
			expectNode: &v1.Node{
				Status: v1.NodeStatus{
					NodeInfo: v1.NodeSystemInfo{
						KernelVersion:           "KernelVersion",
						OSImage:                 "ContainerOSVersion",
						ContainerRuntimeVersion: "RuntimeType://RuntimeVersion",
						KubeletVersion:          version.Get().String(),
					},
				},
			},
			kubeProxyVersion: false,
		},
		{
			desc: "DisableNodeKubeProxyVersion FeatureGate enable, KubeProxyVersion will be cleared if it is set.",
			node: &v1.Node{
				Status: v1.NodeStatus{
					NodeInfo: v1.NodeSystemInfo{
						KernelVersion:           "KernelVersion",
						OSImage:                 "ContainerOSVersion",
						ContainerRuntimeVersion: "RuntimeType://RuntimeVersion",
						KubeletVersion:          version.Get().String(),
						KubeProxyVersion:        version.Get().String(),
					},
				},
			},
			versionInfo: &cadvisorapiv1.VersionInfo{
				KernelVersion:      "KernelVersion",
				ContainerOsVersion: "ContainerOSVersion",
			},
			runtimeType: "RuntimeType",
			runtimeVersion: &kubecontainertest.FakeVersion{
				Version: "RuntimeVersion",
			},
			expectNode: &v1.Node{
				Status: v1.NodeStatus{
					NodeInfo: v1.NodeSystemInfo{
						KernelVersion:           "KernelVersion",
						OSImage:                 "ContainerOSVersion",
						ContainerRuntimeVersion: "RuntimeType://RuntimeVersion",
						KubeletVersion:          version.Get().String(),
					},
				},
			},
			kubeProxyVersion: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			defer featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.DisableNodeKubeProxyVersion, !tc.kubeProxyVersion)()

			ctx := context.Background()
			versionInfoFunc := func() (*cadvisorapiv1.VersionInfo, error) {
				return tc.versionInfo, tc.versionInfoError
			}
			runtimeTypeFunc := func() string {
				return tc.runtimeType
			}
			runtimeVersionFunc := func(_ context.Context) (kubecontainer.Version, error) {
				return tc.runtimeVersion, tc.runtimeVersionError
			}
			// construct setter
			setter := VersionInfo(versionInfoFunc, runtimeTypeFunc, runtimeVersionFunc)
			// call setter on node
			err := setter(ctx, tc.node)
			require.Equal(t, tc.expectError, err)
			// check expected node
			assert.True(t, apiequality.Semantic.DeepEqual(tc.expectNode, tc.node),
				"Diff: %s", cmp.Diff(tc.expectNode, tc.node))
		})
	}
}

func TestImages(t *testing.T) {
	const (
		minImageSize = 23 * 1024 * 1024
		maxImageSize = 1000 * 1024 * 1024
	)

	cases := []struct {
		desc           string
		maxImages      int32
		imageList      []kubecontainer.Image
		imageListError error
		expectError    error
	}{
		{
			desc:      "max images enforced",
			maxImages: 1,
			imageList: makeImageList(2, 1, minImageSize, maxImageSize),
		},
		{
			desc:      "no max images cap for -1",
			maxImages: -1,
			imageList: makeImageList(2, 1, minImageSize, maxImageSize),
		},
		{
			desc:      "max names per image enforced",
			maxImages: -1,
			imageList: makeImageList(1, MaxNamesPerImageInNodeStatus+1, minImageSize, maxImageSize),
		},
		{
			desc:      "images are sorted by size, descending",
			maxImages: -1,
			// makeExpectedImageList will sort them for expectedNode when the test case is run
			imageList: []kubecontainer.Image{{Size: 3}, {Size: 1}, {Size: 4}, {Size: 2}},
		},
		{
			desc:      "repo digests and tags both show up in image names",
			maxImages: -1,
			// makeExpectedImageList will use both digests and tags
			imageList: []kubecontainer.Image{
				{
					RepoDigests: []string{"foo", "bar"},
					RepoTags:    []string{"baz", "quux"},
				},
			},
		},
		{
			desc:           "error getting image list, image list on node is reset to empty",
			maxImages:      -1,
			imageListError: fmt.Errorf("foo"),
			expectError:    fmt.Errorf("error getting image list: foo"),
		},
	}

	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			ctx := context.Background()
			imageListFunc := func() ([]kubecontainer.Image, error) {
				// today, imageListFunc is expected to return a sorted list,
				// but we may choose to sort in the setter at some future point
				// (e.g. if the image cache stopped sorting for us)
				sort.Sort(sliceutils.ByImageSize(tc.imageList))
				return tc.imageList, tc.imageListError
			}
			// construct setter
			setter := Images(tc.maxImages, imageListFunc)
			// call setter on node
			node := &v1.Node{}
			err := setter(ctx, node)
			require.Equal(t, tc.expectError, err)
			// check expected node, image list should be reset to empty when there is an error
			expectNode := &v1.Node{}
			if err == nil {
				expectNode.Status.Images = makeExpectedImageList(tc.imageList, tc.maxImages, MaxNamesPerImageInNodeStatus)
			}
			assert.True(t, apiequality.Semantic.DeepEqual(expectNode, node),
				"Diff: %s", cmp.Diff(expectNode, node))
		})
	}

}

func TestReadyCondition(t *testing.T) {
	now := time.Now()
	before := now.Add(-time.Second)
	nowFunc := func() time.Time { return now }

	withCapacity := &v1.Node{
		Status: v1.NodeStatus{
			Capacity: v1.ResourceList{
				v1.ResourceCPU:              *resource.NewMilliQuantity(2000, resource.DecimalSI),
				v1.ResourceMemory:           *resource.NewQuantity(10e9, resource.BinarySI),
				v1.ResourcePods:             *resource.NewQuantity(100, resource.DecimalSI),
				v1.ResourceEphemeralStorage: *resource.NewQuantity(5000, resource.BinarySI),
			},
		},
	}

	withoutStorageCapacity := &v1.Node{
		Status: v1.NodeStatus{
			Capacity: v1.ResourceList{
				v1.ResourceCPU:    *resource.NewMilliQuantity(2000, resource.DecimalSI),
				v1.ResourceMemory: *resource.NewQuantity(10e9, resource.BinarySI),
				v1.ResourcePods:   *resource.NewQuantity(100, resource.DecimalSI),
			},
		},
	}

	cases := []struct {
		desc                                 string
		node                                 *v1.Node
		runtimeErrors                        error
		networkErrors                        error
		storageErrors                        error
		cmStatus                             cm.Status
		nodeShutdownManagerErrors            error
		expectConditions                     []v1.NodeCondition
		expectEvents                         []testEvent
		disableLocalStorageCapacityIsolation bool
	}{
		{
			desc:             "new, ready",
			node:             withCapacity.DeepCopy(),
			expectConditions: []v1.NodeCondition{*makeReadyCondition(true, "kubelet is posting ready status", now, now)},
			// TODO(mtaufen): The current behavior is that we don't send an event for the initial NodeReady condition,
			// the reason for this is unclear, so we may want to actually send an event, and change these test cases
			// to ensure an event is sent.
		},
		{
			desc: "new, ready: soft requirement warning",
			node: withCapacity.DeepCopy(),
			cmStatus: cm.Status{
				SoftRequirements: fmt.Errorf("foo"),
			},
			expectConditions: []v1.NodeCondition{*makeReadyCondition(true, "kubelet is posting ready status. WARNING: foo", now, now)},
		},
		{
			desc:             "new, not ready: storage errors",
			node:             withCapacity.DeepCopy(),
			storageErrors:    errors.New("some storage error"),
			expectConditions: []v1.NodeCondition{*makeReadyCondition(false, "some storage error", now, now)},
		},
		{
			desc:                      "new, not ready: shutdown active",
			node:                      withCapacity.DeepCopy(),
			nodeShutdownManagerErrors: errors.New("node is shutting down"),
			expectConditions:          []v1.NodeCondition{*makeReadyCondition(false, "node is shutting down", now, now)},
		},
		{
			desc:             "new, not ready: runtime and network errors",
			node:             withCapacity.DeepCopy(),
			runtimeErrors:    errors.New("runtime"),
			networkErrors:    errors.New("network"),
			expectConditions: []v1.NodeCondition{*makeReadyCondition(false, "[runtime, network]", now, now)},
		},
		{
			desc:             "new, not ready: missing capacities",
			node:             &v1.Node{},
			expectConditions: []v1.NodeCondition{*makeReadyCondition(false, "missing node capacity for resources: cpu, memory, pods, ephemeral-storage", now, now)},
		},
		{
			desc:                                 "new, ready: localStorageCapacityIsolation is not supported",
			node:                                 withoutStorageCapacity.DeepCopy(),
			disableLocalStorageCapacityIsolation: true,
			expectConditions:                     []v1.NodeCondition{*makeReadyCondition(true, "kubelet is posting ready status", now, now)},
		},
		// the transition tests ensure timestamps are set correctly, no need to test the entire condition matrix in this section
		{
			desc: "transition to ready",
			node: func() *v1.Node {
				node := withCapacity.DeepCopy()
				node.Status.Conditions = []v1.NodeCondition{*makeReadyCondition(false, "", before, before)}
				return node
			}(),
			expectConditions: []v1.NodeCondition{*makeReadyCondition(true, "kubelet is posting ready status", now, now)},
			expectEvents: []testEvent{
				{
					eventType: v1.EventTypeNormal,
					event:     events.NodeReady,
				},
			},
		},
		{
			desc: "transition to not ready",
			node: func() *v1.Node {
				node := withCapacity.DeepCopy()
				node.Status.Conditions = []v1.NodeCondition{*makeReadyCondition(true, "", before, before)}
				return node
			}(),
			runtimeErrors:    errors.New("foo"),
			expectConditions: []v1.NodeCondition{*makeReadyCondition(false, "foo", now, now)},
			expectEvents: []testEvent{
				{
					eventType: v1.EventTypeNormal,
					event:     events.NodeNotReady,
				},
			},
		},
		{
			desc: "ready, no transition",
			node: func() *v1.Node {
				node := withCapacity.DeepCopy()
				node.Status.Conditions = []v1.NodeCondition{*makeReadyCondition(true, "", before, before)}
				return node
			}(),
			expectConditions: []v1.NodeCondition{*makeReadyCondition(true, "kubelet is posting ready status", before, now)},
			expectEvents:     []testEvent{},
		},
		{
			desc: "not ready, no transition",
			node: func() *v1.Node {
				node := withCapacity.DeepCopy()
				node.Status.Conditions = []v1.NodeCondition{*makeReadyCondition(false, "", before, before)}
				return node
			}(),
			runtimeErrors:    errors.New("foo"),
			expectConditions: []v1.NodeCondition{*makeReadyCondition(false, "foo", before, now)},
			expectEvents:     []testEvent{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			ctx := context.Background()
			runtimeErrorsFunc := func() error {
				return tc.runtimeErrors
			}
			networkErrorsFunc := func() error {
				return tc.networkErrors
			}
			storageErrorsFunc := func() error {
				return tc.storageErrors
			}
			cmStatusFunc := func() cm.Status {
				return tc.cmStatus
			}
			nodeShutdownErrorsFunc := func() error {
				return tc.nodeShutdownManagerErrors
			}
			events := []testEvent{}
			recordEventFunc := func(eventType, event string) {
				events = append(events, testEvent{
					eventType: eventType,
					event:     event,
				})
			}
			// construct setter
			setter := ReadyCondition(nowFunc, runtimeErrorsFunc, networkErrorsFunc, storageErrorsFunc, cmStatusFunc, nodeShutdownErrorsFunc, recordEventFunc, !tc.disableLocalStorageCapacityIsolation)
			// call setter on node
			if err := setter(ctx, tc.node); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			// check expected condition
			assert.True(t, apiequality.Semantic.DeepEqual(tc.expectConditions, tc.node.Status.Conditions),
				"Diff: %s", cmp.Diff(tc.expectConditions, tc.node.Status.Conditions))
			// check expected events
			require.Equal(t, len(tc.expectEvents), len(events))
			for i := range tc.expectEvents {
				assert.Equal(t, tc.expectEvents[i], events[i])
			}
		})
	}
}

func TestMemoryPressureCondition(t *testing.T) {
	now := time.Now()
	before := now.Add(-time.Second)
	nowFunc := func() time.Time { return now }

	cases := []struct {
		desc             string
		node             *v1.Node
		pressure         bool
		expectConditions []v1.NodeCondition
		expectEvents     []testEvent
	}{
		{
			desc:             "new, no pressure",
			node:             &v1.Node{},
			pressure:         false,
			expectConditions: []v1.NodeCondition{*makeMemoryPressureCondition(false, now, now)},
			expectEvents: []testEvent{
				{
					eventType: v1.EventTypeNormal,
					event:     "NodeHasSufficientMemory",
				},
			},
		},
		{
			desc:             "new, pressure",
			node:             &v1.Node{},
			pressure:         true,
			expectConditions: []v1.NodeCondition{*makeMemoryPressureCondition(true, now, now)},
			expectEvents: []testEvent{
				{
					eventType: v1.EventTypeNormal,
					event:     "NodeHasInsufficientMemory",
				},
			},
		},
		{
			desc: "transition to pressure",
			node: &v1.Node{
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{*makeMemoryPressureCondition(false, before, before)},
				},
			},
			pressure:         true,
			expectConditions: []v1.NodeCondition{*makeMemoryPressureCondition(true, now, now)},
			expectEvents: []testEvent{
				{
					eventType: v1.EventTypeNormal,
					event:     "NodeHasInsufficientMemory",
				},
			},
		},
		{
			desc: "transition to no pressure",
			node: &v1.Node{
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{*makeMemoryPressureCondition(true, before, before)},
				},
			},
			pressure:         false,
			expectConditions: []v1.NodeCondition{*makeMemoryPressureCondition(false, now, now)},
			expectEvents: []testEvent{
				{
					eventType: v1.EventTypeNormal,
					event:     "NodeHasSufficientMemory",
				},
			},
		},
		{
			desc: "pressure, no transition",
			node: &v1.Node{
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{*makeMemoryPressureCondition(true, before, before)},
				},
			},
			pressure:         true,
			expectConditions: []v1.NodeCondition{*makeMemoryPressureCondition(true, before, now)},
			expectEvents:     []testEvent{},
		},
		{
			desc: "no pressure, no transition",
			node: &v1.Node{
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{*makeMemoryPressureCondition(false, before, before)},
				},
			},
			pressure:         false,
			expectConditions: []v1.NodeCondition{*makeMemoryPressureCondition(false, before, now)},
			expectEvents:     []testEvent{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			ctx := context.Background()
			events := []testEvent{}
			recordEventFunc := func(eventType, event string) {
				events = append(events, testEvent{
					eventType: eventType,
					event:     event,
				})
			}
			pressureFunc := func() bool {
				return tc.pressure
			}
			// construct setter
			setter := MemoryPressureCondition(nowFunc, pressureFunc, recordEventFunc)
			// call setter on node
			if err := setter(ctx, tc.node); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			// check expected condition
			assert.True(t, apiequality.Semantic.DeepEqual(tc.expectConditions, tc.node.Status.Conditions),
				"Diff: %s", cmp.Diff(tc.expectConditions, tc.node.Status.Conditions))
			// check expected events
			require.Equal(t, len(tc.expectEvents), len(events))
			for i := range tc.expectEvents {
				assert.Equal(t, tc.expectEvents[i], events[i])
			}
		})
	}
}

func TestPIDPressureCondition(t *testing.T) {
	now := time.Now()
	before := now.Add(-time.Second)
	nowFunc := func() time.Time { return now }

	cases := []struct {
		desc             string
		node             *v1.Node
		pressure         bool
		expectConditions []v1.NodeCondition
		expectEvents     []testEvent
	}{
		{
			desc:             "new, no pressure",
			node:             &v1.Node{},
			pressure:         false,
			expectConditions: []v1.NodeCondition{*makePIDPressureCondition(false, now, now)},
			expectEvents: []testEvent{
				{
					eventType: v1.EventTypeNormal,
					event:     "NodeHasSufficientPID",
				},
			},
		},
		{
			desc:             "new, pressure",
			node:             &v1.Node{},
			pressure:         true,
			expectConditions: []v1.NodeCondition{*makePIDPressureCondition(true, now, now)},
			expectEvents: []testEvent{
				{
					eventType: v1.EventTypeNormal,
					event:     "NodeHasInsufficientPID",
				},
			},
		},
		{
			desc: "transition to pressure",
			node: &v1.Node{
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{*makePIDPressureCondition(false, before, before)},
				},
			},
			pressure:         true,
			expectConditions: []v1.NodeCondition{*makePIDPressureCondition(true, now, now)},
			expectEvents: []testEvent{
				{
					eventType: v1.EventTypeNormal,
					event:     "NodeHasInsufficientPID",
				},
			},
		},
		{
			desc: "transition to no pressure",
			node: &v1.Node{
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{*makePIDPressureCondition(true, before, before)},
				},
			},
			pressure:         false,
			expectConditions: []v1.NodeCondition{*makePIDPressureCondition(false, now, now)},
			expectEvents: []testEvent{
				{
					eventType: v1.EventTypeNormal,
					event:     "NodeHasSufficientPID",
				},
			},
		},
		{
			desc: "pressure, no transition",
			node: &v1.Node{
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{*makePIDPressureCondition(true, before, before)},
				},
			},
			pressure:         true,
			expectConditions: []v1.NodeCondition{*makePIDPressureCondition(true, before, now)},
			expectEvents:     []testEvent{},
		},
		{
			desc: "no pressure, no transition",
			node: &v1.Node{
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{*makePIDPressureCondition(false, before, before)},
				},
			},
			pressure:         false,
			expectConditions: []v1.NodeCondition{*makePIDPressureCondition(false, before, now)},
			expectEvents:     []testEvent{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			ctx := context.Background()
			events := []testEvent{}
			recordEventFunc := func(eventType, event string) {
				events = append(events, testEvent{
					eventType: eventType,
					event:     event,
				})
			}
			pressureFunc := func() bool {
				return tc.pressure
			}
			// construct setter
			setter := PIDPressureCondition(nowFunc, pressureFunc, recordEventFunc)
			// call setter on node
			if err := setter(ctx, tc.node); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			// check expected condition
			assert.True(t, apiequality.Semantic.DeepEqual(tc.expectConditions, tc.node.Status.Conditions),
				"Diff: %s", cmp.Diff(tc.expectConditions, tc.node.Status.Conditions))
			// check expected events
			require.Equal(t, len(tc.expectEvents), len(events))
			for i := range tc.expectEvents {
				assert.Equal(t, tc.expectEvents[i], events[i])
			}
		})
	}
}

func TestDiskPressureCondition(t *testing.T) {
	now := time.Now()
	before := now.Add(-time.Second)
	nowFunc := func() time.Time { return now }

	cases := []struct {
		desc             string
		node             *v1.Node
		pressure         bool
		expectConditions []v1.NodeCondition
		expectEvents     []testEvent
	}{
		{
			desc:             "new, no pressure",
			node:             &v1.Node{},
			pressure:         false,
			expectConditions: []v1.NodeCondition{*makeDiskPressureCondition(false, now, now)},
			expectEvents: []testEvent{
				{
					eventType: v1.EventTypeNormal,
					event:     "NodeHasNoDiskPressure",
				},
			},
		},
		{
			desc:             "new, pressure",
			node:             &v1.Node{},
			pressure:         true,
			expectConditions: []v1.NodeCondition{*makeDiskPressureCondition(true, now, now)},
			expectEvents: []testEvent{
				{
					eventType: v1.EventTypeNormal,
					event:     "NodeHasDiskPressure",
				},
			},
		},
		{
			desc: "transition to pressure",
			node: &v1.Node{
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{*makeDiskPressureCondition(false, before, before)},
				},
			},
			pressure:         true,
			expectConditions: []v1.NodeCondition{*makeDiskPressureCondition(true, now, now)},
			expectEvents: []testEvent{
				{
					eventType: v1.EventTypeNormal,
					event:     "NodeHasDiskPressure",
				},
			},
		},
		{
			desc: "transition to no pressure",
			node: &v1.Node{
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{*makeDiskPressureCondition(true, before, before)},
				},
			},
			pressure:         false,
			expectConditions: []v1.NodeCondition{*makeDiskPressureCondition(false, now, now)},
			expectEvents: []testEvent{
				{
					eventType: v1.EventTypeNormal,
					event:     "NodeHasNoDiskPressure",
				},
			},
		},
		{
			desc: "pressure, no transition",
			node: &v1.Node{
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{*makeDiskPressureCondition(true, before, before)},
				},
			},
			pressure:         true,
			expectConditions: []v1.NodeCondition{*makeDiskPressureCondition(true, before, now)},
			expectEvents:     []testEvent{},
		},
		{
			desc: "no pressure, no transition",
			node: &v1.Node{
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{*makeDiskPressureCondition(false, before, before)},
				},
			},
			pressure:         false,
			expectConditions: []v1.NodeCondition{*makeDiskPressureCondition(false, before, now)},
			expectEvents:     []testEvent{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			ctx := context.Background()
			events := []testEvent{}
			recordEventFunc := func(eventType, event string) {
				events = append(events, testEvent{
					eventType: eventType,
					event:     event,
				})
			}
			pressureFunc := func() bool {
				return tc.pressure
			}
			// construct setter
			setter := DiskPressureCondition(nowFunc, pressureFunc, recordEventFunc)
			// call setter on node
			if err := setter(ctx, tc.node); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			// check expected condition
			assert.True(t, apiequality.Semantic.DeepEqual(tc.expectConditions, tc.node.Status.Conditions),
				"Diff: %s", cmp.Diff(tc.expectConditions, tc.node.Status.Conditions))
			// check expected events
			require.Equal(t, len(tc.expectEvents), len(events))
			for i := range tc.expectEvents {
				assert.Equal(t, tc.expectEvents[i], events[i])
			}
		})
	}
}

func TestVolumesInUse(t *testing.T) {
	withVolumesInUse := &v1.Node{
		Status: v1.NodeStatus{
			VolumesInUse: []v1.UniqueVolumeName{"foo"},
		},
	}

	cases := []struct {
		desc               string
		node               *v1.Node
		synced             bool
		volumesInUse       []v1.UniqueVolumeName
		expectVolumesInUse []v1.UniqueVolumeName
	}{
		{
			desc:               "synced",
			node:               withVolumesInUse.DeepCopy(),
			synced:             true,
			volumesInUse:       []v1.UniqueVolumeName{"bar"},
			expectVolumesInUse: []v1.UniqueVolumeName{"bar"},
		},
		{
			desc:               "not synced",
			node:               withVolumesInUse.DeepCopy(),
			synced:             false,
			volumesInUse:       []v1.UniqueVolumeName{"bar"},
			expectVolumesInUse: []v1.UniqueVolumeName{"foo"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			ctx := context.Background()
			syncedFunc := func() bool {
				return tc.synced
			}
			volumesInUseFunc := func() []v1.UniqueVolumeName {
				return tc.volumesInUse
			}
			// construct setter
			setter := VolumesInUse(syncedFunc, volumesInUseFunc)
			// call setter on node
			if err := setter(ctx, tc.node); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			// check expected volumes
			assert.True(t, apiequality.Semantic.DeepEqual(tc.expectVolumesInUse, tc.node.Status.VolumesInUse),
				"Diff: %s", cmp.Diff(tc.expectVolumesInUse, tc.node.Status.VolumesInUse))
		})
	}
}

func TestVolumeLimits(t *testing.T) {
	const (
		volumeLimitKey = "attachable-volumes-fake-provider"
		volumeLimitVal = 16
	)

	var cases = []struct {
		desc             string
		volumePluginList []volume.VolumePluginWithAttachLimits
		expectNode       *v1.Node
	}{
		{
			desc: "translate limits to capacity and allocatable for plugins that return successfully from GetVolumeLimits",
			volumePluginList: []volume.VolumePluginWithAttachLimits{
				&volumetest.FakeVolumePlugin{
					VolumeLimits: map[string]int64{volumeLimitKey: volumeLimitVal},
				},
			},
			expectNode: &v1.Node{
				Status: v1.NodeStatus{
					Capacity: v1.ResourceList{
						volumeLimitKey: *resource.NewQuantity(volumeLimitVal, resource.DecimalSI),
					},
					Allocatable: v1.ResourceList{
						volumeLimitKey: *resource.NewQuantity(volumeLimitVal, resource.DecimalSI),
					},
				},
			},
		},
		{
			desc: "skip plugins that return errors from GetVolumeLimits",
			volumePluginList: []volume.VolumePluginWithAttachLimits{
				&volumetest.FakeVolumePlugin{
					VolumeLimitsError: fmt.Errorf("foo"),
				},
			},
			expectNode: &v1.Node{},
		},
		{
			desc:       "no plugins",
			expectNode: &v1.Node{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			ctx := context.Background()
			volumePluginListFunc := func() []volume.VolumePluginWithAttachLimits {
				return tc.volumePluginList
			}
			// construct setter
			setter := VolumeLimits(volumePluginListFunc)
			// call setter on node
			node := &v1.Node{}
			if err := setter(ctx, node); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			// check expected node
			assert.True(t, apiequality.Semantic.DeepEqual(tc.expectNode, node),
				"Diff: %s", cmp.Diff(tc.expectNode, node))
		})
	}
}

func TestDaemonEndpoints(t *testing.T) {
	for _, test := range []struct {
		name      string
		endpoints *v1.NodeDaemonEndpoints
		expected  *v1.NodeDaemonEndpoints
	}{
		{
			name:      "empty daemon endpoints",
			endpoints: &v1.NodeDaemonEndpoints{},
			expected:  &v1.NodeDaemonEndpoints{KubeletEndpoint: v1.DaemonEndpoint{Port: 0}},
		},
		{
			name:      "daemon endpoints with specific port",
			endpoints: &v1.NodeDaemonEndpoints{KubeletEndpoint: v1.DaemonEndpoint{Port: 5678}},
			expected:  &v1.NodeDaemonEndpoints{KubeletEndpoint: v1.DaemonEndpoint{Port: 5678}},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			existingNode := &v1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: testKubeletHostname,
				},
				Spec: v1.NodeSpec{},
				Status: v1.NodeStatus{
					Addresses: []v1.NodeAddress{},
				},
			}

			setter := DaemonEndpoints(test.endpoints)
			if err := setter(ctx, existingNode); err != nil {
				t.Fatal(err)
			}

			assert.Equal(t, *test.expected, existingNode.Status.DaemonEndpoints)
		})
	}
}

// Test Helpers:

// testEvent is used to record events for tests
type testEvent struct {
	eventType string
	event     string
	message   string
}

// makeImageList randomly generates a list of images with the given count
func makeImageList(numImages, numTags, minSize, maxSize int32) []kubecontainer.Image {
	images := make([]kubecontainer.Image, numImages)
	for i := range images {
		image := &images[i]
		image.ID = string(uuid.NewUUID())
		image.RepoTags = makeImageTags(numTags)
		image.Size = rand.Int63nRange(int64(minSize), int64(maxSize+1))
	}
	return images
}

func makeExpectedImageList(imageList []kubecontainer.Image, maxImages, maxNames int32) []v1.ContainerImage {
	// copy the imageList, we do not want to mutate it in-place and accidentally edit a test case
	images := make([]kubecontainer.Image, len(imageList))
	copy(images, imageList)
	// sort images by size
	sort.Sort(sliceutils.ByImageSize(images))
	// convert to []v1.ContainerImage and truncate the list of names
	expectedImages := make([]v1.ContainerImage, len(images))
	for i := range images {
		image := &images[i]
		expectedImage := &expectedImages[i]
		names := append(image.RepoDigests, image.RepoTags...)
		if len(names) > int(maxNames) {
			names = names[0:maxNames]
		}
		expectedImage.Names = names
		expectedImage.SizeBytes = image.Size
	}
	// -1 means no limit, truncate result list if necessary.
	if maxImages > -1 &&
		int(maxImages) < len(expectedImages) {
		return expectedImages[0:maxImages]
	}
	return expectedImages
}

func makeImageTags(num int32) []string {
	tags := make([]string, num)
	for i := range tags {
		tags[i] = "registry.k8s.io:v" + strconv.Itoa(i)
	}
	return tags
}

func makeReadyCondition(ready bool, message string, transition, heartbeat time.Time) *v1.NodeCondition {
	if ready {
		return &v1.NodeCondition{
			Type:               v1.NodeReady,
			Status:             v1.ConditionTrue,
			Reason:             "KubeletReady",
			Message:            message,
			LastTransitionTime: metav1.NewTime(transition),
			LastHeartbeatTime:  metav1.NewTime(heartbeat),
		}
	}
	return &v1.NodeCondition{
		Type:               v1.NodeReady,
		Status:             v1.ConditionFalse,
		Reason:             "KubeletNotReady",
		Message:            message,
		LastTransitionTime: metav1.NewTime(transition),
		LastHeartbeatTime:  metav1.NewTime(heartbeat),
	}
}

func makeMemoryPressureCondition(pressure bool, transition, heartbeat time.Time) *v1.NodeCondition {
	if pressure {
		return &v1.NodeCondition{
			Type:               v1.NodeMemoryPressure,
			Status:             v1.ConditionTrue,
			Reason:             "KubeletHasInsufficientMemory",
			Message:            "kubelet has insufficient memory available",
			LastTransitionTime: metav1.NewTime(transition),
			LastHeartbeatTime:  metav1.NewTime(heartbeat),
		}
	}
	return &v1.NodeCondition{
		Type:               v1.NodeMemoryPressure,
		Status:             v1.ConditionFalse,
		Reason:             "KubeletHasSufficientMemory",
		Message:            "kubelet has sufficient memory available",
		LastTransitionTime: metav1.NewTime(transition),
		LastHeartbeatTime:  metav1.NewTime(heartbeat),
	}
}

func makePIDPressureCondition(pressure bool, transition, heartbeat time.Time) *v1.NodeCondition {
	if pressure {
		return &v1.NodeCondition{
			Type:               v1.NodePIDPressure,
			Status:             v1.ConditionTrue,
			Reason:             "KubeletHasInsufficientPID",
			Message:            "kubelet has insufficient PID available",
			LastTransitionTime: metav1.NewTime(transition),
			LastHeartbeatTime:  metav1.NewTime(heartbeat),
		}
	}
	return &v1.NodeCondition{
		Type:               v1.NodePIDPressure,
		Status:             v1.ConditionFalse,
		Reason:             "KubeletHasSufficientPID",
		Message:            "kubelet has sufficient PID available",
		LastTransitionTime: metav1.NewTime(transition),
		LastHeartbeatTime:  metav1.NewTime(heartbeat),
	}
}

func makeDiskPressureCondition(pressure bool, transition, heartbeat time.Time) *v1.NodeCondition {
	if pressure {
		return &v1.NodeCondition{
			Type:               v1.NodeDiskPressure,
			Status:             v1.ConditionTrue,
			Reason:             "KubeletHasDiskPressure",
			Message:            "kubelet has disk pressure",
			LastTransitionTime: metav1.NewTime(transition),
			LastHeartbeatTime:  metav1.NewTime(heartbeat),
		}
	}
	return &v1.NodeCondition{
		Type:               v1.NodeDiskPressure,
		Status:             v1.ConditionFalse,
		Reason:             "KubeletHasNoDiskPressure",
		Message:            "kubelet has no disk pressure",
		LastTransitionTime: metav1.NewTime(transition),
		LastHeartbeatTime:  metav1.NewTime(heartbeat),
	}
}
