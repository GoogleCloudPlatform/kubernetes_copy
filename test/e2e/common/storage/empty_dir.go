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
)

const (
	volumePath = "/test-volume"
)

var (
	nonRootUID = int64(1001)
)

var _ = SIGDescribe("EmptyDir volumes", func() {
	f := framework.NewDefaultFramework("emptydir")
	f.NamespacePodSecurityLevel = admissionapi.LevelBaseline

	f.Context("when FSGroup is specified [LinuxOnly]", nodefeature.FSGroup, func() {

		ginkgo.BeforeEach(func() {
			// Windows does not support the FSGroup SecurityContext option.
			e2eskipper.SkipIfNodeOSDistroIs("windows")
		})

		ginkgo.It("new files should be created with FSGroup ownership when container is root", func(ctx context.Context) {
			doTestSetgidFSGroup(ctx, f, 0, v1.StorageMediumMemory)
		})

		ginkgo.It("new files should be created with FSGroup ownership when container is non-root", func(ctx context.Context) {
			doTestSetgidFSGroup(ctx, f, nonRootUID, v1.StorageMediumMemory)
		})

		ginkgo.It("nonexistent volume subPath should have the correct mode and owner using FSGroup", func(ctx context.Context) {
			doTestSubPathFSGroup(ctx, f, nonRootUID, v1.StorageMediumMemory)
		})

		ginkgo.It("files with FSGroup ownership should support (root,0644,tmpfs)", func(ctx context.Context) {
			doTest0644FSGroup(ctx, f, 0, v1.StorageMediumMemory)
		})

		ginkgo.It("volume on default medium should have the correct mode using FSGroup", func(ctx context.Context) {
			doTestVolumeModeFSGroup(ctx, f, 0, v1.StorageMediumDefault)
		})

		ginkgo.It("volume on tmpfs should have the correct mode using FSGroup", func(ctx context.Context) {
			doTestVolumeModeFSGroup(ctx, f, 0, v1.StorageMediumMemory)
		})
	})

	/*
		Release: v1.9
		Testname: EmptyDir, medium memory, volume mode default
		Description: A Pod created with an 'emptyDir' Volume and 'medium' as 'Memory', the volume MUST have mode set as -rwxrwxrwx and mount type set to tmpfs.
		This test is marked LinuxOnly since Windows does not support the medium = 'Memory'.
	*/
	framework.ConformanceIt("volume on tmpfs should have the correct mode [LinuxOnly]", f.WithNodeConformance(), func(ctx context.Context) {
		doTestVolumeMode(ctx, f, 0, v1.StorageMediumMemory)
	})

	/*
		Release: v1.9
		Testname: EmptyDir, medium memory, volume mode 0644
		Description: A Pod created with an 'emptyDir' Volume and 'medium' as 'Memory', the volume mode set to 0644. The volume MUST have mode -rw-r--r-- and mount type set to tmpfs and the contents MUST be readable.
		This test is marked LinuxOnly since Windows does not support running as UID / GID, or the medium = 'Memory'.
	*/
	framework.ConformanceIt("should support (root,0644,tmpfs) [LinuxOnly]", f.WithNodeConformance(), func(ctx context.Context) {
		doTest0644(ctx, f, 0, v1.StorageMediumMemory)
	})

	/*
		Release: v1.9
		Testname: EmptyDir, medium memory, volume mode 0666
		Description: A Pod created with an 'emptyDir' Volume and 'medium' as 'Memory', the volume mode set to 0666. The volume MUST have mode -rw-rw-rw- and mount type set to tmpfs and the contents MUST be readable.
		This test is marked LinuxOnly since Windows does not support running as UID / GID, or the medium = 'Memory'.
	*/
	framework.ConformanceIt("should support (root,0666,tmpfs) [LinuxOnly]", f.WithNodeConformance(), func(ctx context.Context) {
		doTest0666(ctx, f, 0, v1.StorageMediumMemory)
	})

	/*
		Release: v1.9
		Testname: EmptyDir, medium memory, volume mode 0777
		Description: A Pod created with an 'emptyDir' Volume and 'medium' as 'Memory', the volume mode set to 0777.  The volume MUST have mode set as -rwxrwxrwx and mount type set to tmpfs and the contents MUST be readable.
		This test is marked LinuxOnly since Windows does not support running as UID / GID, or the medium = 'Memory'.
	*/
	framework.ConformanceIt("should support (root,0777,tmpfs) [LinuxOnly]", f.WithNodeConformance(), func(ctx context.Context) {
		doTest0777(ctx, f, 0, v1.StorageMediumMemory)
	})

	/*
		Release: v1.9
		Testname: EmptyDir, medium memory, volume mode 0644, non-root user
		Description: A Pod created with an 'emptyDir' Volume and 'medium' as 'Memory', the volume mode set to 0644. Volume is mounted into the container where container is run as a non-root user. The volume MUST have mode -rw-r--r-- and mount type set to tmpfs and the contents MUST be readable.
		This test is marked LinuxOnly since Windows does not support running as UID / GID, or the medium = 'Memory'.
	*/
	framework.ConformanceIt("should support (non-root,0644,tmpfs) [LinuxOnly]", f.WithNodeConformance(), func(ctx context.Context) {
		doTest0644(ctx, f, nonRootUID, v1.StorageMediumMemory)
	})

	/*
		Release: v1.9
		Testname: EmptyDir, medium memory, volume mode 0666,, non-root user
		Description: A Pod created with an 'emptyDir' Volume and 'medium' as 'Memory', the volume mode set to 0666. Volume is mounted into the container where container is run as a non-root user. The volume MUST have mode -rw-rw-rw- and mount type set to tmpfs and the contents MUST be readable.
		This test is marked LinuxOnly since Windows does not support running as UID / GID, or the medium = 'Memory'.
	*/
	framework.ConformanceIt("should support (non-root,0666,tmpfs) [LinuxOnly]", f.WithNodeConformance(), func(ctx context.Context) {
		doTest0666(ctx, f, nonRootUID, v1.StorageMediumMemory)
	})

	/*
		Release: v1.9
		Testname: EmptyDir, medium memory, volume mode 0777, non-root user
		Description: A Pod created with an 'emptyDir' Volume and 'medium' as 'Memory', the volume mode set to 0777. Volume is mounted into the container where container is run as a non-root user. The volume MUST have mode -rwxrwxrwx and mount type set to tmpfs and the contents MUST be readable.
		This test is marked LinuxOnly since Windows does not support running as UID / GID, or the medium = 'Memory'.
	*/
	framework.ConformanceIt("should support (non-root,0777,tmpfs) [LinuxOnly]", f.WithNodeConformance(), func(ctx context.Context) {
		doTest0777(ctx, f, nonRootUID, v1.StorageMediumMemory)
	})

	/*
		Release: v1.9
		Testname: EmptyDir, medium default, volume mode default
		Description: A Pod created with an 'emptyDir' Volume, the volume MUST have mode set as -rwxrwxrwx and mount type set to tmpfs.
	*/
	framework.ConformanceIt("volume on default medium should have the correct mode [LinuxOnly]", f.WithNodeConformance(), func(ctx context.Context) {
		doTestVolumeMode(ctx, f, 0, v1.StorageMediumDefault)
	})

	/*
		Release: v1.9
		Testname: EmptyDir, medium default, volume mode 0644
		Description: A Pod created with an 'emptyDir' Volume, the volume mode set to 0644. The volume MUST have mode -rw-r--r-- and mount type set to tmpfs and the contents MUST be readable. This test is marked LinuxOnly since Windows does not support running as UID / GID.
	*/
	framework.ConformanceIt("should support (root,0644,default) [LinuxOnly]", f.WithNodeConformance(), func(ctx context.Context) {
		doTest0644(ctx, f, 0, v1.StorageMediumDefault)
	})

	/*
		Release: v1.9
		Testname: EmptyDir, medium default, volume mode 0666
		Description: A Pod created with an 'emptyDir' Volume, the volume mode set to 0666. The volume MUST have mode -rw-rw-rw- and mount type set to tmpfs and the contents MUST be readable. This test is marked LinuxOnly since Windows does not support running as UID / GID.
	*/
	framework.ConformanceIt("should support (root,0666,default) [LinuxOnly]", f.WithNodeConformance(), func(ctx context.Context) {
		doTest0666(ctx, f, 0, v1.StorageMediumDefault)
	})

	/*
		Release: v1.9
		Testname: EmptyDir, medium default, volume mode 0777
		Description: A Pod created with an 'emptyDir' Volume, the volume mode set to 0777.  The volume MUST have mode set as -rwxrwxrwx and mount type set to tmpfs and the contents MUST be readable. This test is marked LinuxOnly since Windows does not support running as UID / GID.
	*/
	framework.ConformanceIt("should support (root,0777,default) [LinuxOnly]", f.WithNodeConformance(), func(ctx context.Context) {
		doTest0777(ctx, f, 0, v1.StorageMediumDefault)
	})

	/*
		Release: v1.9
		Testname: EmptyDir, medium default, volume mode 0644
		Description: A Pod created with an 'emptyDir' Volume, the volume mode set to 0644. Volume is mounted into the container where container is run as a non-root user. The volume MUST have mode -rw-r--r-- and mount type set to tmpfs and the contents MUST be readable.
		This test is marked LinuxOnly since Windows does not support running as UID / GID.
	*/
	framework.ConformanceIt("should support (non-root,0644,default) [LinuxOnly]", f.WithNodeConformance(), func(ctx context.Context) {
		doTest0644(ctx, f, nonRootUID, v1.StorageMediumDefault)
	})

	/*
		Release: v1.9
		Testname: EmptyDir, medium default, volume mode 0666
		Description: A Pod created with an 'emptyDir' Volume, the volume mode set to 0666. Volume is mounted into the container where container is run as a non-root user. The volume MUST have mode -rw-rw-rw- and mount type set to tmpfs and the contents MUST be readable.
		This test is marked LinuxOnly since Windows does not support running as UID / GID.
	*/
	framework.ConformanceIt("should support (non-root,0666,default) [LinuxOnly]", f.WithNodeConformance(), func(ctx context.Context) {
		doTest0666(ctx, f, nonRootUID, v1.StorageMediumDefault)
	})

	/*
		Release: v1.9
		Testname: EmptyDir, medium default, volume mode 0777
		Description: A Pod created with an 'emptyDir' Volume, the volume mode set to 0777. Volume is mounted into the container where container is run as a non-root user. The volume MUST have mode -rwxrwxrwx and mount type set to tmpfs and the contents MUST be readable.
		This test is marked LinuxOnly since Windows does not support running as UID / GID.
	*/
	framework.ConformanceIt("should support (non-root,0777,default) [LinuxOnly]", f.WithNodeConformance(), func(ctx context.Context) {
		doTest0777(ctx, f, nonRootUID, v1.StorageMediumDefault)
	})

	/*
		Release: v1.15
		Testname: EmptyDir, Shared volumes between containers
		Description: A Pod created with an 'emptyDir' Volume, should share volumes between the containeres in the pod. The two busybox image containers should share the volumes mounted to the pod.
		The main container should wait until the sub container drops a file, and main container access the shared data.
	*/
	framework.ConformanceIt("pod should support shared volumes between containers", func(ctx context.Context) {
		var (
			volumeName                 = "shared-data"
			busyBoxMainVolumeMountPath = "/usr/share/volumeshare"
			busyBoxSubVolumeMountPath  = "/pod-data"
			busyBoxMainVolumeFilePath  = fmt.Sprintf("%s/shareddata.txt", busyBoxMainVolumeMountPath)
			busyBoxSubVolumeFilePath   = fmt.Sprintf("%s/shareddata.txt", busyBoxSubVolumeMountPath)
			message                    = "Hello from the busy-box sub-container"
			busyBoxMainContainerName   = "busybox-main-container"
			busyBoxSubContainerName    = "busybox-sub-container"
			resultString               = ""
			deletionGracePeriod        = int64(0)
		)

		pod := &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: "pod-sharedvolume-" + string(uuid.NewUUID()),
			},
			Spec: v1.PodSpec{
				Volumes: []v1.Volume{
					{
						Name: volumeName,
						VolumeSource: v1.VolumeSource{
							EmptyDir: new(v1.EmptyDirVolumeSource),
						},
					},
				},
				Containers: []v1.Container{
					{
						Name:    busyBoxMainContainerName,
						Image:   imageutils.GetE2EImage(imageutils.BusyBox),
						Command: []string{"/bin/sh"},
						Args:    []string{"-c", e2epod.InfiniteSleepCommand},
						VolumeMounts: []v1.VolumeMount{
							{
								Name:      volumeName,
								MountPath: busyBoxMainVolumeMountPath,
							},
						},
					},
					{
						Name:    busyBoxSubContainerName,
						Image:   imageutils.GetE2EImage(imageutils.BusyBox),
						Command: []string{"/bin/sh"},
						Args:    []string{"-c", fmt.Sprintf("echo %s > %s", message, busyBoxSubVolumeFilePath)},
						VolumeMounts: []v1.VolumeMount{
							{
								Name:      volumeName,
								MountPath: busyBoxSubVolumeMountPath,
							},
						},
					},
				},
				TerminationGracePeriodSeconds: &deletionGracePeriod,
				RestartPolicy:                 v1.RestartPolicyNever,
			},
		}

		ginkgo.By("Creating Pod")
		e2epod.NewPodClient(f).Create(ctx, pod)
		framework.ExpectNoError(e2epod.WaitForPodNameRunningInNamespace(ctx, f.ClientSet, pod.Name, f.Namespace.Name))

		ginkgo.By("Reading file content from the nginx-container")
		result := e2epod.ExecShellInContainer(f, pod.Name, busyBoxMainContainerName, fmt.Sprintf("cat %s", busyBoxMainVolumeFilePath))
		gomega.Expect(result).To(gomega.Equal(message), "failed to match expected string %s with %s", message, resultString)
	})

	/*
		Release: v1.20
		Testname: EmptyDir, Memory backed volume is sized to specified limit
		Description: A Pod created with an 'emptyDir' Volume backed by memory should be sized to user provided value.
	*/
	ginkgo.It("pod should support memory backed volumes of specified size", func(ctx context.Context) {
		var (
			volumeName                 = "shared-data"
			busyBoxMainVolumeMountPath = "/usr/share/volumeshare"
			busyBoxMainContainerName   = "busybox-main-container"
			expectedResult             = "10240" // equal to 10Mi
			deletionGracePeriod        = int64(0)
			sizeLimit                  = resource.MustParse("10Mi")
		)

		pod := &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: "pod-size-memory-volume-" + string(uuid.NewUUID()),
			},
			Spec: v1.PodSpec{
				Volumes: []v1.Volume{
					{
						Name: volumeName,
						VolumeSource: v1.VolumeSource{
							EmptyDir: &v1.EmptyDirVolumeSource{
								Medium:    v1.StorageMediumMemory,
								SizeLimit: &sizeLimit,
							},
						},
					},
				},
				Containers: []v1.Container{
					{
						Name:    busyBoxMainContainerName,
						Image:   imageutils.GetE2EImage(imageutils.BusyBox),
						Command: []string{"/bin/sh"},
						Args:    []string{"-c", e2epod.InfiniteSleepCommand},
						VolumeMounts: []v1.VolumeMount{
							{
								Name:      volumeName,
								MountPath: busyBoxMainVolumeMountPath,
							},
						},
					},
				},
				TerminationGracePeriodSeconds: &deletionGracePeriod,
				RestartPolicy:                 v1.RestartPolicyNever,
			},
		}

		var err error
		ginkgo.By("Creating Pod")
		pod = e2epod.NewPodClient(f).CreateSync(ctx, pod)

		ginkgo.By("Waiting for the pod running")
		err = e2epod.WaitForPodNameRunningInNamespace(ctx, f.ClientSet, pod.Name, f.Namespace.Name)
		framework.ExpectNoError(err, "failed to deploy pod %s", pod.Name)

		ginkgo.By("Getting the pod")
		pod, err = e2epod.NewPodClient(f).Get(ctx, pod.Name, metav1.GetOptions{})
		framework.ExpectNoError(err, "failed to get pod %s", pod.Name)

		ginkgo.By("Reading empty dir size")
		result := e2epod.ExecShellInContainer(f, pod.Name, busyBoxMainContainerName, fmt.Sprintf("df | grep %s | awk '{print $2}'", busyBoxMainVolumeMountPath))
		gomega.Expect(result).To(gomega.Equal(expectedResult), "failed to match expected string %s with %s", expectedResult, result)
	})
})

