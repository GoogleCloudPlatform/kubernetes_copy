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
	"fmt"
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	v1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/component-base/metrics/testutil"
	"k8s.io/component-helpers/storage/ephemeral"
	"k8s.io/kubernetes/pkg/features"
	kubeletmetrics "k8s.io/kubernetes/pkg/kubelet/metrics"
	"k8s.io/kubernetes/test/e2e/feature"
	"k8s.io/kubernetes/test/e2e/framework"
	e2emetrics "k8s.io/kubernetes/test/e2e/framework/metrics"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2epv "k8s.io/kubernetes/test/e2e/framework/pv"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
	"k8s.io/kubernetes/test/e2e/storage/testsuites"
	"k8s.io/kubernetes/test/e2e/storage/utils"
	admissionapi "k8s.io/pod-security-admission/api"
)

// This test needs to run in serial because other tests could interfere
// with metrics being tested here.
var _ = utils.SIGDescribe(framework.WithSerial(), "Volume metrics", func() {
	var (
		c              clientset.Interface
		ns             string
		pvc            *v1.PersistentVolumeClaim
		pvcBlock       *v1.PersistentVolumeClaim
		metricsGrabber *e2emetrics.Grabber
		invalidSc      *storagev1.StorageClass
		defaultScName  string
		err            error
	)
	f := framework.NewDefaultFramework("pv")
	f.NamespacePodSecurityLevel = admissionapi.LevelBaseline

	ginkgo.BeforeEach(func(ctx context.Context) {
		c = f.ClientSet
		ns = f.Namespace.Name
		var err error

		// The tests below make various assumptions about the cluster
		// and the underlying storage driver and therefore don't pass
		// with other kinds of clusters and drivers.
		e2eskipper.SkipUnlessProviderIs("gce", "aws")
		e2epv.SkipIfNoDefaultStorageClass(ctx, c)
		defaultScName, err = e2epv.GetDefaultStorageClassName(ctx, c)
		framework.ExpectNoError(err)

		test := testsuites.StorageClassTest{
			Name:      "default",
			Timeouts:  f.Timeouts,
			ClaimSize: "2Gi",
		}

		fsMode := v1.PersistentVolumeFilesystem
		pvc = e2epv.MakePersistentVolumeClaim(e2epv.PersistentVolumeClaimConfig{
			ClaimSize:  test.ClaimSize,
			VolumeMode: &fsMode,
		}, ns)

		// selected providers all support PersistentVolumeBlock
		blockMode := v1.PersistentVolumeBlock
		pvcBlock = e2epv.MakePersistentVolumeClaim(e2epv.PersistentVolumeClaimConfig{
			ClaimSize:  test.ClaimSize,
			VolumeMode: &blockMode,
		}, ns)

		metricsGrabber, err = e2emetrics.NewMetricsGrabber(ctx, c, nil, f.ClientConfig(), true, false, true, false, false, false)

		if err != nil {
			framework.Failf("Error creating metrics grabber : %v", err)
		}
	})

	ginkgo.AfterEach(func(ctx context.Context) {
		newPvc, err := c.CoreV1().PersistentVolumeClaims(pvc.Namespace).Get(ctx, pvc.Name, metav1.GetOptions{})
		if err != nil {
			framework.Logf("Failed to get pvc %s/%s: %v", pvc.Namespace, pvc.Name, err)
		} else {
			e2epv.DeletePersistentVolumeClaim(ctx, c, newPvc.Name, newPvc.Namespace)
			if newPvc.Spec.VolumeName != "" {
				err = e2epv.WaitForPersistentVolumeDeleted(ctx, c, newPvc.Spec.VolumeName, 5*time.Second, 5*time.Minute)
				framework.ExpectNoError(err, "Persistent Volume %v not deleted by dynamic provisioner", newPvc.Spec.VolumeName)
			}
		}

		if invalidSc != nil {
			err := c.StorageV1().StorageClasses().Delete(ctx, invalidSc.Name, metav1.DeleteOptions{})
			framework.ExpectNoError(err, "Error deleting storageclass %v: %v", invalidSc.Name, err)
			invalidSc = nil
		}
	})

	provisioning := func(ctx context.Context, ephemeral bool) {
		if !metricsGrabber.HasControlPlanePods() {
			e2eskipper.Skipf("Environment does not support getting controller-manager metrics - skipping")
		}

		ginkgo.By("Getting plugin name")
		defaultClass, err := c.StorageV1().StorageClasses().Get(ctx, defaultScName, metav1.GetOptions{})
		framework.ExpectNoError(err, "Error getting default storageclass: %v", err)
		pluginName := defaultClass.Provisioner

		controllerMetrics, err := metricsGrabber.GrabFromControllerManager(ctx)

		framework.ExpectNoError(err, "Error getting c-m metrics : %v", err)

		storageOpMetrics := getControllerStorageMetrics(controllerMetrics, pluginName)

		if !ephemeral {
			pvc, err = c.CoreV1().PersistentVolumeClaims(pvc.Namespace).Create(ctx, pvc, metav1.CreateOptions{})
			framework.ExpectNoError(err)
			gomega.Expect(pvc).ToNot(gomega.BeNil())
		}

		pod := makePod(f, pvc, ephemeral)
		pod, err = c.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{})
		framework.ExpectNoError(err)

		err = e2epod.WaitTimeoutForPodRunningInNamespace(ctx, c, pod.Name, pod.Namespace, f.Timeouts.PodStart)
		framework.ExpectNoError(err, "Error starting pod %s", pod.Name)

		framework.ExpectNoError(e2epod.DeletePodWithWait(ctx, c, pod))

		updatedStorageMetrics := waitForDetachAndGrabMetrics(ctx, storageOpMetrics, metricsGrabber, pluginName)

		gomega.Expect(updatedStorageMetrics.latencyMetrics).ToNot(gomega.BeEmpty(), "Error fetching c-m updated storage metrics")
		gomega.Expect(updatedStorageMetrics.statusMetrics).ToNot(gomega.BeEmpty(), "Error fetching c-m updated storage metrics")

		volumeOperations := []string{"volume_detach", "volume_attach"}

		for _, volumeOp := range volumeOperations {
			verifyMetricCount(storageOpMetrics, updatedStorageMetrics, volumeOp, false)
		}
	}

	provisioningError := func(ctx context.Context, ephemeral bool) {
		if !metricsGrabber.HasControlPlanePods() {
			e2eskipper.Skipf("Environment does not support getting controller-manager metrics - skipping")
		}

		ginkgo.By("Getting default storageclass")
		defaultClass, err := c.StorageV1().StorageClasses().Get(ctx, defaultScName, metav1.GetOptions{})
		framework.ExpectNoError(err, "Error getting default storageclass: %v", err)
		pluginName := defaultClass.Provisioner

		invalidSc = &storagev1.StorageClass{
			ObjectMeta: metav1.ObjectMeta{
				Name: fmt.Sprintf("fail-metrics-invalid-sc-%s", pvc.Namespace),
			},
			Provisioner: defaultClass.Provisioner,
			Parameters: map[string]string{
				"invalidparam": "invalidvalue",
			},
		}
		_, err = c.StorageV1().StorageClasses().Create(ctx, invalidSc, metav1.CreateOptions{})
		framework.ExpectNoError(err, "Error creating new storageclass: %v", err)

		pvc.Spec.StorageClassName = &invalidSc.Name
		if !ephemeral {
			pvc, err = c.CoreV1().PersistentVolumeClaims(pvc.Namespace).Create(ctx, pvc, metav1.CreateOptions{})
			framework.ExpectNoError(err, "failed to create PVC %s/%s", pvc.Namespace, pvc.Name)
			gomega.Expect(pvc).ToNot(gomega.BeNil())
		}

		ginkgo.By("Creating a pod and expecting it to fail")
		pod := makePod(f, pvc, ephemeral)
		pod, err = c.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{})
		framework.ExpectNoError(err, "failed to create Pod %s/%s", pod.Namespace, pod.Name)

		getPod := e2epod.Get(f.ClientSet, pod)
		gomega.Consistently(ctx, getPod, f.Timeouts.PodStart, 2*time.Second).ShouldNot(e2epod.BeInPhase(v1.PodRunning))

		framework.Logf("Deleting pod %q/%q", pod.Namespace, pod.Name)
		framework.ExpectNoError(e2epod.DeletePodWithWait(ctx, c, pod))

		ginkgo.By("Checking failure metrics")
		updatedControllerMetrics, err := metricsGrabber.GrabFromControllerManager(ctx)
		framework.ExpectNoError(err, "failed to get controller manager metrics")
		updatedStorageMetrics := getControllerStorageMetrics(updatedControllerMetrics, pluginName)

		gomega.Expect(updatedStorageMetrics.statusMetrics).ToNot(gomega.BeEmpty(), "Error fetching c-m updated storage metrics")
	}

	filesystemMode := func(ctx context.Context, isEphemeral bool) {
		if !isEphemeral {
			pvc, err = c.CoreV1().PersistentVolumeClaims(pvc.Namespace).Create(ctx, pvc, metav1.CreateOptions{})
			framework.ExpectNoError(err)
			gomega.Expect(pvc).ToNot(gomega.BeNil())
		}

		pod := makePod(f, pvc, isEphemeral)
		pod, err = c.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{})
		framework.ExpectNoError(err)

		err = e2epod.WaitTimeoutForPodRunningInNamespace(ctx, c, pod.Name, pod.Namespace, f.Timeouts.PodStart)
		framework.ExpectNoError(err, "Error starting pod %s", pod.Name)

		pod, err = c.CoreV1().Pods(ns).Get(ctx, pod.Name, metav1.GetOptions{})
		framework.ExpectNoError(err)

		pvcName := pvc.Name
		if isEphemeral {
			pvcName = ephemeral.VolumeClaimName(pod, &pod.Spec.Volumes[0])
		}
		pvcNamespace := pod.Namespace

		// Verify volume stat metrics were collected for the referenced PVC
		volumeStatKeys := []string{
			kubeletmetrics.VolumeStatsUsedBytesKey,
			kubeletmetrics.VolumeStatsCapacityBytesKey,
			kubeletmetrics.VolumeStatsAvailableBytesKey,
			kubeletmetrics.VolumeStatsInodesKey,
			kubeletmetrics.VolumeStatsInodesFreeKey,
			kubeletmetrics.VolumeStatsInodesUsedKey,
		}
		key := volumeStatKeys[0]
		kubeletKeyName := fmt.Sprintf("%s_%s", kubeletmetrics.KubeletSubsystem, key)
		// Poll kubelet metrics waiting for the volume to be picked up
		// by the volume stats collector
		var kubeMetrics e2emetrics.KubeletMetrics
		waitErr := wait.PollUntilContextTimeout(ctx, 30*time.Second, 5*time.Minute, false, func(ctx context.Context) (bool, error) {
			framework.Logf("Grabbing Kubelet metrics")
			// Grab kubelet metrics from the node the pod was scheduled on
			var err error
			kubeMetrics, err = metricsGrabber.GrabFromKubelet(ctx, pod.Spec.NodeName)
			if err != nil {
				framework.Logf("Error fetching kubelet metrics")
				return false, err
			}
			if !findVolumeStatMetric(kubeletKeyName, pvcNamespace, pvcName, kubeMetrics) {
				return false, nil
			}
			return true, nil
		})
		framework.ExpectNoError(waitErr, "Unable to find metric %s for PVC %s/%s", kubeletKeyName, pvcNamespace, pvcName)

		for _, key := range volumeStatKeys {
			kubeletKeyName := fmt.Sprintf("%s_%s", kubeletmetrics.KubeletSubsystem, key)
			found := findVolumeStatMetric(kubeletKeyName, pvcNamespace, pvcName, kubeMetrics)
			if !found {
				framework.Failf("PVC %s, Namespace %s not found for %s", pvcName, pvcNamespace, kubeletKeyName)
			}
		}

		framework.Logf("Deleting pod %q/%q", pod.Namespace, pod.Name)
		framework.ExpectNoError(e2epod.DeletePodWithWait(ctx, c, pod))
	}

	blockmode := func(ctx context.Context, isEphemeral bool) {
		if !isEphemeral {
			pvcBlock, err = c.CoreV1().PersistentVolumeClaims(pvcBlock.Namespace).Create(ctx, pvcBlock, metav1.CreateOptions{})
			framework.ExpectNoError(err)
			gomega.Expect(pvcBlock).ToNot(gomega.BeNil())
		}

		pod := makePod(f, pvcBlock, isEphemeral)
		pod.Spec.Containers[0].VolumeDevices = []v1.VolumeDevice{{
			Name:       pod.Spec.Volumes[0].Name,
			DevicePath: "/mnt/" + pod.Spec.Volumes[0].Name,
		}}
		pod.Spec.Containers[0].VolumeMounts = nil
		pod, err = c.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{})
		framework.ExpectNoError(err)

		err = e2epod.WaitTimeoutForPodRunningInNamespace(ctx, c, pod.Name, pod.Namespace, f.Timeouts.PodStart)
		framework.ExpectNoError(err, "Error starting pod %s", pod.Name)

		pod, err = c.CoreV1().Pods(ns).Get(ctx, pod.Name, metav1.GetOptions{})
		framework.ExpectNoError(err)

		// Verify volume stat metrics were collected for the referenced PVC
		volumeStatKeys := []string{
			// BlockMode PVCs only support capacity (for now)
			kubeletmetrics.VolumeStatsCapacityBytesKey,
		}
		key := volumeStatKeys[0]
		kubeletKeyName := fmt.Sprintf("%s_%s", kubeletmetrics.KubeletSubsystem, key)
		pvcName := pvcBlock.Name
		pvcNamespace := pvcBlock.Namespace
		if isEphemeral {
			pvcName = ephemeral.VolumeClaimName(pod, &pod.Spec.Volumes[0])
			pvcNamespace = pod.Namespace
		}
		// Poll kubelet metrics waiting for the volume to be picked up
		// by the volume stats collector
		var kubeMetrics e2emetrics.KubeletMetrics
		waitErr := wait.Poll(30*time.Second, 5*time.Minute, func() (bool, error) {
			framework.Logf("Grabbing Kubelet metrics")
			// Grab kubelet metrics from the node the pod was scheduled on
			var err error
			kubeMetrics, err = metricsGrabber.GrabFromKubelet(ctx, pod.Spec.NodeName)
			if err != nil {
				framework.Logf("Error fetching kubelet metrics")
				return false, err
			}
			if !findVolumeStatMetric(kubeletKeyName, pvcNamespace, pvcName, kubeMetrics) {
				return false, nil
			}
			return true, nil
		})
		framework.ExpectNoError(waitErr, "Unable to find metric %s for PVC %s/%s", kubeletKeyName, pvcNamespace, pvcName)

		for _, key := range volumeStatKeys {
			kubeletKeyName := fmt.Sprintf("%s_%s", kubeletmetrics.KubeletSubsystem, key)
			found := findVolumeStatMetric(kubeletKeyName, pvcNamespace, pvcName, kubeMetrics)
			if !found {
				framework.Failf("PVC %s, Namespace %s not found for %s", pvcName, pvcNamespace, kubeletKeyName)
			}
		}

		framework.Logf("Deleting pod %q/%q", pod.Namespace, pod.Name)
		framework.ExpectNoError(e2epod.DeletePodWithWait(ctx, c, pod))
	}

	totalTime := func(ctx context.Context, isEphemeral bool) {
		if !isEphemeral {
			pvc, err = c.CoreV1().PersistentVolumeClaims(pvc.Namespace).Create(ctx, pvc, metav1.CreateOptions{})
			framework.ExpectNoError(err)
			gomega.Expect(pvc).ToNot(gomega.BeNil())
		}

		pod := makePod(f, pvc, isEphemeral)
		pod, err = c.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{})
		framework.ExpectNoError(err)

		err = e2epod.WaitTimeoutForPodRunningInNamespace(ctx, c, pod.Name, pod.Namespace, f.Timeouts.PodStart)
		framework.ExpectNoError(err, "Error starting pod %s", pod.Name)

		pod, err = c.CoreV1().Pods(ns).Get(ctx, pod.Name, metav1.GetOptions{})
		framework.ExpectNoError(err)

		controllerMetrics, err := metricsGrabber.GrabFromControllerManager(ctx)
		if err != nil {
			e2eskipper.Skipf("Could not get controller-manager metrics - skipping")
		}

		metricKey := "volume_operation_total_seconds_count"
		dimensions := []string{"operation_name", "plugin_name"}
		err = testutil.ValidateMetrics(testutil.Metrics(controllerMetrics), metricKey, dimensions...)
		framework.ExpectNoError(err, "Invalid metric in P/V Controller metrics: %q", metricKey)

		framework.Logf("Deleting pod %q/%q", pod.Namespace, pod.Name)
		framework.ExpectNoError(e2epod.DeletePodWithWait(ctx, c, pod))
	}

	volumeManager := func(ctx context.Context, isEphemeral bool) {
		if !isEphemeral {
			pvc, err = c.CoreV1().PersistentVolumeClaims(pvc.Namespace).Create(ctx, pvc, metav1.CreateOptions{})
			framework.ExpectNoError(err)
			gomega.Expect(pvc).ToNot(gomega.BeNil())
		}

		pod := makePod(f, pvc, isEphemeral)
		pod, err = c.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{})
		framework.ExpectNoError(err)

		err = e2epod.WaitTimeoutForPodRunningInNamespace(ctx, c, pod.Name, pod.Namespace, f.Timeouts.PodStart)
		framework.ExpectNoError(err, "Error starting pod %s", pod.Name)

		pod, err = c.CoreV1().Pods(ns).Get(ctx, pod.Name, metav1.GetOptions{})
		framework.ExpectNoError(err)

		kubeMetrics, err := metricsGrabber.GrabFromKubelet(ctx, pod.Spec.NodeName)
		framework.ExpectNoError(err)

		// Metrics should have dimensions plugin_name and state available
		totalVolumesKey := "volume_manager_total_volumes"
		dimensions := []string{"state", "plugin_name"}
		err = testutil.ValidateMetrics(testutil.Metrics(kubeMetrics), totalVolumesKey, dimensions...)
		framework.ExpectNoError(err, "Invalid metric in Volume Manager metrics: %q", totalVolumesKey)

		framework.Logf("Deleting pod %q/%q", pod.Namespace, pod.Name)
		framework.ExpectNoError(e2epod.DeletePodWithWait(ctx, c, pod))
	}

	adController := func(ctx context.Context, isEphemeral bool) {
		if !isEphemeral {
			pvc, err = c.CoreV1().PersistentVolumeClaims(pvc.Namespace).Create(ctx, pvc, metav1.CreateOptions{})
			framework.ExpectNoError(err)
			gomega.Expect(pvc).ToNot(gomega.BeNil())
		}

		pod := makePod(f, pvc, isEphemeral)

		// Get metrics
		controllerMetrics, err := metricsGrabber.GrabFromControllerManager(ctx)
		if err != nil {
			e2eskipper.Skipf("Could not get controller-manager metrics - skipping")
		}

		// Create pod
		pod, err = c.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{})
		framework.ExpectNoError(err)
		err = e2epod.WaitTimeoutForPodRunningInNamespace(ctx, c, pod.Name, pod.Namespace, f.Timeouts.PodStart)
		framework.ExpectNoError(err, "Error starting pod %s", pod.Name)
		pod, err = c.CoreV1().Pods(ns).Get(ctx, pod.Name, metav1.GetOptions{})
		framework.ExpectNoError(err)

		// Get updated metrics
		updatedControllerMetrics, err := metricsGrabber.GrabFromControllerManager(ctx)
		if err != nil {
			e2eskipper.Skipf("Could not get controller-manager metrics - skipping")
		}

		// Wait and validate
		totalVolumesKey := "attach_detach_controller_total_volumes"
		states := []string{"actual_state_of_world", "desired_state_of_world"}
		dimensions := []string{"state", "plugin_name"}
		waitForADControllerStatesMetrics(ctx, metricsGrabber, totalVolumesKey, dimensions, states)

		// Total number of volumes in both ActualStateofWorld and DesiredStateOfWorld
		// states should be higher or equal than it used to be
		oldStates := getStatesMetrics(totalVolumesKey, testutil.Metrics(controllerMetrics))
		updatedStates := getStatesMetrics(totalVolumesKey, testutil.Metrics(updatedControllerMetrics))
		for _, stateName := range states {
			if _, ok := oldStates[stateName]; !ok {
				continue
			}
			for pluginName, numVolumes := range updatedStates[stateName] {
				oldNumVolumes := oldStates[stateName][pluginName]
				gomega.Expect(numVolumes).To(gomega.BeNumerically(">=", oldNumVolumes),
					"Wrong number of volumes in state %q, plugin %q: wanted >=%d, got %d",
					stateName, pluginName, oldNumVolumes, numVolumes)
			}
		}

		framework.Logf("Deleting pod %q/%q", pod.Namespace, pod.Name)
		framework.ExpectNoError(e2epod.DeletePodWithWait(ctx, c, pod))
	}

	testAll := func(isEphemeral bool) {
		ginkgo.It("should create prometheus metrics for volume provisioning and attach/detach", func(ctx context.Context) {
			provisioning(ctx, isEphemeral)
		})
		// TODO(mauriciopoppe): after CSIMigration is turned on we're no longer reporting
		// the volume_provision metric (removed in #106609), issue to investigate the bug #106773
		f.It("should create prometheus metrics for volume provisioning errors", f.WithSlow(), func(ctx context.Context) {
			provisioningError(ctx, isEphemeral)
		})
		ginkgo.It("should create volume metrics with the correct FilesystemMode PVC ref", func(ctx context.Context) {
			filesystemMode(ctx, isEphemeral)
		})
		ginkgo.It("should create volume metrics with the correct BlockMode PVC ref", func(ctx context.Context) {
			blockmode(ctx, isEphemeral)
		})
		ginkgo.It("should create metrics for total time taken in volume operations in P/V Controller", func(ctx context.Context) {
			totalTime(ctx, isEphemeral)
		})
		ginkgo.It("should create volume metrics in Volume Manager", func(ctx context.Context) {
			volumeManager(ctx, isEphemeral)
		})
		ginkgo.It("should create metrics for total number of volumes in A/D Controller", func(ctx context.Context) {
			adController(ctx, isEphemeral)
		})
	}

	ginkgo.Context("PVC", func() {
		testAll(false)
	})

	ginkgo.Context("Ephemeral", func() {
		testAll(true)
	})

	// Test for pv controller metrics, concretely: bound/unbound pv/pvc count.
	ginkgo.Describe("PVController", func() {
		const (
			namespaceKey            = "namespace"
			pluginNameKey           = "plugin_name"
			volumeModeKey           = "volume_mode"
			storageClassKey         = "storage_class"
			volumeAttributeClassKey = "volume_attributes_class"

			totalPVKey    = "pv_collector_total_pv_count"
			boundPVKey    = "pv_collector_bound_pv_count"
			unboundPVKey  = "pv_collector_unbound_pv_count"
			boundPVCKey   = "pv_collector_bound_pvc_count"
			unboundPVCKey = "pv_collector_unbound_pvc_count"
		)

		var (
			pv  *v1.PersistentVolume
			pvc *v1.PersistentVolumeClaim

			storageClassName = "bound-unbound-count-test-sc"
			pvConfig         = e2epv.PersistentVolumeConfig{
				PVSource: v1.PersistentVolumeSource{
					HostPath: &v1.HostPathVolumeSource{Path: "/data"},
				},
				NamePrefix:       "pv-test-",
				StorageClassName: storageClassName,
			}
			// TODO: Insert volumeAttributesClassName into pvcConfig when "VolumeAttributesClass" is GA
			volumeAttributesClassName = "bound-unbound-count-test-vac"
			pvcConfig                 = e2epv.PersistentVolumeClaimConfig{StorageClassName: &storageClassName}

			e2emetrics = []struct {
				name      string
				dimension string
			}{
				{boundPVKey, storageClassKey},
				{unboundPVKey, storageClassKey},
				{boundPVCKey, namespaceKey},
				{unboundPVCKey, namespaceKey},
			}

			// Original metric values before we create any PV/PVCs. The length should be 4,
			// and the elements should be bound pv count, unbound pv count, bound pvc count,
			// unbound pvc count in turn.
			// We use these values to calculate relative increment of each test.
			originMetricValues []map[string]int64
		)

		// validator used to validate each metric's values, the length of metricValues
		// should be 4, and the elements should be bound pv count, unbound pv count, bound
		// pvc count, unbound pvc count in turn.
		validator := func(ctx context.Context, metricValues []map[string]int64) {
			gomega.Expect(metricValues).To(gomega.HaveLen(4), "Wrong metric size: %d", len(metricValues))

			controllerMetrics, err := metricsGrabber.GrabFromControllerManager(ctx)
			framework.ExpectNoError(err, "Error getting c-m metricValues: %v", err)

			for i, metric := range e2emetrics {
				expectValues := metricValues[i]
				if expectValues == nil {
					expectValues = make(map[string]int64)
				}
				// We use relative increment value instead of absolute value to reduce unexpected flakes.
				// Concretely, we expect the difference of the updated values and original values for each
				// test suit are equal to expectValues.
				actualValues := calculateRelativeValues(originMetricValues[i],
					testutil.GetMetricValuesForLabel(testutil.Metrics(controllerMetrics), metric.name, metric.dimension))
				gomega.Expect(actualValues).To(gomega.Equal(expectValues), "Wrong pv controller metric %s(%s): wanted %v, got %v",
					metric.name, metric.dimension, expectValues, actualValues)
			}
		}

		ginkgo.BeforeEach(func(ctx context.Context) {
			if !metricsGrabber.HasControlPlanePods() {
				e2eskipper.Skipf("Environment does not support getting controller-manager metrics - skipping")
			}

			pv = e2epv.MakePersistentVolume(pvConfig)
			pvc = e2epv.MakePersistentVolumeClaim(pvcConfig, ns)

			// Initializes all original metric values.
			controllerMetrics, err := metricsGrabber.GrabFromControllerManager(ctx)
			framework.ExpectNoError(err, "Error getting c-m metricValues: %v", err)
			for _, metric := range e2emetrics {
				originMetricValues = append(originMetricValues,
					testutil.GetMetricValuesForLabel(testutil.Metrics(controllerMetrics), metric.name, metric.dimension))
			}
		})

		ginkgo.AfterEach(func(ctx context.Context) {
			if err := e2epv.DeletePersistentVolume(ctx, c, pv.Name); err != nil {
				framework.Failf("Error deleting pv: %v", err)
			}
			if err := e2epv.DeletePersistentVolumeClaim(ctx, c, pvc.Name, pvc.Namespace); err != nil {
				framework.Failf("Error deleting pvc: %v", err)
			}

			// Clear original metric values.
			originMetricValues = nil
		})

		ginkgo.It("should create none metrics for pvc controller before creating any PV or PVC", func(ctx context.Context) {
			validator(ctx, []map[string]int64{nil, nil, nil, nil})
		})

		ginkgo.It("should create unbound pv count metrics for pvc controller after creating pv only",
			func(ctx context.Context) {
				var err error
				pv, err = e2epv.CreatePV(ctx, c, f.Timeouts, pv)
				framework.ExpectNoError(err, "Error creating pv: %v", err)
				waitForPVControllerSync(ctx, metricsGrabber, unboundPVKey, storageClassKey)
				validator(ctx, []map[string]int64{nil, {storageClassName: 1}, nil, nil})
			})

		ginkgo.It("should create unbound pvc count metrics for pvc controller after creating pvc only",
			func(ctx context.Context) {
				var err error
				pvc, err = e2epv.CreatePVC(ctx, c, ns, pvc)
				framework.ExpectNoError(err, "Error creating pvc: %v", err)
				waitForPVControllerSync(ctx, metricsGrabber, unboundPVCKey, namespaceKey)
				validator(ctx, []map[string]int64{nil, nil, nil, {ns: 1}})
			})

		ginkgo.It("should create bound pv/pvc count metrics for pvc controller after creating both pv and pvc",
			func(ctx context.Context) {
				var err error
				pv, pvc, err = e2epv.CreatePVPVC(ctx, c, f.Timeouts, pvConfig, pvcConfig, ns, true)
				framework.ExpectNoError(err, "Error creating pv pvc: %v", err)
				waitForPVControllerSync(ctx, metricsGrabber, boundPVKey, storageClassKey)
				waitForPVControllerSync(ctx, metricsGrabber, boundPVCKey, namespaceKey)
				validator(ctx, []map[string]int64{{storageClassName: 1}, nil, {ns: 1}, nil})
			})

		// TODO: Merge with bound/unbound tests when "VolumeAttributesClass" feature is enabled by default
		f.It("should create unbound pvc count metrics for pvc controller with volume attributes class dimension after creating pvc only",
			feature.VolumeAttributesClass, framework.WithFeatureGate(features.VolumeAttributesClass), func(ctx context.Context) {
				var err error
				dimensions := []string{namespaceKey, storageClassKey, volumeAttributeClassKey}
				pvcConfigWithVAC := pvcConfig
				pvcConfigWithVAC.VolumeAttributesClassName = &volumeAttributesClassName
				pvcWithVAC := e2epv.MakePersistentVolumeClaim(pvcConfigWithVAC, ns)
				pvc, err = e2epv.CreatePVC(ctx, c, ns, pvcWithVAC)
				framework.ExpectNoError(err, "Error creating pvc: %v", err)
				waitForPVControllerSync(ctx, metricsGrabber, unboundPVCKey, volumeAttributeClassKey)
				controllerMetrics, err := metricsGrabber.GrabFromControllerManager(ctx)
				framework.ExpectNoError(err, "Error getting c-m metricValues: %v", err)
				err = testutil.ValidateMetrics(testutil.Metrics(controllerMetrics), unboundPVCKey, dimensions...)
				framework.ExpectNoError(err, "Invalid metric in Controller Manager metrics: %q", unboundPVCKey)
			})

		// TODO: Merge with bound/unbound tests when "VolumeAttributesClass" feature is enabled by default
		f.It("should create bound pv/pvc count metrics for pvc controller with volume attributes class dimension after creating both pv and pvc",
			feature.VolumeAttributesClass, framework.WithFeatureGate(features.VolumeAttributesClass), func(ctx context.Context) {
				var err error
				dimensions := []string{namespaceKey, storageClassKey, volumeAttributeClassKey}
				pvcConfigWithVAC := pvcConfig
				pvcConfigWithVAC.VolumeAttributesClassName = &volumeAttributesClassName
				pv, pvc, err = e2epv.CreatePVPVC(ctx, c, f.Timeouts, pvConfig, pvcConfigWithVAC, ns, true)
				framework.ExpectNoError(err, "Error creating pv pvc: %v", err)
				waitForPVControllerSync(ctx, metricsGrabber, boundPVKey, storageClassKey)
				waitForPVControllerSync(ctx, metricsGrabber, boundPVCKey, volumeAttributeClassKey)
				controllerMetrics, err := metricsGrabber.GrabFromControllerManager(ctx)
				framework.ExpectNoError(err, "Error getting c-m metricValues: %v", err)
				err = testutil.ValidateMetrics(testutil.Metrics(controllerMetrics), boundPVCKey, dimensions...)
				framework.ExpectNoError(err, "Invalid metric in Controller Manager metrics: %q", boundPVCKey)
			})

		ginkgo.It("should create total pv count metrics for with plugin and volume mode labels after creating pv",
			func(ctx context.Context) {
				var err error
				dimensions := []string{pluginNameKey, volumeModeKey}
				pv, err = e2epv.CreatePV(ctx, c, f.Timeouts, pv)
				framework.ExpectNoError(err, "Error creating pv: %v", err)
				waitForPVControllerSync(ctx, metricsGrabber, totalPVKey, pluginNameKey)
				controllerMetrics, err := metricsGrabber.GrabFromControllerManager(ctx)
				framework.ExpectNoError(err, "Error getting c-m metricValues: %v", err)
				err = testutil.ValidateMetrics(testutil.Metrics(controllerMetrics), totalPVKey, dimensions...)
				framework.ExpectNoError(err, "Invalid metric in Controller Manager metrics: %q", totalPVKey)
			})
	})
})

