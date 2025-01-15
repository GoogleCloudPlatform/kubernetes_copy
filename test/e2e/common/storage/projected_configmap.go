/*
Copyright 2014 The Kubernetes Authors.

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

	v1 "k8s.io/api/core/v1"
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

var _ = SIGDescribe("Projected configMap", func() {
	f := framework.NewDefaultFramework("projected")
	f.NamespacePodSecurityLevel = admissionapi.LevelBaseline

	/*
	   Release: v1.9
	   Testname: Projected Volume, ConfigMap, volume mode default
	   Description: A Pod is created with projected volume source 'ConfigMap' to store a configMap with default permission mode. Pod MUST be able to read the content of the ConfigMap successfully and the mode on the volume MUST be -rw-r--r--.
	*/
	framework.ConformanceIt("should be consumable from pods in volume", f.WithNodeConformance(), func(ctx context.Context) {
		doProjectedConfigMapE2EWithoutMappings(ctx, f, false, 0, nil)
	})

	/*
	   Release: v1.9
	   Testname: Projected Volume, ConfigMap, volume mode 0400
	   Description: A Pod is created with projected volume source 'ConfigMap' to store a configMap with permission mode set to 0400. Pod MUST be able to read the content of the ConfigMap successfully and the mode on the volume MUST be -r--------.
	*/
	framework.ConformanceIt("should be consumable from pods in volume with defaultMode set", f.WithNodeConformance(), func(ctx context.Context) {
		defaultMode := int32(0400)
		doProjectedConfigMapE2EWithoutMappings(ctx, f, false, 0, &defaultMode)
	})

	f.It("should be consumable from pods in volume as non-root with defaultMode and fsGroup set [LinuxOnly]", nodefeature.FSGroup, func(ctx context.Context) {
		// Windows does not support RunAsUser / FSGroup SecurityContext options.
		e2eskipper.SkipIfNodeOSDistroIs("windows")
		defaultMode := int32(0440) /* setting fsGroup sets mode to at least 440 */
		doProjectedConfigMapE2EWithoutMappings(ctx, f, true, 1001, &defaultMode)
	})

	/*
	   Release: v1.9
	   Testname: Projected Volume, ConfigMap, non-root user
	   Description: A Pod is created with projected volume source 'ConfigMap' to store a configMap as non-root user with uid 1000. Pod MUST be able to read the content of the ConfigMap successfully and the mode on the volume MUST be -rw-r--r--.
	*/
	framework.ConformanceIt("should be consumable from pods in volume as non-root", f.WithNodeConformance(), func(ctx context.Context) {
		doProjectedConfigMapE2EWithoutMappings(ctx, f, true, 0, nil)
	})

	f.It("should be consumable from pods in volume as non-root with FSGroup [LinuxOnly]", nodefeature.FSGroup, func(ctx context.Context) {
		// Windows does not support RunAsUser / FSGroup SecurityContext options.
		e2eskipper.SkipIfNodeOSDistroIs("windows")
		doProjectedConfigMapE2EWithoutMappings(ctx, f, true, 1001, nil)
	})

	/*
	   Release: v1.9
	   Testname: Projected Volume, ConfigMap, mapped
	   Description: A Pod is created with projected volume source 'ConfigMap' to store a configMap with default permission mode. The ConfigMap is also mapped to a custom path. Pod MUST be able to read the content of the ConfigMap from the custom location successfully and the mode on the volume MUST be -rw-r--r--.
	*/
	framework.ConformanceIt("should be consumable from pods in volume with mappings", f.WithNodeConformance(), func(ctx context.Context) {
		doProjectedConfigMapE2EWithMappings(ctx, f, false, 0, nil)
	})

	/*
	   Release: v1.9
	   Testname: Projected Volume, ConfigMap, mapped, volume mode 0400
	   Description: A Pod is created with projected volume source 'ConfigMap' to store a configMap with permission mode set to 0400. The ConfigMap is also mapped to a custom path. Pod MUST be able to read the content of the ConfigMap from the custom location successfully and the mode on the volume MUST be -r--r--r--.
	*/
	framework.ConformanceIt("should be consumable from pods in volume with mappings and Item mode set", f.WithNodeConformance(), func(ctx context.Context) {
		mode := int32(0400)
		doProjectedConfigMapE2EWithMappings(ctx, f, false, 0, &mode)
	})

	/*
	   Release: v1.9
	   Testname: Projected Volume, ConfigMap, mapped, non-root user
	   Description: A Pod is created with projected volume source 'ConfigMap' to store a configMap as non-root user with uid 1000. The ConfigMap is also mapped to a custom path. Pod MUST be able to read the content of the ConfigMap from the custom location successfully and the mode on the volume MUST be -r--r--r--.
	*/
	framework.ConformanceIt("should be consumable from pods in volume with mappings as non-root", f.WithNodeConformance(), func(ctx context.Context) {
		doProjectedConfigMapE2EWithMappings(ctx, f, true, 0, nil)
	})

	f.It("should be consumable from pods in volume with mappings as non-root with FSGroup [LinuxOnly]", nodefeature.FSGroup, func(ctx context.Context) {
		// Windows does not support RunAsUser / FSGroup SecurityContext options.
		e2eskipper.SkipIfNodeOSDistroIs("windows")
		doProjectedConfigMapE2EWithMappings(ctx, f, true, 1001, nil)
	})

	/*
	   Release: v1.9
	   Testname: Projected Volume, ConfigMap, update
	   Description: A Pod is created with projected volume source 'ConfigMap' to store a configMap and performs a create and update to new value. Pod MUST be able to create the configMap with value-1. Pod MUST be able to update the value in the confgiMap to value-2.
	*/
	framework.ConformanceIt("updates should be reflected in volume", f.WithNodeConformance(), func(ctx context.Context) {
		podLogTimeout := e2epod.GetPodSecretUpdateTimeout(ctx, f.ClientSet)
		containerTimeoutArg := fmt.Sprintf("--retry_time=%v", int(podLogTimeout.Seconds()))

		name := "projected-configmap-test-upd-" + string(uuid.NewUUID())
		volumeName := "projected-configmap-volume"
		volumeMountPath := "/etc/projected-configmap-volume"
		configMap := &v1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: f.Namespace.Name,
				Name:      name,
			},
			Data: map[string]string{
				"data-1": "value-1",
			},
		}

		ginkgo.By(fmt.Sprintf("Creating projection with configMap that has name %s", configMap.Name))
		var err error
		if configMap, err = f.ClientSet.CoreV1().ConfigMaps(f.Namespace.Name).Create(ctx, configMap, metav1.CreateOptions{}); err != nil {
			framework.Failf("unable to create test configMap %s: %v", configMap.Name, err)
		}

		pod := createProjectedConfigMapMounttestPod(f.Namespace.Name, volumeName, name, volumeMountPath,
			"--break_on_expected_content=false", containerTimeoutArg, "--file_content_in_loop=/etc/projected-configmap-volume/data-1")

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
	   Release: v1.9
	   Testname: Projected Volume, ConfigMap, create, update and delete
	   Description: Create a Pod with three containers with ConfigMaps namely a create, update and delete container. Create Container when started MUST not have configMap, update and delete containers MUST be created with a ConfigMap value as 'value-1'. Create a configMap in the create container, the Pod MUST be able to read the configMap from the create container. Update the configMap in the update container, Pod MUST be able to read the updated configMap value. Delete the configMap in the delete container. Pod MUST fail to read the configMap from the delete container.
	*/
	framework.ConformanceIt("optional updates should be reflected in volume", f.WithNodeConformance(), func(ctx context.Context) {
		podLogTimeout := e2epod.GetPodSecretUpdateTimeout(ctx, f.ClientSet)
		containerTimeoutArg := fmt.Sprintf("--retry_time=%v", int(podLogTimeout.Seconds()))
		trueVal := true
		volumeMountPath := "/etc/projected-configmap-volumes"

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
				Name: "pod-projected-configmaps-" + string(uuid.NewUUID()),
			},
			Spec: v1.PodSpec{
				Volumes: []v1.Volume{
					{
						Name: deleteVolumeName,
						VolumeSource: v1.VolumeSource{
							Projected: &v1.ProjectedVolumeSource{
								Sources: []v1.VolumeProjection{
									{
										ConfigMap: &v1.ConfigMapProjection{
											LocalObjectReference: v1.LocalObjectReference{
												Name: deleteName,
											},
											Optional: &trueVal,
										},
									},
								},
							},
						},
					},
					{
						Name: updateVolumeName,
						VolumeSource: v1.VolumeSource{
							Projected: &v1.ProjectedVolumeSource{
								Sources: []v1.VolumeProjection{
									{
										ConfigMap: &v1.ConfigMapProjection{
											LocalObjectReference: v1.LocalObjectReference{
												Name: updateName,
											},
											Optional: &trueVal,
										},
									},
								},
							},
						},
					},
					{
						Name: createVolumeName,
						VolumeSource: v1.VolumeSource{
							Projected: &v1.ProjectedVolumeSource{
								Sources: []v1.VolumeProjection{
									{
										ConfigMap: &v1.ConfigMapProjection{
											LocalObjectReference: v1.LocalObjectReference{
												Name: createName,
											},
											Optional: &trueVal,
										},
									},
								},
							},
						},
					},
				},
				Containers: []v1.Container{
					{
						Name:  deleteContainerName,
						Image: imageutils.GetE2EImage(imageutils.Agnhost),
						Args:  []string{"mounttest", "--break_on_expected_content=false", containerTimeoutArg, "--file_content_in_loop=/etc/projected-configmap-volumes/delete/data-1"},
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
						Args:  []string{"mounttest", "--break_on_expected_content=false", containerTimeoutArg, "--file_content_in_loop=/etc/projected-configmap-volumes/update/data-3"},
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
						Args:  []string{"mounttest", "--break_on_expected_content=false", containerTimeoutArg, "--file_content_in_loop=/etc/projected-configmap-volumes/create/data-1"},
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
		gomega.Eventually(ctx, pollCreateLogs, podLogTimeout, framework.Poll).Should(gomega.ContainSubstring("Error reading file /etc/projected-configmap-volumes/create/data-1"))

		pollUpdateLogs := func() (string, error) {
			return e2epod.GetPodLogs(ctx, f.ClientSet, f.Namespace.Name, pod.Name, updateContainerName)
		}
		gomega.Eventually(ctx, pollUpdateLogs, podLogTimeout, framework.Poll).Should(gomega.ContainSubstring("Error reading file /etc/projected-configmap-volumes/update/data-3"))

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
		gomega.Eventually(ctx, pollDeleteLogs, podLogTimeout, framework.Poll).Should(gomega.ContainSubstring("Error reading file /etc/projected-configmap-volumes/delete/data-1"))
	})

	/*
	   Release: v1.9
	   Testname: Projected Volume, ConfigMap, multiple volume paths
	   Description: A Pod is created with a projected volume source 'ConfigMap' to store a configMap. The configMap is mapped to two different volume mounts. Pod MUST be able to read the content of the configMap successfully from the two volume mounts.
	*/
	framework.ConformanceIt("should be consumable in multiple volumes in the same pod", f.WithNodeConformance(), func(ctx context.Context) {
		var (
			name             = "projected-configmap-test-volume-" + string(uuid.NewUUID())
			volumeName       = "projected-configmap-volume"
			volumeMountPath  = "/etc/projected-configmap-volume"
			volumeName2      = "projected-configmap-volume-2"
			volumeMountPath2 = "/etc/projected-configmap-volume-2"
			configMap        = newConfigMap(f, name)
		)

		ginkgo.By(fmt.Sprintf("Creating configMap with name %s", configMap.Name))
		var err error
		if configMap, err = f.ClientSet.CoreV1().ConfigMaps(f.Namespace.Name).Create(ctx, configMap, metav1.CreateOptions{}); err != nil {
			framework.Failf("unable to create test configMap %s: %v", configMap.Name, err)
		}

		pod := &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: "pod-projected-configmaps-" + string(uuid.NewUUID()),
			},
			Spec: v1.PodSpec{
				Volumes: []v1.Volume{
					{
						Name: volumeName,
						VolumeSource: v1.VolumeSource{
							Projected: &v1.ProjectedVolumeSource{
								Sources: []v1.VolumeProjection{
									{
										ConfigMap: &v1.ConfigMapProjection{
											LocalObjectReference: v1.LocalObjectReference{
												Name: name,
											},
										},
									},
								},
							},
						},
					},
					{
						Name: volumeName2,
						VolumeSource: v1.VolumeSource{
							Projected: &v1.ProjectedVolumeSource{
								Sources: []v1.VolumeProjection{
									{

										ConfigMap: &v1.ConfigMapProjection{
											LocalObjectReference: v1.LocalObjectReference{
												Name: name,
											},
										},
									},
								},
							},
						},
					},
				},
				Containers: []v1.Container{
					{
						Name:  "projected-configmap-volume-test",
						Image: imageutils.GetE2EImage(imageutils.Agnhost),
						Args:  []string{"mounttest", "--file_content=/etc/projected-configmap-volume/data-1"},
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
			"content of file \"/etc/projected-configmap-volume/data-1\": value-1",
		})

	})

	//The pod is in pending during volume creation until the configMap objects are available
	//or until mount the configMap volume times out. There is no configMap object defined for the pod, so it should return timeout exception unless it is marked optional.
	//Slow (~5 mins)
	f.It("Should fail non-optional pod creation due to configMap object does not exist", f.WithSlow(), func(ctx context.Context) {
		volumeMountPath := "/etc/projected-configmap-volumes"
		pod := createNonOptionalConfigMapPod(ctx, f, volumeMountPath)
		getPod := e2epod.Get(f.ClientSet, pod)
		gomega.Consistently(ctx, getPod).WithTimeout(f.Timeouts.PodStart).Should(e2epod.BeInPhase(v1.PodPending))
	})

	//ConfigMap object defined for the pod, If a key is specified which is not present in the ConfigMap,
	// the volume setup will error unless it is marked optional, during the pod creation.
	//Slow (~5 mins)
	f.It("Should fail non-optional pod creation due to the key in the configMap object does not exist", f.WithSlow(), func(ctx context.Context) {
		volumeMountPath := "/etc/configmap-volumes"
		pod := createNonOptionalConfigMapPodWithConfig(ctx, f, volumeMountPath)
		getPod := e2epod.Get(f.ClientSet, pod)
		gomega.Consistently(ctx, getPod).WithTimeout(f.Timeouts.PodStart).Should(e2epod.BeInPhase(v1.PodPending))
	})
})