const (
	containerName = "test-container"
	volumeName    = "test-volume"
)

func doTestSetgidFSGroup(ctx context.Context, f *framework.Framework, uid int64, medium v1.StorageMedium) {
	var (
		filePath = path.Join(volumePath, "test-file")
		source   = &v1.EmptyDirVolumeSource{Medium: medium}
		pod      = testPodWithVolume(uid, volumePath, source)
	)

	pod.Spec.Containers[0].Args = []string{
		"mounttest",
		fmt.Sprintf("--fs_type=%v", volumePath),
		fmt.Sprintf("--new_file_0660=%v", filePath),
		fmt.Sprintf("--file_perm=%v", filePath),
		fmt.Sprintf("--file_owner=%v", filePath),
	}

	fsGroup := int64(123)
	pod.Spec.SecurityContext.FSGroup = &fsGroup

	msg := fmt.Sprintf("emptydir 0644 on %v", formatMedium(medium))
	out := []string{
		"perms of file \"/test-volume/test-file\": -rw-rw----",
		"content of file \"/test-volume/test-file\": mount-tester new file",
		"owner GID of \"/test-volume/test-file\": 123",
	}
	if medium == v1.StorageMediumMemory {
		out = append(out, "mount type of \"/test-volume\": tmpfs")
	}
	e2epodoutput.TestContainerOutput(ctx, f, msg, pod, 0, out)
}

