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

package fuzzer

import (
	"math/rand"
	"time"

	fuzz "github.com/google/gofuzz"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	runtimeserializer "k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/kubelet/config/v1beta1"
	"k8s.io/kubernetes/pkg/cluster/ports"
	kubeletconfig "k8s.io/kubernetes/pkg/kubelet/apis/config"
	kubeletconfigv1beta1 "k8s.io/kubernetes/pkg/kubelet/apis/config/v1beta1"
	"k8s.io/kubernetes/pkg/kubelet/eviction"
	"k8s.io/kubernetes/pkg/kubelet/qos"
	kubetypes "k8s.io/kubernetes/pkg/kubelet/types"
	"k8s.io/utils/ptr"
)

// Funcs returns the fuzzer functions for the kubeletconfig apis.
func Funcs(codecs runtimeserializer.CodecFactory) []interface{} {
	return []interface{}{
		// provide non-empty values for fields with defaults, so the defaulter doesn't change values during round-trip
		func(obj *kubeletconfig.KubeletConfiguration, c fuzz.Continue) {
			c.FuzzNoCustom(obj)
			obj.EnableServer = true
			obj.Authentication.Anonymous.Enabled = true
			obj.Authentication.Webhook.Enabled = false
			obj.Authentication.Webhook.CacheTTL = metav1.Duration{Duration: 2 * time.Minute}
			obj.Authorization.Mode = kubeletconfig.KubeletAuthorizationModeAlwaysAllow
			obj.Authorization.Webhook.CacheAuthorizedTTL = metav1.Duration{Duration: 5 * time.Minute}
			obj.Authorization.Webhook.CacheUnauthorizedTTL = metav1.Duration{Duration: 30 * time.Second}
			obj.Address = "0.0.0.0"
			obj.VolumeStatsAggPeriod = metav1.Duration{Duration: time.Minute}
			obj.RuntimeRequestTimeout = metav1.Duration{Duration: 2 * time.Minute}
			obj.CPUCFSQuota = true
			obj.EventBurst = 10
			obj.EventRecordQPS = 5
			obj.EnableControllerAttachDetach = true
			obj.EnableDebuggingHandlers = true
			obj.EnableSystemLogQuery = false
			obj.FileCheckFrequency = metav1.Duration{Duration: 20 * time.Second}
			obj.HealthzBindAddress = "127.0.0.1"
			obj.HealthzPort = 10248
			obj.HTTPCheckFrequency = metav1.Duration{Duration: 20 * time.Second}
			obj.ImageMinimumGCAge = metav1.Duration{Duration: 2 * time.Minute}
			obj.ImageMaximumGCAge = metav1.Duration{}
			obj.ImageGCHighThresholdPercent = 85
			obj.ImageGCLowThresholdPercent = 80
			obj.KernelMemcgNotification = false
			obj.MaxOpenFiles = 1000000
			obj.MaxPods = 110
			obj.MemoryManagerPolicy = v1beta1.NoneMemoryManagerPolicy
			obj.PodPidsLimit = -1
			obj.NodeStatusUpdateFrequency = metav1.Duration{Duration: 10 * time.Second}
			obj.NodeStatusReportFrequency = metav1.Duration{Duration: time.Minute}
			obj.NodeLeaseDurationSeconds = 40
			obj.CPUManagerPolicy = "none"
			obj.CPUManagerPolicyOptions = nil
			obj.CPUManagerReconcilePeriod = obj.NodeStatusUpdateFrequency
			obj.NodeStatusMaxImages = 50
			obj.TopologyManagerPolicy = kubeletconfig.NoneTopologyManagerPolicy
			obj.TopologyManagerScope = kubeletconfig.ContainerTopologyManagerScope
			obj.TopologyManagerPolicyOptions = nil
			obj.QOSReserved = map[string]string{
				"memory": "50%",
			}
			obj.OOMScoreAdj = int32(qos.KubeletOOMScoreAdj)
			obj.PodLogsDir = "/var/log/pods"
			obj.Port = ports.KubeletPort
			obj.ReadOnlyPort = ports.KubeletReadOnlyPort
			obj.RegistryBurst = 10
			obj.RegistryPullQPS = 5
			obj.ResolverConfig = kubetypes.ResolvConfDefault
			obj.SerializeImagePulls = true
			obj.StreamingConnectionIdleTimeout = metav1.Duration{Duration: 4 * time.Hour}
			obj.SyncFrequency = metav1.Duration{Duration: 1 * time.Minute}
			obj.ContentType = "application/vnd.kubernetes.protobuf"
			obj.KubeAPIQPS = 50
			obj.KubeAPIBurst = 100
			obj.HairpinMode = v1beta1.PromiscuousBridge
			obj.EvictionHard = eviction.DefaultEvictionHard
			obj.EvictionPressureTransitionPeriod = metav1.Duration{Duration: 5 * time.Minute}
			obj.MergeDefaultEvictionSettings = false
			obj.MakeIPTablesUtilChains = true
			obj.IPTablesMasqueradeBit = kubeletconfigv1beta1.DefaultIPTablesMasqueradeBit
			obj.IPTablesDropBit = kubeletconfigv1beta1.DefaultIPTablesDropBit
			obj.CgroupsPerQOS = true
			obj.CgroupDriver = "cgroupfs"
			obj.EnforceNodeAllocatable = kubeletconfigv1beta1.DefaultNodeAllocatableEnforcement
			obj.StaticPodURLHeader = nil
			obj.SingleProcessOOMKill = ptr.To(false)
			obj.ContainerLogMaxFiles = 5
			obj.ContainerLogMaxSize = "10Mi"
			obj.ContainerLogMaxWorkers = 1
			obj.ContainerLogMonitorInterval = metav1.Duration{Duration: 10 * time.Second}
			obj.ConfigMapAndSecretChangeDetectionStrategy = "Watch"
			obj.AllowedUnsafeSysctls = nil
			obj.VolumePluginDir = kubeletconfigv1beta1.DefaultVolumePluginDir
			obj.ContainerRuntimeEndpoint = "unix:///run/containerd/containerd.sock"

			if obj.Logging.Format == "" {
				obj.Logging.Format = "text"
			}
			obj.EnableSystemLogHandler = true
			obj.MemoryThrottlingFactor = ptr.To(rand.Float64())
			obj.LocalStorageCapacityIsolation = true
			obj.FeatureGates = map[string]bool{
				"AllAlpha": false,
				"AllBeta":  true,
			}
		},
	}
}
