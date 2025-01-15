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
	"path"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/kubernetes/test/e2e/framework"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2epodoutput "k8s.io/kubernetes/test/e2e/framework/pod/output"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
	"k8s.io/kubernetes/test/e2e/nodefeature"
	imageutils "k8s.io/kubernetes/test/utils/image"
	admissionapi "k8s.io/pod-security-admission/api"
)

var _ = SIGDescribe("ConfigMap", func() {
	f := framework.NewDefaultFramework("configmap")
	f.NamespacePodSecurityLevel = admissionapi.LevelBaseline

	/*
		Release: v1.9
		Testname: ConfigMap Volume, without mapping
		Description: Create a ConfigMap, create a Pod that mounts a volume and populates the volume with data stored in the ConfigMap. The ConfigMap that is created MUST be accessible to read from the newly created Pod using the volume mount. The data content of the file MUST be readable and verified and file modes MUST default to 0x644.
	*/
	framework.ConformanceIt("should be consumable from pods in volume", f.WithNodeConformance(), func(ctx context.Context) {
		doConfigMapE2EWithoutMappings(ctx, f, false, 0, nil)
	})

	/*
		Release: v1.9
		Testname: ConfigMap Volume, without mapping, volume mode set
		Description: Create a ConfigMap, create a Pod that mounts a volume and populates the volume with data stored in the ConfigMap. File mode is changed to a custom value of '0x400'. The ConfigMap that is created MUST be accessible to read from the newly created Pod using the volume mount. The data content of the file MUST be readable and verified and file modes MUST be set to the custom value of '0x400'
	*/
	framework.ConformanceIt("should be consumable from pods in volume with defaultMode set", f.WithNodeConformance(), func(ctx context.Context) {
		defaultMode := int32(0400)
		doConfigMapE2EWithoutMappings(ctx, f, false, 0, &defaultMode)
	})

	f.It("should be consumable from pods in volume as non-root with defaultMode and fsGroup set [LinuxOnly]", nodefeature.FSGroup, func(ctx context.Context) {
		// Windows does not support RunAsUser / FSGroup SecurityContext options.
		e2eskipper.SkipIfNodeOSDistroIs("windows")
		defaultMode := int32(0440) /* setting fsGroup sets mode to at least 440 */
		doConfigMapE2EWithoutMappings(ctx, f, true, 1001, &defaultMode)
	})

	/*
		Release: v1.9
		Testname: ConfigMap Volume, without mapping, non-root user
		Description: Create a ConfigMap, create a Pod that mounts a volume and populates the volume with data stored in the ConfigMap. Pod is run as a non-root user with uid=1000. The ConfigMap that is created MUST be accessible to read from the newly created Pod using the volume mount. The file on the volume MUST have file mode set to default value of 0x644.
	*/
	framework.ConformanceIt("should be consumable from pods in volume as non-root", f.WithNodeConformance(), func(ctx context.Context) {
		doConfigMapE2EWithoutMappings(ctx, f, true, 0, nil)
	})

	f.It("should be consumable from pods in volume as non-root with FSGroup [LinuxOnly]", nodefeature.FSGroup, func(ctx context.Context) {
		// Windows does not support RunAsUser / FSGroup SecurityContext options.
		e2eskipper.SkipIfNodeOSDistroIs("windows")
		doConfigMapE2EWithoutMappings(ctx, f, true, 1001, nil)
	})

	/*
		Release: v1.9
		Testname: ConfigMap Volume, with mapping
		Description: Create a ConfigMap, create a Pod that mounts a volume and populates the volume with data stored in the ConfigMap. Files are mapped to a path in the volume. The ConfigMap that is created MUST be accessible to read from the newly created Pod using the volume mount. The data content of the file MUST be readable and verified and file modes MUST default to 0x644.
	*/
	framework.ConformanceIt("should be consumable from pods in volume with mappings", f.WithNodeConformance(), func(ctx context.Context) {
		doConfigMapE2EWithMappings(ctx, f, false, 0, nil)
	})

	/*
		Release: v1.9
		Testname: ConfigMap Volume, with mapping, volume mode set
		Description: Create a ConfigMap, create a Pod that mounts a volume and populates the volume with data stored in the ConfigMap. Files are mapped to a path in the volume. File mode is changed to a custom value of '0x400'. The ConfigMap that is created MUST be accessible to read from the newly created Pod using the volume mount. The data content of the file MUST be readable and verified and file modes MUST be set to the custom value of '0x400'
	*/
	framework.ConformanceIt("should be consumable from pods in volume with mappings and Item mode set", f.WithNodeConformance(), func(ctx context.Context) {
		mode := int32(0400)
		doConfigMapE2EWithMappings(ctx, f, false, 0, &mode)
	})

	/*
		Release: v1.9
		Testname: ConfigMap Volume, with mapping, non-root user
		Description: Create a ConfigMap, create a Pod that mounts a volume and populates the volume with data stored in the ConfigMap. Files are mapped to a path in the volume. Pod is run as a non-root user with uid=1000. The ConfigMap that is created MUST be accessible to read from the newly created Pod using the volume mount. The file on the volume MUST have file mode set to default value of 0x644.
	*/
	framework.ConformanceIt("should be consumable from pods in volume with mappings as non-root", f.WithNodeConformance(), func(ctx context.Context) {
		doConfigMapE2EWithMappings(ctx, f, true, 0, nil)
	})

	f.It("should be consumable from pods in volume with mappings as non-root with FSGroup [LinuxOnly]", nodefeature.FSGroup, func(ctx context.Context) {
		// Windows does not support RunAsUser / FSGroup SecurityContext options.
		e2eskipper.SkipIfNodeOSDistroIs("windows")
		doConfigMapE2EWithMappings(ctx, f, true, 1001, nil)
	})

	/*
		Release: v1.9
		Testname: ConfigMap Volume, update
		Description: The ConfigMap that is created MUST be accessible to read from the newly created Pod using the volume mount that is mapped to custom path in the Pod. When the ConfigMap is updated the change to the config map MUST be verified by reading the content from the mounted file in the Pod.
	*/
	framework.ConformanceIt("updates should be reflected in volume", f.WithNodeConformance(), func(ctx context.Context) {
		podLogTimeout := e2epod.GetPodSecretUpdateTimeout(ctx, f.ClientSet)
		containerTimeoutArg := fmt.Sprintf("--retry_time=%v", int(podLogTimeout.Seconds()))

		name := "configmap-test-upd-" + string(uuid.NewUUID())
		volumeName := "configmap-volume"
		volumeMountPath := "/etc/configmap-volume"

		configMap := &v1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: f.Namespace.Name,
				Name:      name,
			},
			Data: map[string]string{
				"data-1": "value-1",
			},
		}

		ginkgo.By(fmt.Sprintf("Creating configMap with name %s", configMap.Name))
		var err error
		if configMap, err = f.ClientSet.CoreV1().ConfigMaps(f.Namespace.Name).Create(ctx, configMap, metav1.CreateOptions{}); err != nil {
			framework.Failf("unable to create test configMap %s: %v", configMap.Name, err)
		}

		pod := createConfigMapVolumeMounttestPod(f.Namespace.Name, volumeName, name, volumeMountPath,
			"--break_on_expected_content=false", containerTimeoutArg, "--file_content_in_loop=/etc/configmap-volume/data-1")

		ginkgo.By("Creating the pod")
		e2epod.NewPodClient(f).CreateSync(ctx, pod)

		pollLogs := func() (string, error) {
			return e2epod.GetPodLogs(ctx, f.ClientSet, f.Namespace.Name, pod.Name, pod.Spec.Containers[0].Name)
		}

		gomega.Eventually(ctx, pollLogs, podLogTimeout, framework.Poll).Should(gomega.ContainSubstring("value-1"))

		ginkgo.By(fmt.Sprintf("Updating configmap %v", configMap.Name))
		configMap.ResourceVersion = "" // to force update
		configMap.Data["data-1"] = "value-2"
		_, err = f.ClientSet.CoreV1().ConfigMaps(f.Namespace.Name).Update(ctx, configMap, metav1.UpdateOptions{})
		framework.ExpectNoError(err, "Failed to update configmap %q in namespace %q", configMap.Name, f.Namespace.Name)

		ginkgo.By("waiting to observe update in volume")
		gomega.Eventually(ctx, pollLogs, podLogTimeout, framework.Poll).Should(gomega.ContainSubstring("value-2"))
	})

	/*
		Release: v1.12
		Testname: ConfigMap Volume, text data, binary data
		Description: The ConfigMap that is created with text data and binary data MUST be accessible to read from the newly created Pod using the volume mount that is mapped to custom path in the Pod. ConfigMap's text data and binary data MUST be verified by reading the content from the mounted files in the Pod.
	*/
	framework.ConformanceIt("binary data should be reflected in volume", f.WithNodeConformance(), func(ctx context.Context) {
		podLogTimeout := e2epod.GetPodSecretUpdateTimeout(ctx, f.ClientSet)
		containerTimeoutArg := fmt.Sprintf("--retry_time=%v", int(podLogTimeout.Seconds()))

		name := "configmap-test-upd-" + string(uuid.NewUUID())
		volumeName := "configmap-volume"
		volumeMountPath := "/etc/configmap-volume"
		containerName := "configmap-volume-binary-test"

		configMap := &v1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: f.Namespace.Name,
				Name:      name,
			},
			Data: map[string]string{
				"data-1": "value-1",
			},
			BinaryData: map[string][]byte{
				"dump.bin": {0xde, 0xca, 0xfe, 0xba, 0xd0, 0xfe, 0xff},
			},
		}

		ginkgo.By(fmt.Sprintf("Creating configMap with name %s", configMap.Name))
		var err error
		if configMap, err = f.ClientSet.CoreV1().ConfigMaps(f.Namespace.Name).Create(ctx, configMap, metav1.CreateOptions{}); err != nil {
			framework.Failf("unable to create test configMap %s: %v", configMap.Name, err)
		}

		pod := createConfigMapVolumeMounttestPod(f.Namespace.Name, volumeName, name, volumeMountPath,
			"--break_on_expected_content=false", containerTimeoutArg, "--file_content_in_loop=/etc/configmap-volume/data-1")
		pod.Spec.Containers = append(pod.Spec.Containers, v1.Container{
			Name:    containerName,
			Image:   imageutils.GetE2EImage(imageutils.BusyBox),
			Command: []string{"hexdump", "-C", "/etc/configmap-volume/dump.bin"},
			VolumeMounts: []v1.VolumeMount{
				{
					Name:      volumeName,
					MountPath: volumeMountPath,
					ReadOnly:  true,
				},
			},
		})

		ginkgo.By("Creating the pod")
		e2epod.NewPodClient(f).Create(ctx, pod)
		framework.ExpectNoError(e2epod.WaitForPodNameRunningInNamespace(ctx, f.ClientSet, pod.Name, f.Namespace.Name))

		pollLogs1 := func() (string, error) {
			return e2epod.GetPodLogs(ctx, f.ClientSet, f.Namespace.Name, pod.Name, pod.Spec.Containers[0].Name)
		}
		pollLogs2 := func() (string, error) {
			return e2epod.GetPodLogs(ctx, f.ClientSet, f.Namespace.Name, pod.Name, pod.Spec.Containers[1].Name)
		}

		ginkgo.By("Waiting for pod with text data")
		gomega.Eventually(ctx, pollLogs1, podLogTimeout, framework.Poll).Should(gomega.ContainSubstring("value-1"))
		ginkgo.By("Waiting for pod with binary data")
		gomega.Eventually(ctx, pollLogs2, podLogTimeout, framework.Poll).Should(gomega.ContainSubstring("de ca fe ba d0 fe ff"))
	})

	/*
		Release: v1.9
		Testname: ConfigMap Volume, create, update and delete
		Description: The ConfigMap that is created MUST be accessible to read from the newly created Pod using the volume mount that is mapped to custom path in the Pod. When the config map is updated the change to the config map MUST be verified by reading the content from the mounted file in the Pod. Also when the item(file) is deleted from the map that MUST result in a error reading that item(file).
	*/
	framework.ConformanceIt("optional updates should be reflected in volume", f.WithNodeConformance(), func(ctx context.Context) {
		podLogTimeout := e2epod.GetPodSecretUpdateTimeout(ctx, f.ClientSet)
		containerTimeoutArg := fmt.Sprintf("--retry_time=%v", int(podLogTimeout.Seconds()))
		trueVal := true
		volumeMountPath := "/etc/configmap-volumes"

		deleteName := "cm-test-opt-del-" + string(uuid.NewUUID())
		deleteContainerName := "delcm-volume-test"
		deleteVolumeName := "deletecm-volume"
		deleteConfigMap := &v1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: f.Namespace.Name,
				Name:      deleteName,
			},
			Data: map[string]string{
				"data-1": "value-1",
			},
		}

		updateName := "cm-test-opt-upd-" + string(uuid.NewUUID())
		updateContainerName := "updcm-volume-test"
		updateVolumeName := "updatecm-volume"
		updateConfigMap := &v1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: f.Namespace.Name,
				Name:      updateName,
			},
			Data: map[string]string{
				"data-1": "value-1",
			},
		}

		createName := "cm-test-opt-create-" + string(uuid.NewUUID())
		createContainerName := "createcm-volume-test"
		createVolumeName := "createcm-volume"
		createConfigMap := &v1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: f.Namespace.Name,
				Name:      createName,
			},
			Data: map[string]string{
				"data-1": "value-1",
			},
		}

		ginkgo.By(fmt.Sprintf("Creating configMap with name %s", deleteConfigMap.Name))
		var err error
		if deleteConfigMap, err = f.ClientSet.CoreV1().ConfigMaps(f.Namespace.Name).Create(ctx, deleteConfigMap, metav1.CreateOptions{}); err != nil {
			framework.Failf("unable to create test configMap %s: %v", deleteConfigMap.Name, err)
		}

		ginkgo.By(fmt.Sprintf("Creating configMap with name %s", updateConfigMap.Name))
		if updateConfigMap, err = f.ClientSet.CoreV1().ConfigMaps(f.Namespace.Name).Create(ctx, updateConfigMap, metav1.CreateOptions{}); err != nil {
			framework.Failf("unable to create test configMap %s: %v", updateConfigMap.Name, err)
		}

		pod := &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: "pod-configmaps-" + string(uuid.NewUUID()),
			},
			Spec: v1.PodSpec{
				Volumes: []v1.Volume{
					{
						Name: deleteVolumeName,
						VolumeSource: v1.VolumeSource{
							ConfigMap: &v1.ConfigMapVolumeSource{
								LocalObjectReference: v1.LocalObjectReference{
									Name: deleteName,
								},
								Optional: &trueVal,
							},
						},
					},
					{
						Name: updateVolumeName,
						VolumeSource: v1.VolumeSource{
							ConfigMap: &v1.ConfigMapVolumeSource{
								LocalObjectReference: v1.LocalObjectReference{
									Name: updateName,
								},
								Optional: &trueVal,
							},
						},
					},
					{
						Name: createVolumeName,
						VolumeSource: v1.VolumeSource{
							ConfigMap: &v1.ConfigMapVolumeSource{
								LocalObjectReference: v1.LocalObjectReference{
									Name: createName,
								},
								Optional: &trueVal,
							},
						},
					},
				},
				Containers: []v1.Container{
					{
						Name:  deleteContainerName,
						Image: imageutils.GetE2EImage(imageutils.Agnhost),
						Args:  []string{"mounttest", "--break_on_expected_content=false", containerTimeoutArg, "--file_content_in_loop=/etc/configmap-volumes/delete/data-1"},
						VolumeMounts: []v1.VolumeMount{
							{
								Name:      deleteVolumeName,
								MountPath: path.Join(volumeMountPath, "delete"),
								ReadOnly:  true,
							},
						},
					},
					{
						Name:  updateContainerName,
						Image: imageutils.GetE2EImage(imageutils.Agnhost),
						Args:  []string{"mounttest", "--break_on_expected_content=false", containerTimeoutArg, "--file_content_in_loop=/etc/configmap-volumes/update/data-3"},
						VolumeMounts: []v1.VolumeMount{
							{
								Name:      updateVolumeName,
								MountPath: path.Join(volumeMountPath, "update"),
								ReadOnly:  true,
							},
						},
					},
					{
						Name:  createContainerName,
						Image: imageutils.GetE2EImage(imageutils.Agnhost),
						Args:  []string{"mounttest", "--break_on_expected_content=false", containerTimeoutArg, "--file_content_in_loop=/etc/configmap-volumes/create/data-1"},
						VolumeMounts: []v1.VolumeMount{
							{
								Name:      createVolumeName,
								MountPath: path.Join(volumeMountPath, "create"),
								ReadOnly:  true,
							},
						},
					},
				},
				RestartPolicy: v1.RestartPolicyNever,
			},
		}
		ginkgo.By("Creating the pod")
		e2epod.NewPodClient(f).CreateSync(ctx, pod)

		pollCreateLogs := func() (string, error) {
			return e2epod.GetPodLogs(ctx, f.ClientSet, f.Namespace.Name, pod.Name, createContainerName)
		}
		gomega.Eventually(ctx, pollCreateLogs, podLogTimeout, framework.Poll).Should(gomega.ContainSubstring("Error reading file /etc/configmap-volumes/create/data-1"))

		pollUpdateLogs := func() (string, error) {
			return e2epod.GetPodLogs(ctx, f.ClientSet, f.Namespace.Name, pod.Name, updateContainerName)
		}
		gomega.Eventually(ctx, pollUpdateLogs, podLogTimeout, framework.Poll).Should(gomega.ContainSubstring("Error reading file /etc/configmap-volumes/update/data-3"))

		pollDeleteLogs := func() (string, error) {
			return e2epod.GetPodLogs(ctx, f.ClientSet, f.Namespace.Name, pod.Name, deleteContainerName)
		}
		gomega.Eventually(ctx, pollDeleteLogs, podLogTimeout, framework.Poll).Should(gomega.ContainSubstring("value-1"))

		ginkgo.By(fmt.Sprintf("Deleting configmap %v", deleteConfigMap.Name))
		err = f.ClientSet.CoreV1().ConfigMaps(f.Namespace.Name).Delete(ctx, deleteConfigMap.Name, metav1.DeleteOptions{})
		framework.ExpectNoError(err, "Failed to delete configmap %q in namespace %q", deleteConfigMap.Name, f.Namespace.Name)

		ginkgo.By(fmt.Sprintf("Updating configmap %v", updateConfigMap.Name))
		updateConfigMap.ResourceVersion = "" // to force update
		delete(updateConfigMap.Data, "data-1")
		updateConfigMap.Data["data-3"] = "value-3"
		_, err = f.ClientSet.CoreV1().ConfigMaps(f.Namespace.Name).Update(ctx, updateConfigMap, metav1.UpdateOptions{})
		framework.ExpectNoError(err, "Failed to update configmap %q in namespace %q", updateConfigMap.Name, f.Namespace.Name)

		ginkgo.By(fmt.Sprintf("Creating configMap with name %s", createConfigMap.Name))
		if createConfigMap, err = f.ClientSet.CoreV1().ConfigMaps(f.Namespace.Name).Create(ctx, createConfigMap, metav1.CreateOptions{}); err != nil {
			framework.Failf("unable to create test configMap %s: %v", createConfigMap.Name, err)
		}

		ginkgo.By("waiting to observe update in volume")

		gomega.Eventually(ctx, pollCreateLogs, podLogTimeout, framework.Poll).Should(gomega.ContainSubstring("value-1"))
		gomega.Eventually(ctx, pollUpdateLogs, podLogTimeout, framework.Poll).Should(gomega.ContainSubstring("value-3"))
		gomega.Eventually(ctx, pollDeleteLogs, podLogTimeout, framework.Poll).Should(gomega.ContainSubstring("Error reading file /etc/configmap-volumes/delete/data-1"))
	})

	/*
		Release: v1.9
		Testname: ConfigMap Volume, multiple volume maps
		Description: The ConfigMap that is created MUST be accessible to read from the newly created Pod using the volume mount that is mapped to multiple paths in the Pod. The content MUST be accessible from all the mapped volume mounts.
	*/
	framework.ConformanceIt("should be consumable in multiple volumes in the same pod", f.WithNodeConformance(), func(ctx context.Context) {
		var (
			name             = "configmap-test-volume-" + string(uuid.NewUUID())
			volumeName       = "configmap-volume"
			volumeMountPath  = "/etc/configmap-volume"
			volumeName2      = "configmap-volume-2"
			volumeMountPath2 = "/etc/configmap-volume-2"
			configMap        = newConfigMap(f, name)
		)

		ginkgo.By(fmt.Sprintf("Creating configMap with name %s", configMap.Name))
		var err error
		if configMap, err = f.ClientSet.CoreV1().ConfigMaps(f.Namespace.Name).Create(ctx, configMap, metav1.CreateOptions{}); err != nil {
			framework.Failf("unable to create test configMap %s: %v", configMap.Name, err)
		}

		pod := &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: "pod-configmaps-" + string(uuid.NewUUID()),
			},
			Spec: v1.PodSpec{
				Volumes: []v1.Volume{
					{
						Name: volumeName,
						VolumeSource: v1.VolumeSource{
							ConfigMap: &v1.ConfigMapVolumeSource{
								LocalObjectReference: v1.LocalObjectReference{
									Name: name,
								},
							},
						},
					},
					{
						Name: volumeName2,
						VolumeSource: v1.VolumeSource{
							ConfigMap: &v1.ConfigMapVolumeSource{
								LocalObjectReference: v1.LocalObjectReference{
									Name: name,
								},
							},
						},
					},
				},
				Containers: []v1.Container{
					{
						Name:  "configmap-volume-test",
						Image: imageutils.GetE2EImage(imageutils.Agnhost),
						Args:  []string{"mounttest", "--file_content=/etc/configmap-volume/data-1"},
						VolumeMounts: []v1.VolumeMount{
							{
								Name:      volumeName,
								MountPath: volumeMountPath,
								ReadOnly:  true,
							},
							{
								Name:      volumeName2,
								MountPath: volumeMountPath2,
								ReadOnly:  true,
							},
						},
					},
				},
				RestartPolicy: v1.RestartPolicyNever,
			},
		}

		e2epodoutput.TestContainerOutput(ctx, f, "consume configMaps", pod, 0, []string{
			"content of file \"/etc/configmap-volume/data-1\": value-1",
		})

	})

	/*
		Release: v1.21
		Testname: ConfigMap Volume, immutability
		Description: Create a ConfigMap. Update it's data field, the update MUST succeed.
			Mark the ConfigMap as immutable, the update MUST succeed. Try to update its data, the update MUST fail.
			Try to mark the ConfigMap back as not immutable, the update MUST fail.
			Try to update the ConfigMap`s metadata (labels), the update must succeed.
			Try to delete the ConfigMap, the deletion must succeed.
	*/
	framework.ConformanceIt("should be immutable if `immutable` field is set", func(ctx context.Context) {
		name := "immutable"
		configMap := newConfigMap(f, name)

		currentConfigMap, err := f.ClientSet.CoreV1().ConfigMaps(f.Namespace.Name).Create(ctx, configMap, metav1.CreateOptions{})
		framework.ExpectNoError(err, "Failed to create config map %q in namespace %q", configMap.Name, configMap.Namespace)

		currentConfigMap.Data["data-4"] = "value-4"
		currentConfigMap, err = f.ClientSet.CoreV1().ConfigMaps(f.Namespace.Name).Update(ctx, currentConfigMap, metav1.UpdateOptions{})
		framework.ExpectNoError(err, "Failed to update config map %q in namespace %q", configMap.Name, configMap.Namespace)

		// Mark config map as immutable.
		trueVal := true
		currentConfigMap.Immutable = &trueVal
		currentConfigMap, err = f.ClientSet.CoreV1().ConfigMaps(f.Namespace.Name).Update(ctx, currentConfigMap, metav1.UpdateOptions{})
		framework.ExpectNoError(err, "Failed to mark config map %q in namespace %q as immutable", configMap.Name, configMap.Namespace)

		// Ensure data can't be changed now.
		currentConfigMap.Data["data-5"] = "value-5"
		_, err = f.ClientSet.CoreV1().ConfigMaps(f.Namespace.Name).Update(ctx, currentConfigMap, metav1.UpdateOptions{})
		if !apierrors.IsInvalid(err) {
			framework.Failf("expected 'invalid' as error, got instead: %v", err)
		}

		// Ensure config map can't be switched from immutable to mutable.
		currentConfigMap, err = f.ClientSet.CoreV1().ConfigMaps(f.Namespace.Name).Get(ctx, name, metav1.GetOptions{})
		framework.ExpectNoError(err, "Failed to get config map %q in namespace %q", configMap.Name, configMap.Namespace)
		if !*currentConfigMap.Immutable {
			framework.Failf("currentConfigMap %s can be switched from immutable to mutable", currentConfigMap.Name)
		}

		falseVal := false
		currentConfigMap.Immutable = &falseVal
		_, err = f.ClientSet.CoreV1().ConfigMaps(f.Namespace.Name).Update(ctx, currentConfigMap, metav1.UpdateOptions{})
		if !apierrors.IsInvalid(err) {
			framework.Failf("expected 'invalid' as error, got instead: %v", err)
		}

		// Ensure that metadata can be changed.
		currentConfigMap, err = f.ClientSet.CoreV1().ConfigMaps(f.Namespace.Name).Get(ctx, name, metav1.GetOptions{})
		framework.ExpectNoError(err, "Failed to get config map %q in namespace %q", configMap.Name, configMap.Namespace)
		currentConfigMap.Labels = map[string]string{"label1": "value1"}
		_, err = f.ClientSet.CoreV1().ConfigMaps(f.Namespace.Name).Update(ctx, currentConfigMap, metav1.UpdateOptions{})
		framework.ExpectNoError(err, "Failed to update config map %q in namespace %q", configMap.Name, configMap.Namespace)

		// Ensure that immutable config map can be deleted.
		err = f.ClientSet.CoreV1().ConfigMaps(f.Namespace.Name).Delete(ctx, name, metav1.DeleteOptions{})
		framework.ExpectNoError(err, "Failed to delete config map %q in namespace %q", configMap.Name, configMap.Namespace)
	})

	// The pod is in pending during volume creation until the configMap objects are available
	// or until mount the configMap volume times out. There is no configMap object defined for the pod, so it should return timeout exception unless it is marked optional.
	// Slow (~5 mins)
	f.It("Should fail non-optional pod creation due to configMap object does not exist", f.WithSlow(), func(ctx context.Context) {
		volumeMountPath := "/etc/configmap-volumes"
		pod := createNonOptionalConfigMapPod(ctx, f, volumeMountPath)
		getPod := e2epod.Get(f.ClientSet, pod)
		gomega.Consistently(ctx, getPod).WithTimeout(f.Timeouts.PodStart).Should(e2epod.BeInPhase(v1.PodPending))
	})

	// ConfigMap object defined for the pod, If a key is specified which is not present in the ConfigMap,
	// the volume setup will error unless it is marked optional, during the pod creation.
	// Slow (~5 mins)
	f.It("Should fail non-optional pod creation due to the key in the configMap object does not exist", f.WithSlow(), func(ctx context.Context) {
		volumeMountPath := "/etc/configmap-volumes"
		pod := createNonOptionalConfigMapPodWithConfig(ctx, f, volumeMountPath)
		getPod := e2epod.Get(f.ClientSet, pod)
		gomega.Consistently(ctx, getPod).WithTimeout(f.Timeouts.PodStart).Should(e2epod.BeInPhase(v1.PodPending))
	})
})

