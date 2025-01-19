/*
Copyright 2017 The Kubernetes Authors.

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

/*
 * This test checks that various VolumeSources are working.
 *
 * There are two ways, how to test the volumes:
 * 1) With containerized server (NFS, Ceph, iSCSI, ...)
 * The test creates a server pod, exporting simple 'index.html' file.
 * Then it uses appropriate VolumeSource to import this file into a client pod
 * and checks that the pod can see the file. It does so by importing the file
 * into web server root and loading the index.html from it.
 *
 * These tests work only when privileged containers are allowed, exporting
 * various filesystems (ex: NFS) usually needs some mounting or
 * other privileged magic in the server pod.
 *
 * Note that the server containers are for testing purposes only and should not
 * be used in production.
 *
 * 2) With server outside of Kubernetes
 * Appropriate server must exist somewhere outside
 * the tested Kubernetes cluster. The test itself creates a new volume,
 * and checks, that Kubernetes can use it as a volume.
 */

package volume

import (
	"context"
	"crypto/sha256"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
	clientexec "k8s.io/client-go/util/exec"
	"k8s.io/kubernetes/test/e2e/framework"
	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2epodoutput "k8s.io/kubernetes/test/e2e/framework/pod/output"
	imageutils "k8s.io/kubernetes/test/utils/image"
	admissionapi "k8s.io/pod-security-admission/api"
	uexec "k8s.io/utils/exec"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
)

const (
	// Kb is byte size of kilobyte
	Kb int64 = 1000
	// Mb is byte size of megabyte
	Mb int64 = 1000 * Kb
	// Gb is byte size of gigabyte
	Gb int64 = 1000 * Mb
	// Tb is byte size of terabyte
	Tb int64 = 1000 * Gb
	// KiB is byte size of kibibyte
	KiB int64 = 1024
	// MiB is byte size of mebibyte
	MiB int64 = 1024 * KiB
	// GiB is byte size of gibibyte
	GiB int64 = 1024 * MiB
	// TiB is byte size of tebibyte
	TiB int64 = 1024 * GiB

	// VolumeServerPodStartupTimeout is a waiting period for volume server (Ceph, ...) to initialize itself.
	VolumeServerPodStartupTimeout = 3 * time.Minute

	// PodCleanupTimeout is a waiting period for pod to be cleaned up and unmount its volumes so we
	// don't tear down containers with NFS/Ceph server too early.
	PodCleanupTimeout = 20 * time.Second
)

// SizeRange encapsulates a range of sizes specified as minimum and maximum quantity strings
// Both values are optional.
// If size is not set, it will assume there's not limitation and it may set a very small size (E.g. 1ki)
// as Min and set a considerable big size(E.g. 10Ei) as Max, which make it possible to calculate
// the intersection of given intervals (if it exists)
type SizeRange struct {
	// Max quantity specified as a string including units. E.g "3Gi".
	// If the Max size is unset, It will be assign a default valid maximum size 10Ei,
	// which is defined in test/e2e/storage/testsuites/base.go
	Max string
	// Min quantity specified as a string including units. E.g "1Gi"
	// If the Min size is unset, It will be assign a default valid minimum size 1Ki,
	// which is defined in test/e2e/storage/testsuites/base.go
	Min string
}

// TestConfig is a struct for configuration of one tests. The test consist of:
// - server pod - runs serverImage, exports ports[]
// - client pod - does not need any special configuration
type TestConfig struct {
	Namespace string
	// Prefix of all pods. Typically the test name.
	Prefix string
	// Name of container image for the server pod.
	ServerImage string
	// Ports to export from the server pod. TCP only.
	ServerPorts []int
	// Commands to run in the container image.
	ServerCmds []string
	// Arguments to pass to the container image.
	ServerArgs []string
	// Volumes needed to be mounted to the server container from the host
	// map <host (source) path> -> <container (dst.) path>
	// if <host (source) path> is empty, mount a tmpfs emptydir
	ServerVolumes map[string]string
	// Message to wait for before starting clients
	ServerReadyMessage string
	// Use HostNetwork for the server
	ServerHostNetwork bool
	// Wait for the pod to terminate successfully
	// False indicates that the pod is long running
	WaitForCompletion bool
	// ClientNodeSelection restricts where the client pod runs on.  Default is any node.
	ClientNodeSelection e2epod.NodeSelection
}

