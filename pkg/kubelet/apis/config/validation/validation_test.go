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

package validation_test

import (
	"strings"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	logsapi "k8s.io/component-base/logs/api/v1"
	tracingapi "k8s.io/component-base/tracing/api/v1"
	"k8s.io/kubelet/config/v1alpha1"
	kubeletconfig "k8s.io/kubernetes/pkg/kubelet/apis/config"
	"k8s.io/kubernetes/pkg/kubelet/apis/config/validation"
	kubetypes "k8s.io/kubernetes/pkg/kubelet/types"
	kubeletutil "k8s.io/kubernetes/pkg/kubelet/util"
	"k8s.io/utils/ptr"
)

var (
	successConfig = kubeletconfig.KubeletConfiguration{
		CgroupsPerQOS:                   cgroupsPerQOS,
		EnforceNodeAllocatable:          enforceNodeAllocatable,
		SystemReservedCgroup:            "/system.slice",
		KubeReservedCgroup:              "/kubelet.service",
		PodLogsDir:                      "/logs",
		SystemCgroups:                   "",
		CgroupRoot:                      "",
		EventBurst:                      10,
		EventRecordQPS:                  5,
		HealthzPort:                     10248,
		ImageGCHighThresholdPercent:     85,
		ImageGCLowThresholdPercent:      80,
		IPTablesDropBit:                 15,
		IPTablesMasqueradeBit:           14,
		KubeAPIBurst:                    10,
		KubeAPIQPS:                      5,
		MaxOpenFiles:                    1000000,
		MaxPods:                         110,
		OOMScoreAdj:                     -999,
		PodsPerCore:                     100,
		Port:                            65535,
		ReadOnlyPort:                    0,
		RegistryBurst:                   10,
		RegistryPullQPS:                 5,
		MaxParallelImagePulls:           nil,
		HairpinMode:                     kubeletconfig.PromiscuousBridge,
		NodeLeaseDurationSeconds:        1,
		CPUCFSQuotaPeriod:               metav1.Duration{Duration: 25 * time.Millisecond},
		TopologyManagerScope:            kubeletconfig.PodTopologyManagerScope,
		TopologyManagerPolicy:           kubeletconfig.SingleNumaNodeTopologyManagerPolicy,
		ShutdownGracePeriod:             metav1.Duration{Duration: 30 * time.Second},
		ShutdownGracePeriodCriticalPods: metav1.Duration{Duration: 10 * time.Second},
		MemoryThrottlingFactor:          ptr.To(0.9),
		FeatureGates: map[string]bool{
			"CustomCPUCFSQuotaPeriod":    true,
			"GracefulNodeShutdown":       true,
			"MemoryQoS":                  true,
			"KubeletCrashLoopBackOffMax": true,
		},
		Logging: logsapi.LoggingConfiguration{
			Format: "text",
		},
		ContainerRuntimeEndpoint:    "unix:///run/containerd/containerd.sock",
		ContainerLogMaxWorkers:      1,
		ContainerLogMaxFiles:        5,
		ContainerLogMonitorInterval: metav1.Duration{Duration: 10 * time.Second},
		SingleProcessOOMKill:        ptr.To(!kubeletutil.IsCgroup2UnifiedMode()),
		CrashLoopBackOff: kubeletconfig.CrashLoopBackOffConfig{
			MaxContainerRestartPeriod: &metav1.Duration{Duration: 3 * time.Second},
		},
	}
)

