/*
Copyright 2015 The Kubernetes Authors.

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

/* This test check that SecurityContext parameters specified at the
 * pod or the container level work as intended. These tests cannot be
 * run when the 'SecurityContextDeny' admission controller is not used
 * so they are skipped by default.
 */

package node

import (
	"context"
	"fmt"
	"reflect"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/kubernetes/pkg/features"
	"k8s.io/kubernetes/test/e2e/feature"
	"k8s.io/kubernetes/test/e2e/framework"
	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2eoutput "k8s.io/kubernetes/test/e2e/framework/pod/output"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
	imageutils "k8s.io/kubernetes/test/utils/image"
	admissionapi "k8s.io/pod-security-admission/api"
	ptr "k8s.io/utils/ptr"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"github.com/onsi/gomega/gcustom"
)

// SeccompProcStatusField is the field of /proc/$PID/status referencing the seccomp filter type.
const SeccompProcStatusField = "Seccomp:"

// ProcSelfStatusPath is the path to /proc/self/status.
const ProcSelfStatusPath = "/proc/self/status"

func scTestPod(hostIPC bool, hostPID bool) *v1.Pod {
	podName := "security-context-" + string(uuid.NewUUID())
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        podName,
			Labels:      map[string]string{"name": podName},
			Annotations: map[string]string{},
		},
		Spec: v1.PodSpec{
			HostIPC:         hostIPC,
			HostPID:         hostPID,
			SecurityContext: &v1.PodSecurityContext{},
			Containers: []v1.Container{
				{
					Name:  "test-container",
					Image: imageutils.GetE2EImage(imageutils.BusyBox),
				},
			},
			RestartPolicy: v1.RestartPolicyNever,
		},
	}

	return pod
}