func doTestSubPathFSGroup(ctx context.Context, f *framework.Framework, uid int64, medium v1.StorageMedium) {
	var (
		subPath = "test-sub"
		source  = &v1.EmptyDirVolumeSource{Medium: medium}
		pod     = testPodWithVolume(uid, volumePath, source)
	)

	pod.Spec.Containers[0].Args = []string{
		"mounttest",
		fmt.Sprintf("--fs_type=%v", volumePath),
		fmt.Sprintf("--file_perm=%v", volumePath),
		fmt.Sprintf("--file_owner=%v", volumePath),
		fmt.Sprintf("--file_mode=%v", volumePath),
	}

	pod.Spec.Containers[0].VolumeMounts[0].SubPath = subPath

	fsGroup := int64(123)
	pod.Spec.SecurityContext.FSGroup = &fsGroup

	msg := fmt.Sprintf("emptydir subpath on %v", formatMedium(medium))
	out := []string{
		"perms of file \"/test-volume\": -rwxrwxrwx",
		"owner UID of \"/test-volume\": 0",
		"owner GID of \"/test-volume\": 123",
		"mode of file \"/test-volume\": dgtrwxrwxrwx",
	}
	if medium == v1.StorageMediumMemory {
		out = append(out, "mount type of \"/test-volume\": tmpfs")
	}
	e2epodoutput.TestContainerOutput(ctx, f, msg, pod, 0, out)
}

