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

package storage

import (
	"context"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	"fmt"
	"time"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"
	volumeutil "k8s.io/kubernetes/pkg/volume/util"
	"k8s.io/kubernetes/test/e2e/framework"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2epv "k8s.io/kubernetes/test/e2e/framework/pv"
	"k8s.io/kubernetes/test/e2e/storage/testsuites"
	"k8s.io/kubernetes/test/e2e/storage/utils"
	admissionapi "k8s.io/pod-security-admission/api"
)

const (
	// claimDeletingTimeout is How long claims have to become deleted.
	claimDeletingTimeout = 3 * time.Minute
)

// waitForPersistentVolumeClaimDeleted waits for a PersistentVolumeClaim to be removed from the system until timeout occurs, whichever comes first.
func waitForPersistentVolumeClaimDeleted(ctx context.Context, c clientset.Interface, ns string, pvcName string, Poll, timeout time.Duration) error {
	framework.Logf("Waiting up to %v for PersistentVolumeClaim %s to be removed", timeout, pvcName)
	for start := time.Now(); time.Since(start) < timeout; time.Sleep(Poll) {
		_, err := c.CoreV1().PersistentVolumeClaims(ns).Get(ctx, pvcName, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				framework.Logf("Claim %q in namespace %q doesn't exist in the system", pvcName, ns)
				return nil
			}
			framework.Logf("Failed to get claim %q in namespace %q, retrying in %v. Error: %v", pvcName, ns, Poll, err)
		}
	}
	return fmt.Errorf("PersistentVolumeClaim %s is not removed from the system within %v", pvcName, timeout)
}

