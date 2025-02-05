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

package node

import (
	"context"
	"os"
	"regexp"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/uuid"
	clientset "k8s.io/client-go/kubernetes"
	extensionsinternal "k8s.io/kubernetes/pkg/apis/extensions"
	"k8s.io/kubernetes/test/e2e/feature"
	"k8s.io/kubernetes/test/e2e/framework"
	e2egpu "k8s.io/kubernetes/test/e2e/framework/gpu"
	e2ejob "k8s.io/kubernetes/test/e2e/framework/job"
	e2emanifest "k8s.io/kubernetes/test/e2e/framework/manifest"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2eresource "k8s.io/kubernetes/test/e2e/framework/resource"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
	e2etestfiles "k8s.io/kubernetes/test/e2e/framework/testfiles"
	admissionapi "k8s.io/pod-security-admission/api"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
)

// NOTE: All the tests in this file are run serially because they share a limited set of GPU(s), please inspect
// the CI job definitions to see how many GPU(s) are available in the environment
// Currently the CI jobs have 2 nodes each with 4 Nvidia T4's across both GCE and AWS harness(es).

var _ = SIGDescribe(feature.GPUDevicePlugin, framework.WithSerial(), "Sanity test using nvidia-smi", func() {

	f := framework.NewDefaultFramework("nvidia-gpu1")
	f.NamespacePodSecurityLevel = admissionapi.LevelPrivileged
	var podClient *e2epod.PodClient

	ginkgo.BeforeEach(func() {
		e2eskipper.SkipUnlessProviderIs("aws", "gce")
		podClient = e2epod.NewPodClient(f)
	})

	f.It("should run nvidia-smi and cuda-demo-suite", func(ctx context.Context) {
		SetupEnvironmentAndSkipIfNeeded(ctx, f, f.ClientSet)
		pod := testNvidiaCLIPod()

		ginkgo.By("Creating a pod that runs nvidia-smi")
		createAndValidatePod(ctx, f, podClient, pod)

		ginkgo.By("Getting logs from the pod")
		log, err := e2epod.GetPodLogs(ctx, f.ClientSet, f.Namespace.Name, pod.Name, pod.Spec.Containers[0].Name)
		framework.ExpectNoError(err)

		ginkgo.By("Checking output from nvidia-smi")
		gomega.Expect(log).To(gomega.ContainSubstring("NVIDIA-SMI"))
		gomega.Expect(log).To(gomega.ContainSubstring("Driver Version:"))
		gomega.Expect(log).To(gomega.ContainSubstring("CUDA Version:"))
	})
})

var _ = SIGDescribe(feature.GPUDevicePlugin, framework.WithSerial(), "Test using a Pod", func() {

	f := framework.NewDefaultFramework("nvidia-gpu2")
	f.NamespacePodSecurityLevel = admissionapi.LevelPrivileged
	var podClient *e2epod.PodClient

	ginkgo.BeforeEach(func() {
		e2eskipper.SkipUnlessProviderIs("aws", "gce")
		podClient = e2epod.NewPodClient(f)
	})

	f.It("should run gpu based matrix multiplication", func(ctx context.Context) {
		SetupEnvironmentAndSkipIfNeeded(ctx, f, f.ClientSet)
		pod := testMatrixMultiplicationPod()

		ginkgo.By("Creating a pod that runs matrix multiplication")
		createAndValidatePod(ctx, f, podClient, pod)

		ginkgo.By("Getting logs from the pod")
		log, err := e2epod.GetPodLogs(ctx, f.ClientSet, f.Namespace.Name, pod.Name, pod.Spec.Containers[0].Name)
		framework.ExpectNoError(err)

		ginkgo.By("Checking output from nvidia-smi")
		framework.Logf("Got container logs for %s:\n%v", pod.Spec.Containers[0].Name, log)

		gomega.Expect(log).To(gomega.ContainSubstring("TensorFlow version"))
		gomega.Expect(log).To(gomega.ContainSubstring("Matrix multiplication result:"))
		gomega.Expect(log).To(gomega.ContainSubstring("Time taken for 5000x5000 matrix multiplication"))
	})
})

