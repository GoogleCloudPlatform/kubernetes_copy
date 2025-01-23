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

package app

import (
	"fmt"
	"os"
	"path"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/require"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	componentbaseconfig "k8s.io/component-base/config"
	logsapi "k8s.io/component-base/logs/api/v1"
	kubeproxyconfig "k8s.io/kubernetes/pkg/proxy/apis/config"
	"k8s.io/utils/ptr"
)

// TestLoadConfig tests proper operation of loadConfig()
func TestLoadConfig(t *testing.T) {

	yamlTemplate := `apiVersion: kubeproxy.config.k8s.io/v1alpha1
bindAddress: %s
clientConnection:
  acceptContentTypes: "abc"
  burst: 100
  contentType: content-type
  kubeconfig: "/path/to/kubeconfig"
  qps: 7
clusterCIDR: "%s"
configSyncPeriod: 15s
conntrack:
  maxPerCore: 2
  min: 1
  tcpCloseWaitTimeout: 10s
  tcpEstablishedTimeout: 20s
healthzBindAddress: "%s"
hostnameOverride: "foo"
iptables:
  masqueradeAll: true
  masqueradeBit: 17
  minSyncPeriod: 10s
  syncPeriod: 60s
  localhostNodePorts: true
ipvs:
  minSyncPeriod: 10s
  syncPeriod: 60s
  excludeCIDRs:
    - "10.20.30.40/16"
    - "fd00:1::0/64"
nftables:
  masqueradeAll: true
  masqueradeBit: 18
  minSyncPeriod: 10s
  syncPeriod: 60s
kind: KubeProxyConfiguration
metricsBindAddress: "%s"
mode: "%s"
oomScoreAdj: 17
portRange: "2-7"
detectLocalMode: "ClusterCIDR"
detectLocal:
  bridgeInterface: "cbr0"
  interfaceNamePrefix: "veth"
nodePortAddresses:
  - "10.20.30.40/16"
  - "fd00:1::0/64"
`

	testCases := []struct {
		name               string
		mode               string
		bindAddress        string
		clusterCIDR        string
		healthzBindAddress string
		metricsBindAddress string
		extraConfig        string
	}{
		{
			name:               "iptables mode, IPv4 all-zeros bind address",
			mode:               "iptables",
			bindAddress:        "0.0.0.0",
			clusterCIDR:        "1.2.3.0/24",
			healthzBindAddress: "1.2.3.4:12345",
			metricsBindAddress: "2.3.4.5:23456",
		},
		{
			name:               "iptables mode, non-zeros IPv4 config",
			mode:               "iptables",
			bindAddress:        "9.8.7.6",
			clusterCIDR:        "1.2.3.0/24",
			healthzBindAddress: "1.2.3.4:12345",
			metricsBindAddress: "2.3.4.5:23456",
		},
		{
			// Test for 'bindAddress: "::"' (IPv6 all-zeros) in kube-proxy
			// config file. The user will need to put quotes around '::' since
			// 'bindAddress: ::' is invalid yaml syntax.
			name:               "iptables mode, IPv6 \"::\" bind address",
			mode:               "iptables",
			bindAddress:        "\"::\"",
			clusterCIDR:        "fd00:1::0/64",
			healthzBindAddress: "[fd00:1::5]:12345",
			metricsBindAddress: "[fd00:2::5]:23456",
		},
		{
			// Test for 'bindAddress: "[::]"' (IPv6 all-zeros in brackets)
			// in kube-proxy config file. The user will need to use
			// surrounding quotes here since 'bindAddress: [::]' is invalid
			// yaml syntax.
			name:               "iptables mode, IPv6 \"[::]\" bind address",
			mode:               "iptables",
			bindAddress:        "\"[::]\"",
			clusterCIDR:        "fd00:1::0/64",
			healthzBindAddress: "[fd00:1::5]:12345",
			metricsBindAddress: "[fd00:2::5]:23456",
		},
		{
			// Test for 'bindAddress: ::0' (another form of IPv6 all-zeros).
			// No surrounding quotes are required around '::0'.
			name:               "iptables mode, IPv6 ::0 bind address",
			mode:               "iptables",
			bindAddress:        "::0",
			clusterCIDR:        "fd00:1::0/64",
			healthzBindAddress: "[fd00:1::5]:12345",
			metricsBindAddress: "[fd00:2::5]:23456",
		},
		{
			name:               "ipvs mode, IPv6 config",
			mode:               "ipvs",
			bindAddress:        "2001:db8::1",
			clusterCIDR:        "fd00:1::0/64",
			healthzBindAddress: "[fd00:1::5]:12345",
			metricsBindAddress: "[fd00:2::5]:23456",
		},
		{
			// Test for unknown field within config.
			// For v1alpha1 a lenient path is implemented and will throw a
			// strict decoding warning instead of failing to load
			name:               "unknown field",
			mode:               "iptables",
			bindAddress:        "9.8.7.6",
			clusterCIDR:        "1.2.3.0/24",
			healthzBindAddress: "1.2.3.4:12345",
			metricsBindAddress: "2.3.4.5:23456",
			extraConfig:        "foo: bar",
		},
		{
			// Test for duplicate field within config.
			// For v1alpha1 a lenient path is implemented and will throw a
			// strict decoding warning instead of failing to load
			name:               "duplicate field",
			mode:               "iptables",
			bindAddress:        "9.8.7.6",
			clusterCIDR:        "1.2.3.0/24",
			healthzBindAddress: "1.2.3.4:12345",
			metricsBindAddress: "2.3.4.5:23456",
			extraConfig:        "bindAddress: 9.8.7.6",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			expBindAddr := tc.bindAddress
			if tc.bindAddress[0] == '"' {
				// Surrounding double quotes will get stripped by the yaml parser.
				expBindAddr = expBindAddr[1 : len(tc.bindAddress)-1]
			}
			expected := &kubeproxyconfig.KubeProxyConfiguration{
				NodeIPOverride: []string{expBindAddr},
				IPFamilyPolicy: kubeproxyconfig.IPFamilyPolicyPreferDualStack,
				ClientConnection: componentbaseconfig.ClientConnectionConfiguration{
					AcceptContentTypes: "abc",
					Burst:              100,
					ContentType:        "content-type",
					Kubeconfig:         "/path/to/kubeconfig",
					QPS:                7,
				},
				MinSyncPeriod:    metav1.Duration{Duration: 10 * time.Second},
				SyncPeriod:       metav1.Duration{Duration: 60 * time.Second},
				ConfigSyncPeriod: metav1.Duration{Duration: 15 * time.Second},
				Linux: kubeproxyconfig.KubeProxyLinuxConfiguration{
					Conntrack: kubeproxyconfig.KubeProxyConntrackConfiguration{
						MaxPerCore:            ptr.To[int32](2),
						Min:                   ptr.To[int32](1),
						TCPCloseWaitTimeout:   &metav1.Duration{Duration: 10 * time.Second},
						TCPEstablishedTimeout: &metav1.Duration{Duration: 20 * time.Second},
					},
					MasqueradeAll: true,
					OOMScoreAdj:   ptr.To[int32](17),
				},
				FeatureGates:       map[string]bool{},
				HealthzBindAddress: tc.healthzBindAddress,
				HostnameOverride:   "foo",
				IPTables: kubeproxyconfig.KubeProxyIPTablesConfiguration{
					MasqueradeBit:      ptr.To[int32](17),
					LocalhostNodePorts: ptr.To(true),
				},
				IPVS: kubeproxyconfig.KubeProxyIPVSConfiguration{
					ExcludeCIDRs: []string{"10.20.30.40/16", "fd00:1::0/64"},
				},
				NFTables: kubeproxyconfig.KubeProxyNFTablesConfiguration{
					MasqueradeBit: ptr.To[int32](18),
				},
				MetricsBindAddress: tc.metricsBindAddress,
				Mode:               kubeproxyconfig.ProxyMode(tc.mode),
				NodePortAddresses:  []string{"10.20.30.40/16", "fd00:1::0/64"},
				DetectLocalMode:    kubeproxyconfig.LocalModeClusterCIDR,
				DetectLocal: kubeproxyconfig.DetectLocalConfiguration{
					BridgeInterface:     "cbr0",
					ClusterCIDRs:        strings.Split(tc.clusterCIDR, ","),
					InterfaceNamePrefix: "veth",
				},
				Logging: logsapi.LoggingConfiguration{
					Format:         "text",
					FlushFrequency: logsapi.TimeOrMetaDuration{Duration: metav1.Duration{Duration: 5 * time.Second}, SerializeAsString: true},
				},
			}

			options := NewOptions()

			baseYAML := fmt.Sprintf(
				yamlTemplate, tc.bindAddress, tc.clusterCIDR,
				tc.healthzBindAddress, tc.metricsBindAddress, tc.mode)

			// Append additional configuration to the base yaml template
			yaml := fmt.Sprintf("%s\n%s", baseYAML, tc.extraConfig)

			config, err := options.loadConfig([]byte(yaml))

			require.NoError(t, err, "unexpected error for %s: %v", tc.name, err)

			if diff := cmp.Diff(config, expected); diff != "" {
				t.Fatalf("unexpected config, diff = %s", diff)
			}
		})
	}
}

