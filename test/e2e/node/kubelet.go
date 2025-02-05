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

package node

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/onsi/gomega"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/pkg/features"
	admissionapi "k8s.io/pod-security-admission/api"

	"k8s.io/kubernetes/test/e2e/feature"
	"k8s.io/kubernetes/test/e2e/framework"
	e2ekubectl "k8s.io/kubernetes/test/e2e/framework/kubectl"
	e2ekubelet "k8s.io/kubernetes/test/e2e/framework/kubelet"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2erc "k8s.io/kubernetes/test/e2e/framework/rc"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
	e2essh "k8s.io/kubernetes/test/e2e/framework/ssh"
	e2evolume "k8s.io/kubernetes/test/e2e/framework/volume"
	testutils "k8s.io/kubernetes/test/utils"
	imageutils "k8s.io/kubernetes/test/utils/image"

	"github.com/onsi/ginkgo/v2"
)

const (
	// Interval to framework.Poll /runningpods on a node
	pollInterval = 1 * time.Second
	// Interval to framework.Poll /stats/container on a node
	containerStatsPollingInterval = 5 * time.Second
	// Maximum number of nodes that we constraint to
	maxNodesToCheck = 10
)

// getPodMatches returns a set of pod names on the given node that matches the
// podNamePrefix and namespace.
func getPodMatches(ctx context.Context, c clientset.Interface, nodeName string, podNamePrefix string, namespace string) sets.String {
	matches := sets.NewString()
	framework.Logf("Checking pods on node %v via /runningpods endpoint", nodeName)
	runningPods, err := e2ekubelet.GetKubeletPods(ctx, c, nodeName)
	if err != nil {
		framework.Logf("Error checking running pods on %v: %v", nodeName, err)
		return matches
	}
	for _, pod := range runningPods.Items {
		if pod.Namespace == namespace && strings.HasPrefix(pod.Name, podNamePrefix) {
			matches.Insert(pod.Name)
		}
	}
	return matches
}

// waitTillNPodsRunningOnNodes polls the /runningpods endpoint on kubelet until
// it finds targetNumPods pods that match the given criteria (namespace and
// podNamePrefix). Note that we usually use label selector to filter pods that
// belong to the same RC. However, we use podNamePrefix with namespace here
// because pods returned from /runningpods do not contain the original label
// information; they are reconstructed by examining the container runtime. In
// the scope of this test, we do not expect pod naming conflicts so
// podNamePrefix should be sufficient to identify the pods.
func waitTillNPodsRunningOnNodes(ctx context.Context, c clientset.Interface, nodeNames sets.String, podNamePrefix string, namespace string, targetNumPods int, timeout time.Duration) error {
	return wait.PollUntilContextTimeout(ctx, pollInterval, timeout, false, func(ctx context.Context) (bool, error) {
		matchCh := make(chan sets.String, len(nodeNames))
		for _, item := range nodeNames.List() {
			// Launch a goroutine per node to check the pods running on the nodes.
			nodeName := item
			go func() {
				matchCh <- getPodMatches(ctx, c, nodeName, podNamePrefix, namespace)
			}()
		}

		seen := sets.NewString()
		for i := 0; i < len(nodeNames.List()); i++ {
			seen = seen.Union(<-matchCh)
		}
		if seen.Len() == targetNumPods {
			return true, nil
		}
		framework.Logf("Waiting for %d pods to be running on the node; %d are currently running;", targetNumPods, seen.Len())
		return false, nil
	})
}