var _ = SIGDescribe(feature.GPUDevicePlugin, framework.WithSerial(), "Test using a Job", func() {

	f := framework.NewDefaultFramework("nvidia-gpu2")
	f.NamespacePodSecurityLevel = admissionapi.LevelPrivileged

	ginkgo.BeforeEach(func() {
		e2eskipper.SkipUnlessProviderIs("aws", "gce")
	})

	f.It("should run gpu based jobs", func(ctx context.Context) {
		SetupEnvironmentAndSkipIfNeeded(ctx, f, f.ClientSet)

		// Job set to have 5 completions with parallelism of 1 to ensure that it lasts long enough to experience the node recreation
		completions := int32(5)
		ginkgo.By("Starting GPU job")
		StartJob(ctx, f, completions)

		job, err := e2ejob.GetJob(ctx, f.ClientSet, f.Namespace.Name, "cuda-add")
		framework.ExpectNoError(err)

		// make sure job is running by waiting for its first pod to start running
		err = e2ejob.WaitForJobPodsRunningWithTimeout(ctx, f.ClientSet, f.Namespace.Name, job.Name, 1, e2ejob.JobTimeout*2)
		framework.ExpectNoError(err)

		numNodes, err := e2enode.TotalRegistered(ctx, f.ClientSet)
		framework.ExpectNoError(err)
		_, err = e2enode.CheckReady(ctx, f.ClientSet, numNodes, framework.NodeReadyInitialTimeout)
		framework.ExpectNoError(err)

		ginkgo.By("Waiting for gpu job to finish")
		err = e2ejob.WaitForJobFinishWithTimeout(ctx, f.ClientSet, f.Namespace.Name, job.Name, e2ejob.JobTimeout*2)
		framework.ExpectNoError(err)
		ginkgo.By("Done with gpu job")

		gomega.Expect(job.Status.Failed).To(gomega.BeZero(), "Job pods failed during node recreation: %v", job.Status.Failed)

		VerifyJobNCompletions(ctx, f, completions)
	})
})

func createAndValidatePod(ctx context.Context, f *framework.Framework, podClient *e2epod.PodClient, pod *v1.Pod) {
	pod = podClient.Create(ctx, pod)

	ginkgo.By("Watching for error events or started pod")
	ev, err := podClient.WaitForErrorEventOrSuccessWithTimeout(ctx, pod, framework.PodStartTimeout*6)
	framework.ExpectNoError(err)
	gomega.Expect(ev).To(gomega.BeNil())

	ginkgo.By("Waiting for pod completion")
	err = e2epod.WaitForPodNoLongerRunningInNamespace(ctx, f.ClientSet, pod.Name, f.Namespace.Name)
	framework.ExpectNoError(err)
	pod, err = podClient.Get(ctx, pod.Name, metav1.GetOptions{})
	framework.ExpectNoError(err)

	ginkgo.By("Checking that the pod succeeded")
	gomega.Expect(pod.Status.Phase).To(gomega.Equal(v1.PodSucceeded))
}

func testNvidiaCLIPod() *v1.Pod {
	podName := "gpu-cli-" + string(uuid.NewUUID())
	pod := v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        podName,
			Annotations: map[string]string{},
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name:  "nvidia-smi",
					Image: "nvidia/cuda:12.5.0-devel-ubuntu22.04",
					Command: []string{
						"bash",
						"-c",
						`
nvidia-smi
apt-get update -y && \
	DEBIAN_FRONTEND=noninteractive apt-get install -y --allow-unauthenticated cuda-demo-suite-12-5
/usr/local/cuda/extras/demo_suite/deviceQuery
/usr/local/cuda/extras/demo_suite/vectorAdd
/usr/local/cuda/extras/demo_suite/bandwidthTest --device=all --csv
/usr/local/cuda/extras/demo_suite/busGrind -a
`,
					},
					Resources: v1.ResourceRequirements{
						Limits: v1.ResourceList{
							"nvidia.com/gpu": resource.MustParse("1"),
						},
					},
				},
			},
			RestartPolicy: v1.RestartPolicyNever,
		},
	}
	return &pod
}