func doTestVolumeModeFSGroup(ctx context.Context, f *framework.Framework, uid int64, medium v1.StorageMedium) {
	var (
		source = &v1.EmptyDirVolumeSource{Medium: medium}
		pod    = testPodWithVolume(uid, volumePath, source)
	)

	pod.Spec.Containers[0].Args = []string{
		"mounttest",
		fmt.Sprintf("--fs_type=%v", volumePath),
		fmt.Sprintf("--file_perm=%v", volumePath),
	}

	fsGroup := int64(1001)
	pod.Spec.SecurityContext.FSGroup = &fsGroup

	msg := fmt.Sprintf("emptydir volume type on %v", formatMedium(medium))
	out := []string{
		"perms of file \"/test-volume\": -rwxrwxrwx",
	}
	if medium == v1.StorageMediumMemory {
		out = append(out, "mount type of \"/test-volume\": tmpfs")
	}
	e2epodoutput.TestContainerOutput(ctx, f, msg, pod, 0, out)
}

func doTest0644FSGroup(ctx context.Context, f *framework.Framework, uid int64, medium v1.StorageMedium) {
	var (
		filePath = path.Join(volumePath, "test-file")
		source   = &v1.EmptyDirVolumeSource{Medium: medium}
		pod      = testPodWithVolume(uid, volumePath, source)
	)

	pod.Spec.Containers[0].Args = []string{
		"mounttest",
		fmt.Sprintf("--fs_type=%v", volumePath),
		fmt.Sprintf("--new_file_0644=%v", filePath),
		fmt.Sprintf("--file_perm=%v", filePath),
	}

	fsGroup := int64(123)
	pod.Spec.SecurityContext.FSGroup = &fsGroup

	msg := fmt.Sprintf("emptydir 0644 on %v", formatMedium(medium))
	out := []string{
		"perms of file \"/test-volume/test-file\": -rw-r--r--",
		"content of file \"/test-volume/test-file\": mount-tester new file",
	}
	if medium == v1.StorageMediumMemory {
		out = append(out, "mount type of \"/test-volume\": tmpfs")
	}
	e2epodoutput.TestContainerOutput(ctx, f, msg, pod, 0, out)
}