var _ = SIGDescribe("Security Context", func() {
	f := framework.NewDefaultFramework("security-context")
	f.NamespacePodSecurityLevel = admissionapi.LevelPrivileged

	ginkgo.It("should support pod.Spec.SecurityContext.SupplementalGroups [LinuxOnly]", func(ctx context.Context) {
		pod := scTestPod(false, false)
		pod.Spec.Containers[0].Command = []string{"id", "-G"}
		pod.Spec.SecurityContext.SupplementalGroups = []int64{1234, 5678}
		groups := []string{"1234", "5678"}
		e2eoutput.TestContainerOutput(ctx, f, "pod.Spec.SecurityContext.SupplementalGroups", pod, 0, groups)
	})

	ginkgo.When("if the container's primary UID belongs to some groups in the image [LinuxOnly]", func() {
		ginkgo.It("should add pod.Spec.SecurityContext.SupplementalGroups to them [LinuxOnly] in resultant supplementary groups for the container processes", func(ctx context.Context) {
			uidInImage := int64(1000)
			gidDefinedInImage := int64(50000)
			supplementalGroup := int64(60000)
			agnhost := imageutils.GetConfig(imageutils.Agnhost)
			pod := scTestPod(false, false)
			pod.Spec.Containers[0].Image = agnhost.GetE2EImage()
			pod.Spec.Containers[0].Command = []string{"id", "-G"}
			pod.Spec.SecurityContext.SupplementalGroups = []int64{int64(supplementalGroup)}
			pod.Spec.SecurityContext.RunAsUser = &uidInImage

			// In specified image(agnhost E2E image),
			// - user-defined-in-image(uid=1000) is defined
			// - user-defined-in-image belongs to group-defined-in-image(gid=50000)
			// thus, resultant supplementary group of the container processes should be
			// - 1000: self
			// - 50000: pre-defined groups define in the container image of self(uid=1000)
			// - 60000: SupplementalGroups
			// $ id -G
			// 1000 50000 60000
			e2eoutput.TestContainerOutput(
				ctx,
				f,
				"pod.Spec.SecurityContext.SupplementalGroups with pre-defined-group in the image",
				pod, 0,
				[]string{fmt.Sprintf("%d %d %d", uidInImage, gidDefinedInImage, supplementalGroup)},
			)
		})
	})

	SIGDescribe("SupplementalGroupsPolicy", feature.SupplementalGroupsPolicy, framework.WithFeatureGate(features.SupplementalGroupsPolicy), func() {
		timeout := 3 * time.Minute

		agnhostImage := imageutils.GetE2EImage(imageutils.Agnhost)
		uidInImage := int64(1000)
		gidDefinedInImage := int64(50000)
		supplementalGroup := int64(60000)

		supportsSupplementalGroupsPolicy := func(ctx context.Context, f *framework.Framework, nodeName string) bool {
			node, err := f.ClientSet.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
			framework.ExpectNoError(err)
			gomega.Expect(node).NotTo(gomega.BeNil())
			if node.Status.Features != nil {
				supportsSupplementalGroupsPolicy := node.Status.Features.SupplementalGroupsPolicy
				if supportsSupplementalGroupsPolicy != nil && *supportsSupplementalGroupsPolicy {
					return true
				}
			}
			return false
		}
		mkPod := func(policy *v1.SupplementalGroupsPolicy) *v1.Pod {
			pod := scTestPod(false, false)

			// In specified image(agnhost E2E image),
			// - user-defined-in-image(uid=1000) is defined
			// - user-defined-in-image belongs to group-defined-in-image(gid=50000)
			// thus, resultant supplementary group of the container processes should be
			// - 1000 : self
			// - 50000: pre-defined groups defined in the container image(/etc/group) of self(uid=1000)
			// - 60000: specified in SupplementalGroups
			// $ id -G
			// 1000 50000 60000 (if SupplementalGroupsPolicy=Merge or not set)
			// 1000 60000       (if SupplementalGroupsPolicy=Strict)
			pod.Spec.SecurityContext.RunAsUser = &uidInImage
			pod.Spec.SecurityContext.SupplementalGroupsPolicy = policy
			pod.Spec.SecurityContext.SupplementalGroups = []int64{supplementalGroup}
			pod.Spec.Containers[0].Image = agnhostImage
			pod.Spec.Containers[0].Command = []string{"sh", "-c", "id -G; while :; do sleep 1; done"}

			return pod
		}
		waitForContainerUser := func(ctx context.Context, f *framework.Framework, podName string, containerName string, expectedContainerUser *v1.ContainerUser) error {
			return framework.Gomega().Eventually(ctx,
				framework.RetryNotFound(framework.GetObject(f.ClientSet.CoreV1().Pods(f.Namespace.Name).Get, podName, metav1.GetOptions{}))).
				WithTimeout(timeout).
				Should(gcustom.MakeMatcher(func(p *v1.Pod) (bool, error) {
					for _, s := range p.Status.ContainerStatuses {
						if s.Name == containerName {
							return reflect.DeepEqual(s.User, expectedContainerUser), nil
						}
					}
					return false, nil
				}))
		}
		waitForPodLogs := func(ctx context.Context, f *framework.Framework, podName string, containerName string, expectedLog string) error {
			return framework.Gomega().Eventually(ctx,
				framework.RetryNotFound(framework.GetObject(f.ClientSet.CoreV1().Pods(f.Namespace.Name).Get, podName, metav1.GetOptions{}))).
				WithTimeout(timeout).
				Should(gcustom.MakeMatcher(func(p *v1.Pod) (bool, error) {
					podLogs, err := e2epod.GetPodLogs(ctx, f.ClientSet, p.Namespace, p.Name, containerName)
					if err != nil {
						return false, err
					}
					return podLogs == expectedLog, nil
				}))
		}

		ginkgo.When("SupplementalGroupsPolicy was not set", func() {
			ginkgo.It("if the container's primary UID belongs to some groups in the image, it should add SupplementalGroups to them [LinuxOnly]", func(ctx context.Context) {
				var pod *v1.Pod
				ginkgo.By("creating a pod", func() {
					pod = e2epod.NewPodClient(f).Create(ctx, mkPod(nil))
					framework.ExpectNoError(e2epod.WaitForPodScheduled(ctx, f.ClientSet, pod.Namespace, pod.Name))
					var err error
					pod, err = e2epod.NewPodClient(f).Get(ctx, pod.Name, metav1.GetOptions{})
					framework.ExpectNoError(err)
					if !supportsSupplementalGroupsPolicy(ctx, f, pod.Spec.NodeName) {
						e2eskipper.Skipf("node does not support SupplementalGroupsPolicy")
					}
					framework.ExpectNoError(e2epod.WaitForPodRunningInNamespace(ctx, f.ClientSet, pod))
				})
				expectedOutput := fmt.Sprintf("%d %d %d", uidInImage, gidDefinedInImage, supplementalGroup)
				expectedContainerUser := &v1.ContainerUser{
					Linux: &v1.LinuxContainerUser{
						UID:                uidInImage,
						GID:                uidInImage,
						SupplementalGroups: []int64{uidInImage, gidDefinedInImage, supplementalGroup},
					},
				}

				framework.ExpectNoError(waitForContainerUser(ctx, f, pod.Name, pod.Spec.Containers[0].Name, expectedContainerUser))
				framework.ExpectNoError(waitForPodLogs(ctx, f, pod.Name, pod.Spec.Containers[0].Name, expectedOutput+"\n"))

				stdout := e2epod.ExecCommandInContainer(f, pod.Name, pod.Spec.Containers[0].Name, "id", "-G")
				gomega.Expect(stdout).To(gomega.Equal(expectedOutput))
			})
		})
		ginkgo.When("SupplementalGroupsPolicy was set to Merge", func() {
			ginkgo.It("if the container's primary UID belongs to some groups in the image, it should add SupplementalGroups to them [LinuxOnly]", func(ctx context.Context) {
				var pod *v1.Pod
				ginkgo.By("creating a pod", func() {
					pod = e2epod.NewPodClient(f).Create(ctx, mkPod(ptr.To(v1.SupplementalGroupsPolicyMerge)))
					framework.ExpectNoError(e2epod.WaitForPodScheduled(ctx, f.ClientSet, pod.Namespace, pod.Name))
					var err error
					pod, err = e2epod.NewPodClient(f).Get(ctx, pod.Name, metav1.GetOptions{})
					framework.ExpectNoError(err)
					if !supportsSupplementalGroupsPolicy(ctx, f, pod.Spec.NodeName) {
						e2eskipper.Skipf("node does not support SupplementalGroupsPolicy")
					}
					framework.ExpectNoError(e2epod.WaitForPodRunningInNamespace(ctx, f.ClientSet, pod))
				})

				expectedOutput := fmt.Sprintf("%d %d %d", uidInImage, gidDefinedInImage, supplementalGroup)
				expectedContainerUser := &v1.ContainerUser{
					Linux: &v1.LinuxContainerUser{
						UID:                uidInImage,
						GID:                uidInImage,
						SupplementalGroups: []int64{uidInImage, gidDefinedInImage, supplementalGroup},
					},
				}

				framework.ExpectNoError(waitForContainerUser(ctx, f, pod.Name, pod.Spec.Containers[0].Name, expectedContainerUser))
				framework.ExpectNoError(waitForPodLogs(ctx, f, pod.Name, pod.Spec.Containers[0].Name, expectedOutput+"\n"))

				stdout := e2epod.ExecCommandInContainer(f, pod.Name, pod.Spec.Containers[0].Name, "id", "-G")
				gomega.Expect(stdout).To(gomega.Equal(expectedOutput))
			})
		})
		ginkgo.When("SupplementalGroupsPolicy was set to Strict", func() {
			ginkgo.It("even if the container's primary UID belongs to some groups in the image, it should not add SupplementalGroups to them [LinuxOnly]", func(ctx context.Context) {
				var pod *v1.Pod
				ginkgo.By("creating a pod", func() {
					pod = e2epod.NewPodClient(f).Create(ctx, mkPod(ptr.To(v1.SupplementalGroupsPolicyStrict)))
					framework.ExpectNoError(e2epod.WaitForPodScheduled(ctx, f.ClientSet, pod.Namespace, pod.Name))
					var err error
					pod, err = e2epod.NewPodClient(f).Get(ctx, pod.Name, metav1.GetOptions{})
					framework.ExpectNoError(err)
					if !supportsSupplementalGroupsPolicy(ctx, f, pod.Spec.NodeName) {
						e2eskipper.Skipf("node does not support SupplementalGroupsPolicy")
					}
					framework.ExpectNoError(e2epod.WaitForPodRunningInNamespace(ctx, f.ClientSet, pod))
				})

				expectedOutput := fmt.Sprintf("%d %d", uidInImage, supplementalGroup)
				expectedContainerUser := &v1.ContainerUser{
					Linux: &v1.LinuxContainerUser{
						UID:                uidInImage,
						GID:                uidInImage,
						SupplementalGroups: []int64{uidInImage, supplementalGroup},
					},
				}

				framework.ExpectNoError(waitForContainerUser(ctx, f, pod.Name, pod.Spec.Containers[0].Name, expectedContainerUser))
				framework.ExpectNoError(waitForPodLogs(ctx, f, pod.Name, pod.Spec.Containers[0].Name, expectedOutput+"\n"))

				stdout := e2epod.ExecCommandInContainer(f, pod.Name, pod.Spec.Containers[0].Name, "id", "-G")
				gomega.Expect(stdout).To(gomega.Equal(expectedOutput))
			})
		})
	})

	ginkgo.It("should support pod.Spec.SecurityContext.RunAsUser [LinuxOnly]", func(ctx context.Context) {
		pod := scTestPod(false, false)
		userID := int64(1001)
		pod.Spec.SecurityContext.RunAsUser = &userID
		pod.Spec.Containers[0].Command = []string{"sh", "-c", "id"}

		e2eoutput.TestContainerOutput(ctx, f, "pod.Spec.SecurityContext.RunAsUser", pod, 0, []string{
			fmt.Sprintf("uid=%v", userID),
			fmt.Sprintf("gid=%v", 0),
		})
	})

	/*
		Release: v1.21
		Testname: Security Context, test RunAsGroup at pod level
		Description: Container is created with runAsUser and runAsGroup option by passing uid 1001 and gid 2002 at pod level. Pod MUST be in Succeeded phase.
		[LinuxOnly]: This test is marked as LinuxOnly since Windows does not support running as UID / GID.
	*/
	framework.ConformanceIt("should support pod.Spec.SecurityContext.RunAsUser And pod.Spec.SecurityContext.RunAsGroup [LinuxOnly]", func(ctx context.Context) {
		pod := scTestPod(false, false)
		userID := int64(1001)
		groupID := int64(2002)
		pod.Spec.SecurityContext.RunAsUser = &userID
		pod.Spec.SecurityContext.RunAsGroup = &groupID
		pod.Spec.Containers[0].Command = []string{"sh", "-c", "id"}

		e2eoutput.TestContainerOutput(ctx, f, "pod.Spec.SecurityContext.RunAsUser", pod, 0, []string{
			fmt.Sprintf("uid=%v", userID),
			fmt.Sprintf("gid=%v", groupID),
		})
	})

	ginkgo.It("should support container.SecurityContext.RunAsUser [LinuxOnly]", func(ctx context.Context) {
		pod := scTestPod(false, false)
		userID := int64(1001)
		overrideUserID := int64(1002)
		pod.Spec.SecurityContext.RunAsUser = &userID
		pod.Spec.Containers[0].SecurityContext = new(v1.SecurityContext)
		pod.Spec.Containers[0].SecurityContext.RunAsUser = &overrideUserID
		pod.Spec.Containers[0].Command = []string{"sh", "-c", "id"}

		e2eoutput.TestContainerOutput(ctx, f, "pod.Spec.SecurityContext.RunAsUser", pod, 0, []string{
			fmt.Sprintf("uid=%v", overrideUserID),
			fmt.Sprintf("gid=%v", 0),
		})
	})

	/*
		Release: v1.21
		Testname: Security Context, test RunAsGroup at container level
		Description: Container is created with runAsUser and runAsGroup option by passing uid 1001 and gid 2002 at containr level. Pod MUST be in Succeeded phase.
		[LinuxOnly]: This test is marked as LinuxOnly since Windows does not support running as UID / GID.
	*/
	framework.ConformanceIt("should support container.SecurityContext.RunAsUser And container.SecurityContext.RunAsGroup [LinuxOnly]", func(ctx context.Context) {
		pod := scTestPod(false, false)
		userID := int64(1001)
		groupID := int64(2001)
		overrideUserID := int64(1002)
		overrideGroupID := int64(2002)
		pod.Spec.SecurityContext.RunAsUser = &userID
		pod.Spec.SecurityContext.RunAsGroup = &groupID
		pod.Spec.Containers[0].SecurityContext = new(v1.SecurityContext)
		pod.Spec.Containers[0].SecurityContext.RunAsUser = &overrideUserID
		pod.Spec.Containers[0].SecurityContext.RunAsGroup = &overrideGroupID
		pod.Spec.Containers[0].Command = []string{"sh", "-c", "id"}

		e2eoutput.TestContainerOutput(ctx, f, "pod.Spec.SecurityContext.RunAsUser", pod, 0, []string{
			fmt.Sprintf("uid=%v", overrideUserID),
			fmt.Sprintf("gid=%v", overrideGroupID),
		})
	})

	f.It("should support volume SELinux relabeling", f.WithFlaky(), f.WithLabel("LinuxOnly"), func(ctx context.Context) {
		testPodSELinuxLabeling(ctx, f, false, false)
	})

	f.It("should support volume SELinux relabeling when using hostIPC", f.WithFlaky(), f.WithLabel("LinuxOnly"), func(ctx context.Context) {
		testPodSELinuxLabeling(ctx, f, true, false)
	})

	f.It("should support volume SELinux relabeling when using hostPID", f.WithFlaky(), f.WithLabel("LinuxOnly"), func(ctx context.Context) {
		testPodSELinuxLabeling(ctx, f, false, true)
	})

	ginkgo.It("should support seccomp unconfined on the container [LinuxOnly]", func(ctx context.Context) {
		pod := scTestPod(false, false)
		pod.Spec.Containers[0].SecurityContext = &v1.SecurityContext{SeccompProfile: &v1.SeccompProfile{Type: v1.SeccompProfileTypeUnconfined}}
		pod.Spec.SecurityContext = &v1.PodSecurityContext{SeccompProfile: &v1.SeccompProfile{Type: v1.SeccompProfileTypeRuntimeDefault}}
		pod.Spec.Containers[0].Command = []string{"grep", SeccompProcStatusField, ProcSelfStatusPath}
		e2eoutput.TestContainerOutput(ctx, f, "seccomp unconfined container", pod, 0, []string{"0"}) // seccomp disabled
	})

	ginkgo.It("should support seccomp unconfined on the pod [LinuxOnly]", func(ctx context.Context) {
		pod := scTestPod(false, false)
		pod.Spec.SecurityContext = &v1.PodSecurityContext{SeccompProfile: &v1.SeccompProfile{Type: v1.SeccompProfileTypeUnconfined}}
		pod.Spec.Containers[0].Command = []string{"grep", SeccompProcStatusField, ProcSelfStatusPath}
		e2eoutput.TestContainerOutput(ctx, f, "seccomp unconfined pod", pod, 0, []string{"0"}) // seccomp disabled
	})

	ginkgo.It("should support seccomp runtime/default [LinuxOnly]", func(ctx context.Context) {
		pod := scTestPod(false, false)
		pod.Spec.Containers[0].SecurityContext = &v1.SecurityContext{SeccompProfile: &v1.SeccompProfile{Type: v1.SeccompProfileTypeRuntimeDefault}}
		pod.Spec.Containers[0].Command = []string{"grep", SeccompProcStatusField, ProcSelfStatusPath}
		e2eoutput.TestContainerOutput(ctx, f, "seccomp runtime/default", pod, 0, []string{"2"}) // seccomp filtered
	})

	ginkgo.It("should support seccomp default which is unconfined [LinuxOnly]", func(ctx context.Context) {
		pod := scTestPod(false, false)
		pod.Spec.Containers[0].Command = []string{"grep", SeccompProcStatusField, ProcSelfStatusPath}
		e2eoutput.TestContainerOutput(ctx, f, "seccomp default unconfined", pod, 0, []string{"0"}) // seccomp disabled
	})
})