// Test contains a volume to mount into a client pod and its
// expected content.
type Test struct {
	Volume v1.VolumeSource
	Mode   v1.PersistentVolumeMode
	// Name of file to read/write in FileSystem mode
	File            string
	ExpectedContent string
}

// NewNFSServer is a NFS-specific wrapper for CreateStorageServer.
func NewNFSServer(ctx context.Context, cs clientset.Interface, namespace string, args []string) (config TestConfig, pod *v1.Pod, host string) {
	return NewNFSServerWithNodeName(ctx, cs, namespace, args, "")
}

func NewNFSServerWithNodeName(ctx context.Context, cs clientset.Interface, namespace string, args []string, nodeName string) (config TestConfig, pod *v1.Pod, host string) {
	config = TestConfig{
		Namespace:          namespace,
		Prefix:             "nfs",
		ServerImage:        imageutils.GetE2EImage(imageutils.VolumeNFSServer),
		ServerPorts:        []int{2049},
		ServerVolumes:      map[string]string{"": "/exports"},
		ServerReadyMessage: "NFS started",
	}
	if nodeName != "" {
		config.ClientNodeSelection = e2epod.NodeSelection{Name: nodeName}
	}

	if len(args) > 0 {
		config.ServerArgs = args
	}
	pod, host = CreateStorageServer(ctx, cs, config)
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return config, pod, host
}

// Restart the passed-in nfs-server by issuing a `rpc.nfsd 1` command in the
// pod's (only) container. This command changes the number of nfs server threads from
// (presumably) zero back to 1, and therefore allows nfs to open connections again.
func RestartNFSServer(f *framework.Framework, serverPod *v1.Pod) {
	const startcmd = "rpc.nfsd 1"
	_, _, err := PodExec(f, serverPod, startcmd)
	framework.ExpectNoError(err)
}

// Stop the passed-in nfs-server by issuing a `rpc.nfsd 0` command in the
// pod's (only) container. This command changes the number of nfs server threads to 0,
// thus closing all open nfs connections.
func StopNFSServer(f *framework.Framework, serverPod *v1.Pod) {
	const stopcmd = "rpc.nfsd 0 && for i in $(seq 200); do rpcinfo -p | grep -q nfs || break; sleep 1; done"
	_, _, err := PodExec(f, serverPod, stopcmd)
	framework.ExpectNoError(err)
}

// CreateStorageServer is a wrapper for startVolumeServer(). A storage server config is passed in, and a pod pointer
// and ip address string are returned.
// Note: Expect() is called so no error is returned.
func CreateStorageServer(ctx context.Context, cs clientset.Interface, config TestConfig) (pod *v1.Pod, ip string) {
	pod = startVolumeServer(ctx, cs, config)
	gomega.Expect(pod).NotTo(gomega.BeNil(), "storage server pod should not be nil")
	ip = pod.Status.PodIP
	gomega.Expect(ip).NotTo(gomega.BeEmpty(), fmt.Sprintf("pod %s's IP should not be empty", pod.Name))
	framework.Logf("%s server pod IP address: %s", config.Prefix, ip)
	return pod, ip
}

// GetVolumeAttachmentName returns the hash value of the provisioner, the config ClientNodeSelection name,
// and the VolumeAttachment name of the PV that is bound to the PVC with the passed in claimName and claimNamespace.
func GetVolumeAttachmentName(ctx context.Context, cs clientset.Interface, config TestConfig, provisioner string, claimName string, claimNamespace string) string {
	var nodeName string
	// For provisioning tests, ClientNodeSelection is not set so we do not know the NodeName of the VolumeAttachment of the PV that is
	// bound to the PVC with the passed in claimName and claimNamespace. We need this NodeName because it is used to generate the
	// attachmentName that is returned, and used to look up a certain VolumeAttachment in WaitForVolumeAttachmentTerminated.
	// To get the nodeName of the VolumeAttachment, we get all the VolumeAttachments, look for the VolumeAttachment with a
	// PersistentVolumeName equal to the PV that is bound to the passed in PVC, and then we get the NodeName from that VolumeAttachment.
	if config.ClientNodeSelection.Name == "" {
		claim, _ := cs.CoreV1().PersistentVolumeClaims(claimNamespace).Get(ctx, claimName, metav1.GetOptions{})
		pvName := claim.Spec.VolumeName
		volumeAttachments, _ := cs.StorageV1().VolumeAttachments().List(ctx, metav1.ListOptions{})
		for _, volumeAttachment := range volumeAttachments.Items {
			if *volumeAttachment.Spec.Source.PersistentVolumeName == pvName {
				nodeName = volumeAttachment.Spec.NodeName
				break
			}
		}
	} else {
		nodeName = config.ClientNodeSelection.Name
	}
	handle := getVolumeHandle(ctx, cs, claimName, claimNamespace)
	attachmentHash := sha256.Sum256([]byte(fmt.Sprintf("%s%s%s", handle, provisioner, nodeName)))
	return fmt.Sprintf("csi-%x", attachmentHash)
}

