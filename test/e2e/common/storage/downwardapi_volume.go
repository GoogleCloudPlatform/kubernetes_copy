/*
Copyright 2016 The Kubernetes Authors.

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

package storage

import (
	"context"
	"fmt"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/kubernetes/test/e2e/framework"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2epodoutput "k8s.io/kubernetes/test/e2e/framework/pod/output"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
	"k8s.io/kubernetes/test/e2e/nodefeature"
	imageutils "k8s.io/kubernetes/test/utils/image"
	admissionapi "k8s.io/pod-security-admission/api"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
)

var _ = SIGDescribe("Downward API volume", func() {
	// How long to wait for a log pod to be displayed
	const podLogTimeout = 3 * time.Minute
	f := framework.NewDefaultFramework("downward-api")
	f.NamespacePodSecurityLevel = admissionapi.LevelBaseline
	var podClient *e2epod.PodClient
	ginkgo.BeforeEach(func() {
		podClient = e2epod.NewPodClient(f)
	})

	/*
		Release: v1.9
		Testname: DownwardAPI volume, pod name
		Description: A Pod is configured with DownwardAPIVolumeSource and DownwardAPIVolumeFiles contains a item for the Pod name. The container runtime MUST be able to access Pod name from the specified path on the mounted volume.
	*/
	framework.ConformanceIt("should provide podname only", f.WithNodeConformance(), func(ctx context.Context) {
		podName := "downwardapi-volume-" + string(uuid.NewUUID())
		pod := downwardAPIVolumePodForSimpleTest(podName, "/etc/podinfo/podname")

		e2epodoutput.TestContainerOutput(ctx, f, "downward API volume plugin", pod, 0, []string{
			fmt.Sprintf("%s\n", podName),
		})
	})

	/*
		Release: v1.9
		Testname: DownwardAPI volume, volume mode 0400
		Description: A Pod is configured with DownwardAPIVolumeSource with the volumesource mode set to -r-------- and DownwardAPIVolumeFiles contains a item for the Pod name. The container runtime MUST be able to access Pod name from the specified path on the mounted volume.
	*/
	framework.ConformanceIt("should set DefaultMode on files", f.WithNodeConformance(), func(ctx context.Context) {
		podName := "downwardapi-volume-" + string(uuid.NewUUID())
		defaultMode := int32(0400)
		pod := downwardAPIVolumePodForModeTest(podName, "/etc/podinfo/podname", nil, &defaultMode)

		fileModeRegexp := getFileModeRegex("/etc/podinfo/podname", &defaultMode)
		e2epodoutput.TestContainerOutputRegexp(ctx, f, "downward API volume plugin", pod, 0, []string{
			fileModeRegexp,
		})
	})

	/*
		Release: v1.9
		Testname: DownwardAPI volume, file mode 0400
		Description: A Pod is configured with DownwardAPIVolumeSource and DownwardAPIVolumeFiles contains a item for the Pod name with the file mode set to -r--------. The container runtime MUST be able to access Pod name from the specified path on the mounted volume.
	*/
	framework.ConformanceIt("should set mode on item file", f.WithNodeConformance(), func(ctx context.Context) {
		podName := "downwardapi-volume-" + string(uuid.NewUUID())
		mode := int32(0400)
		pod := downwardAPIVolumePodForModeTest(podName, "/etc/podinfo/podname", &mode, nil)

		fileModeRegexp := getFileModeRegex("/etc/podinfo/podname", &mode)
		e2epodoutput.TestContainerOutputRegexp(ctx, f, "downward API volume plugin", pod, 0, []string{
			fileModeRegexp,
		})
	})

	f.It("should provide podname as non-root with fsgroup [LinuxOnly]", nodefeature.FSGroup, func(ctx context.Context) {
		// Windows does not support RunAsUser / FSGroup SecurityContext options.
		e2eskipper.SkipIfNodeOSDistroIs("windows")
		podName := "metadata-volume-" + string(uuid.NewUUID())
		gid := int64(1234)
		pod := downwardAPIVolumePodForSimpleTest(podName, "/etc/podinfo/podname")
		pod.Spec.SecurityContext = &v1.PodSecurityContext{
			FSGroup: &gid,
		}
		setPodNonRootUser(pod)
		e2epodoutput.TestContainerOutput(ctx, f, "downward API volume plugin", pod, 0, []string{
			fmt.Sprintf("%s\n", podName),
		})
	})

	f.It("should provide podname as non-root with fsgroup and defaultMode [LinuxOnly]", nodefeature.FSGroup, func(ctx context.Context) {
		// Windows does not support RunAsUser / FSGroup SecurityContext options.
		e2eskipper.SkipIfNodeOSDistroIs("windows")
		podName := "metadata-volume-" + string(uuid.NewUUID())
		gid := int64(1234)
		mode := int32(0440) /* setting fsGroup sets mode to at least 440 */
		pod := downwardAPIVolumePodForModeTest(podName, "/etc/podinfo/podname", &mode, nil)
		pod.Spec.SecurityContext = &v1.PodSecurityContext{
			FSGroup: &gid,
		}
		setPodNonRootUser(pod)
		e2epodoutput.TestContainerOutput(ctx, f, "downward API volume plugin", pod, 0, []string{
			"mode of file \"/etc/podinfo/podname\": -r--r-----",
		})
	})

	/*
		Release: v1.9
		Testname: DownwardAPI volume, update label
		Description: A Pod is configured with DownwardAPIVolumeSource and DownwardAPIVolumeFiles contains list of items for each of the Pod labels. The container runtime MUST be able to access Pod labels from the specified path on the mounted volume. Update the labels by adding a new label to the running Pod. The new label MUST be available from the mounted volume.
	*/
	framework.ConformanceIt("should update labels on modification", f.WithNodeConformance(), func(ctx context.Context) {
		labels := map[string]string{}
		labels["key1"] = "value1"
		labels["key2"] = "value2"

		podName := "labelsupdate" + string(uuid.NewUUID())
		pod := downwardAPIVolumePodForUpdateTest(podName, labels, map[string]string{}, "/etc/podinfo/labels")
		containerName := "client-container"
		ginkgo.By("Creating the pod")
		podClient.CreateSync(ctx, pod)

		gomega.Eventually(ctx, func() (string, error) {
			return e2epod.GetPodLogs(ctx, f.ClientSet, f.Namespace.Name, podName, containerName)
		},
			podLogTimeout, framework.Poll).Should(gomega.ContainSubstring("key1=\"value1\"\n"))

		//modify labels
		podClient.Update(ctx, podName, func(pod *v1.Pod) {
			pod.Labels["key3"] = "value3"
		})

		gomega.Eventually(ctx, func() (string, error) {
			return e2epod.GetPodLogs(ctx, f.ClientSet, f.Namespace.Name, pod.Name, containerName)
		},
			podLogTimeout, framework.Poll).Should(gomega.ContainSubstring("key3=\"value3\"\n"))
	})

	/*
		Release: v1.9
		Testname: DownwardAPI volume, update annotations
		Description: A Pod is configured with DownwardAPIVolumeSource and DownwardAPIVolumeFiles contains list of items for each of the Pod annotations. The container runtime MUST be able to access Pod annotations from the specified path on the mounted volume. Update the annotations by adding a new annotation to the running Pod. The new annotation MUST be available from the mounted volume.
	*/
	framework.ConformanceIt("should update annotations on modification", f.WithNodeConformance(), func(ctx context.Context) {
		annotations := map[string]string{}
		annotations["builder"] = "bar"
		podName := "annotationupdate" + string(uuid.NewUUID())
		pod := downwardAPIVolumePodForUpdateTest(podName, map[string]string{}, annotations, "/etc/podinfo/annotations")

		containerName := "client-container"
		ginkgo.By("Creating the pod")
		pod = podClient.CreateSync(ctx, pod)

		gomega.Eventually(ctx, func() (string, error) {
			return e2epod.GetPodLogs(ctx, f.ClientSet, f.Namespace.Name, pod.Name, containerName)
		},
			podLogTimeout, framework.Poll).Should(gomega.ContainSubstring("builder=\"bar\"\n"))

		//modify annotations
		podClient.Update(ctx, podName, func(pod *v1.Pod) {
			pod.Annotations["builder"] = "foo"
		})

		gomega.Eventually(ctx, func() (string, error) {
			return e2epod.GetPodLogs(ctx, f.ClientSet, f.Namespace.Name, pod.Name, containerName)
		},
			podLogTimeout, framework.Poll).Should(gomega.ContainSubstring("builder=\"foo\"\n"))
	})

	/*
		Release: v1.9
		Testname: DownwardAPI volume, CPU limits
		Description: A Pod is configured with DownwardAPIVolumeSource and DownwardAPIVolumeFiles contains a item for the CPU limits. The container runtime MUST be able to access CPU limits from the specified path on the mounted volume.
	*/
	framework.ConformanceIt("should provide container's cpu limit", f.WithNodeConformance(), func(ctx context.Context) {
		podName := "downwardapi-volume-" + string(uuid.NewUUID())
		pod := downwardAPIVolumeForContainerResources(podName, "/etc/podinfo/cpu_limit")

		e2epodoutput.TestContainerOutput(ctx, f, "downward API volume plugin", pod, 0, []string{
			fmt.Sprintf("2\n"),
		})
	})

	/*
		Release: v1.9
		Testname: DownwardAPI volume, memory limits
		Description: A Pod is configured with DownwardAPIVolumeSource and DownwardAPIVolumeFiles contains a item for the memory limits. The container runtime MUST be able to access memory limits from the specified path on the mounted volume.
	*/
	framework.ConformanceIt("should provide container's memory limit", f.WithNodeConformance(), func(ctx context.Context) {
		podName := "downwardapi-volume-" + string(uuid.NewUUID())
		pod := downwardAPIVolumeForContainerResources(podName, "/etc/podinfo/memory_limit")

		e2epodoutput.TestContainerOutput(ctx, f, "downward API volume plugin", pod, 0, []string{
			"134217728\n",
		})
	})

	/*
		Release: v1.9
		Testname: DownwardAPI volume, CPU request
		Description: A Pod is configured with DownwardAPIVolumeSource and DownwardAPIVolumeFiles contains a item for the CPU request. The container runtime MUST be able to access CPU request from the specified path on the mounted volume.
	*/
	framework.ConformanceIt("should provide container's cpu request", f.WithNodeConformance(), func(ctx context.Context) {
		podName := "downwardapi-volume-" + string(uuid.NewUUID())
		pod := downwardAPIVolumeForContainerResources(podName, "/etc/podinfo/cpu_request")

		e2epodoutput.TestContainerOutput(ctx, f, "downward API volume plugin", pod, 0, []string{
			fmt.Sprintf("1\n"),
		})
	})

	/*
		Release: v1.9
		Testname: DownwardAPI volume, memory request
		Description: A Pod is configured with DownwardAPIVolumeSource and DownwardAPIVolumeFiles contains a item for the memory request. The container runtime MUST be able to access memory request from the specified path on the mounted volume.
	*/
	framework.ConformanceIt("should provide container's memory request", f.WithNodeConformance(), func(ctx context.Context) {
		podName := "downwardapi-volume-" + string(uuid.NewUUID())
		pod := downwardAPIVolumeForContainerResources(podName, "/etc/podinfo/memory_request")

		e2epodoutput.TestContainerOutput(ctx, f, "downward API volume plugin", pod, 0, []string{
			fmt.Sprintf("33554432\n"),
		})
	})

	/*
		Release: v1.9
		Testname: DownwardAPI volume, CPU limit, default node allocatable
		Description: A Pod is configured with DownwardAPIVolumeSource and DownwardAPIVolumeFiles contains a item for the CPU limits. CPU limits is not specified for the container. The container runtime MUST be able to access CPU limits from the specified path on the mounted volume and the value MUST be default node allocatable.
	*/
	framework.ConformanceIt("should provide node allocatable (cpu) as default cpu limit if the limit is not set", f.WithNodeConformance(), func(ctx context.Context) {
		podName := "downwardapi-volume-" + string(uuid.NewUUID())
		pod := downwardAPIVolumeForDefaultContainerResources(podName, "/etc/podinfo/cpu_limit")

		e2epodoutput.TestContainerOutputRegexp(ctx, f, "downward API volume plugin", pod, 0, []string{"[1-9]"})
	})

	/*
		Release: v1.9
		Testname: DownwardAPI volume, memory limit, default node allocatable
		Description: A Pod is configured with DownwardAPIVolumeSource and DownwardAPIVolumeFiles contains a item for the memory limits. memory limits is not specified for the container. The container runtime MUST be able to access memory limits from the specified path on the mounted volume and the value MUST be default node allocatable.
	*/
	framework.ConformanceIt("should provide node allocatable (memory) as default memory limit if the limit is not set", f.WithNodeConformance(), func(ctx context.Context) {
		podName := "downwardapi-volume-" + string(uuid.NewUUID())
		pod := downwardAPIVolumeForDefaultContainerResources(podName, "/etc/podinfo/memory_limit")

		e2epodoutput.TestContainerOutputRegexp(ctx, f, "downward API volume plugin", pod, 0, []string{"[1-9]"})
	})
})