// Creates a pod that mounts an nfs volume that is served by the nfs-server pod. The container
// will execute the passed in shell cmd. Waits for the pod to start.
// Note: the nfs plugin is defined inline, no PV or PVC.
func createPodUsingNfs(ctx context.Context, f *framework.Framework, c clientset.Interface, ns, nfsIP, cmd string) *v1.Pod {
	ginkgo.By("create pod using nfs volume")

	isPrivileged := true
	cmdLine := []string{"-c", cmd}
	pod := &v1.Pod{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Pod",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "pod-nfs-vol-",
			Namespace:    ns,
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name:    "pod-nfs-vol",
					Image:   imageutils.GetE2EImage(imageutils.BusyBox),
					Command: []string{"/bin/sh"},
					Args:    cmdLine,
					VolumeMounts: []v1.VolumeMount{
						{
							Name:      "nfs-vol",
							MountPath: "/mnt",
						},
					},
					SecurityContext: &v1.SecurityContext{
						Privileged: &isPrivileged,
					},
				},
			},
			RestartPolicy: v1.RestartPolicyNever, // don't restart pod
			Volumes: []v1.Volume{
				{
					Name: "nfs-vol",
					VolumeSource: v1.VolumeSource{
						NFS: &v1.NFSVolumeSource{
							Server:   nfsIP,
							Path:     "/exports",
							ReadOnly: false,
						},
					},
				},
			},
		},
	}
	rtnPod, err := c.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{})
	framework.ExpectNoError(err)

	err = e2epod.WaitTimeoutForPodReadyInNamespace(ctx, f.ClientSet, rtnPod.Name, f.Namespace.Name, framework.PodStartTimeout) // running & ready
	framework.ExpectNoError(err)

	rtnPod, err = c.CoreV1().Pods(ns).Get(ctx, rtnPod.Name, metav1.GetOptions{}) // return fresh pod
	framework.ExpectNoError(err)
	return rtnPod
}

