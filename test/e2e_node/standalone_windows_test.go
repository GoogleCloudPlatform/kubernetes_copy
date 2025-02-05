/*
Copyright 2025 The Kubernetes Authors.

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
	"os"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/cli-runtime/pkg/printers"
	"k8s.io/kubernetes/test/e2e/feature"
	"k8s.io/kubernetes/test/e2e/framework"
	imageutils "k8s.io/kubernetes/test/utils/image"
	admissionapi "k8s.io/pod-security-admission/api"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	testutils "k8s.io/kubernetes/test/utils"
)

var _ = SIGDescribe(feature.Windows, func() {
	f := framework.NewDefaultFramework("static-pod")
	f.NamespacePodSecurityLevel = admissionapi.LevelBaseline
	ginkgo.Context("when creating a windows static pod", func() {
		var ns, podPath, staticPodName string

		ginkgo.It("the pod should be running", func(ctx context.Context) {
			ns = f.Namespace.Name
			staticPodName = "static-pod-" + string(uuid.NewUUID())
			podPath = kubeletCfg.StaticPodPath
			err := createWindowsBasicStaticPod(podPath, staticPodName, ns)
			framework.ExpectNoError(err)

			gomega.Eventually(ctx, func(ctx context.Context) error {
				pod, err := getPodFromStandaloneKubelet(ctx, ns, staticPodName)
				if err != nil {
					return fmt.Errorf("error getting pod(%v/%v) from standalone kubelet: %v", ns, staticPodName, err)
				}

				isReady, err := testutils.PodRunningReady(pod)
				if err != nil {
					return fmt.Errorf("error checking if pod (%v/%v) is running ready: %v", ns, staticPodName, err)
				}
				if !isReady {
					return fmt.Errorf("pod (%v/%v) is not running", ns, staticPodName)
				}
				return nil
			}, f.Timeouts.PodStart, time.Second*5).Should(gomega.BeNil())
		})

		ginkgo.AfterEach(func(ctx context.Context) {
			ginkgo.By(fmt.Sprintf("delete the static pod (%v/%v)", ns, staticPodName))
			err := deleteStaticPod(podPath, staticPodName, ns)
			framework.ExpectNoError(err)

			ginkgo.By(fmt.Sprintf("wait for pod to disappear (%v/%v)", ns, staticPodName))
			gomega.Eventually(ctx, func(ctx context.Context) error {
				_, err := getPodFromStandaloneKubelet(ctx, ns, staticPodName)

				if apierrors.IsNotFound(err) {
					return nil
				}
				return fmt.Errorf("pod (%v/%v) still exists", ns, staticPodName)
			}).Should(gomega.Succeed())
		})
	})
})

func createWindowsBasicStaticPod(dir, name, namespace string) error {
	podSpec := &v1.Pod{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Pod",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: v1.PodSpec{
			RestartPolicy: v1.RestartPolicyAlways,
			Containers: []v1.Container{
				{
					Name:  "regular1",
					Image: imageutils.GetE2EImage(imageutils.BusyBox),
					Command: []string{
						// "/bin/sh", "-c", "touch /tmp/healthy; sleep 10000",
						"powershell", "-c", "touch /tmp/healthy; sleep 10000",
					},
					Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{
							// v1.ResourceMemory: resource.MustParse("15Mi"),
							v1.ResourceMemory: resource.MustParse("100Mi"),
						},
						Limits: v1.ResourceList{
							// v1.ResourceMemory: resource.MustParse("15Mi"),
							v1.ResourceMemory: resource.MustParse("100Mi"),
						},
					},
					ReadinessProbe: &v1.Probe{
						InitialDelaySeconds: 2,
						TimeoutSeconds:      2,
						ProbeHandler: v1.ProbeHandler{
							Exec: &v1.ExecAction{
								// Command: []string{"/bin/sh", "-c", "cat /tmp/healthy"},
								Command: []string{"powershell", "-c", "cat /tmp/healthy"},
							},
						},
					},
				},
			},
		},
	}

	file := staticPodPath(dir, name, namespace)
	f, err := os.OpenFile(file, os.O_RDWR|os.O_TRUNC|os.O_CREATE, 0666)
	if err != nil {
		return err
	}
	defer f.Close()

	y := printers.YAMLPrinter{}
	y.PrintObj(podSpec, f)

	return nil
}
