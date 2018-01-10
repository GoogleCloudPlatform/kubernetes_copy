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

package common

import (
	"fmt"
	"os"
	"path"

	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/kubernetes/test/e2e/framework"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("[sig-storage] Secrets", func() {
	f := framework.NewDefaultFramework("secrets")

	/*
		    Testname: secret-volume-mount-without-mapping
		    Description: Ensure that secret can be mounted without mapping to a
			pod volume.
	*/
	framework.ConformanceIt("should be consumable from pods in volume ", func() {
		doSecretE2EWithoutMapping(f, nil /* default mode */, "secret-test-"+string(uuid.NewUUID()), nil, nil)
	})

	/*
		    Testname: secret-volume-mount-without-mapping-default-mode
		    Description: Ensure that secret can be mounted without mapping to a
			pod volume in default mode.
	*/
	framework.ConformanceIt("should be consumable from pods in volume with defaultMode set ", func() {
		defaultMode := int32(0400)
		doSecretE2EWithoutMapping(f, &defaultMode, "secret-test-"+string(uuid.NewUUID()), nil, nil)
	})

	/*
		    Testname: secret-volume-mount-without-mapping-non-root-default-mode-fsgroup
		    Description: Ensure that secret can be mounted without mapping to a pod
			volume as non-root in default mode with fsGroup set.
	*/
	framework.ConformanceIt("should be consumable from pods in volume as non-root with defaultMode and fsGroup set ", func() {
		defaultMode := int32(0440) /* setting fsGroup sets mode to at least 440 */
		fsGroup := int64(1001)
		uid := int64(1000)
		doSecretE2EWithoutMapping(f, &defaultMode, "secret-test-"+string(uuid.NewUUID()), &fsGroup, &uid)
	})

	/*
		    Testname: secret-volume-mount-with-mapping
		    Description: Ensure that secret can be mounted with mapping to a pod
			volume.
	*/
	framework.ConformanceIt("should be consumable from pods in volume with mappings ", func() {
		doSecretE2EWithMapping(f, nil)
	})

	/*
		    Testname: secret-volume-mount-with-mapping-item-mode
		    Description: Ensure that secret can be mounted with mapping to a pod
			volume in item mode.
	*/
	framework.ConformanceIt("should be consumable from pods in volume with mappings and Item Mode set ", func() {
		mode := int32(0400)
		doSecretE2EWithMapping(f, &mode)
	})

	It("should be able to mount in a volume regardless of a different secret existing with same name in different namespace", func() {
		var (
			namespace2  *v1.Namespace
			err         error
			secret2Name = "secret-test-" + string(uuid.NewUUID())
		)

		if namespace2, err = f.CreateNamespace("secret-namespace", nil); err != nil {
			framework.Failf("unable to create new namespace %s: %v", namespace2.Name, err)
		}

		secret2 := secretForTest(namespace2.Name, secret2Name)
		secret2.Data = map[string][]byte{
			"this_should_not_match_content_of_other_secret": []byte("similarly_this_should_not_match_content_of_other_secret\n"),
		}
		if secret2, err = f.ClientSet.CoreV1().Secrets(namespace2.Name).Create(secret2); err != nil {
			framework.Failf("unable to create test secret %s: %v", secret2.Name, err)
		}
		doSecretE2EWithoutMapping(f, nil /* default mode */, secret2.Name, nil, nil)
	})

	/*
	   Testname: secret-multiple-volume-mounts
	   Description: Ensure that secret can be mounted to multiple pod volumes.
	*/
	framework.ConformanceIt("should be consumable in multiple volumes in a pod ", func() {
		// This test ensures that the same secret can be mounted in multiple
		// volumes in the same pod.  This test case exists to prevent
		// regressions that break this use-case.
		var (
			name             = "secret-test-" + string(uuid.NewUUID())
			volumeName       = "secret-volume"
			volumeMountPath  = "/etc/secret-volume"
			volumeName2      = "secret-volume-2"
			volumeMountPath2 = "/etc/secret-volume-2"
			secret           = secretForTest(f.Namespace.Name, name)
		)

		By(fmt.Sprintf("Creating secret with name %s", secret.Name))
		var err error
		if secret, err = f.ClientSet.CoreV1().Secrets(f.Namespace.Name).Create(secret); err != nil {
			framework.Failf("unable to create test secret %s: %v", secret.Name, err)
		}

		pod := &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: "pod-secrets-" + string(uuid.NewUUID()),
			},
			Spec: v1.PodSpec{
				Volumes: []v1.Volume{
					{
						Name: volumeName,
						VolumeSource: v1.VolumeSource{
							Secret: &v1.SecretVolumeSource{
								SecretName: name,
							},
						},
					},
					{
						Name: volumeName2,
						VolumeSource: v1.VolumeSource{
							Secret: &v1.SecretVolumeSource{
								SecretName: name,
							},
						},
					},
				},
				Containers: []v1.Container{
					{
						Name:  "secret-volume-test",
						Image: mountImage,
						Args: []string{
							"--file_content=/etc/secret-volume/data-1",
							"--file_mode=/etc/secret-volume/data-1"},
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

		f.TestContainerOutput("consume secrets", pod, 0, []string{
			"content of file \"/etc/secret-volume/data-1\": value-1",
			"mode of file \"/etc/secret-volume/data-1\": -rw-r--r--",
		})
	})

	/*
	   Testname: secret-file-mode
	   Description: Ensure that secret files are mounted with the correct mode
	*/
	framework.ConformanceIt("should be mounted with the correct mode", func() {
		// This test ensures that the specified file mode is honored, even when a fsGroup is present
		var (
			name            = "secret-test-" + string(uuid.NewUUID())
			volumeName      = "secret-volume"
			volumeMountPath = "/etc/secret-volume"
			secret          = secretForTest(f.Namespace.Name, name)
		)

		By(fmt.Sprintf("Creating secret with name %s", secret.Name))
		var err error
		if secret, err = f.ClientSet.CoreV1().Secrets(f.Namespace.Name).Create(secret); err != nil {
			framework.Failf("unable to create test secret %s: %v", secret.Name, err)
		}

		ownerRO := int32(0400)
		ownerRX := int32(0500)
		ownerRW := int32(0600)

		uid1000 := int64(1000)
		uid2000 := int64(2000)

		fsGroup := int64(3000)

		pod := f.PodClient().Create(&v1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "pod-secrets-" + string(uuid.NewUUID())},
			Spec: v1.PodSpec{
				Volumes: []v1.Volume{{
					Name: volumeName,
					VolumeSource: v1.VolumeSource{
						Secret: &v1.SecretVolumeSource{
							SecretName:  name,
							DefaultMode: &ownerRO,
							Items: []v1.KeyToPath{
								{Key: "data-1", Path: "data-1"}, // data-1 uses default mode
								{Key: "data-2", Path: "data-2", Mode: &ownerRX},
								{Key: "data-3", Path: "data-3", Mode: &ownerRW},
							},
						},
					},
				}},
				SecurityContext: &v1.PodSecurityContext{FSGroup: &fsGroup},
				Containers: []v1.Container{
					{
						Name:            "data-1-uid-1000",
						Image:           mountImage,
						Args:            []string{"--file_content=/etc/secret-volume/data-1", "--file_mode=/etc/secret-volume/data-1", "--file_owner=/etc/secret-volume/data-1"},
						VolumeMounts:    []v1.VolumeMount{{Name: volumeName, MountPath: volumeMountPath, ReadOnly: true}},
						SecurityContext: &v1.SecurityContext{RunAsUser: &uid1000},
					},
					{
						Name:            "data-1-uid-2000",
						Image:           mountImage,
						Args:            []string{"--file_content=/etc/secret-volume/data-1", "--file_mode=/etc/secret-volume/data-1", "--file_owner=/etc/secret-volume/data-1"},
						VolumeMounts:    []v1.VolumeMount{{Name: volumeName, MountPath: volumeMountPath, ReadOnly: true}},
						SecurityContext: &v1.SecurityContext{RunAsUser: &uid2000},
					},
					{
						Name:         "data-2",
						Image:        mountImage,
						Args:         []string{"--file_content=/etc/secret-volume/data-2", "--file_mode=/etc/secret-volume/data-2"},
						VolumeMounts: []v1.VolumeMount{{Name: volumeName, MountPath: volumeMountPath, ReadOnly: true}},
					},
					{
						Name:         "data-3",
						Image:        mountImage,
						Args:         []string{"--file_content=/etc/secret-volume/data-3", "--file_mode=/etc/secret-volume/data-3"},
						VolumeMounts: []v1.VolumeMount{{Name: volumeName, MountPath: volumeMountPath, ReadOnly: true}},
					},
				},
				RestartPolicy: v1.RestartPolicyNever,
			},
		})
		errRun := framework.WaitForPodSuccessInNamespace(f.ClientSet, pod.Name, f.Namespace.Name)

		data1uid1000Logs, err1 := framework.GetPodLogs(f.ClientSet, f.Namespace.Name, pod.Name, pod.Spec.Containers[0].Name)
		framework.Logf("container[0]: %s", data1uid1000Logs)
		data1uid2000Logs, err2 := framework.GetPodLogs(f.ClientSet, f.Namespace.Name, pod.Name, pod.Spec.Containers[1].Name)
		framework.Logf("container[0]: %s", data1uid2000Logs)
		data2Logs, err3 := framework.GetPodLogs(f.ClientSet, f.Namespace.Name, pod.Name, pod.Spec.Containers[2].Name)
		framework.Logf("container[1]: %s", data2Logs)
		data3Logs, err4 := framework.GetPodLogs(f.ClientSet, f.Namespace.Name, pod.Name, pod.Spec.Containers[3].Name)
		framework.Logf("container[2]: %s", data3Logs)

		Expect(errRun).NotTo(HaveOccurred())
		Expect(err1).NotTo(HaveOccurred())
		Expect(err2).NotTo(HaveOccurred())
		Expect(err3).NotTo(HaveOccurred())
		Expect(err4).NotTo(HaveOccurred())

		Expect(data1uid1000Logs).To(ContainSubstring("value-1"))
		Expect(data1uid1000Logs).To(ContainSubstring(`content of file "/etc/secret-volume/data-1": value-1`))
		Expect(data1uid1000Logs).To(ContainSubstring(`mode of file "/etc/secret-volume/data-1": -r--------`))
		Expect(data1uid1000Logs).To(ContainSubstring(`owner UID of file "/etc/secret-volume/data-1": 1000`))
		Expect(data1uid1000Logs).To(ContainSubstring(`owner GID of file "/etc/secret-volume/data-1": 3000`))

		Expect(data1uid2000Logs).To(ContainSubstring("value-1"))
		Expect(data1uid2000Logs).To(ContainSubstring(`content of file "/etc/secret-volume/data-1": value-1`))
		Expect(data1uid2000Logs).To(ContainSubstring(`mode of file "/etc/secret-volume/data-1": -r--------`))
		Expect(data1uid2000Logs).To(ContainSubstring(`owner UID of file "/etc/secret-volume/data-1": 2000`))
		Expect(data1uid2000Logs).To(ContainSubstring(`owner GID of file "/etc/secret-volume/data-1": 3000`))

		Expect(data2Logs).To(ContainSubstring("value-2"))
		Expect(data2Logs).To(ContainSubstring(`content of file "/etc/secret-volume/data-2": value-2`))
		Expect(data2Logs).To(ContainSubstring(`mode of file "/etc/secret-volume/data-2": -r-x------`))
		Expect(data2Logs).To(ContainSubstring(`owner GID of file "/etc/secret-volume/data-2": 3000`))

		Expect(data3Logs).To(ContainSubstring("value-3"))
		Expect(data3Logs).To(ContainSubstring(`content of file "/etc/secret-volume/data-3": value-3`))
		Expect(data3Logs).To(ContainSubstring(`mode of file "/etc/secret-volume/data-3": -rw-------`))
		Expect(data3Logs).To(ContainSubstring(`owner GID of file "/etc/secret-volume/data-3": 3000`))
	})

	/*
		    Testname: secret-mounted-volume-optional-update-change
		    Description: Ensure that optional update change to secret can be
			reflected on a mounted volume.
	*/
	framework.ConformanceIt("optional updates should be reflected in volume ", func() {
		podLogTimeout := framework.GetPodSecretUpdateTimeout(f.ClientSet)
		containerTimeoutArg := fmt.Sprintf("--retry_time=%v", int(podLogTimeout.Seconds()))
		trueVal := true
		volumeMountPath := "/etc/secret-volumes"

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

		By(fmt.Sprintf("Creating secret with name %s", deleteSecret.Name))
		var err error
		if deleteSecret, err = f.ClientSet.CoreV1().Secrets(f.Namespace.Name).Create(deleteSecret); err != nil {
			framework.Failf("unable to create test secret %s: %v", deleteSecret.Name, err)
		}

		By(fmt.Sprintf("Creating secret with name %s", updateSecret.Name))
		if updateSecret, err = f.ClientSet.CoreV1().Secrets(f.Namespace.Name).Create(updateSecret); err != nil {
			framework.Failf("unable to create test secret %s: %v", updateSecret.Name, err)
		}

		pod := &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: "pod-secrets-" + string(uuid.NewUUID()),
			},
			Spec: v1.PodSpec{
				Volumes: []v1.Volume{
					{
						Name: deleteVolumeName,
						VolumeSource: v1.VolumeSource{
							Secret: &v1.SecretVolumeSource{
								SecretName: deleteName,
								Optional:   &trueVal,
							},
						},
					},
					{
						Name: updateVolumeName,
						VolumeSource: v1.VolumeSource{
							Secret: &v1.SecretVolumeSource{
								SecretName: updateName,
								Optional:   &trueVal,
							},
						},
					},
					{
						Name: createVolumeName,
						VolumeSource: v1.VolumeSource{
							Secret: &v1.SecretVolumeSource{
								SecretName: createName,
								Optional:   &trueVal,
							},
						},
					},
				},
				Containers: []v1.Container{
					{
						Name:    deleteContainerName,
						Image:   mountImage,
						Command: []string{"/mounttest", "--break_on_expected_content=false", containerTimeoutArg, "--file_content_in_loop=/etc/secret-volumes/delete/data-1"},
						VolumeMounts: []v1.VolumeMount{
							{
								Name:      deleteVolumeName,
								MountPath: path.Join(volumeMountPath, "delete"),
								ReadOnly:  true,
							},
						},
					},
					{
						Name:    updateContainerName,
						Image:   mountImage,
						Command: []string{"/mounttest", "--break_on_expected_content=false", containerTimeoutArg, "--file_content_in_loop=/etc/secret-volumes/update/data-3"},
						VolumeMounts: []v1.VolumeMount{
							{
								Name:      updateVolumeName,
								MountPath: path.Join(volumeMountPath, "update"),
								ReadOnly:  true,
							},
						},
					},
					{
						Name:    createContainerName,
						Image:   mountImage,
						Command: []string{"/mounttest", "--break_on_expected_content=false", containerTimeoutArg, "--file_content_in_loop=/etc/secret-volumes/create/data-1"},
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
		By("Creating the pod")
		f.PodClient().CreateSync(pod)

		pollCreateLogs := func() (string, error) {
			return framework.GetPodLogs(f.ClientSet, f.Namespace.Name, pod.Name, createContainerName)
		}
		Eventually(pollCreateLogs, podLogTimeout, framework.Poll).Should(ContainSubstring("Error reading file /etc/secret-volumes/create/data-1"))

		pollUpdateLogs := func() (string, error) {
			return framework.GetPodLogs(f.ClientSet, f.Namespace.Name, pod.Name, updateContainerName)
		}
		Eventually(pollUpdateLogs, podLogTimeout, framework.Poll).Should(ContainSubstring("Error reading file /etc/secret-volumes/update/data-3"))

		pollDeleteLogs := func() (string, error) {
			return framework.GetPodLogs(f.ClientSet, f.Namespace.Name, pod.Name, deleteContainerName)
		}
		Eventually(pollDeleteLogs, podLogTimeout, framework.Poll).Should(ContainSubstring("value-1"))

		By(fmt.Sprintf("Deleting secret %v", deleteSecret.Name))
		err = f.ClientSet.CoreV1().Secrets(f.Namespace.Name).Delete(deleteSecret.Name, &metav1.DeleteOptions{})
		Expect(err).NotTo(HaveOccurred(), "Failed to delete secret %q in namespace %q", deleteSecret.Name, f.Namespace.Name)

		By(fmt.Sprintf("Updating secret %v", updateSecret.Name))
		updateSecret.ResourceVersion = "" // to force update
		delete(updateSecret.Data, "data-1")
		updateSecret.Data["data-3"] = []byte("value-3")
		_, err = f.ClientSet.CoreV1().Secrets(f.Namespace.Name).Update(updateSecret)
		Expect(err).NotTo(HaveOccurred(), "Failed to update secret %q in namespace %q", updateSecret.Name, f.Namespace.Name)

		By(fmt.Sprintf("Creating secret with name %s", createSecret.Name))
		if createSecret, err = f.ClientSet.CoreV1().Secrets(f.Namespace.Name).Create(createSecret); err != nil {
			framework.Failf("unable to create test secret %s: %v", createSecret.Name, err)
		}

		By("waiting to observe update in volume")

		Eventually(pollCreateLogs, podLogTimeout, framework.Poll).Should(ContainSubstring("value-1"))
		Eventually(pollUpdateLogs, podLogTimeout, framework.Poll).Should(ContainSubstring("value-3"))
		Eventually(pollDeleteLogs, podLogTimeout, framework.Poll).Should(ContainSubstring("Error reading file /etc/secret-volumes/delete/data-1"))
	})
})

func secretForTest(namespace, name string) *v1.Secret {
	return &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      name,
		},
		Data: map[string][]byte{
			"data-1": []byte("value-1\n"),
			"data-2": []byte("value-2\n"),
			"data-3": []byte("value-3\n"),
		},
	}
}

