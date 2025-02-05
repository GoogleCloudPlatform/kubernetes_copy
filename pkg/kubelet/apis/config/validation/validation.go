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
	"fmt"
	"time"
	"unicode"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	utilvalidation "k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/component-base/featuregate"
	logsapi "k8s.io/component-base/logs/api/v1"
	"k8s.io/component-base/metrics"
	tracingapi "k8s.io/component-base/tracing/api/v1"
	"k8s.io/kubernetes/pkg/features"
	kubeletconfig "k8s.io/kubernetes/pkg/kubelet/apis/config"
	imagepullmanager "k8s.io/kubernetes/pkg/kubelet/images/pullmanager"
	kubetypes "k8s.io/kubernetes/pkg/kubelet/types"
	utilfs "k8s.io/kubernetes/pkg/util/filesystem"
	utiltaints "k8s.io/kubernetes/pkg/util/taints"
	"k8s.io/utils/cpuset"
)

var (
	defaultCFSQuota = metav1.Duration{Duration: 100 * time.Millisecond}
)

// ValidateKubeletConfiguration validates `kc` and returns an error if it is invalid
func ValidateKubeletConfiguration(kc *kubeletconfig.KubeletConfiguration, featureGate featuregate.FeatureGate) error {
	allErrors := []error{}

	// TODO(lauralorenz): Reasses / confirm interpretation of feature gates
	// depending on where we are in merging dynamic KubeletConfiguration when
	// this is called. Here we copy the gates to a local var, and unilaterally
	// merge it with the current gate config to test. See also defaults.go which
	// intersects with this assumption.

	// Make a local copy of the feature gates and combine it with the gates set by this configuration.
	// This allows us to validate the config against the set of gates it will actually run against.
	localFeatureGate := featureGate.DeepCopy()
	if err := localFeatureGate.SetFromMap(kc.FeatureGates); err != nil {
		return err
	}

	if kc.NodeLeaseDurationSeconds <= 0 {
		allErrors = append(allErrors, fmt.Errorf("invalid configuration: nodeLeaseDurationSeconds must be greater than 0"))
	}
	if kc.SystemCgroups != "" && kc.CgroupRoot == "" {
		allErrors = append(allErrors, fmt.Errorf("invalid configuration: systemCgroups (--system-cgroups) was specified and cgroupRoot (--cgroup-root) was not specified"))
	}
	if kc.EventBurst < 0 {
		allErrors = append(allErrors, fmt.Errorf("invalid configuration: eventBurst (--event-burst) %v must not be a negative number", kc.EventBurst))
	}
	if kc.EventRecordQPS < 0 {
		allErrors = append(allErrors, fmt.Errorf("invalid configuration: eventRecordQPS (--event-qps) %v must not be a negative number", kc.EventRecordQPS))
	}
	if kc.HealthzPort != 0 && utilvalidation.IsValidPortNum(int(kc.HealthzPort)) != nil {
		allErrors = append(allErrors, fmt.Errorf("invalid configuration: healthzPort (--healthz-port) %v must be between 1 and 65535, inclusive", kc.HealthzPort))
	}
	if !localFeatureGate.Enabled(features.CPUCFSQuotaPeriod) && kc.CPUCFSQuotaPeriod != defaultCFSQuota {
		allErrors = append(allErrors, fmt.Errorf("invalid configuration: cpuCFSQuotaPeriod (--cpu-cfs-quota-period) %v requires feature gate CustomCPUCFSQuotaPeriod", kc.CPUCFSQuotaPeriod))
	}
	if localFeatureGate.Enabled(features.CPUCFSQuotaPeriod) && utilvalidation.IsInRange(int(kc.CPUCFSQuotaPeriod.Duration), int(1*time.Millisecond), int(time.Second)) != nil {
		allErrors = append(allErrors, fmt.Errorf("invalid configuration: cpuCFSQuotaPeriod (--cpu-cfs-quota-period) %v must be between 1ms and 1sec, inclusive", kc.CPUCFSQuotaPeriod))
	}
	if utilvalidation.IsInRange(int(kc.ImageGCHighThresholdPercent), 0, 100) != nil {
		allErrors = append(allErrors, fmt.Errorf("invalid configuration: imageGCHighThresholdPercent (--image-gc-high-threshold) %v must be between 0 and 100, inclusive", kc.ImageGCHighThresholdPercent))
	}
	if utilvalidation.IsInRange(int(kc.ImageGCLowThresholdPercent), 0, 100) != nil {
		allErrors = append(allErrors, fmt.Errorf("invalid configuration: imageGCLowThresholdPercent (--image-gc-low-threshold) %v must be between 0 and 100, inclusive", kc.ImageGCLowThresholdPercent))
	}
	if kc.ImageGCLowThresholdPercent >= kc.ImageGCHighThresholdPercent {
		allErrors = append(allErrors, fmt.Errorf("invalid configuration: imageGCLowThresholdPercent (--image-gc-low-threshold) %v must be less than imageGCHighThresholdPercent (--image-gc-high-threshold) %v", kc.ImageGCLowThresholdPercent, kc.ImageGCHighThresholdPercent))
	}
	if kc.ImageMaximumGCAge.Duration != 0 && !localFeatureGate.Enabled(features.ImageMaximumGCAge) {
		allErrors = append(allErrors, fmt.Errorf("invalid configuration: ImageMaximumGCAge feature gate is required for Kubelet configuration option imageMaximumGCAge"))
	}
	if kc.ImageMaximumGCAge.Duration < 0 {
		allErrors = append(allErrors, fmt.Errorf("invalid configuration: imageMaximumGCAge %v must not be negative", kc.ImageMaximumGCAge.Duration))
	}
	if kc.ImageMaximumGCAge.Duration > 0 && kc.ImageMaximumGCAge.Duration <= kc.ImageMinimumGCAge.Duration {
		allErrors = append(allErrors, fmt.Errorf("invalid configuration: imageMaximumGCAge %v must be greater than imageMinimumGCAge %v", kc.ImageMaximumGCAge.Duration, kc.ImageMinimumGCAge.Duration))
	}
	if utilvalidation.IsInRange(int(kc.IPTablesDropBit), 0, 31) != nil {
		allErrors = append(allErrors, fmt.Errorf("invalid configuration: iptablesDropBit (--iptables-drop-bit) %v must be between 0 and 31, inclusive", kc.IPTablesDropBit))
	}
	if utilvalidation.IsInRange(int(kc.IPTablesMasqueradeBit), 0, 31) != nil {
		allErrors = append(allErrors, fmt.Errorf("invalid configuration: iptablesMasqueradeBit (--iptables-masquerade-bit) %v must be between 0 and 31, inclusive", kc.IPTablesMasqueradeBit))
	}
	if kc.KubeAPIBurst < 0 {
		allErrors = append(allErrors, fmt.Errorf("invalid configuration: kubeAPIBurst (--kube-api-burst) %v must not be a negative number", kc.KubeAPIBurst))
	}
	if kc.KubeAPIQPS < 0 {
		allErrors = append(allErrors, fmt.Errorf("invalid configuration: kubeAPIQPS (--kube-api-qps) %v must not be a negative number", kc.KubeAPIQPS))
	}
	if kc.NodeStatusMaxImages < -1 {
		allErrors = append(allErrors, fmt.Errorf("invalid configuration: nodeStatusMaxImages (--node-status-max-images) %v must be -1 or greater", kc.NodeStatusMaxImages))
	}
	if kc.MaxOpenFiles < 0 {
		allErrors = append(allErrors, fmt.Errorf("invalid configuration: maxOpenFiles (--max-open-files) %v must not be a negative number", kc.MaxOpenFiles))
	}
	if kc.MaxPods < 0 {
		allErrors = append(allErrors, fmt.Errorf("invalid configuration: maxPods (--max-pods) %v must not be a negative number", kc.MaxPods))
	}
	if utilvalidation.IsInRange(int(kc.OOMScoreAdj), -1000, 1000) != nil {
		allErrors = append(allErrors, fmt.Errorf("invalid configuration: oomScoreAdj (--oom-score-adj) %v must be between -1000 and 1000, inclusive", kc.OOMScoreAdj))
	}
	if kc.PodsPerCore < 0 {
		allErrors = append(allErrors, fmt.Errorf("invalid configuration: podsPerCore (--pods-per-core) %v must not be a negative number", kc.PodsPerCore))
	}
	if utilvalidation.IsValidPortNum(int(kc.Port)) != nil {
		allErrors = append(allErrors, fmt.Errorf("invalid configuration: port (--port) %v must be between 1 and 65535, inclusive", kc.Port))
	}
	if kc.ReadOnlyPort != 0 && utilvalidation.IsValidPortNum(int(kc.ReadOnlyPort)) != nil {
		allErrors = append(allErrors, fmt.Errorf("invalid configuration: readOnlyPort (--read-only-port) %v must be between 0 and 65535, inclusive", kc.ReadOnlyPort))
	}
	if kc.RegistryBurst < 0 {
		allErrors = append(allErrors, fmt.Errorf("invalid configuration: registryBurst (--registry-burst) %v must not be a negative number", kc.RegistryBurst))
	}
	if kc.RegistryPullQPS < 0 {
		allErrors = append(allErrors, fmt.Errorf("invalid configuration: registryPullQPS (--registry-qps) %v must not be a negative number", kc.RegistryPullQPS))
	}
	if kc.MaxParallelImagePulls != nil && *kc.MaxParallelImagePulls < 1 {
		allErrors = append(allErrors, fmt.Errorf("invalid configuration: maxParallelImagePulls %v must be a positive number", *kc.MaxParallelImagePulls))
	}
	if kc.SerializeImagePulls && kc.MaxParallelImagePulls != nil && *kc.MaxParallelImagePulls > 1 {
		allErrors = append(allErrors, fmt.Errorf("invalid configuration: maxParallelImagePulls cannot be larger than 1 unless SerializeImagePulls (--serialize-image-pulls) is set to false"))
	}
	if kc.ServerTLSBootstrap && !localFeatureGate.Enabled(features.RotateKubeletServerCertificate) {
		allErrors = append(allErrors, fmt.Errorf("invalid configuration: serverTLSBootstrap %v requires feature gate RotateKubeletServerCertificate", kc.ServerTLSBootstrap))
	}
	if kc.RunOnce {
		allErrors = append(allErrors, fmt.Errorf("invalid configuration: runOnce (--runOnce) %v, Runonce mode has been deprecated and should not be set", kc.RunOnce))
	}

	for _, nodeTaint := range kc.RegisterWithTaints {
		if err := utiltaints.CheckTaintValidation(nodeTaint); err != nil {
			allErrors = append(allErrors, fmt.Errorf("invalid taint: %v", nodeTaint))
		}
		if nodeTaint.TimeAdded != nil {
			allErrors = append(allErrors, fmt.Errorf("invalid configuration: taint.TimeAdded is not nil"))
		}
	}

	switch kc.TopologyManagerPolicy {
	case kubeletconfig.NoneTopologyManagerPolicy:
	case kubeletconfig.BestEffortTopologyManagerPolicy:
	case kubeletconfig.RestrictedTopologyManagerPolicy:
	case kubeletconfig.SingleNumaNodeTopologyManagerPolicy:
	default:
		allErrors = append(allErrors, fmt.Errorf("invalid configuration: topologyManagerPolicy (--topology-manager-policy) %q must be one of: %q", kc.TopologyManagerPolicy, []string{kubeletconfig.NoneTopologyManagerPolicy, kubeletconfig.BestEffortTopologyManagerPolicy, kubeletconfig.RestrictedTopologyManagerPolicy, kubeletconfig.SingleNumaNodeTopologyManagerPolicy}))
	}

	switch kc.TopologyManagerScope {
	case kubeletconfig.ContainerTopologyManagerScope:
	case kubeletconfig.PodTopologyManagerScope:
	default:
		allErrors = append(allErrors, fmt.Errorf("invalid configuration: topologyManagerScope (--topology-manager-scope) %q must be one of: %q, or %q", kc.TopologyManagerScope, kubeletconfig.ContainerTopologyManagerScope, kubeletconfig.PodTopologyManagerScope))
	}

	if localFeatureGate.Enabled(features.GracefulNodeShutdown) {
		if kc.ShutdownGracePeriodCriticalPods.Duration > kc.ShutdownGracePeriod.Duration {
			allErrors = append(allErrors, fmt.Errorf("invalid configuration: shutdownGracePeriodCriticalPods %v must be <= shutdownGracePeriod %v", kc.ShutdownGracePeriodCriticalPods, kc.ShutdownGracePeriod))
		}
		if kc.ShutdownGracePeriod.Duration < 0 || (kc.ShutdownGracePeriod.Duration > 0 && kc.ShutdownGracePeriod.Duration < time.Second) {
			allErrors = append(allErrors, fmt.Errorf("invalid configuration: shutdownGracePeriod %v must be either zero or otherwise >= 1 sec", kc.ShutdownGracePeriod))
		}
		if kc.ShutdownGracePeriodCriticalPods.Duration < 0 || (kc.ShutdownGracePeriodCriticalPods.Duration > 0 && kc.ShutdownGracePeriodCriticalPods.Duration < time.Second) {
			allErrors = append(allErrors, fmt.Errorf("invalid configuration: shutdownGracePeriodCriticalPods %v must be either zero or otherwise >= 1 sec", kc.ShutdownGracePeriodCriticalPods))
		}
	}
	if (kc.ShutdownGracePeriod.Duration > 0 || kc.ShutdownGracePeriodCriticalPods.Duration > 0) && !localFeatureGate.Enabled(features.GracefulNodeShutdown) {
		allErrors = append(allErrors, fmt.Errorf("invalid configuration: specifying shutdownGracePeriod or shutdownGracePeriodCriticalPods requires feature gate GracefulNodeShutdown"))
	}
	if localFeatureGate.Enabled(features.GracefulNodeShutdownBasedOnPodPriority) {
		if len(kc.ShutdownGracePeriodByPodPriority) != 0 && (kc.ShutdownGracePeriod.Duration > 0 || kc.ShutdownGracePeriodCriticalPods.Duration > 0) {
			allErrors = append(allErrors, fmt.Errorf("invalid configuration: Cannot specify both shutdownGracePeriodByPodPriority and shutdownGracePeriod at the same time"))
		}
	}
	if !localFeatureGate.Enabled(features.GracefulNodeShutdownBasedOnPodPriority) {
		if len(kc.ShutdownGracePeriodByPodPriority) != 0 {
			allErrors = append(allErrors, fmt.Errorf("invalid configuration: Specifying shutdownGracePeriodByPodPriority requires feature gate GracefulNodeShutdownBasedOnPodPriority"))
		}
	}
	if localFeatureGate.Enabled(features.NodeSwap) {
		switch kc.MemorySwap.SwapBehavior {
		case "":
		case kubetypes.NoSwap:
		case kubetypes.LimitedSwap:
		default:
			allErrors = append(allErrors, fmt.Errorf("invalid configuration: memorySwap.swapBehavior %q must be one of: \"\", %q or %q", kc.MemorySwap.SwapBehavior, kubetypes.LimitedSwap, kubetypes.NoSwap))
		}
	}
	if !localFeatureGate.Enabled(features.NodeSwap) && kc.MemorySwap != (kubeletconfig.MemorySwapConfiguration{}) {
		allErrors = append(allErrors, fmt.Errorf("invalid configuration: memorySwap.swapBehavior cannot be set when NodeSwap feature flag is disabled"))
	}

	if localFeatureGate.Enabled(features.KubeletCrashLoopBackOffMax) {
		if kc.CrashLoopBackOff.MaxContainerRestartPeriod == nil {
			allErrors = append(allErrors, fmt.Errorf("invalid configuration: FeatureGate KubeletCrashLoopBackOffMax is enabled, CrashLoopBackOff.MaxContainerRestartPeriod must be set"))
		}
		if kc.CrashLoopBackOff.MaxContainerRestartPeriod != nil && utilvalidation.IsInRange(int(kc.CrashLoopBackOff.MaxContainerRestartPeriod.Duration.Milliseconds()), 1000, 300000) != nil {
			allErrors = append(allErrors, fmt.Errorf("invalid configuration: CrashLoopBackOff.MaxContainerRestartPeriod (got: %v seconds) must be set between 1s and 300s", kc.CrashLoopBackOff.MaxContainerRestartPeriod.Seconds()))
		}
	} else if kc.CrashLoopBackOff.MaxContainerRestartPeriod != nil {
		allErrors = append(allErrors, fmt.Errorf("invalid configuration: FeatureGate KubeletCrashLoopBackOffMax not enabled, CrashLoopBackOff.MaxContainerRestartPeriod must not be set"))
	}

	// Check for mutually exclusive keys before the main validation loop
	reservedKeys := map[string]bool{
		kubetypes.SystemReservedEnforcementKey:             false,
		kubetypes.SystemReservedCompressibleEnforcementKey: false,
		kubetypes.KubeReservedEnforcementKey:               false,
		kubetypes.KubeReservedCompressibleEnforcementKey:   false,
	}

	for _, val := range kc.EnforceNodeAllocatable {
		reservedKeys[val] = true
	}

	if reservedKeys[kubetypes.SystemReservedCompressibleEnforcementKey] && reservedKeys[kubetypes.SystemReservedEnforcementKey] {
		allErrors = append(allErrors, fmt.Errorf("invalid configuration: both %q and %q cannot be specified together in enforceNodeAllocatable (--enforce-node-allocatable)", kubetypes.SystemReservedEnforcementKey, kubetypes.SystemReservedCompressibleEnforcementKey))
	}

	if reservedKeys[kubetypes.KubeReservedCompressibleEnforcementKey] && reservedKeys[kubetypes.KubeReservedEnforcementKey] {
		allErrors = append(allErrors, fmt.Errorf("invalid configuration: both %q and %q cannot be specified together in enforceNodeAllocatable (--enforce-node-allocatable)", kubetypes.KubeReservedEnforcementKey, kubetypes.KubeReservedCompressibleEnforcementKey))
	}

	uniqueEnforcements := sets.Set[string]{}
	for _, val := range kc.EnforceNodeAllocatable {
		if uniqueEnforcements.Has(val) {
			allErrors = append(allErrors, fmt.Errorf("invalid configuration: duplicated enforcements %q in enforceNodeAllocatable (--enforce-node-allocatable)", val))
			continue
		}
		uniqueEnforcements.Insert(val)

		switch val {
		case kubetypes.NodeAllocatableEnforcementKey:
		case kubetypes.SystemReservedEnforcementKey, kubetypes.SystemReservedCompressibleEnforcementKey:
			if kc.SystemReservedCgroup == "" {
				allErrors = append(allErrors, fmt.Errorf("invalid configuration: systemReservedCgroup (--system-reserved-cgroup) must be specified when %q or %q included in enforceNodeAllocatable (--enforce-node-allocatable)", kubetypes.SystemReservedEnforcementKey, kubetypes.SystemReservedCompressibleEnforcementKey))
			}
		case kubetypes.KubeReservedEnforcementKey, kubetypes.KubeReservedCompressibleEnforcementKey:
			if kc.KubeReservedCgroup == "" {
				allErrors = append(allErrors, fmt.Errorf("invalid configuration: kubeReservedCgroup (--kube-reserved-cgroup) must be specified when %q or %q included in enforceNodeAllocatable (--enforce-node-allocatable)", kubetypes.KubeReservedEnforcementKey, kubetypes.KubeReservedCompressibleEnforcementKey))
			}
		case kubetypes.NodeAllocatableNoneKey:
			if len(kc.EnforceNodeAllocatable) > 1 {
				allErrors = append(allErrors, fmt.Errorf("invalid configuration: enforceNodeAllocatable (--enforce-node-allocatable) may not contain additional enforcements when %q is specified", kubetypes.NodeAllocatableNoneKey))
			}
			// skip all further checks when this is explicitly "none"
			continue
		default:
			allErrors = append(allErrors, fmt.Errorf("invalid configuration: option %q specified for enforceNodeAllocatable (--enforce-node-allocatable). Valid options are %q, %q, %q, %q, %q or %q",
				val, kubetypes.NodeAllocatableEnforcementKey, kubetypes.SystemReservedEnforcementKey, kubetypes.SystemReservedCompressibleEnforcementKey,
				kubetypes.KubeReservedEnforcementKey, kubetypes.KubeReservedCompressibleEnforcementKey, kubetypes.NodeAllocatableNoneKey))
			continue
		}

		if !kc.CgroupsPerQOS {
			allErrors = append(allErrors, fmt.Errorf("invalid configuration: cgroupsPerQOS (--cgroups-per-qos) must be set to true when %q contained in enforceNodeAllocatable (--enforce-node-allocatable)", val))
		}
	}
	switch kc.HairpinMode {
	case kubeletconfig.HairpinNone:
	case kubeletconfig.HairpinVeth:
	case kubeletconfig.PromiscuousBridge:
	default:
		allErrors = append(allErrors, fmt.Errorf("invalid configuration: option %q specified for hairpinMode (--hairpin-mode). Valid options are %q, %q or %q",
			kc.HairpinMode, kubeletconfig.HairpinNone, kubeletconfig.HairpinVeth, kubeletconfig.PromiscuousBridge))
	}

	if localFeatureGate.Enabled(features.KubeletEnsureSecretPulledImages) {
		switch kc.ImagePullCredentialsVerificationPolicy {
		case "", "NeverVerify",
			"NeverVerifyPreloadedImages",
			"NeverVerifyAllowlistedImages",
			"AlwaysVerify":
		default:
			allErrors = append(allErrors, fmt.Errorf("invalid configuration: option %q specified for imagePullCredentialsVerificationPolicy. Valid options are %q, %q, %q or %q",
				kc.ImagePullCredentialsVerificationPolicy, "NeverVerify", "NeverVerifyPreloadedImages", "NeverVerifyAllowlistedImages", "AlwaysVerify"))
		}

		if len(kc.PreloadedImagesVerificationAllowlist) > 0 && kc.ImagePullCredentialsVerificationPolicy != "NeverVerifyAllowlistedImages" {
			allErrors = append(allErrors, fmt.Errorf("invalid configuration: can't set `preloadedImagesVerificationAllowlist` if `imagePullCredentialsVertificationPolicy` is not \"NeverVerifyAllowlistedImages\""))
		} else if err := imagepullmanager.ValidateAllowlistImagesPatterns(kc.PreloadedImagesVerificationAllowlist); err != nil {
			allErrors = append(allErrors, fmt.Errorf("invalid configuration: invalid image pattern in `preloadedImagesVerificationAllowlist`: %w", err))
		}
	} else {
		if len(kc.ImagePullCredentialsVerificationPolicy) > 0 {
			allErrors = append(allErrors, fmt.Errorf("invalid configuration: `imagePullCredentialsVerificationPolicy` must not be set if KubeletEnsureSecretPulledImages feature gate is not enabled"))
		}
		if len(kc.PreloadedImagesVerificationAllowlist) > 0 {
			allErrors = append(allErrors, fmt.Errorf("invalid configuration: `preloadedImagesVerificationAllowlist` must not be set if KubeletEnsureSecretPulledImages feature gate is not enabled"))
		}
	}

	if kc.ReservedSystemCPUs != "" {
		// --reserved-cpus does not support --system-reserved-cgroup or --kube-reserved-cgroup
		if kc.SystemReservedCgroup != "" || kc.KubeReservedCgroup != "" {
			allErrors = append(allErrors, fmt.Errorf("invalid configuration: can't use reservedSystemCPUs (--reserved-cpus) with systemReservedCgroup (--system-reserved-cgroup) or kubeReservedCgroup (--kube-reserved-cgroup)"))
		}
		if _, err := cpuset.Parse(kc.ReservedSystemCPUs); err != nil {
			allErrors = append(allErrors, fmt.Errorf("invalid configuration: unable to parse reservedSystemCPUs (--reserved-cpus) %v, error: %w", kc.ReservedSystemCPUs, err))
		}
	}

	allErrors = append(allErrors, validateReservedMemoryConfiguration(kc)...)

	if err := validateKubeletOSConfiguration(kc); err != nil {
		allErrors = append(allErrors, err)
	}
	allErrors = append(allErrors, metrics.ValidateShowHiddenMetricsVersion(kc.ShowHiddenMetricsForVersion)...)

	if errs := logsapi.Validate(&kc.Logging, localFeatureGate, field.NewPath("logging")); len(errs) > 0 {
		allErrors = append(allErrors, errs.ToAggregate().Errors()...)
	}

	if localFeatureGate.Enabled(features.KubeletTracing) {
		if errs := tracingapi.ValidateTracingConfiguration(kc.Tracing, localFeatureGate, field.NewPath("tracing")); len(errs) > 0 {
			allErrors = append(allErrors, errs.ToAggregate().Errors()...)
		}
	} else if kc.Tracing != nil {
		allErrors = append(allErrors, fmt.Errorf("invalid configuration: tracing should not be configured if KubeletTracing feature flag is disabled."))
	}

	if localFeatureGate.Enabled(features.MemoryQoS) && kc.MemoryThrottlingFactor == nil {
		allErrors = append(allErrors, fmt.Errorf("invalid configuration: memoryThrottlingFactor is required when MemoryQoS feature flag is enabled"))
	}
	if kc.MemoryThrottlingFactor != nil && (*kc.MemoryThrottlingFactor <= 0 || *kc.MemoryThrottlingFactor > 1.0) {
		allErrors = append(allErrors, fmt.Errorf("invalid configuration: memoryThrottlingFactor %v must be greater than 0 and less than or equal to 1.0", *kc.MemoryThrottlingFactor))
	}

	if kc.ContainerRuntimeEndpoint == "" {
		allErrors = append(allErrors, fmt.Errorf("invalid configuration: the containerRuntimeEndpoint was not specified or empty"))
	}

	if kc.EnableSystemLogQuery && !localFeatureGate.Enabled(features.NodeLogQuery) {
		allErrors = append(allErrors, fmt.Errorf("invalid configuration: NodeLogQuery feature gate is required for enableSystemLogHandler"))
	}
	if kc.EnableSystemLogQuery && !kc.EnableSystemLogHandler {
		allErrors = append(allErrors,
			fmt.Errorf("invalid configuration: enableSystemLogHandler is required for enableSystemLogQuery"))
	}

	if kc.ContainerLogMaxWorkers < 1 {
		allErrors = append(allErrors, fmt.Errorf("invalid configuration: containerLogMaxWorkers must be greater than or equal to 1"))
	}

	if kc.ContainerLogMonitorInterval.Duration.Seconds() < 3 {
		allErrors = append(allErrors, fmt.Errorf("invalid configuration: containerLogMonitorInterval must be a positive time duration greater than or equal to 3s"))
	}

	if kc.ContainerLogMaxFiles <= 1 {
		allErrors = append(allErrors, fmt.Errorf("invalid configuration: containerLogMaxFiles must be greater than 1"))
	}

	if kc.PodLogsDir == "" {
		allErrors = append(allErrors, fmt.Errorf("invalid configuration: podLogsDir was not specified"))
	}

	if !utilfs.IsAbs(kc.PodLogsDir) {
		allErrors = append(allErrors, fmt.Errorf("invalid configuration: pod logs path %q must be absolute path", kc.PodLogsDir))
	}

	if !utilfs.IsPathClean(kc.PodLogsDir) {
		allErrors = append(allErrors, fmt.Errorf("invalid configuration: pod logs path %q must be normalized", kc.PodLogsDir))
	}

	// Since pod logs path is used in metrics, make sure it contains only ASCII characters.
	for _, c := range kc.PodLogsDir {
		if c > unicode.MaxASCII {
			allErrors = append(allErrors, fmt.Errorf("invalid configuration: pod logs path %q mut contains ASCII characters only", kc.PodLogsDir))
			break
		}
	}

	return utilerrors.NewAggregate(allErrors)
}