type storageControllerMetrics struct {
	latencyMetrics map[string]int64
	statusMetrics  map[string]statusMetricCounts
}

type statusMetricCounts struct {
	successCount int64
	failCount    int64
	otherCount   int64
}

func newStorageControllerMetrics() *storageControllerMetrics {
	return &storageControllerMetrics{
		latencyMetrics: make(map[string]int64),
		statusMetrics:  make(map[string]statusMetricCounts),
	}
}

func waitForDetachAndGrabMetrics(ctx context.Context, oldMetrics *storageControllerMetrics, metricsGrabber *e2emetrics.Grabber, pluginName string) *storageControllerMetrics {
	backoff := wait.Backoff{
		Duration: 10 * time.Second,
		Factor:   1.2,
		Steps:    21,
	}

	updatedStorageMetrics := newStorageControllerMetrics()
	oldDetachCount, ok := oldMetrics.latencyMetrics["volume_detach"]
	if !ok {
		oldDetachCount = 0
	}

	verifyMetricFunc := func(ctx context.Context) (bool, error) {
		updatedMetrics, err := metricsGrabber.GrabFromControllerManager(ctx)

		if err != nil {
			framework.Logf("Error fetching controller-manager metrics")
			return false, err
		}

		updatedStorageMetrics = getControllerStorageMetrics(updatedMetrics, pluginName)
		newDetachCount, ok := updatedStorageMetrics.latencyMetrics["volume_detach"]

		// if detach metrics are not yet there, we need to retry
		if !ok {
			return false, nil
		}

		// if old Detach count is more or equal to new detach count, that means detach
		// event has not been observed yet.
		if oldDetachCount >= newDetachCount {
			return false, nil
		}

		return true, nil
	}

	waitErr := wait.ExponentialBackoffWithContext(ctx, backoff, verifyMetricFunc)
	framework.ExpectNoError(waitErr, "Unable to get updated metrics for plugin %s", pluginName)
	return updatedStorageMetrics
}

