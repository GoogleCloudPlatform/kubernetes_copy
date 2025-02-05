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

package e2enode

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	schedulingv1 "k8s.io/api/scheduling/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	kubeletstatsv1alpha1 "k8s.io/kubelet/pkg/apis/stats/v1alpha1"
	kubeletconfig "k8s.io/kubernetes/pkg/kubelet/apis/config"
	"k8s.io/kubernetes/pkg/kubelet/eviction"
	evictionapi "k8s.io/kubernetes/pkg/kubelet/eviction/api"
	kubeletmetrics "k8s.io/kubernetes/pkg/kubelet/metrics"
	kubetypes "k8s.io/kubernetes/pkg/kubelet/types"
	"k8s.io/kubernetes/test/e2e/feature"
	"k8s.io/kubernetes/test/e2e/framework"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
	testutils "k8s.io/kubernetes/test/utils"
	imageutils "k8s.io/kubernetes/test/utils/image"
	admissionapi "k8s.io/pod-security-admission/api"
	"k8s.io/utils/ptr"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
)

// Eviction Policy is described here:
// https://github.com/kubernetes/design-proposals-archive/blob/main/node/kubelet-eviction.md

const (
	postTestConditionMonitoringPeriod = 1 * time.Minute
	evictionPollInterval              = 2 * time.Second
	pressureDisappearTimeout          = 10 * time.Minute
	// pressure conditions often surface after evictions because the kubelet only updates
	// node conditions periodically.
	// we wait this period after evictions to make sure that we wait out this delay
	pressureDelay     = 20 * time.Second
	testContextFmt    = "when we run containers that should cause %s"
	noPressure        = v1.NodeConditionType("NoPressure")
	lotsOfDisk        = 10240      // 10 Gb in Mb
	lotsOfFiles       = 1000000000 // 1 billion
	resourceInodes    = v1.ResourceName("inodes")
	noStarvedResource = v1.ResourceName("none")
)

// InodeEviction tests that the node responds to node disk pressure by evicting only responsible pods.
// Node disk pressure is induced by consuming all inodes on the node.
var _ = SIGDescribe("InodeEviction", framework.WithSlow(), framework.WithSerial(), framework.WithDisruptive(), feature.Eviction, func() {
	f := framework.NewDefaultFramework("inode-eviction-test")
	f.NamespacePodSecurityLevel = admissionapi.LevelPrivileged
	expectedNodeCondition := v1.NodeDiskPressure
	expectedStarvedResource := resourceInodes
	pressureTimeout := 15 * time.Minute
	inodesConsumed := uint64(200000)
	ginkgo.Context(fmt.Sprintf(testContextFmt, expectedNodeCondition), func() {
		tempSetCurrentKubeletConfig(f, func(ctx context.Context, initialConfig *kubeletconfig.KubeletConfiguration) {
			// Set the eviction threshold to inodesFree - inodesConsumed, so that using inodesConsumed causes an eviction.
			summary := eventuallyGetSummary(ctx)
			inodesFree := *summary.Node.Fs.InodesFree
			if inodesFree <= inodesConsumed {
				e2eskipper.Skipf("Too few inodes free on the host for the InodeEviction test to run")
			}
			initialConfig.EvictionHard = map[string]string{string(evictionapi.SignalNodeFsInodesFree): fmt.Sprintf("%d", inodesFree-inodesConsumed)}
			initialConfig.EvictionMinimumReclaim = map[string]string{}
		})
		runEvictionTest(f, pressureTimeout, expectedNodeCondition, expectedStarvedResource, logInodeMetrics, []podEvictSpec{
			{
				evictionPriority: 1,
				// TODO(#127864): Container runtime may not immediate free up the resources after the pod eviction,
				// causing the test to fail. We provision an emptyDir volume to avoid relying on the runtime behavior.
				pod: inodeConsumingPod("volume-inode-hog", lotsOfFiles, &v1.VolumeSource{EmptyDir: &v1.EmptyDirVolumeSource{}}),
			},
			{
				evictionPriority: 0,
				pod:              innocentPod(),
			},
		})
	})
})

// ImageGCNoEviction tests that the node does not evict pods when inodes are consumed by images
// Disk pressure is induced by pulling large images
var _ = SIGDescribe("ImageGCNoEviction", framework.WithSlow(), framework.WithSerial(), framework.WithDisruptive(), feature.Eviction, func() {
	f := framework.NewDefaultFramework("image-gc-eviction-test")
	f.NamespacePodSecurityLevel = admissionapi.LevelPrivileged
	pressureTimeout := 10 * time.Minute
	expectedNodeCondition := v1.NodeDiskPressure
	expectedStarvedResource := resourceInodes
	inodesConsumed := uint64(100000)
	ginkgo.Context(fmt.Sprintf(testContextFmt, expectedNodeCondition), func() {
		tempSetCurrentKubeletConfig(f, func(ctx context.Context, initialConfig *kubeletconfig.KubeletConfiguration) {
			// Set the eviction threshold to inodesFree - inodesConsumed, so that using inodesConsumed causes an eviction.
			summary := eventuallyGetSummary(ctx)
			inodesFree := *summary.Node.Fs.InodesFree
			if inodesFree <= inodesConsumed {
				e2eskipper.Skipf("Too few inodes free on the host for the InodeEviction test to run")
			}
			initialConfig.EvictionHard = map[string]string{string(evictionapi.SignalNodeFsInodesFree): fmt.Sprintf("%d", inodesFree-inodesConsumed)}
			initialConfig.EvictionMinimumReclaim = map[string]string{}
		})
		// Consume enough inodes to induce disk pressure,
		// but expect that image garbage collection can reduce it enough to avoid an eviction
		runEvictionTest(f, pressureTimeout, expectedNodeCondition, expectedStarvedResource, logDiskMetrics, []podEvictSpec{
			{
				evictionPriority: 0,
				pod:              inodeConsumingPod("container-inode", 110000, nil),
			},
		})
	})
})

// MemoryAllocatableEviction tests that the node responds to node memory pressure by evicting only responsible pods.
// Node memory pressure is only encountered because we reserve the majority of the node's capacity via kube-reserved.
var _ = SIGDescribe("MemoryAllocatableEviction", framework.WithSlow(), framework.WithSerial(), framework.WithDisruptive(), feature.Eviction, func() {
	f := framework.NewDefaultFramework("memory-allocatable-eviction-test")
	f.NamespacePodSecurityLevel = admissionapi.LevelPrivileged
	expectedNodeCondition := v1.NodeMemoryPressure
	expectedStarvedResource := v1.ResourceMemory
	pressureTimeout := 10 * time.Minute
	ginkgo.Context(fmt.Sprintf(testContextFmt, expectedNodeCondition), func() {
		tempSetCurrentKubeletConfig(f, func(ctx context.Context, initialConfig *kubeletconfig.KubeletConfiguration) {
			// Set large system and kube reserved values to trigger allocatable thresholds far before hard eviction thresholds.
			kubeReserved := getNodeCPUAndMemoryCapacity(ctx, f)[v1.ResourceMemory]
			// The default hard eviction threshold is 250Mb, so Allocatable = Capacity - Reserved - 250Mb
			// We want Allocatable = 50Mb, so set Reserved = Capacity - Allocatable - 250Mb = Capacity - 300Mb
			kubeReserved.Sub(resource.MustParse("300Mi"))
			initialConfig.KubeReserved = map[string]string{
				string(v1.ResourceMemory): kubeReserved.String(),
			}
			initialConfig.EnforceNodeAllocatable = []string{kubetypes.NodeAllocatableEnforcementKey}
			initialConfig.CgroupsPerQOS = true
		})
		runEvictionTest(f, pressureTimeout, expectedNodeCondition, expectedStarvedResource, logMemoryMetrics, []podEvictSpec{
			{
				evictionPriority: 1,
				pod:              getMemhogPod("memory-hog-pod", "memory-hog", v1.ResourceRequirements{}),
			},
			{
				evictionPriority: 0,
				pod:              innocentPod(),
			},
		})
	})
})