func doTestVolumeMode(ctx context.Context, f *framework.Framework, uid int64, medium v1.StorageMedium) {
	var (
		source = &v1.EmptyDirVolumeSource{Medium: medium}
		pod    = testPodWithVolume(uid, volumePath, source)
	)

	pod.Spec.Containers[0].Args = []string{
		"mounttest",
		fmt.Sprintf("--fs_type=%v", volumePath),
		fmt.Sprintf("--file_perm=%v", volumePath),
	}

	msg := fmt.Sprintf("emptydir volume type on %v", formatMedium(medium))
	out := []string{
		"perms of file \"/test-volume\": -rwxrwxrwx",
	}
	if medium == v1.StorageMediumMemory {
		out = append(out, "mount type of \"/test-volume\": tmpfs")
	}
	e2epodoutput.TestContainerOutput(ctx, f, msg, pod, 0, out)
}

func doTest0644(ctx context.Context, f *framework.Framework, uid int64, medium v1.StorageMedium) {
	var (
		filePath = path.Join(volumePath, "test-file")
		source   = &v1.EmptyDirVolumeSource{Medium: medium}
		pod      = testPodWithVolume(uid, volumePath, source)
	)

	pod.Spec.Containers[0].Args = []string{
		"mounttest",
		fmt.Sprintf("--fs_type=%v", volumePath),
		fmt.Sprintf("--new_file_0644=%v", filePath),
		fmt.Sprintf("--file_perm=%v", filePath),
	}

	msg := fmt.Sprintf("emptydir 0644 on %v", formatMedium(medium))
	out := []string{
		"perms of file \"/test-volume/test-file\": -rw-r--r--",
		"content of file \"/test-volume/test-file\": mount-tester new file",
	}
	if medium == v1.StorageMediumMemory {
		out = append(out, "mount type of \"/test-volume\": tmpfs")
	}
	e2epodoutput.TestContainerOutput(ctx, f, msg, pod, 0, out)
}