// getVolumeHandle returns the VolumeHandle of the PV that is bound to the PVC with the passed in claimName and claimNamespace.
func getVolumeHandle(ctx context.Context, cs clientset.Interface, claimName string, claimNamespace string) string {
	// re-get the claim to the latest state with bound volume
	claim, err := cs.CoreV1().PersistentVolumeClaims(claimNamespace).Get(ctx, claimName, metav1.GetOptions{})
	if err != nil {
		framework.ExpectNoError(err, "Cannot get PVC")
		return ""
	}
	pvName := claim.Spec.VolumeName
	pv, err := cs.CoreV1().PersistentVolumes().Get(ctx, pvName, metav1.GetOptions{})
	if err != nil {
		framework.ExpectNoError(err, "Cannot get PV")
		return ""
	}
	if pv.Spec.CSI == nil {
		gomega.Expect(pv.Spec.CSI).NotTo(gomega.BeNil())
		return ""
	}
	return pv.Spec.CSI.VolumeHandle
}

// WaitForVolumeAttachmentCreated waits for the VolumeAttachment with the passed in attachmentName to be created.
func WaitForVolumeAttachmentCreated(ctx context.Context, attachmentName string, cs clientset.Interface, interval, timeout time.Duration) error {
	waitErr := wait.PollUntilContextTimeout(ctx, interval, timeout, true, func(ctx context.Context) (bool, error) {
		_, err := cs.StorageV1().VolumeAttachments().Get(ctx, attachmentName, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				return false, nil
			}
			return false, err
		}
		return true, nil
	})
	if waitErr != nil {
		return fmt.Errorf("error waiting volume attachment %v to be created: %w", attachmentName, waitErr)
	}
	return nil
}

// WaitForVolumeAttachmentTerminated waits for the VolumeAttachment with the passed in attachmentName to be terminated.
func WaitForVolumeAttachmentTerminated(ctx context.Context, attachmentName string, cs clientset.Interface, timeout time.Duration) error {
	waitErr := wait.PollUntilContextTimeout(ctx, 10*time.Second, timeout, true, func(ctx context.Context) (bool, error) {
		_, err := cs.StorageV1().VolumeAttachments().Get(ctx, attachmentName, metav1.GetOptions{})
		if err != nil {
			// if the volumeattachment object is not found, it means it has been terminated.
			if apierrors.IsNotFound(err) {
				return true, nil
			}
			return false, err
		}
		return false, nil
	})
	if waitErr != nil {
		return fmt.Errorf("error waiting volume attachment %v to terminate: %v", attachmentName, waitErr)
	}
	return nil
}