func doProjectedConfigMapE2EWithoutMappings(ctx context.Context, f *framework.Framework, asUser bool, fsGroup int64, defaultMode *int32) {
	groupID := int64(fsGroup)

	var (
		name            = "projected-configmap-test-volume-" + string(uuid.NewUUID())
		volumeName      = "projected-configmap-volume"
		volumeMountPath = "/etc/projected-configmap-volume"
		configMap       = newConfigMap(f, name)
	)

	ginkgo.By(fmt.Sprintf("Creating configMap with name %s", configMap.Name))
	var err error
	if configMap, err = f.ClientSet.CoreV1().ConfigMaps(f.Namespace.Name).Create(ctx, configMap, metav1.CreateOptions{}); err != nil {
		framework.Failf("unable to create test configMap %s: %v", configMap.Name, err)
	}

	pod := createProjectedConfigMapMounttestPod(f.Namespace.Name, volumeName, name, volumeMountPath,
		"--file_content=/etc/projected-configmap-volume/data-1", "--file_mode=/etc/projected-configmap-volume/data-1")

	if asUser {
		setPodNonRootUser(pod)
	}

	if groupID != 0 {
		pod.Spec.SecurityContext.FSGroup = &groupID
	}

	if defaultMode != nil {
		//pod.Spec.Volumes[0].VolumeSource.Projected.Sources[0].ConfigMap.DefaultMode = defaultMode
		pod.Spec.Volumes[0].VolumeSource.Projected.DefaultMode = defaultMode
	}

	fileModeRegexp := getFileModeRegex("/etc/projected-configmap-volume/data-1", defaultMode)
	output := []string{
		"content of file \"/etc/projected-configmap-volume/data-1\": value-1",
		fileModeRegexp,
	}
	e2epodoutput.TestContainerOutputRegexp(ctx, f, "consume configMaps", pod, 0, output)
}