// TestLoadConfigFailures tests failure modes for loadConfig()
func TestLoadConfigFailures(t *testing.T) {
	// TODO(phenixblue): Uncomment below template when v1alpha2+ of kube-proxy config is
	// released with strict decoding. These associated tests will fail with
	// the lenient codec and only one config API version.
	/*
			yamlTemplate := `bindAddress: 0.0.0.0
		clusterCIDR: "1.2.3.0/24"
		configSyncPeriod: 15s
		kind: KubeProxyConfiguration`
	*/

	testCases := []struct {
		name    string
		config  string
		expErr  string
		checkFn func(err error) bool
	}{
		{
			name:   "Decode error test",
			config: "Twas bryllyg, and ye slythy toves",
			expErr: "could not find expected ':'",
		},
		{
			name:   "Bad config type test",
			config: "kind: KubeSchedulerConfiguration",
			expErr: "no kind",
		},
		{
			name:   "Missing quotes around :: bindAddress",
			config: "bindAddress: ::",
			expErr: "mapping values are not allowed in this context",
		},
		// TODO(phenixblue): Uncomment below tests when v1alpha2+ of kube-proxy config is
		// released with strict decoding. These tests will fail with the
		// lenient codec and only one config API version.
		/*
			{
				name:    "Duplicate fields",
				config:  fmt.Sprintf("%s\nbindAddress: 1.2.3.4", yamlTemplate),
				checkFn: kuberuntime.IsStrictDecodingError,
			},
			{
				name:    "Unknown field",
				config:  fmt.Sprintf("%s\nfoo: bar", yamlTemplate),
				checkFn: kuberuntime.IsStrictDecodingError,
			},
		*/
	}

	version := "apiVersion: kubeproxy.config.k8s.io/v1alpha1"
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			options := NewOptions()
			config := fmt.Sprintf("%s\n%s", version, tc.config)
			_, err := options.loadConfig([]byte(config))

			require.Error(t, err, tc.name)
			require.ErrorContains(t, err, tc.expErr)

			if tc.checkFn != nil {
				require.True(t, tc.checkFn(err), tc.name)
			}
		})
	}
}

