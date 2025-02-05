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
	"os"
	"sync"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/pkg/errors"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/uuid"
	kubeletconfig "k8s.io/kubernetes/pkg/kubelet/apis/config"
	"k8s.io/kubernetes/test/e2e/feature"
	"k8s.io/kubernetes/test/e2e/framework"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	"k8s.io/kubernetes/test/e2e_node/criproxy"
	imageutils "k8s.io/kubernetes/test/utils/image"
	admissionapi "k8s.io/pod-security-admission/api"
)

var (
	httpdImage    = imageutils.GetE2EImage(imageutils.Httpd)
	httpdNewImage = imageutils.GetE2EImage(imageutils.HttpdNew)
)

// This test needs to run in serial to prevent caching of the images by other tests
// and to prevent the wait time of image pulls to be increased by other images
var _ = SIGDescribe("PLEG", feature.CriProxy, framework.WithSerial(), func() {

	f := framework.NewDefaultFramework("pleg-test")
	f.NamespacePodSecurityLevel = admissionapi.LevelPrivileged


	// TODO, probably, the test is about PLEG and should work in Generic PLEG as well as Evented PLEG. 
	ginkgo.Context("PLEG", func() {
		tempSetCurrentKubeletConfig(f, func(ctx context.Context, initialConfig *kubeletconfig.KubeletConfiguration) {
			if initialConfig.FeatureGates == nil {
				initialConfig.FeatureGates = make(map[string]bool)
			}
			initialConfig.FeatureGates["EventedPLEG"] = true
		})
		runAllTests(f)
	})

	// ginkgo.Context("EventedPLEG=false", func() {
	// 	tempSetCurrentKubeletConfig(f, func(ctx context.Context, initialConfig *kubeletconfig.KubeletConfiguration) {
	// 		if initialConfig.FeatureGates == nil {
	// 			initialConfig.FeatureGates = make(map[string]bool)
	// 		}
	// 		initialConfig.FeatureGates["EventedPLEG"] = false
	// 	})
	// 	runAllTests(f)
	// })
})

func runAllTests(f *framework.Framework) {

	ginkgo.BeforeEach(func(ctx context.Context) {
		// TODO skip should follow other cri proxy injected testing
		if err := resetCRIProxyInjector(e2eCriProxy); err != nil {
			ginkgo.Skip("Skip the test since the CRI Proxy is undefined.")
		}
		// cleanup the inject status
		callStatus = make(map[int]chan struct{})
		callStatus2 = make(map[int]chan struct{})
	})

	ginkgo.AfterEach(func(ctx context.Context) {
		err := resetCRIProxyInjector(e2eCriProxy)
		framework.ExpectNoError(err)
	})
	ginkgo.It("should run with empty injector and first init container start timeout or failure", func(ctx context.Context) {
		emptyInjector := func(apiName string) error {
			return nil
		}
		node := getNodeName(ctx, f)

		testpod := &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "testpod-",
				Namespace: f.Namespace.Name,
			},
			Spec: v1.PodSpec{
				InitContainers: []v1.Container{{
					Name:            "init-1",
					Image:           httpdImage,
					ImagePullPolicy: v1.PullAlways,
					RestartPolicy:   &containerRestartPolicyAlways,
				}},
				Containers: []v1.Container{{
					Name:            "testpod",
					Image:           httpdNewImage,
					ImagePullPolicy: v1.PullAlways,
				}},
				NodeName:      node,
				RestartPolicy: v1.RestartPolicyNever,
			},
		}
		podRunningWithInject(f, ctx, testpod, 3*time.Minute, emptyInjector)

		podRunningWithInject(f, ctx, testpod, 3*time.Minute, firstStartContainerErrorInjector)
		podRunningWithInject(f, ctx, testpod, 11*time.Minute, firstStartContainerTimeoutInjector)
	})

	ginkgo.It("should be running finally with first container start timeout or failure", func(ctx context.Context) {
		node := getNodeName(ctx, f)

		testpod := &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "testpod-",
				Namespace: f.Namespace.Name,
			},
			Spec: v1.PodSpec{
				Containers: []v1.Container{{
					Name:            "testpod",
					Image:           httpdImage,
					ImagePullPolicy: v1.PullAlways,
				}},
				NodeName:      node,
				RestartPolicy: v1.RestartPolicyNever,
			},
		}
		podRunningWithInject(f, ctx, testpod, 3*time.Minute, firstStartContainerErrorInjector)

		podRunningWithInject(f, ctx, testpod, 3*time.Minute, firstStartContainerTimeoutInjector)
	})

	ginkgo.It("should be running finally with static pod first container start failure", func(ctx context.Context) {
		var ns, podPath, staticPodName, mirrorPodName string
		ns = f.Namespace.Name
		staticPodName = "graceful-pod-" + string(uuid.NewUUID())
		mirrorPodName = staticPodName + "-" + framework.TestContext.NodeName

		podPath = kubeletCfg.StaticPodPath

		err := addCRIProxyInjector(e2eCriProxy, firstStartContainerErrorInjector)
		framework.ExpectNoError(err)

		ginkgo.By("create the static pod")
		err = createHttpdStaticPod(podPath, staticPodName, ns)
		framework.ExpectNoError(err)

		ginkgo.By("wait for the mirror pod to be running")
		gomega.Eventually(ctx, func(ctx context.Context) error {
			return checkMirrorPodRunning(ctx, f.ClientSet, mirrorPodName, ns)
		}, 3*time.Minute, time.Second*4).Should(gomega.BeNil())

		err = addCRIProxyInjector(e2eCriProxy, firstStartContainerTimeoutInjector)
		framework.ExpectNoError(err)

		staticPodName = "graceful-pod-" + string(uuid.NewUUID())
		mirrorPodName = staticPodName + "-" + framework.TestContext.NodeName
		ginkgo.By("create the static pod")
		err = createHttpdStaticPod(podPath, staticPodName, ns)
		framework.ExpectNoError(err)

		ginkgo.By("wait for the mirror pod to be running")
		gomega.Eventually(ctx, func(ctx context.Context) error {
			return checkMirrorPodRunning(ctx, f.ClientSet, mirrorPodName, ns)
		}, 3*time.Minute, time.Second*4).Should(gomega.BeNil())
	})
}

