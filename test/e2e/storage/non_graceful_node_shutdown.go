/*
Copyright 2022 The Kubernetes Authors.

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

	"github.com/onsi/ginkgo/v2"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2epv "k8s.io/kubernetes/test/e2e/framework/pv"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
	"k8s.io/kubernetes/test/e2e/storage/drivers"
	storageframework "k8s.io/kubernetes/test/e2e/storage/framework"
	"k8s.io/kubernetes/test/e2e/storage/utils"
	imageutils "k8s.io/kubernetes/test/utils/image"
	admissionapi "k8s.io/pod-security-admission/api"
)

/*
This test assumes the following:
- The infra is GCP.

This test performs the following:
- Deploys a gce-pd csi driver
- Creates a gce-pd csi storage class
- Creates a pvc using the created gce-pd storage class
- Creates an app deployment with replica count 1 and uses the created pvc for volume
- Shutdowns the kubelet of node on which the app pod is scheduled.
  This shutdown is a non graceful shutdown as by default the grace period is 0 on Kubelet.
- Adds `out-of-service` taint on the node which is shut down.
- Verifies that pod gets immediately scheduled to a different node and gets into running and ready state.
- Starts the kubelet back.
- Removes the `out-of-service` taint from the node.
*/

var _ = utils.SIGDescribe(framework.WithDisruptive(), "[LinuxOnly] NonGracefulNodeShutdown", func() {
	var (
		c  clientset.Interface
		ns string
	)
	f := framework.NewDefaultFramework("non-graceful-shutdown")
	f.NamespacePodSecurityLevel = admissionapi.LevelPrivileged

	ginkgo.BeforeEach(func(ctx context.Context) {
		c = f.ClientSet
		ns = f.Namespace.Name
		e2eskipper.SkipUnlessProviderIs("gce")
		nodeList, err := e2enode.GetReadySchedulableNodes(ctx, c)
		if err != nil {
			framework.Logf("Failed to list node: %v", err)
		}
		if len(nodeList.Items) < 2 {
			ginkgo.Skip("At least 2 nodes are required to run the test")
		}
	})

	ginkgo.Describe("[NonGracefulNodeShutdown] pod that uses a persistent volume via gce pd driver", func() {
		ginkgo.It("should get immediately rescheduled to a different node after non graceful node shutdown ", func(ctx context.Context) {
			// Install gce pd csi driver
			ginkgo.By("deploying csi gce-pd driver")
			driver := drivers.InitGcePDCSIDriver()
			config := driver.PrepareTest(ctx, f)
			dDriver, ok := driver.(storageframework.DynamicPVTestDriver)
			if !ok {
				e2eskipper.Skipf("csi driver expected DynamicPVTestDriver but got %v", driver)
			}
			ginkgo.By("Creating a gce-pd storage class")
			sc := dDriver.GetDynamicProvisionStorageClass(ctx, config, "")
			_, err := c.StorageV1().StorageClasses().Create(ctx, sc, metav1.CreateOptions{})
			framework.ExpectNoError(err, "failed to create a storageclass")
			scName := &sc.Name

			deploymentName := "sts-pod-gcepd"
			podLabels := map[string]string{"app": deploymentName}
			pod := createAndVerifyStatefulDeployment(ctx, scName, deploymentName, ns, podLabels, c)
			oldNodeName := pod.Spec.NodeName

			ginkgo.By("Stopping the kubelet non gracefully for pod" + pod.Name)
			utils.KubeletCommand(ctx, utils.KStop, c, pod)
			ginkgo.DeferCleanup(utils.KubeletCommand, utils.KStart, c, pod)

			ginkgo.By("Adding out of service taint on node " + oldNodeName)
			// taint this node as out-of-service node
			taint := v1.Taint{
				Key:    v1.TaintNodeOutOfService,
				Effect: v1.TaintEffectNoExecute,
			}
			e2enode.AddOrUpdateTaintOnNode(ctx, c, oldNodeName, taint)
			ginkgo.DeferCleanup(e2enode.RemoveTaintOffNode, c, oldNodeName, taint)

			ginkgo.By(fmt.Sprintf("Checking if the pod %s got rescheduled to a new node", pod.Name))
			labelSelectorStr := labels.SelectorFromSet(podLabels).String()
			podListOpts := metav1.ListOptions{
				LabelSelector: labelSelectorStr,
				FieldSelector: fields.OneTermNotEqualSelector("spec.nodeName", oldNodeName).String(),
			}
			_, err = e2epod.WaitForPods(ctx, c, ns, podListOpts, e2epod.Range{MinMatching: 1}, framework.PodStartTimeout, "be running and ready", e2epod.RunningReady)
			framework.ExpectNoError(err)

			// Bring the node back online and remove the taint
			utils.KubeletCommand(ctx, utils.KStart, c, pod)
			e2enode.RemoveTaintOffNode(ctx, c, oldNodeName, taint)

			// Verify that a pod gets scheduled to the older node that was terminated non gracefully and now
			// is back online
			newDeploymentName := "sts-pod-gcepd-new"
			newPodLabels := map[string]string{"app": newDeploymentName}
			createAndVerifyStatefulDeployment(ctx, scName, newDeploymentName, ns, newPodLabels, c)
		})
	})
})