// LocalStorageEviction tests that the node responds to node disk pressure by evicting only responsible pods
// Disk pressure is induced by running pods which consume disk space.
var _ = SIGDescribe("LocalStorageEviction", framework.WithSlow(), framework.WithSerial(), framework.WithDisruptive(), feature.Eviction, func() {
	f := framework.NewDefaultFramework("localstorage-eviction-test")
	f.NamespacePodSecurityLevel = admissionapi.LevelPrivileged
	pressureTimeout := 15 * time.Minute
	expectedNodeCondition := v1.NodeDiskPressure
	expectedStarvedResource := v1.ResourceEphemeralStorage
	ginkgo.Context(fmt.Sprintf(testContextFmt, expectedNodeCondition), func() {

		tempSetCurrentKubeletConfig(f, func(ctx context.Context, initialConfig *kubeletconfig.KubeletConfiguration) {
			summary := eventuallyGetSummary(ctx)

			diskConsumedByTest := resource.MustParse("4Gi")
			availableBytesOnSystem := *(summary.Node.Fs.AvailableBytes)
			evictionThreshold := strconv.FormatUint(availableBytesOnSystem-uint64(diskConsumedByTest.Value()), 10)

			if availableBytesOnSystem <= uint64(diskConsumedByTest.Value()) {
				e2eskipper.Skipf("Too little disk free on the host for the LocalStorageEviction test to run")
			}

			initialConfig.EvictionHard = map[string]string{string(evictionapi.SignalNodeFsAvailable): evictionThreshold}
			initialConfig.EvictionMinimumReclaim = map[string]string{}
		})

		runEvictionTest(f, pressureTimeout, expectedNodeCondition, expectedStarvedResource, logDiskMetrics, []podEvictSpec{
			{
				evictionPriority: 1,
				// TODO(#127864): Container runtime may not immediate free up the resources after the pod eviction,
				// causing the test to fail. We provision an emptyDir volume to avoid relying on the runtime behavior.
				pod: diskConsumingPod("container-disk-hog", lotsOfDisk, &v1.VolumeSource{EmptyDir: &v1.EmptyDirVolumeSource{}}, v1.ResourceRequirements{}),
			},
			{
				evictionPriority: 0,
				pod:              innocentPod(),
			},
		})
	})
})

// LocalStorageEviction tests that the node responds to node disk pressure by evicting only responsible pods
// Disk pressure is induced by running pods which consume disk space, which exceed the soft eviction threshold.
// Note: This test's purpose is to test Soft Evictions.  Local storage was chosen since it is the least costly to run.
var _ = SIGDescribe("LocalStorageSoftEviction", framework.WithSlow(), framework.WithSerial(), framework.WithDisruptive(), feature.Eviction, func() {
	f := framework.NewDefaultFramework("localstorage-eviction-test")
	f.NamespacePodSecurityLevel = admissionapi.LevelPrivileged
	pressureTimeout := 10 * time.Minute
	expectedNodeCondition := v1.NodeDiskPressure
	expectedStarvedResource := v1.ResourceEphemeralStorage
	ginkgo.Context(fmt.Sprintf(testContextFmt, expectedNodeCondition), func() {
		tempSetCurrentKubeletConfig(f, func(ctx context.Context, initialConfig *kubeletconfig.KubeletConfiguration) {
			diskConsumed := resource.MustParse("4Gi")
			summary := eventuallyGetSummary(ctx)
			availableBytes := *(summary.Node.Fs.AvailableBytes)
			if availableBytes <= uint64(diskConsumed.Value()) {
				e2eskipper.Skipf("Too little disk free on the host for the LocalStorageSoftEviction test to run")
			}
			initialConfig.EvictionSoft = map[string]string{string(evictionapi.SignalNodeFsAvailable): fmt.Sprintf("%d", availableBytes-uint64(diskConsumed.Value()))}
			initialConfig.EvictionSoftGracePeriod = map[string]string{string(evictionapi.SignalNodeFsAvailable): "1m"}
			// Defer to the pod default grace period
			initialConfig.EvictionMaxPodGracePeriod = 30
			initialConfig.EvictionMinimumReclaim = map[string]string{}
			// Ensure that pods are not evicted because of the eviction-hard threshold
			// setting a threshold to 0% disables; non-empty map overrides default value (necessary due to omitempty)
			initialConfig.EvictionHard = map[string]string{string(evictionapi.SignalMemoryAvailable): "0%"}
		})
		runEvictionTest(f, pressureTimeout, expectedNodeCondition, expectedStarvedResource, logDiskMetrics, []podEvictSpec{
			{
				evictionPriority: 1,
				// TODO(#127864): Container runtime may not immediate free up the resources after the pod eviction,
				// causing the test to fail. We provision an emptyDir volume to avoid relying on the runtime behavior.
				pod: diskConsumingPod("container-disk-hog", lotsOfDisk, &v1.VolumeSource{EmptyDir: &v1.EmptyDirVolumeSource{}}, v1.ResourceRequirements{}),
			},
			{
				evictionPriority: 0,
				pod:              innocentPod(),
			},
		})
	})
})

var _ = SIGDescribe("LocalStorageSoftEvictionNotOverwriteTerminationGracePeriodSeconds", framework.WithSlow(), framework.WithSerial(), framework.WithDisruptive(), feature.Eviction, func() {
	f := framework.NewDefaultFramework("localstorage-eviction-test")
	f.NamespacePodSecurityLevel = admissionapi.LevelPrivileged
	pressureTimeout := 10 * time.Minute
	expectedNodeCondition := v1.NodeDiskPressure
	expectedStarvedResource := v1.ResourceEphemeralStorage

	evictionMaxPodGracePeriod := 30
	evictionSoftGracePeriod := 30
	ginkgo.Context(fmt.Sprintf(testContextFmt, expectedNodeCondition), func() {
		tempSetCurrentKubeletConfig(f, func(ctx context.Context, initialConfig *kubeletconfig.KubeletConfiguration) {
			diskConsumed := resource.MustParse("4Gi")
			summary := eventuallyGetSummary(ctx)
			availableBytes := *(summary.Node.Fs.AvailableBytes)
			if availableBytes <= uint64(diskConsumed.Value()) {
				e2eskipper.Skipf("Too little disk free on the host for the LocalStorageSoftEviction test to run")
			}
			initialConfig.EvictionSoft = map[string]string{string(evictionapi.SignalNodeFsAvailable): fmt.Sprintf("%d", availableBytes-uint64(diskConsumed.Value()))}
			initialConfig.EvictionSoftGracePeriod = map[string]string{string(evictionapi.SignalNodeFsAvailable): "30s"}
			// Defer to the pod default grace period
			initialConfig.EvictionMaxPodGracePeriod = int32(evictionMaxPodGracePeriod)
			initialConfig.EvictionMinimumReclaim = map[string]string{}
			// Ensure that pods are not evicted because of the eviction-hard threshold
			// setting a threshold to 0% disables; non-empty map overrides default value (necessary due to omitempty)
			initialConfig.EvictionHard = map[string]string{string(evictionapi.SignalMemoryAvailable): "0%"}
		})

		runEvictionTest(f, pressureTimeout, expectedNodeCondition, expectedStarvedResource, logDiskMetrics, []podEvictSpec{
			{
				evictionMaxPodGracePeriod: evictionSoftGracePeriod,
				evictionSoftGracePeriod:   evictionMaxPodGracePeriod,
				evictionPriority:          1,
				pod:                       diskConsumingPod("container-disk-hog", lotsOfDisk, nil, v1.ResourceRequirements{}),
			},
		})
	})
})