func newConfigMap(f *framework.Framework, name string) *v1.ConfigMap {
	return &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: f.Namespace.Name,
			Name:      name,
		},
		Data: map[string]string{
			"data-1": "value-1",
			"data-2": "value-2",
			"data-3": "value-3",
		},
	}
}

func doConfigMapE2EWithoutMappings(ctx context.Context, f *framework.Framework, asUser bool, fsGroup int64, defaultMode *int32) {
	groupID := int64(fsGroup)

	var (
		name            = "configmap-test-volume-" + string(uuid.NewUUID())
		volumeName      = "configmap-volume"
		volumeMountPath = "/etc/configmap-volume"
		configMap       = newConfigMap(f, name)
	)

	ginkgo.By(fmt.Sprintf("Creating configMap with name %s", configMap.Name))
	var err error
	if configMap, err = f.ClientSet.CoreV1().ConfigMaps(f.Namespace.Name).Create(ctx, configMap, metav1.CreateOptions{}); err != nil {
		framework.Failf("unable to create test configMap %s: %v", configMap.Name, err)
	}

	pod := createConfigMapVolumeMounttestPod(f.Namespace.Name, volumeName, name, volumeMountPath,
		"--file_content=/etc/configmap-volume/data-1", "--file_mode=/etc/configmap-volume/data-1")
	one := int64(1)
	pod.Spec.TerminationGracePeriodSeconds = &one

	if asUser {
		setPodNonRootUser(pod)
	}

	if groupID != 0 {
		pod.Spec.SecurityContext.FSGroup = &groupID
	}

	if defaultMode != nil {
		pod.Spec.Volumes[0].VolumeSource.ConfigMap.DefaultMode = defaultMode
	}

	fileModeRegexp := getFileModeRegex("/etc/configmap-volume/data-1", defaultMode)
	output := []string{
		"content of file \"/etc/configmap-volume/data-1\": value-1",
		fileModeRegexp,
	}
	e2epodoutput.TestContainerOutputRegexp(ctx, f, "consume configMaps", pod, 0, output)
}