// startVolumeServer starts a container specified by config.serverImage and exports all
// config.serverPorts from it. The returned pod should be used to get the server
// IP address and create appropriate VolumeSource.
func startVolumeServer(ctx context.Context, client clientset.Interface, config TestConfig) *v1.Pod {
	podClient := client.CoreV1().Pods(config.Namespace)

	portCount := len(config.ServerPorts)
	serverPodPorts := make([]v1.ContainerPort, portCount)

	for i := 0; i < portCount; i++ {
		portName := fmt.Sprintf("%s-%d", config.Prefix, i)

		serverPodPorts[i] = v1.ContainerPort{
			Name:          portName,
			ContainerPort: int32(config.ServerPorts[i]),
			Protocol:      v1.ProtocolTCP,
		}
	}

	volumeCount := len(config.ServerVolumes)
	volumes := make([]v1.Volume, volumeCount)
	mounts := make([]v1.VolumeMount, volumeCount)

	i := 0
	for src, dst := range config.ServerVolumes {
		mountName := fmt.Sprintf("path%d", i)
		volumes[i].Name = mountName
		if src == "" {
			volumes[i].VolumeSource.EmptyDir = &v1.EmptyDirVolumeSource{}
		} else {
			volumes[i].VolumeSource.HostPath = &v1.HostPathVolumeSource{
				Path: src,
			}
		}

		mounts[i].Name = mountName
		mounts[i].ReadOnly = false
		mounts[i].MountPath = dst

		i++
	}

	serverPodName := fmt.Sprintf("%s-server", config.Prefix)
	ginkgo.By(fmt.Sprint("creating ", serverPodName, " pod"))
	privileged := new(bool)
	*privileged = true

	restartPolicy := v1.RestartPolicyAlways
	if config.WaitForCompletion {
		restartPolicy = v1.RestartPolicyNever
	}
	serverPod := &v1.Pod{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Pod",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: serverPodName,
			Labels: map[string]string{
				"role": serverPodName,
			},
		},

		Spec: v1.PodSpec{
			HostNetwork: config.ServerHostNetwork,
			Containers: []v1.Container{
				{
					Name:  serverPodName,
					Image: config.ServerImage,
					SecurityContext: &v1.SecurityContext{
						Privileged: privileged,
					},
					Command:      config.ServerCmds,
					Args:         config.ServerArgs,
					Ports:        serverPodPorts,
					VolumeMounts: mounts,
				},
			},
			Volumes:       volumes,
			RestartPolicy: restartPolicy,
		},
	}

	if config.ClientNodeSelection.Name != "" {
		serverPod.Spec.NodeName = config.ClientNodeSelection.Name
	}

	var pod *v1.Pod
	serverPod, err := podClient.Create(ctx, serverPod, metav1.CreateOptions{})
	// ok if the server pod already exists. TODO: make this controllable by callers
	if err != nil {
		if apierrors.IsAlreadyExists(err) {
			framework.Logf("Ignore \"already-exists\" error, re-get pod...")
			ginkgo.By(fmt.Sprintf("re-getting the %q server pod", serverPodName))
			serverPod, err = podClient.Get(ctx, serverPodName, metav1.GetOptions{})
			framework.ExpectNoError(err, "Cannot re-get the server pod %q: %v", serverPodName, err)
			pod = serverPod
		} else {
			framework.ExpectNoError(err, "Failed to create %q pod: %v", serverPodName, err)
		}
	}
	if config.WaitForCompletion {
		framework.ExpectNoError(e2epod.WaitForPodSuccessInNamespace(ctx, client, serverPod.Name, serverPod.Namespace))
		framework.ExpectNoError(podClient.Delete(ctx, serverPod.Name, metav1.DeleteOptions{}))
	} else {
		framework.ExpectNoError(e2epod.WaitForPodRunningInNamespace(ctx, client, serverPod))
		if pod == nil {
			ginkgo.By(fmt.Sprintf("locating the %q server pod", serverPodName))
			pod, err = podClient.Get(ctx, serverPodName, metav1.GetOptions{})
			framework.ExpectNoError(err, "Cannot locate the server pod %q: %v", serverPodName, err)
		}
	}
	if config.ServerReadyMessage != "" {
		_, err := e2epodoutput.LookForStringInLogWithoutKubectl(ctx, client, pod.Namespace, pod.Name, serverPodName, config.ServerReadyMessage, VolumeServerPodStartupTimeout)
		framework.ExpectNoError(err, "Failed to find %q in pod logs: %s", config.ServerReadyMessage, err)
	}
	return pod
}

// TestServerCleanup cleans server pod.
func TestServerCleanup(ctx context.Context, f *framework.Framework, config TestConfig) {
	ginkgo.By(fmt.Sprint("cleaning the environment after ", config.Prefix))
	defer ginkgo.GinkgoRecover()

	if config.ServerImage == "" {
		return
	}

	err := e2epod.DeletePodWithWaitByName(ctx, f.ClientSet, config.Prefix+"-server", config.Namespace)
	framework.ExpectNoError(err, "delete pod %v in namespace %v", config.Prefix+"-server", config.Namespace)
}