func TestValidateKubeletConfiguration(t *testing.T) {
	featureGate := utilfeature.DefaultFeatureGate.DeepCopy()
	logsapi.AddFeatureGates(featureGate)

	cases := []struct {
		name      string
		configure func(config *kubeletconfig.KubeletConfiguration) *kubeletconfig.KubeletConfiguration
		errMsg    string
	}{{
		name: "Success",
		configure: func(conf *kubeletconfig.KubeletConfiguration) *kubeletconfig.KubeletConfiguration {
			return conf
		},
	}, {
		name: "invalid NodeLeaseDurationSeconds",
		configure: func(conf *kubeletconfig.KubeletConfiguration) *kubeletconfig.KubeletConfiguration {
			conf.NodeLeaseDurationSeconds = 0
			return conf
		},
		errMsg: "invalid configuration: nodeLeaseDurationSeconds must be greater than 0",
	}, {
		name: "specify EnforceNodeAllocatable without enabling CgroupsPerQOS",
		configure: func(conf *kubeletconfig.KubeletConfiguration) *kubeletconfig.KubeletConfiguration {
			conf.CgroupsPerQOS = false
			conf.EnforceNodeAllocatable = []string{kubetypes.NodeAllocatableEnforcementKey}
			return conf
		},
		errMsg: "invalid configuration: cgroupsPerQOS (--cgroups-per-qos) must be set to true when \"pods\" contained in enforceNodeAllocatable (--enforce-node-allocatable)",
	}, {
		name: "specify SystemCgroups without CgroupRoot",
		configure: func(conf *kubeletconfig.KubeletConfiguration) *kubeletconfig.KubeletConfiguration {
			conf.SystemCgroups = "/"
			conf.CgroupRoot = ""
			return conf
		},
		errMsg: "invalid configuration: systemCgroups (--system-cgroups) was specified and cgroupRoot (--cgroup-root) was not specified",
	}, {
		name: "invalid EventBurst",
		configure: func(conf *kubeletconfig.KubeletConfiguration) *kubeletconfig.KubeletConfiguration {
			conf.EventBurst = -1
			return conf
		},
		errMsg: "invalid configuration: eventBurst (--event-burst) -1 must not be a negative number",
	}, {
		name: "invalid EventRecordQPS",
		configure: func(conf *kubeletconfig.KubeletConfiguration) *kubeletconfig.KubeletConfiguration {
			conf.EventRecordQPS = -1
			return conf
		},
		errMsg: "invalid configuration: eventRecordQPS (--event-qps) -1 must not be a negative number",
	}, {
		name: "invalid HealthzPort",
		configure: func(conf *kubeletconfig.KubeletConfiguration) *kubeletconfig.KubeletConfiguration {
			conf.HealthzPort = 65536
			return conf
		},
		errMsg: "invalid configuration: healthzPort (--healthz-port) 65536 must be between 1 and 65535, inclusive",
	}, {
		name: "specify CPUCFSQuotaPeriod without enabling CPUCFSQuotaPeriod",
		configure: func(conf *kubeletconfig.KubeletConfiguration) *kubeletconfig.KubeletConfiguration {
			conf.FeatureGates = map[string]bool{"CustomCPUCFSQuotaPeriod": false}
			conf.CPUCFSQuotaPeriod = metav1.Duration{Duration: 200 * time.Millisecond}
			return conf
		},
		errMsg: "invalid configuration: cpuCFSQuotaPeriod (--cpu-cfs-quota-period) {200ms} requires feature gate CustomCPUCFSQuotaPeriod",
	}, {
		name: "invalid CPUCFSQuotaPeriod",
		configure: func(conf *kubeletconfig.KubeletConfiguration) *kubeletconfig.KubeletConfiguration {
			conf.FeatureGates = map[string]bool{"CustomCPUCFSQuotaPeriod": true}
			conf.CPUCFSQuotaPeriod = metav1.Duration{Duration: 2 * time.Second}
			return conf
		},
		errMsg: "invalid configuration: cpuCFSQuotaPeriod (--cpu-cfs-quota-period) {2s} must be between 1ms and 1sec, inclusive",
	}, {
		name: "invalid ImageGCHighThresholdPercent",
		configure: func(conf *kubeletconfig.KubeletConfiguration) *kubeletconfig.KubeletConfiguration {
			conf.ImageGCHighThresholdPercent = 101
			return conf
		},
		errMsg: "invalid configuration: imageGCHighThresholdPercent (--image-gc-high-threshold) 101 must be between 0 and 100, inclusive",
	}, {
		name: "invalid ImageGCLowThresholdPercent",
		configure: func(conf *kubeletconfig.KubeletConfiguration) *kubeletconfig.KubeletConfiguration {
			conf.ImageGCLowThresholdPercent = -1
			return conf
		},
		errMsg: "invalid configuration: imageGCLowThresholdPercent (--image-gc-low-threshold) -1 must be between 0 and 100, inclusive",
	}, {
		name: "ImageGCLowThresholdPercent is equal to ImageGCHighThresholdPercent",
		configure: func(conf *kubeletconfig.KubeletConfiguration) *kubeletconfig.KubeletConfiguration {
			conf.ImageGCHighThresholdPercent = 0
			conf.ImageGCLowThresholdPercent = 0
			return conf
		},
		errMsg: "invalid configuration: imageGCLowThresholdPercent (--image-gc-low-threshold) 0 must be less than imageGCHighThresholdPercent (--image-gc-high-threshold) 0",
	}, {
		name: "ImageGCLowThresholdPercent is greater than ImageGCHighThresholdPercent",
		configure: func(conf *kubeletconfig.KubeletConfiguration) *kubeletconfig.KubeletConfiguration {
			conf.ImageGCHighThresholdPercent = 0
			conf.ImageGCLowThresholdPercent = 1
			return conf
		},
		errMsg: "invalid configuration: imageGCLowThresholdPercent (--image-gc-low-threshold) 1 must be less than imageGCHighThresholdPercent (--image-gc-high-threshold) 0",
	}, {
		name: "invalid IPTablesDropBit",
		configure: func(conf *kubeletconfig.KubeletConfiguration) *kubeletconfig.KubeletConfiguration {
			conf.IPTablesDropBit = 32
			return conf
		},
		errMsg: "invalid configuration: iptablesDropBit (--iptables-drop-bit) 32 must be between 0 and 31, inclusive",
	}, {
		name: "invalid IPTablesMasqueradeBit",
		configure: func(conf *kubeletconfig.KubeletConfiguration) *kubeletconfig.KubeletConfiguration {
			conf.IPTablesMasqueradeBit = 32
			return conf
		},
		errMsg: "invalid configuration: iptablesMasqueradeBit (--iptables-masquerade-bit) 32 must be between 0 and 31, inclusive",
	}, {
		name: "invalid KubeAPIBurst",
		configure: func(conf *kubeletconfig.KubeletConfiguration) *kubeletconfig.KubeletConfiguration {
			conf.KubeAPIBurst = -1
			return conf
		},
		errMsg: "invalid configuration: kubeAPIBurst (--kube-api-burst) -1 must not be a negative number",
	}, {
		name: "invalid KubeAPIQPS",
		configure: func(conf *kubeletconfig.KubeletConfiguration) *kubeletconfig.KubeletConfiguration {
			conf.KubeAPIQPS = -1
			return conf
		},
		errMsg: "invalid configuration: kubeAPIQPS (--kube-api-qps) -1 must not be a negative number",
	}, {
		name: "invalid NodeStatusMaxImages",
		configure: func(conf *kubeletconfig.KubeletConfiguration) *kubeletconfig.KubeletConfiguration {
			conf.NodeStatusMaxImages = -2
			return conf
		},
		errMsg: "invalid configuration: nodeStatusMaxImages (--node-status-max-images) -2 must be -1 or greater",
	}, {
		name: "invalid MaxOpenFiles",
		configure: func(conf *kubeletconfig.KubeletConfiguration) *kubeletconfig.KubeletConfiguration {
			conf.MaxOpenFiles = -1
			return conf
		},
		errMsg: "invalid configuration: maxOpenFiles (--max-open-files) -1 must not be a negative number",
	}, {
		name: "invalid MaxPods",
		configure: func(conf *kubeletconfig.KubeletConfiguration) *kubeletconfig.KubeletConfiguration {
			conf.MaxPods = -1
			return conf
		},
		errMsg: "invalid configuration: maxPods (--max-pods) -1 must not be a negative number",
	}, {
		name: "invalid OOMScoreAdj",
		configure: func(conf *kubeletconfig.KubeletConfiguration) *kubeletconfig.KubeletConfiguration {
			conf.OOMScoreAdj = 1001
			return conf
		},
		errMsg: "invalid configuration: oomScoreAdj (--oom-score-adj) 1001 must be between -1000 and 1000, inclusive",
	}, {
		name: "invalid PodsPerCore",
		configure: func(conf *kubeletconfig.KubeletConfiguration) *kubeletconfig.KubeletConfiguration {
			conf.PodsPerCore = -1
			return conf
		},
		errMsg: "invalid configuration: podsPerCore (--pods-per-core) -1 must not be a negative number",
	}, {
		name: "invalid Port",
		configure: func(conf *kubeletconfig.KubeletConfiguration) *kubeletconfig.KubeletConfiguration {
			conf.Port = 65536
			return conf
		},
		errMsg: "invalid configuration: port (--port) 65536 must be between 1 and 65535, inclusive",
	}, {
		name: "invalid ReadOnlyPort",
		configure: func(conf *kubeletconfig.KubeletConfiguration) *kubeletconfig.KubeletConfiguration {
			conf.ReadOnlyPort = 65536
			return conf
		},
		errMsg: "invalid configuration: readOnlyPort (--read-only-port) 65536 must be between 0 and 65535, inclusive",
	}, {
		name: "invalid RegistryBurst",
		configure: func(conf *kubeletconfig.KubeletConfiguration) *kubeletconfig.KubeletConfiguration {
			conf.RegistryBurst = -1
			return conf
		},
		errMsg: "invalid configuration: registryBurst (--registry-burst) -1 must not be a negative number",
	}, {
		name: "invalid RegistryPullQPS",
		configure: func(conf *kubeletconfig.KubeletConfiguration) *kubeletconfig.KubeletConfiguration {
			conf.RegistryPullQPS = -1
			return conf
		},
		errMsg: "invalid configuration: registryPullQPS (--registry-qps) -1 must not be a negative number",
	}, {
		name: "invalid MaxParallelImagePulls",
		configure: func(conf *kubeletconfig.KubeletConfiguration) *kubeletconfig.KubeletConfiguration {
			conf.MaxParallelImagePulls = ptr.To[int32](0)
			return conf
		},
		errMsg: "invalid configuration: maxParallelImagePulls 0 must be a positive number",
	}, {
		name: "invalid MaxParallelImagePulls and SerializeImagePulls combination",
		configure: func(conf *kubeletconfig.KubeletConfiguration) *kubeletconfig.KubeletConfiguration {
			conf.MaxParallelImagePulls = ptr.To[int32](3)
			conf.SerializeImagePulls = true
			return conf
		},
		errMsg: "invalid configuration: maxParallelImagePulls cannot be larger than 1 unless SerializeImagePulls (--serialize-image-pulls) is set to false",
	}, {
		name: "valid MaxParallelImagePulls and SerializeImagePulls combination",
		configure: func(conf *kubeletconfig.KubeletConfiguration) *kubeletconfig.KubeletConfiguration {
			conf.MaxParallelImagePulls = ptr.To[int32](1)
			conf.SerializeImagePulls = true
			return conf
		},
	}, {
		name: "specify ServerTLSBootstrap without enabling RotateKubeletServerCertificate",
		configure: func(conf *kubeletconfig.KubeletConfiguration) *kubeletconfig.KubeletConfiguration {
			conf.FeatureGates = map[string]bool{"RotateKubeletServerCertificate": false}
			conf.ServerTLSBootstrap = true
			return conf
		},
		errMsg: "invalid configuration: serverTLSBootstrap true requires feature gate RotateKubeletServerCertificate",
	}, {
		name: "invalid TopologyManagerPolicy",
		configure: func(conf *kubeletconfig.KubeletConfiguration) *kubeletconfig.KubeletConfiguration {
			conf.TopologyManagerPolicy = "invalid-policy"
			return conf
		},
		errMsg: "invalid configuration: topologyManagerPolicy (--topology-manager-policy) \"invalid-policy\" must be one of: [\"none\" \"best-effort\" \"restricted\" \"single-numa-node\"]",
	}, {
		name: "invalid TopologyManagerScope",
		configure: func(conf *kubeletconfig.KubeletConfiguration) *kubeletconfig.KubeletConfiguration {
			conf.TopologyManagerScope = "invalid-scope"
			return conf
		},
		errMsg: "invalid configuration: topologyManagerScope (--topology-manager-scope) \"invalid-scope\" must be one of: \"container\", or \"pod\"",
	}, {
		name: "ShutdownGracePeriodCriticalPods is greater than ShutdownGracePeriod",
		configure: func(conf *kubeletconfig.KubeletConfiguration) *kubeletconfig.KubeletConfiguration {
			conf.FeatureGates = map[string]bool{"GracefulNodeShutdown": true}
			conf.ShutdownGracePeriodCriticalPods = metav1.Duration{Duration: 2 * time.Second}
			conf.ShutdownGracePeriod = metav1.Duration{Duration: 1 * time.Second}
			return conf
		},
		errMsg: "invalid configuration: shutdownGracePeriodCriticalPods {2s} must be <= shutdownGracePeriod {1s}",
	}, {
		name: "ShutdownGracePeriod is less than 1 sec",
		configure: func(conf *kubeletconfig.KubeletConfiguration) *kubeletconfig.KubeletConfiguration {
			conf.FeatureGates = map[string]bool{"GracefulNodeShutdown": true}
			conf.ShutdownGracePeriod = metav1.Duration{Duration: 1 * time.Millisecond}
			return conf
		},
		errMsg: "invalid configuration: shutdownGracePeriod {1ms} must be either zero or otherwise >= 1 sec",
	}, {
		name: "ShutdownGracePeriodCriticalPods is less than 1 sec",
		configure: func(conf *kubeletconfig.KubeletConfiguration) *kubeletconfig.KubeletConfiguration {
			conf.FeatureGates = map[string]bool{"GracefulNodeShutdown": true}
			conf.ShutdownGracePeriodCriticalPods = metav1.Duration{Duration: 1 * time.Millisecond}
			return conf
		},
		errMsg: "invalid configuration: shutdownGracePeriodCriticalPods {1ms} must be either zero or otherwise >= 1 sec",
	}, {
		name: "specify ShutdownGracePeriod without enabling GracefulNodeShutdown",
		configure: func(conf *kubeletconfig.KubeletConfiguration) *kubeletconfig.KubeletConfiguration {
			conf.FeatureGates = map[string]bool{"GracefulNodeShutdown": false}
			conf.ShutdownGracePeriod = metav1.Duration{Duration: 1 * time.Second}
			return conf
		},
		errMsg: "invalid configuration: specifying shutdownGracePeriod or shutdownGracePeriodCriticalPods requires feature gate GracefulNodeShutdown",
	}, {
		name: "specify ShutdownGracePeriodCriticalPods without enabling GracefulNodeShutdown",
		configure: func(conf *kubeletconfig.KubeletConfiguration) *kubeletconfig.KubeletConfiguration {
			conf.FeatureGates = map[string]bool{"GracefulNodeShutdown": false}
			conf.ShutdownGracePeriodCriticalPods = metav1.Duration{Duration: 1 * time.Second}
			return conf
		},
		errMsg: "invalid configuration: specifying shutdownGracePeriod or shutdownGracePeriodCriticalPods requires feature gate GracefulNodeShutdown",
	}, {
		name: "invalid MemorySwap.SwapBehavior",
		configure: func(conf *kubeletconfig.KubeletConfiguration) *kubeletconfig.KubeletConfiguration {
			conf.FeatureGates = map[string]bool{"NodeSwap": true}
			conf.MemorySwap.SwapBehavior = "invalid-behavior"
			return conf
		},
		errMsg: "invalid configuration: memorySwap.swapBehavior \"invalid-behavior\" must be one of: \"\", \"LimitedSwap\" or \"NoSwap\"",
	}, {
		name: "specify MemorySwap.SwapBehavior without enabling NodeSwap",
		configure: func(conf *kubeletconfig.KubeletConfiguration) *kubeletconfig.KubeletConfiguration {
			conf.FeatureGates = map[string]bool{"NodeSwap": false}
			conf.MemorySwap.SwapBehavior = kubetypes.LimitedSwap
			return conf
		},
		errMsg: "invalid configuration: memorySwap.swapBehavior cannot be set when NodeSwap feature flag is disabled",
	}, {
		name: "CrashLoopBackOff.MaxContainerRestartPeriod too low",
		configure: func(conf *kubeletconfig.KubeletConfiguration) *kubeletconfig.KubeletConfiguration {
			conf.FeatureGates = map[string]bool{"KubeletCrashLoopBackOffMax": true}
			conf.CrashLoopBackOff = kubeletconfig.CrashLoopBackOffConfig{
				MaxContainerRestartPeriod: &metav1.Duration{Duration: 0 * time.Second},
			}
			return conf
		},
		errMsg: "invalid configuration: CrashLoopBackOff.MaxContainerRestartPeriod (got: 0 seconds) must be set between 1s and 300s",
	}, {
		name: "CrashLoopBackOff.MaxContainerRestartPeriod too high",
		configure: func(conf *kubeletconfig.KubeletConfiguration) *kubeletconfig.KubeletConfiguration {
			conf.FeatureGates = map[string]bool{"KubeletCrashLoopBackOffMax": true}
			conf.CrashLoopBackOff = kubeletconfig.CrashLoopBackOffConfig{
				MaxContainerRestartPeriod: &metav1.Duration{Duration: 301 * time.Second},
			}
			return conf
		},
		errMsg: "invalid configuration: CrashLoopBackOff.MaxContainerRestartPeriod (got: 301 seconds) must be set between 1s and 300s",
	}, {
		name: "CrashLoopBackOff.MaxContainerRestartPeriod just a little too high",
		configure: func(conf *kubeletconfig.KubeletConfiguration) *kubeletconfig.KubeletConfiguration {
			conf.FeatureGates = map[string]bool{"KubeletCrashLoopBackOffMax": true, "CustomCPUCFSQuotaPeriod": true}
			conf.CrashLoopBackOff = kubeletconfig.CrashLoopBackOffConfig{
				// 300.9 seconds
				MaxContainerRestartPeriod: &metav1.Duration{Duration: 300900 * time.Millisecond},
			}
			return conf
		},
		errMsg: "invalid configuration: CrashLoopBackOff.MaxContainerRestartPeriod (got: 300.9 seconds) must be set between 1s and 300s",
	},
		{
			name: "CrashLoopBackOff.MaxContainerRestartPeriod just a little too low",
			configure: func(conf *kubeletconfig.KubeletConfiguration) *kubeletconfig.KubeletConfiguration {
				conf.FeatureGates = map[string]bool{"KubeletCrashLoopBackOffMax": true, "CustomCPUCFSQuotaPeriod": true}
				conf.CrashLoopBackOff = kubeletconfig.CrashLoopBackOffConfig{
					// 300.9 seconds
					MaxContainerRestartPeriod: &metav1.Duration{Duration: 999 * time.Millisecond},
				}
				return conf
			},
			errMsg: "invalid configuration: CrashLoopBackOff.MaxContainerRestartPeriod (got: 0.999 seconds) must be set between 1s and 300s",
		},
		{
			name: "KubeletCrashLoopBackOffMax feature gate on, no crashLoopBackOff config, ok",
			configure: func(conf *kubeletconfig.KubeletConfiguration) *kubeletconfig.KubeletConfiguration {
				conf.FeatureGates = map[string]bool{"KubeletCrashLoopBackOffMax": true, "CustomCPUCFSQuotaPeriod": true}
				return conf
			},
		}, {
			name: "KubeletCrashLoopBackOffMax feature gate on, but no crashLoopBackOff.MaxContainerRestartPeriod config",
			configure: func(conf *kubeletconfig.KubeletConfiguration) *kubeletconfig.KubeletConfiguration {
				conf.FeatureGates = map[string]bool{"KubeletCrashLoopBackOffMax": true, "CustomCPUCFSQuotaPeriod": true}
				conf.CrashLoopBackOff = kubeletconfig.CrashLoopBackOffConfig{}
				return conf
			},
			errMsg: "invalid configuration: FeatureGate KubeletCrashLoopBackOffMax is enabled, CrashLoopBackOff.MaxContainerRestartPeriod must be set",
		},
		{
			name: "specify SystemReservedEnforcementKey without specifying SystemReservedCgroup",
			configure: func(conf *kubeletconfig.KubeletConfiguration) *kubeletconfig.KubeletConfiguration {
				conf.EnforceNodeAllocatable = []string{kubetypes.SystemReservedEnforcementKey}
				conf.SystemReservedCgroup = ""
				return conf
			},
			errMsg: "invalid configuration: systemReservedCgroup (--system-reserved-cgroup) must be specified when \"system-reserved\" or \"system-reserved-compressible\" included in enforceNodeAllocatable (--enforce-node-allocatable)",
		}, {
			name: "specify SystemReservedCompressibleEnforcementKey without specifying SystemReservedCgroup",
			configure: func(conf *kubeletconfig.KubeletConfiguration) *kubeletconfig.KubeletConfiguration {
				conf.EnforceNodeAllocatable = []string{kubetypes.SystemReservedCompressibleEnforcementKey}
				conf.SystemReservedCgroup = ""
				return conf
			},
			errMsg: "invalid configuration: systemReservedCgroup (--system-reserved-cgroup) must be specified when \"system-reserved\" or \"system-reserved-compressible\" included in enforceNodeAllocatable (--enforce-node-allocatable)",
		}, {
			name: "specify SystemReservedCompressibleEnforcementKey with SystemReservedEnforcementKey",
			configure: func(conf *kubeletconfig.KubeletConfiguration) *kubeletconfig.KubeletConfiguration {
				conf.EnforceNodeAllocatable = []string{kubetypes.SystemReservedCompressibleEnforcementKey, kubetypes.SystemReservedEnforcementKey}
				return conf
			},
			errMsg: "invalid configuration: both \"system-reserved\" and \"system-reserved-compressible\" cannot be specified together in enforceNodeAllocatable (--enforce-node-allocatable)",
		}, {
			name: "specify KubeReservedCompressibleEnforcementKey without specifying KubeReservedCgroup",
			configure: func(conf *kubeletconfig.KubeletConfiguration) *kubeletconfig.KubeletConfiguration {
				conf.EnforceNodeAllocatable = []string{kubetypes.KubeReservedCompressibleEnforcementKey}
				conf.KubeReservedCgroup = ""
				return conf
			},
			errMsg: "invalid configuration: kubeReservedCgroup (--kube-reserved-cgroup) must be specified when \"kube-reserved\" or \"kube-reserved-compressible\" included in enforceNodeAllocatable (--enforce-node-allocatable)",
		}, {
			name: "specify KubeReservedEnforcementKey without specifying KubeReservedCgroup",
			configure: func(conf *kubeletconfig.KubeletConfiguration) *kubeletconfig.KubeletConfiguration {
				conf.EnforceNodeAllocatable = []string{kubetypes.KubeReservedEnforcementKey}
				conf.KubeReservedCgroup = ""
				return conf
			},
			errMsg: "invalid configuration: kubeReservedCgroup (--kube-reserved-cgroup) must be specified when \"kube-reserved\" or \"kube-reserved-compressible\" included in enforceNodeAllocatable (--enforce-node-allocatable)",
		}, {
			name: "specify KubeReservedCompressibleEnforcementKey with KubeReservedEnforcementKey",
			configure: func(conf *kubeletconfig.KubeletConfiguration) *kubeletconfig.KubeletConfiguration {
				conf.EnforceNodeAllocatable = []string{kubetypes.KubeReservedCompressibleEnforcementKey, kubetypes.KubeReservedEnforcementKey}
				return conf
			},
			errMsg: "invalid configuration: both \"kube-reserved\" and \"kube-reserved-compressible\" cannot be specified together in enforceNodeAllocatable (--enforce-node-allocatable)",
		}, {
			name: "specify NodeAllocatableNoneKey with additional enforcements",
			configure: func(conf *kubeletconfig.KubeletConfiguration) *kubeletconfig.KubeletConfiguration {
				conf.EnforceNodeAllocatable = []string{kubetypes.NodeAllocatableNoneKey, kubetypes.KubeReservedEnforcementKey}
				return conf
			},
			errMsg: "invalid configuration: enforceNodeAllocatable (--enforce-node-allocatable) may not contain additional enforcements when \"none\" is specified",
		}, {
			name: "duplicated EnforceNodeAllocatable",
			configure: func(conf *kubeletconfig.KubeletConfiguration) *kubeletconfig.KubeletConfiguration {
				conf.EnforceNodeAllocatable = []string{kubetypes.NodeAllocatableNoneKey, kubetypes.NodeAllocatableNoneKey}
				return conf
			},
			errMsg: "invalid configuration: duplicated enforcements \"none\" in enforceNodeAllocatable (--enforce-node-allocatable)",
		}, {
			name: "invalid EnforceNodeAllocatable",
			configure: func(conf *kubeletconfig.KubeletConfiguration) *kubeletconfig.KubeletConfiguration {
				conf.EnforceNodeAllocatable = []string{"invalid-enforce-node-allocatable"}
				return conf
			},
			errMsg: "invalid configuration: option \"invalid-enforce-node-allocatable\" specified for enforceNodeAllocatable (--enforce-node-allocatable). Valid options are \"pods\", \"system-reserved\", \"system-reserved-compressible\", \"kube-reserved\", \"kube-reserved-compressible\" or \"none\"",
		}, {
			name: "invalid HairpinMode",
			configure: func(conf *kubeletconfig.KubeletConfiguration) *kubeletconfig.KubeletConfiguration {
				conf.HairpinMode = "invalid-hair-pin-mode"
				return conf
			},
			errMsg: "invalid configuration: option \"invalid-hair-pin-mode\" specified for hairpinMode (--hairpin-mode). Valid options are \"none\", \"hairpin-veth\" or \"promiscuous-bridge\"",
		}, {
			name: "specify ReservedSystemCPUs with SystemReservedCgroup",
			configure: func(conf *kubeletconfig.KubeletConfiguration) *kubeletconfig.KubeletConfiguration {
				conf.ReservedSystemCPUs = "0-3"
				conf.SystemReservedCgroup = "/system.slice"
				return conf
			},
			errMsg: "invalid configuration: can't use reservedSystemCPUs (--reserved-cpus) with systemReservedCgroup (--system-reserved-cgroup) or kubeReservedCgroup (--kube-reserved-cgroup)",
		}, {
			name: "specify ReservedSystemCPUs with KubeReservedCgroup",
			configure: func(conf *kubeletconfig.KubeletConfiguration) *kubeletconfig.KubeletConfiguration {
				conf.ReservedSystemCPUs = "0-3"
				conf.KubeReservedCgroup = "/system.slice"
				return conf
			},
			errMsg: "invalid configuration: can't use reservedSystemCPUs (--reserved-cpus) with systemReservedCgroup (--system-reserved-cgroup) or kubeReservedCgroup (--kube-reserved-cgroup)",
		}, {
			name: "invalid ReservedSystemCPUs",
			configure: func(conf *kubeletconfig.KubeletConfiguration) *kubeletconfig.KubeletConfiguration {
				conf.ReservedSystemCPUs = "invalid-reserved-system-cpus"
				return conf
			},
			errMsg: "invalid configuration: unable to parse reservedSystemCPUs (--reserved-cpus) invalid-reserved-system-cpus, error:",
		}, {
			name: "enable MemoryQoS without specifying MemoryThrottlingFactor",
			configure: func(conf *kubeletconfig.KubeletConfiguration) *kubeletconfig.KubeletConfiguration {
				conf.FeatureGates = map[string]bool{"MemoryQoS": true}
				conf.MemoryThrottlingFactor = nil
				return conf
			},
			errMsg: "invalid configuration: memoryThrottlingFactor is required when MemoryQoS feature flag is enabled",
		}, {
			name: "invalid MemoryThrottlingFactor",
			configure: func(conf *kubeletconfig.KubeletConfiguration) *kubeletconfig.KubeletConfiguration {
				conf.MemoryThrottlingFactor = ptr.To(1.1)
				return conf
			},
			errMsg: "invalid configuration: memoryThrottlingFactor 1.1 must be greater than 0 and less than or equal to 1.0",
		}, {
			name: "invalid Taint.TimeAdded",
			configure: func(conf *kubeletconfig.KubeletConfiguration) *kubeletconfig.KubeletConfiguration {
				now := metav1.Now()
				conf.RegisterWithTaints = []v1.Taint{{TimeAdded: &now}}
				return conf
			},
			errMsg: "invalid configuration: taint.TimeAdded is not nil",
		}, {
			name: "specify tracing with KubeletTracing disabled",
			configure: func(conf *kubeletconfig.KubeletConfiguration) *kubeletconfig.KubeletConfiguration {
				samplingRate := int32(99999)
				conf.FeatureGates = map[string]bool{"KubeletTracing": false}
				conf.Tracing = &tracingapi.TracingConfiguration{SamplingRatePerMillion: &samplingRate}
				return conf
			},
			errMsg: "invalid configuration: tracing should not be configured if KubeletTracing feature flag is disabled.",
		}, {
			name: "specify tracing invalid sampling rate",
			configure: func(conf *kubeletconfig.KubeletConfiguration) *kubeletconfig.KubeletConfiguration {
				samplingRate := int32(-1)
				conf.FeatureGates = map[string]bool{"KubeletTracing": true}
				conf.Tracing = &tracingapi.TracingConfiguration{SamplingRatePerMillion: &samplingRate}
				return conf
			},
			errMsg: "tracing.samplingRatePerMillion: Invalid value: -1: sampling rate must be positive",
		}, {
			name: "specify tracing invalid endpoint",
			configure: func(conf *kubeletconfig.KubeletConfiguration) *kubeletconfig.KubeletConfiguration {
				ep := "dn%2s://localhost:4317"
				conf.FeatureGates = map[string]bool{"KubeletTracing": true}
				conf.Tracing = &tracingapi.TracingConfiguration{Endpoint: &ep}
				return conf
			},
			errMsg: "tracing.endpoint: Invalid value: \"dn%2s://localhost:4317\": parse \"dn%2s://localhost:4317\": first path segment in URL cannot contain colon",
		}, {
			name: "invalid GracefulNodeShutdownBasedOnPodPriority",
			configure: func(conf *kubeletconfig.KubeletConfiguration) *kubeletconfig.KubeletConfiguration {
				conf.FeatureGates = map[string]bool{"GracefulNodeShutdownBasedOnPodPriority": true}
				conf.ShutdownGracePeriodByPodPriority = []kubeletconfig.ShutdownGracePeriodByPodPriority{{
					Priority:                   0,
					ShutdownGracePeriodSeconds: 0,
				}}
				return conf
			},
			errMsg: "invalid configuration: Cannot specify both shutdownGracePeriodByPodPriority and shutdownGracePeriod at the same time",
		}, {
			name: "Specifying shutdownGracePeriodByPodPriority without enable GracefulNodeShutdownBasedOnPodPriority",
			configure: func(conf *kubeletconfig.KubeletConfiguration) *kubeletconfig.KubeletConfiguration {
				conf.FeatureGates = map[string]bool{"GracefulNodeShutdownBasedOnPodPriority": false}
				conf.ShutdownGracePeriodByPodPriority = []kubeletconfig.ShutdownGracePeriodByPodPriority{{
					Priority:                   0,
					ShutdownGracePeriodSeconds: 0,
				}}
				return conf
			},
			errMsg: "invalid configuration: Specifying shutdownGracePeriodByPodPriority requires feature gate GracefulNodeShutdownBasedOnPodPriority",
		}, {
			name: "enableSystemLogQuery is enabled without NodeLogQuery feature gate",
			configure: func(conf *kubeletconfig.KubeletConfiguration) *kubeletconfig.KubeletConfiguration {
				conf.EnableSystemLogQuery = true
				return conf
			},
			errMsg: "invalid configuration: NodeLogQuery feature gate is required for enableSystemLogHandler",
		}, {
			name: "enableSystemLogQuery is enabled without enableSystemLogHandler",
			configure: func(conf *kubeletconfig.KubeletConfiguration) *kubeletconfig.KubeletConfiguration {
				conf.FeatureGates = map[string]bool{"NodeLogQuery": true}
				conf.EnableSystemLogHandler = false
				conf.EnableSystemLogQuery = true
				return conf
			},
			errMsg: "invalid configuration: enableSystemLogHandler is required for enableSystemLogQuery",
		}, {
			name: "imageMaximumGCAge should not be specified without feature gate",
			configure: func(conf *kubeletconfig.KubeletConfiguration) *kubeletconfig.KubeletConfiguration {
				conf.FeatureGates = map[string]bool{"ImageMaximumGCAge": false}
				conf.ImageMaximumGCAge = metav1.Duration{Duration: 1}
				return conf
			},
			errMsg: "invalid configuration: ImageMaximumGCAge feature gate is required for Kubelet configuration option imageMaximumGCAge",
		}, {
			name: "imageMaximumGCAge should not be negative",
			configure: func(conf *kubeletconfig.KubeletConfiguration) *kubeletconfig.KubeletConfiguration {
				conf.FeatureGates = map[string]bool{"ImageMaximumGCAge": true}
				conf.ImageMaximumGCAge = metav1.Duration{Duration: -1}
				return conf
			},
			errMsg: "invalid configuration: imageMaximumGCAge -1ns must not be negative",
		}, {
			name: "imageMaximumGCAge should not be less than imageMinimumGCAge",
			configure: func(conf *kubeletconfig.KubeletConfiguration) *kubeletconfig.KubeletConfiguration {
				conf.FeatureGates = map[string]bool{"ImageMaximumGCAge": true}
				conf.ImageMaximumGCAge = metav1.Duration{Duration: 1}
				conf.ImageMinimumGCAge = metav1.Duration{Duration: 2}
				return conf
			},
			errMsg: "invalid configuration: imageMaximumGCAge 1ns must be greater than imageMinimumGCAge 2ns",
		}, {
			name: "containerLogMaxWorkers must be greater than or equal to 1",
			configure: func(conf *kubeletconfig.KubeletConfiguration) *kubeletconfig.KubeletConfiguration {
				conf.ContainerLogMaxWorkers = 0
				return conf
			},
			errMsg: "invalid configuration: containerLogMaxWorkers must be greater than or equal to 1",
		}, {
			name: "containerLogMonitorInterval must be a positive time duration",
			configure: func(conf *kubeletconfig.KubeletConfiguration) *kubeletconfig.KubeletConfiguration {
				conf.ContainerLogMonitorInterval = metav1.Duration{Duration: -1 * time.Second}
				return conf
			},
			errMsg: "invalid configuration: containerLogMonitorInterval must be a positive time duration greater than or equal to 3s",
		}, {
			name: "containerLogMonitorInterval must be at least 3s or higher",
			configure: func(conf *kubeletconfig.KubeletConfiguration) *kubeletconfig.KubeletConfiguration {
				conf.ContainerLogMonitorInterval = metav1.Duration{Duration: 2 * time.Second}
				return conf
			},
			errMsg: "invalid configuration: containerLogMonitorInterval must be a positive time duration greater than or equal to 3s",
		}, {
			name: "pod logs path must be not empty",
			configure: func(config *kubeletconfig.KubeletConfiguration) *kubeletconfig.KubeletConfiguration {
				config.PodLogsDir = ""
				return config
			},
			errMsg: "invalid configuration: podLogsDir was not specified",
		}, {
			name: "pod logs path must be absolute",
			configure: func(config *kubeletconfig.KubeletConfiguration) *kubeletconfig.KubeletConfiguration {
				config.PodLogsDir = "./test"
				return config
			},
			errMsg: `invalid configuration: pod logs path "./test" must be absolute path`,
		}, {
			name: "pod logs path must be normalized",
			configure: func(config *kubeletconfig.KubeletConfiguration) *kubeletconfig.KubeletConfiguration {
				config.PodLogsDir = "/path/../"
				return config
			},
			errMsg: `invalid configuration: pod logs path "/path/../" must be normalized`,
		}, {
			name: "pod logs path is ascii only",
			configure: func(config *kubeletconfig.KubeletConfiguration) *kubeletconfig.KubeletConfiguration {
				config.PodLogsDir = "/🧪"
				return config
			},
			errMsg: `invalid configuration: pod logs path "/🧪" mut contains ASCII characters only`,
		}, {
			name: "invalid containerLogMaxFiles",
			configure: func(conf *kubeletconfig.KubeletConfiguration) *kubeletconfig.KubeletConfiguration {
				conf.ContainerLogMaxFiles = 1
				return conf
			},
			errMsg: "invalid configuration: containerLogMaxFiles must be greater than 1",
		}, {
			name: "invalid ContainerRuntimeEndpoint",
			configure: func(conf *kubeletconfig.KubeletConfiguration) *kubeletconfig.KubeletConfiguration {
				conf.ContainerRuntimeEndpoint = ""
				return conf
			},
			errMsg: "invalid configuration: the containerRuntimeEndpoint was not specified or empty",
		}, {
			name: "invalid Logging configuration",
			configure: func(conf *kubeletconfig.KubeletConfiguration) *kubeletconfig.KubeletConfiguration {
				conf.Logging.Format = "invalid"
				return conf
			},
			errMsg: "logging.format: Invalid value: \"invalid\": Unsupported log format",
		}, {
			name: "invalid imagePullCredentialsVerificationPolicy configuration",
			configure: func(conf *kubeletconfig.KubeletConfiguration) *kubeletconfig.KubeletConfiguration {
				conf.FeatureGates = map[string]bool{"KubeletEnsureSecretPulledImages": true}
				conf.ImagePullCredentialsVerificationPolicy = "invalid"
				return conf
			},
			errMsg: `option "invalid" specified for imagePullCredentialsVerificationPolicy. Valid options are "NeverVerify", "NeverVerifyPreloadedImages", "NeverVerifyAllowlistedImages" or "AlwaysVerify"]`,
		}, {
			name: "invalid PreloadedImagesVerificationAllowlist configuration - featuregate enabled",
			configure: func(conf *kubeletconfig.KubeletConfiguration) *kubeletconfig.KubeletConfiguration {
				conf.FeatureGates = map[string]bool{"KubeletEnsureSecretPulledImages": true}
				conf.ImagePullCredentialsVerificationPolicy = string(v1alpha1.NeverVerify)
				conf.PreloadedImagesVerificationAllowlist = []string{"test.test/repo"}
				return conf
			},
			errMsg: "can't set `preloadedImagesVerificationAllowlist` if `imagePullCredentialsVertificationPolicy` is not \"NeverVerifyAllowlistedImages\"]",
		}, {
			name: "invalid PreloadedImagesVerificationAllowlist configuration - featuregate disabled",
			configure: func(conf *kubeletconfig.KubeletConfiguration) *kubeletconfig.KubeletConfiguration {
				conf.FeatureGates = map[string]bool{"KubeletEnsureSecretPulledImages": false}
				conf.ImagePullCredentialsVerificationPolicy = string(v1alpha1.NeverVerify)
				return conf
			},
			errMsg: "invalid configuration: `imagePullCredentialsVerificationPolicy` must not be set if KubeletEnsureSecretPulledImages feature gate is not enabled",
		}, {
			name: "invalid PreloadedImagesVerificationAllowlist configuration",
			configure: func(conf *kubeletconfig.KubeletConfiguration) *kubeletconfig.KubeletConfiguration {
				conf.FeatureGates = map[string]bool{"KubeletEnsureSecretPulledImages": false}
				conf.PreloadedImagesVerificationAllowlist = []string{"test.test/repo"}
				return conf
			},
			errMsg: "invalid configuration: `preloadedImagesVerificationAllowlist` must not be set if KubeletEnsureSecretPulledImages feature gate is not enabled",
		}, {
			name: "invalid FeatureGate",
			configure: func(conf *kubeletconfig.KubeletConfiguration) *kubeletconfig.KubeletConfiguration {
				conf.FeatureGates["invalid"] = true
				return conf
			},
			errMsg: "unrecognized feature gate: invalid",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errs := validation.ValidateKubeletConfiguration(tc.configure(successConfig.DeepCopy()), featureGate)

			if len(tc.errMsg) == 0 {
				if errs != nil {
					t.Errorf("unexpected error: %s", errs)
				}

				return
			}

			if errs == nil {
				t.Errorf("expected error: %s", tc.errMsg)
				return
			}

			if got := errs.Error(); !strings.Contains(got, tc.errMsg) {
				t.Errorf("unexpected error: %s expected to contain %s", got, tc.errMsg)
			}
		})
	}
}