func doConfigMapE2EWithMappings(ctx context.Context, f *framework.Framework, asUser bool, fsGroup int64, itemMode *int32) {
	groupID := int64(fsGroup)

	var (
		name            = "configmap-test-volume-map-" + string(uuid.NewUUID())
		volumeName      = "configmap-volume"
		volumeMountPath = "/etc/configmap-volume"
		configMap       = newConfigMap(f, name)
	)

	ginkgo.By(fmt.Sprintf("Creating configMap with name %s", configMap.Name))

	var err error
	if configMap, err = f.ClientSet.CoreV1().ConfigMaps(f.Namespace.Name).Create(ctx, configMap, metav1.CreateOptions{}); err != nil {
		framework.Failf("unable to create test configMap %s: %v", configMap.Name, err)
	}

	pod := createConfigMapVolumeMounttestPod(f.Namespace.Name, volumeName, name, volumeMountPath,
		"--file_content=/etc/configmap-volume/path/to/data-2", "--file_mode=/etc/configmap-volume/path/to/data-2")
	one := int64(1)
	pod.Spec.TerminationGracePeriodSeconds = &one
	pod.Spec.Volumes[0].VolumeSource.ConfigMap.Items = []v1.KeyToPath{
		{
			Key:  "data-2",
			Path: "path/to/data-2",
		},
	}

	if asUser {
		setPodNonRootUser(pod)
	}

	if groupID != 0 {
		pod.Spec.SecurityContext.FSGroup = &groupID
	}

	if itemMode != nil {
		pod.Spec.Volumes[0].VolumeSource.ConfigMap.Items[0].Mode = itemMode
	}

	// Just check file mode if fsGroup is not set. If fsGroup is set, the
	// final mode is adjusted and we are not testing that case.
	output := []string{
		"content of file \"/etc/configmap-volume/path/to/data-2\": value-2",
	}
	if fsGroup == 0 {
		fileModeRegexp := getFileModeRegex("/etc/configmap-volume/path/to/data-2", itemMode)
		output = append(output, fileModeRegexp)
	}
	e2epodoutput.TestContainerOutputRegexp(ctx, f, "consume configMaps", pod, 0, output)
}

