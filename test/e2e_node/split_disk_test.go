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

package e2enode

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"k8s.io/kubernetes/pkg/features"
	"k8s.io/kubernetes/test/e2e/feature"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
	kubeletconfig "k8s.io/kubernetes/pkg/kubelet/apis/config"
	evictionapi "k8s.io/kubernetes/pkg/kubelet/eviction/api"
	kubeletmetrics "k8s.io/kubernetes/pkg/kubelet/metrics"
	"k8s.io/kubernetes/test/e2e/framework"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	imageutils "k8s.io/kubernetes/test/utils/image"
	admissionapi "k8s.io/pod-security-admission/api"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
)

var _ = SIGDescribe("KubeletSeparateDiskGC", feature.KubeletSeparateDiskGC, func() {
	f := framework.NewDefaultFramework("split-disk-test")
	f.NamespacePodSecurityLevel = admissionapi.LevelPrivileged
	pressureTimeout := 10 * time.Minute
	expectedNodeCondition := v1.NodeDiskPressure

	ginkgo.BeforeEach(func(ctx context.Context) {
		e2eskipper.SkipUnlessFeatureGateEnabled(features.KubeletSeparateDiskGC)
		if !hasSplitFileSystem(ctx) {
			ginkgo.Skip("it doesn't have split filesystem")
		}
	})

	f.It("should display different stats for imageFs and containerFs", func(ctx context.Context) {
		summary := eventuallyGetSummary(ctx)
		gomega.Expect(summary.Node.Fs.AvailableBytes).ToNot(gomega.Equal(summary.Node.Runtime.ImageFs.AvailableBytes))
		gomega.Expect(summary.Node.Fs.CapacityBytes).ToNot(gomega.Equal(summary.Node.Runtime.ImageFs.CapacityBytes))
		// Node.Fs represents rootfs where /var/lib/kubelet is located.
		// Since graphroot is left as the default in storage.conf, it will use the same filesystem location as rootfs.
		// Therefore, Node.Fs should be the same as Runtime.ContainerFs.
		gomega.Expect(summary.Node.Fs.AvailableBytes).To(gomega.Equal(summary.Node.Runtime.ContainerFs.AvailableBytes))
		gomega.Expect(summary.Node.Fs.CapacityBytes).To(gomega.Equal(summary.Node.Runtime.ContainerFs.CapacityBytes))
	})

	f.Context("when there is disk pressure", framework.WithSlow(), framework.WithSerial(), framework.WithDisruptive(), func() {
		f.Context("on imageFs", func() {
			tempSetCurrentKubeletConfig(f, func(ctx context.Context, initialConfig *kubeletconfig.KubeletConfiguration) {
				initialConfig.EvictionHard = map[string]string{
					string(evictionapi.SignalNodeFsAvailable):      "30%",
					string(evictionapi.SignalContainerFsAvailable): "30%",
					string(evictionapi.SignalImageFsAvailable):     "30%",
				}
				initialConfig.EvictionMinimumReclaim = map[string]string{}
				ginkgo.By(fmt.Sprintf("EvictionHard %s", initialConfig.EvictionHard))
			})

			runImageFsPressureTest(f, pressureTimeout, expectedNodeCondition, logDiskMetrics, []podEvictSpec{
				{
					evictionPriority: 1,
					pod:              innocentPod(),
				},
			})
		})

		f.Context("on containerFs", func() {
			expectedStarvedResource := v1.ResourceEphemeralStorage
			diskTestInMb := 5000

			tempSetCurrentKubeletConfig(f, func(ctx context.Context, initialConfig *kubeletconfig.KubeletConfiguration) {
				initialConfig.EvictionHard = map[string]string{
					string(evictionapi.SignalNodeFsAvailable):  "30%",
					string(evictionapi.SignalImageFsAvailable): "30%",
				}
				initialConfig.EvictionMinimumReclaim = map[string]string{}
				ginkgo.By(fmt.Sprintf("EvictionHard %s", initialConfig.EvictionHard))
			})
			runEvictionTest(f, pressureTimeout, expectedNodeCondition, expectedStarvedResource, logDiskMetrics, []podEvictSpec{
				{
					// This pod should exceed disk capacity on nodeFs since it writes a lot to writeable layer.
					evictionPriority: 1,
					pod: diskConsumingPod("container-emptydir-disk-limit", diskTestInMb, nil,
						v1.ResourceRequirements{}, true),
				},
			})
		})
	})
})

