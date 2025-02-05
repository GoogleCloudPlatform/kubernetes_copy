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

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	v1 "k8s.io/api/core/v1"
	kubeletdevicepluginv1beta1 "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	admissionapi "k8s.io/pod-security-admission/api"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/kubernetes/test/e2e/framework"
	"k8s.io/kubernetes/test/e2e_node/testdeviceplugin"
)

var _ = SIGDescribe("Device Plugin Failures:", framework.WithNodeConformance(), func() {
	f := framework.NewDefaultFramework("device-plugin-failures")
	f.NamespacePodSecurityLevel = admissionapi.LevelPrivileged

	type ResourceValue struct {
		Allocatable int
		Capacity    int
	}

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

	var createPod = func(resourceName string, quantity int) *v1.Pod {
		ginkgo.GinkgoHelper()
		rl := v1.ResourceList{v1.ResourceName(resourceName): *resource.NewQuantity(int64(quantity), resource.DecimalSI)}
		pod := &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "device-plugin-failures-test-" + string(uuid.NewUUID())},
			Spec: v1.PodSpec{
				RestartPolicy: v1.RestartPolicyAlways,
				Containers: []v1.Container{{
					Image:   busyboxImage,
					Name:    "container-1",
					Command: []string{"sh", "-c", fmt.Sprintf("env && sleep %s", sleepIntervalForever)},
					Resources: v1.ResourceRequirements{
						Limits:   rl,
						Requests: rl,
					},
				}},
			},
		}
		return pod
	}

	nodeStatusUpdateTimeout := 1 * time.Minute
	devicePluginUpdateTimeout := 1 * time.Minute
	devicePluginGracefulTimeout := 5 * time.Minute // see endpointStopGracePeriod in pkg/kubelet/cm/devicemanager/types.go

	ginkgo.It("when GetDevicePluginOptions fails, device plugin will not be used", func(ctx context.Context) {
		// randomizing so tests can run in parallel
		resourceName := fmt.Sprintf("test.device/%s", f.UniqueName)

		expectedErr := fmt.Errorf("GetDevicePluginOptions failed")

		plugin, _ := testdeviceplugin.NewDevicePlugin(resourceName, f.UniqueName, true, []kubeletdevicepluginv1beta1.Device{{ID: "testdevice", Health: kubeletdevicepluginv1beta1.Healthy}}, func(name string) error {
			if name == "GetDevicePluginOptions" {
				return expectedErr
			}
			return nil
		})

		err := plugin.RegisterDevicePlugin(ctx)
		defer plugin.Stop() // should stop even if registration failed
		gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring("failed to get device plugin options")))
		gomega.Expect(err).To(gomega.MatchError(gomega.ContainSubstring(expectedErr.Error())))

		gomega.Expect(plugin.WasCalled("ListAndWatch")).To(gomega.BeFalseBecause("plugin should not be used if GetDevicePluginOptions fails"))
		gomega.Expect(plugin.WasCalled("GetDevicePluginOptions")).To(gomega.BeTrueBecause("get device plugin options should be called exactly once"))
		gomega.Expect(plugin.Calls()).To(gomega.HaveLen(1))

		// kubelet will not even register the resource
		gomega.Eventually(getNodeResourceValues, nodeStatusUpdateTimeout, f.Timeouts.Poll).WithContext(ctx).WithArguments(resourceName).Should(gomega.Equal(ResourceValue{Allocatable: -1, Capacity: -1}))
	})

	ginkgo.It("when GetDevicePluginOptions fails with the registrar, device plugin will not be used", func(ctx context.Context) {
		// randomizing so tests can run in parallel
		resourceName := fmt.Sprintf("test.device/%s", f.UniqueName)

		expectedErr := fmt.Errorf("GetDevicePluginOptions failed")

		r, err := testdeviceplugin.NewDevicePluginRegistrar(resourceName, f.UniqueName, []kubeletdevicepluginv1beta1.Device{{ID: "testdevice", Health: kubeletdevicepluginv1beta1.Healthy}}, func(name string) error {
			if name == "GetDevicePluginOptions" {
				return expectedErr
			}
			return nil
		}, nil)

		defer r.Stop()

		plugin := r.GetDevicePlugin()

		gomega.Expect(err).To(gomega.Succeed())

		// need to wait a bit for kubelet to discover the device plugin
		gomega.Eventually(plugin.WasCalled, nodeStatusUpdateTimeout, f.Timeouts.Poll).WithArguments("GetDevicePluginOptions").Should(gomega.BeTrueBecause("get device plugin options should be called at least once"))
		gomega.Expect(plugin.WasCalled("ListAndWatch")).To(gomega.BeFalseBecause("plugin should not be used if GetDevicePluginOptions fails"))

		// kubelet will retry shortly to register the device plugin
		gomega.Eventually(func() int { return len(plugin.Calls()) }, nodeStatusUpdateTimeout, f.Timeouts.Poll).Should(gomega.BeNumerically(">=", 2))

		// kubelet will not even register the resource
		gomega.Eventually(getNodeResourceValues, nodeStatusUpdateTimeout, f.Timeouts.Poll).WithContext(ctx).WithArguments(resourceName).Should(gomega.Equal(ResourceValue{Allocatable: -1, Capacity: -1}))
	})

	ginkgo.It("when kubelet restarts, while using plugin registration, there is no race condition with the pod admission", func(ctx context.Context) {
		// randomizing so tests can run in parallel
		resourceName := fmt.Sprintf("test.device/%s", f.UniqueName)

		r, err := testdeviceplugin.NewDevicePluginRegistrar(resourceName, f.UniqueName, []kubeletdevicepluginv1beta1.Device{{ID: "testdevice", Health: kubeletdevicepluginv1beta1.Healthy}}, nil, func(method string) error {
			if method == "GetInfo" {
				// sleep for a long time to simulate delay in plugin registration
				time.Sleep(500 * time.Millisecond) //grpc timeout for plugin registration is 1 second
			}
			return nil
		})
		defer r.Stop()

		gomega.Expect(err).To(gomega.Succeed())

		plugin := r.GetDevicePlugin()

		// need to wait a bit for kubelet to discover the device plugin
		ginkgo.By("need to wait a bit for kubelet to discover the device plugin")
		gomega.Eventually(getNodeResourceValues, nodeStatusUpdateTimeout, f.Timeouts.Poll).WithContext(ctx).WithArguments(resourceName).Should(gomega.Equal(ResourceValue{Allocatable: 1, Capacity: 1}))

		gomega.Expect(plugin.WasCalled("ListAndWatch")).To(gomega.BeTrueBecause("List And Watch should be called at least once"))

		// stop the kubelet to simulate race condition
		ginkgo.By("stop the kubelet")
		restartKubelet := stopKubelet()

		// schedule a pod that requests the device
		client := e2epod.NewPodClient(f)
		podSpec := createPod(resourceName, 1)
		podSpec.Spec.NodeName = framework.TestContext.NodeName
		pod := client.Create(ctx, podSpec)

		ginkgo.By("Restarting the kubelet")
		restartKubelet()

		// wait for the pod to be running
		gomega.Expect(e2epod.WaitForPodRunningInNamespace(ctx, f.ClientSet, pod)).To(gomega.Succeed())

	})

	ginkgo.It("will set allocatable to zero when a single device became unhealthy and then back to 1 if it got healthy again", func(ctx context.Context) {
		// randomizing so tests can run in parallel
		resourceName := fmt.Sprintf("test.device/%s", f.UniqueName)
		devices := []kubeletdevicepluginv1beta1.Device{{ID: "testdevice", Health: kubeletdevicepluginv1beta1.Healthy}}
		plugin, _ := testdeviceplugin.NewDevicePlugin(resourceName, f.UniqueName, true, devices, nil)

		err := plugin.RegisterDevicePlugin(ctx)
		defer plugin.Stop() // should stop even if registration failed
		gomega.Expect(err).To(gomega.Succeed())

		// at first the device is healthy
		gomega.Eventually(getNodeResourceValues, nodeStatusUpdateTimeout, f.Timeouts.Poll).WithContext(ctx).WithArguments(resourceName).Should(gomega.Equal(ResourceValue{Allocatable: 1, Capacity: 1}))

		// now make the device unhealthy
		devices[0].Health = kubeletdevicepluginv1beta1.Unhealthy
		plugin.UpdateDevices(devices)

		gomega.Eventually(getNodeResourceValues, nodeStatusUpdateTimeout, f.Timeouts.Poll).WithContext(ctx).WithArguments(resourceName).Should(gomega.Equal(ResourceValue{Allocatable: 0, Capacity: 1}))

		// now make the device healthy again
		devices[0].Health = kubeletdevicepluginv1beta1.Healthy
		plugin.UpdateDevices(devices)

		gomega.Eventually(getNodeResourceValues, nodeStatusUpdateTimeout, f.Timeouts.Poll).WithContext(ctx).WithArguments(resourceName).Should(gomega.Equal(ResourceValue{Allocatable: 1, Capacity: 1}))
	})

	ginkgo.It("will set allocatable to zero when a single device became unhealthy, but capacity will stay at 1", func(ctx context.Context) {
		// randomizing so tests can run in parallel
		resourceName := fmt.Sprintf("test.device/%s", f.UniqueName)
		devices := []kubeletdevicepluginv1beta1.Device{{ID: "testdevice", Health: kubeletdevicepluginv1beta1.Healthy}}
		plugin, _ := testdeviceplugin.NewDevicePlugin(resourceName, f.UniqueName, true, devices, nil)

		err := plugin.RegisterDevicePlugin(ctx)
		defer plugin.Stop() // should stop even if registration failed
		gomega.Expect(err).To(gomega.Succeed())

		ginkgo.By("initial state: capacity and allocatable are set")
		gomega.Eventually(getNodeResourceValues, nodeStatusUpdateTimeout, f.Timeouts.Poll).WithContext(ctx).WithArguments(resourceName).Should(gomega.Equal(ResourceValue{Allocatable: 1, Capacity: 1}))

		// schedule a pod that requests the device
		client := e2epod.NewPodClient(f)
		pod := client.Create(ctx, createPod(resourceName, 1))

		// wait for the pod to be running
		gomega.Expect(e2epod.WaitForPodRunningInNamespace(ctx, f.ClientSet, pod)).To(gomega.Succeed())

		ginkgo.By("once pod is running, it does not affect allocatable value")
		gomega.Eventually(getNodeResourceValues, nodeStatusUpdateTimeout, f.Timeouts.Poll).WithContext(ctx).WithArguments(resourceName).Should(gomega.Equal(ResourceValue{Allocatable: 1, Capacity: 1}))

		// now make the device unhealthy
		devices[0].Health = kubeletdevicepluginv1beta1.Unhealthy
		plugin.UpdateDevices(devices)

		ginkgo.By("even when device became unhealthy. pod is still running and keeping the capacity")
		// we keep the allocatable at the same value even though device is not healthy any longer
		gomega.Eventually(getNodeResourceValues, nodeStatusUpdateTimeout, f.Timeouts.Poll).WithContext(ctx).WithArguments(resourceName).Should(gomega.Equal(ResourceValue{Allocatable: 0, Capacity: 1}))

		// pod is not affected by the device becoming unhealthy

		gomega.Consistently(func() v1.PodPhase {
			pod, err = f.ClientSet.CoreV1().Pods(f.Namespace.Name).Get(ctx, pod.Name, metav1.GetOptions{})
			return pod.Status.Phase
		}, devicePluginUpdateTimeout, f.Timeouts.Poll).Should(gomega.Equal(v1.PodRunning))

		// deleting the pod
		err = f.ClientSet.CoreV1().Pods(f.Namespace.Name).Delete(ctx, pod.Name, metav1.DeleteOptions{})
		gomega.Expect(err).To(gomega.Succeed())

		// wait for the pod to be deleted
		gomega.Eventually(func() error {
			_, err := f.ClientSet.CoreV1().Pods(f.Namespace.Name).Get(ctx, pod.Name, metav1.GetOptions{})
			return err
		}, f.Timeouts.PodDelete, f.Timeouts.Poll).Should(gomega.MatchError((gomega.ContainSubstring("not found"))))

		ginkgo.By("when pod is deleted, nothing changes")
		gomega.Eventually(getNodeResourceValues, devicePluginGracefulTimeout+1*time.Minute, f.Timeouts.Poll).WithContext(ctx).WithArguments(resourceName).Should(gomega.Equal(ResourceValue{Allocatable: 0, Capacity: 1}))
	})

	ginkgo.It("will lower allocatable to a number of unhealthy devices and then back if they became healthy again", func(ctx context.Context) {
		// randomizing so tests can run in parallel
		resourceName := fmt.Sprintf("test.device/%s", f.UniqueName)

		devices := []kubeletdevicepluginv1beta1.Device{
			{ID: "0", Health: kubeletdevicepluginv1beta1.Healthy},
			{ID: "1", Health: kubeletdevicepluginv1beta1.Healthy},
			{ID: "2", Health: kubeletdevicepluginv1beta1.Healthy},
			{ID: "3", Health: kubeletdevicepluginv1beta1.Healthy},
		}
		plugin, _ := testdeviceplugin.NewDevicePlugin(resourceName, f.UniqueName, true, devices, nil)

		err := plugin.RegisterDevicePlugin(ctx)
		defer plugin.Stop() // should stop even if registration failed
		gomega.Expect(err).To(gomega.Succeed())

		// at first all the devices are healthy
		gomega.Eventually(getNodeResourceValues, nodeStatusUpdateTimeout, f.Timeouts.Poll).WithContext(ctx).WithArguments(resourceName).Should(gomega.Equal(ResourceValue{Allocatable: 4, Capacity: 4}))

		// now make one device unhealthy
		devices[3].Health = kubeletdevicepluginv1beta1.Unhealthy
		plugin.UpdateDevices(devices)

		gomega.Eventually(getNodeResourceValues, nodeStatusUpdateTimeout, f.Timeouts.Poll).WithContext(ctx).WithArguments(resourceName).Should(gomega.Equal(ResourceValue{Allocatable: 3, Capacity: 4}))

		// now make the device healthy again
		devices[3].Health = kubeletdevicepluginv1beta1.Healthy
		plugin.UpdateDevices(devices)

		gomega.Eventually(getNodeResourceValues, nodeStatusUpdateTimeout, f.Timeouts.Poll).WithContext(ctx).WithArguments(resourceName).Should(gomega.Equal(ResourceValue{Allocatable: 4, Capacity: 4}))

		// now make two devices unhealthy
		devices[1].Health = kubeletdevicepluginv1beta1.Unhealthy
		devices[3].Health = kubeletdevicepluginv1beta1.Unhealthy
		plugin.UpdateDevices(devices)

		gomega.Eventually(getNodeResourceValues, nodeStatusUpdateTimeout, f.Timeouts.Poll).WithContext(ctx).WithArguments(resourceName).Should(gomega.Equal(ResourceValue{Allocatable: 2, Capacity: 4}))

		// now make the device healthy again
		devices[3].Health = kubeletdevicepluginv1beta1.Healthy
		plugin.UpdateDevices(devices)

		gomega.Eventually(getNodeResourceValues, nodeStatusUpdateTimeout, f.Timeouts.Poll).WithContext(ctx).WithArguments(resourceName).Should(gomega.Equal(ResourceValue{Allocatable: 3, Capacity: 4}))

		// now make the device healthy again
		devices[1].Health = kubeletdevicepluginv1beta1.Healthy
		plugin.UpdateDevices(devices)

		gomega.Eventually(getNodeResourceValues, nodeStatusUpdateTimeout, f.Timeouts.Poll).WithContext(ctx).WithArguments(resourceName).Should(gomega.Equal(ResourceValue{Allocatable: 4, Capacity: 4}))
	})

	ginkgo.It("when ListAndWatch fails immediately, node allocatable will be set to zero and kubelet will not retry to list resources", func(ctx context.Context) {
		// randomizing so tests can run in parallel
		resourceName := fmt.Sprintf("test.device/%s", f.UniqueName)
		devices := []kubeletdevicepluginv1beta1.Device{{ID: "testdevice", Health: kubeletdevicepluginv1beta1.Healthy}}

		// Initially, there are no allocatable of this resource
		gomega.Eventually(getNodeResourceValues, nodeStatusUpdateTimeout, f.Timeouts.Poll).WithContext(ctx).WithArguments(resourceName).Should(gomega.Equal(ResourceValue{Allocatable: -1, Capacity: -1}))

		plugin, _ := testdeviceplugin.NewDevicePlugin(resourceName, f.UniqueName, true, devices, func(name string) error {
			if name == "ListAndWatch" {
				return fmt.Errorf("ListAndWatch failed")
			}
			return nil
		})

		err := plugin.RegisterDevicePlugin(ctx)
		defer plugin.Stop() // should stop even if registration failed
		gomega.Expect(err).To(gomega.Succeed())

		// kubelet registers the resource, but will not have any allocatable
		gomega.Eventually(getNodeResourceValues, nodeStatusUpdateTimeout, f.Timeouts.Poll).WithContext(ctx).WithArguments(resourceName).Should(gomega.Equal(ResourceValue{Allocatable: 0, Capacity: 0}))

		// kubelet will never retry ListAndWatch (this will sleep for a long time)
		gomega.Consistently(plugin.Calls, devicePluginUpdateTimeout, f.Timeouts.Poll).Should(gomega.HaveLen(2))

		// however kubelet will not delete the resource
		gomega.Eventually(getNodeResourceValues, nodeStatusUpdateTimeout, f.Timeouts.Poll).WithContext(ctx).WithArguments(resourceName).Should(gomega.Equal(ResourceValue{Allocatable: 0, Capacity: 0}))
	})

	ginkgo.It("when ListAndWatch fails after provisioning devices, node allocatable will be set to zero and kubelet will not retry to list resources", func(ctx context.Context) {
		// randomizing so tests can run in parallel
		resourceName := fmt.Sprintf("test.device/%s", f.UniqueName)
		devices := []kubeletdevicepluginv1beta1.Device{
			{ID: "0", Health: kubeletdevicepluginv1beta1.Healthy},
			{ID: "1", Health: kubeletdevicepluginv1beta1.Healthy},
		}

		failing := false
		plugin, _ := testdeviceplugin.NewDevicePlugin(resourceName, f.UniqueName, true, devices, func(name string) error {
			if name == "ListAndWatch" {
				if failing {
					return fmt.Errorf("ListAndWatch failed")
				}
			}
			return nil
		})

		err := plugin.RegisterDevicePlugin(ctx)
		defer plugin.Stop() // should stop even if registration failed
		gomega.Expect(err).To(gomega.Succeed())

		// at first the device is healthy
		gomega.Eventually(getNodeResourceValues, nodeStatusUpdateTimeout, f.Timeouts.Poll).WithContext(ctx).WithArguments(resourceName).Should(gomega.Equal(ResourceValue{Allocatable: 2, Capacity: 2}))

		// let's make ListAndWatch fail
		failing = true

		// kubelet will mark all devices as unhealthy
		gomega.Eventually(getNodeResourceValues, nodeStatusUpdateTimeout, f.Timeouts.Poll).WithContext(ctx).WithArguments(resourceName).Should(gomega.Equal(ResourceValue{Allocatable: 0, Capacity: 2}))

		// kubelet will never retry ListAndWatch (this will sleep for a long time)
		gomega.Consistently(plugin.Calls, devicePluginUpdateTimeout, f.Timeouts.Poll).Should(gomega.HaveLen(2))

		// however kubelet will not delete the resource and will keep the capacity
		gomega.Eventually(getNodeResourceValues, nodeStatusUpdateTimeout, f.Timeouts.Poll).WithContext(ctx).WithArguments(resourceName).Should(gomega.Equal(ResourceValue{Allocatable: 0, Capacity: 2}))

		// after the graceful period devices capacity will reset to zero
		gomega.Eventually(getNodeResourceValues, devicePluginGracefulTimeout+1*time.Minute, f.Timeouts.Poll).WithContext(ctx).WithArguments(resourceName).Should(gomega.Equal(ResourceValue{Allocatable: 0, Capacity: 0}))
	})

	ginkgo.It("when device plugin is stopped after provisioning devices, node allocatable will be set to zero", func(ctx context.Context) {
		// randomizing so tests can run in parallel
		resourceName := fmt.Sprintf("test.device/%s", f.UniqueName)
		devices := []kubeletdevicepluginv1beta1.Device{
			{ID: "0", Health: kubeletdevicepluginv1beta1.Healthy},
			{ID: "1", Health: kubeletdevicepluginv1beta1.Healthy},
		}

		gomega.Eventually(getNodeResourceValues, nodeStatusUpdateTimeout, f.Timeouts.Poll).WithContext(ctx).WithArguments(resourceName).Should(gomega.Equal(ResourceValue{Allocatable: -1, Capacity: -1}))

		plugin, _ := testdeviceplugin.NewDevicePlugin(resourceName, f.UniqueName, true, devices, nil)

		err := plugin.RegisterDevicePlugin(ctx)
		defer plugin.Stop() // should stop even if registration failed
		gomega.Expect(err).To(gomega.Succeed())

		// at first the device is healthy
		gomega.Eventually(getNodeResourceValues, nodeStatusUpdateTimeout, f.Timeouts.Poll).WithContext(ctx).WithArguments(resourceName).Should(gomega.Equal(ResourceValue{Allocatable: 2, Capacity: 2}))

		// let's unload the plugin
		plugin.Stop()

		// kubelet will mark all devices as unhealthy
		gomega.Eventually(getNodeResourceValues, nodeStatusUpdateTimeout, f.Timeouts.Poll).WithContext(ctx).WithArguments(resourceName).Should(gomega.Equal(ResourceValue{Allocatable: 0, Capacity: 2}))

		// after the graceful period devices capacity will reset to zero
		gomega.Eventually(getNodeResourceValues, devicePluginGracefulTimeout+1*time.Minute, f.Timeouts.Poll).WithContext(ctx).WithArguments(resourceName).Should(gomega.Equal(ResourceValue{Allocatable: 0, Capacity: 0}))
	})
})
