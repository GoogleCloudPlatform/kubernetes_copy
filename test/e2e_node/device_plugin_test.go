/*
Copyright 2017 The Kubernetes Authors.

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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/onsi/gomega/gcustom"
	"github.com/onsi/gomega/types"

	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
	kubeletdevicepluginv1beta1 "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
	admissionapi "k8s.io/pod-security-admission/api"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/kubectl/pkg/util/podutils"
	kubeletpodresourcesv1 "k8s.io/kubelet/pkg/apis/podresources/v1"
	kubeletpodresourcesv1alpha1 "k8s.io/kubelet/pkg/apis/podresources/v1alpha1"
	"k8s.io/kubernetes/pkg/features"
	"k8s.io/kubernetes/test/e2e/feature"
	"k8s.io/kubernetes/test/e2e/framework"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2etestfiles "k8s.io/kubernetes/test/e2e/framework/testfiles"
)

var (
	appsScheme = runtime.NewScheme()
	appsCodecs = serializer.NewCodecFactory(appsScheme)
)

// Serial because the test restarts Kubelet
var _ = SIGDescribe("Device Plugin", framework.WithSerial(), feature.DevicePlugin, func() {
	f := framework.NewDefaultFramework("device-plugin-errors")
	f.NamespacePodSecurityLevel = admissionapi.LevelPrivileged
	testDevicePlugin(f, kubeletdevicepluginv1beta1.DevicePluginPath)
	testDevicePluginNodeReboot(f, kubeletdevicepluginv1beta1.DevicePluginPath)
})

// readDaemonSetV1OrDie reads daemonset object from bytes. Panics on error.
func readDaemonSetV1OrDie(objBytes []byte) *appsv1.DaemonSet {
	appsv1.AddToScheme(appsScheme)
	requiredObj, err := runtime.Decode(appsCodecs.UniversalDecoder(appsv1.SchemeGroupVersion), objBytes)
	if err != nil {
		panic(err)
	}
	return requiredObj.(*appsv1.DaemonSet)
}

const (
	// TODO(vikasc): Instead of hard-coding number of devices, provide number of devices in the sample-device-plugin using configmap
	// and then use the same here
	expectedSampleDevsAmount int64 = 2

	// This is the sleep interval specified in the command executed in the pod to ensure container is running "forever" in the test timescale
	sleepIntervalForever string = "24h"

	// This is the sleep interval specified in the command executed in the pod so that container is restarted within the expected test run time
	sleepIntervalWithRestart string = "60s"

	// This is the sleep interval specified in the command executed in the pod so that container is restarted within the expected test run time
	sleepIntervalToCompletion string = "5s"
)

func testDevicePlugin(f *framework.Framework, pluginSockDir string) {
	pluginSockDir = filepath.Join(pluginSockDir) + "/"

	type ResourceValue struct {
		Allocatable int
		Capacity    int
	}

	devicePluginGracefulTimeout := 5 * time.Minute // see endpointStopGracePeriod in pkg/kubelet/cm/devicemanager/types.go

	var getNodeResourceValues = func(ctx context.Context, resourceName string) ResourceValue {
		ginkgo.GinkgoHelper()
		node := getLocalNode(ctx, f)

		// -1 represents that the resource is not found
		result := ResourceValue{
			Allocatable: -1,
			Capacity:    -1,
		}

		for key, val := range node.Status.Capacity {
			resource := string(key)
			if resource == resourceName {
				result.Capacity = int(val.Value())
				break
			}
		}

		for key, val := range node.Status.Allocatable {
			resource := string(key)
			if resource == resourceName {
				result.Allocatable = int(val.Value())
				break
			}
		}

		return result
	}

	f.Context("DevicePlugin", f.WithSerial(), f.WithDisruptive(), func() {
		var devicePluginPod, dptemplate *v1.Pod
		var v1alphaPodResources *kubeletpodresourcesv1alpha1.ListPodResourcesResponse
		var v1PodResources *kubeletpodresourcesv1.ListPodResourcesResponse
		var err error

		ginkgo.BeforeEach(func(ctx context.Context) {
			ginkgo.By("Wait for node to be ready")
			gomega.Eventually(ctx, func(ctx context.Context) bool {
				nodes, err := e2enode.TotalReady(ctx, f.ClientSet)
				framework.ExpectNoError(err)
				return nodes == 1
			}, time.Minute, time.Second).Should(gomega.BeTrueBecause("expected node to be ready"))

			// Before we run the device plugin test, we need to ensure
			// that the cluster is in a clean state and there are no
			// pods running on this node.
			// This is done in a gomega.Eventually with retries since a prior test in a different test suite could've run and the deletion of it's resources may still be in progress.
			// xref: https://issue.k8s.io/115381
			gomega.Eventually(ctx, func(ctx context.Context) error {
				v1alphaPodResources, err = getV1alpha1NodeDevices(ctx)
				if err != nil {
					return fmt.Errorf("failed to get node local podresources by accessing the (v1alpha) podresources API endpoint: %v", err)
				}

				v1PodResources, err = getV1NodeDevices(ctx)
				if err != nil {
					return fmt.Errorf("failed to get node local podresources by accessing the (v1) podresources API endpoint: %v", err)
				}

				if len(v1alphaPodResources.PodResources) > 0 {
					return fmt.Errorf("expected v1alpha pod resources to be empty, but got non-empty resources: %+v", v1alphaPodResources.PodResources)
				}

				if len(v1PodResources.PodResources) > 0 {
					return fmt.Errorf("expected v1 pod resources to be empty, but got non-empty resources: %+v", v1PodResources.PodResources)
				}
				return nil
			}, f.Timeouts.PodDelete, f.Timeouts.Poll).Should(gomega.Succeed())

			ginkgo.By("Scheduling a sample device plugin pod")
			dp := getSampleDevicePluginPod(pluginSockDir)
			dptemplate = dp.DeepCopy()
			devicePluginPod = e2epod.NewPodClient(f).CreateSync(ctx, dp)

			ginkgo.By("Waiting for devices to become available on the local node")
			gomega.Eventually(ctx, func(ctx context.Context) bool {
				node, ready := getLocalTestNode(ctx, f)
				return ready && CountSampleDeviceCapacity(node) > 0
			}, 5*time.Minute, framework.Poll).Should(gomega.BeTrueBecause("expected devices to be available on local node"))
			framework.Logf("Successfully created device plugin pod")

			ginkgo.By(fmt.Sprintf("Waiting for the resource exported by the sample device plugin to become available on the local node (instances: %d)", expectedSampleDevsAmount))
			gomega.Eventually(ctx, func(ctx context.Context) bool {
				node, ready := getLocalTestNode(ctx, f)
				return ready &&
					CountSampleDeviceCapacity(node) == expectedSampleDevsAmount &&
					CountSampleDeviceAllocatable(node) == expectedSampleDevsAmount
			}, 30*time.Second, framework.Poll).Should(gomega.BeTrueBecause("expected resource to be available on local node"))
		})

		ginkgo.AfterEach(func(ctx context.Context) {
			ginkgo.By("Deleting the device plugin pod")
			e2epod.NewPodClient(f).DeleteSync(ctx, devicePluginPod.Name, metav1.DeleteOptions{}, f.Timeouts.PodDelete)

			ginkgo.By("Deleting any Pods created by the test")
			l, err := e2epod.NewPodClient(f).List(ctx, metav1.ListOptions{})
			framework.ExpectNoError(err)
			for _, p := range l.Items {
				if p.Namespace != f.Namespace.Name {
					continue
				}

				framework.Logf("Deleting pod: %s", p.Name)
				e2epod.NewPodClient(f).DeleteSync(ctx, p.Name, metav1.DeleteOptions{}, f.Timeouts.PodDelete)
			}

			restartKubelet(ctx, true)

			ginkgo.By("Waiting for devices to become unavailable on the local node")
			gomega.Eventually(ctx, func(ctx context.Context) bool {
				node, ready := getLocalTestNode(ctx, f)
				return ready && CountSampleDeviceCapacity(node) <= 0
			}, 5*time.Minute, framework.Poll).Should(gomega.BeTrueBecause("expected devices to be unavailable on local node"))

			ginkgo.By("devices now unavailable on the local node")
		})

		ginkgo.It("Can schedule a pod that requires a device", func(ctx context.Context) {
			podRECMD := fmt.Sprintf("devs=$(ls /tmp/ | egrep '^Dev-[0-9]+$') && echo stub devices: $devs && sleep %s", sleepIntervalWithRestart)
			pod1 := e2epod.NewPodClient(f).CreateSync(ctx, makeBusyboxPod(SampleDeviceResourceName, podRECMD))
			deviceIDRE := "stub devices: (Dev-[0-9]+)"
			devID1, err := parseLog(ctx, f, pod1.Name, pod1.Name, deviceIDRE)
			framework.ExpectNoError(err, "getting logs for pod %q", pod1.Name)
			gomega.Expect(devID1).To(gomega.Not(gomega.Equal("")), "pod1 requested a device but started successfully without")

			v1alphaPodResources, err = getV1alpha1NodeDevices(ctx)
			framework.ExpectNoError(err)

			v1PodResources, err = getV1NodeDevices(ctx)
			framework.ExpectNoError(err)

			framework.Logf("v1alphaPodResources.PodResources:%+v\n", v1alphaPodResources.PodResources)
			framework.Logf("v1PodResources.PodResources:%+v\n", v1PodResources.PodResources)
			framework.Logf("len(v1alphaPodResources.PodResources):%+v", len(v1alphaPodResources.PodResources))
			framework.Logf("len(v1PodResources.PodResources):%+v", len(v1PodResources.PodResources))

			gomega.Expect(v1alphaPodResources.PodResources).To(gomega.HaveLen(2))
			gomega.Expect(v1PodResources.PodResources).To(gomega.HaveLen(2))

			var v1alphaResourcesForOurPod *kubeletpodresourcesv1alpha1.PodResources
			for _, res := range v1alphaPodResources.GetPodResources() {
				if res.Name == pod1.Name {
					v1alphaResourcesForOurPod = res
				}
			}

			var v1ResourcesForOurPod *kubeletpodresourcesv1.PodResources
			for _, res := range v1PodResources.GetPodResources() {
				if res.Name == pod1.Name {
					v1ResourcesForOurPod = res
				}
			}

			gomega.Expect(v1alphaResourcesForOurPod).NotTo(gomega.BeNil())
			gomega.Expect(v1ResourcesForOurPod).NotTo(gomega.BeNil())

			gomega.Expect(v1alphaResourcesForOurPod.Name).To(gomega.Equal(pod1.Name))
			gomega.Expect(v1ResourcesForOurPod.Name).To(gomega.Equal(pod1.Name))

			gomega.Expect(v1alphaResourcesForOurPod.Namespace).To(gomega.Equal(pod1.Namespace))
			gomega.Expect(v1ResourcesForOurPod.Namespace).To(gomega.Equal(pod1.Namespace))

			gomega.Expect(v1alphaResourcesForOurPod.Containers).To(gomega.HaveLen(1))
			gomega.Expect(v1ResourcesForOurPod.Containers).To(gomega.HaveLen(1))

			gomega.Expect(v1alphaResourcesForOurPod.Containers[0].Name).To(gomega.Equal(pod1.Spec.Containers[0].Name))
			gomega.Expect(v1ResourcesForOurPod.Containers[0].Name).To(gomega.Equal(pod1.Spec.Containers[0].Name))

			gomega.Expect(v1alphaResourcesForOurPod.Containers[0].Devices).To(gomega.HaveLen(1))
			gomega.Expect(v1ResourcesForOurPod.Containers[0].Devices).To(gomega.HaveLen(1))

			gomega.Expect(v1alphaResourcesForOurPod.Containers[0].Devices[0].ResourceName).To(gomega.Equal(SampleDeviceResourceName))
			gomega.Expect(v1ResourcesForOurPod.Containers[0].Devices[0].ResourceName).To(gomega.Equal(SampleDeviceResourceName))

			gomega.Expect(v1alphaResourcesForOurPod.Containers[0].Devices[0].DeviceIds).To(gomega.HaveLen(1))
			gomega.Expect(v1ResourcesForOurPod.Containers[0].Devices[0].DeviceIds).To(gomega.HaveLen(1))
		})

		f.It("can make a CDI device accessible in a container", feature.DevicePluginCDIDevices, func(ctx context.Context) {
			// check if CDI_DEVICE env variable is set
			// and only one correspondent device node /tmp/<CDI_DEVICE> is available inside a container
			podObj := makeBusyboxPod(SampleDeviceResourceName, "[ $(ls /tmp/CDI-Dev-[1,2] | wc -l) -eq 1 -a -b /tmp/$CDI_DEVICE ]")
			podObj.Spec.RestartPolicy = v1.RestartPolicyNever
			pod := e2epod.NewPodClient(f).Create(ctx, podObj)
			framework.ExpectNoError(e2epod.WaitForPodSuccessInNamespace(ctx, f.ClientSet, pod.Name, pod.Namespace))
		})

		// simulate container restart, while all other involved components (kubelet, device plugin) stay stable. To do so, in the container
		// entry point we sleep for a limited and short period of time. The device assignment should be kept and be stable across the container
		// restarts. For the sake of brevity we however check just the fist restart.
		ginkgo.It("Keeps device plugin assignments across pod restarts (no kubelet restart, no device plugin restart)", func(ctx context.Context) {
			podRECMD := fmt.Sprintf("devs=$(ls /tmp/ | egrep '^Dev-[0-9]+$') && echo stub devices: $devs && sleep %s", sleepIntervalWithRestart)
			pod1 := e2epod.NewPodClient(f).CreateSync(ctx, makeBusyboxPod(SampleDeviceResourceName, podRECMD))
			deviceIDRE := "stub devices: (Dev-[0-9]+)"
			devID1, err := parseLog(ctx, f, pod1.Name, pod1.Name, deviceIDRE)
			framework.ExpectNoError(err, "getting logs for pod %q", pod1.Name)
			gomega.Expect(devID1).To(gomega.Not(gomega.Equal("")), "pod1 requested a device but started successfully without")

			pod1, err = e2epod.NewPodClient(f).Get(ctx, pod1.Name, metav1.GetOptions{})
			framework.ExpectNoError(err)

			ginkgo.By("Waiting for container to restart")
			ensurePodContainerRestart(ctx, f, pod1.Name, pod1.Name)

			// check from the device assignment is preserved and stable from perspective of the container
			ginkgo.By("Confirming that after a container restart, fake-device assignment is kept")
			devIDRestart1, err := parseLog(ctx, f, pod1.Name, pod1.Name, deviceIDRE)
			framework.ExpectNoError(err, "getting logs for pod %q", pod1.Name)
			gomega.Expect(devIDRestart1).To(gomega.Equal(devID1))

			// crosscheck from the device assignment is preserved and stable from perspective of the kubelet.
			// needs to match the container perspective.
			ginkgo.By("Verifying the device assignment after container restart using podresources API")
			v1PodResources, err = getV1NodeDevices(ctx)
			if err != nil {
				framework.ExpectNoError(err, "getting pod resources assignment after pod restart")
			}
			err, _ = checkPodResourcesAssignment(v1PodResources, pod1.Namespace, pod1.Name, pod1.Spec.Containers[0].Name, SampleDeviceResourceName, []string{devID1})
			framework.ExpectNoError(err, "inconsistent device assignment after pod restart")

			ginkgo.By("Creating another pod")
			pod2 := e2epod.NewPodClient(f).CreateSync(ctx, makeBusyboxPod(SampleDeviceResourceName, podRECMD))
			err = e2epod.WaitTimeoutForPodRunningInNamespace(ctx, f.ClientSet, pod2.Name, f.Namespace.Name, 1*time.Minute)
			framework.ExpectNoError(err)

			ginkgo.By("Checking that pod got a fake device")
			devID2, err := parseLog(ctx, f, pod2.Name, pod2.Name, deviceIDRE)
			framework.ExpectNoError(err, "getting logs for pod %q", pod2.Name)

			gomega.Expect(devID2).To(gomega.Not(gomega.Equal("")), "pod2 requested a device but started successfully without")

			ginkgo.By("Verifying the device assignment after extra container start using podresources API")
			v1PodResources, err = getV1NodeDevices(ctx)
			if err != nil {
				framework.ExpectNoError(err, "getting pod resources assignment after pod restart")
			}
			err, _ = checkPodResourcesAssignment(v1PodResources, pod1.Namespace, pod1.Name, pod1.Spec.Containers[0].Name, SampleDeviceResourceName, []string{devID1})
			framework.ExpectNoError(err, "inconsistent device assignment after extra container restart - pod1")
			err, _ = checkPodResourcesAssignment(v1PodResources, pod2.Namespace, pod2.Name, pod2.Spec.Containers[0].Name, SampleDeviceResourceName, []string{devID2})
			framework.ExpectNoError(err, "inconsistent device assignment after extra container restart - pod2")
		})

		// simulate kubelet restart. A compliant device plugin is expected to re-register, while the pod and the container stays running.
		// The flow with buggy or slow device plugin is deferred to another test.
		// The device assignment should be kept and be stable across the kubelet restart, because it's the kubelet which performs the device allocation,
		// and both the device plugin and the actual consumer (container) are stable.
		ginkgo.It("Keeps device plugin assignments across kubelet restarts (no pod restart, no device plugin restart)", func(ctx context.Context) {
			podRECMD := fmt.Sprintf("devs=$(ls /tmp/ | egrep '^Dev-[0-9]+$') && echo stub devices: $devs && sleep %s", sleepIntervalForever)
			pod1 := e2epod.NewPodClient(f).CreateSync(ctx, makeBusyboxPod(SampleDeviceResourceName, podRECMD))
			deviceIDRE := "stub devices: (Dev-[0-9]+)"
			devID1, err := parseLog(ctx, f, pod1.Name, pod1.Name, deviceIDRE)
			framework.ExpectNoError(err, "getting logs for pod %q", pod1.Name)
			gomega.Expect(devID1).To(gomega.Not(gomega.Equal("")), "pod1 requested a device but started successfully without")

			pod1, err = e2epod.NewPodClient(f).Get(ctx, pod1.Name, metav1.GetOptions{})
			framework.ExpectNoError(err)
			framework.Logf("testing pod: pre-restart  UID=%s namespace=%s name=%s ready=%v", pod1.UID, pod1.Namespace, pod1.Name, podutils.IsPodReady(pod1))

			ginkgo.By("Restarting Kubelet")
			restartKubelet(ctx, true)

			ginkgo.By("Wait for node to be ready again")
			e2enode.WaitForAllNodesSchedulable(ctx, f.ClientSet, 5*time.Minute)

			ginkgo.By("Waiting for resource to become available on the local node after restart")
			gomega.Eventually(ctx, func() bool {
				node, ready := getLocalTestNode(ctx, f)
				return ready &&
					CountSampleDeviceCapacity(node) == expectedSampleDevsAmount &&
					CountSampleDeviceAllocatable(node) == expectedSampleDevsAmount
			}, 30*time.Second, framework.Poll).Should(gomega.BeTrueBecause("expected resource to be available after restart"))

			ginkgo.By("Checking the same instance of the pod is still running")
			gomega.Eventually(ctx, getPodByName).
				WithArguments(f, pod1.Name).
				WithTimeout(time.Minute).
				Should(BeTheSamePodStillRunning(pod1),
					"the same pod instance not running across kubelet restarts, workload should not be perturbed by kubelet restarts")

			pod2, err := e2epod.NewPodClient(f).Get(ctx, pod1.Name, metav1.GetOptions{})
			framework.ExpectNoError(err)
			framework.Logf("testing pod: post-restart UID=%s namespace=%s name=%s ready=%v", pod2.UID, pod2.Namespace, pod2.Name, podutils.IsPodReady(pod2))

			// crosscheck from the device assignment is preserved and stable from perspective of the kubelet.
			// note we don't check again the logs of the container: the check is done at startup, the container
			// never restarted (runs "forever" from this test timescale perspective) hence re-doing this check
			// is useless.
			ginkgo.By("Verifying the device assignment after kubelet restart using podresources API")
			gomega.Eventually(ctx, func() error {
				v1PodResources, err = getV1NodeDevices(ctx)
				return err
			}, 30*time.Second, framework.Poll).ShouldNot(gomega.HaveOccurred(), "cannot fetch the compute resource assignment after kubelet restart")

			err, _ = checkPodResourcesAssignment(v1PodResources, pod2.Namespace, pod2.Name, pod2.Spec.Containers[0].Name, SampleDeviceResourceName, []string{devID1})
			framework.ExpectNoError(err, "inconsistent device assignment after pod restart")
		})

		// simulate kubelet and container restart, *but not* device plugin restart.
		// The device assignment should be kept and be stable across the kubelet and container restart, because it's the kubelet which
		// performs the device allocation, and both the device plugin is stable.
		ginkgo.It("Keeps device plugin assignments across pod and kubelet restarts (no device plugin restart)", func(ctx context.Context) {
			podRECMD := fmt.Sprintf("devs=$(ls /tmp/ | egrep '^Dev-[0-9]+$') && echo stub devices: $devs && sleep %s", sleepIntervalWithRestart)
			pod1 := e2epod.NewPodClient(f).CreateSync(ctx, makeBusyboxPod(SampleDeviceResourceName, podRECMD))
			deviceIDRE := "stub devices: (Dev-[0-9]+)"
			devID1, err := parseLog(ctx, f, pod1.Name, pod1.Name, deviceIDRE)
			framework.ExpectNoError(err, "getting logs for pod %q", pod1.Name)

			gomega.Expect(devID1).To(gomega.Not(gomega.Equal("")), "pod1 requested a device but started successfully without")

			pod1, err = e2epod.NewPodClient(f).Get(ctx, pod1.Name, metav1.GetOptions{})
			framework.ExpectNoError(err)

			ginkgo.By("Wait for node to be ready again")
			e2enode.WaitForAllNodesSchedulable(ctx, f.ClientSet, 5*time.Minute)

			ginkgo.By("Waiting for container to restart")
			ensurePodContainerRestart(ctx, f, pod1.Name, pod1.Name)

			ginkgo.By("Confirming that after a container restart, fake-device assignment is kept")
			devIDRestart1, err := parseLog(ctx, f, pod1.Name, pod1.Name, deviceIDRE)
			framework.ExpectNoError(err, "getting logs for pod %q", pod1.Name)
			gomega.Expect(devIDRestart1).To(gomega.Equal(devID1))

			ginkgo.By("Restarting Kubelet")
			restartKubelet(ctx, true)

			ginkgo.By("Wait for node to be ready again")
			e2enode.WaitForAllNodesSchedulable(ctx, f.ClientSet, 5*time.Minute)

			ginkgo.By("Checking an instance of the pod is running")
			gomega.Eventually(ctx, getPodByName).
				WithArguments(f, pod1.Name).
				// The kubelet restarts pod with an exponential back-off delay, with a maximum cap of 5 minutes.
				// Allow 5 minutes and 10 seconds for the pod to start in a slow environment.
				WithTimeout(5*time.Minute+10*time.Second).
				Should(gomega.And(
					BeAPodInPhase(v1.PodRunning),
					BeAPodReady(),
				),
					"the pod should still be running, the workload should not be perturbed by kubelet restarts")

			ginkgo.By("Verifying the device assignment after pod and kubelet restart using container logs")
			var devID1Restarted string
			gomega.Eventually(ctx, func() string {
				devID1Restarted, err = parseLog(ctx, f, pod1.Name, pod1.Name, deviceIDRE)
				if err != nil {
					framework.Logf("error getting logds for pod %q: %v", pod1.Name, err)
					return ""
				}
				return devID1Restarted
			}, 30*time.Second, framework.Poll).Should(gomega.Equal(devID1), "pod %s reports a different device after restarts: %s (expected %s)", pod1.Name, devID1Restarted, devID1)

			ginkgo.By("Verifying the device assignment after pod and kubelet restart using podresources API")
			gomega.Eventually(ctx, func() error {
				v1PodResources, err = getV1NodeDevices(ctx)
				return err
			}, 30*time.Second, framework.Poll).ShouldNot(gomega.HaveOccurred(), "cannot fetch the compute resource assignment after kubelet restart")

			err, _ = checkPodResourcesAssignment(v1PodResources, pod1.Namespace, pod1.Name, pod1.Spec.Containers[0].Name, SampleDeviceResourceName, []string{devID1})
			framework.ExpectNoError(err, "inconsistent device assignment after pod restart")
		})

		ginkgo.It("will not attempt to admit the succeeded pod after the kubelet restart and device plugin removed", func(ctx context.Context) {
			podRECMD := fmt.Sprintf("devs=$(ls /tmp/ | egrep '^Dev-[0-9]+$') && echo stub devices: $devs && sleep %s", sleepIntervalToCompletion)
			podSpec := makeBusyboxPod(SampleDeviceResourceName, podRECMD)
			podSpec.Spec.RestartPolicy = v1.RestartPolicyNever
			// Making sure the pod will not be garbage collected and will stay thru the kubelet restart after
			// it reached the terminated state. Using finalizers makes the test more reliable.
			podSpec.ObjectMeta.Finalizers = []string{testFinalizer}
			pod := e2epod.NewPodClient(f).CreateSync(ctx, podSpec)

			deviceIDRE := "stub devices: (Dev-[0-9]+)"
			devID1, err := parseLog(ctx, f, pod.Name, pod.Name, deviceIDRE)
			framework.ExpectNoError(err, "getting logs for pod %q", pod.Name)

			gomega.Expect(devID1).To(gomega.Not(gomega.Equal("")), "pod requested a device but started successfully without")

			pod, err = e2epod.NewPodClient(f).Get(ctx, pod.Name, metav1.GetOptions{})
			framework.ExpectNoError(err)

			ginkgo.By("Wait for node to be ready")
			gomega.Expect(e2enode.WaitForAllNodesSchedulable(ctx, f.ClientSet, 5*time.Minute)).To(gomega.Succeed())

			ginkgo.By("Waiting for pod to succeed")
			gomega.Expect(e2epod.WaitForPodSuccessInNamespace(ctx, f.ClientSet, pod.Name, pod.Namespace)).To(gomega.Succeed())

			ginkgo.By("Deleting the device plugin")
			e2epod.NewPodClient(f).DeleteSync(ctx, devicePluginPod.Name, metav1.DeleteOptions{}, f.Timeouts.PodDelete)
			waitForContainerRemoval(ctx, devicePluginPod.Spec.Containers[0].Name, devicePluginPod.Name, devicePluginPod.Namespace)

			gomega.Eventually(getNodeResourceValues, devicePluginGracefulTimeout, f.Timeouts.Poll).WithContext(ctx).WithArguments(SampleDeviceResourceName).Should(gomega.Equal(ResourceValue{Allocatable: 0, Capacity: int(expectedSampleDevsAmount)}))

			ginkgo.By("Restarting Kubelet")
			restartKubelet(ctx, true)

			ginkgo.By("Wait for node to be ready again")
			gomega.Expect(e2enode.WaitForAllNodesSchedulable(ctx, f.ClientSet, 5*time.Minute)).To(gomega.Succeed())

			ginkgo.By("Pod should still be in Succeed state")
			// This ensures that the pod was admitted successfully.
			// In the past we had and issue when kubelet will attempt to re-admit the terminated pod and will change it's phase to Failed.
			// There are no indication that the pod was re-admitted so we just wait for a minute after the node became ready.
			gomega.Consistently(func() v1.PodPhase {
				pod, err = f.ClientSet.CoreV1().Pods(f.Namespace.Name).Get(ctx, pod.Name, metav1.GetOptions{})
				return pod.Status.Phase
			}, 1*time.Minute, f.Timeouts.Poll).Should(gomega.Equal(v1.PodSucceeded))

			ginkgo.By("Removing the finalizer from the pod so it can be deleted now")
			e2epod.NewPodClient(f).RemoveFinalizer(context.TODO(), podSpec.Name, testFinalizer)
		})

		// simulate device plugin re-registration, *but not* container and kubelet restart.
		// After the device plugin has re-registered, the list healthy devices is repopulated based on the devices discovered.
		// Once Pod2 is running we determine the device that was allocated it. As long as the device allocation succeeds the
		// test should pass.
		ginkgo.It("Keeps device plugin assignments after the device plugin has restarted (no kubelet restart, pod restart)", func(ctx context.Context) {
			podRECMD := fmt.Sprintf("devs=$(ls /tmp/ | egrep '^Dev-[0-9]+$') && echo stub devices: $devs && sleep %s", sleepIntervalForever)
			pod1 := e2epod.NewPodClient(f).CreateSync(ctx, makeBusyboxPod(SampleDeviceResourceName, podRECMD))
			deviceIDRE := "stub devices: (Dev-[0-9]+)"
			devID1, err := parseLog(ctx, f, pod1.Name, pod1.Name, deviceIDRE)
			framework.ExpectNoError(err, "getting logs for pod %q", pod1.Name)
			gomega.Expect(devID1).To(gomega.Not(gomega.Equal("")), "pod1 requested a device but started successfully without")

			pod1, err = e2epod.NewPodClient(f).Get(ctx, pod1.Name, metav1.GetOptions{})
			framework.ExpectNoError(err)

			ginkgo.By("Wait for node to be ready again")
			e2enode.WaitForAllNodesSchedulable(ctx, f.ClientSet, 5*time.Minute)

			ginkgo.By("Re-Register resources and delete the plugin pod")
			gp := int64(0)
			deleteOptions := metav1.DeleteOptions{
				GracePeriodSeconds: &gp,
			}
			e2epod.NewPodClient(f).DeleteSync(ctx, devicePluginPod.Name, deleteOptions, f.Timeouts.PodDelete)
			waitForContainerRemoval(ctx, devicePluginPod.Spec.Containers[0].Name, devicePluginPod.Name, devicePluginPod.Namespace)

			ginkgo.By("Recreating the plugin pod")
			devicePluginPod = e2epod.NewPodClient(f).CreateSync(ctx, dptemplate)
			err = e2epod.WaitTimeoutForPodRunningInNamespace(ctx, f.ClientSet, devicePluginPod.Name, devicePluginPod.Namespace, 1*time.Minute)
			framework.ExpectNoError(err)

			ginkgo.By("Waiting for resource to become available on the local node after re-registration")
			gomega.Eventually(ctx, func() bool {
				node, ready := getLocalTestNode(ctx, f)
				return ready &&
					CountSampleDeviceCapacity(node) == expectedSampleDevsAmount &&
					CountSampleDeviceAllocatable(node) == expectedSampleDevsAmount
			}, 30*time.Second, framework.Poll).Should(gomega.BeTrueBecause("expected resource to be available after re-registration"))

			// crosscheck that after device plugin restart the device assignment is preserved and
			// stable from the kubelet's perspective.
			// note we don't check again the logs of the container: the check is done at startup, the container
			// never restarted (runs "forever" from this test timescale perspective) hence re-doing this check
			// is useless.
			ginkgo.By("Verifying the device assignment after device plugin restart using podresources API")
			gomega.Eventually(ctx, func() error {
				v1PodResources, err = getV1NodeDevices(ctx)
				return err
			}, 30*time.Second, framework.Poll).ShouldNot(gomega.HaveOccurred(), "cannot fetch the compute resource assignment after kubelet restart")

			err, _ = checkPodResourcesAssignment(v1PodResources, pod1.Namespace, pod1.Name, pod1.Spec.Containers[0].Name, SampleDeviceResourceName, []string{devID1})
			framework.ExpectNoError(err, "inconsistent device assignment after pod restart")
		})

		// simulate kubelet restart *and* device plugin restart, while the pod and the container stays running.
		// The device assignment should be kept and be stable across the kubelet/device plugin restart, as both the aforementioned components
		// orchestrate the device allocation: the actual consumer (container) is stable.
		ginkgo.It("Keeps device plugin assignments after kubelet restart and device plugin restart (no pod restart)", func(ctx context.Context) {
			podRECMD := fmt.Sprintf("devs=$(ls /tmp/ | egrep '^Dev-[0-9]+$') && echo stub devices: $devs && sleep %s", sleepIntervalForever) // the pod has to run "forever" in the timescale of this test
			pod1 := e2epod.NewPodClient(f).CreateSync(ctx, makeBusyboxPod(SampleDeviceResourceName, podRECMD))
			deviceIDRE := "stub devices: (Dev-[0-9]+)"
			devID1, err := parseLog(ctx, f, pod1.Name, pod1.Name, deviceIDRE)
			framework.ExpectNoError(err, "getting logs for pod %q", pod1.Name)

			gomega.Expect(devID1).To(gomega.Not(gomega.BeEmpty()), "pod1 requested a device but started successfully without")

			pod1, err = e2epod.NewPodClient(f).Get(ctx, pod1.Name, metav1.GetOptions{})
			framework.ExpectNoError(err)

			ginkgo.By("Restarting Kubelet")
			restartKubelet(ctx, true)

			ginkgo.By("Wait for node to be ready again")
			e2enode.WaitForAllNodesSchedulable(ctx, f.ClientSet, 5*time.Minute)

			ginkgo.By("Checking the same instance of the pod is still running after kubelet restart")
			gomega.Eventually(ctx, getPodByName).
				WithArguments(f, pod1.Name).
				WithTimeout(time.Minute).
				Should(BeTheSamePodStillRunning(pod1),
					"the same pod instance not running across kubelet restarts, workload should not be perturbed by kubelet restarts")

			// crosscheck from the device assignment is preserved and stable from perspective of the kubelet.
			// note we don't check again the logs of the container: the check is done at startup, the container
			// never restarted (runs "forever" from this test timescale perspective) hence re-doing this check
			// is useless.
			ginkgo.By("Verifying the device assignment after kubelet restart using podresources API")
			gomega.Eventually(ctx, func() error {
				v1PodResources, err = getV1NodeDevices(ctx)
				return err
			}, 30*time.Second, framework.Poll).ShouldNot(gomega.HaveOccurred(), "cannot fetch the compute resource assignment after kubelet restart")

			err, _ = checkPodResourcesAssignment(v1PodResources, pod1.Namespace, pod1.Name, pod1.Spec.Containers[0].Name, SampleDeviceResourceName, []string{devID1})
			framework.ExpectNoError(err, "inconsistent device assignment after pod restart")

			ginkgo.By("Re-Register resources by deleting the plugin pod")
			gp := int64(0)
			deleteOptions := metav1.DeleteOptions{
				GracePeriodSeconds: &gp,
			}
			e2epod.NewPodClient(f).DeleteSync(ctx, devicePluginPod.Name, deleteOptions, f.Timeouts.PodDelete)
			waitForContainerRemoval(ctx, devicePluginPod.Spec.Containers[0].Name, devicePluginPod.Name, devicePluginPod.Namespace)

			ginkgo.By("Recreating the plugin pod")
			devicePluginPod = e2epod.NewPodClient(f).CreateSync(ctx, dptemplate)

			ginkgo.By("Waiting for resource to become available on the local node after restart")
			gomega.Eventually(ctx, func() bool {
				node, ready := getLocalTestNode(ctx, f)
				return ready &&
					CountSampleDeviceCapacity(node) == expectedSampleDevsAmount &&
					CountSampleDeviceAllocatable(node) == expectedSampleDevsAmount
			}, 30*time.Second, framework.Poll).Should(gomega.BeTrueBecause("expected resource to be available after restart"))

			ginkgo.By("Checking the same instance of the pod is still running after the device plugin restart")
			gomega.Eventually(ctx, getPodByName).
				WithArguments(f, pod1.Name).
				WithTimeout(time.Minute).
				Should(BeTheSamePodStillRunning(pod1),
					"the same pod instance not running across kubelet restarts, workload should not be perturbed by device plugins restarts")
		})

		ginkgo.It("[OrphanedPods] Ensures pods consuming devices deleted while kubelet is down are cleaned up correctly", func(ctx context.Context) {
			podRECMD := fmt.Sprintf("devs=$(ls /tmp/ | egrep '^Dev-[0-9]+$') && echo stub devices: $devs && sleep %s", sleepIntervalWithRestart)
			pod := e2epod.NewPodClient(f).CreateSync(ctx, makeBusyboxPod(SampleDeviceResourceName, podRECMD))

			deviceIDRE := "stub devices: (Dev-[0-9]+)"
			devID, err := parseLog(ctx, f, pod.Name, pod.Name, deviceIDRE)
			framework.ExpectNoError(err, "getting logs for pod %q", pod.Name)
			gomega.Expect(devID).To(gomega.Not(gomega.BeEmpty()), "pod1 requested a device but started successfully without")

			pod, err = e2epod.NewPodClient(f).Get(ctx, pod.Name, metav1.GetOptions{})
			framework.ExpectNoError(err)

			ginkgo.By("stopping the kubelet")
			restartKubelet := mustStopKubelet(ctx, f)

			// wait until the kubelet health check will fail
			gomega.Eventually(ctx, func() bool {
				ok := kubeletHealthCheck(kubeletHealthCheckURL)
				framework.Logf("kubelet health check at %q value=%v", kubeletHealthCheckURL, ok)
				return ok
			}, f.Timeouts.PodStart, f.Timeouts.Poll).Should(gomega.BeFalseBecause("expected kubelet health check to be failed"))

			framework.Logf("Delete the pod while the kubelet is not running")
			// Delete pod sync by name will force delete the pod, removing it from kubelet's config
			deletePodSyncByName(ctx, f, pod.Name)

			framework.Logf("Starting the kubelet")
			restartKubelet(ctx)

			// wait until the kubelet health check will succeed
			gomega.Eventually(ctx, func() bool {
				ok := kubeletHealthCheck(kubeletHealthCheckURL)
				framework.Logf("kubelet health check at %q value=%v", kubeletHealthCheckURL, ok)
				return ok
			}, f.Timeouts.PodStart, f.Timeouts.Poll).Should(gomega.BeTrueBecause("expected kubelet to be in healthy state"))

			framework.Logf("wait for the pod %v to disappear", pod.Name)
			gomega.Eventually(ctx, func(ctx context.Context) error {
				err := checkMirrorPodDisappear(ctx, f.ClientSet, pod.Name, pod.Namespace)
				framework.Logf("pod %s/%s disappear check err=%v", pod.Namespace, pod.Name, err)
				return err
			}, f.Timeouts.PodDelete, f.Timeouts.Poll).Should(gomega.BeNil())

			waitForAllContainerRemoval(ctx, pod.Name, pod.Namespace)

			ginkgo.By("Verifying the device assignment after device plugin restart using podresources API")
			gomega.Eventually(ctx, func() error {
				v1PodResources, err = getV1NodeDevices(ctx)
				return err
			}, 30*time.Second, framework.Poll).ShouldNot(gomega.HaveOccurred(), "cannot fetch the compute resource assignment after kubelet restart")
			err, allocated := checkPodResourcesAssignment(v1PodResources, pod.Namespace, pod.Name, pod.Spec.Containers[0].Name, SampleDeviceResourceName, []string{})
			if err == nil || allocated {
				framework.Fail(fmt.Sprintf("stale device assignment after pod deletion while kubelet was down allocated=%v error=%v", allocated, err))
			}
		})

		f.It("Can schedule a pod with a restartable init container", feature.SidecarContainers, func(ctx context.Context) {
			podRECMD := "devs=$(ls /tmp/ | egrep '^Dev-[0-9]+$') && echo stub devices: $devs && sleep %s"
			sleepOneSecond := "1s"
			rl := v1.ResourceList{v1.ResourceName(SampleDeviceResourceName): *resource.NewQuantity(1, resource.DecimalSI)}
			pod := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "device-plugin-test-" + string(uuid.NewUUID())},
				Spec: v1.PodSpec{
					RestartPolicy: v1.RestartPolicyAlways,
					InitContainers: []v1.Container{
						{
							Image:   busyboxImage,
							Name:    "init-1",
							Command: []string{"sh", "-c", fmt.Sprintf(podRECMD, sleepOneSecond)},
							Resources: v1.ResourceRequirements{
								Limits:   rl,
								Requests: rl,
							},
						},
						{
							Image:   busyboxImage,
							Name:    "restartable-init-2",
							Command: []string{"sh", "-c", fmt.Sprintf(podRECMD, sleepIntervalForever)},
							Resources: v1.ResourceRequirements{
								Limits:   rl,
								Requests: rl,
							},
							RestartPolicy: &containerRestartPolicyAlways,
						},
					},
					Containers: []v1.Container{{
						Image:   busyboxImage,
						Name:    "regular-1",
						Command: []string{"sh", "-c", fmt.Sprintf(podRECMD, sleepIntervalForever)},
						Resources: v1.ResourceRequirements{
							Limits:   rl,
							Requests: rl,
						},
					}},
				},
			}

			pod1 := e2epod.NewPodClient(f).CreateSync(ctx, pod)
			deviceIDRE := "stub devices: (Dev-[0-9]+)"

			devID1, err := parseLog(ctx, f, pod1.Name, pod1.Spec.InitContainers[0].Name, deviceIDRE)
			framework.ExpectNoError(err, "getting logs for pod %q/%q", pod1.Name, pod1.Spec.InitContainers[0].Name)
			gomega.Expect(devID1).To(gomega.Not(gomega.Equal("")), "pod1's init container requested a device but started successfully without")

			devID2, err := parseLog(ctx, f, pod1.Name, pod1.Spec.InitContainers[1].Name, deviceIDRE)
			framework.ExpectNoError(err, "getting logs for pod %q/%q", pod1.Name, pod1.Spec.InitContainers[1].Name)
			gomega.Expect(devID2).To(gomega.Not(gomega.Equal("")), "pod1's restartable init container requested a device but started successfully without")

			gomega.Expect(devID2).To(gomega.Equal(devID1), "pod1's init container and restartable init container should share the same device")

			devID3, err := parseLog(ctx, f, pod1.Name, pod1.Spec.Containers[0].Name, deviceIDRE)
			framework.ExpectNoError(err, "getting logs for pod %q/%q", pod1.Name, pod1.Spec.Containers[0].Name)
			gomega.Expect(devID3).To(gomega.Not(gomega.Equal("")), "pod1's regular container requested a device but started successfully without")

			gomega.Expect(devID3).NotTo(gomega.Equal(devID2), "pod1's restartable init container and regular container should not share the same device")

			podResources, err := getV1NodeDevices(ctx)
			framework.ExpectNoError(err)

			framework.Logf("PodResources.PodResources:%+v\n", podResources.PodResources)
			framework.Logf("len(PodResources.PodResources):%+v", len(podResources.PodResources))

			gomega.Expect(podResources.PodResources).To(gomega.HaveLen(2))

			var resourcesForOurPod *kubeletpodresourcesv1.PodResources
			for _, res := range podResources.GetPodResources() {
				if res.Name == pod1.Name {
					resourcesForOurPod = res
				}
			}

			gomega.Expect(resourcesForOurPod).NotTo(gomega.BeNil())

			gomega.Expect(resourcesForOurPod.Name).To(gomega.Equal(pod1.Name))
			gomega.Expect(resourcesForOurPod.Namespace).To(gomega.Equal(pod1.Namespace))

			gomega.Expect(resourcesForOurPod.Containers).To(gomega.HaveLen(2))

			for _, container := range resourcesForOurPod.Containers {
				if container.Name == pod1.Spec.InitContainers[1].Name {
					gomega.Expect(container.Devices).To(gomega.HaveLen(1))
					gomega.Expect(container.Devices[0].ResourceName).To(gomega.Equal(SampleDeviceResourceName))
					gomega.Expect(container.Devices[0].DeviceIds).To(gomega.HaveLen(1))
				} else if container.Name == pod1.Spec.Containers[0].Name {
					gomega.Expect(container.Devices).To(gomega.HaveLen(1))
					gomega.Expect(container.Devices[0].ResourceName).To(gomega.Equal(SampleDeviceResourceName))
					gomega.Expect(container.Devices[0].DeviceIds).To(gomega.HaveLen(1))
				} else {
					framework.Failf("unexpected container name: %s", container.Name)
				}
			}
		})
	})
}

func testDevicePluginNodeReboot(f *framework.Framework, pluginSockDir string) {
	f.Context("DevicePlugin", f.WithSerial(), f.WithDisruptive(), func() {
		var devicePluginPod *v1.Pod
		var v1PodResources *kubeletpodresourcesv1.ListPodResourcesResponse
		var triggerPathFile, triggerPathDir string
		var err error

		ginkgo.BeforeEach(func(ctx context.Context) {
			ginkgo.By("Wait for node to be ready")
			gomega.Eventually(ctx, e2enode.TotalReady).
				WithArguments(f.ClientSet).
				WithTimeout(time.Minute).
				Should(gomega.BeEquivalentTo(1))

			// Before we run the device plugin test, we need to ensure
			// that the cluster is in a clean state and there are no
			// pods running on this node.
			// This is done in a gomega.Eventually with retries since a prior test in a different test suite could've run and the deletion of it's resources may still be in progress.
			// xref: https://issue.k8s.io/115381
			gomega.Eventually(ctx, func(ctx context.Context) error {
				v1PodResources, err = getV1NodeDevices(ctx)
				if err != nil {
					return fmt.Errorf("failed to get node local podresources by accessing the (v1) podresources API endpoint: %v", err)
				}

				if len(v1PodResources.PodResources) > 0 {
					return fmt.Errorf("expected v1 pod resources to be empty, but got non-empty resources: %+v", v1PodResources.PodResources)
				}
				return nil
			}, f.Timeouts.SystemDaemonsetStartup, f.Timeouts.Poll).Should(gomega.Succeed())

			ginkgo.By("Setting up the directory for controlling registration")
			triggerPathDir = filepath.Join(devicePluginDir, "sample")
			if _, err := os.Stat(triggerPathDir); err != nil {
				if errors.Is(err, os.ErrNotExist) {
					if err := os.Mkdir(triggerPathDir, os.ModePerm); err != nil {
						framework.Fail(fmt.Sprintf("registration control directory %q creation failed: %v ", triggerPathDir, err))
					}
					framework.Logf("registration control directory created successfully")
				} else {
					framework.Fail(fmt.Sprintf("unexpected error checking %q: %v", triggerPathDir, err))
				}
			} else {
				framework.Logf("registration control directory %q already present", triggerPathDir)
			}

			ginkgo.By("Setting up the file trigger for controlling registration")
			triggerPathFile = filepath.Join(triggerPathDir, "registration")
			if _, err := os.Stat(triggerPathFile); err != nil {
				if errors.Is(err, os.ErrNotExist) {
					if _, err = os.Create(triggerPathFile); err != nil {
						framework.Fail(fmt.Sprintf("registration control file %q creation failed: %v", triggerPathFile, err))
					}
					framework.Logf("registration control file created successfully")
				} else {
					framework.Fail(fmt.Sprintf("unexpected error creating %q: %v", triggerPathFile, err))
				}
			} else {
				framework.Logf("registration control file %q already present", triggerPathFile)
			}

			ginkgo.By("Scheduling a sample device plugin pod")
			data, err := e2etestfiles.Read(SampleDevicePluginControlRegistrationDSYAML)
			if err != nil {
				framework.Fail(fmt.Sprintf("error reading test data %q: %v", SampleDevicePluginControlRegistrationDSYAML, err))
			}
			ds := readDaemonSetV1OrDie(data)

			dp := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: SampleDevicePluginName,
				},
				Spec: ds.Spec.Template.Spec,
			}

			devicePluginPod = e2epod.NewPodClient(f).CreateSync(ctx, dp)

			go func() {
				// Since autoregistration is disabled for the device plugin (as REGISTER_CONTROL_FILE
				// environment variable is specified), device plugin registration needs to be triggerred
				// manually.
				// This is done by deleting the control file at the following path:
				// `/var/lib/kubelet/device-plugins/sample/registration`.

				defer ginkgo.GinkgoRecover()
				framework.Logf("Deleting the control file: %q to trigger registration", triggerPathFile)
				err := os.Remove(triggerPathFile)
				framework.ExpectNoError(err)
			}()

			ginkgo.By("Waiting for devices to become available on the local node")
			gomega.Eventually(ctx, func(ctx context.Context) bool {
				node, ready := getLocalTestNode(ctx, f)
				return ready && CountSampleDeviceCapacity(node) > 0
			}, 5*time.Minute, framework.Poll).Should(gomega.BeTrueBecause("expected devices to be available on the local node"))
			framework.Logf("Successfully created device plugin pod")

			ginkgo.By(fmt.Sprintf("Waiting for the resource exported by the sample device plugin to become available on the local node (instances: %d)", expectedSampleDevsAmount))
			gomega.Eventually(ctx, func(ctx context.Context) bool {
				node, ready := getLocalTestNode(ctx, f)
				return ready &&
					CountSampleDeviceCapacity(node) == expectedSampleDevsAmount &&
					CountSampleDeviceAllocatable(node) == expectedSampleDevsAmount
			}, 30*time.Second, framework.Poll).Should(gomega.BeTrueBecause("expected resource to be available on local node"))
		})

		ginkgo.AfterEach(func(ctx context.Context) {
			ginkgo.By("Deleting the device plugin pod")
			e2epod.NewPodClient(f).DeleteSync(ctx, devicePluginPod.Name, metav1.DeleteOptions{}, f.Timeouts.PodDelete)

			ginkgo.By("Deleting any Pods created by the test")
			l, err := e2epod.NewPodClient(f).List(ctx, metav1.ListOptions{})
			framework.ExpectNoError(err)
			for _, p := range l.Items {
				if p.Namespace != f.Namespace.Name {
					continue
				}

				ginkgo.By("Removing the finalizer from the pod in case it was used")
				e2epod.NewPodClient(f).RemoveFinalizer(context.TODO(), p.Name, testFinalizer)

				framework.Logf("Deleting pod: %s", p.Name)
				e2epod.NewPodClient(f).DeleteSync(ctx, p.Name, metav1.DeleteOptions{}, f.Timeouts.PodDelete)
			}

			err = os.Remove(triggerPathDir)
			framework.ExpectNoError(err)

			ginkgo.By("Waiting for devices to become unavailable on the local node")
			gomega.Eventually(ctx, func(ctx context.Context) bool {
				node, ready := getLocalTestNode(ctx, f)
				return ready && CountSampleDeviceCapacity(node) <= 0
			}, 5*time.Minute, framework.Poll).Should(gomega.BeTrueBecause("expected devices to be unavailable on local node"))

			ginkgo.By("devices now unavailable on the local node")
		})

		// simulate node reboot scenario by removing pods using CRI before kubelet is started. In addition to that,
		// intentionally a scenario is created where after node reboot, application pods requesting devices appear before the device plugin pod
		// exposing those devices as resource has restarted. The expected behavior is that the application pod fails at admission time.
		framework.It("Does not keep device plugin assignments across node reboots if fails admission (no pod restart, no device plugin re-registration)", func(ctx context.Context) {
			podRECMD := fmt.Sprintf("devs=$(ls /tmp/ | egrep '^Dev-[0-9]+$') && echo stub devices: $devs && sleep %s", sleepIntervalForever)
			pod1 := e2epod.NewPodClient(f).CreateSync(ctx, makeBusyboxPod(SampleDeviceResourceName, podRECMD))
			deviceIDRE := "stub devices: (Dev-[0-9]+)"
			devID1, err := parseLog(ctx, f, pod1.Name, pod1.Name, deviceIDRE)
			framework.ExpectNoError(err, "getting logs for pod %q", pod1.Name)

			gomega.Expect(devID1).To(gomega.Not(gomega.Equal("")))

			pod1, err = e2epod.NewPodClient(f).Get(ctx, pod1.Name, metav1.GetOptions{})
			framework.ExpectNoError(err)

			ginkgo.By("stopping the kubelet")
			restartKubelet := mustStopKubelet(ctx, f)

			ginkgo.By("stopping all the local containers - using CRI")
			rs, _, err := getCRIClient()
			framework.ExpectNoError(err)
			sandboxes, err := rs.ListPodSandbox(ctx, &runtimeapi.PodSandboxFilter{})
			framework.ExpectNoError(err)
			for _, sandbox := range sandboxes {
				gomega.Expect(sandbox.Metadata).ToNot(gomega.BeNil())
				ginkgo.By(fmt.Sprintf("deleting pod using CRI: %s/%s -> %s", sandbox.Metadata.Namespace, sandbox.Metadata.Name, sandbox.Id))

				err := rs.RemovePodSandbox(ctx, sandbox.Id)
				framework.ExpectNoError(err)
			}

			ginkgo.By("restarting the kubelet")
			restartKubelet(ctx)

			ginkgo.By("Wait for node to be ready again")
			e2enode.WaitForAllNodesSchedulable(ctx, f.ClientSet, 5*time.Minute)

			ginkgo.By("Waiting for the pod to fail with admission error as device plugin hasn't re-registered yet")
			gomega.Eventually(ctx, getPod).
				WithArguments(f, pod1.Name).
				WithTimeout(time.Minute).
				Should(HaveFailedWithAdmissionError(),
					"the pod succeeded to start, when it should fail with the admission error")

			// crosscheck from the device assignment is preserved and stable from perspective of the kubelet.
			// note we don't check again the logs of the container: the check is done at startup, the container
			// never restarted (runs "forever" from this test timescale perspective) hence re-doing this check
			// is useless.
			ginkgo.By("Verifying the device assignment after kubelet restart using podresources API")
			gomega.Eventually(ctx, func() error {
				v1PodResources, err = getV1NodeDevices(ctx)
				return err
			}, 30*time.Second, framework.Poll).ShouldNot(gomega.HaveOccurred(), "cannot fetch the compute resource assignment after kubelet restart")

			// if we got this far, podresources API will now report 2 entries:
			// - sample device plugin pod, running and doing fine
			// - our test pod, in failed state. Pods in terminal state will still be reported, see https://github.com/kubernetes/kubernetes/issues/119423
			// so we care about our test pod, and it will be present in the returned list till 119423 is fixed, but since it failed admission it must not have
			// any device allocated to it, hence we check for empty device set in the podresources response. So, we check that
			// A. our test pod must be present in the list response *and*
			// B. it has no devices assigned to it.
			// anything else is unexpected and thus makes the test fail. Once 119423 is fixed, a better, simpler and more intuitive check will be for the
			// test pod to not be present in the podresources list response, but till that time we're stuck with this approach.
			_, found := checkPodResourcesAssignment(v1PodResources, pod1.Namespace, pod1.Name, pod1.Spec.Containers[0].Name, SampleDeviceResourceName, []string{})
			gomega.Expect(found).To(gomega.BeTrueBecause("%s/%s/%s failed admission, should not have devices registered", pod1.Namespace, pod1.Name, pod1.Spec.Containers[0].Name))
		})
	})
}

// makeBusyboxPod returns a simple Pod spec with a busybox container
// that requests SampleDeviceResourceName and runs the specified command.
func makeBusyboxPod(SampleDeviceResourceName, cmd string) *v1.Pod {
	podName := "device-plugin-test-" + string(uuid.NewUUID())
	rl := v1.ResourceList{v1.ResourceName(SampleDeviceResourceName): *resource.NewQuantity(1, resource.DecimalSI)}

	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: podName},
		Spec: v1.PodSpec{
			RestartPolicy: v1.RestartPolicyAlways,
			Containers: []v1.Container{{
				Image: busyboxImage,
				Name:  podName,
				// Runs the specified command in the test pod.
				Command: []string{"sh", "-c", cmd},
				Resources: v1.ResourceRequirements{
					Limits:   rl,
					Requests: rl,
				},
			}},
		},
	}
}

// ensurePodContainerRestart confirms that pod container has restarted at least once
func ensurePodContainerRestart(ctx context.Context, f *framework.Framework, podName string, contName string) {
	var initialCount int32
	var currentCount int32
	p, err := e2epod.NewPodClient(f).Get(ctx, podName, metav1.GetOptions{})
	if err != nil || len(p.Status.ContainerStatuses) < 1 {
		framework.Failf("ensurePodContainerRestart failed for pod %q: %v", podName, err)
	}
	initialCount = p.Status.ContainerStatuses[0].RestartCount
	gomega.Eventually(ctx, func() int {
		p, err = e2epod.NewPodClient(f).Get(ctx, podName, metav1.GetOptions{})
		if err != nil || len(p.Status.ContainerStatuses) < 1 {
			return 0
		}
		currentCount = p.Status.ContainerStatuses[0].RestartCount
		framework.Logf("initial %v, current %v", initialCount, currentCount)
		return int(currentCount)
	}, 5*time.Minute, framework.Poll).Should(gomega.BeNumerically(">", initialCount))
}

// parseLog returns the matching string for the specified regular expression parsed from the container logs.
func parseLog(ctx context.Context, f *framework.Framework, podName string, contName string, re string) (string, error) {
	logs, err := e2epod.GetPodLogs(ctx, f.ClientSet, f.Namespace.Name, podName, contName)
	if err != nil {
		return "", err
	}

	framework.Logf("got pod logs: %v", logs)
	regex := regexp.MustCompile(re)
	matches := regex.FindStringSubmatch(logs)
	if len(matches) < 2 {
		return "", fmt.Errorf("unexpected match in logs: %q", logs)
	}

	return matches[1], nil
}

func checkPodResourcesAssignment(v1PodRes *kubeletpodresourcesv1.ListPodResourcesResponse, podNamespace, podName, containerName, resourceName string, devs []string) (error, bool) {
	for _, podRes := range v1PodRes.PodResources {
		if podRes.Namespace != podNamespace || podRes.Name != podName {
			continue
		}
		for _, contRes := range podRes.Containers {
			if contRes.Name != containerName {
				continue
			}
			return matchContainerDevices(podNamespace+"/"+podName+"/"+containerName, contRes.Devices, resourceName, devs)
		}
	}
	v1PodResStr := ""
	for _, p := range v1PodRes.PodResources {
		for _, c := range p.Containers {
			v1PodResStr += fmt.Sprintf("%s/%s/%s,", p.Namespace, p.Name, c.Name)
		}
	}
	err := fmt.Errorf("no resources found for %s/%s/%s in listpodresources [%s]", podNamespace, podName, containerName, v1PodResStr)
	framework.Logf("%v", err)
	return err, false
}

func matchContainerDevices(ident string, contDevs []*kubeletpodresourcesv1.ContainerDevices, resourceName string, devs []string) (error, bool) {
	expected := sets.New[string](devs...)
	assigned := sets.New[string]()
	for _, contDev := range contDevs {
		if contDev.ResourceName != resourceName {
			continue
		}
		assigned = assigned.Insert(contDev.DeviceIds...)
	}
	expectedStr := strings.Join(expected.UnsortedList(), ",")
	assignedStr := strings.Join(assigned.UnsortedList(), ",")
	framework.Logf("%s: devices expected %q assigned %q", ident, expectedStr, assignedStr)
	if !assigned.Equal(expected) {
		return fmt.Errorf("device allocation mismatch for %s expected %s assigned %s", ident, expectedStr, assignedStr), true
	}
	return nil, true
}

// getSampleDevicePluginPod returns the Sample Device Plugin pod to be used e2e tests.
func getSampleDevicePluginPod(pluginSockDir string) *v1.Pod {
	data, err := e2etestfiles.Read(SampleDevicePluginDSYAML)
	if err != nil {
		framework.Fail(err.Error())
	}

	ds := readDaemonSetV1OrDie(data)
	dp := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: SampleDevicePluginName,
		},
		Spec: ds.Spec.Template.Spec,
	}
	for i := range dp.Spec.Containers[0].Env {
		if dp.Spec.Containers[0].Env[i].Name == SampleDeviceEnvVarNamePluginSockDir {
			dp.Spec.Containers[0].Env[i].Value = pluginSockDir
		}
	}

	if utilfeature.DefaultFeatureGate.Enabled(features.DevicePluginCDIDevices) {
		dp.Spec.Containers[0].Env = append(dp.Spec.Containers[0].Env, v1.EnvVar{Name: "CDI_ENABLED", Value: "1"})
	}

	return dp
}

func BeTheSamePodStillRunning(expected *v1.Pod) types.GomegaMatcher {
	return gomega.And(
		BeTheSamePodAs(expected.UID),
		BeAPodInPhase(v1.PodRunning),
		BeAPodReady(),
	)
}

// BeReady matches if the pod is reported ready
func BeAPodReady() types.GomegaMatcher {
	return gcustom.MakeMatcher(func(actual *v1.Pod) (bool, error) {
		return podutils.IsPodReady(actual), nil
	}).WithTemplate("Pod {{.Actual.Namespace}}/{{.Actual.Name}} UID {{.Actual.UID}} not ready yet")
}

// BeAPodInPhase matches if the pod is running
func BeAPodInPhase(phase v1.PodPhase) types.GomegaMatcher {
	return gcustom.MakeMatcher(func(actual *v1.Pod) (bool, error) {
		return actual.Status.Phase == phase, nil
	}).WithTemplate("Pod {{.Actual.Namespace}}/{{.Actual.Name}} failed {{.To}} be in phase {{.Data}} instead is in phase {{.Actual.Status.Phase}}").WithTemplateData(phase)
}

// BeTheSamePodAs matches if the pod has the given UID
func BeTheSamePodAs(podUID k8stypes.UID) types.GomegaMatcher {
	return gcustom.MakeMatcher(func(actual *v1.Pod) (bool, error) {
		return actual.UID == podUID, nil
	}).WithTemplate("Pod {{.Actual.Namespace}}/{{.Actual.Name}} expected UID {{.Data}} has UID instead {{.Actual.UID}}").WithTemplateData(podUID)
}