func createHttpdStaticPod(dir, name, namespace string) error {
	template := `
apiVersion: v1
kind: Pod
metadata:
  name: %s
  namespace: %s
spec:
  containers:
  - name: m-test
    image: %s
    ports:
    - containerPort: 80
`
	file := staticPodPath(dir, name, namespace)
	podYaml := fmt.Sprintf(template, name, namespace, imageutils.GetE2EImage(imageutils.Httpd))

	f, err := os.OpenFile(file, os.O_RDWR|os.O_TRUNC|os.O_CREATE, 0666)
	if err != nil {
		return err
	}
	defer func() {
		closeErr := f.Close()
		if closeErr != nil && err == nil {
			err = closeErr
		}
	}()

	_, err = f.WriteString(podYaml)
	framework.Logf("has written %v", file)
	return err
}

func podRunningWithInject(f *framework.Framework, ctx context.Context, testpod *v1.Pod, timeout time.Duration, injector func(apiName string) error) {
	err := addCRIProxyInjector(e2eCriProxy, injector)
	framework.ExpectNoError(err)
	pod := e2epod.NewPodClient(f).Create(ctx, testpod)
	err = e2epod.WaitForPodCondition(ctx, f.ClientSet, f.Namespace.Name, pod.Name, "Running", timeout, func(pod *v1.Pod) (bool, error) {
		if pod.Status.Phase == v1.PodRunning {
			return true, nil
		}
		return false, nil
	})
	framework.ExpectNoError(err)

	e2epod.NewPodClient(f).Delete(ctx, testpod.Name, metav1.DeleteOptions{})
}

var (
	mu, mu2     sync.Mutex
	callCh      = make(chan struct{})
	callStatus  = make(map[int]chan struct{})
	callStatus2 = make(map[int]chan struct{})

	// inject a start container failure
	firstStartContainerErrorInjector = func(apiName string) error {
		if apiName == criproxy.StartContainer {
			mu.Lock()
			callID := len(callStatus)
			callStatus[callID] = callCh
			mu.Unlock()
			if callID == 0 {
				// stuck the first container start
				return errors.New("stuck the first container start")
			}
		}
		return nil
	}

	// inject a start container timeout
	firstStartContainerTimeoutInjector = func(apiName string) error {
		if apiName == criproxy.StartContainer {
			mu2.Lock()
			callID := len(callStatus2)
			callStatus2[callID] = callCh
			mu2.Unlock()
			if callID == 0 {
				time.Sleep(3 * time.Minute)
			}
		}
		return nil
	}
)