// createAndVerifyStatefulDeployment creates:
// i) a pvc using the provided storage class
// ii) creates a deployment with replica count 1 using the created pvc
// iii) finally verifies if the pod is running and ready and returns the pod object
func createAndVerifyStatefulDeployment(ctx context.Context, scName *string, name, ns string, podLabels map[string]string,
	c clientset.Interface) *v1.Pod {
	ginkgo.By("Creating a pvc using the storage class " + *scName)
	pvc := e2epv.MakePersistentVolumeClaim(e2epv.PersistentVolumeClaimConfig{
		StorageClassName: scName,
	}, ns)
	gotPVC, err := c.CoreV1().PersistentVolumeClaims(ns).Create(ctx, pvc, metav1.CreateOptions{})
	framework.ExpectNoError(err, "failed to create a persistent volume claim")

	ginkgo.By("Creating a deployment using the pvc " + pvc.Name)
	dep := makeDeployment(ns, name, gotPVC.Name, podLabels)
	_, err = c.AppsV1().Deployments(ns).Create(ctx, dep, metav1.CreateOptions{})
	framework.ExpectNoError(err, "failed to created the deployment")

	ginkgo.By(fmt.Sprintf("Ensuring that the pod of deployment %s is running and ready", dep.Name))
	labelSelector := labels.SelectorFromSet(labels.Set(podLabels))
	podList, err := e2epod.WaitForPodsWithLabelRunningReady(ctx, c, ns, labelSelector, 1, framework.PodStartTimeout)
	framework.ExpectNoError(err)
	pod := &podList.Items[0]
	return pod
}

func makeDeployment(ns, name, pvcName string, labels map[string]string) *appsv1.Deployment {
	ssReplicas := int32(1)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &ssReplicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: labels,
			},
			Template: v1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
				},
				Spec: v1.PodSpec{
					Containers: []v1.Container{
						{
							Name:  "sts-pod-nginx",
							Image: imageutils.GetE2EImage(imageutils.Agnhost),
							Command: []string{
								"/bin/sh",
								"-c",
								"while true; do echo $(date) >> /mnt/managed/outfile; sleep 1; done",
							},
							VolumeMounts: []v1.VolumeMount{
								{
									Name:      "managed",
									MountPath: "/mnt/managed",
								},
							},
						},
					},
					Volumes: []v1.Volume{
						{
							Name: "managed",
							VolumeSource: v1.VolumeSource{
								PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{
									ClaimName: pvcName,
								},
							},
						},
					},
				},
			},
		},
	}
}