// getHostExternalAddress gets the node for a pod and returns the first External
// address. Returns an error if the node the pod is on doesn't have an External
// address.
func getHostExternalAddress(ctx context.Context, client clientset.Interface, p *v1.Pod) (externalAddress string, err error) {
	node, err := client.CoreV1().Nodes().Get(ctx, p.Spec.NodeName, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	for _, address := range node.Status.Addresses {
		if address.Type == v1.NodeExternalIP {
			if address.Address != "" {
				externalAddress = address.Address
				break
			}
		}
	}
	if externalAddress == "" {
		err = fmt.Errorf("No external address for pod %v on node %v",
			p.Name, p.Spec.NodeName)
	}
	return
}

// Checks for a lingering nfs mount and/or uid directory on the pod's host. The host IP is used
// so that this test runs in GCE, where it appears that SSH cannot resolve the hostname.
// If expectClean is true then we expect the node to be cleaned up and thus commands like
// `ls <uid-dir>` should fail (since that dir was removed). If expectClean is false then we expect
// the node is not cleaned up, and thus cmds like `ls <uid-dir>` should succeed. We wait for the
// kubelet to be cleaned up, afterwhich an error is reported.
func checkPodCleanup(ctx context.Context, c clientset.Interface, pod *v1.Pod, expectClean bool) {
	timeout := 5 * time.Minute
	poll := 20 * time.Second
	podDir := filepath.Join("/var/lib/kubelet/pods", string(pod.UID))
	mountDir := filepath.Join(podDir, "volumes", "kubernetes.io~nfs")
	// use ip rather than hostname in GCE
	nodeIP, err := getHostExternalAddress(ctx, c, pod)
	framework.ExpectNoError(err)

	condMsg := "deleted"
	if !expectClean {
		condMsg = "present"
	}

	// table of host tests to perform (order may matter so not using a map)
	type testT struct {
		feature string // feature to test
		cmd     string // remote command to execute on node
	}
	tests := []testT{
		{
			feature: "pod UID directory",
			cmd:     fmt.Sprintf("sudo ls %v", podDir),
		},
		{
			feature: "pod nfs mount",
			cmd:     fmt.Sprintf("sudo mount | grep %v", mountDir),
		},
	}

	for _, test := range tests {
		framework.Logf("Wait up to %v for host's (%v) %q to be %v", timeout, nodeIP, test.feature, condMsg)
		err = wait.PollUntilContextTimeout(ctx, poll, timeout, false, func(ctx context.Context) (bool, error) {
			result, err := e2essh.NodeExec(ctx, nodeIP, test.cmd, framework.TestContext.Provider)
			framework.ExpectNoError(err)
			e2essh.LogResult(result)
			ok := result.Code == 0 && len(result.Stdout) > 0 && len(result.Stderr) == 0
			if expectClean && ok { // keep trying
				return false, nil
			}
			if !expectClean && !ok { // stop wait loop
				return true, fmt.Errorf("%v is gone but expected to exist", test.feature)
			}
			return true, nil // done, host is as expected
		})
		framework.ExpectNoError(err, fmt.Sprintf("Host (%v) cleanup error: %v. Expected %q to be %v", nodeIP, err, test.feature, condMsg))
	}

	if expectClean {
		framework.Logf("Pod's host has been cleaned up")
	} else {
		framework.Logf("Pod's host has not been cleaned up (per expectation)")
	}
}

var _ = SIGDescribe("kubelet", func() {
	var (
		c  clientset.Interface
		ns string
	)
	f := framework.NewDefaultFramework("kubelet")
	f.NamespacePodSecurityLevel = admissionapi.LevelPrivileged

	ginkgo.BeforeEach(func() {
		c = f.ClientSet
		ns = f.Namespace.Name
	})

	ginkgo.Describe("Clean up pods on node", func() {
		var (
			numNodes        int
			nodeNames       sets.String
			nodeLabels      map[string]string
			resourceMonitor *e2ekubelet.ResourceMonitor
		)
		type DeleteTest struct {
			podsPerNode int
			timeout     time.Duration
		}

		deleteTests := []DeleteTest{
			{podsPerNode: 10, timeout: 1 * time.Minute},
		}

		// Must be called in each It with the context of the test.
		start := func(ctx context.Context) {
			// Use node labels to restrict the pods to be assigned only to the
			// nodes we observe initially.
			nodeLabels = make(map[string]string)
			nodeLabels["kubelet_cleanup"] = "true"
			nodes, err := e2enode.GetBoundedReadySchedulableNodes(ctx, c, maxNodesToCheck)
			numNodes = len(nodes.Items)
			framework.ExpectNoError(err)
			nodeNames = sets.NewString()
			for i := 0; i < len(nodes.Items); i++ {
				nodeNames.Insert(nodes.Items[i].Name)
			}
			for nodeName := range nodeNames {
				for k, v := range nodeLabels {
					e2enode.AddOrUpdateLabelOnNode(c, nodeName, k, v)
					ginkgo.DeferCleanup(e2enode.RemoveLabelOffNode, c, nodeName, k)
				}
			}

			// While we only use a bounded number of nodes in the test. We need to know
			// the actual number of nodes in the cluster, to avoid running resourceMonitor
			// against large clusters.
			actualNodes, err := e2enode.GetReadySchedulableNodes(ctx, c)
			framework.ExpectNoError(err)

			// Start resourceMonitor only in small clusters.
			if len(actualNodes.Items) <= maxNodesToCheck {
				resourceMonitor = e2ekubelet.NewResourceMonitor(f.ClientSet, e2ekubelet.TargetContainers(), containerStatsPollingInterval)
				resourceMonitor.Start(ctx)
				ginkgo.DeferCleanup(resourceMonitor.Stop)
			}
		}

		for _, itArg := range deleteTests {
			name := fmt.Sprintf(
				"kubelet should be able to delete %d pods per node in %v.", itArg.podsPerNode, itArg.timeout)
			itArg := itArg
			ginkgo.It(name, func(ctx context.Context) {
				start(ctx)
				totalPods := itArg.podsPerNode * numNodes
				ginkgo.By(fmt.Sprintf("Creating a RC of %d pods and wait until all pods of this RC are running", totalPods))
				rcName := fmt.Sprintf("cleanup%d-%s", totalPods, string(uuid.NewUUID()))

				err := e2erc.RunRC(ctx, testutils.RCConfig{
					Client:       f.ClientSet,
					Name:         rcName,
					Namespace:    f.Namespace.Name,
					Image:        imageutils.GetPauseImageName(),
					Replicas:     totalPods,
					NodeSelector: nodeLabels,
				})
				framework.ExpectNoError(err)
				// Perform a sanity check so that we know all desired pods are
				// running on the nodes according to kubelet. The timeout is set to
				// only 30 seconds here because e2erc.RunRC already waited for all pods to
				// transition to the running status.
				err = waitTillNPodsRunningOnNodes(ctx, f.ClientSet, nodeNames, rcName, ns, totalPods, time.Second*30)
				framework.ExpectNoError(err)
				if resourceMonitor != nil {
					resourceMonitor.LogLatest()
				}

				ginkgo.By("Deleting the RC")
				e2erc.DeleteRCAndWaitForGC(ctx, f.ClientSet, f.Namespace.Name, rcName)
				// Check that the pods really are gone by querying /runningpods on the
				// node. The /runningpods handler checks the container runtime (or its
				// cache) and  returns a list of running pods. Some possible causes of
				// failures are:
				//   - kubelet deadlock
				//   - a bug in graceful termination (if it is enabled)
				//   - docker slow to delete pods (or resource problems causing slowness)
				start := time.Now()
				err = waitTillNPodsRunningOnNodes(ctx, f.ClientSet, nodeNames, rcName, ns, 0, itArg.timeout)
				framework.ExpectNoError(err)
				framework.Logf("Deleting %d pods on %d nodes completed in %v after the RC was deleted", totalPods, len(nodeNames),
					time.Since(start))
				if resourceMonitor != nil {
					resourceMonitor.LogCPUSummary()
				}
			})
		}
	})

	// Test host cleanup when disrupting the volume environment.
	f.Describe("host cleanup with volume mounts [HostCleanup]", f.WithFlaky(), func() {

		type hostCleanupTest struct {
			itDescr string
			podCmd  string
		}

		// Disrupt the nfs-server pod after a client pod accesses the nfs volume.
		// Note: the nfs-server is stopped NOT deleted. This is done to preserve its ip addr.
		//       If the nfs-server pod is deleted the client pod's mount can not be unmounted.
		//       If the nfs-server pod is deleted and re-created, due to having a different ip
		//       addr, the client pod's mount still cannot be unmounted.
		ginkgo.Context("Host cleanup after disrupting NFS volume [NFS]", func() {
			// issue #31272
			var (
				nfsServerPod *v1.Pod
				nfsIP        string
				pod          *v1.Pod // client pod
			)

			// fill in test slice for this context
			testTbl := []hostCleanupTest{
				{
					itDescr: "after stopping the nfs-server and deleting the (sleeping) client pod, the NFS mount and the pod's UID directory should be removed.",
					podCmd:  "sleep 6000", // keep pod running
				},
				{
					itDescr: "after stopping the nfs-server and deleting the (active) client pod, the NFS mount and the pod's UID directory should be removed.",
					podCmd:  "while true; do echo FeFieFoFum >>/mnt/SUCCESS; sleep 1; cat /mnt/SUCCESS; done",
				},
			}

			ginkgo.BeforeEach(func(ctx context.Context) {
				e2eskipper.SkipUnlessProviderIs(framework.ProvidersWithSSH...)
				_, nfsServerPod, nfsIP = e2evolume.NewNFSServer(ctx, c, ns, []string{"-G", "777", "/exports"})
			})

			ginkgo.AfterEach(func(ctx context.Context) {
				err := e2epod.DeletePodWithWait(ctx, c, pod)
				framework.ExpectNoError(err, "AfterEach: Failed to delete client pod ", pod.Name)
				err = e2epod.DeletePodWithWait(ctx, c, nfsServerPod)
				framework.ExpectNoError(err, "AfterEach: Failed to delete server pod ", nfsServerPod.Name)
			})

			// execute It blocks from above table of tests
			for _, t := range testTbl {
				t := t
				ginkgo.It(t.itDescr, func(ctx context.Context) {
					pod = createPodUsingNfs(ctx, f, c, ns, nfsIP, t.podCmd)

					ginkgo.By("Stop the NFS server")
					e2evolume.StopNFSServer(ctx, f, nfsServerPod)

					ginkgo.By("Delete the pod mounted to the NFS volume -- expect failure")
					err := e2epod.DeletePodWithWait(ctx, c, pod)
					gomega.Expect(err).To(gomega.HaveOccurred())
					// pod object is now stale, but is intentionally not nil

					ginkgo.By("Check if pod's host has been cleaned up -- expect not")
					checkPodCleanup(ctx, c, pod, false)

					ginkgo.By("Restart the nfs server")
					e2evolume.RestartNFSServer(ctx, f, nfsServerPod)

					ginkgo.By("Verify that the deleted client pod is now cleaned up")
					checkPodCleanup(ctx, c, pod, true)
				})
			}
		})
	})

	// Tests for NodeLogQuery feature
	f.Describe("kubectl get --raw \"/api/v1/nodes/<insert-node-name-here>/proxy/logs/?query=/<insert-log-file-name-here>", feature.NodeLogQuery, func() {
		var linuxNodeName string
		var windowsNodeName string

		ginkgo.BeforeEach(func(ctx context.Context) {
			allNodes, err := e2enode.GetReadyNodesIncludingTainted(ctx, c)
			framework.ExpectNoError(err)
			if len(allNodes.Items) == 0 {
				framework.Fail("Expected at least one node to be present")
			}
			// Make a copy of the node list as getLinuxNodes will filter out the Windows nodes
			nodes := allNodes.DeepCopy()

			linuxNodes := getLinuxNodes(nodes)
			if len(linuxNodes.Items) == 0 {
				framework.Fail("Expected at least one Linux node to be present")
			}
			linuxNodeName = linuxNodes.Items[0].Name

			windowsNodes := getWindowsNodes(allNodes)
			if len(windowsNodes.Items) == 0 {
				framework.Logf("No Windows node found")
			} else {
				windowsNodeName = windowsNodes.Items[0].Name
			}

		})

		/*
			Test if kubectl get --raw "/api/v1/nodes/<insert-node-name-here>/proxy/logs/?query"
			returns an error!
		*/

		ginkgo.It("should return the error with an empty --query option", func() {
			ginkgo.By("Starting the command")
			tk := e2ekubectl.NewTestKubeconfig(framework.TestContext.CertDir, framework.TestContext.Host, framework.TestContext.KubeConfig, framework.TestContext.KubeContext, framework.TestContext.KubectlPath, ns)

			queryCommand := fmt.Sprintf("/api/v1/nodes/%s/proxy/logs/?query", linuxNodeName)
			cmd := tk.KubectlCmd("get", "--raw", queryCommand)
			_, _, err := framework.StartCmdAndStreamOutput(cmd)
			if err != nil {
				framework.Failf("Failed to start kubectl command! Error: %v", err)
			}
			err = cmd.Wait()
			gomega.Expect(err).To(gomega.HaveOccurred(), "Command kubectl get --raw "+queryCommand+" was expected to return an error!")
		})

		/*
			Test if kubectl get --raw "/api/v1/nodes/<insert-linux-node-name-here>/proxy/logs/?query=kubelet"
			returns the kubelet logs
		*/

		ginkgo.It("should return the kubelet logs ", func(ctx context.Context) {
			ginkgo.By("Starting the command")
			tk := e2ekubectl.NewTestKubeconfig(framework.TestContext.CertDir, framework.TestContext.Host, framework.TestContext.KubeConfig, framework.TestContext.KubeContext, framework.TestContext.KubectlPath, ns)

			queryCommand := fmt.Sprintf("/api/v1/nodes/%s/proxy/logs/?query=kubelet", linuxNodeName)
			cmd := tk.KubectlCmd("get", "--raw", queryCommand)
			result := runKubectlCommand(cmd)
			assertContains("kubelet", result)
		})

		/*
			Test if kubectl get --raw "/api/v1/nodes/<insert-linux-node-name-here>/proxy/logs/?query=kubelet&boot=0"
			returns kubelet logs from the current boot
		*/

		ginkgo.It("should return the kubelet logs for the current boot", func(ctx context.Context) {
			ginkgo.By("Starting the command")
			tk := e2ekubectl.NewTestKubeconfig(framework.TestContext.CertDir, framework.TestContext.Host, framework.TestContext.KubeConfig, framework.TestContext.KubeContext, framework.TestContext.KubectlPath, ns)

			queryCommand := fmt.Sprintf("/api/v1/nodes/%s/proxy/logs/?query=kubelet&boot=0", linuxNodeName)
			cmd := tk.KubectlCmd("get", "--raw", queryCommand)
			result := runKubectlCommand(cmd)
			assertContains("kubelet", result)
		})

		/*
			Test if kubectl get --raw "/api/v1/nodes/<insert-linux-node-name-here>/proxy/logs/?query=kubelet&tailLines=3"
			returns the last three lines of the kubelet log
		*/

		ginkgo.It("should return the last three lines of the kubelet logs", func(ctx context.Context) {
			e2eskipper.SkipUnlessProviderIs(framework.ProvidersWithSSH...)
			ginkgo.By("Starting the command")
			tk := e2ekubectl.NewTestKubeconfig(framework.TestContext.CertDir, framework.TestContext.Host, framework.TestContext.KubeConfig, framework.TestContext.KubeContext, framework.TestContext.KubectlPath, ns)

			queryCommand := fmt.Sprintf("/api/v1/nodes/%s/proxy/logs/?query=kubelet&tailLines=3", linuxNodeName)
			cmd := tk.KubectlCmd("get", "--raw", queryCommand)
			result := runKubectlCommand(cmd)
			logs := journalctlCommandOnNode(linuxNodeName, "-u kubelet -n 3")
			if result != logs {
				framework.Failf("Failed to receive the correct kubelet logs or the correct amount of lines of logs")
			}
		})

		/*
			Test if kubectl get --raw "/api/v1/nodes/<insert-linux-node-name-here>/proxy/logs/?query=kubelet&pattern=container"
			returns kubelet logs for the current boot with the pattern container
		*/

		ginkgo.It("should return the kubelet logs for the current boot with the pattern container", func(ctx context.Context) {
			e2eskipper.SkipUnlessProviderIs(framework.ProvidersWithSSH...)
			ginkgo.By("Starting the command")
			tk := e2ekubectl.NewTestKubeconfig(framework.TestContext.CertDir, framework.TestContext.Host, framework.TestContext.KubeConfig, framework.TestContext.KubeContext, framework.TestContext.KubectlPath, ns)

			queryCommand := fmt.Sprintf("/api/v1/nodes/%s/proxy/logs/?query=kubelet&boot=0&pattern=container", linuxNodeName)
			cmd := tk.KubectlCmd("get", "--raw", queryCommand)
			result := runKubectlCommand(cmd)
			logs := journalctlCommandOnNode(linuxNodeName, "-u kubelet --grep container --boot 0")
			if result != logs {
				framework.Failf("Failed to receive the correct kubelet logs")
			}
		})

		/*
			Test if kubectl get --raw "/api/v1/nodes/<insert-linux-node-name-here>/proxy/logs/?query=kubelet&sinceTime=<now>"
			returns the kubelet logs since the current date and time. This can be "-- No entries --" which is correct.
		*/

		ginkgo.It("should return the kubelet logs since the current date and time", func() {
			ginkgo.By("Starting the command")
			start := time.Now().UTC()
			tk := e2ekubectl.NewTestKubeconfig(framework.TestContext.CertDir, framework.TestContext.Host, framework.TestContext.KubeConfig, framework.TestContext.KubeContext, framework.TestContext.KubectlPath, ns)

			currentTime := start.Format(time.RFC3339)
			queryCommand := fmt.Sprintf("/api/v1/nodes/%s/proxy/logs/?query=kubelet&sinceTime=%s", linuxNodeName, currentTime)
			cmd := tk.KubectlCmd("get", "--raw", queryCommand)
			journalctlDateLayout := "2006-1-2 15:4:5"
			result := runKubectlCommand(cmd)
			logs := journalctlCommandOnNode(linuxNodeName, fmt.Sprintf("-u kubelet --since \"%s\"", start.Format(journalctlDateLayout)))
			if result != logs {
				framework.Failf("Failed to receive the correct kubelet logs or the correct amount of lines of logs")
			}
		})

		/*
			Test if kubectl get --raw "/api/v1/nodes/<insert-windows-node-name-here>/proxy/logs/?query="Microsoft-Windows-Security-SPP"
			returns the Microsoft-Windows-Security-SPP log
		*/

		ginkgo.It("should return the Microsoft-Windows-Security-SPP logs", func(ctx context.Context) {
			if len(windowsNodeName) == 0 {
				ginkgo.Skip("No Windows node found")
			}
			ginkgo.By("Starting the command")
			tk := e2ekubectl.NewTestKubeconfig(framework.TestContext.CertDir, framework.TestContext.Host, framework.TestContext.KubeConfig, framework.TestContext.KubeContext, framework.TestContext.KubectlPath, ns)

			queryCommand := fmt.Sprintf("/api/v1/nodes/%s/proxy/logs/?query=Microsoft-Windows-Security-SPP", windowsNodeName)
			cmd := tk.KubectlCmd("get", "--raw", queryCommand)
			result := runKubectlCommand(cmd)
			assertContains("ProviderName: Microsoft-Windows-Security-SPP", result)
		})

		/*
			Test if kubectl get --raw "/api/v1/nodes/<insert-windows-node-name-here>/proxy/logs/?query=Microsoft-Windows-Security-SPP&tailLines=3"
			returns the last three lines of the Microsoft-Windows-Security-SPP log
		*/

		ginkgo.It("should return the last three lines of the Microsoft-Windows-Security-SPP logs", func(ctx context.Context) {
			e2eskipper.SkipUnlessProviderIs(framework.ProvidersWithSSH...)
			if len(windowsNodeName) == 0 {
				ginkgo.Skip("No Windows node found")
			}
			ginkgo.By("Starting the command")
			tk := e2ekubectl.NewTestKubeconfig(framework.TestContext.CertDir, framework.TestContext.Host, framework.TestContext.KubeConfig, framework.TestContext.KubeContext, framework.TestContext.KubectlPath, ns)

			queryCommand := fmt.Sprintf("/api/v1/nodes/%s/proxy/logs/?query=Microsoft-Windows-Security-SPP&tailLines=3", windowsNodeName)
			cmd := tk.KubectlCmd("get", "--raw", queryCommand)
			result := runKubectlCommand(cmd)
			logs := getWinEventCommandOnNode(windowsNodeName, "Microsoft-Windows-Security-SPP", " -MaxEvents 3")
			if trimSpaceNewlineInString(result) != trimSpaceNewlineInString(logs) {
				framework.Failf("Failed to receive the correct Microsoft-Windows-Security-SPP logs or the correct amount of lines of logs")
			}
		})

		/*
			Test if kubectl get --raw "/api/v1/nodes/<insert-windows-node-name-here>/proxy/logs/?query=Microsoft-Windows-Security-SPP&pattern=Health"
			returns the lines of the Microsoft-Windows-Security-SPP log with the pattern Health
		*/

		ginkgo.It("should return the Microsoft-Windows-Security-SPP logs with the pattern Health", func(ctx context.Context) {
			e2eskipper.SkipUnlessProviderIs(framework.ProvidersWithSSH...)
			if len(windowsNodeName) == 0 {
				ginkgo.Skip("No Windows node found")
			}
			ginkgo.By("Starting the command")
			tk := e2ekubectl.NewTestKubeconfig(framework.TestContext.CertDir, framework.TestContext.Host, framework.TestContext.KubeConfig, framework.TestContext.KubeContext, framework.TestContext.KubectlPath, ns)

			queryCommand := fmt.Sprintf("/api/v1/nodes/%s/proxy/logs/?query=Microsoft-Windows-Security-SPP&pattern=Health", windowsNodeName)
			cmd := tk.KubectlCmd("get", "--raw", queryCommand)
			result := runKubectlCommand(cmd)
			logs := getWinEventCommandOnNode(windowsNodeName, "Microsoft-Windows-Security-SPP", "  | Where-Object -Property Message -Match Health")
			if trimSpaceNewlineInString(result) != trimSpaceNewlineInString(logs) {
				framework.Failf("Failed to receive the correct Microsoft-Windows-Security-SPP logs or the correct amount of lines of logs")
			}
		})
	})
})

var _ = SIGDescribe("specific log stream", feature.PodLogsQuerySplitStreams, framework.WithFeatureGate(features.PodLogsQuerySplitStreams), func() {
	var (
		c  clientset.Interface
		ns string
	)
	f := framework.NewDefaultFramework("pod-log-stream")
	f.NamespacePodSecurityLevel = admissionapi.LevelPrivileged

	ginkgo.BeforeEach(func() {
		c = f.ClientSet
		ns = f.Namespace.Name
	})

	ginkgo.It("kubectl get --raw /api/v1/namespaces/default/pods/<pod-name>/log?stream", func(ctx context.Context) {
		ginkgo.By("create pod")

		pod := &v1.Pod{
			TypeMeta: metav1.TypeMeta{
				Kind:       "Pod",
				APIVersion: "v1",
			},
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "log-stream-",
				Namespace:    ns,
			},
			Spec: v1.PodSpec{
				Containers: []v1.Container{
					{
						Name:    "log-stream",
						Image:   imageutils.GetE2EImage(imageutils.BusyBox),
						Command: []string{"/bin/sh"},
						Args:    []string{"-c", "echo out1; echo err1 >&2; tail -f /dev/null"},
					},
				},
				RestartPolicy: v1.RestartPolicyNever, // don't restart pod
			},
		}
		rtnPod, err := c.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{})
		framework.ExpectNoError(err)

		err = e2epod.WaitTimeoutForPodReadyInNamespace(ctx, f.ClientSet, rtnPod.Name, f.Namespace.Name, framework.PodStartTimeout) // running & ready
		framework.ExpectNoError(err)

		rtnPod, err = c.CoreV1().Pods(ns).Get(ctx, rtnPod.Name, metav1.GetOptions{}) // return fresh pod
		framework.ExpectNoError(err)

		ginkgo.By("Starting the command")
		tk := e2ekubectl.NewTestKubeconfig(framework.TestContext.CertDir, framework.TestContext.Host, framework.TestContext.KubeConfig, framework.TestContext.KubeContext, framework.TestContext.KubectlPath, ns)

		queryCommand := fmt.Sprintf("/api/v1/namespaces/%s/pods/%s/log?stream=All", rtnPod.Namespace, rtnPod.Name)
		cmd := tk.KubectlCmd("get", "--raw", queryCommand)
		result := runKubectlCommand(cmd)
		// the order of the logs is indeterminate
		assertContains("out1", result)
		assertContains("err1", result)

		queryCommand = fmt.Sprintf("/api/v1/namespaces/%s/pods/%s/log?stream=Stdout", rtnPod.Namespace, rtnPod.Name)
		cmd = tk.KubectlCmd("get", "--raw", queryCommand)
		result = runKubectlCommand(cmd)
		assertContains("out1", result)
		assertNotContains("err1", result)

		queryCommand = fmt.Sprintf("/api/v1/namespaces/%s/pods/%s/log?stream=Stderr", rtnPod.Namespace, rtnPod.Name)
		cmd = tk.KubectlCmd("get", "--raw", queryCommand)
		result = runKubectlCommand(cmd)
		assertContains("err1", result)
		assertNotContains("out1", result)
	})
})