func verifyMetricCount(oldMetrics, newMetrics *storageControllerMetrics, metricName string, expectFailure bool) {
	oldLatencyCount, ok := oldMetrics.latencyMetrics[metricName]
	// if metric does not exist in oldMap, it probably hasn't been emitted yet.
	if !ok {
		oldLatencyCount = 0
	}

	oldStatusCount := int64(0)
	if oldStatusCounts, ok := oldMetrics.statusMetrics[metricName]; ok {
		if expectFailure {
			oldStatusCount = oldStatusCounts.failCount
		} else {
			oldStatusCount = oldStatusCounts.successCount
		}
	}

	newLatencyCount, ok := newMetrics.latencyMetrics[metricName]
	if !expectFailure {
		if !ok {
			framework.Failf("Error getting updated latency metrics for %s", metricName)
		}
	}
	newStatusCounts, ok := newMetrics.statusMetrics[metricName]
	if !ok {
		framework.Failf("Error getting updated status metrics for %s", metricName)
	}

	newStatusCount := int64(0)
	if expectFailure {
		newStatusCount = newStatusCounts.failCount
	} else {
		newStatusCount = newStatusCounts.successCount
	}

	// It appears that in a busy cluster some spurious detaches are unavoidable
	// even if the test is run serially.  We really just verify if new count
	// is greater than old count
	if !expectFailure {
		gomega.Expect(newLatencyCount).To(gomega.BeNumerically(">", oldLatencyCount), "New latency count %d should be more than old count %d for action %s", newLatencyCount, oldLatencyCount, metricName)
	}
	gomega.Expect(newStatusCount).To(gomega.BeNumerically(">", oldStatusCount), "New status count %d should be more than old count %d for action %s", newStatusCount, oldStatusCount, metricName)
}