func testMatrixMultiplicationPod() *v1.Pod {
	podName := "gpu-matmul-" + string(uuid.NewUUID())
	pod := v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        podName,
			Annotations: map[string]string{},
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name:  "gpu-matmul",
					Image: "tensorflow/tensorflow:latest-gpu",
					Command: []string{
						"python",
						"-c",
						`
import tensorflow as tf
import time

print("TensorFlow version:", tf.__version__)
print("Num GPUs Available: ", len(tf.config.experimental.list_physical_devices('GPU')))

# Simple matrix multiplication test
with tf.device('/GPU:0'):
    a = tf.constant([[1.0, 2.0, 3.0], [4.0, 5.0, 6.0]])
    b = tf.constant([[1.0, 2.0], [3.0, 4.0], [5.0, 6.0]])
    c = tf.matmul(a, b)

print("Matrix multiplication result:", c.numpy())

# Performance test
n = 5000
start_time = time.time()
with tf.device('/GPU:0'):
    matrix1 = tf.random.normal((n, n))
    matrix2 = tf.random.normal((n, n))
    result = tf.matmul(matrix1, matrix2)
end_time = time.time()

print(f"Time taken for {n}x{n} matrix multiplication: {end_time - start_time:.2f} seconds")
`,
					},
					Resources: v1.ResourceRequirements{
						Limits: v1.ResourceList{
							"nvidia.com/gpu": resource.MustParse("1"),
						},
					},
				},
			},
			RestartPolicy: v1.RestartPolicyNever,
		},
	}
	return &pod
}

func SetupEnvironmentAndSkipIfNeeded(ctx context.Context, f *framework.Framework, clientSet clientset.Interface) {
	if framework.ProviderIs("gce") {
		SetupNVIDIAGPUNode(ctx, f)
	} else if framework.ProviderIs("aws") {
		// see nvidia-device-plugin.yml in https://github.com/NVIDIA/k8s-device-plugin/tree/main/deployments/static
		waitForGPUs(ctx, f, "kube-system", "nvidia-device-plugin-daemonset")
	}

	nodes, err := e2enode.GetReadySchedulableNodes(ctx, clientSet)
	framework.ExpectNoError(err)
	capacity := 0
	allocatable := 0
	for _, node := range nodes.Items {
		val, ok := node.Status.Capacity[e2egpu.NVIDIAGPUResourceName]
		if !ok {
			continue
		}
		capacity += int(val.Value())
		val, ok = node.Status.Allocatable[e2egpu.NVIDIAGPUResourceName]
		if !ok {
			continue
		}
		allocatable += int(val.Value())
	}
	if capacity == 0 {
		framework.Failf("%d ready nodes do not have any Nvidia GPU(s). Bailing out...", len(nodes.Items))
	}
	if allocatable == 0 {
		framework.Failf("%d ready nodes do not have any allocatable Nvidia GPU(s). Bailing out...", len(nodes.Items))
	}
}

func areGPUsAvailableOnAllSchedulableNodes(ctx context.Context, clientSet clientset.Interface) bool {
	framework.Logf("Getting list of Nodes from API server")
	nodeList, err := clientSet.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		framework.Logf("Unexpected error getting node list: %v", err)
		return false
	}
	for _, node := range nodeList.Items {
		if node.Spec.Unschedulable {
			continue
		}
		if _, ok := node.Labels[framework.ControlPlaneLabel]; ok {
			continue
		}
		framework.Logf("gpuResourceName %s", e2egpu.NVIDIAGPUResourceName)
		if val, ok := node.Status.Capacity[e2egpu.NVIDIAGPUResourceName]; !ok || val.Value() == 0 {
			framework.Logf("Nvidia GPUs not available on Node: %q", node.Name)
			return false
		}
	}
	framework.Logf("Nvidia GPUs exist on all schedulable nodes")
	return true
}

func logOSImages(ctx context.Context, f *framework.Framework) {
	nodeList, err := f.ClientSet.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	framework.ExpectNoError(err, "getting node list")
	for _, node := range nodeList.Items {
		framework.Logf("Nodename: %v, OS Image: %v", node.Name, node.Status.NodeInfo.OSImage)
	}
}

const (
	// Nvidia driver installation can take upwards of 5 minutes.
	driverInstallTimeout = 10 * time.Minute
)