func testPodSELinuxLabeling(ctx context.Context, f *framework.Framework, hostIPC bool, hostPID bool) {
	// Write and read a file with an empty_dir volume
	// with a pod with the MCS label s0:c0,c1
	pod := scTestPod(hostIPC, hostPID)
	volumeName := "test-volume"
	mountPath := "/mounted_volume"
	pod.Spec.Containers[0].VolumeMounts = []v1.VolumeMount{
		{
			Name:      volumeName,
			MountPath: mountPath,
		},
	}
	pod.Spec.Volumes = []v1.Volume{
		{
			Name: volumeName,
			VolumeSource: v1.VolumeSource{
				EmptyDir: &v1.EmptyDirVolumeSource{
					Medium: v1.StorageMediumDefault,
				},
			},
		},
	}
	pod.Spec.SecurityContext.SELinuxOptions = &v1.SELinuxOptions{
		Level: "s0:c0,c1",
	}
	pod.Spec.Containers[0].Command = []string{"sleep", "6000"}

	client := f.ClientSet.CoreV1().Pods(f.Namespace.Name)
	pod, err := client.Create(ctx, pod, metav1.CreateOptions{})

	framework.ExpectNoError(err, "Error creating pod %v", pod)
	framework.ExpectNoError(e2epod.WaitForPodRunningInNamespace(ctx, f.ClientSet, pod))

	testContent := "hello"
	testFilePath := mountPath + "/TEST"
	tk := e2ekubectl.NewTestKubeconfig(framework.TestContext.CertDir, framework.TestContext.Host, framework.TestContext.KubeConfig, framework.TestContext.KubeContext, framework.TestContext.KubectlPath, f.Namespace.Name)
	err = tk.WriteFileViaContainer(pod.Name, pod.Spec.Containers[0].Name, testFilePath, testContent)
	framework.ExpectNoError(err)
	content, err := tk.ReadFileViaContainer(pod.Name, pod.Spec.Containers[0].Name, testFilePath)
	framework.ExpectNoError(err)
	gomega.Expect(content).To(gomega.ContainSubstring(testContent))

	foundPod, err := f.ClientSet.CoreV1().Pods(f.Namespace.Name).Get(ctx, pod.Name, metav1.GetOptions{})
	framework.ExpectNoError(err)

	// Confirm that the file can be accessed from a second
	// pod using host_path with the same MCS label
	volumeHostPath := fmt.Sprintf("%s/pods/%s/volumes/kubernetes.io~empty-dir/%s", framework.TestContext.KubeletRootDir, foundPod.UID, volumeName)
	ginkgo.By(fmt.Sprintf("confirming a container with the same label can read the file under --kubelet-root-dir=%s", framework.TestContext.KubeletRootDir))
	pod = scTestPod(hostIPC, hostPID)
	pod.Spec.NodeName = foundPod.Spec.NodeName
	volumeMounts := []v1.VolumeMount{
		{
			Name:      volumeName,
			MountPath: mountPath,
		},
	}
	volumes := []v1.Volume{
		{
			Name: volumeName,
			VolumeSource: v1.VolumeSource{
				HostPath: &v1.HostPathVolumeSource{
					Path: volumeHostPath,
				},
			},
		},
	}
	pod.Spec.Containers[0].VolumeMounts = volumeMounts
	pod.Spec.Volumes = volumes
	pod.Spec.Containers[0].Command = []string{"cat", testFilePath}
	pod.Spec.SecurityContext.SELinuxOptions = &v1.SELinuxOptions{
		Level: "s0:c0,c1",
	}
	e2eoutput.TestContainerOutput(ctx, f, "Pod with same MCS label reading test file", pod, 0, []string{testContent})

	// Confirm that the same pod with a different MCS
	// label cannot access the volume
	ginkgo.By("confirming a container with a different MCS label is unable to read the file")
	pod = scTestPod(hostIPC, hostPID)
	pod.Spec.Volumes = volumes
	pod.Spec.Containers[0].VolumeMounts = volumeMounts
	pod.Spec.Containers[0].Command = []string{"sleep", "6000"}
	pod.Spec.SecurityContext.SELinuxOptions = &v1.SELinuxOptions{
		Level: "s0:c2,c3",
	}
	_, err = client.Create(ctx, pod, metav1.CreateOptions{})
	framework.ExpectNoError(err, "Error creating pod %v", pod)

	err = e2epod.WaitForPodNameRunningInNamespace(ctx, f.ClientSet, pod.Name, f.Namespace.Name)
	framework.ExpectNoError(err, "Error waiting for pod to run %v", pod)

	// for this to work, SELinux should be in enforcing mode, so let's check that
	isEnforced, err := tk.ReadFileViaContainer(pod.Name, "test-container", "/sys/fs/selinux/enforce")
	if err == nil && isEnforced == "1" {
		_, err = tk.ReadFileViaContainer(pod.Name, "test-container", testFilePath)
		gomega.Expect(err).To(gomega.HaveOccurred(), "expecting SELinux to not let the container with different MCS label to read the file")
	}
}