// TestProcessHostnameOverrideFlag tests processing hostname-override arg
func TestProcessHostnameOverrideFlag(t *testing.T) {
	testCases := []struct {
		name                 string
		hostnameOverrideFlag string
		expectedHostname     string
		expectError          bool
	}{
		{
			name:                 "Hostname from config file",
			hostnameOverrideFlag: "",
			expectedHostname:     "foo",
			expectError:          false,
		},
		{
			name:                 "Hostname from flag",
			hostnameOverrideFlag: "  bar ",
			expectedHostname:     "bar",
			expectError:          false,
		},
		{
			name:                 "Hostname is space",
			hostnameOverrideFlag: "   ",
			expectError:          true,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			options := NewOptions()
			options.config = &kubeproxyconfig.KubeProxyConfiguration{
				HostnameOverride: "foo",
			}

			options.hostnameOverride = tc.hostnameOverrideFlag

			err := options.processHostnameOverrideFlag()
			if tc.expectError {
				if err == nil {
					t.Fatalf("should error for this case %s", tc.name)
				}
			} else {
				require.NoError(t, err, "unexpected error %v", err)
				if tc.expectedHostname != options.config.HostnameOverride {
					t.Fatalf("expected hostname: %s, but got: %s", tc.expectedHostname, options.config.HostnameOverride)
				}
			}
		})
	}
}