func downwardAPIVolumePodForModeTest(name, filePath string, itemMode, defaultMode *int32) *v1.Pod {
	pod := downwardAPIVolumeBasePod(name, nil, nil)

	pod.Spec.Containers = []v1.Container{
		{
			Name:  "client-container",
			Image: imageutils.GetE2EImage(imageutils.Agnhost),
			Args:  []string{"mounttest", "--file_mode=" + filePath},
			VolumeMounts: []v1.VolumeMount{
				{
					Name:      "podinfo",
					MountPath: "/etc/podinfo",
				},
			},
		},
	}
	if itemMode != nil {
		pod.Spec.Volumes[0].VolumeSource.DownwardAPI.Items[0].Mode = itemMode
	}
	if defaultMode != nil {
		pod.Spec.Volumes[0].VolumeSource.DownwardAPI.DefaultMode = defaultMode
	}

	return pod
}

func downwardAPIVolumePodForSimpleTest(name string, filePath string) *v1.Pod {
	pod := downwardAPIVolumeBasePod(name, nil, nil)

	pod.Spec.Containers = []v1.Container{
		{
			Name:  "client-container",
			Image: imageutils.GetE2EImage(imageutils.Agnhost),
			Args:  []string{"mounttest", "--file_content=" + filePath},
			VolumeMounts: []v1.VolumeMount{
				{
					Name:      "podinfo",
					MountPath: "/etc/podinfo",
					ReadOnly:  false,
				},
			},
		},
	}

	return pod
}

