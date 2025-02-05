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

	"github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubefeatures "k8s.io/kubernetes/pkg/features"
	kubeletconfig "k8s.io/kubernetes/pkg/kubelet/apis/config"
	"k8s.io/kubernetes/test/e2e/framework"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
	"k8s.io/kubernetes/test/e2e/nodefeature"
	imageutils "k8s.io/kubernetes/test/utils/image"
	admissionapi "k8s.io/pod-security-admission/api"
)

var _ = SIGDescribe("UserNamespaces", "[LinuxOnly]", nodefeature.UserNamespacesSupport, framework.WithSerial(), func() {
	f := framework.NewDefaultFramework("user-namespace-off-test")
	f.NamespacePodSecurityLevel = admissionapi.LevelPrivileged
	f.Context("when UserNamespacesSupport=false in the kubelet", func() {
		// Turn off UserNamespacesSupport for this test
		// TODO: once the UserNamespacesSupport feature is removed, this test should be removed too
		tempSetCurrentKubeletConfig(f, func(ctx context.Context, initialConfig *kubeletconfig.KubeletConfiguration) {
			if initialConfig.FeatureGates == nil {
				initialConfig.FeatureGates = make(map[string]bool)
			}
			initialConfig.FeatureGates[string(kubefeatures.UserNamespacesSupport)] = false
		})
		f.It("will fail to create a hostUsers=false pod", func(ctx context.Context) {
			if on, ok := serviceFeatureGates[string(kubefeatures.UserNamespacesSupport)]; !ok || !on {
				e2eskipper.Skipf("services do not have user namespaces on")
			}
			falseVar := false
			podClient := e2epod.NewPodClient(f)
			pod, err := podClient.PodInterface.Create(ctx, &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "userns-pod"},
				Spec: v1.PodSpec{
					Containers: []v1.Container{
						{
							Name:    "test-container-1",
							Image:   imageutils.GetE2EImage(imageutils.BusyBox),
							Command: []string{"/bin/sleep"},
							Args:    []string{"10000"},
						},
					},
					HostUsers: &falseVar,
				},
			}, metav1.CreateOptions{})
			framework.ExpectNoError(err)

			// Pod should stay in pending
			// Events would be a better way to tell this, as we could actually read the event,
			// but history proves events aren't reliable enough to base a test on.
			gomega.Consistently(ctx, func() error {
				p, err := f.ClientSet.CoreV1().Pods(f.Namespace.Name).Get(ctx, pod.Name, metav1.GetOptions{})
				if err != nil {
					return err
				}
				if p.Status.Phase != v1.PodPending {
					return fmt.Errorf("Pod phase isn't pending")
				}
				return nil
			}, 30*time.Second, 5*time.Second).ShouldNot(gomega.HaveOccurred())
		})
	})
})