var _ = utils.SIGDescribe("PVC Protection", func() {
	var (
		client                  clientset.Interface
		nameSpace               string
		err                     error
		pvc                     *v1.PersistentVolumeClaim
		pvcCreatedAndNotDeleted bool
		pod                     *v1.Pod
	)

	f := framework.NewDefaultFramework("pvc-protection")
	f.NamespacePodSecurityLevel = admissionapi.LevelBaseline
	ginkgo.BeforeEach(func(ctx context.Context) {
		client = f.ClientSet
		nameSpace = f.Namespace.Name
		framework.ExpectNoError(e2enode.WaitForAllNodesSchedulable(ctx, client, f.Timeouts.NodeSchedulable))

		ginkgo.By("Creating a PVC")
		prefix := "pvc-protection"
		e2epv.SkipIfNoDefaultStorageClass(ctx, client)
		t := testsuites.StorageClassTest{
			Timeouts:  f.Timeouts,
			ClaimSize: "1Gi",
		}
		pvc = e2epv.MakePersistentVolumeClaim(e2epv.PersistentVolumeClaimConfig{
			NamePrefix: prefix,
			ClaimSize:  t.ClaimSize,
			VolumeMode: &t.VolumeMode,
		}, nameSpace)
		pvc, err = client.CoreV1().PersistentVolumeClaims(pvc.Namespace).Create(ctx, pvc, metav1.CreateOptions{})
		framework.ExpectNoError(err, "Error creating PVC")
		pvcCreatedAndNotDeleted = true

		ginkgo.By("Creating a Pod that becomes Running and therefore is actively using the PVC")
		pvcClaims := []*v1.PersistentVolumeClaim{pvc}
		pod, err = e2epod.CreatePod(ctx, client, nameSpace, nil, pvcClaims, f.NamespacePodSecurityLevel, "")
		framework.ExpectNoError(err, "While creating pod that uses the PVC or waiting for the Pod to become Running")

		ginkgo.By("Waiting for PVC to become Bound")
		err = e2epv.WaitForPersistentVolumeClaimPhase(ctx, v1.ClaimBound, client, nameSpace, pvc.Name, framework.Poll, f.Timeouts.ClaimBound)
		framework.ExpectNoError(err, "Failed waiting for PVC to be bound %v", err)

		ginkgo.By("Checking that PVC Protection finalizer is set")
		pvc, err = client.CoreV1().PersistentVolumeClaims(pvc.Namespace).Get(ctx, pvc.Name, metav1.GetOptions{})
		framework.ExpectNoError(err, "While getting PVC status")
		gomega.Expect(pvc.ObjectMeta.Finalizers).Should(gomega.ContainElement(volumeutil.PVCProtectionFinalizer), "PVC Protection finalizer(%v) is not set in %v", volumeutil.PVCProtectionFinalizer, pvc.ObjectMeta.Finalizers)
	})

	ginkgo.AfterEach(func(ctx context.Context) {
		if pvcCreatedAndNotDeleted {
			e2epv.DeletePersistentVolumeClaim(ctx, client, pvc.Name, nameSpace)
		}
	})

	ginkgo.It("Verify \"immediate\" deletion of a PVC that is not in active use by a pod", func(ctx context.Context) {
		ginkgo.By("Deleting the pod using the PVC")
		err = e2epod.DeletePodWithWait(ctx, client, pod)
		framework.ExpectNoError(err, "Error terminating and deleting pod")

		ginkgo.By("Deleting the PVC")
		err = client.CoreV1().PersistentVolumeClaims(pvc.Namespace).Delete(ctx, pvc.Name, *metav1.NewDeleteOptions(0))
		framework.ExpectNoError(err, "Error deleting PVC")
		waitForPersistentVolumeClaimDeleted(ctx, client, pvc.Namespace, pvc.Name, framework.Poll, claimDeletingTimeout)
		pvcCreatedAndNotDeleted = false
	})

	ginkgo.It("Verify that PVC in active use by a pod is not removed immediately", func(ctx context.Context) {
		ginkgo.By("Deleting the PVC, however, the PVC must not be removed from the system as it's in active use by a pod")
		err = client.CoreV1().PersistentVolumeClaims(pvc.Namespace).Delete(ctx, pvc.Name, *metav1.NewDeleteOptions(0))
		framework.ExpectNoError(err, "Error deleting PVC")

		ginkgo.By("Checking that the PVC status is Terminating")
		pvc, err = client.CoreV1().PersistentVolumeClaims(pvc.Namespace).Get(ctx, pvc.Name, metav1.GetOptions{})
		framework.ExpectNoError(err, "While checking PVC status")
		gomega.Expect(pvc.ObjectMeta.DeletionTimestamp).ToNot(gomega.BeNil())

		ginkgo.By("Deleting the pod that uses the PVC")
		err = e2epod.DeletePodWithWait(ctx, client, pod)
		framework.ExpectNoError(err, "Error terminating and deleting pod")

		ginkgo.By("Checking that the PVC is automatically removed from the system because it's no longer in active use by a pod")
		waitForPersistentVolumeClaimDeleted(ctx, client, pvc.Namespace, pvc.Name, framework.Poll, claimDeletingTimeout)
		pvcCreatedAndNotDeleted = false
	})

	ginkgo.It("Verify that scheduling of a pod that uses PVC that is being deleted fails and the pod becomes Unschedulable", func(ctx context.Context) {
		ginkgo.By("Deleting the PVC, however, the PVC must not be removed from the system as it's in active use by a pod")
		err = client.CoreV1().PersistentVolumeClaims(pvc.Namespace).Delete(ctx, pvc.Name, *metav1.NewDeleteOptions(0))
		framework.ExpectNoError(err, "Error deleting PVC")

		ginkgo.By("Checking that the PVC status is Terminating")
		pvc, err = client.CoreV1().PersistentVolumeClaims(pvc.Namespace).Get(ctx, pvc.Name, metav1.GetOptions{})
		framework.ExpectNoError(err, "While checking PVC status")
		gomega.Expect(pvc.ObjectMeta.DeletionTimestamp).ToNot(gomega.BeNil())

		ginkgo.By("Creating second Pod whose scheduling fails because it uses a PVC that is being deleted")
		secondPod, err2 := e2epod.CreateUnschedulablePod(ctx, client, nameSpace, nil, []*v1.PersistentVolumeClaim{pvc}, f.NamespacePodSecurityLevel, "")
		framework.ExpectNoError(err2, "While creating second pod that uses a PVC that is being deleted and that is Unschedulable")

		ginkgo.By("Deleting the second pod that uses the PVC that is being deleted")
		err = e2epod.DeletePodWithWait(ctx, client, secondPod)
		framework.ExpectNoError(err, "Error terminating and deleting pod")

		ginkgo.By("Checking again that the PVC status is Terminating")
		pvc, err = client.CoreV1().PersistentVolumeClaims(pvc.Namespace).Get(ctx, pvc.Name, metav1.GetOptions{})
		framework.ExpectNoError(err, "While checking PVC status")
		gomega.Expect(pvc.ObjectMeta.DeletionTimestamp).ToNot(gomega.BeNil())

		ginkgo.By("Deleting the first pod that uses the PVC")
		err = e2epod.DeletePodWithWait(ctx, client, pod)
		framework.ExpectNoError(err, "Error terminating and deleting pod")

		ginkgo.By("Checking that the PVC is automatically removed from the system because it's no longer in active use by a pod")
		waitForPersistentVolumeClaimDeleted(ctx, client, pvc.Namespace, pvc.Name, framework.Poll, claimDeletingTimeout)
		pvcCreatedAndNotDeleted = false
	})

	ginkgo.It("Verify deletion of a PVC that is not in active use by a pod with \"Failed\" phase", func(ctx context.Context) {
		ginkgo.By("Deleting the PVC, however, the PVC must not be removed from the system as it's in active use by a pod")
		err = client.CoreV1().PersistentVolumeClaims(pvc.Namespace).Delete(ctx, pvc.Name, *metav1.NewDeleteOptions(0))
		framework.ExpectNoError(err, "Error deleting PVC")

		ginkgo.By("Checking that the PVC status is Terminating")
		pvc, err = client.CoreV1().PersistentVolumeClaims(pvc.Namespace).Get(ctx, pvc.Name, metav1.GetOptions{})
		framework.ExpectNoError(err, "While checking PVC status")
		gomega.Expect(pvc.DeletionTimestamp).ToNot(gomega.BeNil())

		ginkgo.By("Updating the pod for ActiveDeadlineSeconds")
		// Use ActiveDeadlineSeconds to make sure pod phase to "Failed"
		newDeadline := int64(5)
		pod.Spec.ActiveDeadlineSeconds = &newDeadline
		pod, err = client.CoreV1().Pods(pod.Namespace).Update(ctx, pod, metav1.UpdateOptions{})
		framework.ExpectNoError(err, "While updating pod spec for ActiveDeadlineSeconds")
		framework.ExpectNoError(e2epod.WaitForPodTerminatedInNamespace(ctx, f.ClientSet, pod.Name, "DeadlineExceeded", pod.Namespace))

		ginkgo.By("Checking that the PVC is automatically removed from the system because it's no longer in active use by a pod with \"Failed\" phase")
		err = waitForPersistentVolumeClaimDeleted(ctx, client, pvc.Namespace, pvc.Name, framework.Poll, claimDeletingTimeout)
		framework.ExpectNoError(err, "Error deleting PVC")

		ginkgo.By("Deleting the pod with \"Failed\" phase")
		err = e2epod.DeletePodWithWait(ctx, client, pod)
		framework.ExpectNoError(err, "Error deleting pod")
		pvcCreatedAndNotDeleted = false
	})

	ginkgo.It("Verify deletion of a PVC that is not in active use by a pod with \"Succeeded\" phase", func(ctx context.Context) {
		// Delete the pods created during the preprocessing phase, because the pods for custom commands will be created next.
		ginkgo.By("Deleting the pod that uses the PVC")
		err = e2epod.DeletePodWithWait(ctx, client, pod)
		framework.ExpectNoError(err, "Error deleting pod")

		pod, err = e2epod.CreatePod(ctx, client, nameSpace, nil, []*v1.PersistentVolumeClaim{pvc}, f.NamespacePodSecurityLevel, "sleep 60; exit 0")
		framework.ExpectNoError(err, "While creating pod that uses the PVC or waiting for the Pod to become Running")

		ginkgo.By("Deleting the PVC, however, the PVC must not be removed from the system as it's in active use by a pod")
		err = client.CoreV1().PersistentVolumeClaims(pvc.Namespace).Delete(ctx, pvc.Name, *metav1.NewDeleteOptions(0))
		framework.ExpectNoError(err, "Error deleting PVC")

		ginkgo.By("Checking that the PVC status is Terminating")
		pvc, err = client.CoreV1().PersistentVolumeClaims(pvc.Namespace).Get(ctx, pvc.Name, metav1.GetOptions{})
		framework.ExpectNoError(err, "While checking PVC status")
		gomega.Expect(pvc.DeletionTimestamp).ToNot(gomega.BeNil())

		ginkgo.By("Checking that the PVC is automatically removed from the system because it's no longer in active use by a pod with \"Succeeded\" phase")
		err = waitForPersistentVolumeClaimDeleted(ctx, client, pvc.Namespace, pvc.Name, framework.Poll, claimDeletingTimeout)
		framework.ExpectNoError(err, "Error deleting PVC")

		ginkgo.By("Deleting the pod with \"Succeeded\" phase")
		err = e2epod.DeletePodWithWait(ctx, client, pod)
		framework.ExpectNoError(err, "Error deleting pod")
		pvcCreatedAndNotDeleted = false
	})
})