func getControllerStorageMetrics(ms e2emetrics.ControllerManagerMetrics, pluginName string) *storageControllerMetrics {
	result := newStorageControllerMetrics()

	for method, samples := range ms {
		switch method {
		// from the base metric name "storage_operation_duration_seconds"
		case "storage_operation_duration_seconds_count":
			for _, sample := range samples {
				count := int64(sample.Value)
				operation := string(sample.Metric["operation_name"])
				// if the volumes were provisioned with a CSI Driver
				// the metric operation name will be prefixed with
				// "kubernetes.io/csi:"
				metricPluginName := string(sample.Metric["volume_plugin"])
				status := string(sample.Metric["status"])
				if strings.Index(metricPluginName, pluginName) < 0 {
					// the metric volume plugin field doesn't match
					// the default storageClass.Provisioner field
					continue
				}

				statusCounts := result.statusMetrics[operation]
				switch status {
				case "success":
					statusCounts.successCount = count
				case "fail-unknown":
					statusCounts.failCount = count
				default:
					statusCounts.otherCount = count
				}
				result.statusMetrics[operation] = statusCounts
				result.latencyMetrics[operation] = count
			}
		}
	}
	return result
}

// Finds the sample in the specified metric from `KubeletMetrics` tagged with
// the specified namespace and pvc name
func findVolumeStatMetric(metricKeyName string, namespace string, pvcName string, kubeletMetrics e2emetrics.KubeletMetrics) bool {
	found := false
	errCount := 0
	framework.Logf("Looking for sample in metric `%s` tagged with namespace `%s`, PVC `%s`", metricKeyName, namespace, pvcName)
	if samples, ok := kubeletMetrics[metricKeyName]; ok {
		for _, sample := range samples {
			framework.Logf("Found sample %s", sample.String())
			samplePVC, ok := sample.Metric["persistentvolumeclaim"]
			if !ok {
				framework.Logf("Error getting pvc for metric %s, sample %s", metricKeyName, sample.String())
				errCount++
			}
			sampleNS, ok := sample.Metric["namespace"]
			if !ok {
				framework.Logf("Error getting namespace for metric %s, sample %s", metricKeyName, sample.String())
				errCount++
			}

			if string(samplePVC) == pvcName && string(sampleNS) == namespace {
				found = true
				break
			}
		}
	}
	gomega.Expect(errCount).To(gomega.Equal(0), "Found invalid samples")
	return found
}