func getLinuxNodes(nodes *v1.NodeList) *v1.NodeList {
	filteredNodes := nodes
	e2enode.Filter(filteredNodes, func(node v1.Node) bool {
		return isNode(&node, "linux")
	})
	return filteredNodes
}

func getWindowsNodes(nodes *v1.NodeList) *v1.NodeList {
	filteredNodes := nodes
	e2enode.Filter(filteredNodes, func(node v1.Node) bool {
		return isNode(&node, "windows")
	})
	return filteredNodes
}

func isNode(node *v1.Node, os string) bool {
	if node == nil {
		return false
	}
	if foundOS, found := node.Labels[v1.LabelOSStable]; found {
		return os == foundOS
	}
	return false
}

func runKubectlCommand(cmd *exec.Cmd) (result string) {
	stdout, stderr, err := framework.StartCmdAndStreamOutput(cmd)
	var buf bytes.Buffer
	if err != nil {
		framework.Failf("Failed to start kubectl command! Stderr: %v, error: %v", stderr, err)
	}
	defer stdout.Close()
	defer stderr.Close()
	defer framework.TryKill(cmd)

	b_read, err := io.Copy(&buf, stdout)
	if err != nil {
		framework.Failf("Expected output from kubectl alpha node-logs %s: %v\n Stderr: %v", cmd.Args, err, stderr)
	}
	out := ""
	if b_read >= 0 {
		out = buf.String()
	}

	framework.Logf("Kubectl output: %s", out)
	return out
}

