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

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/kubernetes/test/e2e/framework"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"

	"github.com/onsi/ginkgo/v2"
)

var _ = SIGDescribe(framework.WithNodeConformance(), "Shortened Grace Period", func() {
	f := framework.NewDefaultFramework("shortened-grace-period")
	var podClient *e2epod.PodClient
	var ns string
	var podName = "test-shortened-grace"
	var ctx = context.Background()
	const (
		gracePeriod      = 10000
		gracePeriodShort = 3
	)

	ginkgo.BeforeEach(func() {
		ns = f.Namespace.Name
		podClient = e2epod.NewPodClient(f)
	})

	ginkgo.It("delete terminating pod with a shorter grace period", func() {
		podClient.CreateSync(ctx, getGracePeriodTestPod(podName, ns, gracePeriod))

		// delete pod
		ginkgo.By("delete a running pod")
		err := podClient.Delete(ctx, podName, *metav1.NewDeleteOptions(gracePeriod))
		framework.ExpectNoError(err, "failed to delete pod with grace period %v", gracePeriod)

		// wait until pod is terminating
		err = e2epod.WaitForPodTerminatingInNamespaceTimeout(ctx, f.ClientSet, podName, ns, 10*time.Second)
		framework.ExpectNoError(err, "failed to wait pod being terminating")
		// wait a few seconds to make sure pre stop hook is running
		time.Sleep(5 * time.Second)

		ginkgo.By("delete the terminating pod with short grace period")
		// delete again with shorter grace period
		err = podClient.Delete(ctx, podName, *metav1.NewDeleteOptions(gracePeriodShort))
		framework.ExpectNoError(err, "failed to delete pod again with grace period %v", gracePeriodShort)

		ginkgo.By("wait the pod being fully removed")
		err = e2epod.WaitForPodNotFoundInNamespace(ctx, f.ClientSet, podName, ns, 30*time.Second)
		framework.ExpectNoError(err, "failed to fully remove pod in 30s ")
	})
})

func getGracePeriodTestPod(name, ns string, gracePeriod int64) *v1.Pod {
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				"test-shortened-grace": "true",
			},
			Namespace: ns,
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name:    name,
					Image:   busyboxImage,
					Command: []string{"/bin/sh"},
					TTY:     true,
					Lifecycle: &v1.Lifecycle{
						PreStop: &v1.LifecycleHandler{
							Exec: &v1.ExecAction{
								Command: []string{"/bin/sh", "-c", "sleep 1000000"},
							},
						},
					},
				},
			},
			TerminationGracePeriodSeconds: &gracePeriod,
		},
	}
	return e2epod.MustMixinRestrictedPodSecurity(pod)
}
