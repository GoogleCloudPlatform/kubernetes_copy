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
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/kubernetes/test/e2e/framework"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	admissionapi "k8s.io/pod-security-admission/api"
)

var _ = SIGDescribe("termination Message", func() {
	f := framework.NewDefaultFramework("termination-message-pods")
	f.NamespacePodSecurityLevel = admissionapi.LevelBaseline

	ginkgo.It("pod should evict after the termination-log exceeds the ephemeral storage limit", func(ctx context.Context) {
		pod := &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: "pod1",
			},
			Spec: v1.PodSpec{
				Containers: []v1.Container{
					{
						Name:  "container1",
						Image: busyboxImage,
						Command: []string{
							"/bin/sh",
						},
						Args: []string{
							"-c",
							// Wait 15 seconds before writing the message so that we can observe the pod entering the ready state.
							`sleep 15 && yes $(printf 'Hello world!!!!\n%.0s' $(seq 1 $((1024/16)))) | dd bs=1024 count=$((200*1024)) > /dev/termination-log
while sleep 3600; do
  true
done`,
						},
						Resources: v1.ResourceRequirements{
							Limits: v1.ResourceList{
								v1.ResourceEphemeralStorage: resource.MustParse("50Mi"),
							},
						},
					},
				},
			},
		}
		client := e2epod.NewPodClient(f)
		pod = client.Create(context.TODO(), pod)
		gomega.Expect(pod.Spec.NodeName).ToNot(gomega.BeEmpty())
		ginkgo.DeferCleanup(e2epod.NewPodClient(f).DeleteSync, "pod1", metav1.DeleteOptions{}, time.Minute)

		// Make sure that the pod is in the ready state before evicting it.
		gomega.Eventually(ctx, func() bool {
			pod, _ = client.Get(context.TODO(), pod.Name, metav1.GetOptions{})
			for _, c := range pod.Status.Conditions {
				if c.Type == v1.ContainersReady && c.Status == v1.ConditionTrue {
					return true
				}
			}
			return false
		}, 20*time.Second, 1*time.Second).Should(gomega.BeTrueBecause("the pod should be in the ready state before being evicted"))

		gomega.Eventually(ctx, func() bool {
			pod, _ = client.Get(context.TODO(), pod.Name, metav1.GetOptions{})
			return pod.Status.Reason == "Evicted"
		}, 1*time.Minute, 1*time.Second).Should(gomega.BeTrueBecause("the pod should be evicted"))
	})
})
