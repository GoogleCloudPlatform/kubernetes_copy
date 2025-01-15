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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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

var _ = SIGDescribe("Secrets", func() {
	f := framework.NewDefaultFramework("secrets")
	f.NamespacePodSecurityLevel = admissionapi.LevelBaseline

	/*
		Release: v1.9
		Testname: Secrets Volume, default
		Description: Create a secret. Create a Pod with secret volume source configured into the container. Pod MUST be able to read the secret from the mounted volume from the container runtime and the file mode of the secret MUST be -rw-r--r-- by default.
	*/
	framework.ConformanceIt("should be consumable from pods in volume", f.WithNodeConformance(), func(ctx context.Context) {
		doSecretE2EWithoutMapping(ctx, f, nil /* default mode */, "secret-test-"+string(uuid.NewUUID()), nil, nil)
	})

	/*
		Release: v1.9
		Testname: Secrets Volume, volume mode 0400
		Description: Create a secret. Create a Pod with secret volume source configured into the container with file mode set to 0x400. Pod MUST be able to read the secret from the mounted volume from the container runtime and the file mode of the secret MUST be -r-------- by default.
	*/
	framework.ConformanceIt("should be consumable from pods in volume with defaultMode set", f.WithNodeConformance(), func(ctx context.Context) {
		defaultMode := int32(0400)
		doSecretE2EWithoutMapping(ctx, f, &defaultMode, "secret-test-"+string(uuid.NewUUID()), nil, nil)
	})

	/*
		Release: v1.9
		Testname: Secrets Volume, volume mode 0440, fsGroup 1001 and uid 1000
		Description: Create a secret. Create a Pod with secret volume source configured into the container with file mode set to 0x440 as a non-root user with uid 1000 and fsGroup id 1001. Pod MUST be able to read the secret from the mounted volume from the container runtime and the file mode of the secret MUST be -r--r-----by default.
		This test is marked LinuxOnly since Windows does not support running as UID / GID.
	*/
	framework.ConformanceIt("should be consumable from pods in volume as non-root with defaultMode and fsGroup set [LinuxOnly]", f.WithNodeConformance(), func(ctx context.Context) {
		defaultMode := int32(0440) /* setting fsGroup sets mode to at least 440 */
		fsGroup := int64(1001)
		doSecretE2EWithoutMapping(ctx, f, &defaultMode, "secret-test-"+string(uuid.NewUUID()), &fsGroup, &nonRootTestUserID)
	})

	/*
		Release: v1.9
		Testname: Secrets Volume, mapping
		Description: Create a secret. Create a Pod with secret volume source configured into the container with a custom path. Pod MUST be able to read the secret from the mounted volume from the specified custom path. The file mode of the secret MUST be -rw-r--r-- by default.
	*/
	framework.ConformanceIt("should be consumable from pods in volume with mappings", f.WithNodeConformance(), func(ctx context.Context) {
		doSecretE2EWithMapping(ctx, f, nil)
	})

	/*
		Release: v1.9
		Testname: Secrets Volume, mapping, volume mode 0400
		Description: Create a secret. Create a Pod with secret volume source configured into the container with a custom path and file mode set to 0x400. Pod MUST be able to read the secret from the mounted volume from the specified custom path. The file mode of the secret MUST be -r--r--r--.
	*/
	framework.ConformanceIt("should be consumable from pods in volume with mappings and Item Mode set", f.WithNodeConformance(), func(ctx context.Context) {
		mode := int32(0400)
		doSecretE2EWithMapping(ctx, f, &mode)
	})

	/*
		Release: v1.12
		Testname: Secrets Volume, volume mode default, secret with same name in different namespace
		Description: Create a secret with same name in two namespaces. Create a Pod with secret volume source configured into the container. Pod MUST be able to read the secrets from the mounted volume from the container runtime and only secrets which are associated with namespace where pod is created. The file mode of the secret MUST be -rw-r--r-- by default.
	*/
	framework.ConformanceIt("should be able to mount in a volume regardless of a different secret existing with same name in different namespace", f.WithNodeConformance(), func(ctx context.Context) {
		var (
			namespace2  *v1.Namespace
			err         error
			secret2Name = "secret-test-" + string(uuid.NewUUID())
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
		doSecretE2EWithoutMapping(ctx, f, nil /* default mode */, secret2.Name, nil, nil)
	})

	/*
		Release: v1.9
		Testname: Secrets Volume, mapping multiple volume paths
		Description: Create a secret. Create a Pod with two secret volume sources configured into the container in to two different custom paths. Pod MUST be able to read the secret from the both the mounted volumes from the two specified custom paths.
	*/
	framework.ConformanceIt("should be consumable in multiple volumes in a pod", f.WithNodeConformance(), func(ctx context.Context) {
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

		ginkgo.By(fmt.Sprintf("Creating secret with name %s", secret.Name))
		var err error
		if secret, err = f.ClientSet.CoreV1().Secrets(f.Namespace.Name).Create(ctx, secret, metav1.CreateOptions{}); err != nil {
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
						Image: imageutils.GetE2EImage(imageutils.Agnhost),
						Args: []string{
							"mounttest",
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

		fileModeRegexp := getFileModeRegex("/etc/secret-volume/data-1", nil)
		e2epodoutput.TestContainerOutputRegexp(ctx, f, "consume secrets", pod, 0, []string{
			"content of file \"/etc/secret-volume/data-1\": value-1",
			fileModeRegexp,
		})
	})

	/*
		Release: v1.9
		Testname: Secrets Volume, create, update and delete
		Description: Create a Pod with three containers with secrets volume sources namely a create, update and delete container. Create Container when started MUST not have secret, update and delete containers MUST be created with a secret value. Create a secret in the create container, the Pod MUST be able to read the secret from the create container. Update the secret in the update container, Pod MUST be able to read the updated secret value. Delete the secret in the delete container. Pod MUST fail to read the secret from the delete container.
	*/
	framework.ConformanceIt("optional updates should be reflected in volume", f.WithNodeConformance(), func(ctx context.Context) {
		podLogTimeout := e2epod.GetPodSecretUpdateTimeout(ctx, f.ClientSet)
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
						Name:  deleteContainerName,
						Image: imageutils.GetE2EImage(imageutils.Agnhost),
						Args:  []string{"mounttest", "--break_on_expected_content=false", containerTimeoutArg, "--file_content_in_loop=/etc/secret-volumes/delete/data-1"},
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
						Args:  []string{"mounttest", "--break_on_expected_content=false", containerTimeoutArg, "--file_content_in_loop=/etc/secret-volumes/update/data-3"},
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
						Args:  []string{"mounttest", "--break_on_expected_content=false", containerTimeoutArg, "--file_content_in_loop=/etc/secret-volumes/create/data-1"},
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
		gomega.Eventually(ctx, pollCreateLogs, podLogTimeout, framework.Poll).Should(gomega.ContainSubstring("Error reading file /etc/secret-volumes/create/data-1"))

		pollUpdateLogs := func() (string, error) {
			return e2epod.GetPodLogs(ctx, f.ClientSet, f.Namespace.Name, pod.Name, updateContainerName)
		}
		gomega.Eventually(ctx, pollUpdateLogs, podLogTimeout, framework.Poll).Should(gomega.ContainSubstring("Error reading file /etc/secret-volumes/update/data-3"))

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
		gomega.Eventually(ctx, pollDeleteLogs, podLogTimeout, framework.Poll).Should(gomega.ContainSubstring("Error reading file /etc/secret-volumes/delete/data-1"))
	})

	/*
		Release: v1.21
		Testname: Secrets Volume, immutability
		Description: Create a secret. Update it's data field, the update MUST succeed.
			Mark the secret as immutable, the update MUST succeed. Try to update its data, the update MUST fail.
			Try to mark the secret back as not immutable, the update MUST fail.
			Try to update the secret`s metadata (labels), the update must succeed.
			Try to delete the secret, the deletion must succeed.
	*/
	framework.ConformanceIt("should be immutable if `immutable` field is set", func(ctx context.Context) {
		name := "immutable"
		secret := secretForTest(f.Namespace.Name, name)

		currentSecret, err := f.ClientSet.CoreV1().Secrets(f.Namespace.Name).Create(ctx, secret, metav1.CreateOptions{})
		framework.ExpectNoError(err, "Failed to create secret %q in namespace %q", secret.Name, secret.Namespace)

		currentSecret.Data["data-4"] = []byte("value-4\n")
		currentSecret, err = f.ClientSet.CoreV1().Secrets(f.Namespace.Name).Update(ctx, currentSecret, metav1.UpdateOptions{})
		framework.ExpectNoError(err, "Failed to update secret %q in namespace %q", secret.Name, secret.Namespace)

		// Mark secret as immutable.
		trueVal := true
		currentSecret.Immutable = &trueVal
		currentSecret, err = f.ClientSet.CoreV1().Secrets(f.Namespace.Name).Update(ctx, currentSecret, metav1.UpdateOptions{})
		framework.ExpectNoError(err, "Failed to mark secret %q in namespace %q as immutable", secret.Name, secret.Namespace)

		// Ensure data can't be changed now.
		currentSecret.Data["data-5"] = []byte("value-5\n")
		_, err = f.ClientSet.CoreV1().Secrets(f.Namespace.Name).Update(ctx, currentSecret, metav1.UpdateOptions{})
		if !apierrors.IsInvalid(err) {
			framework.Failf("expected 'invalid' as error, got instead: %v", err)
		}

		// Ensure secret can't be switched from immutable to mutable.
		currentSecret, err = f.ClientSet.CoreV1().Secrets(f.Namespace.Name).Get(ctx, name, metav1.GetOptions{})
		framework.ExpectNoError(err, "Failed to get secret %q in namespace %q", secret.Name, secret.Namespace)
		if !*currentSecret.Immutable {
			framework.Failf("currentSecret %s can be switched from immutable to mutable", currentSecret.Name)
		}

		falseVal := false
		currentSecret.Immutable = &falseVal
		_, err = f.ClientSet.CoreV1().Secrets(f.Namespace.Name).Update(ctx, currentSecret, metav1.UpdateOptions{})
		if !apierrors.IsInvalid(err) {
			framework.Failf("expected 'invalid' as error, got instead: %v", err)
		}

		// Ensure that metadata can be changed.
		currentSecret, err = f.ClientSet.CoreV1().Secrets(f.Namespace.Name).Get(ctx, name, metav1.GetOptions{})
		framework.ExpectNoError(err, "Failed to get secret %q in namespace %q", secret.Name, secret.Namespace)
		currentSecret.Labels = map[string]string{"label1": "value1"}
		_, err = f.ClientSet.CoreV1().Secrets(f.Namespace.Name).Update(ctx, currentSecret, metav1.UpdateOptions{})
		framework.ExpectNoError(err, "Failed to update secret %q in namespace %q", secret.Name, secret.Namespace)

		// Ensure that immutable secret can be deleted.
		err = f.ClientSet.CoreV1().Secrets(f.Namespace.Name).Delete(ctx, name, metav1.DeleteOptions{})
		framework.ExpectNoError(err, "Failed to delete secret %q in namespace %q", secret.Name, secret.Namespace)
	})

	// The secret is in pending during volume creation until the secret objects are available
	// or until mount the secret volume times out. There is no secret object defined for the pod, so it should return timeout exception unless it is marked optional.
	// Slow (~5 mins)
	f.It("Should fail non-optional pod creation due to secret object does not exist", f.WithSlow(), func(ctx context.Context) {
		volumeMountPath := "/etc/secret-volumes"
		podName := "pod-secrets-" + string(uuid.NewUUID())
		pod := createNonOptionalSecretPod(ctx, f, volumeMountPath, podName)
		getPod := e2epod.Get(f.ClientSet, pod)
		gomega.Consistently(ctx, getPod).WithTimeout(f.Timeouts.PodStart).Should(e2epod.BeInPhase(v1.PodPending))
	})

	// Secret object defined for the pod, If a key is specified which is not present in the secret,
	// the volume setup will error unless it is marked optional, during the pod creation.
	// Slow (~5 mins)
	f.It("Should fail non-optional pod creation due to the key in the secret object does not exist", f.WithSlow(), func(ctx context.Context) {
		volumeMountPath := "/etc/secret-volumes"
		podName := "pod-secrets-" + string(uuid.NewUUID())
		pod := createNonOptionalSecretPodWithSecret(ctx, f, volumeMountPath, podName)
		getPod := e2epod.Get(f.ClientSet, pod)
		gomega.Consistently(ctx, getPod).WithTimeout(f.Timeouts.PodStart).Should(e2epod.BeInPhase(v1.PodPending))
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

func doSecretE2EWithoutMapping(ctx context.Context, f *framework.Framework, defaultMode *int32, secretName string,
	fsGroup *int64, uid *int64) {
	var (
		volumeName      = "secret-volume"
		volumeMountPath = "/etc/secret-volume"
		secret          = secretForTest(f.Namespace.Name, secretName)
	)

	ginkgo.By(fmt.Sprintf("Creating secret with name %s", secret.Name))
	var err error
	if secret, err = f.ClientSet.CoreV1().Secrets(f.Namespace.Name).Create(ctx, secret, metav1.CreateOptions{}); err != nil {
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
					Image: imageutils.GetE2EImage(imageutils.Agnhost),
					Args: []string{
						"mounttest",
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
	}

	if fsGroup != nil || uid != nil {
		pod.Spec.SecurityContext = &v1.PodSecurityContext{
			FSGroup:   fsGroup,
			RunAsUser: uid,
		}
	}

	fileModeRegexp := getFileModeRegex("/etc/secret-volume/data-1", defaultMode)
	expectedOutput := []string{
		"content of file \"/etc/secret-volume/data-1\": value-1",
		fileModeRegexp,
	}

	e2epodoutput.TestContainerOutputRegexp(ctx, f, "consume secrets", pod, 0, expectedOutput)
}

func doSecretE2EWithMapping(ctx context.Context, f *framework.Framework, mode *int32) {
	var (
		name            = "secret-test-map-" + string(uuid.NewUUID())
		volumeName      = "secret-volume"
		volumeMountPath = "/etc/secret-volume"
		secret          = secretForTest(f.Namespace.Name, name)
	)

	ginkgo.By(fmt.Sprintf("Creating secret with name %s", secret.Name))
	var err error
	if secret, err = f.ClientSet.CoreV1().Secrets(f.Namespace.Name).Create(ctx, secret, metav1.CreateOptions{}); err != nil {
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
					Image: imageutils.GetE2EImage(imageutils.Agnhost),
					Args: []string{
						"mounttest",
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
	}

	fileModeRegexp := getFileModeRegex("/etc/secret-volume/new-path-data-1", mode)
	expectedOutput := []string{
		"content of file \"/etc/secret-volume/new-path-data-1\": value-1",
		fileModeRegexp,
	}

	e2epodoutput.TestContainerOutputRegexp(ctx, f, "consume secrets", pod, 0, expectedOutput)
}

func createNonOptionalSecretPod(ctx context.Context, f *framework.Framework, volumeMountPath, podName string) *v1.Pod {
	podLogTimeout := e2epod.GetPodSecretUpdateTimeout(ctx, f.ClientSet)
	containerTimeoutArg := fmt.Sprintf("--retry_time=%v", int(podLogTimeout.Seconds()))
	falseValue := false

	createName := "s-test-opt-create-" + string(uuid.NewUUID())
	createContainerName := "creates-volume-test"
	createVolumeName := "creates-volume"

	// creating a pod without secret object created, by mentioning the secret volume source reference name
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: podName,
		},
		Spec: v1.PodSpec{
			Volumes: []v1.Volume{
				{
					Name: createVolumeName,
					VolumeSource: v1.VolumeSource{
						Secret: &v1.SecretVolumeSource{
							SecretName: createName,
							Optional:   &falseValue,
						},
					},
				},
			},
			Containers: []v1.Container{
				{
					Name:  createContainerName,
					Image: imageutils.GetE2EImage(imageutils.Agnhost),
					Args:  []string{"mounttest", "--break_on_expected_content=false", containerTimeoutArg, "--file_content_in_loop=/etc/secret-volumes/create/data-1"},
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
	pod = e2epod.NewPodClient(f).Create(ctx, pod)
	return pod
}

func createNonOptionalSecretPodWithSecret(ctx context.Context, f *framework.Framework, volumeMountPath, podName string) *v1.Pod {
	podLogTimeout := e2epod.GetPodSecretUpdateTimeout(ctx, f.ClientSet)
	containerTimeoutArg := fmt.Sprintf("--retry_time=%v", int(podLogTimeout.Seconds()))
	falseValue := false

	createName := "s-test-opt-create-" + string(uuid.NewUUID())
	createContainerName := "creates-volume-test"
	createVolumeName := "creates-volume"

	secret := secretForTest(f.Namespace.Name, createName)

	ginkgo.By(fmt.Sprintf("Creating secret with name %s", secret.Name))
	var err error
	if secret, err = f.ClientSet.CoreV1().Secrets(f.Namespace.Name).Create(ctx, secret, metav1.CreateOptions{}); err != nil {
		framework.Failf("unable to create test secret %s: %v", secret.Name, err)
	}
	// creating a pod with secret object, with the key which is not present in secret object.
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: podName,
		},
		Spec: v1.PodSpec{
			Volumes: []v1.Volume{
				{
					Name: createVolumeName,
					VolumeSource: v1.VolumeSource{
						Secret: &v1.SecretVolumeSource{
							SecretName: createName,
							Items: []v1.KeyToPath{
								{
									Key:  "data_4",
									Path: "value-4\n",
								},
							},
							Optional: &falseValue,
						},
					},
				},
			},
			Containers: []v1.Container{
				{
					Name:  createContainerName,
					Image: imageutils.GetE2EImage(imageutils.Agnhost),
					Args:  []string{"mounttest", "--break_on_expected_content=false", containerTimeoutArg, "--file_content_in_loop=/etc/secret-volumes/create/data-1"},
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
	pod = e2epod.NewPodClient(f).Create(ctx, pod)
	return pod
}