// runImageFsPressureTest tests are similar to eviction tests but will skip the checks on eviction itself,
// as we want to induce disk pressure on the imageFs filesystem.
func runImageFsPressureTest(f *framework.Framework, pressureTimeout time.Duration, expectedNodeCondition v1.NodeConditionType, logFunc func(ctx context.Context), testSpecs []podEvictSpec) {
	// Place the remainder of the test within a context so that the kubelet config is set before and after the test.
	ginkgo.Context("", func() {
		ginkgo.BeforeEach(func(ctx context.Context) {
			// Reduce memory usage in the allocatable cgroup to ensure we do not have MemoryPressure.
			reduceAllocatableMemoryUsageIfCgroupv1()
			// Nodes do not immediately report local storage capacity,
			// so wait a little to allow pods requesting local storage to be scheduled.
			time.Sleep(30 * time.Second)
			ginkgo.By("setting up pods to be used by tests")
			pods := []*v1.Pod{}
			for _, spec := range testSpecs {
				pods = append(pods, spec.pod)
			}
			e2epod.NewPodClient(f).CreateBatch(ctx, pods)
		})

		ginkgo.It("should evict all of the correct pods", func(ctx context.Context) {
			_, is, err := getCRIClient()
			framework.ExpectNoError(err)
			resp, err := is.ImageFsInfo(ctx)
			framework.ExpectNoError(err)
			gomega.Expect(resp.ImageFilesystems).NotTo(gomega.BeEmpty())
			gomega.Expect(resp.ImageFilesystems[0].FsId).NotTo(gomega.BeNil())
			diskToPressure := filepath.Dir(resp.ImageFilesystems[0].FsId.Mountpoint)
			ginkgo.By(fmt.Sprintf("Got imageFs directory: %s", diskToPressure))
			imagesLenBeforeGC := 1
			sizeOfPressure := "8000"
			gomega.Eventually(ctx, func(ctx context.Context) error {
				images, err := is.ListImages(ctx, &runtimeapi.ImageFilter{})
				imagesLenBeforeGC = len(images)
				return err
			}, 1*time.Minute, evictionPollInterval).Should(gomega.Succeed())
			ginkgo.By(fmt.Sprintf("Number of images found before GC was %d", imagesLenBeforeGC))
			ginkgo.By(fmt.Sprintf("Induce disk pressure on %s with size %s", diskToPressure, sizeOfPressure))
			gomega.Expect(runDDOnFilesystem(diskToPressure, sizeOfPressure)).Should(gomega.Succeed())
			ginkgo.By(fmt.Sprintf("Waiting for node to have NodeCondition: %s", expectedNodeCondition))

			gomega.Eventually(ctx, func(ctx context.Context) error {
				logFunc(ctx)
				if expectedNodeCondition == noPressure || hasNodeCondition(ctx, f, expectedNodeCondition) {
					return nil
				}
				return fmt.Errorf("NodeCondition: %s not encountered", expectedNodeCondition)
			}, pressureTimeout, evictionPollInterval).Should(gomega.BeNil())

			ginkgo.By("Waiting for evictions to occur")
			gomega.Eventually(ctx, func(ctx context.Context) error {
				if expectedNodeCondition != noPressure {
					if hasNodeCondition(ctx, f, expectedNodeCondition) {
						framework.Logf("Node has condition: %s", expectedNodeCondition)
					} else {
						framework.Logf("Node does NOT have condition: %s", expectedNodeCondition)
					}
				}
				logKubeletLatencyMetrics(ctx, kubeletmetrics.EvictionStatsAgeKey)
				logFunc(ctx)
				return verifyEvictionOrdering(ctx, f, testSpecs)
			}, pressureTimeout, evictionPollInterval).Should(gomega.Succeed())

			ginkgo.By("checking for the expected pod conditions for evicted pods")
			verifyPodConditions(ctx, f, testSpecs)

			gomega.Eventually(ctx, func(ctx context.Context) error {
				images, err := is.ListImages(ctx, &runtimeapi.ImageFilter{})
				if err != nil {
					return err
				}
				imagesLenAfterGC := len(images)
				if imagesLenAfterGC < imagesLenBeforeGC {
					return nil
				}
				return fmt.Errorf("garbage collection of images should have occurred. before: %d after: %d", imagesLenBeforeGC, imagesLenAfterGC)
			}, pressureTimeout, evictionPollInterval).Should(gomega.Succeed())

			gomega.Expect(removeDiskPressure(diskToPressure)).Should(gomega.Succeed(), "removing disk pressure should not fail")

			ginkgo.By("making sure pressure from test has surfaced before continuing")

			ginkgo.By(fmt.Sprintf("Waiting for NodeCondition: %s to no longer exist on the node", expectedNodeCondition))
			gomega.Eventually(ctx, func(ctx context.Context) error {
				logFunc(ctx)
				logKubeletLatencyMetrics(ctx, kubeletmetrics.EvictionStatsAgeKey)
				if expectedNodeCondition != noPressure && hasNodeCondition(ctx, f, expectedNodeCondition) {
					return fmt.Errorf("conditions haven't returned to normal, node still has: %s", expectedNodeCondition)
				}
				return nil
			}, pressureTimeout, evictionPollInterval).Should(gomega.BeNil())

			ginkgo.By("checking for stable, pressure-free condition without unexpected pod failures")
			gomega.Consistently(ctx, func(ctx context.Context) error {
				if expectedNodeCondition != noPressure && hasNodeCondition(ctx, f, expectedNodeCondition) {
					return fmt.Errorf("condition %s disappeared and then reappeared", expectedNodeCondition)
				}
				logFunc(ctx)
				logKubeletLatencyMetrics(ctx, kubeletmetrics.EvictionStatsAgeKey)
				return verifyEvictionOrdering(ctx, f, testSpecs)
			}, postTestConditionMonitoringPeriod, evictionPollInterval).Should(gomega.Succeed())
		})

		ginkgo.AfterEach(func(ctx context.Context) {
			prePullImagesIfNecessary := func() {
				if expectedNodeCondition == v1.NodeDiskPressure && framework.TestContext.PrepullImages {
					// The disk eviction test may cause the pre-pulled images to be evicted,
					// so pre-pull those images again to ensure this test does not affect subsequent tests.
					err := PrePullAllImages(ctx)
					framework.ExpectNoError(err)
				}
			}
			// Run pre-pull for images using a `defer` to ensure that images are pulled even when the subsequent assertions fail.
			defer prePullImagesIfNecessary()

			ginkgo.By("deleting pods")
			for _, spec := range testSpecs {
				ginkgo.By(fmt.Sprintf("deleting pod: %s", spec.pod.Name))
				e2epod.NewPodClient(f).DeleteSync(ctx, spec.pod.Name, metav1.DeleteOptions{}, 10*time.Minute)
			}

			// In case a test fails before verifying that NodeCondition no longer exist on the node,
			// we should wait for the NodeCondition to disappear.
			ginkgo.By(fmt.Sprintf("making sure NodeCondition %s no longer exists on the node", expectedNodeCondition))
			gomega.Eventually(ctx, func(ctx context.Context) error {
				if expectedNodeCondition != noPressure && hasNodeCondition(ctx, f, expectedNodeCondition) {
					return fmt.Errorf("conditions haven't returned to normal, node still has: %s", expectedNodeCondition)
				}
				return nil
			}, pressureDisappearTimeout, evictionPollInterval).Should(gomega.BeNil())

			reduceAllocatableMemoryUsageIfCgroupv1()
			ginkgo.By("making sure we have all the required images for testing")
			prePullImagesIfNecessary()

			// Ensure that the NodeCondition hasn't returned after pulling images.
			ginkgo.By(fmt.Sprintf("making sure NodeCondition %s doesn't exist again after pulling images", expectedNodeCondition))
			gomega.Eventually(ctx, func(ctx context.Context) error {
				if expectedNodeCondition != noPressure && hasNodeCondition(ctx, f, expectedNodeCondition) {
					return fmt.Errorf("conditions haven't returned to normal, node still has: %s", expectedNodeCondition)
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

func runDDOnFilesystem(diskToPressure, sizeOfPressure string) error {
	script := strings.Split(fmt.Sprintf("if=/dev/zero of=%s/file.txt bs=1M count=%s", diskToPressure, sizeOfPressure), " ")
	ginkgo.By(fmt.Sprintf("running dd with %s", fmt.Sprintf("if=/dev/zero of=%s/file.txt bs=1M count=%s", diskToPressure, sizeOfPressure)))
	cmd := exec.Command("dd", script...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		fmt.Println(string(output))
		fmt.Println(err)
	}
	return err
}

func removeDiskPressure(diskToPressure string) error {
	fileToRemove := fmt.Sprintf("%s/file.txt", diskToPressure)
	ginkgo.By(fmt.Sprintf("calling rm %s", fileToRemove))
	cmd := exec.Command("rm", fileToRemove)
	_, err := cmd.CombinedOutput()
	return err
}

func hasSplitFileSystem(ctx context.Context) bool {
	_, is, err := getCRIClient()
	framework.ExpectNoError(err)
	resp, err := is.ImageFsInfo(ctx)
	framework.ExpectNoError(err)
	if resp.ContainerFilesystems == nil || resp.ImageFilesystems == nil || len(resp.ContainerFilesystems) == 0 || len(resp.ImageFilesystems) == 0 {
		return false
	}
	if resp.ContainerFilesystems[0].FsId != nil && resp.ImageFilesystems[0].FsId != nil {
		return resp.ContainerFilesystems[0].FsId.Mountpoint != resp.ImageFilesystems[0].FsId.Mountpoint
	}
	return false
}
