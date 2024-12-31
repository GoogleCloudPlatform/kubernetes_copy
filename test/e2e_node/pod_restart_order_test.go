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
	"sort"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	schedulingv1 "k8s.io/api/scheduling/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/kubernetes/pkg/features"
	"k8s.io/kubernetes/pkg/kubelet/apis/config"
	"k8s.io/kubernetes/test/e2e/framework"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	testutils "k8s.io/kubernetes/test/utils"
	imageutils "k8s.io/kubernetes/test/utils/image"
	admissionapi "k8s.io/pod-security-admission/api"
)

var _ = SIGDescribe(framework.WithSerial(), "Pod Restart Order [testFocusHere2]", func() {
	f := framework.NewDefaultFramework("pod-restart-order-serial")
	f.NamespacePodSecurityLevel = admissionapi.LevelPrivileged

	f.Context("restart and get the target node order [testFocusHere2]", func() {

		const (
			podAmount       = int32(5)
			podRetryTimeout = 5 * time.Minute
		)

		tempSetCurrentKubeletConfig(f, func(ctx context.Context, initialConfig *config.KubeletConfiguration) {
			initialConfig.FeatureGates = map[string]bool{
				string(features.PodStartingOrderByPriority): true,
			}
		})

		var podCli *e2epod.PodClient

		ginkgo.BeforeEach(func(ctx context.Context) {
			ginkgo.By("Wait for the node to be ready")
			waitForNodeReady(ctx)
			ginkgo.By("Create priorityclasses")
			for i := range podAmount {
				f.ClientSet.SchedulingV1().PriorityClasses().Create(ctx, getPriorityClassDef(i), metav1.CreateOptions{})
			}
			podCli = e2epod.NewPodClient(f)
		})

		ginkgo.It("Should [testFocusHere2]", func(ctx context.Context) {
			nodeName := getNodeName(ctx, f)
			pods := []*v1.Pod{}
			for i := range podAmount {
				pods = append(pods, getPodWithPriority(fmt.Sprintf("%d", i), nodeName, fmt.Sprintf("%d", i)))
			}
			podCli.CreateBatch(ctx, pods)

			podList, err := f.ClientSet.CoreV1().Pods(f.Namespace.Name).List(ctx, metav1.ListOptions{})
			framework.ExpectNoError(err, "should be able to list pods")
			gomega.Expect(podList.Items).To(gomega.HaveLen(int(podAmount)))
			orderedPodList := podList.Items
			sort.Slice(orderedPodList, func(a, b int) bool {
				return orderedPodList[a].CreationTimestamp.Before(&orderedPodList[b].CreationTimestamp)
			})
			gomega.Expect(orderedPodList).To(gomega.Equal(podList.Items))

			restartKubelet(ctx, true)

			waitForKubeletToStart(ctx, f)

			waitForNodeReady(ctx)

			exec.Command(sleepCommand(120))

			postRestartPods := waitForPodsCondition(ctx, f, int(podAmount), podRetryTimeout, testutils.PodRunningReadyOrSucceeded)

			gomega.Expect(postRestartPods).To(gomega.HaveLen(int(podAmount)))

			gomega.Expect(getRestartCount(*postRestartPods[0])).ToNot(gomega.BeZero()) //Fails, pods have not been restarted

			restartedOrderedPods := postRestartPods
			sort.Slice(restartedOrderedPods, func(a, b int) bool {
				return restartedOrderedPods[a].CreationTimestamp.Before(&restartedOrderedPods[b].CreationTimestamp)
			})
			gomega.Expect(restartedOrderedPods).To(gomega.Equal(postRestartPods))
		})
	})
})

// getRestartCount return the restart count of given pod (total number of its containers restarts).
// Copied over from dashboard
func getRestartCount(pod v1.Pod) int32 {
	var restartCount int32 = 0
	for _, containerStatus := range pod.Status.ContainerStatuses {
		restartCount += containerStatus.RestartCount
	}
	return restartCount
}

func getPriorityClassDef(priority int32) *schedulingv1.PriorityClass {
	prio := &schedulingv1.PriorityClass{
		TypeMeta: metav1.TypeMeta{
			Kind:       "PriorityClass",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: fmt.Sprintf("%d", priority),
		},
		Value:         priority,
		GlobalDefault: false,
		Description:   "Test priority",
	}
	return prio
}

func getPodWithPriority(name string, node string, priority string) *v1.Pod {
	pod := &v1.Pod{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Pod",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name:  name,
					Image: imageutils.GetPauseImageName(),
				},
			},
			PriorityClassName: priority,
			NodeName:          node,
			RestartPolicy:     "Always",
		},
	}
	return pod
}