// SetupNVIDIAGPUNode install Nvidia Drivers and wait for Nvidia GPUs to be available on nodes
func SetupNVIDIAGPUNode(ctx context.Context, f *framework.Framework) {
	logOSImages(ctx, f)

	var err error
	var ds *appsv1.DaemonSet
	dsYamlURLFromEnv := os.Getenv("NVIDIA_DRIVER_INSTALLER_DAEMONSET")
	if dsYamlURLFromEnv != "" {
		// Using DaemonSet from remote URL
		framework.Logf("Using remote nvidia-driver-installer daemonset manifest from %v", dsYamlURLFromEnv)
		ds, err = e2emanifest.DaemonSetFromURL(ctx, dsYamlURLFromEnv)
		framework.ExpectNoError(err, "failed get remote")
	} else {
		// Using default local DaemonSet
		framework.Logf("Using default local nvidia-driver-installer daemonset manifest.")
		data, err := e2etestfiles.Read("test/e2e/testing-manifests/gpu/gce/nvidia-driver-installer.yaml")
		framework.ExpectNoError(err, "failed to read local manifest for nvidia-driver-installer daemonset")
		ds, err = e2emanifest.DaemonSetFromData(data)
		framework.ExpectNoError(err, "failed to parse local manifest for nvidia-driver-installer daemonset")
	}

	prev, err := f.ClientSet.AppsV1().DaemonSets(f.Namespace.Name).Get(ctx, ds.Name, metav1.GetOptions{})
	if err == nil && prev != nil {
		framework.Logf("nvidia-driver-installer Daemonset already installed, skipping...")
	} else {
		ds.Namespace = f.Namespace.Name
		_, err = f.ClientSet.AppsV1().DaemonSets(f.Namespace.Name).Create(ctx, ds, metav1.CreateOptions{})
		framework.ExpectNoError(err, "failed to create nvidia-driver-installer daemonset")
		framework.Logf("Successfully created daemonset to install Nvidia drivers.")
	}

	data, err := e2etestfiles.Read("test/e2e/testing-manifests/gpu/gce/nvidia-gpu-device-plugin.yaml")
	framework.ExpectNoError(err, "failed to read local manifest for nvidia-gpu-device-plugin daemonset")
	ds, err = e2emanifest.DaemonSetFromData(data)
	framework.ExpectNoError(err, "failed to parse local manifest for nvidia-gpu-device-plugin daemonset")

	prev, err = f.ClientSet.AppsV1().DaemonSets(ds.Namespace).Get(ctx, ds.Name, metav1.GetOptions{})
	if err == nil && prev != nil {
		framework.Logf("nvidia-gpu-device-plugin Daemonset already installed, skipping...")
	} else {
		_, err = f.ClientSet.AppsV1().DaemonSets(ds.Namespace).Create(ctx, ds, metav1.CreateOptions{})
		framework.ExpectNoError(err, "failed to create nvidia-gpu-device-plugin daemonset")
		framework.Logf("Successfully created daemonset to install Nvidia device plugin.")
	}

	waitForGPUs(ctx, f, ds.Namespace, ds.Name)
}

func waitForGPUs(ctx context.Context, f *framework.Framework, namespace, name string) {
	pods, err := e2eresource.WaitForControlledPods(ctx, f.ClientSet, namespace, name, extensionsinternal.Kind("DaemonSet"))
	framework.ExpectNoError(err, "failed to get pods controlled by the nvidia-driver-installer daemonset")

	devicepluginPods, err := e2eresource.WaitForControlledPods(ctx, f.ClientSet, "kube-system", "nvidia-gpu-device-plugin", extensionsinternal.Kind("DaemonSet"))
	if err == nil {
		framework.Logf("Adding deviceplugin addon pod.")
		pods.Items = append(pods.Items, devicepluginPods.Items...)
	}

	// Wait for Nvidia GPUs to be available on nodes
	framework.Logf("Waiting for drivers to be installed and GPUs to be available in Node Capacity...")
	gomega.Eventually(ctx, func(ctx context.Context) bool {
		return areGPUsAvailableOnAllSchedulableNodes(ctx, f.ClientSet)
	}, driverInstallTimeout, time.Second).Should(gomega.BeTrueBecause("expected GPU resources to be available within the timout"))
}

