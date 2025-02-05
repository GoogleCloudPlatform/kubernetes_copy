/*
Copyright 2019 The Kubernetes Authors.

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
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/kubernetes/pkg/features"
	"k8s.io/kubernetes/test/e2e/feature"
	"k8s.io/kubernetes/test/e2e/framework"
	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"
	e2emetrics "k8s.io/kubernetes/test/e2e/framework/metrics"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
	e2evolume "k8s.io/kubernetes/test/e2e/framework/volume"
	admissionapi "k8s.io/pod-security-admission/api"

	"github.com/prometheus/common/model"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/onsi/gomega/gstruct"
	"github.com/onsi/gomega/types"
)

const (
	pod0        = "stats-busybox-0"
	pod1        = "stats-busybox-1"
	maxStatsAge = time.Minute
)

var _ = SIGDescribe("ResourceMetricsAPI", feature.ResourceMetrics, func() {
	f := framework.NewDefaultFramework("resource-metrics")
	f.NamespacePodSecurityLevel = admissionapi.LevelPrivileged
	ginkgo.Context("when querying /resource/metrics", func() {
		ginkgo.BeforeEach(func(ctx context.Context) {
			createMetricsPods(ctx, f)
		})
		ginkgo.It("should report resource usage through the resource metrics api", func(ctx context.Context) {
			ginkgo.By("Fetching node so we can match against an appropriate memory limit")
			node := getLocalNode(ctx, f)
			memoryCapacity := node.Status.Capacity["memory"]
			memoryLimit := memoryCapacity.Value()

			keys := []string{
				"resource_scrape_error", "node_cpu_usage_seconds_total", "node_memory_working_set_bytes",
				"pod_cpu_usage_seconds_total", "pod_memory_working_set_bytes",
			}

			// NOTE: This check should be removed when ListMetricDescriptors is implemented
			// by CRI-O and Containerd
			if !e2eskipper.IsFeatureGateEnabled(features.PodAndContainerStatsFromCRI) {
				keys = append(keys, "container_cpu_usage_seconds_total", "container_memory_working_set_bytes", "container_start_time_seconds")
			}

			matchResourceMetrics := gomega.And(gstruct.MatchKeys(gstruct.IgnoreMissing, gstruct.Keys{
				"resource_scrape_error": gstruct.Ignore(),
				"node_cpu_usage_seconds_total": gstruct.MatchAllElements(nodeID, gstruct.Elements{
					"": boundedSample(1, 1e6),
				}),
				"node_memory_working_set_bytes": gstruct.MatchAllElements(nodeID, gstruct.Elements{
					"": boundedSample(10*e2evolume.Mb, memoryLimit),
				}),

				"container_cpu_usage_seconds_total": gstruct.MatchElements(containerID, gstruct.IgnoreExtras, gstruct.Elements{
					fmt.Sprintf("%s::%s::%s", f.Namespace.Name, pod0, "busybox-container"): boundedSample(0, 100),
					fmt.Sprintf("%s::%s::%s", f.Namespace.Name, pod1, "busybox-container"): boundedSample(0, 100),
				}),

				"container_memory_working_set_bytes": gstruct.MatchElements(containerID, gstruct.IgnoreExtras, gstruct.Elements{
					fmt.Sprintf("%s::%s::%s", f.Namespace.Name, pod0, "busybox-container"): boundedSample(10*e2evolume.Kb, 80*e2evolume.Mb),
					fmt.Sprintf("%s::%s::%s", f.Namespace.Name, pod1, "busybox-container"): boundedSample(10*e2evolume.Kb, 80*e2evolume.Mb),
				}),

				"container_start_time_seconds": gstruct.MatchElements(containerID, gstruct.IgnoreExtras, gstruct.Elements{
					fmt.Sprintf("%s::%s::%s", f.Namespace.Name, pod0, "busybox-container"): boundedSample(time.Now().Add(-maxStatsAge).Unix(), time.Now().Add(2*time.Minute).Unix()),
					fmt.Sprintf("%s::%s::%s", f.Namespace.Name, pod1, "busybox-container"): boundedSample(time.Now().Add(-maxStatsAge).Unix(), time.Now().Add(2*time.Minute).Unix()),
				}),

				"pod_cpu_usage_seconds_total": gstruct.MatchElements(podID, gstruct.IgnoreExtras, gstruct.Elements{
					fmt.Sprintf("%s::%s", f.Namespace.Name, pod0): boundedSample(0, 100),
					fmt.Sprintf("%s::%s", f.Namespace.Name, pod1): boundedSample(0, 100),
				}),

				"pod_memory_working_set_bytes": gstruct.MatchElements(podID, gstruct.IgnoreExtras, gstruct.Elements{
					fmt.Sprintf("%s::%s", f.Namespace.Name, pod0): boundedSample(10*e2evolume.Kb, 80*e2evolume.Mb),
					fmt.Sprintf("%s::%s", f.Namespace.Name, pod1): boundedSample(10*e2evolume.Kb, 80*e2evolume.Mb),
				}),

				"pod_swap_usage_bytes": gstruct.MatchElements(podID, gstruct.IgnoreExtras, gstruct.Elements{
					fmt.Sprintf("%s::%s", f.Namespace.Name, pod0): boundedSample(0*e2evolume.Kb, 80*e2evolume.Mb),
					fmt.Sprintf("%s::%s", f.Namespace.Name, pod1): boundedSample(0*e2evolume.Kb, 80*e2evolume.Mb),
				}),
			}),
				haveKeys(keys...),
			)
			ginkgo.By("Giving pods a minute to start up and produce metrics")
			gomega.Eventually(ctx, getResourceMetrics, 1*time.Minute, 15*time.Second).Should(matchResourceMetrics)
			ginkgo.By("Ensuring the metrics match the expectations a few more times")
			gomega.Consistently(ctx, getResourceMetrics, 1*time.Minute, 15*time.Second).Should(matchResourceMetrics)
		})
		ginkgo.AfterEach(func(ctx context.Context) {
			removeMetricsPods(ctx, f)
		})
	})
})

func createMetricsPods(ctx context.Context, f *framework.Framework) {
	ginkgo.By("Creating test pods to measure their resource usage")
	numRestarts := int32(1)
	pods := getSummaryTestPods(f, numRestarts, pod0, pod1)
	e2epod.NewPodClient(f).CreateBatch(ctx, pods)

	ginkgo.By("Restarting the containers to ensure container metrics are still being gathered after a container is restarted")
	gomega.Eventually(ctx, func(ctx context.Context) error {
		for _, pod := range pods {
			err := verifyPodRestartCount(ctx, f, pod.Name, len(pod.Spec.Containers), numRestarts)
			if err != nil {
				return err
			}
		}
		return nil
	}, time.Minute, 5*time.Second).Should(gomega.Succeed())
}

func getResourceMetrics(ctx context.Context) (e2emetrics.KubeletMetrics, error) {
	ginkgo.By("getting stable resource metrics API")
	return e2emetrics.GrabKubeletMetricsWithoutProxy(ctx, nodeNameOrIP()+":10255", "/metrics/resource")
}

func nodeID(element interface{}) string {
	return ""
}

func podID(element interface{}) string {
	el := element.(*model.Sample)
	return fmt.Sprintf("%s::%s", el.Metric["namespace"], el.Metric["pod"])
}

func containerID(element interface{}) string {
	el := element.(*model.Sample)
	return fmt.Sprintf("%s::%s::%s", el.Metric["namespace"], el.Metric["pod"], el.Metric["container"])
}

func makeCustomPairID(pri, sec string) func(interface{}) string {
	return func(element interface{}) string {
		el := element.(*model.Sample)
		return fmt.Sprintf("%s::%s", el.Metric[model.LabelName(pri)], el.Metric[model.LabelName(sec)])
	}
}

func boundedSample(lower, upper interface{}) types.GomegaMatcher {
	return gstruct.PointTo(gstruct.MatchAllFields(gstruct.Fields{
		// We already check Metric when matching the Id
		"Metric": gstruct.Ignore(),
		"Value":  gomega.And(gomega.BeNumerically(">=", lower), gomega.BeNumerically("<=", upper)),
		"Timestamp": gomega.WithTransform(func(t model.Time) time.Time {
			if t.Unix() <= 0 {
				return time.Now()
			}

			// model.Time is in Milliseconds since epoch
			return time.Unix(0, int64(t)*int64(time.Millisecond))
		},
			gomega.And(
				gomega.BeTemporally(">=", time.Now().Add(-maxStatsAge)),
				// Now() is the test start time, not the match time, so permit a few extra minutes.
				gomega.BeTemporally("<", time.Now().Add(2*time.Minute))),
		),
		"Histogram": gstruct.Ignore(),
	}))
}

func haveKeys(keys ...string) types.GomegaMatcher {
	gomega.ExpectWithOffset(1, keys).ToNot(gomega.BeEmpty())
	matcher := gomega.HaveKey(keys[0])

	if len(keys) == 1 {
		return matcher
	}

	for _, key := range keys[1:] {
		matcher = gomega.And(matcher, gomega.HaveKey(key))
	}

	return matcher
}

func removeMetricsPods(ctx context.Context, f *framework.Framework) {
	ginkgo.By("Deleting test pods")
	var zero int64 = 0
	e2epod.NewPodClient(f).DeleteSync(ctx, pod0, metav1.DeleteOptions{GracePeriodSeconds: &zero}, 10*time.Minute)
	e2epod.NewPodClient(f).DeleteSync(ctx, pod1, metav1.DeleteOptions{GracePeriodSeconds: &zero}, 10*time.Minute)
	if !ginkgo.CurrentSpecReport().Failed() {
		return
	}
	if framework.TestContext.DumpLogsOnFailure {
		e2ekubectl.LogFailedContainers(ctx, f.ClientSet, f.Namespace.Name, framework.Logf)
	}
	ginkgo.By("Recording processes in system cgroups")
	recordSystemCgroupProcesses(ctx)
}