// LocalStorageCapacityIsolationEviction tests that container and volume local storage limits are enforced through evictions
var _ = SIGDescribe("LocalStorageCapacityIsolationEviction", framework.WithSlow(), framework.WithSerial(), framework.WithDisruptive(), feature.LocalStorageCapacityIsolationQuota, feature.Eviction, func() {
	f := framework.NewDefaultFramework("localstorage-eviction-test")
	f.NamespacePodSecurityLevel = admissionapi.LevelPrivileged
	evictionTestTimeout := 10 * time.Minute
	ginkgo.Context(fmt.Sprintf(testContextFmt, "evictions due to pod local storage violations"), func() {
		tempSetCurrentKubeletConfig(f, func(ctx context.Context, initialConfig *kubeletconfig.KubeletConfiguration) {
			// setting a threshold to 0% disables; non-empty map overrides default value (necessary due to omitempty)
			initialConfig.EvictionHard = map[string]string{string(evictionapi.SignalMemoryAvailable): "0%"}
		})
		sizeLimit := resource.MustParse("100Mi")
		useOverLimit := 101 /* Mb */
		useUnderLimit := 99 /* Mb */
		containerLimit := v1.ResourceList{v1.ResourceEphemeralStorage: sizeLimit}

		runEvictionTest(f, evictionTestTimeout, noPressure, noStarvedResource, logDiskMetrics, []podEvictSpec{
			{
				evictionPriority: 1, // This pod should be evicted because emptyDir (default storage type) usage violation
				pod: diskConsumingPod("emptydir-disk-sizelimit", useOverLimit, &v1.VolumeSource{
					EmptyDir: &v1.EmptyDirVolumeSource{SizeLimit: &sizeLimit},
				}, v1.ResourceRequirements{}),
			},
			{
				evictionPriority: 1, // This pod should cross the container limit by writing to its writable layer.
				pod:              diskConsumingPod("container-disk-limit", useOverLimit, nil, v1.ResourceRequirements{Limits: containerLimit}),
			},
			{
				evictionPriority: 1, // This pod should hit the container limit by writing to an emptydir
				pod: diskConsumingPod("container-emptydir-disk-limit", useOverLimit, &v1.VolumeSource{EmptyDir: &v1.EmptyDirVolumeSource{}},
					v1.ResourceRequirements{Limits: containerLimit}),
			},
			{
				evictionPriority: 0, // This pod should not be evicted because MemoryBackedVolumes cannot use more space than is allocated to them since SizeMemoryBackedVolumes was enabled
				pod: diskConsumingPod("emptydir-memory-sizelimit", useOverLimit, &v1.VolumeSource{
					EmptyDir: &v1.EmptyDirVolumeSource{Medium: "Memory", SizeLimit: &sizeLimit},
				}, v1.ResourceRequirements{}),
			},
			{
				evictionPriority: 0, // This pod should not be evicted because it uses less than its limit
				pod: diskConsumingPod("emptydir-disk-below-sizelimit", useUnderLimit, &v1.VolumeSource{
					EmptyDir: &v1.EmptyDirVolumeSource{SizeLimit: &sizeLimit},
				}, v1.ResourceRequirements{}),
			},
			{
				evictionPriority: 0, // This pod should not be evicted because it uses less than its limit
				pod:              diskConsumingPod("container-disk-below-sizelimit", useUnderLimit, nil, v1.ResourceRequirements{Limits: containerLimit}),
			},
		})
	})
})

// PriorityMemoryEvictionOrdering tests that the node responds to node memory pressure by evicting pods.
// This test tests that the guaranteed pod is never evicted, and that the lower-priority pod is evicted before
// the higher priority pod.
var _ = SIGDescribe("PriorityMemoryEvictionOrdering", framework.WithSlow(), framework.WithSerial(), framework.WithDisruptive(), feature.Eviction, func() {
	f := framework.NewDefaultFramework("priority-memory-eviction-ordering-test")
	f.NamespacePodSecurityLevel = admissionapi.LevelPrivileged
	expectedNodeCondition := v1.NodeMemoryPressure
	expectedStarvedResource := v1.ResourceMemory
	pressureTimeout := 10 * time.Minute

	highPriorityClassName := f.BaseName + "-high-priority"
	highPriority := int32(999999999)

	ginkgo.Context(fmt.Sprintf(testContextFmt, expectedNodeCondition), func() {
		tempSetCurrentKubeletConfig(f, func(ctx context.Context, initialConfig *kubeletconfig.KubeletConfiguration) {
			memoryConsumed := resource.MustParse("600Mi")
			summary := eventuallyGetSummary(ctx)
			availableBytes := *(summary.Node.Memory.AvailableBytes)
			if availableBytes <= uint64(memoryConsumed.Value()) {
				e2eskipper.Skipf("Too little memory free on the host for the PriorityMemoryEvictionOrdering test to run")
			}
			initialConfig.EvictionHard = map[string]string{string(evictionapi.SignalMemoryAvailable): fmt.Sprintf("%d", availableBytes-uint64(memoryConsumed.Value()))}
			initialConfig.EvictionMinimumReclaim = map[string]string{}
		})
		ginkgo.BeforeEach(func(ctx context.Context) {
			_, err := f.ClientSet.SchedulingV1().PriorityClasses().Create(ctx, &schedulingv1.PriorityClass{ObjectMeta: metav1.ObjectMeta{Name: highPriorityClassName}, Value: highPriority}, metav1.CreateOptions{})
			if err != nil && !apierrors.IsAlreadyExists(err) {
				framework.ExpectNoError(err, "failed to create priority class")
			}
		})
		ginkgo.AfterEach(func(ctx context.Context) {
			err := f.ClientSet.SchedulingV1().PriorityClasses().Delete(ctx, highPriorityClassName, metav1.DeleteOptions{})
			framework.ExpectNoError(err)
		})
		specs := []podEvictSpec{
			{
				evictionPriority: 2,
				pod:              getMemhogPod("memory-hog-pod", "memory-hog", v1.ResourceRequirements{}),
			},
			{
				evictionPriority: 1,
				pod:              getMemhogPod("high-priority-memory-hog-pod", "high-priority-memory-hog", v1.ResourceRequirements{}),
			},
			{
				evictionPriority: 0,
				pod: getMemhogPod("guaranteed-pod", "guaranteed-pod", v1.ResourceRequirements{
					Requests: v1.ResourceList{
						v1.ResourceMemory: resource.MustParse("300Mi"),
					},
					Limits: v1.ResourceList{
						v1.ResourceMemory: resource.MustParse("300Mi"),
					},
				}),
			},
		}
		specs[1].pod.Spec.PriorityClassName = highPriorityClassName
		runEvictionTest(f, pressureTimeout, expectedNodeCondition, expectedStarvedResource, logMemoryMetrics, specs)
	})
})