func downwardAPIVolumeForContainerResources(name string, filePath string) *v1.Pod {
	pod := downwardAPIVolumeBasePod(name, nil, nil)
	pod.Spec.Containers = downwardAPIVolumeBaseContainers("client-container", filePath)
	return pod
}

func downwardAPIVolumeForDefaultContainerResources(name string, filePath string) *v1.Pod {
	pod := downwardAPIVolumeBasePod(name, nil, nil)
	pod.Spec.Containers = downwardAPIVolumeDefaultBaseContainer("client-container", filePath)
	return pod
}

func downwardAPIVolumeBaseContainers(name, filePath string) []v1.Container {
	return []v1.Container{
		{
			Name:  name,
			Image: imageutils.GetE2EImage(imageutils.Agnhost),
			Args:  []string{"mounttest", "--file_content=" + filePath},
			Resources: v1.ResourceRequirements{
				Requests: v1.ResourceList{
					v1.ResourceCPU:    resource.MustParse("250m"),
					v1.ResourceMemory: resource.MustParse("32Mi"),
				},
				Limits: v1.ResourceList{
					v1.ResourceCPU:    resource.MustParse("1250m"),
					v1.ResourceMemory: resource.MustParse("128Mi"),
				},
			},
			VolumeMounts: []v1.VolumeMount{
				{
					Name:      "podinfo",
					MountPath: "/etc/podinfo",
					ReadOnly:  false,
				},
			},
		},
	}

}

