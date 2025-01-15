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
	imageutils "k8s.io/kubernetes/test/utils/image"
	admissionapi "k8s.io/pod-security-admission/api"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
)

var _ = SIGDescribe("Projected secret", func() {
	f := framework.NewDefaultFramework("projected")
	f.NamespacePodSecurityLevel = admissionapi.LevelBaseline

	/*
	   Release: v1.9
	   Testname: Projected Volume, Secrets, volume mode default
	   Description: A Pod is created with a projected volume source 'secret' to store a secret with a specified key with default permission mode. Pod MUST be able to read the content of the key successfully and the mode MUST be -rw-r--r-- by default.
	*/
	framework.ConformanceIt("should be consumable from pods in volume", f.WithNodeConformance(), func(ctx context.Context) {
		doProjectedSecretE2EWithoutMapping(ctx, f, nil /* default mode */, "projected-secret-test-"+string(uuid.NewUUID()), nil, nil)
	})

	/*
	   Release: v1.9
	   Testname: Projected Volume, Secrets, volume mode 0400
	   Description: A Pod is created with a projected volume source 'secret' to store a secret with a specified key with permission mode set to 0x400 on the Pod. Pod MUST be able to read the content of the key successfully and the mode MUST be -r--------.
	*/
	framework.ConformanceIt("should be consumable from pods in volume with defaultMode set", f.WithNodeConformance(), func(ctx context.Context) {
		defaultMode := int32(0400)
		doProjectedSecretE2EWithoutMapping(ctx, f, &defaultMode, "projected-secret-test-"+string(uuid.NewUUID()), nil, nil)
	})

	/*
	   Release: v1.9
	   Testname: Project Volume, Secrets, non-root, custom fsGroup
	   Description: A Pod is created with a projected volume source 'secret' to store a secret with a specified key. The volume has permission mode set to 0440, fsgroup set to 1001 and user set to non-root uid of 1000. Pod MUST be able to read the content of the key successfully and the mode MUST be -r--r-----.
	   This test is marked LinuxOnly since Windows does not support running as UID / GID.
	*/
	framework.ConformanceIt("should be consumable from pods in volume as non-root with defaultMode and fsGroup set [LinuxOnly]", f.WithNodeConformance(), func(ctx context.Context) {
		defaultMode := int32(0440) /* setting fsGroup sets mode to at least 440 */
		fsGroup := int64(1001)
		doProjectedSecretE2EWithoutMapping(ctx, f, &defaultMode, "projected-secret-test-"+string(uuid.NewUUID()), &fsGroup, &nonRootTestUserID)
	})

	/*
	   Release: v1.9
	   Testname: Projected Volume, Secrets, mapped
	   Description: A Pod is created with a projected volume source 'secret' to store a secret with a specified key with default permission mode. The secret is also mapped to a custom path. Pod MUST be able to read the content of the key successfully and the mode MUST be -r--------on the mapped volume.
	*/
	framework.ConformanceIt("should be consumable from pods in volume with mappings", f.WithNodeConformance(), func(ctx context.Context) {
		doProjectedSecretE2EWithMapping(ctx, f, nil)
	})

	/*
	   Release: v1.9
	   Testname: Projected Volume, Secrets, mapped, volume mode 0400
	   Description: A Pod is created with a projected volume source 'secret' to store a secret with a specified key with permission mode set to 0400. The secret is also mapped to a specific name. Pod MUST be able to read the content of the key successfully and the mode MUST be -r-------- on the mapped volume.
	*/
	framework.ConformanceIt("should be consumable from pods in volume with mappings and Item Mode set", f.WithNodeConformance(), func(ctx context.Context) {
		mode := int32(0400)
		doProjectedSecretE2EWithMapping(ctx, f, &mode)
	})

	f.It("should be able to mount in a volume regardless of a different secret existing with same name in different namespace", f.WithNodeConformance(), func(ctx context.Context) {
		var (
			namespace2  *v1.Namespace
			err         error
			secret2Name = "projected-secret-test-" + string(uuid.NewUUID())
		)

		if namespace2, err = f.CreateNamespace(ctx, "secret-namespace", nil); err != nil {
			framework.Failf("unable to create new namespace %s: %v", namespace2.Name, err)
		}

		secret2 := secretForTest(namespace2.Name, secret2Name)
		secret2.Data = map[string][]byte{
			"this_should_not_match_content_of_other_secret": []byte("similarly_this_should_not_match_content_of_other_secret\n"),
		}
		if secret2, err = f.ClientSet.CoreV1().Secrets(namespace2.Name).Create(ctx, secret2, metav1.CreateOptions{}); err != nil {
			framework.Failf("unable to create test secret %s: %v", secret2.Name, err)
		}
		doProjectedSecretE2EWithoutMapping(ctx, f, nil /* default mode */, secret2.Name, nil, nil)
	})

	/*
	   Release: v1.9
	   Testname: Projected Volume, Secrets, mapped, multiple paths
	   Description: A Pod is created with a projected volume source 'secret' to store a secret with a specified key. The secret is mapped to two different volume mounts. Pod MUST be able to read the content of the key successfully from the two volume mounts and the mode MUST be -r-------- on the mapped volumes.
	*/
	framework.ConformanceIt("should be consumable in multiple volumes in a pod", f.WithNodeConformance(), func(ctx context.Context) {
		// This test ensures that the same secret can be mounted in multiple
		// volumes in the same pod.  This test case exists to prevent
		// regressions that break this use-case.
		var (
			name             = "projected-secret-test-" + string(uuid.NewUUID())
			volumeName       = "projected-secret-volume"
			volumeMountPath  = "/etc/projected-secret-volume"
			volumeName2      = "projected-secret-volume-2"
			volumeMountPath2 = "/etc/projected-secret-volume-2"
			secret           = secretForTest(f.Namespace.Name, name)
		)

		ginkgo.By(fmt.Sprintf("Creating secret with name %s", secret.Name))
		var err error
		if secret, err = f.ClientSet.CoreV1().Secrets(f.Namespace.Name).Create(ctx, secret, metav1.CreateOptions{}); err != nil {
			framework.Failf("unable to create test secret %s: %v", secret.Name, err)
		}

		pod := &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: "pod-projected-secrets-" + string(uuid.NewUUID()),
			},
			Spec: v1.PodSpec{
				Volumes: []v1.Volume{
					{
						Name: volumeName,
						VolumeSource: v1.VolumeSource{
							Projected: &v1.ProjectedVolumeSource{
								Sources: []v1.VolumeProjection{
									{
										Secret: &v1.SecretProjection{
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
										Secret: &v1.SecretProjection{
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
						Name:  "secret-volume-test",
						Image: imageutils.GetE2EImage(imageutils.Agnhost),
						Args: []string{
							"mounttest",
							"--file_content=/etc/projected-secret-volume/data-1",
							"--file_mode=/etc/projected-secret-volume/data-1"},
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

		fileModeRegexp := getFileModeRegex("/etc/projected-secret-volume/data-1", nil)
		e2epodoutput.TestContainerOutputRegexp(ctx, f, "consume secrets", pod, 0, []string{
			"content of file \"/etc/projected-secret-volume/data-1\": value-1",
			fileModeRegexp,
		})
	})

	/*
	   Release: v1.9
	   Testname: Projected Volume, Secrets, create, update delete
	   Description: Create a Pod with three containers with secrets namely a create, update and delete container. Create Container when started MUST no have a secret, update and delete containers MUST be created with a secret value. Create a secret in the create container, the Pod MUST be able to read the secret from the create container. Update the secret in the update container, Pod MUST be able to read the updated secret value. Delete the secret in the delete container. Pod MUST fail to read the secret from the delete container.
	*/
	framework.ConformanceIt("optional updates should be reflected in volume", f.WithNodeConformance(), func(ctx context.Context) {
		podLogTimeout := e2epod.GetPodSecretUpdateTimeout(ctx, f.ClientSet)
		containerTimeoutArg := fmt.Sprintf("--retry_time=%v", int(podLogTimeout.Seconds()))
		trueVal := true
		volumeMountPath := "/etc/projected-secret-volumes"

		deleteName := "s-test-opt-del-" + string(uuid.NewUUID())
		deleteContainerName := "dels-volume-test"
		deleteVolumeName := "deletes-volume"
		deleteSecret := &v1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: f.Namespace.Name,
				Name:      deleteName,
			},
			Data: map[string][]byte{
				"data-1": []byte("value-1"),
			},
		}

		updateName := "s-test-opt-upd-" + string(uuid.NewUUID())
		updateContainerName := "upds-volume-test"
		updateVolumeName := "updates-volume"
		updateSecret := &v1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: f.Namespace.Name,
				Name:      updateName,
			},
			Data: map[string][]byte{
				"data-1": []byte("value-1"),
			},
		}

		createName := "s-test-opt-create-" + string(uuid.NewUUID())
		createContainerName := "creates-volume-test"
		createVolumeName := "creates-volume"
		createSecret := &v1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: f.Namespace.Name,
				Name:      createName,
			},
			Data: map[string][]byte{
				"data-1": []byte("value-1"),
			},
		}

		ginkgo.By(fmt.Sprintf("Creating secret with name %s", deleteSecret.Name))
		var err error
		if deleteSecret, err = f.ClientSet.CoreV1().Secrets(f.Namespace.Name).Create(ctx, deleteSecret, metav1.CreateOptions{}); err != nil {
			framework.Failf("unable to create test secret %s: %v", deleteSecret.Name, err)
		}

		ginkgo.By(fmt.Sprintf("Creating secret with name %s", updateSecret.Name))
		if updateSecret, err = f.ClientSet.CoreV1().Secrets(f.Namespace.Name).Create(ctx, updateSecret, metav1.CreateOptions{}); err != nil {
			framework.Failf("unable to create test secret %s: %v", updateSecret.Name, err)
		}

		pod := &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: "pod-projected-secrets-" + string(uuid.NewUUID()),
			},
			Spec: v1.PodSpec{
				Volumes: []v1.Volume{
					{
						Name: deleteVolumeName,
						VolumeSource: v1.VolumeSource{
							Projected: &v1.ProjectedVolumeSource{
								Sources: []v1.VolumeProjection{
									{
										Secret: &v1.SecretProjection{
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
										Secret: &v1.SecretProjection{
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
										Secret: &v1.SecretProjection{
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
						Args:  []string{"mounttest", "--break_on_expected_content=false", containerTimeoutArg, "--file_content_in_loop=/etc/projected-secret-volumes/delete/data-1"},
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
						Args:  []string{"mounttest", "--break_on_expected_content=false", containerTimeoutArg, "--file_content_in_loop=/etc/projected-secret-volumes/update/data-3"},
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
						Args:  []string{"mounttest", "--break_on_expected_content=false", containerTimeoutArg, "--file_content_in_loop=/etc/projected-secret-volumes/create/data-1"},
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
		gomega.Eventually(ctx, pollCreateLogs, podLogTimeout, framework.Poll).Should(gomega.ContainSubstring("Error reading file /etc/projected-secret-volumes/create/data-1"))

		pollUpdateLogs := func() (string, error) {
			return e2epod.GetPodLogs(ctx, f.ClientSet, f.Namespace.Name, pod.Name, updateContainerName)
		}
		gomega.Eventually(ctx, pollUpdateLogs, podLogTimeout, framework.Poll).Should(gomega.ContainSubstring("Error reading file /etc/projected-secret-volumes/update/data-3"))

		pollDeleteLogs := func() (string, error) {
			return e2epod.GetPodLogs(ctx, f.ClientSet, f.Namespace.Name, pod.Name, deleteContainerName)
		}
		gomega.Eventually(ctx, pollDeleteLogs, podLogTimeout, framework.Poll).Should(gomega.ContainSubstring("value-1"))

		ginkgo.By(fmt.Sprintf("Deleting secret %v", deleteSecret.Name))
		err = f.ClientSet.CoreV1().Secrets(f.Namespace.Name).Delete(ctx, deleteSecret.Name, metav1.DeleteOptions{})
		framework.ExpectNoError(err, "Failed to delete secret %q in namespace %q", deleteSecret.Name, f.Namespace.Name)

		ginkgo.By(fmt.Sprintf("Updating secret %v", updateSecret.Name))
		updateSecret.ResourceVersion = "" // to force update
		delete(updateSecret.Data, "data-1")
		updateSecret.Data["data-3"] = []byte("value-3")
		_, err = f.ClientSet.CoreV1().Secrets(f.Namespace.Name).Update(ctx, updateSecret, metav1.UpdateOptions{})
		framework.ExpectNoError(err, "Failed to update secret %q in namespace %q", updateSecret.Name, f.Namespace.Name)

		ginkgo.By(fmt.Sprintf("Creating secret with name %s", createSecret.Name))
		if createSecret, err = f.ClientSet.CoreV1().Secrets(f.Namespace.Name).Create(ctx, createSecret, metav1.CreateOptions{}); err != nil {
			framework.Failf("unable to create test secret %s: %v", createSecret.Name, err)
		}

		ginkgo.By("waiting to observe update in volume")

		gomega.Eventually(ctx, pollCreateLogs, podLogTimeout, framework.Poll).Should(gomega.ContainSubstring("value-1"))
		gomega.Eventually(ctx, pollUpdateLogs, podLogTimeout, framework.Poll).Should(gomega.ContainSubstring("value-3"))
		gomega.Eventually(ctx, pollDeleteLogs, podLogTimeout, framework.Poll).Should(gomega.ContainSubstring("Error reading file /etc/projected-secret-volumes/delete/data-1"))
	})

	//The secret is in pending during volume creation until the secret objects are available
	//or until mount the secret volume times out. There is no secret object defined for the pod, so it should return timeout exception unless it is marked optional.
	//Slow (~5 mins)
	f.It("Should fail non-optional pod creation due to secret object does not exist", f.WithSlow(), func(ctx context.Context) {
		volumeMountPath := "/etc/projected-secret-volumes"
		podName := "pod-secrets-" + string(uuid.NewUUID())
		pod := createNonOptionalSecretPod(ctx, f, volumeMountPath, podName)
		getPod := e2epod.Get(f.ClientSet, pod)
		gomega.Consistently(ctx, getPod).WithTimeout(f.Timeouts.PodStart).Should(e2epod.BeInPhase(v1.PodPending))
	})

	//Secret object defined for the pod, If a key is specified which is not present in the secret,
	// the volume setup will error unless it is marked optional, during the pod creation.
	//Slow (~5 mins)
	f.It("Should fail non-optional pod creation due to the key in the secret object does not exist", f.WithSlow(), func(ctx context.Context) {
		volumeMountPath := "/etc/secret-volumes"
		podName := "pod-secrets-" + string(uuid.NewUUID())
		pod := createNonOptionalSecretPodWithSecret(ctx, f, volumeMountPath, podName)
		getPod := e2epod.Get(f.ClientSet, pod)
		gomega.Consistently(ctx, getPod).WithTimeout(f.Timeouts.PodStart).Should(e2epod.BeInPhase(v1.PodPending))
	})
})

func doProjectedSecretE2EWithoutMapping(ctx context.Context, f *framework.Framework, defaultMode *int32,
	secretName string, fsGroup *int64, uid *int64) {
	var (
		volumeName      = "projected-secret-volume"
		volumeMountPath = "/etc/projected-secret-volume"
		secret          = secretForTest(f.Namespace.Name, secretName)
	)

	ginkgo.By(fmt.Sprintf("Creating projection with secret that has name %s", secret.Name))
	var err error
	if secret, err = f.ClientSet.CoreV1().Secrets(f.Namespace.Name).Create(ctx, secret, metav1.CreateOptions{}); err != nil {
		framework.Failf("unable to create test secret %s: %v", secret.Name, err)
	}

	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod-projected-secrets-" + string(uuid.NewUUID()),
			Namespace: f.Namespace.Name,
		},
		Spec: v1.PodSpec{
			Volumes: []v1.Volume{
				{
					Name: volumeName,
					VolumeSource: v1.VolumeSource{
						Projected: &v1.ProjectedVolumeSource{
							Sources: []v1.VolumeProjection{
								{
									Secret: &v1.SecretProjection{
										LocalObjectReference: v1.LocalObjectReference{
											Name: secretName,
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
					Name:  "projected-secret-volume-test",
					Image: imageutils.GetE2EImage(imageutils.Agnhost),
					Args: []string{
						"mounttest",
						"--file_content=/etc/projected-secret-volume/data-1",
						"--file_mode=/etc/projected-secret-volume/data-1"},
					VolumeMounts: []v1.VolumeMount{
						{
							Name:      volumeName,
							MountPath: volumeMountPath,
						},
					},
				},
			},
			RestartPolicy: v1.RestartPolicyNever,
		},
	}

	if defaultMode != nil {
		//pod.Spec.Volumes[0].VolumeSource.Projected.Sources[0].Secret.DefaultMode = defaultMode
		pod.Spec.Volumes[0].VolumeSource.Projected.DefaultMode = defaultMode
	}

	if fsGroup != nil || uid != nil {
		pod.Spec.SecurityContext = &v1.PodSecurityContext{
			FSGroup:   fsGroup,
			RunAsUser: uid,
		}
	}

	fileModeRegexp := getFileModeRegex("/etc/projected-secret-volume/data-1", defaultMode)
	expectedOutput := []string{
		"content of file \"/etc/projected-secret-volume/data-1\": value-1",
		fileModeRegexp,
	}

	e2epodoutput.TestContainerOutputRegexp(ctx, f, "consume secrets", pod, 0, expectedOutput)
}

func doProjectedSecretE2EWithMapping(ctx context.Context, f *framework.Framework, mode *int32) {
	var (
		name            = "projected-secret-test-map-" + string(uuid.NewUUID())
		volumeName      = "projected-secret-volume"
		volumeMountPath = "/etc/projected-secret-volume"
		secret          = secretForTest(f.Namespace.Name, name)
	)

	ginkgo.By(fmt.Sprintf("Creating projection with secret that has name %s", secret.Name))
	var err error
	if secret, err = f.ClientSet.CoreV1().Secrets(f.Namespace.Name).Create(ctx, secret, metav1.CreateOptions{}); err != nil {
		framework.Failf("unable to create test secret %s: %v", secret.Name, err)
	}

	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pod-projected-secrets-" + string(uuid.NewUUID()),
		},
		Spec: v1.PodSpec{
			Volumes: []v1.Volume{
				{
					Name: volumeName,
					VolumeSource: v1.VolumeSource{
						Projected: &v1.ProjectedVolumeSource{
							Sources: []v1.VolumeProjection{
								{
									Secret: &v1.SecretProjection{
										LocalObjectReference: v1.LocalObjectReference{
											Name: name,
										},
										Items: []v1.KeyToPath{
											{
												Key:  "data-1",
												Path: "new-path-data-1",
											},
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
					Name:  "projected-secret-volume-test",
					Image: imageutils.GetE2EImage(imageutils.Agnhost),
					Args: []string{
						"mounttest",
						"--file_content=/etc/projected-secret-volume/new-path-data-1",
						"--file_mode=/etc/projected-secret-volume/new-path-data-1"},
					VolumeMounts: []v1.VolumeMount{
						{
							Name:      volumeName,
							MountPath: volumeMountPath,
						},
					},
				},
			},
			RestartPolicy: v1.RestartPolicyNever,
		},
	}

	if mode != nil {
		//pod.Spec.Volumes[0].VolumeSource.Projected.Sources[0].Secret.Items[0].Mode = mode
		pod.Spec.Volumes[0].VolumeSource.Projected.DefaultMode = mode
	}

	fileModeRegexp := getFileModeRegex("/etc/projected-secret-volume/new-path-data-1", mode)
	expectedOutput := []string{
		"content of file \"/etc/projected-secret-volume/new-path-data-1\": value-1",
		fileModeRegexp,
	}

	e2epodoutput.TestContainerOutputRegexp(ctx, f, "consume secrets", pod, 0, expectedOutput)
}