func runVolumeTesterPod(ctx context.Context, client clientset.Interface, timeouts *framework.TimeoutContext, config TestConfig, podSuffix string, privileged bool, fsGroup *int64, tests []Test, slow bool) (*v1.Pod, error) {
	ginkgo.By(fmt.Sprint("starting ", config.Prefix, "-", podSuffix))
	var gracePeriod int64 = 1
	var command string

	/**
	This condition fixes running storage e2e tests in SELinux environment.
	HostPath Volume Plugin creates a directory within /tmp on host machine, to be mounted as volume.
	Inject-pod writes content to the volume, and a client-pod tries the read the contents and verify.
	When SELinux is enabled on the host, client-pod can not read the content, with permission denied.
	Invoking client-pod as privileged, so that it can access the volume content, even when SELinux is enabled on the host.
	*/
	securityLevel := admissionapi.LevelBaseline // TODO (#118184): also support LevelRestricted
	if privileged || config.Prefix == "hostpathsymlink" || config.Prefix == "hostpath" {
		securityLevel = admissionapi.LevelPrivileged
	}
	command = "while true ; do sleep 2; done "
	seLinuxOptions := &v1.SELinuxOptions{Level: "s0:c0,c1"}
	clientPod := &v1.Pod{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Pod",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: config.Prefix + "-" + podSuffix,
			Labels: map[string]string{
				"role": config.Prefix + "-" + podSuffix,
			},
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name:       config.Prefix + "-" + podSuffix,
					Image:      e2epod.GetDefaultTestImage(),
					WorkingDir: "/opt",
					// An imperative and easily debuggable container which reads/writes vol contents for
					// us to scan in the tests or by eye.
					// We expect that /opt is empty in the minimal containers which we use in this test.
					Command:      e2epod.GenerateScriptCmd(command),
					VolumeMounts: []v1.VolumeMount{},
				},
			},
			TerminationGracePeriodSeconds: &gracePeriod,
			SecurityContext:               e2epod.GeneratePodSecurityContext(fsGroup, seLinuxOptions),
			Volumes:                       []v1.Volume{},
		},
	}
	e2epod.SetNodeSelection(&clientPod.Spec, config.ClientNodeSelection)

	for i, test := range tests {
		volumeName := fmt.Sprintf("%s-%s-%d", config.Prefix, "volume", i)

		// We need to make the container privileged when SELinux is enabled on the
		// host,  so the test can write data to a location like /tmp. Also, due to
		// the Docker bug below, it's not currently possible to map a device with
		// a privileged container, so we don't go privileged for block volumes.
		// https://github.com/moby/moby/issues/35991
		if privileged && test.Mode == v1.PersistentVolumeBlock {
			securityLevel = admissionapi.LevelBaseline
		}
		clientPod.Spec.Containers[0].SecurityContext = e2epod.GenerateContainerSecurityContext(securityLevel)

		if test.Mode == v1.PersistentVolumeBlock {
			clientPod.Spec.Containers[0].VolumeDevices = append(clientPod.Spec.Containers[0].VolumeDevices, v1.VolumeDevice{
				Name:       volumeName,
				DevicePath: fmt.Sprintf("/opt/%d", i),
			})
		} else {
			clientPod.Spec.Containers[0].VolumeMounts = append(clientPod.Spec.Containers[0].VolumeMounts, v1.VolumeMount{
				Name:      volumeName,
				MountPath: fmt.Sprintf("/opt/%d", i),
			})
		}
		clientPod.Spec.Volumes = append(clientPod.Spec.Volumes, v1.Volume{
			Name:         volumeName,
			VolumeSource: test.Volume,
		})
	}
	podsNamespacer := client.CoreV1().Pods(config.Namespace)
	clientPod, err := podsNamespacer.Create(ctx, clientPod, metav1.CreateOptions{})
	if err != nil {
		return nil, err
	}
	if slow {
		err = e2epod.WaitTimeoutForPodRunningInNamespace(ctx, client, clientPod.Name, clientPod.Namespace, timeouts.PodStartSlow)
	} else {
		err = e2epod.WaitTimeoutForPodRunningInNamespace(ctx, client, clientPod.Name, clientPod.Namespace, timeouts.PodStart)
	}
	if err != nil {
		e2epod.DeletePodOrFail(ctx, client, clientPod.Namespace, clientPod.Name)
		_ = e2epod.WaitForPodNotFoundInNamespace(ctx, client, clientPod.Name, clientPod.Namespace, timeouts.PodDelete)
		return nil, err
	}
	return clientPod, nil
}

