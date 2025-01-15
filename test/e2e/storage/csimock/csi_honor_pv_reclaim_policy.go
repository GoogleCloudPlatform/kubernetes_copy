/*
Copyright 2024 The Kubernetes Authors.

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

package csimock

import (
	"context"
	"fmt"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	storagehelpers "k8s.io/component-helpers/storage/volume"
	"k8s.io/kubernetes/pkg/features"
	"k8s.io/kubernetes/test/e2e/framework"
	e2epv "k8s.io/kubernetes/test/e2e/framework/pv"
	"k8s.io/kubernetes/test/e2e/storage/utils"
	admissionapi "k8s.io/pod-security-admission/api"
	"k8s.io/utils/ptr"
)

var _ = utils.SIGDescribe("CSI Mock honor pv reclaim policy", framework.WithFeatureGate(features.HonorPVReclaimPolicy), func() {
	f := framework.NewDefaultFramework("csi-mock-honor-pv-reclaim-policy")
	f.NamespacePodSecurityLevel = admissionapi.LevelPrivileged
	m := newMockDriverSetup(f)

	ginkgo.Context("CSI honor pv reclaim policy using mock driver", func() {
		ginkgo.It("Dynamic provisioning should honor pv delete reclaim policy", func(ctx context.Context) {
			m.init(ctx, testParameters{
				registerDriver:             true,
				enableHonorPVReclaimPolicy: true,
				reclaimPolicy:              ptr.To(v1.PersistentVolumeReclaimDelete),
			})
			ginkgo.DeferCleanup(m.cleanup)

			_, pvc := m.createPVC(ctx)

			ginkgo.By(fmt.Sprintf("Waiting for PVC %s to be bound", pvc.Name))
			pvs, err := e2epv.WaitForPVClaimBoundPhase(ctx, f.ClientSet, []*v1.PersistentVolumeClaim{pvc}, framework.ClaimProvisionTimeout)
			framework.ExpectNoError(err, "failed to wait for PVC to be bound")
			gomega.Expect(pvs).To(gomega.HaveLen(1), "expected 1 PV to be bound to PVC, got %d", len(pvs))

			pv := pvs[0]
			ginkgo.By(fmt.Sprintf("PVC %s is bound to PV %s", pvc.Name, pv.Name))
			gomega.Expect(pv.Spec.PersistentVolumeReclaimPolicy).To(gomega.Equal(v1.PersistentVolumeReclaimDelete),
				"expected PV %s to have reclaim policy %s, got %s", pv.Name, v1.PersistentVolumeReclaimDelete, pv.Spec.PersistentVolumeReclaimPolicy)
			// For dynamic provisioning, the PV should be created with the deletion protection finalizer.
			gomega.Expect(pv.Finalizers).To(gomega.ContainElement(storagehelpers.PVDeletionProtectionFinalizer),
				"expected PV %s to have finalizer %s", pv.Name, storagehelpers.PVDeletionProtectionFinalizer)

			ginkgo.By(fmt.Sprintf("Deleting PV %s", pv.Name))
			err = f.ClientSet.CoreV1().PersistentVolumes().Delete(ctx, pv.Name, metav1.DeleteOptions{})
			framework.ExpectNoError(err, "failed to delete PV %s", pv.Name)

			ginkgo.By(fmt.Sprintf("Deleting PVC %s", pvc.Name))
			err = f.ClientSet.CoreV1().PersistentVolumeClaims(pvc.Namespace).Delete(ctx, pvc.Name, metav1.DeleteOptions{})
			framework.ExpectNoError(err, "failed to delete PVC %s", pvc.Name)

			ginkgo.By(fmt.Sprintf("Waiting for PV %s to be deleted", pv.Name))
			err = e2epv.WaitForPersistentVolumeDeleted(ctx, f.ClientSet, pv.Name, framework.Poll, 2*time.Minute)
			framework.ExpectNoError(err, "failed to wait for PV to be deleted")

			ginkgo.By(fmt.Sprintf("Verifying that the driver received DeleteVolume call for PV %s", pv.Name))
			gomega.Expect(m.driver.GetCalls(ctx)).To(gomega.ContainElement(gomega.HaveField("Method", gomega.Equal("DeleteVolume"))))
		})

		ginkgo.It("Dynamic provisioning should honor pv retain reclaim policy", func(ctx context.Context) {
			m.init(ctx, testParameters{
				registerDriver:             true,
				enableHonorPVReclaimPolicy: true,
				reclaimPolicy:              ptr.To(v1.PersistentVolumeReclaimRetain),
			})
			ginkgo.DeferCleanup(m.cleanup)

			_, pvc := m.createPVC(ctx)

			ginkgo.By(fmt.Sprintf("Waiting for PVC %s to be bound", pvc.Name))
			pvs, err := e2epv.WaitForPVClaimBoundPhase(ctx, f.ClientSet, []*v1.PersistentVolumeClaim{pvc}, framework.ClaimProvisionTimeout)
			framework.ExpectNoError(err, "failed to wait for PVC to be bound")
			gomega.Expect(pvs).To(gomega.HaveLen(1), "expected 1 PV to be bound to PVC, got %d", len(pvs))

			pv := pvs[0]
			ginkgo.By(fmt.Sprintf("PVC %s is bound to PV %s", pvc.Name, pv.Name))
			gomega.Expect(pv.Spec.PersistentVolumeReclaimPolicy).To(gomega.Equal(v1.PersistentVolumeReclaimRetain),
				"expected PV %s to have reclaim policy %s, got %s", pv.Name, v1.PersistentVolumeReclaimRetain, pv.Spec.PersistentVolumeReclaimPolicy)

			ginkgo.By(fmt.Sprintf("Verifying that the PV %s does not have finalizer %s after creation", pv.Name, storagehelpers.PVDeletionProtectionFinalizer))
			gomega.Consistently(ctx, framework.GetObject(f.ClientSet.CoreV1().PersistentVolumes().Get, pv.Name, metav1.GetOptions{})).
				WithPolling(framework.Poll).WithTimeout(framework.ClaimProvisionTimeout).ShouldNot(gomega.HaveField("Finalizers",
				gomega.ContainElement(storagehelpers.PVDeletionProtectionFinalizer)), "pv unexpectedly has the finalizer %s", storagehelpers.PVDeletionProtectionFinalizer)

			ginkgo.By(fmt.Sprintf("Deleting PV %s", pv.Name))
			err = f.ClientSet.CoreV1().PersistentVolumes().Delete(ctx, pv.Name, metav1.DeleteOptions{})
			framework.ExpectNoError(err, "failed to delete PV %s", pv.Name)

			ginkgo.By(fmt.Sprintf("Deleting PVC %s", pvc.Name))
			err = f.ClientSet.CoreV1().PersistentVolumeClaims(pvc.Namespace).Delete(ctx, pvc.Name, metav1.DeleteOptions{})
			framework.ExpectNoError(err, "failed to delete PVC %s", pvc.Name)

			ginkgo.By(fmt.Sprintf("Waiting for PV %s to be deleted", pv.Name))
			err = e2epv.WaitForPersistentVolumeDeleted(ctx, f.ClientSet, pv.Name, framework.Poll, 2*time.Minute)
			framework.ExpectNoError(err, "failed to wait for PV to be deleted")

			ginkgo.By(fmt.Sprintf("Verifying that the driver did not receive DeleteVolume call for PV %s", pv.Name))
			gomega.Expect(m.driver.GetCalls(ctx)).NotTo(gomega.ContainElement(gomega.HaveField("Method", gomega.Equal("DeleteVolume"))))
		})

		ginkgo.It("Static provisioning should honor pv delete reclaim policy", func(ctx context.Context) {
			m.init(ctx, testParameters{
				registerDriver:             true,
				enableHonorPVReclaimPolicy: true,
				reclaimPolicy:              ptr.To(v1.PersistentVolumeReclaimDelete),
			})
			ginkgo.DeferCleanup(m.cleanup)

			sc, pv, pvc := m.createPVPVC(ctx)
			gomega.Expect(pv.Spec.PersistentVolumeReclaimPolicy).To(gomega.Equal(v1.PersistentVolumeReclaimDelete),
				"expected PV %s to have reclaim policy %s, got %s", pv.Name, v1.PersistentVolumeReclaimDelete, pv.Spec.PersistentVolumeReclaimPolicy)
			gomega.Expect(pv.Annotations).NotTo(gomega.HaveKeyWithValue(storagehelpers.AnnDynamicallyProvisioned, sc.Provisioner), "expected PV %s to not have annotation %s", pv.Name, storagehelpers.AnnDynamicallyProvisioned)

			ginkgo.By(fmt.Sprintf("Verifying that the PV %s has finalizer %s after creation", pv.Name, storagehelpers.PVDeletionProtectionFinalizer))
			gomega.Eventually(ctx, framework.GetObject(f.ClientSet.CoreV1().PersistentVolumes().Get, pv.Name, metav1.GetOptions{})).
				WithPolling(framework.Poll).WithTimeout(framework.ClaimProvisionTimeout).Should(gomega.HaveField("Finalizers",
				gomega.ContainElement(storagehelpers.PVDeletionProtectionFinalizer)), "failed to wait for PV to have finalizer %s", storagehelpers.PVDeletionProtectionFinalizer)

			ginkgo.By(fmt.Sprintf("Deleting PV %s", pv.Name))
			err := f.ClientSet.CoreV1().PersistentVolumes().Delete(ctx, pv.Name, metav1.DeleteOptions{})
			framework.ExpectNoError(err, "failed to delete PV %s", pv.Name)

			ginkgo.By(fmt.Sprintf("Deleting PVC %s", pvc.Name))
			err = f.ClientSet.CoreV1().PersistentVolumeClaims(pvc.Namespace).Delete(ctx, pvc.Name, metav1.DeleteOptions{})
			framework.ExpectNoError(err, "failed to delete PVC %s", pvc.Name)

			ginkgo.By(fmt.Sprintf("Waiting for PV %s to be deleted", pv.Name))
			err = e2epv.WaitForPersistentVolumeDeleted(ctx, f.ClientSet, pv.Name, framework.Poll, 2*time.Minute)
			framework.ExpectNoError(err, "failed to wait for PV to be deleted")

			ginkgo.By(fmt.Sprintf("Verifying that the driver received DeleteVolume call for PV %s", pv.Name))
			gomega.Expect(m.driver.GetCalls(ctx)).To(gomega.ContainElement(gomega.HaveField("Method", gomega.Equal("DeleteVolume"))))
		})

		ginkgo.It("Static provisioning should honor pv retain reclaim policy", func(ctx context.Context) {
			m.init(ctx, testParameters{
				registerDriver:             true,
				enableHonorPVReclaimPolicy: true,
				reclaimPolicy:              ptr.To(v1.PersistentVolumeReclaimRetain),
			})
			ginkgo.DeferCleanup(m.cleanup)

			sc, pv, pvc := m.createPVPVC(ctx)
			gomega.Expect(pv.Spec.PersistentVolumeReclaimPolicy).To(gomega.Equal(v1.PersistentVolumeReclaimRetain),
				"expected PV %s to have reclaim policy %s, got %s", pv.Name, v1.PersistentVolumeReclaimRetain, pv.Spec.PersistentVolumeReclaimPolicy)
			gomega.Expect(pv.Annotations).NotTo(gomega.HaveKeyWithValue(storagehelpers.AnnDynamicallyProvisioned, sc.Provisioner), "expected PV %s to not have annotation %s", pv.Name, storagehelpers.AnnDynamicallyProvisioned)

			ginkgo.By(fmt.Sprintf("Verifying that the PV %s does not have finalizer %s after creation", pv.Name, storagehelpers.PVDeletionProtectionFinalizer))
			gomega.Consistently(ctx, framework.GetObject(f.ClientSet.CoreV1().PersistentVolumes().Get, pv.Name, metav1.GetOptions{})).
				WithPolling(framework.Poll).WithTimeout(framework.ClaimProvisionTimeout).ShouldNot(gomega.HaveField("Finalizers",
				gomega.ContainElement(storagehelpers.PVDeletionProtectionFinalizer)), "pv unexpectedly has the finalizer %s", storagehelpers.PVDeletionProtectionFinalizer)

			ginkgo.By(fmt.Sprintf("Deleting PV %s", pv.Name))
			err := f.ClientSet.CoreV1().PersistentVolumes().Delete(ctx, pv.Name, metav1.DeleteOptions{})
			framework.ExpectNoError(err, "failed to delete PV %s", pv.Name)

			ginkgo.By(fmt.Sprintf("Deleting PVC %s", pvc.Name))
			err = f.ClientSet.CoreV1().PersistentVolumeClaims(pvc.Namespace).Delete(ctx, pvc.Name, metav1.DeleteOptions{})
			framework.ExpectNoError(err, "failed to delete PVC %s", pvc.Name)

			ginkgo.By(fmt.Sprintf("Waiting for PV %s to be deleted", pv.Name))
			err = e2epv.WaitForPersistentVolumeDeleted(ctx, f.ClientSet, pv.Name, framework.Poll, 2*time.Minute)
			framework.ExpectNoError(err, "failed to wait for PV to be deleted")

			ginkgo.By(fmt.Sprintf("Verifying that the driver did not receive DeleteVolume call for PV %s", pv.Name))
			gomega.Expect(m.driver.GetCalls(ctx)).NotTo(gomega.ContainElement(gomega.HaveField("Method", gomega.Equal("DeleteVolume"))))
		})
	})
})