func downwardAPIVolumeDefaultBaseContainer(name, filePath string) []v1.Container {
	return []v1.Container{
		{
			Name:  name,
			Image: imageutils.GetE2EImage(imageutils.Agnhost),
			Args:  []string{"mounttest", "--file_content=" + filePath},
			VolumeMounts: []v1.VolumeMount{
				{
					Name:      "podinfo",
					MountPath: "/etc/podinfo",
				},
			},
		},
	}

}

func downwardAPIVolumePodForUpdateTest(name string, labels, annotations map[string]string, filePath string) *v1.Pod {
	pod := downwardAPIVolumeBasePod(name, labels, annotations)

	pod.Spec.Containers = []v1.Container{
		{
			Name:  "client-container",
			Image: imageutils.GetE2EImage(imageutils.Agnhost),
			Args:  []string{"mounttest", "--break_on_expected_content=false", "--retry_time=120", "--file_content_in_loop=" + filePath},
			VolumeMounts: []v1.VolumeMount{
				{
					Name:      "podinfo",
					MountPath: "/etc/podinfo",
					ReadOnly:  false,
				},
			},
		},
	}

	applyLabelsAndAnnotationsToDownwardAPIPod(labels, annotations, pod)
	return pod
}

func downwardAPIVolumeBasePod(name string, labels, annotations map[string]string) *v1.Pod {
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: v1.PodSpec{
			Volumes: []v1.Volume{
				{
					Name: "podinfo",
					VolumeSource: v1.VolumeSource{
						DownwardAPI: &v1.DownwardAPIVolumeSource{
							Items: []v1.DownwardAPIVolumeFile{
								{
									Path: "podname",
									FieldRef: &v1.ObjectFieldSelector{
										APIVersion: "v1",
										FieldPath:  "metadata.name",
									},
								},
								{
									Path: "cpu_limit",
									ResourceFieldRef: &v1.ResourceFieldSelector{
										ContainerName: "client-container",
										Resource:      "limits.cpu",
									},
								},
								{
									Path: "cpu_request",
									ResourceFieldRef: &v1.ResourceFieldSelector{
										ContainerName: "client-container",
										Resource:      "requests.cpu",
									},
								},
								{
									Path: "memory_limit",
									ResourceFieldRef: &v1.ResourceFieldSelector{
										ContainerName: "client-container",
										Resource:      "limits.memory",
									},
								},
								{
									Path: "memory_request",
									ResourceFieldRef: &v1.ResourceFieldSelector{
										ContainerName: "client-container",
										Resource:      "requests.memory",
									},
								},
							},
						},
					},
				},
			},
			RestartPolicy: v1.RestartPolicyNever,
		},
	}

	return pod
}

func applyLabelsAndAnnotationsToDownwardAPIPod(labels, annotations map[string]string, pod *v1.Pod) {
	if len(labels) > 0 {
		pod.Spec.Volumes[0].DownwardAPI.Items = append(pod.Spec.Volumes[0].DownwardAPI.Items, v1.DownwardAPIVolumeFile{
			Path: "labels",
			FieldRef: &v1.ObjectFieldSelector{
				APIVersion: "v1",
				FieldPath:  "metadata.labels",
			},
		})
	}

	if len(annotations) > 0 {
		pod.Spec.Volumes[0].DownwardAPI.Items = append(pod.Spec.Volumes[0].DownwardAPI.Items, v1.DownwardAPIVolumeFile{
			Path: "annotations",
			FieldRef: &v1.ObjectFieldSelector{
				APIVersion: "v1",
				FieldPath:  "metadata.annotations",
			},
		})
	}
}

// TODO: add test-webserver example as pointed out in https://github.com/kubernetes/kubernetes/pull/5093#discussion-diff-37606771