func doTest0666(ctx context.Context, f *framework.Framework, uid int64, medium v1.StorageMedium) {
	var (
		filePath = path.Join(volumePath, "test-file")
		source   = &v1.EmptyDirVolumeSource{Medium: medium}
		pod      = testPodWithVolume(uid, volumePath, source)
	)

	pod.Spec.Containers[0].Args = []string{
		"mounttest",
		fmt.Sprintf("--fs_type=%v", volumePath),
		fmt.Sprintf("--new_file_0666=%v", filePath),
		fmt.Sprintf("--file_perm=%v", filePath),
	}

	msg := fmt.Sprintf("emptydir 0666 on %v", formatMedium(medium))
	out := []string{
		"perms of file \"/test-volume/test-file\": -rw-rw-rw-",
		"content of file \"/test-volume/test-file\": mount-tester new file",
	}
	if medium == v1.StorageMediumMemory {
		out = append(out, "mount type of \"/test-volume\": tmpfs")
	}
	e2epodoutput.TestContainerOutput(ctx, f, msg, pod, 0, out)
}

func doTest0777(ctx context.Context, f *framework.Framework, uid int64, medium v1.StorageMedium) {
	var (
		filePath = path.Join(volumePath, "test-file")
		source   = &v1.EmptyDirVolumeSource{Medium: medium}
		pod      = testPodWithVolume(uid, volumePath, source)
	)

	pod.Spec.Containers[0].Args = []string{
		"mounttest",
		fmt.Sprintf("--fs_type=%v", volumePath),
		fmt.Sprintf("--new_file_0777=%v", filePath),
		fmt.Sprintf("--file_perm=%v", filePath),
	}

	msg := fmt.Sprintf("emptydir 0777 on %v", formatMedium(medium))
	out := []string{
		"perms of file \"/test-volume/test-file\": -rwxrwxrwx",
		"content of file \"/test-volume/test-file\": mount-tester new file",
	}
	if medium == v1.StorageMediumMemory {
		out = append(out, "mount type of \"/test-volume\": tmpfs")
	}
	e2epodoutput.TestContainerOutput(ctx, f, msg, pod, 0, out)
}