// Wait for the count of a pv controller's metric specified by metricName and dimension bigger than zero.
func waitForPVControllerSync(ctx context.Context, metricsGrabber *e2emetrics.Grabber, metricName, dimension string) {
	backoff := wait.Backoff{
		Duration: 10 * time.Second,
		Factor:   1.2,
		Steps:    21,
	}
	verifyMetricFunc := func(ctx context.Context) (bool, error) {
		updatedMetrics, err := metricsGrabber.GrabFromControllerManager(ctx)
		if err != nil {
			framework.Logf("Error fetching controller-manager metrics")
			return false, err
		}
		return len(testutil.GetMetricValuesForLabel(testutil.Metrics(updatedMetrics), metricName, dimension)) > 0, nil
	}
	waitErr := wait.ExponentialBackoffWithContext(ctx, backoff, verifyMetricFunc)
	framework.ExpectNoError(waitErr, "Unable to get pv controller metrics")
}

func calculateRelativeValues(originValues, updatedValues map[string]int64) map[string]int64 {
	relativeValues := make(map[string]int64)
	for key, value := range updatedValues {
		relativeValue := value - originValues[key]
		if relativeValue != 0 {
			relativeValues[key] = relativeValue
		}
	}
	for key, value := range originValues {
		if _, exist := updatedValues[key]; !exist && value > 0 {
			relativeValues[key] = -value
		}
	}
	return relativeValues
}