func testVolumeContent(f *framework.Framework, pod *v1.Pod, containerName string, fsGroup *int64, fsType string, tests []Test) {
	ginkgo.By("Checking that text file contents are perfect.")
	for i, test := range tests {
		if test.Mode == v1.PersistentVolumeBlock {
			// Block: check content
			deviceName := fmt.Sprintf("/opt/%d", i)
			commands := GenerateReadBlockCmd(deviceName, len(test.ExpectedContent))
			_, err := e2epodoutput.LookForStringInPodExecToContainer(pod.Namespace, pod.Name, containerName, commands, test.ExpectedContent, time.Minute)
			framework.ExpectNoError(err, "failed: finding the contents of the block device %s.", deviceName)

			// Check that it's a real block device
			CheckVolumeModeOfPath(f, pod, test.Mode, deviceName)
		} else {
			// Filesystem: check content
			fileName := fmt.Sprintf("/opt/%d/%s", i, test.File)
			commands := GenerateReadFileCmd(fileName)
			_, err := e2epodoutput.LookForStringInPodExecToContainer(pod.Namespace, pod.Name, containerName, commands, test.ExpectedContent, time.Minute)
			framework.ExpectNoError(err, "failed: finding the contents of the mounted file %s.", fileName)

			// Check that a directory has been mounted
			dirName := filepath.Dir(fileName)
			CheckVolumeModeOfPath(f, pod, test.Mode, dirName)

			if !framework.NodeOSDistroIs("windows") {
				// Filesystem: check fsgroup
				if fsGroup != nil {
					ginkgo.By("Checking fsGroup is correct.")
					_, err = e2epodoutput.LookForStringInPodExecToContainer(pod.Namespace, pod.Name, containerName, []string{"ls", "-ld", dirName}, strconv.Itoa(int(*fsGroup)), time.Minute)
					framework.ExpectNoError(err, "failed: getting the right privileges in the file %v", int(*fsGroup))
				}

				// Filesystem: check fsType
				if fsType != "" {
					ginkgo.By("Checking fsType is correct.")
					_, err = e2epodoutput.LookForStringInPodExecToContainer(pod.Namespace, pod.Name, containerName, []string{"grep", " " + dirName + " ", "/proc/mounts"}, fsType, time.Minute)
					framework.ExpectNoError(err, "failed: getting the right fsType %s", fsType)
				}
			}
		}
	}
}

// TestVolumeClient start a client pod using given VolumeSource (exported by startVolumeServer())
// and check that the pod sees expected data, e.g. from the server pod.
// Multiple Tests can be specified to mount multiple volumes to a single
// pod.
// Timeout for dynamic provisioning (if "WaitForFirstConsumer" is set && provided PVC is not bound yet),
// pod creation, scheduling and complete pod startup (incl. volume attach & mount) is pod.podStartTimeout.
// It should be used for cases where "regular" dynamic provisioning of an empty volume is requested.
func TestVolumeClient(ctx context.Context, f *framework.Framework, config TestConfig, fsGroup *int64, fsType string, tests []Test) {
	testVolumeClient(ctx, f, config, fsGroup, fsType, tests, false)
}

// TestVolumeClientSlow is the same as TestVolumeClient except for its timeout.
// Timeout for dynamic provisioning (if "WaitForFirstConsumer" is set && provided PVC is not bound yet),
// pod creation, scheduling and complete pod startup (incl. volume attach & mount) is pod.slowPodStartTimeout.
// It should be used for cases where "special" dynamic provisioning is requested, such as volume cloning
// or snapshot restore.
func TestVolumeClientSlow(ctx context.Context, f *framework.Framework, config TestConfig, fsGroup *int64, fsType string, tests []Test) {
	testVolumeClient(ctx, f, config, fsGroup, fsType, tests, true)
}