func doSecretE2EWithoutMapping(f *framework.Framework, defaultMode *int32, secretName string,
	fsGroup *int64, uid *int64) {
	var (
		volumeName      = "secret-volume"
		volumeMountPath = "/etc/secret-volume"
		secret          = secretForTest(f.Namespace.Name, secretName)
	)

	By(fmt.Sprintf("Creating secret with name %s", secret.Name))
	var err error
	if secret, err = f.ClientSet.CoreV1().Secrets(f.Namespace.Name).Create(secret); err != nil {
		framework.Failf("unable to create test secret %s: %v", secret.Name, err)
	}

	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod-secrets-" + string(uuid.NewUUID()),
			Namespace: f.Namespace.Name,
		},
		Spec: v1.PodSpec{
			Volumes: []v1.Volume{
				{
					Name: volumeName,
					VolumeSource: v1.VolumeSource{
						Secret: &v1.SecretVolumeSource{
							SecretName: secretName,
						},
					},
				},
			},
			Containers: []v1.Container{
				{
					Name:  "secret-volume-test",
					Image: mountImage,
					Args: []string{
						"--file_content=/etc/secret-volume/data-1",
						"--file_mode=/etc/secret-volume/data-1"},
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
		pod.Spec.Volumes[0].VolumeSource.Secret.DefaultMode = defaultMode
	} else {
		mode := int32(0644)
		defaultMode = &mode
	}

	if fsGroup != nil || uid != nil {
		pod.Spec.SecurityContext = &v1.PodSecurityContext{
			FSGroup:   fsGroup,
			RunAsUser: uid,
		}
	}

	modeString := fmt.Sprintf("%v", os.FileMode(*defaultMode))
	expectedOutput := []string{
		"content of file \"/etc/secret-volume/data-1\": value-1",
		"mode of file \"/etc/secret-volume/data-1\": " + modeString,
	}

	f.TestContainerOutput("consume secrets", pod, 0, expectedOutput)
}

func doSecretE2EWithMapping(f *framework.Framework, mode *int32) {
	var (
		name            = "secret-test-map-" + string(uuid.NewUUID())
		volumeName      = "secret-volume"
		volumeMountPath = "/etc/secret-volume"
		secret          = secretForTest(f.Namespace.Name, name)
	)

	By(fmt.Sprintf("Creating secret with name %s", secret.Name))
	var err error
	if secret, err = f.ClientSet.CoreV1().Secrets(f.Namespace.Name).Create(secret); err != nil {
		framework.Failf("unable to create test secret %s: %v", secret.Name, err)
	}

	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pod-secrets-" + string(uuid.NewUUID()),
		},
		Spec: v1.PodSpec{
			Volumes: []v1.Volume{
				{
					Name: volumeName,
					VolumeSource: v1.VolumeSource{
						Secret: &v1.SecretVolumeSource{
							SecretName: name,
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
			Containers: []v1.Container{
				{
					Name:  "secret-volume-test",
					Image: mountImage,
					Args: []string{
						"--file_content=/etc/secret-volume/new-path-data-1",
						"--file_mode=/etc/secret-volume/new-path-data-1"},
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
		pod.Spec.Volumes[0].VolumeSource.Secret.Items[0].Mode = mode
	} else {
		defaultItemMode := int32(0644)
		mode = &defaultItemMode
	}

	modeString := fmt.Sprintf("%v", os.FileMode(*mode))
	expectedOutput := []string{
		"content of file \"/etc/secret-volume/new-path-data-1\": value-1",
		"mode of file \"/etc/secret-volume/new-path-data-1\": " + modeString,
	}

	f.TestContainerOutput("consume secrets", pod, 0, expectedOutput)
}