// PriorityLocalStorageEvictionOrdering tests that the node responds to node disk pressure by evicting pods.
// This test tests that the guaranteed pod is never evicted, and that the lower-priority pod is evicted before
// the higher priority pod.
var _ = SIGDescribe("PriorityLocalStorageEvictionOrdering", framework.WithSlow(), framework.WithSerial(), framework.WithDisruptive(), feature.Eviction, func() {
	f := framework.NewDefaultFramework("priority-disk-eviction-ordering-test")
	f.NamespacePodSecurityLevel = admissionapi.LevelPrivileged
	expectedNodeCondition := v1.NodeDiskPressure
	expectedStarvedResource := v1.ResourceEphemeralStorage
	pressureTimeout := 15 * time.Minute

	highPriorityClassName := f.BaseName + "-high-priority"
	highPriority := int32(999999999)

	ginkgo.Context(fmt.Sprintf(testContextFmt, expectedNodeCondition), func() {
		tempSetCurrentKubeletConfig(f, func(ctx context.Context, initialConfig *kubeletconfig.KubeletConfiguration) {
			diskConsumed := resource.MustParse("4Gi")
			summary := eventuallyGetSummary(ctx)
			availableBytes := *(summary.Node.Fs.AvailableBytes)
			if availableBytes <= uint64(diskConsumed.Value()) {
				e2eskipper.Skipf("Too little disk free on the host for the PriorityLocalStorageEvictionOrdering test to run")
			}
			initialConfig.EvictionHard = map[string]string{string(evictionapi.SignalNodeFsAvailable): fmt.Sprintf("%d", availableBytes-uint64(diskConsumed.Value()))}
			initialConfig.EvictionMinimumReclaim = map[string]string{}
		})
		ginkgo.BeforeEach(func(ctx context.Context) {
			_, err := f.ClientSet.SchedulingV1().PriorityClasses().Create(ctx, &schedulingv1.PriorityClass{ObjectMeta: metav1.ObjectMeta{Name: highPriorityClassName}, Value: highPriority}, metav1.CreateOptions{})
			if err != nil && !apierrors.IsAlreadyExists(err) {
				framework.ExpectNoError(err, "failed to create priority class")
			}
		})
		ginkgo.AfterEach(func(ctx context.Context) {
			err := f.ClientSet.SchedulingV1().PriorityClasses().Delete(ctx, highPriorityClassName, metav1.DeleteOptions{})
			framework.ExpectNoError(err)
		})
		specs := []podEvictSpec{
			{
				evictionPriority: 2,
				// TODO(#127864): Container runtime may not immediate free up the resources after the pod eviction,
				// causing the test to fail. We provision an emptyDir volume to avoid relying on the runtime behavior.
				pod: diskConsumingPod("best-effort-disk", lotsOfDisk, &v1.VolumeSource{EmptyDir: &v1.EmptyDirVolumeSource{}}, v1.ResourceRequirements{}),
			},
			{
				evictionPriority: 1,
				// TODO(#127864): Container runtime may not immediate free up the resources after the pod eviction,
				// causing the test to fail. We provision an emptyDir volume to avoid relying on the runtime behavior.
				pod: diskConsumingPod("high-priority-disk", lotsOfDisk, &v1.VolumeSource{EmptyDir: &v1.EmptyDirVolumeSource{}}, v1.ResourceRequirements{}),
			},
			{
				evictionPriority: 0,
				// Only require 99% accuracy (297/300 Mb) because on some OS distributions, the file itself (excluding contents), consumes disk space.
				pod: diskConsumingPod("guaranteed-disk", 297 /* Mb */, &v1.VolumeSource{EmptyDir: &v1.EmptyDirVolumeSource{}}, v1.ResourceRequirements{
					Requests: v1.ResourceList{
						v1.ResourceEphemeralStorage: resource.MustParse("300Mi"),
					},
					Limits: v1.ResourceList{
						v1.ResourceEphemeralStorage: resource.MustParse("300Mi"),
					},
				}),
			},
		}
		specs[1].pod.Spec.PriorityClassName = highPriorityClassName
		runEvictionTest(f, pressureTimeout, expectedNodeCondition, expectedStarvedResource, logDiskMetrics, specs)
	})
})

// PriorityPidEvictionOrdering tests that the node emits pid pressure in response to a fork bomb, and evicts pods by priority
var _ = SIGDescribe("PriorityPidEvictionOrdering", framework.WithSlow(), framework.WithSerial(), framework.WithDisruptive(), feature.Eviction, func() {
	f := framework.NewDefaultFramework("pidpressure-eviction-test")
	f.NamespacePodSecurityLevel = admissionapi.LevelPrivileged
	pressureTimeout := 10 * time.Minute
	expectedNodeCondition := v1.NodePIDPressure
	expectedStarvedResource := noStarvedResource

	highPriorityClassName := f.BaseName + "-high-priority"
	highPriority := int32(999999999)
	processes := 30000

	// if criStats is true, PodAndContainerStatsFromCRI will use data from cri instead of cadvisor for kubelet to get pid count of pods
	for _, criStats := range []bool{true, false} {
		ginkgo.Context(fmt.Sprintf("when we run containers with PodAndContainerStatsFromCRI=%v that should cause %s", criStats, expectedNodeCondition), func() {
			tempSetCurrentKubeletConfig(f, func(ctx context.Context, initialConfig *kubeletconfig.KubeletConfiguration) {
				pidsConsumed := int64(10000)
				summary := eventuallyGetSummary(ctx)
				availablePids := *(summary.Node.Rlimit.MaxPID) - *(summary.Node.Rlimit.NumOfRunningProcesses)
				initialConfig.EvictionHard = map[string]string{string(evictionapi.SignalPIDAvailable): fmt.Sprintf("%d", availablePids-pidsConsumed)}
				initialConfig.EvictionMinimumReclaim = map[string]string{}
				if initialConfig.FeatureGates == nil {
					initialConfig.FeatureGates = make(map[string]bool)
				}
				if criStats {
					initialConfig.FeatureGates["PodAndContainerStatsFromCRI"] = true
				}

			})
			ginkgo.BeforeEach(func(ctx context.Context) {
				_, err := f.ClientSet.SchedulingV1().PriorityClasses().Create(ctx, &schedulingv1.PriorityClass{ObjectMeta: metav1.ObjectMeta{Name: highPriorityClassName}, Value: highPriority}, metav1.CreateOptions{})
				if err != nil && !apierrors.IsAlreadyExists(err) {
					framework.ExpectNoError(err, "failed to create priority class")
				}
			})
			ginkgo.AfterEach(func(ctx context.Context) {
				err := f.ClientSet.SchedulingV1().PriorityClasses().Delete(ctx, highPriorityClassName, metav1.DeleteOptions{})
				framework.ExpectNoError(err)
			})
			specs := []podEvictSpec{
				{
					evictionPriority: 2,
					pod:              pidConsumingPod("fork-bomb-container-with-low-priority", processes),
				},
				{
					evictionPriority: 0,
					pod:              innocentPod(),
				},
				{
					evictionPriority: 1,
					pod:              pidConsumingPod("fork-bomb-container-with-high-priority", processes),
				},
			}
			specs[1].pod.Spec.PriorityClassName = highPriorityClassName
			specs[2].pod.Spec.PriorityClassName = highPriorityClassName
			runEvictionTest(f, pressureTimeout, expectedNodeCondition, expectedStarvedResource, logPidMetrics, specs)
		})
	}

	f.Context(fmt.Sprintf(testContextFmt, expectedNodeCondition)+"; baseline scenario to verify DisruptionTarget is added", func() {
		tempSetCurrentKubeletConfig(f, func(ctx context.Context, initialConfig *kubeletconfig.KubeletConfiguration) {
			pidsConsumed := int64(10000)
			summary := eventuallyGetSummary(ctx)
			availablePids := *(summary.Node.Rlimit.MaxPID) - *(summary.Node.Rlimit.NumOfRunningProcesses)
			initialConfig.EvictionHard = map[string]string{string(evictionapi.SignalPIDAvailable): fmt.Sprintf("%d", availablePids-pidsConsumed)}
			initialConfig.EvictionMinimumReclaim = map[string]string{}
		})
		specs := []podEvictSpec{
			{
				evictionPriority:           1,
				pod:                        pidConsumingPod("fork-bomb-container", processes),
				wantPodDisruptionCondition: ptr.To(v1.DisruptionTarget),
			},
		}
		runEvictionTest(f, pressureTimeout, expectedNodeCondition, expectedStarvedResource, logPidMetrics, specs)
	})
})