func assertContains(expectedString string, result string) {
	if strings.Contains(result, expectedString) {
		return
	}
	framework.Failf("Failed to find \"%s\"", expectedString)
}

func assertNotContains(expectedString string, result string) {
	if !strings.Contains(result, expectedString) {
		return
	}
	framework.Failf("Found unexpected \"%s\"", expectedString)
}

func commandOnNode(nodeName string, cmd string) string {
	result, err := e2essh.NodeExec(context.Background(), nodeName, cmd, framework.TestContext.Provider)
	framework.ExpectNoError(err)
	e2essh.LogResult(result)
	return result.Stdout
}

func journalctlCommandOnNode(nodeName string, args string) string {
	return commandOnNode(nodeName, "journalctl --utc --no-pager --output=short-precise "+args)
}

func getWinEventCommandOnNode(nodeName string, providerName, args string) string {
	output := commandOnNode(nodeName, "Get-WinEvent -FilterHashtable @{LogName='Application'; ProviderName='"+providerName+"'}"+args+" | Sort-Object TimeCreated | Format-Table -AutoSize -Wrap")
	return output
}

func trimSpaceNewlineInString(s string) string {
	// Remove Windows newlines
	re := regexp.MustCompile(` +\r?\n +`)
	s = re.ReplaceAllString(s, "")
	// Replace spaces to account for cases like "\r\n " that could lead to false negatives
	return strings.ReplaceAll(s, " ", "")
}
