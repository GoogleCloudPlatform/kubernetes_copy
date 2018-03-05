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

package openstack

import (
	"fmt"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/uuid"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"
	"k8s.io/kubernetes/test/e2e/storage/utils"
)

var _ = utils.SIGDescribe("Volume Attach Verify [Feature:openstack][Serial][Disruptive]", func() {
	f := framework.NewDefaultFramework("restart-master")

	const labelKey = "openstack_e2e_label"
	var (
		client                clientset.Interface
		namespace             string
		volumeIDs             []string
		pods                  []*v1.Pod
		numNodes              int
		nodeKeyValueLabelList []map[string]string
		nodeNameList          []string
	)
	BeforeEach(func() {
		framework.SkipUnlessProviderIs("openstack")
		client = f.ClientSet
		namespace = f.Namespace.Name
		framework.ExpectNoError(framework.WaitForAllNodesSchedulable(client, framework.TestContext.NodeSchedulableTimeout))

		nodes := framework.GetReadySchedulableNodesOrDie(client)
		numNodes = len(nodes.Items)
		if numNodes < 2 {
			framework.Skipf("Requires at least %d nodes (not %d)", 2, len(nodes.Items))
		}

		for i := 0; i < numNodes; i++ {
			nodeName := nodes.Items[i].Name
			nodeNameList = append(nodeNameList, nodeName)
			nodeLabelValue := "openstack_e2e_" + string(uuid.NewUUID())
			nodeKeyValueLabel := make(map[string]string)
			nodeKeyValueLabel[labelKey] = nodeLabelValue
			nodeKeyValueLabelList = append(nodeKeyValueLabelList, nodeKeyValueLabel)
			framework.AddOrUpdateLabelOnNode(client, nodeName, labelKey, nodeLabelValue)
		}
	})

	It("verify volume remains attached after master kubelet restart", func() {
		osp, id, err := getOpenstack(client)
		Expect(err).NotTo(HaveOccurred())

		// Create pod on each node
		for i := 0; i < numNodes; i++ {
			By(fmt.Sprintf("%d: Creating a test openstack volume", i))
			volumeID, err := createOpenstackVolume(osp)
			Expect(err).NotTo(HaveOccurred())

			volumeIDs = append(volumeIDs, volumeID)

			By(fmt.Sprintf("Creating pod %d on node %v", i, nodeNameList[i]))
			podspec := getOpenstackPodSpecWithVolumeIDs([]string{volumeID}, nodeKeyValueLabelList[i], nil)
			pod, err := client.CoreV1().Pods(namespace).Create(podspec)
			Expect(err).NotTo(HaveOccurred())
			defer framework.DeletePodWithWait(f, client, pod)

			By("Waiting for pod to be ready")
			Expect(framework.WaitForPodNameRunningInNamespace(client, pod.Name, namespace)).To(Succeed())

			pod, err = client.CoreV1().Pods(namespace).Get(pod.Name, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())

			pods = append(pods, pod)

			nodeName := types.NodeName(pod.Spec.NodeName)
			By(fmt.Sprintf("Verify volume %s is attached to the pod %v", volumeID, nodeName))
			isAttached, err := verifyOpenstackDiskAttached(client, osp, id, volumeID, types.NodeName(nodeName))
			Expect(err).NotTo(HaveOccurred())
			Expect(isAttached).To(BeTrue(), fmt.Sprintf("disk: %s is not attached with the node", volumeID))

		}

		By("Restarting kubelet on master node")
		masterAddress := framework.GetMasterHost() + ":22"
		err = framework.RestartKubelet(masterAddress)
		Expect(err).NotTo(HaveOccurred(), "Unable to restart kubelet on master node")

		By("Verifying the kubelet on master node is up")
		err = framework.WaitForKubeletUp(masterAddress)
		Expect(err).NotTo(HaveOccurred())

		for i, pod := range pods {
			volumeID := volumeIDs[i]

			nodeName := types.NodeName(pod.Spec.NodeName)
			By(fmt.Sprintf("After master restart, verify volume %v is attached to the pod %v", volumeID, nodeName))
			isAttached, err := verifyOpenstackDiskAttached(client, osp, id, volumeIDs[i], types.NodeName(nodeName))
			Expect(err).NotTo(HaveOccurred())
			Expect(isAttached).To(BeTrue(), fmt.Sprintf("disk: %s is not attached with the node", volumeID))

			By(fmt.Sprintf("Deleting pod on node %v", nodeName))
			err = framework.DeletePodWithWait(f, client, pod)
			Expect(err).NotTo(HaveOccurred())

			By(fmt.Sprintf("Waiting for volume %s to be detached from the node %v", volumeID, nodeName))
			err = waitForOpenstackDiskToDetach(client, id, osp, volumeID, types.NodeName(nodeName))
			Expect(err).NotTo(HaveOccurred())

			By(fmt.Sprintf("Deleting volume %s", volumeID))
			err = osp.DeleteVolume(volumeID)
			Expect(err).NotTo(HaveOccurred())
		}
	})
})