func formatMedium(medium v1.StorageMedium) string {
	if medium == v1.StorageMediumMemory {
		return "tmpfs"
	}

	return "node default medium"
}

// testPodWithVolume creates a Pod that runs as the given UID and with the given empty dir source mounted at the given path.
// If the uid is 0, the Pod will run as its default user (root).
func testPodWithVolume(uid int64, path string, source *v1.EmptyDirVolumeSource) *v1.Pod {
	podName := "pod-" + string(uuid.NewUUID())
	pod := &v1.Pod{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Pod",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: podName,
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name:  containerName,
					Image: imageutils.GetE2EImage(imageutils.Agnhost),
					VolumeMounts: []v1.VolumeMount{
						{
							Name:      volumeName,
							MountPath: path,
						},
					},
				},
			},
			SecurityContext: &v1.PodSecurityContext{
				SELinuxOptions: &v1.SELinuxOptions{
					Level: "s0",
				},
			},
			RestartPolicy: v1.RestartPolicyNever,
			Volumes: []v1.Volume{
				{
					Name: volumeName,
					VolumeSource: v1.VolumeSource{
						EmptyDir: source,
					},
				},
			},
		},
	}

	if uid != 0 {
		pod.Spec.SecurityContext.RunAsUser = &uid
	}

	return pod
}
