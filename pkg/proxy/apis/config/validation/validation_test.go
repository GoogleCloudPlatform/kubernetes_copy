/*
Copyright 2017 The Kubernetes Authors.

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

package validation

import (
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"
	componentbaseconfig "k8s.io/component-base/config"
	logsapi "k8s.io/component-base/logs/api/v1"
	kubeproxyconfig "k8s.io/kubernetes/pkg/proxy/apis/config"
	"k8s.io/utils/ptr"
)

func TestValidateKubeProxyConfiguration(t *testing.T) {
	baseConfig := &kubeproxyconfig.KubeProxyConfiguration{
		NodeIPOverride:     []string{"192.168.59.103"},
		HealthzBindAddress: "0.0.0.0:10256",
		MetricsBindAddress: "127.0.0.1:10249",
		DetectLocalMode:    kubeproxyconfig.LocalModeClusterCIDR,
		DetectLocal: kubeproxyconfig.DetectLocalConfiguration{
			ClusterCIDRs: []string{"192.168.59.0/24"},
		},
		SyncPeriod:       metav1.Duration{Duration: 5 * time.Second},
		MinSyncPeriod:    metav1.Duration{Duration: 2 * time.Second},
		ConfigSyncPeriod: metav1.Duration{Duration: 1 * time.Second},
		IPTables:         kubeproxyconfig.KubeProxyIPTablesConfiguration{},
		Linux: kubeproxyconfig.KubeProxyLinuxConfiguration{
			Conntrack: kubeproxyconfig.KubeProxyConntrackConfiguration{
				MaxPerCore:            ptr.To[int32](1),
				Min:                   ptr.To[int32](1),
				TCPEstablishedTimeout: &metav1.Duration{Duration: 5 * time.Second},
				TCPCloseWaitTimeout:   &metav1.Duration{Duration: 5 * time.Second},
			},
			MasqueradeAll: true,
		},
		Logging: logsapi.LoggingConfiguration{
			Format: "text",
		},
	}
	newPath := field.NewPath("KubeProxyConfiguration")

	testCases := map[string]struct {
		mutateConfigFunc func(*kubeproxyconfig.KubeProxyConfiguration)
		expectedErrs     field.ErrorList
	}{
		"basic config, unspecified Mode": {
			mutateConfigFunc: func(_ *kubeproxyconfig.KubeProxyConfiguration) {},
		},
		"Mode specified, extra mode-specific configs": {
			mutateConfigFunc: func(config *kubeproxyconfig.KubeProxyConfiguration) {
				if runtime.GOOS == "windows" {
					config.Mode = kubeproxyconfig.ProxyModeKernelspace
				} else {
					config.Mode = kubeproxyconfig.ProxyModeIPVS
				}
			},
		},
		"empty HealthzBindAddress": {
			mutateConfigFunc: func(config *kubeproxyconfig.KubeProxyConfiguration) {
				config.HealthzBindAddress = ""
			},
		},
		"empty NodeIPOverride": {
			mutateConfigFunc: func(config *kubeproxyconfig.KubeProxyConfiguration) {
				config.NodeIPOverride = []string{}
			},
		},
		"IPv6": {
			mutateConfigFunc: func(config *kubeproxyconfig.KubeProxyConfiguration) {
				config.NodeIPOverride = []string{"fd00:192:168:59::103"}
				config.HealthzBindAddress = ""
				config.MetricsBindAddress = "[::1]:10249"
				config.DetectLocal.ClusterCIDRs = []string{"fd00:192:168:59::/64"}
			},
		},
		"alternate healthz port": {
			mutateConfigFunc: func(config *kubeproxyconfig.KubeProxyConfiguration) {
				config.HealthzBindAddress = "0.0.0.0:12345"
			},
		},
		"ClusterCIDR is wrong IP family": {
			mutateConfigFunc: func(config *kubeproxyconfig.KubeProxyConfiguration) {
				config.DetectLocal.ClusterCIDRs = []string{"fd00:192:168:59::/64"}
			},
		},
		"ClusterCIDR is dual-stack": {
			mutateConfigFunc: func(config *kubeproxyconfig.KubeProxyConfiguration) {
				config.DetectLocal.ClusterCIDRs = []string{"192.168.59.0/24", "fd00:192:168::/64"}
			},
		},
		"LocalModeInterfaceNamePrefix": {
			mutateConfigFunc: func(config *kubeproxyconfig.KubeProxyConfiguration) {
				config.DetectLocalMode = kubeproxyconfig.LocalModeInterfaceNamePrefix
				config.DetectLocal = kubeproxyconfig.DetectLocalConfiguration{
					InterfaceNamePrefix: "vethabcde",
				}
			},
		},
		"LocalModeBridgeInterface": {
			mutateConfigFunc: func(config *kubeproxyconfig.KubeProxyConfiguration) {
				config.DetectLocalMode = kubeproxyconfig.LocalModeBridgeInterface
				config.DetectLocal = kubeproxyconfig.DetectLocalConfiguration{
					BridgeInterface: "avz",
				}
			},
		},
		"invalid NodeIPOverride": {
			mutateConfigFunc: func(config *kubeproxyconfig.KubeProxyConfiguration) {
				config.NodeIPOverride = []string{"10.10.12.11:2000"}
			},
			expectedErrs: field.ErrorList{field.Invalid(newPath.Child("NodeIPOverride").Index(0), "10.10.12.11:2000", "must be a valid IP (e.g. 10.100.0.0 or fde4:8dba:82e1::)")},
		},
		"invalid HealthzBindAddress": {
			mutateConfigFunc: func(config *kubeproxyconfig.KubeProxyConfiguration) {
				config.HealthzBindAddress = "0.0.0.0"
			},
			expectedErrs: field.ErrorList{field.Invalid(newPath.Child("HealthzBindAddress"), "0.0.0.0", "must be IP:port")},
		},
		"invalid MetricsBindAddress": {
			mutateConfigFunc: func(config *kubeproxyconfig.KubeProxyConfiguration) {
				config.MetricsBindAddress = "127.0.0.1"
			},
			expectedErrs: field.ErrorList{field.Invalid(newPath.Child("MetricsBindAddress"), "127.0.0.1", "must be IP:port")},
		},
		"ConfigSyncPeriod must be > 0": {
			mutateConfigFunc: func(config *kubeproxyconfig.KubeProxyConfiguration) {
				config.ConfigSyncPeriod = metav1.Duration{Duration: -1 * time.Second}
			},
			expectedErrs: field.ErrorList{field.Invalid(newPath.Child("ConfigSyncPeriod"), metav1.Duration{Duration: -1 * time.Second}, "must be greater than 0")},
		},
		"SyncPeriod must be > 0": {
			mutateConfigFunc: func(config *kubeproxyconfig.KubeProxyConfiguration) {
				config.SyncPeriod = metav1.Duration{Duration: -5 * time.Second}
			},
			expectedErrs: field.ErrorList{field.Invalid(newPath.Child("SyncPeriod"), metav1.Duration{Duration: -5 * time.Second}, "must be greater than 0"),
				field.Invalid(newPath.Child("SyncPeriod"), metav1.Duration{Duration: 2 * time.Second}, "must be greater than or equal to KubeProxyConfiguration.MinSyncPeriod")},
		},
		"MinSyncPeriod must be > 0": {
			mutateConfigFunc: func(config *kubeproxyconfig.KubeProxyConfiguration) {
				config.MinSyncPeriod = metav1.Duration{Duration: -2 * time.Second}
			},
			expectedErrs: field.ErrorList{field.Invalid(newPath.Child("MinSyncPeriod"), metav1.Duration{Duration: -2 * time.Second}, "must be greater than or equal to 0")},
		},
		"SyncPeriod must be >= MinSyncPeriod": {
			mutateConfigFunc: func(config *kubeproxyconfig.KubeProxyConfiguration) {
				config.SyncPeriod = metav1.Duration{Duration: 1 * time.Second}
				config.MinSyncPeriod = metav1.Duration{Duration: 5 * time.Second}
			},
			expectedErrs: field.ErrorList{field.Invalid(newPath.Child("SyncPeriod"), metav1.Duration{Duration: 5 * time.Second}, "must be greater than or equal to KubeProxyConfiguration.MinSyncPeriod")},
		},
		"invalid DetectLocalMode": {
			mutateConfigFunc: func(config *kubeproxyconfig.KubeProxyConfiguration) {
				config.DetectLocalMode = "Guess"
			},
			expectedErrs: field.ErrorList{field.NotSupported(newPath.Child("DetectLocalMode"), "Guess", []string{"ClusterCIDR", "NodeCIDR", "BridgeInterface", "InterfaceNamePrefix", ""})},
		},
		"invalid logging format": {
			mutateConfigFunc: func(config *kubeproxyconfig.KubeProxyConfiguration) {
				config.Logging = logsapi.LoggingConfiguration{
					Format: "unsupported format",
				}
			},
			expectedErrs: field.ErrorList{field.Invalid(newPath.Child("logging.format"), "unsupported format", "Unsupported log format")},
		},
	}

	for name, testCase := range testCases {
		t.Run(name, func(t *testing.T) {
			config := baseConfig.DeepCopy()
			testCase.mutateConfigFunc(config)
			errs := Validate(config)
			if len(testCase.expectedErrs) == 0 {
				assert.Equal(t, field.ErrorList{}, errs, "expected no validation errors")
			} else {
				assert.Equal(t, testCase.expectedErrs, errs, "did not get expected validation errors")
			}
		})
	}
}

func TestValidateKubeProxyIPTablesConfiguration(t *testing.T) {
	newPath := field.NewPath("KubeProxyConfiguration")

	for name, testCase := range map[string]struct {
		config       kubeproxyconfig.KubeProxyIPTablesConfiguration
		expectedErrs field.ErrorList
	}{
		"valid custom MasqueradeBit": {
			config: kubeproxyconfig.KubeProxyIPTablesConfiguration{
				MasqueradeBit: ptr.To[int32](5),
			},
			expectedErrs: field.ErrorList{},
		},
		"MasqueradeBit cannot be < 0": {
			config: kubeproxyconfig.KubeProxyIPTablesConfiguration{
				MasqueradeBit: ptr.To[int32](-10),
			},
			expectedErrs: field.ErrorList{field.Invalid(newPath.Child("KubeIPTablesConfiguration.MasqueradeBit"), ptr.To[int32](-10), "must be within the range [0, 31]")},
		},
	} {
		t.Run(name, func(t *testing.T) {
			errs := validateKubeProxyIPTablesConfiguration(testCase.config, newPath.Child("KubeIPTablesConfiguration"))
			assert.Equal(t, testCase.expectedErrs, errs, "did not get expected validation errors")
		})
	}
}

func TestValidateKubeProxyIPVSConfiguration(t *testing.T) {
	newPath := field.NewPath("KubeProxyConfiguration")
	for name, testCase := range map[string]struct {
		config       kubeproxyconfig.KubeProxyIPVSConfiguration
		expectedErrs field.ErrorList
	}{
		"IPVS Timeout can be 0": {
			config: kubeproxyconfig.KubeProxyIPVSConfiguration{
				TCPTimeout:    metav1.Duration{Duration: 0 * time.Second},
				TCPFinTimeout: metav1.Duration{Duration: 0 * time.Second},
				UDPTimeout:    metav1.Duration{Duration: 0 * time.Second},
			},
			expectedErrs: field.ErrorList{},
		},
		"IPVS Timeout > 0": {
			config: kubeproxyconfig.KubeProxyIPVSConfiguration{
				TCPTimeout:    metav1.Duration{Duration: 1 * time.Second},
				TCPFinTimeout: metav1.Duration{Duration: 2 * time.Second},
				UDPTimeout:    metav1.Duration{Duration: 3 * time.Second},
			},
			expectedErrs: field.ErrorList{},
		},
		"TCP,TCPFin,UDP Timeouts < 0": {
			config: kubeproxyconfig.KubeProxyIPVSConfiguration{
				TCPTimeout:    metav1.Duration{Duration: -1 * time.Second},
				UDPTimeout:    metav1.Duration{Duration: -1 * time.Second},
				TCPFinTimeout: metav1.Duration{Duration: -1 * time.Second},
			},
			expectedErrs: field.ErrorList{field.Invalid(newPath.Child("KubeIPVSConfiguration.TCPTimeout"), metav1.Duration{Duration: -1 * time.Second}, "must be greater than or equal to 0"),
				field.Invalid(newPath.Child("KubeIPVSConfiguration.TCPFinTimeout"), metav1.Duration{Duration: -1 * time.Second}, "must be greater than or equal to 0"),
				field.Invalid(newPath.Child("KubeIPVSConfiguration.UDPTimeout"), metav1.Duration{Duration: -1 * time.Second}, "must be greater than or equal to 0")},
		},
	} {
		t.Run(name, func(t *testing.T) {
			errs := validateKubeProxyIPVSConfiguration(testCase.config, newPath.Child("KubeIPVSConfiguration"))
			assert.Equal(t, testCase.expectedErrs, errs, "did not get expected validation errors")
		})
	}
}

func TestValidateKubeProxyLinuxConfiguration(t *testing.T) {
	newPath := field.NewPath("KubeProxyConfiguration")
	for name, testCase := range map[string]struct {
		config       kubeproxyconfig.KubeProxyLinuxConfiguration
		expectedErrs field.ErrorList
	}{
		"valid 5 second timeouts": {
			config: kubeproxyconfig.KubeProxyLinuxConfiguration{
				Conntrack: kubeproxyconfig.KubeProxyConntrackConfiguration{
					MaxPerCore:            ptr.To[int32](1),
					Min:                   ptr.To[int32](1),
					TCPEstablishedTimeout: &metav1.Duration{Duration: 5 * time.Second},
					TCPCloseWaitTimeout:   &metav1.Duration{Duration: 5 * time.Second},
					UDPTimeout:            metav1.Duration{Duration: 5 * time.Second},
					UDPStreamTimeout:      metav1.Duration{Duration: 5 * time.Second},
				},
				OOMScoreAdj: ptr.To[int32](0),
			},
			expectedErrs: field.ErrorList{},
		},
		"valid duration equal to 0 second timeout": {
			config: kubeproxyconfig.KubeProxyLinuxConfiguration{
				Conntrack: kubeproxyconfig.KubeProxyConntrackConfiguration{
					MaxPerCore:            ptr.To[int32](1),
					Min:                   ptr.To[int32](1),
					TCPEstablishedTimeout: &metav1.Duration{Duration: 0 * time.Second},
					TCPCloseWaitTimeout:   &metav1.Duration{Duration: 0 * time.Second},
					UDPTimeout:            metav1.Duration{Duration: 0 * time.Second},
					UDPStreamTimeout:      metav1.Duration{Duration: 0 * time.Second},
				},
				OOMScoreAdj: ptr.To[int32](0),
			},
			expectedErrs: field.ErrorList{},
		},
		"invalid MaxPerCore < 0": {
			config: kubeproxyconfig.KubeProxyLinuxConfiguration{
				Conntrack: kubeproxyconfig.KubeProxyConntrackConfiguration{
					MaxPerCore:            ptr.To[int32](-1),
					Min:                   ptr.To[int32](1),
					TCPEstablishedTimeout: &metav1.Duration{Duration: 5 * time.Second},
					TCPCloseWaitTimeout:   &metav1.Duration{Duration: 5 * time.Second},
					UDPTimeout:            metav1.Duration{Duration: 5 * time.Second},
					UDPStreamTimeout:      metav1.Duration{Duration: 5 * time.Second},
				},
				OOMScoreAdj: ptr.To[int32](0),
			},
			expectedErrs: field.ErrorList{field.Invalid(newPath.Child("KubeProxyLinuxConfiguration.KubeProxyConntrackConfiguration.MaxPerCore"), ptr.To[int32](-1), "must be greater than or equal to 0")},
		},
		"invalid minimum < 0": {
			config: kubeproxyconfig.KubeProxyLinuxConfiguration{
				Conntrack: kubeproxyconfig.KubeProxyConntrackConfiguration{
					MaxPerCore:            ptr.To[int32](1),
					Min:                   ptr.To[int32](-1),
					TCPEstablishedTimeout: &metav1.Duration{Duration: 5 * time.Second},
					TCPCloseWaitTimeout:   &metav1.Duration{Duration: 5 * time.Second},
					UDPTimeout:            metav1.Duration{Duration: 5 * time.Second},
					UDPStreamTimeout:      metav1.Duration{Duration: 5 * time.Second},
				},
				OOMScoreAdj: ptr.To[int32](0),
			},
			expectedErrs: field.ErrorList{field.Invalid(newPath.Child("KubeProxyLinuxConfiguration.KubeProxyConntrackConfiguration.Min"), ptr.To[int32](-1), "must be greater than or equal to 0")},
		},
		"invalid TCPEstablishedTimeout < 0": {
			config: kubeproxyconfig.KubeProxyLinuxConfiguration{
				Conntrack: kubeproxyconfig.KubeProxyConntrackConfiguration{
					MaxPerCore:            ptr.To[int32](1),
					Min:                   ptr.To[int32](1),
					TCPEstablishedTimeout: &metav1.Duration{Duration: -5 * time.Second},
					TCPCloseWaitTimeout:   &metav1.Duration{Duration: 5 * time.Second},
					UDPTimeout:            metav1.Duration{Duration: 5 * time.Second},
					UDPStreamTimeout:      metav1.Duration{Duration: 5 * time.Second},
				},
				OOMScoreAdj: ptr.To[int32](0),
			},
			expectedErrs: field.ErrorList{field.Invalid(newPath.Child("KubeProxyLinuxConfiguration.KubeProxyConntrackConfiguration.TCPEstablishedTimeout"), &metav1.Duration{Duration: -5 * time.Second}, "must be greater than or equal to 0")},
		},
		"invalid TCPCloseWaitTimeout < 0": {
			config: kubeproxyconfig.KubeProxyLinuxConfiguration{
				Conntrack: kubeproxyconfig.KubeProxyConntrackConfiguration{
					MaxPerCore:            ptr.To[int32](1),
					Min:                   ptr.To[int32](1),
					TCPEstablishedTimeout: &metav1.Duration{Duration: 5 * time.Second},
					TCPCloseWaitTimeout:   &metav1.Duration{Duration: -5 * time.Second},
					UDPTimeout:            metav1.Duration{Duration: 5 * time.Second},
					UDPStreamTimeout:      metav1.Duration{Duration: 5 * time.Second},
				},
				OOMScoreAdj: ptr.To[int32](0),
			},
			expectedErrs: field.ErrorList{field.Invalid(newPath.Child("KubeProxyLinuxConfiguration.KubeProxyConntrackConfiguration.TCPCloseWaitTimeout"), &metav1.Duration{Duration: -5 * time.Second}, "must be greater than or equal to 0")},
		},
		"invalid UDPTimeout < 0": {
			config: kubeproxyconfig.KubeProxyLinuxConfiguration{
				Conntrack: kubeproxyconfig.KubeProxyConntrackConfiguration{
					MaxPerCore:            ptr.To[int32](1),
					Min:                   ptr.To[int32](1),
					TCPEstablishedTimeout: &metav1.Duration{Duration: 5 * time.Second},
					TCPCloseWaitTimeout:   &metav1.Duration{Duration: 5 * time.Second},
					UDPTimeout:            metav1.Duration{Duration: -5 * time.Second},
					UDPStreamTimeout:      metav1.Duration{Duration: 5 * time.Second},
				},
				OOMScoreAdj: ptr.To[int32](999),
			},
			expectedErrs: field.ErrorList{field.Invalid(newPath.Child("KubeProxyLinuxConfiguration.KubeProxyConntrackConfiguration.UDPTimeout"), metav1.Duration{Duration: -5 * time.Second}, "must be greater than or equal to 0")},
		},
		"invalid UDPStreamTimeout < 0": {
			config: kubeproxyconfig.KubeProxyLinuxConfiguration{
				Conntrack: kubeproxyconfig.KubeProxyConntrackConfiguration{
					MaxPerCore:            ptr.To[int32](1),
					Min:                   ptr.To[int32](1),
					TCPEstablishedTimeout: &metav1.Duration{Duration: 5 * time.Second},
					TCPCloseWaitTimeout:   &metav1.Duration{Duration: 5 * time.Second},
					UDPTimeout:            metav1.Duration{Duration: 5 * time.Second},
					UDPStreamTimeout:      metav1.Duration{Duration: -5 * time.Second},
				},
				OOMScoreAdj: ptr.To[int32](-999),
			},
			expectedErrs: field.ErrorList{field.Invalid(newPath.Child("KubeProxyLinuxConfiguration.KubeProxyConntrackConfiguration.UDPStreamTimeout"), metav1.Duration{Duration: -5 * time.Second}, "must be greater than or equal to 0")},
		},
		"invalid OOMScoreAdj < -1000": {
			config: kubeproxyconfig.KubeProxyLinuxConfiguration{
				Conntrack: kubeproxyconfig.KubeProxyConntrackConfiguration{
					MaxPerCore:            ptr.To[int32](1),
					Min:                   ptr.To[int32](1),
					TCPEstablishedTimeout: &metav1.Duration{Duration: 5 * time.Second},
					TCPCloseWaitTimeout:   &metav1.Duration{Duration: 5 * time.Second},
					UDPTimeout:            metav1.Duration{Duration: 5 * time.Second},
					UDPStreamTimeout:      metav1.Duration{Duration: 5 * time.Second},
				},
				OOMScoreAdj: ptr.To[int32](-1001),
			},
			expectedErrs: field.ErrorList{field.Invalid(newPath.Child("KubeProxyLinuxConfiguration.OOMScoreAdj"), int32(-1001), "must be within the range [-1000, 1000]")},
		},
		"invalid OOMScoreAdj > 1000": {
			config: kubeproxyconfig.KubeProxyLinuxConfiguration{
				Conntrack: kubeproxyconfig.KubeProxyConntrackConfiguration{
					MaxPerCore:            ptr.To[int32](1),
					Min:                   ptr.To[int32](1),
					TCPEstablishedTimeout: &metav1.Duration{Duration: 5 * time.Second},
					TCPCloseWaitTimeout:   &metav1.Duration{Duration: 5 * time.Second},
					UDPTimeout:            metav1.Duration{Duration: 5 * time.Second},
					UDPStreamTimeout:      metav1.Duration{Duration: 5 * time.Second},
				},
				OOMScoreAdj: ptr.To[int32](1001),
			},
			expectedErrs: field.ErrorList{field.Invalid(newPath.Child("KubeProxyLinuxConfiguration.OOMScoreAdj"), int32(1001), "must be within the range [-1000, 1000]")},
		},
	} {
		t.Run(name, func(t *testing.T) {
			errs := validateKubeProxyLinuxConfiguration(testCase.config, newPath.Child("KubeProxyLinuxConfiguration"))
			assert.Equal(t, testCase.expectedErrs, errs, "did not get expected validation errors")
		})
	}
}

func TestValidateProxyMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		testValidateProxyModeWindows(t)
	} else {
		testValidateProxyModeLinux(t)
	}
}

func testValidateProxyModeLinux(t *testing.T) {
	newPath := field.NewPath("KubeProxyConfiguration")
	for name, testCase := range map[string]struct {
		mode         kubeproxyconfig.ProxyMode
		expectedErrs field.ErrorList
	}{
		"blank mode should default": {
			mode: kubeproxyconfig.ProxyMode(""),
		},
		"iptables is allowed": {
			mode: kubeproxyconfig.ProxyModeIPTables,
		},
		"ipvs is allowed": {
			mode: kubeproxyconfig.ProxyModeIPVS,
		},
		"nftables is allowed": {
			mode: kubeproxyconfig.ProxyModeNFTables,
		},
		"winkernel is not allowed": {
			mode:         kubeproxyconfig.ProxyModeKernelspace,
			expectedErrs: field.ErrorList{field.Invalid(newPath.Child("ProxyMode"), "kernelspace", "must be iptables, ipvs, nftables or blank (blank means the best-available proxy [currently iptables])")},
		},
		"invalid mode non-existent": {
			mode:         kubeproxyconfig.ProxyMode("non-existing"),
			expectedErrs: field.ErrorList{field.Invalid(newPath.Child("ProxyMode"), "non-existing", "must be iptables, ipvs, nftables or blank (blank means the best-available proxy [currently iptables])")},
		},
	} {
		t.Run(name, func(t *testing.T) {
			errs := validateProxyMode(testCase.mode, newPath)
			assert.Equal(t, testCase.expectedErrs, errs, "did not get expected validation errors")
		})
	}
}

func testValidateProxyModeWindows(t *testing.T) {
	// TODO: remove skip once the test has been fixed.
	if runtime.GOOS == "windows" {
		t.Skip("Skipping failing test on Windows.")
	}
	newPath := field.NewPath("KubeProxyConfiguration")
	for name, testCase := range map[string]struct {
		mode         kubeproxyconfig.ProxyMode
		expectedErrs field.ErrorList
	}{
		"blank mode should default": {
			mode: kubeproxyconfig.ProxyMode(""),
		},
		"winkernel is allowed": {
			mode: kubeproxyconfig.ProxyModeKernelspace,
		},
		"iptables is not allowed": {
			mode:         kubeproxyconfig.ProxyModeIPTables,
			expectedErrs: field.ErrorList{field.Invalid(newPath.Child("ProxyMode"), "iptables", "must be kernelspace or blank (blank means the most-available proxy [currently kernelspace])")},
		},
		"ipvs is not allowed": {
			mode:         kubeproxyconfig.ProxyModeIPVS,
			expectedErrs: field.ErrorList{field.Invalid(newPath.Child("ProxyMode"), "ipvs", "must be kernelspace or blank (blank means the most-available proxy [currently kernelspace])")},
		},
		"nftables is not allowed": {
			mode:         kubeproxyconfig.ProxyModeNFTables,
			expectedErrs: field.ErrorList{field.Invalid(newPath.Child("ProxyMode"), "nftables", "must be kernelspace or blank (blank means the most-available proxy [currently kernelspace])")},
		},
		"invalid mode non-existent": {
			mode:         kubeproxyconfig.ProxyMode("non-existing"),
			expectedErrs: field.ErrorList{field.Invalid(newPath.Child("ProxyMode"), "non-existing", "must be kernelspace or blank (blank means the most-available proxy [currently kernelspace])")},
		},
	} {
		t.Run(name, func(t *testing.T) {
			errs := validateProxyMode(testCase.mode, newPath)
			assert.Equal(t, testCase.expectedErrs, errs, "did not get expected validation errors")
		})
	}
}

func TestValidateClientConnectionConfiguration(t *testing.T) {
	newPath := field.NewPath("KubeProxyConfiguration")
	for name, testCase := range map[string]struct {
		ccc          componentbaseconfig.ClientConnectionConfiguration
		expectedErrs field.ErrorList
	}{
		"successful 0 value": {
			ccc:          componentbaseconfig.ClientConnectionConfiguration{Burst: 0},
			expectedErrs: field.ErrorList{},
		},
		"successful 5 value": {
			ccc:          componentbaseconfig.ClientConnectionConfiguration{Burst: 5},
			expectedErrs: field.ErrorList{},
		},
		"burst < 0": {
			ccc:          componentbaseconfig.ClientConnectionConfiguration{Burst: -5},
			expectedErrs: field.ErrorList{field.Invalid(newPath.Child("Burst"), int64(-5), "must be greater than or equal to 0")},
		},
	} {
		t.Run(name, func(t *testing.T) {
			errs := validateClientConnectionConfiguration(testCase.ccc, newPath)
			assert.Equal(t, testCase.expectedErrs, errs, "did not get expected validation errors")
		})
	}
}

func TestValidateHostPort(t *testing.T) {
	newPath := field.NewPath("KubeProxyConfiguration")
	for name, testCase := range map[string]struct {
		ip           string
		expectedErrs field.ErrorList
	}{
		"all IPs": {
			ip: "0.0.0.0:10256",
		},
		"localhost": {
			ip: "127.0.0.1:10256",
		},
		"specific IP": {
			ip: "10.10.10.10:10256",
		},
		"missing port": {
			ip:           "10.10.10.10",
			expectedErrs: field.ErrorList{field.Invalid(newPath.Child("HealthzBindAddress"), "10.10.10.10", "must be IP:port")},
		},
		"digits outside of 1-255": {
			ip:           "123.456.789.10:12345",
			expectedErrs: field.ErrorList{field.Invalid(newPath.Child("HealthzBindAddress"), "123.456.789.10", "must be a valid IP")},
		},
		"invalid named-port": {
			ip:           "10.10.10.10:foo",
			expectedErrs: field.ErrorList{field.Invalid(newPath.Child("HealthzBindAddress"), "foo", "must be a valid port")},
		},
		"port cannot be 0": {
			ip:           "10.10.10.10:0",
			expectedErrs: field.ErrorList{field.Invalid(newPath.Child("HealthzBindAddress"), "0", "must be a valid port")},
		},
		"port is greater than allowed range": {
			ip:           "10.10.10.10:65536",
			expectedErrs: field.ErrorList{field.Invalid(newPath.Child("HealthzBindAddress"), "65536", "must be a valid port")},
		},
	} {
		t.Run(name, func(t *testing.T) {
			errs := validateHostPort(testCase.ip, newPath.Child("HealthzBindAddress"))
			if len(testCase.expectedErrs) == 0 {
				assert.Equal(t, field.ErrorList{}, errs, "expected no validation errors")
			} else {
				assert.Equal(t, testCase.expectedErrs, errs, "did not get expected validation errors")
			}
		})
	}
}

func TestValidateKubeProxyNodePortAddress(t *testing.T) {
	newPath := field.NewPath("KubeProxyConfiguration")
	for name, testCase := range map[string]struct {
		addresses    []string
		expectedErrs field.ErrorList
	}{
		"no addresses": {
			addresses: []string{},
		},
		"valid 1": {
			addresses: []string{"127.0.0.0/8"},
		},
		"valid 2": {
			addresses: []string{"0.0.0.0/0"},
		},
		"valid 3": {
			addresses: []string{"::/0"},
		},
		"valid 4": {
			addresses: []string{"127.0.0.1/32", "1.2.3.0/24"},
		},
		"valid 5": {
			addresses: []string{"127.0.0.1/32"},
		},
		"valid 6": {
			addresses: []string{"::1/128"},
		},
		"valid 7": {
			addresses: []string{"1.2.3.4/32"},
		},
		"valid 8": {
			addresses: []string{"10.20.30.0/24"},
		},
		"valid 9": {
			addresses: []string{"10.20.0.0/16", "100.200.0.0/16"},
		},
		"valid 10": {
			addresses: []string{"10.0.0.0/8"},
		},
		"valid 11": {
			addresses: []string{"2001:db8::/32"},
		},
		"primary": {
			addresses: []string{kubeproxyconfig.NodePortAddressesPrimary},
		},
		"invalid foo address": {
			addresses:    []string{"foo"},
			expectedErrs: field.ErrorList{field.Invalid(newPath.Child("NodePortAddresses[0]"), "foo", "must be a valid CIDR")},
		},
		"invalid octet address": {
			addresses:    []string{"10.0.0.0/0", "1.2.3"},
			expectedErrs: field.ErrorList{field.Invalid(newPath.Child("NodePortAddresses[1]"), "1.2.3", "must be a valid CIDR")},
		},
		"address cannot be 0": {
			addresses:    []string{"127.0.0.1/32", "0", "1.2.3.0/24"},
			expectedErrs: field.ErrorList{field.Invalid(newPath.Child("NodePortAddresses[1]"), "0", "must be a valid CIDR")},
		},
		"address missing subnet range": {
			addresses:    []string{"127.0.0.1/32", "10.20.30.40", "1.2.3.0/24"},
			expectedErrs: field.ErrorList{field.Invalid(newPath.Child("NodePortAddresses[1]"), "10.20.30.40", "must be a valid CIDR")},
		},
		"missing ipv6 subnet ranges": {
			addresses: []string{"::0", "::1", "2001:db8::/32"},
			expectedErrs: field.ErrorList{field.Invalid(newPath.Child("NodePortAddresses[0]"), "::0", "must be a valid CIDR"),
				field.Invalid(newPath.Child("NodePortAddresses[1]"), "::1", "must be a valid CIDR")},
		},
		"invalid ipv6 ip format": {
			addresses:    []string{"::1/128", "2001:db8::/32", "2001:db8:xyz/64"},
			expectedErrs: field.ErrorList{field.Invalid(newPath.Child("NodePortAddresses[2]"), "2001:db8:xyz/64", "must be a valid CIDR")},
		},
		"invalid primary/CIDR mix 1": {
			addresses:    []string{"primary", "127.0.0.1/32"},
			expectedErrs: field.ErrorList{field.Invalid(newPath.Child("NodePortAddresses[0]"), "primary", "can't use both 'primary' and CIDRs")},
		},
		"invalid primary/CIDR mix 2": {
			addresses:    []string{"127.0.0.1/32", "primary"},
			expectedErrs: field.ErrorList{field.Invalid(newPath.Child("NodePortAddresses[1]"), "primary", "can't use both 'primary' and CIDRs")},
		},
	} {
		t.Run(name, func(t *testing.T) {
			errs := validateKubeProxyNodePortAddress(testCase.addresses, newPath.Child("NodePortAddresses"))
			if len(testCase.expectedErrs) == 0 {
				assert.Equal(t, field.ErrorList{}, errs, "expected no validation errors")
			} else {
				assert.Equal(t, testCase.expectedErrs, errs, "did not get expected validation errors")
			}
		})
	}
}

func TestValidateKubeProxyExcludeCIDRs(t *testing.T) {
	newPath := field.NewPath("KubeProxyConfiguration")
	for name, testCase := range map[string]struct {
		addresses    []string
		expectedErrs field.ErrorList
	}{
		"no cidrs": {
			addresses: []string{},
		},
		"valid 1": {
			addresses: []string{"127.0.0.0/8"},
		},
		"valid 2": {
			addresses: []string{"0.0.0.0/0"},
		},
		"valid 3": {
			addresses: []string{"::/0"},
		},
		"valid 4": {
			addresses: []string{"127.0.0.1/32", "1.2.3.0/24"},
		},
		"valid 5": {
			addresses: []string{"127.0.0.1/32"},
		},
		"valid 6": {
			addresses: []string{"::1/128"},
		},
		"valid 7": {
			addresses: []string{"1.2.3.4/32"},
		},
		"valid 8": {
			addresses: []string{"10.20.30.0/24"},
		},
		"valid 9": {
			addresses: []string{"10.20.0.0/16", "100.200.0.0/16"},
		},
		"valid 10": {
			addresses: []string{"10.0.0.0/8"},
		},
		"valid 11": {
			addresses: []string{"2001:db8::/32"},
		},
		"invalid foo address": {
			addresses:    []string{"foo"},
			expectedErrs: field.ErrorList{field.Invalid(newPath.Child("ExcludeCIDRS[0]"), "foo", "must be a valid CIDR")},
		},
		"invalid octet address": {
			addresses:    []string{"10.0.0.0/0", "1.2.3"},
			expectedErrs: field.ErrorList{field.Invalid(newPath.Child("ExcludeCIDRS[1]"), "1.2.3", "must be a valid CIDR")},
		},
		"address cannot be 0": {
			addresses:    []string{"127.0.0.1/32", "0", "1.2.3.0/24"},
			expectedErrs: field.ErrorList{field.Invalid(newPath.Child("ExcludeCIDRS[1]"), "0", "must be a valid CIDR")},
		},
		"address missing subnet range": {
			addresses:    []string{"127.0.0.1/32", "10.20.30.40", "1.2.3.0/24"},
			expectedErrs: field.ErrorList{field.Invalid(newPath.Child("ExcludeCIDRS[1]"), "10.20.30.40", "must be a valid CIDR")},
		},
		"missing ipv6 subnet ranges": {
			addresses: []string{"::0", "::1", "2001:db8::/32"},
			expectedErrs: field.ErrorList{field.Invalid(newPath.Child("ExcludeCIDRS[0]"), "::0", "must be a valid CIDR"),
				field.Invalid(newPath.Child("ExcludeCIDRS[1]"), "::1", "must be a valid CIDR")},
		},
		"invalid ipv6 ip format": {
			addresses:    []string{"::1/128", "2001:db8::/32", "2001:db8:xyz/64"},
			expectedErrs: field.ErrorList{field.Invalid(newPath.Child("ExcludeCIDRS[2]"), "2001:db8:xyz/64", "must be a valid CIDR")},
		},
	} {
		t.Run(name, func(t *testing.T) {
			errs := validateIPVSExcludeCIDRs(testCase.addresses, newPath.Child("ExcludeCIDRS"))
			if len(testCase.expectedErrs) == 0 {
				assert.Equal(t, field.ErrorList{}, errs, "expected no validation errors")
			} else {
				assert.Equal(t, testCase.expectedErrs, errs, "did not get expected validation errors")
			}
		})
	}
}

func TestValidateDetectLocalConfiguration(t *testing.T) {
	newPath := field.NewPath("KubeProxyConfiguration")

	testCases := []struct {
		name         string
		mode         kubeproxyconfig.LocalMode
		config       kubeproxyconfig.DetectLocalConfiguration
		expectedErrs field.ErrorList
	}{
		{
			name: "valid interface name prefix",
			mode: kubeproxyconfig.LocalModeInterfaceNamePrefix,
			config: kubeproxyconfig.DetectLocalConfiguration{
				InterfaceNamePrefix: "vethabcde",
			},
			expectedErrs: field.ErrorList{},
		},
		{
			name: "valid bridge interface",
			mode: kubeproxyconfig.LocalModeBridgeInterface,
			config: kubeproxyconfig.DetectLocalConfiguration{
				BridgeInterface: "avz",
			},
			expectedErrs: field.ErrorList{},
		},
		{
			name: "interfacePrefix is empty",
			mode: kubeproxyconfig.LocalModeInterfaceNamePrefix,
			config: kubeproxyconfig.DetectLocalConfiguration{
				InterfaceNamePrefix: "",
			},
			expectedErrs: field.ErrorList{field.Invalid(newPath.Child("DetectLocal").Child("InterfacePrefix"), "", "must not be empty")},
		},
		{
			name: "bridgeInterfaceName is empty",
			mode: kubeproxyconfig.LocalModeBridgeInterface,
			config: kubeproxyconfig.DetectLocalConfiguration{
				InterfaceNamePrefix: "eth0", // we won't care about prefix since mode is not prefix
			},
			expectedErrs: field.ErrorList{field.Invalid(newPath.Child("DetectLocal").Child("InterfaceName"), "", "must not be empty")},
		},
		{
			name: "valid cluster cidr",
			mode: kubeproxyconfig.LocalModeClusterCIDR,
			config: kubeproxyconfig.DetectLocalConfiguration{
				ClusterCIDRs: []string{"192.168.59.0/24", "fd00:192:168::/64"},
			},
			expectedErrs: field.ErrorList{},
		},
		{
			name: "invalid number of cluster cidrs",
			mode: kubeproxyconfig.LocalModeClusterCIDR,
			config: kubeproxyconfig.DetectLocalConfiguration{
				ClusterCIDRs: []string{"192.168.59.0/24", "fd00:192:168::/64", "10.0.0.0/16"},
			},
			expectedErrs: field.ErrorList{field.Invalid(newPath.Child("DetectLocal").Child("ClusterCIDRs"), []string{"192.168.59.0/24", "fd00:192:168::/64", "10.0.0.0/16"}, "must be a either a single CIDR or dual-stack pair of CIDRs (e.g. [10.100.0.0/16, fde4:8dba:82e1::/48]")},
		},
		{
			name: "invalid cluster cidr",
			mode: kubeproxyconfig.LocalModeClusterCIDR,
			config: kubeproxyconfig.DetectLocalConfiguration{
				ClusterCIDRs: []string{"192.168.59.0"},
			},
			expectedErrs: field.ErrorList{field.Invalid(newPath.Child("DetectLocal").Child("ClusterCIDRs").Index(0), "192.168.59.0", "must be a valid CIDR block (e.g. 10.100.0.0/16 or fde4:8dba:82e1::/48)")},
		},
		{
			name: "empty cluster cidrs with cluster cidr mode",
			mode: kubeproxyconfig.LocalModeClusterCIDR,
			config: kubeproxyconfig.DetectLocalConfiguration{
				ClusterCIDRs: []string{},
			},
			expectedErrs: field.ErrorList{},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			errs := validateDetectLocalConfiguration(tc.mode, tc.config, newPath.Child("DetectLocal"))
			assert.Equalf(t, len(tc.expectedErrs), len(errs),
				"expected %d errors, got %d errors: %v", len(tc.expectedErrs), len(errs), errs,
			)
			for i, err := range errs {
				assert.Equal(t, tc.expectedErrs[i].Error(), err.Error())
			}
		})
	}
}

func TestValidateDualStackCIDRStrings(t *testing.T) {
	newPath := field.NewPath("KubeProxyConfiguration")

	testCases := []struct {
		name         string
		cidrStrings  []string
		expectedErrs field.ErrorList
	}{
		{
			name:         "empty cidr string",
			cidrStrings:  []string{},
			expectedErrs: field.ErrorList{field.Invalid(newPath.Child("DualStackCIDRList"), []string{}, "must contain at least one CIDR")},
		},
		{
			name:         "single ipv4 cidr",
			cidrStrings:  []string{"192.168.0.0/16"},
			expectedErrs: field.ErrorList{},
		},
		{
			name:         "single ipv6 cidr",
			cidrStrings:  []string{"fd00:10:96::/112"},
			expectedErrs: field.ErrorList{},
		},
		{
			name:         "dual stack cidr pair",
			cidrStrings:  []string{"172.16.200.0/24", "fde4:8dba:82e1::/48"},
			expectedErrs: field.ErrorList{},
		},
		{
			name:         "multiple ipv4 cidrs",
			cidrStrings:  []string{"10.100.0.0/16", "192.168.0.0/16", "fde4:8dba:82e1::/48"},
			expectedErrs: field.ErrorList{field.Invalid(newPath.Child("DualStackCIDRList"), []string{"10.100.0.0/16", "192.168.0.0/16", "fde4:8dba:82e1::/48"}, "must be a either a single CIDR or dual-stack pair of CIDRs (e.g. [10.100.0.0/16, fde4:8dba:82e1::/48]")},
		},
		{
			name:         "multiple ipv6 cidrs",
			cidrStrings:  []string{"fd00:10:96::/112", "fde4:8dba:82e1::/48"},
			expectedErrs: field.ErrorList{field.Invalid(newPath.Child("DualStackCIDRList"), []string{"fd00:10:96::/112", "fde4:8dba:82e1::/48"}, "must be a either a single CIDR or dual-stack pair of CIDRs (e.g. [10.100.0.0/16, fde4:8dba:82e1::/48]")},
		},
		{
			name:         "malformed ipv4 cidr",
			cidrStrings:  []string{"fde4:8dba:82e1::/48", "172.16.200.0:24"},
			expectedErrs: field.ErrorList{field.Invalid(newPath.Child("DualStackCIDRList").Index(1), "172.16.200.0:24", "must be a valid CIDR block (e.g. 10.100.0.0/16 or fde4:8dba:82e1::/48)")},
		},
		{
			name:         "malformed ipv6 cidr",
			cidrStrings:  []string{"fd00:10:96::", "192.168.0.0/16"},
			expectedErrs: field.ErrorList{field.Invalid(newPath.Child("DualStackCIDRList").Index(0), "fd00:10:96::", "must be a valid CIDR block (e.g. 10.100.0.0/16 or fde4:8dba:82e1::/48)")},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			errs := validateDualStackCIDRStrings(tc.cidrStrings, newPath.Child("DualStackCIDRList"))
			assert.Equalf(t, len(tc.expectedErrs), len(errs),
				"expected %d errors, got %d errors: %v", len(tc.expectedErrs), len(errs), errs,
			)
			for i, err := range errs {
				assert.Equal(t, tc.expectedErrs[i].Error(), err.Error())
			}
		})
	}
}

func TestValidateDualStackIPStrings(t *testing.T) {
	newPath := field.NewPath("KubeProxyConfiguration")

	testCases := []struct {
		name         string
		ipStrings    []string
		expectedErrs field.ErrorList
	}{
		{
			name:         "empty cidr string",
			ipStrings:    []string{},
			expectedErrs: field.ErrorList{field.Invalid(newPath.Child("DualStackIPList"), []string{}, "must contain at least one IP")},
		},
		{
			name:         "single ipv4",
			ipStrings:    []string{"192.168.0.0"},
			expectedErrs: field.ErrorList{},
		},
		{
			name:         "single ipv6",
			ipStrings:    []string{"fd00:10:96::"},
			expectedErrs: field.ErrorList{},
		},
		{
			name:         "dual stack ip pair",
			ipStrings:    []string{"172.16.200.0", "fde4:8dba:82e1::"},
			expectedErrs: field.ErrorList{},
		},
		{
			name:         "multiple ipv4",
			ipStrings:    []string{"10.100.0.0", "192.168.0.0", "fde4:8dba:82e1::"},
			expectedErrs: field.ErrorList{field.Invalid(newPath.Child("DualStackIPList"), []string{"10.100.0.0", "192.168.0.0", "fde4:8dba:82e1::"}, "must be a either a single IP or dual-stack pair of IPs (e.g. [10.100.0.0, fde4:8dba:82e1::])")},
		},
		{
			name:         "multiple ipv6",
			ipStrings:    []string{"fd00:10:96::", "fde4:8dba:82e1::"},
			expectedErrs: field.ErrorList{field.Invalid(newPath.Child("DualStackIPList"), []string{"fd00:10:96::", "fde4:8dba:82e1::"}, "must be a either a single IP or dual-stack pair of IPs (e.g. [10.100.0.0, fde4:8dba:82e1::])")},
		},
		{
			name:         "ipv6 host-port",
			ipStrings:    []string{"[fd00:10:96::]:54321", "192.168.0.0"},
			expectedErrs: field.ErrorList{field.Invalid(newPath.Child("DualStackIPList").Index(0), "[fd00:10:96::]:54321", "must be a valid IP (e.g. 10.100.0.0 or fde4:8dba:82e1::)")},
		},
		{
			name:         "ipv4 host-port",
			ipStrings:    []string{"fde4:8dba:82e1::", "172.16.200.0:3306"},
			expectedErrs: field.ErrorList{field.Invalid(newPath.Child("DualStackIPList").Index(1), "172.16.200.0:3306", "must be a valid IP (e.g. 10.100.0.0 or fde4:8dba:82e1::)")},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			errs := validateDualStackIPStrings(tc.ipStrings, newPath.Child("DualStackIPList"))
			assert.Equalf(t, len(tc.expectedErrs), len(errs),
				"expected %d errors, got %d errors: %v", len(tc.expectedErrs), len(errs), errs,
			)
			for i, err := range errs {
				assert.Equal(t, tc.expectedErrs[i].Error(), err.Error())
			}
		})
	}
}