// TestProcessV1Alpha1Flags tests processing v1alpha1 flags.
func TestProcessV1Alpha1Flags(t *testing.T) {
	testCases := []struct {
		name     string
		flags    []string
		validate func(*kubeproxyconfig.KubeProxyConfiguration) bool
	}{
		{
			name: "iptables configuration",
			flags: []string{
				"--iptables-sync-period=36s",
				"--iptables-min-sync-period=3s",
				"--proxy-mode=iptables",
			},
			validate: func(config *kubeproxyconfig.KubeProxyConfiguration) bool {
				return config.SyncPeriod == metav1.Duration{Duration: 36 * time.Second} &&
					config.MinSyncPeriod == metav1.Duration{Duration: 3 * time.Second}
			},
		},
		{
			name: "iptables + ipvs configuration with iptables mode",
			flags: []string{
				"--iptables-sync-period=36s",
				"--iptables-min-sync-period=3s",
				"--ipvs-sync-period=16s",
				"--ipvs-min-sync-period=7s",
				"--proxy-mode=iptables",
			},
			validate: func(config *kubeproxyconfig.KubeProxyConfiguration) bool {
				return config.SyncPeriod == metav1.Duration{Duration: 36 * time.Second} &&
					config.MinSyncPeriod == metav1.Duration{Duration: 3 * time.Second}
			},
		},
		{
			name: "winkernel configuration",
			flags: []string{
				"--iptables-sync-period=36s",
				"--iptables-min-sync-period=3s",
				"--proxy-mode=kernelspace",
			},
			validate: func(config *kubeproxyconfig.KubeProxyConfiguration) bool {
				return config.SyncPeriod == metav1.Duration{Duration: 36 * time.Second} &&
					config.MinSyncPeriod == metav1.Duration{Duration: 3 * time.Second}
			},
		},
		{
			name: "ipvs + iptables configuration with ipvs mode",
			flags: []string{
				"--iptables-sync-period=36s",
				"--iptables-min-sync-period=3s",
				"--ipvs-sync-period=16s",
				"--ipvs-min-sync-period=7s",
				"--proxy-mode=ipvs",
			},
			validate: func(config *kubeproxyconfig.KubeProxyConfiguration) bool {
				return config.SyncPeriod == metav1.Duration{Duration: 16 * time.Second} &&
					config.MinSyncPeriod == metav1.Duration{Duration: 7 * time.Second}
			},
		},
		{
			name: "ipvs configuration",
			flags: []string{
				"--ipvs-sync-period=16s",
				"--ipvs-min-sync-period=7s",
				"--proxy-mode=ipvs",
			},
			validate: func(config *kubeproxyconfig.KubeProxyConfiguration) bool {
				return config.SyncPeriod == metav1.Duration{Duration: 16 * time.Second} &&
					config.MinSyncPeriod == metav1.Duration{Duration: 7 * time.Second}
			},
		},
		{
			name: "cluster cidr",
			flags: []string{
				"--cluster-cidr=2002:0:0:1234::/64,10.0.0.0/14",
			},
			validate: func(config *kubeproxyconfig.KubeProxyConfiguration) bool {
				return reflect.DeepEqual(config.DetectLocal.ClusterCIDRs, []string{"2002:0:0:1234::/64", "10.0.0.0/14"})
			},
		},
		{
			name: "bind address",
			flags: []string{
				"--bind-address=0.0.0.0",
			},
			validate: func(config *kubeproxyconfig.KubeProxyConfiguration) bool {
				return reflect.DeepEqual(config.NodeIPOverride, []string{"0.0.0.0"})
			},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			options := NewOptions()
			fs := new(pflag.FlagSet)
			options.AddFlags(fs)
			require.NoError(t, fs.Parse(tc.flags))
			options.processV1Alpha1Flags(fs)
			require.True(t, tc.validate(options.config))
		})
	}
}