// StartJob starts a simple CUDA job that requests gpu and the specified number of completions
func StartJob(ctx context.Context, f *framework.Framework, completions int32) {
	var activeSeconds int64 = 3600
	testJob := e2ejob.NewTestJob("succeed", "cuda-add", v1.RestartPolicyAlways, 1, completions, &activeSeconds, 6)
	testJob.Spec.Template.Spec = v1.PodSpec{
		RestartPolicy: v1.RestartPolicyOnFailure,
		Containers: []v1.Container{
			{
				Name:  "vector-addition",
				Image: "cupy/cupy:v13.3.0",
				Command: []string{
					"python3",
					"-c",
					`
import cupy as cp
import numpy as np
import time

# Set the number of elements to test
num_elements_list = [10, 100, 1000, 10000, 100000, 1000000]

for num_elements in num_elements_list:
    # Create random input vectors on the CPU
    h_A = np.random.rand(num_elements).astype(np.float32)
    h_B = np.random.rand(num_elements).astype(np.float32)

    # Transfer the input vectors to the GPU
    d_A = cp.asarray(h_A)
    d_B = cp.asarray(h_B)

    # Perform vector addition on the GPU
    start_gpu = time.time()
    d_C = d_A + d_B
    gpu_time = time.time() - start_gpu

    # Transfer the result back to the CPU
    h_C = cp.asnumpy(d_C)

    # Compute the expected result on the CPU
    start_cpu = time.time()
    h_C_expected = h_A + h_B
    cpu_time = time.time() - start_cpu

    # Verify the result
    if np.allclose(h_C_expected, h_C, atol=1e-5):
        print(f"GPU time: {gpu_time:.6f} seconds")
        print(f"CPU time: {cpu_time:.6f} seconds")
        print(f"GPU speedup: {cpu_time / gpu_time:.2f}x")
    else:
        print(f"Test FAILED for {num_elements} elements.")

    # Print the first few elements for verification
    print("First few elements of A:", h_A[:5])
    print("First few elements of B:", h_B[:5])
    print("First few elements of C:", h_C[:5])

print(f"Test PASSED")
`,
				},
				Resources: v1.ResourceRequirements{
					Limits: v1.ResourceList{
						e2egpu.NVIDIAGPUResourceName: *resource.NewQuantity(1, resource.DecimalSI),
					},
				},
			},
		},
	}
	ns := f.Namespace.Name
	_, err := e2ejob.CreateJob(ctx, f.ClientSet, ns, testJob)
	framework.ExpectNoError(err)
	framework.Logf("Created job %v", testJob)
}

func podNames(pods []v1.Pod) []string {
	originalPodNames := make([]string, len(pods))
	for i, p := range pods {
		originalPodNames[i] = p.ObjectMeta.Name
	}
	return originalPodNames
}

// VerifyJobNCompletions verifies that the job has completions number of successful pods
func VerifyJobNCompletions(ctx context.Context, f *framework.Framework, completions int32) {
	ns := f.Namespace.Name
	pods, err := e2ejob.GetJobPods(ctx, f.ClientSet, f.Namespace.Name, "cuda-add")
	framework.ExpectNoError(err)
	createdPods := pods.Items
	createdPodNames := podNames(createdPods)
	framework.Logf("Got the following pods for job cuda-add: %v", createdPodNames)

	successes := int32(0)
	regex := regexp.MustCompile("PASSED")
	for _, podName := range createdPodNames {
		e2epod.NewPodClient(f).WaitForFinish(ctx, podName, 5*time.Minute)
		logs, err := e2epod.GetPodLogs(ctx, f.ClientSet, ns, podName, "vector-addition")
		framework.ExpectNoError(err, "Should be able to get logs for pod %v", podName)
		if regex.MatchString(logs) {
			successes++
		}
		gomega.Expect(logs).To(gomega.Not(gomega.ContainSubstring("FAILED")))
	}
	if successes != completions {
		framework.Failf("Only got %v completions. Expected %v completions.", successes, completions)
	}
}