// Struct used by runEvictionTest that specifies the pod, and when that pod should be evicted, relative to other pods
type podEvictSpec struct {
	// P0 should never be evicted, P1 shouldn't evict before P2, etc.
	// If two are ranked at P1, either is permitted to fail before the other.
	// The test ends when all pods other than p0 have been evicted
	evictionPriority           int
	pod                        *v1.Pod
	wantPodDisruptionCondition *v1.PodConditionType

	evictionMaxPodGracePeriod int
	evictionSoftGracePeriod   int
}

// runEvictionTest sets up a testing environment given the provided pods, and checks a few things:
//
//	It ensures that the desired expectedNodeCondition is actually triggered.
//	It ensures that evictionPriority 0 pods are not evicted
//	It ensures that lower evictionPriority pods are always evicted before higher evictionPriority pods (2 evicted before 1, etc.)
//	It ensures that all pods with non-zero evictionPriority are eventually evicted.
//
// runEvictionTest then cleans up the testing environment by deleting provided pods, and ensures that expectedNodeCondition no longer exists
func runEvictionTest(f *framework.Framework, pressureTimeout time.Duration, expectedNodeCondition v1.NodeConditionType, expectedStarvedResource v1.ResourceName, logFunc func(ctx context.Context), testSpecs []podEvictSpec) {
	// Place the remainder of the test within a context so that the kubelet config is set before and after the test.
	ginkgo.Context("", func() {
		ginkgo.BeforeEach(func(ctx context.Context) {
			// reduce memory usage in the allocatable cgroup to ensure we do not have MemoryPressure
			reduceAllocatableMemoryUsageIfCgroupv1()
			// Nodes do not immediately report local storage capacity
			// Sleep so that pods requesting local storage do not fail to schedule
			time.Sleep(30 * time.Second)
			// Check for Pressure
			ginkgo.By("make sure node has no pressure before starting")
			gomega.Eventually(ctx, func(ctx context.Context) error {
				if expectedNodeCondition == noPressure || !hasNodeCondition(ctx, f, expectedNodeCondition) {
					return nil
				}
				return fmt.Errorf("NodeCondition: %s encountered", expectedNodeCondition)
			}, pressureDisappearTimeout, evictionPollInterval).Should(gomega.BeNil())
			gomega.Expect(PrePullAllImages(ctx)).Should(gomega.Succeed())
			ginkgo.By("setting up pods to be used by tests")
			pods := []*v1.Pod{}
			for _, spec := range testSpecs {
				pods = append(pods, spec.pod)
			}
			e2epod.NewPodClient(f).CreateBatch(ctx, pods)
		})

		ginkgo.It("should eventually evict all of the correct pods", func(ctx context.Context) {
			ginkgo.By(fmt.Sprintf("Waiting for node to have NodeCondition: %s", expectedNodeCondition))
			gomega.Eventually(ctx, func(ctx context.Context) error {
				logFunc(ctx)
				if expectedNodeCondition == noPressure || hasNodeCondition(ctx, f, expectedNodeCondition) {
					return nil
				}
				return fmt.Errorf("NodeCondition: %s not encountered", expectedNodeCondition)
			}, pressureTimeout, evictionPollInterval).Should(gomega.BeNil())

			ginkgo.By("Waiting for evictions to occur")
			nodeUnreadyTime := time.Now()

			gomega.Eventually(ctx, func(ctx context.Context) error {
				if expectedNodeCondition != noPressure {
					if hasNodeCondition(ctx, f, expectedNodeCondition) {
						framework.Logf("Node has %s", expectedNodeCondition)
					} else {
						framework.Logf("Node does NOT have %s", expectedNodeCondition)
						nodeUnreadyTime = time.Now()
					}
				}
				logKubeletLatencyMetrics(ctx, kubeletmetrics.EvictionStatsAgeKey)
				logFunc(ctx)

				verifyEvictionPeriod(ctx, f, testSpecs, nodeUnreadyTime)
				return verifyEvictionOrdering(ctx, f, testSpecs)
			}, pressureTimeout, evictionPollInterval).Should(gomega.Succeed())

			ginkgo.By("checking for the expected pod conditions for evicted pods")
			verifyPodConditions(ctx, f, testSpecs)

			// We observe pressure from the API server.  The eviction manager observes pressure from the kubelet internal stats.
			// This means the eviction manager will observe pressure before we will, creating a delay between when the eviction manager
			// evicts a pod, and when we observe the pressure by querying the API server.  Add a delay here to account for this delay
			ginkgo.By("making sure pressure from test has surfaced before continuing")
			time.Sleep(pressureDelay)

			ginkgo.By(fmt.Sprintf("Waiting for NodeCondition: %s to no longer exist on the node", expectedNodeCondition))
			gomega.Eventually(ctx, func(ctx context.Context) error {
				logFunc(ctx)
				logKubeletLatencyMetrics(ctx, kubeletmetrics.EvictionStatsAgeKey)
				if expectedNodeCondition != noPressure && hasNodeCondition(ctx, f, expectedNodeCondition) {
					return fmt.Errorf("Conditions haven't returned to normal, node still has %s", expectedNodeCondition)
				}
				return nil
			}, pressureDisappearTimeout, evictionPollInterval).Should(gomega.BeNil())

			ginkgo.By("checking for stable, pressure-free condition without unexpected pod failures")
			gomega.Consistently(ctx, func(ctx context.Context) error {
				if expectedNodeCondition != noPressure && hasNodeCondition(ctx, f, expectedNodeCondition) {
					return fmt.Errorf("%s disappeared and then reappeared", expectedNodeCondition)
				}
				logFunc(ctx)
				logKubeletLatencyMetrics(ctx, kubeletmetrics.EvictionStatsAgeKey)
				return verifyEvictionOrdering(ctx, f, testSpecs)
			}, postTestConditionMonitoringPeriod, evictionPollInterval).Should(gomega.Succeed())

			ginkgo.By("checking for correctly formatted eviction events")
			gomega.Eventually(ctx, func(ctx context.Context) error {
				return verifyEvictionEvents(ctx, f, testSpecs, expectedStarvedResource)
			}, postTestConditionMonitoringPeriod, evictionPollInterval).Should(gomega.Succeed())
		})

		ginkgo.AfterEach(func(ctx context.Context) {
			prePullImagesIfNeccecary := func() {
				if framework.TestContext.PrepullImages {
					// The disk eviction test may cause the prepulled images to be evicted,
					// prepull those images again to ensure this test not affect following tests.
					err := PrePullAllImages(ctx)
					gomega.Expect(err).ShouldNot(gomega.HaveOccurred())
				}
			}
			// Run prePull using a defer to make sure it is executed even when the assertions below fails
			defer prePullImagesIfNeccecary()

			ginkgo.By("deleting pods")
			for _, spec := range testSpecs {
				ginkgo.By(fmt.Sprintf("deleting pod: %s", spec.pod.Name))
				e2epod.NewPodClient(f).DeleteSync(ctx, spec.pod.Name, metav1.DeleteOptions{}, 10*time.Minute)
			}

			// In case a test fails before verifying that NodeCondition no longer exist on the node,
			// we should wait for the NodeCondition to disappear
			ginkgo.By(fmt.Sprintf("making sure NodeCondition %s no longer exists on the node", expectedNodeCondition))
			gomega.Eventually(ctx, func(ctx context.Context) error {
				if expectedNodeCondition != noPressure && hasNodeCondition(ctx, f, expectedNodeCondition) {
					return fmt.Errorf("Conditions haven't returned to normal, node still has %s", expectedNodeCondition)
				}
				return nil
			}, pressureDisappearTimeout, evictionPollInterval).Should(gomega.BeNil())

			reduceAllocatableMemoryUsageIfCgroupv1()
			ginkgo.By("making sure we have all the required images for testing")
			prePullImagesIfNeccecary()

			// Ensure that the NodeCondition hasn't returned after pulling images
			ginkgo.By(fmt.Sprintf("making sure NodeCondition %s doesn't exist again after pulling images", expectedNodeCondition))
			gomega.Eventually(ctx, func(ctx context.Context) error {
				if expectedNodeCondition != noPressure && hasNodeCondition(ctx, f, expectedNodeCondition) {
					return fmt.Errorf("Conditions haven't returned to normal, node still has %s", expectedNodeCondition)
				}
				return nil
			}, pressureDisappearTimeout, evictionPollInterval).Should(gomega.BeNil())

			ginkgo.By("making sure we can start a new pod after the test")
			podName := "test-admit-pod"
			e2epod.NewPodClient(f).CreateSync(ctx, &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: podName,
				},
				Spec: v1.PodSpec{
					RestartPolicy: v1.RestartPolicyNever,
					Containers: []v1.Container{
						{
							Image: imageutils.GetPauseImageName(),
							Name:  podName,
						},
					},
				},
			})

			if ginkgo.CurrentSpecReport().Failed() {
				if framework.TestContext.DumpLogsOnFailure {
					logPodEvents(ctx, f)
					logNodeEvents(ctx, f)
				}
			}
		})
	})
}