func doProjectedConfigMapE2EWithMappings(ctx context.Context, f *framework.Framework, asUser bool, fsGroup int64, itemMode *int32) {
	groupID := int64(fsGroup)

	var (
		name            = "projected-configmap-test-volume-map-" + string(uuid.NewUUID())
		volumeName      = "projected-configmap-volume"
		volumeMountPath = "/etc/projected-configmap-volume"
		configMap       = newConfigMap(f, name)
	)

	ginkgo.By(fmt.Sprintf("Creating configMap with name %s", configMap.Name))

	var err error
	if configMap, err = f.ClientSet.CoreV1().ConfigMaps(f.Namespace.Name).Create(ctx, configMap, metav1.CreateOptions{}); err != nil {
		framework.Failf("unable to create test configMap %s: %v", configMap.Name, err)
	}

	pod := createProjectedConfigMapMounttestPod(f.Namespace.Name, volumeName, name, volumeMountPath,
		"--file_content=/etc/projected-configmap-volume/path/to/data-2", "--file_mode=/etc/projected-configmap-volume/path/to/data-2")
	pod.Spec.Volumes[0].VolumeSource.Projected.Sources[0].ConfigMap.Items = []v1.KeyToPath{
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
		//pod.Spec.Volumes[0].VolumeSource.ConfigMap.Items[0].Mode = itemMode
		pod.Spec.Volumes[0].VolumeSource.Projected.DefaultMode = itemMode
	}

	// Just check file mode if fsGroup is not set. If fsGroup is set, the
	// final mode is adjusted and we are not testing that case.
	output := []string{
		"content of file \"/etc/projected-configmap-volume/path/to/data-2\": value-2",
	}
	if fsGroup == 0 {
		fileModeRegexp := getFileModeRegex("/etc/projected-configmap-volume/path/to/data-2", itemMode)
		output = append(output, fileModeRegexp)
	}
	e2epodoutput.TestContainerOutputRegexp(ctx, f, "consume configMaps", pod, 0, output)
}

func createProjectedConfigMapMounttestPod(namespace, volumeName, referenceName, mountPath string, mounttestArgs ...string) *v1.Pod {
	volumes := []v1.Volume{
		{
			Name: volumeName,
			VolumeSource: v1.VolumeSource{
				Projected: &v1.ProjectedVolumeSource{
					Sources: []v1.VolumeProjection{
						{
							ConfigMap: &v1.ConfigMapProjection{
								LocalObjectReference: v1.LocalObjectReference{
									Name: referenceName,
								},
							},
						},
					},
				},
			},
		},
	}
	podName := "pod-projected-configmaps-" + string(uuid.NewUUID())
	mounttestArgs = append([]string{"mounttest"}, mounttestArgs...)
	pod := e2epod.NewAgnhostPod(namespace, podName, volumes, createMounts(volumeName, mountPath, true), nil, mounttestArgs...)
	pod.Spec.RestartPolicy = v1.RestartPolicyNever
	return pod
}