// TestOptionsComplete checks that command line flags are combined with a
// config properly.
func TestOptionsComplete(t *testing.T) {
	header := `apiVersion: kubeproxy.config.k8s.io/v1alpha1
kind: KubeProxyConfiguration
`

	// Determine default config (depends on platform defaults).
	o := NewOptions()
	require.NoError(t, o.Complete(new(pflag.FlagSet)))
	expected := o.config

	config := header + `logging:
  format: json
  flushFrequency: 1s
  verbosity: 10
  vmodule:
  - filePattern: foo.go
    verbosity: 6
  - filePattern: bar.go
    verbosity: 8
`
	expectedLoggingConfig := logsapi.LoggingConfiguration{
		Format:         "json",
		FlushFrequency: logsapi.TimeOrMetaDuration{Duration: metav1.Duration{Duration: time.Second}, SerializeAsString: true},
		Verbosity:      10,
		VModule: []logsapi.VModuleItem{
			{
				FilePattern: "foo.go",
				Verbosity:   6,
			},
			{
				FilePattern: "bar.go",
				Verbosity:   8,
			},
		},
		Options: logsapi.FormatOptions{
			JSON: logsapi.JSONOptions{
				OutputRoutingOptions: logsapi.OutputRoutingOptions{
					InfoBufferSize: resource.QuantityValue{Quantity: resource.MustParse("0")},
				},
			},
			Text: logsapi.TextOptions{
				OutputRoutingOptions: logsapi.OutputRoutingOptions{
					InfoBufferSize: resource.QuantityValue{Quantity: resource.MustParse("0")},
				},
			},
		},
	}

	for name, tc := range map[string]struct {
		config   string
		flags    []string
		expected *kubeproxyconfig.KubeProxyConfiguration
	}{
		"empty": {
			expected: expected,
		},
		"conntrack": {
			flags: []string{
				"--conntrack-max-per-core=0",
				"--conntrack-min=0",
				"--conntrack-tcp-timeout-established=0",
				"--conntrack-tcp-timeout-close-wait=0",
			},
			expected: func() *kubeproxyconfig.KubeProxyConfiguration {
				c := expected.DeepCopy()
				c.Linux.Conntrack.MaxPerCore = ptr.To(int32(0))
				c.Linux.Conntrack.Min = ptr.To(int32(0))
				c.Linux.Conntrack.TCPEstablishedTimeout = ptr.To(metav1.Duration{})
				c.Linux.Conntrack.TCPCloseWaitTimeout = ptr.To(metav1.Duration{})
				return c
			}(),
		},
		"empty-config": {
			config:   header,
			expected: expected,
		},
		"logging-config": {
			config: config,
			expected: func() *kubeproxyconfig.KubeProxyConfiguration {
				c := expected.DeepCopy()
				c.Logging = *expectedLoggingConfig.DeepCopy()
				return c
			}(),
		},
		"flags": {
			flags: []string{
				"-v=7",
				"--vmodule", "goo.go=8",
			},
			expected: func() *kubeproxyconfig.KubeProxyConfiguration {
				c := expected.DeepCopy()
				c.Logging.Verbosity = 7
				c.Logging.VModule = append(c.Logging.VModule, logsapi.VModuleItem{
					FilePattern: "goo.go",
					Verbosity:   8,
				})
				return c
			}(),
		},
		"both": {
			config: config,
			flags: []string{
				"-v=7",
				"--vmodule", "goo.go=8",
				"--ipvs-scheduler", "some-scheduler", // Overwritten by config.
			},
			expected: func() *kubeproxyconfig.KubeProxyConfiguration {
				c := expected.DeepCopy()
				c.Logging = *expectedLoggingConfig.DeepCopy()
				// Flag wins.
				c.Logging.Verbosity = 7
				// Flag and config get merged with command line flags first.
				c.Logging.VModule = append([]logsapi.VModuleItem{
					{
						FilePattern: "goo.go",
						Verbosity:   8,
					},
				}, c.Logging.VModule...)
				return c
			}(),
		},
	} {
		t.Run(name, func(t *testing.T) {
			options := NewOptions()
			fs := new(pflag.FlagSet)
			options.AddFlags(fs)
			flags := tc.flags
			if len(tc.config) > 0 {
				tmp := t.TempDir()
				configFile := path.Join(tmp, "kube-proxy.conf")
				require.NoError(t, os.WriteFile(configFile, []byte(tc.config), 0666))
				flags = append(flags, "--config", configFile)
			}
			require.NoError(t, fs.Parse(flags))
			require.NoError(t, options.Complete(fs))
			require.Equal(t, tc.expected, options.config)
		})
	}
}