// verifyEvictionOrdering returns an error if all non-zero priority pods have not been evicted, nil otherwise
// This function panics (via Expect) if eviction ordering is violated, or if a priority-zero pod fails.
func verifyEvictionOrdering(ctx context.Context, f *framework.Framework, testSpecs []podEvictSpec) error {
	// Gather current information
	updatedPodList, err := f.ClientSet.CoreV1().Pods(f.Namespace.Name).List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	updatedPods := updatedPodList.Items
	for _, p := range updatedPods {
		framework.Logf("fetching pod %s; phase= %v", p.Name, p.Status.Phase)
	}

	ginkgo.By("checking eviction ordering and ensuring important pods don't fail")
	done := true
	pendingPods := []string{}
	for _, priorityPodSpec := range testSpecs {
		var priorityPod v1.Pod
		for _, p := range updatedPods {
			if p.Name == priorityPodSpec.pod.Name {
				priorityPod = p
			}
		}
		gomega.Expect(priorityPod).NotTo(gomega.BeNil())
		gomega.Expect(priorityPod.Status.Phase).ToNot(gomega.Equal(v1.PodSucceeded),
			fmt.Sprintf("pod: %s succeeded unexpectedly", priorityPod.Name))

		// Check eviction ordering.
		// Note: it is alright for a priority 1 and priority 2 pod (for example) to fail in the same round,
		// but never alright for a priority 1 pod to fail while the priority 2 pod is still running
		for _, lowPriorityPodSpec := range testSpecs {
			var lowPriorityPod v1.Pod
			for _, p := range updatedPods {
				if p.Name == lowPriorityPodSpec.pod.Name {
					lowPriorityPod = p
				}
			}
			gomega.Expect(lowPriorityPod).NotTo(gomega.BeNil())
			if priorityPodSpec.evictionPriority < lowPriorityPodSpec.evictionPriority && lowPriorityPod.Status.Phase == v1.PodRunning {
				gomega.Expect(priorityPod.Status.Phase).ToNot(gomega.Equal(v1.PodFailed),
					fmt.Sprintf("priority %d pod: %s failed before priority %d pod: %s",
						priorityPodSpec.evictionPriority, priorityPodSpec.pod.Name, lowPriorityPodSpec.evictionPriority, lowPriorityPodSpec.pod.Name))
			}
		}

		if priorityPod.Status.Phase == v1.PodFailed {
			gomega.Expect(priorityPod.Status.Reason).To(gomega.Equal(eviction.Reason), "pod %s failed; expected Status.Reason to be %s, but got %s",
				priorityPod.Name, eviction.Reason, priorityPod.Status.Reason)
		}

		// EvictionPriority 0 pods should not fail
		if priorityPodSpec.evictionPriority == 0 {
			gomega.Expect(priorityPod.Status.Phase).ToNot(gomega.Equal(v1.PodFailed),
				fmt.Sprintf("priority 0 pod: %s failed", priorityPod.Name))
		}

		// If a pod that is not evictionPriority 0 has not been evicted, we are not done
		if priorityPodSpec.evictionPriority != 0 && priorityPod.Status.Phase != v1.PodFailed {
			pendingPods = append(pendingPods, priorityPod.ObjectMeta.Name)
			done = false
		}
	}
	if done {
		return nil
	}
	return fmt.Errorf("pods that should be evicted are still running: %#v", pendingPods)
}

func verifyEvictionPeriod(ctx context.Context, f *framework.Framework, testSpecs []podEvictSpec, nodeUnreadyTime time.Time) {
	for i, spec := range testSpecs {
		if spec.evictionMaxPodGracePeriod == 0 && spec.evictionSoftGracePeriod == 0 {
			continue
		}
		softEvictionPeriod := spec.evictionMaxPodGracePeriod + spec.evictionSoftGracePeriod

		pod, err := f.ClientSet.CoreV1().Pods(f.Namespace.Name).Get(ctx, spec.pod.Name, metav1.GetOptions{})
		framework.ExpectNoError(err, "Failed to get the recent pod object for name: %q", pod.Name)

		minSoftEvictionPeriod := min(float64(softEvictionPeriod), float64(*spec.pod.Spec.TerminationGracePeriodSeconds+int64(spec.evictionSoftGracePeriod)))
		if pod.Status.Phase == v1.PodFailed {
			if time.Since(nodeUnreadyTime).Seconds() > minSoftEvictionPeriod+15 {
				framework.Failf("pod %s should be evicted within %f seconds, but it has not been evicted for %f seconds.", pod.Name, minSoftEvictionPeriod, time.Since(nodeUnreadyTime).Seconds())
			} else {
				testSpecs[i].evictionMaxPodGracePeriod = 0
				testSpecs[i].evictionSoftGracePeriod = 0
			}
		}
	}
}

func verifyPodConditions(ctx context.Context, f *framework.Framework, testSpecs []podEvictSpec) {
	for _, spec := range testSpecs {
		if spec.wantPodDisruptionCondition != nil {
			pod, err := f.ClientSet.CoreV1().Pods(f.Namespace.Name).Get(ctx, spec.pod.Name, metav1.GetOptions{})
			framework.ExpectNoError(err, "Failed to get the recent pod object for name: %q", pod.Name)

			cType := *spec.wantPodDisruptionCondition
			podDisruptionCondition := e2epod.FindPodConditionByType(&pod.Status, cType)
			if podDisruptionCondition == nil {
				framework.Failf("pod %q should have the condition: %q, pod status: %v", pod.Name, cType, pod.Status)
			}
		}
	}
}