func testVolumeClient(ctx context.Context, f *framework.Framework, config TestConfig, fsGroup *int64, fsType string, tests []Test, slow bool) {
	timeouts := f.Timeouts
	clientPod, err := runVolumeTesterPod(ctx, f.ClientSet, timeouts, config, "client", false, fsGroup, tests, slow)
	if err != nil {
		framework.Failf("Failed to create client pod: %v", err)
	}
	defer func() {
		// testVolumeClient might get used more than once per test, therefore
		// we have to clean up before returning.
		e2epod.DeletePodOrFail(ctx, f.ClientSet, clientPod.Namespace, clientPod.Name)
		framework.ExpectNoError(e2epod.WaitForPodNotFoundInNamespace(ctx, f.ClientSet, clientPod.Name, clientPod.Namespace, timeouts.PodDelete))
	}()

	testVolumeContent(f, clientPod, "", fsGroup, fsType, tests)

	ginkgo.By("Repeating the test on an ephemeral container (if enabled)")
	ec := &v1.EphemeralContainer{
		EphemeralContainerCommon: v1.EphemeralContainerCommon(clientPod.Spec.Containers[0]),
	}
	ec.Resources = v1.ResourceRequirements{}
	ec.Name = "volume-ephemeral-container"
	err = e2epod.NewPodClient(f).AddEphemeralContainerSync(ctx, clientPod, ec, timeouts.PodStart)
	// The API server will return NotFound for the subresource when the feature is disabled
	framework.ExpectNoError(err, "failed to add ephemeral container for re-test")
	testVolumeContent(f, clientPod, ec.Name, fsGroup, fsType, tests)
}

// InjectContent inserts index.html with given content into given volume. It does so by
// starting and auxiliary pod which writes the file there.
// The volume must be writable.
func InjectContent(ctx context.Context, f *framework.Framework, config TestConfig, fsGroup *int64, fsType string, tests []Test) {
	privileged := true
	timeouts := f.Timeouts
	if framework.NodeOSDistroIs("windows") {
		privileged = false
	}
	injectorPod, err := runVolumeTesterPod(ctx, f.ClientSet, timeouts, config, "injector", privileged, fsGroup, tests, false /*slow*/)
	if err != nil {
		framework.Failf("Failed to create injector pod: %v", err)
		return
	}
	defer func() {
		// This pod must get deleted before the function returns becaue the test relies on
		// the volume not being in use.
		e2epod.DeletePodOrFail(ctx, f.ClientSet, injectorPod.Namespace, injectorPod.Name)
		framework.ExpectNoError(e2epod.WaitForPodNotFoundInNamespace(ctx, f.ClientSet, injectorPod.Name, injectorPod.Namespace, timeouts.PodDelete))
	}()

	ginkgo.By("Writing text file contents in the container.")
	for i, test := range tests {
		commands := []string{"exec", injectorPod.Name, fmt.Sprintf("--namespace=%v", injectorPod.Namespace), "--"}
		if test.Mode == v1.PersistentVolumeBlock {
			// Block: write content
			deviceName := fmt.Sprintf("/opt/%d", i)
			commands = append(commands, generateWriteBlockCmd(test.ExpectedContent, deviceName)...)

		} else {
			// Filesystem: write content
			fileName := fmt.Sprintf("/opt/%d/%s", i, test.File)
			commands = append(commands, generateWriteFileCmd(test.ExpectedContent, fileName)...)
		}
		out, err := e2ekubectl.RunKubectl(injectorPod.Namespace, commands...)
		framework.ExpectNoError(err, "failed: writing the contents: %s", out)
	}

	// Check that the data have been really written in this pod.
	// This tests non-persistent volume types
	testVolumeContent(f, injectorPod, "", fsGroup, fsType, tests)
}

// generateWriteCmd is used by generateWriteBlockCmd and generateWriteFileCmd
func generateWriteCmd(content, path string) []string {
	var commands []string
	commands = []string{"/bin/sh", "-c", "echo '" + content + "' > " + path + "; sync"}
	return commands
}