func createNonOptionalConfigMapPod(ctx context.Context, f *framework.Framework, volumeMountPath string) *v1.Pod {
	podLogTimeout := e2epod.GetPodSecretUpdateTimeout(ctx, f.ClientSet)
	containerTimeoutArg := fmt.Sprintf("--retry_time=%v", int(podLogTimeout.Seconds()))
	falseValue := false

	createName := "cm-test-opt-create-" + string(uuid.NewUUID())
	createVolumeName := "createcm-volume"

	// creating a pod without configMap object created, by mentioning the configMap volume source's local reference name
	pod := createConfigMapVolumeMounttestPod(f.Namespace.Name, createVolumeName, createName, path.Join(volumeMountPath, "create"),
		"--break_on_expected_content=false", containerTimeoutArg, "--file_content_in_loop=/etc/configmap-volumes/create/data-1")
	pod.Spec.Volumes[0].VolumeSource.ConfigMap.Optional = &falseValue

	ginkgo.By("Creating the pod")
	pod = e2epod.NewPodClient(f).Create(ctx, pod)
	return pod
}

func createNonOptionalConfigMapPodWithConfig(ctx context.Context, f *framework.Framework, volumeMountPath string) *v1.Pod {
	podLogTimeout := e2epod.GetPodSecretUpdateTimeout(ctx, f.ClientSet)
	containerTimeoutArg := fmt.Sprintf("--retry_time=%v", int(podLogTimeout.Seconds()))
	falseValue := false

	createName := "cm-test-opt-create-" + string(uuid.NewUUID())
	createVolumeName := "createcm-volume"
	configMap := newConfigMap(f, createName)

	ginkgo.By(fmt.Sprintf("Creating configMap with name %s", configMap.Name))
	var err error
	if configMap, err = f.ClientSet.CoreV1().ConfigMaps(f.Namespace.Name).Create(ctx, configMap, metav1.CreateOptions{}); err != nil {
		framework.Failf("unable to create test configMap %s: %v", configMap.Name, err)
	}
	// creating a pod with configMap object, but with different key which is not present in configMap object.
	pod := createConfigMapVolumeMounttestPod(f.Namespace.Name, createVolumeName, createName, path.Join(volumeMountPath, "create"),
		"--break_on_expected_content=false", containerTimeoutArg, "--file_content_in_loop=/etc/configmap-volumes/create/data-1")
	pod.Spec.Volumes[0].VolumeSource.ConfigMap.Optional = &falseValue
	pod.Spec.Volumes[0].VolumeSource.ConfigMap.Items = []v1.KeyToPath{
		{
			Key:  "data-4",
			Path: "path/to/data-4",
		},
	}

	ginkgo.By("Creating the pod")
	pod = e2epod.NewPodClient(f).Create(ctx, pod)
	return pod
}

func createConfigMapVolumeMounttestPod(namespace, volumeName, referenceName, mountPath string, mounttestArgs ...string) *v1.Pod {
	volumes := []v1.Volume{
		{
			Name: volumeName,
			VolumeSource: v1.VolumeSource{
				ConfigMap: &v1.ConfigMapVolumeSource{
					LocalObjectReference: v1.LocalObjectReference{
						Name: referenceName,
					},
				},
			},
		},
	}
	podName := "pod-configmaps-" + string(uuid.NewUUID())
	mounttestArgs = append([]string{"mounttest"}, mounttestArgs...)
	pod := e2epod.NewAgnhostPod(namespace, podName, volumes, createMounts(volumeName, mountPath, true), nil, mounttestArgs...)
	pod.Spec.RestartPolicy = v1.RestartPolicyNever
	return pod
}