func verifyEvictionEvents(ctx context.Context, f *framework.Framework, testSpecs []podEvictSpec, expectedStarvedResource v1.ResourceName) error {
	for _, spec := range testSpecs {
		pod := spec.pod
		if spec.evictionPriority != 0 {
			selector := fields.Set{
				"involvedObject.kind":      "Pod",
				"involvedObject.name":      pod.Name,
				"involvedObject.namespace": f.Namespace.Name,
				"reason":                   eviction.Reason,
			}.AsSelector().String()
			podEvictEvents, err := f.ClientSet.CoreV1().Events(f.Namespace.Name).List(ctx, metav1.ListOptions{FieldSelector: selector})
			framework.ExpectNoError(err, "getting events")
			gomega.Expect(podEvictEvents.Items).To(gomega.HaveLen(1), "Expected to find 1 eviction event for pod %s, got %d", pod.Name, len(podEvictEvents.Items))
			event := podEvictEvents.Items[0]
			if expectedStarvedResource != noStarvedResource {
				// Check the eviction.StarvedResourceKey
				starved, found := event.Annotations[eviction.StarvedResourceKey]
				if !found {
					return fmt.Errorf("Expected to find an annotation on the eviction event for pod %s containing the starved resource %s, but it was not found",
						pod.Name, expectedStarvedResource)
				}
				starvedResource := v1.ResourceName(starved)
				gomega.Expect(starvedResource).To(gomega.Equal(expectedStarvedResource), "Expected to the starved_resource annotation on pod %s to contain %s, but got %s instead",
					pod.Name, expectedStarvedResource, starvedResource)
				// We only check these keys for memory, because ephemeral storage evictions may be due to volume usage, in which case these values are not present
				if expectedStarvedResource == v1.ResourceMemory {
					// Check the eviction.OffendingContainersKey
					offendersString, found := event.Annotations[eviction.OffendingContainersKey]
					if !found {
						return fmt.Errorf("Expected to find an annotation on the eviction event for pod %s containing the offending containers, but it was not found",
							pod.Name)
					}
					offendingContainers := strings.Split(offendersString, ",")
					gomega.Expect(offendingContainers).To(gomega.HaveLen(1), "Expected to find the offending container's usage in the %s annotation, but no container was found",
						eviction.OffendingContainersKey)
					gomega.Expect(offendingContainers[0]).To(gomega.Equal(pod.Spec.Containers[0].Name), "Expected to find the offending container: %s's usage in the %s annotation, but found %s instead",
						pod.Spec.Containers[0].Name, eviction.OffendingContainersKey, offendingContainers[0])

					// Check the eviction.OffendingContainersUsageKey
					offendingUsageString, found := event.Annotations[eviction.OffendingContainersUsageKey]
					if !found {
						return fmt.Errorf("Expected to find an annotation on the eviction event for pod %s containing the offending containers' usage, but it was not found",
							pod.Name)
					}
					offendingContainersUsage := strings.Split(offendingUsageString, ",")
					gomega.Expect(offendingContainersUsage).To(gomega.HaveLen(1), "Expected to find the offending container's usage in the %s annotation, but found %+v",
						eviction.OffendingContainersUsageKey, offendingContainersUsage)
					usageQuantity, err := resource.ParseQuantity(offendingContainersUsage[0])
					framework.ExpectNoError(err, "parsing pod %s's %s annotation as a quantity", pod.Name, eviction.OffendingContainersUsageKey)
					request := pod.Spec.Containers[0].Resources.Requests[starvedResource]
					gomega.Expect(usageQuantity.Cmp(request)).To(gomega.Equal(1), "Expected usage of offending container: %s in pod %s to exceed its request %s",
						usageQuantity.String(), pod.Name, request.String())
				}
			}
		}
	}
	return nil
}

// Returns TRUE if the node has the node condition, FALSE otherwise
func hasNodeCondition(ctx context.Context, f *framework.Framework, expectedNodeCondition v1.NodeConditionType) bool {
	localNodeStatus := getLocalNode(ctx, f).Status
	_, actualNodeCondition := testutils.GetNodeCondition(&localNodeStatus, expectedNodeCondition)
	gomega.Expect(actualNodeCondition).NotTo(gomega.BeNil())
	return actualNodeCondition.Status == v1.ConditionTrue
}

func logInodeMetrics(ctx context.Context) {
	summary, err := getNodeSummary(ctx)
	if err != nil {
		framework.Logf("Error getting summary: %v", err)
		return
	}
	if summary.Node.Runtime != nil && summary.Node.Runtime.ImageFs != nil && summary.Node.Runtime.ImageFs.Inodes != nil && summary.Node.Runtime.ImageFs.InodesFree != nil {
		framework.Logf("imageFsInfo.Inodes: %d, imageFsInfo.InodesFree: %d", *summary.Node.Runtime.ImageFs.Inodes, *summary.Node.Runtime.ImageFs.InodesFree)
	}
	if summary.Node.Fs != nil && summary.Node.Fs.Inodes != nil && summary.Node.Fs.InodesFree != nil {
		framework.Logf("rootFsInfo.Inodes: %d, rootFsInfo.InodesFree: %d", *summary.Node.Fs.Inodes, *summary.Node.Fs.InodesFree)
	}
	for _, pod := range summary.Pods {
		framework.Logf("Pod: %s", pod.PodRef.Name)
		for _, container := range pod.Containers {
			if container.Rootfs != nil && container.Rootfs.InodesUsed != nil {
				framework.Logf("--- summary Container: %s inodeUsage: %d", container.Name, *container.Rootfs.InodesUsed)
			}
		}
		for _, volume := range pod.VolumeStats {
			if volume.FsStats.InodesUsed != nil {
				framework.Logf("--- summary Volume: %s inodeUsage: %d", volume.Name, *volume.FsStats.InodesUsed)
			}
		}
	}
}

func logDiskMetrics(ctx context.Context) {
	summary, err := getNodeSummary(ctx)
	if err != nil {
		framework.Logf("Error getting summary: %v", err)
		return
	}
	if summary.Node.Runtime != nil && summary.Node.Runtime.ImageFs != nil && summary.Node.Runtime.ImageFs.CapacityBytes != nil && summary.Node.Runtime.ImageFs.AvailableBytes != nil {
		framework.Logf("imageFsInfo.CapacityBytes: %d, imageFsInfo.AvailableBytes: %d", *summary.Node.Runtime.ImageFs.CapacityBytes, *summary.Node.Runtime.ImageFs.AvailableBytes)
	}
	if summary.Node.Fs != nil && summary.Node.Fs.CapacityBytes != nil && summary.Node.Fs.AvailableBytes != nil {
		framework.Logf("rootFsInfo.CapacityBytes: %d, rootFsInfo.AvailableBytes: %d", *summary.Node.Fs.CapacityBytes, *summary.Node.Fs.AvailableBytes)
	}
	for _, pod := range summary.Pods {
		framework.Logf("Pod: %s", pod.PodRef.Name)
		for _, container := range pod.Containers {
			if container.Rootfs != nil && container.Rootfs.UsedBytes != nil {
				framework.Logf("--- summary Container: %s UsedBytes: %d", container.Name, *container.Rootfs.UsedBytes)
			}
		}
		for _, volume := range pod.VolumeStats {
			if volume.FsStats.InodesUsed != nil {
				framework.Logf("--- summary Volume: %s UsedBytes: %d", volume.Name, *volume.FsStats.UsedBytes)
			}
		}
	}
}

func logMemoryMetrics(ctx context.Context) {
	summary, err := getNodeSummary(ctx)
	if err != nil {
		framework.Logf("Error getting summary: %v", err)
		return
	}
	if summary.Node.Memory != nil && summary.Node.Memory.WorkingSetBytes != nil && summary.Node.Memory.AvailableBytes != nil {
		framework.Logf("Node.Memory.WorkingSetBytes: %d, Node.Memory.AvailableBytes: %d", *summary.Node.Memory.WorkingSetBytes, *summary.Node.Memory.AvailableBytes)
	}
	for _, sysContainer := range summary.Node.SystemContainers {
		if sysContainer.Name == kubeletstatsv1alpha1.SystemContainerPods && sysContainer.Memory != nil && sysContainer.Memory.WorkingSetBytes != nil && sysContainer.Memory.AvailableBytes != nil {
			framework.Logf("Allocatable.Memory.WorkingSetBytes: %d, Allocatable.Memory.AvailableBytes: %d", *sysContainer.Memory.WorkingSetBytes, *sysContainer.Memory.AvailableBytes)
		}
	}
	for _, pod := range summary.Pods {
		framework.Logf("Pod: %s", pod.PodRef.Name)
		for _, container := range pod.Containers {
			if container.Memory != nil && container.Memory.WorkingSetBytes != nil {
				framework.Logf("--- summary Container: %s WorkingSetBytes: %d", container.Name, *container.Memory.WorkingSetBytes)
			}
		}
	}
}