// GenerateReadBlockCmd generates the corresponding command lines to read from a block device with the given file path.
func GenerateReadBlockCmd(fullPath string, numberOfCharacters int) []string {
	var commands []string
	commands = []string{"head", "-c", strconv.Itoa(numberOfCharacters), fullPath}
	return commands
}

// generateWriteBlockCmd generates the corresponding command lines to write to a block device the given content.
func generateWriteBlockCmd(content, fullPath string) []string {
	return generateWriteCmd(content, fullPath)
}

// GenerateReadFileCmd generates the corresponding command lines to read from a file with the given file path.
func GenerateReadFileCmd(fullPath string) []string {
	var commands []string
	commands = []string{"cat", fullPath}
	return commands
}

// generateWriteFileCmd generates the corresponding command lines to write a file with the given content and file path.
func generateWriteFileCmd(content, fullPath string) []string {
	return generateWriteCmd(content, fullPath)
}

// CheckVolumeModeOfPath check mode of volume
func CheckVolumeModeOfPath(f *framework.Framework, pod *v1.Pod, volMode v1.PersistentVolumeMode, path string) {
	if volMode == v1.PersistentVolumeBlock {
		// Check if block exists
		VerifyExecInPodSucceed(f, pod, fmt.Sprintf("test -b %s", path))

		// Double check that it's not directory
		VerifyExecInPodFail(f, pod, fmt.Sprintf("test -d %s", path), 1)
	} else {
		// Check if directory exists
		VerifyExecInPodSucceed(f, pod, fmt.Sprintf("test -d %s", path))

		// Double check that it's not block
		VerifyExecInPodFail(f, pod, fmt.Sprintf("test -b %s", path), 1)
	}
}

// PodExec runs f.ExecCommandInContainerWithFullOutput to execute a shell cmd in target pod
// TODO: put this under e2epod once https://github.com/kubernetes/kubernetes/issues/81245
// is resolved. Otherwise there will be dependency issue.
func PodExec(f *framework.Framework, pod *v1.Pod, shExec string) (string, string, error) {
	return e2epod.ExecCommandInContainerWithFullOutput(f, pod.Name, pod.Spec.Containers[0].Name, "/bin/sh", "-c", shExec)
}

// VerifyExecInPodSucceed verifies shell cmd in target pod succeed
// TODO: put this under e2epod once https://github.com/kubernetes/kubernetes/issues/81245
// is resolved. Otherwise there will be dependency issue.
func VerifyExecInPodSucceed(f *framework.Framework, pod *v1.Pod, shExec string) {
	stdout, stderr, err := PodExec(f, pod, shExec)
	if err != nil {
		if exiterr, ok := err.(uexec.CodeExitError); ok {
			exitCode := exiterr.ExitStatus()
			framework.ExpectNoError(err,
				"%q should succeed, but failed with exit code %d and error message %q\nstdout: %s\nstderr: %s",
				shExec, exitCode, exiterr, stdout, stderr)
		} else {
			framework.ExpectNoError(err,
				"%q should succeed, but failed with error message %q\nstdout: %s\nstderr: %s",
				shExec, err, stdout, stderr)
		}
	}
}

// VerifyExecInPodFail verifies shell cmd in target pod fail with certain exit code
// TODO: put this under e2epod once https://github.com/kubernetes/kubernetes/issues/81245
// is resolved. Otherwise there will be dependency issue.
func VerifyExecInPodFail(f *framework.Framework, pod *v1.Pod, shExec string, exitCode int) {
	stdout, stderr, err := PodExec(f, pod, shExec)
	if err != nil {
		if exiterr, ok := err.(clientexec.ExitError); ok {
			actualExitCode := exiterr.ExitStatus()
			gomega.Expect(actualExitCode).To(gomega.Equal(exitCode),
				"%q should fail with exit code %d, but failed with exit code %d and error message %q\nstdout: %s\nstderr: %s",
				shExec, exitCode, actualExitCode, exiterr, stdout, stderr)
		} else {
			framework.ExpectNoError(err,
				"%q should fail with exit code %d, but failed with error message %q\nstdout: %s\nstderr: %s",
				shExec, exitCode, err, stdout, stderr)
		}
	}
	gomega.Expect(err).To(gomega.HaveOccurred(), "%q should fail with exit code %d, but exit without error", shExec, exitCode)
}
