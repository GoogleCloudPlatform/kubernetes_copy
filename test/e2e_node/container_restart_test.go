//go:build linux
// +build linux

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
	"time"

	podv1util "k8s.io/kubernetes/pkg/api/v1/pod"
	imageutils "k8s.io/kubernetes/test/utils/image"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/pkg/errors"
	kubeletconfig "k8s.io/kubernetes/pkg/kubelet/apis/config"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/kubernetes/test/e2e/feature"
	"k8s.io/kubernetes/test/e2e/framework"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	admissionapi "k8s.io/pod-security-admission/api"
)

const containerName = "restarts"

var _ = SIGDescribe("Container Restart", feature.CriProxy, framework.WithSerial(), func() {
	f := framework.NewDefaultFramework("container-restart")
	f.NamespacePodSecurityLevel = admissionapi.LevelPrivileged

	ginkgo.Context("Container restart backs off", func() {

		ginkgo.BeforeEach(func() {
			if err := resetCRIProxyInjector(e2eCriProxy); err != nil {
				ginkgo.Skip("Skip the test since the CRI Proxy is undefined.")
			}
			ginkgo.DeferCleanup(func() error {
				return resetCRIProxyInjector(e2eCriProxy)
			})
		})

		ginkgo.It("Container restart backs off.", func(ctx context.Context) {
			// 0s, 0s, 10s, 30s, 70s, 150s, 310s
			doTest(ctx, f, 3, containerName, 7)
		})
	})

	ginkgo.Context("Alternate container restart backs off as expected", func() {

		tempSetCurrentKubeletConfig(f, func(ctx context.Context, initialConfig *kubeletconfig.KubeletConfiguration) {
			initialConfig.CrashLoopBackOff.MaxContainerRestartPeriod = &metav1.Duration{Duration: time.Duration(30 * time.Second)}
			initialConfig.FeatureGates = map[string]bool{"KubeletCrashLoopBackOffMax": true}
		})

		ginkgo.BeforeEach(func() {
			if err := resetCRIProxyInjector(e2eCriProxy); err != nil {
				ginkgo.Skip("Skip the test since the CRI Proxy is undefined.")
			}
			ginkgo.DeferCleanup(func() error {
				return resetCRIProxyInjector(e2eCriProxy)
			})
		})

		ginkgo.It("Alternate restart backs off.", func(ctx context.Context) {
			// 0s, 0s, 10s, 30s, 60s, 90s, 120s, 150s, 180s, 210s, 240s, 270s, 300s
			doTest(ctx, f, 3, containerName, 13)
		})
	})
})

func doTest(ctx context.Context, f *framework.Framework, targetRestarts int, containerName string, maxRestarts int) {

	pod := e2epod.NewPodClient(f).Create(ctx, newFailAlwaysPod())
	podErr := e2epod.WaitForPodContainerToFail(ctx, f.ClientSet, f.Namespace.Name, pod.Name, 0, "CrashLoopBackOff", 1*time.Minute)
	gomega.Expect(podErr).To(gomega.HaveOccurred())

	// Hard wait 30 seconds for targetRestarts in the best case; longer timeout later will handle if infra was slow.
	time.Sleep(30 * time.Second)
	podErr = waitForContainerRestartedNTimes(ctx, f, f.Namespace.Name, pod.Name, containerName, 5*time.Minute, targetRestarts)
	gomega.Expect(podErr).ShouldNot(gomega.HaveOccurred(), "Expected container to repeatedly back off container failures")

	r, err := extractObservedBackoff(ctx, f, pod.Name, containerName)
	framework.ExpectNoError(err)

	gomega.Expect(r).Should(gomega.BeNumerically("<=", maxRestarts))
}

func extractObservedBackoff(ctx context.Context, f *framework.Framework, podName string, containerName string) (int32, error) {
	var r int32
	pod, err := f.ClientSet.CoreV1().Pods(f.Namespace.Name).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		return r, err
	}
	for _, statuses := range [][]v1.ContainerStatus{pod.Status.ContainerStatuses, pod.Status.InitContainerStatuses, pod.Status.EphemeralContainerStatuses} {
		for _, cs := range statuses {
			if cs.Name == containerName {
				return cs.RestartCount, nil
			}
		}
	}
	return r, errors.Errorf("Could not find container status for container %s in pod %s", containerName, podName)
}

func newFailAlwaysPod() *v1.Pod {
	podName := "container-restart" + string(uuid.NewUUID())
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: podName,
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name:            containerName,
					Image:           imageutils.GetE2EImage(imageutils.BusyBox),
					ImagePullPolicy: v1.PullIfNotPresent,
				},
			},
		},
	}
	return pod
}

func waitForContainerRestartedNTimes(ctx context.Context, f *framework.Framework, namespace string, podName string, containerName string, timeout time.Duration, target int) error {
	conditionDesc := fmt.Sprintf("A container in pod %s restarted at least %d times", podName, target)
	return e2epod.WaitForPodCondition(ctx, f.ClientSet, namespace, podName, conditionDesc, timeout, func(pod *v1.Pod) (bool, error) {
		cs, found := podv1util.GetContainerStatus(pod.Status.ContainerStatuses, containerName)
		if !found {
			return false, fmt.Errorf("could not find container %s in  pod %s", containerName, podName)
		}
		return cs.RestartCount >= int32(target), nil
	})
}