func logPidMetrics(ctx context.Context) {
	summary, err := getNodeSummary(ctx)
	if err != nil {
		framework.Logf("Error getting summary: %v", err)
		return
	}
	if summary.Node.Rlimit != nil && summary.Node.Rlimit.MaxPID != nil && summary.Node.Rlimit.NumOfRunningProcesses != nil {
		framework.Logf("Node.Rlimit.MaxPID: %d, Node.Rlimit.RunningProcesses: %d", *summary.Node.Rlimit.MaxPID, *summary.Node.Rlimit.NumOfRunningProcesses)
	}
}

func eventuallyGetSummary(ctx context.Context) (s *kubeletstatsv1alpha1.Summary) {
	gomega.Eventually(ctx, func() error {
		summary, err := getNodeSummary(ctx)
		if err != nil {
			return err
		}
		if summary == nil || summary.Node.Fs == nil || summary.Node.Fs.InodesFree == nil || summary.Node.Fs.AvailableBytes == nil {
			return fmt.Errorf("some part of data is nil")
		}
		s = summary
		return nil
	}, time.Minute, evictionPollInterval).Should(gomega.BeNil())
	return
}

// returns a pod that does not use any resources
func innocentPod() *v1.Pod {
	// Due to https://github.com/kubernetes/kubernetes/issues/115819,
	// When evictionHard to used, we were setting grace period to 0 which meant the default setting (30 seconds)
	// This could help with flakiness as we should send sigterm right away.
	var gracePeriod int64 = 1
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "innocent-pod"},
		Spec: v1.PodSpec{
			RestartPolicy:                 v1.RestartPolicyNever,
			TerminationGracePeriodSeconds: &gracePeriod,
			Containers: []v1.Container{
				{
					Image: busyboxImage,
					Name:  "innocent-container",
					Command: []string{
						"sh",
						"-c",
						"while true; do sleep 5; done",
					},
					Resources: v1.ResourceRequirements{
						// These values are set so that we don't consider this pod to be over the limits
						// If the requests are not set, then we assume a request limit of 0 so it is always over.
						// This fixes this for the innocent pod.
						Requests: v1.ResourceList{
							v1.ResourceEphemeralStorage: resource.MustParse("50Mi"),
							v1.ResourceMemory:           resource.MustParse("50Mi"),
						},
						Limits: v1.ResourceList{
							v1.ResourceEphemeralStorage: resource.MustParse("50Mi"),
							v1.ResourceMemory:           resource.MustParse("50Mi"),
						},
					},
				},
			},
		},
	}
}

const (
	volumeMountPath = "/test-mnt"
	volumeName      = "test-volume"
)

func inodeConsumingPod(name string, numFiles int, volumeSource *v1.VolumeSource) *v1.Pod {
	path := ""
	if volumeSource != nil {
		path = volumeMountPath
	}
	// Each iteration creates an empty file
	return podWithCommand(volumeSource, v1.ResourceRequirements{}, numFiles, name, fmt.Sprintf("touch %s${i}.txt; sleep 0.001;", filepath.Join(path, "file")))
}

func diskConsumingPod(name string, diskConsumedMB int, volumeSource *v1.VolumeSource, resources v1.ResourceRequirements) *v1.Pod {
	path := ""
	if volumeSource != nil {
		path = volumeMountPath
	}
	// Each iteration writes 1 Mb, so do diskConsumedMB iterations.
	return podWithCommand(volumeSource, resources, diskConsumedMB, name, fmt.Sprintf("dd if=/dev/urandom of=%s${i} bs=1048576 count=1 2>/dev/null; sleep .1;", filepath.Join(path, "file")))
}

func pidConsumingPod(name string, numProcesses int) *v1.Pod {
	// Each iteration forks once, but creates two processes
	return podWithCommand(nil, v1.ResourceRequirements{}, numProcesses/2, name, "(while true; do /bin/sleep 5; done)&")
}

// podWithCommand returns a pod with the provided volumeSource and resourceRequirements.
func podWithCommand(volumeSource *v1.VolumeSource, resources v1.ResourceRequirements, iterations int, name, command string) *v1.Pod {
	// Due to https://github.com/kubernetes/kubernetes/issues/115819,
	// When evictionHard to used, we were setting grace period to 0 which meant the default setting (30 seconds)
	// This could help with flakiness as we should send sigterm right away.
	var gracePeriod int64 = 1
	volumeMounts := []v1.VolumeMount{}
	volumes := []v1.Volume{}
	if volumeSource != nil {
		volumeMounts = []v1.VolumeMount{{MountPath: volumeMountPath, Name: volumeName}}
		volumes = []v1.Volume{{Name: volumeName, VolumeSource: *volumeSource}}
	}
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("%s-pod", name)},
		Spec: v1.PodSpec{
			RestartPolicy:                 v1.RestartPolicyNever,
			TerminationGracePeriodSeconds: &gracePeriod,
			Containers: []v1.Container{
				{
					Image: busyboxImage,
					Name:  fmt.Sprintf("%s-container", name),
					Command: []string{
						"sh",
						"-c",
						fmt.Sprintf("i=0; while [ $i -lt %d ]; do %s i=$(($i+1)); done; while true; do sleep 5; done", iterations, command),
					},
					Resources:    resources,
					VolumeMounts: volumeMounts,
				},
			},
			Volumes: volumes,
		},
	}
}

func getMemhogPod(podName string, ctnName string, res v1.ResourceRequirements) *v1.Pod {
	// Due to https://github.com/kubernetes/kubernetes/issues/115819,
	// When evictionHard to used, we were setting grace period to 0 which meant the default setting (30 seconds)
	// This could help with flakiness as we should send sigterm right away.
	var gracePeriod int64 = 1
	env := []v1.EnvVar{
		{
			Name: "MEMORY_LIMIT",
			ValueFrom: &v1.EnvVarSource{
				ResourceFieldRef: &v1.ResourceFieldSelector{
					Resource: "limits.memory",
				},
			},
		},
	}

	// If there is a limit specified, pass 80% of it for -mem-total, otherwise use the downward API
	// to pass limits.memory, which will be the total memory available.
	// This helps prevent a guaranteed pod from triggering an OOM kill due to it's low memory limit,
	// which will cause the test to fail inappropriately.
	var memLimit string
	if limit, ok := res.Limits[v1.ResourceMemory]; ok {
		memLimit = strconv.Itoa(int(
			float64(limit.Value()) * 0.8))
	} else {
		memLimit = "$(MEMORY_LIMIT)"
	}

	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: podName,
		},
		Spec: v1.PodSpec{
			RestartPolicy:                 v1.RestartPolicyNever,
			TerminationGracePeriodSeconds: &gracePeriod,
			Containers: []v1.Container{
				{
					Name:            ctnName,
					Image:           imageutils.GetE2EImage(imageutils.Agnhost),
					ImagePullPolicy: "Always",
					Env:             env,
					// 60 min timeout * 60s / tick per 10s = 360 ticks before timeout => ~11.11Mi/tick
					// to fill ~4Gi of memory, so initial ballpark 12Mi/tick.
					// We might see flakes due to timeout if the total memory on the nodes increases.
					Args:      []string{"stress", "--mem-alloc-size", "12Mi", "--mem-alloc-sleep", "10s", "--mem-total", memLimit},
					Resources: res,
				},
			},
		},
	}
}