func getStatesMetrics(metricKey string, givenMetrics testutil.Metrics) map[string]map[string]int64 {
	states := make(map[string]map[string]int64)
	for _, sample := range givenMetrics[metricKey] {
		framework.Logf("Found sample %q", sample.String())
		state := string(sample.Metric["state"])
		pluginName := string(sample.Metric["plugin_name"])
		states[state] = map[string]int64{pluginName: int64(sample.Value)}
	}
	return states
}

func waitForADControllerStatesMetrics(ctx context.Context, metricsGrabber *e2emetrics.Grabber, metricName string, dimensions []string, stateNames []string) {
	backoff := wait.Backoff{
		Duration: 10 * time.Second,
		Factor:   1.2,
		Steps:    21,
	}
	verifyMetricFunc := func(ctx context.Context) (bool, error) {
		updatedMetrics, err := metricsGrabber.GrabFromControllerManager(ctx)
		if err != nil {
			e2eskipper.Skipf("Could not get controller-manager metrics - skipping")
			return false, err
		}
		err = testutil.ValidateMetrics(testutil.Metrics(updatedMetrics), metricName, dimensions...)
		if err != nil {
			return false, fmt.Errorf("could not get valid metrics: %v ", err)
		}
		states := getStatesMetrics(metricName, testutil.Metrics(updatedMetrics))
		for _, name := range stateNames {
			if _, ok := states[name]; !ok {
				return false, fmt.Errorf("could not get state %q from A/D Controller metrics", name)
			}
		}
		return true, nil
	}
	waitErr := wait.ExponentialBackoffWithContext(ctx, backoff, verifyMetricFunc)
	framework.ExpectNoError(waitErr, "Unable to get A/D controller metrics")
}

// makePod creates a pod which either references the PVC or creates it via a
// generic ephemeral volume claim template.
func makePod(f *framework.Framework, pvc *v1.PersistentVolumeClaim, isEphemeral bool) *v1.Pod {
	claims := []*v1.PersistentVolumeClaim{pvc}
	pod := e2epod.MakePod(f.Namespace.Name, nil, claims, f.NamespacePodSecurityLevel, "")
	if isEphemeral {
		volSrc := pod.Spec.Volumes[0]
		volSrc.PersistentVolumeClaim = nil
		volSrc.Ephemeral = &v1.EphemeralVolumeSource{
			VolumeClaimTemplate: &v1.PersistentVolumeClaimTemplate{
				Spec: pvc.Spec,
			},
		}
		pod.Spec.Volumes[0] = volSrc
	}
	return pod
}
