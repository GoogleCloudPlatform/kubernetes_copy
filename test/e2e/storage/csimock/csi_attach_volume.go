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

package csimock

import (
	"context"
	"fmt"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	v1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/kubernetes/pkg/kubelet/events"
	"k8s.io/kubernetes/test/e2e/framework"
	e2eevents "k8s.io/kubernetes/test/e2e/framework/events"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2evolume "k8s.io/kubernetes/test/e2e/framework/volume"
	storageframework "k8s.io/kubernetes/test/e2e/storage/framework"
	"k8s.io/kubernetes/test/e2e/storage/utils"
	admissionapi "k8s.io/pod-security-admission/api"
)

var _ = utils.SIGDescribe("CSI Mock volume attach", func() {
	// The CSIDriverRegistry feature gate is needed for this test in Kubernetes 1.12.
	f := framework.NewDefaultFramework("csi-mock-volumes-attach")
	f.NamespacePodSecurityLevel = admissionapi.LevelPrivileged
	m := newMockDriverSetup(f)

	ginkgo.Context("CSI attach test using mock driver", func() {
		tests := []struct {
			name                   string
			disableAttach          bool
			deployClusterRegistrar bool
			volumeType             volumeType
		}{
			{
				name:                   "should not require VolumeAttach for drivers without attachment",
				disableAttach:          true,
				deployClusterRegistrar: true,
			},
			{
				name:                   "should require VolumeAttach for drivers with attachment",
				deployClusterRegistrar: true,
			},
			{
				name:                   "should require VolumeAttach for ephemermal volume and drivers with attachment",
				deployClusterRegistrar: true,
				volumeType:             genericEphemeral,
			},
			{
				name:                   "should preserve attachment policy when no CSIDriver present",
				deployClusterRegistrar: false,
			},
		}
		for _, t := range tests {
			test := t
			ginkgo.It(t.name, func(ctx context.Context) {
				var err error
				m.init(ctx, testParameters{registerDriver: test.deployClusterRegistrar, disableAttach: test.disableAttach})
				ginkgo.DeferCleanup(m.cleanup)

				volumeType := test.volumeType
				if volumeType == "" {
					volumeType = pvcReference
				}
				_, claim, pod := m.createPod(ctx, volumeType)
				if pod == nil {
					return
				}
				err = e2epod.WaitForPodNameRunningInNamespace(ctx, m.cs, pod.Name, pod.Namespace)
				framework.ExpectNoError(err, "Failed to start pod: %v", err)

				ginkgo.By("Checking if VolumeAttachment was created for the pod")
				testConfig := storageframework.ConvertTestConfig(m.config)
				attachmentName := e2evolume.GetVolumeAttachmentName(ctx, m.cs, testConfig, m.provisioner, claim.Name, claim.Namespace)
				_, err = m.cs.StorageV1().VolumeAttachments().Get(context.TODO(), attachmentName, metav1.GetOptions{})
				if err != nil {
					if apierrors.IsNotFound(err) {
						if !test.disableAttach {
							framework.ExpectNoError(err, "Expected VolumeAttachment but none was found")
						}
					} else {
						framework.ExpectNoError(err, "Failed to find VolumeAttachment")
					}
				}
				if test.disableAttach {
					gomega.Expect(err).To(gomega.MatchError(apierrors.IsNotFound, "Unexpected VolumeAttachment found"))
				}
			})

		}
	})

	ginkgo.Context("When CSIDriver AttachRequired changes from true to false", func() {
		f.It("should cleanup VolumeAttachment properly", f.WithSlow(), func(ctx context.Context) {
			var err error
			m.init(ctx, testParameters{registerDriver: true, disableAttach: false})
			ginkgo.DeferCleanup(m.cleanup)

			driverName := m.config.GetUniqueDriverName()
			csiDriver, err := m.cs.StorageV1().CSIDrivers().Get(ctx, driverName, metav1.GetOptions{})
			framework.ExpectNoError(err, "Failed to get CSIDriver: %v", err)
			driver := csiDriver.DeepCopy()

			_, claim, pod := m.createPod(ctx, pvcReference)
			if pod == nil {
				return
			}
			testConfig := storageframework.ConvertTestConfig(m.config)
			attachmentName := e2evolume.GetVolumeAttachmentName(ctx, m.cs, testConfig, m.provisioner, claim.Name, claim.Namespace)
			ginkgo.DeferCleanup(func(ctx context.Context) {
				_, err := m.cs.StorageV1().VolumeAttachments().Get(ctx, attachmentName, metav1.GetOptions{})
				if err == nil {
					err := m.cs.StorageV1().VolumeAttachments().Delete(ctx, attachmentName, metav1.DeleteOptions{})
					framework.ExpectNoError(err, "Failed to delete VolumeAttachment: %v", err)
				} else if !apierrors.IsNotFound(err) {
					framework.ExpectNoError(err, "Failed to get VolumeAttachment")
				}
			})

			// Use a short interval to quickly detect VolumeAttachment creation and immediately recreate the CSIDriver
			interval := 200 * time.Millisecond
			ginkgo.By("Checking if VolumeAttachment was created for the pod")
			err = e2evolume.WaitForVolumeAttachmentCreated(ctx, attachmentName, m.cs, interval, csiVolumeAttachmentTimeout)
			framework.ExpectNoError(err, "Failed to wait for VolumeAttachment %s to be created: %v", attachmentName, err)
			ginkgo.By("Delete CSIDriver object")
			err = m.cs.StorageV1().CSIDrivers().Delete(ctx, driverName, metav1.DeleteOptions{})
			framework.ExpectNoError(err, "Failed to delete CSIDriver: %v", err)
			err = waitForCSIDriverDeleted(ctx, m.cs, driverName, interval, csiDriverTimeout)
			framework.ExpectNoError(err, "Failed to delete CSIDriver: %v", err)

			attachRequired := false
			driver.Spec.AttachRequired = &attachRequired
			newDriver := &storagev1.CSIDriver{
				ObjectMeta: metav1.ObjectMeta{
					Name: driverName,
				},
				Spec: driver.Spec,
			}
			ginkgo.By("Recreating CSIDriver with AttachRequired=false")
			_, err = m.cs.StorageV1().CSIDrivers().Create(ctx, newDriver, metav1.CreateOptions{})
			framework.ExpectNoError(err, "Failed to create CSIDriver: %v", err)
			err = waitForCSIDriver(m.cs, driverName)
			framework.ExpectNoError(err, "Failed to get CSIDriver: %v", err)

			ginkgo.By("Wait for the volumeattachment to be deleted")
			err = e2evolume.WaitForVolumeAttachmentTerminated(ctx, attachmentName, m.cs, csiVolumeAttachmentTimeout)
			framework.ExpectNoError(err, "Failed to delete VolumeAttachment: %v", err)
			err = e2epod.WaitForPodNameRunningInNamespace(ctx, m.cs, pod.Name, pod.Namespace)
			framework.ExpectNoError(err, "Failed to start pod: %v", err)
		})
	})

	ginkgo.Context("CSI CSIDriver deployment after pod creation using non-attachable mock driver", func() {
		f.It("should bringup pod after deploying CSIDriver attach=false", f.WithSlow(), func(ctx context.Context) {
			var err error
			m.init(ctx, testParameters{registerDriver: false, disableAttach: true})
			ginkgo.DeferCleanup(m.cleanup)

			_, claim, pod := m.createPod(ctx, pvcReference) // late binding as specified above
			if pod == nil {
				return
			}

			ginkgo.By("Checking if attaching failed and pod cannot start")
			eventSelector := fields.Set{
				"involvedObject.kind":      "Pod",
				"involvedObject.name":      pod.Name,
				"involvedObject.namespace": pod.Namespace,
				"reason":                   events.FailedAttachVolume,
			}.AsSelector().String()
			msg := "AttachVolume.Attach failed for volume"

			err = e2eevents.WaitTimeoutForEvent(ctx, m.cs, pod.Namespace, eventSelector, msg, f.Timeouts.PodStart)
			if err != nil {
				getPod := e2epod.Get(m.cs, pod)
				gomega.Consistently(ctx, getPod).WithTimeout(10*time.Second).Should(e2epod.BeInPhase(v1.PodPending),
					"Pod should not be in running status because attaching should failed")
				// Events are unreliable, don't depend on the event. It's used only to speed up the test.
				framework.Logf("Attach should fail and the corresponding event should show up, error: %v", err)
			}

			// VolumeAttachment should be created because the default value for CSI attachable is true
			ginkgo.By("Checking if VolumeAttachment was created for the pod")
			testConfig := storageframework.ConvertTestConfig(m.config)
			attachmentName := e2evolume.GetVolumeAttachmentName(ctx, m.cs, testConfig, m.provisioner, claim.Name, claim.Namespace)
			_, err = m.cs.StorageV1().VolumeAttachments().Get(context.TODO(), attachmentName, metav1.GetOptions{})
			if err != nil {
				if apierrors.IsNotFound(err) {
					framework.ExpectNoError(err, "Expected VolumeAttachment but none was found")
				} else {
					framework.ExpectNoError(err, "Failed to find VolumeAttachment")
				}
			}

			ginkgo.By("Deploy CSIDriver object with attachRequired=false")
			driverNamespace := m.config.DriverNamespace

			canAttach := false
			o := utils.PatchCSIOptions{
				OldDriverName: "csi-mock",
				NewDriverName: "csi-mock-" + f.UniqueName,
				CanAttach:     &canAttach,
			}
			err = utils.CreateFromManifests(ctx, f, driverNamespace, func(item interface{}) error {
				return utils.PatchCSIDeployment(f, o, item)
			}, "test/e2e/testing-manifests/storage-csi/mock/csi-mock-driverinfo.yaml")
			if err != nil {
				framework.Failf("fail to deploy CSIDriver object: %v", err)
			}

			ginkgo.By("Wait for the pod in running status")
			err = e2epod.WaitForPodNameRunningInNamespace(ctx, m.cs, pod.Name, pod.Namespace)
			framework.ExpectNoError(err, "Failed to start pod: %v", err)

			ginkgo.By(fmt.Sprintf("Wait for the volumeattachment to be deleted up to %v", csiVolumeAttachmentTimeout))
			// This step can be slow because we have to wait either a NodeUpdate event happens or
			// the detachment for this volume timeout so that we can do a force detach.
			err = e2evolume.WaitForVolumeAttachmentTerminated(ctx, attachmentName, m.cs, csiVolumeAttachmentTimeout)
			framework.ExpectNoError(err, "Failed to delete VolumeAttachment: %v", err)
		})
	})
})