func TestAddressFromDeprecatedFlags(t *testing.T) {
	testCases := []struct {
		name               string
		healthzPort        int32
		healthzBindAddress string
		metricsPort        int32
		metricsBindAddress string
		expHealthz         string
		expMetrics         string
	}{
		{
			name:               "IPv4 bind address",
			healthzBindAddress: "1.2.3.4",
			healthzPort:        12345,
			metricsBindAddress: "2.3.4.5",
			metricsPort:        23456,
			expHealthz:         "1.2.3.4:12345",
			expMetrics:         "2.3.4.5:23456",
		},
		{
			name:               "IPv4 bind address has port",
			healthzBindAddress: "1.2.3.4:12345",
			healthzPort:        23456,
			metricsBindAddress: "2.3.4.5:12345",
			metricsPort:        23456,
			expHealthz:         "1.2.3.4:12345",
			expMetrics:         "2.3.4.5:12345",
		},
		{
			name:               "IPv6 bind address",
			healthzBindAddress: "fd00:1::5",
			healthzPort:        12345,
			metricsBindAddress: "fd00:1::6",
			metricsPort:        23456,
			expHealthz:         "[fd00:1::5]:12345",
			expMetrics:         "[fd00:1::6]:23456",
		},
		{
			name:               "IPv6 bind address has port",
			healthzBindAddress: "[fd00:1::5]:12345",
			healthzPort:        56789,
			metricsBindAddress: "[fd00:1::6]:56789",
			metricsPort:        12345,
			expHealthz:         "[fd00:1::5]:12345",
			expMetrics:         "[fd00:1::6]:56789",
		},
		{
			name:               "Invalid IPv6 Config",
			healthzBindAddress: "[fd00:1::5]",
			healthzPort:        12345,
			metricsBindAddress: "[fd00:1::6]",
			metricsPort:        56789,
			expHealthz:         "[fd00:1::5]",
			expMetrics:         "[fd00:1::6]",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			gotHealthz := addressFromDeprecatedFlags(tc.healthzBindAddress, tc.healthzPort)
			gotMetrics := addressFromDeprecatedFlags(tc.metricsBindAddress, tc.metricsPort)

			require.Equal(t, tc.expHealthz, gotHealthz)
			require.Equal(t, tc.expMetrics, gotMetrics)
		})
	}
}
